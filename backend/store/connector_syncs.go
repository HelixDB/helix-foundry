package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"slices"
	"time"
)

const connectorSyncColumns = `id, connector_id, workspace_id, status, lease_owner, lease_until, attempts,
       next_attempt_at, error, rows_synced, requested_by_user_id, created_at, started_at, finished_at, origin, mode`

// EnqueueConnectorSync queues a sync. ErrConflict means the connector already
// has a live one; GetActiveConnectorSync returns it.
func (s *mysqlDB) EnqueueConnectorSync(ctx context.Context, sync *model.ConnectorSync) error {
	origin, mode := sync.Origin, sync.Mode
	if origin == "" {
		origin = model.ConnectorSyncManual
	}
	if mode == "" {
		mode = model.ConnectorSyncChanges
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO connector_syncs (id, connector_id, workspace_id, status, active_key, requested_by_user_id, origin, mode)
VALUES (?, ?, ?, 'queued', 1, ?, ?, ?)`, sync.ID, sync.ConnectorID, sync.WorkspaceID, sync.RequestedByUserID, string(origin), string(mode))
	if isDuplicateKey(err) {
		return ErrConflict
	}
	return err
}

func (s *mysqlDB) GetActiveConnectorSync(ctx context.Context, workspaceID, connectorID string) (*model.ConnectorSync, error) {
	return scanConnectorSync(s.db.QueryRowContext(ctx, "SELECT "+connectorSyncColumns+`
FROM connector_syncs WHERE connector_id=? AND workspace_id=? AND active_key=1`, connectorID, workspaceID))
}

func (s *mysqlDB) GetConnectorSync(ctx context.Context, workspaceID, id string) (*model.ConnectorSync, error) {
	return scanConnectorSync(s.db.QueryRowContext(ctx, "SELECT "+connectorSyncColumns+`
FROM connector_syncs WHERE id=? AND workspace_id=?`, id, workspaceID))
}

// ListConnectorSyncs returns a connector's syncs, newest first.
func (s *mysqlDB) ListConnectorSyncs(ctx context.Context, workspaceID, connectorID string, limit int) ([]*model.ConnectorSync, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+connectorSyncColumns+`
FROM connector_syncs WHERE connector_id=? AND workspace_id=? ORDER BY created_at DESC, id DESC LIMIT ?`,
		connectorID, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ConnectorSync
	for rows.Next() {
		sync, err := scanConnectorSync(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sync)
	}
	return out, rows.Err()
}

// ClaimConnectorSync leases the oldest due sync to workerID: a queued one whose
// retry time has come, or a running one whose lease expired. Nil means none is due.
func (s *mysqlDB) ClaimConnectorSync(ctx context.Context, workerID string, lease time.Duration) (_ *model.ConnectorSync, err error) {
	if s.root == nil {
		return nil, fmt.Errorf("connector sync claim requires a root database connection")
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin connector sync claim: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	// One running sync per workspace and class keeps a workspace with many
	// connectors from holding every worker. The classes are event sources'
	// syncs (origin events) and the rest: an event source's sync takes
	// seconds, and must not wait hours behind a database's, nor hold one
	// back. The check reads without locking, so two workers claiming at once
	// can still start two of a class's syncs; that costs fairness only, since
	// a connector has one live sync at most. A locking clause locks only the
	// outer query's rows, which is why the subquery takes none; Vitess cannot
	// parse FOR UPDATE OF.
	sync, err := scanConnectorSync(tx.QueryRowContext(ctx, "SELECT "+connectorSyncColumns+`
FROM connector_syncs c
WHERE ((c.status='queued' AND c.next_attempt_at <= NOW(6)) OR (c.status='running' AND c.lease_until < NOW(6)))
  AND NOT EXISTS (SELECT 1 FROM connector_syncs r
    WHERE r.workspace_id=c.workspace_id AND r.status='running' AND r.lease_until >= NOW(6) AND r.id<>c.id
      AND (r.origin='events') = (c.origin='events'))
ORDER BY c.next_attempt_at, c.id
LIMIT 1 FOR UPDATE SKIP LOCKED`))
	if err == ErrNotFound {
		_ = tx.Rollback()
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select connector sync claim: %w", err)
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(lease)
	if _, err = tx.ExecContext(ctx, `
UPDATE connector_syncs
SET status='running', attempts=attempts+1, lease_owner=?, lease_until=?, started_at=COALESCE(started_at, NOW(6))
WHERE id=?`, workerID, leaseUntil, sync.ID); err != nil {
		return nil, fmt.Errorf("claim connector sync: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit connector sync claim: %w", err)
	}
	sync.Status, sync.LeaseOwner, sync.LeaseUntil = model.ConnectorSyncRunning, &workerID, &leaseUntil
	sync.Attempts++
	if sync.StartedAt == nil {
		sync.StartedAt = &now
	}
	return sync, nil
}

// RenewConnectorSyncLease extends the lease; ErrNotFound means workerID lost it.
func (s *mysqlDB) RenewConnectorSyncLease(ctx context.Context, id, workerID string, lease time.Duration) error {
	return execOrNotFound(ctx, s.db, `UPDATE connector_syncs SET lease_until=?
WHERE id=? AND status='running' AND lease_owner=?`, time.Now().UTC().Add(lease), id, workerID)
}

// RequeueConnectorSync gives back workerID's attempt of a sync it stopped
// before the attempt ended, because the worker is stopping: queued again,
// due when it was, and its attempt not counted, so a deploy never brings a
// sync nearer its last attempt. ErrNotFound means workerID no longer holds
// it.
func (s *mysqlDB) RequeueConnectorSync(ctx context.Context, id, workerID string) error {
	return execOrNotFound(ctx, s.db, `UPDATE connector_syncs
SET status='queued', attempts=GREATEST(attempts - 1, 0), lease_owner=NULL, lease_until=NULL
WHERE id=? AND status='running' AND lease_owner=?`, id, workerID)
}

// FinishConnectorSync ends workerID's attempt: requeued for RetryAt, or terminal
// and no longer live, writing the connector's next schedule with it. ErrNotFound
// means the lease was lost.
//
// A pushed schedule writes no interval and is not guarded by one: an event
// source has none. It sets the pause, and the next sync as its PushedNext
// says, so a sync a send scheduled during this one moves only by PushedAt.
func (s *mysqlDB) FinishConnectorSync(ctx context.Context, id, workerID string, outcome model.ConnectorSyncOutcome) error {
	if outcome.RetryAt != nil {
		return execOrNotFound(ctx, s.db, `UPDATE connector_syncs
SET status='queued', next_attempt_at=?, error=NULLIF(?, ''), lease_owner=NULL, lease_until=NULL
WHERE id=? AND status='running' AND lease_owner=?`, outcome.RetryAt.UTC(), outcome.Error, id, workerID)
	}
	if outcome.Status != model.ConnectorSyncSucceeded && outcome.Status != model.ConnectorSyncFailed {
		return fmt.Errorf("connector sync outcome %q is not terminal", outcome.Status)
	}
	var schedule string
	var scheduleArgs []any
	switch next := outcome.Schedule; {
	case next == nil:
	case next.Pushed == nil:
		// An owner who changed the interval during the sync already scheduled
		// the next one by it; the outcome, worked out from the old one, gives way.
		schedule = `UPDATE connectors c JOIN connector_syncs s ON s.connector_id = c.id
SET c.next_sync_at = ?, c.schedule_paused = NULLIF(?, '')
WHERE s.id=? AND c.deleted_at IS NULL AND c.sync_every_seconds <=> ?`
		scheduleArgs = []any{utcPtr(next.NextSyncAt), next.Paused, id, next.Every}
	default:
		if next.NextSyncAt != nil || next.Every != nil {
			return fmt.Errorf("connector sync %s: a pushed schedule has no next sync time or interval of its own", id)
		}
		var at string
		switch p := next.Pushed.(type) {
		case model.PushedKeep:
			at = "c.next_sync_at"
		case model.PushedNotBefore:
			at, scheduleArgs = "GREATEST(COALESCE(c.next_sync_at, ?), ?)", []any{p.At.UTC(), p.At.UTC()}
		case model.PushedAt:
			at, scheduleArgs = "?", []any{p.At.UTC()}
		default:
			return fmt.Errorf("connector sync %s: pushed schedule %T is none of Keep, NotBefore and At", id, p)
		}
		schedule = `UPDATE connectors c JOIN connector_syncs s ON s.connector_id = c.id
SET c.next_sync_at = ` + at + `, c.schedule_paused = NULLIF(?, '')
WHERE s.id=? AND c.deleted_at IS NULL`
		scheduleArgs = append(scheduleArgs, next.Paused, id)
	}
	return s.withConnectorSyncLease(ctx, id, workerID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE connector_syncs
SET status=?, error=NULLIF(?, ''), rows_synced=?, active_key=NULL, lease_owner=NULL, lease_until=NULL,
    finished_at=NOW(6), mode=COALESCE(NULLIF(?, ''), mode)
WHERE id=?`, string(outcome.Status), outcome.Error, outcome.RowsSynced, string(outcome.Mode), id); err != nil {
			return fmt.Errorf("finish connector sync %s: %w", id, err)
		}
		if schedule == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, schedule, scheduleArgs...); err != nil {
			return fmt.Errorf("schedule connector after sync %s: %w", id, err)
		}
		return nil
	})
}

