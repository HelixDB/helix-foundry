package ontologyagent

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// graph.go is the agent's graph-build contract (schema/ingest_*.json,
// schema/extract_*.json and schema/delete_load_*.json): the requests the
// foundry-graph-worker builds, and a reading of the events and results that
// keeps the agent's reports as it wrote them.
//
// Every node and edge a load writes carries its id as foundry_load, so loads
// of different versions sit side by side in one database: Ingest creates a
// load, Extract adds documents to it, and DeleteLoad removes it.

// HelixTarget is where Ingest and Extract write: the job's graph proxy, which
// holds the database's credentials so the agent never does.
type HelixTarget struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	// MaxRequestBytes is the largest body the proxy forwards; the agent
	// refuses a larger write before sending it.
	MaxRequestBytes uint64 `json:"max_request_bytes"`
	TimeoutSecs     uint64 `json:"timeout_secs"`
}

// IngestRequest creates a load in a database and fills it from the tables
// through a version's mappings (schema/ingest_request.json).
type IngestRequest struct {
	// RequestID is the idempotency key; every attempt of a job sends its id.
	RequestID string `json:"request_id"`
	// Database names the database to the agent, which writes one at a time
	// to each; Foundry sends the tenant id.
	Database string `json:"database"`
	// Load is the load created: the load job's id, which every attempt, and
	// every later job of the load, sends.
	Load         string `json:"load"`
	OntologyTOML string `json:"ontology_toml"`
	// OntologyVersion is what the graph records in place of the TOML's own:
	// foundry-v<version number>.
	OntologyVersion string      `json:"ontology_version"`
	Helix           HelixTarget `json:"helix"`
	DryRun          bool        `json:"dry_run"`
	MappingsTOML    string      `json:"mappings_toml"`
	// Sources are the tables the mappings read, as the generation named them.
	Sources   []Source `json:"sources"`
	ChunkRows uint64   `json:"chunk_rows"`
	// Reload is how a load that already exists is treated: ReloadDiff brings
	// it up to Sources in place, writing only what changed. Empty refuses.
	Reload Reload `json:"reload,omitempty"`
}

// Reload is what Ingest does with a load the database already holds.
type Reload string

// ReloadDiff diffs each stream against what the load holds by row key and
// row hash: unchanged streams are skipped, changed rows retracted and
// written again, and removed rows retracted with what they alone supported.
const ReloadDiff Reload = "diff"

// MediaType is how a document's text is written, for Extract.
type MediaType string

const (
	MediaMarkdown  MediaType = "markdown"
	MediaPlainText MediaType = "plain_text"
)

// StageText is a document's text as the agent reads it: the media type it is
// sent as, the extension of the file it is staged in, and its text, given
// what model.FoundryDocument.TextKey holds. Markdown and MDX are markdown,
// MDX with its code blanked to the same length (document.BlankMDX), so its
// sections and marks fall on the original's bytes; plain text, and the text
// a PDF's read stored (its pages split by form feeds), are plain_text. The
// engine has no PDF type and needs none. False for a type that is not read
// as text.
func StageText(mediaType model.FoundryMediaType, text []byte) (MediaType, string, []byte, bool) {
	switch mediaType {
	case model.FoundryMediaMarkdown:
		return MediaMarkdown, ".md", text, true
	case model.FoundryMediaMDX:
		return MediaMarkdown, ".md", document.BlankMDX(text), true
	case model.FoundryMediaPlainText, model.FoundryMediaPDF:
		return MediaPlainText, ".txt", text, true
	}
	return "", "", nil, false
}

// ExtractDocument is one document to extract, as a local file.
type ExtractDocument struct {
	// DocID is the Foundry document id, which the graph records it by.
	DocID string `json:"doc_id"`
	// Set is the id of its document set: the routes of the request's
	// mappings that name it, or "all", are its scope. A document no route
	// names is stored with its sections and makes no model call
	// (OutcomeOutOfScope).
	Set string `json:"set"`
	// URI is where the original is kept; the agent records it and never reads it.
	URI       string    `json:"uri"`
	Path      string    `json:"path"`
	MediaType MediaType `json:"media_type"`
	// Artifacts are every saved model output of earlier runs of the
	// document, as local files. The agent replays each model call whose
	// request it finds in any of them and pays only for the rest, so a route
	// edit re-pays only the calls it changes. Never nil.
	Artifacts []string `json:"artifacts"`
}

