package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryconfig "github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/ports"
)

func (s *Service) GetFoundryPlan(ctx context.Context, req *foundryv1.GetFoundryPlanRequest) (*foundryv1.FoundryPlanResponse, error) {
	plans, err := s.foundryPlans(req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	current, err := plans.Status(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, foundryPlanError("read foundry plan", err)
	}
	return &foundryv1.FoundryPlanResponse{Plan: foundryPlanToProto(current)}, nil
}

// checkoutCodes are the catalog codes the tier enum names, for callers that
// predate plan_code; an unset tier buys Pro.
var checkoutCodes = map[foundryv1.FoundryTier]string{
	foundryv1.FoundryTier_FOUNDRY_TIER_UNSPECIFIED: "pro",
	foundryv1.FoundryTier_FOUNDRY_TIER_PRO:         "pro",
	foundryv1.FoundryTier_FOUNDRY_TIER_MAX:         "max",
}

func (s *Service) CreateFoundryProCheckout(ctx context.Context, req *foundryv1.CreateFoundryProCheckoutRequest) (
	*foundryv1.CreateFoundryProCheckoutResponse, error) {
	plans, err := s.foundryPlans(req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	if req.GetClientToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "client_token is required")
	}
	code := req.GetPlanCode()
	if code == "" {
		var ok bool
		if code, ok = checkoutCodes[req.GetTier()]; !ok {
			return nil, status.Errorf(codes.InvalidArgument, "tier %s cannot be bought", req.GetTier())
		}
	}
	out, err := plans.Checkout(ctx, req.GetWorkspaceId(), code, strings.TrimSpace(req.GetPromoCode()), req.GetClientToken())
	if err != nil {
		return nil, foundryPlanError("start foundry checkout", err)
	}
	return &foundryv1.CreateFoundryProCheckoutResponse{
		CheckoutUrl: out.CheckoutURL,
		Plan:        foundryPlanToProto(out.Status),
	}, nil
}

// ListFoundryPlans is the catalog, Free first; it answers on a deployment that sells nothing.
func (s *Service) ListFoundryPlans(_ context.Context, req *foundryv1.ListFoundryPlansRequest) (*foundryv1.ListFoundryPlansResponse, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	resp := &foundryv1.ListFoundryPlansResponse{}
	for _, p := range s.foundryCatalog().Plans() {
		resp.Plans = append(resp.Plans, &foundryv1.FoundryCatalogPlan{
			Code: p.Code, DisplayName: p.DisplayName, Sold: p.Sold() && s.deps.Plans != nil,
			Kinds: p.Kinds, Sync: syncRulesToProto(p.Sync),
		})
	}
	return resp, nil
}

