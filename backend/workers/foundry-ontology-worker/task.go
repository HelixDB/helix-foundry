package foundryontologyworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/dig"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologygen"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sample"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	"github.com/helixdb/helix-foundry/backend/server/tasks"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const Name = "foundry-ontology-worker"

type Deps struct {
	dig.In

	Cfg    *config.Config
	Logger *slog.Logger
	Health endpoint.Health `optional:"true"`
	DB     store.Store     `optional:"true"`
	// Boosts lift a database's rate limit while a revision reads it; nil
	// never does, and foundry.graph.load_rate_limit is then refused.
	Boosts ports.Boosts `optional:"true"`
	// Databases are the databases a revision reads.
	Databases ports.Databases `optional:"true"`
	// Plans says which workspaces' plans let them draft on their own provider.
	Plans ports.PlanReader `optional:"true"`
	AWS   aws.Config       `optional:"true"`
}

type generationQueue interface {
	ClaimOntologyGeneration(ctx context.Context, workerID string, lease time.Duration) (*model.OntologyGeneration, error)
	RenewOntologyGenerationLease(ctx context.Context, id, workerID string, lease time.Duration) error
	FinishOntologyGeneration(ctx context.Context, id, workerID string, outcome model.OntologyGenerationOutcome) error
}

type generator interface {
	Run(ctx context.Context, gen *model.OntologyGeneration) (ontologygen.Result, error)
}

// Task drafts queued ontologies one at a time with the ontology agent, and
// runs revisions of drafts.
type Task struct {
	deps      Deps
	cfg       config.OntologyConfig
	queue     generationQueue
	generator generator
	reviser   *reviser
	// closeGenerator releases the agent client and the read-only proxy.
	closeGenerator func() error
	workerID       string
	now            func() time.Time
	worker         tasks.Worker
}

func New(d Deps) (*Task, error) {
	if d.DB == nil || d.Databases == nil {
		return nil, errors.New("foundry-ontology-worker: db not configured")
	}
	if d.Cfg.Foundry.Graph.LoadRateLimit.Enabled() && d.Boosts == nil {
		return nil, errors.New("foundry-ontology-worker: foundry.graph.load_rate_limit is set, but the host has no rate-limit boosts")
	}
	cfg := d.Cfg.Foundry.Ontology
	client := s3.NewFromConfig(d.AWS)
	snapshots := snapshot.NewS3(client, d.Cfg.Foundry.Sync.DataBucket)
	t := &Task{deps: d, cfg: cfg, queue: d.DB, workerID: id.NewWorkerID(), now: time.Now}
	resolver, err := ai.NewResolver(d.Cfg, d.DB, d.Plans)
	if err != nil {
		return nil, fmt.Errorf("foundry-ontology-worker: %w", err)
	}
	agent, err := ontologyagent.Dial(cfg.Agent.Addr)
	if err != nil {
		return nil, fmt.Errorf("foundry-ontology-worker: %w", err)
	}
	limits := cfg.Agent.Limits
	t.generator = &ontologyagent.Generator{
		DB: d.DB, Snapshots: snapshots, Documents: document.New(client, d.Cfg.Foundry.Sync.DataBucket), Agent: agent, AI: resolver,
		WorkDir: cfg.Agent.WorkDir, MaxWorkBytes: cfg.Agent.MaxWorkBytes, Now: time.Now, Enrichment: d.Cfg.Foundry.Enrichment.Enabled,
		Limits: ontologyagent.Limits{MaxUSD: limits.MaxUSD, MaxSteps: uint32(limits.MaxSteps), MaxWallSecs: uint64(limits.MaxWallSecs)},
	}
	// A revision's evaluation reads the load through the pod's read-only
	// proxy. One read waits up to the agent's Helix timeout; the margin
	// covers the gateway's own. A read the gateway turned away is sent
	// again for a third of it.
	helixTimeout := time.Duration(d.Cfg.Foundry.Graph.HelixTimeoutSecs) * time.Second
	proxy, err := graphproxy.ServeReadOnly(&helixgateway.Client{
		HTTP:             helixgateway.NewHTTPClient(helixTimeout + 10*time.Second),
		ProvisionerKey:   d.Cfg.Host.GatewayKey,
		Keyless:          d.Cfg.Host.Local,
		MaxResponseBytes: graphproxy.MaxResponseBytes,
	}, helixTimeout/3)
	if err != nil {
		_ = agent.Close()
		return nil, fmt.Errorf("foundry-ontology-worker: %w", err)
	}
	t.reviser = newReviser(d.DB, d.Databases, d.Boosts, d.Cfg, t.workerID, agent, proxy)
	t.closeGenerator = func() error { return errors.Join(proxy.Close(), agent.Close()) }
	return t, nil
}

func (t *Task) Name() string { return Name }

func (t *Task) Start(ctx context.Context) error {
	return t.worker.Run(ctx, Name, t.cfg.HTTP.Listen, t.deps.Health, t.deps.Logger, func(ctx context.Context) error {
		tasks.Tick(ctx, t.cfg.PollInterval, t.drain)
		return nil
	})
}

func (t *Task) Stop(ctx context.Context) error {
	err := t.worker.Stop(ctx)
	if t.closeGenerator != nil {
		err = errors.Join(err, t.closeGenerator())
	}
	return err
}

// drain runs due generations until none is left.
func (t *Task) drain(ctx context.Context) {
	for ctx.Err() == nil {
		gen, err := t.queue.ClaimOntologyGeneration(ctx, t.workerID, t.cfg.Lease)
		if err != nil {
			log.From(ctx).Error("claim ontology generation failed", "err", err)
			return
		}
		if gen == nil {
			return
		}
		t.process(ctx, gen)
	}
}

