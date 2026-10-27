package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// SettleFoundryReview reads trusted identity from the current load's review.
// It stores only an override: no graph mutation or extract is queued.
func (s *Service) SettleFoundryReview(ctx context.Context, req *foundryv1.SettleFoundryReviewRequest) (*foundryv1.FoundryReviewDecisionResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetReviewId() == "" || req.GetDecision() == nil {
		return nil, status.Error(codes.InvalidArgument, "review_id and decision are required")
	}
	if actor(ctx).UserID == "" {
		return nil, status.Error(codes.Unauthenticated, "a user is required to settle a review")
	}
	if o.CurrentLoadID == nil {
		return nil, status.Error(codes.FailedPrecondition, "load the ontology before settling a review")
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
	rows, err := s.foundryDecisionRead(ctx, o, load.ID, map[string]string{"$label": "FoundryReview", "review_key": req.GetReviewId()}, []string{"reason", "type", "doc_id", "norm_name", "entity_key", "prop"})
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, status.Error(codes.NotFound, "review not found in the current load")
	}
	review := rows[0]
	text := func(key string) string { var s string; _ = json.Unmarshal(review[key], &s); return s }
	d := &foundrymodel.FoundryReviewDecision{ID: id.New(), OntologyID: o.ID, Type: text("type"), DecidedByUserID: actor(ctx).UserID}
	if d.Type == "" {
		return nil, status.Error(codes.FailedPrecondition, "this review is missing its identity; start a new load before settling it")
	}
	version, err := s.loadOntologyVersion(ctx, o.ID, load.VersionID)
	if err != nil {
		return nil, err
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(version.Definition, &def); err != nil {
		return nil, status.Error(codes.Internal, "read load definition")
	}
	types, err := foundrymodel.FoundryDecisionTypes(def.OntologyTOML)
	if err != nil {
		return nil, status.Error(codes.Internal, "read load schema")
	}
	ty, ok := types[d.Type]
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "the review type is not in the loaded version")
	}
	switch text("reason") {
	case "unresolved", "ambiguous", "collision", "identifier_conflict", "adjudicated":
		choice := req.GetDecision().GetResolve()
		if choice == nil {
			return nil, status.Error(codes.InvalidArgument, "this review requires a resolve decision")
		}
		doc, name := text("doc_id"), text("norm_name")
		d.Kind = foundrymodel.FoundryReviewResolve
		d.DocID = &doc
		d.NormName = &name
		switch target := choice.GetTarget().(type) {
		case *foundryv1.FoundryResolveChoice_EntityKey:
			if target.EntityKey == "" {
				return nil, status.Error(codes.InvalidArgument, "entity_key must not be empty")
			}
			d.EntityKey = &target.EntityKey
		case *foundryv1.FoundryResolveChoice_NotInData:
			if !target.NotInData {
				return nil, status.Error(codes.InvalidArgument, "not_in_data must be true")
			}
		default:
			return nil, status.Error(codes.InvalidArgument, "choose an entity or not_in_data")
		}
	case "prop_conflict":
		choice := req.GetDecision().GetProp()
		if choice.GetValue() == nil {
			return nil, status.Error(codes.InvalidArgument, "this review requires a prop decision")
		}
		entity, prop := text("entity_key"), text("prop")
		d.Kind = foundrymodel.FoundryReviewProp
		d.EntityKey = &entity
		d.Prop = &prop
		propType, ok := ty.Props[prop]
		if !ok {
			return nil, status.Error(codes.FailedPrecondition, "the property is not in the loaded version")
		}
		// Stored as the engine reads it back, and refused when the engine
		// would refuse it: one such decision fails every later load's Extract.
		// An integer is written from its int64, so it keeps every digit.
		var (
			raw []byte
			err error
		)
		switch v := choice.GetValue().(type) {
		case *foundryv1.FoundryPropValue_Null:
			raw = []byte("null")
		case *foundryv1.FoundryPropValue_Text:
			raw, err = json.Marshal(v.Text)
		case *foundryv1.FoundryPropValue_Integer:
			raw = strconv.AppendInt(nil, v.Integer, 10)
		case *foundryv1.FoundryPropValue_Number:
			raw, err = json.Marshal(v.Number)
		case *foundryv1.FoundryPropValue_Boolean:
			raw = strconv.AppendBool(nil, v.Boolean)
		}
		if err == nil {
			d.Value, err = propType.DecisionValue(raw)
		}
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "the chosen value does not match the property's type")
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "this review kind cannot be settled")
	}
	if err := d.Validate(); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "the review is missing its decision identity")
	}
	if d.EntityKey != nil {
		// Keyed and named types use different graph identities.
		prop := "entity_key"
		if len(ty.Key) == 0 {
			prop = "norm_name"
		}
		entities, err := s.foundryDecisionRead(ctx, o, load.ID, map[string]string{"$label": d.Type, prop: *d.EntityKey}, []string{prop})
		if err != nil {
			return nil, err
		}
		if len(entities) != 1 {
			return nil, status.Error(codes.InvalidArgument, "the chosen entity is not in this load")
		}
	}
	if err := s.deps.DB.SettleFoundryReviewDecision(ctx, o.WorkspaceID, d); err != nil {
		if errors.Is(err, foundrystore.ErrConflict) {
			return nil, status.Error(codes.FailedPrecondition, "the current load's version no longer has this review's type or property; reload the review and try again")
		}
		if errors.Is(err, foundrystore.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "ontology not found")
		}
		return nil, status.Errorf(codes.Internal, "settle review: %v", err)
	}
	return &foundryv1.FoundryReviewDecisionResponse{Decision: foundryReviewDecisionProto(d)}, nil
}

