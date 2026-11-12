package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A rebuild keys a sentence otherwise when the entities its claim concerns
// change, so the same statement at the same place has another claim key. From
// contract 4.14.0 the engine binds a release whose key names no review of its
// document by its place and statement hash: a decision saved under the old
// key is the claim's under the new one too.

// keyed is d under claim key k, as another load's review of the same
// sentence would key it, with id tag.
func keyed(d *model.FoundryReviewDecision, k, tag string) *model.FoundryReviewDecision {
	key, out := strings.Repeat(k, 32), *d
	out.ClaimKey, out.ID = &key, id26(tag)
	return &out
}

// stored is the ontology's saved decision id, as read back.
func (f resendFixture) stored(t *testing.T, id string) *model.FoundryReviewDecision {
	t.Helper()
	all, err := f.db.ListFoundryReviewDecisions(context.Background(), f.ontology.ID, "", nil, 100)
	require.NoError(t, err)
	for _, d := range all {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// sends are the ids of the release decisions a job of the load would send
// with doc.
func (f resendFixture) sends(t *testing.T, loadID, doc string) []string {
	t.Helper()
	rows, err := f.db.ListFoundryLoadDecisions(context.Background(), loadID, "", []string{doc}, 100)
	require.NoError(t, err)
	ids := []string{}
	for _, d := range rows {
		if d.Kind == model.FoundryReviewRelease {
			ids = append(ids, d.ID)
		}
	}
	return ids
}

// resent runs the queued resend of one document and records what the engine
// said of its releases.
func (f resendFixture) resent(t *testing.T, tag string, outcomes ...model.FoundryReleaseOutcome) {
	t.Helper()
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(context.Background(), f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	f.sent(t, job, owed[0], 0, outcomes...)
	f.end(t, job, tag, model.FoundryGraphJobSucceeded)
}

// Approved under the key K1, refused, and then rejected on the review a
// rebuild keyed K2: the approval goes from the ontology's decisions and from
// every load's list, so no resend of the document and no new load sends it,
// and the engine can't write through the place the claim a person rejected.
func TestFoundryRejectTakesAnApprovalSavedUnderAnotherKey(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "MKREJ")
	approved := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", approved))
	f.resent(t, "refused", refused(approved, "unresolved"))
	require.Equal(t, []string{approved.ID}, f.sends(t, f.load, f.a))

	no := keyed(f.rejected(f.a, 0, readA), "cd", "MKREJ-no")
	f.reject(t, no)
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{no.ID: model.FoundryReviewReject}, f.kinds(t))
	assert.Empty(t, f.copies(t, f.load))

	// The next resend of the document, for another claim of it, sends that
	// claim's approval alone.
	other := f.held(f.a, 1, readA)
	require.True(t, f.approve(t, "2", other))
	assert.Equal(t, []string{other.ID}, f.sends(t, f.load, f.a))
	f.resent(t, "other", written(other))
	// And so does a new load.
	f.draftVersion(t, "MKREJ2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "MKREJ2")))
	assert.Equal(t, []string{other.ID}, f.sends(t, id26("FJ-MKREJ2"), f.a))
}

// A rejection of another statement at the place, under another key, leaves
// an approval of what was read alone: it isn't that claim.
func TestFoundryRejectOfAnotherStatementLeavesTheApproval(t *testing.T) {
	f := newResendFixture(t, "MKREJB")
	kept := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", kept))
	f.resent(t, "refused", refused(kept, "unresolved"))
	f.reject(t, keyed(f.rejected(f.a, 0, readB), "cd", "MKREJB-no"))
	assert.Equal(t, model.FoundryReviewRelease, f.kinds(t)[kept.ID])
	assert.Equal(t, []string{kept.ID}, f.sends(t, f.load, f.a))
}

// The rejection's guards see an approval under another key of what was read
// at the place: while a resend of the load runs with it waiting the engine
// may be writing the claim, and once the load wrote it the claim is a fact.
func TestFoundryRejectWaitsForAnApprovalSavedUnderAnotherKey(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "MKRUN")
	approved := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", approved))
	no := keyed(f.rejected(f.a, 0, readA), "cd", "MKRUN-no")
	job := f.claim(t)
	assert.ErrorIs(t, f.db.SettleFoundryReject(ctx, f.ontology.WorkspaceID, f.load, no), store.ErrConflict)
	assert.Equal(t, map[string]string{approved.ID: "pending"}, f.copies(t, f.load))

	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 1, written(approved))
	f.end(t, job, "written", model.FoundryGraphJobSucceeded)
	assert.ErrorIs(t, f.db.SettleFoundryReject(ctx, f.ontology.WorkspaceID, f.load, no), store.ErrConflict, "the load wrote it")
	assert.Equal(t, map[string]string{approved.ID: "written"}, f.copies(t, f.load))
}

