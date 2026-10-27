package store_test

import (
	"context"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimed claims the due job as worker and checks it is the one expected.
func (f foundryFixture) claimed(t *testing.T, worker, jobID string) *model.FoundryGraphJob {
	t.Helper()
	job, err := f.db.ClaimFoundryGraphJob(context.Background(), worker, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, job, "a job is due")
	require.Equal(t, jobID, job.ID)
	return job
}

// ended ends a claimed job with status, handing it next.
func (f foundryFixture) ended(t *testing.T, worker, jobID string, status model.FoundryGraphJobStatus, next model.FoundryGraphNext) model.FoundryGraphJobFinish {
	t.Helper()
	finish, err := f.db.FinishFoundryGraphJob(context.Background(), jobID, worker, model.FoundryGraphJobOutcome{Status: status, Next: next})
	require.NoError(t, err)
	return finish
}

func TestFoundryLoadStatus(t *testing.T) {
	// A load the user asked for serves reads once its job succeeds.
	t.Run("makes a succeeded load ready and current", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "LS1")
		loadID := f.load(t, "L1")
		load, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, loadID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryLoadWriting, load.Status)
		assert.Nil(t, load.FinishedAt)
		assert.Equal(t, 1, load.Version)
		assert.Equal(t, "foundry-v1", load.OntologyVersion())
		assert.Nil(t, f.ontologyNow(t).CurrentLoadID, "a load being written serves no reads")

		f.loaded(t, "w1")
		load, err = f.db.GetFoundryLoad(ctx, f.ontology.ID, loadID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryLoadReady, load.Status)
		assert.NotNil(t, load.FinishedAt)
		assert.Nil(t, load.DeletedAt)
		require.NotNil(t, f.ontologyNow(t).CurrentLoadID)
		assert.Equal(t, loadID, *f.ontologyNow(t).CurrentLoadID)
		_, err = f.db.GetFoundryLoad(ctx, id26("FO-elsewhere"), loadID)
		assert.ErrorIs(t, err, store.ErrNotFound, "loads are scoped to their ontology")
	})

	// A budget stop leaves a partial load: it serves reads when nothing else
	// does, and never replaces a load that is current. A failed load never does.
	t.Run("makes a partial load current only without a current load", func(t *testing.T) {
		f := newFoundryFixture(t, "LS2")
		first := f.load(t, "L1")
		f.finish(t, "w1", "a", model.FoundryGraphJobBudgetExhausted)
		assert.Equal(t, model.FoundryLoadPartial, f.loadStatus(t, first))
		assert.Equal(t, first, *f.ontologyNow(t).CurrentLoadID, "a first partial load still serves reads and takes uploads")

		ready := f.load(t, "L2")
		f.finish(t, "w1", "b", model.FoundryGraphJobSucceeded)
		assert.Equal(t, ready, *f.ontologyNow(t).CurrentLoadID, "a ready load replaces a partial one")

		partial := f.load(t, "L3")
		f.finish(t, "w1", "c", model.FoundryGraphJobBudgetExhausted)
		assert.Equal(t, model.FoundryLoadPartial, f.loadStatus(t, partial))
		assert.Equal(t, ready, *f.ontologyNow(t).CurrentLoadID, "a later partial load does not replace a ready current load")

		failed := f.load(t, "L4")
		f.finish(t, "w1", "d", model.FoundryGraphJobFailed)
		assert.Equal(t, model.FoundryLoadFailed, f.loadStatus(t, failed))
		assert.Equal(t, ready, *f.ontologyNow(t).CurrentLoadID)
	})

	// A refresh whose budget ran out replaces the load it refreshed once it
	// holds what that load held: every document that load recorded which is
	// still uploaded and of the refresh's own membership. So a refresh that
	// replayed everything at $0 is current, and one that lost a replay, or
	// recorded nothing, is not.
	t.Run("makes a partial refresh current once it holds what it replaces", func(t *testing.T) {
		ctx := context.Background()
		moveOut := func(t *testing.T, f foundryFixture, tag, doc string) {
			other := f.documentSet(tag + "-other")
			require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, other, 100))
			_, _, err := f.db.MoveFoundryDocument(ctx, f.ontology.WorkspaceID, doc, other.ID, f.fan(tag+"-move"))
			require.NoError(t, err)
		}
		deleted := func(t *testing.T, f foundryFixture, _, doc string) {
			require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, doc))
		}
		for i, tc := range []struct {
			name string
			// beforeRefresh and whileRefreshing change document b before the
			// refresh is queued, and while it runs.
			beforeRefresh, whileRefreshing func(t *testing.T, f foundryFixture, tag, doc string)
			// recorded are the documents the refresh records, of a and b.
			recorded []string
			current  bool
		}{
			{name: "it recorded every one", recorded: []string{"a", "b"}, current: true},
			{name: "it missed one", recorded: []string{"a"}},
			{name: "it recorded none"},
			{name: "the one it missed moved out of its sets before it", beforeRefresh: moveOut, recorded: []string{"a"}, current: true},
			{name: "the one it missed was deleted", whileRefreshing: deleted, recorded: []string{"a"}, current: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tag := fmt.Sprintf("LS4-%d", i)
				f := newFoundryFixture(t, tag)
				ws, o := f.ontology.WorkspaceID, f.ontology.ID
				docs := map[string]string{"a": f.uploaded(t, "a"), "b": f.uploaded(t, "b")}
				first := f.load(t, "L1")
				f.loaded(t, "w1", docs["a"], docs["b"])
				if tc.beforeRefresh != nil {
					tc.beforeRefresh(t, f, tag, docs["b"])
				}
				_, err := testDB.ExecContext(ctx, `UPDATE foundry_ontologies SET refresh_due_at=NOW(6) - INTERVAL 1 HOUR WHERE id=?`, o)
				require.NoError(t, err)
				queued, err := f.db.QueueFoundryRefresh(ctx, ws, o, refreshJob(tag))
				require.NoError(t, err)
				require.True(t, queued)
				if tc.whileRefreshing != nil {
					tc.whileRefreshing(t, f, tag, docs["b"])
				}
				var recorded []string
				for _, name := range tc.recorded {
					recorded = append(recorded, docs[name])
				}
				job, finish := f.finish(t, "w2", tag, model.FoundryGraphJobBudgetExhausted, recorded...)
				require.Equal(t, id26("RJ-"+tag), job.ID)
				require.Equal(t, model.FoundryGraphJobEnded, finish)
				assert.Equal(t, model.FoundryLoadPartial, f.loadStatus(t, job.LoadID))
				want := first
				if tc.current {
					want = job.LoadID
				}
				assert.Equal(t, want, *f.ontologyNow(t).CurrentLoadID)
				assert.Equal(t, model.FoundryLoadReady, f.loadStatus(t, first), "the customer's own load is kept")
			})
		}
	})

	// A refresh whose budget stop left a set with an allowance unfinished
	// (HeldSets, an API source's) replaces the load it refreshed only once
	// it recorded every document of that set the load recorded, those
	// deleted since included: a window's trimmed pieces keep their entities
	// until a run with budget extracts what replaced them. One that would
	// drop some never becomes current, so it is deleted. Held sets change
	// nothing for the other sets, and the rule for a still-uploaded
	// document stays.
	t.Run("keeps the load a partial refresh would drop a held set's documents of", func(t *testing.T) {
		ctx := context.Background()
		for i, tc := range []struct {
			name string
			// deleteB deletes b, as a window's trim does, while the refresh
			// runs; held names the sets the refresh's stop left unfinished.
			deleteB  bool
			held     func(o *model.FoundryOntology) []string
			recorded []string
			want     model.FoundryLoadStatus
			current  bool
		}{
			{"it dropped a deleted document of the held set", true, func(o *model.FoundryOntology) []string { return []string{generalSetID(o.ID)} },
				[]string{"a"}, model.FoundryLoadDeleting, false},
			{"it dropped one of a set it did not hold", true, func(*model.FoundryOntology) []string { return []string{id26("FS-other")} },
				[]string{"a"}, model.FoundryLoadPartial, true},
			{"it recorded every one of the held set", false, func(o *model.FoundryOntology) []string { return []string{generalSetID(o.ID)} },
				[]string{"a", "b"}, model.FoundryLoadPartial, true},
			{"it missed an uploaded one, holding nothing", false, func(*model.FoundryOntology) []string { return nil },
				[]string{"a"}, model.FoundryLoadPartial, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tag := fmt.Sprintf("LS5-%d", i)
				f := newFoundryFixture(t, tag)
				ws, o := f.ontology.WorkspaceID, f.ontology.ID
				docs := map[string]string{"a": f.uploaded(t, "a"), "b": f.uploaded(t, "b")}
				first := f.load(t, "L1")
				f.loaded(t, "w1", docs["a"], docs["b"])
				_, err := testDB.ExecContext(ctx, `UPDATE foundry_ontologies SET refresh_due_at=NOW(6) - INTERVAL 1 HOUR WHERE id=?`, o)
				require.NoError(t, err)
				queued, err := f.db.QueueFoundryRefresh(ctx, ws, o, refreshJob(tag))
				require.NoError(t, err)
				require.True(t, queued)
				if tc.deleteB {
					require.NoError(t, f.db.DeleteFoundryDocument(ctx, ws, docs["b"]))
				}
				job, err := f.db.ClaimFoundryGraphJob(ctx, "w2", time.Minute)
				require.NoError(t, err)
				require.Equal(t, id26("RJ-"+tag), job.ID)
				for _, name := range tc.recorded {
					require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w2", extracted(docs[name]), false))
				}
				finish, err := f.db.FinishFoundryGraphJob(ctx, job.ID, "w2", model.FoundryGraphJobOutcome{
					Status: model.FoundryGraphJobBudgetExhausted, IngestDone: true, HeldSets: tc.held(f.ontology), Next: next(tag),
				})
				require.NoError(t, err)
				require.Equal(t, model.FoundryGraphJobEnded, finish)
				assert.Equal(t, tc.want, f.loadStatus(t, job.LoadID))
				want := first
				if tc.current {
					want = job.LoadID
				}
				assert.Equal(t, want, *f.ontologyNow(t).CurrentLoadID)
				assert.Equal(t, model.FoundryLoadReady, f.loadStatus(t, first), "the load it refreshed is kept")
			})
		}
	})
	t.Run("refuses held sets on an outcome other than a budget stop", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "LS6")
		f.load(t, "L1")
		job, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		retry := time.Now().Add(time.Minute)
		for _, outcome := range []model.FoundryGraphJobOutcome{
			{Status: model.FoundryGraphJobSucceeded, HeldSets: []string{"set"}, Next: next("LS6")},
			{Status: model.FoundryGraphJobFailed, HeldSets: []string{"set"}, Next: next("LS6")},
			{RetryAt: &retry, HeldSets: []string{"set"}},
		} {
			_, err := f.db.FinishFoundryGraphJob(ctx, job.ID, "w1", outcome)
			assert.ErrorContains(t, err, "holds sets back", "%+v", outcome)
		}
		assert.Equal(t, model.FoundryLoadWriting, f.loadStatus(t, job.LoadID), "nothing was written")
	})

	// The Loads page shows every load not yet deleted, newest first, with
	// its version and its jobs.
	t.Run("lists loads with their versions and jobs", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "LS3")
		first := f.load(t, "L1")
		f.loaded(t, "w1")
		f.uploaded(t, "a")
		extract := f.liveJob(t)
		require.NotNil(t, extract)
		require.Equal(t, first, extract.LoadID)
		f.version(t, "L2")
		assert.ErrorIs(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "L2")), store.ErrConflict, "an extract is writing")
		f.finish(t, "w1", "a", model.FoundryGraphJobSucceeded, id26("FD-a"))
		require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "L2")))
		second := id26("FJ-L2")

		loads, err := f.db.ListFoundryLoads(ctx, f.ontology.ID)
		require.NoError(t, err)
		require.Len(t, loads, 2)
		assert.Equal(t, second, loads[0].ID, "newest first")
		assert.Equal(t, 2, loads[0].Version)
		assert.Equal(t, model.FoundryLoadWriting, loads[0].Status)
		assert.Equal(t, first, loads[1].ID)
		assert.Equal(t, 1, loads[1].Version)

		jobs, err := f.db.ListFoundryLoadJobs(ctx, []string{first, second, id26("FJ-none")}, 20)
		require.NoError(t, err)
		require.Len(t, jobs[first].Newest, 2)
		assert.Equal(t, extract.ID, jobs[first].Newest[0].ID, "newest first")
		assert.Equal(t, first, jobs[first].Newest[1].ID)
		assert.Equal(t, 2, jobs[first].Count)
		assert.Equal(t, int64(250_000), jobs[first].USDMicros)
		require.Len(t, jobs[second].Newest, 1)
		assert.Equal(t, 1, jobs[second].Count)
		assert.NotContains(t, jobs, id26("FJ-none"))
		empty, err := f.db.ListFoundryLoadJobs(ctx, nil, 20)
		require.NoError(t, err)
		assert.Empty(t, empty)
	})

	// A current load gains an extract per upload for as long as it is
	// current, and the Loads page polls its jobs: only the newest are listed,
	// with the count and spend of them all.
	t.Run("lists a load's newest jobs with the totals of all", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "LS4")
		current := f.load(t, "L1")
		f.loaded(t, "w1")
		var extracts []string
		for _, tag := range []string{"a", "b", "c"} {
			doc := f.uploaded(t, tag)
			job, _ := f.finish(t, "w1", tag, model.FoundryGraphJobSucceeded, doc)
			require.Equal(t, current, job.LoadID)
			extracts = append(extracts, job.ID)
		}
		other := f.load(t, "L2")

		jobs, err := f.db.ListFoundryLoadJobs(ctx, []string{current, other}, 2)
		require.NoError(t, err)
		require.Len(t, jobs[current].Newest, 2, "only the newest two")
		assert.Equal(t, extracts[2], jobs[current].Newest[0].ID)
		assert.Equal(t, extracts[1], jobs[current].Newest[1].ID)
		assert.Equal(t, 4, jobs[current].Count, "the load job and three extracts")
		assert.Equal(t, int64(3*250_000), jobs[current].USDMicros)
		require.Len(t, jobs[other].Newest, 1, "each load has its own limit")
		assert.Equal(t, 1, jobs[other].Count)

		_, err = f.db.ListFoundryLoadJobs(ctx, []string{current}, 0)
		assert.Error(t, err, "a load lists at least one job")
	})
}

