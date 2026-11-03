package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	// panicOnGet crashes the nth GetFoundryQuestion; 0 never does.
	panicOnGet, gets int
	// tokens are the client tokens asked with, in order.
	tokens       []string
	mu           sync.Mutex
	question     *foundryv1.FoundryQuestion
	queued       int
	queuedSignal chan struct{}
	err          error
}

func (b *httpBackend) AskFoundryQuestion(_ context.Context, r *foundryv1.AskFoundryQuestionRequest, _ ...grpc.CallOption) (*foundryv1.FoundryQuestionResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = append(b.tokens, r.ClientToken)
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
	if b.gets++; b.gets == b.panicOnGet {
		panic("a bug on the way to the backend")
	}
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
	h := NewHTTPHandler(func(r *http.Request, ontologyID string) (HTTPCall, error) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			return HTTPCall{}, status.Error(codes.Unauthenticated, "secret reason")
		}
		if !ValidIDs([]string{ontologyID}) {
			return HTTPCall{}, FoundryError(status.Error(codes.InvalidArgument, "invalid ontology id"))
		}
		return HTTPCall{Context: r.Context(), Service: s, Identity: i, WorkspaceID: "workspace-2"}, nil
	})
	return h, b
}

// httpObserved is the fixture with what its host's Done was told, and a short body timeout.
type httpObserved struct {
	handler http.Handler
	backend *httpBackend
	service *Service
	mu      sync.Mutex
	seen    []HTTPOutcome
}

func httpObservedFixture(t *testing.T, state string, bodyTimeout time.Duration) *httpObserved {
	t.Helper()
	o := &httpObserved{backend: &httpBackend{question: &foundryv1.FoundryQuestion{Id: "question-1", Status: state}}}
	o.service = &Service{Foundry: o.backend, Wait: 5 * time.Millisecond, Polls: []time.Duration{time.Millisecond}}
	identity := &Identity{Authorize: func(_ context.Context, r any, _ Grant, _ ...string) error {
		if r.(interface{ GetOntologyId() string }).GetOntologyId() != "ontology-1" {
			return status.Error(codes.PermissionDenied, "refused")
		}
		return nil
	}}
	done := func(outcome HTTPOutcome) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.seen = append(o.seen, outcome)
	}
	o.handler = newHTTPHandler(func(r *http.Request, ontologyID string) (HTTPCall, error) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			return HTTPCall{Done: done}, status.Error(codes.Unauthenticated, "secret reason")
		}
		if !ValidIDs([]string{ontologyID}) {
			return HTTPCall{Done: done}, FoundryError(status.Error(codes.InvalidArgument, "invalid ontology id"))
		}
		return HTTPCall{Context: r.Context(), Service: o.service, Identity: identity, WorkspaceID: "workspace-2", Caller: r.Header.Get("X-Caller"), Done: done}, nil
	}, bodyTimeout)
	return o
}

// outcome waits for the request's one outcome: Done runs once the response is written.
func (o *httpObserved) outcome(t *testing.T) HTTPOutcome {
	t.Helper()
	var seen []HTTPOutcome
	require.Eventually(t, func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		seen = append(seen[:0], o.seen...)
		return len(seen) > 0
	}, 5*time.Second, time.Millisecond)
	require.Len(t, seen, 1)
	return seen[0]
}

func httpObservedRequest(ctx context.Context, method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer fixture")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "key")
	return r
}

