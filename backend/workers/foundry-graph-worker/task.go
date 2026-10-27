// Package foundrygraphworker loads Foundry ontologies' databases. It claims
// one graph job at a time, binds a loopback proxy to the ontology's tenant,
// and drives the ontology agent sidecar: a load's Ingest writes an approved
// version's tables into a new load, then Extract adds the ontology's
// documents to it; a delete's DeleteLoad removes an old load. The sidecar
// holds only its model keys; the proxy holds the database's.
package foundrygraphworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/dig"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/boost"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/enrichment"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	"github.com/helixdb/helix-foundry/backend/server/tasks"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const Name = "foundry-graph-worker"

const (
	// finishTimeout bounds recording an attempt's outcome, which runs after the
	// attempt's own context may have ended.
	finishTimeout = 15 * time.Second
	// stopMargin is what Stop allows past the drain and the finish, for the
	// lease renewal in flight and the loop's return.
	stopMargin = 5 * time.Second
)

type Deps struct {
	dig.In

	Cfg    *config.Config
	Logger *slog.Logger
	Health endpoint.Health    `optional:"true"`
	DB     foundrystore.Store `optional:"true"`
	// Boosts lift a database's rate limit while a job writes it; nil never
	// does, and foundry.graph.load_rate_limit is then refused.
	Boosts ports.Boosts `optional:"true"`
	// Databases are the databases ontologies load into.
	Databases ports.Databases `optional:"true"`
	// Plans tells People enrichment which workspaces are on Foundry Pro, and
	// the AI resolver which may read documents on their own provider.
	Plans ports.PlanReader `optional:"true"`
	AWS   aws.Config       `optional:"true"`
}

type jobQueue interface {
	ClaimFoundryGraphJob(ctx context.Context, workerID string, lease time.Duration) (*model.FoundryGraphJob, error)
	RenewFoundryGraphJobLease(ctx context.Context, id, workerID string, lease time.Duration) error
	FinishFoundryGraphJob(ctx context.Context, jobID, workerID string, outcome model.FoundryGraphJobOutcome) (model.FoundryGraphJobFinish, error)
	ParkFoundryGraphJob(ctx context.Context, jobID, workerID string, until time.Time) (bool, error)
}

// Task runs queued graph jobs one at a time.
type Task struct {
	deps  Deps
	cfg   config.GraphConfig
	queue jobQueue
	// boosts is the host's, whose lapsed ones the worker sweeps; nil has none.
	boosts   ports.Boosts
	runner   *runner
	closeFn  func() error
	workerID string
	now      func() time.Time
	worker   tasks.Worker
	// refreshLag records how long a refresh load waited for its first claim.
	refreshLag prometheus.Observer
}

func New(d Deps) (*Task, error) {
	if d.DB == nil || d.Databases == nil {
		return nil, errors.New("foundry-graph-worker: db not configured")
	}
	cfg := d.Cfg.Foundry.Graph
	if cfg.LoadRateLimit.Enabled() && d.Boosts == nil {
		return nil, errors.New("foundry-graph-worker: foundry.graph.load_rate_limit is set, but the host has no rate-limit boosts")
	}
	resolver, err := ai.NewResolver(d.Cfg, d.DB, d.Plans)
	if err != nil {
		return nil, fmt.Errorf("foundry-graph-worker: %w", err)
	}
	agent, err := ontologyagent.Dial(cfg.Agent.Addr)
	if err != nil {
		return nil, fmt.Errorf("foundry-graph-worker: %w", err)
	}
	client := s3.NewFromConfig(d.AWS)
	bucket := d.Cfg.Foundry.Sync.DataBucket
	workerID := id.NewWorkerID()
	t := &Task{deps: d, cfg: cfg, queue: d.DB, boosts: d.Boosts, closeFn: agent.Close, workerID: workerID, now: time.Now, refreshLag: refreshLag}
	snapshots := snapshot.NewS3(client, bucket)
	enrich := &enrichment.Load{DB: d.DB, Files: &enrichment.Files{Store: snapshots}, Cfg: d.Cfg.Foundry.Enrichment, Now: time.Now}
	// Without Foundry plans sold no workspace is on Pro: loads apply the
	// profiles found before, look no one up, and read documents on Helix's
	// Claude alone.
	enrich.Pro = d.Plans
	t.runner = &runner{
		db: d.DB, dbs: d.Databases, boosts: d.Boosts, agent: agent, ai: resolver, documents: document.New(client, bucket), cfg: d.Cfg, bucket: bucket, workerID: workerID, now: time.Now,
		syncWait: boostSyncWait, syncPoll: boostSyncPoll, enrichment: enrich, maxRequestBytes: ontologyagent.MaxRequestBytes,
		snapshots: &ontologyagent.SnapshotCache{
			DB: d.DB, Snapshots: snapshots, WorkDir: cfg.Agent.WorkDir, Dir: ontologyagent.GraphCache,
			MaxBytes: cfg.Agent.MaxWorkBytes, Now: time.Now,
		},
		// One request waits up to the agent's Helix timeout; the margin covers the gateway's own.
		gateway: &helixgateway.Client{
			HTTP:             helixgateway.NewHTTPClient(time.Duration(cfg.HelixTimeoutSecs)*time.Second + 10*time.Second),
			ProvisionerKey:   d.Cfg.Host.GatewayKey,
			Keyless:          d.Cfg.Host.Local,
			MaxResponseBytes: graphproxy.MaxResponseBytes,
		},
	}
	return t, nil
}

