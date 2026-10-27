package foundrygraphworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/enrichment"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologygen"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// agentReadyWait is how long an attempt waits for a sidecar still starting.
	agentReadyWait = 30 * time.Second
	// maxReportBytes bounds the report a job keeps; past it the ingest
	// report is cut to its status.
	maxReportBytes = 256 << 10
	// jobsDir, under the work dir, holds what each attempt stages for the
	// agent, in jobsDir/<job>: removed as the attempt ends, and all of it as
	// a worker starts.
	jobsDir = "jobs"
)

// store is what an attempt reads and records of Foundry's store.
type store interface {
	ontologyagent.TableStore
	ontologyagent.DocumentTableStore
	graphproxy.Store
	GetOntologyVersionScope(ctx context.Context, ontologyID, versionID string) ([]string, error)
	ListFoundryLoadDecisions(ctx context.Context, loadID, afterID string, documentIDs []string, limit int) ([]*model.FoundryReviewDecision, error)
	GetOntologyVersion(ctx context.Context, ontologyID, id string) (*model.OntologyVersion, error)
	GetOntologyGeneration(ctx context.Context, ontologyID, id string) (*model.OntologyGeneration, error)
	ListPendingFoundryDocuments(ctx context.Context, loadID string) ([]*model.FoundryDocument, error)
	ListRemovedFoundryDocuments(ctx context.Context, loadID string) ([]string, error)
	RecordFoundryDocumentRemoval(ctx context.Context, jobID, workerID, documentID string) error
	DeleteFoundryDocument(ctx context.Context, workspaceID, id string) error
	SumFoundryGraphJobSpend(ctx context.Context, workspaceID string, since time.Time) (int64, error)
	UpdateFoundryGraphJob(ctx context.Context, id, workerID string, update model.FoundryGraphJobUpdate) error
	SetFoundryGraphJobSources(ctx context.Context, id, workerID string, sources []model.FoundryLoadSource) error
	ListFoundryLoadSources(ctx context.Context, loadIDs []string) (map[string][]model.FoundryLoadSource, error)
	ListConnectorChangeBatchesAfter(ctx context.Context, workspaceID, connectorID, table string, afterSeq, toSeq int64) (
		[]*model.ConnectorChangeBatch, error)
	RecordFoundryDocumentExtraction(ctx context.Context, jobID, workerID string, e *model.FoundryDocumentExtraction, keepExisting bool) error
	RecordFoundryLoadEnrichmentIngest(ctx context.Context, loadID, jobWorker string, applied, unmatched int64) error
	FoundryDocumentSetSpend(ctx context.Context, workspaceID string, setIDs []string, month time.Time) (map[string]model.FoundryDocumentSetSpend, error)
	GetFoundryLoad(ctx context.Context, ontologyID, id string) (*model.FoundryLoad, error)
	ListFoundryDocumentExtractions(ctx context.Context, loadID string, documentIDs []string) (map[string]*model.FoundryDocumentExtraction, error)
}

// agent is the sidecar; *ontologyagent.Client calls it.
type agent interface {
	Info(ctx context.Context) (*agentv1.GetInfoResponse, error)
	Ingest(ctx context.Context, request []byte, onEvent func(json.RawMessage) error) (json.RawMessage, error)
	Extract(ctx context.Context, request []byte, key ontologyagent.ModelKey, onEvent func(json.RawMessage) error) (json.RawMessage, error)
	DeleteLoad(ctx context.Context, request []byte, onEvent func(json.RawMessage) error) (json.RawMessage, error)
	Clues(ctx context.Context, request []byte, onEvent func(json.RawMessage) error) (json.RawMessage, error)
}

// resolver is where a workspace's model calls go; *ai.Resolver resolves it.
type resolver interface {
	ResolveServed(ctx context.Context, workspaceID string, providers []string) (ai.Resolved, error)
}

// documentStore is where documents and their artifacts live (document.Store).
type documentStore interface {
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]string, error)
	OpenArtifact(ctx context.Context, key string) ([]byte, error)
	PutArtifact(ctx context.Context, key string, artifact []byte) error
}

// failure is why an attempt did not succeed, worded for the customer.
// Permanent ends the job; Budget ends it as budget_exhausted; anything else
// is retried with backoff, and an Uncounted one (the database still building
// an index) gives its attempt back.
type failure struct {
	Message   string
	Permanent bool
	Budget    bool
	Uncounted bool
	// Unsupported is an apply the agent can't do in place (unsupported).
	Unsupported bool
	Err         error
}

func (f *failure) Error() string {
	if f.Err != nil {
		return f.Message + ": " + f.Err.Error()
	}
	return f.Message
}

func (f *failure) Unwrap() error { return f.Err }

// parked is an attempt that ends without an outcome: its load waits for its
// People enrichment step, so the job is queued again at until, at
// provisioning, the attempt given back (ParkFoundryGraphJob).
type parked struct{ until time.Time }

func (p *parked) Error() string {
	return "the load waits for its people to be looked up until " + p.until.Format(time.RFC3339)
}

func retry(message string, err error) error { return &failure{Message: message, Err: err} }

func permanent(message string, err error) error {
	return &failure{Message: message, Permanent: true, Err: err}
}

// unsupported ends an apply the agent can't bring up to date in place; the
// finish queues a full refresh load instead (FinishFoundryGraphJob).
func unsupported(reason string) error {
	return &failure{Message: clip("Helix rebuilt the graph instead of updating it in place: " + reason), Permanent: true, Unsupported: true}
}

// attempt is what one attempt did, kept whether or not it succeeded.
type attempt struct {
	// ingestDone means the version's tables are in the load.
	ingestDone bool
	// applied is what an apply's Ingest did to each stream, once it is done.
	applied []ontologyagent.AppliedStream
	// ingestViolations is how many violation records this attempt's Ingest
	// left open, once its report counted them: every update records it on
	// the load (FoundryGraphJobUpdate.IngestViolations).
	ingestViolations *uint64
	progress         model.FoundryGraphJobProgress
	// rows are the table rows the attempt's chunks covered, for its finish log.
	rows uint64
	// results are the agent's results by call, kept as the job's report.
	results map[string]json.RawMessage
	// spent is what the documents this attempt recorded spent, in
	// millionths of a dollar, which the job's spend its claim read lacks.
	spent int64
	// held are the sets with an allowance whose documents a budget stop of
	// the attempt left unrecorded (extract), which its outcome hands on.
	held []string
}

// report is the job's report: each call's result, the ingest report cut to
// its status when the whole would pass maxReportBytes.
func (a *attempt) report() json.RawMessage {
	if len(a.results) == 0 {
		return nil
	}
	raw, err := json.Marshal(a.results)
	if err == nil && len(raw) <= maxReportBytes {
		return raw
	}
	trimmed := map[string]json.RawMessage{}
	for call, result := range a.results {
		var status struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(result, &status) // best-effort: an unreadable result keeps no status
		trimmed[call], _ = json.Marshal(status)
	}
	raw, _ = json.Marshal(trimmed)
	return raw
}

// runner runs one attempt of a graph job.
type runner struct {
	db  store
	dbs ports.Databases
	// boosts lift its database's rate limit for an attempt; nil never does.
	boosts ports.Boosts
	agent  agent
	// ai resolves, every attempt that extracts, the models and key it runs on.
	ai        resolver
	documents documentStore
	snapshots *ontologyagent.SnapshotCache
	// enrichment is the load's side of People enrichment's step.
	enrichment *enrichment.Load
	gateway    graphproxy.Querier
	cfg        *config.Config
	bucket     string
	workerID   string
	now        func() time.Time
	// syncWait and syncPoll bound the wait for the gateway sync that carries
	// an attempt's boost (boostSyncWait, boostSyncPoll).
	syncWait time.Duration
	syncPoll time.Duration
	// maxRequestBytes is the most JSON one Extract may send
	// (ontologyagent.MaxRequestBytes).
	maxRequestBytes int
}

