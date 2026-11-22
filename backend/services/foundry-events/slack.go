package foundryevents

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryapi "github.com/helixdb/helix-foundry/backend/api"
	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
)

const (
	// slackReadFor bounds how long a Slack request's body takes to arrive, from when the door
	// takes the request: Slack sends from its own servers, at once.
	slackReadFor = time.Second
	// slackFor bounds a Slack request from when the door takes it to the backend's answer, its
	// body's arrival included: Slack waits 3 s for an answer, then counts the request failed (an
	// event is retried; a click shows its person an error).
	slackFor = 2500 * time.Millisecond
)

// retryReasonForm is an X-Slack-Retry-Reason the door passes on: Slack's are short words, such
// as http_timeout. Any other is passed on as none.
var retryReasonForm = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// The door's own codes for Slack's requests, beside handler's.
const (
	// codeUnsigned is a request without one well-formed X-Slack-Signature ("v0=" and 64 hex
	// digits) and X-Slack-Request-Timestamp (Unix seconds).
	codeUnsigned = "unsigned"
	// codeStale is a request whose timestamp is more than slack.MaxSkew from the door's clock.
	codeStale = "stale"
)

// slackRoute is one of Slack's two routes on the door: the route the backend is told, its
// label in the metric and the log, and its body's cap.
type slackRoute struct {
	route   foundryv1.SlackRoute
	label   string
	maxBody int64
}

var (
	// slackEvents is POST /v1/slack/events: the Events API, url_verification included.
	slackEvents = slackRoute{route: foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, label: "events", maxBody: config.SlackEventsBodyBytes}
	// slackInteractivity is POST /v1/slack/interactivity: clicks.
	slackInteractivity = slackRoute{
		route: foundryv1.SlackRoute_SLACK_ROUTE_INTERACTIVITY, label: "interactivity", maxBody: config.SlackInteractivityBodyBytes,
	}
)

// slackLastRetry is X-Slack-Retry-Num of Slack's last retry of an event: past it Slack drops the
// event, telling no one.
const slackLastRetry = 3

// classNone is a request's class in the metric and the log until its body is read and classified.
const classNone = "none"

// slackDoor serves Slack's routes, POST /v1/slack/events and POST /v1/slack/interactivity,
// while foundry.slack is on. The door holds no signing secret: it refuses what no signed
// request from Slack looks like, and the backend verifies the signature. It checks a request in
// this order, cheapest first, and answers the first check it fails:
//  1. A Content-Length past the route's cap (config.SlackEventsBodyBytes,
//     SlackInteractivityBodyBytes): 413 payload_too_large.
//  2. The signature headers (slack.Precheck), with no RPC: anything but one X-Slack-Signature
//     of "v0=" and 64 lowercase hex digits and one X-Slack-Request-Timestamp of Unix seconds is
//     400 unsigned; a timestamp more than slack.MaxSkew from now is 400 stale.
//  3. The read room, held only while the body is read: one of the address's places and the
//     body's bytes (the route's cap for a body of unknown length), of endpoint.slack's bounds.
//  4. The body, within the route's cap and readFor of the request's start: 413, or 408
//     request_timeout, or 400 invalid_arguments for one cut short; the connection is closed.
//  5. Its class (slack.Classify; a click is always slack.ClassBot), and that class's gate,
//     held until the backend answers: ClassBot's endpoint.slack, ClassReply's
//     endpoint.slack_reply, ClassFeed's endpoint.slack_feed, each charged in a gate's order
//     (the address's bucket, its place, then the pod's places, bytes and bucket). No class
//     takes another's room, so a flood of a channel's messages never refuses a mention, a click
//     or a reply in a thread, nor they it.
//  6. ReceiveSlackRequest, as the door (ports.SourceEventSource with no token), with the raw
//     body, the headers and the retry's number and reason, by answerFor from the request's
//     start. Its answer is the door's: its status, media type and body (url_verification's
//     challenge). Any error, or a status that is no HTTP status, is 500, so Slack retries an
//     event; the door never retries, so a click is taken at most once.
//
// A request refused for room (3 or 5) is 429 rate_limited for a click, and 503 for an event, which
// Slack sends again. On Slack's last retry (slackLastRetry), an event of ClassReply or ClassFeed
// that its class's pod-wide room refuses, or that the backend fails, is lost to Slack: it is
// answered 200 with X-Slack-No-Retry, and its team's, channel's and minute's gap is held (feedGaps)
// for the backend to record as a gap of the Slack data connector, whose backfill reads it again
// (gap_recorded), or counted gap_lost past the cap, which hosted alarms on; one whose body names no
// team, or a channel shared with another organisation, which no feed reads, is gap_unattributed.
// No one has verified such a request was Slack's, so an event its own address's room refuses (its
// bucket or places, in 3 or 5), or that the read room refuses before its body is read, holds no
// gap and raises no alarm, which anyone could otherwise do at will: it is refused as any other
// is, and counted in helix_foundry_slack_feed_gaps_total as unverified. A ClassBot event is never
// answered so: the bot's events have no backfill.
//
// Every refusal but a gap's 200 is the door's JSON error body, which Slack ignores. Each request
// logs one line, its route, class, status and outcome, never its body, headers or address, but a
// refusal for room, which a flood is answered with (a gap lost for room is logged all the same);
// and counts in helix_foundry_slack_requests_total.
type slackDoor struct {
	backend backendAPI
	// read is the room bodies are read in, before their class is known: of its gate (endpoint.slack's
	// bounds) only the address places and the bytes.
	read *gate
	// classes is each class's gate, held from its body's read to the backend's answer.
	classes map[slack.Class]*gate
	// gaps are the minutes of channels whose events Slack drops, until flushGaps sends them.
	gaps *feedGaps
	// readFor and answerFor are slackReadFor and slackFor, but in tests.
	readFor   time.Duration
	answerFor time.Duration
	logger    *slog.Logger
	// now is the clock of the buckets, of the timestamp's window, and of a gap's minute when its
	// event names none.
	now func() time.Time
}

