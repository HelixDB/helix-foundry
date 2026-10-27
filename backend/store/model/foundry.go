package model

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// FoundryOntology is a workspace's model of its data, bound to one Helix
// database: a tenant of one of the workspace's projects, which the user chose
// or created with the tenant routes. At most one ontology holds a tenant, so a
// database has one Foundry writer. Deleting an ontology deletes its loads
// from the database and keeps the rest; it holds the database until they are
// gone.
type FoundryOntology struct {
	ID          string
	WorkspaceID string
	Name        string
	ProjectID   string
	TenantID    string
	// CurrentVersionID is the version the ontology shows; nil before one is chosen.
	CurrentVersionID *string
	// CurrentLoadID is the load reads go to and uploads are extracted into;
	// nil before a load finished writing, and once the ontology is deleted.
	CurrentLoadID *string
	// ExtractOwed means an upload, or a change of current load, came while a
	// job was live: that job's end queues an extract of the current load.
	ExtractOwed bool
	// ClientToken makes creating an ontology idempotent per workspace.
	ClientToken     string
	CreatedByUserID *string
	CreatedAt       time.Time
	DeletedAt       *time.Time
	// AutoRefresh lets a changed table reload the current version by itself.
	AutoRefresh bool
	// RefreshDueAt is when an owed refresh may be queued; nil when none is owed.
	RefreshDueAt *time.Time
}

// FoundryLoadOrigin is what created a load.
type FoundryLoadOrigin string

const (
	FoundryLoadByUser FoundryLoadOrigin = "user"
	// FoundryLoadByRefresh is a load a changed table queued.
	FoundryLoadByRefresh FoundryLoadOrigin = "refresh"
)

// FoundryLoadStatus is where a load is in its life.
type FoundryLoadStatus string

const (
	// FoundryLoadWriting is a load whose load job is live.
	FoundryLoadWriting FoundryLoadStatus = "writing"
	// FoundryLoadReady is a load whose load job succeeded.
	FoundryLoadReady FoundryLoadStatus = "ready"
	// FoundryLoadPartial is a load whose load job ran out of budget: its
	// tables are in, some documents are not.
	FoundryLoadPartial FoundryLoadStatus = "partial"
	// FoundryLoadFailed is a load whose load job failed; it may hold part of
	// its tables.
	FoundryLoadFailed FoundryLoadStatus = "failed"
	// FoundryLoadDeleting is a load a delete job removes, oldest first.
	FoundryLoadDeleting FoundryLoadStatus = "deleting"
	// FoundryLoadDeleteFailed is a load whose delete job failed for good;
	// only deleting it again retries it.
	FoundryLoadDeleteFailed FoundryLoadStatus = "delete_failed"
	// FoundryLoadDeleted is gone from the database.
	FoundryLoadDeleted FoundryLoadStatus = "deleted"
)

// Servable reports a load that may be the current one.
func (s FoundryLoadStatus) Servable() bool {
	return s == FoundryLoadReady || s == FoundryLoadPartial
}

// Retained reports a load that counts toward the loads an ontology keeps.
func (s FoundryLoadStatus) Retained() bool {
	return s.Servable() || s == FoundryLoadFailed
}

// Deletable reports a load the user may delete, unless it is current or a
// job targets it.
func (s FoundryLoadStatus) Deletable() bool {
	return s.Retained() || s == FoundryLoadDeleteFailed
}

// FoundryLoad is one write of an approved version into an ontology's
// database. Its id is the id of the load job that creates it, and every node
// and edge it writes carries that id as foundry_load.
type FoundryLoad struct {
	ID          string
	WorkspaceID string
	OntologyID  string
	VersionID   string
	// Version is the version's number, which the graph records as foundry-v<Version>.
	Version int
	Status  FoundryLoadStatus
	// IngestViolations is how many violation records the load's ingest left
	// open, as its report counted them; nil when the report did not count
	// every chunk. Extraction and later sweeps change the live records, not
	// this count.
	IngestViolations *uint64
	// FinishedAt is when writing ended; nil exactly while writing.
	FinishedAt *time.Time
	CreatedAt  time.Time
	// DeletedAt is set exactly once the load is deleted.
	DeletedAt *time.Time
	Origin    FoundryLoadOrigin
	// RefreshesLoadID is the current load a refresh replaces.
	RefreshesLoadID *string
	// AppliedAt is when an apply job last brought the load's tables up to
	// date in place; nil until one has.
	AppliedAt *time.Time
}

