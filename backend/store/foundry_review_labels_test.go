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

// A decision made on a card of the review queue keeps how its card was
// drawn, and lists with it; a load's list copies the decision without it.
// An approval of the statement it approved keeps it with who first decided
// and when, as it is the decision first made; a rejection, a name or a
// value decided again is a decision made now, drawn as it was made.
func TestFoundryDecisionDrawn(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "DRAWN")
	approval := f.held(f.a, 0, readA)
	approval.Drawn = ptr(model.FoundryDrawnRandom)
	f.approve(t, "d1", approval)
	require.NotNil(t, approval.Drawn)
	assert.Equal(t, model.FoundryDrawnRandom, *approval.Drawn, "the stored decision reads back as drawn")

	saved, err := f.db.ListFoundryReviewDecisions(ctx, f.ontology.ID, "", nil, 100)
	require.NoError(t, err)
	require.Len(t, saved, 1)
	require.NotNil(t, saved[0].Drawn)
	assert.Equal(t, model.FoundryDrawnRandom, *saved[0].Drawn)
	got, err := f.db.GetFoundryReviewDecision(ctx, f.ontology.ID, approval.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryDrawnRandom, *got.Drawn)
	_, err = f.db.GetFoundryReviewDecision(ctx, f.ontology.ID, id26("nope"))
	assert.ErrorIs(t, err, store.ErrNotFound)

	listed, err := f.db.ListFoundryLoadDecisions(ctx, f.load, "", nil, 100)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Nil(t, listed[0].Drawn, "a load's list carries no draw")

	// The same statement approved again, outside the queue: the decision
	// first made stands, drawn as it was.
	again := f.held(f.a, 0, readA)
	again.ID = id26("DRAWN-again")
	f.approve(t, "d2", again)
	assert.Equal(t, approval.ID, again.ID)
	require.NotNil(t, again.Drawn)
	assert.Equal(t, model.FoundryDrawnRandom, *again.Drawn)

	// Rejected from the queue's order instead: a new decision, ranked.
	rejection := f.rejected(f.a, 0, readA)
	rejection.Drawn = ptr(model.FoundryDrawnRanked)
	f.reject(t, rejection)
	require.NotNil(t, rejection.Drawn)
	assert.Equal(t, model.FoundryDrawnRanked, *rejection.Drawn)
	// And again from a document's page: made now, drawn by nothing.
	plain := f.rejected(f.a, 0, readB)
	f.reject(t, plain)
	assert.Equal(t, rejection.ID, plain.ID)
	assert.Nil(t, plain.Drawn)

	// A draw the store doesn't know is refused, as the column's CHECK does.
	bad := f.held(f.b, 0, readA)
	bad.Drawn = ptr(model.FoundryReviewDrawn("lucky"))
	_, err = f.db.SettleFoundryReleases(ctx, f.ontology.WorkspaceID, f.load, []*model.FoundryReviewDecision{bad}, &model.FoundryGraphJob{ID: id26("RS-bad")})
	assert.Error(t, err)
}

// label is a person's word on the claim n of doc, held (a claim key) or
// accepted (a fact key).
func (f resendFixture) label(doc string, n int, user string, held bool, verdict model.FoundryVerdict) *model.FoundryReviewLabel {
	kind := "f"
	if held {
		kind = "h"
	}
	l := &model.FoundryReviewLabel{ID: id26("LB-" + kind + user + "-" + string(rune('0'+n))), OntologyID: f.ontology.ID, LoadID: f.load, Drawn: model.FoundryLabelRandom,
		DocID: doc, Span: model.FoundryClaimSpan{Section: uint32(n), Start: 4, End: 20}, Verdict: verdict, DecidedByUserID: id26(user)}
	if held {
		key, hash := strings.Repeat("ab", 32), readA
		l.ClaimKey, l.StatementHash = &key, &hash
	} else {
		key := "fact-" + string(rune('0'+n))
		l.FactKey = &key
	}
	return l
}

