package foundryevents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryapi "github.com/helixdb/helix-foundry/backend/api"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
)

const (
	// maxReminderBody bounds a reminder request's body: an address, a zone
	// and a consent fit in far less.
	maxReminderBody = 2 << 10
	// reminderFor bounds the backend's answer to a reminder request, a
	// short transaction on one row.
	reminderFor = 5 * time.Second
	// reminderLimitedFor is the Retry-After of a request past the backend's
	// hourly cap when its refusal says no wait of its own.
	reminderLimitedFor = time.Minute
)

// reminderRoute is one of the reminders' two routes, by its metric label.
type reminderRoute string

const (
	routeCreate reminderRoute = "create"
	routeStop   reminderRoute = "stop"
)

// reminderBody is a request for reminders as the site sends it.
type reminderBody struct {
	Email    string `json:"email"`
	TimeZone string `json:"time_zone"`
	Consent  string `json:"consent"`
}

// stopBody is a request to stop an address's reminders.
type stopBody struct {
	Email string `json:"email"`
}

// reminderAnswer is a 200 to a request for reminders: when the address's
// next one goes, in RFC 3339 UTC, and whether it already had a schedule
// running, left as it was.
type reminderAnswer struct {
	NextSendAt string `json:"next_send_at"`
	Again      bool   `json:"again"`
}

// reminders serves the site's two routes of the reminders people ask for on
// the landing page, from a phone, to try Foundry on a computer:
//
//	POST /v1/reminders       {"email", "time_zone", "consent"} → {"next_send_at", "again"}
//	POST /v1/reminders/stop  {"email"}                         → {}
//
// each with Authorization: Bearer and the site's secret. The door holds no
// secret: it passes the bearer's SHA-256 on, and the backend compares it with
// foundry.reminders.secret_sha256. A request is checked in this order,
// cheapest first, and answered at the first check it fails:
//  1. The client address's bucket, shared with the sends: 429 rate_limited.
//  2. The headers: a Content-Length past maxReminderBody is 413
//     payload_too_large, a body not declared JSON 415
//     unsupported_media_type, and no Authorization: Bearer 401
//     unauthorized.
//  3. The pod's room, as a send takes it: 429 rate_limited.
//  4. The body, one JSON object with no other field, within maxReminderBody
//     and readFor: 413, 408 request_timeout or 400 invalid_arguments; an
//     address or consent missing is 400 too.
//  5. The backend, as the door (ports.SourceEventSource with no token),
//     within reminderFor: its refusals by reminderFailure.
//
// Each request logs one line, as a send does, but never the address, the
// zone or the bearer.
type reminders struct {
	backend backendAPI
	gate    *gate
	logger  *slog.Logger
	now     func() time.Time
}

// route is the handler of one of the two routes.
func (rm *reminders) route(which reminderRoute) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(requestID) {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", requestID)
		out, fail := rm.serve(w, r, which)
		code, status := codeOK, http.StatusOK
		if fail != nil {
			code, status = fail.code, fail.status
			fail.write(w)
		} else {
			writeJSON(w, http.StatusOK, out)
		}
		reminderRequestsTotal.WithLabelValues(string(which), code).Inc()
		if fail != nil && fail.quiet {
			return
		}
		attrs := []any{"route", string(which), "status", status, "code", code, "request_id", requestID, "ms", time.Since(start).Milliseconds()}
		level := slog.LevelInfo
		if status >= http.StatusInternalServerError {
			level = slog.LevelWarn
		}
		if fail != nil && fail.cause != nil {
			attrs = append(attrs, "err", fail.cause.Error())
		}
		rm.logger.Log(r.Context(), level, "foundry reminders request", attrs...)
	})
}

