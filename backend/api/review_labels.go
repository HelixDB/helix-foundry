package api

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// SettleFoundryReviewLabel keeps a person's word on a claim, to measure
// precision: a claim the review queue drew at random (a fact's quote, or a
// held claim someone has decided), or a second person's re-check of a
// decision on a held claim or of another person's label drawn at random. It
// reads the claim from the current load (its link and the verifier's
// score), the decision or the label, never from the request, and changes
// no decision and no load.
func (s *Service) SettleFoundryReviewLabel(ctx context.Context, req *foundryv1.SettleFoundryReviewLabelRequest) (*foundryv1.FoundryReviewLabelResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	user := actor(ctx).UserID
	if user == "" {
		return nil, status.Error(codes.Unauthenticated, "a user is required to label a claim")
	}
	verdict, err := foundrymodel.ParseFoundryVerdict(req.GetVerdict())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	drawn := foundrymodel.FoundryLabelDrawn(req.GetDrawn())
	if drawn != foundrymodel.FoundryLabelRandom && drawn != foundrymodel.FoundryLabelRecheck {
		return nil, status.Error(codes.InvalidArgument, "drawn must be random or recheck")
	}
	if o.CurrentLoadID == nil {
		return nil, status.Error(codes.FailedPrecondition, "load the ontology before labelling a claim")
	}
	load, err := s.deps.DB.GetFoundryLoad(ctx, o.ID, *o.CurrentLoadID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "load not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get load: %v", err)
	}
	if !load.Status.Servable() {
		return nil, status.Error(codes.FailedPrecondition, "the load is not ready")
	}
	label := &foundrymodel.FoundryReviewLabel{ID: id.New(), OntologyID: o.ID, LoadID: load.ID, Drawn: drawn, Verdict: verdict, DecidedByUserID: user}
	// A draw is of a claim the load holds; a re-check of another's word.
	random := drawn == foundrymodel.FoundryLabelRandom
	switch subject := req.GetSubject().(type) {
	case *foundryv1.SettleFoundryReviewLabelRequest_Fact:
		if !random {
			return nil, status.Error(codes.InvalidArgument, "a fact is labelled when drawn at random; re-check another's label by its label_id")
		}
		err = s.labelOfFact(ctx, o, load.ID, subject.Fact, label)
	case *foundryv1.SettleFoundryReviewLabelRequest_ReviewId:
		if !random {
			return nil, status.Error(codes.InvalidArgument, "a held claim is labelled when drawn at random; re-check a decision by its decision_id")
		}
		err = s.labelOfReview(ctx, o, load.ID, subject.ReviewId, label)
	case *foundryv1.SettleFoundryReviewLabelRequest_DecisionId:
		if random {
			return nil, status.Error(codes.InvalidArgument, "a label of a decision is a re-check")
		}
		err = s.labelOfDecision(ctx, o.ID, subject.DecisionId, label)
	case *foundryv1.SettleFoundryReviewLabelRequest_LabelId:
		if random {
			return nil, status.Error(codes.InvalidArgument, "a label of a label is a re-check")
		}
		err = s.labelOfLabel(ctx, o.ID, subject.LabelId, label)
	default:
		return nil, status.Error(codes.InvalidArgument, "a label is of a fact, a held claim, a decision or a label")
	}
	if err != nil {
		return nil, err
	}
	// One label per person and claim, so neither kind replaces the other: a
	// re-check is a second person's word, never one who labelled the claim
	// at random; and a person's re-check stands against a later draw of it.
	labels, err := s.deps.DB.FoundryReviewLabelsOf(ctx, o.ID, label.DocID, label.Span, label.ClaimKey, label.FactKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read labels: %v", err)
	}
	for _, l := range labels {
		if l.DecidedByUserID != user || l.Drawn == label.Drawn {
			continue
		}
		if l.Drawn == foundrymodel.FoundryLabelRandom {
			return nil, status.Error(codes.FailedPrecondition, "you have labelled this claim already; a re-check is another person's")
		}
		return nil, status.Error(codes.FailedPrecondition, "you have re-checked this claim; that word stands")
	}
	if err := s.deps.DB.SaveFoundryReviewLabel(ctx, o.WorkspaceID, label); err != nil {
		if errors.Is(err, foundrystore.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "ontology not found")
		}
		return nil, status.Errorf(codes.Internal, "save label: %v", err)
	}
	return &foundryv1.FoundryReviewLabelResponse{Label: foundryReviewLabelProto(label)}, nil
}

