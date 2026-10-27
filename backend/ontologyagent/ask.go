package ontologyagent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// ask.go is the Ask RPC's contract (schema/ask_request.json,
// schema/ask_event.json, schema/ask_result.json): the request the ask worker
// builds, and a check of the events and the result it stores for the
// website, which renders them as the agent wrote them.

const (
	// MaxAskHistory is the most earlier turns of its thread a question carries.
	MaxAskHistory = 5
	// MaxAskVisuals is the most query results one answer shows.
	MaxAskVisuals = 4
	// MaxVisualTitleChars bounds a visual's title, in characters.
	MaxVisualTitleChars = 80
	// maxOutputRows and maxOutputGroups bound a query's recorded output, as
	// the agent bounds the query's limit.
	maxOutputRows   = 200
	maxOutputGroups = 100
	// maxHistoryQueryChars bounds a visual's query in a thread's history
	// (AnswerText), in characters, so 5 turns of 4 visuals stay small.
	maxHistoryQueryChars = 1000
)

// AskRequest is one question's request (schema/ask_request.json).
type AskRequest struct {
	// RequestID is the question's id, the same on every attempt.
	RequestID string `json:"request_id"`
	// Database names the database to the agent: the tenant id.
	Database string `json:"database"`
	// Load is the load the agent reads; its store is bound to it.
	Load string `json:"load"`
	// OntologyVersion is what the load recorded as its version: foundry-v<n>.
	OntologyVersion string `json:"ontology_version"`
	OntologyTOML    string `json:"ontology_toml"`
	MappingsTOML    string `json:"mappings_toml"`
	// DraftJSON and QuestionsJSON are the version's Definition.draft and
	// Definition.questions, as JSON text.
	DraftJSON     string `json:"draft_json"`
	QuestionsJSON string `json:"questions_json"`
	// Goal is the brief of the generation that drafted the version.
	Goal    string         `json:"goal"`
	Catalog []CatalogEntry `json:"catalog"`
	// Question is the user's question.
	Question string `json:"question"`
	// Focus is the entity the user asked it about, if any. The agent reads it
	// before the first step; one the load doesn't hold is said so in the
	// question, which still runs.
	Focus *EntityRef `json:"focus,omitempty"`
	// History is the thread's earlier answered turns, oldest first, at most
	// MaxAskHistory. Never nil: the schema needs an array.
	History []AskTurn `json:"history"`
	// Helix is the question's read-only proxy.
	Helix HelixTarget `json:"helix"`
	// AsOf is when the question was asked (RFC 3339), the `today` its rules see.
	AsOf   string `json:"as_of"`
	Limits Limits `json:"limits"`
	// Models is the question's model, resolved for the workspace; the key
	// goes in the call's metadata.
	Models AskModels `json:"models"`
}

// EntityRef is one entity of a load: its type, and its identity in the load,
// the key's encoding for a keyed type (such as `scus_1`) or the normalized
// name for a named one.
type EntityRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

func (e EntityRef) whole() bool { return e.Type != "" && e.ID != "" }

// ParseFocus reads what a question is asked about: exactly
// {"type": ..., "id": ...}, both set.
func ParseFocus(raw string) (EntityRef, error) {
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	var focus EntityRef
	if err := d.Decode(&focus); err != nil || d.More() || !focus.whole() {
		return EntityRef{}, errors.New(`a focus is {"type": ..., "id": ...}, both set`)
	}
	return focus, nil
}

// CatalogKind is what a source in the catalog is.
type CatalogKind string

const (
	CatalogTable       CatalogKind = "table"
	CatalogDocumentSet CatalogKind = "document_set"
)

// CatalogEntry names one source of the version to the agent: the id its
// artifacts use, and what the user calls it.
type CatalogEntry struct {
	ID   string      `json:"id"`
	Kind CatalogKind `json:"kind"`
	Name string      `json:"name"`
}

// AskTurn is one earlier question of the thread and its answer as plain text.
type AskTurn struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// TableSourceID is the id the agent knows a connector table by: the first 8
// bytes of SHA-256 of the connector id, NUL and the table, so every turn and
// question names one connector table alike.
func TableSourceID(connectorID, table string) string {
	sum := sha256.Sum256([]byte(connectorID + "\x00" + table))
	return "src_" + hex.EncodeToString(sum[:8])
}

