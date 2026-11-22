package foundryevents

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	foundryapi "github.com/helixdb/helix-foundry/backend/api"
	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sendtoken"
)

// relayToken is the i-th well-formed relay token; relayTok(1) is live.
func relayTok(i int) string { return fmt.Sprintf("hx_rl_%064x", i) }

// relayBackend serves the relay's RPCs the door calls, behind Foundry's actor interceptor. Its
// link is relayTok(1)'s; answer, when set, answers each call but the lookup by its method.
type relayBackend struct {
	foundryv1.UnimplementedFoundryEventsServiceServer
	mu        sync.Mutex
	live      map[string]bool
	lookupErr error
	lookups   int
	calls     []any
	actors    []ports.Actor
	answer    func(method string) error
	hold      chan struct{}
}

func (b *relayBackend) record(ctx context.Context, method string, req any) error {
	b.mu.Lock()
	b.calls = append(b.calls, req)
	a, _ := ports.ActorFrom(ctx)
	b.actors = append(b.actors, a)
	answer, hold := b.answer, b.hold
	b.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if answer != nil {
		return answer(method)
	}
	return nil
}

func (b *relayBackend) last() any {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.calls) == 0 {
		return nil
	}
	return b.calls[len(b.calls)-1]
}

func (b *relayBackend) ValidateFoundryRelayToken(_ context.Context, req *foundryv1.ValidateFoundryRelayTokenRequest) (
	*foundryv1.ValidateFoundryRelayTokenResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lookups++
	if b.lookupErr != nil {
		return nil, b.lookupErr
	}
	if !b.live[req.GetTokenSha256()] {
		return &foundryv1.ValidateFoundryRelayTokenResponse{}, nil
	}
	return &foundryv1.ValidateFoundryRelayTokenResponse{Valid: true, LinkId: "link-1", WorkspaceId: "ws-1"}, nil
}

func (b *relayBackend) RedeemFoundryRelayCode(ctx context.Context, req *foundryv1.RedeemFoundryRelayCodeRequest) (
	*foundryv1.RedeemFoundryRelayCodeResponse, error) {
	if err := b.record(ctx, "redeem", req); err != nil {
		return nil, err
	}
	return &foundryv1.RedeemFoundryRelayCodeResponse{Token: relayTok(9), LinkId: "link-9", WorkspaceId: "ws-1", AccountEmail: "ada@acme.com"}, nil
}

func (b *relayBackend) ClaimFoundryRelayDeliveries(ctx context.Context, req *foundryv1.ClaimFoundryRelayDeliveriesRequest) (
	*foundryv1.ClaimFoundryRelayDeliveriesResponse, error) {
	if err := b.record(ctx, "claim", req); err != nil {
		return nil, err
	}
	return &foundryv1.ClaimFoundryRelayDeliveriesResponse{Deliveries: []*foundryv1.FoundryRelayDelivery{{Id: "d-1", Kind: "event"}}, Acked: 2,
		HostedFeed: true}, nil
}

func (b *relayBackend) CallSlackForRelay(ctx context.Context, req *foundryv1.CallSlackForRelayRequest) (*foundryv1.CallSlackForRelayResponse, error) {
	if err := b.record(ctx, "slack", req); err != nil {
		return nil, err
	}
	if req.GetMethod() == "chat.update" {
		return &foundryv1.CallSlackForRelayResponse{RetryAfterS: 7}, nil
	}
	return &foundryv1.CallSlackForRelayResponse{BodyJson: `{"ok":true,"ts":"1.2"}`}, nil
}

func (b *relayBackend) StartFoundryRelaySlackInstall(ctx context.Context, req *foundryv1.StartFoundryRelaySlackInstallRequest) (
	*foundryv1.StartFoundryRelaySlackInstallResponse, error) {
	if err := b.record(ctx, "install", req); err != nil {
		return nil, err
	}
	return &foundryv1.StartFoundryRelaySlackInstallResponse{AuthorizeUrl: "https://slack.com/oauth/v2/authorize?state=s"}, nil
}

