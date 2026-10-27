package sendtoken

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// address is the client the tests send from, and elsewhere another one.
const address, elsewhere = "203.0.113.9", "198.51.100.7"

var (
	t0      = time.Unix(1_700_000_000, 0)
	sender1 = Sender{TokenID: "token-1", WorkspaceID: "workspace-1", ConnectorID: "source-1"}
)

// token is the i-th well-formed send token.
func token(i int) string { return fmt.Sprintf("hx_ev_%064x", i) }

// backend is a fake Lookup: tokens are the live ones by hash, and err its failure. calls counts
// the lookups, and a lookup waits for gate to close when one is set, never watching its context.
type backend struct {
	mu     sync.Mutex
	tokens map[string]Sender
	err    error
	gate   chan struct{}
	calls  atomic.Int32
}

// newBackend finds each of tokens live as sender1.
func newBackend(tokens ...string) *backend {
	b := &backend{tokens: map[string]Sender{}}
	for _, token := range tokens {
		b.tokens[Hash(token)] = sender1
	}
	return b
}

func (b *backend) lookup(_ context.Context, hash string) (Sender, bool, error) {
	b.calls.Add(1)
	b.mu.Lock()
	gate := b.gate
	b.mu.Unlock()
	if gate != nil {
		<-gate
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sender, live := b.tokens[hash]
	return sender, live, b.err
}

// hold makes lookups wait until the returned gate closes.
func (b *backend) hold() chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gate = make(chan struct{})
	return b.gate
}

func (b *backend) revoke(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.tokens, Hash(token))
}

func (b *backend) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

// waiting is a request context that counts its Done calls. Admit calls Done only once its
// request waits on a lookup in flight, so with a lookup that never watches its context, requests
// sharing one waiting all wait on that lookup once it counts them all.
type waiting struct {
	context.Context
	n atomic.Int32
}

func newWaiting() *waiting { return &waiting{Context: context.Background()} }

func (w *waiting) Done() <-chan struct{} {
	w.n.Add(1)
	return w.Context.Done()
}

// failuresLeft spends what is left of address's failures at now, counting them.
func failuresLeft(a *Admission, address string, now time.Time) int {
	n := 0
	for a.Unauthenticated(address, now).Verdict == Refused {
		n++
	}
	return n
}

// eventually waits for a count the test's goroutines reach.
func eventually(t *testing.T, done func() bool) {
	t.Helper()
	require.Eventually(t, done, 5*time.Second, time.Millisecond)
}

