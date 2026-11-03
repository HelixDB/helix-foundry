package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/store"
)

// questionFixture is a workspace with one ontology, whose questions are asked
// of load FL-<tag>, $0.50 each, under a $1 month.
type questionFixture struct {
	db          store.Store
	workspaceID string
	ontologyID  string
	tag         string
}

const questionCap = 500_000

func newQuestionFixture(t *testing.T, tag string) questionFixture {
	t.Helper()
	db := newTestStore(t)
	f := questionFixture{db: db, workspaceID: id26("QWS-" + tag), ontologyID: id26("QO-" + tag), tag: tag}
	createWorkspace(t, f.workspaceID)
	return f
}

func (f questionFixture) limits() model.FoundryQuestionLimits {
	now := time.Now().UTC()
	return model.FoundryQuestionLimits{
		Since: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), CapMicros: 1_000_000, PerHour: 30, Running: 5,
	}
}

func (f questionFixture) claim(limits model.FoundryQuestionLimits) model.FoundryQuestionClaim {
	return model.FoundryQuestionClaim{Lease: time.Minute, Limits: limits, Defer: time.Hour}
}

// ask queues question Q-<tag>-<n> in thread (its own when ""), and returns its id.
func (f questionFixture) ask(t *testing.T, n int, thread string) string {
	t.Helper()
	require.NoError(t, f.tryAsk(n, thread, f.limits()))
	return f.id(n)
}

// id is question n's id, its number fixed-width so that padding never makes two alike.
func (f questionFixture) id(n int) string { return id26(fmt.Sprintf("Q-%s-%03d-", f.tag, n)) }

func (f questionFixture) tryAsk(n int, thread string, limits model.FoundryQuestionLimits) error {
	id := f.id(n)
	if thread == "" {
		thread = id
	}
	token := fmt.Sprintf("ask-%d", n)
	return f.db.CreateFoundryQuestion(context.Background(), &model.FoundryQuestion{
		ID: id, WorkspaceID: f.workspaceID, OntologyID: f.ontologyID, LoadID: id26("FL-" + f.tag), VersionID: id26("FV-" + f.tag),
		ThreadID: thread, Question: fmt.Sprintf("question %d?", n), Focus: json.RawMessage(`{"type":"Customer"}`),
		ClientToken: &token, MaxUSDMicros: questionCap,
	}, limits)
}

func (f questionFixture) get(t *testing.T, id string) *model.FoundryQuestion {
	t.Helper()
	q, err := f.db.GetFoundryQuestion(context.Background(), f.ontologyID, id)
	require.NoError(t, err)
	return q
}

