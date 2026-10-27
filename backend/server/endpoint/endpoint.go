// Package endpoint serves one Foundry service: its gRPC server, an optional
// grpc-gateway, and the HTTP listener with health checks and metrics.
package endpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/test/bufconn"
)

type Health interface {
	LivenessHandler() http.HandlerFunc
	ReadinessHandler() http.HandlerFunc
	GRPCServer() *health.Server
}

// ServerInterceptors supplies the unary + stream gRPC interceptor chain a
// service installs on its endpoint, such as its authentication.
type ServerInterceptors interface {
	Unary() grpc.UnaryServerInterceptor
	Stream() grpc.StreamServerInterceptor
}

// GRPC is a service's gRPC listener.
type GRPC struct {
	// Listen is the bind address (e.g. ":50061").
	Listen string
	// MaxRecvMsgMB caps incoming message size.
	MaxRecvMsgMB int
}

// HTTP is a service's HTTP listener for /healthz, /readyz, /metrics and the
// grpc-gateway.
type HTTP struct {
	// Listen is the bind address (e.g. ":8081").
	Listen string
}

// Options configures a new Endpoint. A nil Interceptor installs no auth chain;
// BuildGateway=false skips the grpc-gateway. StatsHandler
// observes transport details such as exact payload size. GatewayErrorHandler
// overrides HTTP error translation for this endpoint only; keep service-specific
// mappings there rather than changing global gRPC status semantics.
type Options struct {
	Name                string
	Logger              *slog.Logger
	Health              Health
	GRPC                GRPC
	HTTP                HTTP
	Interceptor         ServerInterceptors
	BuildGateway        bool
	StatsHandler        stats.Handler
	GatewayErrorHandler runtime.ErrorHandlerFunc
}

// Endpoint owns the gRPC + HTTP servers for one service. Callers
// register their proto + gateway handlers on GRPC and Gateway (when
// BuildGateway was true) before invoking Serve.
type Endpoint struct {
	Name        string
	GRPC        *grpc.Server
	Gateway     *runtime.ServeMux
	GatewayConn *grpc.ClientConn

	logger      *slog.Logger
	grpcLn      net.Listener
	gatewayBuf  *bufconn.Listener
	httpSrv     *http.Server
	httpLn      net.Listener
	httpMux     *http.ServeMux
	hasGateway  bool
	bindGateway bool
}

const bufSize = 1 << 20