// labelOfFact fills label from the fact's quote f names, as the current
// load holds it: its link and the verifier's score.
func (s *Service) labelOfFact(ctx context.Context, o *foundrymodel.FoundryOntology, loadID string, f *foundryv1.FoundryLabelFact, label *foundrymodel.FoundryReviewLabel) error {
	key := f.GetFactKey()
	if f.GetDocId() == "" || len(f.GetDocId()) > 26 || key == "" || len(key) > foundrymodel.FoundryFactKeyMax || f.GetStart() >= f.GetEnd() {
		return status.Error(codes.InvalidArgument, "a fact is its document, section, byte range and fact key")
	}
	span := foundrymodel.FoundryClaimSpan{Section: f.GetSection(), Start: f.GetStart(), End: f.GetEnd()}
	link, confidence, err := s.foundryFactQuote(ctx, o, loadID, f.GetDocId(), key, span)
	if err != nil {
		return err
	}
	label.DocID, label.Span, label.FactKey, label.Link, label.Confidence = f.GetDocId(), span, &key, &link, confidence
	return nil
}

// labelOfReview fills label from the held claim the current load's review
// reviewID holds back: its key and place, the hash of its statement, its
// link and the verifier's score. Its decision, if any, stands as it is.
func (s *Service) labelOfReview(ctx context.Context, o *foundrymodel.FoundryOntology, loadID, reviewID string, label *foundrymodel.FoundryReviewLabel) error {
	if reviewID == "" || len(reviewID) > 256 {
		return status.Error(codes.InvalidArgument, "invalid review_id")
	}
	rows, err := s.foundryDecisionRead(ctx, o, loadID, map[string]string{"$label": "FoundryReview", "review_key": reviewID},
		[]string{"reason", "doc_id", "claim_key", "section", "start", "end", "suggested_link", "statement", "confidence"})
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return status.Error(codes.NotFound, "review not found in the current load")
	}
	review := rows[0]
	var reason, doc, claimKey, link, statement string
	var span foundrymodel.FoundryClaimSpan
	var confidence *float64
	_ = json.Unmarshal(review["reason"], &reason)
	_ = json.Unmarshal(review["doc_id"], &doc)
	_ = json.Unmarshal(review["claim_key"], &claimKey)
	_ = json.Unmarshal(review["section"], &span.Section)
	_ = json.Unmarshal(review["start"], &span.Start)
	_ = json.Unmarshal(review["end"], &span.End)
	_ = json.Unmarshal(review["suggested_link"], &link)
	_ = json.Unmarshal(review["statement"], &statement)
	_ = json.Unmarshal(review["confidence"], &confidence)
	switch reason {
	case "low_confidence", "unverified", "conflict":
	default:
		return status.Error(codes.FailedPrecondition, "only a held claim is labelled so")
	}
	if doc == "" || link == "" || statement == "" {
		return status.Error(codes.FailedPrecondition, "this review is missing its identity; start a new load before labelling it")
	}
	hash := foundrymodel.FoundryStatementHash(link, statement)
	label.DocID, label.Span, label.ClaimKey, label.StatementHash, label.Link = doc, span, &claimKey, &hash, &link
	if confidence != nil && *confidence >= 0 && *confidence <= 1 {
		label.Confidence = confidence
	}
	return nil
}

// labelOfLabel fills a re-check's label from another person's label drawn
// at random, which it checks: its claim, link and score, and its verdict.
func (s *Service) labelOfLabel(ctx context.Context, ontologyID, labelID string, label *foundrymodel.FoundryReviewLabel) error {
	if labelID == "" || len(labelID) > 26 {
		return status.Error(codes.InvalidArgument, "invalid label_id")
	}
	l, err := s.deps.DB.GetFoundryReviewLabel(ctx, ontologyID, labelID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return status.Error(codes.NotFound, "review label not found")
	}
	if err != nil {
		return status.Errorf(codes.Internal, "get review label: %v", err)
	}
	if l.Drawn != foundrymodel.FoundryLabelRandom {
		return status.Error(codes.FailedPrecondition, "only a label drawn at random is re-checked")
	}
	if l.DecidedByUserID == label.DecidedByUserID {
		return status.Error(codes.FailedPrecondition, "a re-check is another person's word, not the label's own")
	}
	against := l.Verdict
	label.DocID, label.Span, label.ClaimKey, label.StatementHash, label.FactKey = l.DocID, l.Span, l.ClaimKey, l.StatementHash, l.FactKey
	label.Link, label.Confidence, label.Against = l.Link, l.Confidence, &against
	return nil
}