const (
	httpAskPath      = "/v1/ontologies/ontology-1/ask"
	httpQuestionPath = "/v1/ontologies/ontology-1/questions/question-1"
)

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
	assert.Equal(t, 404, httpCall(h, "POST", "/v1/ontologies/ontology-other/ask", `{"question":"q"}`, "key").Code)
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
	for code, want := range map[codes.Code]int{codes.AlreadyExists: 409, codes.FailedPrecondition: 412, codes.ResourceExhausted: 429, codes.Unavailable: 503, codes.Internal: 500} {
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

// A host learns how each request ended from its Done, with nothing to parse.
func TestHTTPOutcome(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, state, method, path string
		backendErr                error
		want                      HTTPOutcome
	}{
		{name: "asked and answered", state: "answered", method: "POST", path: httpAskPath, want: HTTPOutcome{Tool: HTTPAskTool, Status: 200, QuestionID: "question-1"}},
		{name: "asked and running", state: "queued", method: "POST", path: httpAskPath, want: HTTPOutcome{Tool: HTTPAskTool, Status: 202, QuestionID: "question-1"}},
		{name: "a question that failed is a request that succeeded", state: "failed", method: "GET", path: httpQuestionPath, want: HTTPOutcome{Tool: HTTPGetQuestionTool, Status: 200, QuestionID: "question-1"}},
		{name: "a question read", state: "answered", method: "GET", path: httpQuestionPath, want: HTTPOutcome{Tool: HTTPGetQuestionTool, Status: 200, QuestionID: "question-1"}},
		{name: "a question no one has", state: "answered", method: "GET", path: "/v1/ontologies/ontology-1/questions/question-2", want: HTTPOutcome{Tool: HTTPGetQuestionTool, Status: 404, Code: "not_found"}},
		{name: "refused", state: "answered", method: "POST", path: "/v1/ontologies/ontology-2/ask", want: HTTPOutcome{Tool: HTTPAskTool, Status: 404, Code: "not_found"}},
		{name: "a spent budget", state: "answered", method: "POST", path: httpAskPath, backendErr: status.Error(codes.FailedPrecondition, "spent"), want: HTTPOutcome{Tool: HTTPAskTool, Status: 412, Code: "failed_precondition"}},
		{name: "an outage", state: "answered", method: "POST", path: httpAskPath, backendErr: status.Error(codes.Unavailable, "private"), want: HTTPOutcome{Tool: HTTPAskTool, Status: 503, Code: "backend_unavailable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := httpObservedFixture(t, tc.state, httpBodyTimeout)
			o.backend.err = tc.backendErr
			w := httptest.NewRecorder()
			o.handler.ServeHTTP(w, httpObservedRequest(ctx, tc.method, tc.path, `{"question":"q"}`))
			got := o.outcome(t)
			assert.Equal(t, w.Code, got.Status)
			assert.Equal(t, w.Header().Get("X-Request-ID"), got.RequestID)
			got.RequestID = ""
			assert.Equal(t, tc.want, got)
		})
	}
	t.Run("a refusal in resolving, for a host that audits it", func(t *testing.T) {
		o := httpObservedFixture(t, "answered", httpBodyTimeout)
		r := httpObservedRequest(ctx, "POST", httpAskPath, `{"question":"q"}`)
		r.Header.Del("Authorization")
		o.handler.ServeHTTP(httptest.NewRecorder(), r)
		got := o.outcome(t)
		assert.Equal(t, 401, got.Status)
		assert.Equal(t, "unauthenticated", got.Code)
	})
	t.Run("HEAD is refused before anything is resolved, so it has no outcome", func(t *testing.T) {
		o := httpObservedFixture(t, "answered", httpBodyTimeout)
		w := httptest.NewRecorder()
		o.handler.ServeHTTP(w, httpObservedRequest(ctx, "HEAD", httpQuestionPath, ""))
		assert.Equal(t, 405, w.Code)
		assert.Empty(t, o.seen)
	})
}

// A client that hangs up during the wait leaves a question running and spending, so the question's
// ID must reach the host's audit.
func TestHTTPHangUpDuringTheWaitKeepsTheQuestionID(t *testing.T) {
	for name, request := range map[string]func(context.Context) *http.Request{
		"ask": func(ctx context.Context) *http.Request {
			return httpObservedRequest(ctx, "POST", httpAskPath, `{"question":"q"}`)
		},
		"get": func(ctx context.Context) *http.Request { return httpObservedRequest(ctx, "GET", httpQuestionPath, "") },
	} {
		t.Run(name, func(t *testing.T) {
			o := httpObservedFixture(t, "running", httpBodyTimeout)
			o.service.Wait = time.Hour
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			time.AfterFunc(20*time.Millisecond, cancel)
			o.handler.ServeHTTP(httptest.NewRecorder(), request(ctx))
			got := o.outcome(t)
			assert.Equal(t, HTTPClientDisconnected, got.Code)
			assert.Equal(t, "question-1", got.QuestionID)
		})
	}
}

