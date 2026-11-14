// Package foundryagentworker runs the Assistant's turns (package agent): it
// claims queued turns, reserving each one's cap against its workspace's
// monthly Assistant budget, runs an attempt on the workspace's model provider
// with its person's tools, renewing its lease, and settles what it spent.
// Off (foundry.agent.enabled false) it claims nothing, so a host may list it
// everywhere.
package foundryagentworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.uber.org/dig"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	"github.com/helixdb/helix-foundry/backend/server/tasks"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const Name = "foundry-agent-worker"

const (
	// finishTimeout bounds settling an attempt, after its own context ended.
	finishTimeout = 15 * time.Second
	// maxRenewEvery is the longest gap between lease renewals: a cancel is
	// noticed at the next renewal or write, whichever comes first.
	maxRenewEvery = 10 * time.Second
)

type Deps struct {
	dig.In

	Cfg    *config.Config
	Logger *slog.Logger
	Health endpoint.Health    `optional:"true"`
	DB     foundrystore.Store `optional:"true"`
	// Plans says which workspaces' plans let them run on their own provider.
	Plans ports.PlanReader `optional:"true"`
	// Sessions opens a person's tools; required while the Assistant is on.
	Sessions integration.SessionHost `optional:"true"`
	// Databases says whose a database is, for an action on one; without
	// it, such actions are refused.
	Databases ports.Databases `optional:"true"`
}

type turnQueue interface {
	ClaimFoundryAgentTurn(ctx context.Context, workerID string, claim model.FoundryAgentClaim) (*model.FoundryAgentTurn, error)
	RenewFoundryAgentTurnLease(ctx context.Context, id, workerID string, lease time.Duration) error
	AppendFoundryAgentEvents(ctx context.Context, id, workerID string, events []json.RawMessage) error
	SettleFoundryAgentTurn(ctx context.Context, id, workerID string, outcome model.FoundryAgentTurnOutcome) error
}

// runner runs one attempt; *agent.Runner.
type runner interface {
	Run(ctx context.Context, turn *model.FoundryAgentTurn) (*agent.Attempt, error)
}

// Task runs up to foundry.agent.concurrency turns at once.
type Task struct {
	deps     Deps
	cfg      config.AssistantConfig
	queue    turnQueue
	runner   runner
	workerID string
	now      func() time.Time
	worker   tasks.Worker
	slots    chan struct{}
	running  sync.WaitGroup
}

func New(d Deps) (*Task, error) {
	cfg := d.Cfg.Foundry.Agent
	t := &Task{deps: d, cfg: cfg, workerID: id.NewWorkerID(), now: time.Now, slots: make(chan struct{}, max(cfg.Concurrency, 1))}
	if !cfg.Enabled {
		return t, nil
	}
	if d.DB == nil || d.Sessions == nil {
		return nil, errors.New("foundry-agent-worker: the store and a session host are required while foundry.agent.enabled")
	}
	resolver, err := ai.NewResolver(d.Cfg, d.DB, d.Plans)
	if err != nil {
		return nil, fmt.Errorf("foundry-agent-worker: %w", err)
	}
	catalog, err := ai.VendoredCatalog()
	if err != nil {
		return nil, fmt.Errorf("foundry-agent-worker: %w", err)
	}
	t.queue = d.DB
	t.runner = &agent.Runner{
		DB: d.DB, Sessions: d.Sessions, AI: resolver, Models: llm.New, Catalog: catalog, Keys: resolver.Keyring, Databases: d.Databases,
		Cfg: cfg, WorkerID: t.workerID, Now: time.Now,
	}
	return t, nil
}

func (t *Task) Name() string { return Name }

// Start claims turns every PollInterval while it has a free slot; off, it
// only serves its health listener.
func (t *Task) Start(ctx context.Context) error {
	return t.worker.Run(ctx, Name, t.cfg.HTTP.Listen, t.deps.Health, t.deps.Logger, func(ctx context.Context) error {
		if !t.cfg.Enabled {
			<-ctx.Done()
			return nil
		}
		tasks.Tick(ctx, t.cfg.PollInterval, t.drain)
		t.running.Wait()
		return nil
	})
}

// Stop stops claiming and cancels the turns in flight, which are given back
// to the queue, their attempt uncounted, and resume from their transcript.
func (t *Task) Stop(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*finishTimeout)
	defer cancel()
	return t.worker.Stop(waitCtx)
}

// claim is how this worker claims, under foundry.agent's limits this month.
func (t *Task) claim() model.FoundryAgentClaim {
	now := t.now().UTC()
	return model.FoundryAgentClaim{
		Runtime: model.FoundryAgentRuntimeAPI, Lease: t.cfg.Lease, Defer: t.cfg.Defer,
		Limits: model.FoundryAgentLimits{
			Since: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), CapMicros: t.cfg.MonthlyMicros(),
			PerHour: t.cfg.PerHour, Running: t.cfg.MaxRunning,
		},
	}
}

// drain claims due turns while a slot is free, each run on its own goroutine.
func (t *Task) drain(ctx context.Context) {
	for ctx.Err() == nil {
		select {
		case t.slots <- struct{}{}:
		default:
			return
		}
		turn, err := t.queue.ClaimFoundryAgentTurn(ctx, t.workerID, t.claim())
		if err != nil || turn == nil {
			<-t.slots
			if err != nil {
				log.From(ctx).Error("claim agent turn failed", "err", err)
			}
			return
		}
		t.running.Add(1)
		go func() {
			defer t.running.Done()
			defer func() { <-t.slots }()
			t.process(ctx, turn)
		}()
	}
}

