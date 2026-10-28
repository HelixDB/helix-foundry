package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// OntologyGenerationStatus is where a generation is in its run.
type OntologyGenerationStatus string

const (
	OntologyGenerationQueued    OntologyGenerationStatus = "queued"
	OntologyGenerationRunning   OntologyGenerationStatus = "running"
	OntologyGenerationSucceeded OntologyGenerationStatus = "succeeded"
	OntologyGenerationFailed    OntologyGenerationStatus = "failed"
)

// OntologyGeneration is one request to draft a version of an ontology, and the
// leased job the ontology worker claims. An ontology has at most one live one.
type OntologyGeneration struct {
	ID                string
	WorkspaceID       string
	OntologyID        string
	Status            OntologyGenerationStatus
	Goal              *string
	Title             *string // one line naming Goal, at most 60 characters; nil without a goal and before titles
	ReviseResult      json.RawMessage
	Input             OntologyGenerationInput
	LeaseOwner        *string
	LeaseUntil        *time.Time
	Attempts          int
	NextAttemptAt     time.Time
	Error             *string
	ResultVersionID   *string
	Model             *string
	InputTokens       *int
	OutputTokens      *int
	RequestedByUserID *string
	CreatedAt         time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
}

// OntologyGenerationInput is what the generation was asked to cover. The
// worker reads each table's newest snapshot, which may be later than SyncID.
type OntologyGenerationInput struct {
	Revise *OntologyRevision         `json:"revise,omitempty"`
	Tables []OntologyGenerationTable `json:"tables"`
	// DocumentSetIDs are the document sets the draft reads, none when empty:
	// the agent reads each set's newest uploaded documents when the
	// generation runs, and drafts the types, props and links extraction fills
	// from them (a Contract linked to the tables' Customer). Sets deleted
	// since are left out. The version it stores has them as its scope. A
	// revision's are its parent version's scope; the version it stores adds
	// the live sets its revised routes name.
	DocumentSetIDs []string `json:"document_set_ids,omitempty"`
	// Documents are the table files of DocumentSetIDs whose read had
	// succeeded when the generation was requested: each is a table of its
	// own, whose source id is the document's id, which the draft maps
	// beside the synced tables. Loads of its versions read those still
	// uploaded in the version's sets. A revision's are its parent's.
	Documents []OntologyGenerationDocument `json:"documents,omitempty"`
	// BaseVersionID is the version that was current when the generation was
	// requested; the draft extends it.
	BaseVersionID string `json:"base_version_id,omitempty"`
	// Sample is the sample company (sample.Acme) whose ready-made draft
	// the generation stores in place of the AI's, mapped to Documents; empty
	// for an AI draft.
	Sample string `json:"sample,omitempty"`
}

type OntologyGenerationTable struct {
	ConnectorID string `json:"connector_id"`
	Table       string `json:"table"`
	SyncID      string `json:"sync_id"`
}

// OntologyGenerationDocument is a table file a generation drafts from, and
// the set it was in then.
type OntologyGenerationDocument struct {
	ID    string `json:"id"`
	SetID string `json:"set_id"`
}

// OntologyGenerationOutcome ends a generation's attempt. A non-nil RetryAt
// requeues it; otherwise Status must be succeeded, with Result (except a
// revision preview), or failed. ReviseResult stores cards and findings.
// Tokens add to what earlier attempts spent.
type OntologyGenerationOutcome struct {
	Status       OntologyGenerationStatus
	ReviseResult json.RawMessage
	Error        string
	RetryAt      *time.Time
	Result       *OntologyVersion
	Model        string
	// ModelProvider is the provider kind that served Model (anthropic, ...).
	ModelProvider string
	InputTokens   int
	OutputTokens  int
}

// OntologyRevision is a queued edit of an immutable parent. Tables and sets
// remain in Input so the resulting manual version is loadable by its generation.
type OntologyRevision struct {
	ParentVersionID string        `json:"parent_version_id"`
	Edits           OntologyEdits `json:"edits"`
	DryRun          bool          `json:"dry_run"`
}

// OntologyEdit is one edit of a revision: an OntologyPut or an
// OntologyRemove, nothing else, as the unexported method seals it. A put
// always has its TOML and a remove never has any.
type OntologyEdit interface {
	// Item is the draft item the edit puts or removes, such as rule.overdue.
	Item() string
	ontologyEdit()
}

// OntologyPut puts an item's TOML snippet in the draft, replacing the item
// if the draft has it.
type OntologyPut struct {
	ItemID string `json:"item_id"`
	TOML   string `json:"toml"`
}

// OntologyRemove removes an item from the draft.
type OntologyRemove struct {
	ItemID string `json:"item_id"`
}

func (p OntologyPut) Item() string    { return p.ItemID }
func (r OntologyRemove) Item() string { return r.ItemID }
func (OntologyPut) ontologyEdit()     {}
func (OntologyRemove) ontologyEdit()  {}

// OntologyEdits are a revision's edits, in order, in the agent's externally
// tagged JSON, which the generation's input stores and Revise sends:
// {"put":{"item_id","toml"}} or {"remove":{"item_id"}} each. Reading refuses
// any other shape: an unknown or second tag, a put without its toml, a
// remove with one.
type OntologyEdits []OntologyEdit

// MarshalJSON writes each edit under its tag; a nil edit is an error.
func (es OntologyEdits) MarshalJSON() ([]byte, error) {
	tagged := make([]any, 0, len(es))
	for i, e := range es {
		switch e := e.(type) {
		case OntologyPut:
			tagged = append(tagged, map[string]OntologyPut{"put": e})
		case OntologyRemove:
			tagged = append(tagged, map[string]OntologyRemove{"remove": e})
		default:
			return nil, fmt.Errorf("edit %d is neither a put nor a remove", i)
		}
	}
	return json.Marshal(tagged)
}

// UnmarshalJSON reads edits as MarshalJSON writes them, and nothing else.
func (es *OntologyEdits) UnmarshalJSON(raw []byte) error {
	var tagged []json.RawMessage
	if err := json.Unmarshal(raw, &tagged); err != nil {
		return err
	}
	edits := make(OntologyEdits, 0, len(tagged))
	for i, one := range tagged {
		var edit struct {
			Put *struct {
				ItemID *string `json:"item_id"`
				TOML   *string `json:"toml"`
			} `json:"put"`
			Remove *struct {
				ItemID *string `json:"item_id"`
			} `json:"remove"`
		}
		decoder := json.NewDecoder(bytes.NewReader(one))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&edit); err != nil {
			return fmt.Errorf("edit %d: %w", i, err)
		}
		switch {
		case edit.Put != nil && edit.Remove == nil && edit.Put.ItemID != nil && edit.Put.TOML != nil:
			edits = append(edits, OntologyPut{ItemID: *edit.Put.ItemID, TOML: *edit.Put.TOML})
		case edit.Remove != nil && edit.Put == nil && edit.Remove.ItemID != nil:
			edits = append(edits, OntologyRemove{ItemID: *edit.Remove.ItemID})
		default:
			return fmt.Errorf("edit %d is neither {\"put\":{item_id,toml}} nor {\"remove\":{item_id}}", i)
		}
	}
	*es = edits
	return nil
}
