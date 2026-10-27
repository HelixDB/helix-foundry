package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/health"
)

// probes are the install's health (endpoint.Health): live while the process
// runs, ready once everything has started and while its database answers
// ping.
type probes struct {
	ready  atomic.Bool
	ping   func(context.Context) error
	grpc   *health.Server
	logger *slog.Logger
}

func newProbes(ping func(context.Context) error, logger *slog.Logger) *probes {
	return &probes{ping: ping, grpc: health.NewServer(), logger: logger}
}

func (h *probes) GRPCServer() *health.Server { return h.grpc }

func (h *probes) markReady() { h.ready.Store(true) }

func (h *probes) LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
}

// ReadinessHandler answers with its status code alone: the cause of a
// failure is logged, never written.
func (h *probes) ReadinessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := h.ping(ctx); err != nil {
			h.logger.Warn("readiness check failed", "component", "db", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}
