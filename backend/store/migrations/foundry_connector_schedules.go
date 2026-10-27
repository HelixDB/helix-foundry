package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"io"
)

// scheduleSpreadSeconds is the window existing connectors' first scheduled
// syncs are spread over, so a deploy doesn't sync them all at once.
const scheduleSpreadSeconds = 3600

// ScheduleFoundryConnectors gives every live connector that predates
// schedules (migration 20261101000000) its first automatic sync, at a random
// time within the next hour. A connector already scheduled, paused, set to
// manual only, or with a live sync is left alone: the sync's end schedules it.
// A re-run is safe and finds nothing once done.
func ScheduleFoundryConnectors(ctx context.Context, db *sql.DB, apply bool, out io.Writer) error {
	const unscheduled = ` FROM connectors c
WHERE c.deleted_at IS NULL AND c.next_sync_at IS NULL AND c.schedule_paused IS NULL
  AND (c.sync_every_seconds IS NULL OR c.sync_every_seconds > 0)
  AND NOT EXISTS (SELECT 1 FROM connector_syncs s WHERE s.connector_id = c.id AND s.active_key = 1)`
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*)"+unscheduled).Scan(&count); err != nil {
		return fmt.Errorf("count unscheduled connectors: %w", err)
	}
	if !apply {
		_, err := fmt.Fprintf(out, "would schedule %d connectors within the next hour\n", count)
		return err
	}
	res, err := db.ExecContext(ctx, `UPDATE connectors c
SET c.next_sync_at = NOW(6) + INTERVAL FLOOR(RAND() * ?) SECOND
WHERE c.id IN (SELECT id FROM (SELECT c.id`+unscheduled+`) due)`, scheduleSpreadSeconds)
	if err != nil {
		return fmt.Errorf("schedule connectors: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "scheduled %d connectors within the next hour\n", n)
	return err
}
