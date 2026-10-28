package api

import (
	"context"
	"errors"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// CreateFoundryLoad queues a new load of the ontology's database: the
// foundry-graph-worker ingests the version's tables through its mappings
// into a load of its own, beside the loads already there, then extracts the
// ontology's documents into it. Authz has checked the caller may write the
// database.
//  1. The same client token returns the load it queued.
//  2. The version must be one ontologyagent.LoadRefusal accepts: one of the
//     ontology's agent drafts, finished (done) and drafted by a generation,
//     whose tables the load reads.
//  3. The workspace's month must have budget left.
//  4. A live delete gives way to it; a live load or extract is
//     FailedPrecondition.
func (s *Service) CreateFoundryLoad(ctx context.Context, req *foundryv1.CreateFoundryLoadRequest) (*foundryv1.FoundryLoadResponse, error) {
	if req.GetClientToken() == "" || len(req.GetClientToken()) > maxClientTokenLength {
		return nil, status.Errorf(codes.InvalidArgument, "client_token is required, at most %d characters", maxClientTokenLength)
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	existing, err := s.deps.DB.GetFoundryGraphJobByClientToken(ctx, o.ID, req.GetClientToken())
	switch {
	case err == nil && existing.VersionID != req.GetVersionId():
		return nil, status.Error(codes.InvalidArgument, "client_token was already used for another version")
	case err == nil:
		return s.foundryLoadResponse(ctx, o, existing.LoadID)
	case !errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Errorf(codes.Internal, "look up load: %v", err)
	}
	v, err := s.loadOntologyVersion(ctx, o.ID, req.GetVersionId())
	if err != nil {
		return nil, err
	}
	refusal, err := ontologyagent.LoadRefusal(v)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read ontology version %s: %v", v.ID, err)
	}
	if refusal != "" {
		return nil, status.Error(codes.FailedPrecondition, refusal)
	}
	budget := s.deps.Cfg.Foundry.Graph.MonthBudget(time.Now())
	spent, err := s.deps.DB.SumFoundryGraphJobSpend(ctx, o.WorkspaceID, budget.Since)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sum graph spend: %v", err)
	}
	if spent >= budget.CapMicros {
		return nil, status.Errorf(codes.FailedPrecondition, "the workspace has spent this month's $%.2f of loads", float64(budget.CapMicros)/1e6)
	}
	token := req.GetClientToken()
	loadID := id.New()
	job := &foundrymodel.FoundryGraphJob{
		ID: loadID, WorkspaceID: o.WorkspaceID, OntologyID: o.ID, VersionID: v.ID, LoadID: loadID, Kind: foundrymodel.FoundryGraphJobLoad,
		Phase: foundrymodel.FoundryGraphJobProvisioning, ClientToken: &token,
	}
	if userID := actor(ctx).UserID; userID != "" {
		job.RequestedByUserID = &userID
	}
	switch err := s.deps.DB.CreateFoundryLoad(ctx, job); {
	case errors.Is(err, foundrystore.ErrConflict):
		// The same click again won the race, or a load or extract is live.
		raced, lookupErr := s.deps.DB.GetFoundryGraphJobByClientToken(ctx, o.ID, token)
		if lookupErr == nil && raced.VersionID == v.ID {
			return s.foundryLoadResponse(ctx, o, raced.LoadID)
		}
		return nil, status.Error(codes.FailedPrecondition, "a load or extract of this ontology is already queued or running")
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "ontology not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "queue load: %v", err)
	}
	return s.foundryLoadResponse(ctx, o, loadID)
}

// foundryLoadJobsShown is how many of a load's newest jobs it shows; its job
// count and spend cover them all.
const foundryLoadJobsShown = 20

// ListFoundryLoads returns the ontology's loads not yet deleted, newest
// first, each with its newest jobs. Retention keeps the list short.
func (s *Service) ListFoundryLoads(ctx context.Context, req *foundryv1.ListFoundryLoadsRequest) (*foundryv1.ListFoundryLoadsResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	loads, err := s.deps.DB.ListFoundryLoads(ctx, o.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list loads: %v", err)
	}
	ids := make([]string, 0, len(loads))
	for _, l := range loads {
		ids = append(ids, l.ID)
	}
	jobs, err := s.deps.DB.ListFoundryLoadJobs(ctx, ids, foundryLoadJobsShown)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list load jobs: %v", err)
	}
	steps, err := s.deps.DB.ListFoundryLoadEnrichments(ctx, ids)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list load enrichments: %v", err)
	}
	sources, err := s.deps.DB.ListFoundryLoadSources(ctx, ids)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list load sources: %v", err)
	}
	resp := &foundryv1.ListFoundryLoadsResponse{}
	for _, l := range loads {
		resp.Loads = append(resp.Loads, foundryLoadToProto(l, o, jobs[l.ID], steps[l.ID], sources[l.ID]))
	}
	return resp, nil
}

// GetFoundryLoad returns one load with its newest jobs; a deleted load is NotFound.
func (s *Service) GetFoundryLoad(ctx context.Context, req *foundryv1.GetFoundryLoadRequest) (*foundryv1.FoundryLoadResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	return s.foundryLoadResponse(ctx, o, req.GetId())
}