// An approval takes a rejection's place under another key of what it read
// at the place, as a rejection takes an approval's; and a rejection replaces
// one saved under another key, so removing the one left lists the claim.
func TestFoundryApprovalTakesARejectionSavedUnderAnotherKey(t *testing.T) {
	f := newResendFixture(t, "MKAPP")
	no := f.rejected(f.a, 0, readA)
	f.reject(t, no)
	again := keyed(f.rejected(f.a, 0, readA), "cd", "MKAPP-no2")
	f.reject(t, again)
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{again.ID: model.FoundryReviewReject}, f.kinds(t),
		"one rejection a claim")

	yes := keyed(f.held(f.a, 0, readA), "ef", "MKAPP-yes")
	require.True(t, f.approve(t, "1", yes))
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{yes.ID: model.FoundryReviewRelease}, f.kinds(t))
	assert.Equal(t, []string{yes.ID}, f.sends(t, f.load, f.a))
}

// Approving the review a rebuild keyed otherwise, of what an approval saved
// under the old key read there, approves that decision again: it takes the
// key the review has now and keeps its id, its time and who made it, so the
// engine is sent one decision of the claim and weighs it from when it was
// first approved, however a load keys the sentence.
func TestFoundryApprovalUnderAMovedKeyIsTheDecisionApprovedAgain(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "MKAGAIN")
	first := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", first))
	f.resent(t, "refused", refused(first, "unresolved"))
	at := f.stored(t, first.ID).DecidedAt

	// An approval of what the review read before at the new key goes too.
	older := keyed(f.held(f.a, 0, readB), "cd", "MKAGAIN-old")
	require.True(t, f.approve(t, "2", older))
	require.Len(t, f.kinds(t), 2)

	time.Sleep(10 * time.Millisecond)
	again := keyed(f.held(f.a, 0, readA), "cd", "MKAGAIN-again")
	again.DecidedByUserID = id26("another-user")
	require.False(t, f.approve(t, "3", again))
	assert.Equal(t, first.ID, again.ID, "the decision approved again")
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{first.ID: model.FoundryReviewRelease}, f.kinds(t))
	saved := f.stored(t, first.ID)
	assert.Equal(t, strings.Repeat("cd", 32), *saved.ClaimKey)
	assert.Equal(t, at, saved.DecidedAt)
	assert.Equal(t, id26("user"), saved.DecidedByUserID)
	assert.Equal(t, map[string]string{first.ID: "pending"}, f.copies(t, f.load))
	sent, err := f.db.ListFoundryLoadDecisions(ctx, f.load, "", []string{f.a}, 100)
	require.NoError(t, err)
	require.Len(t, sent, 1)
	assert.Equal(t, strings.Repeat("cd", 32), *sent[0].ClaimKey, "sent as the review stores it now")
	assert.Equal(t, at, sent[0].DecidedAt)
}

