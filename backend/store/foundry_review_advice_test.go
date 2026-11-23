package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/reviewadvice"
	"github.com/helixdb/helix-foundry/backend/reviewcards"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// adviceDue makes f's load due advice now.
func (f cardsFixture) adviceDue(t *testing.T) {
	t.Helper()
	_, err := testDB.Exec(`UPDATE foundry_review_advice_loads SET due_at=NOW(6) - INTERVAL 1 SECOND WHERE load_id=?`, f.load)
	require.NoError(t, err)
}

// adviceClaim claims f's load's advice as worker; nil when none is due.
func (f cardsFixture) adviceClaim(t *testing.T, worker string) *model.FoundryReviewAdviceClaim {
	t.Helper()
	// Other tests' loads are never due here.
	_, err := testDB.Exec(`UPDATE foundry_review_advice_loads SET due_at=NULL WHERE load_id<>?`, f.load)
	require.NoError(t, err)
	c, err := f.db.ClaimFoundryReviewAdvice(context.Background(), worker, time.Minute)
	require.NoError(t, err)
	return c
}

// adviceSeconds is how far from now f's load is due, in seconds; nil when
// it is due no more.
func (f cardsFixture) adviceSeconds(t *testing.T) *float64 {
	t.Helper()
	var s *float64
	require.NoError(t, testDB.QueryRow(`SELECT TIMESTAMPDIFF(MICROSECOND, NOW(6), due_at) / 1e6 FROM foundry_review_advice_loads WHERE load_id=?`,
		f.load).Scan(&s))
	return s
}

