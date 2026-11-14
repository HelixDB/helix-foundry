package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

func listReviews(keys []string, token string) *foundryv1.ListFoundryReviewsRequest {
	return &foundryv1.ListFoundryReviewsRequest{WorkspaceId: "workspace-a", OntologyId: "o", ReviewKeys: keys, PageToken: token}
}

// Each review says what settles it, as SettleFoundryReview reads it, and
// nothing of the record is taken on trust.
func TestListFoundryReviews(t *testing.T) {
	values, _ := json.Marshal(`[{"value":"GBP","doc_id":"doc-b","quote":"billed in GBP"},{"value":12,"doc_id":"doc-c"},{"doc_id":"no-value"}]`)
	statement, _ := json.Marshal(testStatement)
	items := `{"items":[` +
		`{"review_key":"r-resolve","reason":"unresolved","type":"Customer","doc_id":"doc-a","norm_name":"globexx","candidates":["scus_1","scus_1","",7,"scus_2"],"quote":"Globexx","section":1,"start":4,"end":11,"confidence":0.4},` +
		`{"review_key":"r-old","reason":"ambiguous","type":"Customer","doc_id":"doc-a"},` +
		`{"review_key":"r-prop","reason":"prop_conflict","type":"Customer","entity_key":"scus_1","prop":"currency","values":` + string(values) + `},` +
		`{"review_key":"r-held","reason":"low_confidence","doc_id":"doc-a","claim_key":"k","section":2,"start":10,"end":25,"suggested_link":"promised_to","statement":` + string(statement) + `,"confidence":7},` +
		`{"review_key":"r-held-old","reason":"unverified","doc_id":"doc-a"},` +
		`{"review_key":"r-one","reason":"conflict","doc_id":"doc-a","claim_key":"k","section":2,"start":10,"end":25,"suggested_link":"promised_to","statement":` + string(statement) + `},` +
		`{"review_key":"r-many","reason":"conflict","doc_id":"doc-a","claim_key":"k","section":2,"start":10,"end":25,"suggested_link":"partner_of","statement":` + string(statement) + `},` +
		`{"review_key":"r-before","reason":"conflict","doc_id":"doc-a"},` +
		`{"review_key":"r-other","reason":"no_link"},` +
		`{"reason":"unresolved","type":"Customer","doc_id":"doc-a","norm_name":"x"}]}`
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
	for _, part := range []string{`"foundry_load"`, `"load-1"`, `"FoundryReview"`, `"start":{"literal":0}`, `"end":{"literal":51}`} {
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
	s, _, queries := reviewService(t, `{"items":[`+strings.Join(many, ",")+`]}`)
	out, err := s.ListFoundryReviews(ports.WithActor(context.Background(), reviewer), listReviews(nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.GetReviews()) != 50 || out.GetNextPageToken() != "50" {
		t.Fatalf("%d reviews, next %q", len(out.GetReviews()), out.GetNextPageToken())
	}
	s, _, queries = reviewService(t, `{"items":[`+strings.Join(many, ",")+`]}`)
	out, err = s.ListFoundryReviews(ports.WithActor(context.Background(), reviewer), listReviews(nil, "9950"))
	if err != nil {
		t.Fatal(err)
	}
	if out.GetNextPageToken() != "10000" || !strings.Contains((*queries)[0], `"start":{"literal":9950}`) {
		t.Fatalf("next %q after %s", out.GetNextPageToken(), (*queries)[0])
	}
	s, _, _ = reviewService(t, `{"items":[`+strings.Join(many, ",")+`]}`)
	out, err = s.ListFoundryReviews(ports.WithActor(context.Background(), reviewer), listReviews(nil, "10000"))
	if err != nil || out.GetNextPageToken() != "" {
		t.Fatalf("past the last page: %v %q", err, out.GetNextPageToken())
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
		"a word token":     listReviews(nil, "next"),
		"a zero token":     listReviews(nil, "0"),
		"a padded token":   listReviews(nil, "050"),
		"a deep token":     listReviews(nil, "10001"),
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
