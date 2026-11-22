package foundryevents

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	foundryapi "github.com/helixdb/helix-foundry/backend/api"
	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/keytoken"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sendtoken"
)

const (
	// relayFor bounds the backend's answer to a relay call but a claim: one
	// call to Slack (10 s at most) and the reads around it.
	relayFor = 15 * time.Second
	// relayClaimSlack is how much longer than its wait a claim may take: its
	// acks, its lease and its installs. With the longest wait it stays within
	// sendFor, so a claim in flight as the pod stops is answered.
	relayClaimSlack = 5 * time.Second
)

// codeForbidden is a call the relay refuses for what it names: a method,
// channel or user the install may not reach.
const codeForbidden = "forbidden"

// relayRoute is one of the relay's routes: its label in the metric and
// the log.
type relayRoute string

const (
	relayToken   relayRoute = "token"
	relayClaim   relayRoute = "claim"
	relaySlack   relayRoute = "slack"
	relayInstall relayRoute = "slack_install"
	relayRevoke  relayRoute = "revoke"
)

// relayBearer is what a live relay token acts as: its link, of its Helix
// workspace.
type relayBearer struct{ LinkID, WorkspaceID string }

// Complete reports that the link and its workspace are named.
func (b relayBearer) Complete() bool { return b.LinkID != "" && b.WorkspaceID != "" }

// relayDoor serves Helix's Slack relay to linked standalone installs while
// foundry.slack and its relay are on (relay.go of the API is the other
// side). Its routes take JSON and answer JSON, field names as the protos
// name them:
//
//	POST /v1/relay/token          {"code", "code_verifier"}: RedeemFoundryRelayCode's answer
//	POST /v1/relay/claim          ClaimFoundryRelayDeliveriesRequest's acks, wait_ms, max, feeds: its answer
//	POST /v1/relay/slack/{method} {"local_workspace_id", "args": {…}}: Slack's answer, as it is
//	POST /v1/relay/slack-install  {"local_workspace_id", "return_to"}: {"authorize_url"}
//	POST /v1/relay/revoke         (no body): {}
//
// Every route but token bears Authorization: Bearer hx_rl_…; the door
// passes its SHA-256, never the token, and the client's address to a claim.
// A request is checked in this order, cheapest first, and answered at the
// first check it fails:
//  1. The address's bucket of the relay's gate (endpoint.relay): 429
//     rate_limited.
//  2. Content-Length past config.MaxSlackRelayCallBytes: 413; a body that
//     isn't JSON: 415.
//  3. The token's prescreen (sendtoken.Admission over keytoken.ValidateRelay;
//     none for token): 401 or 429, spending a failure as a send's does.
//  4. The room: a claim takes one of the pod's relay_max_waiting places and
//     its link's one waiting claim on the pod (429 rate_limited), any other
//     request the address's place and the pod's of the relay's gate.
//  5. The token's admission: 401 invalid_token, 429, or 503.
//  6. The body, within the cap and readFor: 413, 408 or 400.
//  7. The backend, as the door (ports.SourceEventSource with no token),
//     within relayFor, or a claim's wait and relayClaimSlack: its refusal by
//     code (relayFailure); UNAUTHENTICATED tells the admission the token is
//     revoked.
//
// A call to Slack answers Slack's own body with 200, or 429 with
// Retry-After when it may not go yet. Each request logs one line, its
// route, status and code, never its body, token or address.
type relayDoor struct {
	backend   backendAPI
	gate      *gate
	admission *sendtoken.Admission[relayBearer]
	// waiting are the pod's places for claims, and claims the links with a
	// claim waiting here.
	waiting *semaphore.Weighted
	mu      sync.Mutex
	claims  map[string]bool
	logger  *slog.Logger
	now     func() time.Time
	// answerFor is relayFor, but in tests.
	answerFor time.Duration
}

