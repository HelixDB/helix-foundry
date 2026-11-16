package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// dashboardStore keeps dashboards in memory with the MySQL store's rules (a
// revision per edit, the editor's rights, the caps), over foundryStore's
// ontologies, versions and loads.
type dashboardStore struct {
	*foundryStore
	dashboards map[string]*foundrymodel.FoundryDashboard
	snapshots  map[string]json.RawMessage
	questions  map[string]*foundrymodel.FoundryQuestion
	order      []string
	settings   map[string]foundrymodel.FoundryDashboardSettings
}

func (s *dashboardStore) CreateFoundryDashboard(_ context.Context, d *foundrymodel.FoundryDashboard, max int) error {
	count := 0
	for _, other := range s.dashboards {
		if other.WorkspaceID != d.WorkspaceID {
			continue
		}
		if other.CreatedByUserID == d.CreatedByUserID && *other.ClientToken == *d.ClientToken {
			return foundrystore.ErrConflict
		}
		count++
	}
	if count >= max {
		return foundrymodel.FoundryDashboardRefusal{Reason: foundrymodel.FoundryDashboardLimit, Detail: "too many"}
	}
	stored := *d
	stored.Revision, stored.UpdatedByUserID, stored.CreatedAt, stored.UpdatedAt = 1, d.CreatedByUserID, time.Now(), time.Now()
	s.dashboards[d.ID] = &stored
	s.order = append(s.order, d.ID)
	return nil
}

func (s *dashboardStore) GetFoundryDashboard(_ context.Context, ws, id string) (*foundrymodel.FoundryDashboard, error) {
	d, ok := s.dashboards[id]
	if !ok || d.WorkspaceID != ws {
		return nil, foundrystore.ErrNotFound
	}
	copied := *d
	return &copied, nil
}

