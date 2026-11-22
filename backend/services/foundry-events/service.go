// Package foundryevents is the foundry-events role: Foundry's public HTTP door, to its API
// sources (connector kind events), beside the MCP's send tool, for the reminders people ask for
// on the landing page, for dashboards' public links, and for Slack's requests to Helix Foundry's
// Slack app. It serves four kinds of route. The first is a send's:
//
//	POST /v1/sources/{event_source_id}/pieces
//	Authorization: Bearer hx_ev_…
//	Content-Type: application/json
//
//	{"client_token": "…", "pieces": [{"id": "…", "occurred_at": "…", "data": {…}}]}
//
// It checks a send as the backend would and forwards it to FoundryEventsService.SendFoundryEvents,
// which keeps the limits, the caps and the writer. client_token is required, so a sender can
// always retry a 429, 502, 503 or 504 with the same body: a stored send is answered again, never
// stored twice. A 200 is the MCP's send answer (Sent); every other answer the handler gives is
// {"error": {"code", "message", "piece"?, "reason"?}} (handler.go lists them).
//
// The second is the site's, for the reminders a person asks for from a phone to try Foundry on
// a computer, and stops once they sign in (reminders.go):
//
//	POST /v1/reminders       {"email": "…", "time_zone": "Europe/London", "consent": "remind-v1"}
//	POST /v1/reminders/stop  {"email": "…"}
//	Authorization: Bearer <the site's secret>
//
// It forwards them to FoundryEventsService.CreateFoundryReminder and StopFoundryReminders with the
// SHA-256 of the bearer, which the backend checks.
//
// The third is a dashboard's public snapshot, for the site's page at its link (shared.go):
//
//	GET /v1/shared-dashboards/{token}
//
// It forwards the token's SHA-256 to FoundryEventsService.GetSharedFoundryDashboard and answers
// the snapshot, readable by any page.
//
// The fourth is Slack's, while foundry.slack is on (slack.go): the Events API and clicks, sent to
// Helix Foundry's Slack app,
//
//	POST /v1/slack/events          (JSON)
//	POST /v1/slack/interactivity   (a form: payload=…)
//	X-Slack-Signature: v0=…
//	X-Slack-Request-Timestamp: …
//
// behind gates of their own, one per class of request (slack.Classify: the bot's, replies in
// threads, and the data connector's channel messages), so a flood of one never refuses another.
// The door refuses headers no signed request carries, or a time outside Slack's five minutes, and
// forwards the raw body and headers to FoundryEventsService.ReceiveSlackRequest, which verifies
// the signature with the app's signing secret, and answers what it answers, url_verification's
// challenge included. A channel's event Slack is about to drop on its last retry is answered 200,
// and the minute of its channel sent, once a minute, to
// FoundryEventsService.RecordFoundrySlackFeedGaps, for the data connector to read again.
//
// The fifth is Helix's Slack relay, while foundry.slack and its relay are on (relay.go): standalone
// Foundry installs linked to a Helix workspace redeem their link's code, claim what Slack sent
// their installs, call Slack as them, start installs and unlink, bearing their hx_rl_ token,
//
//	POST /v1/relay/token, /v1/relay/claim, /v1/relay/slack/{method}, /v1/relay/slack-install, /v1/relay/revoke
//
// behind a gate of their own, a claim waiting in a place of its own, and forwarded to
// FoundryEventsService's relay methods with the token's SHA-256.
//
// Another method on a route is 405, and another path 404, from the mux.
//
// The role holds nothing: no Secret, database, AWS identity or service-account token; not even the
// site's secret or Slack's signing secret, only passing the site's hash and Slack's signature on. It reaches only FoundryEventsService, and stores
// nothing of its own: its buckets, token cache and Slack's gaps live in memory per pod, the gaps
// sent once more as it stops.
package foundryevents

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.uber.org/dig"

	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
)

// Name is the service's name in HELIX_SERVICES, and the name of its backend client.
const Name = "foundry-events"

const (
	// readFor bounds how long a request's body takes to arrive, from when the door takes the
	// request: a slow client holds its place in flight, or its connection, no longer.
	readFor = 15 * time.Second
	// sendFor bounds the backend's SendFoundryEvents, below the process's shutdown grace (30 s),
	// so a send in flight when the pod is told to stop is answered before it exits. With readFor
	// it bounds a request to 40 s: the load balancer in front must let a deregistered pod's
	// requests finish for longer; raise that with either.
	sendFor = 25 * time.Second
	// lookupFor bounds a send token's lookup, one indexed read on the backend.
	lookupFor = 5 * time.Second
)

type Deps struct {
	dig.In

	Cfg     *config.Config
	Logger  *slog.Logger
	Health  endpoint.Health
	Backend foundryv1.FoundryEventsServiceClient `name:"foundry-events"`
}

type Service struct {
	ep *endpoint.Endpoint
	// slack is Slack's door, nil while foundry.slack is off, whose gaps the service flushes.
	slack *slackDoor
	// flushing is the flush of Slack's gaps while the service runs.
	flushing sync.WaitGroup
}

// New serves the route on foundry.events.endpoint's listeners, in front of the client of
// FoundryEventsService, which every call reaches as an event source's sender. It needs the
// client and its address; the config's bounds are checked by config.Validate where the role runs.
func New(d Deps) (*Service, error) {
	cfg := d.Cfg.Foundry.Events
	if d.Backend == nil || cfg.Endpoint.Backend.Address == "" {
		return nil, errors.New("foundry-events: foundry.events.endpoint.backend.address and its client are required")
	}
	logger := d.Logger.With("service", Name)
	ep, err := endpoint.New(endpoint.Options{
		Name: Name, Logger: logger, Health: d.Health,
		GRPC: endpoint.GRPC(cfg.Endpoint.GRPC), HTTP: endpoint.HTTP(cfg.Endpoint.HTTP), BuildGateway: false,
	})
	if err != nil {
		return nil, err
	}
	h := newHandler(d.Backend, cfg, d.Cfg.Foundry.Slack, logger, time.Now)
	ep.RegisterHTTPHandler("/", h.routes())
	return &Service{ep: ep, slack: h.slack}, nil
}

func (s *Service) Name() string { return Name }

// Start serves until ctx ends, flushing Slack's gaps every gapFlushEvery meanwhile.
func (s *Service) Start(ctx context.Context) error {
	if s.slack != nil {
		s.flushing.Go(func() { s.slack.flushGapsEvery(ctx, gapFlushEvery) })
	}
	return s.ep.Serve(ctx)
}

// Stop closes the listeners and waits, within ctx, for the requests in flight to be answered,
// then for Start's flushes to end, and sends the gaps Slack's requests left once more.
func (s *Service) Stop(ctx context.Context) error {
	err := s.ep.GracefulStop(ctx)
	if s.slack == nil {
		return err
	}
	flushed := make(chan struct{})
	go func() {
		s.flushing.Wait()
		close(flushed)
	}()
	select {
	case <-flushed:
	case <-ctx.Done():
	}
	s.slack.flushGaps(ctx)
	return err
}
