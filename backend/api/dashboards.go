package api

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/dashboard"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// Dashboards (package dashboard). Who may do what:
//   - Every member sees every dashboard of the workspace, and reads its tiles
//     as themselves: each tile's query runs through ReadFoundryGraph as the
//     viewer, so a dashboard shows no one more than they may read. A grantee
//     of source grants sees only the pinned ones, and reads them through its
//     grants.
//   - Any member creates dashboards, and changes and deletes their own while
//     they aren't pinned: a dashboard holds no data, only queries its
//     viewers run, so making one gives nobody anything.
//   - Owners and admins change and delete any, and pin them in every
//     member's sidebar: what the whole workspace is shown is theirs to
//     choose, so a pinned dashboard is theirs to change.
//   - A dashboard's page in Foundry is how it is shared inside the workspace:
//     every member who opens it reads it as themselves, as above.
//   - Owners and admins publish a dashboard's snapshot at a public link, read
//     as themselves, for people outside the workspace, and unpublish it: a
//     link shows data to anyone, outside the workspace's own access, which
//     only they decide. A link may expire (7, 30 or 90 days), and is then
//     served as an unpublished one. Hosted only, with
//     foundry.dashboards.public_enabled (config.PublicDashboards), and while
//     the workspace's owners keep public links on (FoundryDashboardSettings):
//     off, no link of the workspace is served or published.

const (
	// maxDashboards is the most dashboards a workspace keeps, so a list is
	// one answer.
	maxDashboards = 200
	// maxPinnedDashboards is the most pinned in the sidebar at once.
	maxPinnedDashboards = 20
	// maxDashboardNameChars and maxDashboardDescriptionChars bound a
	// dashboard's name and description.
	maxDashboardNameChars        = 120
	maxDashboardDescriptionChars = 1000
	// shareTokenContext is what a link's token is sealed for, with its
	// dashboard's id.
	shareTokenContext = "foundry-dashboard-share:"
)

// shareTokenPattern is a link's token: hxd_ and 32 random bytes, base64url.
var shareTokenPattern = regexp.MustCompile(`^hxd_[A-Za-z0-9_-]{43}$`)

// dashboardCaller is who calls a dashboard's route.
type dashboardCaller struct {
	userID string
	// manager is an owner or admin of the workspace; owner, an owner.
	manager bool
	owner   bool
	// grantee is a caller source grants admitted: it sees the pinned
	// dashboards only, and the access rules deny it every change.
	grantee bool
}

