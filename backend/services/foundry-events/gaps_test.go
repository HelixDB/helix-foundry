package foundryevents

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
)

// gapCalls are the gaps the fake backend was sent, a call each, with their Actors; answer answers
// the n-th call (OK when nil). Signed events are kept apart (signed), each answered by
// answerSigned (recorded when nil).
type gapCalls struct {
	mu           sync.Mutex
	calls        [][]*foundryv1.FoundrySlackFeedGap
	actors       []ports.Actor
	answer       func(n int) error
	signed       []*foundryv1.FoundrySlackSignedEvent
	answerSigned func() (foundryv1.SlackSignedGap, error)
}

// gaps is the gap calls b was sent.
func (b *fakeBackend) gaps() *gapCalls { return &b.slackGaps }

func (b *fakeBackend) RecordFoundrySlackFeedGaps(ctx context.Context, req *foundryv1.RecordFoundrySlackFeedGapsRequest) (
	*foundryv1.RecordFoundrySlackFeedGapsResponse, error) {
	g := b.gaps()
	g.mu.Lock()
	defer g.mu.Unlock()
	actor, _ := ports.ActorFrom(ctx)
	g.actors = append(g.actors, actor)
	if signed := req.GetSigned(); signed != nil {
		g.signed = append(g.signed, signed)
		answer := foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_RECORDED
		if g.answerSigned != nil {
			var err error
			if answer, err = g.answerSigned(); err != nil {
				return nil, err
			}
		}
		return &foundryv1.RecordFoundrySlackFeedGapsResponse{Signed: answer}, nil
	}
	g.calls = append(g.calls, req.GetGaps())
	if g.answer != nil {
		if err := g.answer(len(g.calls)); err != nil {
			return nil, err
		}
	}
	return &foundryv1.RecordFoundrySlackFeedGapsResponse{}, nil
}

// signedSent is every signed event the backend was sent, and every call's Actor.
func (g *gapCalls) signedSent() ([]*foundryv1.FoundrySlackSignedEvent, []ports.Actor) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]*foundryv1.FoundrySlackSignedEvent(nil), g.signed...), append([]ports.Actor(nil), g.actors...)
}

// sent is every gap the backend was sent, by call.
func (g *gapCalls) sent() [][]*foundryv1.FoundrySlackFeedGap {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]*foundryv1.FoundrySlackFeedGap(nil), g.calls...)
}

// t0Minute is feedBody's gap: the minute of t0 in C1 of T1.
var t0Minute = feedGap{team: "T1", channel: "C1", minute: t0.Unix() - t0.Unix()%60}

// retried is a request Slack sends to path with body on its retry num.
func retried(path, body string, num int) *http.Request {
	r := signed(path, body)
	r.Header.Set("X-Slack-Retry-Num", fmt.Sprint(num))
	return r
}

