package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// httpReadResult is an engine ReadResult of one read.
const httpReadResult = `{"asked_at":"2026-11-24T10:00:00Z","results":[{"entity":{"type":"Person"}}],"stats":{"reads":1}}`

// readsBackend answers the read routes' RPCs and keeps the last request each received.
type readsBackend struct {
	foundryv1.FoundryServiceClient
	mu   sync.Mutex
	seen map[string]any
	// readGate, when set, holds each ReadFoundryGraph until it is closed.
	readGate chan struct{}
	reading  chan struct{}
	// docSet is the set the document is in; setsReadBy the sets ontology-1 reads.
	docSet     string
	setsReadBy []string
	contentErr error
}

func (b *readsBackend) saw(method string, req any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		b.seen = map[string]any{}
	}
	b.seen[method] = req
}

func (b *readsBackend) last(method string) any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seen[method]
}

func (b *readsBackend) ListFoundryOntologies(_ context.Context, r *foundryv1.ListFoundryOntologiesRequest, _ ...grpc.CallOption) (*foundryv1.ListFoundryOntologiesResponse, error) {
	b.saw("ListFoundryOntologies", r)
	return &foundryv1.ListFoundryOntologiesResponse{
		Ontologies: []*foundryv1.FoundryOntology{{Id: "ontology-1", Name: "Sales"}}, NextPageToken: "next",
	}, nil
}

func (b *readsBackend) GetFoundryOntology(_ context.Context, r *foundryv1.GetFoundryOntologyRequest, _ ...grpc.CallOption) (*foundryv1.FoundryOntologyResponse, error) {
	b.saw("GetFoundryOntology", r)
	return &foundryv1.FoundryOntologyResponse{Ontology: &foundryv1.FoundryOntology{
		Id: "ontology-1", Name: "Sales", CurrentLoad: &foundryv1.FoundryOntologyCurrentLoad{Id: "load-1", Version: "foundry-v2"},
		LastLoad: &foundryv1.FoundryOntologyLastLoad{JobId: "load-1", Status: "succeeded"},
	}}, nil
}

func (b *readsBackend) GetCurrentOntologyVersion(context.Context, *foundryv1.GetCurrentOntologyVersionRequest, ...grpc.CallOption) (*foundryv1.OntologyVersionResponse, error) {
	return &foundryv1.OntologyVersionResponse{}, nil
}

func (b *readsBackend) GetOntologyVersion(_ context.Context, r *foundryv1.GetOntologyVersionRequest, _ ...grpc.CallOption) (*foundryv1.OntologyVersionResponse, error) {
	b.saw("GetOntologyVersion", r)
	definition, err := structpb.NewStruct(map[string]any{"format": "agent_bundle", "status": "done", "ontology_toml": "[types.Person]\nkey = [\"email\"]"})
	if err != nil {
		return nil, err
	}
	return &foundryv1.OntologyVersionResponse{Version: &foundryv1.OntologyVersion{
		Id: r.GetId(), Version: 2, Format: foundryv1.OntologyFormat_ONTOLOGY_FORMAT_AGENT_BUNDLE, Definition: definition,
	}}, nil
}

func (b *readsBackend) ListOntologyVersions(context.Context, *foundryv1.ListOntologyVersionsRequest, ...grpc.CallOption) (*foundryv1.ListOntologyVersionsResponse, error) {
	return &foundryv1.ListOntologyVersionsResponse{}, nil
}

func (b *readsBackend) ListFoundryLoads(context.Context, *foundryv1.ListFoundryLoadsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryLoadsResponse, error) {
	return &foundryv1.ListFoundryLoadsResponse{Loads: []*foundryv1.FoundryLoad{{Id: "load-1", VersionId: "version-2", Version: "foundry-v2", Status: "ready"}}}, nil
}

func (b *readsBackend) ListOntologyGenerations(context.Context, *foundryv1.ListOntologyGenerationsRequest, ...grpc.CallOption) (*foundryv1.ListOntologyGenerationsResponse, error) {
	return &foundryv1.ListOntologyGenerationsResponse{}, nil
}

func (b *readsBackend) ListFoundryDocumentSets(_ context.Context, r *foundryv1.ListFoundryDocumentSetsRequest, _ ...grpc.CallOption) (*foundryv1.ListFoundryDocumentSetsResponse, error) {
	b.saw("ListFoundryDocumentSets", r)
	out := &foundryv1.ListFoundryDocumentSetsResponse{}
	for _, set := range b.setsReadBy {
		out.Sets = append(out.Sets, &foundryv1.FoundryDocumentSet{Id: set, ReadBy: []*foundryv1.FoundryDocumentSetReader{{OntologyId: "ontology-1"}}})
	}
	return out, nil
}