// ExtractLimits are the hard caps on one Extract.
type ExtractLimits struct {
	MaxUSD            float64 `json:"max_usd"`
	DocumentsInFlight uint64  `json:"documents_in_flight"`
	MaxTextBytes      uint64  `json:"max_text_bytes"`
}

// ExtractRequest extracts documents into a load Ingest created
// (schema/extract_request.json).
type ExtractRequest struct {
	RequestID string `json:"request_id"`
	Database  string `json:"database"`
	// Load is the load written to. Ingest must have created it with
	// OntologyVersion and it must not be being deleted, or nothing is written.
	Load            string `json:"load"`
	OntologyTOML    string `json:"ontology_toml"`
	OntologyVersion string `json:"ontology_version"`
	// MappingsTOML is the version's mappings, whose [extractions.*] routes
	// scope each document by its set.
	MappingsTOML string            `json:"mappings_toml"`
	Helix        HelixTarget       `json:"helix"`
	DryRun       bool              `json:"dry_run"`
	Documents    []ExtractDocument `json:"documents"`
	Limits       ExtractLimits     `json:"limits"`
	// Decisions are ontology-wide overrides for this request (at most 10,000).
	Decisions []ExtractDecision `json:"decisions,omitempty"`
	// Models are the extraction's models, resolved for the workspace; the
	// key goes in the call's metadata.
	Models ExtractModels `json:"models"`
}

// ExtractDecision is one settled review, internally tagged by kind.
// Resolve's entity_key is explicitly null for "not in data"; prop's value
// preserves its JSON scalar, including null. Every decision names itself by
// its id and when it was settled, which the contract takes both or neither
// of: the version of a value a prop decision overwrites keeps them. Who
// decided never leaves the BFF.
type ExtractDecision struct {
	Kind       model.FoundryReviewDecisionKind `json:"kind"`
	Type       string                          `json:"type"`
	DocID      string                          `json:"doc_id,omitempty"`
	NormName   string                          `json:"norm_name,omitempty"`
	EntityKey  *string                         `json:"entity_key"`
	Prop       string                          `json:"prop,omitempty"`
	Value      json.RawMessage                 `json:"value,omitempty"`
	DecisionID string                          `json:"decision_id"`
	DecidedAt  time.Time                       `json:"decided_at"`
}

// IngestStatus is how an Ingest ended.
type IngestStatus string

const (
	IngestDone IngestStatus = "done"
	// IngestLoadMismatch is a load the database holds from another version.
	IngestLoadMismatch IngestStatus = "load_mismatch"
	// IngestLoadUnknown is a load being deleted.
	IngestLoadUnknown IngestStatus = "load_unknown"
	// IngestIndexesPending is an index still building over data the database
	// already held; a retry goes on once it is built.
	IngestIndexesPending    IngestStatus = "indexes_pending"
	IngestSourcesChanged    IngestStatus = "sources_changed"
	IngestInvalidMappings   IngestStatus = "invalid_mappings"
	IngestSourceUnavailable IngestStatus = "source_unavailable"
	IngestOutcomeUnknown    IngestStatus = "outcome_unknown"
	IngestCancelled         IngestStatus = "cancelled"
	IngestStoreFailed       IngestStatus = "store_failed"
	// IngestDiffUnsupported is a ReloadDiff the agent can't apply in place;
	// nothing was written, and a full load gives the same graph.
	IngestDiffUnsupported IngestStatus = "diff_unsupported"
	// IngestPositionMismatch is a changes source whose from_seq isn't where
	// the load holds its stream; nothing was written for those streams.
	IngestPositionMismatch IngestStatus = "position_mismatch"
)

// AppliedStream is what a ReloadDiff did to one stream the request named:
// zero counts for a stream it skipped as unchanged.
type AppliedStream struct {
	Stream    string `json:"stream"`
	Snapshot  string `json:"snapshot"`
	ChangeSeq *int64 `json:"change_seq"`
	Added     uint64 `json:"added"`
	Changed   uint64 `json:"changed"`
	Removed   uint64 `json:"removed"`
}