// OntologyVersion is the version the graph records the load under.
func (l *FoundryLoad) OntologyVersion() string {
	return fmt.Sprintf("foundry-v%d", l.Version)
}

// FoundryLoadJobs is what a load's jobs show: the newest few, and the count
// and spend of them all. A current load gains an extract per upload for as
// long as it is current, so its jobs are not bounded and only the newest are
// read.
type FoundryLoadJobs struct {
	// Newest are its newest jobs, newest first.
	Newest []*FoundryGraphJob
	// Count is how many jobs it has had, at least len(Newest).
	Count int
	// USDMicros is what all of them spent.
	USDMicros int64
}

// FoundryDocumentStatus is where a document is in its upload. A connector's
// document is only ever uploaded or deleted: its sync writes the bytes
// before the row.
type FoundryDocumentStatus string

const (
	// FoundryDocumentPendingUpload is an upload's row written before the
	// bytes; the browser PUTs them to the presigned URL, then completes the
	// upload.
	FoundryDocumentPendingUpload FoundryDocumentStatus = "pending_upload"
	// FoundryDocumentUploaded has its bytes: an upload's checked against its
	// declared size and checksum, a connector's written by its sync. They
	// never change after; a connector's new text is a new document that
	// replaces it.
	FoundryDocumentUploaded FoundryDocumentStatus = "uploaded"
	// FoundryDocumentDeleted has no original left: deleted by a person, or
	// a connector's document replaced or removed at its source. What it put
	// in the database stays there.
	FoundryDocumentDeleted FoundryDocumentStatus = "deleted"
)

// FoundryMediaType is a document's format, as the browser declares it and the
// presigned PUT signs it as Content-Type. The file extension picks it at
// upload (document.MediaTypeOf) and a rename keeps it. Foundry stores and
// shows every one; how ontologies read it is its ReadKind.
type FoundryMediaType string

const (
	FoundryMediaMarkdown FoundryMediaType = "text/markdown"
	// FoundryMediaMDX is Markdown with JSX and JavaScript expressions. It is
	// read as Markdown once its code is blanked (document.BlankMDX); it is
	// never compiled or run.
	FoundryMediaMDX       FoundryMediaType = "text/mdx"
	FoundryMediaPlainText FoundryMediaType = "text/plain"
	FoundryMediaJSON      FoundryMediaType = "application/json"
	FoundryMediaCSV       FoundryMediaType = "text/csv"
	FoundryMediaTSV       FoundryMediaType = "text/tab-separated-values"
	FoundryMediaJSONL     FoundryMediaType = "application/x-ndjson"
	FoundryMediaParquet   FoundryMediaType = "application/vnd.apache.parquet"
	FoundryMediaPDF       FoundryMediaType = "application/pdf"
)

// FoundryMediaTypes are the types Foundry stores, in a fixed order.
func FoundryMediaTypes() []FoundryMediaType {
	return []FoundryMediaType{
		FoundryMediaMarkdown, FoundryMediaMDX, FoundryMediaPlainText, FoundryMediaJSON, FoundryMediaCSV,
		FoundryMediaTSV, FoundryMediaJSONL, FoundryMediaParquet, FoundryMediaPDF,
	}
}

// FoundryMediaTypesRead are the types Foundry stores that ontologies read
// as one of kinds, in FoundryMediaTypes' order.
//
//	FoundryMediaTypesRead(FoundryReadText) // text/markdown, text/mdx, text/plain
func FoundryMediaTypesRead(kinds ...FoundryReadKind) []FoundryMediaType {
	var out []FoundryMediaType
	for _, t := range FoundryMediaTypes() {
		if slices.Contains(kinds, t.ReadKind()) {
			out = append(out, t)
		}
	}
	return out
}

// FoundryReadKind is how ontologies read a type of document.
type FoundryReadKind string

const (
	// FoundryReadText is read as text, by extraction: Markdown, MDX and
	// plain text.
	FoundryReadText FoundryReadKind = "text"
	// FoundryReadTable loads as a table of its own, through mappings, whose
	// source id is the document's id: CSV, TSV, JSON, JSONL and Parquet. The
	// document worker reads it once (foundry_document_reads); its text is
	// never extracted.
	FoundryReadTable FoundryReadKind = "table"
	// FoundryReadPDF is read as text once the document worker has read its
	// text (foundry_document_reads), and not before: a PDF whose read is
	// still to end waits in its loads, and one whose read failed is
	// recorded unsupported there.
	FoundryReadPDF FoundryReadKind = "pdf"
)

