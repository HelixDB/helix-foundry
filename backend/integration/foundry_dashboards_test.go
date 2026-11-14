package integration

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

const (
	dashWorkspace = "01JZZZZZZZZZZZZZZZZZZZZZZW"
	dashID        = "01JZZZZZZZZZZZZZZZZZZZZZZD"
	dashTile      = `{"id":"t1","title":"Invoices","ontology_id":"ont-1","load":{"kind":"current"},"query":{"start":{"type":"Invoice"},"return":{"count":{}}},"visual":{"kind":"figure"}}`
)

// dashBackend answers the dashboard RPCs as Foundry does, recording what it was asked.
type dashBackend struct {
	FoundryBackendAPI
	FoundryActionAPI
	ontologies map[string]bool
	creates    []*foundryv1.CreateFoundryDashboardRequest
	updates    []*foundryv1.UpdateFoundryDashboardRequest
	deletes    []*foundryv1.DeleteFoundryDashboardRequest
	pins       []*foundryv1.PinFoundryDashboardRequest
	publishes  []*foundryv1.PublishFoundryDashboardRequest
	unpublish  int
	previews   []*foundryv1.PreviewFoundryDashboardTileRequest
}

func dashboardProto() *foundryv1.FoundryDashboard {
	return &foundryv1.FoundryDashboard{Id: dashID, Name: "Revenue", Revision: 3, Pinned: true, CanEdit: true, CanPin: true, CanPublish: true,
		SpecJson: `{"v":1,"tiles":[` + dashTile + `],"layout":{"rows":[["t1"]]}}`,
		Share:    &foundryv1.FoundryDashboardShare{Token: "hxd_token", SpecRevision: 2, PublishedAt: timestamppb.Now()}}
}

func (b *dashBackend) ListFoundryDashboards(context.Context, *foundryv1.ListFoundryDashboardsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryDashboardsResponse, error) {
	listed := dashboardProto()
	listed.SpecJson, listed.Share = "", nil
	return &foundryv1.ListFoundryDashboardsResponse{Dashboards: []*foundryv1.FoundryDashboard{listed}, CanCreate: true}, nil
}

func (b *dashBackend) GetFoundryDashboard(context.Context, *foundryv1.GetFoundryDashboardRequest, ...grpc.CallOption) (*foundryv1.FoundryDashboardResponse, error) {
	return &foundryv1.FoundryDashboardResponse{Dashboard: dashboardProto()}, nil
}

func (b *dashBackend) GetFoundryOntology(_ context.Context, r *foundryv1.GetFoundryOntologyRequest, _ ...grpc.CallOption) (*foundryv1.FoundryOntologyResponse, error) {
	if !b.ontologies[r.GetOntologyId()] {
		return nil, status.Error(codes.NotFound, "ontology not found")
	}
	return &foundryv1.FoundryOntologyResponse{Ontology: &foundryv1.FoundryOntology{Id: r.GetOntologyId()}}, nil
}

func (b *dashBackend) PreviewFoundryDashboardTile(_ context.Context, r *foundryv1.PreviewFoundryDashboardTileRequest, _ ...grpc.CallOption) (
	*foundryv1.PreviewFoundryDashboardTileResponse, error) {
	b.previews = append(b.previews, r)
	return &foundryv1.PreviewFoundryDashboardTileResponse{TileJson: r.GetTileJson(), LoadId: "load-1", AsOf: "2026-11-14",
		OutcomeJson: `{"ok":{"kind":"count","count":12345678901234567890}}`}, nil
}

func (b *dashBackend) CreateFoundryDashboard(_ context.Context, r *foundryv1.CreateFoundryDashboardRequest, _ ...grpc.CallOption) (*foundryv1.FoundryDashboardResponse, error) {
	b.creates = append(b.creates, r)
	return &foundryv1.FoundryDashboardResponse{Dashboard: dashboardProto()}, nil
}

func (b *dashBackend) UpdateFoundryDashboard(_ context.Context, r *foundryv1.UpdateFoundryDashboardRequest, _ ...grpc.CallOption) (*foundryv1.FoundryDashboardResponse, error) {
	b.updates = append(b.updates, r)
	return &foundryv1.FoundryDashboardResponse{Dashboard: dashboardProto()}, nil
}