// StoredPosition is where the load holds a stream a changes source didn't start from.
type StoredPosition struct {
	Stream          string `json:"stream"`
	StoredChangeSeq *int64 `json:"stored_change_seq"`
}

// SourceChange is a stream the database holds from another snapshot or
// mapping version than the request asks for.
type SourceChange struct {
	Stream        string `json:"stream"`
	Stored        string `json:"stored"`
	StoredVersion string `json:"stored_version"`
	Snapshot      string `json:"snapshot"`
	Version       string `json:"version"`
}

// IngestResult is how an Ingest ended (schema/ingest_result.json). Which
// fields are set follows Status: Report for done and cancelled, with Applied
// for a done ReloadDiff; StoredVersion for load_mismatch, Label for
// indexes_pending, Streams for sources_changed, Errors for invalid_mappings,
// Source for source_unavailable, Mapping, Pass, Chunk and Error for
// outcome_unknown, Error for store_failed, Reason and Unsupported for
// diff_unsupported, and Positions for position_mismatch.
type IngestResult struct {
	Status        IngestStatus
	Report        json.RawMessage
	Applied       []AppliedStream
	StoredVersion string
	Label         string
	Streams       []SourceChange
	Errors        []string
	Source        string
	Mapping       string
	Pass          string
	Chunk         uint32
	Error         string
	Reason        string
	Unsupported   []string
	Positions     []StoredPosition
}

