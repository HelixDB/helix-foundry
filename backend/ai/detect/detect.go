// Package detect finds a local install's default model provider on its
// machine, for an install whose configuration names none (FOUNDRY_AI_PROVIDER
// unset): the first of
//
//  1. an ANTHROPIC_API_KEY where foundry was started: Claude, on the models
//     the kind suggests (ai.Suggested);
//  2. an OPENAI_API_KEY: OpenAI, likewise;
//  3. an Ollama it reaches, the bundled one (the compose file's ollama
//     profile) and then this computer's, with a model that calls tools and
//     loads drafting's context window: the most capable such model, on
//     every task;
//  4. LM Studio on this computer serving a model: that model, on every task;
//
// else none, until an owner chooses a provider in Settings › AI models. Every
// look is bounded by short timeouts: a server that doesn't answer is passed
// over, never waited on. It also reads an Ollama server's models for the
// Ollama card (Detector.Ollama). Hosted Foundry runs none of this.
package detect

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sync/singleflight"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
)

// Where an install looks, from its containers: the bundled Ollama shares
// foundry's network namespace, and this computer is host.docker.internal.
const (
	BundledOllama = "http://127.0.0.1:11434"
	HostOllama    = "http://host.docker.internal:11434"
	LMStudio      = "http://host.docker.internal:1234/v1"
	AnthropicAPI  = "https://api.anthropic.com"
	OpenAIAPI     = "https://api.openai.com"
)

// SuggestedOllamaModel is the model the copy suggests pulling: the bundled
// Ollama's (FOUNDRY_OLLAMA_MODEL in local/compose.yaml).
const SuggestedOllamaModel = "qwen3.5:4b"

// Timeouts: a call to a server that may not be there, a model's details
// (Ollama reads its file), and a whole look.
const (
	callTimeout   = 2 * time.Second
	showTimeout   = 4 * time.Second
	detectTimeout = 15 * time.Second
	// maxModels is the most models of a server read in detail.
	maxModels = 64
	// maxBody is the most read of any answer.
	maxBody = 4 << 20
)

// Detector looks for a local install's default and stores what it finds in
// Live, which every resolve reads. The zero value of each field but Live
// and Check is the install's own: the URLs above, the keys unset, a client
// with no timeout of its own (each call has one).
type Detector struct {
	// AnthropicKey and OpenAIKey are the keys foundry was started with.
	AnthropicKey, OpenAIKey string
	// Ollamas are the Ollama servers looked at, in order; nil is the
	// bundled one, then this computer's.
	Ollamas []string
	// LMStudio is LM Studio's base URL, including /v1.
	LMStudio string
	// AnthropicAPI and OpenAIAPI are where a key is checked.
	AnthropicAPI, OpenAIAPI string
	HTTP                    *http.Client
	// Check checks a default as the deployment would read it
	// (ai.Deployment.CheckDefault); one it refuses is none.
	Check  func(config.AIDefault) error
	Live   *config.LiveAIDefault
	Logger *slog.Logger
	Now    func() time.Time

	looks singleflight.Group
}

func (d *Detector) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return http.DefaultClient
}

func (d *Detector) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

func (d *Detector) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func or(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// Detect looks again, stores what it finds and returns it. Calls that
// overlap share one look, which runs to its own deadline whatever happens
// to the caller that started it.
func (d *Detector) Detect(ctx context.Context) config.DetectedAIDefault {
	v, _, _ := d.looks.Do("detect", func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detectTimeout)
		defer cancel()
		found := d.detect(ctx)
		if found.Default.Provider != "" && d.Check != nil {
			if err := d.Check(found.Default); err != nil {
				d.logger().Error("ai default: found one the deployment refuses; none is used", "provider", found.Default.Provider, "err", err)
				found = config.DetectedAIDefault{Summary: "No AI provider is set up: the one found couldn't be used. Choose one below.", At: found.At}
			}
		}
		d.Live.Store(found)
		attrs := []any{"why", found.Summary}
		if p := found.Default.Provider; p != "" {
			attrs = append(attrs, "provider", p, "model", found.Default.Models.Draft)
		}
		if b := found.Default.BaseURL; b != "" {
			attrs = append(attrs, "base_url", b)
		}
		d.logger().Info("ai default", attrs...)
		return found, nil
	})
	return v.(config.DetectedAIDefault)
}

