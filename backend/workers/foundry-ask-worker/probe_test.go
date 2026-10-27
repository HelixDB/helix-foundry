package foundryaskworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// probeQueueStub holds one setting waiting for its probe, and records how
// each claim ended.
type probeQueueStub struct {
	mu       sync.Mutex
	waiting  *model.FoundryAIConfig
	claims   []time.Duration
	released []string
	returned []string
	finished []model.FoundryAIProbeOutcome
	// finishErr is every finish's answer.
	finishErr error
}

func (q *probeQueueStub) ClaimFoundryAIProbe(_ context.Context, owner string, lease time.Duration, maxAttempts int) (*model.FoundryAIConfig, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if owner != "w1" || maxAttempts != 3 {
		return nil, fmt.Errorf("unexpected claim by %s of %d attempts", owner, maxAttempts)
	}
	q.claims = append(q.claims, lease)
	c := q.waiting
	q.waiting = nil
	return c, nil
}

func (q *probeQueueStub) ReleaseFoundryAIProbe(_ context.Context, configID, owner string, maxAttempts int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.released = append(q.released, configID+" "+owner+fmt.Sprint(maxAttempts))
	return nil
}

func (q *probeQueueStub) ReturnFoundryAIProbe(_ context.Context, configID, owner string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.returned = append(q.returned, configID+" "+owner)
	return nil
}

func (q *probeQueueStub) FinishFoundryAIProbe(_ context.Context, configID, owner string, outcome model.FoundryAIProbeOutcome) (model.FoundryAIProbeState, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.finished = append(q.finished, outcome)
	if q.finishErr != nil {
		return "", q.finishErr
	}
	if outcome.Failure != nil {
		return model.FoundryAIProbeFailed, nil
	}
	return model.FoundryAIProbePassed, nil
}

// probeUsage is a probe result's usage.
const probeUsage = `"usage":{"input":900,"cache_read":0,"cache_write":0,"output":60,"usd":0.02}`

// probeResult answers every check of a probe request: passed, but for the
// role failing, which fails with failure and detail.
func probeResult(failing, failure, detail string) func(string) (string, error) {
	return func(req string) (string, error) {
		var r struct {
			Checks []struct {
				Role string `json:"role"`
			} `json:"checks"`
		}
		if err := json.Unmarshal([]byte(req), &r); err != nil {
			return "", err
		}
		var checks []string
		for _, c := range r.Checks {
			outcome := `{"passed":{"seconds":1.5,"reasoning":true}}`
			if c.Role == failing {
				outcome = `{"failed":{"failure":"` + failure + `","detail":"` + detail + `"}}`
			}
			checks = append(checks, `{"role":"`+c.Role+`","outcome":`+outcome+`}`)
		}
		return `{"checks":[` + strings.Join(checks, ",") + `],` + probeUsage + `}`, nil
	}
}

type probeHarness struct {
	prober *prober
	queue  *probeQueueStub
	agent  *fakeAgent
	keys   *secretbox.Keyring
}

// newProbeHarness probes the owner's own Claude, its key sealed, on a hosted
// deployment of Helix's Claude.
func newProbeHarness(t *testing.T) *probeHarness {
	t.Helper()
	cfg := config.Defaults()
	cfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: houseKey, Models: config.AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
	}}
	deployment, err := ai.NewDeployment(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(ai.Settings{Provider: ai.Anthropic{}, Models: ai.Roles{
		Draft: ai.Model{Name: "claude-opus-5-5", Price: ai.PriceTable{}}, Ask: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}},
		Extract: ai.Model{Name: "claude-haiku-5-5", Price: ai.PriceTable{}}, Verify: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}},
	}})
	c := &model.FoundryAIConfig{ID: "cfg-1", WorkspaceID: "ws-1", Kind: "anthropic", Settings: settings, ProbeAttempts: 1}
	if c.Secret, err = ai.SealKey(keys, c.ID, ontologyagent.NewModelKey(ownKey)); err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{info: &agentv1.GetInfoResponse{ContractVersion: ontologyagent.ContractVersion, Providers: []string{"anthropic"}},
		probe: probeResult("", "", "")}
	queue := &probeQueueStub{waiting: c}
	return &probeHarness{
		prober: &prober{queue: queue, agent: startAgent(t, agent), keys: keys, deployment: deployment, cfg: cfg.Foundry.AI.Probe, owner: "w1"},
		queue:  queue, agent: agent, keys: keys,
	}
}

