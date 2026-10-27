package store_test

import (
	"context"
	"encoding/json"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestConnectorSyncQueue(t *testing.T) {
	workspaceID, connectorID := id26("SYNC-WS"), id26("SYNC-CONN")
	newSync := func(suffix string) *model.ConnectorSync {
		return &model.ConnectorSync{ID: id26("SYNC-" + suffix), ConnectorID: connectorID, WorkspaceID: workspaceID}
	}
	succeeded := func(rows int64) model.ConnectorSyncOutcome {
		return model.ConnectorSyncOutcome{Status: model.ConnectorSyncSucceeded, RowsSynced: &rows}
	}

	// Two syncs of one connector would race to overwrite the same snapshots.
	t.Run("keeps one live sync per connector", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		require.NoError(t, db.EnqueueConnectorSync(ctx, newSync("A")))
		assert.ErrorIs(t, db.EnqueueConnectorSync(ctx, newSync("B")), store.ErrConflict)

		active, err := db.GetActiveConnectorSync(ctx, workspaceID, connectorID)
		require.NoError(t, err)
		assert.Equal(t, id26("SYNC-A"), active.ID)

		claimed, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		require.NoError(t, db.FinishConnectorSync(ctx, claimed.ID, "worker-1", succeeded(7)))
		require.NoError(t, db.EnqueueConnectorSync(ctx, newSync("B")), "a finished sync no longer blocks the next")
	})

	t.Run("claims a due sync once and reclaims it when the lease expires", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		none, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		assert.Nil(t, none)
		require.NoError(t, db.EnqueueConnectorSync(ctx, newSync("A")))

		first, err := db.ClaimConnectorSync(ctx, "worker-1", -time.Second)
		require.NoError(t, err)
		require.NotNil(t, first)
		assert.Equal(t, model.ConnectorSyncRunning, first.Status)
		assert.Equal(t, 1, first.Attempts)

		second, err := db.ClaimConnectorSync(ctx, "worker-2", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, second, "an expired lease is claimable")
		assert.Equal(t, 2, second.Attempts)
		assert.ErrorIs(t, db.FinishConnectorSync(ctx, first.ID, "worker-1", succeeded(1)), store.ErrNotFound,
			"the worker that lost the lease cannot finish the sync")

		again, err := db.ClaimConnectorSync(ctx, "worker-3", time.Minute)
		require.NoError(t, err)
		assert.Nil(t, again, "a live lease is not claimable")
	})

	// A failed attempt waits for its backoff, then runs again.
	t.Run("requeues an attempt for its retry time", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		require.NoError(t, db.EnqueueConnectorSync(ctx, newSync("A")))
		claimed, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)

		later := time.Now().Add(time.Hour)
		require.NoError(t, db.FinishConnectorSync(ctx, claimed.ID, "worker-1", model.ConnectorSyncOutcome{RetryAt: &later, Error: "stripe 500"}))
		notYet, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		assert.Nil(t, notYet)

		stored, err := db.GetConnectorSync(ctx, workspaceID, claimed.ID)
		require.NoError(t, err)
		assert.Equal(t, model.ConnectorSyncQueued, stored.Status)
		assert.Equal(t, "stripe 500", *stored.Error)
		_, err = db.GetActiveConnectorSync(ctx, workspaceID, connectorID)
		assert.NoError(t, err, "a requeued sync is still live")
	})

	// A worker that stops mid-attempt gives the sync back at once and
	// uncounted, rather than leave it to its lease, whose expiry would count
	// the attempt; only the worker holding the lease may.
	t.Run("gives back a stopped attempt uncounted, for its holder only", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		require.NoError(t, db.EnqueueConnectorSync(ctx, newSync("A")))
		claimed, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		assert.ErrorIs(t, db.RequeueConnectorSync(ctx, claimed.ID, "worker-2"), store.ErrNotFound)

		require.NoError(t, db.RequeueConnectorSync(ctx, claimed.ID, "worker-1"))
		stored, err := db.GetConnectorSync(ctx, workspaceID, claimed.ID)
		require.NoError(t, err)
		assert.Equal(t, model.ConnectorSyncQueued, stored.Status)
		assert.Equal(t, 0, stored.Attempts)
		assert.Nil(t, stored.LeaseOwner)
		assert.Nil(t, stored.LeaseUntil)
		assert.ErrorIs(t, db.RequeueConnectorSync(ctx, claimed.ID, "worker-1"), store.ErrNotFound, "a queued sync is given back once")

		again, err := db.ClaimConnectorSync(ctx, "worker-2", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, again, "it is due at once")
		assert.Equal(t, 1, again.Attempts, "the stopped attempt is not counted")
	})

	// A workspace with many connectors must not hold every worker while others wait.
	t.Run("runs one sync per workspace at a time", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		sync := func(ws, conn, suffix string) *model.ConnectorSync {
			return &model.ConnectorSync{ID: id26("FAIR-" + suffix), ConnectorID: id26(conn), WorkspaceID: id26(ws)}
		}
		for _, s := range []*model.ConnectorSync{sync("FAIR-WS1", "FAIR-C1", "A"), sync("FAIR-WS1", "FAIR-C2", "B"), sync("FAIR-WS2", "FAIR-C3", "C")} {
			require.NoError(t, db.EnqueueConnectorSync(ctx, s))
			time.Sleep(2 * time.Millisecond)
		}
		first, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, id26("FAIR-A"), first.ID)
		second, err := db.ClaimConnectorSync(ctx, "worker-2", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, id26("FAIR-C"), second.ID, "the busy workspace's next sync waits")
		none, err := db.ClaimConnectorSync(ctx, "worker-3", time.Minute)
		require.NoError(t, err)
		assert.Nil(t, none)

		require.NoError(t, db.FinishConnectorSync(ctx, first.ID, "worker-1", succeeded(1)))
		third, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, id26("FAIR-B"), third.ID, "the workspace's next sync runs once its last ends")
	})

	// A dead worker's sync must not block its workspace until the lease is reclaimed.
	t.Run("ignores a running sync whose lease lapsed", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		ws := id26("LAPSE-WS")
		for _, suffix := range []string{"A", "B"} {
			require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("LAPSE-" + suffix), ConnectorID: id26("LAPSE-C" + suffix), WorkspaceID: ws}))
			time.Sleep(2 * time.Millisecond)
		}
		dead, err := db.ClaimConnectorSync(ctx, "worker-1", -time.Second)
		require.NoError(t, err)
		assert.Equal(t, id26("LAPSE-A"), dead.ID)
		// Another claim holding the lapsed row makes this one look past it.
		tx, err := testDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "SELECT id FROM connector_syncs WHERE id=? FOR UPDATE", dead.ID)
		require.NoError(t, err)
		next, err := db.ClaimConnectorSync(ctx, "worker-2", time.Minute)
		require.NoError(t, tx.Rollback())
		require.NoError(t, err)
		require.NotNil(t, next, "a lapsed lease does not hold the workspace")
		assert.Equal(t, id26("LAPSE-B"), next.ID)
		blocked, err := db.ClaimConnectorSync(ctx, "worker-3", time.Minute)
		require.NoError(t, err)
		assert.Nil(t, blocked, "B is live now, so the lapsed A waits")
	})

	// The claim locks only the row it takes: the busy check must see a
	// running sync another transaction holds, neither skipping it (two
	// syncs of a workspace) nor waiting on it.
	t.Run("sees a locked running sync of the workspace", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		ws := id26("HELD-WS")
		for _, suffix := range []string{"A", "B"} {
			require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("HELD-" + suffix), ConnectorID: id26("HELD-C" + suffix), WorkspaceID: ws}))
			time.Sleep(2 * time.Millisecond)
		}
		running, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, id26("HELD-A"), running.ID)
		tx, err := testDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "SELECT id FROM connector_syncs WHERE id=? FOR UPDATE", running.ID)
		require.NoError(t, err)
		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		next, err := db.ClaimConnectorSync(claimCtx, "worker-2", time.Minute)
		cancel()
		require.NoError(t, tx.Rollback())
		require.NoError(t, err)
		assert.Nil(t, next, "B waits for the workspace's running A")
	})

	// An event source's sync takes seconds and a database's hours: a
	// workspace runs one sync of each class at a time, so neither waits
	// behind the other, and two of one class never run together.
	t.Run("runs one event source sync per workspace beside one other sync", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		ws := id26("CLASS-WS")
		for _, s := range []struct {
			tag    string
			origin model.ConnectorSyncOrigin
		}{{"E1", model.ConnectorSyncEvents}, {"DB", model.ConnectorSyncSchedule}, {"E2", model.ConnectorSyncEvents}, {"M", model.ConnectorSyncManual}} {
			require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("CLASS-" + s.tag), ConnectorID: id26("CLASS-C" + s.tag),
				WorkspaceID: ws, Origin: s.origin}))
			time.Sleep(2 * time.Millisecond)
		}
		events, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, id26("CLASS-E1"), events.ID)
		database, err := db.ClaimConnectorSync(ctx, "worker-2", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, database, "a database's sync runs beside an event source's")
		assert.Equal(t, id26("CLASS-DB"), database.ID)
		none, err := db.ClaimConnectorSync(ctx, "worker-3", time.Minute)
		require.NoError(t, err)
		assert.Nil(t, none, "the second event source sync and the manual sync wait for their class")

		require.NoError(t, db.FinishConnectorSync(ctx, events.ID, "worker-1", succeeded(1)))
		next, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, next, "an event source's sync runs beside the database's")
		assert.Equal(t, id26("CLASS-E2"), next.ID)
		none, err = db.ClaimConnectorSync(ctx, "worker-3", time.Minute)
		require.NoError(t, err)
		assert.Nil(t, none, "the manual sync still waits for the database's")

		require.NoError(t, db.FinishConnectorSync(ctx, database.ID, "worker-2", succeeded(1)))
		manual, err := db.ClaimConnectorSync(ctx, "worker-2", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, manual)
		assert.Equal(t, id26("CLASS-M"), manual.ID, "the workspace's next other sync runs once the database's ends")
	})

	t.Run("records the outcome and lists newest first", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		require.NoError(t, db.EnqueueConnectorSync(ctx, newSync("A")))
		first, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		require.NoError(t, db.FinishConnectorSync(ctx, first.ID, "worker-1", model.ConnectorSyncOutcome{Status: model.ConnectorSyncFailed, Error: "key revoked"}))
		time.Sleep(2 * time.Millisecond)
		require.NoError(t, db.EnqueueConnectorSync(ctx, newSync("B")))

		list, err := db.ListConnectorSyncs(ctx, workspaceID, connectorID, 10)
		require.NoError(t, err)
		require.Len(t, list, 2)
		assert.Equal(t, id26("SYNC-B"), list[0].ID)
		assert.Equal(t, model.ConnectorSyncFailed, list[1].Status)
		assert.NotNil(t, list[1].FinishedAt)
		other, err := db.ListConnectorSyncs(ctx, id26("SYNC-OTHER"), connectorID, 10)
		require.NoError(t, err)
		assert.Empty(t, other, "syncs are scoped to their workspace")
	})
}