// run runs an attempt of job.
//  1. Check the agent speaks the vendored contract; another fails the job.
//  2. A delete job binds a proxy to the ontology's database and deletes its
//     load (deleteLoad). Otherwise:
//  3. Read the version's ontology and mappings. Unless the tables are in,
//     a version with an [enrichments] item runs the load's People
//     enrichment step first (enrich), which either chooses the profiles the
//     load applies or parks the job while the people are looked up: the
//     attempt then ends there, with no outcome (*parked).
//  4. Bind a proxy to the ontology's database and, unless the tables are
//     in, Ingest the generation's tables through the mappings, with the
//     profiles chosen, creating the job's load.
//  5. Extract the documents the load waits for, each with its set and every
//     artifact saved for it, under what is left of the job's and the month's
//     budget, on the models and key the workspace's AI setting resolves to,
//     of a provider the agent calls. The version's mappings go with them:
//     their extraction routes scope each document by its set, and a document
//     no route names ends out_of_scope with no model call. Each artifact the
//     agent sends is saved before its document commits, and each document's
//     end is recorded as it comes. With the budget gone they still go, with
//     nothing to spend: those whose saved artifacts replay commit at $0, the
//     rest wait, so a refresh keeps the graph's documents with the month
//     spent. A job no person started extracts each API source's documents in
//     an Extract of its own, within what is left of the source's month's
//     allowance (extract). An attempt after one that reached the documents
//     may owe the sweep the engine runs only at a run's end: it calls
//     Extract whatever is pending. A request past the agent's message limit
//     fails for good.
//
// Both proxies come from serve: with foundry.graph.load_rate_limit set, the
// database's rate limit is lifted for the attempt first (boost), and lowered
// again however the attempt ends.
//
// Its error is a *failure, or another error to retry on.
//
// The tables are the generation's synced tables, at the snapshots the job's
// first read of them pinned, and its table files still uploaded in the
// version's sets (documentTables).
func (r *runner) run(ctx context.Context, job *model.FoundryGraphJob) (*attempt, error) {
	a := &attempt{ingestDone: !job.Phase.RunsIngest(), results: map[string]json.RawMessage{}}
	infoCtx, cancelInfo := context.WithTimeout(ctx, agentReadyWait)
	info, err := r.agent.Info(infoCtx)
	cancelInfo()
	if err != nil {
		return a, retry("the ontology agent is unavailable; Helix will retry", err)
	}
	if info.GetContractVersion() != ontologyagent.ContractVersion {
		return a, permanent(fmt.Sprintf("the ontology agent speaks contract %s, not %s", info.GetContractVersion(), ontologyagent.ContractVersion), nil)
	}
	if job.Kind == model.FoundryGraphJobDelete {
		defer r.timeStep("delete", r.now())
		return a, r.deleteLoad(ctx, job, a)
	}
	version, err := r.db.GetOntologyVersion(ctx, job.OntologyID, job.VersionID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return a, permanent("the version to load is gone", err)
	}
	if err != nil {
		return a, retry("could not read the version; Helix will retry", err)
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(version.Definition, &def); err != nil || version.Format != model.OntologyFormatAgentBundle {
		return a, permanent("the version is not an ontology agent draft", err)
	}
	var item *ontologyagent.Enrichment
	var profiles *model.FoundryEnrichmentApplied
	if !a.ingestDone {
		if item, err = ontologyagent.ParseEnrichments(def.MappingsTOML); err != nil {
			return a, permanent("the version's People enrichment item can't be read", err)
		}
		// Applying in place would reuse the profiles the load was built with.
		if item != nil && job.Kind == model.FoundryGraphJobApply {
			return a, unsupported("the version enriches people")
		}
		if item != nil {
			if profiles, err = r.enrich(ctx, job, version, def, item); err != nil {
				return a, err
			}
		}
	}
	binding, target, stop, err := r.serve(ctx, job)
	if err != nil {
		return a, err
	}
	defer stop()
	ontologyVersion := fmt.Sprintf("foundry-v%d", version.Version)
	if !a.ingestDone {
		if err := r.ingest(ctx, job, version, def, binding, target, ontologyVersion, item, profiles, a); err != nil {
			return a, err
		}
		a.ingestDone = true
	}
	defer r.timeStep("extract", r.now())
	return a, r.extract(ctx, job, def, binding, target, ontologyVersion, info.GetProviders(), a)
}

func (r *runner) timeStep(step string, start time.Time) {
	stepSeconds.WithLabelValues(step).Observe(r.now().Sub(start).Seconds())
}

// chunkCounts reads a chunk event's rows and outcome. The report's shape is
// not part of the contract, so a report it can't read counts as no rows of an
// unknown outcome rather than failing the load.
func chunkCounts(event json.RawMessage) (rows uint64, outcome string) {
	var chunk struct {
		Report struct {
			Rows    uint64 `json:"rows"`
			Outcome string `json:"outcome"`
		} `json:"report"`
	}
	if json.Unmarshal(event, &chunk) != nil || chunk.Report.Outcome == "" {
		return chunk.Report.Rows, "unknown"
	}
	return chunk.Report.Rows, chunk.Report.Outcome
}

// enrich runs the load's People enrichment step (enrichment.Load.Step)
// before anything is written, and returns the profiles its Ingest applies,
// the same on every attempt. A step still looking people up parks the job
// (*parked). The step reads the load's people with the engine's Clues
// (people); whatever else goes wrong preparing it ends the step failed, and
// the load goes on with the profiles found before: enrichment never fails a
// load.
func (r *runner) enrich(ctx context.Context, job *model.FoundryGraphJob, version *model.OntologyVersion, def ontologyagent.Definition,
	item *ontologyagent.Enrichment) (*model.FoundryEnrichmentApplied, error) {
	next, err := r.enrichment.Step(ctx, job, r.workerID, item, func(ctx context.Context, add func(enrichment.Clue) error) (int64, error) {
		return r.people(ctx, job, version, def, add)
	})
	var f *failure
	switch {
	case errors.As(err, &f):
		return nil, f
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, retry("the job's lease was lost", err)
	case err != nil:
		return nil, retry("could not prepare People enrichment; Helix will retry", err)
	case next.Apply == nil:
		return nil, &parked{until: next.Until}
	}
	return next.Apply, nil
}

// people is the engine's Clues over the tables the load's Ingest reads: it
// hands add every person of the item's type, with the clues and name the
// load's identifier index holds, as each page comes, and returns Clues'
// shared count. What would fail the load's Ingest (the tables unreadable,
// the mappings invalid, a call a retry may get through) fails the attempt
// as Ingest would (*enrichment.LoadFailure); anything else, add's error
// included, fails the step, and the load goes on.
func (r *runner) people(ctx context.Context, job *model.FoundryGraphJob, version *model.OntologyVersion, def ontologyagent.Definition,
	add func(enrichment.Clue) error) (int64, error) {
	sources, err := r.tables(ctx, job, version, def)
	if err != nil {
		return 0, &enrichment.LoadFailure{Err: err}
	}
	end, err := ontologyagent.ReadClues(ctx, r.agent.Clues, ontologyagent.CluesRequest{
		RequestID: job.ID + ":clues", Load: job.LoadID, OntologyTOML: def.OntologyTOML, MappingsTOML: def.MappingsTOML,
		Sources: sources, MaxRequestBytes: graphproxy.MaxRequestBytes,
	}, add)
	if err != nil {
		var f *failure
		_, isCall := status.FromError(err)
		if (isCall || errors.Is(err, ontologyagent.ErrNoResult)) && errors.As(callFailure("read the people to look up", err), &f) && !f.Permanent {
			return 0, &enrichment.LoadFailure{Err: f}
		}
		return 0, fmt.Errorf("clues: %w", err)
	}
	switch end := end.(type) {
	case ontologyagent.CluesDone:
		return end.Shared, nil
	case ontologyagent.CluesInvalidMappings:
		return 0, &enrichment.LoadFailure{
			Err: permanent(clip("the version's mappings don't fit its tables: "+strings.Join(end.Errors, "; ")), nil),
		}
	case ontologyagent.CluesSourceUnavailable:
		r.snapshots.Forget(sources)
		return 0, &enrichment.LoadFailure{
			Err: retry("the ontology agent could not read a synced table; Helix will retry", fmt.Errorf("source %s unavailable", end.Source)),
		}
	}
	return 0, errors.New("clues: the ontology agent found no [enrichments] item where Helix read one")
}

// tables resolves the version's generation's tables to the snapshots the
// job pinned, and its table files still uploaded in the version's sets to
// their reads (documentTables), as local files under the agent's files root.
// The job's first call pins the newest snapshot of each table, before
// downloading any (SnapshotCache.Pinned), and sets job.Sources; every later
// call, in that attempt or another, reads those. So a load's People
// enrichment step and its Ingest read the same rows, however long the job
// parks while its people are looked up: a parked job is queued, and the
// sweep keeps the snapshots a queued or running job pins.
func (r *runner) tables(ctx context.Context, job *model.FoundryGraphJob, version *model.OntologyVersion, def ontologyagent.Definition) ([]ontologyagent.Source, error) {
	if version.GenerationID == nil {
		return nil, permanent("the version has no generation to name its tables", nil)
	}
	gen, err := r.db.GetOntologyGeneration(ctx, job.OntologyID, *version.GenerationID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, permanent("the version's generation is gone", err)
	}
	if err != nil {
		return nil, retry("could not read the synced data; Helix will retry", err)
	}
	files, err := r.documentTables(ctx, job, version, def, gen)
	if err != nil {
		return nil, err
	}
	// The first attempt pins the snapshots it reads, so a sync that lands
	// before a retry can't change the data under the load.
	pin := func(pins []model.FoundryLoadSource) error {
		if err := r.db.SetFoundryGraphJobSources(ctx, job.ID, r.workerID, pins); errors.Is(err, foundrystore.ErrNotFound) {
			return &ontologygen.Failure{Message: "the load moved to another worker", Err: err}
		} else if err != nil {
			return &ontologygen.Failure{Message: "could not record the synced data the load reads; Helix will retry", Err: err}
		}
		return nil
	}
	sources, pins, err := r.snapshots.Pinned(ctx, job.WorkspaceID, gen.Input.Tables, files, job.Sources, pin)
	var genFailure *ontologygen.Failure
	if errors.As(err, &genFailure) {
		return nil, &failure{Message: genFailure.Message, Permanent: genFailure.Permanent, Err: genFailure.Err}
	}
	if err != nil {
		return nil, retry("could not read the synced data; Helix will retry", err)
	}
	job.Sources = pins
	return sources, nil
}

// serve binds the job's ontology's database and serves a proxy to it for the
// attempt. Every kind of job writes the database, so with
// foundry.graph.load_rate_limit set it first lifts the database's rate limit
// for the attempt (boost). The stop it returns closes the proxy, then lowers
// the limit again; the caller runs it however the attempt ends. The proxy
// lives as long as the attempt: a lease that may lapse ends ctx, and with it
// every write. A query the gateway turns away before running it is sent
// again for up to a third of the agent's Helix timeout, which leaves the
// rest to the query itself.
func (r *runner) serve(ctx context.Context, job *model.FoundryGraphJob) (graphproxy.Binding, ontologyagent.HelixTarget, func(), error) {
	binding, err := graphproxy.Bind(ctx, r.db, r.dbs, job.WorkspaceID, job.OntologyID, job.Kind)
	switch code := status.Code(err); {
	case err == nil:
	case code == codes.NotFound:
		return binding, ontologyagent.HelixTarget{}, nil, permanent(status.Convert(err).Message(), err)
	case code == codes.FailedPrecondition:
		// Paused queries or a cluster mid-change: either can lift before the attempts run out.
		return binding, ontologyagent.HelixTarget{}, nil, retry(status.Convert(err).Message(), err)
	default:
		return binding, ontologyagent.HelixTarget{}, nil, retry("the ontology's database can't be reached; Helix will retry", err)
	}
	release := func() {}
	if r.cfg.Foundry.Graph.LoadRateLimit.Enabled() {
		if release, err = r.boost(ctx, job, binding.TenantID); err != nil {
			return binding, ontologyagent.HelixTarget{}, nil, err
		}
	}
	helixTimeout := time.Duration(r.cfg.Foundry.Graph.HelixTimeoutSecs) * time.Second
	proxy, err := graphproxy.Serve(ctx, binding, r.gateway, func(ctx context.Context) error {
		_, err := graphproxy.Bind(ctx, r.db, r.dbs, job.WorkspaceID, job.OntologyID, job.Kind)
		return err
	}, helixTimeout/3)
	if err != nil {
		release()
		return binding, ontologyagent.HelixTarget{}, nil, retry("the database proxy could not start; Helix will retry", err)
	}
	stop := func() {
		_ = proxy.Close() // best-effort: the next attempt has its own
		release()         // after the proxy closes: no write of the attempt outlives its boost
	}
	return binding, ontologyagent.HelixTarget{
		URL: proxy.URL(), Token: proxy.Token(), MaxRequestBytes: graphproxy.MaxRequestBytes,
		TimeoutSecs: uint64(r.cfg.Foundry.Graph.HelixTimeoutSecs),
	}, stop, nil
}

// update records the attempt's phase and progress; a lost lease ends the attempt.
func (r *runner) update(ctx context.Context, job *model.FoundryGraphJob, phase model.FoundryGraphJobPhase, a *attempt) error {
	err := r.db.UpdateFoundryGraphJob(ctx, job.ID, r.workerID, model.FoundryGraphJobUpdate{
		Phase: phase, Progress: a.progress, IngestViolations: a.ingestViolations,
	})
	if errors.Is(err, foundrystore.ErrNotFound) {
		return retry("the job's lease was lost", err)
	}
	if err != nil {
		log.From(ctx).Warn("record graph job progress failed", "job", job.ID, "err", err)
	}
	return nil
}

// documentTables are the table files the version's generation recorded that
// are still uploaded, read and in the version's sets, which the load reads
// as tables. A file its mappings read that is gone fails the load for good:
// the version maps a table that is no longer there, until it is revised.
func (r *runner) documentTables(ctx context.Context, job *model.FoundryGraphJob, version *model.OntologyVersion, def ontologyagent.Definition,
	gen *model.OntologyGeneration) ([]ontologyagent.DocumentTable, error) {
	if len(gen.Input.Documents) == 0 {
		return nil, nil
	}
	scope, err := r.db.GetOntologyVersionScope(ctx, job.OntologyID, version.ID)
	if err != nil {
		return nil, retry("could not read the version's document sets; Helix will retry", err)
	}
	files, gone, err := ontologyagent.ResolveDocumentTables(ctx, r.db, job.WorkspaceID, gen.Input.Documents, scope)
	if err != nil {
		return nil, retry("could not read the version's files; Helix will retry", err)
	}
	if len(gone) == 0 {
		return files, nil
	}
	mapped, err := ontologyagent.MappedSources(def.MappingsTOML)
	if err != nil {
		return nil, permanent("the version's mappings can't be read", err)
	}
	for _, g := range gone {
		if slices.Contains(mapped, g.ID) {
			return nil, permanent(clip(fmt.Sprintf("the file %q this version maps was deleted or moved out of its document sets; "+
				"revise the version to stop mapping it, or put the file back, then load again", g.Name)), nil)
		}
	}
	return files, nil
}

// ingest loads the tables, at the snapshots the job pinned (tables), and,
// for a version that enriches people (item), the profiles its step chose. They
// are staged as foundry_enrichment in the attempt's own directory, which is
// removed once Ingest returns: the profiles' bytes are the load's, so every
// attempt stages the same ones.
func (r *runner) ingest(ctx context.Context, job *model.FoundryGraphJob, version *model.OntologyVersion, def ontologyagent.Definition,
	binding graphproxy.Binding, target ontologyagent.HelixTarget, ontologyVersion string, item *ontologyagent.Enrichment,
	profiles *model.FoundryEnrichmentApplied, a *attempt) error {
	if err := r.update(ctx, job, model.FoundryGraphJobIngest, a); err != nil {
		return err
	}
	tablesStart := r.now()
	tables, err := r.tables(ctx, job, version, def)
	if err != nil {
		return err
	}
	r.timeStep("tables", tablesStart)
	sources := tables
	if profiles != nil {
		dir := filepath.Join(jobsDir, job.ID, "enrichment")
		if err := os.RemoveAll(filepath.Join(r.snapshots.WorkDir, dir)); err != nil {
			return retry("could not prepare People enrichment's profiles; Helix will retry", err)
		}
		defer func() { _ = os.RemoveAll(filepath.Join(r.snapshots.WorkDir, dir)) }() // best-effort: the next attempt stages afresh
		path := filepath.Join(dir, ontologyagent.EnrichmentSourceID+".jsonl")
		if err := enrichment.Stage(ctx, r.enrichment.Files.Store, *profiles, filepath.Join(r.snapshots.WorkDir, path)); err != nil {
			return retry("could not read People enrichment's profiles; Helix will retry", err)
		}
		sources = append(slices.Clip(tables), ontologyagent.EnrichmentSource(path, profiles.SHA256))
	}
	req := ontologyagent.IngestRequest{
		RequestID: job.ID, Database: binding.TenantID, Load: job.LoadID, OntologyTOML: def.OntologyTOML, OntologyVersion: ontologyVersion,
		Helix: target, MappingsTOML: def.MappingsTOML, Sources: sources, ChunkRows: uint64(r.cfg.Foundry.Graph.ChunkRows),
	}
	if job.Kind == model.FoundryGraphJobApply {
		req.Reload = ontologyagent.ReloadDiff
		changes := filepath.Join(r.snapshots.WorkDir, jobsDir, job.ID, "changes")
		defer func() { _ = os.RemoveAll(changes) }() // best-effort: the next attempt merges afresh
		req.Sources = changeFiles(ctx, r.db, r.snapshots.Snapshots, r.snapshots.WorkDir, job, def.MappingsTOML, sources)
	}
	result, err := r.callIngest(ctx, job, req, a)
	if err == nil && result.Status == ontologyagent.IngestPositionMismatch {
		// The load holds those tables at other positions than their change files start from.
		if req.Sources, err = withoutChanges(req.Sources, result.Positions); err != nil {
			return permanent("the ontology agent returned a result Helix can't read", err)
		}
		req.RequestID = job.ID + ":snapshots"
		result, err = r.callIngest(ctx, job, req, a)
	}
	if err != nil {
		return err
	}
	switch result.Status {
	case ontologyagent.IngestDone:
		// An apply's report counts what it changed, not what the load holds.
		if job.Kind == model.FoundryGraphJobApply {
			a.applied = result.Applied
			return nil
		}
		// The next update records it with the phase past Ingest, which no retry runs again.
		a.ingestViolations = ontologyagent.IngestViolations(result.Report)
		if item == nil {
			return nil
		}
		applied, unmatched, ok := ontologyagent.EnrichmentApplied(result.Report, item.Name)
		if !ok {
			log.From(ctx).Warn("graph job: the ingest report doesn't say what the profiles bound", "job", job.ID)
			return nil
		}
		switch err := r.db.RecordFoundryLoadEnrichmentIngest(ctx, job.LoadID, r.workerID, applied, unmatched); {
		case errors.Is(err, foundrystore.ErrNotFound):
			return retry("the job's lease was lost", err)
		case err != nil:
			log.From(ctx).Warn("graph job: record what the profiles bound failed", "job", job.ID, "err", err)
		}
		return nil
	case ontologyagent.IngestLoadMismatch:
		return permanent(fmt.Sprintf("the database holds this load from another version (%s); start a new load", result.StoredVersion), nil)
	case ontologyagent.IngestLoadUnknown:
		return permanent("this load is being deleted from the database; start a new load", nil)
	case ontologyagent.IngestIndexesPending:
		return &failure{Uncounted: true, Message: "the database is still indexing its data; Helix will retry",
			Err: fmt.Errorf("index on %s still building", result.Label)}
	case ontologyagent.IngestSourcesChanged:
		return permanent("the tables were synced again while this load was written; start a new load", nil)
	case ontologyagent.IngestInvalidMappings:
		return permanent(clip("the version's mappings don't fit its tables: "+strings.Join(result.Errors, "; ")), nil)
	case ontologyagent.IngestSourceUnavailable:
		r.snapshots.Forget(tables)
		return retry("the ontology agent could not read a synced table; Helix will retry", fmt.Errorf("source %s unavailable", result.Source))
	case ontologyagent.IngestOutcomeUnknown:
		return retry("a write may not have landed; Helix will retry once the database settles",
			fmt.Errorf("mapping %s pass %s chunk %d: %s", result.Mapping, result.Pass, result.Chunk, result.Error))
	case ontologyagent.IngestCancelled:
		return retry("the load was stopped; Helix will retry", nil)
	case ontologyagent.IngestDiffUnsupported:
		return unsupported(fmt.Sprintf("%s (%s)", result.Reason, strings.Join(result.Unsupported, ", ")))
	case ontologyagent.IngestPositionMismatch:
		return unsupported("the graph holds the tables at other positions than their snapshots")
	default:
		return retry("the database refused a write; Helix will retry", errors.New(result.Error))
	}
}

// callIngest sends req to Ingest, counting its chunks as they come, and
// keeps its result as the job's ingest report.
func (r *runner) callIngest(ctx context.Context, job *model.FoundryGraphJob, req ontologyagent.IngestRequest, a *attempt) (
	ontologyagent.IngestResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return ontologyagent.IngestResult{}, fmt.Errorf("encode ingest request: %w", err)
	}
	ingestStart := r.now()
	raw, err := r.agent.Ingest(ctx, body, func(e json.RawMessage) error {
		if name, _ := ontologyagent.EventName(e); name != "chunk" {
			return nil
		}
		rows, outcome := chunkCounts(e)
		a.rows += rows
		chunkRows.WithLabelValues(string(job.Kind)).Add(float64(rows))
		chunks.WithLabelValues(string(job.Kind), outcome).Inc()
		a.progress.ChunksDone++
		return r.update(ctx, job, model.FoundryGraphJobIngest, a)
	})
	r.timeStep("ingest", ingestStart)
	if err != nil {
		return ontologyagent.IngestResult{}, callFailure("load the tables", err)
	}
	a.results["ingest"] = raw
	result, err := ontologyagent.DecodeIngestResult(raw)
	if err != nil {
		return ontologyagent.IngestResult{}, permanent("the ontology agent returned a result Helix can't read", err)
	}
	return result, nil
}

