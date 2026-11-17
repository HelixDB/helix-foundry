package foundryagentworker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/config"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// queue hands out its turns once each and records how they were settled.
type queue struct {
	mu       sync.Mutex
	pending  []*model.FoundryAgentTurn
	settled  map[string]model.FoundryAgentTurnOutcome
	events   map[string][]json.RawMessage
	claimed  int
	renewErr error
}

func (q *queue) ClaimFoundryAgentTurn(context.Context, string, model.FoundryAgentClaim) (*model.FoundryAgentTurn, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.claimed++
	if len(q.pending) == 0 {
		return nil, nil
	}
	t := q.pending[0]
	q.pending = q.pending[1:]
	return t, nil
}

func (q *queue) RenewFoundryAgentTurnLease(context.Context, string, string, time.Duration) error {
	return q.renewErr
}

func (q *queue) AppendFoundryAgentEvents(_ context.Context, id, _ string, events []json.RawMessage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.events[id] = append(q.events[id], events...)
	return nil
}

func (q *queue) SettleFoundryAgentTurn(_ context.Context, id, _ string, o model.FoundryAgentTurnOutcome) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := o.Validate(); err != nil {
		return err
	}
	q.settled[id] = o
	return nil
}

type fakeRunner struct{ err error }

func (r fakeRunner) Run(context.Context, *model.FoundryAgentTurn) (*agent.Attempt, error) {
	return &agent.Attempt{Steps: 2, Spend: model.FoundryGraphJobSpend{USDMicros: 700}, Stop: agent.StopAnswered}, r.err
}

func newTask(err error) (*Task, *queue) {
	q := &queue{settled: map[string]model.FoundryAgentTurnOutcome{}, events: map[string][]json.RawMessage{}}
	cfg := config.Defaults().Foundry.Agent
	cfg.Enabled = true
	return &Task{cfg: cfg, automations: config.Defaults().Foundry.Automations, queue: q, runner: fakeRunner{err: err}, workerID: "w1", now: time.Now,
		slots: make(chan struct{}, 2)}, q
}

func turn(attempts int) *model.FoundryAgentTurn {
	return &model.FoundryAgentTurn{ID: "t1", Attempts: attempts}
}

func TestProcessSettlesHowTheAttemptEnded(t *testing.T) {
	ctx := context.Background()
	t.Run("done, with what it spent", func(t *testing.T) {
		task, q := newTask(nil)
		task.process(ctx, turn(1))
		assert.Equal(t, model.FoundryAgentTurnOutcome{Status: model.FoundryAgentTurnDone, Steps: 2, Spend: model.FoundryGraphJobSpend{USDMicros: 700}}, q.settled["t1"])
	})
	t.Run("a permanent failure says why and fails", func(t *testing.T) {
		task, q := newTask(&agent.Failure{Message: "No AI provider", Permanent: true})
		task.process(ctx, turn(1))
		assert.Equal(t, model.FoundryAgentTurnFailed, q.settled["t1"].Status)
		assert.EqualValues(t, 700, q.settled["t1"].Spend.USDMicros, "what it spent is charged")
		require.Len(t, q.events["t1"], 1)
		assert.Contains(t, string(q.events["t1"][0]), `"type":"finished"`)
		assert.Contains(t, string(q.events["t1"][0]), "No AI provider")
	})
	t.Run("another failure is retried", func(t *testing.T) {
		task, q := newTask(errors.New("provider down"))
		task.process(ctx, turn(1))
		assert.NotNil(t, q.settled["t1"].RetryAt)
		assert.False(t, q.settled["t1"].Uncounted)
	})
	t.Run("until its attempts are spent", func(t *testing.T) {
		task, q := newTask(errors.New("provider down"))
		task.process(ctx, turn(task.cfg.MaxAttempts))
		assert.Equal(t, model.FoundryAgentTurnFailed, q.settled["t1"].Status)
		task.process(ctx, turn(task.cfg.MaxAttempts+1))
		assert.Contains(t, q.settled["t1"].Error, "stopped without finishing")
	})
	t.Run("a stopping worker gives the attempt back", func(t *testing.T) {
		task, q := newTask(errors.New("cancelled"))
		stopped, cancel := context.WithCancel(ctx)
		cancel()
		task.process(stopped, turn(1))
		assert.True(t, q.settled["t1"].Uncounted)
	})
	t.Run("a lost turn settles nothing", func(t *testing.T) {
		task, q := newTask(agent.ErrLost)
		task.process(ctx, turn(1))
		assert.NotContains(t, q.settled, "t1")
		task, q = newTask(nil)
		q.renewErr = foundrystore.ErrNotFound
		task.cfg.Lease = 30 * time.Millisecond
		task.runner = slowRunner{}
		task.process(ctx, turn(1))
		assert.NotContains(t, q.settled, "t1")
	})
}

// slowRunner runs until its context ends.
type slowRunner struct{}

func (slowRunner) Run(ctx context.Context, _ *model.FoundryAgentTurn) (*agent.Attempt, error) {
	<-ctx.Done()
	return &agent.Attempt{}, ctx.Err()
}

func TestDrainClaimsWhileASlotIsFree(t *testing.T) {
	task, q := newTask(nil)
	q.pending = []*model.FoundryAgentTurn{{ID: "a", Attempts: 1}, {ID: "b", Attempts: 1}}
	task.drain(context.Background())
	task.running.Wait()
	assert.Len(t, q.settled, 2)
}

func TestOffItClaimsNothing(t *testing.T) {
	cfg := config.Defaults()
	cfg.Foundry.Agent.HTTP.Listen = ""
	task, err := New(Deps{Cfg: &cfg})
	require.NoError(t, err, "off, it needs neither store nor sessions")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, task.Start(ctx))
	assert.Nil(t, task.queue)

	cfg.Foundry.Agent.Enabled = true
	_, err = New(Deps{Cfg: &cfg})
	assert.Error(t, err, "on, it needs a session host")
}

func TestClaimKeepsAutomationsApart(t *testing.T) {
	task, _ := newTask(nil)
	task.automations.MonthlyUSD, task.automations.MaxRunning = 7, 1
	claim := task.claim()
	require.Equal(t, task.cfg.MonthlyMicros(), claim.Limits.CapMicros)
	require.Equal(t, int64(7_000_000), claim.Automations.CapMicros, "automations' turns spend a budget of their own")
	require.Equal(t, 1, claim.Automations.Running)
	require.Equal(t, claim.Limits.Since, claim.Automations.Since)
}
