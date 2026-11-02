package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/connector"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

const maxConnectorNameLength = 120

func (s *Service) ListConnectorKinds(ctx context.Context, req *foundryv1.ListConnectorKindsRequest) (*foundryv1.ListConnectorKindsResponse, error) {
	catalog, _, err := s.connectors(req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	plan, err := s.workspacePlan(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	plans := s.foundryCatalog()
	resp := &foundryv1.ListConnectorKindsResponse{}
	for _, k := range catalog.Kinds() {
		kind := &foundryv1.ConnectorKind{Kind: k.Name(), DisplayName: k.DisplayName(), Available: plan.Includes(k.Name())}
		if lowest := plans.LowestWith(k.Name()); lowest != nil {
			kind.PlanCode, kind.RequiresPro = lowest.Code, lowest != plans.Free()
		}
		if rs, ok := k.(connector.RoleSetup); ok {
			if principal, externalID, on := rs.Setup(req.GetWorkspaceId()); on {
				kind.Setup = &foundryv1.ConnectorKind_S3{S3: &foundryv1.S3ConnectorSetup{PrincipalArn: principal, ExternalId: externalID}}
			}
		}
		resp.Kinds = append(resp.Kinds, kind)
	}
	return resp, nil
}

func (s *Service) TestConnector(ctx context.Context, req *foundryv1.TestConnectorRequest) (*foundryv1.TestConnectorResponse, error) {
	catalog, _, err := s.connectors(req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	kind, src, _, err := sourceFromProto(catalog, req.GetConfig())
	if err != nil {
		return nil, err
	}
	settings, discovery, err := s.testSource(ctx, req.GetWorkspaceId(), kind, src)
	if err != nil {
		return nil, err
	}
	cfg, err := configToProto(kind.Name(), settings)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read connector settings: %v", err)
	}
	return &foundryv1.TestConnectorResponse{Discovery: discoveryToProto(discovery), Config: cfg}, nil
}

// CreateConnector tests the credential, seals it under the connector's id and
// stores it. An event source has no credential, and an empty one is sealed
// in its place; its workspace's lock keeps its name unique among the
// workspace's live event sources, and their count within the cap
// (createEventSource).
func (s *Service) CreateConnector(ctx context.Context, req *foundryv1.CreateConnectorRequest) (*foundryv1.ConnectorResponse, error) {
	catalog, keyring, err := s.connectors(req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	name, err := connectorName(req.GetName())
	if err != nil {
		return nil, err
	}
	kind, src, _, err := sourceFromProto(catalog, req.GetConfig())
	if err != nil {
		return nil, err
	}
	settings, discovery, err := s.testSource(ctx, req.GetWorkspaceId(), kind, src)
	if err != nil {
		return nil, err
	}
	c := &foundrymodel.Connector{
		ID: id.New(), WorkspaceID: req.GetWorkspaceId(), Kind: kind.Name(), Name: name,
		Config: settings, SecretHint: kind.Hint(src.Secret),
	}
	// A new connector syncs at the scheduler's next tick, then on its plan's
	// interval; an event source once its first pieces arrive.
	if c.Kind != foundrymodel.ConnectorKindEvents {
		now := time.Now()
		c.NextSyncAt = &now
	}
	if userID := actor(ctx).UserID; userID != "" {
		c.CreatedByUserID = &userID
	}
	c.SecretCiphertext, c.SecretKeyID, err = keyring.Seal([]byte(src.Secret), connector.SecretContext(c.ID))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "seal connector credential: %v", err)
	}
	if c.Kind == foundrymodel.ConnectorKindEvents {
		err = s.createEventSource(ctx, c)
	} else if err = s.deps.DB.CreateConnector(ctx, c); err != nil {
		err = status.Errorf(codes.Internal, "create connector: %v", err)
	}
	if err != nil {
		return nil, err
	}
	stored, err := s.deps.DB.GetConnector(ctx, c.WorkspaceID, c.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read created connector: %v", err)
	}
	return s.connectorResponse(ctx, stored, &discovery)
}

func (s *Service) ListConnectors(ctx context.Context, req *foundryv1.ListConnectorsRequest) (*foundryv1.ListConnectorsResponse, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	list, err := s.deps.DB.ListConnectors(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list connectors: %v", err)
	}
	resp := &foundryv1.ListConnectorsResponse{}
	for _, c := range list {
		pc, err := s.connectorToProto(ctx, c)
		if err != nil {
			return nil, err
		}
		resp.Connectors = append(resp.Connectors, pc)
	}
	return resp, nil
}

func (s *Service) GetConnector(ctx context.Context, req *foundryv1.GetConnectorRequest) (*foundryv1.ConnectorResponse, error) {
	c, err := s.loadConnector(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	return s.connectorResponse(ctx, c, nil)
}

// UpdateConnector renames a connector or replaces its settings. New settings
// are tested before saving; a blank credential keeps the stored one. Settings
// that point elsewhere queue a sync, which replaces the old source's tables.
// An event source takes a path of its own (updateEventSource).
func (s *Service) UpdateConnector(ctx context.Context, req *foundryv1.UpdateConnectorRequest) (*foundryv1.ConnectorResponse, error) {
	catalog, keyring, err := s.connectors(req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	current, err := s.loadConnector(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	if current.Kind == foundrymodel.ConnectorKindEvents {
		return s.updateEventSource(ctx, catalog, current, req)
	}
	var update foundrymodel.ConnectorUpdate
	if req.Name != nil {
		name, err := connectorName(req.GetName())
		if err != nil {
			return nil, err
		}
		update.Name = &name
	}
	var discovery *connector.Discovery
	settingsChanged := false
	if req.GetConfig() != nil {
		kind, src, secretless, err := sourceFromProto(catalog, req.GetConfig())
		if err != nil {
			return nil, err
		}
		if kind.Name() != current.Kind {
			return nil, status.Error(codes.InvalidArgument, "a connector's kind cannot change; create a new connector")
		}
		// A blank credential keeps the stored one, unless the access needs
		// none: then the old one is sealed over with nothing.
		kept := !secretless && src.Secret == ""
		if kept {
			secret, err := keyring.Open(current.SecretCiphertext, current.SecretKeyID, connector.SecretContext(current.ID))
			if err != nil {
				return nil, status.Errorf(codes.Internal, "open connector %s credential: %v", current.ID, err)
			}
			src.Secret = string(secret)
		}
		// Settings that send the credential to another server move it: the
		// stored credential never goes there, so moving needs it entered again.
		moved := false
		if targeted, ok := kind.(connector.Targeted); ok {
			prepared, err := kind.Prepare(src)
			if err != nil {
				return nil, connectorError(ctx, err)
			}
			was, errWas := targeted.Target(current.Config)
			now, errNow := targeted.Target(prepared)
			moved = errWas != nil || errNow != nil || was != now
		}
		if kept && moved {
			return nil, status.Error(codes.InvalidArgument, "these settings connect to a different server; enter the credential again")
		}
		settings, d, err := s.testSource(ctx, req.GetWorkspaceId(), kind, src)
		if err != nil {
			return nil, err
		}
		discovery = &d
		if settingsChanged = !sameJSON(current.Config, settings); settingsChanged {
			if _, err := s.deps.DB.GetActiveConnectorSync(ctx, req.GetWorkspaceId(), current.ID); err == nil {
				return nil, status.Error(codes.FailedPrecondition, "a sync of this connector is queued or running; change its settings once the sync ends")
			} else if !errors.Is(err, foundrystore.ErrNotFound) {
				return nil, status.Errorf(codes.Internal, "read active sync: %v", err)
			}
			if err := s.releaseOld(ctx, kind, keyring, current, src.Secret, moved); err != nil {
				return nil, err
			}
		}
		update.Config, update.SecretHint = settings, kind.Hint(src.Secret)
		update.SecretCiphertext, update.SecretKeyID, err = keyring.Seal([]byte(src.Secret), connector.SecretContext(current.ID))
		if err != nil {
			return nil, status.Errorf(codes.Internal, "seal connector credential: %v", err)
		}
	}
	if req.SyncEverySeconds != nil {
		schedule, err := s.scheduleUpdate(ctx, req.GetWorkspaceId(), req.GetSyncEverySeconds())
		if err != nil {
			return nil, err
		}
		update.Schedule = schedule
	} else if update.Config != nil {
		// Saving settings clears a pause. Changed settings queue a sync, which
		// schedules the next; a new credential alone keeps the schedule, and
		// resumes a paused connector at once.
		update.Schedule = &foundrymodel.ConnectorScheduleUpdate{SyncEverySeconds: current.SyncEverySeconds}
		manualOnly := current.SyncEverySeconds != nil && *current.SyncEverySeconds == 0
		switch {
		case settingsChanged || manualOnly:
		case current.NextSyncAt != nil:
			update.Schedule.NextSyncAt = current.NextSyncAt
		default:
			now := time.Now()
			update.Schedule.NextSyncAt = &now
		}
	}
	if update.Name == nil && update.Config == nil && update.Schedule == nil {
		return nil, status.Error(codes.InvalidArgument, "set a name, config or sync_every_seconds to update")
	}
	if err := s.deps.DB.UpdateConnector(ctx, req.GetWorkspaceId(), req.GetId(), update); err != nil {
		if errors.Is(err, foundrystore.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "connector not found")
		}
		return nil, status.Errorf(codes.Internal, "update connector: %v", err)
	}
	stored, err := s.loadConnector(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	// AlreadyExists is a sync queued since the check above; it loads the connector when it starts.
	if settingsChanged {
		if _, err := s.queueConnectorSync(ctx, stored, foundrymodel.ConnectorSyncSettings, foundrymodel.ConnectorSyncFull); err != nil &&
			status.Code(err) != codes.AlreadyExists {
			return nil, err
		}
	}
	return s.connectorResponse(ctx, stored, discovery)
}

// sameJSON compares stored settings by value, since MySQL rewrites JSON on the way in.
func sameJSON(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func (s *Service) DeleteConnector(ctx context.Context, req *foundryv1.DeleteConnectorRequest) (*emptypb.Empty, error) {
	if req.GetWorkspaceId() == "" || req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and id are required")
	}
	// Deleting the connector deletes its tables and documents, which a sync
	// still live would be writing. One that starts after this check writes
	// none past the delete: each write of a table or document locks the
	// connector's row and refuses a deleted connector.
	if _, err := s.deps.DB.GetActiveConnectorSync(ctx, req.GetWorkspaceId(), req.GetId()); err == nil {
		return nil, status.Error(codes.FailedPrecondition, "a sync of this connector is queued or running; delete it once the sync ends")
	} else if !errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "read active sync: %v", err)
	}
	err := s.deps.DB.DeleteConnector(ctx, req.GetWorkspaceId(), req.GetId())
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "connector not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete connector: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// releaseOld gives back what the connector holds under its stored settings,
// such as a replication slot, before new settings replace them. Saving forgets
// the change cursor, so no later sync would release it, and a PostgreSQL slot
// can only be dropped from the database that made it. The update's credential
// is tried first, unless the update moved to another server (the old one may
// not be the customer's), then the stored one. A source neither can reach is
// likely gone and doesn't block the change; one that refuses the drop does.
func (s *Service) releaseOld(ctx context.Context, kind connector.Kind, keyring *secretbox.Keyring, current *foundrymodel.Connector, secret string, moved bool) error {
	releaser, ok := kind.(connector.Releaser)
	if !ok {
		return nil
	}
	var secrets []string
	if !moved {
		secrets = append(secrets, secret)
	}
	if stored, err := keyring.Open(current.SecretCiphertext, current.SecretKeyID, connector.SecretContext(current.ID)); err == nil {
		secrets = append(secrets, string(stored))
	}
	secrets = slices.Compact(secrets)
	var err error
	for _, secret := range secrets {
		src := connector.Source{Settings: current.Config, Secret: secret, WorkspaceID: current.WorkspaceID, ConnectorID: current.ID}
		if err = releaser.Release(ctx, src, current.ChangeCursor); err == nil {
			return nil
		}
		var refused *connector.RefusedError
		var invalid *connector.InvalidError
		if !errors.As(err, &refused) && !errors.As(err, &invalid) {
			return status.Errorf(codes.FailedPrecondition, "the old settings' replication slot could not be dropped: %v; drop it on that database, then save again", err)
		}
	}
	log.From(ctx).Warn("release under old settings failed; saving the new ones", "connector", current.ID, "err", err)
	return nil
}

func (s *Service) connectors(workspaceID string) (*connector.Catalog, *secretbox.Keyring, error) {
	if workspaceID == "" {
		return nil, nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	if s.deps.ConnectorCatalog == nil || s.deps.ConnectorKeys == nil {
		return nil, nil, status.Error(codes.Unavailable, "foundry connectors are not configured")
	}
	return s.deps.ConnectorCatalog, s.deps.ConnectorKeys, nil
}

func (s *Service) loadConnector(ctx context.Context, workspaceID, connectorID string) (*foundrymodel.Connector, error) {
	if workspaceID == "" || connectorID == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and id are required")
	}
	c, err := s.deps.DB.GetConnector(ctx, workspaceID, connectorID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "connector not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get connector: %v", err)
	}
	return c, nil
}

// testSource gates kinds by plan, checks the settings and proves the credential.
// It returns the settings to store, which discovery may have completed.
func (s *Service) testSource(ctx context.Context, workspaceID string, kind connector.Kind, src connector.Source) (
	json.RawMessage, connector.Discovery, error) {
	if err := s.requireKind(ctx, workspaceID, kind); err != nil {
		return nil, connector.Discovery{}, err
	}
	settings, err := kind.Prepare(src)
	if err != nil {
		return nil, connector.Discovery{}, connectorError(ctx, err)
	}
	if err := s.requireLogCapture(ctx, workspaceID, settings); err != nil {
		return nil, connector.Discovery{}, err
	}
	discovery, err := kind.Discover(ctx, connector.Source{Settings: settings, Secret: src.Secret, WorkspaceID: workspaceID})
	if err != nil {
		return nil, connector.Discovery{}, connectorError(ctx, err)
	}
	if discovery.Settings != nil {
		settings = discovery.Settings
	}
	return settings, discovery, nil
}

// connectorError shows the customer the connector's own message; the driver
// error behind a refusal goes to the log only.
func connectorError(ctx context.Context, err error) error {
	var invalidErr *connector.InvalidError
	var refusedErr *connector.RefusedError
	switch {
	case errors.As(err, &invalidErr):
		return status.Error(codes.InvalidArgument, invalidErr.Message)
	case errors.As(err, &refusedErr):
		log.From(ctx).Info("connector refused", "err", err)
		return status.Error(codes.FailedPrecondition, refusedErr.Message)
	}
	return status.Errorf(codes.Internal, "test connector: %v", err)
}

func connectorName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || len(name) > maxConnectorNameLength {
		return "", status.Errorf(codes.InvalidArgument, "name must be 1 to %d characters", maxConnectorNameLength)
	}
	return name, nil
}

// sourceFromProto splits a config into the kind, its stored settings and the
// credential. API keys are trimmed; a database password is kept byte for
// byte. secretless is access that needs no credential: an S3 role, or an
// event source, whose fields are its sends' and never a request's.
func sourceFromProto(catalog *connector.Catalog, cfg *foundryv1.ConnectorConfig) (_ connector.Kind, _ connector.Source, secretless bool, _ error) {
	var (
		name     string
		settings any
		secret   string
	)
	switch k := cfg.GetKind().(type) {
	case *foundryv1.ConnectorConfig_Stripe:
		name, settings, secret = "stripe", connector.StripeSettings{}, strings.TrimSpace(k.Stripe.GetApiKey())
	case *foundryv1.ConnectorConfig_Workos:
		name, settings, secret = "workos", struct{}{}, strings.TrimSpace(k.Workos.GetApiKey())
	case *foundryv1.ConnectorConfig_Posthog:
		p := k.Posthog
		name, secret = "posthog", strings.TrimSpace(p.GetApiKey())
		ph := connector.PostHogSettings{Region: p.GetRegion(), Host: p.GetHost(), ProjectID: p.GetProjectId(), EventDays: int(p.GetEventDays()),
			Mode: p.GetMode()}
		if b := p.GetExportBucket(); b != nil {
			ph.ExportBucket = &connector.S3Settings{Bucket: b.GetBucket(), Prefix: b.GetPrefix(), Region: b.GetRegion()}
			if role := b.GetRole(); role != nil {
				ph.ExportBucket.Role = &connector.S3Role{ARN: role.GetRoleArn()}
			}
		}
		settings = ph
	case *foundryv1.ConnectorConfig_Github:
		g := k.Github
		name, secret = "github", strings.TrimSpace(g.GetToken())
		settings = connector.GitHubSettings{Repos: g.GetRepos(), ReviewDays: int(g.GetReviewDays())}
	case *foundryv1.ConnectorConfig_Linear:
		name, settings, secret = "linear", struct{}{}, strings.TrimSpace(k.Linear.GetApiKey())
	case *foundryv1.ConnectorConfig_Calcom:
		name, settings, secret = "calcom", struct{}{}, strings.TrimSpace(k.Calcom.GetApiKey())
	case *foundryv1.ConnectorConfig_Granola:
		g := k.Granola
		name, settings, secret = "granola", connector.GranolaSettings{Transcripts: g.GetTranscripts()}, strings.TrimSpace(g.GetApiKey())
	case *foundryv1.ConnectorConfig_Slack:
		sl := k.Slack
		name, settings, secret = "slack", connector.SlackSettings{HistoryDays: int(sl.GetHistoryDays())}, strings.TrimSpace(sl.GetBotToken())
	case *foundryv1.ConnectorConfig_Notion:
		name, settings, secret = "notion", struct{}{}, strings.TrimSpace(k.Notion.GetToken())
	case *foundryv1.ConnectorConfig_Google:
		g := k.Google
		name, secret = "google", strings.TrimSpace(g.GetServiceAccountKey())
		settings = connector.GoogleSettings{Subject: g.GetSubject(), EventDays: int(g.GetEventDays())}
	case *foundryv1.ConnectorConfig_Postgres:
		name, settings, secret = "postgres", databaseSettings(k.Postgres), k.Postgres.GetPassword()
	case *foundryv1.ConnectorConfig_Mysql:
		name, settings, secret = "mysql", databaseSettings(k.Mysql), k.Mysql.GetPassword()
	case *foundryv1.ConnectorConfig_S3:
		c := k.S3
		s3 := connector.S3Settings{Bucket: c.GetBucket(), Prefix: c.GetPrefix(), Region: c.GetRegion(), Tables: c.GetTables()}
		switch auth := c.GetAuth().(type) {
		case *foundryv1.S3ConnectorConfig_Role:
			s3.Role, secretless = &connector.S3Role{ARN: auth.Role.GetRoleArn()}, true
		case *foundryv1.S3ConnectorConfig_Keys:
			s3.Keys = &connector.S3Keys{AccessKeyID: auth.Keys.GetAccessKeyId(), Endpoint: auth.Keys.GetEndpoint()}
			secret = strings.TrimSpace(auth.Keys.GetSecretAccessKey())
		}
		name, settings = "s3", s3
	case *foundryv1.ConnectorConfig_Events:
		// Each field only when the request carries it: an update that leaves one out keeps it.
		e := k.Events
		name, settings, secretless = foundrymodel.ConnectorKindEvents, connector.EventsSettings{Description: e.Description, TextField: e.TextField}, true
	default:
		return nil, connector.Source{}, false, status.Error(codes.InvalidArgument, "config is required")
	}
	kind, err := catalog.Kind(name)
	if err != nil {
		return nil, connector.Source{}, false, status.Errorf(codes.InvalidArgument, "connector kind %q is not available", name)
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return nil, connector.Source{}, false, status.Errorf(codes.Internal, "encode connector settings: %v", err)
	}
	return kind, connector.Source{Settings: raw, Secret: secret}, secretless, nil
}

func databaseSettings(c *foundryv1.DatabaseConnectorConfig) connector.DatabaseSettings {
	d := connector.DatabaseSettings{
		Host: c.GetHost(), Port: int(c.GetPort()), Database: c.GetDatabase(), User: c.GetUser(),
		Schema: c.GetSchema(), Tables: c.GetTables(), DisableTLS: c.GetDisableTls(), CA: c.GetCa(),
		CursorColumns: c.GetCursorColumns(),
	}
	if cdc := c.GetCdc(); cdc != nil {
		d.CDC = &connector.DatabaseCDC{Enabled: cdc.GetEnabled(), Publication: cdc.GetPublication(), ServerID: cdc.GetServerId()}
	}
	return d
}

// configToProto rebuilds a config from stored settings alone, so a response
// can never carry a credential.
func configToProto(kind string, settings json.RawMessage) (*foundryv1.ConnectorConfig, error) {
	switch kind {
	case "stripe":
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Stripe{Stripe: &foundryv1.StripeConnectorConfig{}}}, nil
	case "workos":
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Workos{Workos: &foundryv1.WorkOSConnectorConfig{}}}, nil
	case "posthog":
		var p connector.PostHogSettings
		if err := json.Unmarshal(settings, &p); err != nil {
			return nil, err
		}
		out := &foundryv1.PostHogConnectorConfig{Region: p.Region, Host: p.Host, ProjectId: p.ProjectID, EventDays: int32(p.EventDays), Mode: p.Mode}
		if b := p.ExportBucket; b != nil {
			out.ExportBucket = &foundryv1.S3ConnectorConfig{Bucket: b.Bucket, Prefix: b.Prefix, Region: b.Region}
			if b.Role != nil {
				out.ExportBucket.Auth = &foundryv1.S3ConnectorConfig_Role{Role: &foundryv1.S3RoleAuth{RoleArn: b.Role.ARN}}
			}
		}
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Posthog{Posthog: out}}, nil
	case "github":
		var g connector.GitHubSettings
		if err := json.Unmarshal(settings, &g); err != nil {
			return nil, err
		}
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Github{Github: &foundryv1.GitHubConnectorConfig{
			Repos: g.Repos, ReviewDays: int32(g.ReviewDays),
		}}}, nil
	case "linear":
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Linear{Linear: &foundryv1.LinearConnectorConfig{}}}, nil
	case "calcom":
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Calcom{Calcom: &foundryv1.CalcomConnectorConfig{}}}, nil
	case "granola":
		var g connector.GranolaSettings
		if err := json.Unmarshal(settings, &g); err != nil {
			return nil, err
		}
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Granola{Granola: &foundryv1.GranolaConnectorConfig{
			Transcripts: g.Transcripts,
		}}}, nil
	case "slack":
		var sl connector.SlackSettings
		if err := json.Unmarshal(settings, &sl); err != nil {
			return nil, err
		}
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Slack{Slack: &foundryv1.SlackConnectorConfig{
			HistoryDays: int32(sl.HistoryDays),
		}}}, nil
	case "notion":
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Notion{Notion: &foundryv1.NotionConnectorConfig{}}}, nil
	case "google":
		var g connector.GoogleSettings
		if err := json.Unmarshal(settings, &g); err != nil {
			return nil, err
		}
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Google{Google: &foundryv1.GoogleWorkspaceConnectorConfig{
			Subject: g.Subject, EventDays: int32(g.EventDays),
		}}}, nil
	case "postgres", "mysql":
		var d connector.DatabaseSettings
		if err := json.Unmarshal(settings, &d); err != nil {
			return nil, err
		}
		db := &foundryv1.DatabaseConnectorConfig{
			Host: d.Host, Port: int32(d.Port), Database: d.Database, User: d.User, Schema: d.Schema,
			Tables: d.Tables, DisableTls: d.DisableTLS, Ca: d.CA, CursorColumns: d.CursorColumns,
		}
		if d.CDC != nil {
			db.Cdc = &foundryv1.DatabaseCDCConfig{Enabled: d.CDC.Enabled, Publication: d.CDC.Publication, ServerId: d.CDC.ServerID}
		}
		if kind == "postgres" {
			return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Postgres{Postgres: db}}, nil
		}
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Mysql{Mysql: db}}, nil
	case "s3":
		var s connector.S3Settings
		if err := json.Unmarshal(settings, &s); err != nil {
			return nil, err
		}
		c := &foundryv1.S3ConnectorConfig{Bucket: s.Bucket, Prefix: s.Prefix, Region: s.Region, Tables: s.Tables}
		switch {
		case s.Role != nil:
			c.Auth = &foundryv1.S3ConnectorConfig_Role{Role: &foundryv1.S3RoleAuth{RoleArn: s.Role.ARN}}
		case s.Keys != nil:
			c.Auth = &foundryv1.S3ConnectorConfig_Keys{Keys: &foundryv1.S3KeyAuth{AccessKeyId: s.Keys.AccessKeyID, Endpoint: s.Keys.Endpoint}}
		}
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_S3{S3: c}}, nil
	case foundrymodel.ConnectorKindEvents:
		return eventsConfigToProto(settings)
	}
	return nil, fmt.Errorf("unknown connector kind %q", kind)
}

