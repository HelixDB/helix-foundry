package store_test

import (
	"context"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
)

const enrichmentProvider = "fake"

// generous caps leave every reservation whole.
var generous = model.FoundryEnrichmentCaps{Workspace: 1 << 40, Global: 1 << 40, Since: time.Unix(0, 0)}

// claimedLoad queues load tag and claims its job as the graph worker "graph",
// at provisioning.
func (f foundryFixture) claimedLoad(t *testing.T, tag string) *model.FoundryGraphJob {
	t.Helper()
	f.load(t, tag)
	job, err := f.db.ClaimFoundryGraphJob(context.Background(), "graph", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, id26("FJ-"+tag), job.ID)
	return job
}

// queuedStep prepares load tag's step, queues it until claimBy and parks its job.
func (f foundryFixture) queuedStep(t *testing.T, tag string, claimBy time.Time) *model.FoundryGraphJob {
	t.Helper()
	ctx := context.Background()
	job := f.claimedLoad(t, tag)
	_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
	require.NoError(t, err)
	row, err := f.db.QueueFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentQueue{
		PlanKey: "plan-" + tag, CarriedKey: "carried-" + tag, ClaimBy: claimBy,
	})
	require.NoError(t, err)
	require.Equal(t, model.FoundryEnrichmentQueued, row.Status)
	due, err := f.db.ParkFoundryGraphJob(ctx, job.ID, "graph", claimBy)
	require.NoError(t, err)
	require.False(t, due)
	return job
}

// step re-reads a load's step.
func (f foundryFixture) step(t *testing.T, loadID string) *model.FoundryLoadEnrichment {
	t.Helper()
	row, err := f.db.GetFoundryLoadEnrichment(context.Background(), loadID)
	require.NoError(t, err)
	return row
}

// jobDue reports whether a job is queued and due now.
func jobDue(t *testing.T, db store.Store, ontologyID, jobID string) bool {
	t.Helper()
	job, err := db.GetFoundryGraphJob(context.Background(), ontologyID, jobID)
	require.NoError(t, err)
	return job.Status == model.FoundryGraphJobQueued && !job.NextAttemptAt.After(time.Now().UTC().Add(time.Second))
}

// running claims the due step as worker and checks it is load's.
func running(t *testing.T, db store.Store, worker, loadID string) *model.FoundryLoadEnrichment {
	t.Helper()
	row, _, err := db.ClaimFoundryLoadEnrichment(context.Background(), worker, enrichmentProvider, time.Minute, time.Hour, 2*time.Minute, 3)
	require.NoError(t, err)
	require.NotNil(t, row, "a step is due")
	require.Equal(t, loadID, row.LoadID)
	return row
}

// expireEnrichmentLease lets a step's lease run out by the database's
// clock, which claims compare it with: a sleep on the test's clock races
// any skew between the two.
func expireEnrichmentLease(t *testing.T, loadID string) {
	t.Helper()
	_, err := testDB.Exec(`UPDATE foundry_load_enrichments SET lease_until=NOW(6) - INTERVAL 1 SECOND WHERE load_id=?`, loadID)
	require.NoError(t, err)
}

func consent(workspaceID, tag string, enabled bool, capCredits int64) *model.FoundryEnrichmentConsent {
	return &model.FoundryEnrichmentConsent{
		ID: id26("FEC-" + tag), WorkspaceID: workspaceID, Enabled: enabled, MonthlyCreditCap: capCredits, NoticeVersion: "2026-10",
		UserID: id26("FU-owner"),
	}
}

func TestFoundryEnrichmentConsent(t *testing.T) {
	// The setting is the newest change; every change stays as history.
	t.Run("the newest change is the setting", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "EC1")
		ws := f.ontology.WorkspaceID
		_, err := f.db.GetFoundryEnrichmentConsent(ctx, ws)
		assert.ErrorIs(t, err, store.ErrNotFound, "never set is off")
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(ws, "a", true, 50_000)))
		time.Sleep(2 * time.Millisecond)
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(ws, "b", true, 20_000)))
		got, err := f.db.GetFoundryEnrichmentConsent(ctx, ws)
		require.NoError(t, err)
		assert.True(t, got.Enabled)
		assert.Equal(t, int64(20_000), got.MonthlyCreditCap)
		assert.Error(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(ws, "c", true, -1)), "a negative cap is refused")
	})

	// Without consent no lookups run and no profiles stay: a queued step ends
	// cancelled and its parked job wakes to apply nothing, a running one is
	// asked to cancel, every step's files are marked for the sweep, and an
	// ontology with a load that applied profiles (current, or still writing)
	// gets a purge.
	t.Run("turning consent off cancels steps and purges", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "EC2")
		ws := f.ontology.WorkspaceID
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(ws, "on", true, 50_000)))

		// Load 1 applied profiles and is current.
		first := f.claimedLoad(t, "EC2-1")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", first)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, first.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "carried-1", ResultsKey: "results-1",
			Applied: model.FoundryEnrichmentApplied{Key: "carried-1", SHA256: "aa"},
		}))
		_, err = f.db.FinishFoundryGraphJob(ctx, first.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("EC2-1"),
		})
		require.NoError(t, err)
		require.Equal(t, first.LoadID, *f.ontologyNow(t).CurrentLoadID)

		// Load 2's step is queued; another ontology's step is running.
		second := f.queuedStep(t, "EC2-2", time.Now().Add(2*time.Hour))
		other := foundryFixture{db: f.db, ontology: sampleOntology(t, f.db, ws, "EC2-other")}
		otherJob := other.queuedStep(t, "EC2-3", time.Now().Add(time.Hour))
		running(t, f.db, "w1", otherJob.LoadID)
		// A third ontology's load, still writing, chose its profiles.
		writing := foundryFixture{db: f.db, ontology: sampleOntology(t, f.db, ws, "EC2-writing")}
		writingJob := writing.claimedLoad(t, "EC2-4")
		_, err = f.db.StartFoundryLoadEnrichment(ctx, "graph", writingJob)
		require.NoError(t, err)
		_, err = f.db.ApplyFoundryLoadEnrichment(ctx, writingJob.LoadID, "graph", model.FoundryEnrichmentApplied{Key: "profiles-4", SHA256: "dd"})
		require.NoError(t, err)

		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(ws, "off", false, 50_000)))

		queued := f.step(t, second.LoadID)
		assert.Equal(t, model.FoundryEnrichmentCancelled, queued.Status)
		assert.True(t, jobDue(t, f.db, f.ontology.ID, second.ID), "the parked job wakes to apply nothing")
		run := other.step(t, otherJob.LoadID)
		assert.Equal(t, model.FoundryEnrichmentRunning, run.Status)
		assert.True(t, run.CancelRequested)
		for _, loadID := range []string{first.LoadID, second.LoadID, otherJob.LoadID} {
			assert.NotNil(t, f.step(t, loadID).PurgedAt, "every step is purged")
		}
		purges, err := f.db.ListFoundryEnrichmentPurges(ctx, 3, 10)
		require.NoError(t, err)
		purged := []string{}
		for _, p := range purges {
			purged = append(purged, p.OntologyID)
		}
		assert.ElementsMatch(t, []string{f.ontology.ID, writing.ontology.ID}, purged, "only ontologies with a load that applied profiles")
		signals, err := f.db.RenewFoundryLoadEnrichmentLease(ctx, otherJob.LoadID, "w1", enrichmentProvider, time.Minute)
		require.NoError(t, err)
		assert.True(t, signals.Cancel, "the running worker hears the cancel at its next renewal")
		assert.Equal(t, model.FoundryGraphJobRunning, writing.liveJob(t).Status, "the loads go on, without lookups")
	})
}

