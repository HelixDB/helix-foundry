package api

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxFoundryOntologyNameChars = 120
	foundryPageSize             = 50
	maxClientTokenLength        = 64
	deletedOntologyToken        = "the ontology this client_token created was deleted; create a new one with a new client_token"
)

// CreateFoundryOntology binds a new ontology to one of the workspace's
// databases. The website lists them, or creates one, with the tenant routes;
// authz has checked the caller may write it.
//  1. Check the name and the client token; the same token returns the
//     ontology it made, provided it names the same database and is not
//     deleted (a deleted one's token is FailedPrecondition: tokens are never
//     reused, so a retry can't be told from a new create).
//  2. The tenant must be live, of a live project of this workspace.
//  3. Write the row. It has no document sets of its own: they are the
//     workspace's, and its versions read the ones a generation picks. A
//     database another live ontology names is AlreadyExists;
//     one a deleted ontology still holds while its loads are deleted from it
//     is FailedPrecondition, until they are. Asking for it again is how the
//     user retries those deletes that failed for good: they are queued again
//     (ResumeFoundryOntologyDrain), so the database is not held forever.
func (s *Service) CreateFoundryOntology(ctx context.Context, req *foundryv1.CreateFoundryOntologyRequest) (*foundryv1.FoundryOntologyResponse, error) {
	ws, name := req.GetWorkspaceId(), strings.TrimSpace(req.GetName())
	switch {
	case ws == "" || req.GetTenantId() == "":
		return nil, status.Error(codes.InvalidArgument, "workspace_id and tenant_id are required")
	case name == "" || utf8.RuneCountInString(name) > maxFoundryOntologyNameChars:
		return nil, status.Errorf(codes.InvalidArgument, "name must be 1 to %d characters", maxFoundryOntologyNameChars)
	case req.GetClientToken() == "" || len(req.GetClientToken()) > maxClientTokenLength:
		return nil, status.Errorf(codes.InvalidArgument, "client_token is required, at most %d characters", maxClientTokenLength)
	}
	existing, err := s.deps.DB.GetFoundryOntologyByClientToken(ctx, ws, req.GetClientToken())
	switch {
	case err == nil && existing.DeletedAt != nil:
		return nil, status.Error(codes.FailedPrecondition, deletedOntologyToken)
	case err == nil && existing.TenantID != req.GetTenantId():
		return nil, status.Error(codes.InvalidArgument, "client_token was already used for another database")
	case err == nil:
		return s.foundryOntologyResponse(ctx, existing)
	case !errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Errorf(codes.Internal, "look up ontology: %v", err)
	}
	if s.deps.Databases == nil {
		return nil, status.Error(codes.Unavailable, "databases are not available here")
	}
	// The database must be the workspace's before anything else of it is
	// told: whether its project can take new work (Live) is its owner's to know.
	switch database, err := s.deps.Databases.Get(ctx, req.GetTenantId()); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "tenant not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "get tenant: %v", err)
	case database.Deleted || database.Retired:
		return nil, status.Errorf(codes.FailedPrecondition, "tenant %s is deleted", database.TenantID)
	case database.WorkspaceID != ws:
		return nil, status.Error(codes.NotFound, "tenant not found")
	}
	tenant, err := s.deps.Databases.Live(ctx, req.GetTenantId())
	if err != nil {
		return nil, err
	}
	o := &foundrymodel.FoundryOntology{
		ID: id.New(), WorkspaceID: ws, Name: name, ProjectID: tenant.ProjectID, TenantID: tenant.TenantID, ClientToken: req.GetClientToken(),
	}
	if userID := actor(ctx).UserID; userID != "" {
		o.CreatedByUserID = &userID
	}
	err = s.deps.DB.CreateFoundryOntology(ctx, o)
	if errors.Is(err, foundrystore.ErrConflict) {
		// A retry of the same click won the race, or the database already has an ontology.
		raced, lookupErr := s.deps.DB.GetFoundryOntologyByClientToken(ctx, ws, req.GetClientToken())
		switch {
		case lookupErr == nil && raced.DeletedAt != nil:
			return nil, status.Error(codes.FailedPrecondition, deletedOntologyToken)
		case lookupErr == nil && raced.TenantID == tenant.TenantID:
			return s.foundryOntologyResponse(ctx, raced)
		}
		holder, holderErr := s.deps.DB.GetFoundryOntologyByTenant(ctx, tenant.TenantID)
		if holderErr == nil && holder.DeletedAt != nil {
			resumeErr := s.deps.DB.ResumeFoundryOntologyDrain(ctx, holder.WorkspaceID, holder.ID, id.New())
			if resumeErr != nil && !errors.Is(resumeErr, foundrystore.ErrNotFound) {
				return nil, status.Errorf(codes.Internal, "resume removing the previous ontology's loads: %v", resumeErr)
			}
			return nil, status.Error(codes.FailedPrecondition, "the previous ontology's loads are still being removed from this database; try again once they are")
		}
		return nil, status.Error(codes.AlreadyExists, "another ontology already uses this database; choose or create another")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create ontology: %v", err)
	}
	created, err := s.liveFoundryOntology(ctx, ws, o.ID)
	if err != nil {
		return nil, err
	}
	return s.foundryOntologyResponse(ctx, created)
}

