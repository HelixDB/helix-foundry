package endpoint

import (
	"context"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/pkg/log"
)

// unaryServerChain is every endpoint's unary interceptor chain. The extra
// interceptors (usually auth) run after recovery and logging, so panics and
// timings are captured even when auth fails.
func unaryServerChain(extra ...grpc.UnaryServerInterceptor) grpc.ServerOption {
	chain := []grpc.UnaryServerInterceptor{unaryRecover(), unaryLogger()}
	chain = append(chain, extra...)
	return grpc.ChainUnaryInterceptor(chain...)
}

// streamServerChain mirrors unaryServerChain for streaming RPCs.
func streamServerChain(extra ...grpc.StreamServerInterceptor) grpc.ServerOption {
	chain := []grpc.StreamServerInterceptor{streamRecover(), streamLogger()}
	chain = append(chain, extra...)
	return grpc.ChainStreamInterceptor(chain...)
}

func unaryLogger() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		log.From(ctx).Info("grpc.unary", rpcLogAttrs(info.FullMethod, err, start)...)
		return resp, err
	}
}

func streamLogger() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		log.From(ss.Context()).Info("grpc.stream", rpcLogAttrs(info.FullMethod, err, start)...)
		return err
	}
}

// rpcLogAttrs adds the status message for server faults, which callers
// already receive, so a bare code=Internal line still names its cause.
func rpcLogAttrs(method string, err error, start time.Time) []any {
	code := status.Code(err)
	attrs := []any{
		"method", method,
		"code", code.String(),
		"ms", time.Since(start).Milliseconds(),
	}
	switch code {
	case codes.Internal, codes.Unknown, codes.DataLoss:
		attrs = append(attrs, "err", status.Convert(err).Message())
	}
	return attrs
}

func unaryRecover() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.From(ctx).Error("grpc.panic",
					"method", info.FullMethod,
					"panic", r,
					"stack", string(debug.Stack()))
				err = status.Errorf(codes.Internal, "panic: %v", r)
			}
		}()
		return handler(ctx, req)
	}
}

func streamRecover() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				log.From(ss.Context()).Error("grpc.panic",
					"method", info.FullMethod,
					"panic", r,
					"stack", string(debug.Stack()))
				err = status.Errorf(codes.Internal, "panic: %v", r)
			}
		}()
		return handler(srv, ss)
	}
}