// connectorRow writes live connector id of the workspace, which a sync's
// table writes lock.
func connectorRow(t *testing.T, db store.Store, workspaceID, id string) {
	t.Helper()
	require.NoError(t, db.CreateConnector(context.Background(), &model.Connector{
		ID: id, WorkspaceID: workspaceID, Kind: "stripe", Name: "Stripe", Config: json.RawMessage(`{}`),
		SecretCiphertext: []byte("sealed"), SecretKeyID: "k1", SecretHint: "sk_test…1234",
	}))
}

func TestConnectorTables(t *testing.T) {
	workspaceID, connectorID := id26("TBL-WS"), id26("TBL-CONN")
	// store is an empty store holding the connector.
	newStore := func(t *testing.T) store.Store {
		t.Helper()
		db := newTestStore(t)
		connectorRow(t, db, workspaceID, connectorID)
		return db
	}
	// claim queues a sync and leases it to workerID.
	claim := func(t *testing.T, db store.Store, suffix, workerID string, lease time.Duration) *model.ConnectorSync {
		t.Helper()
		ctx := context.Background()
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("TBL-SYNC-" + suffix), ConnectorID: connectorID, WorkspaceID: workspaceID}))
		sync, err := db.ClaimConnectorSync(ctx, workerID, lease)
		require.NoError(t, err)
		require.NotNil(t, sync)
		return sync
	}
	table := func(name, key string, sync *model.ConnectorSync) *model.ConnectorTable {
		return &model.ConnectorTable{
			ID: id26("TBL-" + name + "-" + key), ConnectorID: sync.ConnectorID, WorkspaceID: workspaceID, TableName: name,
			Columns: json.RawMessage(`[{"Name":"customer_id"}]`), PrimaryKey: json.RawMessage(`["customer_id"]`),
			ForeignKeys: json.RawMessage(`[]`), S3Key: "ws/" + name + "/" + key + ".jsonl.gz", RowCount: 3, SyncID: sync.ID,
			SyncedAt: time.Now(),
		}
	}

	// A new sync replaces the table's snapshot pointer rather than adding a row.
	t.Run("upserts the latest snapshot per table", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		first := claim(t, db, "1", "worker-1", time.Minute)
		require.NoError(t, db.UpsertConnectorTable(ctx, table("customers", "1", first), nil, "worker-1"))
		rows := int64(3)
		require.NoError(t, db.FinishConnectorSync(ctx, first.ID, "worker-1", model.ConnectorSyncOutcome{Status: model.ConnectorSyncSucceeded, RowsSynced: &rows}))
		second := claim(t, db, "2", "worker-1", time.Minute)
		next := table("customers", "2", second)
		next.RowCount = 5
		require.NoError(t, db.UpsertConnectorTable(ctx, next, nil, "worker-1"))

		tables, err := db.ListConnectorTables(ctx, workspaceID, connectorID)
		require.NoError(t, err)
		require.Len(t, tables, 1)
		assert.Equal(t, id26("TBL-customers-1"), tables[0].ID, "the row keeps its id")
		assert.Equal(t, "ws/customers/2.jsonl.gz", tables[0].S3Key)
		assert.Equal(t, int64(5), tables[0].RowCount)
		assert.JSONEq(t, `["customer_id"]`, string(tables[0].PrimaryKey))
	})

	// A paused worker that resumes after a takeover must not overwrite the new owner's result.
	t.Run("refuses writes from a worker that lost the lease", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		stale := claim(t, db, "1", "worker-1", -time.Second)
		taken, err := db.ClaimConnectorSync(ctx, "worker-2", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, taken)
		require.NoError(t, db.UpsertConnectorTable(ctx, table("customers", "new", taken), nil, "worker-2"))

		assert.ErrorIs(t, db.UpsertConnectorTable(ctx, table("customers", "old", stale), nil, "worker-1"), store.ErrNotFound)
		_, err = db.PruneConnectorTables(ctx, workspaceID, connectorID, nil, stale.ID, "worker-1")
		assert.ErrorIs(t, err, store.ErrNotFound)
		tables, err := db.ListConnectorTables(ctx, workspaceID, connectorID)
		require.NoError(t, err)
		require.Len(t, tables, 1)
		assert.Equal(t, "ws/customers/new.jsonl.gz", tables[0].S3Key)
	})

	// A sync that a delete of its connector overtook writes no table after
	// it: the delete took the connector's tables, and nothing would take one
	// written later.
	t.Run("refuses writes to a deleted connector", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		sync := claim(t, db, "1", "worker-1", time.Minute)
		require.NoError(t, db.DeleteConnector(ctx, workspaceID, connectorID))
		assert.ErrorIs(t, db.UpsertConnectorTable(ctx, table("customers", "1", sync), nil, "worker-1"), store.ErrNotFound)
		_, err := db.PruneConnectorTables(ctx, workspaceID, connectorID, nil, sync.ID, "worker-1")
		assert.ErrorIs(t, err, store.ErrNotFound)
		tables, err := db.ListConnectorTables(ctx, workspaceID, connectorID)
		require.NoError(t, err)
		assert.Empty(t, tables)
	})

	t.Run("prunes the connector's tables that were not kept", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		sync := claim(t, db, "1", "worker-1", time.Minute)
		require.NoError(t, db.UpsertConnectorTable(ctx, table("customers", "1", sync), nil, "worker-1"))
		require.NoError(t, db.UpsertConnectorTable(ctx, table("charges", "1", sync), nil, "worker-1"))
		other := *sync
		other.ConnectorID = id26("TBL-OTHER")
		connectorRow(t, db, workspaceID, other.ConnectorID)
		require.NoError(t, db.UpsertConnectorTable(ctx, table("refunds", "1", &other), nil, "worker-1"))

		removed, err := db.PruneConnectorTables(ctx, workspaceID, connectorID, []string{"charges"}, sync.ID, "worker-1")
		require.NoError(t, err)
		assert.Equal(t, []string{"customers"}, removed)
		replaced, err := db.ListReplacedSnapshots(ctx, time.Now().Add(time.Minute), 10)
		require.NoError(t, err)
		require.Len(t, replaced, 1, "the dropped table's snapshot waits for the sweep")
		assert.Equal(t, "ws/customers/1.jsonl.gz", replaced[0].S3Key)
		batches, err := db.ListConnectorChangeBatches(ctx, workspaceID, sync.ID)
		require.NoError(t, err)
		require.Len(t, batches, 1)
		assert.Equal(t, model.ConnectorDropBatch, batches[0].Kind)
		tables, err := db.ListConnectorTables(ctx, workspaceID, connectorID)
		require.NoError(t, err)
		require.Len(t, tables, 1)
		assert.Equal(t, "charges", tables[0].TableName)
		others, err := db.ListConnectorTables(ctx, workspaceID, id26("TBL-OTHER"))
		require.NoError(t, err)
		assert.Len(t, others, 1, "another connector's tables are left alone")
	})
}

