package foundryevents

import (
	"context"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryapi "github.com/helixdb/helix-foundry/backend/api"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/events"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sendtoken"
)

// backendAPI is what the door calls on Foundry's API (FoundryEventsService), as an event
// source's sender (ports.SourceEventSource).
type backendAPI interface {
	ValidateFoundryEventSourceToken(context.Context, *foundryv1.ValidateFoundryEventSourceTokenRequest, ...grpc.CallOption) (
		*foundryv1.ValidateFoundryEventSourceTokenResponse, error)
	SendFoundryEvents(context.Context, *foundryv1.SendFoundryEventsRequest, ...grpc.CallOption) (*foundryv1.SendFoundryEventsResponse, error)
}

// handler serves POST /v1/sources/{source}/pieces. It checks a request in this order, cheapest
// first, and answers the first check it fails:
//  1. The client address's bucket (gate.address): 429 rate_limited.
//  2. The headers: a Content-Length past max_body_bytes is 413 payload_too_large; a type other
//     than application/json (with no charset, or utf-8), or any Content-Encoding but identity,
//     is 415 unsupported_media_type.
//  3. What needs no lookup (sendtoken.Admission.Prescreen): a request without exactly one
//     Authorization: Bearer header is 401 unauthorized, and one whose token is malformed 401
//     invalid_token, each spending one of the address's failed tokens; either is 429
//     rate_limited when the address has none left, as is a token not found live lately from
//     such an address. None of these takes the pod's room: a flood of them costs it nothing.
//  4. The room, held until the request is answered: one of the address's places in flight
//     (gate.addressPlace, address_max_in_flight), so one address, however many tokens it holds
//     and however slowly its bodies arrive, never holds all of the pod's room; then the pod's
//     places and bytes in flight and its bucket (gate.enter), the bucket last, so a full pod
//     spends none of its rate. Either is 429 rate_limited, Retry-After 1.
//  5. The send token (sendtoken.Admission, which caches one found live for validated_for): 401
//     invalid_token, 429 rate_limited, or 503 backend_unavailable when the lookup can't answer.
//  6. The path's source is the token's: else 404 not_found.
//  7. The body, within max_body_bytes and readFor of the request's start: 413, 408
//     request_timeout, or 400 invalid_arguments for one that isn't a send (an unknown field,
//     data after it). The connection is closed after any of them.
//  8. client_token, required, then the backend's stateless rules (events.Validate): 400
//     invalid_arguments, with the refused piece's 1-based place in piece.
//  9. SendFoundryEvents within sendFor, as the token, with each piece's data compacted: its
//     answer mapped by backendFailure. PERMISSION_DENIED tells the admission the token is revoked.
//
// Each request but those refused for a flood (rate_limited) logs one line: its status and code,
// its source and token id once admitted, its pieces once read, and how long it took; never the
// client's address, the token or any of the pieces' content.
type handler struct {
	backend   backendAPI
	gate      *gate
	admission *sendtoken.Admission
	limits    events.Limits
	maxBody   int64
	readFor   time.Duration
	sendFor   time.Duration
	logger    *slog.Logger
	// now is the clock of the buckets and the token cache.
	now func() time.Time
}

// newHandler is the handler of foundry.events, with its gate, its send-token admission and the
// piece limits.
func newHandler(backend backendAPI, cfg config.EventsConfig, logger *slog.Logger, now func() time.Time) *handler {
	lookup := func(ctx context.Context, hash string) (sendtoken.Sender, bool, error) {
		ctx, cancel := context.WithTimeout(foundryapi.AttachActor(ctx, ports.Actor{Source: ports.SourceEventSource}), lookupFor)
		defer cancel()
		found, err := backend.ValidateFoundryEventSourceToken(ctx, &foundryv1.ValidateFoundryEventSourceTokenRequest{Hash: hash})
		return sendtoken.Sender{TokenID: found.GetTokenId(), WorkspaceID: found.GetWorkspaceId(), ConnectorID: found.GetConnectorId()},
			found.GetValid(), err
	}
	return &handler{
		backend: backend, gate: newGate(cfg.Endpoint), admission: sendtoken.New(lookup, cfg.Endpoint.ValidatedFor),
		limits:  events.Limits{MaxPieces: cfg.MaxPiecesPerSend, MaxBytes: cfg.MaxSendBytes, MaxPieceBytes: cfg.MaxPieceBytes},
		maxBody: cfg.Endpoint.MaxBodyBytes, readFor: readFor, sendFor: sendFor, logger: logger, now: now,
	}
}

// routes is the role's one route on a mux of its own, which answers another method on it 405,
// and another path 404. Every request's connection gets readFor from the request's start to
// read its body, the mux's refusals' too: the server reads what a handler leaves of a short body
// before it answers and again before it reuses the connection, with no deadline of its own once
// the headers are in, so a slow body sent with a 404 or 405 held its connection for as long as
// it trickled, outside the gate's bounds. serve clears the deadline once a send's body is in.
func (h *handler) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/sources/{source}/pieces", h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(h.readFor)) // net/http's server supports it; a test's recorder doesn't
		mux.ServeHTTP(w, r)
	})
}

