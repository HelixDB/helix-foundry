package graphproxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/status"
)

// readPassThrough are the client headers a read may send: the agent reads
// from the writer, so a read sees every write acknowledged before it. A read
// never awaits durability, so X-Helix-Await-Durable is refused.
var readPassThrough = []string{"X-Helix-Require-Writer"}

// ReadProxy lets ontology agent reads reach Foundry ontologies' databases,
// never writing them. One serves a whole pod on loopback: each read call (a
// foundry-read Read, or an attempt of a question) registers its binding for
// its own context and gets a token of its own, which works only while that
// context is live and, for a call given a recheck, while its binding holds.
//
// A request is forwarded only when it is POST /v2/query with a live call's
// token, carries no Helix header but x-helix-require-writer, is at most
// MaxRequestBytes, and is a JSON object whose request_type is "read" (and
// has no requestType alias). The body forwarded is that object written
// again, so the gateway reads the same one request_type the proxy checked.
type ReadProxy struct {
	forwarder
	listener net.Listener
	server   *http.Server

	mu    sync.Mutex
	calls map[string]readCall
}

// readCall is one registered read call: its binding, while ctx is live and
// bound holds.
type readCall struct {
	ctx     context.Context
	binding Binding
	bound   *rechecker
}

// ServeReadOnly starts a read-only proxy on a fresh loopback port. A query
// the gateway turned away before running it is sent again for up to
// admissionWait.
func ServeReadOnly(gateway Querier, admissionWait time.Duration) (*ReadProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("graphproxy: listen: %w", err)
	}
	p := &ReadProxy{
		forwarder: forwarder{gateway: gateway, admissionWait: admissionWait, now: time.Now},
		listener:  ln, calls: map[string]readCall{},
	}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.server.Serve(ln) }() // ends with Close
	return p, nil
}

// URL is the proxy's base URL, for the agent's HelixTarget.url.
func (p *ReadProxy) URL() string { return "http://" + p.listener.Addr().String() }

// Close stops serving; queries in flight are cut off.
func (p *ReadProxy) Close() error { return p.server.Close() }

// Register lets one call's queries reach binding until ctx ends, and returns
// the bearer token they must carry. Once ctx ends the token is refused and
// the call's queries in flight are cancelled.
//
// recheck runs Bind again, at most every RecheckEvery, as Serve's does: once
// it reports the database gone or paused (NotFound, FailedPrecondition), the
// call's queries are refused. A call that ends within RecheckEvery, or a nil
// recheck, is never checked again.
func (p *ReadProxy) Register(ctx context.Context, binding Binding, recheck func(context.Context) error) (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("graphproxy: token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	p.mu.Lock()
	p.calls[token] = readCall{ctx: ctx, binding: binding, bound: newRechecker(recheck, p.now())}
	p.mu.Unlock()
	context.AfterFunc(ctx, func() {
		p.mu.Lock()
		delete(p.calls, token)
		p.mu.Unlock()
	})
	return token, nil
}

// registered is the live call token names; ok is false for any other token.
func (p *ReadProxy) registered(r *http.Request) (readCall, bool) {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return readCall{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for token, call := range p.calls {
		// Compared in constant time, as the job proxy's single token is.
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1 {
			return call, call.ctx.Err() == nil
		}
	}
	return readCall{}, false
}

// ServeHTTP checks and forwards one read.
func (p *ReadProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call, registered := p.registered(r)
	refuse := refuser(w, r, call.binding.OntologyID)
	if !routed(r, refuse) {
		return
	}
	if !registered {
		refuse(http.StatusUnauthorized, "a live read's token is required")
		return
	}
	body, extra, ok := readQuery(r, readPassThrough, refuse)
	if !ok {
		return
	}
	stillBound := func(ctx context.Context) error { return call.bound.stillBound(ctx, p.now, call.binding.OntologyID) }
	if err := stillBound(r.Context()); err != nil {
		refuse(http.StatusForbidden, status.Convert(err).Message())
		return
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		refuse(http.StatusBadRequest, "the body is not a query request")
		return
	}
	var requestType string
	if _, alias := envelope["requestType"]; alias || json.Unmarshal(envelope["request_type"], &requestType) != nil || requestType != "read" {
		refuse(http.StatusForbidden, `only queries whose request_type is "read" are served`)
		return
	}
	// Written again from the object checked: a duplicate request_type the
	// gateway might read in place of this one is gone.
	body, err := json.Marshal(envelope)
	if err != nil {
		refuse(http.StatusBadRequest, "the body is not a query request")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(call.ctx, cancel)
	defer stop()
	p.forward(ctx, w, refuse, call.binding, body, extra, stillBound)
}
