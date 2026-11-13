package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withdrawn is the engine's word on a withdraw of d (contract 4.16.0).
func withdrawn(d *model.FoundryReviewDecision, kind model.FoundryReleaseOutcomeKind, reason string) model.FoundryReleaseOutcome {
	return model.FoundryReleaseOutcome{DecisionID: d.ID, StatementHash: *d.StatementHash, Withdraw: true, Kind: kind, Reason: reason}
}

// kinds lists a load's decisions by id with their kind, as its list holds them.
func kinds(t *testing.T, loadID string) map[string]string {
	t.Helper()
	rows, err := testDB.Query(`SELECT id, kind FROM foundry_load_decisions WHERE load_id=?`, loadID)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, kind string
		require.NoError(t, rows.Scan(&id, &kind))
		out[id] = kind
	}
	require.NoError(t, rows.Err())
	return out
}

// sentKinds is what the next Extract of doc sends of the load's list, by id.
func (f resendFixture) sentKinds(t *testing.T, doc string) map[string]model.FoundryReviewDecisionKind {
	t.Helper()
	list, err := f.db.ListFoundryLoadDecisions(context.Background(), f.load, "", []string{doc}, 100)
	require.NoError(t, err)
	out := map[string]model.FoundryReviewDecisionKind{}
	for _, d := range list {
		out[d.ID] = d.Kind
	}
	return out
}

// writeApproval approves d on the current load and has its resend write it.
func (f resendFixture) writeApproval(t *testing.T, tag string, d *model.FoundryReviewDecision) {
	t.Helper()
	require.True(t, f.approve(t, tag, d))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(context.Background(), f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	f.sent(t, job, owed[0], 1, written(d))
	f.end(t, job, tag+"-end", model.FoundryGraphJobSucceeded)
	require.Equal(t, map[string]string{d.ID: "written"}, f.applied(t, d))
}

// A person takes back an approval the current load wrote. The decision goes,
// as any removal, and the load's copy becomes a withdraw of it, the release
// as it was sent, which waits for a resend of its document, queued as an
// approval's is. The resend sends it, the engine takes back what it wrote,
// and the load says so; the withdraw is never sent again, and the document's
// counts move back. The reader is shown the withdraw beside the saved
// decisions while that load is current, and can't remove it. Approved
// again, the claim is a new decision under a new id: the list holds both,
// and sends the new release alone.
func TestFoundryWrittenApprovalTakenBack(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "WBACK")
	_, err := testDB.Exec(`UPDATE foundry_load_extractions SET reviews=3 WHERE load_id=? AND document_id=?`, f.load, f.a)
	require.NoError(t, err)
	d := f.held(f.a, 0, readA)
	f.writeApproval(t, "1", d)

	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, d.ID,
		&model.FoundryGraphJob{ID: id26("RS-WBACK-2")}))
	assert.Equal(t, map[string]string{d.ID: "withdraw"}, kinds(t, f.load))
	assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{d.ID: model.FoundryReviewWithdraw}, f.sentKinds(t, f.a))
	assert.Equal(t, map[string]int{f.a: 2}, f.owed(t))
	live := f.liveJob(t)
	require.NotNil(t, live, "the take-back is queued as an approval is")
	assert.Equal(t, model.FoundryGraphJobResend, live.Kind)
	assert.Equal(t, id26("RS-WBACK-2"), live.ID)
	// No saved decision: removing it again finds none.
	assert.ErrorIs(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, d.ID, nil), store.ErrNotFound)
	saved, err := f.db.ListFoundryReviewDecisions(ctx, f.ontology.ID, "", nil, 100)
	require.NoError(t, err)
	assert.Empty(t, saved)
	shown, err := f.db.ListFoundryShownDecisions(ctx, f.ontology.ID, "", 100)
	require.NoError(t, err)
	require.Len(t, shown, 1)
	assert.Equal(t, model.FoundryReviewWithdraw, shown[0].Kind)
	assert.Equal(t, d.ID, shown[0].ID)
	assert.Equal(t, *d.StatementHash, *shown[0].StatementHash)

	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, &model.FoundryDocumentExtraction{
		DocumentID: f.a, Status: model.FoundryExtractionExtracted,
		Releases: []model.FoundryReleaseOutcome{withdrawn(d, model.FoundryReleaseOutcomeWithdrawn, "")},
	}, true))
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
		DocumentID: f.a, Reason: owed[0].Reason, Requests: owed[0].Requests,
	}))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "back", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t), "nothing more is owed")
	assert.Equal(t, map[string]string{d.ID: "withdrawn"}, f.applied(t, d))
	assert.Empty(t, f.sentKinds(t, f.a), "an answered withdraw is never sent again")
	row, err := f.db.GetFoundryDocumentExtraction(ctx, f.load, f.a)
	require.NoError(t, err)
	assert.Equal(t, 1, row.Counts.Facts, "the commit's fact: the one the approval wrote is taken back")
	assert.Equal(t, 3, row.Counts.Reviews, "its review is held again")

	again := f.held(f.a, 0, readA)
	again.ID = id26("WBACK-again")
	require.True(t, f.approve(t, "3", again))
	assert.Equal(t, id26("WBACK-again"), again.ID, "a new decision")
	assert.Equal(t, map[string]string{d.ID: "withdraw", again.ID: "release"}, kinds(t, f.load))
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{again.ID: model.FoundryReviewRelease}, f.sentKinds(t, f.a))
	shown, err = f.db.ListFoundryShownDecisions(ctx, f.ontology.ID, "", 100)
	require.NoError(t, err)
	require.Len(t, shown, 2)
	// Paged by id across both.
	page, err := f.db.ListFoundryShownDecisions(ctx, f.ontology.ID, "", 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	rest, err := f.db.ListFoundryShownDecisions(ctx, f.ontology.ID, page[0].ID, 1)
	require.NoError(t, err)
	require.Len(t, rest, 1)
	assert.NotEqual(t, page[0].ID, rest[0].ID)
}

