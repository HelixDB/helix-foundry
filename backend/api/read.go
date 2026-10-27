package api

import (
	"context"
	"encoding/json"
	"errors"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundryreadv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundryread/v1"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// ReadFoundryGraph reads a load of the ontology's database through
// foundry-read. Authz has checked the caller may read the database.
//  1. The reads must be 1 to 20 of the contract's kinds.
//  2. The load must be the ontology's and not deleted (NotFound), nor being
//     deleted (FailedPrecondition): its marker no longer serves reads.
//  3. foundry-read evaluates the reads with the load's version's rules; its
//     statuses (the workspace's read limit, a paused database, a load the
//     database doesn't hold) are the caller's.
func (s *Service) ReadFoundryGraph(ctx context.Context, req *foundryv1.ReadFoundryGraphRequest) (*foundryv1.ReadFoundryGraphResponse, error) {
	if s.deps.FoundryRead == nil {
		return nil, status.Error(codes.Unavailable, "graph reads are not available here")
	}
	if req.GetLoadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "load_id is required")
	}
	if _, err := ontologyagent.ParseReads(req.GetReadsJson()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	load, err := s.deps.DB.GetFoundryLoad(ctx, o.ID, req.GetLoadId())
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && load.Status == foundrymodel.FoundryLoadDeleted:
		return nil, status.Error(codes.NotFound, "load not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "get load: %v", err)
	case load.Status == foundrymodel.FoundryLoadDeleting || load.Status == foundrymodel.FoundryLoadDeleteFailed:
		return nil, status.Error(codes.FailedPrecondition, "the load is being deleted")
	}
	v, err := s.loadOntologyVersion(ctx, o.ID, load.VersionID)
	if err != nil {
		return nil, err
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(v.Definition, &def); err != nil || v.Format != foundrymodel.OntologyFormatAgentBundle {
		return nil, status.Errorf(codes.Internal, "the load's version %s is not an ontology agent draft", v.ID)
	}
	out, err := s.deps.FoundryRead.Read(ctx, &foundryreadv1.ReadRequest{
		WorkspaceId: o.WorkspaceID, OntologyId: o.ID, LoadId: load.ID, OntologyVersion: load.OntologyVersion(),
		OntologyToml: def.OntologyTOML, ReadsJson: req.GetReadsJson(),
	})
	if err != nil {
		return nil, err
	}
	return &foundryv1.ReadFoundryGraphResponse{ResultJson: out.GetResultJson()}, nil
}
