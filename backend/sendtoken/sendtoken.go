// Package sendtoken admits the bearers of Foundry API sources' send tokens (hx_ev_, connector
// kind events). It is the one check both front doors that take them share: the MCP and the
// foundry-events HTTP door.
//
// A send token is a bearer anyone may guess at, so an Admission counts each address's failed
// tokens, FailuresPerMinute a minute on one pod, and refuses an address with none left before
// any lookup, so the lookup can't be used to guess tokens. Per pod:
//   - A request whose token the pod hasn't found live lately takes one of its address's failures
//     before its lookup, and gives it back once the token proves live. So an address has no more
//     lookups of unknown tokens in flight than it has failures left, however many requests it
//     sends at once.
//   - Requests bearing one such token at once share its lookup, and only the request leading it
//     takes a failure: one guess is one failure, however many requests carry it, and a sender's
//     first requests, sent at once, all pass.
//   - A token found live in the last 10 minutes is no guess: its requests skip the bucket, so a
//     sender may send many at once and many senders may share one address. Without it, an
//     address's 11th request in flight was refused however live its token, and the failures
//     those requests gave back as others reserved after them stayed spent (a rate.Reservation
//     gives back only what no later reservation depends on), so valid traffic alone held an
//     address refused. Each such request is still looked up, unless the Admission caches answers.
//   - A malformed or missing token, and one the lookup doesn't find live (unknown, revoked, or of
//     a deleted source or workspace), spend a failure. A lookup that can't answer spends none: the
//     sender did nothing wrong.
//   - A live token the backend refuses later (Revoked) spends one failure, however many requests
//     saw it refused.
//   - A door that spends room of its own on a request (the HTTP door's pod gate) asks Prescreen
//     first, which refuses with no lookup what Admit would, so a flood of such refusals costs it
//     nothing.
//
// It holds at most 10,000 live tokens and as many addresses' buckets. It stores nothing: a
// restarted pod knows no token live.
package sendtoken

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/helixdb/helix-foundry/backend/internal/iplimit"
	"github.com/helixdb/helix-foundry/backend/pkg/keytoken"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

const (
	// FailuresPerMinute is how many send tokens one address may fail with a minute on one pod, its
	// burst too.
	FailuresPerMinute = 10
	// capacity bounds the addresses tracked, as the sandbox's limiters do, and the live tokens.
	capacity = 10_000
	// liveFor is how long a token skips the failed-auth bucket after a lookup last found it live.
	liveFor = 10 * time.Minute
)

// Sender is whom a live token sends as: the token, and the one API source and its workspace it
// may send to.
type Sender struct {
	TokenID, WorkspaceID, ConnectorID string
}

// Lookup finds a send token by its Hash. It reports false for a token that isn't live (unknown,
// revoked, or of a deleted source or workspace), and an error only when it couldn't answer. A
// Sender missing any of its ids is taken for no token.
type Lookup func(ctx context.Context, hash string) (Sender, bool, error)

// Verdict is what a door does with a request.
type Verdict int

const (
	// Refused: no live send token; a door answers 401. It is the zero Verdict, so an Outcome no
	// one decided admits no one.
	Refused Verdict = iota
	// Admitted: a live send token, sending as Outcome.Sender.
	Admitted
	// Limited: the address has no failure left, so the token wasn't looked up; a door answers 429,
	// to ask again after Outcome.RetryAfter.
	Limited
	// Unavailable: the lookup couldn't answer, or the request went away first; a door answers 503.
	Unavailable
)

// Outcome is an Admission's answer for one request. Sender is set only when Admitted, and
// RetryAfter only when Limited.
type Outcome struct {
	Verdict
	Sender
	RetryAfter time.Duration
}

// Admission admits send tokens on one pod; make one with New and share it among requests.
type Admission struct {
	lookup   Lookup
	cacheFor time.Duration
	failures *iplimit.Limiters
	flights  singleflight.Group

	mu sync.Mutex
	// live are the tokens, by hash, a lookup found live in the last liveFor.
	live map[string]found
}

// found is a token a lookup found live at at, sending as sender.
type found struct {
	sender Sender
	at     time.Time
}

// flight is a cold lookup's outcome for every request that waited on it. address is the
// leader's, for a Limited outcome, and gone says the leader went away before the lookup
// answered: either outcome is the leader's alone.
type flight struct {
	Outcome
	address string
	gone    bool
}

// New makes a pod's admission over lookup. With cacheFor 0 each request bearing a live token is
// looked up, so a revoked token is refused on its next request. With more, a token found live is
// admitted for cacheFor without another lookup: for a door whose backend checks the token again
// on every send, and that calls Revoked when it refuses one. New panics without a lookup, or for
// a cacheFor below 0 or past how long a token stays live (10 minutes).
func New(lookup Lookup, cacheFor time.Duration) *Admission {
	if lookup == nil || cacheFor < 0 || cacheFor > liveFor {
		panic(fmt.Sprintf("sendtoken: New needs a lookup and a cacheFor within [0, %v], got %v", liveFor, cacheFor))
	}
	return &Admission{
		lookup: lookup, cacheFor: cacheFor,
		failures: iplimit.New(rate.Every(time.Minute/FailuresPerMinute), FailuresPerMinute, capacity),
		live:     map[string]found{},
	}
}

