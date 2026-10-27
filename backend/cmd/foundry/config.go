package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"

	"github.com/helixdb/helix-foundry/backend/ai"
	foundryconfig "github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/local"
)

// Config is a standalone install's configuration: Foundry's own, under the
// keys every deployment reads it from (foundryconfig.Config), and the
// install's.
type Config struct {
	Foundry foundryconfig.Foundry `koanf:"foundry"`
	Billing BillingConfig         `koanf:"billing"`
	Backend BackendConfig         `koanf:"backend"`

	DB    DBConfig    `koanf:"db"`
	AWS   AWSConfig   `koanf:"aws"`
	Local LocalConfig `koanf:"local"`
	Log   LogConfig   `koanf:"log"`

	// detectedAI is the default found on this machine, set when
	// foundry.ai.default names no provider (load).
	detectedAI *foundryconfig.LiveAIDefault
}

// noAI is the foundry.ai.default.provider that means none, and no look for
// one either (FOUNDRY_AI_PROVIDER=none): model work waits for an owner's
// choice.
const noAI = "none"

// BillingConfig holds billing.autumn.foundry: the plan catalog. An install
// sells none, so its one plan is every workspace's.
type BillingConfig struct {
	Autumn struct {
		Foundry foundryconfig.Billing `koanf:"foundry"`
	} `koanf:"autumn"`
}

// BackendConfig holds backend.foundry_read: where the API reaches
// foundry-read, in this process.
type BackendConfig struct {
	FoundryRead foundryconfig.ReadClient `koanf:"foundry_read"`
}

// DBConfig is the install's MySQL database, which it migrates at every start.
type DBConfig struct {
	DSN string `koanf:"dsn"`
}

// AWSConfig is the region S3 requests are signed for; an S3-compatible store
// accepts any.
type AWSConfig struct {
	Region string `koanf:"region"`
}

// LocalConfig is what the install is made of.
type LocalConfig struct {
	// SiteToken is the bearer the Foundry site sends for the owner: at least
	// 32 bytes, shared with the site and nothing else.
	SiteToken string `koanf:"site_token"`
	// HelixURL is the HelixDB every database is queried at, an origin such
	// as http://helix:8080. It keeps no databases apart: every ontology's
	// loads share it.
	HelixURL string `koanf:"helix_url"`
	// S3 is the store snapshots and documents are kept in.
	S3 local.S3 `koanf:"s3"`
	// Listen is where the install serves.
	Listen ListenConfig `koanf:"listen"`
	// SecretsDir, when set, is the directory `foundry init-secrets` wrote
	// the install's secrets to: the site token, the store's secret, the
	// database's password (for a db.dsn without one) and the connector key
	// are read from it unless configured otherwise, which wins.
	SecretsDir string `koanf:"secrets_dir"`
}

// ListenConfig is the install's listeners: the site calls REST, and gRPC
// and health stay on loopback.
type ListenConfig struct {
	REST   string `koanf:"rest"`
	GRPC   string `koanf:"grpc"`
	Health string `koanf:"health"`
}

type LogConfig struct {
	Format string `koanf:"format"`
	Level  string `koanf:"level"`
}

// defaults are Foundry's own defaults under the install's.
func defaults() Config {
	f := foundryconfig.Defaults()
	c := Config{
		Foundry: f.Foundry,
		Backend: BackendConfig{FoundryRead: f.Read},
		AWS:     AWSConfig{Region: "us-east-1"},
		Local:   LocalConfig{Listen: ListenConfig{REST: ":8081", GRPC: "127.0.0.1:50061", Health: "127.0.0.1:8083"}},
		Log:     LogConfig{Format: "json", Level: "info"},
	}
	c.Billing.Autumn.Foundry = f.Billing
	return c
}

