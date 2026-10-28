// Package ontologyagent drafts Helix Foundry ontologies with the ontology
// agent. The agent is a sidecar of the foundry-ontology-worker pod, serving
// ontology_agent.v1.Agent on loopback; Generator runs one start turn per
// ontology generation, on the models the workspace's AI setting resolves to.
//
// contract.go is the agent's JSON contract, vendored in
// pb/proto/ontology_agent/v1/schema: the requests the worker builds, and a
// lenient reading of what comes back that keeps the state, the draft and the
// bundle as the agent wrote them.
package ontologyagent

import (
	"encoding/json"
	"fmt"
	"time"
)

// ContractVersion is the contract the vendored schemas describe, the first
// field of pb/proto/ontology_agent/v1/schema/VERSION. A generation fails
// rather than call an agent whose GetInfo reports another.
const ContractVersion = "4.2.0"

// TurnRequest is one turn's request (schema/turn_request.json).
type TurnRequest struct {
	SessionID string `json:"session_id"`
	// RequestID is the idempotency key: every attempt of a turn sends the
	// same one. An agent run with a response store replays what it already
	// paid for; Foundry's sidecar runs without one.
	RequestID string `json:"request_id"`
	// State is the last turn's state, sent back as it came; nil (JSON null)
	// for a start, the only turn Foundry sends so far.
	State   json.RawMessage `json:"state"`
	Sources []Source        `json:"sources"`
	Input   Input           `json:"input"`
	Limits  Limits          `json:"limits"`
	// Models are the draft's model and its extraction previews', resolved
	// for the workspace; the key goes in the call's metadata.
	Models TurnModels `json:"models"`
}

// SourceKind is what a source is, which fixes its route, its trust and how
// it is read.
type SourceKind int

const (
	// SourceTable is a table, mapped and fully trusted: a synced table, or
	// a table file of a document set, as a file in Format.
	SourceTable SourceKind = iota
	// SourceDocuments is a document set: a few of its documents, as text
	// files, which extraction routes ([extractions.*]) name by the set's id.
	SourceDocuments
	// SourceProfiles is People enrichment's profiles (EnrichmentSource): a
	// JSONL file, mapped at EnrichmentTrust, the only trust the engine takes
	// for it. Its catalog is the engine's constant one, so a zero-byte file
	// is a source of no rows.
	SourceProfiles
)

// FileFormat is how a table's file is written. The zero value is JSON
// Lines, which every synced table and converted table file is.
type FileFormat int

const (
	// FileJSONL is one JSON object per line: a row each.
	FileJSONL FileFormat = iota
	// FileJSON is one JSON document, whose arrays of objects the agent
	// catalogs by pointer and mappings read with records and parent.
	FileJSON
)

// String is the format as the contract names it.
func (f FileFormat) String() string {
	if f == FileJSON {
		return "json"
	}
	return "jsonl"
}

// People enrichment's built-in source, as the engine reserves it.
const (
	// EnrichmentSourceID is the source's reserved id; no user mapping reads it.
	EnrichmentSourceID = "foundry_enrichment"
	// EnrichmentTrust is what everything its rows state is worth.
	EnrichmentTrust = 0.7
)

// EnrichmentSource is People enrichment's profiles as a source: the file at
// path, relative to the agent's files root, whose fingerprint identifies its
// bytes. A turn gets a zero-byte file it never reads; a load the profiles it
// applies.
func EnrichmentSource(path, fingerprint string) Source {
	return Source{Kind: SourceProfiles, ID: EnrichmentSourceID, DisplayName: "People enrichment", Path: path, Fingerprint: fingerprint}
}

// trust is how far the engine trusts what a mapped source's rows state.
func (k SourceKind) trust() float64 {
	if k == SourceProfiles {
		return EnrichmentTrust
	}
	return 1
}

