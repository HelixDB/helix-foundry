package foundryevents

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryapi "github.com/helixdb/helix-foundry/backend/api"
	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sendtoken"
)

// address is the client the tests send from, as the load balancer's last X-Forwarded-For hop,
// and elsewhere another one.
const address, elsewhere = "203.0.113.9", "198.51.100.7"

const (
	sendPath    = "/v1/sources/source-1/pieces"
	clientToken = "4f8d2c51-9b7a-4e0f-8c3d-2a1b0c9d8e7f"
	// validBody is a send of one piece.
	validBody = `{"client_token":"` + clientToken + `","pieces":[{"id":"o-1","data":{"total":12}}]}`
)

var (
	t0        = time.Unix(1_700_000_000, 0)
	liveToken = token(1)
	sender1   = sendtoken.Sender{TokenID: "token-1", WorkspaceID: "workspace-1", ConnectorID: "source-1"}
)

// token is the i-th well-formed send token.
func token(i int) string { return fmt.Sprintf("hx_ev_%064x", i) }

// fakeBackend serves the two RPCs the door calls. It is reached over gRPC behind Foundry's actor
// interceptor, so each request arrives as the backend would see it, with its Actor. live are the
// tokens it finds by hash, and answer answers each send (a stored batch when nil).
type fakeBackend struct {
	foundryv1.UnimplementedFoundryEventsServiceServer
	lookups, sends atomic.Int32

	mu        sync.Mutex
	live      map[string]sendtoken.Sender
	lookupErr error
	answer    func(context.Context, *foundryv1.SendFoundryEventsRequest) (*foundryv1.SendFoundryEventsResponse, error)
	received  []*foundryv1.SendFoundryEventsRequest
	// actors are the Actors of received, and lookupActor the last lookup's.
	actors      []ports.Actor
	lookupActor ports.Actor
}

func (b *fakeBackend) ValidateFoundryEventSourceToken(ctx context.Context, req *foundryv1.ValidateFoundryEventSourceTokenRequest) (
	*foundryv1.ValidateFoundryEventSourceTokenResponse, error) {
	b.lookups.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lookupActor, _ = ports.ActorFrom(ctx)
	if b.lookupErr != nil {
		return nil, b.lookupErr
	}
	sender, ok := b.live[req.GetHash()]
	return &foundryv1.ValidateFoundryEventSourceTokenResponse{
		Valid: ok, TokenId: sender.TokenID, WorkspaceId: sender.WorkspaceID, ConnectorId: sender.ConnectorID,
	}, nil
}

func (b *fakeBackend) SendFoundryEvents(ctx context.Context, req *foundryv1.SendFoundryEventsRequest) (*foundryv1.SendFoundryEventsResponse, error) {
	b.sends.Add(1)
	b.mu.Lock()
	b.received = append(b.received, req)
	actor, _ := ports.ActorFrom(ctx)
	b.actors = append(b.actors, actor)
	answer := b.answer
	b.mu.Unlock()
	if answer != nil {
		return answer(ctx, req)
	}
	return stored(int32(len(req.GetPieces()))), nil
}

// stored is the backend's answer to a stored send of pieces.
func stored(pieces int32) *foundryv1.SendFoundryEventsResponse {
	return &foundryv1.SendFoundryEventsResponse{
		Batch:     &foundryv1.FoundryEventBatch{Id: "batch-1", Seq: 7, Pieces: pieces, CreatedAt: timestamppb.New(t0)},
		NewFields: []*foundryv1.EventSourceField{{Name: "total", Type: "number"}},
		Capacity:  &foundryv1.EventSourceCapacity{Rows: 10, MaxRows: 100, Bytes: 500, MaxBytes: 1000},
	}
}

// set makes the backend answer sends with answer.
func (b *fakeBackend) set(answer func(context.Context, *foundryv1.SendFoundryEventsRequest) (*foundryv1.SendFoundryEventsResponse, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answer = answer
}

// refuse makes the backend answer every send err.
func (b *fakeBackend) refuse(err error) {
	b.set(func(context.Context, *foundryv1.SendFoundryEventsRequest) (*foundryv1.SendFoundryEventsResponse, error) {
		return nil, err
	})
}

// hold makes each send say so on entered, then wait for release to close and answer err, or a
// stored batch when err is nil.
func (b *fakeBackend) hold(err error) (entered chan struct{}, release chan struct{}) {
	entered, release = make(chan struct{}, 128), make(chan struct{})
	b.set(func(_ context.Context, req *foundryv1.SendFoundryEventsRequest) (*foundryv1.SendFoundryEventsResponse, error) {
		entered <- struct{}{}
		<-release
		if err != nil {
			return nil, err
		}
		return stored(int32(len(req.GetPieces()))), nil
	})
	return entered, release
}

func (b *fakeBackend) revoke(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.live, sendtoken.Hash(token))
}

func (b *fakeBackend) last() *foundryv1.SendFoundryEventsRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.received[len(b.received)-1]
}

// dial serves backend on an in-memory listener behind Foundry's actor interceptor, as the backend
// serves FoundryEventsService, and dials it as a host does.
func dial(t *testing.T, backend *fakeBackend) foundryv1.FoundryEventsServiceClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnaryInterceptor(foundryapi.ActorInterceptor()))
	foundryv1.RegisterFoundryEventsServiceServer(server, backend)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///backend",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return foundryv1.NewFoundryEventsServiceClient(conn)
}

// clock is the door's clock, still unless a test moves it.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// door is the route over a fake backend that finds liveToken live as sender1.
type door struct {
	http.Handler
	h       *handler
	backend *fakeBackend
	clock   *clock
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newDoor is a door of the default foundry.events, after change.
func newDoor(t *testing.T, change func(*config.EventsConfig)) *door {
	t.Helper()
	cfg := config.Defaults().Foundry.Events
	if change != nil {
		change(&cfg)
	}
	backend := &fakeBackend{live: map[string]sendtoken.Sender{sendtoken.Hash(liveToken): sender1}}
	clock := &clock{at: t0}
	h := newHandler(dial(t, backend), cfg, discard(), clock.now)
	return &door{Handler: h.routes(), h: h, backend: backend, clock: clock}
}

// post is a send of body to source-1 from address, bearing liveToken.
func post(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, sendPath, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+liveToken)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-For", "192.0.2.50, "+address)
	return r
}

func (d *door) do(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, r)
	return rec
}