// tick runs one probe tick with its log captured.
func (h *probeHarness) tick(t *testing.T) string {
	t.Helper()
	var logs bytes.Buffer
	h.prober.tick(log.Into(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil))))
	return logs.String()
}

func TestProbe(t *testing.T) {
	// A passed probe records every check, the key having gone to the agent
	// in the call's metadata alone, and nowhere else.
	t.Run("records a passed probe", func(t *testing.T) {
		h := newProbeHarness(t)
		logs := h.tick(t)
		if len(h.queue.claims) != 1 || h.queue.claims[0] != 3*time.Minute || len(h.queue.finished) != 1 || len(h.queue.released) != 0 {
			t.Fatalf("claims %v, finished %v, released %v", h.queue.claims, h.queue.finished, h.queue.released)
		}
		got := h.queue.finished[0]
		if got.Failure != nil || !strings.Contains(string(got.Report), `{"role":"title","passed":true,"reasoning":true,"seconds":1.5}`) {
			t.Fatalf("outcome = %+v, report %s", got, got.Report)
		}
		if len(h.agent.probes) != 1 || fmt.Sprint(h.agent.probeKeys) != "[["+ownKey+"]]" {
			t.Fatalf("probes %v, keys %v", h.agent.probes, h.agent.probeKeys)
		}
		if err := contracttest.Validate(t, contracttest.Schema(t, "probe_request.json"), []byte(h.agent.probes[0])); err != nil {
			t.Fatalf("probe request %s: %v", h.agent.probes[0], err)
		}
		if !strings.Contains(h.agent.probes[0], `"request_id":"cfg-1"`) || !strings.Contains(h.agent.probes[0], `"limits":{"max_usd":0.5,"max_wall_secs":120}`) {
			t.Fatalf("probe request = %s", h.agent.probes[0])
		}
		if stored := h.agent.probes[0] + string(got.Report) + logs; strings.Contains(stored, ownKey) || strings.Contains(stored, houseKey) {
			t.Fatalf("a key reached a request, the report or a log")
		}
		if !strings.Contains(logs, `"state":"passed"`) {
			t.Fatalf("log = %s", logs)
		}
	})

	// A failed check fails the setting with that check's failure and
	// detail; the one before it stays.
	t.Run("records a failed check", func(t *testing.T) {
		h := newProbeHarness(t)
		h.agent.probe = probeResult("ask", "unsupported", "tools_rejected")
		h.tick(t)
		if len(h.queue.finished) != 1 || h.queue.finished[0].Failure == nil ||
			*h.queue.finished[0].Failure != (model.FoundryAIProbeFailure{Failure: "unsupported", Detail: "tools_rejected"}) {
			t.Fatalf("finished %+v", h.queue.finished)
		}
		if !strings.Contains(string(h.queue.finished[0].Report), `{"role":"ask","passed":false,"reasoning":false,"seconds":0}`) {
			t.Fatalf("report = %s", h.queue.finished[0].Report)
		}
	})

	// A probe that couldn't run gives its claim back, its attempt counted,
	// and logs why for on-call; no key is logged.
	t.Run("gives back a probe that couldn't run", func(t *testing.T) {
		for name, setup := range map[string]func(h *probeHarness){
			"no keyring":            func(h *probeHarness) { h.prober.keys = nil },
			"a key that won't open": func(h *probeHarness) { h.queue.waiting.Secret.Ciphertext[0] ^= 0xff },
			"another contract": func(h *probeHarness) {
				h.agent.info = &agentv1.GetInfoResponse{ContractVersion: "3.12.0", Providers: []string{"anthropic"}}
			},
			"a kind the agent doesn't call": func(h *probeHarness) { h.agent.info.Providers = []string{"openai"} },
			"an agent unavailable": func(h *probeHarness) {
				h.agent.probe = func(string) (string, error) { return "", status.Error(codes.Unavailable, "draining") }
			},
			"an unreadable result": func(h *probeHarness) {
				h.agent.probe = func(string) (string, error) { return `{"checks":[]}`, nil }
			},
			"unreadable settings": func(h *probeHarness) { h.queue.waiting.Settings = json.RawMessage(`{"v":9}`) },
		} {
			h := newProbeHarness(t)
			setup(h)
			logs := h.tick(t)
			if len(h.queue.finished) != 0 || fmt.Sprint(h.queue.released) != "[cfg-1 w13]" || len(h.queue.returned) != 0 || !strings.Contains(logs, `"level":"ERROR"`) {
				t.Fatalf("%s: finished %v, released %v, log %s", name, h.queue.finished, h.queue.released, logs)
			}
			if strings.Contains(logs, ownKey) {
				t.Fatalf("%s: the key was logged", name)
			}
		}
	})

	// A worker that stops mid-probe gives it back uncounted, as a warning:
	// the setting and the agent did nothing wrong.
	t.Run("gives back uncounted a probe cut short as the worker stops", func(t *testing.T) {
		h := newProbeHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		h.agent.probe = func(string) (string, error) {
			cancel()
			return "", status.Error(codes.Unavailable, "draining")
		}
		var logs bytes.Buffer
		h.prober.tick(log.Into(ctx, slog.New(slog.NewJSONHandler(&logs, nil))))
		if fmt.Sprint(h.queue.returned) != "[cfg-1 w1]" || len(h.queue.released) != 0 || len(h.queue.finished) != 0 {
			t.Fatalf("returned %v, released %v, finished %v", h.queue.returned, h.queue.released, h.queue.finished)
		}
		if strings.Contains(logs.String(), `"level":"ERROR"`) {
			t.Fatalf("a stopping worker logged an error: %s", logs.String())
		}
	})

	// The lease covers the probe's timeout and everything around it, as
	// config checks: no other replica claims a probe still running.
	t.Run("ends inside its lease", func(t *testing.T) {
		if held := config.AskReadyWait + probeTail + finishTimeout; held > config.AIProbeOverhead {
			t.Fatalf("a claim is held %s past the probe's timeout, but config allows %s", held, config.AIProbeOverhead)
		}
		h := newProbeHarness(t)
		h.prober.cfg.Lease = finishTimeout + 300*time.Millisecond
		h.agent.probe = func(req string) (string, error) {
			time.Sleep(2 * time.Second)
			return probeResult("", "", "")(req)
		}
		start := time.Now()
		h.tick(t)
		if took := time.Since(start); took > time.Second || fmt.Sprint(h.queue.released) != "[cfg-1 w13]" || len(h.queue.finished) != 0 {
			t.Fatalf("took %s, released %v, finished %v", took, h.queue.released, h.queue.finished)
		}
	})

	// A kind that takes no key is probed with none.
	t.Run("probes a keyless kind with no key", func(t *testing.T) {
		h := newProbeHarness(t)
		settings, _ := json.Marshal(ai.Settings{Provider: ai.Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: 65536}, Models: ai.Roles{
			Draft: ai.Model{Name: "qwen3:32b", Price: ai.PriceFree{}}, Ask: ai.Model{Name: "qwen3:32b", Price: ai.PriceFree{}},
			Extract: ai.Model{Name: "qwen3:8b", Price: ai.PriceFree{}}, Verify: ai.Model{Name: "qwen3:8b", Price: ai.PriceFree{}},
		}})
		h.queue.waiting = &model.FoundryAIConfig{ID: "cfg-2", WorkspaceID: "ws-1", Kind: "ollama", Settings: settings}
		h.agent.info.Providers = []string{"anthropic", "ollama"}
		h.tick(t)
		if len(h.queue.finished) != 1 || fmt.Sprint(h.agent.probeKeys) != "[[]]" ||
			!strings.Contains(h.agent.probes[0], `"provider":{"ollama":{"base_url":"http://127.0.0.1:11434"}}`) {
			t.Fatalf("finished %v, keys %v, probe %v", h.queue.finished, h.agent.probeKeys, h.agent.probes)
		}
	})

	// OpenAI and an Azure resource are probed on their endpoint with the
	// owner's key, once the agent calls their kind.
	t.Run("probes OpenAI and Azure with their keys", func(t *testing.T) {
		for kind, tc := range map[ai.Kind]struct {
			settings ai.Settings
			endpoint string
		}{
			ai.KindOpenAI: {ai.Settings{Provider: ai.OpenAI{}, Models: ai.Roles{
				Draft: ai.Model{Name: "gpt-6.1-sol", Price: ai.PriceTable{}}, Ask: ai.Model{Name: "gpt-6.1-sol", Price: ai.PriceTable{}},
				Extract: ai.Model{Name: "gpt-6-luna", Price: ai.PriceTable{}}, Verify: ai.Model{Name: "gpt-6.1-sol", Price: ai.PriceTable{}},
			}}, `"provider":"openai"`},
			ai.KindAzureOpenAI: {ai.Settings{Provider: ai.AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai/v1"}, Models: ai.Roles{
				Draft: ai.Model{Name: "prod-sol", Price: ai.PriceTableAs{Model: "gpt-6.1-sol"}}, Ask: ai.Model{Name: "prod-sol", Price: ai.PriceTableAs{Model: "gpt-6.1-sol"}},
				Extract: ai.Model{Name: "prod-luna", Price: ai.PriceTableAs{Model: "gpt-6-luna"}}, Verify: ai.Model{Name: "prod-sol", Price: ai.PriceTableAs{Model: "gpt-6.1-sol"}},
			}}, `"provider":{"azure_openai":{"endpoint":"https://acme.openai.azure.com/openai/v1"}}`},
		} {
			h := newProbeHarness(t)
			raw, _ := json.Marshal(tc.settings)
			c := &model.FoundryAIConfig{ID: "cfg-" + string(kind), WorkspaceID: "ws-1", Kind: string(kind), Settings: raw}
			var err error
			if c.Secret, err = ai.SealKey(h.keys, c.ID, ontologyagent.NewModelKey(ownKey)); err != nil {
				t.Fatal(err)
			}
			h.queue.waiting = c
			h.agent.info.Providers = []string{"anthropic", "openai", "azure_openai"}
			logs := h.tick(t)
			if len(h.queue.finished) != 1 || fmt.Sprint(h.agent.probeKeys) != "[["+ownKey+"]]" || !strings.Contains(h.agent.probes[0], tc.endpoint) {
				t.Fatalf("%s: finished %v, keys %v, probe %v", kind, h.queue.finished, h.agent.probeKeys, h.agent.probes)
			}
			if strings.Contains(logs, ownKey) || strings.Contains(h.agent.probes[0], ownKey) {
				t.Fatalf("%s: the key left its metadata", kind)
			}
		}
	})

	// A Bedrock setting is probed in its region, on its FIPS host if chosen,
	// with the owner's Bedrock API key; an agent that doesn't call Bedrock
	// probes nothing.
	t.Run("probes Bedrock with its key", func(t *testing.T) {
		roles, _ := ai.Suggested(ai.KindBedrock)
		raw, _ := json.Marshal(ai.Settings{Provider: ai.Bedrock{Region: "eu-west-1", FIPS: true}, Models: roles})
		for name, providers := range map[string][]string{"an agent that calls Bedrock": {"anthropic", "bedrock"}, "one that doesn't": {"anthropic"}} {
			h := newProbeHarness(t)
			c := &model.FoundryAIConfig{ID: "cfg-bedrock", WorkspaceID: "ws-1", Kind: "bedrock", Settings: raw}
			var err error
			if c.Secret, err = ai.SealKey(h.keys, c.ID, ontologyagent.NewModelKey(ownKey)); err != nil {
				t.Fatal(err)
			}
			h.queue.waiting = c
			h.agent.info.Providers = providers
			logs := h.tick(t)
			if strings.Contains(logs, ownKey) || slices.ContainsFunc(h.agent.probes, func(p string) bool { return strings.Contains(p, ownKey) }) {
				t.Fatalf("%s: the key left its metadata", name)
			}
			if !slices.Contains(providers, "bedrock") {
				if len(h.agent.probes) != 0 || len(h.queue.finished) != 0 || fmt.Sprint(h.queue.released) != "[cfg-bedrock w13]" {
					t.Fatalf("%s: probes %v, finished %v, released %v", name, h.agent.probes, h.queue.finished, h.queue.released)
				}
				continue
			}
			if len(h.queue.finished) != 1 || fmt.Sprint(h.agent.probeKeys) != "[["+ownKey+"]]" ||
				!strings.Contains(h.agent.probes[0], `"provider":{"bedrock":{"region":"eu-west-1","fips":true}}`) ||
				!strings.Contains(h.agent.probes[0], `"name":"`+roles.Draft.Name+`"`) {
				t.Fatalf("%s: finished %v, keys %v, probe %v", name, h.queue.finished, h.agent.probeKeys, h.agent.probes)
			}
		}
	})

	// Nothing waiting, nothing done; a probe whose lease another worker
	// took records nothing more.
	t.Run("does nothing without a claim, and gives up a lost one", func(t *testing.T) {
		h := newProbeHarness(t)
		h.queue.waiting = nil
		h.tick(t)
		if len(h.queue.claims) != 1 || len(h.agent.probes) != 0 || len(h.queue.finished) != 0 {
			t.Fatalf("claims %v, probes %v", h.queue.claims, h.agent.probes)
		}
		h = newProbeHarness(t)
		h.queue.finishErr = foundrystore.ErrNotFound
		if logs := h.tick(t); !strings.Contains(logs, "another worker owns it") || len(h.queue.released) != 0 {
			t.Fatalf("log %s, released %v", logs, h.queue.released)
		}
		h = newProbeHarness(t)
		h.queue.finishErr = errors.New("db down")
		if logs := h.tick(t); !strings.Contains(logs, "lease lapses") {
			t.Fatalf("log %s", logs)
		}
	})
}

