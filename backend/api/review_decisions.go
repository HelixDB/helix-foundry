package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
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

// SettleFoundryReview reads trusted identity from the current load's review
// and stores an override. A resolve decision applies from the next load. A
// release, a person's approval of a held claim, applies to the current load
// at once: it joins that load's decisions and a resend job replays the
// claim's document, which writes it (SettleFoundryReleases). So does a prop
// decision, a value a person chose: the same job sends the load its
// decisions, and the engine sets the value (SettleFoundryValue). The
// decision returned says so (applied), and ListFoundryReviewDecisions what
// became of it. A reject, a person's word that a held claim is wrong, is
// only saved: nothing is written or sent, and readers leave the claim out
// of what needs review (SettleFoundryReject).
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
	rows, err := s.foundryDecisionRead(ctx, o, load.ID, map[string]string{"$label": "FoundryReview", "review_key": req.GetReviewId()}, []string{"reason", "type", "doc_id", "norm_name", "entity_key", "prop", "values", "claim_key", "section", "start", "end", "suggested_link", "statement"})
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, status.Error(codes.NotFound, "review not found in the current load")
	}
	review := rows[0]
	text := func(key string) string { var s string; _ = json.Unmarshal(review[key], &s); return s }
	d := &foundrymodel.FoundryReviewDecision{ID: id.New(), OntologyID: o.ID, Type: text("type"), DecidedByUserID: actor(ctx).UserID}
	// A held claim names no type: the engine reads what it states from its
	// document. A claim held as conflicting with what a link that holds one
	// value holds is one from contract 4.14.0, where the approval decides
	// the link; on another link it is a property that differs from the
	// fact's, which no approval changes, and it isn't settled.
	switch reason := text("reason"); reason {
	case "low_confidence", "unverified", "conflict":
		if reason == "conflict" {
			ontologyTOML, err := s.loadOntologyTOML(ctx, o.ID, load.VersionID)
			if err != nil {
				return nil, err
			}
			one, err := foundrymodel.FoundryOneValuedLink(ontologyTOML, text("suggested_link"))
			if err != nil {
				return nil, status.Error(codes.Internal, "read load schema")
			}
			if !one {
				return nil, status.Error(codes.FailedPrecondition, "this claim conflicts with a property of a fact the graph holds; only a conflict on a link that holds one value can be approved")
			}
		}
		if err := settleHeldClaim(req, d, review); err != nil {
			return nil, err
		}
		if d.Kind == foundrymodel.FoundryReviewReject {
			if err := s.deps.DB.SettleFoundryReject(ctx, o.WorkspaceID, load.ID, d); err != nil {
				return nil, settleFoundryReviewError(err)
			}
			return &foundryv1.FoundryReviewDecisionResponse{Decision: foundryReviewDecisionProto(d)}, nil
		}
		userID := actor(ctx).UserID
		resend := &foundrymodel.FoundryGraphJob{ID: id.New(), RequestedByUserID: &userID}
		if _, err := s.deps.DB.SettleFoundryReleases(ctx, o.WorkspaceID, load.ID, []*foundrymodel.FoundryReviewDecision{d}, resend); err != nil {
			return nil, settleFoundryReviewError(err)
		}
		return &foundryv1.FoundryReviewDecisionResponse{Decision: foundryReviewDecisionProto(d)}, nil
	}
	if d.Type == "" {
		return nil, status.Error(codes.FailedPrecondition, "this review is missing its identity; start a new load before settling it")
	}
	ontologyTOML, err := s.loadOntologyTOML(ctx, o.ID, load.VersionID)
	if err != nil {
		return nil, err
	}
	types, err := foundrymodel.FoundryDecisionTypes(ontologyTOML)
	if err != nil {
		return nil, status.Error(codes.Internal, "read load schema")
	}
	ty, ok := types[d.Type]
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "the review type is not in the loaded version")
	}
	switch text("reason") {
	// A name the load matched to no one row, or linked on less than a person's
	// word: by the model, by its legal form, by part of a row's name, or by an
	// identifier that is the name the text calls it by.
	case "unresolved", "ambiguous", "collision", "identifier_conflict", "adjudicated", "legal_form", "part_name", "own_name":
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
		// The engine's PropQuote records, each naming the document it quotes:
		// what the value chosen came from, so a grantee covers it only by
		// covering them. Unreadable quotes record none, and leave the decision
		// covered by no one.
		var quotes []struct {
			DocID string `json:"doc_id"`
		}
		_ = json.Unmarshal([]byte(text("values")), &quotes)
		for _, q := range quotes {
			if q.DocID != "" && !slices.Contains(d.DocumentIDs, q.DocID) {
				d.DocumentIDs = append(d.DocumentIDs, q.DocID)
			}
		}
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
	if d.Kind == foundrymodel.FoundryReviewProp {
		userID := actor(ctx).UserID
		resend := &foundrymodel.FoundryGraphJob{ID: id.New(), RequestedByUserID: &userID}
		if _, err := s.deps.DB.SettleFoundryValue(ctx, o.WorkspaceID, load.ID, d, resend); err != nil {
			return nil, settleFoundryReviewError(err)
		}
		return &foundryv1.FoundryReviewDecisionResponse{Decision: foundryReviewDecisionProto(d)}, nil
	}
	return s.saveFoundryReviewDecision(ctx, o, d)
}

