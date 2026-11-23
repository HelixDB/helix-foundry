package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

const (
	// maxSettleItems is the most items one batch settles.
	maxSettleItems = 100
	// maxReviewIDChars is the longest review_id a settle takes.
	maxReviewIDChars = 256
)

// settleReviewProperties are what a settle reads of each review.
var settleReviewProperties = []string{"review_key", "reason", "type", "doc_id", "norm_name", "entity_key", "prop", "values", "claim_key",
	"section", "start", "end", "suggested_link", "statement", "confidence"}

// SettleFoundryReview reads trusted identity from the current load's review
// and stores an override: a batch of one on the current load
// (settleReviews), which doesn't expect the card open, its refusal answered
// as its status. A resolve decision applies to the current load at once
// (contract 4.22.0): it joins that load's decisions and a resend job sends
// its document again, with the documents whose reviews ask about the same
// name where it chose an entity (an alias of every document), with their
// saved model work, which the engine replays to write what the name held
// back; a load read before prompt 9 has no saved work to replay, and the
// next load applies it. Another load made current after the review was read
// (both take the ontology's lock) gets the decision as one saved before it
// became current does: carried, and its asking documents read from that
// load by the resend job, as those read here are the earlier load's; applied
// says pending where it waits there. A release, a person's approval of a
// held claim, applies to the current load at once: it joins that load's
// decisions and a resend job sends the claim's document again, and the
// engine writes it from the review the load keeps. So does a prop decision,
// a value a person chose: the same job sends the load its decisions, and the
// engine sets the value. The decision returned says so (applied), and
// ListFoundryReviewDecisions what became of it. A reject, a person's word
// that a held claim is wrong, is only saved: nothing is written or sent, and
// readers leave the claim out of what needs review. A release or reject the
// queue drew at random is also kept as a label (labelDrawnClaim).
func (s *Service) SettleFoundryReview(ctx context.Context, req *foundryv1.SettleFoundryReviewRequest) (*foundryv1.FoundryReviewDecisionResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetReviewId() == "" || req.GetDecision() == nil {
		return nil, status.Error(codes.InvalidArgument, "review_id and decision are required")
	}
	drawn, err := foundrymodel.ParseFoundryReviewDrawn(req.GetDrawn())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	load, err := s.settleLoad(ctx, o)
	if err != nil {
		return nil, err
	}
	out, err := s.settleReviews(ctx, o, load, []*foundryv1.FoundryReviewSettleItem{{ReviewId: req.GetReviewId(), Decision: req.GetDecision()}},
		drawn, false)
	if err != nil {
		return nil, err
	}
	it := out[0]
	if it.refused != nil {
		return nil, it.refused.Err
	}
	if drawn != nil && *drawn == foundrymodel.FoundryDrawnRandom && it.d.OfClaim() {
		if err := s.labelDrawnClaim(ctx, o, load.ID, it.d, it.review); err != nil {
			return nil, err
		}
	}
	return &foundryv1.FoundryReviewDecisionResponse{Decision: foundryReviewDecisionProto(it.d)}, nil
}

