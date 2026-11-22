package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func contains(s, substr string) bool { return strings.Contains(s, substr) }

func helixClaude() AIDefault {
	return AIDefault{Provider: "anthropic", APIKey: "test", Models: AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
	}}
}

// every runs each of Foundry's roles but foundry-notify-worker, which needs
// the site's origin (TestFoundryNotifyValidate).
var every = Roles{API: true, Read: true, Events: true, SyncWorker: true, DocumentWorker: true, OntologyWorker: true, GraphWorker: true,
	AskWorker: true, EnrichmentWorker: true, HealWorker: true, AgentWorker: true, SlackWorker: true}

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
	// Off, the heal worker's settings aren't read; on, the defaults serve.
	if c := valid(); c.Foundry.Heal.Enabled {
		t.Fatal("heal is on by default")
	}
	off := valid()
	off.Foundry.Heal.Lease = 0
	if err := off.Validate(Roles{HealWorker: true}); err != nil {
		t.Fatalf("an off heal worker's settings were checked: %v", err)
	}
	on := valid()
	on.Foundry.Heal.Enabled = true
	if err := on.Validate(every); err != nil {
		t.Fatalf("heal on, the defaults: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*Config)
		roles  Roles
		want   string
	}{
		"the sync worker's bucket": {func(c *Config) { c.Foundry.Sync.DataBucket = "" }, Roles{SyncWorker: true}, "foundry.sync.data_bucket"},
		"foundry-read's sidecar":   {func(c *Config) { c.Foundry.Read.AgentAddr = "agent:50051" }, Roles{Read: true}, "loopback"},
		"the door's backend":       {func(c *Config) { c.Foundry.Events.Endpoint.Backend.Address = "" }, Roles{Events: true}, "backend.address"},
		"the API's generation cap": {func(c *Config) { c.Foundry.Ontology.PerHour = 0 }, Roles{API: true}, "foundry.ontology.per_hour"},
		"the API's answer wait":    {func(c *Config) { c.Foundry.Ontology.AnswerDays = 0 }, Roles{API: true}, "foundry.ontology.answer_days"},
		"the worker's answer wait": {func(c *Config) { c.Foundry.Ontology.AnswerDays = 0 }, Roles{OntologyWorker: true}, "foundry.ontology.answer_days"},
		"the worker's question rounds": {func(c *Config) { c.Foundry.Ontology.MaxQuestionRounds = 0 }, Roles{OntologyWorker: true},
			"foundry.ontology.max_question_rounds"},
		"the hosted default's key":    {func(c *Config) { c.Foundry.AI.Default.APIKey = "" }, Roles{GraphWorker: true}, "api_key must be set"},
		"the graph worker's rate":     {func(c *Config) { c.Foundry.Graph.LoadRateLimit.Burst = 5 }, Roles{GraphWorker: true}, "load_rate_limit"},
		"the ask worker's read caps":  {func(c *Config) { c.Foundry.Ask.ReadScanEntities = 0 }, Roles{AskWorker: true}, "read_scan_entities"},
		"the enrichment worker's key": {func(c *Config) { c.Foundry.Enrichment.Provider = EnrichmentContextDev }, Roles{EnrichmentWorker: true}, "api_key"},
		"the shared plan catalog":     {func(c *Config) { c.Billing.Plans = nil }, Roles{}, "at least the Free plan"},
		"a stream's extracts":         {func(c *Config) { c.Foundry.Events.MaxExtracted = c.Foundry.Documents.MaxPerStream + 1 }, Roles{}, "max_extracted"},
		"the heal worker's lease":     {func(c *Config) { c.Foundry.Heal.Enabled, c.Foundry.Heal.Lease = true, time.Second }, Roles{HealWorker: true}, "foundry.heal.lease"},
		"the agent worker's lease":    {func(c *Config) { c.Foundry.Agent.Enabled, c.Foundry.Agent.Lease = true, time.Second }, Roles{AgentWorker: true}, "foundry.agent"},
		"the Assistant's month":       {func(c *Config) { c.Foundry.Agent.Enabled, c.Foundry.Agent.MonthlyUSD = true, 0.1 }, Roles{API: true}, "monthly_usd"},
		"the agent worker's key":      {func(c *Config) { c.Foundry.AI.Default.APIKey = "" }, Roles{AgentWorker: true}, "api_key must be set"},
		"the probe's tool check": {func(c *Config) {
			c.Foundry.Agent.Enabled, c.Foundry.AI.Probe.Lease = true, c.Foundry.AI.Probe.Timeout+AIProbeOverhead+AIToolCheckTimeout-time.Second
		}, Roles{AskWorker: true}, "tool check"},
		"an automation run's lease": {func(c *Config) {
			c.Foundry.Automations.Enabled, c.Foundry.Automations.Lease = true, MinAutomationLease-time.Second
		}, Roles{AutomationWorker: true}, "lease at least 4m0s"},
		"the heal worker's every": {func(c *Config) { c.Foundry.Heal.Enabled, c.Foundry.Heal.Every = true, 0 }, Roles{HealWorker: true}, "foundry.heal"},
		"the heal worker's sidecar": {func(c *Config) { c.Foundry.Heal.Enabled, c.Foundry.Graph.Agent.Addr = true, "agent:50051" }, Roles{HealWorker: true},
			"foundry.graph.agent.addr"},
		"the heal worker's timeout": {func(c *Config) { c.Foundry.Heal.Enabled, c.Foundry.Graph.HelixTimeoutSecs = true, 0 }, Roles{HealWorker: true},
			"helix_timeout_secs"},
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
		"a door":          {func(c *Config) { c.Foundry.Events.Endpoint.PublicURL = "https://ingest.example.com" }, "no API sources HTTP door"},
		"reminders taken": {func(c *Config) { c.Foundry.Reminders.SecretSHA256 = strings.Repeat("a", 64) }, "takes no reminders"},
		"Ollama offered":  {func(c *Config) { c.Foundry.AI.Kinds = []string{"ollama"} }, ""},
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

// Each read of a question is held to the agent's own defaults unless a
// deployment raises them, so a hosted question reads as it did before the
// setting: the agent's schema says what it reads in their place.
func TestFoundryAskReadDefaults(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(contracttest.Dir(t), "ask_request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs struct {
			AskReadLimits struct {
				Properties struct {
					WallMS       struct{ Default int } `json:"wall_ms"`
					ScanEntities struct{ Default int } `json:"scan_entities"`
				} `json:"properties"`
			} `json:"AskReadLimits"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	a, agent := Defaults().Foundry.Ask, schema.Defs.AskReadLimits.Properties
	if a.ReadWallMS != 5000 || a.ReadScanEntities != 20000 || a.ReadWallMS != agent.WallMS.Default || a.ReadScanEntities != agent.ScanEntities.Default {
		t.Fatalf("foundry.ask read_wall_ms %d and read_scan_entities %d, the agent's defaults %d and %d",
			a.ReadWallMS, a.ReadScanEntities, agent.WallMS.Default, agent.ScanEntities.Default)
	}
}

// A read's caps are positive, and its wall time fits its question's, which
// the agent checks too; a read may take the whole question's time.
func TestFoundryAskReadLimits(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*AskConfig)
		want   string
	}{
		"the defaults":                {func(*AskConfig) {}, ""},
		"no wall time":                {func(a *AskConfig) { a.ReadWallMS = 0 }, "read_wall_ms and read_scan_entities must be positive"},
		"a negative wall time":        {func(a *AskConfig) { a.ReadWallMS = -1 }, "read_wall_ms and read_scan_entities must be positive"},
		"no matches":                  {func(a *AskConfig) { a.ReadScanEntities = 0 }, "read_wall_ms and read_scan_entities must be positive"},
		"the question's whole time":   {func(a *AskConfig) { a.ReadWallMS = a.MaxWallSecs * 1000 }, ""},
		"past the question's time":    {func(a *AskConfig) { a.ReadWallMS = a.MaxWallSecs*1000 + 1 }, "foundry.ask.read_wall_ms (240001) must be at most max_wall_secs (240s)"},
		"one ms in a one-second call": {func(a *AskConfig) { a.MaxWallSecs, a.ReadWallMS = 1, 1 }, ""},
		"a local install's caps": {func(a *AskConfig) {
			a.MaxWallSecs, a.ReadWallMS, a.ReadScanEntities = AskWallSecsLimit-1, 30_000, 100_000
		},
			""},
	} {
		t.Run(name, func(t *testing.T) {
			a := Defaults().Foundry.Ask
			tc.mutate(&a)
			err := a.validate()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !contains(err.Error(), tc.want)) {
				t.Fatalf("validate = %v, want %q", err, tc.want)
			}
		})
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

// A sign-in returns only to a callback the broker can match exactly.
func TestFoundrySignInConfig(t *testing.T) {
	for _, tc := range []struct {
		c  ConnectorsConfig
		ok bool
	}{
		{ConnectorsConfig{SignInReturnTo: []string{"https://foundry.helix-db.com/connect/callback",
			"https://local.foundry.helix-db.com:3000/connect/callback"},
			GitHubAppInstallURL: "https://github.com/apps/helix-foundry/installations/new"}, true},
		{ConnectorsConfig{SignInReturnTo: []string{"/connect/callback"}}, false},
		{ConnectorsConfig{SignInReturnTo: []string{"https://foundry.helix-db.com/connect/callback?sign_in=1"}}, false},
		{ConnectorsConfig{GitHubAppInstallURL: "http://github.com/apps/helix-foundry"}, false},
	} {
		if err := tc.c.validate(); (err == nil) != tc.ok {
			t.Errorf("validate(%+v) = %v", tc.c, err)
		}
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

// The notify worker links into the site, so it needs the site's origin,
// checked only where it runs; its windows must make sense.
func TestFoundryNotifyValidate(t *testing.T) {
	notify := Roles{NotifyWorker: true}
	valid := func() Config {
		c := Defaults()
		c.Foundry.Notify.SiteURL = "https://foundry.helix-db.com"
		return c
	}
	if c := valid(); c.Validate(notify) != nil {
		t.Fatalf("defaults with a site: %v", c.Validate(notify))
	}
	if d := Defaults().Foundry.Notify; d.PollInterval != 30*time.Second || d.FollowUpAfter != 24*time.Hour || d.FollowUpWindow != 72*time.Hour ||
		d.MaxAttempts != 5 || d.SiteURL != "" {
		t.Fatalf("defaults = %+v", d)
	}
	for name, tc := range map[string]struct {
		mutate func(*NotifyConfig)
		want   string
	}{
		"no site":            {func(n *NotifyConfig) { n.SiteURL = "" }, "foundry.notify.site_url must be set"},
		"a site with a path": {func(n *NotifyConfig) { n.SiteURL = "https://foundry.helix-db.com/app" }, "foundry.notify.site_url"},
		"a trailing slash":   {func(n *NotifyConfig) { n.SiteURL = "https://foundry.helix-db.com/" }, "must be an origin"},
		"plain HTTP":         {func(n *NotifyConfig) { n.SiteURL = "http://foundry.helix-db.com" }, "HTTPS"},
		"no poll":            {func(n *NotifyConfig) { n.PollInterval = 0 }, "poll_interval"},
		"no attempt":         {func(n *NotifyConfig) { n.MaxAttempts = 0 }, "max_attempts"},
		"no wait":            {func(n *NotifyConfig) { n.FollowUpAfter = 0 }, "follow_up_after"},
		"a shut window":      {func(n *NotifyConfig) { n.FollowUpWindow = n.FollowUpAfter }, "follow_up_window"},
	} {
		t.Run(name, func(t *testing.T) {
			c := valid()
			tc.mutate(&c.Foundry.Notify)
			if err := c.Validate(notify); err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
			if err := c.Validate(Roles{}); err != nil {
				t.Fatalf("checked without its role: %v", err)
			}
		})
	}
	c := valid()
	c.Foundry.Notify.SiteURL = "http://localhost:3000"
	if err := c.Validate(notify); err != nil {
		t.Fatalf("a local site: %v", err)
	}
}

// The reminders' settings are checked wherever Foundry runs: every part
// reads them. Off (no secret) is valid.
func TestFoundryRemindersValidate(t *testing.T) {
	d := Defaults().Foundry.Reminders
	if d.SecretSHA256 != "" || d.MaxNewPerHour != 300 || d.MaxNewPerClientPerHour != 5 || !slices.Equal(d.GapDays, []int{7, 14, 28}) || d.MinGapDays != 7 {
		t.Fatalf("defaults = %+v", d)
	}
	on := func() Config {
		c := Defaults()
		c.Foundry.Reminders.SecretSHA256 = strings.Repeat("0123456789abcdef", 4)
		return c
	}
	for name, c := range map[string]Config{"off": Defaults(), "on": on()} {
		if err := c.Validate(Roles{}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	single := on()
	single.Foundry.Reminders.GapDays = nil
	if err := single.Validate(Roles{}); err != nil {
		t.Fatalf("one reminder only: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*RemindersConfig)
		want   string
	}{
		"a secret, not its hash": {func(r *RemindersConfig) { r.SecretSHA256 = "hunter2" }, "secret_sha256"},
		"an upper-case hash":     {func(r *RemindersConfig) { r.SecretSHA256 = strings.Repeat("A", 64) }, "secret_sha256"},
		"no new reminders":       {func(r *RemindersConfig) { r.MaxNewPerHour = 0 }, "max_new_per_hour"},
		"no client's reminders":  {func(r *RemindersConfig) { r.MaxNewPerClientPerHour = 0 }, "max_new_per_client_per_hour"},
		"a client past the hour": {func(r *RemindersConfig) { r.MaxNewPerClientPerHour = r.MaxNewPerHour + 1 }, "max_new_per_client_per_hour"},
		"no least gap":           {func(r *RemindersConfig) { r.MinGapDays = 0 }, "min_gap_days"},
		"a gap of no days":       {func(r *RemindersConfig) { r.GapDays = []int{7, 0} }, "gap_days"},
		"a gap past a year":      {func(r *RemindersConfig) { r.GapDays = []int{400} }, "gap_days"},
		"too many gaps":          {func(r *RemindersConfig) { r.GapDays = make([]int, 11) }, "at most 10"},
	} {
		t.Run(name, func(t *testing.T) {
			c := on()
			tc.mutate(&c.Foundry.Reminders)
			if err := c.Validate(Roles{}); err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
		})
	}
}

// slackOn is the defaults with the Slack app on and complete, as a hosted
// stamp sets it.
func slackOn() Config {
	c := Defaults()
	c.Foundry.AI.Default = helixClaude()
	c.Foundry.Slack.Enabled = true
	c.Foundry.Slack.ClientID, c.Foundry.Slack.AppID = "1234.5678", "A0123"
	c.Foundry.Slack.ClientSecret, c.Foundry.Slack.SigningSecret = "client-secret", "signing-secret"
	c.Foundry.Slack.SiteURL = "https://foundry.helix-db.com"
	return c
}

// The app is off by default, and its worker's defaults are the reviewed
// ones: a second's poll, three minutes' lease, 16 at once, half of each of
// Slack's limits, and what came from Slack kept 30 days at most.
func TestSlackDefaults(t *testing.T) {
	s := Defaults().Foundry.Slack
	want := SlackConfig{
		HTTP: ServiceHTTPConfig{Listen: ":8104"}, PollInterval: time.Second, Lease: 3 * time.Minute, Concurrency: 16, MaxAttempts: 5,
		RetryBackoff: 10 * time.Second, APIShare: 0.5, BusyWait: 15 * time.Minute,
		Retention: SlackRetentionConfig{Threads: 30 * 24 * time.Hour, Intake: 7 * 24 * time.Hour, Replies: 7 * 24 * time.Hour},
	}
	if s.Enabled || s.InstallAllowlist != nil || s.ClientSecret != "" || s.SigningSecret != "" {
		t.Fatalf("defaults turn the app on or hold a secret: %+v", s)
	}
	if s.HTTP != want.HTTP || s.PollInterval != want.PollInterval || s.Lease != want.Lease || s.Concurrency != want.Concurrency ||
		s.MaxAttempts != want.MaxAttempts || s.RetryBackoff != want.RetryBackoff || s.APIShare != want.APIShare || s.BusyWait != want.BusyWait ||
		s.Retention != want.Retention {
		t.Fatalf("defaults = %+v", s)
	}
	// Off, nothing of it is read, so the defaults serve every role.
	off := slackOn()
	off.Foundry.Slack = Defaults().Foundry.Slack
	if err := off.Validate(Roles{SlackWorker: true, API: true, Events: true}); err != nil {
		t.Fatalf("an off app was checked: %v", err)
	}
	// The worker's defaults outlast a default turn's attempt.
	if d := Defaults().Foundry; d.Slack.BusyWait < d.Agent.AttemptTimeout() {
		t.Fatalf("busy_wait %s is shorter than a turn's attempt %s", d.Slack.BusyWait, d.Agent.AttemptTimeout())
	}
}

// On, the API and the worker need the app's identity, its secrets and the
// site's origin; the worker its own settings; the door, which holds no
// secret, nothing but its gate. Each bound holds at its edge and refuses past
// it, and binds only the roles that read it.
func TestSlackValidate(t *testing.T) {
	worker, api := Roles{SlackWorker: true}, Roles{API: true}
	for name, roles := range map[string]Roles{"the API": api, "the worker": worker, "both": {API: true, SlackWorker: true}} {
		if c := slackOn(); c.Validate(roles) != nil {
			t.Fatalf("%s, complete: %v", name, c.Validate(roles))
		}
	}
	// The door reads only enabled and its gate: no secret, no site.
	door := Defaults()
	door.Foundry.Slack.Enabled = true
	if err := door.Validate(Roles{Events: true}); err != nil {
		t.Fatalf("the door with Slack on and no secrets: %v", err)
	}
	if err := door.Validate(Roles{}); err != nil {
		t.Fatalf("a process of no role with Slack on: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*SlackConfig)
		roles  Roles
		want   string // empty when valid
	}{
		"no client id":           {func(s *SlackConfig) { s.ClientID = "" }, api, "foundry.slack.client_id must be set"},
		"a blank app id":         {func(s *SlackConfig) { s.AppID = " " }, worker, "foundry.slack.app_id must be set"},
		"no client secret":       {func(s *SlackConfig) { s.ClientSecret = "" }, api, "foundry.slack.client_secret must be set"},
		"no signing secret":      {func(s *SlackConfig) { s.SigningSecret = "\t" }, worker, "foundry.slack.signing_secret must be set"},
		"no site":                {func(s *SlackConfig) { s.SiteURL = "" }, api, "foundry.slack.site_url must be set"},
		"a site on plain HTTP":   {func(s *SlackConfig) { s.SiteURL = "http://foundry.helix-db.com" }, api, "HTTPS"},
		"a site with a path":     {func(s *SlackConfig) { s.SiteURL = "https://foundry.helix-db.com/app" }, worker, "foundry.slack.site_url"},
		"a site's trailing /":    {func(s *SlackConfig) { s.SiteURL = "https://foundry.helix-db.com/" }, api, "must be an origin"},
		"a site not absolute":    {func(s *SlackConfig) { s.SiteURL = "foundry.helix-db.com" }, api, "not an absolute origin"},
		"a local site":           {func(s *SlackConfig) { s.SiteURL = "http://localhost:3000" }, api, ""},
		"a site with a port":     {func(s *SlackConfig) { s.SiteURL = "https://local.foundry.helix-db.com:3000" }, api, ""},
		"an allowlist":           {func(s *SlackConfig) { s.InstallAllowlist = []string{"ws-1", "ws-2"} }, api, ""},
		"a blank allowed id":     {func(s *SlackConfig) { s.InstallAllowlist = []string{"ws-1", ""} }, api, "install_allowlist[1]"},
		"a spaced allowed id":    {func(s *SlackConfig) { s.InstallAllowlist = []string{" ws-1"} }, worker, "install_allowlist[0]"},
		"an id allowed twice":    {func(s *SlackConfig) { s.InstallAllowlist = []string{"ws-1", "ws-2", "ws-1"} }, api, `lists "ws-1" twice`},
		"no health listener":     {func(s *SlackConfig) { s.HTTP.Listen = "" }, worker, "foundry.slack.http.listen"},
		"no poll":                {func(s *SlackConfig) { s.PollInterval = 0 }, worker, "poll_interval"},
		"no concurrency":         {func(s *SlackConfig) { s.Concurrency = 0 }, worker, "concurrency"},
		"no attempt":             {func(s *SlackConfig) { s.MaxAttempts = 0 }, worker, "max_attempts"},
		"no backoff":             {func(s *SlackConfig) { s.RetryBackoff = 0 }, worker, "retry_backoff"},
		"a lease short of a run": {func(s *SlackConfig) { s.Lease = 3*time.Minute - 1 }, worker, "foundry.slack.lease is 2m59.999999999s"},
		"the old 2m lease":       {func(s *SlackConfig) { s.Lease = 2 * time.Minute }, worker, "it must be at least 3m0s"},
		"a lease past a run":     {func(s *SlackConfig) { s.Lease = 3 * time.Minute }, worker, ""},
		"no share":               {func(s *SlackConfig) { s.APIShare = 0 }, worker, "api_share is 0"},
		"a negative share":       {func(s *SlackConfig) { s.APIShare = -0.5 }, worker, "api_share"},
		"more than every limit":  {func(s *SlackConfig) { s.APIShare = 1.01 }, worker, "api_share is 1.01"},
		"no share at all (NaN)":  {func(s *SlackConfig) { s.APIShare = math.NaN() }, worker, "api_share is NaN"},
		"every limit, one":       {func(s *SlackConfig) { s.APIShare = 1 }, worker, ""},
		"a wait short of a turn": {func(s *SlackConfig) { s.BusyWait = 5*time.Minute - 1 }, worker, "busy_wait 4m59.999999999s must be at least"},
		"a wait of a turn":       {func(s *SlackConfig) { s.BusyWait = 5 * time.Minute }, worker, ""},
		"threads kept no time":   {func(s *SlackConfig) { s.Retention.Threads = 0 }, worker, "retention.threads is 0s"},
		"threads past 30 days":   {func(s *SlackConfig) { s.Retention.Threads = MaxSlackThreadRetention + 1 }, worker, "at most 720h0m0s"},
		"threads kept a day":     {func(s *SlackConfig) { s.Retention.Threads = 24 * time.Hour }, worker, ""},
		"intake gone in retries": {func(s *SlackConfig) { s.Retention.Intake = time.Hour - 1 }, worker, "retention.intake is 59m59.999999999s"},
		"intake kept an hour":    {func(s *SlackConfig) { s.Retention.Intake = time.Hour }, worker, ""},
		"replies kept no time":   {func(s *SlackConfig) { s.Retention.Replies = 0 }, worker, "retention.replies must be positive"},
	} {
		t.Run(name, func(t *testing.T) {
			c := slackOn()
			tc.mutate(&c.Foundry.Slack)
			if err := c.Validate(tc.roles); tc.want == "" && err != nil || tc.want != "" && (err == nil || !contains(err.Error(), tc.want)) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
			for _, other := range []Roles{{}, {Events: true}, {AgentWorker: true}} {
				if err := c.Validate(other); err != nil {
					t.Fatalf("checked without its role (%+v): %v", other, err)
				}
			}
			// Off, nothing of it is read.
			c.Foundry.Slack.Enabled = false
			if err := c.Validate(tc.roles); err != nil {
				t.Fatalf("checked while off: %v", err)
			}
		})
	}
	// The worker's settings are its own: the API never reads them.
	c := slackOn()
	c.Foundry.Slack.Lease, c.Foundry.Slack.Retention = 0, SlackRetentionConfig{}
	if err := c.Validate(api); err != nil {
		t.Fatalf("the API checked the worker's settings: %v", err)
	}
	// A turn allowed longer needs a longer wait.
	c = slackOn()
	c.Foundry.Agent.MaxWallSecs = 900
	if err := c.Validate(worker); err == nil || !contains(err.Error(), "(16m0s)") {
		t.Fatalf("a wait shorter than a longer turn = %v", err)
	}
}

// A standalone install reaches Slack through Helix's relay: on, its API and
// worker need no app of their own, and it refuses the app's secrets.
func TestSlackLocal(t *testing.T) {
	local := func() Config {
		c := Defaults()
		c.Host.Local = true
		c.Foundry.Sync.DataBucket = "foundry-data"
		c.Foundry.Connectors = ConnectorsConfig{CurrentKeyID: "local", CurrentKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}
		c.Read.Address = "127.0.0.1:50068"
		plans := DefaultPlans()
		c.Billing.Plans = []PlanConfig{{Code: "local", DisplayName: "Local", Kinds: plans[len(plans)-1].Kinds, Sync: plans[len(plans)-1].Sync}}
		c.Foundry.Slack.Enabled = true
		return c
	}
	roles := Roles{API: true, SlackWorker: true}
	if c := local(); c.Validate(roles) != nil {
		t.Fatalf("an install with Slack on: %v", c.Validate(roles))
	}
	for name, mutate := range map[string]func(*SlackConfig){
		"a client secret":  func(s *SlackConfig) { s.ClientSecret = "client-secret" },
		"a signing secret": func(s *SlackConfig) { s.SigningSecret = "signing-secret" },
	} {
		t.Run(name, func(t *testing.T) {
			c := local()
			mutate(&c.Foundry.Slack)
			if err := c.Validate(Roles{}); err == nil || !contains(err.Error(), "no Slack app of its own") {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
	// Its worker's own settings still bind it.
	c := local()
	c.Foundry.Slack.Concurrency = 0
	if err := c.Validate(roles); err == nil || !contains(err.Error(), "concurrency") {
		t.Fatalf("Validate = %v", err)
	}
}

// The shared bot's bounds default to 120 messages an hour, 20 of them one
// Slack user's, and a day for a proposal; hosted, the API, the agent worker
// and the Slack worker check them, each at its edge; off, or on a
// standalone install, where bot mode is refused, nothing does.
func TestSlackBot(t *testing.T) {
	if b := Defaults().Foundry.Slack.Bot; b != (SlackBotConfig{PerHour: 120, PerUserPerHour: 20, ProposalTTL: 24 * time.Hour}) {
		t.Fatalf("defaults = %+v", b)
	}
	readers := []Roles{{API: true}, {AgentWorker: true}, {SlackWorker: true}}
	for name, tc := range map[string]struct {
		mutate func(*SlackBotConfig)
		want   string // empty when valid
	}{
		"no turns an hour":       {func(b *SlackBotConfig) { b.PerHour = 0 }, "per_hour and per_user_per_hour must be positive"},
		"no turns a user":        {func(b *SlackBotConfig) { b.PerUserPerHour = -1 }, "per_hour and per_user_per_hour must be positive"},
		"one each":               {func(b *SlackBotConfig) { b.PerHour, b.PerUserPerHour = 1, 1 }, ""},
		"proposals never wait":   {func(b *SlackBotConfig) { b.ProposalTTL = 0 }, "proposal_ttl is 0s"},
		"proposals past threads": {func(b *SlackBotConfig) { b.ProposalTTL = MaxSlackThreadRetention + 1 }, "at most 720h0m0s"},
		"proposals as threads":   {func(b *SlackBotConfig) { b.ProposalTTL = MaxSlackThreadRetention }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			for _, roles := range readers {
				c := slackOn()
				tc.mutate(&c.Foundry.Slack.Bot)
				if err := c.Validate(roles); tc.want == "" && err != nil || tc.want != "" && (err == nil || !contains(err.Error(), tc.want)) {
					t.Fatalf("Validate(%+v) = %v, want %q", roles, err, tc.want)
				}
				for _, other := range []Roles{{}, {Events: true}} {
					if err := c.Validate(other); err != nil {
						t.Fatalf("checked without its role (%+v): %v", other, err)
					}
				}
				c.Foundry.Slack.Enabled = false
				if err := c.Validate(roles); err != nil {
					t.Fatalf("checked while off: %v", err)
				}
			}
		})
	}
	// A standalone install has no bot mode: its bounds aren't read.
	c := Defaults()
	c.Host.Local = true
	c.Foundry.Slack.Enabled = true
	c.Foundry.Slack.Bot = SlackBotConfig{}
	if err := c.Foundry.Slack.validate(Roles{API: true, AgentWorker: true}, true, c.Foundry.Agent); err != nil {
		t.Fatalf("a standalone install's bot bounds were checked: %v", err)
	}
}

// Slack's routes have a gate of their own on the door, wide for Slack's few
// addresses, checked where the door runs whether or not the app is on: each
// bound holds at its edge and refuses past it, as the send gate's do.
func TestSlackDoorGate(t *testing.T) {
	d := Defaults().Foundry.Events.Endpoint
	if want := (DoorGateConfig{
		AddressRequestsPerSecond: 100, AddressBurstRequests: 200, AddressMaxInFlight: 32, RequestsPerSecond: 200, BurstRequests: 400,
		MaxInFlight: 64, MaxBytesInFlight: 64 << 20,
	}); d.Slack != want {
		t.Fatalf("defaults = %+v", d.Slack)
	}
	if want := (DoorGateConfig{
		AddressRequestsPerSecond: 50, AddressBurstRequests: 100, AddressMaxInFlight: 8, RequestsPerSecond: 200, BurstRequests: 400,
		MaxInFlight: 32, MaxBytesInFlight: 32 << 20,
	}); d.SendGate() != want {
		t.Fatalf("SendGate() = %+v", d.SendGate())
	}
	door := Roles{Events: true}
	const body = SlackInteractivityBodyBytes
	for name, tc := range map[string]struct {
		mutate func(*DoorGateConfig)
		want   string // empty when valid
	}{
		"no address rate":        {func(g *DoorGateConfig) { g.AddressRequestsPerSecond = 0 }, "foundry.events.endpoint.slack.address_requests_per_second must be positive"},
		"a negative burst":       {func(g *DoorGateConfig) { g.AddressBurstRequests = -1 }, "foundry.events.endpoint.slack.address_burst_requests must be positive"},
		"no address place":       {func(g *DoorGateConfig) { g.AddressMaxInFlight = 0 }, "foundry.events.endpoint.slack.address_max_in_flight must be positive"},
		"no pod rate":            {func(g *DoorGateConfig) { g.RequestsPerSecond = 0 }, "foundry.events.endpoint.slack.requests_per_second must be positive"},
		"no pod burst":           {func(g *DoorGateConfig) { g.BurstRequests = 0 }, "foundry.events.endpoint.slack.burst_requests must be positive"},
		"no place":               {func(g *DoorGateConfig) { g.MaxInFlight = 0 }, "foundry.events.endpoint.slack.max_in_flight must be positive"},
		"no bytes":               {func(g *DoorGateConfig) { g.MaxBytesInFlight = 0 }, "foundry.events.endpoint.slack.max_bytes_in_flight must be positive"},
		"bytes short of a click": {func(g *DoorGateConfig) { g.MaxBytesInFlight = body - 1 }, "foundry.events.endpoint.slack.max_bytes_in_flight must be at least max_body_bytes"},
		"an address as wide as the pod": {func(g *DoorGateConfig) { g.AddressMaxInFlight = g.MaxInFlight },
			"foundry.events.endpoint.slack.address_max_in_flight is 64; it must be below max_in_flight (64)"},
		"an address one place short": {func(g *DoorGateConfig) { g.AddressMaxInFlight = g.MaxInFlight - 1; g.MaxBytesInFlight = 63*body + 1 }, ""},
		"an address's bodies filling the pod's bytes": {func(g *DoorGateConfig) { g.MaxBytesInFlight = 32 * body },
			"foundry.events.endpoint.slack.address_max_in_flight is 32"},
		"an address's bodies a byte short": {func(g *DoorGateConfig) { g.MaxBytesInFlight = 32*body + 1 }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			c := Defaults()
			tc.mutate(&c.Foundry.Events.Endpoint.Slack)
			if err := c.Validate(door); tc.want == "" && err != nil || tc.want != "" && (err == nil || !contains(err.Error(), tc.want)) {
				t.Fatalf("Validate = %v, want %q", err, tc.want)
			}
			if err := c.Validate(Roles{SlackWorker: true}); err != nil {
				t.Fatalf("checked without the door: %v", err)
			}
		})
	}
	// The send gate's bounds keep their own names and their order: the
	// endpoint's own checks, then its gate's, then Slack's.
	c := Defaults()
	c.Foundry.Events.Endpoint.MaxInFlight, c.Foundry.Events.Endpoint.Slack.MaxInFlight = 0, 0
	if err := c.Validate(door); err == nil || err.Error() != "foundry.events.endpoint.max_in_flight must be positive" {
		t.Fatalf("Validate = %v", err)
	}
	c = Defaults()
	c.Foundry.Events.Endpoint.MaxBodyBytes, c.Foundry.Events.Endpoint.AddressMaxInFlight = MaxEventSendBytes+1, 0
	if err := c.Validate(door); err == nil || !contains(err.Error(), "foundry.events.endpoint.max_body_bytes is 4194305") {
		t.Fatalf("Validate = %v", err)
	}
	for want, mutate := range map[string]func(*EventsEndpointConfig){
		"grpc.listen and http.listen must be set": func(e *EventsEndpointConfig) { e.HTTP.Listen, e.Slack.MaxInFlight = "", 0 },
		"validated_for is 0s":                     func(e *EventsEndpointConfig) { e.ValidatedFor, e.Slack.MaxInFlight = 0, 0 },
	} {
		c = Defaults()
		mutate(&c.Foundry.Events.Endpoint)
		if err := c.Validate(door); err == nil || !contains(err.Error(), want) {
			t.Fatalf("Validate = %v, want %q", err, want)
		}
	}
	c = Defaults()
	c.Foundry.Events.Endpoint.MaxBytesInFlight = c.Foundry.Events.Endpoint.MaxBodyBytes - 1
	if err := c.Validate(door); err == nil || err.Error() != "foundry.events.endpoint.max_bytes_in_flight must be at least max_body_bytes, or the largest body is always refused" {
		t.Fatalf("Validate = %v", err)
	}
	c = Defaults()
	c.Foundry.Events.Endpoint.AddressMaxInFlight = 16
	if err := c.Validate(door); err == nil || err.Error() !=
		"foundry.events.endpoint.address_max_in_flight is 16; it must be below max_in_flight (32), and that many bodies of max_body_bytes below max_bytes_in_flight (33554432), so one address never holds all of a pod's room" {
		t.Fatalf("Validate = %v", err)
	}
}

// Channel messages and replies in threads have gates of their own, apart
// from the bot's: the reviewed defaults, checked by name where the door runs
// whether or not the app is on, for event bodies only, after Slack's gate.
func TestSlackClassGates(t *testing.T) {
	d := Defaults().Foundry.Events.Endpoint
	if want := (DoorGateConfig{
		AddressRequestsPerSecond: 200, AddressBurstRequests: 400, AddressMaxInFlight: 8, RequestsPerSecond: 400, BurstRequests: 800,
		MaxInFlight: 16, MaxBytesInFlight: 32 << 20,
	}); d.SlackFeed != want {
		t.Fatalf("feed defaults = %+v", d.SlackFeed)
	}
	if want := (DoorGateConfig{
		AddressRequestsPerSecond: 50, AddressBurstRequests: 100, AddressMaxInFlight: 4, RequestsPerSecond: 100, BurstRequests: 200,
		MaxInFlight: 8, MaxBytesInFlight: 4 << 20,
	}); d.SlackReply != want {
		t.Fatalf("reply defaults = %+v", d.SlackReply)
	}
	door := Roles{Events: true}
	const body = SlackEventsBodyBytes
	for name, gate := range map[string]func(*EventsEndpointConfig) *DoorGateConfig{
		"slack_feed":  func(e *EventsEndpointConfig) *DoorGateConfig { return &e.SlackFeed },
		"slack_reply": func(e *EventsEndpointConfig) *DoorGateConfig { return &e.SlackReply },
	} {
		prefix := "foundry.events.endpoint." + name
		for what, tc := range map[string]struct {
			mutate func(*DoorGateConfig)
			want   string // empty when valid
		}{
			"no place":                 {func(g *DoorGateConfig) { g.MaxInFlight = 0 }, prefix + ".max_in_flight must be positive"},
			"no pod rate":              {func(g *DoorGateConfig) { g.RequestsPerSecond = 0 }, prefix + ".requests_per_second must be positive"},
			"bytes short of an event":  {func(g *DoorGateConfig) { g.MaxBytesInFlight = body - 1 }, prefix + ".max_bytes_in_flight must be at least"},
			"an address as the pod":    {func(g *DoorGateConfig) { g.AddressMaxInFlight = g.MaxInFlight }, prefix + ".address_max_in_flight is"},
			"one event's bytes":        {func(g *DoorGateConfig) { g.AddressMaxInFlight, g.MaxInFlight, g.MaxBytesInFlight = 1, 2, body+1 }, ""},
			"a click's bytes not read": {func(g *DoorGateConfig) { g.AddressMaxInFlight, g.MaxBytesInFlight = 1, SlackInteractivityBodyBytes-1 }, ""},
		} {
			t.Run(name+"/"+what, func(t *testing.T) {
				c := Defaults()
				tc.mutate(gate(&c.Foundry.Events.Endpoint))
				if err := c.Validate(door); tc.want == "" && err != nil || tc.want != "" && (err == nil || !contains(err.Error(), tc.want)) {
					t.Fatalf("Validate = %v, want %q", err, tc.want)
				}
				if err := c.Validate(Roles{SlackWorker: true}); err != nil {
					t.Fatalf("checked without the door: %v", err)
				}
			})
		}
	}
	// Slack's gate first, then the feed's, then the replies'.
	c := Defaults()
	e := &c.Foundry.Events.Endpoint
	e.Slack.MaxInFlight, e.SlackFeed.MaxInFlight, e.SlackReply.MaxInFlight = 0, 0, 0
	for _, want := range []string{"slack.max_in_flight", "slack_feed.max_in_flight", "slack_reply.max_in_flight"} {
		if err := c.Validate(door); err == nil || err.Error() != "foundry.events.endpoint."+want+" must be positive" {
			t.Fatalf("Validate = %v, want %s", err, want)
		}
		switch want {
		case "slack.max_in_flight":
			e.Slack.MaxInFlight = Defaults().Foundry.Events.Endpoint.Slack.MaxInFlight
		case "slack_feed.max_in_flight":
			e.SlackFeed.MaxInFlight = Defaults().Foundry.Events.Endpoint.SlackFeed.MaxInFlight
		}
	}
}

// The feed and the follow-ups are off by default, hosted's stance until
// Slack confirms, and their settings are the reviewed ones: Slack's pace for
// apps outside its Marketplace, 30 days of replies, a feed's data deleted a
// week after its install is gone, and a day of follow-ups.
func TestSlackFeedDefaults(t *testing.T) {
	s := Defaults().Foundry.Slack
	if s.Feed.Enabled || s.Follow.Enabled || s.ReadsHistory() {
		t.Fatalf("defaults read history: %+v %+v", s.Feed, s.Follow)
	}
	if want := (SlackFeedConfig{
		Spacing: 2 * time.Minute, ReadBudget: SlackFeedReadBudget{Rows: 50_000, Bytes: 64 << 20}, MaxBufferedRows: 200_000, BackendInFlight: 8,
		DirectoryEvery: time.Hour, RedocumentEvery: 24 * time.Hour, CloseAt: 10 * time.Minute, GapThreadLookback: 24 * time.Hour,
		OrphanRetention: 168 * time.Hour,
	}); s.Feed != want {
		t.Fatalf("feed = %+v", s.Feed)
	}
	if s.Follow != (SlackFollowConfig{Window: 24 * time.Hour}) {
		t.Fatalf("follow = %+v", s.Follow)
	}
	if want := (SlackBackfillConfig{
		HistoryPerMinute: 1, RepliesPerMinute: 1, PageLimit: 15, RepliesDays: 30, GapCallsPerDay: 60, MaxAttempts: 5, RetryBackoff: time.Minute,
		PollInterval: 5 * time.Second,
	}); s.Backfill != want {
		t.Fatalf("backfill = %+v", s.Backfill)
	}
	if want := (SlackRelayConfig{FeedTTL: 24 * time.Hour, FeedBatch: 500, FeedBatchBytes: 4 << 20, FeedMaxRows: 100_000}); s.Relay != want {
		t.Fatalf("relay = %+v", s.Relay)
	}
	// Both on, the defaults pass every role.
	c := slackOn()
	c.Foundry.Sync.DataBucket = "foundry-data"
	c.Foundry.Connectors = ConnectorsConfig{CurrentKeyID: "k1", CurrentKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}
	c.Foundry.Slack.Feed.Enabled, c.Foundry.Slack.Follow.Enabled = true, true
	if err := c.Validate(every); err != nil {
		t.Fatalf("defaults with both on: %v", err)
	}
	if MaxSlackFeedBackendInFlight != 12 || MinSlackFeedCloseAt != 6*time.Minute || MaxSlackAppHistoryDays != 90 {
		t.Fatalf("bounds = %d, %s, %d", MaxSlackFeedBackendInFlight, MinSlackFeedCloseAt, MaxSlackAppHistoryDays)
	}
}

// The feed and the follow-ups need the app; on, each bound holds at its edge
// and refuses past it, binds the API and the sync and Slack workers only, and
// binds only while what reads it is on: the feed's own settings while the
// feed is, the follow window while the follow-ups are, and the backend's
// room and the relay's while either is.
func TestSlackFeedValidate(t *testing.T) {
	for name, mutate := range map[string]func(*SlackConfig){
		"the feed":       func(s *SlackConfig) { s.Feed.Enabled = true },
		"the follow-ups": func(s *SlackConfig) { s.Follow.Enabled = true },
	} {
		c := Defaults()
		mutate(&c.Foundry.Slack)
		for _, roles := range []Roles{{}, {Events: true}, {SlackWorker: true}} {
			if err := c.Validate(roles); err == nil || err.Error() != "foundry.slack.feed.enabled and foundry.slack.follow.enabled need foundry.slack.enabled" {
				t.Fatalf("%s without the app (%+v) = %v", name, roles, err)
			}
		}
	}
	type on struct{ feed, follow bool }
	feed, follow, both := on{feed: true}, on{follow: true}, on{true, true}
	for name, tc := range map[string]struct {
		mutate func(*SlackConfig)
		want   string // empty when valid
		off    on     // a mode in which the bound isn't read: valid there
	}{
		"no backend room":            {func(s *SlackConfig) { s.Feed.BackendInFlight = 0 }, "feed.backend_in_flight is 0; it must be from 1 to 12", on{}},
		"one backend place":          {func(s *SlackConfig) { s.Feed.BackendInFlight = 1 }, "", on{}},
		"half the pool":              {func(s *SlackConfig) { s.Feed.BackendInFlight = 12 }, "", on{}},
		"past half the pool":         {func(s *SlackConfig) { s.Feed.BackendInFlight = 13 }, "feed.backend_in_flight is 13", on{}},
		"no follow window":           {func(s *SlackConfig) { s.Follow.Window = 0 }, "follow.window is 0s", feed},
		"a window past the thread":   {func(s *SlackConfig) { s.Follow.Window = s.Retention.Threads + 1 }, "at most foundry.slack.retention.threads (720h0m0s)", feed},
		"a window of the thread":     {func(s *SlackConfig) { s.Follow.Window = s.Retention.Threads }, "", on{}},
		"a window past a short keep": {func(s *SlackConfig) { s.Retention.Threads = time.Hour }, "follow.window is 24h0m0s", feed},
		"no relay ttl":               {func(s *SlackConfig) { s.Relay.FeedTTL = 0 }, "foundry.slack.relay feed_ttl", on{}},
		"no relay batch":             {func(s *SlackConfig) { s.Relay.FeedBatch = 0 }, "feed_batch and feed_max_rows", on{}},
		"no relay rows":              {func(s *SlackConfig) { s.Relay.FeedMaxRows = 0 }, "feed_max_rows must be positive", on{}},
		"a relay batch short":        {func(s *SlackConfig) { s.Relay.FeedBatchBytes = SlackEventsBodyBytes - 1 }, "relay.feed_batch_bytes is 524287", on{}},
		"a relay batch of one":       {func(s *SlackConfig) { s.Relay.FeedBatchBytes = SlackEventsBodyBytes }, "", on{}},
		"no spacing":                 {func(s *SlackConfig) { s.Feed.Spacing = 0 }, "foundry.slack.feed spacing", follow},
		"no fold rows":               {func(s *SlackConfig) { s.Feed.ReadBudget.Rows = 0 }, "read_budget.rows", follow},
		"no buffer":                  {func(s *SlackConfig) { s.Feed.MaxBufferedRows = 0 }, "max_buffered_rows", follow},
		"a lookback into the future": {func(s *SlackConfig) { s.Feed.GapThreadLookback = -1 }, "gap_thread_lookback not negative", follow},
		"no lookback":                {func(s *SlackConfig) { s.Feed.GapThreadLookback = 0 }, "", on{}},
		"orphans gone at once":       {func(s *SlackConfig) { s.Feed.OrphanRetention = 0 }, "orphan_retention", follow},
		"a fold short of a row":      {func(s *SlackConfig) { s.Feed.ReadBudget.Bytes = SlackEventsBodyBytes - 1 }, "read_budget.bytes is 524287", follow},
		"a fold of one row":          {func(s *SlackConfig) { s.Feed.ReadBudget.Bytes = SlackEventsBodyBytes }, "", on{}},
		"a directory too often":      {func(s *SlackConfig) { s.Feed.DirectoryEvery = 5*time.Minute - 1 }, "directory_every is 4m59.999999999s", follow},
		"a directory at 5m":          {func(s *SlackConfig) { s.Feed.DirectoryEvery = 5 * time.Minute }, "", on{}},
		"documents too often":        {func(s *SlackConfig) { s.Feed.RedocumentEvery = time.Hour - 1 }, "redocument_every is 59m59.999999999s", follow},
		"documents hourly":           {func(s *SlackConfig) { s.Feed.RedocumentEvery = time.Hour }, "", on{}},
		"a close before retries":     {func(s *SlackConfig) { s.Feed.CloseAt = 6*time.Minute - 1 }, "close_at is 5m59.999999999s; it must be from 6m0s", follow},
		"a close past them":          {func(s *SlackConfig) { s.Feed.CloseAt = 6 * time.Minute }, "", on{}},
		"a close the next day":       {func(s *SlackConfig) { s.Feed.CloseAt = 24 * time.Hour }, "close_at is 24h0m0s", follow},
		"a close just before":        {func(s *SlackConfig) { s.Feed.CloseAt = 24*time.Hour - 1 }, "", on{}},
		"no history calls":           {func(s *SlackConfig) { s.Backfill.HistoryPerMinute = 0 }, "history_per_minute and replies_per_minute", follow},
		"history past Slack's":       {func(s *SlackConfig) { s.Backfill.HistoryPerMinute = 51 }, "from 1 to 50", follow},
		"history at Marketplace's":   {func(s *SlackConfig) { s.Backfill.HistoryPerMinute = 50 }, "", on{}},
		"no replies calls":           {func(s *SlackConfig) { s.Backfill.RepliesPerMinute = 0 }, "replies_per_minute", follow},
		"replies past Slack's":       {func(s *SlackConfig) { s.Backfill.RepliesPerMinute = 51 }, "replies_per_minute", follow},
		"replies at Marketplace's":   {func(s *SlackConfig) { s.Backfill.RepliesPerMinute = 50 }, "", on{}},
		"an empty page":              {func(s *SlackConfig) { s.Backfill.PageLimit = 0 }, "page_limit is 0", follow},
		"a page past Slack's":        {func(s *SlackConfig) { s.Backfill.PageLimit = 1000 }, "page_limit is 1000; it must be from 1 to 999", follow},
		"the largest page":           {func(s *SlackConfig) { s.Backfill.PageLimit = 999 }, "", on{}},
		"no replies read":            {func(s *SlackConfig) { s.Backfill.RepliesDays = 0 }, "replies_days is 0", follow},
		"replies past the window":    {func(s *SlackConfig) { s.Backfill.RepliesDays = 91 }, "replies_days is 91; it must be from 1 to 90", follow},
		"replies for the window":     {func(s *SlackConfig) { s.Backfill.RepliesDays = 90 }, "", on{}},
		"no gap repairs":             {func(s *SlackConfig) { s.Backfill.GapCallsPerDay = 0 }, "", on{}},
		"a negative gap budget":      {func(s *SlackConfig) { s.Backfill.GapCallsPerDay = -1 }, "gap_calls_per_day not negative", follow},
		"no backfill attempt":        {func(s *SlackConfig) { s.Backfill.MaxAttempts = 0 }, "foundry.slack.backfill max_attempts", follow},
		"no backfill backoff":        {func(s *SlackConfig) { s.Backfill.RetryBackoff = 0 }, "retry_backoff and poll_interval", follow},
		"no backfill poll":           {func(s *SlackConfig) { s.Backfill.PollInterval = 0 }, "poll_interval must be positive", follow},
	} {
		t.Run(name, func(t *testing.T) {
			for _, mode := range []on{feed, follow, both} {
				c := slackOn()
				c.Foundry.Sync.DataBucket = "foundry-data"
				c.Foundry.Connectors = ConnectorsConfig{CurrentKeyID: "k1", CurrentKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}
				c.Foundry.Slack.Feed.Enabled, c.Foundry.Slack.Follow.Enabled = mode.feed, mode.follow
				tc.mutate(&c.Foundry.Slack)
				want := tc.want
				if mode == tc.off {
					want = ""
				}
				for _, roles := range []Roles{{API: true}, {SyncWorker: true}, {SlackWorker: true}} {
					if err := c.Validate(roles); want == "" && err != nil || want != "" && (err == nil || !contains(err.Error(), want)) {
						t.Fatalf("%+v, %+v: Validate = %v, want %q", mode, roles, err, want)
					}
				}
				for _, other := range []Roles{{}, {Events: true}, {AgentWorker: true}, {GraphWorker: true}} {
					if err := c.Validate(other); err != nil {
						t.Fatalf("%+v: checked without its role (%+v): %v", mode, other, err)
					}
				}
				// Neither on, nothing of it is read.
				c.Foundry.Slack.Feed.Enabled, c.Foundry.Slack.Follow.Enabled = false, false
				if err := c.Validate(Roles{API: true, SyncWorker: true}); err != nil {
					t.Fatalf("checked while off: %v", err)
				}
			}
		})
	}
}

// The app reads channel messages, and asks Slack for what that takes, while
// either the feed or the follow-ups is on.
func ExampleSlackConfig_ReadsHistory() {
	for _, s := range []SlackConfig{
		{},
		{Feed: SlackFeedConfig{Enabled: true}},
		{Follow: SlackFollowConfig{Enabled: true}},
		{Feed: SlackFeedConfig{Enabled: true}, Follow: SlackFollowConfig{Enabled: true}},
	} {
		fmt.Println(s.ReadsHistory())
	}
	// Output:
	// false
	// true
	// true
	// true
}

// Before Slack authorizes the app's commercial distribution, a stamp lets
// only its design partners' workspaces add it; with no list, every one.
func ExampleSlackConfig_InstallAllowed() {
	open := SlackConfig{}
	partners := SlackConfig{InstallAllowlist: []string{"ws-acme"}}
	fmt.Println(open.InstallAllowed("ws-acme"), open.InstallAllowed("ws-globex"))
	fmt.Println(partners.InstallAllowed("ws-acme"), partners.InstallAllowed("ws-globex"), partners.InstallAllowed(""))
	// Output:
	// true true
	// true false false
}
