package foundryevents

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sendtoken"
)

// shareToken is the i-th well-formed link token.
func shareToken(i int) string { return fmt.Sprintf("hxd_%043d", i) }

const snapshot = `{"v":1,"name":"Revenue","tiles":[]}`

// GetSharedFoundryDashboard answers the snapshot of shareToken(1), sharedErr
// for every request when set, and records each request's hash and Actor.
func (b *fakeBackend) GetSharedFoundryDashboard(ctx context.Context, req *foundryv1.GetSharedFoundryDashboardRequest) (
	*foundryv1.GetSharedFoundryDashboardResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	actor, _ := ports.ActorFrom(ctx)
	b.sharedHashes, b.sharedActors = append(b.sharedHashes, req.GetTokenSha256()), append(b.sharedActors, actor)
	switch {
	case b.sharedErr != nil:
		return nil, b.sharedErr
	case req.GetTokenSha256() != sendtoken.Hash(shareToken(1)):
		return nil, status.Error(codes.NotFound, "no dashboard is shared at this link")
	}
	return &foundryv1.GetSharedFoundryDashboardResponse{SnapshotJson: snapshot}, nil
}

func view(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/shared-dashboards/"+token, nil)
	r.Header.Set("X-Forwarded-For", address)
	return r
}

func TestSharedDashboards(t *testing.T) {
	t.Run("serves a snapshot by its token's hash, readable by any page", func(t *testing.T) {
		d := newDoor(t, nil)
		rec := d.do(view(shareToken(1)))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, snapshot, rec.Body.String())
		for header, want := range map[string]string{"Content-Type": "application/json", "Access-Control-Allow-Origin": "*",
			"Cache-Control": "no-store", "X-Robots-Tag": "noindex, nofollow", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff"} {
			assert.Equal(t, want, rec.Header().Get(header), header)
		}
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"))
		require.Len(t, d.backend.sharedHashes, 1)
		assert.Equal(t, sendtoken.Hash(shareToken(1)), d.backend.sharedHashes[0], "the backend gets the hash, never the token")
		assert.Equal(t, ports.Actor{Source: ports.SourceEventSource}, d.backend.sharedActors[0], "as the door, of no one")
	})

	t.Run("answers what shows nothing with not found", func(t *testing.T) {
		d := newDoor(t, nil)
		rec := d.do(view(shareToken(2)))
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, codeNotFound, errorOf(t, rec).Code)
		assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"), "the page reads a refusal too")
		for _, malformed := range []string{"hx_ev_" + strings.Repeat("0", 64), "hxd_short", "hxd_" + strings.Repeat("!", 43)} {
			assert.Equal(t, http.StatusNotFound, d.do(view(malformed)).Code, malformed)
		}
		assert.Len(t, d.backend.sharedHashes, 1, "a malformed token reaches no backend")
		r := view(shareToken(1))
		r.Method = http.MethodPost
		assert.Equal(t, http.StatusMethodNotAllowed, d.do(r).Code)
	})

	t.Run("refuses an address that keeps missing", func(t *testing.T) {
		d := newDoor(t, nil)
		for i := range missesPerMinute {
			require.Equal(t, http.StatusNotFound, d.do(view(shareToken(100+i))).Code)
		}
		rec := d.do(view(shareToken(1)))
		assert.Equal(t, http.StatusTooManyRequests, rec.Code, "even its link, until its misses come back")
		assert.NotEmpty(t, rec.Header().Get("Retry-After"))
		other := view(shareToken(1))
		other.Header.Set("X-Forwarded-For", elsewhere)
		assert.Equal(t, http.StatusOK, d.do(other).Code, "another address is its own")
		d.clock.advance(time.Minute)
		assert.Equal(t, http.StatusOK, d.do(view(shareToken(1))).Code)
	})

	t.Run("shares the address's bucket", func(t *testing.T) {
		d := newDoor(t, func(c *config.EventsConfig) {
			c.Endpoint.AddressBurstRequests, c.Endpoint.AddressRequestsPerSecond = 1, 1
		})
		assert.Equal(t, http.StatusOK, d.do(view(shareToken(1))).Code)
		assert.Equal(t, http.StatusTooManyRequests, d.do(view(shareToken(1))).Code)
	})

	t.Run("says when the backend can't answer", func(t *testing.T) {
		d := newDoor(t, nil)
		d.backend.sharedErr = status.Error(codes.Unavailable, "down")
		rec := d.do(view(shareToken(1)))
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Equal(t, codeBackendUnavailable, errorOf(t, rec).Code)
		d.backend.sharedErr = status.Error(codes.Internal, "broken")
		rec = d.do(view(shareToken(1)))
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "broken", "the backend's words stay in the log")
	})
}