// errorOf is a refusal's error body, after checking it is JSON.
func errorOf(t *testing.T, rec *httptest.ResponseRecorder) errorDetail {
	t.Helper()
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var out errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return out.Error
}

// requireRefused checks rec is status with code.
func requireRefused(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) errorDetail {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	refusal := errorOf(t, rec)
	require.Equal(t, code, refusal.Code, refusal.Message)
	return refusal
}

// padded is validBody grown with spaces to size bytes: valid JSON whose data is small once
// compacted.
func padded(size int) string {
	return validBody[:len(validBody)-1] + strings.Repeat(" ", size-len(validBody)) + "}"
}

// unread is a body a test expects never to be read.
type unread struct{ t *testing.T }

func (u unread) Read([]byte) (int, error) {
	u.t.Error("the body was read")
	return 0, io.EOF
}

// parallel runs n requests made by request at once, and returns their answers once all are in.
func (d *door) parallel(n int, request func(i int) *http.Request) []*httptest.ResponseRecorder {
	answers := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { answers[i] = d.do(request(i)) })
	}
	wg.Wait()
	return answers
}

// waitFor takes n sends from entered.
func waitFor(t *testing.T, entered chan struct{}, n int) {
	t.Helper()
	for range n {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("sends did not reach the backend")
		}
	}
}

// requireIdle checks the gate holds nothing: every place in flight, every address's and every
// byte is free.
func (d *door) requireIdle(t *testing.T, cfg config.EventsEndpointConfig) {
	t.Helper()
	require.True(t, d.h.gate.inFlight.TryAcquire(int64(cfg.MaxInFlight)), "a place in flight is held")
	d.h.gate.inFlight.Release(int64(cfg.MaxInFlight))
	require.True(t, d.h.gate.bytes.TryAcquire(cfg.MaxBytesInFlight), "bytes are held")
	d.h.gate.bytes.Release(cfg.MaxBytesInFlight)
	require.Zero(t, d.addressPlaces(), "an address's place is held")
}

// addressPlaces counts the places in flight the gate holds for addresses.
func (d *door) addressPlaces() int {
	d.h.gate.mu.Lock()
	defer d.h.gate.mu.Unlock()
	n := 0
	for _, held := range d.h.gate.held {
		n += held
	}
	return n
}

// from is a send from the i-th of four addresses, so 32 at once fit their places in flight.
func from(i int) *http.Request {
	r := post(validBody)
	r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i%4+1))
	return r
}

// A valid send is forwarded and answered with the MCP's answer.
func TestSend(t *testing.T) {
	d := newDoor(t, nil)
	request := post(validBody)
	request.Header.Set("X-Request-ID", "req-1")
	rec := d.do(request)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "req-1", rec.Header().Get("X-Request-ID"))
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"), "server-side senders only: no CORS")
	assert.JSONEq(t, `{"event_source_id":"source-1","seq":"7","pieces":1,"received_at":"2023-11-14T22:13:20Z","replayed":false,`+
		`"new_fields":[{"name":"total","type":"number"}],"capacity":{"rows":"10","max_rows":"100","bytes":"500","max_bytes":"1000","percent_full":50},`+
		`"next":"`+next+`"}`, rec.Body.String())
	assert.Equal(t, int32(1), d.backend.lookups.Load())

	// An X-Request-ID the door wouldn't echo is replaced.
	request = post(validBody)
	request.Header.Set("X-Request-ID", "not one\n")
	rec = d.do(request)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, rec.Header().Get("X-Request-ID"), 36)
}

// Each client address has a bucket, charged before anything else.
func TestAddressBucket(t *testing.T) {
	// A flood from one address is refused before any lookup, while another sends.
	t.Run("refuses an address's 101st request in a second, before any lookup", func(t *testing.T) {
		d := newDoor(t, nil)
		for i := range 100 {
			require.Equal(t, http.StatusOK, d.do(post(validBody)).Code, "request %d", i+1)
		}
		rec := d.do(post(validBody))
		requireRefused(t, rec, http.StatusTooManyRequests, codeRateLimited)
		assert.Equal(t, "1", rec.Header().Get("Retry-After"))
		assert.Equal(t, int32(1), d.backend.lookups.Load(), "one lookup, then the cache")
		assert.Equal(t, int32(100), d.backend.sends.Load())

		// Unauthenticated, it is refused just the same: the bucket comes before the token.
		unsigned := post(validBody)
		unsigned.Header.Del("Authorization")
		requireRefused(t, d.do(unsigned), http.StatusTooManyRequests, codeRateLimited)

		// Only the last hop counts: one the client wrote can't escape the bucket.
		spoofed := post(validBody)
		spoofed.Header.Set("X-Forwarded-For", elsewhere+", "+address)
		requireRefused(t, d.do(spoofed), http.StatusTooManyRequests, codeRateLimited)

		other := post(validBody)
		other.Header.Set("X-Forwarded-For", address+", "+elsewhere)
		require.Equal(t, http.StatusOK, d.do(other).Code, "another address still sends")

		// The bucket refills.
		d.clock.advance(time.Second)
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code)
	})

	// Without X-Forwarded-For, the peer's address is the client's.
	t.Run("keys a request without X-Forwarded-For by its peer", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) { c.Endpoint.AddressBurstRequests = 1 })
		direct := func() *http.Request {
			r := post(validBody)
			r.Header.Del("X-Forwarded-For")
			return r
		}
		require.Equal(t, http.StatusOK, d.do(direct()).Code)
		requireRefused(t, d.do(direct()), http.StatusTooManyRequests, codeRateLimited)
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code, "a forwarded address has its own bucket")
	})
}

