package foundryaskworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// assess.go runs workflow runs' assessments beside the questions, one at a
// time, every foundry.workflows.assess.poll_interval: the ontology agent's
// Assess, on the workspace's question model, of a finished run's request,
// the calls it recorded (a question's answer read from its row, a send's
// source and an upload's document named) and its answer, against the
// ontology version the run read. An assessment is skipped once the
// workspace's month of them is spent; a failure a retry may fix is tried
// again, up to max_attempts.

// assessPerTick bounds the assessments one tick runs, so a backlog shares
// the worker's time with the next tick's checks.
const assessPerTick = 10

// assessAgent is the sidecar's calls an assessment makes.
type assessAgent interface {
	Info(ctx context.Context) (*agentv1.GetInfoResponse, error)
	Assess(ctx context.Context, request []byte, key ontologyagent.ModelKey) (json.RawMessage, error)
}

// assessStore is what an assessment reads and writes.
type assessStore interface {
	ClaimFoundryWorkflowRunAssessment(ctx context.Context, owner string, lease time.Duration) (*model.FoundryWorkflowAssessment, error)
	RenewFoundryWorkflowRunAssessment(ctx context.Context, runID, owner string, lease time.Duration) error
	FinishFoundryWorkflowRunAssessment(ctx context.Context, runID, owner string, o model.FoundryWorkflowAssessmentOutcome) error
	SumFoundryWorkflowAssessmentSpend(ctx context.Context, workspaceID string, since time.Time) (int64, error)
	GetFoundryWorkflowRun(ctx context.Context, ontologyID, id string) (*model.FoundryWorkflowRun, error)
	ListFoundryWorkflowRunAccesses(ctx context.Context, runID string, afterSeq int32, limit int) ([]*model.FoundryWorkflowAccess, bool, error)
	GetFoundryQuestion(ctx context.Context, ontologyID, id string) (*model.FoundryQuestion, error)
	GetConnector(ctx context.Context, workspaceID, id string) (*model.Connector, error)
	GetFoundryDocument(ctx context.Context, workspaceID, id string) (*model.FoundryDocument, error)
	GetFoundryLoad(ctx context.Context, ontologyID, id string) (*model.FoundryLoad, error)
	GetFoundryOntology(ctx context.Context, workspaceID, id string) (*model.FoundryOntology, error)
	GetOntologyVersion(ctx context.Context, ontologyID, id string) (*model.OntologyVersion, error)
}

type assessor struct {
	db    assessStore
	agent assessAgent
	ai    resolver
	cfg   config.WorkflowAssessConfig
	// month is the start of the month a moment is in, as budgets count it.
	month func(time.Time) time.Time
	owner string
	now   func() time.Time
}

// tick runs the assessments due, up to assessPerTick.
func (a *assessor) tick(ctx context.Context) {
	if !a.cfg.Enabled {
		return
	}
	for range assessPerTick {
		claimed, err := a.db.ClaimFoundryWorkflowRunAssessment(ctx, a.owner, a.cfg.Lease)
		if errors.Is(err, foundrystore.ErrNotFound) {
			return
		}
		if err != nil {
			log.From(ctx).Warn("foundry assess: claim failed", "err", err)
			return
		}
		a.run(ctx, claimed)
	}
}

// run runs one claimed assessment to its outcome and stores it.
func (a *assessor) run(ctx context.Context, claimed *model.FoundryWorkflowAssessment) {
	logger := log.From(ctx).With("run", claimed.RunID, "workspace", claimed.WorkspaceID, "attempt", claimed.Attempts)
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(a.cfg.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-attemptCtx.Done():
				return
			case <-ticker.C:
				if err := a.db.RenewFoundryWorkflowRunAssessment(attemptCtx, claimed.RunID, a.owner, a.cfg.Lease); errors.Is(err, foundrystore.ErrNotFound) {
					// Its run went, or another worker took it.
					cancel()
					return
				}
			}
		}
	}()
	outcome := a.attempt(attemptCtx, claimed)
	cancel()
	<-renewed
	finishCtx, done := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer done()
	switch err := a.db.FinishFoundryWorkflowRunAssessment(finishCtx, claimed.RunID, a.owner, outcome); {
	case errors.Is(err, foundrystore.ErrNotFound):
		logger.Info("foundry assess: the assessment went or was taken before it finished")
	case err != nil:
		logger.Warn("foundry assess: storing the outcome failed; its lease will lapse and it runs again", "err", err)
	default:
		logger.Info("foundry assess: finished", "status", outcome.Status, "verdict", outcome.Verdict, "usd_micros", outcome.USDMicros)
	}
}