// dashboardCallerOf is the person calling for workspaceID: a user signed in
// to the site or calling through the MCP, or a user source grants admit;
// never a service key, an event source or the host. A role that can't be
// read is refused, and no People port makes no one a manager.
func (s *Service) dashboardCallerOf(ctx context.Context, workspaceID string) (dashboardCaller, error) {
	if workspaceID == "" {
		return dashboardCaller{}, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	if g, ok := access.GranteeFrom(ctx); ok {
		if g.Principal.Kind != access.PrincipalUser {
			return dashboardCaller{}, status.Error(codes.PermissionDenied, "dashboards are people's: sign in to Foundry")
		}
		return dashboardCaller{userID: g.Principal.ID, grantee: true}, nil
	}
	a := actor(ctx)
	if (a.Source != ports.SourceUser && a.Source != ports.SourceMCP) || a.UserID == "" || a.ServiceCredentialID != "" {
		return dashboardCaller{}, status.Error(codes.PermissionDenied, "dashboards are people's: sign in to Foundry")
	}
	c := dashboardCaller{userID: a.UserID}
	if s.deps.People != nil {
		m, err := s.deps.People.Member(ctx, workspaceID, a.UserID)
		if err != nil {
			return dashboardCaller{}, status.Errorf(codes.Unavailable, "read your role: %v", err)
		}
		c.manager = m.Role == ports.RoleOwner || m.Role == ports.RoleAdmin
		c.owner = m.Role == ports.RoleOwner
	}
	return c, nil
}

func (c dashboardCaller) editor() foundrymodel.FoundryDashboardEditor {
	return foundrymodel.FoundryDashboardEditor{UserID: c.userID, Manager: c.manager}
}

// mayEdit reports whether c may change d's name, description and tiles, as
// the store decides at the write.
func (c dashboardCaller) mayEdit(d *foundrymodel.FoundryDashboard) bool {
	return !c.grantee && (c.manager || d.CreatedByUserID == c.userID && d.PinnedAt == nil)
}

// mayDelete reports whether c may delete d, as the store decides at the
// write: as they may edit it, and only as an owner or admin while it is
// published, as unpublishing is.
func (c dashboardCaller) mayDelete(d *foundrymodel.FoundryDashboard) bool {
	return c.mayEdit(d) && (d.Share == nil || c.manager)
}

// dashboardRefusal is the API's answer to a refusal of the store's; false
// when err is none.
func dashboardRefusal(workspaceID string, err error) (error, bool) {
	var r foundrymodel.FoundryDashboardRefusal
	if !errors.As(err, &r) {
		return nil, false
	}
	switch r.Reason {
	case foundrymodel.FoundryDashboardNotEditable:
		// FAILED_PRECONDITION saying who may, never PERMISSION_DENIED, which
		// agents read as not found: callers refuse it only of a dashboard the
		// caller sees (visibleDashboard).
		return foundryPrecondition(workspaceID, "FOUNDRY_DASHBOARD_NOT_EDITABLE", r.Detail), true
	case foundrymodel.FoundryDashboardChanged:
		return foundryPrecondition(workspaceID, "FOUNDRY_DASHBOARD_CHANGED", r.Detail), true
	case foundrymodel.FoundryDashboardUnreadable:
		return foundryPrecondition(workspaceID, "FOUNDRY_DASHBOARD_TILE_FAILED", r.Detail), true
	default:
		return foundryPrecondition(workspaceID, "FOUNDRY_DASHBOARD_LIMIT", r.Detail), true
	}
}

// visibleDashboard is the workspace's dashboard as c sees it: NotFound for
// one that isn't there, or isn't pinned, for a grantee.
func (s *Service) visibleDashboard(ctx context.Context, c dashboardCaller, workspaceID, dashboardID string) (*foundrymodel.FoundryDashboard, error) {
	if dashboardID == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	d, err := s.deps.DB.GetFoundryDashboard(ctx, workspaceID, dashboardID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && c.grantee && d.PinnedAt == nil:
		return nil, status.Error(codes.NotFound, "dashboard not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the dashboard: %v", err)
	}
	return d, nil
}

// dashboardResponse reads the dashboard again, as c sees it.
func (s *Service) dashboardResponse(ctx context.Context, c dashboardCaller, workspaceID, dashboardID string) (*foundryv1.FoundryDashboardResponse, error) {
	d, err := s.visibleDashboard(ctx, c, workspaceID, dashboardID)
	if err != nil {
		return nil, err
	}
	settings, err := s.dashboardSettings(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return &foundryv1.FoundryDashboardResponse{Dashboard: s.dashboardToProto(d, c, settings, time.Now())}, nil
}

// dashboardSettings are the workspace's dashboard settings, read only where
// the host offers public links: elsewhere they change nothing, and read as
// the default.
func (s *Service) dashboardSettings(ctx context.Context, workspaceID string) (foundrymodel.FoundryDashboardSettings, error) {
	if !s.deps.Cfg.PublicDashboards() {
		return foundrymodel.FoundryDashboardSettings{WorkspaceID: workspaceID, PublicLinks: true}, nil
	}
	settings, err := s.deps.DB.GetFoundryDashboardSettings(ctx, workspaceID)
	if err != nil {
		return foundrymodel.FoundryDashboardSettings{}, status.Errorf(codes.Internal, "read the dashboard settings: %v", err)
	}
	return settings, nil
}

// publishBlock is why c may not publish a dashboard of a workspace with
// settings; UNSPECIFIED when they may. The host comes first: where it offers
// no public links, no one is told to ask an owner or admin.
func (s *Service) publishBlock(c dashboardCaller, settings foundrymodel.FoundryDashboardSettings) foundryv1.FoundryDashboardPublishBlock {
	switch {
	case !s.deps.Cfg.PublicDashboards():
		return foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_UNAVAILABLE
	case c.grantee || !c.manager:
		return foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_ADMIN_ONLY
	case !settings.PublicLinks:
		return foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_WORKSPACE_OFF
	}
	return foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_UNSPECIFIED
}

// publishRefusal is the refusal of a publish blocked for block (none for
// UNSPECIFIED), with its reason.
func publishRefusal(workspaceID string, block foundryv1.FoundryDashboardPublishBlock) error {
	switch block {
	case foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_UNAVAILABLE:
		return foundryPrecondition(workspaceID, "FOUNDRY_DASHBOARD_PUBLIC_UNAVAILABLE", "public dashboard links aren't available here")
	case foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_ADMIN_ONLY:
		return foundryPrecondition(workspaceID, "FOUNDRY_DASHBOARD_PUBLIC_ADMIN_ONLY",
			"only an owner or admin makes a public link: share the dashboard's page in Foundry with the workspace instead")
	case foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_WORKSPACE_OFF:
		return foundryPrecondition(workspaceID, "FOUNDRY_DASHBOARD_PUBLIC_OFF", "an owner turned public dashboard links off for this workspace")
	}
	return nil
}

// shareStatus is whether share shows its snapshot at now, for a workspace
// with settings: the host, then the workspace's switch, then its expiry.
func (s *Service) shareStatus(share *foundrymodel.FoundryDashboardShare, settings foundrymodel.FoundryDashboardSettings, now time.Time) foundryv1.FoundryDashboardShareStatus {
	switch {
	case !s.deps.Cfg.PublicDashboards():
		return foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_UNAVAILABLE
	case !settings.PublicLinks:
		return foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_WORKSPACE_OFF
	case share.Expired(now):
		return foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_EXPIRED
	}
	return foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_LIVE
}

// linkExpiry is what expiry asks of a publish at now.
func linkExpiry(expiry foundryv1.FoundryDashboardLinkExpiry, now time.Time) (foundrymodel.FoundryDashboardLinkExpiry, error) {
	days := 0
	switch expiry {
	case foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_UNSPECIFIED:
		return foundrymodel.FoundryDashboardLinkExpiry{Keep: true}, nil
	case foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_NEVER:
		return foundrymodel.FoundryDashboardLinkExpiry{}, nil
	case foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_7:
		days = 7
	case foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_30:
		days = 30
	case foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_90:
		days = 90
	default:
		return foundrymodel.FoundryDashboardLinkExpiry{}, status.Error(codes.InvalidArgument, "expiry is never, 7, 30 or 90 days")
	}
	at := now.Add(time.Duration(days) * 24 * time.Hour)
	return foundrymodel.FoundryDashboardLinkExpiry{At: &at}, nil
}

func (s *Service) ListFoundryDashboards(ctx context.Context, req *foundryv1.ListFoundryDashboardsRequest) (*foundryv1.ListFoundryDashboardsResponse, error) {
	c, err := s.dashboardCallerOf(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	list, err := s.deps.DB.ListFoundryDashboards(ctx, req.GetWorkspaceId(), c.grantee)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list dashboards: %v", err)
	}
	settings, err := s.dashboardSettings(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := &foundryv1.ListFoundryDashboardsResponse{CanCreate: !c.grantee, Dashboards: make([]*foundryv1.FoundryDashboard, 0, len(list))}
	for _, d := range list {
		out.Dashboards = append(out.Dashboards, s.dashboardToProto(d, c, settings, now))
	}
	return out, nil
}

func (s *Service) GetFoundryDashboard(ctx context.Context, req *foundryv1.GetFoundryDashboardRequest) (*foundryv1.FoundryDashboardResponse, error) {
	c, err := s.dashboardCallerOf(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	return s.dashboardResponse(ctx, c, req.GetWorkspaceId(), req.GetId())
}

// nameAndDescription checks a dashboard's or an automation's name and
// description, trimmed.
func nameAndDescription(name, description string) (string, string, error) {
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	switch {
	case name == "" || utf8.RuneCountInString(name) > maxDashboardNameChars || strings.ContainsFunc(name, unicode.IsControl):
		return "", "", status.Errorf(codes.InvalidArgument, "name must be 1 to %d characters, with no control characters", maxDashboardNameChars)
	case !utf8.ValidString(description) || utf8.RuneCountInString(description) > maxDashboardDescriptionChars:
		return "", "", status.Errorf(codes.InvalidArgument, "description must be at most %d characters", maxDashboardDescriptionChars)
	}
	return name, description, nil
}

// dashboardSpec checks a spec of the workspace's (empty: no tiles) that
// replaces stored (none for a new dashboard): its shape, and that each tile
// with a new target (dashboard.Spec.NewTargets) reads a live ontology of the
// workspace and a load of it. A tile whose target is as stored was checked
// when it was given: its load deleted since, or its ontology, it is no
// reason to refuse an edit of another tile.
func (s *Service) dashboardSpec(ctx context.Context, workspaceID, raw string, stored dashboard.Spec) (dashboard.Spec, error) {
	if raw == "" {
		raw = `{"v":1,"tiles":[],"layout":{"rows":[]}}`
	}
	spec, err := dashboard.ParseSpec([]byte(raw))
	if err != nil {
		return dashboard.Spec{}, status.Error(codes.InvalidArgument, err.Error())
	}
	ontologies := map[string]*foundrymodel.FoundryOntology{}
	for _, t := range spec.NewTargets(stored) {
		if err := s.dashboardTileTarget(ctx, workspaceID, t, ontologies); err != nil {
			return dashboard.Spec{}, err
		}
	}
	return spec, nil
}

// dashboardTileTarget checks that t reads a live ontology of the workspace
// and, pinned, a load of it, remembering the ontologies read in seen.
func (s *Service) dashboardTileTarget(ctx context.Context, workspaceID string, t dashboard.Tile, seen map[string]*foundrymodel.FoundryOntology) error {
	o, ok := seen[t.OntologyID]
	if !ok {
		found, err := s.liveFoundryOntology(ctx, workspaceID, t.OntologyID)
		if status.Code(err) == codes.NotFound {
			return status.Errorf(codes.InvalidArgument, "tile %s reads an ontology this workspace doesn't have", t.ID)
		}
		if err != nil {
			return err
		}
		o, seen[t.OntologyID] = found, found
	}
	pinned, ok := t.Load.(dashboard.PinnedLoad)
	if !ok {
		return nil
	}
	load, err := s.deps.DB.GetFoundryLoad(ctx, o.ID, pinned.ID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && load.Status == foundrymodel.FoundryLoadDeleted:
		return status.Errorf(codes.InvalidArgument, "tile %s pins a load %s doesn't have", t.ID, o.Name)
	case err != nil:
		return status.Errorf(codes.Internal, "read the load: %v", err)
	}
	return nil
}

func (s *Service) CreateFoundryDashboard(ctx context.Context, req *foundryv1.CreateFoundryDashboardRequest) (*foundryv1.FoundryDashboardResponse, error) {
	ws := req.GetWorkspaceId()
	c, err := s.dashboardCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	token := req.GetClientToken()
	if token == "" || len(token) > maxClientTokenLength {
		return nil, status.Errorf(codes.InvalidArgument, "client_token is required, at most %d characters", maxClientTokenLength)
	}
	name, description, err := nameAndDescription(req.GetName(), req.GetDescription())
	if err != nil {
		return nil, err
	}
	spec, err := s.dashboardSpec(ctx, ws, req.GetSpecJson(), dashboard.Spec{})
	if err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(spec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "write the spec: %v", err)
	}
	d := &foundrymodel.FoundryDashboard{ID: id.New(), WorkspaceID: ws, Name: name, Description: description, Spec: canonical,
		CreatedByUserID: c.userID, ClientToken: &token}
	err = s.deps.DB.CreateFoundryDashboard(ctx, d, maxDashboards)
	if refused, ok := dashboardRefusal(ws, err); ok {
		return nil, refused
	}
	switch {
	case errors.Is(err, foundrystore.ErrConflict):
		// The same create again: its dashboard, whatever it is called now.
		made, err := s.deps.DB.GetFoundryDashboardByClientToken(ctx, ws, c.userID, token)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read the dashboard: %v", err)
		}
		d.ID = made.ID
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "workspace not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "create the dashboard: %v", err)
	}
	return s.dashboardResponse(ctx, c, ws, d.ID)
}

func (s *Service) UpdateFoundryDashboard(ctx context.Context, req *foundryv1.UpdateFoundryDashboardRequest) (*foundryv1.FoundryDashboardResponse, error) {
	ws := req.GetWorkspaceId()
	c, err := s.dashboardCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	if req.GetExpectedRevision() < 1 || req.Name == nil && req.Description == nil && req.SpecJson == nil {
		return nil, status.Error(codes.InvalidArgument, "expected_revision and a change of name, description or spec_json are required")
	}
	d, err := s.visibleDashboard(ctx, c, ws, req.GetId())
	if err != nil {
		return nil, err
	}
	var u foundrymodel.FoundryDashboardUpdate
	if req.Name != nil || req.Description != nil {
		name, description := d.Name, d.Description
		if req.Name != nil {
			name = req.GetName()
		}
		if req.Description != nil {
			description = req.GetDescription()
		}
		name, description, err := nameAndDescription(name, description)
		if err != nil {
			return nil, err
		}
		u.Name, u.Description = &name, &description
	}
	if req.SpecJson != nil {
		stored, err := dashboard.ParseSpec(d.Spec)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read the dashboard's spec: %v", err)
		}
		spec, err := s.dashboardSpec(ctx, ws, req.GetSpecJson(), stored)
		if err != nil {
			return nil, err
		}
		if u.Spec, err = json.Marshal(spec); err != nil {
			return nil, status.Errorf(codes.Internal, "write the spec: %v", err)
		}
	}
	if err := s.updateDashboard(ctx, c, ws, d.ID, req.GetExpectedRevision(), u); err != nil {
		return nil, err
	}
	return s.dashboardResponse(ctx, c, ws, d.ID)
}

// updateDashboard makes u as c, answering the store's refusals.
func (s *Service) updateDashboard(ctx context.Context, c dashboardCaller, workspaceID, dashboardID string, expected int64,
	u foundrymodel.FoundryDashboardUpdate) error {
	err := s.deps.DB.UpdateFoundryDashboard(ctx, workspaceID, dashboardID, c.editor(), expected, u)
	if refused, ok := dashboardRefusal(workspaceID, err); ok {
		return refused
	}
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return status.Error(codes.NotFound, "dashboard not found")
	case err != nil:
		return status.Errorf(codes.Internal, "update the dashboard: %v", err)
	}
	return nil
}

