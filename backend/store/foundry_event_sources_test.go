package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/events"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store"
)

var (
	eventLimits  = events.Limits{MaxPieces: 500, MaxBytes: 1 << 20, MaxPieceBytes: 256 << 10}
	eventCaps    = events.Caps{MaxRows: 2_000_000, MaxBytes: 2 << 30}
	eventSpacing = model.EventSyncSpacing{After: time.Minute, PerMillionRows: 10 * time.Minute}
	// eventBudget takes every batch a test writes in one read.
	eventBudget = events.ReadBudget{Batches: 1000, Pieces: 50_000, Bytes: 128 << 20}
)

// eventWorkspace stores workspace workspaceID unless it exists.
func eventWorkspace(t *testing.T, db store.Store, workspaceID string) {
	t.Helper()
	createWorkspace(t, workspaceID)
}

// eventSource stores event source ES-<tag>, named "Source <tag>", in
// workspace workspaceID, which it creates if need be.
func eventSource(t *testing.T, db store.Store, workspaceID, tag string) *model.Connector {
	t.Helper()
	eventWorkspace(t, db, workspaceID)
	c := newEventSource(workspaceID, tag)
	require.NoError(t, db.CreateEventSource(context.Background(), c, 20))
	return c
}

func newEventSource(workspaceID, tag string) *model.Connector {
	return &model.Connector{
		ID: id26("ES-" + tag), WorkspaceID: workspaceID, Kind: model.ConnectorKindEvents, Name: "Source " + tag,
		Config: json.RawMessage(`{"description":"Orders from the shop"}`), SecretCiphertext: []byte("sealed"), SecretKeyID: "k1",
	}
}

// eventSend is a send to c of one piece per data, with ids <prefix>-0,
// <prefix>-1 and so on, received now, to a source whose snapshot, if it has
// one, holds 1 MiB.
func eventSend(t *testing.T, c *model.Connector, prefix string, data ...string) model.FoundryEventSend {
	t.Helper()
	pieces := make([]events.Piece, len(data))
	for i, d := range data {
		pieces[i] = events.Piece{ID: fmt.Sprintf("%s-%d", prefix, i), Data: []byte(d)}
	}
	send, err := events.Validate(pieces, eventLimits)
	require.NoError(t, err)
	obj, err := events.EncodeLines(send, time.Now())
	require.NoError(t, err)
	batchID := id.New()
	return model.FoundryEventSend{
		ID: batchID, WorkspaceID: c.WorkspaceID, ConnectorID: c.ID, Key: "ws/" + c.WorkspaceID + "/events/" + c.ID + "/" + batchID + ".jsonl.gz",
		Send: send, Object: obj, MaxFields: 200, Caps: eventCaps, Spacing: eventSpacing,
		TableBytes: func(context.Context, string) (int64, error) { return 1 << 20, nil },
	}
}

// sent commits send, which must be stored.
func sent(t *testing.T, db store.Store, send model.FoundryEventSend) model.FoundryEventsSent {
	t.Helper()
	out, err := db.SendFoundryEvents(context.Background(), send)
	require.NoError(t, err)
	return out
}

// eventTable writes c's events table as a sync publishes it: rows rows in
// snapshot key, with columns id, received_at, occurred_at and fields.
func eventTable(t *testing.T, c *model.Connector, key string, rows int64, fields ...string) {
	t.Helper()
	columns := []map[string]string{{"name": "id"}, {"name": "received_at"}, {"name": "occurred_at"}}
	for _, f := range fields {
		columns = append(columns, map[string]string{"name": f})
	}
	encoded, err := json.Marshal(columns)
	require.NoError(t, err)
	_, err = testDB.Exec(`INSERT INTO connector_tables (id, connector_id, workspace_id, table_name, columns, primary_key, foreign_keys,
  s3_key, row_count, sync_id, synced_at)
VALUES (?, ?, ?, 'events', CAST(? AS JSON), '["id"]', '[]', ?, ?, ?, NOW(6))
ON DUPLICATE KEY UPDATE columns=VALUES(columns), s3_key=VALUES(s3_key), row_count=VALUES(row_count)`,
		id.New(), c.ID, c.WorkspaceID, string(encoded), key, rows, id26("SYNC"))
	require.NoError(t, err)
}

// eventCursor stores c's change cursor at seq, as its sync does.
func eventCursor(t *testing.T, c *model.Connector, seq int64) {
	t.Helper()
	_, err := testDB.Exec(`UPDATE connectors SET change_cursor=CAST(? AS JSON) WHERE id=?`, fmt.Sprintf(`{"seq":%d}`, seq), c.ID)
	require.NoError(t, err)
}

// fieldNames are a schema's field names.
func fieldNames(s events.Schema) []string {
	var out []string
	for _, f := range s.Fields() {
		out = append(out, f.Name)
	}
	return out
}

// batchSeqs are the seqs of batches.
func batchSeqs(batches []events.Batch) []int64 {
	out := make([]int64, len(batches))
	for i, b := range batches {
		out[i] = b.Seq
	}
	return out
}

// storedEventSource is c as stored, deleted or not.
func storedEventSource(t *testing.T, c *model.Connector) (config map[string]any, next *time.Time, cursor sql.NullString, secret []byte) {
	t.Helper()
	var raw string
	var at sql.NullTime
	require.NoError(t, testDB.QueryRow(`SELECT config, next_sync_at, change_cursor, secret_ciphertext FROM connectors WHERE id=?`, c.ID).
		Scan(&raw, &at, &cursor, &secret))
	require.NoError(t, json.Unmarshal([]byte(raw), &config))
	if at.Valid {
		next = &at.Time
	}
	return config, next, cursor, secret
}

