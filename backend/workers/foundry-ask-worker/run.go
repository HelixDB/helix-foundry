package foundryaskworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/boost"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// flushEvery is how often a running question's new events are stored.
const flushEvery = 500 * time.Millisecond

// store is what an attempt reads and records of Foundry's store.
type store interface {
	graphproxy.Store
	GetFoundryLoad(ctx context.Context, ontologyID, id string) (*model.FoundryLoad, error)
	GetOntologyVersion(ctx context.Context, ontologyID, id string) (*model.OntologyVersion, error)
	GetOntologyGeneration(ctx context.Context, ontologyID, id string) (*model.OntologyGeneration, error)
	ListConnectors(ctx context.Context, workspaceID string) ([]*model.Connector, error)
	ListFoundryDocumentSets(ctx context.Context, workspaceID string) ([]*model.FoundryDocumentSet, error)
	ListFoundryDocumentsByID(ctx context.Context, workspaceID string, ids []string) (map[string]*model.FoundryDocument, error)
	GetOntologyVersionScope(ctx context.Context, ontologyID, versionID string) ([]string, error)
	ListFoundryQuestionHistory(ctx context.Context, q *model.FoundryQuestion, limit int) ([]*model.FoundryQuestion, error)
	AppendFoundryQuestionEvents(ctx context.Context, id, workerID string, events []json.RawMessage) error
	ListFoundryAccessGrants(ctx context.Context, workspaceID string, principals []access.Principal) ([]model.FoundryAccessGrant, error)
	ListFoundryCurrentLoadInputs(ctx context.Context, workspaceID string, ontologyIDs []string) (map[string]model.FoundryLoadInputs, error)
	ListFoundryLoadInputs(ctx context.Context, workspaceID string, loadIDs []string) (map[string]model.FoundryLoadInputs, error)
	FoundryLoadHoldings(ctx context.Context, workspaceID, loadID string) (access.Holdings, error)
	SetFoundryQuestionSeen(ctx context.Context, questionID string, seen *access.Inputs) error
	FoundryTeamsOf(ctx context.Context, workspaceID, userID string) ([]string, error)
}

// agent is the sidecar; *ontologyagent.Client calls it.
type agent interface {
	Info(ctx context.Context) (*agentv1.GetInfoResponse, error)
	Ask(ctx context.Context, request []byte, key ontologyagent.ModelKey, onEvent func(json.RawMessage) error) (json.RawMessage, error)
	Probe(ctx context.Context, request []byte, key ontologyagent.ModelKey) (json.RawMessage, error)
}

// resolver is where a workspace's model calls go; *ai.Resolver resolves it.
type resolver interface {
	ResolveServed(ctx context.Context, workspaceID string, providers []string) (ai.Resolved, error)
}

// failure is why an attempt did not end with the agent's result, worded for
// the user. Permanent fails the question; anything else is retried, and an
// Uncounted one (the agent too busy to start) gives its attempt back.
type failure struct {
	Message   string
	Permanent bool
	Uncounted bool
	Err       error
}

func (f *failure) Error() string {
	if f.Err != nil {
		return f.Message + ": " + f.Err.Error()
	}
	return f.Message
}

func (f *failure) Unwrap() error { return f.Err }

func retry(message string, err error) error { return &failure{Message: message, Err: err} }

func permanent(message string, err error) error {
	return &failure{Message: message, Permanent: true, Err: err}
}

// attempt is what one attempt leaves: its outcome when the agent sent a
// result, and what it spent either way.
type attempt struct {
	outcome model.FoundryQuestionOutcome
}

// runner runs one attempt of a question.
type runner struct {
	db  store
	dbs ports.Databases
	// boosts lift its database's rate limit for an attempt; nil never does.
	boosts ports.Boosts
	agent  agent
	// ai resolves, every attempt, the model and key the question runs on.
	ai resolver
	// proxy is the pod's read-only proxy: each attempt registers its binding
	// on it for the attempt's life.
	proxy    *graphproxy.ReadProxy
	cfg      *config.Config
	workerID string
	// flushEvery is how often new events are stored (flushEvery).
	flushEvery time.Duration
}