func (b *relayBackend) RevokeFoundryRelayToken(ctx context.Context, req *foundryv1.RevokeFoundryRelayTokenRequest) (
	*foundryv1.RevokeFoundryRelayTokenResponse, error) {
	if err := b.record(ctx, "revoke", req); err != nil {
		return nil, err
	}
	return &foundryv1.RevokeFoundryRelayTokenResponse{}, nil
}

// relayFixture is the door with foundry.slack and its relay on, over a relayBackend.
type relayFixture struct {
	http.Handler
	h       *handler
	backend *relayBackend
}

func newRelayDoorFixture(t *testing.T, change func(*config.EventsConfig)) relayFixture {
	t.Helper()
	cfg := config.Defaults().Foundry.Events
	if change != nil {
		change(&cfg)
	}
	backend := &relayBackend{live: map[string]bool{sendtoken.Hash(relayTok(1)): true}}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnaryInterceptor(foundryapi.ActorInterceptor()))
	foundryv1.RegisterFoundryEventsServiceServer(server, backend)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///backend",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	clock := &clock{at: t0}
	h := newHandler(foundryv1.NewFoundryEventsServiceClient(conn), cfg, config.SlackConfig{Enabled: true, Relay: config.SlackRelayConfig{Enabled: true}},
		discard(), clock.now)
	return relayFixture{Handler: h.routes(), h: h, backend: backend}
}

// relayRequest is a request to path with body, bearing token when it isn't "".
func relayRequest(path, token, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-Forwarded-For", "192.0.2.50, "+address)
	return r
}

func (f relayFixture) do(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, r)
	return rec
}

func TestRelayRoutesFollowTheConfig(t *testing.T) {
	d := newDoor(t, nil)
	assert.Equal(t, http.StatusNotFound, d.do(relayRequest("/v1/relay/claim", relayTok(1), "{}")).Code, "relay off")
	h := newHandler(nil, config.Defaults().Foundry.Events, config.SlackConfig{Relay: config.SlackRelayConfig{Enabled: true}}, discard(), time.Now)
	assert.Nil(t, h.relay, "the relay needs foundry.slack")
	f := newRelayDoorFixture(t, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, f.do(httptest.NewRequest(http.MethodGet, "/v1/relay/claim", nil)).Code)
}