// newSlackDoor is the door of Slack's routes under the gates of cfg, before backend.
func newSlackDoor(backend backendAPI, cfg config.EventsEndpointConfig, logger *slog.Logger, now func() time.Time) *slackDoor {
	largest := int64(max(config.SlackEventsBodyBytes, config.SlackInteractivityBodyBytes))
	return &slackDoor{
		backend: backend, read: newGate(cfg.Slack, largest),
		classes: map[slack.Class]*gate{
			slack.ClassBot:   newGate(cfg.Slack, largest),
			slack.ClassReply: newGate(cfg.SlackReply, config.SlackEventsBodyBytes),
			slack.ClassFeed:  newGate(cfg.SlackFeed, config.SlackEventsBodyBytes),
		},
		gaps:    &feedGaps{held: map[feedGap]struct{}{}},
		readFor: slackReadFor, answerFor: slackFor, logger: logger, now: now,
	}
}

// route is the handler of one of Slack's routes.
func (s *slackDoor) route(route slackRoute) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serveHTTP(w, r, route) })
}

// slackAnswer is how the door answered a request: the backend's answer, or the door's failure,
// or a gap's 200 (gap, its outcome), and the request's class.
type slackAnswer struct {
	backend *foundryv1.ReceiveSlackRequestResponse
	fail    *failure
	// gap is gap_recorded, gap_lost or gap_unattributed for an event answered 200 with
	// X-Slack-No-Retry on Slack's last retry; "" otherwise.
	gap   string
	class string
	// cause is the backend's failure behind a gap, logged.
	cause error
}

