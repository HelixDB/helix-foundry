package graphproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// gateway records the queries it is sent and answers each with the next of
// answers, then with answer.
type gateway struct {
	mu      sync.Mutex
	queries []query
	answers []helixgateway.Response
	answer  helixgateway.Response
	err     error
	// onQuery, when set, runs after each query is recorded.
	onQuery func()
}

type query struct {
	endpoint, tenant string
	body             []byte
	extra            http.Header
}

func (g *gateway) Query(_ context.Context, endpoint, tenantID string, body []byte, extra http.Header) (helixgateway.Response, error) {
	g.mu.Lock()
	g.queries = append(g.queries, query{endpoint, tenantID, body, extra})
	answer := g.answer
	if len(g.answers) > 0 {
		answer, g.answers = g.answers[0], g.answers[1:]
	}
	onQuery := g.onQuery
	g.mu.Unlock()
	if onQuery != nil {
		onQuery()
	}
	return answer, g.err
}

var (
	ok          = helixgateway.Response{StatusCode: 200, ContentType: "application/json", Body: []byte(`{"ok":1}`)}
	rateLimited = helixgateway.Response{StatusCode: 429, ContentType: "application/json", Body: []byte(`{"error":"rate_limited","msg":"rate limit exceeded"}`)}
	noLimit     = helixgateway.Response{
		StatusCode: 503, ContentType: "application/json", Body: []byte(`{"error":"rate_limit_unavailable","msg":"tenant rate limit is unavailable"}`),
	}
)

func (g *gateway) sent() []query {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]query(nil), g.queries...)
}

