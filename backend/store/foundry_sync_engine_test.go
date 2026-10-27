package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
)

// scheduledConnector stores a connector whose next sync is at next.
func scheduledConnector(t *testing.T, db store.Store, workspaceID, tag string, next *time.Time) *model.Connector {
	t.Helper()
	c := &model.Connector{
		ID: id26("SC-" + tag), WorkspaceID: workspaceID, Kind: "stripe", Name: "Stripe " + tag, Config: json.RawMessage(`{}`),
		SecretCiphertext: []byte("sealed"), SecretKeyID: "k1", SecretHint: "sk_test…1234", NextSyncAt: next,
	}
	require.NoError(t, db.CreateConnector(context.Background(), c))
	return c
}

func TestConnectorSchedules(t *testing.T) {
	workspaceID := id26("SCHED-WS")
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)

	// Two sync worker replicas tick at once; each due connector must be queued once.
	t.Run("queues each due connector once across two schedulers", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		due := scheduledConnector(t, db, workspaceID, "due", &past)
		busy := scheduledConnector(t, db, workspaceID, "busy", &past)
		scheduledConnector(t, db, workspaceID, "later", &future)
		gone := scheduledConnector(t, db, workspaceID, "gone", &past)
		require.NoError(t, db.DeleteConnector(ctx, workspaceID, gone.ID))
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("SC-busy-sync"), ConnectorID: busy.ID, WorkspaceID: workspaceID}))

		var wg sync.WaitGroup
		queued := make([]int, 2)
		for i := range queued {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n, err := db.QueueDueConnectorSyncs(ctx, 10, time.Minute)
				assert.NoError(t, err)
				queued[i] = n
			}()
		}
		wg.Wait()
		assert.Equal(t, 1, queued[0]+queued[1], "only the due connector without a live sync is queued")
		active, err := db.GetActiveConnectorSync(ctx, workspaceID, due.ID)
		require.NoError(t, err)
		assert.Equal(t, model.ConnectorSyncSchedule, active.Origin)
		for _, c := range []*model.Connector{due, busy} {
			stored, err := db.GetConnector(ctx, workspaceID, c.ID)
			require.NoError(t, err)
			assert.Nil(t, stored.NextSyncAt, "a queued connector waits for its sync to end to be scheduled again")
		}
		later, err := db.GetConnector(ctx, workspaceID, id26("SC-later"))
		require.NoError(t, err)
		assert.NotNil(t, later.NextSyncAt)
	})

	// The schedule moves only when a sync ends, so a long or retried sync never overlaps the next.
	t.Run("sets the next sync or the pause as a sync ends", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := scheduledConnector(t, db, workspaceID, "c", nil)
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("SC-s1"), ConnectorID: c.ID, WorkspaceID: workspaceID}))
		claimed, err := db.ClaimConnectorSync(ctx, "w1", time.Minute)
		require.NoError(t, err)
		rows := int64(1)
		next := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		require.NoError(t, db.FinishConnectorSync(ctx, claimed.ID, "w1", model.ConnectorSyncOutcome{
			Status: model.ConnectorSyncSucceeded, RowsSynced: &rows, Schedule: &model.ConnectorScheduleOutcome{NextSyncAt: &next},
		}))
		stored, err := db.GetConnector(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		require.NotNil(t, stored.NextSyncAt)
		assert.WithinDuration(t, next, *stored.NextSyncAt, time.Millisecond)

		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("SC-s2"), ConnectorID: c.ID, WorkspaceID: workspaceID}))
		claimed, err = db.ClaimConnectorSync(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.NoError(t, db.FinishConnectorSync(ctx, claimed.ID, "w1", model.ConnectorSyncOutcome{
			Status: model.ConnectorSyncFailed, Error: "Stripe rejected this key", Schedule: &model.ConnectorScheduleOutcome{Paused: "Stripe rejected this key"},
		}))
		stored, err = db.GetConnector(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		assert.Nil(t, stored.NextSyncAt)
		require.NotNil(t, stored.SchedulePaused)
		assert.Equal(t, "Stripe rejected this key", *stored.SchedulePaused)

		// A new choice of interval resumes the schedule.
		resume := time.Now().Add(time.Hour)
		require.NoError(t, db.UpdateConnector(ctx, workspaceID, c.ID, model.ConnectorUpdate{Schedule: &model.ConnectorScheduleUpdate{NextSyncAt: &resume}}))
		stored, err = db.GetConnector(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		assert.Nil(t, stored.SchedulePaused)
		assert.NotNil(t, stored.NextSyncAt)
	})

	// An owner who turned automatic syncs off while a sync ran keeps them off.
	t.Run("leaves a manual-only connector unscheduled", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := scheduledConnector(t, db, workspaceID, "m", nil)
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("SC-m1"), ConnectorID: c.ID, WorkspaceID: workspaceID}))
		manual := 0
		require.NoError(t, db.UpdateConnector(ctx, workspaceID, c.ID, model.ConnectorUpdate{Schedule: &model.ConnectorScheduleUpdate{SyncEverySeconds: &manual}}))
		claimed, err := db.ClaimConnectorSync(ctx, "w1", time.Minute)
		require.NoError(t, err)
		next := time.Now().Add(time.Hour)
		require.NoError(t, db.FinishConnectorSync(ctx, claimed.ID, "w1", model.ConnectorSyncOutcome{
			Status: model.ConnectorSyncSucceeded, Schedule: &model.ConnectorScheduleOutcome{NextSyncAt: &next},
		}))
		stored, err := db.GetConnector(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		assert.Nil(t, stored.NextSyncAt)
		require.NotNil(t, stored.SyncEverySeconds)
		assert.Equal(t, 0, *stored.SyncEverySeconds)
	})

	// A queued change read that ran in full is recorded as full.
	t.Run("records the mode a sync ran with", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := scheduledConnector(t, db, workspaceID, "md", nil)
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("SC-md1"), ConnectorID: c.ID, WorkspaceID: workspaceID,
			Mode: model.ConnectorSyncChanges}))
		claimed, err := db.ClaimConnectorSync(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.NoError(t, db.FinishConnectorSync(ctx, claimed.ID, "w1", model.ConnectorSyncOutcome{
			Status: model.ConnectorSyncSucceeded, Mode: model.ConnectorSyncFull,
		}))
		syncs, err := db.ListConnectorSyncs(ctx, workspaceID, c.ID, 1)
		require.NoError(t, err)
		require.Len(t, syncs, 1)
		assert.Equal(t, model.ConnectorSyncFull, syncs[0].Mode)
	})

	// The sync worked its next time out from the interval it started with; an
	// owner who turned automatic syncs on meanwhile already scheduled one.
	t.Run("keeps a schedule the owner changed during the sync", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		manual := 0
		c := scheduledConnector(t, db, workspaceID, "e", nil)
		require.NoError(t, db.UpdateConnector(ctx, workspaceID, c.ID, model.ConnectorUpdate{Schedule: &model.ConnectorScheduleUpdate{SyncEverySeconds: &manual}}))
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("SC-e1"), ConnectorID: c.ID, WorkspaceID: workspaceID}))
		claimed, err := db.ClaimConnectorSync(ctx, "w1", time.Minute)
		require.NoError(t, err)
		owners := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		require.NoError(t, db.UpdateConnector(ctx, workspaceID, c.ID, model.ConnectorUpdate{Schedule: &model.ConnectorScheduleUpdate{NextSyncAt: &owners}}))
		require.NoError(t, db.FinishConnectorSync(ctx, claimed.ID, "w1", model.ConnectorSyncOutcome{
			Status: model.ConnectorSyncSucceeded, Schedule: &model.ConnectorScheduleOutcome{Every: &manual},
		}))
		stored, err := db.GetConnector(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		require.NotNil(t, stored.NextSyncAt, "the outcome of a manual-only start erased the owner's schedule")
		assert.WithinDuration(t, owners, *stored.NextSyncAt, time.Millisecond)
	})

	// A cursor belongs to the settings it was read with, and only the sync's owner may move it.
	t.Run("keeps the change cursor behind the lease and drops it with new settings", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := scheduledConnector(t, db, workspaceID, "cur", nil)
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("SC-cur1"), ConnectorID: c.ID, WorkspaceID: workspaceID}))
		claimed, err := db.ClaimConnectorSync(ctx, "w1", time.Minute)
		require.NoError(t, err)
		assert.ErrorIs(t, db.UpdateConnectorCursor(ctx, c.ID, json.RawMessage(`{"event":"evt_1"}`), claimed.ID, "w2"), store.ErrNotFound)
		require.NoError(t, db.UpdateConnectorCursor(ctx, c.ID, json.RawMessage(`{"event":"evt_1"}`), claimed.ID, "w1"))
		stored, err := db.GetConnector(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		assert.JSONEq(t, `{"event":"evt_1"}`, string(stored.ChangeCursor))

		require.NoError(t, db.UpdateConnector(ctx, workspaceID, c.ID, model.ConnectorUpdate{
			Config: json.RawMessage(`{"other":true}`), SecretCiphertext: []byte("x"), SecretKeyID: "k1", SecretHint: "h",
		}))
		stored, err = db.GetConnector(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		assert.Nil(t, stored.ChangeCursor)
	})
}