// reachedBudget ends a run the job's or the month's budget stopped.
const reachedBudget = "the load reached its budget; documents it did not finish wait for the next load"

// allowanceSet is one set's pending documents, newest first, which a job no
// person started extracts within the set's allowance: what such work may
// spend this UTC month on the set, and on its workspace's sets of its
// connector's kind together.
type allowanceSet struct {
	id        string
	allowance model.FoundrySetAllowance
	docs      []*model.FoundryDocument
}

// extract sends the documents the job's load waits for to Extract, each
// with every artifact saved for it (run, step 5). A job no person started
// splits them (partition): the rest go first, in one Extract under the
// job's and the month's budget (budget); then each set with an allowance,
// in an Extract of its own. Just before a set's Extract its spend is read
// again: with nothing left of its allowance or of its workspace's
// (model.FoundryDocumentSetSpend.Left), its documents are held, never
// downloaded or recorded (helix_foundry_graph_documents_held_total);
// otherwise the Extract spends at most the least of what the budget leaves
// after what the attempt recorded and what the allowance leaves, so a call
// overshoots it by at most the documents in flight. An Extract the
// allowance stops goes on to the next set; one the job's or the month's
// budget stops ends the run, as the first Extract's does. A held or stopped
// set then ends the job budget_exhausted, naming its source: ended
// succeeded with documents it never recorded, the job would be requeued at
// once (FinishFoundryGraphJob). Any budget stop, the allowance's, the
// job's or the month's, hands the sets it left unfinished to the job's
// outcome (HeldSets), so a refresh that held a source's pieces never
// replaces the load it refreshed with less of that source than it had. An
// attempt that owes the sweep and sent nothing sends an empty Extract,
// which spends nothing.
func (r *runner) extract(ctx context.Context, job *model.FoundryGraphJob, def ontologyagent.Definition,
	binding graphproxy.Binding, target ontologyagent.HelixTarget, ontologyVersion string, providers []string, a *attempt) (err error) {
	if err := r.update(ctx, job, model.FoundryGraphJobDocuments, a); err != nil {
		return err
	}
	pending, err := r.db.ListPendingFoundryDocuments(ctx, job.LoadID)
	if err != nil {
		return retry("could not list the documents; Helix will retry", err)
	}
	removed, err := r.db.ListRemovedFoundryDocuments(ctx, job.LoadID)
	if err != nil {
		return retry("could not list the documents to remove; Helix will retry", err)
	}
	// A previous attempt in documents may have committed every document but
	// stopped before Extract's final sweep. Any Extract finishes it.
	needsSweep := job.Phase == model.FoundryGraphJobDocuments
	if len(pending) == 0 && len(removed) == 0 && !needsSweep {
		return r.update(ctx, job, model.FoundryGraphJobFinishing, a)
	}
	free, sets, err := r.partition(ctx, job, pending)
	if err != nil {
		return err
	}
	// finished are the sets whose Extract ended every document it was sent;
	// a budget stop holds the rest (attempt.held).
	finished := map[string]bool{}
	defer func() {
		var f *failure
		if !errors.As(err, &f) || !f.Budget {
			return
		}
		for _, set := range sets {
			if !finished[set.id] {
				a.held = append(a.held, set.id)
			}
		}
	}()
	jobDir := filepath.Join(jobsDir, job.ID)
	if len(pending) > 0 {
		if err := os.RemoveAll(filepath.Join(r.snapshots.WorkDir, jobDir)); err != nil {
			return retry("could not prepare the documents; Helix will retry", err)
		}
		defer func() { _ = os.RemoveAll(filepath.Join(r.snapshots.WorkDir, jobDir)) }() // best-effort: the next attempt starts afresh
	}
	// swept means an Extract reached the run's end, where the engine sweeps.
	swept := false
	if len(removed) > 0 {
		// Removals go first, in an Extract of their own: they spend nothing.
		if _, err := r.send(ctx, job, def, binding, target, ontologyVersion, job.ID+":remove", "remove", []ontologyagent.ExtractDocument{},
			removed, 0, providers, a); err != nil {
			return err
		}
		swept = true
	}
	if len(free) > 0 {
		maxUSD, spent, err := r.budget(ctx, job, a)
		if err != nil {
			return err
		}
		documents, err := r.stage(ctx, job, jobDir, free)
		if err != nil {
			return err
		}
		if len(documents) > 0 {
			exhausted, err := r.send(ctx, job, def, binding, target, ontologyVersion, job.ID, "extract", documents, nil, maxUSD, providers, a)
			switch {
			case err != nil:
				return err
			case exhausted && spent != nil:
				return spent
			case exhausted:
				return &failure{Budget: true, Message: reachedBudget}
			}
			swept = true
		}
	}
	// stop is how the first set its allowance held or stopped ends the job.
	var stop *failure
	for _, set := range sets {
		spends, err := r.db.FoundryDocumentSetSpend(ctx, job.WorkspaceID, []string{set.id}, r.now())
		if err != nil {
			return retry("could not read an API source's spend; Helix will retry", err)
		}
		spend, ok := spends[set.id]
		if !ok {
			return retry("could not read an API source's spend; Helix will retry", fmt.Errorf("document set %s is gone", set.id))
		}
		remaining := spend.Left(set.allowance)
		if remaining <= 0 {
			documentsHeld.WithLabelValues("set_allowance").Add(float64(len(set.docs)))
			if stop == nil {
				stop = allowanceSpent(spend, set.allowance)
			}
			continue
		}
		maxUSD, spent, err := r.budget(ctx, job, a)
		if err != nil {
			return err
		}
		documents, err := r.stage(ctx, job, jobDir, set.docs)
		if err != nil {
			return err
		}
		if len(documents) == 0 {
			finished[set.id] = true
			continue
		}
		remainingUSD := float64(remaining) / 1e6
		exhausted, err := r.send(ctx, job, def, binding, target, ontologyVersion, job.ID+":"+set.id, "extract:"+set.id, documents, nil,
			min(maxUSD, remainingUSD), providers, a)
		switch {
		case err != nil:
			return err
		case !exhausted:
			finished[set.id] = true
		case spent != nil:
			return spent
		case remainingUSD >= maxUSD:
			return &failure{Budget: true, Message: reachedBudget}
		case stop == nil:
			stop = allowanceSpent(spend, set.allowance)
		}
		swept = true
	}
	if needsSweep && !swept {
		// JSON [] even when staging found only deleted originals.
		exhausted, err := r.send(ctx, job, def, binding, target, ontologyVersion, job.ID, "extract", []ontologyagent.ExtractDocument{}, nil, 0,
			providers, a)
		switch {
		case err != nil:
			return err
		case exhausted && stop == nil:
			return &failure{Budget: true, Message: reachedBudget}
		}
	}
	if stop != nil {
		return stop
	}
	return r.update(ctx, job, model.FoundryGraphJobFinishing, a)
}

