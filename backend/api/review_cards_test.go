package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/reviewcards"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// cardStore is workspace-a's ontology o, its current load load-1 and that
// load's review cards index.
type cardStore struct {
	foundrystore.Unimplemented
	ontology  *foundrymodel.FoundryOntology
	load      *foundrymodel.FoundryLoad
	summaries []foundrymodel.FoundryReviewSummary
	cards     []*foundrymodel.FoundryReviewListedCard
	// after are the cursors the card lists were asked from.
	after     []*foundrymodel.FoundryReviewCardCursor
	filters   []foundrymodel.FoundryReviewCardFilter
	claims    []*foundrymodel.FoundryReviewDecision
	names     []*foundrymodel.FoundryReviewNameDecisions
	draws     []foundrymodel.FoundryReviewDraw
	pools     [6]reviewcards.Pool
	quoteDocs []string
	byID      []string
	withdraws bool
	// indexed: the load's index is whole (not indexing).
	indexed bool
}

func (s *cardStore) GetFoundryOntology(_ context.Context, ws, id string) (*foundrymodel.FoundryOntology, error) {
	if s.ontology.WorkspaceID != ws || s.ontology.ID != id {
		return nil, foundrystore.ErrNotFound
	}
	return s.ontology, nil
}
func (s *cardStore) GetFoundryLoad(_ context.Context, _, id string) (*foundrymodel.FoundryLoad, error) {
	if s.load == nil || s.load.ID != id {
		return nil, foundrystore.ErrNotFound
	}
	return s.load, nil
}
func (s *cardStore) ListFoundryReviewSummaries(_ context.Context, ws string) ([]foundrymodel.FoundryReviewSummary, error) {
	if ws != s.ontology.WorkspaceID {
		return nil, nil
	}
	return s.summaries, nil
}
func (s *cardStore) FoundryReviewIndexing(context.Context, string) (bool, error) {
	return !s.indexed, nil
}
func (s *cardStore) ListFoundryReviewCards(_ context.Context, _, _ string, filter foundrymodel.FoundryReviewCardFilter,
	after *foundrymodel.FoundryReviewCardCursor, limit int) ([]*foundrymodel.FoundryReviewListedCard, error) {
	s.after, s.filters = append(s.after, after), append(s.filters, filter)
	start := 0
	if after != nil {
		for i, c := range s.cards {
			if c.ID == after.ID {
				start = i + 1
			}
		}
	}
	return s.cards[start:min(len(s.cards), start+limit)], nil
}
func (s *cardStore) ListFoundryClaimDecisions(context.Context, string, []string) ([]*foundrymodel.FoundryReviewDecision, error) {
	return s.claims, nil
}
func (s *cardStore) ListFoundryReviewNameDecisions(_ context.Context, _ string, names [][2]string) ([]*foundrymodel.FoundryReviewNameDecisions, error) {
	return s.names, nil
}
func (s *cardStore) ListFoundryDecisionsApplied(context.Context, string, []string) (map[string]foundrymodel.FoundryDecisionApplied, error) {
	return nil, nil
}
func (s *cardStore) FoundryReviewPrecision(context.Context, string) (foundrymodel.FoundryReviewPrecision, error) {
	return foundrymodel.FoundryReviewPrecision{Bands: []foundrymodel.FoundryReviewBandPrecision{{Band: "0.9", Right: 3, Total: 4}}}, nil
}
func (s *cardStore) ListFoundryReviewDraws(context.Context, string, string) ([]foundrymodel.FoundryReviewDraw, error) {
	return s.draws, nil
}
func (s *cardStore) ListFoundryReviewLabelsAt(context.Context, string, []foundrymodel.FoundryReviewDraw) ([]*foundrymodel.FoundryReviewLabel, error) {
	return nil, nil
}
func (s *cardStore) ListFoundryReviewRechecks(context.Context, string, string, int) ([]*foundrymodel.FoundryReviewDecision, []*foundrymodel.FoundryReviewLabel, error) {
	return nil, nil, nil
}
func (s *cardStore) FoundryReviewDrawPools(context.Context, string) ([6]reviewcards.Pool, error) {
	return s.pools, nil
}
func (s *cardStore) DrawFoundryHeldClaim(_ context.Context, load string, seq, band int) (bool, error) {
	if s.pools[band].Held == 0 {
		return false, nil
	}
	s.pools[band].Held--
	key, hash := strings.Repeat("a", 64), strings.Repeat("b", 64)
	s.draws = append(s.draws, foundrymodel.FoundryReviewDraw{LoadID: load, Seq: seq, Kind: foundrymodel.FoundryReviewDrawHeld, Band: reviewcards.Bands[band],
		DocID: "doc-1", Span: foundrymodel.FoundryClaimSpan{Start: uint32(seq), End: uint32(seq) + 1}, ClaimKey: &key, StatementHash: &hash})
	return true, nil
}
func (s *cardStore) FoundryReviewQuoteDocuments(context.Context, string, int, int, int) ([]string, error) {
	return s.quoteDocs, nil
}
func (s *cardStore) FoundryReviewPlacesTaken(context.Context, string, string, string) (map[string]bool, error) {
	taken := map[string]bool{}
	for _, d := range s.draws {
		taken[foundrystore.FoundryReviewPlace(d.Span.Section, d.Span.Start, d.Span.End, linkOf(d.Link))] = true
	}
	return taken, nil
}
func (s *cardStore) InsertFoundryReviewQuoteDraw(_ context.Context, d foundrymodel.FoundryReviewDraw) error {
	s.pools[slicesIndex(d.Band)].Quotes--
	s.draws = append(s.draws, d)
	return nil
}
func (s *cardStore) ListFoundryReviewDecisionsByID(_ context.Context, _ string, ids []string, withdraws bool) ([]*foundrymodel.FoundryReviewDecision, error) {
	s.byID, s.withdraws = ids, withdraws
	return nil, nil
}