// foundry is Foundry's configuration, as a host that runs on one machine.
func (c *Config) foundry() *foundryconfig.Config {
	return &foundryconfig.Config{
		Foundry: c.Foundry, Billing: c.Billing.Autumn.Foundry, Read: c.Backend.FoundryRead,
		Host: foundryconfig.Host{Local: true, DetectedAI: c.detectedAI},
	}
}

// load reads the configuration as a hosted deployment does: defaults, then
// the YAML file at path (if any), then HELIX_ environment variables. A
// variable names a key by its segments joined with "_" (HELIX_DB_DSN is
// db.dsn) or, for keys with underscores of their own, with "__"
// (HELIX_LOCAL__SITE_TOKEN is local.site_token). Secrets it still lacks come
// from local.secrets_dir. A foundry.ai.default with no provider is one found
// on this machine as the install starts (package ai/detect); "none" is none.
func load(path string) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(structs.Provider(defaults(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}
	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("load %s: %w", path, err)
		}
	}
	single := env.Provider("HELIX_", ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, "HELIX_")), "_", ".")
	})
	nested := env.Provider("HELIX_", ".", func(s string) string {
		s = strings.TrimPrefix(s, "HELIX_")
		if !strings.Contains(s, "__") {
			return ""
		}
		return strings.ToLower(strings.ReplaceAll(s, "__", "."))
	})
	for _, p := range []*env.Env{single, nested} {
		if err := k.Load(p, nil); err != nil {
			return nil, fmt.Errorf("load env: %w", err)
		}
	}
	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			Result:           &cfg,
			TagName:          "koanf",
			WeaklyTypedInput: true,
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc(),
				mapstructure.StringToSliceHookFunc(","),
			),
		},
	}); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if err := cfg.fillSecrets(); err != nil {
		return nil, err
	}
	// No provider is one found on this machine; "none" is none at all.
	switch cfg.Foundry.AI.Default.Provider {
	case "":
		cfg.detectedAI = &foundryconfig.LiveAIDefault{}
	case noAI:
		cfg.Foundry.AI.Default = foundryconfig.AIDefault{}
	}
	if err := validator.New().Struct(cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}

// validate refuses a configuration the install cannot run as: it starts
// with everything it serves, or not at all.
func (c *Config) validate() error {
	if strings.TrimSpace(c.DB.DSN) == "" {
		return errors.New("db.dsn is required")
	}
	if len(c.Local.SiteToken) < local.MinSiteToken {
		return fmt.Errorf("local.site_token must be at least %d bytes", local.MinSiteToken)
	}
	for _, endpoint := range []struct{ name, raw string }{
		{"local.helix_url", c.Local.HelixURL},
		{"local.s3.endpoint", c.Local.S3.Endpoint},
	} {
		u, err := url.Parse(endpoint.raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			strings.TrimSuffix(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("%s must be an http(s) origin, got %q", endpoint.name, endpoint.raw)
		}
	}
	if c.Local.S3.AccessKeyID == "" || c.Local.S3.SecretAccessKey == "" {
		return errors.New("local.s3.access_key_id and local.s3.secret_access_key are required")
	}
	if strings.TrimSpace(c.AWS.Region) == "" {
		return errors.New("aws.region is required")
	}
	for _, listen := range []struct{ name, addr string }{
		{"local.listen.rest", c.Local.Listen.REST},
		{"local.listen.grpc", c.Local.Listen.GRPC},
		{"local.listen.health", c.Local.Listen.Health},
	} {
		if strings.TrimSpace(listen.addr) == "" {
			return fmt.Errorf("%s is required", listen.name)
		}
	}
	// The install runs the API, foundry-read and every worker but people
	// enrichment's.
	f := c.foundry()
	if err := f.Validate(foundryconfig.Roles{
		API: true, Read: true, SyncWorker: true, DocumentWorker: true, OntologyWorker: true, GraphWorker: true, AskWorker: true,
	}); err != nil {
		return err
	}
	// The install's AI default, which the API reads when it starts.
	_, err := ai.NewDeployment(f)
	return err
}
