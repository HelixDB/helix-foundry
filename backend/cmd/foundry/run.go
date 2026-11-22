package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	gomysql "github.com/go-sql-driver/mysql"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/dig"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	foundryagent "github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/ai/detect"
	"github.com/helixdb/helix-foundry/backend/api"
	foundryconfig "github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/local"
	"github.com/helixdb/helix-foundry/backend/local/directory"
	"github.com/helixdb/helix-foundry/backend/local/relay"
	directoryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/directory/v1"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	foundryreadservice "github.com/helixdb/helix-foundry/backend/services/foundry-read"
	"github.com/helixdb/helix-foundry/backend/slackbot"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/syncer"
	foundryagentworker "github.com/helixdb/helix-foundry/backend/workers/foundry-agent-worker"
	foundryaskworker "github.com/helixdb/helix-foundry/backend/workers/foundry-ask-worker"
	foundryautomationworker "github.com/helixdb/helix-foundry/backend/workers/foundry-automation-worker"
	foundrydocumentworker "github.com/helixdb/helix-foundry/backend/workers/foundry-document-worker"
	foundrygraphworker "github.com/helixdb/helix-foundry/backend/workers/foundry-graph-worker"
	foundryhealworker "github.com/helixdb/helix-foundry/backend/workers/foundry-heal-worker"
	foundryontologyworker "github.com/helixdb/helix-foundry/backend/workers/foundry-ontology-worker"
	foundryslackworker "github.com/helixdb/helix-foundry/backend/workers/foundry-slack-worker"
	foundrysyncworker "github.com/helixdb/helix-foundry/backend/workers/foundry-sync-worker"
)

// shutdownGrace is how long the install's parts get to stop.
const shutdownGrace = 30 * time.Second

// component is one part of the install that runs until stopped: the front
// end, foundry-read and each worker.
type component interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// workers are the parts an install runs beside its front end: foundry-read,
// which the API asks for graph reads and titles, and the workers of syncs,
// documents, drafts, loads and questions. People enrichment's worker is not
// one: an install has no lookup provider.
var workers = []any{
	foundryreadservice.New,
	foundrysyncworker.New,
	foundrydocumentworker.New,
	foundryontologyworker.New,
	foundrygraphworker.New,
	foundryaskworker.New,
}

// healWorker audits loads for self-healing's findings, beside the graph
// worker on its sidecar, only with foundry.heal.enabled (off by default).
var healWorker = foundryhealworker.New

// agentWorker runs the Assistant's turns, with foundry.agent.enabled (on in
// the image's foundry.yaml).
var agentWorker = foundryagentworker.New

// automationWorker fires the owner's automations and walks their runs, with
// foundry.automations.enabled (on in the image's foundry.yaml).
var automationWorker = foundryautomationworker.New

// slackWorker runs the Slack bot on what Helix's relay delivers, with
// foundry.slack.enabled (on in the image's foundry.yaml).
var slackWorker = foundryslackworker.New