// run runs an attempt of q. lost stops the attempt for good: a cancel or
// another worker took the question.
//  1. Check the agent speaks the vendored contract, waiting up to
//     config.AskReadyWait for a sidecar still starting; another contract fails
//     the question. Resolve the workspace's AI setting: the model and key the
//     question runs on, of a provider the agent calls.
//  2. Read the load, which must still be ready or partial, its version's
//     Definition, the generation's goal and tables, and the thread's history;
//     take the question's focus as the entity it names.
//  3. Bind the ontology's database and register the binding on the pod's
//     read-only proxy until run returns; with foundry.graph.load_rate_limit
//     set, lift the database's limit for the question first, without waiting
//     for the gateway's sync.
//  4. Ask, the key in the call's metadata, storing the events every
//     flushEvery as they come, and the rest once the call ends, before the
//     question is settled. The started event names the model asked, which
//     is logged.
//
// Its error is a *failure, or another error to retry on.
func (r *runner) run(ctx context.Context, q *model.FoundryQuestion, lost func()) (*attempt, error) {
	a := &attempt{}
	infoCtx, cancelInfo := context.WithTimeout(ctx, config.AskReadyWait)
	info, err := r.agent.Info(infoCtx)
	cancelInfo()
	if err != nil {
		return a, retry("the ontology agent is unavailable; Helix will retry", err)
	}
	if info.GetContractVersion() != ontologyagent.ContractVersion {
		return a, permanent(fmt.Sprintf("the ontology agent speaks contract %s, not %s", info.GetContractVersion(), ontologyagent.ContractVersion), nil)
	}
	resolved, err := r.ai.ResolveServed(ctx, q.WorkspaceID, info.GetProviders())
	if err != nil {
		c := ai.ResolveFailure(err).Logged(ctx, err)
		return a, &failure{Message: c.Message, Permanent: c.Permanent, Err: err}
	}
	held, err := r.granted(ctx, q)
	if err != nil {
		return a, err
	}
	request, err := r.request(ctx, q, held)
	if err != nil {
		return a, err
	}
	request.Models = resolved.Ask()
	// A deleted ontology is not asked.
	bind := func(ctx context.Context) (graphproxy.Binding, error) {
		return graphproxy.BindRead(ctx, r.db, r.dbs, q.WorkspaceID, q.OntologyID)
	}
	binding, err := bind(ctx)
	switch code := status.Code(err); {
	case err == nil:
	case code == codes.NotFound:
		return a, permanent(status.Convert(err).Message(), err)
	case code == codes.FailedPrecondition:
		return a, retry(status.Convert(err).Message(), err)
	default:
		return a, retry("the ontology's database can't be reached; Helix will retry", err)
	}
	release := func() {}
	if limit := r.cfg.Foundry.Graph.LoadRateLimit; limit.Enabled() {
		booster := boost.Booster{Boosts: r.boosts, WorkerID: r.workerID, Limit: limit}
		release, err = booster.Acquire(ctx, boost.Attempt{
			Holder: model.FoundryBoostQuestion, ID: q.ID, Number: q.Attempts, TenantID: binding.TenantID, Timeout: r.cfg.Foundry.Ask.AttemptTimeout(),
		})
		if errors.Is(err, foundrystore.ErrNotFound) {
			lost()
			return a, retry("the question's lease was lost", err)
		}
	}
	defer release() // after endCall: no read of the question outlives its boost
	// The question's token, and any of its reads in flight, end with callCtx.
	// An attempt outlives graphproxy.RecheckEvery, so its binding is checked
	// again: a database paused or deleted meanwhile takes no more of its reads.
	callCtx, endCall := context.WithCancel(ctx)
	defer endCall()
	token, err := r.proxy.Register(callCtx, binding, func(ctx context.Context) error {
		_, err := bind(ctx)
		return err
	})
	if err != nil {
		return a, retry("the database proxy could not take the question; Helix will retry", err)
	}
	request.Database = binding.TenantID
	request.Helix = ontologyagent.HelixTarget{
		URL: r.proxy.URL(), Token: token, MaxRequestBytes: graphproxy.MaxRequestBytes, TimeoutSecs: uint64(r.cfg.Foundry.Ask.HelixTimeoutSecs),
	}
	body, err := json.Marshal(request)
	if err != nil {
		return a, permanent("the question could not be sent to the agent", err)
	}
	if err := r.ask(ctx, q, body, resolved, lost, a); err != nil {
		return a, err
	}
	// A grantee whose grants were taken away while it ran gets no answer, and
	// one who holds less of the load than it did gets none it could say more in.
	still, err := r.granted(ctx, q)
	if err == nil && !still.within(held) {
		err = errNoLongerGranted
	}
	if err != nil {
		a.outcome.Result = nil
		return a, err
	}
	return a, nil
}

