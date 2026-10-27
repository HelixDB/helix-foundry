package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	gomysql "github.com/go-sql-driver/mysql"
)

// An install's secrets live in one directory, a file each, which `foundry
// init-secrets` writes before anything else starts (the compose file's
// init-secrets service, into its secrets volume) and the install's
// services read: the site its token, MySQL its password, the store its
// secret, and foundry all four (local.secrets_dir). A file is written once
// and never again: the connector key seals every stored credential, so a
// new one would make them unreadable, and MySQL keeps the password it was
// first started with.

// secret is one of the install's secrets: its file in the directory, and
// the variable an install's .env named it by before the directory, whose
// value is adopted rather than a new one made.
type secret struct {
	file string
	env  string
	make func() (string, error)
}

// randomHex is 32 random bytes in hex, as setup.sh wrote them with openssl
// rand -hex 32.
func randomHex() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// randomKey is a 32-byte AES key in base64, the form foundry.connectors
// keys take.
func randomKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// The install's secrets, by file.
const (
	secretSiteToken     = "site-token"
	secretS3            = "s3-secret"
	secretMySQLPassword = "mysql-password"
	secretConnectorKey  = "connector-key"
)

var installSecrets = []secret{
	{file: secretSiteToken, env: "FOUNDRY_LOCAL_TOKEN", make: randomHex},
	{file: secretS3, env: "FOUNDRY_S3_SECRET", make: randomHex},
	{file: secretMySQLPassword, env: "FOUNDRY_MYSQL_PASSWORD", make: randomHex},
	{file: secretConnectorKey, env: "FOUNDRY_CONNECTOR_KEY", make: randomKey},
}

// ids are a file's owner and group.
type ids struct{ uid, gid int }

// secretsLayout is who owns the files: owner, and a reader group per file
// that another service reads (the site's user's group for its token,
// MySQL's for its password). A file with a reader is 0440, owner and
// group; one without, 0400.
type secretsLayout struct {
	owner   ids
	readers map[string]int
}

// of is a file's mode and owner under l.
func (l secretsLayout) of(file string) (fs.FileMode, ids) {
	if gid, ok := l.readers[file]; ok {
		return 0o440, ids{uid: l.owner.uid, gid: gid}
	}
	return 0o400, l.owner
}

// initSecrets writes into dir each secret it lacks: the value of its
// variable (getenv) when set, else a new random one. A file already there is
// kept as it is, and only given its owner and mode; a variable that differs
// from it is reported and ignored. It refuses an empty file. Nothing it
// logs or returns holds a secret.
//
//	initSecrets("/secrets", layout, os.Getenv, logger)
func initSecrets(dir string, layout secretsLayout, getenv func(string) string, logger *slog.Logger) error {
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return fmt.Errorf("secrets directory: %w", err)
	}
	// Others may open the files they are given by name, but not list them.
	if err := os.Chmod(dir, 0o711); err != nil {
		return fmt.Errorf("secrets directory: %w", err)
	}
	for _, s := range installSecrets {
		mode, owner := layout.of(s.file)
		path := filepath.Join(dir, s.file)
		given := strings.TrimSpace(getenv(s.env))
		existing, err := os.ReadFile(path)
		switch {
		case err == nil:
			kept := strings.TrimSpace(string(existing))
			if kept == "" {
				return fmt.Errorf("%s is empty; it is never rewritten, so remove it to make a new one (for %s, only with the data it sealed)",
					path, secretConnectorKey)
			}
			if given != "" && given != kept {
				logger.Warn("kept the stored secret; the variable that differs from it is ignored", "secret", s.file, "variable", s.env)
			} else {
				logger.Info("kept", "secret", s.file)
			}
		case errors.Is(err, fs.ErrNotExist):
			value, from := given, "variable "+s.env
			if value == "" {
				if value, err = s.make(); err != nil {
					return fmt.Errorf("make %s: %w", s.file, err)
				}
				from = "generated"
			}
			if err := writeOnce(path, value, mode, owner); err != nil {
				return err
			}
			logger.Info("wrote", "secret", s.file, "from", from)
		default:
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err := os.Chown(path, owner.uid, owner.gid); err != nil {
			return fmt.Errorf("own %s: %w", path, err)
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("mode %s: %w", path, err)
		}
	}
	return nil
}