// detect is the first provider found, in the package's order.
func (d *Detector) detect(ctx context.Context) config.DetectedAIDefault {
	at := d.now()
	for _, k := range []struct {
		kind              ai.Kind
		key, name, api    string
		variable, refused string
	}{
		{ai.KindAnthropic, d.AnthropicKey, "Claude", or(d.AnthropicAPI, AnthropicAPI), "ANTHROPIC_API_KEY", "Anthropic"},
		{ai.KindOpenAI, d.OpenAIKey, "OpenAI", or(d.OpenAIAPI, OpenAIAPI), "OPENAI_API_KEY", "OpenAI"},
	} {
		key := strings.TrimSpace(k.key)
		if key == "" {
			continue
		}
		summary := fmt.Sprintf("Using %s: Foundry was started with an %s.", k.name, k.variable)
		if d.keyRefused(ctx, k.kind, k.api, key) {
			summary = fmt.Sprintf("Using %s: Foundry was started with an %s, but %s refused it. Set a valid key and run docker compose up -d again, or choose a provider below.",
				k.name, k.variable, k.refused)
		}
		suggested, _ := ai.Suggested(k.kind)
		return config.DetectedAIDefault{Default: config.AIDefault{
			Provider: string(k.kind), APIKey: key, Models: config.AIModels{
				Draft: suggested.Draft.Name, Ask: suggested.Ask.Name, Extract: suggested.Extract.Name, Verify: suggested.Verify.Name,
			},
		}, Summary: summary, At: at}
	}

	// Every server at once: the slowest bounds the look, not their sum.
	ollamas := d.Ollamas
	if ollamas == nil {
		ollamas = []string{BundledOllama, HostOllama}
	}
	servers := make([]Server, len(ollamas))
	var studio studioFound
	var wg sync.WaitGroup
	for i, base := range ollamas {
		wg.Go(func() { servers[i] = d.Ollama(ctx, base) })
	}
	wg.Go(func() { studio = d.lmStudio(ctx) })
	wg.Wait()

	var notes []string
	for i, s := range servers {
		where := serverName(ollamas[i])
		if m, ok := s.Best(); ok {
			return config.DetectedAIDefault{Default: config.AIDefault{
				Provider: string(ai.KindOllama), BaseURL: ollamas[i],
				Models: config.AIModels{Draft: m.Name, Ask: m.Name, Extract: m.Name, Verify: m.Name},
			}, Summary: fmt.Sprintf("Using %s from %s: the most capable model it has that calls tools.", m.Name, where), At: at}
		}
		switch {
		case s.Version == "":
			// Not running, or not there: nothing to say.
		case s.tooOld:
			notes = append(notes, fmt.Sprintf("%s (%s) is too old to say which models call tools: update it from ollama.com.", capital(where), s.Version))
		case s.Problem != "":
			notes = append(notes, s.Problem)
		case len(s.Models) == 0:
			notes = append(notes, fmt.Sprintf("%s has no models: pull one that calls tools, like %s.", capital(where), SuggestedOllamaModel))
		default:
			notes = append(notes, fmt.Sprintf("%s has no model Foundry can run: pull one that calls tools, like %s.", capital(where), SuggestedOllamaModel))
		}
	}
	if studio.model != "" {
		return config.DetectedAIDefault{Default: config.AIDefault{
			Provider: string(ai.KindCompatible), BaseURL: or(d.LMStudio, LMStudio), API: string(ai.CompatChatCompletions),
			Models: config.AIModels{Draft: studio.model, Ask: studio.model, Extract: studio.model, Verify: studio.model},
		}, Summary: fmt.Sprintf("Using %s from LM Studio on this computer.", studio.model), At: at}
	}
	if studio.answered {
		notes = append(notes, "LM Studio on this computer serves no model: load one.")
	}
	summary := "No AI provider was found. Foundry looks for an ANTHROPIC_API_KEY or OPENAI_API_KEY where it was started, a model that calls tools in Ollama, and a model LM Studio serves on this computer."
	return config.DetectedAIDefault{Summary: strings.Join(append([]string{summary}, notes...), " "), At: at}
}

