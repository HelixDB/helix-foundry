package api

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/reviewcards"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// reviewCardPage is how many cards one page of the walk lists.
	reviewCardPage = 50
	// reviewRechecks is how many decisions and labels a spot-check read
	// offers to re-check, of each.
	reviewRechecks = 20
	// maxDecisionIDs is how many decisions one read by id names.
	maxDecisionIDs = 100
	// reviewDrawTries bounds the draws one request makes, losers of a race
	// and empty picks included.
	reviewDrawTries = 30
	// reviewQuoteDocuments is how many documents a quote's draw tries.
	reviewQuoteDocuments = 5
)

// GetFoundryReviewSummary is what each live ontology's current load asks a
// person to decide, read from the review cards index alone, so the badge,
// the nudge and the job dock poll it cheaply. A restricted member, who may
// read none of the cards, gets none.
func (s *Service) GetFoundryReviewSummary(ctx context.Context, req *foundryv1.GetFoundryReviewSummaryRequest) (*foundryv1.GetFoundryReviewSummaryResponse, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	out := &foundryv1.GetFoundryReviewSummaryResponse{}
	if _, ok := access.GranteeFrom(ctx); ok {
		return out, nil
	}
	summaries, err := s.deps.DB.ListFoundryReviewSummaries(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the review summary: %v", err)
	}
	for _, r := range summaries {
		out.Ontologies = append(out.Ontologies, &foundryv1.FoundryReviewSummary{OntologyId: r.OntologyID, LoadId: r.LoadID, Open: int32(r.Open),
			More: r.More, Indexing: r.Indexing})
	}
	return out, nil
}

// currentReviewLoad is the ontology's current load whose cards a walk
// reads: FAILED_PRECONDITION without one, or while it can't be read.
func (s *Service) currentReviewLoad(ctx context.Context, o *foundrymodel.FoundryOntology) (*foundrymodel.FoundryLoad, error) {
	if o.CurrentLoadID == nil {
		return nil, status.Error(codes.FailedPrecondition, "load the ontology before reading its reviews")
	}
	load, err := s.deps.DB.GetFoundryLoad(ctx, o.ID, *o.CurrentLoadID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "load not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "get load: %v", err)
	case !load.Status.Servable():
		return nil, status.Error(codes.FailedPrecondition, "the load is not ready")
	}
	return load, nil
}

// reviewCardToken is a page token's cursor: the last card's walk keys.
type reviewCardToken struct {
	PerDecision float64 `json:"p"`
	Facts       int     `json:"f"`
	KindOrder   int     `json:"k"`
	Shuffle     uint32  `json:"s"`
	ID          string  `json:"i"`
}

