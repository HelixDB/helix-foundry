package foundryevents

import (
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/internal/iplimit"
)

// addressEntries bounds the client addresses whose buckets a pod holds, the least recently seen
// dropped first, so address churn can't grow the map without end.
const addressEntries = 50_000

// gate is a pod's admission before it looks at a request's token, so a flood is refused at its
// cheapest. Nothing in it waits: a request past any bound is refused at once.
//   - address: each client address's bucket, charged first, so one address never spends the
//     pod's budget.
//   - addressPlace: one of the address's places in flight, taken before the pod's room and held
//     with it, so one address never holds more than address_max_in_flight of the pod's places
//     and that many bodies' bytes, however slowly its bodies arrive.
//   - enter: the pod's places in flight, the bytes of the bodies in flight, a body of unknown
//     length (chunked) weighing the most a body may be, and the pod's bucket, charged last so a
//     full pod spends none of its rate. The places and bytes are held until the request is
//     answered.
type gate struct {
	addresses *iplimit.Limiters
	requests  *rate.Limiter
	inFlight  *semaphore.Weighted
	bytes     *semaphore.Weighted
	maxBody   int64
	// perAddress is address_max_in_flight, and held each address's places taken: an address
	// with none is dropped, so held has no more addresses than the pod has requests in flight.
	perAddress int
	mu         sync.Mutex
	held       map[string]int
}

func newGate(cfg config.EventsEndpointConfig) *gate {
	return &gate{
		addresses:  iplimit.New(rate.Limit(cfg.AddressRequestsPerSecond), cfg.AddressBurstRequests, addressEntries),
		requests:   rate.NewLimiter(rate.Limit(cfg.RequestsPerSecond), cfg.BurstRequests),
		inFlight:   semaphore.NewWeighted(int64(cfg.MaxInFlight)),
		bytes:      semaphore.NewWeighted(cfg.MaxBytesInFlight),
		maxBody:    cfg.MaxBodyBytes,
		perAddress: cfg.AddressMaxInFlight,
		held:       map[string]int{},
	}
}

// address charges one request to address's bucket at now. It returns 0 when the bucket had room,
// and otherwise how long until it has, having charged nothing.
func (g *gate) address(address string, now time.Time) time.Duration {
	reservation := g.addresses.Reserve(address, now)
	wait := reservation.DelayFrom(now)
	if wait > 0 {
		reservation.CancelAt(now)
	}
	return wait
}

// addressPlace takes one of address's places in flight. It returns its release, to call once,
// or false, taking nothing, when the address has address_max_in_flight already.
func (g *gate) addressPlace(address string) (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held[address] >= g.perAddress {
		return nil, false
	}
	g.held[address]++
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.held[address]--
		if g.held[address] == 0 {
			delete(g.held, address)
		}
	}, true
}

// enter takes the pod's room for a request at now whose body is length bytes, -1 when unknown
// (no larger than the cap: a larger Content-Length is refused before): a place in flight, the
// body's bytes and a token of the pod's bucket, in that order. It returns their release, or
// false, holding and spending nothing, when any is full: the token, which can't be given back,
// is taken only once the rest is held.
func (g *gate) enter(length int64, now time.Time) (func(), bool) {
	if length < 0 {
		length = g.maxBody
	}
	if !g.inFlight.TryAcquire(1) {
		return nil, false
	}
	release := func() {
		g.bytes.Release(length)
		g.inFlight.Release(1)
	}
	if !g.bytes.TryAcquire(length) {
		g.inFlight.Release(1)
		return nil, false
	}
	if !g.requests.AllowN(now, 1) {
		release()
		return nil, false
	}
	return release, true
}
