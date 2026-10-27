package ai

import (
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