// settleHeldClaim makes d the release or the reject of the one review it
// reads, as the request chose, by its claim's key and its quote's place, of
// the statement the person read: a load since may read the claim otherwise,
// and they haven't seen that. The engine writes what a released review
// states once its ends resolve, and keeps it held if a name no rung resolved
// is yet to be settled, saying why; a rejected one it never hears of. A
// review written before claims had keys and statements is settled by a new
// load's.
func settleHeldClaim(req *foundryv1.SettleFoundryReviewRequest, d *foundrymodel.FoundryReviewDecision, review map[string]json.RawMessage) error {
	var read string
	switch choice := req.GetDecision().GetChoice().(type) {
	case *foundryv1.FoundryReviewChoice_Release:
		if choice.Release == nil {
			return status.Error(codes.InvalidArgument, "this review requires a release or a reject decision")
		}
		d.Kind, read = foundrymodel.FoundryReviewRelease, choice.Release.GetStatementHash()
	case *foundryv1.FoundryReviewChoice_Reject:
		if choice.Reject == nil {
			return status.Error(codes.InvalidArgument, "this review requires a release or a reject decision")
		}
		d.Kind, read = foundrymodel.FoundryReviewReject, choice.Reject.GetStatementHash()
	default:
		return status.Error(codes.InvalidArgument, "this review requires a release or a reject decision")
	}
	var doc, claimKey, link, statement string
	var span foundrymodel.FoundryClaimSpan
	_ = json.Unmarshal(review["doc_id"], &doc)
	_ = json.Unmarshal(review["claim_key"], &claimKey)
	_ = json.Unmarshal(review["section"], &span.Section)
	_ = json.Unmarshal(review["start"], &span.Start)
	_ = json.Unmarshal(review["end"], &span.End)
	_ = json.Unmarshal(review["suggested_link"], &link)
	_ = json.Unmarshal(review["statement"], &statement)
	hash := foundrymodel.FoundryStatementHash(link, statement)
	d.Type, d.DocID, d.ClaimKey, d.ClaimSpan, d.StatementHash = "", &doc, &claimKey, &span, &hash
	if err := d.Validate(); err != nil || link == "" || statement == "" {
		return status.Error(codes.FailedPrecondition, "this review is missing its identity; start a new load before settling it")
	}
	if read != hash {
		return status.Error(codes.FailedPrecondition, "this claim reads differently now; reload the review and decide on what it says")
	}
	return nil
}

