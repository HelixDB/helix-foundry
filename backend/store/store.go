// Package store is Foundry's MySQL store: its connectors and syncs,
// documents and document sets, ontologies, loads and graph jobs, questions,
// People enrichment, event sources and model providers.
//
// Its host owns the workspaces Foundry's rows belong to. The store reads
// and locks them through Workspaces, inside its own statements and
// transactions, and the host runs the store in a transaction of its own
// (InTx) for a write Foundry's must commit with, such as a workspace's
// deletion (CancelWorkspaceFoundryWork).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/events"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// ErrNotFound means the row is absent, or a guarded write's guard did not hold.
var ErrNotFound = errors.New("store: not found")

// ErrLimitExceeded means a write would pass a cap, such as a workspace's
// number of event sources.
var ErrLimitExceeded = errors.New("store: limit exceeded")

// ErrConflict means a uniqueness constraint rejected the write. For an
// idempotency key that is the expected answer, not a failure.
var ErrConflict = errors.New("store: conflict")

// ErrInvalidPageToken is a page token no list handed out: the caller's
// mistake, unlike every other error a list returns.
var ErrInvalidPageToken = errors.New("store: invalid page token")

// Executor is the query surface shared by *sql.DB and *sql.Tx, so the same
// statements run directly or inside a transaction.
type Executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Workspaces is the host's table of workspaces, which the store locks and
// joins but does not own. Lock and Deleting run on the executor they are
// given, so inside a transaction they are part of it.
type Workspaces interface {
	// Table names the table in the store's statements. They read three of
	// its columns: id, deleted_at (set once a workspace is deleted) and
	// status ('deleting' while it is being deleted, 'deleted' after).
	Table() string
	// Lock locks a live workspace's row until q's transaction ends:
	// ErrNotFound when it is deleted or absent.
	Lock(ctx context.Context, q Executor, id string) error
	// Deleting reports whether a workspace is deleted or being deleted:
	// ErrNotFound when it is absent.
	Deleting(ctx context.Context, q Executor, id string) (bool, error)
}

