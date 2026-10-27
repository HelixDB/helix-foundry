package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"strings"
	"time"
)

// utcPtr passes a time to MySQL in UTC, keeping nil as NULL.
func utcPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

const connectorColumns = `id, workspace_id, kind, name, config, secret_ciphertext, secret_key_id,
       secret_hint, created_by_user_id, created_at, updated_at, sync_every_seconds, next_sync_at, schedule_paused, change_cursor`

func (s *mysqlDB) CreateConnector(ctx context.Context, c *model.Connector) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO connectors
  (id, workspace_id, kind, name, config, secret_ciphertext, secret_key_id, secret_hint, created_by_user_id,
   sync_every_seconds, next_sync_at)
VALUES (?, ?, ?, ?, CAST(? AS JSON), ?, ?, ?, ?, ?, ?)`,
		c.ID, c.WorkspaceID, c.Kind, c.Name, string(c.Config), c.SecretCiphertext, c.SecretKeyID,
		c.SecretHint, c.CreatedByUserID, c.SyncEverySeconds, utcPtr(c.NextSyncAt),
	)
	return err
}

func (s *mysqlDB) GetConnector(ctx context.Context, workspaceID, id string) (*model.Connector, error) {
	return scanConnector(s.db.QueryRowContext(ctx, "SELECT "+connectorColumns+`
FROM connectors WHERE id=? AND workspace_id=? AND deleted_at IS NULL`, id, workspaceID))
}

func (s *mysqlDB) ListConnectors(ctx context.Context, workspaceID string) ([]*model.Connector, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+connectorColumns+`
FROM connectors WHERE workspace_id=? AND deleted_at IS NULL ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Connector
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *mysqlDB) UpdateConnector(ctx context.Context, workspaceID, id string, update model.ConnectorUpdate) error {
	var assignments []string
	var args []any
	if update.Name != nil {
		assignments = append(assignments, "name=?")
		args = append(args, *update.Name)
	}
	if update.Config != nil {
		// New settings may point elsewhere, so the change cursor no longer applies.
		assignments = append(assignments, "config=CAST(? AS JSON)", "secret_ciphertext=?", "secret_key_id=?", "secret_hint=?",
			"change_cursor=NULL")
		args = append(args, string(update.Config), update.SecretCiphertext, update.SecretKeyID, update.SecretHint)
	}
	if update.Schedule != nil {
		assignments = append(assignments, "sync_every_seconds=?", "next_sync_at=?", "schedule_paused=NULL")
		args = append(args, update.Schedule.SyncEverySeconds, utcPtr(update.Schedule.NextSyncAt))
	}
	if len(assignments) == 0 {
		return fmt.Errorf("connector update is empty")
	}
	assignments = append(assignments, "updated_at=NOW(6)")
	args = append(args, id, workspaceID)
	return execOrNotFound(ctx, s.db, `UPDATE connectors SET `+strings.Join(assignments, ", ")+`
WHERE id=? AND workspace_id=? AND deleted_at IS NULL`, args...)
}

// DeleteConnector deletes a live connector of the workspace with what its
// syncs wrote (deleteConnector), in one transaction that locks as every
// writer of documents does: the workspace's live ontologies, then the
// connector, FOR UPDATE, then its sets and documents. What its tables and
// documents put in the graph stays until a load without them is current, so
// each ontology with refresh on whose current load reads one of its tables
// (OweFoundryRefreshes), or that reads one of its sets as a sync's removal
// sees it (oweFoundrySetRefreshes), owes a refresh due now. ErrNotFound
// means the workspace has no such live connector.
func (s *mysqlDB) DeleteConnector(ctx context.Context, workspaceID, id string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		ontologies, err := tx.lockLiveFoundryOntologies(ctx, workspaceID)
		if err != nil {
			return err
		}
		var locked string
		err = tx.db.QueryRowContext(ctx, `SELECT id FROM connectors WHERE id=? AND workspace_id=? AND deleted_at IS NULL FOR UPDATE`,
			id, workspaceID).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock connector %s: %w", id, err)
		}
		tables, sets, err := tx.deleteConnector(ctx, workspaceID, id)
		if err != nil {
			return err
		}
		if len(tables) > 0 {
			if err := tx.OweFoundryRefreshes(ctx, workspaceID, id, tables, 0); err != nil {
				return err
			}
		}
		if len(sets) == 0 {
			return nil
		}
		return tx.oweFoundrySetRefreshes(ctx, ontologies, sets, 0)
	})
}

