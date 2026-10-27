// Package config is Foundry's configuration: what its API, services and
// workers read. A host loads it from its own configuration under fixed keys
// (koanf tags, with HELIX_ environment overrides), so the same settings mean
// the same thing in every deployment:
//
//	foundry.*                Foundry           connectors, sync, ontologies, AI, documents, graph, read, ask, enrichment, events
//	billing.autumn.foundry.* Billing           the plan catalog and the checkout return page
//	backend.foundry_read.*   Read              where the API reaches foundry-read
//
// Host is set by the host itself, never loaded. Defaults is every setting's
// default, and Validate checks a process's settings before it starts.
package config

import (
	"encoding/base64"
	"fmt"
	"math"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Config is everything Foundry reads of its configuration.
type Config struct {
	// Foundry is the foundry block.
	Foundry Foundry
	// Billing is the plan catalog, in rank order with the free plan first,
	// and the page checkout returns to.
	Billing Billing
	// Read is where the API reaches the foundry-read service.
	Read ReadClient
	// Host is what the host running Foundry says of itself.
	Host Host
}

// Host is what the host running Foundry sets, rather than loads.
type Host struct {
	// Local is a standalone install on one machine: one owner, nothing sold,
	// every model provider kind, Ollama included, open to the owner, and a
	// HelixDB that checks no gateway key.
	Local bool
	// GatewayKey is the bearer token Foundry's graph queries send to the
	// databases' gateway. Without one they are refused, unless Local, which
	// sends none.
	GatewayKey string
	// DetectedAI is set only by a local install whose foundry.ai.default
	// names no provider: the default it found on its machine, which may
	// change while it runs (it looks again as an owner opens Settings › AI
	// models). Every resolve reads it. nil hosted.
	DetectedAI *LiveAIDefault
}

// LiveAIDefault is a default model provider that may change while the
// process runs: Store replaces it, and every reader sees the new one.
type LiveAIDefault struct {
	v atomic.Pointer[DetectedAIDefault]
}

// DetectedAIDefault is a default found on the machine.
type DetectedAIDefault struct {
	// Default is the provider found, read as foundry.ai.default is; its
	// Provider is "" when none was.
	Default AIDefault
	// Summary says, for the owner, what is used and why, or what was looked
	// for and how to set a provider up. It never holds a key.
	Summary string
	At      time.Time
}

// Load is the default last stored, or none found yet (the zero value).
func (l *LiveAIDefault) Load() DetectedAIDefault {
	if d := l.v.Load(); d != nil {
		return *d
	}
	return DetectedAIDefault{}
}

// Store makes d the default.
func (l *LiveAIDefault) Store(d DetectedAIDefault) { l.v.Store(&d) }

// Foundry is the foundry block: connectors and their credentials, syncs,
// ontologies and their models, documents, graph loads, reads, questions,
// People enrichment and event sources.
type Foundry struct {
	Connectors ConnectorsConfig `koanf:"connectors"`
	Sync       SyncConfig       `koanf:"sync"`
	Ontology   OntologyConfig   `koanf:"ontology"`
	AI         AIConfig         `koanf:"ai"`
	Documents  DocumentsConfig  `koanf:"documents"`
	Graph      GraphConfig      `koanf:"graph"`
	Read       ReadConfig       `koanf:"read"`
	Ask        AskConfig        `koanf:"ask"`
	Enrichment EnrichmentConfig `koanf:"enrichment"`
	Events     EventsConfig     `koanf:"events"`
}

// EnrichmentProvider is who the foundry-enrichment-worker looks people
// up with.
type EnrichmentProvider string

const (
	// EnrichmentContextDev calls context.dev with api_key.
	EnrichmentContextDev EnrichmentProvider = "contextdev"
	// EnrichmentFake answers in process, from the clue's text: no
	// person's data leaves Helix.
	EnrichmentFake EnrichmentProvider = "fake"
)

// EnrichmentConfig drives People enrichment: a load whose version
// enriches a type looks its people up while the job is parked at
// provisioning, and the foundry-enrichment-worker, the only holder of the
// provider's key, makes the calls.
type EnrichmentConfig struct {
	HTTP ServiceHTTPConfig `koanf:"http"`
	// Enabled is the deployment's switch, not the users': off, loads make
	// no calls and apply the profiles earlier loads found. A deployment sets
	// it only where it runs the worker.
	Enabled  bool               `koanf:"enabled"`
	Provider EnrichmentProvider `koanf:"provider"`
	// BaseURL is context.dev's API root; APIKey its key, which only the
	// worker's process should hold.
	BaseURL string `koanf:"base_url"`
	APIKey  string `koanf:"api_key"`
	// MinScore is the least match score the accept gate takes.
	MinScore int `koanf:"min_score"`
	// Concurrency is the calls a replica makes at once before any 429.
	Concurrency int `koanf:"concurrency"`
	// MaxRunning is how many steps run at once, at most one per workspace.
	MaxRunning int `koanf:"max_running"`
	// ClaimWait is how long a queued step waits for a worker; LoadWait how
	// long a claimed one calls; Grace how long past LoadWait the load waits
	// for it to publish.
	ClaimWait time.Duration `koanf:"claim_wait"`
	LoadWait  time.Duration `koanf:"load_wait"`
	Grace     time.Duration `koanf:"grace"`
	// MonthlyCreditsMax is the most an owner may set as a workspace's
	// monthly cap; GlobalMonthlyCredits Helix's across workspaces.
	MonthlyCreditsMax    int64 `koanf:"monthly_credits_max"`
	GlobalMonthlyCredits int64 `koanf:"global_monthly_credits"`
	// RawRetentionDays keeps the raw calls; NotFoundDays is how long a
	// person not found is not looked up again.
	RawRetentionDays int `koanf:"raw_retention_days"`
	NotFoundDays     int `koanf:"not_found_days"`
	// PollInterval is how often the worker claims and sweeps; Lease how long
	// a claimed step stays its own without a renewal.
	PollInterval time.Duration `koanf:"poll_interval"`
	Lease        time.Duration `koanf:"lease"`
	// MaxPeople is the most people the graph worker plans for one load's
	// step: it holds enrichment.PersonBytes (49) of memory per person, so
	// 2,000,000 people take about 90 MB live (220 MB peak RSS measured) of
	// its container's 512Mi. A load with more fails its step and goes on
	// with the newest profiles.
	MaxPeople int `koanf:"max_people"`
}

// EnrichmentCallTimeout bounds one context.dev call; the request asks
// context.dev to give up 10s sooner.
const EnrichmentCallTimeout = 30 * time.Second

func (e EnrichmentConfig) validate() error {
	switch {
	case e.HTTP.Listen == "":
		return fmt.Errorf("foundry.enrichment.http.listen must be set when 'foundry-enrichment-worker' is in tasks")
	case e.Provider != EnrichmentContextDev && e.Provider != EnrichmentFake:
		return fmt.Errorf("foundry.enrichment.provider is %q; it must be contextdev or fake", e.Provider)
	case e.Provider == EnrichmentContextDev && (e.APIKey == "" || e.BaseURL == ""):
		return fmt.Errorf("foundry.enrichment api_key and base_url must be set for the contextdev provider")
	case e.MinScore < 0 || e.MinScore > 100:
		return fmt.Errorf("foundry.enrichment.min_score must be 0 to 100")
	case e.Concurrency < 4 || e.MaxRunning < 1:
		return fmt.Errorf("foundry.enrichment concurrency must be at least 4 and max_running at least 1")
	case e.ClaimWait <= 0 || e.LoadWait <= 0 || e.Grace <= 0 || e.PollInterval <= 0:
		return fmt.Errorf("foundry.enrichment claim_wait, load_wait, grace and poll_interval must be positive")
	case e.Lease < 2*EnrichmentCallTimeout:
		// Calls stop a call's timeout before the lease could lapse; the rest is the time between renewals.
		return fmt.Errorf("foundry.enrichment.lease %s must be at least twice a call's timeout (%s)", e.Lease, EnrichmentCallTimeout)
	case e.MonthlyCreditsMax < 1 || e.GlobalMonthlyCredits < e.MonthlyCreditsMax:
		return fmt.Errorf("foundry.enrichment monthly_credits_max must be positive and global_monthly_credits at least it")
	case e.RawRetentionDays < 1 || e.NotFoundDays < 1:
		return fmt.Errorf("foundry.enrichment raw_retention_days and not_found_days must be positive")
	}
	return nil
}

// ReadConfig drives the foundry-read service, which reads Foundry
// ontology loads for the backend through the ontology agent sidecar of its
// pod, the sidecar reaching each database through a read-only proxy.
type ReadConfig struct {
	GRPC ServiceGRPCConfig `koanf:"grpc"`
	HTTP ServiceHTTPConfig `koanf:"http"`
	// AgentAddr is the sidecar's loopback gRPC address.
	AgentAddr string `koanf:"agent_addr"`
}

// DocumentsConfig bounds the documents a workspace uploads. The bytes go
// straight to foundry.sync.data_bucket through a presigned PUT.
type DocumentsConfig struct {
	// MaxBytes caps one text document (Markdown, MDX, plain text),
	// MaxTableBytes one table file and MaxPDFBytes one PDF (MaxBytesFor),
	// checked when it is created and again against the object S3 holds;
	// :content and :bytes return a document whole up to its cap. Extraction
	// reads at most MaxBytes of text. Each is at most MaxDocumentBytes.
	MaxBytes      int64 `koanf:"max_bytes"`
	MaxTableBytes int64 `koanf:"max_table_bytes"`
	MaxPDFBytes   int64 `koanf:"max_pdf_bytes"`
	// MediaTypes are the Content-Types accepted, of those Foundry stores
	// (model.FoundryMediaType); every one by default.
	MediaTypes []model.FoundryMediaType `koanf:"media_types"`
	// MaxPerWorkspace caps a workspace's uploads: those uploaded, and those
	// waiting for their bytes for less than UploadTTL (an older one was
	// abandoned). A connector's documents take no place in it.
	MaxPerWorkspace int `koanf:"max_per_workspace"`
	// MaxPerStream caps the documents a connector keeps of one stream: a
	// full read keeps the newest MaxPerStream at the source, 1 to
	// MaxDocumentsPerStream.
	MaxPerStream int `koanf:"max_per_stream"`
	// UploadTTL is how long a presigned PUT stays valid.
	UploadTTL time.Duration        `koanf:"upload_ttl"`
	Worker    DocumentWorkerConfig `koanf:"worker"`
}

// MaxBytesFor is the cap on a document of mediaType, by how ontologies read
// it; 0 for a type Foundry does not store.
func (d DocumentsConfig) MaxBytesFor(mediaType model.FoundryMediaType) int64 {
	switch mediaType.ReadKind() {
	case model.FoundryReadText:
		return d.MaxBytes
	case model.FoundryReadTable:
		return d.MaxTableBytes
	case model.FoundryReadPDF:
		return d.MaxPDFBytes
	}
	return 0
}

// DocumentWorkerConfig drives the foundry-document-worker, a second
// task of the foundry-sync-worker role, which reads each table file and PDF
// once, when its upload completes (document/read). It has
// no HTTP listener of its own: the pod's is the sync worker's.
type DocumentWorkerConfig struct {
	PollInterval time.Duration `koanf:"poll_interval"`
	// Lease is how long a claimed read stays the worker's without a
	// renewal; a read whose claims lapse three times in a row fails.
	Lease time.Duration `koanf:"lease"`
	// Timeout bounds one read.
	Timeout time.Duration `koanf:"timeout"`
	// MaxPages is the most pages a PDF may have to be read; a longer one
	// fails as too_large before any page is read.
	MaxPages int `koanf:"max_pages"`
	// RetryBackoff is the least a read waits after a failure that is not the
	// file's, such as storage's; it waits as long as it has existed, up to
	// six hours.
	RetryBackoff time.Duration `koanf:"retry_backoff"`
}

// GraphConfig drives the foundry-graph-worker, which loads Foundry
// ontologies' databases with the ontology agent sidecar: a load ingests the
// tables of an approved version, then extracts the documents, writing through
// a per-attempt proxy to the ontology's tenant; a delete removes an old load.
type GraphConfig struct {
	HTTP         ServiceHTTPConfig `koanf:"http"`
	PollInterval time.Duration     `koanf:"poll_interval"`
	// Lease is how long a claimed job stays owned without a renewal. An
	// attempt with no renewal for Lease less HelixTimeoutSecs stops writing,
	// so it must be at least twice HelixTimeoutSecs.
	Lease time.Duration `koanf:"lease"`
	// Timeout bounds one attempt: downloads, Ingest and Extract.
	Timeout     time.Duration `koanf:"timeout"`
	MaxAttempts int           `koanf:"max_attempts"`
	// RetryBackoff is the first retry's wait, doubling after. It must exceed
	// twice the agent's Helix timeout, so a write that timed out has settled.
	RetryBackoff time.Duration `koanf:"retry_backoff"`
	// ShutdownDrain is how long a stopping worker lets the agent's stream end
	// by itself before cancelling it; it must fit the pod's grace period.
	ShutdownDrain time.Duration `koanf:"shutdown_drain"`
	// Agent is the sidecar: its gRPC address and the volume it reads.
	Agent GraphAgentConfig `koanf:"agent"`
	// HelixTimeoutSecs is how long the agent waits for one Helix request.
	HelixTimeoutSecs int `koanf:"helix_timeout_secs"`
	// ChunkRows is the most rows one Ingest write holds.
	ChunkRows int `koanf:"chunk_rows"`
	// DocumentsInFlight is how many documents Extract works on at once.
	DocumentsInFlight int `koanf:"documents_in_flight"`
	// MaxJobUSD caps one job's model spend; MonthlyUSD a workspace's per
	// calendar month, across its ontologies, checked before work is queued
	// and when it runs (MonthBudget). Work no person started stops MaxJobUSD
	// short of MonthlyUSD, which must be above it.
	MaxJobUSD  float64 `koanf:"max_job_usd"`
	MonthlyUSD float64 `koanf:"monthly_usd"`
	// Apply makes a refresh bring the current load up to date in place,
	// writing only what changed; off, every refresh builds a new load.
	Apply bool `koanf:"apply"`
	// RetainLoads is how many loads an ontology keeps: the current one and
	// the newest ready, partial or failed others. Older ones are deleted.
	RetainLoads int `koanf:"retain_loads"`
	// DeleteBatch is the most nodes or edges one request of a load's delete
	// removes, 1 to 5,000.
	DeleteBatch int `koanf:"delete_batch"`
	// LoadRateLimit is the gateway rate limit an ontology's database gets
	// while an attempt writes it, where its plan's is lower. Zero turns it off.
	LoadRateLimit GraphLoadRateLimit `koanf:"load_rate_limit"`
}

// GraphLoadRateLimit is a load's rate limit: both numbers set, or
// neither (off).
type GraphLoadRateLimit struct {
	RequestsPerSecond uint64 `koanf:"requests_per_second"`
	Burst             uint64 `koanf:"burst"`
}

// Enabled reports whether loads lift their database's rate limit.
func (l GraphLoadRateLimit) Enabled() bool { return l.RequestsPerSecond > 0 }

func (l GraphLoadRateLimit) validate() error {
	if (l.RequestsPerSecond == 0) != (l.Burst == 0) || l.RequestsPerSecond > math.MaxUint32 || l.Burst > math.MaxUint32 {
		return fmt.Errorf("foundry.graph.load_rate_limit requests_per_second and burst must both be set, up to %d, or both be 0 (off)", uint32(math.MaxUint32))
	}
	return nil
}

// AskConfig drives questions over a load: the backend checks a
// workspace's limits when one is asked, and the foundry-ask-worker runs them
// with the ontology agent sidecar's read-only Ask, through a per-question
// read-only proxy to the ontology's tenant.
type AskConfig struct {
	HTTP         ServiceHTTPConfig `koanf:"http"`
	PollInterval time.Duration     `koanf:"poll_interval"`
	// Lease is how long a claimed question stays owned without a renewal.
	Lease time.Duration `koanf:"lease"`
	// Concurrency is how many questions one worker runs at once; the
	// sidecar's --max-concurrent-asks matches it.
	Concurrency int `koanf:"concurrency"`
	MaxAttempts int `koanf:"max_attempts"`
	// RetryBackoff is the wait before a failed attempt's retry.
	RetryBackoff time.Duration `koanf:"retry_backoff"`
	// Defer is how long a question held back by its workspace's running
	// questions or their reservations waits before it is tried again.
	Defer time.Duration `koanf:"defer"`
	// Agent is the sidecar's gRPC address.
	Agent AskAgentConfig `koanf:"agent"`
	// HelixTimeoutSecs is how long the agent waits for one Helix read.
	HelixTimeoutSecs int `koanf:"helix_timeout_secs"`
	// MaxUSD, MaxSteps and MaxWallSecs cap one question.
	MaxUSD      float64 `koanf:"max_usd"`
	MaxSteps    int     `koanf:"max_steps"`
	MaxWallSecs int     `koanf:"max_wall_secs"`
	// MonthlyUSD is a workspace's monthly budget for questions, separate
	// from foundry.graph.monthly_usd; MaxRunning and PerHour bound its
	// questions at once and asked in an hour.
	MonthlyUSD float64 `koanf:"monthly_usd"`
	MaxRunning int     `koanf:"max_running"`
	PerHour    int     `koanf:"per_hour"`
}

// AskAgentConfig is the ontology agent sidecar of the ask worker.
type AskAgentConfig struct {
	Addr string `koanf:"addr"`
}

// AskReadyWait is how long a question's attempt waits for a sidecar still
// starting, before the agent's wall-time cap starts.
const AskReadyWait = 30 * time.Second

// AskWallSecsLimit bounds foundry.ask.max_wall_secs from above: the agent
// prices each step as read from the prompt cache, whose entries live five
// minutes, so it refuses a question allowed that long or longer.
const AskWallSecsLimit = 300

// AskAttemptMargin is what a question's attempt allows beyond the agent's
// wall-time cap: building the request, and the call's tail.
const AskAttemptMargin = 30 * time.Second

// AttemptTimeout bounds one attempt of a question: AskReadyWait for the
// sidecar, then the agent's wall-time cap, MaxWallSecs, which starts with the
// call, and AskAttemptMargin after it, so an agent that stops at its cap is
// never cut off first, however long its sidecar took to come up. The
// question's lease is renewed while the attempt runs, and its boost and proxy
// token last as long as the attempt, so none of them bounds it.
func (a AskConfig) AttemptTimeout() time.Duration {
	return AskReadyWait + time.Duration(a.MaxWallSecs)*time.Second + AskAttemptMargin
}

// MaxMicros is a question's cap, MaxUSD, in millionths of a US dollar.
func (a AskConfig) MaxMicros() int64 { return int64(math.Round(a.MaxUSD * 1e6)) }

// MonthlyMicros is a workspace's monthly budget, MonthlyUSD, in millionths
// of a US dollar.
func (a AskConfig) MonthlyMicros() int64 { return int64(math.Round(a.MonthlyUSD * 1e6)) }

func (a AskConfig) validate() error {
	switch {
	case a.MaxUSD <= 0 || a.MaxSteps < 1 || a.MaxWallSecs < 1:
		return fmt.Errorf("foundry.ask max_usd, max_steps and max_wall_secs must be positive")
	case a.MaxWallSecs >= AskWallSecsLimit:
		return fmt.Errorf("foundry.ask.max_wall_secs must be under %d, which the agent refuses", AskWallSecsLimit)
	case a.MonthlyUSD < a.MaxUSD:
		return fmt.Errorf("foundry.ask.monthly_usd must be at least max_usd")
	case a.MaxRunning < 1 || a.PerHour < 1:
		return fmt.Errorf("foundry.ask max_running and per_hour must be positive")
	}
	return nil
}

// GraphAgentConfig is the ontology agent sidecar of the graph worker.
type GraphAgentConfig struct {
	Addr string `koanf:"addr"`
	// WorkDir is the volume the worker shares with the sidecar, which serves
	// it read-only as --files-root.
	WorkDir string `koanf:"work_dir"`
	// MaxWorkBytes caps the snapshots one load unpacks into WorkDir; it stays
	// below the volume's size.
	MaxWorkBytes int64 `koanf:"max_work_bytes"`
}

// MonthBudget is the graph budget of the UTC calendar month now is in: what
// the workspace's jobs created since its first day may spend, MonthlyUSD,
// of which work no person started leaves MaxJobUSD
// (model.FoundryGraphBudget.CapFor).
func (g GraphConfig) MonthBudget(now time.Time) model.FoundryGraphBudget {
	now = now.UTC()
	return model.FoundryGraphBudget{
		Since:         time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC),
		CapMicros:     int64(math.Round(g.MonthlyUSD * 1e6)),
		ReserveMicros: int64(math.Round(g.MaxJobUSD * 1e6)),
	}
}