func (b *dashBackend) DeleteFoundryDashboard(_ context.Context, r *foundryv1.DeleteFoundryDashboardRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	b.deletes = append(b.deletes, r)
	return &emptypb.Empty{}, nil
}

func (b *dashBackend) PinFoundryDashboard(_ context.Context, r *foundryv1.PinFoundryDashboardRequest, _ ...grpc.CallOption) (*foundryv1.FoundryDashboardResponse, error) {
	b.pins = append(b.pins, r)
	return &foundryv1.FoundryDashboardResponse{Dashboard: dashboardProto()}, nil
}

func (b *dashBackend) PublishFoundryDashboard(_ context.Context, r *foundryv1.PublishFoundryDashboardRequest, _ ...grpc.CallOption) (*foundryv1.FoundryDashboardResponse, error) {
	b.publishes = append(b.publishes, r)
	return &foundryv1.FoundryDashboardResponse{Dashboard: dashboardProto()}, nil
}

func (b *dashBackend) UnpublishFoundryDashboard(context.Context, *foundryv1.UnpublishFoundryDashboardRequest, ...grpc.CallOption) (*foundryv1.FoundryDashboardResponse, error) {
	b.unpublish++
	return &foundryv1.FoundryDashboardResponse{Dashboard: &foundryv1.FoundryDashboard{Id: dashID}}, nil
}

func dashService(public bool) (*Service, *dashBackend) {
	b := &dashBackend{ontologies: map[string]bool{"ont-1": true}}
	return &Service{Foundry: b, Actions: b, Confirmations: &memConfirmations{}, FoundryURL: "https://foundry.example", PublicDashboards: public}, b
}

func tileArgs(t *testing.T) map[string]any {
	t.Helper()
	var tile map[string]any
	require.NoError(t, json.Unmarshal([]byte(dashTile), &tile))
	return tile
}