func TestFoundryLoadEnrichmentPrepare(t *testing.T) {
	// A retried attempt finds the step the first one inserted.
	t.Run("starts one step per load, fenced on the job", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "EP1")
		job := f.claimedLoad(t, "EP1")
		row, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentPreparing, row.Status)
		again, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		assert.Equal(t, row.CreatedAt, again.CreatedAt)
		_, err = f.db.StartFoundryLoadEnrichment(ctx, "stale", job)
		assert.ErrorIs(t, err, store.ErrNotFound, "a worker without the job's lease writes nothing")
		_, err = f.db.QueueFoundryLoadEnrichment(ctx, job.LoadID, "stale", model.FoundryEnrichmentQueue{
			PlanKey: "p", CarriedKey: "c", ClaimBy: time.Now().Add(time.Minute),
		})
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// The file a load applies is chosen once, so a retried attempt stages
	// the same bytes and Ingest can't see its sources change.
	t.Run("writes the applied file once", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "EP2")
		job := f.claimedLoad(t, "EP2")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		first, err := f.db.ApplyFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentApplied{Key: "carried", SHA256: "c1"})
		require.NoError(t, err)
		second, err := f.db.ApplyFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentApplied{Key: "profiles", SHA256: "p1"})
		require.NoError(t, err)
		assert.Equal(t, first, second)
		assert.Equal(t, "carried", second.Key)
		_, err = f.db.ApplyFoundryLoadEnrichment(ctx, job.LoadID, "stale", model.FoundryEnrichmentNone)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// Consent withdrawn while a load waited for its step: the profiles it
	// would apply are no longer allowed, whatever the attempt read before.
	t.Run("a purged step applies no profiles", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "EP5")
		job := f.claimedLoad(t, "EP5")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(f.ontology.WorkspaceID, "EP5-off", false, 0)))
		applied, err := f.db.ApplyFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentApplied{Key: "profiles", SHA256: "p1"})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentNone, applied)
	})

	// A step that failed to prepare applies the newest file a step applied
	// of its own, never one a failed step borrowed: that file goes with the
	// load it belongs to, and a later load falling back to it would find
	// nothing to stage, and fail.
	t.Run("falls back only to a file a step applied of its own", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "EP7")
		owner := f.claimedLoad(t, "EP7-1")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", owner)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, owner.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "carried-EP7-1", ResultsKey: "results-EP7-1",
			Applied: model.FoundryEnrichmentApplied{Key: "carried-EP7-1", SHA256: "cc"},
		}))
		_, err = f.db.FinishFoundryGraphJob(ctx, owner.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("EP7-1"),
		})
		require.NoError(t, err)
		time.Sleep(2 * time.Millisecond)
		borrower := f.claimedLoad(t, "EP7-2")
		_, err = f.db.StartFoundryLoadEnrichment(ctx, "graph", borrower)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, borrower.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentFailed), Applied: model.FoundryEnrichmentApplied{Key: "carried-EP7-1", SHA256: "cc"},
		}))
		_, err = f.db.FinishFoundryGraphJob(ctx, borrower.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("EP7-2"),
		})
		require.NoError(t, err)

		latest, err := f.db.GetLatestFoundryEnrichmentApplied(ctx, f.ontology.ID)
		require.NoError(t, err)
		assert.Equal(t, owner.LoadID, latest.LoadID, "the file's own step, not the newer one that borrowed it")
		_, err = testDB.Exec(`UPDATE foundry_loads SET status='deleted', deleted_at=NOW(6) WHERE id=?`, owner.LoadID)
		require.NoError(t, err)
		_, err = f.db.GetLatestFoundryEnrichmentApplied(ctx, f.ontology.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "the file went with its load")
	})

	// A Stop or a cancel that lands while the plan is written must not be
	// lost: handing the step over ends it instead.
	t.Run("a stop or cancel asked while preparing ends the step", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "EP3")
		job := f.claimedLoad(t, "EP3")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		require.NoError(t, f.db.StopFoundryLoadEnrichment(ctx, f.ontology.WorkspaceID, f.ontology.ID, job.LoadID, id26("FU-ada")))
		row, err := f.db.QueueFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentQueue{
			PlanKey: "p", CarriedKey: "c", ClaimBy: time.Now().Add(time.Minute),
		})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentPartial, row.Status)
		assert.Equal(t, model.FoundryEnrichmentStopStopped, *row.StoppedBy)
		assert.Equal(t, id26("FU-ada"), *row.StopRequested)

		g := newFoundryFixture(t, "EP4")
		ws := g.ontology.WorkspaceID
		job = g.claimedLoad(t, "EP4")
		_, err = g.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		require.NoError(t, g.db.SetFoundryEnrichmentConsent(ctx, consent(ws, "off", false, 0)))
		require.NoError(t, g.db.EndPreparedFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "c", ResultsKey: "r",
			Applied: model.FoundryEnrichmentApplied{Key: "c", SHA256: "cc"},
		}))
		ended := g.step(t, job.LoadID)
		assert.Equal(t, model.FoundryEnrichmentCancelled, ended.Status)
		assert.Equal(t, model.FoundryEnrichmentNone, *ended.Applied, "consent turned off mid-prepare applies nothing")
	})

	// A step nobody claimed in time ends busy, but one claimed at the last
	// moment keeps running.
	t.Run("ends busy only while still queued", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "EP5")
		job := f.claimedLoad(t, "EP5")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		_, err = f.db.QueueFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentQueue{
			PlanKey: "p", CarriedKey: "c", ClaimBy: time.Now().Add(-time.Second),
		})
		require.NoError(t, err)
		claimed, _, err := f.db.ClaimFoundryLoadEnrichment(ctx, "w1", enrichmentProvider, time.Minute, time.Hour, 2*time.Minute, 3)
		require.NoError(t, err)
		assert.Nil(t, claimed, "a step past its claim_by is never claimed")
		require.NoError(t, f.db.EndBusyFoundryLoadEnrichment(ctx, job.LoadID, "graph"))
		row := f.step(t, job.LoadID)
		assert.Equal(t, model.FoundryEnrichmentPartial, row.Status)
		assert.Equal(t, model.FoundryEnrichmentStopBusy, *row.StoppedBy)

		g := newFoundryFixture(t, "EP6")
		job = g.claimedLoad(t, "EP6")
		_, err = g.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		_, err = g.db.QueueFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentQueue{
			PlanKey: "p", CarriedKey: "c", ClaimBy: time.Now().Add(time.Hour),
		})
		require.NoError(t, err)
		running(t, g.db, "w1", job.LoadID)
		require.NoError(t, g.db.EndBusyFoundryLoadEnrichment(ctx, job.LoadID, "graph"))
		assert.Equal(t, model.FoundryEnrichmentRunning, g.step(t, job.LoadID).Status)
	})
}

func TestClaimFoundryLoadEnrichment(t *testing.T) {
	// Three slots across workspaces and one per workspace keep one
	// workspace's people from taking every lookup.
	t.Run("claims at most max running and one per workspace", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		claimBy := time.Now().Add(time.Hour)
		var loads []string
		for i, tag := range []string{"CL-A", "CL-B", "CL-C", "CL-D"} {
			f := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-"+tag), tag)}
			loads = append(loads, f.queuedStep(t, tag, claimBy.Add(time.Duration(i)*time.Second)).LoadID)
		}
		sameWorkspace := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-CL-A"), "CL-A2")}
		loads = append(loads, sameWorkspace.queuedStep(t, "CL-A2", claimBy.Add(-time.Minute)).LoadID)

		first := running(t, db, "w1", loads[4])
		assert.NotNil(t, first.Deadline)
		assert.WithinDuration(t, time.Now().Add(time.Hour), *first.Deadline, time.Minute)
		running(t, db, "w1", loads[1]) // CL-A waits for its workspace's step.
		running(t, db, "w1", loads[2])
		none, _, err := db.ClaimFoundryLoadEnrichment(ctx, "w1", enrichmentProvider, time.Minute, time.Hour, 2*time.Minute, 3)
		require.NoError(t, err)
		assert.Nil(t, none, "three are running")
	})

	// A worker that died mid-step leaves its batch and reservation in the
	// row; the next claim takes the step over where it stopped.
	t.Run("reclaims a step whose lease ran out", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "CL2")
		job := f.queuedStep(t, "CL2", time.Now().Add(time.Hour))
		row, first, err := f.db.ClaimFoundryLoadEnrichment(ctx, "dead", enrichmentProvider, time.Minute, time.Hour, 2*time.Minute, 3)
		require.NoError(t, err)
		require.NotNil(t, row)
		assert.True(t, first, "taking a queued step is its first claim")
		_, err = f.db.ReserveFoundryEnrichmentCredits(ctx, job.LoadID, "dead", enrichmentProvider, 200, generous)
		require.NoError(t, err)
		begun, err := f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "dead", enrichmentProvider, 0, 5, generous)
		require.NoError(t, err)
		require.Equal(t, 5, begun.N)
		expireEnrichmentLease(t, job.LoadID)
		taken, first, err := f.db.ClaimFoundryLoadEnrichment(ctx, "w2", enrichmentProvider, time.Minute, time.Hour, 2*time.Minute, 3)
		require.NoError(t, err)
		require.NotNil(t, taken)
		require.Equal(t, job.LoadID, taken.LoadID)
		assert.False(t, first, "a reclaim is not the step's first claim")
		assert.Equal(t, int64(100), taken.InflightCredits, "the batch in flight is the new worker's to settle")
		assert.Equal(t, *row.Deadline, *taken.Deadline, "the deadline counts from the first claim")
		assert.Equal(t, *row.ClaimedAt, *taken.ClaimedAt, "a reclaim keeps the first claim's time")
	})

	// A step past its deadline and the grace runs no lookup, and its load
	// no longer waits for it: only its late publish is left, which retries
	// may take a while to land. It holds no running slot meanwhile, its
	// workspace's or Helix's.
	t.Run("a step past its deadline and the grace holds no running slot", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		late := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-CL4"), "CL4")}
		next := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-CL4"), "CL4-2")}
		lateJob := late.queuedStep(t, "CL4", time.Now().Add(time.Hour))
		nextJob := next.queuedStep(t, "CL4-2", time.Now().Add(time.Hour+time.Minute))
		claim := func(worker string) *model.FoundryLoadEnrichment {
			t.Helper()
			row, _, err := db.ClaimFoundryLoadEnrichment(ctx, worker, enrichmentProvider, time.Minute, time.Hour, 2*time.Minute, 1)
			require.NoError(t, err)
			return row
		}
		first := claim("w1")
		require.NotNil(t, first)
		require.Equal(t, lateJob.LoadID, first.LoadID)
		assert.Nil(t, claim("w2"), "one runs, of one allowed")
		_, err := testDB.Exec(`UPDATE foundry_load_enrichments SET deadline=NOW(6) - INTERVAL 1 MINUTE WHERE load_id=?`, lateJob.LoadID)
		require.NoError(t, err)
		assert.Nil(t, claim("w2"), "within the grace it keeps its slot: its load still waits")
		_, err = testDB.Exec(`UPDATE foundry_load_enrichments SET deadline=NOW(6) - INTERVAL 3 MINUTE WHERE load_id=?`, lateJob.LoadID)
		require.NoError(t, err)
		taken := claim("w2")
		require.NotNil(t, taken, "past the grace it holds neither its workspace's slot nor Helix's")
		assert.Equal(t, nextJob.LoadID, taken.LoadID)
		assert.Equal(t, model.FoundryEnrichmentRunning, late.step(t, lateJob.LoadID).Status, "the late step still runs, to publish")
	})

	// A worker stopping for a deploy releases its step: the next claim takes
	// it at once, where the row says, without waiting for the lease to run
	// out. Only the lease's owner can release it.
	t.Run("hands a released step to the next claim at once", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "CL3")
		job := f.queuedStep(t, "CL3", time.Now().Add(time.Hour))
		running(t, f.db, "w1", job.LoadID)
		_, err := f.db.ReserveFoundryEnrichmentCredits(ctx, job.LoadID, "w1", enrichmentProvider, 200, generous)
		require.NoError(t, err)
		_, err = f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "w1", enrichmentProvider, 0, 5, generous)
		require.NoError(t, err)
		assert.ErrorIs(t, f.db.ReleaseFoundryLoadEnrichment(ctx, job.LoadID, "w2"), store.ErrNotFound, "not w2's lease")
		none, _, err := f.db.ClaimFoundryLoadEnrichment(ctx, "w2", enrichmentProvider, time.Minute, time.Hour, 2*time.Minute, 3)
		require.NoError(t, err)
		assert.Nil(t, none, "w1 still holds it")

		require.NoError(t, f.db.ReleaseFoundryLoadEnrichment(ctx, job.LoadID, "w1"))
		require.NoError(t, f.db.ReleaseFoundryLoadEnrichment(ctx, job.LoadID, "w1"), "releasing twice is no failure")
		taken := running(t, f.db, "w2", job.LoadID)
		assert.Equal(t, model.FoundryEnrichmentRunning, taken.Status)
		assert.Equal(t, 0, taken.BatchCursor)
		assert.Equal(t, int64(100), taken.InflightCredits, "the batch in flight is the next claim's to settle")
		assert.Equal(t, "w2", *taken.LeaseOwner)
		assert.ErrorIs(t, f.db.ReleaseFoundryLoadEnrichment(ctx, job.LoadID, "w1"), store.ErrNotFound, "w1 no longer holds it")
		assert.True(t, f.step(t, job.LoadID).LeaseUntil.After(time.Now().UTC()), "w2's lease stands")
	})
}