// The worker probes as itself, on the deployment's probe settings and
// keyring.
// noDatabases stands in for the host's databases where none is read.
type noDatabases struct{ ports.Databases }

func TestNewProber(t *testing.T) {
	cfg := config.Defaults()
	cfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: houseKey, Models: config.AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
	}}
	task, err := New(Deps{Cfg: &cfg, DB: foundrystore.Unimplemented{}, Databases: noDatabases{},
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = task.closeFn() })
	p := task.prober
	if p.owner != task.workerID || p.cfg != cfg.Foundry.AI.Probe || task.probeEvery != 2*time.Second || p.deployment.Default == nil ||
		p.deployment.Default.Key.Reveal() != houseKey {
		t.Fatalf("prober = %+v, every %s", p, task.probeEvery)
	}
}

// A custom OpenAI-compatible server is probed on its endpoint, with its key
// if it was saved with one and none otherwise, and what the probe found is
// recorded as for any kind: a failed check's failure and detail, and the
// reasoning knob a model refused.
func TestProbeCompatible(t *testing.T) {
	const gatewayKey = "sk-gw-sentinel-probe-0123456789abcdef"
	server := func(t *testing.T, h *probeHarness, key string) {
		t.Helper()
		settings, _ := json.Marshal(ai.Settings{
			Provider: ai.Compatible{BaseURL: "https://llm.example.com/v1", API: ai.CompatChatCompletions, MaxCallsInFlight: 4},
			Models: ai.Roles{
				Draft: ai.Model{Name: "llama-4-maverick", Price: ai.PriceRates{Input: 270, Output: 850}}, Ask: ai.Model{Name: "llama-4-maverick", Price: ai.PriceFree{}},
				Extract: ai.Model{Name: "qwen3-32b", Price: ai.PriceFree{}}, Verify: ai.Model{Name: "qwen3-32b", Price: ai.PriceFree{}},
			},
		})
		c := &model.FoundryAIConfig{ID: "cfg-c", WorkspaceID: "ws-1", Kind: "openai_compatible", Settings: settings}
		if key != "" {
			var err error
			if c.Secret, err = ai.SealKey(h.keys, c.ID, ontologyagent.NewModelKey(key)); err != nil {
				t.Fatal(err)
			}
		}
		h.queue.waiting = c
		h.agent.info.Providers = []string{"anthropic", "openai_compatible"}
	}
	const endpoint = `"endpoint":{"provider":{"openai_compatible":{"base_url":"https://llm.example.com/v1","api":"chat_completions"}},` +
		`"call_timeout_secs":600,"max_calls_in_flight":4}`

	t.Run("probes with its key or none", func(t *testing.T) {
		for key, want := range map[string]string{"": "[[]]", gatewayKey: "[[" + gatewayKey + "]]"} {
			h := newProbeHarness(t)
			server(t, h, key)
			logs := h.tick(t)
			if len(h.queue.finished) != 1 || h.queue.finished[0].Failure != nil || fmt.Sprint(h.agent.probeKeys) != want ||
				!strings.Contains(h.agent.probes[0], endpoint) {
				t.Fatalf("key %q: finished %v, keys %v, probe %v", key, h.queue.finished, h.agent.probeKeys, h.agent.probes)
			}
			if err := contracttest.Validate(t, contracttest.Schema(t, "probe_request.json"), []byte(h.agent.probes[0])); err != nil {
				t.Fatalf("probe request %s: %v", h.agent.probes[0], err)
			}
			if stored := h.agent.probes[0] + string(h.queue.finished[0].Report) + logs; strings.Contains(stored, "sentinel") {
				t.Fatal("the key left its metadata")
			}
		}
	})

	// A draft model that took its tools only without reasoning passes so, and
	// runs so; a server that later resolves inward fails the setting.
	t.Run("records what the probe found", func(t *testing.T) {
		h := newProbeHarness(t)
		server(t, h, "")
		h.agent.probe = func(req string) (string, error) {
			out, err := probeResult("", "", "")(req)
			return strings.Replace(out, `{"role":"draft","outcome":{"passed":{"seconds":1.5,"reasoning":true}}}`,
				`{"role":"draft","outcome":{"passed":{"seconds":4,"reasoning":false}}}`, 1), err
		}
		h.tick(t)
		report := string(h.queue.finished[0].Report)
		if h.queue.finished[0].Failure != nil || !strings.Contains(report, `{"role":"draft","passed":true,"reasoning":false,"seconds":4}`) ||
			!strings.Contains(report, `{"role":"ask","passed":true,"reasoning":true,"seconds":1.5}`) {
			t.Fatalf("finished %+v, report %s", h.queue.finished, report)
		}
		for failing, f := range map[string]model.FoundryAIProbeFailure{
			"draft":   {Failure: "private_host", Detail: "none"},
			"ask":     {Failure: "unsupported", Detail: "tools_rejected"},
			"extract": {Failure: "unsupported", Detail: "schema_rejected"},
			"title":   {Failure: "auth", Detail: "none"},
		} {
			h := newProbeHarness(t)
			server(t, h, gatewayKey)
			h.agent.probe = probeResult(failing, f.Failure, f.Detail)
			h.tick(t)
			if len(h.queue.finished) != 1 || h.queue.finished[0].Failure == nil || *h.queue.finished[0].Failure != f {
				t.Fatalf("%s failing: finished %+v", failing, h.queue.finished)
			}
		}
	})

	// An agent that doesn't call the kind can't probe it: the claim is given
	// back, its attempt counted.
	t.Run("gives back a probe the agent can't run", func(t *testing.T) {
		h := newProbeHarness(t)
		server(t, h, "")
		h.agent.info.Providers = []string{"anthropic", "openai", "azure_openai"}
		if logs := h.tick(t); len(h.queue.finished) != 0 || fmt.Sprint(h.queue.released) != "[cfg-c w13]" || !strings.Contains(logs, "openai_compatible") {
			t.Fatalf("finished %v, released %v, log %s", h.queue.finished, h.queue.released, logs)
		}
	})
}