// Admit decides a request bearing token from address at now, address being the client's as the
// load balancer saw it. A malformed token is Unauthenticated. A token found live within cacheFor
// is admitted as it was; one found live lately is looked up again, skipping the bucket. Any other
// is looked up once for every request bearing it at the time, by the first of them, which takes
// one of its address's failures first: Limited without a lookup when none is left, given back
// unless the token isn't live. A request that only waited on another's lookup takes its outcome
// and spends nothing, unless the outcome was the other's alone (its own address limited, or it
// went away first): then it goes again.
func (a *Admission) Admit(ctx context.Context, address, token string, now time.Time) Outcome {
	if keytoken.ValidateEventSource(token) != nil {
		return a.Unauthenticated(address, now)
	}
	hash := Hash(token)
	a.mu.Lock()
	known, held := a.live[hash]
	a.mu.Unlock()
	age := now.Sub(known.at)
	switch {
	case held && age >= 0 && age < a.cacheFor:
		return Outcome{Verdict: Admitted, Sender: known.sender}
	case held && age < liveFor:
		sender, live, err := a.find(ctx, hash)
		switch {
		case err != nil:
			return Outcome{Verdict: Unavailable}
		case !live:
			a.Revoked(address, token, now)
			return Outcome{Verdict: Refused}
		}
		a.hold(hash, sender, now)
		return Outcome{Verdict: Admitted, Sender: sender}
	}
	for {
		var led bool
		answer := a.flights.DoChan(hash, func() (any, error) {
			led = true
			return a.lead(ctx, address, hash, now), nil
		})
		select {
		case <-ctx.Done():
			return Outcome{Verdict: Unavailable}
		case result := <-answer:
			// led is the flight's write, which happened before its answer was sent.
			outcome := result.Val.(flight)
			if led || !outcome.gone && (outcome.Verdict != Limited || outcome.address == address) {
				return outcome.Outcome
			}
		}
	}
}

// lead looks a token not found live lately up for every request bearing it at the time, taking
// one of the leading request's address's failures first and keeping it only when the token isn't
// live.
func (a *Admission) lead(ctx context.Context, address, hash string, now time.Time) flight {
	reservation := a.failures.Reserve(address, now)
	if wait := reservation.DelayFrom(now); wait > 0 {
		reservation.CancelAt(now)
		return flight{Outcome: Outcome{Verdict: Limited, RetryAfter: wait}, address: address}
	}
	sender, live, err := a.find(ctx, hash)
	switch {
	case err != nil:
		reservation.CancelAt(now)
		return flight{Outcome: Outcome{Verdict: Unavailable}, gone: ctx.Err() != nil}
	case !live:
		return flight{Outcome: Outcome{Verdict: Refused}}
	}
	reservation.CancelAt(now)
	a.hold(hash, sender, now)
	return flight{Outcome: Outcome{Verdict: Admitted, Sender: sender}}
}

// find is the lookup, taking a sender missing any id for no token: the doors act on all three.
func (a *Admission) find(ctx context.Context, hash string) (Sender, bool, error) {
	sender, live, err := a.lookup(ctx, hash)
	return sender, live && sender.TokenID != "" && sender.WorkspaceID != "" && sender.ConnectorID != "", err
}

// hold marks a token found live at now. Full, it first makes room from the tokens found live
// longer than liveFor ago, one pass over them per new token, and holds no new token when none
// is: that token's requests then take the bucket as an unknown token's do.
func (a *Admission) hold(hash string, sender Sender, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.live[hash]; !ok && len(a.live) >= capacity {
		maps.DeleteFunc(a.live, func(_ string, f found) bool { return now.Sub(f.at) >= liveFor })
		if len(a.live) >= capacity {
			return
		}
	}
	a.live[hash] = found{sender: sender, at: now}
}

// Prescreen decides, with no lookup, a request bearing token from address at now that Admit
// would refuse before any: something that isn't a send token (an empty one for a request with
// none) spends one of address's failures, Refused or Limited as Unauthenticated; a send token not
// found live lately, from an address with no failure left, is Limited, spending nothing. It
// reports false, deciding nothing, for any other request: a door then makes room for it and
// admits it with Admit. So a door that prescreens spends nothing of its own on a request it would
// refuse unlooked. Admit decides on its own all the same, since another request may spend the
// address's last failure, or find the token live, in between; and an address with none left
// looks up no new token, not even by waiting on another address's lookup of it.
func (a *Admission) Prescreen(address, token string, now time.Time) (Outcome, bool) {
	if keytoken.ValidateEventSource(token) != nil {
		return a.Unauthenticated(address, now), true
	}
	a.mu.Lock()
	known, held := a.live[Hash(token)]
	a.mu.Unlock()
	if held && now.Sub(known.at) < liveFor {
		return Outcome{}, false
	}
	if wait := a.failures.Delay(address, now); wait > 0 {
		return Outcome{Verdict: Limited, RetryAfter: wait}, true
	}
	return Outcome{}, false
}

// Unauthenticated spends one of address's failures on a request that bears no send token, or
// something that isn't one: Refused, or Limited once the address has none left.
func (a *Admission) Unauthenticated(address string, now time.Time) Outcome {
	reservation := a.failures.Reserve(address, now)
	if wait := reservation.DelayFrom(now); wait > 0 {
		reservation.CancelAt(now)
		return Outcome{Verdict: Limited, RetryAfter: wait}
	}
	return Outcome{Verdict: Refused}
}

// Revoked tells the admission the backend refused a token it admitted (PERMISSION_DENIED: the
// token revoked, or its source or workspace deleted). It forgets the token, so its next request
// is looked up as an unknown token's, and the call that forgot it spends one of address's
// failures, if one is left: one per revoked token per pod, however many requests saw it refused.
func (a *Admission) Revoked(address, token string, now time.Time) {
	hash := Hash(token)
	a.mu.Lock()
	_, held := a.live[hash]
	delete(a.live, hash)
	a.mu.Unlock()
	if held {
		a.Unauthenticated(address, now)
	}
}

// Hash is the token's SHA-256 in lowercase hex, the form the backend stores and finds it by.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