func TestFoundryEnrichmentLedger(t *testing.T) {
	// A month's cap counts only the credits charged in that month.
	t.Run("counts the charging month only", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "LG1")
		job := f.queuedStep(t, "LG1", time.Now().Add(time.Hour))
		running(t, f.db, "w1", job.LoadID)
		_, err := f.db.ReserveFoundryEnrichmentCredits(ctx, job.LoadID, "w1", enrichmentProvider, 1000, generous)
		require.NoError(t, err)
		for batch := range 2 {
			_, err := f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "w1", enrichmentProvider, batch, 3, generous)
			require.NoError(t, err)
			require.NoError(t, f.db.ChargeFoundryEnrichmentBatch(ctx, job.LoadID, "w1",
				model.FoundryEnrichmentCharge{Batch: batch, Credits: 40, UncertainCredits: 20, Remaining: 1000}, model.FoundryEnrichmentCounts{}))
		}
		now := time.Now().UTC()
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		_, err = testDB.Exec(`UPDATE foundry_enrichment_charges SET charged_at=? WHERE batch=0`, monthStart.Add(-time.Hour))
		require.NoError(t, err)
		ledger, err := f.db.GetFoundryEnrichmentLedger(ctx, f.ontology.WorkspaceID, monthStart)
		require.NoError(t, err)
		assert.Equal(t, int64(60), ledger.WorkspaceCharged)
		assert.Equal(t, int64(60), ledger.GlobalCharged)
		assert.Equal(t, int64(1000-120), ledger.WorkspaceReserved, "the reservation shrinks by what was charged")
		previous, err := f.db.GetFoundryEnrichmentLedger(ctx, f.ontology.WorkspaceID, monthStart.AddDate(0, -1, 0))
		require.NoError(t, err)
		assert.Equal(t, int64(120), previous.WorkspaceCharged)
	})

	// A batch's free answers leave reserved what they didn't cost: each
	// charge cuts the step's reservation to what its calls after the batch
	// may cost, never raising it, so other steps can reserve what this one
	// can no longer spend.
	t.Run("cuts a reservation to what the step's calls left may cost", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "LG5")
		job := f.queuedStep(t, "LG5", time.Now().Add(time.Hour))
		running(t, f.db, "w1", job.LoadID)
		_, err := f.db.ReserveFoundryEnrichmentCredits(ctx, job.LoadID, "w1", enrichmentProvider, 1000, generous)
		require.NoError(t, err)
		reserved := func() int64 {
			ledger, err := f.db.GetFoundryEnrichmentLedger(ctx, f.ontology.WorkspaceID, time.Unix(0, 0))
			require.NoError(t, err)
			return ledger.WorkspaceReserved
		}
		_, err = f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "w1", enrichmentProvider, 0, 5, generous)
		require.NoError(t, err)
		require.NoError(t, f.db.ChargeFoundryEnrichmentBatch(ctx, job.LoadID, "w1",
			model.FoundryEnrichmentCharge{Batch: 0, Credits: 20, Remaining: 200}, model.FoundryEnrichmentCounts{}))
		assert.Equal(t, int64(200), reserved(), "cut to what the calls left may cost")
		_, err = f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "w1", enrichmentProvider, 1, 5, generous)
		require.NoError(t, err)
		require.NoError(t, f.db.ChargeFoundryEnrichmentBatch(ctx, job.LoadID, "w1",
			model.FoundryEnrichmentCharge{Batch: 1, Credits: 40, Remaining: 500}, model.FoundryEnrichmentCounts{}))
		assert.Equal(t, int64(160), reserved(), "less what was charged, never raised")
		_, err = f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "w1", enrichmentProvider, 2, 5, generous)
		require.NoError(t, err)
		assert.Error(t, f.db.ChargeFoundryEnrichmentBatch(ctx, job.LoadID, "w1",
			model.FoundryEnrichmentCharge{Batch: 2, Remaining: -20}, model.FoundryEnrichmentCounts{}), "a negative remainder")
		assert.Equal(t, 2, f.step(t, job.LoadID).BatchCursor, "nothing written")
	})

	// The reservation and both caps bound each batch, so the workspace and
	// Helix never charge past their month's credits.
	t.Run("bounds batches by the reservation and the caps", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		a := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-LG2-A"), "LG2-A")}
		b := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-LG2-B"), "LG2-B")}
		jobA := a.queuedStep(t, "LG2-A", time.Now().Add(time.Hour))
		jobB := b.queuedStep(t, "LG2-B", time.Now().Add(time.Hour))
		running(t, db, "w1", jobA.LoadID)
		running(t, db, "w1", jobB.LoadID)
		caps := model.FoundryEnrichmentCaps{Workspace: 300, Global: 500, Since: time.Unix(0, 0)}

		reservedA, err := db.ReserveFoundryEnrichmentCredits(ctx, jobA.LoadID, "w1", enrichmentProvider, 1000, caps)
		require.NoError(t, err)
		assert.Equal(t, int64(300), reservedA, "the workspace cap")
		reservedB, err := db.ReserveFoundryEnrichmentCredits(ctx, jobB.LoadID, "w1", enrichmentProvider, 1000, caps)
		require.NoError(t, err)
		assert.Equal(t, int64(200), reservedB, "what Helix's cap leaves after the other workspace's reservation")

		begun, err := db.BeginFoundryEnrichmentBatch(ctx, jobB.LoadID, "w1", enrichmentProvider, 0, 50, caps)
		require.NoError(t, err)
		assert.Equal(t, 10, begun.N, "a batch shrinks to its reservation")
		_, err = db.BeginFoundryEnrichmentBatch(ctx, jobB.LoadID, "w1", enrichmentProvider, 0, 50, caps)
		assert.Error(t, err, "a batch in flight must be settled first")
		require.NoError(t, db.ChargeFoundryEnrichmentBatch(ctx, jobB.LoadID, "w1",
			model.FoundryEnrichmentCharge{Batch: 0, Credits: 200}, model.FoundryEnrichmentCounts{Found: 10}))
		begun, err = db.BeginFoundryEnrichmentBatch(ctx, jobB.LoadID, "w1", enrichmentProvider, 1, 50, caps)
		require.NoError(t, err)
		assert.Equal(t, 0, begun.N, "the reservation is spent: the step ends partial(cap)")
		assert.Equal(t, int64(10), b.step(t, jobB.LoadID).Counts.Found)

		lowered := model.FoundryEnrichmentCaps{Workspace: 100, Global: 500, Since: time.Unix(0, 0)}
		begun, err = db.BeginFoundryEnrichmentBatch(ctx, jobA.LoadID, "w1", enrichmentProvider, 0, 50, lowered)
		require.NoError(t, err)
		assert.Equal(t, 5, begun.N, "a cap lowered mid-step bounds the next batch")
	})

	// A crash between the charge and the next batch replays the settlement;
	// the batch is charged once.
	t.Run("charges a batch once", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "LG3")
		job := f.queuedStep(t, "LG3", time.Now().Add(time.Hour))
		running(t, f.db, "w1", job.LoadID)
		_, err := f.db.ReserveFoundryEnrichmentCredits(ctx, job.LoadID, "w1", enrichmentProvider, 100, generous)
		require.NoError(t, err)
		_, err = f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "w1", enrichmentProvider, 0, 5, generous)
		require.NoError(t, err)
		assert.Error(t, f.db.ChargeFoundryEnrichmentBatch(ctx, job.LoadID, "w1",
			model.FoundryEnrichmentCharge{Batch: 0, Credits: 120}, model.FoundryEnrichmentCounts{}), "more than the batch had in flight")
		require.NoError(t, f.db.ChargeFoundryEnrichmentBatch(ctx, job.LoadID, "w1",
			model.FoundryEnrichmentCharge{Batch: 0, Credits: 60}, model.FoundryEnrichmentCounts{}))
		_, err = testDB.Exec(`UPDATE foundry_load_enrichments SET batch_cursor=0, reserved_credits=100, inflight_credits=100 WHERE load_id=?`, job.LoadID)
		require.NoError(t, err)
		require.NoError(t, f.db.ChargeFoundryEnrichmentBatch(ctx, job.LoadID, "w1",
			model.FoundryEnrichmentCharge{Batch: 0, Credits: 60}, model.FoundryEnrichmentCounts{}))
		ledger, err := f.db.GetFoundryEnrichmentLedger(ctx, f.ontology.WorkspaceID, time.Unix(0, 0))
		require.NoError(t, err)
		assert.Equal(t, int64(60), ledger.WorkspaceCharged)
		assert.Equal(t, 1, f.step(t, job.LoadID).BatchCursor)
	})

	// A worker whose lease another took over can't call, charge or publish:
	// every write is fenced on the lease.
	t.Run("fences a stale lease", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "LG4")
		job := f.queuedStep(t, "LG4", time.Now().Add(time.Hour))
		_, _, err := f.db.ClaimFoundryLoadEnrichment(ctx, "stale", enrichmentProvider, time.Minute, time.Hour, 2*time.Minute, 3)
		require.NoError(t, err)
		expireEnrichmentLease(t, job.LoadID)
		running(t, f.db, "fresh", job.LoadID)
		_, err = f.db.ReserveFoundryEnrichmentCredits(ctx, job.LoadID, "stale", enrichmentProvider, 100, generous)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "stale", enrichmentProvider, 0, 5, generous)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.ErrorIs(t, f.db.ChargeFoundryEnrichmentBatch(ctx, job.LoadID, "stale",
			model.FoundryEnrichmentCharge{Batch: 0}, model.FoundryEnrichmentCounts{}), store.ErrNotFound)
		_, err = f.db.RenewFoundryLoadEnrichmentLease(ctx, job.LoadID, "stale", enrichmentProvider, time.Minute)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.ErrorIs(t, f.db.EndFoundryLoadEnrichment(ctx, job.LoadID, "stale", model.EndFoundryEnrichment(model.FoundryEnrichmentDone),
			&model.FoundryEnrichmentPublished{ProfilesKey: "p", ResultsKey: "r"}, model.FoundryEnrichmentCounts{}), store.ErrNotFound)
		assert.Equal(t, "fresh", *f.step(t, job.LoadID).LeaseOwner)
	})
}

func TestFoundryEnrichmentProviderHalt(t *testing.T) {
	// A 402 halts lookups for every worker, and a restart doesn't lift it.
	t.Run("survives a restart and keeps the longer halt", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		fresh, err := db.GetFoundryEnrichmentProvider(ctx, "contextdev")
		require.NoError(t, err)
		assert.False(t, fresh.Halted(time.Now()))
		until := time.Now().UTC().Add(15 * time.Minute)
		require.NoError(t, db.HaltFoundryEnrichmentProvider(ctx, "contextdev", until, "402"))
		require.NoError(t, db.HaltFoundryEnrichmentProvider(ctx, "contextdev", until.Add(-time.Minute), "401"))
		restarted := store.NewMySQL(testDB, hostWorkspaces{})
		halt, err := restarted.GetFoundryEnrichmentProvider(ctx, "contextdev")
		require.NoError(t, err)
		assert.True(t, halt.Halted(time.Now()))
		assert.False(t, halt.Halted(until.Add(time.Second)))
		assert.Equal(t, "402", *halt.HaltCode)
	})
}

