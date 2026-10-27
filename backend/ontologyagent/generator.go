package ontologyagent

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/ontologygen"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// defaultWorkflow is the brief's workflow when the generation has no goal.
	defaultWorkflow = "Model the business entities, links and rules in this data."
	// agentReadyWait is how long a generation waits for a sidecar that is
	// still starting, as after a rollout, before it counts as a failed attempt.
	agentReadyWait = 30 * time.Second
	// TurnTail is how long the turn's call may outlast the agent's wall-time
	// cap, for the agent to finish its last step and send its draft.
	TurnTail = 2 * time.Minute
	// maxDefinitionBytes bounds a stored draft below gRPC's 4 MiB default,
	// which the backend's and WFE's clients keep, with room for the response.
	maxDefinitionBytes = 3 << 20
	// MaxSetDocuments is how many of a document set's newest uploaded
	// documents that read as text (Markdown, MDX, plain text, and PDFs whose
	// text was read) a generation shows the agent: enough to see what the set
	// holds, which its routes then extract from every document.
	MaxSetDocuments = 5
)

type store interface {
	WorkspaceDeleting(ctx context.Context, id string) (bool, error)
	// GetFoundryEnrichmentConsent is the workspace's People enrichment
	// setting; foundrystore.ErrNotFound means never set, which is off.
	GetFoundryEnrichmentConsent(ctx context.Context, workspaceID string) (*model.FoundryEnrichmentConsent, error)
	TableStore
	DocumentSetStore
	DocumentTableStore
}

// DocumentSetStore is what staging a generation's document sets reads.
type DocumentSetStore interface {
	GetFoundryDocumentSet(ctx context.Context, workspaceID, id string) (*model.FoundryDocumentSet, error)
	ListUploadedFoundryDocuments(ctx context.Context, workspaceID, setID string, limit int) ([]*model.FoundryDocument, error)
}

// TableStore is what resolving a generation's tables to snapshots reads.
type TableStore interface {
	ListConnectors(ctx context.Context, workspaceID string) ([]*model.Connector, error)
	ListConnectorTables(ctx context.Context, workspaceID, connectorID string) ([]*model.ConnectorTable, error)
}

// agent is the sidecar; *Client calls it.
type agent interface {
	Info(ctx context.Context) (*agentv1.GetInfoResponse, error)
	Turn(ctx context.Context, request []byte, key ModelKey, onEvent func(json.RawMessage)) (json.RawMessage, error)
}

// Draft is what one attempt drafts on: the turn's models and their key, as
// the workspace's AI setting resolved, and how a failure of them reads.
type Draft struct {
	Models TurnModels
	Key    ModelKey
	// Failure is the attempt's *ontologygen.Failure for a turn whose model
	// calls failed with f.
	Failure func(ctx context.Context, f ModelFailure) error
}

// drafter resolves what each attempt drafts on; *ai.Resolver is one.
type drafter interface {
	// Draft is the workspace's provider for one attempt on a sidecar that
	// calls providers (GetInfo's), or the *ontologygen.Failure of a setting
	// that can't run there.
	Draft(ctx context.Context, workspaceID string, providers []string) (Draft, error)
}

