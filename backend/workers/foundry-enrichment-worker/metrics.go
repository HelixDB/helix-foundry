package foundryenrichmentworker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var sweeps = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_enrichment_sweeps_total",
	Help: "Enrichment sweep runs by outcome: ok, error or panic.",
}, []string{"outcome"})