// GraphRetryFloor is the least foundry.graph.retry_backoff may be: twice the
// agent's Helix timeout, so a write that timed out has committed or not by
// the time the retry reads the store.
func (g GraphConfig) GraphRetryFloor() time.Duration {
	return 2 * time.Duration(g.HelixTimeoutSecs) * time.Second
}

// MaxDocumentBytes bounds each of foundry.documents' caps: the backend returns a
// document whole in one gRPC message to the WFE (GetFoundryDocumentBytes),
// whose client takes at most 4 MiB.
const MaxDocumentBytes = 3 << 20

// MaxDocumentsPerStream bounds foundry.documents.max_per_stream: a load sends
// every document it waits for in one Extract request, about 400 bytes each,
// and a version reads up to 20 sets, so 20 streams at this cap fit the
// agent's 32 MiB message with room to spare. More needs paged extracts.
const MaxDocumentsPerStream = 1000

func (d DocumentsConfig) validate() error {
	if d.MaxPerWorkspace < 1 || d.UploadTTL <= 0 || d.UploadTTL > 7*24*time.Hour {
		return fmt.Errorf("foundry.documents max_per_workspace and upload_ttl (at most 7 days) must be positive")
	}
	if d.MaxPerStream < 1 || d.MaxPerStream > MaxDocumentsPerStream {
		return fmt.Errorf("foundry.documents.max_per_stream is %d; it must be 1 to %d, as one load sends a version's documents in one request",
			d.MaxPerStream, MaxDocumentsPerStream)
	}
	for _, c := range []struct {
		key string
		max int64
	}{{"max_bytes", d.MaxBytes}, {"max_table_bytes", d.MaxTableBytes}, {"max_pdf_bytes", d.MaxPDFBytes}} {
		if c.max <= 0 || c.max > MaxDocumentBytes {
			return fmt.Errorf("foundry.documents.%s is %d; it must be positive, and a document is returned whole in one gRPC message, so at most %d",
				c.key, c.max, MaxDocumentBytes)
		}
	}
	for _, mediaType := range d.MediaTypes {
		if mediaType.Format() == "" {
			return fmt.Errorf("foundry.documents.media_types has %q, which Foundry does not store", mediaType)
		}
	}
	return nil
}