// process runs one attempt while renewing its lease, then records the outcome.
// Losing the lease cancels the attempt: another worker owns the generation now.
func (t *Task) process(ctx context.Context, gen *model.OntologyGeneration) {
	runCtx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
	defer cancel()
	stopRenewing := t.renewLease(runCtx, cancel, gen.ID)
	var result ontologygen.Result
	var revision revisionResult
	var err error
	if gen.Input.Revise != nil {
		if t.reviser == nil {
			err = &ontologygen.Failure{Message: "revisions require the ontology agent", Permanent: true}
		} else {
			revision, err = t.reviser.run(runCtx, gen)
			result.Format, result.Definition = model.OntologyFormatAgentBundle, revision.definition
		}
	} else if gen.Input.Sample != "" {
		result, err = sampleDraft(gen)
	} else {
		result, err = t.generator.Run(runCtx, gen)
	}
	stopRenewing()

	outcome := t.outcome(ctx, gen, result, err)
	outcome.ReviseResult = revision.report
	if err := t.queue.FinishOntologyGeneration(ctx, gen.ID, t.workerID, outcome); err != nil {
		log.From(ctx).Warn("finish ontology generation failed", "generation", gen.ID, "err", err)
		return
	}
	tokens.WithLabelValues("input").Add(float64(result.InputTokens))
	tokens.WithLabelValues("output").Add(float64(result.OutputTokens))
	switch {
	case outcome.Status == model.OntologyGenerationSucceeded:
		generations.WithLabelValues("succeeded").Inc()
	case outcome.RetryAt != nil:
		generations.WithLabelValues("retried").Inc()
	default:
		generations.WithLabelValues("failed").Inc()
	}
}

func (t *Task) renewLease(ctx context.Context, cancel context.CancelFunc, genID string) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(t.cfg.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := t.queue.RenewOntologyGenerationLease(ctx, genID, t.workerID, t.cfg.Lease)
				if errors.Is(err, store.ErrNotFound) {
					log.From(ctx).Warn("ontology generation lease lost", "generation", genID)
					cancel()
					return
				}
				if err != nil {
					log.From(ctx).Warn("renew ontology generation lease failed", "generation", genID, "err", err)
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// sampleDraft is a sample generation's result, with no model called: the
// sample's draft, each of its tables named by its source id in the
// connector the generation drafts from. A generation that recorded one
// missing fails for good.
func sampleDraft(gen *model.OntologyGeneration) (ontologygen.Result, error) {
	result := ontologygen.Result{Format: model.OntologyFormatAgentBundle}
	sources := map[string]string{}
	for _, t := range gen.Input.Tables {
		sources[t.Table] = ontologyagent.TableSourceID(t.ConnectorID, t.Table)
	}
	set := ""
	if len(gen.Input.DocumentSetIDs) > 0 {
		set = gen.Input.DocumentSetIDs[0]
	}
	var err error
	if result.Definition, err = sample.Draft(gen.Input.Sample, sources, set); err != nil {
		return result, &ontologygen.Failure{Message: "the sample's database is missing a table: add the sample again", Permanent: true, Err: err}
	}
	return result, nil
}

// outcome decides what an attempt leaves behind: a draft version, a retry
// after a backoff that doubles from a minute, or a failure for good. The
// version's scope is the sets the generation read: for a revision, its
// parent version's scope and the sets its revised routes name (storing the
// version keeps the live ones of the workspace).
func (t *Task) outcome(ctx context.Context, gen *model.OntologyGeneration, result ontologygen.Result, err error) model.OntologyGenerationOutcome {
	usage := model.OntologyGenerationOutcome{
		Model: result.Model, ModelProvider: result.ModelProvider, InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
	}
	if err == nil {
		usage.Status = model.OntologyGenerationSucceeded
		source := model.OntologySourceGenerated
		scope := append([]string{}, gen.Input.DocumentSetIDs...)
		if rev := gen.Input.Revise; rev != nil {
			if rev.DryRun {
				return usage
			}
			source = model.OntologySourceManual
			var def ontologyagent.Definition
			var routes ontologyagent.DocumentRoutes
			readErr := json.Unmarshal(result.Definition, &def)
			if readErr == nil {
				routes, readErr = ontologyagent.ParseDocumentRoutes(def.MappingsTOML)
			}
			if readErr != nil {
				log.From(ctx).Warn("read revised routes failed; the version reads its parent's sets", "generation", gen.ID, "err", readErr)
			}
			scope = append(scope, routes.Sets()...)
		}
		usage.Result = &model.OntologyVersion{
			ID: id.New(), WorkspaceID: gen.WorkspaceID, OntologyID: gen.OntologyID, Format: result.Format, Definition: result.Definition,
			Source: source, GenerationID: &gen.ID, DocumentSetIDs: &scope, CreatedByUserID: gen.RequestedByUserID,
		}
		return usage
	}
	var failure *ontologygen.Failure
	if !errors.As(err, &failure) {
		failure = &ontologygen.Failure{Message: "the generation failed; Helix will retry", Err: err}
	}
	log.From(ctx).Warn("ontology generation attempt failed", "generation", gen.ID, "workspace", gen.WorkspaceID,
		"attempt", gen.Attempts, "permanent", failure.Permanent, "err", err)
	if !failure.Permanent && gen.Attempts < t.cfg.MaxAttempts {
		retryAt := t.now().Add(time.Minute << (gen.Attempts - 1))
		usage.RetryAt, usage.Error = &retryAt, failure.Message
		return usage
	}
	usage.Status, usage.Error = model.OntologyGenerationFailed, failure.Message
	if !failure.Permanent {
		usage.Error = fmt.Sprintf("%s (gave up after %d attempts)", failure.Message, gen.Attempts)
	}
	return usage
}