func (t *Task) Name() string { return Name }

// Start claims jobs every PollInterval and, beside them, sweeps lapsed load
// boosts every boost.SweepEvery, so a long job doesn't hold the sweep up.
// First it removes what attempts staged under the work dir's jobs/: each
// attempt removes its own as it ends, but one a worker that died (an OOM
// kill) left, documents and the profiles People enrichment chose, would stay
// on the pod's volume until the pod goes. No attempt runs here yet.
func (t *Task) Start(ctx context.Context) error {
	if err := os.RemoveAll(filepath.Join(t.cfg.Agent.WorkDir, jobsDir)); err != nil {
		return fmt.Errorf("foundry-graph-worker: remove what attempts staged: %w", err)
	}
	return t.worker.Run(ctx, Name, t.cfg.HTTP.Listen, t.deps.Health, t.deps.Logger, func(ctx context.Context) error {
		var sweeping sync.WaitGroup
		if t.boosts != nil {
			sweeping.Add(1)
			go func() {
				defer sweeping.Done()
				tasks.Tick(ctx, boost.SweepEvery, t.sweep)
			}()
		}
		tasks.Tick(ctx, t.cfg.PollInterval, t.drain)
		sweeping.Wait()
		return nil
	})
}

// Stop stops claiming and waits for the attempt in flight to end and record
// its outcome: ShutdownDrain for the agent's stream, boost.ReleaseTimeout for
// its load boost, then finishTimeout, plus stopMargin (115s by default). The wait
// is the task's own, not bounded by ctx: the process's
// shared shutdown grace (provision.shutdown_grace, 30s) is shorter than a
// drain, so the pod's grace period must be longer than both.
// The agent's connection closes last, so no running attempt loses its stream.
func (t *Task) Stop(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.cfg.ShutdownDrain+boost.ReleaseTimeout+finishTimeout+stopMargin)
	defer cancel()
	return errors.Join(t.worker.Stop(waitCtx), t.closeFn())
}

// drain runs due jobs until none is left.
func (t *Task) drain(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := t.queue.ClaimFoundryGraphJob(ctx, t.workerID, t.cfg.Lease)
		if err != nil {
			log.From(ctx).Error("claim graph job failed", "err", err)
			return
		}
		if job == nil {
			return
		}
		t.process(ctx, job)
	}
}

