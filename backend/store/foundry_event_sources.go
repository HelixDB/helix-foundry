package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/events"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
)

// foundry_event_sources.go stores Foundry's event sources (connector kind
// events): the sources, their batches and the send that adds one, their
// send-only tokens, the reads the events kind syncs from, the purge of
// deleted sources and the pruning of their old syncs. Package events holds
// every rule of a send; this file applies them to what it reads under the
// source's lock.
//
// Every send of a source takes its connectors row lock (FOR UPDATE) as its
// transaction's first statement and holds it to commit, so its seq, taken
// under the lock, is commit order. The transaction's other reads are plain
// reads after the lock, and InnoDB takes a REPEATABLE READ transaction's view
// at its first plain read: they see every send committed before the lock.

// ErrNotEventSource is a connector of another kind where an event source is needed.
var ErrNotEventSource = errors.New("store: connector is not an event source")

// EventSourceNameTakenError is a name a live event source of the workspace
// already has: ConnectorID is that source. It is an ErrConflict.
type EventSourceNameTakenError struct {
	ConnectorID string
}

func (e *EventSourceNameTakenError) Error() string {
	return "store: event source " + e.ConnectorID + " has this name"
}

func (e *EventSourceNameTakenError) Unwrap() error { return ErrConflict }

// EventSourcePausedError is a send to a source whose syncs a permanent
// failure paused: Reason is its schedule_paused. Its pieces could not sync
// until an owner resumes it.
type EventSourcePausedError struct {
	Reason string
}

func (e *EventSourcePausedError) Error() string {
	return "store: event source is paused: " + e.Reason
}

const eventBatchColumns = `id, workspace_id, connector_id, seq, client_token, s3_key, events, bytes, first_at, last_at, created_at`

const eventSourceTokenColumns = `t.id, t.workspace_id, t.connector_id, t.hash, t.prefix, t.created_by, t.created_at, t.revoked_at,
       t.last_used_at`

// liveEventSourceTokens is the tokens that authenticate: not revoked, of a
// live event source of the token's own workspace, in a live workspace. A
// token whose connector somehow moved workspace never matches.
func (s *mysqlDB) liveEventSourceTokens() string {
	return ` FROM foundry_event_source_tokens t
JOIN connectors c ON c.id = t.connector_id AND c.workspace_id = t.workspace_id AND c.deleted_at IS NULL AND c.kind = 'events'
JOIN ` + s.workspaces.Table() + ` w ON w.id = t.workspace_id AND w.deleted_at IS NULL
WHERE t.revoked_at IS NULL AND `
}

// purgeableEventSources are the ids of event sources a purge empties: those
// deleted before the bound. A deleted workspace's sources are among them,
// since its delete deletes its connectors first (DeleteWorkspaceConnectors),
// and no send reaches a deleted source, so none gains a batch again.
const purgeableEventSources = `SELECT c.id FROM connectors c WHERE c.kind = 'events' AND c.deleted_at < ?`

// CreateEventSource stores c, a new connector of kind events, under the
// workspace's lock. A live event source of the workspace with c's name, in
// the column's collation, is an *EventSourceNameTakenError naming it, and a
// workspace that already has max live event sources is ErrLimitExceeded.
// ErrNotFound is a workspace that is gone.
func (s *mysqlDB) CreateEventSource(ctx context.Context, c *model.Connector, max int) error {
	if c.Kind != model.ConnectorKindEvents {
		return fmt.Errorf("create event source: kind is %q", c.Kind)
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, c.WorkspaceID); err != nil {
			return err
		}
		if err := tx.eventSourceNameFree(ctx, c.WorkspaceID, c.Name, c.ID); err != nil {
			return err
		}
		var n int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM connectors
WHERE workspace_id=? AND kind='events' AND deleted_at IS NULL`, c.WorkspaceID).Scan(&n); err != nil {
			return fmt.Errorf("count event sources: %w", err)
		}
		if n >= max {
			return ErrLimitExceeded
		}
		return tx.CreateConnector(ctx, c)
	})
}

// eventSourceNameFree is nil when no live event source of the workspace but
// exceptID has name, in the column's collation, and otherwise an
// *EventSourceNameTakenError naming the oldest that has. The caller holds the
// workspace's lock.
func (s *mysqlDB) eventSourceNameFree(ctx context.Context, workspaceID, name, exceptID string) error {
	var taken string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM connectors
WHERE workspace_id=? AND kind='events' AND name=? AND id<>? AND deleted_at IS NULL
ORDER BY created_at, id LIMIT 1`, workspaceID, name, exceptID).Scan(&taken)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("read event source names: %w", err)
	}
	return &EventSourceNameTakenError{ConnectorID: taken}
}

