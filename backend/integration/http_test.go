package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type httpBackend struct {
	foundryv1.FoundryServiceClient
	mu           sync.Mutex
	question     *foundryv1.FoundryQuestion
	queued       int
	queuedSignal chan struct{}
	err          error
}

func (b *httpBackend) AskFoundryQuestion(_ context.Context, r *foundryv1.AskFoundryQuestionRequest, _ ...grpc.CallOption) (*foundryv1.FoundryQuestionResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	if b.question.Question != "" && b.question.Question != strings.TrimSpace(r.Question) {
		return nil, status.Error(codes.InvalidArgument, "client_token was already used for another question")
	}
	if b.question.Question == "" {
		b.queued++
		b.question.Question = strings.TrimSpace(r.Question)
		if b.queuedSignal != nil {
			close(b.queuedSignal)
		}
	}
	return &foundryv1.FoundryQuestionResponse{Question: proto.Clone(b.question).(*foundryv1.FoundryQuestion)}, nil
}
func (b *httpBackend) GetFoundryQuestion(_ context.Context, r *foundryv1.GetFoundryQuestionRequest, _ ...grpc.CallOption) (*foundryv1.FoundryQuestionResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Id != b.question.Id {
		return nil, status.Error(codes.NotFound, "question not found")
	}
	return &foundryv1.FoundryQuestionResponse{Question: proto.Clone(b.question).(*foundryv1.FoundryQuestion)}, nil
}

func httpFixture(t *testing.T, state string) (http.Handler, *httpBackend) {
	t.Helper()
	b := &httpBackend{question: &foundryv1.FoundryQuestion{Id: "question-1", LoadId: "load-original", Status: state,
		ResultJson: `{"status":"answered","answer":{"blocks":[]},"citations":[],"queries":[{"query":{"reads":[]},"output":{"secret":"large rows"}}]}`}}
	s := &Service{Foundry: b, Wait: 5 * time.Millisecond, Polls: []time.Duration{time.Millisecond}}
	i := &Identity{Authorize: func(_ context.Context, r any, grant Grant, _ ...string) error {
		req := r.(interface {
			GetWorkspaceId() string
			GetOntologyId() string
		})
		if req.GetWorkspaceId() != "workspace-2" || req.GetOntologyId() != "ontology-1" || grant != QueryRead {
			return status.Error(codes.PermissionDenied, "refused")
		}
		return nil
	}}
	h := NewHTTPHandler(func(r *http.Request, _ string) (context.Context, *Service, *Identity, string, error) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			return r.Context(), nil, nil, "", status.Error(codes.Unauthenticated, "secret reason")
		}
		return r.Context(), s, i, "workspace-2", nil
	})
	return h, b
}

func httpCall(h http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer fixture")
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHTTPQuestionLifecycle(t *testing.T) {
	h, b := httpFixture(t, "queued")
	w := httpCall(h, "POST", "/v1/ontologies/ontology-1/ask", `{"question":"A question"}`, "retry-key")
	require.Equal(t, 202, w.Code, w.Body.String())
	require.Equal(t, "/v1/ontologies/ontology-1/questions/question-1", w.Header().Get("Location"))
	require.Equal(t, "1", w.Header().Get("Retry-After"))
	w = httpCall(h, "POST", "/v1/ontologies/ontology-1/ask", `{"question":"A question"}`, "retry-key")
	require.Equal(t, 202, w.Code)
	require.Equal(t, 1, b.queued)
	b.mu.Lock()
	b.question.Status = "answered"
	b.mu.Unlock()
	w = httpCall(h, "GET", "/v1/ontologies/ontology-1/questions/question-1", "", "")
	require.Equal(t, 200, w.Code, w.Body.String())
	var out FoundryQuestionOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.True(t, out.Done)
	assert.Equal(t, "load-original", out.LoadID)
	assert.NotContains(t, w.Body.String(), "large rows")
	assert.Contains(t, w.Body.String(), "citations")
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	w = httpCall(h, "POST", "/v1/ontologies/ontology-1/ask", `{"question":"Changed question"}`, "retry-key")
	assert.Equal(t, 400, w.Code)
}

func TestHTTPTerminalStates(t *testing.T) {
	for _, state := range []string{"answered", "cannot_answer", "failed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			h, _ := httpFixture(t, state)
			w := httpCall(h, "POST", "/v1/ontologies/ontology-1/ask", `{"question":"Question"}`, "key")
			assert.Equal(t, 200, w.Code)
			assert.Contains(t, w.Body.String(), `"done":true`)
		})
	}
}

