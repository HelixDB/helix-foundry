// Package foundryaskworker answers questions asked of Foundry ontologies'
// loads. It claims queued questions, reserving each one's cap against its
// workspace's monthly questions budget, registers the ontology's tenant on
// the pod's read-only loopback proxy for the question, lifts the database's
// rate limit for the question, and runs the ontology agent sidecar's Ask on
// the workspace's AI setting: its events are stored as they come, then its
// result settles the question and its spend. The sidecar holds no key: each
// call carries its model key, resolved for the attempt; the proxy holds the
// database's, and forwards reads only.
package foundryaskworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.uber.org/dig"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/boost"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	"github.com/helixdb/helix-foundry/backend/server/tasks"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const Name = "foundry-ask-worker"

const (
	// finishTimeout bounds storing an attempt's last events and settling it,
	// which run after the attempt's own context may have ended.
	finishTimeout = 15 * time.Second
	// maxRenewEvery is the longest gap between lease renewals: a cancel is
	// noticed at the next renewal or event flush, whichever comes first.
	maxRenewEvery = 10 * time.Second
	// stopMargin is what Stop allows past the attempts' finish.
	stopMargin = 5 * time.Second
)

type Deps struct {
	dig.In

	Cfg    *config.Config
	Logger *slog.Logger
	Health endpoint.Health    `optional:"true"`
	DB     foundrystore.Store `optional:"true"`
	// Boosts lift a database's rate limit while a question reads it; nil
	// never does, and foundry.graph.load_rate_limit is then refused.
	Boosts ports.Boosts `optional:"true"`
	// Databases are the databases questions read.
	Databases ports.Databases `optional:"true"`
	// Plans says which workspaces' plans let them ask on their own provider.
	Plans ports.PlanReader `optional:"true"`
}

type questionQueue interface {
	ClaimFoundryQuestion(ctx context.Context, workerID string, claim model.FoundryQuestionClaim) (*model.FoundryQuestion, error)
	RenewFoundryQuestionLease(ctx context.Context, id, workerID string, lease time.Duration) error
	SettleFoundryQuestion(ctx context.Context, id, workerID string, outcome model.FoundryQuestionOutcome) error
}

// Task runs up to foundry.ask.concurrency questions at once and, beside
// them, one probe of saved AI settings at a time (probe.go).

type Task struct {
	deps  Deps
	cfg   config.AskConfig
	queue questionQueue
	// boosts is the host's, whose lapsed ones the worker sweeps; nil has none.
	boosts ports.Boosts
	runner *runner
	prober *prober
	// probeEvery is how often a probe is claimed (foundry.ai.probe.poll_interval).
	probeEvery time.Duration
	closeFn    func() error
	workerID   string
	now        func() time.Time
	worker     tasks.Worker
	// slots holds one token per running question.
	slots   chan struct{}
	running sync.WaitGroup
}

func New(d Deps) (*Task, error) {
	if d.DB == nil || d.Databases == nil {
		return nil, errors.New("foundry-ask-worker: db not configured")
	}
	if d.Cfg.Foundry.Graph.LoadRateLimit.Enabled() && d.Boosts == nil {
		return nil, errors.New("foundry-ask-worker: foundry.graph.load_rate_limit is set, but the host has no rate-limit boosts")
	}
	cfg := d.Cfg.Foundry.Ask
	resolver, err := ai.NewResolver(d.Cfg, d.DB, d.Plans)
	if err != nil {
		return nil, fmt.Errorf("foundry-ask-worker: %w", err)
	}
	agent, err := ontologyagent.Dial(cfg.Agent.Addr)
	if err != nil {
		return nil, fmt.Errorf("foundry-ask-worker: %w", err)
	}
	helixTimeout := time.Duration(cfg.HelixTimeoutSecs) * time.Second
	// One read waits up to the agent's Helix timeout; the margin covers the
	// gateway's own. A read the gateway turned away is sent again for a third
	// of it.
	proxy, err := graphproxy.ServeReadOnly(&helixgateway.Client{
		HTTP:             helixgateway.NewHTTPClient(helixTimeout + 10*time.Second),
		ProvisionerKey:   d.Cfg.Host.GatewayKey,
		Keyless:          d.Cfg.Host.Local,
		MaxResponseBytes: graphproxy.MaxResponseBytes,
	}, helixTimeout/3)
	if err != nil {
		_ = agent.Close()
		return nil, fmt.Errorf("foundry-ask-worker: %w", err)
	}
	workerID := id.NewWorkerID()
	probe := d.Cfg.Foundry.AI.Probe
	return &Task{
		deps: d, cfg: cfg, queue: d.DB, boosts: d.Boosts, workerID: workerID, now: time.Now, slots: make(chan struct{}, cfg.Concurrency),
		closeFn: func() error { return errors.Join(proxy.Close(), agent.Close()) },
		runner:  &runner{db: d.DB, dbs: d.Databases, boosts: d.Boosts, agent: agent, ai: resolver, proxy: proxy, cfg: d.Cfg, workerID: workerID, flushEvery: flushEvery},
		prober: &prober{
			queue: d.DB, agent: agent, keys: resolver.Keyring, deployment: resolver.Deployment, cfg: probe, owner: workerID,
		},
		probeEvery: probe.PollInterval,
	}, nil
}