// labelOfDecision fills a re-check's label from the saved decision on a
// held claim it checks, made by someone else: the claim's place and key,
// the statement read, and the verdict it gave (an approval right, a
// rejection wrong), with the link and score of the claim's random label
// where the decision came with one.
func (s *Service) labelOfDecision(ctx context.Context, ontologyID, decisionID string, label *foundrymodel.FoundryReviewLabel) error {
	if label.Drawn != foundrymodel.FoundryLabelRecheck {
		return status.Error(codes.InvalidArgument, "a label of a decision is a re-check")
	}
	if decisionID == "" || len(decisionID) > 26 {
		return status.Error(codes.InvalidArgument, "invalid decision_id")
	}
	d, err := s.deps.DB.GetFoundryReviewDecision(ctx, ontologyID, decisionID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return status.Error(codes.NotFound, "review decision not found")
	}
	if err != nil {
		return status.Errorf(codes.Internal, "get review decision: %v", err)
	}
	if !d.OfClaim() {
		return status.Error(codes.FailedPrecondition, "only a decision on a held claim is re-checked")
	}
	if d.DecidedByUserID == label.DecidedByUserID {
		return status.Error(codes.FailedPrecondition, "a re-check is another person's word, not the decision's own")
	}
	against := foundrymodel.FoundryVerdictRight
	if d.Kind == foundrymodel.FoundryReviewReject {
		against = foundrymodel.FoundryVerdictWrong
	}
	label.DocID, label.Span, label.ClaimKey, label.StatementHash = *d.DocID, *d.ClaimSpan, d.ClaimKey, d.StatementHash
	label.DecisionID, label.Against = &d.ID, &against
	labels, err := s.deps.DB.FoundryReviewLabelsOf(ctx, ontologyID, label.DocID, label.Span, d.ClaimKey, nil)
	if err != nil {
		return status.Errorf(codes.Internal, "read labels: %v", err)
	}
	for _, l := range labels {
		if l.DecisionID != nil && *l.DecisionID == d.ID && l.Drawn == foundrymodel.FoundryLabelRandom {
			label.Link, label.Confidence = l.Link, l.Confidence
		}
	}
	return nil
}