// SetCurrentFoundryLoad points the ontology's reads and uploads at a ready or
// partial load, but not one that applied People enrichment profiles consent
// was since withdrawn for (FailedPrecondition). A document it lacks
// (uploaded since it was written, or that failed in it retryably) is
// extracted into it: at once within the month's budget, or by the live
// job's end. Authz has checked the caller may write the database.
func (s *Service) SetCurrentFoundryLoad(ctx context.Context, req *foundryv1.SetCurrentFoundryLoadRequest) (*foundryv1.FoundryLoadResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	extract := &foundrymodel.FoundryGraphJob{ID: id.New()}
	if userID := actor(ctx).UserID; userID != "" {
		extract.RequestedByUserID = &userID
	}
	if _, err := s.deps.DB.SetCurrentFoundryLoad(ctx, o.WorkspaceID, o.ID, req.GetId(), extract, s.deps.Cfg.Foundry.Graph.MonthBudget(time.Now())); err != nil {
		return nil, foundryLoadError("set the current load", err)
	}
	o, err = s.liveFoundryOntology(ctx, o.WorkspaceID, o.ID)
	if err != nil {
		return nil, err
	}
	return s.foundryLoadResponse(ctx, o, req.GetId())
}

// DeleteFoundryLoad marks a load deleting; a delete job removes it from the
// database, its nodes taking every edge attached to them, then the load is
// gone from the list. The current load, and one a job writes, can't be
// deleted. Authz has checked the caller may write the database.
func (s *Service) DeleteFoundryLoad(ctx context.Context, req *foundryv1.DeleteFoundryLoadRequest) (*foundryv1.FoundryLoadResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if err := s.deps.DB.DeleteFoundryLoad(ctx, o.WorkspaceID, o.ID, req.GetId(), id.New()); err != nil {
		return nil, foundryLoadError("delete the load", err)
	}
	return s.foundryLoadResponse(ctx, o, req.GetId())
}

// foundryLoadError maps a refused change of a load to its status.
func foundryLoadError(what string, err error) error {
	var refusal foundrystore.FoundryLoadRefusal
	switch {
	case errors.As(err, &refusal):
		return status.Error(codes.FailedPrecondition, refusal.Error())
	case errors.Is(err, foundrystore.ErrNotFound):
		return status.Error(codes.NotFound, "load not found")
	}
	return status.Errorf(codes.Internal, "%s: %v", what, err)
}

// foundryLoadResponse is one of o's loads with its newest jobs; a deleted load is NotFound.
func (s *Service) foundryLoadResponse(ctx context.Context, o *foundrymodel.FoundryOntology, loadID string) (*foundryv1.FoundryLoadResponse, error) {
	load, err := s.deps.DB.GetFoundryLoad(ctx, o.ID, loadID)
	if errors.Is(err, foundrystore.ErrNotFound) || err == nil && load.Status == foundrymodel.FoundryLoadDeleted {
		return nil, status.Error(codes.NotFound, "load not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get load: %v", err)
	}
	jobs, err := s.deps.DB.ListFoundryLoadJobs(ctx, []string{load.ID}, foundryLoadJobsShown)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list load jobs: %v", err)
	}
	steps, err := s.deps.DB.ListFoundryLoadEnrichments(ctx, []string{load.ID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list load enrichments: %v", err)
	}
	sources, err := s.deps.DB.ListFoundryLoadSources(ctx, []string{load.ID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list load sources: %v", err)
	}
	return &foundryv1.FoundryLoadResponse{Load: foundryLoadToProto(load, o, jobs[load.ID], steps[load.ID], sources[load.ID])}, nil
}

// foundryLoadToProto is a load with its newest jobs, the snapshots it holds
// and, when its version enriches people, its step (nil otherwise).
func foundryLoadToProto(l *foundrymodel.FoundryLoad, o *foundrymodel.FoundryOntology, jobs foundrymodel.FoundryLoadJobs, step *foundrymodel.FoundryLoadEnrichment,
	sources []foundrymodel.FoundryLoadSource) *foundryv1.FoundryLoad {
	out := &foundryv1.FoundryLoad{
		Id: l.ID, VersionId: l.VersionID, Version: l.OntologyVersion(), Status: string(l.Status),
		Current: o.CurrentLoadID != nil && *o.CurrentLoadID == l.ID, CreatedAt: timestamppb.New(l.CreatedAt),
		JobCount: int32(jobs.Count), UsdMicros: jobs.USDMicros, IngestViolations: l.IngestViolations, Origin: string(l.Origin),
	}
	if l.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*l.FinishedAt)
	}
	if l.AppliedAt != nil {
		out.AppliedAt = timestamppb.New(*l.AppliedAt)
	}
	for _, src := range sources {
		s := &foundryv1.FoundryLoadSource{ConnectorId: src.ConnectorID, Table: src.Table, SyncId: src.SyncID}
		if !src.SyncedAt.IsZero() {
			s.SyncedAt = timestamppb.New(src.SyncedAt)
		}
		out.Sources = append(out.Sources, s)
	}
	for _, job := range jobs.Newest {
		out.Jobs = append(out.Jobs, foundryJobToProto(job))
	}
	if step != nil {
		out.Enrichment = foundryLoadEnrichmentToProto(step)
	}
	return out
}

func foundryJobToProto(job *foundrymodel.FoundryGraphJob) *foundryv1.FoundryGraphJob {
	out := &foundryv1.FoundryGraphJob{
		Id: job.ID, VersionId: job.VersionID, LoadId: job.LoadID, Status: string(job.Status), Phase: string(job.Phase),
		Kind: string(job.Kind), Progress: foundryProgressToProto(job.Progress), UsdMicros: job.Spend.USDMicros, Error: job.Error,
		CreatedAt: timestamppb.New(job.CreatedAt),
	}
	if job.StartedAt != nil {
		out.StartedAt = timestamppb.New(*job.StartedAt)
	}
	if job.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*job.FinishedAt)
	}
	return out
}

func foundryProgressToProto(p foundrymodel.FoundryGraphJobProgress) *foundryv1.FoundryGraphJobProgress {
	return &foundryv1.FoundryGraphJobProgress{
		ChunksDone: int32(p.ChunksDone), DocumentsDone: int32(p.DocumentsDone), DocumentsTotal: int32(p.DocumentsTotal),
	}
}