// A crash during the wait leaves a question running and spending, as a hang-up does, so its ID must
// reach the host's audit too. net/http recovers the panic and drops the connection, so a host's Done
// is the only record of it.
func TestHTTPPanicKeepsTheQuestionID(t *testing.T) {
	ask := func() *http.Request {
		return httpObservedRequest(context.Background(), "POST", httpAskPath, `{"question":"q"}`)
	}
	get := func() *http.Request { return httpObservedRequest(context.Background(), "GET", httpQuestionPath, "") }
	for _, tc := range []struct {
		name       string
		request    func() *http.Request
		panicOnGet int
		question   string
	}{
		{name: "the wait after asking", request: ask, panicOnGet: 1, question: "question-1"},
		{name: "the wait after reading", request: get, panicOnGet: 2, question: "question-1"},
		{name: "the first read, before any question is known", request: get, panicOnGet: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := httpObservedFixture(t, "running", httpBodyTimeout)
			o.service.Wait = time.Hour
			o.backend.panicOnGet = tc.panicOnGet
			assert.Panics(t, func() { o.handler.ServeHTTP(httptest.NewRecorder(), tc.request()) })
			got := o.outcome(t)
			assert.Zero(t, got.Status, "a host tells a panic by this")
			assert.Empty(t, got.Code)
			assert.Equal(t, tc.question, got.QuestionID)
		})
	}
}

// A hang-up names only what it caused: a refusal the server made stays that refusal in the audit.
func TestHTTPHangUpDoesNotHideARefusal(t *testing.T) {
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name, path string
		backendErr error
		anonymous  bool
		want       string
	}{
		{name: "no credential", path: httpAskPath, anonymous: true, want: "unauthenticated"},
		{name: "no access", path: "/v1/ontologies/ontology-2/ask", want: "not_found"},
		{name: "a limit", path: httpAskPath, backendErr: status.Error(codes.ResourceExhausted, "limit"), want: "resource_exhausted"},
		{name: "a precondition", path: httpAskPath, backendErr: status.Error(codes.FailedPrecondition, "spent"), want: "failed_precondition"},
		{name: "a call the hang-up cut", path: httpAskPath, backendErr: status.Error(codes.Canceled, "cancelled"), want: HTTPClientDisconnected},
		{name: "an outage seen by no one", path: httpAskPath, backendErr: status.Error(codes.Unavailable, "down"), want: HTTPClientDisconnected},
		{name: "an internal error seen by no one", path: httpAskPath, backendErr: status.Error(codes.Internal, "boom"), want: HTTPClientDisconnected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := httpObservedFixture(t, "answered", httpBodyTimeout)
			o.backend.err = tc.backendErr
			r := httpObservedRequest(gone, "POST", tc.path, `{"question":"q"}`)
			if tc.anonymous {
				r.Header.Del("Authorization")
			}
			o.handler.ServeHTTP(httptest.NewRecorder(), r)
			assert.Equal(t, tc.want, o.outcome(t).Code)
		})
	}
}

