package graphproxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
)

func serveReadOnly(t *testing.T, g Querier, admissionWait time.Duration) *ReadProxy {
	t.Helper()
	p, err := ServeReadOnly(g, admissionWait)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func register(t *testing.T, p *ReadProxy, ctx context.Context, tenant string) string {
	t.Helper()
	token, err := p.Register(ctx, Binding{OntologyID: "ont-" + tenant, TenantID: tenant, Endpoint: "https://gw.example/v2/query"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func postRead(t *testing.T, url, token string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+QueryPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

const readQueryBody = `{"request_type":"read","query":{"queries":[]},"parameters":{}}`

func TestReadProxy(t *testing.T) {
	ctx := context.Background()

	// Each call's reads reach its own binding's tenant, asking for the writer.
	t.Run("forwards each call's reads to its binding", func(t *testing.T) {
		g := &gateway{answer: ok}
		p := serveReadOnly(t, g, 0)
		if !strings.HasPrefix(p.URL(), "http://127.0.0.1:") {
			t.Fatalf("url %s is not loopback", p.URL())
		}
		first, second := register(t, p, ctx, "tenant-1"), register(t, p, ctx, "tenant-2")
		for _, token := range []string{first, second} {
			resp := postRead(t, p.URL(), token, []byte(readQueryBody), map[string]string{"X-Helix-Require-Writer": "true"})
			if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || string(body) != `{"ok":1}` {
				t.Fatalf("response = %d %s", resp.StatusCode, body)
			}
		}
		sent := g.sent()
		if len(sent) != 2 || sent[0].tenant != "tenant-1" || sent[1].tenant != "tenant-2" || sent[0].endpoint != "https://gw.example/v2/query" {
			t.Fatalf("sent = %+v", sent)
		}
		if string(sent[0].body) != `{"parameters":{},"query":{"queries":[]},"request_type":"read"}` ||
			sent[0].extra.Get("X-Helix-Require-Writer") != "true" || len(sent[0].extra) != 1 {
			t.Fatalf("forwarded %s with %v", sent[0].body, sent[0].extra)
		}
	})

	// Nothing that could write, or name its own database, reaches the gateway.
	t.Run("refuses writes and what it must not forward", func(t *testing.T) {
		g := &gateway{answer: ok}
		p := serveReadOnly(t, g, 0)
		token := register(t, p, ctx, "tenant-1")
		for name, tc := range map[string]struct {
			token   string
			headers map[string]string
			body    string
			want    int
		}{
			"a write":                  {token, nil, `{"request_type":"write","query":{}}`, http.StatusForbidden},
			"no request type":          {token, nil, `{"query":{}}`, http.StatusForbidden},
			"a numeric request type":   {token, nil, `{"request_type":0,"query":{}}`, http.StatusForbidden},
			"a write after a read":     {token, nil, `{"request_type":"read","request_type":"write","query":{}}`, http.StatusForbidden},
			"the camel-case alias":     {token, nil, `{"request_type":"read","requestType":"write","query":{}}`, http.StatusForbidden},
			"not an object":            {token, nil, `["read"]`, http.StatusBadRequest},
			"not JSON":                 {token, nil, `read`, http.StatusBadRequest},
			"an await-durable header":  {token, map[string]string{"X-Helix-Await-Durable": "true"}, readQueryBody, http.StatusBadRequest},
			"a tenant header":          {token, map[string]string{"X-Helix-Tenant-ID": "tenant-2"}, readQueryBody, http.StatusBadRequest},
			"no token":                 {"", nil, readQueryBody, http.StatusUnauthorized},
			"an unregistered token":    {token + "x", nil, readQueryBody, http.StatusUnauthorized},
			"a body past the limit":    {token, nil, `{"request_type":"read","pad":"` + strings.Repeat("a", MaxRequestBytes) + `"}`, http.StatusRequestEntityTooLarge},
			"a write named in unicode": {token, nil, `{"request_type":"read","request\u005ftype":"write"}`, http.StatusForbidden},
		} {
			if resp := postRead(t, p.URL(), tc.token, []byte(tc.body), tc.headers); resp.StatusCode != tc.want {
				t.Fatalf("%s: status %d, want %d", name, resp.StatusCode, tc.want)
			}
		}
		req, _ := http.NewRequest(http.MethodGet, p.URL()+QueryPath, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET: status %d", resp.StatusCode)
		}
		if resp := postRead(t, p.URL()+"/v1", token, []byte(readQueryBody), nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("another route: status %d", resp.StatusCode)
		}
		if sent := g.sent(); len(sent) != 0 {
			t.Fatalf("forwarded %d refused requests: %+v", len(sent), sent)
		}
	})

	// A duplicate request_type is gone from what the gateway reads: it sees
	// exactly the read the proxy checked.
	t.Run("forwards only the request type it checked", func(t *testing.T) {
		g := &gateway{answer: ok}
		p := serveReadOnly(t, g, 0)
		resp := postRead(t, p.URL(), register(t, p, ctx, "tenant-1"), []byte(`{"request_type":"write","request_type":"read","query":{}}`), nil)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if sent := g.sent(); len(sent) != 1 || string(sent[0].body) != `{"query":{},"request_type":"read"}` {
			t.Fatalf("sent = %+v", sent)
		}
	})

	// A call's token works only while the call runs, and its reads in flight end with it.
	t.Run("ends a call's access with its context", func(t *testing.T) {
		g := &blockingGateway{started: make(chan struct{}), cancelled: make(chan struct{})}
		p := serveReadOnly(t, g, 0)
		callCtx, endCall := context.WithCancel(ctx)
		token := register(t, p, callCtx, "tenant-1")
		inFlight := make(chan *http.Response, 1)
		go func() {
			req, _ := http.NewRequest(http.MethodPost, p.URL()+QueryPath, strings.NewReader(readQueryBody))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				inFlight <- nil
				return
			}
			inFlight <- resp
		}()
		<-g.started
		endCall()
		select {
		case <-g.cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("the read in flight was not cancelled")
		}
		if resp := <-inFlight; resp != nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatal("the read in flight was answered as a success")
			}
		}
		if resp := postRead(t, p.URL(), token, []byte(readQueryBody), nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d after the call ended", resp.StatusCode)
		}
		p.mu.Lock()
		left := len(p.calls)
		p.mu.Unlock()
		if left != 0 {
			t.Fatalf("%d calls still registered", left)
		}
	})

	// A question's attempt reads for minutes: a database paused or deleted
	// meanwhile stops taking its reads once its binding is checked again,
	// while the pod's other calls keep reading theirs.
	t.Run("refuses a call's reads once its binding no longer holds", func(t *testing.T) {
		g := &gateway{answer: ok}
		p := serveReadOnly(t, g, 0)
		now := time.Now()
		p.now = func() time.Time { return now }
		var verdict error
		var checks int
		rechecked, err := p.Register(ctx, Binding{OntologyID: "ont-1", TenantID: "tenant-1", Endpoint: "https://gw.example/v2/query"},
			func(context.Context) error { checks++; return verdict })
		if err != nil {
			t.Fatal(err)
		}
		other := register(t, p, ctx, "tenant-2")
		if resp := postRead(t, p.URL(), rechecked, []byte(readQueryBody), nil); resp.StatusCode != 200 || checks != 0 {
			t.Fatalf("status %d after %d checks: no check before RecheckEvery", resp.StatusCode, checks)
		}
		now = now.Add(RecheckEvery)
		verdict = status.Error(codes.Unavailable, "db down")
		if resp := postRead(t, p.URL(), rechecked, []byte(readQueryBody), nil); resp.StatusCode != 200 || checks != 1 {
			t.Fatalf("status %d after %d checks: a failed check keeps serving", resp.StatusCode, checks)
		}
		verdict = status.Error(codes.FailedPrecondition, "queries are paused")
		resp := postRead(t, p.URL(), rechecked, []byte(readQueryBody), nil)
		if body, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "paused") || checks != 2 {
			t.Fatalf("status %d %s after %d checks", resp.StatusCode, body, checks)
		}
		verdict = nil
		if resp := postRead(t, p.URL(), rechecked, []byte(readQueryBody), nil); resp.StatusCode != http.StatusForbidden || checks != 2 {
			t.Fatalf("status %d after %d checks: a refusal sticks", resp.StatusCode, checks)
		}
		if resp := postRead(t, p.URL(), other, []byte(readQueryBody), nil); resp.StatusCode != 200 {
			t.Fatalf("another call: status %d", resp.StatusCode)
		}
		if sent := g.sent(); len(sent) != 3 || sent[2].tenant != "tenant-2" {
			t.Fatalf("sent = %+v", sent)
		}
	})

	// A read the gateway turned away before running it is sent again, as a write is.
	t.Run("sends a turned-away read again", func(t *testing.T) {
		g := &gateway{answers: []helixgateway.Response{rateLimited}, answer: ok}
		p := serveReadOnly(t, g, time.Minute)
		resp := postRead(t, p.URL(), register(t, p, ctx, "tenant-1"), []byte(readQueryBody), nil)
		if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || string(body) != `{"ok":1}` || len(g.sent()) != 2 {
			t.Fatalf("response = %d %s after %d queries", resp.StatusCode, body, len(g.sent()))
		}
	})

	t.Run("stops serving on close", func(t *testing.T) {
		p := serveReadOnly(t, &gateway{answer: ok}, 0)
		url := p.URL()
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		client := http.Client{Timeout: time.Second}
		if resp, err := client.Post(url+QueryPath, "application/json", strings.NewReader(readQueryBody)); err == nil {
			_ = resp.Body.Close()
			t.Fatal("served after close")
		}
	})
}
