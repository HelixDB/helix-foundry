package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// cutover is the rebuilds' cutover in these tests: loads created before it
// are an earlier engine's.
var cutover = time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)

// earlier dates a load before cutover, as an earlier release wrote it.
func earlier(t *testing.T, loadID string) {
	t.Helper()
	_, err := testDB.ExecContext(context.Background(), `UPDATE foundry_loads SET created_at='2020-06-01 00:00:00' WHERE id=?`, loadID)
	require.NoError(t, err)
}

func rebuildJob(tag string) *model.FoundryGraphJob {
	return &model.FoundryGraphJob{ID: id26("RB-" + tag)}
}

func TestFoundryRebuild(t *testing.T) {
	// The gates of a refresh don't apply: with refresh off and none due, the
	// current load is loaded again in full, as a refresh replaces it.
	t.Run("queues a full refresh load of an earlier current load, refresh off included", func(t *testing.T) {
		ctx := context.Background()
		f, _, current := refreshFixture(t, "RB1")
		require.NoError(t, f.db.SetFoundryOntologyAutoRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, false))
		earlier(t, current)
		require.Nil(t, f.ontologyNow(t).RefreshDueAt)

		done, err := f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("1"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildQueued, done)
		live := f.liveJob(t)
		require.NotNil(t, live)
		assert.Equal(t, id26("RB-1"), live.ID)
		assert.Equal(t, model.FoundryGraphJobLoad, live.Kind, "a full load, not an apply")
		assert.Equal(t, model.FoundryGraphJobProvisioning, live.Phase)
		assert.Equal(t, live.ID, live.LoadID, "it writes a load of its own")
		assert.Nil(t, live.RequestedByUserID, "no person started it")
		rebuild, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, live.LoadID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryLoadByRefresh, rebuild.Origin)
		require.NotNil(t, rebuild.RefreshesLoadID)
		assert.Equal(t, current, *rebuild.RefreshesLoadID)
		old, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, current)
		require.NoError(t, err)
		assert.Equal(t, old.VersionID, rebuild.VersionID, "the current load's version")
		o := f.ontologyNow(t)
		assert.False(t, o.AutoRefresh, "refresh stays off")
		assert.Nil(t, o.RefreshDueAt)
		assert.Equal(t, current, *o.CurrentLoadID, "the old load serves until the rebuild holds everything")

		// Once it is written, it is current, and nothing more is owed.
		f.loaded(t, "w2")
		assert.Equal(t, id26("RB-1"), *f.ontologyNow(t).CurrentLoadID)
		done, err = f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("2"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildNotOwed, done)
		assert.Nil(t, f.liveJob(t))
	})

	// A second run while the first rebuild is queued or running adds none.
	t.Run("is idempotent while its rebuild is live", func(t *testing.T) {
		ctx := context.Background()
		f, _, current := refreshFixture(t, "RB2")
		earlier(t, current)
		done, err := f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("a"))
		require.NoError(t, err)
		require.Equal(t, store.FoundryRebuildQueued, done)
		for _, tag := range []string{"b", "c"} {
			done, err = f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob(tag))
			require.NoError(t, err)
			assert.Equal(t, store.FoundryRebuildUnderWay, done)
		}
		f.claimed(t, "w2", id26("RB-a"))
		done, err = f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("d"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildUnderWay, done, "running, it is still the rebuild")
		assert.Equal(t, id26("RB-a"), f.liveJob(t).ID)
		_, err = f.db.GetFoundryLoad(ctx, f.ontology.ID, id26("RB-b"))
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// A rebuild that fails leaves the old load current and still owed.
	t.Run("is owed again after a rebuild that failed", func(t *testing.T) {
		ctx := context.Background()
		f, _, current := refreshFixture(t, "RB3")
		earlier(t, current)
		done, err := f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("f1"))
		require.NoError(t, err)
		require.Equal(t, store.FoundryRebuildQueued, done)
		f.finish(t, "w2", "failed", model.FoundryGraphJobFailed)
		assert.Equal(t, current, *f.ontologyNow(t).CurrentLoadID)
		done, err = f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("f2"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildQueued, done)
		assert.Equal(t, id26("RB-f2"), f.liveJob(t).ID)
	})

	// Someone else's job goes first; an owed refresh stays owed.
	t.Run("waits for another live job, and leaves an owed refresh owed", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, current := refreshFixture(t, "RB4")
		earlier(t, current)
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, "", []string{"customers"}, time.Millisecond))
		userLoad := f.load(t, "U-RB4")
		done, err := f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("w1"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildBusy, done)
		assert.Equal(t, userLoad, f.liveJob(t).LoadID, "the user's load is untouched")
		assert.NotNil(t, f.ontologyNow(t).RefreshDueAt)
	})

	// An apply writes in place into the load the rebuild replaces: it gives
	// way, queued or running, as it does to a person's load, and the
	// refresh it was is owed again, to run once the rebuild ends.
	t.Run("cancels a live apply and owes its refresh again", func(t *testing.T) {
		for _, running := range []bool{false, true} {
			t.Run(fmt.Sprintf("running=%t", running), func(t *testing.T) {
				ctx := context.Background()
				tag := fmt.Sprintf("RBP%t", running)[:5]
				f, connectorID, current := refreshFixture(t, tag)
				earlier(t, current)
				require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, "", []string{"customers"}, time.Millisecond))
				queued, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, applyJob(tag))
				require.NoError(t, err)
				require.True(t, queued)
				require.Nil(t, f.ontologyNow(t).RefreshDueAt, "the apply took the owed refresh")
				if running {
					f.claimed(t, "w2", id26("AJ-"+tag))
				}

				done, err := f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob(tag))
				require.NoError(t, err)
				assert.Equal(t, store.FoundryRebuildQueued, done)
				apply, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, id26("AJ-"+tag))
				require.NoError(t, err)
				assert.Equal(t, model.FoundryGraphJobCancelled, apply.Status)
				assert.Contains(t, *apply.Error, "rebuild")
				live := f.liveJob(t)
				require.NotNil(t, live)
				assert.Equal(t, id26("RB-"+tag), live.ID)
				require.NotNil(t, live.ClientToken)
				assert.Equal(t, model.FoundryRebuildToken+live.ID, *live.ClientToken)
				assert.Equal(t, model.FoundryLoadReady, f.loadStatus(t, current), "the applied load serves until the rebuild replaces it")
				assert.NotNil(t, f.ontologyNow(t).RefreshDueAt, "the refresh is owed again")
				// The owed refresh waits for the rebuild.
				queued, err = f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, applyJob(tag+"b"))
				require.NoError(t, err)
				assert.False(t, queued)
				assert.Equal(t, id26("RB-"+tag), f.liveJob(t).ID)
			})
		}
	})

	// A delete only frees space: it gives way to the rebuild as to any load.
	t.Run("lets a rebuild preempt a delete", func(t *testing.T) {
		ctx := context.Background()
		f, _, first := refreshFixture(t, "RBD")
		version := mustLoad(t, f, first).VersionID
		second := loadJob(f.ontology, "RBD2")
		second.VersionID = version
		require.NoError(t, f.db.CreateFoundryLoad(ctx, second))
		f.loaded(t, "w2")
		earlier(t, second.LoadID)
		require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, id26("FN-D-RBD")))

		done, err := f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("d1"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildQueued, done)
		del, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, id26("FN-D-RBD"))
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobCancelled, del.Status)
		assert.Equal(t, id26("RB-d1"), f.liveJob(t).ID)
	})

	// Each rebuild loads every table again: one that keeps ending without
	// replacing the load is queued MaxFoundryRebuilds times, then given up
	// on. One a person's load cancelled doesn't count.
	t.Run("gives up on a load its rebuilds keep failing to replace", func(t *testing.T) {
		ctx := context.Background()
		f, _, current := refreshFixture(t, "RBF")
		earlier(t, current)
		done, err := f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("c0"))
		require.NoError(t, err)
		require.Equal(t, store.FoundryRebuildQueued, done)
		f.load(t, "U-RBF")
		cancelled, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, id26("RB-c0"))
		require.NoError(t, err)
		require.Equal(t, model.FoundryGraphJobCancelled, cancelled.Status)
		f.finish(t, "w2", "u", model.FoundryGraphJobFailed)
		// The person's load failed, and the cancelled rebuild's load is being
		// deleted: the next rebuild preempts that delete.
		require.Equal(t, current, *f.ontologyNow(t).CurrentLoadID)

		for i := range store.MaxFoundryRebuilds {
			done, err := f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob(fmt.Sprint("f", i)))
			require.NoError(t, err)
			require.Equal(t, store.FoundryRebuildQueued, done, "attempt %d", i)
			f.finish(t, "w2", fmt.Sprint("f", i), model.FoundryGraphJobFailed)
		}
		done, err = f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("last"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildGaveUp, done)
		if live := f.liveJob(t); live != nil {
			assert.Equal(t, model.FoundryGraphJobDelete, live.Kind, "only the failed rebuilds' loads being deleted")
		}
		got, err := store.RebuildFoundryLoads(ctx, f.db, cutover)
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuilds{GaveUp: []string{f.ontology.ID}}, got)
		assert.Equal(t, current, *f.ontologyNow(t).CurrentLoadID)
	})

	// A refresh an earlier engine started writes that engine's load: it is
	// no rebuild, and holds one back until it ends.
	t.Run("takes an earlier engine's live refresh for no rebuild", func(t *testing.T) {
		ctx := context.Background()
		f, connectorID, current := refreshFixture(t, "RB8")
		earlier(t, current)
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, "", []string{"customers"}, time.Millisecond))
		queued, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, refreshJob("RB8"))
		require.NoError(t, err)
		require.True(t, queued)
		earlier(t, id26("RJ-RB8"))
		done, err := f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("e1"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildBusy, done)
		// One this engine started is the rebuild, whoever queued it.
		_, err = testDB.ExecContext(ctx, `UPDATE foundry_loads SET created_at=NOW(6) WHERE id=?`, id26("RJ-RB8"))
		require.NoError(t, err)
		done, err = f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("e2"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildUnderWay, done)
	})

	t.Run("owes nothing without a current load, or for one written since the cutover", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "RB5")
		done, err := f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("n1"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildNotOwed, done)

		g, _, _ := refreshFixture(t, "RB6")
		done, err = g.db.QueueFoundryRebuild(ctx, g.ontology.WorkspaceID, g.ontology.ID, cutover, rebuildJob("n2"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildNotOwed, done)
		assert.Nil(t, g.liveJob(t))
		// The cutover is exclusive: a load created at it is this engine's.
		var created time.Time
		require.NoError(t, testDB.QueryRowContext(ctx, `SELECT created_at FROM foundry_loads WHERE id=?`, *g.ontologyNow(t).CurrentLoadID).Scan(&created))
		done, err = g.db.QueueFoundryRebuild(ctx, g.ontology.WorkspaceID, g.ontology.ID, created, rebuildJob("n3"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildNotOwed, done)
		done, err = g.db.QueueFoundryRebuild(ctx, g.ontology.WorkspaceID, g.ontology.ID, created.Add(time.Microsecond), rebuildJob("n4"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildQueued, done)
	})

	t.Run("refuses an ontology that isn't live", func(t *testing.T) {
		ctx := context.Background()
		f, _, current := refreshFixture(t, "RB7")
		earlier(t, current)
		_, err := testDB.ExecContext(ctx, `UPDATE foundry_ontologies SET deleted_at=NOW(6), current_load_id=NULL WHERE id=?`, f.ontology.ID)
		require.NoError(t, err)
		_, err = f.db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("x"))
		assert.ErrorIs(t, err, store.ErrNotFound)
		listed, err := f.db.ListFoundryRebuilds(ctx, cutover, "", 10)
		require.NoError(t, err)
		assert.Empty(t, listed, "a deleted ontology is never listed")
		// Live again with its earlier load, it is.
		_, err = testDB.ExecContext(ctx, `UPDATE foundry_ontologies SET deleted_at=NULL, current_load_id=? WHERE id=?`, current, f.ontology.ID)
		require.NoError(t, err)
		listed, err = f.db.ListFoundryRebuilds(ctx, cutover, "", 10)
		require.NoError(t, err)
		assert.Len(t, listed, 1)
	})

	// The ontologies owed a rebuild, by id, a page at a time; the run
	// queues each it can and says which a live job held back.
	t.Run("lists and rebuilds every ontology owed one", func(t *testing.T) {
		ctx := context.Background()
		a, _, aLoad := refreshFixture(t, "RBA")
		b := foundryFixture{db: a.db, ontology: sampleOntology(t, a.db, a.ontology.WorkspaceID, "RBB")}
		b.version(t, "RBB")
		require.NoError(t, b.db.CreateFoundryLoad(ctx, loadJob(b.ontology, "RBB")))
		b.loaded(t, "wb")
		earlier(t, aLoad)
		earlier(t, id26("FJ-RBB"))
		c := foundryFixture{db: a.db, ontology: sampleOntology(t, a.db, a.ontology.WorkspaceID, "RBC")}
		c.load(t, "RBC")
		c.loaded(t, "wc")
		// b has a live job of its own.
		b.load(t, "RBB2")

		var listed []string
		for after := ""; ; {
			page, err := a.db.ListFoundryRebuilds(ctx, cutover, after, 1)
			require.NoError(t, err)
			if len(page) == 0 {
				break
			}
			listed = append(listed, page[0].ID)
			after = page[0].ID
		}
		assert.Equal(t, []string{a.ontology.ID, b.ontology.ID}, listed, "c's load is this engine's")

		got, err := store.RebuildFoundryLoads(ctx, a.db, cutover)
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuilds{Queued: []string{a.ontology.ID}, Busy: []string{b.ontology.ID}}, got)
		assert.Equal(t, model.FoundryLoadByRefresh, mustLoad(t, a, a.liveJob(t).LoadID).Origin)
		assert.Nil(t, c.liveJob(t))

		// Run again once b's job ends: b only, a's rebuild being live.
		b.finish(t, "wb2", "b2", model.FoundryGraphJobFailed)
		got, err = store.RebuildFoundryLoads(ctx, a.db, cutover)
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuilds{Queued: []string{b.ontology.ID}, UnderWay: []string{a.ontology.ID}}, got)
	})
}

