package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rejected is a reject of the claim n of doc, read as hash: the claim
// resendFixture.held releases.
func (f resendFixture) rejected(doc string, n int, hash string) *model.FoundryReviewDecision {
	d := f.held(doc, n, hash)
	d.ID, d.Kind = id26("RJ-"+strings.TrimRight(d.ID, "0")), model.FoundryReviewReject
	return d
}

// reject settles a rejection against the fixture's load.
func (f resendFixture) reject(t *testing.T, d *model.FoundryReviewDecision) {
	t.Helper()
	require.NoError(t, f.db.SettleFoundryReject(context.Background(), f.ontology.WorkspaceID, f.load, d))
}

// kinds are the ontology's saved decisions' kinds, by id.
func (f resendFixture) kinds(t *testing.T) map[string]model.FoundryReviewDecisionKind {
	t.Helper()
	all, err := f.db.ListFoundryReviewDecisions(context.Background(), f.ontology.ID, "", nil, 100)
	require.NoError(t, err)
	out := map[string]model.FoundryReviewDecisionKind{}
	for _, d := range all {
		out[d.ID] = d.Kind
	}
	return out
}

// listed are the ids of the decisions a load's list holds.
func (f resendFixture) listed(t *testing.T, loadID string) []string {
	t.Helper()
	rows, err := f.db.ListFoundryLoadDecisions(context.Background(), loadID, "", nil, 100)
	require.NoError(t, err)
	ids := []string{}
	for _, d := range rows {
		ids = append(ids, d.ID)
	}
	return ids
}

// A rejection is saved and goes nowhere else: no load's list holds it, no
// document is owed, no job is queued, and nothing says what a load made of
// it. Rejecting again is the same decision. A load made later doesn't copy
// it, a version made current doesn't prune it, and removing it leaves
// nothing behind.
func TestFoundryReject(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "XNOW")
	d := f.rejected(f.a, 0, readA)
	f.reject(t, d)
	assert.Equal(t, model.FoundryReviewReject, d.Kind)
	assert.Empty(t, d.Type)
	assert.Equal(t, f.a, *d.DocID)
	assert.Equal(t, readA, *d.StatementHash)
	assert.Nil(t, d.Applied)
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{d.ID: model.FoundryReviewReject}, f.kinds(t))
	assert.Empty(t, f.listed(t, f.load), "no load's list holds a reject")
	assert.Empty(t, f.applied(t, d))
	assert.Empty(t, f.owed(t))
	assert.Zero(t, f.requests(t, f.load))
	assert.Nil(t, f.liveJob(t), "nothing is sent, so no job is queued")

	// Rejected again, as it reads now: the same decision.
	again := f.rejected(f.a, 0, readB)
	again.ID = id26("XNOW-again")
	f.reject(t, again)
	assert.Equal(t, d.ID, again.ID)
	assert.Equal(t, readB, *again.StatementHash)
	assert.Len(t, f.kinds(t), 1)

	// Another claim of the document, and an approval of a third.
	other, approved := f.rejected(f.a, 1, readA), f.held(f.b, 0, readA)
	f.reject(t, other)
	require.True(t, f.approve(t, "1", approved))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 1, written(approved))
	f.end(t, job, "xnow", model.FoundryGraphJobSucceeded)

	// A version made current prunes none: a reject names no type.
	next := f.draftVersion(t, "XNOW2", "[types.Client]\nprops = { name = \"string\" }")
	require.NoError(t, f.db.SetCurrentOntologyVersion(ctx, f.ontology.ID, next.ID, nil))
	assert.Len(t, f.kinds(t), 3)
	// A load made now copies the approval, and no reject.
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "XNOW2")))
	assert.Equal(t, []string{approved.ID}, f.listed(t, id26("FJ-XNOW2")))
	f.loaded(t, f.worker, f.a, f.b)

	// Only a reject is settled this way, against the load its review was
	// read from.
	assert.Error(t, f.db.SettleFoundryReject(ctx, f.ontology.WorkspaceID, f.load, f.held(f.b, 1, readA)))
	late := f.rejected(f.b, 1, readA)
	assert.ErrorIs(t, f.db.SettleFoundryReject(ctx, f.ontology.WorkspaceID, f.load, late), store.ErrConflict, "the load changed since the review was read")
	assert.ErrorIs(t, f.db.SettleFoundryReject(ctx, id26("another-workspace"), id26("FJ-XNOW2"), late), store.ErrNotFound)
	bad := f.rejected(f.b, 1, strings.Repeat("XY", 32))
	assert.Error(t, f.db.SettleFoundryReject(ctx, f.ontology.WorkspaceID, id26("FJ-XNOW2"), bad), "a hash the engine wouldn't take")

	// Removed: the decision is gone, and nothing was ever in a load.
	f.remove(t, "x", d)
	f.remove(t, "y", other)
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{approved.ID: model.FoundryReviewRelease}, f.kinds(t))
	assert.ErrorIs(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, d.ID, resendJob("XNOW-z")), store.ErrNotFound)
	assert.Nil(t, f.liveJob(t))
}