func slicesIndex(band string) int {
	for i, b := range reviewcards.Bands {
		if b == band {
			return i
		}
	}
	return -1
}

func cardService(t *testing.T, graph graphFunc) (*Service, *cardStore) {
	t.Helper()
	current := "load-1"
	db := &cardStore{
		ontology: &foundrymodel.FoundryOntology{ID: "o", WorkspaceID: "workspace-a", TenantID: "tenant-a", CurrentLoadID: &current},
		load:     &foundrymodel.FoundryLoad{ID: current, VersionID: "v", Status: foundrymodel.FoundryLoadReady},
	}
	s := &Service{deps: &runtimeDeps{DB: db}}
	if graph != nil {
		s.deps.Graph = graph
	}
	return s, db
}

func reviewerContext() context.Context { return ports.WithActor(context.Background(), reviewer) }

func TestGetFoundryReviewSummary(t *testing.T) {
	s, db := cardService(t, nil)
	db.summaries = []foundrymodel.FoundryReviewSummary{{OntologyID: "o", LoadID: "load-1", Open: 1284, More: false, Indexing: true}}
	resp, err := s.GetFoundryReviewSummary(reviewerContext(), &foundryv1.GetFoundryReviewSummaryRequest{WorkspaceId: "workspace-a"})
	require.NoError(t, err)
	require.Len(t, resp.GetOntologies(), 1)
	assert.Equal(t, int32(1284), resp.GetOntologies()[0].GetOpen())
	assert.True(t, resp.GetOntologies()[0].GetIndexing())

	// A restricted member reads no cards, so no count: an empty list, never
	// a refusal on every page.
	grantee := access.WithCaller(reviewerContext(), access.Grantee{WorkspaceID: "workspace-a", Principal: agentKey})
	resp, err = s.GetFoundryReviewSummary(grantee, &foundryv1.GetFoundryReviewSummaryRequest{WorkspaceId: "workspace-a"})
	require.NoError(t, err)
	assert.Empty(t, resp.GetOntologies())

	_, err = s.GetFoundryReviewSummary(reviewerContext(), &foundryv1.GetFoundryReviewSummaryRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

// card is a listed card of doc-1: a name with a held claim on it, or one
// of its own.
func card(i int, kind reviewcards.Kind) *foundrymodel.FoundryReviewListedCard {
	ty, norm, band := "Customer", fmt.Sprintf("name-%d", i), "0.9"
	section, start, end, claimKey := uint32(0), uint32(i), uint32(i+1), strings.Repeat("a", 64)
	c := &reviewcards.Card{Kind: kind, Lane: reviewcards.LaneDecide, DocID: "doc-1", ReviewKey: fmt.Sprintf("r-%d", i), Facts: 1, PerDecision: 1}
	c.ID[0], c.ID[1] = byte(i), byte(i>>8)
	switch kind {
	case reviewcards.KindName:
		c.Type, c.NormName, c.ReviewKeys, c.ReviewCount = &ty, &norm, []string{c.ReviewKey}, 1
	case reviewcards.KindClaim:
		c.Band, c.ClaimKey, c.ClaimSection, c.ClaimStart, c.ClaimEnd = &band, &claimKey, &section, &start, &end
	}
	return &foundrymodel.FoundryReviewListedCard{Card: c}
}

func TestListFoundryReviewCards(t *testing.T) {
	s, db := cardService(t, nil)
	for i := range 60 {
		db.cards = append(db.cards, card(i, []reviewcards.Kind{reviewcards.KindName, reviewcards.KindClaim}[i%2]))
	}
	held := card(1000, reviewcards.KindClaim)
	db.cards[0].Held = []*reviewcards.Card{held.Card}
	docID, key, hash := "doc-1", strings.Repeat("a", 64), strings.Repeat("c", 64)
	db.claims = []*foundrymodel.FoundryReviewDecision{{ID: "old-approval", Kind: foundrymodel.FoundryReviewRelease, DocID: &docID, ClaimKey: &key,
		StatementHash: &hash, ClaimSpan: &foundrymodel.FoundryClaimSpan{Start: 1000, End: 1001}}}
	norm := "name-0"
	db.names = []*foundrymodel.FoundryReviewNameDecisions{{Type: "Customer", NormName: norm, Total: 3, Choices: 1,
		Newest: []*foundrymodel.FoundryReviewDecision{{ID: "dn", Kind: foundrymodel.FoundryReviewResolve, Type: "Customer", DocID: &docID, NormName: &norm}}}}

	req := &foundryv1.ListFoundryReviewCardsRequest{WorkspaceId: "workspace-a", OntologyId: "o",
		Kind: []foundryv1.FoundryReviewCardKind{foundryv1.FoundryReviewCardKind_FOUNDRY_REVIEW_CARD_KIND_NAME}, Type: "Customer"}
	first, err := s.ListFoundryReviewCards(reviewerContext(), req)
	require.NoError(t, err)
	assert.Equal(t, "load-1", first.GetLoadId())
	assert.True(t, first.GetIndexing())
	require.Len(t, first.GetCards(), reviewCardPage)
	name := first.GetCards()[0].GetName()
	require.NotNil(t, name)
	assert.Equal(t, "name-0", name.GetNormName())
	require.Len(t, name.GetHeld(), 1)
	assert.Equal(t, "r-1000", name.GetHeld()[0].GetReviewKey())
	require.Len(t, name.GetHeld()[0].GetDecisions(), 1, "an approval of what the claim said before rides with it")
	assert.Equal(t, "0.9", first.GetCards()[1].GetClaim().GetBand())
	require.Len(t, first.GetNames(), 1)
	assert.Equal(t, int32(3), first.GetNames()[0].GetTotal())
	require.NotNil(t, first.GetPrecision())
	assert.Equal(t, []foundrymodel.FoundryReviewCardFilter{{Lane: reviewcards.LaneDecide, Kinds: []reviewcards.Kind{reviewcards.KindName}, Type: "Customer"}},
		db.filters)
	require.NotEmpty(t, first.GetNextPageToken())
	assert.True(t, strings.HasPrefix(first.GetNextPageToken(), "load-1:"))

	req.PageToken = first.GetNextPageToken()
	second, err := s.ListFoundryReviewCards(reviewerContext(), req)
	require.NoError(t, err)
	assert.Len(t, second.GetCards(), 10)
	assert.Empty(t, second.GetNextPageToken())
	assert.Nil(t, second.GetPrecision(), "precision comes with the first page only")
	assert.Equal(t, db.cards[49].ID, db.after[1].ID)

	// A page token of a load no longer current.
	other := "load-2"
	db.ontology.CurrentLoadID, db.load.ID = &other, other
	_, err = s.ListFoundryReviewCards(reviewerContext(), req)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	for name, bad := range map[string]*foundryv1.ListFoundryReviewCardsRequest{
		"a lane it doesn't know": {WorkspaceId: "workspace-a", OntologyId: "o", Lane: 9},
		"a kind it doesn't know": {WorkspaceId: "workspace-a", OntologyId: "o", Kind: []foundryv1.FoundryReviewCardKind{0}},
		"a token it never gave":  {WorkspaceId: "workspace-a", OntologyId: "o", PageToken: "load-2:nope"},
		"a type too long":        {WorkspaceId: "workspace-a", OntologyId: "o", Type: strings.Repeat("t", 121)},
	} {
		_, err := s.ListFoundryReviewCards(reviewerContext(), bad)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), name)
	}
	db.ontology.CurrentLoadID = nil
	_, err = s.ListFoundryReviewCards(reviewerContext(), &foundryv1.ListFoundryReviewCardsRequest{WorkspaceId: "workspace-a", OntologyId: "o"})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "no current load")
}