func TestHTTPValidationAndIsolation(t *testing.T) {
	h, b := httpFixture(t, "answered")
	for _, body := range []string{`{}`, `{"question":" "}`, `{"question":"q","workspace_id":"elsewhere"}`, `{"question":"q"}{}`, `{"question":"` + strings.Repeat("x", 65<<10) + `"}`} {
		w := httpCall(h, "POST", "/v1/ontologies/ontology-1/ask", body, "key")
		assert.Equal(t, 400, w.Code, w.Body.String())
	}
	// Bytes that aren't UTF-8 can't reach gRPC, and would fail there for good as a 500.
	for _, key := range []string{"", strings.Repeat("x", 65), "\xff"} {
		assert.Equal(t, 400, httpCall(h, "POST", "/v1/ontologies/ontology-1/ask", `{"question":"q"}`, key).Code)
	}
	assert.Equal(t, 403, httpCall(h, "POST", "/v1/ontologies/ontology-other/ask", `{"question":"q"}`, "key").Code)
	r := httptest.NewRequest("POST", "/v1/ontologies/ontology-1/ask", strings.NewReader(`{"question":"q"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assert.Equal(t, 401, w.Code)
	assert.NotContains(t, w.Body.String(), "secret reason")
	assert.Equal(t, 0, b.queued)
	assert.Equal(t, 404, httpCall(h, "GET", "/metrics", "", "").Code)
	assert.Equal(t, 405, httpCall(h, "DELETE", "/v1/ontologies/ontology-1/ask", "", "").Code)
}

// HEAD matches the GET route, but reads nothing, so it must not look pending.
func TestHTTPHeadIsRefused(t *testing.T) {
	h, b := httpFixture(t, "queued")
	for _, ontology := range []string{"ontology-1", "ontology-other"} {
		w := httpCall(h, "HEAD", "/v1/ontologies/"+ontology+"/questions/question-1", "", "")
		assert.Equal(t, 405, w.Code)
		assert.Equal(t, "GET", w.Header().Get("Allow"))
		assert.Empty(t, w.Header().Get("Location"))
		assert.Empty(t, w.Header().Get("Retry-After"))
	}
	assert.Equal(t, 0, b.queued)
}

func TestHTTPBackendErrors(t *testing.T) {
	for code, want := range map[codes.Code]int{codes.FailedPrecondition: 412, codes.ResourceExhausted: 429, codes.Unavailable: 503, codes.Internal: 500} {
		t.Run(code.String(), func(t *testing.T) {
			h, b := httpFixture(t, "answered")
			b.err = status.Error(code, "private backend detail")
			w := httpCall(h, "POST", "/v1/ontologies/ontology-1/ask", `{"question":"q"}`, "key")
			assert.Equal(t, want, w.Code)
			if want >= 500 {
				assert.NotContains(t, w.Body.String(), "private backend detail")
			}
		})
	}
}

func TestHTTPConcurrentRetries(t *testing.T) {
	h, b := httpFixture(t, "answered")
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			w := httpCall(h, "POST", "/v1/ontologies/ontology-1/ask", `{"question":"q"}`, "one-key")
			assert.Equal(t, 200, w.Code)
		})
	}
	wg.Wait()
	assert.Equal(t, 1, b.queued)
}

func TestHTTPDisconnectKeepsQueuedQuestion(t *testing.T) {
	h, b := httpFixture(t, "queued")
	b.queuedSignal = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/ontologies/ontology-1/ask", strings.NewReader(`{"question":"q"}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer fixture")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "same-question")
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}()
	select {
	case <-b.queuedSignal:
	case <-time.After(time.Second):
		t.Fatal("question was not queued")
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop on disconnect")
	}
	// The transport never cancels the job. The worker can finish after its
	// caller disconnects, and another request can retrieve the same question.
	b.mu.Lock()
	b.question.Status = "answered"
	b.mu.Unlock()
	w := httpCall(h, "GET", "/v1/ontologies/ontology-1/questions/question-1", "", "")
	require.Equal(t, 200, w.Code)
	assert.Equal(t, 1, b.queued)
	assert.Contains(t, w.Body.String(), `"done":true`)
}