// partition splits the documents a job sends. For a job no person started,
// those of each set whose connector's kind has an allowance
// (config.Foundry.SetAllowances: an API source's) go to that set's
// own Extract: the sets in the order of their newest pending document, each
// one's documents newest first, as ListPendingFoundryDocuments orders them.
// The rest are free, under the job's and the month's budget alone: uploads,
// other kinds' documents, and the documents a refresh replays, which the
// load it refreshes recorded extracted, duplicate or out of scope. Their
// saved artifacts replay at no cost (unless the engine, its prompts or its
// models changed), and the refresh replaces that load only once it holds
// them all, partial or not (FinishFoundryGraphJob). Any other job replays
// none: an extract's documents are new to its load, and what another
// ontology's loads extracted is theirs. A person's job has no allowance:
// every document is free. One read of the sets' spend, by primary key; a
// refresh also reads its load, and what the load it refreshes recorded of
// the sets' documents.
func (r *runner) partition(ctx context.Context, job *model.FoundryGraphJob, pending []*model.FoundryDocument) (
	free []*model.FoundryDocument, sets []*allowanceSet, err error) {
	if job.RequestedByUserID != nil || len(pending) == 0 {
		return pending, nil, nil
	}
	var setIDs []string
	seen := map[string]bool{}
	for _, doc := range pending {
		if !seen[doc.SetID] {
			seen[doc.SetID] = true
			setIDs = append(setIDs, doc.SetID)
		}
	}
	spend, err := r.db.FoundryDocumentSetSpend(ctx, job.WorkspaceID, setIDs, r.now())
	if err != nil {
		return nil, nil, retry("could not read the document sets' spend; Helix will retry", err)
	}
	allowances := r.cfg.Foundry.SetAllowances()
	limited := map[string]*allowanceSet{}
	for _, id := range setIDs {
		owned, isOwned := spend[id]
		if allowance, ok := allowances[owned.ConnectorKind]; isOwned && ok {
			limited[id] = &allowanceSet{id: id, allowance: allowance}
			sets = append(sets, limited[id])
		}
	}
	if len(sets) == 0 {
		return pending, nil, nil
	}
	replays := map[string]bool{}
	if job.Kind == model.FoundryGraphJobLoad && job.Origin == model.FoundryLoadByRefresh {
		load, err := r.db.GetFoundryLoad(ctx, job.OntologyID, job.LoadID)
		if err != nil {
			return nil, nil, retry("could not read the load; Helix will retry", err)
		}
		if load.RefreshesLoadID != nil {
			var ids []string
			for _, doc := range pending {
				if limited[doc.SetID] != nil {
					ids = append(ids, doc.ID)
				}
			}
			prior, err := r.db.ListFoundryDocumentExtractions(ctx, *load.RefreshesLoadID, ids)
			if err != nil {
				return nil, nil, retry("could not read what the refreshed load recorded; Helix will retry", err)
			}
			for id, e := range prior {
				switch e.Status {
				case model.FoundryExtractionExtracted, model.FoundryExtractionDuplicate, model.FoundryExtractionOutOfScope:
					replays[id] = true
				}
			}
		}
	}
	for _, doc := range pending {
		if set := limited[doc.SetID]; set != nil && !replays[doc.ID] {
			set.docs = append(set.docs, doc)
		} else {
			free = append(free, doc)
		}
	}
	// A set whose every document replays has no Extract of its own.
	return free, slices.DeleteFunc(sets, func(s *allowanceSet) bool { return len(s.docs) == 0 }), nil
}