// Source is a table or a document set, as local files under the agent's
// files root. Foundry sends only these, at paths relative to that root, so
// nothing else can be expressed.
type Source struct {
	Kind SourceKind
	// ID is what the agent's artifacts name the source by: ASCII letters,
	// digits, '_' and '-'. Stable for one connector table; a table file's
	// document id; a document set's own id, which its routes and every
	// Extract name it by.
	ID string
	// DisplayName is what the user calls it; the agent keeps it out of the
	// artifacts.
	DisplayName string
	// Description is what it holds in the user's words; empty for none.
	Description string
	// Path is a table's file, relative to the agent's files root, written
	// in Format.
	Path   string
	Format FileFormat
	// Changes, when set, is sent in place of Path: the table's changes since
	// the position the load holds it at. Only a ReloadDiff takes it.
	Changes *ChangeFile
	// Documents are a document set's staged documents.
	Documents []DocumentFile
	// Fingerprint identifies the data; the agent measures changed data afresh.
	Fingerprint string
	// Observed is when a table's data was observed: the sync of its pinned
	// snapshot, or a table file's upload. Nil when unknown, and for a
	// document set and People enrichment's profiles, whose rows it isn't.
	Observed Observed
}

// Observed is when a source's data was observed (SourceDescriptor.observed):
// ObservedSync or ObservedUpload, nothing else, as the unexported method
// seals it. An Ingest records it on the transaction of each chunk of the
// source's rows; a turn and Clues read nothing of it.
type Observed interface {
	json.Marshaler
	observed()
}

// ObservedSync is the connector sync a table's snapshot was taken at: when
// it observed the table, the sync's id, and the table's change batch the
// snapshot holds every change up to (0 before the first).
type ObservedSync struct {
	At        time.Time
	SyncID    string
	ChangeSeq uint64
}

// ObservedUpload is the upload of a table file, by its document id.
type ObservedUpload struct {
	DocumentID string
	UploadedAt time.Time
}

func (ObservedSync) observed()   {}
func (ObservedUpload) observed() {}

// MarshalJSON writes {"sync": {"at": ..., "sync_id": ..., "change_seq": ...}}.
func (o ObservedSync) MarshalJSON() ([]byte, error) {
	type sync struct {
		At        time.Time `json:"at"`
		SyncID    string    `json:"sync_id"`
		ChangeSeq uint64    `json:"change_seq"`
	}
	return json.Marshal(struct {
		Sync sync `json:"sync"`
	}{sync(o)})
}

// MarshalJSON writes {"upload": {"document_id": ..., "uploaded_at": ...}}.
func (o ObservedUpload) MarshalJSON() ([]byte, error) {
	type upload struct {
		DocumentID string    `json:"document_id"`
		UploadedAt time.Time `json:"uploaded_at"`
	}
	return json.Marshal(struct {
		Upload upload `json:"upload"`
	}{upload(o)})
}

// DocumentFile is one document of a document set, as a local text file.
type DocumentFile struct {
	DocID string `json:"doc_id"`
	// Path is relative to the agent's files root.
	Path      string    `json:"path"`
	MediaType MediaType `json:"media_type"`
	// Title is the document's name, as the user uploaded it.
	Title string `json:"title"`
}

// MarshalJSON writes a SourceDescriptor: a table is routed through mappings
// with full trust and read as a file in its format, and People
// enrichment's profiles at EnrichmentTrust as a JSONL file; a document set
// is routed to extraction and read through its documents' files. observed
// is left out when Observed is nil.
func (s Source) MarshalJSON() ([]byte, error) {
	type mapping struct {
		Trust float64 `json:"trust"`
	}
	type file struct {
		Path   string `json:"path"`
		Format string `json:"format"`
	}
	type documents struct {
		Files []DocumentFile `json:"files"`
	}
	var route, access any = struct {
		Mapping mapping `json:"mapping"`
	}{mapping{Trust: s.Kind.trust()}}, struct {
		File file `json:"file"`
	}{file{Path: s.Path, Format: s.Format.String()}}
	switch {
	case s.Kind == SourceDocuments:
		route, access = "extraction", struct {
			Documents documents `json:"documents"`
		}{documents{Files: s.Documents}}
	case s.Changes != nil:
		access = struct {
			Changes *ChangeFile `json:"changes"`
		}{s.Changes}
	}
	return json.Marshal(struct {
		ID          string   `json:"id"`
		DisplayName string   `json:"display_name"`
		Description string   `json:"description,omitempty"`
		Route       any      `json:"route"`
		Access      any      `json:"access"`
		Fingerprint string   `json:"fingerprint"`
		Observed    Observed `json:"observed,omitempty"`
	}{s.ID, s.DisplayName, s.Description, route, access, s.Fingerprint, s.Observed})
}