// UpsertConnectorTable points the table at its newest snapshot, only while
// workerID still holds t.SyncID's lease and the connector is live;
// ErrNotFound means it lost the lease or the connector is deleted. It holds
// the connector FOR SHARE, so a DeleteConnector that commits first leaves no
// table behind it. In the same transaction it records the snapshot it
// replaced for the sweep and, with a batch, the table's next change batch,
// setting batch.Seq and t.ChangeSeq.
func (s *mysqlDB) UpsertConnectorTable(ctx context.Context, t *model.ConnectorTable, batch *model.ConnectorChangeBatch, workerID string) error {
	return s.withConnectorSyncLease(ctx, t.SyncID, workerID, func(tx *sql.Tx) error {
		if err := shareLiveConnector(ctx, tx, t.WorkspaceID, t.ConnectorID); err != nil {
			return err
		}
		var previousKey sql.NullString
		var seq int64
		err := tx.QueryRowContext(ctx, `SELECT s3_key, change_seq FROM connector_tables
WHERE connector_id=? AND table_name=? FOR UPDATE`, t.ConnectorID, t.TableName).Scan(&previousKey, &seq)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// A table dropped and chosen again keeps its batches until they
			// expire, so its numbering goes on from them.
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM connector_change_batches
WHERE connector_id=? AND table_name=?`, t.ConnectorID, t.TableName).Scan(&seq); err != nil {
				return fmt.Errorf("read connector table %s batches: %w", t.TableName, err)
			}
		case err != nil:
			return fmt.Errorf("lock connector table %s: %w", t.TableName, err)
		}
		t.ChangeSeq = seq
		if batch != nil {
			batch.Seq = seq + 1
			if err := insertChangeBatch(ctx, tx, batch); err != nil {
				return err
			}
			t.ChangeSeq = batch.Seq
		}
		if previousKey.Valid && previousKey.String != t.S3Key {
			if err := recordReplacedSnapshot(ctx, tx, t.WorkspaceID, t.ConnectorID, previousKey.String); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO connector_tables
  (id, connector_id, workspace_id, table_name, columns, primary_key, foreign_keys, s3_key, row_count, sync_id, synced_at,
   content_digest, change_seq, reconciled_at)
VALUES (?, ?, ?, ?, CAST(? AS JSON), CAST(? AS JSON), CAST(? AS JSON), ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE columns=VALUES(columns), primary_key=VALUES(primary_key), foreign_keys=VALUES(foreign_keys),
  s3_key=VALUES(s3_key), row_count=VALUES(row_count), sync_id=VALUES(sync_id), synced_at=VALUES(synced_at),
  content_digest=VALUES(content_digest), change_seq=VALUES(change_seq),
  reconciled_at=COALESCE(VALUES(reconciled_at), reconciled_at)`,
			t.ID, t.ConnectorID, t.WorkspaceID, t.TableName, string(t.Columns), string(t.PrimaryKey), string(t.ForeignKeys),
			t.S3Key, t.RowCount, t.SyncID, t.SyncedAt.UTC(), t.ContentDigest, t.ChangeSeq, utcPtr(t.ReconciledAt))
		if err != nil {
			return fmt.Errorf("upsert connector table %s: %w", t.TableName, err)
		}
		return nil
	})
}