// newRelayDoor is the relay's door under cfg's relay gate, before backend.
func newRelayDoor(backend backendAPI, cfg config.EventsEndpointConfig, logger *slog.Logger, now func() time.Time) *relayDoor {
	lookup := func(ctx context.Context, hash string) (relayBearer, bool, error) {
		ctx, cancel := context.WithTimeout(foundryapi.AttachActor(ctx, ports.Actor{Source: ports.SourceEventSource}), lookupFor)
		defer cancel()
		found, err := backend.ValidateFoundryRelayToken(ctx, &foundryv1.ValidateFoundryRelayTokenRequest{TokenSha256: hash})
		return relayBearer{LinkID: found.GetLinkId(), WorkspaceID: found.GetWorkspaceId()}, found.GetValid(), err
	}
	return &relayDoor{
		backend: backend, gate: newGate(cfg.Relay, config.MaxSlackRelayCallBytes),
		admission: sendtoken.New(keytoken.ValidateRelay, lookup, cfg.ValidatedFor),
		waiting:   semaphore.NewWeighted(int64(cfg.RelayMaxWaiting)), claims: map[string]bool{},
		logger: logger, now: now, answerFor: relayFor,
	}
}

// routes adds the relay's routes to mux.
func (d *relayDoor) routes(mux *http.ServeMux) {
	mux.Handle("POST /v1/relay/token", d.route(relayToken))
	mux.Handle("POST /v1/relay/claim", d.route(relayClaim))
	mux.Handle("POST /v1/relay/slack/{method}", d.route(relaySlack))
	mux.Handle("POST /v1/relay/slack-install", d.route(relayInstall))
	mux.Handle("POST /v1/relay/revoke", d.route(relayRevoke))
}

// relayAnswer is a 200's body, or a call to Slack's retry.
type relayAnswer struct {
	body       []byte
	retryAfter time.Duration
}

func (d *relayDoor) route(route relayRoute) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(requestID) {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", requestID)
		answer, fail := d.serve(w, r, route)
		code, httpStatus := codeOK, http.StatusOK
		switch {
		case fail != nil:
			code, httpStatus = fail.code, fail.status
			fail.write(w)
		case answer.retryAfter > 0:
			code, httpStatus = codeRateLimited, http.StatusTooManyRequests
			limited(answer.retryAfter, "Slack takes this call later; send it again after Retry-After").write(w)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(answer.body) // best-effort: the install may have gone
		}
		relayRequestsTotal.WithLabelValues(string(route), code).Inc()
		if fail != nil && fail.quiet {
			return
		}
		attrs := []any{"route", string(route), "status", httpStatus, "code", code, "request_id", requestID, "ms", time.Since(start).Milliseconds()}
		level := slog.LevelInfo
		if httpStatus >= http.StatusInternalServerError {
			level = slog.LevelWarn
		}
		if fail != nil && fail.cause != nil {
			attrs = append(attrs, "err", fail.cause.Error())
		}
		d.logger.Log(r.Context(), level, "foundry relay request", attrs...)
	})
}