// allowanceSpent ends a job no person started whose set's allowance held or
// stopped documents, naming the set's source: its own allowance when that
// had no more left than its workspace's, else the workspace's. Only API
// sources (connector kind events) have an allowance.
func allowanceSpent(spend model.FoundryDocumentSetSpend, a model.FoundrySetAllowance) *failure {
	if a.SetMicros-spend.USDMicros <= a.WorkspaceMicros-spend.WorkspaceUSDMicros {
		return &failure{Budget: true, Message: clip(fmt.Sprintf("the API source %q spent its $%.2f of this month's extraction by work no one "+
			"started; its newest pieces wait for next month or a load a person starts", spend.ConnectorName, float64(a.SetMicros)/1e6))}
	}
	return &failure{Budget: true, Message: clip(fmt.Sprintf("the workspace's API sources spent their $%.2f of this month's extraction by work "+
		"no one started; the newest pieces of %q wait for next month or a load a person starts", float64(a.WorkspaceMicros)/1e6,
		spend.ConnectorName))}
}

// send is one Extract of documents into the job's load, spending at most
// maxUSD, under requestID, its result kept in the job's report as call. It
// runs on the models and key the workspace's AI setting resolves to just
// before the call, of one of providers, the ones the agent calls.
// Each artifact the agent sends is saved before its document commits, and
// each document's end is recorded as it comes. Exhausted means the run
// ended budget_exhausted, what it did not finish waiting; a done run must
// have ended every document it was sent. Any other end is the attempt's
// failure, and a request past the agent's message limit fails for good.
func (r *runner) send(ctx context.Context, job *model.FoundryGraphJob, def ontologyagent.Definition, binding graphproxy.Binding,
	target ontologyagent.HelixTarget, ontologyVersion, requestID, call string, documents []ontologyagent.ExtractDocument, remove []string,
	maxUSD float64, providers []string, a *attempt) (exhausted bool, err error) {
	a.progress.DocumentsTotal += len(documents)
	if err := r.update(ctx, job, model.FoundryGraphJobDocuments, a); err != nil {
		return false, err
	}
	decisions, err := r.decisions(ctx, job, def.OntologyTOML, documents)
	if err != nil {
		return false, err
	}
	resolved, err := r.ai.ResolveServed(ctx, job.WorkspaceID, providers)
	if err != nil {
		c := ai.ResolveFailure(err).Logged(ctx, err)
		return false, &failure{Message: c.Message, Permanent: c.Permanent, Err: err}
	}
	body, err := json.Marshal(ontologyagent.ExtractRequest{
		RequestID: requestID, Database: binding.TenantID, Load: job.LoadID, OntologyTOML: def.OntologyTOML, OntologyVersion: ontologyVersion,
		MappingsTOML: def.MappingsTOML, Helix: target, Documents: documents, Decisions: decisions,
		Limits: ontologyagent.ExtractLimits{
			MaxUSD: maxUSD, DocumentsInFlight: uint64(r.cfg.Foundry.Graph.DocumentsInFlight),
			MaxTextBytes: uint64(r.cfg.Foundry.Documents.MaxBytes),
		},
		Models: resolved.Extract(),
		Remove: remove,
	})
	if err != nil {
		return false, fmt.Errorf("encode extract request: %w", err)
	}
	if len(body) > r.maxRequestBytes {
		// gRPC would refuse it as a full agent, and every retry would too.
		return false, permanent("this version reads more documents than one load can send",
			fmt.Errorf("the extract request is %d bytes; the agent takes %d", len(body), r.maxRequestBytes))
	}
	sent := map[string]bool{}
	for _, d := range documents {
		sent[d.DocID] = true
	}
	artifacts := map[string]string{}
	done := a.progress.DocumentsDone
	// removing are the documents of remove not yet recorded as taken out.
	removing := map[string]bool{}
	for _, docID := range remove {
		removing[docID] = true
	}
	raw, err := r.agent.Extract(ctx, body, resolved.Key, func(e json.RawMessage) error {
		switch name, _ := ontologyagent.EventName(e); name {
		case "artifact":
			r.saveArtifact(ctx, job, e, sent, artifacts)
		case "document":
			return r.recordDocument(ctx, job, e, sent, artifacts, resolved, a)
		case "document_removed":
			var removed ontologyagent.DocumentRemoved
			if err := json.Unmarshal(e, &removed); err != nil || !removing[removed.DocID] {
				log.From(ctx).Warn("graph job: the ontology agent removed a document Helix didn't send", "job", job.ID, "event", string(e))
				return nil
			}
			log.From(ctx).Info("graph job: a document left the load", "job", job.ID, "document", removed.DocID,
				"facts", removed.FactsRemoved, "entities", removed.EntitiesRemoved)
			return r.recordRemoval(ctx, job, removed.DocID, removing)
		}
		return nil
	})
	if err != nil {
		return false, callFailure("extract the documents", err)
	}
	a.results[call] = raw
	result, err := ontologyagent.DecodeExtractResult(raw)
	if err != nil {
		return false, permanent("the ontology agent returned a result Helix can't read", err)
	}
	switch result.Status {
	case ontologyagent.ExtractDone:
		// Done took out every document of remove, those the load didn't hold included.
		for _, docID := range remove {
			if err := r.recordRemoval(ctx, job, docID, removing); err != nil {
				return false, err
			}
		}
		// Done means every document ended; one that didn't would be sent, and merged, forever.
		if ended := a.progress.DocumentsDone - done; ended < len(documents) {
			return false, retry("the ontology agent left documents unfinished; Helix will retry",
				fmt.Errorf("%d of %d documents ended", ended, len(documents)))
		}
		return false, nil
	case ontologyagent.ExtractBudgetExhausted:
		return true, nil
	case ontologyagent.ExtractLoadMismatch:
		return false, permanent(fmt.Sprintf("the database holds this load from another version (%s)", result.StoredVersion), nil)
	case ontologyagent.ExtractLoadUnknown:
		return false, permanent("the database does not hold this load, or it is being deleted; make another load current or start a new one", nil)
	case ontologyagent.ExtractIndexesPending:
		return false, &failure{Uncounted: true, Message: "the database is still indexing its data; Helix will retry",
			Err: fmt.Errorf("index on %s still building", result.Label)}
	case ontologyagent.ExtractCancelled:
		return false, retry("the load was stopped; Helix will retry", nil)
	case ontologyagent.ExtractOutcomeUnknown:
		return false, retry("a document's write may not have landed; Helix will retry once the database settles",
			fmt.Errorf("document %s: %s", result.DocID, result.Error))
	default:
		return false, retry("the database refused a write; Helix will retry", errors.New(result.Error))
	}
}

