package ai

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologygen"
)

// agent.go writes a resolved provider as the ontology agent's contract
// names it: the endpoint every call of a request goes to, and a ModelSpec
// per role. The key goes beside, in the call's metadata.

// ProbeMaxUSD is the most one probe spends, on the key it tests.
const ProbeMaxUSD = 0.5

// NewResolver is the resolver of a process that makes model calls: cfg's
// deployment; the connectors' keyring, which seals workspaces' keys (nil
// where connectors aren't configured: then only a workspace's own provider
// fails to resolve); and plans, which says which hosted workspaces may run
// on their own provider (the host's; nil lets none).
func NewResolver(cfg *config.Config, db ActiveConfigs, plans PlanReader) (*Resolver, error) {
	d, err := NewDeployment(cfg)
	if err != nil {
		return nil, err
	}
	keys, err := connector.NewKeyring(cfg.Foundry.Connectors)
	if err != nil {
		return nil, err
	}
	return &Resolver{DB: db, Keyring: keys, Deployment: d, Plans: NewPlans(plans)}, nil
}

// Turn is a turn's models: the draft's, and its previews' extraction and
// verification models.
func (r Resolved) Turn() ontologyagent.TurnModels {
	m := r.Settings.Models
	return ontologyagent.TurnModels{
		Endpoint: r.endpoint(), Draft: r.spec(RoleDraft, m.Draft), Extract: r.spec(RoleExtract, m.Extract), Verify: r.spec(RoleVerify, m.Verify),
	}
}

// Ask is a question's model.
func (r Resolved) Ask() ontologyagent.AskModels {
	return ontologyagent.AskModels{Endpoint: r.endpoint(), Ask: r.spec(RoleAsk, r.Settings.Models.Ask)}
}

// Extract is a document extraction's models.
func (r Resolved) Extract() ontologyagent.ExtractModels {
	m := r.Settings.Models
	return ontologyagent.ExtractModels{Endpoint: r.endpoint(), Extract: r.spec(RoleExtract, m.Extract), Verify: r.spec(RoleVerify, m.Verify)}
}

// Title is a title's model: the extraction model, its reasoning off, as
// Helix's Claude has always titled with Haiku and thinking disabled.
func (r Resolved) Title() ontologyagent.TitleModels {
	spec := r.spec(RoleExtract, r.Settings.Models.Extract)
	spec.Reasoning = false
	return ontologyagent.TitleModels{Endpoint: r.endpoint(), Title: spec}
}

func (r Resolved) endpoint() ontologyagent.Endpoint { return r.Settings.endpoint(r.CallTimeout) }

// spec is role's model m as r runs it: its reasoning knob as the probe
// found it took, and for Ollama what its probe read of the model. The
// probe reports one model's digest and context window, so they pin a
// setting that runs one model for every role: its runs send the digest as
// their revision (a model pulled again since fails model_changed) and
// load it in no more than the model's own window. A setting of several
// models, and a local install's default, which no probe tests, run on
// whatever the server has, in the window the owner chose.
func (r Resolved) spec(role Role, m Model) ontologyagent.ModelSpec {
	spec := r.Settings.spec(m, r.Report.Reasoning(role), "")
	tested := r.Report.Ollama
	if _, ok := r.Settings.Provider.(Ollama); !ok || tested == nil || !r.Settings.Models.oneModel() {
		return spec
	}
	spec.Revision = tested.Digest
	if tested.ContextLength > 0 {
		spec.ContextTokens = min(spec.ContextTokens, tested.ContextLength)
	}
	return spec
}

// oneModel reports whether every role runs the same model.
func (r Roles) oneModel() bool {
	return r.Ask.Name == r.Draft.Name && r.Extract.Name == r.Draft.Name && r.Verify.Name == r.Draft.Name
}

// endpoint is where s's calls go, each JSON or text call's attempt bounded
// by callTimeout (at least a second). A local server takes calls a few at
// a time: Ollama one, an OpenAI-compatible server as many as its owner set.
func (s Settings) endpoint(callTimeout time.Duration) ontologyagent.Endpoint {
	e := ontologyagent.Endpoint{CallTimeoutSecs: uint32(max(1, math.Ceil(callTimeout.Seconds())))}
	switch p := s.Provider.(type) {
	case Anthropic:
		e.Provider = ontologyagent.Anthropic{}
	case OpenAI:
		e.Provider = ontologyagent.OpenAI{}
	case AzureOpenAI:
		e.Provider = ontologyagent.AzureOpenAI{Endpoint: p.Endpoint}
	case Bedrock:
		e.Provider = ontologyagent.Bedrock{Region: p.Region, FIPS: p.FIPS}
	case Ollama:
		e.Provider, e.MaxCallsInFlight = ontologyagent.Ollama{BaseURL: p.BaseURL}, 1
	case Compatible:
		e.Provider = ontologyagent.OpenAICompatible{BaseURL: p.BaseURL, API: ontologyagent.CompatAPI(p.API)}
		e.MaxCallsInFlight = p.MaxCallsInFlight
	}
	return e
}