// AgentAttemptMargin is what an agent generation's attempt needs beyond the
// agent's wall-time cap: the snapshot downloads before the call, and the two
// minutes the call may outlast the cap (ontologyagent.TurnTail).
const AgentAttemptMargin = 5 * time.Minute

// OntologyConfig drives the foundry-ontology-worker, which drafts
// ontologies with the ontology agent from the snapshots in
// foundry.sync.data_bucket.
type OntologyConfig struct {
	HTTP         ServiceHTTPConfig `koanf:"http"`
	PollInterval time.Duration     `koanf:"poll_interval"`
	// Lease is how long a claimed generation stays owned without a renewal.
	Lease time.Duration `koanf:"lease"`
	// Timeout bounds one attempt: the downloads and the agent's turn.
	Timeout     time.Duration `koanf:"timeout"`
	MaxAttempts int           `koanf:"max_attempts"`
	// MaxTables caps the tables, synced and table files, one generation reads.
	MaxTables int `koanf:"max_tables"`
	// PerHour caps the generations a workspace may start in an hour; the
	// backend checks it when one is requested.
	PerHour int         `koanf:"per_hour"`
	Agent   AgentConfig `koanf:"agent"`
}

// AgentConfig is the ontology agent the worker drafts with: a
// sidecar in the worker's pod serving gRPC on loopback, reading the snapshots
// the worker decompresses into WorkDir.
type AgentConfig struct {
	// Addr is the sidecar's gRPC address.
	Addr string `koanf:"addr"`
	// WorkDir is the volume the worker shares with the sidecar, which serves
	// it as --files-root.
	WorkDir string      `koanf:"work_dir"`
	Limits  AgentLimits `koanf:"limits"`
	// MaxWorkBytes caps the snapshots one generation unpacks into WorkDir;
	// it stays below the volume's size.
	MaxWorkBytes int64 `koanf:"max_work_bytes"`
}

// AgentLimits are the caps the agent enforces on one turn.
// foundry.ontology.timeout must leave AgentAttemptMargin above max_wall_secs.
type AgentLimits struct {
	MaxUSD      float64 `koanf:"max_usd"`
	MaxSteps    int     `koanf:"max_steps"`
	MaxWallSecs int     `koanf:"max_wall_secs"`
}

// AIConfig is where Foundry's model calls go (package ai):
// the deployment's default, the provider kinds an owner may choose for a
// workspace instead, and how a choice is tested before it is used.
type AIConfig struct {
	// Default is the deployment's provider. Hosted it is Helix's Claude and
	// required; locally it is the install's own, and none while Provider is
	// empty, so model work is refused until an owner chooses one.
	Default AIDefault `koanf:"default"`
	// Kinds are the provider kinds owners may choose: the rollout gate. A kind
	// is added once the ontology-agent image implements it.
	Kinds []string `koanf:"kinds"`
	// AllowPrivateHosts lets endpoints be http and private addresses; local
	// mode only.
	AllowPrivateHosts bool `koanf:"allow_private_hosts"`
	// CallTimeout bounds one JSON or text call attempt.
	CallTimeout time.Duration `koanf:"call_timeout"`
	Probe       AIProbe       `koanf:"probe"`
}

// AIDefault is the deployment's provider, read as an owner's saved
// choice is: Provider is its kind ("" none), BaseURL, Endpoint, Region and
// API the kind's endpoint, and APIKey the key its calls send.
// ContextTokens is the context window an Ollama server loads the models
// with; 0 is the ai package's default.
type AIDefault struct {
	Provider      string   `koanf:"provider"`
	APIKey        string   `koanf:"api_key"`
	BaseURL       string   `koanf:"base_url"`
	Endpoint      string   `koanf:"endpoint"`
	Region        string   `koanf:"region"`
	API           string   `koanf:"api"`
	ContextTokens uint32   `koanf:"context_tokens"`
	Models        AIModels `koanf:"models"`
}

// AIModels names a model per task: drafting ontologies, answering
// questions, reading documents and checking the facts read.
type AIModels struct {
	Draft   string `koanf:"draft"`
	Ask     string `koanf:"ask"`
	Extract string `koanf:"extract"`
	Verify  string `koanf:"verify"`
}

// AIProbeOverhead is the most a probe's claim is held past
// foundry.ai.probe.timeout: the foundry-ask-worker waits for its sidecar to
// answer, lets the Probe call end its checks, and records the outcome
// (AskReadyWait, and its probeTail and finishTimeout). The lease covers it
// all, so no other replica probes the same key while one still is.
const AIProbeOverhead = time.Minute

// AIProbe is how the foundry-ask-worker tests a saved choice: it
// polls every PollInterval, a probe runs at most Timeout under a Lease of at
// least Timeout plus AIProbeOverhead, a probe that loses its worker
// is tried MaxAttempts times, and a workspace saves at most PerHour choices
// an hour.
type AIProbe struct {
	PollInterval time.Duration `koanf:"poll_interval"`
	Timeout      time.Duration `koanf:"timeout"`
	Lease        time.Duration `koanf:"lease"`
	PerHour      int           `koanf:"per_hour"`
	MaxAttempts  int           `koanf:"max_attempts"`
}

// validate checks foundry.ai for a process that serves or makes model calls.
// Hosted Foundry runs on Helix's Claude: an anthropic default naming every model,
// public https endpoints only and no Ollama (a hosted Ollama would be a
// public server with no key); the key is required where model calls are
// made. Locally an empty provider is none. The kinds, endpoints and models
// themselves are the ai package's to check when the process starts.
func (a AIConfig) validate(local, modelCalls bool) error {
	p := a.Probe
	d := a.Default
	models := d.Models
	switch {
	case len(a.Kinds) == 0:
		return fmt.Errorf("foundry.ai.kinds must name at least one provider kind")
	case a.CallTimeout <= 0:
		return fmt.Errorf("foundry.ai.call_timeout must be positive")
	case p.PollInterval <= 0 || p.Timeout <= 0 || p.PerHour < 1 || p.MaxAttempts < 1:
		return fmt.Errorf("foundry.ai.probe poll_interval, timeout, per_hour and max_attempts must be positive")
	case p.Lease < p.Timeout+AIProbeOverhead:
		return fmt.Errorf("foundry.ai.probe.lease %s must be at least its timeout %s plus %s for the sidecar to answer and the outcome to be recorded",
			p.Lease, p.Timeout, AIProbeOverhead)
	case d.Provider == "" && d != (AIDefault{}):
		return fmt.Errorf("foundry.ai.default has settings but no provider")
	case d.Provider != "" && (models.Draft == "" || models.Ask == "" || models.Extract == "" || models.Verify == ""):
		return fmt.Errorf("foundry.ai.default.models must name draft, ask, extract and verify")
	}
	if local {
		return nil
	}
	switch {
	case a.AllowPrivateHosts:
		return fmt.Errorf("foundry.ai.allow_private_hosts is for local mode only")
	case slices.Contains(a.Kinds, "ollama"):
		return fmt.Errorf("foundry.ai.kinds cannot offer ollama in cloud")
	case d.Provider != "anthropic":
		return fmt.Errorf("foundry.ai.default.provider is %q; in cloud it must be anthropic, Helix's Claude", d.Provider)
	case modelCalls && d.APIKey == "":
		return fmt.Errorf("foundry.ai.default.api_key must be set where Foundry's workers or foundry-read run")
	}
	return nil
}

