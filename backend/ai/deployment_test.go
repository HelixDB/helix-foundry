package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/config"
)

// fmtAll prints v every way fmt can.
func fmtAll(v any) string { return fmt.Sprintf("%v %+v %#v %s", v, v, v, v) }

func TestNewDeployment(t *testing.T) {
	// Hosted Foundry offers Claude to bring a key for, beside Helix's Claude.
	t.Run("reads Helix's Claude", func(t *testing.T) {
		d, err := NewDeployment(hostedConfig(sentinel))
		if err != nil {
			t.Fatal(err)
		}
		if d.Default == nil || d.Default.Settings.Provider != (Anthropic{}) || d.Default.Key.Reveal() != sentinel || d.Local {
			t.Fatalf("NewDeployment() = %+v", d)
		}
		if !d.Offers(KindAnthropic) || d.Offers(KindOpenAI) || d.Rules.AllowPrivate {
			t.Fatalf("kinds %v, rules %+v", d.Kinds, d.Rules)
		}
		if roles, _ := Suggested(KindAnthropic); d.Default.Settings.Models != roles {
			t.Fatalf("the default's models %+v are not Claude's suggestions", d.Default.Settings.Models)
		}
	})

	// The backend reads and saves settings without Helix's key; the workers
	// that call the model are refused it by config.
	t.Run("reads the default without a key in cloud", func(t *testing.T) {
		d, err := NewDeployment(hostedConfig(""))
		if err != nil || !d.Default.Key.IsZero() {
			t.Fatalf("NewDeployment() = %+v, %v", d, err)
		}
	})

	// A default is checked as an owner's save would be, at start, so a bad
	// one fails the boot rather than every run.
	bedrock, _ := Suggested(KindBedrock)
	profile := bedrock.Ask.Name
	for name, tc := range map[string]struct {
		mutate func(*config.Config)
		want   string
	}{
		"an unknown kind offered":     {func(c *config.Config) { c.Foundry.AI.Kinds = []string{"anthropic", "gemini"} }, "foundry.ai.kinds"},
		"a kind offered twice":        {func(c *config.Config) { c.Foundry.AI.Kinds = []string{"anthropic", "anthropic"} }, "twice"},
		"an unknown default":          {func(c *config.Config) { c.Foundry.AI.Default.Provider = "gemini" }, "foundry.ai.default.provider"},
		"another kind's field":        {func(c *config.Config) { c.Foundry.AI.Default.BaseURL = "https://proxy.example.com" }, "base_url is not used by anthropic"},
		"an unpriced model":           {func(c *config.Config) { c.Foundry.AI.Default.Models.Ask = "claude-next" }, "models.ask"},
		"a model name with a space":   {func(c *config.Config) { c.Foundry.AI.Default.Models.Draft = "claude opus" }, "models.draft.name"},
		"no default in cloud":         {func(c *config.Config) { c.Foundry.AI.Default = config.AIDefault{} }, "required in cloud"},
		"an over-long key":            {func(c *config.Config) { c.Foundry.AI.Default.APIKey = strings.Repeat("k", MaxKeyBytes+1) }, "longer than"},
		"a keyed Ollama locally":      {local(config.AIDefault{Provider: "ollama", APIKey: "sk", BaseURL: "http://127.0.0.1:11434", Models: models("qwen3:32b")}, true), "takes no key"},
		"a keyless Claude locally":    {local(config.AIDefault{Provider: "anthropic", Models: models("claude-opus-5-5")}, true), "api_key is required"},
		"a keyless OpenAI locally":    {local(config.AIDefault{Provider: "openai", Models: models("gpt-6.1-sol")}, true), "api_key is required"},
		"an unpriced OpenAI locally":  {local(config.AIDefault{Provider: "openai", APIKey: "sk-proj-x", Models: models("gpt-next")}, true), "no price for this openai model"},
		"an Azure default off Azure":  {local(config.AIDefault{Provider: "azure_openai", APIKey: "k", Endpoint: "https://llm.example.com", Models: models("gpt-6.1-sol")}, true), "provider.endpoint"},
		"an endpoint for OpenAI":      {local(config.AIDefault{Provider: "openai", APIKey: "k", Endpoint: "https://acme.openai.azure.com", Models: models("gpt-6.1-sol")}, true), "endpoint is not used by openai"},
		"a keyless Bedrock locally":   {local(config.AIDefault{Provider: "bedrock", Region: "us-east-1", Models: models(profile)}, true), "api_key is required"},
		"a Bedrock default anywhere":  {local(config.AIDefault{Provider: "bedrock", APIKey: "ABSK-k", Models: models(profile)}, true), "provider.region"},
		"a Bedrock region as a host":  {local(config.AIDefault{Provider: "bedrock", APIKey: "ABSK-k", Region: "bedrock-runtime.us-east-1.amazonaws.com", Models: models(profile)}, true), "provider.region"},
		"an unpriced Bedrock model":   {local(config.AIDefault{Provider: "bedrock", APIKey: "ABSK-k", Region: "us-east-1", Models: models("us.anthropic.claude-next")}, true), "no price for this bedrock model"},
		"a base URL for Bedrock":      {local(config.AIDefault{Provider: "bedrock", APIKey: "ABSK-k", Region: "us-east-1", BaseURL: "https://bedrock.example.com", Models: models(profile)}, true), "base_url is not used by bedrock"},
		"a session's Bedrock key":     {local(config.AIDefault{Provider: "bedrock", APIKey: "bedrock-api-key-" + strings.Repeat("Ym", 600), Region: "us-east-1", Models: models(profile)}, true), "use a long-term Bedrock API key"},
		"a region for Claude":         {func(c *config.Config) { c.Foundry.AI.Default.Region = "us-east-1" }, "region is not used by anthropic"},
		"http without private hosts":  {local(config.AIDefault{Provider: "openai_compatible", BaseURL: "http://127.0.0.1:8000/v1", Models: models("m")}, false), "provider.base_url"},
		"an unknown API for a server": {local(config.AIDefault{Provider: "openai_compatible", BaseURL: "https://llm.example.com/v1", API: "messages", Models: models("m")}, true), "provider.api"},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			cfg := hostedConfig(sentinel)
			tc.mutate(cfg)
			_, err := NewDeployment(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewDeployment() = %v, want an error naming %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), sentinel) {
				t.Fatalf("the error names the key: %v", err)
			}
		})
	}

	// A local install may have no provider, or run on a server on the same
	// machine, priced free.
	t.Run("reads a local install's default", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Host.Local = true
		d, err := NewDeployment(&cfg)
		if err != nil || d.Default != nil || !d.Local {
			t.Fatalf("no default: %+v, %v", d, err)
		}
		cfg.Foundry.AI.AllowPrivateHosts = true
		cfg.Foundry.AI.Default = config.AIDefault{Provider: "ollama", BaseURL: "http://127.0.0.1:11434/", Models: models("qwen3:32b")}
		d, err = NewDeployment(&cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: DefaultContextTokens}
		if d.Default.Settings.Provider != want || d.Default.Settings.Models.Draft.Price != (PriceFree{}) {
			t.Fatalf("the Ollama default = %+v", d.Default.Settings)
		}
		// OpenAI and an Azure resource's deployments, named as the models they
		// deploy, are priced by the table under the install's key.
		cfg.Foundry.AI.Default = config.AIDefault{Provider: "openai", APIKey: "sk-proj-local", Models: models("gpt-6.1-sol")}
		if d, err = NewDeployment(&cfg); err != nil || d.Default.Settings.Provider != (OpenAI{}) || d.Default.Key.Reveal() != "sk-proj-local" ||
			d.Default.Settings.Models.Ask != (Model{Name: "gpt-6.1-sol", Price: PriceTable{}}) {
			t.Fatalf("the OpenAI default = %+v, %v", d, err)
		}
		cfg.Foundry.AI.Default = config.AIDefault{Provider: "azure_openai", APIKey: "azure-key", Endpoint: "https://Acme.openai.azure.com/", Models: models("gpt-6-luna")}
		if d, err = NewDeployment(&cfg); err != nil || d.Default.Settings.Provider != (AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai/v1"}) {
			t.Fatalf("the Azure default = %+v, %v", d, err)
		}
		// Amazon Bedrock in the install's region, on an inference profile
		// priced as its model, under the install's Bedrock API key.
		cfg.Foundry.AI.Default = config.AIDefault{Provider: "bedrock", APIKey: "ABSK-local-key", Region: "eu-west-1", Models: models(profile)}
		if d, err = NewDeployment(&cfg); err != nil || d.Default.Settings.Provider != (Bedrock{Region: "eu-west-1"}) || d.Default.Key.Reveal() != "ABSK-local-key" ||
			d.Default.Settings.Models.Draft != (Model{Name: profile, Price: PriceTable{}}) || Label(d.Default.Settings.Provider) != "Amazon Bedrock" {
			t.Fatalf("the Bedrock default = %+v, %v", d, err)
		}
	})
}