// DocumentStore reads an uploaded document's original, or the text its read
// stored (document.Store); a missing one is document.ErrMissing.
type DocumentStore interface {
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// Generator drafts a generation's ontology with one start turn of the agent,
// for the foundry-ontology-worker, which runs one generation at a time.
type Generator struct {
	DB        store
	Snapshots snapshot.Store
	// Documents holds the originals of the generation's documents.
	Documents DocumentStore
	Agent     agent
	// AI resolves, every attempt, the models and key the turn runs on.
	AI drafter
	// WorkDir is the agent's files root. Snapshots are decompressed into
	// WorkDir/GenerationCache, one file per snapshot key.
	WorkDir string
	Limits  Limits
	// MaxWorkBytes caps the snapshots unpacked into WorkDir for one attempt.
	MaxWorkBytes int64
	// Enrichment is Helix's People enrichment flag (foundry.enrichment.enabled):
	// with it on and the workspace's consent, the turn may draft an
	// [enrichments] item.
	Enrichment bool
	Now        func() time.Time
}

// Definition is what an agent generation stores as its version, in format
// agent_bundle: the draft the start turn built, as the agent rendered it.
type Definition struct {
	// Format is always "agent_bundle", so a reader of the JSON alone knows it.
	Format          string `json:"format"`
	ParentVersionID string `json:"parent_version_id,omitempty"`
	// Status is how the turn ended: done, needs_input or budget_exhausted.
	Status       StatusKind `json:"status"`
	OntologyTOML string     `json:"ontology_toml"`
	MappingsTOML string     `json:"mappings_toml"`
	// Draft is the agent's DraftView without its questions, which are
	// Questions. The session state is not kept, so a draft can't be continued.
	Draft     json.RawMessage `json:"draft"`
	Questions json.RawMessage `json:"questions"`
	Summary   *string         `json:"summary"`
	// Rules are the draft's rule counts as the turn reported them (null when
	// it reported none); a load needs every rule evaluated.
	Rules *RuleCounts `json:"rules"`
	Usage Usage       `json:"usage"`
}

// LoadRefusal says why a load can't ingest version v, written for the customer, or
// "" when it can. CreateFoundryLoad refuses with it, and the MCP shows it on a version
// before an agent asks for a load. A load ingests only an agent draft that:
//   - finished (done), every one of its rules evaluated against data;
//   - has a generation to name its tables.
//
// A hand-written Definition v1 never loads. The error is a definition that doesn't
// read as a Definition.
func LoadRefusal(v *model.OntologyVersion) (string, error) {
	if v.Format != model.OntologyFormatAgentBundle {
		return "only an ontology agent draft can be loaded; generate one", nil
	}
	var def Definition
	if err := json.Unmarshal(v.Definition, &def); err != nil {
		return "", err
	}
	switch {
	case def.Status != StatusDone:
		return fmt.Sprintf("this draft ended as %s; only a finished draft can be loaded", def.Status), nil
	case def.Rules == nil:
		return "this draft records no evaluation of its rules; generate it again", nil
	case def.Rules.Evaluated != def.Rules.Count:
		return fmt.Sprintf("%d of this draft's %d rules were evaluated against data; only a draft whose rules all were can be loaded",
			def.Rules.Evaluated, def.Rules.Count), nil
	case v.GenerationID == nil:
		return "this draft has no generation to name its tables", nil
	}
	return "", nil
}

// Run drafts one ontology.
//  1. Check the agent speaks the vendored contract. A sidecar still starting
//     gets agentReadyWait; another contract fails the generation for good.
//  2. Load the workspace, which must not be being deleted, and resolve its
//     AI setting: the models and key the turn runs on, of a provider the
//     agent calls (GetInfo's providers). Then decompress the
//     newest snapshot of each of the generation's tables, and the read of
//     each of its table files still uploaded in its sets, into the cache: a
//     table file is a table of its own, named "Set / file", whose source id
//     is its document id. Download the newest MaxSetDocuments uploaded text
//     documents of each of its document sets into
//     WorkDir/generations/<generation>, removed after the turn, MDX with its
//     code blanked (StageText). Each set is one source, routed to
//     extraction, so the draft can route it to the types, props and links
//     its documents fill. A set or a document deleted since is left out; an
//     empty set is sent for routing. With no table and no text left, the
//     generation fails for good.
//     With the enrichment flag on and the workspace's consent, People
//     enrichment's profiles are a source too: a zero-byte file the turn
//     never reads, so the draft may propose an [enrichments] item.
//  3. Run a start turn on those models, the key in the call's metadata: the
//     goal is the brief, the generation's id is both the session and the
//     request id. The call ends at the agent's wall-time cap plus TurnTail. A
//     retry sends the same request and pays again. The result records the
//     model the turn started on, and its provider.
//  4. Store the draft as an agent_bundle Definition of at most 3 MiB, with
//     the turn's rule counts.
//
// Every failure is an ontologygen.Failure; tokens are set even then, from the
// agent's last usage event.
func (g *Generator) Run(ctx context.Context, gen *model.OntologyGeneration) (ontologygen.Result, error) {
	result := ontologygen.Result{Format: model.OntologyFormatAgentBundle}
	infoCtx, cancelInfo := context.WithTimeout(ctx, agentReadyWait)
	info, err := g.Agent.Info(infoCtx)
	cancelInfo()
	if err != nil {
		return result, &ontologygen.Failure{Message: "the ontology agent is unavailable; Helix will retry", Err: err}
	}
	if info.GetContractVersion() != ContractVersion {
		return result, &ontologygen.Failure{Permanent: true,
			Message: fmt.Sprintf("the ontology agent speaks contract %s, not %s", info.GetContractVersion(), ContractVersion)}
	}
	deleting, err := g.DB.WorkspaceDeleting(ctx, gen.WorkspaceID)
	if errors.Is(err, foundrystore.ErrNotFound) || err == nil && deleting {
		return result, &ontologygen.Failure{Message: "the workspace was deleted", Permanent: true}
	}
	if err != nil {
		return result, &ontologygen.Failure{Message: "could not read the synced data; Helix will retry", Err: fmt.Errorf("load workspace: %w", err)}
	}
	draft, err := g.AI.Draft(ctx, gen.WorkspaceID, info.GetProviders())
	if err != nil {
		return result, err
	}
	result.Model, result.ModelProvider = draft.Models.Draft.Name, string(draft.Models.Endpoint.Provider.Kind())
	files, _, err := ResolveDocumentTables(ctx, g.DB, gen.WorkspaceID, gen.Input.Documents, gen.Input.DocumentSetIDs)
	if err != nil {
		return result, &ontologygen.Failure{Message: "could not read the documents; Helix will retry", Err: err}
	}
	cache := &SnapshotCache{DB: g.DB, Snapshots: g.Snapshots, WorkDir: g.WorkDir, Dir: GenerationCache, MaxBytes: g.MaxWorkBytes, Now: g.Now}
	sources, err := cache.Sources(ctx, gen.WorkspaceID, gen.Input.Tables, files)
	if err != nil {
		return result, err
	}
	defer func() { _ = os.RemoveAll(filepath.Join(g.WorkDir, "generations", gen.ID)) }() // best-effort: the next attempt starts afresh
	documents, err := g.stageDocumentSets(ctx, gen)
	if err != nil {
		return result, &ontologygen.Failure{Message: "could not read the documents; Helix will retry", Err: err}
	}
	sources = append(sources, documents...)
	if !slices.ContainsFunc(sources, func(s Source) bool { return s.Kind == SourceTable || len(s.Documents) > 0 }) {
		return result, &ontologygen.Failure{Permanent: true,
			Message: "nothing to generate from is left: the files and documents chosen were deleted or moved; choose others and try again"}
	}
	profiles, err := g.enrichmentSource(ctx, gen)
	if err != nil {
		return result, err
	}
	sources = append(sources, profiles...)
	start := Start{Workflow: defaultWorkflow, Goals: []string{}}
	if gen.Goal != nil && *gen.Goal != "" {
		start = Start{Workflow: *gen.Goal, Goals: []string{*gen.Goal}}
	}
	body, err := json.Marshal(TurnRequest{
		SessionID: gen.ID, RequestID: gen.ID, Sources: sources, Input: Input{Start: start}, Limits: g.Limits, Models: draft.Models,
	})
	if err != nil {
		return result, fmt.Errorf("encode turn request: %w", err)
	}

	var events int
	var spent Usage
	turnCtx, cancelTurn := context.WithTimeout(ctx, time.Duration(g.Limits.MaxWallSecs)*time.Second+TurnTail)
	defer cancelTurn()
	raw, err := g.Agent.Turn(turnCtx, body, draft.Key, func(e json.RawMessage) {
		events++
		var line struct {
			Event string    `json:"event"`
			Usage *Usage    `json:"usage"`
			Model *ModelRef `json:"model"`
		}
		if json.Unmarshal(e, &line) != nil {
			return
		}
		switch {
		case line.Event == "usage" && line.Usage != nil:
			spent = *line.Usage
		case line.Event == "turn_started" && line.Model != nil && line.Model.Name != "":
			result.Model, result.ModelProvider = line.Model.Name, string(line.Model.Provider)
		}
	})
	if err != nil {
		tokens(&result, spent)
		return result, turnFailure(err, events > 0)
	}
	turn, err := DecodeTurnResult(raw)
	if err != nil {
		tokens(&result, spent)
		return result, &ontologygen.Failure{Message: "the ontology agent returned a result Helix can't read", Permanent: true, Err: err}
	}
	tokens(&result, turn.Usage)
	switch turn.Status.Kind {
	case StatusDone, StatusNeedsInput, StatusBudgetExhausted:
	case StatusModelFailed:
		return result, draft.Failure(ctx, turn.Status.Failure)
	case StatusSourceUnavailable:
		// The next attempt downloads every file again. Only this generation
		// reads them: the worker runs one at a time.
		cache.Forget(sources)
		return result, &ontologygen.Failure{Message: "the ontology agent could not read a synced table; Helix will retry",
			Err: fmt.Errorf("source %s unavailable", turn.Status.Source)}
	case StatusInvalidInput:
		return result, &ontologygen.Failure{Message: "the ontology agent refused the request", Permanent: true}
	default:
		return result, &ontologygen.Failure{Message: "the ontology agent ended a first build as " + string(turn.Status.Kind), Permanent: true}
	}
	// The website reads the questions at the top level, so the draft's own copy goes.
	var view, inner map[string]json.RawMessage
	if err := json.Unmarshal(turn.Draft, &view); err != nil {
		return result, fmt.Errorf("read agent draft: %w", err)
	}
	if err := json.Unmarshal(view["draft"], &inner); err != nil {
		return result, fmt.Errorf("read agent draft: %w", err)
	}
	delete(inner, "questions")
	if view["draft"], err = json.Marshal(inner); err != nil {
		return result, fmt.Errorf("encode agent draft: %w", err)
	}
	drafted, err := json.Marshal(view)
	if err != nil {
		return result, fmt.Errorf("encode agent draft: %w", err)
	}
	definition, err := json.Marshal(Definition{
		Format: string(model.OntologyFormatAgentBundle), Status: turn.Status.Kind, OntologyTOML: turn.OntologyTOML,
		MappingsTOML: turn.MappingsTOML, Draft: drafted, Questions: turn.Questions, Summary: turn.Summary, Rules: turn.Rules, Usage: turn.Usage,
	})
	if err != nil {
		return result, fmt.Errorf("encode agent draft: %w", err)
	}
	if len(definition) > maxDefinitionBytes {
		return result, &ontologygen.Failure{Permanent: true,
			Message: fmt.Sprintf("the ontology agent's draft is %d KiB; Helix stores drafts of up to %d MiB", len(definition)>>10, maxDefinitionBytes>>20)}
	}
	result.Definition = definition
	return result, nil
}

// stageDocumentSets writes the newest MaxSetDocuments uploaded documents
// that read as text (model.FoundryDocument.TextKey: text documents, and PDFs
// whose read succeeded) of each of the generation's document sets, as
// StageText writes them, under WorkDir/generations/<generation>, the
// generation's alone, and returns a source per live set. Deleted sets and
// documents whose text is gone are left out. Empty live sets stay available
// for routing future uploads. A set's fingerprint hashes its staged
// documents' ids and the SHA-256 of the text staged, so a change to how a
// document is staged (an MDX blanked otherwise) measures it afresh.
func (g *Generator) stageDocumentSets(ctx context.Context, gen *model.OntologyGeneration) ([]Source, error) {
	dir := filepath.Join("generations", gen.ID)
	if err := os.RemoveAll(filepath.Join(g.WorkDir, dir)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(g.WorkDir, dir), 0o755); err != nil {
		return nil, err
	}
	var out []Source
	for _, setID := range gen.Input.DocumentSetIDs {
		set, err := g.DB.GetFoundryDocumentSet(ctx, gen.WorkspaceID, setID)
		if errors.Is(err, foundrystore.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get document set %s: %w", setID, err)
		}
		docs, err := g.DB.ListUploadedFoundryDocuments(ctx, gen.WorkspaceID, set.ID, MaxSetDocuments)
		if err != nil {
			return nil, fmt.Errorf("list document set %s: %w", set.ID, err)
		}
		files := []DocumentFile{}
		fingerprint := sha256.New()
		for _, d := range docs {
			key, ok := d.TextKey()
			if !ok {
				// The listing returns only documents with text; one that slips through is still not staged.
				continue
			}
			body, err := g.Documents.Open(ctx, key)
			if errors.Is(err, document.ErrMissing) {
				continue
			}
			if err != nil {
				return nil, err
			}
			stored, err := io.ReadAll(body)
			_ = body.Close() // best-effort: only read
			if err != nil {
				return nil, fmt.Errorf("read document %s: %w", d.ID, err)
			}
			media, ext, text, _ := StageText(d.MediaType, stored) // a document with text, checked above
			path := filepath.Join(dir, d.ID+ext)
			if err := os.WriteFile(filepath.Join(g.WorkDir, path), text, 0o644); err != nil {
				return nil, fmt.Errorf("stage document %s: %w", d.ID, err)
			}
			files = append(files, DocumentFile{DocID: d.ID, Path: path, MediaType: media, Title: d.Name})
			fmt.Fprintf(fingerprint, "%s %x\n", d.ID, sha256.Sum256(text))
		}
		out = append(out, Source{
			Kind: SourceDocuments, ID: set.ID, DisplayName: set.DisplayName(), Description: set.Description, Documents: files,
			Fingerprint: hex.EncodeToString(fingerprint.Sum(nil)),
		})
	}
	return out, nil
}

// enrichmentSource is People enrichment's profiles for gen's turn, only
// while the flag and the workspace's consent are both on: a zero-byte file
// under WorkDir/generations/<generation>, which the engine never reads (its
// catalog is a constant) and which the turn's cleanup removes.
func (g *Generator) enrichmentSource(ctx context.Context, gen *model.OntologyGeneration) ([]Source, error) {
	if !g.Enrichment {
		return nil, nil
	}
	consent, err := g.DB.GetFoundryEnrichmentConsent(ctx, gen.WorkspaceID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, &ontologygen.Failure{Message: "could not read the workspace's People enrichment setting; Helix will retry", Err: err}
	case !consent.Enabled:
		return nil, nil
	}
	path := filepath.Join("generations", gen.ID, EnrichmentSourceID+".jsonl")
	if err := os.WriteFile(filepath.Join(g.WorkDir, path), nil, 0o644); err != nil {
		return nil, &ontologygen.Failure{Message: "could not prepare the sources; Helix will retry", Err: err}
	}
	return []Source{EnrichmentSource(path, model.FoundryEnrichmentNone.SHA256)}, nil
}

// tokens records what the turn spent: every kind of input token as input.
func tokens(result *ontologygen.Result, u Usage) {
	result.InputTokens, result.OutputTokens = int(u.Input+u.CacheRead+u.CacheWrite), int(u.Output)
}

// turnFailure classifies a failed call. A result past the agent's 32 MiB
// limit is OUT_OF_RANGE: the agent (tonic) refuses to encode it and ends the
// stream with that status. The client's own limit gives RESOURCE_EXHAUSTED
// after events; the agent's busy refusal comes before any event.
func turnFailure(err error, afterEvents bool) error {
	switch code := status.Code(err); {
	case code == codes.OutOfRange, code == codes.ResourceExhausted && afterEvents:
		return &ontologygen.Failure{Message: fmt.Sprintf("the ontology agent's draft is larger than %d MiB", MaxMessageBytes>>20), Permanent: true, Err: err}
	case code == codes.InvalidArgument:
		return &ontologygen.Failure{Message: "the ontology agent refused the request", Permanent: true, Err: err}
	case code == codes.FailedPrecondition:
		// A provider the agent doesn't call, or a model it can't price.
		return &ontologygen.Failure{Message: "the ontology agent can't draft on these models", Permanent: true, Err: err}
	case code == codes.Unimplemented:
		return &ontologygen.Failure{Message: "the ontology agent does not serve turns", Permanent: true, Err: err}
	}
	// UNAVAILABLE, RESOURCE_EXHAUSTED before any event, a dropped stream, a
	// deadline: another attempt may get through.
	return &ontologygen.Failure{Message: "the ontology agent is unavailable; Helix will retry", Err: err}
}

// The cache directories under the agent's files root: one per worker, since
// each prunes what its own attempt doesn't use. In the cloud each worker has
// a pod and a files root of its own; in local mode the ontology and graph
// workers share one process and one files root, and run at once.
const (
	GenerationCache = "cache"
	GraphCache      = "graph-cache"
)

// SnapshotCache gives the agent the tables as local files: each synced
// table's newest snapshot, and each table file's read, decompressed into
// WorkDir/Dir once per key. Generations and graph loads read through it,
// each worker one attempt at a time.
type SnapshotCache struct {
	DB        TableStore
	Snapshots snapshot.Store
	// WorkDir is the agent's files root.
	WorkDir string
	// Dir is this worker's cache directory under WorkDir, GenerationCache or
	// GraphCache: the only one its Sources prunes.
	Dir string
	// MaxBytes caps the cache files one attempt uses, decompressed: the
	// volume holding WorkDir is only so big.
	MaxBytes int64
	Now      func() time.Time
}

// Sources resolves each table to its newest snapshot, and makes sure the
// agent has it and each table file of documents as a local file: a table
// file's source id is its document id, and its file is its read's, JSON
// Lines unless it is a JSON document. An event source's table carries the
// source's description. Tables no longer synced are left out; with no table
// synced and no file left, asking for some fails for good.
// Cache files this call does not use are
// removed first, so what it unpacks has the volume; more than MaxBytes in
// all fails for good. Every failure is an ontologygen.Failure.
func (c *SnapshotCache) Sources(ctx context.Context, workspaceID string, tables []model.OntologyGenerationTable, documents []DocumentTable) (
	[]Source, error) {
	sources, _, err := c.Pinned(ctx, workspaceID, tables, documents, nil, nil)
	return sources, err
}

// Pinned is Sources reading the snapshots pins names, or each table's newest
// when pins is empty, and returns the snapshots it read so a load's retries
// read the same data. A pinned snapshot that is gone fails for good: the
// tables changed and were swept while the load waited. When pins is empty,
// record gets the newest snapshots before any is downloaded: the sweep spares
// a replaced snapshot only for its grace until something pins it. Each
// table's source says the sync its snapshot was pinned at (ObservedSync), so
// every attempt sends the same; each table file's, its upload.
func (c *SnapshotCache) Pinned(ctx context.Context, workspaceID string, tables []model.OntologyGenerationTable, documents []DocumentTable,
	pins []model.FoundryLoadSource, record func([]model.FoundryLoadSource) error) ([]Source, []model.FoundryLoadSource, error) {
	retry := func(err error) error {
		return &ontologygen.Failure{Message: "could not read the synced data; Helix will retry", Err: err}
	}
	// Pruning WorkDir itself, or another worker's directory, would delete files
	// in use: the directory is one of the two, and set.
	if c.Dir != GenerationCache && c.Dir != GraphCache {
		panic(fmt.Sprintf("ontologyagent: SnapshotCache.Dir is %q, not GenerationCache or GraphCache", c.Dir))
	}
	cacheDir := filepath.Join(c.WorkDir, c.Dir)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, nil, retry(err)
	}
	connectors, err := c.DB.ListConnectors(ctx, workspaceID)
	if err != nil {
		return nil, nil, retry(fmt.Errorf("list connectors: %w", err))
	}
	type table struct {
		connectorName string
		pin           model.FoundryLoadSource
	}
	names, descriptions := map[string]string{}, map[string]string{}
	for _, conn := range connectors {
		names[conn.ID] = conn.Name
		// An event source says what each of its pieces is, which its table's
		// name alone ("<name> / events") does not.
		var settings connector.EventsSettings
		if conn.Kind == model.ConnectorKindEvents && json.Unmarshal(conn.Config, &settings) == nil && settings.Description != nil {
			descriptions[conn.ID] = *settings.Description
		}
	}
	synced := map[[2]string]table{}
	for _, pin := range pins {
		synced[[2]string{pin.ConnectorID, pin.Table}] = table{names[pin.ConnectorID], pin}
	}
	for _, conn := range connectors {
		if len(pins) > 0 {
			break
		}
		rows, err := c.DB.ListConnectorTables(ctx, workspaceID, conn.ID)
		if err != nil {
			return nil, nil, retry(fmt.Errorf("list connector %s tables: %w", conn.ID, err))
		}
		for _, row := range rows {
			synced[[2]string{conn.ID, row.TableName}] = table{conn.Name, model.FoundryLoadSource{
				ConnectorID: conn.ID, Table: row.TableName, S3Key: row.S3Key,
				SyncID: row.SyncID, SyncedAt: row.SyncedAt.UTC(), ChangeSeq: row.ChangeSeq,
			}}
		}
	}
	out := []Source{} // never nil: the contract takes an array, empty for a version drafted from documents alone
	var read []model.FoundryLoadSource
	keys := map[string]string{} // cache file name to the key of the object it copies
	for _, t := range tables {
		found, ok := synced[[2]string{t.ConnectorID, t.Table}]
		if !ok {
			continue
		}
		read = append(read, found.pin)
		keySum := sha256.Sum256([]byte(found.pin.S3Key))
		keyHash := hex.EncodeToString(keySum[:])
		keys[keyHash+".jsonl"] = found.pin.S3Key
		var observed Observed // a pin from before pins kept their sync doesn't say
		if found.pin.SyncID != "" {
			observed = ObservedSync{At: found.pin.SyncedAt, SyncID: found.pin.SyncID, ChangeSeq: uint64(found.pin.ChangeSeq)}
		}
		// The fingerprint is the key's hash: the agent shows it to the model, and the key names the workspace and connector.
		out = append(out, Source{
			ID: TableSourceID(t.ConnectorID, t.Table), DisplayName: TableDisplayName(found.connectorName, t.Table),
			Description: descriptions[t.ConnectorID], Path: filepath.Join(c.Dir, keyHash+".jsonl"), Fingerprint: keyHash,
			Observed: observed,
		})
	}
	if len(tables) > 0 && len(out) == 0 && len(documents) == 0 {
		return nil, nil, &ontologygen.Failure{Message: "none of the tables to generate from are synced any more; sync a connector and try again", Permanent: true}
	}
	for _, d := range documents {
		key := *d.Document.Read.ArtifactKey
		keySum := sha256.Sum256([]byte(key))
		keyHash := hex.EncodeToString(keySum[:])
		name := keyHash + "." + d.Format().String()
		keys[name] = key
		var observed Observed // every uploaded document has its time; nil only if one slipped through without
		if d.Document.UploadedAt != nil {
			observed = ObservedUpload{DocumentID: d.Document.ID, UploadedAt: d.Document.UploadedAt.UTC()}
		}
		out = append(out, Source{
			ID: d.Document.ID, DisplayName: d.DisplayName(), Path: filepath.Join(c.Dir, name), Format: d.Format(), Fingerprint: keyHash,
			Observed: observed,
		})
	}
	if len(pins) == 0 && record != nil {
		if err := record(read); err != nil {
			return nil, nil, err
		}
	}
	c.removeUnused(ctx, cacheDir, keys)
	// Files already here count first, so a download cannot take their room.
	var used int64
	var missing []string
	for name := range keys {
		info, err := os.Stat(filepath.Join(cacheDir, name))
		if err != nil {
			missing = append(missing, name)
			continue
		}
		used += info.Size()
		now := c.Now()
		if err := os.Chtimes(filepath.Join(cacheDir, name), now, now); err != nil {
			return nil, nil, retry(err)
		}
	}
	tooBig := &ontologygen.Failure{Permanent: true,
		Message: fmt.Sprintf("the tables chosen hold more than %s once unpacked; choose fewer tables", connector.FormatBytes(c.MaxBytes))}
	if used > c.MaxBytes {
		return nil, nil, tooBig
	}
	slices.Sort(missing)
	for _, name := range missing {
		n, err := c.cache(ctx, keys[name], filepath.Join(cacheDir, name), c.MaxBytes-used)
		switch {
		case errors.Is(err, errOverBudget):
			return nil, nil, tooBig
		case errors.Is(err, snapshot.ErrMissing) && len(pins) > 0:
			return nil, nil, &ontologygen.Failure{Permanent: true, Err: err,
				Message: "the tables this load started from were synced again and are gone; start a new load"}
		case err != nil:
			return nil, nil, retry(err)
		}
		used += n
	}
	return out, read, nil
}

