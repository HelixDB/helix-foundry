package local

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
)

// owedRebuild is a row of foundry_local_rebuilds not yet done: a release
// whose engine writes the graph differently, and when this install first
// started it.
type owedRebuild struct {
	name  string
	since time.Time
}

// RebuildOwedLoads queues the rebuilds this install owes, as hosted runs
// store.RebuildFoundryLoads per stamp: for each row of
// foundry_local_rebuilds not yet done (a directory migration adds one for a
// release whose engine writes the graph differently, such as 00005's typed
// DateTime, at the time it ran), a full reload of every live ontology's
// current load created before then. A row is marked done once nothing is
// left to queue or wait for: no rebuild queued or under way, none held
// back by another live job, none that failed to queue. One whose rebuilds
// keep ending without replacing its load is given up on after
// store.MaxFoundryRebuilds, and logged; a sample's load is never reloaded
// (store.FoundryRebuildSample). Until a row is done each call tries
// again (Rebuilds calls it while one is owed), and what a call already
// queued is not queued twice. It reports whether a row is still owed. Only
// reading or marking the rows fails it; every ontology it couldn't queue is
// logged. run calls it after migrating, before the workers start.
func RebuildOwedLoads(ctx context.Context, db *sql.DB, st store.Store, logger *slog.Logger) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, owed_since FROM foundry_local_rebuilds WHERE done_at IS NULL ORDER BY owed_since, name`)
	if err != nil {
		return false, fmt.Errorf("read owed rebuilds: %w", err)
	}
	// Read whole before any rebuild: the rows' connection is free once
	// they are.
	defer func() { _ = rows.Close() }()
	var owed []owedRebuild
	for rows.Next() {
		var r owedRebuild
		if err := rows.Scan(&r.name, &r.since); err != nil {
			return false, fmt.Errorf("read owed rebuilds: %w", err)
		}
		owed = append(owed, r)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read owed rebuilds: %w", err)
	}
	pending := false
	for _, r := range owed {
		got, err := store.RebuildFoundryLoads(ctx, st, r.since)
		log := logger.With("rebuild", r.name, "current_created_before", r.since)
		if len(got.Queued) > 0 {
			log.Info("queued full reloads of current loads an earlier release wrote", "ontologies", got.Queued)
		}
		if len(got.GaveUp) > 0 {
			log.Warn("current loads an earlier release wrote whose reloads keep failing are not reloaded again; load them again by hand",
				"ontologies", got.GaveUp)
		}
		if len(got.Samples) > 0 {
			log.Info("the sample's loads are not reloaded: Helix converted their datetimes exactly", "ontologies", got.Samples)
		}
		if err != nil || len(got.Queued)+len(got.UnderWay)+len(got.Busy) > 0 {
			if err != nil || len(got.Busy) > 0 {
				log.Warn("some current loads an earlier release wrote are not queued for a reload yet; foundry tries again shortly",
					"busy", got.Busy, "err", err)
			}
			pending = true
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE foundry_local_rebuilds SET done_at=NOW(6) WHERE name=?`, r.name); err != nil {
			return true, fmt.Errorf("mark rebuild %s done: %w", r.name, err)
		}
		log.Info("every current load an earlier release wrote is reloaded")
	}
	return pending, nil
}

// RebuildEvery is how often Rebuilds runs RebuildOwedLoads.
const RebuildEvery = time.Minute

// Rebuilds is the part of an install that owes a rebuild (RebuildOwedLoads
// reported one owed at start): it runs RebuildOwedLoads every Every until
// nothing is owed, then ends, so a rebuild a live job held back is queued
// once that job ends, and one that failed or was cancelled is queued again.
// A failed run is logged and tried again.
type Rebuilds struct {
	DB     *sql.DB
	Store  store.Store
	Logger *slog.Logger
	Every  time.Duration
}

func (r Rebuilds) Name() string { return "rebuilds" }

// Start runs the rebuilds until nothing is owed or ctx ends.
func (r Rebuilds) Start(ctx context.Context) error {
	tick := time.NewTicker(r.Every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		owed, err := RebuildOwedLoads(ctx, r.DB, r.Store, r.Logger)
		if err != nil {
			r.Logger.Warn("rebuilds: try again shortly", "err", err)
			continue
		}
		if !owed {
			return nil
		}
	}
}

// Stop does nothing: Start ends with its context.
func (Rebuilds) Stop(context.Context) error { return nil }