func TestDrawFoundryReviewSpotChecks(t *testing.T) {
	reads := 0
	graph := graphFunc(func(_ context.Context, tenant string, query []byte) (int, []byte, error) {
		reads++
		require.Equal(t, "tenant-a", tenant)
		return 200, []byte(`{"sections":[{"$id":1,"ordinal":0}],"quoted_in":[{"$from":10,"$to":1},{"$from":11,"$to":1},{"$from":12,"$to":1}],
"quotes":[{"$id":10,"fact_key":"f1","link":"works_at","start":0,"end":4,"confidence":0.95},
{"$id":11,"fact_key":"f2","link":"works_at","start":5,"end":9,"confidence":0.97},
{"$id":12,"fact_key":"f3","link":"works_at","start":10,"end":14,"confidence":0.5}]}`), nil
	})
	s, db := cardService(t, graph)
	db.pools = [6]reviewcards.Pool{0: {Held: 3, Quotes: 2}}
	db.quoteDocs = []string{"doc-q"}

	_, err := s.DrawFoundryReviewSpotChecks(reviewerContext(), &foundryv1.DrawFoundryReviewSpotChecksRequest{WorkspaceId: "workspace-a", OntologyId: "o",
		LoadId: "load-0"})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "a load not current")

	// While the load's index isn't whole, nothing is drawn: draws are for
	// good, and the pool is only the documents indexed so far.
	early, err := s.DrawFoundryReviewSpotChecks(reviewerContext(), &foundryv1.DrawFoundryReviewSpotChecksRequest{WorkspaceId: "workspace-a",
		OntologyId: "o", LoadId: "load-1"})
	require.NoError(t, err)
	assert.Empty(t, early.GetDraws())
	assert.Zero(t, reads)
	db.indexed = true

	resp, err := s.DrawFoundryReviewSpotChecks(reviewerContext(), &foundryv1.DrawFoundryReviewSpotChecksRequest{WorkspaceId: "workspace-a",
		OntologyId: "o", LoadId: "load-1"})
	require.NoError(t, err)
	// Three held claims and both 0.9 quotes there are: five, from 0.9 alone.
	require.Len(t, resp.GetDraws(), reviewcards.SpotChecks)
	var quotes []string
	for i, d := range resp.GetDraws() {
		assert.Equal(t, int64(i), d.GetSeq())
		assert.Equal(t, "0.9", d.GetBand())
		if q := d.GetQuote(); q != nil {
			quotes = append(quotes, q.GetFactKey())
		}
	}
	assert.ElementsMatch(t, []string{"f1", "f2"}, quotes, "each quote once, the one of another band never")
	assert.Positive(t, reads)

	// With five undone, a draw draws nothing.
	again, err := s.DrawFoundryReviewSpotChecks(reviewerContext(), &foundryv1.DrawFoundryReviewSpotChecksRequest{WorkspaceId: "workspace-a",
		OntologyId: "o", LoadId: "load-1"})
	require.NoError(t, err)
	assert.Len(t, again.GetDraws(), reviewcards.SpotChecks)
	// The read draws nothing either.
	listed, err := s.ListFoundryReviewSpotChecks(reviewerContext(), &foundryv1.ListFoundryReviewSpotChecksRequest{WorkspaceId: "workspace-a", OntologyId: "o"})
	require.NoError(t, err)
	assert.Len(t, listed.GetDraws(), reviewcards.SpotChecks)
}