func (s *Service) ListFoundryOntologies(ctx context.Context, req *foundryv1.ListFoundryOntologiesRequest) (*foundryv1.ListFoundryOntologiesResponse, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	ontologies, next, err := s.deps.DB.ListFoundryOntologies(ctx, req.GetWorkspaceId(), req.GetPageToken(), foundryPageSize)
	switch {
	case errors.Is(err, foundrystore.ErrInvalidPageToken):
		return nil, status.Error(codes.InvalidArgument, "page_token is not one this list returned")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "list ontologies: %v", err)
	}
	resp := &foundryv1.ListFoundryOntologiesResponse{NextPageToken: next}
	for _, o := range ontologies {
		out, err := s.foundryOntologyToProto(ctx, o)
		if err != nil {
			return nil, err
		}
		resp.Ontologies = append(resp.Ontologies, out)
	}
	return resp, nil
}

func (s *Service) GetFoundryOntology(ctx context.Context, req *foundryv1.GetFoundryOntologyRequest) (*foundryv1.FoundryOntologyResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	return s.foundryOntologyResponse(ctx, o)
}

// DeleteFoundryOntology deletes the ontology: its live job is cancelled, and
// delete jobs remove its loads from its database one by one, after which the
// database is free for another ontology. Deleting a load's nodes deletes
// the edges anyone attached to them too; the rest of the database, the
// user's own data included, is left as it is. Authz has checked the caller
// may write the database.
func (s *Service) DeleteFoundryOntology(ctx context.Context, req *foundryv1.DeleteFoundryOntologyRequest) (*foundryv1.FoundryOntologyResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	switch err := s.deps.DB.DeleteFoundryOntology(ctx, o.WorkspaceID, o.ID, id.New()); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "ontology not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "delete ontology: %v", err)
	}
	deleted, err := s.deps.DB.GetFoundryOntology(ctx, o.WorkspaceID, o.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get ontology: %v", err)
	}
	return s.foundryOntologyResponse(ctx, deleted)
}

// UpdateFoundryOntology turns refresh on or off; off forgets an owed refresh.
// Authz has checked the caller may write the database, which a refresh does.
func (s *Service) UpdateFoundryOntology(ctx context.Context, req *foundryv1.UpdateFoundryOntologyRequest) (*foundryv1.FoundryOntologyResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.AutoRefresh == nil {
		return nil, status.Error(codes.InvalidArgument, "set auto_refresh to update")
	}
	switch err := s.deps.DB.SetFoundryOntologyAutoRefresh(ctx, o.WorkspaceID, o.ID, req.GetAutoRefresh()); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "ontology not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "update ontology: %v", err)
	}
	updated, err := s.liveFoundryOntology(ctx, o.WorkspaceID, o.ID)
	if err != nil {
		return nil, err
	}
	return s.foundryOntologyResponse(ctx, updated)
}