// UpdateEventSource renames a live event source of the workspace, replaces
// its description (config.description) or changes its text field
// (config.text_field), in one transaction that holds the source's row lock,
// the one a send takes (FOR UPDATE), from before it reads the source:
//
//   - A rename takes the workspace's lock first and refuses a name another
//     live event source has, as CreateEventSource does.
//   - The description and the text field are set in place with one
//     JSON_SET, each only when the update carries it, so the schema a
//     concurrent send learns stays.
//   - A new text field is locked as a string in the locked schema
//     (events.Schema.WithText), whose *events.Refusal it returns, and the
//     schema is written when the lock changed it. It queues a full sync of
//     origin events, which renders the source's newest pieces by the new
//     field; a live sync of the source is ErrConflict, since it read the old
//     one. A text field the source already has writes and queues nothing.
//
// A sync of the source that finishes meanwhile takes the same two locks the
// other way round (FinishConnectorSync clears its active_key, then sets the
// source's next sync), and InnoDB rolls one of the two back. When that is
// this update, it runs again once, from the start; a second deadlock is
// ErrConflict, as a live sync is.
//
// Any error leaves nothing written. The change cursor, next_sync_at and the
// sealed secret are never written. ErrNotFound is no live event source with
// this id in the workspace.
func (s *mysqlDB) UpdateEventSource(ctx context.Context, workspaceID, connectorID string, update model.EventSourceUpdate) error {
	if update.Name == nil && update.Description == nil && update.Text == nil {
		return fmt.Errorf("event source update is empty")
	}
	attempt := func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var assignments, paths []string
		var args, values []any
		if update.Name != nil {
			if err := tx.workspaces.Lock(ctx, tx.db, workspaceID); err != nil {
				return err
			}
			if err := tx.eventSourceNameFree(ctx, workspaceID, *update.Name, connectorID); err != nil {
				return err
			}
			assignments = append(assignments, "name=?")
			args = append(args, *update.Name)
		}
		var config string
		err := tx.db.QueryRowContext(ctx, `SELECT config FROM connectors
WHERE id=? AND workspace_id=? AND kind='events' AND deleted_at IS NULL FOR UPDATE`, connectorID, workspaceID).Scan(&config)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock event source %s: %w", connectorID, err)
		}
		if update.Description != nil {
			paths, values = append(paths, "'$.description', ?"), append(values, *update.Description)
		}
		if text := update.Text; text != nil {
			var stored struct {
				TextField string        `json:"text_field"`
				Schema    events.Schema `json:"schema"`
			}
			if err := json.Unmarshal([]byte(config), &stored); err != nil {
				return fmt.Errorf("read event source %s config: %w", connectorID, err)
			}
			if stored.TextField != text.Field {
				if text.Field != "" {
					locked, err := stored.Schema.WithText(text.Field, text.MaxFields)
					if err != nil {
						return err
					}
					if !slices.Equal(locked.Fields(), stored.Schema.Fields()) {
						schema, err := json.Marshal(locked)
						if err != nil {
							return fmt.Errorf("encode event source schema: %w", err)
						}
						paths, values = append(paths, "'$.schema', CAST(? AS JSON)"), append(values, string(schema))
					}
				}
				paths, values = append(paths, "'$.text_field', ?"), append(values, text.Field)
				if err := tx.EnqueueConnectorSync(ctx, &model.ConnectorSync{
					ID: id.New(), ConnectorID: connectorID, WorkspaceID: workspaceID, RequestedByUserID: text.RequestedByUserID,
					Origin: model.ConnectorSyncEvents, Mode: model.ConnectorSyncFull,
				}); err != nil {
					return err
				}
			}
		}
		if len(paths) > 0 {
			assignments = append(assignments, "config=JSON_SET(config, "+joinComma(paths)+")")
			args = append(args, values...)
		}
		if len(assignments) == 0 {
			return nil
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE connectors SET `+joinComma(assignments)+`, updated_at=NOW(6) WHERE id=?`,
			append(args, connectorID)...)
		if err != nil {
			return fmt.Errorf("update event source %s: %w", connectorID, err)
		}
		return nil
	}
	err := s.WithTx(ctx, attempt)
	if isDeadlock(err) {
		err = s.WithTx(ctx, attempt)
	}
	if isDeadlock(err) {
		return ErrConflict
	}
	return err
}

// eventSourceState reads a live event source of the workspace, its events
// table and the sums of its batches above its cursor. With lock it takes the
// source's row lock first, as a send does. ErrNotFound is no live connector
// with this id in the workspace, ErrNotEventSource one of another kind.
func (s *mysqlDB) eventSourceState(ctx context.Context, workspaceID, connectorID string, lock bool) (model.FoundryEventSourceState, error) {
	q := "SELECT " + connectorColumns + ` FROM connectors WHERE id=? AND workspace_id=? AND deleted_at IS NULL`
	if lock {
		q += " FOR UPDATE"
	}
	c, err := scanConnector(s.db.QueryRowContext(ctx, q, connectorID, workspaceID))
	if err != nil {
		return model.FoundryEventSourceState{}, err
	}
	if c.Kind != model.ConnectorKindEvents {
		return model.FoundryEventSourceState{}, ErrNotEventSource
	}
	schema, cursor, err := eventSourceProgress(c.ID, c.Config, c.ChangeCursor)
	if err != nil {
		return model.FoundryEventSourceState{}, err
	}
	state := model.FoundryEventSourceState{Connector: c, Schema: schema, Cursor: cursor}
	var columns string
	err = s.db.QueryRowContext(ctx, `SELECT s3_key, row_count, columns FROM connector_tables
WHERE connector_id=? AND table_name='events'`, connectorID).Scan(&state.TableKey, &state.TableRows, &columns)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return model.FoundryEventSourceState{}, fmt.Errorf("read event source %s table: %w", connectorID, err)
	default:
		var named []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(columns), &named); err != nil {
			return model.FoundryEventSourceState{}, fmt.Errorf("read event source %s columns: %w", connectorID, err)
		}
		for _, col := range named {
			state.TableFields = append(state.TableFields, col.Name)
		}
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(events), 0), COALESCE(SUM(bytes), 0) FROM foundry_event_batches
WHERE connector_id=? AND seq > ?`, connectorID, cursor).Scan(&state.PendingPieces, &state.PendingBytes); err != nil {
		return model.FoundryEventSourceState{}, fmt.Errorf("sum event source %s batches: %w", connectorID, err)
	}
	return state, nil
}