// spec is m as the contract writes it: Ollama's models with the server's
// context window and, once tested, the digest revision.
func (s Settings) spec(m Model, reasoning bool, revision string) ontologyagent.ModelSpec {
	spec := ontologyagent.ModelSpec{Name: m.Name, Reasoning: reasoning}
	switch p := m.Price.(type) {
	case PriceTable:
		spec.Price = ontologyagent.PriceTable{}
	case PriceTableAs:
		spec.Price = ontologyagent.PriceTableAs{Model: p.Model}
	case PriceRates:
		spec.Price = ontologyagent.PriceRates{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead}
	case PriceFree:
		spec.Price = ontologyagent.PriceFree{}
	}
	if o, ok := s.Provider.(Ollama); ok {
		spec.ContextTokens, spec.Revision = o.ContextTokens, revision
	}
	return spec
}

// ProbeRequest is the agent's test of s, saved as config configID: every
// role's model as it would run, reasoning on (the probe turns it off where a
// model refuses it), and a title on the extraction model, under ProbeMaxUSD
// and the deployment's probe timeout.
func (d *Deployment) ProbeRequest(configID string, s Settings, timeout time.Duration) ontologyagent.ProbeRequest {
	m := s.Models
	title := s.spec(m.Extract, false, "")
	return ontologyagent.ProbeRequest{
		RequestID: configID, Endpoint: s.endpoint(d.CallTimeout),
		Checks: []ontologyagent.ProbeCheck{
			{Role: ontologyagent.ProbeDraft, Model: s.spec(m.Draft, true, "")},
			{Role: ontologyagent.ProbeAsk, Model: s.spec(m.Ask, true, "")},
			{Role: ontologyagent.ProbeExtract, Model: s.spec(m.Extract, true, "")},
			{Role: ontologyagent.ProbeVerify, Model: s.spec(m.Verify, true, "")},
			{Role: ontologyagent.ProbeTitle, Model: title},
		},
		Limits: ontologyagent.ProbeLimits{MaxUSD: ProbeMaxUSD, MaxWallSecs: uint32(max(1, math.Ceil(timeout.Seconds())))},
	}
}

// ollamaDigest is an Ollama model's digest as the agent takes it for a
// revision.
var ollamaDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Usable refuses what a passed probe found when its runs couldn't send it:
// an Ollama digest the agent wouldn't take as a revision, which would fail
// every run.
func (p ProbeReport) Usable() error {
	if o := p.Ollama; o != nil && !ollamaDigest.MatchString(o.Digest) {
		return errors.New("the probe's Ollama digest is not sha256: and 64 hex digits")
	}
	return nil
}

// ReportOf is what a probe found, as probe_report keeps it: per check its
// role, outcome, reasoning and time, and Ollama's digest and context
// window; never a model's text.
func ReportOf(r ontologyagent.ProbeResult) ProbeReport {
	out := ProbeReport{Checks: make([]ProbeCheck, 0, len(r.Checks))}
	for _, c := range r.Checks {
		out.Checks = append(out.Checks, ProbeCheck{Role: string(c.Role), Passed: c.Passed, Reasoning: c.Reasoning, Seconds: c.Seconds})
	}
	if o := r.Ollama; o != nil {
		out.Ollama = &OllamaReport{Digest: o.Digest, ContextLength: uint32(min(o.ContextLength, math.MaxUint32))}
	}
	return out
}

// Draft is what ws's next draft runs on, for the agent's generator, whose
// sidecar calls providers: its model failures worded by DraftFailure. A
// setting that can't run is an *ontologygen.Failure worded by
// ResolveFailure.
func (r *Resolver) Draft(ctx context.Context, workspaceID string, providers []string) (ontologyagent.Draft, error) {
	resolved, err := r.ResolveServed(ctx, workspaceID, providers)
	if err != nil {
		return ontologyagent.Draft{}, Failure(ctx, ResolveFailure(err), err)
	}
	return ontologyagent.Draft{
		Models: resolved.Turn(), Key: resolved.Key,
		Failure: func(ctx context.Context, f ontologyagent.ModelFailure) error {
			return Failure(ctx, DraftFailure(resolved, f), fmt.Errorf("the draft's model failed: %s", f))
		},
	}, nil
}

// Failure is c as an attempt's *ontologygen.Failure, err its cause, logged
// first when it is Helix's to fix (Copy.Logged).
func Failure(ctx context.Context, c Copy, err error) *ontologygen.Failure {
	c = c.Logged(ctx, err)
	return &ontologygen.Failure{Message: c.Message, Permanent: c.Permanent, Err: err}
}