// noLongerGranted fails a grantee's question its grants no longer cover.
const noLongerGranted = "your access to this ontology changed; ask again"

var errNoLongerGranted = permanent(noLongerGranted, nil)

// heldLoad is what the grantee that asked a question holds of its load.
type heldLoad struct {
	grants *access.Grants
	// part is what it holds, when it isn't all of the load.
	part *access.Part
	// visibility is what the agent is told it holds, all of it or that part: a
	// grantee reads nothing raw or historic, which holds what the load no
	// longer does.
	visibility ontologyagent.Visibility
	// inputs are what the load holds.
	inputs access.Inputs
}

// within reports whether h, read after an answer, still holds all that the
// answer read, before.
func (h heldLoad) within(before heldLoad) bool {
	if before.grants == nil || before.part == nil {
		return h.part == nil
	}
	return h.part == nil || h.part.Holds(*before.part)
}

// granted is what the grants of the grantee that asked q (and of its teams,
// now) hold of its load, none for a question someone else asked: q's load must
// still be its ontology's current one, and they must hold some of it, or the
// question fails (errNoLongerGranted).
func (r *runner) granted(ctx context.Context, q *model.FoundryQuestion) (heldLoad, error) {
	if q.Grantee == nil {
		return heldLoad{}, nil
	}
	principals := []access.Principal{*q.Grantee}
	if q.Grantee.Kind == access.PrincipalUser {
		teams, err := r.db.FoundryTeamsOf(ctx, q.WorkspaceID, q.Grantee.ID)
		if err != nil {
			return heldLoad{}, retry("could not read your access; Helix will retry", err)
		}
		principals = access.Grantee{Principal: *q.Grantee, Teams: teams}.Principals()
	}
	held, err := r.db.ListFoundryAccessGrants(ctx, q.WorkspaceID, principals)
	if err != nil {
		return heldLoad{}, retry("could not read your access; Helix will retry", err)
	}
	targets := make([]access.Target, len(held))
	for i, g := range held {
		targets[i] = g.Target
	}
	grants := access.NewGrants(targets)
	loads, err := r.db.ListFoundryCurrentLoadInputs(ctx, q.WorkspaceID, []string{q.OntologyID})
	if err != nil {
		return heldLoad{}, retry("could not read your access; Helix will retry", err)
	}
	load, ok := loads[q.OntologyID]
	if !ok || load.LoadID != q.LoadID || !grants.Reaches(load.Inputs) {
		return heldLoad{}, errNoLongerGranted
	}
	holdings, err := r.db.FoundryLoadHoldings(ctx, q.WorkspaceID, q.LoadID)
	if err != nil {
		return heldLoad{}, retry("could not read your access; Helix will retry", err)
	}
	part := grants.Part(holdings)
	out := heldLoad{grants: &grants, visibility: ontologyagent.VisibilityOf(part), inputs: load.Inputs}
	if grants.Covers(load.Inputs) {
		out.visibility.Documents = ontologyagent.VisibleDocuments{All: true}
		return out, nil
	}
	out.part = &part
	return out, nil
}

