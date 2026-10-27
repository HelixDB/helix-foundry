// Package foundryenrichmentworker runs People enrichment's lookups. It
// claims load steps the graph worker queued (at most
// foundry.enrichment.max_running at once, one per workspace), looks their
// people up on the provider, charges the ledger, publishes the profiles and
// wakes each load. Beside them it sweeps ended steps' files and moves
// purges on. It is the only holder of the provider's key.
package foundryenrichmentworker

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/dig"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/enrichment"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	"github.com/helixdb/helix-foundry/backend/server/tasks"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const Name = "foundry-enrichment-worker"

const (
	// sweepEvery is how often ended steps' files are swept and purges moved
	// on; sweepTimeout bounds each of the two, apart.
	sweepEvery   = 10 * time.Minute
	sweepTimeout = 5 * time.Minute
	// stopWait bounds Stop: a call in flight ends within its timeout, then
	// its batch's files, charge and the lease's hand-over follow. It fits
	// the pod's 90s grace.
	stopWait = config.EnrichmentCallTimeout + 45*time.Second
)

type Deps struct {
	dig.In

	Cfg    *config.Config
	Logger *slog.Logger
	Health endpoint.Health `optional:"true"`
	DB     store.Store     `optional:"true"`
	// Plans says which workspaces are on Foundry Pro; nil where it is not sold.
	Plans ports.PlanReader `optional:"true"`
	AWS   aws.Config       `optional:"true"`
}

// steps claims and runs steps: enrichment.Worker.
type steps interface {
	Claim(ctx context.Context) (*model.FoundryLoadEnrichment, error)
	Run(stopping context.Context, step *model.FoundryLoadEnrichment, leased time.Time) error
}

// sweeper sweeps files and moves purges on: enrichment.Sweeper.
type sweeper interface {
	Files(ctx context.Context) (int, error)
	Purges(ctx context.Context) (map[model.FoundryEnrichmentPurgeStep]int, error)
}

// Task runs claimed steps side by side, each in its own goroutine.
type Task struct {
	deps    Deps
	cfg     config.EnrichmentConfig
	steps   steps
	sweeper sweeper
	now     func() time.Time
	// slots holds one token per step running here.
	slots   chan struct{}
	running sync.WaitGroup
	worker  tasks.Worker
}

func New(d Deps) (*Task, error) {
	if d.DB == nil {
		return nil, errors.New("foundry-enrichment-worker: db not configured")
	}
	cfg := d.Cfg.Foundry.Enrichment
	store := snapshot.NewS3(s3.NewFromConfig(d.AWS), d.Cfg.Foundry.Sync.DataBucket)
	w := &enrichment.Worker{
		DB: d.DB, Files: &enrichment.Files{Store: store}, Client: enrichment.NewClient(cfg), Cfg: cfg, WorkerID: id.NewWorkerID(),
		// Without Foundry plans sold no workspace is on Pro, and no lookup runs.
		Pro: d.Plans, Now: time.Now,
	}
	return &Task{
		deps: d, cfg: cfg, steps: w, now: time.Now, slots: make(chan struct{}, cfg.MaxRunning),
		sweeper: &enrichment.Sweeper{DB: d.DB, Store: store, Cfg: cfg, GraphLease: d.Cfg.Foundry.Graph.Lease, Now: time.Now, NewID: id.New},
	}, nil
}

func (t *Task) Name() string { return Name }

// Start claims steps every PollInterval while a slot is free and, beside
// them, sweeps every sweepEvery. Once stopped it claims no more and waits
// for the steps running to hand over.
func (t *Task) Start(ctx context.Context) error {
	return t.worker.Run(ctx, Name, t.cfg.HTTP.Listen, t.deps.Health, t.deps.Logger, func(ctx context.Context) error {
		var sweeping sync.WaitGroup
		sweeping.Go(func() { tasks.Tick(ctx, sweepEvery, t.sweep) })
		tasks.Tick(ctx, t.cfg.PollInterval, t.claim)
		sweeping.Wait()
		t.running.Wait()
		return nil
	})
}

// Stop is SIGTERM: no step is claimed any more, no call starts, and each
// running step settles its batch and hands its lease on, within stopWait.
// The wait is the task's own, not bounded by ctx: the process's shared
// shutdown grace is shorter, so the pod's grace period must be longer than
// both.
func (t *Task) Stop(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopWait)
	defer cancel()
	return t.worker.Stop(waitCtx)
}

// claim claims due steps while a slot is free, and runs each beside the
// others until it ends or hands over.
func (t *Task) claim(ctx context.Context) {
	for ctx.Err() == nil {
		select {
		case t.slots <- struct{}{}:
		default:
			return
		}
		leased := t.now()
		step, err := t.steps.Claim(ctx)
		if err != nil || step == nil {
			<-t.slots
			if err != nil {
				log.From(ctx).Error("claim foundry enrichment step failed", "err", err)
			}
			return
		}
		t.running.Go(func() {
			defer func() { <-t.slots }()
			defer func() {
				// The step's lease lapses and another claim resumes it.
				if r := recover(); r != nil {
					log.From(ctx).Error("foundry enrichment step panicked", "load", step.LoadID, "panic", r, "stack", string(debug.Stack()))
				}
			}()
			if err := t.steps.Run(ctx, step, leased); err != nil {
				log.From(ctx).Warn("foundry enrichment step attempt failed; it is claimed again", "load", step.LoadID, "err", err)
			}
		})
	}
}

// sweep deletes the files ended steps no longer need and moves purges on,
// each within its own sweepTimeout: a slow file sweep never leaves the
// purges, which remove withdrawn profiles, an expired context.
func (t *Task) sweep(ctx context.Context) {
	tasks.Guarded(ctx, "foundry enrichment sweep", sweepTimeout, sweeps, false, func(ctx context.Context) error {
		cleared, err := t.sweeper.Files(ctx)
		if cleared > 0 {
			log.From(ctx).Info("foundry enrichment swept", "steps", cleared)
		}
		return err
	})
	tasks.Guarded(ctx, "foundry enrichment purges", sweepTimeout, sweeps, false, func(ctx context.Context) error {
		purged, err := t.sweeper.Purges(ctx)
		if len(purged) > 0 {
			log.From(ctx).Info("foundry enrichment purges moved on", "purges", purged)
		}
		return err
	})
}
