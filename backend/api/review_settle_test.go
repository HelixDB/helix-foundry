package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// batchGraph answers a load's reviews by key, newest first (so a settle maps
// them back by key, never by order), the entities it holds, and a name's
// asking reviews; it counts each kind of read.
type batchGraph struct {
	reviews  map[string]map[string]any
	entities []string
	queries  *[]string
	// limits are the count each review read asked for.
	limits []int
	reads  map[string]int
	// asking is the documents whose reviews ask about any name read.
	asking []string
}

func (g *batchGraph) ReadQuery(_ context.Context, _ string, query []byte) (int, []byte, error) {
	q := string(query)
	*g.queries = append(*g.queries, q)
	var items []map[string]any
	switch {
	case strings.Contains(q, `"properties":["doc_id","reason","entity_key"]`):
		g.reads["asking"]++
		for _, doc := range g.asking {
			items = append(items, map[string]any{"doc_id": doc, "reason": "unresolved"})
		}
	case strings.Contains(q, `"FoundryReview"`):
		g.reads["reviews"]++
		var parsed struct {
			Query struct {
				Read struct {
					Entries []struct {
						Query struct {
							Root struct {
								ValueMap struct {
									Input struct {
										Limit struct {
											Count struct {
												Literal int `json:"literal"`
											} `json:"count"`
										} `json:"limit"`
									} `json:"input"`
								} `json:"value_map"`
							} `json:"root"`
						} `json:"query"`
					} `json:"entries"`
				} `json:"read"`
			} `json:"query"`
		}
		_ = json.Unmarshal(query, &parsed)
		g.limits = append(g.limits, parsed.Query.Read.Entries[0].Query.Root.ValueMap.Input.Limit.Count.Literal)
		keys := make([]string, 0, len(g.reviews))
		for key := range g.reviews {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		slices.Reverse(keys)
		for _, key := range keys {
			if strings.Contains(q, `{"string":"`+key+`"}`) {
				items = append(items, g.reviews[key])
			}
		}
	default:
		g.reads["entities"]++
		for _, e := range g.entities {
			if strings.Contains(q, `{"string":"`+e+`"}`) {
				items = append(items, map[string]any{"entity_key": e})
			}
		}
	}
	out, _ := json.Marshal(map[string]any{"items": items})
	return 200, out, nil
}

// batchService is reviewService over a graph holding reviews (by key) and
// entities.
func batchService(t *testing.T, reviews map[string]string, entities ...string) (*Service, *decisionStore, *batchGraph) {
	t.Helper()
	s, db, queries := reviewService(t, `{"items":[]}`)
	g := &batchGraph{reviews: map[string]map[string]any{}, entities: entities, queries: queries, reads: map[string]int{}}
	for key, review := range reviews {
		var item map[string]any
		if err := json.Unmarshal([]byte(review), &item); err != nil {
			t.Fatal(err)
		}
		item["review_key"] = key
		g.reviews[key] = item
	}
	s.deps.Graph = g
	return s, db, g
}

func batchRequest(expectOpen bool, items ...*foundryv1.FoundryReviewSettleItem) *foundryv1.BatchSettleFoundryReviewsRequest {
	return &foundryv1.BatchSettleFoundryReviewsRequest{WorkspaceId: "workspace-a", OntologyId: "o", LoadId: "load-1", Items: items, ExpectOpen: expectOpen}
}

func settleItem(key string, choice *foundryv1.FoundryReviewChoice) *foundryv1.FoundryReviewSettleItem {
	return &foundryv1.FoundryReviewSettleItem{ReviewId: key, Decision: choice}
}

// heldAt is a held claim at start of section 2 of doc-a, stating
// testStatement on promised_to.
func heldAt(start int) string {
	stored, _ := json.Marshal(testStatement)
	return fmt.Sprintf(`{"reason":"low_confidence","doc_id":"doc-a","claim_key":"%s","section":2,"start":%d,"end":%d,"suggested_link":"promised_to","statement":%s}`,
		testClaimKey, start, start+5, stored)
}

const (
	nameReview  = `{"reason":"unresolved","type":"Customer","doc_id":"doc-a","norm_name":"variant"}`
	valueReview = `{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"name"}`
)

// Each refusal a single settle answers with a status, a batch answers per
// item with its reason and the same words, beside an item that is saved.
func TestBatchSettleRefusals(t *testing.T) {
	type refusal = foundryv1.FoundryReviewSettleRefusalReason
	for name, tc := range map[string]struct {
		review  string
		choice  *foundryv1.FoundryReviewChoice
		saveErr error
		code    codes.Code
		reason  refusal
	}{
		"no such review":         {"", noneChoice(), nil, codes.NotFound, foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_NOT_FOUND},
		"a choice of other kind": {nameReview, propChoice(propText("x")), nil, codes.InvalidArgument, foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_INVALID},
		"an entity not loaded":   {nameReview, resolveChoice("customer-9"), nil, codes.InvalidArgument, foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_INVALID},
		"a value of other type":  {valueReview, propChoice(propNumber(3)), nil, codes.InvalidArgument, foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_INVALID},
		"no identity":            {`{"reason":"unresolved"}`, noneChoice(), nil, codes.FailedPrecondition, foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_INVALID},
		"a type the version lacks": {`{"reason":"unresolved","type":"Other","doc_id":"doc-a","norm_name":"x"}`, noneChoice(), nil, codes.FailedPrecondition,
			foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_STALE},
		"a claim that reads otherwise": {heldAt(40), releaseChoice(strings.Repeat("0", 64)), nil, codes.FailedPrecondition,
			foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_STALE},
		"a load changed under the lock": {valueReview, propChoice(propText("x")), foundrystore.ErrConflict, codes.FailedPrecondition,
			foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_STALE},
		"the value cap": {valueReview, propChoice(propText("x")), foundrystore.FoundryValueDecisionsFull, codes.FailedPrecondition,
			foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_FULL},
	} {
		t.Run(name, func(t *testing.T) {
			reviews := map[string]string{"good": heldAt(70)}
			if tc.review != "" {
				reviews["review-1"] = tc.review
			}
			ctx := ports.WithActor(context.Background(), reviewer)
			s, db, _ := batchService(t, reviews, "customer-1")
			db.saveErr = tc.saveErr
			_, single := s.SettleFoundryReview(ctx, settleRequest(tc.choice))
			if status.Code(single) != tc.code {
				t.Fatalf("single: %v", single)
			}
			s, db, _ = batchService(t, reviews, "customer-1")
			db.saveErr = tc.saveErr
			out, err := s.BatchSettleFoundryReviews(ctx, batchRequest(false, settleItem("review-1", tc.choice), settleItem("good", releaseChoice(testRead))))
			if tc.saveErr != nil {
				// The store refuses every item alike here: the good one too.
				if err != nil || out.Results[0].GetRefused().GetReason() != tc.reason {
					t.Fatalf("%v %v", err, out)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			refused := out.Results[0].GetRefused()
			if refused.GetReason() != tc.reason || refused.GetMessage() != status.Convert(single).Message() || out.Results[0].GetReviewId() != "review-1" {
				t.Fatalf("refused %+v, single %v", refused, single)
			}
			if out.Results[1].GetDecision().GetKind() != "release" || len(db.saved) != 1 {
				t.Fatalf("the good item: %+v saved %v", out.Results[1], db.saved)
			}
		})
	}
	// Another person's decision of the card, refused under expect open,
	// names it; the batch is ranked, and sends expect open on.
	s, db, _ := batchService(t, map[string]string{"review-1": valueReview}, "customer-1")
	db.saveErr = foundrystore.FoundryReviewDecidedElsewhere
	first := "user-b"
	db.decidedBy = &foundrymodel.FoundryReviewDecision{ID: "dec-b", Kind: foundrymodel.FoundryReviewProp, Type: "Customer", DecidedByUserID: first}
	out, err := s.BatchSettleFoundryReviews(ports.WithActor(context.Background(), reviewer), batchRequest(true, settleItem("review-1", propChoice(propText("x")))))
	if err != nil {
		t.Fatal(err)
	}
	refused := out.Results[0].GetRefused()
	if refused.GetReason() != foundryv1.FoundryReviewSettleRefusalReason_FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_DECIDED ||
		refused.GetDecided().GetDecidedByUserId() != first || !db.expectOpen || !strings.Contains(refused.GetMessage(), "another person decided") {
		t.Fatalf("%+v expect open %v", refused, db.expectOpen)
	}
}

// A batch saves every item it can in one store call, ranked, each result in
// its item's order; a name's asking documents are read once.
func TestBatchSettleSaves(t *testing.T) {
	reviews := map[string]string{"claim": heldAt(10), "value": valueReview}
	for i := range 25 {
		reviews[fmt.Sprintf("name-%02d", i)] = fmt.Sprintf(`{"reason":"unresolved","type":"Customer","doc_id":"doc-%d","norm_name":"variant"}`, i)
	}
	s, db, g := batchService(t, reviews, "customer-1")
	items := []*foundryv1.FoundryReviewSettleItem{settleItem("claim", releaseChoice(testRead)), settleItem("value", propChoice(propText("Chosen")))}
	for i := range 25 {
		items = append(items, settleItem(fmt.Sprintf("name-%02d", i), resolveChoice("customer-1")))
	}
	out, err := s.BatchSettleFoundryReviews(ports.WithActor(context.Background(), reviewer), batchRequest(true, items...))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 27 || len(db.saved) != 27 || !slices.Equal(db.batches, []int{27}) || len(db.resends) != 1 {
		t.Fatalf("%d results, %d saved, batches %v, resends %d", len(out.Results), len(db.saved), db.batches, len(db.resends))
	}
	for i, r := range out.Results {
		if r.GetReviewId() != items[i].GetReviewId() || r.GetDecision() == nil || r.GetDecision().GetDrawn() != "ranked" {
			t.Fatalf("result %d: %+v", i, r)
		}
	}
	if out.Results[0].GetDecision().GetKind() != "release" || out.Results[1].GetDecision().GetKind() != "prop" || out.Results[2].GetDecision().GetDocId() != "doc-0" {
		t.Fatalf("out of order: %+v", out.Results[:3])
	}
	if g.reads["reviews"] != 1 || g.reads["entities"] != 1 || g.reads["asking"] != 1 {
		t.Fatalf("reads %v", g.reads)
	}
	if db.readsAtSave != len(*g.queries) {
		t.Fatalf("%d reads after the save", len(*g.queries)-db.readsAtSave)
	}
}

// A batch reads a name's asking documents once, for its links: "not in
// data" of the same name in another document is handed none, so it owes no
// resend of them, whichever item comes first.
func TestBatchSettleNotInDataAsksNone(t *testing.T) {
	reviews := map[string]string{
		"linked": nameReview,
		"none":   `{"reason":"unresolved","type":"Customer","doc_id":"doc-b","norm_name":"variant"}`,
	}
	link, none := settleItem("linked", resolveChoice("customer-1")), settleItem("none", noneChoice())
	for name, items := range map[string][]*foundryv1.FoundryReviewSettleItem{"link first": {link, none}, "not in data first": {none, link}} {
		t.Run(name, func(t *testing.T) {
			s, db, g := batchService(t, reviews, "customer-1")
			g.asking = []string{"doc-c", "doc-d"}
			if _, err := s.BatchSettleFoundryReviews(ports.WithActor(context.Background(), reviewer), batchRequest(true, items...)); err != nil {
				t.Fatal(err)
			}
			none, saved := db.askingOf["doc-b"]
			if g.reads["asking"] != 1 || !slices.Equal(db.askingOf["doc-a"], g.asking) || !saved || none != nil {
				t.Fatalf("reads %v, asking %v", g.reads, db.askingOf)
			}
		})
	}
}

// Reviews are read 50 keys a read, each read asking for one row more than
// its keys, and mapped back by key whatever order the graph answers in.
func TestBatchSettleReadsInChunks(t *testing.T) {
	reviews := map[string]string{}
	items := make([]*foundryv1.FoundryReviewSettleItem, 0, 100)
	for i := range 100 {
		key := fmt.Sprintf("claim-%03d", i)
		reviews[key] = heldAt(10 + 10*i)
		items = append(items, settleItem(key, releaseChoice(testRead)))
	}
	s, db, g := batchService(t, reviews)
	out, err := s.BatchSettleFoundryReviews(ports.WithActor(context.Background(), reviewer), batchRequest(true, items...))
	if err != nil {
		t.Fatal(err)
	}
	if g.reads["reviews"] != 2 || g.reads["entities"] != 0 || !slices.Equal(g.limits, []int{51, 51}) || len(db.saved) != 100 {
		t.Fatalf("reads %v limits %v saved %d", g.reads, g.limits, len(db.saved))
	}
	for i, r := range out.Results {
		if r.GetDecision().GetClaimStart() != uint32(10+10*i) {
			t.Fatalf("item %d mapped to %+v", i, r.GetDecision())
		}
	}
}

// A batch the API can't take is refused whole, saving nothing: bad fields
// and another current load before any graph read; two items deciding one
// thing once the reviews are read, before any other read.
func TestBatchSettleRefusedWhole(t *testing.T) {
	ctx := ports.WithActor(context.Background(), reviewer)
	release := releaseChoice(testRead)
	many := make([]*foundryv1.FoundryReviewSettleItem, 101)
	for i := range many {
		many[i] = settleItem(fmt.Sprint("r", i), release)
	}
	for name, req := range map[string]*foundryv1.BatchSettleFoundryReviewsRequest{
		"no items":         batchRequest(false),
		"101 items":        batchRequest(false, many...),
		"an empty id":      batchRequest(false, settleItem("", release)),
		"a long id":        batchRequest(false, settleItem(strings.Repeat("r", 257), release)),
		"no decision":      batchRequest(false, settleItem("r", nil)),
		"no load":          {WorkspaceId: "workspace-a", OntologyId: "o", Items: []*foundryv1.FoundryReviewSettleItem{settleItem("r", release)}},
		"another load":     {WorkspaceId: "workspace-a", OntologyId: "o", LoadId: "load-0", Items: []*foundryv1.FoundryReviewSettleItem{settleItem("r", release)}},
		"another ontology": {WorkspaceId: "workspace-a", OntologyId: "x", LoadId: "load-1", Items: []*foundryv1.FoundryReviewSettleItem{settleItem("r", release)}},
	} {
		t.Run(name, func(t *testing.T) {
			s, db, g := batchService(t, map[string]string{"r": heldAt(10)})
			_, err := s.BatchSettleFoundryReviews(ctx, req)
			want := codes.InvalidArgument
			switch name {
			case "another load":
				want = codes.FailedPrecondition
			case "another ontology":
				want = codes.NotFound
			}
			if status.Code(err) != want || len(*g.queries) != 0 || len(db.batches) != 0 {
				t.Fatalf("%v after %d reads, %d batches", err, len(*g.queries), len(db.batches))
			}
			if name == "another load" && !strings.Contains(err.Error(), "read the cards again") {
				t.Fatal(err)
			}
		})
	}
	// One name decided twice (one review listed twice, or two reviews of
	// it), or one claim approved and rejected: the last would silently win.
	for name, items := range map[string][]*foundryv1.FoundryReviewSettleItem{
		"one review twice":   {settleItem("a", noneChoice()), settleItem("a", resolveChoice("customer-1"))},
		"two reviews of one": {settleItem("a", noneChoice()), settleItem("b", noneChoice())},
		"approve and reject": {settleItem("c", releaseChoice(testRead)), settleItem("d", rejectChoice(testRead))},
	} {
		t.Run(name, func(t *testing.T) {
			s, db, g := batchService(t, map[string]string{"a": nameReview, "b": nameReview, "c": heldAt(10), "d": heldAt(10)}, "customer-1")
			_, err := s.BatchSettleFoundryReviews(ctx, batchRequest(true, items...))
			if status.Code(err) != codes.InvalidArgument || len(db.batches) != 0 || g.reads["entities"] != 0 || g.reads["asking"] != 0 {
				t.Fatalf("%v, batches %v, reads %v", err, db.batches, g.reads)
			}
		})
	}
	// More rows than keys read: a key names one review.
	s, _, g := batchService(t, map[string]string{"r": heldAt(10)})
	g.reviews["s"] = g.reviews["r"]
	if _, err := s.BatchSettleFoundryReviews(ctx, batchRequest(false, settleItem("r", release), settleItem("s", release))); status.Code(err) != codes.Internal {
		t.Fatalf("%v", err)
	}
}

// The single settle is a batch of one that doesn't expect its card open.
func TestSettleFoundryReviewIsABatchOfOne(t *testing.T) {
	s, db, _ := batchService(t, map[string]string{"review-1": heldAt(10)})
	if _, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(releaseChoice(testRead))); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(db.batches, []int{1}) || db.expectOpen {
		t.Fatalf("batches %v, expect open %v", db.batches, db.expectOpen)
	}
}

func TestSettleKey(t *testing.T) {
	doc, name, entity, prop, key := "doc-a", "acme", "c-1", "tier", testClaimKey
	resolve := &foundrymodel.FoundryReviewDecision{Kind: foundrymodel.FoundryReviewResolve, Type: "Customer", DocID: &doc, NormName: &name}
	chosen := *resolve
	chosen.EntityKey = &entity
	value := &foundrymodel.FoundryReviewDecision{Kind: foundrymodel.FoundryReviewProp, Type: "Customer", EntityKey: &entity, Prop: &prop}
	release := &foundrymodel.FoundryReviewDecision{Kind: foundrymodel.FoundryReviewRelease, DocID: &doc, ClaimKey: &key,
		ClaimSpan: &foundrymodel.FoundryClaimSpan{Section: 1, Start: 2, End: 3}}
	reject := *release
	reject.Kind = foundrymodel.FoundryReviewReject
	moved := *release
	moved.ClaimSpan = &foundrymodel.FoundryClaimSpan{Section: 1, Start: 2, End: 4}
	if settleKey(resolve) != settleKey(&chosen) || settleKey(release) != settleKey(&reject) {
		t.Fatal("one name, or one claim, is one key whatever is chosen")
	}
	keys := []string{settleKey(resolve), settleKey(value), settleKey(release), settleKey(&moved)}
	slices.Sort(keys)
	if len(slices.Compact(keys)) != 4 {
		t.Fatalf("distinct things share a key: %q", keys)
	}
}