// A send token is a bearer anyone may guess at: each address's failures are counted before any
// lookup, and a sender that holds a live token is never charged for it.
func TestAdmit(t *testing.T) {
	ctx := context.Background()

	t.Run("admits a live token and gives its failure back", func(t *testing.T) {
		b := newBackend(token(1))
		a := New(b.lookup, 0)
		assert.Equal(t, Outcome{Verdict: Admitted, Sender: sender1}, a.Admit(ctx, address, token(1), t0))
		assert.EqualValues(t, 1, b.calls.Load())
		assert.Equal(t, FailuresPerMinute, failuresLeft(a, address, t0))
	})

	// Taken before the lookup answers, so requests sent at once can't all pass a look at the
	// bucket taken before any was charged.
	t.Run("takes an unknown token's failure before its lookup", func(t *testing.T) {
		b := newBackend()
		gate := b.hold()
		a := New(b.lookup, 0)
		answer := make(chan Outcome, 1)
		go func() { answer <- a.Admit(ctx, address, token(1), t0) }()
		eventually(t, func() bool { return b.calls.Load() == 1 })
		assert.Equal(t, FailuresPerMinute-1, failuresLeft(a, address, t0))
		close(gate)
		assert.Equal(t, Outcome{Verdict: Refused}, <-answer)
	})

	t.Run("keeps the failure of a token that isn't live", func(t *testing.T) {
		b := newBackend()
		a := New(b.lookup, 0)
		assert.Equal(t, Outcome{Verdict: Refused}, a.Admit(ctx, address, token(1), t0))
		assert.Equal(t, FailuresPerMinute-1, failuresLeft(a, address, t0))
	})

	// The doors act on all three ids, so an answer missing one is no sender.
	for name, sender := range map[string]Sender{
		"a token id":  {WorkspaceID: "workspace-1", ConnectorID: "source-1"},
		"a workspace": {TokenID: "token-1", ConnectorID: "source-1"},
		"a source":    {TokenID: "token-1", WorkspaceID: "workspace-1"},
		"any id":      {},
	} {
		t.Run("refuses a live answer without "+name, func(t *testing.T) {
			a := New(func(context.Context, string) (Sender, bool, error) { return sender, true, nil }, 0)
			assert.Equal(t, Outcome{Verdict: Refused}, a.Admit(ctx, address, token(1), t0))
			assert.Equal(t, FailuresPerMinute-1, failuresLeft(a, address, t0))
		})
	}

	t.Run("spends a failure on a malformed token, with no lookup", func(t *testing.T) {
		b := newBackend()
		a := New(b.lookup, 0)
		digits := strings.TrimPrefix(token(1), "hx_ev_")
		malformed := []string{
			"", "hx_ev_", "hx_ev_" + digits[1:], token(1) + "0", "hx_ev_" + strings.Repeat("A", 64), "hx_ev_g" + digits[1:], "hlxs_" + digits,
		}
		for _, token := range malformed {
			assert.Equal(t, Outcome{Verdict: Refused}, a.Admit(ctx, address, token, t0), token)
		}
		assert.Zero(t, b.calls.Load())
		assert.Equal(t, FailuresPerMinute-len(malformed), failuresLeft(a, address, t0))
	})

	// Once the address has spent its failures even a live token is refused unlooked, until its
	// bucket refills; another address keeps its own.
	t.Run("limits an address with no failure left before any lookup", func(t *testing.T) {
		b := newBackend(token(1), token(2))
		a := New(b.lookup, 0)
		for i := range FailuresPerMinute {
			require.Equal(t, Outcome{Verdict: Refused}, a.Admit(ctx, address, token(100+i), t0))
		}
		limited := a.Admit(ctx, address, token(1), t0)
		assert.Equal(t, Limited, limited.Verdict)
		assert.InDelta(t, 6*time.Second, limited.RetryAfter, float64(time.Millisecond), "one failure comes back every 6 s")
		assert.Zero(t, limited.Sender)
		later := a.Admit(ctx, address, token(1), t0.Add(5*time.Second))
		assert.Equal(t, Limited, later.Verdict)
		assert.InDelta(t, time.Second, later.RetryAfter, float64(time.Millisecond))
		assert.EqualValues(t, FailuresPerMinute, b.calls.Load(), "a limited address gets no lookup")
		assert.Equal(t, Outcome{Verdict: Admitted, Sender: sender1}, a.Admit(ctx, elsewhere, token(2), t0))
		assert.Equal(t, Outcome{Verdict: Admitted, Sender: sender1}, a.Admit(ctx, address, token(1), t0.Add(6*time.Second)))
		assert.EqualValues(t, FailuresPerMinute+2, b.calls.Load())
	})

	// The sender did nothing wrong, whether its token was known live or not.
	t.Run("spends nothing when the lookup can't answer", func(t *testing.T) {
		b := newBackend(token(1))
		a := New(b.lookup, 0)
		require.Equal(t, Admitted, a.Admit(ctx, address, token(1), t0).Verdict)
		b.fail(errors.New("mysql down"))
		for i := range 2 * FailuresPerMinute {
			assert.Equal(t, Outcome{Verdict: Unavailable}, a.Admit(ctx, address, token(i%2), t0), "token %d", i%2)
		}
		assert.Equal(t, FailuresPerMinute, failuresLeft(a, address, t0))
	})

	// A token found live is no guess: its requests skip the bucket for 10 minutes after a lookup
	// last found it live, and each such lookup holds it longer.
	t.Run("lets a token found live lately skip the bucket", func(t *testing.T) {
		b := newBackend(token(1))
		a := New(b.lookup, 0)
		require.Equal(t, Admitted, a.Admit(ctx, address, token(1), t0).Verdict)
		at := t0.Add(liveFor - time.Nanosecond)
		failuresLeft(a, address, at)
		assert.Equal(t, Outcome{Verdict: Admitted, Sender: sender1}, a.Admit(ctx, address, token(1), at))
		later := at.Add(liveFor - time.Nanosecond)
		failuresLeft(a, address, later)
		assert.Equal(t, Admitted, a.Admit(ctx, address, token(1), later).Verdict, "its last lookup held it longer")
		expired := later.Add(liveFor)
		failuresLeft(a, address, expired)
		assert.Equal(t, Limited, a.Admit(ctx, address, token(1), expired).Verdict, "unfound for liveFor, it is unknown again")
		assert.EqualValues(t, 3, b.calls.Load())
	})

	// With no cache (the MCP) each request is looked up, so a revoked token fails on its next
	// request; with one (the HTTP door) a token found live is admitted unlooked for cacheFor.
	for _, c := range []struct {
		name     string
		cacheFor time.Duration
		lookups  []int32
	}{
		{"looks up every request with no cache", 0, []int32{1, 2, 3, 4, 5}},
		{"answers a token found live within cacheFor unlooked", 30 * time.Second, []int32{1, 1, 1, 2, 2}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newBackend(token(1))
			a := New(b.lookup, c.cacheFor)
			for i, at := range []time.Duration{0, time.Second, 30*time.Second - time.Nanosecond, 30 * time.Second, 59 * time.Second} {
				assert.Equal(t, Outcome{Verdict: Admitted, Sender: sender1}, a.Admit(ctx, address, token(1), t0.Add(at)), at)
				assert.Equal(t, c.lookups[i], b.calls.Load(), at)
			}
		})
	}

	// A sender's first requests sent at once are one lookup: none waits on the bucket, and none
	// is charged.
	t.Run("shares one lookup among a live token's first requests", func(t *testing.T) {
		b := newBackend(token(1))
		gate := b.hold()
		a := New(b.lookup, 0)
		const sent = 50
		waiting := newWaiting()
		answers := make(chan Outcome, sent)
		for range sent {
			go func() { answers <- a.Admit(waiting, address, token(1), t0) }()
		}
		eventually(t, func() bool { return waiting.n.Load() == sent })
		close(gate)
		for range sent {
			assert.Equal(t, Outcome{Verdict: Admitted, Sender: sender1}, <-answers)
		}
		assert.EqualValues(t, 1, b.calls.Load())
		assert.Equal(t, FailuresPerMinute, failuresLeft(a, address, t0))
	})

	// One guess, one failure, however many requests carry it.
	t.Run("shares one lookup and one failure among an unknown token's requests", func(t *testing.T) {
		b := newBackend()
		gate := b.hold()
		a := New(b.lookup, 0)
		const sent = 50
		waiting := newWaiting()
		answers := make(chan Outcome, sent)
		for range sent {
			go func() { answers <- a.Admit(waiting, address, token(1), t0) }()
		}
		eventually(t, func() bool { return waiting.n.Load() == sent })
		close(gate)
		for range sent {
			assert.Equal(t, Outcome{Verdict: Refused}, <-answers)
		}
		assert.EqualValues(t, 1, b.calls.Load())
		assert.Equal(t, FailuresPerMinute-1, failuresLeft(a, address, t0))
	})

	// Distinct guesses are a failure each, taken before their lookups: past the address's 10 the
	// rest are refused while those lookups still wait.
	t.Run("looks up at most 10 distinct unknown tokens of an address at once", func(t *testing.T) {
		b := newBackend()
		gate := b.hold()
		a := New(b.lookup, 0)
		const sent = 50
		answers := make(chan Outcome, sent)
		for i := range sent {
			go func() { answers <- a.Admit(ctx, address, token(i), t0) }()
		}
		for range sent - FailuresPerMinute {
			assert.Equal(t, Limited, (<-answers).Verdict, "only a limited request answers while the lookups wait")
		}
		eventually(t, func() bool { return b.calls.Load() == FailuresPerMinute })
		close(gate)
		for range FailuresPerMinute {
			assert.Equal(t, Outcome{Verdict: Refused}, <-answers)
		}
		assert.EqualValues(t, FailuresPerMinute, b.calls.Load())
	})

	// A leader limited by its own address's failures answers nothing for a request from another
	// address, which goes again and leads a lookup under its own; a request from the leader's
	// address takes the leader's outcome.
	t.Run("sends a request again when its leader's address was limited", func(t *testing.T) {
		b := newBackend(token(1))
		a := New(b.lookup, 0)
		failuresLeft(a, elsewhere, t0)
		hash := Hash(token(1))
		gate := make(chan struct{})
		// A request from elsewhere leads token 1's lookup, held until both requests wait on it.
		led := a.flights.DoChan(hash, func() (any, error) {
			<-gate
			return a.lead(ctx, elsewhere, hash, t0), nil
		})
		same, other := newWaiting(), newWaiting()
		sameAnswer, otherAnswer := make(chan Outcome, 1), make(chan Outcome, 1)
		go func() { sameAnswer <- a.Admit(same, elsewhere, token(1), t0) }()
		go func() { otherAnswer <- a.Admit(other, address, token(1), t0) }()
		eventually(t, func() bool { return same.n.Load() == 1 && other.n.Load() == 1 })
		close(gate)
		assert.Equal(t, Limited, (<-led).Val.(flight).Verdict)
		assert.Equal(t, Limited, (<-sameAnswer).Verdict)
		assert.EqualValues(t, 1, same.n.Load(), "the leader's address's request took its outcome")
		assert.Equal(t, Outcome{Verdict: Admitted, Sender: sender1}, <-otherAnswer)
		assert.EqualValues(t, 2, other.n.Load(), "another address's request went again")
		assert.EqualValues(t, 1, b.calls.Load())
		assert.Equal(t, FailuresPerMinute, failuresLeft(a, address, t0), "it gave its own failure back")
	})

	// A leader whose request went away answers nothing for the requests that waited on it.
	t.Run("sends a request again when its leader went away", func(t *testing.T) {
		var calls atomic.Int32
		a := New(func(ctx context.Context, _ string) (Sender, bool, error) {
			if calls.Add(1) == 1 {
				<-ctx.Done()
				return Sender{}, false, ctx.Err()
			}
			return sender1, true, nil
		}, 0)
		leaderCtx, leave := context.WithCancel(ctx)
		leader := make(chan Outcome, 1)
		go func() { leader <- a.Admit(leaderCtx, address, token(1), t0) }()
		eventually(t, func() bool { return calls.Load() == 1 })
		follower := newWaiting()
		answer := make(chan Outcome, 1)
		go func() { answer <- a.Admit(follower, address, token(1), t0) }()
		eventually(t, func() bool { return follower.n.Load() == 1 })
		leave()
		assert.Equal(t, Outcome{Verdict: Unavailable}, <-leader)
		assert.Equal(t, Outcome{Verdict: Admitted, Sender: sender1}, <-answer)
		assert.EqualValues(t, 2, calls.Load())
		assert.Equal(t, FailuresPerMinute, failuresLeft(a, address, t0), "the leader gave its failure back")
	})

	t.Run("answers a request that goes away while it waits", func(t *testing.T) {
		b := newBackend(token(1))
		gate := b.hold()
		a := New(b.lookup, 0)
		leader := make(chan Outcome, 1)
		go func() { leader <- a.Admit(ctx, address, token(1), t0) }()
		eventually(t, func() bool { return b.calls.Load() == 1 })
		gone, leave := context.WithCancel(ctx)
		leave()
		assert.Equal(t, Outcome{Verdict: Unavailable}, a.Admit(gone, elsewhere, token(1), t0))
		close(gate)
		assert.Equal(t, Outcome{Verdict: Admitted, Sender: sender1}, <-leader)
		assert.Equal(t, FailuresPerMinute, failuresLeft(a, elsewhere, t0))
	})

	// A token found live and since revoked: its requests in flight at once all fail their
	// lookups, and only the one that forgot it spends a failure.
	t.Run("spends one failure on a live token's refused lookups", func(t *testing.T) {
		b := newBackend(token(1))
		a := New(b.lookup, 0)
		require.Equal(t, Admitted, a.Admit(ctx, address, token(1), t0).Verdict)
		b.revoke(token(1))
		gate := b.hold()
		const sent = 10
		answers := make(chan Outcome, sent)
		for range sent {
			go func() { answers <- a.Admit(ctx, address, token(1), t0) }()
		}
		eventually(t, func() bool { return b.calls.Load() == 1+sent })
		close(gate)
		for range sent {
			assert.Equal(t, Outcome{Verdict: Refused}, <-answers)
		}
		assert.Equal(t, FailuresPerMinute-1, failuresLeft(a, address, t0))
	})
}