// serve checks and forwards one request (relayDoor's steps).
func (d *relayDoor) serve(w http.ResponseWriter, r *http.Request, route relayRoute) (relayAnswer, *failure) {
	deadline := http.NewResponseController(w)
	now := d.now()
	address := clientAddress(r)
	if wait := d.gate.address(address, now); wait > 0 {
		return relayAnswer{}, limited(wait, "this address calls the relay too often; send again after Retry-After")
	}
	if r.ContentLength > config.MaxSlackRelayCallBytes {
		return relayAnswer{}, tooLarge(config.MaxSlackRelayCallBytes)
	}
	if route != relayRevoke && !jsonBody(r.Header) {
		return relayAnswer{}, &failure{status: http.StatusUnsupportedMediaType, code: codeUnsupportedMediaType,
			message: "send the body as Content-Type: application/json, in UTF-8, with no Content-Encoding"}
	}
	token, given := bearer(r.Header)
	var link relayBearer
	if route != relayToken {
		screened, decided := d.admission.Prescreen(address, token, now)
		if decided {
			return relayAnswer{}, d.unadmitted(screened, given)
		}
	}
	if route == relayClaim {
		if !d.waiting.TryAcquire(1) {
			return relayAnswer{}, limited(time.Second, "Foundry holds too many claims just now; claim again after Retry-After")
		}
		defer d.waiting.Release(1)
	} else {
		leave, ok := d.gate.addressPlace(address)
		if !ok {
			return relayAnswer{}, limited(time.Second, "this address has too many relay calls in flight; send again after Retry-After")
		}
		defer leave()
		release, ok := d.gate.enter(r.ContentLength, now)
		if !ok {
			return relayAnswer{}, limited(time.Second, "Foundry is taking too many relay calls at once; send again after Retry-After")
		}
		defer release()
	}
	if route != relayToken {
		admitted := d.admission.Admit(r.Context(), address, token, now)
		if admitted.Verdict != sendtoken.Admitted {
			return relayAnswer{}, d.unadmitted(admitted, given)
		}
		link = admitted.Bearer
	}
	if route == relayClaim {
		d.mu.Lock()
		taken := d.claims[link.LinkID]
		d.claims[link.LinkID] = true
		d.mu.Unlock()
		if taken {
			return relayAnswer{}, limited(time.Second, "this install already has a claim waiting; claim again once it is answered")
		}
		defer func() {
			d.mu.Lock()
			delete(d.claims, link.LinkID)
			d.mu.Unlock()
		}()
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, config.MaxSlackRelayCallBytes))
	if err != nil {
		w.Header().Set("Connection", "close")
		return relayAnswer{}, bodyFailure(err)
	}
	_ = deadline.SetReadDeadline(time.Time{})

	hash := sendtoken.Hash(token)
	ctx := foundryapi.AttachActor(r.Context(), ports.Actor{Source: ports.SourceEventSource})
	answerFor := d.answerFor
	var resp proto.Message
	switch route {
	case relayToken:
		req := &foundryv1.RedeemFoundryRelayCodeRequest{}
		if fail := unmarshalRelay(raw, req); fail != nil {
			return relayAnswer{}, fail
		}
		ctx, cancel := context.WithTimeout(ctx, answerFor)
		defer cancel()
		resp, err = d.backend.RedeemFoundryRelayCode(ctx, req)
	case relayClaim:
		req := &foundryv1.ClaimFoundryRelayDeliveriesRequest{}
		if fail := unmarshalRelay(raw, req); fail != nil {
			return relayAnswer{}, fail
		}
		if req.GetWaitMs() < 0 || req.GetWaitMs() > int32(config.MaxSlackRelayClaimWait/time.Millisecond) {
			return relayAnswer{}, invalid("wait_ms is from 0 to 20000")
		}
		req.TokenSha256, req.ClientAddress = hash, address
		ctx, cancel := context.WithTimeout(ctx, time.Duration(req.GetWaitMs())*time.Millisecond+relayClaimSlack)
		defer cancel()
		resp, err = d.backend.ClaimFoundryRelayDeliveries(ctx, req)
	case relaySlack:
		var call struct {
			LocalWorkspaceID string          `json:"local_workspace_id"`
			Args             json.RawMessage `json:"args"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&call); err != nil || dec.More() {
			return relayAnswer{}, invalid(`the body is {"local_workspace_id": "…", "args": {…}}`)
		}
		ctx, cancel := context.WithTimeout(ctx, answerFor)
		defer cancel()
		out, err := d.backend.CallSlackForRelay(ctx, &foundryv1.CallSlackForRelayRequest{
			TokenSha256: hash, LocalWorkspaceId: call.LocalWorkspaceID, Method: r.PathValue("method"), BodyJson: string(call.Args),
		})
		if err != nil {
			return relayAnswer{}, d.refused(address, token, err)
		}
		if s := out.GetRetryAfterS(); s > 0 {
			return relayAnswer{retryAfter: time.Duration(s) * time.Second}, nil
		}
		return relayAnswer{body: []byte(out.GetBodyJson())}, nil
	case relayInstall:
		req := &foundryv1.StartFoundryRelaySlackInstallRequest{}
		if fail := unmarshalRelay(raw, req); fail != nil {
			return relayAnswer{}, fail
		}
		req.TokenSha256 = hash
		ctx, cancel := context.WithTimeout(ctx, answerFor)
		defer cancel()
		resp, err = d.backend.StartFoundryRelaySlackInstall(ctx, req)
	case relayRevoke:
		ctx, cancel := context.WithTimeout(ctx, answerFor)
		defer cancel()
		resp, err = d.backend.RevokeFoundryRelayToken(ctx, &foundryv1.RevokeFoundryRelayTokenRequest{TokenSha256: hash})
	}
	if err != nil {
		return relayAnswer{}, d.refused(address, token, err)
	}
	body, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(resp)
	if err != nil {
		return relayAnswer{}, &failure{status: http.StatusInternalServerError, code: codeInternal, cause: err, message: "Foundry failed to answer"}
	}
	return relayAnswer{body: body}, nil
}

// unadmitted answers a token the admission didn't admit.
func (d *relayDoor) unadmitted(o sendtoken.Outcome[relayBearer], given bool) *failure {
	switch o.Verdict {
	case sendtoken.Limited:
		return limited(o.RetryAfter, "this address has sent too many requests without a live relay token; send again after Retry-After")
	case sendtoken.Unavailable:
		return &failure{status: http.StatusServiceUnavailable, code: codeBackendUnavailable, retryAfter: time.Second,
			message: "Foundry can't check the relay token just now; send again after Retry-After"}
	}
	if !given {
		return &failure{status: http.StatusUnauthorized, code: codeUnauthorized, challenge: "Bearer",
			message: "send one Authorization: Bearer header with the install's relay token (hx_rl_)"}
	}
	return &failure{status: http.StatusUnauthorized, code: codeInvalidToken, challenge: `Bearer error="invalid_token"`,
		message: "the relay token is malformed, unknown or revoked: link the install to Helix again"}
}

// refused answers the backend's refusal of a relay call (relayFailure), and
// tells the admission of a token the backend found revoked.
func (d *relayDoor) refused(address, token string, err error) *failure {
	if status.Code(err) == codes.Unauthenticated && token != "" {
		d.admission.Revoked(address, token, d.now())
	}
	return relayFailure(err)
}

// relayFailure answers the backend's refusal by its code: INVALID_ARGUMENT
// 400, UNAUTHENTICATED 401 invalid_token, PERMISSION_DENIED 403 forbidden,
// NOT_FOUND 404, FAILED_PRECONDITION 409 with its reason,
// RESOURCE_EXHAUSTED 429; UNAVAILABLE, DEADLINE_EXCEEDED and CANCELED 503;
// anything else 500. The messages of the 4xx are the backend's, written for
// the install's owner.
func relayFailure(err error) *failure {
	st := status.Convert(err)
	switch st.Code() {
	case codes.InvalidArgument:
		return invalid(st.Message())
	case codes.Unauthenticated:
		return &failure{status: http.StatusUnauthorized, code: codeInvalidToken, challenge: `Bearer error="invalid_token"`, message: st.Message()}
	case codes.PermissionDenied:
		return &failure{status: http.StatusForbidden, code: codeForbidden, message: st.Message()}
	case codes.NotFound:
		return &failure{status: http.StatusNotFound, code: codeNotFound, message: st.Message()}
	case codes.FailedPrecondition:
		f := &failure{status: http.StatusConflict, code: codeFailedPrecondition, message: st.Message()}
		for _, detail := range st.Details() {
			if info, ok := detail.(*errdetails.ErrorInfo); ok {
				f.reason = info.GetReason()
			}
		}
		return f
	case codes.ResourceExhausted:
		return &failure{status: http.StatusTooManyRequests, code: codeResourceExhausted, message: st.Message(), retryAfter: time.Second}
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return &failure{status: http.StatusServiceUnavailable, code: codeBackendUnavailable, retryAfter: time.Second, cause: err,
			message: "Foundry can't take relay calls just now; send again after Retry-After"}
	}
	return &failure{status: http.StatusInternalServerError, code: codeInternal, cause: err, message: "Foundry failed to take the relay call"}
}

// unmarshalRelay reads a relay body into m, its fields named as the proto
// names them; an empty body is m's zero value.
func unmarshalRelay(raw []byte, m proto.Message) *failure {
	if len(raw) == 0 {
		return nil
	}
	if err := protojson.Unmarshal(raw, m); err != nil {
		return invalid("the body is not this route's: " + err.Error())
	}
	return nil
}
