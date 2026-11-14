package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// reviewPage is how many reviews one page lists.
	reviewPage = 50
	// maxReviewKeys is how many reviews one call names.
	maxReviewKeys = 50
	// maxReviewOffset is how deep pages go, as the site's Records do.
	maxReviewOffset = 10_000
	// maxReviewKeyBytes is the longest review key settle takes.
	maxReviewKeyBytes = 256
)

// reviewProperties are what a review's record says, as SettleFoundryReview reads it.
var reviewProperties = []string{"review_key", "reason", "type", "doc_id", "quote", "section", "start", "end", "norm_name", "candidates",
	"entity_key", "prop", "values", "suggested_link", "statement", "claim_key", "confidence"}

// resolveReasons are the reasons a resolve decision settles, heldReasons a
// release or a reject, as SettleFoundryReview takes them.
var (
	resolveReasons = map[string]bool{"unresolved": true, "ambiguous": true, "collision": true, "identifier_conflict": true, "adjudicated": true,
		"legal_form": true, "part_name": true, "own_name": true}
	heldReasons = map[string]bool{"low_confidence": true, "unverified": true, "conflict": true}
)

// ListFoundryReviews lists the reviews the ontology's current load holds,
// read from its database as the caller, each with what settles it as
// SettleFoundryReview would and the entities it concerns: a page at a time
// (an offset into one load, at most 10,000 deep), or the reviews review_keys
// names. A review a decision settled stays listed until a load writes the
// decision; ListFoundryReviewDecisions lists those.
func (s *Service) ListFoundryReviews(ctx context.Context, req *foundryv1.ListFoundryReviewsRequest) (*foundryv1.ListFoundryReviewsResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	keys := req.GetReviewKeys()
	// A page token is the load its list read and where the next page
	// starts: <load id>:<offset>.
	tokenLoad, offset := "", 0
	switch token := req.GetPageToken(); {
	case len(keys) > maxReviewKeys:
		return nil, status.Errorf(codes.InvalidArgument, "review_keys names at most %d reviews", maxReviewKeys)
	case len(keys) > 0 && token != "":
		return nil, status.Error(codes.InvalidArgument, "review_keys and page_token can't be used together")
	case token != "":
		var at string
		tokenLoad, at, _ = strings.Cut(token, ":")
		offset, err = strconv.Atoi(at)
		if err != nil || tokenLoad == "" || offset <= 0 || offset > maxReviewOffset || strconv.Itoa(offset) != at {
			return nil, status.Error(codes.InvalidArgument, "page_token is not one this list returned")
		}
	}
	for _, key := range keys {
		if key == "" || len(key) > maxReviewKeyBytes || strings.ContainsFunc(key, unicode.IsControl) {
			return nil, status.Error(codes.InvalidArgument, "each review key is 1 to 256 bytes")
		}
	}
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
	case tokenLoad != "" && tokenLoad != load.ID:
		return nil, status.Error(codes.FailedPrecondition, "another load became current since the first page: list the reviews again")
	case s.deps.Graph == nil:
		return nil, status.Error(codes.Unavailable, "graph reads are not available here")
	}
	predicates := []any{eqConstant("foundry_load", load.ID), eqConstant("$label", "FoundryReview")}
	if len(keys) > 0 {
		either := make([]any, 0, len(keys))
		for _, key := range keys {
			either = append(either, eqConstant("review_key", key))
		}
		predicates = append(predicates, map[string]any{"or": map[string]any{"predicates": either}})
	}
	// One past the page says whether another follows.
	end := offset + reviewPage + 1
	if len(keys) > 0 {
		end = len(keys)
	}
	reviews := map[string]any{"nodes": map[string]any{"reference": map[string]string{"var": "review_nodes"}}}
	concerns := func(step string) map[string]any {
		return map[string]any{step: map[string]any{"input": reviews, "label": "FOUNDRY_CONCERNS"}}
	}
	entries := []any{
		map[string]any{"query": map[string]any{"name": "review_nodes", "root": map[string]any{"range": map[string]any{
			"input": map[string]any{"nodes_where": map[string]any{"predicate": map[string]any{"and": map[string]any{"predicates": predicates}}}},
			"start": map[string]int{"literal": offset}, "end": map[string]int{"literal": end},
		}}}},
		map[string]any{"query": map[string]any{"name": "items", "root": map[string]any{"value_map": map[string]any{"input": reviews,
			"properties": append([]string{"$id"}, reviewProperties...)}}}},
		// The entities each concerns: the edges say whose, the nodes who.
		map[string]any{"query": map[string]any{"name": "review_concerns", "root": map[string]any{"edge_properties": map[string]any{"input": concerns("out_e")}}}},
		map[string]any{"query": map[string]any{"name": "concerned", "root": map[string]any{"values": map[string]any{
			"input":      map[string]any{"dedup": map[string]any{"input": concerns("out")}},
			"properties": []string{"$id", "$label", "entity_key", "norm_name", "name"},
		}}}},
	}
	body, err := json.Marshal(map[string]any{"request_type": "read", "query": map[string]any{"read": map[string]any{
		"entries": entries, "returns": []string{"items", "review_concerns", "concerned"}}}})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build the read: %v", err)
	}
	code, out, err := s.deps.Graph.ReadQuery(ctx, o.TenantID, body)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, status.Error(codes.Unavailable, "the review graph could not be read; try again")
	}
	var response struct {
		Items    []map[string]json.RawMessage `json:"items"`
		Concerns []struct {
			From int64 `json:"$from"`
			To   int64 `json:"$to"`
		} `json:"review_concerns"`
		Concerned []struct {
			ID        int64  `json:"$id"`
			Label     string `json:"$label"`
			EntityKey string `json:"entity_key"`
			NormName  string `json:"norm_name"`
			Name      string `json:"name"`
		} `json:"concerned"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return nil, status.Error(codes.Internal, "read review graph response")
	}
	result := &foundryv1.ListFoundryReviewsResponse{LoadId: load.ID}
	if len(keys) == 0 && len(response.Items) > reviewPage {
		response.Items = response.Items[:reviewPage]
		if next := offset + reviewPage; next <= maxReviewOffset {
			result.NextPageToken = load.ID + ":" + strconv.Itoa(next)
		}
	}
	// An entity is one of the ontology's, by the identity a review names it
	// by: Foundry's own nodes, and one with neither, are left out.
	entities := map[int64]*foundryv1.FoundryReviewEntity{}
	for _, e := range response.Concerned {
		if e.Label != "" && !strings.HasPrefix(e.Label, "Foundry") && (e.EntityKey != "") != (e.NormName != "") {
			entities[e.ID] = &foundryv1.FoundryReviewEntity{Type: e.Label, EntityKey: e.EntityKey, NormName: e.NormName, Name: e.Name}
		}
	}
	concerned := map[int64][]*foundryv1.FoundryReviewEntity{}
	for _, edge := range response.Concerns {
		if e, ok := entities[edge.To]; ok && !slices.Contains(concerned[edge.From], e) {
			concerned[edge.From] = append(concerned[edge.From], e)
		}
	}
	// Whether a conflict can be approved is its link's: read the load's
	// schema once, only when one is listed.
	var oneValued func(link string) bool
	for _, item := range response.Items {
		r := foundryReviewToProto(item)
		var nodeID int64
		if json.Unmarshal(item["$id"], &nodeID) == nil {
			r.Entities = concerned[nodeID]
		}
		if r.GetReason() == "conflict" && r.GetSettle() == foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_RELEASE {
			if oneValued == nil {
				ontologyTOML, err := s.loadOntologyTOML(ctx, o.ID, load.VersionID)
				if err != nil {
					return nil, err
				}
				oneValued = func(link string) bool {
					one, err := foundrymodel.FoundryOneValuedLink(ontologyTOML, link)
					return err == nil && one
				}
			}
			if !oneValued(r.GetLink()) {
				r.Settle = foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_UNSPECIFIED
			}
		}
		result.Reviews = append(result.Reviews, r)
	}
	return result, nil
}

// eqConstant is a predicate of property equal to a string.
func eqConstant(property, value string) map[string]any {
	return map[string]any{"eq": map[string]any{"left": map[string]string{"property": property}, "right": map[string]any{"constant": map[string]string{"string": value}}}}
}

// foundryReviewToProto reads a review's record, nothing of which is trusted,
// and says what settles it as SettleFoundryReview reads it.
func foundryReviewToProto(item map[string]json.RawMessage) *foundryv1.FoundryReview {
	text := func(key string) string { var s string; _ = json.Unmarshal(item[key], &s); return s }
	offset := func(key string) (int64, bool) {
		var n *int64
		if json.Unmarshal(item[key], &n) != nil || n == nil || *n < 0 {
			return 0, false
		}
		return *n, true
	}
	r := &foundryv1.FoundryReview{
		ReviewKey: text("review_key"), Reason: text("reason"), Type: text("type"), DocId: text("doc_id"), Quote: text("quote"),
		NormName: text("norm_name"), EntityKey: text("entity_key"), Prop: text("prop"), Link: text("suggested_link"), StatementJson: text("statement"),
	}
	section, okSection := offset("section")
	start, okStart := offset("start")
	end, okEnd := offset("end")
	if okSection && okStart && okEnd {
		r.Span = &foundryv1.FoundryReviewSpan{Section: section, Start: start, End: end}
	}
	var candidates []any
	_ = json.Unmarshal(item["candidates"], &candidates)
	for _, c := range candidates {
		if key, ok := c.(string); ok && key != "" && !slices.Contains(r.Candidates, key) {
			r.Candidates = append(r.Candidates, key)
		}
	}
	// The engine's PropQuote records, as JSON text.
	var quotes []struct {
		Value json.RawMessage `json:"value"`
		DocID string          `json:"doc_id"`
		Quote string          `json:"quote"`
	}
	if values := text("values"); values != "" && json.Unmarshal([]byte(values), &quotes) == nil {
		for _, q := range quotes {
			if len(q.Value) > 0 {
				r.Values = append(r.Values, &foundryv1.FoundryReviewValue{ValueJson: string(q.Value), DocId: q.DocID, Quote: q.Quote})
			}
		}
	}
	var confidence *float64
	if json.Unmarshal(item["confidence"], &confidence) == nil && confidence != nil && *confidence >= 0 && *confidence <= 1 {
		r.Confidence = confidence
	}
	if r.Link != "" && r.StatementJson != "" {
		r.StatementHash = foundrymodel.FoundryStatementHash(r.Link, r.StatementJson)
	}
	stale := foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_STALE
	switch reason := r.Reason; {
	case r.ReviewKey == "":
		r.Settle = stale
	case resolveReasons[reason]:
		r.Settle = foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_RESOLVE
		if r.Type == "" || r.DocId == "" || r.NormName == "" {
			r.Settle = stale
		}
	case reason == "prop_conflict":
		r.Settle = foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_PROP
		if r.Type == "" || r.EntityKey == "" || r.Prop == "" {
			r.Settle = stale
		}
	case reason == "conflict" && r.StatementJson == "":
		// Written before claims kept statements: never one a person settles.
	case heldReasons[reason]:
		r.Settle = foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_RELEASE
		if r.DocId == "" || text("claim_key") == "" || r.Span == nil || r.StatementHash == "" {
			r.Settle = stale
		}
	}
	return r
}