// eventSourceProgress reads an event source's learned schema from its config
// ({"schema": {...}}, empty before its first send) and its change cursor's
// seq ({"seq": n}, 0 before its first sync).
func eventSourceProgress(connectorID string, config, cursor json.RawMessage) (events.Schema, int64, error) {
	var settings struct {
		Schema events.Schema `json:"schema"`
	}
	if err := json.Unmarshal(config, &settings); err != nil {
		return events.Schema{}, 0, fmt.Errorf("read event source %s config: %w", connectorID, err)
	}
	var at struct {
		Seq int64 `json:"seq"`
	}
	if len(cursor) > 0 {
		if err := json.Unmarshal(cursor, &at); err != nil {
			return events.Schema{}, 0, fmt.Errorf("read event source %s cursor: %w", connectorID, err)
		}
	}
	return settings.Schema, at.Seq, nil
}

// EventSendState reads a live event source of the workspace in one
// consistent read, for a send's checks before it writes its object and takes
// the lock. ErrNotFound is no live connector with this id in the workspace,
// ErrNotEventSource one of another kind.
func (s *mysqlDB) EventSendState(ctx context.Context, workspaceID, connectorID string) (model.FoundryEventSourceState, error) {
	var state model.FoundryEventSourceState
	err := s.WithTx(ctx, func(txDB Store) error {
		var err error
		state, err = txDB.(*mysqlDB).eventSourceState(ctx, workspaceID, connectorID, false)
		return err
	})
	return state, err
}

// EventBatchByToken returns the source's batch that carries clientToken,
// through its unique key; ErrNotFound when none does.
func (s *mysqlDB) EventBatchByToken(ctx context.Context, workspaceID, connectorID, clientToken string) (*model.FoundryEventBatch, error) {
	return scanEventBatch(s.db.QueryRowContext(ctx, "SELECT "+eventBatchColumns+`
FROM foundry_event_batches WHERE connector_id=? AND client_token=? AND workspace_id=?`, connectorID, clientToken, workspaceID))
}