// foundryFactQuote reads the accepted fact's quote at doc's span from load:
// its evidence node of fact key, whose byte range is the span's and whose
// section (FOUNDRY_FROM_SECTION) has the span's ordinal. It returns the
// quote's link and the verifier's score. NotFound means the load holds no
// such quote.
func (s *Service) foundryFactQuote(ctx context.Context, o *foundrymodel.FoundryOntology, load, doc, factKey string, span foundrymodel.FoundryClaimSpan) (string, *float64, error) {
	eq := func(property, value string) any {
		return map[string]any{"eq": map[string]any{"left": map[string]string{"property": property}, "right": map[string]any{"constant": map[string]string{"string": value}}}}
	}
	ref := map[string]any{"nodes": map[string]any{"reference": map[string]string{"var": "quotes"}}}
	entries := []any{
		map[string]any{"query": map[string]any{"name": "quotes", "root": map[string]any{"limit": map[string]any{
			"input": map[string]any{"nodes_where": map[string]any{"predicate": map[string]any{"and": map[string]any{"predicates": []any{
				eq("$label", "FoundryEvidence"), eq("foundry_load", load), eq("fact_key", factKey), eq("doc_id", doc)}}}}},
			"count": map[string]int{"literal": 201}}}}},
		map[string]any{"query": map[string]any{"name": "items", "root": map[string]any{"values": map[string]any{"input": ref,
			"properties": []string{"$id", "start", "end", "link", "confidence"}}}}},
		map[string]any{"query": map[string]any{"name": "in_section", "root": map[string]any{"edge_properties": map[string]any{
			"input": map[string]any{"out_e": map[string]any{"input": ref, "label": "FOUNDRY_FROM_SECTION"}}}}}},
		map[string]any{"query": map[string]any{"name": "sections", "root": map[string]any{"values": map[string]any{
			"input": map[string]any{"out": map[string]any{"input": ref, "label": "FOUNDRY_FROM_SECTION"}}, "properties": []string{"$id", "ordinal"}}}}},
	}
	body, err := json.Marshal(map[string]any{"request_type": "read", "query": map[string]any{"read": map[string]any{
		"entries": entries, "returns": []string{"items", "in_section", "sections"}}}})
	if err != nil {
		return "", nil, err
	}
	if s.deps.Graph == nil {
		return "", nil, status.Error(codes.Unavailable, "graph reads are not available here")
	}
	code, out, err := s.deps.Graph.ReadQuery(ctx, o.TenantID, body)
	if err != nil {
		return "", nil, err
	}
	if code != 200 {
		return "", nil, status.Error(codes.Unavailable, "the graph could not be read; try again")
	}
	var response struct {
		Items []struct {
			ID         int64    `json:"$id"`
			Start      *uint32  `json:"start"`
			End        *uint32  `json:"end"`
			Link       string   `json:"link"`
			Confidence *float64 `json:"confidence"`
		} `json:"items"`
		InSection []struct {
			From int64 `json:"$from"`
			To   int64 `json:"$to"`
		} `json:"in_section"`
		Sections []struct {
			ID      int64   `json:"$id"`
			Ordinal *uint32 `json:"ordinal"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return "", nil, status.Error(codes.Internal, "read graph response")
	}
	ordinal := map[int64]uint32{}
	for _, sec := range response.Sections {
		if sec.Ordinal != nil {
			ordinal[sec.ID] = *sec.Ordinal
		}
	}
	section := map[int64]int64{}
	for _, e := range response.InSection {
		section[e.From] = e.To
	}
	for _, q := range response.Items {
		to, ok := section[q.ID]
		if !ok || q.Start == nil || q.End == nil || *q.Start != span.Start || *q.End != span.End || q.Link == "" {
			continue
		}
		if at, ok := ordinal[to]; !ok || at != span.Section {
			continue
		}
		if c := q.Confidence; c != nil && (*c < 0 || *c > 1) {
			return q.Link, nil, nil
		}
		return q.Link, q.Confidence, nil
	}
	return "", nil, status.Error(codes.NotFound, "the current load holds no such quote of the fact")
}

// ListFoundryReviewLabels lists an ontology's labels, 100 a page by id.
func (s *Service) ListFoundryReviewLabels(ctx context.Context, req *foundryv1.ListFoundryReviewLabelsRequest) (*foundryv1.ListFoundryReviewLabelsResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if len(req.GetPageToken()) > 26 {
		return nil, status.Error(codes.InvalidArgument, "invalid page_token")
	}
	rows, err := s.deps.DB.ListFoundryReviewLabels(ctx, o.ID, req.GetPageToken(), 101)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list review labels: %v", err)
	}
	out := &foundryv1.ListFoundryReviewLabelsResponse{}
	if len(rows) > 100 {
		out.NextPageToken = rows[99].ID
		rows = rows[:100]
	}
	for _, l := range rows {
		out.Labels = append(out.Labels, foundryReviewLabelProto(l))
	}
	return out, nil
}

func foundryReviewLabelProto(l *foundrymodel.FoundryReviewLabel) *foundryv1.FoundryReviewLabel {
	out := &foundryv1.FoundryReviewLabel{Id: l.ID, Drawn: string(l.Drawn), DocId: l.DocID, ClaimSection: l.Span.Section, ClaimStart: l.Span.Start,
		ClaimEnd: l.Span.End, ClaimKey: l.ClaimKey, StatementHash: l.StatementHash, FactKey: l.FactKey, Link: l.Link, Confidence: l.Confidence,
		Verdict: string(l.Verdict), DecisionId: l.DecisionID, LoadId: l.LoadID, DecidedByUserId: l.DecidedByUserID, DecidedAt: timestamppb.New(l.DecidedAt)}
	if l.Against != nil {
		against := string(*l.Against)
		out.Against = &against
	}
	return out
}