func eventBatchCount(t *testing.T, c *model.Connector) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_event_batches WHERE connector_id=?`, c.ID).Scan(&n))
	return n
}

func TestSendFoundryEvents(t *testing.T) {
	workspaceID := id26("ES-SEND-WS")

	// Seq order is commit order: a sync that has read seq k+1 must already
	// have read k, or it would skip k's pieces for good.
	t.Run("numbers 32 concurrent sends 1 to 32, never visible out of order", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "seq")

		sends := make([]model.FoundryEventSend, 32)
		for i := range sends {
			sends[i] = eventSend(t, c, fmt.Sprint("p", i), `{"n":1}`)
		}
		done := make(chan struct{})
		var reader sync.WaitGroup
		reader.Add(1)
		var seen []int64
		go func() {
			defer reader.Done()
			after := int64(0)
			for {
				// Once every send has committed, one more read takes the rest.
				var last bool
				select {
				case <-done:
					last = true
				default:
				}
				_, batches, err := db.EventBatchesAfter(ctx, c.ID, after, eventBudget)
				if !assert.NoError(t, err) {
					return
				}
				for _, b := range batches {
					if !assert.Equal(t, after+1, b.Seq, "a reader polling above %d saw seq %d first", after, b.Seq) {
						return
					}
					after = b.Seq
					seen = append(seen, b.Seq)
				}
				if last {
					return
				}
			}
		}()
		var senders sync.WaitGroup
		for _, send := range sends {
			senders.Add(1)
			go func() {
				defer senders.Done()
				_, err := db.SendFoundryEvents(ctx, send)
				assert.NoError(t, err)
			}()
		}
		senders.Wait()
		close(done)
		reader.Wait()

		want := make([]int64, 32)
		for i := range want {
			want[i] = int64(i + 1)
		}
		assert.Equal(t, want, seen)
		_, batches, err := db.EventBatchesAfter(ctx, c.ID, 0, eventBudget)
		require.NoError(t, err)
		assert.Equal(t, want, batchSeqs(batches), "seqs 1 to 32 with no gap")
	})

	// A retry after a timeout must learn what happened to its send, not
	// store the pieces twice, even when it races the send it retries.
	t.Run("stores one batch for a client token, whichever retry wins the lock", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "token")
		token := "0f3c2a4e-5b6d-4e7f-8a9b-0c1d2e3f4a5b"

		results := make([]error, 8)
		retries := make([]model.FoundryEventSend, len(results))
		for i := range retries {
			retries[i] = eventSend(t, c, "o", `{"amount":1}`)
			retries[i].ClientToken = &token
		}
		var wg sync.WaitGroup
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, results[i] = db.SendFoundryEvents(ctx, retries[i])
			}()
		}
		wg.Wait()
		stored := 0
		for _, err := range results {
			if err == nil {
				stored++
				continue
			}
			assert.ErrorIs(t, err, store.ErrConflict)
		}
		assert.Equal(t, 1, stored, "one send stores the batch, and the others find it")
		assert.Equal(t, 1, eventBatchCount(t, c))

		winner, err := db.EventBatchByToken(ctx, workspaceID, c.ID, token)
		require.NoError(t, err)
		assert.Equal(t, int64(1), winner.Seq)
		require.NotNil(t, winner.ClientToken)
		assert.Equal(t, token, *winner.ClientToken)

		// A later retry finds it too, and the same token on another source is another send.
		send := eventSend(t, c, "o", `{"amount":1}`)
		send.ClientToken = &token
		_, err = db.SendFoundryEvents(ctx, send)
		assert.ErrorIs(t, err, store.ErrConflict)
		other := eventSource(t, db, workspaceID, "token-other")
		send = eventSend(t, other, "o", `{"amount":1}`)
		send.ClientToken = &token
		sent(t, db, send)
		_, err = db.EventBatchByToken(ctx, workspaceID, c.ID, "another-token-of-sixteen")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// A sync reads the schema with the batches it merges; a field must
	// never reach a batch without reaching the schema.
	t.Run("commits the learned schema with its batch", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "schema")

		first := eventSend(t, c, "o", `{"amount":12.5,"note":null}`, `{"customer":"c_1"}`)
		out := sent(t, db, first)
		assert.Equal(t, []events.Field{{Name: "amount", Type: events.TypeNumber}, {Name: "customer", Type: events.TypeString}, {Name: "note", Type: events.TypeNull}}, out.New)
		assert.Equal(t, model.FoundryEventBatch{
			ID: first.ID, WorkspaceID: workspaceID, ConnectorID: c.ID, Seq: 1, S3Key: first.Key, Pieces: 2, Bytes: first.Send.Bytes(),
			FirstAt: first.Object.FirstAt, LastAt: first.Object.LastAt, CreatedAt: first.Object.ReceivedAt,
		}, out.Batch)
		stored, err := db.ListFoundryEventBatches(ctx, workspaceID, c.ID, 10)
		require.NoError(t, err)
		require.Len(t, stored.Batches, 1)
		assert.Equal(t, out.Batch.CreatedAt, stored.Batches[0].CreatedAt.UTC(), "created_at is the received_at written on every line")

		// A field seen only as null takes the type of its first value, and
		// changes the schema without adding a field.
		out = sent(t, db, eventSend(t, c, "p", `{"note":"late"}`))
		assert.Empty(t, out.New)
		schema, batches, err := db.EventBatchesAfter(ctx, c.ID, 0, eventBudget)
		require.NoError(t, err)
		assert.Equal(t, []int64{1, 2}, batchSeqs(batches))
		assert.Equal(t, []events.Field{{Name: "amount", Type: events.TypeNumber}, {Name: "customer", Type: events.TypeString}, {Name: "note", Type: events.TypeString}}, schema.Fields())

		config, _, _, _ := storedEventSource(t, c)
		assert.Equal(t, "Orders from the shop", config["description"], "a send writes only the schema of the config")
	})

	// A refused send must leave the source exactly as it was: no row, no
	// schema change and no sync scheduled for pieces that never arrived.
	// Each case breaks one check made under the lock.
	for _, c := range []struct {
		name  string
		alter func(*testing.T, *model.FoundryEventSend)
		want  func(t *testing.T, err error)
	}{
		{"a field of another type", func(t *testing.T, s *model.FoundryEventSend) {
			s.Send = mustValidate(t, `{"amount":"12.5"}`)
		}, refusedAs(events.Invalid)},
		{"a field past max_fields", func(t *testing.T, s *model.FoundryEventSend) {
			s.MaxFields = 1
			s.Send = mustValidate(t, `{"extra":1}`)
		}, refusedAs(events.TooManyFields)},
		{"rows past the cap", func(t *testing.T, s *model.FoundryEventSend) {
			s.Caps.MaxRows = 3
			s.Send = mustValidate(t, `{"amount":1}`, `{"amount":2}`)
		}, refusedAs(events.Full)},
		{"a snapshot that fills the byte cap", func(_ *testing.T, s *model.FoundryEventSend) {
			s.TableBytes = func(context.Context, string) (int64, error) { return eventCaps.MaxBytes, nil }
		}, refusedAs(events.Full)},
		{"a snapshot whose size cannot be read", func(_ *testing.T, s *model.FoundryEventSend) {
			s.TableBytes = func(context.Context, string) (int64, error) { return 0, errors.New("s3: slow down") }
		}, func(t *testing.T, err error) { assert.ErrorContains(t, err, "slow down") }},
	} {
		t.Run("refuses "+c.name+" and stores nothing", func(t *testing.T) {
			ctx := context.Background()
			db := newTestStore(t)
			src := eventSource(t, db, workspaceID, "refused")
			sent(t, db, eventSend(t, src, "o", `{"amount":1}`))
			eventTable(t, src, "ws/snapshot-1", 2, "amount")
			eventCursor(t, src, 1)
			_, err := testDB.Exec(`UPDATE connectors SET next_sync_at=NULL WHERE id=?`, src.ID)
			require.NoError(t, err)
			before, _, _, _ := storedEventSource(t, src)

			send := eventSend(t, src, "r", `{"amount":2}`)
			c.alter(t, &send)
			_, err = db.SendFoundryEvents(ctx, send)
			c.want(t, err)
			assert.Equal(t, 1, eventBatchCount(t, src), "no row of the refused send")
			after, next, _, _ := storedEventSource(t, src)
			assert.Equal(t, before, after, "the schema is as it was")
			assert.Nil(t, next, "no sync is scheduled")
		})
	}

	// A paused source cannot sync, so pieces sent to it would wait unseen;
	// the refusal says why it paused.
	t.Run("refuses a paused source with its reason", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "paused")
		_, err := testDB.Exec(`UPDATE connectors SET schedule_paused='batch 3 of this event source is missing from storage' WHERE id=?`, c.ID)
		require.NoError(t, err)
		_, err = db.SendFoundryEvents(ctx, eventSend(t, c, "o", `{"amount":1}`))
		var paused *store.EventSourcePausedError
		require.ErrorAs(t, err, &paused)
		assert.Equal(t, "batch 3 of this event source is missing from storage", paused.Reason)
		assert.Equal(t, 0, eventBatchCount(t, c))
	})

	// A send names its source by id within the workspace; any other
	// connector is no event source of the sender's.
	t.Run("refuses a deleted source, another workspace's and another kind", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "gone")
		require.NoError(t, db.DeleteConnector(ctx, workspaceID, c.ID))
		_, err := db.SendFoundryEvents(ctx, eventSend(t, c, "o", `{}`))
		assert.ErrorIs(t, err, store.ErrNotFound)

		elsewhere := eventSource(t, db, workspaceID, "elsewhere")
		send := eventSend(t, elsewhere, "o", `{}`)
		send.WorkspaceID = id26("ES-OTHER-WS")
		_, err = db.SendFoundryEvents(ctx, send)
		assert.ErrorIs(t, err, store.ErrNotFound)

		stripe := scheduledConnector(t, db, workspaceID, "stripe", nil)
		_, err = db.SendFoundryEvents(ctx, eventSend(t, stripe, "o", `{}`))
		assert.ErrorIs(t, err, store.ErrNotEventSource)
		assert.Equal(t, 0, eventBatchCount(t, elsewhere)+eventBatchCount(t, stripe))
	})

	// A send schedules one sync, a spacing from now that grows with the
	// table, and never moves one already scheduled: a busy source would
	// otherwise never sync.
	t.Run("schedules a sync the spacing away unless one is scheduled", func(t *testing.T) {
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "spacing")
		eventTable(t, c, "ws/snapshot-1", 1_000_000)
		start := time.Now()
		sent(t, db, eventSend(t, c, "o", `{}`))
		_, next, _, _ := storedEventSource(t, c)
		require.NotNil(t, next)
		assert.WithinDuration(t, start.Add(11*time.Minute), *next, 10*time.Second, "1m and 10m per million rows")

		sent(t, db, eventSend(t, c, "p", `{}`))
		_, again, _, _ := storedEventSource(t, c)
		require.NotNil(t, again)
		assert.Equal(t, *next, *again, "a later send keeps the scheduled sync")
	})

	// An owner's edit and a sender's send both write the connectors row;
	// neither may drop what the other wrote.
	t.Run("keeps every learned field and the scheduled sync through concurrent description edits", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "race")
		sent(t, db, eventSend(t, c, "first", `{"f00":1}`))
		_, scheduled, _, _ := storedEventSource(t, c)
		require.NotNil(t, scheduled)

		var wg sync.WaitGroup
		for i := 1; i <= 10; i++ {
			send := eventSend(t, c, fmt.Sprint("p", i), fmt.Sprintf(`{"f%02d":1}`, i))
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, err := db.SendFoundryEvents(ctx, send)
				assert.NoError(t, err)
			}()
			go func() {
				defer wg.Done()
				description := fmt.Sprintf("edit %d", i)
				assert.NoError(t, db.UpdateEventSource(ctx, workspaceID, c.ID, model.EventSourceUpdate{Description: &description}))
			}()
		}
		wg.Wait()
		config, next, _, _ := storedEventSource(t, c)
		schema := config["schema"].(map[string]any)
		assert.Len(t, schema, 11, "every send's field stays: %v", schema)
		assert.Regexp(t, `^edit \d+$`, config["description"])
		require.NotNil(t, next)
		assert.Equal(t, *scheduled, *next, "edits never move the scheduled sync")
	})
}

// mustValidate is a send of one piece per data.
func mustValidate(t *testing.T, data ...string) events.Send {
	t.Helper()
	pieces := make([]events.Piece, len(data))
	for i, d := range data {
		pieces[i] = events.Piece{ID: fmt.Sprint("v-", i), Data: []byte(d)}
	}
	send, err := events.Validate(pieces, eventLimits)
	require.NoError(t, err)
	return send
}

// refusedAs asserts an *events.Refusal of kind.
func refusedAs(kind events.RefusalKind) func(t *testing.T, err error) {
	return func(t *testing.T, err error) {
		t.Helper()
		var r *events.Refusal
		require.ErrorAs(t, err, &r)
		assert.Equal(t, kind, r.Kind, r.Message)
	}
}

func TestFoundryEventSourceState(t *testing.T) {
	workspaceID := id26("ES-STATE-WS")

	// The caps count the table and every batch no sync has merged; a batch
	// at or below the cursor is in the table already and must not count twice.
	t.Run("sums only the batches above the cursor", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "pending")
		sent(t, db, eventSend(t, c, "a", `{"amount":1}`))
		two := sent(t, db, eventSend(t, c, "b", `{"amount":2}`, `{"note":"x"}`))
		three := sent(t, db, eventSend(t, c, "c", `{"amount":3}`))
		eventTable(t, c, "ws/snapshot-1", 1, "amount")
		eventCursor(t, c, 1)

		state, err := db.EventSendState(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		assert.Equal(t, c.ID, state.Connector.ID)
		assert.Equal(t, int64(1), state.Cursor)
		assert.Equal(t, "ws/snapshot-1", state.TableKey)
		assert.Equal(t, int64(1), state.TableRows)
		assert.Equal(t, []string{"id", "received_at", "occurred_at", "amount"}, state.TableFields)
		assert.Equal(t, int64(3), state.PendingPieces)
		assert.Equal(t, two.Batch.Bytes+three.Batch.Bytes, state.PendingBytes)
		assert.Equal(t, []string{"amount", "note"}, fieldNames(state.Schema))

		list, err := db.ListFoundryEventBatches(ctx, workspaceID, c.ID, 2)
		require.NoError(t, err)
		assert.Equal(t, state.PendingPieces, list.State.PendingPieces)
		require.Len(t, list.Batches, 2, "at most limit, newest first")
		assert.Equal(t, []int64{3, 2}, []int64{list.Batches[0].Seq, list.Batches[1].Seq})
	})

	// Before its first sync a source has no table: the caps count its
	// batches alone.
	t.Run("reads a source with no table or cursor", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "new")
		state, err := db.EventSendState(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEventSourceState{Connector: state.Connector}, state)
		assert.Equal(t, events.Holding{}, state.Holding(0))
	})

	// Every event source RPC loads its source by workspace and id: another
	// workspace's, a deleted one or another kind is no source of the caller's.
	t.Run("refuses a source of another workspace, a deleted one and another kind", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "elsewhere")
		_, err := db.EventSendState(ctx, id26("ES-OTHER-WS"), c.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = db.ListFoundryEventBatches(ctx, id26("ES-OTHER-WS"), c.ID, 10)
		assert.ErrorIs(t, err, store.ErrNotFound)
		require.NoError(t, db.DeleteConnector(ctx, workspaceID, c.ID))
		_, err = db.EventSendState(ctx, workspaceID, c.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		stripe := scheduledConnector(t, db, workspaceID, "stripe", nil)
		_, err = db.ListFoundryEventBatches(ctx, workspaceID, stripe.ID, 10)
		assert.ErrorIs(t, err, store.ErrNotEventSource)
	})
}

func TestEventSourceNames(t *testing.T) {
	workspaceID := id26("ES-NAME-WS")

	// Names tell sources apart in prompts and the Data tree; two creates
	// racing with one name must leave one source, and the other learns its id.
	t.Run("creates one source of a name however many race", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		eventWorkspace(t, db, workspaceID)
		errs := make([]error, 8)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := newEventSource(workspaceID, fmt.Sprint("race", i))
				c.Name = "Orders"
				errs[i] = db.CreateEventSource(ctx, c, 20)
			}()
		}
		wg.Wait()
		var created []string
		for i, err := range errs {
			if err == nil {
				created = append(created, id26(fmt.Sprint("ES-race", i)))
			}
		}
		require.Len(t, created, 1)
		for _, err := range errs {
			if err == nil {
				continue
			}
			var taken *store.EventSourceNameTakenError
			require.ErrorAs(t, err, &taken)
			assert.Equal(t, created[0], taken.ConnectorID, "the refusal names the source that has the name")
			assert.ErrorIs(t, err, store.ErrConflict)
		}
	})

	// A workspace holds at most max sources, even when creates race.
	t.Run("creates at most max sources however many race", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		eventWorkspace(t, db, workspaceID)
		errs := make([]error, 8)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = db.CreateEventSource(ctx, newEventSource(workspaceID, fmt.Sprint("max", i)), 5)
			}()
		}
		wg.Wait()
		created := 0
		for _, err := range errs {
			if err == nil {
				created++
				continue
			}
			assert.ErrorIs(t, err, store.ErrLimitExceeded)
		}
		assert.Equal(t, 5, created)
	})

	// Only live event sources hold names and count: a deleted source, a
	// connector of another kind and another workspace's source never block one.
	t.Run("counts and names only the workspace's live event sources", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		gone := eventSource(t, db, workspaceID, "gone")
		require.NoError(t, db.DeleteConnector(ctx, workspaceID, gone.ID))
		stripe := scheduledConnector(t, db, workspaceID, "stripe", nil)
		other := eventSource(t, db, id26("ES-NAME-OTHER"), "other")

		for i, name := range []string{gone.Name, stripe.Name, other.Name} {
			c := newEventSource(workspaceID, fmt.Sprint("free", i))
			c.Name = name
			assert.NoError(t, db.CreateEventSource(ctx, c, 3), "%q is free", name)
		}
		err := db.CreateEventSource(ctx, newEventSource(workspaceID, "fourth"), 3)
		assert.ErrorIs(t, err, store.ErrLimitExceeded)
	})

	// Names compare in the column's collation, as people read them.
	t.Run("refuses a name that differs only in case", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		orders := newEventSource(workspaceID, "orders")
		orders.Name = "Orders"
		eventWorkspace(t, db, workspaceID)
		require.NoError(t, db.CreateEventSource(ctx, orders, 20))
		clash := newEventSource(workspaceID, "clash")
		clash.Name = "orders"
		var taken *store.EventSourceNameTakenError
		require.ErrorAs(t, db.CreateEventSource(ctx, clash, 20), &taken)
		assert.Equal(t, orders.ID, taken.ConnectorID)
	})

	// A gone workspace takes no new sources, and only an event source is
	// created this way.
	t.Run("refuses a deleted workspace and another kind", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		eventWorkspace(t, db, workspaceID)
		deleteWorkspace(t, workspaceID)
		assert.ErrorIs(t, db.CreateEventSource(ctx, newEventSource(workspaceID, "late"), 20), store.ErrNotFound)
		stripe := newEventSource(id26("ES-NAME-OTHER"), "stripe")
		stripe.Kind = "stripe"
		assert.Error(t, db.CreateEventSource(ctx, stripe, 20))
	})
}

func TestUpdateEventSource(t *testing.T) {
	workspaceID := id26("ES-EDIT-WS")

	// An edit is the owner's: it must never write what sends and syncs own,
	// or it could drop a learned field, a sync's place or a scheduled sync.
	t.Run("writes the name and description only", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "edit")
		sent(t, db, eventSend(t, c, "o", `{"amount":1}`))
		eventCursor(t, c, 1)
		beforeConfig, beforeNext, beforeCursor, beforeSecret := storedEventSource(t, c)

		name, description := "Shop orders", "Every order the shop takes"
		require.NoError(t, db.UpdateEventSource(ctx, workspaceID, c.ID, model.EventSourceUpdate{Name: &name, Description: &description}))
		config, next, cursor, secret := storedEventSource(t, c)
		assert.Equal(t, description, config["description"])
		assert.Equal(t, beforeConfig["schema"], config["schema"])
		assert.Equal(t, beforeNext, next)
		assert.Equal(t, beforeCursor, cursor)
		assert.Equal(t, beforeSecret, secret)
		stored, err := db.GetConnector(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		assert.Equal(t, name, stored.Name)

		// A description alone keeps the name.
		description = "Orders, refunds included"
		require.NoError(t, db.UpdateEventSource(ctx, workspaceID, c.ID, model.EventSourceUpdate{Description: &description}))
		stored, err = db.GetConnector(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		assert.Equal(t, name, stored.Name)
	})

	// A rename follows create's rule, and keeping one's own name is no clash.
	t.Run("refuses a rename to another source's name", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		a := eventSource(t, db, workspaceID, "a")
		b := eventSource(t, db, workspaceID, "b")
		var taken *store.EventSourceNameTakenError
		require.ErrorAs(t, db.UpdateEventSource(ctx, workspaceID, b.ID, model.EventSourceUpdate{Name: &a.Name}), &taken)
		assert.Equal(t, a.ID, taken.ConnectorID)
		assert.NoError(t, db.UpdateEventSource(ctx, workspaceID, b.ID, model.EventSourceUpdate{Name: &b.Name}))
	})

	// Only a live event source of the workspace is edited this way.
	t.Run("refuses an empty edit and anything but a live event source of the workspace", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "c")
		description := "x"
		assert.Error(t, db.UpdateEventSource(ctx, workspaceID, c.ID, model.EventSourceUpdate{}))
		assert.ErrorIs(t, db.UpdateEventSource(ctx, id26("ES-OTHER-WS"), c.ID, model.EventSourceUpdate{Description: &description}), store.ErrNotFound)
		stripe := scheduledConnector(t, db, workspaceID, "stripe", nil)
		assert.ErrorIs(t, db.UpdateEventSource(ctx, workspaceID, stripe.ID, model.EventSourceUpdate{Description: &description}), store.ErrNotFound)
		require.NoError(t, db.DeleteConnector(ctx, workspaceID, c.ID))
		assert.ErrorIs(t, db.UpdateEventSource(ctx, workspaceID, c.ID, model.EventSourceUpdate{Description: &description}), store.ErrNotFound)
	})
}

// textField is an update that makes field its source's text field, within
// 200 fields, for user-1.
func textField(field string) model.EventSourceUpdate {
	return model.EventSourceUpdate{Text: &model.EventSourceText{Field: field, MaxFields: 200, RequestedByUserID: ptr("user-1")}}
}

// eventSyncs are c's syncs, newest first.
func eventSyncs(t *testing.T, db store.Store, c *model.Connector) []*model.ConnectorSync {
	t.Helper()
	syncs, err := db.ListConnectorSyncs(context.Background(), c.WorkspaceID, c.ID, 100)
	require.NoError(t, err)
	return syncs
}

// finishSyncs ends c's live sync, as the sync worker does.
func finishSyncs(t *testing.T, c *model.Connector) {
	t.Helper()
	_, err := testDB.Exec(`UPDATE connector_syncs SET status='succeeded', active_key=NULL, finished_at=NOW(6) WHERE connector_id=?`, c.ID)
	require.NoError(t, err)
}

// A text field is the body of each piece's document: changing it locks it as
// a string in the schema the sends write, under the lock they take, and
// queues the one full sync that renders the newest pieces by it, all or
// nothing. Nothing that sends and syncs own moves.
func TestEventSourceTextField(t *testing.T) {
	workspaceID := id26("ES-TEXT-WS")

	t.Run("locks a new field as a string and queues one full sync with it", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "lock")
		sent(t, db, eventSend(t, c, "o", `{"amount":1,"note":null,"title":"Hi"}`))
		eventCursor(t, c, 1)
		before, beforeNext, beforeCursor, beforeSecret := storedEventSource(t, c)

		for _, tc := range []struct{ field, schema string }{
			// One no piece has carried is added, one only ever null is
			// locked, and one held as a string stays as it is.
			{"reply", "map[amount:number note:null reply:string title:string]"},
			{"note", "map[amount:number note:string reply:string title:string]"},
			{"title", "map[amount:number note:string reply:string title:string]"},
		} {
			require.NoError(t, db.UpdateEventSource(ctx, workspaceID, c.ID, textField(tc.field)), tc.field)
			config, next, cursor, secret := storedEventSource(t, c)
			assert.Equal(t, tc.field, config["text_field"])
			assert.Equal(t, tc.schema, fmt.Sprint(config["schema"]), tc.field)
			assert.Equal(t, before["description"], config["description"], "a text field alone keeps the description")
			assert.Equal(t, beforeNext, next)
			assert.Equal(t, beforeCursor, cursor)
			assert.Equal(t, beforeSecret, secret)
			syncs := eventSyncs(t, db, c)
			require.NotEmpty(t, syncs)
			live := syncs[0]
			assert.Equal(t, model.ConnectorSyncQueued, live.Status)
			assert.Equal(t, model.ConnectorSyncEvents, live.Origin)
			assert.Equal(t, model.ConnectorSyncFull, live.Mode)
			require.NotNil(t, live.RequestedByUserID)
			assert.Equal(t, "user-1", *live.RequestedByUserID)
			finishSyncs(t, c)
		}
		assert.Len(t, eventSyncs(t, db, c), 3, "one sync for each change")
	})

	t.Run("writes and queues nothing for the field it already has", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "same")
		require.NoError(t, db.UpdateEventSource(ctx, workspaceID, c.ID, textField("reply")))
		finishSyncs(t, c)
		before, _, _, _ := storedEventSource(t, c)
		require.NoError(t, db.UpdateEventSource(ctx, workspaceID, c.ID, textField("reply")))
		after, _, _, _ := storedEventSource(t, c)
		assert.Equal(t, before, after)
		assert.Len(t, eventSyncs(t, db, c), 1)
	})

	// "" turns the documents off; the field stays locked, as sends may
	// already have complied.
	t.Run("clears the field and queues the sync that empties the set", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "clear")
		require.NoError(t, db.UpdateEventSource(ctx, workspaceID, c.ID, textField("reply")))
		finishSyncs(t, c)
		require.NoError(t, db.UpdateEventSource(ctx, workspaceID, c.ID, textField("")))
		config, _, _, _ := storedEventSource(t, c)
		assert.Equal(t, "", config["text_field"])
		assert.Equal(t, "map[reply:string]", fmt.Sprint(config["schema"]))
		syncs := eventSyncs(t, db, c)
		require.Len(t, syncs, 2)
		assert.Equal(t, model.ConnectorSyncFull, syncs[0].Mode)
		assert.Equal(t, model.ConnectorSyncQueued, syncs[0].Status)
	})

	// A refusal or a live sync leaves the source exactly as it was: no
	// description, no lock, no sync.
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, db store.Store, c *model.Connector)
		update model.EventSourceUpdate
		check  func(t *testing.T, err error)
	}{
		{"refuses a field held as another type", nil, textField("amount"), refusedAs(events.Invalid)},
		{"refuses a field that would pass the field cap", nil,
			model.EventSourceUpdate{Text: &model.EventSourceText{Field: "reply", MaxFields: 2}}, refusedAs(events.TooManyFields)},
		{"refuses a reserved name", nil, textField("received_at"), refusedAs(events.Invalid)},
		{"refuses a change while a sync is live, since it read the old field", func(t *testing.T, db store.Store, c *model.Connector) {
			require.NoError(t, db.EnqueueConnectorSync(context.Background(), &model.ConnectorSync{ID: id.New(), ConnectorID: c.ID,
				WorkspaceID: c.WorkspaceID, Origin: model.ConnectorSyncEvents}))
		}, textField("reply"), func(t *testing.T, err error) { assert.ErrorIs(t, err, store.ErrConflict) }},
	} {
		t.Run(tc.name+", writing nothing", func(t *testing.T) {
			ctx := context.Background()
			db := newTestStore(t)
			c := eventSource(t, db, workspaceID, "refused")
			sent(t, db, eventSend(t, c, "o", `{"amount":1,"title":"Hi"}`))
			if tc.setup != nil {
				tc.setup(t, db, c)
			}
			syncs := len(eventSyncs(t, db, c))
			before, _, _, _ := storedEventSource(t, c)
			update, description := tc.update, "never stored"
			update.Description = &description
			tc.check(t, db.UpdateEventSource(ctx, workspaceID, c.ID, update))
			after, _, _, _ := storedEventSource(t, c)
			assert.Equal(t, before, after)
			assert.Len(t, eventSyncs(t, db, c), syncs)
		})
	}

	t.Run("refuses anything but a live event source of the workspace", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "gone")
		assert.ErrorIs(t, db.UpdateEventSource(ctx, id26("ES-OTHER-WS"), c.ID, textField("reply")), store.ErrNotFound)
		stripe := scheduledConnector(t, db, workspaceID, "stripe", nil)
		assert.ErrorIs(t, db.UpdateEventSource(ctx, workspaceID, stripe.ID, textField("reply")), store.ErrNotFound)
		require.NoError(t, db.DeleteConnector(ctx, workspaceID, c.ID))
		assert.ErrorIs(t, db.UpdateEventSource(ctx, workspaceID, c.ID, textField("reply")), store.ErrNotFound)
		assert.Empty(t, eventSyncs(t, db, c))
		assert.Empty(t, eventSyncs(t, db, stripe))
	})

	// Sends learn fields under the row lock the change takes: none is lost,
	// and exactly one change wins, since its sync is live for the rest.
	t.Run("keeps every field concurrent sends learn, and one change", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "race")
		sent(t, db, eventSend(t, c, "first", `{"f00":1}`))
		_, scheduled, _, _ := storedEventSource(t, c)
		require.NotNil(t, scheduled)

		var wg sync.WaitGroup
		var mu sync.Mutex
		var won []string
		for i := 1; i <= 8; i++ {
			send := eventSend(t, c, fmt.Sprint("p", i), fmt.Sprintf(`{"f%02d":1}`, i))
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, err := db.SendFoundryEvents(ctx, send)
				assert.NoError(t, err)
			}()
			go func() {
				defer wg.Done()
				field, description := fmt.Sprintf("text%d", i), fmt.Sprintf("edit %d", i)
				update := textField(field)
				update.Description = &description
				if err := db.UpdateEventSource(ctx, workspaceID, c.ID, update); err != nil {
					assert.ErrorIs(t, err, store.ErrConflict)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				won = append(won, fmt.Sprint(i))
			}()
		}
		wg.Wait()
		require.Len(t, won, 1)
		config, next, _, _ := storedEventSource(t, c)
		schema := config["schema"].(map[string]any)
		assert.Len(t, schema, 10, "every send's field and the winner's text field: %v", schema)
		assert.Equal(t, "string", schema["text"+won[0]])
		assert.Equal(t, "text"+won[0], config["text_field"])
		assert.Equal(t, "edit "+won[0], config["description"], "a refused change writes no description either")
		require.NotNil(t, next)
		assert.Equal(t, *scheduled, *next, "changes never move the scheduled sync")
		assert.Len(t, eventSyncs(t, db, c), 1)
	})

	// A sync that finishes clears its active_key, then sets the source's
	// next sync (FinishConnectorSync); a change takes the source's row,
	// then inserts its sync into that same unique key. Run in between,
	// InnoDB breaks the deadlock by rolling one back. The change then either
	// wins, its sync queued once the finish commits, or is refused as a
	// live sync's: never the deadlock itself.
	t.Run("changes the field, or refuses it as a live sync's, while a sync finishes", func(t *testing.T) {
		ctx := context.Background()
		// Lock waits are in performance_schema, which the test user, like
		// the app's, may not read: it holds only its own schema
		// (docker-compose.test.yml). Root reads them.
		rootDSN := os.Getenv("HELIX_TEST_ROOT_DSN")
		if rootDSN == "" {
			rootDSN = "root:helix@tcp(127.0.0.1:3307)/"
		}
		root, err := sql.Open("mysql", rootDSN)
		require.NoError(t, err)
		t.Cleanup(func() { _ = root.Close() })
		require.NoError(t, root.PingContext(ctx), "reading lock waits needs HELIX_TEST_ROOT_DSN")
		for round := range 3 {
			db := newTestStore(t)
			c := eventSource(t, db, workspaceID, fmt.Sprint("finish", round))
			live := &model.ConnectorSync{ID: id.New(), ConnectorID: c.ID, WorkspaceID: c.WorkspaceID, Origin: model.ConnectorSyncEvents}
			require.NoError(t, db.EnqueueConnectorSync(ctx, live))
			finish, err := testDB.BeginTx(ctx, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = finish.Rollback() })
			_, err = finish.ExecContext(ctx, `SELECT id FROM connector_syncs WHERE id=? FOR UPDATE`, live.ID)
			require.NoError(t, err)
			_, err = finish.ExecContext(ctx, `UPDATE connector_syncs SET status='succeeded', active_key=NULL, finished_at=NOW(6) WHERE id=?`, live.ID)
			require.NoError(t, err)
			// Only a wait on the finish's transaction counts, since other
			// packages' tests share the server. By transaction, not thread:
			// the key the finish cleared is an implicit lock, and InnoDB
			// names the waiter as the thread of the lock it makes explicit.
			var finishConn, finishTrx uint64
			require.NoError(t, finish.QueryRowContext(ctx, `SELECT CONNECTION_ID()`).Scan(&finishConn))
			require.NoError(t, root.QueryRowContext(ctx, `SELECT trx_id FROM information_schema.innodb_trx WHERE trx_mysql_thread_id=?`,
				finishConn).Scan(&finishTrx))

			changed := make(chan error, 1)
			go func() { changed <- db.UpdateEventSource(ctx, workspaceID, c.ID, textField("reply")) }()
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				var waiting int
				require.NoError(collect, root.QueryRowContext(ctx, `SELECT COUNT(*) FROM performance_schema.data_lock_waits
