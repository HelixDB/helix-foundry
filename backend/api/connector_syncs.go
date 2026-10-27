package api

import (
	"context"
	"encoding/json"
	"errors"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/connector"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

const (
	defaultSyncListLimit = 20
	maxSyncListLimit     = 100
)

// SyncConnector queues a sync for the foundry-sync-worker. A connector has at
// most one live sync; a second request is AlreadyExists. An event source
// syncs when pieces arrive, so one asked of it resumes a paused source or
// reads it in full; it is refused while the source holds no batch, and like
// every sync of an event source it has origin events.
func (s *Service) SyncConnector(ctx context.Context, req *foundryv1.SyncConnectorRequest) (*foundryv1.ConnectorSyncResponse, error) {
	catalog, _, err := s.connectors(req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	c, err := s.loadConnector(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	kind, err := catalog.Kind(c.Kind)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "connector kind %q is no longer supported", c.Kind)
	}
	if err := s.requireKind(ctx, c.WorkspaceID, kind); err != nil {
		return nil, err
	}
	mode := foundrymodel.ConnectorSyncChanges
	if req.GetFull() {
		mode = foundrymodel.ConnectorSyncFull
	}
	origin := foundrymodel.ConnectorSyncManual
	if c.Kind == foundrymodel.ConnectorKindEvents {
		list, err := s.deps.DB.ListFoundryEventBatches(ctx, c.WorkspaceID, c.ID, 1)
		if err != nil {
			return nil, eventSourceError("list the API source's batches", c.ID, err)
		}
		if len(list.Batches) == 0 {
			return nil, status.Error(codes.FailedPrecondition, "an API source syncs when pieces arrive; send some first")
		}
		origin = foundrymodel.ConnectorSyncEvents
	}
	stored, err := s.queueConnectorSync(ctx, c, origin, mode)
	if err != nil {
		return nil, err
	}
	return &foundryv1.ConnectorSyncResponse{Sync: syncToProto(stored)}, nil
}

func (s *Service) queueConnectorSync(ctx context.Context, c *foundrymodel.Connector,
	origin foundrymodel.ConnectorSyncOrigin, mode foundrymodel.ConnectorSyncMode) (*foundrymodel.ConnectorSync, error) {
	sync := &foundrymodel.ConnectorSync{ID: id.New(), ConnectorID: c.ID, WorkspaceID: c.WorkspaceID, Origin: origin, Mode: mode}
	if userID := actor(ctx).UserID; userID != "" {
		sync.RequestedByUserID = &userID
	}
	err := s.deps.DB.EnqueueConnectorSync(ctx, sync)
	if errors.Is(err, foundrystore.ErrConflict) {
		return nil, status.Error(codes.AlreadyExists, "a sync of this connector is already queued or running")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "queue connector sync: %v", err)
	}
	stored, err := s.deps.DB.GetConnectorSync(ctx, c.WorkspaceID, sync.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read queued sync: %v", err)
	}
	return stored, nil
}

func (s *Service) GetConnectorSync(ctx context.Context, req *foundryv1.GetConnectorSyncRequest) (*foundryv1.ConnectorSyncResponse, error) {
	if req.GetWorkspaceId() == "" || req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and id are required")
	}
	sync, err := s.deps.DB.GetConnectorSync(ctx, req.GetWorkspaceId(), req.GetId())
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "sync not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get connector sync: %v", err)
	}
	out, err := s.syncWithChanges(ctx, sync)
	if err != nil {
		return nil, err
	}
	return &foundryv1.ConnectorSyncResponse{Sync: out}, nil
}

func (s *Service) ListConnectorSyncs(ctx context.Context, req *foundryv1.ListConnectorSyncsRequest) (*foundryv1.ListConnectorSyncsResponse, error) {
	if _, err := s.loadConnector(ctx, req.GetWorkspaceId(), req.GetConnectorId()); err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultSyncListLimit
	}
	limit = min(limit, maxSyncListLimit)
	syncs, err := s.deps.DB.ListConnectorSyncs(ctx, req.GetWorkspaceId(), req.GetConnectorId(), limit)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list connector syncs: %v", err)
	}
	resp := &foundryv1.ListConnectorSyncsResponse{}
	for _, sync := range syncs {
		out, err := s.syncWithChanges(ctx, sync)
		if err != nil {
			return nil, err
		}
		resp.Syncs = append(resp.Syncs, out)
	}
	return resp, nil
}