// ReadStore is the read-only surface.
type ReadStore interface {
	// Workspaces, as the host keeps them
	WorkspaceDeleting(ctx context.Context, id string) (bool, error)

	// Foundry connectors
	GetConnector(ctx context.Context, workspaceID, id string) (*model.Connector, error)
	ListConnectors(ctx context.Context, workspaceID string) ([]*model.Connector, error)
	GetActiveConnectorSync(ctx context.Context, workspaceID, connectorID string) (*model.ConnectorSync, error)
	GetConnectorSync(ctx context.Context, workspaceID, id string) (*model.ConnectorSync, error)
	ListConnectorSyncs(ctx context.Context, workspaceID, connectorID string, limit int) ([]*model.ConnectorSync, error)
	ListConnectorTables(ctx context.Context, workspaceID, connectorID string) ([]*model.ConnectorTable, error)
	ListConnectorChangeBatches(ctx context.Context, workspaceID, syncID string) ([]*model.ConnectorChangeBatch, error)
	ListConnectorChangeBatchesAfter(ctx context.Context, workspaceID, connectorID, table string, afterSeq, toSeq int64) ([]*model.ConnectorChangeBatch, error)
	ListExpiredChangeBatches(ctx context.Context, before time.Time, limit int) ([]*model.ConnectorChangeBatch, error)
	ListReplacedSnapshots(ctx context.Context, before time.Time, limit int) ([]*model.ReplacedSnapshot, error)
	ListPinnedSnapshotKeys(ctx context.Context) ([]string, error)
	ListConnectorDocuments(ctx context.Context, workspaceID, setID, after string, limit int) ([]model.ConnectorDocument, error)
	ListFoundryDocumentPurges(ctx context.Context, before time.Time, limit int) ([]*model.FoundryDocumentPurge, error)
	FoundryDocumentSetSpend(ctx context.Context, workspaceID string, setIDs []string, month time.Time) (map[string]model.FoundryDocumentSetSpend, error)
	SumFoundryDocumentSetSpend(ctx context.Context, workspaceID, connectorKind string, month time.Time) (int64, error)

	// Foundry event sources (connector kind events): their state, batches,
	// tokens and purge (foundry_event_sources.go)
	EventSendState(ctx context.Context, workspaceID, connectorID string) (model.FoundryEventSourceState, error)
	EventBatchByToken(ctx context.Context, workspaceID, connectorID, clientToken string) (*model.FoundryEventBatch, error)
	ListFoundryEventBatches(ctx context.Context, workspaceID, connectorID string, limit int) (model.FoundryEventBatchList, error)
	SumFoundryEventBatches(ctx context.Context, workspaceID string, since time.Time) (model.FoundryEventTotals, error)
	EventBatchesAfter(ctx context.Context, connectorID string, after int64, budget events.ReadBudget) (events.Schema, []events.Batch, error)
	EventTable(ctx context.Context, connectorID string) (events.Schema, int64, string, error)
	EventBacklog(ctx context.Context, connectorID string, after int64, budget events.ReadBudget) (events.Backlog, error)
	ListFoundryEventSourceTokens(ctx context.Context, workspaceID, connectorID string, limit int) ([]*model.FoundryEventSourceToken, error)
	GetLiveFoundryEventSourceToken(ctx context.Context, workspaceID, connectorID, id string) (*model.FoundryEventSourceToken, error)
	ListFoundryEventBatchPurges(ctx context.Context, deletedBefore time.Time, limit int) ([]*model.FoundryEventBatch, error)
	ListFoundryEventSourcePurges(ctx context.Context, deletedBefore time.Time, after model.FoundryEventSourcePurge, limit int) ([]model.FoundryEventSourcePurge, error)

	// Foundry ontologies, each bound to one tenant database
	GetFoundryOntology(ctx context.Context, workspaceID, id string) (*model.FoundryOntology, error)
	GetFoundryOntologyByClientToken(ctx context.Context, workspaceID, clientToken string) (*model.FoundryOntology, error)
	GetFoundryOntologyByTenant(ctx context.Context, tenantID string) (*model.FoundryOntology, error)
	ListFoundryOntologies(ctx context.Context, workspaceID, pageToken string, limit int) ([]*model.FoundryOntology, string, error)
	GetOntologyVersion(ctx context.Context, ontologyID, id string) (*model.OntologyVersion, error)
	GetCurrentOntologyVersion(ctx context.Context, ontologyID string) (*model.OntologyVersion, error)
	ListOntologyVersions(ctx context.Context, ontologyID string, limit int) ([]*model.OntologyVersion, error)
	GetActiveOntologyGeneration(ctx context.Context, ontologyID string) (*model.OntologyGeneration, error)
	ListDueFoundryRefreshes(ctx context.Context, afterDue time.Time, afterID string, limit int) ([]*model.FoundryOntology, error)
	GetOntologyGeneration(ctx context.Context, ontologyID, id string) (*model.OntologyGeneration, error)
	ListOntologyGenerations(ctx context.Context, ontologyID string, limit int) ([]*model.OntologyGeneration, error)

	ListFoundryLoadDecisions(ctx context.Context, loadID, afterID string, documentIDs []string, limit int) ([]*model.FoundryReviewDecision, error)
	ListFoundryReviewDecisions(ctx context.Context, ontologyID, afterID string, documentIDs []string, limit int) ([]*model.FoundryReviewDecision, error)

	// Foundry document sets, documents and the Data layout, per workspace
	GetFoundryDocumentSet(ctx context.Context, workspaceID, id string) (*model.FoundryDocumentSet, error)
	ListFoundryDocumentSets(ctx context.Context, workspaceID string) ([]*model.FoundryDocumentSet, error)
	GetFoundryDocument(ctx context.Context, workspaceID, id string) (*model.FoundryDocument, error)
	CountFoundryDocuments(ctx context.Context, workspaceID string, pendingSince time.Time) (int, error)
	CountFoundryDocumentsInSets(ctx context.Context, workspaceID string, setIDs []string) (int, error)
	ListFoundryDocuments(ctx context.Context, workspaceID, setID, pageToken string, limit int) ([]*model.FoundryDocument, string, error)
	ListUploadedFoundryDocuments(ctx context.Context, workspaceID, setID string, limit int) ([]*model.FoundryDocument, error)
	ListReadFoundryTableDocuments(ctx context.Context, workspaceID string, setIDs []string) ([]*model.FoundryDocument, error)
	CountReadableFoundryTextDocuments(ctx context.Context, workspaceID string, setIDs []string) (int, error)
	ListFoundryDocumentsByID(ctx context.Context, workspaceID string, ids []string) (map[string]*model.FoundryDocument, error)
	GetFoundryDataLayout(ctx context.Context, workspaceID string) (*model.FoundryDataLayout, error)

	// Foundry loads and graph jobs, per ontology
	GetOntologyVersionScope(ctx context.Context, ontologyID, versionID string) ([]string, error)
	GetOntologyVersionMappings(ctx context.Context, ontologyID, versionID string) (string, error)
	ListFoundryOntologyDocuments(ctx context.Context, workspaceID, currentLoadID string, scope []string, filter model.FoundryDocumentFilter,
		pageToken string, limit int) ([]*model.FoundryDocument, string, error)
	FoundryLoadHoldsDocument(ctx context.Context, loadID, documentID string) (bool, error)
	ListPendingFoundryDocuments(ctx context.Context, loadID string) ([]*model.FoundryDocument, error)
	GetFoundryDocumentExtraction(ctx context.Context, loadID, documentID string) (*model.FoundryDocumentExtraction, error)
	ListFoundryDocumentExtractions(ctx context.Context, loadID string, documentIDs []string) (map[string]*model.FoundryDocumentExtraction, error)
	GetFoundryLoad(ctx context.Context, ontologyID, id string) (*model.FoundryLoad, error)
	ListFoundryLoads(ctx context.Context, ontologyID string) ([]*model.FoundryLoad, error)
	ListFoundryLoadJobs(ctx context.Context, loadIDs []string, newest int) (map[string]model.FoundryLoadJobs, error)
	GetActiveFoundryGraphJob(ctx context.Context, ontologyID string) (*model.FoundryGraphJob, error)
	GetLatestFoundryGraphJob(ctx context.Context, ontologyID string, kind model.FoundryGraphJobKind) (*model.FoundryGraphJob, error)
	GetFoundryGraphJob(ctx context.Context, ontologyID, id string) (*model.FoundryGraphJob, error)
	GetFoundryGraphJobByClientToken(ctx context.Context, ontologyID, clientToken string) (*model.FoundryGraphJob, error)
	SumFoundryGraphJobSpend(ctx context.Context, workspaceID string, since time.Time) (int64, error)
	GetFoundryQuestion(ctx context.Context, ontologyID, id string) (*model.FoundryQuestion, error)
	GetFoundryQuestionByClientToken(ctx context.Context, ontologyID, clientToken string) (*model.FoundryQuestion, error)
	ListFoundryQuestions(ctx context.Context, ontologyID, threadID, pageToken string, limit int) ([]*model.FoundryQuestion, string, error)
	ListFoundryQuestionHistory(ctx context.Context, q *model.FoundryQuestion, limit int) ([]*model.FoundryQuestion, error)
	ListFoundryQuestionEvents(ctx context.Context, questionID string, afterSeq, limit int) ([]model.FoundryQuestionEvent, error)
	SumFoundryQuestionSpend(ctx context.Context, workspaceID string, since time.Time) (model.FoundryQuestionSpend, error)

	// Foundry People enrichment: consent, each load's step, the ledger and the sweep
	GetFoundryEnrichmentConsent(ctx context.Context, workspaceID string) (*model.FoundryEnrichmentConsent, error)
	GetFoundryEnrichmentSettings(ctx context.Context, workspaceID string, since time.Time) (model.FoundryEnrichmentSettings, error)
	GetFoundryLoadEnrichment(ctx context.Context, loadID string) (*model.FoundryLoadEnrichment, error)
	ListFoundryLoadEnrichments(ctx context.Context, loadIDs []string) (map[string]*model.FoundryLoadEnrichment, error)
	ListFoundryLoadSources(ctx context.Context, loadIDs []string) (map[string][]model.FoundryLoadSource, error)
	GetLatestFoundryEnrichmentResults(ctx context.Context, ontologyID string) (*model.FoundryLoadEnrichment, error)
	GetLatestFoundryEnrichmentApplied(ctx context.Context, ontologyID string) (*model.FoundryLoadEnrichment, error)
	GetFoundryEnrichmentLedger(ctx context.Context, workspaceID string, since time.Time) (model.FoundryEnrichmentLedger, error)
	GetFoundryEnrichmentProvider(ctx context.Context, provider string) (*model.FoundryEnrichmentProvider, error)
	CheckFoundryLoadEnrichment(ctx context.Context, loadID, provider string) (model.FoundryEnrichmentChecks, error)
	ListFoundryEnrichmentSweeps(ctx context.Context, callsBefore, supersededBefore, wholeBefore time.Time, limit int) ([]model.FoundryEnrichmentSweep, error)
	ListFoundryEnrichmentPurges(ctx context.Context, maxAttempts, limit int) ([]*model.FoundryEnrichmentPurge, error)

	// Foundry model providers: the workspace's setting and its configs
	GetFoundryAIView(ctx context.Context, workspaceID string) (model.FoundryAIView, error)
	GetActiveFoundryAIConfig(ctx context.Context, workspaceID string) (*model.FoundryAIConfig, error)
}

