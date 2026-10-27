package main

import (
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	foundryconfig "github.com/helixdb/helix-foundry/backend/config"
)

const siteToken = "site-token-0123456789abcdefghijklmnop"

// secrets are the variables the local compose file passes.
func secrets(t *testing.T) {
	t.Setenv("HELIX_DB_DSN", "helix:pw@tcp(mysql:3306)/helix?parseTime=true&multiStatements=true")
	t.Setenv("HELIX_LOCAL__SITE_TOKEN", siteToken)
	t.Setenv("HELIX_LOCAL__S3__SECRET_ACCESS_KEY", "s3-secret")
	t.Setenv("HELIX_FOUNDRY__CONNECTORS__CURRENT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

// The image's file, with the compose file's secrets, is an install's
// configuration.
func TestBakedConfig(t *testing.T) {
	secrets(t)
	cfg, err := load("foundry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.Local
	if l.SiteToken != siteToken || l.HelixURL != "http://helix:8080" || l.S3.Endpoint != "http://localhost:8333" ||
		l.S3.AccessKeyID != "foundry" || l.S3.SecretAccessKey != "s3-secret" {
		t.Fatalf("local = %+v", l)
	}
	if l.Listen != (ListenConfig{REST: ":8081", GRPC: "127.0.0.1:50061", Health: "127.0.0.1:8083"}) {
		t.Fatalf("listen = %+v", l.Listen)
	}
	if cfg.DB.DSN != "helix:pw@tcp(mysql:3306)/helix?parseTime=true&multiStatements=true" || cfg.AWS.Region != "us-east-1" {
		t.Fatalf("db %q, region %q", cfg.DB.DSN, cfg.AWS.Region)
	}
	if cfg.Log != (LogConfig{Format: "json", Level: "info"}) {
		t.Fatalf("log = %+v", cfg.Log)
	}
	if f := cfg.foundry(); !f.Host.Local || f.Host.GatewayKey != "" {
		t.Fatalf("host = %+v", f.Host)
	}
}

// The image's Foundry settings: one plan, sold to no one, with every
// connector and Max's sync rules; model providers of every kind, Ollama
// first, with no default until the install sets one; and the longer steps
// of a model server on this machine.
func TestBakedFoundryConfig(t *testing.T) {
	secrets(t)
	cfg, err := load("foundry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f := cfg.foundry()
	if f.Foundry.Ask.HTTP.Listen == f.Foundry.Read.HTTP.Listen {
		t.Fatalf("ask and read share %q", f.Foundry.Ask.HTTP.Listen)
	}
	if plans := f.Billing.Plans; len(plans) != 1 || plans[0].Code != "local" || plans[0].AutumnPlanID != "" ||
		plans[0].Sync.Every != 15*time.Minute || !slices.Contains(plans[0].Sync.Capture, "log") {
		t.Fatalf("plans = %+v", plans)
	}
	defaults := foundryconfig.Defaults()
	if got := f.Foundry.AI; !slices.Equal(got.Kinds, []string{"ollama", "anthropic", "openai", "azure_openai", "openai_compatible", "bedrock"}) ||
		got.Default.Provider != "" || !got.AllowPrivateHosts || got.CallTimeout != 30*time.Minute ||
		got.Probe.Timeout != 15*time.Minute || got.Probe.Lease != 20*time.Minute || got.Probe.PerHour != 10 ||
		got.Probe.PollInterval != defaults.Foundry.AI.Probe.PollInterval || got.Probe.MaxAttempts != 3 {
		t.Fatalf("foundry.ai = %+v", got)
	}
	// A question gets the most the agent allows, and a draft forty minutes,
	// its attempt keeping the margin above.
	if got := f.Foundry.Ask.MaxWallSecs; got != foundryconfig.AskWallSecsLimit-1 {
		t.Fatalf("foundry.ask.max_wall_secs = %d", got)
	}
	if o := f.Foundry.Ontology; o.Agent.Limits.MaxWallSecs != 2400 || o.Timeout != 40*time.Minute+foundryconfig.AgentAttemptMargin {
		t.Fatalf("foundry.ontology timeout %s, agent.limits %+v", o.Timeout, o.Agent.Limits)
	}
}

// An install's own default model provider comes from the compose file's
// FOUNDRY_AI_* variables, any of them empty when unset; the ai package
// checks it as the process starts.
func TestAIDefaultFromEnvironment(t *testing.T) {
	const bedrock = "us.anthropic.claude-sonnet-5-5"
	for name, tc := range map[string]struct {
		env  map[string]string
		want foundryconfig.AIDefault
	}{
		"Amazon Bedrock": {map[string]string{
			"PROVIDER": "bedrock", "REGION": "eu-west-1", "API_KEY": "ABSKbG9jYWwtYmVkcm9jay1rZXk=",
			"MODELS__DRAFT": bedrock, "MODELS__ASK": bedrock, "MODELS__EXTRACT": bedrock, "MODELS__VERIFY": bedrock,
		}, foundryconfig.AIDefault{Provider: "bedrock", Region: "eu-west-1", APIKey: "ABSKbG9jYWwtYmVkcm9jay1rZXk=",
			Models: foundryconfig.AIModels{Draft: bedrock, Ask: bedrock, Extract: bedrock, Verify: bedrock}}},
		"Ollama, its window unset": {map[string]string{
			"PROVIDER": "ollama", "API_KEY": "", "BASE_URL": "http://127.0.0.1:11434", "CONTEXT_TOKENS": "",
			"MODELS__DRAFT": "qwen3:32b", "MODELS__ASK": "qwen3:8b", "MODELS__EXTRACT": "qwen3:8b", "MODELS__VERIFY": "qwen3:8b",
		}, foundryconfig.AIDefault{Provider: "ollama", BaseURL: "http://127.0.0.1:11434",
			Models: foundryconfig.AIModels{Draft: "qwen3:32b", Ask: "qwen3:8b", Extract: "qwen3:8b", Verify: "qwen3:8b"}}},
		"Ollama with a window": {map[string]string{
			"PROVIDER": "ollama", "BASE_URL": "http://127.0.0.1:11434", "CONTEXT_TOKENS": "65536",
			"MODELS__DRAFT": "qwen3:32b", "MODELS__ASK": "qwen3:8b", "MODELS__EXTRACT": "qwen3:8b", "MODELS__VERIFY": "qwen3:8b",
		}, foundryconfig.AIDefault{Provider: "ollama", BaseURL: "http://127.0.0.1:11434", ContextTokens: 65536,
			Models: foundryconfig.AIModels{Draft: "qwen3:32b", Ask: "qwen3:8b", Extract: "qwen3:8b", Verify: "qwen3:8b"}}},
		"a custom server with no key": {map[string]string{
			"PROVIDER": "openai_compatible", "BASE_URL": "http://host.docker.internal:1234/v1", "API": "responses",
			"MODELS__DRAFT": "qwen3-32b", "MODELS__ASK": "qwen3-32b", "MODELS__EXTRACT": "qwen3-8b", "MODELS__VERIFY": "qwen3-8b",
		}, foundryconfig.AIDefault{Provider: "openai_compatible", BaseURL: "http://host.docker.internal:1234/v1", API: "responses",
			Models: foundryconfig.AIModels{Draft: "qwen3-32b", Ask: "qwen3-32b", Extract: "qwen3-8b", Verify: "qwen3-8b"}}},
		"a custom server with a key": {map[string]string{
			"PROVIDER": "openai_compatible", "BASE_URL": "http://host.docker.internal:1234/v1", "API_KEY": "sk-local-gateway-0123456789",
			"MODELS__DRAFT": "qwen3-32b", "MODELS__ASK": "qwen3-32b", "MODELS__EXTRACT": "qwen3-8b", "MODELS__VERIFY": "qwen3-8b",
		}, foundryconfig.AIDefault{Provider: "openai_compatible", BaseURL: "http://host.docker.internal:1234/v1", APIKey: "sk-local-gateway-0123456789",
			Models: foundryconfig.AIModels{Draft: "qwen3-32b", Ask: "qwen3-32b", Extract: "qwen3-8b", Verify: "qwen3-8b"}}},
	} {
		t.Run(name, func(t *testing.T) {
			secrets(t)
			for k, v := range tc.env {
				t.Setenv("HELIX_FOUNDRY__AI__DEFAULT__"+k, v)
			}
			cfg, err := load("foundry.yaml")
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Foundry.AI.Default; got != tc.want {
				t.Fatalf("foundry.ai.default = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Every key takes a HELIX_ variable: one underscore between segments, or
// two where a segment has its own.
func TestEnvironmentOverrides(t *testing.T) {
	secrets(t)
	t.Setenv("HELIX_LOCAL__HELIX_URL", "https://helix.example:8443/")
	t.Setenv("HELIX_LOCAL__LISTEN__REST", "127.0.0.1:9081")
	t.Setenv("HELIX_LOG_LEVEL", "debug")
	t.Setenv("HELIX_AWS_REGION", "eu-west-1")
	cfg, err := load("foundry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Local.HelixURL != "https://helix.example:8443/" || cfg.Local.Listen.REST != "127.0.0.1:9081" ||
		cfg.Log.Level != "debug" || cfg.AWS.Region != "eu-west-1" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

// Without a file the defaults hold and the secrets are required.
func TestDefaults(t *testing.T) {
	t.Setenv("HELIX_DB_DSN", "")
	if _, err := load(""); err == nil || !strings.Contains(err.Error(), "db.dsn is required") {
		t.Fatalf("err = %v", err)
	}
	if _, err := load("no-such-file.yaml"); err == nil {
		t.Fatal("loaded a missing file")
	}
}

func TestValidate(t *testing.T) {
	secrets(t)
	valid := func() *Config {
		c, err := load("foundry.yaml")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if err := valid().validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for name, tc := range map[string]struct {
		change func(*Config)
		want   string
	}{
		"no dsn":             {func(c *Config) { c.DB.DSN = " " }, "db.dsn is required"},
		"a short token":      {func(c *Config) { c.Local.SiteToken = siteToken[:31] }, "at least 32 bytes"},
		"no helix":           {func(c *Config) { c.Local.HelixURL = "" }, "local.helix_url must be an http(s) origin"},
		"a helix path":       {func(c *Config) { c.Local.HelixURL = "http://helix:8080/v2" }, "local.helix_url must be"},
		"a helix query":      {func(c *Config) { c.Local.HelixURL = "http://helix:8080?x=1" }, "local.helix_url must be"},
		"a helix user":       {func(c *Config) { c.Local.HelixURL = "http://u:p@helix:8080" }, "local.helix_url must be"},
		"a helix fragment":   {func(c *Config) { c.Local.HelixURL = "http://helix:8080#x" }, "local.helix_url must be"},
		"another scheme":     {func(c *Config) { c.Local.HelixURL = "ftp://helix" }, "local.helix_url must be"},
		"no store":           {func(c *Config) { c.Local.S3.Endpoint = "localhost:8333" }, "local.s3.endpoint must be"},
		"no store key":       {func(c *Config) { c.Local.S3.AccessKeyID = "" }, "local.s3.access_key_id and"},
		"no store secret":    {func(c *Config) { c.Local.S3.SecretAccessKey = "" }, "local.s3.access_key_id and"},
		"no region":          {func(c *Config) { c.AWS.Region = "" }, "aws.region is required"},
		"no rest listener":   {func(c *Config) { c.Local.Listen.REST = "" }, "local.listen.rest is required"},
		"no grpc listener":   {func(c *Config) { c.Local.Listen.GRPC = "" }, "local.listen.grpc is required"},
		"no health listener": {func(c *Config) { c.Local.Listen.Health = "" }, "local.listen.health is required"},
		// Foundry's own checks, for a host on one machine running every part
		// the install runs.
		"no data bucket":    {func(c *Config) { c.Foundry.Sync.DataBucket = "" }, "local mode needs foundry.sync.data_bucket"},
		"a plan sold":       {func(c *Config) { c.Billing.Autumn.Foundry.Plans[0].AutumnPlanID = "pro" }, "local mode sells no plan"},
		"no agent for read": {func(c *Config) { c.Foundry.Read.AgentAddr = "" }, "foundry.read.agent_addr must be set"},
		"an ask listener":   {func(c *Config) { c.Foundry.Ask.HTTP.Listen = "" }, "foundry.ask"},
		// The AI default, before the install migrates anything.
		"an AI default's stray field": {func(c *Config) {
			c.detectedAI = nil
			c.Foundry.AI.Default = foundryconfig.AIDefault{Provider: "anthropic", APIKey: "test", BaseURL: "http://127.0.0.1:1",
				Models: foundryconfig.AIModels{Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5"}}
		}, "foundry.ai.default.base_url is not used by anthropic"},
		// A default configured and one found exclude each other.
		"a default and a look for one": {func(c *Config) {
			c.Foundry.AI.Default = foundryconfig.AIDefault{Provider: "anthropic", APIKey: "test",
				Models: foundryconfig.AIModels{Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5"}}
		}, "a local install finds its default only when it sets none"},
	} {
		t.Run(name, func(t *testing.T) {
			c := valid()
			tc.change(c)
			if err := c.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// No provider configured is a look for one on this machine, shared by every
// part of the install; "none" is none at all, whatever else is set; a
// provider is that provider, and no look.
func TestAIDefaultDetection(t *testing.T) {
	secrets(t)
	cfg, err := load("foundry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.detectedAI == nil || cfg.foundry().Host.DetectedAI != cfg.detectedAI || cfg.foundry().Host.DetectedAI != cfg.foundry().Host.DetectedAI {
		t.Fatalf("detected = %v", cfg.detectedAI)
	}
	d, err := newDetector(cfg.foundry(), slog.New(slog.NewTextHandler(io.Discard, nil)), func(k string) string {
		return map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x", "OPENAI_API_KEY": "sk-x"}[k]
	})
	if err != nil || d == nil || d.AnthropicKey != "sk-ant-x" || d.OpenAIKey != "sk-x" || d.Live != cfg.detectedAI || d.Check == nil {
		t.Fatalf("detector = %+v, %v", d, err)
	}
	// What it finds is checked as the configured default would be.
	if err := d.Check(foundryconfig.AIDefault{Provider: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Models: foundryconfig.AIModels{
		Draft: "m", Ask: "m", Extract: "m", Verify: "m"}}); err == nil {
		t.Fatal("checked an Ollama default at /v1")
	}

	t.Setenv("HELIX_FOUNDRY__AI__DEFAULT__PROVIDER", "none")
	t.Setenv("HELIX_FOUNDRY__AI__DEFAULT__MODELS__DRAFT", "ignored")
	cfg, err = load("foundry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.detectedAI != nil || cfg.Foundry.AI.Default != (foundryconfig.AIDefault{}) {
		t.Fatalf("none: detected %v, default %+v", cfg.detectedAI, cfg.Foundry.AI.Default)
	}
	if d, err := newDetector(cfg.foundry(), slog.Default(), os.Getenv); err != nil || d != nil {
		t.Fatalf("none: detector %v, %v", d, err)
	}

	t.Setenv("HELIX_FOUNDRY__AI__DEFAULT__PROVIDER", "openai")
	t.Setenv("HELIX_FOUNDRY__AI__DEFAULT__API_KEY", "sk-configured")
	for _, role := range []string{"DRAFT", "ASK", "EXTRACT", "VERIFY"} {
		t.Setenv("HELIX_FOUNDRY__AI__DEFAULT__MODELS__"+role, "gpt-6.1-sol")
	}
	if cfg, err = load("foundry.yaml"); err != nil || cfg.detectedAI != nil || cfg.Foundry.AI.Default.Provider != "openai" {
		t.Fatalf("configured: %+v, %v", cfg, err)
	}
}
