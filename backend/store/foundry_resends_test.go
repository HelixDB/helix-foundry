package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resendFixture is an ontology whose current load holds two documents, each
// extracted with a review that holds a claim back.
type resendFixture struct {
	foundryFixture
	load   string
	a, b   string
	worker string
}

func newResendFixture(t *testing.T, tag string) resendFixture {
	t.Helper()
	f := newFoundryFixture(t, tag)
	a, b := f.uploaded(t, tag+"-a"), f.uploaded(t, tag+"-b")
	f.draftVersion(t, tag, customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(context.Background(), loadJob(f.ontology, tag)))
	f.loaded(t, "w-"+tag, a, b)
	return resendFixture{foundryFixture: f, load: id26("FJ-" + tag), a: a, b: b, worker: "w-" + tag}
}

// held is a release of the claim n of doc, read as hash.
func (f resendFixture) held(doc string, n int, hash string) *model.FoundryReviewDecision {
	key := strings.Repeat("ab", 32)
	return &model.FoundryReviewDecision{
		ID: id26(strings.TrimRight(doc[3:], "0") + "-" + string(rune('0'+n)) + "-" + hash[:2]), OntologyID: f.ontology.ID, Kind: model.FoundryReviewRelease,
		DocID: &doc, ClaimKey: &key, ClaimSpan: &model.FoundryClaimSpan{Section: uint32(n), Start: 4, End: 20}, StatementHash: &hash,
		DecidedByUserID: id26("user"),
	}
}

// approve settles releases against the current load, as resend job RS-<tag>.
func (f resendFixture) approve(t *testing.T, tag string, ds ...*model.FoundryReviewDecision) bool {
	t.Helper()
	queued, err := f.db.SettleFoundryReleases(context.Background(), f.ontology.WorkspaceID, f.load, ds,
		&model.FoundryGraphJob{ID: id26("RS-" + tag), RequestedByUserID: ptr(id26("user"))})
	require.NoError(t, err)
	return queued
}

// due makes the ontology's queued job due now: a resend waits for the
// approvals that come with it.
func (f resendFixture) due(t *testing.T) {
	t.Helper()
	_, err := testDB.Exec(`UPDATE foundry_load_jobs SET next_attempt_at=NOW(6) - INTERVAL 1 SECOND WHERE ontology_id=? AND status='queued'`, f.ontology.ID)
	require.NoError(t, err)
}

// claim claims the due job as the fixture's worker.
func (f resendFixture) claim(t *testing.T) *model.FoundryGraphJob {
	t.Helper()
	f.due(t)
	job, err := f.db.ClaimFoundryGraphJob(context.Background(), f.worker, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, job, "a job is due")
	return job
}

// end ends job with status, naming what it may queue after by tag.
func (f resendFixture) end(t *testing.T, job *model.FoundryGraphJob, tag string, status model.FoundryGraphJobStatus) model.FoundryGraphJobFinish {
	t.Helper()
	finish, err := f.db.FinishFoundryGraphJob(context.Background(), job.ID, f.worker, model.FoundryGraphJobOutcome{
		Status: status, IngestDone: job.Kind == model.FoundryGraphJobLoad, Next: next(tag),
	})
	require.NoError(t, err)
	return finish
}

// sent records doc as a resend's Extract ended it: held already, with what
// the engine said of its releases, and its request ended.
func (f resendFixture) sent(t *testing.T, job *model.FoundryGraphJob, r *model.FoundryLoadResend, released int, outcomes ...model.FoundryReleaseOutcome) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, &model.FoundryDocumentExtraction{
		DocumentID: r.DocumentID, Status: model.FoundryExtractionExtracted, Counts: model.FoundryExtractionCounts{Facts: released},
		Releases: outcomes, Released: released,
	}, true))
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
		DocumentID: r.DocumentID, Reason: r.Reason, Requests: r.Requests,
	}))
}

// applied reads what became of the decisions in the current load, by id.
func (f resendFixture) applied(t *testing.T, ds ...*model.FoundryReviewDecision) map[string]string {
	t.Helper()
	ids := make([]string, 0, len(ds))
	for _, d := range ds {
		ids = append(ids, d.ID)
	}
	got, err := f.db.ListFoundryDecisionsApplied(context.Background(), f.ontology.ID, ids)
	require.NoError(t, err)
	out := map[string]string{}
	for id, a := range got {
		out[id] = string(a.Status)
		if a.Reason != "" {
			out[id] += "/" + a.Reason
		}
		assert.Equal(t, a.Status.Waits() || a.Status == "", a.At == nil, "%s at %v", a.Status, a.At)
	}
	return out
}

// owed lists the load's owed documents with how often each was requested.
func (f resendFixture) owed(t *testing.T) map[string]int {
	t.Helper()
	rows, err := f.db.ListFoundryLoadResends(context.Background(), f.load)
	require.NoError(t, err)
	out := map[string]int{}
	for _, r := range rows {
		assert.Equal(t, model.FoundryResendApproval, r.Reason)
		out[r.DocumentID] = r.Requests
	}
	return out
}

func written(d *model.FoundryReviewDecision) model.FoundryReleaseOutcome {
	return model.FoundryReleaseOutcome{DecisionID: d.ID, StatementHash: *d.StatementHash, Kind: model.FoundryReleaseOutcomeWritten}
}

func refused(d *model.FoundryReviewDecision, reason string) model.FoundryReleaseOutcome {
	return model.FoundryReleaseOutcome{DecisionID: d.ID, StatementHash: *d.StatementHash, Kind: model.FoundryReleaseOutcomeRefused, Reason: reason}
}

func notHeld(d *model.FoundryReviewDecision) model.FoundryReleaseOutcome {
	return model.FoundryReleaseOutcome{DecisionID: d.ID, StatementHash: *d.StatementHash, Kind: model.FoundryReleaseOutcomeNotHeld}
}

var (
	readA = strings.Repeat("0a", 32)
	readB = strings.Repeat("0b", 32)
)