// Each class of Slack's events has its own room: a flood of one never refuses another.
func TestSlackClasses(t *testing.T) {
	counted := func(class, outcome string) func() float64 {
		c := slackRequestsTotal.WithLabelValues("events", class, outcome)
		before := testutil.ToFloat64(c)
		return func() float64 { return testutil.ToFloat64(c) - before }
	}

	t.Run("a feed flood at ten times its rate leaves mentions, clicks and replies admitted", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) {
			c.Endpoint.SlackFeed.RequestsPerSecond, c.Endpoint.SlackFeed.BurstRequests = 1, 2
		})
		refused, ok := counted("feed", codeRateLimited), counted("feed", codeOK)
		for range 20 {
			d.do(signed(eventsPath, feedBody))
		}
		assert.Equal(t, 18.0, refused())
		assert.Equal(t, 2.0, ok())
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, eventBody)).Code, "a mention")
		require.Equal(t, http.StatusOK, d.do(signed(interactivityPath, clickBody)).Code, "a click")
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, replyBody)).Code, "a reply")
		rec := d.do(signed(eventsPath, feedBody))
		requireRefused(t, rec, http.StatusServiceUnavailable, codeRateLimited)
		assert.Empty(t, rec.Header().Get("X-Slack-No-Retry"), "Slack sends it again")
		assert.Equal(t, 5, d.backend.slackCount())
	})

	t.Run("a feed holding its places leaves the others admitted", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) { c.Endpoint.SlackFeed.MaxInFlight = 2 })
		entered, release := make(chan struct{}, 8), make(chan struct{})
		d.backend.answerSlack(func(_ context.Context, req *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
			if slack.Classify(req.GetBody()) == slack.ClassFeed {
				entered <- struct{}{}
				<-release
			}
			return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusOK}, nil
		})
		done := make(chan []*httptest.ResponseRecorder)
		go func() {
			done <- d.parallel(2, func(i int) *http.Request {
				r := signed(eventsPath, feedBody)
				r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
				return r
			})
		}()
		waitFor(t, entered, 2)
		requireRefused(t, d.do(signed(eventsPath, feedBody)), http.StatusServiceUnavailable, codeRateLimited)
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, eventBody)).Code)
		require.Equal(t, http.StatusOK, d.do(signed(interactivityPath, clickBody)).Code)
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, replyBody)).Code)
		close(release)
		for _, rec := range <-done {
			require.Equal(t, http.StatusOK, rec.Code)
		}
		d.requireSlackIdle(t)
	})

	t.Run("a reply flood leaves mentions and the feed admitted", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) {
			c.Endpoint.SlackReply.RequestsPerSecond, c.Endpoint.SlackReply.BurstRequests = 1, 1
		})
		refused := counted("reply", codeRateLimited)
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, replyBody)).Code)
		requireRefused(t, d.do(signed(eventsPath, replyBody)), http.StatusServiceUnavailable, codeRateLimited)
		assert.Equal(t, 1.0, refused())
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, feedBody)).Code)
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, eventBody)).Code)
	})

	t.Run("a bot flood leaves the feed and replies admitted", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) {
			c.Endpoint.Slack.RequestsPerSecond, c.Endpoint.Slack.BurstRequests = 1, 1
		})
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, eventBody)).Code)
		requireRefused(t, d.do(signed(interactivityPath, clickBody)), http.StatusTooManyRequests, codeRateLimited)
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, feedBody)).Code)
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, replyBody)).Code)
	})

	t.Run("a click is the bot's whatever it holds", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) {
			c.Endpoint.SlackFeed.RequestsPerSecond, c.Endpoint.SlackFeed.BurstRequests = 1, 1
		})
		require.Equal(t, http.StatusOK, d.do(signed(eventsPath, feedBody)).Code)
		require.Equal(t, http.StatusOK, d.do(signed(interactivityPath, feedBody)).Code)
	})
}