// removeUnused deletes the cache files, leftovers of a crashed download
// included, that are not keep's: the directory is this worker's, and the
// worker runs one attempt at a time, so no other attempt reads them.
func (c *SnapshotCache) removeUnused(ctx context.Context, dir string, keep map[string]string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.From(ctx).Warn("list ontology agent cache failed", "err", err)
		return
	}
	for _, e := range entries {
		if _, ok := keep[e.Name()]; ok {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			log.From(ctx).Warn("remove ontology agent cache file failed", "file", e.Name(), "err", err)
		}
	}
}

// Forget removes the tables' files, so the next attempt downloads them again.
// A document set's files are the attempt's own, staged afresh each time.
func (c *SnapshotCache) Forget(sources []Source) {
	for _, s := range sources {
		if s.Kind != SourceTable {
			continue
		}
		_ = os.Remove(filepath.Join(c.WorkDir, s.Path)) // best-effort: a missing file is downloaded anyway
	}
}

// errOverBudget is a snapshot larger, decompressed, than the room left.
var errOverBudget = errors.New("ontologyagent: snapshot over the work budget")

// cache makes path a copy of the object at key, decompressed when its key
// ends .gz (a snapshot, a table file's converted rows) and as it is
// otherwise (a JSON or JSON Lines original), at most room bytes, and returns
// its size. A rename puts each copy in place whole, and every object cached
// is immutable, so a file named for its key never goes stale.
func (c *SnapshotCache) cache(ctx context.Context, key, path string, room int64) (_ int64, err error) {
	body, err := c.Snapshots.Open(ctx, key)
	if err != nil {
		return 0, err
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	var data io.Reader = body
	if strings.HasSuffix(key, ".gz") {
		gz, err := gzip.NewReader(body)
		if err != nil {
			return 0, fmt.Errorf("read snapshot %s: %w", key, err)
		}
		data = gz
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name()) // best-effort: the next attempt removes leftovers
		}
	}()
	n, err := io.Copy(tmp, io.LimitReader(data, max(room, 0)+1))
	if err == nil && n > room {
		err = errOverBudget
	}
	if err != nil {
		_ = tmp.Close()
		if errors.Is(err, errOverBudget) {
			return 0, err
		}
		return 0, fmt.Errorf("decompress snapshot %s: %w", key, err)
	}
	if err = tmp.Close(); err != nil {
		return 0, err
	}
	now := c.Now()
	if err = os.Chtimes(tmp.Name(), now, now); err != nil {
		return 0, err
	}
	return n, os.Rename(tmp.Name(), path)
}
