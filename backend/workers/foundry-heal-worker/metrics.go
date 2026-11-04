package foundryhealworker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var runs = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_heal_runs_total",
	Help: "Audits of ontologies' loads by outcome: complete (its findings stored), incomplete (a page of one document was cut short, " +
		"the run ran out of time, or a graph job changed the load), failed, held (another worker runs the ontology's), stale (the load " +
		"wasn't due by the time the run started), lost (its lease lapsed) or unrecorded (failed as its end was recorded).",
}, []string{"outcome"})

var pages = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_heal_pages_total",
	Help: "Audit calls by outcome: done, or cut (the agent's 60 s wall came first, so the page is audited again at half the size).",
}, []string{"outcome"})

var runSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "helix_foundry_heal_run_seconds",
	Help:    "How long an audit of a load took, by outcome; about a second a page of 1,000 documents.",
	Buckets: []float64{1, 5, 15, 60, 300, 900, 1800, 3600, 7200},
}, []string{"outcome"})

var findings = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "helix_foundry_heal_findings",
	Help:    "Findings a complete audit stores: those backed by two documents or more, and by rows.",
	Buckets: []float64{0, 10, 100, 1000, 10_000, 100_000, 1_000_000},
})