// TableDisplayName is what the user calls a connector table, as a
// connector's document set is called (model.FoundryDocumentSet.DisplayName).
func TableDisplayName(connectorName, table string) string {
	return model.ConnectorPartName(connectorName, table)
}

// AskEventKind is what an Ask event reports, its "event" field.
type AskEventKind string

const (
	AskStarted    AskEventKind = "started"
	AskToolCall   AskEventKind = "tool_call"
	AskToolResult AskEventKind = "tool_result"
	AskFinished   AskEventKind = "finished"
)

// AskEvent is one event of an Ask stream (schema/ask_event.json): one of
// AskStartedEvent, AskToolCallEvent, AskToolResultEvent and AskFinishedEvent.
type AskEvent interface {
	Kind() AskEventKind
}

// AskStartedEvent opens the stream, naming the model that answers.
type AskStartedEvent struct {
	Model ModelRef
}

// AskToolCallEvent is a tool the agent called, with its input as the model
// wrote it; a query's input is the typed query itself.
type AskToolCallEvent struct {
	CallID string
	Tool   string
	Input  json.RawMessage
}

// AskToolResultEvent is what a call returned: a line saying what it found,
// and at most one of EvidenceID, for a read recorded as evidence the answer
// may cite, and Error, for a call that failed or was refused. The answer or
// cannot_answer call that ends the question has neither.
type AskToolResultEvent struct {
	CallID     string
	Summary    string
	EvidenceID string
	Error      *string
}

// AskFinishedEvent closes the events; the result follows.
type AskFinishedEvent struct{}

func (AskStartedEvent) Kind() AskEventKind    { return AskStarted }
func (AskToolCallEvent) Kind() AskEventKind   { return AskToolCall }
func (AskToolResultEvent) Kind() AskEventKind { return AskToolResult }
func (AskFinishedEvent) Kind() AskEventKind   { return AskFinished }

