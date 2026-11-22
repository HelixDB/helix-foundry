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
//
//	foundry --config /etc/foundry/foundry.yaml rebuild-loads --current-created-before=2026-11-22T00:00:00Z
//
// queues a full reload of every live ontology's current load created before
// that time (store.RebuildFoundryLoads), says which a live job held back, and
// exits (rebuild.go). It reads the database the configuration names, which
// it doesn't migrate. Run it again once those jobs end; it never queues a
// reload twice.
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
		switch args[0] {
		case "init-secrets":
			logger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("command", "init-secrets")
			if err := runInitSecrets(args[1:], os.Getenv, logger); err != nil {
				fmt.Fprintln(os.Stderr, "init-secrets:", err)
				os.Exit(1)
			}
		case "rebuild-loads":
			cfg, err := load(*path)
			if err != nil {
				fmt.Fprintln(os.Stderr, "config:", err)
				os.Exit(1)
			}
			if err := runRebuildLoads(context.Background(), cfg.DB.DSN, args[1:], os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, "rebuild-loads:", err)
				os.Exit(1)
			}
		default:
			fmt.Fprintf(os.Stderr, "foundry: unknown command %q\n", args[0])
			os.Exit(2)
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