// request builds the question's AskRequest, all but its Helix target. A
// grantee's question is asked without the goal its ontology was drafted from,
// and with only what it holds of the catalog; of a load it holds only some of,
// with what it holds.
func (r *runner) request(ctx context.Context, q *model.FoundryQuestion, held heldLoad) (*ontologyagent.AskRequest, error) {
	grants := held.grants
	load, err := r.db.GetFoundryLoad(ctx, q.OntologyID, q.LoadID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && load.Status == model.FoundryLoadDeleted:
		return nil, permanent("the load was deleted", err)
	case err != nil:
		return nil, retry("could not read the load; Helix will retry", err)
	case !load.Status.Servable():
		return nil, permanent("the load is being deleted; ask another load", nil)
	}
	version, err := r.db.GetOntologyVersion(ctx, q.OntologyID, q.VersionID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, permanent("the load's version is gone", err)
	}
	if err != nil {
		return nil, retry("could not read the version; Helix will retry", err)
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(version.Definition, &def); err != nil || version.Format != model.OntologyFormatAgentBundle {
		return nil, permanent("the load's version is not an ontology agent draft", err)
	}
	cfg := r.cfg.Foundry.Ask
	req := &ontologyagent.AskRequest{
		RequestID: q.ID, Load: load.ID, OntologyVersion: load.OntologyVersion(),
		OntologyTOML: def.OntologyTOML, MappingsTOML: def.MappingsTOML, DraftJSON: rawText(def.Draft, "{}"),
		QuestionsJSON: rawText(def.Questions, "[]"), Catalog: []ontologyagent.CatalogEntry{}, Question: q.Question,
		History: []ontologyagent.AskTurn{}, AsOf: q.CreatedAt.UTC().Format(time.RFC3339),
		// The cap is the question's own, reserved and charged by it: config may have moved since it was asked.
		Limits: ontologyagent.AskLimits{
			MaxUSD: float64(q.MaxUSDMicros) / 1e6, MaxSteps: uint32(cfg.MaxSteps), MaxWallSecs: uint64(cfg.MaxWallSecs),
			Read: ontologyagent.AskReadLimits{WallMS: uint64(cfg.ReadWallMS), ScanEntities: uint64(cfg.ReadScanEntities)},
		},
	}
	if len(q.Focus) > 0 {
		// AskFoundryQuestion stored it as the entity ParseFocus read.
		focus, err := ontologyagent.ParseFocus(string(q.Focus))
		if err != nil {
			return nil, permanent("the question's focus is not an entity", err)
		}
		req.Focus = &focus
	}
	var tables []model.OntologyGenerationTable
	var files []model.OntologyGenerationDocument
	if version.GenerationID != nil {
		gen, err := r.db.GetOntologyGeneration(ctx, q.OntologyID, *version.GenerationID)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
			// A version outlives its generation's row only by hand; the agent asks without the goal and the tables' names.
		case err != nil:
			return nil, retry("could not read the version's generation; Helix will retry", err)
		default:
			if gen.Goal != nil && grants == nil {
				req.Goal = *gen.Goal
			}
			tables, files = gen.Input.Tables, gen.Input.Documents
		}
	}
	scope, err := r.db.GetOntologyVersionScope(ctx, q.OntologyID, version.ID)
	if err != nil {
		return nil, retry("could not read the version's document sets; Helix will retry", err)
	}
	if req.Catalog, err = r.catalog(ctx, q.WorkspaceID, tables, files, scope, held); err != nil {
		return nil, err
	}
	if grants != nil {
		req.Visibility = &held.visibility
	}
	history, err := r.db.ListFoundryQuestionHistory(ctx, q, ontologyagent.MaxAskHistory)
	if err != nil {
		return nil, retry("could not read the thread; Helix will retry", err)
	}
	// What the turns it's told of read: its answer can repeat them.
	var told access.Inputs
	if grants != nil && len(history) > 0 {
		// A grantee is told only what it said of loads its grants cover now, or
		// of the part of one they still hold.
		loadIDs := make([]string, len(history))
		for i, h := range history {
			loadIDs[i] = h.LoadID
		}
		slices.Sort(loadIDs)
		loads, err := r.db.ListFoundryLoadInputs(ctx, q.WorkspaceID, slices.Compact(loadIDs))
		if err != nil {
			return nil, retry("could not read the thread; Helix will retry", err)
		}
		history = slices.DeleteFunc(history, func(h *model.FoundryQuestion) bool {
			l, ok := loads[h.LoadID]
			read := l.Inputs
			if h.Seen != nil {
				read = *h.Seen
			}
			if !ok || !grants.Covers(read) {
				return true
			}
			told = told.Union(read)
			return false
		})
	}
	switch {
	case held.part != nil:
		// What it reads of the load is what its answer can say; the answer reads
		// only while its grants hold that, with what an earlier attempt's events
		// show, all of the load if it ran whole.
		seen := held.part.Inputs().Union(told)
		if q.Attempts > 1 {
			earlier := held.inputs
			if q.Seen != nil {
				earlier = *q.Seen
			}
			seen = seen.Union(earlier)
		}
		if err := r.db.SetFoundryQuestionSeen(ctx, q.ID, &seen); err != nil {
			return nil, retry("could not record your access; Helix will retry", err)
		}
	case grants != nil && slices.ContainsFunc(history, func(h *model.FoundryQuestion) bool { return h.LoadID != q.LoadID }):
		// All of this load, and a turn of another that may hold what this one doesn't.
		seen := held.inputs.Union(told)
		if err := r.db.SetFoundryQuestionSeen(ctx, q.ID, &seen); err != nil {
			return nil, retry("could not record your access; Helix will retry", err)
		}
	case q.Seen != nil:
		// It holds all of the load now: the answer reads only while it does.
		if err := r.db.SetFoundryQuestionSeen(ctx, q.ID, nil); err != nil {
			return nil, retry("could not record your access; Helix will retry", err)
		}
	}
	for _, h := range history {
		req.History = append(req.History, ontologyagent.AskTurn{Question: h.Question, Answer: ontologyagent.AnswerText(h.Result)})
	}
	return req, nil
}