func (s *dashboardStore) GetFoundryDashboardByClientToken(_ context.Context, ws, userID, token string) (*foundrymodel.FoundryDashboard, error) {
	for _, d := range s.dashboards {
		if d.WorkspaceID == ws && d.CreatedByUserID == userID && *d.ClientToken == token {
			copied := *d
			return &copied, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *dashboardStore) ListFoundryDashboards(_ context.Context, ws string, pinnedOnly bool) ([]*foundrymodel.FoundryDashboard, error) {
	var out []*foundrymodel.FoundryDashboard
	for _, id := range s.order {
		d, ok := s.dashboards[id]
		if ok && d.WorkspaceID == ws && (!pinnedOnly || d.PinnedAt != nil) {
			listed := *d
			listed.Spec = nil
			out = append(out, &listed)
		}
	}
	return out, nil
}

func (s *dashboardStore) editable(ws, id string, editor foundrymodel.FoundryDashboardEditor, expected int64) (*foundrymodel.FoundryDashboard, error) {
	d, ok := s.dashboards[id]
	switch {
	case !ok || d.WorkspaceID != ws:
		return nil, foundrystore.ErrNotFound
	case expected != 0 && d.Revision != expected:
		return nil, foundrymodel.FoundryDashboardRefusal{Reason: foundrymodel.FoundryDashboardChanged, Detail: "changed"}
	case !editor.Manager && (d.CreatedByUserID != editor.UserID || d.PinnedAt != nil):
		return nil, foundrymodel.FoundryDashboardRefusal{Reason: foundrymodel.FoundryDashboardNotEditable, Detail: "not yours"}
	}
	return d, nil
}

func (s *dashboardStore) UpdateFoundryDashboard(_ context.Context, ws, id string, editor foundrymodel.FoundryDashboardEditor, expected int64,
	u foundrymodel.FoundryDashboardUpdate) error {
	d, err := s.editable(ws, id, editor, expected)
	if err != nil {
		return err
	}
	if u.Name != nil {
		d.Name = *u.Name
	}
	if u.Description != nil {
		d.Description = *u.Description
	}
	if u.Spec != nil {
		d.Spec = u.Spec
	}
	d.Revision++
	d.UpdatedByUserID = editor.UserID
	return nil
}

func (s *dashboardStore) SetFoundryDashboardPinned(_ context.Context, ws, id, userID string, expected int64, pinned bool, _ int) error {
	d, err := s.editable(ws, id, foundrymodel.FoundryDashboardEditor{Manager: true}, expected)
	if err != nil {
		return err
	}
	if pinned {
		now := time.Now()
		d.PinnedAt, d.PinnedByUserID = &now, &userID
	} else {
		d.PinnedAt, d.PinnedByUserID = nil, nil
	}
	return nil
}

func (s *dashboardStore) DeleteFoundryDashboard(_ context.Context, ws, id string, editor foundrymodel.FoundryDashboardEditor) error {
	d, err := s.editable(ws, id, editor, 0)
	if err != nil {
		return err
	}
	if d.Share != nil && !editor.Manager {
		return foundrymodel.FoundryDashboardRefusal{Reason: foundrymodel.FoundryDashboardNotEditable, Detail: "published"}
	}
	delete(s.dashboards, id)
	delete(s.snapshots, id)
	return nil
}

func (s *dashboardStore) PublishFoundryDashboard(_ context.Context, share *foundrymodel.FoundryDashboardShare,
	expiry foundrymodel.FoundryDashboardLinkExpiry) (*foundrymodel.FoundryDashboardShare, error) {
	d, err := s.editable(share.WorkspaceID, share.DashboardID, foundrymodel.FoundryDashboardEditor{Manager: true}, share.SpecRevision)
	if err != nil {
		return nil, err
	}
	if d.Share != nil && d.Share.Expired(time.Now()) {
		d.Share = nil
	}
	stored := *share
	stored.ExpiresAt = expiry.At
	if d.Share != nil {
		stored.TokenSHA256, stored.TokenSealed, stored.TokenKeyID = d.Share.TokenSHA256, d.Share.TokenSealed, d.Share.TokenKeyID
		if expiry.Keep {
			stored.ExpiresAt = d.Share.ExpiresAt
		}
	}
	s.snapshots[d.ID] = stored.Snapshot
	stored.Snapshot = nil
	d.Share = &stored
	return &stored, nil
}

func (s *dashboardStore) UnpublishFoundryDashboard(_ context.Context, ws, id string) error {
	d, ok := s.dashboards[id]
	if !ok || d.WorkspaceID != ws || d.Share == nil {
		return foundrystore.ErrNotFound
	}
	d.Share = nil
	delete(s.snapshots, id)
	return nil
}

func (s *dashboardStore) GetSharedFoundryDashboard(_ context.Context, hash string) (*foundrymodel.FoundryDashboardShare, error) {
	for _, d := range s.dashboards {
		if d.Share != nil && d.Share.TokenSHA256 == hash {
			share := *d.Share
			share.Snapshot = s.snapshots[d.ID]
			return &share, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *dashboardStore) GetFoundryDashboardSettings(_ context.Context, ws string) (foundrymodel.FoundryDashboardSettings, error) {
	if settings, ok := s.settings[ws]; ok {
		return settings, nil
	}
	return foundrymodel.FoundryDashboardSettings{WorkspaceID: ws, PublicLinks: true}, nil
}

func (s *dashboardStore) SetFoundryDashboardSettings(_ context.Context, ws, userID string, publicLinks bool) error {
	now := time.Now()
	s.settings[ws] = foundrymodel.FoundryDashboardSettings{WorkspaceID: ws, PublicLinks: publicLinks, UpdatedByUserID: userID, UpdatedAt: &now}
	return nil
}

func (s *dashboardStore) GetFoundryQuestion(_ context.Context, ontologyID, id string) (*foundrymodel.FoundryQuestion, error) {
	q, ok := s.questions[id]
	if !ok || q.OntologyID != ontologyID {
		return nil, foundrystore.ErrNotFound
	}
	return q, nil
}

// dashboardPeople are the workspace's members by user id.
type dashboardPeople map[string]ports.Role

func (dashboardPeople) MayManageAI(context.Context, string, string) bool    { return false }
func (dashboardPeople) DisplayName(context.Context, string) (string, error) { return "", nil }
func (p dashboardPeople) Member(_ context.Context, _, userID string) (ports.Member, error) {
	return ports.Member{Role: p[userID]}, nil
}

var (
	dashboardOwner  = ports.Actor{Source: ports.SourceUser, UserID: "user-owner", WorkspaceID: "ws-1", QueryRead: ports.AllProjects()}
	dashboardMember = ports.Actor{Source: ports.SourceUser, UserID: "user-member", WorkspaceID: "ws-1", QueryRead: ports.AllProjects()}
	dashboardOther  = ports.Actor{Source: ports.SourceMCP, UserID: "user-other", WorkspaceID: "ws-1", QueryRead: ports.AllProjects()}
	dashboardAdmin  = ports.Actor{Source: ports.SourceUser, UserID: "user-admin", WorkspaceID: "ws-1", QueryRead: ports.AllProjects()}
	dashboardDoor   = ports.Actor{Source: ports.SourceEventSource}
)

const (
	countTile = `{"id":"t1","title":"Customers","ontology_id":"ont-1","load":{"kind":"current"},"query":{"start":{"type":"Customer"},"return":{"count":{}}},"visual":{"kind":"figure"}}`
	groupTile = `{"id":"t2","title":"By country","ontology_id":"ont-1","load":{"kind":"pinned","load_id":"load-1"},"query":{"start":{"type":"Customer"},"return":{"group_count":{"at":0,"prop":"country"}}}}`
)

func twoTiles() string {
	return `{"v":1,"tiles":[` + countTile + `,` + groupTile + `],"layout":{"rows":[["t1","t2"]]}}`
}

type dashboardFixture struct {
	s      *Service
	store  *dashboardStore
	reader *fakeFoundryRead
}

func newDashboardFixture(t *testing.T) dashboardFixture {
	t.Helper()
	s, fs, _ := foundryService(t)
	def, _ := json.Marshal(map[string]any{"format": "agent_bundle", "status": "done", "ontology_toml": "version = \"v3\"\n", "mappings_toml": ""})
	fs.versions = append(fs.versions, &foundrymodel.OntologyVersion{ID: "v-3", WorkspaceID: "ws-1", OntologyID: "ont-1", Version: 3,
		Format: foundrymodel.OntologyFormatAgentBundle, Definition: def})
	fs.loads = append(fs.loads, &foundrymodel.FoundryLoad{ID: "load-1", WorkspaceID: "ws-1", OntologyID: "ont-1", VersionID: "v-3", Version: 3,
		Status: foundrymodel.FoundryLoadReady})
	current := "load-1"
	fs.ontologies["ont-1"].CurrentLoadID = &current
	store := &dashboardStore{foundryStore: fs, dashboards: map[string]*foundrymodel.FoundryDashboard{}, snapshots: map[string]json.RawMessage{},
		questions: map[string]*foundrymodel.FoundryQuestion{}, settings: map[string]foundrymodel.FoundryDashboardSettings{}}
	reader := &fakeFoundryRead{result: `{"as_of":"2026-11-14","results":[` + queryOK(`{"kind":"count","count":3}`) + `,` +
		queryOK(`{"kind":"rows","total":1,"rows":[{"cells":[{"type":"Customer","id":"scus_1","name":"Globex"}],"edges":[]}]}`) + `]}`}
	s.deps.DB, s.deps.FoundryRead, s.deps.ConnectorKeys = store, reader, aiKeyring(t)
	s.deps.People = dashboardPeople{"user-owner": ports.RoleOwner, "user-member": ports.RoleMember, "user-other": ports.RoleMember,
		"user-admin": ports.RoleAdmin}
	return dashboardFixture{s: s, store: store, reader: reader}
}

// deleteLoad marks one of the fixture's loads deleted.
func (f dashboardFixture) deleteLoad(id string) {
	for _, l := range f.store.loads {
		if l.ID == id {
			l.Status = foundrymodel.FoundryLoadDeleted
		}
	}
}

// queryOK is a query read's outcome: its answer, result as given.
func queryOK(result string) string {
	return `{"ok":{"kind":"query","query_echo":{"start":{"type":"Customer","ids":["scus_1"],"text":null,"where":[]},"steps":[],"filters":[],"return":{"count":{"distinct_at":null}}},` +
		`"result":` + result + `,"stats":{"matched":1}}}`
}

func asActor(a ports.Actor) context.Context { return ports.WithActor(context.Background(), a) }

func (f dashboardFixture) create(t *testing.T, a ports.Actor, spec string) *foundryv1.FoundryDashboard {
	t.Helper()
	resp, err := f.s.CreateFoundryDashboard(asActor(a), &foundryv1.CreateFoundryDashboardRequest{WorkspaceId: "ws-1", Name: "Customers",
		SpecJson: spec, ClientToken: "c-" + a.UserID + fmt.Sprint(len(f.store.dashboards))})
	require.NoError(t, err)
	return resp.GetDashboard()
}

func TestDashboardCallers(t *testing.T) {
	f := newDashboardFixture(t)
	req := &foundryv1.ListFoundryDashboardsRequest{WorkspaceId: "ws-1"}
	for _, a := range []ports.Actor{{Source: ports.SourceMCP, ServiceCredentialID: "key-1", WorkspaceID: "ws-1"}, dashboardDoor, {Source: ports.SourceUser}} {
		_, err := f.s.ListFoundryDashboards(asActor(a), req)
		assert.Equal(t, codes.PermissionDenied, status.Code(err), "%+v", a)
	}
	_, err := f.s.ListFoundryDashboards(asActor(dashboardOwner), &foundryv1.ListFoundryDashboardsRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	mine := f.create(t, dashboardMember, "")
	pinned := f.create(t, dashboardOwner, "")
	_, err = f.s.PinFoundryDashboard(asActor(dashboardOwner), &foundryv1.PinFoundryDashboardRequest{WorkspaceId: "ws-1", Id: pinned.GetId(), Pinned: true, ExpectedRevision: 1})
	require.NoError(t, err)

	list, err := f.s.ListFoundryDashboards(asActor(dashboardOther), req)
	require.NoError(t, err)
	assert.True(t, list.GetCanCreate())
	assert.Len(t, list.GetDashboards(), 2, "every member sees every dashboard")

	// A grantee sees the pinned ones, and may change none.
	grantee := access.WithCaller(asActor(ports.Actor{Source: ports.SourceGrantee, UserID: "user-g", WorkspaceID: "ws-1"}),
		access.Grantee{WorkspaceID: "ws-1", Principal: access.Principal{Kind: access.PrincipalUser, ID: "user-g"}})
	list, err = f.s.ListFoundryDashboards(grantee, req)
	require.NoError(t, err)
	require.Len(t, list.GetDashboards(), 1)
	assert.False(t, list.GetCanCreate())
	got := list.GetDashboards()[0]
	assert.Equal(t, pinned.GetId(), got.GetId())
	assert.False(t, got.GetCanEdit() || got.GetCanPin() || got.GetCanPublish())
	_, err = f.s.GetFoundryDashboard(grantee, &foundryv1.GetFoundryDashboardRequest{WorkspaceId: "ws-1", Id: mine.GetId()})
	assert.Equal(t, codes.NotFound, status.Code(err), "an unpinned dashboard is no grantee's to see")
	_, err = f.s.DeleteFoundryDashboard(grantee, &foundryv1.DeleteFoundryDashboardRequest{WorkspaceId: "ws-1", Id: mine.GetId()})
	assert.Equal(t, codes.NotFound, status.Code(err), "nor to be refused")
	key := access.WithCaller(asActor(ports.Actor{Source: ports.SourceGrantee, ServiceCredentialID: "key-1", WorkspaceID: "ws-1"}),
		access.Grantee{WorkspaceID: "ws-1", Principal: access.Principal{Kind: access.PrincipalServiceCredential, ID: "key-1"}})
	_, err = f.s.ListFoundryDashboards(key, req)
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "a key, even one granted sources, has no dashboards")
	for _, method := range []string{foundryv1.FoundryService_CreateFoundryDashboard_FullMethodName, foundryv1.FoundryService_PublishFoundryDashboard_FullMethodName,
		foundryv1.FoundryService_GetFoundryDashboardSettings_FullMethodName, foundryv1.FoundryService_UpdateFoundryDashboardSettings_FullMethodName} {
		assert.Equal(t, access.Deny, access.RuleOf(method))
	}
}

func TestCreateFoundryDashboard(t *testing.T) {
	f := newDashboardFixture(t)
	resp, err := f.s.CreateFoundryDashboard(asActor(dashboardMember), &foundryv1.CreateFoundryDashboardRequest{WorkspaceId: "ws-1", Name: "  Customers ",
		Description: " Weekly ", SpecJson: twoTiles(), ClientToken: "c1"})
	require.NoError(t, err)
	d := resp.GetDashboard()
	assert.Equal(t, "Customers", d.GetName())
	assert.Equal(t, "Weekly", d.GetDescription())
	assert.EqualValues(t, 1, d.GetRevision())
	assert.True(t, d.GetCanEdit(), "its creator edits it")
	assert.False(t, d.GetCanPin() || d.GetCanPublish(), "a member neither pins nor publishes")
	assert.Contains(t, d.GetSpecJson(), `"load":{"kind":"pinned","load_id":"load-1"}`)

	again, err := f.s.CreateFoundryDashboard(asActor(dashboardMember), &foundryv1.CreateFoundryDashboardRequest{WorkspaceId: "ws-1", Name: "Other",
		ClientToken: "c1"})
	require.NoError(t, err)
	assert.Equal(t, d.GetId(), again.GetDashboard().GetId(), "the same client token is the same dashboard")

	empty, err := f.s.CreateFoundryDashboard(asActor(dashboardMember), &foundryv1.CreateFoundryDashboardRequest{WorkspaceId: "ws-1", Name: "Empty", ClientToken: "c2"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"v":1,"tiles":[],"layout":{"rows":[]}}`, empty.GetDashboard().GetSpecJson())

	for name, c := range map[string]struct {
		req  *foundryv1.CreateFoundryDashboardRequest
		code codes.Code
	}{
		"no token":         {&foundryv1.CreateFoundryDashboardRequest{Name: "x"}, codes.InvalidArgument},
		"long token":       {&foundryv1.CreateFoundryDashboardRequest{Name: "x", ClientToken: strings.Repeat("c", 65)}, codes.InvalidArgument},
		"no name":          {&foundryv1.CreateFoundryDashboardRequest{Name: " ", ClientToken: "x"}, codes.InvalidArgument},
		"long name":        {&foundryv1.CreateFoundryDashboardRequest{Name: strings.Repeat("n", 121), ClientToken: "x"}, codes.InvalidArgument},
		"control in name":  {&foundryv1.CreateFoundryDashboardRequest{Name: "a\x00b", ClientToken: "x"}, codes.InvalidArgument},
		"long description": {&foundryv1.CreateFoundryDashboardRequest{Name: "x", Description: strings.Repeat("d", 1001), ClientToken: "x"}, codes.InvalidArgument},
		"bad spec":         {&foundryv1.CreateFoundryDashboardRequest{Name: "x", SpecJson: `{"v":2}`, ClientToken: "x"}, codes.InvalidArgument},
		"another workspace's ontology": {&foundryv1.CreateFoundryDashboardRequest{Name: "x", ClientToken: "x",
			SpecJson: strings.Replace(twoTiles(), `"ontology_id":"ont-1"`, `"ontology_id":"ont-2"`, 1)}, codes.InvalidArgument},
		"a load it doesn't have": {&foundryv1.CreateFoundryDashboardRequest{Name: "x", ClientToken: "x",
			SpecJson: strings.Replace(twoTiles(), `"load-1"`, `"load-9"`, 1)}, codes.InvalidArgument},
	} {
		c.req.WorkspaceId = "ws-1"
		_, err := f.s.CreateFoundryDashboard(asActor(dashboardMember), c.req)
		assert.Equal(t, c.code, status.Code(err), name)
	}

	for i := len(f.store.dashboards); i < maxDashboards; i++ {
		f.create(t, dashboardOwner, "")
	}
	_, err = f.s.CreateFoundryDashboard(asActor(dashboardOwner), &foundryv1.CreateFoundryDashboardRequest{WorkspaceId: "ws-1", Name: "x", ClientToken: "last"})
	assert.Equal(t, "FOUNDRY_DASHBOARD_LIMIT", reasonOf(err))
}

func TestEditingDashboards(t *testing.T) {
	update := func(f dashboardFixture, a ports.Actor, id string, revision int64, name string) (*foundryv1.FoundryDashboardResponse, error) {
		return f.s.UpdateFoundryDashboard(asActor(a), &foundryv1.UpdateFoundryDashboardRequest{WorkspaceId: "ws-1", Id: id, ExpectedRevision: revision, Name: &name})
	}
	f := newDashboardFixture(t)
	d := f.create(t, dashboardMember, twoTiles())

	resp, err := update(f, dashboardMember, d.GetId(), 1, "Renamed")
	require.NoError(t, err)
	assert.Equal(t, "Renamed", resp.GetDashboard().GetName())
	assert.EqualValues(t, 2, resp.GetDashboard().GetRevision())
	assert.Equal(t, d.GetSpecJson(), resp.GetDashboard().GetSpecJson(), "what an edit leaves out stays")

	_, err = update(f, dashboardMember, d.GetId(), 1, "Stale")
	assert.Equal(t, "FOUNDRY_DASHBOARD_CHANGED", reasonOf(err))
	_, err = update(f, dashboardOther, d.GetId(), 2, "Theirs")
	assert.Equal(t, "FOUNDRY_DASHBOARD_NOT_EDITABLE", reasonOf(err), "another member's dashboard")
	_, err = f.s.DeleteFoundryDashboard(asActor(dashboardOther), &foundryv1.DeleteFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
	assert.Equal(t, "FOUNDRY_DASHBOARD_NOT_EDITABLE", reasonOf(err))

	spec := `{"v":1,"tiles":[` + countTile + `],"layout":{"rows":[["t1"]]}}`
	resp, err = f.s.UpdateFoundryDashboard(asActor(dashboardOwner), &foundryv1.UpdateFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(),
		ExpectedRevision: 2, SpecJson: &spec})
	require.NoError(t, err, "an owner edits anyone's")
	assert.Equal(t, "Renamed", resp.GetDashboard().GetName())
	assert.NotContains(t, resp.GetDashboard().GetSpecJson(), "t2")

	_, err = f.s.PinFoundryDashboard(asActor(dashboardMember), &foundryv1.PinFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(), Pinned: true, ExpectedRevision: 3})
	assert.Equal(t, "FOUNDRY_DASHBOARD_NOT_EDITABLE", reasonOf(err), "a member pins nothing")
	_, err = f.s.PinFoundryDashboard(asActor(dashboardOwner), &foundryv1.PinFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(), Pinned: true, ExpectedRevision: 2})
	assert.Equal(t, "FOUNDRY_DASHBOARD_CHANGED", reasonOf(err), "a pin of a revision its pinner didn't see")
	_, err = f.s.PinFoundryDashboard(asActor(dashboardOwner), &foundryv1.PinFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(), Pinned: true})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "a pin names the revision it saw")
	pinned, err := f.s.PinFoundryDashboard(asActor(dashboardOwner), &foundryv1.PinFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(), Pinned: true, ExpectedRevision: 3})
	require.NoError(t, err)
	assert.True(t, pinned.GetDashboard().GetPinned())
	assert.Equal(t, "user-owner", pinned.GetDashboard().GetPinnedByUserId())
	got, err := f.s.GetFoundryDashboard(asActor(dashboardMember), &foundryv1.GetFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
	require.NoError(t, err)
	assert.False(t, got.GetDashboard().GetCanEdit(), "its creator, once it is pinned")
	_, err = update(f, dashboardMember, d.GetId(), 3, "Mine again")
	assert.Equal(t, "FOUNDRY_DASHBOARD_NOT_EDITABLE", reasonOf(err))

	_, err = f.s.UpdateFoundryDashboard(asActor(dashboardOwner), &foundryv1.UpdateFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(), ExpectedRevision: 3})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "an edit changes something")
	bad := `{"v":2}`
	_, err = f.s.UpdateFoundryDashboard(asActor(dashboardOwner), &foundryv1.UpdateFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(), ExpectedRevision: 3, SpecJson: &bad})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = update(f, dashboardOwner, "missing", 1, "x")
	assert.Equal(t, codes.NotFound, status.Code(err))

	_, err = f.s.PinFoundryDashboard(asActor(dashboardOwner), &foundryv1.PinFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(), Pinned: false, ExpectedRevision: 3})
	require.NoError(t, err)
	_, err = f.s.DeleteFoundryDashboard(asActor(dashboardMember), &foundryv1.DeleteFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
	require.NoError(t, err, "unpinned, it is its creator's again")
	_, err = f.s.GetFoundryDashboard(asActor(dashboardMember), &foundryv1.GetFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestEditingAroundAStaleTile(t *testing.T) {
	f := newDashboardFixture(t)
	d := f.create(t, dashboardMember, twoTiles())
	// t2's pinned load is deleted, as loads past the five kept are.
	f.deleteLoad("load-1")

	retitled := strings.Replace(twoTiles(), `"title":"Customers"`, `"title":"All customers"`, 1)
	resp, err := f.s.UpdateFoundryDashboard(asActor(dashboardMember), &foundryv1.UpdateFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(),
		ExpectedRevision: 1, SpecJson: &retitled})
	require.NoError(t, err, "a tile whose load went is no reason to refuse an edit of another")
	assert.Contains(t, resp.GetDashboard().GetSpecJson(), "All customers")

	added := `{"v":1,"tiles":[` + countTile + `,` + groupTile + `,` + strings.Replace(groupTile, `"id":"t2"`, `"id":"t3"`, 1) + `],"layout":{"rows":[["t1","t2","t3"]]}}`
	_, err = f.s.UpdateFoundryDashboard(asActor(dashboardMember), &foundryv1.UpdateFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(),
		ExpectedRevision: 2, SpecJson: &added})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "a new tile pinned to a deleted load")
	assert.Contains(t, status.Convert(err).Message(), "tile t3 pins a load")
	moved := strings.Replace(twoTiles(), `{"kind":"current"}`, `{"kind":"pinned","load_id":"load-1"}`, 1)
	_, err = f.s.UpdateFoundryDashboard(asActor(dashboardMember), &foundryv1.UpdateFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(),
		ExpectedRevision: 2, SpecJson: &moved})
	assert.Contains(t, status.Convert(err).Message(), "tile t1 pins a load", "a tile moved to a deleted load")
}

func TestPreviewFoundryDashboardTile(t *testing.T) {
	f := newDashboardFixture(t)
	f.reader.result = `{"as_of":"2026-11-14","results":[` + queryOK(`{"kind":"count","count":3}`) + `]}`
	resp, err := f.s.PreviewFoundryDashboardTile(asActor(dashboardMember), &foundryv1.PreviewFoundryDashboardTileRequest{WorkspaceId: "ws-1", TileJson: countTile})
	require.NoError(t, err)
	assert.Equal(t, "load-1", resp.GetLoadId(), "the current load")
	assert.Equal(t, "2026-11-14", resp.GetAsOf())
	assert.JSONEq(t, queryOK(`{"kind":"count","count":3}`), resp.GetOutcomeJson())
	require.Len(t, f.reader.requests, 1)
	assert.JSONEq(t, `[{"query":{"return":{"count":{}},"start":{"type":"Customer"}}}]`, f.reader.requests[0].GetReadsJson())

	_, err = f.s.PreviewFoundryDashboardTile(asActor(dashboardMember), &foundryv1.PreviewFoundryDashboardTileRequest{WorkspaceId: "ws-1", TileJson: `{"id":"t1"}`})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	noScope := dashboardMember
	noScope.QueryRead = ports.NoProjects()
	_, err = f.s.PreviewFoundryDashboardTile(asActor(noScope), &foundryv1.PreviewFoundryDashboardTileRequest{WorkspaceId: "ws-1", TileJson: countTile})
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "one who can't read the database previews nothing")
	f.store.ontologies["ont-1"].CurrentLoadID = nil
	_, err = f.s.PreviewFoundryDashboardTile(asActor(dashboardMember), &foundryv1.PreviewFoundryDashboardTileRequest{WorkspaceId: "ws-1", TileJson: countTile})
	assert.Equal(t, "FOUNDRY_DASHBOARD_NOT_LOADED", reasonOf(err))
	assert.Len(t, f.reader.requests, 1)
}

func TestPublishingADashboard(t *testing.T) {
	publish := func(f dashboardFixture, a ports.Actor, id string, revision int64) (*foundryv1.FoundryDashboardResponse, error) {
		return f.s.PublishFoundryDashboard(asActor(a), &foundryv1.PublishFoundryDashboardRequest{WorkspaceId: "ws-1", Id: id, ExpectedRevision: revision})
	}
	shared := func(f dashboardFixture, token string) (*foundryv1.GetSharedFoundryDashboardResponse, error) {
		return f.s.GetSharedFoundryDashboard(asActor(dashboardDoor), &foundryv1.GetSharedFoundryDashboardRequest{TokenSha256: hashToken(token)})
	}

	t.Run("reads every tile as its owner and serves the snapshot at its link", func(t *testing.T) {
		f := newDashboardFixture(t)
		d := f.create(t, dashboardMember, twoTiles())
		_, err := publish(f, dashboardMember, d.GetId(), 1)
		assert.Equal(t, "FOUNDRY_DASHBOARD_PUBLIC_ADMIN_ONLY", reasonOf(err), "a member publishes nothing")
		assert.Equal(t, foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_ADMIN_ONLY, d.GetPublishBlock())

		resp, err := publish(f, dashboardOwner, d.GetId(), 1)
		require.NoError(t, err)
		share := resp.GetDashboard().GetShare()
		require.NotNil(t, share)
		assert.Regexp(t, `^hxd_[A-Za-z0-9_-]{43}$`, share.GetToken())
		assert.EqualValues(t, 1, share.GetSpecRevision())
		assert.Equal(t, "user-owner", share.GetPublishedByUserId())
		assert.Equal(t, foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_LIVE, share.GetStatus())
		assert.Nil(t, share.GetExpiresAt(), "a link never ends unless asked")
		require.Len(t, f.reader.requests, 1, "both tiles read the same load: one read")
		assert.Equal(t, "load-1", f.reader.requests[0].GetLoadId())

		got, err := shared(f, share.GetToken())
		require.NoError(t, err)
		snapshot := got.GetSnapshotJson()
		assert.Contains(t, snapshot, `"name":"Globex"`)
		for _, private := range []string{"ws-1", "ont-1", "load-1", "user-owner", "user-member", "scus_1", d.GetId()} {
			assert.NotContains(t, snapshot, private, "a snapshot names no id")
		}
		var parsed struct {
			Tiles []struct {
				ID     string          `json:"id"`
				Output json.RawMessage `json:"output"`
			} `json:"tiles"`
		}
		require.NoError(t, json.Unmarshal([]byte(snapshot), &parsed))
		require.Len(t, parsed.Tiles, 2)
		assert.JSONEq(t, `{"kind":"count","count":3}`, string(parsed.Tiles[0].Output))

		member, err := f.s.GetFoundryDashboard(asActor(dashboardMember), &foundryv1.GetFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
		require.NoError(t, err)
		assert.Empty(t, member.GetDashboard().GetShare().GetToken(), "only owners and admins see the link")
		assert.NotNil(t, member.GetDashboard().GetShare().GetPublishedAt())
		assert.True(t, member.GetDashboard().GetCanEdit(), "its creator still edits it")
		assert.False(t, member.GetDashboard().GetCanDelete() || member.GetDashboard().GetCanUnpublish(), "but takes its link down neither way")
		_, err = f.s.DeleteFoundryDashboard(asActor(dashboardMember), &foundryv1.DeleteFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
		assert.Equal(t, "FOUNDRY_DASHBOARD_NOT_EDITABLE", reasonOf(err), "a member deletes no published dashboard")
		assert.True(t, resp.GetDashboard().GetCanDelete() && resp.GetDashboard().GetCanUnpublish(), "an owner does both")

		// Republishing reads again and keeps the link.
		name := "Renamed"
		_, err = f.s.UpdateFoundryDashboard(asActor(dashboardOwner), &foundryv1.UpdateFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(),
			ExpectedRevision: 1, Name: &name})
		require.NoError(t, err)
		_, err = publish(f, dashboardOwner, d.GetId(), 1)
		assert.Equal(t, "FOUNDRY_DASHBOARD_CHANGED", reasonOf(err), "a publish of what the owner didn't see")
		again, err := publish(f, dashboardOwner, d.GetId(), 2)
		require.NoError(t, err)
		assert.Equal(t, share.GetToken(), again.GetDashboard().GetShare().GetToken())
		got, err = shared(f, share.GetToken())
		require.NoError(t, err)
		assert.Contains(t, got.GetSnapshotJson(), `"name":"Renamed"`)

		// Unpublishing ends the link; doing it again changes nothing.
		for range 2 {
			resp, err := f.s.UnpublishFoundryDashboard(asActor(dashboardOwner), &foundryv1.UnpublishFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
			require.NoError(t, err)
			assert.Nil(t, resp.GetDashboard().GetShare())
		}
		_, err = shared(f, share.GetToken())
		assert.Equal(t, codes.NotFound, status.Code(err))
		_, err = f.s.UnpublishFoundryDashboard(asActor(dashboardMember), &foundryv1.UnpublishFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
		assert.Equal(t, "FOUNDRY_DASHBOARD_PUBLIC_ADMIN_ONLY", reasonOf(err))
	})

	t.Run("an admin publishes as an owner does", func(t *testing.T) {
		f := newDashboardFixture(t)
		d := f.create(t, dashboardMember, twoTiles())
		resp, err := publish(f, dashboardAdmin, d.GetId(), 1)
		require.NoError(t, err)
		assert.NotEmpty(t, resp.GetDashboard().GetShare().GetToken())
		assert.True(t, resp.GetDashboard().GetCanPublish())
		assert.Equal(t, foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_UNSPECIFIED, resp.GetDashboard().GetPublishBlock())
	})

	t.Run("a link ends when it expires, and publishing again makes a new one", func(t *testing.T) {
		f := newDashboardFixture(t)
		d := f.create(t, dashboardOwner, twoTiles())
		expire := func(e foundryv1.FoundryDashboardLinkExpiry) (*foundryv1.FoundryDashboard, error) {
			resp, err := f.s.PublishFoundryDashboard(asActor(dashboardOwner), &foundryv1.PublishFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId(),
				ExpectedRevision: 1, Expiry: e})
			return resp.GetDashboard(), err
		}
		_, err := expire(foundryv1.FoundryDashboardLinkExpiry(99))
		assert.Equal(t, codes.InvalidArgument, status.Code(err))

		got, err := expire(foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_30)
		require.NoError(t, err)
		token := got.GetShare().GetToken()
		require.NotNil(t, got.GetShare().GetExpiresAt())
		assert.WithinDuration(t, time.Now().Add(30*24*time.Hour), got.GetShare().GetExpiresAt().AsTime(), time.Minute)

		// Unset keeps a live link's own; another choice moves it.
		got, err = expire(foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_UNSPECIFIED)
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(30*24*time.Hour), got.GetShare().GetExpiresAt().AsTime(), time.Minute)
		got, err = expire(foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_7)
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(7*24*time.Hour), got.GetShare().GetExpiresAt().AsTime(), time.Minute)
		assert.Equal(t, token, got.GetShare().GetToken(), "a new expiry keeps the link")
		got, err = expire(foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_NEVER)
		require.NoError(t, err)
		assert.Nil(t, got.GetShare().GetExpiresAt())
		got, err = expire(foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_90)
		require.NoError(t, err)
		_, err = shared(f, token)
		require.NoError(t, err)

		// Past its expiry it is served as an unpublished link, shown as
		// expired, and its token is no one's to copy.
		past := time.Now().Add(-time.Second)
		f.store.dashboards[d.GetId()].Share.ExpiresAt = &past
		_, err = shared(f, token)
		assert.Equal(t, codes.NotFound, status.Code(err))
		read, err := f.s.GetFoundryDashboard(asActor(dashboardOwner), &foundryv1.GetFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
		require.NoError(t, err)
		assert.Equal(t, foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_EXPIRED, read.GetDashboard().GetShare().GetStatus())
		assert.Empty(t, read.GetDashboard().GetShare().GetToken())
		assert.True(t, read.GetDashboard().GetCanUnpublish() && read.GetDashboard().GetCanPublish())

		got, err = expire(foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_UNSPECIFIED)
		require.NoError(t, err)
		assert.NotEqual(t, token, got.GetShare().GetToken(), "an expired link is never revived")
		assert.Nil(t, got.GetShare().GetExpiresAt(), "a new link keeps no old expiry")
		_, err = shared(f, token)
		assert.Equal(t, codes.NotFound, status.Code(err))
		_, err = shared(f, got.GetShare().GetToken())
		require.NoError(t, err)
	})

	t.Run("an owner turns public links off for the workspace", func(t *testing.T) {
		f := newDashboardFixture(t)
		d := f.create(t, dashboardOwner, twoTiles())
		resp, err := publish(f, dashboardOwner, d.GetId(), 1)
		require.NoError(t, err)
		token := resp.GetDashboard().GetShare().GetToken()
		settings := func(a ports.Actor) (*foundryv1.FoundryDashboardSettings, error) {
			resp, err := f.s.GetFoundryDashboardSettings(asActor(a), &foundryv1.GetFoundryDashboardSettingsRequest{WorkspaceId: "ws-1"})
			return resp.GetSettings(), err
		}
		turn := func(a ports.Actor, on bool) (*foundryv1.FoundryDashboardSettings, error) {
			resp, err := f.s.UpdateFoundryDashboardSettings(asActor(a), &foundryv1.UpdateFoundryDashboardSettingsRequest{WorkspaceId: "ws-1", PublicLinks: on})
			return resp.GetSettings(), err
		}

		got, err := settings(dashboardMember)
		require.NoError(t, err)
		assert.True(t, got.GetPublicLinks() && got.GetAvailable(), "on until an owner turns them off")
		assert.False(t, got.GetCanChange(), "a member reads it")
		assert.Empty(t, got.GetUpdatedByUserId())
		assert.Nil(t, got.GetUpdatedAt())
		got, err = settings(dashboardAdmin)
		require.NoError(t, err)
		assert.False(t, got.GetCanChange(), "an admin reads it")
		for _, a := range []ports.Actor{dashboardMember, dashboardAdmin} {
			_, err = turn(a, false)
			assert.Equal(t, codes.PermissionDenied, status.Code(err), a.UserID)
		}
		_, err = settings(dashboardDoor)
		assert.Equal(t, codes.PermissionDenied, status.Code(err))

		got, err = turn(dashboardOwner, false)
		require.NoError(t, err)
		assert.False(t, got.GetPublicLinks())
		assert.True(t, got.GetCanChange())
		assert.Equal(t, "user-owner", got.GetUpdatedByUserId())
		assert.NotNil(t, got.GetUpdatedAt())

		_, err = shared(f, token)
		assert.Equal(t, codes.NotFound, status.Code(err), "off, no link of the workspace shows")
		_, err = publish(f, dashboardOwner, d.GetId(), 1)
		assert.Equal(t, "FOUNDRY_DASHBOARD_PUBLIC_OFF", reasonOf(err))
		_, err = publish(f, dashboardMember, d.GetId(), 1)
		assert.Equal(t, "FOUNDRY_DASHBOARD_PUBLIC_ADMIN_ONLY", reasonOf(err), "a member is told who may, before the switch")
		read, err := f.s.GetFoundryDashboard(asActor(dashboardOwner), &foundryv1.GetFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
		require.NoError(t, err)
		assert.False(t, read.GetDashboard().GetCanPublish())
		assert.Equal(t, foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_WORKSPACE_OFF, read.GetDashboard().GetPublishBlock())
		assert.Equal(t, foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_WORKSPACE_OFF, read.GetDashboard().GetShare().GetStatus())
		assert.True(t, read.GetDashboard().GetCanUnpublish(), "off, an owner still ends a link")
		list, err := f.s.ListFoundryDashboards(asActor(dashboardOwner), &foundryv1.ListFoundryDashboardsRequest{WorkspaceId: "ws-1"})
		require.NoError(t, err)
		assert.Equal(t, foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_WORKSPACE_OFF, list.GetDashboards()[0].GetShare().GetStatus())

		// On again, the link not ended meanwhile shows again.
		_, err = turn(dashboardOwner, true)
		require.NoError(t, err)
		_, err = shared(f, token)
		require.NoError(t, err)
	})

	t.Run("refuses what it can't publish", func(t *testing.T) {
		f := newDashboardFixture(t)
		empty := f.create(t, dashboardOwner, "")
		_, err := publish(f, dashboardOwner, empty.GetId(), 1)
		assert.Equal(t, "FOUNDRY_DASHBOARD_EMPTY", reasonOf(err))
		_, err = publish(f, dashboardOwner, empty.GetId(), 0)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))

		d := f.create(t, dashboardOwner, twoTiles())
		f.reader.result = `{"as_of":"2026-11-14","results":[` + queryOK(`{"kind":"count","count":3}`) + `,{"error":{"code":"too_broad","message":"too broad","limit":1,"count":2}}]}`
		_, err = publish(f, dashboardOwner, d.GetId(), 1)
		assert.Equal(t, "FOUNDRY_DASHBOARD_TILE_FAILED", reasonOf(err))
		assert.Contains(t, status.Convert(err).Message(), `"By country" (t2) can't be read: too broad`)

		f.reader.result = `{"as_of":"2026-11-14","results":[` + queryOK(`{"kind":"count","count":3}`) + `]}`
		_, err = publish(f, dashboardOwner, d.GetId(), 1)
		assert.Equal(t, codes.Internal, status.Code(err), "a result that doesn't answer each tile")

		f.reader.result = `{"as_of":"2026-11-14","results":[` + queryOK(`{"kind":"count","count":3}`) + `,` + queryOK(`{"kind":"rows","total":1,"rows":[{"cells":["`+
			strings.Repeat("x", 990)+`"],"edges":[]}`+strings.Repeat(`,{"cells":["`+strings.Repeat("y", 990)+`"],"edges":[]}`, 600)+`]}`) + `]}`
		_, err = publish(f, dashboardOwner, d.GetId(), 1)
		assert.Equal(t, "FOUNDRY_DASHBOARD_TOO_LARGE", reasonOf(err))

		noScope := dashboardOwner
		noScope.QueryRead = ports.SelectedProjects([]string{"proj-other"})
		_, err = publish(f, noScope, d.GetId(), 1)
		assert.Equal(t, codes.PermissionDenied, status.Code(err), "an owner who can't read a tile's database")

		f.store.ontologies["ont-1"].CurrentLoadID = nil
		_, err = publish(f, dashboardOwner, d.GetId(), 1)
		assert.Equal(t, "FOUNDRY_DASHBOARD_NOT_LOADED", reasonOf(err))

		deleted := time.Now()
		f.store.ontologies["ont-1"].DeletedAt = &deleted
		_, err = publish(f, dashboardOwner, d.GetId(), 1)
		assert.Equal(t, "FOUNDRY_DASHBOARD_TILE_FAILED", reasonOf(err))
		assert.Contains(t, status.Convert(err).Message(), "deleted")
		assert.Empty(t, f.store.snapshots, "nothing refused was kept")
	})

	t.Run("off wherever public links are", func(t *testing.T) {
		f := newDashboardFixture(t)
		d := f.create(t, dashboardOwner, twoTiles())
		resp, err := publish(f, dashboardOwner, d.GetId(), 1)
		require.NoError(t, err)
		token := resp.GetDashboard().GetShare().GetToken()

		f.s.deps.Cfg.Host.Local = true
		_, err = publish(f, dashboardOwner, d.GetId(), 1)
		assert.Equal(t, "FOUNDRY_DASHBOARD_PUBLIC_UNAVAILABLE", reasonOf(err), "a standalone install never publishes")
		_, err = publish(f, dashboardMember, d.GetId(), 1)
		assert.Equal(t, "FOUNDRY_DASHBOARD_PUBLIC_UNAVAILABLE", reasonOf(err), "nor sends a member to an owner")
		_, err = shared(f, token)
		assert.Equal(t, codes.NotFound, status.Code(err))
		got, err := f.s.GetFoundryDashboard(asActor(dashboardOwner), &foundryv1.GetFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
		require.NoError(t, err)
		assert.False(t, got.GetDashboard().GetCanPublish())
		assert.Equal(t, foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_UNAVAILABLE, got.GetDashboard().GetPublishBlock())
		assert.Equal(t, foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_UNAVAILABLE, got.GetDashboard().GetShare().GetStatus())
		off, err := f.s.GetFoundryDashboardSettings(asActor(dashboardOwner), &foundryv1.GetFoundryDashboardSettingsRequest{WorkspaceId: "ws-1"})
		require.NoError(t, err)
		assert.False(t, off.GetSettings().GetAvailable())

		f.s.deps.Cfg.Host.Local = false
		f.s.deps.Cfg.Foundry.Dashboards.PublicEnabled = false
		_, err = shared(f, token)
		assert.Equal(t, codes.NotFound, status.Code(err), "turning links off stops every one at once")

		// Off, an owner still takes a link down, so turning links on again
		// revives none they meant to end.
		got, err = f.s.GetFoundryDashboard(asActor(dashboardOwner), &foundryv1.GetFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
		require.NoError(t, err)
		assert.False(t, got.GetDashboard().GetCanPublish())
		assert.True(t, got.GetDashboard().GetCanUnpublish())
		_, err = f.s.UnpublishFoundryDashboard(asActor(dashboardOwner), &foundryv1.UnpublishFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
		require.NoError(t, err)
		f.s.deps.Cfg.Foundry.Dashboards.PublicEnabled = true
		_, err = shared(f, token)
		assert.Equal(t, codes.NotFound, status.Code(err))
		got, err = f.s.GetFoundryDashboard(asActor(dashboardOwner), &foundryv1.GetFoundryDashboardRequest{WorkspaceId: "ws-1", Id: d.GetId()})
		require.NoError(t, err)
		assert.False(t, got.GetDashboard().GetCanUnpublish(), "nothing left to unpublish")
	})

	t.Run("serves only the door, by a token's hash", func(t *testing.T) {
		f := newDashboardFixture(t)
		for _, a := range []ports.Actor{dashboardOwner, {Source: ports.SourceEventSource, EventSourceTokenID: "tok"},
			{Source: ports.SourceEventSource, WorkspaceID: "ws-1"}} {
			_, err := f.s.GetSharedFoundryDashboard(asActor(a), &foundryv1.GetSharedFoundryDashboardRequest{TokenSha256: hashToken("x")})
			assert.Equal(t, codes.PermissionDenied, status.Code(err), "%+v", a)
		}
		for _, hash := range []string{"", "abc", strings.ToUpper(hashToken("x")), strings.Repeat("z", 64)} {
			_, err := f.s.GetSharedFoundryDashboard(asActor(dashboardDoor), &foundryv1.GetSharedFoundryDashboardRequest{TokenSha256: hash})
			assert.Equal(t, codes.InvalidArgument, status.Code(err), hash)
		}
		_, err := shared(f, "hxd_nothing")
		assert.Equal(t, codes.NotFound, status.Code(err))
	})
}

func TestAddFoundryDashboardQuestionVisual(t *testing.T) {
	f := newDashboardFixture(t)
	f.store.questions["q-1"] = &foundrymodel.FoundryQuestion{ID: "q-1", WorkspaceID: "ws-1", OntologyID: "ont-1", LoadID: "load-1",
		Question: "How many customers are in each country?", Status: foundrymodel.FoundryQuestionAnswered,
		Result: json.RawMessage(`{"status":"answered","answer":{"blocks":[],"visuals":[{"evidence_id":"e2","kind":"bar"}]},"citations":[],` +
			`"queries":[{"evidence_id":"e2","query":{"start":{"type":"Customer"},"return":{"group_count":{"at":0,"prop":"country"}}}}],"usage":{}}`)}
	d := f.create(t, dashboardMember, `{"v":1,"tiles":[`+countTile+`],"layout":{"rows":[["t1"]]}}`)
	add := func(revision int64, pin bool, evidence string) (*foundryv1.FoundryDashboardResponse, error) {
		return f.s.AddFoundryDashboardQuestionVisual(asActor(dashboardMember), &foundryv1.AddFoundryDashboardQuestionVisualRequest{WorkspaceId: "ws-1",
			Id: d.GetId(), ExpectedRevision: revision, OntologyId: "ont-1", QuestionId: "q-1", EvidenceId: evidence, PinLoad: pin})
	}
	resp, err := add(1, false, "e2")
	require.NoError(t, err)
	var spec struct {
		Tiles []struct {
			ID     string                        `json:"id"`
			Title  string                        `json:"title"`
			Load   struct{ Kind, LoadID string } `json:"load"`
			Visual struct{ Kind string }         `json:"visual"`
		} `json:"tiles"`
		Layout struct{ Rows [][]string } `json:"layout"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.GetDashboard().GetSpecJson()), &spec))
	require.Len(t, spec.Tiles, 2)
	added := spec.Tiles[1]
	assert.Equal(t, "t2", added.ID)
	assert.Equal(t, "How many customers are in each country?", added.Title, "a visual without a title takes the question's")
	assert.Equal(t, "current", added.Load.Kind)
	assert.Equal(t, "bar", added.Visual.Kind)
	assert.Equal(t, [][]string{{"t1"}, {"t2"}}, spec.Layout.Rows)

	resp, err = add(2, true, "e2")
	require.NoError(t, err)
	assert.Contains(t, resp.GetDashboard().GetSpecJson(), `{"id":"t3","title":"How many customers are in each country?","ontology_id":"ont-1","load":{"kind":"pinned","load_id":"load-1"}`)

	_, err = add(2, false, "e2")
	assert.Equal(t, "FOUNDRY_DASHBOARD_CHANGED", reasonOf(err))
	_, err = add(3, false, "e9")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "a result the question didn't record")
	_, err = f.s.AddFoundryDashboardQuestionVisual(asActor(dashboardOther), &foundryv1.AddFoundryDashboardQuestionVisualRequest{WorkspaceId: "ws-1",
		Id: d.GetId(), ExpectedRevision: 3, OntologyId: "ont-1", QuestionId: "q-1", EvidenceId: "e2"})
	assert.Equal(t, "FOUNDRY_DASHBOARD_NOT_EDITABLE", reasonOf(err), "another member's dashboard")
	_, err = f.s.AddFoundryDashboardQuestionVisual(asActor(dashboardMember), &foundryv1.AddFoundryDashboardQuestionVisualRequest{WorkspaceId: "ws-1",
		Id: d.GetId(), ExpectedRevision: 3, OntologyId: "ont-1", QuestionId: "q-x", EvidenceId: "e2"})
	assert.Equal(t, codes.NotFound, status.Code(err))
	assert.Equal(t, "t1", freeTileID(nil))

	f.deleteLoad("load-1")
	_, err = add(3, true, "e2")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err), "pinning a load deleted since the question")
	assert.Contains(t, status.Convert(err).Message(), "the question's load was deleted")
	_, err = add(3, false, "e2")
	require.NoError(t, err, "the current load's still reads")
}
