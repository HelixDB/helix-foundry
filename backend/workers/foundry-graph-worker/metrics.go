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

var documentsHeld = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_graph_documents_held_total",
	Help: "Documents a job no person started held back unsent, by reason: set_allowance (the month's allowance of their set, or of its " +
		"workspace's sets of its kind, is spent: config.FoundryConfig.SetAllowances). They wait for next month or a load a person starts.",
}, []string{"reason"})