func TestListFoundryReviewDecisionsByID(t *testing.T) {
	s, db := cardService(t, nil)
	_, err := s.ListFoundryReviewDecisions(reviewerContext(), &foundryv1.ListFoundryReviewDecisionsRequest{WorkspaceId: "workspace-a", OntologyId: "o",
		Ids: []string{"d1", "d2"}, IncludeWithdraws: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"d1", "d2"}, db.byID)
	assert.True(t, db.withdraws)

	many := make([]string, maxDecisionIDs+1)
	for i := range many {
		many[i] = fmt.Sprint(i)
	}
	for name, req := range map[string]*foundryv1.ListFoundryReviewDecisionsRequest{
		"over 100 ids":        {WorkspaceId: "workspace-a", OntologyId: "o", Ids: many},
		"ids with a page":     {WorkspaceId: "workspace-a", OntologyId: "o", Ids: []string{"d1"}, PageToken: "d0"},
		"an id that can't be": {WorkspaceId: "workspace-a", OntologyId: "o", Ids: []string{strings.Repeat("x", 27)}},
	} {
		_, err := s.ListFoundryReviewDecisions(reviewerContext(), req)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), name)
	}
}

// An advised name or value card carries its model advice and the choices it
// was offered; advice that doesn't read, or on a held claim, is left out. A
// grantee never reads cards, so never advice.
func TestListFoundryReviewCardsModelAdvice(t *testing.T) {
	s, db := cardService(t, nil)
	name, value, broken, held := card(0, reviewcards.KindName), card(1, reviewcards.KindValue), card(2, reviewcards.KindName), card(3, reviewcards.KindClaim)
	name.Advice = &foundrymodel.FoundryReviewAdvice{Choice: []byte(`{"resolve":{"entityKey":"scus_2"}}`),
		Offered:   []byte(`[{"resolve":{"entityKey":"scus_1"}},{"resolve":{"entityKey":"scus_2"}},{"resolve":{"notInData":true}}]`),
		Rationale: "The quote names the London office.", Cites: []byte(`[{"docId":"doc-1","section":2,"start":10,"end":40}]`)}
	value.Advice = &foundrymodel.FoundryReviewAdvice{Choice: []byte(`{"prop":{"integer":"9007199254740993"}}`),
		Offered: []byte(`[{"prop":{"integer":"9007199254740993"}},{"prop":{"number":12.5}}]`), Rationale: "Two quotes.",
		Cites: []byte(`[{"docId":"doc-1","section":0,"start":0,"end":5}]`)}
	broken.Advice = &foundrymodel.FoundryReviewAdvice{Choice: []byte(`{"resolve":{"entity":"x"}}`), Offered: []byte(`[]`), Rationale: "x",
		Cites: []byte(`[]`)}
	held.Advice = value.Advice
	db.cards = []*foundrymodel.FoundryReviewListedCard{name, value, broken, held}

	out, err := s.ListFoundryReviewCards(reviewerContext(), &foundryv1.ListFoundryReviewCardsRequest{WorkspaceId: "workspace-a", OntologyId: "o"})
	require.NoError(t, err)
	require.Len(t, out.GetCards(), 4)
	a := out.GetCards()[0].GetModelAdvice()
	require.NotNil(t, a)
	assert.Equal(t, "scus_2", a.GetChoice().GetResolve().GetEntityKey())
	require.Len(t, a.GetOffered(), 3)
	assert.True(t, a.GetOffered()[2].GetResolve().GetNotInData())
	assert.Equal(t, "The quote names the London office.", a.GetRationale())
	require.Len(t, a.GetCites(), 1)
	assert.Equal(t, "doc-1", a.GetCites()[0].GetDocId())
	assert.Equal(t, int64(2), a.GetCites()[0].GetSpan().GetSection())
	assert.Equal(t, int64(40), a.GetCites()[0].GetSpan().GetEnd())
	v := out.GetCards()[1].GetModelAdvice()
	require.NotNil(t, v)
	assert.Equal(t, int64(9007199254740993), v.GetChoice().GetProp().GetInteger(), "to the digit")
	assert.Equal(t, 12.5, v.GetOffered()[1].GetProp().GetNumber())
	assert.Nil(t, out.GetCards()[2].GetModelAdvice(), "advice that doesn't read")
	assert.Nil(t, out.GetCards()[3].GetModelAdvice(), "a held claim is never advised")

	assert.False(t, access.GranteeAdmits(foundryv1.FoundryService_ListFoundryReviewCards_FullMethodName), "a grantee reads no cards")
}
