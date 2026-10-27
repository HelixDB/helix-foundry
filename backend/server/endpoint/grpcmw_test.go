package endpoint

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRPCLogAttrsIncludeServerFaultMessages(t *testing.T) {
	start := time.Now()
	internal := rpcLogAttrs("/svc/Method",
		status.Error(codes.Internal, "get latency: context deadline exceeded"), start)
	assert.Equal(t, "get latency: context deadline exceeded", internal[len(internal)-1])
	assert.Equal(t, "err", internal[len(internal)-2])

	unknown := rpcLogAttrs("/svc/Method", errors.New("plain error"), start)
	assert.Equal(t, "plain error", unknown[len(unknown)-1])

	for _, err := range []error{
		nil,
		status.Error(codes.NotFound, "tenant missing"),
		status.Error(codes.Canceled, context.Canceled.Error()),
	} {
		attrs := rpcLogAttrs("/svc/Method", err, start)
		assert.NotContains(t, attrs, "err", "client-side outcomes keep the compact log line")
	}
}
