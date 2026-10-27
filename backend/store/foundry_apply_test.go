package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

func applyJob(tag string) *model.FoundryGraphJob {
	return &model.FoundryGraphJob{ID: id26("AJ-" + tag), Kind: model.FoundryGraphJobApply}
}

// applyPins are the snapshots an apply job pins, as the graph worker's first read sets them.
func applyPins(syncTag string, seq int64) []model.FoundryLoadSource {
	return []model.FoundryLoadSource{{
		ConnectorID: id26("RC-c"), Table: "customers", S3Key: "ws/customers/" + syncTag + ".jsonl.gz",
		SyncID: id26("AS-" + syncTag), SyncedAt: time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC), ChangeSeq: seq,
	}}
}

func TestFoundryApply(t *testing.T) {
	// A refresh that can apply in place keeps the load its readers use: no new
	// load is created and none is deleted after.
	t.Run("queues an apply to the current load", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, current := refreshFixture(t, "AQ")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		queued, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, applyJob("q1"))
		require.NoError(t, err)
		require.True(t, queued)
		live := f.liveJob(t)
		require.NotNil(t, live)
		assert.Equal(t, model.FoundryGraphJobApply, live.Kind)
		assert.Equal(t, current, live.LoadID)
		assert.Nil(t, f.ontologyNow(t).RefreshDueAt, "a queued apply is owed no more")
		loads, err := f.db.ListFoundryLoads(ctx, f.ontology.ID)
		require.NoError(t, err)
		assert.Len(t, loads, 1)
	})

	// The load's position moves forward with each apply, so later applies and
	// the API read where its tables are from the load, not its first job.
	t.Run("records the snapshots it applied and keeps the load current", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, current := refreshFixture(t, "AR")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		_, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, applyJob("r1"))
		require.NoError(t, err)
		job := f.claimed(t, "w2", id26("AJ-r1"))
		require.NoError(t, f.db.SetFoundryGraphJobSources(ctx, job.ID, "w2", applyPins("r1", 7)))
		finish, err := f.db.FinishFoundryGraphJob(ctx, job.ID, "w2", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("r1"),
		})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobEnded, finish)

		load, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, current)
		require.NoError(t, err)
		require.NotNil(t, load.AppliedAt)
		assert.WithinDuration(t, time.Now(), *load.AppliedAt, 5*time.Second)
		assert.Equal(t, model.FoundryLoadReady, load.Status)
		assert.Equal(t, current, *f.ontologyNow(t).CurrentLoadID)
		sources, err := f.db.ListFoundryLoadSources(ctx, []string{current})
		require.NoError(t, err)
		assert.Equal(t, applyPins("r1", 7), sources[current])
		assert.Nil(t, f.liveJob(t), "no delete follows an apply")
	})

	// An apply creates no load, yet it is a refresh: the next one waits for
	// the plan's interval as it would after a refresh load.
	t.Run("spaces applies by the plan's interval", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, _ := refreshFixture(t, "AS")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Hour))
		_, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, applyJob("s1"))
		require.NoError(t, err)
		job := f.claimed(t, "w2", id26("AJ-s1"))
		_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, "w2", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("s1"),
		})
		require.NoError(t, err)
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Hour))
		due := f.ontologyNow(t).RefreshDueAt
		require.NotNil(t, due)
		assert.WithinDuration(t, time.Now().Add(time.Hour), *due, 5*time.Second)
	})

	// An apply that couldn't bring the tables in leaves the graph behind them,
	// so the same finish queues the full refresh the tick would have.
	t.Run("hands a failed apply to a full refresh load", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, current := refreshFixture(t, "AF")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		_, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, applyJob("f1"))
		require.NoError(t, err)
		job := f.claimed(t, "w2", id26("AJ-f1"))
		handover := next("f1")
		handover.RefreshID = id26("AR-f1")
		_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, "w2", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobFailed, Error: "Helix rebuilt the graph instead", Next: handover,
		})
		require.NoError(t, err)

		live := f.liveJob(t)
		require.NotNil(t, live)
		assert.Equal(t, id26("AR-f1"), live.ID)
		assert.Equal(t, model.FoundryGraphJobLoad, live.Kind)
		refresh, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, live.LoadID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryLoadByRefresh, refresh.Origin)
		require.NotNil(t, refresh.RefreshesLoadID)
		assert.Equal(t, current, *refresh.RefreshesLoadID)
		applied, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, current)
		require.NoError(t, err)
		assert.Nil(t, applied.AppliedAt)
	})

	// Once the tables are in, only documents failed: the load is up to date
	// and a full rebuild would redo what the apply did.
	t.Run("keeps the tables an apply brought in when its documents fail", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, current := refreshFixture(t, "AD")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		_, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, applyJob("d1"))
		require.NoError(t, err)
		job := f.claimed(t, "w2", id26("AJ-d1"))
		require.NoError(t, f.db.SetFoundryGraphJobSources(ctx, job.ID, "w2", applyPins("d1", 3)))
		handover := next("d1")
		handover.RefreshID = id26("AR-d1")
		_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, "w2", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobFailed, IngestDone: true, Error: "documents failed", Next: handover,
		})
		require.NoError(t, err)
		assert.Nil(t, f.liveJob(t))
		load, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, current)
		require.NoError(t, err)
		assert.NotNil(t, load.AppliedAt)
	})

	// The user waits for a load they started; an apply does not, and the load
	// it was applying to stays, since readers still use it.
	t.Run("gives way to a load the user starts", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, current := refreshFixture(t, "AP")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		_, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, applyJob("p1"))
		require.NoError(t, err)
		f.claimed(t, "w2", id26("AJ-p1"))

		userLoad := f.load(t, "U1")
		assert.Equal(t, userLoad, f.liveJob(t).LoadID)
		apply, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, id26("AJ-p1"))
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobCancelled, apply.Status)
		assert.Equal(t, model.FoundryLoadReady, f.loadStatus(t, current))
	})

	// An apply is a refresh: a load someone is watching goes first.
	t.Run("claims a user's job before an apply", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, _ := refreshFixture(t, "AO")
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		_, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, applyJob("o1"))
		require.NoError(t, err)
		g := foundryFixture{db: f.db, ontology: sampleOntology(t, f.db, id26("FWS-AO2"), "AO2")}
		userLoad := g.load(t, "U2")
		job, err := f.db.ClaimFoundryGraphJob(ctx, "w9", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, job)
		assert.Equal(t, userLoad, job.ID, "the user's job is claimed first though queued later")
		apply, err := f.db.ClaimFoundryGraphJob(ctx, "w9", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, apply)
		assert.Equal(t, model.FoundryGraphJobApply, apply.Kind)
	})

	// A deleted load's positions go with its other rows.
	t.Run("forgets a deleted load's snapshots", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "AX")
		old := f.load(t, "L1")
		job := f.claimed(t, "w1", old)
		require.NoError(t, f.db.SetFoundryGraphJobSources(ctx, job.ID, "w1", applyPins("x1", 1)))
		f.ended(t, "w1", job.ID, model.FoundryGraphJobSucceeded, next("x1"))
		f.load(t, "L2")
		f.loaded(t, "w2")
		require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, old, id26("AX-del")))
		f.finish(t, "w3", "x3", model.FoundryGraphJobSucceeded)
		require.Equal(t, model.FoundryLoadDeleted, f.loadStatus(t, old))
		sources, err := f.db.ListFoundryLoadSources(ctx, []string{old})
		require.NoError(t, err)
		assert.Empty(t, sources)
	})

	// A full load records where its tables are from as it ends, so the first
	// apply to it knows its position.
	t.Run("records the snapshots a load pinned", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "AL")
		loadID := f.load(t, "L1")
		job := f.claimed(t, "w1", loadID)
		require.NoError(t, f.db.SetFoundryGraphJobSources(ctx, job.ID, "w1", applyPins("l1", 1)))
		f.ended(t, "w1", job.ID, model.FoundryGraphJobSucceeded, next("l1"))
		sources, err := f.db.ListFoundryLoadSources(ctx, []string{loadID, id26("FJ-none")})
		require.NoError(t, err)
		assert.Equal(t, map[string][]model.FoundryLoadSource{loadID: applyPins("l1", 1)}, sources)
	})
}
