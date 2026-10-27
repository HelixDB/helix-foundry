package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

func contains(s, substr string) bool { return strings.Contains(s, substr) }

func helixClaude() AIDefault {
	return AIDefault{Provider: "anthropic", APIKey: "test", Models: AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
	}}
}

// every runs each of Foundry's roles.
var every = Roles{API: true, Read: true, Events: true, SyncWorker: true, DocumentWorker: true, OntologyWorker: true, GraphWorker: true,
	AskWorker: true, EnrichmentWorker: true}

// The defaults, with a model provider and the connectors' key, serve every
// role; each role's own settings are checked only where it runs.
func TestValidateRoles(t *testing.T) {
	valid := func() Config {
		c := Defaults()
		c.Foundry.AI.Default = helixClaude()
		c.Foundry.Sync.DataBucket = "foundry-data"
		c.Foundry.Connectors = ConnectorsConfig{CurrentKeyID: "k1", CurrentKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}
		return c
	}
	if c := valid(); c.Validate(every) != nil {
		t.Fatalf("defaults for every role: %v", c.Validate(every))
	}
	for name, tc := range map[string]struct {
		mutate func(*Config)
		roles  Roles
		want   string
	}{
		"the sync worker's bucket":    {func(c *Config) { c.Foundry.Sync.DataBucket = "" }, Roles{SyncWorker: true}, "foundry.sync.data_bucket"},
		"foundry-read's sidecar":      {func(c *Config) { c.Foundry.Read.AgentAddr = "agent:50051" }, Roles{Read: true}, "loopback"},
		"the door's backend":          {func(c *Config) { c.Foundry.Events.Endpoint.Backend.Address = "" }, Roles{Events: true}, "backend.address"},
		"the API's generation cap":    {func(c *Config) { c.Foundry.Ontology.PerHour = 0 }, Roles{API: true}, "foundry.ontology.per_hour"},
		"the hosted default's key":    {func(c *Config) { c.Foundry.AI.Default.APIKey = "" }, Roles{GraphWorker: true}, "api_key must be set"},
		"the graph worker's rate":     {func(c *Config) { c.Foundry.Graph.LoadRateLimit.Burst = 5 }, Roles{GraphWorker: true}, "load_rate_limit"},
		"the enrichment worker's key": {func(c *Config) { c.Foundry.Enrichment.Provider = EnrichmentContextDev }, Roles{EnrichmentWorker: true}, "api_key"},
		"the shared plan catalog":     {func(c *Config) { c.Billing.Plans = nil }, Roles{}, "at least the Free plan"},
		"a stream's extracts":         {func(c *Config) { c.Foundry.Events.MaxExtracted = c.Foundry.Documents.MaxPerStream + 1 }, Roles{}, "max_extracted"},
	} {
		t.Run(name, func(t *testing.T) {
			c := valid()
			tc.mutate(&c)
			if err := c.Validate(tc.roles); err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
			if tc.roles == (Roles{}) {
				return
			}
			if err := c.Validate(Roles{}); err != nil {
				t.Fatalf("checked without its role: %v", err)
			}
		})
	}
}