// DecodeAskEvent reads an event, refusing a kind this contract doesn't name
// or one missing a field its kind needs. The worker checks the agent speaks
// this contract before it asks, so nothing else arrives from a sound agent.
func DecodeAskEvent(data []byte) (AskEvent, error) {
	var raw struct {
		Event      AskEventKind    `json:"event"`
		Model      *string         `json:"model"`
		Provider   *ProviderKind   `json:"provider"`
		CallID     *string         `json:"call_id"`
		Tool       *string         `json:"tool"`
		Input      json.RawMessage `json:"input"`
		EvidenceID *string         `json:"evidence_id"`
		Summary    *string         `json:"summary"`
		Error      *string         `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("read ask event: %w", err)
	}
	switch raw.Event {
	case AskStarted:
		if raw.Model == nil || *raw.Model == "" || raw.Provider == nil || !slices.Contains(ProviderKinds, *raw.Provider) {
			return nil, fmt.Errorf("started event names no model or provider")
		}
		return AskStartedEvent{Model: ModelRef{Provider: *raw.Provider, Name: *raw.Model}}, nil
	case AskToolCall:
		if raw.CallID == nil || *raw.CallID == "" || raw.Tool == nil || *raw.Tool == "" || isNull(raw.Input) {
			return nil, fmt.Errorf("tool_call event lacks its call id, tool or input")
		}
		return AskToolCallEvent{CallID: *raw.CallID, Tool: *raw.Tool, Input: raw.Input}, nil
	case AskToolResult:
		switch {
		case raw.CallID == nil || *raw.CallID == "" || raw.Summary == nil:
			return nil, fmt.Errorf("tool_result event lacks its call id or summary")
		case raw.EvidenceID != nil && *raw.EvidenceID == "":
			return nil, fmt.Errorf("tool_result event has an empty evidence id")
		case raw.EvidenceID != nil && raw.Error != nil:
			return nil, fmt.Errorf("tool_result event has both an evidence id and an error")
		}
		e := AskToolResultEvent{CallID: *raw.CallID, Summary: *raw.Summary, Error: raw.Error}
		if raw.EvidenceID != nil {
			e.EvidenceID = *raw.EvidenceID
		}
		return e, nil
	case AskFinished:
		return AskFinishedEvent{}, nil
	}
	return nil, fmt.Errorf("ask event %.200s has an unknown kind %q", data, raw.Event)
}

// AskStatus is how a question ended.
type AskStatus string

const (
	AskAnswered        AskStatus = "answered"
	AskCannotAnswer    AskStatus = "cannot_answer"
	AskBudgetExhausted AskStatus = "budget_exhausted"
	AskFailed          AskStatus = "failed"
)

// AskFailure is why a question failed, a failed result's reason. A model
// call's failure is its ModelFailure prefixed model_ (ModelFailure reads it),
// but for unavailable, which is model_failed, and a question too long for
// the model, which ends budget_exhausted instead.
type AskFailure string

const (
	// AskModelFailed is a model call that failed after the agent's retries:
	// the provider was unavailable.
	AskModelFailed AskFailure = "model_failed"
	// AskModelRefused is a model that declined.
	AskModelRefused     AskFailure = "model_refused"
	AskModelAuth        AskFailure = "model_auth"
	AskModelPermission  AskFailure = "model_permission"
	AskModelNotFound    AskFailure = "model_not_found"
	AskModelRateLimited AskFailure = "model_rate_limited"
	AskModelQuota       AskFailure = "model_quota"
	AskModelUnsupported AskFailure = "model_unsupported"
	AskModelTruncated   AskFailure = "model_truncated"
	AskModelMalformed   AskFailure = "model_malformed"
	AskModelTimeout     AskFailure = "model_timeout"
	AskModelChanged     AskFailure = "model_changed"
	AskModelPrivateHost AskFailure = "model_private_host"
	// AskNoAnswer is a model that stopped, reminded, without answer or cannot_answer.
	AskNoAnswer AskFailure = "no_answer"
	// AskLoadUnknown is a graph that holds no such load, or is deleting it.
	AskLoadUnknown AskFailure = "load_unknown"
	// AskLoadMismatch is a load of another version.
	AskLoadMismatch AskFailure = "load_mismatch"
	// AskStoreFailed is a graph that couldn't be read.
	AskStoreFailed AskFailure = "store_failed"
)

// askModelFailures are the reasons that are a model call's failure.
var askModelFailures = map[AskFailure]ModelFailure{
	AskModelFailed: ModelUnavailable, AskModelRefused: ModelRefused, AskModelAuth: ModelAuth, AskModelPermission: ModelPermission,
	AskModelNotFound: ModelNotFound, AskModelRateLimited: ModelRateLimited, AskModelQuota: ModelQuota,
	AskModelUnsupported: ModelUnsupported, AskModelTruncated: ModelTruncated, AskModelMalformed: ModelMalformed,
	AskModelTimeout: ModelTimeout, AskModelChanged: ModelChanged, AskModelPrivateHost: ModelPrivateHost,
}

// AskFailures are every reason a question fails for.
var AskFailures = []AskFailure{
	AskModelFailed, AskModelRefused, AskModelAuth, AskModelPermission, AskModelNotFound, AskModelRateLimited, AskModelQuota,
	AskModelUnsupported, AskModelTruncated, AskModelMalformed, AskModelTimeout, AskModelChanged, AskModelPrivateHost,
	AskNoAnswer, AskLoadUnknown, AskLoadMismatch, AskStoreFailed,
}

// ModelFailure is how the model call failed, when the reason is a model
// call's failure.
func (f AskFailure) ModelFailure() (ModelFailure, bool) {
	m, ok := askModelFailures[f]
	return m, ok
}

// AskResult is how a question ended (schema/ask_result.json). The worker
// keeps the JSON the agent sent as the question's result, for the website to
// render; this is what it checked that JSON holds.
type AskResult struct {
	Status AskStatus
	// Answer and Citations are an answered question's, nil otherwise.
	Answer    *AskAnswer
	Citations []AskCitation
	// Reason and Gap say, in plain text, why a cannot_answer question can't
	// be answered and what the ontology or the data would need.
	Reason string
	Gap    string
	// Failure is why a failed question failed.
	Failure AskFailure
	// Queries are the typed queries the agent ran, by their results' evidence.
	Queries []AskQuery
	Usage   Usage
}

// AskAnswer is an answer: plain text in blocks, with [cN] markers and no
// links, images or HTML, and the query results it shows as tables or charts.
type AskAnswer struct {
	Blocks []AnswerBlock `json:"blocks"`
	// Visuals are at most MaxAskVisuals query results, each of a query of the
	// result with an output and shown once. Nil for an answer that shows
	// none, as every answer of a contract before 3.11.0.
	Visuals []AskVisual `json:"visuals,omitempty"`
}

// VisualKind is how a visual shows its result. It is a hint: the website
// shows a table when the output's shape doesn't suit it.
type VisualKind string

const (
	// VisualAuto leaves the kind to the website, from the output's shape.
	VisualAuto   VisualKind = ""
	VisualTable  VisualKind = "table"
	VisualBar    VisualKind = "bar"
	VisualLine   VisualKind = "line"
	VisualFigure VisualKind = "figure"
)

var visualKinds = []VisualKind{VisualAuto, VisualTable, VisualBar, VisualLine, VisualFigure}

// visualNames name each kind in a thread's history (AnswerText).
var visualNames = map[VisualKind]string{
	VisualAuto: "a table or a chart", VisualTable: "a table", VisualBar: "a bar chart", VisualLine: "a line chart", VisualFigure: "a figure",
}

// AskVisual is a query result an answer shows as a table or a chart. What it
// shows is the output the agent recorded for the query (AskQuery.Output),
// never text the model wrote; the model chose only which result, where, the
// kind and the title.
type AskVisual struct {
	// EvidenceID is the query's result, as AskQuery names it.
	EvidenceID string `json:"evidence_id"`
	// After is the index of the block it follows, from 0; nil puts it after
	// the last block.
	After *uint32    `json:"after,omitempty"`
	Kind  VisualKind `json:"kind,omitempty"`
	// Title is plain text of at most MaxVisualTitleChars characters; empty
	// for none.
	Title string `json:"title,omitempty"`
}

// AnswerBlock is a paragraph or a list: exactly one of Para and List is set.
type AnswerBlock struct {
	Para *string  `json:"para,omitempty"`
	List []string `json:"list,omitempty"`
}

// AskCitation points marker ("c1" for [c1]) at a target the tool result
// recorded as evidence EvidenceID showed.
type AskCitation struct {
	Marker     string         `json:"marker"`
	EvidenceID string         `json:"evidence_id"`
	Target     CitationTarget `json:"target"`
}

// CitationTarget is what a citation points at: exactly one of its fields is set.
type CitationTarget struct {
	Entity    *EntityRef       `json:"entity,omitempty"`
	Fact      *FactTarget      `json:"fact,omitempty"`
	Derived   *DerivedTarget   `json:"derived,omitempty"`
	Row       *RowTarget       `json:"row,omitempty"`
	Quote     *QuoteTarget     `json:"quote,omitempty"`
	Violation *ViolationTarget `json:"violation,omitempty"`
	Result    *ResultTarget    `json:"result,omitempty"`
	Version   *VersionTarget   `json:"version,omitempty"`
}

// FactTarget is a stated fact: a link from its subject to its object.
type FactTarget struct {
	Link    string    `json:"link"`
	Subject EntityRef `json:"subject"`
	Object  EntityRef `json:"object"`
}

// DerivedTarget is a fact the rules derive: `link:<l>`, `relation:<r>`,
// `derived:<d>` or `violation:<c>` over its arguments.
type DerivedTarget struct {
	Pred string      `json:"pred"`
	Args []EntityRef `json:"args"`
}

// RowTarget is a source row, by its source, the mapping that read it and its
// key. A People enrichment row (EnrichmentSourceID) has no key: its key is
// the provider's profile id, which a stored result must not keep.
type RowTarget struct {
	Source  string `json:"source"`
	Mapping string `json:"mapping"`
	RowKey  string `json:"row_key"`
}

// QuoteTarget is a span of a document: a section, from 0, and byte offsets
// Start to End (exclusive) into that section's text.
type QuoteTarget struct {
	DocID   string `json:"doc_id"`
	Section uint32 `json:"section"`
	Start   uint64 `json:"start"`
	End     uint64 `json:"end"`
}

// ViolationTarget is a stored violation record, by its constraint and key.
type ViolationTarget struct {
	Constraint string `json:"constraint"`
	Key        string `json:"key"`
}

// ResultTarget is a whole tool result, such as a query's count.
type ResultTarget struct {
	EvidenceID string `json:"evidence_id"`
}

// VersionTarget is a state the load recorded, as a history showed it: what
// Of was as of the transaction Tx, the load's own opaque id for it, which
// committed At. It names that one state, which the load may no longer hold.
type VersionTarget struct {
	Tx string    `json:"tx"`
	At time.Time `json:"at"`
	Of VersionOf `json:"of"`
}

// VersionOf is what a recorded version is of: exactly one of its fields is
// set. Record is a violation record, open or not.
type VersionOf struct {
	Entity *EntityRef       `json:"entity,omitempty"`
	Prop   *PropTarget      `json:"prop,omitempty"`
	Fact   *FactTarget      `json:"fact,omitempty"`
	Record *ViolationTarget `json:"record,omitempty"`
}

// PropTarget is one of an entity's properties.
type PropTarget struct {
	Entity EntityRef `json:"entity"`
	Prop   string    `json:"prop"`
}

// AskQuery is a typed ontology query the agent ran, as it wrote it with its
// entity ids resolved, by the evidence id of its result, with the output the
// agent recorded for it.
type AskQuery struct {
	EvidenceID string          `json:"evidence_id"`
	Query      json.RawMessage `json:"query"`
	// Output is the query's output as the agent computed it, before it was
	// cut for the model, kept as the agent wrote it: the Read call's query
	// answer by its "kind" (rows, count, group_count, aggregate or
	// grouped_aggregate), at most 200 rows or 100 groups, and an optional
	// "cut" saying what was left out. The outputs of a result are at most
	// 512 KiB together. Nil when none was recorded: in a result of a contract
	// before 3.11.0, or past that bound for a query no visual shows.
	Output json.RawMessage `json:"output,omitempty"`
}

// DecodeAskResult reads a result, refusing a status this contract doesn't
// name, a missing status, usage or queries, and what a status lacks: an
// answer with its blocks and citations, a cannot_answer's reason and gap, a
// failure's known reason. It refuses an answer on another status, a block
// that is not exactly a paragraph or a list, a citation without its marker,
// evidence or exactly one whole target (a row with a key, but a People
// enrichment row without one; a version with its transaction, its time and
// exactly one whole thing it is of), a marker cited twice, two queries of
// one evidence, an output that isn't a query answer, and a visual
// checkVisuals refuses. A result of a
// contract before 3.11.0 reads as one with no outputs and no visuals.
func DecodeAskResult(data []byte) (AskResult, error) {
	var raw struct {
		Status    *AskStatus      `json:"status"`
		Answer    *AskAnswer      `json:"answer"`
		Citations *[]AskCitation  `json:"citations"`
		Reason    json.RawMessage `json:"reason"`
		Gap       *string         `json:"gap"`
		Queries   *[]AskQuery     `json:"queries"`
		Usage     *Usage          `json:"usage"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return AskResult{}, fmt.Errorf("read ask result: %w", err)
	}
	switch {
	case raw.Status == nil:
		return AskResult{}, fmt.Errorf("ask result has no status")
	case raw.Queries == nil || raw.Usage == nil:
		return AskResult{}, fmt.Errorf("ask result has no queries or usage")
	case *raw.Status != AskAnswered && raw.Answer != nil:
		return AskResult{}, fmt.Errorf("ask result is %s with an answer", *raw.Status)
	}
	r := AskResult{Status: *raw.Status, Queries: *raw.Queries, Usage: *raw.Usage}
	switch r.Status {
	case AskAnswered:
		if raw.Answer == nil || len(raw.Answer.Blocks) == 0 || raw.Citations == nil {
			return AskResult{}, fmt.Errorf("ask result is answered with no answer or citations")
		}
		r.Answer, r.Citations = raw.Answer, *raw.Citations
	case AskCannotAnswer:
		if isNull(raw.Reason) || json.Unmarshal(raw.Reason, &r.Reason) != nil || raw.Gap == nil {
			return AskResult{}, fmt.Errorf("ask result is cannot_answer with no reason or gap")
		}
		r.Gap = *raw.Gap
	case AskFailed:
		if json.Unmarshal(raw.Reason, &r.Failure) != nil || !slices.Contains(AskFailures, r.Failure) {
			return AskResult{}, fmt.Errorf("ask result failed with an unknown reason %s", raw.Reason)
		}
	case AskBudgetExhausted:
	default:
		return AskResult{}, fmt.Errorf("unknown ask status %q", r.Status)
	}
	if r.Answer != nil {
		for i, b := range r.Answer.Blocks {
			if (b.Para == nil) == (b.List == nil) {
				return AskResult{}, fmt.Errorf("answer block %d is not exactly a paragraph or a list", i)
			}
		}
	}
	markers := make(map[string]bool, len(r.Citations))
	for _, c := range r.Citations {
		switch {
		case c.Marker == "" || c.EvidenceID == "":
			return AskResult{}, fmt.Errorf("a citation lacks its marker or evidence")
		case markers[c.Marker]:
			return AskResult{}, fmt.Errorf("citation %s is given twice", c.Marker)
		}
		markers[c.Marker] = true
		if err := c.Target.validate(); err != nil {
			return AskResult{}, fmt.Errorf("citation %s: %w", c.Marker, err)
		}
	}
	// outputs says, of each query's evidence, whether its output was recorded.
	outputs := make(map[string]bool, len(r.Queries))
	for i := range r.Queries {
		q := &r.Queries[i]
		if q.EvidenceID == "" || isNull(q.Query) || q.Query[0] != '{' {
			return AskResult{}, fmt.Errorf("a query lacks its evidence or itself")
		}
		if _, twice := outputs[q.EvidenceID]; twice {
			return AskResult{}, fmt.Errorf("two queries are recorded as %s", q.EvidenceID)
		}
		if isNull(q.Output) {
			q.Output = nil
		} else if err := checkOutput(q.Output); err != nil {
			return AskResult{}, fmt.Errorf("query %s's output: %w", q.EvidenceID, err)
		}
		outputs[q.EvidenceID] = q.Output != nil
	}
	if r.Answer != nil {
		if err := r.Answer.checkVisuals(outputs); err != nil {
			return AskResult{}, err
		}
	}
	return r, nil
}

