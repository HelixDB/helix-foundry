package slackbot

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	intake = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_slack_intake_total",
		Help: "Slack intake rows the worker handled, by kind (mention, dm, stopped, approve, dismiss, refresh, review, run, stop, unlink, unknown) " +
			"and outcome: done, failed, retried, waiting (behind a busy thread), lost (its lease was taken) or unrecorded.",
	}, []string{"kind", "outcome"})
	follows = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_slack_follows_total",
		Help: "Founder turns the worker followed into Slack, by how a follow ended: finished, gone (its install or turn went), " +
			"released (tried again), deferred (a rate limit), lost, gave_up (Slack won't take the reply) or unrecorded.",
	}, []string{"outcome"})
	sweeps = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_slack_sweep_total",
		Help: "What the Slack worker's sweep did, by step (uninstall, purge, prune) and outcome: uninstalled, deferred, purged and " +
			"pruned (rows), or error.",
	}, []string{"step", "outcome"})
)