// A load is owed advice as it becomes current and whenever its index
// stores; a store that lost its lease owes nothing.
func TestFoundryReviewAdviceOwing(t *testing.T) {
	ctx := context.Background()
	f := newCardsFixture(t, "rao")
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice_loads WHERE load_id=? AND workspace_id=? AND ontology_id=?
 AND claimed_by IS NULL AND stopped IS NULL`, f.load, f.ontology.WorkspaceID, f.ontology.ID), "made current, it is owed")
	assert.InDelta(t, 30, *f.adviceSeconds(t), 5, "30 s on")

	_, err := testDB.Exec(`DELETE FROM foundry_review_advice_loads WHERE load_id=?`, f.load)
	require.NoError(t, err)
	claims, err := f.db.ClaimFoundryReviewDocuments(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	_, err = testDB.Exec(`UPDATE foundry_review_owed SET due_at=NOW(6) - INTERVAL 1 SECOND WHERE load_id=?`, f.load)
	require.NoError(t, err)
	stored, err := f.db.StoreFoundryReviewDocuments(ctx, "w1", claims[0], []model.FoundryReviewIndexed{{DocID: f.a}, {DocID: f.b}})
	require.NoError(t, err)
	require.False(t, stored)
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice_loads WHERE load_id=?`, f.load), "a lost lease owes nothing")

	f.pass(t)
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice_loads WHERE load_id=?`, f.load), "a store owes it")

	// Made current again after another load, it is owed again.
	_, err = testDB.Exec(`UPDATE foundry_review_advice_loads SET due_at=NULL, stopped='done' WHERE load_id=?`, f.load)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_ontologies SET current_load_id=NULL WHERE id=?`, f.ontology.ID)
	require.NoError(t, err)
	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, f.load, extractJob("RAO"), resendJob("RAO"), noBudget)
	require.NoError(t, err)
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice_loads WHERE load_id=? AND stopped IS NULL AND due_at IS NOT NULL`, f.load))
}

// A claim takes a load once it is current and its index whole; it neither
// spins on one that isn't nor waits on the ontology a settle locks.
func TestFoundryReviewAdviceClaim(t *testing.T) {
	ctx := context.Background()
	f := newCardsFixture(t, "rac")
	assert.Nil(t, f.adviceClaim(t, "w1"), "not due for 30 s")

	// Not indexed yet: looked at again in a minute.
	f.adviceDue(t)
	assert.Nil(t, f.adviceClaim(t, "w1"))
	assert.InDelta(t, 60, *f.adviceSeconds(t), 5)

	f.pass(t)
	f.adviceDue(t)
	// A settle's lock on the ontology doesn't hold up a claim.
	tx, err := testDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`SELECT id FROM foundry_ontologies WHERE id=? FOR UPDATE`, f.ontology.ID)
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT id FROM foundry_loads WHERE id=? FOR UPDATE`, f.load)
	require.NoError(t, err)
	quick, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = testDB.ExecContext(quick, `UPDATE foundry_review_advice_loads SET due_at=NULL WHERE load_id<>?`, f.load)
	require.NoError(t, err)
	c, err := f.db.ClaimFoundryReviewAdvice(quick, "w1", time.Minute)
	require.NoError(t, err, "a claim waited on a settle's locks")
	require.NotNil(t, c)
	require.NoError(t, tx.Rollback())
	assert.Equal(t, model.FoundryReviewAdviceClaim{LoadID: f.load, WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID, OwedAt: c.OwedAt},
		*c)
	assert.Nil(t, f.adviceClaim(t, "w2"), "held by its lease")

	// Not current: due no more, until it is owed again.
	_, err = testDB.Exec(`UPDATE foundry_review_advice_loads SET claimed_by=NULL WHERE load_id=?`, f.load)
	require.NoError(t, err)
	f.adviceDue(t)
	_, err = testDB.Exec(`UPDATE foundry_ontologies SET current_load_id=NULL WHERE id=?`, f.ontology.ID)
	require.NoError(t, err)
	assert.Nil(t, f.adviceClaim(t, "w1"))
	assert.Nil(t, f.adviceSeconds(t))

	// Its ontology gone: its row goes.
	f.adviceDue(t)
	_, err = testDB.Exec(`UPDATE foundry_ontologies SET deleted_at=NOW(6) WHERE id=?`, f.ontology.ID)
	require.NoError(t, err)
	assert.Nil(t, f.adviceClaim(t, "w1"))
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice_loads WHERE load_id=?`, f.load))
}

// Renew, Store, Finish and BackOff act only while the caller holds its
// claim, and a re-owe during it wins.
func TestFoundryReviewAdviceLease(t *testing.T) {
	ctx := context.Background()
	f := newCardsFixture(t, "ral")
	f.pass(t)
	f.adviceDue(t)
	c := f.adviceClaim(t, "w1")
	require.NotNil(t, c)
	cards := f.list(t, model.FoundryReviewCardFilter{})
	require.GreaterOrEqual(t, len(cards), 3)
	row := func(card *model.FoundryReviewListedCard, outcome model.FoundryReviewAdviceOutcome) model.FoundryReviewAdviceRow {
		return model.FoundryReviewAdviceRow{CardID: card.ID, Outcome: outcome}
	}

	held, err := f.db.RenewFoundryReviewAdvice(ctx, "w2", *c, time.Hour)
	require.NoError(t, err)
	assert.False(t, held, "another worker's")
	held, err = f.db.RenewFoundryReviewAdvice(ctx, "w1", *c, 3*time.Minute)
	require.NoError(t, err)
	assert.True(t, held)
	assert.InDelta(t, 180, *f.adviceSeconds(t), 5)

	// A store forgets the failed attempts; a card's first row is kept.
	_, err = testDB.Exec(`UPDATE foundry_review_advice_loads SET attempts=2 WHERE load_id=?`, f.load)
	require.NoError(t, err)
	stored, err := f.db.StoreFoundryReviewAdvice(ctx, "w1", *c, []model.FoundryReviewAdviceRow{row(cards[0], model.FoundryReviewAdviceRules)})
	require.NoError(t, err)
	require.True(t, stored)
	stored, err = f.db.StoreFoundryReviewAdvice(ctx, "w1", *c, []model.FoundryReviewAdviceRow{row(cards[0], model.FoundryReviewAdviceSkipped)})
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice WHERE load_id=? AND outcome='rules'`, f.load))
	assert.Equal(t, 0, f.count(t, `SELECT attempts FROM foundry_review_advice_loads WHERE load_id=?`, f.load))
	stored, err = f.db.StoreFoundryReviewAdvice(ctx, "w2", *c, []model.FoundryReviewAdviceRow{row(cards[1], model.FoundryReviewAdviceRules)})
	require.NoError(t, err)
	assert.False(t, stored, "another worker's store")

	// Finish: done is due no more; a recheck is due then.
	ended, err := f.db.FinishFoundryReviewAdvice(ctx, "w1", *c, model.FoundryReviewAdvicePlan, time.Hour)
	require.NoError(t, err)
	require.True(t, ended)
	assert.InDelta(t, 3600, *f.adviceSeconds(t), 5)
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice_loads WHERE load_id=? AND stopped='plan' AND claimed_by IS NULL`, f.load))
	ended, err = f.db.FinishFoundryReviewAdvice(ctx, "w1", *c, model.FoundryReviewAdviceDone, 0)
	require.NoError(t, err)
	assert.False(t, ended, "a claim ended is held no more")

	// A lapsed lease stores nothing.
	f.adviceDue(t)
	c = f.adviceClaim(t, "w1")
	require.NotNil(t, c)
	_, err = testDB.Exec(`UPDATE foundry_review_advice_loads SET due_at=NOW(6) - INTERVAL 1 SECOND WHERE load_id=?`, f.load)
	require.NoError(t, err)
	stored, err = f.db.StoreFoundryReviewAdvice(ctx, "w1", *c, []model.FoundryReviewAdviceRow{row(cards[1], model.FoundryReviewAdviceRules)})
	require.NoError(t, err)
	assert.False(t, stored)
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice WHERE load_id=?`, f.load))

	// BackOff: one more back-off, due 30 s × 2ⁿ on, n the row's own count.
	c = f.adviceClaim(t, "w1")
	require.NotNil(t, c)
	_, err = testDB.Exec(`UPDATE foundry_review_advice_loads SET attempts=1 WHERE load_id=?`, f.load)
	require.NoError(t, err)
	backed, failed, err := f.db.BackOffFoundryReviewAdvice(ctx, "w1", *c, model.FoundryReviewAdviceReadFailed)
	require.NoError(t, err)
	require.True(t, backed)
	assert.False(t, failed)
	assert.InDelta(t, 60, *f.adviceSeconds(t), 5)
	assert.Equal(t, 2, f.count(t, `SELECT attempts FROM foundry_review_advice_loads WHERE load_id=?`, f.load))
	assert.Equal(t, 0, f.count(t, `SELECT failures FROM foundry_review_advice_loads WHERE load_id=?`, f.load), "no provider failed")

	// Only the provider's failures stop the load, the third in a row,
	// counted by the row however many back-offs came between; reads that
	// fail never do.
	backOff := func(failure model.FoundryReviewAdviceFailure) bool {
		t.Helper()
		f.adviceDue(t)
		c := f.adviceClaim(t, "w1")
		require.NotNil(t, c)
		held, failed, err := f.db.BackOffFoundryReviewAdvice(ctx, "w1", *c, failure)
		require.NoError(t, err)
		require.True(t, held)
		return failed
	}
	for range 5 {
		assert.False(t, backOff(model.FoundryReviewAdviceReadFailed))
	}
	assert.False(t, backOff(model.FoundryReviewAdviceProviderFailed))
	assert.False(t, backOff(model.FoundryReviewAdviceProviderFailed))
	assert.InDelta(t, 3600, *f.adviceSeconds(t), 5, "a back-off is at most an hour")
	// A store of rows the model answered forgets its failures; one of rows
	// no call wrote (rules, skipped) only the back-offs.
	f.adviceDue(t)
	c = f.adviceClaim(t, "w1")
	require.NotNil(t, c)
	stored, err = f.db.StoreFoundryReviewAdvice(ctx, "w1", *c, []model.FoundryReviewAdviceRow{row(cards[1], model.FoundryReviewAdviceSkipped)})
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice_loads WHERE load_id=? AND attempts=0 AND failures=2`, f.load))
	held, failed, err = f.db.BackOffFoundryReviewAdvice(ctx, "w1", *c, model.FoundryReviewAdviceProviderFailed)
	require.NoError(t, err)
	require.True(t, held)
	assert.True(t, failed, "the third failure in a row")
	assert.Nil(t, f.adviceSeconds(t), "due no more until it is owed again")
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice_loads WHERE load_id=? AND stopped='failed' AND claimed_by IS NULL
 AND failures=3`, f.load))
	f.adviceDue(t)
	_, err = testDB.Exec(`UPDATE foundry_review_advice_loads SET failures=2 WHERE load_id=?`, f.load)
	require.NoError(t, err)
	c = f.adviceClaim(t, "w1")
	require.NotNil(t, c)
	stored, err = f.db.StoreFoundryReviewAdvice(ctx, "w1", *c, []model.FoundryReviewAdviceRow{{CardID: cards[2].ID,
		Outcome: model.FoundryReviewAdviceNone, Offered: []reviewadvice.Choice{}, Model: "claude", PromptVersion: reviewadvice.PromptVersion}})
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, 0, f.count(t, `SELECT failures FROM foundry_review_advice_loads WHERE load_id=?`, f.load), "an answer forgets them")
	ended, err = f.db.FinishFoundryReviewAdvice(ctx, "w1", *c, model.FoundryReviewAdviceDone, 0)
	require.NoError(t, err)
	require.True(t, ended)
	held, _, err = f.db.BackOffFoundryReviewAdvice(ctx, "w1", *c, model.FoundryReviewAdviceProviderFailed)
	require.NoError(t, err)
	assert.False(t, held, "a claim ended is held no more")

	// Owed again during a claim (a store of the index): the lease is
	// extended past the owe, its back-offs and failures forgotten, and its Finish and
	// BackOff leave it due 30 s after that owe, with no stop, so a load
	// stopped failed is advised again.
	for _, end := range []string{"finish", "backoff"} {
		f.adviceDue(t)
		c = f.adviceClaim(t, "w1")
		require.NotNil(t, c, end)
		_, err = testDB.Exec(`UPDATE foundry_review_advice_loads SET due_at=NOW(6) + INTERVAL 1 SECOND, attempts=2, failures=2 WHERE load_id=?`,
			f.load)
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
		_, err = testDB.Exec(`INSERT INTO foundry_review_owed (load_id, doc_id) VALUES (?, ?)`, f.load, f.a)
		require.NoError(t, err)
		f.index(t, "indexer", nil)
		assert.InDelta(t, 30, *f.adviceSeconds(t), 5, "%s: the lease is extended past the owe", end)
		assert.Equal(t, 0, f.count(t, `SELECT attempts + failures FROM foundry_review_advice_loads WHERE load_id=?`, f.load),
			"%s: the owe forgets its back-offs and failures", end)
		if end == "finish" {
			ended, err = f.db.FinishFoundryReviewAdvice(ctx, "w1", *c, model.FoundryReviewAdviceFailed, 0)
		} else {
			ended, _, err = f.db.BackOffFoundryReviewAdvice(ctx, "w1", *c, model.FoundryReviewAdviceProviderFailed)
		}
		require.NoError(t, err)
		require.True(t, ended, end)
		assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice_loads WHERE load_id=? AND stopped IS NULL AND claimed_by IS NULL
 AND attempts=0 AND failures=0`, f.load), end)
		assert.InDelta(t, 30, *f.adviceSeconds(t), 5, end)
	}
}

// A listing leaves out cards with advice when asked, and joins an advised
// card's advice; a park keeps advice, a clear takes it.
func TestFoundryReviewAdviceListAndClear(t *testing.T) {
	ctx := context.Background()
	f := newCardsFixture(t, "raj")
	f.pass(t)
	f.adviceDue(t)
	c := f.adviceClaim(t, "w1")
	require.NotNil(t, c)
	cards := f.list(t, model.FoundryReviewCardFilter{})
	require.GreaterOrEqual(t, len(cards), 3)
	text := "GBP"
	choice := reviewadvice.Choice{Prop: &reviewadvice.Value{Text: &text}}
	cite := reviewadvice.Cite{DocID: f.a, Section: 0, Start: 0, End: 1}
	stored, err := f.db.StoreFoundryReviewAdvice(ctx, "w1", *c, []model.FoundryReviewAdviceRow{
		{CardID: cards[0].ID, Outcome: model.FoundryReviewAdvised, Offered: []reviewadvice.Choice{choice}, Choice: &choice,
			Rationale: "Two quotes say GBP.", Cites: []reviewadvice.Cite{cite}, Model: "claude", PromptVersion: reviewadvice.PromptVersion},
		{CardID: cards[1].ID, Outcome: model.FoundryReviewAdviceNone, Offered: []reviewadvice.Choice{choice}, Model: "claude",
			PromptVersion: reviewadvice.PromptVersion},
	})
	require.NoError(t, err)
	require.True(t, stored)

	listed := f.list(t, model.FoundryReviewCardFilter{})
	byID := map[reviewcards.ID]*model.FoundryReviewListedCard{}
	for _, l := range listed {
		byID[l.ID] = l
	}
	a := byID[cards[0].ID].Advice
	require.NotNil(t, a, "an advised card's advice")
	assert.Equal(t, "Two quotes say GBP.", a.Rationale)
	assert.JSONEq(t, `{"prop":{"text":"GBP"}}`, string(a.Choice))
	assert.JSONEq(t, `[{"prop":{"text":"GBP"}}]`, string(a.Offered))
	var cites []reviewadvice.Cite
	require.NoError(t, json.Unmarshal(a.Cites, &cites))
	assert.Equal(t, []reviewadvice.Cite{cite}, cites)
	assert.Nil(t, byID[cards[1].ID].Advice, "none is no advice")
	assert.Nil(t, byID[cards[2].ID].Advice)

	unadvised := f.list(t, model.FoundryReviewCardFilter{Unadvised: true})
	assert.Len(t, unadvised, len(listed)-2)
	for _, u := range unadvised {
		assert.NotEqual(t, cards[0].ID, u.ID)
		assert.NotEqual(t, cards[1].ID, u.ID)
	}
	// A page after a cursor still leaves them out.
	first := unadvised[0]
	page, err := f.db.ListFoundryReviewCards(ctx, f.ontology.ID, f.load, model.FoundryReviewCardFilter{Lane: reviewcards.LaneDecide, Unadvised: true},
		&model.FoundryReviewCardCursor{PerDecision: first.PerDecision, Facts: first.Facts, KindOrder: first.Kind.KindOrder(), Shuffle: first.Shuffle,
			ID: first.ID}, 100)
	require.NoError(t, err)
	assert.Len(t, page, len(unadvised)-1)

	// A park (its database briefly gone) keeps paid advice.
	require.NoError(t, f.db.ParkFoundryReviewLoad(ctx, f.load))
	assert.Equal(t, 2, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice WHERE load_id=?`, f.load))
	// A clear (no longer live) takes it, and the load's advice row.
	_, err = testDB.Exec(`UPDATE foundry_ontologies SET current_load_id=NULL WHERE id=?`, f.ontology.ID)
	require.NoError(t, err)
	_, err = f.db.ClearFoundryReviewLoads(ctx, 1000)
	require.NoError(t, err)
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice WHERE load_id=?`, f.load))
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_advice_loads WHERE load_id=?`, f.load))
	stored, err = f.db.StoreFoundryReviewAdvice(ctx, "w1", *c, []model.FoundryReviewAdviceRow{{CardID: cards[2].ID, Outcome: model.FoundryReviewAdviceRules}})
	require.NoError(t, err)
	assert.False(t, stored, "a store after a clear writes nothing")
}

func TestFoundryReviewAdviceSpend(t *testing.T) {
	ctx := context.Background()
	f := newCardsFixture(t, "ras")
	ws := f.ontology.WorkspaceID
	require.NoError(t, f.db.ChargeFoundryReviewAdvice(ctx, ws, 1500))
	require.NoError(t, f.db.ChargeFoundryReviewAdvice(ctx, ws, 2500))
	require.Error(t, f.db.ChargeFoundryReviewAdvice(ctx, ws, -1))
	spent, err := f.db.SumFoundryReviewAdviceSpend(ctx, ws, time.Now())
	require.NoError(t, err)
	assert.EqualValues(t, 4000, spent)
	spent, err = f.db.SumFoundryReviewAdviceSpend(ctx, ws, time.Now().AddDate(0, -2, 0))
	require.NoError(t, err)
	assert.Zero(t, spent, "another month's")
	spent, err = f.db.SumFoundryReviewAdviceSpend(ctx, id26("other-ws"), time.Now())
	require.NoError(t, err)
	assert.Zero(t, spent, "another workspace's")
}
