// Package graphproxy lets the ontology agent sidecar write one Foundry
// ontology's Helix database without holding a credential. For each attempt of
// a graph job the foundry-graph-worker binds the ontology's tenant and serves
// a loopback proxy with a fresh token: the agent sends its queries there, and
// the proxy forwards them to the tenant's gateway with the provisioner key and
// the tenant header, which the agent can't choose or see.
//
// A request is forwarded only when it is POST /v2/query with the job's token,
// carries no Helix header but x-helix-require-writer and x-helix-await-durable
// (which pass through, as writes need them), and its body is at most
// MaxRequestBytes. The binding is checked again every RecheckEvery, so a
// database paused or deleted mid-attempt stops taking the agent's writes.
// The proxy serves only while its attempt's context is live: once the
// attempt ends, or its lease may lapse, nothing more reaches the database
// through it, so a replacement worker is never a second writer.
//
// A query the gateway turns away before running it, rate limited (429) or
// with no limit it can charge (503 rate_limit_unavailable: the tenant's not
// loaded yet, or the limiter unreachable), is sent again with backoff for a
// while before the refusal reaches the agent: it never ran, so it can't apply
// twice, and a bucket refills, or a gateway pod loads the tenant's
// limits.json, in the meantime more often than not.
//
// ReadProxy (readonly.go) is the read-only mode foundry-read and the
// foundry-ask-worker serve: one per pod, a token per read call (a read, or a
// question's attempt, whose binding is checked again the same way), and only
// read queries forwarded.
package graphproxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// MaxRequestBytes is the largest query body forwarded; the agent's
	// HelixTarget.max_request_bytes says the same, so it refuses a larger
	// write before sending it.
	MaxRequestBytes = 2 << 20
	// MaxResponseBytes bounds a gateway answer the proxy relays: the agent's
	// reads (stored chunks, names to resolve, sweep pages) come back through it.
	MaxResponseBytes = 32 << 20
	// QueryPath is the one route served.
	QueryPath = "/v2/query"
	// RecheckEvery is how often a serving proxy checks its binding again.
	RecheckEvery = 30 * time.Second
	// firstAdmissionBackoff and maxAdmissionBackoff bound the wait before a
	// turned-away query is sent again; it doubles from the first, jittered.
	firstAdmissionBackoff = 250 * time.Millisecond
	maxAdmissionBackoff   = 4 * time.Second
)

var admissionRetries = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_graph_proxy_admission_retries_total",
	Help: "Queries the graph proxy sent again after the gateway turned them away before running them, by reason: rate_limited or rate_limit_unavailable.",
}, []string{"reason"})

// passThrough are the client headers forwarded: the agent asks for the writer
// and for durable writes. Every other X-Helix-* header is refused.
var passThrough = []string{"X-Helix-Require-Writer", "X-Helix-Await-Durable"}

// Store is what binding an ontology reads of Foundry's store; the databases
// it is bound to are the host's (ports.Databases).
type Store interface {
	GetFoundryOntology(ctx context.Context, workspaceID, id string) (*model.FoundryOntology, error)
}

// Binding is one ontology's database, resolved when an attempt starts.
type Binding struct {
	OntologyID string
	TenantID   string
	// Endpoint is the database's query URL.
	Endpoint string
}

// Bind resolves an ontology to its database's query URL for a job of kind.
// Only a delete job binds a deleted ontology: its loads are deleted from the
// database it still holds. Its errors are gRPC statuses: NotFound for a
// deleted ontology (to any other job) or database, FailedPrecondition for a
// database whose queries are paused or whose host is not serving,
// Unavailable when it can't be resolved safely.
func Bind(ctx context.Context, db Store, dbs ports.Databases, workspaceID, ontologyID string, kind model.FoundryGraphJobKind) (Binding, error) {
	return bind(ctx, db, dbs, workspaceID, ontologyID, kind == model.FoundryGraphJobDelete)
}

// BindRead resolves a live ontology to its database's query URL for a read,
// as Bind does for a job other than a delete.
func BindRead(ctx context.Context, db Store, dbs ports.Databases, workspaceID, ontologyID string) (Binding, error) {
	return bind(ctx, db, dbs, workspaceID, ontologyID, false)
}

