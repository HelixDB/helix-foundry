package foundrygraphworker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var jobs = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_graph_jobs_total",
	Help: "Graph job attempts by outcome: succeeded, budget_exhausted, failed, retried, merged, or parked (waiting for its People enrichment lookups).",
}, []string{"outcome"})

var refreshLag = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "helix_foundry_refresh_lag_seconds",
	Help:    "How long a refresh load waited between being queued and its first claim; growth means the deployment needs more graph workers.",
	Buckets: []float64{1, 10, 30, 60, 300, 900, 1800, 3600, 4 * 3600},
})

var loadDeletes = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_load_deletes_total",
	Help: "Delete job attempts by outcome: done, retried, or failed (the load is left delete_failed; alert on it).",
}, []string{"outcome"})

// durationBuckets span a small extract (seconds) to a load that uses its whole attempt (2h).
var durationBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200}

var jobSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "helix_foundry_graph_job_seconds",
	Help:    "How long a graph job attempt took, by kind (load, extract, apply, delete), the origin of the load it works on (user or refresh) and outcome.",
	Buckets: durationBuckets,
}, []string{"kind", "origin", "outcome"})

var stepSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "helix_foundry_graph_step_seconds",
	Help:    "How long a step of a graph job attempt took: tables (pinning and downloading snapshots), ingest, extract or delete.",
	Buckets: durationBuckets,
}, []string{"step"})

var chunkRows = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_graph_rows_total",
	Help: "Table rows the ontology agent handled in its chunks, by job kind.",
}, []string{"kind"})

var chunks = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_graph_chunks_total",
	Help: "Chunks the ontology agent handled, by job kind and outcome: loaded, already_loaded (a retry found it stored), empty, or unknown.",
}, []string{"kind", "outcome"})

var documentsHeld = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_graph_documents_held_total",
	Help: "Documents a job no person started held back unsent, by reason: set_allowance (the month's allowance of their set, or of its " +
		"workspace's sets of its kind, is spent: config.FoundryConfig.SetAllowances). They wait for next month or a load a person starts.",
}, []string{"reason"})

var applies = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_graph_applies_total",
	Help: "Apply jobs that ended, by outcome: done (the tables are up to date), unsupported (a full refresh load replaces it) or failed.",
}, []string{"outcome"})

var applyRows = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_graph_apply_rows_total",
	Help: "Table rows an apply wrote, by change: added, changed or removed. Rows it found unchanged are not counted.",
}, []string{"change"})

var changeFileRows = promauto.NewCounter(prometheus.CounterOpts{
	Name: "helix_foundry_graph_change_file_rows_total",
	Help: "Changes an apply sent as change files in place of whole snapshots, one per changed key.",
})
