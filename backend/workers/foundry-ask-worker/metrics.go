package foundryaskworker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var questions = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_questions_total",
	Help: "Question attempts by outcome: answered, cannot_answer, failed, retried, or lost (cancelled, or taken by another worker).",
}, []string{"outcome"})

var probes = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_ai_probes_total",
	Help: "AI settings probes by outcome: passed, failed, superseded, released (could not run), returned (cut short as the worker stopped), or lost (taken by another worker).",
}, []string{"outcome"})