// BatchSettleFoundryReviews settles up to maxSettleItems reviews of load_id,
// which must be the current load, each as SettleFoundryReview settles one
// (settleReviews), ranked, in one transaction: each result is the decision
// saved or why its item was refused. With expect_open, an item whose card
// another person decided is refused (DECIDED), never replacing their
// decision. Invalid fields, two items deciding one thing, another current
// load, a load not ready or a graph that can't be read refuse the whole
// batch, saving nothing.
func (s *Service) BatchSettleFoundryReviews(ctx context.Context, req *foundryv1.BatchSettleFoundryReviewsRequest) (*foundryv1.BatchSettleFoundryReviewsResponse, error) {
	items := req.GetItems()
	switch {
	case len(items) == 0 || len(items) > maxSettleItems:
		return nil, status.Errorf(codes.InvalidArgument, "items holds 1 to %d reviews", maxSettleItems)
	case slices.ContainsFunc(items, func(it *foundryv1.FoundryReviewSettleItem) bool {
		return it.GetReviewId() == "" || len(it.GetReviewId()) > maxReviewIDChars || it.GetDecision() == nil
	}):
		return nil, status.Errorf(codes.InvalidArgument, "each item has a review_id of 1 to %d characters and a decision", maxReviewIDChars)
	case req.GetLoadId() == "":
		return nil, status.Error(codes.InvalidArgument, "load_id is required")
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	load, err := s.settleLoad(ctx, o)
	if err != nil {
		return nil, err
	}
	if load.ID != req.GetLoadId() {
		return nil, status.Error(codes.FailedPrecondition, "another load became current: read the cards again")
	}
	ranked := foundrymodel.FoundryDrawnRanked
	out, err := s.settleReviews(ctx, o, load, items, &ranked, req.GetExpectOpen())
	if err != nil {
		return nil, err
	}
	resp := &foundryv1.BatchSettleFoundryReviewsResponse{Results: make([]*foundryv1.FoundryReviewSettleResult, len(out))}
	for i, it := range out {
		result := &foundryv1.FoundryReviewSettleResult{ReviewId: items[i].GetReviewId()}
		if r := it.refused; r != nil {
			refusal := &foundryv1.FoundryReviewSettleRefusal{Reason: settleReasonProto(r.Reason), Message: status.Convert(r.Err).Message()}
			if r.Decided != nil {
				refusal.Decided = foundryReviewDecisionProto(r.Decided)
			}
			result.Result = &foundryv1.FoundryReviewSettleResult_Refused{Refused: refusal}
		} else {
			result.Result = &foundryv1.FoundryReviewSettleResult_Decision{Decision: foundryReviewDecisionProto(it.d)}
		}
		resp.Results[i] = result
	}
	return resp, nil
}

// settleReasonProto is a refusal's reason as the API names it.
func settleReasonProto(r foundrymodel.FoundrySettleReason) foundryv1.FoundryReviewSettleRefusalReason {
	switch r {
	case foundrymodel.FoundrySettleStale:
		return foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_STALE
	case foundrymodel.FoundrySettleDecided:
		return foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_DECIDED
	case foundrymodel.FoundrySettleFull:
		return foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_FULL
	case foundrymodel.FoundrySettleInvalid:
		return foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_INVALID
	case foundrymodel.FoundrySettleNotFound:
		return foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_NOT_FOUND
	}
	return foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_UNSPECIFIED
}

// settleLoad is the load a person settles reviews of: o's current load, if
// it is ready.
func (s *Service) settleLoad(ctx context.Context, o *foundrymodel.FoundryOntology) (*foundrymodel.FoundryLoad, error) {
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
	return load, nil
}

// settled is one item of settleReviews: the review it read (nil when the load
// holds none), and the decision saved or why the item was refused.
type settled struct {
	review  map[string]json.RawMessage
	d       *foundrymodel.FoundryReviewDecision
	refused *foundrymodel.FoundrySettleRefusal
}

// refuse is an item's refusal: its reason, and the status a single settle
// answers with.
func refuse(reason foundrymodel.FoundrySettleReason, code codes.Code, message string) *foundrymodel.FoundrySettleRefusal {
	return &foundrymodel.FoundrySettleRefusal{Reason: reason, Err: status.Error(code, message)}
}

// settleSchema is the version of the load a batch settles, read once.
type settleSchema struct {
	toml  string
	types map[string]foundrymodel.FoundryDecisionType
	// err is why its types can't be read: only an item that needs them
	// fails on it.
	err error
}

// settleReviews is the one path a settle takes: it reads every review and
// entity it needs from the graph first, then saves every item in one store
// call (SettleFoundryReviews), so nothing is read under the ontology's lock.
// The reviews are read 50 a read by review_key, and mapped back by it; each
// item's decision is made from its review and choice (decisionOf); no two
// items may decide one thing (settleKey), as the last would silently win;
// each chosen entity is checked with one read per type and identity
// property; and each name's asking documents are read once. A refusal is
// per item, with its reason; any other failure refuses the batch.
func (s *Service) settleReviews(ctx context.Context, o *foundrymodel.FoundryOntology, load *foundrymodel.FoundryLoad,
	items []*foundryv1.FoundryReviewSettleItem, drawn *foundrymodel.FoundryReviewDrawn, expectOpen bool) ([]settled, error) {
	userID := actor(ctx).UserID
	keys := make([]string, 0, len(items))
	for _, it := range items {
		if !slices.Contains(keys, it.GetReviewId()) {
			keys = append(keys, it.GetReviewId())
		}
	}
	reviews := map[string]map[string]json.RawMessage{}
	for chunk := range slices.Chunk(keys, maxReviewKeys) {
		rows, err := s.foundryDecisionRead(ctx, o, load.ID, map[string]string{"$label": "FoundryReview"}, "review_key", chunk, settleReviewProperties)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			key := text(row, "review_key")
			if !slices.Contains(chunk, key) || reviews[key] != nil {
				return nil, status.Error(codes.Internal, "the load holds more than one review of a key")
			}
			reviews[key] = row
		}
	}
	var schema settleSchema
	schema.toml, schema.err = s.loadOntologyTOML(ctx, o.ID, load.VersionID)
	if schema.err != nil {
		return nil, schema.err
	}
	schema.types, schema.err = foundrymodel.FoundryDecisionTypes(schema.toml)

	out := make([]settled, len(items))
	byKey := map[string]int{}
	for i, it := range items {
		review := reviews[it.GetReviewId()]
		out[i].review = review
		if review == nil {
			out[i].refused = refuse(foundrymodel.FoundrySettleNotFound, codes.NotFound, "review not found in the current load")
			continue
		}
		d, refused, err := decisionOf(review, it.GetDecision(), schema, foundrymodel.FoundryReviewDecision{ID: id.New(), OntologyID: o.ID,
			DecidedByUserID: userID, Drawn: drawn})
		if err != nil {
			return nil, err
		}
		if refused != nil {
			out[i].refused = refused
			continue
		}
		key := settleKey(d)
		if _, twice := byKey[key]; twice {
			return nil, status.Error(codes.InvalidArgument, "two items decide the same review; send one")
		}
		byKey[key] = i
		out[i].d = d
	}

	// The entities chosen, one read per type and the property that
	// identifies it: keyed and named types use different graph identities.
	type identity struct{ ty, prop string }
	chosen := map[identity][]string{}
	for _, it := range out {
		if it.d == nil || it.d.EntityKey == nil {
			continue
		}
		at := identity{it.d.Type, "entity_key"}
		if len(schema.types[it.d.Type].Key) == 0 {
			at.prop = "norm_name"
		}
		if !slices.Contains(chosen[at], *it.d.EntityKey) {
			chosen[at] = append(chosen[at], *it.d.EntityKey)
		}
	}
	found := map[identity]map[string]int{}
	for at, entities := range chosen {
		found[at] = map[string]int{}
		for chunk := range slices.Chunk(entities, maxReviewKeys) {
			rows, err := s.foundryDecisionRead(ctx, o, load.ID, map[string]string{"$label": at.ty}, at.prop, chunk, []string{at.prop})
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				found[at][text(row, at.prop)]++
			}
		}
	}
	// What each name's asking documents are, read once a name.
	asking := map[[2]string][]string{}
	for i := range out {
		d := out[i].d
		if d == nil || d.EntityKey == nil {
			continue
		}
		at := identity{d.Type, "entity_key"}
		if len(schema.types[d.Type].Key) == 0 {
			at.prop = "norm_name"
		}
		if found[at][*d.EntityKey] != 1 {
			out[i].d, out[i].refused = nil, refuse(foundrymodel.FoundrySettleInvalid, codes.InvalidArgument, "the chosen entity is not in this load")
			continue
		}
		if d.Kind != foundrymodel.FoundryReviewResolve || !ontologyagent.AsksAboutName(text(out[i].review, "reason")) {
			continue
		}
		name := [2]string{d.Type, *d.NormName}
		if _, read := asking[name]; read {
			continue
		}
		docs, err := s.documentsAsking(ctx, o, load.ID, d.Type, *d.NormName)
		if err != nil {
			return nil, err
		}
		asking[name] = docs
	}

	// Every read is done: save what is left, in one transaction.
	var batch []*foundrymodel.FoundrySettle
	var at []int
	for i, it := range out {
		if it.d == nil {
			continue
		}
		item := &foundrymodel.FoundrySettle{D: it.d}
		if it.d.Kind == foundrymodel.FoundryReviewResolve {
			item.Name = foundrymodel.FoundryNameLinked{}
			if ontologyagent.AsksAboutName(text(it.review, "reason")) {
				// Only a link reaches the other documents asking about the
				// name (as an alias): "not in data" names none, though
				// another item of the batch read them.
				var docs []string
				if it.d.EntityKey != nil {
					docs = asking[[2]string{it.d.Type, *it.d.NormName}]
				}
				item.Name = foundrymodel.FoundryNameAsking{Documents: docs}
			}
		}
		batch, at = append(batch, item), append(at, i)
	}
	resend := &foundrymodel.FoundryGraphJob{ID: id.New(), RequestedByUserID: &userID}
	refusals, _, err := s.deps.DB.SettleFoundryReviews(ctx, o.WorkspaceID, o.ID, load.ID, batch, expectOpen, resend)
	if err != nil {
		return nil, settleFoundryReviewError(err)
	}
	for j, r := range refusals {
		if r == nil {
			continue
		}
		i := at[j]
		refused := &foundrymodel.FoundrySettleRefusal{Reason: r.Reason, Err: settleFoundryReviewError(r.Err), Decided: r.Decided}
		out[i].d, out[i].refused = nil, refused
	}
	return out, nil
}