// SendFoundryEvents commits a send whose object is stored, in one
// transaction under the source's row lock:
//
//  1. a live event source of the workspace, else ErrNotFound or
//     ErrNotEventSource;
//  2. no batch of the source carries the send's client token: one that does
//     won a concurrent retry, and the send is ErrConflict (its object is the
//     caller's to delete, and EventBatchByToken returns the winner);
//  3. the source is not paused, else *EventSourcePausedError;
//  4. the send's fields merge into the locked schema, else the
//     *events.Refusal of Schema.Merge;
//  5. the table stays within the caps with this send, counted from the
//     locked values, else the *events.Refusal of Holding.Project;
//  6. the batch takes seq one above the source's newest, and its row is
//     inserted with created_at its received_at;
//  7. the schema is written when it grew, and next_sync_at, when no sync is
//     scheduled, is set the spacing from now. Nothing else of the source moves.
//
// Any error leaves nothing written. A send without a client token always
// stores a new batch.
func (s *mysqlDB) SendFoundryEvents(ctx context.Context, send model.FoundryEventSend) (model.FoundryEventsSent, error) {
	if send.TableBytes == nil {
		return model.FoundryEventsSent{}, fmt.Errorf("send foundry events: no TableBytes")
	}
	var sent model.FoundryEventsSent
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		state, err := tx.eventSourceState(ctx, send.WorkspaceID, send.ConnectorID, true)
		if err != nil {
			return err
		}
		if send.ClientToken != nil {
			switch _, err := tx.EventBatchByToken(ctx, send.WorkspaceID, send.ConnectorID, *send.ClientToken); {
			case err == nil:
				return ErrConflict
			case !errors.Is(err, ErrNotFound):
				return fmt.Errorf("read event batch by client token: %w", err)
			}
		}
		if state.Connector.SchedulePaused != nil {
			return &EventSourcePausedError{Reason: *state.Connector.SchedulePaused}
		}
		merged, err := state.Schema.Merge(send.Send, send.MaxFields)
		if err != nil {
			return err
		}
		var tableBytes int64
		if state.TableKey != "" {
			if tableBytes, err = send.TableBytes(ctx, state.TableKey); err != nil {
				return fmt.Errorf("read event source %s table size: %w", send.ConnectorID, err)
			}
		}
		capacity, err := state.Holding(tableBytes).Project(send.Send, merged.Schema, send.Caps)
		if err != nil {
			return err
		}
		batch := model.FoundryEventBatch{
			ID: send.ID, WorkspaceID: send.WorkspaceID, ConnectorID: send.ConnectorID, ClientToken: send.ClientToken, S3Key: send.Key,
			Pieces: send.Send.Pieces(), Bytes: send.Send.Bytes(),
			FirstAt: send.Object.FirstAt.UTC(), LastAt: send.Object.LastAt.UTC(), CreatedAt: send.Object.ReceivedAt.UTC(),
		}
		if err := tx.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM foundry_event_batches WHERE connector_id=?`,
			send.ConnectorID).Scan(&batch.Seq); err != nil {
			return fmt.Errorf("number event batch: %w", err)
		}
		if _, err := tx.db.ExecContext(ctx, `
INSERT INTO foundry_event_batches (id, workspace_id, connector_id, seq, client_token, s3_key, events, bytes, first_at, last_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, batch.ID, batch.WorkspaceID, batch.ConnectorID, batch.Seq, batch.ClientToken, batch.S3Key,
			batch.Pieces, batch.Bytes, batch.FirstAt, batch.LastAt, batch.CreatedAt); err != nil {
			return fmt.Errorf("insert event batch: %w", err)
		}
		var assignments []string
		var args []any
		if merged.Changed() {
			schema, err := json.Marshal(merged.Schema)
			if err != nil {
				return fmt.Errorf("encode event source schema: %w", err)
			}
			assignments = append(assignments, "config=JSON_SET(config, '$.schema', CAST(? AS JSON))")
			args = append(args, string(schema))
		}
		if state.Connector.NextSyncAt == nil {
			assignments = append(assignments, "next_sync_at=NOW(6) + INTERVAL ? MICROSECOND")
			args = append(args, send.Spacing.For(state.TableRows).Microseconds())
		}
		if len(assignments) > 0 {
			if _, err := tx.db.ExecContext(ctx, `UPDATE connectors SET `+joinComma(assignments)+` WHERE id=?`,
				append(args, send.ConnectorID)...); err != nil {
				return fmt.Errorf("update event source %s: %w", send.ConnectorID, err)
			}
		}
		sent = model.FoundryEventsSent{Batch: batch, New: merged.New, Capacity: capacity}
		return nil
	})
	return sent, err
}

