package slackbot

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	intake = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_slack_intake_total",
		Help: "Slack intake rows the worker handled, by kind (mention, dm, stopped, approve, dismiss, refresh, review, run, stop, unlink, unknown) " +
			"and outcome: done, failed, retried (a failure), deferred (a rate limit, or a busy thread with no turn to wait on), " +
			"waiting (behind a busy thread), lost (its lease was taken) or unrecorded.",
	}, []string{"kind", "outcome"})
	follows = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_slack_follows_total",
		Help: "Founder turns the worker followed into Slack, by how a follow ended: finished, gone (its install or turn went), " +
			"released (tried again), deferred (a rate limit), lost, gave_up (Slack refused the reply for good) or unrecorded.",
	}, []string{"outcome"})
	sweeps = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_slack_sweep_total",
		Help: "What the Slack worker's sweep did, by step (relay_offline, relay_expire, relay_feed, uninstall, purge, prune, prune_feed, " +
			"orphan, feeds, close, flip) and outcome: told (a relay notice posted), limited (a relay notice rate-limited, not tried again), " +
			"unreadable (a relay delivery's conversation), expired (relay feed rows made gaps), uninstalled, abandoned (taken as " +
			"uninstalled without Slack's word), deferred, purged and pruned (rows), deleted and unbound (connectors of removed installs), " +
			"busy, gone, closed (feeds), flipped, waiting, or error.",
	}, []string{"step", "outcome"})
	backfills = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_slack_backfill_total",
		Help: "Pages of Slack history the worker's backfill took, by method (history, replies) and outcome: read, busy (another replica " +
			"had the slot), lossy (a gap past the day's budget), deferred (a rate limit), scopes (a history scope missing), refused " +
			"(the install's token), cancelled or gone (the channel or thread can't be read), retried, failed (a window's last attempt), " +
			"conflict (another worker wrote the page), unbound or error.",
	}, []string{"method", "outcome"})
)