func TestFoundryLoadRetention(t *testing.T) {
	// Up to RetainLoads loads stay: the current one whatever its age, then
	// the newest ready, partial and failed loads. The rest are deleted, oldest first.
	t.Run("keeps the newest loads and the current one", func(t *testing.T) {
		f := newFoundryFixture(t, "RT1")
		keep3 := func(tag string) model.FoundryGraphNext {
			n := next(tag)
			n.RetainLoads = 3
			return n
		}
		current := f.load(t, "L1")
		f.ended(t, "w1", f.claimed(t, "w1", current).ID, model.FoundryGraphJobSucceeded, keep3("a"))
		var failed []string
		for _, tag := range []string{"L2", "L3", "L4"} {
			id := f.load(t, tag)
			f.ended(t, "w1", f.claimed(t, "w1", id).ID, model.FoundryGraphJobFailed, keep3(tag))
			failed = append(failed, id)
		}
		assert.Equal(t, current, *f.ontologyNow(t).CurrentLoadID)
		assert.Equal(t, model.FoundryLoadReady, f.loadStatus(t, current), "the current load is kept, oldest as it is")
		assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, failed[0]), "failed loads count toward the loads kept")
		assert.Equal(t, model.FoundryLoadFailed, f.loadStatus(t, failed[1]))
		assert.Equal(t, model.FoundryLoadFailed, f.loadStatus(t, failed[2]))
		del := f.liveJob(t)
		require.NotNil(t, del)
		assert.Equal(t, id26("FN-D-L4"), del.ID)
		assert.Equal(t, model.FoundryGraphJobDelete, del.Kind)
		assert.Equal(t, model.FoundryGraphJobDeleting, del.Phase)
		assert.Equal(t, failed[0], del.LoadID)
		assert.Equal(t, id26("FV-L2"), del.VersionID)
	})

	// With the default of five, a sixth load deletes the oldest.
	t.Run("deletes the oldest when a sixth load ends", func(t *testing.T) {
		f := newFoundryFixture(t, "RT2")
		var loads []string
		for i := range 6 {
			tag := fmt.Sprintf("L%d", i)
			loads = append(loads, f.load(t, tag))
			f.finish(t, "w1", tag, model.FoundryGraphJobSucceeded)
			if i < 5 {
				assert.Nil(t, f.liveJob(t), "load %d: five loads are kept", i)
			}
		}
		assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, loads[0]))
		for _, id := range loads[1:] {
			assert.Equal(t, model.FoundryLoadReady, f.loadStatus(t, id))
		}
		require.NotNil(t, f.liveJob(t))
		assert.Equal(t, loads[0], f.liveJob(t).LoadID)
	})
}