// Store is ReadStore plus every write.
type Store interface {
	ReadStore

	// WithTx runs fn in a single transaction; the Store passed to fn is tx-scoped.
	WithTx(ctx context.Context, fn func(Store) error) error

	CreateConnector(ctx context.Context, c *model.Connector) error
	UpdateConnector(ctx context.Context, workspaceID, id string, update model.ConnectorUpdate) error
	DeleteConnector(ctx context.Context, workspaceID, id string) error
	DeleteWorkspaceConnectors(ctx context.Context, workspaceID string) error
	EnqueueConnectorSync(ctx context.Context, sync *model.ConnectorSync) error
	ClaimConnectorSync(ctx context.Context, workerID string, lease time.Duration) (*model.ConnectorSync, error)
	RenewConnectorSyncLease(ctx context.Context, id, workerID string, lease time.Duration) error
	FinishConnectorSync(ctx context.Context, id, workerID string, outcome model.ConnectorSyncOutcome) error
	RequeueConnectorSync(ctx context.Context, id, workerID string) error
	UpsertConnectorTable(ctx context.Context, t *model.ConnectorTable, batch *model.ConnectorChangeBatch, workerID string) error
	TouchConnectorTable(ctx context.Context, connectorID, table, syncID string, reconciled bool, workerID string) error
	PruneConnectorTables(ctx context.Context, workspaceID, connectorID string, keep []string, syncID, workerID string) ([]string, error)
	QueueDueConnectorSyncs(ctx context.Context, limit int, eventsAfter time.Duration) (int, error)
	UpdateConnectorCursor(ctx context.Context, connectorID string, cursor json.RawMessage, syncID, workerID string) error
	ListStrandedConnectorCursors(ctx context.Context, kinds []string, limit int) ([]*model.Connector, error)
	ClearConnectorCursor(ctx context.Context, connectorID string, cursor json.RawMessage) error
	DeleteReplacedSnapshot(ctx context.Context, id string) error
	DeleteConnectorChangeBatch(ctx context.Context, id string) error
	EnsureConnectorDocumentSet(ctx context.Context, set *model.FoundryDocumentSet, syncID, workerID string) (string, error)
	ReserveConnectorDocuments(ctx context.Context, workspaceID string, ids []string, syncID, workerID string) error
	PublishConnectorDocuments(ctx context.Context, p model.ConnectorDocumentsPublish, syncID, workerID string) ([]model.FoundryDocumentRead, error)
	DeleteFoundryDocumentPurge(ctx context.Context, documentID string) error
	MarkFoundryDocumentSetSkipped(ctx context.Context, workspaceID, setID string, month time.Time) error
	ClearFoundryDocumentSetSkipped(ctx context.Context, workspaceID, setID, syncID, workerID string) error
	CreateEventSource(ctx context.Context, c *model.Connector, max int) error
	UpdateEventSource(ctx context.Context, workspaceID, id string, update model.EventSourceUpdate) error
	SendFoundryEvents(ctx context.Context, send model.FoundryEventSend) (model.FoundryEventsSent, error)
	CreateFoundryEventSourceToken(ctx context.Context, t *model.FoundryEventSourceToken, maxLive int) error
	RevokeFoundryEventSourceToken(ctx context.Context, workspaceID, connectorID, id string) (*model.FoundryEventSourceToken, error)
	ValidateFoundryEventSourceToken(ctx context.Context, hash string) (*model.FoundryEventSourceToken, error)
	DeleteFoundryEventBatch(ctx context.Context, id string, deletedBefore time.Time) error
	FinishFoundryEventSourcePurge(ctx context.Context, p model.FoundryEventSourcePurge, deletedBefore time.Time) error
	DeleteExpiredEventSyncs(ctx context.Context, finishedBefore time.Time, limit int) (int, error)
	CreateFoundryOntology(ctx context.Context, o *model.FoundryOntology) error
	DeleteFoundryOntology(ctx context.Context, workspaceID, id, deleteJobID string) error
	ResumeFoundryOntologyDrain(ctx context.Context, workspaceID, id, deleteJobID string) error
	CreateOntologyVersion(ctx context.Context, v *model.OntologyVersion) error
	SetCurrentOntologyVersion(ctx context.Context, ontologyID, versionID string, expected *string) error
	EnqueueOntologyGeneration(ctx context.Context, gen *model.OntologyGeneration, perHour int) error
	SetOntologyGenerationTitle(ctx context.Context, id, title string) error
	ClaimOntologyGeneration(ctx context.Context, workerID string, lease time.Duration) (*model.OntologyGeneration, error)
	RenewOntologyGenerationLease(ctx context.Context, id, workerID string, lease time.Duration) error
	FinishOntologyGeneration(ctx context.Context, id, workerID string, outcome model.OntologyGenerationOutcome) error
	SettleFoundryReviewDecision(ctx context.Context, workspaceID string, decision *model.FoundryReviewDecision) error
	DeleteFoundryReviewDecision(ctx context.Context, ontologyID, id string) error
	CreateFoundryDocumentSet(ctx context.Context, set *model.FoundryDocumentSet, max int) error
	UpdateFoundryDocumentSet(ctx context.Context, workspaceID, id string, update model.FoundryDocumentSetUpdate) error
	DeleteFoundryDocumentSet(ctx context.Context, workspaceID, id string) error
	CreateFoundryDocument(ctx context.Context, d *model.FoundryDocument, limit model.FoundryDocumentCap) (*model.FoundryDocument, bool, error)
	RenameFoundryDocument(ctx context.Context, workspaceID, id, name string) error
	MoveFoundryDocument(ctx context.Context, workspaceID, id, setID string, fan model.FoundryDocumentFanOut) (*model.FoundryDocument, []model.FoundryDocumentRead, error)
	CompleteFoundryDocumentUpload(ctx context.Context, workspaceID, id string, fan model.FoundryDocumentFanOut) (*model.FoundryDocument, []model.FoundryDocumentRead, error)
	DeleteFoundryDocument(ctx context.Context, workspaceID, id string) error
	ClaimFoundryFileRead(ctx context.Context, mediaTypes []model.FoundryMediaType, token string, lease time.Duration, wake model.FoundryFileReadWake) (*model.FoundryFileReadClaim, error)
	RenewFoundryFileReadLease(ctx context.Context, documentID string, mode model.FoundryFileReadMode, token string, lease time.Duration) error
	FinishFoundryFileRead(ctx context.Context, documentID string, mode model.FoundryFileReadMode, token string, outcome model.FoundryFileReadOutcome, wake model.FoundryFileReadWake) error
	PutFoundryDataLayout(ctx context.Context, l *model.FoundryDataLayout, expectedRevision int64) error
	CreateFoundryLoad(ctx context.Context, job *model.FoundryGraphJob) error
	QueueFoundryRefresh(ctx context.Context, workspaceID, ontologyID string, job *model.FoundryGraphJob) (bool, error)
	OweFoundryRefreshes(ctx context.Context, workspaceID, connectorID string, tables []string, refreshEvery time.Duration) error
	SetFoundryOntologyAutoRefresh(ctx context.Context, workspaceID, id string, on bool) error
	SetFoundryGraphJobSources(ctx context.Context, id, workerID string, sources []model.FoundryLoadSource) error
	SetCurrentFoundryLoad(ctx context.Context, workspaceID, ontologyID, loadID string, extract *model.FoundryGraphJob, budget model.FoundryGraphBudget) (bool, error)
	DeleteFoundryLoad(ctx context.Context, workspaceID, ontologyID, loadID, deleteJobID string) error
	ClaimFoundryGraphJob(ctx context.Context, workerID string, lease time.Duration) (*model.FoundryGraphJob, error)
	RenewFoundryGraphJobLease(ctx context.Context, id, workerID string, lease time.Duration) error
	UpdateFoundryGraphJob(ctx context.Context, id, workerID string, update model.FoundryGraphJobUpdate) error
	RecordFoundryDocumentExtraction(ctx context.Context, jobID, workerID string, e *model.FoundryDocumentExtraction, keepExisting bool) error
	FinishFoundryGraphJob(ctx context.Context, jobID, workerID string, outcome model.FoundryGraphJobOutcome) (model.FoundryGraphJobFinish, error)
	CreateFoundryQuestion(ctx context.Context, q *model.FoundryQuestion, limits model.FoundryQuestionLimits) error
	ClaimFoundryQuestion(ctx context.Context, workerID string, claim model.FoundryQuestionClaim) (*model.FoundryQuestion, error)
	RenewFoundryQuestionLease(ctx context.Context, id, workerID string, lease time.Duration) error
	AppendFoundryQuestionEvents(ctx context.Context, id, workerID string, events []json.RawMessage) error
	SettleFoundryQuestion(ctx context.Context, id, workerID string, outcome model.FoundryQuestionOutcome) error
	CancelFoundryQuestion(ctx context.Context, ontologyID, id string) (*model.FoundryQuestion, error)
	ParkFoundryGraphJob(ctx context.Context, jobID, workerID string, until time.Time) (bool, error)
	SetFoundryEnrichmentConsent(ctx context.Context, c *model.FoundryEnrichmentConsent) error
	StartFoundryLoadEnrichment(ctx context.Context, jobWorker string, job *model.FoundryGraphJob) (*model.FoundryLoadEnrichment, error)
	QueueFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string, q model.FoundryEnrichmentQueue) (*model.FoundryLoadEnrichment, error)
	EndPreparedFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string, p model.FoundryEnrichmentPrepared) error
	ApplyFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string, applied model.FoundryEnrichmentApplied) (model.FoundryEnrichmentApplied, error)
	EndBusyFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string) error
	RecordFoundryLoadEnrichmentIngest(ctx context.Context, loadID, jobWorker string, applied, unmatched int64) error
	CancelWorkspaceFoundryWork(ctx context.Context, workspaceID string) error
	ClaimFoundryLoadEnrichment(ctx context.Context, workerID, provider string, lease, loadWait, grace time.Duration, maxRunning int) (step *model.FoundryLoadEnrichment, first bool, err error)
	RenewFoundryLoadEnrichmentLease(ctx context.Context, loadID, workerID, provider string, lease time.Duration) (model.FoundryEnrichmentSignals, error)
	ReleaseFoundryLoadEnrichment(ctx context.Context, loadID, workerID string) error
	ReserveFoundryEnrichmentCredits(ctx context.Context, loadID, workerID, provider string, want int64, caps model.FoundryEnrichmentCaps) (int64, error)
	BeginFoundryEnrichmentBatch(ctx context.Context, loadID, workerID, provider string, batch, want int, caps model.FoundryEnrichmentCaps) (model.FoundryEnrichmentBegun, error)
	ChargeFoundryEnrichmentBatch(ctx context.Context, loadID, workerID string, charge model.FoundryEnrichmentCharge, counts model.FoundryEnrichmentCounts) error
	EndFoundryLoadEnrichment(ctx context.Context, loadID, workerID string, end model.FoundryEnrichmentEnd, published *model.FoundryEnrichmentPublished, counts model.FoundryEnrichmentCounts) error
	StopFoundryLoadEnrichment(ctx context.Context, workspaceID, ontologyID, loadID, userID string) error
	HaltFoundryEnrichmentProvider(ctx context.Context, provider string, until time.Time, code string) error
	ClearFoundryEnrichmentFiles(ctx context.Context, sweep model.FoundryEnrichmentSweep) error
	AdvanceFoundryEnrichmentPurge(ctx context.Context, ontologyID string, load *model.FoundryGraphJob, deleteJobID string, retryAfter time.Duration, maxAttempts int) (model.FoundryEnrichmentPurgeStep, error)
	CreateFoundryAIConfig(ctx context.Context, c *model.FoundryAIConfig, perHour int) error
	ClaimFoundryAIProbe(ctx context.Context, owner string, lease time.Duration, maxAttempts int) (*model.FoundryAIConfig, error)
	ReleaseFoundryAIProbe(ctx context.Context, configID, owner string, maxAttempts int) error
	ReturnFoundryAIProbe(ctx context.Context, configID, owner string) error
	FinishFoundryAIProbe(ctx context.Context, configID, owner string, outcome model.FoundryAIProbeOutcome) (model.FoundryAIProbeState, error)
	ActivateFoundryAIDefault(ctx context.Context, workspaceID, userID string) error
}