// The headers are checked before the pod's room and the token: a body too large or not JSON is
// never looked at.
func TestHeaders(t *testing.T) {
	t.Run("refuses a Content-Length past the cap with no lookup", func(t *testing.T) {
		d := newDoor(t, nil)
		r := httptest.NewRequest(http.MethodPost, sendPath, unread{t})
		r.Header = post("").Header
		r.ContentLength = 3 << 20
		requireRefused(t, d.do(r), http.StatusRequestEntityTooLarge, codePayloadTooLarge)
		assert.Zero(t, d.backend.lookups.Load())
	})

	t.Run("takes a body of exactly the cap", func(t *testing.T) {
		d := newDoor(t, nil)
		rec := d.do(post(padded(2 << 20)))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Len(t, d.backend.last().GetPieces()[0].GetDataJson(), len(`{"total":12}`))
	})

	// A chunked body has no length to check first: it is cut off at the cap.
	t.Run("refuses a chunked body past the cap", func(t *testing.T) {
		d := newDoor(t, nil)
		r := post(padded(2<<20 + 1))
		r.ContentLength = -1
		requireRefused(t, d.do(r), http.StatusRequestEntityTooLarge, codePayloadTooLarge)
		assert.Zero(t, d.backend.sends.Load())
	})

	for _, c := range []struct {
		name, contentType, encoding string
		ok                          bool
	}{
		{name: "application/json", contentType: "application/json", ok: true},
		{name: "any case", contentType: "Application/JSON", ok: true},
		{name: "charset utf-8", contentType: "application/json; charset=utf-8", ok: true},
		{name: "charset UTF-8", contentType: "application/json; charset=UTF-8", ok: true},
		{name: "identity encoding", contentType: "application/json", encoding: "identity", ok: true},
		{name: "charset iso-8859-1", contentType: "application/json; charset=iso-8859-1"},
		{name: "charset latin1", contentType: "application/json; charset=latin1"},
		{name: "text/plain", contentType: "text/plain"},
		{name: "a missing type", contentType: ""},
		{name: "a malformed type", contentType: "application/json; charset"},
		{name: "a JSON suffix type", contentType: "application/vnd.api+json"},
		{name: "gzip", contentType: "application/json", encoding: "gzip"},
		{name: "deflate", contentType: "application/json", encoding: "deflate"},
	} {
		t.Run("content type "+c.name, func(t *testing.T) {
			d := newDoor(t, nil)
			r := post(validBody)
			r.Header.Set("Content-Type", c.contentType)
			if c.encoding != "" {
				r.Header.Set("Content-Encoding", c.encoding)
			}
			rec := d.do(r)
			if c.ok {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				return
			}
			requireRefused(t, rec, http.StatusUnsupportedMediaType, codeUnsupportedMediaType)
			assert.Zero(t, d.backend.lookups.Load())
		})
	}

	t.Run("refuses two Content-Encoding headers", func(t *testing.T) {
		d := newDoor(t, nil)
		r := post(validBody)
		r.Header.Add("Content-Encoding", "identity")
		r.Header.Add("Content-Encoding", "gzip")
		requireRefused(t, d.do(r), http.StatusUnsupportedMediaType, codeUnsupportedMediaType)
	})
}

// The pod's gate refuses at once a request past its rate, its places in flight or its bytes in
// flight, and holds nothing for it.
func TestPodGate(t *testing.T) {
	t.Run("refuses the 33rd request in flight", func(t *testing.T) {
		d := newDoor(t, nil)
		entered, release := d.backend.hold(nil)
		done := make(chan []*httptest.ResponseRecorder)
		go func() { done <- d.parallel(32, from) }()
		waitFor(t, entered, 32)
		rec := d.do(post(validBody))
		requireRefused(t, rec, http.StatusTooManyRequests, codeRateLimited)
		assert.Equal(t, "1", rec.Header().Get("Retry-After"))
		close(release)
		for _, rec := range <-done {
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}
		d.requireIdle(t, config.Defaults().Foundry.Events.Endpoint)
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code)
	})

	t.Run("refuses a request past the pod's rate", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) { c.Endpoint.RequestsPerSecond, c.Endpoint.BurstRequests = 1, 3 })
		for range 3 {
			require.Equal(t, http.StatusOK, d.do(post(validBody)).Code)
		}
		other := post(validBody)
		other.Header.Set("X-Forwarded-For", elsewhere)
		rec := d.do(other)
		requireRefused(t, rec, http.StatusTooManyRequests, codeRateLimited)
		assert.Equal(t, "1", rec.Header().Get("Retry-After"))
		d.clock.advance(time.Second)
		require.Equal(t, http.StatusOK, d.do(other).Code)
	})

	for _, c := range []struct {
		name   string
		length int64
	}{
		{"weighs a body by its Content-Length", 2 << 20},
		{"weighs a chunked body as the cap", -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			change := func(cfg *config.EventsConfig) { cfg.Endpoint.MaxBytesInFlight = 4 << 20 }
			d := newDoor(t, change)
			entered, release := d.backend.hold(nil)
			done := make(chan []*httptest.ResponseRecorder)
			go func() {
				done <- d.parallel(2, func(int) *http.Request {
					body := validBody
					if c.length > 0 {
						body = padded(int(c.length))
					}
					r := post(body)
					r.ContentLength = c.length
					return r
				})
			}()
			waitFor(t, entered, 2)
			// 4 MiB are held: a third request of any size waits for none of it.
			requireRefused(t, d.do(post(validBody)), http.StatusTooManyRequests, codeRateLimited)
			close(release)
			for _, rec := range <-done {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}
			cfg := config.Defaults().Foundry.Events
			change(&cfg)
			d.requireIdle(t, cfg.Endpoint)
		})
	}

	// A request refused anywhere after the gate gives its room back.
	t.Run("gives the room of every refused request back", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) { c.Endpoint.MaxInFlight = 1 })
		unsigned := post(validBody)
		unsigned.Header.Del("Authorization")
		requireRefused(t, d.do(unsigned), http.StatusUnauthorized, codeUnauthorized)
		requireRefused(t, d.do(post(`{}`)), http.StatusBadRequest, codeInvalidArguments)
		d.backend.refuse(status.Error(codes.Unavailable, "down"))
		requireRefused(t, d.do(post(validBody)), http.StatusServiceUnavailable, codeBackendUnavailable)
		d.backend.set(nil)
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code)
		d.requireIdle(t, config.EventsEndpointConfig{MaxInFlight: 1, MaxBytesInFlight: 32 << 20})
	})
}