// liveFoundryOntology is the workspace's ontology unless it is missing or deleted.
func (s *Service) liveFoundryOntology(ctx context.Context, workspaceID, ontologyID string) (*foundrymodel.FoundryOntology, error) {
	if workspaceID == "" || ontologyID == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and ontology_id are required")
	}
	o, err := s.deps.DB.GetFoundryOntology(ctx, workspaceID, ontologyID)
	if errors.Is(err, foundrystore.ErrNotFound) || err == nil && o.DeletedAt != nil {
		return nil, status.Error(codes.NotFound, "ontology not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get ontology: %v", err)
	}
	return o, nil
}

func (s *Service) foundryOntologyResponse(ctx context.Context, o *foundrymodel.FoundryOntology) (*foundryv1.FoundryOntologyResponse, error) {
	out, err := s.foundryOntologyToProto(ctx, o)
	if err != nil {
		return nil, err
	}
	return &foundryv1.FoundryOntologyResponse{Ontology: out}, nil
}

// foundryOntologyToProto adds the database's name, the newest load job, the
// current load, the live job and the document count: the uploaded documents
// its reading version (foundryReader) has in scope. A database deleted since
// has no name.
func (s *Service) foundryOntologyToProto(ctx context.Context, o *foundrymodel.FoundryOntology) (*foundryv1.FoundryOntology, error) {
	out := &foundryv1.FoundryOntology{
		Id: o.ID, Name: o.Name, ProjectId: o.ProjectID, TenantId: o.TenantID, CurrentVersionId: o.CurrentVersionID,
		CreatedAt: timestamppb.New(o.CreatedAt), AutoRefresh: o.AutoRefresh,
	}
	if o.RefreshDueAt != nil {
		out.RefreshDueAt = timestamppb.New(*o.RefreshDueAt)
	}
	if s.deps.Databases != nil {
		switch database, err := s.deps.Databases.Get(ctx, o.TenantID); {
		case err == nil && !database.Deleted:
			out.DatabaseName = database.Name
		case err != nil && !errors.Is(err, foundrystore.ErrNotFound):
			return nil, status.Errorf(codes.Internal, "get tenant: %v", err)
		}
	}
	latest, err := s.deps.DB.GetLatestFoundryGraphJob(ctx, o.ID, foundrymodel.FoundryGraphJobLoad)
	switch {
	case err == nil:
		out.LastLoad = &foundryv1.FoundryOntologyLastLoad{JobId: latest.ID, Status: string(latest.Status)}
		if latest.FinishedAt != nil {
			out.LastLoad.FinishedAt = timestamppb.New(*latest.FinishedAt)
		}
	case !errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Errorf(codes.Internal, "get latest load: %v", err)
	}
	if o.CurrentLoadID != nil {
		current, err := s.deps.DB.GetFoundryLoad(ctx, o.ID, *o.CurrentLoadID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "get current load: %v", err)
		}
		out.CurrentLoad = &foundryv1.FoundryOntologyCurrentLoad{Id: current.ID, Version: current.OntologyVersion()}
		if current.FinishedAt != nil {
			out.CurrentLoad.FinishedAt = timestamppb.New(*current.FinishedAt)
		}
	}
	active, err := s.deps.DB.GetActiveFoundryGraphJob(ctx, o.ID)
	switch {
	case err == nil:
		out.ActiveJob = &foundryv1.FoundryOntologyActiveJob{
			Kind: string(active.Kind), LoadId: active.LoadID, Phase: string(active.Phase), Progress: foundryProgressToProto(active.Progress),
		}
	case !errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Errorf(codes.Internal, "get active job: %v", err)
	}
	// A due refresh waits only for the live job: a spent month holds none back.
	if active != nil && o.RefreshDueAt != nil && !o.RefreshDueAt.After(time.Now()) {
		out.RefreshWaiting = "busy"
	}
	reader, err := s.foundryReaderOf(ctx, o)
	if err != nil {
		return nil, err
	}
	documents, err := s.deps.DB.CountFoundryDocumentsInSets(ctx, o.WorkspaceID, reader.scope)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "count documents: %v", err)
	}
	out.Counts = &foundryv1.FoundryOntologyCounts{Documents: int32(documents)}
	return out, nil
}