// due makes a deferred question due again.
func (f questionFixture) due(t *testing.T, id string) {
	t.Helper()
	_, err := testDB.Exec(`UPDATE foundry_questions SET next_attempt_at=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, id)
	require.NoError(t, err)
}

func answered(usd int64) model.FoundryQuestionOutcome {
	return model.FoundryQuestionOutcome{
		Status: model.FoundryQuestionAnswered, Result: json.RawMessage(`{"status":"answered"}`),
		Spend: model.FoundryGraphJobSpend{USDMicros: usd, InputTokens: 10, OutputTokens: 2},
	}
}

func TestFoundryQuestionCreate(t *testing.T) {
	ctx := context.Background()

	t.Run("queues a question and finds it by its client token", func(t *testing.T) {
		f := newQuestionFixture(t, "C1")
		id := f.ask(t, 1, "")
		q := f.get(t, id)
		assert.Equal(t, model.FoundryQuestionQueued, q.Status)
		assert.Equal(t, id, q.ThreadID)
		assert.JSONEq(t, `{"type":"Customer"}`, string(q.Focus))
		assert.Equal(t, int64(questionCap), q.MaxUSDMicros)
		assert.Zero(t, q.ReservedMicros)
		assert.Nil(t, q.FinishedAt)
		byToken, err := f.db.GetFoundryQuestionByClientToken(ctx, f.ontologyID, "ask-1")
		require.NoError(t, err)
		assert.Equal(t, id, byToken.ID)
		assert.ErrorIs(t, f.tryAsk(1, "", f.limits()), store.ErrConflict, "the same client token is the same question")
	})

	t.Run("refuses past the hour's count", func(t *testing.T) {
		f := newQuestionFixture(t, "C2")
		limits := f.limits()
		limits.PerHour = 2
		require.NoError(t, f.tryAsk(1, "", limits))
		require.NoError(t, f.tryAsk(2, "", limits))
		var refusal model.FoundryQuestionRefusal
		require.ErrorAs(t, f.tryAsk(3, "", limits), &refusal)
		assert.False(t, refusal.Budget)
		_, err := testDB.Exec(`UPDATE foundry_questions SET created_at=NOW(6) - INTERVAL 2 HOUR`)
		require.NoError(t, err)
		require.NoError(t, f.tryAsk(3, "", limits), "an hour later the count starts again")
	})

	// What the month settled counts; a question charged to last month doesn't.
	t.Run("refuses a question the month's settled spend can't fit", func(t *testing.T) {
		f := newQuestionFixture(t, "C3")
		id := f.ask(t, 1, "")
		q, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		require.Equal(t, id, q.ID)
		require.NoError(t, f.db.SettleFoundryQuestion(ctx, id, "w1", answered(600_000)))
		var refusal model.FoundryQuestionRefusal
		require.ErrorAs(t, f.tryAsk(2, "", f.limits()), &refusal)
		assert.True(t, refusal.Budget)
		assert.Contains(t, refusal.Error(), "$1.00")
		_, err = testDB.Exec(`UPDATE foundry_questions SET charged_at=? WHERE id=?`, f.limits().Since.Add(-time.Hour), id)
		require.NoError(t, err)
		require.NoError(t, f.tryAsk(2, "", f.limits()))
	})

	// A question asked last month but run this month spent this month's money,
	// so a new ask must see it.
	t.Run("refuses a question past what last month's questions spent this month", func(t *testing.T) {
		f := newQuestionFixture(t, "C5")
		id := f.ask(t, 1, "")
		_, err := testDB.Exec(`UPDATE foundry_questions SET created_at=? WHERE id=?`, f.limits().Since.Add(-time.Hour), id)
		require.NoError(t, err)
		_, err = f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		require.NoError(t, f.db.SettleFoundryQuestion(ctx, id, "w1", answered(600_000)))
		var refusal model.FoundryQuestionRefusal
		require.ErrorAs(t, f.tryAsk(2, "", f.limits()), &refusal)
		assert.True(t, refusal.Budget)
	})

	t.Run("refuses a workspace that is gone", func(t *testing.T) {
		f := newQuestionFixture(t, "C4")
		f.workspaceID = id26("QWS-GONE")
		assert.ErrorIs(t, f.tryAsk(1, "", f.limits()), store.ErrNotFound)
	})
}

func TestFoundryQuestionClaim(t *testing.T) {
	ctx := context.Background()

	// Two $0.50 reservations fill a $1 month; the third waits, behind
	// questions after it, until a settle frees enough.
	t.Run("reserves the cap and holds back what the month's reservations can't fit", func(t *testing.T) {
		f := newQuestionFixture(t, "L1")
		for n := 1; n <= 3; n++ {
			f.ask(t, n, "")
		}
		claim := f.claim(f.limits())
		for n := 1; n <= 2; n++ {
			q, err := f.db.ClaimFoundryQuestion(ctx, "w1", claim)
			require.NoError(t, err)
			require.NotNil(t, q)
			assert.Equal(t, f.id(n), q.ID)
			assert.Equal(t, model.FoundryQuestionRunning, q.Status)
			assert.Equal(t, int64(questionCap), q.ReservedMicros)
			assert.Equal(t, 1, q.Attempts)
			require.NotNil(t, q.StartedAt)
		}
		q, err := f.db.ClaimFoundryQuestion(ctx, "w1", claim)
		require.NoError(t, err)
		assert.Nil(t, q, "the third would pass the month with the two running")
		third := f.get(t, f.id(3))
		assert.Equal(t, model.FoundryQuestionQueued, third.Status)
		assert.True(t, third.NextAttemptAt.After(time.Now().Add(30*time.Minute)), "deferred by claim.Defer")

		require.NoError(t, f.db.SettleFoundryQuestion(ctx, f.id(1), "w1", answered(100_000)))
		f.due(t, f.id(3))
		q, err = f.db.ClaimFoundryQuestion(ctx, "w1", claim)
		require.NoError(t, err)
		assert.Nil(t, q, "0.10 settled + 0.50 reserved + 0.50 is still over")
		require.NoError(t, f.db.SettleFoundryQuestion(ctx, f.id(2), "w1", answered(100_000)))
		f.due(t, f.id(3))
		q, err = f.db.ClaimFoundryQuestion(ctx, "w1", claim)
		require.NoError(t, err)
		require.NotNil(t, q)
		assert.Equal(t, f.id(3), q.ID)
	})

	// A question the month's settled spend alone can't fit would wait for
	// next month: it fails now, and holds nothing.
	t.Run("fails a question over the month's budget", func(t *testing.T) {
		f := newQuestionFixture(t, "L2")
		f.ask(t, 1, "")
		f.ask(t, 2, "")
		_, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		require.NoError(t, f.db.SettleFoundryQuestion(ctx, f.id(1), "w1", answered(900_000)))
		q, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		assert.Nil(t, q)
		refused := f.get(t, f.id(2))
		assert.Equal(t, model.FoundryQuestionFailed, refused.Status)
		require.NotNil(t, refused.Error)
		assert.Contains(t, *refused.Error, "spent this month's $1.00")
		assert.NotNil(t, refused.FinishedAt)
		assert.Zero(t, refused.ReservedMicros)
	})

	// A workspace at its running limit waits; another workspace's question,
	// queued after it, is not held up behind it.
	t.Run("holds a workspace to its running questions without blocking others", func(t *testing.T) {
		f := newQuestionFixture(t, "L3")
		other := questionFixture{db: f.db, workspaceID: id26("QWS-L3B"), ontologyID: id26("QO-L3B"), tag: "L3B"}
		createWorkspace(t, other.workspaceID)
		f.ask(t, 1, "")
		f.ask(t, 2, "")
		other.ask(t, 1, "")
		limits := f.limits()
		limits.Running = 1
		first, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(limits))
		require.NoError(t, err)
		assert.Equal(t, f.id(1), first.ID)
		next, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(limits))
		require.NoError(t, err)
		require.NotNil(t, next)
		assert.Equal(t, other.id(1), next.ID, "the held-back question went behind the other workspace's")
		assert.Equal(t, model.FoundryQuestionQueued, f.get(t, f.id(2)).Status)

		require.NoError(t, f.db.SettleFoundryQuestion(ctx, f.id(1), "w1", answered(1)))
		f.due(t, f.id(2))
		q, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(limits))
		require.NoError(t, err)
		require.NotNil(t, q)
		assert.Equal(t, f.id(2), q.ID)
	})

	// A worker that died left its lease to lapse: another claims the
	// question, its dead attempt's reservation charged as spent.
	t.Run("reclaims an expired lease and fences the old worker", func(t *testing.T) {
		f := newQuestionFixture(t, "L4")
		id := f.ask(t, 1, "")
		claim := f.claim(f.limits())
		claim.Lease = time.Millisecond
		_, err := f.db.ClaimFoundryQuestion(ctx, "w1", claim)
		require.NoError(t, err)
		time.Sleep(20 * time.Millisecond)
		q, err := f.db.ClaimFoundryQuestion(ctx, "w2", f.claim(f.limits()))
		require.NoError(t, err)
		require.NotNil(t, q)
		assert.Equal(t, id, q.ID)
		assert.Equal(t, 2, q.Attempts)
		assert.Equal(t, int64(questionCap), q.Spend.USDMicros, "the dead attempt is charged its reservation")
		assert.Equal(t, int64(questionCap), q.ReservedMicros)
		assert.ErrorIs(t, f.db.RenewFoundryQuestionLease(ctx, id, "w1", time.Minute), store.ErrNotFound)
		assert.ErrorIs(t, f.db.AppendFoundryQuestionEvents(ctx, id, "w1", []json.RawMessage{json.RawMessage(`{}`)}), store.ErrNotFound)
		assert.ErrorIs(t, f.db.SettleFoundryQuestion(ctx, id, "w1", answered(1)), store.ErrNotFound)
		require.NoError(t, f.db.RenewFoundryQuestionLease(ctx, id, "w2", time.Minute))
		assert.Zero(t, f.get(t, id).LastSeq, "the old worker stored no event")
	})

	// The month's side of the budget: what its questions spent and what the
	// running ones reserve. A question charged before the month doesn't
	// count, nor does another workspace's.
	t.Run("sums the month's spend and reservations", func(t *testing.T) {
		f := newQuestionFixture(t, "L6")
		for n := 1; n <= 3; n++ {
			f.ask(t, n, "")
		}
		claim := f.claim(f.limits())
		_, err := f.db.ClaimFoundryQuestion(ctx, "w1", claim)
		require.NoError(t, err)
		require.NoError(t, f.db.SettleFoundryQuestion(ctx, f.id(1), "w1", answered(200_000)))
		running, err := f.db.ClaimFoundryQuestion(ctx, "w1", claim)
		require.NoError(t, err)
		require.Equal(t, f.id(2), running.ID)
		_, err = testDB.Exec(`UPDATE foundry_questions SET charged_at=?, usd_micros=900000 WHERE id=?`, f.limits().Since.Add(-time.Hour), f.id(3))
		require.NoError(t, err)

		spend, err := f.db.SumFoundryQuestionSpend(ctx, f.workspaceID, f.limits().Since)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryQuestionSpend{SpentMicros: 200_000, ReservedMicros: questionCap}, spend)
		none, err := f.db.SumFoundryQuestionSpend(ctx, id26("QWS-NONE"), f.limits().Since)
		require.NoError(t, err)
		assert.Zero(t, none)
	})

	// A question is charged to the month that claims it, not the one that
	// asked it: its reservation and spend count against this month, and the
	// list's "spent this month" shows them.
	t.Run("charges a question asked last month to the month that runs it", func(t *testing.T) {
		f := newQuestionFixture(t, "L9")
		id := f.ask(t, 1, "")
		_, err := testDB.Exec(`UPDATE foundry_questions SET created_at=? WHERE id=?`, f.limits().Since.Add(-time.Hour), id)
		require.NoError(t, err)
		before := time.Now()
		q, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		require.Equal(t, id, q.ID)
		var chargedAt time.Time
		require.NoError(t, testDB.QueryRow(`SELECT charged_at FROM foundry_questions WHERE id=?`, id).Scan(&chargedAt))
		assert.WithinDuration(t, before, chargedAt, time.Minute)
		spend, err := f.db.SumFoundryQuestionSpend(ctx, f.workspaceID, f.limits().Since)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryQuestionSpend{ReservedMicros: questionCap}, spend)
		require.NoError(t, f.db.SettleFoundryQuestion(ctx, id, "w1", answered(300_000)))
		spend, err = f.db.SumFoundryQuestionSpend(ctx, f.workspaceID, f.limits().Since)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryQuestionSpend{SpentMicros: 300_000}, spend)
	})

	// After a worker outage across the month's start, every question queued
	// last month is due at once. Their reservations and spend count against
	// the month that runs them, so running them together, or one after
	// another, never passes its budget: two $0.50 questions fill the $1 month
	// and the rest wait, then fail once the month is spent.
	t.Run("holds a backlog asked last month to this month's budget", func(t *testing.T) {
		f := newQuestionFixture(t, "L10")
		const backlog = 6
		for n := 1; n <= backlog; n++ {
			f.ask(t, n, "")
		}
		_, err := testDB.Exec(`UPDATE foundry_questions SET created_at=? WHERE workspace_id=?`, f.limits().Since.Add(-time.Hour), f.workspaceID)
		require.NoError(t, err)
		claim := f.claim(f.limits())
		for n := 1; n <= 2; n++ {
			q, err := f.db.ClaimFoundryQuestion(ctx, "w1", claim)
			require.NoError(t, err)
			require.NotNil(t, q)
			assert.Equal(t, f.id(n), q.ID)
		}
		q, err := f.db.ClaimFoundryQuestion(ctx, "w1", claim)
		require.NoError(t, err)
		assert.Nil(t, q, "the two running reserve the whole month")
		for n := 3; n <= backlog; n++ {
			assert.Equal(t, model.FoundryQuestionQueued, f.get(t, f.id(n)).Status, "question %d waits", n)
		}

		for n := 1; n <= 2; n++ {
			require.NoError(t, f.db.SettleFoundryQuestion(ctx, f.id(n), "w1", answered(questionCap)))
		}
		for n := 3; n <= backlog; n++ {
			f.due(t, f.id(n))
		}
		q, err = f.db.ClaimFoundryQuestion(ctx, "w1", claim)
		require.NoError(t, err)
		assert.Nil(t, q, "the month is spent")
		for n := 3; n <= backlog; n++ {
			assert.Equal(t, model.FoundryQuestionFailed, f.get(t, f.id(n)).Status, "question %d fails", n)
		}
		spend, err := f.db.SumFoundryQuestionSpend(ctx, f.workspaceID, f.limits().Since)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryQuestionSpend{SpentMicros: 1_000_000}, spend)
	})

	// A retried question claimed last month and again this month carries what
	// its earlier attempt spent into this month: never less than it spends
	// here, so the month can't be passed.
	t.Run("carries an earlier month's attempt into the month that retries it", func(t *testing.T) {
		f := newQuestionFixture(t, "L11")
		id := f.ask(t, 1, "")
		claim := f.claim(f.limits())
		_, err := f.db.ClaimFoundryQuestion(ctx, "w1", claim)
		require.NoError(t, err)
		retry := time.Now().Add(-time.Second)
		require.NoError(t, f.db.SettleFoundryQuestion(ctx, id, "w1", model.FoundryQuestionOutcome{
			Status: model.FoundryQuestionQueued, RetryAt: &retry, Error: "busy", Spend: model.FoundryGraphJobSpend{USDMicros: 400_000},
		}))
		_, err = testDB.Exec(`UPDATE foundry_questions SET created_at=?, charged_at=? WHERE id=?`,
			f.limits().Since.Add(-time.Hour), f.limits().Since.Add(-time.Hour), id)
		require.NoError(t, err)
		spend, err := f.db.SumFoundryQuestionSpend(ctx, f.workspaceID, f.limits().Since)
		require.NoError(t, err)
		assert.Zero(t, spend, "last month's attempt is last month's")

		q, err := f.db.ClaimFoundryQuestion(ctx, "w1", claim)
		require.NoError(t, err)
		require.NotNil(t, q)
		spend, err = f.db.SumFoundryQuestionSpend(ctx, f.workspaceID, f.limits().Since)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryQuestionSpend{SpentMicros: 400_000, ReservedMicros: questionCap}, spend)
	})

	t.Run("fails a question whose workspace was deleted", func(t *testing.T) {
		f := newQuestionFixture(t, "L7")
		id := f.ask(t, 1, "")
		_, err := testDB.Exec(`UPDATE workspaces SET deleted_at=NOW(6) WHERE id=?`, f.workspaceID)
		require.NoError(t, err)
		q, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		assert.Nil(t, q)
		failed := f.get(t, id)
		assert.Equal(t, model.FoundryQuestionFailed, failed.Status)
		require.NotNil(t, failed.Error)
		assert.Equal(t, "the workspace was deleted", *failed.Error)
	})

	// Asks lock the workspace, then insert; a claim locks the workspace, then
	// the question. Asked and claimed at once in one workspace by three
	// workers, nothing deadlocks, every question is answered once, and no
	// claim ever sees more than the workspace's two running questions.
	t.Run("asks and claims at once without deadlocking", func(t *testing.T) {
		f := newQuestionFixture(t, "L8")
		roomy := f.limits()
		roomy.CapMicros, roomy.PerHour, roomy.Running = 1_000_000_000, 1_000, 2
		claim := f.claim(roomy)
		claim.Defer = time.Millisecond
		const asks = 40
		runCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		var settled, maxRunning atomic.Int64
		g, gctx := errgroup.WithContext(runCtx)
		g.Go(func() error {
			for n := range asks {
				if err := f.tryAsk(n, "", roomy); err != nil {
					return err
				}
			}
			return nil
		})
		for w := range 3 {
			worker := fmt.Sprintf("w%d", w)
			g.Go(func() error {
				for settled.Load() < asks {
					q, err := f.db.ClaimFoundryQuestion(gctx, worker, claim)
					switch {
					case err != nil:
						return err
					case q == nil:
						time.Sleep(2 * time.Millisecond)
						continue
					}
					var running int64
					if err := testDB.QueryRowContext(gctx, `SELECT COUNT(*) FROM foundry_questions WHERE workspace_id=? AND status='running'`,
						f.workspaceID).Scan(&running); err != nil {
						return err
					}
					for seen := maxRunning.Load(); running > seen; seen = maxRunning.Load() {
						if maxRunning.CompareAndSwap(seen, running) {
							break
						}
					}
					time.Sleep(5 * time.Millisecond)
					if err := f.db.SettleFoundryQuestion(gctx, q.ID, worker, answered(1)); err != nil {
						return fmt.Errorf("settle %s: %w", q.ID, err)
					}
					settled.Add(1)
				}
				return nil
			})
		}
		require.NoError(t, g.Wait())
		assert.Equal(t, int64(asks), settled.Load())
		assert.LessOrEqual(t, maxRunning.Load(), int64(2))
		var answeredCount int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_questions WHERE workspace_id=? AND status='answered' AND attempts=1`,
			f.workspaceID).Scan(&answeredCount))
		assert.Equal(t, asks, answeredCount)
	})

	t.Run("finds nothing when nothing is due", func(t *testing.T) {
		f := newQuestionFixture(t, "L5")
		q, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		assert.Nil(t, q)
		_, err = f.db.ClaimFoundryQuestion(ctx, "w1", model.FoundryQuestionClaim{})
		assert.Error(t, err, "a claim without limits is refused")
	})
}