// A request bears its token in one Authorization: Bearer header. Every request without a live
// token spends one of its address's failures, and an address with none left is refused before
// any lookup.
func TestAuthentication(t *testing.T) {
	for _, c := range []struct {
		name    string
		headers []string
		code    string
	}{
		{name: "a missing header", code: codeUnauthorized},
		{name: "two headers", headers: []string{"Bearer " + liveToken, "Bearer " + liveToken}, code: codeUnauthorized},
		{name: "another scheme", headers: []string{"Basic " + liveToken}, code: codeUnauthorized},
		{name: "no token", headers: []string{"Bearer "}, code: codeUnauthorized},
		{name: "a token with a space", headers: []string{"Bearer " + liveToken + " x"}, code: codeUnauthorized},
		{name: "a malformed token", headers: []string{"Bearer hx_ev_0123"}, code: codeInvalidToken},
		{name: "another kind of token", headers: []string{"Bearer hlxs_" + strings.Repeat("a", 64)}, code: codeInvalidToken},
		{name: "an uppercase token", headers: []string{"Bearer " + strings.ToUpper(liveToken)}, code: codeInvalidToken},
	} {
		t.Run("refuses "+c.name+" with no lookup", func(t *testing.T) {
			d := newDoor(t, nil)
			request := func() *http.Request {
				r := post(validBody)
				r.Header.Del("Authorization")
				for _, h := range c.headers {
					r.Header.Add("Authorization", h)
				}
				return r
			}
			for range sendtoken.FailuresPerMinute {
				rec := d.do(request())
				requireRefused(t, rec, http.StatusUnauthorized, c.code)
				assert.True(t, strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer"))
			}
			rec := d.do(request())
			requireRefused(t, rec, http.StatusTooManyRequests, codeRateLimited)
			assert.NotEmpty(t, rec.Header().Get("Retry-After"))
			assert.Zero(t, d.backend.lookups.Load())
			assert.Zero(t, d.backend.sends.Load())
		})
	}

	t.Run("takes the scheme in any case", func(t *testing.T) {
		d := newDoor(t, nil)
		r := post(validBody)
		r.Header.Set("Authorization", "bearer "+liveToken)
		require.Equal(t, http.StatusOK, d.do(r).Code)
	})

	t.Run("refuses an unknown token after its lookup", func(t *testing.T) {
		d := newDoor(t, nil)
		r := post(validBody)
		r.Header.Set("Authorization", "Bearer "+token(2))
		rec := d.do(r)
		requireRefused(t, rec, http.StatusUnauthorized, codeInvalidToken)
		assert.Equal(t, `Bearer error="invalid_token"`, rec.Header().Get("WWW-Authenticate"))
		assert.Equal(t, int32(1), d.backend.lookups.Load())
		assert.Zero(t, d.backend.sends.Load())
	})

	t.Run("answers 503 when the lookup can't answer", func(t *testing.T) {
		d := newDoor(t, nil)
		d.backend.lookupErr = status.Error(codes.Unavailable, "down")
		rec := d.do(post(validBody))
		requireRefused(t, rec, http.StatusServiceUnavailable, codeBackendUnavailable)
		assert.Equal(t, "1", rec.Header().Get("Retry-After"))
		assert.Zero(t, d.backend.sends.Load())
	})

	// The backend checks the token on every send, so the door may trust what it found for a while.
	t.Run("looks a token up again after validated_for", func(t *testing.T) {
		d := newDoor(t, nil)
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code)
		d.clock.advance(29 * time.Second)
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code)
		assert.Equal(t, int32(1), d.backend.lookups.Load(), "within 30 s")
		d.clock.advance(time.Second)
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code)
		assert.Equal(t, int32(2), d.backend.lookups.Load(), "at 30 s")
	})

	// A token revoked within the cache is refused by the backend; the door forgets it, and its
	// concurrent refusals spend one failure between them.
	t.Run("forgets a token the backend refuses, spending one failure", func(t *testing.T) {
		d := newDoor(t, nil)
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code)
		entered, release := d.backend.hold(status.Error(codes.PermissionDenied, "this send token is revoked, or sends to another API source"))
		done := make(chan []*httptest.ResponseRecorder)
		go func() { done <- d.parallel(5, func(int) *http.Request { return post(validBody) }) }()
		waitFor(t, entered, 5)
		close(release)
		for _, rec := range <-done {
			requireRefused(t, rec, http.StatusUnauthorized, codeInvalidToken)
			assert.Equal(t, `Bearer error="invalid_token"`, rec.Header().Get("WWW-Authenticate"))
		}
		assert.Equal(t, int32(1), d.backend.lookups.Load(), "the five were admitted from the cache")

		// The next request looks the token up again, and finds it revoked: a second failure.
		d.backend.revoke(liveToken)
		requireRefused(t, d.do(post(validBody)), http.StatusUnauthorized, codeInvalidToken)
		assert.Equal(t, int32(2), d.backend.lookups.Load())

		// So 8 of the address's 10 failures are left.
		unsigned := func() *http.Request {
			r := post(validBody)
			r.Header.Del("Authorization")
			return r
		}
		for range 8 {
			requireRefused(t, d.do(unsigned()), http.StatusUnauthorized, codeUnauthorized)
		}
		requireRefused(t, d.do(unsigned()), http.StatusTooManyRequests, codeRateLimited)
	})
}

// One address, whatever tokens it holds, has at most address_max_in_flight of a pod's places,
// and that many bodies' bytes, however slowly its bodies arrive: past them its requests are
// refused at once, spending none of the pod's room or rate, and other addresses still send.
func TestAddressPlaces(t *testing.T) {
	t.Run("refuses an address's 9th send in flight, while another sends", func(t *testing.T) {
		// The pod's bucket holds the 8 sends and another address's.
		d := newDoor(t, func(cfg *config.EventsConfig) {
			cfg.Endpoint.RequestsPerSecond, cfg.Endpoint.BurstRequests = 1, 9
		})
		entered, release := d.backend.hold(nil)
		done := make(chan []*httptest.ResponseRecorder)
		go func() { done <- d.parallel(8, func(int) *http.Request { return post(validBody) }) }()
		waitFor(t, entered, 8)
		for range 20 {
			rec := d.do(post(validBody))
			refusal := requireRefused(t, rec, http.StatusTooManyRequests, codeRateLimited)
			assert.Equal(t, "1", rec.Header().Get("Retry-After"))
			assert.Equal(t, "this address has too many sends in flight; send again after Retry-After", refusal.Message)
		}
		other := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			r := post(validBody)
			r.Header.Set("X-Forwarded-For", elsewhere)
			other <- d.do(r)
		}()
		waitFor(t, entered, 1)
		close(release)
		for _, rec := range append(<-done, <-other) {
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}
		d.requireIdle(t, config.Defaults().Foundry.Events.Endpoint)
		d.clock.advance(time.Second)
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code, "its places came back")
	})

	// 16 bodies of 2 MiB would hold the pod's 32 MiB for readFor; one address holds 8 of them.
	t.Run("leaves the pod's room to others while one address's slow bodies hold its places", func(t *testing.T) {
		d := newDoor(t, nil)
		server := httptest.NewServer(d)
		t.Cleanup(server.Close)
		header := "Authorization: Bearer " + liveToken + "\r\nX-Forwarded-For: " + address + "\r\n"
		conns := make([]net.Conn, 16)
		for i := range conns {
			conns[i] = stalled(t, server, header, 2<<20)
		}
		require.Eventually(t, func() bool { return d.addressPlaces() == 8 }, 5*time.Second, time.Millisecond)

		// The 8 past its places were answered at once; the 8 in them still wait on their bodies.
		answers := make([]error, len(conns))
		var refused atomic.Int32
		var wg sync.WaitGroup
		for i, conn := range conns {
			wg.Go(func() {
				resp, refusal, err := answer(conn, 500*time.Millisecond)
				if answers[i] = err; err == nil && resp.StatusCode == http.StatusTooManyRequests && refusal.Code == codeRateLimited {
					refused.Add(1)
				}
			})
		}
		wg.Wait()
		assert.EqualValues(t, 8, refused.Load(), "%v", answers)

		req, err := http.NewRequest(http.MethodPost, server.URL+sendPath, strings.NewReader(padded(2<<20)))
		require.NoError(t, err)
		req.Header = post(validBody).Header
		req.Header.Set("X-Forwarded-For", elsewhere)
		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "another address's 2 MiB send: %s", body)
		assert.Equal(t, int32(1), d.backend.sends.Load())
	})
}