// Limits are the hard caps on one turn.
type Limits struct {
	MaxUSD      float64 `json:"max_usd"`
	MaxSteps    uint32  `json:"max_steps"`
	MaxWallSecs uint64  `json:"max_wall_secs"`
}

// Input is what a turn is for. Foundry sends only starts: a first build of a
// draft from the user's brief, with no base bundle.
type Input struct {
	Start Start `json:"start"`
}

// Start is a first build of a draft.
type Start struct {
	// Workflow is what the user does, in their words.
	Workflow string `json:"workflow"`
	// Goals are what they want the graph to answer. Never nil: the schema
	// needs an array.
	Goals []string `json:"goals"`
}

// StatusKind is how a turn ended.
type StatusKind string

const (
	StatusDone              StatusKind = "done"
	StatusNeedsInput        StatusKind = "needs_input"
	StatusApproved          StatusKind = "approved"
	StatusBudgetExhausted   StatusKind = "budget_exhausted"
	StatusSourceUnavailable StatusKind = "source_unavailable"
	StatusModelFailed       StatusKind = "model_failed"
	StatusInvalidInput      StatusKind = "invalid_input"
	StatusStaleDraft        StatusKind = "stale_draft"
)

// Status is how a turn ended, with the detail the worker acts on. The
// invalid_input errors stay in the result for the user.
type Status struct {
	Kind StatusKind
	// Source is the source that kept failing, for StatusSourceUnavailable.
	Source string
	// Failure is how the model failed, for StatusModelFailed.
	Failure ModelFailure
}

// UnmarshalJSON reads a status in either of its forms: a name ("done") or a
// tagged object ({"model_failed": {"reason": "unavailable"}}). A status this contract
// doesn't name is an error, since the worker can't decide what it means.
func (s *Status) UnmarshalJSON(data []byte) error {
	var name StatusKind
	if json.Unmarshal(data, &name) == nil {
		switch name {
		case StatusDone, StatusNeedsInput, StatusApproved, StatusBudgetExhausted, StatusStaleDraft:
			*s = Status{Kind: name}
			return nil
		}
		return fmt.Errorf("unknown turn status %q", name)
	}
	var tagged struct {
		SourceUnavailable *struct {
			Source string `json:"source"`
		} `json:"source_unavailable"`
		ModelFailed *struct {
			Reason ModelFailure `json:"reason"`
		} `json:"model_failed"`
		InvalidInput json.RawMessage `json:"invalid_input"`
	}
	if err := json.Unmarshal(data, &tagged); err != nil {
		return fmt.Errorf("read turn status: %w", err)
	}
	switch {
	case tagged.SourceUnavailable != nil:
		*s = Status{Kind: StatusSourceUnavailable, Source: tagged.SourceUnavailable.Source}
	case tagged.ModelFailed != nil && tagged.ModelFailed.Reason != "":
		*s = Status{Kind: StatusModelFailed, Failure: tagged.ModelFailed.Reason}
	case tagged.InvalidInput != nil:
		*s = Status{Kind: StatusInvalidInput}
	default:
		return fmt.Errorf("unknown turn status %s", data)
	}
	return nil
}

