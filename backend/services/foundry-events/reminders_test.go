package foundryevents

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sendtoken"
)

// nextSend is when the fake backend says an address's next reminder goes.
var nextSend = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

func (b *fakeBackend) CreateFoundryReminder(ctx context.Context, req *foundryv1.CreateFoundryReminderRequest) (
	*foundryv1.CreateFoundryReminderResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	actor, _ := ports.ActorFrom(ctx)
	b.reminderCalls, b.reminderActors = append(b.reminderCalls, req), append(b.reminderActors, actor)
	if b.reminderErr != nil {
		return nil, b.reminderErr
	}
	return &foundryv1.CreateFoundryReminderResponse{NextSendAt: timestamppb.New(nextSend), Again: b.reminderAgain}, nil
}

func (b *fakeBackend) StopFoundryReminders(ctx context.Context, req *foundryv1.StopFoundryRemindersRequest) (
	*foundryv1.StopFoundryRemindersResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	actor, _ := ports.ActorFrom(ctx)
	b.reminderCalls, b.reminderActors = append(b.reminderCalls, req), append(b.reminderActors, actor)
	if b.reminderErr != nil {
		return nil, b.reminderErr
	}
	return &foundryv1.StopFoundryRemindersResponse{}, nil
}

func (b *fakeBackend) reminderCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.reminderCalls)
}

const (
	remindPath = "/v1/reminders"
	stopPath   = "/v1/reminders/stop"
	siteSecret = "site-reminders-secret-5f1c0e9a7b3d2846c1e0f9a8b7d6c5e4"
	remindBody = `{"email":"ada@example.com","time_zone":"Europe/London","consent":"remind-v1"}`
)

// remind is a request of the site's to path, bearing its secret, from address.
func remind(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+siteSecret)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-For", address)
	return r
}

// The site's requests reach the backend as the door's, with the SHA-256 of
// its bearer, and come back in snake_case.
func TestReminders(t *testing.T) {
	t.Run("schedules reminders", func(t *testing.T) {
		d := newDoor(t, nil)
		rec := d.do(remind(remindPath, remindBody))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		assert.JSONEq(t, `{"next_send_at":"2026-11-11T09:00:00Z","again":false}`, rec.Body.String())
		require.Equal(t, 1, d.backend.reminderCount())
		got := d.backend.reminderCalls[0].(*foundryv1.CreateFoundryReminderRequest)
		assert.True(t, proto.Equal(&foundryv1.CreateFoundryReminderRequest{
			SecretSha256: sendtoken.Hash(siteSecret), Email: "ada@example.com", TimeZone: "Europe/London", Consent: "remind-v1",
		}, got), "%v", got)
		assert.Equal(t, ports.Actor{Source: ports.SourceEventSource}, d.backend.reminderActors[0], "the door's, of no one")
		assert.Zero(t, d.backend.lookups.Load()+d.backend.sends.Load(), "no send token is looked up")
	})
	t.Run("answers a schedule running", func(t *testing.T) {
		d := newDoor(t, nil)
		d.backend.reminderAgain = true
		rec := d.do(remind(remindPath, remindBody))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"next_send_at":"2026-11-11T09:00:00Z","again":true}`, rec.Body.String())
	})
	t.Run("stops reminders", func(t *testing.T) {
		d := newDoor(t, nil)
		rec := d.do(remind(stopPath, `{"email":"ada@example.com"}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.JSONEq(t, `{}`, rec.Body.String())
		got := d.backend.reminderCalls[0].(*foundryv1.StopFoundryRemindersRequest)
		assert.Equal(t, sendtoken.Hash(siteSecret), got.GetSecretSha256())
		assert.Equal(t, "ada@example.com", got.GetEmail())
	})
	t.Run("only takes POST", func(t *testing.T) {
		d := newDoor(t, nil)
		for _, path := range []string{remindPath, stopPath} {
			r := remind(path, remindBody)
			r.Method = http.MethodGet
			assert.Equal(t, http.StatusMethodNotAllowed, d.do(r).Code, path)
		}
		assert.Equal(t, http.StatusNotFound, d.do(remind("/v1/reminders/start", remindBody)).Code)
		assert.Zero(t, d.backend.reminderCount())
	})
}

// What isn't the site's request is refused before the backend.
func TestReminderRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		request func() *http.Request
		status  int
		code    string
	}{
		"no bearer": {func() *http.Request {
			r := remind(remindPath, remindBody)
			r.Header.Del("Authorization")
			return r
		}, http.StatusUnauthorized, codeUnauthorized},
		"another scheme": {func() *http.Request {
			r := remind(remindPath, remindBody)
			r.Header.Set("Authorization", "Basic "+siteSecret)
			return r
		}, http.StatusUnauthorized, codeUnauthorized},
		"not JSON": {func() *http.Request {
			r := remind(remindPath, remindBody)
			r.Header.Set("Content-Type", "text/plain")
			return r
		}, http.StatusUnsupportedMediaType, codeUnsupportedMediaType},
		"a Content-Length past the cap": {func() *http.Request {
			r := remind(remindPath, remindBody)
			r.ContentLength = maxReminderBody + 1
			return r
		}, http.StatusRequestEntityTooLarge, codePayloadTooLarge},
		"a body past the cap": {func() *http.Request {
			r := remind(remindPath, `{"email":"`+strings.Repeat("a", maxReminderBody)+`@example.com"}`)
			r.ContentLength = -1
			return r
		}, http.StatusRequestEntityTooLarge, codePayloadTooLarge},
		"an empty body": {func() *http.Request { return remind(remindPath, "") }, http.StatusBadRequest, codeInvalidArguments},
		"an unknown field": {func() *http.Request {
			return remind(remindPath, `{"email":"a@b.co","consent":"remind-v1","name":"Ada"}`)
		}, http.StatusBadRequest, codeInvalidArguments},
		"two values":        {func() *http.Request { return remind(remindPath, remindBody+remindBody) }, http.StatusBadRequest, codeInvalidArguments},
		"no address":        {func() *http.Request { return remind(remindPath, `{"time_zone":"UTC","consent":"remind-v1"}`) }, http.StatusBadRequest, codeInvalidArguments},
		"no consent":        {func() *http.Request { return remind(remindPath, `{"email":"ada@example.com","time_zone":"UTC"}`) }, http.StatusBadRequest, codeInvalidArguments},
		"a stop of no one":  {func() *http.Request { return remind(stopPath, `{"email":" "}`) }, http.StatusBadRequest, codeInvalidArguments},
		"a stop's own body": {func() *http.Request { return remind(stopPath, remindBody) }, http.StatusBadRequest, codeInvalidArguments},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDoor(t, nil)
			refusal := requireRefused(t, d.do(c.request()), c.status, c.code)
			assert.NotContains(t, refusal.Message, "ada@", "a refusal quotes no address")
			assert.Zero(t, d.backend.reminderCount())
		})
	}
}