// ListFoundryReviewCards lists a page of the current load's open cards in
// the walk's order, with the decisions of the names its name cards are of
// and, on the first page, the spot checks' precision. A page token is
// <load id>:<the last card's walk keys>, of the load still current.
func (s *Service) ListFoundryReviewCards(ctx context.Context, req *foundryv1.ListFoundryReviewCardsRequest) (*foundryv1.ListFoundryReviewCardsResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	filter := foundrymodel.FoundryReviewCardFilter{Lane: reviewcards.LaneDecide, Type: req.GetType(), DocumentID: req.GetDocumentId()}
	switch req.GetLane() {
	case foundryv1.FoundryReviewLane_FOUNDRY_REVIEW_LANE_UNSPECIFIED, foundryv1.FoundryReviewLane_FOUNDRY_REVIEW_LANE_DECIDE:
	case foundryv1.FoundryReviewLane_FOUNDRY_REVIEW_LANE_FYI:
		filter.Lane = reviewcards.LaneFYI
	default:
		return nil, status.Error(codes.InvalidArgument, "lane is DECIDE or FYI")
	}
	for _, k := range req.GetKind() {
		kind, ok := map[foundryv1.FoundryReviewCardKind]reviewcards.Kind{
			foundryv1.FoundryReviewCardKind_FOUNDRY_REVIEW_CARD_KIND_NAME:  reviewcards.KindName,
			foundryv1.FoundryReviewCardKind_FOUNDRY_REVIEW_CARD_KIND_CLAIM: reviewcards.KindClaim,
			foundryv1.FoundryReviewCardKind_FOUNDRY_REVIEW_CARD_KIND_VALUE: reviewcards.KindValue,
		}[k]
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "kind is NAME, CLAIM or VALUE")
		}
		if !slices.Contains(filter.Kinds, kind) {
			filter.Kinds = append(filter.Kinds, kind)
		}
	}
	if utf8.RuneCountInString(filter.Type) > 120 || len(filter.DocumentID) > 26 {
		return nil, status.Error(codes.InvalidArgument, "type is at most 120 characters and document_id 26")
	}
	tokenLoad, after, err := parseReviewCardToken(req.GetPageToken())
	if err != nil {
		return nil, err
	}
	load, err := s.currentReviewLoad(ctx, o)
	if err != nil {
		return nil, err
	}
	if tokenLoad != "" && tokenLoad != load.ID {
		return nil, status.Error(codes.FailedPrecondition, "another load became current since the first page: list the cards again")
	}
	indexing, err := s.deps.DB.FoundryReviewIndexing(ctx, load.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the review cards: %v", err)
	}
	cards, err := s.deps.DB.ListFoundryReviewCards(ctx, o.ID, load.ID, filter, after, reviewCardPage+1)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list review cards: %v", err)
	}
	out := &foundryv1.ListFoundryReviewCardsResponse{LoadId: load.ID, Indexing: indexing}
	if len(cards) > reviewCardPage {
		cards = cards[:reviewCardPage]
		last := cards[len(cards)-1]
		raw, _ := json.Marshal(reviewCardToken{PerDecision: last.PerDecision, Facts: last.Facts, KindOrder: last.Kind.KindOrder(), Shuffle: last.Shuffle,
			ID: hex.EncodeToString(last.ID[:])})
		out.NextPageToken = load.ID + ":" + base64.RawURLEncoding.EncodeToString(raw)
	}
	// The decisions at each held claim's place, and of each name.
	var docs []string
	var names [][2]string
	for _, c := range cards {
		docs = append(docs, c.DocID)
		if c.Kind == reviewcards.KindName && !slices.Contains(names, [2]string{*c.Type, *c.NormName}) {
			names = append(names, [2]string{*c.Type, *c.NormName})
		}
	}
	slices.Sort(docs)
	claimDecisions, err := s.deps.DB.ListFoundryClaimDecisions(ctx, o.ID, slices.Compact(docs))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list review cards: %v", err)
	}
	nameDecisions, err := s.deps.DB.ListFoundryReviewNameDecisions(ctx, o.ID, names)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list review cards: %v", err)
	}
	ids := make([]string, 0, len(claimDecisions))
	for _, d := range claimDecisions {
		ids = append(ids, d.ID)
	}
	for _, n := range nameDecisions {
		for _, d := range n.Newest {
			ids = append(ids, d.ID)
		}
	}
	applied, err := s.deps.DB.ListFoundryDecisionsApplied(ctx, o.ID, ids)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list review cards: %v", err)
	}
	decision := func(d *foundrymodel.FoundryReviewDecision) *foundryv1.FoundryReviewDecision {
		if a, ok := applied[d.ID]; ok {
			d.Applied = &a
		}
		return foundryReviewDecisionProto(d)
	}
	claim := func(c *reviewcards.Card) *foundryv1.FoundryReviewClaimCard {
		out := &foundryv1.FoundryReviewClaimCard{ReviewKey: c.ReviewKey, Band: *c.Band}
		for _, d := range claimDecisions {
			if s := d.ClaimSpan; *d.DocID == c.DocID && s.Section == *c.ClaimSection && s.Start == *c.ClaimStart && s.End == *c.ClaimEnd {
				out.Decisions = append(out.Decisions, decision(d))
			}
		}
		return out
	}
	for _, c := range cards {
		card := &foundryv1.FoundryReviewCard{CardId: hex.EncodeToString(c.ID[:]), Lane: reviewLaneProto(c.Lane), DocId: c.DocID, Facts: int32(c.Facts)}
		switch c.Kind {
		case reviewcards.KindName:
			name := &foundryv1.FoundryReviewNameCard{ReviewKey: c.ReviewKey, ReviewKeys: c.ReviewKeys, ReviewCount: int32(c.ReviewCount),
				Blocked: int32(c.Blocked), Type: *c.Type, NormName: *c.NormName}
			for _, h := range c.Held {
				name.Held = append(name.Held, claim(h))
			}
			card.Card = &foundryv1.FoundryReviewCard_Name{Name: name}
		case reviewcards.KindClaim:
			card.Card = &foundryv1.FoundryReviewCard_Claim{Claim: claim(c.Card)}
		case reviewcards.KindValue:
			card.Card = &foundryv1.FoundryReviewCard_Value{Value: &foundryv1.FoundryReviewValueCard{ReviewKey: c.ReviewKey}}
		}
		out.Cards = append(out.Cards, card)
	}
	for _, n := range nameDecisions {
		pb := &foundryv1.FoundryReviewNameDecisions{Type: n.Type, NormName: n.NormName, Total: int32(n.Total), Choices: int32(n.Choices)}
		for _, d := range n.Newest {
			pb.Newest = append(pb.Newest, decision(d))
		}
		out.Names = append(out.Names, pb)
	}
	if req.GetPageToken() == "" {
		p, err := s.deps.DB.FoundryReviewPrecision(ctx, o.ID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read the spot checks' precision: %v", err)
		}
		out.Precision = &foundryv1.FoundryReviewPrecision{RechecksAgreed: int32(p.RechecksAgreed), RechecksTotal: int32(p.RechecksTotal)}
		for _, b := range p.Bands {
			out.Precision.Bands = append(out.Precision.Bands, &foundryv1.FoundryReviewPrecision_Band{Band: b.Band, Right: int32(b.Right), Total: int32(b.Total)})
		}
	}
	return out, nil
}

