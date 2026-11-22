package foundryevents

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
)

const (
	eventsPath        = "/v1/slack/events"
	interactivityPath = "/v1/slack/interactivity"
	// eventBody is an event as Slack sends it; the door never reads into it.
	eventBody = `{"type":"event_callback","team_id":"T1","event_id":"Ev1","event":{"type":"app_mention","text":"<@U0BOT> hi"}}`
	clickBody = `payload=%7B%22type%22%3A%22block_actions%22%7D`
	// feedBody is a channel message (slack.ClassFeed) of the minute of t0, and replyBody a reply in
	// its thread (slack.ClassReply).
	feedBody = `{"type":"event_callback","team_id":"T1","event_id":"Ev2","event_time":1700000000,` +
		`"event":{"type":"message","channel":"C1","channel_type":"channel","user":"U1","text":"hi","ts":"1700000000.000100"}}`
	replyBody = `{"type":"event_callback","team_id":"T1","event_id":"Ev3","event_time":1700000000,` +
		`"event":{"type":"message","channel":"C1","channel_type":"channel","user":"U1","text":"hi","ts":"1700000001.000100",` +
		`"thread_ts":"1700000000.000100"}}`
)

// slackTimestamp is the door's clock, t0, as Slack writes it.
var slackTimestamp = strconv.FormatInt(t0.Unix(), 10)

func (b *fakeBackend) ReceiveSlackRequest(ctx context.Context, req *foundryv1.ReceiveSlackRequestRequest) (
	*foundryv1.ReceiveSlackRequestResponse, error) {
	b.mu.Lock()
	b.slackCalls = append(b.slackCalls, req)
	actor, _ := ports.ActorFrom(ctx)
	b.slackActors = append(b.slackActors, actor)
	answer := b.slackAnswer
	b.mu.Unlock()
	if answer != nil {
		return answer(ctx, req)
	}
	return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusOK}, nil
}

// slackCount is how many Slack requests reached the backend.
func (b *fakeBackend) slackCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.slackCalls)
}

// slackCall is the i-th Slack request the backend received.
func (b *fakeBackend) slackCall(i int) *foundryv1.ReceiveSlackRequestRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.slackCalls[i]
}

// answerSlack makes the backend answer every Slack request with answer.
func (b *fakeBackend) answerSlack(answer func(context.Context, *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.slackAnswer = answer
}

// holdSlack makes each Slack request say so on entered, then wait for release to close and
// answer 200.
func (b *fakeBackend) holdSlack() (entered chan struct{}, release chan struct{}) {
	entered, release = make(chan struct{}, 128), make(chan struct{})
	b.answerSlack(func(context.Context, *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
		entered <- struct{}{}
		<-release
		return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusOK}, nil
	})
	return entered, release
}

// signed is a request Slack sends to path with body, from address, signed at the door's clock.
func signed(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if path == interactivityPath {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("X-Slack-Request-Timestamp", slackTimestamp)
	r.Header.Set("X-Slack-Signature", slack.Sign("signing-secret", slackTimestamp, []byte(body)))
	r.Header.Set("X-Forwarded-For", "192.0.2.50, "+address)
	return r
}

// reading starts a request to path from address whose body, of unknown length, arrives as the
// test writes it to reading; its answer comes on done once the test closes reading. It returns
// once the door is reading the body, holding the read room.
func (d *door) reading(t *testing.T, path string) (reading *io.PipeWriter, done chan *httptest.ResponseRecorder) {
	t.Helper()
	body, reading := io.Pipe()
	t.Cleanup(func() { _ = reading.Close() })
	r := signed(path, "")
	r.Body, r.ContentLength = body, -1
	done = make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- d.do(r) }()
	// The read room takes the address's place, then the body's bytes.
	room := d.cfg.Endpoint.Slack.MaxBytesInFlight
	require.Eventually(t, func() bool {
		if d.h.slack.read.bytes.TryAcquire(room) {
			d.h.slack.read.bytes.Release(room)
			return false
		}
		return true
	}, 5*time.Second, time.Millisecond)
	return reading, done
}