// A standalone install sells nothing, has no gateway to boost and no door,
// and needs its bucket, its connectors' key and foundry-read.
func TestValidateLocal(t *testing.T) {
	local := func() Config {
		c := Defaults()
		c.Host.Local = true
		c.Foundry.Sync.DataBucket = "foundry-data"
		c.Foundry.Connectors = ConnectorsConfig{CurrentKeyID: "local", CurrentKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}
		c.Read.Address = "127.0.0.1:50068"
		plans := DefaultPlans()
		c.Billing.Plans = []PlanConfig{{Code: "local", DisplayName: "Local", Kinds: plans[len(plans)-1].Kinds, Sync: plans[len(plans)-1].Sync}}
		return c
	}
	if c := local(); c.Validate(every) != nil {
		t.Fatalf("a complete install: %v", c.Validate(every))
	}
	for name, tc := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"no bucket":          {func(c *Config) { c.Foundry.Sync.DataBucket = "" }, "local mode needs foundry.sync.data_bucket"},
		"no connectors' key": {func(c *Config) { c.Foundry.Connectors = ConnectorsConfig{} }, "local mode needs foundry.connectors.current_key"},
		"no foundry-read":    {func(c *Config) { c.Read.Address = " " }, "local mode needs backend.foundry_read.address"},
		"a plan sold":        {func(c *Config) { c.Billing.Plans[0].AutumnPlanID = "pro" }, "sells no plan"},
		"two plans":          {func(c *Config) { c.Billing.Plans = DefaultPlans() }, "sells no plan"},
		"a kind left out":    {func(c *Config) { c.Billing.Plans[0].Kinds = []string{"stripe"} }, "lacks"},
		"a load rate limit": {func(c *Config) { c.Foundry.Graph.LoadRateLimit = GraphLoadRateLimit{RequestsPerSecond: 1, Burst: 1} },
			"no gateway to rate limit"},
		"a door":         {func(c *Config) { c.Foundry.Events.Endpoint.PublicURL = "https://ingest.example.com" }, "no API sources HTTP door"},
		"Ollama offered": {func(c *Config) { c.Foundry.AI.Kinds = []string{"ollama"} }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			c := local()
			tc.mutate(&c)
			err := c.Validate(Roles{API: true})
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !contains(err.Error(), tc.want)) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFoundryBillingValidate(t *testing.T) {
	const returnURL = "https://foundry.example/settings/billing"
	sold := func(ids ...string) []PlanConfig {
		plans := DefaultPlans()
		for i, id := range ids {
			plans[i+1].AutumnPlanID = id
		}
		return plans
	}
	// Foundry billing is off until a plan is sold, whatever else is set.
	t.Run("allows no plan sold", func(t *testing.T) {
		for _, c := range []Billing{{Plans: DefaultPlans()}, {Plans: DefaultPlans(), ReturnURL: "http://foundry.example/billing"}} {
			if c.Enabled() {
				t.Fatalf("%+v is enabled", c)
			}
			if err := c.validate(); err != nil {
				t.Fatalf("validate(%+v): %v", c, err)
			}
		}
	})
	// A deployment may sell one plan before it sells the next.
	for name, c := range map[string]Billing{
		"pro only":    {Plans: sold("foundry-pro"), ReturnURL: returnURL},
		"max only":    {Plans: sold("", "foundry-max"), ReturnURL: returnURL},
		"pro and max": {Plans: sold("foundry-pro", "foundry-max"), ReturnURL: returnURL},
		"localhost":   {Plans: sold("foundry-pro"), ReturnURL: "http://localhost:3000/settings/billing"},
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			if !c.Enabled() {
				t.Fatalf("%+v is not enabled", c)
			}
			if err := c.validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
	// One Autumn plan cannot be two plans: the plan read back would be ambiguous.
	t.Run("rejects one autumn plan for two plans", func(t *testing.T) {
		c := Billing{Plans: sold("foundry-pro", "foundry-pro"), ReturnURL: returnURL}
		if err := c.validate(); err == nil {
			t.Fatal("validate succeeded, want error")
		}
	})
	// Checkout cannot send the customer nowhere once a plan is sold.
	t.Run("rejects a sold plan without a return page", func(t *testing.T) {
		if err := (Billing{Plans: sold("foundry-pro")}).validate(); err == nil {
			t.Fatal("validate succeeded, want error")
		}
	})
	t.Run("rejects an insecure return page", func(t *testing.T) {
		if err := (Billing{Plans: sold("foundry-pro"), ReturnURL: "http://foundry.example/billing"}).validate(); err == nil {
			t.Fatal("validate succeeded, want error")
		}
	})
	// Each rule a catalog entry can break is refused at startup, before a sync reads it.
	for name, edit := range map[string]func(p []PlanConfig){
		"no plans":                   func(p []PlanConfig) {},
		"a sold free plan":           func(p []PlanConfig) { p[0].AutumnPlanID = "foundry-free" },
		"a repeated code":            func(p []PlanConfig) { p[2].Code = "pro" },
		"a plan without a name":      func(p []PlanConfig) { p[1].DisplayName = "" },
		"capture without full":       func(p []PlanConfig) { p[1].Sync.Capture = []string{"cursor"} },
		"an unknown capture":         func(p []PlanConfig) { p[1].Sync.Capture = []string{"full", "webhook"} },
		"shortest above every":       func(p []PlanConfig) { p[1].Sync.Shortest = 2 * time.Hour },
		"reconcile below every":      func(p []PlanConfig) { p[1].Sync.ReconcileEvery = time.Minute },
		"no refresh interval":        func(p []PlanConfig) { p[2].Sync.RefreshEvery = 0 },
		"no automatic sync interval": func(p []PlanConfig) { p[0].Sync.Every = 0 },
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			plans := DefaultPlans()
			edit(plans)
			if name == "no plans" {
				plans = nil
			}
			if err := (Billing{Plans: plans}).validate(); err == nil {
				t.Fatal("validate succeeded, want error")
			}
		})
	}
}