// A question still running when the request's time runs out keeps its ID: 202, not the deadline's
// error, however little of the deadline was left and with any host's Wait.
func TestHTTPWaitEndsBeforeTheDeadline(t *testing.T) {
	for name, left := range map[string]time.Duration{
		"time to wait":              waitDeadlineMargin + 100*time.Millisecond,
		"less than the margin left": waitDeadlineMargin / 2,
		"no time left":              0,
	} {
		t.Run(name, func(t *testing.T) {
			o := httpObservedFixture(t, "running", httpBodyTimeout)
			o.service.Wait = time.Hour
			ctx, cancel := context.WithTimeout(context.Background(), left)
			defer cancel()
			started := time.Now()
			w := httptest.NewRecorder()
			o.handler.ServeHTTP(w, httpObservedRequest(ctx, "POST", httpAskPath, `{"question":"q"}`))
			assert.Equal(t, 202, w.Code, w.Body.String())
			assert.Equal(t, httpQuestionPath, w.Header().Get("Location"))
			assert.Less(t, time.Since(started), max(left-waitDeadlineMargin, 0)+time.Second)
			assert.Equal(t, "question-1", o.outcome(t).QuestionID)
		})
	}
}

// httpServed starts the fixture on a real listener: a recorder has no read deadline, and only
// net/http's own connection shows how those end.
func httpServed(t *testing.T, o *httpObserved) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(o.handler)
	t.Cleanup(server.Close)
	return server
}

const httpGoodHeaders = "Authorization: Bearer fixture\r\nContent-Type: application/json\r\nIdempotency-Key: key\r\n"

// httpRaw sends head and what there is of the body, declaring contentLength, and holds the
// connection open.
func httpRaw(t *testing.T, server *httptest.Server, headers string, contentLength int, body string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	httpRawSend(t, conn, headers, contentLength, body)
	return conn
}

func httpRawSend(t *testing.T, conn net.Conn, headers string, contentLength int, body string) {
	t.Helper()
	_, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: test\r\n%sContent-Length: %d\r\n\r\n%s", httpAskPath, headers, contentLength, body)
	require.NoError(t, err)
}