func (b *readsBackend) GetFoundryLoad(_ context.Context, r *foundryv1.GetFoundryLoadRequest, _ ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error) {
	b.saw("GetFoundryLoad", r)
	return &foundryv1.FoundryLoadResponse{Load: &foundryv1.FoundryLoad{Id: r.GetId(), VersionId: "version-2", Version: "foundry-v2", Status: "ready"}}, nil
}

func (b *readsBackend) ReadFoundryGraph(ctx context.Context, r *foundryv1.ReadFoundryGraphRequest, _ ...grpc.CallOption) (*foundryv1.ReadFoundryGraphResponse, error) {
	b.saw("ReadFoundryGraph", r)
	if b.readGate != nil {
		b.reading <- struct{}{}
		select {
		case <-b.readGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &foundryv1.ReadFoundryGraphResponse{ResultJson: httpReadResult}, nil
}

func (b *readsBackend) ListFoundryOntologyDocuments(_ context.Context, r *foundryv1.ListFoundryOntologyDocumentsRequest, _ ...grpc.CallOption) (*foundryv1.ListFoundryDocumentsResponse, error) {
	b.saw("ListFoundryOntologyDocuments", r)
	return &foundryv1.ListFoundryDocumentsResponse{Documents: []*foundryv1.FoundryDocument{{Id: "doc-1", Name: "a.md", Status: "uploaded"}}}, nil
}

func (b *readsBackend) ListFoundryDocuments(_ context.Context, r *foundryv1.ListFoundryDocumentsRequest, _ ...grpc.CallOption) (*foundryv1.ListFoundryDocumentsResponse, error) {
	b.saw("ListFoundryDocuments", r)
	return &foundryv1.ListFoundryDocumentsResponse{}, nil
}

func (b *readsBackend) GetFoundryOntologyDocument(_ context.Context, r *foundryv1.GetFoundryOntologyDocumentRequest, _ ...grpc.CallOption) (*foundryv1.FoundryDocumentResponse, error) {
	b.saw("GetFoundryOntologyDocument", r)
	return &foundryv1.FoundryDocumentResponse{Document: &foundryv1.FoundryDocument{Id: r.GetId(), Name: "a.md", Status: "uploaded", SetId: b.docSet}}, nil
}

func (b *readsBackend) GetFoundryDocumentContent(_ context.Context, r *foundryv1.GetFoundryDocumentContentRequest, _ ...grpc.CallOption) (*foundryv1.GetFoundryDocumentContentResponse, error) {
	b.saw("GetFoundryDocumentContent", r)
	if b.contentErr != nil {
		return nil, b.contentErr
	}
	return &foundryv1.GetFoundryDocumentContentResponse{Text: "# Contract\nSigned."}, nil
}

func (b *readsBackend) ListFoundryQuestions(_ context.Context, r *foundryv1.ListFoundryQuestionsRequest, _ ...grpc.CallOption) (*foundryv1.ListFoundryQuestionsResponse, error) {
	b.saw("ListFoundryQuestions", r)
	return &foundryv1.ListFoundryQuestionsResponse{Questions: []*foundryv1.FoundryQuestion{{Id: "question-1", Status: "answered"}}}, nil
}

// readsHost is a host of the read routes: the ontology each request resolved, and its caller from
// X-Caller. Its identity is a user's unless key is set, and a key is a grantee when grantee is.
type readsHost struct {
	backend  *readsBackend
	handler  http.Handler
	mu       sync.Mutex
	resolved []string
}

func readsFixture(t *testing.T, key, grantee bool) *readsHost {
	t.Helper()
	h := &readsHost{backend: &readsBackend{docSet: "set-1", setsReadBy: []string{"set-1"}}}
	s := &Service{Foundry: h.backend}
	identity := &Identity{ServiceKey: key, WorkspaceID: "workspace-2",
		Authorize: func(_ context.Context, r any, grant Grant, _ ...string) error {
			if req, ok := r.(interface{ GetOntologyId() string }); ok && req.GetOntologyId() != "ontology-1" {
				return status.Error(codes.PermissionDenied, "refused")
			}
			return nil
		},
		Allows:    func(string, string, Grant) bool { return true },
		IsGrantee: func(context.Context) bool { return grantee },
	}
	h.handler = NewHTTPHandler(func(r *http.Request, ontologyID string) (HTTPCall, error) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			return HTTPCall{}, status.Error(codes.Unauthenticated, "secret reason")
		}
		h.mu.Lock()
		h.resolved = append(h.resolved, ontologyID)
		h.mu.Unlock()
		if ontologyID != "" && !ValidIDs([]string{ontologyID}) {
			return HTTPCall{}, FoundryError(status.Error(codes.InvalidArgument, "invalid ontology id"))
		}
		return HTTPCall{Context: r.Context(), Service: s, Identity: identity, WorkspaceID: "workspace-2", Caller: r.Header.Get("X-Caller")}, nil
	})
	return h
}

