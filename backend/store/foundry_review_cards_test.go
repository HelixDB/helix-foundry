package store_test

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/reviewcards"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// cardsFixture is a resend fixture (a current load holding documents a and
// b) whose review cards tests build and store.
type cardsFixture struct {
	resendFixture
}

func newCardsFixture(t *testing.T, tag string) cardsFixture {
	t.Helper()
	return cardsFixture{newResendFixture(t, tag)}
}

// statement is a held claim's stored statement of subject and object.
func statement(subjectType, subject, objectType, object string) string {
	return `{"subject":{"type":"` + subjectType + `","name":"` + subject + `"},"object":{"type":"` + objectType + `","name":"` + object + `"},"props":{}}`
}

// heldReview is a low-confidence held claim of doc at start, its key k.
func heldReview(doc, key string, k byte, start int64, stmt string, confidence float64) *reviewcards.Review {
	section, end := int64(0), start+5
	return &reviewcards.Review{Key: key, Reason: "low_confidence", DocID: doc, Statement: stmt, ClaimKey: strings.Repeat(string([]byte{k}), 64),
		Claim: `{"about":[1,2]}`, Section: &section, Start: &start, End: &end, SuggestedLink: "works_at", Confidence: &confidence}
}

// docReviews are a document's reviews: a name linked by part of a name
// (Westmere), a held claim that waits on it, one that doesn't, and a value.
func docReviews(doc, tag string) []*reviewcards.Review {
	zero := int64(0)
	return []*reviewcards.Review{
		{Key: tag + "-name", Reason: "part_name", DocID: doc, Type: "Company", NormName: "westmere", Section: &zero, Start: &zero},
		heldReview(doc, tag+"-waits", 'a', 10, statement("Company", "Westmere", "Person", "Ada"), 0.65),
		heldReview(doc, tag+"-own", 'b', 30, statement("Person", "Bea", "Company", "Acme"), 0.95),
		{Key: tag + "-value", Reason: "prop_conflict", DocID: doc, Type: "Customer", EntityKey: "scus_1", Prop: "currency",
			Values:    `[{"value":"GBP","doc_id":"` + doc + `","quote":"q","span":{"section":0,"start":0,"end":1}}]`,
			Concerned: []reviewcards.Entity{{Type: "Customer", Key: "scus_1"}}},
	}
}

// index claims the load's owed documents as worker and stores each as the
// graph would read it: its docReviews, or gone when present says so.
func (f cardsFixture) index(t *testing.T, worker string, present map[string]bool) {
	t.Helper()
	ctx := context.Background()
	claims, err := f.db.ClaimFoundryReviewDocuments(ctx, worker, time.Minute)
	require.NoError(t, err)
	for _, c := range claims {
		var indexed []model.FoundryReviewIndexed
		for _, doc := range c.DocIDs() {
			there := present == nil || present[doc]
			built := reviewcards.Build(c.LoadID, doc, docReviews(doc, doc[3:5]), nil, func(string) bool { return false })
			indexed = append(indexed, model.FoundryReviewIndexed{DocID: doc, Present: there, Cards: built.Cards, Quotes: [6]uint32{1, 2, 0, 0, 0, 3}})
		}
		stored, err := f.db.StoreFoundryReviewDocuments(ctx, worker, c, indexed)
		require.NoError(t, err)
		require.True(t, stored)
	}
}

func (f cardsFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(query, args...).Scan(&n))
	return n
}

// pass runs the indexer's housekeeping and indexes until nothing is owed.
func (f cardsFixture) pass(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for range 5 {
		_, err := f.db.RequestFoundryReviewPasses(ctx)
		require.NoError(t, err)
		_, err = f.db.AdvanceFoundryReviewPasses(ctx)
		require.NoError(t, err)
		f.index(t, "indexer", nil)
	}
	_, err := f.db.AdvanceFoundryReviewPasses(ctx)
	require.NoError(t, err)
	_, err = f.db.RecountFoundryReviewLoads(ctx, 10)
	require.NoError(t, err)
}

