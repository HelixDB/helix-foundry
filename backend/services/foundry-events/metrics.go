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