func (h *readsHost) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer fixture")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func decoded(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
	return out
}

// Each read route calls its Service method with what the request named, and answers with the
// method's JSON.
func TestHTTPReadRoutes(t *testing.T) {
	t.Run("ontologies are the caller's workspace's, resolved with no ontology", func(t *testing.T) {
		h := readsFixture(t, false, false)
		w := h.do(t, "GET", "/v1/ontologies?page_token=p1", "")
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.Equal(t, []string{""}, h.resolved)
		req := h.backend.last("ListFoundryOntologies").(*foundryv1.ListFoundryOntologiesRequest)
		assert.Equal(t, "workspace-2", req.GetWorkspaceId())
		assert.Equal(t, "p1", req.GetPageToken())
		out := decoded(t, w)
		assert.Equal(t, "next", out["next_page_token"])
		assert.Equal(t, "ontology-1", out["ontologies"].([]any)[0].(map[string]any)["id"])
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		assert.NotEmpty(t, w.Header().Get("X-Request-ID"))
	})
	t.Run("an ontology", func(t *testing.T) {
		h := readsFixture(t, false, false)
		w := h.do(t, "GET", "/v1/ontologies/ontology-1", "")
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.Equal(t, []string{"ontology-1"}, h.resolved)
		assert.Equal(t, "Sales", decoded(t, w)["ontology"].(map[string]any)["name"])
	})
	t.Run("its schema", func(t *testing.T) {
		h := readsFixture(t, false, false)
		w := h.do(t, "GET", "/v1/ontologies/ontology-1/schema", "")
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.Equal(t, "load-1", h.backend.last("GetFoundryLoad").(*foundryv1.GetFoundryLoadRequest).GetId(), "it describes the current load's version")
		assert.Contains(t, decoded(t, w), "example_questions")
	})
	t.Run("reads, of the current load or one named", func(t *testing.T) {
		h := readsFixture(t, false, false)
		w := h.do(t, "POST", "/v1/ontologies/ontology-1/reads", `{"reads":[{"entity":{"type":"Person","name":"Ada"}}]}`)
		require.Equal(t, 200, w.Code, w.Body.String())
		req := h.backend.last("ReadFoundryGraph").(*foundryv1.ReadFoundryGraphRequest)
		assert.Equal(t, "load-1", req.GetLoadId())
		assert.JSONEq(t, `[{"entity":{"type":"Person","name":"Ada"}}]`, req.GetReadsJson())
		out := decoded(t, w)
		assert.Equal(t, "load-1", out["load_id"])
		assert.Equal(t, []any{map[string]any{"entity": map[string]any{"type": "Person"}}}, out["result"].(map[string]any)["results"])

		w = h.do(t, "POST", "/v1/ontologies/ontology-1/reads", `{"reads":[{"entity":{"type":"Person","name":"Ada"}}],"load_id":"load-7","valid_at":"2026-01-02"}`)
		require.Equal(t, 200, w.Code, w.Body.String())
		req = h.backend.last("ReadFoundryGraph").(*foundryv1.ReadFoundryGraphRequest)
		assert.Equal(t, "load-7", req.GetLoadId())
		assert.Equal(t, "2026-01-02", req.GetValidAt())
	})
	t.Run("an integer past 2^53 in a read reaches the backend exactly", func(t *testing.T) {
		h := readsFixture(t, false, false)
		w := h.do(t, "POST", "/v1/ontologies/ontology-1/reads", `{"reads":[{"entity":{"type":"Order","key":9007199254740993}}]}`)
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.Contains(t, h.backend.last("ReadFoundryGraph").(*foundryv1.ReadFoundryGraphRequest).GetReadsJson(), "9007199254740993")
	})
	t.Run("a load, waited for or not", func(t *testing.T) {
		h := readsFixture(t, false, false)
		for _, path := range []string{"/v1/ontologies/ontology-1/loads/load-3", "/v1/ontologies/ontology-1/loads/load-3?wait=true", "/v1/ontologies/ontology-1/loads/load-3?wait=false"} {
			w := h.do(t, "GET", path, "")
			require.Equal(t, 200, w.Code, w.Body.String())
			assert.Equal(t, "load-3", decoded(t, w)["load"].(map[string]any)["id"], path)
		}
	})
	t.Run("documents, the ontology's or one set's", func(t *testing.T) {
		h := readsFixture(t, false, false)
		w := h.do(t, "GET", "/v1/ontologies/ontology-1/documents?page_token=p2", "")
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.Equal(t, "p2", h.backend.last("ListFoundryOntologyDocuments").(*foundryv1.ListFoundryOntologyDocumentsRequest).GetPageToken())
		w = h.do(t, "GET", "/v1/ontologies/ontology-1/documents?set_id=set-1", "")
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.Equal(t, "set-1", h.backend.last("ListFoundryDocuments").(*foundryv1.ListFoundryDocumentsRequest).GetSetId())
	})
	t.Run("a document with its text", func(t *testing.T) {
		h := readsFixture(t, false, false)
		w := h.do(t, "GET", "/v1/ontologies/ontology-1/documents/doc-9", "")
		require.Equal(t, 200, w.Code, w.Body.String())
		out := decoded(t, w)
		assert.Equal(t, "# Contract\nSigned.", out["text"])
		assert.Equal(t, "doc-9", out["document"].(map[string]any)["id"])
		assert.Equal(t, contentTrustUntrusted, out["content_trust"])
		assert.Equal(t, "workspace-2", h.backend.last("GetFoundryDocumentContent").(*foundryv1.GetFoundryDocumentContentRequest).GetWorkspaceId())
	})
	t.Run("questions", func(t *testing.T) {
		h := readsFixture(t, false, false)
		w := h.do(t, "GET", "/v1/ontologies/ontology-1/questions?thread_id=thread-1", "")
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.Equal(t, "thread-1", h.backend.last("ListFoundryQuestions").(*foundryv1.ListFoundryQuestionsRequest).GetThreadId())
	})
	t.Run("another ontology is not found", func(t *testing.T) {
		h := readsFixture(t, false, false)
		for _, path := range []string{"/v1/ontologies/ontology-2/schema", "/v1/ontologies/ontology-2/documents/doc-1", "/v1/ontologies/ontology-2/questions"} {
			w := h.do(t, "GET", path, "")
			assert.Equal(t, 404, w.Code, path)
			assert.Contains(t, w.Body.String(), `"code":"not_found"`, path)
			assert.NotContains(t, w.Body.String(), "refused", path)
		}
	})
}