// A context doesn't bound reading a body, so a client trickling or stalling one is what the read
// deadline is for. It is told so, and nothing blames the backend.
func TestHTTPBodyDeadline(t *testing.T) {
	const timeout = 100 * time.Millisecond
	for name, body := range map[string]string{
		"a body stalled partway":                  `{"question":`,
		"a whole object, then a body that stalls": `{"question":"q"} `,
	} {
		t.Run(name, func(t *testing.T) {
			o := httpObservedFixture(t, "answered", timeout)
			conn := httpRaw(t, httpServed(t, o), httpGoodHeaders, 100, body)
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			require.NoError(t, err)
			defer response.Body.Close()
			read, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			assert.Equal(t, http.StatusRequestTimeout, response.StatusCode, string(read))
			assert.Contains(t, string(read), "request_timeout")
			got := o.outcome(t)
			assert.Equal(t, "request_timeout", got.Code)
			assert.Equal(t, 408, got.Status)
			assert.Equal(t, 0, o.backend.queued, "nothing is asked for a body that never came")
		})
	}
	t.Run("a client that hangs up partway through its body", func(t *testing.T) {
		o := httpObservedFixture(t, "answered", httpBodyTimeout)
		conn := httpRaw(t, httpServed(t, o), httpGoodHeaders, 100, `{"question":`)
		require.NoError(t, conn.Close())
		assert.Equal(t, HTTPClientDisconnected, o.outcome(t).Code)
		assert.Equal(t, 0, o.backend.queued)
	})
	// net/http watches for a hang-up with a read of its own once the body ends, clearing the
	// deadline first. Should it ever not, that read would fail at the deadline and cancel the
	// request during the wait: this holds the assumption readHTTPQuestion rests on.
	t.Run("the deadline ends with the body, so the wait can outlast it", func(t *testing.T) {
		o := httpObservedFixture(t, "running", 20*time.Millisecond)
		o.service.Wait = 300 * time.Millisecond
		const body = `{"question":"q"}`
		started := time.Now()
		conn := httpRaw(t, httpServed(t, o), httpGoodHeaders, len(body), body)
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		require.NoError(t, err)
		defer response.Body.Close()
		assert.Equal(t, http.StatusAccepted, response.StatusCode)
		assert.GreaterOrEqual(t, time.Since(started), o.service.Wait)
		got := o.outcome(t)
		assert.Empty(t, got.Code)
		assert.Equal(t, "question-1", got.QuestionID)
	})
	// net/http reads what a handler left of a body before it writes the response, so a refusal
	// that reads none of it must be answered all the same when the client stalls.
	for name, tc := range map[string]struct {
		headers, body string
		status        int
	}{
		"refused before the body":         {"Content-Type: application/json\r\n", `{"question":`, 401},
		"the wrong content type":          {"Authorization: Bearer fixture\r\nContent-Type: text/plain\r\n", `{"question":`, 400},
		"a body already seen to be wrong": {httpGoodHeaders, `not json `, 400},
	} {
		t.Run(name+", then a stalled body", func(t *testing.T) {
			o := httpObservedFixture(t, "answered", timeout)
			conn := httpRaw(t, httpServed(t, o), tc.headers, 100, tc.body)
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			require.NoError(t, err)
			defer response.Body.Close()
			assert.Equal(t, tc.status, response.StatusCode)
			assert.True(t, response.Close, "what is left of the body must not be read as another request")
			assert.Equal(t, tc.status, o.outcome(t).Status)
		})
	}
	// Nor does an id that can't be one wait on the body.
	t.Run("a malformed id, then a stalled body", func(t *testing.T) {
		o := httpObservedFixture(t, "answered", timeout)
		conn, err := net.Dial("tcp", httpServed(t, o).Listener.Addr().String())
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
		_, err = fmt.Fprintf(conn, "POST /v1/ontologies/bad!id/ask HTTP/1.1\r\nHost: test\r\n%sContent-Length: 100\r\n\r\n{", httpGoodHeaders)
		require.NoError(t, err)
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		require.NoError(t, err)
		defer response.Body.Close()
		assert.Equal(t, http.StatusBadRequest, response.StatusCode)
		assert.True(t, response.Close)
		assert.Equal(t, http.StatusBadRequest, o.outcome(t).Status)
	})
	// A deadline or its failed read must not outlive the request on a connection that goes on.
	t.Run("a connection serves its next request", func(t *testing.T) {
		o := httpObservedFixture(t, "answered", 20*time.Millisecond)
		const body = `{"question":"q"}`
		conn := httpRaw(t, httpServed(t, o), httpGoodHeaders, len(body), body)
		reader := bufio.NewReader(conn)
		for i := range 3 {
			response, err := http.ReadResponse(reader, nil)
			require.NoError(t, err, i)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err, i)
			require.NoError(t, response.Body.Close())
			assert.Equal(t, http.StatusOK, response.StatusCode, i)
			assert.False(t, response.Close, i)
			time.Sleep(40 * time.Millisecond) // past the deadline each body had
			httpRawSend(t, conn, httpGoodHeaders, len(body), body)
		}
	})
	t.Run("a refused body already here keeps the connection", func(t *testing.T) {
		o := httpObservedFixture(t, "answered", timeout)
		const body = `{"question":"q"}`
		conn := httpRaw(t, httpServed(t, o), "Content-Type: application/json\r\n", len(body), body)
		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, nil)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		assert.Equal(t, 401, response.StatusCode)
		assert.False(t, response.Close)
		httpRawSend(t, conn, httpGoodHeaders, len(body), body)
		response, err = http.ReadResponse(reader, nil)
		require.NoError(t, err)
		defer response.Body.Close()
		assert.Equal(t, http.StatusOK, response.StatusCode)
	})
}

// A plain context error is the contract's 503, as the same RPC's status would be: a stalled
// lookup in a host's resolver returns one.
func TestHTTPContextErrorsAreUnavailable(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, fmt.Errorf("read: %w", context.DeadlineExceeded)} {
		h := NewHTTPHandler(func(*http.Request, string) (HTTPCall, error) { return HTTPCall{}, cause })
		w := httpCall(h, "GET", "/v1/ontologies/ontology-1/questions/question-1", "", "")
		assert.Equal(t, 503, w.Code, cause)
		assert.Contains(t, w.Body.String(), "backend_unavailable")
		assert.Equal(t, errCodeBackendUnavailable, stableErrorCode(cause), "audit logs read the same")
	}
}