// recordRemoval forgets a document the agent took out of the job's load,
// once: a lost lease ends the attempt, and any other failure leaves it for
// the next extract to send again, which changes nothing in the graph.
func (r *runner) recordRemoval(ctx context.Context, job *model.FoundryGraphJob, docID string, removing map[string]bool) error {
	if !removing[docID] {
		return nil
	}
	delete(removing, docID)
	switch err := r.db.RecordFoundryDocumentRemoval(ctx, job.ID, r.workerID, docID); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return retry("the job's lease was lost", err)
	case err != nil:
		log.From(ctx).Warn("graph job: record a document's removal failed", "job", job.ID, "document", docID, "err", err)
		return nil
	}
	documentsRemoved.Inc()
	return nil
}

// deleteLoad deletes the job's load from the ontology's database, a batch
// per request. DeleteLoad is idempotent and resumable: a retry goes on from
// whatever is left, and a load already gone is done.
func (r *runner) deleteLoad(ctx context.Context, job *model.FoundryGraphJob, a *attempt) error {
	if err := r.update(ctx, job, model.FoundryGraphJobDeleting, a); err != nil {
		return err
	}
	binding, target, stop, err := r.serve(ctx, job)
	if err != nil {
		return err
	}
	defer stop()
	body, err := json.Marshal(ontologyagent.DeleteLoadRequest{
		RequestID: job.ID, Database: binding.TenantID, Load: job.LoadID, Helix: target, Batch: uint64(r.cfg.Foundry.Graph.DeleteBatch),
	})
	if err != nil {
		return fmt.Errorf("encode delete load request: %w", err)
	}
	raw, err := r.agent.DeleteLoad(ctx, body, func(json.RawMessage) error { return nil })
	if err != nil {
		return callFailure("delete a load", err)
	}
	a.results["delete"] = raw
	result, err := ontologyagent.DecodeDeleteLoadResult(raw)
	if err != nil {
		return permanent("the ontology agent returned a result Helix can't read", err)
	}
	switch result.Status {
	case ontologyagent.DeleteLoadDone:
		return nil
	case ontologyagent.DeleteLoadCancelled:
		return retry("the delete was stopped; Helix will retry", nil)
	case ontologyagent.DeleteLoadOutcomeUnknown:
		return retry("a delete may not have landed; Helix will retry", fmt.Errorf("label %s: %s", result.Label, result.Error))
	default:
		return retry("the database refused a delete; Helix will retry", errors.New(result.Error))
	}
}