func TestFoundryConnectorsValidate(t *testing.T) {
	key := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=" // 32 bytes
	// Connectors are off until a sealing key is configured.
	t.Run("allows no keys", func(t *testing.T) {
		if err := (ConnectorsConfig{}).validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
	})
	t.Run("accepts a current key and an optional previous key", func(t *testing.T) {
		for _, c := range []ConnectorsConfig{
			{CurrentKeyID: "k2", CurrentKey: key},
			{CurrentKeyID: "k2", CurrentKey: key, PreviousKeyID: "k1", PreviousKey: key},
		} {
			if err := c.validate(); err != nil {
				t.Fatalf("validate(%+v): %v", c, err)
			}
		}
	})
	// A half-configured keyring would seal credentials nobody can open after a restart.
	for name, c := range map[string]ConnectorsConfig{
		"a key without an id":           {CurrentKey: key},
		"an id without a key":           {CurrentKeyID: "k1"},
		"a short key":                   {CurrentKeyID: "k1", CurrentKey: "c2hvcnQ="},
		"a previous key without its id": {CurrentKeyID: "k2", CurrentKey: key, PreviousKey: key},
		"a previous key reusing the id": {CurrentKeyID: "k1", CurrentKey: key, PreviousKeyID: "k1", PreviousKey: key},
		"only a previous key":           {PreviousKeyID: "k1", PreviousKey: key},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if err := c.validate(); err == nil {
				t.Fatal("validate succeeded, want error")
			}
		})
	}
}

// Documents are of the types Foundry stores and of bounded size, uploaded through a presigned PUT that expires.
func TestFoundryDocumentsConfig(t *testing.T) {
	t.Run("accepts the defaults", func(t *testing.T) {
		if err := Defaults().Foundry.Documents.validate(); err != nil {
			t.Fatal(err)
		}
	})
	// Each kind of document has its own cap: text 1 MiB, table files and
	// PDFs 3 MiB; a type Foundry does not store has none.
	t.Run("caps each kind of document", func(t *testing.T) {
		d := Defaults().Foundry.Documents
		for mediaType, want := range map[model.FoundryMediaType]int64{
			model.FoundryMediaMarkdown: 1 << 20, model.FoundryMediaMDX: 1 << 20, model.FoundryMediaPlainText: 1 << 20,
			model.FoundryMediaCSV: 3 << 20, model.FoundryMediaTSV: 3 << 20, model.FoundryMediaJSON: 3 << 20, model.FoundryMediaJSONL: 3 << 20,
			model.FoundryMediaParquet: 3 << 20, model.FoundryMediaPDF: 3 << 20, "text/html": 0,
		} {
			if got := d.MaxBytesFor(mediaType); got != want {
				t.Errorf("%s cap = %d, want %d", mediaType, got, want)
			}
		}
	})
	// Each case breaks one bound the upload routes rely on.
	for name, mutate := range map[string]func(*DocumentsConfig){
		"no size cap":                 func(d *DocumentsConfig) { d.MaxBytes = 0 },
		"no table file cap":           func(d *DocumentsConfig) { d.MaxTableBytes = 0 },
		"no PDF cap":                  func(d *DocumentsConfig) { d.MaxPDFBytes = -1 },
		"a table past one message":    func(d *DocumentsConfig) { d.MaxTableBytes = MaxDocumentBytes + 1 },
		"no document cap":             func(d *DocumentsConfig) { d.MaxPerWorkspace = 0 },
		"no upload expiry":            func(d *DocumentsConfig) { d.UploadTTL = 0 },
		"an upload past S3's 7 days":  func(d *DocumentsConfig) { d.UploadTTL = 8 * 24 * time.Hour },
		"a format Foundry can't show": func(d *DocumentsConfig) { d.MediaTypes = []model.FoundryMediaType{"text/html"} },
		"a document past one message": func(d *DocumentsConfig) { d.MaxBytes = MaxDocumentBytes + 1 },
		"no stream cap":               func(d *DocumentsConfig) { d.MaxPerStream = 0 },
		"a stream past one extract":   func(d *DocumentsConfig) { d.MaxPerStream = MaxDocumentsPerStream + 1 },
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			d := Defaults().Foundry.Documents
			mutate(&d)
			if err := d.validate(); err == nil {
				t.Fatal("validated")
			}
		})
	}
}

