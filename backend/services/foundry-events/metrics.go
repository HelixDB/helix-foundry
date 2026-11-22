package foundryevents

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// requestsTotal counts the route's answers by code: ok for a 200, or the error body's code. The
// mux's own 404 and 405 aren't counted.
var requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_events_http_requests_total",
	Help: "API sources' HTTP sends by answer: ok, or the error's code (rate_limited, payload_too_large, unsupported_media_type, " +
		"unauthorized, invalid_token, not_found, request_timeout, invalid_arguments, failed_precondition, resource_exhausted, " +
		"backend_unavailable or internal).",
}, []string{"code"})

// reminderRequestsTotal counts the reminders' answers by route (create or stop) and code: ok for a
// 200, or the error body's code.
var reminderRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_events_reminder_requests_total",
	Help: "The site's reminder requests by route (create, stop) and answer: ok, or the error's code (rate_limited, payload_too_large, " +
		"unsupported_media_type, unauthorized, request_timeout, invalid_arguments, not_found, backend_unavailable or internal).",
}, []string{"route", "code"})

// sharedRequestsTotal counts the shared dashboards' answers by code: ok for a 200, or the error
// body's code.
var sharedRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_events_shared_dashboard_requests_total",
	Help: "Requests for dashboards' public snapshots by answer: ok, or the error's code (rate_limited, not_found, " +
		"backend_unavailable or internal).",
}, []string{"code"})

// slackRequestsTotal counts Slack's requests by route (events or interactivity), class (bot,
// reply or feed, as slack.Classify sorts them; none before the body is read) and outcome: ok for a
// 2xx, unverified for the backend's 401 (a signature it found false), disabled for its 404
// (foundry.slack off where it runs), refused for its other 4xx, gap_recorded or gap_lost for an
// event of the reply or feed class answered 200 on Slack's last retry, its gap held or not, or the
// door's error code (rate_limited, 429 for a click and 503 for an event; payload_too_large,
// unsigned, stale, request_timeout, invalid_arguments, backend_unavailable or internal, each 5xx of
// the two last). Past 95% failed for an hour, Slack turns the app's events off for every install:
// alert on the 5xx well before, on a class's rate_limited share, and on any gap_lost.
var slackRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_slack_requests_total",
	Help: "Slack's requests to Helix Foundry's Slack app at the door, by route (events, interactivity), class (bot, reply, feed, " +
		"none) and outcome: ok, unverified, disabled, refused, gap_recorded, gap_lost, or the door's code (rate_limited, " +
		"payload_too_large, unsigned, stale, request_timeout, invalid_arguments, backend_unavailable or internal; the last two " +
		"answered 500).",
}, []string{"route", "class", "outcome"})

// feedGapsTotal counts the gaps of Slack events lost on Slack's last retry, by outcome: held
// until the next flush, flushed to the backend, retried (held again after a flush failed), lost
// (the pod's gaps full, or refused by the backend for good), unattributed (no team to name, or a
// channel no feed reads) or unverified (refused for its own address's room or before its body was
// read: no gap is held for what anyone could send).
var feedGapsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_slack_feed_gaps_total",
	Help: "Minutes of channels whose Slack events the door answered on Slack's last retry without the backend taking them, by " +
		"outcome: held, flushed, retried, lost, unattributed or unverified.",
}, []string{"outcome"})

// relayRequestsTotal counts the relay's answers to linked installs by route (token, claim, slack,
// slack_install, revoke) and code: ok for a 200, or the error body's code (rate_limited also for a
// call Slack takes later).
var relayRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_events_relay_requests_total",
	Help: "Linked installs' calls to Helix's Slack relay by route (token, claim, slack, slack_install, revoke) and answer: ok, or the " +
		"error's code (rate_limited, payload_too_large, unsupported_media_type, unauthorized, invalid_token, forbidden, not_found, " +
		"failed_precondition, resource_exhausted, request_timeout, invalid_arguments, backend_unavailable or internal).",
}, []string{"route", "code"})