// loopback reports whether addr, a host and port, dials this machine. Each
// process that calls the ontology agent sends it model keys in plaintext
// gRPC metadata, so the agent must be the process's own sidecar (its pod's,
// or locally the compose stack's shared network namespace).
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// SyncConfig drives the foundry-sync-worker, which copies connector
// tables into S3 snapshots.
type SyncConfig struct {
	HTTP ServiceHTTPConfig `koanf:"http"`
	// DataBucket holds the snapshots.
	DataBucket   string        `koanf:"data_bucket"`
	PollInterval time.Duration `koanf:"poll_interval"`
	// Lease is how long a claimed sync stays owned without a renewal.
	Lease time.Duration `koanf:"lease"`
	// Timeout bounds one attempt.
	Timeout         time.Duration `koanf:"timeout"`
	MaxAttempts     int           `koanf:"max_attempts"`
	MaxRowsPerTable int64         `koanf:"max_rows_per_table"`
	// MaxTableBytes caps one table's snapshot, uncompressed, for every kind.
	// At most MaxSnapshotBytes, so its gzip stays under S3's 5 GiB single PUT.
	MaxTableBytes int64 `koanf:"max_table_bytes"`
	// MaxSourceBytes caps the stored bytes of one file source table, which a
	// sync downloads to the worker's scratch volume.
	MaxSourceBytes int64 `koanf:"max_source_bytes"`
	// MaxConnectorBytes caps the stored bytes of the tables one file source
	// sync reads, which bounds its time; at least MaxSourceBytes.
	MaxConnectorBytes int64 `koanf:"max_connector_bytes"`
	// SchedulePoll is how often due connectors are queued, at most ScheduleBatch a tick.
	SchedulePoll  time.Duration `koanf:"schedule_poll"`
	ScheduleBatch int           `koanf:"schedule_batch"`
	// ReplacedSnapshotGrace is how long a replaced snapshot outlives its
	// replacement before the sweep may delete it: it covers a load that
	// resolved the table but has not pinned it yet.
	ReplacedSnapshotGrace time.Duration `koanf:"replaced_snapshot_grace"`
	// ChangeRetention is how long change batches are kept.
	ChangeRetention time.Duration `koanf:"change_retention"`
	// RefreshPoll is how often owed graph refreshes are queued.
	RefreshPoll time.Duration `koanf:"refresh_poll"`
}

// MaxSnapshotBytes is the most foundry.sync.max_table_bytes may be.
const MaxSnapshotBytes = 4 << 30

// validateLimits checks the byte limits connector tests and syncs share.
func (s SyncConfig) validateLimits() error {
	switch {
	case s.MaxTableBytes <= 0 || s.MaxTableBytes > MaxSnapshotBytes:
		return fmt.Errorf("foundry.sync.max_table_bytes must be positive and at most %d", int64(MaxSnapshotBytes))
	case s.MaxSourceBytes <= 0 || s.MaxConnectorBytes < s.MaxSourceBytes:
		return fmt.Errorf("foundry.sync.max_source_bytes must be positive and max_connector_bytes at least max_source_bytes")
	}
	return nil
}

// EventsEndpointConfig is the foundry-events role: the HTTP door that
// takes API sources' pieces, POST /v1/sources/{id}/pieces with a send token
// as the bearer, and forwards them to the backend's SendFoundryEvents. It
// first runs the backend's stateless rules under foundry.events' piece
// limits, so a send they refuse never reaches the backend. It holds no
// secret and stores nothing.
//
// A request passes, cheapest first: the address's bucket, the wire caps
// (MaxBodyBytes), the pod's bucket and its in-flight bounds, then the send
// token, looked up again past ValidatedFor. Its bounds are checked only
// where the role runs (Validate); PublicURL wherever the config loads, since
// the MCP reads it.
type EventsEndpointConfig struct {
	// GRPC and HTTP are the role's listeners; HTTP takes the pieces.
	GRPC ServiceGRPCConfig `koanf:"grpc"`
	HTTP ServiceHTTPConfig `koanf:"http"`
	// Backend is the backend the door forwards each send to.
	Backend BackendClientConfig `koanf:"backend"`
	// PublicURL is the door's origin, such as https://ingest.helix-db.com,
	// which the MCP gives senders. Empty means the deployment has no HTTP door.
	PublicURL string `koanf:"public_url"`
	// MaxBodyBytes caps a request's body on the wire. Its data may be
	// pretty-printed, so it is above max_send_bytes, which the door checks on
	// the data compacted.
	MaxBodyBytes int64 `koanf:"max_body_bytes"`
	// AddressRequestsPerSecond and AddressBurstRequests bound each client
	// address's requests on a pod. They sit above one workspace's send rate,
	// so they never bind a single workspace.
	AddressRequestsPerSecond int `koanf:"address_requests_per_second"`
	AddressBurstRequests     int `koanf:"address_burst_requests"`
	// AddressMaxInFlight is how many requests one client address may have
	// in flight on a pod, its bodies' bytes included, held until each is
	// answered. It stays below MaxInFlight, and its bodies at most
	// MaxBodyBytes each below MaxBytesInFlight, so one address, slow bodies
	// and all, never holds all of a pod's room.
	AddressMaxInFlight int `koanf:"address_max_in_flight"`
	// RequestsPerSecond and BurstRequests bound a pod's requests.
	RequestsPerSecond int `koanf:"requests_per_second"`
	BurstRequests     int `koanf:"burst_requests"`
	// MaxInFlight is how many requests a pod serves at once, and
	// MaxBytesInFlight how many bytes of their bodies it holds, a body of
	// unknown length weighing MaxBodyBytes. A request past either is refused,
	// never queued.
	MaxInFlight      int   `koanf:"max_in_flight"`
	MaxBytesInFlight int64 `koanf:"max_bytes_in_flight"`
	// ValidatedFor is how long a pod admits a send token it found live
	// without looking it up again. The backend reads the token again on every
	// send, so a revoked token's sends are refused at once all the same.
	ValidatedFor time.Duration `koanf:"validated_for"`
}

// EventEnvelopeBytes is the least room foundry.events.endpoint.max_body_bytes
// leaves a body beyond foundry.events.max_send_bytes: for its pieces' ids and
// times, its client_token and its JSON's punctuation.
const EventEnvelopeBytes = 128 << 10

// MaxEventValidatedFor bounds foundry.events.endpoint.validated_for: a token
// revoked meanwhile still reaches the backend, which refuses it, for as long.
const MaxEventValidatedFor = 5 * time.Minute

// Validate checks the door's bounds where the foundry-events role runs.
// sendBytes is foundry.events.max_send_bytes, a send's compacted data, which
// a body must hold with its envelope; a body stays within MaxEventSendBytes,
// as the request the door forwards it in does.
func (d EventsEndpointConfig) Validate(sendBytes int) error {
	for _, v := range []struct {
		name  string
		value int64
	}{
		{"address_requests_per_second", int64(d.AddressRequestsPerSecond)}, {"address_burst_requests", int64(d.AddressBurstRequests)},
		{"address_max_in_flight", int64(d.AddressMaxInFlight)},
		{"requests_per_second", int64(d.RequestsPerSecond)}, {"burst_requests", int64(d.BurstRequests)},
		{"max_in_flight", int64(d.MaxInFlight)}, {"max_bytes_in_flight", d.MaxBytesInFlight},
	} {
		if v.value <= 0 {
			return fmt.Errorf("foundry.events.endpoint.%s must be positive", v.name)
		}
	}
	switch least := int64(sendBytes) + EventEnvelopeBytes; {
	case d.GRPC.Listen == "" || d.HTTP.Listen == "":
		return fmt.Errorf("foundry.events.endpoint grpc.listen and http.listen must be set when 'foundry-events' is in services")
	case d.Backend.Address == "":
		return fmt.Errorf("foundry.events.endpoint.backend.address must be set when 'foundry-events' is in services")
	case d.MaxBodyBytes < least || d.MaxBodyBytes > MaxEventSendBytes:
		return fmt.Errorf("foundry.events.endpoint.max_body_bytes is %d; it must be from foundry.events.max_send_bytes plus %d (%d) to %d",
			d.MaxBodyBytes, EventEnvelopeBytes, least, MaxEventSendBytes)
	case d.MaxBytesInFlight < d.MaxBodyBytes:
		return fmt.Errorf("foundry.events.endpoint.max_bytes_in_flight must be at least max_body_bytes, or the largest body is always refused")
	case d.AddressMaxInFlight >= d.MaxInFlight || int64(d.AddressMaxInFlight)*d.MaxBodyBytes >= d.MaxBytesInFlight:
		return fmt.Errorf("foundry.events.endpoint.address_max_in_flight is %d; it must be below max_in_flight (%d), and that many bodies of max_body_bytes below max_bytes_in_flight (%d), so one address never holds all of a pod's room",
			d.AddressMaxInFlight, d.MaxInFlight, d.MaxBytesInFlight)
	case d.ValidatedFor <= 0 || d.ValidatedFor > MaxEventValidatedFor:
		return fmt.Errorf("foundry.events.endpoint.validated_for is %s; it must be above 0 and at most %s", d.ValidatedFor, MaxEventValidatedFor)
	}
	return nil
}