// ListFoundryEventBatches returns a live event source's newest batches, at
// most limit, newest first, with its state from the same consistent read:
// what a list shows of pending pieces and how full the source is.
// ErrNotFound is no live connector with this id in the workspace,
// ErrNotEventSource one of another kind.
func (s *mysqlDB) ListFoundryEventBatches(ctx context.Context, workspaceID, connectorID string, limit int) (model.FoundryEventBatchList, error) {
	var list model.FoundryEventBatchList
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var err error
		if list.State, err = tx.eventSourceState(ctx, workspaceID, connectorID, false); err != nil {
			return err
		}
		list.Batches, err = tx.queryEventBatches(ctx, `WHERE connector_id=? ORDER BY seq DESC LIMIT ?`, connectorID, limit)
		return err
	})
	return list, err
}

// SumFoundryEventBatches sums a workspace's batches created since since, as
// its daily caps count them, through foundry_event_batches_day_idx.
func (s *mysqlDB) SumFoundryEventBatches(ctx context.Context, workspaceID string, since time.Time) (model.FoundryEventTotals, error) {
	var totals model.FoundryEventTotals
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(events), 0), COALESCE(SUM(bytes), 0) FROM foundry_event_batches
WHERE workspace_id=? AND created_at >= ?`, workspaceID, since.UTC()).Scan(&totals.Pieces, &totals.Bytes)
	if err != nil {
		return model.FoundryEventTotals{}, fmt.Errorf("sum workspace %s event batches: %w", workspaceID, err)
	}
	return totals, nil
}

// EventBatchesAfter returns event source connectorID's schema and its
// batches with seq above after, oldest first, as many as one read takes
// within budget (always the first, when any waits), from one consistent
// read: every batch it returns holds only fields of the schema it returns,
// since a send writes both in its transaction. ErrNotFound is no event source
// with this id, deleted or not.
func (s *mysqlDB) EventBatchesAfter(ctx context.Context, connectorID string, after int64, budget events.ReadBudget) (events.Schema, []events.Batch, error) {
	schema, waiting, err := s.eventBatchesAbove(ctx, connectorID, after, budget.Batches)
	if err != nil {
		return events.Schema{}, nil, err
	}
	return schema, budget.Cut(schema, waiting), nil
}

// EventBacklog says, from one consistent read, what event source connectorID
// holds above after: nothing, what one read within budget takes, or more.
func (s *mysqlDB) EventBacklog(ctx context.Context, connectorID string, after int64, budget events.ReadBudget) (events.Backlog, error) {
	// One batch past what a read may take tells a full read from a longer backlog.
	schema, waiting, err := s.eventBatchesAbove(ctx, connectorID, after, budget.Batches+1)
	if err != nil {
		return events.BacklogNone, err
	}
	return budget.Backlog(schema, waiting), nil
}

// eventBatchesAbove reads, in one transaction, event source connectorID's
// schema and up to limit of its batches with seq above after, oldest first,
// through foundry_event_batches_seq_uq.
func (s *mysqlDB) eventBatchesAbove(ctx context.Context, connectorID string, after int64, limit int) (events.Schema, []events.Batch, error) {
	var schema events.Schema
	var waiting []events.Batch
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var err error
		if schema, _, err = tx.eventSourceSchema(ctx, connectorID); err != nil {
			return err
		}
		rows, err := tx.db.QueryContext(ctx, `SELECT id, seq, s3_key, events, bytes FROM foundry_event_batches
WHERE connector_id=? AND seq > ? ORDER BY seq LIMIT ?`, connectorID, after, limit)
		if err != nil {
			return fmt.Errorf("read event source %s batches: %w", connectorID, err)
		}
		defer rows.Close()
		for rows.Next() {
			var b events.Batch
			if err := rows.Scan(&b.ID, &b.Seq, &b.Key, &b.Pieces, &b.Bytes); err != nil {
				return fmt.Errorf("scan event batch: %w", err)
			}
			waiting = append(waiting, b)
		}
		return rows.Err()
	})
	if err != nil {
		return events.Schema{}, nil, err
	}
	return schema, waiting, nil
}

// eventSourceSchema reads event source connectorID's schema and cursor,
// deleted or not. ErrNotFound is no event source with this id.
func (s *mysqlDB) eventSourceSchema(ctx context.Context, connectorID string) (events.Schema, int64, error) {
	var config string
	var cursor sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT config, change_cursor FROM connectors WHERE id=? AND kind='events'`,
		connectorID).Scan(&config, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return events.Schema{}, 0, ErrNotFound
	}
	if err != nil {
		return events.Schema{}, 0, fmt.Errorf("read event source %s: %w", connectorID, err)
	}
	var at json.RawMessage
	if cursor.Valid {
		at = json.RawMessage(cursor.String)
	}
	return eventSourceProgress(connectorID, json.RawMessage(config), at)
}