// On Slack's last retry, an event of the connector's classes the door can't have taken is
// answered 200 with X-Slack-No-Retry, and its gap held; anything else is answered as before.
func TestSlackLastRetry(t *testing.T) {
	full := func(c *config.EventsConfig) {
		c.Endpoint.SlackFeed.RequestsPerSecond, c.Endpoint.SlackFeed.BurstRequests = 1, 1
		c.Endpoint.SlackReply.RequestsPerSecond, c.Endpoint.SlackReply.BurstRequests = 1, 1
		c.Endpoint.Slack.RequestsPerSecond, c.Endpoint.Slack.BurstRequests = 1, 1
	}
	spend := func(d *door) {
		for _, body := range []string{feedBody, replyBody, eventBody} {
			require.Equal(t, http.StatusOK, d.do(signed(eventsPath, body)).Code)
		}
	}
	requireGap := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "1", rec.Header().Get("X-Slack-No-Retry"))
		assert.Empty(t, rec.Body.String())
	}

	for _, c := range []struct {
		class, body string
	}{{"feed", feedBody}, {"reply", replyBody}} {
		t.Run("holds the gap of a "+c.class+" event the room refuses", func(t *testing.T) {
			d := newDoor(t, full)
			spend(d)
			recorded := slackRequestsTotal.WithLabelValues("events", c.class, "gap_recorded")
			before := testutil.ToFloat64(recorded)
			requireGap(t, d.do(retried(eventsPath, c.body, slackLastRetry)))
			assert.Equal(t, before+1, testutil.ToFloat64(recorded))
			assert.Equal(t, 3, d.backend.slackCount(), "with no RPC")
			assert.Equal(t, []feedGap{t0Minute}, d.h.slack.gaps.take())
		})

		t.Run("holds the gap of a "+c.class+" event the backend fails", func(t *testing.T) {
			for name, answer := range map[string]func() (*foundryv1.ReceiveSlackRequestResponse, error){
				"unavailable": func() (*foundryv1.ReceiveSlackRequestResponse, error) {
					return nil, status.Error(codes.Unavailable, "down")
				},
				"an error": func() (*foundryv1.ReceiveSlackRequestResponse, error) {
					return nil, status.Error(codes.Internal, "boom")
				},
				"a 503": func() (*foundryv1.ReceiveSlackRequestResponse, error) {
					return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusServiceUnavailable}, nil
				},
				"no status": func() (*foundryv1.ReceiveSlackRequestResponse, error) {
					return &foundryv1.ReceiveSlackRequestResponse{}, nil
				},
			} {
				var logs bytes.Buffer
				d := newDoor(t, nil)
				d.h.slack.logger = slog.New(slog.NewJSONHandler(&logs, nil))
				d.backend.answerSlack(func(context.Context, *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
					return answer()
				})
				requireGap(t, d.do(retried(eventsPath, c.body, slackLastRetry)))
				assert.Equal(t, 1, d.backend.slackCount(), name)
				assert.Equal(t, []feedGap{t0Minute}, d.h.slack.gaps.take(), name)
				assert.Contains(t, logs.String(), `"outcome":"gap_recorded"`, name)
				assert.Contains(t, logs.String(), `"level":"WARN"`, name)
				assert.Contains(t, logs.String(), `"err"`, name)
			}
		})
	}

	t.Run("answers an event the backend takes on the last retry as it answers", func(t *testing.T) {
		for _, code := range []int32{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized} {
			d := newDoor(t, nil)
			d.backend.answerSlack(func(context.Context, *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
				return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: code}, nil
			})
			rec := d.do(retried(eventsPath, feedBody, slackLastRetry))
			assert.Equal(t, int(code), rec.Code)
			assert.Empty(t, rec.Header().Get("X-Slack-No-Retry"))
			assert.Empty(t, d.h.slack.gaps.take())
		}
	})

	t.Run("answers before the last retry as before", func(t *testing.T) {
		d := newDoor(t, full)
		spend(d)
		requireRefused(t, d.do(retried(eventsPath, feedBody, slackLastRetry-1)), http.StatusServiceUnavailable, codeRateLimited)
		d.clock.advance(time.Second)
		d.backend.answerSlack(func(context.Context, *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
			return nil, status.Error(codes.Unavailable, "down")
		})
		requireRefused(t, d.do(retried(eventsPath, feedBody, slackLastRetry-1)), http.StatusInternalServerError, codeBackendUnavailable)
		assert.Empty(t, d.h.slack.gaps.take())
	})

	t.Run("never answers the bot's events so: they have no backfill", func(t *testing.T) {
		d := newDoor(t, full)
		spend(d)
		requireRefused(t, d.do(retried(eventsPath, eventBody, slackLastRetry)), http.StatusServiceUnavailable, codeRateLimited)
		d.clock.advance(time.Second)
		d.backend.answerSlack(func(context.Context, *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
			return nil, status.Error(codes.Unavailable, "down")
		})
		requireRefused(t, d.do(retried(eventsPath, eventBody, slackLastRetry)), http.StatusInternalServerError, codeBackendUnavailable)
		assert.Empty(t, d.h.slack.gaps.take())
	})

	t.Run("counts a gap lost when the pod holds its most, which it logs", func(t *testing.T) {
		d := newDoor(t, full)
		spend(d)
		var logs bytes.Buffer
		d.h.slack.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		lost := slackRequestsTotal.WithLabelValues("events", "feed", "gap_lost")
		before := testutil.ToFloat64(lost)
		for i := range maxFeedGaps {
			require.Zero(t, d.h.slack.gaps.add(feedGap{team: "T1", channel: "C1", minute: int64(i) * 60}))
		}
		requireGap(t, d.do(retried(eventsPath, feedBody, slackLastRetry)))
		assert.Equal(t, before+1, testutil.ToFloat64(lost))
		assert.Len(t, d.h.slack.gaps.take(), maxFeedGaps)
		// Logged, though refused for room, which hosted alarms on.
		line := strings.TrimSpace(logs.String())
		assert.Contains(t, line, `"level":"WARN"`)
		assert.Contains(t, line, `"msg":"foundry slack request"`)
		assert.Contains(t, line, `"outcome":"gap_lost"`)
	})

	// A body anyone may send names what it likes: one naming no team holds no
	// gap, and raises no alarm.
	t.Run("counts quietly a gap that names no team", func(t *testing.T) {
		d := newDoor(t, full)
		spend(d)
		var logs bytes.Buffer
		d.h.slack.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		unattributed := feedGapsTotal.WithLabelValues("unattributed")
		before := testutil.ToFloat64(unattributed)
		requireGap(t, d.do(retried(eventsPath, strings.Replace(feedBody, `"T1"`, `"t-1"`, 1), slackLastRetry)))
		assert.Equal(t, before+1, testutil.ToFloat64(unattributed))
		assert.Empty(t, d.h.slack.gaps.take())
		assert.Empty(t, logs.String())
	})

	// Its own address's room, or the read room before its body is read,
	// refusing an event holds no gap from its body: no one verified it was
	// Slack's, and anyone could send it to mint gaps. The backend verifies
	// its signature and records its gap, or refuses it.
	refusing := map[string]func(*config.EventsConfig){
		"its address's bucket": func(c *config.EventsConfig) {
			c.Endpoint.SlackFeed.AddressRequestsPerSecond, c.Endpoint.SlackFeed.AddressBurstRequests = 1, 1
		},
		"its address's places":                   func(c *config.EventsConfig) { c.Endpoint.SlackFeed.AddressMaxInFlight = 0 },
		"the read room":                          func(c *config.EventsConfig) { c.Endpoint.Slack.MaxBytesInFlight = 1 },
		"the read room's places for its address": func(c *config.EventsConfig) { c.Endpoint.Slack.AddressMaxInFlight = 0 },
	}
	refused := func(t *testing.T, name string, change func(*config.EventsConfig)) (*door, *bytes.Buffer) {
		t.Helper()
		d := newDoor(t, change)
		if name == "its address's bucket" {
			require.Equal(t, http.StatusOK, d.do(signed(eventsPath, feedBody)).Code)
		}
		logs := &bytes.Buffer{}
		d.h.slack.logger = slog.New(slog.NewJSONHandler(logs, nil))
		return d, logs
	}
	for name, change := range refusing {
		t.Run("records the gap of an event "+name+" refuses once the backend verifies it", func(t *testing.T) {
			d, logs := refused(t, name, change)
			recorded := slackRequestsTotal.WithLabelValues("events", "feed", "gap_recorded")
			before := testutil.ToFloat64(recorded)
			requireGap(t, d.do(retried(eventsPath, feedBody, slackLastRetry)))
			assert.Equal(t, before+1, testutil.ToFloat64(recorded))
			sent, actors := d.backend.gaps().signedSent()
			require.Len(t, sent, 1)
			assert.Equal(t, feedBody, string(sent[0].GetBody()), "the body as Slack signed it")
			assert.NotEmpty(t, sent[0].GetSignature())
			assert.NotEmpty(t, sent[0].GetTimestamp())
			assert.Equal(t, ports.Actor{Source: ports.SourceEventSource}, actors[0], "as the door")
			assert.Empty(t, d.h.slack.gaps.take(), "nothing held from the body")
			assert.Empty(t, logs.String(), "a gap recorded is quiet")
		})

		t.Run("refuses an event "+name+" refuses whose signature is false", func(t *testing.T) {
			d, logs := refused(t, name, change)
			d.backend.gaps().answerSigned = func() (foundryv1.SlackSignedGap, error) {
				return foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_UNVERIFIED, nil
			}
			unverified := feedGapsTotal.WithLabelValues("unverified")
			before := testutil.ToFloat64(unverified)
			requireRefused(t, d.do(retried(eventsPath, feedBody, slackLastRetry)), http.StatusServiceUnavailable, codeRateLimited)
			assert.Equal(t, before+1, testutil.ToFloat64(unverified))
			assert.Empty(t, d.h.slack.gaps.take())
			assert.Empty(t, logs.String(), "anyone may send one: no alarm")
		})
	}

	t.Run("answers a verified event no feed reads quietly", func(t *testing.T) {
		d, logs := refused(t, "its address's bucket", refusing["its address's bucket"])
		d.backend.gaps().answerSigned = func() (foundryv1.SlackSignedGap, error) { return foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_UNREAD, nil }
		unattributed := slackRequestsTotal.WithLabelValues("events", "feed", "gap_unattributed")
		before := testutil.ToFloat64(unattributed)
		requireGap(t, d.do(retried(eventsPath, feedBody, slackLastRetry)))
		assert.Equal(t, before+1, testutil.ToFloat64(unattributed))
		assert.Empty(t, logs.String())
	})

	t.Run("counts and logs a gap the backend fails to record, which hosted alarms on", func(t *testing.T) {
		for name, answer := range map[string]func() (foundryv1.SlackSignedGap, error){
			"unavailable": func() (foundryv1.SlackSignedGap, error) { return 0, status.Error(codes.Unavailable, "down") },
			"no answer": func() (foundryv1.SlackSignedGap, error) {
				return foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_UNSPECIFIED, nil
			},
		} {
			d, logs := refused(t, "the read room", refusing["the read room"])
			d.backend.gaps().answerSigned = answer
			unrecorded := slackRequestsTotal.WithLabelValues("events", "feed", "gap_unrecorded")
			before := testutil.ToFloat64(unrecorded)
			requireGap(t, d.do(retried(eventsPath, feedBody, slackLastRetry)))
			assert.Equal(t, before+1, testutil.ToFloat64(unrecorded), name)
			assert.Contains(t, logs.String(), `"level":"WARN"`, name)
			assert.Contains(t, logs.String(), `"outcome":"gap_unrecorded"`, name)
			assert.Contains(t, logs.String(), `"err"`, name)
		}
	})

	// The backend verifies at most the last-retry room's worth: past it, a
	// last retry is lost unrecorded, counted, and logged once a minute, so
	// a flood of forged ones is no flood of the backend's calls or of lines.
	t.Run("counts last retries past the last-retry room, and logs them once a flush", func(t *testing.T) {
		d, logs := refused(t, "its address's bucket", refusing["its address's bucket"])
		d.h.slack.lastRetry = newGate(config.DoorGateConfig{
			AddressRequestsPerSecond: 1, AddressBurstRequests: 1, AddressMaxInFlight: 1, RequestsPerSecond: 1, BurstRequests: 1, MaxInFlight: 1,
			MaxBytesInFlight: config.SlackEventsBodyBytes,
		}, config.SlackEventsBodyBytes)
		unrecorded := slackRequestsTotal.WithLabelValues("events", "feed", "gap_unrecorded")
		before := testutil.ToFloat64(unrecorded)
		for range 5 {
			requireGap(t, d.do(retried(eventsPath, feedBody, slackLastRetry)))
		}
		sent, _ := d.backend.gaps().signedSent()
		assert.Len(t, sent, 1, "one verified, within the room's rate")
		assert.Equal(t, before+4, testutil.ToFloat64(unrecorded))
		assert.Empty(t, logs.String(), "no line a request")
		d.h.slack.flushGaps(context.Background())
		line := strings.TrimSpace(logs.String())
		assert.Contains(t, line, `"level":"WARN"`)
		assert.Contains(t, line, `"msg":"foundry slack last retries unrecorded"`)
		assert.Contains(t, line, `"requests":4`)
		logs.Reset()
		d.h.slack.flushGaps(context.Background())
		assert.Empty(t, logs.String(), "counted once")
	})

	t.Run("refuses a bot event the read room refused, read in the last-retry room", func(t *testing.T) {
		d, _ := refused(t, "the read room", refusing["the read room"])
		requireRefused(t, d.do(retried(eventsPath, eventBody, slackLastRetry)), http.StatusServiceUnavailable, codeRateLimited)
		sent, _ := d.backend.gaps().signedSent()
		assert.Empty(t, sent)
	})
}