// parseReviewCardToken reads a page token: its load and where the page
// starts. An empty token is the first page.
func parseReviewCardToken(token string) (string, *foundrymodel.FoundryReviewCardCursor, error) {
	if token == "" {
		return "", nil, nil
	}
	invalid := status.Error(codes.InvalidArgument, "page_token is not one this list returned")
	load, rest, ok := strings.Cut(token, ":")
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	var t reviewCardToken
	if !ok || load == "" || len(load) > 26 || err != nil || json.Unmarshal(raw, &t) != nil {
		return "", nil, invalid
	}
	id, err := hex.DecodeString(t.ID)
	if err != nil || len(id) != len(reviewcards.ID{}) {
		return "", nil, invalid
	}
	c := &foundrymodel.FoundryReviewCardCursor{PerDecision: t.PerDecision, Facts: t.Facts, KindOrder: t.KindOrder, Shuffle: t.Shuffle}
	copy(c.ID[:], id)
	return load, c, nil
}

func reviewLaneProto(l reviewcards.Lane) foundryv1.FoundryReviewLane {
	if l == reviewcards.LaneFYI {
		return foundryv1.FoundryReviewLane_FOUNDRY_REVIEW_LANE_FYI
	}
	return foundryv1.FoundryReviewLane_FOUNDRY_REVIEW_LANE_DECIDE
}

// ListFoundryReviewSpotChecks reads the current load's spot checks, as
// drawn, with what the caller may re-check and the decisions and labels at
// the drawn places. It draws nothing (DrawFoundryReviewSpotChecks does).
func (s *Service) ListFoundryReviewSpotChecks(ctx context.Context, req *foundryv1.ListFoundryReviewSpotChecksRequest) (*foundryv1.FoundryReviewSpotChecksResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	load, err := s.currentReviewLoad(ctx, o)
	if err != nil {
		return nil, err
	}
	return s.spotChecks(ctx, o, load.ID)
}