func TestBeginFoundryEnrichmentBatch(t *testing.T) {
	// A halt is the provider's: an operator's during an incident, or one
	// another step recorded after a 402, stops a step already running. The
	// lease's renewal reads it, so a batch stops sending at once, and so do
	// the re-checks and the next batch, with nothing put in flight.
	t.Run("sees a halt recorded meanwhile", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "BG1")
		name := "halted-BG1"
		job := f.queuedStep(t, "BG1", time.Now().Add(time.Hour))
		running(t, f.db, "w1", job.LoadID)
		_, err := f.db.ReserveFoundryEnrichmentCredits(ctx, job.LoadID, "w1", name, 1000, generous)
		require.NoError(t, err)
		begun, err := f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "w1", name, 0, 5, generous)
		require.NoError(t, err)
		require.Equal(t, model.FoundryEnrichmentBegun{N: 5}, begun)
		require.NoError(t, f.db.ChargeFoundryEnrichmentBatch(ctx, job.LoadID, "w1", model.FoundryEnrichmentCharge{Batch: 0},
			model.FoundryEnrichmentCounts{}))
		signals, err := f.db.RenewFoundryLoadEnrichmentLease(ctx, job.LoadID, "w1", name, time.Minute)
		require.NoError(t, err)
		assert.False(t, signals.Halted)
		require.NoError(t, f.db.HaltFoundryEnrichmentProvider(ctx, name, time.Now().UTC().Add(time.Hour), "operator"))
		signals, err = f.db.RenewFoundryLoadEnrichmentLease(ctx, job.LoadID, "w1", name, time.Minute)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentSignals{Halted: true}, signals, "the renewal reads the halt")
		other, err := f.db.RenewFoundryLoadEnrichmentLease(ctx, job.LoadID, "w1", enrichmentProvider, time.Minute)
		require.NoError(t, err)
		assert.False(t, other.Halted, "another provider's halt")
		checks, err := f.db.CheckFoundryLoadEnrichment(ctx, job.LoadID, name)
		require.NoError(t, err)
		assert.True(t, checks.Halted, "the re-checks read it")
		begun, err = f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "w1", name, 1, 5, generous)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentBegun{FoundryEnrichmentSignals: model.FoundryEnrichmentSignals{Halted: true}}, begun)
		assert.Zero(t, f.step(t, job.LoadID).InflightCredits, "nothing in flight")
		_, err = testDB.Exec(`UPDATE foundry_enrichment_provider SET halted_until=NOW(6) - INTERVAL 1 SECOND WHERE provider=?`, name)
		require.NoError(t, err)
		signals, err = f.db.RenewFoundryLoadEnrichmentLease(ctx, job.LoadID, "w1", name, time.Minute)
		require.NoError(t, err)
		assert.False(t, signals.Halted, "a halt past its end, by the database's clock")
	})

	// A user's Stop and a cancel (consent off) end a running step before
	// its next batch, whenever the lease's renewal would have carried them.
	t.Run("sees a Stop and a cancel asked meanwhile", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "BG2")
		job := f.queuedStep(t, "BG2", time.Now().Add(time.Hour))
		running(t, f.db, "w1", job.LoadID)
		_, err := f.db.ReserveFoundryEnrichmentCredits(ctx, job.LoadID, "w1", enrichmentProvider, 1000, generous)
		require.NoError(t, err)
		require.NoError(t, f.db.StopFoundryLoadEnrichment(ctx, f.ontology.WorkspaceID, f.ontology.ID, job.LoadID, id26("FU-ada")))
		begun, err := f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "w1", enrichmentProvider, 0, 5, generous)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentBegun{FoundryEnrichmentSignals: model.FoundryEnrichmentSignals{Stop: true}}, begun)
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(f.ontology.WorkspaceID, "BG2-off", false, 0)))
		begun, err = f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "w1", enrichmentProvider, 0, 5, generous)
		require.NoError(t, err)
		assert.True(t, begun.Cancel)
		assert.Zero(t, begun.N)
		assert.Zero(t, f.step(t, job.LoadID).InflightCredits, "nothing in flight")
	})
}

func TestParkFoundryGraphJob(t *testing.T) {
	// A step that ends before the job parks must not leave the job asleep
	// until claim_by: the park reads the step under the same lock.
	t.Run("a step ended before the park makes the job due", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "PK1")
		job := f.claimedLoad(t, "PK1")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		_, err = f.db.QueueFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentQueue{
			PlanKey: "p", CarriedKey: "c", ClaimBy: time.Now().Add(time.Hour),
		})
		require.NoError(t, err)
		require.NoError(t, f.db.StopFoundryLoadEnrichment(ctx, f.ontology.WorkspaceID, f.ontology.ID, job.LoadID, id26("FU-ada")))
		due, err := f.db.ParkFoundryGraphJob(ctx, job.ID, "graph", time.Now().Add(time.Hour))
		require.NoError(t, err)
		assert.True(t, due)
		assert.True(t, jobDue(t, f.db, f.ontology.ID, job.ID))
		parked, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, job.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, parked.Attempts, "a park gives its attempt back")
		assert.Equal(t, model.FoundryGraphJobProvisioning, parked.Phase)
	})

	// A step still queued keeps the job asleep until its claim_by. The
	// attempt that parks gives itself back, not the earlier failed one, and
	// that failure's error no longer describes a job that is only waiting.
	t.Run("sleeps until claim_by, without an earlier attempt's error", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "PK3")
		job := f.claimedLoad(t, "PK3")
		retryAt := time.Now().Add(-time.Second)
		_, err := f.db.FinishFoundryGraphJob(ctx, job.ID, "graph", model.FoundryGraphJobOutcome{
			RetryAt: &retryAt, Error: "the ontology agent is unavailable; Helix will retry",
		})
		require.NoError(t, err)
		job, err = f.db.ClaimFoundryGraphJob(ctx, "graph", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, job)
		require.Equal(t, 2, job.Attempts)
		_, err = f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		claimBy := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		_, err = f.db.QueueFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentQueue{PlanKey: "p", CarriedKey: "c", ClaimBy: claimBy})
		require.NoError(t, err)
		due, err := f.db.ParkFoundryGraphJob(ctx, job.ID, "graph", claimBy)
		require.NoError(t, err)
		assert.False(t, due)
		parked, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, job.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobQueued, parked.Status)
		assert.True(t, parked.NextAttemptAt.Equal(claimBy), "next attempt at %s, want %s", parked.NextAttemptAt, claimBy)
		assert.Equal(t, 1, parked.Attempts)
		assert.Nil(t, parked.Error)
		assert.Nil(t, parked.LeaseOwner)
		_, err = f.db.ParkFoundryGraphJob(ctx, job.ID, "graph", claimBy)
		assert.ErrorIs(t, err, store.ErrNotFound, "a parked attempt holds no lease")
	})

	// Whichever of the end and the park commits first, the job ends up due.
	t.Run("never loses a wake to a concurrent park", func(t *testing.T) {
		ctx := context.Background()
		for i := range 10 {
			f := newFoundryFixture(t, "PK2")
			tag := "PK2"
			job := f.claimedLoad(t, tag)
			_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
			require.NoError(t, err)
			_, err = f.db.QueueFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentQueue{
				PlanKey: "p", CarriedKey: "c", ClaimBy: time.Now().Add(time.Hour),
			})
			require.NoError(t, err)
			running(t, f.db, "w1", job.LoadID)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				assert.NoError(t, f.db.EndFoundryLoadEnrichment(ctx, job.LoadID, "w1", model.EndFoundryEnrichment(model.FoundryEnrichmentDone),
					&model.FoundryEnrichmentPublished{ProfilesKey: "p", ResultsKey: "r"}, model.FoundryEnrichmentCounts{}))
			}()
			go func() {
				defer wg.Done()
				_, err := f.db.ParkFoundryGraphJob(ctx, job.ID, "graph", time.Now().Add(time.Hour))
				assert.NoError(t, err)
			}()
			wg.Wait()
			assert.True(t, jobDue(t, f.db, f.ontology.ID, job.ID), "round %d", i)
		}
	})
}

func TestStopFoundryLoadEnrichment(t *testing.T) {
	// A queued step has nothing to settle: it ends at once and its job wakes.
	t.Run("ends a queued step and wakes its job", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "ST1")
		job := f.queuedStep(t, "ST1", time.Now().Add(time.Hour))
		require.NoError(t, f.db.StopFoundryLoadEnrichment(ctx, f.ontology.WorkspaceID, f.ontology.ID, job.LoadID, id26("FU-ada")))
		row := f.step(t, job.LoadID)
		assert.Equal(t, model.FoundryEnrichmentPartial, row.Status)
		assert.Equal(t, model.FoundryEnrichmentStopStopped, *row.StoppedBy)
		assert.True(t, jobDue(t, f.db, f.ontology.ID, job.ID))
		assert.ErrorIs(t, f.db.StopFoundryLoadEnrichment(ctx, f.ontology.WorkspaceID, f.ontology.ID, id26("FJ-none"), id26("FU-ada")),
			store.ErrNotFound)
	})

	// A running step settles its batch before stopping, so the worker hears
	// the Stop at its next renewal and publishes what it found.
	t.Run("asks a running step to stop", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "ST2")
		job := f.queuedStep(t, "ST2", time.Now().Add(time.Hour))
		running(t, f.db, "w1", job.LoadID)
		require.NoError(t, f.db.StopFoundryLoadEnrichment(ctx, f.ontology.WorkspaceID, f.ontology.ID, job.LoadID, id26("FU-ada")))
		assert.Equal(t, model.FoundryEnrichmentRunning, f.step(t, job.LoadID).Status)
		signals, err := f.db.RenewFoundryLoadEnrichmentLease(ctx, job.LoadID, "w1", enrichmentProvider, time.Minute)
		require.NoError(t, err)
		assert.True(t, signals.Stop)
		assert.False(t, jobDue(t, f.db, f.ontology.ID, job.ID))
	})
}