// TouchConnectorTable records that syncID read the table and found it
// unchanged: its snapshot stays, and reconciled marks a full read.
func (s *mysqlDB) TouchConnectorTable(ctx context.Context, connectorID, table, syncID string, reconciled bool, workerID string) error {
	return s.withConnectorSyncLease(ctx, syncID, workerID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE connector_tables
SET sync_id=?, synced_at=NOW(6), reconciled_at=IF(?, NOW(6), reconciled_at)
WHERE connector_id=? AND table_name=?`, syncID, reconciled, connectorID, table)
		if err != nil {
			return fmt.Errorf("touch connector table %s: %w", table, err)
		}
		return nil
	})
}

func insertChangeBatch(ctx context.Context, tx *sql.Tx, b *model.ConnectorChangeBatch) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO connector_change_batches
  (id, workspace_id, connector_id, table_name, seq, sync_id, kind, s3_key, inserts, updates, deletes)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, b.ID, b.WorkspaceID, b.ConnectorID, b.TableName, b.Seq, b.SyncID, string(b.Kind),
		b.S3Key, b.Inserts, b.Updates, b.Deletes)
	if isDuplicateKey(err) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("insert change batch for %s: %w", b.TableName, err)
	}
	return nil
}

func recordReplacedSnapshot(ctx context.Context, tx Executor, workspaceID, connectorID, key string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO connector_replaced_snapshots (id, workspace_id, connector_id, s3_key)
VALUES (?, ?, ?, ?)`, id.New(), workspaceID, connectorID, key); err != nil {
		return fmt.Errorf("record replaced snapshot: %w", err)
	}
	return nil
}