// DrawFoundryReviewSpotChecks tops load_id's spot checks up to
// reviewcards.SpotChecks not yet labelled, then reads them as
// ListFoundryReviewSpotChecks does. Each draw n takes a band in turn
// (reviewcards.Next) and, in proportion, a held claim (the band's first by
// shuffle at a free place) or a fact's quote (a document drawn by its band's
// quotes, read from the load, its first free quote by the load's order).
// Two requests at once race on n: the loser reads again. Idempotent: with
// enough undone draws it draws nothing.
func (s *Service) DrawFoundryReviewSpotChecks(ctx context.Context, req *foundryv1.DrawFoundryReviewSpotChecksRequest) (*foundryv1.FoundryReviewSpotChecksResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	load, err := s.currentReviewLoad(ctx, o)
	if err != nil {
		return nil, err
	}
	if req.GetLoadId() != load.ID {
		return nil, status.Error(codes.FailedPrecondition, "load_id is not the current load: read the spot checks again")
	}
	// The bands whose held claims or quotes this request found none of,
	// whatever the counts say.
	var noHeld, noQuotes [6]bool
	for range reviewDrawTries {
		draws, err := s.deps.DB.ListFoundryReviewDraws(ctx, o.ID, load.ID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read the spot checks: %v", err)
		}
		undone, seq := 0, 0
		for _, d := range draws {
			if !d.Done {
				undone++
			}
			seq = max(seq, d.Seq+1)
		}
		if undone >= reviewcards.SpotChecks {
			break
		}
		pools, err := s.deps.DB.FoundryReviewDrawPools(ctx, load.ID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read the spot checks' pools: %v", err)
		}
		for b := range pools {
			if noHeld[b] {
				pools[b].Held = 0
			}
			if noQuotes[b] || s.deps.Graph == nil {
				pools[b].Quotes = 0
			}
		}
		next, ok := reviewcards.Next(load.ID, seq, pools)
		if !ok {
			break
		}
		if !next.Quote {
			drawn, err := s.deps.DB.DrawFoundryHeldClaim(ctx, load.ID, seq, next.Band)
			switch {
			case errors.Is(err, foundrystore.ErrConflict):
			case err != nil:
				return nil, status.Errorf(codes.Internal, "draw a spot check: %v", err)
			case !drawn:
				noHeld[next.Band] = true
			}
			continue
		}
		drawn, err := s.drawQuote(ctx, o, load.ID, seq, next.Band, pools[next.Band].Quotes)
		switch {
		case errors.Is(err, foundrystore.ErrConflict):
		case err != nil:
			return nil, err
		case !drawn:
			noQuotes[next.Band] = true
		}
	}
	return s.spotChecks(ctx, o, load.ID)
}

// drawQuote draws spot check seq of load loadID from band's quotes of facts:
// the documents from the one holding the band's target quote, each read
// from the load, its first quote of the band (by the load's order) at a
// place no draw or random label holds. drawn false means none of those
// documents had one; ErrConflict that seq was drawn by someone else first.
func (s *Service) drawQuote(ctx context.Context, o *foundrymodel.FoundryOntology, loadID string, seq, band, quotes int) (bool, error) {
	docs, err := s.deps.DB.FoundryReviewQuoteDocuments(ctx, loadID, band, reviewcards.QuoteTarget(loadID, band, seq, quotes), reviewQuoteDocuments)
	if err != nil {
		return false, status.Errorf(codes.Internal, "draw a spot check: %v", err)
	}
	for _, doc := range docs {
		body, err := reviewcards.QuotesQuery(loadID, doc)
		if err != nil {
			return false, status.Errorf(codes.Internal, "build the read: %v", err)
		}
		code, out, err := s.deps.Graph.ReadQuery(ctx, o.TenantID, body)
		if err != nil {
			return false, err
		}
		if code != 200 {
			return false, status.Error(codes.Unavailable, "the load's quotes could not be read; try again")
		}
		read, err := reviewcards.ParseQuotes(out)
		if err != nil {
			return false, status.Error(codes.Internal, "read the load's quotes")
		}
		taken, err := s.deps.DB.FoundryReviewPlacesTaken(ctx, o.ID, loadID, doc)
		if err != nil {
			return false, status.Errorf(codes.Internal, "draw a spot check: %v", err)
		}
		for _, q := range reviewcards.QuoteOrder(loadID, doc, read) {
			if reviewcards.BandOf(q.Confidence) != band || taken[foundrystore.FoundryReviewPlace(q.Section, q.Start, q.End, q.Link)] {
				continue
			}
			link, fact := q.Link, q.FactKey
			err := s.deps.DB.InsertFoundryReviewQuoteDraw(ctx, foundrymodel.FoundryReviewDraw{LoadID: loadID, Seq: seq, Kind: foundrymodel.FoundryReviewDrawQuote,
				Band: reviewcards.Bands[band], DocID: doc, Span: foundrymodel.FoundryClaimSpan{Section: q.Section, Start: q.Start, End: q.End},
				Link: &link, Confidence: q.Confidence, FactKey: &fact})
			if err != nil && !errors.Is(err, foundrystore.ErrConflict) {
				return false, status.Errorf(codes.Internal, "draw a spot check: %v", err)
			}
			return err == nil, err
		}
	}
	return false, nil
}