// EventTable returns event source connectorID's schema, its stored cursor's
// seq (0 for none) and the key of its events table's snapshot ("" for none),
// from one consistent read. ErrNotFound is no event source with this id,
// deleted or not.
func (s *mysqlDB) EventTable(ctx context.Context, connectorID string) (events.Schema, int64, string, error) {
	var schema events.Schema
	var cursor int64
	var key string
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var err error
		if schema, cursor, err = tx.eventSourceSchema(ctx, connectorID); err != nil {
			return err
		}
		err = tx.db.QueryRowContext(ctx, `SELECT s3_key FROM connector_tables WHERE connector_id=? AND table_name='events'`,
			connectorID).Scan(&key)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return events.Schema{}, 0, "", err
	}
	return schema, cursor, key, nil
}

// CreateFoundryEventSourceToken stores t, a new token of a live event source
// of its workspace, under the source's row lock, so concurrent creates never
// pass maxLive live tokens: ErrLimitExceeded. It sets t.CreatedAt.
// ErrNotFound is no live connector with that id in the workspace,
// ErrNotEventSource one of another kind, and ErrConflict a hash already
// stored.
func (s *mysqlDB) CreateFoundryEventSourceToken(ctx context.Context, t *model.FoundryEventSourceToken, maxLive int) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var kind string
		err := tx.db.QueryRowContext(ctx, `SELECT kind FROM connectors WHERE id=? AND workspace_id=? AND deleted_at IS NULL FOR UPDATE`,
			t.ConnectorID, t.WorkspaceID).Scan(&kind)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("lock event source %s: %w", t.ConnectorID, err)
		case kind != model.ConnectorKindEvents:
			return ErrNotEventSource
		}
		var live int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_event_source_tokens
WHERE connector_id=? AND revoked_at IS NULL`, t.ConnectorID).Scan(&live); err != nil {
			return fmt.Errorf("count event source tokens: %w", err)
		}
		if live >= maxLive {
			return ErrLimitExceeded
		}
		_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_event_source_tokens (id, workspace_id, connector_id, hash, prefix, created_by)
VALUES (?, ?, ?, ?, ?, ?)`, t.ID, t.WorkspaceID, t.ConnectorID, t.Hash, t.Prefix, t.CreatedBy)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("insert event source token: %w", err)
		}
		return tx.db.QueryRowContext(ctx, `SELECT created_at FROM foundry_event_source_tokens WHERE id=?`, t.ID).Scan(&t.CreatedAt)
	})
}

// ListFoundryEventSourceTokens returns a source's tokens, revoked ones
// included, newest first, at most limit.
func (s *mysqlDB) ListFoundryEventSourceTokens(ctx context.Context, workspaceID, connectorID string, limit int) ([]*model.FoundryEventSourceToken, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+eventSourceTokenColumns+` FROM foundry_event_source_tokens t
WHERE t.connector_id=? AND t.workspace_id=? ORDER BY t.created_at DESC, t.id DESC LIMIT ?`, connectorID, workspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list event source tokens: %w", err)
	}
	defer rows.Close()
	var out []*model.FoundryEventSourceToken
	for rows.Next() {
		t, err := scanEventSourceToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeFoundryEventSourceToken revokes the token with all three ids and
// returns it. Revoking a revoked token keeps its first revoked_at and is no
// error; ErrNotFound is no token matching all three.
func (s *mysqlDB) RevokeFoundryEventSourceToken(ctx context.Context, workspaceID, connectorID, id string) (*model.FoundryEventSourceToken, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_event_source_tokens SET revoked_at=NOW(6)
WHERE id=? AND connector_id=? AND workspace_id=? AND revoked_at IS NULL`, id, connectorID, workspaceID); err != nil {
		return nil, fmt.Errorf("revoke event source token %s: %w", id, err)
	}
	return scanEventSourceToken(s.db.QueryRowContext(ctx, "SELECT "+eventSourceTokenColumns+` FROM foundry_event_source_tokens t
WHERE t.id=? AND t.connector_id=? AND t.workspace_id=?`, id, connectorID, workspaceID))
}

// ValidateFoundryEventSourceToken returns the live token with this hash,
// through its unique key, and records its use: last_used_at moves at most
// once a minute, so a busy sender writes the row rarely. ErrNotFound is no
// such token, a revoked one, or one whose source or workspace is gone.
func (s *mysqlDB) ValidateFoundryEventSourceToken(ctx context.Context, hash string) (*model.FoundryEventSourceToken, error) {
	t, err := scanEventSourceToken(s.db.QueryRowContext(ctx, "SELECT "+eventSourceTokenColumns+s.liveEventSourceTokens()+"t.hash=?", hash))
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_event_source_tokens SET last_used_at=NOW(6)
WHERE id=? AND (last_used_at IS NULL OR last_used_at < NOW(6) - INTERVAL 1 MINUTE)`, t.ID); err != nil {
		return nil, fmt.Errorf("record event source token %s use: %w", t.ID, err)
	}
	return t, nil
}