// budget is what an Extract of the attempt may spend, in dollars: what is
// left of the job's cap, less what the attempt recorded, and of the
// workspace's month, whichever is less. A job no person started (a refresh,
// or an extract no one asked for) has the month less a job's cap
// (model.FoundryGraphBudget.CapFor), which a person's work keeps. With
// nothing left it is 0, and spent is how the run ends if it stops: the
// documents still go, with nothing to spend, so those whose saved artifacts
// replay commit at $0 (a refresh's, mostly), the rest wait, and the run ends
// budget_exhausted, or done when every one replayed. It also reaches the
// run's end, where the engine sweeps what an earlier attempt committed.
func (r *runner) budget(ctx context.Context, job *model.FoundryGraphJob, a *attempt) (maxUSD float64, spent *failure, err error) {
	g := r.cfg.Foundry.Graph
	month := g.MonthBudget(r.now())
	monthSpent, err := r.db.SumFoundryGraphJobSpend(ctx, job.WorkspaceID, month.Since)
	if err != nil {
		return 0, nil, retry("could not read the month's spend; Helix will retry", err)
	}
	monthCap := month.CapFor(job.RequestedByUserID)
	jobLeft := int64(g.MaxJobUSD*1e6) - job.Spend.USDMicros - a.spent
	monthLeft := monthCap - monthSpent
	switch {
	case jobLeft <= 0:
		return 0, &failure{Budget: true, Message: fmt.Sprintf("the load spent its $%.2f cap", g.MaxJobUSD)}, nil
	case monthLeft <= 0 && job.RequestedByUserID == nil:
		return 0, &failure{Budget: true, Message: fmt.Sprintf(
			"the workspace spent the $%.2f of this month's $%.2f that work no one started may use; the rest is kept for loads people start",
			float64(monthCap)/1e6, g.MonthlyUSD)}, nil
	case monthLeft <= 0:
		return 0, &failure{Budget: true, Message: fmt.Sprintf("the workspace spent this month's $%.2f of loads", g.MonthlyUSD)}, nil
	}
	return float64(min(jobLeft, monthLeft)) / 1e6, nil, nil
}

// stage downloads each pending document's text and every artifact saved for
// it under jobDir, for the agent, which reads the work directory and replays
// each model call it finds in any of them. The text is a text document's
// original, or what a PDF's read stored (model.FoundryDocument.TextKey),
// staged as ontologyagent.StageText writes it: a PDF is plain_text. A
// document whose text is gone is left out and marked deleted: a delete that
// removed its prefix but not the row would otherwise be owed, and merged,
// forever. A document with no text to send is never sent, and is recorded
// failed, which no later job sends again: a PDF whose read failed with the
// read's code (the document's read says why), anything else as unsupported.
// A PDF still being read never gets here: it is not pending until its read
// ends.
func (r *runner) stage(ctx context.Context, job *model.FoundryGraphJob, jobDir string, pending []*model.FoundryDocument) ([]ontologyagent.ExtractDocument, error) {
	root := r.snapshots.WorkDir
	var out []ontologyagent.ExtractDocument
	for _, doc := range pending {
		key, ok := doc.TextKey()
		if !ok {
			code := model.FoundryExtractionUnsupported
			if doc.Read != nil && doc.Read.Error != nil {
				code = model.FoundryExtractionErrorCode(doc.Read.Error.Code)
			}
			err := r.db.RecordFoundryDocumentExtraction(ctx, job.ID, r.workerID, &model.FoundryDocumentExtraction{
				DocumentID: doc.ID, Status: model.FoundryExtractionFailed,
				Error: &model.FoundryExtractionError{Code: code, Retryable: false},
			}, false)
			if errors.Is(err, foundrystore.ErrNotFound) {
				return nil, retry("the job's lease was lost", err)
			}
			if err != nil {
				return nil, retry("could not record a document; Helix will retry", err)
			}
			continue
		}
		body, err := r.documents.Open(ctx, key)
		if errors.Is(err, document.ErrMissing) {
			if err := r.db.DeleteFoundryDocument(ctx, doc.WorkspaceID, doc.ID); err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
				return nil, retry("could not read a document; Helix will retry", err)
			}
			continue
		}
		if err != nil {
			return nil, retry("could not read a document; Helix will retry", err)
		}
		stored, err := io.ReadAll(body)
		_ = body.Close() // best-effort: only read
		if err != nil {
			return nil, retry("could not read a document; Helix will retry", err)
		}
		media, ext, text, _ := ontologyagent.StageText(doc.MediaType, stored) // a document with text, checked above
		docPath := path.Join(jobDir, "docs", doc.ID+ext)
		if err := writeFile(filepath.Join(root, docPath), bytes.NewReader(text)); err != nil {
			return nil, retry("could not prepare the documents; Helix will retry", err)
		}
		prefix := document.ArtifactPrefix(job.WorkspaceID, doc.ID)
		keys, err := r.documents.List(ctx, prefix)
		if err != nil {
			return nil, retry("could not read a document's saved extraction; Helix will retry", err)
		}
		saved := []string{}
		for _, key := range keys {
			name, ok := strings.CutSuffix(strings.TrimPrefix(key, prefix), ".json.gz")
			if !ok || strings.Contains(name, "/") {
				continue
			}
			raw, err := r.documents.OpenArtifact(ctx, key)
			if err != nil {
				return nil, retry("could not read a document's saved extraction; Helix will retry", err)
			}
			artifactPath := path.Join(jobDir, "artifacts", doc.ID, name+".json")
			if err := writeFile(filepath.Join(root, artifactPath), bytes.NewReader(raw)); err != nil {
				return nil, retry("could not prepare the documents; Helix will retry", err)
			}
			saved = append(saved, artifactPath)
		}
		out = append(out, ontologyagent.ExtractDocument{
			DocID: doc.ID, Set: doc.SetID, URI: "s3://" + r.bucket + "/" + doc.S3Key, Path: docPath, MediaType: media, Artifacts: saved,
		})
	}
	return out, nil
}