// DeleteWorkspaceConnectors deletes every live connector of a workspace
// being deleted, with what its syncs wrote, as DeleteConnector does but
// owing no refresh: the workspace's ontologies go with it. ErrConflict means
// a sync of one of the workspace's connectors is still live; the caller
// retries once it ends. None starts reading again: a sync of a workspace
// being deleted fails at its start (syncer.Run).
func (s *mysqlDB) DeleteWorkspaceConnectors(ctx context.Context, workspaceID string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var live bool
		if err := tx.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM connectors c
  JOIN connector_syncs s ON s.connector_id = c.id AND s.active_key = 1 WHERE c.workspace_id=?)`, workspaceID).Scan(&live); err != nil {
			return fmt.Errorf("find live syncs: %w", err)
		}
		if live {
			return ErrConflict
		}
		if _, err := tx.lockLiveFoundryOntologies(ctx, workspaceID); err != nil {
			return err
		}
		rows, err := tx.db.QueryContext(ctx, `SELECT id FROM connectors WHERE workspace_id=? AND deleted_at IS NULL ORDER BY id FOR UPDATE`,
			workspaceID)
		if err != nil {
			return fmt.Errorf("lock connectors: %w", err)
		}
		ids, err := scanStrings(rows)
		if err != nil {
			return fmt.Errorf("lock connectors: %w", err)
		}
		for _, id := range ids {
			if _, _, err := tx.deleteConnector(ctx, workspaceID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// deleteConnector deletes a connector of the workspace and what its syncs
// wrote, and returns the names of the tables and the ids of the sets it had.
// The caller holds the workspace's live ontologies, then the connector, FOR
// UPDATE; this locks its sets, documents and tables after them:
//   - its sets are deleted, and so is each of their live documents, whose
//     prefix waits for the sweep (a purge row each, ListFoundryDocumentPurges);
//   - each table's snapshot waits for the sweep as a replaced one does
//     (ListReplacedSnapshots), sparing it while a live load job pins it,
//     and the table goes;
//   - its change batches go at the sweep's next tick (ListExpiredChangeBatches);
//   - an event source's live send tokens are revoked, so none outlives it,
//     and its batches wait for the sync worker's purge of deleted sources
//     (ListFoundryEventBatchPurges). No other kind has either.
func (s *mysqlDB) deleteConnector(ctx context.Context, workspaceID, id string) (tables, sets []string, err error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE connectors SET deleted_at=NOW(6), updated_at=NOW(6), next_sync_at=NULL WHERE id=?`,
		id); err != nil {
		return nil, nil, fmt.Errorf("delete connector %s: %w", id, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_event_source_tokens SET revoked_at=NOW(6)
WHERE connector_id=? AND workspace_id=? AND revoked_at IS NULL`, id, workspaceID); err != nil {
		return nil, nil, fmt.Errorf("revoke connector %s send tokens: %w", id, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM foundry_document_sets
WHERE connector_id=? AND workspace_id=? AND live_key=1 ORDER BY id FOR UPDATE`, id, workspaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("lock connector %s sets: %w", id, err)
	}
	if sets, err = scanStrings(rows); err != nil {
		return nil, nil, fmt.Errorf("lock connector %s sets: %w", id, err)
	}
	if len(sets) > 0 {
		in := idPlaceholders(len(sets))
		if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_document_purges (document_id, workspace_id)
SELECT id, workspace_id FROM foundry_documents WHERE set_id IN (`+in+`) AND status='uploaded'`, toAnySlice(sets)...); err != nil {
			return nil, nil, fmt.Errorf("purge connector %s documents: %w", id, err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_documents SET status='deleted', deleted_at=NOW(6)
WHERE set_id IN (`+in+`) AND status='uploaded'`, toAnySlice(sets)...); err != nil {
			return nil, nil, fmt.Errorf("delete connector %s documents: %w", id, err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_document_sets SET deleted_at=NOW(6), live_key=NULL WHERE id IN (`+in+`)`,
			toAnySlice(sets)...); err != nil {
			return nil, nil, fmt.Errorf("delete connector %s sets: %w", id, err)
		}
	}
	rows, err = s.db.QueryContext(ctx, `SELECT table_name, s3_key FROM connector_tables
WHERE connector_id=? AND workspace_id=? ORDER BY table_name FOR UPDATE`, id, workspaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("lock connector %s tables: %w", id, err)
	}
	var keys []string
	for rows.Next() {
		var table, key string
		if err := rows.Scan(&table, &key); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("scan connector %s table: %w", id, err)
		}
		tables, keys = append(tables, table), append(keys, key)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("lock connector %s tables: %w", id, err)
	}
	for _, key := range keys {
		if err := recordReplacedSnapshot(ctx, s.db, workspaceID, id, key); err != nil {
			return nil, nil, err
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM connector_tables WHERE connector_id=? AND workspace_id=?`, id, workspaceID); err != nil {
		return nil, nil, fmt.Errorf("delete connector %s tables: %w", id, err)
	}
	return tables, sets, nil
}

// QueueDueConnectorSyncs queues a schedule sync for each connector whose
// next_sync_at has come, at most limit, and returns how many it queued. The
// rows are locked SKIP LOCKED, so two schedulers ticking at once share them;
// a connector with a live sync keeps it and is unscheduled until it ends.
//
// An event source's sync is queued with origin events. One with a live sync
// is due again eventsAfter from now instead: the pieces that made it due may
// have come after that sync read what waits (connector.Pushed), and nothing
// else would schedule them, since a send schedules a sync only when none is.
func (s *mysqlDB) QueueDueConnectorSyncs(ctx context.Context, limit int, eventsAfter time.Duration) (_ int, err error) {
	if s.root == nil {
		return 0, fmt.Errorf("queue connector syncs requires a root database connection")
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin queue connector syncs: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.QueryContext(ctx, `SELECT id, workspace_id, kind FROM connectors
WHERE next_sync_at <= NOW(6) AND deleted_at IS NULL
ORDER BY next_sync_at, id LIMIT ? FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, fmt.Errorf("select due connectors: %w", err)
	}
	type due struct{ id, workspaceID, kind string }
	var connectors []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.workspaceID, &d.kind); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan due connector: %w", err)
		}
		connectors = append(connectors, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("select due connectors: %w", err)
	}
	queued := 0
	for _, c := range connectors {
		events := c.kind == model.ConnectorKindEvents
		origin := model.ConnectorSyncSchedule
		if events {
			origin = model.ConnectorSyncEvents
		}
		res, err := tx.ExecContext(ctx, `
INSERT IGNORE INTO connector_syncs (id, connector_id, workspace_id, status, active_key, origin, mode)
VALUES (?, ?, ?, 'queued', 1, ?, 'changes')`, id.New(), c.id, c.workspaceID, string(origin))
		if err != nil {
			return 0, fmt.Errorf("queue sync of connector %s: %w", c.id, err)
		}
		n, _ := res.RowsAffected()
		if n == 1 {
			queued++
		}
		next, args := "NULL", []any{c.id}
		if n == 0 && events {
			next, args = "NOW(6) + INTERVAL ? MICROSECOND", []any{eventsAfter.Microseconds(), c.id}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE connectors SET next_sync_at=`+next+` WHERE id=?`, args...); err != nil {
			return 0, fmt.Errorf("unschedule connector %s: %w", c.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit queue connector syncs: %w", err)
	}
	return queued, nil
}

// UpdateConnectorCursor stores where the connector's next change read starts,
// only while workerID still holds syncID's lease.
func (s *mysqlDB) UpdateConnectorCursor(ctx context.Context, connectorID string, cursor json.RawMessage, syncID, workerID string) error {
	return s.withConnectorSyncLease(ctx, syncID, workerID, func(tx *sql.Tx) error {
		var value any
		if cursor != nil {
			value = string(cursor)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE connectors SET change_cursor=CAST(? AS JSON) WHERE id=?`, value, connectorID); err != nil {
			return fmt.Errorf("update connector %s cursor: %w", connectorID, err)
		}
		return nil
	})
}

// ListStrandedConnectorCursors lists connectors of kinds whose change cursor
// holds something on the source, such as a replication slot, that no sync
// will read again: deleted in the last 30 days, in a deleted workspace, or
// with a paused schedule. A connector with a live sync is left to that sync.
func (s *mysqlDB) ListStrandedConnectorCursors(ctx context.Context, kinds []string, limit int) ([]*model.Connector, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(kinds)+1)
	for _, k := range kinds {
		args = append(args, k)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+connectorColumns+`
FROM connectors c
WHERE c.change_cursor IS NOT NULL AND c.kind IN (`+idPlaceholders(len(kinds))+`)
  AND (c.deleted_at > NOW(6) - INTERVAL 30 DAY
       OR c.deleted_at IS NULL AND (c.schedule_paused IS NOT NULL OR c.workspace_id IN (
         SELECT w.id FROM `+s.workspaces.Table()+` w WHERE w.deleted_at IS NOT NULL OR w.status IN ('deleting', 'deleted'))))
  AND NOT EXISTS (SELECT 1 FROM connector_syncs s WHERE s.connector_id = c.id AND s.active_key = 1)
ORDER BY c.id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list stranded connector cursors: %w", err)
	}
	defer rows.Close()
	var out []*model.Connector
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, fmt.Errorf("scan stranded connector: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClearConnectorCursor forgets cursor once what it held on the source is
// released. A cursor a sync has since replaced, or one a live sync is
// reading, stays.
func (s *mysqlDB) ClearConnectorCursor(ctx context.Context, connectorID string, cursor json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `UPDATE connectors SET change_cursor=NULL
WHERE id=? AND change_cursor = CAST(? AS JSON)
  AND NOT EXISTS (SELECT 1 FROM connector_syncs s WHERE s.connector_id = ? AND s.active_key = 1)`,
		connectorID, string(cursor), connectorID)
	if err != nil {
		return fmt.Errorf("clear connector %s cursor: %w", connectorID, err)
	}
	return nil
}

func scanConnector(scanner rowScanner) (*model.Connector, error) {
	var c model.Connector
	var config string
	var createdBy, paused, cursor sql.NullString
	var every sql.NullInt64
	var next sql.NullTime
	if err := scanner.Scan(
		&c.ID, &c.WorkspaceID, &c.Kind, &c.Name, &config, &c.SecretCiphertext, &c.SecretKeyID,
		&c.SecretHint, &createdBy, &c.CreatedAt, &c.UpdatedAt, &every, &next, &paused, &cursor,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.Config = []byte(config)
	if createdBy.Valid {
		c.CreatedByUserID = &createdBy.String
	}
	if every.Valid {
		seconds := int(every.Int64)
		c.SyncEverySeconds = &seconds
	}
	if next.Valid {
		c.NextSyncAt = &next.Time
	}
	if paused.Valid {
		c.SchedulePaused = &paused.String
	}
	if cursor.Valid {
		c.ChangeCursor = json.RawMessage(cursor.String)
	}
	return &c, nil
}