// EventsConfig drives event sources (connector kind events), which
// people and their agents send pieces of JSON to. Every plan has the same.
type EventsConfig struct {
	// Endpoint is the foundry-events role, the HTTP door to event sources
	// beside the MCP's send tool.
	Endpoint EventsEndpointConfig `koanf:"endpoint"`
	// MaxSourcesPerWorkspace is how many live event sources a workspace may
	// have, and MaxTokensPerSource how many live send tokens a source may.
	MaxSourcesPerWorkspace int `koanf:"max_sources_per_workspace"`
	MaxTokensPerSource     int `koanf:"max_tokens_per_source"`
	// MaxPiecesPerSend, MaxSendBytes and MaxPieceBytes bound one send: its
	// pieces, the compacted data of them all, and that of one.
	MaxPiecesPerSend int `koanf:"max_pieces_per_send"`
	MaxSendBytes     int `koanf:"max_send_bytes"`
	MaxPieceBytes    int `koanf:"max_piece_bytes"`
	// MaxFields is how many top-level fields a source's pieces may carry.
	MaxFields int `koanf:"max_fields"`
	// The send rates, a sustained rate and a burst, per source and per
	// workspace.
	SourceSendsPerSecond    int `koanf:"source_sends_per_second"`
	SourceSendsBurst        int `koanf:"source_sends_burst"`
	WorkspaceSendsPerSecond int `koanf:"workspace_sends_per_second"`
	WorkspaceSendsBurst     int `koanf:"workspace_sends_burst"`
	// WorkspacePiecesPerDay and WorkspaceBytesPerDay cap what a workspace
	// sends in a UTC day: its pieces, and their compacted data.
	WorkspacePiecesPerDay int64 `koanf:"workspace_pieces_per_day"`
	WorkspaceBytesPerDay  int64 `koanf:"workspace_bytes_per_day"`
	// SendConcurrency is how many send transactions the backend runs at
	// once, across the deployment: each holds one of the pool's connections.
	SendConcurrency int `koanf:"send_concurrency"`
	// MaxSendsInProgress is how many sends the backend checks, encodes and
	// stores at once, across the deployment, before their transactions: each holds its
	// pieces several times over in memory meanwhile, and a send past it waits.
	MaxSendsInProgress int `koanf:"max_sends_in_progress"`
	// SyncAfter is how long after a send its source syncs, and the least
	// time between two of its syncs; SyncAfterPerMillionRows adds this much
	// per million rows of its table, since every merge rewrites it.
	SyncAfter               time.Duration `koanf:"sync_after"`
	SyncAfterPerMillionRows time.Duration `koanf:"sync_after_per_million_rows"`
	// RefreshAfterPerMillionRows spaces the refreshes of an ontology that
	// reads a source this much further apart per million rows of its table,
	// beyond the plan's refresh_every: each refresh reads the whole table.
	RefreshAfterPerMillionRows time.Duration `koanf:"refresh_after_per_million_rows"`
	// MaxBatchesPerSync, MaxPiecesPerSync and MaxBytesPerSync bound one
	// sync's read of a source (events.ReadBudget). The bytes are estimated:
	// a batch's, plus each piece's padding and parsed fields.
	MaxBatchesPerSync int   `koanf:"max_batches_per_sync"`
	MaxPiecesPerSync  int64 `koanf:"max_pieces_per_sync"`
	MaxBytesPerSync   int64 `koanf:"max_bytes_per_sync"`
	// MaxExtracted is how many pieces a source's own document set holds, the
	// newest received, at every commit; at most
	// foundry.documents.max_per_stream.
	MaxExtracted int `koanf:"max_extracted"`
	// MaxSystemUSDPerSourceMonth is what work no person started may spend
	// extracting one source's pieces in a UTC month, and
	// MaxSystemUSDPerWorkspaceMonth what it may spend on all of a
	// workspace's sources together (Foundry.SetAllowances).
	MaxSystemUSDPerSourceMonth    float64 `koanf:"max_system_usd_per_source_month"`
	MaxSystemUSDPerWorkspaceMonth float64 `koanf:"max_system_usd_per_workspace_month"`
}

// SetAllowances maps a connector kind to what work no person started
// (refreshes, and extracts no one asked for) may spend in a UTC month
// extracting the documents of the sets that kind owns: on each set, and on
// all of a workspace's sets of the kind together. A kind it leaves out has
// no allowance; its sets draw on the graph's budget alone
// (GraphConfig.MonthBudget).
func (f Foundry) SetAllowances() map[string]model.FoundrySetAllowance {
	return map[string]model.FoundrySetAllowance{model.ConnectorKindEvents: {
		SetMicros:       int64(math.Round(f.Events.MaxSystemUSDPerSourceMonth * 1e6)),
		WorkspaceMicros: int64(math.Round(f.Events.MaxSystemUSDPerWorkspaceMonth * 1e6)),
	}}
}

// MaxEventSendBytes bounds foundry.events.max_send_bytes: a send is held
// whole while it is checked, encoded and stored, and travels in one gRPC
// request, so it stays within gRPC's default 4 MiB message.
const MaxEventSendBytes = 4 << 20

// MinEventBytesPerSync bounds foundry.events.max_bytes_per_sync from below:
// a sync's read always takes its first batch, so its budget must hold
// several of the largest sends.
const MinEventBytesPerSync = 8 << 20

func (e EventsConfig) Validate() error {
	for _, v := range []struct {
		name  string
		value int64
	}{
		{"max_sources_per_workspace", int64(e.MaxSourcesPerWorkspace)}, {"max_tokens_per_source", int64(e.MaxTokensPerSource)},
		{"max_pieces_per_send", int64(e.MaxPiecesPerSend)}, {"max_send_bytes", int64(e.MaxSendBytes)},
		{"max_piece_bytes", int64(e.MaxPieceBytes)}, {"max_fields", int64(e.MaxFields)},
		{"source_sends_per_second", int64(e.SourceSendsPerSecond)}, {"source_sends_burst", int64(e.SourceSendsBurst)},
		{"workspace_sends_per_second", int64(e.WorkspaceSendsPerSecond)}, {"workspace_sends_burst", int64(e.WorkspaceSendsBurst)},
		{"workspace_pieces_per_day", e.WorkspacePiecesPerDay}, {"workspace_bytes_per_day", e.WorkspaceBytesPerDay},
		{"send_concurrency", int64(e.SendConcurrency)}, {"sync_after", int64(e.SyncAfter)},
		{"max_sends_in_progress", int64(e.MaxSendsInProgress)},
		{"sync_after_per_million_rows", int64(e.SyncAfterPerMillionRows)},
		{"refresh_after_per_million_rows", int64(e.RefreshAfterPerMillionRows)},
		{"max_batches_per_sync", int64(e.MaxBatchesPerSync)}, {"max_pieces_per_sync", e.MaxPiecesPerSync},
		{"max_bytes_per_sync", e.MaxBytesPerSync}, {"max_extracted", int64(e.MaxExtracted)},
	} {
		if v.value <= 0 {
			return fmt.Errorf("foundry.events.%s must be positive", v.name)
		}
	}
	for _, v := range []struct {
		name string
		usd  float64
	}{
		{"max_system_usd_per_source_month", e.MaxSystemUSDPerSourceMonth},
		{"max_system_usd_per_workspace_month", e.MaxSystemUSDPerWorkspaceMonth},
	} {
		// Spend is counted in whole millionths of a dollar (SetAllowances),
		// so the amount must round to at least one and fit an int64: this
		// also refuses NaN and the infinities.
		if micros := math.Round(v.usd * 1e6); !(micros >= 1 && micros < math.MaxInt64) {
			return fmt.Errorf("foundry.events.%s must be positive: at least a millionth of a dollar", v.name)
		}
	}
	switch {
	case e.MaxPieceBytes > e.MaxSendBytes || e.MaxSendBytes > MaxEventSendBytes:
		return fmt.Errorf("foundry.events max_piece_bytes must be at most max_send_bytes, and max_send_bytes at most %d", MaxEventSendBytes)
	case e.MaxBytesPerSync < MinEventBytesPerSync:
		return fmt.Errorf("foundry.events.max_bytes_per_sync must be at least %d", MinEventBytesPerSync)
	}
	// The MCP joins send URLs onto the door's origin in whatever role reads
	// it, so it is checked wherever the config loads.
	public := e.Endpoint.PublicURL
	if public == "" {
		return nil
	}
	origin, err := validatedApplicationOrigin(public)
	switch {
	case err != nil:
		return fmt.Errorf("foundry.events.endpoint.public_url: %w", err)
	case origin != public:
		return fmt.Errorf("foundry.events.endpoint.public_url %q must be an origin, such as https://ingest.helix-db.com, with no path", public)
	}
	return nil
}

// ConnectorsConfig holds the AES-256 keys that seal connector
// credentials. An empty CurrentKey turns connectors off; the previous key is
// kept while rows sealed under it remain.
type ConnectorsConfig struct {
	CurrentKeyID  string `koanf:"current_key_id"`
	CurrentKey    string `koanf:"current_key"`
	PreviousKeyID string `koanf:"previous_key_id"`
	PreviousKey   string `koanf:"previous_key"`
	// AllowPrivateHosts lets database connectors dial loopback and private
	// addresses. Local installs only: hosted, it would open the host's own network to customers.
	AllowPrivateHosts bool `koanf:"allow_private_hosts"`
	// S3RoleARN is the foundry-connector role customers' S3 roles trust; the
	// backend and sync worker chain through it. Empty turns IAM role access
	// off, leaving access keys.
	S3RoleARN string `koanf:"s3_role_arn"`
}

