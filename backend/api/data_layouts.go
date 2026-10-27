package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// maxDataLayoutBytes bounds a workspace's Data layout, as compact JSON.
const maxDataLayoutBytes = 64 << 10

// GetFoundryDataLayout returns how the workspace's Data tree is arranged:
// the website's JSON object and its revision, {} and 0 before the first
// write.
func (s *Service) GetFoundryDataLayout(ctx context.Context, req *foundryv1.GetFoundryDataLayoutRequest) (*foundryv1.FoundryDataLayoutResponse, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	l, err := s.deps.DB.GetFoundryDataLayout(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get data layout: %v", err)
	}
	var layout structpb.Struct
	if err := protojson.Unmarshal(l.Layout, &layout); err != nil {
		return nil, status.Errorf(codes.Internal, "read data layout: %v", err)
	}
	return &foundryv1.FoundryDataLayoutResponse{Layout: &layout, Revision: l.Revision}, nil
}

// PutFoundryDataLayout stores the layout over the one the caller read at
// expected_revision. The backend keeps it as it is, beyond requiring a JSON
// object of at most 64 KiB as compact JSON (no space outside strings), which is
// what it stores. A stale revision is FailedPrecondition: another
// write came first (a first write at 0 included), so the website reads the
// layout again and reapplies its change.
func (s *Service) PutFoundryDataLayout(ctx context.Context, req *foundryv1.PutFoundryDataLayoutRequest) (*foundryv1.FoundryDataLayoutResponse, error) {
	switch {
	case req.GetWorkspaceId() == "":
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	case req.GetLayout() == nil:
		return nil, status.Error(codes.InvalidArgument, "layout is required: a JSON object")
	case req.GetExpectedRevision() < 0:
		return nil, status.Error(codes.InvalidArgument, "expected_revision is the revision read, 0 or more")
	}
	marshalled, err := protojson.Marshal(req.GetLayout())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "layout is not valid JSON: %v", err)
	}
	// protojson puts a space after a comma on some builds and not others
	// (its output is deliberately unstable), so the limit is measured on, and
	// the store keeps, the compact form: the same bytes on every deploy.
	// Not encoding/json's Marshal, which would escape <, > and &.
	var raw bytes.Buffer
	if err := json.Compact(&raw, marshalled); err != nil {
		return nil, status.Errorf(codes.Internal, "compact layout: %v", err)
	}
	if raw.Len() > maxDataLayoutBytes {
		return nil, status.Errorf(codes.InvalidArgument, "a layout holds up to %d KiB; this one is %d bytes", maxDataLayoutBytes>>10, raw.Len())
	}
	l := &foundrymodel.FoundryDataLayout{WorkspaceID: req.GetWorkspaceId(), Layout: raw.Bytes()}
	if userID := actor(ctx).UserID; userID != "" {
		l.UpdatedByUserID = &userID
	}
	switch err := s.deps.DB.PutFoundryDataLayout(ctx, l, req.GetExpectedRevision()); {
	case errors.Is(err, foundrystore.ErrConflict):
		return nil, status.Error(codes.FailedPrecondition, "the layout changed since you read it; read it again")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "put data layout: %v", err)
	}
	return &foundryv1.FoundryDataLayoutResponse{Layout: req.GetLayout(), Revision: l.Revision}, nil
}