// A request names only what its route takes: an unknown query parameter, a repeated one, a bad
// wait, an empty or malformed read body, and a body of the wrong type are each a 400, before any
// backend call.
func TestHTTPReadRoutesRefuseMalformedRequests(t *testing.T) {
	for _, c := range []struct{ name, method, path, body, message string }{
		{"unknown parameter", "GET", "/v1/ontologies/ontology-1/questions?threadId=t", "", `unknown query parameter \"threadId\"`},
		{"parameter on a route with none", "GET", "/v1/ontologies/ontology-1/schema?x=1", "", `unknown query parameter \"x\"`},
		{"repeated parameter", "GET", "/v1/ontologies?page_token=a&page_token=b", "", "more than once"},
		{"bad wait", "GET", "/v1/ontologies/ontology-1/loads/load-1?wait=yes", "", "wait must be true or false"},
		{"no reads", "POST", "/v1/ontologies/ontology-1/reads", `{"reads":[]}`, "1 to 20 reads"},
		{"unknown field", "POST", "/v1/ontologies/ontology-1/reads", `{"reads":[{"entity":{}}],"load":"x"}`, "containing reads"},
		{"two objects", "POST", "/v1/ontologies/ontology-1/reads", `{"reads":[{"entity":{}}]}{}`, "exactly one JSON object"},
		{"a malformed read", "POST", "/v1/ontologies/ontology-1/reads", `{"reads":[{"nonsense":{}}]}`, ""},
		{"too many reads", "POST", "/v1/ontologies/ontology-1/reads", `{"reads":[` + strings.TrimSuffix(strings.Repeat(`{"entity":{"type":"P","name":"a"}},`, 21), ",") + `]}`, ""},
		{"a semicolon in the query", "GET", "/v1/ontologies/ontology-1/documents?set_id=set-1;x=1", "", "query string is malformed"},
		{"a bad escape in the query", "GET", "/v1/ontologies/ontology-1/questions?thread_id=%zz", "", "query string is malformed"},
		{"malformed load", "GET", "/v1/ontologies/ontology-1/loads/bad!id", "", ""},
		{"malformed document", "GET", "/v1/ontologies/ontology-1/documents/bad!id", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := readsFixture(t, false, false)
			w := h.do(t, c.method, c.path, c.body)
			assert.Equal(t, 400, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), `"code":"invalid_arguments"`)
			assert.Contains(t, w.Body.String(), c.message)
			assert.Nil(t, h.backend.last("ReadFoundryGraph"))
			assert.Nil(t, h.backend.last("GetFoundryLoad"))
			assert.Nil(t, h.backend.last("ListFoundryQuestions"))
		})
	}
	t.Run("a read without JSON", func(t *testing.T) {
		h := readsFixture(t, false, false)
		r := httptest.NewRequest("POST", "/v1/ontologies/ontology-1/reads", strings.NewReader(`{"reads":[]}`))
		r.Header.Set("Authorization", "Bearer fixture")
		r.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		assert.Equal(t, 400, w.Code)
		assert.Contains(t, w.Body.String(), "Content-Type must be application/json")
	})
	t.Run("an anonymous caller is told only that", func(t *testing.T) {
		h := readsFixture(t, false, false)
		r := httptest.NewRequest("GET", "/v1/ontologies?bogus=1", nil)
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		assert.Equal(t, 401, w.Code)
		assert.NotContains(t, w.Body.String(), "bogus")
	})
}