// saveArtifact stores a document's model output so a later run replays it.
// A failure costs only a later run's model calls, so it is logged, not fatal.
func (r *runner) saveArtifact(ctx context.Context, job *model.FoundryGraphJob, e json.RawMessage, sent map[string]bool, saved map[string]string) {
	artifact, err := ontologyagent.DecodeArtifact(e)
	if err != nil || !sent[artifact.DocID] {
		log.From(ctx).Warn("graph job: ignored an artifact", "job", job.ID, "err", err)
		return
	}
	key, ok := document.ArtifactKey(job.WorkspaceID, artifact.DocID, artifact.Key)
	if !ok {
		log.From(ctx).Warn("graph job: ignored an artifact with an unusable key", "job", job.ID, "document", artifact.DocID)
		return
	}
	if err := r.documents.PutArtifact(ctx, key, []byte(artifact.JSON)); err != nil {
		log.From(ctx).Warn("graph job: save artifact failed", "job", job.ID, "document", artifact.DocID, "err", err)
		return
	}
	saved[artifact.DocID] = key
}

// recordDocument stores how a document ended, with what it spent, which
// the attempt counts. A document whose model call failed keeps the code
// model; when the failure is the workspace's own provider's that every
// document would meet (its key, account, models or endpoint), the load then
// stops as partial, worded as ai.DocumentFailure says, and the documents it
// didn't reach wait for the next load.
func (r *runner) recordDocument(ctx context.Context, job *model.FoundryGraphJob, e json.RawMessage, sent map[string]bool,
	artifacts map[string]string, resolved ai.Resolved, a *attempt) error {
	event, err := ontologyagent.DecodeDocumentEvent(e)
	if err != nil {
		return permanent("the ontology agent sent a document Helix can't read", err)
	}
	if !sent[event.DocID] {
		log.From(ctx).Warn("graph job: ignored a document it did not send", "job", job.ID, "document", event.DocID)
		return nil
	}
	c, u := event.Counts, event.Usage
	extraction := &model.FoundryDocumentExtraction{
		DocumentID: event.DocID, Status: model.FoundryExtractionExtracted,
		Counts: model.FoundryExtractionCounts{
			Attributes: int(c.Attributes), AttributeReviews: int(c.AttributeReviews), PropConflicts: int(c.PropConflicts),
			Created: int(c.Created), IdentifiersUnverified: int(c.IdentifiersUnverified), AttributesOverEntries: int(c.AttributesOverEntries),
			ResolvedUser: int(c.Resolved.User), ResolvedAdjudicated: int(c.Resolved.Adjudicated),
			ResolvedIdentifier: int(c.Resolved.Identifier), ResolvedName: int(c.Resolved.Name),
			ResolvedAlias: int(c.Resolved.Alias), ResolvedCreated: int(c.Resolved.Created),
			Sections: int(event.Sections), Entities: int(c.Entities), Mentions: int(c.Mentions), Facts: int(c.Facts),
			Reviews: int(c.Reviews), Unresolved: int(c.Unresolved), Rejected: int(c.Rejected),
		},
		USDMicros: int64(u.USDMicros), InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens),
	}
	if key, ok := artifacts[event.DocID]; ok {
		extraction.ArtifactKey = &key
	}
	switch event.Outcome.Kind {
	case ontologyagent.OutcomeOutOfScope:
		extraction.Status = model.FoundryExtractionOutOfScope
	case ontologyagent.OutcomeDuplicate:
		of := event.Outcome.Of
		extraction.Status, extraction.DuplicateOf = model.FoundryExtractionDuplicate, &of
	case ontologyagent.OutcomeFailed:
		extraction.Status = model.FoundryExtractionFailed
		extraction.Error = &model.FoundryExtractionError{
			Code: model.FoundryExtractionErrorCode(event.Outcome.Code), Retryable: event.Outcome.Retryable,
		}
	}
	keep := event.Outcome.Kind == ontologyagent.OutcomeAlreadyIngested
	err = r.db.RecordFoundryDocumentExtraction(ctx, job.ID, r.workerID, extraction, keep)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return retry("the job's lease was lost", err)
	}
	if err != nil {
		return retry("could not record a document; Helix will retry", err)
	}
	a.progress.DocumentsDone++
	a.spent += extraction.USDMicros
	if err := r.update(ctx, job, model.FoundryGraphJobDocuments, a); err != nil {
		return err
	}
	if f := event.Outcome.ModelFailure; f != "" {
		cause := fmt.Errorf("document %s: the model failed: %s", event.DocID, f)
		c, stop := ai.DocumentFailure(resolved, f)
		if stop {
			// Ends the load as partial, as a spent budget does: what is in stays.
			return &failure{Budget: true, Message: c.Message, Err: cause}
		}
		c.Logged(ctx, cause)
	}
	return nil
}

// callFailure classifies a failed Ingest, Extract or DeleteLoad call. The
// agent's own refusals come before any event; a dropped stream or a deadline
// may get through on another attempt, and the agent skips what is already
// stored or deleted. FAILED_PRECONDITION is Extract on models the agent
// can't run (a provider it doesn't call, a model it can't price): no retry
// fixes that.
func callFailure(what string, err error) error {
	var f *failure
	if errors.As(err, &f) {
		return f
	}
	switch status.Code(err) {
	case codes.InvalidArgument:
		return permanent("the ontology agent refused the request to "+what, err)
	case codes.Unimplemented:
		return permanent("the ontology agent does not "+what+"; it must serve contract "+ontologyagent.ContractVersion, err)
	case codes.FailedPrecondition:
		return permanent("the ontology agent can't run these models, so it can't "+what, err)
	case codes.Aborted:
		return retry("another job is writing this database; Helix will retry", err)
	}
	return retry("the ontology agent is unavailable; Helix will retry", err)
}

// clip keeps a message for the customer under a kilobyte.
func clip(message string) string {
	const limit = 1000
	if len(message) <= limit {
		return message
	}
	return strings.ToValidUTF8(message[:limit], "") + "…"
}

// writeFile writes body to path whole, making its directory.
func writeFile(path string, body io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