// run starts the install and blocks until ctx is cancelled, SIGINT or
// SIGTERM arrives, or a part exits, returning the first error.
//
//  1. Open the database, migrate it (the directory's set, then Foundry's) and
//     write the owner's row, their first workspace (until they have one)
//     and the bucket.
//  2. Without a configured AI provider, look for one on this machine
//     (package ai/detect), so model work starts on it.
//  3. Build the link to Helix's Slack relay (with foundry.slack.enabled),
//     the front end (the directory and FoundryService, behind the site
//     token), foundry-read and the workers.
//  4. Start them, then report ready on the health listener.
//  5. On shutdown, stop the workers, then the front end, within shutdownGrace.
func run(parent context.Context, cfg *Config) error {
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger := log.New(cfg.Log.Format, cfg.Log.Level)
	// Code that logs with no logger of its own, such as each call's
	// grpc.unary line, logs in the same format and level.
	slog.SetDefault(logger)
	ctx = log.Into(ctx, logger)
	fcfg := cfg.foundry()

	db, err := openDB(ctx, cfg.DB.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	logger.Info("running db migrations")
	if err := local.Migrate(ctx, db); err != nil {
		return err
	}
	dir := directory.NewStore(db)
	if err := dir.SeedOwner(ctx); err != nil {
		return err
	}
	directorySvc := directory.NewService(dir, cfg.Local.HelixURL, helixgateway.NewHTTPClient(directory.QueryTimeout))
	switch seeded, err := directorySvc.SeedWorkspace(ctx); {
	case err != nil:
		return err
	case seeded != nil:
		logger.Info("created the owner's first workspace", "workspace", seeded.GetId(), "name", seeded.GetDisplayName())
	}
	awsCfg := local.AWSConfig(cfg.AWS.Region, cfg.Local.S3)
	if err := local.CreateBucket(ctx, s3.NewFromConfig(awsCfg), fcfg.Foundry.Sync.DataBucket); err != nil {
		return err
	}
	detector, err := newDetector(fcfg, logger, os.Getenv)
	if err != nil {
		return err
	}
	if detector != nil {
		detector.Detect(ctx)
	}

	st := store.NewMySQL(db, local.Workspaces{})
	probes := newProbes(db.PingContext, logger)
	link, err := newRelay(cfg, fcfg, logger, dir, st)
	if err != nil {
		return err
	}
	f, err := newFront(ctx, cfg, logger, probes, directorySvc, dir, st, awsCfg, detector, link)
	if err != nil {
		return err
	}
	dbs := local.Databases{Directory: dir, HelixURL: cfg.Local.HelixURL}
	var relayed *slackbot.Relayed
	if link != nil {
		relayed = link.Transport()
	}
	parts, err := buildWorkers(fcfg, logger, probes, st, awsCfg, dbs, local.People{Directory: dir}, f.sessions, relayed)
	if err != nil {
		_ = f.Stop(ctx)
		return err
	}
	// The relay runs whether or not the install is linked: unlinked, it only
	// reads whether it was.
	if link != nil {
		parts = append(parts, link)
	}
	healthLn, err := net.Listen("tcp", cfg.Local.Listen.Health)
	if err != nil {
		_ = f.Stop(ctx)
		return fmt.Errorf("health listen %s: %w", cfg.Local.Listen.Health, err)
	}
	return serve(ctx, logger, probes, healthLn, append([]component{f}, parts...))
}

// newRelay is the install's link to Helix's Slack relay, with
// foundry.slack.enabled; nil without. Helix names the install by this
// machine's name.
func newRelay(cfg *Config, fcfg *foundryconfig.Config, logger *slog.Logger, dir *directory.Store, st store.Store) (*relay.Relay, error) {
	if !fcfg.Foundry.Slack.Enabled {
		return nil, nil
	}
	keys, err := connector.NewKeyring(fcfg.Foundry.Connectors)
	if err != nil {
		return nil, err
	}
	name, err := os.Hostname()
	if name = strings.TrimSpace(name); err != nil || name == "" {
		name = "Foundry"
	}
	if len(name) > 128 {
		name = name[:128]
	}
	return relay.New(relay.Options{Directory: dir, DB: st, Keys: keys, Door: cfg.Local.Relay.URL, Site: cfg.Local.Relay.SiteURL,
		Origin: strings.TrimRight(cfg.Local.SiteOrigin, "/"), Name: name, Slack: fcfg.Foundry.Slack, Logger: logger})
}

// newDetector is the look for an AI default on this machine, of an install
// that configures none (Host.DetectedAI), or nil: the keys foundry was
// started with, the bundled Ollama, this computer's, and LM Studio.
func newDetector(fcfg *foundryconfig.Config, logger *slog.Logger, getenv func(string) string) (*detect.Detector, error) {
	if fcfg.Host.DetectedAI == nil {
		return nil, nil
	}
	d, err := ai.NewDeployment(fcfg)
	if err != nil {
		return nil, err
	}
	return &detect.Detector{
		AnthropicKey: getenv("ANTHROPIC_API_KEY"), OpenAIKey: getenv("OPENAI_API_KEY"),
		Check: d.CheckDefault, Live: fcfg.Host.DetectedAI, Logger: logger.With("component", "ai-detect"),
	}, nil
}

// openDB opens the MySQL database at dsn as Foundry's store expects it.
func openDB(ctx context.Context, dsn string) (*sql.DB, error) {
	mc, err := gomysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse db.dsn: %w", err)
	}
	// Times scan as time.Time; parameters are interpolated, so statements
	// run on the text protocol; and an UPDATE reports the rows it matched,
	// so a guarded write that changes nothing is not taken for a missing
	// row.
	mc.ParseTime, mc.InterpolateParams, mc.ClientFoundRows = true, true, true
	connector, err := gomysql.NewConnector(mc)
	if err != nil {
		return nil, fmt.Errorf("configure mysql: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	// The database may still be starting: wait for it a while.
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err == nil || attempt == 30 || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return db, nil
}

// front is the install's front end: one gRPC server with the directory and
// FoundryService behind the site token, their REST gateway, which is all the
// site calls, and the MCP and HTTP Ask the owner's agents call beside it.
type front struct {
	ep  *endpoint.Endpoint
	api *api.Service
	// sessions open the owner's tools for the Assistant, in this process.
	sessions *local.AgentSessions
	// turns are the helpers' side of the Assistant's turns, nil while it is
	// off; hosts the install's helpers.
	turns  *foundryagent.Hosts
	hosts  *directory.Store
	logger *slog.Logger
}

// sweepUnclaimedEvery is how often the front end fails helpers' turns that
// no helper took (agent.Hosts.EndUnclaimed).
const sweepUnclaimedEvery = 30 * time.Second

// newFront builds the front end over the install's directory and Foundry's
// store, its objects kept in the install's bucket (foundry.sync.data_bucket),
// with the look for an AI default, if the install has one, and its link to
// Helix's Slack relay, if Slack is on (link).
// The site token's interceptor sets each call's Actor, so FoundryService is
// served without api.ActorInterceptor: nothing stands between the site and
// it.
func newFront(ctx context.Context, cfg *Config, logger *slog.Logger, probes *probes, directorySvc *directory.Service, dir *directory.Store,
	st store.Store, awsCfg aws.Config, detector *detect.Detector, link *relay.Relay) (*front, error) {
	auth, err := local.NewAuthenticator(cfg.Local.SiteToken, dir, st, logger.With("component", "auth"))
	if err != nil {
		return nil, err
	}
	fcfg := cfg.foundry()
	ep, err := endpoint.New(endpoint.Options{
		Name: "foundry", Logger: logger.With("service", "foundry"), Health: probes,
		GRPC: endpoint.GRPC{Listen: cfg.Local.Listen.GRPC, MaxRecvMsgMB: 16}, HTTP: endpoint.HTTP{Listen: cfg.Local.Listen.REST},
		Interceptor: auth, BuildGateway: true,
		// local/compose.yaml publishes REST on this machine's loopback for
		// agents and the Assistant's helper; the probes stay on the health
		// listener (serve), which isn't published.
		NoProbes: true,
	})
	if err != nil {
		return nil, err
	}
	// The Assistant's tools call this front end's gRPC as the owner, as the
	// MCP's do; its clicks run them from the API, its turns from the worker.
	sessions := local.NewAgentSessions(auth, local.LocalService(dir, ep.GatewayConn, fcfg.Foundry.Documents, fcfg.Foundry.Automations.Enabled, cfg.Local.SiteOrigin,
		logger.With("component", "agent")), foundryv1.NewFoundryServiceClient(ep.GatewayConn), directoryv1.NewDirectoryServiceClient(ep.GatewayConn),
		cfg.Local.SiteToken)
	dbs := local.Databases{Directory: dir, HelixURL: cfg.Local.HelixURL}
	// A nil link must stay a nil interface: the API refuses the relay's
	// methods without one.
	var linker api.RelayLinker
	if link != nil {
		linker = link
	}
	foundrySvc, err := api.New(api.Deps{
		Cfg: fcfg, Logger: logger.With("service", "foundry"), DB: st, AWS: awsCfg,
		Documents: document.New(s3.NewFromConfig(awsCfg), fcfg.Foundry.Sync.DataBucket),
		Databases: dbs,
		Graph:     local.GraphReader{Directory: directorySvc}, People: local.People{Directory: dir},
		LocalAI: detector, AgentSessions: sessions, AgentHosts: dir,
		AgentHostURL: fmt.Sprintf("http://127.0.0.1:%d", cfg.Local.MCPPort), Relay: linker,
	})
	if err != nil {
		_ = ep.GracefulStop(ctx)
		return nil, err
	}
	f := &front{ep: ep, api: foundrySvc, sessions: sessions, hosts: dir, logger: logger.With("component", "agent")}
	directoryv1.RegisterDirectoryServiceServer(ep.GRPC, directorySvc)
	foundrySvc.Register(ep.GRPC)
	for _, register := range []func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error{
		directoryv1.RegisterDirectoryServiceHandler, foundryv1.RegisterFoundryServiceHandler,
	} {
		if err := register(ctx, ep.Gateway, ep.GatewayConn); err != nil {
			_ = f.Stop(ctx)
			return nil, err
		}
	}
	// The helpers on the owner's machine run the Assistant's threads on their
	// CLIs' subscriptions, and call back to this listener (local/agent_host.go).
	var turns *foundryagent.Hosts
	if fcfg.Foundry.Agent.Enabled {
		keys, err := connector.NewKeyring(fcfg.Foundry.Connectors)
		if err != nil {
			_ = f.Stop(ctx)
			return nil, err
		}
		turns = &foundryagent.Hosts{DB: st, Sessions: sessions, Keys: keys, Databases: dbs, Cfg: fcfg.Foundry.Agent, Now: time.Now}
		f.turns = turns
	}
	integrations := local.IntegrationHandler(auth, dir, ep.GatewayConn, fcfg.Foundry.Documents, fcfg.Foundry.Automations.Enabled, cfg.Local.SiteOrigin, turns, logger.With("component", "integration"))
	ep.RegisterHTTPHandler("/mcp", integrations)
	ep.RegisterHTTPHandler("/v1/ontologies/", integrations)
	ep.RegisterHTTPHandler("/agent-host/", integrations)
	return f, nil
}

func (f *front) Name() string { return "foundry" }

func (f *front) Start(ctx context.Context) error {
	if f.turns != nil {
		go f.sweepUnclaimed(ctx)
	}
	return f.ep.Serve(ctx)
}

// sweepUnclaimed fails, every sweepUnclaimedEvery until ctx ends, the
// helpers' turns that waited too long while no helper that runs them is
// online, so their threads don't wait for ever.
func (f *front) sweepUnclaimed(ctx context.Context) {
	tick := time.NewTicker(sweepUnclaimedEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := f.turns.EndUnclaimed(ctx, f.hosts); err != nil && ctx.Err() == nil {
			f.logger.Warn("agent: end unclaimed helper turns", "err", err)
		}
	}
}

func (f *front) Stop(ctx context.Context) error {
	return errors.Join(f.ep.GracefulStop(ctx), f.api.Close())
}

// buildWorkers builds foundry-read and the workers from what the install
// provides them: Foundry's configuration, the logger, the health, the store,
// the object store, the databases, who the install's people are (people:
// the ask worker matches a grantee to its sources by email), and, with Slack
// on, the way to Slack through Helix's relay (relayed), for the Slack bot
// and the Slack data connector's syncs. Nothing else is provided, so the optional ports an
// install lacks are nil: no plan is sold (PlanReader) and nothing is
// boosted (Boosts).
func buildWorkers(cfg *foundryconfig.Config, logger *slog.Logger, probes *probes, st store.Store, awsCfg aws.Config,
	dbs ports.Databases, people ports.People, sessions integration.SessionHost, relayed *slackbot.Relayed) ([]component, error) {
	c := dig.New()
	for _, provide := range []any{
		func() *foundryconfig.Config { return cfg },
		func() *slog.Logger { return logger },
		func() endpoint.Health { return probes },
		func() store.Store { return st },
		func() aws.Config { return awsCfg },
		func() ports.Databases { return dbs },
		func() ports.People { return people },
		func() integration.SessionHost { return sessions },
	} {
		if err := c.Provide(provide); err != nil {
			return nil, err
		}
	}
	if relayed != nil {
		for _, provide := range []any{
			func() *slackbot.Relayed { return relayed },
			func() syncer.SlackCallers { return relayed.Callers },
		} {
			if err := c.Provide(provide); err != nil {
				return nil, err
			}
		}
	}
	constructors := slices.Clip(workers)
	if cfg.Foundry.Heal.Enabled {
		constructors = append(constructors, healWorker)
	}
	if cfg.Foundry.Agent.Enabled {
		constructors = append(constructors, agentWorker)
	}
	if cfg.Foundry.Automations.Enabled {
		constructors = append(constructors, automationWorker)
	}
	if cfg.Foundry.Slack.Enabled {
		constructors = append(constructors, slackWorker)
	}
	for _, constructor := range constructors {
		if err := c.Provide(constructor, dig.Group("components"), dig.As(new(component))); err != nil {
			return nil, err
		}
	}
	var parts []component
	err := c.Invoke(func(in struct {
		dig.In
		Parts []component `group:"components"`
	}) {
		parts = in.Parts
	})
	if err != nil {
		return nil, fmt.Errorf("build workers: %w", err)
	}
	return parts, nil
}

// serve runs parts and the health listener until ctx ends or one of them
// exits, then stops them, the front end (parts[0]) last.
func serve(ctx context.Context, logger *slog.Logger, probes *probes, healthLn net.Listener, parts []component) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", probes.LivenessHandler())
	mux.HandleFunc("/readyz", probes.ReadinessHandler())
	mux.Handle("/metrics", promhttp.Handler())
	healthSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	eg, egCtx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		if err := healthSrv.Serve(healthLn); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("health: %w", err)
		}
		return nil
	})
	for _, p := range parts {
		eg.Go(func() error {
			logger.Info("started", "part", p.Name())
			if err := p.Start(egCtx); err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("%s: %w", p.Name(), err)
			}
			return nil
		})
	}
	probes.markReady()
	logger.Info("foundry ready")

	<-egCtx.Done()
	logger.Info("foundry shutdown", "reason", egCtx.Err())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	for i := len(parts) - 1; i >= 0; i-- {
		if err := parts[i].Stop(shutdownCtx); err != nil {
			logger.Error("stop", "part", parts[i].Name(), "err", err)
		}
	}
	_ = healthSrv.Shutdown(shutdownCtx)
	if err := eg.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