// keyRefused reports whether the provider refuses key, by listing its
// models, which costs nothing. Anything but a refusal (no network, an
// outage) is not one: the key is used, and a call that fails says why.
func (d *Detector) keyRefused(ctx context.Context, kind ai.Kind, api, key string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(api, "/")+"/v1/models", nil)
	if err != nil {
		return false
	}
	if kind == ai.KindAnthropic {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
}

// serverName names an Ollama server for the owner.
func serverName(base string) string {
	switch strings.TrimSuffix(base, "/") {
	case BundledOllama:
		return "the bundled Ollama"
	case HostOllama:
		return "Ollama on this computer"
	}
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return "the Ollama server at " + u.Host
	}
	return "the Ollama server"
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Server is what an Ollama server has, as Settings › AI models shows it.
type Server struct {
	// Version is its version; "" when it didn't answer as Ollama.
	Version string
	// Problem says, for the owner, why its models can't be listed or told
	// apart, and how to fix it; "" when they can.
	Problem string
	// Models are usable ones first, most capable first, then the rest by
	// name.
	Models []Model
	// tooOld is a server too old to report its models' capabilities.
	tooOld bool
}

// Model is a model a server has pulled.
type Model struct {
	Name string
	// Size is its size on disk, in bytes.
	Size int64
	// ParameterSize is the server's, like "4.0B".
	ParameterSize string
	// Parameters is its parameter count, 0 when the server doesn't say.
	Parameters int64
	// ContextLength is the most context it loads, 0 when unknown.
	ContextLength int
	// Unusable says, for the owner, why Foundry can't run on it; "" when it
	// can, or when the server is too old to say.
	Unusable string
}

// Best is the model a default would run on: the first usable one, unless
// the server can't say which are.
func (s Server) Best() (Model, bool) {
	if s.Problem != "" {
		return Model{}, false
	}
	for _, m := range s.Models {
		if m.Unusable == "" {
			return m, true
		}
	}
	return Model{}, false
}

// Ollama's answers, as far as they are read.
type (
	ollamaVersion struct {
		Version string `json:"version"`
	}
	ollamaTags struct {
		Models []struct {
			Name       string `json:"name"`
			Size       int64  `json:"size"`
			RemoteHost string `json:"remote_host"`
			Details    struct {
				ParameterSize string `json:"parameter_size"`
			} `json:"details"`
		} `json:"models"`
	}
	ollamaShow struct {
		// Capabilities is absent from a server too old to report them.
		Capabilities *[]string      `json:"capabilities"`
		ModelInfo    map[string]any `json:"model_info"`
		RemoteHost   string         `json:"remote_host"`
		// Requires is the oldest Ollama that runs the model, where it says.
		Requires string `json:"requires"`
	}
)

// cloudTag is an Ollama cloud model's tag, as the site reads it.
var cloudTag = regexp.MustCompile(`(?i):(?:.*-)?cloud$`)

