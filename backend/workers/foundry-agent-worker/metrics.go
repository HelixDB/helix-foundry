package foundryagentworker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var turns = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_agent_turns_total",
	Help: "Assistant turn attempts by outcome: done, failed, retried, or lost (cancelled, deleted, or taken by another worker).",
}, []string{"outcome"})

var steps = promauto.NewCounter(prometheus.CounterOpts{
	Name: "helix_foundry_agent_steps_total",
	Help: "Model calls the Assistant's turns made.",
})

var spend = promauto.NewCounter(prometheus.CounterOpts{
	Name: "helix_foundry_agent_spend_usd_micros_total",
	Help: "What the Assistant's turns spent, in millionths of a US dollar.",
})

var suggestionWrites = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_automation_suggestion_writes_total",
	Help: "Writes of automation suggestions by outcome: ready, failed, or lost (the lease went to another worker, or the row with its ontology).",
}, []string{"outcome"})

var suggestionSpend = promauto.NewCounter(prometheus.CounterOpts{
	Name: "helix_foundry_automation_suggestion_spend_usd_micros_total",
	Help: "What writes of automation suggestions spent, in millionths of a US dollar.",
})