// DecodeIngestResult reads a result, refusing one whose status this contract
// doesn't name or that lacks what its status needs.
func DecodeIngestResult(data []byte) (IngestResult, error) {
	var raw struct {
		Status        IngestStatus    `json:"status"`
		Report        json.RawMessage `json:"report"`
		Applied       []AppliedStream `json:"applied"`
		StoredVersion *string         `json:"stored_version"`
		Label         *string         `json:"label"`
		// Streams' shape follows the status: sources_changed's changes,
		// diff_unsupported's names, position_mismatch's positions.
		Streams json.RawMessage `json:"streams"`
		Reason  *string         `json:"reason"`
		Errors  *[]string       `json:"errors"`
		Source  *string         `json:"source"`
		Mapping *string         `json:"mapping"`
		Pass    *string         `json:"pass"`
		Chunk   *uint32         `json:"chunk"`
		Error   *string         `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return IngestResult{}, fmt.Errorf("read ingest result: %w", err)
	}
	r := IngestResult{Status: raw.Status, Report: raw.Report}
	streams := func(into any) bool {
		return len(raw.Streams) > 0 && raw.Streams[0] == '[' && json.Unmarshal(raw.Streams, into) == nil
	}
	missing := false
	switch raw.Status {
	case IngestDone, IngestCancelled:
		missing = len(raw.Report) == 0 || raw.Report[0] != '{'
		r.Applied = raw.Applied
	case IngestLoadMismatch:
		missing = raw.StoredVersion == nil
		if !missing {
			r.StoredVersion = *raw.StoredVersion
		}
	case IngestLoadUnknown:
	case IngestIndexesPending:
		missing = raw.Label == nil
		if !missing {
			r.Label = *raw.Label
		}
	case IngestSourcesChanged:
		missing = !streams(&r.Streams)
	case IngestInvalidMappings:
		missing = raw.Errors == nil
		if !missing {
			r.Errors = *raw.Errors
		}
	case IngestSourceUnavailable:
		missing = raw.Source == nil
		if !missing {
			r.Source = *raw.Source
		}
	case IngestOutcomeUnknown:
		missing = raw.Mapping == nil || raw.Pass == nil || raw.Chunk == nil || raw.Error == nil
		if !missing {
			r.Mapping, r.Pass, r.Chunk, r.Error = *raw.Mapping, *raw.Pass, *raw.Chunk, *raw.Error
		}
	case IngestStoreFailed:
		missing = raw.Error == nil
		if !missing {
			r.Error = *raw.Error
		}
	case IngestDiffUnsupported:
		missing = raw.Reason == nil || !streams(&r.Unsupported)
		if !missing {
			r.Reason = *raw.Reason
		}
	case IngestPositionMismatch:
		missing = !streams(&r.Positions)
	default:
		return IngestResult{}, fmt.Errorf("unknown ingest status %q", raw.Status)
	}
	if missing {
		return IngestResult{}, fmt.Errorf("ingest result %s lacks what its status needs", raw.Status)
	}
	return r, nil
}

// IngestViolations reads, from a done Ingest's report, how many violation
// records the load's tables left open: what each mapping's create and
// resolve passes opened, less what they resolved, plus what the sweep of the
// entities the load created opened, less what it closed (a record it
// updated stays open). The report is the ingest crate's (ingest::Report),
// which the contract leaves as display data, so this reads only the counts
// and is nil when it can't give one: a pass skipped chunks an earlier run
// wrote, whose records it doesn't count; the sweep didn't run in this call;
// or the report isn't one it reads.
func IngestViolations(report json.RawMessage) *uint64 {
	type counted struct {
		Count *uint64 `json:"count"`
	}
	type pass struct {
		Skipped  *uint64 `json:"skipped"`
		Opened   counted `json:"violations_opened"`
		Resolved counted `json:"violations_resolved"`
	}
	var r struct {
		Mappings *[]struct {
			Create  pass `json:"create"`
			Resolve pass `json:"resolve"`
		} `json:"mappings"`
		Created struct {
			Sweep  string  `json:"sweep"`
			Opened counted `json:"opened"`
			Closed counted `json:"closed"`
		} `json:"created"`
	}
	if json.Unmarshal(report, &r) != nil || r.Mappings == nil || r.Created.Sweep != "swept" {
		return nil
	}
	opened, closed := []counted{r.Created.Opened}, []counted{r.Created.Closed}
	for _, m := range *r.Mappings {
		for _, p := range []pass{m.Create, m.Resolve} {
			if p.Skipped == nil || *p.Skipped != 0 {
				return nil
			}
			opened, closed = append(opened, p.Opened), append(closed, p.Resolved)
		}
	}
	// Each sum is of counts of records held in memory, so it can't overflow;
	// a missing count, or more closed than opened, is a report this can't read.
	sum := func(counts []counted) (total uint64, ok bool) {
		for _, c := range counts {
			if c.Count == nil {
				return 0, false
			}
			total += *c.Count
		}
		return total, true
	}
	open, okOpened := sum(opened)
	shut, okClosed := sum(closed)
	if !okOpened || !okClosed || shut > open {
		return nil
	}
	left := open - shut
	return &left
}

// EnrichmentApplied reads, from a done Ingest's report, what the load did
// with People enrichment's profiles: of the rows the item's expansion
// mapping enrichment_<name> read (one per profile), how many bound a person
// (applied) and how many bound no one (unmatched: the person changed since
// the lookup). Only that mapping counts: its work history's mapping also
// counts roles at employers the load doesn't hold. ok is false when the
// report has no such mapping or isn't one it reads.
func EnrichmentApplied(report json.RawMessage, item string) (applied, unmatched int64, ok bool) {
	type pass struct {
		Unmatched int64 `json:"unmatched"`
	}
	var r struct {
		Mappings []struct {
			Mapping string `json:"mapping"`
			Rows    int64  `json:"rows"`
			Create  pass   `json:"create"`
			Resolve pass   `json:"resolve"`
		} `json:"mappings"`
	}
	if json.Unmarshal(report, &r) != nil {
		return 0, 0, false
	}
	for _, m := range r.Mappings {
		if m.Mapping != "enrichment_"+item {
			continue
		}
		unmatched = m.Create.Unmatched + m.Resolve.Unmatched
		if unmatched < 0 || unmatched > m.Rows {
			return 0, 0, false
		}
		return m.Rows - unmatched, unmatched, true
	}
	return 0, 0, false
}

// EventName is an event's kind: every Ingest and Extract event names itself.
func EventName(data []byte) (string, error) {
	var e struct {
		Event string `json:"event"`
	}
	if err := json.Unmarshal(data, &e); err != nil || e.Event == "" {
		return "", fmt.Errorf("read event: %s is not a named event", data)
	}
	return e.Event, nil
}

// OutcomeKind is how one document ended.
type OutcomeKind string

const (
	OutcomeIngested        OutcomeKind = "ingested"
	OutcomeAlreadyIngested OutcomeKind = "already_ingested"
	OutcomeDuplicate       OutcomeKind = "duplicate"
	OutcomeFailed          OutcomeKind = "failed"
	// OutcomeOutOfScope is a document whose set no route of the version
	// names: stored with its sections, with no model call.
	OutcomeOutOfScope OutcomeKind = "out_of_scope"
)

// DocumentOutcome is how one document ended: Of is set for duplicate, Code
// and Retryable for failed, and ModelFailure for a failed code "model".
type DocumentOutcome struct {
	Kind         OutcomeKind  `json:"kind"`
	Of           string       `json:"of"`
	Code         string       `json:"code"`
	ModelFailure ModelFailure `json:"model_failure,omitempty"`
	Retryable    bool         `json:"retryable"`
}

// DocumentCounts are what one document put in the database.
type DocumentCounts struct {
	Entities   uint64 `json:"entities"`
	Mentions   uint64 `json:"mentions"`
	Facts      uint64 `json:"facts"`
	Reviews    uint64 `json:"reviews"`
	Unresolved uint64 `json:"unresolved"`
	Rejected   uint64 `json:"rejected"`
	// Attributes are property values it gave evidence for.
	Attributes uint64 `json:"attributes"`
	// AttributeReviews are attributes the verifier left for review.
	AttributeReviews uint64 `json:"attribute_reviews"`
	// PropConflicts are properties whose evidence disagrees with a row or
	// with other documents, so the property has a review.
	PropConflicts uint64 `json:"prop_conflicts"`
	// Created are entities it created, of types its routes may create.
	Created uint64 `json:"created"`
	// IdentifiersUnverified are identifiers the verifier did not accept,
	// which resolved nothing.
	IdentifiersUnverified uint64 `json:"identifiers_unverified"`
	// AttributesOverEntries are attributes left out because the commit had
	// no entries left for them.
	AttributesOverEntries uint64 `json:"attributes_over_entries"`
	// Resolved counts its entities by how they resolved.
	Resolved ResolvedCounts `json:"resolved"`
}

// ResolvedCounts are a document's entities by how each resolved to the
// load's: by a verified identifier, by name or alias, or created.
type ResolvedCounts struct {
	User        uint64 `json:"user"`
	Adjudicated uint64 `json:"adjudicated"`
	Identifier  uint64 `json:"identifier"`
	Name        uint64 `json:"name"`
	Alias       uint64 `json:"alias"`
	Created     uint64 `json:"created"`
}

// DocumentUsage is what one document spent.
type DocumentUsage struct {
	USDMicros    uint64 `json:"usd_micros"`
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	Replayed     bool   `json:"replayed"`
}

// Artifact is a document's model output, sent before its commit.
type Artifact struct {
	DocID string `json:"doc_id"`
	Key   string `json:"key"`
	JSON  string `json:"json"`
}

// DocumentEvent is a document that reached its final state.
type DocumentEvent struct {
	DocID     string          `json:"doc_id"`
	Outcome   DocumentOutcome `json:"outcome"`
	TextBytes uint64          `json:"text_bytes"`
	Sections  uint64          `json:"sections"`
	Counts    DocumentCounts  `json:"counts"`
	Usage     DocumentUsage   `json:"usage"`
}

// DecodeArtifact reads an artifact event.
func DecodeArtifact(data []byte) (Artifact, error) {
	var a Artifact
	if err := json.Unmarshal(data, &a); err != nil {
		return Artifact{}, fmt.Errorf("read artifact event: %w", err)
	}
	if a.DocID == "" || a.Key == "" || a.JSON == "" {
		return Artifact{}, fmt.Errorf("artifact event lacks its document, key or JSON")
	}
	return a, nil
}

// DecodeDocumentEvent reads a document event, refusing an outcome this
// contract doesn't name or one missing what its kind needs.
func DecodeDocumentEvent(data []byte) (DocumentEvent, error) {
	var raw struct {
		DocumentEvent
		Outcome struct {
			Kind         OutcomeKind   `json:"kind"`
			Of           *string       `json:"of"`
			Code         *string       `json:"code"`
			ModelFailure *ModelFailure `json:"model_failure"`
			Retryable    *bool         `json:"retryable"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return DocumentEvent{}, fmt.Errorf("read document event: %w", err)
	}
	e := raw.DocumentEvent
	e.Outcome = DocumentOutcome{Kind: raw.Outcome.Kind}
	switch o := raw.Outcome; o.Kind {
	case OutcomeIngested, OutcomeAlreadyIngested, OutcomeOutOfScope:
	case OutcomeDuplicate:
		if o.Of == nil || *o.Of == "" {
			return DocumentEvent{}, fmt.Errorf("duplicate document %s names no original", e.DocID)
		}
		e.Outcome.Of = *o.Of
	case OutcomeFailed:
		if o.Code == nil || o.Retryable == nil {
			return DocumentEvent{}, fmt.Errorf("failed document %s has no code", e.DocID)
		}
		switch *o.Code {
		case "model":
			if o.ModelFailure == nil {
				return DocumentEvent{}, fmt.Errorf("failed document %s names no model failure", e.DocID)
			}
			e.Outcome.ModelFailure = *o.ModelFailure
		case "too_large", "unsupported", "store":
			if o.ModelFailure != nil {
				return DocumentEvent{}, fmt.Errorf("failed document %s has a model failure with code %q", e.DocID, *o.Code)
			}
		default:
			return DocumentEvent{}, fmt.Errorf("failed document %s has unknown code %q", e.DocID, *o.Code)
		}
		e.Outcome.Code, e.Outcome.Retryable = *o.Code, *o.Retryable
	default:
		return DocumentEvent{}, fmt.Errorf("document %s has unknown outcome %q", e.DocID, o.Kind)
	}
	if e.DocID == "" {
		return DocumentEvent{}, fmt.Errorf("document event names no document")
	}
	return e, nil
}