func (t *Task) Name() string { return Name }

// Start claims questions every PollInterval while it has a free slot and,
// beside them, sweeps lapsed boosts every boost.SweepEvery and probes saved
// AI settings, one at a time, every foundry.ai.probe.poll_interval.
func (t *Task) Start(ctx context.Context) error {
	return t.worker.Run(ctx, Name, t.cfg.HTTP.Listen, t.deps.Health, t.deps.Logger, func(ctx context.Context) error {
		var beside sync.WaitGroup
		if t.boosts != nil {
			beside.Add(1)
			go func() {
				defer beside.Done()
				tasks.Tick(ctx, boost.SweepEvery, func(ctx context.Context) { boost.Sweep(ctx, t.boosts) })
			}()
		}
		beside.Add(1)
		go func() {
			defer beside.Done()
			tasks.Tick(ctx, t.probeEvery, t.prober.tick)
		}()
		tasks.Tick(ctx, t.cfg.PollInterval, t.drain)
		beside.Wait()
		t.running.Wait()
		return nil
	})
}

// Stop stops claiming and cancels the questions in flight, which are given
// back to the queue (their attempt uncounted), then closes the read-only
// proxy and the agent's connection. The wait is the task's own, not bounded
// by ctx.
func (t *Task) Stop(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), boost.ReleaseTimeout+2*finishTimeout+stopMargin)
	defer cancel()
	return errors.Join(t.worker.Stop(waitCtx), t.closeFn())
}

// claim is how this worker claims, under foundry.ask's limits this month.
func (t *Task) claim() model.FoundryQuestionClaim {
	now := t.now().UTC()
	return model.FoundryQuestionClaim{
		Lease: t.cfg.Lease, Defer: t.cfg.Defer,
		Limits: model.FoundryQuestionLimits{
			Since: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), CapMicros: t.cfg.MonthlyMicros(),
			PerHour: t.cfg.PerHour, Running: t.cfg.MaxRunning,
		},
	}
}

// drain claims due questions while a slot is free, each run on its own goroutine.
func (t *Task) drain(ctx context.Context) {
	for ctx.Err() == nil {
		select {
		case t.slots <- struct{}{}:
		default:
			return
		}
		q, err := t.queue.ClaimFoundryQuestion(ctx, t.workerID, t.claim())
		if err != nil || q == nil {
			<-t.slots
			if err != nil {
				log.From(ctx).Error("claim question failed", "err", err)
			}
			return
		}
		t.running.Add(1)
		go func() {
			defer t.running.Done()
			defer func() { <-t.slots }()
			t.process(ctx, q)
		}()
	}
}

// process runs one attempt while renewing its lease, then settles it. A
// question that lost its lease (cancelled, or another worker owns it) is
// stopped and settles nothing: the cancel or the other worker did.
func (t *Task) process(ctx context.Context, q *model.FoundryQuestion) {
	logger := log.From(ctx).With("question", q.ID, "attempt", q.Attempts)
	ctx = log.Into(ctx, logger)
	if q.Attempts > t.cfg.MaxAttempts {
		t.settle(ctx, q, model.FoundryQuestionOutcome{
			Status: model.FoundryQuestionFailed, Error: fmt.Sprintf("the question stopped without finishing %d times; ask it again", q.Attempts-1),
		})
		return
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.cfg.AttemptTimeout())
	defer cancel()
	stopping := context.AfterFunc(ctx, cancel)
	defer stopping()
	var lost lostFlag
	stopRenewing := t.renewLease(runCtx, func() { lost.set(); cancel() }, q.ID)
	a, err := t.runner.run(runCtx, q, func() { lost.set(); cancel() })
	stopRenewing()
	if lost.get() {
		questions.WithLabelValues("lost").Inc()
		logger.Info("question: the lease was lost, to a cancel or another worker; nothing more is recorded")
		return
	}
	t.settle(ctx, q, t.outcome(runCtx, q, a, err, ctx.Err() != nil))
}

