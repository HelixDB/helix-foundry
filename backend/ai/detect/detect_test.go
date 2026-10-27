package detect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
)

// model is a model a fake Ollama has pulled, as its API describes it.
type model struct {
	name      string
	size      int64
	paramSize string
	params    int64
	context   int
	// caps nil omits capabilities, as a server too old to report them does.
	caps     []string
	remote   string
	requires string
	// showFails answers /api/show with a 500.
	showFails bool
}

var (
	chat = []string{"completion", "tools"}
	// A tool model with drafting's window, and others Foundry can't run on.
	qwen8b   = model{name: "qwen3:8b", size: 5_200_000_000, paramSize: "8.2B", params: 8_190_735_360, context: 40960 * 4, caps: append(chat, "thinking")}
	llama1b  = model{name: "llama3.2:1b", size: 1_300_000_000, paramSize: "1.2B", params: 1_235_814_432, context: 131072, caps: chat}
	tiny     = model{name: "qwen2.5:0.5b", size: 397_000_000, paramSize: "494.03M", context: 32768, caps: chat}
	embed    = model{name: "embeddinggemma:latest", size: 621_000_000, paramSize: "307.58M", context: 2048, caps: []string{"embedding"}}
	notools  = model{name: "gemma3:4b", size: 3_300_000_000, paramSize: "4.3B", context: 131072, caps: []string{"completion", "vision"}}
	cloud    = model{name: "gpt-oss:120b-cloud", size: 384, paramSize: "116.8B", remote: "https://ollama.com:443", caps: chat}
	newer    = model{name: "qwen3.5:4b", size: 3_400_000_000, paramSize: "4.7B", context: 262144, caps: chat, requires: "0.50.0"}
	unread   = model{name: "broken:1b", size: 1_000_000_000, paramSize: "1B", showFails: true}
	oldModel = model{name: "qwen2.5:7b", size: 4_700_000_000, paramSize: "7.6B", context: 32768}
)

// ollama is a fake Ollama server of version with models; it counts the
// requests it answers.
type ollama struct {
	*httptest.Server
	calls atomic.Int32
}

func fakeOllama(t *testing.T, version string, models ...model) *ollama {
	t.Helper()
	return pulling(t, version, func() []model { return models })
}

// pulling is a fake Ollama whose models are pulled() at each request.
func pulling(t *testing.T, version string, pulled func() []model) *ollama {
	t.Helper()
	o := &ollama{}
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.calls.Add(1)
		models := pulled()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"version": version})
		case "/api/tags":
			var list []map[string]any
			for _, m := range models {
				entry := map[string]any{"name": m.name, "model": m.name, "size": m.size, "digest": "sha256:0",
					"details": map[string]any{"format": "gguf", "parameter_size": m.paramSize}}
				if m.remote != "" {
					entry["remote_host"] = m.remote
				}
				list = append(list, entry)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": list})
		case "/api/show":
			var req struct{ Model string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			for _, m := range models {
				if m.name != req.Model {
					continue
				}
				if m.showFails {
					http.Error(w, `{"error":"unknown model architecture"}`, http.StatusInternalServerError)
					return
				}
				info := map[string]any{"general.architecture": "x"}
				if m.params > 0 {
					info["general.parameter_count"] = m.params
				}
				if m.context > 0 {
					info["x.context_length"] = m.context
				}
				show := map[string]any{"details": map[string]any{"parameter_size": m.paramSize}, "model_info": info}
				if m.caps != nil {
					show["capabilities"] = m.caps
				}
				if m.remote != "" {
					show["remote_host"] = m.remote
				}
				if m.requires != "" {
					show["requires"] = m.requires
				}
				_ = json.NewEncoder(w).Encode(show)
				return
			}
			http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(o.Close)
	return o
}

// nowhere is an address nothing listens on.
func nowhere(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

// silent is a server that accepts and never answers.
func silent(t *testing.T) string {
	t.Helper()
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); s.Close() })
	return s.URL
}

// keys is a fake Anthropic and OpenAI that accept only "good".
func keys(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if r.URL.Path != "/v1/models" || key != "good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// detector looks only at the servers given, with a log in log.
func detector(t *testing.T, ollamas []string, studio string, log *bytes.Buffer) *Detector {
	t.Helper()
	if log == nil {
		log = &bytes.Buffer{}
	}
	return &Detector{
		Ollamas: ollamas, LMStudio: studio, AnthropicAPI: keys(t), OpenAIAPI: keys(t),
		Live: &config.LiveAIDefault{}, Logger: slog.New(slog.NewTextHandler(log, nil)),
		Now: func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) },
	}
}