func TestFoundryQuestionRun(t *testing.T) {
	ctx := context.Background()

	t.Run("numbers events in order, only for the lease holder", func(t *testing.T) {
		f := newQuestionFixture(t, "R1")
		id := f.ask(t, 1, "")
		_, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		require.NoError(t, f.db.AppendFoundryQuestionEvents(ctx, id, "w1",
			[]json.RawMessage{json.RawMessage(`{"event":"started","model":"m"}`), json.RawMessage(`{"event":"tool_call","call_id":"c1"}`)}))
		require.NoError(t, f.db.AppendFoundryQuestionEvents(ctx, id, "w1", nil))
		require.NoError(t, f.db.AppendFoundryQuestionEvents(ctx, id, "w1", []json.RawMessage{json.RawMessage(`{"event":"finished"}`)}))
		assert.ErrorIs(t, f.db.AppendFoundryQuestionEvents(ctx, id, "w2", []json.RawMessage{json.RawMessage(`{}`)}), store.ErrNotFound)
		assert.Equal(t, 3, f.get(t, id).LastSeq)

		events, err := f.db.ListFoundryQuestionEvents(ctx, id, 1, 10)
		require.NoError(t, err)
		require.Len(t, events, 2)
		assert.Equal(t, 2, events[0].Seq)
		assert.JSONEq(t, `{"event":"tool_call","call_id":"c1"}`, string(events[0].Event))
		assert.Equal(t, 3, events[1].Seq)
		page, err := f.db.ListFoundryQuestionEvents(ctx, id, 0, 1)
		require.NoError(t, err)
		require.Len(t, page, 1)
		assert.Equal(t, 1, page[0].Seq)
	})

	t.Run("settles a retry, giving back the reservation and, when uncounted, the attempt", func(t *testing.T) {
		f := newQuestionFixture(t, "R2")
		id := f.ask(t, 1, "")
		_, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		retryAt := time.Now().Add(time.Minute)
		require.NoError(t, f.db.SettleFoundryQuestion(ctx, id, "w1", model.FoundryQuestionOutcome{
			Status: model.FoundryQuestionQueued, RetryAt: &retryAt, Uncounted: true, Error: "the agent is unavailable",
			Spend: model.FoundryGraphJobSpend{USDMicros: 7},
		}))
		q := f.get(t, id)
		assert.Equal(t, model.FoundryQuestionQueued, q.Status)
		assert.Zero(t, q.Attempts)
		assert.Zero(t, q.ReservedMicros)
		assert.Equal(t, int64(7), q.Spend.USDMicros)
		assert.Nil(t, q.LeaseOwner)
		assert.WithinDuration(t, retryAt, q.NextAttemptAt, time.Second)
		assert.ErrorIs(t, f.db.SettleFoundryQuestion(ctx, id, "w1", answered(1)), store.ErrNotFound, "the retry gave the lease up")
	})

	t.Run("settles an answer with its result and spend", func(t *testing.T) {
		f := newQuestionFixture(t, "R3")
		id := f.ask(t, 1, "")
		_, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		require.Error(t, f.db.SettleFoundryQuestion(ctx, id, "w1", model.FoundryQuestionOutcome{Status: model.FoundryQuestionAnswered}),
			"an answer without its result is refused")
		require.NoError(t, f.db.SettleFoundryQuestion(ctx, id, "w1", answered(123)))
		q := f.get(t, id)
		assert.Equal(t, model.FoundryQuestionAnswered, q.Status)
		assert.JSONEq(t, `{"status":"answered"}`, string(q.Result))
		assert.Equal(t, model.FoundryGraphJobSpend{USDMicros: 123, InputTokens: 10, OutputTokens: 2}, q.Spend)
		assert.Zero(t, q.ReservedMicros)
		assert.NotNil(t, q.FinishedAt)
		assert.Nil(t, q.Error)
	})

	// A cancel ends the question at once; the running attempt finds its
	// lease gone at its next write, and its reservation was charged.
	t.Run("cancels a queued or running question", func(t *testing.T) {
		f := newQuestionFixture(t, "R4")
		running := f.ask(t, 1, "")
		queued := f.ask(t, 2, "")
		_, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)

		q, err := f.db.CancelFoundryQuestion(ctx, f.ontologyID, running)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryQuestionCancelled, q.Status)
		assert.Equal(t, int64(questionCap), q.Spend.USDMicros)
		assert.Zero(t, q.ReservedMicros)
		assert.NotNil(t, q.FinishedAt)
		assert.ErrorIs(t, f.db.AppendFoundryQuestionEvents(ctx, running, "w1", []json.RawMessage{json.RawMessage(`{}`)}), store.ErrNotFound)
		assert.ErrorIs(t, f.db.RenewFoundryQuestionLease(ctx, running, "w1", time.Minute), store.ErrNotFound)
		assert.ErrorIs(t, f.db.SettleFoundryQuestion(ctx, running, "w1", answered(1)), store.ErrNotFound)

		q, err = f.db.CancelFoundryQuestion(ctx, f.ontologyID, queued)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryQuestionCancelled, q.Status)
		assert.Zero(t, q.Spend.USDMicros)
		again, err := f.db.CancelFoundryQuestion(ctx, f.ontologyID, queued)
		require.NoError(t, err, "cancelling twice is the same cancel")
		assert.Equal(t, q.FinishedAt, again.FinishedAt)
		claimed, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		assert.Nil(t, claimed, "a cancelled question is never claimed")

		_, err = f.db.CancelFoundryQuestion(ctx, id26("QO-OTHER"), queued)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("refuses to cancel a question that ended", func(t *testing.T) {
		f := newQuestionFixture(t, "R5")
		id := f.ask(t, 1, "")
		_, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		require.NoError(t, f.db.SettleFoundryQuestion(ctx, id, "w1", answered(1)))
		_, err = f.db.CancelFoundryQuestion(ctx, f.ontologyID, id)
		assert.ErrorIs(t, err, store.ErrConflict)
	})
}

