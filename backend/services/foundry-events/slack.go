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

// slackDoor serves Slack's routes, POST /v1/slack/events and POST /v1/slack/interactivity,
// while foundry.slack is on. The door holds no signing secret: it refuses what no signed
// request from Slack looks like, and the backend verifies the signature. It checks a request in
// this order, cheapest first, and answers the first check it fails:
//  1. The client address's bucket of the Slack gate (its own, apart from the sends'): 429
//     rate_limited, unlogged.
//  2. A Content-Length past the route's cap (config.SlackEventsBodyBytes,
//     SlackInteractivityBodyBytes): 413 payload_too_large.
//  3. The signature headers (slack.Precheck), with no RPC: anything but one X-Slack-Signature
//     of "v0=" and 64 lowercase hex digits and one X-Slack-Request-Timestamp of Unix seconds is
//     400 unsigned; a timestamp more than slack.MaxSkew from now is 400 stale.
//  4. The room of the Slack gate, held until the request is answered: one of the address's
//     places in flight, then the pod's places, bytes and bucket: 429 rate_limited, Retry-After 1.
//  5. The body, within the route's cap and readFor of the request's start: 413, or 408
//     request_timeout, or 400 invalid_arguments for one cut short; the connection is closed.
//  6. ReceiveSlackRequest, as the door (ports.SourceEventSource with no token), with the raw
//     body, the headers and the retry's number and reason, by answerFor from the request's
//     start. Its answer is the door's: its status, media type and body (url_verification's
//     challenge). Any error, or a status that is no HTTP status, is 500, so Slack retries an
//     event; the door never retries, so a click is taken at most once.
//
// Every refusal but rate_limited's is the door's JSON error body, which Slack ignores. Each
// request logs one line, its route, status and outcome, never its body, headers or address,
// and counts in helix_foundry_slack_requests_total.
type slackDoor struct {
	backend backendAPI
	gate    *gate
	// readFor and answerFor are slackReadFor and slackFor, but in tests.
	readFor   time.Duration
	answerFor time.Duration
	logger    *slog.Logger
	// now is the clock of the buckets and of the timestamp's window.
	now func() time.Time
}

// route is the handler of one of Slack's routes.
func (s *slackDoor) route(route slackRoute) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serveHTTP(w, r, route) })
}