WHERE BLOCKING_ENGINE_TRANSACTION_ID=?`, finishTrx).Scan(&waiting))
				assert.Positive(collect, waiting)
			}, 10*time.Second, 10*time.Millisecond, "the change waits on the finishing sync's key")
			_, err = finish.ExecContext(ctx, `UPDATE connectors c JOIN connector_syncs s ON s.connector_id=c.id
SET c.next_sync_at=c.next_sync_at WHERE s.id=?`, live.ID)
			if err == nil {
				require.NoError(t, finish.Commit())
			} else {
				_ = finish.Rollback()
			}
			finished := err == nil

			err = <-changed
			syncs := eventSyncs(t, db, c)
			config, _, _, _ := storedEventSource(t, c)
			switch {
			case err == nil:
				require.True(t, finished, "the change won only once the finish committed")
				require.Len(t, syncs, 2)
				assert.Equal(t, model.ConnectorSyncQueued, syncs[0].Status)
				assert.Equal(t, model.ConnectorSyncFull, syncs[0].Mode)
				assert.Equal(t, "reply", config["text_field"])
			default:
				require.ErrorIs(t, err, store.ErrConflict)
				require.Len(t, syncs, 1, "a refused change queues nothing")
				assert.Nil(t, config["text_field"], "a refused change writes nothing")
			}
		}
	})

	// A send carrying the field as a number and a change locking it as a
	// string race for the field: whichever commits first holds it, and the
	// other is refused.
	t.Run("lets a send or a change hold a field, never both", func(t *testing.T) {
		ctx := context.Background()
		for round := range 5 {
			db := newTestStore(t)
			c := eventSource(t, db, workspaceID, fmt.Sprint("type", round))
			send := eventSend(t, c, "n", `{"reply":7}`)
			var sendErr, changeErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _, sendErr = db.SendFoundryEvents(ctx, send) }()
			go func() { defer wg.Done(); changeErr = db.UpdateEventSource(ctx, workspaceID, c.ID, textField("reply")) }()
			wg.Wait()
			config, _, _, _ := storedEventSource(t, c)
			schema := config["schema"].(map[string]any)
			switch {
			case sendErr == nil:
				refusedAs(events.Invalid)(t, changeErr)
				assert.Equal(t, "number", schema["reply"])
				assert.Nil(t, config["text_field"])
				assert.Equal(t, 1, eventBatchCount(t, c))
			default:
				require.NoError(t, changeErr)
				refusedAs(events.Invalid)(t, sendErr)
				assert.Equal(t, "string", schema["reply"])
				assert.Equal(t, "reply", config["text_field"])
				assert.Equal(t, 0, eventBatchCount(t, c))
			}
		}
	})
}

func TestFoundryEventSourceTokens(t *testing.T) {
	workspaceID := id26("ES-TOKEN-WS")
	token := func(c *model.Connector, tag string) *model.FoundryEventSourceToken {
		return &model.FoundryEventSourceToken{
			ID: id26("ET-" + tag), WorkspaceID: c.WorkspaceID, ConnectorID: c.ID,
			Hash: fmt.Sprintf("%064x", []byte(tag)), Prefix: "hx_ev_0a1b2c", CreatedBy: id26("USER"),
		}
	}

	// A source holds at most maxLive live tokens, even when creates race;
	// revoking one makes room.
	t.Run("creates at most maxLive live tokens however many race", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "max")
		errs := make([]error, 15)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = db.CreateFoundryEventSourceToken(ctx, token(c, fmt.Sprintf("max%02d", i)), 10)
			}()
		}
		wg.Wait()
		var created []string
		for i, err := range errs {
			if err == nil {
				created = append(created, id26(fmt.Sprintf("ET-max%02d", i)))
				continue
			}
			assert.ErrorIs(t, err, store.ErrLimitExceeded)
		}
		require.Len(t, created, 10)
		assert.ErrorIs(t, db.CreateFoundryEventSourceToken(ctx, token(c, "eleventh"), 10), store.ErrLimitExceeded)
		_, err := db.RevokeFoundryEventSourceToken(ctx, workspaceID, c.ID, created[0])
		require.NoError(t, err)
		next := token(c, "eleventh")
		require.NoError(t, db.CreateFoundryEventSourceToken(ctx, next, 10))
		assert.WithinDuration(t, time.Now(), next.CreatedAt, time.Minute, "the create sets created_at")
	})

	// A token is only ever a live event source's.
	t.Run("refuses a token for anything but a live event source of the workspace", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "refuse")
		stray := token(c, "stray")
		stray.WorkspaceID = id26("ES-OTHER-WS")
		assert.ErrorIs(t, db.CreateFoundryEventSourceToken(ctx, stray, 10), store.ErrNotFound)
		stripe := scheduledConnector(t, db, workspaceID, "stripe", nil)
		assert.ErrorIs(t, db.CreateFoundryEventSourceToken(ctx, token(stripe, "stripe"), 10), store.ErrNotEventSource)
		require.NoError(t, db.CreateFoundryEventSourceToken(ctx, token(c, "first"), 10))
		twin := token(c, "first")
		twin.ID = id26("ET-twin")
		assert.ErrorIs(t, db.CreateFoundryEventSourceToken(ctx, twin, 10), store.ErrConflict, "a hash is stored once")
		require.NoError(t, db.DeleteConnector(ctx, workspaceID, c.ID))
		assert.ErrorIs(t, db.CreateFoundryEventSourceToken(ctx, token(c, "late"), 10), store.ErrNotFound)
	})

	// The panel lists every token a source has had, newest first, so people
	// can tell a revoked one from a live one.
	t.Run("lists a source's tokens newest first, revoked ones included", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "list")
		other := eventSource(t, db, workspaceID, "list-other")
		for _, tag := range []string{"t1", "t2", "t3"} {
			require.NoError(t, db.CreateFoundryEventSourceToken(ctx, token(c, tag), 10))
			_, err := testDB.Exec(`UPDATE foundry_event_source_tokens SET created_at = NOW(6) - INTERVAL ? SECOND WHERE id=?`,
				map[string]int{"t1": 30, "t2": 20, "t3": 10}[tag], id26("ET-"+tag))
			require.NoError(t, err)
		}
		require.NoError(t, db.CreateFoundryEventSourceToken(ctx, token(other, "o1"), 10))
		_, err := db.RevokeFoundryEventSourceToken(ctx, workspaceID, c.ID, id26("ET-t2"))
		require.NoError(t, err)

		list, err := db.ListFoundryEventSourceTokens(ctx, workspaceID, c.ID, 50)
		require.NoError(t, err)
		var ids []string
		for _, tok := range list {
			ids = append(ids, tok.ID)
		}
		assert.Equal(t, []string{id26("ET-t3"), id26("ET-t2"), id26("ET-t1")}, ids)
		assert.NotNil(t, list[1].RevokedAt)
		assert.Nil(t, list[0].RevokedAt)
		limited, err := db.ListFoundryEventSourceTokens(ctx, workspaceID, c.ID, 2)
		require.NoError(t, err)
		assert.Len(t, limited, 2)
		none, err := db.ListFoundryEventSourceTokens(ctx, id26("ES-OTHER-WS"), c.ID, 50)
		require.NoError(t, err)
		assert.Empty(t, none, "another workspace sees none of them")
	})

	// The registry checks only the workspace, so a revoke must match the
	// token, its source and its workspace together.
	t.Run("revokes only a token matching all three ids, twice without error", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "revoke")
		other := eventSource(t, db, workspaceID, "revoke-other")
		require.NoError(t, db.CreateFoundryEventSourceToken(ctx, token(c, "r1"), 10))
		tokID := id26("ET-r1")
		_, err := db.RevokeFoundryEventSourceToken(ctx, workspaceID, other.ID, tokID)
		assert.ErrorIs(t, err, store.ErrNotFound, "another source's token")
		_, err = db.RevokeFoundryEventSourceToken(ctx, id26("ES-OTHER-WS"), c.ID, tokID)
		assert.ErrorIs(t, err, store.ErrNotFound, "another workspace's")
		_, err = db.ValidateFoundryEventSourceToken(ctx, token(c, "r1").Hash)
		require.NoError(t, err, "the misses revoked nothing")

		first, err := db.RevokeFoundryEventSourceToken(ctx, workspaceID, c.ID, tokID)
		require.NoError(t, err)
		require.NotNil(t, first.RevokedAt)
		again, err := db.RevokeFoundryEventSourceToken(ctx, workspaceID, c.ID, tokID)
		require.NoError(t, err)
		assert.Equal(t, *first.RevokedAt, *again.RevokedAt, "a second revoke keeps the first time")
	})

	// A token authenticates only while it, its source and its workspace
	// are live and agree; revocation takes effect at the next request.
	t.Run("validates only a live token of a live source of its own workspace", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "valid")
		for _, tag := range []string{"live", "revoked", "moved"} {
			require.NoError(t, db.CreateFoundryEventSourceToken(ctx, token(c, tag), 10))
		}
		got, err := db.ValidateFoundryEventSourceToken(ctx, token(c, "live").Hash)
		require.NoError(t, err)
		assert.Equal(t, []string{id26("ET-live"), workspaceID, c.ID}, []string{got.ID, got.WorkspaceID, got.ConnectorID})
		live, err := db.GetLiveFoundryEventSourceToken(ctx, workspaceID, c.ID, id26("ET-live"))
		require.NoError(t, err)
		assert.Equal(t, got.Hash, live.Hash)
		_, err = db.GetLiveFoundryEventSourceToken(ctx, workspaceID, eventSource(t, db, workspaceID, "valid-other").ID, id26("ET-live"))
		assert.ErrorIs(t, err, store.ErrNotFound, "a send to another source under it")

		_, err = db.RevokeFoundryEventSourceToken(ctx, workspaceID, c.ID, id26("ET-revoked"))
		require.NoError(t, err)
		_, err = db.ValidateFoundryEventSourceToken(ctx, token(c, "revoked").Hash)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = db.GetLiveFoundryEventSourceToken(ctx, workspaceID, c.ID, id26("ET-revoked"))
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = db.ValidateFoundryEventSourceToken(ctx, fmt.Sprintf("%064x", 7))
		assert.ErrorIs(t, err, store.ErrNotFound, "an unknown hash")

		// A token row naming another workspace than its connector's never authenticates.
		_, err = testDB.Exec(`UPDATE foundry_event_source_tokens SET workspace_id=? WHERE id=?`, id26("ES-OTHER-WS"), id26("ET-moved"))
		require.NoError(t, err)
		_, err = db.ValidateFoundryEventSourceToken(ctx, token(c, "moved").Hash)
		assert.ErrorIs(t, err, store.ErrNotFound)

		// Nor does any token of a deleted source or workspace.
		deleteWorkspace(t, workspaceID)
		_, err = db.ValidateFoundryEventSourceToken(ctx, token(c, "live").Hash)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// last_used_at tells people which token is in use; writing it on every
	// request would write the row up to every send of a busy sender.
	t.Run("records a token's use at most once a minute", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "used")
		tok := token(c, "used")
		require.NoError(t, db.CreateFoundryEventSourceToken(ctx, tok, 10))
		usedAt := func() time.Time {
			t.Helper()
			var at sql.NullTime
			require.NoError(t, testDB.QueryRow(`SELECT last_used_at FROM foundry_event_source_tokens WHERE id=?`, tok.ID).Scan(&at))
			require.True(t, at.Valid, "a validated token records its use")
			return at.Time
		}
		_, err := db.ValidateFoundryEventSourceToken(ctx, tok.Hash)
		require.NoError(t, err)
		first := usedAt()
		_, err = db.ValidateFoundryEventSourceToken(ctx, tok.Hash)
		require.NoError(t, err)
		assert.Equal(t, first, usedAt(), "a second use within the minute writes nothing")

		_, err = testDB.Exec(`UPDATE foundry_event_source_tokens SET last_used_at = NOW(6) - INTERVAL 61 SECOND WHERE id=?`, tok.ID)
		require.NoError(t, err)
		_, err = db.ValidateFoundryEventSourceToken(ctx, tok.Hash)
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now(), usedAt(), 10*time.Second, "a use after the minute moves it")
	})
}

// Deleting an event source goes as every connector's delete does, in one
// transaction: its table goes and what it put in the graph goes with the
// refresh the delete owes, and its send tokens stop at once, before the
// purge removes them with its batches.
func TestDeleteEventSource(t *testing.T) {
	ctx := context.Background()
	token := func(c *model.Connector, tag string) *model.FoundryEventSourceToken {
		return &model.FoundryEventSourceToken{
			ID: id26("ET-" + tag), WorkspaceID: c.WorkspaceID, ConnectorID: c.ID,
			Hash: fmt.Sprintf("%064x", []byte(tag)), Prefix: "hx_ev_0a1b2c", CreatedBy: id26("USER"),
		}
	}
	f := newFoundryFixture(t, "ES-DEL")
	ws := f.ontology.WorkspaceID
	c := eventSource(t, f.db, ws, "deleted")
	sent(t, f.db, eventSend(t, c, "o", `{"amount":1}`))
	eventTable(t, c, "ws/deleted-snapshot", 1, "amount")
	require.NoError(t, f.db.CreateFoundryEventSourceToken(ctx, token(c, "d1"), 10))
	require.NoError(t, f.db.CreateFoundryEventSourceToken(ctx, token(c, "d2"), 10))
	earlier, err := f.db.RevokeFoundryEventSourceToken(ctx, ws, c.ID, id26("ET-d2"))
	require.NoError(t, err)
	// The ontology's current load maps the source's table.
	f.currentReading(t, "ES-DEL", []model.OntologyGenerationTable{{ConnectorID: c.ID, Table: "events"}}, []string{generalSetID(f.ontology.ID)})
	other := eventSource(t, f.db, ws, "kept")
	require.NoError(t, f.db.CreateFoundryEventSourceToken(ctx, token(other, "k1"), 10))

	start := time.Now().UTC()
	require.NoError(t, f.db.DeleteConnector(ctx, ws, c.ID))

	_, err = f.db.GetConnector(ctx, ws, c.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	tables, err := f.db.ListConnectorTables(ctx, ws, c.ID)
	require.NoError(t, err)
	assert.Empty(t, tables, "its table goes")
	assert.Equal(t, []string{"ws/deleted-snapshot"}, replacedKeys(t, f.db), "its snapshot waits for the sweep")
	due := f.ontologyNow(t).RefreshDueAt
	require.NotNil(t, due, "the ontology mapping its table owes a refresh")
	assert.WithinDuration(t, start, *due, 5*time.Second)
	list, err := f.db.ListFoundryEventSourceTokens(ctx, ws, c.ID, 50)
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, tok := range list {
		require.NotNil(t, tok.RevokedAt, tok.ID)
	}
	d2 := list[slices.IndexFunc(list, func(tok *model.FoundryEventSourceToken) bool { return tok.ID == id26("ET-d2") })]
	assert.Equal(t, *earlier.RevokedAt, *d2.RevokedAt, "a token revoked before keeps its time")
	_, err = f.db.ValidateFoundryEventSourceToken(ctx, token(c, "d1").Hash)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Equal(t, 1, eventBatchCount(t, c), "its batches wait for the purge")
	kept, err := f.db.ListFoundryEventSourceTokens(ctx, ws, other.ID, 50)
	require.NoError(t, err)
	require.Len(t, kept, 1)
	assert.Nil(t, kept[0].RevokedAt, "another source's token stays live")
	assert.ErrorIs(t, f.db.DeleteConnector(ctx, ws, c.ID), store.ErrNotFound, "a source is deleted once")

	// A deleted workspace's sources go with its other connectors, their
	// tokens revoked the same way.
	require.NoError(t, f.db.DeleteWorkspaceConnectors(ctx, ws))
	kept, err = f.db.ListFoundryEventSourceTokens(ctx, ws, other.ID, 50)
	require.NoError(t, err)
	require.Len(t, kept, 1)
	assert.NotNil(t, kept[0].RevokedAt)
}

func TestEventSourceSyncReads(t *testing.T) {
	workspaceID := id26("ES-READ-WS")

	// One read's memory stays bounded however the backlog is shaped: each
	// bound cuts the read, and the rest waits for the next sync.
	for _, c := range []struct {
		name   string
		budget events.ReadBudget
		want   []int64
	}{
		{"batches", events.ReadBudget{Batches: 2, Pieces: 1000, Bytes: 1 << 30}, []int64{1, 2}},
		{"pieces", events.ReadBudget{Batches: 100, Pieces: 4, Bytes: 1 << 30}, []int64{1, 2}},
		{"estimated bytes", events.ReadBudget{Batches: 100, Pieces: 1000, Bytes: 1}, []int64{1}},
	} {
		t.Run("cuts a read at its budget of "+c.name, func(t *testing.T) {
			ctx := context.Background()
			db := newTestStore(t)
			src := eventSource(t, db, workspaceID, "cut")
			for i := range 3 {
				sent(t, db, eventSend(t, src, fmt.Sprint("b", i), `{"amount":1}`, `{"amount":2}`))
			}
			_, batches, err := db.EventBatchesAfter(ctx, src.ID, 0, c.budget)
			require.NoError(t, err)
			assert.Equal(t, c.want, batchSeqs(batches))
		})
	}

	// A backlog larger than one read's bytes is read across syncs, each
	// batch exactly once and in order, so no piece is lost or merged twice.
	t.Run("splits a backlog over the byte budget across reads", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "split")
		var sizes []int64
		for i := range 7 {
			sizes = append(sizes, sent(t, db, eventSend(t, c, fmt.Sprint("b", i), `{"text":"`+strings.Repeat("x", 1000)+`"}`)).Batch.Bytes)
		}
		schema, _, err := db.EventBatchesAfter(ctx, c.ID, 0, eventBudget)
		require.NoError(t, err)
		// Two batches and their padding and parsed fields fit; a third does not.
		perPiece := schema.NullFill() + 64*int64(schema.Len())
		budget := events.ReadBudget{Batches: 100, Pieces: 1000, Bytes: sizes[0] + sizes[1] + 2*perPiece}

		var read []int64
		after := int64(0)
		for reads := 0; ; reads++ {
			require.Less(t, reads, 10, "the backlog drains")
			backlog, err := db.EventBacklog(ctx, c.ID, after, budget)
			require.NoError(t, err)
			_, batches, err := db.EventBatchesAfter(ctx, c.ID, after, budget)
			require.NoError(t, err)
			if len(batches) == 0 {
				assert.Equal(t, events.BacklogNone, backlog)
				break
			}
			if after+int64(len(batches)) < 7 {
				assert.Equal(t, events.BacklogMore, backlog, "more waits above %d than one read takes", after)
			} else {
				assert.Equal(t, events.BacklogSome, backlog, "one read takes what waits above %d", after)
			}
			assert.LessOrEqual(t, len(batches), 2)
			read = append(read, batchSeqs(batches)...)
			after = batches[len(batches)-1].Seq
		}
		assert.Equal(t, []int64{1, 2, 3, 4, 5, 6, 7}, read)
	})

	// The scheduler syncs at once only when one read cannot take the
	// backlog: a backlog of exactly one read's batches is Some, one more is More.
	t.Run("tells a backlog one read takes from a longer one", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "backlog")
		budget := events.ReadBudget{Batches: 2, Pieces: 1000, Bytes: 1 << 30}
		backlog, err := db.EventBacklog(ctx, c.ID, 0, budget)
		require.NoError(t, err)
		assert.Equal(t, events.BacklogNone, backlog)
		for i := range 3 {
			sent(t, db, eventSend(t, c, fmt.Sprint("b", i), `{}`))
		}
		for after, want := range map[int64]events.Backlog{0: events.BacklogMore, 1: events.BacklogSome, 2: events.BacklogSome, 3: events.BacklogNone} {
			backlog, err := db.EventBacklog(ctx, c.ID, after, budget)
			require.NoError(t, err)
			assert.Equal(t, want, backlog, "above %d", after)
		}
	})

	// A full read starts from the stored cursor and snapshot; before the
	// first sync there are neither.
	t.Run("reads the table's schema, cursor and snapshot", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "table")
		schema, cursor, key, err := db.EventTable(ctx, c.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, schema.Len())
		assert.Equal(t, int64(0), cursor)
		assert.Equal(t, "", key)

		sent(t, db, eventSend(t, c, "o", `{"amount":1}`))
		eventTable(t, c, "ws/snapshot-1", 1, "amount")
		eventCursor(t, c, 1)
		schema, cursor, key, err = db.EventTable(ctx, c.ID)
		require.NoError(t, err)
		assert.Equal(t, []string{"amount"}, fieldNames(schema))
		assert.Equal(t, int64(1), cursor)
		assert.Equal(t, "ws/snapshot-1", key)
	})

	// The kind reads only event sources; an unknown id or another kind's is
	// a fault, never an empty read that would publish an empty table.
	t.Run("refuses an unknown id and another kind", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		stripe := scheduledConnector(t, db, workspaceID, "stripe", nil)
		for _, connectorID := range []string{id26("ES-NONE"), stripe.ID} {
			_, _, err := db.EventBatchesAfter(ctx, connectorID, 0, eventBudget)
			assert.ErrorIs(t, err, store.ErrNotFound)
			_, _, _, err = db.EventTable(ctx, connectorID)
			assert.ErrorIs(t, err, store.ErrNotFound)
			_, err = db.EventBacklog(ctx, connectorID, 0, eventBudget)
			assert.ErrorIs(t, err, store.ErrNotFound)
		}
	})

	// A change read pads each row to the schema it returns, so that schema
	// must hold every field of every batch it returns, while sends commit
	// new fields under it.
	t.Run("never returns a batch with a field its schema lacks", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "fields")
		sends := make([]model.FoundryEventSend, 30)
		for i := range sends {
			sends[i] = eventSend(t, c, fmt.Sprint("p", i), fmt.Sprintf(`{"f%02d":1}`, i))
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			for _, send := range sends {
				_, err := db.SendFoundryEvents(ctx, send)
				assert.NoError(t, err)
			}
		}()
		for running := true; running; {
			select {
			case <-done:
				running = false
			default:
			}
			schema, batches, err := db.EventBatchesAfter(ctx, c.ID, 0, eventBudget)
			require.NoError(t, err)
			// Batch k added field f<k-1>, so a read holding it needs k fields.
			if len(batches) > 0 {
				require.GreaterOrEqual(t, schema.Len(), int(batches[len(batches)-1].Seq), "schema %v for seq %d", fieldNames(schema), batches[len(batches)-1].Seq)
			}
		}
	})

	// A sync publishes its snapshot before it stores its cursor; a full
	// read that saw the new cursor with the old snapshot would skip the
	// batches between them.
	t.Run("never returns a cursor newer than its snapshot", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "cursor")
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("ES-cursor-sync"), ConnectorID: c.ID, WorkspaceID: workspaceID}))
		claimed, err := db.ClaimConnectorSync(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, claimed)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for seq := int64(1); seq <= 30; seq++ {
				table := &model.ConnectorTable{
					ID: id26("ES-cursor-table"), ConnectorID: c.ID, WorkspaceID: workspaceID, TableName: "events",
					Columns: json.RawMessage(`[{"name":"id"}]`), PrimaryKey: json.RawMessage(`["id"]`), ForeignKeys: json.RawMessage(`[]`),
					S3Key: fmt.Sprintf("ws/snapshot-%03d", seq), RowCount: seq, SyncID: claimed.ID, SyncedAt: time.Now(),
				}
				if !assert.NoError(t, db.UpsertConnectorTable(ctx, table, nil, "w1")) {
					return
				}
				if !assert.NoError(t, db.UpdateConnectorCursor(ctx, c.ID, json.RawMessage(fmt.Sprintf(`{"seq":%d}`, seq)), claimed.ID, "w1")) {
					return
				}
			}
		}()
		for running := true; running; {
			select {
			case <-done:
				running = false
			default:
			}
			_, cursor, key, err := db.EventTable(ctx, c.ID)
			require.NoError(t, err)
			if cursor > 0 {
				require.GreaterOrEqual(t, key, fmt.Sprintf("ws/snapshot-%03d", cursor), "cursor %d read with snapshot %q", cursor, key)
			}
		}
	})
}

func TestSumFoundryEventBatches(t *testing.T) {
	// The daily caps are seeded from what the workspace sent since 00:00
	// UTC: other workspaces' sends and earlier days' never count.
	t.Run("sums the workspace's batches since a time", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		workspaceID := id26("ES-SUM-WS")
		c := eventSource(t, db, workspaceID, "sum")
		other := eventSource(t, db, id26("ES-SUM-OTHER"), "sum-other")
		old := sent(t, db, eventSend(t, c, "old", `{}`))
		_, err := testDB.Exec(`UPDATE foundry_event_batches SET created_at = NOW(6) - INTERVAL 2 DAY WHERE id=?`, old.Batch.ID)
		require.NoError(t, err)
		a := sent(t, db, eventSend(t, c, "a", `{}`, `{}`))
		b := sent(t, db, eventSend(t, c, "b", `{"amount":1}`))
		sent(t, db, eventSend(t, other, "x", `{}`))

		totals, err := db.SumFoundryEventBatches(ctx, workspaceID, time.Now().Add(-24*time.Hour))
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEventTotals{Pieces: 3, Bytes: a.Batch.Bytes + b.Batch.Bytes}, totals)
		none, err := db.SumFoundryEventBatches(ctx, id26("ES-SUM-NONE"), time.Now().Add(-24*time.Hour))
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEventTotals{}, none)
	})

	// The seed runs once per workspace per process per day; without the day
	// index it would scan every workspace's batches.
	t.Run("reads through the day index", func(t *testing.T) {
		db := newTestStore(t)
		for w := range 20 {
			c := eventSource(t, db, id26(fmt.Sprintf("ES-DAY-%02d", w)), fmt.Sprintf("day%02d", w))
			for i := range 10 {
				sent(t, db, eventSend(t, c, fmt.Sprint("p", i), `{}`))
			}
		}
		_, err := testDB.Exec(`ANALYZE TABLE foundry_event_batches`)
		require.NoError(t, err)
		assert.Equal(t, "foundry_event_batches_day_idx", explainedKey(t, `SELECT COALESCE(SUM(events), 0), COALESCE(SUM(bytes), 0)