// requireSlackIdle checks Slack's gates hold nothing: the read room, and each class's room.
func (d *door) requireSlackIdle(t *testing.T) {
	t.Helper()
	cfg := d.cfg.Endpoint
	for name, c := range map[string]struct {
		g   *gate
		cfg config.DoorGateConfig
	}{
		"read": {d.h.slack.read, cfg.Slack}, "bot": {d.h.slack.classes[slack.ClassBot], cfg.Slack},
		"reply": {d.h.slack.classes[slack.ClassReply], cfg.SlackReply}, "feed": {d.h.slack.classes[slack.ClassFeed], cfg.SlackFeed},
	} {
		require.True(t, c.g.inFlight.TryAcquire(int64(c.cfg.MaxInFlight)), "a place in flight is held: %s", name)
		c.g.inFlight.Release(int64(c.cfg.MaxInFlight))
		require.True(t, c.g.bytes.TryAcquire(c.cfg.MaxBytesInFlight), "bytes are held: %s", name)
		c.g.bytes.Release(c.cfg.MaxBytesInFlight)
		c.g.mu.Lock()
		held := len(c.g.held)
		c.g.mu.Unlock()
		require.Zero(t, held, "an address's place is held: %s", name)
	}
}

// A request Slack signed is forwarded as it came, and answered as the backend answers.
func TestSlackForwarding(t *testing.T) {
	for _, c := range []struct {
		path, body string
		route      foundryv1.SlackRoute
	}{
		{eventsPath, eventBody, foundryv1.SlackRoute_SLACK_ROUTE_EVENTS},
		{interactivityPath, clickBody, foundryv1.SlackRoute_SLACK_ROUTE_INTERACTIVITY},
	} {
		t.Run(c.path, func(t *testing.T) {
			d := newDoor(t, nil)
			r := signed(c.path, c.body)
			r.Header.Set("X-Request-ID", "req-1")
			rec := d.do(r)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Empty(t, rec.Body.String())
			assert.Empty(t, rec.Header().Get("Content-Type"), "an answer without a body has no type")
			assert.Equal(t, "req-1", rec.Header().Get("X-Request-ID"))
			require.Equal(t, 1, d.backend.slackCount())
			got := d.backend.slackCall(0)
			assert.Equal(t, c.route, got.GetRoute())
			assert.Equal(t, []byte(c.body), got.GetBody(), "the raw body, byte for byte: the signature is over it")
			assert.Equal(t, r.Header.Get("X-Slack-Signature"), got.GetSignature())
			assert.Equal(t, slackTimestamp, got.GetTimestamp())
			assert.Zero(t, got.GetRetryNum())
			assert.Empty(t, got.GetRetryReason())
			d.backend.mu.Lock()
			assert.Equal(t, ports.Actor{Source: ports.SourceEventSource}, d.backend.slackActors[0], "the bare door, with no token")
			d.backend.mu.Unlock()
			d.requireSlackIdle(t)
		})
	}

	t.Run("answers url_verification's challenge as the backend writes it", func(t *testing.T) {
		d := newDoor(t, nil)
		d.backend.answerSlack(func(context.Context, *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
			return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusOK, ContentType: "application/json", Body: []byte(`{"challenge":"c-1"}`)}, nil
		})
		rec := d.do(signed(eventsPath, `{"type":"url_verification","challenge":"c-1","token":"x"}`))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		assert.Equal(t, `{"challenge":"c-1"}`, rec.Body.String())
	})

	for _, c := range []struct {
		status  int32
		outcome string
	}{
		{http.StatusUnauthorized, "unverified"}, {http.StatusNotFound, "disabled"}, {http.StatusBadRequest, "refused"},
		{http.StatusServiceUnavailable, codeInternal}, {http.StatusNoContent, codeOK},
	} {
		t.Run(fmt.Sprintf("answers the backend's %d as it is, counted %s", c.status, c.outcome), func(t *testing.T) {
			d := newDoor(t, nil)
			d.backend.answerSlack(func(context.Context, *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
				return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: c.status}, nil
			})
			counted := slackRequestsTotal.WithLabelValues("events", "bot", c.outcome)
			before := testutil.ToFloat64(counted)
			rec := d.do(signed(eventsPath, eventBody))
			assert.Equal(t, int(c.status), rec.Code)
			assert.Empty(t, rec.Body.String())
			assert.Equal(t, before+1, testutil.ToFloat64(counted))
		})
	}

	for _, c := range []struct {
		num, reason         string
		wantNum             int32
		wantReason, comment string
	}{
		{"2", "http_timeout", 2, "http_timeout", "a retry's number and reason"},
		{"x", "two words", 0, "", "no number, a reason of another form"},
		{"-1", "", 0, "", "a negative number"},
		{"4294967296", strings.Repeat("a", 65), 0, "", "a number past int32, a reason too long"},
		{"1", "caf\xc3\xa9", 1, "", "a reason that isn't ASCII"},
	} {
		t.Run("passes on "+c.comment, func(t *testing.T) {
			d := newDoor(t, nil)
			r := signed(eventsPath, eventBody)
			r.Header.Set("X-Slack-Retry-Num", c.num)
			r.Header.Set("X-Slack-Retry-Reason", c.reason)
			require.Equal(t, http.StatusOK, d.do(r).Code)
			assert.Equal(t, c.wantNum, d.backend.slackCall(0).GetRetryNum())
			assert.Equal(t, c.wantReason, d.backend.slackCall(0).GetRetryReason())
		})
	}

	t.Run("gives the backend until the request's answer is due", func(t *testing.T) {
		d := newDoor(t, nil)
		var left time.Duration
		d.backend.answerSlack(func(ctx context.Context, _ *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			left = time.Until(deadline)
			return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusOK}, nil
		})
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, eventBody)).Code)
		assert.LessOrEqual(t, left, slackFor)
		assert.Greater(t, left, slackFor-time.Second)
		assert.Less(t, slackReadFor, slackFor, "the body arrives within the answer's time")
		assert.Less(t, slackFor, 3*time.Second, "answered within Slack's three seconds")
	})
}