// earlierLoad writes, in db, a live ontology FO-<tag> of workspaceID whose
// current load FJ-R<tag> an earlier engine wrote, of a version generation
// RG-<tag> drafted: from the sample company when sample is set.
func earlierLoad(t *testing.T, db store.Store, workspaceID, tag string, sample bool) foundryFixture {
	t.Helper()
	ctx := context.Background()
	f := foundryFixture{db: db, ontology: sampleOntology(t, db, workspaceID, tag)}
	genID := id26("RG-" + tag)
	in := model.OntologyGenerationInput{Tables: []model.OntologyGenerationTable{{ConnectorID: id26("RC-" + tag), Table: "customers"}}}
	if sample {
		in.Sample = "acme"
	}
	input, err := json.Marshal(in)
	require.NoError(t, err)
	_, err = testDB.ExecContext(ctx, `INSERT INTO foundry_ontology_generations (id, workspace_id, ontology_id, status, input)
VALUES (?, ?, ?, 'succeeded', ?)`, genID, workspaceID, f.ontology.ID, string(input))
	require.NoError(t, err)
	v := &model.OntologyVersion{
		ID: id26("FV-R" + tag), WorkspaceID: workspaceID, OntologyID: f.ontology.ID, Source: model.OntologySourceGenerated,
		Format: model.OntologyFormatAgentBundle, Definition: json.RawMessage(`{}`), GenerationID: &genID,
		DocumentSetIDs: &[]string{generalSetID(f.ontology.ID)},
	}
	require.NoError(t, db.CreateOntologyVersion(ctx, v))
	job := loadJob(f.ontology, "R"+tag)
	job.VersionID = v.ID
	require.NoError(t, db.CreateFoundryLoad(ctx, job))
	f.loaded(t, "w-"+tag)
	earlier(t, job.LoadID)
	return f
}