// ReadKind is how ontologies read the type; "" for a type Foundry does not
// store.
func (t FoundryMediaType) ReadKind() FoundryReadKind {
	switch t {
	case FoundryMediaMarkdown, FoundryMediaMDX, FoundryMediaPlainText:
		return FoundryReadText
	case FoundryMediaJSON, FoundryMediaCSV, FoundryMediaTSV, FoundryMediaJSONL, FoundryMediaParquet:
		return FoundryReadTable
	case FoundryMediaPDF:
		return FoundryReadPDF
	}
	return ""
}

// Format names the type for people, as in "PDF documents have no text"; ""
// for a type Foundry does not store.
func (t FoundryMediaType) Format() string {
	switch t {
	case FoundryMediaMarkdown:
		return "Markdown"
	case FoundryMediaMDX:
		return "MDX"
	case FoundryMediaPlainText:
		return "Plain text"
	case FoundryMediaJSON:
		return "JSON"
	case FoundryMediaCSV:
		return "CSV"
	case FoundryMediaTSV:
		return "TSV"
	case FoundryMediaJSONL:
		return "JSONL"
	case FoundryMediaParquet:
		return "Parquet"
	case FoundryMediaPDF:
		return "PDF"
	}
	return ""
}

// Binary reports a type whose bytes are not text: Parquet and PDF. The Data
// document page reads those as bytes, never as text.
func (t FoundryMediaType) Binary() bool {
	return t == FoundryMediaParquet || t == FoundryMediaPDF
}

// FoundryGeneralSetName names the set the website makes when a file is
// dropped into a workspace with no set, and each ontology's first set before
// sets belonged to the workspace: the workspace-foundry-documents step
// renames those that collide to their ontology's name.
const FoundryGeneralSetName = "General"

// FoundryDocumentSet is a named group of a workspace's documents, a folder of
// the Data tree. Each document belongs to exactly one set. A version reads
// the sets of its scope (OntologyVersion.DocumentSetIDs), and its extraction
// routes ([extractions.<name>] in its mappings) name the sets whose
// documents fill which types, props and links: a document of its scope whose
// set no route names is stored but read by none.
type FoundryDocumentSet struct {
	ID          string
	WorkspaceID string
	// Name is unique among the workspace's live sets that no connector owns,
	// under the column's collation (case and accents aside). A connector's
	// set is named for its stream, as "Notes": DisplayName says whose.
	Name string
	// Description is what its documents hold, in the user's words or its
	// stream's; the ontology agent reads it. Empty when the user gave none.
	Description string
	// Owner is the connector whose syncs write the set; nil for a set a
	// person made. People can't change a connector's set or its documents.
	Owner           *FoundryDocumentSetOwner
	CreatedByUserID *string
	CreatedAt       time.Time
	// Documents counts its documents that are not deleted, when it was read.
	Documents int
}

// FoundryDocumentSetOwner is the connector and stream a set holds the
// documents of: one set per connector and stream, for as long as the
// connector lives.
type FoundryDocumentSetOwner struct {
	ConnectorID string
	// ConnectorKind and ConnectorName are the connector's kind ("granola",
	// ConnectorKindEvents) and name when the set was read; a kind may give
	// its sets an allowance (FoundrySetAllowance).
	ConnectorKind string
	ConnectorName string
	// Stream is the kind of document the set holds, the kind's own name
	// for it ("notes"); it never changes.
	Stream string
}

// DisplayName is what the user calls the set: its name, or "<connection> /
// <name>" for a connector's, as a connector's table is called
// (ConnectorPartName), since connections' sets share stream names.
func (s *FoundryDocumentSet) DisplayName() string {
	if s.Owner == nil {
		return s.Name
	}
	return ConnectorPartName(s.Owner.ConnectorName, s.Name)
}

// FoundryDocumentSetUpdate changes a set's name, its description, or both;
// a nil field is left as it is.
type FoundryDocumentSetUpdate struct {
	Name        *string
	Description *string
}