// Slack's routes are served only while foundry.slack is on; another method is 405.
func TestSlackRouting(t *testing.T) {
	t.Run("are not found while foundry.slack is off", func(t *testing.T) {
		backend := &fakeBackend{}
		h := newHandler(dial(t, backend), config.Defaults().Foundry.Events, config.SlackConfig{}, discard(), (&clock{at: t0}).now)
		assert.Nil(t, h.slack)
		for _, path := range []string{eventsPath, interactivityPath} {
			rec := httptest.NewRecorder()
			h.routes().ServeHTTP(rec, signed(path, eventBody))
			assert.Equal(t, http.StatusNotFound, rec.Code)
		}
		assert.Zero(t, backend.slackCount())
	})

	t.Run("answers GET 405", func(t *testing.T) {
		d := newDoor(t, nil)
		r := signed(eventsPath, eventBody)
		r.Method = http.MethodGet
		rec := d.do(r)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		assert.Contains(t, rec.Header().Get("Allow"), http.MethodPost)
		assert.Zero(t, d.backend.slackCount())
	})

	t.Run("answers another path under /v1/slack 404", func(t *testing.T) {
		d := newDoor(t, nil)
		assert.Equal(t, http.StatusNotFound, d.do(signed("/v1/slack/commands", eventBody)).Code)
		assert.Zero(t, d.backend.slackCount())
	})
}