// syncWithChanges is the sync with what it changed in each table.
func (s *Service) syncWithChanges(ctx context.Context, sync *foundrymodel.ConnectorSync) (*foundryv1.ConnectorSync, error) {
	out := syncToProto(sync)
	batches, err := s.deps.DB.ListConnectorChangeBatches(ctx, sync.WorkspaceID, sync.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list sync %s changes: %v", sync.ID, err)
	}
	// A retried sync can record several batches for a table; they add up to one change.
	byTable := map[string]*foundryv1.ConnectorTableChange{}
	for _, b := range batches {
		change, ok := byTable[b.TableName]
		if !ok {
			change = &foundryv1.ConnectorTableChange{Table: b.TableName, Kind: string(b.Kind)}
			byTable[b.TableName] = change
			out.Tables = append(out.Tables, change)
		}
		change.Inserts += b.Inserts
		change.Updates += b.Updates
		change.Deletes += b.Deletes
	}
	return out, nil
}

var syncStatusToProto = map[foundrymodel.ConnectorSyncStatus]foundryv1.ConnectorSyncStatus{
	foundrymodel.ConnectorSyncQueued:    foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_QUEUED,
	foundrymodel.ConnectorSyncRunning:   foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_RUNNING,
	foundrymodel.ConnectorSyncSucceeded: foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_SUCCEEDED,
	foundrymodel.ConnectorSyncFailed:    foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_FAILED,
}

func syncToProto(sync *foundrymodel.ConnectorSync) *foundryv1.ConnectorSync {
	out := &foundryv1.ConnectorSync{
		Id: sync.ID, ConnectorId: sync.ConnectorID, Status: syncStatusToProto[sync.Status], Attempts: int32(sync.Attempts),
		CreatedAt: timestamppb.New(sync.CreatedAt), NextAttemptAt: timestamppb.New(sync.NextAttemptAt),
		Origin: string(sync.Origin), Mode: string(sync.Mode),
	}
	if sync.Error != nil {
		out.Error = *sync.Error
	}
	if sync.RowsSynced != nil {
		out.RowsSynced = *sync.RowsSynced
	}
	if sync.StartedAt != nil {
		out.StartedAt = timestamppb.New(*sync.StartedAt)
	}
	if sync.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*sync.FinishedAt)
	}
	return out
}

func syncedTableToProto(t *foundrymodel.ConnectorTable) (*foundryv1.SyncedTable, error) {
	var columns []connector.Column
	var primaryKey []string
	var foreignKeys []connector.ForeignKey
	for _, field := range []struct {
		raw json.RawMessage
		dst any
	}{{t.Columns, &columns}, {t.PrimaryKey, &primaryKey}, {t.ForeignKeys, &foreignKeys}} {
		if err := json.Unmarshal(field.raw, field.dst); err != nil {
			return nil, err
		}
	}
	d := discoveryToProto(connector.Discovery{Tables: []connector.Table{{
		Name: t.TableName, Columns: columns, PrimaryKey: primaryKey, ForeignKeys: foreignKeys,
	}}}).GetTables()[0]
	out := &foundryv1.SyncedTable{
		Name: t.TableName, RowCount: t.RowCount, SyncedAt: timestamppb.New(t.SyncedAt), SyncId: t.SyncID,
		Columns: d.GetColumns(), PrimaryKey: d.GetPrimaryKey(), ForeignKeys: d.GetForeignKeys(),
	}
	if t.ReconciledAt != nil {
		out.ReconciledAt = timestamppb.New(*t.ReconciledAt)
	}
	return out, nil
}