func every(name string) config.AIModels {
	return config.AIModels{Draft: name, Ask: name, Extract: name, Verify: name}
}

// A key where foundry was started comes first: Claude's before OpenAI's,
// on the models each kind suggests. A key the provider refuses is still
// used, and the summary says so; the log never holds it.
func TestDetectKeys(t *testing.T) {
	claude, _ := ai.Suggested(ai.KindAnthropic)
	openai, _ := ai.Suggested(ai.KindOpenAI)
	ollama := fakeOllama(t, "0.40.10", qwen8b)
	for name, tc := range map[string]struct {
		anthropic, openai string
		want              config.AIDefault
		summary           string
	}{
		"Claude": {"good", "", config.AIDefault{Provider: "anthropic", APIKey: "good", Models: config.AIModels{
			Draft: claude.Draft.Name, Ask: claude.Ask.Name, Extract: claude.Extract.Name, Verify: claude.Verify.Name}},
			"Using Claude: Foundry was started with an ANTHROPIC_API_KEY."},
		"Claude before OpenAI": {"good", "good", config.AIDefault{Provider: "anthropic", APIKey: "good", Models: config.AIModels{
			Draft: claude.Draft.Name, Ask: claude.Ask.Name, Extract: claude.Extract.Name, Verify: claude.Verify.Name}},
			"Using Claude: Foundry was started with an ANTHROPIC_API_KEY."},
		"a refused Claude key": {"sk-ant-dummy", "", config.AIDefault{Provider: "anthropic", APIKey: "sk-ant-dummy", Models: config.AIModels{
			Draft: claude.Draft.Name, Ask: claude.Ask.Name, Extract: claude.Extract.Name, Verify: claude.Verify.Name}},
			"Using Claude: Foundry was started with an ANTHROPIC_API_KEY, but Anthropic refused it. Set a valid key and run docker compose up -d again, or choose a provider below."},
		"OpenAI": {" ", " good\n", config.AIDefault{Provider: "openai", APIKey: "good", Models: config.AIModels{
			Draft: openai.Draft.Name, Ask: openai.Ask.Name, Extract: openai.Extract.Name, Verify: openai.Verify.Name}},
			"Using OpenAI: Foundry was started with an OPENAI_API_KEY."},
		"a refused OpenAI key": {"", "sk-dummy", config.AIDefault{Provider: "openai", APIKey: "sk-dummy", Models: config.AIModels{
			Draft: openai.Draft.Name, Ask: openai.Ask.Name, Extract: openai.Extract.Name, Verify: openai.Verify.Name}},
			"Using OpenAI: Foundry was started with an OPENAI_API_KEY, but OpenAI refused it. Set a valid key and run docker compose up -d again, or choose a provider below."},
	} {
		t.Run(name, func(t *testing.T) {
			var log bytes.Buffer
			d := detector(t, []string{ollama.URL}, nowhere(t), &log)
			d.AnthropicKey, d.OpenAIKey = tc.anthropic, tc.openai
			got := d.Detect(context.Background())
			if got.Default != tc.want || got.Summary != tc.summary || !got.At.Equal(d.Now()) {
				t.Fatalf("found %+v", got)
			}
			if d.Live.Load().Default != tc.want {
				t.Fatalf("stored %+v", d.Live.Load())
			}
			if strings.Contains(log.String(), tc.want.APIKey) || !strings.Contains(log.String(), "provider="+tc.want.Provider) {
				t.Fatalf("log = %s", log.String())
			}
		})
	}
	// A provider that can't be reached hasn't refused the key.
	t.Run("an unreachable provider", func(t *testing.T) {
		d := detector(t, []string{ollama.URL}, nowhere(t), nil)
		d.AnthropicKey, d.AnthropicAPI = "sk-ant-any", nowhere(t)
		if got := d.Detect(context.Background()); got.Summary != "Using Claude: Foundry was started with an ANTHROPIC_API_KEY." {
			t.Fatalf("found %+v", got)
		}
	})
}