// FoundryDocument is one document of a workspace: one a person uploaded, or
// the text of one row of a connector's table (Source), which the
// connector's syncs write into its set.
type FoundryDocument struct {
	ID          string
	WorkspaceID string
	// SetID is the document set it belongs to. Moving an upload to another
	// set applies from the next load: membership captures the set when it
	// joins. A connector's document never moves.
	SetID     string
	Name      string
	MediaType FoundryMediaType
	SizeBytes int64
	// SHA256 is the lowercase hex digest of its bytes: for an upload, the
	// one the browser declared and S3 checked.
	SHA256 string
	// ClientToken makes creating an upload idempotent per workspace; nil
	// for a connector's document, exactly when Source is not.
	ClientToken *string
	// Source is the row of a connector's table the document is the text
	// of; nil for an upload.
	Source          *FoundryDocumentSource
	Status          FoundryDocumentStatus
	S3Key           string
	CreatedByUserID *string
	CreatedAt       time.Time
	UploadedAt      *time.Time
	DeletedAt       *time.Time
	// Read is its standard read (foundry_document_reads): nil for a text
	// document, and for a table file or a PDF before its upload queued one.
	Read *FoundryFileRead
}

// FoundryDocumentSource is where a connector's document comes from.
type FoundryDocumentSource struct {
	// ExternalID is the key of its row: at most one uploaded document of
	// a set has it.
	ExternalID string
	// Version is the source's version of the row's text; a new one with the
	// same bytes changes the document in place.
	Version string
	// At is the item's own time at the source (a note's creation), which
	// orders the set's documents, newest first.
	At time.Time
	// URL is the item's page at the source, an absolute https URL; nil
	// when it has none.
	URL *string
}

// ConnectorDocument is one entry of a connector set's live index: the
// uploaded document of one row, which a sync keeps while the source's
// version is the same. At is its time at the source (its Source.At), which
// orders a stream's window of newest documents.
type ConnectorDocument struct {
	ID         string
	ExternalID string
	Version    string
	SHA256     string
	At         time.Time
}

// ConnectorDocumentsPublish is what one flush of a connector's sync changes
// in one of its sets, in one transaction.
type ConnectorDocumentsPublish struct {
	WorkspaceID string
	ConnectorID string
	SetID       string
	// Add are new documents of the set, each with its Source, no client
	// token and no uploader, whose bytes are in and whose ids were reserved
	// (a purge row each) within Grace/2.
	Add []*FoundryDocument
	// Update changes documents of the set in place: the same bytes, a new
	// version.
	Update []ConnectorDocumentUpdate
	// Remove are documents of the set replaced or gone at the source.
	Remove []string
	// Grace is how long the purge sweep leaves a purge row
	// (ListFoundryDocumentPurges' before is now less Grace). A reservation
	// older than Grace/2 is refused, so the sweep never deletes the bytes
	// of a document being published.
	Grace time.Duration
	// Fan reads Add into the ontologies that read the set. A sync's has no
	// requester and may write every database: the trust a refresh has. A
	// stream whose kind has an allowance flushes with Fan.SystemOnly.
	Fan FoundryDocumentFanOut
}

// ConnectorDocumentUpdate is a new version of a connector's document whose
// bytes did not change: its name, version, time and link change in place,
// and nothing is extracted again.
type ConnectorDocumentUpdate struct {
	ID      string
	Name    string
	Version string
	At      time.Time
	URL     *string
}

// FoundryDocumentPurge is a document whose prefix (its original and
// extractions) the sync worker's sweep deletes: a sync's reservation no
// publish claimed, or a connector's document replaced or removed at the
// source.
type FoundryDocumentPurge struct {
	DocumentID  string
	WorkspaceID string
	CreatedAt   time.Time
}

// FoundryDocumentCap is a workspace's document cap: at most Max documents
// that are uploaded or were created since PendingSince waiting for their
// bytes. An older pending upload was abandoned (its URL expired) and no
// longer counts.
type FoundryDocumentCap struct {
	Max          int
	PendingSince time.Time
}

// FoundryDocumentReadOutcome is what completing or moving a document did for
// one ontology that reads its set: one whose live job's load, or current
// load, is of a version whose scope holds the set.
type FoundryDocumentReadOutcome string