// Ollama reads the server at base: its version, its models, and of each
// whether Foundry can run on it (it calls tools, chats, is local and loads
// at least ai.MinContextTokens). A server that doesn't answer, isn't Ollama
// or is too old to say which models call tools is a Problem.
func (d *Detector) Ollama(ctx context.Context, base string) Server {
	base = strings.TrimSuffix(base, "/")
	where := serverName(base)
	var v ollamaVersion
	switch status, err := d.call(ctx, callTimeout, http.MethodGet, base+"/api/version", nil, &v); {
	case errors.Is(err, errNoAnswer):
		return Server{Problem: unreachable(base, where)}
	case err != nil || status != http.StatusOK || v.Version == "":
		return Server{Problem: fmt.Sprintf("Something answered at %s, but not Ollama's API. Check the address.", base)}
	}
	var tags ollamaTags
	if status, err := d.call(ctx, callTimeout, http.MethodGet, base+"/api/tags", nil, &tags); err != nil || status != http.StatusOK {
		return Server{Version: v.Version, Problem: fmt.Sprintf("%s (%s) answered, but couldn't list its models. Check that it runs.", capital(where), v.Version)}
	}
	listed := tags.Models
	if len(listed) > maxModels {
		listed = listed[:maxModels]
	}
	models := make([]Model, len(listed))
	tooOld := make([]bool, len(listed))
	// A few at a time: each reads the model's file.
	limit := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, t := range listed {
		models[i] = Model{Name: t.Name, Size: t.Size, ParameterSize: t.Details.ParameterSize, Parameters: parameters(t.Details.ParameterSize)}
		if t.RemoteHost != "" || cloudTag.MatchString(t.Name) {
			models[i].Unusable = "Runs on ollama.com, so what it is sent would leave this computer."
			continue
		}
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			tooOld[i] = d.judge(ctx, base, v.Version, &models[i])
		})
	}
	wg.Wait()
	if slices.Contains(tooOld, true) {
		for i := range models {
			if !cloudTag.MatchString(models[i].Name) && listed[i].RemoteHost == "" {
				models[i].Unusable = ""
			}
		}
		slices.SortFunc(models, func(a, b Model) int { return strings.Compare(a.Name, b.Name) })
		return Server{Version: v.Version, Models: models, tooOld: true,
			Problem: fmt.Sprintf("This Ollama (%s) is too old to say which models call tools. Update it from ollama.com.", v.Version)}
	}
	slices.SortStableFunc(models, func(a, b Model) int {
		if ua, ub := a.Unusable == "", b.Unusable == ""; ua != ub {
			if ua {
				return -1
			}
			return 1
		}
		if a.Unusable == "" {
			if c := cmp.Compare(b.Parameters, a.Parameters); c != 0 {
				return c
			}
			if c := cmp.Compare(b.Size, a.Size); c != 0 {
				return c
			}
		}
		return strings.Compare(a.Name, b.Name)
	})
	return Server{Version: v.Version, Models: models}
}

// unreachable is a server that didn't answer, with how to start it.
func unreachable(base, where string) string {
	switch base {
	case BundledOllama:
		return "The bundled Ollama isn't running. Start it with docker compose --profile ollama up -d."
	case HostOllama:
		return "Ollama on this computer didn't answer. Start it; on Linux, with OLLAMA_HOST=172.17.0.1, so Docker can reach it."
	}
	return fmt.Sprintf("Nothing answered at %s. Check the address and that Ollama runs there.", base)
}

// judge reads m's details and says why Foundry can't run on it, if it
// can't; it reports a server too old to list capabilities.
func (d *Detector) judge(ctx context.Context, base, version string, m *Model) (tooOld bool) {
	var show ollamaShow
	body, _ := json.Marshal(map[string]string{"model": m.Name})
	if status, err := d.call(ctx, showTimeout, http.MethodPost, base+"/api/show", body, &show); err != nil || status != http.StatusOK {
		m.Unusable = "Ollama couldn't read it."
		return false
	}
	for key, value := range show.ModelInfo {
		n, ok := value.(float64)
		switch {
		case !ok:
		case key == "general.parameter_count":
			m.Parameters = int64(n)
		case strings.HasSuffix(key, ".context_length"):
			m.ContextLength = int(min(n, math.MaxInt32))
		}
	}
	switch {
	case show.RemoteHost != "":
		m.Unusable = "Runs on ollama.com, so what it is sent would leave this computer."
	case show.Requires != "" && older(version, show.Requires):
		m.Unusable = fmt.Sprintf("Needs Ollama %s or newer.", show.Requires)
	case show.Capabilities == nil:
		return true
	case !slices.Contains(*show.Capabilities, "completion"):
		m.Unusable = "Not a chat model."
	case !slices.Contains(*show.Capabilities, "tools"):
		m.Unusable = "Calls no tools."
	case m.ContextLength > 0 && m.ContextLength < ai.MinContextTokens:
		m.Unusable = fmt.Sprintf("Its context window, %s tokens, is under the %s drafting needs.",
			thousands(m.ContextLength), thousands(ai.MinContextTokens))
	}
	return false
}

