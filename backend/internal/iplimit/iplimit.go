package iplimit

import (
	"container/list"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiters holds one token bucket per source address so one internet client
// cannot drain the shared admission budget. The LRU capacity bound stops the
// map itself from becoming a memory DoS under attacker-controlled address churn.
type Limiters struct {
	mu       sync.Mutex
	rate     rate.Limit
	burst    int
	capacity int
	entries  map[string]*list.Element
	order    *list.List
}

type entry struct {
	ip      string
	limiter *rate.Limiter
}

func New(r rate.Limit, burst, capacity int) *Limiters {
	return &Limiters{
		rate:     r,
		burst:    burst,
		capacity: capacity,
		entries:  make(map[string]*list.Element),
		order:    list.New(),
	}
}

// Allow charges one request against the address's bucket, evicting the
// least-recently-seen address when the map is full.
func (l *Limiters) Allow(ip string) bool {
	return l.limiter(ip).Allow()
}

// Reserve takes one request from the address's bucket at now, for a limit
// that counts only some outcomes, such as failed authentications. The caller
// refuses the request when the reservation's DelayFrom(now) > 0, cancelling
// it first, and gives it back with CancelAt(now) when the outcome doesn't
// count. Taking it before the outcome is known, rather than looking first
// and charging after, bounds the requests in flight at once to what the
// bucket holds: concurrent requests can't all pass a look taken before any
// was charged.
func (l *Limiters) Reserve(ip string, now time.Time) *rate.Reservation {
	return l.limiter(ip).ReserveN(now, 1)
}

// Delay is how long from now until the address's bucket has a request to
// give, taking none: 0 when it has one now, as an address with no bucket
// does (its first is full). The same wait a Reserve at now would report, for
// a caller that refuses early what a reservation would refuse, without
// spending anything or moving the address in the LRU order.
func (l *Limiters) Delay(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	el, ok := l.entries[ip]
	l.mu.Unlock()
	if !ok {
		return 0
	}
	limiter := el.Value.(*entry).limiter
	missing := 1 - limiter.TokensAt(now)
	if missing <= 0 {
		return 0
	}
	// As rate computes a reservation's delay, so the two agree to the nanosecond.
	return time.Duration((missing / float64(limiter.Limit())) * float64(time.Second))
}

// limiter is the address's bucket, made when missing, evicting the
// least-recently-seen address when the map is full.
func (l *Limiters) limiter(ip string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.entries[ip]
	if ok {
		l.order.MoveToFront(el)
	} else {
		if l.order.Len() >= l.capacity {
			oldest := l.order.Back()
			l.order.Remove(oldest)
			delete(l.entries, oldest.Value.(*entry).ip)
		}
		el = l.order.PushFront(&entry{ip: ip, limiter: rate.NewLimiter(l.rate, l.burst)})
		l.entries[ip] = el
	}
	return el.Value.(*entry).limiter
}