// spotChecks reads load loadID's spot checks with what the caller may
// re-check, and the decisions and labels at the drawn places.
func (s *Service) spotChecks(ctx context.Context, o *foundrymodel.FoundryOntology, loadID string) (*foundryv1.FoundryReviewSpotChecksResponse, error) {
	draws, err := s.deps.DB.ListFoundryReviewDraws(ctx, o.ID, loadID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the spot checks: %v", err)
	}
	out := &foundryv1.FoundryReviewSpotChecksResponse{LoadId: loadID}
	var docs []string
	for _, d := range draws {
		docs = append(docs, d.DocID)
		pb := &foundryv1.FoundryReviewDraw{Seq: int64(d.Seq), Band: d.Band, DocId: d.DocID, Done: d.Done, Confidence: d.Confidence,
			Span: &foundryv1.FoundryReviewSpan{Section: int64(d.Span.Section), Start: int64(d.Span.Start), End: int64(d.Span.End)}}
		if d.Link != nil {
			pb.Link = *d.Link
		}
		switch d.Kind {
		case foundrymodel.FoundryReviewDrawHeld:
			pb.Drawn = &foundryv1.FoundryReviewDraw_Held{Held: &foundryv1.FoundryReviewHeldDraw{ClaimKey: *d.ClaimKey, StatementHash: *d.StatementHash,
				ReviewKey: d.ReviewKey}}
		case foundrymodel.FoundryReviewDrawQuote:
			pb.Drawn = &foundryv1.FoundryReviewDraw_Quote{Quote: &foundryv1.FoundryReviewQuoteDraw{FactKey: *d.FactKey}}
		}
		out.Draws = append(out.Draws, pb)
	}
	slices.Sort(docs)
	decisions, err := s.deps.DB.ListFoundryClaimDecisions(ctx, o.ID, slices.Compact(docs))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the spot checks: %v", err)
	}
	at := func(doc string, span foundrymodel.FoundryClaimSpan) bool {
		return slices.ContainsFunc(draws, func(d foundrymodel.FoundryReviewDraw) bool { return d.DocID == doc && d.Span == span })
	}
	var atPlaces []*foundrymodel.FoundryReviewDecision
	for _, d := range decisions {
		if at(*d.DocID, *d.ClaimSpan) {
			atPlaces = append(atPlaces, d)
		}
	}
	labels, err := s.deps.DB.ListFoundryReviewLabelsAt(ctx, o.ID, draws)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the spot checks: %v", err)
	}
	for _, l := range labels {
		if slices.ContainsFunc(draws, func(d foundrymodel.FoundryReviewDraw) bool {
			return d.DocID == l.DocID && d.Span == l.Span && linkOf(d.Link) == linkOf(l.Link)
		}) {
			out.Labels = append(out.Labels, foundryReviewLabelProto(l))
		}
	}
	var recheckDecisions []*foundrymodel.FoundryReviewDecision
	var recheckLabels []*foundrymodel.FoundryReviewLabel
	if user := actor(ctx).UserID; user != "" {
		if recheckDecisions, recheckLabels, err = s.deps.DB.ListFoundryReviewRechecks(ctx, o.ID, user, reviewRechecks); err != nil {
			return nil, status.Errorf(codes.Internal, "read the spot checks: %v", err)
		}
	}
	ids := make([]string, 0, len(atPlaces)+len(recheckDecisions))
	for _, d := range append(slices.Clone(atPlaces), recheckDecisions...) {
		ids = append(ids, d.ID)
	}
	applied, err := s.deps.DB.ListFoundryDecisionsApplied(ctx, o.ID, ids)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the spot checks: %v", err)
	}
	decision := func(d *foundrymodel.FoundryReviewDecision) *foundryv1.FoundryReviewDecision {
		if a, ok := applied[d.ID]; ok {
			d.Applied = &a
		}
		return foundryReviewDecisionProto(d)
	}
	for _, d := range atPlaces {
		out.Decisions = append(out.Decisions, decision(d))
	}
	for _, d := range recheckDecisions {
		out.Rechecks = append(out.Rechecks, &foundryv1.FoundryReviewRecheck{Of: &foundryv1.FoundryReviewRecheck_Decision{Decision: decision(d)}})
	}
	for _, l := range recheckLabels {
		out.Rechecks = append(out.Rechecks, &foundryv1.FoundryReviewRecheck{Of: &foundryv1.FoundryReviewRecheck_Label{Label: foundryReviewLabelProto(l)}})
	}
	return out, nil
}

// linkOf is a place's link, none being empty.
func linkOf(link *string) string {
	if link == nil {
		return ""
	}
	return *link
}