// checkVisuals checks an answer shows at most MaxAskVisuals results, each
// once, each of a query whose output was recorded (outputs, by evidence id),
// after one of its blocks, of a kind this contract names, titled within
// MaxVisualTitleChars.
func (a AskAnswer) checkVisuals(outputs map[string]bool) error {
	if len(a.Visuals) > MaxAskVisuals {
		return fmt.Errorf("the answer has %d visuals, more than %d", len(a.Visuals), MaxAskVisuals)
	}
	shown := make(map[string]bool, len(a.Visuals))
	for i, v := range a.Visuals {
		recorded, ran := outputs[v.EvidenceID]
		switch {
		case !ran:
			return fmt.Errorf("visual %d shows %q, not a query of the question", i, v.EvidenceID)
		case !recorded:
			return fmt.Errorf("visual %d shows %s, whose output was not recorded", i, v.EvidenceID)
		case shown[v.EvidenceID]:
			return fmt.Errorf("two visuals show %s", v.EvidenceID)
		case v.After != nil && uint64(*v.After) >= uint64(len(a.Blocks)):
			return fmt.Errorf("visual %d follows block %d of %d", i, *v.After, len(a.Blocks))
		case !slices.Contains(visualKinds, v.Kind):
			return fmt.Errorf("visual %d is of an unknown kind %q", i, v.Kind)
		case utf8.RuneCountInString(v.Title) > MaxVisualTitleChars:
			return fmt.Errorf("visual %d's title is over %d characters", i, MaxVisualTitleChars)
		}
		shown[v.EvidenceID] = true
	}
	return nil
}