// The HTTP door admits from its cache and learns of a revoked token only when the backend
// refuses a send with it.
func TestRevoked(t *testing.T) {
	ctx := context.Background()

	t.Run("spends one failure however many requests saw the token refused", func(t *testing.T) {
		b := newBackend(token(1), token(2))
		a := New(b.lookup, 30*time.Second)
		require.Equal(t, Admitted, a.Admit(ctx, address, token(1), t0).Verdict)
		for range FailuresPerMinute - 2 {
			a.Unauthenticated(address, t0)
		}
		var revoked sync.WaitGroup
		for range 10 {
			revoked.Go(func() { a.Revoked(address, token(1), t0) })
		}
		revoked.Wait()
		assert.Equal(t, Outcome{Verdict: Admitted, Sender: sender1}, a.Admit(ctx, address, token(2), t0),
			"a new token from the address is admitted at once")
		assert.Equal(t, 1, failuresLeft(a, address, t0), "the 10 calls spent one failure")
	})

	t.Run("looks the token up again on its next request", func(t *testing.T) {
		b := newBackend(token(1))
		a := New(b.lookup, 30*time.Second)
		require.Equal(t, Admitted, a.Admit(ctx, address, token(1), t0).Verdict)
		b.revoke(token(1))
		assert.Equal(t, Admitted, a.Admit(ctx, address, token(1), t0.Add(time.Second)).Verdict, "the cache doesn't know")
		a.Revoked(address, token(1), t0.Add(time.Second))
		assert.Equal(t, Outcome{Verdict: Refused}, a.Admit(ctx, address, token(1), t0.Add(2*time.Second)))
		assert.EqualValues(t, 2, b.calls.Load())
	})

	t.Run("spends nothing for a token it doesn't hold", func(t *testing.T) {
		a := New(newBackend().lookup, 30*time.Second)
		a.Revoked(address, token(1), t0)
		assert.Equal(t, FailuresPerMinute, failuresLeft(a, address, t0))
	})
}