// text is a review's string field, empty when it has none.
func text(review map[string]json.RawMessage, key string) string {
	var s string
	_ = json.Unmarshal(review[key], &s)
	return s
}

// settleKey is what a decision decides, so two items of a batch never decide
// one thing: a name in a document, a value of an entity's property, or a
// held claim at its place (its release and its reject collide).
func settleKey(d *foundrymodel.FoundryReviewDecision) string {
	switch d.Kind {
	case foundrymodel.FoundryReviewResolve:
		return strings.Join([]string{"name", d.Type, *d.DocID, *d.NormName}, "\x00")
	case foundrymodel.FoundryReviewProp:
		return strings.Join([]string{"value", d.Type, *d.EntityKey, *d.Prop}, "\x00")
	}
	span := d.ClaimSpan
	return strings.Join([]string{"claim", *d.DocID, strconv.FormatUint(uint64(span.Section), 10), strconv.FormatUint(uint64(span.Start), 10),
		strconv.FormatUint(uint64(span.End), 10), *d.ClaimKey}, "\x00")
}

// decisionOf makes the decision that choice makes of review, from base (its
// id, ontology, person and drawn), checked against schema, the version of
// the load the review was read from. A choice that can't settle the review is
// refused with its reason; err is a failure of the batch (a schema that
// can't be read). Held claims name no type: the engine reads what they state
// from their document. A claim held as conflicting with what a link that
// holds one value holds is one from contract 4.14.0, where the approval
// decides the link; on another link it is a property that differs from the
// fact's, which no approval changes, and it isn't settled.
func decisionOf(review map[string]json.RawMessage, choice *foundryv1.FoundryReviewChoice, schema settleSchema,
	base foundrymodel.FoundryReviewDecision) (*foundrymodel.FoundryReviewDecision, *foundrymodel.FoundrySettleRefusal, error) {
	d := &base
	d.Type = text(review, "type")
	switch reason := text(review, "reason"); reason {
	case "low_confidence", "unverified", "conflict":
		if reason == "conflict" {
			one, err := foundrymodel.FoundryOneValuedLink(schema.toml, text(review, "suggested_link"))
			if err != nil {
				return nil, nil, status.Error(codes.Internal, "read load schema")
			}
			if !one {
				return nil, refuse(foundrymodel.FoundrySettleInvalid, codes.FailedPrecondition,
					"this claim conflicts with a property of a fact the graph holds; only a conflict on a link that holds one value can be approved"), nil
			}
		}
		if refused := settleHeldClaim(choice, d, review); refused != nil {
			return nil, refused, nil
		}
		return d, nil, nil
	}
	if d.Type == "" {
		return nil, refuse(foundrymodel.FoundrySettleInvalid, codes.FailedPrecondition, "this review is missing its identity; start a new load before settling it"), nil
	}
	if schema.err != nil {
		return nil, nil, status.Error(codes.Internal, "read load schema")
	}
	ty, ok := schema.types[d.Type]
	if !ok {
		return nil, refuse(foundrymodel.FoundrySettleStale, codes.FailedPrecondition, "the review type is not in the loaded version"), nil
	}
	invalid := func(message string) (*foundrymodel.FoundryReviewDecision, *foundrymodel.FoundrySettleRefusal, error) {
		return nil, refuse(foundrymodel.FoundrySettleInvalid, codes.InvalidArgument, message), nil
	}
	switch text(review, "reason") {
	// A name the load matched to no one row, or linked on less than a person's
	// word: by the model, by its legal form, by part of a row's name, or by an
	// identifier that is the name the text calls it by.
	case "unresolved", "ambiguous", "collision", "identifier_conflict", "adjudicated", "legal_form", "part_name", "own_name":
		resolve := choice.GetResolve()
		if resolve == nil {
			return invalid("this review requires a resolve decision")
		}
		doc, name := text(review, "doc_id"), text(review, "norm_name")
		d.Kind, d.DocID, d.NormName = foundrymodel.FoundryReviewResolve, &doc, &name
		switch target := resolve.GetTarget().(type) {
		case *foundryv1.FoundryResolveChoice_EntityKey:
			if target.EntityKey == "" {
				return invalid("entity_key must not be empty")
			}
			d.EntityKey = &target.EntityKey
		case *foundryv1.FoundryResolveChoice_NotInData:
			if !target.NotInData {
				return invalid("not_in_data must be true")
			}
		default:
			return invalid("choose an entity or not_in_data")
		}
	case "prop_conflict":
		value := choice.GetProp()
		if value.GetValue() == nil {
			return invalid("this review requires a prop decision")
		}
		entity, prop := text(review, "entity_key"), text(review, "prop")
		d.Kind, d.EntityKey, d.Prop = foundrymodel.FoundryReviewProp, &entity, &prop
		// The engine's PropQuote records, each naming the document it quotes:
		// what the value chosen came from, so a grantee covers it only by
		// covering them. Unreadable quotes record none, and leave the decision
		// covered by no one.
		var quotes []struct {
			DocID string `json:"doc_id"`
		}
		_ = json.Unmarshal([]byte(text(review, "values")), &quotes)
		for _, q := range quotes {
			if q.DocID != "" && !slices.Contains(d.DocumentIDs, q.DocID) {
				d.DocumentIDs = append(d.DocumentIDs, q.DocID)
			}
		}
		propType, ok := ty.Props[prop]
		if !ok {
			return nil, refuse(foundrymodel.FoundrySettleStale, codes.FailedPrecondition, "the property is not in the loaded version"), nil
		}
		// Stored as the engine reads it back, and refused when the engine
		// would refuse it: one such decision fails every later load's Extract.
		// An integer is written from its int64, so it keeps every digit.
		var (
			raw []byte
			err error
		)
		switch v := value.GetValue().(type) {
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
			return invalid("the chosen value does not match the property's type")
		}
	default:
		return invalid("this review kind cannot be settled")
	}
	if err := d.Validate(); err != nil {
		return nil, refuse(foundrymodel.FoundrySettleInvalid, codes.FailedPrecondition, "the review is missing its decision identity"), nil
	}
	return d, nil, nil
}