// A refusal that needs no lookup takes none of the pod's room or rate, so a flood of them from
// addresses that cost the sender nothing leaves a valid sender its pod.
func TestFreeRefusals(t *testing.T) {
	for _, c := range []struct {
		name          string
		authorization string
		// spent has the flooding address spend its failures first.
		spent  bool
		status int
		code   string
	}{
		{name: "no Authorization", status: http.StatusUnauthorized, code: codeUnauthorized},
		{name: "another scheme", authorization: "Basic " + liveToken, status: http.StatusUnauthorized, code: codeUnauthorized},
		{name: "a malformed token", authorization: "Bearer hx_ev_0123", status: http.StatusUnauthorized, code: codeInvalidToken},
		{name: "a malformed token from an address with no failure left", authorization: "Bearer hx_ev_0123", spent: true,
			status: http.StatusTooManyRequests, code: codeRateLimited},
		{name: "an unknown token from an address with no failure left", authorization: "Bearer " + token(2), spent: true,
			status: http.StatusTooManyRequests, code: codeRateLimited},
		{name: "a live token not found live lately, from an address with no failure left", authorization: "Bearer " + liveToken, spent: true,
			status: http.StatusTooManyRequests, code: codeRateLimited},
	} {
		t.Run("takes no room for "+c.name, func(t *testing.T) {
			// The pod's bucket holds one request, the valid send's.
			d := newDoor(t, func(cfg *config.EventsConfig) {
				cfg.Endpoint.RequestsPerSecond, cfg.Endpoint.BurstRequests = 1, 1
			})
			if c.spent {
				for range sendtoken.FailuresPerMinute {
					d.h.admission.Unauthenticated(elsewhere, t0)
				}
			}
			for i := range 3 * sendtoken.FailuresPerMinute {
				r := post(validBody)
				r.Header.Set("X-Forwarded-For", elsewhere)
				r.Header.Del("Authorization")
				if c.authorization != "" {
					r.Header.Set("Authorization", c.authorization)
				}
				status, code := c.status, c.code
				if i >= sendtoken.FailuresPerMinute {
					status, code = http.StatusTooManyRequests, codeRateLimited
				}
				requireRefused(t, d.do(r), status, code)
			}
			assert.Zero(t, d.backend.lookups.Load())
			d.requireIdle(t, config.Defaults().Foundry.Events.Endpoint)
			require.Equal(t, http.StatusOK, d.do(post(validBody)).Code, "the pod's one request is left")
			requireRefused(t, d.do(post(validBody)), http.StatusTooManyRequests, codeRateLimited)
		})
	}
}

// The address the buckets key is the load balancer's last hop, or the peer's.
func TestClientAddress(t *testing.T) {
	for _, c := range []struct{ forwarded, remote, want string }{
		{"203.0.113.9", "10.0.0.1:443", "203.0.113.9"},
		{"192.0.2.1, 198.51.100.7 ,  203.0.113.9 ", "10.0.0.1:443", "203.0.113.9"},
		{"", "10.0.0.1:443", "10.0.0.1"},
		{"", "[2001:db8::1]:443", "2001:db8::1"},
		{"", "10.0.0.1", "10.0.0.1"},
	} {
		r := httptest.NewRequest(http.MethodPost, sendPath, nil)
		r.RemoteAddr = c.remote
		if c.forwarded != "" {
			r.Header.Set("X-Forwarded-For", c.forwarded)
		}
		assert.Equal(t, c.want, clientAddress(r), "%q from %q", c.forwarded, c.remote)
	}
}

// A token sends to its own source only, on the one route.
func TestRouting(t *testing.T) {
	t.Run("refuses a path naming another source as not found, with no send", func(t *testing.T) {
		d := newDoor(t, nil)
		r := post(validBody)
		r.URL.Path = "/v1/sources/source-2/pieces"
		refusal := requireRefused(t, d.do(r), http.StatusNotFound, codeNotFound)
		assert.NotContains(t, refusal.Message, "source-1")
		assert.Zero(t, d.backend.sends.Load())
	})

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodOptions, http.MethodDelete} {
		t.Run("answers "+method+" 405 with no lookup", func(t *testing.T) {
			d := newDoor(t, nil)
			r := post(validBody)
			r.Method = method
			rec := d.do(r)
			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
			assert.Contains(t, rec.Header().Get("Allow"), http.MethodPost)
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
			assert.Zero(t, d.backend.lookups.Load())
		})
	}

	for _, path := range []string{"/", "/v1/sources/source-1", "/v1/sources/source-1/pieces/1", "/v1/sources/source-1/piece", "/metrics", "/v2/sources/source-1/pieces"} {
		t.Run("answers "+path+" 404", func(t *testing.T) {
			d := newDoor(t, nil)
			r := post(validBody)
			r.URL.Path = path
			assert.Equal(t, http.StatusNotFound, d.do(r).Code)
			assert.Zero(t, d.backend.lookups.Load())
		})
	}
}