func (s *Service) DeleteFoundryDashboard(ctx context.Context, req *foundryv1.DeleteFoundryDashboardRequest) (*emptypb.Empty, error) {
	ws := req.GetWorkspaceId()
	c, err := s.dashboardCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	// One a grantee doesn't see is not found, never refused as not theirs.
	if _, err := s.visibleDashboard(ctx, c, ws, req.GetId()); err != nil {
		return nil, err
	}
	err = s.deps.DB.DeleteFoundryDashboard(ctx, ws, req.GetId(), c.editor())
	if refused, ok := dashboardRefusal(ws, err); ok {
		return nil, refused
	}
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "dashboard not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "delete the dashboard: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Service) PinFoundryDashboard(ctx context.Context, req *foundryv1.PinFoundryDashboardRequest) (*foundryv1.FoundryDashboardResponse, error) {
	ws := req.GetWorkspaceId()
	c, err := s.dashboardCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	if !c.manager {
		return nil, foundryPrecondition(ws, "FOUNDRY_DASHBOARD_NOT_EDITABLE", "only an owner or admin pins a dashboard for the workspace")
	}
	if req.GetId() == "" || req.GetExpectedRevision() < 1 {
		return nil, status.Error(codes.InvalidArgument, "id and expected_revision are required")
	}
	err = s.deps.DB.SetFoundryDashboardPinned(ctx, ws, req.GetId(), c.userID, req.GetExpectedRevision(), req.GetPinned(), maxPinnedDashboards)
	if refused, ok := dashboardRefusal(ws, err); ok {
		return nil, refused
	}
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "dashboard not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "pin the dashboard: %v", err)
	}
	return s.dashboardResponse(ctx, c, ws, req.GetId())
}