// decide saves a decision row as a settle would, for open's rules alone.
func (f cardsFixture) decide(t *testing.T, id, kind, ty string, doc, norm, entity, prop, value, claimKey any, start any, hash any) {
	t.Helper()
	var section any
	if start != nil {
		section = 0
	}
	var end any
	if s, ok := start.(int); ok {
		end = s + 5
	}
	_, err := testDB.Exec(`INSERT INTO foundry_review_decisions (id, ontology_id, kind, type, doc_id, norm_name, entity_key, prop, value,
 claim_key, claim_section, claim_start, claim_end, statement_hash, decided_by_user_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'u')`,
		id26(id), f.ontology.ID, kind, ty, doc, norm, entity, prop, value, claimKey, section, start, end, hash)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_review_loads SET dirty_at=NOW(6) WHERE ontology_id=?`, f.ontology.ID)
	require.NoError(t, err)
}

func (f cardsFixture) list(t *testing.T, filter model.FoundryReviewCardFilter) []*model.FoundryReviewListedCard {
	t.Helper()
	if filter.Lane == "" {
		filter.Lane = reviewcards.LaneDecide
	}
	cards, err := f.db.ListFoundryReviewCards(context.Background(), f.ontology.ID, f.load, filter, nil, 100)
	require.NoError(t, err)
	return cards
}

func keys(cards []*model.FoundryReviewListedCard) []string {
	var out []string
	for _, c := range cards {
		k := c.ReviewKey
		for _, h := range c.Held {
			k += "+" + h.ReviewKey
		}
		out = append(out, k)
	}
	return out
}

func TestFoundryReviewCardOwing(t *testing.T) {
	ctx := context.Background()
	f := newCardsFixture(t, "rco")

	// Each document the load job recorded is owed, in the record's
	// transaction, and the load got its row and a pass as it became current.
	assert.Equal(t, 2, f.count(t, `SELECT COUNT(*) FROM foundry_review_owed WHERE load_id=?`, f.load))
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_loads WHERE load_id=? AND full_owed_at IS NOT NULL`, f.load))

	// A record whose job's lease is lost writes and owes nothing.
	_, err := testDB.Exec(`DELETE FROM foundry_review_owed`)
	require.NoError(t, err)
	assert.ErrorIs(t, f.db.RecordFoundryDocumentExtraction(ctx, id26("nojob"), f.worker, extracted(f.a), false), store.ErrNotFound)
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_owed`))

	// A job's end that sent an Extract asks for a pass; one that sent none
	// doesn't.
	_, err = testDB.Exec(`UPDATE foundry_review_loads SET full_owed_at=NULL`)
	require.NoError(t, err)
	f.approve(t, "rco-1", f.held(f.a, 1, strings.Repeat("cd", 32)))
	job := f.claim(t)
	_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, f.worker, model.FoundryGraphJobOutcome{Status: model.FoundryGraphJobSucceeded, Next: next("rco-1")})
	require.NoError(t, err)
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_loads WHERE full_owed_at IS NOT NULL`))
	f.approve(t, "rco-2", f.held(f.b, 1, strings.Repeat("cd", 32)))
	job = f.claim(t)
	_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, f.worker, model.FoundryGraphJobOutcome{Status: model.FoundryGraphJobSucceeded, Next: next("rco-2"),
		ReindexReviews: true})
	require.NoError(t, err)
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_loads WHERE load_id=? AND full_owed_at IS NOT NULL`, f.load))
}

func TestFoundryReviewCardClaimAndStore(t *testing.T) {
	ctx := context.Background()
	f := newCardsFixture(t, "rcs")

	// Two workers never claim the same row.
	first, err := f.db.ClaimFoundryReviewDocuments(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.ElementsMatch(t, []string{f.a, f.b}, first[0].DocIDs())
	assert.Equal(t, f.ontology.ID, first[0].OntologyID)
	second, err := f.db.ClaimFoundryReviewDocuments(ctx, "w2", time.Minute)
	require.NoError(t, err)
	assert.Empty(t, second)

	// Another worker's store, or one past the lease, writes nothing.
	built := reviewcards.Build(f.load, f.a, docReviews(f.a, "a"), nil, func(string) bool { return false })
	indexed := []model.FoundryReviewIndexed{{DocID: f.a, Present: true, Cards: built.Cards}, {DocID: f.b, Present: true}}
	stored, err := f.db.StoreFoundryReviewDocuments(ctx, "w2", first[0], indexed)
	require.NoError(t, err)
	assert.False(t, stored)
	_, err = testDB.Exec(`UPDATE foundry_review_owed SET due_at=NOW(6) - INTERVAL 1 SECOND`)
	require.NoError(t, err)
	stored, err = f.db.StoreFoundryReviewDocuments(ctx, "w1", first[0], indexed)
	require.NoError(t, err)
	assert.False(t, stored, "a lapsed lease")
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_cards`))

	// Claimed again, a document owed again during the claim stays owed,
	// due at once; the other is no longer owed.
	claims, err := f.db.ClaimFoundryReviewDocuments(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	_, err = testDB.Exec(`UPDATE foundry_review_owed SET owed_at=NOW(6) + INTERVAL 1 SECOND WHERE doc_id=?`, f.b)
	require.NoError(t, err)
	stored, err = f.db.StoreFoundryReviewDocuments(ctx, "w1", claims[0], indexed)
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, 4, f.count(t, `SELECT COUNT(*) FROM foundry_review_cards WHERE doc_id=?`, f.a))
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_owed WHERE doc_id=?`, f.a))
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_owed WHERE doc_id=? AND claimed_by IS NULL AND due_at <= NOW(6)`, f.b))

	// The same cards again replace, never add; a document gone from the
	// graph loses its cards and its row.
	f.index(t, "w3", map[string]bool{f.a: true, f.b: true})
	_, err = testDB.Exec(`INSERT INTO foundry_review_owed (load_id, doc_id) VALUES (?, ?), (?, ?)`, f.load, f.a, f.load, f.b)
	require.NoError(t, err)
	f.index(t, "w3", map[string]bool{f.a: true, f.b: true})
	assert.Equal(t, 8, f.count(t, `SELECT COUNT(*) FROM foundry_review_cards`))
	assert.Equal(t, 2, f.count(t, `SELECT COUNT(*) FROM foundry_review_docs`))
	_, err = testDB.Exec(`INSERT INTO foundry_review_owed (load_id, doc_id) VALUES (?, ?)`, f.load, f.b)
	require.NoError(t, err)
	f.index(t, "w3", map[string]bool{f.a: true})
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_cards WHERE doc_id=?`, f.b))
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_docs WHERE doc_id=?`, f.b))

	// A document whose read keeps failing backs off, then is set aside, and
	// a load that is set aside is no longer indexing.
	_, err = testDB.Exec(`INSERT INTO foundry_review_owed (load_id, doc_id) VALUES (?, ?)`, f.load, f.b)
	require.NoError(t, err)
	for i := range model.FoundryReviewQuarantine {
		_, err = testDB.Exec(`UPDATE foundry_review_owed SET due_at=NOW(6) WHERE doc_id=?`, f.b)
		require.NoError(t, err)
		claims, err := f.db.ClaimFoundryReviewDocuments(ctx, "w4", time.Minute)
		require.NoError(t, err)
		require.Len(t, claims, 1)
		set, err := f.db.BackOffFoundryReviewDocument(ctx, "w4", f.load, f.b)
		require.NoError(t, err)
		assert.Equal(t, i == model.FoundryReviewQuarantine-1, set, "attempt %d", i+1)
	}
	claims, err = f.db.ClaimFoundryReviewDocuments(ctx, "w4", time.Minute)
	require.NoError(t, err)
	assert.Empty(t, claims, "a document set aside is claimed no more")
	// Owed again, it is claimed again.
	_, err = testDB.Exec(`INSERT INTO foundry_review_owed (load_id, doc_id) VALUES (?, ?)
ON DUPLICATE KEY UPDATE owed_at=NOW(6), due_at=IF(attempts >= 8, NOW(6), GREATEST(due_at, NOW(6))), attempts=0`, f.load, f.b)
	require.NoError(t, err)
	claims, err = f.db.ClaimFoundryReviewDocuments(ctx, "w4", time.Minute)
	require.NoError(t, err)
	assert.Len(t, claims, 1)

	// A load whose database can't be read waits, its documents with it.
	require.NoError(t, f.db.BackOffFoundryReviewLoad(ctx, f.load))
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_owed WHERE doc_id=? AND claimed_by IS NULL AND due_at > NOW(6) + INTERVAL 20 SECOND`, f.b))
	require.NoError(t, f.db.DropFoundryReviewLoad(ctx, f.load))
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_cards`)+f.count(t, `SELECT COUNT(*) FROM foundry_review_owed`)+
		f.count(t, `SELECT COUNT(*) FROM foundry_review_loads`))
}

