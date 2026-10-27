package api

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryconfig "github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/ports"
)

// testCatalog is the default catalog with Pro and Max sold.
func testCatalog() *foundryplan.Catalog {
	plans := foundryconfig.DefaultPlans()
	plans[1].AutumnPlanID, plans[2].AutumnPlanID = "foundry-pro", "foundry-max"
	c, err := foundryplan.NewCatalog(plans, nil)
	if err != nil {
		panic(err)
	}
	return c
}

// held is a status holding the catalog plan with code.
func held(code string) foundryplan.Status { return foundryplan.Status{Plan: testCatalog().Get(code)} }

// stubFoundryPlans answers every call with status (Free when unset) and err,
// recording checkouts as workspace|plan|promotion code|client token.
type stubFoundryPlans struct {
	status    foundryplan.Status
	checkout  foundryplan.CheckoutResult
	err       error
	checkouts []string
}

func (s *stubFoundryPlans) current() foundryplan.Status {
	if s.status.Plan == nil {
		return held("free")
	}
	return s.status
}

func (s *stubFoundryPlans) Status(context.Context, string) (foundryplan.Status, error) {
	return s.current(), s.err
}

func (s *stubFoundryPlans) Checkout(_ context.Context, workspaceID, code, promotionCode, clientToken string) (
	foundryplan.CheckoutResult, error) {
	s.checkouts = append(s.checkouts, workspaceID+"|"+code+"|"+promotionCode+"|"+clientToken)
	// Checkout always reports a plan; a test that sets none gets Free.
	out := s.checkout
	if out == (foundryplan.CheckoutResult{}) {
		out.Status = held("free")
	}
	return out, s.err
}

func (s *stubFoundryPlans) Cancel(context.Context, string) (foundryplan.Status, error) {
	return s.current(), s.err
}

func (s *stubFoundryPlans) Resume(context.Context, string) (foundryplan.Status, error) {
	return s.current(), s.err
}

func (s *stubFoundryPlans) RequireKind(_ context.Context, _, kind string) error {
	if s.err != nil {
		return s.err
	}
	if !s.current().Plan.Includes(kind) {
		return &foundryplan.PlanRequiredError{Kind: kind, Plan: testCatalog().LowestWith(kind)}
	}
	return nil
}

func foundryPlanService(plans ports.Plans) *Service {
	return &Service{deps: &runtimeDeps{Plans: plans, Catalog: testCatalog()}}
}