// A person's approval joins the current load's decisions and queues one
// resend job for its document. Approvals that come before the job is claimed
// (two of one document, one of another: Approve all) go in that job. The
// job's Extract records what became of each, counts what it released on the
// document's row, and ends each request; with nothing left owed, no job
// follows.
func TestFoundryReleaseAppliesAtOnce(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RNOW")
	first, second, other := f.held(f.a, 0, readA), f.held(f.a, 1, readA), f.held(f.b, 0, readA)

	before := time.Now()
	require.True(t, f.approve(t, "1", first), "the first approval queues the job")
	assert.Equal(t, model.FoundryReleasePending, first.Applied.Status, "the answer says it waits")
	job := f.liveJob(t)
	require.NotNil(t, job)
	assert.Equal(t, id26("RS-1"), job.ID)
	assert.Equal(t, model.FoundryGraphJobResend, job.Kind)
	assert.Equal(t, model.FoundryGraphJobQueued, job.Status)
	assert.Equal(t, model.FoundryGraphJobDocuments, job.Phase)
	assert.Equal(t, f.load, job.LoadID)
	assert.Equal(t, id26("user"), *job.RequestedByUserID)
	assert.WithinDuration(t, before.Add(model.FoundryResendSettle), job.NextAttemptAt, time.Second, "it waits for what comes with it")
	none, err := f.db.ClaimFoundryGraphJob(ctx, f.worker, time.Minute)
	require.NoError(t, err)
	assert.Nil(t, none, "not due yet")

	// The rest of Approve all: no second job, and the first is put off.
	_, err = testDB.Exec(`UPDATE foundry_load_jobs SET next_attempt_at=NOW(6) WHERE id=?`, job.ID)
	require.NoError(t, err)
	require.False(t, f.approve(t, "2", second, other), "they join the queued job")
	again := f.liveJob(t)
	assert.Equal(t, job.ID, again.ID)
	assert.True(t, again.NextAttemptAt.After(time.Now().Add(model.FoundryResendSettle/2)), "put off again: %s", again.NextAttemptAt)
	assert.Equal(t, map[string]int{f.a: 2, f.b: 1}, f.owed(t))
	assert.Equal(t, map[string]string{first.ID: "pending", second.ID: "pending", other.ID: "pending"}, f.applied(t, first, second, other))

	// The load's own list has them, with the document's Extract.
	list, err := f.db.ListFoundryLoadDecisions(ctx, f.load, "", []string{f.a}, 100)
	require.NoError(t, err)
	require.Len(t, list, 2)

	claimed := f.claim(t)
	require.Equal(t, job.ID, claimed.ID)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 2)
	// In the order a load sends its documents: uploads, oldest first.
	require.Equal(t, []string{f.a, f.b}, []string{owed[0].DocumentID, owed[1].DocumentID})
	require.NotNil(t, owed[0].Document)
	assert.Equal(t, generalSetID(f.ontology.ID), owed[0].Document.SetID)
	f.sent(t, claimed, owed[0], 1, written(first), refused(second, "unresolved"))
	f.sent(t, claimed, owed[1], 1, written(other))
	assert.Equal(t, map[string]string{first.ID: "written", second.ID: "refused/unresolved", other.ID: "written"}, f.applied(t, first, second, other))
	assert.Empty(t, f.owed(t))

	// The row its commit wrote stays, with what the release moved.
	row, err := f.db.GetFoundryDocumentExtraction(ctx, f.load, f.a)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryExtractionExtracted, row.Status)
	assert.Equal(t, id26("FJ-RNOW"), row.JobID, "the job that committed it")
	assert.Equal(t, model.FoundryExtractionCounts{Sections: 2, Entities: 3, Mentions: 4, Facts: 2}, row.Counts)

	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, claimed, "now", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t), "nothing owed, nothing queued")
	listed, err := f.db.ListFoundryReviewDecisions(ctx, f.ontology.ID, "", nil, 100)
	require.NoError(t, err)
	assert.Len(t, listed, 3, "later loads copy them as any decision")
}

// A retry after a stop. The engine wrote the release, and the worker died
// before it recorded so: the request stays owed, the next attempt sends the
// document again, and the engine finds no review. The decision was settled
// against a review of this load, so that review left it by a release: it is
// written. Counts are not added twice, and a second not_held changes nothing.
func TestFoundryResendRetryAfterAStop(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RRETRY")
	d := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", d))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	// The attempt stops here: its lease runs out, and it is claimed again.
	_, err = testDB.Exec(`UPDATE foundry_load_jobs SET lease_until=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, job.ID)
	require.NoError(t, err)
	retry, err := f.db.ClaimFoundryGraphJob(ctx, "w-other", time.Minute)
	require.NoError(t, err)
	require.Equal(t, job.ID, retry.ID)
	assert.Equal(t, 2, retry.Attempts)
	// The stopped attempt records nothing: it lost its lease.
	assert.ErrorIs(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{DocumentID: f.a, Reason: model.FoundryResendApproval, Requests: 1}),
		store.ErrNotFound)
	assert.Equal(t, map[string]int{f.a: 1}, f.owed(t), "still owed")

	f.worker = "w-other"
	f.sent(t, retry, owed[0], 0, notHeld(d))
	assert.Equal(t, map[string]string{d.ID: "written"}, f.applied(t, d))
	row, err := f.db.GetFoundryDocumentExtraction(ctx, f.load, f.a)
	require.NoError(t, err)
	assert.Equal(t, 1, row.Counts.Facts, "a release sent again counts nothing")
	// Sent a third time, as another approval of the document would: no change.
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, retry.ID, "w-other", &model.FoundryDocumentExtraction{
		DocumentID: f.a, Status: model.FoundryExtractionExtracted, Releases: []model.FoundryReleaseOutcome{notHeld(d)},
	}, true))
	assert.Equal(t, map[string]string{d.ID: "written"}, f.applied(t, d))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, retry, "retry", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))
}

// An approval the engine refuses, and what follows. A claim read differently
// since (changed) is approved again as it reads now: the same decision, with
// the new hash, waits again, and the old statement's outcome, if it comes
// late, is not of it. A second approval that lands while the job runs keeps
// the document owed past the job's end of the request it read, and the job's
// end queues the resend that sends it.
func TestFoundryReleaseRefusedAndApprovedAgain(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RAGAIN")
	d := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", d))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 0, refused(d, "changed"))
	assert.Equal(t, map[string]string{d.ID: "refused/changed"}, f.applied(t, d))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "refused", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t), "a refusal queues nothing")

	// Approved again, of what it says now: the first decision, bound anew.
	now := f.held(f.a, 0, readB)
	now.ID = id26("another-id")
	require.True(t, f.approve(t, "2", now))
	assert.Equal(t, d.ID, now.ID, "one decision a claim")
	assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))
	list, err := f.db.ListFoundryLoadDecisions(ctx, f.load, "", []string{f.a}, 100)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, readB, *list[0].StatementHash, "the load's list holds what was approved last")

	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	assert.Equal(t, 2, owed[0].Requests)
	// While the job runs, another claim of the document is approved.
	late := f.held(f.a, 1, readA)
	require.False(t, f.approve(t, "3", late), "a running job is not put off, and no second job is live")
	// The run's word on the first statement comes late: not of this approval.
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, &model.FoundryDocumentExtraction{
		DocumentID: f.a, Status: model.FoundryExtractionExtracted, Releases: []model.FoundryReleaseOutcome{refused(d, "changed")},
	}, true))
	assert.Equal(t, map[string]string{d.ID: "pending", late.ID: "pending"}, f.applied(t, now, late))
	f.sent(t, job, owed[0], 1, written(now))
	assert.Equal(t, map[string]string{d.ID: "written", late.ID: "pending"}, f.applied(t, now, late))
	assert.Equal(t, map[string]int{f.a: 3}, f.owed(t), "the request that came during the run is still owed")

	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "again", model.FoundryGraphJobSucceeded))
	next := f.liveJob(t)
	require.NotNil(t, next, "the job's end queues the resend still owed")
	assert.Equal(t, id26("FN-R-again"), next.ID)
	assert.Equal(t, model.FoundryGraphJobResend, next.Kind)
	assert.Nil(t, next.RequestedByUserID)
	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 1, notHeld(now), written(late))
	assert.Equal(t, map[string]string{d.ID: "written", late.ID: "written"}, f.applied(t, now, late))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "last", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))
}

// A load that is mid-extract or mid-apply has a live job, and one job at a
// time writes a database: the approval joins the load's list and stays owed,
// and that job's end queues the resend. A live delete of another load gives
// way, as it does to a new load, and resumes after.
func TestFoundryReleaseWhileAJobIsLive(t *testing.T) {
	ctx := context.Background()
	t.Run("an extract", func(t *testing.T) {
		f := newResendFixture(t, "RLIVE")
		// An upload queues an extract into the current load.
		c := f.uploaded(t, "RLIVE-c")
		extract := f.liveJob(t)
		require.NotNil(t, extract)
		require.Equal(t, model.FoundryGraphJobExtract, extract.Kind)
		d := f.held(f.a, 0, readA)
		require.False(t, f.approve(t, "1", d), "the extract goes on")
		assert.Equal(t, extract.ID, f.liveJob(t).ID)
		assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))
		assert.Equal(t, map[string]int{f.a: 1}, f.owed(t))

		job := f.claim(t)
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(c), false))
		assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "live", model.FoundryGraphJobSucceeded))
		resend := f.liveJob(t)
		require.NotNil(t, resend, "the extract's end queues the resend")
		assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
		assert.Equal(t, id26("FN-R-live"), resend.ID)
		assert.False(t, resend.NextAttemptAt.After(time.Now().Add(time.Second)), "due at once: nothing else is coming")
	})

	t.Run("an apply", func(t *testing.T) {
		f := newResendFixture(t, "RAPPLY")
		_, err := testDB.Exec(`UPDATE foundry_ontologies SET auto_refresh=TRUE, refresh_due_at=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, f.ontology.ID)
		require.NoError(t, err)
		queued, err := f.db.QueueFoundryRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID,
			&model.FoundryGraphJob{ID: id26("AP-1"), Kind: model.FoundryGraphJobApply})
		require.NoError(t, err)
		require.True(t, queued)
		d := f.held(f.a, 0, readA)
		require.False(t, f.approve(t, "1", d))
		job := f.claim(t)
		require.Equal(t, model.FoundryGraphJobApply, job.Kind)
		finish, err := f.db.FinishFoundryGraphJob(ctx, job.ID, f.worker, model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("apply"),
		})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobEnded, finish)
		resend := f.liveJob(t)
		require.NotNil(t, resend)
		assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
		assert.Equal(t, f.load, resend.LoadID)
	})

	t.Run("a delete gives way", func(t *testing.T) {
		f := newResendFixture(t, "RDEL")
		// A second load becomes current; the first is deleted.
		old := f.load
		f.draftVersion(t, "RDEL2", customerTOML)
		require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "RDEL2")))
		f.loaded(t, f.worker, f.a, f.b)
		f.load = id26("FJ-RDEL2")
		require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, old, id26("DL-1")))
		require.Equal(t, model.FoundryGraphJobDelete, f.liveJob(t).Kind)
		d := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", d), "the approval goes first")
		resend := f.liveJob(t)
		assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
		cancelled, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, id26("DL-1"))
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobCancelled, cancelled.Status)
		job := f.claim(t)
		owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
		require.NoError(t, err)
		f.sent(t, job, owed[0], 1, written(d))
		assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "del", model.FoundryGraphJobSucceeded))
		after := f.liveJob(t)
		require.NotNil(t, after, "the delete resumes")
		assert.Equal(t, model.FoundryGraphJobDelete, after.Kind)
		assert.Equal(t, old, after.LoadID)
	})
}