const (
	// FoundryDocumentReadQueued queued an extract of the document into the
	// ontology's current load.
	FoundryDocumentReadQueued FoundryDocumentReadOutcome = "queued"
	// FoundryDocumentReadOwed left it to the ontology's live job: the job
	// reads it, or its end queues the extract that does.
	FoundryDocumentReadOwed FoundryDocumentReadOutcome = "owed"
	// FoundryDocumentReadNoBudget queued nothing: the workspace has spent
	// its month's budget. The ontology's next load or extract reads it.
	FoundryDocumentReadNoBudget FoundryDocumentReadOutcome = "no_budget"
	// FoundryDocumentReadHeld queued nothing: the current load already holds
	// the document (a move), and keeps what it read until the next load.
	FoundryDocumentReadHeld FoundryDocumentReadOutcome = "held"
	// FoundryDocumentReadSkipped queued and owed nothing: the caller may not
	// write the ontology's database. A later load or extract reads it.
	FoundryDocumentReadSkipped FoundryDocumentReadOutcome = "skipped"
	// FoundryDocumentReadReading queued and owed nothing yet: the document
	// is a PDF whose text is still being read. It has joined the load, and
	// the read's end has the ontology read it, as this would have.
	FoundryDocumentReadReading FoundryDocumentReadOutcome = "reading"
)

// FoundryDocumentRead is the outcome of completing or moving a document for
// one ontology that reads its set.
type FoundryDocumentRead struct {
	OntologyID string
	Outcome    FoundryDocumentReadOutcome
}

// FoundryDocumentFanOut is what completing or moving a document hands every
// ontology that reads its set: the extract each may queue (its requester, an
// id per ontology), the month's budget, and which databases the caller may
// write (by project id). An ontology whose database it may not is skipped.
type FoundryDocumentFanOut struct {
	NewJobID          func() string
	RequestedByUserID *string
	Budget            FoundryGraphBudget
	CanWrite          func(projectID string) bool
	// SystemOnly means the documents are work no person asked for (a sync
	// of a stream whose kind has a FoundrySetAllowance), so their extraction
	// is spent from that allowance: they never join a load a person's job
	// is writing, and that job's end owes their extract instead, which no
	// one requests. It has no requester (RequestedByUserID nil).
	SystemOnly bool
}

// FoundryDocumentFilter narrows an ontology's documents to how its current
// load holds them. The zero value keeps every one.
type FoundryDocumentFilter string

const (
	FoundryDocumentFilterAll FoundryDocumentFilter = ""
	// FoundryDocumentFilterWaiting is an uploaded document the current load
	// has not extracted (no load is current, or it has not got to it yet).
	FoundryDocumentFilterWaiting FoundryDocumentFilter = "waiting"
	// FoundryDocumentFilterExtracted, Duplicate, Failed and OutOfScope are
	// the extraction's status in the current load.
	FoundryDocumentFilterExtracted  FoundryDocumentFilter = "extracted"
	FoundryDocumentFilterDuplicate  FoundryDocumentFilter = "duplicate"
	FoundryDocumentFilterFailed     FoundryDocumentFilter = "failed"
	FoundryDocumentFilterOutOfScope FoundryDocumentFilter = "out_of_scope"
	// FoundryDocumentFilterReview is an extracted document that left reviews.
	FoundryDocumentFilterReview FoundryDocumentFilter = "review"
)

// Valid reports a filter the list knows.
func (f FoundryDocumentFilter) Valid() bool {
	switch f {
	case FoundryDocumentFilterAll, FoundryDocumentFilterWaiting, FoundryDocumentFilterExtracted, FoundryDocumentFilterDuplicate,
		FoundryDocumentFilterFailed, FoundryDocumentFilterOutOfScope, FoundryDocumentFilterReview:
		return true
	}
	return false
}

// FoundryDataLayout is how a workspace's Data tree is arranged: the
// website's JSON object, which the backend stores as it is. Revision counts
// its writes; a workspace that never wrote one has revision 0 and {}.
type FoundryDataLayout struct {
	WorkspaceID     string
	Layout          json.RawMessage
	Revision        int64
	UpdatedByUserID *string
	UpdatedAt       *time.Time
}

// FoundryExtractionStatus is how a document ended in its ontology's
// database. A document with no extraction has not been extracted yet.
type FoundryExtractionStatus string

const (
	FoundryExtractionExtracted FoundryExtractionStatus = "extracted"
	// FoundryExtractionDuplicate is text the database already holds under
	// another document; DuplicateOf names it.
	FoundryExtractionDuplicate FoundryExtractionStatus = "duplicate"
	// FoundryExtractionFailed carries an Error.
	FoundryExtractionFailed FoundryExtractionStatus = "failed"
	// FoundryExtractionOutOfScope is a document no extraction route of the
	// load's version reads: the database holds it and its sections, and no
	// model was called.
	FoundryExtractionOutOfScope FoundryExtractionStatus = "out_of_scope"
)

// FoundryExtractionErrorCode is why a document failed.
type FoundryExtractionErrorCode string

