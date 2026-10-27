package ai

import (
	"fmt"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
)

// Defaults for an endpoint's own settings, where the owner left them out.
const (
	// DefaultContextTokens is the context window Ollama is asked to load
	// models with: room for a draft's system prompt, tools and turns, as the
	// agent's own default (llm::wire::ollama::CONTEXT_TOKENS) and no less
	// than MinContextTokens.
	DefaultContextTokens = 65536
	// DefaultCallsInFlight is how many calls an OpenAI-compatible server is
	// sent at once.
	DefaultCallsInFlight = 4
)

// Deployment is a deployment's foundry.ai, checked once as a process starts.
type Deployment struct {
	// Kinds are the kinds owners may choose, in the settings page's order.
	Kinds []Kind
	// Default is the deployment's own provider, as configured; nil is none,
	// which only a local install may have. Current is the one in force.
	Default *Default
	// Detected is set only in a local install that configures no default:
	// the one it found on its machine (config.Host.DetectedAI), which may
	// change while it runs.
	Detected *config.LiveAIDefault
	Rules    Rules
	Catalog  *Catalog
	// Local is a local install: no plan is sold, so owners bring keys freely.
	Local       bool
	CallTimeout time.Duration
}

// Current is the default in force: the configured one, else the one a local
// install last found on its machine, else nil, none. A found default that
// doesn't check (the detector stores only ones that do) is none. Reading a
// found one checks it again, which costs no I/O.
//
//	d.Current() // Helix's Claude, hosted
func (d *Deployment) Current() *Default {
	if d.Default != nil || d.Detected == nil {
		return d.Default
	}
	from := d.Detected.Load().Default
	if from.Provider == "" {
		return nil
	}
	def, err := d.defaultOf(from)
	if err != nil {
		return nil
	}
	return def
}

// CheckDefault checks a default as NewDeployment checks the configured one,
// under d's rules: the detector's, before it stores what it found.
func (d *Deployment) CheckDefault(c config.AIDefault) error {
	_, err := d.defaultOf(c)
	return err
}

// Default is the deployment's provider: Helix's Claude when hosted.
type Default struct {
	Settings Settings
	// Key is the zero key where the process makes no model calls (the
	// backend), and for a kind that takes none.
	Key ontologyagent.ModelKey
}

// Offers reports whether owners may choose kind: never Ollama hosted, whose
// server takes no key, so a hosted one would have to be open to anyone.
func (d *Deployment) Offers(kind Kind) bool {
	return slices.Contains(d.Kinds, kind) && (d.Local || kind != KindOllama)
}

// NewDeployment checks cfg's foundry.ai beyond what config can: the kinds
// are known ones, Ollama among them only locally, and the default is a
// provider an owner could have saved (its endpoint, models and prices), with
// a key where its kind needs one in a local install. Cloud's key is required
// only where model calls are made, which config checks.
func NewDeployment(cfg *config.Config) (*Deployment, error) {
	a := cfg.Foundry.AI
	d := &Deployment{
		Rules: Rules{AllowPrivate: a.AllowPrivateHosts}, Local: cfg.Host.Local, CallTimeout: a.CallTimeout,
	}
	catalog, err := VendoredCatalog()
	if err != nil {
		return nil, err
	}
	d.Catalog = catalog
	for _, name := range a.Kinds {
		kind, err := ParseKind(name)
		if err != nil {
			return nil, fmt.Errorf("foundry.ai.kinds: %w", err)
		}
		switch {
		case slices.Contains(d.Kinds, kind):
			return nil, fmt.Errorf("foundry.ai.kinds lists %s twice", kind)
		case kind == KindOllama && !d.Local:
			return nil, fmt.Errorf("foundry.ai.kinds cannot offer ollama in cloud: an Ollama server takes no key")
		}
		d.Kinds = append(d.Kinds, kind)
	}
	if detected := cfg.Host.DetectedAI; detected != nil {
		switch {
		case !d.Local:
			return nil, fmt.Errorf("only a local install finds its AI default on its machine")
		case a.Default.Provider != "":
			return nil, fmt.Errorf("foundry.ai.default.provider is set: a local install finds its default only when it sets none")
		}
		d.Detected = detected
	}
	if a.Default.Provider == "" {
		if !d.Local {
			return nil, fmt.Errorf("foundry.ai.default is required in cloud")
		}
		return d, nil
	}
	def, err := d.defaultOf(a.Default)
	if err != nil {
		return nil, err
	}
	d.Default = def
	return d, nil
}

