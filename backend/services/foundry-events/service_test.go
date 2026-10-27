package foundryevents

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/health"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/sendtoken"
)

type testHealth struct{ grpc *health.Server }

func (testHealth) LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
}

func (testHealth) ReadinessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
}

func (h testHealth) GRPCServer() *health.Server { return h.grpc }

func availableAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}

// started is the role serving on free ports in front of backend, and its HTTP origin.
func started(t *testing.T, backend *fakeBackend) (*Service, string, context.CancelFunc, chan error) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Foundry.Events.Endpoint.GRPC.Listen = availableAddress(t)
	cfg.Foundry.Events.Endpoint.HTTP.Listen = availableAddress(t)
	svc, err := New(Deps{Cfg: &cfg, Logger: discard(), Health: testHealth{grpc: health.NewServer()}, Backend: dial(t, backend)})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- svc.Start(ctx) }()
	origin := "http://" + cfg.Foundry.Events.Endpoint.HTTP.Listen
	require.Eventually(t, func() bool {
		resp, err := http.Get(origin + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)
	return svc, origin, cancel, served
}

func send(origin string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, origin+sendPath, strings.NewReader(validBody))
	if err != nil {
		return nil, err
	}
	req.Header = post(validBody).Header
	return http.DefaultClient.Do(req)
}

// The role is one service, built only with its backend client.
func TestService(t *testing.T) {
	t.Run("is named for its role", func(t *testing.T) {
		assert.Equal(t, "foundry-events", (&Service{}).Name())
	})

	t.Run("needs a backend client and its address", func(t *testing.T) {
		cfg := config.Defaults()
		_, err := New(Deps{Cfg: &cfg, Logger: discard(), Health: testHealth{grpc: health.NewServer()}})
		require.ErrorContains(t, err, "foundry.events.endpoint.backend.address")
		cfg.Foundry.Events.Endpoint.Backend.Address = ""
		_, err = New(Deps{Cfg: &cfg, Logger: discard(), Health: testHealth{grpc: health.NewServer()}, Backend: dial(t, &fakeBackend{})})
		require.ErrorContains(t, err, "foundry.events.endpoint.backend.address")
	})

	t.Run("fails on a listener it can't bind", func(t *testing.T) {
		cfg := config.Defaults()
		taken, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = taken.Close() })
		cfg.Foundry.Events.Endpoint.GRPC.Listen = availableAddress(t)
		cfg.Foundry.Events.Endpoint.HTTP.Listen = taken.Addr().String()
		_, err = New(Deps{Cfg: &cfg, Logger: discard(), Health: testHealth{grpc: health.NewServer()}, Backend: dial(t, &fakeBackend{})})
		require.Error(t, err)
	})

	// A rolling deploy stops a pod with sends in flight: each is answered before it exits.
	t.Run("answers a send in flight before it stops", func(t *testing.T) {
		backend := &fakeBackend{live: map[string]sendtoken.Sender{sendtoken.Hash(liveToken): sender1}}
		svc, origin, cancel, served := started(t, backend)

		// Its health and metrics are on its own listener; the load balancer routes only the
		// route to it.
		resp, err := send(origin)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_ = resp.Body.Close()
		metrics, err := http.Get(origin + "/metrics")
		require.NoError(t, err)
		text, _ := io.ReadAll(metrics.Body)
		_ = metrics.Body.Close()
		assert.Contains(t, string(text), `helix_foundry_events_http_requests_total{code="ok"}`)

		entered, release := backend.hold(nil)
		answered := make(chan *http.Response, 1)
		go func() {
			resp, err := send(origin)
			assert.NoError(t, err)
			answered <- resp
		}()
		waitFor(t, entered, 1)

		cancel()
		require.ErrorIs(t, <-served, context.Canceled)
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		stopped := make(chan error, 1)
		go func() { stopped <- svc.Stop(stopCtx) }()
		select {
		case <-stopped:
			t.Fatal("stopped with a send in flight")
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		resp = <-answered
		require.NotNil(t, resp)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		_ = resp.Body.Close()
		require.NoError(t, <-stopped)

		_, err = send(origin)
		assert.Error(t, err, "a stopped pod takes no request")
	})
}