func TestFoundryReviewCardPasses(t *testing.T) {
	ctx := context.Background()
	f := newCardsFixture(t, "rcp")
	indexing := func() bool {
		t.Helper()
		v, err := f.db.FoundryReviewIndexing(ctx, f.load)
		require.NoError(t, err)
		return v
	}
	// A current load never indexed is indexing, never "nothing to review".
	assert.True(t, indexing())
	summaries, err := f.db.ListFoundryReviewSummaries(ctx, f.ontology.WorkspaceID)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.True(t, summaries[0].Indexing)

	// Its pass starts at once, owes its extracted documents, and ends once
	// they're indexed, at this builder's version.
	f.pass(t)
	assert.False(t, indexing())
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_loads WHERE load_id=? AND version=? AND full_started_at IS NULL
 AND full_owed_at IS NULL AND indexed_at IS NOT NULL`, f.load, reviewcards.Version))
	summaries, err = f.db.ListFoundryReviewSummaries(ctx, f.ontology.WorkspaceID)
	require.NoError(t, err)
	// Per document: the name (its held claim on it), the held claim of its
	// own, the value: 4 open a document in the decide lane.
	assert.Equal(t, model.FoundryReviewSummary{OntologyID: f.ontology.ID, LoadID: f.load, Open: 8}, summaries[0])

	// A request within the hour waits; one for an older builder doesn't.
	_, err = testDB.Exec(`UPDATE foundry_review_loads SET full_owed_at=NOW(6)`)
	require.NoError(t, err)
	started, err := f.db.AdvanceFoundryReviewPasses(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, started)
	_, err = testDB.Exec(`UPDATE foundry_review_loads SET version=0`)
	require.NoError(t, err)
	started, err = f.db.AdvanceFoundryReviewPasses(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, started)
	assert.Equal(t, 2, f.count(t, `SELECT COUNT(*) FROM foundry_review_owed WHERE load_id=?`, f.load))
	assert.True(t, indexing())
	f.pass(t)

	// A load that is no longer live (another became current, this one
	// deleted) is cleared, its spot checks with it once it is deleted.
	_, err = testDB.Exec(`UPDATE foundry_ontologies SET current_load_id=NULL WHERE id=?`, f.ontology.ID)
	require.NoError(t, err)
	_, err = testDB.Exec(`INSERT INTO foundry_review_draws (load_id, seq, kind, band, doc_id, claim_section, claim_start, claim_end, fact_key)
 VALUES (?, 0, 'quote', '0.9', ?, 0, 1, 2, 'fk')`, f.load, f.a)
	require.NoError(t, err)
	cleared, err := f.db.ClearFoundryReviewLoads(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, cleared)
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_cards`)+f.count(t, `SELECT COUNT(*) FROM foundry_review_docs`)+
		f.count(t, `SELECT COUNT(*) FROM foundry_review_loads`))
	assert.Equal(t, 1, f.count(t, `SELECT COUNT(*) FROM foundry_review_draws`), "a load kept keeps its draws")
	_, err = testDB.Exec(`UPDATE foundry_loads SET status='deleted', deleted_at=NOW(6) WHERE id=?`, f.load)
	require.NoError(t, err)
	_, err = f.db.ClearFoundryReviewLoads(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 0, f.count(t, `SELECT COUNT(*) FROM foundry_review_draws`))
}