// local makes c a local install with default def.
func local(def config.AIDefault, allowPrivate bool) func(*config.Config) {
	return func(c *config.Config) {
		c.Host.Local = true
		c.Foundry.AI.Default = def
		c.Foundry.AI.AllowPrivateHosts = allowPrivate
	}
}

func models(name string) config.AIModels {
	return config.AIModels{Draft: name, Ask: name, Extract: name, Verify: name}
}

// A local install that configures no default may find one on its machine:
// Current reads the one found last, checked, so a model found later is used
// without a restart. Only such an install looks.
func TestDetectedDefault(t *testing.T) {
	cfg := hostedConfig("")
	local(config.AIDefault{}, true)(cfg)
	cfg.Foundry.AI.Kinds = []string{"ollama", "anthropic"}
	found := &config.LiveAIDefault{}
	cfg.Host.DetectedAI = found
	d, err := NewDeployment(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if d.Default != nil || d.Detected != found || d.Current() != nil {
		t.Fatalf("before a look: %+v", d)
	}
	found.Store(config.DetectedAIDefault{Default: config.AIDefault{Provider: "ollama", BaseURL: "HTTP://127.0.0.1:11434/", Models: models("llama3.2:1b")}})
	cur := d.Current()
	if cur == nil || cur.Settings.Provider != (Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: DefaultContextTokens}) ||
		cur.Settings.Models.Ask != (Model{Name: "llama3.2:1b", Price: PriceFree{}}) || !cur.Key.IsZero() {
		t.Fatalf("found Ollama: %+v", cur)
	}
	if err := d.CheckDefault(found.Load().Default); err != nil {
		t.Fatal(err)
	}
	found.Store(config.DetectedAIDefault{Default: config.AIDefault{Provider: "anthropic", APIKey: sentinel, Models: models("claude-sonnet-5-5")}})
	if cur := d.Current(); cur == nil || cur.Settings.Provider != (Anthropic{}) || cur.Key.Reveal() != sentinel {
		t.Fatalf("found Claude: %+v", cur)
	}
	// A workspace without a setting of its own runs on it.
	r := &Resolver{DB: activeConfigs{}, Deployment: d}
	if got, err := r.Resolve(context.Background(), "ws-1"); err != nil || got.Source != SourceDefault || got.Key.Reveal() != sentinel {
		t.Fatalf("Resolve() = %+v, %v", got, err)
	}
	// One that doesn't check is none, as is none found.
	bad := config.AIDefault{Provider: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Models: models("m")}
	found.Store(config.DetectedAIDefault{Default: bad})
	if d.Current() != nil || d.CheckDefault(bad) == nil {
		t.Fatal("used a default that doesn't check")
	}
	found.Store(config.DetectedAIDefault{Summary: "none"})
	if d.Current() != nil || !errors.Is(r.Usable(context.Background(), "ws-1"), ErrNotConfigured) {
		t.Fatal("none found is a default")
	}

	// A configured default wins, and the two exclude each other.
	t.Run("with a configured default", func(t *testing.T) {
		cfg := hostedConfig(sentinel)
		local(cfg.Foundry.AI.Default, true)(cfg)
		cfg.Host.DetectedAI = found
		if _, err := NewDeployment(cfg); err == nil || !strings.Contains(err.Error(), "finds its default only when it sets none") {
			t.Fatalf("err = %v", err)
		}
		cfg.Host.DetectedAI = nil
		d, err := NewDeployment(cfg)
		if err != nil || d.Current() != d.Default || d.Current() == nil {
			t.Fatalf("configured: %+v, %v", d, err)
		}
	})
	t.Run("hosted", func(t *testing.T) {
		cfg := hostedConfig(sentinel)
		cfg.Host.DetectedAI = found
		if _, err := NewDeployment(cfg); err == nil || !strings.Contains(err.Error(), "only a local install") {
			t.Fatalf("err = %v", err)
		}
	})
}
