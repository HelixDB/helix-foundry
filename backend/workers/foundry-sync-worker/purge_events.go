package foundrysyncworker

import (
	"context"

	"github.com/helixdb/helix-foundry/backend/events"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
)

// purgeEvents deletes what event sources leave behind, each tick:
//
//  1. Up to sweepBatch batches of sources deleted more than
//     foundry.sync.replaced_snapshot_grace ago: each batch's object, then
//     its row, so no object outlives the row that names it. A batch whose
//     object can't be deleted keeps its row for the next tick.
//  2. Up to sweepBatch of those sources, in the order they were deleted,
//     from where the last tick's pass stopped (t.sourcesPurged): of each
//     with no batch left, every object under its folder (orphans of sends
//     whose commit failed or whose outcome was never known), then its
//     tokens. The pass reaches every deleted source, even one that never
//     held a batch or a token: nothing stored says a folder is done, so
//     the pass's cursor does. It moves past a source only once that source
//     is done, so one with batches left, or whose folder or tokens can't be
//     deleted, is visited again next tick; the sources after it are
//     visited this tick all the same, and again next tick, which deletes
//     nothing new. The cursor lives in memory, so a new worker goes through
//     every deleted source once more, one listing of an empty folder each.
//  3. Up to sweepBatch event source syncs that succeeded more than
//     foundry.sync.change_retention ago, a source's newest kept.
//
// Each step logs a failure and goes on to the next; every step is safe to
// repeat, so two workers sweeping at once only repeat each other's work. A
// deleted source's table goes with its delete (provider.DeleteConnector).
// Only the purge tick reads or moves t.sourcesPurged.
func (t *Task) purgeEvents(ctx context.Context) {
	before := t.now().Add(-t.cfg.ReplacedSnapshotGrace)
	batches, err := t.queue.ListFoundryEventBatchPurges(ctx, before, sweepBatch)
	if err != nil {
		log.From(ctx).Error("list purged event batches failed", "err", err)
	}
	for _, b := range batches {
		if err := t.snapshots.Delete(ctx, b.S3Key); err != nil {
			log.From(ctx).Warn("delete purged event batch object failed", "batch", b.ID, "key", b.S3Key, "err", err)
			continue
		}
		if err := t.queue.DeleteFoundryEventBatch(ctx, b.ID, before); err != nil {
			log.From(ctx).Warn("forget purged event batch failed", "batch", b.ID, "err", err)
			continue
		}
		eventBatchesPurged.Inc()
	}

	sources, err := t.queue.ListFoundryEventSourcePurges(ctx, before, t.sourcesPurged, sweepBatch)
	if err != nil {
		log.From(ctx).Error("list purged event sources failed", "err", err)
	}
	// done is whether every source so far is done, so the cursor may move.
	done := true
	for _, s := range sources {
		if s.Batches {
			// Step 1 deletes them first, on this tick or a later one.
			done = false
			continue
		}
		if err := t.snapshots.DeletePrefix(ctx, events.BatchPrefix(s.WorkspaceID, s.ConnectorID)); err != nil {
			log.From(ctx).Warn("delete purged event source objects failed", "connector", s.ConnectorID, "err", err)
			done = false
			continue
		}
		if err := t.queue.FinishFoundryEventSourcePurge(ctx, s, before); err != nil {
			log.From(ctx).Warn("finish event source purge failed", "connector", s.ConnectorID, "err", err)
			done = false
			continue
		}
		if done {
			t.sourcesPurged = s
			eventSourcesPurged.Inc()
		}
	}

	pruned, err := t.queue.DeleteExpiredEventSyncs(ctx, t.now().Add(-t.cfg.ChangeRetention), sweepBatch)
	if err != nil {
		log.From(ctx).Error("delete expired event source syncs failed", "err", err)
		return
	}
	eventSyncsPruned.Add(float64(pruned))
}