const (
	FoundryExtractionTooLarge    FoundryExtractionErrorCode = "too_large"
	FoundryExtractionUnsupported FoundryExtractionErrorCode = "unsupported"
	FoundryExtractionModel       FoundryExtractionErrorCode = "model"
	FoundryExtractionStore       FoundryExtractionErrorCode = "store"
)

// FoundryExtractionError is a failed document's reason.
type FoundryExtractionError struct {
	Code FoundryExtractionErrorCode
	// Retryable means a later load may succeed: the next one sends it again.
	Retryable bool
}

// FoundryExtractionCounts are what one document put in the database.
type FoundryExtractionCounts struct {
	Attributes            int
	AttributeReviews      int
	PropConflicts         int
	Created               int
	IdentifiersUnverified int
	AttributesOverEntries int
	ResolvedUser          int
	ResolvedAdjudicated   int
	ResolvedIdentifier    int
	ResolvedName          int
	ResolvedAlias         int
	ResolvedCreated       int

	Sections   int
	Entities   int
	Mentions   int
	Facts      int
	Reviews    int
	Unresolved int
	Rejected   int
}

// FoundryDocumentExtraction is what extracting one document into one load
// made of it. DuplicateOf is set exactly for duplicate, Error exactly for
// failed; the database refuses any other shape.
type FoundryDocumentExtraction struct {
	// LoadID is the load it was extracted into: the job's.
	LoadID      string
	DocumentID  string
	OntologyID  string
	WorkspaceID string
	// JobID is the job that recorded it.
	JobID        string
	Status       FoundryExtractionStatus
	DuplicateOf  *string
	Error        *FoundryExtractionError
	Counts       FoundryExtractionCounts
	USDMicros    int64
	InputTokens  int64
	OutputTokens int64
	// ArtifactKey is the saved model output the document was committed from.
	ArtifactKey *string
	FinishedAt  time.Time
}

// FoundryGraphJobKind is what a graph job does.
type FoundryGraphJobKind string

const (
	// FoundryGraphJobLoad creates a load, whose id is its own, and fills it
	// with a version's tables, then the documents. The website's Load queues it.
	FoundryGraphJobLoad FoundryGraphJobKind = "load"
	// FoundryGraphJobExtract extracts documents into a load: the current one
	// when it was queued, by an upload, a change of current load or the end
	// of a job that owed it.
	FoundryGraphJobExtract FoundryGraphJobKind = "extract"
	// FoundryGraphJobDelete deletes a load from the database.
	FoundryGraphJobDelete FoundryGraphJobKind = "delete"
	// FoundryGraphJobApply brings the current load up to its tables' newest
	// snapshots in place, writing only what changed, then extracts the
	// documents it waits for. A refresh queues it instead of a new load.
	FoundryGraphJobApply FoundryGraphJobKind = "apply"
)

// FoundryGraphJobStatus is where a graph job is in its run.
type FoundryGraphJobStatus string

const (
	FoundryGraphJobQueued          FoundryGraphJobStatus = "queued"
	FoundryGraphJobRunning         FoundryGraphJobStatus = "running"
	FoundryGraphJobSucceeded       FoundryGraphJobStatus = "succeeded"
	FoundryGraphJobBudgetExhausted FoundryGraphJobStatus = "budget_exhausted"
	FoundryGraphJobFailed          FoundryGraphJobStatus = "failed"
	// FoundryGraphJobCancelled is a job ended from outside: a delete a new
	// load preempted, or any job of a deleted ontology. A worker running it
	// loses its lease and stops.
	FoundryGraphJobCancelled FoundryGraphJobStatus = "cancelled"
)

// Live reports a job that is queued or running.
func (s FoundryGraphJobStatus) Live() bool {
	return s == FoundryGraphJobQueued || s == FoundryGraphJobRunning
}

// FoundryGraphJobPhase is the step a graph job is on. A load starts at
// provisioning, an extract at documents; a load whose tables are in resumes
// at documents. A delete is always at deleting.
type FoundryGraphJobPhase string

const (
	// FoundryGraphJobProvisioning prepares an attempt: the agent, the
	// database's proxy, the snapshots and the documents.
	FoundryGraphJobProvisioning FoundryGraphJobPhase = "provisioning"
	FoundryGraphJobIngest       FoundryGraphJobPhase = "ingest"
	FoundryGraphJobDocuments    FoundryGraphJobPhase = "documents"
	FoundryGraphJobFinishing    FoundryGraphJobPhase = "finishing"
	FoundryGraphJobDeleting     FoundryGraphJobPhase = "deleting"
)

