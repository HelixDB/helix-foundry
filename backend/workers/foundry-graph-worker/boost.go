package foundrygraphworker

import (
	"context"
	"errors"
	"time"

	"github.com/helixdb/helix-foundry/backend/boost"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// boostSyncWait bounds the wait for the gateway sync that carries a boost;
	// past it the attempt writes at whatever limit the gateway has.
	boostSyncWait = 2 * time.Minute
	// boostSyncPoll is how often that wait reads the cluster's synced revision.
	boostSyncPoll = 2 * time.Second
)

// boost lifts the ontology's database to foundry.graph.load_rate_limit for
// this attempt of any kind of job (a load's ingest, its documents'
// extraction or its delete), and waits for the gateway to have it
// (boost.Booster.Acquire). The release it returns must run however the
// attempt ends.
func (r *runner) boost(ctx context.Context, job *model.FoundryGraphJob, tenantID string) (release func(), err error) {
	booster := boost.Booster{Boosts: r.boosts, WorkerID: r.workerID, Limit: r.cfg.Foundry.Graph.LoadRateLimit, SyncWait: r.syncWait, SyncPoll: r.syncPoll}
	release, err = booster.Acquire(ctx, boost.Attempt{
		Holder: model.FoundryBoostLoad, ID: job.ID, Number: job.Attempts, TenantID: tenantID, Timeout: r.cfg.Foundry.Graph.Timeout,
	})
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, retry("the attempt no longer holds its job or its database; Helix will retry", err)
	}
	return release, err
}

// sweep removes the boosts that lapsed before their attempt removed them.
func (t *Task) sweep(ctx context.Context) { boost.Sweep(ctx, t.boosts) }