// sampled makes the generation of earlierLoad's fixture tag a sample's, as
// if an earlier run had queued its rebuild before samples were skipped.
func sampled(t *testing.T, tag string) {
	t.Helper()
	_, err := testDB.ExecContext(context.Background(), `UPDATE foundry_ontology_generations SET input=JSON_SET(input, '$.sample', 'acme') WHERE id=?`,
		id26("RG-"+tag))
	require.NoError(t, err)
}

func TestFoundryRebuildSamples(t *testing.T) {
	// The sample's data is fixed and its datetimes are to the minute: Helix
	// converted them exactly, so its load is listed but never rebuilt.
	t.Run("never rebuilds a sample's load", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		f := earlierLoad(t, db, id26("FWS-RBS1"), "RBS1", true)
		current := *f.ontologyNow(t).CurrentLoadID

		done, err := db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("s1"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildSample, done)
		assert.Nil(t, f.liveJob(t))
		got, err := store.RebuildFoundryLoads(ctx, db, cutover)
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuilds{Samples: []string{f.ontology.ID}}, got)
		assert.Nil(t, f.liveJob(t))
		assert.Equal(t, current, *f.ontologyNow(t).CurrentLoadID)
		job, err := db.ClaimFoundryGraphJob(ctx, "w2", time.Minute)
		require.NoError(t, err)
		assert.Nil(t, job, "nothing queued")
	})

	// A rebuild an earlier run queued is withdrawn while it waits, its load
	// deleted at once (nothing of it reached the database); one a worker
	// started runs to its end.
	t.Run("withdraws a queued rebuild of a sample, and leaves a started one", func(t *testing.T) {
		for _, started := range []bool{false, true} {
			t.Run(fmt.Sprintf("started=%t", started), func(t *testing.T) {
				ctx := context.Background()
				db := newTestStore(t)
				tag := fmt.Sprintf("RBW%t", started)[:5]
				f := earlierLoad(t, db, id26("FWS-"+tag), tag, false)
				current := *f.ontologyNow(t).CurrentLoadID
				done, err := db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob(tag))
				require.NoError(t, err)
				require.Equal(t, store.FoundryRebuildQueued, done)
				rebuild := id26("RB-" + tag)
				if started {
					f.claimed(t, "w2", rebuild)
				}
				sampled(t, tag)

				got, err := store.RebuildFoundryLoads(ctx, db, cutover)
				require.NoError(t, err)
				job, err := db.GetFoundryGraphJob(ctx, f.ontology.ID, rebuild)
				require.NoError(t, err)
				if started {
					assert.Equal(t, store.FoundryRebuilds{UnderWay: []string{f.ontology.ID}}, got)
					assert.Equal(t, model.FoundryGraphJobRunning, job.Status)
					assert.Equal(t, rebuild, f.liveJob(t).ID)
					assert.Equal(t, model.FoundryLoadWriting, f.loadStatus(t, rebuild))
					return
				}
				assert.Equal(t, store.FoundryRebuilds{Samples: []string{f.ontology.ID}}, got)
				assert.Equal(t, model.FoundryGraphJobCancelled, job.Status)
				assert.Contains(t, *job.Error, "sample")
				assert.Nil(t, f.liveJob(t), "no delete job is queued for it")
				assert.Equal(t, model.FoundryLoadDeleted, f.loadStatus(t, rebuild))
				for _, table := range []string{"foundry_load_access", "foundry_load_decisions", "foundry_load_documents"} {
					var n int
					require.NoError(t, testDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE load_id=?`, rebuild).Scan(&n))
					assert.Zero(t, n, table)
				}
				loads, err := db.ListFoundryLoads(ctx, f.ontology.ID)
				require.NoError(t, err)
				require.Len(t, loads, 1)
				assert.Equal(t, current, loads[0].ID)
				assert.Equal(t, current, *f.ontologyNow(t).CurrentLoadID, "the sample's load stays current")
				claimed, err := db.ClaimFoundryGraphJob(ctx, "w3", time.Minute)
				require.NoError(t, err)
				assert.Nil(t, claimed, "nothing left to run")

				// Run again: still skipped, nothing withdrawn twice.
				got, err = store.RebuildFoundryLoads(ctx, db, cutover)
				require.NoError(t, err)
				assert.Equal(t, store.FoundryRebuilds{Samples: []string{f.ontology.ID}}, got)
			})
		}
	})

	// Only a rebuild is withdrawn: a person's load of a sample is theirs.
	t.Run("leaves a sample's other live job alone", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		f := earlierLoad(t, db, id26("FWS-RBS3"), "RBS3", true)
		userLoad := f.load(t, "U-RBS3")
		done, err := db.QueueFoundryRebuild(ctx, f.ontology.WorkspaceID, f.ontology.ID, cutover, rebuildJob("s3"))
		require.NoError(t, err)
		assert.Equal(t, store.FoundryRebuildSample, done)
		live := f.liveJob(t)
		require.NotNil(t, live)
		assert.Equal(t, userLoad, live.LoadID)
		assert.Equal(t, model.FoundryGraphJobQueued, live.Status)
	})

	// A run over a mix queues only the customers' loads, and withdraws the
	// samples' rebuilds an earlier run queued ahead of them, so workers
	// claim the customers' rebuilds and nothing else.
	t.Run("leaves only the customers' rebuilds to claim", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		ws := id26("FWS-RBQ")
		samples := []foundryFixture{earlierLoad(t, db, ws, "RBQ0", false), earlierLoad(t, db, ws, "RBQ3", false)}
		for i, s := range samples {
			done, err := db.QueueFoundryRebuild(ctx, ws, s.ontology.ID, cutover, rebuildJob(fmt.Sprint("q", i)))
			require.NoError(t, err)
			require.Equal(t, store.FoundryRebuildQueued, done)
		}
		sampled(t, "RBQ0")
		sampled(t, "RBQ3")
		a := earlierLoad(t, db, ws, "RBQ1", false)
		b := earlierLoad(t, db, ws, "RBQ2", false)

		got, err := store.RebuildFoundryLoads(ctx, db, cutover)
		require.NoError(t, err)
		assert.Equal(t, []string{a.ontology.ID, b.ontology.ID}, got.Queued)
		assert.Equal(t, []string{samples[0].ontology.ID, samples[1].ontology.ID}, got.Samples)
		assert.Empty(t, got.UnderWay)
		assert.Empty(t, got.Busy)
		var claimed []string
		for i := range 3 {
			job, err := db.ClaimFoundryGraphJob(ctx, fmt.Sprint("w", i), time.Minute)
			require.NoError(t, err)
			if job == nil {
				break
			}
			require.NotNil(t, job.ClientToken)
			assert.Equal(t, model.FoundryRebuildToken+job.ID, *job.ClientToken)
			claimed = append(claimed, job.OntologyID)
		}
		assert.Equal(t, []string{a.ontology.ID, b.ontology.ID}, claimed)
	})
}

// Graph workers on several replicas claim from one queue: each job goes to
// one worker, however many claim at once, and every due job is claimed.
func TestFoundryGraphJobClaimsUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws := id26("FWS-RBC")
	const ontologies, workers = 12, 6
	want := map[string]bool{}
	for i := range ontologies {
		f := earlierLoad(t, db, ws, fmt.Sprintf("RBC%02d", i), false)
		done, err := db.QueueFoundryRebuild(ctx, ws, f.ontology.ID, cutover, rebuildJob(fmt.Sprintf("C%02d", i)))
		require.NoError(t, err)
		require.Equal(t, store.FoundryRebuildQueued, done)
		want[id26(fmt.Sprintf("RB-C%02d", i))] = true
	}
	var mu sync.Mutex
	owners := map[string][]string{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker := fmt.Sprint("replica-", w)
			<-start
			for {
				job, err := db.ClaimFoundryGraphJob(ctx, worker, time.Minute)
				if !assert.NoError(t, err) || job == nil {
					return
				}
				mu.Lock()
				owners[job.ID] = append(owners[job.ID], worker)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	require.Len(t, owners, ontologies, "every due job is claimed")
	for jobID, by := range owners {
		assert.True(t, want[jobID], "claimed %s", jobID)
		assert.Len(t, by, 1, "job %s claimed by %v", jobID, by)
		var owner string
		require.NoError(t, testDB.QueryRowContext(ctx, `SELECT lease_owner FROM foundry_load_jobs WHERE id=? AND status='running'`, jobID).Scan(&owner))
		assert.Equal(t, by[0], owner, "the lease is the claimer's")
	}
}

func mustLoad(t *testing.T, f foundryFixture, id string) *model.FoundryLoad {
	t.Helper()
	l, err := f.db.GetFoundryLoad(context.Background(), f.ontology.ID, id)
	require.NoError(t, err)
	return l
}

// rebuildStore is a Store whose rebuild list and queue are canned: pages of
// ontologies by id, and an outcome for each.
type rebuildStore struct {
	store.Unimplemented
	ids     []string
	outcome map[string]func() (store.FoundryRebuild, error)
	listErr error
	asked   []string
}

func (s *rebuildStore) ListFoundryRebuilds(_ context.Context, before time.Time, afterID string, limit int) ([]*model.FoundryOntology, error) {
	if !before.Equal(cutover) {
		return nil, fmt.Errorf("before %v", before)
	}
	if s.listErr != nil && afterID != "" {
		return nil, s.listErr
	}
	var page []*model.FoundryOntology
	for _, id := range s.ids {
		if id > afterID && len(page) < limit {
			page = append(page, &model.FoundryOntology{ID: id, WorkspaceID: "ws"})
		}
	}
	return page, nil
}

func (s *rebuildStore) QueueFoundryRebuild(_ context.Context, workspaceID, ontologyID string, before time.Time,
	job *model.FoundryGraphJob) (store.FoundryRebuild, error) {
	if workspaceID != "ws" || !before.Equal(cutover) || len(job.ID) != 26 {
		return 0, fmt.Errorf("asked %s %s %v %q", workspaceID, ontologyID, before, job.ID)
	}
	s.asked = append(s.asked, ontologyID)
	if f, ok := s.outcome[ontologyID]; ok {
		return f()
	}
	return store.FoundryRebuildQueued, nil
}

// RebuildFoundryLoads pages past a full page, goes on past an ontology it
// can't queue, and says what it did.
func TestRebuildFoundryLoadsPages(t *testing.T) {
	ctx := context.Background()
	s := &rebuildStore{outcome: map[string]func() (store.FoundryRebuild, error){
		"o003": func() (store.FoundryRebuild, error) { return store.FoundryRebuildBusy, nil },
		"o004": func() (store.FoundryRebuild, error) { return store.FoundryRebuildNotOwed, nil },
		"o007": func() (store.FoundryRebuild, error) { return store.FoundryRebuildUnderWay, nil },
		"o005": func() (store.FoundryRebuild, error) { return 0, store.ErrNotFound },
		"o006": func() (store.FoundryRebuild, error) { return 0, errors.New("boom") },
		"o008": func() (store.FoundryRebuild, error) { return store.FoundryRebuildGaveUp, nil },
	}}
	for i := range 101 {
		s.ids = append(s.ids, fmt.Sprintf("o%03d", i))
	}
	got, err := store.RebuildFoundryLoads(ctx, s, cutover)
	require.ErrorContains(t, err, "rebuild ontology o006: boom")
	assert.Equal(t, s.ids, s.asked, "every ontology once, past the first page")
	assert.Equal(t, []string{"o003"}, got.Busy)
	assert.Equal(t, []string{"o007"}, got.UnderWay)
	assert.Equal(t, []string{"o008"}, got.GaveUp)
	assert.Len(t, got.Queued, 101-6)
	assert.Equal(t, "o100", got.Queued[len(got.Queued)-1])

	// A page that can't be read stops the run, with what it did so far.
	s = &rebuildStore{ids: s.ids, listErr: errors.New("gone")}
	got, err = store.RebuildFoundryLoads(ctx, s, cutover)
	require.ErrorContains(t, err, "list ontologies to rebuild: gone")
	assert.Len(t, got.Queued, 100)

	// Nothing owed: nothing done.
	got, err = store.RebuildFoundryLoads(ctx, &rebuildStore{}, cutover)
	require.NoError(t, err)
	assert.Equal(t, store.FoundryRebuilds{}, got)
}