// RunsIngest reports whether an attempt starting at this phase loads the tables.
func (p FoundryGraphJobPhase) RunsIngest() bool {
	return p == FoundryGraphJobProvisioning || p == FoundryGraphJobIngest
}

// FoundryGraphJobProgress is how far a job got.
type FoundryGraphJobProgress struct {
	ChunksDone     int
	DocumentsDone  int
	DocumentsTotal int
}

// FoundryGraphJobSpend is model spend, in millionths of a US dollar and tokens.
type FoundryGraphJobSpend struct {
	USDMicros    int64
	InputTokens  int64
	OutputTokens int64
}

// FoundryBoostHolder is the kind of work whose attempts hold boosts, as
// foundry_boost_holders.kind stores it.
type FoundryBoostHolder string

const (
	// FoundryBoostLoad is a graph job (foundry_load_jobs): a load, an
	// extract or a load's delete, which writes the database.
	FoundryBoostLoad FoundryBoostHolder = "load"
	// FoundryBoostQuestion is a question (foundry_questions), which reads it.
	FoundryBoostQuestion FoundryBoostHolder = "question"
	// FoundryBoostRevision is a revision generation
	// (foundry_ontology_generations with a revise input), which reads it to
	// measure the revision.
	FoundryBoostRevision FoundryBoostHolder = "revise"
)

// Valid reports whether h is one of the holders above.
func (h FoundryBoostHolder) Valid() bool {
	switch h {
	case FoundryBoostLoad, FoundryBoostQuestion, FoundryBoostRevision:
		return true
	}
	return false
}

// FoundryLoadBoost is one job or question attempt's leased rate limit on a
// tenant. Independent holders hold independent leases, whose active limits
// are combined, in place of the tenant's plan limit when that is lower.
type FoundryLoadBoost struct {
	TenantID string
	Holder   FoundryBoostHolder
	// JobID is the graph job's, the question's or the revision generation's id.
	JobID string
	// Attempt is the job's attempt number: a later attempt takes the boost
	// over, and an earlier one can no longer remove it.
	Attempt           int
	RequestsPerSecond uint64
	Burst             uint64
	// TTL is how long the boost outlives an attempt that never removes it.
	TTL time.Duration
}

// FoundryGraphJob is one leased unit of work on an ontology's database. An
// ontology has at most one live job, which is the database's only Foundry
// writer.
type FoundryGraphJob struct {
	ID          string
	WorkspaceID string
	OntologyID  string
	// VersionID is the version of its load, whose ontology and mappings the
	// job writes.
	VersionID string
	// LoadID is the load the job writes or deletes: a load job's own id.
	LoadID string
	Kind   FoundryGraphJobKind
	Status FoundryGraphJobStatus
	Phase  FoundryGraphJobPhase
	// ClientToken makes queueing a load idempotent; extract jobs have none.
	ClientToken       *string
	LeaseOwner        *string
	LeaseUntil        *time.Time
	Attempts          int
	NextAttemptAt     time.Time
	Error             *string
	Report            json.RawMessage
	Progress          FoundryGraphJobProgress
	Spend             FoundryGraphJobSpend
	RequestedByUserID *string
	CreatedAt         time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
	// Sources are the snapshots a load job reads, pinned on its first attempt.
	Sources []FoundryLoadSource
	// Origin and RefreshesLoadID are written to the load a load job creates;
	// a claim reads Origin back from the job's load.
	Origin          FoundryLoadOrigin
	RefreshesLoadID *string
}

// FoundryGraphJobUpdate is what a running attempt records as it goes.
type FoundryGraphJobUpdate struct {
	Phase    FoundryGraphJobPhase
	Progress FoundryGraphJobProgress
	// IngestViolations, once the attempt's Ingest is done and its report
	// counted them, is the job's load's FoundryLoad.IngestViolations. Nil
	// leaves the load's count as it is.
	IngestViolations *uint64
}