// Two approvers at once, of one claim: one decision, the last to land as who
// decided, one job, and the document owed for both requests.
func TestFoundryReleaseTwoApproversAtOnce(t *testing.T) {
	f := newResendFixture(t, "RTWO")
	var wg sync.WaitGroup
	queued := make([]bool, 2)
	ids := make([]string, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := f.held(f.a, 0, readA)
			d.ID, d.DecidedByUserID = id26("two-"+string(rune('a'+i))), id26("user-"+string(rune('a'+i)))
			q, err := f.db.SettleFoundryReleases(context.Background(), f.ontology.WorkspaceID, f.load, []*model.FoundryReviewDecision{d},
				&model.FoundryGraphJob{ID: id26("RS-two-" + string(rune('a'+i))), RequestedByUserID: &d.DecidedByUserID})
			assert.NoError(t, err)
			queued[i], ids[i] = q, d.ID
		}()
	}
	wg.Wait()
	assert.NotEqual(t, queued[0], queued[1], "one of them queued the job: %v", queued)
	assert.Equal(t, ids[0], ids[1], "one decision")
	assert.Equal(t, map[string]int{f.a: 2}, f.owed(t))
	list, err := f.db.ListFoundryLoadDecisions(context.Background(), f.load, "", []string{f.a}, 100)
	require.NoError(t, err)
	assert.Len(t, list, 1)
	var jobs int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_jobs WHERE ontology_id=? AND kind='resend'`, f.ontology.ID).Scan(&jobs))
	assert.Equal(t, 1, jobs)
}

// A document with no saved extraction to replay and one deleted since refuse
// the approvals that waited, saying why; a job that fails leaves them
// unconfirmed. None leaves a job behind or queues another.
func TestFoundryResendRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("not replayed, and gone", func(t *testing.T) {
		f := newResendFixture(t, "RGONE")
		da, db := f.held(f.a, 0, readA), f.held(f.b, 0, readA)
		require.True(t, f.approve(t, "1", da, db))
		// The second document is deleted before the job runs.
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, f.b))
		job := f.claim(t)
		owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
		require.NoError(t, err)
		require.Len(t, owed, 2)
		require.NotNil(t, owed[0].Document)
		require.Equal(t, f.b, owed[1].DocumentID)
		require.Nil(t, owed[1].Document, "a deleted document can't be sent")
		assert.Empty(t, owed[0].Unsendable)
		assert.Equal(t, model.FoundryReleaseDocumentGone, owed[1].Unsendable)
		for i, why := range []string{model.FoundryReleaseNotReplayed, model.FoundryReleaseDocumentGone} {
			require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
				DocumentID: owed[i].DocumentID, Reason: owed[i].Reason, Requests: owed[i].Requests, Refused: why,
			}))
		}
		assert.Equal(t, map[string]string{da.ID: "refused/not_replayed", db.ID: "refused/document_gone"}, f.applied(t, da, db))
		assert.Empty(t, f.owed(t))
		row, err := f.db.GetFoundryDocumentExtraction(ctx, f.load, f.a)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryExtractionExtracted, row.Status, "its row says what its commit wrote")
		assert.Equal(t, 1, row.Counts.Facts)
	})

	// A failure recorded over the row a document's commit wrote, as a lost
	// event's retry can leave it: the load still holds the document, so it
	// is not gone, but there is no extraction of it to replay.
	t.Run("held, with no extraction to replay", func(t *testing.T) {
		f := newResendFixture(t, "RUNREAD")
		d := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", d))
		job := f.claim(t)
		failed := extracted(f.a)
		failed.Status, failed.Error = model.FoundryExtractionFailed, &model.FoundryExtractionError{Code: model.FoundryExtractionModel, Retryable: true}
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, failed, false))
		owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
		require.NoError(t, err)
		require.Len(t, owed, 1)
		require.Nil(t, owed[0].Document)
		assert.Equal(t, model.FoundryReleaseDocumentUnread, owed[0].Unsendable)
		require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
			DocumentID: f.a, Reason: owed[0].Reason, Requests: owed[0].Requests, Refused: owed[0].Unsendable,
		}))
		assert.Equal(t, map[string]string{d.ID: "refused/document_unread"}, f.applied(t, d))
	})

	t.Run("the job fails", func(t *testing.T) {
		f := newResendFixture(t, "RFAIL")
		da, db := f.held(f.a, 0, readA), f.held(f.b, 0, readA)
		require.True(t, f.approve(t, "1", da, db))
		job := f.claim(t)
		owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
		require.NoError(t, err)
		f.sent(t, job, owed[0], 1, written(da))
		assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "fail", model.FoundryGraphJobFailed))
		assert.Equal(t, map[string]string{da.ID: "written", db.ID: "pending/job_failed"}, f.applied(t, da, db),
			"what it wrote stays written; what waited is unconfirmed, not refused: no one knows it stays held")
		assert.Empty(t, f.owed(t))
		assert.Nil(t, f.liveJob(t), "a failed resend queues no other")
		// Approving again owes the document anew.
		require.True(t, f.approve(t, "2", db))
		assert.Equal(t, map[string]string{db.ID: "pending"}, f.applied(t, db))
		assert.Equal(t, map[string]int{f.b: 2}, f.owed(t))
	})

	t.Run("a retried attempt keeps them waiting", func(t *testing.T) {
		f := newResendFixture(t, "RWAIT")
		d := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", d))
		job := f.claim(t)
		at := time.Now().Add(time.Minute)
		finish, err := f.db.FinishFoundryGraphJob(ctx, job.ID, f.worker, model.FoundryGraphJobOutcome{RetryAt: &at, Error: "the ontology agent is unavailable"})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobRetried, finish)
		assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))
		assert.Equal(t, map[string]int{f.a: 1}, f.owed(t))
		assert.Equal(t, job.ID, f.liveJob(t).ID)
	})
}

// A load other than the current one has other reviews: an approval read from
// one is refused, and saves nothing.
func TestFoundryReleaseOfAnotherLoad(t *testing.T) {
	f := newResendFixture(t, "ROTHER")
	d := f.held(f.a, 0, readA)
	_, err := f.db.SettleFoundryReleases(context.Background(), f.ontology.WorkspaceID, id26("another-load"), []*model.FoundryReviewDecision{d},
		&model.FoundryGraphJob{ID: id26("RS-1")})
	assert.ErrorIs(t, err, store.ErrConflict)
	assert.Empty(t, f.decisionIDs(t))
	assert.Nil(t, f.liveJob(t))
	assert.Empty(t, f.applied(t, d), "a decision the load's list doesn't hold says nothing of it")
	// Only releases are applied this way.
	resolve := &model.FoundryReviewDecision{ID: id26("res"), OntologyID: f.ontology.ID, Kind: model.FoundryReviewResolve, Type: "Customer",
		DocID: &f.a, NormName: ptr("acme"), DecidedByUserID: id26("user")}
	_, err = f.db.SettleFoundryReleases(context.Background(), f.ontology.WorkspaceID, f.load, []*model.FoundryReviewDecision{resolve},
		&model.FoundryGraphJob{ID: id26("RS-2")})
	assert.Error(t, err)
	assert.Empty(t, f.decisionIDs(t))
}

// An approval saved while a new load is being written is settled against the
// load that is current then. When the new load becomes current its list,
// copied at its creation, lacks it: the load's end carries it over, and a
// resend applies it there. The engine finding no review at its place in the
// new load means that load holds no such claim (missed), not that it was
// written: no review of this load was read for it.
func TestFoundryReleaseCarriedToTheNextLoad(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RCARRY")
	first := f.load
	early := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "0", early))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 1, written(early))
	f.end(t, job, "early", model.FoundryGraphJobSucceeded)

	// A new load starts: it copies the first approval, which it will read at
	// its documents' commits.
	f.draftVersion(t, "RCARRY2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "RCARRY2")))
	second := id26("FJ-RCARRY2")
	// While it is written, two more claims are approved on the first load.
	late, lost := f.held(f.a, 1, readA), f.held(f.b, 0, readA)
	require.False(t, f.approve(t, "1", late, lost), "the load job goes on")
	assert.Equal(t, map[string]int{f.a: 2, f.b: 1}, f.owed(t), "owed by the load they were read from")

	// The new load's documents commit, and it becomes current.
	loadJob := f.claim(t)
	require.Equal(t, second, loadJob.ID)
	for _, doc := range []string{f.a, f.b} {
		e := extracted(doc)
		if doc == f.a {
			e.Releases = []model.FoundryReleaseOutcome{written(early)}
		}
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, loadJob.ID, f.worker, e, false))
	}
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, loadJob, "carry", model.FoundryGraphJobSucceeded))
	require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
	f.load = second
	assert.Equal(t, map[string]string{early.ID: "written", late.ID: "carried", lost.ID: "carried"}, f.applied(t, early, late, lost))
	assert.Equal(t, map[string]int{f.a: 1, f.b: 1}, f.owed(t), "the new load owes their documents")
	resend := f.liveJob(t)
	require.NotNil(t, resend)
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, second, resend.LoadID)

	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, second)
	require.NoError(t, err)
	require.Len(t, owed, 2)
	f.sent(t, job, owed[0], 1, notHeld(early), written(late))
	f.sent(t, job, owed[1], 0, notHeld(lost))
	assert.Equal(t, map[string]string{early.ID: "written", late.ID: "written", lost.ID: "missed"}, f.applied(t, early, late, lost))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "carried", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t), "carried once: nothing more is owed")

	// What the first load owed went with nothing sent: it is no longer the
	// load reads go to, and its rows end with it when it is deleted.
	var left int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND status='owed'`, first).Scan(&left))
	assert.Equal(t, 2, left)
	require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, id26("DL-carry")))
	del := f.claim(t)
	require.Equal(t, model.FoundryGraphJobDelete, del.Kind)
	f.end(t, del, "deleted", model.FoundryGraphJobSucceeded)
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=?`, first).Scan(&left))
	assert.Zero(t, left)
}

// An approval saved while a new load is written is carried to that load only
// once it holds the claim's document as extracted. Where the document is a
// duplicate there, out of scope, failed or deleted, nothing would ever say
// what became of the decision: it stays out of the load's list, so no reader
// waits for it, and no resend is owed for it. A document read later (a
// failure retried) brings its approval in at that job's end.
func TestFoundryReleaseNotCarriedWithoutItsDocument(t *testing.T) {
	ctx := context.Background()
	base := newFoundryFixture(t, "RNODOC")
	var docs []string
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		docs = append(docs, base.uploaded(t, "RNODOC-"+n))
	}
	duplicate, outOfScope, failed, deleted, read := docs[0], docs[1], docs[2], docs[3], docs[4]
	base.draftVersion(t, "RNODOC", customerTOML)
	require.NoError(t, base.db.CreateFoundryLoad(ctx, loadJob(base.ontology, "RNODOC")))
	base.loaded(t, "w-RNODOC", docs...)
	f := resendFixture{foundryFixture: base, load: id26("FJ-RNODOC"), a: duplicate, b: outOfScope, worker: "w-RNODOC"}

	// A new load starts, and a claim of each document is approved on the
	// load that is still current.
	f.draftVersion(t, "RNODOC2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "RNODOC2")))
	var approvals []*model.FoundryReviewDecision
	for _, doc := range docs {
		approvals = append(approvals, f.held(doc, 0, readA))
	}
	require.False(t, f.approve(t, "1", approvals...), "the load job goes on")
	byDoc := map[string]*model.FoundryReviewDecision{}
	for i, doc := range docs {
		byDoc[doc] = approvals[i]
	}

	// The new load holds only one of the documents as extracted.
	require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, deleted))
	job := f.claim(t)
	require.Equal(t, id26("FJ-RNODOC2"), job.ID)
	dup := extracted(duplicate)
	dup.Status, dup.DuplicateOf = model.FoundryExtractionDuplicate, &read
	scope := extracted(outOfScope)
	scope.Status = model.FoundryExtractionOutOfScope
	fail := extracted(failed)
	fail.Status, fail.Error = model.FoundryExtractionFailed, &model.FoundryExtractionError{Code: model.FoundryExtractionModel, Retryable: true}
	for _, e := range []*model.FoundryDocumentExtraction{dup, scope, fail, extracted(read)} {
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, e, false))
	}
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "nodoc", model.FoundryGraphJobSucceeded))
	f.load = job.LoadID
	require.Equal(t, f.load, *f.ontologyNow(t).CurrentLoadID)

	// The deleted document is taken out of the load by an extract, which goes
	// first; its end carries what the load can apply.
	job = f.claim(t)
	require.Equal(t, model.FoundryGraphJobExtract, job.Kind)
	assert.Empty(t, f.applied(t, approvals...), "none is in the load's list yet, so none waits")
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "nodoc-removed", model.FoundryGraphJobSucceeded))

	assert.Equal(t, map[string]string{byDoc[read].ID: "carried"}, f.applied(t, approvals...),
		"only the approval whose document the load extracted joins its list")
	assert.Equal(t, map[string]int{read: 1}, f.owed(t))
	job = f.claim(t)
	require.Equal(t, model.FoundryGraphJobResend, job.Kind)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	f.sent(t, job, owed[0], 1, written(byDoc[read]))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "nodoc-sent", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t), "nothing waits, and nothing is owed")
	assert.Equal(t, map[string]string{byDoc[read].ID: "written"}, f.applied(t, approvals...))

	// The failed document is read after all, by the extract an upload queues:
	// its end finds the document extracted and carries the approval.
	late := f.uploaded(t, "RNODOC-f")
	job = f.claim(t)
	require.Equal(t, model.FoundryGraphJobExtract, job.Kind)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(failed), false))
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(late), false))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "nodoc-read", model.FoundryGraphJobSucceeded))
	assert.Equal(t, map[string]string{byDoc[read].ID: "written", byDoc[failed].ID: "carried"}, f.applied(t, approvals...))
	assert.Equal(t, map[string]int{failed: 1}, f.owed(t))
	resend := f.liveJob(t)
	require.NotNil(t, resend)
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
}

// Making another load current carries approvals as a job's end does. One
// saved since that load copied its decisions joins its list, and with no job
// live a resend is queued for its document at once, so its card never says
// the load hasn't read it until some other job happens to end.
func TestFoundryReleaseCarriedWhenALoadIsMadeCurrent(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RCUR")
	first := f.load
	f.draftVersion(t, "RCUR2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "RCUR2")))
	f.loaded(t, f.worker, f.a, f.b)
	f.load = id26("FJ-RCUR2")
	require.Equal(t, f.load, *f.ontologyNow(t).CurrentLoadID)

	// Approved on the second load, which writes it.
	d := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", d))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 1, written(d))
	f.end(t, job, "second", model.FoundryGraphJobSucceeded)
	require.Nil(t, f.liveJob(t))

	// The first load is made current again: it never read the approval.
	queued, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("RCUR"),
		&model.FoundryGraphJob{ID: id26("FR-RCUR"), RequestedByUserID: ptr(id26("user"))}, noBudget)
	require.NoError(t, err)
	assert.False(t, queued, "no document waits for an extract")
	f.load = first
	assert.Equal(t, map[string]string{d.ID: "carried"}, f.applied(t, d))
	assert.Equal(t, map[string]int{f.a: 1}, f.owed(t))
	resend := f.liveJob(t)
	require.NotNil(t, resend, "the change of current load queues the resend")
	assert.Equal(t, id26("FR-RCUR"), resend.ID)
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, first, resend.LoadID)
	assert.Equal(t, id26("user"), *resend.RequestedByUserID)

	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, first)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	f.sent(t, job, owed[0], 1, written(d))
	assert.Equal(t, map[string]string{d.ID: "written"}, f.applied(t, d))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "first", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))

	// A load with nothing to carry queues nothing.
	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, id26("FJ-RCUR2"), extractJob("RCURb"), resendJob("RCURb"), noBudget)
	require.NoError(t, err)
	assert.Nil(t, f.liveJob(t))
}

// A purge of enriched profiles makes its clean load current, the third way
// a load becomes current. That load copied its decisions when it was
// created: an approval saved since is carried to it there and then, and its
// resend goes before the delete of the load that applied the profiles, which
// the resend's end queues.
func TestFoundryReleaseCarriedWhenAPurgeMakesItsLoadCurrent(t *testing.T) {
	ctx := context.Background()
	base := newFoundryFixture(t, "RPURGE")
	a, b := base.uploaded(t, "RPURGE-a"), base.uploaded(t, "RPURGE-b")
	base.draftVersion(t, "RPURGE", customerTOML)
	require.NoError(t, base.db.CreateFoundryLoad(ctx, loadJob(base.ontology, "RPURGE")))
	f := resendFixture{foundryFixture: base, load: id26("FJ-RPURGE"), a: a, b: b, worker: "w-RPURGE"}
	// loads claims the queued load job, has it apply profiles (or none),
	// records both documents and ends it with status.
	loads := func(prepared model.FoundryEnrichmentPrepared, status model.FoundryGraphJobStatus) *model.FoundryGraphJob {
		job := f.claim(t)
		require.Equal(t, model.FoundryGraphJobLoad, job.Kind)
		_, err := f.db.StartFoundryLoadEnrichment(ctx, f.worker, job)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, job.LoadID, f.worker, prepared))
		for _, doc := range []string{a, b} {
			require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(doc), false))
		}
		outcome := model.FoundryGraphJobOutcome{Status: status, IngestDone: true, Next: next("RPURGE-" + string(status))}
		if status == model.FoundryGraphJobBudgetExhausted {
			outcome.Error = "budget"
		}
		_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, f.worker, outcome)
		require.NoError(t, err)
		return job
	}
	dirty := loads(model.FoundryEnrichmentPrepared{
		End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "c", ResultsKey: "r",
		Applied: model.FoundryEnrichmentApplied{Key: "c", SHA256: "cc"},
	}, model.FoundryGraphJobSucceeded)
	require.Equal(t, f.load, dirty.LoadID)
	require.Equal(t, f.load, *f.ontologyNow(t).CurrentLoadID)

	// Consent is withdrawn, and the purge queues its clean load.
	require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(f.ontology.WorkspaceID, "off", false, 0)))
	step, err := f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, &model.FoundryGraphJob{ID: id26("FJ-RPURGE-clean")}, id26("FN-RPURGE-1"),
		24*time.Hour, 3)
	require.NoError(t, err)
	require.Equal(t, model.FoundryEnrichmentPurgeQueued, step)

	// While it is written a claim is approved on the load still current. The
	// clean load runs out of budget, so it ends partial and is not made
	// current by its own end; the approval is written to the first load.
	d := f.held(a, 0, readA)
	require.False(t, f.approve(t, "1", d), "the load job goes on")
	clean := loads(model.FoundryEnrichmentPrepared{End: model.EndFoundryEnrichment(model.FoundryEnrichmentOff), Applied: model.FoundryEnrichmentNone},
		model.FoundryGraphJobBudgetExhausted)
	require.Equal(t, model.FoundryLoadPartial, f.loadStatus(t, clean.LoadID))
	require.Equal(t, dirty.LoadID, *f.ontologyNow(t).CurrentLoadID)
	job := f.claim(t)
	require.Equal(t, model.FoundryGraphJobResend, job.Kind)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	f.sent(t, job, owed[0], 1, written(d))
	f.end(t, job, "dirty", model.FoundryGraphJobSucceeded)
	require.Nil(t, f.liveJob(t))

	// The purge makes the clean load current. It never read the approval.
	step, err = f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, &model.FoundryGraphJob{ID: id26("FJ-RPURGE-again")}, id26("FN-RPURGE-2"),
		24*time.Hour, 3)
	require.NoError(t, err)
	require.Equal(t, model.FoundryEnrichmentPurgeDone, step)
	require.Equal(t, clean.LoadID, *f.ontologyNow(t).CurrentLoadID)
	f.load = clean.LoadID
	assert.Equal(t, map[string]string{d.ID: "carried"}, f.applied(t, d), "carried as the load is made current")
	assert.Equal(t, map[string]int{a: 1}, f.owed(t))
	resend := f.liveJob(t)
	require.NotNil(t, resend)
	assert.Equal(t, id26("FN-RPURGE-2"), resend.ID)
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, clean.LoadID, resend.LoadID)
	assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, dirty.LoadID), "the load that applied purged profiles goes")

	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	f.sent(t, job, owed[0], 1, written(d))
	assert.Equal(t, map[string]string{d.ID: "written"}, f.applied(t, d))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "clean", model.FoundryGraphJobSucceeded))
	deleting := f.liveJob(t)
	require.NotNil(t, deleting, "the resend's end queues the delete")
	assert.Equal(t, model.FoundryGraphJobDelete, deleting.Kind)
	assert.Equal(t, dirty.LoadID, deleting.LoadID)
}

// An approval taken back before it is applied leaves the current load's list
// with it, so the resend that was queued writes nothing of it; one already
// written stays in the load that wrote it. A claim approved again after that
// is one decision in the load's list, under its new id.
func TestFoundryReleaseTakenBack(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RBACK")
	waits, done := f.held(f.a, 0, readA), f.held(f.b, 0, readA)
	require.True(t, f.approve(t, "1", done))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 1, written(done))
	f.end(t, job, "done", model.FoundryGraphJobSucceeded)
	require.True(t, f.approve(t, "2", waits))

	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.ID, waits.ID))
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.ID, done.ID))
	list, err := f.db.ListFoundryLoadDecisions(ctx, f.load, "", nil, 100)
	require.NoError(t, err)
	require.Len(t, list, 1, "the one it wrote stays")
	assert.Equal(t, done.ID, list[0].ID)
	assert.ErrorIs(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.ID, waits.ID), store.ErrNotFound)

	// The queued resend still sends the document: its Extract carries no
	// decision of it, and the engine says nothing of one.
	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	f.sent(t, job, owed[0], 0)
	f.end(t, job, "back", model.FoundryGraphJobSucceeded)
	assert.Nil(t, f.liveJob(t))

	// Approved again: a new decision, the only one of its claim in the list.
	again := f.held(f.b, 0, readA)
	again.ID = id26("again")
	require.True(t, f.approve(t, "3", again))
	assert.Equal(t, id26("again"), again.ID)
	list, err = f.db.ListFoundryLoadDecisions(ctx, f.load, "", []string{f.b}, 100)
	require.NoError(t, err)
	require.Len(t, list, 1, "the engine refuses a release listed twice")
	assert.Equal(t, again.ID, list[0].ID)
}

// An approval taken back after its load said something of it, or before any
// job did, leaves the current load's list too, unless the load wrote it: one
// the engine refused, one whose job failed, one the load found no claim for,
// and one the load copied at its creation that no job reported on. A resend
// sends every release its load's list holds of a document, so a copy left
// behind would be written by the next approval of that document: here the
// resends that follow send only the approvals that still stand.
func TestFoundryReleaseTakenBackAfterTheLoadsWord(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RWORD")
	missed, unreported := f.held(f.a, 0, readA), f.held(f.a, 1, readA)
	require.True(t, f.approve(t, "1", missed, unreported))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 2, written(missed), written(unreported))
	f.end(t, job, "first", model.FoundryGraphJobSucceeded)

	// A second load copies both. Its commit of the document finds no claim at
	// the first's place, and says nothing of the second.
	f.draftVersion(t, "RWORD2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "RWORD2")))
	load := f.claim(t)
	committed := extracted(f.a)
	committed.Releases = []model.FoundryReleaseOutcome{{DecisionID: missed.ID, StatementHash: readA, Kind: model.FoundryReleaseOutcomeMissed}}
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, load.ID, f.worker, committed, false))
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, load.ID, f.worker, extracted(f.b), false))
	f.end(t, load, "second", model.FoundryGraphJobSucceeded)
	f.load = load.ID

	// On the second load one approval is refused by the engine, one waits for
	// a job that fails, and one is written.
	refusedOne, failed, kept := f.held(f.a, 2, readA), f.held(f.b, 0, readA), f.held(f.b, 1, readA)
	require.True(t, f.approve(t, "2", refusedOne, kept))
	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 2)
	f.sent(t, job, owed[0], 0, refused(refusedOne, "conflict"))
	f.sent(t, job, owed[1], 1, written(kept))
	f.end(t, job, "third", model.FoundryGraphJobSucceeded)
	require.True(t, f.approve(t, "3", failed))
	f.end(t, f.claim(t), "failed", model.FoundryGraphJobFailed)
	got := f.applied(t, missed, unreported, refusedOne, failed, kept)
	assert.Equal(t, "missed", got[missed.ID])
	assert.Equal(t, "", got[unreported.ID])
	assert.Equal(t, "refused/conflict", got[refusedOne.ID])
	assert.NotEqual(t, "written", got[failed.ID])
	assert.Equal(t, "written", got[kept.ID])
	require.Len(t, got, 5, "the load's list holds each")

	for _, d := range []*model.FoundryReviewDecision{missed, unreported, refusedOne, failed, kept} {
		require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.ID, d.ID))
	}
	list, err := f.db.ListFoundryLoadDecisions(ctx, f.load, "", nil, 100)
	require.NoError(t, err)
	require.Len(t, list, 1, "only what the load wrote stays in its list")
	assert.Equal(t, kept.ID, list[0].ID)

	// Another claim of each document is approved: its resend sends that
	// approval, the one the load wrote (which finds no review), and none of
	// those taken back.
	laterA, laterB := f.held(f.a, 3, readA), f.held(f.b, 2, readA)
	require.True(t, f.approve(t, "4", laterA, laterB))
	sentA, err := f.db.ListFoundryLoadDecisions(ctx, f.load, "", []string{f.a}, 100)
	require.NoError(t, err)
	require.Len(t, sentA, 1)
	assert.Equal(t, laterA.ID, sentA[0].ID)
	sentB, err := f.db.ListFoundryLoadDecisions(ctx, f.load, "", []string{f.b}, 100)
	require.NoError(t, err)
	ids := []string{sentB[0].ID}
	if len(sentB) > 1 {
		ids = append(ids, sentB[1].ID)
	}
	assert.ElementsMatch(t, []string{kept.ID, laterB.ID}, ids)
	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 2)
	f.sent(t, job, owed[0], 1, written(laterA))
	f.sent(t, job, owed[1], 1, notHeld(kept), written(laterB))
	f.end(t, job, "later", model.FoundryGraphJobSucceeded)
	assert.Equal(t, map[string]string{laterA.ID: "written", laterB.ID: "written"}, f.applied(t, missed, unreported, refusedOne, failed, laterA, laterB))
	assert.Nil(t, f.liveJob(t))
}

// An approval taken back while another load is current leaves a copy in the
// load it was settled against: removal reaches only the load that is current
// then. When that load is made current again the copy goes before anything
// is sent, whether it still waited there (approved while the other load was
// written, so its document is still owed) or that load had refused it. The
// resend the owed document gets sends nothing of it, and neither does a
// later approval of another claim of the refused one's document.
func TestFoundryReleaseTakenBackWhileAnotherLoadIsCurrent(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RSWAP")
	first := f.load
	// The first load refuses one approval.
	turnedDown := f.held(f.b, 0, readA)
	require.True(t, f.approve(t, "0", turnedDown))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, first)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 0, refused(turnedDown, "conflict"))
	f.end(t, job, "refused", model.FoundryGraphJobSucceeded)

	// While a second load is written another claim is approved on the first:
	// it waits there, and the first load owes its document.
	f.draftVersion(t, "RSWAP2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "RSWAP2")))
	waits := f.held(f.a, 0, readA)
	require.False(t, f.approve(t, "1", waits), "the load job goes on")

	// The second load becomes current, is carried that approval, and refuses it.
	f.loaded(t, f.worker, f.a, f.b)
	f.load = id26("FJ-RSWAP2")
	require.Equal(t, f.load, *f.ontologyNow(t).CurrentLoadID)
	job = f.claim(t)
	require.Equal(t, model.FoundryGraphJobResend, job.Kind)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	f.sent(t, job, owed[0], 0, refused(waits, "conflict"))
	f.end(t, job, "second", model.FoundryGraphJobSucceeded)
	require.Equal(t, map[string]string{turnedDown.ID: "", waits.ID: "refused/conflict"}, f.applied(t, turnedDown, waits))

	// The person takes both back. They leave the current load's list; the
	// first load keeps its copies, and still owes the document.
	for _, d := range []*model.FoundryReviewDecision{turnedDown, waits} {
		require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.ID, d.ID))
	}
	assert.Empty(t, f.applied(t, turnedDown, waits))
	var left int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_decisions WHERE load_id=?`, first).Scan(&left))
	require.Equal(t, 2, left, "removal reaches only the load that is current")
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND status='owed'`, first).Scan(&left))
	require.Equal(t, 1, left)

	// The first load is made current again: its copies go before the resend
	// its owed document queues.
	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("RSWAP"), resendJob("RSWAP"), noBudget)
	require.NoError(t, err)
	f.load = first
	assert.Empty(t, f.applied(t, turnedDown, waits), "neither is in the list any more")
	list, err := f.db.ListFoundryLoadDecisions(ctx, first, "", nil, 100)
	require.NoError(t, err)
	assert.Empty(t, list, "a resend sends what the list holds: nothing a person took back")
	job = f.claim(t)
	require.Equal(t, id26("FR-RSWAP"), job.ID)
	require.Equal(t, model.FoundryGraphJobResend, job.Kind)
	owed, err = f.db.ListFoundryLoadResends(ctx, first)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	assert.Equal(t, f.a, owed[0].DocumentID)
	f.sent(t, job, owed[0], 0)
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "back", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))

	// Another claim of the refused one's document is approved: it goes alone.
	later := f.held(f.b, 1, readA)
	require.True(t, f.approve(t, "2", later))
	list, err = f.db.ListFoundryLoadDecisions(ctx, first, "", []string{f.b}, 100)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, later.ID, list[0].ID)
}

// A job that fails for good after the engine wrote a release, and before the
// event that said so was recorded. The decision is not refused: it stays
// unconfirmed, and no job is queued for it. The next resend of its document,
// which another approval of that document queues, sends it again; the engine
// holds no review for it any more, so it was written, and the load says so.
// A refusal of that document's waiting approvals, by a job that couldn't
// send it, says nothing of the unconfirmed one.
func TestFoundryResendFailsAfterTheEngineWrote(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RLOST")
	d, other := f.held(f.a, 0, readA), f.held(f.b, 0, readA)
	require.True(t, f.approve(t, "1", d, other))
	job := f.claim(t)
	// The engine writes both, and the agent goes away before either event is
	// recorded, for every attempt the job has.
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "lost", model.FoundryGraphJobFailed))
	assert.Equal(t, map[string]string{d.ID: "pending/job_failed", other.ID: "pending/job_failed"}, f.applied(t, d, other))
	got, err := f.db.ListFoundryDecisionsApplied(ctx, f.ontology.ID, []string{d.ID})
	require.NoError(t, err)
	assert.True(t, got[d.ID].Unconfirmed())
	assert.Empty(t, f.owed(t))
	assert.Nil(t, f.liveJob(t), "nothing is queued for a decision whose job failed")

	// Another claim of each document is approved. The first document's
	// resend finds no review for d: it was written.
	laterA, laterB := f.held(f.a, 1, readA), f.held(f.b, 1, readA)
	require.True(t, f.approve(t, "2", laterA, laterB))
	job = f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 2)
	f.sent(t, job, owed[0], 1, notHeld(d), written(laterA))
	// The second document can't be sent: what waited for this request is
	// refused, and the unconfirmed one stays unconfirmed.
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
		DocumentID: owed[1].DocumentID, Reason: owed[1].Reason, Requests: owed[1].Requests, Refused: model.FoundryReleaseNotReplayed,
	}))
	assert.Equal(t, map[string]string{
		d.ID: "written", laterA.ID: "written", other.ID: "pending/job_failed", laterB.ID: "refused/not_replayed",
	}, f.applied(t, d, other, laterA, laterB))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "found", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))

	// Approved again, the unconfirmed one waits for a job as any approval.
	require.True(t, f.approve(t, "3", other))
	assert.Equal(t, map[string]string{other.ID: "pending"}, f.applied(t, other))
}

// A carried approval whose resend job failed is unconfirmed too, and is not
// owed again at each job's end: a job that can't succeed would follow itself.
func TestFoundryCarriedReleaseWhoseJobFailed(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RCFAIL")
	f.draftVersion(t, "RCFAIL2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "RCFAIL2")))
	d := f.held(f.a, 0, readA)
	require.False(t, f.approve(t, "1", d), "the load job goes on")
	f.loaded(t, f.worker, f.a, f.b)
	f.load = id26("FJ-RCFAIL2")
	require.Equal(t, map[string]string{d.ID: "carried"}, f.applied(t, d))

	job := f.claim(t)
	require.Equal(t, model.FoundryGraphJobResend, job.Kind)
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "cfail", model.FoundryGraphJobFailed))
	assert.Equal(t, map[string]string{d.ID: "carried/job_failed"}, f.applied(t, d))
	assert.Empty(t, f.owed(t))
	assert.Nil(t, f.liveJob(t), "the failed job's end owes it no other")

	// Nor does a later job's end.
	c := f.uploaded(t, "RCFAIL-c")
	job = f.claim(t)
	require.Equal(t, model.FoundryGraphJobExtract, job.Kind)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(c), false))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "cfail-up", model.FoundryGraphJobSucceeded))
	assert.Empty(t, f.owed(t))
	assert.Nil(t, f.liveJob(t))
	assert.Equal(t, map[string]string{d.ID: "carried/job_failed"}, f.applied(t, d))
}

// A claim two reviews held is one fact once it is released: the document's
// row gains the fact the decision wrote and loses both reviews.
func TestFoundryResendCountsFactsByDecision(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RFACT")
	_, err := testDB.Exec(`UPDATE foundry_load_extractions SET reviews=3 WHERE load_id=? AND document_id=?`, f.load, f.a)
	require.NoError(t, err)
	d := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", d))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, &model.FoundryDocumentExtraction{
		DocumentID: f.a, Status: model.FoundryExtractionExtracted, Counts: model.FoundryExtractionCounts{Facts: 1},
		Releases: []model.FoundryReleaseOutcome{written(d)}, Released: 2,
	}, true))
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
		DocumentID: f.a, Reason: owed[0].Reason, Requests: owed[0].Requests,
	}))
	row, err := f.db.GetFoundryDocumentExtraction(ctx, f.load, f.a)
	require.NoError(t, err)
	assert.Equal(t, 2, row.Counts.Facts, "the commit's fact, and the one the decision wrote")
	assert.Equal(t, 1, row.Counts.Reviews, "both reviews of the claim left")
}

// An approval that lands while a resend runs. The job read its documents and
// their decisions before, so it speaks only for the request it read: the
// document owed again stays owed, with every approval of it still waiting,
// even where the job refuses what it was sent for; a document first owed
// meanwhile waits too. The job's end queues the resend that sends them all.
func TestFoundryReleaseLandsWhileAResendRuns(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RRUN")
	first := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", first))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)

	same, other := f.held(f.a, 1, readA), f.held(f.b, 0, readA)
	require.False(t, f.approve(t, "2", same, other), "the running job is not put off, and no second job is live")
	assert.Equal(t, job.ID, f.liveJob(t).ID)
	assert.Equal(t, map[string]int{f.a: 2, f.b: 1}, f.owed(t))

	// The run couldn't replay the first document. It refuses the request it
	// read, which the document's row no longer is.
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
		DocumentID: f.a, Reason: owed[0].Reason, Requests: owed[0].Requests, Refused: model.FoundryReleaseNotReplayed,
	}))
	assert.Equal(t, map[string]string{first.ID: "pending", same.ID: "pending", other.ID: "pending"}, f.applied(t, first, same, other),
		"the request that came since is the next job's to answer, for every approval of the document")
	assert.Equal(t, map[string]int{f.a: 2, f.b: 1}, f.owed(t))

	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "run", model.FoundryGraphJobSucceeded))
	next := f.liveJob(t)
	require.NotNil(t, next, "the job's end queues the resend of what came while it ran")
	assert.Equal(t, id26("FN-R-run"), next.ID)
	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 2)
	f.sent(t, job, owed[0], 2, written(first), written(same))
	f.sent(t, job, owed[1], 1, written(other))
	assert.Equal(t, map[string]string{first.ID: "written", same.ID: "written", other.ID: "written"}, f.applied(t, first, same, other))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "ran", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))
}

// A resend job an older binary ran, as after a rollback: it claims the job
// as an extract of its load, sends the documents the load waits for (none),
// and ends it, knowing nothing of what the load owes. The approvals go on
// waiting and their documents stay owed. Back on this binary, the next
// approval, or the next job's end, queues the resend that applies them, less
// any that binary removed meanwhile.
func TestFoundryResendEndedByAnOlderBinary(t *testing.T) {
	ctx := context.Background()
	// older ends the ontology's live job as that binary's finish does.
	older := func(t *testing.T, f resendFixture) {
		t.Helper()
		job := f.liveJob(t)
		require.NotNil(t, job)
		require.Equal(t, model.FoundryGraphJobResend, job.Kind)
		_, err := testDB.Exec(`UPDATE foundry_load_jobs SET status='succeeded', active_key=NULL, lease_owner=NULL, lease_until=NULL,
  finished_at=NOW(6) WHERE id=?`, job.ID)
		require.NoError(t, err)
		require.Nil(t, f.liveJob(t))
	}

	t.Run("the next approval sends them", func(t *testing.T) {
		f := newResendFixture(t, "ROLD")
		d := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", d))
		older(t, f)
		assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))
		assert.Equal(t, map[string]int{f.a: 1}, f.owed(t))

		e := f.held(f.b, 0, readA)
		require.True(t, f.approve(t, "2", e), "a job is queued again")
		job := f.claim(t)
		owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
		require.NoError(t, err)
		require.Len(t, owed, 2, "with the document the older binary left owed")
		f.sent(t, job, owed[0], 1, written(d))
		f.sent(t, job, owed[1], 1, written(e))
		assert.Equal(t, map[string]string{d.ID: "written", e.ID: "written"}, f.applied(t, d, e))
		assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "old", model.FoundryGraphJobSucceeded))
		assert.Nil(t, f.liveJob(t))
	})

	t.Run("the next job's end sends them", func(t *testing.T) {
		f := newResendFixture(t, "ROLDJ")
		d := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", d))
		older(t, f)
		c := f.uploaded(t, "ROLDJ-c")
		job := f.claim(t)
		require.Equal(t, model.FoundryGraphJobExtract, job.Kind)
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(c), false))
		assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "oldj", model.FoundryGraphJobSucceeded))
		resend := f.liveJob(t)
		require.NotNil(t, resend)
		assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
		job = f.claim(t)
		owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
		require.NoError(t, err)
		require.Len(t, owed, 1)
		f.sent(t, job, owed[0], 1, written(d))
		assert.Equal(t, map[string]string{d.ID: "written"}, f.applied(t, d))
	})

	// That binary's removal of a decision takes it from the saved decisions
	// only: the current load's copy stays, waiting, with its document owed.
	// removed takes decisions back as it does.
	removed := func(t *testing.T, f resendFixture, ds ...*model.FoundryReviewDecision) {
		t.Helper()
		for _, d := range ds {
			_, err := testDB.Exec(`DELETE FROM foundry_review_decisions WHERE ontology_id=? AND id=?`, f.ontology.ID, d.ID)
			require.NoError(t, err)
		}
	}

	// Back on this binary the copy goes before a resend is queued: the next
	// approval of that document's claims is sent without it.
	t.Run("the next approval sends nothing that binary removed", func(t *testing.T) {
		f := newResendFixture(t, "ROLDR")
		gone, alsoGone := f.held(f.a, 0, readA), f.held(f.b, 0, readA)
		require.True(t, f.approve(t, "1", gone, alsoGone))
		older(t, f)
		removed(t, f, gone, alsoGone)
		require.Equal(t, map[string]string{gone.ID: "pending", alsoGone.ID: "pending"}, f.applied(t, gone, alsoGone))
		require.Equal(t, map[string]int{f.a: 1, f.b: 1}, f.owed(t))

		kept := f.held(f.a, 1, readA)
		require.True(t, f.approve(t, "2", kept))
		assert.Equal(t, map[string]string{kept.ID: "pending"}, f.applied(t, gone, alsoGone, kept))
		list, err := f.db.ListFoundryLoadDecisions(ctx, f.load, "", nil, 100)
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Equal(t, kept.ID, list[0].ID)
		job := f.claim(t)
		owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
		require.NoError(t, err)
		require.Len(t, owed, 2, "the documents the removed ones owed are sent, with what is left")
		f.sent(t, job, owed[0], 1, written(kept))
		f.sent(t, job, owed[1], 0)
		assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "oldr", model.FoundryGraphJobSucceeded))
		assert.Nil(t, f.liveJob(t))
	})

	t.Run("the next job's end sends nothing that binary removed", func(t *testing.T) {
		f := newResendFixture(t, "ROLDS")
		gone := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", gone))
		older(t, f)
		removed(t, f, gone)
		c := f.uploaded(t, "ROLDS-c")
		job := f.claim(t)
		require.Equal(t, model.FoundryGraphJobExtract, job.Kind)
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(c), false))
		assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "olds", model.FoundryGraphJobSucceeded))
		assert.Empty(t, f.applied(t, gone))
		list, err := f.db.ListFoundryLoadDecisions(ctx, f.load, "", nil, 100)
		require.NoError(t, err)
		assert.Empty(t, list)
		job = f.claim(t)
		require.Equal(t, model.FoundryGraphJobResend, job.Kind, "the document it owed is still sent")
		owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
		require.NoError(t, err)
		require.Len(t, owed, 1)
		f.sent(t, job, owed[0], 0)
		assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "olds-sent", model.FoundryGraphJobSucceeded))
		assert.Nil(t, f.liveJob(t))
	})
}

// A resend sends only what its load holds. A document uploaded while it runs
// is the owed extract's, which the resend's end queues: the resend is not
// queued again for it, as a load or an extract would be.
func TestFoundryResendLeavesUploadsToAnExtract(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RUP")
	d := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", d))
	job := f.claim(t)
	c := f.uploaded(t, "RUP-c")
	assert.Equal(t, job.ID, f.liveJob(t).ID, "the upload waits for the job")
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1, "a document the load hasn't extracted is not one to send again")
	f.sent(t, job, owed[0], 1, written(d))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "up", model.FoundryGraphJobSucceeded))
	extract := f.liveJob(t)
	require.NotNil(t, extract)
	assert.Equal(t, model.FoundryGraphJobExtract, extract.Kind)
	pending, err := f.db.ListPendingFoundryDocuments(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, c, pending[0].ID)
}

// What the engine says of a decision a load copied at its creation is
// recorded by the commit that reads it, and a later not_held, which a retry
// of that commit's job reads, says nothing new of one no job reported on.
func TestFoundryReleaseOutcomesOfACopiedDecision(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RCOPY")
	d, e := f.held(f.a, 0, readA), f.held(f.a, 1, readA)
	require.True(t, f.approve(t, "1", d, e))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 2, written(d), written(e))
	f.end(t, job, "copy", model.FoundryGraphJobSucceeded)

	f.draftVersion(t, "RCOPY2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "RCOPY2")))
	load := f.claim(t)
	missed := model.FoundryReleaseOutcome{DecisionID: d.ID, StatementHash: readA, Kind: model.FoundryReleaseOutcomeMissed}
	committed := extracted(f.a)
	committed.Releases = []model.FoundryReleaseOutcome{missed, notHeld(e)}
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, load.ID, f.worker, committed, false))
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, load.ID, f.worker, extracted(f.b), false))
	f.end(t, load, "copied", model.FoundryGraphJobSucceeded)
	f.load = load.ID
	assert.Equal(t, map[string]string{d.ID: "missed", e.ID: ""}, f.applied(t, d, e))
	assert.Nil(t, f.liveJob(t), "a copied decision is no reason to send its document again")
}
