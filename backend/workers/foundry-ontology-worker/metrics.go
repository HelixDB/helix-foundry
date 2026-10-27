package foundryontologyworker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	generations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_ontology_generations_total",
		Help: "Ontology generation attempts by outcome: succeeded, retried or failed.",
	}, []string{"outcome"})
	tokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_ontology_tokens_total",
		Help: "Claude tokens spent on ontology generations, by direction: input or output.",
	}, []string{"direction"})
)