// Headers no request Slack signed carries are refused with no RPC and no read of the body.
func TestSlackPrecheck(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, c := range []struct {
		name   string
		change func(http.Header)
		code   string
	}{
		{"no signature", func(h http.Header) { h.Del("X-Slack-Signature") }, codeUnsigned},
		{"no timestamp", func(h http.Header) { h.Del("X-Slack-Request-Timestamp") }, codeUnsigned},
		{"two signatures", func(h http.Header) { h.Add("X-Slack-Signature", "v0="+digest) }, codeUnsigned},
		{"two timestamps", func(h http.Header) { h.Add("X-Slack-Request-Timestamp", slackTimestamp) }, codeUnsigned},
		{"another version", func(h http.Header) { h.Set("X-Slack-Signature", "v1="+digest) }, codeUnsigned},
		{"a short digest", func(h http.Header) { h.Set("X-Slack-Signature", "v0="+digest[1:]) }, codeUnsigned},
		{"uppercase hex", func(h http.Header) { h.Set("X-Slack-Signature", "v0="+strings.ToUpper(digest)) }, codeUnsigned},
		{"no hex", func(h http.Header) { h.Set("X-Slack-Signature", "v0="+strings.Repeat("g", 64)) }, codeUnsigned},
		{"a timestamp of no number", func(h http.Header) { h.Set("X-Slack-Request-Timestamp", "1.5e9") }, codeUnsigned},
		{"a negative timestamp", func(h http.Header) { h.Set("X-Slack-Request-Timestamp", "-1") }, codeUnsigned},
		{"an empty timestamp", func(h http.Header) { h.Set("X-Slack-Request-Timestamp", "") }, codeUnsigned},
		{"a timestamp past the window", func(h http.Header) {
			h.Set("X-Slack-Request-Timestamp", strconv.FormatInt(t0.Add(-slack.MaxSkew-time.Second).Unix(), 10))
		}, codeStale},
		{"a timestamp before the window", func(h http.Header) {
			h.Set("X-Slack-Request-Timestamp", strconv.FormatInt(t0.Add(slack.MaxSkew+time.Second).Unix(), 10))
		}, codeStale},
	} {
		t.Run("refuses "+c.name, func(t *testing.T) {
			d := newDoor(t, nil)
			r := signed(eventsPath, eventBody)
			r.Body = io.NopCloser(unread{t})
			c.change(r.Header)
			counted := slackRequestsTotal.WithLabelValues("events", classNone, c.code)
			before := testutil.ToFloat64(counted)
			requireRefused(t, d.do(r), http.StatusBadRequest, c.code)
			assert.Zero(t, d.backend.slackCount())
			assert.Equal(t, before+1, testutil.ToFloat64(counted))
			d.requireSlackIdle(t)
		})
	}

	// The backend verifies the signature: the door can't, holding no secret.
	for _, skew := range []time.Duration{-slack.MaxSkew, slack.MaxSkew} {
		t.Run(fmt.Sprintf("forwards a timestamp %s away, at the window's edge", skew), func(t *testing.T) {
			d := newDoor(t, nil)
			r := signed(eventsPath, eventBody)
			r.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(t0.Add(skew).Unix(), 10))
			r.Header.Set("X-Slack-Signature", "v0="+digest)
			require.Equal(t, http.StatusOK, d.do(r).Code)
			assert.Equal(t, 1, d.backend.slackCount())
		})
	}
}

// Each route caps its body: by its Content-Length before any room, or as it is read.
func TestSlackBodies(t *testing.T) {
	for _, c := range []struct {
		path string
		max  int64
	}{{eventsPath, config.SlackEventsBodyBytes}, {interactivityPath, config.SlackInteractivityBodyBytes}} {
		t.Run(c.path+" takes a body of its cap", func(t *testing.T) {
			d := newDoor(t, nil)
			body := strings.Repeat("x", int(c.max))
			require.Equal(t, http.StatusOK, d.do(signed(c.path, body)).Code)
			assert.Len(t, d.backend.slackCall(0).GetBody(), int(c.max))
		})

		t.Run(c.path+" refuses a Content-Length past its cap unread", func(t *testing.T) {
			d := newDoor(t, nil)
			r := signed(c.path, "")
			r.Body, r.ContentLength = io.NopCloser(unread{t}), c.max+1
			refusal := requireRefused(t, d.do(r), http.StatusRequestEntityTooLarge, codePayloadTooLarge)
			assert.Contains(t, refusal.Message, strconv.FormatInt(c.max, 10))
			assert.Zero(t, d.backend.slackCount())
		})

		t.Run(c.path+" refuses a chunked body past its cap as it is read, and closes", func(t *testing.T) {
			d := newDoor(t, nil)
			r := signed(c.path, strings.Repeat("x", int(c.max)+1))
			r.ContentLength = -1
			rec := d.do(r)
			requireRefused(t, rec, http.StatusRequestEntityTooLarge, codePayloadTooLarge)
			assert.Equal(t, "close", rec.Header().Get("Connection"))
			assert.Zero(t, d.backend.slackCount())
			d.requireSlackIdle(t)
		})
	}

	t.Run("refuses a body cut short", func(t *testing.T) {
		d := newDoor(t, nil)
		r := signed(eventsPath, "")
		r.Body = io.NopCloser(io.MultiReader(strings.NewReader("{"), errorReader{io.ErrUnexpectedEOF}))
		rec := d.do(r)
		requireRefused(t, rec, http.StatusBadRequest, codeInvalidArguments)
		assert.Equal(t, "close", rec.Header().Get("Connection"))
		assert.Zero(t, d.backend.slackCount())
	})
}

// errorReader fails every read with err.
type errorReader struct{ err error }

