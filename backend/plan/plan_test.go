package plan

import (
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
)

const (
	proPlan = "foundry-pro"
	maxPlan = "foundry-max"
)

// catalogSelling is the default catalog with Pro and Max sold under these
// Autumn plans; an empty id leaves that plan unsold.
func catalogSelling(t *testing.T, pro, max string) *Catalog {
	t.Helper()
	plans := config.DefaultPlans()
	plans[1].AutumnPlanID, plans[2].AutumnPlanID = pro, max
	c, err := NewCatalog(plans, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// workspaceAccounts holds billing accounts by workspace; a workspace absent
// from the map has never been billed.
func TestFoundryCatalog(t *testing.T) {
	// A plan naming a kind no connector has would gate nothing; it is refused when the deployment starts.
	t.Run("refuses an unknown kind", func(t *testing.T) {
		plans := config.DefaultPlans()
		plans[1].Kinds = append(plans[1].Kinds, "salesforce")
		if _, err := NewCatalog(plans, []string{"stripe", "workos", "posthog", "github", "linear", "calcom", "granola", "postgres", "mysql", "s3"}); err == nil {
			t.Fatal("NewCatalog succeeded, want error")
		}
	})
	// A refused kind names the cheapest plan that unlocks it.
	t.Run("finds the lowest plan with a kind", func(t *testing.T) {
		c := catalogSelling(t, proPlan, maxPlan)
		if got := c.LowestWith("postgres"); got == nil || got.Code != "pro" {
			t.Fatalf("LowestWith(postgres) = %+v, want pro", got)
		}
		if got := c.LowestWith("stripe"); got == nil || got.Code != "free" {
			t.Fatalf("LowestWith(stripe) = %+v, want free", got)
		}
		if got := c.LowestWith("salesforce"); got != nil {
			t.Fatalf("LowestWith(salesforce) = %+v, want none", got)
		}
	})
	// A billing system names the sold plans only; Free is no plan of its.
	t.Run("finds a sold plan by its billing plan", func(t *testing.T) {
		c := catalogSelling(t, proPlan, "")
		if got := c.ByAutumnPlan(proPlan); got == nil || got.Code != "pro" {
			t.Fatalf("ByAutumnPlan(%s) = %+v, want pro", proPlan, got)
		}
		if got := c.ByAutumnPlan(""); got != nil {
			t.Fatalf("ByAutumnPlan(\"\") = %+v, want none", got)
		}
		if got := c.ByAutumnPlan(maxPlan); got != nil {
			t.Fatalf("ByAutumnPlan(%s) = %+v, want none: max is not sold", maxPlan, got)
		}
	})
	// The sync rules the scheduler reads come straight from the entry.
	t.Run("carries each plan's sync rules", func(t *testing.T) {
		max := catalogSelling(t, proPlan, maxPlan).Get("max")
		if max.Sync.Every != 15*time.Minute || !max.Sync.Allows(CaptureLog) || max.Rank != 2 {
			t.Fatalf("max = %+v", max)
		}
		if free := catalogSelling(t, proPlan, maxPlan).Free(); free.Sync.Allows(CaptureCursor) || free.Sold() {
			t.Fatalf("free = %+v", free)
		}
	})
}
