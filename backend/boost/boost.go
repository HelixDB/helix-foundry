// Package boost lifts a Foundry ontology's database to
// foundry.graph.load_rate_limit for one attempt of a worker's work, so the
// attempt is not held to the plan's few requests a second: a graph job's
// writes (foundry-graph-worker) and a question's reads (foundry-ask-worker).
// Each attempt holds its own lease on the database's boost
// (foundry_boost_holders), named by its work and attempt, that lapses on its
// own if the attempt never removes it; the tenant's override is the live
// holders' greatest limit, so a load and a question on one database never
// clear each other's. Sweep removes the lapsed ones.
package boost

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// Margin is how long a boost outlives its attempt's timeout, for an
	// attempt that never removes it because its worker died.
	Margin = 5 * time.Minute
	// ReleaseTimeout bounds taking a boost back, which runs after the
	// attempt's context may have ended; a release that fails lapses instead.
	ReleaseTimeout = 5 * time.Second
	// SweepEvery is how often a worker removes boosts that lapsed, and
	// SweepLimit how many one transaction removes.
	SweepEvery = time.Minute
	SweepLimit = 100
)

var boosts = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_graph_load_boosts_total",
	Help: "Rate-limit boosts by holder (load or question) and event: set, unconfirmed (its gateway sync not seen in time), held_elsewhere (skipped: a staff override), failed, released or swept.",
}, []string{"holder", "event"})

// Booster lifts databases' limits for one worker's attempts, through the
// host's boosts.
type Booster struct {
	Boosts   ports.Boosts
	WorkerID string
	Limit    config.GraphLoadRateLimit
	// SyncWait bounds the wait for the gateway sync that carries a boost;
	// zero doesn't wait, leaving the lag to the proxy's resends. SyncPoll is
	// how often the wait reads the cluster's synced revision.
	SyncWait time.Duration
	SyncPoll time.Duration
}

// Attempt is the attempt a boost is for.
type Attempt struct {
	Holder model.FoundryBoostHolder
	// ID is the graph job's or the question's id, Number its attempt.
	ID       string
	Number   int
	TenantID string
	// Timeout is the attempt's own bound; the boost lapses Margin after it.
	Timeout time.Duration
}

// Acquire lifts a's database to the limit while a holds its work:
//  1. Write the attempt's own lease on the boost and mark the tenant's
//     cluster dirty, in one transaction; other attempts' leases on the
//     database stay. A live staff override on the tenant or its account
//     wins: nothing is written, the attempt runs at it.
//  2. Wait up to SyncWait for the cluster's synced revision to reach the
//     change's. An agent cluster's agent has then written limits.json; a
//     push cluster's sync is only queued, and a gateway pod may lag its
//     watch. The proxy's resends of turned-away queries cover both.
//
// The release it returns takes the attempt's lease back, leaving other
// holders'; it must run however the attempt ends, and does nothing when the
// attempt holds no lease. An error wrapping foundrystore.ErrNotFound means a no
// longer holds its work or its database; any other failure runs the attempt
// unboosted.
func (b *Booster) Acquire(ctx context.Context, a Attempt) (release func(), err error) {
	holder := string(a.Holder)
	boost := model.FoundryLoadBoost{
		TenantID: a.TenantID, Holder: a.Holder, JobID: a.ID, Attempt: a.Number,
		RequestsPerSecond: b.Limit.RequestsPerSecond, Burst: b.Limit.Burst, TTL: a.Timeout + Margin,
	}
	logger := log.From(ctx).With("holder", holder, "id", a.ID, "tenant", a.TenantID, "attempt", a.Number)
	release = func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ReleaseTimeout)
		defer cancel()
		err := b.Boosts.ReleaseLoadBoost(releaseCtx, boost)
		switch {
		case err == nil:
			boosts.WithLabelValues(holder, "released").Inc()
		case !errors.Is(err, foundrystore.ErrNotFound):
			logger.Warn("boost: lowering the database's rate limit failed; it lapses on its own", "err", err)
		}
	}
	change, err := b.Boosts.AcquireLoadBoost(ctx, b.WorkerID, boost)
	switch {
	case errors.Is(err, foundrystore.ErrConflict):
		boosts.WithLabelValues(holder, "held_elsewhere").Inc()
		logger.Info("boost: a staff override holds the database; the attempt runs at it")
		return func() {}, nil
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, fmt.Errorf("the attempt no longer holds its work or its database: %w", err)
	case err != nil:
		// The write may have committed: the release takes back whatever landed.
		boosts.WithLabelValues(holder, "failed").Inc()
		logger.Warn("boost: lifting the database's rate limit failed; the attempt runs at the database's own", "err", err)
		return release, nil
	}
	boosts.WithLabelValues(holder, "set").Inc()
	if b.SyncWait > 0 && !b.awaitSync(ctx, change) && ctx.Err() == nil {
		boosts.WithLabelValues(holder, "unconfirmed").Inc()
		logger.Warn("boost: the gateway did not confirm the rate limit in time; the attempt runs at the limit it has",
			"cluster", change.ClusterID, "revision", change.Revision)
	}
	return release, nil
}

// awaitSync waits up to SyncWait for the cluster's synced revision to reach
// the change's, and reports whether it did. A failed read counts as not yet.
func (b *Booster) awaitSync(ctx context.Context, change ports.BoostChange) bool {
	waitCtx, cancel := context.WithTimeout(ctx, b.SyncWait)
	defer cancel()
	for {
		synced, err := b.Boosts.SyncedRevision(waitCtx, change.ClusterID)
		if err == nil && synced >= change.Revision {
			return true
		}
		if err != nil && !errors.Is(err, foundrystore.ErrNotFound) && waitCtx.Err() == nil {
			log.From(ctx).Warn("boost: reading the gateway sync failed", "cluster", change.ClusterID, "err", err)
		}
		timer := time.NewTimer(b.SyncPoll)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// Sweep removes the boosts that lapsed before their attempt removed them, as
// a worker that died mid-attempt leaves them, a transaction of SweepLimit at
// a time until none is left. Sweeps of several workers skip each other's rows.
func Sweep(ctx context.Context, host ports.Boosts) {
	for ctx.Err() == nil {
		swept, err := host.SweepLoadBoosts(ctx, SweepLimit)
		if err != nil {
			log.From(ctx).Warn("sweeping lapsed rate limit boosts failed", "err", err)
			return
		}
		if swept > 0 {
			boosts.WithLabelValues("any", "swept").Add(float64(swept))
			log.From(ctx).Info("removed lapsed rate limit boosts", "count", swept)
		}
		if swept < SweepLimit {
			return
		}
	}
}