// A door asks Prescreen before it spends room of its own: it refuses with no lookup what Admit
// would refuse before one, and leaves every other request to Admit, having spent nothing. Each
// case runs on two admissions readied alike, one prescreened and one not, which must end alike.
func TestPrescreen(t *testing.T) {
	ctx := context.Background()
	digits := strings.TrimPrefix(token(1), "hx_ev_")
	// spent spends address's failures at at.
	spent := func(n int, at time.Time) func(*Admission) {
		return func(a *Admission) {
			for range n {
				a.Unauthenticated(address, at)
			}
		}
	}
	// live finds token 1 live at at.
	live := func(at time.Time) func(*Admission) {
		return func(a *Admission) { require.Equal(t, Admitted, a.Admit(ctx, address, token(1), at).Verdict) }
	}
	for _, c := range []struct {
		name    string
		prepare []func(*Admission)
		token   string
		at      time.Time
		decided bool
		want    Verdict
	}{
		{name: "refuses no token, spending a failure", token: "", at: t0, decided: true, want: Refused},
		{name: "refuses a malformed token, spending a failure", token: "hx_ev_" + digits[1:], at: t0, decided: true, want: Refused},
		{name: "refuses another kind of token, spending a failure", token: "hlxs_" + digits, at: t0, decided: true, want: Refused},
		{name: "limits a malformed token from an address with no failure left", prepare: []func(*Admission){spent(FailuresPerMinute, t0)},
			token: "hx_ev_", at: t0, decided: true, want: Limited},
		{name: "leaves an unknown token to Admit, spending nothing", token: token(2), at: t0, want: Refused},
		{name: "leaves an unknown token from an address with one failure left to Admit", prepare: []func(*Admission){spent(FailuresPerMinute-1, t0)},
			token: token(2), at: t0, want: Refused},
		{name: "limits an unknown token from an address with no failure left, spending nothing",
			prepare: []func(*Admission){spent(FailuresPerMinute, t0)}, token: token(2), at: t0.Add(5 * time.Second), decided: true, want: Limited},
		{name: "leaves a token found live within cacheFor to Admit, with no failure left",
			prepare: []func(*Admission){live(t0), spent(FailuresPerMinute, t0)}, token: token(1), at: t0, want: Admitted},
		{name: "leaves a token found live lately to Admit, with no failure left",
			prepare: []func(*Admission){live(t0), spent(FailuresPerMinute, t0.Add(liveFor-time.Nanosecond))}, token: token(1),
			at: t0.Add(liveFor - time.Nanosecond), want: Admitted},
		{name: "limits a token found live longer ago than liveFor, with no failure left",
			prepare: []func(*Admission){live(t0), spent(FailuresPerMinute, t0.Add(liveFor))}, token: token(1), at: t0.Add(liveFor), decided: true, want: Limited},
		{name: "leaves a request to Admit when only another address has no failure left",
			prepare: []func(*Admission){func(a *Admission) { failuresLeft(a, elsewhere, t0) }}, token: token(1), at: t0, want: Admitted},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, twinBackend := newBackend(token(1)), newBackend(token(1))
			a, twin := New(b.lookup, 30*time.Second), New(twinBackend.lookup, 30*time.Second)
			for _, prepare := range c.prepare {
				prepare(a)
				prepare(twin)
			}
			unscreened := twin.Admit(ctx, address, c.token, c.at)
			require.Equal(t, c.want, unscreened.Verdict, "the case is what Admit decides")
			calls := b.calls.Load()
			outcome, decided := a.Prescreen(address, c.token, c.at)
			require.Equal(t, c.decided, decided)
			assert.Equal(t, calls, b.calls.Load(), "no lookup")
			if decided {
				// To the nanosecond of its Retry-After.
				assert.Equal(t, unscreened, outcome)
			} else {
				assert.Equal(t, unscreened, a.Admit(ctx, address, c.token, c.at), "Admit decides as it would have unprescreened")
			}
			assert.Equal(t, failuresLeft(twin, address, c.at), failuresLeft(a, address, c.at), "it spent what Admit would have")
		})
	}

	// Lookups in flight hold their failures, so the address's last one, reserved by a lookup that
	// hasn't answered, is no failure left.
	t.Run("limits an address whose last failure a lookup in flight holds", func(t *testing.T) {
		b := newBackend()
		gate := b.hold()
		a := New(b.lookup, 30*time.Second)
		for range FailuresPerMinute - 1 {
			a.Unauthenticated(address, t0)
		}
		answer := make(chan Outcome, 1)
		go func() { answer <- a.Admit(ctx, address, token(2), t0) }()
		eventually(t, func() bool { return b.calls.Load() == 1 })
		outcome, decided := a.Prescreen(address, token(3), t0)
		assert.True(t, decided)
		assert.Equal(t, Limited, outcome.Verdict)
		close(gate)
		assert.Equal(t, Outcome{Verdict: Refused}, <-answer)
	})
}