// attempt runs one attempt: skipped when the month's budget can't cover it,
// else the agent's Assess of the run.
func (a *assessor) attempt(ctx context.Context, claimed *model.FoundryWorkflowAssessment) model.FoundryWorkflowAssessmentOutcome {
	if claimed.Attempts > a.cfg.MaxAttempts {
		// Its earlier attempts' workers stopped before storing what came of
		// them, as many times as a failure gets.
		return model.FoundryWorkflowAssessmentOutcome{Status: model.FoundryWorkflowAssessmentFailed,
			Error: "the assessment kept stopping before it finished"}
	}
	spent, err := a.db.SumFoundryWorkflowAssessmentSpend(ctx, claimed.WorkspaceID, a.month(a.now()))
	if err != nil {
		return a.retry(claimed, 0, "the month's spend couldn't be read; Helix will retry")
	}
	if spent+a.cfg.MaxMicros() > a.cfg.MonthlyMicros() {
		return model.FoundryWorkflowAssessmentOutcome{Status: model.FoundryWorkflowAssessmentSkipped,
			Error: fmt.Sprintf("this workspace's $%.2f of run assessments for the month is spent", a.cfg.MonthlyUSD)}
	}
	request, err := a.request(ctx, claimed)
	if err != nil {
		return a.retry(claimed, 0, "the run couldn't be read; Helix will retry")
	}
	info, err := a.agent.Info(ctx)
	if err != nil {
		return a.retry(claimed, 0, "the ontology agent is unavailable; Helix will retry")
	}
	if info.GetContractVersion() != ontologyagent.ContractVersion {
		// A sidecar from before this backend: waiting costs no attempt.
		return a.waitForAgent(fmt.Sprintf("the ontology agent speaks contract %s, not %s; Helix will retry",
			info.GetContractVersion(), ontologyagent.ContractVersion))
	}
	resolved, err := a.ai.ResolveServed(ctx, claimed.WorkspaceID, info.GetProviders())
	if err != nil {
		c := ai.ResolveFailure(err).Logged(ctx, err)
		if c.Permanent {
			return model.FoundryWorkflowAssessmentOutcome{Status: model.FoundryWorkflowAssessmentFailed, Error: c.Message}
		}
		return a.retry(claimed, 0, c.Message)
	}
	m := resolved.Ask()
	request.Models = ontologyagent.AssessModels{Endpoint: m.Endpoint, Assess: m.Ask}
	body, err := json.Marshal(request)
	if err != nil {
		return model.FoundryWorkflowAssessmentOutcome{Status: model.FoundryWorkflowAssessmentFailed, Error: "the run couldn't be sent to the ontology agent"}
	}
	raw, err := a.agent.Assess(ctx, body, resolved.Key)
	switch status.Code(err) {
	case codes.OK:
	case codes.Unimplemented:
		return a.waitForAgent("the ontology agent doesn't assess runs yet; Helix will retry")
	case codes.InvalidArgument, codes.FailedPrecondition:
		log.From(ctx).Error("foundry assess: the agent refused the request", "run", claimed.RunID, "err", err)
		return model.FoundryWorkflowAssessmentOutcome{Status: model.FoundryWorkflowAssessmentFailed,
			Error: "the ontology agent couldn't assess this run"}
	default:
		return a.retry(claimed, 0, "the ontology agent is unavailable; Helix will retry")
	}
	result, err := ontologyagent.DecodeAssessResult(raw)
	if err != nil {
		log.From(ctx).Error("foundry assess: the agent's result isn't the contract's", "run", claimed.RunID, "err", err)
		return model.FoundryWorkflowAssessmentOutcome{Status: model.FoundryWorkflowAssessmentFailed,
			Error: "the ontology agent's assessment couldn't be read"}
	}
	spentNow := int64(math.Round(result.Usage.USD * 1e6))
	if result.Failure != "" {
		c := ai.AssessFailure(resolved, result.Failure).Logged(ctx, fmt.Errorf("assess failed: %s", result.Failure))
		if c.Permanent {
			return model.FoundryWorkflowAssessmentOutcome{Status: model.FoundryWorkflowAssessmentFailed,
				Failure: string(result.Failure), Error: c.Message, USDMicros: spentNow}
		}
		o := a.retry(claimed, spentNow, c.Message)
		o.Failure = string(result.Failure)
		return o
	}
	return model.FoundryWorkflowAssessmentOutcome{Status: model.FoundryWorkflowAssessmentDone, Verdict: result.Verdict,
		Assessment: result.Assessment, LeftOut: result.LeftOut, USDMicros: spentNow}
}

// waitForAgent queues the assessment again after retry_backoff without
// counting the attempt: the ontology agent beside this worker can't assess
// runs yet, which no attempt changes.
func (a *assessor) waitForAgent(why string) model.FoundryWorkflowAssessmentOutcome {
	return model.FoundryWorkflowAssessmentOutcome{Status: model.FoundryWorkflowAssessmentQueued, Error: why,
		RetryAt: a.now().Add(a.cfg.RetryBackoff), Uncounted: true}
}