func bind(ctx context.Context, db Store, dbs ports.Databases, workspaceID, ontologyID string, deletedToo bool) (Binding, error) {
	o, err := db.GetFoundryOntology(ctx, workspaceID, ontologyID)
	if errors.Is(err, foundrystore.ErrNotFound) || err == nil && o.DeletedAt != nil && !deletedToo {
		return Binding{}, status.Error(codes.NotFound, "the ontology was deleted")
	}
	if err != nil {
		return Binding{}, status.Errorf(codes.Unavailable, "get ontology: %v", err)
	}
	database, err := dbs.Get(ctx, o.TenantID)
	if errors.Is(err, foundrystore.ErrNotFound) || err == nil && (database.Deleted || database.Retired) {
		return Binding{}, status.Error(codes.NotFound, "the ontology's database was deleted")
	}
	if err != nil {
		return Binding{}, status.Errorf(codes.Unavailable, "get tenant: %v", err)
	}
	if database.WorkspaceID != o.WorkspaceID {
		return Binding{}, status.Error(codes.NotFound, "the ontology's database is not in its workspace")
	}
	endpoint, err := dbs.Endpoint(ctx, database.TenantID, QueryPath)
	if err != nil {
		return Binding{}, err
	}
	return Binding{OntologyID: o.ID, TenantID: database.TenantID, Endpoint: endpoint}, nil
}

// Querier sends a query to a database as the control plane (helixgateway.Client).
type Querier interface {
	Query(ctx context.Context, endpoint, tenantID string, body []byte, extra http.Header) (helixgateway.Response, error)
}

// Proxy serves one binding on loopback while its context is live, until Close.
type Proxy struct {
	ctx      context.Context
	binding  Binding
	gateway  Querier
	token    string
	listener net.Listener
	server   *http.Server
	// bound checks the binding again while the proxy serves.
	bound *rechecker
	// admissionWait is how long a query the gateway turned away is sent again.
	admissionWait time.Duration
	now           func() time.Time
}

// Serve starts a proxy for binding on a fresh loopback port with a fresh token.
// Once ctx ends, every request is refused and those in flight are cancelled.
// recheck runs Bind again, at most every RecheckEvery; once it reports the
// database gone or paused (NotFound, FailedPrecondition), the proxy refuses
// every request. Other errors leave it serving until the next check.
// admissionWait is how long a query the gateway turned away before running it
// is sent again; zero relays the refusal at once.
func Serve(ctx context.Context, binding Binding, gateway Querier, recheck func(context.Context) error, admissionWait time.Duration) (*Proxy, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("graphproxy: token: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("graphproxy: listen: %w", err)
	}
	p := &Proxy{
		ctx: ctx, binding: binding, gateway: gateway, token: base64.RawURLEncoding.EncodeToString(secret), listener: ln,
		bound: newRechecker(recheck, time.Now()), admissionWait: admissionWait, now: time.Now,
	}
	// Every request's context derives from ctx, so ending it cancels the gateway calls in flight.
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { _ = p.server.Serve(ln) }() // ends with Close
	return p, nil
}

// URL is the proxy's base URL, for the agent's HelixTarget.url.
func (p *Proxy) URL() string { return "http://" + p.listener.Addr().String() }

// Token is the bearer token the agent must send.
func (p *Proxy) Token() string { return p.token }

// Close stops serving; queries in flight are cut off.
func (p *Proxy) Close() error { return p.server.Close() }

// ServeHTTP checks and forwards one query.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	refuse := refuser(w, r, p.binding.OntologyID)
	if p.ctx.Err() != nil {
		refuse(http.StatusServiceUnavailable, "the attempt this proxy served has ended")
		return
	}
	if !routed(r, refuse) {
		return
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(p.token)) != 1 {
		refuse(http.StatusUnauthorized, "the job's token is required")
		return
	}
	body, extra, ok := readQuery(r, passThrough, refuse)
	if !ok {
		return
	}
	if err := p.stillBound(r.Context()); err != nil {
		refuse(http.StatusForbidden, status.Convert(err).Message())
		return
	}
	f := forwarder{gateway: p.gateway, admissionWait: p.admissionWait, now: p.now}
	f.forward(r.Context(), w, refuse, p.binding, body, extra, p.stillBound)
}

// refuser answers a refused request with code and reason, and logs it.
func refuser(w http.ResponseWriter, r *http.Request, ontologyID string) func(code int, reason string) {
	return func(code int, reason string) {
		log.From(r.Context()).Warn("graph proxy refused a request", "ontology", ontologyID, "reason", reason)
		http.Error(w, reason, code)
	}
}

// routed reports whether r is a POST to QueryPath, refusing it otherwise.
func routed(r *http.Request, refuse func(int, string)) bool {
	if r.URL.Path != QueryPath {
		refuse(http.StatusNotFound, "only "+QueryPath+" is served")
		return false
	}
	if r.Method != http.MethodPost {
		refuse(http.StatusMethodNotAllowed, "only POST is served")
		return false
	}
	return true
}