func TestPushedConnectorSchedules(t *testing.T) {
	workspaceID := id26("PUSH-WS")
	// setNext stores c's next sync at next, none for nil.
	setNext := func(t *testing.T, c *model.Connector, next *time.Time) {
		t.Helper()
		var at any
		if next != nil {
			at = next.UTC()
		}
		_, err := testDB.Exec(`UPDATE connectors SET next_sync_at=? WHERE id=?`, at, c.ID)
		require.NoError(t, err)
	}
	// claimed queues a sync of event source c, as every sync of one is, and
	// leases it to w1.
	claimed := func(t *testing.T, db store.Store, c *model.Connector) *model.ConnectorSync {
		t.Helper()
		ctx := context.Background()
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id.New(), ConnectorID: c.ID, WorkspaceID: c.WorkspaceID,
			Origin: model.ConnectorSyncEvents}))
		sync, err := db.ClaimConnectorSync(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, sync)
		require.Equal(t, c.ID, sync.ConnectorID)
		return sync
	}
	pushed := func(next model.PushedNext, paused string) model.ConnectorSyncOutcome {
		rows := int64(1)
		return model.ConnectorSyncOutcome{Status: model.ConnectorSyncSucceeded, RowsSynced: &rows,
			Schedule: &model.ConnectorScheduleOutcome{Pushed: next, Paused: paused}}
	}

	// The claim keeps a workspace to one event source sync at a time, which
	// it tells by origin: an event source's scheduled sync must say so.
	t.Run("queues an event source's sync as origin events", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		past := time.Now().Add(-time.Minute)
		source := eventSource(t, db, workspaceID, "due")
		setNext(t, source, &past)
		database := scheduledConnector(t, db, workspaceID, "due-db", &past)

		n, err := db.QueueDueConnectorSyncs(ctx, 10, time.Minute)
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		for c, origin := range map[*model.Connector]model.ConnectorSyncOrigin{source: model.ConnectorSyncEvents, database: model.ConnectorSyncSchedule} {
			active, err := db.GetActiveConnectorSync(ctx, workspaceID, c.ID)
			require.NoError(t, err)
			assert.Equal(t, origin, active.Origin, c.Kind)
			stored, err := db.GetConnector(ctx, workspaceID, c.ID)
			require.NoError(t, err)
			assert.Nil(t, stored.NextSyncAt, "%s waits for its sync to end", c.Kind)
		}
	})

	// The pieces that made a source due may have come after the live sync
	// read what waits, and a send schedules a sync only when none is: left
	// unscheduled, they would wait for the next send, however long.
	t.Run("schedules an event source with a live sync again after sync_after", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		past := time.Now().Add(-time.Minute)
		source := eventSource(t, db, workspaceID, "busy")
		setNext(t, source, &past)
		live := claimed(t, db, source)
		database := scheduledConnector(t, db, workspaceID, "busy-db", &past)
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id.New(), ConnectorID: database.ID, WorkspaceID: workspaceID}))

		start := time.Now()
		n, err := db.QueueDueConnectorSyncs(ctx, 10, 5*time.Minute)
		require.NoError(t, err)
		assert.Equal(t, 0, n, "each keeps its live sync")
		stored, err := db.GetConnector(ctx, workspaceID, source.ID)
		require.NoError(t, err)
		require.NotNil(t, stored.NextSyncAt)
		assert.WithinDuration(t, start.Add(5*time.Minute), *stored.NextSyncAt, 10*time.Second)
		other, err := db.GetConnector(ctx, workspaceID, database.ID)
		require.NoError(t, err)
		assert.Nil(t, other.NextSyncAt, "a kind with an interval is scheduled as its sync ends")

		require.NoError(t, db.FinishConnectorSync(ctx, live.ID, "w1", pushed(model.PushedKeep{}, "")))
		after, err := db.GetConnector(ctx, workspaceID, source.ID)
		require.NoError(t, err)
		require.NotNil(t, after.NextSyncAt, "the live sync's end keeps the sync queued after it")
		assert.True(t, stored.NextSyncAt.Equal(*after.NextSyncAt))
	})

	// A send schedules a sync while one runs; the finish says what becomes
	// of it. NotBefore never pulls a send's later sync earlier, nor lets the
	// spacing be cut short; only At, for a backlog over one read, moves it.
	base := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
	at := func(d time.Duration) *time.Time {
		t := base.Add(d)
		return &t
	}
	for _, c := range []struct {
		name         string
		stored, want *time.Time
		next         model.PushedNext
	}{
		{"keeps no sync when nothing waits", nil, nil, model.PushedKeep{}},
		{"keeps the sync a send scheduled", at(time.Minute), at(time.Minute), model.PushedKeep{}},
		{"schedules the spacing when no sync is", nil, at(0), model.PushedNotBefore{At: base}},
		{"moves an earlier sync out to the spacing", at(-time.Minute), at(0), model.PushedNotBefore{At: base}},
		{"keeps a later sync a send scheduled", at(time.Minute), at(time.Minute), model.PushedNotBefore{At: base}},
		{"syncs at once before a later sync", at(time.Minute), at(0), model.PushedAt{At: base}},
		{"syncs at once when no sync is", nil, at(0), model.PushedAt{At: base}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			db := newTestStore(t)
			source := eventSource(t, db, workspaceID, "next")
			sync := claimed(t, db, source)
			setNext(t, source, c.stored)
			require.NoError(t, db.FinishConnectorSync(ctx, sync.ID, "w1", pushed(c.next, "")))
			stored, err := db.GetConnector(ctx, workspaceID, source.ID)
			require.NoError(t, err)
			if c.want == nil {
				assert.Nil(t, stored.NextSyncAt)
				return
			}
			require.NotNil(t, stored.NextSyncAt)
			assert.True(t, c.want.Equal(*stored.NextSyncAt), "next sync %v, want %v", *stored.NextSyncAt, *c.want)
		})
	}

	// An event source has no interval of its own to guard the schedule
	// with: an interval stored on one is not the finish's to honour.
	t.Run("writes the pause and the next sync whatever interval is stored", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		source := eventSource(t, db, workspaceID, "guard")
		_, err := testDB.Exec(`UPDATE connectors SET sync_every_seconds=3600 WHERE id=?`, source.ID)
		require.NoError(t, err)
		sync := claimed(t, db, source)
		require.NoError(t, db.FinishConnectorSync(ctx, sync.ID, "w1", pushed(model.PushedAt{At: base}, "batch 3 of this event source is missing")))
		stored, err := db.GetConnector(ctx, workspaceID, source.ID)
		require.NoError(t, err)
		require.NotNil(t, stored.NextSyncAt)
		assert.True(t, base.Equal(*stored.NextSyncAt))
		require.NotNil(t, stored.SchedulePaused)
		assert.Equal(t, "batch 3 of this event source is missing", *stored.SchedulePaused)

		sync = claimed(t, db, source)
		require.NoError(t, db.FinishConnectorSync(ctx, sync.ID, "w1", pushed(model.PushedKeep{}, "")))
		stored, err = db.GetConnector(ctx, workspaceID, source.ID)
		require.NoError(t, err)
		assert.Nil(t, stored.SchedulePaused, "a sync that succeeds clears the pause")
		require.NotNil(t, stored.NextSyncAt)
		assert.True(t, base.Equal(*stored.NextSyncAt))
	})

	// A deleted source must never be queued again.
	t.Run("leaves a deleted source unscheduled", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		source := eventSource(t, db, workspaceID, "gone")
		sync := claimed(t, db, source)
		require.NoError(t, db.DeleteConnector(ctx, workspaceID, source.ID))
		require.NoError(t, db.FinishConnectorSync(ctx, sync.ID, "w1", pushed(model.PushedAt{At: base}, "")))
		_, next, _, _ := storedEventSource(t, source)
		assert.Nil(t, next)
	})

	// A pushed schedule replaces the time and the interval; one that also
	// names either says two things, and neither is written. Nor is a
	// PushedNext the finish does not know, such as a pointer to one.
	t.Run("refuses a pushed schedule with a time, an interval or an unknown next", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		source := eventSource(t, db, workspaceID, "both")
		sync := claimed(t, db, source)
		hour := 3600
		for _, schedule := range []model.ConnectorScheduleOutcome{
			{Pushed: model.PushedKeep{}, NextSyncAt: at(0)},
			{Pushed: model.PushedNotBefore{At: base}, Every: &hour},
			{Pushed: &model.PushedAt{At: base}},
		} {
			outcome := pushed(nil, "")
			outcome.Schedule = &schedule
			assert.Error(t, db.FinishConnectorSync(ctx, sync.ID, "w1", outcome))
		}
		active, err := db.GetActiveConnectorSync(ctx, workspaceID, source.ID)
		require.NoError(t, err)
		assert.Equal(t, model.ConnectorSyncRunning, active.Status, "the sync is still running")
	})
}