// connectorToProto adds the connector's synced tables and newest sync. A
// pushed kind's table (an event source's) is followed on every plan, and it
// syncs when rows arrive rather than on an interval.
func (s *Service) connectorToProto(ctx context.Context, c *foundrymodel.Connector) (*foundryv1.Connector, error) {
	cfg, err := configToProto(c.Kind, c.Config)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read connector %s settings: %v", c.ID, err)
	}
	plan := s.planOrFree(ctx, c.WorkspaceID)
	src := connector.Source{Settings: c.Config, WorkspaceID: c.WorkspaceID, ConnectorID: c.ID}
	var reader connector.ChangeReader
	var floor time.Duration
	var pushed bool
	if catalog := s.deps.ConnectorCatalog; catalog != nil {
		if kind, err := catalog.Kind(c.Kind); err == nil {
			reader, _ = kind.(connector.ChangeReader)
			_, pushed = kind.(connector.Pushed)
			if f, ok := kind.(connector.IntervalFloor); ok {
				floor = f.MinInterval(src)
			}
		}
	}
	out := &foundryv1.Connector{
		Id: c.ID, WorkspaceId: c.WorkspaceID, Kind: c.Kind, Name: c.Name, Config: cfg, SecretHint: c.SecretHint,
		CreatedAt: timestamppb.New(c.CreatedAt), UpdatedAt: timestamppb.New(c.UpdatedAt),
		Schedule: scheduleToProto(c, plan, floor), CdcStatus: cdcStatusToProto(c.ChangeCursor),
	}
	if pushed {
		out.Schedule.EverySeconds, out.Schedule.EffectiveEverySeconds = 0, 0
	}
	tables, err := s.deps.DB.ListConnectorTables(ctx, c.WorkspaceID, c.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list connector %s tables: %v", c.ID, err)
	}
	for _, capture := range plan.Sync.Capture {
		src.Captures = append(src.Captures, connector.Capture(capture))
	}
	for _, t := range tables {
		pt, err := syncedTableToProto(t)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read connector %s table %s: %v", c.ID, t.TableName, err)
		}
		pt.Capture = string(connector.CaptureFull)
		// As in the syncer: a keyless table can't be merged by key, so it is always read in full.
		if reader != nil && len(pt.GetPrimaryKey()) > 0 {
			if capture := reader.Capture(src, t.TableName); pushed || plan.Sync.Allows(foundryplan.Capture(capture)) {
				pt.Capture = string(capture)
			}
		}
		out.Tables = append(out.Tables, pt)
	}
	syncs, err := s.deps.DB.ListConnectorSyncs(ctx, c.WorkspaceID, c.ID, 1)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list connector %s syncs: %v", c.ID, err)
	}
	if len(syncs) > 0 {
		if out.LastSync, err = s.syncWithChanges(ctx, syncs[0]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// planOrFree is the workspace's plan, Free when it can't be read: a
// connector's display never fails on the billing system.
func (s *Service) planOrFree(ctx context.Context, workspaceID string) *foundryplan.Plan {
	plan, err := s.workspacePlan(ctx, workspaceID)
	if err != nil {
		return s.foundryCatalog().Free()
	}
	return plan
}

// maxSyncEvery bounds an owner's interval: past a month, a schedule is no schedule.
const maxSyncEvery = 30 * 24 * time.Hour

// scheduleUpdate turns an owner's sync_every_seconds into the stored schedule:
// 0 is the plan's interval, -1 manual only, and a positive value at least
// the plan's shortest. The next sync is one interval from now.
func (s *Service) scheduleUpdate(ctx context.Context, workspaceID string, every int64) (*foundrymodel.ConnectorScheduleUpdate, error) {
	plan, err := s.workspacePlan(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	next := time.Now().Add(plan.Sync.Every)
	switch {
	case every == 0:
		return &foundrymodel.ConnectorScheduleUpdate{NextSyncAt: &next}, nil
	case every == -1:
		manual := 0
		return &foundrymodel.ConnectorScheduleUpdate{SyncEverySeconds: &manual}, nil
	case every < 0 || time.Duration(every)*time.Second > maxSyncEvery:
		return nil, status.Errorf(codes.InvalidArgument, "sync_every_seconds must be -1, 0 or between %d and %d",
			int64(plan.Sync.Shortest/time.Second), int64(maxSyncEvery/time.Second))
	case time.Duration(every)*time.Second < plan.Sync.Shortest:
		return nil, status.Errorf(codes.InvalidArgument, "Foundry %s syncs at most every %s", plan.DisplayName, plan.Sync.Shortest)
	}
	seconds := int(every)
	next = time.Now().Add(time.Duration(every) * time.Second)
	return &foundrymodel.ConnectorScheduleUpdate{SyncEverySeconds: &seconds, NextSyncAt: &next}, nil
}

// scheduleToProto shows the owner's choice as the API encodes it (0 the
// plan's interval, -1 manual only) and the interval in force under plan and
// the kind's floor, as the sync worker schedules it.
func scheduleToProto(c *foundrymodel.Connector, plan *foundryplan.Plan, floor time.Duration) *foundryv1.ConnectorSchedule {
	out := &foundryv1.ConnectorSchedule{EffectiveEverySeconds: int64(max(plan.Sync.Every, floor) / time.Second)}
	switch choice := c.SyncEverySeconds; {
	case choice == nil:
	case *choice == 0:
		out.EverySeconds, out.EffectiveEverySeconds = -1, 0
	default:
		out.EverySeconds = int64(*choice)
		out.EffectiveEverySeconds = int64(max(time.Duration(*choice)*time.Second, plan.Sync.Shortest, floor) / time.Second)
	}
	if c.NextSyncAt != nil {
		out.NextSyncAt = timestamppb.New(*c.NextSyncAt)
	}
	if c.SchedulePaused != nil {
		out.PausedReason = *c.SchedulePaused
	}
	return out
}

// cdcStatusToProto reads the cdc object a log-reading kind keeps in its
// cursor; nil for every other connector.
func cdcStatusToProto(cursor json.RawMessage) *foundryv1.ConnectorCDCStatus {
	var c struct {
		CDC *struct {
			Position      string     `json:"position"`
			RetainedBytes int64      `json:"retained_bytes"`
			LastResetAt   *time.Time `json:"last_reset_at"`
			LastReset     string     `json:"last_reset"`
		} `json:"cdc"`
	}
	if len(cursor) == 0 || json.Unmarshal(cursor, &c) != nil || c.CDC == nil {
		return nil
	}
	out := &foundryv1.ConnectorCDCStatus{Position: c.CDC.Position, RetainedBytes: c.CDC.RetainedBytes, LastReset: c.CDC.LastReset}
	if c.CDC.LastResetAt != nil {
		out.LastResetAt = timestamppb.New(*c.CDC.LastResetAt)
	}
	return out
}

func (s *Service) connectorResponse(ctx context.Context, c *foundrymodel.Connector, d *connector.Discovery) (*foundryv1.ConnectorResponse, error) {
	pc, err := s.connectorToProto(ctx, c)
	if err != nil {
		return nil, err
	}
	resp := &foundryv1.ConnectorResponse{Connector: pc}
	if d != nil {
		resp.Discovery = discoveryToProto(*d)
	}
	return resp, nil
}

func discoveryToProto(d connector.Discovery) *foundryv1.ConnectorDiscovery {
	out := &foundryv1.ConnectorDiscovery{MaxBytes: d.MaxBytes, Warnings: d.Warnings}
	for _, u := range d.Unavailable {
		if u.Reason == "" {
			out.Unavailable = append(out.Unavailable, u.Name)
			continue
		}
		out.Unavailable = append(out.Unavailable, u.Name+": "+u.Reason)
	}
	for _, t := range d.Tables {
		pt := &foundryv1.ConnectorTable{Name: t.Name, PrimaryKey: t.PrimaryKey, FileCount: int32(len(t.Files)), Problem: t.Problem}
		for _, f := range t.Files {
			pt.Format, pt.Bytes = f.Format.String(), pt.Bytes+f.Size
		}
		for _, row := range t.Preview {
			pt.Preview = append(pt.Preview, string(row))
		}
		for _, c := range t.Columns {
			pt.Columns = append(pt.Columns, &foundryv1.ConnectorColumn{Name: c.Name, Type: c.Type, Nullable: c.Nullable})
		}
		for _, fk := range t.ForeignKeys {
			pt.ForeignKeys = append(pt.ForeignKeys, &foundryv1.ConnectorForeignKey{
				Column: fk.Column, ReferencedTable: fk.ReferencedTable, ReferencedColumn: fk.ReferencedColumn,
			})
		}
		out.Tables = append(out.Tables, pt)
	}
	return out
}