// A host refuses a malformed ontology id (ResolveHTTP) and the tool a malformed question id, each
// after authenticating and before any lookup, and the host's Done hears of the 400 either way.
func TestHTTPMalformedIDs(t *testing.T) {
	for _, target := range []struct{ method, path string }{
		{"POST", "/v1/ontologies/bad!id/ask"},
		{"GET", "/v1/ontologies/bad!id/questions/question-1"},
		{"GET", "/v1/ontologies/ontology-1/questions/bad!id"},
		{"GET", "/v1/ontologies/%ff/questions/question-1"},
	} {
		t.Run(target.path, func(t *testing.T) {
			o := httpObservedFixture(t, "answered", httpBodyTimeout)
			w := httptest.NewRecorder()
			o.handler.ServeHTTP(w, httpObservedRequest(context.Background(), target.method, target.path, `{"question":"q"}`))
			assert.Equal(t, 400, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "invalid_arguments")
			assert.NotEmpty(t, w.Header().Get("X-Request-ID"))
			got := o.outcome(t)
			assert.Equal(t, 400, got.Status)
			assert.Equal(t, "invalid_arguments", got.Code)
			assert.Zero(t, o.backend.queued)
			assert.Zero(t, o.backend.gets)
		})
	}
	t.Run("an anonymous caller is told only that", func(t *testing.T) {
		o := httpObservedFixture(t, "answered", httpBodyTimeout)
		r := httpObservedRequest(context.Background(), "POST", "/v1/ontologies/bad!id/ask", `{"question":"q"}`)
		r.Header.Del("Authorization")
		w := httptest.NewRecorder()
		o.handler.ServeHTTP(w, r)
		assert.Equal(t, 401, w.Code)
	})
}

// One caller's Idempotency-Key can't find another's question: a client token is unique per ontology,
// not per caller.
func TestHTTPIdempotencyKeysBelongToTheirCaller(t *testing.T) {
	o := httpObservedFixture(t, "answered", httpBodyTimeout)
	ask := func(caller, key string) string {
		r := httpObservedRequest(context.Background(), "POST", httpAskPath, `{"question":"q"}`)
		r.Header.Set("X-Caller", caller)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		o.handler.ServeHTTP(w, r)
		require.Equal(t, 200, w.Code, w.Body.String())
		return o.backend.tokens[len(o.backend.tokens)-1]
	}
	first := ask("credential-a", "retry-1")
	assert.Equal(t, first, ask("credential-a", "retry-1"), "a retry is the same question")
	for name, other := range map[string]string{
		"another caller":                     ask("credential-b", "retry-1"),
		"another key":                        ask("credential-a", "retry-2"),
		"a key differing only in case":       ask("credential-a", "RETRY-1"),
		"a key differing only in its spaces": ask("credential-a", "retry-1 "),
		"a host with one caller":             ask("", "retry-1"),
	} {
		assert.NotEqual(t, first, other, name)
	}
	assert.Len(t, first, 64, "it must fit the column, whatever the key's length")
	assert.Len(t, ask("credential-a", strings.Repeat("x", 64)), 64)
	assert.NotContains(t, first, "retry-1")
}