// AddFoundryDashboardQuestionVisual adds an Analyst answer's query result to
// the dashboard as a tile of its own row, last: the query the question
// recorded, with its visual's kind and title (else the question's words), of
// the current load, or the question's with pin_load.
func (s *Service) AddFoundryDashboardQuestionVisual(ctx context.Context, req *foundryv1.AddFoundryDashboardQuestionVisualRequest) (
	*foundryv1.FoundryDashboardResponse, error) {
	ws := req.GetWorkspaceId()
	c, err := s.dashboardCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	if req.GetExpectedRevision() < 1 || req.GetEvidenceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "expected_revision and evidence_id are required")
	}
	d, err := s.visibleDashboard(ctx, c, ws, req.GetId())
	if err != nil {
		return nil, err
	}
	o, err := s.liveFoundryOntology(ctx, ws, req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	q, err := s.foundryQuestion(ctx, o.ID, req.GetQuestionId())
	if err != nil {
		return nil, err
	}
	visual, err := dashboard.FromQuestion(q.Result, req.GetEvidenceId())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	spec, err := dashboard.ParseSpec(d.Spec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the dashboard's spec: %v", err)
	}
	tiles, rows := spec.Tiles(), spec.Rows()
	tile := dashboard.Tile{ID: freeTileID(tiles), Title: cmp.Or(visual.Title, dashboard.TileTitle(q.Question), "Answer"), OntologyID: o.ID,
		Load: dashboard.CurrentLoad{}, Query: visual.Query, Visual: visual.Visual}
	if req.GetPinLoad() {
		tile.Load = dashboard.PinnedLoad{ID: q.LoadID}
		err := s.dashboardTileTarget(ctx, ws, tile, map[string]*foundrymodel.FoundryOntology{o.ID: o})
		if status.Code(err) == codes.InvalidArgument {
			return nil, status.Error(codes.FailedPrecondition, "the question's load was deleted: add its result reading the current load instead")
		}
		if err != nil {
			return nil, err
		}
	}
	added, err := dashboard.NewSpec(append(tiles, tile), append(rows, []string{tile.ID}))
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	raw, err := json.Marshal(added)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "write the spec: %v", err)
	}
	if err := s.updateDashboard(ctx, c, ws, d.ID, req.GetExpectedRevision(), foundrymodel.FoundryDashboardUpdate{Spec: raw}); err != nil {
		return nil, err
	}
	return s.dashboardResponse(ctx, c, ws, d.ID)
}

