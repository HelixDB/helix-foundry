package directory_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/local/directory"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestConfirmationsBoundDurableAndSingleUse(t *testing.T) {
	st := needDB(t)
	ctx := asOwner(t, st)
	c := directory.Confirmations{Store: st}
	b := integration.ConfirmationBinding{Operation: "foundry:create_load", Target: "ontology:ws/ontology", Audience: "http://localhost:8081/mcp", PayloadHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	p, err := c.Prepare(ctx, b)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(5*time.Minute), p.ExpiresAt, time.Second)
	b.ID, b.Token = p.ID, p.Token
	for _, change := range []func(*integration.ConfirmationBinding){func(x *integration.ConfirmationBinding) { x.Token = "wrong" }, func(x *integration.ConfirmationBinding) { x.Target = "ontology:ws/other" }, func(x *integration.ConfirmationBinding) { x.Audience = "elsewhere" }, func(x *integration.ConfirmationBinding) { x.PayloadHash = "other" }, func(x *integration.ConfirmationBinding) { x.Operation = "foundry:other" }} {
		other := b
		change(&other)
		require.Equal(t, codes.FailedPrecondition, status.Code(c.Consume(ctx, other)))
	}
	unknown := b
	unknown.ID = "no-such-confirmation"
	require.Equal(t, codes.FailedPrecondition, status.Code(c.Consume(ctx, unknown)))
	otherOwner := ports.WithActor(context.Background(), ports.Actor{UserID: "another-owner"})
	mismatch := c.Consume(otherOwner, b)
	require.Equal(t, codes.FailedPrecondition, status.Code(mismatch))
	require.Contains(t, status.Convert(mismatch).Message(), "prepare it again")
	// A new store/adapter models restarting the server; persistence must still enforce replay protection.
	c = directory.Confirmations{Store: directory.NewStore(testDB)}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			err := c.Consume(ctx, b)
			if err == nil {
				successes.Add(1)
			} else if status.Code(err) != codes.AlreadyExists {
				t.Errorf("consume: %v", err)
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 1, successes.Load())
	require.Equal(t, codes.AlreadyExists, status.Code(c.Consume(ctx, b)))
	p, err = c.Prepare(ctx, b)
	require.NoError(t, err)
	b.ID, b.Token = p.ID, p.Token
	_, err = testDB.Exec(`UPDATE foundry_local_mcp_confirmations SET expires_at=? WHERE id=?`, time.Now().Add(-time.Second), p.ID)
	require.NoError(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(c.Consume(ctx, b)))
	_, err = c.Prepare(context.Background(), b)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}