// Without a key, Ollama: the most capable model that calls tools and loads
// drafting's window, on every task, at the server's address.
func TestDetectOllama(t *testing.T) {
	o := fakeOllama(t, "0.40.10", tiny, embed, llama1b, notools, cloud, qwen8b, newer, unread)
	d := detector(t, []string{nowhere(t), o.URL}, nowhere(t), nil)
	got := d.Detect(context.Background())
	want := config.AIDefault{Provider: "ollama", BaseURL: o.URL, Models: every("qwen3:8b")}
	if got.Default != want {
		t.Fatalf("default = %+v", got.Default)
	}
	if !strings.HasPrefix(got.Summary, "Using qwen3:8b from the Ollama server at 127.0.0.1:") ||
		!strings.HasSuffix(got.Summary, ": the most capable model it has that calls tools.") {
		t.Fatalf("summary = %q", got.Summary)
	}

	// The card lists them, usable first and most capable first, each other
	// with why Foundry can't run on it.
	s := d.Ollama(context.Background(), o.URL+"/")
	if s.Version != "0.40.10" || s.Problem != "" {
		t.Fatalf("server = %+v", s)
	}
	var lines []string
	for _, m := range s.Models {
		lines = append(lines, m.Name+" "+m.Unusable)
	}
	wantLines := []string{
		"qwen3:8b ",
		"llama3.2:1b ",
		"broken:1b Ollama couldn't read it.",
		"embeddinggemma:latest Not a chat model.",
		"gemma3:4b Calls no tools.",
		"gpt-oss:120b-cloud Runs on ollama.com, so what it is sent would leave this computer.",
		"qwen2.5:0.5b Its context window, 32,768 tokens, is under the 65,536 drafting needs.",
		"qwen3.5:4b Needs Ollama 0.50.0 or newer.",
	}
	if strings.Join(lines, "\n") != strings.Join(wantLines, "\n") {
		t.Fatalf("models:\n%s", strings.Join(lines, "\n"))
	}
	if m := s.Models[0]; m.Size != qwen8b.size || m.ParameterSize != "8.2B" || m.Parameters != qwen8b.params || m.ContextLength != qwen8b.context {
		t.Fatalf("qwen3:8b = %+v", m)
	}
	// A newer server runs what an older one couldn't.
	if s := d.Ollama(context.Background(), fakeOllama(t, "0.50.1", newer).URL); s.Models[0].Unusable != "" {
		t.Fatalf("on 0.50.1: %+v", s.Models[0])
	}
}

// The bundled Ollama comes before this computer's, which is looked at when
// the bundled one has nothing Foundry can run (it may be pulling still).
func TestDetectOllamaOrder(t *testing.T) {
	bundled, host := fakeOllama(t, "0.40.10", llama1b), fakeOllama(t, "0.40.10", qwen8b)
	if got := detector(t, []string{bundled.URL, host.URL}, nowhere(t), nil).Detect(context.Background()); got.Default.BaseURL != bundled.URL {
		t.Fatalf("found %+v", got)
	}
	empty := fakeOllama(t, "0.40.10")
	got := detector(t, []string{empty.URL, host.URL}, nowhere(t), nil).Detect(context.Background())
	if got.Default.BaseURL != host.URL || got.Default.Models != every("qwen3:8b") {
		t.Fatalf("found %+v", got)
	}
}

// Nothing found: the summary says what was looked for, and what each server
// that answered lacks.
func TestDetectNone(t *testing.T) {
	empty, unfit, old := fakeOllama(t, "0.40.10"), fakeOllama(t, "0.40.10", tiny, embed), fakeOllama(t, "0.5.7", oldModel)
	studio := fakeStudio(t, map[string]string{"/api/v0/models": `{"data":[{"id":"nomic-embed","type":"embeddings","state":"loaded"}]}`})
	var log bytes.Buffer
	d := detector(t, []string{empty.URL, unfit.URL, old.URL, nowhere(t)}, studio+"/v1", &log)
	got := d.Detect(context.Background())
	host := func(o *ollama) string { return strings.TrimPrefix(o.URL, "http://") }
	want := "No AI provider was found. Foundry looks for an ANTHROPIC_API_KEY or OPENAI_API_KEY where it was started, a model that calls tools in Ollama, and a model LM Studio serves on this computer." +
		" The Ollama server at " + host(empty) + " has no models: pull one that calls tools, like qwen3.5:4b." +
		" The Ollama server at " + host(unfit) + " has no model Foundry can run: pull one that calls tools, like qwen3.5:4b." +
		" The Ollama server at " + host(old) + " (0.5.7) is too old to say which models call tools: update it from ollama.com." +
		" LM Studio on this computer serves no model: load one."
	if got.Default != (config.AIDefault{}) || got.Summary != want {
		t.Fatalf("found %+v\nwant %q", got, want)
	}
	if d.Live.Load().Summary != want || !strings.Contains(log.String(), "ai default") {
		t.Fatalf("stored %+v, log %s", d.Live.Load(), log.String())
	}
	// Nothing at all answering says only what was looked for.
	if got := detector(t, []string{nowhere(t)}, nowhere(t), nil).Detect(context.Background()); !strings.HasSuffix(got.Summary, "on this computer.") ||
		strings.Count(got.Summary, ".") != 2 {
		t.Fatalf("summary = %q", got.Summary)
	}
}