// FoundryGraphJobOutcome ends an attempt. A non-nil RetryAt requeues the job;
// otherwise Status is succeeded, budget_exhausted or failed, and a delete
// job never ends budget_exhausted.
type FoundryGraphJobOutcome struct {
	RetryAt *time.Time
	// Uncounted, only with RetryAt, means the attempt did nothing wrong: the
	// worker stopped during it (a deploy or a restart), or the database was
	// still building an index. The retry gives the attempt back, so it
	// doesn't count against max_attempts.
	Uncounted bool
	Status    FoundryGraphJobStatus
	// Error is why, written for the customer.
	Error string
	// IngestDone means the version's tables are in the load: a retry skips
	// Ingest.
	IngestDone bool
	Report     json.RawMessage
	Progress   FoundryGraphJobProgress
	// HeldSets, only with Status budget_exhausted, are the sets with an
	// allowance (FoundrySetAllowance) some of whose documents the job left
	// unrecorded when a budget, theirs, the job's or the month's, stopped
	// it. A partial refresh that held some
	// never drops what the load it refreshed extracted of them: it replaces
	// that load only once it recorded every document of theirs that load
	// recorded, those deleted since included, and is deleted otherwise
	// (FinishFoundryGraphJob). Any other job ignores them.
	HeldSets []string
	// Next is what an ended job may queue after it; a retry ignores it.
	Next FoundryGraphNext
}

// FoundryGraphNext is what the end of a job leaves behind: the ontology keeps
// RetainLoads loads, and the job may queue one more, an extract of the
// current load it owes (within Budget) or else a delete of the oldest load
// being deleted, with the id given here.
type FoundryGraphNext struct {
	ExtractID string
	DeleteID  string
	// RefreshID is the id of the full refresh load an apply that failed
	// before its tables were in queues in its place.
	RefreshID   string
	Budget      FoundryGraphBudget
	RetainLoads int
}

// FoundryGraphJobFinish is what finishing an attempt did.
type FoundryGraphJobFinish string

const (
	// FoundryGraphJobRetried requeued the job after a failed attempt.
	FoundryGraphJobRetried FoundryGraphJobFinish = "retried"
	// FoundryGraphJobMerged requeued a finished attempt because documents
	// uploaded during it still wait for its load, the current one: the next
	// attempt extracts them.
	FoundryGraphJobMerged FoundryGraphJobFinish = "merged"
	// FoundryGraphJobEnded ended the job with the outcome's status.
	FoundryGraphJobEnded FoundryGraphJobFinish = "ended"
)

// FoundryGraphBudget is a workspace's monthly cap on graph spend: work is
// queued, and spends, only while the jobs created since Since spent under
// its cap (CapFor). Work no person started stops ReserveMicros (one job's
// cap) short of CapMicros, so a backlog nobody asked for always leaves a
// person one job's worth of the month.
type FoundryGraphBudget struct {
	Since         time.Time
	CapMicros     int64
	ReserveMicros int64
}

// CapFor is the month's cap for a job requestedBy a person, or by nobody
// (nil): a refresh, or an extract no one asked for, stops ReserveMicros
// short of CapMicros.
func (b FoundryGraphBudget) CapFor(requestedBy *string) int64 {
	if requestedBy == nil {
		return b.CapMicros - b.ReserveMicros
	}
	return b.CapMicros
}

// FoundrySetAllowance is what work no person started (refreshes, and
// extracts no one asked for) may spend in a UTC month extracting the
// documents of the sets one connector kind owns, in millionths of a US
// dollar: SetMicros on each set, and WorkspaceMicros on all of a
// workspace's sets of the kind together. Both are positive
// (config.FoundryConfig.SetAllowances). It bounds that work within the
// graph budget's share for it (FoundryGraphBudget.CapFor), never beyond.
type FoundrySetAllowance struct {
	SetMicros       int64
	WorkspaceMicros int64
}

// FoundryDocumentSetSpend is what work no person started spent, in
// millionths of a US dollar, extracting a connector's set's documents in one
// UTC month, as read (provider.FoundryDocumentSetSpend): USDMicros on the
// set, and WorkspaceUSDMicros on all of its workspace's sets of
// ConnectorKind together, this one and those of deleted connectors
// included. Skipped means a sync of the set's stream left documents
// unwritten in some month while work no person started could spend no more
// on the set: the next sync with room reads in full.
type FoundryDocumentSetSpend struct {
	SetID              string
	ConnectorKind      string
	ConnectorName      string
	USDMicros          int64
	WorkspaceUSDMicros int64
	Skipped            bool
}

// Left is what work no person started may still spend this month on the
// set under a: the least of what the set's and the workspace's allowances
// have left. Zero or less means the month is spent; other sets of the kind
// spending after the read lower it further.
func (s FoundryDocumentSetSpend) Left(a FoundrySetAllowance) int64 {
	return min(a.SetMicros-s.USDMicros, a.WorkspaceMicros-s.WorkspaceUSDMicros)
}