// cdcConnector stores a postgres connector whose last sync left cursor, and
// ends that sync with outcome.
func cdcConnector(t *testing.T, db store.Store, workspaceID, tag, kind, cursor string, outcome model.ConnectorSyncOutcome) *model.Connector {
	t.Helper()
	ctx := context.Background()
	c := &model.Connector{
		ID: id26("CDC-" + tag), WorkspaceID: workspaceID, Kind: kind, Name: "CDC " + tag, Config: json.RawMessage(`{}`),
		SecretCiphertext: []byte("sealed"), SecretKeyID: "k1", SecretHint: "h",
	}
	require.NoError(t, db.CreateConnector(ctx, c))
	require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("CDCS-" + tag), ConnectorID: c.ID, WorkspaceID: workspaceID}))
	claimed, err := db.ClaimConnectorSync(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Equal(t, c.ID, claimed.ConnectorID)
	require.NoError(t, db.UpdateConnectorCursor(ctx, c.ID, json.RawMessage(cursor), claimed.ID, "w1"))
	require.NoError(t, db.FinishConnectorSync(ctx, claimed.ID, "w1", outcome))
	return c
}

func TestStrandedConnectorCursors(t *testing.T) {
	ctx := context.Background()
	rows := int64(1)
	succeeded := model.ConnectorSyncOutcome{Status: model.ConnectorSyncSucceeded, RowsSynced: &rows}
	paused := model.ConnectorSyncOutcome{Status: model.ConnectorSyncFailed, Error: "password rejected",
		Schedule: &model.ConnectorScheduleOutcome{Paused: "password rejected"}}
	slot := `{"slot":"helix_foundry_x","lsn":"0/16B3748"}`

	// A slot nobody reads keeps WAL on the customer's disk, so every connector
	// no sync will read again is listed, and only those.
	t.Run("lists deleted, paused and orphaned connectors of the given kinds", func(t *testing.T) {
		db := newTestStore(t)
		live, gone := id26("STR-LIVE"), id26("STR-GONE")
		createWorkspace(t, live)
		createWorkspace(t, gone)
		setWorkspaceStatus(t, gone, "deleting")

		cdcConnector(t, db, live, "running", "postgres", slot, succeeded)
		deleted := cdcConnector(t, db, live, "deleted", "postgres", slot, succeeded)
		require.NoError(t, db.DeleteConnector(ctx, live, deleted.ID))
		pausedC := cdcConnector(t, db, live, "paused", "postgres", slot, paused)
		orphan := cdcConnector(t, db, gone, "orphan", "postgres", slot, succeeded)
		stripe := cdcConnector(t, db, live, "stripe", "stripe", `{"event":"evt_1"}`, succeeded)
		require.NoError(t, db.DeleteConnector(ctx, live, stripe.ID))
		resumed := cdcConnector(t, db, live, "resumed", "postgres", slot, paused)
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("CDCS-resumed2"), ConnectorID: resumed.ID, WorkspaceID: live}))

		stranded, err := db.ListStrandedConnectorCursors(ctx, []string{"postgres"}, 10)
		require.NoError(t, err)
		var ids []string
		for _, c := range stranded {
			ids = append(ids, c.ID)
			assert.JSONEq(t, slot, string(c.ChangeCursor))
		}
		assert.ElementsMatch(t, []string{deleted.ID, pausedC.ID, orphan.ID}, ids,
			"a running connector, another kind and one with a live sync are left alone")
	})

	// The cursor goes only once its slot is gone; a sync that replaced it, or
	// is reading it, keeps its own.
	t.Run("clears only the released cursor while no sync is live", func(t *testing.T) {
		db := newTestStore(t)
		ws := id26("STR-CLR")
		createWorkspace(t, ws)
		c := cdcConnector(t, db, ws, "clear", "postgres", slot, paused)
		cursor := func() json.RawMessage {
			stored, err := db.ListConnectors(ctx, ws)
			require.NoError(t, err)
			return stored[0].ChangeCursor
		}

		require.NoError(t, db.ClearConnectorCursor(ctx, c.ID, json.RawMessage(`{"slot":"helix_foundry_other","lsn":"0/1"}`)))
		assert.NotNil(t, cursor(), "a cursor a sync has since replaced stays")

		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("CDCS-clear2"), ConnectorID: c.ID, WorkspaceID: ws}))
		require.NoError(t, db.ClearConnectorCursor(ctx, c.ID, json.RawMessage(slot)))
		assert.NotNil(t, cursor(), "a live sync owns the cursor")

		claimed, err := db.ClaimConnectorSync(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.NoError(t, db.FinishConnectorSync(ctx, claimed.ID, "w1", paused))
		require.NoError(t, db.ClearConnectorCursor(ctx, c.ID, json.RawMessage(`{"lsn": "0/16B3748", "slot": "helix_foundry_x"}`)))
		assert.Nil(t, cursor(), "the same JSON value clears it, whatever its spacing or key order")
	})
}