func (e errorReader) Read([]byte) (int, error) { return 0, e.err }

// The backend's failure is a 500, which Slack retries for an event; the door never retries.
func TestSlackBackendFailures(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		code string
	}{
		{"unavailable", status.Error(codes.Unavailable, "down"), codeBackendUnavailable},
		{"past its deadline", status.Error(codes.DeadlineExceeded, "slow"), codeBackendUnavailable},
		{"an internal error", status.Error(codes.Internal, "boom"), codeInternal},
		{"a refusal", status.Error(codes.PermissionDenied, "only the door"), codeInternal},
		{"an argument refused", status.Error(codes.InvalidArgument, "route"), codeInternal},
	} {
		t.Run("answers "+c.name+" 500, called once", func(t *testing.T) {
			d := newDoor(t, nil)
			d.backend.answerSlack(func(context.Context, *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
				return nil, c.err
			})
			counted := slackRequestsTotal.WithLabelValues("interactivity", "bot", c.code)
			before := testutil.ToFloat64(counted)
			requireRefused(t, d.do(signed(interactivityPath, clickBody)), http.StatusInternalServerError, c.code)
			assert.Equal(t, 1, d.backend.slackCount(), "a click is taken at most once")
			assert.Equal(t, before+1, testutil.ToFloat64(counted))
			d.requireSlackIdle(t)
		})
	}

	for _, code := range []int32{0, 199, 600} {
		t.Run(fmt.Sprintf("answers a status %d 500", code), func(t *testing.T) {
			d := newDoor(t, nil)
			d.backend.answerSlack(func(context.Context, *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
				return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: code}, nil
			})
			requireRefused(t, d.do(signed(eventsPath, eventBody)), http.StatusInternalServerError, codeInternal)
		})
	}
}

