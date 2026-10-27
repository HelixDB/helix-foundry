package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	"github.com/helixdb/helix-foundry/backend/ontologygen"
)

// everyKind is a saved setting of each kind, priced each way a model can be.
func everyKind() map[Kind]Settings {
	cached := uint64(250)
	return map[Kind]Settings{
		KindAnthropic: claude,
		KindOpenAI:    {Provider: OpenAI{}, Models: every(Model{Name: "gpt-6.1", Price: PriceRates{Input: 1250, Output: 10000, CacheRead: &cached}})},
		KindAzureOpenAI: {Provider: AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai/v1"},
			Models: every(Model{Name: "drafts", Price: PriceTableAs{Model: "gpt-6.1"}})},
		KindBedrock: {Provider: Bedrock{Region: "eu-west-1", FIPS: true},
			Models: every(Model{Name: "eu.anthropic.claude-sonnet-5-5", Price: PriceTableAs{Model: "anthropic.claude-sonnet-5-5"}})},
		KindOllama: {Provider: Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: 65536}, Models: every(Model{Name: "qwen3:32b", Price: PriceFree{}})},
		KindCompatible: {Provider: Compatible{BaseURL: "https://llm.example.com/v1", API: CompatResponses, MaxCallsInFlight: 6},
			Models: every(Model{Name: "llama-4", Price: PriceFree{}})},
	}
}

