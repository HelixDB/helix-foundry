package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/events"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Unimplemented is a Store whose every method panics. Tests embed it and
// override only the methods they exercise, like the proto
// Unimplemented*Server pattern.
type Unimplemented struct{}

var _ Store = (*Unimplemented)(nil)

func (Unimplemented) unimplemented(name string) {
	panic(fmt.Sprintf("store: %s not implemented in this stub", name))
}

func (u Unimplemented) WithTx(context.Context, func(Store) error) error {
	u.unimplemented("WithTx")
	return nil
}

func (u Unimplemented) WorkspaceDeleting(context.Context, string) (bool, error) {
	u.unimplemented("WorkspaceDeleting")
	return false, nil
}

func (u Unimplemented) ListFoundryLoadDecisions(context.Context, string, string, []string, int) ([]*model.FoundryReviewDecision, error) {
	u.unimplemented("ListFoundryLoadDecisions")
	return nil, nil
}
func (u Unimplemented) ListFoundryReviewDecisions(context.Context, string, string, []string, int) ([]*model.FoundryReviewDecision, error) {
	u.unimplemented("ListFoundryReviewDecisions")
	return nil, nil
}
func (u Unimplemented) SettleFoundryReviewDecision(context.Context, string, *model.FoundryReviewDecision) error {
	u.unimplemented("SettleFoundryReviewDecision")
	return nil
}
func (u Unimplemented) DeleteFoundryReviewDecision(context.Context, string, string) error {
	u.unimplemented("DeleteFoundryReviewDecision")
	return nil
}
func (u Unimplemented) GetConnector(context.Context, string, string) (*model.Connector, error) {
	u.unimplemented("GetConnector")
	return nil, nil
}
func (u Unimplemented) ListConnectors(context.Context, string) ([]*model.Connector, error) {
	u.unimplemented("ListConnectors")
	return nil, nil
}
func (u Unimplemented) CreateConnector(context.Context, *model.Connector) error {
	u.unimplemented("CreateConnector")
	return nil
}
func (u Unimplemented) UpdateConnector(context.Context, string, string, model.ConnectorUpdate) error {
	u.unimplemented("UpdateConnector")
	return nil
}
func (u Unimplemented) DeleteConnector(context.Context, string, string) error {
	u.unimplemented("DeleteConnector")
	return nil
}
func (u Unimplemented) DeleteWorkspaceConnectors(context.Context, string) error {
	u.unimplemented("DeleteWorkspaceConnectors")
	return nil
}
func (u Unimplemented) GetActiveConnectorSync(context.Context, string, string) (*model.ConnectorSync, error) {
	u.unimplemented("GetActiveConnectorSync")
	return nil, nil
}
func (u Unimplemented) GetConnectorSync(context.Context, string, string) (*model.ConnectorSync, error) {
	u.unimplemented("GetConnectorSync")
	return nil, nil
}
func (u Unimplemented) ListConnectorSyncs(context.Context, string, string, int) ([]*model.ConnectorSync, error) {
	u.unimplemented("ListConnectorSyncs")
	return nil, nil
}
func (u Unimplemented) ListConnectorTables(context.Context, string, string) ([]*model.ConnectorTable, error) {
	u.unimplemented("ListConnectorTables")
	return nil, nil
}
func (u Unimplemented) EnqueueConnectorSync(context.Context, *model.ConnectorSync) error {
	u.unimplemented("EnqueueConnectorSync")
	return nil
}
func (u Unimplemented) ClaimConnectorSync(context.Context, string, time.Duration) (*model.ConnectorSync, error) {
	u.unimplemented("ClaimConnectorSync")
	return nil, nil
}
func (u Unimplemented) RenewConnectorSyncLease(context.Context, string, string, time.Duration) error {
	u.unimplemented("RenewConnectorSyncLease")
	return nil
}
func (u Unimplemented) FinishConnectorSync(context.Context, string, string, model.ConnectorSyncOutcome) error {
	u.unimplemented("FinishConnectorSync")
	return nil
}
func (u Unimplemented) RequeueConnectorSync(context.Context, string, string) error {
	u.unimplemented("RequeueConnectorSync")
	return nil
}
func (u Unimplemented) UpsertConnectorTable(context.Context, *model.ConnectorTable, *model.ConnectorChangeBatch, string) error {
	u.unimplemented("UpsertConnectorTable")
	return nil
}
func (u Unimplemented) TouchConnectorTable(context.Context, string, string, string, bool, string) error {
	u.unimplemented("TouchConnectorTable")
	return nil
}
func (u Unimplemented) QueueDueConnectorSyncs(context.Context, int, time.Duration) (int, error) {
	u.unimplemented("QueueDueConnectorSyncs")
	return 0, nil
}
func (u Unimplemented) UpdateConnectorCursor(context.Context, string, json.RawMessage, string, string) error {
	u.unimplemented("UpdateConnectorCursor")
	return nil
}
func (u Unimplemented) ListStrandedConnectorCursors(context.Context, []string, int) ([]*model.Connector, error) {
	u.unimplemented("ListStrandedConnectorCursors")
	return nil, nil
}
func (u Unimplemented) ClearConnectorCursor(context.Context, string, json.RawMessage) error {
	u.unimplemented("ClearConnectorCursor")
	return nil
}
func (u Unimplemented) DeleteReplacedSnapshot(context.Context, string) error {
	u.unimplemented("DeleteReplacedSnapshot")
	return nil
}
func (u Unimplemented) DeleteConnectorChangeBatch(context.Context, string) error {
	u.unimplemented("DeleteConnectorChangeBatch")
	return nil
}
func (u Unimplemented) ListConnectorChangeBatches(context.Context, string, string) ([]*model.ConnectorChangeBatch, error) {
	u.unimplemented("ListConnectorChangeBatches")
	return nil, nil
}
func (u Unimplemented) ListConnectorChangeBatchesAfter(context.Context, string, string, string, int64, int64) ([]*model.ConnectorChangeBatch, error) {
	u.unimplemented("ListConnectorChangeBatchesAfter")
	return nil, nil
}
func (u Unimplemented) ListExpiredChangeBatches(context.Context, time.Time, int) ([]*model.ConnectorChangeBatch, error) {
	u.unimplemented("ListExpiredChangeBatches")
	return nil, nil
}
func (u Unimplemented) ListReplacedSnapshots(context.Context, time.Time, int) ([]*model.ReplacedSnapshot, error) {
	u.unimplemented("ListReplacedSnapshots")
	return nil, nil
}
func (u Unimplemented) ListPinnedSnapshotKeys(context.Context) ([]string, error) {
	u.unimplemented("ListPinnedSnapshotKeys")
	return nil, nil
}
func (u Unimplemented) EventSendState(context.Context, string, string) (model.FoundryEventSourceState, error) {
	u.unimplemented("EventSendState")
	return model.FoundryEventSourceState{}, nil
}
func (u Unimplemented) EventBatchByToken(context.Context, string, string, string) (*model.FoundryEventBatch, error) {
	u.unimplemented("EventBatchByToken")
	return nil, nil
}
func (u Unimplemented) ListFoundryEventBatches(context.Context, string, string, int) (model.FoundryEventBatchList, error) {
	u.unimplemented("ListFoundryEventBatches")
	return model.FoundryEventBatchList{}, nil
}
func (u Unimplemented) SumFoundryEventBatches(context.Context, string, time.Time) (model.FoundryEventTotals, error) {
	u.unimplemented("SumFoundryEventBatches")
	return model.FoundryEventTotals{}, nil
}
func (u Unimplemented) EventBatchesAfter(context.Context, string, int64, events.ReadBudget) (events.Schema, []events.Batch, error) {
	u.unimplemented("EventBatchesAfter")
	return events.Schema{}, nil, nil
}
func (u Unimplemented) EventTable(context.Context, string) (events.Schema, int64, string, error) {
	u.unimplemented("EventTable")
	return events.Schema{}, 0, "", nil
}
func (u Unimplemented) EventBacklog(context.Context, string, int64, events.ReadBudget) (events.Backlog, error) {
	u.unimplemented("EventBacklog")
	return events.BacklogNone, nil
}
func (u Unimplemented) ListFoundryEventSourceTokens(context.Context, string, string, int) ([]*model.FoundryEventSourceToken, error) {
	u.unimplemented("ListFoundryEventSourceTokens")
	return nil, nil
}
func (u Unimplemented) GetLiveFoundryEventSourceToken(context.Context, string, string, string) (*model.FoundryEventSourceToken, error) {
	u.unimplemented("GetLiveFoundryEventSourceToken")
	return nil, nil
}
func (u Unimplemented) ListFoundryEventBatchPurges(context.Context, time.Time, int) ([]*model.FoundryEventBatch, error) {
	u.unimplemented("ListFoundryEventBatchPurges")
	return nil, nil
}
func (u Unimplemented) ListFoundryEventSourcePurges(context.Context, time.Time, model.FoundryEventSourcePurge, int) ([]model.FoundryEventSourcePurge, error) {
	u.unimplemented("ListFoundryEventSourcePurges")
	return nil, nil
}
func (u Unimplemented) CreateEventSource(context.Context, *model.Connector, int) error {
	u.unimplemented("CreateEventSource")
	return nil
}
func (u Unimplemented) UpdateEventSource(context.Context, string, string, model.EventSourceUpdate) error {
	u.unimplemented("UpdateEventSource")
	return nil
}
func (u Unimplemented) SendFoundryEvents(context.Context, model.FoundryEventSend) (model.FoundryEventsSent, error) {
	u.unimplemented("SendFoundryEvents")
	return model.FoundryEventsSent{}, nil
}
func (u Unimplemented) CreateFoundryEventSourceToken(context.Context, *model.FoundryEventSourceToken, int) error {
	u.unimplemented("CreateFoundryEventSourceToken")
	return nil
}
func (u Unimplemented) RevokeFoundryEventSourceToken(context.Context, string, string, string) (*model.FoundryEventSourceToken, error) {
	u.unimplemented("RevokeFoundryEventSourceToken")
	return nil, nil
}
func (u Unimplemented) ValidateFoundryEventSourceToken(context.Context, string) (*model.FoundryEventSourceToken, error) {
	u.unimplemented("ValidateFoundryEventSourceToken")
	return nil, nil
}
func (u Unimplemented) DeleteFoundryEventBatch(context.Context, string, time.Time) error {
	u.unimplemented("DeleteFoundryEventBatch")
	return nil
}
func (u Unimplemented) FinishFoundryEventSourcePurge(context.Context, model.FoundryEventSourcePurge, time.Time) error {
	u.unimplemented("FinishFoundryEventSourcePurge")
	return nil
}
func (u Unimplemented) DeleteExpiredEventSyncs(context.Context, time.Time, int) (int, error) {
	u.unimplemented("DeleteExpiredEventSyncs")
	return 0, nil
}
func (u Unimplemented) PruneConnectorTables(context.Context, string, string, []string, string, string) ([]string, error) {
	u.unimplemented("PruneConnectorTables")
	return nil, nil
}
func (u Unimplemented) ListConnectorDocuments(context.Context, string, string, string, int) ([]model.ConnectorDocument, error) {
	u.unimplemented("ListConnectorDocuments")
	return nil, nil
}
func (u Unimplemented) ListFoundryDocumentPurges(context.Context, time.Time, int) ([]*model.FoundryDocumentPurge, error) {
	u.unimplemented("ListFoundryDocumentPurges")
	return nil, nil
}
func (u Unimplemented) EnsureConnectorDocumentSet(context.Context, *model.FoundryDocumentSet, string, string) (string, error) {
	u.unimplemented("EnsureConnectorDocumentSet")
	return "", nil
}
func (u Unimplemented) ReserveConnectorDocuments(context.Context, string, []string, string, string) error {
	u.unimplemented("ReserveConnectorDocuments")
	return nil
}
func (u Unimplemented) PublishConnectorDocuments(context.Context, model.ConnectorDocumentsPublish, string, string) (
	[]model.FoundryDocumentRead, error) {
	u.unimplemented("PublishConnectorDocuments")
	return nil, nil
}
func (u Unimplemented) DeleteFoundryDocumentPurge(context.Context, string) error {
	u.unimplemented("DeleteFoundryDocumentPurge")
	return nil
}
func (u Unimplemented) FoundryDocumentSetSpend(context.Context, string, []string, time.Time) (map[string]model.FoundryDocumentSetSpend, error) {
	u.unimplemented("FoundryDocumentSetSpend")
	return nil, nil
}
func (u Unimplemented) SumFoundryDocumentSetSpend(context.Context, string, string, time.Time) (int64, error) {
	u.unimplemented("SumFoundryDocumentSetSpend")
	return 0, nil
}
func (u Unimplemented) MarkFoundryDocumentSetSkipped(context.Context, string, string, time.Time) error {
	u.unimplemented("MarkFoundryDocumentSetSkipped")
	return nil
}
func (u Unimplemented) ClearFoundryDocumentSetSkipped(context.Context, string, string, string, string) error {
	u.unimplemented("ClearFoundryDocumentSetSkipped")
	return nil
}
func (u Unimplemented) GetOntologyVersion(context.Context, string, string) (*model.OntologyVersion, error) {
	u.unimplemented("GetOntologyVersion")
	return nil, nil
}
func (u Unimplemented) GetCurrentOntologyVersion(context.Context, string) (*model.OntologyVersion, error) {
	u.unimplemented("GetCurrentOntologyVersion")
	return nil, nil
}
func (u Unimplemented) ListOntologyVersions(context.Context, string, int) ([]*model.OntologyVersion, error) {
	u.unimplemented("ListOntologyVersions")
	return nil, nil
}
func (u Unimplemented) CreateOntologyVersion(context.Context, *model.OntologyVersion) error {
	u.unimplemented("CreateOntologyVersion")
	return nil
}
func (u Unimplemented) SetCurrentOntologyVersion(context.Context, string, string, *string) error {
	u.unimplemented("SetCurrentOntologyVersion")
	return nil
}
func (u Unimplemented) GetActiveOntologyGeneration(context.Context, string) (*model.OntologyGeneration, error) {
	u.unimplemented("GetActiveOntologyGeneration")
	return nil, nil
}
func (u Unimplemented) GetOntologyGeneration(context.Context, string, string) (*model.OntologyGeneration, error) {
	u.unimplemented("GetOntologyGeneration")
	return nil, nil
}
func (u Unimplemented) ListOntologyGenerations(context.Context, string, int) ([]*model.OntologyGeneration, error) {
	u.unimplemented("ListOntologyGenerations")
	return nil, nil
}
func (u Unimplemented) EnqueueOntologyGeneration(context.Context, *model.OntologyGeneration, int) error {
	u.unimplemented("EnqueueOntologyGeneration")
	return nil
}
func (u Unimplemented) SetOntologyGenerationTitle(context.Context, string, string) error {
	u.unimplemented("SetOntologyGenerationTitle")
	return nil
}
func (u Unimplemented) ClaimOntologyGeneration(context.Context, string, time.Duration) (*model.OntologyGeneration, error) {
	u.unimplemented("ClaimOntologyGeneration")
	return nil, nil
}
func (u Unimplemented) RenewOntologyGenerationLease(context.Context, string, string, time.Duration) error {
	u.unimplemented("RenewOntologyGenerationLease")
	return nil
}
func (u Unimplemented) FinishOntologyGeneration(context.Context, string, string, model.OntologyGenerationOutcome) error {
	u.unimplemented("FinishOntologyGeneration")
	return nil
}
func (u Unimplemented) GetFoundryOntology(context.Context, string, string) (*model.FoundryOntology, error) {
	u.unimplemented("GetFoundryOntology")
	return nil, nil
}
func (u Unimplemented) GetFoundryOntologyByClientToken(context.Context, string, string) (*model.FoundryOntology, error) {
	u.unimplemented("GetFoundryOntologyByClientToken")
	return nil, nil
}
func (u Unimplemented) GetFoundryOntologyByTenant(context.Context, string) (*model.FoundryOntology, error) {
	u.unimplemented("GetFoundryOntologyByTenant")
	return nil, nil
}
func (u Unimplemented) ListFoundryOntologies(context.Context, string, string, int) ([]*model.FoundryOntology, string, error) {
	u.unimplemented("ListFoundryOntologies")
	return nil, "", nil
}
func (u Unimplemented) CreateFoundryOntology(context.Context, *model.FoundryOntology) error {
	u.unimplemented("CreateFoundryOntology")
	return nil
}
func (u Unimplemented) DeleteFoundryOntology(context.Context, string, string, string) error {
	u.unimplemented("DeleteFoundryOntology")
	return nil
}
func (u Unimplemented) ResumeFoundryOntologyDrain(context.Context, string, string, string) error {
	u.unimplemented("ResumeFoundryOntologyDrain")
	return nil
}
func (u Unimplemented) GetFoundryDocumentSet(context.Context, string, string) (*model.FoundryDocumentSet, error) {
	u.unimplemented("GetFoundryDocumentSet")
	return nil, nil
}
func (u Unimplemented) ListFoundryDocumentSets(context.Context, string) ([]*model.FoundryDocumentSet, error) {
	u.unimplemented("ListFoundryDocumentSets")
	return nil, nil
}
func (u Unimplemented) GetFoundryDocument(context.Context, string, string) (*model.FoundryDocument, error) {
	u.unimplemented("GetFoundryDocument")
	return nil, nil
}
func (u Unimplemented) CountFoundryDocuments(context.Context, string, time.Time) (int, error) {
	u.unimplemented("CountFoundryDocuments")
	return 0, nil
}
func (u Unimplemented) CountFoundryDocumentsInSets(context.Context, string, []string) (int, error) {
	u.unimplemented("CountFoundryDocumentsInSets")
	return 0, nil
}
func (u Unimplemented) GetFoundryDataLayout(context.Context, string) (*model.FoundryDataLayout, error) {
	u.unimplemented("GetFoundryDataLayout")
	return nil, nil
}
func (u Unimplemented) GetOntologyVersionScope(context.Context, string, string) ([]string, error) {
	u.unimplemented("GetOntologyVersionScope")
	return nil, nil
}
func (u Unimplemented) GetOntologyVersionMappings(context.Context, string, string) (string, error) {
	u.unimplemented("GetOntologyVersionMappings")
	return "", nil
}
func (u Unimplemented) ListFoundryOntologyDocuments(context.Context, string, string, []string, model.FoundryDocumentFilter, string, int) ([]*model.FoundryDocument, string, error) {
	u.unimplemented("ListFoundryOntologyDocuments")
	return nil, "", nil
}
func (u Unimplemented) FoundryLoadHoldsDocument(context.Context, string, string) (bool, error) {
	u.unimplemented("FoundryLoadHoldsDocument")
	return false, nil
}
func (u Unimplemented) ListFoundryDocuments(context.Context, string, string, string, int) ([]*model.FoundryDocument, string, error) {
	u.unimplemented("ListFoundryDocuments")
	return nil, "", nil
}
func (u Unimplemented) ListPendingFoundryDocuments(context.Context, string) ([]*model.FoundryDocument, error) {
	u.unimplemented("ListPendingFoundryDocuments")
	return nil, nil
}
func (u Unimplemented) ListRemovedFoundryDocuments(context.Context, string) ([]string, error) {
	u.unimplemented("ListRemovedFoundryDocuments")
	return nil, nil
}
func (u Unimplemented) RecordFoundryDocumentRemoval(context.Context, string, string, string) error {
	u.unimplemented("RecordFoundryDocumentRemoval")
	return nil
}
func (u Unimplemented) ListUploadedFoundryDocuments(context.Context, string, string, int) ([]*model.FoundryDocument, error) {
	u.unimplemented("ListUploadedFoundryDocuments")
	return nil, nil
}
func (u Unimplemented) GetFoundryDocumentExtraction(context.Context, string, string) (*model.FoundryDocumentExtraction, error) {
	u.unimplemented("GetFoundryDocumentExtraction")
	return nil, nil
}
func (u Unimplemented) ListFoundryDocumentExtractions(context.Context, string, []string) (map[string]*model.FoundryDocumentExtraction, error) {
	u.unimplemented("ListFoundryDocumentExtractions")
	return nil, nil
}
func (u Unimplemented) GetFoundryLoad(context.Context, string, string) (*model.FoundryLoad, error) {
	u.unimplemented("GetFoundryLoad")
	return nil, nil
}
func (u Unimplemented) ListFoundryLoads(context.Context, string) ([]*model.FoundryLoad, error) {
	u.unimplemented("ListFoundryLoads")
	return nil, nil
}
func (u Unimplemented) ListFoundryLoadJobs(context.Context, []string, int) (map[string]model.FoundryLoadJobs, error) {
	u.unimplemented("ListFoundryLoadJobs")
	return nil, nil
}
func (u Unimplemented) GetActiveFoundryGraphJob(context.Context, string) (*model.FoundryGraphJob, error) {
	u.unimplemented("GetActiveFoundryGraphJob")
	return nil, nil
}
func (u Unimplemented) GetLatestFoundryGraphJob(context.Context, string, model.FoundryGraphJobKind) (*model.FoundryGraphJob, error) {
	u.unimplemented("GetLatestFoundryGraphJob")
	return nil, nil
}
func (u Unimplemented) GetFoundryGraphJob(context.Context, string, string) (*model.FoundryGraphJob, error) {
	u.unimplemented("GetFoundryGraphJob")
	return nil, nil
}
func (u Unimplemented) GetFoundryGraphJobByClientToken(context.Context, string, string) (*model.FoundryGraphJob, error) {
	u.unimplemented("GetFoundryGraphJobByClientToken")
	return nil, nil
}
func (u Unimplemented) SumFoundryGraphJobSpend(context.Context, string, time.Time) (int64, error) {
	u.unimplemented("SumFoundryGraphJobSpend")
	return 0, nil
}
func (u Unimplemented) CreateFoundryDocumentSet(context.Context, *model.FoundryDocumentSet, int) error {
	u.unimplemented("CreateFoundryDocumentSet")
	return nil
}
func (u Unimplemented) UpdateFoundryDocumentSet(context.Context, string, string, model.FoundryDocumentSetUpdate) error {
	u.unimplemented("UpdateFoundryDocumentSet")
	return nil
}
func (u Unimplemented) DeleteFoundryDocumentSet(context.Context, string, string) error {
	u.unimplemented("DeleteFoundryDocumentSet")
	return nil
}
func (u Unimplemented) MoveFoundryDocument(context.Context, string, string, string, model.FoundryDocumentFanOut) (*model.FoundryDocument, []model.FoundryDocumentRead, error) {
	u.unimplemented("MoveFoundryDocument")
	return nil, nil, nil
}
func (u Unimplemented) CreateFoundryDocument(context.Context, *model.FoundryDocument, model.FoundryDocumentCap) (*model.FoundryDocument, bool, error) {
	u.unimplemented("CreateFoundryDocument")
	return nil, false, nil
}
func (u Unimplemented) RenameFoundryDocument(context.Context, string, string, string) error {
	u.unimplemented("RenameFoundryDocument")
	return nil
}
func (u Unimplemented) CompleteFoundryDocumentUpload(context.Context, string, string, model.FoundryDocumentFanOut) (*model.FoundryDocument, []model.FoundryDocumentRead, error) {
	u.unimplemented("CompleteFoundryDocumentUpload")
	return nil, nil, nil
}
func (u Unimplemented) PutFoundryDataLayout(context.Context, *model.FoundryDataLayout, int64) error {
	u.unimplemented("PutFoundryDataLayout")
	return nil
}
func (u Unimplemented) DeleteFoundryDocument(context.Context, string, string) error {
	u.unimplemented("DeleteFoundryDocument")
	return nil
}
func (u Unimplemented) ClaimFoundryFileRead(context.Context, []model.FoundryMediaType, string, time.Duration, model.FoundryFileReadWake) (*model.FoundryFileReadClaim, error) {
	u.unimplemented("ClaimFoundryFileRead")
	return nil, nil
}
func (u Unimplemented) RenewFoundryFileReadLease(context.Context, string, model.FoundryFileReadMode, string, time.Duration) error {
	u.unimplemented("RenewFoundryFileReadLease")
	return nil
}
func (u Unimplemented) FinishFoundryFileRead(context.Context, string, model.FoundryFileReadMode, string, model.FoundryFileReadOutcome, model.FoundryFileReadWake) error {
	u.unimplemented("FinishFoundryFileRead")
	return nil
}
func (u Unimplemented) ListReadFoundryTableDocuments(context.Context, string, []string) ([]*model.FoundryDocument, error) {
	u.unimplemented("ListReadFoundryTableDocuments")
	return nil, nil
}
func (u Unimplemented) CountReadableFoundryTextDocuments(context.Context, string, []string) (int, error) {
	u.unimplemented("CountReadableFoundryTextDocuments")
	return 0, nil
}
func (u Unimplemented) ListFoundryDocumentsByID(context.Context, string, []string) (map[string]*model.FoundryDocument, error) {
	u.unimplemented("ListFoundryDocumentsByID")
	return nil, nil
}
func (u Unimplemented) CreateFoundryLoad(context.Context, *model.FoundryGraphJob) error {
	u.unimplemented("CreateFoundryLoad")
	return nil
}
func (u Unimplemented) SetCurrentFoundryLoad(context.Context, string, string, string, *model.FoundryGraphJob, model.FoundryGraphBudget) (bool, error) {
	u.unimplemented("SetCurrentFoundryLoad")
	return false, nil
}
func (u Unimplemented) DeleteFoundryLoad(context.Context, string, string, string, string) error {
	u.unimplemented("DeleteFoundryLoad")
	return nil
}
func (u Unimplemented) ClaimFoundryGraphJob(context.Context, string, time.Duration) (*model.FoundryGraphJob, error) {
	u.unimplemented("ClaimFoundryGraphJob")
	return nil, nil
}
func (u Unimplemented) RenewFoundryGraphJobLease(context.Context, string, string, time.Duration) error {
	u.unimplemented("RenewFoundryGraphJobLease")
	return nil
}
func (u Unimplemented) UpdateFoundryGraphJob(context.Context, string, string, model.FoundryGraphJobUpdate) error {
	u.unimplemented("UpdateFoundryGraphJob")
	return nil
}
func (u Unimplemented) RecordFoundryDocumentExtraction(context.Context, string, string, *model.FoundryDocumentExtraction, bool) error {
	u.unimplemented("RecordFoundryDocumentExtraction")
	return nil
}
func (u Unimplemented) FinishFoundryGraphJob(context.Context, string, string, model.FoundryGraphJobOutcome) (model.FoundryGraphJobFinish, error) {
	u.unimplemented("FinishFoundryGraphJob")
	return "", nil
}
func (u Unimplemented) GetFoundryQuestion(context.Context, string, string) (*model.FoundryQuestion, error) {
	u.unimplemented("GetFoundryQuestion")
	return nil, nil
}
func (u Unimplemented) GetFoundryQuestionByClientToken(context.Context, string, string) (*model.FoundryQuestion, error) {
	u.unimplemented("GetFoundryQuestionByClientToken")
	return nil, nil
}
func (u Unimplemented) ListFoundryQuestions(context.Context, string, string, string, int) ([]*model.FoundryQuestion, string, error) {
	u.unimplemented("ListFoundryQuestions")
	return nil, "", nil
}
func (u Unimplemented) ListFoundryQuestionHistory(context.Context, *model.FoundryQuestion, int) ([]*model.FoundryQuestion, error) {
	u.unimplemented("ListFoundryQuestionHistory")
	return nil, nil
}
func (u Unimplemented) ListFoundryQuestionEvents(context.Context, string, int, int) ([]model.FoundryQuestionEvent, error) {
	u.unimplemented("ListFoundryQuestionEvents")
	return nil, nil
}
func (u Unimplemented) SumFoundryQuestionSpend(context.Context, string, time.Time) (model.FoundryQuestionSpend, error) {
	u.unimplemented("SumFoundryQuestionSpend")
	return model.FoundryQuestionSpend{}, nil
}
func (u Unimplemented) CreateFoundryQuestion(context.Context, *model.FoundryQuestion, model.FoundryQuestionLimits) error {
	u.unimplemented("CreateFoundryQuestion")
	return nil
}
func (u Unimplemented) ClaimFoundryQuestion(context.Context, string, model.FoundryQuestionClaim) (*model.FoundryQuestion, error) {
	u.unimplemented("ClaimFoundryQuestion")
	return nil, nil
}
func (u Unimplemented) RenewFoundryQuestionLease(context.Context, string, string, time.Duration) error {
	u.unimplemented("RenewFoundryQuestionLease")
	return nil
}
func (u Unimplemented) AppendFoundryQuestionEvents(context.Context, string, string, []json.RawMessage) error {
	u.unimplemented("AppendFoundryQuestionEvents")
	return nil
}
func (u Unimplemented) SettleFoundryQuestion(context.Context, string, string, model.FoundryQuestionOutcome) error {
	u.unimplemented("SettleFoundryQuestion")
	return nil
}
func (u Unimplemented) CancelFoundryQuestion(context.Context, string, string) (*model.FoundryQuestion, error) {
	u.unimplemented("CancelFoundryQuestion")
	return nil, nil
}
func (u Unimplemented) ListDueFoundryRefreshes(context.Context, time.Time, string, int) ([]*model.FoundryOntology, error) {
	u.unimplemented("ListDueFoundryRefreshes")
	return nil, nil
}
func (u Unimplemented) QueueFoundryRefresh(context.Context, string, string, *model.FoundryGraphJob) (bool, error) {
	u.unimplemented("QueueFoundryRefresh")
	return false, nil
}
func (u Unimplemented) OweFoundryRefreshes(context.Context, string, string, []string, time.Duration) error {
	u.unimplemented("OweFoundryRefreshes")
	return nil
}
func (u Unimplemented) SetFoundryOntologyAutoRefresh(context.Context, string, string, bool) error {
	u.unimplemented("SetFoundryOntologyAutoRefresh")
	return nil
}
func (u Unimplemented) SetFoundryGraphJobSources(context.Context, string, string, []model.FoundryLoadSource) error {
	u.unimplemented("SetFoundryGraphJobSources")
	return nil
}
func (u Unimplemented) GetFoundryEnrichmentConsent(context.Context, string) (*model.FoundryEnrichmentConsent, error) {
	u.unimplemented("GetFoundryEnrichmentConsent")
	return nil, nil
}
func (u Unimplemented) GetFoundryLoadEnrichment(context.Context, string) (*model.FoundryLoadEnrichment, error) {
	u.unimplemented("GetFoundryLoadEnrichment")
	return nil, nil
}
func (u Unimplemented) ListFoundryLoadEnrichments(context.Context, []string) (map[string]*model.FoundryLoadEnrichment, error) {
	u.unimplemented("ListFoundryLoadEnrichments")
	return nil, nil
}
func (u Unimplemented) ListFoundryLoadSources(context.Context, []string) (map[string][]model.FoundryLoadSource, error) {
	u.unimplemented("ListFoundryLoadSources")
	return nil, nil
}
func (u Unimplemented) GetLatestFoundryEnrichmentResults(context.Context, string) (*model.FoundryLoadEnrichment, error) {
	u.unimplemented("GetLatestFoundryEnrichmentResults")
	return nil, nil
}
func (u Unimplemented) GetLatestFoundryEnrichmentApplied(context.Context, string) (*model.FoundryLoadEnrichment, error) {
	u.unimplemented("GetLatestFoundryEnrichmentApplied")
	return nil, nil
}
func (u Unimplemented) GetFoundryEnrichmentLedger(context.Context, string, time.Time) (model.FoundryEnrichmentLedger, error) {
	u.unimplemented("GetFoundryEnrichmentLedger")
	return model.FoundryEnrichmentLedger{}, nil
}
func (u Unimplemented) GetFoundryEnrichmentProvider(context.Context, string) (*model.FoundryEnrichmentProvider, error) {
	u.unimplemented("GetFoundryEnrichmentProvider")
	return nil, nil
}
func (u Unimplemented) CheckFoundryLoadEnrichment(context.Context, string, string) (model.FoundryEnrichmentChecks, error) {
	u.unimplemented("CheckFoundryLoadEnrichment")
	return model.FoundryEnrichmentChecks{}, nil
}
func (u Unimplemented) ListFoundryEnrichmentSweeps(context.Context, time.Time, time.Time, time.Time, int) ([]model.FoundryEnrichmentSweep, error) {
	u.unimplemented("ListFoundryEnrichmentSweeps")
	return nil, nil
}
func (u Unimplemented) ListFoundryEnrichmentPurges(context.Context, int, int) ([]*model.FoundryEnrichmentPurge, error) {
	u.unimplemented("ListFoundryEnrichmentPurges")
	return nil, nil
}
func (u Unimplemented) ParkFoundryGraphJob(context.Context, string, string, time.Time) (bool, error) {
	u.unimplemented("ParkFoundryGraphJob")
	return false, nil
}
func (u Unimplemented) SetFoundryEnrichmentConsent(context.Context, *model.FoundryEnrichmentConsent) error {
	u.unimplemented("SetFoundryEnrichmentConsent")
	return nil
}
func (u Unimplemented) StartFoundryLoadEnrichment(context.Context, string, *model.FoundryGraphJob) (*model.FoundryLoadEnrichment, error) {
	u.unimplemented("StartFoundryLoadEnrichment")
	return nil, nil
}
func (u Unimplemented) QueueFoundryLoadEnrichment(context.Context, string, string, model.FoundryEnrichmentQueue) (*model.FoundryLoadEnrichment, error) {
	u.unimplemented("QueueFoundryLoadEnrichment")
	return nil, nil
}
func (u Unimplemented) EndPreparedFoundryLoadEnrichment(context.Context, string, string, model.FoundryEnrichmentPrepared) error {
	u.unimplemented("EndPreparedFoundryLoadEnrichment")
	return nil
}
func (u Unimplemented) ApplyFoundryLoadEnrichment(context.Context, string, string, model.FoundryEnrichmentApplied) (model.FoundryEnrichmentApplied, error) {
	u.unimplemented("ApplyFoundryLoadEnrichment")
	return model.FoundryEnrichmentApplied{}, nil
}
func (u Unimplemented) EndBusyFoundryLoadEnrichment(context.Context, string, string) error {
	u.unimplemented("EndBusyFoundryLoadEnrichment")
	return nil
}
func (u Unimplemented) GetFoundryEnrichmentSettings(context.Context, string, time.Time) (model.FoundryEnrichmentSettings, error) {
	u.unimplemented("GetFoundryEnrichmentSettings")
	return model.FoundryEnrichmentSettings{}, nil
}
func (u Unimplemented) RecordFoundryLoadEnrichmentIngest(context.Context, string, string, int64, int64) error {
	u.unimplemented("RecordFoundryLoadEnrichmentIngest")
	return nil
}
func (u Unimplemented) CancelWorkspaceFoundryWork(context.Context, string) error {
	u.unimplemented("CancelWorkspaceFoundryWork")
	return nil
}
func (u Unimplemented) ClaimFoundryLoadEnrichment(context.Context, string, string, time.Duration, time.Duration, time.Duration, int) (*model.FoundryLoadEnrichment, bool, error) {
	u.unimplemented("ClaimFoundryLoadEnrichment")
	return nil, false, nil
}
func (u Unimplemented) RenewFoundryLoadEnrichmentLease(context.Context, string, string, string, time.Duration) (model.FoundryEnrichmentSignals, error) {
	u.unimplemented("RenewFoundryLoadEnrichmentLease")
	return model.FoundryEnrichmentSignals{}, nil
}
func (u Unimplemented) ReleaseFoundryLoadEnrichment(context.Context, string, string) error {
	u.unimplemented("ReleaseFoundryLoadEnrichment")
	return nil
}
func (u Unimplemented) ReserveFoundryEnrichmentCredits(context.Context, string, string, string, int64, model.FoundryEnrichmentCaps) (int64, error) {
	u.unimplemented("ReserveFoundryEnrichmentCredits")
	return 0, nil
}
func (u Unimplemented) BeginFoundryEnrichmentBatch(context.Context, string, string, string, int, int, model.FoundryEnrichmentCaps) (model.FoundryEnrichmentBegun, error) {
	u.unimplemented("BeginFoundryEnrichmentBatch")
	return model.FoundryEnrichmentBegun{}, nil
}
func (u Unimplemented) ChargeFoundryEnrichmentBatch(context.Context, string, string, model.FoundryEnrichmentCharge, model.FoundryEnrichmentCounts) error {
	u.unimplemented("ChargeFoundryEnrichmentBatch")
	return nil
}
func (u Unimplemented) EndFoundryLoadEnrichment(context.Context, string, string, model.FoundryEnrichmentEnd, *model.FoundryEnrichmentPublished, model.FoundryEnrichmentCounts) error {
	u.unimplemented("EndFoundryLoadEnrichment")
	return nil
}
func (u Unimplemented) StopFoundryLoadEnrichment(context.Context, string, string, string, string) error {
	u.unimplemented("StopFoundryLoadEnrichment")
	return nil
}
func (u Unimplemented) HaltFoundryEnrichmentProvider(context.Context, string, time.Time, string) error {
	u.unimplemented("HaltFoundryEnrichmentProvider")
	return nil
}
func (u Unimplemented) ClearFoundryEnrichmentFiles(context.Context, model.FoundryEnrichmentSweep) error {
	u.unimplemented("ClearFoundryEnrichmentFiles")
	return nil
}
func (u Unimplemented) AdvanceFoundryEnrichmentPurge(context.Context, string, *model.FoundryGraphJob, string, time.Duration, int) (model.FoundryEnrichmentPurgeStep, error) {
	u.unimplemented("AdvanceFoundryEnrichmentPurge")
	return "", nil
}
func (u Unimplemented) GetFoundryAIView(context.Context, string) (model.FoundryAIView, error) {
	u.unimplemented("GetFoundryAIView")
	return model.FoundryAIView{}, nil
}
func (u Unimplemented) GetActiveFoundryAIConfig(context.Context, string) (*model.FoundryAIConfig, error) {
	u.unimplemented("GetActiveFoundryAIConfig")
	return nil, nil
}
func (u Unimplemented) CreateFoundryAIConfig(context.Context, *model.FoundryAIConfig, int) error {
	u.unimplemented("CreateFoundryAIConfig")
	return nil
}
func (u Unimplemented) ClaimFoundryAIProbe(context.Context, string, time.Duration, int) (*model.FoundryAIConfig, error) {
	u.unimplemented("ClaimFoundryAIProbe")
	return nil, nil
}
func (u Unimplemented) ReleaseFoundryAIProbe(context.Context, string, string, int) error {
	u.unimplemented("ReleaseFoundryAIProbe")
	return nil
}
func (u Unimplemented) ReturnFoundryAIProbe(context.Context, string, string) error {
	u.unimplemented("ReturnFoundryAIProbe")
	return nil
}
func (u Unimplemented) FinishFoundryAIProbe(context.Context, string, string, model.FoundryAIProbeOutcome) (model.FoundryAIProbeState, error) {
	u.unimplemented("FinishFoundryAIProbe")
	return "", nil
}
func (u Unimplemented) ActivateFoundryAIDefault(context.Context, string, string) error {
	u.unimplemented("ActivateFoundryAIDefault")
	return nil
}
func (u Unimplemented) DiscoverFoundryOntologyReady(context.Context, time.Time, int, func() string) (int, error) {
	u.unimplemented("DiscoverFoundryOntologyReady")
	return 0, nil
}
func (u Unimplemented) DiscoverFoundrySampleFollowUps(context.Context, time.Time, time.Time, int, func() string) (int, error) {
	u.unimplemented("DiscoverFoundrySampleFollowUps")
	return 0, nil
}
func (u Unimplemented) ClaimFoundryNotification(context.Context, string, time.Duration) (*model.FoundryNotification, error) {
	u.unimplemented("ClaimFoundryNotification")
	return nil, nil
}
func (u Unimplemented) FinishFoundryNotification(context.Context, string, string, model.FoundryNotificationOutcome) error {
	u.unimplemented("FinishFoundryNotification")
	return nil
}
