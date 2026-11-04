package foundryhealworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// agentReadyWait is how long a run waits for a sidecar still starting.
	agentReadyWait = 30 * time.Second
	// auditWait bounds one page's Audit: the agent's 60 s wall, then the
	// reads in flight and the result.
	auditWait = 2 * time.Minute
	// finishWait is how long recording a run's end may take, and how much
	// longer a publish goes on once the worker stops.
	finishWait = 30 * time.Second
	// publishBatchWait is how long a publish may take for each batch of
	// store.HealPublishBatch findings, past finishWait.
	publishBatchWait = 10 * time.Second
	// maxRunTime caps a run: past it, the run ends incomplete at its next
	// page, so one load holds the worker no longer than this and the
	// stalest load due goes next. A load whose audit needs longer never
	// completes.
	maxRunTime = 4 * time.Hour
)

// runner audits one load at a time.
type runner struct {
	db    healStore
	agent auditor
	// open registers a run's reads of an ontology's database.
	open func(ctx context.Context, due model.FoundryHealDue) (target, error)
	cfg  config.HealConfig
	// pageSize is a page's documents, before a cut halves it; maxRun is
	// maxRunTime.
	pageSize int
	maxRun   time.Duration
	newID    func() string
}

// audited is what a run's pages found.
type audited struct {
	pages, cuts  int
	sums         ontologyagent.AuditSums
	rowsMeasured bool
	// stopped is set when the run ended before its last page: past maxRun,
	// at a page of one document still cut short, or once its load changed
	// under it. It stores nothing.
	stopped bool
}

// run audits due's load as the ontology's one running run, then ends the
// run: complete, storing its findings, once every page was read whole;
// incomplete when even a page of one document was cut, the run ran out of
// time, or a graph job changed the load under it; failed otherwise,
// including when storing its findings failed. A run another worker holds
// is left to it, and one whose load is no longer due isn't started; one
// whose lease is lost, as when it lapsed and another worker took the
// ontology, records nothing more. The lease is renewed until the run ends,
// through storing its findings.
func (r *runner) run(ctx context.Context, due model.FoundryHealDue) {
	run := &model.FoundryHealRun{ID: r.newID(), OntologyID: due.OntologyID, LoadID: due.Load.ID}
	switch err := r.db.StartFoundryHealRun(ctx, run, r.cfg.Lease); {
	case errors.Is(err, store.ErrConflict):
		runs.WithLabelValues("held").Inc()
		return
	case errors.Is(err, store.ErrStale):
		runs.WithLabelValues("stale").Inc()
		return
	case err != nil:
		log.From(ctx).Error("start heal run failed", "ontology", due.OntologyID, "err", err)
		return
	}
	logger := log.From(ctx).With("run", run.ID, "ontology", due.OntologyID, "load", due.Load.ID)
	started := time.Now()
	// The audit ends with the worker; recording the run's end, a little
	// after (finishWait), so a stop leaves no run unrecorded. Both end at
	// once when the lease is lost.
	endCtx, cancelEnd := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelEnd()
	stopAfter := context.AfterFunc(ctx, func() { time.AfterFunc(finishWait, cancelEnd) })
	defer stopAfter()
	auditCtx, cancelAudit := context.WithCancel(ctx)
	defer cancelAudit()
	lost, stopRenewing := r.renew(endCtx, func() { cancelAudit(); cancelEnd() }, run.ID)
	a, err := r.audit(auditCtx, due, run.ID, started)
	cancelAudit()

	outcome := "complete"
	finish := func(status model.FoundryHealRunStatus) error {
		finishCtx, cancel := context.WithTimeout(endCtx, finishWait)
		defer cancel()
		return r.db.FinishFoundryHealRun(finishCtx, run.ID, status, a.pages, a.cuts)
	}
	switch {
	case lost.Load():
		outcome = "lost"
		err = nil
	case err != nil:
		outcome = "failed"
		logger.Warn("heal run failed", "pages", a.pages, "err", err)
		err = finish(model.FoundryHealRunFailed)
	case a.stopped:
		outcome = "incomplete"
		err = finish(model.FoundryHealRunIncomplete)
	default:
		var seen []model.FoundryHealSeen
		if seen, err = a.sums.Seen(); err == nil {
			batches := (len(seen) + store.HealPublishBatch - 1) / store.HealPublishBatch
			publishCtx, cancel := context.WithTimeout(endCtx, finishWait+time.Duration(batches)*publishBatchWait)
			err = r.db.PublishFoundryHealRun(publishCtx, model.FoundryHealPublish{
				RunID: run.ID, OntologyID: due.OntologyID, LoadID: due.Load.ID, VersionID: due.Load.VersionID, Pages: a.pages, Cuts: a.cuts,
				Findings: seen, RowsMeasured: a.rowsMeasured,
			})
			cancel()
		}
		findings.Observe(float64(len(seen)))
		if errors.Is(err, store.ErrStale) {
			// A graph job changed the load under the audit: nothing of it
			// is stored, and the load is audited again once it's due.
			outcome = "incomplete"
			err = finish(model.FoundryHealRunIncomplete)
		}
	}
	// A renewal now finds the run ended, whatever the outcome: stop them.
	stopRenewing()
	switch {
	case errors.Is(err, store.ErrNotFound) || err != nil && lost.Load():
		outcome = "lost"
	case err != nil:
		// Failed, the run waits retry_after rather than its lease alone.
		logger.Error("record heal run failed", "outcome", outcome, "err", err)
		if err := finish(model.FoundryHealRunFailed); err != nil && !errors.Is(err, store.ErrNotFound) {
			logger.Error("fail heal run failed", "err", err)
		}
		outcome = "unrecorded"
	}
	runs.WithLabelValues(outcome).Inc()
	runSeconds.WithLabelValues(outcome).Observe(time.Since(started).Seconds())
	logger.Info("heal run ended", "outcome", outcome, "pages", a.pages, "cuts", a.cuts, "findings", a.sums.Len())
}