// A server too old to report capabilities lists its models with nothing
// said of them, and is no default.
func TestOllamaTooOld(t *testing.T) {
	o := fakeOllama(t, "0.5.7", oldModel, cloud, tiny)
	s := detector(t, nil, "", nil).Ollama(context.Background(), o.URL)
	if s.Problem != "This Ollama (0.5.7) is too old to say which models call tools. Update it from ollama.com." || len(s.Models) != 3 {
		t.Fatalf("server = %+v", s)
	}
	for _, m := range s.Models {
		if (m.Name == cloud.name) != (m.Unusable != "") {
			t.Fatalf("%s: %q", m.Name, m.Unusable)
		}
	}
	if _, ok := s.Best(); ok {
		t.Fatal("chose a model on a server too old to say")
	}
}

// What doesn't answer, or isn't Ollama, says so, and how to fix it, within
// the call's timeout: never a hang.
func TestOllamaProblems(t *testing.T) {
	d := detector(t, nil, "", nil)
	notOllama := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notOllama.Close)
	brokenTags := fakeStudio(t, map[string]string{"/api/version": `{"version":"0.40.10"}`})
	for name, tc := range map[string]struct{ base, want string }{
		"the bundled one": {BundledOllama, "The bundled Ollama isn't running. Start it with docker compose --profile ollama up -d."},
		"this computer's": {HostOllama, "Ollama on this computer didn't answer. Start it; on Linux, with OLLAMA_HOST=172.17.0.1, so Docker can reach it."},
		"another":         {"http://gpu.invalid:11434", "Nothing answered at http://gpu.invalid:11434. Check the address and that Ollama runs there."},
		"not Ollama":      {notOllama.URL, "Something answered at " + notOllama.URL + ", but not Ollama's API. Check the address."},
		"no list":         {brokenTags, "The Ollama server at " + strings.TrimPrefix(brokenTags, "http://") + " (0.40.10) answered, but couldn't list its models. Check that it runs."},
	} {
		t.Run(name, func(t *testing.T) {
			// The bundled and this computer's addresses, wherever they are,
			// answer nothing here: a client that dials nowhere.
			d.HTTP = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if strings.HasPrefix(tc.base, BundledOllama) || strings.HasPrefix(tc.base, HostOllama) || strings.Contains(tc.base, ".invalid") {
					return nil, errors.New("connection refused")
				}
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}}}
			if s := d.Ollama(context.Background(), tc.base); s.Problem != tc.want || len(s.Models) != 0 {
				t.Fatalf("server = %+v", s)
			}
		})
	}
	t.Run("a server that never answers", func(t *testing.T) {
		d.HTTP = nil
		start := time.Now()
		s := d.Ollama(context.Background(), silent(t))
		if !strings.HasPrefix(s.Problem, "Nothing answered at") || time.Since(start) > callTimeout+time.Second {
			t.Fatalf("server = %+v after %s", s, time.Since(start))
		}
	})
}