// Each word the engine may give a withdraw is kept as it came: nothing the
// load held (not_written), and a refusal, whose reason says why what it wrote
// stays. A document a resend can't send refuses the withdraws that wait for
// it, as it does approvals, so none waits for good.
func TestFoundryTakeBackOutcomes(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "WWORD")
	none, differs, gone := f.held(f.a, 0, readA), f.held(f.a, 1, readA), f.held(f.b, 0, readA)
	require.True(t, f.approve(t, "1", none, differs, gone))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 2)
	f.sent(t, job, owed[0], 2, written(none), written(differs))
	f.sent(t, job, owed[1], 1, written(gone))
	f.end(t, job, "written", model.FoundryGraphJobSucceeded)
	for _, d := range []*model.FoundryReviewDecision{none, differs, gone} {
		require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, d.ID,
			&model.FoundryGraphJob{ID: id26("RS-WWORD-" + d.ID[len(d.ID)-4:])}))
	}
	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 2)
	f.sent(t, job, owed[0], 0, withdrawn(none, model.FoundryReleaseOutcomeNotWritten, ""),
		withdrawn(differs, model.FoundryReleaseOutcomeRefused, "differs"))
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
		DocumentID: owed[1].DocumentID, Reason: owed[1].Reason, Requests: owed[1].Requests, Refused: model.FoundryReleaseDocumentGone,
	}))
	f.end(t, job, "words", model.FoundryGraphJobSucceeded)
	// not_written of an approval the load recorded written is the take-back
	// done by a run that said nothing of it: withdrawn. (One the load never
	// recorded written stays not_written: TestFoundryTakeBackWhateverBecameOfItsDocument.)
	assert.Equal(t, map[string]string{
		none.ID: "withdrawn", differs.ID: "refused/differs", gone.ID: "refused/document_gone",
	}, f.applied(t, none, differs, gone))
	assert.Empty(t, f.sentKinds(t, f.a))
	assert.Empty(t, f.sentKinds(t, f.b))
	assert.Nil(t, f.liveJob(t))
}