// New binds the gRPC and HTTP listeners and constructs the gateway
// loopback (if requested). Callers must register handlers on Endpoint
// fields before Serve is called.
func New(opts Options) (*Endpoint, error) {
	if opts.GRPC.Listen == "" {
		return nil, fmt.Errorf("endpoint %s: grpc.listen is empty", opts.Name)
	}
	if opts.HTTP.Listen == "" {
		return nil, fmt.Errorf("endpoint %s: http.listen is empty", opts.Name)
	}

	var grpcOpts []grpc.ServerOption
	if opts.Interceptor != nil {
		grpcOpts = append(grpcOpts,
			unaryServerChain(opts.Interceptor.Unary()),
			streamServerChain(opts.Interceptor.Stream()),
		)
	} else {
		grpcOpts = append(grpcOpts,
			unaryServerChain(),
			streamServerChain(),
		)
	}
	if opts.GRPC.MaxRecvMsgMB > 0 {
		grpcOpts = append(grpcOpts, grpc.MaxRecvMsgSize(opts.GRPC.MaxRecvMsgMB*1024*1024))
	}
	if opts.StatsHandler != nil {
		grpcOpts = append(grpcOpts, grpc.StatsHandler(opts.StatsHandler))
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	healthpb.RegisterHealthServer(grpcSrv, opts.Health.GRPCServer())

	grpcLn, err := net.Listen("tcp", opts.GRPC.Listen)
	if err != nil {
		return nil, fmt.Errorf("endpoint %s: grpc listen %s: %w", opts.Name, opts.GRPC.Listen, err)
	}

	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/healthz", opts.Health.LivenessHandler())
	httpMux.HandleFunc("/readyz", opts.Health.ReadinessHandler())
	httpMux.Handle("/metrics", promhttp.Handler())

	ep := &Endpoint{
		Name:        opts.Name,
		GRPC:        grpcSrv,
		logger:      opts.Logger,
		grpcLn:      grpcLn,
		httpMux:     httpMux,
		bindGateway: opts.BuildGateway,
	}

	if opts.BuildGateway {
		gatewayBuf := bufconn.Listen(bufSize)
		conn, err := grpc.NewClient("passthrough:///gateway-buf",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return gatewayBuf.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			_ = grpcLn.Close()
			return nil, fmt.Errorf("endpoint %s: gateway bufconn dial: %w", opts.Name, err)
		}
		errorHandler := opts.GatewayErrorHandler
		if errorHandler == nil {
			errorHandler = runtime.DefaultHTTPErrorHandler
		}
		gw := runtime.NewServeMux(
			runtime.WithIncomingHeaderMatcher(headerMatcher),
			runtime.WithErrorHandler(errorHandler),
		)
		httpMux.Handle("/", gw)
		ep.Gateway = gw
		ep.GatewayConn = conn
		ep.gatewayBuf = gatewayBuf
		ep.hasGateway = true
	}

	httpLn, err := net.Listen("tcp", opts.HTTP.Listen)
	if err != nil {
		_ = grpcLn.Close()
		if ep.gatewayBuf != nil {
			_ = ep.gatewayBuf.Close()
		}
		return nil, fmt.Errorf("endpoint %s: http listen %s: %w", opts.Name, opts.HTTP.Listen, err)
	}
	ep.httpLn = httpLn
	ep.httpSrv = &http.Server{
		Handler:           httpMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	return ep, nil
}

// RegisterHTTPHandler installs a service-specific HTTP route on the
// endpoint's mux. Patterns more specific than "/" win against the
// grpc-gateway catch-all, so this is the hook for static pages,
// OAuth landing endpoints, and the like. Must be called before Serve.
func (e *Endpoint) RegisterHTTPHandler(pattern string, handler http.Handler) {
	e.httpMux.Handle(pattern, handler)
}

// Serve starts every listener and blocks until the first one returns.
// Cancelling ctx triggers GracefulStop and returns the cancellation.
func (e *Endpoint) Serve(ctx context.Context) error {
	if e.logger != nil {
		e.logger.Info("endpoint serving",
			"name", e.Name,
			"grpc", e.grpcLn.Addr().String(),
			"http", e.httpLn.Addr().String(),
			"gateway", e.hasGateway,
		)
	}
	errCh := make(chan error, 3)
	go func() { errCh <- e.GRPC.Serve(e.grpcLn) }()
	go func() {
		err := e.httpSrv.Serve(e.httpLn)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()
	if e.hasGateway {
		go func() { errCh <- e.GRPC.Serve(e.gatewayBuf) }()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// GracefulStop drains the gRPC + HTTP servers. Respects ctx's deadline
// for the HTTP shutdown; if exceeded, falls back to a hard close.
func (e *Endpoint) GracefulStop(ctx context.Context) error {
	if e.httpSrv != nil {
		_ = e.httpSrv.Shutdown(ctx)
	}
	if e.GRPC != nil {
		e.GRPC.GracefulStop()
	}
	if e.GatewayConn != nil {
		_ = e.GatewayConn.Close()
	}
	if e.gatewayBuf != nil {
		_ = e.gatewayBuf.Close()
	}
	return nil
}

// headerMatcher controls which HTTP headers the grpc-gateway forwards to
// gRPC handlers as metadata.
func headerMatcher(h string) (string, bool) {
	switch strings.ToLower(h) {
	case "authorization", "x-request-id":
		return strings.ToLower(h), true
	}
	return runtime.DefaultHeaderMatcher(h)
}
