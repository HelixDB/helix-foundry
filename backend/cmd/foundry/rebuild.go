package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/local"
	"github.com/helixdb/helix-foundry/backend/store"
)

// runRebuildLoads is `foundry rebuild-loads`: it reads
// --current-created-before (RFC 3339) from args, queues a full reload of
// every live ontology's current load created before it in the database at
// dsn (store.RebuildFoundryLoads), and writes to out what it queued, what is
// still reloading, what a live job held back and what it gave up on
// (store.MaxFoundryRebuilds reloads of it failed), and the samples it
// doesn't reload (store.FoundryRebuildSample). Run again until nothing is
// queued, reloading or held back. It fails on arguments it can't read,
// before opening the database, and with every ontology it couldn't queue,
// after writing what it did.
func runRebuildLoads(ctx context.Context, dsn string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("rebuild-loads", flag.ContinueOnError)
	flags.SetOutput(out)
	before := flags.String("current-created-before", "", "reload every current load created before this RFC 3339 time")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", flags.Args())
	}
	if *before == "" {
		return errors.New("--current-created-before is required")
	}
	cutover, err := time.Parse(time.RFC3339Nano, *before)
	if err != nil {
		return fmt.Errorf("--current-created-before: %w", err)
	}
	db, err := openDB(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	got, err := store.RebuildFoundryLoads(ctx, store.NewMySQL(db, local.Workspaces{}), cutover)
	fmt.Fprintf(out, "queued %d reloads: %s\n", len(got.Queued), strings.Join(got.Queued, " "))
	fmt.Fprintf(out, "already reloading, %d: %s\n", len(got.UnderWay), strings.Join(got.UnderWay, " "))
	fmt.Fprintf(out, "held back by a live job, %d (run again once it ends): %s\n", len(got.Busy), strings.Join(got.Busy, " "))
	fmt.Fprintf(out, "given up after %d reloads that failed, %d (load each again by hand): %s\n", store.MaxFoundryRebuilds, len(got.GaveUp),
		strings.Join(got.GaveUp, " "))
	fmt.Fprintf(out, "samples not reloaded, %d (a queued reload of one withdrawn): %s\n", len(got.Samples), strings.Join(got.Samples, " "))
	return err
}