var iamRoleARN = regexp.MustCompile(`^arn:aws:iam::\d{12}:role/[\w+=,.@/-]+$`)

// Enabled reports whether connector credentials can be sealed.
func (c ConnectorsConfig) Enabled() bool { return c.CurrentKey != "" }

func (c ConnectorsConfig) validate() error {
	if c.S3RoleARN != "" && !iamRoleARN.MatchString(c.S3RoleARN) {
		return fmt.Errorf("foundry.connectors.s3_role_arn must be an IAM role ARN, like arn:aws:iam::123456789012:role/name")
	}
	if !c.Enabled() && c.CurrentKeyID == "" && c.PreviousKeyID == "" && c.PreviousKey == "" {
		return nil
	}
	if err := validateAESKey("foundry.connectors", "current", c.CurrentKeyID, c.CurrentKey, true); err != nil {
		return err
	}
	if err := validateAESKey("foundry.connectors", "previous", c.PreviousKeyID, c.PreviousKey, false); err != nil {
		return err
	}
	if c.PreviousKeyID != "" && c.PreviousKeyID == c.CurrentKeyID {
		return fmt.Errorf("foundry.connectors current and previous key ids must differ")
	}
	return nil
}

// Billing is Helix Foundry's plan catalog and checkout return
// page. A plan with no Autumn plan is not sold; with none sold Foundry billing
// is off.
type Billing struct {
	// Plans in rank order. The first is Free, which no Autumn plan grants. The
	// sold plans must share one Autumn group of their own, so attaching one
	// replaces another; a plan in helix_base's group (no group) replaces
	// helix_base and takes the helix_credits balance with it.
	Plans []PlanConfig `koanf:"plans"`
	// ReturnURL is the Foundry page Stripe Checkout sends the customer back to.
	ReturnURL string `koanf:"return_url"`
}

// PlanConfig is one plan: the connector kinds it allows and how often
// its connectors sync and its graphs refresh.
type PlanConfig struct {
	Code         string          `koanf:"code"`
	DisplayName  string          `koanf:"display_name"`
	AutumnPlanID string          `koanf:"autumn_plan_id"`
	Kinds        []string        `koanf:"kinds"`
	Sync         SyncRulesConfig `koanf:"sync"`
}

// SyncRulesConfig is how often a plan's connectors sync and how.
type SyncRulesConfig struct {
	// Every is the interval a connector gets unless its owner picks a longer one.
	Every time.Duration `koanf:"every"`
	// Shortest is the shortest interval an owner may pick.
	Shortest time.Duration `koanf:"shortest"`
	// Capture lists the methods allowed: full, cursor, events, log.
	Capture []string `koanf:"capture"`
	// ReconcileEvery is how often a table that takes changes is read in full; 0 when every sync is full.
	ReconcileEvery time.Duration `koanf:"reconcile_every"`
	// RefreshEvery is the least time between two refreshes of one ontology.
	RefreshEvery time.Duration `koanf:"refresh_every"`
}

// CaptureMethods are the capture methods a plan may allow.
var CaptureMethods = []string{"full", "cursor", "events", "log"}

// DefaultPlans is Free, Pro and Max, none of them sold.
func DefaultPlans() []PlanConfig {
	// Free has the SaaS kinds and event sources, whose rows Helix holds.
	free := []string{"stripe", "workos", "posthog", "github", "linear", "calcom", "granola", "events"}
	every := append(slices.Clone(free), "postgres", "mysql", "s3")
	return []PlanConfig{
		{Code: "free", DisplayName: "Free", Kinds: free, Sync: SyncRulesConfig{
			Every: 24 * time.Hour, Shortest: 24 * time.Hour, Capture: []string{"full"}, RefreshEvery: 24 * time.Hour}},
		{Code: "pro", DisplayName: "Pro", Kinds: every, Sync: SyncRulesConfig{
			Every: time.Hour, Shortest: time.Hour, Capture: []string{"full", "cursor", "events"},
			ReconcileEvery: 24 * time.Hour, RefreshEvery: time.Hour}},
		{Code: "max", DisplayName: "Max", Kinds: every, Sync: SyncRulesConfig{
			Every: 15 * time.Minute, Shortest: 15 * time.Minute, Capture: []string{"full", "cursor", "events", "log"},
			ReconcileEvery: 24 * time.Hour, RefreshEvery: 15 * time.Minute}},
	}
}

// Enabled reports whether any Foundry plan is sold.
func (c Billing) Enabled() bool {
	return slices.ContainsFunc(c.Plans, func(p PlanConfig) bool { return p.AutumnPlanID != "" })
}

func (c Billing) validate() error {
	if len(c.Plans) == 0 {
		return fmt.Errorf("billing.autumn.foundry.plans needs at least the Free plan")
	}
	if c.Plans[0].AutumnPlanID != "" {
		return fmt.Errorf("billing.autumn.foundry.plans[0] is Free and must have no autumn_plan_id")
	}
	codes, autumnIDs := map[string]bool{}, map[string]bool{}
	for i, p := range c.Plans {
		field := fmt.Sprintf("billing.autumn.foundry.plans[%d]", i)
		switch {
		case p.Code == "" || p.DisplayName == "":
			return fmt.Errorf("%s needs a code and a display_name", field)
		case codes[p.Code]:
			return fmt.Errorf("%s repeats code %q", field, p.Code)
		case p.AutumnPlanID != "" && autumnIDs[p.AutumnPlanID]:
			return fmt.Errorf("%s repeats autumn_plan_id %q", field, p.AutumnPlanID)
		}
		codes[p.Code], autumnIDs[p.AutumnPlanID] = true, p.AutumnPlanID != ""
		if err := p.Sync.validate(field + ".sync"); err != nil {
			return err
		}
	}
	if !c.Enabled() {
		return nil
	}
	return validatePageURL("billing.autumn.foundry.return_url", c.ReturnURL)
}

func (r SyncRulesConfig) validate(field string) error {
	switch {
	case r.Every <= 0 || r.Shortest <= 0 || r.Shortest > r.Every:
		return fmt.Errorf("%s needs every and shortest above zero, shortest at most every", field)
	case r.ReconcileEvery != 0 && r.ReconcileEvery < r.Every:
		return fmt.Errorf("%s.reconcile_every must be 0 or at least every", field)
	case r.RefreshEvery <= 0:
		return fmt.Errorf("%s.refresh_every must be above zero", field)
	case !slices.Contains(r.Capture, "full"):
		return fmt.Errorf("%s.capture must include full", field)
	}
	for _, c := range r.Capture {
		if !slices.Contains(CaptureMethods, c) {
			return fmt.Errorf("%s.capture %q is not one of %v", field, c, CaptureMethods)
		}
	}
	return nil
}

// ServiceGRPCConfig is the per-service gRPC listener.
type ServiceGRPCConfig struct {
	// Listen is the bind address (e.g. ":50061").
	Listen string `koanf:"listen"`
	// MaxRecvMsgMB caps incoming message size.
	MaxRecvMsgMB int `koanf:"max_recv_msg_mb"`
}

// ServiceHTTPConfig is a service's or worker's HTTP listener, for /healthz,
// /readyz and /metrics.
type ServiceHTTPConfig struct {
	// Listen is the bind address (e.g. ":8081").
	Listen string `koanf:"listen"`
}

// BackendClientConfig is where a service reaches the process serving
// Foundry's API.
type BackendClientConfig struct {
	// Address is its gRPC address (e.g. "localhost:50063").
	Address string `koanf:"address"`
}

// ReadClient is where the API reaches foundry-read; with no address its
// graph reads are Unavailable.
type ReadClient struct {
	// Address is its gRPC address (e.g. "foundry-read:50068").
	Address string `koanf:"address"`
}