func digestOf(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func TestConnectorChangeBatches(t *testing.T) {
	workspaceID, connectorID := id26("CB-WS"), id26("CB-CONN")
	// store is an empty store holding the connector.
	newStore := func(t *testing.T) store.Store {
		t.Helper()
		db := newTestStore(t)
		connectorRow(t, db, workspaceID, connectorID)
		return db
	}
	claim := func(t *testing.T, db store.Store, suffix string) *model.ConnectorSync {
		t.Helper()
		ctx := context.Background()
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("CB-" + suffix), ConnectorID: connectorID, WorkspaceID: workspaceID}))
		sync, err := db.ClaimConnectorSync(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, sync)
		return sync
	}
	finish := func(t *testing.T, db store.Store, sync *model.ConnectorSync) {
		t.Helper()
		require.NoError(t, db.FinishConnectorSync(context.Background(), sync.ID, "w1", model.ConnectorSyncOutcome{Status: model.ConnectorSyncSucceeded}))
	}
	table := func(sync *model.ConnectorSync, key string) *model.ConnectorTable {
		return &model.ConnectorTable{
			ID: id26("CB-T-" + key), ConnectorID: connectorID, WorkspaceID: workspaceID, TableName: "customers",
			Columns: json.RawMessage(`[]`), PrimaryKey: json.RawMessage(`["customer_id"]`), ForeignKeys: json.RawMessage(`[]`),
			S3Key: "ws/customers/" + key, RowCount: 2, SyncID: sync.ID, SyncedAt: time.Now(), ContentDigest: digestOf(key),
		}
	}
	batch := func(sync *model.ConnectorSync, tag string) *model.ConnectorChangeBatch {
		key := "ws/customers/changes/" + tag
		return &model.ConnectorChangeBatch{ID: id26("CB-B-" + tag), WorkspaceID: workspaceID, ConnectorID: connectorID,
			TableName: "customers", SyncID: sync.ID, Kind: model.ConnectorChangesBatch, S3Key: &key, Inserts: 1, Updates: 2, Deletes: 3}
	}

	// An engine applying changes reads a table's batches in order, so each sync's batch takes the next number.
	t.Run("numbers each table's batches in order with its snapshot", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		first := claim(t, db, "s1")
		require.NoError(t, db.UpsertConnectorTable(ctx, table(first, "a"), batch(first, "1"), "w1"))
		finish(t, db, first)
		second := claim(t, db, "s2")
		require.NoError(t, db.UpsertConnectorTable(ctx, table(second, "b"), batch(second, "2"), "w1"))

		tables, err := db.ListConnectorTables(ctx, workspaceID, connectorID)
		require.NoError(t, err)
		require.Len(t, tables, 1)
		assert.Equal(t, int64(2), tables[0].ChangeSeq)
		assert.Equal(t, digestOf("b"), tables[0].ContentDigest)
		batches, err := db.ListConnectorChangeBatches(ctx, workspaceID, second.ID)
		require.NoError(t, err)
		require.Len(t, batches, 1)
		assert.Equal(t, int64(2), batches[0].Seq)
		assert.Equal(t, int64(3), batches[0].Deletes)
		replaced, err := db.ListReplacedSnapshots(ctx, time.Now().Add(time.Minute), 10)
		require.NoError(t, err)
		require.Len(t, replaced, 1, "the replaced snapshot waits for the sweep instead of being deleted")
		assert.Equal(t, "ws/customers/a", replaced[0].S3Key)
	})

	// A retry re-reads a followed table on top of its first attempt's write; what it adds is the next batch.
	t.Run("records a retry's change to a table as the next batch", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		sync := claim(t, db, "s1")
		require.NoError(t, db.UpsertConnectorTable(ctx, table(sync, "a"), batch(sync, "1"), "w1"))
		require.NoError(t, db.UpsertConnectorTable(ctx, table(sync, "a2"), batch(sync, "1b"), "w1"))
		tables, err := db.ListConnectorTables(ctx, workspaceID, connectorID)
		require.NoError(t, err)
		assert.Equal(t, "ws/customers/a2", tables[0].S3Key)
		assert.Equal(t, int64(2), tables[0].ChangeSeq)
		batches, err := db.ListConnectorChangeBatches(ctx, workspaceID, sync.ID)
		require.NoError(t, err)
		assert.Len(t, batches, 2)
	})

	// An unchanged table keeps its snapshot, so a pinned load and the graph are left alone.
	t.Run("touches an unchanged table without moving its snapshot", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		first := claim(t, db, "s1")
		require.NoError(t, db.UpsertConnectorTable(ctx, table(first, "a"), nil, "w1"))
		finish(t, db, first)
		second := claim(t, db, "s2")
		require.NoError(t, db.TouchConnectorTable(ctx, connectorID, "customers", second.ID, true, "w1"))
		assert.ErrorIs(t, db.TouchConnectorTable(ctx, connectorID, "customers", second.ID, true, "w2"), store.ErrNotFound)
		tables, err := db.ListConnectorTables(ctx, workspaceID, connectorID)
		require.NoError(t, err)
		assert.Equal(t, "ws/customers/a", tables[0].S3Key)
		assert.Equal(t, second.ID, tables[0].SyncID)
		assert.NotNil(t, tables[0].ReconciledAt)
		replaced, err := db.ListReplacedSnapshots(ctx, time.Now().Add(time.Minute), 10)
		require.NoError(t, err)
		assert.Empty(t, replaced)
	})

	// A dropped table keeps its batches until they expire; choosing it again
	// must not reuse their numbers, or every sync of it fails on the unique key.
	t.Run("numbers a table chosen again after the batches it left", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		first := claim(t, db, "r1")
		require.NoError(t, db.UpsertConnectorTable(ctx, table(first, "r1"), batch(first, "r1"), "w1"))
		dropped, err := db.PruneConnectorTables(ctx, workspaceID, connectorID, nil, first.ID, "w1")
		require.NoError(t, err)
		require.Equal(t, []string{"customers"}, dropped)
		finish(t, db, first)

		again := claim(t, db, "r2")
		b := batch(again, "r2")
		require.NoError(t, db.UpsertConnectorTable(ctx, table(again, "r2"), b, "w1"))
		assert.Equal(t, int64(3), b.Seq, "after the first batch and the drop")
	})
	t.Run("lists batches past their retention and forgets them", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		sync := claim(t, db, "s1")
		require.NoError(t, db.UpsertConnectorTable(ctx, table(sync, "a"), batch(sync, "1"), "w1"))
		none, err := db.ListExpiredChangeBatches(ctx, time.Now().Add(-time.Hour), 10)
		require.NoError(t, err)
		assert.Empty(t, none)
		expired, err := db.ListExpiredChangeBatches(ctx, time.Now().Add(time.Minute), 10)
		require.NoError(t, err)
		require.Len(t, expired, 1)
		require.NoError(t, db.DeleteConnectorChangeBatch(ctx, expired[0].ID))
		left, err := db.ListConnectorChangeBatches(ctx, workspaceID, sync.ID)
		require.NoError(t, err)
		assert.Empty(t, left)
	})

	// Nothing reads a deleted connector's batches again, so the sweep takes
	// them at its next tick instead of after the retention.
	t.Run("lists a deleted connector's batches whatever their age", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		live := id26("CB-LIVE")
		connectorRow(t, db, workspaceID, live)
		seq := 0
		put := func(tag, connector string, age time.Duration) string {
			t.Helper()
			seq++
			_, err := testDB.Exec(`INSERT INTO connector_change_batches (id, workspace_id, connector_id, table_name, seq, sync_id, kind,
  inserts, updates, deletes, created_at) VALUES (?, ?, ?, 'customers', ?, ?, 'changes', 0, 0, 0, NOW(6) - INTERVAL ? MICROSECOND)`,
				id26("CB-B-"+tag), workspaceID, connector, seq, id26("CB-S"), age.Microseconds())
			require.NoError(t, err)
			return id26("CB-B-" + tag)
		}
		goneOld, goneNew := put("gone-old", connectorID, 3*time.Hour), put("gone-new", connectorID, 0)
		liveOld := put("live-old", live, 2*time.Hour)
		put("live-new", live, 0)
		require.NoError(t, db.DeleteConnector(ctx, workspaceID, connectorID))
		listed := func(limit int) []string {
			t.Helper()
			batches, err := db.ListExpiredChangeBatches(ctx, time.Now().Add(-time.Hour), limit)
			require.NoError(t, err)
			var ids []string
			for _, b := range batches {
				ids = append(ids, b.ID)
			}
			return ids
		}
		assert.Equal(t, []string{goneOld, liveOld, goneNew}, listed(10), "oldest first, each once; a live connector's fresh one stays")
		assert.Equal(t, []string{goneOld, liveOld}, listed(2))
	})
}