// documentsAsking are the documents of load whose reviews still ask about
// name ty/name, which an alias of that name settles in place when they are
// sent again (contract 4.22.0): one read of the load's reviews of that type
// and name (ontologyagent.AskingQuery), each document once, every one of
// them. The engine indexes reviews by name (contract 4.24.0), so the read is
// one lookup; a person's settle makes it, not an Extract. A document sent
// that no longer asks costs nothing: the engine says not_asked and writes
// nothing.
func (s *Service) documentsAsking(ctx context.Context, o *foundrymodel.FoundryOntology, load, ty, name string) ([]string, error) {
	body, err := ontologyagent.AskingQuery(load, ty, name)
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
	docs, past, err := ontologyagent.AskingDocuments(out)
	if err != nil {
		return nil, status.Error(codes.Internal, "read review graph response")
	}
	if past {
		log.From(ctx).Warn("settle: a name has more reviews in the load than one read returns; the rest of its documents wait for the next load",
			"ontology", o.ID, "documents", len(docs), "reviews", ontologyagent.AskingReviewsMax)
	}
	return docs, nil
}

// settleHeldClaim makes d the release or the reject of the one review it
// reads, as choice chose, by its claim's key and its quote's place, of the
// statement the person read: a load since may read the claim otherwise, and
// they haven't seen that. The engine writes what a released review states
// once its ends resolve, and keeps it held if a name no rung resolved is yet
// to be settled, saying why; a rejected one it never hears of. A review
// written before claims had keys and statements is settled by a new load's.
func settleHeldClaim(choice *foundryv1.FoundryReviewChoice, d *foundrymodel.FoundryReviewDecision, review map[string]json.RawMessage) *foundrymodel.FoundrySettleRefusal {
	var read string
	switch c := choice.GetChoice().(type) {
	case *foundryv1.FoundryReviewChoice_Release:
		if c.Release == nil {
			return refuse(foundrymodel.FoundrySettleInvalid, codes.InvalidArgument, "this review requires a release or a reject decision")
		}
		d.Kind, read = foundrymodel.FoundryReviewRelease, c.Release.GetStatementHash()
	case *foundryv1.FoundryReviewChoice_Reject:
		if c.Reject == nil {
			return refuse(foundrymodel.FoundrySettleInvalid, codes.InvalidArgument, "this review requires a release or a reject decision")
		}
		d.Kind, read = foundrymodel.FoundryReviewReject, c.Reject.GetStatementHash()
	default:
		return refuse(foundrymodel.FoundrySettleInvalid, codes.InvalidArgument, "this review requires a release or a reject decision")
	}
	var claimKey, link, statement string
	var span foundrymodel.FoundryClaimSpan
	doc := text(review, "doc_id")
	_ = json.Unmarshal(review["claim_key"], &claimKey)
	_ = json.Unmarshal(review["section"], &span.Section)
	_ = json.Unmarshal(review["start"], &span.Start)
	_ = json.Unmarshal(review["end"], &span.End)
	_ = json.Unmarshal(review["suggested_link"], &link)
	_ = json.Unmarshal(review["statement"], &statement)
	hash := foundrymodel.FoundryStatementHash(link, statement)
	d.Type, d.DocID, d.ClaimKey, d.ClaimSpan, d.StatementHash = "", &doc, &claimKey, &span, &hash
	if err := d.Validate(); err != nil || link == "" || statement == "" {
		return refuse(foundrymodel.FoundrySettleInvalid, codes.FailedPrecondition, "this review is missing its identity; start a new load before settling it")
	}
	if read != hash {
		return refuse(foundrymodel.FoundrySettleStale, codes.FailedPrecondition, "this claim reads differently now; reload the review and decide on what it says")
	}
	return nil
}

