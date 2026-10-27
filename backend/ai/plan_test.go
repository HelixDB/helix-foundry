package ai

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A sold plan is trusted for PlanTTL, so polling and every model call
// don't each ask Autumn; Free and a failed read are asked again, so an
// upgrade counts at once. Only workspaces seen sold within PlanTTL are kept.
func TestPlans(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(1_800_000_000, 0)
	pro := &onPlan{plan: proPlan}
	p := NewPlans(pro)
	p.now = func() time.Time { return clock }
	for range 3 {
		if sold, err := p.Sold(ctx, "ws-1"); err != nil || !sold {
			t.Fatalf("Sold() = %v, %v", sold, err)
		}
	}
	if pro.reads != 1 {
		t.Fatalf("%d reads within PlanTTL, want 1", pro.reads)
	}
	// The workspace leaves Pro: it counts once PlanTTL has passed.
	pro.plan = freePlan
	clock = clock.Add(PlanTTL)
	for range 2 {
		if sold, err := p.Sold(ctx, "ws-1"); err != nil || sold {
			t.Fatalf("after leaving Pro: Sold() = %v, %v", sold, err)
		}
	}
	if pro.reads != 3 {
		t.Fatalf("%d reads, want Free read each time", pro.reads)
	}
	// An upgrade counts at once, and the expired answers are dropped.
	pro.plan = proPlan
	if sold, _ := p.Sold(ctx, "ws-1"); !sold {
		t.Fatal("an upgrade did not count")
	}
	clock = clock.Add(PlanTTL)
	if _, err := p.Sold(ctx, "ws-2"); err != nil || len(p.soldAt) != 1 {
		t.Fatalf("kept %v", p.soldAt)
	}
	// A failed read says no and is not kept.
	down := &onPlan{err: errors.New("autumn down")}
	p = NewPlans(down)
	for range 2 {
		if sold, err := p.Sold(ctx, "ws-1"); err == nil || sold {
			t.Fatalf("Autumn down: Sold() = %v, %v", sold, err)
		}
	}
	if down.reads != 2 {
		t.Fatalf("%d reads, want each failure read again", down.reads)
	}
	// A deployment that sells no plan has no workspace on one.
	for _, none := range []*Plans{NewPlans(nil), nil} {
		if sold, err := none.Sold(ctx, "ws-1"); err != nil || sold {
			t.Fatalf("no plans: Sold() = %v, %v", sold, err)
		}
	}
}