func TestFoundryLoadPins(t *testing.T) {
	// The sweep keeps every snapshot a live load reads; one whose job ended is free to go.
	t.Run("lists the snapshots live load jobs pinned", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "PIN1")
		loadID := f.load(t, "L1")
		job := f.claimed(t, "w1", loadID)
		// Each pin keeps the sync that observed its snapshot, to the microsecond, through the JSON column.
		pins := []model.FoundryLoadSource{
			{ConnectorID: "c1", Table: "customers", S3Key: "ws/c1/customers/a.jsonl.gz",
				SyncID: id26("PS1"), SyncedAt: time.Date(2026, 10, 20, 8, 0, 0, 123456000, time.UTC), ChangeSeq: 42},
			{ConnectorID: "c1", Table: "refunds", S3Key: "ws/c1/refunds/a.jsonl.gz",
				SyncID: id26("PS1"), SyncedAt: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)},
		}
		assert.ErrorIs(t, f.db.SetFoundryGraphJobSources(ctx, job.ID, "w2", pins), store.ErrNotFound, "only the lease holder pins")
		require.NoError(t, f.db.SetFoundryGraphJobSources(ctx, job.ID, "w1", pins))
		keys, err := f.db.ListPinnedSnapshotKeys(ctx)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"ws/c1/customers/a.jsonl.gz", "ws/c1/refunds/a.jsonl.gz"}, keys)
		stored, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, job.ID)
		require.NoError(t, err)
		assert.Equal(t, pins, stored.Sources, "a retry reads the pins back")

		f.ended(t, "w1", job.ID, model.FoundryGraphJobSucceeded, next("pin"))
		keys, err = f.db.ListPinnedSnapshotKeys(ctx)
		require.NoError(t, err)
		assert.Empty(t, keys)
	})
}