// parameters reads a parameter size like "4.0B" or "600M"; 0 when it can't.
func parameters(size string) int64 {
	size = strings.TrimSpace(strings.ToUpper(size))
	if size == "" {
		return 0
	}
	scale := map[byte]float64{'K': 1e3, 'M': 1e6, 'B': 1e9, 'T': 1e12}[size[len(size)-1]]
	if scale == 0 {
		return 0
	}
	n, err := strconv.ParseFloat(size[:len(size)-1], 64)
	if err != nil || n < 0 {
		return 0
	}
	return int64(n * scale)
}

// older reports whether version is older than want, both like "0.12.3"; a
// version either can't read is not, nor a build from source (0.0.0).
func older(version, want string) bool {
	a, okA := semver(version)
	b, okB := semver(want)
	return okA && okB && slices.Compare(a, []int{0, 0, 0}) != 0 && slices.Compare(a, b) < 0
}

func semver(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexFunc(v, func(r rune) bool { return r != '.' && !unicode.IsDigit(r) }); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return nil, false
	}
	out := make([]int, 3)
	for i, p := range parts[:min(3, len(parts))] {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

// thousands writes n with commas: 65,536.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// studioFound is what LM Studio serves: whether it answered, and the model
// a default would run on.
type studioFound struct {
	answered bool
	model    string
}

// lmStudio is the model LM Studio serves: a loaded one first, never an
// embedding model. Its own API (/api/v0) says which are loaded and of what
// type; the OpenAI-compatible list, for a version without it, only names
// them.
func (d *Detector) lmStudio(ctx context.Context) studioFound {
	base := strings.TrimSuffix(or(d.LMStudio, LMStudio), "/")
	var native struct {
		Data []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			State string `json:"state"`
		} `json:"data"`
	}
	if status, err := d.call(ctx, callTimeout, http.MethodGet, strings.TrimSuffix(base, "/v1")+"/api/v0/models", nil, &native); err == nil && status == http.StatusOK {
		var chat []string
		for _, m := range native.Data {
			if m.Type != "llm" && m.Type != "vlm" {
				continue
			}
			if m.State == "loaded" {
				return studioFound{answered: true, model: m.ID}
			}
			chat = append(chat, m.ID)
		}
		if len(chat) > 0 {
			return studioFound{answered: true, model: chat[0]}
		}
		return studioFound{answered: true}
	} else if errors.Is(err, errNoAnswer) {
		return studioFound{}
	}
	var openai struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	status, err := d.call(ctx, callTimeout, http.MethodGet, base+"/models", nil, &openai)
	if err != nil || status != http.StatusOK {
		return studioFound{answered: !errors.Is(err, errNoAnswer)}
	}
	for _, m := range openai.Data {
		if m.ID != "" && !strings.Contains(strings.ToLower(m.ID), "embed") {
			return studioFound{answered: true, model: m.ID}
		}
	}
	return studioFound{answered: true}
}

// errNoAnswer is a server that didn't answer: nothing listens, or nothing
// answered in time.
var errNoAnswer = errors.New("no answer")

// call sends a request of at most timeout and decodes a 200's JSON into out:
// its status, errNoAnswer when there was none, or a decoding error.
func (d *Detector) call(ctx context.Context, timeout time.Duration, method, target string, body []byte, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errNoAnswer, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode %s: %w", target, err)
	}
	return resp.StatusCode, nil
}
