package api

import (
	"context"
	"errors"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// People enrichment's settings and a load's Stop. An owner allows lookups
// and caps the month's credits; Helix's flag (foundry.enrichment.enabled)
// and Foundry Pro gate new lookups; loads apply the profiles found before
// whatever the gates say, unless consent is off.

// maxNoticeVersion is the longest notice version a consent row keeps.
const maxNoticeVersion = 32

// errEnrichmentUnavailable is Helix's flag off.
var errEnrichmentUnavailable = status.Error(codes.FailedPrecondition, "People enrichment isn't available")

// GetFoundryEnrichment returns the workspace's People enrichment setting
// and month. Authz has checked the caller may read the workspace.
func (s *Service) GetFoundryEnrichment(ctx context.Context, req *foundryv1.GetFoundryEnrichmentRequest) (*foundryv1.FoundryEnrichmentResponse, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	return s.foundryEnrichmentResponse(ctx, req.GetWorkspaceId())
}

// UpdateFoundryEnrichment records an owner's change of the setting, as
// history with the notice they were shown. Turning lookups on needs Helix's
// flag, Foundry Pro and a cap within the most an owner may set; turning them
// off waits on none of these, a cap above a maximum lowered since included:
// it cancels the workspace's running lookups, purges what they stored, and
// queues the clean loads (provider SetFoundryEnrichmentConsent). Authz has
// checked the caller is an owner.
func (s *Service) UpdateFoundryEnrichment(ctx context.Context, req *foundryv1.UpdateFoundryEnrichmentRequest) (*foundryv1.FoundryEnrichmentResponse, error) {
	cfg := s.deps.Cfg.Foundry.Enrichment
	ws, userID := req.GetWorkspaceId(), actor(ctx).UserID
	switch {
	case ws == "":
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	case req.GetNoticeVersion() == "" || len(req.GetNoticeVersion()) > maxNoticeVersion:
		return nil, status.Errorf(codes.InvalidArgument, "notice_version is required, at most %d characters", maxNoticeVersion)
	case req.GetMonthlyCreditCap() < 0 || req.GetEnabled() && req.GetMonthlyCreditCap() > cfg.MonthlyCreditsMax:
		return nil, status.Errorf(codes.InvalidArgument, "monthly_credit_cap must be 0 to %d", cfg.MonthlyCreditsMax)
	case userID == "":
		return nil, status.Error(codes.PermissionDenied, "only a signed-in owner can change People enrichment")
	}
	if req.GetEnabled() {
		if !cfg.Enabled {
			return nil, errEnrichmentUnavailable
		}
		plan, err := s.workspacePlan(ctx, ws)
		if err != nil {
			return nil, err
		}
		if !plan.Sold() {
			return nil, billingPrecondition(ws, "FOUNDRY_PRO_REQUIRED", "People enrichment needs Foundry Pro")
		}
	}
	if err := s.deps.DB.SetFoundryEnrichmentConsent(ctx, &foundrymodel.FoundryEnrichmentConsent{
		ID: id.New(), WorkspaceID: ws, Enabled: req.GetEnabled(), MonthlyCreditCap: req.GetMonthlyCreditCap(),
		NoticeVersion: req.GetNoticeVersion(), UserID: userID,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "set people enrichment: %v", err)
	}
	return s.foundryEnrichmentResponse(ctx, ws)
}

// StopFoundryLoadEnrichment stops a load's lookups: a queued step ends at
// once and wakes its load, a running one settles its batch and publishes
// first; the load then applies what was found. Authz has checked the caller
// may manage the workspace's ontologies.
func (s *Service) StopFoundryLoadEnrichment(ctx context.Context, req *foundryv1.StopFoundryLoadEnrichmentRequest) (*foundryv1.FoundryLoadResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	userID := actor(ctx).UserID
	switch {
	case req.GetId() == "":
		return nil, status.Error(codes.InvalidArgument, "id is required")
	case userID == "":
		return nil, status.Error(codes.PermissionDenied, "only a signed-in user can stop a load's lookups")
	}
	switch err := s.deps.DB.StopFoundryLoadEnrichment(ctx, o.WorkspaceID, o.ID, req.GetId(), userID); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "this load looks no one up")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "stop the lookups: %v", err)
	}
	return s.foundryLoadResponse(ctx, o, req.GetId())
}

// foundryEnrichmentResponse reads what the settings show. Only availability
// needs the billing system: when it can't say whether the workspace is on Pro,
// availability is UNSPECIFIED and the rest, all MySQL's, is shown as ever.
// So the page still loads and an owner can still turn lookups off, and a
// change already saved (turning them off needs no Pro) is never reported as
// failed.
func (s *Service) foundryEnrichmentResponse(ctx context.Context, workspaceID string) (*foundryv1.FoundryEnrichmentResponse, error) {
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	settings, err := s.deps.DB.GetFoundryEnrichmentSettings(ctx, workspaceID, month)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read people enrichment: %v", err)
	}
	availability := foundryv1.FoundryEnrichmentAvailability_FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED
	if s.deps.Cfg.Foundry.Enrichment.Enabled {
		switch plan, err := s.workspacePlan(ctx, workspaceID); {
		case err != nil:
			log.From(ctx).Warn("people enrichment settings: foundry plan unreadable; availability unspecified", "workspace", workspaceID, "err", err)
			availability = foundryv1.FoundryEnrichmentAvailability_FOUNDRY_ENRICHMENT_AVAILABILITY_UNSPECIFIED
		case plan.Sold():
			availability = foundryv1.FoundryEnrichmentAvailability_FOUNDRY_ENRICHMENT_AVAILABILITY_AVAILABLE
		default:
			availability = foundryv1.FoundryEnrichmentAvailability_FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED
		}
	}
	out := &foundryv1.FoundryEnrichment{
		Availability: availability, CreditsUsed: settings.Ledger.WorkspaceCharged, CreditsReserved: settings.Ledger.WorkspaceReserved,
		CapResetsAt: timestamppb.New(month.AddDate(0, 1, 0)), PurgesPending: int32(settings.PurgesPending),
	}
	if c := settings.Consent; c != nil {
		out.Enabled, out.MonthlyCreditCap = c.Enabled, c.MonthlyCreditCap
	}
	if c := settings.Enabled; c != nil {
		out.EnabledBy, out.EnabledAt = c.UserID, timestamppb.New(c.CreatedAt)
	}
	return &foundryv1.FoundryEnrichmentResponse{Enrichment: out}, nil
}

// foundryLoadEnrichmentToProto is a load's step as FoundryLoad.enrichment
// shows it.
func foundryLoadEnrichmentToProto(e *foundrymodel.FoundryLoadEnrichment) *foundryv1.FoundryLoadEnrichment {
	c := e.Counts
	out := &foundryv1.FoundryLoadEnrichment{
		Status: string(e.Status), People: c.People, NoName: c.NoName, NoClue: c.NoClue, Shared: c.Shared, Skipped: c.Skipped,
		Carried: c.Carried, ToLookUp: c.ToLookUp, LookedUp: c.LookedUp, Found: c.Found, Held: c.Held, NotFound: c.NotFound,
		Uncertain: c.Uncertain, Credits: c.Credits, Applied: c.Applied, Unmatched: c.Unmatched,
	}
	if e.StoppedBy != nil {
		why := string(*e.StoppedBy)
		out.StoppedBy = &why
	}
	if e.PublishedAt != nil {
		out.PublishedAt = timestamppb.New(*e.PublishedAt)
	}
	return out
}