// Removing an approval the load never wrote stays as before: it leaves the
// list and nothing is sent. One whose job failed before the engine's word was
// recorded may be written, so it is taken back; so is one a running job of
// the load may be sending now.
func TestFoundryRemovedApprovalTakenBackOnlyWhereItMayBeWritten(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "WMAY")
	waits := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", waits))
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, waits.ID,
		&model.FoundryGraphJob{ID: id26("RS-WMAY-x")}))
	assert.Empty(t, kinds(t, f.load), "never sent: dropped")

	// The resend the first approval queued sends this one too, and fails.
	failed := f.held(f.a, 1, readA)
	require.False(t, f.approve(t, "2", failed), "the queued resend takes it")
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, f.claim(t), "failed", model.FoundryGraphJobFailed))
	assert.Equal(t, map[string]string{failed.ID: "pending/job_failed"}, f.applied(t, failed))
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, failed.ID,
		&model.FoundryGraphJob{ID: id26("RS-WMAY-3")}))
	assert.Equal(t, map[string]string{failed.ID: "withdraw"}, kinds(t, f.load), "unconfirmed: it may be written")
	assert.Equal(t, map[string]string{failed.ID: "pending"}, f.applied(t, failed))

	// While that resend runs, another claim is approved and taken back: the
	// running job may be writing it.
	job := f.claim(t)
	require.Equal(t, id26("RS-WMAY-3"), job.ID)
	running := f.held(f.b, 0, readA)
	require.False(t, f.approve(t, "4", running), "the running job's end queues it")
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, running.ID,
		&model.FoundryGraphJob{ID: id26("RS-WMAY-5")}))
	assert.Equal(t, map[string]string{failed.ID: "withdraw", running.ID: "withdraw"}, kinds(t, f.load))
	// The engine's word on what the running job sent as a release is not of
	// the withdraw it became.
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	for _, o := range owed {
		if o.DocumentID == f.a {
			f.sent(t, job, o, 0, withdrawn(failed, model.FoundryReleaseOutcomeWithdrawn, ""))
		}
	}
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, &model.FoundryDocumentExtraction{
		DocumentID: f.b, Status: model.FoundryExtractionExtracted, Releases: []model.FoundryReleaseOutcome{written(running)},
	}, true))
	assert.Equal(t, map[string]string{failed.ID: "withdrawn", running.ID: "pending"}, f.applied(t, failed, running))
	f.end(t, job, "ran", model.FoundryGraphJobSucceeded)
	next := f.liveJob(t)
	require.NotNil(t, next, "the job's end sends the withdraw it became")
	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	assert.Equal(t, f.b, owed[0].DocumentID)
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{running.ID: model.FoundryReviewWithdraw}, f.sentKinds(t, f.b))
	f.sent(t, job, owed[0], 0, withdrawn(running, model.FoundryReleaseOutcomeWithdrawn, ""))
	f.end(t, job, "after", model.FoundryGraphJobSucceeded)
	assert.Equal(t, map[string]string{running.ID: "withdrawn"}, f.applied(t, running))
	assert.Nil(t, f.liveJob(t))
}

// A take-back whose resend job fails is unconfirmed, and is not owed again by
// that job's own end; the end of the ontology's next job owes it again, so it
// never waits on a person, who has no card to send it from.
func TestFoundryTakeBackWhoseJobFailed(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "WFAIL")
	d := f.held(f.a, 0, readA)
	f.writeApproval(t, "1", d)
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, d.ID,
		&model.FoundryGraphJob{ID: id26("RS-WFAIL-2")}))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, f.claim(t), "failed", model.FoundryGraphJobFailed))
	assert.Equal(t, map[string]string{d.ID: "pending/job_failed"}, f.applied(t, d))
	got, err := f.db.ListFoundryDecisionsApplied(ctx, f.ontology.ID, []string{d.ID})
	require.NoError(t, err)
	assert.True(t, got[d.ID].Unconfirmed())
	assert.Nil(t, f.liveJob(t), "a failed resend is never followed by itself")
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{d.ID: model.FoundryReviewWithdraw}, f.sentKinds(t, f.a),
		"its list still sends it")

	// Another claim of the other document is approved and written: that job's
	// end owes the take-back again.
	other := f.held(f.b, 0, readA)
	require.True(t, f.approve(t, "3", other))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	assert.Equal(t, f.b, owed[0].DocumentID)
	f.sent(t, job, owed[0], 1, written(other))
	f.end(t, job, "other", model.FoundryGraphJobSucceeded)
	assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))
	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	assert.Equal(t, f.a, owed[0].DocumentID)
	f.sent(t, job, owed[0], 0, withdrawn(d, model.FoundryReleaseOutcomeWithdrawn, ""))
	f.end(t, job, "again", model.FoundryGraphJobSucceeded)
	assert.Equal(t, map[string]string{d.ID: "withdrawn"}, f.applied(t, d))
	assert.Nil(t, f.liveJob(t))
}