// checkOutput checks a recorded output is the Read call's query answer of a
// kind this contract names, with the fields its kind needs and at most
// maxOutputRows rows or maxOutputGroups groups, and that its cut, if any, is
// text. Cells and group values are left as the agent wrote them: the website
// shows them as text.
func checkOutput(raw json.RawMessage) error {
	type queryAnswer struct {
		Kind string `json:"kind"`
		Rows *[]struct {
			Cells *[]json.RawMessage `json:"cells"`
			Edges *[]json.RawMessage `json:"edges"`
		} `json:"rows"`
		Groups *[]struct {
			Value     json.RawMessage `json:"value"`
			Count     *uint64         `json:"count"`
			Aggregate json.RawMessage `json:"aggregate"`
			Values    *uint64         `json:"values"`
		} `json:"groups"`
		Total       *uint64         `json:"total"`
		Count       *uint64         `json:"count"`
		TotalGroups *uint64         `json:"total_groups"`
		Value       json.RawMessage `json:"value"`
		Values      *uint64         `json:"values"`
		Cut         *string         `json:"cut"`
	}
	var o queryAnswer
	if err := json.Unmarshal(raw, &o); err != nil {
		return err
	}
	// number is a field that is there, a number or null.
	number := func(raw json.RawMessage) bool {
		var n *float64
		return len(raw) > 0 && json.Unmarshal(raw, &n) == nil
	}
	switch o.Kind {
	case "rows":
		if o.Rows == nil || o.Total == nil || len(*o.Rows) > maxOutputRows {
			return fmt.Errorf("rows are not at most %d rows and their total", maxOutputRows)
		}
		for i, row := range *o.Rows {
			if row.Cells == nil || row.Edges == nil {
				return fmt.Errorf("row %d lacks its cells or edges", i)
			}
		}
	case "count":
		if o.Count == nil {
			return errors.New("a count lacks its count")
		}
	case "aggregate":
		if !number(o.Value) || o.Values == nil {
			return errors.New("an aggregate lacks its value or values")
		}
	case "group_count", "grouped_aggregate":
		if o.Groups == nil || o.TotalGroups == nil || len(*o.Groups) > maxOutputGroups {
			return fmt.Errorf("%s is not at most %d groups and their total", o.Kind, maxOutputGroups)
		}
		for i, g := range *o.Groups {
			whole := len(g.Value) > 0 && g.Count != nil
			if o.Kind == "grouped_aggregate" {
				whole = len(g.Value) > 0 && number(g.Aggregate) && g.Values != nil
			}
			if !whole {
				return fmt.Errorf("%s group %d is not whole", o.Kind, i)
			}
		}
	default:
		return fmt.Errorf("unknown kind %q", o.Kind)
	}
	return nil
}