// labelDrawnClaim keeps the decision d of a held claim the review queue drew
// at random as a label of that claim (a release says it is right, a reject
// that it is wrong), with the link and the verifier's score its review was
// read at: the measure of precision in the claim's score band, which outlives
// the decision. The decision is saved first; a label that fails to save says
// so, and settling again (which saves the same decision) saves it.
func (s *Service) labelDrawnClaim(ctx context.Context, o *foundrymodel.FoundryOntology, loadID string, d *foundrymodel.FoundryReviewDecision, review map[string]json.RawMessage) error {
	verdict := foundrymodel.FoundryVerdictRight
	if d.Kind == foundrymodel.FoundryReviewReject {
		verdict = foundrymodel.FoundryVerdictWrong
	}
	// The person's own word, with the decision that stands: an approval of a
	// statement someone approved before keeps theirs (store saveFoundryReviewDecision).
	label := &foundrymodel.FoundryReviewLabel{ID: id.New(), OntologyID: o.ID, LoadID: loadID, Drawn: foundrymodel.FoundryLabelRandom,
		DocID: *d.DocID, Span: *d.ClaimSpan, ClaimKey: d.ClaimKey, StatementHash: d.StatementHash, Verdict: verdict,
		DecisionID: &d.ID, DecidedByUserID: actor(ctx).UserID}
	var link string
	if json.Unmarshal(review["suggested_link"], &link) == nil && link != "" {
		label.Link = &link
	}
	// A review with no score (unverified) reads as null or nothing.
	var confidence *float64
	if json.Unmarshal(review["confidence"], &confidence) == nil && confidence != nil && *confidence >= 0 && *confidence <= 1 {
		label.Confidence = confidence
	}
	// A re-check this person made of the claim stands: one label a claim.
	labels, err := s.deps.DB.FoundryReviewLabelsOf(ctx, o.ID, label.DocID, label.Span, label.ClaimKey, nil)
	if err != nil {
		return status.Errorf(codes.Internal, "the decision is saved, but not its label of the random slice; settle it again: %v", err)
	}
	for _, l := range labels {
		if l.DecidedByUserID == label.DecidedByUserID && l.Drawn == foundrymodel.FoundryLabelRecheck {
			return nil
		}
	}
	if err := s.deps.DB.SaveFoundryReviewLabel(ctx, o.WorkspaceID, label); err != nil {
		return status.Errorf(codes.Internal, "the decision is saved, but not its label of the random slice; settle it again: %v", err)
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

// settleFoundryReviewError maps a settle the store refused to its status: the
// current load changed since the review was read (or wrote the claim a
// rejection names), its version no longer takes the decision, the ontology
// takes no value decision of another property, or another person decided
// the card (expect open). A status passes as it is.
func settleFoundryReviewError(err error) error {
	if errors.Is(err, foundrystore.FoundryReviewDecidedElsewhere) {
		return status.Error(codes.FailedPrecondition, "another person decided this review; read it again")
	}
	if errors.Is(err, foundrystore.ErrConflict) {
		return status.Error(codes.FailedPrecondition, "the current load changed, or its version no longer has this review's type or property; reload the review and try again")
	}
	if errors.Is(err, foundrystore.ErrNotFound) {
		return status.Error(codes.NotFound, "ontology not found")
	}
	if errors.Is(err, foundrystore.FoundryValueDecisionsFull) {
		return status.Errorf(codes.FailedPrecondition, "this ontology holds %d value decisions, the most a load can be sent; remove one before deciding another",
			foundrymodel.FoundryExtractKindDecisionsMax)
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Errorf(codes.Internal, "settle review: %v", err)
}

// foundryDecisionRead builds a bounded read from server-owned predicates:
// where's properties equal, and property in one of values (1 to 50), at most
// one row a value and one more, so a value that names two shows. All values
// are JSON encoded, and foundry_load is mandatory. The host's graph reader
// (ports.GraphReader) enforces tenant state, endpoint ownership and response
// limits.
func (s *Service) foundryDecisionRead(ctx context.Context, o *foundrymodel.FoundryOntology, load string, where map[string]string, in string,
	values []string, properties []string) ([]map[string]json.RawMessage, error) {
	eq := func(key, value string) any {
		return map[string]any{"eq": map[string]any{"left": map[string]string{"property": key}, "right": map[string]any{"constant": map[string]string{"string": value}}}}
	}
	predicates := []any{eq("foundry_load", load)}
	for key, value := range where {
		predicates = append(predicates, eq(key, value))
	}
	either := make([]any, len(values))
	for i, v := range values {
		either[i] = eq(in, v)
	}
	predicates = append(predicates, map[string]any{"or": map[string]any{"predicates": either}})
	root := map[string]any{"value_map": map[string]any{"input": map[string]any{"limit": map[string]any{"input": map[string]any{"nodes_where": map[string]any{"predicate": map[string]any{"and": map[string]any{"predicates": predicates}}}}, "count": map[string]int{"literal": len(values) + 1}}}, "properties": properties}}
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
	ids := req.GetIds()
	switch {
	case len(ids) > maxDecisionIDs:
		return nil, status.Errorf(codes.InvalidArgument, "ids names at most %d decisions", maxDecisionIDs)
	case len(ids) > 0 && req.GetPageToken() != "":
		return nil, status.Error(codes.InvalidArgument, "ids and page_token can't be used together")
	case slices.ContainsFunc(ids, func(id string) bool { return id == "" || len(id) > 26 }):
		return nil, status.Error(codes.InvalidArgument, "each id is 1 to 26 characters")
	}
	// Only a client that asks gets the current load's withdraws: a site
	// before contract 4.16.0, beside this backend in a deploy's window or
	// after a rollback of the site alone, can't show one.
	list := func() ([]*foundrymodel.FoundryReviewDecision, error) {
		return s.deps.DB.ListFoundryReviewDecisions(ctx, o.ID, req.GetPageToken(), nil, 101)
	}
	switch {
	case len(ids) > 0:
		// A walk polls what became of its own decisions by id.
		list = func() ([]*foundrymodel.FoundryReviewDecision, error) {
			return s.deps.DB.ListFoundryReviewDecisionsByID(ctx, o.ID, ids, req.GetIncludeWithdraws())
		}
	case req.GetIncludeWithdraws():
		list = func() ([]*foundrymodel.FoundryReviewDecision, error) {
			return s.deps.DB.ListFoundryShownDecisions(ctx, o.ID, req.GetPageToken(), 101)
		}
	}
	rows, err := list()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list review decisions: %v", err)
	}
	out := &foundryv1.ListFoundryReviewDecisionsResponse{}
	if len(rows) > 100 {
		out.NextPageToken = rows[99].ID
		rows = rows[:100]
	}
	listed := make([]string, 0, len(rows))
	for _, d := range rows {
		listed = append(listed, d.ID)
	}
	// What became of each in the current load, which the cards show.
	applied, err := s.deps.DB.ListFoundryDecisionsApplied(ctx, o.ID, listed)
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
	if d.Drawn != nil {
		out.Drawn = string(*d.Drawn)
	}
	if span := d.ClaimSpan; span != nil {
		out.ClaimSection, out.ClaimStart, out.ClaimEnd = &span.Section, &span.Start, &span.End
	}
	if a := d.Applied; a != nil {
		// Carried or settled here, a decision a resend owes is pending to a
		// reader; one no word came of (its job failed, or, a withdraw, its
		// document's run said nothing of it) is unconfirmed, and nothing is
		// coming for it alone.
		// A value decision's applied goes as it is.
		status := string(a.Status)
		switch {
		case a.Unconfirmed():
			status = foundrymodel.FoundryReleaseUnconfirmed
		case a.Status.Waits():
			status = string(foundrymodel.FoundryReleasePending)
		}
		out.Applied = &foundryv1.FoundryDecisionApplied{Status: status, Reason: a.Reason, TakesBack: a.TakesBack}
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