// The HTTP door has no token to admit without a bearer header, so such a request is a guess too.
func TestUnauthenticated(t *testing.T) {
	a := New(newBackend().lookup, 0)
	for range FailuresPerMinute {
		require.Equal(t, Outcome{Verdict: Refused}, a.Unauthenticated(address, t0))
	}
	limited := a.Unauthenticated(address, t0)
	assert.Equal(t, Limited, limited.Verdict)
	assert.Positive(t, limited.RetryAfter)
	assert.Equal(t, Outcome{Verdict: Refused}, a.Unauthenticated(elsewhere, t0), "another address keeps its own")
}

// A pod holds at most capacity live tokens. Full, a new token makes room from those unfound for
// liveFor, and is held only if there was some: until then, it takes the bucket.
func TestLiveBound(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	a := New(func(_ context.Context, hash string) (Sender, bool, error) {
		calls.Add(1)
		return Sender{TokenID: hash, WorkspaceID: "workspace-1", ConnectorID: "source-1"}, true, nil
	}, 0)
	for i := range capacity {
		require.Equal(t, Admitted, a.Admit(ctx, address, token(i), t0).Verdict)
	}
	require.Len(t, a.live, capacity)

	newcomer := token(capacity)
	at := t0.Add(time.Minute)
	assert.Equal(t, Admitted, a.Admit(ctx, address, newcomer, at).Verdict)
	assert.Len(t, a.live, capacity, "full of tokens found live lately, a new one isn't held")
	failuresLeft(a, address, at)
	assert.Equal(t, Limited, a.Admit(ctx, address, newcomer, at).Verdict, "unheld, it takes the bucket")

	expired := t0.Add(liveFor)
	assert.Equal(t, Admitted, a.Admit(ctx, address, newcomer, expired).Verdict)
	assert.Len(t, a.live, 1, "the tokens unfound for liveFor made room")
	failuresLeft(a, address, expired)
	assert.Equal(t, Admitted, a.Admit(ctx, address, newcomer, expired).Verdict, "held, it skips the bucket")
	assert.EqualValues(t, capacity+3, calls.Load())
}

func TestNew(t *testing.T) {
	lookup := newBackend().lookup
	for name, build := range map[string]func(){
		"without a lookup":                    func() { New(nil, 0) },
		"with a negative cacheFor":            func() { New(lookup, -time.Nanosecond) },
		"with a cacheFor past a token's life": func() { New(lookup, liveFor+time.Nanosecond) },
	} {
		t.Run("panics "+name, func(t *testing.T) { assert.Panics(t, build) })
	}
	assert.NotPanics(t, func() { New(lookup, liveFor) })
}

// The backend stores and finds a token by its SHA-256 in lowercase hex.
func TestHash(t *testing.T) {
	sum := sha256.Sum256([]byte(token(1)))
	assert.Equal(t, hex.EncodeToString(sum[:]), Hash(token(1)))
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", Hash(""))
}