// An approval a load being written copied when it was created, and wrote,
// while a person took it back from the load that was current: when that
// load becomes current its copy is taken back too, before anything else is
// sent, as its decision is gone. The load it was taken back from keeps its
// withdraw.
func TestFoundryTakeBackCarriedToALoadThatWroteIt(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "WCARRY")
	first := f.load
	d := f.held(f.a, 0, readA)
	f.writeApproval(t, "1", d)
	f.draftVersion(t, "WCARRY2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "WCARRY2")))
	second := id26("FJ-WCARRY2")
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, d.ID,
		&model.FoundryGraphJob{ID: id26("RS-WCARRY-2")}))
	assert.Equal(t, map[string]string{d.ID: "withdraw"}, kinds(t, first))
	assert.Equal(t, map[string]string{d.ID: "release"}, kinds(t, second), "copied as it was created")

	// The new load writes its documents, the copied approval with them.
	load := f.claim(t)
	require.Equal(t, second, load.ID)
	for _, doc := range []string{f.a, f.b} {
		e := extracted(doc)
		if doc == f.a {
			e.Releases = []model.FoundryReleaseOutcome{written(d)}
		}
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, load.ID, f.worker, e, false))
	}
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, load, "carry", model.FoundryGraphJobSucceeded))
	require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
	f.load = second
	assert.Equal(t, map[string]string{d.ID: "withdraw"}, kinds(t, second))
	assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))
	resend := f.liveJob(t)
	require.NotNil(t, resend)
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, second)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	assert.Equal(t, f.a, owed[0].DocumentID)
	f.sent(t, job, owed[0], 0, withdrawn(d, model.FoundryReleaseOutcomeWithdrawn, ""))
	f.end(t, job, "back", model.FoundryGraphJobSucceeded)
	assert.Equal(t, map[string]string{d.ID: "withdrawn"}, f.applied(t, d))
	assert.Nil(t, f.liveJob(t))
}

// retried ends job as an attempt that failed with a retry to come: queued
// again, with the attempt's start kept.
func (f resendFixture) retried(t *testing.T, job *model.FoundryGraphJob) {
	t.Helper()
	later := time.Now().Add(time.Hour)
	_, err := f.db.FinishFoundryGraphJob(context.Background(), job.ID, f.worker, model.FoundryGraphJobOutcome{
		Status: model.FoundryGraphJobFailed, Error: "the stream broke; Helix will retry", RetryAt: &later,
	})
	require.NoError(t, err)
	live := f.liveJob(t)
	require.NotNil(t, live)
	require.Equal(t, model.FoundryGraphJobQueued, live.Status)
	require.NotNil(t, live.StartedAt)
}

// While a job of the current load is in flight it may be sending any copy
// of its list, and may have written one in an attempt whose word was never
// recorded: removing an approval then takes it back, whatever its copy's
// status, rather than deleting a copy the engine may have written. In flight
// is running, or queued again after an attempt started. So is a rejection
// refused while a resend in flight may be writing the claim.
func TestFoundryTakeBackWhileAJobIsInFlight(t *testing.T) {
	ctx := context.Background()

	// A copy the load made as it was created, of a document it has yet to
	// read, as a load made current with that document still to read holds:
	// the job reading it sends the copy with it.
	t.Run("a copy made with the load, of a document it has yet to read", func(t *testing.T) {
		f := newResendFixture(t, "WFLY")
		d := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", d))
		_, err := testDB.Exec(`UPDATE foundry_load_decisions SET applied_status=NULL WHERE load_id=? AND id=?`, f.load, d.ID)
		require.NoError(t, err)
		_, err = testDB.Exec(`DELETE FROM foundry_load_extractions WHERE load_id=? AND document_id=?`, f.load, f.a)
		require.NoError(t, err)
		job := f.claim(t)
		require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, d.ID,
			&model.FoundryGraphJob{ID: id26("RS-WFLY-2")}))
		assert.Equal(t, map[string]string{d.ID: "withdraw"}, kinds(t, f.load), "the job may be writing it")
		assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))
		f.end(t, job, "ran", model.FoundryGraphJobSucceeded)
	})

	// An attempt sent the approval, the engine wrote it, and the attempt
	// failed before its word was recorded: the job is queued again.
	t.Run("an approval its job's failed attempt may have written", func(t *testing.T) {
		f := newResendFixture(t, "WRTY")
		d := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", d))
		f.retried(t, f.claim(t))
		require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, d.ID,
			&model.FoundryGraphJob{ID: id26("RS-WRTY-2")}))
		assert.Equal(t, map[string]string{d.ID: "withdraw"}, kinds(t, f.load))
		assert.Equal(t, map[string]model.FoundryReviewDecisionKind{d.ID: model.FoundryReviewWithdraw}, f.sentKinds(t, f.a),
			"the retry sends the withdraw in its place")
	})

	t.Run("not a rejection while that retry may be writing the claim", func(t *testing.T) {
		f := newResendFixture(t, "WRTX")
		d := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", d))
		f.retried(t, f.claim(t))
		assert.ErrorIs(t, f.db.SettleFoundryReject(ctx, f.ontology.WorkspaceID, f.load, f.rejected(f.a, 0, readA)), store.ErrConflict)
		assert.Equal(t, map[string]string{d.ID: "release"}, kinds(t, f.load))
	})

	// A job queued that never started has sent nothing: a copy no job sent
	// is only dropped, as before.
	t.Run("not a job queued that never started", func(t *testing.T) {
		f := newResendFixture(t, "WQUE")
		d := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", d))
		live := f.liveJob(t)
		require.NotNil(t, live)
		require.Nil(t, live.StartedAt)
		require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, d.ID, nil))
		assert.Empty(t, kinds(t, f.load))
	})
}