// ExtractStatus is how an Extract ended.
type ExtractStatus string

const (
	ExtractDone ExtractStatus = "done"
	// ExtractLoadMismatch is a load the database holds from another version.
	ExtractLoadMismatch ExtractStatus = "load_mismatch"
	// ExtractLoadUnknown is a load the database does not hold, or one being deleted.
	ExtractLoadUnknown ExtractStatus = "load_unknown"
	// ExtractIndexesPending is an index still building over data the database
	// already held; a retry goes on once it is built.
	ExtractIndexesPending  ExtractStatus = "indexes_pending"
	ExtractBudgetExhausted ExtractStatus = "budget_exhausted"
	ExtractCancelled       ExtractStatus = "cancelled"
	ExtractOutcomeUnknown  ExtractStatus = "outcome_unknown"
	ExtractStoreFailed     ExtractStatus = "store_failed"
)

// ExtractResult is how an Extract ended (schema/extract_result.json): Totals
// for done, budget_exhausted and cancelled, StoredVersion for load_mismatch,
// Label for indexes_pending, DocID and Error for outcome_unknown, Error for
// store_failed.
type ExtractResult struct {
	Status        ExtractStatus
	Totals        json.RawMessage
	StoredVersion string
	Label         string
	DocID         string
	Error         string
}