// mysqlDB is the MySQL Store. db is the pool, or the transaction inside
// WithTx; root opens transactions and is nil on a transaction's copy.
type mysqlDB struct {
	db         Executor
	root       *sql.DB
	workspaces Workspaces
}

// NewMySQL is the store on db, whose host's workspaces are workspaces.
func NewMySQL(db *sql.DB, workspaces Workspaces) Store {
	return &mysqlDB{db: db, root: db, workspaces: workspaces}
}

// InTx is the store inside tx, a transaction its host began on the same
// database, for a write of the host that Foundry's must commit with. Its
// WithTx is refused, as a nested transaction.
func InTx(tx Executor, workspaces Workspaces) Store {
	return &mysqlDB{db: tx, workspaces: workspaces}
}

var _ Store = (*mysqlDB)(nil)

// WithTx runs fn against a tx-scoped Store, committing on success and rolling
// back on error or panic. Nested transactions are not supported.
func (s *mysqlDB) WithTx(ctx context.Context, fn func(Store) error) (err error) {
	if s.root == nil {
		return fmt.Errorf("store: nested WithTx is not supported")
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(&mysqlDB{db: tx, workspaces: s.workspaces}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// WorkspaceDeleting reports whether a workspace is deleted or being deleted:
// ErrNotFound when it is absent.
func (s *mysqlDB) WorkspaceDeleting(ctx context.Context, id string) (bool, error) {
	return s.workspaces.Deleting(ctx, s.db, id)
}