// The body is one send, checked by the backend's own stateless rules before any send.
func TestBody(t *testing.T) {
	piece := func(id, data string) string { return `{"id":"` + id + `","data":` + data + `}` }
	send := func(pieces ...string) string {
		return `{"client_token":"` + clientToken + `","pieces":[` + strings.Join(pieces, ",") + `]}`
	}
	many := make([]string, 501)
	for i := range many {
		many[i] = piece(fmt.Sprintf("o-%d", i), `{}`)
	}
	// 5 pieces of 230,000 bytes of data each: 1.15 MB compacted, past max_send_bytes.
	big := make([]string, 5)
	for i := range big {
		big[i] = piece(fmt.Sprintf("o-%d", i), `{"text":"`+strings.Repeat("a", 230_000)+`"}`)
	}
	for _, c := range []struct {
		name, body string
		piece      int
	}{
		{name: "an empty body", body: ""},
		{name: "a body that ends early", body: validBody[:20]},
		{name: "a body that isn't JSON", body: "pieces"},
		{name: "a JSON array", body: `[]`},
		{name: "an unknown field", body: `{"client_token":"` + clientToken + `","pieces":[],"source":"x"}`},
		{name: "an unknown field of a piece", body: send(`{"id":"o-1","data":{},"kind":"x"}`)},
		{name: "a second JSON value", body: validBody + `{}`},
		{name: "data after the JSON", body: validBody + `x`},
		{name: "a missing client_token", body: `{"pieces":[` + piece("o-1", `{}`) + `]}`},
		{name: "an empty client_token", body: `{"client_token":"","pieces":[` + piece("o-1", `{}`) + `]}`},
		{name: "a short client_token", body: `{"client_token":"0123456789abcde","pieces":[` + piece("o-1", `{}`) + `]}`},
		{name: "a long client_token", body: `{"client_token":"` + strings.Repeat("a", 65) + `","pieces":[` + piece("o-1", `{}`) + `]}`},
		{name: "a client_token with a space", body: `{"client_token":"0123456789 abcdef","pieces":[` + piece("o-1", `{}`) + `]}`},
		{name: "a client_token that isn't a string", body: `{"client_token":12,"pieces":[` + piece("o-1", `{}`) + `]}`},
		{name: "no pieces", body: send()},
		{name: "pieces null", body: `{"client_token":"` + clientToken + `","pieces":null}`},
		{name: "501 pieces", body: send(many...)},
		{name: "data past max_send_bytes compacted", body: send(big...)},
		{name: "a bad occurred_at", body: send(piece("o-1", `{}`), `{"id":"o-2","occurred_at":"yesterday","data":{}}`), piece: 2},
		{name: "an occurred_at out of range", body: send(`{"id":"o-1","occurred_at":"0999-12-31T23:59:59Z","data":{}}`), piece: 1},
		{name: "invalid UTF-8 in data", body: send(piece("o-1", `{}`), piece("o-2", "{\"name\":\"\xff\"}")), piece: 2},
		{name: "invalid UTF-8 in an id", body: send(piece("o-1", `{}`), piece("o-\xff", `{}`)), piece: 2},
		{name: "a spaced id", body: send(piece("o-1", `{}`), piece("o-2", `{}`), piece("o 3", `{}`)), piece: 3},
		{name: "a repeated id", body: send(piece("o-1", `{}`), piece("o-1", `{}`)), piece: 2},
		{name: "data that isn't an object", body: send(piece("o-1", `[1]`)), piece: 1},
		{name: "missing data", body: send(`{"id":"o-1"}`), piece: 1},
		{name: "null data", body: send(piece("o-1", `null`)), piece: 1},
		{name: "a reserved field", body: send(piece("o-1", `{"id":"x"}`)), piece: 1},
	} {
		t.Run("refuses "+c.name+" with no send", func(t *testing.T) {
			d := newDoor(t, nil)
			refusal := requireRefused(t, d.do(post(c.body)), http.StatusBadRequest, codeInvalidArguments)
			assert.Equal(t, c.piece, refusal.Piece, refusal.Message)
			assert.NotEmpty(t, refusal.Message)
			assert.Zero(t, d.backend.sends.Load())
		})
	}

	t.Run("takes space after the JSON", func(t *testing.T) {
		d := newDoor(t, nil)
		require.Equal(t, http.StatusOK, d.do(post(validBody+" \n\t")).Code)
	})

	// An empty occurred_at is none, as the MCP takes it; any other is parsed, in UTC.
	t.Run("forwards occurred_at in UTC, and an empty one as none", func(t *testing.T) {
		d := newDoor(t, nil)
		body := send(`{"id":"o-1","occurred_at":"2026-10-24T09:30:00.1234567+02:00","data":{}}`, `{"id":"o-2","occurred_at":"","data":{}}`,
			`{"id":"o-3","occurred_at":null,"data":{}}`)
		require.Equal(t, http.StatusOK, d.do(post(body)).Code)
		pieces := d.backend.last().GetPieces()
		assert.Equal(t, time.Date(2026, 10, 24, 7, 30, 0, 123456000, time.UTC), pieces[0].GetOccurredAt().AsTime())
		assert.Nil(t, pieces[1].GetOccurredAt())
		assert.Nil(t, pieces[2].GetOccurredAt())
	})
}