func TestFoundryPlanRPCs(t *testing.T) {
	// A deployment that sells no Foundry plan must say so rather than report everyone as Free.
	t.Run("reports billing unavailable when no plan is configured", func(t *testing.T) {
		_, err := foundryPlanService(nil).GetFoundryPlan(context.Background(), &foundryv1.GetFoundryPlanRequest{WorkspaceId: "ws-1"})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("GetFoundryPlan error = %v, want Unavailable", err)
		}
	})
	t.Run("requires a workspace", func(t *testing.T) {
		_, err := foundryPlanService(&stubFoundryPlans{}).CancelFoundryPro(context.Background(), &foundryv1.CancelFoundryProRequest{})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("CancelFoundryPro error = %v, want InvalidArgument", err)
		}
	})
	// Each held plan reports its tier, code, sync rules and period.
	for code, want := range map[string]foundryv1.FoundryTier{
		"pro": foundryv1.FoundryTier_FOUNDRY_TIER_PRO,
		"max": foundryv1.FoundryTier_FOUNDRY_TIER_MAX,
	} {
		t.Run("returns "+code+" with its rules and period end", func(t *testing.T) {
			end := time.Date(2026, time.November, 16, 0, 0, 0, 0, time.UTC)
			status := held(code)
			status.CurrentPeriodEnd, status.CancelAtPeriodEnd, status.PastDue = &end, true, true
			plans := &stubFoundryPlans{status: status}

			resp, err := foundryPlanService(plans).GetFoundryPlan(context.Background(), &foundryv1.GetFoundryPlanRequest{WorkspaceId: "ws-1"})
			if err != nil {
				t.Fatal(err)
			}
			plan := resp.GetPlan()
			if plan.GetTier() != want || plan.GetCode() != code || !plan.GetCurrentPeriodEnd().AsTime().Equal(end) || !plan.GetCancelAtPeriodEnd() || !plan.GetPastDue() {
				t.Fatalf("plan = %+v", plan)
			}
			if plan.GetSync().GetEverySeconds() != int64(status.Plan.Sync.Every/time.Second) || !slices.Contains(plan.GetSync().GetCapture(), "cursor") {
				t.Fatalf("sync = %+v", plan.GetSync())
			}
		})
	}
	// A plan a deployment adds has no tier of its own; its code is what the website reads.
	t.Run("returns a plan outside the three tiers by its code", func(t *testing.T) {
		plans := foundryconfig.DefaultPlans()
		plans = append(plans, foundryconfig.PlanConfig{Code: "starter", DisplayName: "Starter", AutumnPlanID: "foundry-starter",
			Kinds: []string{"stripe"}, Sync: foundryconfig.SyncRulesConfig{Every: 6 * time.Hour, Shortest: 6 * time.Hour,
				Capture: []string{"full"}, RefreshEvery: 6 * time.Hour}})
		c, err := foundryplan.NewCatalog(plans, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := foundryPlanService(&stubFoundryPlans{status: foundryplan.Status{Plan: c.Get("starter")}}).GetFoundryPlan(context.Background(),
			&foundryv1.GetFoundryPlanRequest{WorkspaceId: "ws-1"})
		if err != nil || resp.GetPlan().GetCode() != "starter" || resp.GetPlan().GetTier() != foundryv1.FoundryTier_FOUNDRY_TIER_UNSPECIFIED {
			t.Fatalf("GetFoundryPlan = (%+v, %v)", resp, err)
		}
	})
	// The pricing page lists the catalog whether or not the deployment sells anything yet.
	t.Run("lists the catalog in rank order", func(t *testing.T) {
		for name, s := range map[string]*Service{"selling": foundryPlanService(&stubFoundryPlans{}), "not selling": {deps: &runtimeDeps{}}} {
			resp, err := s.ListFoundryPlans(context.Background(), &foundryv1.ListFoundryPlansRequest{WorkspaceId: "ws-1"})
			if err != nil {
				t.Fatal(err)
			}
			var codes []string
			for _, p := range resp.GetPlans() {
				codes = append(codes, p.GetCode())
			}
			if !slices.Equal(codes, []string{"free", "pro", "max"}) || resp.GetPlans()[0].GetSold() ||
				resp.GetPlans()[1].GetSold() != (name == "selling") || !slices.Contains(resp.GetPlans()[1].GetKinds(), "postgres") {
				t.Fatalf("%s: plans = %+v", name, resp.GetPlans())
			}
		}
	})
	t.Run("returns free without a period", func(t *testing.T) {
		resp, err := foundryPlanService(&stubFoundryPlans{}).GetFoundryPlan(context.Background(), &foundryv1.GetFoundryPlanRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		if plan := resp.GetPlan(); plan.GetTier() != foundryv1.FoundryTier_FOUNDRY_TIER_FREE || plan.GetCurrentPeriodEnd() != nil {
			t.Fatalf("plan = %+v", plan)
		}
	})
	// Cancel and resume act on whichever plan is held and report it.
	t.Run("cancel and resume report the held max plan", func(t *testing.T) {
		plans := &stubFoundryPlans{status: held("max")}
		cancelled, err := foundryPlanService(plans).CancelFoundryPro(context.Background(), &foundryv1.CancelFoundryProRequest{WorkspaceId: "ws-1"})
		if err != nil || cancelled.GetPlan().GetTier() != foundryv1.FoundryTier_FOUNDRY_TIER_MAX {
			t.Fatalf("CancelFoundryPro = (%+v, %v)", cancelled, err)
		}
		resumed, err := foundryPlanService(plans).ResumeFoundryPro(context.Background(), &foundryv1.ResumeFoundryProRequest{WorkspaceId: "ws-1"})
		if err != nil || resumed.GetPlan().GetTier() != foundryv1.FoundryTier_FOUNDRY_TIER_MAX {
			t.Fatalf("ResumeFoundryPro = (%+v, %v)", resumed, err)
		}
	})
	// Without the token a double click is two purchases.
	t.Run("checkout requires a client token", func(t *testing.T) {
		plans := &stubFoundryPlans{}
		_, err := foundryPlanService(plans).CreateFoundryProCheckout(context.Background(), &foundryv1.CreateFoundryProCheckoutRequest{WorkspaceId: "ws-1"})
		if status.Code(err) != codes.InvalidArgument || len(plans.checkouts) != 0 {
			t.Fatalf("CreateFoundryProCheckout error = %v, checkouts = %v", err, plans.checkouts)
		}
	})
	// Codes are pasted from emails, often with whitespace around them.
	t.Run("checkout passes the trimmed promotion code", func(t *testing.T) {
		plans := &stubFoundryPlans{checkout: foundryplan.CheckoutResult{CheckoutURL: "https://checkout.stripe.com/c/pay/cs_test", Status: held("free")}}

		resp, err := foundryPlanService(plans).CreateFoundryProCheckout(context.Background(), &foundryv1.CreateFoundryProCheckoutRequest{
			WorkspaceId: "ws-1", PromoCode: " LAUNCH50 ", ClientToken: "click-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetCheckoutUrl() != "https://checkout.stripe.com/c/pay/cs_test" || resp.GetPlan().GetTier() != foundryv1.FoundryTier_FOUNDRY_TIER_FREE {
			t.Fatalf("response = %+v", resp)
		}
		if len(plans.checkouts) != 1 || plans.checkouts[0] != "ws-1|pro|LAUNCH50|click-1" {
			t.Fatalf("checkouts = %v", plans.checkouts)
		}
	})
	// An unset tier is Pro, so a site that predates Max still buys Pro.
	for requested, want := range map[foundryv1.FoundryTier]string{
		foundryv1.FoundryTier_FOUNDRY_TIER_UNSPECIFIED: "pro",
		foundryv1.FoundryTier_FOUNDRY_TIER_PRO:         "pro",
		foundryv1.FoundryTier_FOUNDRY_TIER_MAX:         "max",
	} {
		t.Run("checkout of "+requested.String()+" buys "+want, func(t *testing.T) {
			plans := &stubFoundryPlans{}
			_, err := foundryPlanService(plans).CreateFoundryProCheckout(context.Background(), &foundryv1.CreateFoundryProCheckoutRequest{
				WorkspaceId: "ws-1", ClientToken: "click-1", Tier: requested,
			})
			if err != nil || len(plans.checkouts) != 1 || plans.checkouts[0] != "ws-1|"+want+"||click-1" {
				t.Fatalf("CreateFoundryProCheckout error = %v, checkouts = %v", err, plans.checkouts)
			}
		})
	}
	// A plan code from the catalog wins over the tier older callers send.
	t.Run("checkout buys the plan code it is given", func(t *testing.T) {
		plans := &stubFoundryPlans{}
		_, err := foundryPlanService(plans).CreateFoundryProCheckout(context.Background(), &foundryv1.CreateFoundryProCheckoutRequest{
			WorkspaceId: "ws-1", ClientToken: "click-1", Tier: foundryv1.FoundryTier_FOUNDRY_TIER_PRO, PlanCode: "starter",
		})
		if err != nil || len(plans.checkouts) != 1 || plans.checkouts[0] != "ws-1|starter||click-1" {
			t.Fatalf("CreateFoundryProCheckout error = %v, checkouts = %v", err, plans.checkouts)
		}
	})
	// Free is not bought, and a tier this build does not know is not guessed at.
	for _, requested := range []foundryv1.FoundryTier{foundryv1.FoundryTier_FOUNDRY_TIER_FREE, foundryv1.FoundryTier(99)} {
		t.Run("checkout refuses tier "+requested.String(), func(t *testing.T) {
			plans := &stubFoundryPlans{}
			_, err := foundryPlanService(plans).CreateFoundryProCheckout(context.Background(), &foundryv1.CreateFoundryProCheckoutRequest{
				WorkspaceId: "ws-1", ClientToken: "click-1", Tier: requested,
			})
			if status.Code(err) != codes.InvalidArgument || len(plans.checkouts) != 0 {
				t.Fatalf("CreateFoundryProCheckout error = %v, checkouts = %v", err, plans.checkouts)
			}
		})
	}
	// Each failure the customer can act on gets its own code; anything else is ours.
	for _, tc := range []struct {
		err  error
		want codes.Code
	}{
		{foundryplan.ErrNotPaid, codes.FailedPrecondition},
		{fmt.Errorf("%w: max", foundryplan.ErrPlanNotSold), codes.FailedPrecondition},
		{fmt.Errorf("%w: no such code", foundryplan.ErrPromotionCode), codes.InvalidArgument},
		{foundryplan.ErrDeclined, codes.FailedPrecondition},
		{foundryplan.ErrUnverified, codes.Unavailable},
		{foundryplan.ErrNoCustomer, codes.Unavailable},
		{fmt.Errorf("billing: status 500"), codes.Internal},
	} {
		t.Run(fmt.Sprintf("maps %v to %s", tc.err, tc.want), func(t *testing.T) {
			_, err := foundryPlanService(&stubFoundryPlans{err: tc.err}).CreateFoundryProCheckout(context.Background(),
				&foundryv1.CreateFoundryProCheckoutRequest{WorkspaceId: "ws-1", ClientToken: "click-1"})
			if status.Code(err) != tc.want {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
}