// renew renews run id's lease every third of it until stop. A renewal
// refused (the run is no longer running) marks it lost and cancels the run.
func (r *runner) renew(ctx context.Context, cancel context.CancelFunc, id string) (lost *atomic.Bool, stop func()) {
	lost = &atomic.Bool{}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(r.cfg.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				switch err := r.db.RenewFoundryHealRun(ctx, id, r.cfg.Lease); {
				case errors.Is(err, store.ErrNotFound):
					lost.Store(true)
					cancel()
					return
				case err != nil && ctx.Err() == nil:
					log.From(ctx).Warn("renew heal run failed", "run", id, "err", err)
				}
			}
		}
	}()
	return lost, func() { close(done); <-stopped }
}

// audit pages Audit over due's load: its documents with reviews by id,
// pageSize at a time, the load's rows with the first call. A page the
// agent's wall cuts short is audited again from the same document at half
// the size; pages grow back as they're read whole. A run stops at a page
// of one document still cut short, at its first page past maxRun since
// started, or after a page once its load is no longer current or a graph
// job started on the ontology: what it read may have changed.
func (r *runner) audit(ctx context.Context, due model.FoundryHealDue, runID string, started time.Time) (audited, error) {
	var a audited
	infoCtx, cancelInfo := context.WithTimeout(ctx, agentReadyWait)
	info, err := r.agent.Info(infoCtx)
	cancelInfo()
	if err != nil {
		return a, fmt.Errorf("the ontology agent is unavailable: %w", err)
	}
	if info.GetContractVersion() != ontologyagent.ContractVersion {
		return a, fmt.Errorf("the ontology agent speaks contract %s, not %s", info.GetContractVersion(), ontologyagent.ContractVersion)
	}
	load := due.Load
	version, err := r.db.GetOntologyVersion(ctx, due.OntologyID, load.VersionID)
	if err != nil {
		return a, fmt.Errorf("read the load's version: %w", err)
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(version.Definition, &def); err != nil || version.Format != model.OntologyFormatAgentBundle {
		return a, fmt.Errorf("the load's version is not an ontology agent draft: %v", err)
	}
	// A load's ingest report is its load job's, whose id is the load's.
	var rows *ontologyagent.AuditRows
	switch job, err := r.db.GetFoundryGraphJob(ctx, due.OntologyID, load.ID); {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return a, fmt.Errorf("read the load's job: %w", err)
	default:
		rows = ontologyagent.IngestRowErrors(job.Report)
	}
	t, err := r.open(ctx, due)
	if err != nil {
		return a, fmt.Errorf("reach the ontology's database: %w", err)
	}
	defer t.close()

	request := ontologyagent.AuditRequest{
		Database: t.database, Load: load.ID, OntologyTOML: def.OntologyTOML, OntologyVersion: load.OntologyVersion(),
		MappingsTOML: def.MappingsTOML, Helix: t.helix, Rows: rows,
	}
	for after, size := "", r.pageSize; ; {
		if a.pages > 0 && time.Since(started) >= r.maxRun {
			a.stopped = true
			return a, nil
		}
		ids, err := r.db.ListFoundryHealDocuments(ctx, load.ID, after, size)
		if err != nil {
			return a, err
		}
		if len(ids) == 0 && request.Rows == nil {
			return a, nil
		}
		request.RequestID, request.DocIDs = runID+":"+strconv.Itoa(a.pages), append([]string{}, ids...)
		result, err := r.page(ctx, request)
		a.pages++
		if err != nil {
			return a, err
		}
		switch changed, err := r.db.FoundryHealLoadChanged(ctx, runID, due.OntologyID, load.ID); {
		case err != nil:
			return a, err
		case changed:
			a.stopped = true
			return a, nil
		}
		if len(result.Cut) > 0 {
			pages.WithLabelValues("cut").Inc()
			a.cuts++
			if len(ids) <= 1 {
				a.stopped = true
				return a, nil
			}
			size = max(1, len(ids)/2)
			continue
		}
		pages.WithLabelValues("done").Inc()
		if err := a.sums.Add(result.Findings); err != nil {
			return a, err
		}
		if request.Rows != nil {
			a.rowsMeasured, request.Rows = result.RowsMeasured, nil
		}
		if len(ids) < size {
			return a, nil
		}
		after, size = ids[len(ids)-1], min(r.pageSize, 2*size)
	}
}

// page sends one Audit and reads its result: an error for any but done.
func (r *runner) page(ctx context.Context, request ontologyagent.AuditRequest) (ontologyagent.AuditResult, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return ontologyagent.AuditResult{}, fmt.Errorf("encode audit request: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, auditWait)
	defer cancel()
	raw, err := r.agent.Audit(callCtx, body)
	if err != nil {
		return ontologyagent.AuditResult{}, fmt.Errorf("audit: %w", err)
	}
	result, err := ontologyagent.DecodeAuditResult(raw)
	switch {
	case err != nil:
		return result, err
	case result.Outcome == ontologyagent.AuditLoadUnknown:
		return result, errors.New("the database holds no such load")
	case result.Outcome == ontologyagent.AuditLoadMismatch:
		return result, fmt.Errorf("the database holds this load of another version (%s)", result.StoredVersion)
	}
	return result, nil
}