// countsOf reads a document's facts and reviews in the fixture's load.
func (f resendFixture) countsOf(t *testing.T, doc string) (facts, reviews int) {
	t.Helper()
	row, err := f.db.GetFoundryDocumentExtraction(context.Background(), f.load, doc)
	require.NoError(t, err)
	return row.Counts.Facts, row.Counts.Reviews
}

// The engine takes back every withdraw it is sent before it reads any
// document, and says what it did only in that document's event (contract
// 4.16.0). What it did stands whatever became of the document: a failed
// event's word is recorded, a document that needed a model call (no event)
// had its withdraws taken back all the same, and a withdraw sent again after
// a run that stopped reads not_written, which, of an approval the load had
// recorded written, is the take-back done. Each moves the document's counts
// back once: a fact fewer, a review more. A withdraw never sent (its
// document is gone) is refused, and what it would have taken back stays.
func TestFoundryTakeBackWhateverBecameOfItsDocument(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, tag string) (resendFixture, *model.FoundryReviewDecision, *model.FoundryGraphJob, *model.FoundryLoadResend) {
		f := newResendFixture(t, tag)
		_, err := testDB.Exec(`UPDATE foundry_load_extractions SET reviews=3 WHERE load_id=? AND document_id=?`, f.load, f.a)
		require.NoError(t, err)
		d := f.held(f.a, 0, readA)
		f.writeApproval(t, "1", d)
		facts, reviews := f.countsOf(t, f.a)
		require.Equal(t, [2]int{2, 2}, [2]int{facts, reviews}, "the commit's fact and the approval's")
		require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, d.ID,
			&model.FoundryGraphJob{ID: id26("RS-" + tag + "-2")}))
		job := f.claim(t)
		owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
		require.NoError(t, err)
		require.Len(t, owed, 1)
		return f, d, job, owed[0]
	}
	back := func(t *testing.T, f resendFixture, d *model.FoundryReviewDecision) {
		t.Helper()
		assert.Equal(t, map[string]string{d.ID: "withdrawn"}, f.applied(t, d))
		facts, reviews := f.countsOf(t, f.a)
		assert.Equal(t, [2]int{1, 3}, [2]int{facts, reviews}, "a fact fewer and a review more, once")
		assert.Empty(t, f.sentKinds(t, f.a))
	}

	t.Run("a failed event's word", func(t *testing.T) {
		f, d, job, o := setup(t, "WFAILD")
		require.NoError(t, f.db.RecordFoundryReleaseOutcomes(ctx, job.ID, f.worker, f.a,
			[]model.FoundryReleaseOutcome{withdrawn(d, model.FoundryReleaseOutcomeWithdrawn, "")}))
		require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
			DocumentID: f.a, Reason: o.Reason, Requests: o.Requests, Refused: model.FoundryReleaseDocumentFailed, TakenBack: true,
		}))
		back(t, f, d)
		assert.ErrorIs(t, f.db.RecordFoundryReleaseOutcomes(ctx, job.ID, "another-worker", f.a,
			[]model.FoundryReleaseOutcome{withdrawn(d, model.FoundryReleaseOutcomeWithdrawn, "")}), store.ErrNotFound, "the job's lease")
	})

	t.Run("no event, its document needing a model call", func(t *testing.T) {
		f, d, job, o := setup(t, "WNOEV")
		require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
			DocumentID: f.a, Reason: o.Reason, Requests: o.Requests, Refused: model.FoundryReleaseNotReplayed, TakenBack: true,
		}))
		back(t, f, d)
	})

	t.Run("sent again after a run that stopped", func(t *testing.T) {
		f, d, job, _ := setup(t, "WSTOP")
		f.retried(t, job)
		f.due(t)
		job = f.claim(t)
		owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
		require.NoError(t, err)
		f.sent(t, job, owed[0], 0, withdrawn(d, model.FoundryReleaseOutcomeNotWritten, ""))
		back(t, f, d)
	})

	t.Run("never sent, its document gone", func(t *testing.T) {
		f, d, job, o := setup(t, "WGONE")
		require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
			DocumentID: f.a, Reason: o.Reason, Requests: o.Requests, Refused: model.FoundryReleaseDocumentGone,
		}))
		assert.Equal(t, map[string]string{d.ID: "refused/document_gone"}, f.applied(t, d))
		facts, reviews := f.countsOf(t, f.a)
		assert.Equal(t, [2]int{2, 2}, [2]int{facts, reviews})
	})

	// A withdraw of a copy the load never recorded written (here, one whose
	// job failed before its word) moves nothing back, whatever the engine
	// says: nothing moved forward for it.
	t.Run("of an approval the load never recorded written", func(t *testing.T) {
		f := newResendFixture(t, "WNOTW")
		d := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", d))
		assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, f.claim(t), "failed", model.FoundryGraphJobFailed))
		facts, reviews := f.countsOf(t, f.a)
		require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, d.ID,
			&model.FoundryGraphJob{ID: id26("RS-WNOTW-2")}))
		job := f.claim(t)
		owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
		require.NoError(t, err)
		f.sent(t, job, owed[0], 0, withdrawn(d, model.FoundryReleaseOutcomeNotWritten, ""))
		assert.Equal(t, map[string]string{d.ID: "not_written"}, f.applied(t, d))
		after, afterReviews := f.countsOf(t, f.a)
		assert.Equal(t, [2]int{facts, reviews}, [2]int{after, afterReviews})
	})
}

