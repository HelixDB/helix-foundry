package foundryevents

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/config"
)

// testGate is a gate of a pod taking 2 requests a second from each address (burst 2) and 2 of
// them at once, 4 a second (burst 4), 3 at once and 10 bytes of bodies at once, a body at most 4.
func testGate() *gate {
	return newGate(config.EventsEndpointConfig{
		AddressRequestsPerSecond: 2, AddressBurstRequests: 2, AddressMaxInFlight: 2, RequestsPerSecond: 4, BurstRequests: 4,
		MaxInFlight: 3, MaxBytesInFlight: 10, MaxBodyBytes: 4,
	})
}

// Every bound refuses at once, and a refusal holds nothing.
func TestGate(t *testing.T) {
	t.Run("refuses an address past its burst until it refills, charging nothing", func(t *testing.T) {
		g := testGate()
		assert.Zero(t, g.address(address, t0))
		assert.Zero(t, g.address(address, t0))
		wait := g.address(address, t0)
		assert.Equal(t, 500*time.Millisecond, wait)
		assert.Equal(t, wait, g.address(address, t0), "a refusal took nothing")
		assert.Zero(t, g.address(elsewhere, t0), "another address has its own bucket")
		assert.Zero(t, g.address(address, t0.Add(wait)))
		assert.Positive(t, g.address(address, t0.Add(wait)))
	})

	t.Run("holds an address to its places in flight, and forgets one with none", func(t *testing.T) {
		g := testGate()
		first, ok := g.addressPlace(address)
		require.True(t, ok)
		second, ok := g.addressPlace(address)
		require.True(t, ok)
		_, ok = g.addressPlace(address)
		assert.False(t, ok, "a third at once")
		other, ok := g.addressPlace(elsewhere)
		require.True(t, ok, "another address has its own")
		first()
		third, ok := g.addressPlace(address)
		require.True(t, ok, "a place given back is taken again")
		second()
		third()
		assert.Equal(t, map[string]int{elsewhere: 1}, g.held)
		other()
		assert.Empty(t, g.held, "an address with no place taken is forgotten")
	})

	t.Run("refuses past the pod's rate", func(t *testing.T) {
		g := testGate()
		for range 4 {
			leave, ok := g.enter(0, t0)
			require.True(t, ok)
			leave()
		}
		_, ok := g.enter(0, t0)
		assert.False(t, ok)
		leave, ok := g.enter(0, t0.Add(250*time.Millisecond))
		require.True(t, ok)
		leave()
	})

	t.Run("refuses past its places in flight, and frees one when a request leaves", func(t *testing.T) {
		g := testGate()
		var leaves []func()
		for range 3 {
			leave, ok := g.enter(1, t0)
			require.True(t, ok)
			leaves = append(leaves, leave)
		}
		_, ok := g.enter(1, t0)
		assert.False(t, ok)
		leaves[0]()
		leave, ok := g.enter(1, t0.Add(time.Second))
		require.True(t, ok)
		leave()
		leaves[1]()
		leaves[2]()
		require.True(t, g.inFlight.TryAcquire(3))
		require.True(t, g.bytes.TryAcquire(10))
	})

	t.Run("refuses past its bytes in flight, a body of unknown length weighing the cap", func(t *testing.T) {
		g := testGate()
		first, ok := g.enter(-1, t0)
		require.True(t, ok, "4 bytes")
		second, ok := g.enter(4, t0)
		require.True(t, ok, "8 bytes")
		_, ok = g.enter(3, t0)
		assert.False(t, ok, "11 bytes")
		// The refusal gave its place in flight back: a request that fits takes the last one.
		third, ok := g.enter(2, t0)
		require.True(t, ok, "10 bytes")
		_, ok = g.enter(0, t0.Add(time.Second))
		assert.False(t, ok, "no place left")
		first()
		second()
		third()
		require.True(t, g.inFlight.TryAcquire(3))
		require.True(t, g.bytes.TryAcquire(10))
	})

	// The bucket's token can't be given back, so it is taken last: a request refused for a place
	// or for bytes leaves the pod's rate as it was.
	t.Run("spends none of the pod's rate on a request it has no room for", func(t *testing.T) {
		g := testGate()
		var leaves []func()
		for range 3 {
			leave, ok := g.enter(1, t0)
			require.True(t, ok)
			leaves = append(leaves, leave)
		}
		for range 5 {
			_, ok := g.enter(1, t0)
			require.False(t, ok, "no place left")
		}
		leaves[0]()
		for range 5 {
			_, ok := g.enter(9, t0)
			require.False(t, ok, "11 bytes")
		}
		fourth, ok := g.enter(1, t0)
		require.True(t, ok, "the bucket's 4th token is left")
		fourth()
		_, ok = g.enter(1, t0)
		assert.False(t, ok, "and was the last")
		leaves[1]()
		leaves[2]()
		require.True(t, g.inFlight.TryAcquire(3))
		require.True(t, g.bytes.TryAcquire(10))
	})
}
