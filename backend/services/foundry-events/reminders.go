package foundryevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryapi "github.com/helixdb/helix-foundry/backend/api"
	"github.com/helixdb/helix-foundry/backend/internal/iplimit"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sendtoken"
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
	// secretTrustedFor is how long a pod takes the bearer the backend last
	// accepted for the site's, admitting it without the address's bucket,
	// place or failures: the site calls from a cloud's shared addresses,
	// which others' requests may hold.
	secretTrustedFor = 10 * time.Minute
)

// siteSecretForm is the form of the site's reminders secret: 32 random bytes in
// lowercase hex (openssl rand -hex 32). A bearer of any other form is no
// guess at it, and is refused before the pod's room or the backend.
var siteSecretForm = regexp.MustCompile(`^[0-9a-f]{64}$`)

// reminderRoute is one of the reminders' two routes, by its metric label.
type reminderRoute string

const (
	routeCreate reminderRoute = "create"
	routeStop   reminderRoute = "stop"
)

// reminderBody is a request for reminders as the site sends it. Client is
// the site's key for the visitor, which the backend caps the schedules of.
type reminderBody struct {
	Email    string `json:"email"`
	TimeZone string `json:"time_zone"`
	Consent  string `json:"consent"`
	Client   string `json:"client"`
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
//	POST /v1/reminders       {"email", "time_zone", "consent", "client"} → {"next_send_at", "again"}
//	POST /v1/reminders/stop  {"email"}                                   → {}
//
// each with Authorization: Bearer and the site's secret. The door holds no
// secret: it passes the bearer's SHA-256 on, and the backend compares it with
// foundry.reminders.secret_sha256. The bearer the backend last accepted is
// the site's, for secretTrustedFor (trusted): its requests skip the
// address's checks, since the site calls from addresses a cloud shares. A
// request is checked in this order, cheapest first, and answered at the
// first check it fails:
//  1. Unless trusted, the client address's bucket, shared with the sends:
//     429 rate_limited.
//  2. The headers: a Content-Length past maxReminderBody is 413
//     payload_too_large, a body not declared JSON 415
//     unsupported_media_type, and no Authorization: Bearer, or one not of
//     the secret's form (siteSecretForm), 401 unauthorized, spending one of the
//     address's failures; with none left, it is 429 rate_limited, as is
//     any untrusted bearer then. None of these takes the pod's room.
//  3. The room, held until the request is answered: unless trusted, one of
//     the address's places in flight, as a send takes it, then the pod's:
//     429 rate_limited.
//  4. The body, one JSON object with no other field, within maxReminderBody
//     and readFor: 413, 408 request_timeout or 400 invalid_arguments; an
//     address or consent missing is 400 too.
//  5. The backend, as the door (ports.SourceEventSource with no token),
//     within reminderFor: its refusals by reminderFailure. One saying the
//     bearer is not the secret spends one of the address's failures.
//
// Each request logs one line, as a send does, but never the address, the
// zone, the client or the bearer.
type reminders struct {
	backend backendAPI
	gate    *gate
	secret  *secretAdmission
	logger  *slog.Logger
	now     func() time.Time
}

// secretAdmission is a pod's view of the bearers of the reminders' routes:
// how many each client address failed with lately (sendtoken's
// FailuresPerMinute a minute), and the bearer the backend last accepted, by
// its hash, which is the site's.
type secretAdmission struct {
	failures *iplimit.Limiters
	mu       sync.Mutex
	accepted string
	at       time.Time
}

func newSecretAdmission() *secretAdmission {
	return &secretAdmission{failures: iplimit.New(rate.Limit(float64(sendtoken.FailuresPerMinute)/60), sendtoken.FailuresPerMinute, addressEntries)}
}

// trusted reports whether hash is the bearer's the backend accepted within
// secretTrustedFor of now.
func (a *secretAdmission) trusted(hash string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accepted != "" && a.accepted == hash && now.Sub(a.at) < secretTrustedFor
}

// accept notes that the backend took hash for the site's secret at now.
func (a *secretAdmission) accept(hash string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accepted, a.at = hash, now
}

// fail spends one of address's failures on a bearer that isn't the site's
// secret, forgetting hash if it was trusted (the secret changed). It
// returns how long until address has a failure left, 0 when it had one.
func (a *secretAdmission) fail(address, hash string, now time.Time) time.Duration {
	a.mu.Lock()
	if a.accepted == hash {
		a.accepted = ""
	}
	a.mu.Unlock()
	reservation := a.failures.Reserve(address, now)
	wait := reservation.DelayFrom(now)
	if wait > 0 {
		reservation.CancelAt(now)
	}
	return wait
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
	secret, given := bearer(r.Header)
	hash := sendtoken.Hash(secret)
	trusted := given && rm.secret.trusted(hash, now)
	if !trusted {
		if wait := rm.gate.address(address, now); wait > 0 {
			return nil, limited(wait, "this address sends too many requests; send again after Retry-After")
		}
	}
	if r.ContentLength > maxReminderBody {
		return nil, reminderTooLarge()
	}
	if !jsonBody(r.Header) {
		return nil, &failure{status: http.StatusUnsupportedMediaType, code: codeUnsupportedMediaType,
			message: "send the body as Content-Type: application/json, in UTF-8, with no Content-Encoding"}
	}
	if !given || !siteSecretForm.MatchString(secret) {
		if wait := rm.secret.fail(address, hash, now); wait > 0 {
			return nil, limited(wait, "this address sent too many requests without the site's secret; send again after Retry-After")
		}
		return nil, &failure{status: http.StatusUnauthorized, code: codeUnauthorized, challenge: "Bearer",
			message: "send one Authorization: Bearer header with the site's reminders secret"}
	}
	if !trusted {
		if wait := rm.secret.failures.Delay(address, now); wait > 0 {
			return nil, limited(wait, "this address sent too many requests without the site's secret; send again after Retry-After")
		}
		leave, ok := rm.gate.addressPlace(address)
		if !ok {
			return nil, limited(time.Second, "this address has too many requests in flight; send again after Retry-After")
		}
		defer leave()
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

	ctx, cancel := context.WithTimeout(foundryapi.AttachActor(r.Context(), ports.Actor{Source: ports.SourceEventSource}), reminderFor)
	defer cancel()
	var answer any
	var err error
	if which == routeStop {
		if strings.TrimSpace(stop.Email) == "" {
			return nil, invalid("email is required: the address of the person who signed in")
		}
		_, err = rm.backend.StopFoundryReminders(ctx, &foundryv1.StopFoundryRemindersRequest{SecretSha256: hash, Email: stop.Email})
		answer = struct{}{}
	} else {
		switch {
		case strings.TrimSpace(create.Email) == "":
			return nil, invalid("email is required: the address to remind")
		case create.Consent == "":
			return nil, invalid("consent is required: the version of the words the person agreed to, such as remind-v1")
		}
		var resp *foundryv1.CreateFoundryReminderResponse
		resp, err = rm.backend.CreateFoundryReminder(ctx, &foundryv1.CreateFoundryReminderRequest{
			SecretSha256: hash, Email: create.Email, TimeZone: create.TimeZone, Consent: create.Consent, Client: create.Client,
		})
		answer = reminderAnswer{NextSendAt: resp.GetNextSendAt().AsTime().UTC().Format(time.RFC3339), Again: resp.GetAgain()}
	}
	switch code := status.Code(err); code {
	case codes.OK, codes.InvalidArgument, codes.ResourceExhausted:
		// The backend checks the secret first: any of these took it.
		rm.secret.accept(hash, rm.now())
	case codes.Unauthenticated, codes.PermissionDenied:
		rm.secret.fail(address, hash, rm.now())
	}
	if err != nil {
		return nil, reminderFailure(err)
	}
	return answer, nil
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