// DecodeExtractResult reads a result, refusing an unknown status or one
// lacking what its status needs.
func DecodeExtractResult(data []byte) (ExtractResult, error) {
	var raw struct {
		Status        ExtractStatus   `json:"status"`
		Totals        json.RawMessage `json:"totals"`
		StoredVersion *string         `json:"stored_version"`
		Label         *string         `json:"label"`
		DocID         *string         `json:"doc_id"`
		Error         *string         `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ExtractResult{}, fmt.Errorf("read extract result: %w", err)
	}
	r := ExtractResult{Status: raw.Status, Totals: raw.Totals}
	missing := false
	switch raw.Status {
	case ExtractDone, ExtractBudgetExhausted, ExtractCancelled:
		missing = len(raw.Totals) == 0 || raw.Totals[0] != '{'
	case ExtractLoadMismatch:
		missing = raw.StoredVersion == nil
		if !missing {
			r.StoredVersion = *raw.StoredVersion
		}
	case ExtractLoadUnknown:
	case ExtractIndexesPending:
		missing = raw.Label == nil
		if !missing {
			r.Label = *raw.Label
		}
	case ExtractOutcomeUnknown:
		missing = raw.DocID == nil || raw.Error == nil
		if !missing {
			r.DocID, r.Error = *raw.DocID, *raw.Error
		}
	case ExtractStoreFailed:
		missing = raw.Error == nil
		if !missing {
			r.Error = *raw.Error
		}
	default:
		return ExtractResult{}, fmt.Errorf("unknown extract status %q", raw.Status)
	}
	if missing {
		return ExtractResult{}, fmt.Errorf("extract result %s lacks what its status needs", raw.Status)
	}
	return r, nil
}

// MaxDeleteBatch is the most nodes or edges one DeleteLoad request may delete.
const MaxDeleteBatch = 5000

// DeleteLoadRequest removes one load from a database: its edges, then its
// nodes, Batch per request, then its marker (schema/delete_load_request.json).
// Deleting the load's nodes also deletes every edge attached to them, edges
// the user attached included; nothing without the load's foundry_load is
// touched.
type DeleteLoadRequest struct {
	// RequestID is the idempotency key; every attempt of a delete job sends its id.
	RequestID string      `json:"request_id"`
	Database  string      `json:"database"`
	Load      string      `json:"load"`
	Helix     HelixTarget `json:"helix"`
	// Batch is the most nodes or edges one request deletes, 1 to MaxDeleteBatch.
	Batch uint64 `json:"batch"`
}

// DeleteLoadStatus is how a DeleteLoad ended.
type DeleteLoadStatus string

const (
	// DeleteLoadDone is a load gone, its marker last; a load the database
	// never held is done with nothing deleted.
	DeleteLoadDone DeleteLoadStatus = "done"
	// DeleteLoadCancelled stopped between requests; the load stays, marked deleting.
	DeleteLoadCancelled DeleteLoadStatus = "cancelled"
	// DeleteLoadOutcomeUnknown is a request that may or may not have committed.
	DeleteLoadOutcomeUnknown DeleteLoadStatus = "outcome_unknown"
	DeleteLoadStoreFailed    DeleteLoadStatus = "store_failed"
)

// DeleteLoadResult is how a DeleteLoad ended (schema/delete_load_result.json):
// Nodes and Edges for done and cancelled, Label and Error for
// outcome_unknown, Error for store_failed. What was deleted before a failure
// stays deleted, and a retry goes on from there.
type DeleteLoadResult struct {
	Status DeleteLoadStatus
	Nodes  uint64
	Edges  uint64
	Label  string
	Error  string
}

// DecodeDeleteLoadResult reads a result, refusing an unknown status or one
// lacking what its status needs.
func DecodeDeleteLoadResult(data []byte) (DeleteLoadResult, error) {
	var raw struct {
		Status DeleteLoadStatus `json:"status"`
		Nodes  *uint64          `json:"nodes"`
		Edges  *uint64          `json:"edges"`
		Label  *string          `json:"label"`
		Error  *string          `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return DeleteLoadResult{}, fmt.Errorf("read delete load result: %w", err)
	}
	r := DeleteLoadResult{Status: raw.Status}
	missing := false
	switch raw.Status {
	case DeleteLoadDone, DeleteLoadCancelled:
		missing = raw.Nodes == nil || raw.Edges == nil
		if !missing {
			r.Nodes, r.Edges = *raw.Nodes, *raw.Edges
		}
	case DeleteLoadOutcomeUnknown:
		missing = raw.Label == nil || raw.Error == nil
		if !missing {
			r.Label, r.Error = *raw.Label, *raw.Error
		}
	case DeleteLoadStoreFailed:
		missing = raw.Error == nil
		if !missing {
			r.Error = *raw.Error
		}
	default:
		return DeleteLoadResult{}, fmt.Errorf("unknown delete load status %q", raw.Status)
	}
	if missing {
		return DeleteLoadResult{}, fmt.Errorf("delete load result %s lacks what its status needs", raw.Status)
	}
	return r, nil
}