// seen is what a request was found to be as it was checked, for its log line.
type seen struct {
	sender sendtoken.Sender
	pieces int
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := r.Header.Get("X-Request-ID")
	if !requestIDPattern.MatchString(requestID) {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-ID", requestID)
	var request seen
	out, fail := h.serve(w, r, &request)
	code, status := codeOK, http.StatusOK
	if fail != nil {
		code, status = fail.code, fail.status
		fail.write(w)
	} else {
		writeJSON(w, http.StatusOK, out)
	}
	requestsTotal.WithLabelValues(code).Inc()
	if fail != nil && fail.quiet {
		return
	}
	attrs := []any{"status", status, "code", code, "request_id", requestID, "ms", time.Since(start).Milliseconds()}
	if request.sender.TokenID != "" {
		attrs = append(attrs, "source", request.sender.ConnectorID, "token_id", request.sender.TokenID)
	}
	if request.pieces > 0 {
		attrs = append(attrs, "pieces", request.pieces)
	}
	level := slog.LevelInfo
	if status >= http.StatusInternalServerError {
		level = slog.LevelWarn
	}
	if fail != nil && fail.cause != nil {
		attrs = append(attrs, "err", fail.cause.Error())
	}
	h.logger.Log(r.Context(), level, "foundry events request", attrs...)
}

// serve checks and forwards one request (handler's steps), noting in request what it learns.
func (h *handler) serve(w http.ResponseWriter, r *http.Request, request *seen) (Sent, *failure) {
	// The connection's read deadline (routes) bounds the body, and whatever of it the server reads
	// past a refusal to keep the connection. It is cleared once the body is in, or the server would
	// end the request's context when the connection's read next timed out, the send in flight.
	deadline := http.NewResponseController(w)
	now := h.now()
	address := clientAddress(r)
	if wait := h.gate.address(address, now); wait > 0 {
		return Sent{}, limited(wait, "this address sends too many requests; send again after Retry-After")
	}
	if r.ContentLength > h.maxBody {
		return Sent{}, tooLarge(h.maxBody)
	}
	if !jsonBody(r.Header) {
		return Sent{}, &failure{status: http.StatusUnsupportedMediaType, code: codeUnsupportedMediaType,
			message: "send the body as Content-Type: application/json, in UTF-8, with no Content-Encoding"}
	}
	// A refusal that needs no lookup takes none of the pod's room; the rest are looked up or
	// admitted in it.
	token, given := bearer(r.Header)
	admitted, decided := h.admission.Prescreen(address, token, now)
	if !decided {
		leave, ok := h.gate.addressPlace(address)
		if !ok {
			return Sent{}, limited(time.Second, "this address has too many sends in flight; send again after Retry-After")
		}
		defer leave()
		release, ok := h.gate.enter(r.ContentLength, now)
		if !ok {
			return Sent{}, limited(time.Second, "Foundry is taking too many sends at once; send again after Retry-After")
		}
		defer release()
		admitted = h.admission.Admit(r.Context(), address, token, now)
	}
	switch admitted.Verdict {
	case sendtoken.Admitted:
	case sendtoken.Limited:
		return Sent{}, limited(admitted.RetryAfter, "this address has sent too many requests without a live send token; send again after Retry-After")
	case sendtoken.Unavailable:
		return Sent{}, unavailable(nil)
	default:
		if !given {
			return Sent{}, &failure{status: http.StatusUnauthorized, code: codeUnauthorized, challenge: "Bearer",
				message: "send one Authorization: Bearer header with a send token (hx_ev_) of this API source"}
		}
		return Sent{}, &failure{status: http.StatusUnauthorized, code: codeInvalidToken, challenge: `Bearer error="invalid_token"`,
			message: "the send token is malformed, unknown or revoked"}
	}
	sender := admitted.Sender
	request.sender = sender
	if r.PathValue("source") != sender.ConnectorID {
		return Sent{}, &failure{status: http.StatusNotFound, code: codeNotFound, message: "this send token sends to another API source"}
	}

	b, fail := readBody(http.MaxBytesReader(w, r.Body, h.maxBody))
	if fail != nil {
		// What is left of the body is no request to read next.
		w.Header().Set("Connection", "close")
		return Sent{}, fail
	}
	_ = deadline.SetReadDeadline(time.Time{})
	request.pieces = len(b.Pieces)
	req, fail := sendRequest(b, sender.WorkspaceID, sender.ConnectorID, h.limits)
	if fail != nil {
		return Sent{}, fail
	}

	caller := ports.Actor{Source: ports.SourceEventSource, EventSourceTokenID: sender.TokenID, WorkspaceID: sender.WorkspaceID}
	ctx, cancel := context.WithTimeout(foundryapi.AttachActor(r.Context(), caller), h.sendFor)
	defer cancel()
	resp, err := h.backend.SendFoundryEvents(ctx, req)
	if status.Code(err) == codes.PermissionDenied {
		h.admission.Revoked(address, token, h.now())
	}
	if err != nil {
		return Sent{}, backendFailure(err)
	}
	return sent(sender.ConnectorID, resp), nil
}

// backendFailure answers the backend's refusal of a send by its status:
//   - INVALID_ARGUMENT: 400 invalid_arguments, with the piece its BadRequest field violation
//     pieces[i] (from 0) names, as piece i+1;
//   - NOT_FOUND: 404 not_found;
//   - PERMISSION_DENIED: 401 invalid_token, the token revoked (or its source or workspace
//     deleted) since it was found live;
//   - FAILED_PRECONDITION: 409 failed_precondition, with its ErrorInfo reason (the source full,
//     paused, or at its field cap);
//   - RESOURCE_EXHAUSTED: 429 resource_exhausted, Retry-After from its RetryInfo, or 1 s;
//   - UNAVAILABLE, DEADLINE_EXCEEDED and CANCELED: 503 backend_unavailable, Retry-After 1;
//   - anything else: 500 internal.
//
// The messages of 400, 409 and 429 are the backend's, written for the sender; a 5xx says only to
// send again, and logs the backend's.
func backendFailure(err error) *failure {
	st := status.Convert(err)
	switch st.Code() {
	case codes.InvalidArgument:
		f := invalid(st.Message())
		for _, detail := range st.Details() {
			violations, ok := detail.(*errdetails.BadRequest)
			if !ok {
				continue
			}
			for _, v := range violations.GetFieldViolations() {
				text, isPiece := strings.CutPrefix(v.GetField(), "pieces[")
				text, closed := strings.CutSuffix(text, "]")
				if i, err := strconv.Atoi(text); isPiece && closed && err == nil && i >= 0 {
					f.piece = i + 1
				}
			}
		}
		return f
	case codes.NotFound:
		return &failure{status: http.StatusNotFound, code: codeNotFound, message: "this API source no longer exists"}
	case codes.PermissionDenied:
		return &failure{status: http.StatusUnauthorized, code: codeInvalidToken, challenge: `Bearer error="invalid_token"`, message: st.Message()}
	case codes.FailedPrecondition:
		f := &failure{status: http.StatusConflict, code: codeFailedPrecondition, message: st.Message()}
		for _, detail := range st.Details() {
			if info, ok := detail.(*errdetails.ErrorInfo); ok {
				f.reason = info.GetReason()
			}
		}
		return f
	case codes.ResourceExhausted:
		f := &failure{status: http.StatusTooManyRequests, code: codeResourceExhausted, message: st.Message(), retryAfter: time.Second}
		for _, detail := range st.Details() {
			if info, ok := detail.(*errdetails.RetryInfo); ok && info.GetRetryDelay().AsDuration() > 0 {
				f.retryAfter = info.GetRetryDelay().AsDuration()
			}
		}
		return f
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return unavailable(err)
	}
	return &failure{status: http.StatusInternalServerError, code: codeInternal, cause: err,
		message: "Foundry failed to take the send; send it again with the same client_token"}
}

// unavailable is a 503 for a backend that couldn't answer, cause logged.
func unavailable(cause error) *failure {
	return &failure{status: http.StatusServiceUnavailable, code: codeBackendUnavailable, retryAfter: time.Second, cause: cause,
		message: "Foundry can't take sends just now; send it again with the same client_token after Retry-After"}
}

// bearer is the token of a request's one Authorization header of scheme Bearer, and whether
// there was one: a request with none, two, or another scheme bears no token, "".
func bearer(header http.Header) (string, bool) {
	values := header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, _ := strings.Cut(values[0], " ")
	token = strings.TrimSpace(token)
	if !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

// jsonBody reports whether a request's body is declared JSON in UTF-8 and sent as is: the media
// type application/json in any case, with no charset or utf-8, and no Content-Encoding but
// identity.
func jsonBody(header http.Header) bool {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	charset, hasCharset := params["charset"]
	encodings := header.Values("Content-Encoding")
	return err == nil && mediaType == "application/json" && (!hasCharset || strings.EqualFold(charset, "utf-8")) &&
		(len(encodings) == 0 || len(encodings) == 1 && strings.EqualFold(strings.TrimSpace(encodings[0]), "identity"))
}

// clientAddress is the address the load balancer saw, for the per-address buckets: the last
// X-Forwarded-For hop, since earlier hops are the client's own to write, or the peer's address
// without one. It is never logged or stored.
func clientAddress(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		hops := strings.Split(xff, ",")
		return strings.TrimSpace(hops[len(hops)-1])
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

// requestIDPattern is an X-Request-ID the door echoes; it makes a new one for any other.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
