package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// reviewsBackend answers list_reviews with one review of each kind.
type reviewsBackend struct {
	foundryv1.FoundryServiceClient
	got *foundryv1.ListFoundryReviewsRequest
}

func (b *reviewsBackend) ListFoundryReviews(_ context.Context, r *foundryv1.ListFoundryReviewsRequest, _ ...grpc.CallOption) (*foundryv1.ListFoundryReviewsResponse, error) {
	b.got = r
	confidence := 0.4
	return &foundryv1.ListFoundryReviewsResponse{LoadId: "load-1", NextPageToken: "50", Reviews: []*foundryv1.FoundryReview{
		{ReviewKey: "r-resolve", Reason: "unresolved", Settle: foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_RESOLVE, Type: "Customer", DocId: "doc-a",
			NormName: "globexx", Candidates: []string{"scus_1"}, Quote: "Globexx", Span: &foundryv1.FoundryReviewSpan{Section: 1, Start: 4, End: 11}, Confidence: &confidence},
		{ReviewKey: "r-prop", Reason: "prop_conflict", Settle: foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_PROP, Type: "Customer", EntityKey: "scus_1", Prop: "seats",
			Values: []*foundryv1.FoundryReviewValue{{ValueJson: "12345678901234567890", DocId: "doc-b"}, {ValueJson: "not json"}, {ValueJson: `"GBP"`, Quote: "in GBP"}}},
		{ReviewKey: "r-held", Reason: "low_confidence", Settle: foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_RELEASE, Link: "promised_to",
			StatementJson: `{"subject":{}}`, StatementHash: strings.Repeat("a", 64)},
		{ReviewKey: "r-other", Reason: "no_link"},
	}}, nil
}

func TestListFoundryReviewsTool(t *testing.T) {
	b := &reviewsBackend{}
	s := &Service{Foundry: b, FoundryURL: "http://localhost:3000"}
	out := called(t, s, toolFoundryListReviews, map[string]any{"workspace_id": "ws1", "ontology_id": "ont1", "review_keys": []string{"r-resolve"}})
	assert.True(t, proto.Equal(b.got, &foundryv1.ListFoundryReviewsRequest{WorkspaceId: "ws1", OntologyId: "ont1", ReviewKeys: []string{"r-resolve"}}))
	assert.Equal(t, contentTrustUntrusted, out["content_trust"])
	assert.Equal(t, "load-1", out["load_id"])
	assert.Equal(t, "50", out["next_page_token"])
	reviews := out["reviews"].([]any)
	require.Len(t, reviews, 4)
	resolve := reviews[0].(map[string]any)
	assert.Equal(t, "resolve", resolve["settle"])
	assert.Equal(t, []any{"scus_1"}, resolve["candidates"])
	assert.EqualValues(t, 1, resolve["section"])
	assert.EqualValues(t, 11, resolve["end"])
	assert.EqualValues(t, 0.4, resolve["confidence"])
	prop := reviews[1].(map[string]any)
	assert.Equal(t, "prop", prop["settle"])
	values := prop["values"].([]any)
	require.Len(t, values, 2, "a value that isn't JSON is left out")
	assert.Equal(t, map[string]any{"value": 12345678901234567890.0, "doc_id": "doc-b"}, values[0])
	assert.Equal(t, map[string]any{"value": "GBP", "quote": "in GBP"}, values[1])
	held := reviews[2].(map[string]any)
	assert.Equal(t, "release", held["settle"])
	assert.Equal(t, strings.Repeat("a", 64), held["statement_hash"])
	assert.Equal(t, `{"subject":{}}`, held["statement"])
	assert.Equal(t, "none", reviews[3].(map[string]any)["settle"])

	// Every digit of an integer is kept, as JSON writes it.
	review := FoundryReview(&foundryv1.FoundryReview{Values: []*foundryv1.FoundryReviewValue{{ValueJson: "12345678901234567890"}}})
	assert.Equal(t, "12345678901234567890", review.Values[0].Value.(interface{ String() string }).String())
}