func (s *Service) CancelFoundryPro(ctx context.Context, req *foundryv1.CancelFoundryProRequest) (*foundryv1.FoundryPlanResponse, error) {
	plans, err := s.foundryPlans(req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	current, err := plans.Cancel(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, foundryPlanError("cancel foundry plan", err)
	}
	return &foundryv1.FoundryPlanResponse{Plan: foundryPlanToProto(current)}, nil
}

func (s *Service) ResumeFoundryPro(ctx context.Context, req *foundryv1.ResumeFoundryProRequest) (*foundryv1.FoundryPlanResponse, error) {
	plans, err := s.foundryPlans(req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	current, err := plans.Resume(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, foundryPlanError("resume foundry plan", err)
	}
	return &foundryv1.FoundryPlanResponse{Plan: foundryPlanToProto(current)}, nil
}

func (s *Service) foundryPlans(workspaceID string) (ports.Plans, error) {
	if workspaceID == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	if s.deps.Plans == nil {
		return nil, status.Error(codes.Unavailable, "foundry billing is not configured")
	}
	return s.deps.Plans, nil
}

// foundryCatalog is the deployment's plan catalog; a service built without one
// (tests) gets the default catalog.
func (s *Service) foundryCatalog() *foundryplan.Catalog {
	if s.deps.Catalog != nil {
		return s.deps.Catalog
	}
	c, err := foundryplan.NewCatalog(foundryconfig.DefaultPlans(), nil)
	if err != nil {
		panic(err) // the default catalog is valid by construction
	}
	return c
}

// workspacePlan is the workspace's plan, Free on a deployment that sells none.
func (s *Service) workspacePlan(ctx context.Context, workspaceID string) (*foundryplan.Plan, error) {
	if s.deps.Plans == nil {
		return s.foundryCatalog().Free(), nil
	}
	current, err := s.deps.Plans.Status(ctx, workspaceID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read foundry plan: %v", err)
	}
	return current.Plan, nil
}

// requireKind refuses a kind the workspace's plan doesn't include, naming the
// lowest plan that does.
func (s *Service) requireKind(ctx context.Context, workspaceID string, kind connector.Kind) error {
	var err error
	if s.deps.Plans == nil {
		if !s.foundryCatalog().Free().Includes(kind.Name()) {
			err = &foundryplan.PlanRequiredError{Kind: kind.Name(), Plan: s.foundryCatalog().LowestWith(kind.Name())}
		}
	} else {
		err = s.deps.Plans.RequireKind(ctx, workspaceID, kind.Name())
	}
	var required *foundryplan.PlanRequiredError
	switch {
	case errors.As(err, &required):
		name := "a plan this installation does not sell"
		if required.Plan != nil {
			name = "Foundry " + required.Plan.DisplayName
		}
		return billingPrecondition(workspaceID, "FOUNDRY_PRO_REQUIRED", fmt.Sprintf("%s connectors need %s", kind.DisplayName(), name))
	case err != nil:
		return status.Errorf(codes.Unavailable, "read foundry plan: %v", err)
	}
	return nil
}

// requireLogCapture refuses database settings that read the log (CDC) on a
// plan whose capture methods leave it out, naming the lowest plan with it.
func (s *Service) requireLogCapture(ctx context.Context, workspaceID string, settings json.RawMessage) error {
	var d connector.DatabaseSettings
	if json.Unmarshal(settings, &d) != nil || d.CDC == nil {
		return nil
	}
	plan, err := s.workspacePlan(ctx, workspaceID)
	if err != nil {
		return err
	}
	if plan.Sync.Allows(foundryplan.CaptureLog) {
		return nil
	}
	name := "a plan this installation does not sell"
	for _, p := range s.foundryCatalog().Plans() {
		if p.Sync.Allows(foundryplan.CaptureLog) {
			name = "Foundry " + p.DisplayName
			break
		}
	}
	return billingPrecondition(workspaceID, "FOUNDRY_PRO_REQUIRED", "change data capture needs "+name)
}

func foundryPlanError(op string, err error) error {
	switch {
	case errors.Is(err, foundryplan.ErrNotPaid):
		return status.Error(codes.FailedPrecondition, "the workspace is not on a paid Foundry plan")
	case errors.Is(err, foundryplan.ErrPlanNotSold):
		return status.Error(codes.FailedPrecondition, "this Foundry plan is not sold")
	case errors.Is(err, foundryplan.ErrPromotionCode):
		return status.Error(codes.InvalidArgument, "the promotion code is invalid or does not apply to this Foundry plan")
	case errors.Is(err, foundryplan.ErrDeclined):
		return status.Error(codes.FailedPrecondition, "the payment was declined")
	case errors.Is(err, foundryplan.ErrUnverified):
		return status.Error(codes.Unavailable, "we could not confirm whether your payment went through; check the plan before trying again")
	case errors.Is(err, foundryplan.ErrNoCustomer):
		return status.Error(codes.Unavailable, "billing is temporarily unavailable")
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

// foundryTiers maps the catalog's first three codes onto the tier enum older
// callers read; a deployment's added plans read UNSPECIFIED there and use code.
var foundryTiers = map[string]foundryv1.FoundryTier{
	"free": foundryv1.FoundryTier_FOUNDRY_TIER_FREE,
	"pro":  foundryv1.FoundryTier_FOUNDRY_TIER_PRO,
	"max":  foundryv1.FoundryTier_FOUNDRY_TIER_MAX,
}

func foundryPlanToProto(s foundryplan.Status) *foundryv1.FoundryPlan {
	out := &foundryv1.FoundryPlan{Tier: foundryTiers[s.Plan.Code], Code: s.Plan.Code, DisplayName: s.Plan.DisplayName,
		Sync: syncRulesToProto(s.Plan.Sync)}
	out.PastDue = s.PastDue
	out.CancelAtPeriodEnd = s.CancelAtPeriodEnd
	if s.CurrentPeriodEnd != nil {
		out.CurrentPeriodEnd = timestamppb.New(*s.CurrentPeriodEnd)
	}
	return out
}

func syncRulesToProto(r foundryplan.SyncRules) *foundryv1.FoundrySyncRules {
	out := &foundryv1.FoundrySyncRules{
		EverySeconds: int64(r.Every / time.Second), ShortestSeconds: int64(r.Shortest / time.Second),
		ReconcileEverySeconds: int64(r.ReconcileEvery / time.Second), RefreshEverySeconds: int64(r.RefreshEvery / time.Second),
	}
	for _, c := range r.Capture {
		out.Capture = append(out.Capture, string(c))
	}
	return out
}