FROM foundry_event_batches WHERE workspace_id=? AND created_at >= ?`, id26("ES-DAY-03"), time.Now().Add(-time.Hour).UTC()))
	})
}

// explainedKey is the index MySQL's plan reads the query's first table through.
func explainedKey(t *testing.T, query string, args ...any) string {
	t.Helper()
	rows, err := testDB.Query("EXPLAIN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	columns, err := rows.Columns()
	require.NoError(t, err)
	require.True(t, rows.Next())
	values := make([]sql.NullString, len(columns))
	dest := make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	require.NoError(t, rows.Scan(dest...))
	return values[slices.Index(columns, "key")].String
}

func TestFoundryEventSourcePurge(t *testing.T) {
	workspaceID := id26("ES-PURGE-WS")
	grace := time.Hour

	// purgeFixture is a source deleted past the grace with two batches, a
	// token and a table; one deleted before it that never held a batch, a
	// token or a table, whose folder may still hold an orphan; one deleted
	// within the grace; and a live one.
	type purgeFixture struct {
		db                      store.Store
		old, bare, recent, live *model.Connector
	}
	newPurgeFixture := func(t *testing.T) purgeFixture {
		t.Helper()
		ctx := context.Background()
		db := newTestStore(t)
		f := purgeFixture{
			db: db, old: eventSource(t, db, workspaceID, "old"), bare: eventSource(t, db, workspaceID, "bare"),
			recent: eventSource(t, db, workspaceID, "recent"), live: eventSource(t, db, workspaceID, "live"),
		}
		for _, c := range []*model.Connector{f.old, f.old, f.recent, f.live} {
			sent(t, db, eventSend(t, c, id.New(), `{"amount":1}`))
		}
		eventTable(t, f.old, "ws/old-snapshot", 2, "amount")
		for _, c := range []*model.Connector{f.old, f.live} {
			require.NoError(t, db.CreateFoundryEventSourceToken(ctx, &model.FoundryEventSourceToken{
				ID: id26("ET-" + c.Name), WorkspaceID: workspaceID, ConnectorID: c.ID, Hash: fmt.Sprintf("%064x", []byte(c.Name)),
				Prefix: "hx_ev_000001", CreatedBy: id26("USER"),
			}, 10))
		}
		for c, ago := range map[*model.Connector]string{f.old: "2 HOUR", f.bare: "3 HOUR", f.recent: "30 MINUTE"} {
			require.NoError(t, db.DeleteConnector(ctx, workspaceID, c.ID))
			_, err := testDB.Exec(`UPDATE connectors SET deleted_at = NOW(6) - INTERVAL `+ago+` WHERE id=?`, c.ID)
			require.NoError(t, err)
		}
		return f
	}
	connectorsOf := func(batches []*model.FoundryEventBatch) []string {
		var out []string
		for _, b := range batches {
			out = append(out, b.ConnectorID)
		}
		return out
	}
	tokensOf := func(t *testing.T, c *model.Connector) int {
		t.Helper()
		var n int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_event_source_tokens WHERE connector_id=?`, c.ID).Scan(&n))
		return n
	}

	// A source deleted moments ago may still be read by a sync or restored
	// by hand; only one past the grace is purged.
	t.Run("lists the batches of sources past the grace", func(t *testing.T) {
		ctx := context.Background()
		f := newPurgeFixture(t)
		batches, err := f.db.ListFoundryEventBatchPurges(ctx, time.Now().Add(-grace), 500)
		require.NoError(t, err)
		assert.Equal(t, []string{f.old.ID, f.old.ID}, connectorsOf(batches))
		limited, err := f.db.ListFoundryEventBatchPurges(ctx, time.Now().Add(-grace), 1)
		require.NoError(t, err)
		assert.Len(t, limited, 1)
	})

	// Deleting a batch row loses its pieces for good, so only a purged
	// source's batch is ever deleted, whatever id the caller passes.
	t.Run("deletes only a purged source's batch", func(t *testing.T) {
		ctx := context.Background()
		f := newPurgeFixture(t)
		for _, c := range []*model.Connector{f.live, f.recent} {
			var batchID string
			require.NoError(t, testDB.QueryRow(`SELECT id FROM foundry_event_batches WHERE connector_id=?`, c.ID).Scan(&batchID))
			require.NoError(t, f.db.DeleteFoundryEventBatch(ctx, batchID, time.Now().Add(-grace)))
			assert.Equal(t, 1, eventBatchCount(t, c), "%s keeps its batch", c.Name)
		}
		batches, err := f.db.ListFoundryEventBatchPurges(ctx, time.Now().Add(-grace), 500)
		require.NoError(t, err)
		for _, b := range batches {
			require.NoError(t, f.db.DeleteFoundryEventBatch(ctx, b.ID, time.Now().Add(-grace)))
			require.NoError(t, f.db.DeleteFoundryEventBatch(ctx, b.ID, time.Now().Add(-grace)), "a second delete is no error")
		}
		assert.Equal(t, 0, eventBatchCount(t, f.old))
	})

	// Nothing stored says a deleted source's folder is empty, and an orphan
	// of a send whose commit failed may be in the folder of a source that
	// never held a batch or a token, so the purge goes through every
	// deleted source in the order it was deleted, a cursor saying where it
	// stopped; one deleted later sorts after every one it has passed.
	t.Run("lists every source past the grace in the order it was deleted, from a cursor", func(t *testing.T) {
		ctx := context.Background()
		f := newPurgeFixture(t)
		before := time.Now().Add(-grace)
		all, err := f.db.ListFoundryEventSourcePurges(ctx, before, model.FoundryEventSourcePurge{}, 100)
		require.NoError(t, err)
		require.Len(t, all, 2)
		assert.Equal(t, []string{f.bare.ID, f.old.ID}, []string{all[0].ConnectorID, all[1].ConnectorID})
		assert.False(t, all[0].Batches, "the bare source holds no batch")
		assert.True(t, all[1].Batches, "the old source still holds two")
		assert.Equal(t, workspaceID, all[0].WorkspaceID)
		assert.WithinDuration(t, time.Now().Add(-3*time.Hour), all[0].DeletedAt, time.Minute)

		next, err := f.db.ListFoundryEventSourcePurges(ctx, before, all[0], 100)
		require.NoError(t, err)
		assert.Equal(t, all[1:], next, "a cursor goes on after the source it names")
		last, err := f.db.ListFoundryEventSourcePurges(ctx, before, all[1], 100)
		require.NoError(t, err)
		assert.Empty(t, last)
		first, err := f.db.ListFoundryEventSourcePurges(ctx, before, model.FoundryEventSourcePurge{}, 1)
		require.NoError(t, err)
		assert.Equal(t, all[:1], first)

		// Two sources deleted at the same instant are told apart by id.
		_, err = testDB.Exec(`UPDATE connectors SET deleted_at=? WHERE id=?`, all[0].DeletedAt, f.old.ID)
		require.NoError(t, err)
		tied, err := f.db.ListFoundryEventSourcePurges(ctx, before, model.FoundryEventSourcePurge{}, 100)
		require.NoError(t, err)
		require.Len(t, tied, 2)
		lower, higher := min(f.bare.ID, f.old.ID), max(f.bare.ID, f.old.ID)
		assert.Equal(t, []string{lower, higher}, []string{tied[0].ConnectorID, tied[1].ConnectorID})
		after, err := f.db.ListFoundryEventSourcePurges(ctx, before, tied[0], 100)
		require.NoError(t, err)
		assert.Equal(t, tied[1:], after)
	})

	// Once its batches and folder are gone a source's tokens go too, and
	// only a source past the grace loses its tokens, whatever the caller
	// passes.
	t.Run("deletes the tokens of a source past the grace as it finishes", func(t *testing.T) {
		ctx := context.Background()
		f := newPurgeFixture(t)
		before := time.Now().Add(-grace)
		for _, c := range []*model.Connector{f.old, f.bare, f.live} {
			require.NoError(t, f.db.FinishFoundryEventSourcePurge(ctx, model.FoundryEventSourcePurge{WorkspaceID: workspaceID, ConnectorID: c.ID}, before))
		}
		assert.Equal(t, 0, tokensOf(t, f.old))
		assert.Equal(t, 1, tokensOf(t, f.live), "a live source keeps its token")
		require.NoError(t, f.db.FinishFoundryEventSourcePurge(ctx, model.FoundryEventSourcePurge{WorkspaceID: workspaceID, ConnectorID: f.old.ID}, before),
			"finishing again is no error")
	})

	// The delete retires a source's table as it does every connector's. A
	// load that began before it may still be reading the last snapshot: the
	// snapshot is pinned while that load's job is live, so the sweep keeps
	// it, and once the job ends it waits out the grace before the sweep may
	// delete it.
	t.Run("keeps a deleted source's last snapshot while a live load pins it", func(t *testing.T) {
		ctx := context.Background()
		f := newPurgeFixture(t)
		loads := foundryFixture{db: f.db, ontology: sampleOntology(t, f.db, workspaceID, "es-pin")}
		job := loads.claimedLoad(t, "es-pin")
		require.NoError(t, f.db.SetFoundryGraphJobSources(ctx, job.ID, "graph",
			[]model.FoundryLoadSource{{ConnectorID: f.old.ID, Table: "events", S3Key: "ws/old-snapshot"}}))
		tables, err := f.db.ListConnectorTables(ctx, workspaceID, f.old.ID)
		require.NoError(t, err)
		assert.Empty(t, tables, "the delete took its table")

		retired, err := f.db.ListReplacedSnapshots(ctx, time.Now().Add(time.Minute), 10)
		require.NoError(t, err)
		require.Len(t, retired, 1)
		assert.Equal(t, "ws/old-snapshot", retired[0].S3Key)
		pinned, err := f.db.ListPinnedSnapshotKeys(ctx)
		require.NoError(t, err)
		assert.Contains(t, pinned, retired[0].S3Key, "the sweep skips a pinned snapshot")

		loads.ended(t, "graph", job.ID, model.FoundryGraphJobSucceeded, next("es-pin"))
		pinned, err = f.db.ListPinnedSnapshotKeys(ctx)
		require.NoError(t, err)
		assert.NotContains(t, pinned, retired[0].S3Key)
		due, err := f.db.ListReplacedSnapshots(ctx, time.Now().Add(-grace), 10)
		require.NoError(t, err)
		assert.Empty(t, due, "a snapshot retired moments ago waits out the grace")
	})
}