func TestListFoundryReviewsToolRefusals(t *testing.T) {
	b := &reviewsBackend{}
	s := &Service{Foundry: b, FoundryURL: "http://localhost:3000"}
	for name, args := range map[string]map[string]any{
		"no ontology":   {"workspace_id": "ws1"},
		"a bad id":      {"workspace_id": "ws1", "ontology_id": "a b"},
		"too many keys": {"workspace_id": "ws1", "ontology_id": "ont1", "review_keys": make([]string, 51)},
		"a long token":  {"workspace_id": "ws1", "ontology_id": "ont1", "page_token": strings.Repeat("1", 17)},
	} {
		result, err := connect(t, s, reader()).CallTool(context.Background(), &mcp.CallToolParams{Name: toolFoundryListReviews, Arguments: args})
		require.NoError(t, err, name)
		assert.True(t, result.IsError, name)
	}
	assert.Nil(t, b.got, "nothing reached the backend")

	// The host authorizes it as the method it calls.
	var methods []string
	denied := reader()
	denied.Authorize = func(_ context.Context, _ any, _ Grant, m ...string) error {
		methods = append(methods, m...)
		return errCodeNotFound
	}
	result, err := connect(t, s, denied).CallTool(context.Background(), &mcp.CallToolParams{Name: toolFoundryListReviews,
		Arguments: map[string]any{"workspace_id": "ws1", "ontology_id": "ont1"}})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Equal(t, []string{"ListFoundryReviews"}, methods)
	assert.Nil(t, b.got)
}

// settle_review takes exactly one decision, each as the backend takes it.
func TestReviewChoice(t *testing.T) {
	hash := strings.Repeat("0f", 32)
	text, integer, bad, number, yes := "GBP", "42", "4.2", 4.2, true
	for _, tc := range []struct {
		name     string
		decision ReviewDecisionPayload
		want     *foundryv1.FoundryReviewChoice
	}{
		{"an entity", ReviewDecisionPayload{Resolve: &ReviewResolvePayload{EntityKey: "scus_1"}}, &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Resolve{
			Resolve: &foundryv1.FoundryResolveChoice{Target: &foundryv1.FoundryResolveChoice_EntityKey{EntityKey: "scus_1"}}}}},
		{"none", ReviewDecisionPayload{Resolve: &ReviewResolvePayload{NotInData: true}}, &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Resolve{
			Resolve: &foundryv1.FoundryResolveChoice{Target: &foundryv1.FoundryResolveChoice_NotInData{NotInData: true}}}}},
		{"text", ReviewDecisionPayload{Prop: &PropValuePayload{Text: &text}}, &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Prop{
			Prop: &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Text{Text: text}}}}},
		{"an integer", ReviewDecisionPayload{Prop: &PropValuePayload{Integer: &integer}}, &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Prop{
			Prop: &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Integer{Integer: 42}}}}},
		{"a number", ReviewDecisionPayload{Prop: &PropValuePayload{Number: &number}}, &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Prop{
			Prop: &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Number{Number: number}}}}},
		{"a boolean", ReviewDecisionPayload{Prop: &PropValuePayload{Boolean: &yes}}, &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Prop{
			Prop: &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Boolean{Boolean: true}}}}},
		{"no value", ReviewDecisionPayload{Prop: &PropValuePayload{Null: true}}, &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Prop{
			Prop: &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Null{}}}}},
		{"a release", ReviewDecisionPayload{Release: &StatementPayload{StatementHash: hash}}, &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Release{
			Release: &foundryv1.FoundryReleaseChoice{StatementHash: hash}}}},
		{"a reject", ReviewDecisionPayload{Reject: &StatementPayload{StatementHash: hash}}, &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Reject{
			Reject: &foundryv1.FoundryRejectChoice{StatementHash: hash}}}},
		{"nothing", ReviewDecisionPayload{}, nil},
		{"two kinds", ReviewDecisionPayload{Resolve: &ReviewResolvePayload{NotInData: true}, Release: &StatementPayload{StatementHash: hash}}, nil},
		{"both resolves", ReviewDecisionPayload{Resolve: &ReviewResolvePayload{EntityKey: "k", NotInData: true}}, nil},
		{"an empty resolve", ReviewDecisionPayload{Resolve: &ReviewResolvePayload{}}, nil},
		{"two values", ReviewDecisionPayload{Prop: &PropValuePayload{Text: &text, Null: true}}, nil},
		{"no value given", ReviewDecisionPayload{Prop: &PropValuePayload{}}, nil},
		{"a fraction as an integer", ReviewDecisionPayload{Prop: &PropValuePayload{Integer: &bad}}, nil},
		{"a short hash", ReviewDecisionPayload{Release: &StatementPayload{StatementHash: "ab"}}, nil},
		{"an upper-case hash", ReviewDecisionPayload{Reject: &StatementPayload{StatementHash: strings.ToUpper(hash)}}, nil},
	} {
		got, err := ReviewChoice(tc.decision)
		if tc.want == nil {
			assert.Error(t, err, tc.name)
			continue
		}
		require.NoError(t, err, tc.name)
		assert.True(t, proto.Equal(tc.want, got), tc.name)
	}
}
