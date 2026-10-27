package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
)

// own is this process's user and group: the only owner a test can give a
// file without root.
func own() secretsLayout {
	return secretsLayout{owner: ids{uid: os.Getuid(), gid: os.Getgid()}, readers: map[string]int{}}
}

func noEnv(string) string { return "" }

// logged is a logger into buf.
func logged(buf *bytes.Buffer) *slog.Logger { return slog.New(slog.NewTextHandler(buf, nil)) }

func readSecret(t *testing.T, dir, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// A new install gets four random secrets, in the forms their readers take,
// readable only by their owner, and its directory can't be listed by others.
func TestInitSecretsGenerates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	var log bytes.Buffer
	if err := initSecrets(dir, own(), noEnv, logged(&log)); err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, dir); m != 0o711 {
		t.Fatalf("directory mode %o", m)
	}
	for _, file := range []string{secretSiteToken, secretS3, secretMySQLPassword} {
		v := readSecret(t, dir, file)
		if b, err := hex.DecodeString(v); err != nil || len(b) != 32 {
			t.Fatalf("%s is not 32 bytes of hex", file)
		}
		if m := modeOf(t, filepath.Join(dir, file)); m != 0o400 {
			t.Fatalf("%s mode %o", file, m)
		}
	}
	if b, err := base64.StdEncoding.DecodeString(readSecret(t, dir, secretConnectorKey)); err != nil || len(b) != 32 {
		t.Fatal("the connector key is not a 32-byte key in base64")
	}
	if readSecret(t, dir, secretSiteToken) == readSecret(t, dir, secretS3) {
		t.Fatal("two secrets are the same")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("left %d files, want the 4 secrets and no temporary ones", len(entries))
	}
	if strings.Count(log.String(), "from=generated") != 4 {
		t.Fatalf("log = %s", log.String())
	}
}

// Running again, as every start does, keeps every secret as it was, and
// never logs one.
func TestInitSecretsKeeps(t *testing.T) {
	dir := t.TempDir()
	if err := initSecrets(dir, own(), noEnv, logged(&bytes.Buffer{})); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for _, s := range installSecrets {
		before[s.file] = readSecret(t, dir, s.file)
	}
	var log bytes.Buffer
	// Even a variable naming another value: the stored one stays.
	env := func(name string) string {
		if name == "FOUNDRY_CONNECTOR_KEY" {
			return "another-key"
		}
		return ""
	}
	if err := initSecrets(dir, own(), env, logged(&log)); err != nil {
		t.Fatal(err)
	}
	for file, v := range before {
		if got := readSecret(t, dir, file); got != v {
			t.Fatalf("%s changed", file)
		}
		if strings.Contains(log.String(), v) {
			t.Fatalf("logged %s", file)
		}
	}
	if !strings.Contains(log.String(), "secret=connector-key variable=FOUNDRY_CONNECTOR_KEY") || strings.Contains(log.String(), "another-key") {
		t.Fatalf("log = %s", log.String())
	}
}

// An install made with setup.sh keeps its secrets: the .env's values are
// written, not new ones, so MySQL's password and the key that sealed its
// credentials still match.
func TestInitSecretsAdoptsVariables(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{
		"FOUNDRY_LOCAL_TOKEN":    "token-0123456789abcdef0123456789abcdef",
		"FOUNDRY_S3_SECRET":      " s3-secret-from-env\n",
		"FOUNDRY_MYSQL_PASSWORD": "mysql-password-from-env",
		"FOUNDRY_CONNECTOR_KEY":  base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
	}
	var log bytes.Buffer
	if err := initSecrets(dir, own(), func(k string) string { return env[k] }, logged(&log)); err != nil {
		t.Fatal(err)
	}
	for _, s := range installSecrets {
		if got := readSecret(t, dir, s.file); got != strings.TrimSpace(env[s.env]) {
			t.Fatalf("%s = %q", s.file, got)
		}
	}
	if strings.Contains(log.String(), "from=generated") || strings.Contains(log.String(), "mysql-password-from-env") {
		t.Fatalf("log = %s", log.String())
	}
}

// A file some other service reads is its group's too.
func TestInitSecretsReaders(t *testing.T) {
	dir := t.TempDir()
	layout := own()
	layout.readers[secretSiteToken] = os.Getgid()
	if err := initSecrets(dir, layout, noEnv, logged(&bytes.Buffer{})); err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, filepath.Join(dir, secretSiteToken)); m != 0o440 {
		t.Fatalf("site token mode %o", m)
	}
	if m := modeOf(t, filepath.Join(dir, secretConnectorKey)); m != 0o400 {
		t.Fatalf("connector key mode %o", m)
	}
	// A file kept from before is given its mode again.
	if err := os.Chmod(filepath.Join(dir, secretConnectorKey), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := initSecrets(dir, layout, noEnv, logged(&bytes.Buffer{})); err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, filepath.Join(dir, secretConnectorKey)); m != 0o400 {
		t.Fatalf("connector key mode %o after a second run", m)
	}
}

// An empty file is refused, never filled: it may be the key that sealed
// the install's credentials.
func TestInitSecretsRefusesAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, secretConnectorKey), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := initSecrets(dir, own(), noEnv, logged(&bytes.Buffer{}))
	if err == nil || !strings.Contains(err.Error(), "connector-key is empty") {
		t.Fatalf("err = %v", err)
	}
	if got := readSecret(t, dir, secretConnectorKey); got != "\n" {
		t.Fatalf("rewrote it: %q", got)
	}
}