// lostFlag records that an attempt lost its lease.
type lostFlag struct {
	mu   sync.Mutex
	lost bool
}

func (l *lostFlag) set() { l.mu.Lock(); l.lost = true; l.mu.Unlock() }

func (l *lostFlag) get() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.lost }

// settle records an attempt's outcome, with a bound of its own.
func (t *Task) settle(ctx context.Context, q *model.FoundryQuestion, outcome model.FoundryQuestionOutcome) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	err := t.queue.SettleFoundryQuestion(settleCtx, q.ID, t.workerID, outcome)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		questions.WithLabelValues("lost").Inc()
		return
	case err != nil:
		log.From(ctx).Warn("settle question failed; its lease lapses and it is claimed again", "question", q.ID, "err", err)
		return
	case outcome.RetryAt != nil:
		questions.WithLabelValues("retried").Inc()
	default:
		questions.WithLabelValues(string(outcome.Status)).Inc()
	}
}

// renewLease keeps the question's lease while the attempt runs. ErrNotFound
// means a cancel or another worker took it: lost runs, which stops the
// attempt. Any other failure leaves the lease running out; once no renewal
// has held for Lease less a Helix timeout, lost runs too, so no read of the
// attempt outlives its lease by more than one Helix timeout.
func (t *Task) renewLease(ctx context.Context, lost func(), id string) (stop func()) {
	held := t.cfg.Lease - time.Duration(t.cfg.HelixTimeoutSecs)*time.Second
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
				log.From(ctx).Warn("question lease could lapse; stopping the attempt", "question", id)
				lost()
				return
			case <-ticker.C:
				renewed := time.Now()
				renewCtx, cancel := context.WithTimeout(ctx, every)
				err := t.queue.RenewFoundryQuestionLease(renewCtx, id, t.workerID, t.cfg.Lease)
				cancel()
				switch {
				case errors.Is(err, foundrystore.ErrNotFound):
					lost()
					return
				case err != nil:
					log.From(ctx).Warn("renew question lease failed", "question", id, "err", err)
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

// outcome decides what an attempt leaves behind: its result, a failure for
// good, or a retry after RetryBackoff. An attempt that would be retried
// while the worker is stopping was stopped, not failed, and one the agent
// turned away for being busy did nothing: either gives its attempt back.
func (t *Task) outcome(ctx context.Context, q *model.FoundryQuestion, a *attempt, err error, stopping bool) model.FoundryQuestionOutcome {
	if err == nil {
		return a.outcome
	}
	outcome := model.FoundryQuestionOutcome{Result: a.outcome.Result, Spend: a.outcome.Spend}
	var f *failure
	if !errors.As(err, &f) {
		f = &failure{Message: "the question could not be answered; Helix will retry", Err: err}
	}
	log.From(ctx).Warn("question attempt failed", "question", q.ID, "attempt", q.Attempts, "permanent", f.Permanent, "stopping", stopping,
		"err", err)
	retryAt := t.now().Add(t.cfg.RetryBackoff)
	switch {
	case f.Permanent:
		outcome.Status, outcome.Error = model.FoundryQuestionFailed, f.Message
	case stopping || f.Uncounted:
		outcome.Status, outcome.RetryAt, outcome.Uncounted, outcome.Error = model.FoundryQuestionQueued, &retryAt, true, f.Message
	case q.Attempts < t.cfg.MaxAttempts:
		outcome.Status, outcome.RetryAt, outcome.Error = model.FoundryQuestionQueued, &retryAt, f.Message
	default:
		outcome.Status, outcome.Error = model.FoundryQuestionFailed, fmt.Sprintf("%s (gave up after %d attempts)", f.Message, q.Attempts)
	}
	return outcome
}