// freeTileID is the first of t1, t2, … no tile has.
func freeTileID(tiles []dashboard.Tile) string {
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("t%d", n)
		if !slices.ContainsFunc(tiles, func(t dashboard.Tile) bool { return t.ID == candidate }) {
			return candidate
		}
	}
}

// dashboardRead is what reading tiles of one ontology's load gave: each
// tile's outcome by its id, as ReadFoundryGraph wrote it, and the day it was
// read as of.
type dashboardRead struct {
	loadID   string
	asOf     string
	outcomes map[string]json.RawMessage
}

// readDashboardTiles reads tiles, all of ontology o, as the caller, in one
// ReadFoundryGraph call per load they read: the ontology's current load for
// tiles that read the current one. The caller must be able to query o's
// database, as reading it needs. A load they don't read is FailedPrecondition
// (reason FOUNDRY_DASHBOARD_NOT_LOADED for an ontology with no current load).
func (s *Service) readDashboardTiles(ctx context.Context, o *foundrymodel.FoundryOntology, tiles []dashboard.Tile) ([]dashboardRead, error) {
	if read, _ := actor(ctx).Scope(o.WorkspaceID); !read.Allows(o.ProjectID) {
		return nil, status.Errorf(codes.PermissionDenied, "you can't read the database of %s", o.Name)
	}
	var loads []string
	byLoad := map[string][]dashboard.Tile{}
	for _, t := range tiles {
		var loadID string
		switch l := t.Load.(type) {
		case dashboard.CurrentLoad:
			if o.CurrentLoadID == nil {
				return nil, foundryPrecondition(o.WorkspaceID, "FOUNDRY_DASHBOARD_NOT_LOADED", o.Name+" has no current load to read: load a version of it first")
			}
			loadID = *o.CurrentLoadID
		case dashboard.PinnedLoad:
			loadID = l.ID
		}
		if _, ok := byLoad[loadID]; !ok {
			loads = append(loads, loadID)
		}
		byLoad[loadID] = append(byLoad[loadID], t)
	}
	out := make([]dashboardRead, 0, len(loads))
	for _, loadID := range loads {
		group := byLoad[loadID]
		resp, err := s.ReadFoundryGraph(ctx, &foundryv1.ReadFoundryGraphRequest{
			WorkspaceId: o.WorkspaceID, OntologyId: o.ID, LoadId: loadID, ReadsJson: dashboard.Reads(group),
		})
		if err != nil {
			return nil, err
		}
		var result struct {
			AsOf    string            `json:"as_of"`
			Results []json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal([]byte(resp.GetResultJson()), &result); err != nil || len(result.Results) != len(group) {
			return nil, status.Error(codes.Internal, "the graph read's result doesn't answer each tile")
		}
		read := dashboardRead{loadID: loadID, asOf: result.AsOf, outcomes: make(map[string]json.RawMessage, len(group))}
		for i, t := range group {
			read.outcomes[t.ID] = result.Results[i]
		}
		out = append(out, read)
	}
	return out, nil
}

// PreviewFoundryDashboardTile checks a tile and reads it once, as the caller.
func (s *Service) PreviewFoundryDashboardTile(ctx context.Context, req *foundryv1.PreviewFoundryDashboardTileRequest) (
	*foundryv1.PreviewFoundryDashboardTileResponse, error) {
	ws := req.GetWorkspaceId()
	if _, err := s.dashboardCallerOf(ctx, ws); err != nil {
		return nil, err
	}
	tile, err := dashboard.ParseTile([]byte(req.GetTileJson()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	seen := map[string]*foundrymodel.FoundryOntology{}
	if err := s.dashboardTileTarget(ctx, ws, tile, seen); err != nil {
		return nil, err
	}
	reads, err := s.readDashboardTiles(ctx, seen[tile.OntologyID], []dashboard.Tile{tile})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(tile)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "write the tile: %v", err)
	}
	return &foundryv1.PreviewFoundryDashboardTileResponse{TileJson: string(raw), LoadId: reads[0].loadID, AsOf: reads[0].asOf,
		OutcomeJson: string(reads[0].outcomes[tile.ID])}, nil
}

// PublishFoundryDashboard reads every tile as the caller, an owner or admin,
// and keeps the outputs as the dashboard's public snapshot at its link:
//  1. public links must be on, on the host (config.PublicDashboards) and for
//     the workspace (FoundryDashboardSettings), and the dashboard at the
//     revision the caller saw, with a tile;
//  2. each tile is read as ReadFoundryGraph reads it for the caller, who must
//     be able to query each ontology's database; a tile without an answer
//     refuses the whole publish, naming it and why;
//  3. the snapshot (dashboard.NewSnapshot) holds no workspace's, ontology's,
//     load's or person's id, and is bounded in size;
//  4. a dashboard published already keeps its link, unless it expired; the
//     first publish makes one, a token of 256 random bits whose SHA-256 is
//     what a request is looked up by, sealed with the connectors' keyring so
//     owners and admins are shown it again;
//  5. the link ends as req's expiry says, from now; unset, a live link keeps
//     its own and a new one never ends.
func (s *Service) PublishFoundryDashboard(ctx context.Context, req *foundryv1.PublishFoundryDashboardRequest) (*foundryv1.FoundryDashboardResponse, error) {
	ws := req.GetWorkspaceId()
	c, err := s.dashboardCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	settings, err := s.dashboardSettings(ctx, ws)
	if err != nil {
		return nil, err
	}
	if refused := publishRefusal(ws, s.publishBlock(c, settings)); refused != nil {
		return nil, refused
	}
	switch {
	case s.deps.ConnectorKeys == nil:
		return nil, status.Error(codes.Unavailable, "public dashboard links can't be kept on this host")
	case req.GetExpectedRevision() < 1:
		return nil, status.Error(codes.InvalidArgument, "expected_revision is required")
	}
	expiry, err := linkExpiry(req.GetExpiry(), time.Now())
	if err != nil {
		return nil, err
	}
	d, err := s.visibleDashboard(ctx, c, ws, req.GetId())
	if err != nil {
		return nil, err
	}
	if d.Revision != req.GetExpectedRevision() {
		return nil, foundryPrecondition(ws, "FOUNDRY_DASHBOARD_CHANGED",
			fmt.Sprintf("the dashboard changed since revision %d: it is at %d now; read it again", req.GetExpectedRevision(), d.Revision))
	}
	spec, err := dashboard.ParseSpec(d.Spec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the dashboard's spec: %v", err)
	}
	tiles := spec.Tiles()
	if len(tiles) == 0 {
		return nil, foundryPrecondition(ws, "FOUNDRY_DASHBOARD_EMPTY", "the dashboard has no tiles to publish")
	}
	var ontologyIDs []string
	byOntology := map[string][]dashboard.Tile{}
	for _, t := range tiles {
		if _, ok := byOntology[t.OntologyID]; !ok {
			ontologyIDs = append(ontologyIDs, t.OntologyID)
		}
		byOntology[t.OntologyID] = append(byOntology[t.OntologyID], t)
	}
	reads := make(map[string]dashboard.Read, len(tiles))
	for _, ontologyID := range ontologyIDs {
		group := byOntology[ontologyID]
		o, err := s.liveFoundryOntology(ctx, ws, ontologyID)
		if status.Code(err) == codes.NotFound {
			return nil, tileFailed(ws, group[0], "its ontology was deleted")
		}
		if err != nil {
			return nil, err
		}
		groups, err := s.readDashboardTiles(ctx, o, group)
		switch status.Code(err) {
		case codes.OK:
		case codes.NotFound, codes.FailedPrecondition, codes.InvalidArgument:
			if reason := errorReason(err); reason != "" {
				return nil, err
			}
			return nil, tileFailed(ws, group[0], status.Convert(err).Message())
		default:
			return nil, err
		}
		for _, g := range groups {
			for _, t := range group {
				outcome, ok := g.outcomes[t.ID]
				if !ok {
					continue
				}
				var answer struct {
					OK    json.RawMessage `json:"ok"`
					Error *struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(outcome, &answer); err != nil || answer.Error != nil || len(answer.OK) == 0 {
					why := "its read has no answer"
					if answer.Error != nil {
						why = answer.Error.Message
					}
					return nil, tileFailed(ws, t, why)
				}
				reads[t.ID] = dashboard.Read{AsOf: g.asOf, Answer: answer.OK}
			}
		}
	}
	snapshot, err := dashboard.NewSnapshot(d.Name, d.Description, spec, reads, time.Now())
	switch {
	case errors.Is(err, dashboard.ErrTooLarge):
		return nil, foundryPrecondition(ws, "FOUNDRY_DASHBOARD_TOO_LARGE", err.Error())
	case err != nil:
		return nil, foundryPrecondition(ws, "FOUNDRY_DASHBOARD_TILE_FAILED", err.Error())
	}
	token, err := newShareToken()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "make the link: %v", err)
	}
	sealed, keyID, err := s.deps.ConnectorKeys.Seal([]byte(token), shareTokenContext+d.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "seal the link: %v", err)
	}
	_, err = s.deps.DB.PublishFoundryDashboard(ctx, &foundrymodel.FoundryDashboardShare{
		DashboardID: d.ID, WorkspaceID: ws, TokenSHA256: hashToken(token), TokenSealed: sealed, TokenKeyID: keyID, Snapshot: snapshot,
		SpecRevision: d.Revision, OntologyIDs: ontologyIDs, PublishedByUserID: c.userID,
	}, expiry)
	if refused, ok := dashboardRefusal(ws, err); ok {
		return nil, refused
	}
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "dashboard not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "publish the dashboard: %v", err)
	}
	return s.dashboardResponse(ctx, c, ws, d.ID)
}

