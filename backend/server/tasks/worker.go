// Package tasks is the scaffolding of Foundry's background workers: the
// health and metrics listener, the stop handshake, and guarded ticks.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
)

// runDuration and lastSuccess record every Guarded run by worker name. A host
// whose own workers record runs under these names, with these help strings
// and labels, shares them: whichever registers first, the other reuses.
var (
	runDuration = registerShared(prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "helix_billing_worker_run_duration_seconds",
		Help:    "Wall-clock duration of billing worker runs.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 16),
	}, []string{"worker", "outcome"}))
	lastSuccess = registerShared(prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "helix_billing_worker_last_success_unixtime",
		Help: "Unix timestamp of the last successful billing worker run.",
	}, []string{"worker"}))
)

// registerShared registers c with the default registry, or returns the
// collector already registered with c's descriptors.
func registerShared[C prometheus.Collector](c C) C {
	err := prometheus.Register(c)
	var already prometheus.AlreadyRegisteredError
	if errors.As(err, &already) {
		return already.ExistingCollector.(C)
	}
	if err != nil {
		panic(err)
	}
	return c
}

// Worker is the shared lifecycle scaffolding for ticker-driven background tasks: the optional
// health/metrics HTTP listener plus the stop/done handshake. Tasks embed it and supply their loop.
type Worker struct {
	http *httpServer
	stop context.CancelFunc
	done chan struct{}
}

// Run starts the HTTP listener (when listen is set) and blocks in run until Stop or ctx cancel.
func (w *Worker) Run(ctx context.Context, name, listen string, health endpoint.Health, logger *slog.Logger, run func(context.Context) error) error {
	h, err := startHTTP(name, listen, health, logger)
	if err != nil {
		return err
	}
	w.http = h
	runCtx, cancel := context.WithCancel(ctx)
	w.stop = cancel
	w.done = make(chan struct{})
	defer close(w.done)
	defer cancel()
	return run(runCtx)
}

func (w *Worker) Stop(ctx context.Context) error {
	w.http.shutdown(ctx)
	if w.stop == nil {
		return nil
	}
	w.stop()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Tick runs fn immediately, then every interval until ctx is done.
func Tick(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	fn(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn(ctx)
		}
	}
}

// Guarded runs one tick body with panic recovery, an optional per-run timeout, and an
// ok/error/panic outcome on the runs counter, so one bad tick never kills the worker.
func Guarded(ctx context.Context, name string, timeout time.Duration, runs *prometheus.CounterVec, logOK bool, fn func(context.Context) error) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			log.From(ctx).Error(name+" panic recovered", "panic", r, "stack", string(debug.Stack()))
			runs.WithLabelValues("panic").Inc()
			runDuration.WithLabelValues(name, "panic").Observe(time.Since(start).Seconds())
		}
	}()
	rctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if err := fn(rctx); err != nil {
		log.From(ctx).Error(name+" failed", "err", err)
		runs.WithLabelValues("error").Inc()
		runDuration.WithLabelValues(name, "error").Observe(time.Since(start).Seconds())
		return
	}
	runs.WithLabelValues("ok").Inc()
	runDuration.WithLabelValues(name, "ok").Observe(time.Since(start).Seconds())
	lastSuccess.WithLabelValues(name).Set(float64(time.Now().Unix()))
	if logOK {
		log.From(ctx).Debug(name+" ok", "duration", time.Since(start).String())
	}
}

type httpServer struct {
	srv *http.Server
	ln  net.Listener
}

// startHTTP serves /healthz, /readyz, and /metrics; an empty listen or nil health disables it.
func startHTTP(name, listen string, health endpoint.Health, logger *slog.Logger) (*httpServer, error) {
	if listen == "" || health == nil {
		return nil, nil
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("%s http listen %s: %w", name, listen, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", health.LivenessHandler())
	mux.HandleFunc("/readyz", health.ReadinessHandler())
	mux.Handle("/metrics", promhttp.Handler())
	h := &httpServer{ln: ln, srv: &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}}
	go func() {
		if err := h.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error(name+" http exited", "err", err)
		}
	}()
	return h, nil
}

func (h *httpServer) shutdown(ctx context.Context) {
	if h == nil {
		return
	}
	_ = h.srv.Shutdown(ctx)
}