func TestRelayToken(t *testing.T) {
	f := newRelayDoorFixture(t, nil)
	rec := f.do(relayRequest("/v1/relay/token", "", `{"code":"c-1","code_verifier":"v-1"}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, map[string]any{"token": relayTok(9), "link_id": "link-9", "workspace_id": "ws-1", "account_email": "ada@acme.com"}, out)
	req := f.backend.last().(*foundryv1.RedeemFoundryRelayCodeRequest)
	assert.Equal(t, "c-1", req.GetCode())
	assert.Equal(t, "v-1", req.GetCodeVerifier())
	assert.Equal(t, ports.Actor{Source: ports.SourceEventSource}, f.backend.actors[0], "as the door")

	requireRefused(t, f.do(relayRequest("/v1/relay/token", "", `{"code":`)), http.StatusBadRequest, codeInvalidArguments)
	requireRefused(t, f.do(relayRequest("/v1/relay/token", "", `{"unknown":1}`)), http.StatusBadRequest, codeInvalidArguments)
	text := relayRequest("/v1/relay/token", "", `{}`)
	text.Header.Set("Content-Type", "text/plain")
	requireRefused(t, f.do(text), http.StatusUnsupportedMediaType, codeUnsupportedMediaType)
	big := relayRequest("/v1/relay/token", "", `{}`)
	big.ContentLength = config.MaxSlackRelayCallBytes + 1
	requireRefused(t, f.do(big), http.StatusRequestEntityTooLarge, codePayloadTooLarge)
	f.backend.mu.Lock()
	f.backend.answer = func(string) error { return status.Error(codes.NotFound, "this link expired") }
	f.backend.mu.Unlock()
	refusal := requireRefused(t, f.do(relayRequest("/v1/relay/token", "", `{"code":"c","code_verifier":"v"}`)), http.StatusNotFound, codeNotFound)
	assert.Equal(t, "this link expired", refusal.Message)
}

func TestRelayClaim(t *testing.T) {
	f := newRelayDoorFixture(t, nil)
	rec := f.do(relayRequest("/v1/relay/claim", relayTok(1), `{"acks":["d-0"],"wait_ms":1000,"max":5,"feeds":[{"local_workspace_id":"L1","on":true}],`+
		`"token_sha256":"forged","client_address":"forged"}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"deliveries":[{"id":"d-1","kind":"event"}],"acked":2,"hosted_feed":true}`, rec.Body.String())
	req := f.backend.last().(*foundryv1.ClaimFoundryRelayDeliveriesRequest)
	assert.Equal(t, sendtoken.Hash(relayTok(1)), req.GetTokenSha256(), "the door's hash, never the token")
	assert.Equal(t, address, req.GetClientAddress())
	assert.Equal(t, []string{"d-0"}, req.GetAcks())
	assert.EqualValues(t, 1000, req.GetWaitMs())
	assert.Equal(t, "L1", req.GetFeeds()[0].GetLocalWorkspaceId())

	// An empty body is a claim of nothing, waiting none.
	rec = f.do(relayRequest("/v1/relay/claim", relayTok(1), ``))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireRefused(t, f.do(relayRequest("/v1/relay/claim", relayTok(1), `{"wait_ms":20001}`)), http.StatusBadRequest, codeInvalidArguments)
	requireRefused(t, f.do(relayRequest("/v1/relay/claim", relayTok(1), `{"wait_ms":-1}`)), http.StatusBadRequest, codeInvalidArguments)

	// Tokens: none, malformed, unknown.
	requireRefused(t, f.do(relayRequest("/v1/relay/claim", "", `{}`)), http.StatusUnauthorized, codeUnauthorized)
	requireRefused(t, f.do(relayRequest("/v1/relay/claim", token(1), `{}`)), http.StatusUnauthorized, codeInvalidToken)
	requireRefused(t, f.do(relayRequest("/v1/relay/claim", relayTok(2), `{}`)), http.StatusUnauthorized, codeInvalidToken)

	// One claim of a link waits at a time on a pod.
	hold := make(chan struct{})
	f.backend.mu.Lock()
	f.backend.hold = hold
	f.backend.mu.Unlock()
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- f.do(relayRequest("/v1/relay/claim", relayTok(1), `{"wait_ms":20000}`)) }()
	require.Eventually(t, func() bool {
		f.h.relay.mu.Lock()
		defer f.h.relay.mu.Unlock()
		return f.h.relay.claims["link-1"]
	}, 5*time.Second, time.Millisecond)
	requireRefused(t, f.do(relayRequest("/v1/relay/claim", relayTok(1), `{}`)), http.StatusTooManyRequests, codeRateLimited)
	close(hold)
	assert.Equal(t, http.StatusOK, (<-first).Code)
	f.backend.mu.Lock()
	f.backend.hold = nil
	f.backend.mu.Unlock()
	assert.Equal(t, http.StatusOK, f.do(relayRequest("/v1/relay/claim", relayTok(1), `{}`)).Code, "the link's place is given back")

	// The pod's places for waiting claims.
	full := newRelayDoorFixture(t, func(c *config.EventsConfig) { c.Endpoint.RelayMaxWaiting = 1 })
	require.True(t, full.h.relay.waiting.TryAcquire(1))
	requireRefused(t, full.do(relayRequest("/v1/relay/claim", relayTok(1), `{}`)), http.StatusTooManyRequests, codeRateLimited)
	full.h.relay.waiting.Release(1)
	assert.Equal(t, http.StatusOK, full.do(relayRequest("/v1/relay/claim", relayTok(1), `{}`)).Code)
}

func TestRelaySlackCalls(t *testing.T) {
	f := newRelayDoorFixture(t, nil)
	rec := f.do(relayRequest("/v1/relay/slack/chat.postMessage", relayTok(1), `{"local_workspace_id":"L1","args":{"channel":"C1","text":"hi"}}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"ok":true,"ts":"1.2"}`, rec.Body.String(), "Slack's answer, as it is")
	req := f.backend.last().(*foundryv1.CallSlackForRelayRequest)
	assert.Equal(t, "chat.postMessage", req.GetMethod())
	assert.Equal(t, "L1", req.GetLocalWorkspaceId())
	assert.JSONEq(t, `{"channel":"C1","text":"hi"}`, req.GetBodyJson())
	assert.Equal(t, sendtoken.Hash(relayTok(1)), req.GetTokenSha256())

	rec = f.do(relayRequest("/v1/relay/slack/chat.update", relayTok(1), `{"local_workspace_id":"L1","args":{}}`))
	requireRefused(t, rec, http.StatusTooManyRequests, codeRateLimited)
	assert.Equal(t, "7", rec.Header().Get("Retry-After"), "Slack takes it later")

	for _, body := range []string{`{"channel":"C1"}`, `{"local_workspace_id":"L1","args":{}} {}`, `nope`} {
		requireRefused(t, f.do(relayRequest("/v1/relay/slack/chat.postMessage", relayTok(1), body)), http.StatusBadRequest, codeInvalidArguments)
	}

	// The backend's refusals, by code.
	for code, want := range map[codes.Code]int{
		codes.InvalidArgument: http.StatusBadRequest, codes.PermissionDenied: http.StatusForbidden, codes.NotFound: http.StatusNotFound,
		codes.FailedPrecondition: http.StatusConflict, codes.ResourceExhausted: http.StatusTooManyRequests,
		codes.Unavailable: http.StatusServiceUnavailable, codes.DeadlineExceeded: http.StatusServiceUnavailable, codes.Internal: http.StatusInternalServerError,
	} {
		f.backend.mu.Lock()
		f.backend.answer = func(string) error {
			st, _ := status.New(code, "refused").WithDetails(&errdetails.ErrorInfo{Reason: "FOUNDRY_RELAY_DISABLED"})
			return st.Err()
		}
		f.backend.mu.Unlock()
		rec := f.do(relayRequest("/v1/relay/slack/chat.postMessage", relayTok(1), `{"local_workspace_id":"L1","args":{}}`))
		assert.Equal(t, want, rec.Code, code.String())
		if code == codes.FailedPrecondition {
			assert.Equal(t, "FOUNDRY_RELAY_DISABLED", errorOf(t, rec).Reason)
		}
	}

	// A token the backend finds revoked is forgotten: its next call is looked up again.
	f.backend.mu.Lock()
	f.backend.answer = func(string) error { return status.Error(codes.Unauthenticated, "revoked") }
	f.backend.mu.Unlock()
	requireRefused(t, f.do(relayRequest("/v1/relay/slack/chat.postMessage", relayTok(1), `{"local_workspace_id":"L1","args":{}}`)),
		http.StatusUnauthorized, codeInvalidToken)
	f.backend.mu.Lock()
	lookups := f.backend.lookups
	delete(f.backend.live, sendtoken.Hash(relayTok(1)))
	f.backend.mu.Unlock()
	requireRefused(t, f.do(relayRequest("/v1/relay/slack/chat.postMessage", relayTok(1), `{"local_workspace_id":"L1","args":{}}`)),
		http.StatusUnauthorized, codeInvalidToken)
	f.backend.mu.Lock()
	defer f.backend.mu.Unlock()
	assert.Equal(t, lookups+1, f.backend.lookups)
}

func TestRelayInstallAndRevoke(t *testing.T) {
	f := newRelayDoorFixture(t, nil)
	rec := f.do(relayRequest("/v1/relay/slack-install", relayTok(1), `{"local_workspace_id":"L1","return_to":"http://localhost:3000/settings/slack"}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"authorize_url":"https://slack.com/oauth/v2/authorize?state=s"}`, rec.Body.String())
	req := f.backend.last().(*foundryv1.StartFoundryRelaySlackInstallRequest)
	assert.Equal(t, "http://localhost:3000/settings/slack", req.GetReturnTo())
	assert.Equal(t, sendtoken.Hash(relayTok(1)), req.GetTokenSha256())
	requireRefused(t, f.do(relayRequest("/v1/relay/slack-install", relayTok(1), `[]`)), http.StatusBadRequest, codeInvalidArguments)

	revoke := relayRequest("/v1/relay/revoke", relayTok(1), "")
	revoke.Header.Del("Content-Type")
	rec = f.do(revoke)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{}`, rec.Body.String())
	assert.Equal(t, sendtoken.Hash(relayTok(1)), f.backend.last().(*foundryv1.RevokeFoundryRelayTokenRequest).GetTokenSha256())
}