// retry queues the assessment again after retry_backoff, or fails it once
// it has had max_attempts, saying why.
func (a *assessor) retry(claimed *model.FoundryWorkflowAssessment, spent int64, why string) model.FoundryWorkflowAssessmentOutcome {
	if claimed.Attempts >= a.cfg.MaxAttempts {
		return model.FoundryWorkflowAssessmentOutcome{Status: model.FoundryWorkflowAssessmentFailed, Error: why, USDMicros: spent}
	}
	return model.FoundryWorkflowAssessmentOutcome{Status: model.FoundryWorkflowAssessmentQueued, Error: why, USDMicros: spent,
		RetryAt: a.now().Add(a.cfg.RetryBackoff)}
}

// request is the run as the agent's AssessRequest takes it, but for its
// models: its request and answer, every call it recorded, and the
// definition of the version whose load it read last (the ontology's current
// version when it read none).
func (a *assessor) request(ctx context.Context, claimed *model.FoundryWorkflowAssessment) (*ontologyagent.AssessRequest, error) {
	run, err := a.db.GetFoundryWorkflowRun(ctx, claimed.OntologyID, claimed.RunID)
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	accesses, _, err := a.db.ListFoundryWorkflowRunAccesses(ctx, run.ID, 0, model.FoundryWorkflowRunMaxAccesses)
	if err != nil {
		return nil, fmt.Errorf("list accesses: %w", err)
	}
	out := &ontologyagent.AssessRequest{RequestID: run.ID, Calls: make([]ontologyagent.AssessCall, 0, len(accesses))}
	if run.Input != nil {
		out.Input = *run.Input
	}
	if run.Finished != nil {
		out.Output = run.Finished.Output
	}
	lastLoad := ""
	for _, access := range accesses {
		call := ontologyagent.AssessCall{Seq: access.Seq}
		switch c := access.Call.(type) {
		case *model.FoundryWorkflowRead:
			call.Call, call.Reads, call.Result, lastLoad = "read", c.Reads, c.Result, c.LoadID
		case *model.FoundryWorkflowQuestion:
			call.Call, call.Status, lastLoad = "question", "deleted", c.LoadID
			switch q, err := a.db.GetFoundryQuestion(ctx, claimed.OntologyID, c.QuestionID); {
			case errors.Is(err, foundrystore.ErrNotFound):
			case err != nil:
				return nil, fmt.Errorf("get question: %w", err)
			default:
				call.Question, call.Status = q.Question, string(q.Status)
				if len(q.Result) > 0 {
					call.Answer = ontologyagent.WithoutOutputs(q.Result)
				}
			}
		case *model.FoundryWorkflowSend:
			call.Call, call.Source, call.Pieces = "send", c.ConnectorID, c.PieceIDs
			if conn, err := a.db.GetConnector(ctx, claimed.WorkspaceID, c.ConnectorID); err == nil && conn.Name != "" {
				call.Source = conn.Name
			}
		case *model.FoundryWorkflowUpload:
			call.Call, call.Document = "upload", c.DocumentID
			if doc, err := a.db.GetFoundryDocument(ctx, claimed.WorkspaceID, c.DocumentID); err == nil && doc.Name != "" {
				call.Document = doc.Name
			}
		}
		out.Calls = append(out.Calls, call)
	}
	out.OntologyTOML, err = a.ontologyTOML(ctx, claimed, lastLoad)
	return out, err
}

// ontologyTOML is the agent draft's ontology.toml of the version loadID was
// loaded from, or the ontology's current version's without one: "" for a
// version that isn't an agent draft, or none.
func (a *assessor) ontologyTOML(ctx context.Context, claimed *model.FoundryWorkflowAssessment, loadID string) (string, error) {
	versionID := ""
	if loadID != "" {
		switch load, err := a.db.GetFoundryLoad(ctx, claimed.OntologyID, loadID); {
		case errors.Is(err, foundrystore.ErrNotFound):
		case err != nil:
			return "", fmt.Errorf("get load: %w", err)
		default:
			versionID = load.VersionID
		}
	}
	if versionID == "" {
		o, err := a.db.GetFoundryOntology(ctx, claimed.WorkspaceID, claimed.OntologyID)
		if err != nil {
			return "", fmt.Errorf("get ontology: %w", err)
		}
		if o.CurrentVersionID == nil {
			return "", nil
		}
		versionID = *o.CurrentVersionID
	}
	v, err := a.db.GetOntologyVersion(ctx, claimed.OntologyID, versionID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("get version: %w", err)
	case v.Format != model.OntologyFormatAgentBundle:
		return "", nil
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(v.Definition, &def); err != nil {
		return "", nil
	}
	return def.OntologyTOML, nil
}