// process runs one attempt while renewing its lease, then records the
// outcome. Losing the lease cancels the attempt: another worker owns the job.
// When the worker stops, the agent gets ShutdownDrain to end its stream (it
// stops at its next chunk or commit and says so) before the call is
// cancelled; the outcome is recorded either way, as a retry that gives the
// attempt back.
func (t *Task) process(ctx context.Context, job *model.FoundryGraphJob) {
	// A load parked for its People enrichment lookups gave its attempt back,
	// so it is claimed again at attempt 1; by then its step's Clues pinned
	// the snapshots it reads, so only a refresh's first claim has no pins.
	refresh := job.Kind == model.FoundryGraphJobApply || job.Kind == model.FoundryGraphJobLoad && job.Origin == model.FoundryLoadByRefresh
	if refresh && job.Attempts == 1 && job.Sources == nil {
		t.refreshLag.Observe(t.now().Sub(job.CreatedAt).Seconds())
	}
	// A job claimed again after its lease ran out has had no outcome for that
	// attempt; one that keeps killing the pod stops here rather than loop.
	if job.Attempts > t.cfg.MaxAttempts {
		t.finish(ctx, job, model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobFailed, Progress: job.Progress,
			Error: fmt.Sprintf("the load stopped without finishing %d times; Helix gave up", job.Attempts-1),
		})
		return
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.cfg.Timeout)
	defer cancel()
	stopDrain := context.AfterFunc(ctx, func() {
		timer := time.NewTimer(t.cfg.ShutdownDrain)
		defer timer.Stop()
		select {
		case <-timer.C:
			cancel()
		case <-runCtx.Done():
		}
	})
	defer stopDrain()
	stopRenewing := t.renewLease(runCtx, cancel, job.ID)
	start := t.now()
	a, err := t.runner.run(runCtx, job)
	stopRenewing()

	var wait *parked
	ended := "parked"
	if errors.As(err, &wait) {
		t.park(ctx, job, wait.until)
	} else {
		ended = t.finish(ctx, job, t.outcome(runCtx, job, a, err, ctx.Err() != nil))
	}
	seconds := t.now().Sub(start).Seconds()
	jobSeconds.WithLabelValues(string(job.Kind), string(job.Origin), ended).Observe(seconds)
	if job.Kind == model.FoundryGraphJobApply {
		countApply(ended, a, err)
	}
	log.From(ctx).Info("graph job attempt ended", "job", job.ID, "kind", job.Kind, "origin", job.Origin, "outcome", ended,
		"seconds", seconds, "rows", a.rows, "chunks", a.progress.ChunksDone, "documents", a.progress.DocumentsDone)
}

// countApply counts an apply job that ended: done once its tables are in.
// An attempt that is retried, parked or not recorded counts nothing.
func countApply(ended string, a *attempt, err error) {
	switch model.FoundryGraphJobStatus(ended) {
	case model.FoundryGraphJobSucceeded, model.FoundryGraphJobBudgetExhausted, model.FoundryGraphJobFailed, model.FoundryGraphJobCancelled:
	default:
		return
	}
	var f *failure
	switch {
	case a.ingestDone:
		applies.WithLabelValues("done").Inc()
	case errors.As(err, &f) && f.Unsupported:
		applies.WithLabelValues("unsupported").Inc()
	default:
		applies.WithLabelValues("failed").Inc()
	}
	for _, s := range a.applied {
		applyRows.WithLabelValues("added").Add(float64(s.Added))
		applyRows.WithLabelValues("changed").Add(float64(s.Changed))
		applyRows.WithLabelValues("removed").Add(float64(s.Removed))
	}
}

// park ends an attempt whose load waits for its People enrichment step: the
// job goes back to the queue, due at until (sooner if the step ends first,
// which wakes it), at provisioning, the attempt given back. It runs after
// the attempt's context may have ended, with a bound of its own. A park that
// fails leaves the lease to run out, and the job is claimed again then.
func (t *Task) park(ctx context.Context, job *model.FoundryGraphJob, until time.Time) {
	parkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	due, err := t.queue.ParkFoundryGraphJob(parkCtx, job.ID, t.workerID, until)
	if err != nil {
		log.From(ctx).Warn("park graph job failed", "job", job.ID, "err", err)
		return
	}
	jobs.WithLabelValues("parked").Inc()
	log.From(ctx).Info("graph job parked for its people lookups", "job", job.ID, "until", until, "due", due)
}

// finish records an attempt's outcome. It runs after the attempt's context
// may have ended, the worker's included, so it has a bound of its own. A job
// that ends hands over what may follow it: the ids of an extract it owes
// (queued within this month's budget), of the next delete, or of the full
// refresh load a failed apply hands over to, and how many loads the ontology
// keeps. It returns the outcome it counted, or "unrecorded" when the finish
// didn't land.
func (t *Task) finish(ctx context.Context, job *model.FoundryGraphJob, outcome model.FoundryGraphJobOutcome) string {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	outcome.Next = model.FoundryGraphNext{
		ExtractID: id.New(), DeleteID: id.New(), RefreshID: id.New(), RetainLoads: t.cfg.RetainLoads, Budget: t.cfg.MonthBudget(t.now()),
	}
	finish, err := t.queue.FinishFoundryGraphJob(finishCtx, job.ID, t.workerID, outcome)
	if err != nil {
		log.From(ctx).Warn("finish graph job failed", "job", job.ID, "err", err)
		return "unrecorded"
	}
	ended := string(outcome.Status)
	switch {
	case finish == model.FoundryGraphJobMerged:
		ended = "merged"
	case outcome.RetryAt != nil:
		ended = "retried"
	}
	jobs.WithLabelValues(ended).Inc()
	if job.Kind != model.FoundryGraphJobDelete {
		return ended
	}
	switch {
	case outcome.RetryAt != nil:
		loadDeletes.WithLabelValues("retried").Inc()
	case outcome.Status == model.FoundryGraphJobSucceeded:
		loadDeletes.WithLabelValues("done").Inc()
	default:
		// The load is delete_failed and stays in the database until the user deletes it again, or, for a deleted
		// ontology, which holds its database meanwhile, asks for the database again.
		loadDeletes.WithLabelValues("failed").Inc()
		log.From(ctx).Error("graph job: a load could not be deleted", "job", job.ID, "ontology", job.OntologyID, "load", job.LoadID,
			"err", outcome.Error)
	}
	return ended
}

