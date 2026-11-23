// Package sendtoken admits the bearers of tokens anyone may guess at, at a public door: Foundry
// API sources' send tokens (hx_ev_, connector kind events), which both front doors that take
// them share (the MCP and the foundry-events HTTP door), and the Slack relay's link tokens
// (hx_rl_), which the foundry-events door takes from local installs. An Admission is of one kind
// of token: its format (keytoken's Validate function) and its Lookup, which answers what a live
// token acts as, a Bearer (Sender for a send token).
//
// Such a token is a bearer anyone may guess at, so an Admission counts each address's failed
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
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

const (
	// FailuresPerMinute is how many tokens one address may fail with a minute on one pod, its
	// burst too.
	FailuresPerMinute = 10
	// capacity bounds the addresses tracked, as the sandbox's limiters do, and the live tokens.
	capacity = 10_000
	// liveFor is how long a token skips the failed-auth bucket after a lookup last found it live.
	liveFor = 10 * time.Minute
)

// Bearer is what a live token acts as. Complete reports that it names everything its door acts
// on: a Lookup's answer that isn't complete is taken for no token.
type Bearer interface{ Complete() bool }

// Sender is whom a live send token sends as: the token, and the one API source and its workspace
// it may send to.
type Sender struct {
	TokenID, WorkspaceID, ConnectorID string
}

// Complete reports that the sender names its token, workspace and source: the doors act on all
// three.
func (s Sender) Complete() bool { return s.TokenID != "" && s.WorkspaceID != "" && s.ConnectorID != "" }

// Lookup finds a token by its Hash. It reports false for a token that isn't live (unknown,
// revoked, or of a deleted source, link or workspace), and an error only when it couldn't answer.
// A Bearer that isn't Complete is taken for no token.
type Lookup[B Bearer] func(ctx context.Context, hash string) (B, bool, error)

// Verdict is what a door does with a request.
type Verdict int

const (
	// Refused: no live token; a door answers 401. It is the zero Verdict, so an Outcome no
	// one decided admits no one.
	Refused Verdict = iota
	// Admitted: a live token, acting as Outcome.Bearer.
	Admitted
	// Limited: the address has no failure left, so the token wasn't looked up; a door answers 429,
	// to ask again after Outcome.RetryAfter.
	Limited
	// Unavailable: the lookup couldn't answer, or the request went away first; a door answers 503.
	Unavailable
)

// Outcome is an Admission's answer for one request. Bearer is set only when Admitted, and
// RetryAfter only when Limited.
type Outcome[B Bearer] struct {
	Verdict
	Bearer     B
	RetryAfter time.Duration
}

// Admission admits one kind of token on one pod; make one with New and share it among requests.
type Admission[B Bearer] struct {
	format   func(token string) error
	lookup   Lookup[B]
	cacheFor time.Duration
	failures *iplimit.Limiters
	flights  singleflight.Group

	mu sync.Mutex
	// live are the tokens, by hash, a lookup found live in the last liveFor.
	live map[string]found[B]
}

// found is a token a lookup found live at at, acting as bearer.
type found[B Bearer] struct {
	bearer B
	at     time.Time
}

// flight is a cold lookup's outcome for every request that waited on it. address is the
// leader's, for a Limited outcome, and gone says the leader went away before the lookup
// answered: either outcome is the leader's alone.
type flight[B Bearer] struct {
	Outcome[B]
	address string
	gone    bool
}