func TestAgentModels(t *testing.T) {
	ctx := context.Background()
	hosted, err := NewDeployment(hostedConfig(sentinel))
	if err != nil {
		t.Fatal(err)
	}

	// The default resolves to Helix's Claude exactly as the agent's own
	// hosted default names it: so hosted requests run as they always did.
	t.Run("runs the default as Helix's Claude", func(t *testing.T) {
		r, err := (&Resolver{DB: activeConfigs{}, Deployment: hosted}).Resolve(ctx, "ws-1")
		if err != nil {
			t.Fatal(err)
		}
		endpoint := ontologyagent.Endpoint{Provider: ontologyagent.Anthropic{}, CallTimeoutSecs: 600}
		spec := func(name string, reasoning bool) ontologyagent.ModelSpec {
			return ontologyagent.ModelSpec{Name: name, Price: ontologyagent.PriceTable{}, Reasoning: reasoning}
		}
		if got, want := r.Turn(), (ontologyagent.TurnModels{Endpoint: endpoint, Draft: spec("claude-opus-5-5", true),
			Extract: spec("claude-haiku-5-5", true), Verify: spec("claude-sonnet-5-5", true)}); got != want {
			t.Errorf("Turn() = %+v", got)
		}
		if got := r.Ask(); got != (ontologyagent.AskModels{Endpoint: endpoint, Ask: spec("claude-sonnet-5-5", true)}) {
			t.Errorf("Ask() = %+v", got)
		}
		if got := r.Extract(); got != (ontologyagent.ExtractModels{Endpoint: endpoint, Extract: spec("claude-haiku-5-5", true),
			Verify: spec("claude-sonnet-5-5", true)}) {
			t.Errorf("Extract() = %+v", got)
		}
		// Titles stay on Haiku with thinking off.
		if got := r.Title(); got != (ontologyagent.TitleModels{Endpoint: endpoint, Title: spec("claude-haiku-5-5", false)}) {
			t.Errorf("Title() = %+v", got)
		}
	})

	// Each kind's endpoint and models are what the contract names: a local
	// server's bound and context window, an Ollama model's tested digest,
	// and the reasoning knob as the probe found the model took it.
	t.Run("writes each kind's endpoint and models", func(t *testing.T) {
		kinds := everyKind()
		cached := uint64(250)
		for kind, want := range map[Kind]struct {
			endpoint ontologyagent.Endpoint
			spec     ontologyagent.ModelSpec
		}{
			KindOpenAI: {ontologyagent.Endpoint{Provider: ontologyagent.OpenAI{}, CallTimeoutSecs: 600},
				ontologyagent.ModelSpec{Name: "gpt-6.1", Price: ontologyagent.PriceRates{Input: 1250, Output: 10000, CacheRead: &cached}}},
			KindAzureOpenAI: {ontologyagent.Endpoint{Provider: ontologyagent.AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai/v1"}, CallTimeoutSecs: 600},
				ontologyagent.ModelSpec{Name: "drafts", Price: ontologyagent.PriceTableAs{Model: "gpt-6.1"}}},
			KindBedrock: {ontologyagent.Endpoint{Provider: ontologyagent.Bedrock{Region: "eu-west-1", FIPS: true}, CallTimeoutSecs: 600},
				ontologyagent.ModelSpec{Name: "eu.anthropic.claude-sonnet-5-5", Price: ontologyagent.PriceTableAs{Model: "anthropic.claude-sonnet-5-5"}}},
			KindOllama: {ontologyagent.Endpoint{Provider: ontologyagent.Ollama{BaseURL: "http://127.0.0.1:11434"}, CallTimeoutSecs: 600, MaxCallsInFlight: 1},
				ontologyagent.ModelSpec{Name: "qwen3:32b", Price: ontologyagent.PriceFree{}, ContextTokens: 65536, Revision: "sha256:" + strings.Repeat("cd", 32)}},
			KindCompatible: {ontologyagent.Endpoint{
				Provider: ontologyagent.OpenAICompatible{BaseURL: "https://llm.example.com/v1", API: ontologyagent.CompatResponses}, CallTimeoutSecs: 600,
				MaxCallsInFlight: 6,
			}, ontologyagent.ModelSpec{Name: "llama-4", Price: ontologyagent.PriceFree{}}},
		} {
			r := Resolved{Source: SourceWorkspace, Settings: kinds[kind], CallTimeout: 10 * time.Minute, Report: ProbeReport{
				Checks: []ProbeCheck{{Role: "draft", Passed: true, Reasoning: false}, {Role: "ask", Passed: true, Reasoning: true}},
				Ollama: &OllamaReport{Digest: "sha256:" + strings.Repeat("cd", 32), ContextLength: 65536},
			}}
			if kind != KindOllama {
				r.Report.Ollama = nil
			}
			turn, ask := r.Turn(), r.Ask()
			draft, extract := want.spec, want.spec
			draft.Reasoning, extract.Reasoning = false, true
			if turn.Endpoint != want.endpoint || !specEqual(turn.Draft, draft) || !specEqual(turn.Extract, extract) {
				t.Errorf("%s: Turn() = %+v", kind, turn)
			}
			if !specEqual(ask.Ask, extract) {
				t.Errorf("%s: Ask() = %+v", kind, ask)
			}
		}
	})

	// Every request a worker builds on any kind, and every probe, is one the
	// agent's schemas take.
	t.Run("builds requests the agent accepts, for every kind", func(t *testing.T) {
		for kind, s := range everyKind() {
			r := Resolved{Source: SourceWorkspace, Settings: s, CallTimeout: 30 * time.Minute}
			for schema, request := range map[string]any{
				"turn_request.json": ontologyagent.TurnRequest{SessionID: "s", RequestID: "r", Sources: []ontologyagent.Source{},
					Input:  ontologyagent.Input{Start: ontologyagent.Start{Workflow: "w", Goals: []string{}}},
					Limits: ontologyagent.Limits{MaxUSD: 1, MaxSteps: 1, MaxWallSecs: 1}, Models: r.Turn()},
				"title_request.json": ontologyagent.TitleRequest{RequestID: "gen-1", Goal: "track churn", Models: r.Title()},
				"probe_request.json": hosted.ProbeRequest("cfg-1", s, 2*time.Minute),
			} {
				raw, err := json.Marshal(request)
				if err != nil {
					t.Fatalf("%s %s: %v", kind, schema, err)
				}
				if err := contracttest.Validate(t, contracttest.Schema(t, schema), raw); err != nil {
					t.Errorf("%s: %s refused %s: %v", kind, schema, raw, err)
				}
			}
			// The ask and extract blocks are the same definitions the schemas share.
			for name, block := range map[string]any{"ask": r.Ask(), "extract": r.Extract()} {
				if _, err := json.Marshal(block); err != nil {
					t.Errorf("%s %s: %v", kind, name, err)
				}
			}
		}
		if len(everyKind()) != len(Kinds) {
			t.Fatalf("%d kinds of %d", len(everyKind()), len(Kinds))
		}
	})

	// A probe checks every role as it would run, reasoning on, and a title
	// on the extraction model without it, within the spend and time it may
	// take; it names no revision, which it reads.
	t.Run("probes every role", func(t *testing.T) {
		probe := hosted.ProbeRequest("cfg-1", everyKind()[KindOllama], 90*time.Second+time.Millisecond)
		var roles []ontologyagent.ProbeRole
		for _, c := range probe.Checks {
			roles = append(roles, c.Role)
			if c.Model.Revision != "" || c.Model.ContextTokens != 65536 || c.Model.Reasoning != (c.Role != ontologyagent.ProbeTitle) {
				t.Errorf("check %s = %+v", c.Role, c.Model)
			}
		}
		if probe.RequestID != "cfg-1" || len(roles) != len(ontologyagent.ProbeRoles) || probe.Limits != (ontologyagent.ProbeLimits{MaxUSD: 0.5, MaxWallSecs: 91}) ||
			probe.Endpoint.MaxCallsInFlight != 1 {
			t.Fatalf("probe = %+v", probe)
		}
		for i, role := range ontologyagent.ProbeRoles {
			if roles[i] != role {
				t.Fatalf("roles = %v", roles)
			}
		}
	})

	// What a probe found is kept per check, with Ollama's digest, and is
	// what the setting then runs with.
	t.Run("keeps what the probe found", func(t *testing.T) {
		report := ReportOf(ontologyagent.ProbeResult{
			Checks: []ontologyagent.ProbeCheckResult{
				{Role: ontologyagent.ProbeDraft, Passed: true, Seconds: 3, Reasoning: true},
				{Role: ontologyagent.ProbeExtract, Passed: true, Seconds: 1, Reasoning: false},
			},
			Ollama: &ontologyagent.OllamaInfo{Digest: "sha256:ab", ContextLength: 1 << 40, Capabilities: []string{"tools"}},
		})
		raw, _ := json.Marshal(report)
		const want = `{"checks":[{"role":"draft","passed":true,"reasoning":true,"seconds":3},{"role":"extract","passed":true,"reasoning":false,"seconds":1}],` +
			`"ollama":{"digest":"sha256:ab","context_length":4294967295}}`
		if string(raw) != want {
			t.Fatalf("report = %s", raw)
		}
		if !report.Reasoning(RoleDraft) || report.Reasoning(RoleExtract) || !report.Reasoning(RoleVerify) {
			t.Fatalf("reasoning = %+v", report)
		}
	})

	// The generator's adapter: a setting that can't run, or runs on a kind
	// the agent doesn't call, fails the attempt as worded; a model failure,
	// as the setting's draft copy says.
	t.Run("resolves what a draft runs on", func(t *testing.T) {
		r := &Resolver{DB: activeConfigs{}, Deployment: hosted}
		resolved, err := r.Resolve(ctx, "ws-1")
		if err != nil {
			t.Fatal(err)
		}
		draft, err := r.Draft(ctx, "ws-1", []string{"anthropic"})
		if err != nil || draft.Key.Reveal() != sentinel || draft.Models != resolved.Turn() {
			t.Fatalf("Draft = %+v, %v", draft, err)
		}
		var f *ontologygen.Failure
		if err := draft.Failure(ctx, ontologyagent.ModelRefused); !errors.As(err, &f) || f.Message != houseDeclined || !f.Permanent {
			t.Fatalf("refused = %v", err)
		}
		if err := draft.Failure(ctx, ontologyagent.ModelAuth); !errors.As(err, &f) || f.Message != houseUnavailable || f.Permanent {
			t.Fatalf("auth = %v", err)
		}
		_, err = r.Draft(ctx, "ws-1", []string{"openai"})
		if !errors.As(err, &f) || !errors.Is(err, ErrNotServed) || f.Permanent || !strings.Contains(f.Message, "can't call this AI provider") {
			t.Fatalf("a kind not served = %v", err)
		}
		local := *hosted
		local.Default = nil
		_, err = (&Resolver{DB: activeConfigs{}, Deployment: &local}).Draft(ctx, "ws-1", []string{"anthropic"})
		if !errors.As(err, &f) || !f.Permanent || !strings.HasPrefix(f.Message, "No AI provider is set up") {
			t.Fatalf("none = %v", err)
		}
	})
}

// specEqual compares specs, a rate's cache pointer by value.
func specEqual(a, b ontologyagent.ModelSpec) bool {
	ra, aRates := a.Price.(ontologyagent.PriceRates)
	rb, bRates := b.Price.(ontologyagent.PriceRates)
	if aRates && bRates {
		same := ra.Input == rb.Input && ra.Output == rb.Output && (ra.CacheRead == nil) == (rb.CacheRead == nil) &&
			(ra.CacheRead == nil || *ra.CacheRead == *rb.CacheRead)
		a.Price, b.Price = nil, nil
		return same && a == b
	}
	return a == b
}