// The Slack gate is its own: a flood of Slack's requests never refuses a send, a reminder or a
// shared dashboard, nor a flood of those Slack's.
func TestSlackGate(t *testing.T) {
	t.Run("refuses an address past its Slack burst, while its sends go on", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) {
			c.Endpoint.Slack.AddressRequestsPerSecond, c.Endpoint.Slack.AddressBurstRequests = 1, 2
		})
		for range 2 {
			require.Equal(t, http.StatusOK, d.do(signed(eventsPath, eventBody)).Code)
		}
		rec := d.do(signed(interactivityPath, clickBody))
		requireRefused(t, rec, http.StatusTooManyRequests, codeRateLimited)
		assert.Equal(t, "1", rec.Header().Get("Retry-After"))
		assert.Equal(t, 2, d.backend.slackCount())
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code, "the sends' bucket is another")
		d.clock.advance(time.Second)
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, eventBody)).Code)
	})

	t.Run("refuses nothing of Slack's for an address past its sends' burst", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) {
			c.Endpoint.AddressRequestsPerSecond, c.Endpoint.AddressBurstRequests = 1, 1
		})
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code)
		requireRefused(t, d.do(post(validBody)), http.StatusTooManyRequests, codeRateLimited)
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, eventBody)).Code)
	})

	t.Run("refuses Slack past its places in flight, while sends go on", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) { c.Endpoint.Slack.MaxInFlight = 2 })
		entered, release := d.backend.holdSlack()
		done := make(chan []*httptest.ResponseRecorder)
		go func() {
			done <- d.parallel(2, func(i int) *http.Request {
				r := signed(eventsPath, eventBody)
				r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
				return r
			})
		}()
		waitFor(t, entered, 2)
		other := signed(eventsPath, eventBody)
		other.Header.Set("X-Forwarded-For", elsewhere)
		rec := d.do(other)
		requireRefused(t, rec, http.StatusServiceUnavailable, codeRateLimited)
		assert.Equal(t, "1", rec.Header().Get("Retry-After"))
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code, "a send has the sends' room")
		require.Equal(t, http.StatusOK, d.do(view(shareToken(1))).Code, "a shared dashboard too")
		close(release)
		for _, rec := range <-done {
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}
		d.requireSlackIdle(t)
	})

	t.Run("refuses sends past their places in flight, while Slack goes on", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) { c.Endpoint.MaxInFlight = 1 })
		entered, release := d.backend.hold(nil)
		done := make(chan *httptest.ResponseRecorder)
		go func() { done <- d.do(post(validBody)) }()
		waitFor(t, entered, 1)
		requireRefused(t, d.do(post(validBody)), http.StatusTooManyRequests, codeRateLimited)
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, eventBody)).Code, "Slack has its own room")
		close(release)
		require.Equal(t, http.StatusOK, (<-done).Code)
	})

	t.Run("holds an address to its Slack places in flight", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) { c.Endpoint.Slack.AddressMaxInFlight = 1 })
		entered, release := d.backend.holdSlack()
		done := make(chan *httptest.ResponseRecorder)
		go func() { done <- d.do(signed(eventsPath, eventBody)) }()
		waitFor(t, entered, 1)
		requireRefused(t, d.do(signed(eventsPath, eventBody)), http.StatusServiceUnavailable, codeRateLimited)
		requireRefused(t, d.do(signed(interactivityPath, clickBody)), http.StatusTooManyRequests, codeRateLimited)
		other := signed(eventsPath, eventBody)
		other.Header.Set("X-Forwarded-For", elsewhere)
		go func() { done <- d.do(other) }()
		waitFor(t, entered, 1)
		close(release)
		for range 2 {
			require.Equal(t, http.StatusOK, (<-done).Code)
		}
		d.requireSlackIdle(t)
	})

	t.Run("refuses Slack past the pod's rate", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) {
			c.Endpoint.Slack.RequestsPerSecond, c.Endpoint.Slack.BurstRequests = 1, 2
		})
		for range 2 {
			require.Equal(t, http.StatusOK, d.do(signed(eventsPath, eventBody)).Code)
		}
		other := signed(eventsPath, eventBody)
		other.Header.Set("X-Forwarded-For", elsewhere)
		requireRefused(t, d.do(other), http.StatusServiceUnavailable, codeRateLimited)
		require.Equal(t, http.StatusOK, d.do(post(validBody)).Code)
		d.requireSlackIdle(t)
	})

	t.Run("weighs a body of unknown length as its route's cap while it is read", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) { c.Endpoint.Slack.MaxBytesInFlight = config.SlackEventsBodyBytes })
		reading, done := d.reading(t, eventsPath)
		small := signed(interactivityPath, clickBody)
		small.Header.Set("X-Forwarded-For", elsewhere)
		rec := d.do(small)
		requireRefused(t, rec, http.StatusTooManyRequests, codeRateLimited)
		assert.Equal(t, "1", rec.Header().Get("Retry-After"))
		_, _ = io.WriteString(reading, eventBody)
		require.NoError(t, reading.Close())
		require.Equal(t, http.StatusOK, (<-done).Code)
		require.Equal(t, http.StatusOK, d.do(small).Code, "the read room is free once the body is in")
		d.requireSlackIdle(t)
	})

	t.Run("holds an address to its places in the read room", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) { c.Endpoint.Slack.AddressMaxInFlight = 1 })
		reading, done := d.reading(t, eventsPath)
		counted := slackRequestsTotal.WithLabelValues("events", classNone, codeRateLimited)
		before := testutil.ToFloat64(counted)
		r := signed(eventsPath, feedBody)
		r.Header.Set("X-Slack-Retry-Num", "3")
		requireRefused(t, d.do(r), http.StatusServiceUnavailable, codeRateLimited)
		assert.Equal(t, before+1, testutil.ToFloat64(counted), "of no class, so no gap, on the last retry too")
		assert.Empty(t, d.h.slack.gaps.take())
		other := signed(eventsPath, eventBody)
		other.Header.Set("X-Forwarded-For", elsewhere)
		require.Equal(t, http.StatusOK, d.do(other).Code)
		require.NoError(t, reading.Close())
		require.Equal(t, http.StatusOK, (<-done).Code)
		d.requireSlackIdle(t)
	})

	t.Run("frees the read room while the backend answers", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) {
			c.Endpoint.Slack.MaxBytesInFlight = config.SlackEventsBodyBytes
			c.Endpoint.Slack.AddressMaxInFlight = 1
		})
		entered, release := d.backend.holdSlack()
		done := make(chan *httptest.ResponseRecorder, 2)
		go func() { done <- d.do(signed(eventsPath, feedBody)) }()
		waitFor(t, entered, 1)
		r := signed(eventsPath, eventBody)
		r.ContentLength = -1
		go func() { done <- d.do(r) }()
		waitFor(t, entered, 1)
		close(release)
		for range 2 {
			require.Equal(t, http.StatusOK, (<-done).Code)
		}
		d.requireSlackIdle(t)
	})
}