// validate checks exactly one target is set, and that it is whole: a
// version is whole with its transaction, its time and exactly one whole
// thing it is of.
func (t CitationTarget) validate() error {
	set, whole := 0, true
	if t.Entity != nil {
		set, whole = set+1, t.Entity.whole()
	}
	if t.Fact != nil {
		set, whole = set+1, t.Fact.whole()
	}
	if t.Derived != nil {
		set, whole = set+1, t.Derived.Pred != "" && t.Derived.Args != nil
		for _, a := range t.Derived.Args {
			whole = whole && a.whole()
		}
	}
	if t.Row != nil {
		set, whole = set+1, t.Row.Source != "" && t.Row.Mapping != "" && (t.Row.RowKey == "") == (t.Row.Source == EnrichmentSourceID)
	}
	if t.Quote != nil {
		set, whole = set+1, t.Quote.DocID != "" && t.Quote.Start <= t.Quote.End
	}
	if t.Violation != nil {
		set, whole = set+1, t.Violation.whole()
	}
	if t.Result != nil {
		set, whole = set+1, t.Result.EvidenceID != ""
	}
	if t.Version != nil {
		set, whole = set+1, t.Version.Tx != "" && !t.Version.At.IsZero() && t.Version.Of.whole()
	}
	switch {
	case set != 1:
		return fmt.Errorf("it has %d targets, not one", set)
	case !whole:
		return fmt.Errorf("its target is not whole")
	}
	return nil
}