// A claim is approved or rejected, never both. A rejection takes an approval
// that waits, or that the load refused, out of the ontology's decisions and
// the load's list, so no resend writes a claim a person said is wrong; an
// approval takes a rejection's place the same way. A claim the load already
// wrote on an approval is no longer there to reject.
func TestFoundryRejectAndApprovalExcludeEachOther(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "XBOTH")

	// An approval that waits for its job: rejected, it leaves the list, and
	// the job that was queued sends the document with nothing to release.
	waits := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", waits))
	no := f.rejected(f.a, 0, readA)
	f.reject(t, no)
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{no.ID: model.FoundryReviewReject}, f.kinds(t))
	assert.Empty(t, f.listed(t, f.load))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	f.sent(t, job, owed[0], 0)
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "xboth", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))

	// Approved after all: the rejection goes, and the approval applies.
	yes := f.held(f.a, 0, readA)
	yes.ID = id26("XBOTH-yes")
	require.True(t, f.approve(t, "2", yes))
	assert.Equal(t, map[string]model.FoundryReviewDecisionKind{yes.ID: model.FoundryReviewRelease}, f.kinds(t))
	assert.Equal(t, []string{yes.ID}, f.listed(t, f.load))

	// The load refuses it: rejecting the claim then takes the approval back.
	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 0, refused(yes, "unresolved"))
	f.end(t, job, "xboth2", model.FoundryGraphJobSucceeded)
	require.Equal(t, map[string]string{yes.ID: "refused/unresolved"}, f.applied(t, yes))
	f.reject(t, no)
	assert.Len(t, f.kinds(t), 1)
	assert.Equal(t, model.FoundryReviewReject, f.kinds(t)[no.ID])
	assert.Empty(t, f.listed(t, f.load))

	// A claim the load wrote is a fact there, with no review left to reject:
	// the approval and what the load says of it stay.
	fact := f.held(f.b, 0, readA)
	require.True(t, f.approve(t, "3", fact))
	job = f.claim(t)
	owed, err = f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	f.sent(t, job, owed[0], 1, written(fact))
	f.end(t, job, "xboth3", model.FoundryGraphJobSucceeded)
	assert.ErrorIs(t, f.db.SettleFoundryReject(ctx, f.ontology.WorkspaceID, f.load, f.rejected(f.b, 0, readA)), store.ErrConflict)
	assert.Equal(t, model.FoundryReviewRelease, f.kinds(t)[fact.ID])
	assert.Equal(t, map[string]string{fact.ID: "written"}, f.applied(t, fact))
	// Another claim of that document is still one to reject.
	f.reject(t, f.rejected(f.b, 1, readA))
	assert.Len(t, f.kinds(t), 3)
}