func serve(t *testing.T, g *gateway) *Proxy {
	t.Helper()
	p, err := Serve(context.Background(), Binding{OntologyID: "ont-1", TenantID: "tenant-1", Endpoint: "https://gw.example/v2/query"}, g, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func post(t *testing.T, p *Proxy, path, token string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, p.URL()+path, bytes.NewReader(body))
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

func TestProxy(t *testing.T) {
	// The agent's query reaches the bound tenant's gateway, with the writer headers it asked for.
	t.Run("forwards a query to the bound tenant", func(t *testing.T) {
		g := &gateway{answer: helixgateway.Response{StatusCode: 200, ContentType: "application/json", Body: []byte(`{"ok":1}`)}}
		p := serve(t, g)
		if !strings.HasPrefix(p.URL(), "http://127.0.0.1:") {
			t.Fatalf("url %s is not loopback", p.URL())
		}
		resp := post(t, p, QueryPath, p.Token(), []byte(`{"request_type":"write"}`), map[string]string{
			"X-Helix-Require-Writer": "true", "X-Helix-Await-Durable": "true", "Content-Type": "application/json",
		})
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != `{"ok":1}` || resp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("response = %d %s", resp.StatusCode, body)
		}
		sent := g.sent()
		if len(sent) != 1 || sent[0].tenant != "tenant-1" || sent[0].endpoint != "https://gw.example/v2/query" ||
			string(sent[0].body) != `{"request_type":"write"}` {
			t.Fatalf("sent = %+v", sent)
		}
		if sent[0].extra.Get("X-Helix-Require-Writer") != "true" || sent[0].extra.Get("X-Helix-Await-Durable") != "true" || len(sent[0].extra) != 2 {
			t.Fatalf("extra headers = %v", sent[0].extra)
		}
	})

	// The gateway's own refusals reach the agent as they are.
	t.Run("relays the gateway's status", func(t *testing.T) {
		g := &gateway{answer: helixgateway.Response{StatusCode: 429, Body: []byte(`slow down`)}}
		p := serve(t, g)
		resp := post(t, p, QueryPath, p.Token(), []byte(`{}`), nil)
		if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 429 || string(body) != "slow down" {
			t.Fatalf("response = %d %s", resp.StatusCode, body)
		}
	})

	// Nothing reaches the gateway without the job's token, on another route, or naming its own database.
	t.Run("refuses what it must not forward", func(t *testing.T) {
		g := &gateway{answer: helixgateway.Response{StatusCode: 200}}
		p := serve(t, g)
		other := serve(t, g)
		for name, tc := range map[string]struct {
			path, token string
			headers     map[string]string
			body        []byte
			want        int
		}{
			"no token":            {QueryPath, "", nil, []byte(`{}`), http.StatusUnauthorized},
			"another job's token": {QueryPath, other.Token(), nil, []byte(`{}`), http.StatusUnauthorized},
			"a near token":        {QueryPath, p.Token() + "x", nil, []byte(`{}`), http.StatusUnauthorized},
			"another route":       {"/v1/query", p.Token(), nil, []byte(`{}`), http.StatusNotFound},
			"a tenant header":     {QueryPath, p.Token(), map[string]string{"X-Helix-Tenant-ID": "tenant-2"}, []byte(`{}`), http.StatusBadRequest},
			"a database header":   {QueryPath, p.Token(), map[string]string{"x-helix-database": "db"}, []byte(`{}`), http.StatusBadRequest},
			"a body past 2 MiB":   {QueryPath, p.Token(), nil, bytes.Repeat([]byte("a"), MaxRequestBytes+1), http.StatusRequestEntityTooLarge},
		} {
			resp := post(t, p, tc.path, tc.token, tc.body, tc.headers)
			if resp.StatusCode != tc.want {
				t.Fatalf("%s: status %d, want %d", name, resp.StatusCode, tc.want)
			}
		}
		req, _ := http.NewRequest(http.MethodGet, p.URL()+QueryPath, nil)
		req.Header.Set("Authorization", "Bearer "+p.Token())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET: status %d", resp.StatusCode)
		}
		if sent := g.sent(); len(sent) != 0 {
			t.Fatalf("forwarded %d refused requests", len(sent))
		}
		// Exactly 2 MiB is a query.
		if resp := post(t, p, QueryPath, p.Token(), bytes.Repeat([]byte("a"), MaxRequestBytes), nil); resp.StatusCode != 200 {
			t.Fatalf("a 2 MiB body: status %d", resp.StatusCode)
		}
	})

	// The agent sees a gateway it can't reach as a failed request, not a hang.
	t.Run("answers bad gateway when the gateway fails", func(t *testing.T) {
		p := serve(t, &gateway{err: errors.New("dial")})
		if resp := post(t, p, QueryPath, p.Token(), []byte(`{}`), nil); resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})

	// A database paused or deleted mid-attempt stops taking writes once the binding is checked again.
	t.Run("refuses writes once the binding no longer holds", func(t *testing.T) {
		g := &gateway{answer: helixgateway.Response{StatusCode: 200}}
		var verdict error
		var checks int
		p, err := Serve(context.Background(), Binding{OntologyID: "ont-1", TenantID: "tenant-1", Endpoint: "https://gw.example/v2/query"}, g,
			func(context.Context) error { checks++; return verdict }, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		now := time.Now()
		p.now = func() time.Time { return now }
		if resp := post(t, p, QueryPath, p.Token(), []byte(`{}`), nil); resp.StatusCode != 200 || checks != 0 {
			t.Fatalf("status %d after %d checks: no check before RecheckEvery", resp.StatusCode, checks)
		}
		now = now.Add(RecheckEvery)
		verdict = status.Error(codes.Unavailable, "db down")
		if resp := post(t, p, QueryPath, p.Token(), []byte(`{}`), nil); resp.StatusCode != 200 || checks != 1 {
			t.Fatalf("status %d after %d checks: a failed check keeps serving", resp.StatusCode, checks)
		}
		verdict = status.Error(codes.FailedPrecondition, "queries are paused")
		resp := post(t, p, QueryPath, p.Token(), []byte(`{}`), nil)
		if body, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "paused") || checks != 2 {
			t.Fatalf("status %d %s after %d checks", resp.StatusCode, body, checks)
		}
		verdict = nil
		if resp := post(t, p, QueryPath, p.Token(), []byte(`{}`), nil); resp.StatusCode != http.StatusForbidden || checks != 2 {
			t.Fatalf("status %d after %d checks: a refusal sticks", resp.StatusCode, checks)
		}
		if sent := g.sent(); len(sent) != 2 {
			t.Fatalf("forwarded %d", len(sent))
		}
	})

	// An attempt whose lease may lapse is fenced: a replacement worker may
	// own the job, so nothing more of this attempt reaches the database, not
	// even a write already on its way to the gateway.
	t.Run("refuses every request once its attempt ends", func(t *testing.T) {
		g := &blockingGateway{started: make(chan struct{}), cancelled: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p, err := Serve(ctx, Binding{OntologyID: "ont-1", TenantID: "tenant-1", Endpoint: "https://gw.example/v2/query"}, g, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		inFlight := make(chan *http.Response, 1)
		go func() {
			req, _ := http.NewRequest(http.MethodPost, p.URL()+QueryPath, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+p.Token())
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				inFlight <- nil
				return
			}
			inFlight <- resp
		}()
		<-g.started
		cancel()
		select {
		case <-g.cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("the write in flight was not cancelled")
		}
		if resp := <-inFlight; resp != nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatal("the write in flight was answered as a success")
			}
		}
		resp := post(t, p, QueryPath, p.Token(), []byte(`{}`), nil)
		if body, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "ended") {
			t.Fatalf("status %d %s after the attempt ended", resp.StatusCode, body)
		}
		if g.calls() != 1 {
			t.Fatalf("the gateway saw %d queries, want only the one cut off", g.calls())
		}
	})

	// A gateway pod that has not loaded the tenant's limit yet, or a bucket
	// that is empty, turned the query away before running it: sending it again
	// can't apply it twice, and the agent sees the query run.
	t.Run("sends a turned-away query again until it runs", func(t *testing.T) {
		g := &gateway{answers: []helixgateway.Response{noLimit, rateLimited}, answer: ok}
		p, err := Serve(context.Background(), Binding{OntologyID: "ont-1", TenantID: "tenant-1", Endpoint: "https://gw.example/v2/query"}, g, nil, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		resp := post(t, p, QueryPath, p.Token(), []byte(`{"request_type":"write"}`), map[string]string{"X-Helix-Require-Writer": "true"})
		if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || string(body) != `{"ok":1}` {
			t.Fatalf("response = %d %s", resp.StatusCode, body)
		}
		sent := g.sent()
		if len(sent) != 3 {
			t.Fatalf("sent %d queries, want 3", len(sent))
		}
		for _, q := range sent {
			if q.tenant != "tenant-1" || string(q.body) != `{"request_type":"write"}` || q.extra.Get("X-Helix-Require-Writer") != "true" {
				t.Fatalf("resent query = %+v", q)
			}
		}
	})

	// The wait is bounded: past it the agent gets the refusal, in time to
	// count it against its own Helix timeout rather than time out.
	t.Run("relays the refusal once the wait is spent", func(t *testing.T) {
		g := &gateway{answer: rateLimited}
		p, err := Serve(context.Background(), Binding{OntologyID: "ont-1", TenantID: "tenant-1", Endpoint: "https://gw.example/v2/query"}, g, nil, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		started := time.Now()
		resp := post(t, p, QueryPath, p.Token(), []byte(`{}`), nil)
		if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 429 || !strings.Contains(string(body), "rate_limited") {
			t.Fatalf("response = %d %s", resp.StatusCode, body)
		}
		if took := time.Since(started); took > 3*time.Second {
			t.Fatalf("relayed after %s", took)
		}
		if sent := len(g.sent()); sent < 2 {
			t.Fatalf("sent %d queries, want a resend before giving up", sent)
		}
	})

	// Any other refusal may come from a query that ran, a write that may have
	// landed: it reaches the agent at once, sent once.
	t.Run("relays any other refusal at once", func(t *testing.T) {
		for _, answer := range []helixgateway.Response{
			{StatusCode: 503, Body: []byte(`{"error":"backend_unavailable","msg":"Backend unavailable"}`)},
			{StatusCode: 503, Body: []byte(`{"code":"WRITE_OUTCOME_UNKNOWN"}`)},
			{StatusCode: 503, Body: []byte(`not json`)},
			{StatusCode: 500, Body: []byte(`{"error":"rate_limit_unavailable"}`)},
		} {
			g := &gateway{answer: answer}
			p, err := Serve(context.Background(), Binding{OntologyID: "ont-1", TenantID: "tenant-1", Endpoint: "https://gw.example/v2/query"}, g, nil, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			resp := post(t, p, QueryPath, p.Token(), []byte(`{}`), nil)
			if resp.StatusCode != answer.StatusCode || len(g.sent()) != 1 {
				t.Fatalf("%s: status %d after %d queries", answer.Body, resp.StatusCode, len(g.sent()))
			}
		}
	})

	// A database paused or deleted while a query waits stops it from being sent again.
	t.Run("checks the binding before sending again", func(t *testing.T) {
		g := &gateway{answer: rateLimited}
		var verdict error
		p, err := Serve(context.Background(), Binding{OntologyID: "ont-1", TenantID: "tenant-1", Endpoint: "https://gw.example/v2/query"}, g,
			func(context.Context) error { return verdict }, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		var mu sync.Mutex
		now := time.Now()
		p.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
		g.onQuery = func() {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(RecheckEvery)
			verdict = status.Error(codes.FailedPrecondition, "queries are paused")
		}
		resp := post(t, p, QueryPath, p.Token(), []byte(`{}`), nil)
		if body, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "paused") {
			t.Fatalf("response = %d %s", resp.StatusCode, body)
		}
		if sent := len(g.sent()); sent != 1 {
			t.Fatalf("sent %d queries to a paused database's gateway", sent)
		}
	})

	// An attempt that ends while a query waits stops waiting: nothing more of
	// it reaches the database.
	t.Run("stops waiting when its attempt ends", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := &gateway{answer: rateLimited, onQuery: cancel}
		p, err := Serve(ctx, Binding{OntologyID: "ont-1", TenantID: "tenant-1", Endpoint: "https://gw.example/v2/query"}, g, nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		started := time.Now()
		resp := post(t, p, QueryPath, p.Token(), []byte(`{}`), nil)
		if resp.StatusCode == http.StatusOK || time.Since(started) > 5*time.Second {
			t.Fatalf("status %d after %s", resp.StatusCode, time.Since(started))
		}
		if sent := len(g.sent()); sent != 1 {
			t.Fatalf("sent %d queries after the attempt ended", sent)
		}
	})

	// After Close the port answers nothing: the next attempt has a new proxy and token.
	t.Run("stops serving on close", func(t *testing.T) {
		p := serve(t, &gateway{answer: helixgateway.Response{StatusCode: 200}})
		url := p.URL()
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		client := http.Client{Timeout: time.Second}
		if resp, err := client.Post(url+QueryPath, "application/json", strings.NewReader(`{}`)); err == nil {
			_ = resp.Body.Close()
			t.Fatal("served after close")
		}
	})
}

// blockingGateway holds each query until its context ends, as a slow write does.
type blockingGateway struct {
	mu        sync.Mutex
	n         int
	started   chan struct{}
	cancelled chan struct{}
}

func (g *blockingGateway) Query(ctx context.Context, _, _ string, _ []byte, _ http.Header) (helixgateway.Response, error) {
	g.mu.Lock()
	g.n++
	g.mu.Unlock()
	close(g.started)
	<-ctx.Done()
	close(g.cancelled)
	return helixgateway.Response{}, ctx.Err()
}

func (g *blockingGateway) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

// store is the ontology Bind reads.
type store struct {
	ontology *model.FoundryOntology
}

func (s store) GetFoundryOntology(_ context.Context, workspaceID, id string) (*model.FoundryOntology, error) {
	if s.ontology == nil || s.ontology.ID != id || s.ontology.WorkspaceID != workspaceID {
		return nil, foundrystore.ErrNotFound
	}
	return s.ontology, nil
}

// databases is the host's databases: each one's row, and what resolving its
// query URL answers.
type databases struct {
	rows     map[string]ports.Database
	endpoint error
}

func (d databases) Get(_ context.Context, tenantID string) (ports.Database, error) {
	if row, ok := d.rows[tenantID]; ok {
		return row, nil
	}
	return ports.Database{}, foundrystore.ErrNotFound
}

func (d databases) Live(ctx context.Context, tenantID string) (ports.Database, error) {
	return d.Get(ctx, tenantID)
}

func (d databases) Endpoint(_ context.Context, tenantID, path string) (string, error) {
	if d.endpoint != nil {
		return "", d.endpoint
	}
	return "https://" + tenantID + ".db.example.com" + path, nil
}

func TestBind(t *testing.T) {
	fixture := func() (store, databases) {
		return store{ontology: &model.FoundryOntology{ID: "ont-1", WorkspaceID: "ws-1", TenantID: "tenant-1"}},
			databases{rows: map[string]ports.Database{"tenant-1": {TenantID: "tenant-1", WorkspaceID: "ws-1", ProjectID: "proj-1"}}}
	}

	// The binding names the ontology's own database, at its query URL.
	s, dbs := fixture()
	b, err := Bind(context.Background(), s, dbs, "ws-1", "ont-1", model.FoundryGraphJobLoad)
	if err != nil {
		t.Fatal(err)
	}
	if b.TenantID != "tenant-1" || b.Endpoint != "https://tenant-1.db.example.com/v2/query" || b.OntologyID != "ont-1" {
		t.Fatalf("binding = %+v", b)
	}

	now := time.Now()
	for name, tc := range map[string]struct {
		mutate func(*store, *databases)
		code   codes.Code
	}{
		"a deleted ontology":  {func(s *store, _ *databases) { s.ontology.DeletedAt = &now }, codes.NotFound},
		"another workspace's": {func(s *store, _ *databases) { s.ontology.WorkspaceID = "ws-2" }, codes.NotFound},
		"a deleted database": {func(_ *store, d *databases) {
			row := d.rows["tenant-1"]
			row.Deleted = true
			d.rows["tenant-1"] = row
		}, codes.NotFound},
		"a retired database": {func(_ *store, d *databases) {
			row := d.rows["tenant-1"]
			row.Retired = true
			d.rows["tenant-1"] = row
		}, codes.NotFound},
		"a missing database": {func(_ *store, d *databases) { delete(d.rows, "tenant-1") }, codes.NotFound},
		"a database moved away": {func(_ *store, d *databases) {
			row := d.rows["tenant-1"]
			row.WorkspaceID = "ws-2"
			d.rows["tenant-1"] = row
		}, codes.NotFound},
		"a paused database": {func(_ *store, d *databases) { d.endpoint = status.Error(codes.FailedPrecondition, "paused") },
			codes.FailedPrecondition},
		"an unresolvable database": {func(_ *store, d *databases) { d.endpoint = status.Error(codes.Unavailable, "no route") }, codes.Unavailable},
	} {
		s, dbs := fixture()
		tc.mutate(&s, &dbs)
		if _, err := Bind(context.Background(), s, dbs, "ws-1", "ont-1", model.FoundryGraphJobExtract); status.Code(err) != tc.code {
			t.Fatalf("%s: err = %v, want %s", name, err, tc.code)
		}
	}

	// A deleted ontology holds its database until its loads are deleted from
	// it: only a delete job binds it, and still not a database gone or moved.
	s, dbs = fixture()
	s.ontology.DeletedAt = &now
	if b, err := Bind(context.Background(), s, dbs, "ws-1", "ont-1", model.FoundryGraphJobDelete); err != nil || b.TenantID != "tenant-1" {
		t.Fatalf("a delete of a deleted ontology's load: (%+v, %v)", b, err)
	}
	dbs.rows["tenant-1"] = ports.Database{TenantID: "tenant-1", WorkspaceID: "ws-2"}
	if _, err := Bind(context.Background(), s, dbs, "ws-1", "ont-1", model.FoundryGraphJobDelete); status.Code(err) != codes.NotFound {
		t.Fatalf("a delete in a database moved away: %v", err)
	}
	s, dbs = fixture()
	s.ontology = nil
	if _, err := Bind(context.Background(), s, dbs, "ws-1", "ont-1", model.FoundryGraphJobDelete); status.Code(err) != codes.NotFound {
		t.Fatalf("a delete of a missing ontology: %v", err)
	}

	// A read binds a live ontology's database only.
	s, dbs = fixture()
	if b, err := BindRead(context.Background(), s, dbs, "ws-1", "ont-1"); err != nil || b.TenantID != "tenant-1" {
		t.Fatalf("a read: (%+v, %v)", b, err)
	}
	s.ontology.DeletedAt = &now
	if _, err := BindRead(context.Background(), s, dbs, "ws-1", "ont-1"); status.Code(err) != codes.NotFound {
		t.Fatalf("a read of a deleted ontology: %v", err)
	}
}