// serve checks one request and forwards it to the backend (reminders'
// steps), answering what the route answers.
func (rm *reminders) serve(w http.ResponseWriter, r *http.Request, which reminderRoute) (any, *failure) {
	deadline := http.NewResponseController(w)
	now := rm.now()
	address := clientAddress(r)
	if wait := rm.gate.address(address, now); wait > 0 {
		return nil, limited(wait, "this address sends too many requests; send again after Retry-After")
	}
	if r.ContentLength > maxReminderBody {
		return nil, reminderTooLarge()
	}
	if !jsonBody(r.Header) {
		return nil, &failure{status: http.StatusUnsupportedMediaType, code: codeUnsupportedMediaType,
			message: "send the body as Content-Type: application/json, in UTF-8, with no Content-Encoding"}
	}
	secret, given := bearer(r.Header)
	if !given {
		return nil, &failure{status: http.StatusUnauthorized, code: codeUnauthorized, challenge: "Bearer",
			message: "send one Authorization: Bearer header with the site's reminders secret"}
	}
	release, ok := rm.gate.enter(r.ContentLength, now)
	if !ok {
		return nil, limited(time.Second, "Foundry is taking too many requests at once; send again after Retry-After")
	}
	defer release()

	capped := http.MaxBytesReader(w, r.Body, maxReminderBody)
	var create reminderBody
	var stop stopBody
	var fail *failure
	if which == routeCreate {
		create, fail = readReminderBody[reminderBody](capped)
	} else {
		stop, fail = readReminderBody[stopBody](capped)
	}
	if fail != nil {
		// What is left of the body is no request to read next.
		w.Header().Set("Connection", "close")
		return nil, fail
	}
	_ = deadline.SetReadDeadline(time.Time{})

	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])
	ctx, cancel := context.WithTimeout(foundryapi.AttachActor(r.Context(), ports.Actor{Source: ports.SourceEventSource}), reminderFor)
	defer cancel()
	if which == routeStop {
		if strings.TrimSpace(stop.Email) == "" {
			return nil, invalid("email is required: the address of the person who signed in")
		}
		if _, err := rm.backend.StopFoundryReminders(ctx, &foundryv1.StopFoundryRemindersRequest{SecretSha256: hash, Email: stop.Email}); err != nil {
			return nil, reminderFailure(err)
		}
		return struct{}{}, nil
	}
	switch {
	case strings.TrimSpace(create.Email) == "":
		return nil, invalid("email is required: the address to remind")
	case create.Consent == "":
		return nil, invalid("consent is required: the version of the words the person agreed to, such as remind-v1")
	}
	resp, err := rm.backend.CreateFoundryReminder(ctx, &foundryv1.CreateFoundryReminderRequest{
		SecretSha256: hash, Email: create.Email, TimeZone: create.TimeZone, Consent: create.Consent,
	})
	if err != nil {
		return nil, reminderFailure(err)
	}
	return reminderAnswer{NextSendAt: resp.GetNextSendAt().AsTime().UTC().Format(time.RFC3339), Again: resp.GetAgain()}, nil
}

// reminderFailure answers the backend's refusal of a reminder request by its
// status:
//   - INVALID_ARGUMENT: 400 invalid_arguments, with the backend's message,
//     which quotes no address;
//   - UNAUTHENTICATED and PERMISSION_DENIED: 401 unauthorized, the bearer
//     not the site's secret;
//   - FAILED_PRECONDITION: 404 not_found, reminders off;
//   - RESOURCE_EXHAUSTED: 429 rate_limited, Retry-After from its RetryInfo,
//     or reminderLimitedFor;
//   - UNAVAILABLE, DEADLINE_EXCEEDED and CANCELED: 503 backend_unavailable,
//     Retry-After 1;
//   - anything else: 500 internal.
func reminderFailure(err error) *failure {
	st := status.Convert(err)
	switch st.Code() {
	case codes.InvalidArgument:
		return invalid(st.Message())
	case codes.Unauthenticated, codes.PermissionDenied:
		return &failure{status: http.StatusUnauthorized, code: codeUnauthorized, challenge: `Bearer error="invalid_token"`,
			message: "the bearer is not the site's reminders secret"}
	case codes.FailedPrecondition:
		return &failure{status: http.StatusNotFound, code: codeNotFound, message: "Foundry takes no reminders here"}
	case codes.ResourceExhausted:
		f := &failure{status: http.StatusTooManyRequests, code: codeRateLimited, message: st.Message(), retryAfter: reminderLimitedFor}
		for _, detail := range st.Details() {
			if info, ok := detail.(*errdetails.RetryInfo); ok && info.GetRetryDelay().AsDuration() > 0 {
				f.retryAfter = info.GetRetryDelay().AsDuration()
			}
		}
		return f
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return &failure{status: http.StatusServiceUnavailable, code: codeBackendUnavailable, retryAfter: time.Second, cause: err,
			message: "Foundry can't take reminders just now; send it again after Retry-After"}
	}
	return &failure{status: http.StatusInternalServerError, code: codeInternal, cause: err,
		message: "Foundry failed to take the request; send it again"}
}

// readReminderBody decodes the one JSON object a reminder request's body
// holds, from a reader the caller has capped. Nothing but space may follow
// it, and it may hold no field T doesn't.
func readReminderBody[T any](r io.Reader) (T, *failure) {
	var b T
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return b, reminderBodyFailure(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return b, invalid("the body holds more than one JSON value")
		}
		return b, reminderBodyFailure(err)
	}
	return b, nil
}

// reminderBodyFailure answers a body that couldn't be read or decoded: 413
// past the cap, 408 when it took longer than its deadline, and 400 for
// anything else. encoding/json's messages name fields and types, never a
// value.
func reminderBodyFailure(err error) *failure {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return reminderTooLarge()
	}
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return &failure{status: http.StatusRequestTimeout, code: codeRequestTimeout, message: "the body took too long to arrive; send it again"}
	case errors.Is(err, io.EOF):
		return invalid(`the body is empty; it must be one JSON object, such as {"email": "…", "time_zone": "…", "consent": "remind-v1"}`)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return invalid("the body ended before its JSON did")
	}
	return invalid("the body is not a reminder request: " + strings.TrimPrefix(err.Error(), "json: "))
}

// reminderTooLarge is a 413 for a body past maxReminderBody.
func reminderTooLarge() *failure {
	return &failure{status: http.StatusRequestEntityTooLarge, code: codePayloadTooLarge,
		message: fmt.Sprintf("a reminder request's body holds at most %d bytes", maxReminderBody)}
}
