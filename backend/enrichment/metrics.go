package enrichment

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// People enrichment's metrics. Alerts watch the claim wait, the parked
// age, credits per hour, partial ends by reason, and 402 and 429 responses.
// The enrichment worker counts the steps it ends; a load counts those it
// ends while preparing and those that ended before any worker claimed them
// (busy, or a Stop or a cancel while queued).

var responses = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_enrichment_responses_total",
	Help: "Provider responses by HTTP status (5xx together; none for a timeout or network error). Alert on 402 and 429.",
}, []string{"status"})

var calls = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_enrichment_calls_total",
	Help: "Lookups by what they came to: matched, not_found, refused, failed, no_answer, halt or unreached.",
}, []string{"answer"})

var credits = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_enrichment_credits_total",
	Help: "Credits charged: charged for returned matches, uncertain for calls that got no answer.",
}, []string{"kind"})

var steps = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_enrichment_steps_total",
	Help: "Steps ended, by status and, for partial, why: by the enrichment worker, or by their load while preparing or before any claim.",
}, []string{"status", "stopped_by"})

var claimWait = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "helix_foundry_enrichment_claim_wait_seconds",
	Help:    "How long a step waited, from its preparing to its first claim, or to its busy end when no worker claimed it.",
	Buckets: []float64{1, 5, 15, 30, 60, 120, 180, 240, 300},
})

var parked = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "helix_foundry_enrichment_parked_seconds",
	Help:    "How long a load waited on its step, from its preparing to the profiles it applies, for loads that parked for lookups.",
	Buckets: []float64{60, 300, 600, 1200, 1800, 2700, 3600, 4200},
})

var swept = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_enrichment_swept_total",
	Help: "Ended steps the sweep cleared files of, by outcome: done or failed.",
}, []string{"outcome"})

var purges = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_enrichment_purges_total",
	Help: "Purge advances by step: done, waiting, queued, exhausted (alert: an operator must look) or failed.",
}, []string{"step"})

// stopLabel is a step end's stopped_by label: why a partial step stopped,
// empty for any other.
func stopLabel(why *model.FoundryEnrichmentStop) string {
	if why == nil {
		return ""
	}
	return string(*why)
}

// statusLabel is a response's status label.
func statusLabel(status int) string {
	switch {
	case status == 0:
		return "none"
	case status >= 500:
		return "5xx"
	}
	return strconv.Itoa(status)
}