// Over a real connection, a Slack request has readFor to arrive and is answered by answerFor
// from its start, within Slack's three seconds.
func TestSlackDeadlines(t *testing.T) {
	serve := func(t *testing.T, d *door) *httptest.Server {
		server := httptest.NewServer(d)
		t.Cleanup(server.Close)
		return server
	}

	t.Run("answers a body slower than its deadline 408, with no RPC, and closes", func(t *testing.T) {
		d := newDoor(t, nil)
		d.h.slack.readFor = 200 * time.Millisecond
		server := serve(t, d)
		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		start := time.Now()
		_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: ingest\r\nX-Slack-Signature: v0=%s\r\nX-Slack-Request-Timestamp: %s\r\n"+
			"Content-Length: 1000\r\n\r\n{", eventsPath, strings.Repeat("a", 64), slackTimestamp)
		require.NoError(t, err)
		resp, refusal, err := answer(conn, 3*time.Second)
		require.NoError(t, err)
		assert.Equal(t, http.StatusRequestTimeout, resp.StatusCode)
		assert.Equal(t, codeRequestTimeout, refusal.Code)
		assert.True(t, resp.Close)
		assert.Less(t, time.Since(start), 2*time.Second)
		assert.Zero(t, d.backend.slackCount())
	})

	t.Run("answers a backend slower than the answer's deadline 500", func(t *testing.T) {
		d := newDoor(t, nil)
		d.h.slack.answerFor = 300 * time.Millisecond
		d.backend.answerSlack(func(ctx context.Context, _ *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		start := time.Now()
		requireRefused(t, d.do(signed(eventsPath, eventBody)), http.StatusInternalServerError, codeBackendUnavailable)
		assert.Less(t, time.Since(start), 2*time.Second)
	})

	// The body's deadline is the connection's, so it is cleared once the body is in: otherwise the
	// server would end the request's context while the backend records it.
	t.Run("answers a backend slower than the body's deadline", func(t *testing.T) {
		d := newDoor(t, nil)
		d.h.slack.readFor = 100 * time.Millisecond
		d.backend.answerSlack(func(ctx context.Context, _ *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
			select {
			case <-time.After(400 * time.Millisecond):
				return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusOK}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		server := serve(t, d)
		req, err := http.NewRequest(http.MethodPost, server.URL+eventsPath, bytes.NewReader([]byte(eventBody)))
		require.NoError(t, err)
		req.Header = signed(eventsPath, eventBody).Header
		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

// A Slack request's log line names its route, status and outcome, never its body, signature,
// timestamp or address.
func TestSlackLog(t *testing.T) {
	var logs bytes.Buffer
	backend := &fakeBackend{}
	h := newHandler(dial(t, backend), config.Defaults().Foundry.Events, config.SlackConfig{Enabled: true},
		slog.New(slog.NewJSONHandler(&logs, nil)), (&clock{at: t0}).now)
	routes := h.routes()
	do := func(r *http.Request) int {
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, r)
		return rec.Code
	}
	ok := signed(eventsPath, eventBody)
	require.Equal(t, http.StatusOK, do(ok))
	stale := signed(interactivityPath, clickBody)
	stale.Header.Set("X-Slack-Request-Timestamp", "1")
	require.Equal(t, http.StatusBadRequest, do(stale))
	backend.answerSlack(func(context.Context, *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
		return nil, status.Error(codes.Unavailable, "dial tcp: connection refused")
	})
	require.Equal(t, http.StatusInternalServerError, do(signed(eventsPath, eventBody)))

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], `"route":"events"`)
	assert.Contains(t, lines[0], `"outcome":"ok"`)
	assert.Contains(t, lines[1], `"route":"interactivity"`)
	assert.Contains(t, lines[1], `"outcome":"stale"`)
	assert.Contains(t, lines[2], `"level":"WARN"`)
	assert.Contains(t, lines[2], `"outcome":"backend_unavailable"`)
	assert.Contains(t, lines[2], "connection refused")
	for _, secret := range []string{"app_mention", "block_actions", ok.Header.Get("X-Slack-Signature")[3:], slackTimestamp, address} {
		assert.NotContains(t, logs.String(), secret)
	}
}
