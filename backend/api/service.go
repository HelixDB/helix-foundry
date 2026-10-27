package api

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/ai/detect"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/ontologygen"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundryreadv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundryread/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// Service implements FoundryService and FoundryEventsService.
type Service struct {
	deps *runtimeDeps
	// foundryRead is the connection to foundry-read, closed by Close; nil without one.
	foundryRead *grpc.ClientConn
}

var (
	_ foundryv1.FoundryServiceServer       = (*Service)(nil)
	_ foundryv1.FoundryEventsServiceServer = (*Service)(nil)
)

// runtimeDeps is the service's resolved dependency set.
type runtimeDeps struct {
	Cfg *config.Config
	DB  foundrystore.Store
	// Plans is nil where no plan is sold.
	Plans ports.Plans
	// Catalog is the plan catalog, set whether or not any plan is sold.
	Catalog          *foundryplan.Catalog
	ConnectorCatalog *connector.Catalog
	// ConnectorKeys seals connector credentials and workspaces' model keys.
	ConnectorKeys *secretbox.Keyring
	// AI is the deployment's foundry.ai: the default and the kinds offered.
	AI *ai.Deployment
	// AIPlans is Plans' answer to whether a workspace may bring its own
	// model key, kept a while for the AI settings page, which polls.
	AIPlans *ai.Plans
	// AIResolver resolves the hosts of model endpoints owners type, which a
	// save refuses when they resolve inward; nil is the system's.
	AIResolver *net.Resolver
	// Documents is nil without a data bucket; the document routes are then Unavailable.
	Documents foundryDocumentStore
	// GenerationTitles titles ontology generations through foundry-read; without
	// it, with the goal's first sentence.
	GenerationTitles *ontologygen.Titler
	// FoundryRead reads Foundry loads; nil without backend.foundry_read.address,
	// and graph reads are then Unavailable.
	FoundryRead foundryreadv1.FoundryReadClient
	// EventSends is the in-memory state of event sources' sends, and their
	// batch objects' store; nil without a data bucket, and sends are then
	// Unavailable.
	EventSends *eventSends
	Databases  ports.Databases
	Graph      ports.GraphReader
	People     ports.People
	// LocalAI is nil hosted.
	LocalAI *detect.Detector
}

// New builds the API: its connector keyring and catalog, AI deployment,
// plan catalog, foundry-read client (connected lazily) and event sends.
func New(d Deps) (*Service, error) {
	if d.DB == nil || d.Cfg == nil {
		return nil, errors.New("api: store and configuration are required")
	}
	cfg := d.Cfg
	connectorKeys, err := connector.NewKeyring(cfg.Foundry.Connectors)
	if err != nil {
		return nil, err
	}
	aiDeployment, err := ai.NewDeployment(cfg)
	if err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}
	var connectorCatalog *connector.Catalog
	if connectorKeys != nil {
		// The API only tests and discovers: its events kind reads no batches.
		connectorCatalog = connector.NewCatalog(connector.DefaultEndpoints(),
			connector.Guard{AllowPrivate: cfg.Foundry.Connectors.AllowPrivateHosts}, connector.NewS3Options(cfg, d.AWS), connector.EventsOptions{})
	}
	catalog, err := foundryplan.NewCatalog(cfg.Billing.Plans, connector.KindNames())
	if err != nil {
		return nil, err
	}
	s := &Service{deps: &runtimeDeps{
		Cfg: cfg, DB: d.DB, Plans: d.Plans, Catalog: catalog, ConnectorCatalog: connectorCatalog, ConnectorKeys: connectorKeys,
		AI: aiDeployment, AIPlans: ai.NewPlans(d.Plans), Databases: d.Databases, Graph: d.Graph, People: d.People, LocalAI: d.LocalAI,
	}}
	if d.Documents != nil {
		s.deps.Documents = d.Documents
	}
	if bucket := cfg.Foundry.Sync.DataBucket; bucket != "" {
		// Event sources' batch objects share the bucket with the snapshots.
		s.deps.EventSends = newEventSends(cfg, snapshot.NewS3(s3.NewFromConfig(d.AWS), bucket), time.Now)
	}
	if addr := cfg.Read.Address; addr != "" {
		// Connects lazily: foundry-read may start after the API.
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, fmt.Errorf("api: foundry-read client for %s: %w", addr, err)
		}
		s.foundryRead, s.deps.FoundryRead = conn, foundryreadv1.NewFoundryReadClient(conn)
	}
	// foundry-read writes titles on each workspace's AI setting: the API
	// makes no model call.
	s.deps.GenerationTitles = ontologygen.NewTitler(s.deps.FoundryRead, d.Logger)
	return s, nil
}

// Register serves FoundryService and FoundryEventsService on srv. Install
// ActorInterceptor on srv, and keep srv off any public network: every call
// is trusted as its Actor.
func (s *Service) Register(srv *grpc.Server) {
	foundryv1.RegisterFoundryServiceServer(srv, s)
	foundryv1.RegisterFoundryEventsServiceServer(srv, s)
}

// Close closes the foundry-read connection.
func (s *Service) Close() error {
	if s.foundryRead == nil {
		return nil
	}
	return s.foundryRead.Close()
}