// Prod's graph caps on a day in October: the month began on the 1st (UTC),
// a person's work may spend $50 of it and work no one started $40.
func ExampleGraphConfig_MonthBudget() {
	g := GraphConfig{MaxJobUSD: 10, MonthlyUSD: 50}
	b := g.MonthBudget(time.Date(2026, 10, 23, 1, 0, 0, 0, time.FixedZone("UTC+2", 2*60*60)))
	fmt.Println(b.Since.Format(time.RFC3339), b.CapFor(new("user-1")), b.CapFor(nil))
	// Output: 2026-10-01T00:00:00Z 50000000 40000000
}

// A question runs under $0.50, 20 model calls and 240 s unless a deployment says
// otherwise, and its attempt allows AskReadyWait for the sidecar, then
// outlasts the agent's wall-time cap by AskAttemptMargin, whatever the cap.
func TestFoundryAskDefaults(t *testing.T) {
	a := Defaults().Foundry.Ask
	if a.MaxMicros() != 500_000 || a.MaxSteps != 20 || a.MaxWallSecs != 240 {
		t.Fatalf("default foundry.ask caps = $%v, %d steps, %ds", a.MaxUSD, a.MaxSteps, a.MaxWallSecs)
	}
	for _, wall := range []int{1, 120, 240, AskWallSecsLimit - 1} {
		a.MaxWallSecs = wall
		if got, wallCap := a.AttemptTimeout(), time.Duration(wall)*time.Second; got != AskReadyWait+wallCap+AskAttemptMargin || got <= wallCap {
			t.Fatalf("attempt timeout at %ds = %s", wall, got)
		}
	}
}

// An attempt is bounded by the wait for the sidecar, the wall-time cap and
// its margin.
func ExampleAskConfig_AttemptTimeout() {
	fmt.Println(AskConfig{MaxWallSecs: 240}.AttemptTimeout())
	// Output: 5m0s
}

// A dollar amount is rounded to micros, not truncated: 2.01 * 1e6 is a hair
// under 2010000 in floating point.
func ExampleAskConfig_MaxMicros() {
	a := AskConfig{MaxUSD: 2.01, MonthlyUSD: 30}
	fmt.Println(a.MaxMicros(), a.MonthlyMicros(), int64(a.MaxUSD*1e6))
	// Output: 2010000 30000000 2009999
}

