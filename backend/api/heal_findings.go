package api

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// ListFoundryHealFindings is the ontology's shown self-healing findings
// (foundrymodel.FoundryHealShownLimit open ones, by support), its latest
// audit and its latest complete one, and whether this deployment audits
// loads at all
// (foundry.heal.enabled). It only reads what the foundry-heal-worker
// stored.
func (s *Service) ListFoundryHealFindings(ctx context.Context, req *foundryv1.ListFoundryHealFindingsRequest) (*foundryv1.ListFoundryHealFindingsResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	findings, err := s.deps.DB.ListFoundryHealFindings(ctx, o.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list heal findings: %v", err)
	}
	out := &foundryv1.ListFoundryHealFindingsResponse{Enabled: s.deps.Cfg.Foundry.Heal.Enabled}
	for _, f := range findings {
		out.Findings = append(out.Findings, foundryHealFindingProto(f))
	}
	for _, latest := range []struct {
		get func(context.Context, string) (*foundrymodel.FoundryHealRun, error)
		to  **foundryv1.FoundryHealRun
	}{{s.deps.DB.GetLatestFoundryHealRun, &out.LastRun}, {s.deps.DB.GetLatestCompleteFoundryHealRun, &out.LastCompleteRun}} {
		switch run, err := latest.get(ctx, o.ID); {
		case errors.Is(err, foundrystore.ErrNotFound):
		case err != nil:
			return nil, status.Errorf(codes.Internal, "get heal run: %v", err)
		default:
			*latest.to = &foundryv1.FoundryHealRun{
				Id: run.ID, LoadId: run.LoadID, Status: string(run.Status), Pages: int32(run.Pages), Cuts: int32(run.Cuts),
				RowsMeasured: run.RowsMeasured, StartedAt: timestamppb.New(run.CreatedAt),
			}
			if run.FinishedAt != nil {
				(*latest.to).FinishedAt = timestamppb.New(*run.FinishedAt)
			}
		}
	}
	return out, nil
}

// DismissFoundryHealFinding dismisses an open finding for the caller at
// its support now. The finding stays as it was dismissed, with the
// spellings it holds, until an audit finds it with twice that support or a
// spelling it doesn't hold (foundrymodel.FoundryHealFinding.Next). One
// already dismissed is returned as it is: dismissing is idempotent.
func (s *Service) DismissFoundryHealFinding(ctx context.Context, req *foundryv1.DismissFoundryHealFindingRequest) (*foundryv1.FoundryHealFindingResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetFingerprint() == "" {
		return nil, status.Error(codes.InvalidArgument, "fingerprint is required")
	}
	userID := actor(ctx).UserID
	if userID == "" {
		return nil, status.Error(codes.Unauthenticated, "a user is required to dismiss a finding")
	}
	f, err := s.deps.DB.GetFoundryHealFinding(ctx, o.ID, req.GetFingerprint())
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "finding not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get heal finding: %v", err)
	}
	if f.Status == foundrymodel.FoundryHealOpen {
		switch err := s.deps.DB.DismissFoundryHealFinding(ctx, o.ID, f.Fingerprint, userID); {
		case errors.Is(err, foundrystore.ErrNotFound):
			// It changed since: dismissed by someone else, or superseded.
		case err != nil:
			return nil, status.Errorf(codes.Internal, "dismiss heal finding: %v", err)
		}
		// Read again: a run may have stored a new support just before.
		if f, err = s.deps.DB.GetFoundryHealFinding(ctx, o.ID, f.Fingerprint); err != nil {
			return nil, status.Errorf(codes.Internal, "get heal finding: %v", err)
		}
	}
	if f.Status != foundrymodel.FoundryHealDismissed {
		return nil, status.Errorf(codes.FailedPrecondition, "the finding is %s, not open", f.Status)
	}
	return &foundryv1.FoundryHealFindingResponse{Finding: foundryHealFindingProto(f)}, nil
}

func foundryHealFindingProto(f *foundrymodel.FoundryHealFinding) *foundryv1.FoundryHealFinding {
	out := &foundryv1.FoundryHealFinding{
		Fingerprint: f.Fingerprint, Kind: f.Kind, Status: string(f.Status), Support: f.Support, Sources: f.Sources,
		LoadId: f.LoadID, BaseVersionId: f.BaseVersionID, FindingJson: string(f.Finding),
	}
	if f.Dismissal != nil {
		out.DismissedByUserId, out.SupportAtDismissal = &f.Dismissal.ByUserID, &f.Dismissal.Support
	}
	return out
}