// A label is kept as said, one per person and claim: saying it again
// replaces what it says and keeps its id; another person's is another
// label. Labels list by id and by claim, and change no decision.
func TestFoundryReviewLabels(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "LABEL")
	ws := f.ontology.WorkspaceID

	first := f.label(f.a, 0, "ann", false, model.FoundryVerdictRight)
	link, score := "partner_of", 0.93
	first.Link, first.Confidence = &link, &score
	require.NoError(t, f.db.SaveFoundryReviewLabel(ctx, ws, first))
	assert.Equal(t, id26("LB-fann-0"), first.ID)
	assert.False(t, first.DecidedAt.IsZero())
	assert.Equal(t, 0.93, *first.Confidence)
	assert.Equal(t, "partner_of", *first.Link)

	// Said again: the same label, saying what it says now.
	again := f.label(f.a, 0, "ann", false, model.FoundryVerdictWrong)
	again.ID = id26("LB-ann-again")
	require.NoError(t, f.db.SaveFoundryReviewLabel(ctx, ws, again))
	assert.Equal(t, first.ID, again.ID)
	assert.Equal(t, model.FoundryVerdictWrong, again.Verdict)
	assert.Nil(t, again.Confidence, "what it says now, its score included")

	// Another person's word on the claim, a re-check of Ann's.
	wrong := model.FoundryVerdictWrong
	bob := f.label(f.a, 0, "bob", false, model.FoundryVerdictRight)
	bob.Drawn, bob.Against = model.FoundryLabelRecheck, &wrong
	require.NoError(t, f.db.SaveFoundryReviewLabel(ctx, ws, bob))
	assert.NotEqual(t, first.ID, bob.ID)
	require.NotNil(t, bob.Against)
	assert.Equal(t, model.FoundryVerdictWrong, *bob.Against)

	// A held claim at the same place is another claim.
	held := f.label(f.a, 0, "ann", true, model.FoundryVerdictRight)
	decision := id26("DEC-1")
	held.DecisionID = &decision
	require.NoError(t, f.db.SaveFoundryReviewLabel(ctx, ws, held))
	assert.NotEqual(t, first.ID, held.ID)
	assert.Equal(t, decision, *held.DecisionID)

	fact := "fact-0"
	of, err := f.db.FoundryReviewLabelsOf(ctx, f.ontology.ID, f.a, first.Span, nil, &fact)
	require.NoError(t, err)
	require.Len(t, of, 2)
	assert.Equal(t, []string{first.ID, bob.ID}, []string{of[0].ID, of[1].ID}, "oldest first")
	claim := strings.Repeat("ab", 32)
	of, err = f.db.FoundryReviewLabelsOf(ctx, f.ontology.ID, f.a, first.Span, &claim, nil)
	require.NoError(t, err)
	require.Len(t, of, 1)
	assert.Equal(t, held.ID, of[0].ID)

	got, err := f.db.GetFoundryReviewLabel(ctx, f.ontology.ID, bob.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryLabelRecheck, got.Drawn)
	assert.Equal(t, model.FoundryVerdictWrong, *got.Against)
	_, err = f.db.GetFoundryReviewLabel(ctx, f.ontology.ID, id26("nope"))
	assert.ErrorIs(t, err, store.ErrNotFound)

	all, err := f.db.ListFoundryReviewLabels(ctx, f.ontology.ID, "", 100)
	require.NoError(t, err)
	assert.Len(t, all, 3)
	page, err := f.db.ListFoundryReviewLabels(ctx, f.ontology.ID, all[0].ID, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, all[1].ID, page[0].ID)
	_, err = f.db.ListFoundryReviewLabels(ctx, f.ontology.ID, "", 0)
	assert.Error(t, err)

	// No decision is made or changed.
	decisions, err := f.db.ListFoundryReviewDecisions(ctx, f.ontology.ID, "", nil, 100)
	require.NoError(t, err)
	assert.Empty(t, decisions)

	// What the table refuses, Validate refuses first; another workspace's
	// ontology isn't found.
	for name, mutate := range map[string]func(*model.FoundryReviewLabel){
		"both keys":      func(l *model.FoundryReviewLabel) { k := "f"; l.FactKey = &k },
		"no key":         func(l *model.FoundryReviewLabel) { l.ClaimKey, l.StatementHash = nil, nil },
		"empty span":     func(l *model.FoundryReviewLabel) { l.Span.End = l.Span.Start },
		"verdict":        func(l *model.FoundryReviewLabel) { l.Verdict = "maybe" },
		"drawn":          func(l *model.FoundryReviewLabel) { l.Drawn = "ranked" },
		"recheck alone":  func(l *model.FoundryReviewLabel) { l.Drawn = model.FoundryLabelRecheck },
		"against random": func(l *model.FoundryReviewLabel) { l.Against = &wrong },
		"score":          func(l *model.FoundryReviewLabel) { c := 1.5; l.Confidence = &c },
	} {
		l := f.label(f.b, 1, "cat", true, model.FoundryVerdictRight)
		mutate(l)
		assert.Error(t, f.db.SaveFoundryReviewLabel(ctx, ws, l), name)
	}
	assert.ErrorIs(t, f.db.SaveFoundryReviewLabel(ctx, id26("other-ws"), f.label(f.b, 1, "cat", true, model.FoundryVerdictRight)), store.ErrNotFound)
}