// A rejection takes the claim's approval from every load that has not
// written it, not only the current one.
func TestFoundryRejectTakesTheApprovalFromEveryLoad(t *testing.T) {
	ctx := context.Background()

	// A load being written copied the approval when it was created, and
	// would release it at the end of its run: exactly where a rebuild admits
	// what the current load refused.
	t.Run("a load being written", func(t *testing.T) {
		f := newResendFixture(t, "XWRITE")
		first := f.load
		x := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", x))
		job := f.claim(t)
		owed, err := f.db.ListFoundryLoadResends(ctx, first)
		require.NoError(t, err)
		f.sent(t, job, owed[0], 0, refused(x, "conflict"))
		f.end(t, job, "refused", model.FoundryGraphJobSucceeded)
		f.draftVersion(t, "XWRITE2", customerTOML)
		require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "XWRITE2")))
		second := id26("FJ-XWRITE2")
		require.Equal(t, []string{x.ID}, f.listed(t, second), "the new load copied the approval")

		no := f.rejected(f.a, 0, readA)
		f.reject(t, no)
		assert.Equal(t, map[string]model.FoundryReviewDecisionKind{no.ID: model.FoundryReviewReject}, f.kinds(t))
		assert.Empty(t, f.copies(t, first))
		assert.Empty(t, f.copies(t, second), "the load being written releases nothing of it")

		f.loaded(t, f.worker, f.a, f.b)
		require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
		assert.Empty(t, f.copies(t, second))
		assert.Nil(t, f.liveJob(t), "and nothing is carried to it")
	})

	// A copy that waits in a load that is not current would be sent if that
	// load were made current again.
	t.Run("an older load made current again", func(t *testing.T) {
		f := newResendFixture(t, "XOLD")
		first := f.load
		f.draftVersion(t, "XOLD2", customerTOML)
		require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "XOLD2")))
		x := f.held(f.a, 0, readA)
		require.False(t, f.approve(t, "1", x), "the load job goes on")
		f.loaded(t, f.worker, f.a, f.b)
		second := id26("FJ-XOLD2")
		f.load = second
		job := f.claim(t)
		require.Equal(t, model.FoundryGraphJobResend, job.Kind)
		owed, err := f.db.ListFoundryLoadResends(ctx, second)
		require.NoError(t, err)
		require.Len(t, owed, 1)
		f.sent(t, job, owed[0], 0, refused(x, "conflict"))
		f.end(t, job, "second", model.FoundryGraphJobSucceeded)
		require.Equal(t, map[string]string{x.ID: "pending"}, f.copies(t, first), "the first load's copy still waits")

		f.reject(t, f.rejected(f.a, 0, readA))
		assert.Empty(t, f.copies(t, first), "taken from the load that is not current too")
		assert.Empty(t, f.copies(t, second))

		// Made current again, the first load sends the document it owed,
		// with nothing to release.
		_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("XOLD"), resendJob("XOLD"), noBudget)
		require.NoError(t, err)
		f.load = first
		assert.Empty(t, f.listed(t, first))
		job = f.claim(t)
		require.Equal(t, id26("FR-XOLD"), job.ID)
		owed, err = f.db.ListFoundryLoadResends(ctx, first)
		require.NoError(t, err)
		require.Len(t, owed, 1)
		f.sent(t, job, owed[0], 0)
		assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "back", model.FoundryGraphJobSucceeded))
		assert.Nil(t, f.liveJob(t))
	})

	// A load that wrote the claim keeps its copy: the fact is there.
	t.Run("a load that wrote it keeps its word", func(t *testing.T) {
		f := newResendFixture(t, "XFACT")
		first := f.load
		x := f.held(f.b, 0, readA)
		require.True(t, f.approve(t, "1", x))
		job := f.claim(t)
		owed, err := f.db.ListFoundryLoadResends(ctx, first)
		require.NoError(t, err)
		f.sent(t, job, owed[0], 1, written(x))
		f.end(t, job, "written", model.FoundryGraphJobSucceeded)
		f.draftVersion(t, "XFACT2", customerTOML)
		require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "XFACT2")))
		f.loaded(t, f.worker, f.a, f.b)
		second := id26("FJ-XFACT2")
		f.load = second
		require.Equal(t, map[string]string{x.ID: ""}, f.copies(t, second))

		f.reject(t, f.rejected(f.b, 0, readA))
		assert.Equal(t, map[string]string{x.ID: "written"}, f.copies(t, first))
		// The second load copied it and holds its document, and no job said
		// what became of it there, so it may have written it (contract
		// 4.16.0): the copy is left for the carry, which takes it back. The
		// next settle runs one.
		assert.Equal(t, map[string]string{x.ID: ""}, f.copies(t, second))
		assert.Empty(t, f.listed(t, second), "no Extract sends an approval of a rejected claim")
		other := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "2", other))
		assert.Equal(t, map[string]string{x.ID: "withdraw", other.ID: "release"}, kinds(t, second))
		assert.Equal(t, map[string]int{f.a: 1, f.b: 1}, f.owed(t))
	})

	// While a resend of the load runs with the approval waiting for it, the
	// engine may be writing the claim: the rejection is refused until the
	// job has said what became of it.
	t.Run("not while a resend is writing it", func(t *testing.T) {
		f := newResendFixture(t, "XRUN")
		x := f.held(f.a, 0, readA)
		require.True(t, f.approve(t, "1", x))
		no := f.rejected(f.a, 0, readA)
		job := f.claim(t)
		assert.ErrorIs(t, f.db.SettleFoundryReject(ctx, f.ontology.WorkspaceID, f.load, no), store.ErrConflict)
		assert.Equal(t, map[string]model.FoundryReviewDecisionKind{x.ID: model.FoundryReviewRelease}, f.kinds(t))
		assert.Equal(t, map[string]string{x.ID: "pending"}, f.copies(t, f.load))

		owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
		require.NoError(t, err)
		f.sent(t, job, owed[0], 0, refused(x, "conflict"))
		f.reject(t, no)
		assert.Equal(t, map[string]model.FoundryReviewDecisionKind{no.ID: model.FoundryReviewReject}, f.kinds(t))
		assert.Empty(t, f.copies(t, f.load))
	})
}