// refreshFixture is an ontology whose current load reads connector RC-<tag>'s customers table.
func refreshFixture(t *testing.T, tag string) (foundryFixture, string, string) {
	t.Helper()
	ctx := context.Background()
	f := newFoundryFixture(t, tag)
	connectorID, genID := id26("RC-"+tag), id26("RG-"+tag)
	input, err := json.Marshal(model.OntologyGenerationInput{Tables: []model.OntologyGenerationTable{{ConnectorID: connectorID, Table: "customers"}}})
	require.NoError(t, err)
	_, err = testDB.ExecContext(ctx, `INSERT INTO foundry_ontology_generations (id, workspace_id, ontology_id, status, input)
VALUES (?, ?, ?, 'succeeded', ?)`, genID, f.ontology.WorkspaceID, f.ontology.ID, string(input))
	require.NoError(t, err)
	v := &model.OntologyVersion{
		ID: id26("FV-R" + tag), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID, Source: model.OntologySourceGenerated,
		Format: model.OntologyFormatAgentBundle, Definition: json.RawMessage(`{}`), GenerationID: &genID,
		DocumentSetIDs: &[]string{generalSetID(f.ontology.ID)},
	}
	require.NoError(t, f.db.CreateOntologyVersion(ctx, v))
	job := loadJob(f.ontology, "R"+tag)
	job.VersionID = v.ID
	require.NoError(t, f.db.CreateFoundryLoad(ctx, job))
	f.loaded(t, "w-"+tag)
	return f, connectorID, job.LoadID
}