// readQuery reads a query's body, at most MaxRequestBytes, and the Helix
// headers to forward: those in allowed. Any other X-Helix-* header is refused,
// as the proxy chooses the database.
func readQuery(r *http.Request, allowed []string, refuse func(int, string)) ([]byte, http.Header, bool) {
	extra := http.Header{}
	for name, values := range r.Header {
		if !strings.HasPrefix(strings.ToLower(name), "x-helix-") {
			continue
		}
		canonical := http.CanonicalHeaderKey(name)
		if !slices.Contains(allowed, canonical) {
			refuse(http.StatusBadRequest, "the proxy chooses the database; "+canonical+" is not accepted")
			return nil, nil, false
		}
		extra[canonical] = values
	}
	if r.ContentLength > MaxRequestBytes {
		refuse(http.StatusRequestEntityTooLarge, fmt.Sprintf("a query is at most %d bytes", MaxRequestBytes))
		return nil, nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil {
		refuse(http.StatusBadRequest, "the body could not be read")
		return nil, nil, false
	}
	if len(body) > MaxRequestBytes {
		refuse(http.StatusRequestEntityTooLarge, fmt.Sprintf("a query is at most %d bytes", MaxRequestBytes))
		return nil, nil, false
	}
	return body, extra, true
}

// forwarder sends checked queries to a binding's gateway.
type forwarder struct {
	gateway Querier
	// admissionWait is how long a query the gateway turned away is sent again.
	admissionWait time.Duration
	now           func() time.Time
}

// forward sends body to binding's gateway and relays the answer to w. A query
// the gateway turned away before running it is sent again with backoff until
// admissionWait has passed; stillBound, when set, is checked before each
// resend, and its refusal ends the query.
func (f forwarder) forward(ctx context.Context, w http.ResponseWriter, refuse func(int, string), binding Binding, body []byte, extra http.Header,
	stillBound func(context.Context) error) {
	resp, err := f.gateway.Query(ctx, binding.Endpoint, binding.TenantID, body, extra)
	retryUntil := f.now().Add(f.admissionWait)
	for backoff := firstAdmissionBackoff; err == nil; backoff = min(2*backoff, maxAdmissionBackoff) {
		reason := admissionRefusal(resp)
		wait := backoff/2 + mathrand.N(backoff/2+1)
		if reason == "" || f.now().Add(wait).After(retryUntil) || !sleep(ctx, wait) {
			break
		}
		admissionRetries.WithLabelValues(reason).Inc()
		if stillBound != nil {
			if err := stillBound(ctx); err != nil {
				refuse(http.StatusForbidden, status.Convert(err).Message())
				return
			}
		}
		resp, err = f.gateway.Query(ctx, binding.Endpoint, binding.TenantID, body, extra)
	}
	if err != nil {
		log.From(ctx).Warn("graph proxy: gateway request failed", "ontology", binding.OntologyID, "err", err)
		http.Error(w, "the database could not be reached", http.StatusBadGateway)
		return
	}
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(resp.Body) // best-effort: the agent sees a short body as a failed request
}

// admissionRefusal names why the gateway turned a query away before running
// it: rate_limited, or rate_limit_unavailable when it has no limit loaded for
// the tenant or can't reach its limiter. It is empty for any other answer,
// which may come from a query that ran.
func admissionRefusal(resp helixgateway.Response) string {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusServiceUnavailable:
		var refusal struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(resp.Body, &refusal) == nil && refusal.Error == "rate_limit_unavailable" {
			return refusal.Error
		}
	}
	return ""
}

// sleep waits d, or less when ctx ends first; it reports whether d passed.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// stillBound checks the binding again once RecheckEvery has passed; a refusal
// sticks for the proxy's life.
func (p *Proxy) stillBound(ctx context.Context) error {
	return p.bound.stillBound(ctx, p.now, p.binding.OntologyID)
}

// rechecker checks one binding again, at most every RecheckEvery, for a proxy
// or a read call that may outlive the database it was bound to.
type rechecker struct {
	// recheck runs Bind again; nil never checks.
	recheck func(context.Context) error

	mu      sync.Mutex
	checked time.Time
	// refused is the recheck that ended the binding; every request after it is refused.
	refused error
}

// newRechecker checks with recheck from now on; a nil recheck never refuses.
func newRechecker(recheck func(context.Context) error, now time.Time) *rechecker {
	return &rechecker{recheck: recheck, checked: now}
}

// stillBound runs recheck once RecheckEvery has passed since the last check
// that held. NotFound and FailedPrecondition (the database gone or paused)
// end the binding for good; other errors leave it in use until the next check.
func (c *rechecker) stillBound(ctx context.Context, now func() time.Time, ontologyID string) error {
	if c.recheck == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refused != nil || now().Sub(c.checked) < RecheckEvery {
		return c.refused
	}
	err := c.recheck(ctx)
	switch status.Code(err) {
	case codes.OK:
		c.checked = now()
	case codes.NotFound, codes.FailedPrecondition:
		c.refused = err
	default:
		log.From(ctx).Warn("graph proxy: rechecking the database failed", "ontology", ontologyID, "err", err)
	}
	return c.refused
}
