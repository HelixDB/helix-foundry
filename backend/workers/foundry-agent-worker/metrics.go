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