// GetLiveFoundryEventSourceToken returns the token with all three ids while
// it authenticates, as ValidateFoundryEventSourceToken's: a send made under
// it re-reads it. ErrNotFound is any other token.
func (s *mysqlDB) GetLiveFoundryEventSourceToken(ctx context.Context, workspaceID, connectorID, id string) (*model.FoundryEventSourceToken, error) {
	return scanEventSourceToken(s.db.QueryRowContext(ctx, "SELECT "+eventSourceTokenColumns+s.liveEventSourceTokens()+
		"t.id=? AND t.workspace_id=? AND t.connector_id=?", id, workspaceID, connectorID))
}

// ListFoundryEventBatchPurges returns up to limit batches of the event
// sources a purge empties, those deleted before deletedBefore. The purge
// deletes each batch's object, then DeleteFoundryEventBatch its row.
func (s *mysqlDB) ListFoundryEventBatchPurges(ctx context.Context, deletedBefore time.Time, limit int) ([]*model.FoundryEventBatch, error) {
	return s.queryEventBatches(ctx, `WHERE connector_id IN (`+purgeableEventSources+`)
ORDER BY connector_id, seq LIMIT ?`, deletedBefore.UTC(), limit)
}

// DeleteFoundryEventBatch deletes a batch's row once its object is gone. It
// deletes only a batch of a source the purge empties, deletedBefore as
// ListFoundryEventBatchPurges', so no batch of a live source is ever lost; a
// batch already deleted is no error.
func (s *mysqlDB) DeleteFoundryEventBatch(ctx context.Context, id string, deletedBefore time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_event_batches
WHERE id=? AND connector_id IN (`+purgeableEventSources+`)`, id, deletedBefore.UTC()); err != nil {
		return fmt.Errorf("delete event batch %s: %w", id, err)
	}
	return nil
}

// ListFoundryEventSourcePurges returns up to limit of the event sources a
// purge empties, deletedBefore as ListFoundryEventBatchPurges', in the order
// they were deleted, (DeletedAt, ConnectorID), from just after after; the
// zero after starts from the first. Each says whether batches of it are
// left. The purge goes through them all with after as its cursor, so it
// reaches every deleted source, even one that never held a batch or a token
// but whose folder holds orphans: it deletes the folder of each with no
// batch left, then FinishFoundryEventSourcePurge. A source is deleted at most
// once and its deleted_at never moves, and one deleted later sorts after it,
// so a cursor never skips a source.
func (s *mysqlDB) ListFoundryEventSourcePurges(ctx context.Context, deletedBefore time.Time, after model.FoundryEventSourcePurge, limit int) (
	[]model.FoundryEventSourcePurge, error) {
	q := `SELECT c.workspace_id, c.id, c.deleted_at, EXISTS (SELECT 1 FROM foundry_event_batches b WHERE b.connector_id = c.id)
FROM connectors c WHERE c.kind = 'events' AND c.deleted_at < ?`
	args := []any{deletedBefore.UTC()}
	if after.ConnectorID != "" {
		q += ` AND (c.deleted_at > ? OR (c.deleted_at = ? AND c.id > ?))`
		args = append(args, after.DeletedAt.UTC(), after.DeletedAt.UTC(), after.ConnectorID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY c.deleted_at, c.id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list event source purges: %w", err)
	}
	defer rows.Close()
	var out []model.FoundryEventSourcePurge
	for rows.Next() {
		var p model.FoundryEventSourcePurge
		if err := rows.Scan(&p.WorkspaceID, &p.ConnectorID, &p.DeletedAt, &p.Batches); err != nil {
			return nil, fmt.Errorf("scan event source purge: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FinishFoundryEventSourcePurge ends the purge of a source once its batches
// and its folder are gone: it deletes its tokens, revoked since its delete.
// It deletes only a source the purge empties, deletedBefore as
// ListFoundryEventBatchPurges', and finishing one again is no error.
func (s *mysqlDB) FinishFoundryEventSourcePurge(ctx context.Context, p model.FoundryEventSourcePurge, deletedBefore time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_event_source_tokens
WHERE connector_id=? AND workspace_id=? AND connector_id IN (`+purgeableEventSources+`)`, p.ConnectorID, p.WorkspaceID,
		deletedBefore.UTC()); err != nil {
		return fmt.Errorf("delete event source %s tokens: %w", p.ConnectorID, err)
	}
	return nil
}

