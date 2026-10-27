package foundryontologyworker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/boost"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologygen"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

type reviseAgent interface {
	Info(context.Context) (*agentv1.GetInfoResponse, error)
	Revise(context.Context, []byte) (json.RawMessage, error)
}

type revisionStore interface {
	WorkspaceDeleting(context.Context, string) (bool, error)
	GetFoundryOntology(context.Context, string, string) (*model.FoundryOntology, error)
	GetOntologyVersion(context.Context, string, string) (*model.OntologyVersion, error)
	GetFoundryLoad(context.Context, string, string) (*model.FoundryLoad, error)
}

// reviser runs only inside a leased ontology job. openEvaluation binds a
// read-only proxy and holds a boost until the returned cleanup runs; its
// errors are graphproxy.Bind's gRPC statuses (NotFound: the database is gone;
// FailedPrecondition: its queries are paused or its cluster is changing;
// Unavailable) or the boost's and proxy's own.
type reviser struct {
	db             revisionStore
	agent          reviseAgent
	openEvaluation func(context.Context, *model.OntologyGeneration, *model.FoundryLoad) (ontologyagent.ReviseEval, func(), error)
}

type revisionResult struct {
	definition json.RawMessage
	report     json.RawMessage
}

func (r *reviser) run(ctx context.Context, gen *model.OntologyGeneration) (revisionResult, error) {
	var out revisionResult
	permanent := func(message string, err error) error {
		return &ontologygen.Failure{Message: message, Permanent: true, Err: err}
	}
	infoCtx, cancelInfo := context.WithTimeout(ctx, 30*time.Second)
	info, err := r.agent.Info(infoCtx)
	cancelInfo()
	if err != nil {
		return out, err
	}
	if info.GetContractVersion() != ontologyagent.ContractVersion {
		return out, permanent("the ontology agent contract does not match", nil)
	}
	deleting, err := r.db.WorkspaceDeleting(ctx, gen.WorkspaceID)
	if errors.Is(err, store.ErrNotFound) || err == nil && deleting {
		return out, permanent("the workspace was deleted", nil)
	}
	if err != nil {
		return out, err
	}
	o, err := r.db.GetFoundryOntology(ctx, gen.WorkspaceID, gen.OntologyID)
	if errors.Is(err, store.ErrNotFound) || err == nil && o.DeletedAt != nil {
		return out, permanent("the ontology was deleted", nil)
	}
	if err != nil {
		return out, err
	}
	rev := gen.Input.Revise
	if rev == nil {
		return out, permanent("revision input is missing", nil)
	}
	parent, err := r.db.GetOntologyVersion(ctx, gen.OntologyID, rev.ParentVersionID)
	if errors.Is(err, store.ErrNotFound) {
		return out, permanent("the parent version is unavailable", err)
	}
	if err != nil {
		return out, err
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(parent.Definition, &def); err != nil || parent.Format != model.OntologyFormatAgentBundle || def.Status != ontologyagent.StatusDone || !json.Valid(def.Draft) || !json.Valid(def.Questions) {
		return out, permanent("the parent is not a finished agent draft", err)
	}
	request := ontologyagent.ReviseRequest{Definition: ontologyagent.ReviseDefinition{OntologyTOML: def.OntologyTOML, MappingsTOML: def.MappingsTOML, DraftJSON: string(def.Draft), QuestionsJSON: string(def.Questions)}, Edits: rev.Edits}
	if o.CurrentLoadID != nil {
		load, err := r.db.GetFoundryLoad(ctx, o.ID, *o.CurrentLoadID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return out, err
		}
		if err == nil && load.Status.Servable() {
			// The engine measures on the load only if the revision keeps the
			// types, links and props of the load's version, and reads what
			// People enrichment filled on it from that version's mappings.
			loaded, err := r.db.GetOntologyVersion(ctx, gen.OntologyID, load.VersionID)
			if err != nil {
				return out, err
			}
			var loadedDef ontologyagent.Definition
			if err := json.Unmarshal(loaded.Definition, &loadedDef); err != nil || loaded.Format != model.OntologyFormatAgentBundle {
				return out, permanent("the current load's version is not an ontology agent draft", err)
			}
			evaluation, close, err := r.openEvaluation(ctx, gen, load)
			switch code := status.Code(err); {
			case err == nil:
				defer close()
				evaluation.OntologyTOML, evaluation.MappingsTOML = loadedDef.OntologyTOML, loadedDef.MappingsTOML
				request.Eval = &evaluation
			case code == codes.NotFound:
				// The load's database is gone for good: nothing can be
				// measured, which the engine treats as no comparable load.
			case code == codes.FailedPrecondition:
				// Queries paused or the cluster mid-change, which may lift
				// before the attempts run out; the reason is the customer's.
				return out, &ontologygen.Failure{Message: status.Convert(err).Message(), Err: err}
			default:
				return out, err
			}
		}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return out, permanent("invalid revision input", err)
	}
	raw, err := r.agent.Revise(ctx, body)
	switch status.Code(err) {
	case codes.OK:
	case codes.InvalidArgument, codes.Unimplemented:
		return out, permanent("the ontology agent refused the revision call", err)
	case codes.Unavailable, codes.ResourceExhausted:
		// The agent shutting down or at its two revisions, or reading the load
		// failed in a way a retry may clear: no result, so the attempts retry it.
		return out, &ontologygen.Failure{Message: "the ontology agent could not finish the revision; Helix will retry", Err: err}
	default:
		return out, err
	}
	if len(raw) > 3<<20 {
		return out, permanent("the revision result exceeds 3 MiB", nil)
	}
	result, err := ontologyagent.DecodeReviseResult(raw)
	if err != nil {
		return out, permanent("the ontology agent returned an invalid revision", err)
	}
	// Definition is stored only with a new version, never with a preview.
	revised := result.Definition
	result.Definition = nil
	out.report, err = json.Marshal(result)
	if err != nil {
		return out, permanent("could not encode revision findings", err)
	}
	if result.Status == ontologyagent.ReviseRefused {
		return out, permanent("the ontology agent refused these edits; see findings", nil)
	}
	if rev.DryRun {
		return out, nil
	}
	def.Rules = result.Rules
	def.ParentVersionID = parent.ID
	def.OntologyTOML, def.MappingsTOML = revised.OntologyTOML, revised.MappingsTOML
	def.Draft, def.Questions = json.RawMessage(revised.DraftJSON), json.RawMessage(revised.QuestionsJSON)
	def.Summary, def.Usage = nil, ontologyagent.Usage{}
	out.definition, err = json.Marshal(def)
	if len(out.definition) > 3<<20 {
		return revisionResult{}, permanent("the revised definition exceeds 3 MiB", nil)
	}
	return out, err
}

// newReviser measures revisions on the current load through the pod's
// read-only proxy (graphproxy.ReadProxy): each evaluation binds the load's
// database and registers that binding for the call, so the agent reads it and
// can never write it. With foundry.graph.load_rate_limit set, the attempt
// holds the database's boost as a revision holder (boost.Booster), its own
// lease beside any load's or question's; the gateway's sync is not waited
// for, the proxy sends a turned-away read again. The cleanup ends the
// registration, then releases the boost.
func newReviser(db store.Store, dbs ports.Databases, boosts ports.Boosts, cfg *config.Config, workerID string, agent reviseAgent, proxy *graphproxy.ReadProxy) *reviser {
	return &reviser{db: db, agent: agent, openEvaluation: func(ctx context.Context, gen *model.OntologyGeneration, load *model.FoundryLoad) (ontologyagent.ReviseEval, func(), error) {
		var evaluation ontologyagent.ReviseEval
		bind := func(ctx context.Context) (graphproxy.Binding, error) {
			return graphproxy.BindRead(ctx, db, dbs, gen.WorkspaceID, gen.OntologyID)
		}
		binding, err := bind(ctx)
		if err != nil {
			return evaluation, nil, err
		}
		release := func() {}
		if limit := cfg.Foundry.Graph.LoadRateLimit; limit.Enabled() {
			booster := boost.Booster{Boosts: boosts, WorkerID: workerID, Limit: limit}
			release, err = booster.Acquire(ctx, boost.Attempt{
				Holder: model.FoundryBoostRevision, ID: gen.ID, Number: gen.Attempts, TenantID: binding.TenantID, Timeout: cfg.Foundry.Ontology.Timeout,
			})
			if err != nil {
				return evaluation, nil, err
			}
		}
		// The evaluation's token, and its reads in flight, end with callCtx; a
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
			release()
			return evaluation, nil, err
		}
		evaluation = ontologyagent.ReviseEval{Database: binding.TenantID, Load: load.ID, OntologyVersion: load.OntologyVersion(), Helix: ontologyagent.HelixTarget{
			URL: proxy.URL(), Token: token, MaxRequestBytes: graphproxy.MaxRequestBytes, TimeoutSecs: uint64(cfg.Foundry.Graph.HelixTimeoutSecs),
		}}
		return evaluation, func() { endCall(); release() }, nil
	}}
}