// foundryDecisionRead builds a bounded read from server-owned predicates. All
// values are JSON encoded, and foundry_load is mandatory. The host's graph
// reader (ports.GraphReader) enforces tenant state, endpoint ownership and
// response limits.
func (s *Service) foundryDecisionRead(ctx context.Context, o *foundrymodel.FoundryOntology, load string, where map[string]string, properties []string) ([]map[string]json.RawMessage, error) {
	predicates := []any{map[string]any{"eq": map[string]any{"left": map[string]string{"property": "foundry_load"}, "right": map[string]any{"constant": map[string]string{"string": load}}}}}
	for key, value := range where {
		predicates = append(predicates, map[string]any{"eq": map[string]any{"left": map[string]string{"property": key}, "right": map[string]any{"constant": map[string]string{"string": value}}}})
	}
	root := map[string]any{"value_map": map[string]any{"input": map[string]any{"limit": map[string]any{"input": map[string]any{"nodes_where": map[string]any{"predicate": map[string]any{"and": map[string]any{"predicates": predicates}}}}, "count": map[string]int{"literal": 2}}}, "properties": properties}}
	body, err := json.Marshal(map[string]any{"request_type": "read", "query": map[string]any{"read": map[string]any{"entries": []any{map[string]any{"query": map[string]any{"name": "items", "root": root}}}, "returns": []string{"items"}}}})
	if err != nil {
		return nil, err
	}
	if s.deps.Graph == nil {
		return nil, status.Error(codes.Unavailable, "graph reads are not available here")
	}
	code, out, err := s.deps.Graph.ReadQuery(ctx, o.TenantID, body)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, status.Error(codes.Unavailable, "the review graph could not be read; try again")
	}
	var response struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return nil, status.Error(codes.Internal, "read review graph response")
	}
	return response.Items, nil
}

func (s *Service) ListFoundryReviewDecisions(ctx context.Context, req *foundryv1.ListFoundryReviewDecisionsRequest) (*foundryv1.ListFoundryReviewDecisionsResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if len(req.GetPageToken()) > 26 {
		return nil, status.Error(codes.InvalidArgument, "invalid page_token")
	}
	rows, err := s.deps.DB.ListFoundryReviewDecisions(ctx, o.ID, req.GetPageToken(), nil, 101)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list review decisions: %v", err)
	}
	out := &foundryv1.ListFoundryReviewDecisionsResponse{}
	if len(rows) > 100 {
		out.NextPageToken = rows[99].ID
		rows = rows[:100]
	}
	for _, d := range rows {
		out.Decisions = append(out.Decisions, foundryReviewDecisionProto(d))
	}
	return out, nil
}

func (s *Service) DeleteFoundryReviewDecision(ctx context.Context, req *foundryv1.DeleteFoundryReviewDecisionRequest) (*foundryv1.DeleteFoundryReviewDecisionResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	err = s.deps.DB.DeleteFoundryReviewDecision(ctx, o.ID, req.GetId())
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "review decision not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete review decision: %v", err)
	}
	return &foundryv1.DeleteFoundryReviewDecisionResponse{}, nil
}

func foundryReviewDecisionProto(d *foundrymodel.FoundryReviewDecision) *foundryv1.FoundryReviewDecision {
	out := &foundryv1.FoundryReviewDecision{Id: d.ID, Kind: string(d.Kind), Type: d.Type, DocId: d.DocID, NormName: d.NormName, EntityKey: d.EntityKey, Prop: d.Prop, DecidedByUserId: d.DecidedByUserID, DecidedAt: timestamppb.New(d.DecidedAt)}
	if len(d.Value) == 0 {
		return out
	}
	// Validate keeps a stored value a JSON scalar; UseNumber keeps its digits.
	decoder := json.NewDecoder(bytes.NewReader(d.Value))
	decoder.UseNumber()
	var value any
	_ = decoder.Decode(&value)
	switch v := value.(type) {
	case nil:
		out.Value = &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Null{}}
	case string:
		out.Value = &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Text{Text: v}}
	case bool:
		out.Value = &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Boolean{Boolean: v}}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			out.Value = &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Integer{Integer: n}}
		} else {
			f, _ := v.Float64()
			out.Value = &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Number{Number: f}}
		}
	}
	return out
}