func (s *slackDoor) serveHTTP(w http.ResponseWriter, r *http.Request, route slackRoute) {
	start := time.Now()
	requestID := r.Header.Get("X-Request-ID")
	if !requestIDPattern.MatchString(requestID) {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-ID", requestID)
	answer := s.serve(w, r, route, start)
	var outcome string
	httpStatus := int(answer.backend.GetHttpStatus())
	quiet := false
	switch {
	case answer.gap != "":
		// A gap lost is always logged, which hosted alarms on; any other for room isn't, as a
		// flood's refusals aren't.
		outcome, httpStatus, quiet = answer.gap, http.StatusOK, answer.cause == nil && answer.gap != "gap_lost"
		w.Header().Set("X-Slack-No-Retry", "1")
		w.WriteHeader(http.StatusOK)
	case answer.fail != nil:
		outcome, httpStatus, quiet = answer.fail.code, answer.fail.status, answer.fail.quiet
		answer.fail.write(w)
	default:
		switch {
		case httpStatus < http.StatusMultipleChoices:
			outcome = codeOK
		case httpStatus == http.StatusUnauthorized:
			outcome = "unverified" // a signature the backend found false
		case httpStatus == http.StatusNotFound:
			outcome = "disabled" // foundry.slack off where the backend runs
		case httpStatus < http.StatusInternalServerError:
			outcome = "refused"
		default:
			outcome = codeInternal
		}
		if answer.backend.GetContentType() != "" {
			w.Header().Set("Content-Type", answer.backend.GetContentType())
		}
		w.WriteHeader(httpStatus)
		_, _ = w.Write(answer.backend.GetBody()) // best-effort: Slack may have gone
	}
	slackRequestsTotal.WithLabelValues(route.label, answer.class, outcome).Inc()
	if quiet {
		return
	}
	attrs := []any{"route", route.label, "class", answer.class, "status", httpStatus, "outcome", outcome, "request_id", requestID,
		"ms", time.Since(start).Milliseconds()}
	level := slog.LevelInfo
	if httpStatus >= http.StatusInternalServerError || answer.gap != "" {
		level = slog.LevelWarn
	}
	cause := answer.cause
	if answer.fail != nil {
		cause = answer.fail.cause
	}
	if cause != nil {
		attrs = append(attrs, "err", cause.Error())
	}
	s.logger.Log(r.Context(), level, "foundry slack request", attrs...)
}

// serve checks and forwards one request (slackDoor's steps) that the door took at start.
func (s *slackDoor) serve(w http.ResponseWriter, r *http.Request, route slackRoute, start time.Time) slackAnswer {
	// The connection's read deadline bounds the body, and whatever of it the server reads past a
	// refusal; it is cleared once the body is in, or the server would end the request's context,
	// the backend's call in flight, when the connection's read next timed out.
	deadline := http.NewResponseController(w)
	_ = deadline.SetReadDeadline(start.Add(s.readFor)) // net/http's server supports it; a test's recorder doesn't
	now := s.now()
	address := clientAddress(r)
	fail := func(f *failure) slackAnswer { return slackAnswer{fail: f, class: classNone} }
	if r.ContentLength > route.maxBody {
		return fail(tooLarge(route.maxBody))
	}
	signatures, timestamps := r.Header.Values("X-Slack-Signature"), r.Header.Values("X-Slack-Request-Timestamp")
	if len(signatures) != 1 || len(timestamps) != 1 {
		return fail(&failure{status: http.StatusBadRequest, code: codeUnsigned,
			message: "send one X-Slack-Signature and one X-Slack-Request-Timestamp, as Slack signs its requests"})
	}
	switch err := slack.Precheck(timestamps[0], signatures[0], now); {
	case errors.Is(err, slack.ErrStale):
		return fail(&failure{status: http.StatusBadRequest, code: codeStale,
			message: "X-Slack-Request-Timestamp is more than five minutes from now"})
	case err != nil:
		return fail(&failure{status: http.StatusBadRequest, code: codeUnsigned,
			message: `X-Slack-Signature must be "v0=" and 64 hex digits, and X-Slack-Request-Timestamp Unix seconds`})
	}
	// Slack numbers its retries of an event from 1; anything else is a first delivery.
	retry, err := strconv.ParseInt(r.Header.Get("X-Slack-Retry-Num"), 10, 32)
	if err != nil || retry < 0 {
		retry = 0
	}
	// name is the request's class once its body is read, and lost says the request is an event of
	// the connector's classes on Slack's last retry: anything but the backend's taking it is a gap.
	name, lost := classNone, false
	var body []byte
	// busy refuses a request the room can't take: a click 429, an event 503, which Slack sends
	// again, but a lost one 200 with its gap held when its class's pod-wide room refused it
	// (held). A lost one its own address's room refused, or an event on Slack's last retry
	// refused before its body is read, is refused all the same, and counted unverified.
	busy := func(after time.Duration, message string, held bool) slackAnswer {
		answer := slackAnswer{fail: limited(after, message), class: name}
		events := route.route == foundryv1.SlackRoute_SLACK_ROUTE_EVENTS
		if events {
			answer.fail.status = http.StatusServiceUnavailable
		}
		switch {
		case lost && held:
			answer.gap = s.holdGap(body, now)
		case lost || body == nil && events && retry >= slackLastRetry:
			feedGapsTotal.WithLabelValues("unverified").Inc()
		}
		return answer
	}

	leave, ok := s.read.addressPlace(address)
	if !ok {
		return busy(time.Second, "this address has too many Slack requests in flight; send again after Retry-After", false)
	}
	weight := r.ContentLength
	if weight < 0 {
		weight = route.maxBody
	}
	if !s.read.bytes.TryAcquire(weight) {
		leave()
		return busy(time.Second, "Foundry is reading too many Slack requests at once; send again after Retry-After", false)
	}
	body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, route.maxBody))
	s.read.bytes.Release(weight)
	leave()
	if err != nil {
		// What is left of the body is no request to read next.
		w.Header().Set("Connection", "close")
		if tooBig, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return fail(tooLarge(tooBig.Limit))
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return fail(&failure{status: http.StatusRequestTimeout, code: codeRequestTimeout, message: "the body took too long to arrive"})
		}
		return fail(invalid("the body was cut short"))
	}
	_ = deadline.SetReadDeadline(time.Time{})

	class := slack.ClassBot
	if route.route == foundryv1.SlackRoute_SLACK_ROUTE_EVENTS {
		class = slack.Classify(body)
	}
	name, lost = class.String(), retry >= slackLastRetry && class != slack.ClassBot
	room := s.classes[class]
	if wait := room.address(address, now); wait > 0 {
		return busy(wait, "this address sends too many Slack requests; send again after Retry-After", false)
	}
	leave, ok = room.addressPlace(address)
	if !ok {
		return busy(time.Second, "this address has too many Slack requests in flight; send again after Retry-After", false)
	}
	defer leave()
	release, ok := room.enter(int64(len(body)), now)
	if !ok {
		return busy(time.Second, "Foundry is taking too many Slack requests at once; send again after Retry-After", true)
	}
	defer release()

	reason := r.Header.Get("X-Slack-Retry-Reason")
	if !retryReasonForm.MatchString(reason) {
		reason = ""
	}
	ctx, cancel := context.WithDeadline(foundryapi.AttachActor(r.Context(), ports.Actor{Source: ports.SourceEventSource}), start.Add(s.answerFor))
	defer cancel()
	answer, err := s.backend.ReceiveSlackRequest(ctx, &foundryv1.ReceiveSlackRequestRequest{
		Route: route.route, Body: body, Signature: signatures[0], Timestamp: timestamps[0], RetryNum: int32(retry), RetryReason: reason,
	})
	var failed *failure
	switch code := answer.GetHttpStatus(); status.Code(err) {
	case codes.OK:
		switch {
		case code < http.StatusOK || code > 599:
			failed = &failure{status: http.StatusInternalServerError, code: codeInternal, cause: fmt.Errorf("the backend answered status %d", code),
				message: "Foundry failed to take this request"}
		case code < http.StatusInternalServerError || !lost:
			return slackAnswer{backend: answer, class: name}
		default:
			return slackAnswer{gap: s.holdGap(body, now), class: name, cause: fmt.Errorf("the backend answered status %d", code)}
		}
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		failed = &failure{status: http.StatusInternalServerError, code: codeBackendUnavailable, cause: err,
			message: "Foundry couldn't take this request in time"}
	default:
		failed = &failure{status: http.StatusInternalServerError, code: codeInternal, cause: err, message: "Foundry failed to take this request"}
	}
	if lost {
		return slackAnswer{gap: s.holdGap(body, now), class: name, cause: failed.cause}
	}
	return slackAnswer{fail: failed, class: name}
}
