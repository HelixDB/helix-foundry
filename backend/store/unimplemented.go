package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/agentquestion"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/events"
	"github.com/helixdb/helix-foundry/backend/reviewcards"
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

func (u Unimplemented) ListFoundryLoadAliasesNamed(context.Context, string, []int64) ([]*model.FoundryReviewDecision, error) {
	u.unimplemented("ListFoundryLoadAliasesNamed")
	return nil, nil
}

func (u Unimplemented) FillFoundryLoadNameKeys(context.Context, string) (int, error) {
	u.unimplemented("FillFoundryLoadNameKeys")
	return 0, nil
}

func (u Unimplemented) FoundryLoadHasAliases(context.Context, string) (bool, error) {
	u.unimplemented("FoundryLoadHasAliases")
	return false, nil
}
func (u Unimplemented) ListFoundryShownDecisions(context.Context, string, string, int) ([]*model.FoundryReviewDecision, error) {
	u.unimplemented("ListFoundryShownDecisions")
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
func (u Unimplemented) SettleFoundryReleases(context.Context, string, string, []*model.FoundryReviewDecision, *model.FoundryGraphJob) (bool, error) {
	u.unimplemented("SettleFoundryReleases")
	return false, nil
}
func (u Unimplemented) SettleFoundryResolve(context.Context, string, string, *model.FoundryReviewDecision, []string, *model.FoundryGraphJob) (bool, error) {
	u.unimplemented("SettleFoundryResolve")
	return false, nil
}
func (u Unimplemented) SettleFoundryBoundResolve(context.Context, string, string, *model.FoundryReviewDecision, *model.FoundryGraphJob) (bool, error) {
	u.unimplemented("SettleFoundryBoundResolve")
	return false, nil
}

func (u Unimplemented) SettleFoundryValue(context.Context, string, string, *model.FoundryReviewDecision, *model.FoundryGraphJob) (bool, error) {
	u.unimplemented("SettleFoundryValue")
	return false, nil
}
func (u Unimplemented) SettleFoundryReject(context.Context, string, string, *model.FoundryReviewDecision) error {
	u.unimplemented("SettleFoundryReject")
	return nil
}
func (u Unimplemented) ListFoundryDecisionsApplied(context.Context, string, []string) (map[string]model.FoundryDecisionApplied, error) {
	u.unimplemented("ListFoundryDecisionsApplied")
	return nil, nil
}
func (u Unimplemented) ListFoundryLoadResends(context.Context, string) ([]*model.FoundryLoadResend, error) {
	u.unimplemented("ListFoundryLoadResends")
	return nil, nil
}
func (u Unimplemented) ListFoundryCarriedAliases(context.Context, string, int) ([]*model.FoundryReviewDecision, error) {
	u.unimplemented("ListFoundryCarriedAliases")
	return nil, nil
}
func (u Unimplemented) OweFoundryAskingResends(context.Context, *model.FoundryGraphJob, string, []string, []string) (bool, error) {
	u.unimplemented("OweFoundryAskingResends")
	return false, nil
}
func (u Unimplemented) FoundryLoadDecisionsOwed(context.Context, string) (*model.FoundryLoadResend, error) {
	u.unimplemented("FoundryLoadDecisionsOwed")
	return nil, nil
}
func (u Unimplemented) EndFoundryLoadResend(context.Context, string, string, model.FoundryResendEnd) error {
	u.unimplemented("EndFoundryLoadResend")
	return nil
}
func (u Unimplemented) RecordFoundryReleaseOutcomes(context.Context, string, string, string, []model.FoundryReleaseOutcome, []model.FoundryDocumentName) error {
	u.unimplemented("RecordFoundryReleaseOutcomes")
	return nil
}
func (u Unimplemented) DeleteFoundryReviewDecision(context.Context, string, string, string, *model.FoundryGraphJob) error {
	u.unimplemented("DeleteFoundryReviewDecision")
	return nil
}
func (u Unimplemented) GetFoundryReviewDecision(context.Context, string, string) (*model.FoundryReviewDecision, error) {
	u.unimplemented("GetFoundryReviewDecision")
	return nil, nil
}
func (u Unimplemented) GetFoundryReviewLabel(context.Context, string, string) (*model.FoundryReviewLabel, error) {
	u.unimplemented("GetFoundryReviewLabel")
	return nil, nil
}
func (u Unimplemented) ListFoundryReviewLabels(context.Context, string, string, int) ([]*model.FoundryReviewLabel, error) {
	u.unimplemented("ListFoundryReviewLabels")
	return nil, nil
}
func (u Unimplemented) FoundryReviewLabelsOf(context.Context, string, string, model.FoundryClaimSpan, *string, *string) ([]*model.FoundryReviewLabel, error) {
	u.unimplemented("FoundryReviewLabelsOf")
	return nil, nil
}
func (u Unimplemented) SaveFoundryReviewLabel(context.Context, string, *model.FoundryReviewLabel) error {
	u.unimplemented("SaveFoundryReviewLabel")
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
func (u Unimplemented) ListConnectorAccounts(context.Context, string) ([]model.ConnectorAccountRef, error) {
	u.unimplemented("ListConnectorAccounts")
	return nil, nil
}
func (u Unimplemented) BreakConnectorAccounts(context.Context, string, string, string) error {
	u.unimplemented("BreakConnectorAccounts")
	return nil
}
func (u Unimplemented) BreakConnectorAccount(context.Context, string, string, int, string) (bool, error) {
	u.unimplemented("BreakConnectorAccount")
	return false, nil
}
func (u Unimplemented) ListConnectorAccountChecks(context.Context, time.Time, int) ([]*model.Connector, error) {
	u.unimplemented("ListConnectorAccountChecks")
	return nil, nil
}
func (u Unimplemented) MarkConnectorAccountChecked(context.Context, string, string) error {
	u.unimplemented("MarkConnectorAccountChecked")
	return nil
}
func (u Unimplemented) DeferConnectorAccountCheck(context.Context, string, string, time.Time) error {
	u.unimplemented("DeferConnectorAccountCheck")
	return nil
}
func (u Unimplemented) ListConnectorAccountPurges(context.Context, int) ([]*model.ConnectorAccountPurge, error) {
	u.unimplemented("ListConnectorAccountPurges")
	return nil, nil
}
func (u Unimplemented) ConnectorAccountPurgeBacklog(context.Context) (int, *time.Time, error) {
	u.unimplemented("ConnectorAccountPurgeBacklog")
	return 0, nil, nil
}
func (u Unimplemented) DeleteConnectorAccountPurge(context.Context, string) error {
	u.unimplemented("DeleteConnectorAccountPurge")
	return nil
}
func (u Unimplemented) RetryConnectorAccountPurge(context.Context, string, string, time.Duration) error {
	u.unimplemented("RetryConnectorAccountPurge")
	return nil
}
func (u Unimplemented) GetConnectorSignIn(context.Context, string, string) (*model.ConnectorSignIn, error) {
	u.unimplemented("GetConnectorSignIn")
	return nil, nil
}
func (u Unimplemented) StartConnectorSignIn(context.Context, model.ConnectorSignInStart) (*model.ConnectorSignIn, error) {
	u.unimplemented("StartConnectorSignIn")
	return nil, nil
}
func (u Unimplemented) SetConnectorSignInVerifier(context.Context, string, string, []byte, string) error {
	u.unimplemented("SetConnectorSignInVerifier")
	return nil
}
func (u Unimplemented) AuthorizeConnectorSignIn(context.Context, string, string, time.Duration, time.Duration) error {
	u.unimplemented("AuthorizeConnectorSignIn")
	return nil
}
func (u Unimplemented) ClaimConnectorSignIn(context.Context, string, string) error {
	u.unimplemented("ClaimConnectorSignIn")
	return nil
}
func (u Unimplemented) SetConnectorSignInAccount(context.Context, string, string, string) error {
	u.unimplemented("SetConnectorSignInAccount")
	return nil
}
func (u Unimplemented) FailConnectorSignIn(context.Context, string, string, model.ConnectorSignInEnd) error {
	u.unimplemented("FailConnectorSignIn")
	return nil
}
func (u Unimplemented) ConnectConnectorSignIn(context.Context, model.ConnectorSignInConnect) error {
	u.unimplemented("ConnectConnectorSignIn")
	return nil
}
func (u Unimplemented) DeleteEndedConnectorSignIns(context.Context, time.Duration, int) (int, error) {
	u.unimplemented("DeleteEndedConnectorSignIns")
	return 0, nil
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
func (u Unimplemented) UpdateConnectorCursor(context.Context, string, int, json.RawMessage, string, string) error {
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
func (u Unimplemented) AnswerOntologyGeneration(context.Context, string, string, uint64, []agentquestion.Answer) error {
	u.unimplemented("AnswerOntologyGeneration")
	return nil
}
func (u Unimplemented) CancelOntologyGeneration(context.Context, string, string) error {
	u.unimplemented("CancelOntologyGeneration")
	return nil
}
func (u Unimplemented) ListOntologyGenerationsAskedBefore(context.Context, time.Time, int) ([]*model.OntologyGeneration, error) {
	u.unimplemented("ListOntologyGenerationsAskedBefore")
	return nil, nil
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
func (u Unimplemented) GetOntologyVersionSources(context.Context, string, string) (*model.OntologyVersionSources, error) {
	u.unimplemented("GetOntologyVersionSources")
	return nil, nil
}
func (u Unimplemented) GetOntologyVersionMappings(context.Context, string, string) (string, error) {
	u.unimplemented("GetOntologyVersionMappings")
	return "", nil
}
func (u Unimplemented) ListFoundryOntologyDocuments(context.Context, string, string, []string, *[]string, model.FoundryDocumentFilter, string, int) ([]*model.FoundryDocument, string, error) {
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
func (u Unimplemented) ListFoundryLoadUnread(context.Context, []string) (map[string]model.FoundryLoadUnread, error) {
	u.unimplemented("ListFoundryLoadUnread")
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
func (u Unimplemented) SetCurrentFoundryLoad(context.Context, string, string, string, *model.FoundryGraphJob, *model.FoundryGraphJob,
	model.FoundryGraphBudget) (bool, error) {
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
func (u Unimplemented) ListFoundryQuestions(context.Context, string, string, string, int, *access.Principal) ([]*model.FoundryQuestion, string, error) {
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
func (u Unimplemented) SumFoundryGranteeQuestionSpend(context.Context, string, access.Principal, time.Time) (model.FoundryQuestionSpend, error) {
	u.unimplemented("SumFoundryGranteeQuestionSpend")
	return model.FoundryQuestionSpend{}, nil
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
func (u Unimplemented) QueueFoundryRebuild(context.Context, string, string, time.Time, *model.FoundryGraphJob) (FoundryRebuild, error) {
	u.unimplemented("QueueFoundryRebuild")
	return 0, nil
}
func (u Unimplemented) ListFoundryRebuilds(context.Context, time.Time, string, int) ([]*model.FoundryOntology, error) {
	u.unimplemented("ListFoundryRebuilds")
	return nil, nil
}
func (u Unimplemented) ListFoundryRereads(context.Context, []string, string, int) ([]*model.FoundryOntology, error) {
	u.unimplemented("ListFoundryRereads")
	return nil, nil
}
func (u Unimplemented) QueueFoundryReread(context.Context, string, string, []string, *model.FoundryGraphJob, model.FoundryGraphBudget,
	time.Time) (bool, error) {
	u.unimplemented("QueueFoundryReread")
	return false, nil
}
func (u Unimplemented) OweFoundryRefreshes(context.Context, string, string, string, []string, time.Duration) error {
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
func (u Unimplemented) ListFoundryLoadDocumentSets(context.Context, string) ([]model.FoundryLoadDocumentSet, error) {
	u.unimplemented("ListFoundryLoadDocumentSets")
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
func (u Unimplemented) DiscoverFoundryOntologyQuestions(context.Context, time.Time, int, func() string) (int, error) {
	u.unimplemented("DiscoverFoundryOntologyQuestions")
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

func (u Unimplemented) RequestFoundryReminder(context.Context, model.FoundryReminderRequest) (model.FoundryReminderAnswer, error) {
	u.unimplemented("RequestFoundryReminder")
	return model.FoundryReminderAnswer{}, nil
}
func (u Unimplemented) StopFoundryReminder(context.Context, string, string) (bool, error) {
	u.unimplemented("StopFoundryReminder")
	return false, nil
}
func (u Unimplemented) ClaimFoundryReminder(context.Context, string, time.Duration) (*model.FoundryReminder, error) {
	u.unimplemented("ClaimFoundryReminder")
	return nil, nil
}
func (u Unimplemented) FinishFoundryReminder(context.Context, string, string, model.FoundryReminderOutcome) error {
	u.unimplemented("FinishFoundryReminder")
	return nil
}

func (u Unimplemented) ListFoundryAccessGrants(context.Context, string, []access.Principal) ([]model.FoundryAccessGrant, error) {
	u.unimplemented("ListFoundryAccessGrants")
	return nil, nil
}

func (u Unimplemented) FoundryLoadHoldings(context.Context, string, string) (access.Holdings, error) {
	u.unimplemented("FoundryLoadHoldings")
	return access.Holdings{}, nil
}

func (u Unimplemented) SetFoundryQuestionSeen(context.Context, string, *access.Inputs, []access.ItemRef) error {
	u.unimplemented("SetFoundryQuestionSeen")
	return nil
}

func (u Unimplemented) ListFoundryLoadInputs(context.Context, string, []string) (map[string]model.FoundryLoadInputs, error) {
	u.unimplemented("ListFoundryLoadInputs")
	return nil, nil
}

func (u Unimplemented) ListFoundryCurrentLoadInputs(context.Context, string, []string) (map[string]model.FoundryLoadInputs, error) {
	u.unimplemented("ListFoundryCurrentLoadInputs")
	return nil, nil
}

func (u Unimplemented) SetFoundryAccessGrants(context.Context, string, access.Principal, []access.Target, *string) error {
	u.unimplemented("SetFoundryAccessGrants")
	return nil
}

func (u Unimplemented) DeleteFoundryAccessPrincipal(context.Context, string, access.Principal) error {
	u.unimplemented("DeleteFoundryAccessPrincipal")
	return nil
}

func (u Unimplemented) ListFoundryAccessWorkflowsOff(context.Context, string, []access.Principal) ([]access.Principal, error) {
	u.unimplemented("ListFoundryAccessWorkflowsOff")
	return nil, nil
}

func (u Unimplemented) SetFoundryAccessWorkflows(context.Context, string, access.Principal, bool, *string) error {
	u.unimplemented("SetFoundryAccessWorkflows")
	return nil
}

func (u Unimplemented) FoundryVersionInputs(context.Context, string, string, string) (access.Inputs, error) {
	u.unimplemented("FoundryVersionInputs")
	return access.Inputs{}, nil
}

func (u Unimplemented) ListFoundryOntologiesHoldingDocument(context.Context, string, string) ([]string, error) {
	u.unimplemented("ListFoundryOntologiesHoldingDocument")
	return nil, nil
}

func (u Unimplemented) IsFoundryMemberRestricted(context.Context, string, string) (bool, error) {
	u.unimplemented("IsFoundryMemberRestricted")
	return false, nil
}

func (u Unimplemented) ListFoundryRestrictedMembers(context.Context, string) ([]string, error) {
	u.unimplemented("ListFoundryRestrictedMembers")
	return nil, nil
}

func (u Unimplemented) ListFoundryTeams(context.Context, string) ([]model.FoundryTeam, error) {
	u.unimplemented("ListFoundryTeams")
	return nil, nil
}

func (u Unimplemented) FoundryTeamsOf(context.Context, string, string) ([]string, error) {
	u.unimplemented("FoundryTeamsOf")
	return nil, nil
}

func (u Unimplemented) SetFoundryMemberRestricted(context.Context, string, string, bool, *string) error {
	u.unimplemented("SetFoundryMemberRestricted")
	return nil
}

func (u Unimplemented) CreateFoundryTeam(context.Context, *model.FoundryTeam) error {
	u.unimplemented("CreateFoundryTeam")
	return nil
}

func (u Unimplemented) UpdateFoundryTeam(context.Context, string, string, string, []string) error {
	u.unimplemented("UpdateFoundryTeam")
	return nil
}

func (u Unimplemented) DeleteFoundryTeam(context.Context, string, string) error {
	u.unimplemented("DeleteFoundryTeam")
	return nil
}

func (u Unimplemented) RemoveFoundryAccessMember(context.Context, string, string) error {
	u.unimplemented("RemoveFoundryAccessMember")
	return nil
}
func (u Unimplemented) SyncFoundryDirectoryTeams(context.Context, string, string, string, []model.FoundryDirectoryTeam) (model.FoundryDirectorySync, error) {
	u.unimplemented("SyncFoundryDirectoryTeams")
	return model.FoundryDirectorySync{}, nil
}
func (u Unimplemented) ReleaseFoundryDirectoryTeams(context.Context, string, string) error {
	u.unimplemented("ReleaseFoundryDirectoryTeams")
	return nil
}
func (u Unimplemented) LeaveOutFoundryTeamMembers(context.Context, string, string, []string) error {
	u.unimplemented("LeaveOutFoundryTeamMembers")
	return nil
}
func (u Unimplemented) ListFoundryHealFindings(context.Context, string) ([]*model.FoundryHealFinding, error) {
	u.unimplemented("ListFoundryHealFindings")
	return nil, nil
}
func (u Unimplemented) GetFoundryHealFinding(context.Context, string, string) (*model.FoundryHealFinding, error) {
	u.unimplemented("GetFoundryHealFinding")
	return nil, nil
}
func (u Unimplemented) GetLatestFoundryHealRun(context.Context, string) (*model.FoundryHealRun, error) {
	u.unimplemented("GetLatestFoundryHealRun")
	return nil, nil
}
func (u Unimplemented) GetLatestCompleteFoundryHealRun(context.Context, string) (*model.FoundryHealRun, error) {
	u.unimplemented("GetLatestCompleteFoundryHealRun")
	return nil, nil
}
func (u Unimplemented) FoundryHealLoadChanged(context.Context, string, string, string) (bool, error) {
	u.unimplemented("FoundryHealLoadChanged")
	return false, nil
}
func (u Unimplemented) ListFoundryHealDocuments(context.Context, string, string, int) ([]string, error) {
	u.unimplemented("ListFoundryHealDocuments")
	return nil, nil
}
func (u Unimplemented) DismissFoundryHealFinding(context.Context, string, string, string) error {
	u.unimplemented("DismissFoundryHealFinding")
	return nil
}
func (u Unimplemented) DueFoundryHealOntologies(context.Context, time.Duration, time.Duration, int) ([]model.FoundryHealDue, error) {
	u.unimplemented("DueFoundryHealOntologies")
	return nil, nil
}
func (u Unimplemented) StartFoundryHealRun(context.Context, *model.FoundryHealRun, time.Duration) error {
	u.unimplemented("StartFoundryHealRun")
	return nil
}
func (u Unimplemented) RenewFoundryHealRun(context.Context, string, time.Duration) error {
	u.unimplemented("RenewFoundryHealRun")
	return nil
}
func (u Unimplemented) FinishFoundryHealRun(context.Context, string, model.FoundryHealRunStatus, int, int) error {
	u.unimplemented("FinishFoundryHealRun")
	return nil
}
func (u Unimplemented) PublishFoundryHealRun(context.Context, model.FoundryHealPublish) error {
	u.unimplemented("PublishFoundryHealRun")
	return nil
}
func (u Unimplemented) GetFoundryWorkflow(context.Context, string, string) (*model.FoundryWorkflow, error) {
	u.unimplemented("GetFoundryWorkflow")
	return nil, nil
}
func (u Unimplemented) GetFoundryWorkflowByClientToken(context.Context, string, string) (*model.FoundryWorkflow, error) {
	u.unimplemented("GetFoundryWorkflowByClientToken")
	return nil, nil
}
func (u Unimplemented) ListFoundryWorkflows(context.Context, string, bool, string, int) ([]*model.FoundryWorkflow, string, error) {
	u.unimplemented("ListFoundryWorkflows")
	return nil, "", nil
}
func (u Unimplemented) GetFoundryWorkflowRun(context.Context, string, string) (*model.FoundryWorkflowRun, error) {
	u.unimplemented("GetFoundryWorkflowRun")
	return nil, nil
}
func (u Unimplemented) GetFoundryWorkflowRunInWorkspace(context.Context, string, string) (*model.FoundryWorkflowRun, error) {
	u.unimplemented("GetFoundryWorkflowRunInWorkspace")
	return nil, nil
}
func (u Unimplemented) GetFoundryWorkflowRunByClientToken(context.Context, string, string) (*model.FoundryWorkflowRun, error) {
	u.unimplemented("GetFoundryWorkflowRunByClientToken")
	return nil, nil
}
func (u Unimplemented) ListFoundryWorkflowRuns(context.Context, string, *access.Principal, string, int) ([]*model.FoundryWorkflowRun, string, error) {
	u.unimplemented("ListFoundryWorkflowRuns")
	return nil, "", nil
}

func (u Unimplemented) TallyFoundryWorkflowRuns(context.Context, []string, access.Principal) (map[string]model.FoundryWorkflowRunTally, error) {
	u.unimplemented("TallyFoundryWorkflowRuns")
	return nil, nil
}

func (u Unimplemented) ListFoundryWorkflowInsightRuns(context.Context, string, string, *access.Principal, bool, time.Time, time.Time,
	int) ([]model.FoundryWorkflowInsightRun, error) {
	u.unimplemented("ListFoundryWorkflowInsightRuns")
	return nil, nil
}
func (u Unimplemented) ListFoundryWorkflowRunAccesses(context.Context, string, int32, int) ([]*model.FoundryWorkflowAccess, bool, error) {
	u.unimplemented("ListFoundryWorkflowRunAccesses")
	return nil, false, nil
}
func (u Unimplemented) CreateFoundryWorkflow(context.Context, *model.FoundryWorkflow) error {
	u.unimplemented("CreateFoundryWorkflow")
	return nil
}
func (u Unimplemented) DeleteFoundryWorkflow(context.Context, string, string) error {
	u.unimplemented("DeleteFoundryWorkflow")
	return nil
}
func (u Unimplemented) StartFoundryWorkflowRun(context.Context, *model.FoundryWorkflowRun) error {
	u.unimplemented("StartFoundryWorkflowRun")
	return nil
}
func (u Unimplemented) FinishFoundryWorkflowRun(context.Context, string, *string, string, bool) error {
	u.unimplemented("FinishFoundryWorkflowRun")
	return nil
}
func (u Unimplemented) RecordFoundryWorkflowAccess(context.Context, string, model.FoundryWorkflowCall) (*model.FoundryWorkflowAccess, error) {
	u.unimplemented("RecordFoundryWorkflowAccess")
	return nil, nil
}
func (u Unimplemented) GetFoundryWorkflowRunAssessment(context.Context, string) (*model.FoundryWorkflowAssessment, error) {
	u.unimplemented("GetFoundryWorkflowRunAssessment")
	return nil, nil
}
func (u Unimplemented) ListFoundryWorkflowRunAssessments(context.Context, []string) (map[string]*model.FoundryWorkflowAssessment, error) {
	u.unimplemented("ListFoundryWorkflowRunAssessments")
	return nil, nil
}
func (u Unimplemented) CountFoundryWorkflowVerdicts(context.Context, []string) (map[string]map[string]int64, error) {
	u.unimplemented("CountFoundryWorkflowVerdicts")
	return nil, nil
}
func (u Unimplemented) SumFoundryWorkflowAssessmentSpend(context.Context, string, time.Time) (int64, error) {
	u.unimplemented("SumFoundryWorkflowAssessmentSpend")
	return 0, nil
}
func (u Unimplemented) QueueFoundryWorkflowRunAssessment(context.Context, string) error {
	u.unimplemented("QueueFoundryWorkflowRunAssessment")
	return nil
}
func (u Unimplemented) ClaimFoundryWorkflowRunAssessment(context.Context, string, time.Duration) (*model.FoundryWorkflowAssessment, error) {
	u.unimplemented("ClaimFoundryWorkflowRunAssessment")
	return nil, nil
}
func (u Unimplemented) RenewFoundryWorkflowRunAssessment(context.Context, string, string, time.Duration) error {
	u.unimplemented("RenewFoundryWorkflowRunAssessment")
	return nil
}
func (u Unimplemented) FinishFoundryWorkflowRunAssessment(context.Context, string, string, model.FoundryWorkflowAssessmentOutcome) error {
	u.unimplemented("FinishFoundryWorkflowRunAssessment")
	return nil
}

func (u Unimplemented) GetFoundryAgentThread(context.Context, string, string) (*model.FoundryAgentThread, error) {
	u.unimplemented("GetFoundryAgentThread")
	return nil, nil
}

func (u Unimplemented) ListFoundryAgentThreads(context.Context, string, model.FoundryAgentPrincipal, func(*model.FoundryAgentThread) bool, string, int) ([]*model.FoundryAgentThread, string, error) {
	u.unimplemented("ListFoundryAgentThreads")
	return nil, "", nil
}

func (u Unimplemented) RecordFoundryAgentThreadGrants(context.Context, string, string, *model.FoundryAgentThreadGrants) error {
	u.unimplemented("RecordFoundryAgentThreadGrants")
	return nil
}

func (u Unimplemented) GetFoundryAgentTurn(context.Context, string, string) (*model.FoundryAgentTurn, error) {
	u.unimplemented("GetFoundryAgentTurn")
	return nil, nil
}

func (u Unimplemented) GetFoundryAgentTurnByClientToken(context.Context, string, model.FoundryAgentPrincipal, string) (*model.FoundryAgentTurn, error) {
	u.unimplemented("GetFoundryAgentTurnByClientToken")
	return nil, nil
}

func (u Unimplemented) ListFoundryAgentTurns(context.Context, string, int) ([]*model.FoundryAgentTurn, error) {
	u.unimplemented("ListFoundryAgentTurns")
	return nil, nil
}

func (u Unimplemented) ListFoundryAgentEvents(context.Context, string, int, int) ([]model.FoundryAgentEvent, error) {
	u.unimplemented("ListFoundryAgentEvents")
	return nil, nil
}

func (u Unimplemented) ListFoundryAgentMessages(context.Context, string) ([]model.FoundryAgentMessage, error) {
	u.unimplemented("ListFoundryAgentMessages")
	return nil, nil
}

func (u Unimplemented) GetFoundryAgentAction(context.Context, string, string) (*model.FoundryAgentAction, error) {
	u.unimplemented("GetFoundryAgentAction")
	return nil, nil
}

func (u Unimplemented) GetFoundryAgentActionByPayload(context.Context, string, string) (*model.FoundryAgentAction, error) {
	u.unimplemented("GetFoundryAgentActionByPayload")
	return nil, nil
}

func (u Unimplemented) ListFoundryAgentActions(context.Context, string) ([]*model.FoundryAgentAction, error) {
	u.unimplemented("ListFoundryAgentActions")
	return nil, nil
}

func (u Unimplemented) CreateFoundryAgentTurn(context.Context, *model.FoundryAgentTurn, *model.FoundryAgentThread, model.FoundryAgentLimits, json.RawMessage) error {
	u.unimplemented("CreateFoundryAgentTurn")
	return nil
}

func (u Unimplemented) AskFoundryAgentQuestions(context.Context, string, string, *model.FoundryAgentAsked, json.RawMessage) error {
	u.unimplemented("AskFoundryAgentQuestions")
	return nil
}

func (u Unimplemented) DeleteFoundryAgentThread(context.Context, string, model.FoundryAgentPrincipal, string) error {
	u.unimplemented("DeleteFoundryAgentThread")
	return nil
}

func (u Unimplemented) CancelFoundryAgentTurn(context.Context, string, model.FoundryAgentPrincipal, string) (*model.FoundryAgentTurn, error) {
	u.unimplemented("CancelFoundryAgentTurn")
	return nil, nil
}

func (u Unimplemented) ClaimFoundryAgentTurn(context.Context, string, model.FoundryAgentClaim) (*model.FoundryAgentTurn, error) {
	u.unimplemented("ClaimFoundryAgentTurn")
	return nil, nil
}

func (u Unimplemented) RenewFoundryAgentTurnLease(context.Context, string, string, time.Duration) error {
	u.unimplemented("RenewFoundryAgentTurnLease")
	return nil
}

func (u Unimplemented) AppendFoundryAgentEvents(context.Context, string, string, []json.RawMessage) error {
	u.unimplemented("AppendFoundryAgentEvents")
	return nil
}

func (u Unimplemented) FinishFoundryAgentText(context.Context, string, string, int, []json.RawMessage) error {
	u.unimplemented("FinishFoundryAgentText")
	return nil
}

func (u Unimplemented) AppendFoundryAgentMessages(context.Context, string, string, []model.FoundryAgentMessage, int) error {
	u.unimplemented("AppendFoundryAgentMessages")
	return nil
}

func (u Unimplemented) SettleFoundryAgentTurn(context.Context, string, string, model.FoundryAgentTurnOutcome) error {
	u.unimplemented("SettleFoundryAgentTurn")
	return nil
}

func (u Unimplemented) SetFoundryAgentThreadSession(context.Context, string, string, string) error {
	u.unimplemented("SetFoundryAgentThreadSession")
	return nil
}

func (u Unimplemented) EndUnclaimedFoundryAgentTurns(context.Context, model.FoundryAgentRuntime, time.Time, string) (int64, error) {
	u.unimplemented("EndUnclaimedFoundryAgentTurns")
	return 0, nil
}

func (u Unimplemented) RecordFoundryAgentAction(context.Context, string, *model.FoundryAgentAction, int, func(*model.FoundryAgentAction) (json.RawMessage, error)) (*model.FoundryAgentAction, error) {
	u.unimplemented("RecordFoundryAgentAction")
	return nil, nil
}

func (u Unimplemented) ClaimFoundryAgentAction(context.Context, string, string, string, string) (*model.FoundryAgentAction, error) {
	u.unimplemented("ClaimFoundryAgentAction")
	return nil, nil
}

func (u Unimplemented) FinishFoundryAgentAction(context.Context, string, model.FoundryAgentActionEnd, *model.FoundryAgentTurn, json.RawMessage) error {
	u.unimplemented("FinishFoundryAgentAction")
	return nil
}

func (u Unimplemented) DismissFoundryAgentAction(context.Context, string, string, string) (*model.FoundryAgentAction, error) {
	u.unimplemented("DismissFoundryAgentAction")
	return nil, nil
}

func (u Unimplemented) ClaimFoundryAgentProposal(context.Context, string, string, string, string) (*model.FoundryAgentAction, error) {
	u.unimplemented("ClaimFoundryAgentProposal")
	return nil, nil
}

func (u Unimplemented) DismissFoundryAgentProposal(context.Context, string, string, string) (*model.FoundryAgentAction, error) {
	u.unimplemented("DismissFoundryAgentProposal")
	return nil, nil
}

func (u Unimplemented) RefreshFoundryAgentAction(context.Context, string, string, string, string, json.RawMessage, string, []byte, string, time.Time) (*model.FoundryAgentAction, error) {
	u.unimplemented("RefreshFoundryAgentAction")
	return nil, nil
}

func (u Unimplemented) GetFoundryDashboard(context.Context, string, string) (*model.FoundryDashboard, error) {
	u.unimplemented("GetFoundryDashboard")
	return nil, nil
}

func (u Unimplemented) GetFoundryDashboardByClientToken(context.Context, string, string, string) (*model.FoundryDashboard, error) {
	u.unimplemented("GetFoundryDashboardByClientToken")
	return nil, nil
}

func (u Unimplemented) ListFoundryDashboards(context.Context, string, bool) ([]*model.FoundryDashboard, error) {
	u.unimplemented("ListFoundryDashboards")
	return nil, nil
}

func (u Unimplemented) GetSharedFoundryDashboard(context.Context, string) (*model.FoundryDashboardShare, error) {
	u.unimplemented("GetSharedFoundryDashboard")
	return nil, nil
}

func (u Unimplemented) CreateFoundryDashboard(context.Context, *model.FoundryDashboard, int) error {
	u.unimplemented("CreateFoundryDashboard")
	return nil
}

func (u Unimplemented) UpdateFoundryDashboard(context.Context, string, string, model.FoundryDashboardEditor, int64, model.FoundryDashboardUpdate) error {
	u.unimplemented("UpdateFoundryDashboard")
	return nil
}

func (u Unimplemented) SetFoundryDashboardPinned(context.Context, string, string, string, int64, bool, int) error {
	u.unimplemented("SetFoundryDashboardPinned")
	return nil
}

func (u Unimplemented) DeleteFoundryDashboard(context.Context, string, string, model.FoundryDashboardEditor) error {
	u.unimplemented("DeleteFoundryDashboard")
	return nil
}

func (u Unimplemented) PublishFoundryDashboard(context.Context, *model.FoundryDashboardShare, model.FoundryDashboardLinkExpiry) (
	*model.FoundryDashboardShare, error) {
	u.unimplemented("PublishFoundryDashboard")
	return nil, nil
}

func (u Unimplemented) GetFoundryDashboardSettings(context.Context, string) (model.FoundryDashboardSettings, error) {
	u.unimplemented("GetFoundryDashboardSettings")
	return model.FoundryDashboardSettings{}, nil
}

func (u Unimplemented) SetFoundryDashboardSettings(context.Context, string, string, bool) error {
	u.unimplemented("SetFoundryDashboardSettings")
	return nil
}

func (u Unimplemented) UnpublishFoundryDashboard(context.Context, string, string) error {
	u.unimplemented("UnpublishFoundryDashboard")
	return nil
}

func (u Unimplemented) GetFoundryAutomation(context.Context, string, string) (*model.FoundryAutomation, error) {
	u.unimplemented("GetFoundryAutomation")
	return nil, nil
}

func (u Unimplemented) GetFoundryAutomationByClientToken(context.Context, string, string, string) (*model.FoundryAutomation, error) {
	u.unimplemented("GetFoundryAutomationByClientToken")
	return nil, nil
}

func (u Unimplemented) ListFoundryAutomations(context.Context, string) ([]*model.FoundryAutomation, error) {
	u.unimplemented("ListFoundryAutomations")
	return nil, nil
}

func (u Unimplemented) FoundryAutomationRunStats(context.Context, string, time.Time) (*model.FoundryAutomationRunStats, error) {
	u.unimplemented("FoundryAutomationRunStats")
	return nil, nil
}

func (u Unimplemented) GetFoundryAutomationRun(context.Context, string, string) (*model.FoundryAutomationRun, error) {
	u.unimplemented("GetFoundryAutomationRun")
	return nil, nil
}

func (u Unimplemented) ListFoundryAutomationRuns(context.Context, string, string, int) ([]*model.FoundryAutomationRun, string, error) {
	u.unimplemented("ListFoundryAutomationRuns")
	return nil, "", nil
}

func (u Unimplemented) ListFoundryAutomationRunNodes(context.Context, string) ([]*model.FoundryAutomationRunNode, error) {
	u.unimplemented("ListFoundryAutomationRunNodes")
	return nil, nil
}

func (u Unimplemented) GetFoundryAutomationNotice(context.Context, *model.FoundryNotification) (*model.FoundryAutomationNotice, error) {
	u.unimplemented("GetFoundryAutomationNotice")
	return nil, nil
}

func (u Unimplemented) DueFoundryAutomationSchedules(context.Context, int) ([]*model.FoundryAutomation, error) {
	u.unimplemented("DueFoundryAutomationSchedules")
	return nil, nil
}

func (u Unimplemented) WatchedFoundryAutomations(context.Context, time.Duration, int) ([]*model.FoundryAutomation, error) {
	u.unimplemented("WatchedFoundryAutomations")
	return nil, nil
}

func (u Unimplemented) FindFoundryAutomationEnded(context.Context, *model.FoundryAutomation, time.Duration, int) ([]model.FoundryAutomationEnded, error) {
	u.unimplemented("FindFoundryAutomationEnded")
	return nil, nil
}

func (u Unimplemented) CreateFoundryAutomation(context.Context, *model.FoundryAutomation, int) error {
	u.unimplemented("CreateFoundryAutomation")
	return nil
}

func (u Unimplemented) UpdateFoundryAutomation(context.Context, string, string, string, int64, model.FoundryAutomationUpdate) (*model.FoundryAutomation, error) {
	u.unimplemented("UpdateFoundryAutomation")
	return nil, nil
}

func (u Unimplemented) ApproveFoundryAutomation(context.Context, string, string, string, int64, *time.Time) (*model.FoundryAutomation, error) {
	u.unimplemented("ApproveFoundryAutomation")
	return nil, nil
}

func (u Unimplemented) PauseFoundryAutomation(context.Context, string, string, model.FoundryAutomationActor) (*model.FoundryAutomation, error) {
	u.unimplemented("PauseFoundryAutomation")
	return nil, nil
}

func (u Unimplemented) ResumeFoundryAutomation(context.Context, string, string, string, *time.Time) (*model.FoundryAutomation, error) {
	u.unimplemented("ResumeFoundryAutomation")
	return nil, nil
}

func (u Unimplemented) DeleteFoundryAutomation(context.Context, string, string, model.FoundryAutomationActor) error {
	u.unimplemented("DeleteFoundryAutomation")
	return nil
}

func (u Unimplemented) RunFoundryAutomationNow(context.Context, *model.FoundryAutomationRun, string, json.RawMessage, model.FoundryAutomationLimits) (*model.FoundryAutomationRun, error) {
	u.unimplemented("RunFoundryAutomationNow")
	return nil, nil
}

func (u Unimplemented) CancelFoundryAutomationRun(context.Context, string, string, model.FoundryAutomationActor) (*model.FoundryAutomationRun, error) {
	u.unimplemented("CancelFoundryAutomationRun")
	return nil, nil
}

func (u Unimplemented) PauseFoundryAutomationsOf(context.Context, string, string) error {
	u.unimplemented("PauseFoundryAutomationsOf")
	return nil
}

func (u Unimplemented) TouchFoundryAutomation(context.Context, string) error {
	u.unimplemented("TouchFoundryAutomation")
	return nil
}

func (u Unimplemented) FireFoundryAutomation(context.Context, model.FoundryAutomationFire, model.FoundryAutomationLimits) (model.FoundryAutomationFired, error) {
	u.unimplemented("FireFoundryAutomation")
	return "", nil
}

func (u Unimplemented) ClaimFoundryAutomationRun(context.Context, string, time.Duration) (*model.FoundryAutomationRun, error) {
	u.unimplemented("ClaimFoundryAutomationRun")
	return nil, nil
}

func (u Unimplemented) RenewFoundryAutomationRunLease(context.Context, string, string, time.Duration) error {
	u.unimplemented("RenewFoundryAutomationRunLease")
	return nil
}

func (u Unimplemented) StartFoundryAutomationNode(context.Context, string, string, string, model.FoundryAutomationNodeStatus) error {
	u.unimplemented("StartFoundryAutomationNode")
	return nil
}

func (u Unimplemented) StartFoundryAutomationAssistant(context.Context, string, string, string, *model.FoundryAgentThread, *model.FoundryAgentTurn) error {
	u.unimplemented("StartFoundryAutomationAssistant")
	return nil
}

func (u Unimplemented) EndFoundryAutomationNode(context.Context, string, string, string, model.FoundryAutomationNodeEnd) error {
	u.unimplemented("EndFoundryAutomationNode")
	return nil
}

func (u Unimplemented) AwaitFoundryAutomationJob(context.Context, string, string, string, model.FoundryAutomationJob, json.RawMessage) error {
	u.unimplemented("AwaitFoundryAutomationJob")
	return nil
}

func (u Unimplemented) MarkFoundryAutomationCause(context.Context, string, model.FoundryAutomationJob) error {
	u.unimplemented("MarkFoundryAutomationCause")
	return nil
}

func (u Unimplemented) YieldFoundryAutomationRun(context.Context, string, string, time.Time) error {
	u.unimplemented("YieldFoundryAutomationRun")
	return nil
}

func (u Unimplemented) RetryFoundryAutomationRun(context.Context, string, string, time.Time, string) error {
	u.unimplemented("RetryFoundryAutomationRun")
	return nil
}

func (u Unimplemented) FinishFoundryAutomationRun(context.Context, string, string, model.FoundryAutomationRunEnd) error {
	u.unimplemented("FinishFoundryAutomationRun")
	return nil
}

func (u Unimplemented) PruneFoundryAutomationRuns(context.Context, time.Time) (int, error) {
	u.unimplemented("PruneFoundryAutomationRuns")
	return 0, nil
}

func (u Unimplemented) LatestFoundryWorkflowAssessments(context.Context, []string) (map[string]*model.FoundryWorkflowAssessment, error) {
	u.unimplemented("LatestFoundryWorkflowAssessments")
	return nil, nil
}

func (u Unimplemented) GetFoundryAutomationWorkflowRun(context.Context, string, string) (*model.FoundryWorkflowRun, error) {
	u.unimplemented("GetFoundryAutomationWorkflowRun")
	return nil, nil
}

func (u Unimplemented) ListFoundryAutomationOntologies(context.Context, string, []string) (map[string][]model.FoundryAutomationOntology, error) {
	u.unimplemented("ListFoundryAutomationOntologies")
	return nil, nil
}

func (u Unimplemented) ListFoundryAutomationRunOntologies(context.Context, string) ([]model.FoundryAutomationRunOntology, error) {
	u.unimplemented("ListFoundryAutomationRunOntologies")
	return nil, nil
}

func (u Unimplemented) EnsureFoundryAutomationWorkflow(context.Context, string, string, string) (*model.FoundryWorkflow, error) {
	u.unimplemented("EnsureFoundryAutomationWorkflow")
	return nil, nil
}

func (u Unimplemented) StartFoundryAutomationWorkflowRun(context.Context, model.FoundryAutomationWorkflowStart) (*model.FoundryWorkflowRun, error) {
	u.unimplemented("StartFoundryAutomationWorkflowRun")
	return nil, nil
}

func (u Unimplemented) GetFoundryAutomationSuggestions(context.Context, string, string) (*model.FoundryAutomationSuggestions, error) {
	u.unimplemented("GetFoundryAutomationSuggestions")
	return nil, nil
}

func (u Unimplemented) SumFoundryAssistantSpend(context.Context, string, time.Time) (int64, error) {
	u.unimplemented("SumFoundryAssistantSpend")
	return 0, nil
}

func (u Unimplemented) QueueFoundryAutomationSuggestions(context.Context, string, string, string, []string, int, time.Duration) (
	*model.FoundryAutomationSuggestions, error) {
	u.unimplemented("QueueFoundryAutomationSuggestions")
	return nil, nil
}

func (u Unimplemented) ClaimFoundryAutomationSuggestions(context.Context, string, time.Duration) (*model.FoundryAutomationSuggestions, error) {
	u.unimplemented("ClaimFoundryAutomationSuggestions")
	return nil, nil
}

func (u Unimplemented) RenewFoundryAutomationSuggestionsLease(context.Context, string, string, string, time.Duration) error {
	u.unimplemented("RenewFoundryAutomationSuggestionsLease")
	return nil
}

func (u Unimplemented) FinishFoundryAutomationSuggestions(context.Context, string, string, string, model.FoundryAutomationSuggestionsEnd) error {
	u.unimplemented("FinishFoundryAutomationSuggestions")
	return nil
}

func (u Unimplemented) ChargeFoundryAutomationSuggestions(context.Context, string, int64) error {
	u.unimplemented("ChargeFoundryAutomationSuggestions")
	return nil
}

func (u Unimplemented) SourceSharingFor(context.Context, string, *connector.Email) ([]model.FoundrySourceSharing, error) {
	u.unimplemented("SourceSharingFor")
	return nil, nil
}

func (u Unimplemented) SourcePeopleByEmail(context.Context, string, []connector.Email) (map[connector.Email][]model.FoundrySourcePerson, error) {
	u.unimplemented("SourcePeopleByEmail")
	return nil, nil
}

func (u Unimplemented) SourceAudienceRead(context.Context, string, string) (model.FoundrySourceAudienceRead, error) {
	u.unimplemented("SourceAudienceRead")
	return model.FoundrySourceAudienceRead{}, nil
}

func (u Unimplemented) ListSourceAudiences(context.Context, string) ([]model.FoundrySourceAudience, error) {
	u.unimplemented("ListSourceAudiences")
	return nil, nil
}

func (u Unimplemented) ReplaceSourceAudiences(context.Context, string, string, time.Time, connector.Audiences) error {
	u.unimplemented("ReplaceSourceAudiences")
	return nil
}

func (u Unimplemented) RecordSourceAudienceProblem(context.Context, string, string, time.Time, string) error {
	u.unimplemented("RecordSourceAudienceProblem")
	return nil
}

func (u Unimplemented) GetFoundrySlackInstall(context.Context, string) (*model.SlackInstall, error) {
	u.unimplemented("GetFoundrySlackInstall")
	return nil, nil
}

func (u Unimplemented) GetFoundrySlackInstallByTeam(context.Context, string) (*model.SlackInstall, error) {
	u.unimplemented("GetFoundrySlackInstallByTeam")
	return nil, nil
}

func (u Unimplemented) GetFoundrySlackInstallByEnterprise(context.Context, string) (*model.SlackInstall, error) {
	u.unimplemented("GetFoundrySlackInstallByEnterprise")
	return nil, nil
}

func (u Unimplemented) GetFoundrySlackInstallByID(context.Context, string) (*model.SlackInstall, error) {
	u.unimplemented("GetFoundrySlackInstallByID")
	return nil, nil
}

func (u Unimplemented) GetFoundrySlackInstallToken(context.Context, string) (model.SlackSealedToken, error) {
	u.unimplemented("GetFoundrySlackInstallToken")
	return model.SlackSealedToken{}, nil
}

func (u Unimplemented) ListFoundrySlackUninstalls(context.Context, int) ([]*model.SlackInstall, error) {
	u.unimplemented("ListFoundrySlackUninstalls")
	return nil, nil
}

func (u Unimplemented) GetFoundrySlackLink(context.Context, string, string) (*model.SlackLink, error) {
	u.unimplemented("GetFoundrySlackLink")
	return nil, nil
}

func (u Unimplemented) ListFoundrySlackLinks(context.Context, string) ([]*model.SlackLink, error) {
	u.unimplemented("ListFoundrySlackLinks")
	return nil, nil
}

func (u Unimplemented) GetFoundrySlackLinkToken(context.Context, string) (*model.SlackLinkToken, error) {
	u.unimplemented("GetFoundrySlackLinkToken")
	return nil, nil
}

func (u Unimplemented) GetFoundrySlackThread(context.Context, string, string, string, string) (*model.SlackThread, error) {
	u.unimplemented("GetFoundrySlackThread")
	return nil, nil
}

func (u Unimplemented) GetFoundrySlackReply(context.Context, string) (*model.SlackReply, error) {
	u.unimplemented("GetFoundrySlackReply")
	return nil, nil
}

func (u Unimplemented) ListFoundrySlackFollows(context.Context, string, string, string) ([]*model.SlackReply, error) {
	u.unimplemented("ListFoundrySlackFollows")
	return nil, nil
}

func (u Unimplemented) CountFoundrySlackFollows(context.Context, string) (int, error) {
	u.unimplemented("CountFoundrySlackFollows")
	return 0, nil
}

func (u Unimplemented) InstallFoundrySlack(context.Context, *model.SlackInstall, func(string) (model.SlackSealedToken, error)) (
	*model.SlackInstall, bool, error) {
	u.unimplemented("InstallFoundrySlack")
	return nil, false, nil
}

func (u Unimplemented) RevokeFoundrySlackInstall(context.Context, string, string, bool) error {
	u.unimplemented("RevokeFoundrySlackInstall")
	return nil
}

func (u Unimplemented) MarkFoundrySlackUninstalled(context.Context, string) error {
	u.unimplemented("MarkFoundrySlackUninstalled")
	return nil
}

func (u Unimplemented) UninstallFoundrySlackTeam(context.Context, string, string, string, time.Time) (model.SlackUninstallOutcome, error) {
	u.unimplemented("UninstallFoundrySlackTeam")
	return "", nil
}

func (u Unimplemented) PurgeFoundrySlack(context.Context, string) error {
	u.unimplemented("PurgeFoundrySlack")
	return nil
}

func (u Unimplemented) CreateFoundrySlackInstallState(context.Context, string, string, string, string, time.Duration) error {
	u.unimplemented("CreateFoundrySlackInstallState")
	return nil
}

func (u Unimplemented) SpendFoundrySlackInstallState(context.Context, string, string) (*model.SlackInstallState, error) {
	u.unimplemented("SpendFoundrySlackInstallState")
	return nil, nil
}

func (u Unimplemented) CreateFoundrySlackLinkToken(context.Context, string, string, string, time.Duration) error {
	u.unimplemented("CreateFoundrySlackLinkToken")
	return nil
}

func (u Unimplemented) LinkFoundrySlackUser(context.Context, string, string, string) (*model.SlackLink, error) {
	u.unimplemented("LinkFoundrySlackUser")
	return nil, nil
}

func (u Unimplemented) DeleteFoundrySlackLink(context.Context, string, string) error {
	u.unimplemented("DeleteFoundrySlackLink")
	return nil
}

func (u Unimplemented) CreateFoundrySlackThread(context.Context, *model.SlackThread) error {
	u.unimplemented("CreateFoundrySlackThread")
	return nil
}

func (u Unimplemented) TouchFoundrySlackThread(context.Context, string) error {
	u.unimplemented("TouchFoundrySlackThread")
	return nil
}

func (u Unimplemented) PurgeFoundrySlackThreads(context.Context, time.Duration, int) (int, error) {
	u.unimplemented("PurgeFoundrySlackThreads")
	return 0, nil
}

func (u Unimplemented) QueueFoundrySlackIntake(context.Context, *model.SlackIntake) error {
	u.unimplemented("QueueFoundrySlackIntake")
	return nil
}

func (u Unimplemented) ClaimFoundrySlackIntake(context.Context, time.Duration) (*model.SlackIntake, error) {
	u.unimplemented("ClaimFoundrySlackIntake")
	return nil, nil
}

func (u Unimplemented) SetFoundrySlackIntakeInput(context.Context, string, string, string) error {
	u.unimplemented("SetFoundrySlackIntakeInput")
	return nil
}

func (u Unimplemented) EndFoundrySlackIntake(context.Context, string, string, model.SlackIntakeOutcome) error {
	u.unimplemented("EndFoundrySlackIntake")
	return nil
}

func (u Unimplemented) WakeFoundrySlackIntake(context.Context, string) (int64, error) {
	u.unimplemented("WakeFoundrySlackIntake")
	return 0, nil
}

func (u Unimplemented) HoldFoundrySlackReply(context.Context, *model.SlackReply) (*model.SlackReply, error) {
	u.unimplemented("HoldFoundrySlackReply")
	return nil, nil
}

func (u Unimplemented) ReleaseFoundrySlackHold(context.Context, string) error {
	u.unimplemented("ReleaseFoundrySlackHold")
	return nil
}

func (u Unimplemented) StartFoundrySlackReply(context.Context, string, string, string, string) error {
	u.unimplemented("StartFoundrySlackReply")
	return nil
}

func (u Unimplemented) ClaimFoundrySlackReply(context.Context, time.Duration) (*model.SlackReply, error) {
	u.unimplemented("ClaimFoundrySlackReply")
	return nil, nil
}

func (u Unimplemented) UpdateFoundrySlackReply(context.Context, string, string, model.SlackReplyUpdate) error {
	u.unimplemented("UpdateFoundrySlackReply")
	return nil
}

func (u Unimplemented) ReleaseFoundrySlackReply(context.Context, string, string, time.Duration) error {
	u.unimplemented("ReleaseFoundrySlackReply")
	return nil
}

func (u Unimplemented) FinishFoundrySlackReply(context.Context, string, string) error {
	u.unimplemented("FinishFoundrySlackReply")
	return nil
}

func (u Unimplemented) PruneFoundrySlack(context.Context, time.Duration, time.Duration, int) (int64, error) {
	u.unimplemented("PruneFoundrySlack")
	return 0, nil
}

func (u Unimplemented) ListFoundrySlackThreadMappings(context.Context, string, string, string) ([]*model.SlackThread, error) {
	u.unimplemented("ListFoundrySlackThreadMappings")
	return nil, nil
}

func (u Unimplemented) GetFoundrySlackFeed(context.Context, string) (*model.SlackFeed, error) {
	u.unimplemented("GetFoundrySlackFeed")
	return nil, nil
}

func (u Unimplemented) FoundrySlackFeedAfter(context.Context, string, int64, model.SlackFeedBudget) ([]model.SlackFeedRow, error) {
	u.unimplemented("FoundrySlackFeedAfter")
	return nil, nil
}

func (u Unimplemented) FoundrySlackDaysAfter(context.Context, string, int64) ([]model.SlackFeedDay, error) {
	u.unimplemented("FoundrySlackDaysAfter")
	return nil, nil
}

func (u Unimplemented) ListFoundrySlackFeedChannels(context.Context, string) ([]model.SlackChannelJoin, error) {
	u.unimplemented("ListFoundrySlackFeedChannels")
	return nil, nil
}

func (u Unimplemented) DueFoundrySlackBackfill(context.Context, model.SlackSlotMethod, int) ([]model.SlackBackfillWork, error) {
	u.unimplemented("DueFoundrySlackBackfill")
	return nil, nil
}

func (u Unimplemented) FoundrySlackBackfillProgress(context.Context, string) (model.SlackBackfillProgress, error) {
	u.unimplemented("FoundrySlackBackfillProgress")
	return model.SlackBackfillProgress{}, nil
}

func (u Unimplemented) ListFoundrySlackOrphans(context.Context, time.Duration, int) ([]*model.SlackFeed, error) {
	u.unimplemented("ListFoundrySlackOrphans")
	return nil, nil
}

func (u Unimplemented) UpdateFoundrySlackInstallBot(context.Context, string, model.SlackInstallBotUpdate) error {
	u.unimplemented("UpdateFoundrySlackInstallBot")
	return nil
}

func (u Unimplemented) MarkFoundrySlackThreadBot(context.Context, string, string, string, time.Time) error {
	u.unimplemented("MarkFoundrySlackThreadBot")
	return nil
}

func (u Unimplemented) MarkFoundrySlackThreadOthers(context.Context, string, string, string, time.Time) error {
	u.unimplemented("MarkFoundrySlackThreadOthers")
	return nil
}

func (u Unimplemented) SetFoundrySlackMode(context.Context, string, string, model.SlackIdentityMode) (model.SlackIdentityMode, error) {
	u.unimplemented("SetFoundrySlackMode")
	return nil, nil
}

func (u Unimplemented) AddFoundrySlackThreadPerson(context.Context, string, string) error {
	u.unimplemented("AddFoundrySlackThreadPerson")
	return nil
}

func (u Unimplemented) IsFoundrySlackBot(context.Context, string, string) (bool, error) {
	u.unimplemented("IsFoundrySlackBot")
	return false, nil
}

func (u Unimplemented) IsFoundrySlackBotCredential(context.Context, string) (bool, error) {
	u.unimplemented("IsFoundrySlackBotCredential")
	return false, nil
}

func (u Unimplemented) ListFoundrySlackRetiredBots(context.Context, int) ([]*model.SlackInstall, error) {
	u.unimplemented("ListFoundrySlackRetiredBots")
	return nil, nil
}

func (u Unimplemented) ForgetFoundrySlackBot(context.Context, string, string) error {
	u.unimplemented("ForgetFoundrySlackBot")
	return nil
}

func (u Unimplemented) IsFoundrySlackThreadPerson(context.Context, string, string) (bool, error) {
	u.unimplemented("IsFoundrySlackThreadPerson")
	return false, nil
}

func (u Unimplemented) CountFoundrySlackUserMessages(context.Context, string, string, string, time.Duration) (int, error) {
	u.unimplemented("CountFoundrySlackUserMessages")
	return 0, nil
}

func (u Unimplemented) CreateSlackAppConnector(context.Context, *model.Connector, model.SlackFeedBind) error {
	u.unimplemented("CreateSlackAppConnector")
	return nil
}

func (u Unimplemented) UpdateSlackAppConnector(context.Context, string, string, model.ConnectorUpdate, model.SlackAppSwitch) error {
	u.unimplemented("UpdateSlackAppConnector")
	return nil
}

func (u Unimplemented) AppendFoundrySlackFeed(context.Context, string, model.SlackFeedBatch, model.SlackFeedLimits) (model.SlackFeedAppended, error) {
	u.unimplemented("AppendFoundrySlackFeed")
	return model.SlackFeedAppended{}, nil
}

func (u Unimplemented) AppendFoundrySlackFeedGaps(context.Context, string, []model.SlackFeedGap, time.Duration) (model.SlackFeedAppended, error) {
	u.unimplemented("AppendFoundrySlackFeedGaps")
	return model.SlackFeedAppended{}, nil
}

func (u Unimplemented) RecordFoundrySlackEventsLimited(context.Context, string, time.Time, time.Duration) (model.SlackFeedAppended, error) {
	u.unimplemented("RecordFoundrySlackEventsLimited")
	return model.SlackFeedAppended{}, nil
}

func (u Unimplemented) CommitFoundrySlackFold(context.Context, model.SlackFold) error {
	u.unimplemented("CommitFoundrySlackFold")
	return nil
}

func (u Unimplemented) ForgetFoundrySlackObjects(context.Context, string, []string) error {
	u.unimplemented("ForgetFoundrySlackObjects")
	return nil
}

func (u Unimplemented) MarkFoundrySlackDaysDocumented(context.Context, string, []model.SlackDayKey) error {
	u.unimplemented("MarkFoundrySlackDaysDocumented")
	return nil
}

func (u Unimplemented) PruneFoundrySlackFeed(context.Context, int) (int64, error) {
	u.unimplemented("PruneFoundrySlackFeed")
	return 0, nil
}

func (u Unimplemented) JoinFoundrySlackChannels(context.Context, string, []model.SlackChannelJoin) (int, error) {
	u.unimplemented("JoinFoundrySlackChannels")
	return 0, nil
}

func (u Unimplemented) PurgeFoundrySlackChannel(context.Context, string, string) error {
	u.unimplemented("PurgeFoundrySlackChannel")
	return nil
}

func (u Unimplemented) ReserveFoundrySlackSlot(context.Context, string, model.SlackSlotMethod, time.Duration) (bool, error) {
	u.unimplemented("ReserveFoundrySlackSlot")
	return false, nil
}

func (u Unimplemented) DeferFoundrySlackSlot(context.Context, string, model.SlackSlotMethod, time.Duration) error {
	u.unimplemented("DeferFoundrySlackSlot")
	return nil
}

func (u Unimplemented) AdvanceFoundrySlackBackfill(context.Context, model.SlackBackfillStep, time.Duration) error {
	u.unimplemented("AdvanceFoundrySlackBackfill")
	return nil
}

func (u Unimplemented) QueueFoundrySlackThreads(context.Context, []model.SlackBackfillThread) (int, error) {
	u.unimplemented("QueueFoundrySlackThreads")
	return 0, nil
}

func (u Unimplemented) FailFoundrySlackBackfill(context.Context, model.SlackBackfillWindow, string, int) (model.SlackBackfillStatus, error) {
	u.unimplemented("FailFoundrySlackBackfill")
	return "", nil
}

func (u Unimplemented) EndFoundrySlackBackfill(context.Context, model.SlackBackfillWindow, model.SlackBackfillStatus, string) error {
	u.unimplemented("EndFoundrySlackBackfill")
	return nil
}

func (u Unimplemented) SpendFoundrySlackGap(context.Context, string, int) (bool, error) {
	u.unimplemented("SpendFoundrySlackGap")
	return false, nil
}

func (u Unimplemented) ListFoundrySlackFeeds(context.Context, string, int) ([]*model.SlackFeed, error) {
	u.unimplemented("ListFoundrySlackFeeds")
	return nil, nil
}

func (u Unimplemented) CloseFoundrySlackDays(context.Context, string, time.Time, time.Duration) (int, error) {
	u.unimplemented("CloseFoundrySlackDays")
	return 0, nil
}

func (u Unimplemented) FlipSlackAppConnector(context.Context, string, string, json.RawMessage, json.RawMessage, []byte, string) error {
	u.unimplemented("FlipSlackAppConnector")
	return nil
}

func (u Unimplemented) GetFoundryRelayLinkByToken(context.Context, string) (*model.RelayLink, error) {
	u.unimplemented("GetFoundryRelayLinkByToken")
	return nil, nil
}

func (u Unimplemented) ListFoundryRelayLinks(context.Context, string) ([]*model.RelayLink, error) {
	u.unimplemented("ListFoundryRelayLinks")
	return nil, nil
}

func (u Unimplemented) ListFoundryRelayInstalls(context.Context, string) ([]*model.SlackInstall, error) {
	u.unimplemented("ListFoundryRelayInstalls")
	return nil, nil
}

func (u Unimplemented) GetFoundryRelayInstall(context.Context, string, string) (*model.SlackInstall, error) {
	u.unimplemented("GetFoundryRelayInstall")
	return nil, nil
}

func (u Unimplemented) FoundryRelayWaiting(context.Context, string) (bool, error) {
	u.unimplemented("FoundryRelayWaiting")
	return false, nil
}

func (u Unimplemented) SeenFoundryRelayChannel(context.Context, string, string) (time.Duration, bool, error) {
	u.unimplemented("SeenFoundryRelayChannel")
	return 0, false, nil
}

func (u Unimplemented) ForgetFoundryRelayChannel(context.Context, string, string) error {
	u.unimplemented("ForgetFoundryRelayChannel")
	return nil
}

func (u Unimplemented) KnowsFoundryRelayUsers(context.Context, string, []string) (bool, error) {
	u.unimplemented("KnowsFoundryRelayUsers")
	return false, nil
}

func (u Unimplemented) GetFoundryRelayUsage(context.Context, string, time.Time) (model.RelayUsage, error) {
	u.unimplemented("GetFoundryRelayUsage")
	return model.RelayUsage{}, nil
}

func (u Unimplemented) ListFoundrySlackAccessRequests(context.Context, string) ([]*model.SlackAccessRequest, error) {
	u.unimplemented("ListFoundrySlackAccessRequests")
	return nil, nil
}

func (u Unimplemented) CreateFoundryRelayCode(context.Context, *model.RelayCode, time.Duration) error {
	u.unimplemented("CreateFoundryRelayCode")
	return nil
}

func (u Unimplemented) RedeemFoundryRelayCode(context.Context, string, string, string, string) (*model.RelayLink, error) {
	u.unimplemented("RedeemFoundryRelayCode")
	return nil, nil
}

func (u Unimplemented) SeeFoundryRelayLink(context.Context, string, string) error {
	u.unimplemented("SeeFoundryRelayLink")
	return nil
}

func (u Unimplemented) RevokeFoundryRelayLink(context.Context, string, string) error {
	u.unimplemented("RevokeFoundryRelayLink")
	return nil
}

func (u Unimplemented) CreateFoundrySlackRelayInstallState(context.Context, string, string, string, time.Duration) error {
	u.unimplemented("CreateFoundrySlackRelayInstallState")
	return nil
}

func (u Unimplemented) SetFoundryRelayFeeds(context.Context, string, []model.RelayFeedOn) error {
	u.unimplemented("SetFoundryRelayFeeds")
	return nil
}

func (u Unimplemented) QueueFoundryRelayDelivery(context.Context, *model.RelayDelivery, time.Duration, model.RelaySeen) error {
	u.unimplemented("QueueFoundryRelayDelivery")
	return nil
}

func (u Unimplemented) LeaseFoundryRelay(context.Context, string, time.Duration, model.RelayBudget) (model.RelayClaim, error) {
	u.unimplemented("LeaseFoundryRelay")
	return model.RelayClaim{}, nil
}

func (u Unimplemented) AckFoundryRelay(context.Context, string, []string) (int, error) {
	u.unimplemented("AckFoundryRelay")
	return 0, nil
}

func (u Unimplemented) ExpireFoundryRelayDeliveries(context.Context, time.Duration, int) ([]*model.RelayDelivery, error) {
	u.unimplemented("ExpireFoundryRelayDeliveries")
	return nil, nil
}

func (u Unimplemented) NoticeFoundryRelayOffline(context.Context, time.Duration, time.Duration, int) ([]*model.RelayDelivery, error) {
	u.unimplemented("NoticeFoundryRelayOffline")
	return nil, nil
}

func (u Unimplemented) AppendFoundryRelayFeed(context.Context, string, model.RelayFeedAppend, model.RelayFeedLimits) (model.RelayFeedAppended, error) {
	u.unimplemented("AppendFoundryRelayFeed")
	return model.RelayFeedAppended{}, nil
}

func (u Unimplemented) ExpireFoundryRelayFeed(context.Context, time.Duration, int) (int, error) {
	u.unimplemented("ExpireFoundryRelayFeed")
	return 0, nil
}

func (u Unimplemented) RecordFoundryRelayChannels(context.Context, string, []string) error {
	u.unimplemented("RecordFoundryRelayChannels")
	return nil
}

func (u Unimplemented) CountFoundryRelayUsage(context.Context, string, model.RelayUsage) error {
	u.unimplemented("CountFoundryRelayUsage")
	return nil
}

func (u Unimplemented) MirrorFoundrySlackInstalls(context.Context, []*model.SlackInstall) error {
	u.unimplemented("MirrorFoundrySlackInstalls")
	return nil
}

func (u Unimplemented) AllowFoundrySlackUser(context.Context, string, string, string) (*model.SlackLink, error) {
	u.unimplemented("AllowFoundrySlackUser")
	return nil, nil
}

func (u Unimplemented) RequestFoundrySlackAccess(context.Context, *model.SlackAccessRequest) (bool, error) {
	u.unimplemented("RequestFoundrySlackAccess")
	return false, nil
}

func (u Unimplemented) DeleteFoundrySlackAccessRequest(context.Context, string, string) error {
	u.unimplemented("DeleteFoundrySlackAccessRequest")
	return nil
}

// The review cards index.
func (u Unimplemented) ListFoundryClaimDecisions(context.Context, string, []string) ([]*model.FoundryReviewDecision, error) {
	u.unimplemented("ListFoundryClaimDecisions")
	return nil, nil
}
func (u Unimplemented) ListFoundryReviewSummaries(context.Context, string) ([]model.FoundryReviewSummary, error) {
	u.unimplemented("ListFoundryReviewSummaries")
	return nil, nil
}
func (u Unimplemented) FoundryReviewIndexing(context.Context, string) (bool, error) {
	u.unimplemented("FoundryReviewIndexing")
	return false, nil
}
func (u Unimplemented) ListFoundryReviewCards(context.Context, string, string, model.FoundryReviewCardFilter, *model.FoundryReviewCardCursor, int) ([]*model.FoundryReviewListedCard, error) {
	u.unimplemented("ListFoundryReviewCards")
	return nil, nil
}
func (u Unimplemented) ListFoundryReviewNameDecisions(context.Context, string, [][2]string) ([]*model.FoundryReviewNameDecisions, error) {
	u.unimplemented("ListFoundryReviewNameDecisions")
	return nil, nil
}
func (u Unimplemented) FoundryReviewPrecision(context.Context, string) (model.FoundryReviewPrecision, error) {
	u.unimplemented("FoundryReviewPrecision")
	return model.FoundryReviewPrecision{}, nil
}
func (u Unimplemented) ListFoundryReviewRechecks(context.Context, string, string, int) ([]*model.FoundryReviewDecision, []*model.FoundryReviewLabel, error) {
	u.unimplemented("ListFoundryReviewRechecks")
	return nil, nil, nil
}
func (u Unimplemented) ListFoundryReviewLabelsAt(context.Context, string, []model.FoundryReviewDraw) ([]*model.FoundryReviewLabel, error) {
	u.unimplemented("ListFoundryReviewLabelsAt")
	return nil, nil
}
func (u Unimplemented) ListFoundryReviewDraws(context.Context, string, string) ([]model.FoundryReviewDraw, error) {
	u.unimplemented("ListFoundryReviewDraws")
	return nil, nil
}
func (u Unimplemented) FoundryReviewDrawPools(context.Context, string) ([6]reviewcards.Pool, error) {
	u.unimplemented("FoundryReviewDrawPools")
	return [6]reviewcards.Pool{}, nil
}
func (u Unimplemented) FoundryReviewQuoteDocuments(context.Context, string, int, int, int) ([]string, error) {
	u.unimplemented("FoundryReviewQuoteDocuments")
	return nil, nil
}
func (u Unimplemented) FoundryReviewPlacesTaken(context.Context, string, string, string) (map[string]bool, error) {
	u.unimplemented("FoundryReviewPlacesTaken")
	return nil, nil
}
func (u Unimplemented) ListFoundryReviewDecisionsByID(context.Context, string, []string, bool) ([]*model.FoundryReviewDecision, error) {
	u.unimplemented("ListFoundryReviewDecisionsByID")
	return nil, nil
}
func (u Unimplemented) RequestFoundryReviewPasses(context.Context) (int, error) {
	u.unimplemented("RequestFoundryReviewPasses")
	return 0, nil
}
func (u Unimplemented) AdvanceFoundryReviewPasses(context.Context) (int, error) {
	u.unimplemented("AdvanceFoundryReviewPasses")
	return 0, nil
}
func (u Unimplemented) RecountFoundryReviewLoads(context.Context, int) (int, error) {
	u.unimplemented("RecountFoundryReviewLoads")
	return 0, nil
}
func (u Unimplemented) ClearFoundryReviewLoads(context.Context, int) (int, error) {
	u.unimplemented("ClearFoundryReviewLoads")
	return 0, nil
}
func (u Unimplemented) ParkFoundryReviewLoad(context.Context, string) error {
	u.unimplemented("ParkFoundryReviewLoad")
	return nil
}
func (u Unimplemented) ReserveFoundryReviewRead(context.Context, string, time.Duration) (time.Duration, error) {
	u.unimplemented("ReserveFoundryReviewRead")
	return 0, nil
}
func (u Unimplemented) ClaimFoundryReviewDocuments(context.Context, string, time.Duration) ([]model.FoundryReviewClaim, error) {
	u.unimplemented("ClaimFoundryReviewDocuments")
	return nil, nil
}
func (u Unimplemented) StoreFoundryReviewDocuments(context.Context, string, model.FoundryReviewClaim, []model.FoundryReviewIndexed) (bool, error) {
	u.unimplemented("StoreFoundryReviewDocuments")
	return false, nil
}
func (u Unimplemented) BackOffFoundryReviewDocument(context.Context, string, string, string) (bool, error) {
	u.unimplemented("BackOffFoundryReviewDocument")
	return false, nil
}
func (u Unimplemented) BackOffFoundryReviewLoad(context.Context, string) error {
	u.unimplemented("BackOffFoundryReviewLoad")
	return nil
}
func (u Unimplemented) DrawFoundryHeldClaim(context.Context, string, int, int) (bool, error) {
	u.unimplemented("DrawFoundryHeldClaim")
	return false, nil
}
func (u Unimplemented) InsertFoundryReviewQuoteDraw(context.Context, model.FoundryReviewDraw) error {
	u.unimplemented("InsertFoundryReviewQuoteDraw")
	return nil
}