// DeleteExpiredEventSyncs deletes up to limit syncs of event sources (origin
// events) that succeeded before finishedBefore, and returns how many it
// deleted: a busy source writes up to a sync a minute, and nothing else
// prunes them. A source's newest sync always stays, since it is the
// connector's last sync, so a source idle past the retention still shows how
// it last synced. Failed syncs stay, as does every sync of another origin.
//
// It scans connectors for event sources, then reads each one's syncs through
// connector_syncs_history_idx (connector_id, created_at), from the oldest up
// to finishedBefore, so a tick costs one short index range per event source
// however many syncs other kinds keep; the index is forced because a
// status='succeeded' range of connector_syncs_claim_idx would read them all.
// A sync is created before it finishes, so created_at bounds the range.
func (s *mysqlDB) DeleteExpiredEventSyncs(ctx context.Context, finishedBefore time.Time, limit int) (int, error) {
	before := finishedBefore.UTC()
	rows, err := s.db.QueryContext(ctx, `SELECT s.id FROM connectors c
JOIN connector_syncs s FORCE INDEX (connector_syncs_history_idx) ON s.connector_id = c.id AND s.created_at < ?
WHERE c.kind = 'events' AND s.origin = 'events' AND s.status = 'succeeded' AND s.finished_at < ?
  AND EXISTS (SELECT 1 FROM connector_syncs n WHERE n.connector_id = s.connector_id
    AND (n.created_at > s.created_at OR (n.created_at = s.created_at AND n.id > s.id)))
LIMIT ?`, before, before, limit)
	if err != nil {
		return 0, fmt.Errorf("list expired event source syncs: %w", err)
	}
	defer rows.Close()
	var ids []any
	for rows.Next() {
		var syncID string
		if err := rows.Scan(&syncID); err != nil {
			return 0, fmt.Errorf("scan expired event source sync: %w", err)
		}
		ids = append(ids, syncID)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("list expired event source syncs: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	// The delete repeats the sync's own conditions, so it deletes nothing but
	// an expired event source sync. The newest sync of a source is never
	// listed, by this tick or a concurrent one, so it always stays.
	res, err := s.db.ExecContext(ctx, `DELETE FROM connector_syncs
WHERE origin = 'events' AND status = 'succeeded' AND finished_at < ? AND id IN (`+idPlaceholders(len(ids))+`)`,
		append([]any{before}, ids...)...)
	if err != nil {
		return 0, fmt.Errorf("delete expired event source syncs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted event source syncs: %w", err)
	}
	return int(n), nil
}

func (s *mysqlDB) queryEventBatches(ctx context.Context, where string, args ...any) ([]*model.FoundryEventBatch, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+eventBatchColumns+" FROM foundry_event_batches "+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list event batches: %w", err)
	}
	defer rows.Close()
	var out []*model.FoundryEventBatch
	for rows.Next() {
		b, err := scanEventBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func scanEventBatch(scanner rowScanner) (*model.FoundryEventBatch, error) {
	var b model.FoundryEventBatch
	var token sql.NullString
	if err := scanner.Scan(&b.ID, &b.WorkspaceID, &b.ConnectorID, &b.Seq, &token, &b.S3Key, &b.Pieces, &b.Bytes,
		&b.FirstAt, &b.LastAt, &b.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan event batch: %w", err)
	}
	if token.Valid {
		b.ClientToken = &token.String
	}
	return &b, nil
}

func scanEventSourceToken(scanner rowScanner) (*model.FoundryEventSourceToken, error) {
	var t model.FoundryEventSourceToken
	var revoked, used sql.NullTime
	if err := scanner.Scan(&t.ID, &t.WorkspaceID, &t.ConnectorID, &t.Hash, &t.Prefix, &t.CreatedBy, &t.CreatedAt,
		&revoked, &used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan event source token: %w", err)
	}
	if revoked.Valid {
		t.RevokedAt = &revoked.Time
	}
	if used.Valid {
		t.LastUsedAt = &used.Time
	}
	return &t, nil
}