func refreshJob(tag string) *model.FoundryGraphJob {
	return &model.FoundryGraphJob{ID: id26("RJ-" + tag)}
}

func TestFoundryRefresh(t *testing.T) {
	// Only a change to a table the current load reads makes its graph stale.
	t.Run("owes a refresh only for ontologies that read a changed table", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, _ := refreshFixture(t, "OWE")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"charges"}, time.Hour))
		assert.Nil(t, f.ontologyNow(t).RefreshDueAt)
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, id26("RC-other"), []string{"customers"}, time.Hour))
		assert.Nil(t, f.ontologyNow(t).RefreshDueAt)
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Hour))
		due := f.ontologyNow(t).RefreshDueAt
		require.NotNil(t, due)
		assert.WithinDuration(t, time.Now(), *due, 5*time.Second, "a first refresh is due at once")
		listed, err := f.db.ListDueFoundryRefreshes(ctx, time.Time{}, "", 10)
		require.NoError(t, err)
		require.Len(t, listed, 1)
	})

	// A refresh replaces the load it refreshed, and the next replaces it in turn
	// without pushing the customer's own loads out of retention.
	t.Run("makes each refresh current and deletes the refresh it replaced", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, userLoad := refreshFixture(t, "CUR")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		queued, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, refreshJob("1"))
		require.NoError(t, err)
		require.True(t, queued)
		assert.Nil(t, f.ontologyNow(t).RefreshDueAt, "a queued refresh is owed no more")
		first, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, id26("RJ-1"))
		require.NoError(t, err)
		assert.Equal(t, model.FoundryLoadByRefresh, first.Origin)
		require.NotNil(t, first.RefreshesLoadID)
		assert.Equal(t, userLoad, *first.RefreshesLoadID)
		f.loaded(t, "w2")
		assert.Equal(t, first.ID, *f.ontologyNow(t).CurrentLoadID)

		time.Sleep(5 * time.Millisecond)
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		queued, err = f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, refreshJob("2"))
		require.NoError(t, err)
		require.True(t, queued)
		f.loaded(t, "w3")
		assert.Equal(t, id26("RJ-2"), *f.ontologyNow(t).CurrentLoadID)
		assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, first.ID), "the replaced refresh goes at once")
		assert.Equal(t, model.FoundryLoadReady, f.loadStatus(t, userLoad), "the customer's own load is kept")
	})

	// A refresh soon after the last waits for the plan's interval.
	t.Run("spaces refreshes by the plan's interval", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, _ := refreshFixture(t, "GAP")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Hour))
		queued, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, refreshJob("g1"))
		require.NoError(t, err)
		require.True(t, queued)
		f.loaded(t, "w2")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Hour))
		due := f.ontologyNow(t).RefreshDueAt
		require.NotNil(t, due)
		assert.WithinDuration(t, time.Now().Add(time.Hour), *due, 5*time.Second)
		listed, err := f.db.ListDueFoundryRefreshes(ctx, time.Time{}, "", 10)
		require.NoError(t, err)
		assert.Empty(t, listed)
		queued, err = f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, refreshJob("g2"))
		require.NoError(t, err)
		assert.False(t, queued, "a refresh not yet due is not queued")
	})

	// An owed refresh waits for a live job rather than failing. It does not
	// wait for budget: with the month spent it loads the tables and replays
	// its documents at $0.
	t.Run("waits while a job is live, and not for a spent month", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, userLoad := refreshFixture(t, "WAIT")
		_, err := testDB.ExecContext(ctx, `UPDATE foundry_load_jobs SET usd_micros=? WHERE id=?`, int64(1)<<40, userLoad)
		require.NoError(t, err)
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		f.load(t, "busy")
		queued, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, refreshJob("w1"))
		require.NoError(t, err)
		assert.False(t, queued)
		assert.NotNil(t, f.ontologyNow(t).RefreshDueAt, "still owed")

		f.finish(t, "w2", "busy", model.FoundryGraphJobFailed)
		queued, err = f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, refreshJob("w2"))
		require.NoError(t, err)
		assert.True(t, queued, "the month is spent")
		assert.Nil(t, f.ontologyNow(t).RefreshDueAt)
		assert.Equal(t, id26("RJ-w2"), f.liveJob(t).ID)
	})

	// The user waits for a load they started; a refresh does not.
	t.Run("gives way to a load the user starts", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, _ := refreshFixture(t, "PRE")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		queued, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, refreshJob("p1"))
		require.NoError(t, err)
		require.True(t, queued)
		f.claimed(t, "w2", id26("RJ-p1"))

		userLoad := f.load(t, "U1")
		assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, id26("RJ-p1")))
		assert.Equal(t, userLoad, f.liveJob(t).LoadID)
		refresh, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, id26("RJ-p1"))
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobCancelled, refresh.Status)
	})

	// Someone chose another current load while the refresh ran; their choice stands.
	t.Run("leaves a current load someone chose meanwhile", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, firstLoad := refreshFixture(t, "MOVE")
		other := f.load(t, "O1")
		f.loaded(t, "w2")
		_, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, firstLoad, extractJob("move"), noBudget)
		require.NoError(t, err)
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		queued, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, refreshJob("m1"))
		require.NoError(t, err)
		require.True(t, queued)
		_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, other, extractJob("move2"), noBudget)
		require.NoError(t, err)
		f.loaded(t, "w3")
		assert.Equal(t, other, *f.ontologyNow(t).CurrentLoadID)
		assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, id26("RJ-m1")))
	})

	// A refresh waiting in the queue must not delay a load someone is watching.
	t.Run("claims a user's job before a refresh's", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, _ := refreshFixture(t, "ORD")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		queued, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, refreshJob("o1"))
		require.NoError(t, err)
		require.True(t, queued)
		g := foundryFixture{db: f.db, ontology: sampleOntology(t, f.db, id26("FWS-ORD2"), "ORD2")}
		userLoad := g.load(t, "U2")
		job, err := f.db.ClaimFoundryGraphJob(ctx, "w9", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, job)
		assert.Equal(t, userLoad, job.ID, "the user's job is claimed first though queued later")
		assert.Equal(t, model.FoundryLoadByUser, job.Origin)
		refresh, err := f.db.ClaimFoundryGraphJob(ctx, "w9", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, refresh)
		assert.Equal(t, model.FoundryLoadByRefresh, refresh.Origin, "the worker measures a refresh's wait by its origin")
	})

	// The tick pages by due time and id, so ties and a page boundary lose none.
	t.Run("pages due refreshes in owed order", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "PG1")
		second := sampleOntology(t, f.db, f.ontology.WorkspaceID, "PG2")
		third := sampleOntology(t, f.db, f.ontology.WorkspaceID, "PG3")
		for _, o := range []struct {
			id  string
			ago string
		}{{f.ontology.ID, "2 HOUR"}, {second.ID, "1 HOUR"}, {third.ID, "1 HOUR"}} {
			_, err := testDB.ExecContext(ctx, `UPDATE foundry_ontologies SET refresh_due_at = DATE_SUB(NOW(6), INTERVAL `+o.ago+`) WHERE id = ?`, o.id)
			require.NoError(t, err)
		}
		var got []string
		var afterDue time.Time
		var afterID string
		for range 4 {
			page, err := f.db.ListDueFoundryRefreshes(ctx, afterDue, afterID, 1)
			require.NoError(t, err)
			if len(page) == 0 {
				break
			}
			got = append(got, page[0].ID)
			afterDue, afterID = *page[0].RefreshDueAt, page[0].ID
		}
		ties := []string{second.ID, third.ID}
		slices.Sort(ties)
		assert.Equal(t, append([]string{f.ontology.ID}, ties...), got)
	})
	t.Run("forgets an owed refresh when refresh is turned off", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, _ := refreshFixture(t, "OFF")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		require.NoError(t, f.db.SetFoundryOntologyAutoRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, false))
		o := f.ontologyNow(t)
		assert.False(t, o.AutoRefresh)
		assert.Nil(t, o.RefreshDueAt)
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		assert.Nil(t, f.ontologyNow(t).RefreshDueAt, "an ontology with refresh off owes nothing")
	})
}