// Every route answers another method with 405 and the methods it takes, HEAD included, and a path
// no route has with 404, each in the contract's error shape.
func TestHTTPReadRoutesMethods(t *testing.T) {
	h := readsFixture(t, false, false)
	for _, c := range []struct{ method, path, allow string }{
		{"POST", "/v1/ontologies", "GET"},
		{"DELETE", "/v1/ontologies/ontology-1", "GET"},
		{"GET", "/v1/ontologies/ontology-1/reads", "POST"},
		{"HEAD", "/v1/ontologies/ontology-1/schema", "GET"},
		{"PUT", "/v1/ontologies/ontology-1/documents/doc-1", "GET"},
		{"POST", "/v1/ontologies/ontology-1/questions", "GET"},
		{"GET", "/v1/ontologies/ontology-1/ask", "POST"},
	} {
		w := h.do(t, c.method, c.path, "")
		assert.Equal(t, 405, w.Code, c)
		assert.Equal(t, c.allow, w.Header().Get("Allow"), c)
		if c.method != "HEAD" {
			assert.Contains(t, w.Body.String(), `"code":"method_not_allowed"`, c)
		}
	}
	for _, path := range []string{"/v1/ontologies/ontology-1/loads", "/v1/ontologies/ontology-1/entities", "/v1/workspaces"} {
		w := h.do(t, "GET", path, "")
		assert.Equal(t, 404, w.Code, path)
		assert.Contains(t, w.Body.String(), `"code":"not_found"`, path)
	}
	assert.Empty(t, h.resolved, "nothing was resolved for a route that isn't there")
}