func TestFoundryReviewCardOpen(t *testing.T) {
	ctx := context.Background()
	f := newCardsFixture(t, "rcd")
	f.pass(t)
	hash := func(stmt string) string { return reviewcards.StatementHash("works_at", stmt) }
	a := f.a[3:5]

	// A held claim waiting on an open name is on its card.
	all := f.list(t, model.FoundryReviewCardFilter{DocumentID: f.a})
	assert.Equal(t, []string{a + "-own", a + "-value", a + "-name+" + a + "-waits"}, keys(all))

	// Names only: the name with its claim. Claims only: both claims.
	assert.Equal(t, []string{a + "-name+" + a + "-waits"},
		keys(f.list(t, model.FoundryReviewCardFilter{DocumentID: f.a, Kinds: []reviewcards.Kind{reviewcards.KindName}})))
	assert.ElementsMatch(t, []string{a + "-own", a + "-waits"},
		keys(f.list(t, model.FoundryReviewCardFilter{DocumentID: f.a, Kinds: []reviewcards.Kind{reviewcards.KindClaim}})))
	// A type matches a card's type or a held claim's ends.
	assert.Equal(t, []string{a + "-own", a + "-name+" + a + "-waits"},
		keys(f.list(t, model.FoundryReviewCardFilter{DocumentID: f.a, Type: "Company"})))

	// The name decided, its held claim is a card of its own.
	f.decide(t, "dn", "resolve", "Company", f.a, "westmere", nil, nil, nil, nil, nil, nil)
	decided := keys(f.list(t, model.FoundryReviewCardFilter{DocumentID: f.a}))
	assert.ElementsMatch(t, []string{a + "-own", a + "-waits"}, decided[:2], "the claims rank alike, by shuffle")
	assert.Equal(t, a+"-value", decided[2])

	// An approval of what a claim said before leaves it open; of what it
	// says now, or a rejection by key, closes it.
	own := statement("Person", "Bea", "Company", "Acme")
	f.decide(t, "do", "release", "", f.a, nil, nil, nil, nil, strings.Repeat("b", 64), 30, hash("old"))
	assert.Len(t, f.list(t, model.FoundryReviewCardFilter{DocumentID: f.a}), 3)
	f.decide(t, "dr", "reject", "", f.a, nil, nil, nil, nil, strings.Repeat("a", 64), 10, hash("other"))
	assert.Equal(t, []string{a + "-own", a + "-value"}, keys(f.list(t, model.FoundryReviewCardFilter{DocumentID: f.a})))
	_, err := testDB.Exec(`DELETE FROM foundry_review_decisions WHERE id=?`, id26("do"))
	require.NoError(t, err)
	f.decide(t, "da", "release", "", f.a, nil, nil, nil, nil, strings.Repeat("b", 64), 30, hash(own))
	// A value decided by its identity, with display columns that aren't.
	f.decide(t, "dv", "prop", "Customer", nil, nil, "scus_1", "currency", `"GBP"`, nil, nil, nil)
	assert.Empty(t, f.list(t, model.FoundryReviewCardFilter{DocumentID: f.a}))
	assert.Len(t, f.list(t, model.FoundryReviewCardFilter{DocumentID: f.b}), 2, "the value's decision closes b's value card too")

	// The count follows on a recount, only of a dirty load.
	n, err := f.db.RecountFoundryReviewLoads(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	n, err = f.db.RecountFoundryReviewLoads(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	summaries, err := f.db.ListFoundryReviewSummaries(ctx, f.ontology.WorkspaceID)
	require.NoError(t, err)
	assert.Equal(t, 3, summaries[0].Open)

	// Pages one card at a time walk the same order as one page.
	whole := f.list(t, model.FoundryReviewCardFilter{})
	var paged []string
	var after *model.FoundryReviewCardCursor
	for {
		page, err := f.db.ListFoundryReviewCards(ctx, f.ontology.ID, f.load, model.FoundryReviewCardFilter{Lane: reviewcards.LaneDecide}, after, 1)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		c := page[0]
		paged = append(paged, c.ReviewKey)
		after = &model.FoundryReviewCardCursor{PerDecision: c.PerDecision, Facts: c.Facts, KindOrder: c.Kind.KindOrder(), Shuffle: c.Shuffle, ID: c.ID}
	}
	var want []string
	for _, c := range whole {
		want = append(want, c.ReviewKey)
	}
	assert.Equal(t, want, paged)

	// The decisions of a name, by its type and name.
	names, err := f.db.ListFoundryReviewNameDecisions(ctx, f.ontology.ID, [][2]string{{"Company", "westmere"}, {"Company", "nobody"}})
	require.NoError(t, err)
	require.Len(t, names, 1)
	assert.Equal(t, 1, names[0].Total)
	assert.Equal(t, 1, names[0].Choices)
	assert.Equal(t, id26("dn"), names[0].Newest[0].ID)
}

func TestFoundryReviewCardDraws(t *testing.T) {
	ctx := context.Background()
	f := newCardsFixture(t, "rcw")
	f.pass(t)

	pools, err := f.db.FoundryReviewDrawPools(ctx, f.load)
	require.NoError(t, err)
	// Each document: the waiting claim (0.6) and its own (0.9); quotes as
	// the read counted them.
	assert.Equal(t, [6]reviewcards.Pool{{Held: 2, Quotes: 2}, {Quotes: 4}, {}, {Held: 2}, {}, {Quotes: 6}}, pools)

	drawn, err := f.db.DrawFoundryHeldClaim(ctx, f.load, 0, 0)
	require.NoError(t, err)
	require.True(t, drawn)
	_, err = f.db.DrawFoundryHeldClaim(ctx, f.load, 0, 0)
	assert.ErrorIs(t, err, store.ErrConflict, "a seq drawn twice")
	pools, err = f.db.FoundryReviewDrawPools(ctx, f.load)
	require.NoError(t, err)
	assert.Equal(t, 1, pools[0].Held)
	draws, err := f.db.ListFoundryReviewDraws(ctx, f.ontology.ID, f.load)
	require.NoError(t, err)
	require.Len(t, draws, 1)
	d := draws[0]
	assert.Equal(t, model.FoundryReviewDrawHeld, d.Kind)
	assert.NotEmpty(t, d.ReviewKey)
	assert.False(t, d.Done)
	// A drawn claim is a spot check, not a card to decide.
	for _, c := range f.list(t, model.FoundryReviewCardFilter{DocumentID: d.DocID}) {
		assert.NotEqual(t, d.ReviewKey, c.ReviewKey)
	}

	// Approved, written and indexed again without its review, the draw is
	// still there at its place, and done once labelled at random.
	_, err = testDB.Exec(`DELETE FROM foundry_review_cards WHERE review_key=?`, d.ReviewKey)
	require.NoError(t, err)
	_, err = testDB.Exec(`INSERT INTO foundry_review_labels (id, ontology_id, load_id, drawn, doc_id, claim_section, claim_start, claim_end,
 claim_key, statement_hash, link, verdict, decided_by_user_id) VALUES (?, ?, ?, 'random', ?, ?, ?, ?, ?, ?, ?, 'right', 'ann')`,
		id26("lab"), f.ontology.ID, f.load, d.DocID, d.Span.Section, d.Span.Start, d.Span.End, d.ClaimKey, d.StatementHash, d.Link)
	require.NoError(t, err)
	draws, err = f.db.ListFoundryReviewDraws(ctx, f.ontology.ID, f.load)
	require.NoError(t, err)
	assert.Empty(t, draws[0].ReviewKey)
	assert.True(t, draws[0].Done)

	// A quote's draw, and where to look for one.
	docs, err := f.db.FoundryReviewQuoteDocuments(ctx, f.load, 5, 4, 5)
	require.NoError(t, err)
	assert.Equal(t, []string{max(f.a, f.b), min(f.a, f.b)}, docs, "the 5th of 6 quotes is in the second document")
	fact := "fk"
	require.NoError(t, f.db.InsertFoundryReviewQuoteDraw(ctx, model.FoundryReviewDraw{LoadID: f.load, Seq: 1, Kind: model.FoundryReviewDrawQuote,
		Band: "none", DocID: f.a, Span: model.FoundryClaimSpan{Start: 1, End: 2}, FactKey: &fact}))
	assert.ErrorIs(t, f.db.InsertFoundryReviewQuoteDraw(ctx, model.FoundryReviewDraw{LoadID: f.load, Seq: 1, Kind: model.FoundryReviewDrawQuote,
		Band: "none", DocID: f.a, Span: model.FoundryClaimSpan{Start: 3, End: 4}, FactKey: &fact}), store.ErrConflict)
	taken, err := f.db.FoundryReviewPlacesTaken(ctx, f.ontology.ID, f.load, f.a)
	require.NoError(t, err)
	assert.True(t, taken[store.FoundryReviewPlace(0, 1, 2, "")])

	// Precision: each place's first random label, by band.
	p, err := f.db.FoundryReviewPrecision(ctx, f.ontology.ID)
	require.NoError(t, err)
	assert.Equal(t, []model.FoundryReviewBandPrecision{{Band: "none", Right: 1, Total: 1}}, p.Bands)

	// Rechecks: someone else's sampled random label, at a place this
	// person hasn't labelled.
	var sampled string
	for i := 0; sampled == ""; i++ {
		if id := id26("rl" + strings.Repeat("x", i%3) + hex.EncodeToString([]byte{byte(i)})); reviewcards.Rechecked(id) {
			sampled = id
		}
	}
	_, err = testDB.Exec(`INSERT INTO foundry_review_labels (id, ontology_id, load_id, drawn, doc_id, claim_section, claim_start, claim_end,
 fact_key, verdict, decided_by_user_id) VALUES (?, ?, ?, 'random', ?, 0, 7, 9, 'fk2', 'wrong', 'bob')`, sampled, f.ontology.ID, f.load, f.b)
	require.NoError(t, err)
	_, labels, err := f.db.ListFoundryReviewRechecks(ctx, f.ontology.ID, "ann", 20)
	require.NoError(t, err)
	require.Len(t, labels, 1)
	assert.Equal(t, sampled, labels[0].ID)
	_, labels, err = f.db.ListFoundryReviewRechecks(ctx, f.ontology.ID, "bob", 20)
	require.NoError(t, err)
	for _, l := range labels {
		assert.NotEqual(t, sampled, l.ID, "never one's own")
	}
}

// Read by id, a decision replaced is missing, and its replacement there
// when asked for.
func TestFoundryReviewCardDecisionsByID(t *testing.T) {
	ctx := context.Background()
	f := newCardsFixture(t, "rci")
	f.decide(t, "one", "resolve", "Company", f.a, "westmere", nil, nil, nil, nil, nil, nil)
	got, err := f.db.ListFoundryReviewDecisionsByID(ctx, f.ontology.ID, []string{id26("one"), id26("two")}, false)
	require.NoError(t, err)
	require.Len(t, got, 1)
	_, err = testDB.Exec(`DELETE FROM foundry_review_decisions WHERE id=?`, id26("one"))
	require.NoError(t, err)
	f.decide(t, "two", "resolve", "Company", f.a, "westmere", "scus_1", nil, nil, nil, nil, nil)
	got, err = f.db.ListFoundryReviewDecisionsByID(ctx, f.ontology.ID, []string{id26("one"), id26("two")}, true)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, id26("two"), got[0].ID)
}
