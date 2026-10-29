package foundrynotifyworker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var notifications = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_notifications_total",
	Help: "Foundry's emails by kind (ontology_ready, sample_follow_up) and outcome: found (queued to send, or skipped at once for a person " +
		"with data of their own), sent, skipped, retried, failed, lost (its claim lapsed before it was recorded) or unrecorded.",
}, []string{"kind", "outcome"})