func TestDeleteFoundryOntologyCancelsEnrichment(t *testing.T) {
	// Deleting the ontology cancels its job and its step in one transaction:
	// a running worker publishes nothing, and the step's files are swept.
	t.Run("cancels a running step and purges", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DEL1")
		job := f.queuedStep(t, "DEL1", time.Now().Add(time.Hour))
		running(t, f.db, "w1", job.LoadID)
		require.NoError(t, f.db.DeleteFoundryOntology(ctx, f.ontology.WorkspaceID, f.ontology.ID, id26("FN-DEL1")))
		row := f.step(t, job.LoadID)
		assert.True(t, row.CancelRequested)
		assert.NotNil(t, row.PurgedAt)
		checks, err := f.db.CheckFoundryLoadEnrichment(ctx, job.LoadID, enrichmentProvider)
		require.NoError(t, err)
		assert.True(t, checks.Gone)
		assert.True(t, checks.Cancel)
	})

	// A step still preparing waits for its job's attempt to end it, and a
	// deletion cancels that job: the step ends cancelled at once, so it
	// doesn't stay preparing forever. The attempt preparing it writes its
	// plan, carried and results until its lease renewal stops it, and no
	// row ever names them: the sweep removes its whole layout only a graph
	// lease past its end, once that attempt has stopped.
	t.Run("ends a preparing step cancelled, for the sweep", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DEL3")
		job := f.claimedLoad(t, "DEL3")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		require.NoError(t, f.db.DeleteFoundryOntology(ctx, f.ontology.WorkspaceID, f.ontology.ID, id26("FN-DEL3")))
		row := f.step(t, job.LoadID)
		assert.Equal(t, model.FoundryEnrichmentCancelled, row.Status)
		assert.NotNil(t, row.PurgedAt)
		_, err = f.db.QueueFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentQueue{
			PlanKey: "plan-DEL3", CarriedKey: "carried-DEL3", ClaimBy: time.Now().Add(time.Hour),
		})
		assert.ErrorIs(t, err, store.ErrNotFound, "the attempt that lost its job can't queue it")
		far := time.Now().Add(-time.Hour)
		sweeps, err := f.db.ListFoundryEnrichmentSweeps(ctx, far, far, time.Now().Add(-time.Minute), 10)
		require.NoError(t, err)
		assert.Empty(t, sweeps, "the attempt may still be writing its layout")
		sweeps, err = f.db.ListFoundryEnrichmentSweeps(ctx, far, far, time.Now().Add(time.Minute), 10)
		require.NoError(t, err)
		require.Len(t, sweeps, 1)
		assert.True(t, sweeps[0].WholeLayout, "every file of its layout goes")
	})

	// A parked job's queued step ends cancelled with its job.
	t.Run("ends a queued step cancelled", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DEL2")
		job := f.queuedStep(t, "DEL2", time.Now().Add(time.Hour))
		require.NoError(t, f.db.DeleteFoundryOntology(ctx, f.ontology.WorkspaceID, f.ontology.ID, id26("FN-DEL2")))
		assert.Equal(t, model.FoundryEnrichmentCancelled, f.step(t, job.LoadID).Status)
		cancelled, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, job.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobCancelled, cancelled.Status)
	})
}

func TestRefreshPreemptionCancelsEnrichment(t *testing.T) {
	// refreshing claims a refresh of the fixture's current load as the graph
	// worker "graph" and starts its People enrichment step.
	refreshing := func(t *testing.T, tag string) (foundryFixture, *model.FoundryGraphJob) {
		t.Helper()
		ctx := context.Background()
		f, connectorID, _ := refreshFixture(t, tag)
		require.NoError(t, f.db.OweFoundryRefreshes(ctx, f.ontology.WorkspaceID, connectorID, []string{"customers"}, time.Millisecond))
		queued, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, refreshJob(tag))
		require.NoError(t, err)
		require.True(t, queued)
		job := f.claimed(t, "graph", id26("RJ-"+tag))
		_, err = f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		return f, job
	}
	parked := func(t *testing.T, tag string) (foundryFixture, *model.FoundryGraphJob) {
		t.Helper()
		ctx := context.Background()
		f, job := refreshing(t, tag)
		claimBy := time.Now().Add(time.Hour)
		_, err := f.db.QueueFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentQueue{
			PlanKey: "plan-" + tag, CarriedKey: "carried-" + tag, ClaimBy: claimBy,
		})
		require.NoError(t, err)
		due, err := f.db.ParkFoundryGraphJob(ctx, job.ID, "graph", claimBy)
		require.NoError(t, err)
		require.False(t, due)
		return f, job
	}

	// A refresh parked for its lookups gives way to a user's load like any
	// refresh, and its step goes with its load: no worker claims it, and no
	// load is left waiting for it.
	t.Run("ends a parked refresh's queued step cancelled", func(t *testing.T) {
		ctx := context.Background()
		f, job := parked(t, "PEQ")
		f.load(t, "PEQU")
		assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, job.LoadID))
		row := f.step(t, job.LoadID)
		assert.Equal(t, model.FoundryEnrichmentCancelled, row.Status)
		assert.NotNil(t, row.EndedAt)
		assert.NotNil(t, row.PurgedAt)
		claimed, _, err := f.db.ClaimFoundryLoadEnrichment(ctx, "w1", enrichmentProvider, time.Minute, time.Hour, 2*time.Minute, 3)
		require.NoError(t, err)
		assert.Nil(t, claimed, "no worker looks up a deleted load's people")
	})

	// One still preparing would wait forever for the cancelled job's attempt:
	// it ends at once, that attempt can't queue it, and the sweep removes its
	// whole layout a graph lease on.
	t.Run("ends a preparing step cancelled, for the sweep", func(t *testing.T) {
		ctx := context.Background()
		f, job := refreshing(t, "PEP")
		f.load(t, "PEPU")
		row := f.step(t, job.LoadID)
		assert.Equal(t, model.FoundryEnrichmentCancelled, row.Status)
		assert.NotNil(t, row.PurgedAt)
		_, err := f.db.QueueFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentQueue{
			PlanKey: "plan-PEP", CarriedKey: "carried-PEP", ClaimBy: time.Now().Add(time.Hour),
		})
		assert.ErrorIs(t, err, store.ErrNotFound, "the attempt that lost its job can't queue it")
		far := time.Now().Add(-time.Hour)
		sweeps, err := f.db.ListFoundryEnrichmentSweeps(ctx, far, far, time.Now().Add(time.Minute), 10)
		require.NoError(t, err)
		var whole bool
		for _, sw := range sweeps {
			whole = whole || sw.LoadID == job.LoadID && sw.WholeLayout
		}
		assert.True(t, whole, "every file of its layout goes")
	})

	// A running step is asked to cancel, which publishes nothing.
	t.Run("asks a running step to cancel", func(t *testing.T) {
		ctx := context.Background()
		f, job := parked(t, "PER")
		running(t, f.db, "w1", job.LoadID)
		f.load(t, "PERU")
		row := f.step(t, job.LoadID)
		assert.Equal(t, model.FoundryEnrichmentRunning, row.Status)
		assert.True(t, row.CancelRequested)
		assert.NotNil(t, row.PurgedAt)
		checks, err := f.db.CheckFoundryLoadEnrichment(ctx, job.LoadID, enrichmentProvider)
		require.NoError(t, err)
		assert.True(t, checks.Cancel)
	})

	// A step that ended keeps its results for the next load's carry-forward.
	t.Run("leaves an ended step as it is", func(t *testing.T) {
		ctx := context.Background()
		f, job := parked(t, "PEE")
		require.NoError(t, f.db.StopFoundryLoadEnrichment(ctx, f.ontology.WorkspaceID, f.ontology.ID, job.LoadID, id26("FU-owner")))
		f.load(t, "PEEU")
		row := f.step(t, job.LoadID)
		assert.Equal(t, model.FoundryEnrichmentPartial, row.Status)
		assert.Nil(t, row.PurgedAt)
		assert.False(t, row.CancelRequested)
	})
}