// The backend's refusals are mapped to what the site can act on.
func TestReminderBackendAnswers(t *testing.T) {
	limited, _ := status.New(codes.ResourceExhausted, "Foundry is taking too many reminders just now; ask again later").
		WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(15 * time.Minute)})
	for name, c := range map[string]struct {
		err        error
		status     int
		code       string
		retryAfter string
	}{
		"an address it can't take": {status.Error(codes.InvalidArgument, "email must be one address"), http.StatusBadRequest, codeInvalidArguments, ""},
		"another secret":           {status.Error(codes.Unauthenticated, "no"), http.StatusUnauthorized, codeUnauthorized, ""},
		"another caller":           {status.Error(codes.PermissionDenied, "no"), http.StatusUnauthorized, codeUnauthorized, ""},
		"reminders off":            {status.Error(codes.FailedPrecondition, "off"), http.StatusNotFound, codeNotFound, ""},
		"past the hour's cap":      {limited.Err(), http.StatusTooManyRequests, codeRateLimited, "900"},
		"past a cap with no wait":  {status.Error(codes.ResourceExhausted, "busy"), http.StatusTooManyRequests, codeRateLimited, "60"},
		"a backend away":           {status.Error(codes.Unavailable, "connection refused"), http.StatusServiceUnavailable, codeBackendUnavailable, "1"},
		"a backend failing":        {status.Error(codes.Internal, "boom"), http.StatusInternalServerError, codeInternal, ""},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDoor(t, nil)
			d.backend.reminderErr = c.err
			for _, path := range []string{remindPath, stopPath} {
				body := remindBody
				if path == stopPath {
					body = `{"email":"ada@example.com"}`
				}
				rec := d.do(remind(path, body))
				refusal := requireRefused(t, rec, c.status, c.code)
				assert.Equal(t, c.retryAfter, rec.Header().Get("Retry-After"), path)
				if c.code == codeInvalidArguments {
					assert.Equal(t, "email must be one address", refusal.Message, "the backend's words")
				}
				if c.code == codeUnauthorized {
					assert.NotEmpty(t, rec.Header().Get("WWW-Authenticate"))
				}
			}
		})
	}
}

// The reminders share an address's bucket with the sends: one address can't
// flood either.
func TestReminderAddressBucket(t *testing.T) {
	d := newDoor(t, func(c *config.EventsConfig) { c.Endpoint.AddressBurstRequests = 2 })
	require.Equal(t, http.StatusOK, d.do(remind(remindPath, remindBody)).Code)
	require.Equal(t, http.StatusOK, d.do(post(validBody)).Code)
	rec := d.do(remind(remindPath, remindBody))
	requireRefused(t, rec, http.StatusTooManyRequests, codeRateLimited)
	assert.Equal(t, 1, d.backend.reminderCount())
	d.clock.advance(time.Second)
	require.Equal(t, http.StatusOK, d.do(remind(stopPath, `{"email":"ada@example.com"}`)).Code)
}

// A request's log line names its route, status and code, never the address,
// zone or bearer, whatever it ended as; each answer is counted.
func TestReminderLog(t *testing.T) {
	var logs bytes.Buffer
	cfg := config.Defaults().Foundry.Events
	backend := &fakeBackend{live: map[string]sendtoken.Sender{}}
	clock := &clock{at: t0}
	h := newHandler(dial(t, backend), cfg, slog.New(slog.NewJSONHandler(&logs, nil)), clock.now)
	routes := h.routes()
	do := func(r *http.Request) int {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, r)
		return rec.Code
	}
	require.Equal(t, http.StatusOK, do(remind(remindPath, remindBody)))
	backend.reminderErr = status.Error(codes.Unavailable, "dial tcp: connection refused")
	require.Equal(t, http.StatusServiceUnavailable, do(remind(stopPath, `{"email":"ada@example.com"}`)))
	require.Equal(t, http.StatusBadRequest, do(remind(remindPath, `{"email":"ada@example.com","consent":"remind-v1","x":1}`)))

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Len(t, lines, 3)
	for _, line := range lines {
		assert.NotContains(t, line, "ada", "the address")
		assert.NotContains(t, line, "London", "the zone")
		assert.NotContains(t, line, siteSecret, "the bearer")
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry))
		assert.Equal(t, "foundry reminders request", entry["msg"])
		assert.Contains(t, []any{"create", "stop"}, entry["route"])
	}
	assert.Contains(t, lines[1], "connection refused", "a 5xx logs the backend's error")
}
