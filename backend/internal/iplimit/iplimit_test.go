package iplimit

import (
	"fmt"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// The map is sized by attacker-controlled input, so its bounds are a security
// property rather than a tuning detail.
func TestIPRateLimiter(t *testing.T) {
	// Unbounded, this trades a request-rate DoS for a memory one.
	t.Run("enforces the capacity bound", func(t *testing.T) {
		l := New(rate.Limit(1), 1, 3)

		for i := range 10 {
			l.Allow(fmt.Sprintf("10.0.0.%d", i))
		}

		if l.order.Len() != 3 || len(l.entries) != 3 {
			t.Fatalf("capacity not enforced: list=%d map=%d", l.order.Len(), len(l.entries))
		}
	})

	// Eviction must not become a permanent ban.
	t.Run("gives an evicted address a fresh bucket", func(t *testing.T) {
		l := New(rate.Limit(1), 1, 3)
		for i := range 10 {
			l.Allow(fmt.Sprintf("10.0.0.%d", i))
		}

		if !l.Allow("10.0.0.0") {
			t.Fatal("evicted address should re-enter with a fresh bucket")
		}
	})

	// The burst has to be spent before the address is rejected.
	t.Run("spends one address's burst before rejecting it", func(t *testing.T) {
		l := New(rate.Limit(1), 1, 10)

		if !l.Allow("10.0.0.1") {
			t.Fatal("first request from an address must be admitted")
		}
		if l.Allow("10.0.0.1") {
			t.Fatal("burst of 1 must reject the second request")
		}
	})

	// Otherwise one noisy client denies service to everyone.
	t.Run("keeps buckets independent across addresses", func(t *testing.T) {
		l := New(rate.Limit(1), 1, 10)
		l.Allow("10.0.0.1")
		l.Allow("10.0.0.1")

		if !l.Allow("10.0.0.2") {
			t.Fatal("a different address has its own bucket")
		}
	})

	// A limit that counts only failures reserves before the outcome is
	// known, so requests in flight at once hold the bucket: however many
	// arrive together, no more than its burst get past it, and one whose
	// outcome doesn't count gives its request back.
	t.Run("reserves ahead of an outcome, never more than the burst in flight", func(t *testing.T) {
		l := New(rate.Limit(1e-9), 2, 10)
		now := time.Now()

		first, second := l.Reserve("10.0.0.1", now), l.Reserve("10.0.0.1", now)
		if first.DelayFrom(now) > 0 || second.DelayFrom(now) > 0 {
			t.Fatal("a burst of 2 admits two in flight")
		}
		for range 3 {
			third := l.Reserve("10.0.0.1", now)
			if third.DelayFrom(now) == 0 {
				t.Fatal("a third in flight passed a burst of 2")
			}
			third.CancelAt(now)
		}
		first.CancelAt(now)
		if again := l.Reserve("10.0.0.1", now); again.DelayFrom(now) > 0 {
			t.Fatal("a reservation given back makes room for the next")
		}
		if other := l.Reserve("10.0.0.2", now); other.DelayFrom(now) > 0 {
			t.Fatal("another address has its own bucket")
		}
	})

	// A door refuses early what a reservation would refuse, so the look
	// must agree with Reserve and spend nothing: not a request, not a map
	// entry, not the address's place in the LRU order.
	t.Run("tells the wait a reservation would report, taking nothing", func(t *testing.T) {
		const ip = "10.0.0.1"
		l := New(rate.Every(6*time.Second), 2, 2)
		now := time.Unix(1_700_000_000, 0)
		if d := l.Delay(ip, now); d != 0 || len(l.entries) != 0 {
			t.Fatalf("an unseen address waits %v and is held (%d entries): its bucket is full", d, len(l.entries))
		}
		// A look before each reservation: one that took a request would
		// leave the reservation a longer wait than the look told.
		for range 2 {
			if look, reserved := l.Delay(ip, now), l.Reserve(ip, now); look != 0 || reserved.DelayFrom(now) != 0 {
				t.Fatalf("a burst of 2 waits %v, and its reservation %v", look, reserved.DelayFrom(now))
			}
		}
		for _, after := range []time.Duration{0, 0, time.Second, 5999 * time.Millisecond} {
			at := now.Add(after)
			look := l.Delay(ip, at)
			reserved := l.Reserve(ip, at)
			if wait := reserved.DelayFrom(at); look != wait || wait == 0 {
				t.Fatalf("%v in, the look waits %v and a reservation %v", after, look, wait)
			}
			reserved.CancelAt(at)
		}
		if d := l.Delay(ip, now.Add(7*time.Second)); d != 0 {
			t.Fatalf("a request comes back every 6s, yet 7s in it waits %v", d)
		}
		// ip is the oldest once 10.0.0.2 is seen; a look leaves it so, and
		// 10.0.0.3 evicts it.
		l.Allow("10.0.0.2")
		l.Delay(ip, now)
		l.Allow("10.0.0.3")
		if _, ok := l.entries[ip]; ok {
			t.Fatal("a look moved the address in the LRU order")
		}
	})
}