// A gap is read from the body as the backend reads its team, unverified.
func TestFeedGapOf(t *testing.T) {
	now := time.Unix(1_700_000_125, 0)
	event := `"event":{"type":"message","channel":"C1"}`
	for _, c := range []struct {
		name, body string
		want       feedGap
		ok         bool
	}{
		{"the envelope's team, the event's channel and minute",
			`{"team_id":"T1","event_time":1700000059,` + event + `}`, feedGap{team: "T1", channel: "C1", minute: 1_700_000_040}, true},
		{"the first authorization's team and org",
			`{"team_id":"T1","enterprise_id":"E0","authorizations":[{"team_id":"T2","enterprise_id":"E2"},{"team_id":"T3"}],` + event + `}`,
			feedGap{team: "T2", enterprise: "E2", channel: "C1", minute: 1_700_000_100}, true},
		{"the envelope's org when the authorization names none",
			`{"team_id":"T1","enterprise_id":"E0","authorizations":[{"team_id":"T2"}],` + event + `}`,
			feedGap{team: "T2", enterprise: "E0", channel: "C1", minute: 1_700_000_100}, true},
		{"the context's team when no authorization names one",
			`{"team_id":"T1","context_team_id":"T4","context_enterprise_id":"E4","authorizations":[{"team_id":""}],` + event + `}`,
			feedGap{team: "T4", enterprise: "E4", channel: "C1", minute: 1_700_000_100}, true},
		{"an org of no id's form left out",
			`{"team_id":"T1","enterprise_id":"e-0",` + event + `}`, feedGap{team: "T1", channel: "C1", minute: 1_700_000_100}, true},
		{"a channel object's id",
			`{"team_id":"T1","event":{"type":"channel_rename","channel":{"id":"C2","name":"new"}}}`,
			feedGap{team: "T1", channel: "C2", minute: 1_700_000_100}, true},
		{"no channel: every channel",
			`{"team_id":"T1","event":{"type":"message"}}`, feedGap{team: "T1", minute: 1_700_000_100}, true},
		{"a channel of no id's form: every channel",
			`{"team_id":"T1","event":{"type":"message","channel":"c 1"}}`, feedGap{team: "T1", minute: 1_700_000_100}, true},
		{"a channel of another type: every channel",
			`{"team_id":"T1","event":{"type":"message","channel":7}}`, feedGap{team: "T1", minute: 1_700_000_100}, true},
		{"no team", `{` + event + `}`, feedGap{}, false},
		{"a team of no id's form", `{"team_id":"t1",` + event + `}`, feedGap{}, false},
		{"a channel shared with another organisation", `{"team_id":"T1","is_ext_shared_channel":true,` + event + `}`, feedGap{}, false},
		{"no JSON", `payload=x`, feedGap{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := feedGapOf([]byte(c.body), now)
			assert.Equal(t, c.ok, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

// The gaps a pod holds are a set, up to maxFeedGaps, taken in order.
func TestFeedGaps(t *testing.T) {
	g := &feedGaps{held: map[feedGap]struct{}{}}
	b, a2, a1 := feedGap{team: "T2", minute: 60}, feedGap{team: "T1", channel: "C2", minute: 0}, feedGap{team: "T1", channel: "C1", minute: 120}
	a0 := feedGap{team: "T1", channel: "C1", minute: 60}
	e := feedGap{team: "T1", enterprise: "E1", minute: 0}
	assert.Zero(t, g.add(b, a2, a1, a0, e, a1))
	assert.Equal(t, []feedGap{a0, a1, a2, e, b}, g.take())
	assert.Empty(t, g.take(), "taken")

	for i := range maxFeedGaps {
		require.Zero(t, g.add(feedGap{team: "T1", minute: int64(i)}))
	}
	assert.Zero(t, g.add(feedGap{team: "T1", minute: 0}), "one held already")
	assert.Equal(t, 2, g.add(b, feedGap{team: "T1", minute: maxFeedGaps}))
	assert.Len(t, g.take(), maxFeedGaps)
}

// A flush sends the gaps held, a batch a call, as the door; a failed call's gaps wait for the next.
func TestFlushGaps(t *testing.T) {
	hold := func(d *door, n int) []feedGap {
		gaps := make([]feedGap, n)
		for i := range gaps {
			gaps[i] = feedGap{team: "T1", enterprise: "E1", channel: "C1", minute: int64(i) * 60}
		}
		d.h.slack.gaps.add(gaps...)
		return gaps
	}
	total := func(outcome string) func() float64 {
		c := feedGapsTotal.WithLabelValues(outcome)
		before := testutil.ToFloat64(c)
		return func() float64 { return testutil.ToFloat64(c) - before }
	}

	t.Run("sends nothing when none is held", func(t *testing.T) {
		d := newDoor(t, nil)
		d.h.slack.flushGaps(context.Background())
		assert.Empty(t, d.backend.gaps().sent())
	})

	t.Run("sends every gap held, a batch a call, as the door", func(t *testing.T) {
		d := newDoor(t, nil)
		gaps := hold(d, 2*gapBatch+5)
		flushed := total("flushed")
		d.h.slack.flushGaps(context.Background())
		sent := d.backend.gaps().sent()
		require.Len(t, sent, 3)
		assert.Len(t, sent[0], gapBatch)
		assert.Len(t, sent[2], 5)
		got := sent[0][1]
		assert.Equal(t, "T1", got.GetTeamId())
		assert.Equal(t, "E1", got.GetEnterpriseId())
		assert.Equal(t, "C1", got.GetChannelId())
		assert.Equal(t, timestamppb.New(time.Unix(gaps[1].minute, 0)).AsTime(), got.GetMinute().AsTime())
		assert.Equal(t, ports.Actor{Source: ports.SourceEventSource}, d.backend.gaps().actors[0])
		assert.Equal(t, float64(len(gaps)), flushed())
		assert.Empty(t, d.h.slack.gaps.take())
	})

	t.Run("holds a failed call's gaps and those after it for the next flush", func(t *testing.T) {
		d := newDoor(t, nil)
		gaps := hold(d, 2*gapBatch+5)
		d.backend.gaps().answer = func(n int) error {
			if n == 2 {
				return status.Error(codes.Unavailable, "down")
			}
			return nil
		}
		retried := total("retried")
		d.h.slack.flushGaps(context.Background())
		assert.Len(t, d.backend.gaps().sent(), 2, "stops at the failure")
		assert.Equal(t, float64(gapBatch+5), retried())
		assert.Equal(t, gaps[gapBatch:], d.h.slack.gaps.take())
	})

	t.Run("loses what it can't hold again", func(t *testing.T) {
		d := newDoor(t, nil)
		hold(d, 3)
		d.backend.gaps().answer = func(int) error {
			// Meanwhile the pod's requests fill its gaps.
			for i := range maxFeedGaps - 1 {
				d.h.slack.gaps.add(feedGap{team: "T2", minute: int64(i)})
			}
			return status.Error(codes.DeadlineExceeded, "slow")
		}
		lost := total("lost")
		d.h.slack.flushGaps(context.Background())
		assert.Equal(t, 2.0, lost())
		assert.Len(t, d.h.slack.gaps.take(), maxFeedGaps)
	})

	for _, code := range []codes.Code{codes.InvalidArgument, codes.PermissionDenied, codes.Unimplemented} {
		t.Run("loses a call refused for good, "+code.String()+", and sends on", func(t *testing.T) {
			d := newDoor(t, nil)
			hold(d, gapBatch+1)
			d.backend.gaps().answer = func(n int) error {
				if n == 1 {
					return status.Error(code, "no")
				}
				return nil
			}
			lost, flushed := total("lost"), total("flushed")
			d.h.slack.flushGaps(context.Background())
			assert.Len(t, d.backend.gaps().sent(), 2)
			assert.Equal(t, float64(gapBatch), lost())
			assert.Equal(t, 1.0, flushed())
			assert.Empty(t, d.h.slack.gaps.take())
		})
	}

	t.Run("flushes every period until its context ends", func(t *testing.T) {
		d := newDoor(t, nil)
		hold(d, 1)
		ctx, cancel := context.WithCancel(context.Background())
		stopped := make(chan struct{})
		go func() {
			d.h.slack.flushGapsEvery(ctx, time.Millisecond)
			close(stopped)
		}()
		require.Eventually(t, func() bool { return len(d.backend.gaps().sent()) == 1 }, 5*time.Second, time.Millisecond)
		hold(d, 2)
		require.Eventually(t, func() bool { return len(d.backend.gaps().sent()) == 2 }, 5*time.Second, time.Millisecond)
		cancel()
		<-stopped
	})
}