func TestListFoundryEnrichmentSweeps(t *testing.T) {
	// far is a cutoff nothing reached; late one every step here ended
	// before: an attempt that lost its job has stopped writing.
	far, late := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	// Every row the sweep has finished with stays in the table, one per load
	// ever enriched, and so do the carried and profiles of every load kept.
	// A sweep must read each row once, not each against every other: that
	// grows with the square of the loads, and a year in would outlast the
	// sweep's timeout, which retention and consent purges share. Measured as
	// the rows one session reads.
	t.Run("reads each finished row once", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SW6")
		const finished, kept = 400, 200
		var steps, loads []string
		var stepArgs, loadArgs []any
		for i := range finished + kept {
			load := id26(fmt.Sprintf("FL-SW6-%04d", i))
			if i < finished {
				steps = append(steps, "(?, ?, ?, ?, 'done', NOW(6) - INTERVAL ? SECOND, NOW(6) - INTERVAL 100 DAY, NOW(6) - INTERVAL 99 DAY, NULL, NULL)")
				stepArgs = append(stepArgs, load, f.ontology.WorkspaceID, f.ontology.ID, id26(fmt.Sprintf("FJ-SW6-%04d", i)), finished+kept-i)
				continue
			}
			steps = append(steps, "(?, ?, ?, ?, 'done', NOW(6) - INTERVAL ? SECOND, NOW(6) - INTERVAL 100 DAY, NOW(6) - INTERVAL 99 DAY, ?, ?)")
			stepArgs = append(stepArgs, load, f.ontology.WorkspaceID, f.ontology.ID, id26(fmt.Sprintf("FJ-SW6-%04d", i)), finished+kept-i,
				"carried-"+load, "profiles-"+load)
			loads = append(loads, "(?, ?, ?, ?, 'ready', NOW(6))")
			loadArgs = append(loadArgs, load, f.ontology.WorkspaceID, f.ontology.ID, id26("FV-SW6"))
		}
		_, err := testDB.Exec(`INSERT INTO foundry_load_enrichments (load_id, workspace_id, ontology_id, job_id, status, created_at, ended_at,
  calls_deleted_at, carried_key, profiles_key) VALUES `+strings.Join(steps, ", "), stepArgs...)
		require.NoError(t, err)
		_, err = testDB.Exec(`INSERT INTO foundry_loads (id, workspace_id, ontology_id, version_id, status, finished_at) VALUES `+
			strings.Join(loads, ", "), loadArgs...)
		require.NoError(t, err)
		session, err := openWithRetry(testDSN, 30, 500*time.Millisecond)
		require.NoError(t, err)
		defer session.Close()
		session.SetMaxOpenConns(1) // one connection: its counters are the sweep's
		reads := func() int64 {
			t.Helper()
			rows, err := session.QueryContext(ctx, `SHOW SESSION STATUS LIKE 'Handler_read%'`)
			require.NoError(t, err)
			defer rows.Close()
			var total int64
			for rows.Next() {
				var name string
				var n int64
				require.NoError(t, rows.Scan(&name, &n))
				total += n
			}
			require.NoError(t, rows.Err())
			return total
		}
		before := reads()
		sweeps, err := store.NewMySQL(session, hostWorkspaces{}).ListFoundryEnrichmentSweeps(ctx, far, far, late, 100)
		require.NoError(t, err)
		read := reads() - before
		assert.Empty(t, sweeps, "the kept loads keep their files")
		t.Logf("%d rows read for %d finished steps and %d kept", read, finished, kept)
		assert.Less(t, read, int64(5*(finished+kept)), "rows read for %d steps", finished+kept)
	})

	// Personal data lives only as long as it is needed: the plan until the
	// step ends, raw calls for the retention, carried and profiles while the
	// load exists, and results until superseded.
	t.Run("lists what each ended step may delete", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SW1")
		job := f.queuedStep(t, "SW1", time.Now().Add(time.Hour))
		running(t, f.db, "w1", job.LoadID)
		sweeps, err := f.db.ListFoundryEnrichmentSweeps(ctx, far, far, late, 10)
		require.NoError(t, err)
		assert.Empty(t, sweeps, "a running step keeps its plan")

		_, err = testDB.Exec(`UPDATE foundry_load_enrichments SET batch_cursor=3 WHERE load_id=?`, job.LoadID)
		require.NoError(t, err)
		require.NoError(t, f.db.EndFoundryLoadEnrichment(ctx, job.LoadID, "w1", model.EndFoundryEnrichment(model.FoundryEnrichmentDone),
			&model.FoundryEnrichmentPublished{ProfilesKey: "profiles-SW1", ResultsKey: "results-SW1"}, model.FoundryEnrichmentCounts{}))
		sweeps, err = f.db.ListFoundryEnrichmentSweeps(ctx, far, far, late, 10)
		require.NoError(t, err)
		require.Len(t, sweeps, 1)
		assert.Equal(t, "plan-SW1", *sweeps[0].Plan)
		assert.Equal(t, 3, sweeps[0].Batches)
		assert.Nil(t, sweeps[0].Profiles, "the load still exists")
		assert.Nil(t, sweeps[0].Results, "the newest results feed carry-forward")
		assert.False(t, sweeps[0].Calls, "raw calls stay for the retention")
		require.NoError(t, f.db.ClearFoundryEnrichmentFiles(ctx, sweeps[0]))
		sweeps, err = f.db.ListFoundryEnrichmentSweeps(ctx, far, far, late, 10)
		require.NoError(t, err)
		assert.Empty(t, sweeps)

		sweeps, err = f.db.ListFoundryEnrichmentSweeps(ctx, time.Now().Add(time.Hour), far, late, 10)
		require.NoError(t, err)
		require.Len(t, sweeps, 1, "past the retention")
		assert.True(t, sweeps[0].Calls)
		require.NoError(t, f.db.ClearFoundryEnrichmentFiles(ctx, sweeps[0]))
		assert.NotNil(t, f.step(t, job.LoadID).CallsDeletedAt)

		_, err = testDB.Exec(`UPDATE foundry_loads SET status='deleted', deleted_at=NOW(6), finished_at=NOW(6) WHERE id=?`, job.LoadID)
		require.NoError(t, err)
		sweeps, err = f.db.ListFoundryEnrichmentSweeps(ctx, far, far, late, 10)
		require.NoError(t, err)
		require.Len(t, sweeps, 1, "the load is gone")
		assert.Equal(t, "carried-SW1", *sweeps[0].Carried)
		assert.Equal(t, "profiles-SW1", *sweeps[0].Profiles)
		assert.Nil(t, sweeps[0].Results, "still the newest results, whatever became of the load")
	})

	// Carry-forward reads only the newest results, once a prepare reading
	// the older ones has had time to finish.
	t.Run("lists results a newer step superseded", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SW2")
		older := f.queuedStep(t, "SW2-1", time.Now().Add(time.Hour))
		require.NoError(t, f.db.StopFoundryLoadEnrichment(ctx, f.ontology.WorkspaceID, f.ontology.ID, older.LoadID, id26("FU-ada")))
		_, err := testDB.Exec(`UPDATE foundry_load_enrichments SET results_key='results-old', plan_key=NULL, published_at=NOW(6) WHERE load_id=?`,
			older.LoadID)
		require.NoError(t, err)
		_, err = testDB.Exec(`UPDATE foundry_load_jobs SET status='failed', active_key=NULL, finished_at=NOW(6) WHERE id=?`, older.ID)
		require.NoError(t, err)
		time.Sleep(2 * time.Millisecond)
		newer := f.queuedStep(t, "SW2-2", time.Now().Add(time.Hour))
		_, err = testDB.Exec(`UPDATE foundry_load_enrichments SET results_key='results-new', published_at=NOW(6) WHERE load_id=?`, newer.LoadID)
		require.NoError(t, err)
		sweeps, err := f.db.ListFoundryEnrichmentSweeps(ctx, far, far, late, 10)
		require.NoError(t, err)
		assert.Empty(t, sweeps, "within the margin")
		sweeps, err = f.db.ListFoundryEnrichmentSweeps(ctx, far, time.Now().Add(time.Minute), late, 10)
		require.NoError(t, err)
		require.Len(t, sweeps, 1)
		assert.Equal(t, older.LoadID, sweeps[0].LoadID)
		assert.Equal(t, "results-old", *sweeps[0].Results)
		latest, err := f.db.GetLatestFoundryEnrichmentResults(ctx, f.ontology.ID)
		require.NoError(t, err)
		assert.Equal(t, newer.LoadID, latest.LoadID)
	})

	// An attempt writes a step's plan and carried files before its row names
	// them, and its prepare may fail after, or its job may end with the step
	// still preparing: the step ends with the job, and every file of its
	// layout goes, as no load applies one and no row may name them.
	t.Run("ends a step its job left preparing and sweeps its whole layout", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SW4")
		job := f.claimedLoad(t, "SW4")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		sweeps, err := f.db.ListFoundryEnrichmentSweeps(ctx, far, far, late, 10)
		require.NoError(t, err)
		assert.Empty(t, sweeps, "a preparing step may still be queued")
		_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobFailed, Error: "invalid_mappings", Next: next("SW4"),
		})
		require.NoError(t, err)
		row := f.step(t, job.LoadID)
		assert.Equal(t, model.FoundryEnrichmentFailed, row.Status, "no attempt comes back to it")
		assert.NotNil(t, row.EndedAt)
		sweeps, err = f.db.ListFoundryEnrichmentSweeps(ctx, far, far, far, 10)
		require.NoError(t, err)
		assert.Empty(t, sweeps, "not before a graph lease past its end")
		sweeps, err = f.db.ListFoundryEnrichmentSweeps(ctx, far, far, late, 10)
		require.NoError(t, err)
		require.Len(t, sweeps, 1)
		assert.True(t, sweeps[0].WholeLayout)
		assert.True(t, sweeps[0].Calls)
		require.NoError(t, f.db.ClearFoundryEnrichmentFiles(ctx, sweeps[0]))
		sweeps, err = f.db.ListFoundryEnrichmentSweeps(ctx, far, far, late, 10)
		require.NoError(t, err)
		assert.Empty(t, sweeps, "listed once")

		g := newFoundryFixture(t, "SW5")
		failed := g.claimedLoad(t, "SW5")
		_, err = g.db.StartFoundryLoadEnrichment(ctx, "graph", failed)
		require.NoError(t, err)
		require.NoError(t, g.db.EndPreparedFoundryLoadEnrichment(ctx, failed.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentFailed), Applied: model.FoundryEnrichmentNone,
		}))
		sweeps, err = g.db.ListFoundryEnrichmentSweeps(ctx, far, far, late, 10)
		require.NoError(t, err)
		require.Len(t, sweeps, 1, "a prepare that failed after its uploads")
		assert.True(t, sweeps[0].WholeLayout)
	})

	// A purged step's files go once no writing load applies them, so a load
	// mid-Ingest never loses the file it stages.
	t.Run("waits for a writing load before deleting purged files", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SW3")
		job := f.claimedLoad(t, "SW3")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), PlanKey: "plan-SW3", CarriedKey: "carried-SW3",
			ResultsKey: "results-SW3", Applied: model.FoundryEnrichmentApplied{Key: "carried-SW3", SHA256: "cc"},
		}))
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(f.ontology.WorkspaceID, "off", false, 0)))
		sweeps, err := f.db.ListFoundryEnrichmentSweeps(ctx, far, far, late, 10)
		require.NoError(t, err)
		require.Len(t, sweeps, 1)
		assert.Equal(t, "plan-SW3", *sweeps[0].Plan, "the plan goes at once")
		assert.Nil(t, sweeps[0].Carried, "the writing load stages it")
		assert.Nil(t, sweeps[0].Results)
		assert.False(t, sweeps[0].WholeLayout, "the rest of its layout waits for the writing load too")
		require.NoError(t, f.db.ClearFoundryEnrichmentFiles(ctx, sweeps[0]))

		_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("SW3"),
		})
		require.NoError(t, err)
		sweeps, err = f.db.ListFoundryEnrichmentSweeps(ctx, far, far, late, 10)
		require.NoError(t, err)
		require.Len(t, sweeps, 1)
		assert.Equal(t, "carried-SW3", *sweeps[0].Carried)
		assert.Equal(t, "results-SW3", *sweeps[0].Results)
		assert.True(t, sweeps[0].Calls)
		assert.True(t, sweeps[0].WholeLayout)
		require.NoError(t, f.db.ClearFoundryEnrichmentFiles(ctx, sweeps[0]))
		row := f.step(t, job.LoadID)
		assert.Nil(t, row.CarriedKey)
		assert.Nil(t, row.ResultsKey)
		assert.Equal(t, "carried-SW3", row.Applied.Key, "what the load applied stays on record")
		_, err = f.db.GetLatestFoundryEnrichmentApplied(ctx, f.ontology.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "a purged file is never applied again")
	})
}