// PruneConnectorTables removes the connector's tables not named in keep,
// only while workerID still holds syncID's lease and the connector is live
// (ErrNotFound otherwise), and returns their names. Each gets a drop batch,
// and its snapshot waits for the sweep.
func (s *mysqlDB) PruneConnectorTables(ctx context.Context, workspaceID, connectorID string, keep []string, syncID, workerID string) ([]string, error) {
	var removed []string
	err := s.withConnectorSyncLease(ctx, syncID, workerID, func(tx *sql.Tx) error {
		if err := shareLiveConnector(ctx, tx, workspaceID, connectorID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, table_name, s3_key, change_seq FROM connector_tables
WHERE connector_id=? AND workspace_id=? FOR UPDATE`, connectorID, workspaceID)
		if err != nil {
			return fmt.Errorf("list connector tables: %w", err)
		}
		type dropped struct {
			id, table, key string
			seq            int64
		}
		var drops []dropped
		for rows.Next() {
			var d dropped
			if err := rows.Scan(&d.id, &d.table, &d.key, &d.seq); err != nil {
				rows.Close()
				return fmt.Errorf("scan connector table: %w", err)
			}
			if !slices.Contains(keep, d.table) {
				drops = append(drops, d)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list connector tables: %w", err)
		}
		for _, d := range drops {
			batch := &model.ConnectorChangeBatch{ID: id.New(), WorkspaceID: workspaceID, ConnectorID: connectorID, TableName: d.table,
				Seq: d.seq + 1, SyncID: syncID, Kind: model.ConnectorDropBatch}
			if err := insertChangeBatch(ctx, tx, batch); err != nil {
				return err
			}
			if err := recordReplacedSnapshot(ctx, tx, workspaceID, connectorID, d.key); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM connector_tables WHERE id=?`, d.id); err != nil {
				return fmt.Errorf("delete connector table %s: %w", d.table, err)
			}
			removed = append(removed, d.table)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// ListReplacedSnapshots returns up to limit replaced snapshots recorded before before, oldest first.
func (s *mysqlDB) ListReplacedSnapshots(ctx context.Context, before time.Time, limit int) ([]*model.ReplacedSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, workspace_id, connector_id, s3_key, replaced_at
FROM connector_replaced_snapshots WHERE replaced_at < ? ORDER BY replaced_at, id LIMIT ?`, before.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ReplacedSnapshot
	for rows.Next() {
		var r model.ReplacedSnapshot
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.ConnectorID, &r.S3Key, &r.ReplacedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *mysqlDB) DeleteReplacedSnapshot(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connector_replaced_snapshots WHERE id=?`, id)
	return err
}

// ListPinnedSnapshotKeys returns every snapshot key a queued or running load
// job reads; the sweep keeps them.
func (s *mysqlDB) ListPinnedSnapshotKeys(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sources FROM foundry_load_jobs
WHERE status IN ('queued', 'running') AND sources IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var sources []model.FoundryLoadSource
		if err := json.Unmarshal([]byte(raw), &sources); err != nil {
			return nil, fmt.Errorf("decode load job sources: %w", err)
		}
		for _, src := range sources {
			keys = append(keys, src.S3Key)
		}
	}
	return keys, rows.Err()
}

// changeBatchColumns are a batch's columns, of connector_change_batches b.
const changeBatchColumns = `b.id, b.workspace_id, b.connector_id, b.table_name, b.seq, b.sync_id, b.kind, b.s3_key,
  b.inserts, b.updates, b.deletes, b.created_at`

// ListConnectorChangeBatches returns the batches syncID recorded, by table.
func (s *mysqlDB) ListConnectorChangeBatches(ctx context.Context, workspaceID, syncID string) ([]*model.ConnectorChangeBatch, error) {
	return s.queryChangeBatches(ctx, "SELECT "+changeBatchColumns+` FROM connector_change_batches b
WHERE b.workspace_id=? AND b.sync_id=? ORDER BY b.table_name`, workspaceID, syncID)
}

// ListConnectorChangeBatchesAfter returns the table's batches numbered after
// afterSeq up to toSeq, by seq, through the table's unique seq key.
func (s *mysqlDB) ListConnectorChangeBatchesAfter(ctx context.Context, workspaceID, connectorID, table string, afterSeq, toSeq int64) (
	[]*model.ConnectorChangeBatch, error) {
	return s.queryChangeBatches(ctx, "SELECT "+changeBatchColumns+` FROM connector_change_batches b
WHERE b.connector_id=? AND b.table_name=? AND b.seq > ? AND b.seq <= ? AND b.workspace_id=? ORDER BY b.seq`,
		connectorID, table, afterSeq, toSeq, workspaceID)
}

// ListExpiredChangeBatches returns up to limit batches nothing reads again,
// oldest first: those created before before, and those of a deleted
// connector, whatever their age. Each half reads an index: the batches' age,
// or their connector's.
func (s *mysqlDB) ListExpiredChangeBatches(ctx context.Context, before time.Time, limit int) ([]*model.ConnectorChangeBatch, error) {
	return s.queryChangeBatches(ctx, `(SELECT `+changeBatchColumns+` FROM connector_change_batches b
 WHERE b.created_at < ? ORDER BY b.created_at, b.id LIMIT ?)
UNION ALL
(SELECT `+changeBatchColumns+` FROM connectors c JOIN connector_change_batches b ON b.connector_id = c.id
 WHERE c.deleted_at IS NOT NULL AND b.created_at >= ? ORDER BY b.created_at, b.id LIMIT ?)
ORDER BY created_at, id LIMIT ?`, before.UTC(), limit, before.UTC(), limit, limit)
}

func (s *mysqlDB) DeleteConnectorChangeBatch(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connector_change_batches WHERE id=?`, id)
	return err
}

func (s *mysqlDB) queryChangeBatches(ctx context.Context, query string, args ...any) ([]*model.ConnectorChangeBatch, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ConnectorChangeBatch
	for rows.Next() {
		var b model.ConnectorChangeBatch
		var kind string
		var key sql.NullString
		if err := rows.Scan(&b.ID, &b.WorkspaceID, &b.ConnectorID, &b.TableName, &b.Seq, &b.SyncID, &kind, &key,
			&b.Inserts, &b.Updates, &b.Deletes, &b.CreatedAt); err != nil {
			return nil, err
		}
		b.Kind = model.ConnectorChangeKind(kind)
		if key.Valid {
			b.S3Key = &key.String
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

// withConnectorSyncLease runs fn in a transaction that holds the sync row
// locked, so a lease takeover cannot interleave with fn's writes.
func (s *mysqlDB) withConnectorSyncLease(ctx context.Context, syncID, workerID string, fn func(*sql.Tx) error) (err error) {
	if s.root == nil {
		return fmt.Errorf("connector table write requires a root database connection")
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin connector table write: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var owned string
	err = tx.QueryRowContext(ctx, `SELECT id FROM connector_syncs
WHERE id=? AND status='running' AND lease_owner=? FOR UPDATE`, syncID, workerID).Scan(&owned)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock connector sync %s: %w", syncID, err)
	}
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit connector table write: %w", err)
	}
	return nil
}

func (s *mysqlDB) ListConnectorTables(ctx context.Context, workspaceID, connectorID string) ([]*model.ConnectorTable, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, connector_id, workspace_id, table_name, columns, primary_key, foreign_keys, s3_key, row_count, sync_id, synced_at,
  content_digest, change_seq, reconciled_at
FROM connector_tables WHERE connector_id=? AND workspace_id=? ORDER BY table_name`, connectorID, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ConnectorTable
	for rows.Next() {
		var t model.ConnectorTable
		var columns, primaryKey, foreignKeys string
		var reconciled sql.NullTime
		if err := rows.Scan(&t.ID, &t.ConnectorID, &t.WorkspaceID, &t.TableName, &columns, &primaryKey, &foreignKeys,
			&t.S3Key, &t.RowCount, &t.SyncID, &t.SyncedAt, &t.ContentDigest, &t.ChangeSeq, &reconciled); err != nil {
			return nil, err
		}
		t.Columns, t.PrimaryKey, t.ForeignKeys = []byte(columns), []byte(primaryKey), []byte(foreignKeys)
		if reconciled.Valid {
			t.ReconciledAt = &reconciled.Time
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

func scanConnectorSync(scanner rowScanner) (*model.ConnectorSync, error) {
	var sync model.ConnectorSync
	var status string
	var leaseOwner, errText, requestedBy sql.NullString
	var leaseUntil, startedAt, finishedAt sql.NullTime
	var rowsSynced sql.NullInt64
	var origin, mode string
	if err := scanner.Scan(&sync.ID, &sync.ConnectorID, &sync.WorkspaceID, &status, &leaseOwner, &leaseUntil,
		&sync.Attempts, &sync.NextAttemptAt, &errText, &rowsSynced, &requestedBy, &sync.CreatedAt, &startedAt, &finishedAt,
		&origin, &mode,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	sync.Status = model.ConnectorSyncStatus(status)
	sync.Origin, sync.Mode = model.ConnectorSyncOrigin(origin), model.ConnectorSyncMode(mode)
	if leaseOwner.Valid {
		sync.LeaseOwner = &leaseOwner.String
	}
	if leaseUntil.Valid {
		sync.LeaseUntil = &leaseUntil.Time
	}
	if errText.Valid {
		sync.Error = &errText.String
	}
	if rowsSynced.Valid {
		sync.RowsSynced = &rowsSynced.Int64
	}
	if requestedBy.Valid {
		sync.RequestedByUserID = &requestedBy.String
	}
	if startedAt.Valid {
		sync.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		sync.FinishedAt = &finishedAt.Time
	}
	return &sync, nil
}
