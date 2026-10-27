package foundrysyncworker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	syncRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_sync_runs_total",
		Help: "Connector sync attempts by outcome: succeeded, retried, failed, or requeued uncounted when the worker stopped during one.",
	}, []string{"outcome"})
	syncRows = promauto.NewCounter(prometheus.CounterOpts{
		Name: "helix_foundry_sync_rows_total",
		Help: "Rows written to snapshots by successful connector syncs.",
	})
)

var (
	syncScheduled = promauto.NewCounter(prometheus.CounterOpts{
		Name: "helix_foundry_sync_scheduled_total",
		Help: "Connector syncs the scheduler queued because their connector was due.",
	})
	syncChanges = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_sync_mode_total",
		Help: "Successful connector syncs by how they read: full or changes.",
	}, []string{"mode"})
	snapshotsSwept = promauto.NewCounter(prometheus.CounterOpts{
		Name: "helix_foundry_snapshots_swept_total",
		Help: "Replaced snapshots deleted once no live load pinned them.",
	})
	refreshesQueued = promauto.NewCounter(prometheus.CounterOpts{
		Name: "helix_foundry_refreshes_queued_total",
		Help: "Refresh loads queued because a table an ontology's current load reads changed.",
	})
	batchesSwept = promauto.NewCounter(prometheus.CounterOpts{
		Name: "helix_foundry_change_batches_swept_total",
		Help: "Change batches deleted after foundry.sync.change_retention.",
	})
	documentsPurged = promauto.NewCounter(prometheus.CounterOpts{
		Name: "helix_foundry_documents_purged_total",
		Help: "Documents whose originals and extractions the sweep deleted: syncs' unclaimed reservations, and connectors' documents replaced or removed at their source.",
	})
	eventBatchesPurged = promauto.NewCounter(prometheus.CounterOpts{
		Name: "helix_foundry_event_batches_purged_total",
		Help: "Batches of deleted event sources deleted with their objects.",
	})
	eventSourcesPurged = promauto.NewCounter(prometheus.CounterOpts{
		Name: "helix_foundry_event_sources_purged_total",
		Help: "Deleted event sources the purge is done with: no batch, folder or token left. A new worker goes through them, and counts them, again.",
	})
	eventSyncsPruned = promauto.NewCounter(prometheus.CounterOpts{
		Name: "helix_foundry_event_syncs_pruned_total",
		Help: "Event source syncs deleted after succeeding more than foundry.sync.change_retention ago.",
	})
	cursorReleases = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_cursor_releases_total",
		Help: "Releases of what deleted and paused connectors held on their sources, such as replication slots, by outcome: released or failed.",
	}, []string{"outcome"})
	syncLag = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "helix_foundry_sync_lag_seconds",
		Help:    "How long a scheduled sync waited between being queued and its first claim; growth means connectors fall behind their interval.",
		Buckets: []float64{1, 10, 30, 60, 300, 900, 1800, 3600, 4 * 3600},
	})
)