func TestDeleteExpiredEventSyncs(t *testing.T) {
	workspaceID := id26("ES-PRUNE-WS")
	day := 24 * time.Hour
	retention := 30 * day
	// storeSync stores sync ES-SYNC-<tag> of c, created a minute before it
	// finished, finishedAgo ago.
	storeSync := func(t *testing.T, c *model.Connector, tag string, origin model.ConnectorSyncOrigin, status model.ConnectorSyncStatus,
		finishedAgo time.Duration) {
		t.Helper()
		finished := time.Now().UTC().Add(-finishedAgo)
		_, err := testDB.Exec(`INSERT INTO connector_syncs (id, connector_id, workspace_id, status, origin, created_at, started_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id26("ES-SYNC-"+tag), c.ID, c.WorkspaceID, string(status), string(origin),
			finished.Add(-time.Minute), finished.Add(-time.Minute), finished)
		require.NoError(t, err)
	}
	storedSyncs := func(t *testing.T) []string {
		t.Helper()
		rows, err := testDB.Query(`SELECT id FROM connector_syncs ORDER BY id`)
		require.NoError(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var syncID string
			require.NoError(t, rows.Scan(&syncID))
			out = append(out, syncID)
		}
		require.NoError(t, rows.Err())
		return out
	}

	// A busy source writes up to a sync a minute, so its succeeded syncs go
	// after the change retention. Each connector's newest sync is its last
	// sync on the site and the MCP, so it stays however old; failed syncs
	// stay to say why, and syncs of another origin, or of another kind's
	// connector, are never touched.
	t.Run("deletes event source syncs that succeeded before the bound, each source's newest kept", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		busy := eventSource(t, db, workspaceID, "busy")
		idle := eventSource(t, db, workspaceID, "idle")
		require.NoError(t, db.DeleteConnector(ctx, workspaceID, idle.ID))
		failing := eventSource(t, db, workspaceID, "failing")
		stripe := scheduledConnector(t, db, workspaceID, "prune", nil)
		storeSync(t, busy, "busy-40d", model.ConnectorSyncEvents, model.ConnectorSyncSucceeded, 40*day)
		storeSync(t, busy, "busy-35d", model.ConnectorSyncEvents, model.ConnectorSyncSucceeded, 35*day)
		storeSync(t, busy, "busy-failed", model.ConnectorSyncEvents, model.ConnectorSyncFailed, 38*day)
		storeSync(t, busy, "busy-manual", model.ConnectorSyncManual, model.ConnectorSyncSucceeded, 39*day)
		// Created before the bound, finished after it: not yet past the retention.
		storeSync(t, busy, "busy-straddle", model.ConnectorSyncEvents, model.ConnectorSyncSucceeded, retention-30*time.Second)
		storeSync(t, busy, "busy-1d", model.ConnectorSyncEvents, model.ConnectorSyncSucceeded, day)
		storeSync(t, idle, "idle-40d", model.ConnectorSyncEvents, model.ConnectorSyncSucceeded, 40*day)
		storeSync(t, failing, "failing-40d", model.ConnectorSyncEvents, model.ConnectorSyncSucceeded, 40*day)
		storeSync(t, failing, "failing-35d", model.ConnectorSyncEvents, model.ConnectorSyncFailed, 35*day)
		storeSync(t, stripe, "stripe-40d", model.ConnectorSyncSchedule, model.ConnectorSyncSucceeded, 40*day)
		storeSync(t, stripe, "stripe-1d", model.ConnectorSyncSchedule, model.ConnectorSyncSucceeded, day)
		all := storedSyncs(t)

		n, err := db.DeleteExpiredEventSyncs(ctx, time.Now().Add(-retention), 500)
		require.NoError(t, err)
		assert.Equal(t, 3, n)
		expired := []string{id26("ES-SYNC-busy-40d"), id26("ES-SYNC-busy-35d"), id26("ES-SYNC-failing-40d")}
		assert.Equal(t, slices.DeleteFunc(all, func(s string) bool { return slices.Contains(expired, s) }), storedSyncs(t))
		last, err := db.ListConnectorSyncs(ctx, workspaceID, idle.ID, 1)
		require.NoError(t, err)
		require.Len(t, last, 1)
		assert.Equal(t, id26("ES-SYNC-idle-40d"), last[0].ID, "an idle source keeps its last sync")

		again, err := db.DeleteExpiredEventSyncs(ctx, time.Now().Add(-retention), 500)
		require.NoError(t, err)
		assert.Zero(t, again)
	})

	// One sweep tick deletes a bounded number, and the next goes on.
	t.Run("deletes at most limit a call", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := eventSource(t, db, workspaceID, "limit")
		for i := range 3 {
			storeSync(t, c, fmt.Sprint("old-", i), model.ConnectorSyncEvents, model.ConnectorSyncSucceeded, 40*day+time.Duration(i)*time.Hour)
		}
		storeSync(t, c, "new", model.ConnectorSyncEvents, model.ConnectorSyncSucceeded, day)
		var deleted []int
		for range 3 {
			n, err := db.DeleteExpiredEventSyncs(ctx, time.Now().Add(-retention), 2)
			require.NoError(t, err)
			deleted = append(deleted, n)
		}
		assert.Equal(t, []int{2, 1, 0}, deleted)
		assert.Equal(t, []string{id26("ES-SYNC-new")}, storedSyncs(t))
	})
}