func TestDashboardReadTools(t *testing.T) {
	s, b := dashService(true)
	list := called(t, s, toolFoundryListDashboards, map[string]any{"workspace_id": dashWorkspace})
	assert.Equal(t, true, list["can_create"])
	listed := list["dashboards"].([]any)[0].(map[string]any)
	assert.Equal(t, "Revenue", listed["name"])
	assert.NotContains(t, listed, "spec", "a list leaves specs out")
	assert.Equal(t, "https://foundry.example/dashboards/"+dashID+"?workspace="+dashWorkspace, listed["url"])

	got := called(t, s, toolFoundryGetDashboard, map[string]any{"workspace_id": dashWorkspace, "dashboard_id": dashID})
	assert.EqualValues(t, 3, got["revision"])
	spec := got["spec"].(map[string]any)
	assert.Len(t, spec["tiles"], 1)
	share := got["share"].(map[string]any)
	assert.Equal(t, "https://foundry.example/share/hxd_token", share["url"])
	assert.EqualValues(t, 2, share["spec_revision"])

	preview := called(t, s, toolFoundryPreviewDashboardTile, map[string]any{"workspace_id": dashWorkspace, "tile": tileArgs(t)})
	assert.Equal(t, "load-1", preview["load_id"])
	require.Len(t, b.previews, 1)
	assert.JSONEq(t, `{"id":"t1","title":"Invoices","ontology_id":"ont-1","load":{"kind":"current"},"query":{"return":{"count":{}},"start":{"type":"Invoice"}},"visual":{"kind":"figure"}}`,
		b.previews[0].GetTileJson(), "the tile is sent as a spec keeps it")
	raw, err := json.Marshal(preview["outcome"])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"12345678901234567890"`, "a large count stays exact, as a string")

	result, err := connect(t, s, reader()).CallTool(context.Background(), &mcp.CallToolParams{Name: toolFoundryPreviewDashboardTile,
		Arguments: map[string]any{"workspace_id": dashWorkspace, "tile": map[string]any{"id": "T"}}})
	require.NoError(t, err)
	assert.True(t, result.IsError, "a tile that isn't one says why")
	assert.Contains(t, result.Content[0].(*mcp.TextContent).Text, "id")
}

func TestDashboardToolsAreOffered(t *testing.T) {
	tools := func(identity *Identity) []string {
		s, _ := dashService(true)
		listed, err := connect(t, s, identity).ListTools(context.Background(), nil)
		require.NoError(t, err)
		var names []string
		for _, tool := range listed.Tools {
			names = append(names, tool.Name)
		}
		return names
	}
	names := tools(reader())
	for _, name := range []string{toolFoundryListDashboards, toolFoundryGetDashboard, toolFoundryPreviewDashboardTile} {
		assert.Contains(t, names, name)
	}
	browser := reader()
	browser.HasPermission = func(p Permission) bool { return p == PermissionWorkspaceRead }
	names = tools(browser)
	assert.Contains(t, names, toolFoundryGetDashboard)
	assert.NotContains(t, names, toolFoundryPreviewDashboardTile, "a preview reads a graph")
	key := grantKey(PermissionDatabaseQueryRead)
	assert.NotContains(t, tools(key), toolFoundryListDashboards, "dashboards are people's")
}

func TestDashboardOperations(t *testing.T) {
	prepare := func(s *Service, operation, target string, payload map[string]any) (FoundryOperation, error) {
		return s.ValidateFoundryOperation(context.Background(), reader(), FoundryOperationInput{Operation: operation, Target: target, Payload: payload})
	}
	spec := map[string]any{"v": 1, "tiles": []any{tileArgs(t)}, "layout": map[string]any{"rows": []any{[]any{"t1"}}}}
	dashboardTarget := "dashboard:" + dashWorkspace + "/" + dashID

	t.Run("each runs its call with the payload prepared", func(t *testing.T) {
		s, b := dashService(true)
		op, err := prepare(s, "create_dashboard", "workspace:"+dashWorkspace, map[string]any{"name": " Revenue ", "spec": spec})
		require.NoError(t, err)
		assert.Contains(t, string(op.CanonicalPayload()), `"name":"Revenue"`)
		result, err := op.Execute(context.Background(), s, reader(), "conf-1")
		require.NoError(t, err)
		require.Len(t, b.creates, 1)
		assert.Equal(t, "conf-1", b.creates[0].GetClientToken(), "the confirmation is the create's client token")
		assert.Contains(t, b.creates[0].GetSpecJson(), `"query":{"return":{"count":{}},"start":{"type":"Invoice"}}`)
		assert.Equal(t, dashID, result.(FoundryDashboardOutput).ID)

		empty, err := prepare(s, "create_dashboard", "workspace:"+dashWorkspace, map[string]any{"name": "Empty"})
		require.NoError(t, err)
		assert.Contains(t, string(empty.CanonicalPayload()), `"spec":{"v":1,"tiles":[],"layout":{"rows":[]}}`)

		op, err = prepare(s, "update_dashboard", dashboardTarget, map[string]any{"expected_revision": json.Number("3"), "name": "Renamed", "spec": spec})
		require.NoError(t, err)
		_, err = op.Execute(context.Background(), s, reader(), "conf-2")
		require.NoError(t, err)
		assert.EqualValues(t, 3, b.updates[0].GetExpectedRevision())
		assert.Equal(t, "Renamed", b.updates[0].GetName())
		assert.Nil(t, b.updates[0].Description, "what an update leaves out stays")

		op, err = prepare(s, "pin_dashboard", dashboardTarget, map[string]any{"pinned": true})
		require.NoError(t, err)
		_, err = op.Execute(context.Background(), s, reader(), "conf-3")
		require.NoError(t, err)
		assert.True(t, b.pins[0].GetPinned())

		op, err = prepare(s, "publish_dashboard", dashboardTarget, map[string]any{"expected_revision": json.Number("3")})
		require.NoError(t, err)
		published, err := op.Execute(context.Background(), s, reader(), "conf-4")
		require.NoError(t, err)
		assert.Equal(t, "https://foundry.example/share/hxd_token", published.(FoundryDashboardOutput).Share.URL)

		op, err = prepare(s, "unpublish_dashboard", dashboardTarget, map[string]any{})
		require.NoError(t, err)
		_, err = op.Execute(context.Background(), s, reader(), "conf-5")
		require.NoError(t, err)
		assert.Equal(t, 1, b.unpublish)

		op, err = prepare(s, "delete_dashboard", dashboardTarget, map[string]any{})
		require.NoError(t, err)
		deleted, err := op.Execute(context.Background(), s, reader(), "conf-6")
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"deleted": true, "dashboard_id": dashID}, deleted)
		assert.Equal(t, dashID, b.deletes[0].GetId())
	})

	t.Run("refuses what the backend would", func(t *testing.T) {
		s, _ := dashService(true)
		otherOntology := map[string]any{"v": 1, "tiles": []any{func() map[string]any {
			tile := tileArgs(t)
			tile["ontology_id"] = "ont-9"
			return tile
		}()}, "layout": map[string]any{"rows": []any{[]any{"t1"}}}}
		for name, c := range map[string]struct {
			operation, target string
			payload           map[string]any
			want              string
		}{
			"create on a dashboard":  {"create_dashboard", dashboardTarget, map[string]any{"name": "x"}, "create_dashboard takes"},
			"no name":                {"create_dashboard", "workspace:" + dashWorkspace, map[string]any{"name": " "}, "create_dashboard takes"},
			"control in the name":    {"create_dashboard", "workspace:" + dashWorkspace, map[string]any{"name": "a\u0000b"}, "create_dashboard takes"},
			"long description":       {"create_dashboard", "workspace:" + dashWorkspace, map[string]any{"name": "x", "description": strings.Repeat("d", 1001)}, "create_dashboard takes"},
			"unknown field":          {"create_dashboard", "workspace:" + dashWorkspace, map[string]any{"name": "x", "tiles": []any{}}, "create_dashboard takes"},
			"bad spec":               {"create_dashboard", "workspace:" + dashWorkspace, map[string]any{"name": "x", "spec": map[string]any{"v": 2}}, "this Foundry reads v 1"},
			"another ontology":       {"create_dashboard", "workspace:" + dashWorkspace, map[string]any{"name": "x", "spec": otherOntology}, "doesn't have"},
			"an ontology target":     {"update_dashboard", "ontology:" + dashWorkspace + "/ont-1", map[string]any{"expected_revision": json.Number("1"), "name": "x"}, "takes target dashboard:"},
			"no revision":            {"update_dashboard", dashboardTarget, map[string]any{"name": "x"}, "update_dashboard takes"},
			"no change":              {"update_dashboard", dashboardTarget, map[string]any{"expected_revision": json.Number("1")}, "update_dashboard takes"},
			"update to a bad spec":   {"update_dashboard", dashboardTarget, map[string]any{"expected_revision": json.Number("1"), "spec": map[string]any{"v": 1, "tiles": []any{tileArgs(t)}}}, "leaves out tile t1"},
			"delete with a payload":  {"delete_dashboard", dashboardTarget, map[string]any{"x": 1}, "empty payload"},
			"pin without pinned":     {"pin_dashboard", dashboardTarget, map[string]any{}, "pin_dashboard takes"},
			"publish no revision":    {"publish_dashboard", dashboardTarget, map[string]any{}, "publish_dashboard takes"},
			"unpublish with payload": {"unpublish_dashboard", dashboardTarget, map[string]any{"x": 1}, "empty payload"},
		} {
			_, err := prepare(s, c.operation, c.target, c.payload)
			require.Error(t, err, name)
			assert.Contains(t, ToolError(err).Error()+err.Error(), c.want, name)
		}
	})

	t.Run("offers public links only where the host does", func(t *testing.T) {
		s, _ := dashService(false)
		for _, operation := range []string{"publish_dashboard", "unpublish_dashboard"} {
			_, err := prepare(s, operation, dashboardTarget, map[string]any{"expected_revision": json.Number("1")})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not available here")
		}
		names, payloads := foundryOperations(false, false)
		assert.NotContains(t, names, "publish_dashboard")
		assert.Contains(t, names, "create_dashboard")
		assert.NotContains(t, payloads, "publish_dashboard")
		names, _ = foundryOperations(false, true)
		assert.True(t, slices.Contains(names, any("publish_dashboard")))
	})

	t.Run("refuses a caller the host refuses", func(t *testing.T) {
		s, b := dashService(true)
		refusing := reader()
		refusing.Authorize = func(context.Context, any, Grant, ...string) error { return errCodeNotFound }
		_, err := s.ValidateFoundryOperation(context.Background(), refusing, FoundryOperationInput{Operation: "delete_dashboard", Target: dashboardTarget, Payload: map[string]any{}})
		assert.ErrorIs(t, err, errCodeNotFound)
		assert.Empty(t, b.deletes)
		assert.Equal(t, "dashboard", TargetKind(dashboardTarget))
	})
}