func TestFoundrySyncLimits(t *testing.T) {
	valid := Defaults().Foundry.Sync
	if err := valid.validateLimits(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*SyncConfig)
		want   string
	}{
		// A snapshot's gzip must fit S3's 5 GiB single PUT.
		"no table bytes":          {func(s *SyncConfig) { s.MaxTableBytes = 0 }, "max_table_bytes"},
		"table bytes over 4 GiB":  {func(s *SyncConfig) { s.MaxTableBytes = MaxSnapshotBytes + 1 }, "max_table_bytes"},
		"table bytes of 4 GiB":    {func(s *SyncConfig) { s.MaxTableBytes = MaxSnapshotBytes }, ""},
		"no source bytes":         {func(s *SyncConfig) { s.MaxSourceBytes = 0 }, "max_source_bytes"},
		"connector under source":  {func(s *SyncConfig) { s.MaxConnectorBytes = s.MaxSourceBytes - 1 }, "max_connector_bytes"},
		"connector of one source": {func(s *SyncConfig) { s.MaxConnectorBytes = s.MaxSourceBytes }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			s := valid
			tc.mutate(&s)
			err := s.validateLimits()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !contains(err.Error(), tc.want)) {
				t.Fatalf("validateLimits() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFoundryS3RoleARN(t *testing.T) {
	for arn, ok := range map[string]bool{
		"": true, "arn:aws:iam::123456789012:role/example-foundry-connector": true, "arn:aws:iam::123456789012:role/path/name": true,
		"arn:aws:iam::12345:role/x": false, "arn:aws:iam::123456789012:user/x": false, "example-foundry-connector": false,
	} {
		err := ConnectorsConfig{S3RoleARN: arn}.validate()
		if (err == nil) != ok {
			t.Errorf("s3_role_arn %q: validate() = %v", arn, err)
		}
	}
}

func TestValidateFoundryAI(t *testing.T) {
	// cloud is a process that makes model calls in cloud with ai as foundry.ai.
	cloud := func(mutate func(*AIConfig)) error {
		a := Defaults().Foundry.AI
		a.Default = helixClaude()
		mutate(&a)
		return a.validate(false, true)
	}
	local := func(mutate func(*AIConfig)) error {
		a := Defaults().Foundry.AI
		mutate(&a)
		return a.validate(true, true)
	}
	wantErr := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil || !contains(err.Error(), want) {
			t.Fatalf("err = %v, want one containing %q", err, want)
		}
	}

	// Hosted Foundry runs on Helix's Claude by default, and may offer owners
	// OpenAI and Azure OpenAI beside Claude.
	t.Run("accepts Helix's Claude in cloud", func(t *testing.T) {
		if err := cloud(func(*AIConfig) {}); err != nil {
			t.Fatal(err)
		}
		if err := cloud(func(a *AIConfig) { a.Kinds = []string{"anthropic", "openai", "azure_openai"} }); err != nil {
			t.Fatal(err)
		}
	})
	// A hosted owner may bring an Amazon Bedrock key, whose endpoint is AWS's
	// own; Helix's default stays Claude.
	t.Run("offers Amazon Bedrock in cloud but not as the default", func(t *testing.T) {
		if err := cloud(func(a *AIConfig) { a.Kinds = []string{"anthropic", "bedrock"} }); err != nil {
			t.Fatal(err)
		}
		wantErr(t, cloud(func(a *AIConfig) { a.Default.Provider, a.Default.Region = "bedrock", "us-east-1" }), "in cloud it must be anthropic")
		if err := local(func(a *AIConfig) {
			a.Kinds = []string{"anthropic", "bedrock"}
			a.Default = AIDefault{Provider: "bedrock", Region: "us-east-1", APIKey: "ABSK-local", Models: helixClaude().Models}
		}); err != nil {
			t.Fatal(err)
		}
	})
	// Cloud's default is Helix's Claude and no other: every workspace that
	// brought no key runs on it.
	t.Run("requires an anthropic default in cloud", func(t *testing.T) {
		wantErr(t, cloud(func(a *AIConfig) { a.Default = AIDefault{} }), "in cloud it must be anthropic")
		wantErr(t, cloud(func(a *AIConfig) { a.Default.Provider = "openai" }), "in cloud it must be anthropic")
	})
	// The key fails the boot of a process that would call the model, never a run.
	t.Run("requires the key where model calls are made", func(t *testing.T) {
		wantErr(t, cloud(func(a *AIConfig) { a.Default.APIKey = "" }), "foundry.ai.default.api_key")
		a := Defaults().Foundry.AI
		a.Default = helixClaude()
		a.Default.APIKey = ""
		if err := a.validate(false, false); err != nil {
			t.Fatalf("the backend alone needs no key: %v", err)
		}
	})
	// A hosted endpoint is dialed from inside Helix's network.
	t.Run("refuses private hosts and Ollama in cloud", func(t *testing.T) {
		wantErr(t, cloud(func(a *AIConfig) { a.AllowPrivateHosts = true }), "local mode only")
		wantErr(t, cloud(func(a *AIConfig) { a.Kinds = []string{"anthropic", "ollama"} }), "cannot offer ollama")
	})
	// Every task needs its model, the default's included.
	t.Run("requires a model per task", func(t *testing.T) {
		wantErr(t, cloud(func(a *AIConfig) { a.Default.Models.Verify = "" }), "draft, ask, extract and verify")
	})
	// The rollout gate offers at least one kind, and the probe's timings
	// must let a probe finish inside its lease.
	t.Run("requires kinds, a call timeout and probe timings", func(t *testing.T) {
		wantErr(t, cloud(func(a *AIConfig) { a.Kinds = nil }), "foundry.ai.kinds")
		wantErr(t, cloud(func(a *AIConfig) { a.CallTimeout = 0 }), "foundry.ai.call_timeout")
		wantErr(t, cloud(func(a *AIConfig) { a.Probe.PerHour = 0 }), "foundry.ai.probe")
		wantErr(t, cloud(func(a *AIConfig) { a.Probe.Lease = a.Probe.Timeout }), "must be at least its timeout")
		// A claim is held past the probe's timeout while the sidecar starts,
		// the call ends and its outcome is recorded.
		wantErr(t, cloud(func(a *AIConfig) { a.Probe.Lease = a.Probe.Timeout + AIProbeOverhead - time.Second }), "must be at least its timeout")
		if err := cloud(func(a *AIConfig) { a.Probe.Lease = a.Probe.Timeout + AIProbeOverhead }); err != nil {
			t.Fatal(err)
		}
	})
	// Locally no provider is a valid install: model work waits for an owner
	// to choose one, and endpoints may be on this machine.
	t.Run("accepts no default and private hosts locally", func(t *testing.T) {
		if err := local(func(a *AIConfig) { a.AllowPrivateHosts = true }); err != nil {
			t.Fatal(err)
		}
		if err := local(func(a *AIConfig) { a.Default = helixClaude() }); err != nil {
			t.Fatal(err)
		}
	})
	// A model or key with no provider is a half-set environment, not none.
	t.Run("refuses a default with settings but no provider", func(t *testing.T) {
		wantErr(t, local(func(a *AIConfig) { a.Default.Models.Draft = "llama3.3" }), "no provider")
		wantErr(t, local(func(a *AIConfig) { a.Default.APIKey = "sk-test" }), "no provider")
		wantErr(t, local(func(a *AIConfig) { a.Default.ContextTokens = 65536 }), "no provider")
	})
}

// A custom OpenAI-compatible server may be offered hosted, but is never
// hosted Foundry's default.
func TestFoundryAICompatibleConfig(t *testing.T) {
	cloud := Defaults().Foundry.AI
	cloud.Default, cloud.Kinds = helixClaude(), []string{"anthropic", "openai_compatible"}
	if err := cloud.validate(false, true); err != nil {
		t.Fatalf("offering a custom server hosted: %v", err)
	}
	cloud.Default = AIDefault{Provider: "openai_compatible", BaseURL: "https://llm.example.com/v1", Models: helixClaude().Models}
	if err := cloud.validate(false, false); err == nil || !contains(err.Error(), "in cloud it must be anthropic") {
		t.Fatalf("a custom server as hosted Foundry's default: %v", err)
	}

}