// catalog names the generation's tables and table files, and the document
// sets of the load's version's scope, as the agent knows them, by what the
// user calls them: a table file by its document id and "Set / file", a
// document set by its id, as the generator gave them. A connector, file or
// set deleted since is named by its id.
func (r *runner) catalog(ctx context.Context, workspaceID string, tables []model.OntologyGenerationTable, files []model.OntologyGenerationDocument,
	scope []string, held heldLoad) ([]ontologyagent.CatalogEntry, error) {
	grants := held.grants
	connectors, err := r.db.ListConnectors(ctx, workspaceID)
	if err != nil {
		return nil, retry("could not read the workspace's connectors; Helix will retry", err)
	}
	names := make(map[string]string, len(connectors))
	for _, c := range connectors {
		names[c.ID] = c.Name
	}
	out := make([]ontologyagent.CatalogEntry, 0, len(tables)+len(files)+len(scope))
	for _, t := range tables {
		if held.part != nil && !grants.CoversTable(access.TableRef{ConnectorID: t.ConnectorID, Table: t.Table}) {
			continue
		}
		name, ok := names[t.ConnectorID]
		if !ok {
			name = t.ConnectorID
		}
		out = append(out, ontologyagent.CatalogEntry{
			ID: ontologyagent.TableSourceID(t.ConnectorID, t.Table), Kind: ontologyagent.CatalogTable, Name: ontologyagent.TableDisplayName(name, t.Table),
		})
	}
	if len(files)+len(scope) == 0 {
		return out, nil
	}
	sets, err := r.db.ListFoundryDocumentSets(ctx, workspaceID)
	if err != nil {
		return nil, retry("could not read the workspace's document sets; Helix will retry", err)
	}
	setNames := make(map[string]string, len(sets))
	for _, s := range sets {
		ref := access.SetRef{SetID: s.ID}
		if s.Owner != nil {
			ref.ConnectorID = s.Owner.ConnectorID
		}
		// A grantee's catalog names only the sets it holds; of a load it holds
		// all of, its tables and files are all covered.
		if grants == nil || grants.CoversSet(ref) {
			setNames[s.ID] = s.DisplayName()
		}
	}
	if len(files) > 0 {
		ids := make([]string, len(files))
		for i, f := range files {
			ids[i] = f.ID
		}
		docs, err := r.db.ListFoundryDocumentsByID(ctx, workspaceID, ids)
		if err != nil {
			return nil, retry("could not read the version's files; Helix will retry", err)
		}
		for _, f := range files {
			if held.part != nil && !slices.Contains(held.part.Files, f.ID) {
				continue
			}
			name := f.ID
			if d, ok := docs[f.ID]; ok {
				set, ok := setNames[d.SetID]
				if !ok {
					set = d.SetID
				}
				name = ontologyagent.TableDisplayName(set, d.Name)
			}
			out = append(out, ontologyagent.CatalogEntry{ID: f.ID, Kind: ontologyagent.CatalogTable, Name: name})
		}
	}
	for _, id := range scope {
		name, ok := setNames[id]
		if !ok && grants != nil {
			continue
		}
		if !ok {
			name = id
		}
		out = append(out, ontologyagent.CatalogEntry{ID: id, Kind: ontologyagent.CatalogDocumentSet, Name: name})
	}
	return out, nil
}