// process runs one attempt while renewing its lease, then settles it. A turn
// that lost its lease settles nothing: whoever took it did.
func (t *Task) process(ctx context.Context, turn *model.FoundryAgentTurn) {
	logger := log.From(ctx).With("turn", turn.ID, "attempt", turn.Attempts)
	ctx = log.Into(ctx, logger)
	if turn.Attempts > t.cfg.MaxAttempts {
		t.end(ctx, turn, model.FoundryAgentTurnOutcome{}, fmt.Sprintf("The Assistant stopped without finishing %d times; send your message again.", turn.Attempts-1))
		return
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.cfg.AttemptTimeout())
	defer cancel()
	stopping := context.AfterFunc(ctx, cancel)
	defer stopping()
	var lost lostFlag
	stopRenewing := t.renewLease(runCtx, func() { lost.set(); cancel() }, turn.ID)
	a, err := t.runner.Run(runCtx, turn)
	stopRenewing()
	steps.Add(float64(a.Steps))
	spend.Add(float64(a.Spend.USDMicros))
	spent := model.FoundryAgentTurnOutcome{Spend: a.Spend, Steps: a.Steps}
	switch {
	case lost.get() || errors.Is(err, agent.ErrLost):
		turns.WithLabelValues("lost").Inc()
		logger.Info("agent turn: the lease was lost; nothing more is recorded")
	case err == nil:
		spent.Status = model.FoundryAgentTurnDone
		t.settle(ctx, turn, spent)
	default:
		t.fail(ctx, turn, spent, err, ctx.Err() != nil)
	}
}

// fail settles an attempt that failed: for good when its failure is
// permanent or its attempts are spent, after it says why; else a retry, which
// gives its attempt back when the worker is stopping.
func (t *Task) fail(ctx context.Context, turn *model.FoundryAgentTurn, spent model.FoundryAgentTurnOutcome, err error, stopping bool) {
	var f *agent.Failure
	if !errors.As(err, &f) {
		f = &agent.Failure{Message: "The Assistant couldn't finish; Helix will retry", Err: err}
	}
	log.From(ctx).Warn("agent turn attempt failed", "permanent", f.Permanent, "stopping", stopping, "err", err)
	retryAt := t.now().Add(t.cfg.RetryBackoff)
	switch {
	case f.Permanent:
		t.end(ctx, turn, spent, f.Message)
	case stopping:
		spent.Status, spent.RetryAt, spent.Uncounted, spent.Error = model.FoundryAgentTurnQueued, &retryAt, true, f.Message
		t.settle(ctx, turn, spent)
	case turn.Attempts < t.cfg.MaxAttempts:
		spent.Status, spent.RetryAt, spent.Error = model.FoundryAgentTurnQueued, &retryAt, f.Message
		t.settle(ctx, turn, spent)
	default:
		t.end(ctx, turn, spent, fmt.Sprintf("%s (gave up after %d attempts)", f.Message, turn.Attempts))
	}
}

// end fails the turn for good, showing its person why first.
func (t *Task) end(ctx context.Context, turn *model.FoundryAgentTurn, spent model.FoundryAgentTurnOutcome, why string) {
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if event, err := agent.MarshalEvent(agent.Finished{Status: string(model.FoundryAgentTurnFailed), Reason: agent.StopFailed, Error: why}); err == nil {
		_ = t.queue.AppendFoundryAgentEvents(endCtx, turn.ID, t.workerID, []json.RawMessage{event})
	}
	spent.Status, spent.Error = model.FoundryAgentTurnFailed, why
	t.settle(ctx, turn, spent)
}

// settle records an attempt's outcome, with a bound of its own.
func (t *Task) settle(ctx context.Context, turn *model.FoundryAgentTurn, outcome model.FoundryAgentTurnOutcome) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	err := t.queue.SettleFoundryAgentTurn(settleCtx, turn.ID, t.workerID, outcome)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		turns.WithLabelValues("lost").Inc()
	case err != nil:
		log.From(ctx).Warn("settle agent turn failed; its lease lapses and it is claimed again", "turn", turn.ID, "err", err)
	case outcome.RetryAt != nil:
		turns.WithLabelValues("retried").Inc()
	default:
		turns.WithLabelValues(string(outcome.Status)).Inc()
	}
}

// renewLease keeps the turn's lease while its attempt runs. ErrNotFound means
// a cancel, a deletion or another worker took it: lost runs. Once no renewal
// has held for two thirds of the lease, lost runs too.
func (t *Task) renewLease(ctx context.Context, lost func(), id string) (stop func()) {
	held := t.cfg.Lease * 2 / 3
	every := min(held/3, maxRenewEvery)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		fence := time.NewTimer(held)
		defer fence.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-fence.C:
				log.From(ctx).Warn("agent turn lease could lapse; stopping the attempt", "turn", id)
				lost()
				return
			case <-ticker.C:
				renewed := time.Now()
				renewCtx, cancel := context.WithTimeout(ctx, every)
				err := t.queue.RenewFoundryAgentTurnLease(renewCtx, id, t.workerID, t.cfg.Lease)
				cancel()
				switch {
				case errors.Is(err, foundrystore.ErrNotFound):
					lost()
					return
				case err != nil:
					log.From(ctx).Warn("renew agent turn lease failed", "turn", id, "err", err)
				default:
					fence.Reset(time.Until(renewed.Add(held)))
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// lostFlag records that an attempt lost its lease.
type lostFlag struct {
	mu   sync.Mutex
	lost bool
}

func (l *lostFlag) set() { l.mu.Lock(); l.lost = true; l.mu.Unlock() }

func (l *lostFlag) get() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.lost }