// Two approvals of one claim saved before an approval under a moved key was
// the decision approved again, one under each key: approving the claim again
// keeps the first and drops the other.
func TestFoundryApprovalAgainKeepsTheFirstOfTwo(t *testing.T) {
	f := newResendFixture(t, "MKTWO")
	one := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", one))
	f.resent(t, "refused", refused(one, "unresolved"))
	time.Sleep(10 * time.Millisecond)
	two := keyed(f.held(f.a, 0, readA), "cd", "MKTWO-two")
	_, err := testDB.Exec(`INSERT INTO foundry_review_decisions
 (id, ontology_id, kind, type, doc_id, claim_key, claim_section, claim_start, claim_end, statement_hash, decided_by_user_id)
 VALUES (?, ?, 'release', '', ?, ?, 0, 4, 20, ?, ?)`, two.ID, f.ontology.ID, f.a, *two.ClaimKey, readA, id26("user"))
	require.NoError(t, err)
	three := keyed(f.held(f.a, 0, readA), "ef", "MKTWO-three")
	require.True(t, f.approve(t, "2", three))
	assert.Equal(t, one.ID, three.ID)
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{one.ID: model.FoundryReviewRelease}, f.kinds(t))
	assert.Equal(t, []string{one.ID}, f.sends(t, f.load, f.a))
}

// A load that is not current keeps its copy of a decision under the key the
// claim had there. Made current again after the decision was approved under
// a moved key, it carries no second copy of it.
func TestFoundryCarryKeepsACopyUnderTheKeyItsLoadHad(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "MKCARRY")
	first := f.load
	x := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", x))
	f.resent(t, "refused", refused(x, "unresolved"))

	f.draftVersion(t, "MKCARRY2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "MKCARRY2")))
	f.loaded(t, f.worker, f.a, f.b)
	second := id26("FJ-MKCARRY2")
	f.load = second
	require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
	moved := keyed(f.held(f.a, 0, readA), "cd", "MKCARRY-moved")
	require.True(t, f.approve(t, "2", moved))
	require.Equal(t, x.ID, moved.ID)
	f.resent(t, "refused2", refused(x, "unresolved"))
	// The claim reads otherwise now, and is approved again as it reads: the
	// same decision, under the key it has here, of another statement. The
	// first load's copy has neither, and only its id says it is this one.
	again := keyed(f.held(f.a, 0, readB), "cd", "MKCARRY-again")
	require.True(t, f.approve(t, "3", again))
	require.Equal(t, x.ID, again.ID)
	f.resent(t, "refused3", refused(again, "unresolved"))

	_, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("MKCARRY"), resendJob("MKCARRY"), noBudget)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{x.ID: "refused/unresolved"}, f.copies(t, first), "its copy, under the key it had there")
	rows, err := f.db.ListFoundryLoadDecisions(ctx, first, "", []string{f.a}, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, strings.Repeat("ab", 32), *rows[0].ClaimKey)
}

// A release the ontology holds a rejection of is never sent, by its key or
// by what it read at its place under another key, whatever put both there:
// here a rejection saved before a rejection took approvals saved under
// another key.
func TestFoundryLoadListLeavesOutARejectedClaim(t *testing.T) {
	f := newResendFixture(t, "MKLIST")
	approved, other := f.held(f.a, 0, readA), f.held(f.a, 1, readA)
	require.True(t, f.approve(t, "1", approved, other))
	no := keyed(f.rejected(f.a, 0, readA), "cd", "MKLIST-no")
	_, err := testDB.Exec(`INSERT INTO foundry_review_decisions
 (id, ontology_id, kind, type, doc_id, claim_key, claim_section, claim_start, claim_end, statement_hash, decided_by_user_id)
 VALUES (?, ?, 'reject', '', ?, ?, 0, 4, 20, ?, ?)`, no.ID, f.ontology.ID, f.a, *no.ClaimKey, readA, id26("user"))
	require.NoError(t, err)
	assert.Equal(t, []string{other.ID}, f.sends(t, f.load, f.a))
	assert.Len(t, f.copies(t, f.load), 2, "the list's rows are left as they are")

	// The resend sends the other approval alone. Its job's end takes the
	// withheld copy from the list, so nothing waits on it and no resend
	// follows, and a new load copies none of it.
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(context.Background(), f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	f.sent(t, job, owed[0], 1, written(other))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "end", model.FoundryGraphJobSucceeded))
	assert.Equal(t, map[string]string{other.ID: "written"}, f.copies(t, f.load))
	assert.Nil(t, f.liveJob(t))
	f.draftVersion(t, "MKLIST2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(context.Background(), loadJob(f.ontology, "MKLIST2")))
	assert.Equal(t, map[string]string{other.ID: ""}, f.copies(t, id26("FJ-MKLIST2")))
}