// errorReason is the ErrorInfo reason err carries, if any.
func errorReason(err error) string {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(interface{ GetReason() string }); ok {
			return info.GetReason()
		}
	}
	return ""
}

// tileFailed refuses a publish because tile t can't be read.
func tileFailed(workspaceID string, t dashboard.Tile, why string) error {
	return foundryPrecondition(workspaceID, "FOUNDRY_DASHBOARD_TILE_FAILED", fmt.Sprintf("tile %q (%s) can't be read: %s", t.Title, t.ID, why))
}

// newShareToken is a new link token: hxd_ and 32 random bytes, base64url.
func newShareToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "hxd_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *Service) UnpublishFoundryDashboard(ctx context.Context, req *foundryv1.UnpublishFoundryDashboardRequest) (*foundryv1.FoundryDashboardResponse, error) {
	ws := req.GetWorkspaceId()
	c, err := s.dashboardCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	if !c.manager {
		return nil, foundryPrecondition(ws, "FOUNDRY_DASHBOARD_PUBLIC_ADMIN_ONLY", "only an owner or admin ends a dashboard's public link")
	}
	d, err := s.visibleDashboard(ctx, c, ws, req.GetId())
	if err != nil {
		return nil, err
	}
	// One that isn't published is unpublished already.
	if err := s.deps.DB.UnpublishFoundryDashboard(ctx, ws, d.ID); err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "unpublish the dashboard: %v", err)
	}
	return s.dashboardResponse(ctx, c, ws, d.ID)
}