// fakeStudio answers each path with its JSON; others 404.
func fakeStudio(t *testing.T, paths map[string]string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := paths[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// LM Studio, after Ollama: the model it has loaded, else the first chat
// model it has, never an embedding model; from its OpenAI-compatible list
// on a version without its own API.
func TestDetectLMStudio(t *testing.T) {
	for name, tc := range map[string]struct {
		paths map[string]string
		want  string
	}{
		"a loaded model": {map[string]string{"/api/v0/models": `{"data":[
			{"id":"text-embedding-nomic","type":"embeddings","state":"loaded"},
			{"id":"qwen2.5-7b-instruct","type":"llm","state":"not-loaded"},
			{"id":"gemma-3-12b","type":"vlm","state":"loaded"}]}`}, "gemma-3-12b"},
		"none loaded":        {map[string]string{"/api/v0/models": `{"data":[{"id":"qwen2.5-7b-instruct","type":"llm","state":"not-loaded"}]}`}, "qwen2.5-7b-instruct"},
		"an older LM Studio": {map[string]string{"/v1/models": `{"data":[{"id":"text-embedding-nomic"},{"id":"mistral-7b"}]}`}, "mistral-7b"},
	} {
		t.Run(name, func(t *testing.T) {
			base := fakeStudio(t, tc.paths) + "/v1"
			got := detector(t, []string{nowhere(t)}, base, nil).Detect(context.Background())
			want := config.AIDefault{Provider: "openai_compatible", BaseURL: base, API: "chat_completions", Models: every(tc.want)}
			if got.Default != want || got.Summary != "Using "+tc.want+" from LM Studio on this computer." {
				t.Fatalf("found %+v", got)
			}
		})
	}
	// Ollama with a model comes first.
	o := fakeOllama(t, "0.40.10", llama1b)
	studio := fakeStudio(t, map[string]string{"/v1/models": `{"data":[{"id":"mistral-7b"}]}`}) + "/v1"
	if got := detector(t, []string{o.URL}, studio, nil).Detect(context.Background()); got.Default.Provider != "ollama" {
		t.Fatalf("found %+v", got)
	}
}

// A default the deployment refuses is none: nothing is used that would fail
// every call.
func TestDetectChecked(t *testing.T) {
	o := fakeOllama(t, "0.40.10", llama1b)
	d := detector(t, []string{o.URL}, nowhere(t), nil)
	d.Check = func(config.AIDefault) error { return errors.New("refused") }
	if got := d.Detect(context.Background()); got.Default.Provider != "" || got.Summary != "No AI provider is set up: the one found couldn't be used. Choose one below." {
		t.Fatalf("found %+v", got)
	}
	// The install's own check passes what it finds.
	cfg := config.Defaults()
	cfg.Host.Local, cfg.Foundry.AI.AllowPrivateHosts = true, true
	cfg.Foundry.AI.Kinds = []string{"ollama", "anthropic", "openai", "openai_compatible"}
	cfg.Host.DetectedAI = d.Live
	dep, err := ai.NewDeployment(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.Check = dep.CheckDefault
	if got := d.Detect(context.Background()); got.Default.Provider != "ollama" {
		t.Fatalf("found %+v", got)
	}
	if cur := dep.Current(); cur == nil || cur.Settings.Provider != (ai.Ollama{BaseURL: o.URL, ContextTokens: ai.DefaultContextTokens}) {
		t.Fatalf("current = %+v", cur)
	}
}

// Looks that overlap share one, and each stores what it finds: a model
// pulled since the last look is found by the next.
func TestDetectAgain(t *testing.T) {
	var models []model
	var mu sync.Mutex
	o := pulling(t, "0.40.10", func() []model {
		mu.Lock()
		defer mu.Unlock()
		return models
	})
	d := detector(t, []string{o.URL}, nowhere(t), nil)
	if got := d.Detect(context.Background()); got.Default.Provider != "" {
		t.Fatalf("found %+v", got)
	}
	mu.Lock()
	models = []model{llama1b}
	mu.Unlock()
	before := o.calls.Load()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if got := d.Detect(context.Background()); got.Default.Models != every("llama3.2:1b") {
				t.Errorf("found %+v", got)
			}
		})
	}
	wg.Wait()
	if d.Live.Load().Default.Models != every("llama3.2:1b") {
		t.Fatalf("stored %+v", d.Live.Load())
	}
	// One look is three calls (version, tags, the model's details); four
	// overlapping callers make one or a few looks, never one each.
	if calls := o.calls.Load() - before; calls < 3 || calls > 9 {
		t.Fatalf("%d calls", calls)
	}
	// A caller that gives up doesn't end the look it started.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := d.Detect(ctx); got.Default.Provider != "ollama" {
		t.Fatalf("found %+v with a cancelled caller", got)
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string]int64{"4.0B": 4_000_000_000, "600M": 600_000_000, "1.5t": 1_500_000_000_000, "8K": 8000, "": 0, "8x7B": 0, "7": 0, "-1B": 0} {
		if got := parameters(in); got != want {
			t.Errorf("parameters(%q) = %d, want %d", in, got, want)
		}
	}
	for _, tc := range []struct {
		version, want string
		older         bool
	}{
		{"0.12.3", "0.12.10", true}, {"0.40.10", "0.17.1", false}, {"v0.6.4-rc1", "0.6.4", false}, {"0.6", "0.6.1", true},
		{"0.0.0", "0.1.0", false}, {"dev", "0.1.0", false}, {"0.1.0", "soon", false}, {"1.2.x", "1.3.0", false},
	} {
		if got := older(tc.version, tc.want); got != tc.older {
			t.Errorf("older(%q, %q) = %v", tc.version, tc.want, got)
		}
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 65536: "65,536", 1048576: "1,048,576"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q", n, got)
		}
	}
	for base, want := range map[string]string{BundledOllama: "the bundled Ollama", HostOllama + "/": "Ollama on this computer",
		"http://gpu.lan:11434": "the Ollama server at gpu.lan:11434", "::": "the Ollama server"} {
		if got := serverName(base); got != want {
			t.Errorf("serverName(%q) = %q", base, got)
		}
	}
	if capital("") != "" || capital("the x") != "The x" {
		t.Error("capital")
	}
}