func TestAdvanceFoundryEnrichmentPurge(t *testing.T) {
	purgeLoad := func(tag string) *model.FoundryGraphJob {
		return &model.FoundryGraphJob{ID: id26("FJ-" + tag)}
	}

	// The purge's clean load waits for a live job, is queued once none is,
	// is retried daily up to three times, and ends the purge once current.
	t.Run("queues a clean load past a live job and finishes once it is current", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "PU1")
		ws := f.ontology.WorkspaceID
		dirty := f.claimedLoad(t, "PU1-1")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", dirty)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, dirty.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "carried", ResultsKey: "results",
			Applied: model.FoundryEnrichmentApplied{Key: "carried", SHA256: "cc"},
		}))
		_, err = f.db.FinishFoundryGraphJob(ctx, dirty.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("PU1-1"),
		})
		require.NoError(t, err)
		f.load(t, "PU1-live")
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(ws, "off", false, 0)))

		step, err := f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, purgeLoad("PU1-clean"), id26("FN-PU1"), 24*time.Hour, 3)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentPurgeWaiting, step, "a live job goes first")

		live, err := f.db.ClaimFoundryGraphJob(ctx, "graph", time.Minute)
		require.NoError(t, err)
		_, err = f.db.FinishFoundryGraphJob(ctx, live.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobFailed, Error: "boom", Next: next("PU1-live"),
		})
		require.NoError(t, err)
		step, err = f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, purgeLoad("PU1-clean"), id26("FN-PU1"), 24*time.Hour, 3)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentPurgeQueued, step)
		clean := f.liveJob(t)
		require.NotNil(t, clean)
		assert.Equal(t, id26("FJ-PU1-clean"), clean.ID)
		assert.Equal(t, id26("FV-PU1-1"), clean.VersionID, "the current load's version again")
		assert.Nil(t, clean.RequestedByUserID)

		// The clean load applies nothing and becomes current.
		job, err := f.db.ClaimFoundryGraphJob(ctx, "graph", time.Minute)
		require.NoError(t, err)
		_, err = f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentOff), Applied: model.FoundryEnrichmentNone,
		}))
		_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("PU1-clean"),
		})
		require.NoError(t, err)
		step, err = f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, purgeLoad("PU1-again"), id26("FN-PU1"), 24*time.Hour, 3)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentPurgeDone, step)
		assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, dirty.LoadID), "the load that applied purged profiles goes")
		deleting := f.liveJob(t)
		require.NotNil(t, deleting)
		assert.Equal(t, model.FoundryGraphJobDelete, deleting.Kind)
		_, err = f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, purgeLoad("PU1-more"), id26("FN-PU1"), 24*time.Hour, 3)
		assert.ErrorIs(t, err, store.ErrNotFound, "nothing left to purge")
	})

	// A load still writing when consent is withdrawn may have applied
	// profiles before, and becomes current when it ends: the purge can't
	// call the ontology clean while it writes, though no load is current yet.
	t.Run("waits for a writing load that applied profiles, with none current", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "PU4")
		writing := f.claimedLoad(t, "PU4-1")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", writing)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, writing.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "c", ResultsKey: "r",
			Applied: model.FoundryEnrichmentApplied{Key: "c", SHA256: "cc"},
		}))
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(f.ontology.WorkspaceID, "off", false, 0)))

		step, err := f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, purgeLoad("PU4-clean"), id26("FN-PU4"), 24*time.Hour, 3)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentPurgeWaiting, step, "the writing load may become current")

		_, err = f.db.FinishFoundryGraphJob(ctx, writing.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("PU4-1"),
		})
		require.NoError(t, err)
		require.Equal(t, writing.LoadID, *f.ontologyNow(t).CurrentLoadID)
		step, err = f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, purgeLoad("PU4-clean"), id26("FN-PU4"), 24*time.Hour, 3)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentPurgeQueued, step, "the purged load became current, so a clean load replaces it")
	})

	// Removing withdrawn profiles can't wait for next month: the clean load
	// is queued even once the month's load spend is gone, which a user's
	// load is refused for.
	t.Run("queues the clean load past the month's spend", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "PU3")
		dirty := f.claimedLoad(t, "PU3-1")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", dirty)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, dirty.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "c", ResultsKey: "r",
			Applied: model.FoundryEnrichmentApplied{Key: "c", SHA256: "cc"},
		}))
		_, err = f.db.FinishFoundryGraphJob(ctx, dirty.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("PU3-1"),
		})
		require.NoError(t, err)
		_, err = testDB.Exec(`UPDATE foundry_load_jobs SET usd_micros=1000000000 WHERE id=?`, dirty.ID)
		require.NoError(t, err)
		spent, err := f.db.SumFoundryGraphJobSpend(ctx, f.ontology.WorkspaceID, time.Now().UTC().Add(-time.Hour))
		require.NoError(t, err)
		require.GreaterOrEqual(t, spent, int64(1_000_000_000))
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(f.ontology.WorkspaceID, "off", false, 0)))
		step, err := f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, purgeLoad("PU3-clean"), id26("FN-PU3"), 24*time.Hour, 3)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentPurgeQueued, step)
	})

	// The clean load re-reads the documents and may run out of its USD
	// budget: it ends partial, and a partial load never replaces a current
	// one. It holds no purged profiles, so the purge makes it current
	// rather than let the withdrawn values be served until the budget
	// allows a clean load that is ready.
	t.Run("makes a partial clean load current", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "PU5")
		dirty := f.claimedLoad(t, "PU5-1")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", dirty)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, dirty.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "c", ResultsKey: "r",
			Applied: model.FoundryEnrichmentApplied{Key: "c", SHA256: "cc"},
		}))
		_, err = f.db.FinishFoundryGraphJob(ctx, dirty.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("PU5-1"),
		})
		require.NoError(t, err)
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(f.ontology.WorkspaceID, "off", false, 0)))
		step, err := f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, purgeLoad("PU5-clean"), id26("FN-PU5"), 24*time.Hour, 3)
		require.NoError(t, err)
		require.Equal(t, model.FoundryEnrichmentPurgeQueued, step)

		job, err := f.db.ClaimFoundryGraphJob(ctx, "graph", time.Minute)
		require.NoError(t, err)
		_, err = f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentOff), Applied: model.FoundryEnrichmentNone,
		}))
		_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobBudgetExhausted, IngestDone: true, Error: "budget", Next: next("PU5-clean"),
		})
		require.NoError(t, err)
		require.Equal(t, model.FoundryLoadPartial, f.loadStatus(t, job.LoadID))
		require.Equal(t, dirty.LoadID, *f.ontologyNow(t).CurrentLoadID, "a partial load doesn't replace the current one")

		step, err = f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, purgeLoad("PU5-again"), id26("FN-PU5"), 24*time.Hour, 3)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryEnrichmentPurgeDone, step)
		assert.Equal(t, job.LoadID, *f.ontologyNow(t).CurrentLoadID, "the clean load serves")
		assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, dirty.LoadID), "the load that applied purged profiles goes")
	})

	// However many purges an operator still has to look at, a newer purge
	// still gets its clean load: exhausted purges are listed last.
	t.Run("lists exhausted purges after the others", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		stuck := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-PU6-A"), "PU6-A")}
		fresh := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-PU6-B"), "PU6-B")}
		_, err := testDB.Exec(`INSERT INTO foundry_enrichment_purges (ontology_id, workspace_id, requested_at, attempts) VALUES
  (?, ?, NOW(6) - INTERVAL 2 DAY, 3), (?, ?, NOW(6), 0)`,
			stuck.ontology.ID, stuck.ontology.WorkspaceID, fresh.ontology.ID, fresh.ontology.WorkspaceID)
		require.NoError(t, err)
		purges, err := db.ListFoundryEnrichmentPurges(ctx, 3, 1)
		require.NoError(t, err)
		require.Len(t, purges, 1)
		assert.Equal(t, fresh.ontology.ID, purges[0].OntologyID)
		purges, err = db.ListFoundryEnrichmentPurges(ctx, 3, 10)
		require.NoError(t, err)
		require.Len(t, purges, 2)
		assert.Equal(t, stuck.ontology.ID, purges[1].OntologyID, "still looked at, once the others are")
	})

	// A clean load that keeps failing is retried daily, then left for an
	// operator after the last attempt.
	t.Run("retries daily and gives up after the last attempt", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "PU2")
		dirty := f.claimedLoad(t, "PU2-1")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", dirty)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, dirty.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "c", ResultsKey: "r",
			Applied: model.FoundryEnrichmentApplied{Key: "c", SHA256: "cc"},
		}))
		_, err = f.db.FinishFoundryGraphJob(ctx, dirty.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("PU2-1"),
		})
		require.NoError(t, err)
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(f.ontology.WorkspaceID, "off", false, 0)))
		for attempt := 1; attempt <= 3; attempt++ {
			tag := "PU2-clean-" + string(rune('0'+attempt))
			step, err := f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, purgeLoad(tag), id26("FN-PU2"), 24*time.Hour, 3)
			require.NoError(t, err)
			require.Equal(t, model.FoundryEnrichmentPurgeQueued, step, "attempt %d", attempt)
			job, err := f.db.ClaimFoundryGraphJob(ctx, "graph", time.Minute)
			require.NoError(t, err)
			_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, "graph", model.FoundryGraphJobOutcome{
				Status: model.FoundryGraphJobFailed, Error: "boom", Next: next(tag),
			})
			require.NoError(t, err)
			step, err = f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, purgeLoad(tag+"x"), id26("FN-PU2"), 24*time.Hour, 3)
			require.NoError(t, err)
			if attempt < 3 {
				require.Equal(t, model.FoundryEnrichmentPurgeWaiting, step, "a day between attempts")
				_, err = testDB.Exec(`UPDATE foundry_enrichment_purges SET requested_at=NOW(6) - INTERVAL 25 HOUR WHERE ontology_id=?`, f.ontology.ID)
				require.NoError(t, err)
			} else {
				assert.Equal(t, model.FoundryEnrichmentPurgeExhausted, step)
			}
		}
	})
}

func TestFoundryEnrichmentSettings(t *testing.T) {
	// Settings show the newest change, who turned enrichment on and when (a
	// cap change keeps that), the month's credits and the purges left.
	t.Run("shows the setting, who allowed it, the month and the purges", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SE1")
		ws := f.ontology.WorkspaceID
		month := time.Now().UTC().Add(-time.Hour)
		none, err := f.db.GetFoundryEnrichmentSettings(ctx, ws, month)
		require.NoError(t, err)
		assert.Nil(t, none.Consent, "never set")
		assert.Nil(t, none.Enabled)

		on := consent(ws, "on", true, 50_000)
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, on))
		time.Sleep(2 * time.Millisecond)
		raised := consent(ws, "raised", true, 80_000)
		raised.UserID = id26("FU-other")
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, raised))
		_, err = testDB.Exec(`INSERT INTO foundry_enrichment_charges (load_id, batch, workspace_id, credits, uncertain_credits)
VALUES (?, 0, ?, 400, 20)`, id26("FJ-SE1-charged"), ws)
		require.NoError(t, err)
		got, err := f.db.GetFoundryEnrichmentSettings(ctx, ws, month)
		require.NoError(t, err)
		assert.Equal(t, int64(80_000), got.Consent.MonthlyCreditCap, "the newest change is the setting")
		require.NotNil(t, got.Enabled)
		assert.Equal(t, on.ID, got.Enabled.ID, "a cap change doesn't move who allowed it")
		assert.Equal(t, int64(420), got.Ledger.WorkspaceCharged)
		assert.Zero(t, got.PurgesPending)

		// Load 1 applied profiles; turning consent off purges it, and turning it on again starts afresh.
		job := f.claimedLoad(t, "SE1-1")
		_, err = f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "c", ResultsKey: "r",
			Applied: model.FoundryEnrichmentApplied{Key: "c", SHA256: "cc"},
		}))
		time.Sleep(2 * time.Millisecond)
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(ws, "off", false, 80_000)))
		off, err := f.db.GetFoundryEnrichmentSettings(ctx, ws, month)
		require.NoError(t, err)
		assert.False(t, off.Consent.Enabled)
		assert.Nil(t, off.Enabled, "off has no one who allowed it")
		assert.Equal(t, 1, off.PurgesPending)
		time.Sleep(2 * time.Millisecond)
		again := consent(ws, "again", true, 10_000)
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, again))
		back, err := f.db.GetFoundryEnrichmentSettings(ctx, ws, month)
		require.NoError(t, err)
		assert.Equal(t, again.ID, back.Enabled.ID, "allowed again since it was last off")
	})
}