// The door forwards what it checked: each piece's data compacted, byte for byte as sent, as the
// token's own source, with the token's actor.
func TestForwarding(t *testing.T) {
	d := newDoor(t, nil)
	literals := `{"big":18446744073709551617,"max":9223372036854775807,"price":1.10,"smile":"\ud83d\ude00","nested":{"a":[1, 2.50e3]}}`
	pieces := []string{`{"id":"o-0","data":` + literals + `}`}
	compacted := []string{`{"big":18446744073709551617,"max":9223372036854775807,"price":1.10,"smile":"\ud83d\ude00","nested":{"a":[1,2.50e3]}}`}
	// 400 pieces of about 2.4 KB of data, each with more space than that pretty-printed.
	text := strings.Repeat("a", 2_400)
	for i := 1; i < 400; i++ {
		pieces = append(pieces, fmt.Sprintf("{\n  \"id\": \"o-%d\",\n  \"data\": {\n%s\"text\": \"%s\"\n  }\n}", i, strings.Repeat(" ", 2_700), text))
		compacted = append(compacted, `{"text":"`+text+`"}`)
	}
	body := `{"client_token": "` + clientToken + `", "pieces": [` + strings.Join(pieces, ",\n") + `]}`
	require.Greater(t, len(body), 19<<20/10, "a 1.9 MiB body")
	require.LessOrEqual(t, len(body), 2<<20)

	rec := d.do(post(body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	req := d.backend.last()
	assert.LessOrEqual(t, proto.Size(req), 11<<20/10, "the request is near the compacted size, not the wire's")
	require.Len(t, req.GetPieces(), 400)
	for i, p := range req.GetPieces() {
		require.Equal(t, compacted[i], p.GetDataJson(), "piece %d", i+1)
		require.Equal(t, fmt.Sprintf("o-%d", i), p.GetId())
	}
	assert.Equal(t, "workspace-1", req.GetWorkspaceId(), "the token's workspace")
	assert.Equal(t, "source-1", req.GetConnectorId())
	assert.Equal(t, clientToken, req.GetClientToken())
	d.backend.mu.Lock()
	actor, lookup := d.backend.actors[len(d.backend.actors)-1], d.backend.lookupActor
	d.backend.mu.Unlock()
	assert.Equal(t, ports.Actor{Source: ports.SourceEventSource, EventSourceTokenID: "token-1", WorkspaceID: "workspace-1"}, actor,
		"the token's sender, of no user")
	assert.Equal(t, ports.Actor{Source: ports.SourceEventSource}, lookup, "the token is looked up for no one yet")
}

// The backend's answers are mapped to statuses a sender can act on.
func TestBackendAnswers(t *testing.T) {
	withDetails := func(st *status.Status, details ...protoadapt.MessageV1) error {
		withDetails, err := st.WithDetails(details...)
		require.NoError(t, err)
		return withDetails.Err()
	}
	precondition := func(reason string) error {
		return withDetails(status.New(codes.FailedPrecondition, "refused: "+reason), &errdetails.ErrorInfo{Reason: reason, Domain: "foundry.helix-db.com"})
	}
	for _, c := range []struct {
		name       string
		err        error
		status     int
		code       string
		piece      int
		reason     string
		retryAfter string
		message    string
	}{
		{name: "a type lock at piece 2", err: withDetails(status.New(codes.InvalidArgument, `piece 2 (id "o-2"): field total is a number`),
			&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: "pieces[1]"}}}),
			status: http.StatusBadRequest, code: codeInvalidArguments, piece: 2, message: `piece 2 (id "o-2"): field total is a number`},
		{name: "a refusal with details of another kind", err: withDetails(status.New(codes.InvalidArgument, "bad"), &errdetails.ErrorInfo{Reason: "X"}),
			status: http.StatusBadRequest, code: codeInvalidArguments, message: "bad"},
		{name: "a reused client_token", err: status.Error(codes.InvalidArgument, "client_token was already used"),
			status: http.StatusBadRequest, code: codeInvalidArguments, message: "client_token was already used"},
		{name: "a field violation of no piece", err: withDetails(status.New(codes.InvalidArgument, "bad"),
			&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: "client_token"}, {Field: "pieces[x]"}, {Field: "pieces[-1]"}}}),
			status: http.StatusBadRequest, code: codeInvalidArguments, message: "bad"},
		{name: "a full source", err: precondition("FOUNDRY_EVENT_SOURCE_FULL"), status: http.StatusConflict, code: codeFailedPrecondition,
			reason: "FOUNDRY_EVENT_SOURCE_FULL", message: "refused: FOUNDRY_EVENT_SOURCE_FULL"},
		{name: "a paused source", err: precondition("FOUNDRY_EVENT_SOURCE_PAUSED"), status: http.StatusConflict, code: codeFailedPrecondition,
			reason: "FOUNDRY_EVENT_SOURCE_PAUSED", message: "refused: FOUNDRY_EVENT_SOURCE_PAUSED"},
		{name: "the field cap", err: precondition("FOUNDRY_EVENT_SOURCE_FIELDS"), status: http.StatusConflict, code: codeFailedPrecondition,
			reason: "FOUNDRY_EVENT_SOURCE_FIELDS", message: "refused: FOUNDRY_EVENT_SOURCE_FIELDS"},
		{name: "a source's rate", err: withDetails(status.New(codes.ResourceExhausted, "send again in 1.2s"), &errdetails.RetryInfo{RetryDelay: durationpb.New(1200 * time.Millisecond)}),
			status: http.StatusTooManyRequests, code: codeResourceExhausted, retryAfter: "2", message: "send again in 1.2s"},
		{name: "a limit with no RetryInfo", err: status.Error(codes.ResourceExhausted, "later"),
			status: http.StatusTooManyRequests, code: codeResourceExhausted, retryAfter: "1", message: "later"},
		{name: "a deleted source", err: status.Error(codes.NotFound, "connector not found"), status: http.StatusNotFound, code: codeNotFound},
		{name: "an unavailable backend", err: status.Error(codes.Unavailable, "read the send token: dial tcp 10.0.0.1:50063"),
			status: http.StatusServiceUnavailable, code: codeBackendUnavailable, retryAfter: "1"},
		{name: "a backend past its deadline", err: status.Error(codes.DeadlineExceeded, "slow"),
			status: http.StatusServiceUnavailable, code: codeBackendUnavailable, retryAfter: "1"},
		{name: "an internal error", err: status.Error(codes.Internal, "read batch 3: corrupt"), status: http.StatusInternalServerError, code: codeInternal},
		{name: "an unknown error", err: status.Error(codes.Unknown, "panic"), status: http.StatusInternalServerError, code: codeInternal},
	} {
		t.Run("answers "+c.name, func(t *testing.T) {
			d := newDoor(t, nil)
			d.backend.refuse(c.err)
			rec := d.do(post(validBody))
			refusal := requireRefused(t, rec, c.status, c.code)
			assert.Equal(t, c.piece, refusal.Piece)
			assert.Equal(t, c.reason, refusal.Reason)
			assert.Equal(t, c.retryAfter, rec.Header().Get("Retry-After"))
			if c.message != "" {
				assert.Equal(t, c.message, refusal.Message)
			}
			assert.NotContains(t, refusal.Message, "10.0.0.1", "a 5xx tells nothing of the backend")
			assert.NotContains(t, refusal.Message, "corrupt")
		})
	}

	t.Run("answers a replayed send as the MCP does", func(t *testing.T) {
		d := newDoor(t, nil)
		d.backend.set(func(context.Context, *foundryv1.SendFoundryEventsRequest) (*foundryv1.SendFoundryEventsResponse, error) {
			return &foundryv1.SendFoundryEventsResponse{
				Batch: &foundryv1.FoundryEventBatch{Seq: 3, Pieces: 1, CreatedAt: timestamppb.New(t0.Add(1500 * time.Microsecond))}, Replayed: true,
				Capacity: &foundryv1.EventSourceCapacity{Rows: 85, MaxRows: 100, Bytes: 1, MaxBytes: 1000}, Warning: "85% full",
			}, nil
		})
		rec := d.do(post(validBody))
		require.Equal(t, http.StatusOK, rec.Code)
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		assert.Equal(t, true, out["replayed"])
		assert.Equal(t, "3", out["seq"])
		assert.Equal(t, []any{}, out["new_fields"], "none is an empty list")
		assert.Equal(t, "2023-11-14T22:13:20.0015Z", out["received_at"])
		assert.Equal(t, "85% full", out["warning"])
		assert.Equal(t, float64(85), out["capacity"].(map[string]any)["percent_full"])
	})

	t.Run("gives up on a send past its deadline", func(t *testing.T) {
		d := newDoor(t, nil)
		d.h.sendFor = 50 * time.Millisecond
		d.backend.set(func(ctx context.Context, _ *foundryv1.SendFoundryEventsRequest) (*foundryv1.SendFoundryEventsResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		start := time.Now()
		rec := d.do(post(validBody))
		requireRefused(t, rec, http.StatusServiceUnavailable, codeBackendUnavailable)
		assert.Less(t, time.Since(start), 5*time.Second)
	})
}

// stalled sends server a request's head, with header, and the start of a body of length bytes,
// then nothing; the connection closes when the test ends.
func stalled(t *testing.T, server *httptest.Server, header string, length int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: ingest\r\n%sContent-Type: application/json\r\nContent-Length: %d\r\n\r\n{\"client_token\":",
		sendPath, header, length)
	require.NoError(t, err)
	return conn
}

// answer reads conn's answer, for at most wait.
func answer(conn net.Conn, wait time.Duration) (*http.Response, errorDetail, error) {
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return nil, errorDetail{}, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, errorDetail{}, err
	}
	defer resp.Body.Close()
	var out errorBody
	return resp, out.Error, json.NewDecoder(resp.Body).Decode(&out)
}

