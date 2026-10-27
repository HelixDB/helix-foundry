// Package foundryread serves helix.foundryread.v1.FoundryRead: reads of
// Foundry ontology loads for the backend, and generations' titles. Its pod
// runs an ontology agent sidecar of the read profile (no secrets, no work
// volume); per read the service binds the ontology's database, registers a
// token on the pod's one read-only proxy for the call's life, and calls the
// sidecar's Read, whose Helix requests go through that proxy. Per title it
// resolves the workspace's AI setting and calls the sidecar's Title, the
// key in the call's metadata. Rule evaluation is CPU heavy, so it runs in
// its own role rather than in the backend's pod.
package foundryread

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.uber.org/dig"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryreadv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundryread/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// Name is the service's name in HELIX_SERVICES.
const Name = "foundry-read"

type Deps struct {
	dig.In

	Cfg    *config.Config
	Logger *slog.Logger
	Health endpoint.Health
	DB     foundrystore.Store `optional:"true"`
	// Databases are the databases loads are read from.
	Databases ports.Databases `optional:"true"`
	// Plans says which workspaces' plans let them title on their own provider.
	Plans ports.PlanReader `optional:"true"`
}

type Service struct {
	ep    *endpoint.Endpoint
	proxy *graphproxy.ReadProxy
	agent *ontologyagent.Client
}

// New dials the sidecar, starts the pod's read-only proxy and registers the
// reader. Nothing authenticates callers: the role must be reachable only
// from the backend's pods (no ingress, a network policy on its gRPC port),
// and the backend checks the caller may read the ontology's database or create
// the generation it titles.
func New(d Deps) (*Service, error) {
	if d.DB == nil || d.Databases == nil {
		return nil, errors.New("foundry-read: db provider not configured")
	}
	cfg := d.Cfg.Foundry.Read
	resolver, err := ai.NewResolver(d.Cfg, d.DB, d.Plans)
	if err != nil {
		return nil, fmt.Errorf("foundry-read: %w", err)
	}
	agent, err := ontologyagent.Dial(cfg.AgentAddr)
	if err != nil {
		return nil, err
	}
	gateway := &helixgateway.Client{
		HTTP:             helixgateway.NewHTTPClient(helixTimeoutSecs*time.Second + 5*time.Second),
		ProvisionerKey:   d.Cfg.Host.GatewayKey,
		Keyless:          d.Cfg.Host.Local,
		MaxResponseBytes: graphproxy.MaxResponseBytes,
	}
	proxy, err := graphproxy.ServeReadOnly(gateway, admissionWait)
	if err != nil {
		_ = agent.Close()
		return nil, fmt.Errorf("foundry-read: %w", err)
	}
	ep, err := endpoint.New(endpoint.Options{
		Name: Name, Logger: d.Logger.With("service", Name), Health: d.Health,
		GRPC: endpoint.GRPC(cfg.GRPC), HTTP: endpoint.HTTP(cfg.HTTP), BuildGateway: false,
	})
	if err != nil {
		_ = proxy.Close()
		_ = agent.Close()
		return nil, err
	}
	foundryreadv1.RegisterFoundryReadServer(ep.GRPC, &reader{
		db: d.DB, dbs: d.Databases, cfg: d.Cfg, agent: agent, ai: resolver, proxy: proxy, limit: newWorkspaceLimit(MaxReadsPerWorkspace),
		titles: newWorkspaceLimit(MaxTitlesPerWorkspace), now: time.Now,
	})
	return &Service{ep: ep, proxy: proxy, agent: agent}, nil
}

func (s *Service) Name() string { return Name }

func (s *Service) Start(ctx context.Context) error { return s.ep.Serve(ctx) }

// Stop lets the reads in flight finish, then closes the proxy and the sidecar's connection.
func (s *Service) Stop(ctx context.Context) error {
	err := s.ep.GracefulStop(ctx)
	_ = s.proxy.Close() // best-effort: the process is ending
	_ = s.agent.Close()
	return err
}