func validateAESKey(prefix, name, id, encoded string, required bool) error {
	if !required && id == "" && encoded == "" {
		return nil
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`).MatchString(id) {
		return fmt.Errorf("%s %s key id is invalid", prefix, name)
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) != 32 {
		return fmt.Errorf("%s %s key must decode to exactly 32 bytes", prefix, name)
	}
	return nil
}

func validatedApplicationOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return "", fmt.Errorf("%q is not an absolute origin", raw)
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q must contain only scheme and authority", raw)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && u.Hostname() == "localhost") {
		return "", fmt.Errorf("%q must use HTTPS except for localhost development", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// validatePageURL guards a website page a provider redirects the customer to.
// The query must be empty: the billing system adds its own to the page it returns to.
func validatePageURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("%s %q is not an absolute URL", field, raw)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("%s %q must not contain user info, a query, or a fragment", field, raw)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && u.Hostname() == "localhost") {
		return fmt.Errorf("%s %q must use HTTPS except for localhost development", field, raw)
	}
	return nil
}

// Roles are the parts of Foundry one process runs. Validate checks the
// settings each of them reads, beside those every part shares.
type Roles struct {
	// API serves Foundry's API.
	API bool
	// Read is the foundry-read service; Events the foundry-events door.
	Read, Events bool
	// The workers, by their tasks: foundry-sync-worker,
	// foundry-document-worker, foundry-ontology-worker, foundry-graph-worker,
	// foundry-ask-worker and foundry-enrichment-worker.
	SyncWorker, DocumentWorker, OntologyWorker, GraphWorker, AskWorker, EnrichmentWorker bool
}

// modelCalls reports whether the roles call models: the workers that draft,
// load and answer, and foundry-read, which titles. The API only reads and
// saves the setting.
func (r Roles) modelCalls() bool { return r.OntologyWorker || r.GraphWorker || r.AskWorker || r.Read }

// Validate checks c for a process that runs roles, beyond what each value's
// type says: a standalone install's own rules first (Host.Local), then each
// role's settings, then those every part of Foundry shares. A host that runs
// the foundry-events door alone checks foundry.events with
// EventsEndpointConfig.Validate and EventsConfig.Validate instead, as that
// process reads nothing else.
func (c *Config) Validate(roles Roles) error {
	if c.Host.Local {
		if err := c.validateLocal(); err != nil {
			return err
		}
	}
	if err := c.validateRoles(roles); err != nil {
		return err
	}
	f := c.Foundry
	for _, check := range []func() error{c.Billing.validate, f.Documents.validate, f.Connectors.validate, f.Sync.validateLimits, f.Events.Validate} {
		if err := check(); err != nil {
			return err
		}
	}
	if f.Events.MaxExtracted > f.Documents.MaxPerStream {
		return fmt.Errorf("foundry.events.max_extracted is %d; it must be at most foundry.documents.max_per_stream (%d), the most a stream keeps",
			f.Events.MaxExtracted, f.Documents.MaxPerStream)
	}
	return nil
}

// validateLocal checks a standalone install's Foundry settings: its own
// settings complete, and nothing a hosted deployment alone has.
func (c *Config) validateLocal() error {
	// Each of these leaves a feature Unavailable rather than failing to start.
	for _, required := range []struct{ name, value string }{
		{"foundry.sync.data_bucket", c.Foundry.Sync.DataBucket},
		{"foundry.connectors.current_key", c.Foundry.Connectors.CurrentKey},
		{"backend.foundry_read.address", c.Read.Address},
	} {
		if strings.TrimSpace(required.value) == "" {
			return fmt.Errorf("local mode needs %s", required.name)
		}
	}
	// With no plan sold every workspace is on the first, Free: locally that
	// one plan is everything, so no connector is behind a plan.
	plans := c.Billing.Plans
	if len(plans) != 1 || plans[0].AutumnPlanID != "" {
		return fmt.Errorf("local mode sells no plan: billing.autumn.foundry.plans must be one plan with no autumn_plan_id")
	}
	defaults := DefaultPlans()
	for _, kind := range defaults[len(defaults)-1].Kinds {
		if !slices.Contains(plans[0].Kinds, kind) {
			return fmt.Errorf("local mode's plan must allow every connector kind; it lacks %q", kind)
		}
	}
	// A boost lifts a database's limit at the gateway and waits for its sync;
	// the local HelixDB has neither.
	if c.Foundry.Graph.LoadRateLimit.Enabled() {
		return fmt.Errorf("local mode has no gateway to rate limit: remove foundry.graph.load_rate_limit")
	}
	// The foundry-events door is a hosted role, and so is the MCP that gives
	// senders its URL.
	if c.Foundry.Events.Endpoint.PublicURL != "" {
		return fmt.Errorf("local mode has no API sources HTTP door: remove foundry.events.endpoint.public_url")
	}
	return nil
}

// validateRoles checks that every role the process runs has its listeners,
// its sidecar and its own settings.
func (c *Config) validateRoles(roles Roles) error {
	f := c.Foundry
	if roles.Read {
		r := f.Read
		switch {
		case r.GRPC.Listen == "" || r.HTTP.Listen == "":
			return fmt.Errorf("foundry.read grpc.listen and http.listen must be set when 'foundry-read' is in services")
		case r.AgentAddr == "":
			return fmt.Errorf("foundry.read.agent_addr must be set when 'foundry-read' is in services")
		case !loopback(r.AgentAddr):
			return fmt.Errorf("foundry.read.agent_addr must be a loopback address, not %q: model keys reach the agent in plaintext", r.AgentAddr)
		}
	}
	if roles.Events {
		if err := f.Events.Endpoint.Validate(f.Events.MaxSendBytes); err != nil {
			return err
		}
	}
	if roles.SyncWorker {
		s := f.Sync
		switch {
		case s.HTTP.Listen == "":
			return fmt.Errorf("foundry.sync.http.listen must be set when 'foundry-sync-worker' is in tasks")
		case s.DataBucket == "":
			return fmt.Errorf("foundry.sync.data_bucket must be set when 'foundry-sync-worker' is in tasks")
		case !f.Connectors.Enabled():
			return fmt.Errorf("foundry.connectors keys must be set when 'foundry-sync-worker' is in tasks")
		case s.PollInterval <= 0 || s.Lease <= 0 || s.Timeout <= 0 || s.MaxAttempts < 1 || s.MaxRowsPerTable < 1:
			return fmt.Errorf("foundry.sync poll_interval, lease, timeout, max_attempts and max_rows_per_table must be positive")
		case s.SchedulePoll <= 0 || s.ScheduleBatch < 1 || s.RefreshPoll <= 0 || s.ReplacedSnapshotGrace <= 0 || s.ChangeRetention <= 0:
			return fmt.Errorf("foundry.sync schedule_poll, schedule_batch, refresh_poll, replaced_snapshot_grace and change_retention must be positive")
		}
	}
	if roles.DocumentWorker {
		w := f.Documents.Worker
		switch {
		case f.Sync.DataBucket == "":
			return fmt.Errorf("foundry.sync.data_bucket must be set when 'foundry-document-worker' is in tasks")
		case w.PollInterval <= 0 || w.Timeout <= 0 || w.RetryBackoff <= 0 || w.MaxPages < 1:
			return fmt.Errorf("foundry.documents.worker poll_interval, timeout, retry_backoff and max_pages must be positive")
		case w.Lease < 30*time.Second:
			// Renewed every third of it; shorter would lapse on a slow renewal and read a file twice.
			return fmt.Errorf("foundry.documents.worker.lease must be at least 30s")
		}
	}
	if roles.OntologyWorker {
		o := f.Ontology
		limits := o.Agent.Limits
		switch {
		case o.HTTP.Listen == "":
			return fmt.Errorf("foundry.ontology.http.listen must be set when 'foundry-ontology-worker' is in tasks")
		case f.Sync.DataBucket == "":
			return fmt.Errorf("foundry.sync.data_bucket must be set when 'foundry-ontology-worker' is in tasks")
		case o.PollInterval <= 0 || o.Lease <= 0 || o.Timeout <= 0 || o.MaxAttempts < 1:
			return fmt.Errorf("foundry.ontology poll_interval, lease, timeout and max_attempts must be positive")
		case o.Agent.Addr == "" || o.Agent.WorkDir == "":
			return fmt.Errorf("foundry.ontology.agent addr and work_dir must be set when 'foundry-ontology-worker' is in tasks")
		case !loopback(o.Agent.Addr):
			return fmt.Errorf("foundry.ontology.agent.addr must be a loopback address, not %q: model keys reach the agent in plaintext", o.Agent.Addr)
		case limits.MaxUSD <= 0 || limits.MaxSteps < 1 || limits.MaxWallSecs < 1:
			return fmt.Errorf("foundry.ontology.agent.limits must be positive")
		case o.Agent.MaxWorkBytes <= 0:
			return fmt.Errorf("foundry.ontology.agent.max_work_bytes must be positive")
		case o.Timeout < time.Duration(limits.MaxWallSecs)*time.Second+AgentAttemptMargin:
			return fmt.Errorf("foundry.ontology.timeout %s must be at least agent.limits.max_wall_secs %ds plus %s for downloads and the call's tail",
				o.Timeout, limits.MaxWallSecs, AgentAttemptMargin)
		}
	}
	if roles.GraphWorker {
		g := f.Graph
		switch {
		case g.HTTP.Listen == "":
			return fmt.Errorf("foundry.graph.http.listen must be set when 'foundry-graph-worker' is in tasks")
		case f.Sync.DataBucket == "":
			return fmt.Errorf("foundry.sync.data_bucket must be set when 'foundry-graph-worker' is in tasks")
		case g.Agent.Addr == "" || g.Agent.WorkDir == "":
			return fmt.Errorf("foundry.graph.agent addr and work_dir must be set when 'foundry-graph-worker' is in tasks")
		case !loopback(g.Agent.Addr):
			return fmt.Errorf("foundry.graph.agent.addr must be a loopback address, not %q: model keys reach the agent in plaintext", g.Agent.Addr)
		case g.Agent.MaxWorkBytes <= 0:
			return fmt.Errorf("foundry.graph.agent.max_work_bytes must be positive")
		case g.PollInterval <= 0 || g.Lease <= 0 || g.Timeout <= 0 || g.MaxAttempts < 1 || g.ShutdownDrain <= 0:
			return fmt.Errorf("foundry.graph poll_interval, lease, timeout, max_attempts and shutdown_drain must be positive")
		case g.HelixTimeoutSecs < 1 || g.ChunkRows < 1 || g.DocumentsInFlight < 1:
			return fmt.Errorf("foundry.graph helix_timeout_secs, chunk_rows and documents_in_flight must be positive")
		case g.Lease < 2*time.Duration(g.HelixTimeoutSecs)*time.Second:
			// An attempt stops writing a Helix timeout before its lease could lapse; the rest is its time between renewals.
			return fmt.Errorf("foundry.graph.lease %s must be at least twice helix_timeout_secs (%ds)", g.Lease, g.HelixTimeoutSecs)
		case g.RetryBackoff < g.GraphRetryFloor():
			return fmt.Errorf("foundry.graph.retry_backoff %s must be at least twice helix_timeout_secs (%s)", g.RetryBackoff, g.GraphRetryFloor())
		case g.MaxJobUSD <= 0 || g.MonthlyUSD <= g.MaxJobUSD:
			// Work no person started stops max_job_usd short of the month: at or below it, none would run.
			return fmt.Errorf("foundry.graph max_job_usd must be positive and monthly_usd above max_job_usd")
		case g.RetainLoads < 1:
			return fmt.Errorf("foundry.graph.retain_loads must be at least 1: the current load is always kept")
		case g.DeleteBatch < 1 || g.DeleteBatch > 5000:
			return fmt.Errorf("foundry.graph.delete_batch must be 1 to 5000, the ontology agent's limit")
		case f.Enrichment.MaxPeople < 1 || int64(f.Enrichment.MaxPeople) > math.MaxUint32:
			return fmt.Errorf("foundry.enrichment.max_people must be 1 to %d", uint32(math.MaxUint32))
		}
		if err := g.LoadRateLimit.validate(); err != nil {
			return err
		}
	}
	if roles.AskWorker {
		a := f.Ask
		switch {
		case a.HTTP.Listen == "":
			return fmt.Errorf("foundry.ask.http.listen must be set when 'foundry-ask-worker' is in tasks")
		case a.Agent.Addr == "":
			return fmt.Errorf("foundry.ask.agent.addr must be set when 'foundry-ask-worker' is in tasks")
		case !loopback(a.Agent.Addr):
			return fmt.Errorf("foundry.ask.agent.addr must be a loopback address, not %q: model keys reach the agent in plaintext", a.Agent.Addr)
		case a.PollInterval <= 0 || a.Lease <= 0 || a.Concurrency < 1 || a.MaxAttempts < 1 || a.RetryBackoff <= 0 || a.Defer <= 0:
			return fmt.Errorf("foundry.ask poll_interval, lease, concurrency, max_attempts, retry_backoff and defer must be positive")
		case a.HelixTimeoutSecs < 1:
			return fmt.Errorf("foundry.ask.helix_timeout_secs must be positive")
		case a.Lease < 2*time.Duration(a.HelixTimeoutSecs)*time.Second:
			// An attempt stops reading a Helix timeout before its lease could lapse; the rest is its time between renewals.
			return fmt.Errorf("foundry.ask.lease %s must be at least twice helix_timeout_secs (%ds)", a.Lease, a.HelixTimeoutSecs)
		}
	}
	// The API checks a workspace's limits when a question is asked; the worker when it claims one.
	if roles.API || roles.AskWorker {
		if err := f.Ask.validate(); err != nil {
			return err
		}
	}
	// The API reads and saves the setting; the workers and foundry-read
	// make the model calls.
	if roles.modelCalls() || roles.API {
		if err := f.AI.validate(c.Host.Local, roles.modelCalls()); err != nil {
			return err
		}
	}
	if roles.API && f.Ontology.PerHour < 1 {
		return fmt.Errorf("foundry.ontology.per_hour must be positive")
	}
	if roles.EnrichmentWorker {
		if f.Sync.DataBucket == "" {
			return fmt.Errorf("foundry.sync.data_bucket must be set when 'foundry-enrichment-worker' is in tasks")
		}
		if f.Graph.Lease <= 0 {
			// The sweep deletes a step's whole layout a graph lease past its end, once an attempt that lost its job has stopped writing it.
			return fmt.Errorf("foundry.graph.lease must be positive when 'foundry-enrichment-worker' is in tasks")
		}
		if err := f.Enrichment.validate(); err != nil {
			return err
		}
	}
	return nil
}

// Defaults is every Foundry setting's default; a host loads its file and
// environment over them.
func Defaults() Config {
	return Config{Foundry: Foundry{Sync: SyncConfig{
		HTTP:                  ServiceHTTPConfig{Listen: ":8097"},
		PollInterval:          5 * time.Second,
		Lease:                 2 * time.Minute,
		Timeout:               2 * time.Hour,
		MaxAttempts:           3,
		MaxRowsPerTable:       2_000_000,
		MaxTableBytes:         2 << 30,
		MaxSourceBytes:        2 << 30,
		MaxConnectorBytes:     20 << 30,
		SchedulePoll:          30 * time.Second,
		ScheduleBatch:         100,
		ReplacedSnapshotGrace: time.Hour,
		ChangeRetention:       30 * 24 * time.Hour,
		RefreshPoll:           30 * time.Second,
	}, Events: EventsConfig{
		Endpoint: EventsEndpointConfig{
			GRPC:         ServiceGRPCConfig{Listen: ":50069"},
			HTTP:         ServiceHTTPConfig{Listen: ":8101"},
			Backend:      BackendClientConfig{Address: "localhost:50063"},
			MaxBodyBytes: 2 << 20,
			// Above one workspace's 20 sends a second (burst 50).
			AddressRequestsPerSecond: 50,
			AddressBurstRequests:     100,
			AddressMaxInFlight:       8, // a quarter of the places, half the bytes
			RequestsPerSecond:        200,
			BurstRequests:            400,
			MaxInFlight:              32,
			MaxBytesInFlight:         32 << 20,
			ValidatedFor:             30 * time.Second,
		},
		MaxSourcesPerWorkspace:     20,
		MaxTokensPerSource:         10,
		MaxPiecesPerSend:           500,
		MaxSendBytes:               1 << 20,
		MaxPieceBytes:              256 << 10,
		MaxFields:                  200,
		SourceSendsPerSecond:       5,
		SourceSendsBurst:           20,
		WorkspaceSendsPerSecond:    20,
		WorkspaceSendsBurst:        50,
		WorkspacePiecesPerDay:      200_000,
		WorkspaceBytesPerDay:       1 << 30,
		SendConcurrency:            8,
		MaxSendsInProgress:         16,
		SyncAfter:                  time.Minute,
		SyncAfterPerMillionRows:    10 * time.Minute,
		RefreshAfterPerMillionRows: 30 * time.Minute,
		MaxBatchesPerSync:          1000,
		MaxPiecesPerSync:           50_000,
		MaxBytesPerSync:            128 << 20,
		MaxExtracted:               500,
		MaxSystemUSDPerSourceMonth: 5,
		// Half the system share of a graph month of monthly_usd 50 less
		// max_job_usd 10, so API sources leave the rest to other documents.
		MaxSystemUSDPerWorkspaceMonth: 20,
	}, Ontology: OntologyConfig{
		HTTP:         ServiceHTTPConfig{Listen: ":8098"},
		PollInterval: 5 * time.Second,
		Lease:        2 * time.Minute,
		Timeout:      15 * time.Minute,
		MaxAttempts:  3,
		MaxTables:    50,
		PerHour:      10,
		// Limits fit the 15m timeout with AgentAttemptMargin to spare; a
		// deployment that lets turns run longer raises both.
		Agent: AgentConfig{
			Addr:         "127.0.0.1:50051",
			WorkDir:      "/work",
			Limits:       AgentLimits{MaxUSD: 10, MaxSteps: 200, MaxWallSecs: 600},
			MaxWorkBytes: 6 << 30,
		},
	}, AI: AIConfig{
		// Claude alone, unless a deployment offers more kinds once its
		// agent image calls them.
		Kinds:       []string{"anthropic"},
		CallTimeout: 10 * time.Minute,
		Probe: AIProbe{
			PollInterval: 2 * time.Second, Timeout: 2 * time.Minute, Lease: 3 * time.Minute, PerHour: 10, MaxAttempts: 3,
		},
	}, Documents: DocumentsConfig{
		MaxBytes:        1 << 20,
		MaxTableBytes:   3 << 20,
		MaxPDFBytes:     3 << 20,
		MediaTypes:      model.FoundryMediaTypes(),
		MaxPerWorkspace: 200,
		MaxPerStream:    MaxDocumentsPerStream,
		UploadTTL:       15 * time.Minute,
		Worker: DocumentWorkerConfig{
			PollInterval: 5 * time.Second,
			Lease:        5 * time.Minute,
			Timeout:      5 * time.Minute,
			MaxPages:     150,
			RetryBackoff: time.Minute,
		},
	}, Graph: GraphConfig{
		HTTP:              ServiceHTTPConfig{Listen: ":8099"},
		PollInterval:      5 * time.Second,
		Lease:             2 * time.Minute,
		Timeout:           2 * time.Hour,
		MaxAttempts:       5,
		RetryBackoff:      2 * time.Minute,
		ShutdownDrain:     90 * time.Second,
		Agent:             GraphAgentConfig{Addr: "127.0.0.1:50051", WorkDir: "/work", MaxWorkBytes: 6 << 30},
		HelixTimeoutSecs:  60,
		ChunkRows:         1000,
		DocumentsInFlight: 4,
		MaxJobUSD:         20,
		MonthlyUSD:        100,
		RetainLoads:       5,
		DeleteBatch:       1000,
		Apply:             true,
	}, Read: ReadConfig{
		GRPC:      ServiceGRPCConfig{Listen: ":50068", MaxRecvMsgMB: 16},
		HTTP:      ServiceHTTPConfig{Listen: ":8100"},
		AgentAddr: "127.0.0.1:50051",
	}, Ask: AskConfig{
		HTTP:             ServiceHTTPConfig{Listen: ":8100"},
		PollInterval:     time.Second,
		Lease:            3 * time.Minute,
		Concurrency:      4,
		MaxAttempts:      2,
		RetryBackoff:     10 * time.Second,
		Defer:            15 * time.Second,
		Agent:            AskAgentConfig{Addr: "127.0.0.1:50051"},
		HelixTimeoutSecs: 30,
		MaxUSD:           0.5,
		MaxSteps:         20,
		MaxWallSecs:      240,
		MonthlyUSD:       30,
		MaxRunning:       2,
		PerHour:          30,
	}, Enrichment: EnrichmentConfig{
		HTTP:                 ServiceHTTPConfig{Listen: ":8100"},
		Provider:             EnrichmentFake,
		BaseURL:              "https://api.context.dev/v1",
		MinScore:             90,
		Concurrency:          48,
		MaxRunning:           3,
		ClaimWait:            5 * time.Minute,
		LoadWait:             time.Hour,
		Grace:                2 * time.Minute,
		MonthlyCreditsMax:    100_000,
		GlobalMonthlyCredits: 300_000,
		RawRetentionDays:     90,
		NotFoundDays:         90,
		PollInterval:         5 * time.Second,
		Lease:                2 * time.Minute,
		MaxPeople:            2_000_000,
	}},
		Billing: Billing{Plans: DefaultPlans()},
	}
}