// stall sends a request's head and the start of a body of 1000 bytes to server, then nothing,
// and reads the answer, for at most 5 s.
func stall(t *testing.T, server *httptest.Server, header string) (*http.Response, errorDetail) {
	t.Helper()
	resp, refusal, err := answer(stalled(t, server, header, 1000), 5*time.Second)
	require.NoError(t, err)
	return resp, refusal
}

// Over a real connection, a request has readFor to arrive; its send may take longer.
func TestDeadlines(t *testing.T) {
	t.Run("answers a body slower than its deadline 408, with no send", func(t *testing.T) {
		d := newDoor(t, nil)
		d.h.readFor = 200 * time.Millisecond
		server := httptest.NewServer(d)
		t.Cleanup(server.Close)
		start := time.Now()
		resp, refusal := stall(t, server, "Authorization: Bearer "+liveToken+"\r\n")
		assert.Equal(t, http.StatusRequestTimeout, resp.StatusCode)
		assert.Equal(t, codeRequestTimeout, refusal.Code)
		assert.True(t, resp.Close, "what is left of the body is no request")
		assert.Less(t, time.Since(start), 3*time.Second)
		assert.Zero(t, d.backend.sends.Load())
	})

	// The server reads what is left of a short body before it answers, to keep the connection:
	// the deadline bounds that too.
	t.Run("answers a refusal of a stalled body by its deadline", func(t *testing.T) {
		d := newDoor(t, nil)
		d.h.readFor = 200 * time.Millisecond
		server := httptest.NewServer(d)
		t.Cleanup(server.Close)
		start := time.Now()
		resp, refusal := stall(t, server, "")
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, codeUnauthorized, refusal.Code)
		assert.Less(t, time.Since(start), 3*time.Second)
	})

	// The mux's own refusals get the same deadline: the server reads what is left of a short body
	// before it answers, and without one a body sent slowly with a 404 or 405 held its connection
	// for as long as it trickled.
	for code, line := range map[int]string{http.StatusMethodNotAllowed: "GET " + sendPath, http.StatusNotFound: "POST /v1/sources/source-1/other"} {
		t.Run(fmt.Sprintf("answers the mux's %d of a stalled body by its deadline, and closes", code), func(t *testing.T) {
			d := newDoor(t, nil)
			d.h.readFor = 200 * time.Millisecond
			server := httptest.NewServer(d)
			t.Cleanup(server.Close)
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			_, err = fmt.Fprintf(conn, "%s HTTP/1.1\r\nHost: ingest\r\nContent-Length: 1000\r\n\r\n{", line)
			require.NoError(t, err)
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, nil)
			require.NoError(t, err, "answered by the deadline, not when the body ends")
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			assert.Equal(t, code, resp.StatusCode)
			_, err = reader.ReadByte()
			var timeout net.Error
			assert.False(t, errors.As(err, &timeout) && timeout.Timeout(), "the connection is closed, not held for the rest of the body")
			assert.Error(t, err)
			assert.Zero(t, d.backend.sends.Load())
		})
	}

	// The body's deadline is the connection's, so it is cleared once the body is in: otherwise the
	// server would end the request's context while the backend stores the send.
	t.Run("answers a send slower than the body's deadline", func(t *testing.T) {
		d := newDoor(t, nil)
		d.h.readFor = 100 * time.Millisecond
		d.backend.set(func(ctx context.Context, req *foundryv1.SendFoundryEventsRequest) (*foundryv1.SendFoundryEventsResponse, error) {
			select {
			case <-time.After(400 * time.Millisecond):
				return stored(1), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		server := httptest.NewServer(d)
		t.Cleanup(server.Close)
		req, err := http.NewRequest(http.MethodPost, server.URL+sendPath, bytes.NewReader([]byte(validBody)))
		require.NoError(t, err)
		req.Header = post(validBody).Header
		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	})
}
