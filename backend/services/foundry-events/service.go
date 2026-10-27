// Package foundryevents is the foundry-events role: the HTTP door to Foundry's API sources
// (connector kind events), beside the MCP's send tool. It serves one route:
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
// {"error": {"code", "message", "piece"?, "reason"?}} (handler.go lists them). Another method on
// the route is 405, and another path 404, from the mux.
//
// The role holds nothing: no Secret, database, AWS identity or service-account token. It reaches
// only FoundryEventsService (ValidateFoundryEventSourceToken and SendFoundryEvents), and stores
// nothing of its own: its buckets and token cache live in memory per pod.
package foundryevents

import (
	"context"
	"errors"
	"log/slog"
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
	ep.RegisterHTTPHandler("/", newHandler(d.Backend, cfg, logger, time.Now).routes())
	return &Service{ep: ep}, nil
}

func (s *Service) Name() string { return Name }

func (s *Service) Start(ctx context.Context) error { return s.ep.Serve(ctx) }

// Stop closes the listeners and waits, within ctx, for the requests in flight to be answered.
func (s *Service) Stop(ctx context.Context) error { return s.ep.GracefulStop(ctx) }
