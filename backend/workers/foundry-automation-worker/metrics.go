package foundryautomationworker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var fires = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_automation_fires_total",
	Help: "Automation fires by trigger kind and what they did: queued, moved, skipped, waiting or lost.",
}, []string{"trigger", "fired"})

var runs = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_automation_runs_total",
	Help: "Automation run attempts by outcome: succeeded, failed, waiting (on a step), retried, or lost (cancelled, deleted, or taken by another worker).",
}, []string{"outcome"})

var steps = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_automation_steps_total",
	Help: "Automation steps ended, by status.",
}, []string{"status"})