// Usage is what a turn spent: tokens by kind, and dollars.
type Usage struct {
	Input      uint64  `json:"input"`
	CacheRead  uint64  `json:"cache_read"`
	CacheWrite uint64  `json:"cache_write"`
	Output     uint64  `json:"output"`
	USD        float64 `json:"usd"`
}

// RuleCounts are a draft's rules and constraints, and how many of them the
// agent evaluated against data before finishing (TurnResult.rules).
type RuleCounts struct {
	Count     uint32 `json:"count"`
	Evaluated uint32 `json:"evaluated"`
}

// TurnResult is what a turn returned (schema/turn_result.json). State, Draft,
// Questions and Bundle are kept as the agent wrote them.
type TurnResult struct {
	Status Status
	// State is opaque; the agent bounds it at about 1 MB.
	State json.RawMessage
	// Draft is the draft with its rendered files, as the agent sent it.
	Draft json.RawMessage
	// OntologyTOML and MappingsTOML are the draft's rendered files.
	OntologyTOML string
	MappingsTOML string
	// Questions are the questions the agent asked the user about the draft.
	Questions json.RawMessage
	// Bundle is the approved bundle, set exactly when Status is approved.
	Bundle json.RawMessage
	// Summary is what the model said of the turn when it finished it.
	Summary *string
	// Rules are the draft's rule counts; nil when the agent sent none.
	Rules *RuleCounts
	Usage Usage
}

// DecodeTurnResult reads a result. Fields it doesn't know are ignored; a
// missing state, draft or approved bundle is an error.
func DecodeTurnResult(data []byte) (TurnResult, error) {
	var raw struct {
		Status  *Status         `json:"status"`
		State   json.RawMessage `json:"state"`
		Draft   json.RawMessage `json:"draft"`
		Bundle  json.RawMessage `json:"bundle"`
		Summary *string         `json:"summary"`
		Rules   *RuleCounts     `json:"rules"`
		Usage   Usage           `json:"usage"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return TurnResult{}, fmt.Errorf("read turn result: %w", err)
	}
	var view struct {
		OntologyTOML *string `json:"ontology_toml"`
		MappingsTOML *string `json:"mappings_toml"`
		Draft        struct {
			Questions json.RawMessage `json:"questions"`
		} `json:"draft"`
	}
	if len(raw.Draft) > 0 {
		if err := json.Unmarshal(raw.Draft, &view); err != nil {
			return TurnResult{}, fmt.Errorf("read turn result draft: %w", err)
		}
	}
	hasBundle := len(raw.Bundle) > 0 && string(raw.Bundle) != "null"
	switch {
	case raw.Status == nil:
		return TurnResult{}, fmt.Errorf("turn result has no status")
	case len(raw.State) == 0 || raw.State[0] != '{':
		return TurnResult{}, fmt.Errorf("turn result has no state")
	case view.OntologyTOML == nil || view.MappingsTOML == nil:
		return TurnResult{}, fmt.Errorf("turn result has no rendered draft")
	case hasBundle != (raw.Status.Kind == StatusApproved):
		return TurnResult{}, fmt.Errorf("turn result %s has bundle=%t", raw.Status.Kind, hasBundle)
	case raw.Rules != nil && raw.Rules.Evaluated > raw.Rules.Count:
		return TurnResult{}, fmt.Errorf("turn result evaluated %d of %d rules", raw.Rules.Evaluated, raw.Rules.Count)
	}
	result := TurnResult{
		Status: *raw.Status, State: raw.State, Draft: raw.Draft, OntologyTOML: *view.OntologyTOML, MappingsTOML: *view.MappingsTOML,
		Questions: view.Draft.Questions, Summary: raw.Summary, Rules: raw.Rules, Usage: raw.Usage,
	}
	if len(result.Questions) == 0 {
		result.Questions = json.RawMessage(`[]`)
	}
	if hasBundle {
		result.Bundle = raw.Bundle
	}
	return result, nil
}
