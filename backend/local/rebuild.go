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
// store.RebuildFoundryLoads once per stamp: for each row of
// foundry_local_rebuilds not yet done (a directory migration adds one for a
// release whose engine writes the graph differently, such as 00004's typed
// DateTime, at the time it ran), a full reload of every live ontology's
// current load created before then. A row is marked done once none was held
// back by another live job or failed to queue (one whose rebuild an earlier
// start queued is under way); until then each start tries again, and what a
// start already queued is not queued twice. Only reading or
// marking the rows fails it; every ontology it couldn't queue is logged.
// run calls it after migrating, before the workers start.
func RebuildOwedLoads(ctx context.Context, db *sql.DB, st store.Store, logger *slog.Logger) error {
	rows, err := db.QueryContext(ctx, `SELECT name, owed_since FROM foundry_local_rebuilds WHERE done_at IS NULL ORDER BY owed_since, name`)
	if err != nil {
		return fmt.Errorf("read owed rebuilds: %w", err)
	}
	// Read whole before any rebuild: the rows' connection is free once
	// they are.
	defer func() { _ = rows.Close() }()
	var owed []owedRebuild
	for rows.Next() {
		var r owedRebuild
		if err := rows.Scan(&r.name, &r.since); err != nil {
			return fmt.Errorf("read owed rebuilds: %w", err)
		}
		owed = append(owed, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read owed rebuilds: %w", err)
	}
	for _, r := range owed {
		got, err := store.RebuildFoundryLoads(ctx, st, r.since)
		log := logger.With("rebuild", r.name, "current_created_before", r.since)
		if len(got.Queued) > 0 {
			log.Info("queued full reloads of current loads an earlier release wrote", "ontologies", got.Queued)
		}
		if err != nil || len(got.Busy) > 0 {
			log.Warn("some current loads an earlier release wrote are not queued for a reload yet; the next start tries again",
				"busy", got.Busy, "err", err)
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE foundry_local_rebuilds SET done_at=NOW(6) WHERE name=?`, r.name); err != nil {
			return fmt.Errorf("mark rebuild %s done: %w", r.name, err)
		}
	}
	return nil
}