func TestRelayGate(t *testing.T) {
	f := newRelayDoorFixture(t, func(c *config.EventsConfig) {
		c.Endpoint.Relay.AddressRequestsPerSecond, c.Endpoint.Relay.AddressBurstRequests = 1, 2
	})
	for range 2 {
		require.Equal(t, http.StatusOK, f.do(relayRequest("/v1/relay/revoke", relayTok(1), "")).Code)
	}
	requireRefused(t, f.do(relayRequest("/v1/relay/revoke", relayTok(1), "")), http.StatusTooManyRequests, codeRateLimited)

	// The pod's room, for calls but claims.
	g := newRelayDoorFixture(t, nil)
	var releases []func()
	for range g.h.relay.gate.perAddress {
		leave, ok := g.h.relay.gate.addressPlace(address)
		require.True(t, ok)
		releases = append(releases, leave)
	}
	requireRefused(t, g.do(relayRequest("/v1/relay/revoke", relayTok(1), "")), http.StatusTooManyRequests, codeRateLimited)
	for _, leave := range releases {
		leave()
	}
	require.True(t, g.h.relay.gate.inFlight.TryAcquire(int64(config.Defaults().Foundry.Events.Endpoint.Relay.MaxInFlight)))
	requireRefused(t, g.do(relayRequest("/v1/relay/revoke", relayTok(1), "")), http.StatusTooManyRequests, codeRateLimited)
}

