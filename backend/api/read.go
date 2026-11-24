package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundryreadv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundryread/v1"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// ReadFoundryGraph reads a load of the ontology's database through
// foundry-read. Authz has checked the caller may read the database.
//  1. The reads must be 1 to 20 of the contract's kinds, and valid_at, when
//     set, a day not after today (UTC): every read but view and evidence
//     then fails current_only in its result, never read as of now instead.
//  2. The load must be the ontology's and not deleted (NotFound), nor being
//     deleted (FailedPrecondition): its marker no longer serves reads.
//  3. foundry-read evaluates the reads with the load's version's rules; its
//     statuses (the workspace's read limit, a paused database, a load the
//     database doesn't hold) are the caller's.
//  4. With workflow_run_id (the MCP's use_case_run_id), the reads and their
//     result are recorded under that open run of the ontology's use case (a
//     workflow in the RPCs' names; trackedRun, checked before the read).
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
	if day := req.GetValidAt(); day != "" {
		if _, err := ontologyagent.ParseValidAt(day, time.Now()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	run, err := s.trackedRun(ctx, o.WorkspaceID, o.ID, req.GetWorkflowRunId())
	if err != nil {
		return nil, err
	}
	if granted, ok := granteeLoad(ctx); ok && req.GetLoadId() != granted.LoadID {
		return nil, status.Error(codes.NotFound, "load not found")
	}
	load, out, err := s.readLoad(ctx, o, req)
	// A load Foundry moved in place whose marker moved before its row did:
	// put right from the recorded move, then read again, once.
	if stored, ok := loadMismatch(err); ok {
		switch reconciled, rerr := s.deps.DB.ReconcileFoundryLoadMarker(ctx, req.GetLoadId(), stored); {
		case rerr != nil:
			return nil, status.Errorf(codes.Internal, "reconcile the load's version: %v", rerr)
		case reconciled:
			load, out, err = s.readLoad(ctx, o, req)
		}
	}
	if err != nil {
		return nil, err
	}
	s.track(ctx, run, &foundrymodel.FoundryWorkflowRead{
		LoadID: load.ID, Reads: json.RawMessage(req.GetReadsJson()), Result: json.RawMessage(out.GetResultJson()),
	})
	return &foundryv1.ReadFoundryGraphResponse{ResultJson: out.GetResultJson()}, nil
}

// readLoad reads the request's load of o through foundry-read, at the
// version its row records: the load, and foundry-read's answer.
func (s *Service) readLoad(ctx context.Context, o *foundrymodel.FoundryOntology, req *foundryv1.ReadFoundryGraphRequest) (
	*foundrymodel.FoundryLoad, *foundryreadv1.ReadResponse, error) {
	load, err := s.deps.DB.GetFoundryLoad(ctx, o.ID, req.GetLoadId())
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && load.Status == foundrymodel.FoundryLoadDeleted:
		return nil, nil, status.Error(codes.NotFound, "load not found")
	case err != nil:
		return nil, nil, status.Errorf(codes.Internal, "get load: %v", err)
	case load.Status == foundrymodel.FoundryLoadDeleting || load.Status == foundrymodel.FoundryLoadDeleteFailed:
		return nil, nil, status.Error(codes.FailedPrecondition, "the load is being deleted")
	}
	v, err := s.loadOntologyVersion(ctx, o.ID, load.VersionID)
	if err != nil {
		return nil, nil, err
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(v.Definition, &def); err != nil || v.Format != foundrymodel.OntologyFormatAgentBundle {
		return nil, nil, status.Errorf(codes.Internal, "the load's version %s is not an ontology agent draft", v.ID)
	}
	read := &foundryreadv1.ReadRequest{
		WorkspaceId: o.WorkspaceID, OntologyId: o.ID, LoadId: load.ID, OntologyVersion: load.OntologyVersion(),
		OntologyToml: def.OntologyTOML, ReadsJson: req.GetReadsJson(), ValidAt: req.GetValidAt(),
	}
	if granted, ok := granteeLoad(ctx); ok && granted.Partial {
		g, _ := access.GranteeFrom(ctx)
		if read.VisibilityJson, err = s.granteeVisibility(ctx, g, load.ID); err != nil {
			return nil, nil, err
		}
		read.MappingsToml = def.MappingsTOML
	}
	out, err := s.deps.FoundryRead.Read(ctx, read)
	return load, out, err
}

// loadMismatchPrefix starts the message of a read the engine refused for a
// load its marker records at another version (agent-server's
// Refused::LoadMismatch).
const loadMismatchPrefix = "load_mismatch: the load was created with ontology version "

// loadMismatch is the version a read refused as load_mismatch names, if err
// is one.
func loadMismatch(err error) (string, bool) {
	st, ok := status.FromError(err)
	if !ok || err == nil {
		return "", false
	}
	_, version, found := strings.Cut(st.Message(), loadMismatchPrefix)
	return strings.TrimSpace(version), found && strings.TrimSpace(version) != ""
}