func (s *slackDoor) serveHTTP(w http.ResponseWriter, r *http.Request, route slackRoute) {
	start := time.Now()
	requestID := r.Header.Get("X-Request-ID")
	if !requestIDPattern.MatchString(requestID) {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-ID", requestID)
	answer, fail := s.serve(w, r, route, start)
	var outcome string
	httpStatus := int(answer.GetHttpStatus())
	if fail != nil {
		outcome, httpStatus = fail.code, fail.status
		fail.write(w)
	} else {
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
		if answer.GetContentType() != "" {
			w.Header().Set("Content-Type", answer.GetContentType())
		}
		w.WriteHeader(httpStatus)
		_, _ = w.Write(answer.GetBody()) // best-effort: Slack may have gone
	}
	slackRequestsTotal.WithLabelValues(route.label, outcome).Inc()
	if fail != nil && fail.quiet {
		return
	}
	attrs := []any{"route", route.label, "status", httpStatus, "outcome", outcome, "request_id", requestID, "ms", time.Since(start).Milliseconds()}
	level := slog.LevelInfo
	if httpStatus >= http.StatusInternalServerError {
		level = slog.LevelWarn
	}
	if fail != nil && fail.cause != nil {
		attrs = append(attrs, "err", fail.cause.Error())
	}
	s.logger.Log(r.Context(), level, "foundry slack request", attrs...)
}

// serve checks and forwards one request (slackDoor's steps) that the door took at start.
func (s *slackDoor) serve(w http.ResponseWriter, r *http.Request, route slackRoute, start time.Time) (*foundryv1.ReceiveSlackRequestResponse, *failure) {
	// The connection's read deadline bounds the body, and whatever of it the server reads past a
	// refusal; it is cleared once the body is in, or the server would end the request's context,
	// the backend's call in flight, when the connection's read next timed out.
	deadline := http.NewResponseController(w)
	_ = deadline.SetReadDeadline(start.Add(s.readFor)) // net/http's server supports it; a test's recorder doesn't
	now := s.now()
	address := clientAddress(r)
	if wait := s.gate.address(address, now); wait > 0 {
		return nil, limited(wait, "this address sends too many Slack requests; send again after Retry-After")
	}
	if r.ContentLength > route.maxBody {
		return nil, tooLarge(route.maxBody)
	}
	signatures, timestamps := r.Header.Values("X-Slack-Signature"), r.Header.Values("X-Slack-Request-Timestamp")
	if len(signatures) != 1 || len(timestamps) != 1 {
		return nil, &failure{status: http.StatusBadRequest, code: codeUnsigned,
			message: "send one X-Slack-Signature and one X-Slack-Request-Timestamp, as Slack signs its requests"}
	}
	switch err := slack.Precheck(timestamps[0], signatures[0], now); {
	case errors.Is(err, slack.ErrStale):
		return nil, &failure{status: http.StatusBadRequest, code: codeStale,
			message: "X-Slack-Request-Timestamp is more than five minutes from now"}
	case err != nil:
		return nil, &failure{status: http.StatusBadRequest, code: codeUnsigned,
			message: `X-Slack-Signature must be "v0=" and 64 hex digits, and X-Slack-Request-Timestamp Unix seconds`}
	}

	leave, ok := s.gate.addressPlace(address)
	if !ok {
		return nil, limited(time.Second, "this address has too many Slack requests in flight; send again after Retry-After")
	}
	defer leave()
	release, ok := s.gate.enter(r.ContentLength, now)
	if !ok {
		return nil, limited(time.Second, "Foundry is taking too many Slack requests at once; send again after Retry-After")
	}
	defer release()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, route.maxBody))
	if err != nil {
		// What is left of the body is no request to read next.
		w.Header().Set("Connection", "close")
		if tooBig, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, tooLarge(tooBig.Limit)
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, &failure{status: http.StatusRequestTimeout, code: codeRequestTimeout, message: "the body took too long to arrive"}
		}
		return nil, invalid("the body was cut short")
	}
	_ = deadline.SetReadDeadline(time.Time{})

	// Slack numbers its retries of an event from 1; anything else is a first delivery.
	retry, err := strconv.ParseInt(r.Header.Get("X-Slack-Retry-Num"), 10, 32)
	if err != nil || retry < 0 {
		retry = 0
	}
	reason := r.Header.Get("X-Slack-Retry-Reason")
	if !retryReasonForm.MatchString(reason) {
		reason = ""
	}
	ctx, cancel := context.WithDeadline(foundryapi.AttachActor(r.Context(), ports.Actor{Source: ports.SourceEventSource}), start.Add(s.answerFor))
	defer cancel()
	answer, err := s.backend.ReceiveSlackRequest(ctx, &foundryv1.ReceiveSlackRequestRequest{
		Route: route.route, Body: body, Signature: signatures[0], Timestamp: timestamps[0], RetryNum: int32(retry), RetryReason: reason,
	})
	switch status.Code(err) {
	case codes.OK:
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return nil, &failure{status: http.StatusInternalServerError, code: codeBackendUnavailable, cause: err,
			message: "Foundry couldn't take this request in time"}
	default:
		return nil, &failure{status: http.StatusInternalServerError, code: codeInternal, cause: err,
			message: "Foundry failed to take this request"}
	}
	if code := answer.GetHttpStatus(); code < http.StatusOK || code > 599 {
		return nil, &failure{status: http.StatusInternalServerError, code: codeInternal, cause: fmt.Errorf("the backend answered status %d", code),
			message: "Foundry failed to take this request"}
	}
	return answer, nil
}
