// Command foundry runs Helix Foundry on one machine: its API and the
// directory of its one owner, foundry-read and its workers, in one process,
// over a MySQL database, an S3-compatible store and a HelixDB, with the
// ontology agent beside it.
//
//	foundry --config /etc/foundry/foundry.yaml
//
// The file is the image's (foundry.yaml here); HELIX_ environment variables
// override any key, as HELIX_DB_DSN does db.dsn and HELIX_LOCAL__SITE_TOKEN
// local.site_token, and local.secrets_dir supplies the secrets left unset.
// It migrates its database at every start.
//
//	foundry init-secrets --dir /secrets [--owner UID:GID] [--reader FILE=GID]...
//
// writes the install's secrets that dir lacks, once (secrets.go), and exits.
// The image's entrypoint passes --config first; the subcommand ignores it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
)

func main() {
	flags := flag.NewFlagSet("foundry", flag.ContinueOnError)
	path := flags.String("config", "", "path to the configuration YAML")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if args := flags.Args(); len(args) > 0 {
		if args[0] != "init-secrets" {
			fmt.Fprintf(os.Stderr, "foundry: unknown command %q\n", args[0])
			os.Exit(2)
		}
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("command", "init-secrets")
		if err := runInitSecrets(args[1:], os.Getenv, logger); err != nil {
			fmt.Fprintln(os.Stderr, "init-secrets:", err)
			os.Exit(1)
		}
		return
	}
	cfg, err := load(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if err := run(context.Background(), cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