// What isn't a directory of files is refused, nothing written.
func TestInitSecretsCannotWrite(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := initSecrets(file, own(), noEnv, logged(&bytes.Buffer{})); err == nil || !strings.Contains(err.Error(), "secrets directory") {
		t.Fatalf("err = %v", err)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, secretSiteToken), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := initSecrets(dir, own(), noEnv, logged(&bytes.Buffer{})); err == nil || !strings.Contains(err.Error(), "read "+filepath.Join(dir, secretSiteToken)) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunInitSecretsFlags(t *testing.T) {
	dir := t.TempDir()
	if err := runInitSecrets([]string{"--dir", dir, "--owner", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		"--reader", "mysql-password=" + strconv.Itoa(os.Getgid())}, noEnv, logged(&bytes.Buffer{})); err != nil {
		t.Fatal(err)
	}
	if m := modeOf(t, filepath.Join(dir, secretMySQLPassword)); m != 0o440 {
		t.Fatalf("mysql password mode %o", m)
	}
	for name, args := range map[string][]string{
		"no directory":       {},
		"a stray argument":   {"--dir", dir, "extra"},
		"an unknown secret":  {"--dir", dir, "--reader", "nope=1"},
		"a reader with no =": {"--dir", dir, "--reader", "site-token"},
		"a negative group":   {"--dir", dir, "--reader", "site-token=-1"},
		"an owner of one id": {"--dir", dir, "--owner", "65532"},
		"an owner of words":  {"--dir", dir, "--owner", "a:b"},
		"an unknown flag":    {"--dir", dir, "--nope"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := runInitSecrets(args, noEnv, logged(&bytes.Buffer{})); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// writeSecrets is a secrets directory as init-secrets writes it, with
// known values.
func writeSecrets(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for file, v := range map[string]string{
		secretSiteToken: siteToken + "\n", secretS3: "s3-from-file", secretMySQLPassword: "pw-from-file",
		secretConnectorKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(v), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The compose file passes only the secrets directory and a DSN without a
// password: every secret comes from the directory, the password into the
// DSN.
func TestSecretsDir(t *testing.T) {
	t.Setenv("HELIX_DB_DSN", "helix@tcp(mysql:3306)/helix?parseTime=true&multiStatements=true")
	t.Setenv("HELIX_LOCAL__SECRETS_DIR", writeSecrets(t))
	cfg, err := load("foundry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Local.SiteToken != siteToken || cfg.Local.S3.SecretAccessKey != "s3-from-file" ||
		cfg.Foundry.Connectors.CurrentKey != base64.StdEncoding.EncodeToString(make([]byte, 32)) {
		t.Fatalf("local = %+v, connector key %q", cfg.Local, cfg.Foundry.Connectors.CurrentKey)
	}
	dsn, err := gomysql.ParseDSN(cfg.DB.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if dsn.User != "helix" || dsn.Passwd != "pw-from-file" || dsn.Addr != "mysql:3306" || dsn.DBName != "helix" ||
		!dsn.ParseTime || !dsn.MultiStatements {
		t.Fatalf("dsn = %+v", dsn)
	}
}

// What the configuration sets wins over the directory.
func TestSecretsDirLosesToConfiguration(t *testing.T) {
	secrets(t)
	t.Setenv("HELIX_LOCAL__SECRETS_DIR", writeSecrets(t))
	cfg, err := load("foundry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Local.S3.SecretAccessKey != "s3-secret" || cfg.DB.DSN != "helix:pw@tcp(mysql:3306)/helix?parseTime=true&multiStatements=true" {
		t.Fatalf("s3 %q, dsn %q", cfg.Local.S3.SecretAccessKey, cfg.DB.DSN)
	}
}

func TestSecretsDirMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(dir string)
		want    string
	}{
		"a missing file": {func(dir string) { _ = os.Remove(filepath.Join(dir, secretMySQLPassword)) }, "has no mysql-password: run foundry init-secrets"},
		"an empty file": {func(dir string) {
			_ = os.Remove(filepath.Join(dir, secretSiteToken))
			_ = os.WriteFile(filepath.Join(dir, secretSiteToken), []byte(" \n"), 0o400)
		}, "site-token is empty"},
		"an unreadable directory": {func(dir string) { _ = os.RemoveAll(dir); _ = os.WriteFile(dir, nil, 0o600) }, "local.secrets_dir"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeSecrets(t)
			tc.prepare(dir)
			t.Setenv("HELIX_DB_DSN", "helix@tcp(mysql:3306)/helix")
			t.Setenv("HELIX_LOCAL__SECRETS_DIR", dir)
			if _, err := load("foundry.yaml"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("an unparsable DSN", func(t *testing.T) {
		t.Setenv("HELIX_DB_DSN", "not a dsn")
		t.Setenv("HELIX_LOCAL__SECRETS_DIR", writeSecrets(t))
		if _, err := load("foundry.yaml"); err == nil || !strings.Contains(err.Error(), "parse db.dsn") {
			t.Fatalf("err = %v", err)
		}
	})
}