// New makes a pod's admission of tokens of format (such as keytoken.ValidateEventSource) over
// lookup. With cacheFor 0 each request bearing a live token is looked up, so a revoked token is
// refused on its next request. With more, a token found live is admitted for cacheFor without
// another lookup: for a door whose backend checks the token again on every call, and that calls
// Revoked when it refuses one. New panics without a format or a lookup, or for a cacheFor below 0
// or past how long a token stays live (10 minutes).
func New[B Bearer](format func(token string) error, lookup Lookup[B], cacheFor time.Duration) *Admission[B] {
	if format == nil || lookup == nil || cacheFor < 0 || cacheFor > liveFor {
		panic(fmt.Sprintf("sendtoken: New needs a format, a lookup and a cacheFor within [0, %v], got %v", liveFor, cacheFor))
	}
	return &Admission[B]{
		format: format, lookup: lookup, cacheFor: cacheFor,
		failures: iplimit.New(rate.Every(time.Minute/FailuresPerMinute), FailuresPerMinute, capacity),
		live:     map[string]found[B]{},
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
func (a *Admission[B]) Admit(ctx context.Context, address, token string, now time.Time) Outcome[B] {
	if a.format(token) != nil {
		return a.Unauthenticated(address, now)
	}
	hash := Hash(token)
	a.mu.Lock()
	known, held := a.live[hash]
	a.mu.Unlock()
	age := now.Sub(known.at)
	switch {
	case held && age >= 0 && age < a.cacheFor:
		return Outcome[B]{Verdict: Admitted, Bearer: known.bearer}
	case held && age < liveFor:
		bearer, live, err := a.find(ctx, hash)
		switch {
		case err != nil:
			return Outcome[B]{Verdict: Unavailable}
		case !live:
			a.Revoked(address, token, now)
			return Outcome[B]{Verdict: Refused}
		}
		a.hold(hash, bearer, now)
		return Outcome[B]{Verdict: Admitted, Bearer: bearer}
	}
	for {
		var led bool
		answer := a.flights.DoChan(hash, func() (any, error) {
			led = true
			return a.lead(ctx, address, hash, now), nil
		})
		select {
		case <-ctx.Done():
			return Outcome[B]{Verdict: Unavailable}
		case result := <-answer:
			// led is the flight's write, which happened before its answer was sent.
			outcome := result.Val.(flight[B])
			if led || !outcome.gone && (outcome.Verdict != Limited || outcome.address == address) {
				return outcome.Outcome
			}
		}
	}
}

// lead looks a token not found live lately up for every request bearing it at the time, taking
// one of the leading request's address's failures first and keeping it only when the token isn't
// live.
func (a *Admission[B]) lead(ctx context.Context, address, hash string, now time.Time) flight[B] {
	reservation := a.failures.Reserve(address, now)
	if wait := reservation.DelayFrom(now); wait > 0 {
		reservation.CancelAt(now)
		return flight[B]{Outcome: Outcome[B]{Verdict: Limited, RetryAfter: wait}, address: address}
	}
	bearer, live, err := a.find(ctx, hash)
	switch {
	case err != nil:
		reservation.CancelAt(now)
		return flight[B]{Outcome: Outcome[B]{Verdict: Unavailable}, gone: ctx.Err() != nil}
	case !live:
		return flight[B]{Outcome: Outcome[B]{Verdict: Refused}}
	}
	reservation.CancelAt(now)
	a.hold(hash, bearer, now)
	return flight[B]{Outcome: Outcome[B]{Verdict: Admitted, Bearer: bearer}}
}

// find is the lookup, taking a bearer that isn't complete for no token.
func (a *Admission[B]) find(ctx context.Context, hash string) (B, bool, error) {
	bearer, live, err := a.lookup(ctx, hash)
	return bearer, live && bearer.Complete(), err
}

// hold marks a token found live at now. Full, it first makes room from the tokens found live
// longer than liveFor ago, one pass over them per new token, and holds no new token when none
// is: that token's requests then take the bucket as an unknown token's do.
func (a *Admission[B]) hold(hash string, bearer B, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.live[hash]; !ok && len(a.live) >= capacity {
		maps.DeleteFunc(a.live, func(_ string, f found[B]) bool { return now.Sub(f.at) >= liveFor })
		if len(a.live) >= capacity {
			return
		}
	}
	a.live[hash] = found[B]{bearer: bearer, at: now}
}

// Prescreen decides, with no lookup, a request bearing token from address at now that Admit
// would refuse before any: something that isn't a token of its format (an empty one for a request
// with none) spends one of address's failures, Refused or Limited as Unauthenticated; a token not
// found live lately, from an address with no failure left, is Limited, spending nothing. It
// reports false, deciding nothing, for any other request: a door then makes room for it and
// admits it with Admit. So a door that prescreens spends nothing of its own on a request it would
// refuse unlooked. Admit decides on its own all the same, since another request may spend the
// address's last failure, or find the token live, in between; and an address with none left
// looks up no new token, not even by waiting on another address's lookup of it.
func (a *Admission[B]) Prescreen(address, token string, now time.Time) (Outcome[B], bool) {
	if a.format(token) != nil {
		return a.Unauthenticated(address, now), true
	}
	a.mu.Lock()
	known, held := a.live[Hash(token)]
	a.mu.Unlock()
	if held && now.Sub(known.at) < liveFor {
		return Outcome[B]{}, false
	}
	if wait := a.failures.Delay(address, now); wait > 0 {
		return Outcome[B]{Verdict: Limited, RetryAfter: wait}, true
	}
	return Outcome[B]{}, false
}

// Unauthenticated spends one of address's failures on a request that bears no token, or
// something that isn't one: Refused, or Limited once the address has none left.
func (a *Admission[B]) Unauthenticated(address string, now time.Time) Outcome[B] {
	reservation := a.failures.Reserve(address, now)
	if wait := reservation.DelayFrom(now); wait > 0 {
		reservation.CancelAt(now)
		return Outcome[B]{Verdict: Limited, RetryAfter: wait}
	}
	return Outcome[B]{Verdict: Refused}
}

// Revoked tells the admission the backend refused a token it admitted (the token revoked, or its
// source, link or workspace deleted). It forgets the token, so its next request is looked up as
// an unknown token's, and the call that forgot it spends one of address's failures, if one is
// left: one per revoked token per pod, however many requests saw it refused.
func (a *Admission[B]) Revoked(address, token string, now time.Time) {
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