func TestRecordFoundryLoadEnrichmentIngest(t *testing.T) {
	// What the load's Ingest bound is recorded by the attempt holding the job.
	t.Run("records what Ingest bound, fenced on the job", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "RI1")
		job := f.claimedLoad(t, "RI1")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", job)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "c", ResultsKey: "r",
			Counts: model.FoundryEnrichmentCounts{People: 9, Carried: 4, Profiles: 4}, Applied: model.FoundryEnrichmentApplied{Key: "c", SHA256: "cc"},
		}))
		require.NoError(t, f.db.RecordFoundryLoadEnrichmentIngest(ctx, job.LoadID, "graph", 3, 1))
		counts := f.step(t, job.LoadID).Counts
		assert.Equal(t, model.FoundryEnrichmentCounts{People: 9, Carried: 4, Profiles: 4, Applied: 3, Unmatched: 1}, counts)
		assert.ErrorIs(t, f.db.RecordFoundryLoadEnrichmentIngest(ctx, job.LoadID, "stale", 4, 0), store.ErrNotFound)
		assert.Error(t, f.db.RecordFoundryLoadEnrichmentIngest(ctx, job.LoadID, "graph", -1, 0))
	})

	// A worker past its deadline publishes after the load went on with the
	// carried profiles: its counts must not erase what the Ingest recorded.
	t.Run("a late worker keeps what Ingest recorded", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "RI2")
		job := f.queuedStep(t, "RI2", time.Now().Add(time.Hour))
		running(t, f.db, "w1", job.LoadID)
		_, err := testDB.Exec(`UPDATE foundry_load_jobs SET next_attempt_at=NOW(6) WHERE id=?`, job.ID)
		require.NoError(t, err)
		again, err := f.db.ClaimFoundryGraphJob(ctx, "graph", time.Minute)
		require.NoError(t, err)
		require.Equal(t, job.ID, again.ID)
		_, err = f.db.ApplyFoundryLoadEnrichment(ctx, job.LoadID, "graph", model.FoundryEnrichmentApplied{Key: "carried-RI2", SHA256: "cc"})
		require.NoError(t, err)
		require.NoError(t, f.db.RecordFoundryLoadEnrichmentIngest(ctx, job.LoadID, "graph", 3, 1))

		_, err = f.db.ReserveFoundryEnrichmentCredits(ctx, job.LoadID, "w1", enrichmentProvider, 1000, generous)
		require.NoError(t, err)
		begun, err := f.db.BeginFoundryEnrichmentBatch(ctx, job.LoadID, "w1", enrichmentProvider, 0, 2, generous)
		require.NoError(t, err)
		require.Equal(t, 2, begun.N)
		require.NoError(t, f.db.ChargeFoundryEnrichmentBatch(ctx, job.LoadID, "w1", model.FoundryEnrichmentCharge{Batch: 0, Credits: 20},
			model.FoundryEnrichmentCounts{ToLookUp: 2, LookedUp: 2, Found: 1}))
		assert.Equal(t, int64(3), f.step(t, job.LoadID).Counts.Applied)
		require.NoError(t, f.db.EndFoundryLoadEnrichment(ctx, job.LoadID, "w1", model.EndFoundryEnrichment(model.FoundryEnrichmentDone),
			&model.FoundryEnrichmentPublished{ProfilesKey: "profiles", ResultsKey: "results"},
			model.FoundryEnrichmentCounts{ToLookUp: 2, LookedUp: 2, Found: 1, Profiles: 1, Credits: 20}))
		counts := f.step(t, job.LoadID).Counts
		assert.Equal(t, model.FoundryEnrichmentCounts{ToLookUp: 2, LookedUp: 2, Found: 1, Profiles: 1, Credits: 20, Applied: 3, Unmatched: 1}, counts)
	})
}

func TestSetCurrentFoundryLoadPurged(t *testing.T) {
	// Once consent is withdrawn, a load that applied profiles can't serve
	// reads again; one that applied none still can.
	t.Run("refuses a load that applied purged profiles", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SC1")
		ws := f.ontology.WorkspaceID
		dirty := f.claimedLoad(t, "SC1-1")
		_, err := f.db.StartFoundryLoadEnrichment(ctx, "graph", dirty)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, dirty.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "c", ResultsKey: "r",
			Applied: model.FoundryEnrichmentApplied{Key: "c", SHA256: "cc"},
		}))
		_, err = f.db.FinishFoundryGraphJob(ctx, dirty.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("SC1-1"),
		})
		require.NoError(t, err)
		clean := f.claimedLoad(t, "SC1-2")
		_, err = f.db.StartFoundryLoadEnrichment(ctx, "graph", clean)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, clean.LoadID, "graph", model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), Applied: model.FoundryEnrichmentNone,
		}))
		_, err = f.db.FinishFoundryGraphJob(ctx, clean.ID, "graph", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("SC1-2"),
		})
		require.NoError(t, err)
		require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(ws, "off", false, 0)))

		_, err = f.db.SetCurrentFoundryLoad(ctx, ws, f.ontology.ID, dirty.LoadID, extractJob("SC1-x"), noBudget)
		assert.ErrorIs(t, err, store.FoundryLoadPurged)
		assert.Equal(t, clean.LoadID, *f.ontologyNow(t).CurrentLoadID)
		_, err = f.db.SetCurrentFoundryLoad(ctx, ws, f.ontology.ID, clean.LoadID, extractJob("SC1-y"), noBudget)
		assert.NoError(t, err, "a load that applied no profiles stays servable")
	})
}

func TestCancelWorkspaceFoundryWork(t *testing.T) {
	// Deleting a workspace ends its Foundry work, as deleting each ontology
	// does, in the transaction that marks it deleting: its live jobs are
	// cancelled and the loads they wrote fail, a running worker hears the
	// cancel, every step's files are swept, and a pending purge ends, as the
	// workspace's databases go with it.
	t.Run("cancels every ontology's jobs and steps, and ends its purges", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "CW1")
		ws := f.ontology.WorkspaceID
		parked := f.queuedStep(t, "CW1-1", time.Now().Add(2*time.Hour))
		other := foundryFixture{db: f.db, ontology: sampleOntology(t, f.db, ws, "CW1-other")}
		otherJob := other.queuedStep(t, "CW1-2", time.Now().Add(time.Hour))
		running(t, f.db, "w1", otherJob.LoadID)
		_, err := testDB.Exec(`INSERT INTO foundry_enrichment_purges (ontology_id, workspace_id) VALUES (?, ?)`, other.ontology.ID, ws)
		require.NoError(t, err)
		elsewhere := foundryFixture{db: f.db, ontology: sampleOntology(t, f.db, id26("FWS-CW1-elsewhere"), "CW1-elsewhere")}
		untouched := elsewhere.queuedStep(t, "CW1-3", time.Now().Add(time.Hour))
		_, err = testDB.Exec(`INSERT INTO foundry_enrichment_purges (ontology_id, workspace_id) VALUES (?, ?)`,
			elsewhere.ontology.ID, elsewhere.ontology.WorkspaceID)
		require.NoError(t, err)

		// The host marks the workspace deleting and cancels its work in one
		// transaction of its own (store.InTx).
		tx, err := testDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `UPDATE workspaces SET status='deleting' WHERE id=?`, ws)
		require.NoError(t, err)
		require.NoError(t, store.InTx(tx, hostWorkspaces{}).CancelWorkspaceFoundryWork(ctx, ws))
		require.NoError(t, tx.Commit())
		for _, o := range []foundryFixture{f, other} {
			assert.Nil(t, o.liveJob(t), "no job of the workspace stays live")
		}
		for _, job := range []*model.FoundryGraphJob{parked, otherJob} {
			cancelled, err := f.db.GetFoundryGraphJob(ctx, job.OntologyID, job.ID)
			require.NoError(t, err)
			assert.Equal(t, model.FoundryGraphJobCancelled, cancelled.Status)
			assert.Equal(t, model.FoundryLoadFailed, foundryFixture{db: f.db, ontology: &model.FoundryOntology{ID: job.OntologyID}}.
				loadStatus(t, job.LoadID), "no load stays writing, which the sweep would wait on")
		}
		assert.Equal(t, model.FoundryEnrichmentCancelled, f.step(t, parked.LoadID).Status)
		run := other.step(t, otherJob.LoadID)
		assert.True(t, run.CancelRequested)
		signals, err := f.db.RenewFoundryLoadEnrichmentLease(ctx, otherJob.LoadID, "w1", enrichmentProvider, time.Minute)
		require.NoError(t, err)
		assert.True(t, signals.Cancel)
		for _, loadID := range []string{parked.LoadID, otherJob.LoadID} {
			assert.NotNil(t, f.step(t, loadID).PurgedAt)
		}
		purges, err := f.db.ListFoundryEnrichmentPurges(ctx, 3, 10)
		require.NoError(t, err)
		require.Len(t, purges, 1, "the deleted workspace's purge ends")
		assert.Equal(t, elsewhere.ontology.ID, purges[0].OntologyID)
		assert.Equal(t, model.FoundryEnrichmentQueued, elsewhere.step(t, untouched.LoadID).Status, "another workspace's step is left alone")
		assert.NotNil(t, elsewhere.liveJob(t), "another workspace's job is left alone")
	})
}

func TestParkedJobOnAnOldBinary(t *testing.T) {
	// After a backend rollback, code that knows no enrichment step claims the
	// parked job at provisioning and fails it on the contract check (or the
	// sidecar refuses the item): the load fails, nothing becomes current, and
	// the step ends with its job, so no worker spends credits on it once the
	// new code is back and the sweep deletes its files.
	t.Run("fails the load cleanly and the step ends with it", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "OB1")
		job := f.queuedStep(t, "OB1", time.Now().Add(time.Hour))
		_, err := testDB.Exec(`UPDATE foundry_load_jobs SET next_attempt_at=NOW(6) WHERE id=?`, job.ID)
		require.NoError(t, err)
		old, err := f.db.ClaimFoundryGraphJob(ctx, "old", time.Minute)
		require.NoError(t, err)
		require.Equal(t, model.FoundryGraphJobProvisioning, old.Phase, "an old binary sees a load that has yet to ingest")
		_, err = f.db.FinishFoundryGraphJob(ctx, old.ID, "old", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobFailed, Error: "the ontology agent speaks contract 3.9.0, not 3.8.0", Next: next("OB1"),
		})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryLoadFailed, f.loadStatus(t, job.LoadID))
		assert.Nil(t, f.ontologyNow(t).CurrentLoadID)

		assert.Equal(t, model.FoundryEnrichmentFailed, f.step(t, job.LoadID).Status)
		claimed, _, err := f.db.ClaimFoundryLoadEnrichment(ctx, "w1", enrichmentProvider, time.Minute, time.Hour, 2*time.Minute, 3)
		require.NoError(t, err)
		assert.Nil(t, claimed, "no lookup runs for the failed load")
		failed, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, job.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobFailed, failed.Status, "a failed job stays failed")
	})
}

func TestListFoundryLoadEnrichments(t *testing.T) {
	// A list of loads shows each one's step; a load whose version enriches
	// no one has none.
	t.Run("returns the steps of the loads that have one", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "LE1")
		job := f.queuedStep(t, "LE1-1", time.Now().Add(time.Hour))
		other := foundryFixture{db: f.db, ontology: sampleOntology(t, f.db, f.ontology.WorkspaceID, "LE1-other")}
		plain := other.load(t, "LE1-2")
		got, err := f.db.ListFoundryLoadEnrichments(ctx, []string{job.LoadID, plain, id26("FJ-none")})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, model.FoundryEnrichmentQueued, got[job.LoadID].Status)
		none, err := f.db.ListFoundryLoadEnrichments(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, none)
	})
}