// GetSharedFoundryDashboard serves a public snapshot to Foundry's door, by
// its link token's SHA-256. Wherever public links are off, on the host or
// for the link's workspace, every link is NotFound, as is an expired link
// and a token of no snapshot.
func (s *Service) GetSharedFoundryDashboard(ctx context.Context, req *foundryv1.GetSharedFoundryDashboardRequest) (
	*foundryv1.GetSharedFoundryDashboardResponse, error) {
	a := actor(ctx)
	if a.Source != ports.SourceEventSource || a.EventSourceTokenID != "" || a.WorkspaceID != "" || a.UserID != "" || a.ServiceCredentialID != "" {
		return nil, status.Error(codes.PermissionDenied, "only Foundry's door serves shared dashboards")
	}
	hash := req.GetTokenSha256()
	if _, err := hex.DecodeString(hash); err != nil || len(hash) != 64 || strings.ToLower(hash) != hash {
		return nil, status.Error(codes.InvalidArgument, "token_sha256 is 64 lowercase hex digits")
	}
	if !s.deps.Cfg.PublicDashboards() {
		return nil, status.Error(codes.NotFound, "no dashboard is shared at this link")
	}
	share, err := s.deps.DB.GetSharedFoundryDashboard(ctx, hash)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "no dashboard is shared at this link")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the shared dashboard: %v", err)
	}
	settings, err := s.deps.DB.GetFoundryDashboardSettings(ctx, share.WorkspaceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the dashboard settings: %v", err)
	}
	if s.shareStatus(share, settings, time.Now()) != foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_LIVE {
		return nil, status.Error(codes.NotFound, "no dashboard is shared at this link")
	}
	return &foundryv1.GetSharedFoundryDashboardResponse{SnapshotJson: string(share.Snapshot)}, nil
}

