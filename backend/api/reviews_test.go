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
	"google.golang.org/protobuf/proto"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// protoSlicesEqual reports whether two lists hold equal messages, in order.
func protoSlicesEqual[M proto.Message](a, b []M) bool {
	return slices.EqualFunc(a, b, func(x, y M) bool { return proto.Equal(x, y) })
}

func listReviews(keys []string, token string) *foundryv1.ListFoundryReviewsRequest {
	return &foundryv1.ListFoundryReviewsRequest{WorkspaceId: "workspace-a", OntologyId: "o", ReviewKeys: keys, PageToken: token}
}

// Each review says what settles it, as SettleFoundryReview reads it, and
// nothing of the record is taken on trust.
func TestListFoundryReviews(t *testing.T) {
	values, _ := json.Marshal(`[{"value":"GBP","doc_id":"doc-b","quote":"billed in GBP"},{"value":12,"doc_id":"doc-c"},{"doc_id":"no-value"}]`)
	statement, _ := json.Marshal(testStatement)
	items := `{"items":[` +
		`{"$id":101,"review_key":"r-resolve","reason":"unresolved","type":"Customer","doc_id":"doc-a","norm_name":"globexx","candidates":["scus_1","scus_1","",7,"scus_2"],"quote":"Globexx","section":1,"start":4,"end":11,"confidence":0.4},` +
		`{"review_key":"r-old","reason":"ambiguous","type":"Customer","doc_id":"doc-a"},` +
		`{"$id":103,"review_key":"r-prop","reason":"prop_conflict","type":"Customer","entity_key":"scus_1","prop":"currency","values":` + string(values) + `},` +
		`{"review_key":"r-held","reason":"low_confidence","doc_id":"doc-a","claim_key":"k","section":2,"start":10,"end":25,"suggested_link":"promised_to","statement":` + string(statement) + `,"confidence":7},` +
		`{"review_key":"r-held-old","reason":"unverified","doc_id":"doc-a"},` +
		`{"review_key":"r-one","reason":"conflict","doc_id":"doc-a","claim_key":"k","section":2,"start":10,"end":25,"suggested_link":"promised_to","statement":` + string(statement) + `},` +
		`{"review_key":"r-many","reason":"conflict","doc_id":"doc-a","claim_key":"k","section":2,"start":10,"end":25,"suggested_link":"partner_of","statement":` + string(statement) + `},` +
		`{"review_key":"r-before","reason":"conflict","doc_id":"doc-a"},` +
		`{"review_key":"r-other","reason":"no_link"},` +
		`{"reason":"unresolved","type":"Customer","doc_id":"doc-a","norm_name":"x"}],` +
		// The entities r-resolve and r-prop concern: an edge to Foundry's own
		// node, or to one with no identity, or to none, names no entity.
		`"review_concerns":[{"$from":101,"$to":201},{"$from":101,"$to":202},{"$from":101,"$to":201},{"$from":103,"$to":201},` +
		`{"$from":101,"$to":203},{"$from":101,"$to":204},{"$from":101,"$to":999}],` +
		`"concerned":[{"$id":201,"$label":"Customer","entity_key":"scus_1","name":"Globex"},{"$id":202,"$label":"Person","norm_name":"ada"},` +
		`{"$id":203,"$label":"FoundryDocument","entity_key":"d"},{"$id":204,"$label":"Customer"}]}`
	s, _, queries := reviewService(t, items)
	out, err := s.ListFoundryReviews(ports.WithActor(context.Background(), reviewer), listReviews(nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if out.GetLoadId() != "load-1" || out.GetNextPageToken() != "" || len(out.GetReviews()) != 10 {
		t.Fatalf("%+v", out)
	}
	settle := map[string]foundryv1.FoundryReviewSettle{}
	for _, r := range out.GetReviews() {
		settle[r.GetReviewKey()] = r.GetSettle()
	}
	want := map[string]foundryv1.FoundryReviewSettle{
		"r-resolve": foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_RESOLVE, "r-old": foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_STALE,
		"r-prop": foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_PROP, "r-held": foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_RELEASE,
		"r-held-old": foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_STALE, "r-one": foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_RELEASE,
		"r-many": foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_UNSPECIFIED, "r-before": foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_UNSPECIFIED,
		"r-other": foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_UNSPECIFIED, "": foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_STALE,
	}
	for key, w := range want {
		if settle[key] != w {
			t.Errorf("%s settles by %v, not %v", key, settle[key], w)
		}
	}
	resolve := out.GetReviews()[0]
	if strings.Join(resolve.GetCandidates(), ",") != "scus_1,scus_2" || resolve.GetQuote() != "Globexx" || resolve.GetConfidence() != 0.4 ||
		resolve.GetSpan().GetSection() != 1 || resolve.GetSpan().GetStart() != 4 || resolve.GetSpan().GetEnd() != 11 {
		t.Fatalf("resolve %+v", resolve)
	}
	globex := &foundryv1.FoundryReviewEntity{Type: "Customer", EntityKey: "scus_1", Name: "Globex"}
	if want := []*foundryv1.FoundryReviewEntity{globex, {Type: "Person", NormName: "ada"}}; !protoSlicesEqual(resolve.GetEntities(), want) {
		t.Fatalf("resolve concerns %v, not %v", resolve.GetEntities(), want)
	}
	if !protoSlicesEqual(out.GetReviews()[2].GetEntities(), []*foundryv1.FoundryReviewEntity{globex}) || len(out.GetReviews()[1].GetEntities()) != 0 {
		t.Fatalf("each review names its own: %v, %v", out.GetReviews()[2].GetEntities(), out.GetReviews()[1].GetEntities())
	}
	prop := out.GetReviews()[2]
	if len(prop.GetValues()) != 2 || prop.GetValues()[0].GetValueJson() != `"GBP"` || prop.GetValues()[0].GetQuote() != "billed in GBP" ||
		prop.GetValues()[1].GetValueJson() != "12" || prop.GetSpan() != nil {
		t.Fatalf("prop values %+v", prop.GetValues())
	}
	held := out.GetReviews()[3]
	if held.GetStatementHash() != testRead || held.GetStatementJson() != testStatement || held.Confidence != nil {
		t.Fatalf("held %+v", held)
	}
	if len(*queries) != 1 {
		t.Fatalf("read %d times", len(*queries))
	}
	q := (*queries)[0]
	for _, part := range []string{`"foundry_load"`, `"load-1"`, `"FoundryReview"`, `"start":{"literal":0}`, `"end":{"literal":51}`,
		`"out_e":{"input":{"nodes":{"reference":{"var":"review_nodes"}}},"label":"FOUNDRY_CONCERNS"}`, `"returns":["items","review_concerns","concerned"]`} {
		if !strings.Contains(q, part) {
			t.Fatalf("read %s lacks %s", q, part)
		}
	}
}

// A full page says where the next starts, up to the 10,000th review.
func TestListFoundryReviewsPages(t *testing.T) {
	var many []string
	for i := range 51 {
		many = append(many, fmt.Sprintf(`{"review_key":"r-%d","reason":"no_link"}`, i))
	}
	s, _, _ := reviewService(t, `{"items":[`+strings.Join(many, ",")+`]}`)
	out, err := s.ListFoundryReviews(ports.WithActor(context.Background(), reviewer), listReviews(nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.GetReviews()) != 50 || out.GetNextPageToken() != "load-1:50" {
		t.Fatalf("%d reviews, next %q", len(out.GetReviews()), out.GetNextPageToken())
	}
	s, _, queries := reviewService(t, `{"items":[`+strings.Join(many, ",")+`]}`)
	out, err = s.ListFoundryReviews(ports.WithActor(context.Background(), reviewer), listReviews(nil, "load-1:9950"))
	if err != nil {
		t.Fatal(err)
	}
	if out.GetNextPageToken() != "load-1:10000" || !strings.Contains((*queries)[0], `"start":{"literal":9950}`) {
		t.Fatalf("next %q after %s", out.GetNextPageToken(), (*queries)[0])
	}
	s, _, _ = reviewService(t, `{"items":[`+strings.Join(many, ",")+`]}`)
	out, err = s.ListFoundryReviews(ports.WithActor(context.Background(), reviewer), listReviews(nil, "load-1:10000"))
	if err != nil || out.GetNextPageToken() != "" {
		t.Fatalf("past the last page: %v %q", err, out.GetNextPageToken())
	}
	// A page of a load no longer current is never read from another's.
	s, db, queries := reviewService(t, `{"items":[`+strings.Join(many, ",")+`]}`)
	newer := "load-2"
	db.ontology.CurrentLoadID, db.load.ID = &newer, newer
	_, err = s.ListFoundryReviews(ports.WithActor(context.Background(), reviewer), listReviews(nil, "load-1:50"))
	if status.Code(err) != codes.FailedPrecondition || len(*queries) != 0 {
		t.Fatalf("another load's page: %v after %d reads", err, len(*queries))
	}
}

// Named reviews are read by key alone.
func TestListFoundryReviewsByKey(t *testing.T) {
	s, _, queries := reviewService(t, `{"items":[{"review_key":"r-2","reason":"no_link"}]}`)
	out, err := s.ListFoundryReviews(ports.WithActor(context.Background(), reviewer), listReviews([]string{"r-1", "r-2"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.GetReviews()) != 1 || out.GetNextPageToken() != "" {
		t.Fatalf("%+v", out)
	}
	q := (*queries)[0]
	for _, part := range []string{`"or"`, `"r-1"`, `"r-2"`, `"end":{"literal":2}`} {
		if !strings.Contains(q, part) {
			t.Fatalf("read %s lacks %s", q, part)
		}
	}
}

func TestListFoundryReviewsRefusals(t *testing.T) {
	ctx := ports.WithActor(context.Background(), reviewer)
	tooMany := make([]string, 51)
	for i := range tooMany {
		tooMany[i] = fmt.Sprint("r-", i)
	}
	for name, req := range map[string]*foundryv1.ListFoundryReviewsRequest{
		"too many keys":    listReviews(tooMany, ""),
		"keys and a token": listReviews([]string{"r-1"}, "50"),
		"an empty key":     listReviews([]string{""}, ""),
		"a control key":    listReviews([]string{"r\n1"}, ""),
		"a long key":       listReviews([]string{strings.Repeat("r", 257)}, ""),
		"a word token":     listReviews(nil, "load-1:next"),
		"a zero token":     listReviews(nil, "load-1:0"),
		"a padded token":   listReviews(nil, "load-1:050"),
		"a deep token":     listReviews(nil, "load-1:10001"),
		"no load":          listReviews(nil, ":50"),
		"an offset alone":  listReviews(nil, "50"),
	} {
		s, _, queries := reviewService(t, `{"items":[]}`)
		if _, err := s.ListFoundryReviews(ctx, req); status.Code(err) != codes.InvalidArgument || len(*queries) != 0 {
			t.Errorf("%s: %v after %d reads", name, err, len(*queries))
		}
	}

	s, db, _ := reviewService(t, `{"items":[]}`)
	db.ontology.CurrentLoadID = nil
	if _, err := s.ListFoundryReviews(ctx, listReviews(nil, "")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no load: %v", err)
	}
	s, db, _ = reviewService(t, `{"items":[]}`)
	db.load.Status = foundrymodel.FoundryLoadWriting
	if _, err := s.ListFoundryReviews(ctx, listReviews(nil, "")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a load writing: %v", err)
	}
	s, _, _ = reviewService(t, `{"items":[]}`)
	s.deps.Graph = graphFunc(func(context.Context, string, []byte) (int, []byte, error) { return 503, []byte(`{}`), nil })
	if _, err := s.ListFoundryReviews(ctx, listReviews(nil, "")); status.Code(err) != codes.Unavailable {
		t.Fatalf("a database that refused: %v", err)
	}
	s, _, _ = reviewService(t, `not json`)
	if _, err := s.ListFoundryReviews(ctx, listReviews(nil, "")); status.Code(err) != codes.Internal {
		t.Fatalf("an answer that isn't JSON: %v", err)
	}
	s, _, _ = reviewService(t, `{"items":[]}`)
	s.deps.Graph = nil
	if _, err := s.ListFoundryReviews(ctx, listReviews(nil, "")); status.Code(err) != codes.Unavailable {
		t.Fatalf("no graph: %v", err)
	}
	s, _, _ = reviewService(t, `{"items":[]}`)
	if _, err := s.ListFoundryReviews(ctx, &foundryv1.ListFoundryReviewsRequest{WorkspaceId: "workspace-a", OntologyId: "other"}); status.Code(err) != codes.NotFound {
		t.Fatalf("another ontology: %v", err)
	}
}