func (f FactTarget) whole() bool { return f.Link != "" && f.Subject.whole() && f.Object.whole() }

func (v ViolationTarget) whole() bool { return v.Constraint != "" && v.Key != "" }

// whole reports exactly one of what a version can be of is set, and whole.
func (o VersionOf) whole() bool {
	set, whole := 0, true
	if o.Entity != nil {
		set, whole = set+1, o.Entity.whole()
	}
	if o.Prop != nil {
		set, whole = set+1, o.Prop.Entity.whole() && o.Prop.Prop != ""
	}
	if o.Fact != nil {
		set, whole = set+1, o.Fact.whole()
	}
	if o.Record != nil {
		set, whole = set+1, o.Record.whole()
	}
	return set == 1 && whole
}

// isNull reports a JSON value that is absent or null.
func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

// AnswerText is a stored result's answer as plain text: its paragraphs, and
// its lists' items one a line, blocks a blank line apart. Citation markers
// stay. Each visual is a line after the block it follows, saying what it
// showed and the query it showed the result of (cut past
// maxHistoryQueryChars), so a later question can refer to its rows and the
// agent can read them again; the outputs themselves are left out. It is
// empty for a result with no answer, or one that isn't a result.
func AnswerText(result json.RawMessage) string {
	r, err := DecodeAskResult(result)
	if err != nil || r.Answer == nil {
		return ""
	}
	queries := make(map[string]json.RawMessage, len(r.Queries))
	for _, q := range r.Queries {
		queries[q.EvidenceID] = q.Query
	}
	// shown are the visuals' lines by the block they follow.
	shown := make(map[int][]string, len(r.Answer.Visuals))
	for _, v := range r.Answer.Visuals {
		after := len(r.Answer.Blocks) - 1
		if v.After != nil {
			after = int(*v.After)
		}
		var query bytes.Buffer
		_ = json.Compact(&query, queries[v.EvidenceID]) // decoded as JSON, so it compacts
		text := query.String()
		if runes := []rune(text); len(runes) > maxHistoryQueryChars {
			text = string(runes[:maxHistoryQueryChars]) + "…"
		}
		title := ""
		if v.Title != "" {
			title = fmt.Sprintf(" titled %q,", v.Title)
		}
		shown[after] = append(shown[after], fmt.Sprintf("[Shown here, not in the text: %s%s of the result of the query %s]",
			visualNames[v.Kind], title, text))
	}
	blocks := make([]string, 0, len(r.Answer.Blocks)+len(r.Answer.Visuals))
	for i, b := range r.Answer.Blocks {
		switch {
		case b.Para != nil:
			blocks = append(blocks, *b.Para)
		default:
			blocks = append(blocks, "- "+strings.Join(b.List, "\n- "))
		}
		blocks = append(blocks, shown[i]...)
	}
	return strings.Join(blocks, "\n\n")
}