// A rejection deletes the claim's approvals no load can have written, and
// leaves a copy that a job of another load in flight may be writing: that
// load's list sends it no more, and when the load becomes current its carry
// takes back what the job wrote (review of #305, finding 6).
func TestFoundryRejectLeavesACopyAJobInFlightMayWrite(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "XFLY")
	first := f.load
	x := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", x))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, first)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 0, refused(x, "unresolved"))
	f.end(t, job, "refused", model.FoundryGraphJobSucceeded)

	// A new load copies the approval, and its job starts sending it.
	f.draftVersion(t, "XFLY2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "XFLY2")))
	second := id26("FJ-XFLY2")
	load := f.claim(t)
	require.Equal(t, second, load.ID)
	f.reject(t, f.rejected(f.a, 0, readA))
	assert.Empty(t, f.copies(t, first), "the current load refused it: nothing of it is there")
	assert.Equal(t, map[string]string{x.ID: ""}, f.copies(t, second), "the job in flight may be writing it")
	assert.Empty(t, f.listed(t, second), "though its list sends it no more")

	// The job wrote it: when its load becomes current the carry takes it back.
	wrote := extracted(f.a)
	wrote.Releases = []model.FoundryReleaseOutcome{written(x)}
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, load.ID, f.worker, wrote, false))
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, load.ID, f.worker, extracted(f.b), false))
	f.end(t, load, "loaded", model.FoundryGraphJobSucceeded)
	require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
	assert.Equal(t, map[string]string{x.ID: "withdraw"}, kinds(t, second))
	live := f.liveJob(t)
	require.NotNil(t, live)
	assert.Equal(t, model.FoundryGraphJobResend, live.Kind)
}

// The decisions list says of each release whether removing it takes it back,
// by the rule removal applies (foundryMayHaveWritten): the site words its
// button and notice by it, so the two never disagree (review of #305, minor 8).
func TestFoundryListSaysWhatARemovalTakesBack(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "WSAYS")
	written, waits := f.held(f.a, 0, readA), f.held(f.b, 0, readA)
	f.writeApproval(t, "1", written)
	require.True(t, f.approve(t, "2", waits))
	takes := func() map[string]bool {
		got, err := f.db.ListFoundryDecisionsApplied(ctx, f.ontology.ID, []string{written.ID, waits.ID})
		require.NoError(t, err)
		return map[string]bool{written.ID: got[written.ID].TakesBack, waits.ID: got[waits.ID].TakesBack}
	}
	assert.Equal(t, map[string]bool{written.ID: true, waits.ID: false}, takes())
	// Its resend running, the waiting one may be written now.
	f.claim(t)
	assert.Equal(t, map[string]bool{written.ID: true, waits.ID: true}, takes())
}