func TestFoundryLoadDeletes(t *testing.T) {
	// A delete job's done is the load gone: its rows go with it, and the next
	// load being deleted is queued, oldest first.
	t.Run("deletes a load and its extraction rows, then the next", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DL1")
		doc := f.uploaded(t, "a")
		older := f.load(t, "L1")
		f.loaded(t, "w1", doc)
		newer := f.load(t, "L2")
		f.loaded(t, "w2", doc)
		current := f.load(t, "L3")
		f.loaded(t, "w3", doc)
		require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, newer, id26("FN-D-user1")))
		require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, older, id26("FN-D-user2")))
		del := f.liveJob(t)
		require.NotNil(t, del)
		assert.Equal(t, id26("FN-D-user1"), del.ID, "the first delete was queued, the second waits for it")
		assert.Equal(t, newer, del.LoadID)

		f.ended(t, "w1", f.claimed(t, "w1", del.ID).ID, model.FoundryGraphJobSucceeded, next("a"))
		load, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, newer)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryLoadDeleted, load.Status)
		assert.NotNil(t, load.DeletedAt)
		_, err = f.db.GetFoundryDocumentExtraction(ctx, newer, doc)
		assert.ErrorIs(t, err, store.ErrNotFound, "the deleted load's rows are gone")
		_, err = f.db.GetFoundryDocumentExtraction(ctx, older, doc)
		require.NoError(t, err, "another load's rows stay")
		del = f.liveJob(t)
		require.NotNil(t, del)
		assert.Equal(t, id26("FN-D-a"), del.ID)
		assert.Equal(t, older, del.LoadID)

		f.ended(t, "w1", f.claimed(t, "w1", del.ID).ID, model.FoundryGraphJobSucceeded, next("b"))
		assert.Nil(t, f.liveJob(t), "nothing left to delete")
		loads, err := f.db.ListFoundryLoads(ctx, f.ontology.ID)
		require.NoError(t, err)
		require.Len(t, loads, 1, "deleted loads are not listed")
		assert.Equal(t, current, loads[0].ID)
	})

	// A delete that failed for good stops; only the user deleting the load
	// again retries it, so a load that can't be deleted never loops.
	t.Run("leaves a failed delete delete_failed until deleted again", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DL2")
		stale := f.load(t, "L1")
		f.loaded(t, "w1")
		f.load(t, "L2")
		f.loaded(t, "w1")
		require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, stale, id26("FN-D-user")))
		f.ended(t, "w1", f.claimed(t, "w1", id26("FN-D-user")).ID, model.FoundryGraphJobFailed, next("a"))
		assert.Equal(t, model.FoundryLoadDeleteFailed, f.loadStatus(t, stale))
		assert.Nil(t, f.liveJob(t), "a failed delete is not requeued")
		_, err := f.db.FinishFoundryGraphJob(ctx, id26("FN-D-user"), "w1", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobBudgetExhausted, Next: next("b"),
		})
		assert.Error(t, err, "and is ended once")

		require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, stale, id26("FN-D-retry")))
		assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, stale))
		retry := f.liveJob(t)
		require.NotNil(t, retry)
		assert.Equal(t, id26("FN-D-retry"), retry.ID)
		assert.Equal(t, stale, retry.LoadID)
	})

	// A delete never spends the budget it has none of.
	t.Run("refuses a budget stop of a delete", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DL3")
		stale := f.load(t, "L1")
		f.loaded(t, "w1")
		f.load(t, "L2")
		f.loaded(t, "w1")
		require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, stale, id26("FN-D-user")))
		f.claimed(t, "w1", id26("FN-D-user"))
		_, err := f.db.FinishFoundryGraphJob(ctx, id26("FN-D-user"), "w1", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobBudgetExhausted, Next: next("a"),
		})
		assert.Error(t, err)
		assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, stale))
	})

	// The current load serves reads and a load being written is a job's; the
	// user can delete neither. A load already being deleted stays so.
	t.Run("refuses to delete the current load or one being written", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DL4")
		ws, o := f.ontology.WorkspaceID, f.ontology.ID
		first := f.load(t, "L1")
		f.loaded(t, "w1")
		current := f.load(t, "L2")
		f.loaded(t, "w1")
		assert.ErrorIs(t, f.db.DeleteFoundryLoad(ctx, ws, o, current, id26("FN-D-1")), store.FoundryLoadIsCurrent)
		writing := f.load(t, "L3")
		assert.ErrorIs(t, f.db.DeleteFoundryLoad(ctx, ws, o, writing, id26("FN-D-2")), store.FoundryLoadBusy)
		f.finish(t, "w1", "a", model.FoundryGraphJobFailed)

		// An extract into the first load, made current again, targets it.
		_, err := f.db.SetCurrentFoundryLoad(ctx, ws, o, first, extractJob("sc"), noBudget)
		require.NoError(t, err)
		f.uploaded(t, "a")
		require.Equal(t, first, f.liveJob(t).LoadID)
		_, err = f.db.SetCurrentFoundryLoad(ctx, ws, o, current, extractJob("sc2"), noBudget)
		require.NoError(t, err)
		assert.ErrorIs(t, f.db.DeleteFoundryLoad(ctx, ws, o, first, id26("FN-D-3")), store.FoundryLoadBusy, "a live job targets it")

		// Another load is marked now and deleted after the live job.
		require.NoError(t, f.db.DeleteFoundryLoad(ctx, ws, o, writing, id26("FN-D-4")))
		assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, writing))
		assert.Equal(t, model.FoundryGraphJobExtract, f.liveJob(t).Kind, "the live job stays")
		require.NoError(t, f.db.DeleteFoundryLoad(ctx, ws, o, writing, id26("FN-D-5")), "deleting a load being deleted changes nothing")

		assert.ErrorIs(t, f.db.DeleteFoundryLoad(ctx, ws, o, id26("FJ-none"), id26("FN-D-6")), store.ErrNotFound)
		assert.ErrorIs(t, f.db.DeleteFoundryLoad(ctx, ws, id26("FO-other"), first, id26("FN-D-7")), store.ErrNotFound)
	})

	// A new load is what the user waits for; a delete only frees space. The
	// delete gives way and resumes after the load.
	t.Run("lets a new load preempt a delete", func(t *testing.T) {
		for _, running := range []bool{false, true} {
			t.Run(fmt.Sprintf("running=%t", running), func(t *testing.T) {
				ctx := context.Background()
				f := newFoundryFixture(t, "DL5")
				stale := f.load(t, "L1")
				f.loaded(t, "w1")
				f.load(t, "L2")
				f.loaded(t, "w1")
				doc := f.uploaded(t, "a")
				f.finish(t, "w1", "x", model.FoundryGraphJobSucceeded, doc)
				require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, stale, id26("FN-D-user")))
				if running {
					f.claimed(t, "w1", id26("FN-D-user"))
				}

				fresh := f.load(t, "L3")
				cancelled, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, id26("FN-D-user"))
				require.NoError(t, err)
				assert.Equal(t, model.FoundryGraphJobCancelled, cancelled.Status)
				assert.NotNil(t, cancelled.FinishedAt)
				assert.Contains(t, *cancelled.Error, "resumes")
				assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, stale), "the load waits, still deleting")
				assert.Equal(t, fresh, f.liveJob(t).ID)
				if running {
					// The worker deleting finds its lease gone and records nothing.
					assert.ErrorIs(t, f.db.RenewFoundryGraphJobLease(ctx, cancelled.ID, "w1", time.Minute), store.ErrNotFound)
					_, err := f.db.FinishFoundryGraphJob(ctx, cancelled.ID, "w1", model.FoundryGraphJobOutcome{
						Status: model.FoundryGraphJobSucceeded, Next: next("late"),
					})
					assert.ErrorIs(t, err, store.ErrNotFound)
					assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, stale))
				}

				f.finish(t, "w2", "after", model.FoundryGraphJobSucceeded, doc)
				resumed := f.liveJob(t)
				require.NotNil(t, resumed)
				assert.Equal(t, id26("FN-D-after"), resumed.ID)
				assert.Equal(t, stale, resumed.LoadID, "the delete resumes after the load")
			})
		}
	})

	// The user waits for an upload; a delete does not. An upload during a
	// delete is extracted right after it, once, and the deletes go on after.
	t.Run("extracts an upload during a delete exactly once", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DL6")
		var stale []string
		for _, tag := range []string{"L1", "L2"} {
			stale = append(stale, f.load(t, tag))
			f.loaded(t, "w1")
		}
		current := f.load(t, "L3")
		f.loaded(t, "w1")
		for i, id := range stale {
			require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, id, id26(fmt.Sprintf("FN-D-user%d", i))))
		}
		f.claimed(t, "w1", id26("FN-D-user0"))
		doc := f.uploaded(t, "a")
		assert.Equal(t, model.FoundryGraphJobDelete, f.liveJob(t).Kind, "the upload waits for the delete")
		assert.True(t, f.ontologyNow(t).ExtractOwed)

		f.ended(t, "w1", id26("FN-D-user0"), model.FoundryGraphJobSucceeded, next("a"))
		extract := f.liveJob(t)
		require.NotNil(t, extract)
		assert.Equal(t, model.FoundryGraphJobExtract, extract.Kind, "the upload goes before the next delete")
		assert.Equal(t, current, extract.LoadID)
		assert.False(t, f.ontologyNow(t).ExtractOwed)

		f.finish(t, "w1", "b", model.FoundryGraphJobSucceeded, doc)
		del := f.liveJob(t)
		require.NotNil(t, del)
		assert.Equal(t, model.FoundryGraphJobDelete, del.Kind, "then the deletes go on")
		assert.Equal(t, stale[1], del.LoadID)
		var extracts int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_jobs WHERE ontology_id=? AND kind='extract'`, f.ontology.ID).Scan(&extracts))
		assert.Equal(t, 1, extracts)
	})
}

func TestFoundryLoadSetCurrent(t *testing.T) {
	// Going back to an older load must not leave it without the documents
	// uploaded since; with a job live, that job's end catches it up.
	t.Run("catches a load made current up after the live job", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SC1")
		ws, o := f.ontology.WorkspaceID, f.ontology.ID
		old := f.load(t, "L1")
		f.loaded(t, "w1")
		newer := f.load(t, "L2")
		f.loaded(t, "w1")
		doc := f.uploaded(t, "a")
		extract := f.liveJob(t)
		require.NotNil(t, extract)
		require.Equal(t, newer, extract.LoadID)

		queued, err := f.db.SetCurrentFoundryLoad(ctx, ws, o, old, extractJob("sc"), noBudget)
		require.NoError(t, err)
		assert.False(t, queued, "a job is live")
		assert.Equal(t, old, *f.ontologyNow(t).CurrentLoadID)
		assert.True(t, f.ontologyNow(t).ExtractOwed)

		_, finish := f.finish(t, "w1", "a", model.FoundryGraphJobSucceeded, doc)
		assert.Equal(t, model.FoundryGraphJobEnded, finish)
		catchUp := f.liveJob(t)
		require.NotNil(t, catchUp)
		assert.Equal(t, id26("FN-X-a"), catchUp.ID)
		assert.Equal(t, old, catchUp.LoadID)
		assert.Equal(t, id26("FV-L1"), catchUp.VersionID)
		pending, err := f.db.ListPendingFoundryDocuments(ctx, old)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		assert.Equal(t, doc, pending[0].ID)
	})

	// With nothing live the catch-up is queued at once, and only when a document waits.
	t.Run("queues a catch-up only when a document waits", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SC2")
		ws, o := f.ontology.WorkspaceID, f.ontology.ID
		a := f.uploaded(t, "a")
		old := f.load(t, "L1")
		f.loaded(t, "w1", a)
		newer := f.load(t, "L2")
		f.loaded(t, "w1", a)
		queued, err := f.db.SetCurrentFoundryLoad(ctx, ws, o, old, extractJob("sc1"), noBudget)
		require.NoError(t, err)
		assert.False(t, queued, "the old load holds every document")
		assert.Nil(t, f.liveJob(t))

		// A document only the newer load holds.
		_, err = f.db.SetCurrentFoundryLoad(ctx, ws, o, newer, extractJob("sc2"), noBudget)
		require.NoError(t, err)
		b := f.uploaded(t, "b")
		f.finish(t, "w1", "b", model.FoundryGraphJobSucceeded, b)
		require.Nil(t, f.liveJob(t))

		queued, err = f.db.SetCurrentFoundryLoad(ctx, ws, o, old, extractJob("sc3"), model.FoundryGraphBudget{Since: time.Unix(0, 0), CapMicros: 0})
		require.NoError(t, err)
		assert.False(t, queued, "the month is spent")
		assert.Equal(t, old, *f.ontologyNow(t).CurrentLoadID, "the load is current all the same")

		queued, err = f.db.SetCurrentFoundryLoad(ctx, ws, o, old, extractJob("sc4"), noBudget)
		require.NoError(t, err)
		assert.True(t, queued)
		catchUp := f.liveJob(t)
		require.NotNil(t, catchUp)
		assert.Equal(t, id26("FX-sc4"), catchUp.ID)
		assert.Equal(t, old, catchUp.LoadID)
		assert.Equal(t, model.FoundryGraphJobExtract, catchUp.Kind)
	})

	// Only a load whose tables are in serves reads.
	t.Run("refuses a load that can't serve reads", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SC3")
		ws, o := f.ontology.WorkspaceID, f.ontology.ID
		first := f.load(t, "L1")
		f.loaded(t, "w1")
		f.load(t, "L2")
		f.loaded(t, "w1")
		failed := f.load(t, "L3")
		_, err := f.db.SetCurrentFoundryLoad(ctx, ws, o, failed, extractJob("a"), noBudget)
		assert.ErrorIs(t, err, store.FoundryLoadNotServable, "a load being written")
		f.finish(t, "w1", "a", model.FoundryGraphJobFailed)
		_, err = f.db.SetCurrentFoundryLoad(ctx, ws, o, failed, extractJob("b"), noBudget)
		assert.ErrorIs(t, err, store.FoundryLoadNotServable, "a failed load")
		require.NoError(t, f.db.DeleteFoundryLoad(ctx, ws, o, first, id26("FN-D-user")))
		_, err = f.db.SetCurrentFoundryLoad(ctx, ws, o, first, extractJob("c"), noBudget)
		assert.ErrorIs(t, err, store.FoundryLoadNotServable, "a load being deleted")
		f.ended(t, "w1", f.claimed(t, "w1", id26("FN-D-user")).ID, model.FoundryGraphJobFailed, next("d"))
		_, err = f.db.SetCurrentFoundryLoad(ctx, ws, o, first, extractJob("e"), noBudget)
		assert.ErrorIs(t, err, store.FoundryLoadNotServable, "a load whose delete failed")
		_, err = f.db.SetCurrentFoundryLoad(ctx, ws, o, id26("FJ-none"), extractJob("f"), noBudget)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.Equal(t, id26("FJ-L2"), *f.ontologyNow(t).CurrentLoadID, "every refusal leaves the current load")
	})

	// Setting a load current and deleting it race: one wins, and the current
	// load is never one being deleted.
	t.Run("never deletes the load a racing set-current makes current", func(t *testing.T) {
		for round := range 6 {
			ctx := context.Background()
			f := newFoundryFixture(t, fmt.Sprintf("SC4-%d", round))
			ws, o := f.ontology.WorkspaceID, f.ontology.ID
			target := f.load(t, "L1")
			f.loaded(t, "w1")
			f.load(t, "L2")
			f.loaded(t, "w1")
			var wg sync.WaitGroup
			var setErr, deleteErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, setErr = f.db.SetCurrentFoundryLoad(ctx, ws, o, target, extractJob(fmt.Sprintf("r%d", round)), noBudget)
			}()
			go func() {
				defer wg.Done()
				deleteErr = f.db.DeleteFoundryLoad(ctx, ws, o, target, id26(fmt.Sprintf("FN-D-r%d", round)))
			}()
			wg.Wait()
			current := *f.ontologyNow(t).CurrentLoadID
			switch {
			case setErr == nil:
				assert.ErrorIs(t, deleteErr, store.FoundryLoadIsCurrent, "round %d", round)
				assert.Equal(t, target, current)
			default:
				assert.ErrorIs(t, setErr, store.FoundryLoadNotServable, "round %d", round)
				require.NoError(t, deleteErr)
				assert.NotEqual(t, target, current)
				assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, target))
			}
		}
	})

	// A load ending and a set-current race: whichever commits last decides
	// the current load, which is always one that serves reads.
	t.Run("keeps a servable current load when set-current races a finish", func(t *testing.T) {
		for round := range 6 {
			ctx := context.Background()
			f := newFoundryFixture(t, fmt.Sprintf("SC5-%d", round))
			ws, o := f.ontology.WorkspaceID, f.ontology.ID
			old := f.load(t, "L1")
			f.loaded(t, "w1")
			fresh := f.load(t, "L2")
			f.claimed(t, "w1", fresh)
			var wg sync.WaitGroup
			var setErr, finishErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, setErr = f.db.SetCurrentFoundryLoad(ctx, ws, o, old, extractJob(fmt.Sprintf("r%d", round)), noBudget)
			}()
			go func() {
				defer wg.Done()
				_, finishErr = f.db.FinishFoundryGraphJob(ctx, fresh, "w1", model.FoundryGraphJobOutcome{
					Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next(fmt.Sprintf("r%d", round)),
				})
			}()
			wg.Wait()
			require.NoError(t, setErr)
			require.NoError(t, finishErr)
			current := *f.ontologyNow(t).CurrentLoadID
			assert.Contains(t, []string{old, fresh}, current, "round %d", round)
			assert.True(t, f.loadStatus(t, current).Servable(), "round %d", round)
		}
	})
}

func TestFoundryOntologyDelete(t *testing.T) {
	// Deleting an ontology deletes its loads from the user's database, and
	// only then lets another ontology write the database: a new one bound
	// meanwhile would see the old ontology's loads as its own data.
	t.Run("drains its loads and holds the database meanwhile", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "OD1")
		ws, o := f.ontology.WorkspaceID, f.ontology.ID
		current := f.load(t, "L1")
		f.loaded(t, "w1")
		failed := f.load(t, "L2")
		f.finish(t, "w1", "a", model.FoundryGraphJobFailed)
		writing := f.load(t, "L3")
		f.claimed(t, "w1", writing)
		f.uploaded(t, "a")

		require.NoError(t, f.db.DeleteFoundryOntology(ctx, ws, o, id26("FN-D-onto")))
		cancelled, err := f.db.GetFoundryGraphJob(ctx, o, writing)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobCancelled, cancelled.Status, "the running load is cancelled, not waited for")
		assert.ErrorIs(t, f.db.RenewFoundryGraphJobLease(ctx, writing, "w1", time.Minute), store.ErrNotFound)
		assert.ErrorIs(t, f.db.RecordFoundryDocumentExtraction(ctx, writing, "w1", extracted(id26("FD-a")), false), store.ErrNotFound)
		deleted := f.ontologyNow(t)
		assert.NotNil(t, deleted.DeletedAt)
		assert.Nil(t, deleted.CurrentLoadID)
		assert.False(t, deleted.ExtractOwed)
		for _, id := range []string{current, failed, writing} {
			assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, id))
		}
		load, err := f.db.GetFoundryLoad(ctx, o, writing)
		require.NoError(t, err)
		assert.NotNil(t, load.FinishedAt, "writing ended with the ontology")

		holder, err := f.db.GetFoundryOntologyByTenant(ctx, f.ontology.TenantID)
		require.NoError(t, err)
		assert.Equal(t, o, holder.ID, "the deleted ontology holds its database while its loads drain")
		rebind := *f.ontology
		rebind.ID, rebind.ClientToken = id26("FO-OD1-new"), "ontology-OD1-new"
		assert.ErrorIs(t, f.db.CreateFoundryOntology(ctx, &rebind), store.ErrConflict)

		del := f.liveJob(t)
		require.NotNil(t, del)
		assert.Equal(t, id26("FN-D-onto"), del.ID)
		assert.Equal(t, current, del.LoadID, "oldest first")
		f.ended(t, "w1", f.claimed(t, "w1", del.ID).ID, model.FoundryGraphJobSucceeded, next("b"))
		f.ended(t, "w1", f.claimed(t, "w1", id26("FN-D-b")).ID, model.FoundryGraphJobFailed, next("c"))
		assert.Equal(t, model.FoundryLoadDeleteFailed, f.loadStatus(t, failed), "a failed delete does not stop the drain")
		_, err = f.db.GetFoundryOntologyByTenant(ctx, f.ontology.TenantID)
		require.NoError(t, err, "one load is left")
		last := f.claimed(t, "w1", id26("FN-D-c"))
		assert.Equal(t, writing, last.LoadID)
		f.ended(t, "w1", last.ID, model.FoundryGraphJobSucceeded, next("d"))

		// The failed load is still in the database: a new ontology would
		// take it for its own data.
		assert.Nil(t, f.liveJob(t), "a failed delete is not requeued by itself")
		holder, err = f.db.GetFoundryOntologyByTenant(ctx, f.ontology.TenantID)
		require.NoError(t, err)
		assert.Equal(t, o, holder.ID, "a load whose delete failed holds the database")
		assert.ErrorIs(t, f.db.CreateFoundryOntology(ctx, &rebind), store.ErrConflict)

		require.NoError(t, f.db.ResumeFoundryOntologyDrain(ctx, ws, o, id26("FN-D-resume")))
		assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, failed))
		retry := f.claimed(t, "w1", id26("FN-D-resume"))
		assert.Equal(t, failed, retry.LoadID)
		assert.Equal(t, model.FoundryGraphJobDelete, retry.Kind)
		f.ended(t, "w1", retry.ID, model.FoundryGraphJobSucceeded, next("e"))

		assert.Nil(t, f.liveJob(t))
		assert.Equal(t, model.FoundryLoadDeleted, f.loadStatus(t, failed))
		_, err = f.db.GetFoundryOntologyByTenant(ctx, f.ontology.TenantID)
		assert.ErrorIs(t, err, store.ErrNotFound, "drained, it lets go of the database")
		require.NoError(t, f.db.CreateFoundryOntology(ctx, &rebind), "the database takes a new ontology")
	})

	// A retry is for a deleted ontology's stalled drain only: a live
	// ontology's failed deletes are retried by deleting the load again, and a
	// drain still running needs no second delete job.
	t.Run("resumes only a stalled drain of a deleted ontology", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "OD3")
		ws, o := f.ontology.WorkspaceID, f.ontology.ID
		stale := f.load(t, "L1")
		f.loaded(t, "w1")
		f.load(t, "L2")
		f.loaded(t, "w1")
		require.NoError(t, f.db.DeleteFoundryLoad(ctx, ws, o, stale, id26("FN-D-user")))
		f.ended(t, "w1", f.claimed(t, "w1", id26("FN-D-user")).ID, model.FoundryGraphJobFailed, next("a"))
		assert.ErrorIs(t, f.db.ResumeFoundryOntologyDrain(ctx, ws, o, id26("FN-D-live")), store.ErrNotFound, "a live ontology")
		assert.Equal(t, model.FoundryLoadDeleteFailed, f.loadStatus(t, stale), "left as it was")
		assert.Nil(t, f.liveJob(t))
		assert.ErrorIs(t, f.db.ResumeFoundryOntologyDrain(ctx, ws, id26("FO-none"), id26("FN-D-none")), store.ErrNotFound)
		assert.ErrorIs(t, f.db.ResumeFoundryOntologyDrain(ctx, id26("FWS-other"), o, id26("FN-D-other")), store.ErrNotFound,
			"another workspace's ontology")

		require.NoError(t, f.db.DeleteFoundryOntology(ctx, ws, o, id26("FN-D-onto")))
		draining := f.liveJob(t)
		require.NotNil(t, draining)
		require.NoError(t, f.db.ResumeFoundryOntologyDrain(ctx, ws, o, id26("FN-D-again")))
		assert.Equal(t, draining.ID, f.liveJob(t).ID, "the running drain is left as it is")
		_, err := f.db.GetFoundryGraphJob(ctx, o, id26("FN-D-again"))
		assert.ErrorIs(t, err, store.ErrNotFound, "no second delete job")
	})

	// A queued job of a deleted ontology never runs.
	t.Run("cancels a queued job", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "OD2")
		queued := f.load(t, "L1")
		require.NoError(t, f.db.DeleteFoundryOntology(ctx, f.ontology.WorkspaceID, f.ontology.ID, id26("FN-D-onto")))
		job, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, queued)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobCancelled, job.Status)
		assert.Equal(t, "the ontology was deleted", *job.Error)
		del := f.claimed(t, "w1", id26("FN-D-onto"))
		assert.Equal(t, queued, del.LoadID, "the load it began is deleted all the same")
	})
}

// A load's ingest count is written with the attempt's update past Ingest,
// fenced by the lease like the update; an update without it leaves it, and
// zero is a count, unlike unset.
func TestFoundryLoadIngestViolations(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "LV1")
	loadID := f.load(t, "LV1")
	violations := func() *uint64 {
		t.Helper()
		l, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, loadID)
		require.NoError(t, err)
		listed, err := f.db.ListFoundryLoads(ctx, f.ontology.ID)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, l.IngestViolations, listed[0].IngestViolations, "listed as got")
		return l.IngestViolations
	}
	require.Nil(t, violations(), "unknown before Ingest")
	job, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
	require.NoError(t, err)
	documents := model.FoundryGraphJobUpdate{Phase: model.FoundryGraphJobDocuments, Progress: model.FoundryGraphJobProgress{ChunksDone: 2}}

	stale := documents
	stale.IngestViolations = ptr(uint64(9))
	assert.ErrorIs(t, f.db.UpdateFoundryGraphJob(ctx, job.ID, "w2", stale), store.ErrNotFound, "another worker's attempt")
	require.Nil(t, violations(), "a refused update writes nothing")
	require.Equal(t, model.FoundryGraphJobProvisioning, f.liveJob(t).Phase)

	counted := documents
	counted.IngestViolations = ptr(uint64(2))
	require.NoError(t, f.db.UpdateFoundryGraphJob(ctx, job.ID, "w1", counted))
	require.NoError(t, f.db.UpdateFoundryGraphJob(ctx, job.ID, "w1", counted), "the same update again matches its rows")
	got := violations()
	require.NotNil(t, got)
	assert.Equal(t, uint64(2), *got)
	live := f.liveJob(t)
	assert.Equal(t, model.FoundryGraphJobDocuments, live.Phase)
	assert.Equal(t, 2, live.Progress.ChunksDone)

	documents.Progress.DocumentsDone = 1
	require.NoError(t, f.db.UpdateFoundryGraphJob(ctx, job.ID, "w1", documents))
	require.NotNil(t, violations())
	assert.Equal(t, uint64(2), *violations(), "an update without a count keeps the load's")
	assert.Equal(t, 1, f.liveJob(t).Progress.DocumentsDone)

	zero := documents
	zero.IngestViolations = ptr(uint64(0))
	require.NoError(t, f.db.UpdateFoundryGraphJob(ctx, job.ID, "w1", zero))
	require.NotNil(t, violations())
	assert.Zero(t, *violations())
}