// A token is admitted as a send token is: a lookup that can't answer is 503, and an address that
// guessed too many is 429, looking nothing up.
func TestRelayAdmission(t *testing.T) {
	f := newRelayDoorFixture(t, nil)
	f.backend.mu.Lock()
	f.backend.lookupErr = status.Error(codes.Unavailable, "mysql down")
	f.backend.mu.Unlock()
	requireRefused(t, f.do(relayRequest("/v1/relay/revoke", relayTok(1), "")), http.StatusServiceUnavailable, codeBackendUnavailable)
	f.backend.mu.Lock()
	f.backend.lookupErr = nil
	f.backend.mu.Unlock()
	for i := range sendtoken.FailuresPerMinute {
		requireRefused(t, f.do(relayRequest("/v1/relay/revoke", relayTok(100+i), "")), http.StatusUnauthorized, codeInvalidToken)
	}
	rec := f.do(relayRequest("/v1/relay/revoke", relayTok(1), ""))
	requireRefused(t, rec, http.StatusTooManyRequests, codeRateLimited)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
}

func TestRelayFailure(t *testing.T) {
	assert.Equal(t, http.StatusUnauthorized, relayFailure(status.Error(codes.Unauthenticated, "x")).status)
	assert.Equal(t, http.StatusServiceUnavailable, relayFailure(status.Error(codes.Canceled, "x")).status)
	assert.Equal(t, http.StatusInternalServerError, relayFailure(fmt.Errorf("plain")).status)
	assert.True(t, relayBearer{LinkID: "l", WorkspaceID: "w"}.Complete())
	assert.False(t, relayBearer{LinkID: "l"}.Complete())
}
