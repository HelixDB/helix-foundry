// Package foundryhealworker audits ontologies' current loads with the
// ontology agent's Audit and stores what it finds for people to see. Each
// tick it finds the loads due an audit (store.DueFoundryHealOntologies: one
// as it becomes current, then nightly, never while a graph job is live on
// its ontology, the stalest first), holds the ontology's one running run
// (foundry_heal_runs) while the load is still current, pages Audit over
// the load's documents with reviews by id, and sums each finding's backing
// over the pages. A page the agent's 60 s wall cuts short is audited again
// at half the size. After each page the run checks its load is still
// current with no graph job started since, and stops otherwise. A run
// whose every page was read whole stores its findings
// (store.PublishFoundryHealRun).
//
// It runs as a task beside the foundry-graph-worker, on its ontology agent
// sidecar (foundry.graph.agent). With foundry.heal.enabled off it is a
// task that does nothing, so a host may list it everywhere. It reads each
// ontology's database through a read-only proxy of its own, calls no model
// and writes no graph. It serves no HTTP of its own: the pod's health and
// metrics are the graph worker's listener's.
//
// Known limits:
//   - Source drift: Foundry stores no source fields, so it sends the
//     engine none (ontologyagent.AuditRows.SourceDrift is always empty) and
//     no audit checks drift, though the engine reports the rows measured.
//     The API and the Suggestions page claim no drift coverage.
//   - Scale: a run makes one Audit call per 1,000 reviewed documents. Its
//     memory is one small sum per distinct finding, and the examples of
//     those two documents back (ontologyagent.AuditSums). 1M reviewed
//     documents take about 17 minutes at 1 s a page, and hours where the
//     database's plan rate limit is 1 request a second: audits get no
//     boost. A run stops at maxRunTime, so a load whose audit takes longer
//     never completes.
//   - Busy ontologies: a run stops once a graph job starts on its ontology,
//     so one whose jobs come more often than an audit takes (a connector
//     syncing changes every few minutes) may never complete. Audits that
//     resume across runs would lift this and the cap.
package foundryhealworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.uber.org/dig"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	"github.com/helixdb/helix-foundry/backend/server/tasks"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const Name = "foundry-heal-worker"

// dueBatch is the most loads one tick audits; any more are found on the
// next.
const dueBatch = 50

// Deps are what the worker is built from.
type Deps struct {
	dig.In

	Cfg    *config.Config
	Logger *slog.Logger
	Health endpoint.Health `optional:"true"`
	DB     store.Store     `optional:"true"`
	// Databases are the databases the audits read.
	Databases ports.Databases `optional:"true"`
}

// healStore is foundry_heal_runs and foundry_heal_findings, and what a run
// reads of a load.
type healStore interface {
	DueFoundryHealOntologies(ctx context.Context, every, retryAfter time.Duration, limit int) ([]model.FoundryHealDue, error)
	StartFoundryHealRun(ctx context.Context, run *model.FoundryHealRun, lease time.Duration) error
	RenewFoundryHealRun(ctx context.Context, id string, lease time.Duration) error
	FinishFoundryHealRun(ctx context.Context, id string, status model.FoundryHealRunStatus, pages, cuts int) error
	PublishFoundryHealRun(ctx context.Context, p model.FoundryHealPublish) error
	ListFoundryHealDocuments(ctx context.Context, loadID, after string, limit int) ([]string, error)
	FoundryHealLoadChanged(ctx context.Context, runID, ontologyID, loadID string) (bool, error)
	GetOntologyVersion(ctx context.Context, ontologyID, id string) (*model.OntologyVersion, error)
	GetFoundryGraphJob(ctx context.Context, ontologyID, id string) (*model.FoundryGraphJob, error)
}

// auditor is the ontology agent sidecar.
type auditor interface {
	Info(ctx context.Context) (*agentv1.GetInfoResponse, error)
	Audit(ctx context.Context, request []byte) (json.RawMessage, error)
}

// target is where a run's audits read an ontology's database: the database
// Audit names, its read-only Helix target, and the end of the run's
// registration.
type target struct {
	database string
	helix    ontologyagent.HelixTarget
	close    func()
}

// Task audits the loads due one at a time.
type Task struct {
	deps   Deps
	cfg    config.HealConfig
	runner *runner
	// closeAgent releases the agent client and the read-only proxy.
	closeAgent func() error
	worker     tasks.Worker
}