// loadOntologyTOML is the ontology TOML of one of o's versions, what a
// decision is checked against.
func (s *Service) loadOntologyTOML(ctx context.Context, ontologyID, versionID string) (string, error) {
	version, err := s.loadOntologyVersion(ctx, ontologyID, versionID)
	if err != nil {
		return "", err
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(version.Definition, &def); err != nil {
		return "", status.Error(codes.Internal, "read load definition")
	}
	return def.OntologyTOML, nil
}

// saveFoundryReviewDecision stores d, whose identity the review gave.
func (s *Service) saveFoundryReviewDecision(ctx context.Context, o *foundrymodel.FoundryOntology, d *foundrymodel.FoundryReviewDecision) (*foundryv1.FoundryReviewDecisionResponse, error) {
	if err := s.deps.DB.SettleFoundryReviewDecision(ctx, o.WorkspaceID, d); err != nil {
		return nil, settleFoundryReviewError(err)
	}
	return &foundryv1.FoundryReviewDecisionResponse{Decision: foundryReviewDecisionProto(d)}, nil
}

// settleFoundryReviewError maps a settle the store refused to its status: the
// current load changed since the review was read (or wrote the claim a
// rejection names), its version no longer takes the decision, or the
// ontology takes no value decision of another property.
func settleFoundryReviewError(err error) error {
	if errors.Is(err, foundrystore.ErrConflict) {
		return status.Error(codes.FailedPrecondition, "the current load changed, or its version no longer has this review's type or property; reload the review and try again")
	}
	if errors.Is(err, foundrystore.ErrNotFound) {
		return status.Error(codes.NotFound, "ontology not found")
	}
	if errors.Is(err, foundrystore.FoundryValueDecisionsFull) {
		return status.Errorf(codes.FailedPrecondition, "this ontology holds %d value decisions, the most a load can be sent; remove one before deciding another",
			foundrymodel.FoundryExtractDecisionsMax)
	}
	return status.Errorf(codes.Internal, "settle review: %v", err)
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
	rows, err := s.deps.DB.ListFoundryShownDecisions(ctx, o.ID, req.GetPageToken(), 101)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list review decisions: %v", err)
	}
	out := &foundryv1.ListFoundryReviewDecisionsResponse{}
	if len(rows) > 100 {
		out.NextPageToken = rows[99].ID
		rows = rows[:100]
	}
	ids := make([]string, 0, len(rows))
	for _, d := range rows {
		ids = append(ids, d.ID)
	}
	// What became of each in the current load, which the cards show.
	applied, err := s.deps.DB.ListFoundryDecisionsApplied(ctx, o.ID, ids)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list review decisions: %v", err)
	}
	for _, d := range rows {
		if a, ok := applied[d.ID]; ok {
			d.Applied = &a
		}
		out.Decisions = append(out.Decisions, foundryReviewDecisionProto(d))
	}
	return out, nil
}

// DeleteFoundryReviewDecision removes a saved decision. A value decision is
// withdrawn from the current load at once, as it was applied: a resend job
// sends the load its decisions without it, and the property goes back to
// what its other statements give. An approval is taken back from the current
// load at once too (contract 4.16.0): one the load may have written becomes
// a withdraw there, which a resend job sends, and the engine holds the claim
// again (the list shows the withdraw, and what became of it, while that load
// is current); one it never wrote is only dropped from the load, with
// nothing sent. A resolve stops applying from the next load. A withdraw is
// no saved decision: it is not found.
func (s *Service) DeleteFoundryReviewDecision(ctx context.Context, req *foundryv1.DeleteFoundryReviewDecisionRequest) (*foundryv1.DeleteFoundryReviewDecisionResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	// The job a value decision's withdrawal runs as; no other kind queues one.
	resend := &foundrymodel.FoundryGraphJob{ID: id.New()}
	if userID := actor(ctx).UserID; userID != "" {
		resend.RequestedByUserID = &userID
	}
	err = s.deps.DB.DeleteFoundryReviewDecision(ctx, o.WorkspaceID, o.ID, req.GetId(), resend)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "review decision not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete review decision: %v", err)
	}
	return &foundryv1.DeleteFoundryReviewDecisionResponse{}, nil
}

func foundryReviewDecisionProto(d *foundrymodel.FoundryReviewDecision) *foundryv1.FoundryReviewDecision {
	out := &foundryv1.FoundryReviewDecision{Id: d.ID, Kind: string(d.Kind), Type: d.Type, DocId: d.DocID, NormName: d.NormName, EntityKey: d.EntityKey, Prop: d.Prop, ClaimKey: d.ClaimKey, StatementHash: d.StatementHash, DecidedByUserId: d.DecidedByUserID, DecidedAt: timestamppb.New(d.DecidedAt)}
	if span := d.ClaimSpan; span != nil {
		out.ClaimSection, out.ClaimStart, out.ClaimEnd = &span.Section, &span.Start, &span.End
	}
	if a := d.Applied; a != nil {
		// Carried or settled here, a decision a resend owes is pending to a
		// reader; one whose job failed is unconfirmed, and nothing is coming.
		// A value decision's applied goes as it is.
		status := string(a.Status)
		switch {
		case a.Unconfirmed():
			status = foundrymodel.FoundryReleaseUnconfirmed
		case a.Status.Waits():
			status = string(foundrymodel.FoundryReleasePending)
		}
		out.Applied = &foundryv1.FoundryDecisionApplied{Status: status, Reason: a.Reason}
		if a.At != nil {
			out.Applied.At = timestamppb.New(*a.At)
		}
	}
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
