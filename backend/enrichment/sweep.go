package enrichment

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// sweepBatch is how many steps one sweep clears files of, and how many
	// purges it advances.
	sweepBatch = 100
	// supersededAfter is how long a step's results outlive a newer step's:
	// a load preparing meanwhile may still be reading them.
	supersededAfter = 24 * time.Hour
	// purgeRetry is how long a purge waits before queueing another clean
	// load, and purgeAttempts how many it queues before an operator must
	// look.
	purgeRetry    = 24 * time.Hour
	purgeAttempts = 3
)

// SweepStore is what the sweep reads and writes.
type SweepStore interface {
	ListFoundryEnrichmentSweeps(ctx context.Context, callsBefore, supersededBefore, wholeBefore time.Time, limit int) ([]model.FoundryEnrichmentSweep, error)
	ClearFoundryEnrichmentFiles(ctx context.Context, sweep model.FoundryEnrichmentSweep) error
	ListFoundryEnrichmentPurges(ctx context.Context, maxAttempts, limit int) ([]*model.FoundryEnrichmentPurge, error)
	AdvanceFoundryEnrichmentPurge(ctx context.Context, ontologyID string, load *model.FoundryGraphJob, deleteJobID string,
		retryAfter time.Duration, maxAttempts int) (model.FoundryEnrichmentPurgeStep, error)
}

// Sweeper deletes the files of ended steps once they are no longer needed,
// and moves purges on.
type Sweeper struct {
	DB    SweepStore
	Store snapshot.Store
	Cfg   config.EnrichmentConfig
	// GraphLease is the graph worker's job lease (foundry.graph.lease): a
	// graph attempt that lost its job writes nothing a lease after, so a
	// step's whole layout is deleted only a lease past its end.
	GraphLease time.Duration
	Now        func() time.Time
	// NewID names a purge's clean load and the delete job that follows it.
	NewID func() string
}

// Files deletes what ended steps no longer need (the provider's
// ListFoundryEnrichmentSweeps says what): the plan and the batch results at
// the step's end, the raw calls after Cfg.RawRetentionDays, the carried and
// published profiles once their load is deleted, results once superseded,
// and everything of a step whose consent was withdrawn, or that failed
// before any worker claimed it, once no load writing applies it and a
// graph attempt that lost the step's job has stopped writing it (every
// file of its layout, named on its row or not). A step whose files could
// not all be deleted keeps its keys and is tried again next sweep. It
// returns how many steps it cleared.
func (s *Sweeper) Files(ctx context.Context) (int, error) {
	now := s.Now()
	sweeps, err := s.DB.ListFoundryEnrichmentSweeps(ctx, now.Add(-days(s.Cfg.RawRetentionDays)), now.Add(-supersededAfter),
		now.Add(-s.GraphLease), sweepBatch)
	if err != nil {
		return 0, err
	}
	cleared := 0
	for _, sw := range sweeps {
		layout := LoadLayout(sw.WorkspaceID, sw.OntologyID, sw.LoadID)
		var keys []string
		for _, key := range []*string{sw.Plan, sw.Carried, sw.Profiles, sw.Results} {
			if key != nil {
				keys = append(keys, *key)
			}
		}
		if sw.WholeLayout {
			keys = append(keys, layout.Plan(), layout.Carried(), layout.Profiles(), layout.Results())
			slices.Sort(keys)
			keys = slices.Compact(keys)
		}
		for b := range sw.Batches {
			if sw.Plan != nil {
				keys = append(keys, layout.BatchResults(b))
			}
			if sw.Calls {
				keys = append(keys, layout.Calls(b))
			}
		}
		if err := s.delete(ctx, keys); err != nil {
			swept.WithLabelValues("failed").Inc()
			log.From(ctx).Warn("foundry enrichment sweep could not delete a step's files", "load", sw.LoadID, "err", err)
			continue
		}
		if err := s.DB.ClearFoundryEnrichmentFiles(ctx, sw); err != nil {
			return cleared, err
		}
		swept.WithLabelValues("done").Inc()
		cleared++
	}
	return cleared, nil
}

// delete deletes every key; a key already gone is no failure.
func (s *Sweeper) delete(ctx context.Context, keys []string) error {
	var errs []error
	for _, key := range keys {
		if err := s.Store.Delete(ctx, key); err != nil && !errors.Is(err, snapshot.ErrMissing) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Purges moves on every pending purge of an ontology whose enriched values
// a clean load removes after consent was withdrawn (the provider's
// AdvanceFoundryEnrichmentPurge): it waits past a live job, queues a clean
// load (which skips the monthly spend refusal a user's load gets) and
// retries it daily up to purgeAttempts times, then gives up loudly. It
// returns how many purges reached each step.
func (s *Sweeper) Purges(ctx context.Context) (map[model.FoundryEnrichmentPurgeStep]int, error) {
	pending, err := s.DB.ListFoundryEnrichmentPurges(ctx, purgeAttempts, sweepBatch)
	if err != nil {
		return nil, err
	}
	out := map[model.FoundryEnrichmentPurgeStep]int{}
	for _, p := range pending {
		step, err := s.DB.AdvanceFoundryEnrichmentPurge(ctx, p.OntologyID, &model.FoundryGraphJob{ID: s.NewID()}, s.NewID(), purgeRetry, purgeAttempts)
		switch {
		case errors.Is(err, store.ErrNotFound): // done meanwhile
			continue
		case err != nil:
			purges.WithLabelValues("failed").Inc()
			log.From(ctx).Warn("foundry enrichment purge could not move on", "ontology", p.OntologyID, "err", err)
			continue
		case step == model.FoundryEnrichmentPurgeExhausted:
			log.From(ctx).Error("foundry enrichment purge gave up: enriched values stay until an operator removes them",
				"ontology", p.OntologyID, "workspace", p.WorkspaceID, "attempts", p.Attempts)
		}
		purges.WithLabelValues(string(step)).Inc()
		out[step]++
	}
	return out, nil
}