// renewLease keeps the job's lease while the attempt runs, and fences the
// attempt on it. ErrNotFound means another worker owns the job: the attempt
// is cancelled. Any other failure (the database unreachable) leaves the lease
// running out, and a replacement worker may claim the job once it has: so
// once no renewal has held for Lease less a Helix timeout, the attempt is
// cancelled too, which ends its proxy. A write admitted before then lands
// before the lease could lapse; none is admitted after. Renewals come every
// third of that, so two in a row may fail before the fence, and none may
// outlast it: a database that hangs is one that fails. The claim's lease
// began just before the attempt.
func (t *Task) renewLease(ctx context.Context, cancel context.CancelFunc, jobID string) (stop func()) {
	held := t.cfg.Lease - time.Duration(t.cfg.HelixTimeoutSecs)*time.Second
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(held / 3)
		defer ticker.Stop()
		deadline := time.Now().Add(held)
		fence := time.NewTimer(held)
		defer fence.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-fence.C:
				log.From(ctx).Warn("graph job lease could lapse; stopping the attempt", "job", jobID)
				cancel()
				return
			case <-ticker.C:
				renewed := time.Now()
				renewCtx, cancelRenew := context.WithDeadline(ctx, deadline)
				err := t.queue.RenewFoundryGraphJobLease(renewCtx, jobID, t.workerID, t.cfg.Lease)
				cancelRenew()
				switch {
				case errors.Is(err, foundrystore.ErrNotFound):
					log.From(ctx).Warn("graph job lease lost", "job", jobID)
					cancel()
					return
				case err != nil:
					log.From(ctx).Warn("renew graph job lease failed", "job", jobID, "err", err)
				default:
					deadline = renewed.Add(held)
					fence.Reset(time.Until(deadline))
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// outcome decides what an attempt leaves behind: success, a budget stop
// (with the sets the attempt held for their allowance), a retry after a
// backoff that doubles from RetryBackoff, or a failure for good.
// The backoff's floor outlasts two Helix timeouts, so a write that timed out
// has settled before the retry reads the database. An attempt that would be
// retried while the worker is stopping was stopped, not failed, and one that
// found an index still building did nothing wrong: either is retried after
// the floor and given back, whatever attempt it was.
func (t *Task) outcome(ctx context.Context, job *model.FoundryGraphJob, a *attempt, err error, stopping bool) model.FoundryGraphJobOutcome {
	outcome := model.FoundryGraphJobOutcome{IngestDone: a.ingestDone, Report: a.report(), Progress: a.progress}
	if err == nil {
		outcome.Status = model.FoundryGraphJobSucceeded
		return outcome
	}
	var f *failure
	if !errors.As(err, &f) {
		f = &failure{Message: "the load failed; Helix will retry", Err: err}
	}
	log.From(ctx).Warn("graph job attempt failed", "job", job.ID, "ontology", job.OntologyID, "attempt", job.Attempts,
		"permanent", f.Permanent, "budget", f.Budget, "stopping", stopping, "err", err)
	switch {
	case f.Budget:
		outcome.Status, outcome.Error, outcome.HeldSets = model.FoundryGraphJobBudgetExhausted, f.Message, a.held
	case f.Permanent:
		outcome.Status, outcome.Error = model.FoundryGraphJobFailed, f.Message
	case stopping || f.Uncounted:
		retryAt := t.now().Add(t.cfg.RetryBackoff)
		outcome.RetryAt, outcome.Uncounted, outcome.Error = &retryAt, true, f.Message
	case job.Attempts < t.cfg.MaxAttempts:
		retryAt := t.now().Add(t.cfg.RetryBackoff << (job.Attempts - 1))
		outcome.RetryAt, outcome.Error = &retryAt, f.Message
	default:
		outcome.Status, outcome.Error = model.FoundryGraphJobFailed, fmt.Sprintf("%s (gave up after %d attempts)", f.Message, job.Attempts)
	}
	return outcome
}