// writeOnce writes value to path unless something is there, all or nothing:
// into a file of its own, synced, then linked into place, which fails rather
// than replace a file another run wrote first.
func writeOnce(path, value string, mode fs.FileMode, owner ids) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = io.WriteString(tmp, value)
	err = errors.Join(err, tmp.Chmod(mode), tmp.Chown(owner.uid, owner.gid), tmp.Sync(), tmp.Close())
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("sync %s: %w", filepath.Dir(path), err)
	}
	return errors.Join(d.Sync(), d.Close())
}

// fillSecrets fills each secret c lacks from local.secrets_dir, if it is
// set: the site token, the store's secret, the database's password (into
// db.dsn, when it has none) and the connector key. A value already
// configured wins over the file. A secret it must fill that the directory
// lacks is an error naming it.
func (c *Config) fillSecrets() error {
	dir := c.Local.SecretsDir
	if dir == "" {
		return nil
	}
	read := func(file string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("local.secrets_dir %s has no %s: run foundry init-secrets", dir, file)
		}
		if err != nil {
			return "", fmt.Errorf("local.secrets_dir: %w", err)
		}
		v := string(bytes.TrimSpace(b))
		if v == "" {
			return "", fmt.Errorf("local.secrets_dir %s: %s is empty", dir, file)
		}
		return v, nil
	}
	for _, field := range []struct {
		file string
		to   *string
	}{
		{secretSiteToken, &c.Local.SiteToken},
		{secretS3, &c.Local.S3.SecretAccessKey},
		{secretConnectorKey, &c.Foundry.Connectors.CurrentKey},
	} {
		if *field.to != "" {
			continue
		}
		v, err := read(field.file)
		if err != nil {
			return err
		}
		*field.to = v
	}
	if strings.TrimSpace(c.DB.DSN) == "" {
		return nil
	}
	dsn, err := gomysql.ParseDSN(c.DB.DSN)
	if err != nil {
		return fmt.Errorf("parse db.dsn: %w", err)
	}
	if dsn.Passwd != "" {
		return nil
	}
	if dsn.Passwd, err = read(secretMySQLPassword); err != nil {
		return err
	}
	c.DB.DSN = dsn.FormatDSN()
	return nil
}

// runInitSecrets is `foundry init-secrets`:
//
//	foundry init-secrets --dir /secrets --owner 65532:65532 --reader site-token=1000 --reader mysql-password=999
//
// --owner defaults to the process's own user and group; each --reader gives
// a file's group to another service's.
func runInitSecrets(args []string, getenv func(string) string, logger *slog.Logger) error {
	flags := flag.NewFlagSet("foundry init-secrets", flag.ContinueOnError)
	dir := flags.String("dir", "", "the secrets directory")
	owner := flags.String("owner", "", "the files' owner, UID:GID (default: this process's)")
	layout := secretsLayout{owner: ids{uid: os.Getuid(), gid: os.Getgid()}, readers: map[string]int{}}
	flags.Func("reader", "FILE=GID: the group that may also read FILE (repeatable)", func(v string) error {
		file, gid, ok := strings.Cut(v, "=")
		n, err := strconv.Atoi(gid)
		if !ok || err != nil || n < 0 {
			return fmt.Errorf("want FILE=GID, got %q", v)
		}
		for _, s := range installSecrets {
			if s.file == file {
				layout.readers[file] = n
				return nil
			}
		}
		return fmt.Errorf("no secret %q", file)
	})
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" || flags.NArg() > 0 {
		return errors.New("usage: foundry init-secrets --dir DIR [--owner UID:GID] [--reader FILE=GID]...")
	}
	if *owner != "" {
		uid, gid, ok := strings.Cut(*owner, ":")
		u, errU := strconv.Atoi(uid)
		g, errG := strconv.Atoi(gid)
		if !ok || errU != nil || errG != nil || u < 0 || g < 0 {
			return fmt.Errorf("--owner wants UID:GID, got %q", *owner)
		}
		layout.owner = ids{uid: u, gid: g}
	}
	return initSecrets(*dir, layout, getenv, logger)
}
