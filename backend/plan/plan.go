// Package plan is Foundry's plan catalog: the plans a deployment offers, the
// connector kinds each allows and how often its connectors sync and its
// graphs refresh. A host that sells plans reports the one a workspace holds
// (ports.PlanReader); where none is sold, every workspace is on the first.
package plan

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
)

// Capture is a way a connector's tables are read.
type Capture string

const (
	// CaptureFull reads every row.
	CaptureFull Capture = "full"
	// CaptureCursor reads rows the source says changed after a point.
	CaptureCursor Capture = "cursor"
	// CaptureEvents reads the source's change events.
	CaptureEvents Capture = "events"
	// CaptureLog reads a database's replication log.
	CaptureLog Capture = "log"
)

// SyncRules is how often a plan's connectors sync and how they may read.
type SyncRules struct {
	Every          time.Duration
	Shortest       time.Duration
	Capture        []Capture
	ReconcileEvery time.Duration
	RefreshEvery   time.Duration
}

func (r SyncRules) Allows(c Capture) bool { return slices.Contains(r.Capture, c) }

// Plan is one entry of the Foundry plan catalog. Plans are ordered by Rank: a
// customer holding two mid-switch gets the higher.
type Plan struct {
	Code         string
	DisplayName  string
	AutumnPlanID string
	Rank         int
	Kinds        []string
	Sync         SyncRules
}

// Includes reports whether the plan may create and sync connectors of kind.
func (p *Plan) Includes(kind string) bool { return slices.Contains(p.Kinds, kind) }

// Sold reports whether an Autumn plan grants it; Free never is.
func (p *Plan) Sold() bool { return p.AutumnPlanID != "" }

// Catalog is the deployment's plans in rank order, Free first.
type Catalog struct {
	plans []*Plan
}

// NewCatalog builds the catalog the config validated. known lists the
// connector kinds that exist; a plan naming another is refused. Nil skips the check.
func NewCatalog(plans []config.PlanConfig, known []string) (*Catalog, error) {
	if len(plans) == 0 || plans[0].AutumnPlanID != "" {
		return nil, errors.New("foundry plans: the first plan must be Free, sold by no Autumn plan")
	}
	c := &Catalog{}
	for i, cfg := range plans {
		for _, kind := range cfg.Kinds {
			if known != nil && !slices.Contains(known, kind) {
				return nil, fmt.Errorf("foundry plans: plan %s names unknown connector kind %q", cfg.Code, kind)
			}
		}
		rules := SyncRules{Every: cfg.Sync.Every, Shortest: cfg.Sync.Shortest, ReconcileEvery: cfg.Sync.ReconcileEvery,
			RefreshEvery: cfg.Sync.RefreshEvery}
		for _, m := range cfg.Sync.Capture {
			rules.Capture = append(rules.Capture, Capture(m))
		}
		c.plans = append(c.plans, &Plan{Code: cfg.Code, DisplayName: cfg.DisplayName, AutumnPlanID: cfg.AutumnPlanID, Rank: i,
			Kinds: slices.Clone(cfg.Kinds), Sync: rules})
	}
	return c, nil
}

func (c *Catalog) Free() *Plan { return c.plans[0] }

func (c *Catalog) Plans() []*Plan { return slices.Clone(c.plans) }

// Get returns the plan with code, or nil.
func (c *Catalog) Get(code string) *Plan {
	for _, p := range c.plans {
		if p.Code == code {
			return p
		}
	}
	return nil
}

// LowestWith returns the lowest-ranked plan that includes kind, or nil.
func (c *Catalog) LowestWith(kind string) *Plan {
	for _, p := range c.plans {
		if p.Includes(kind) {
			return p
		}
	}
	return nil
}

// ByAutumnPlan returns the sold plan the billing system names id, or nil.
func (c *Catalog) ByAutumnPlan(id string) *Plan {
	for _, p := range c.plans {
		if p.Sold() && p.AutumnPlanID == id {
			return p
		}
	}
	return nil
}

// Status is a workspace's Foundry plan as its billing system reports it now.
// That system is the record, so nothing here is stored.
type Status struct {
	// Plan is never nil: Free when nothing paid is held.
	Plan             *Plan
	PastDue          bool
	CurrentPeriodEnd *time.Time
	// CancelAtPeriodEnd means the paid plan ends at CurrentPeriodEnd unless resumed.
	CancelAtPeriodEnd bool
}

// CheckoutResult carries a Stripe page the customer must finish, or the plan
// after Autumn charged a card on file. That plan can still read Free for a
// moment while Autumn starts the period; the caller reads Status again.
type CheckoutResult struct {
	CheckoutURL string
	// Status is the plan held once the call returns: the current one while a
	// checkout page waits for payment.
	Status Status
}

// PlanRequiredError is a kind the workspace's plan does not include. Plan is
// the lowest plan that does, nil when none does.
type PlanRequiredError struct {
	Kind string
	Plan *Plan
}

func (e *PlanRequiredError) Error() string {
	if e.Plan == nil {
		return fmt.Sprintf("foundry: no plan includes %s connectors", e.Kind)
	}
	return fmt.Sprintf("foundry: %s connectors need Foundry %s", e.Kind, e.Plan.DisplayName)
}

var (
	// ErrNotPaid is returned for a change that needs a paid plan.
	ErrNotPaid = errors.New("foundry: workspace has no paid plan")
	// ErrPlanNotSold means checkout asked for a plan this deployment does not sell.
	ErrPlanNotSold = errors.New("foundry: plan is not sold")
	// ErrNoCustomer means the workspace's Autumn customer could not be linked.
	ErrNoCustomer = errors.New("foundry: workspace has no billing customer")
	// ErrPromotionCode means Autumn refused the promotion code the customer typed.
	ErrPromotionCode = errors.New("foundry: promotion code was not accepted")
	// ErrDeclined means the charge was refused; nothing is active.
	ErrDeclined = errors.New("foundry: payment was declined")
	// ErrUnverified means the charge outcome is unknown; read Status again before retrying.
	ErrUnverified = errors.New("foundry: payment outcome is unknown")
)