// A document's text reaches a key only from a set its ontology reads; a grantee's is checked by the
// backend, which the key's lookup of the sets would refuse; a binary document says so.
func TestHTTPDocumentText(t *testing.T) {
	t.Run("a key reads a document of a set the ontology reads", func(t *testing.T) {
		h := readsFixture(t, true, false)
		w := h.do(t, "GET", "/v1/ontologies/ontology-1/documents/doc-1", "")
		require.Equal(t, 200, w.Code, w.Body.String())
	})
	t.Run("a key is refused a document of another set, before its text is read", func(t *testing.T) {
		h := readsFixture(t, true, false)
		h.backend.docSet = "set-other"
		w := h.do(t, "GET", "/v1/ontologies/ontology-1/documents/doc-1", "")
		assert.Equal(t, 404, w.Code, w.Body.String())
		assert.Nil(t, h.backend.last("GetFoundryDocumentContent"))
	})
	t.Run("a grantee is left to the backend's check", func(t *testing.T) {
		h := readsFixture(t, true, true)
		h.backend.docSet = "set-other"
		w := h.do(t, "GET", "/v1/ontologies/ontology-1/documents/doc-1", "")
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.Nil(t, h.backend.last("ListFoundryDocumentSets"))
	})
	t.Run("a binary document has no text", func(t *testing.T) {
		h := readsFixture(t, false, false)
		h.backend.contentErr = status.Error(codes.FailedPrecondition, "Parquet documents have no text; use :bytes")
		w := h.do(t, "GET", "/v1/ontologies/ontology-1/documents/doc-1", "")
		assert.Equal(t, 412, w.Code)
		assert.Contains(t, w.Body.String(), "Parquet documents have no text")
	})
	t.Run("the backend's refusal of a grantee is not found", func(t *testing.T) {
		h := readsFixture(t, true, true)
		h.backend.contentErr = status.Error(codes.NotFound, "document not found")
		w := h.do(t, "GET", "/v1/ontologies/ontology-1/documents/doc-1", "")
		assert.Equal(t, 404, w.Code)
	})
}

// A caller makes httpCallerBurst requests at once and then httpCallerRate a second, and runs at
// most httpCallerReads reads at once; another caller, and a host with one caller, are not held to
// what it spent.
func TestHTTPCallerLimits(t *testing.T) {
	t.Run("requests", func(t *testing.T) {
		h := readsFixture(t, false, false)
		get := func(caller string) int {
			r := httptest.NewRequest("GET", "/v1/ontologies/ontology-1/questions", nil)
			r.Header.Set("Authorization", "Bearer fixture")
			r.Header.Set("X-Caller", caller)
			w := httptest.NewRecorder()
			h.handler.ServeHTTP(w, r)
			if w.Code == 429 {
				assert.Contains(t, w.Body.String(), "requests a second")
			}
			return w.Code
		}
		for i := range httpCallerBurst {
			require.Equal(t, 200, get("key-a"), i)
		}
		assert.Equal(t, 429, get("key-a"))
		assert.Equal(t, 200, get("key-b"), "another caller has its own")
		for range 2 * httpCallerBurst {
			require.Equal(t, 200, get(""), "a host with one caller has no limit")
		}
	})
	t.Run("reads at once", func(t *testing.T) {
		h := readsFixture(t, false, false)
		h.backend.readGate, h.backend.reading = make(chan struct{}), make(chan struct{}, httpCallerReads+1)
		read := func(caller string) chan int {
			done := make(chan int, 1)
			go func() {
				r := httptest.NewRequest("POST", "/v1/ontologies/ontology-1/reads", strings.NewReader(`{"reads":[{"entity":{"type":"P","name":"a"}}]}`))
				r.Header.Set("Authorization", "Bearer fixture")
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-Caller", caller)
				w := httptest.NewRecorder()
				h.handler.ServeHTTP(w, r)
				done <- w.Code
			}()
			return done
		}
		var running []chan int
		for range httpCallerReads {
			running = append(running, read("key-a"))
			<-h.backend.reading
		}
		third := <-read("key-a")
		assert.Equal(t, 429, third)
		other := read("key-b")
		<-h.backend.reading
		close(h.backend.readGate)
		for _, done := range append(running, other) {
			assert.Equal(t, 200, <-done)
		}
		assert.Equal(t, 200, <-read("key-a"), "a finished read gives its slot back")
	})
	t.Run("a full replica forgets idle callers first", func(t *testing.T) {
		limits := newHTTPCallerLimits()
		for i := range httpCallerCapacity {
			_, err := limits.admit(fmt.Sprintf("idle-%d", i), false)
			require.NoError(t, err)
		}
		// Every caller spent one request a moment ago, so none is idle yet: a new one is refused,
		// never let through unlimited.
		_, err := limits.admit("new", false)
		require.Error(t, err)
		assert.Equal(t, errCodeResourceExhausted, stableErrorCode(err))
		assert.Len(t, limits.callers, httpCallerCapacity)
		assert.NotContains(t, limits.callers, "new")
		for _, c := range limits.callers {
			c.requests = rate.NewLimiter(httpCallerRate, httpCallerBurst) // as if each had waited to refill
		}
		release, err := limits.admit("reader", true)
		require.NoError(t, err)
		assert.Contains(t, limits.callers, "reader")
		assert.Len(t, limits.callers, 1)
		release()
		assert.Zero(t, limits.callers["reader"].reads)
	})
}