// A result is stored as the agent sent it, in a JSON column MySQL hands back
// with its keys reordered and spaced. A 3.11.0 result's visuals and outputs
// come through that: the question reads whole, a list leaves the outputs out,
// a thread's history is the text with a line for each visual. A 3.10.0
// result reads as it did.
func TestFoundryQuestionResults(t *testing.T) {
	ctx := context.Background()
	const usage = `"usage":{"input":10,"cache_read":0,"cache_write":0,"output":2,"usd":0.01}`
	const query = `{"start":{"type":"Customer","where":[],"ids":null,"text":null},"steps":[],"filters":[],"return":{"count":{"distinct_at":0}}}`
	old := `{"status":"answered","answer":{"blocks":[{"para":"Two customers [c1]."}]},` +
		`"citations":[{"marker":"c1","evidence_id":"e1","target":{"result":{"evidence_id":"e1"}}}],"queries":[{"evidence_id":"e1","query":` + query + `}],` + usage + `}`
	rows := `{"kind":"rows","rows":[{"cells":[{"type":"Customer","id":"acme","name":"Acme"},{"untrusted":"Gold… <b>"},1200.5,null],"edges":[]}],` +
		`"total":13,"cut":"12 rows left out"}`
	groups := `{"kind":"grouped_aggregate","groups":[{"value":{"untrusted":"2026-09"},"aggregate":5400.0,"values":4}],"total_groups":1}`
	visuals := `{"status":"answered","answer":{"blocks":[{"para":"Acme leads [c1]."},{"para":"By month:"}],` +
		`"visuals":[{"evidence_id":"e1","after":0,"kind":"table","title":"Top customers"},{"evidence_id":"e2","kind":"line"}]},` +
		`"citations":[{"marker":"c1","evidence_id":"e1","target":{"entity":{"type":"Customer","id":"acme"}}}],` +
		`"queries":[{"evidence_id":"e1","query":` + query + `,"output":` + rows + `},{"evidence_id":"e2","query":` + query + `,"output":` + groups + `},` +
		`{"evidence_id":"e3","query":` + query + `}],` + usage + `}`

	f := newQuestionFixture(t, "V1")
	thread := f.ask(t, 1, "")
	for n, result := range []string{old, visuals} {
		if n > 0 {
			f.ask(t, n+1, thread)
		}
		q, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(f.limits()))
		require.NoError(t, err)
		require.Equal(t, f.id(n+1), q.ID)
		outcome := answered(1)
		outcome.Result = json.RawMessage(result)
		require.NoError(t, f.db.SettleFoundryQuestion(ctx, q.ID, "w1", outcome))
	}
	f.ask(t, 3, thread)

	t.Run("reads a question's visuals and outputs whole", func(t *testing.T) {
		stored := f.get(t, f.id(2)).Result
		assert.JSONEq(t, visuals, string(stored))
		r, err := ontologyagent.DecodeAskResult(stored)
		require.NoError(t, err)
		require.Len(t, r.Answer.Visuals, 2)
		assert.Equal(t, "Top customers", r.Answer.Visuals[0].Title)
		assert.JSONEq(t, rows, string(r.Queries[0].Output))
		assert.JSONEq(t, groups, string(r.Queries[1].Output))
		assert.Nil(t, r.Queries[2].Output)
		earlier, err := ontologyagent.DecodeAskResult(f.get(t, f.id(1)).Result)
		require.NoError(t, err)
		assert.Nil(t, earlier.Answer.Visuals)
		assert.Nil(t, earlier.Queries[0].Output)
	})

	t.Run("lists them without their outputs", func(t *testing.T) {
		page, _, err := f.db.ListFoundryQuestions(ctx, f.ontologyID, thread, "", 10)
		require.NoError(t, err)
		require.Len(t, page, 3)
		want := strings.NewReplacer(`,"output":`+rows, "", `,"output":`+groups, "").Replace(visuals)
		assert.JSONEq(t, want, string(ontologyagent.WithoutOutputs(page[1].Result)))
		assert.Equal(t, string(page[2].Result), string(ontologyagent.WithoutOutputs(page[2].Result)), "a 3.10.0 result lists as stored")
	})

	t.Run("tells a thread's history as text", func(t *testing.T) {
		history, err := f.db.ListFoundryQuestionHistory(ctx, f.get(t, f.id(3)), 5)
		require.NoError(t, err)
		require.Len(t, history, 2)
		assert.Equal(t, "Two customers [c1].", ontologyagent.AnswerText(history[0].Result))
		// MySQL keeps a JSON document's keys in its own order; the query comes back compact.
		const stored = `{"start":{"ids":null,"text":null,"type":"Customer","where":[]},"steps":[],"return":{"count":{"distinct_at":0}},"filters":[]}`
		assert.Equal(t, "Acme leads [c1].\n\n"+
			`[Shown here, not in the text: a table titled "Top customers", of the result of the query `+stored+"]\n\nBy month:\n\n"+
			"[Shown here, not in the text: a line chart of the result of the query "+stored+"]", ontologyagent.AnswerText(history[1].Result))
	})
}