// A message reaches a client only from the handler or through FoundryError: a host's refusal or an
// Authorize error can name internals.
func TestHTTPErrorsDoNotRepeatUncheckedMessages(t *testing.T) {
	const private = "/helix.foundry.v1.FoundryService/Private is not available in local mode"
	for grpcCode, want := range map[codes.Code]struct {
		status int
		code   string
	}{
		codes.InvalidArgument:    {400, "invalid_arguments"},
		codes.FailedPrecondition: {412, "failed_precondition"},
		codes.AlreadyExists:      {409, "already_exists"},
		codes.ResourceExhausted:  {429, "resource_exhausted"},
		codes.NotFound:           {404, "not_found"},
		codes.PermissionDenied:   {404, "not_found"},
	} {
		w := httptest.NewRecorder()
		WriteHTTPError(w, status.Error(grpcCode, private), "request")
		assert.Equal(t, want.status, w.Code, grpcCode)
		assert.Contains(t, w.Body.String(), `"code":"`+want.code+`"`, grpcCode)
		assert.NotContains(t, w.Body.String(), "helix.foundry", grpcCode)
	}
	t.Run("a backend refusal FoundryError let through keeps its words", func(t *testing.T) {
		w := httptest.NewRecorder()
		WriteHTTPError(w, FoundryError(status.Error(codes.FailedPrecondition, "the ontology has no load")), "request")
		assert.Contains(t, w.Body.String(), "the ontology has no load")
	})
	t.Run("the handler's own refusals keep theirs", func(t *testing.T) {
		o := httpObservedFixture(t, "answered", httpBodyTimeout)
		r := httpObservedRequest(context.Background(), "POST", httpAskPath, `{"question":"q"}`)
		r.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()
		o.handler.ServeHTTP(w, r)
		assert.Contains(t, w.Body.String(), "Content-Type must be application/json")
	})
	t.Run("an Authorize refusal does not", func(t *testing.T) {
		o := httpObservedFixture(t, "answered", httpBodyTimeout)
		h := newHTTPHandler(func(r *http.Request, _ string) (HTTPCall, error) {
			return HTTPCall{Context: r.Context(), Service: o.service, WorkspaceID: "workspace-2", Identity: &Identity{
				Authorize: func(context.Context, any, Grant, ...string) error {
					return status.Error(codes.FailedPrecondition, private)
				},
			}}, nil
		}, httpBodyTimeout)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httpObservedRequest(context.Background(), "GET", httpQuestionPath, ""))
		assert.Equal(t, 412, w.Code)
		assert.NotContains(t, w.Body.String(), "helix.foundry")
	})
}

// Whatever no route takes still answers in the contract's error shape.
func TestHTTPUnroutedRequestsAreTheContractsError(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
		code, allow  string
	}{
		{"GET", httpAskPath, 405, "method_not_allowed", "POST"},
		{"DELETE", httpAskPath, 405, "method_not_allowed", "POST"},
		{"POST", httpQuestionPath, 405, "method_not_allowed", "GET"},
		{"POST", "/v1/ontologies/ontology-1", 404, "not_found", ""},
		{"GET", "/v1/ontologies/ontology-1/questions", 404, "not_found", ""},
		{"GET", "/metrics", 404, "not_found", ""},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			o := httpObservedFixture(t, "answered", httpBodyTimeout)
			w := httptest.NewRecorder()
			o.handler.ServeHTTP(w, httpObservedRequest(context.Background(), tc.method, tc.path, ""))
			assert.Equal(t, tc.status, w.Code)
			assert.Equal(t, tc.allow, w.Header().Get("Allow"))
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
			var body struct {
				Error struct {
					Code      string `json:"code"`
					Message   string `json:"message"`
					RequestID string `json:"request_id"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
			assert.Equal(t, tc.code, body.Error.Code)
			assert.NotEmpty(t, body.Error.Message)
			assert.Equal(t, w.Header().Get("X-Request-ID"), body.Error.RequestID)
			assert.NotEmpty(t, body.Error.RequestID)
			assert.Empty(t, o.seen, "no route took it")
		})
	}
}

// A large integer stays exact whichever JSON reader the client has.
func TestHTTPResultKeepsLargeIntegersExact(t *testing.T) {
	h, b := httpFixture(t, "answered")
	b.question.ResultJson = `{"status":"answered","count":9007199254740993,"small":7}`
	w := httpCall(h, "GET", "/v1/ontologies/ontology-1/questions/question-1", "", "")
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"count":"9007199254740993"`)
	assert.Contains(t, w.Body.String(), `"small":7`)
}