// WithoutOutputs is a stored result with its queries' outputs left out, for a
// list of questions: an output is up to 512 KiB of rows, and a list page holds
// many answers. The rest is kept, visuals too: a listed answer's visuals name
// outputs it leaves out, which the question itself carries. Only a result
// that had an output is written again, its keys perhaps in another order and
// its text as it was (no HTML escapes); one with none, or anything that isn't
// a result, comes back as it is.
func WithoutOutputs(result json.RawMessage) json.RawMessage {
	var doc map[string]json.RawMessage
	var queries []map[string]json.RawMessage
	if json.Unmarshal(result, &doc) != nil || json.Unmarshal(doc["queries"], &queries) != nil {
		return result
	}
	cut := false
	for _, q := range queries {
		_, had := q["output"]
		cut = cut || had
		delete(q, "output")
	}
	if !cut {
		return result
	}
	marshal := func(v any) ([]byte, error) {
		var b bytes.Buffer
		e := json.NewEncoder(&b)
		e.SetEscapeHTML(false)
		err := e.Encode(v)
		return bytes.TrimSuffix(b.Bytes(), []byte("\n")), err
	}
	var err error
	if doc["queries"], err = marshal(queries); err != nil {
		return result
	}
	out, err := marshal(doc)
	if err != nil {
		return result
	}
	return out
}