func TestFoundryQuestionLists(t *testing.T) {
	ctx := context.Background()
	f := newQuestionFixture(t, "S1")
	thread := f.ask(t, 1, "")
	f.ask(t, 2, "")
	f.ask(t, 3, thread)
	f.ask(t, 4, thread)
	// 1 is answered and 3 cannot be; 2 runs, 4 waits.
	roomy := f.limits()
	roomy.CapMicros = 10_000_000
	for range 3 {
		q, err := f.db.ClaimFoundryQuestion(ctx, "w1", f.claim(roomy))
		require.NoError(t, err)
		require.NotNil(t, q)
	}
	require.NoError(t, f.db.SettleFoundryQuestion(ctx, f.id(1), "w1", answered(1)))
	require.NoError(t, f.db.SettleFoundryQuestion(ctx, f.id(3), "w1", model.FoundryQuestionOutcome{
		Status: model.FoundryQuestionCannotAnswer, Result: json.RawMessage(`{"status":"cannot_answer"}`),
	}))

	t.Run("lists newest first, a page at a time", func(t *testing.T) {
		page, next, err := f.db.ListFoundryQuestions(ctx, f.ontologyID, "", "", 3)
		require.NoError(t, err)
		require.Len(t, page, 3)
		assert.Equal(t, f.id(4), page[0].ID)
		require.NotEmpty(t, next)
		rest, next, err := f.db.ListFoundryQuestions(ctx, f.ontologyID, "", next, 3)
		require.NoError(t, err)
		require.Len(t, rest, 1)
		assert.Equal(t, f.id(1), rest[0].ID)
		assert.Empty(t, next)
		_, _, err = f.db.ListFoundryQuestions(ctx, f.ontologyID, "", "not-a-token", 3)
		assert.ErrorIs(t, err, store.ErrInvalidPageToken)
	})

	t.Run("lists one thread", func(t *testing.T) {
		page, _, err := f.db.ListFoundryQuestions(ctx, f.ontologyID, thread, "", 10)
		require.NoError(t, err)
		ids := make([]string, 0, len(page))
		for _, q := range page {
			ids = append(ids, q.ID)
		}
		assert.Equal(t, []string{f.id(4), f.id(3), f.id(1)}, ids)
	})

	t.Run("history is the thread's answered questions before, oldest first", func(t *testing.T) {
		history, err := f.db.ListFoundryQuestionHistory(ctx, f.get(t, f.id(4)), 5)
		require.NoError(t, err)
		require.Len(t, history, 1)
		assert.Equal(t, f.id(1), history[0].ID)
		none, err := f.db.ListFoundryQuestionHistory(ctx, f.get(t, f.id(1)), 5)
		require.NoError(t, err)
		assert.Empty(t, none)
	})
}
