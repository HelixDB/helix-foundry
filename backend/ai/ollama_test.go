package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
)

// digest is an Ollama model's digest, as its probe reads it.
var digest = "sha256:" + strings.Repeat("cd", 32)

// ollama is an Ollama server on this machine running m for every role.
func ollama(m Model) Settings {
	return Settings{Provider: Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: DefaultContextTokens}, Models: every(m)}
}

// refused is the field err refuses; "" for none.
func refused(err error) string {
	var bad *Invalid
	if errors.As(err, &bad) {
		return bad.Field
	}
	return ""
}

// localDeployment is a local install with no default, offering Ollama first.
func localDeployment(t *testing.T) *Deployment {
	t.Helper()
	cfg := config.Defaults()
	cfg.Host.Local = true
	cfg.Foundry.AI.Kinds = []string{"ollama", "anthropic"}
	cfg.Foundry.AI.AllowPrivateHosts = true
	cfg.Foundry.AI.CallTimeout = 30 * time.Minute
	d, err := NewDeployment(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOllamaSettings(t *testing.T) {
	local, hosted := Rules{AllowPrivate: true}, Rules{}
	free := Model{Name: "qwen3:8b", Price: PriceFree{}}
	with := func(base string, window uint32, m Model) Settings {
		return Settings{Provider: Ollama{BaseURL: base, ContextTokens: window}, Models: every(m)}
	}

	// A local install reaches the bundled server, this computer's, or one on
	// its network, at its address; never at the OpenAI-compatible /v1 or at
	// the API's own paths, which every call would 404 on.
	t.Run("checks the server's address", func(t *testing.T) {
		for base, want := range map[string]string{
			"http://127.0.0.1:11434":              "",
			"http://host.docker.internal:11434/":  "",
			"http://10.0.0.7:11434":               "",
			"https://gpu.example.com/ollama":      "",
			"http://127.0.0.1:11434/v1":           "provider.base_url",
			"http://127.0.0.1:11434/api":          "provider.base_url",
			"http://127.0.0.1:11434/api/chat":     "provider.base_url",
			"https://gpu.example.com/ollama/v1/":  "provider.base_url",
			"http://user:pw@127.0.0.1:11434":      "provider.base_url",
			"ollama:11434":                        "provider.base_url",
			"http://127.0.0.1:11434?keep_alive=1": "provider.base_url",
		} {
			if got := refused(with(base, DefaultContextTokens, free).Validate(local)); got != want {
				t.Errorf("Validate(%s) refused %q, want %q", base, got, want)
			}
		}
		// Hosted rules would refuse a plain-http or private server; hosted
		// never offers Ollama at all (TestOllamaDeployment).
		if got := refused(with("http://127.0.0.1:11434", DefaultContextTokens, free).Validate(hosted)); got != "provider.base_url" {
			t.Errorf("hosted rules refused %q", got)
		}
	})

	// The context window is one a draft fits in, up to what any model has.
	t.Run("bounds the context window", func(t *testing.T) {
		for window, want := range map[uint32]string{
			0:                    "provider.context_tokens",
			4096:                 "provider.context_tokens",
			MinContextTokens - 1: "provider.context_tokens",
			MinContextTokens:     "",
			131072:               "",
			MaxContextTokens:     "",
			MaxContextTokens + 1: "provider.context_tokens",
		} {
			if got := refused(with("http://127.0.0.1:11434", window, free).Validate(local)); got != want {
				t.Errorf("a window of %d refused %q, want %q", window, got, want)
			}
		}
		err := with("http://127.0.0.1:11434", 1024, free).Validate(local)
		if err == nil || err.Error() != "provider.context_tokens must be 65536 to 1048576" {
			t.Errorf("Validate() = %v", err)
		}
	})

	// A model there is free, or costs what its owner says: Helix's price
	// table has no Ollama rows.
	t.Run("prices models free or at the owner's rates", func(t *testing.T) {
		for name, tc := range map[string]struct {
			price Price
			want  string
		}{
			"free":             {PriceFree{}, ""},
			"the owner's rate": {PriceRates{Input: 0, Output: 1}, ""},
			"by table":         {PriceTable{}, "models.draft.price"},
			"as another model": {PriceTableAs{Model: "qwen3:32b"}, "models.draft.price"},
		} {
			if got := refused(with("http://127.0.0.1:11434", DefaultContextTokens, Model{Name: "qwen3:8b", Price: tc.price}).Validate(local)); got != tc.want {
				t.Errorf("%s: refused %q, want %q", name, got, tc.want)
			}
		}
		mixed := ollama(free)
		mixed.Models.Verify.Price = PriceTable{}
		if err := mixed.Validate(local); err == nil || err.Error() != "models.verify.price must be free or your own rates for a model on an Ollama server" {
			t.Errorf("Validate() = %v", err)
		}
	})

	// Saved, the address is kept in one form; the window as given.
	t.Run("stores the server normalized", func(t *testing.T) {
		s := with("http://Host.Docker.Internal:11434/", 131072, free).Normalized(local)
		if s.Provider != (Ollama{BaseURL: "http://host.docker.internal:11434", ContextTokens: 131072}) {
			t.Fatalf("Normalized() = %+v", s.Provider)
		}
		if KindOllama.Key() != KeyRefused || !KindOllama.Free() || Label(s.Provider) != "Ollama" {
			t.Fatalf("Ollama's key rule %v, free %v, label %q", KindOllama.Key(), KindOllama.Free(), Label(s.Provider))
		}
		if _, ok := Suggested(KindOllama); ok || CanReuseKey(s, s) {
			t.Fatal("Ollama suggests models or keeps a key")
		}
	})
}

func TestOllamaDeployment(t *testing.T) {
	// Hosted Foundry never offers Ollama: a native server takes no key, so a
	// hosted one would be open to anyone.
	t.Run("never offers Ollama hosted", func(t *testing.T) {
		cfg := hostedConfig(sentinel)
		cfg.Foundry.AI.Kinds = []string{"anthropic", "ollama"}
		if _, err := NewDeployment(cfg); err == nil || !strings.Contains(err.Error(), "cannot offer ollama in cloud") {
			t.Fatalf("NewDeployment() = %v", err)
		}
		hosted := &Deployment{Kinds: []Kind{KindAnthropic, KindOllama}}
		if hosted.Offers(KindOllama) || !hosted.Offers(KindAnthropic) {
			t.Fatalf("a hosted deployment offers %v", hosted.Kinds)
		}
		if d := localDeployment(t); !d.Offers(KindOllama) || d.Kinds[0] != KindOllama {
			t.Fatalf("a local install offers %v", d.Kinds)
		}
	})

	// The agent's probe fails a draft in a window under 65536
	// (agent_server::probe::context_floor(Role::Draft)), and a draft's
	// prompt and tools don't fit in less: a smaller minimum could only fail
	// its test, and a smaller default every test, and every draft of a local
	// install's default, which no probe tests.
	t.Run("drafts in the agent's floor", func(t *testing.T) {
		const draftFloor = 65536
		if MinContextTokens != draftFloor || DefaultContextTokens < draftFloor {
			t.Fatalf("windows of %d up, %d by default; the agent drafts in %d up", MinContextTokens, DefaultContextTokens, draftFloor)
		}
		under := ollama(Model{Name: "qwen3:8b", Price: PriceFree{}})
		under.Provider = Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: draftFloor - 1}
		if got := refused(under.Validate(Rules{AllowPrivate: true})); got != "provider.context_tokens" {
			t.Errorf("a window under the floor refused %q", got)
		}
		cfg := hostedConfig("")
		local(config.AIDefault{Provider: "ollama", BaseURL: "http://127.0.0.1:11434", Models: models("qwen3:8b")}, true)(cfg)
		d, err := NewDeployment(cfg)
		if err != nil {
			t.Fatal(err)
		}
		r, err := (&Resolver{DB: activeConfigs{}, Deployment: d}).Resolve(context.Background(), "ws-1")
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Turn().Draft.ContextTokens; got < draftFloor {
			t.Errorf("an install's default drafts in %d", got)
		}
		for _, c := range d.ProbeRequest("cfg-o", d.Default.Settings, 15*time.Minute).Checks {
			if c.Model.ContextTokens < draftFloor {
				t.Errorf("a default setting probes %s in %d", c.Role, c.Model.ContextTokens)
			}
		}
	})

	// An install's default may be an Ollama server, in the window it sets
	// or the default one; a window is Ollama's alone.
	t.Run("reads an Ollama default", func(t *testing.T) {
		def := config.AIDefault{Provider: "ollama", BaseURL: "http://host.docker.internal:11434", Models: models("qwen3:8b")}
		for window, want := range map[uint32]uint32{0: DefaultContextTokens, 131072: 131072} {
			cfg := hostedConfig("")
			def.ContextTokens = window
			local(def, true)(cfg)
			d, err := NewDeployment(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := d.Default.Settings.Provider; got != (Ollama{BaseURL: "http://host.docker.internal:11434", ContextTokens: want}) {
				t.Fatalf("a window of %d: %+v", window, got)
			}
			if !d.Default.Key.IsZero() || d.Default.Settings.Models.Extract != (Model{Name: "qwen3:8b", Price: PriceFree{}}) {
				t.Fatalf("the default = %+v", d.Default)
			}
		}
		for name, tc := range map[string]struct {
			def  config.AIDefault
			want string
		}{
			"a small window": {config.AIDefault{Provider: "ollama", BaseURL: "http://127.0.0.1:11434", ContextTokens: 8192, Models: models("qwen3:8b")},
				"provider.context_tokens"},
			"the OpenAI-compatible path": {config.AIDefault{Provider: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Models: models("qwen3:8b")},
				"provider.base_url"},
			"no server": {config.AIDefault{Provider: "ollama", Models: models("qwen3:8b")}, "provider.base_url"},
			"a window for Claude": {config.AIDefault{Provider: "anthropic", APIKey: "sk-ant-local", ContextTokens: 65536, Models: models("claude-opus-5-5")},
				"context_tokens is not used by anthropic"},
		} {
			cfg := hostedConfig("")
			local(tc.def, true)(cfg)
			if _, err := NewDeployment(cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: NewDeployment() = %v, want %q", name, err, tc.want)
			}
		}
	})
}

func TestOllamaModels(t *testing.T) {
	ctx := context.Background()
	d := localDeployment(t)
	keys := keyring(t)
	free := Model{Name: "qwen3:8b", Price: PriceFree{}}
	resolve := func(t *testing.T, s Settings, report string) Resolved {
		t.Helper()
		c := savedConfig(t, keys, "cfg-o", "ws-1", "", s)
		if report != "" {
			c.ProbeReport = json.RawMessage(report)
		}
		got, err := (&Resolver{DB: activeConfigs{"ws-1": c}, Keyring: keys, Deployment: d}).Resolve(ctx, "ws-1")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	tested := func(window uint32) string {
		return fmt.Sprintf(`{"checks":[{"role":"draft","passed":true,"reasoning":false,"seconds":40}],"ollama":{"digest":%q,"context_length":%d}}`, digest, window)
	}

	// Calls go one at a time under the install's call timeout, with no key;
	// a model tested alone runs on the digest its probe read, in no more
	// than the model's own window.
	t.Run("runs the model its probe tested", func(t *testing.T) {
		r := resolve(t, ollama(free), tested(262144))
		turn := r.Turn()
		want := ontologyagent.Endpoint{Provider: ontologyagent.Ollama{BaseURL: "http://127.0.0.1:11434"}, CallTimeoutSecs: 1800, MaxCallsInFlight: 1}
		if turn.Endpoint != want || !r.Key.IsZero() || r.Label() != "Ollama" {
			t.Fatalf("Turn().Endpoint = %+v, key %v", turn.Endpoint, r.Key)
		}
		draft := ontologyagent.ModelSpec{Name: "qwen3:8b", Price: ontologyagent.PriceFree{}, ContextTokens: DefaultContextTokens, Revision: digest}
		extract := draft
		extract.Reasoning = true
		if !specEqual(turn.Draft, draft) || !specEqual(turn.Extract, extract) || !specEqual(r.Ask().Ask, extract) || !specEqual(r.Extract().Verify, extract) {
			t.Fatalf("Turn() = %+v, Ask() = %+v", turn, r.Ask())
		}
		// A title runs on the same model without reasoning.
		if title := r.Title().Title; title.Revision != digest || title.Reasoning || title.ContextTokens != DefaultContextTokens {
			t.Fatalf("Title() = %+v", title)
		}
		// A model whose own window is smaller than the owner's loads in its own.
		wide := ollama(free)
		wide.Provider = Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: 262144}
		if got := resolve(t, wide, tested(131072)); got.Turn().Draft.ContextTokens != 131072 || got.Ask().Ask.Revision != digest {
			t.Fatalf("a smaller model window: %+v", got.Turn().Draft)
		}
		// A server that didn't say the model's window leaves the owner's.
		if got := resolve(t, ollama(free), tested(0)); got.Turn().Draft.ContextTokens != DefaultContextTokens {
			t.Fatalf("no model window: %+v", got.Turn().Draft)
		}
	})

	// The probe reads one model: a setting of several, and one never
	// probed, run on what the server has, in the owner's window.
	t.Run("pins only a setting of one model", func(t *testing.T) {
		mixed := ollama(free)
		mixed.Models.Draft = Model{Name: "qwen3:32b", Price: PriceFree{}}
		r := resolve(t, mixed, tested(131072))
		for name, spec := range map[string]ontologyagent.ModelSpec{"draft": r.Turn().Draft, "extract": r.Extract().Extract, "title": r.Title().Title} {
			if spec.Revision != "" || spec.ContextTokens != DefaultContextTokens {
				t.Errorf("%s of several models = %+v", name, spec)
			}
		}
		if got := resolve(t, ollama(free), "").Turn().Draft; got.Revision != "" || got.ContextTokens != DefaultContextTokens {
			t.Errorf("an unprobed setting = %+v", got)
		}
		// Another kind never sends a revision, whatever its report holds.
		other := Resolved{Source: SourceWorkspace, Settings: everyKind()[KindCompatible], Report: ProbeReport{Ollama: &OllamaReport{Digest: digest}}}
		if got := other.Turn().Draft; got.Revision != "" || got.ContextTokens != 0 {
			t.Errorf("a compatible server's spec = %+v", got)
		}
	})

	// A probe tests the owner's window and names no revision: it reads the
	// digest its runs then send.
	t.Run("probes the owner's window", func(t *testing.T) {
		s := Settings{Provider: Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: 131072}, Models: every(free)}
		probe := d.ProbeRequest("cfg-o", s, 15*time.Minute)
		if probe.Endpoint.MaxCallsInFlight != 1 || probe.Endpoint.CallTimeoutSecs != 1800 || probe.Limits.MaxWallSecs != 900 {
			t.Fatalf("probe = %+v", probe)
		}
		for _, c := range probe.Checks {
			if c.Model.ContextTokens != 131072 || c.Model.Revision != "" || c.Model.Price != (ontologyagent.PriceFree{}) {
				t.Errorf("check %s = %+v", c.Role, c.Model)
			}
		}
	})

	// What the probe read is kept only when its runs can send it.
	t.Run("keeps only a digest the agent takes", func(t *testing.T) {
		for value, ok := range map[string]bool{
			digest:                                      true,
			"sha256:" + strings.Repeat("CD", 32):        false,
			strings.Repeat("cd", 32):                    false,
			"sha256:" + strings.Repeat("cd", 31):        false,
			"sha256:" + strings.Repeat("cd", 32) + "\n": false,
			"": false,
		} {
			if err := (ProbeReport{Ollama: &OllamaReport{Digest: value}}).Usable(); (err == nil) != ok {
				t.Errorf("Usable(%q) = %v", value, err)
			}
		}
		if err := (ProbeReport{}).Usable(); err != nil {
			t.Errorf("a report without Ollama: %v", err)
		}
	})
}

func TestOllamaFailure(t *testing.T) {
	saved := Resolved{Source: SourceWorkspace, Settings: ollama(Model{Name: "qwen3:8b", Price: PriceFree{}})}
	// An install's own Ollama default is its owner's to fix, as a saved
	// one is: never Helix's, never raised with on-call.
	installed := saved
	installed.Source = SourceDefault

	// A model the server lacks is one not pulled yet; a prompt too long for
	// the window may fit a larger one; a model pulled again since its test
	// is tested again.
	t.Run("says what an owner of the server can do", func(t *testing.T) {
		for f, want := range map[ontologyagent.ModelFailure]string{
			ontologyagent.ModelNotFound: "Ollama has no model qwen3:8b. Pull it on the Ollama server with ollama pull, or an owner can choose another in Settings › AI models.",
			ontologyagent.ModelContextTooLong: "This was too long for qwen3:8b in its context window. An owner can raise the window, or choose a model with a larger one, in " +
				"Settings › AI models.",
			ontologyagent.ModelChanged:     "The Ollama model changed since it was tested. An owner can test it again in Settings › AI models.",
			ontologyagent.ModelUnsupported: "qwen3:8b can’t do what this task needs (tool calls or structured answers).",
			ontologyagent.ModelTruncated:   "The model ran out of room for its answer.",
		} {
			for name, r := range map[string]Resolved{"saved": saved, "installed": installed} {
				if got := DraftFailure(r, f); got != (Copy{Message: want, Permanent: true}) {
					t.Errorf("%s: DraftFailure(%s) = %+v, want %q", name, f, got, want)
				}
				if got := AskFailure(r, f); got.Message != want || !got.Permanent || got.Ops {
					t.Errorf("%s: AskFailure(%s) = %+v", name, f, got)
				}
			}
		}
	})

	// A server that is down, busy or slow is retried, by Ollama's name.
	t.Run("retries a server that is down", func(t *testing.T) {
		for _, f := range []ontologyagent.ModelFailure{ontologyagent.ModelUnavailable, ontologyagent.ModelTimeout, ontologyagent.ModelMalformed} {
			for name, r := range map[string]Resolved{"saved": saved, "installed": installed} {
				if got := DraftFailure(r, f); got != (Copy{Message: "Ollama is unavailable; Helix will retry"}) {
					t.Errorf("%s: DraftFailure(%s) = %+v", name, f, got)
				}
				if got := AskFailure(r, f); got != (Copy{Message: "the model could not be reached; ask again"}) {
					t.Errorf("%s: AskFailure(%s) = %+v", name, f, got)
				}
			}
		}
		if got := DraftFailure(installed, ontologyagent.ModelRefused); got != (Copy{Message: "Ollama declined to draft this ontology", Permanent: true}) {
			t.Errorf("refused = %+v", got)
		}
	})

	// A model every document would miss stops the load with that copy, on
	// the install's default too; a title falls back quietly.
	t.Run("stops a load whose model is missing", func(t *testing.T) {
		for name, r := range map[string]Resolved{"saved": saved, "installed": installed} {
			c, stop := DocumentFailure(r, ontologyagent.ModelNotFound)
			if !stop || !strings.HasPrefix(c.Message, "Ollama has no model qwen3:8b. Pull it") || c.Ops {
				t.Errorf("%s: DocumentFailure() = %+v, %v", name, c, stop)
			}
			if c, stop := DocumentFailure(r, ontologyagent.ModelUnavailable); stop || c != (Copy{}) {
				t.Errorf("%s: an unavailable server stopped the load: %+v", name, c)
			}
			if got := TitleFailure(r, ontologyagent.ModelNotFound); got.Ops {
				t.Errorf("%s: a title raised on-call: %+v", name, got)
			}
		}
		// A load reading with two models names both.
		two := saved
		two.Settings.Models.Verify.Name = "qwen3:32b"
		if c, _ := DocumentFailure(two, ontologyagent.ModelNotFound); !strings.HasPrefix(c.Message, "Ollama has no model qwen3:8b or qwen3:32b. Pull it") {
			t.Errorf("two models: %q", c.Message)
		}
	})

	// Helix's Claude keeps its own copy: only an Ollama default is worded
	// for an owner.
	t.Run("keeps Helix's Claude's copy", func(t *testing.T) {
		house := Resolved{Source: SourceDefault, Settings: claude}
		if got := DraftFailure(house, ontologyagent.ModelNotFound); got != (Copy{Message: houseUnavailable, Ops: true}) {
			t.Fatalf("DraftFailure(house) = %+v", got)
		}
		if c, stop := DocumentFailure(house, ontologyagent.ModelNotFound); stop || !c.Ops {
			t.Fatalf("DocumentFailure(house) = %+v, %v", c, stop)
		}
	})
}

// A local install's default may be an Ollama server: it runs model work one
// call at a time under the local call timeout, in the window the install
// chose.
func TestOllamaLocalDefault(t *testing.T) {
	// A local install's foundry.ai, its default set from its .env.
	cfg := config.Defaults()
	cfg.Host.Local = true
	cfg.Foundry.AI.Kinds = []string{"ollama", "anthropic", "openai", "azure_openai", "openai_compatible", "bedrock"}
	cfg.Foundry.AI.AllowPrivateHosts, cfg.Foundry.AI.CallTimeout = true, 30*time.Minute
	cfg.Foundry.AI.Default = config.AIDefault{Provider: "ollama", BaseURL: "http://127.0.0.1:11434", ContextTokens: 131072,
		Models: models("qwen3:8b")}
	d, err := NewDeployment(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Local || !d.Rules.AllowPrivate || d.Kinds[0] != KindOllama || !d.Offers(KindOllama) {
		t.Fatalf("NewDeployment() = %+v", d)
	}
	r, err := (&Resolver{DB: activeConfigs{}, Deployment: d}).Resolve(context.Background(), "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	want := ontologyagent.Endpoint{Provider: ontologyagent.Ollama{BaseURL: "http://127.0.0.1:11434"}, CallTimeoutSecs: 1800, MaxCallsInFlight: 1}
	turn := r.Turn()
	if r.Source != SourceDefault || !r.Key.IsZero() || turn.Endpoint != want ||
		turn.Draft != (ontologyagent.ModelSpec{Name: "qwen3:8b", Price: ontologyagent.PriceFree{}, Reasoning: true, ContextTokens: 131072}) {
		t.Fatalf("Resolve() = %+v, Turn() = %+v", r, turn)
	}
	// The request is one the agent takes, as is a pinned one.
	request := ontologyagent.TurnRequest{SessionID: "s", RequestID: "r", Sources: []ontologyagent.Source{},
		Input:  ontologyagent.Input{Start: ontologyagent.Start{Workflow: "w", Goals: []string{}}},
		Limits: ontologyagent.Limits{MaxUSD: 10, MaxSteps: 200, MaxWallSecs: 2400}, Models: turn}
	pinned := Resolved{Source: SourceWorkspace, Settings: r.Settings, CallTimeout: r.CallTimeout, Report: ProbeReport{
		Ollama: &OllamaReport{Digest: digest, ContextLength: 262144},
	}}
	for _, models := range []ontologyagent.TurnModels{turn, pinned.Turn()} {
		request.Models = models
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := contracttest.Validate(t, contracttest.Schema(t, "turn_request.json"), raw); err != nil {
			t.Errorf("turn_request.json refused %s: %v", raw, err)
		}
	}
	if got := DraftFailure(r, ontologyagent.ModelNotFound); !got.Permanent || got.Ops || !strings.HasPrefix(got.Message, "Ollama has no model qwen3:8b. Pull it") {
		t.Fatalf("DraftFailure(not_found) = %+v", got)
	}
}

func ExampleProbeReport_Usable() {
	tested := ProbeReport{Ollama: &OllamaReport{Digest: "sha256:" + strings.Repeat("0", 64), ContextLength: 262144}}
	fmt.Println(tested.Usable())
	fmt.Println(ProbeReport{Ollama: &OllamaReport{Digest: "qwen3:8b"}}.Usable())
	// Output:
	// <nil>
	// the probe's Ollama digest is not sha256: and 64 hex digits
}

func ExampleDraftFailure_ollama() {
	r := Resolved{Source: SourceDefault, Settings: Settings{
		Provider: Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: DefaultContextTokens},
		Models:   Roles{Draft: Model{Name: "qwen3:8b", Price: PriceFree{}}},
	}}
	fmt.Println(DraftFailure(r, ontologyagent.ModelNotFound).Message)
	// Output:
	// Ollama has no model qwen3:8b. Pull it on the Ollama server with ollama pull, or an owner can choose another in Settings › AI models.
}