// defaultOf is c as the deployment's default: a provider an owner could have
// saved (its endpoint, models and prices), with a key where its kind needs
// one in a local install.
func (d *Deployment) defaultOf(c config.AIDefault) (*Default, error) {
	settings, err := defaultSettings(c)
	if err != nil {
		return nil, err
	}
	if err := settings.Validate(d.Rules); err != nil {
		return nil, fmt.Errorf("foundry.ai.default: %w", err)
	}
	settings = settings.Normalized(d.Rules)
	if unpriced := d.Catalog.Unpriced(settings); len(unpriced) > 0 {
		return nil, fmt.Errorf("foundry.ai.default.models.%s: the agent has no price for this %s model", unpriced[0], settings.Provider.Kind())
	}
	key := ontologyagent.NewModelKey(c.APIKey)
	switch rule := settings.Provider.Kind().Key(); {
	case rule == KeyRefused && !key.IsZero():
		return nil, fmt.Errorf("foundry.ai.default.api_key: %s takes no key", settings.Provider.Kind())
	case rule == KeyRequired && key.IsZero() && d.Local:
		return nil, fmt.Errorf("foundry.ai.default.api_key is required for %s", settings.Provider.Kind())
	case len(key.Reveal()) > MaxKeyBytes && settings.Provider.Kind() == KindBedrock:
		return nil, fmt.Errorf("foundry.ai.default.api_key is longer than %d bytes, as a short-term Bedrock key made from temporary AWS credentials is; use a long-term Bedrock API key", MaxKeyBytes)
	case len(key.Reveal()) > MaxKeyBytes:
		return nil, fmt.Errorf("foundry.ai.default.api_key is longer than %d bytes", MaxKeyBytes)
	}
	return &Default{Settings: settings, Key: key}, nil
}

// defaultSettings reads the default as settings: the kind's own fields and
// no other, and a model per task, priced by table or, on the owner's own
// hardware, free. An Ollama default's context window is DefaultContextTokens
// unless the install sets one.
func defaultSettings(c config.AIDefault) (Settings, error) {
	kind, err := ParseKind(c.Provider)
	if err != nil {
		return Settings{}, fmt.Errorf("foundry.ai.default.provider: %w", err)
	}
	unused := map[string]string{"base_url": c.BaseURL, "endpoint": c.Endpoint, "region": c.Region, "api": c.API}
	if kind != KindOllama && c.ContextTokens != 0 {
		return Settings{}, fmt.Errorf("foundry.ai.default.context_tokens is not used by %s", kind)
	}
	var p Provider
	switch kind {
	case KindAnthropic:
		p = Anthropic{}
	case KindOpenAI:
		p = OpenAI{}
	case KindAzureOpenAI:
		p = AzureOpenAI{Endpoint: c.Endpoint}
		delete(unused, "endpoint")
	case KindBedrock:
		p = Bedrock{Region: c.Region}
		delete(unused, "region")
	case KindOllama:
		window := c.ContextTokens
		if window == 0 {
			window = DefaultContextTokens
		}
		p = Ollama{BaseURL: c.BaseURL, ContextTokens: window}
		delete(unused, "base_url")
	case KindCompatible:
		api := CompatAPI(c.API)
		if api == "" {
			api = CompatChatCompletions
		}
		p = Compatible{BaseURL: c.BaseURL, API: api, MaxCallsInFlight: DefaultCallsInFlight}
		delete(unused, "base_url")
		delete(unused, "api")
	}
	for _, field := range []string{"base_url", "endpoint", "region", "api"} {
		if value, ok := unused[field]; ok && value != "" {
			return Settings{}, fmt.Errorf("foundry.ai.default.%s is not used by %s", field, kind)
		}
	}
	var price Price = PriceTable{}
	if kind.Free() {
		price = PriceFree{}
	}
	m := c.Models
	return Settings{Provider: p, Models: Roles{
		Draft: Model{Name: m.Draft, Price: price}, Ask: Model{Name: m.Ask, Price: price},
		Extract: Model{Name: m.Extract, Price: price}, Verify: Model{Name: m.Verify, Price: price},
	}}, nil
}