// New is the worker on d's store and databases, on foundry.graph's
// sidecar. With foundry.heal.enabled off it is a task that waits for Stop
// and does nothing else: no audit runs, no finding is stored, and it needs
// no store.
func New(d Deps) (*Task, error) {
	if !d.Cfg.Foundry.Heal.Enabled {
		return &Task{deps: d, closeAgent: func() error { return nil }}, nil
	}
	if d.DB == nil || d.Databases == nil {
		return nil, errors.New("foundry-heal-worker: db not configured")
	}
	cfg := d.Cfg.Foundry.Heal
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("foundry-heal-worker: %w", err)
	}
	agent, err := ontologyagent.Dial(d.Cfg.Foundry.Graph.Agent.Addr)
	if err != nil {
		return nil, fmt.Errorf("foundry-heal-worker: %w", err)
	}
	// One read waits up to the agent's Helix timeout; the margin covers the
	// gateway's own. A read the gateway turned away is sent again for a
	// third of it.
	helixTimeout := time.Duration(d.Cfg.Foundry.Graph.HelixTimeoutSecs) * time.Second
	proxy, err := graphproxy.ServeReadOnly(&helixgateway.Client{
		HTTP:             helixgateway.NewHTTPClient(helixTimeout + 10*time.Second),
		ProvisionerKey:   d.Cfg.Host.GatewayKey,
		Keyless:          d.Cfg.Host.Local,
		MaxResponseBytes: graphproxy.MaxResponseBytes,
	}, helixTimeout/3)
	if err != nil {
		_ = agent.Close()
		return nil, fmt.Errorf("foundry-heal-worker: %w", err)
	}
	open := func(ctx context.Context, due model.FoundryHealDue) (target, error) {
		bind := func(ctx context.Context) (graphproxy.Binding, error) {
			return graphproxy.BindRead(ctx, d.DB, d.Databases, due.WorkspaceID, due.OntologyID)
		}
		binding, err := bind(ctx)
		if err != nil {
			return target{}, err
		}
		// The run's token, and its reads in flight, end with callCtx; a
		// database paused, deleted or moved meanwhile takes no more of them.
		callCtx, endCall := context.WithCancel(ctx)
		token, err := proxy.Register(callCtx, binding, func(ctx context.Context) error {
			fresh, err := bind(ctx)
			if err == nil && fresh != binding {
				return status.Error(codes.FailedPrecondition, "ontology binding changed")
			}
			return err
		})
		if err != nil {
			endCall()
			return target{}, err
		}
		return target{database: binding.TenantID, close: endCall, helix: ontologyagent.HelixTarget{
			URL: proxy.URL(), Token: token, MaxRequestBytes: graphproxy.MaxRequestBytes, TimeoutSecs: uint64(d.Cfg.Foundry.Graph.HelixTimeoutSecs),
		}}, nil
	}
	return &Task{
		deps: d, cfg: cfg,
		runner: &runner{db: d.DB, agent: agent, open: open, cfg: cfg, pageSize: ontologyagent.MaxAuditDocuments, maxRun: maxRunTime,
			newID: id.New},
		closeAgent: func() error { return errors.Join(proxy.Close(), agent.Close()) },
	}, nil
}

func (t *Task) Name() string { return Name }

// Start audits until Stop; off, it only waits for Stop.
func (t *Task) Start(ctx context.Context) error {
	return t.worker.Run(ctx, Name, "", t.deps.Health, t.deps.Logger, func(ctx context.Context) error {
		if t.runner == nil {
			<-ctx.Done()
			return nil
		}
		tasks.Tick(ctx, t.cfg.PollInterval, t.runner.tick)
		return nil
	})
}

func (t *Task) Stop(ctx context.Context) error {
	return errors.Join(t.worker.Stop(ctx), t.closeAgent())
}

// tick audits the loads due, one at a time, the stalest first, until its
// list is older than the poll interval: the next tick lists them again, so
// a load just made current waits for one run at most, not every one
// queued.
func (r *runner) tick(ctx context.Context) {
	listed := time.Now()
	due, err := r.db.DueFoundryHealOntologies(ctx, r.cfg.Every, r.cfg.RetryAfter, dueBatch)
	if err != nil {
		log.From(ctx).Error("find loads due an audit failed", "err", err)
		return
	}
	for _, d := range due {
		if ctx.Err() != nil {
			return
		}
		r.run(ctx, d)
		if time.Since(listed) > r.cfg.PollInterval {
			return
		}
	}
}