// GetFoundryDashboardSettings returns the workspace's dashboard settings, to
// any member (never a grantee, whom the access rules refuse).
func (s *Service) GetFoundryDashboardSettings(ctx context.Context, req *foundryv1.GetFoundryDashboardSettingsRequest) (
	*foundryv1.FoundryDashboardSettingsResponse, error) {
	c, err := s.dashboardCallerOf(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	return s.dashboardSettingsResponse(ctx, c, req.GetWorkspaceId())
}

// UpdateFoundryDashboardSettings turns the workspace's public dashboard links
// on or off: owners only. It takes effect at once wherever a link is served
// or published, as both read the setting.
func (s *Service) UpdateFoundryDashboardSettings(ctx context.Context, req *foundryv1.UpdateFoundryDashboardSettingsRequest) (
	*foundryv1.FoundryDashboardSettingsResponse, error) {
	ws := req.GetWorkspaceId()
	c, err := s.dashboardCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	if c.grantee || !c.owner {
		return nil, status.Error(codes.PermissionDenied, "only an owner turns public dashboard links on or off")
	}
	if err := s.deps.DB.SetFoundryDashboardSettings(ctx, ws, c.userID, req.GetPublicLinks()); err != nil {
		return nil, status.Errorf(codes.Internal, "save the dashboard settings: %v", err)
	}
	return s.dashboardSettingsResponse(ctx, c, ws)
}

// dashboardSettingsResponse reads the workspace's dashboard settings as c
// sees them.
func (s *Service) dashboardSettingsResponse(ctx context.Context, c dashboardCaller, workspaceID string) (*foundryv1.FoundryDashboardSettingsResponse, error) {
	settings, err := s.deps.DB.GetFoundryDashboardSettings(ctx, workspaceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the dashboard settings: %v", err)
	}
	out := &foundryv1.FoundryDashboardSettings{PublicLinks: settings.PublicLinks, Available: s.deps.Cfg.PublicDashboards(),
		CanChange: !c.grantee && c.owner, UpdatedByUserId: settings.UpdatedByUserID}
	if settings.UpdatedAt != nil {
		out.UpdatedAt = timestamppb.New(*settings.UpdatedAt)
	}
	return &foundryv1.FoundryDashboardSettingsResponse{Settings: out}, nil
}

// dashboardToProto is d of a workspace with settings, as c sees it at now:
// what c may do with it, and its link's token only for an owner or admin,
// while the link hasn't expired.
func (s *Service) dashboardToProto(d *foundrymodel.FoundryDashboard, c dashboardCaller, settings foundrymodel.FoundryDashboardSettings,
	now time.Time) *foundryv1.FoundryDashboard {
	block := s.publishBlock(c, settings)
	out := &foundryv1.FoundryDashboard{
		Id: d.ID, Name: d.Name, Description: d.Description, SpecJson: string(d.Spec), Revision: d.Revision, CreatedByUserId: d.CreatedByUserID,
		UpdatedByUserId: d.UpdatedByUserID, Pinned: d.PinnedAt != nil, CreatedAt: timestamppb.New(d.CreatedAt), UpdatedAt: timestamppb.New(d.UpdatedAt),
		CanEdit: c.mayEdit(d), CanPin: !c.grantee && c.manager, CanPublish: block == foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_UNSPECIFIED,
		CanDelete: c.mayDelete(d), CanUnpublish: !c.grantee && c.manager && d.Share != nil, PublishBlock: block,
	}
	if d.PinnedAt != nil && d.PinnedByUserID != nil {
		out.PinnedAt, out.PinnedByUserId = timestamppb.New(*d.PinnedAt), *d.PinnedByUserID
	}
	if share := d.Share; share != nil {
		out.Share = &foundryv1.FoundryDashboardShare{SpecRevision: share.SpecRevision, PublishedAt: timestamppb.New(share.PublishedAt),
			FirstPublishedAt: timestamppb.New(share.FirstPublishedAt), PublishedByUserId: share.PublishedByUserID,
			Status: s.shareStatus(share, settings, now)}
		if share.ExpiresAt != nil {
			out.Share.ExpiresAt = timestamppb.New(*share.ExpiresAt)
		}
		if out.CanPin && s.deps.ConnectorKeys != nil && !share.Expired(now) {
			// A token that can't be opened (its key retired) is left out:
			// unpublishing and publishing again makes a new link. So is an
			// expired one, which no publish revives.
			if token, err := s.deps.ConnectorKeys.Open(share.TokenSealed, share.TokenKeyID, shareTokenContext+d.ID); err == nil &&
				shareTokenPattern.Match(token) {
				out.Share.Token = string(token)
			}
		}
	}
	return out
}
