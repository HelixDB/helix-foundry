package foundryevents

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryapi "github.com/helixdb/helix-foundry/backend/api"
	"github.com/helixdb/helix-foundry/backend/internal/iplimit"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sendtoken"
)

const (
	// sharedFor bounds the backend's answer for a shared dashboard: one
	// indexed read.
	sharedFor = 5 * time.Second
	// missesPerMinute is how many links an address may ask for that show
	// nothing, a minute, before it is refused: a person mistypes one now
	// and then, and a guesser of 256-bit tokens gets nowhere either way.
	missesPerMinute = 10
)

// shareTokenForm is a dashboard link's token: hxd_ and 32 random bytes,
// base64url. Any other is no link, refused before the pod's room or the
// backend.
var shareTokenForm = regexp.MustCompile(`^hxd_[A-Za-z0-9_-]{43}$`)

// shared serves GET /v1/shared-dashboards/{token}: a dashboard's public
// snapshot, which its link's page reads, with no sign-in. A request is
// checked in this order, cheapest first, and answered at the first check it
// fails:
//  1. The client address's bucket, shared with the sends: 429 rate_limited.
//  2. The address's misses (missesPerMinute): with none left, 429
//     rate_limited.
//  3. The token's form (shareTokenForm): 404 not_found, spending a miss.
//  4. The room, held until the request is answered: one of the address's
//     places in flight, then the pod's: 429 rate_limited.
//  5. The backend, as the door (ports.SourceEventSource with no token),
//     within sharedFor, with the token's SHA-256, never the token: NOT_FOUND
//     is 404 not_found, spending a miss; UNAVAILABLE, DEADLINE_EXCEEDED and
//     CANCELED 503 backend_unavailable; anything else 500 internal.
//
// A 200 is the snapshot as the backend keeps it (dashboard.Snapshot). Every
// answer may be read by any page (Access-Control-Allow-Origin: *, with no
// credentials: the token is the only key), is never stored by a cache, so an
// unpublished link shows nothing at once, and is no page for a search engine.
// Each request logs one line, but never the token or the address.
type shared struct {
	backend backendAPI
	gate    *gate
	misses  *iplimit.Limiters
	logger  *slog.Logger
	now     func() time.Time
}

func newShared(backend backendAPI, g *gate, logger *slog.Logger, now func() time.Time) *shared {
	return &shared{backend: backend, gate: g, logger: logger, now: now,
		misses: iplimit.New(rate.Limit(float64(missesPerMinute)/60), missesPerMinute, addressEntries)}
}

func (sh *shared) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := r.Header.Get("X-Request-ID")
	if !requestIDPattern.MatchString(requestID) {
		requestID = uuid.NewString()
	}
	h := w.Header()
	h.Set("X-Request-ID", requestID)
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	snapshot, fail := sh.serve(r)
	code, httpStatus := codeOK, http.StatusOK
	if fail != nil {
		code, httpStatus = fail.code, fail.status
		fail.write(w)
	} else {
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(snapshot)) // best-effort: the client may have gone
	}
	sharedRequestsTotal.WithLabelValues(code).Inc()
	if fail != nil && fail.quiet {
		return
	}
	attrs := []any{"status", httpStatus, "code", code, "request_id", requestID, "ms", time.Since(start).Milliseconds()}
	level := slog.LevelInfo
	if httpStatus >= http.StatusInternalServerError {
		level = slog.LevelWarn
	}
	if fail != nil && fail.cause != nil {
		attrs = append(attrs, "err", fail.cause.Error())
	}
	sh.logger.Log(r.Context(), level, "foundry shared dashboard request", attrs...)
}

// serve checks one request and reads its snapshot (shared's steps).
func (sh *shared) serve(r *http.Request) (string, *failure) {
	now := sh.now()
	address := clientAddress(r)
	if wait := sh.gate.address(address, now); wait > 0 {
		return "", limited(wait, "this address asks too often; ask again after Retry-After")
	}
	if wait := sh.misses.Delay(address, now); wait > 0 {
		return "", limited(wait, "this address asked for too many links that show nothing; ask again after Retry-After")
	}
	token := r.PathValue("token")
	if !shareTokenForm.MatchString(token) {
		return "", sh.miss(address, now)
	}
	leave, ok := sh.gate.addressPlace(address)
	if !ok {
		return "", limited(time.Second, "this address has too many requests in flight; ask again after Retry-After")
	}
	defer leave()
	release, ok := sh.gate.enter(0, now)
	if !ok {
		return "", limited(time.Second, "Foundry is taking too many requests at once; ask again after Retry-After")
	}
	defer release()

	ctx, cancel := context.WithTimeout(foundryapi.AttachActor(r.Context(), ports.Actor{Source: ports.SourceEventSource}), sharedFor)
	defer cancel()
	resp, err := sh.backend.GetSharedFoundryDashboard(ctx, &foundryv1.GetSharedFoundryDashboardRequest{TokenSha256: sendtoken.Hash(token)})
	switch status.Code(err) {
	case codes.OK:
		return resp.GetSnapshotJson(), nil
	case codes.NotFound:
		return "", sh.miss(address, sh.now())
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return "", &failure{status: http.StatusServiceUnavailable, code: codeBackendUnavailable, retryAfter: time.Second, cause: err,
			message: "Foundry can't show this dashboard just now; try again after Retry-After"}
	}
	return "", &failure{status: http.StatusInternalServerError, code: codeInternal, cause: err,
		message: "Foundry failed to show this dashboard; try again"}
}

// miss spends one of address's misses on a link that shows nothing, and
// answers 404.
func (sh *shared) miss(address string, now time.Time) *failure {
	reservation := sh.misses.Reserve(address, now)
	if wait := reservation.DelayFrom(now); wait > 0 {
		reservation.CancelAt(now)
		return limited(wait, "this address asked for too many links that show nothing; ask again after Retry-After")
	}
	return &failure{status: http.StatusNotFound, code: codeNotFound,
		message: "no dashboard is shared at this link: it may have been unpublished"}
}