// ask calls the agent on resolved's key and records what it sends: the
// events, stored every flushEvery and, once the call ends, the rest, before
// the result settles the question; then the result as a's outcome, a failed
// model call worded as resolved's provider's copy (ai.AskFailure).
func (r *runner) ask(ctx context.Context, q *model.FoundryQuestion, request []byte, resolved ai.Resolved, lost func(), a *attempt) error {
	events := &eventBuffer{}
	flushing := make(chan struct{})
	flushCtx, stopFlushing := context.WithCancel(ctx)
	go func() {
		defer close(flushing)
		ticker := time.NewTicker(r.flushEvery)
		defer ticker.Stop()
		for {
			select {
			case <-flushCtx.Done():
				return
			case <-ticker.C:
				if err := r.flush(flushCtx, q, events); errors.Is(err, foundrystore.ErrNotFound) {
					lost()
					return
				}
			}
		}
	}()
	raw, callErr := r.agent.Ask(ctx, request, resolved.Key, func(e json.RawMessage) error {
		event, err := ontologyagent.DecodeAskEvent(e)
		if err != nil {
			return err
		}
		if started, ok := event.(ontologyagent.AskStartedEvent); ok {
			log.From(ctx).Info("foundry-ask-worker: asking", "question", q.ID, "attempt", q.Attempts,
				"model", started.Model.Name, "provider", started.Model.Provider)
		}
		events.add(e)
		return nil
	})
	stopFlushing()
	<-flushing
	// Until a result says what it spent, an attempt that started is charged its cap.
	if events.started() {
		a.outcome.Spend.USDMicros = q.MaxUSDMicros
	}
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := r.flush(flushCtx, q, events); errors.Is(err, foundrystore.ErrNotFound) {
		lost()
		return retry("the question's lease was lost", err)
	} else if err != nil {
		return retry("the question's steps could not be stored; Helix will retry", err)
	}
	if callErr != nil {
		return askFailure(ctx, callErr)
	}
	result, err := ontologyagent.DecodeAskResult(raw)
	if err != nil {
		return retry("the agent's answer could not be read; Helix will retry", err)
	}
	u := result.Usage
	a.outcome = model.FoundryQuestionOutcome{
		Result: raw,
		Spend: model.FoundryGraphJobSpend{
			USDMicros: int64(math.Round(u.USD * 1e6)), InputTokens: int64(u.Input + u.CacheRead + u.CacheWrite), OutputTokens: int64(u.Output),
		},
	}
	cfg := r.cfg.Foundry.Ask
	switch result.Status {
	case ontologyagent.AskAnswered:
		a.outcome.Status = model.FoundryQuestionAnswered
	case ontologyagent.AskCannotAnswer:
		a.outcome.Status = model.FoundryQuestionCannotAnswer
	case ontologyagent.AskBudgetExhausted:
		a.outcome.Status, a.outcome.Error = model.FoundryQuestionFailed, fmt.Sprintf(
			"the question reached its limit of $%.2f, %d steps or %d seconds before an answer; ask something narrower",
			float64(q.MaxUSDMicros)/1e6, cfg.MaxSteps, cfg.MaxWallSecs)
	default:
		a.outcome.Status, a.outcome.Error = model.FoundryQuestionFailed, failureMessage(ctx, resolved, result.Failure)
	}
	return nil
}

