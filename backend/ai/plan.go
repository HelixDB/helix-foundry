package ai

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
)

// ErrProRequired is a hosted workspace running on its own provider while
// on no plan that allows one (Foundry Pro or Max): it left the plan after
// an owner saved the provider. Its model work stops until an owner upgrades
// or goes back to Helix's Claude.
var ErrProRequired = errors.New("ai: bringing your own model key needs Foundry Pro")

// PlanTTL is how long a workspace's sold plan is trusted once read, as long
// as the site caches it: a workspace that leaves Pro stops running on its
// own key within it.
const PlanTTL = time.Minute

// PlanReader reads a workspace's Foundry plan; the host's ports.PlanReader
// is one.
type PlanReader interface {
	Status(ctx context.Context, workspaceID string) (foundryplan.Status, error)
}

// Plans says whether hosted workspaces are on a plan that lets them bring
// their own provider: any plan sold, Pro or above. A sold plan is kept for
// PlanTTL, so a settings page polling a probe and every model call's
// resolution don't each call Autumn. Free and a failed read are not kept, so
// an upgrade counts at once. It holds only the workspaces found on a sold
// plan within the last PlanTTL.
type Plans struct {
	// reader is nil where the deployment sells no plan: no workspace is on one.
	reader PlanReader
	now    func() time.Time
	mu     sync.Mutex
	// soldAt is when each workspace was last read on a sold plan.
	soldAt map[string]time.Time
}

// NewPlans reads plans through reader; a nil reader sells none.
func NewPlans(reader PlanReader) *Plans {
	return &Plans{reader: reader, now: time.Now, soldAt: map[string]time.Time{}}
}

// Sold reports whether workspaceID is on a sold plan. A nil Plans, like a
// nil reader, sells none.
func (p *Plans) Sold(ctx context.Context, workspaceID string) (bool, error) {
	if p == nil || p.reader == nil {
		return false, nil
	}
	now := p.now()
	p.mu.Lock()
	at, ok := p.soldAt[workspaceID]
	p.mu.Unlock()
	if ok && now.Sub(at) < PlanTTL {
		return true, nil
	}
	status, err := p.reader.Status(ctx, workspaceID)
	if err != nil || !status.Plan.Sold() {
		return false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	maps.DeleteFunc(p.soldAt, func(_ string, at time.Time) bool { return now.Sub(at) >= PlanTTL })
	p.soldAt[workspaceID] = now
	return true, nil
}