// failureMessage words, for the user, why a question failed: a model call's
// failure as its provider's copy says, the rest as failureMessages do.
func failureMessage(ctx context.Context, resolved ai.Resolved, f ontologyagent.AskFailure) string {
	if m, ok := f.ModelFailure(); ok {
		return ai.AskFailure(resolved, m).Logged(ctx, fmt.Errorf("the question's model failed: %s", m)).Message
	}
	return failureMessages[f]
}

// failureMessages word, for the user, each reason a question fails for that
// is not a model call's.
var failureMessages = map[ontologyagent.AskFailure]string{
	ontologyagent.AskNoAnswer:     "the agent stopped without an answer; ask again",
	ontologyagent.AskLoadUnknown:  "the load is no longer in the database; ask another load",
	ontologyagent.AskLoadMismatch: "the database holds this load from another version; ask another load",
	ontologyagent.AskStoreFailed:  "the graph could not be read; ask again",
}

// askFailure words a failed Ask call. A request the agent can't take, or an
// agent that can't answer questions, won't change on a retry; an agent too
// busy to start did nothing; an attempt past its time stopped for good.
func askFailure(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return permanent("the question ran past its time limit", err)
	}
	switch status.Code(err) {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.Unimplemented:
		return permanent("the ontology agent can't answer this question: "+status.Convert(err).Message(), err)
	case codes.ResourceExhausted:
		return &failure{Message: "the ontology agent is busy; Helix will retry", Uncounted: true, Err: err}
	}
	return retry("the question stopped before an answer; Helix will retry", err)
}

// eventBuffer holds the events not yet stored.
type eventBuffer struct {
	mu      sync.Mutex
	pending []json.RawMessage
	any     bool
}

func (b *eventBuffer) add(e json.RawMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending, b.any = append(b.pending, e), true
}

// started reports whether the agent sent any event.
func (b *eventBuffer) started() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.any
}

// take removes the pending events; giveBack returns ones that failed to
// store, ahead of any added since.
func (b *eventBuffer) take() []json.RawMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.pending
	b.pending = nil
	return out
}

func (b *eventBuffer) giveBack(events []json.RawMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(events, b.pending...)
}

// flush stores the pending events in order. ErrNotFound means the question's
// lease was lost; on any other failure they are kept for the next flush.
func (r *runner) flush(ctx context.Context, q *model.FoundryQuestion, events *eventBuffer) error {
	pending := events.take()
	if len(pending) == 0 {
		return nil
	}
	err := r.db.AppendFoundryQuestionEvents(ctx, q.ID, r.workerID, pending)
	if err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
		events.giveBack(pending)
		log.From(ctx).Warn("storing question events failed; they are kept for the next flush", "question", q.ID, "err", err)
	}
	return err
}

// rawText is raw as JSON text, or empty when raw is.
func rawText(raw json.RawMessage, empty string) string {
	if len(raw) == 0 {
		return empty
	}
	return string(raw)
}
