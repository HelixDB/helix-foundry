package integration

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/helixdb/helix-foundry/backend/dashboard"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// Dashboards (package dashboard): the read tools, and the operations helix_foundry_prepare_operation
// prepares. A dashboard holds queries, never data: each of its tiles is one typed query of an
// ontology's load, which whoever views it reads as themselves.
const (
	toolFoundryListDashboards       = "helix_foundry_list_dashboards"
	toolFoundryGetDashboard         = "helix_foundry_get_dashboard"
	toolFoundryPreviewDashboardTile = "helix_foundry_preview_dashboard_tile"
)

// dashboardSpecDoc says what a spec is, for the tools' and the operations' descriptions.
const dashboardSpecDoc = `A spec is {"v": 1, "tiles": [...], "layout": {"rows": [["t1", "t2"], ["t3"]]}}: at most 20 tiles, each in exactly one row of 1 to 3. ` +
	`A tile is {"id": "t1" (1 to 32 lowercase letters, digits, - or _, starting with a letter or digit), "title": 1 to 80 characters, "ontology_id", ` +
	`"load": {"kind": "current"} (whichever load is current when it is viewed) or {"kind": "pinned", "load_id"}, ` +
	`"query": a typed query, the query read of ` + toolFoundryReadGraph + ` (` + toolFoundryReadGuide + ` gives its schema): at most 200 rows or 100 groups, ` +
	`"visual": {"kind": "table" | "bar" | "line" | "figure"} or {} to show it as its output suits}. ` +
	`An answer's visual (helix_foundry_ask's) is a tile already: its question's queries[].query with the visual's kind and title.`

type FoundryDashboardInput struct {
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"Helix workspace identifier. May be empty when the caller has only one workspace."`
	DashboardID string `json:"dashboard_id" jsonschema:"Foundry dashboard identifier."`
}

func (in *FoundryDashboardInput) Workspace() *string { return &in.WorkspaceID }

type PreviewFoundryDashboardTileInput struct {
	WorkspaceID string         `json:"workspace_id,omitempty" jsonschema:"Helix workspace identifier. May be empty when the caller has only one workspace."`
	Tile        map[string]any `json:"tile" jsonschema:"The tile, as a spec holds it."`
}

func (in *PreviewFoundryDashboardTileInput) Workspace() *string { return &in.WorkspaceID }

type ListFoundryDashboardsOutput struct {
	ContentTrust string                   `json:"content_trust"`
	Dashboards   []FoundryDashboardOutput `json:"dashboards"`
	CanCreate    bool                     `json:"can_create"`
}

// FoundryDashboardOutput is a dashboard as the tools return it.
type FoundryDashboardOutput struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Revision is what update_dashboard, pin_dashboard and publish_dashboard name as expected_revision.
	Revision int64 `json:"revision" jsonschema:"What update_dashboard, pin_dashboard and publish_dashboard name as expected_revision."`
	Pinned   bool  `json:"pinned"`
	// See FoundryVersionOutput.Definition: the description keeps the schema an object.
	Spec         any                          `json:"spec,omitempty" jsonschema:"Untrusted dashboard spec: its tiles and layout. Left out of a list. An integer past 2^53 in a query comes as a string of its digits: write it back as a number."`
	CanEdit      bool                         `json:"can_edit"`
	CanPin       bool                         `json:"can_pin"`
	CanPublish   bool                         `json:"can_publish"`
	CanDelete    bool                         `json:"can_delete"`
	CanUnpublish bool                         `json:"can_unpublish"`
	PublishBlock string                       `json:"publish_block,omitempty" jsonschema:"Why you may not publish it at a public link, when can_publish is false: admin_only (only owners and admins do), workspace_off (an owner turned public links off for the workspace) or unavailable (this host has none)."`
	Share        *FoundryDashboardShareOutput `json:"share,omitempty"`
	URL          string                       `json:"url,omitempty" jsonschema:"Its page in Foundry: how to share it inside the workspace. Every member who opens it signs in and reads it as themselves, live."`
}

// FoundryDashboardShareOutput is a dashboard's public snapshot. Its link is never returned: a
// tool's output goes to an agent, its model provider and its transcripts, and a link is a
// credential to the data. Owners and admins copy it from the dashboard's page in Foundry (url).
type FoundryDashboardShareOutput struct {
	Published    bool   `json:"published" jsonschema:"Always true: the dashboard has a public link, which its page in Foundry shows owners and admins. No tool returns the link."`
	SpecRevision int64  `json:"spec_revision" jsonschema:"The dashboard's revision the snapshot shows: older than its revision after an edit, until it is published again."`
	PublishedAt  string `json:"published_at"`
	ExpiresAt    string `json:"expires_at,omitempty" jsonschema:"When the link stops showing the snapshot; left out: never."`
	Status       string `json:"status" jsonschema:"live (the link shows the snapshot), expired (publishing again makes a new link), workspace_off (an owner turned public links off for the workspace) or unavailable (this host has none). Only live shows anything."`
}

type PreviewFoundryDashboardTileOutput struct {
	ContentTrust string `json:"content_trust"`
	Tile         any    `json:"tile" jsonschema:"The tile as a spec would keep it."`
	LoadID       string `json:"load_id"`
	AsOf         string `json:"as_of"`
	Outcome      any    `json:"outcome" jsonschema:"Untrusted outcome of the tile's query: {ok: its answer} or {error: why it has none}. An integer past 2^53 comes as a string of its digits, which JSON numbers would round."`
}

// addFoundryDashboardTools registers the dashboard read tools of a user who may browse the
// workspace; the preview, which reads a graph, needs reads too.
func (s *Service) addFoundryDashboardTools(server *mcp.Server, identity *Identity, reads bool) {
	addFoundryTool(s, server, identity, tool(toolFoundryListDashboards,
		"List a workspace's Foundry dashboards, pinned ones first: each a set of tiles, typed queries of ontologies' loads shown as tables and charts, "+
			"which every member sees and reads as themselves. Specs are left out: "+toolFoundryGetDashboard+" reads one."), s.ListFoundryDashboards)
	addFoundryTool(s, server, identity, tool(toolFoundryGetDashboard,
		"Get one Foundry dashboard with its spec and revision. "+dashboardSpecDoc), s.GetFoundryDashboard)
	if reads {
		addFoundryTool(s, server, identity, tool(toolFoundryPreviewDashboardTile,
			"Check a dashboard tile and read its query once, as you, before you put it in a dashboard: returns its output, or why it has none. "+
				dashboardSpecDoc), s.PreviewFoundryDashboardTile)
	}
}

func (s *Service) ListFoundryDashboards(ctx context.Context, identity *Identity, call *Call, input FoundryWorkspaceInput) (
	ListFoundryDashboardsOutput, int, error) {
	call.ResourceKind, call.ResourceID = "workspace", input.WorkspaceID
	if !ValidIDs([]string{input.WorkspaceID}) {
		return ListFoundryDashboardsOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.ListFoundryDashboardsRequest{WorkspaceId: input.WorkspaceID}
	if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "ListFoundryDashboards"); err != nil {
		return ListFoundryDashboardsOutput{}, 0, err
	}
	resp, err := s.Foundry.ListFoundryDashboards(ctx, req)
	if err != nil {
		return ListFoundryDashboardsOutput{}, 0, FoundryError(err)
	}
	out := ListFoundryDashboardsOutput{ContentTrust: contentTrustUntrusted, CanCreate: resp.GetCanCreate(), Dashboards: []FoundryDashboardOutput{}}
	for _, d := range resp.GetDashboards() {
		out.Dashboards = append(out.Dashboards, s.FoundryDashboard(input.WorkspaceID, d))
	}
	return out, len(out.Dashboards), nil
}

func (s *Service) GetFoundryDashboard(ctx context.Context, identity *Identity, call *Call, input FoundryDashboardInput) (
	FoundryDashboardOutput, int, error) {
	call.ResourceKind, call.ResourceID = "dashboard", input.DashboardID
	if !ValidIDs([]string{input.WorkspaceID, input.DashboardID}) {
		return FoundryDashboardOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.GetFoundryDashboardRequest{WorkspaceId: input.WorkspaceID, Id: input.DashboardID}
	if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "GetFoundryDashboard"); err != nil {
		return FoundryDashboardOutput{}, 0, err
	}
	resp, err := s.Foundry.GetFoundryDashboard(ctx, req)
	if err != nil {
		return FoundryDashboardOutput{}, 0, FoundryError(err)
	}
	return s.FoundryDashboard(input.WorkspaceID, resp.GetDashboard()), 1, nil
}

func (s *Service) PreviewFoundryDashboardTile(ctx context.Context, identity *Identity, call *Call, input PreviewFoundryDashboardTileInput) (
	PreviewFoundryDashboardTileOutput, int, error) {
	call.ResourceKind, call.ResourceID = "workspace", input.WorkspaceID
	raw, err := json.Marshal(input.Tile)
	if !ValidIDs([]string{input.WorkspaceID}) || input.Tile == nil || err != nil {
		return PreviewFoundryDashboardTileOutput{}, 0, errCodeInvalidArguments
	}
	tile, err := dashboard.ParseTile(raw)
	if err != nil {
		return PreviewFoundryDashboardTileOutput{}, 0, detailedToolError{code: errCodeInvalidArguments, message: err.Error()}
	}
	call.ResourceKind, call.ResourceID = "ontology", tile.OntologyID
	canonical := mustCanonical(tile)
	req := &foundryv1.PreviewFoundryDashboardTileRequest{WorkspaceId: input.WorkspaceID, TileJson: string(canonical)}
	// It reads the tile's ontology as a graph read does.
	if err := s.AuthorizeFoundry(ctx, identity, &foundryv1.ReadFoundryGraphRequest{WorkspaceId: input.WorkspaceID, OntologyId: tile.OntologyID},
		QueryRead, "PreviewFoundryDashboardTile", "ReadFoundryGraph"); err != nil {
		return PreviewFoundryDashboardTileOutput{}, 0, err
	}
	// The backend reads the tile with the caller's read scope, as a graph read does.
	resp, err := s.Foundry.PreviewFoundryDashboardTile(identity.WithScope(ctx, input.WorkspaceID, false), req)
	if err != nil {
		return PreviewFoundryDashboardTileOutput{}, 0, FoundryError(err)
	}
	return PreviewFoundryDashboardTileOutput{ContentTrust: contentTrustUntrusted, Tile: UntrustedJSON(string(canonical)), LoadID: resp.GetLoadId(),
		AsOf: resp.GetAsOf(), Outcome: UntrustedJSON(resp.GetOutcomeJson())}, 1, nil
}

// FoundryDashboard is d as the tools return it, with its page in Foundry: never its public
// link (FoundryDashboardShareOutput).
func (s *Service) FoundryDashboard(workspaceID string, d *foundryv1.FoundryDashboard) FoundryDashboardOutput {
	out := FoundryDashboardOutput{
		ID: d.GetId(), WorkspaceID: workspaceID, Name: d.GetName(), Description: d.GetDescription(), Revision: d.GetRevision(), Pinned: d.GetPinned(),
		CanEdit: d.GetCanEdit(), CanPin: d.GetCanPin(), CanPublish: d.GetCanPublish(), CanDelete: d.GetCanDelete(), CanUnpublish: d.GetCanUnpublish(),
		PublishBlock: publishBlocks[d.GetPublishBlock()].name, URL: s.FoundryLink(workspaceID, "dashboards", d.GetId()),
	}
	if d.GetSpecJson() != "" {
		out.Spec = UntrustedJSON(d.GetSpecJson())
	}
	if share := d.GetShare(); share != nil {
		out.Share = &FoundryDashboardShareOutput{Published: true, SpecRevision: share.GetSpecRevision(), PublishedAt: protoTimestamp(share.GetPublishedAt()),
			Status: cmp.Or(shareStatuses[share.GetStatus()], "live")}
		if share.GetExpiresAt() != nil {
			out.Share.ExpiresAt = protoTimestamp(share.GetExpiresAt())
		}
	}
	return out
}

// CreateDashboardPayload is create_dashboard's: a spec of none is a dashboard with no tiles.
type CreateDashboardPayload struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Spec        json.RawMessage `json:"spec,omitempty"`
}

// UpdateDashboardPayload is update_dashboard's: at least one of name, description and spec.
type UpdateDashboardPayload struct {
	ExpectedRevision int64           `json:"expected_revision"`
	Name             *string         `json:"name,omitempty"`
	Description      *string         `json:"description,omitempty"`
	Spec             json.RawMessage `json:"spec,omitempty"`
}

// PinDashboardPayload is pin_dashboard's: the revision read, so what is pinned for the whole
// workspace is what its pinner saw.
type PinDashboardPayload struct {
	ExpectedRevision int64 `json:"expected_revision"`
	Pinned           bool  `json:"pinned"`
}

// PublishDashboardPayload is publish_dashboard's: the revision read, and when the link ends, in
// days from now (dashboardLinkExpiry); left out, a live link keeps its own.
type PublishDashboardPayload struct {
	ExpectedRevision int64 `json:"expected_revision"`
	ExpiresInDays    *int  `json:"expires_in_days,omitempty"`
}

// publishPayloadDoc is the refusal of a publish_dashboard payload that isn't one.
const publishPayloadDoc = "publish_dashboard takes payload {expected_revision, expires_in_days}: the revision you read, and optionally " +
	"7, 30 or 90 for the link to end that many days from now, or 0 for never"

// dashboardLinkExpiry is the expiry publish_dashboard's expires_in_days asks for: none keeps a
// live link's own, 0 never ends, 7, 30 or 90 ends that many days from the publish.
func dashboardLinkExpiry(days *int) (foundryv1.FoundryDashboardLinkExpiry, bool) {
	if days == nil {
		return foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_UNSPECIFIED, true
	}
	switch *days {
	case 0:
		return foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_NEVER, true
	case 7:
		return foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_7, true
	case 30:
		return foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_30, true
	case 90:
		return foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_90, true
	}
	return foundryv1.FoundryDashboardLinkExpiry_FOUNDRY_DASHBOARD_LINK_EXPIRY_UNSPECIFIED, false
}

// publishBlocks are the tools' words for each FoundryDashboardPublishBlock, and why a prepare is
// refused for it, naming the backend's reason; none for UNSPECIFIED.
var publishBlocks = map[foundryv1.FoundryDashboardPublishBlock]struct{ name, refusal string }{
	foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_ADMIN_ONLY: {"admin_only",
		"only an owner or admin makes a public link (FOUNDRY_DASHBOARD_PUBLIC_ADMIN_ONLY): to share the dashboard inside the workspace, give its url, which every member opens and reads as themselves"},
	foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_WORKSPACE_OFF: {"workspace_off",
		"an owner turned public dashboard links off for this workspace (FOUNDRY_DASHBOARD_PUBLIC_OFF): to share the dashboard inside the workspace, give its url"},
	foundryv1.FoundryDashboardPublishBlock_FOUNDRY_DASHBOARD_PUBLISH_BLOCK_UNAVAILABLE: {"unavailable",
		"public dashboard links are not available here (FOUNDRY_DASHBOARD_PUBLIC_UNAVAILABLE): to share the dashboard inside the workspace, give its url"},
}

// shareStatuses are the tools' words for each FoundryDashboardShareStatus.
var shareStatuses = map[foundryv1.FoundryDashboardShareStatus]string{
	foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_LIVE:          "live",
	foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_EXPIRED:       "expired",
	foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_WORKSPACE_OFF: "workspace_off",
	foundryv1.FoundryDashboardShareStatus_FOUNDRY_DASHBOARD_SHARE_STATUS_UNAVAILABLE:   "unavailable",
}

// validDashboardName is a dashboard's name as the backend takes it: 1 to 120 characters once
// trimmed, with no control character.
func validDashboardName(name string) bool {
	return ValidFoundryName(name) && !strings.ContainsFunc(name, unicode.IsControl)
}

// dashboardSpecPayload checks a payload's spec and writes it canonically; none is a dashboard
// with no tiles.
func dashboardSpecPayload(raw json.RawMessage) (json.RawMessage, dashboard.Spec, error) {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{"v":1,"tiles":[],"layout":{"rows":[]}}`)
	}
	spec, err := dashboard.ParseSpec(raw)
	if err != nil {
		return nil, dashboard.Spec{}, err
	}
	return mustCanonical(spec), spec, nil
}

// dashboardOntologies checks each ontology tiles read is one the caller may read in the
// workspace, so a card is never prepared for tiles the backend would refuse: the tiles of a new
// spec, or those of an edit with new targets (dashboard.Spec.NewTargets), as the backend checks.
func (s *Service) dashboardOntologies(ctx context.Context, identity *Identity, workspaceID string, tiles []dashboard.Tile) error {
	seen := map[string]bool{}
	for _, t := range tiles {
		if seen[t.OntologyID] {
			continue
		}
		seen[t.OntologyID] = true
		req := &foundryv1.GetFoundryOntologyRequest{WorkspaceId: workspaceID, OntologyId: t.OntologyID}
		if err := s.AuthorizeFoundry(ctx, identity, req, QueryRead, "GetFoundryOntology"); err != nil {
			return err
		}
		if _, err := s.Foundry.GetFoundryOntology(ctx, req); err != nil {
			return detailedToolError{code: errCodeInvalidArguments, message: "tile " + t.ID + " reads an ontology this workspace doesn't have"}
		}
	}
	return nil
}

// validateDashboardOperation checks a dashboard operation's target and payload and authorizes
// the caller for it, as ValidateFoundryOperation does every operation. Dashboards are people's:
// every one is user-only.
func (s *Service) validateDashboardOperation(ctx context.Context, identity *Identity, input FoundryOperationInput, raw []byte) (FoundryOperation, error) {
	invalid := func(message string) error { return detailedToolError{code: errCodeInvalidArguments, message: message} }
	if input.Operation == "create_dashboard" {
		workspaceID, ok := cutTarget(input.Target, "workspace")
		var payload CreateDashboardPayload
		if !ok || decodeStrict(raw, &payload) != nil || !validDashboardName(payload.Name) || !ValidSetDescription(payload.Description) {
			return nil, invalid("create_dashboard takes target workspace:<workspace_id> and payload {name, description, spec}: a name of 1 to 120 characters, " +
				"a description of at most 1000, and a spec (none for no tiles yet)")
		}
		spec, parsed, err := dashboardSpecPayload(payload.Spec)
		if err != nil {
			return nil, invalid("create_dashboard's spec: " + err.Error())
		}
		payload.Name, payload.Description, payload.Spec = strings.TrimSpace(payload.Name), strings.TrimSpace(payload.Description), spec
		req := &foundryv1.CreateFoundryDashboardRequest{WorkspaceId: workspaceID, Name: payload.Name, Description: payload.Description, SpecJson: string(spec)}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "CreateFoundryDashboard"); err != nil {
			return nil, err
		}
		if err := s.dashboardOntologies(ctx, identity, workspaceID, parsed.Tiles()); err != nil {
			return nil, err
		}
		return dashboardOperation[*foundryv1.CreateFoundryDashboardRequest]{payload: payload, req: req, workspaceID: workspaceID,
			call: func(ctx context.Context, s *Service, req *foundryv1.CreateFoundryDashboardRequest, confirmationID string) (*foundryv1.FoundryDashboardResponse, error) {
				req.ClientToken = confirmationID
				return s.Actions.CreateFoundryDashboard(ctx, req)
			}}, nil
	}
	workspaceID, dashboardID, ok := CutFoundryTarget(input.Target, "dashboard")
	if !ok {
		return nil, invalid(input.Operation + " takes target dashboard:<workspace_id>/<dashboard_id>")
	}
	switch input.Operation {
	case "update_dashboard":
		var payload UpdateDashboardPayload
		if decodeStrict(raw, &payload) != nil || payload.ExpectedRevision < 1 || payload.Name == nil && payload.Description == nil && len(payload.Spec) == 0 ||
			payload.Name != nil && !validDashboardName(*payload.Name) || payload.Description != nil && !ValidSetDescription(*payload.Description) {
			return nil, invalid("update_dashboard takes payload {expected_revision, name, description, spec}: the revision you read " +
				"and at least one change, a name of 1 to 120 characters, a description of at most 1000, a whole spec")
		}
		req := &foundryv1.UpdateFoundryDashboardRequest{WorkspaceId: workspaceID, Id: dashboardID, ExpectedRevision: payload.ExpectedRevision}
		if payload.Name != nil {
			name := strings.TrimSpace(*payload.Name)
			payload.Name, req.Name = &name, &name
		}
		if payload.Description != nil {
			description := strings.TrimSpace(*payload.Description)
			payload.Description, req.Description = &description, &description
		}
		var parsed *dashboard.Spec
		if len(payload.Spec) > 0 {
			spec, p, err := dashboardSpecPayload(payload.Spec)
			if err != nil {
				return nil, invalid("update_dashboard's spec: " + err.Error())
			}
			specJSON := string(spec)
			payload.Spec, parsed, req.SpecJson = spec, &p, &specJSON
		}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "UpdateFoundryDashboard"); err != nil {
			return nil, err
		}
		if parsed != nil {
			get := &foundryv1.GetFoundryDashboardRequest{WorkspaceId: workspaceID, Id: dashboardID}
			if err := s.AuthorizeFoundry(ctx, identity, get, foundryUserOnly, "GetFoundryDashboard"); err != nil {
				return nil, err
			}
			current, err := s.Foundry.GetFoundryDashboard(ctx, get)
			if err != nil {
				return nil, FoundryError(err)
			}
			stored, err := dashboard.ParseSpec([]byte(current.GetDashboard().GetSpecJson()))
			if err != nil {
				return nil, detailedToolError{code: errCodeFailedPrecondition, message: "the dashboard's stored spec can't be read: " + err.Error()}
			}
			if err := s.dashboardOntologies(ctx, identity, workspaceID, parsed.NewTargets(stored)); err != nil {
				return nil, err
			}
		}
		return dashboardOperation[*foundryv1.UpdateFoundryDashboardRequest]{payload: payload, req: req, workspaceID: workspaceID,
			call: func(ctx context.Context, s *Service, req *foundryv1.UpdateFoundryDashboardRequest, _ string) (*foundryv1.FoundryDashboardResponse, error) {
				return s.Actions.UpdateFoundryDashboard(ctx, req)
			}}, nil
	case "delete_dashboard":
		if len(input.Payload) != 0 {
			return nil, invalid("delete_dashboard takes an empty payload")
		}
		req := &foundryv1.DeleteFoundryDashboardRequest{WorkspaceId: workspaceID, Id: dashboardID}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "DeleteFoundryDashboard"); err != nil {
			return nil, err
		}
		return DeleteDashboardOperation{req: req}, nil
	case "pin_dashboard":
		var payload PinDashboardPayload
		_, hasPinned := input.Payload["pinned"]
		if decodeStrict(raw, &payload) != nil || !hasPinned || payload.ExpectedRevision < 1 {
			return nil, invalid("pin_dashboard takes payload {expected_revision, pinned}: the revision you read, and true to pin it in every member's sidebar, false to unpin it")
		}
		req := &foundryv1.PinFoundryDashboardRequest{WorkspaceId: workspaceID, Id: dashboardID, Pinned: payload.Pinned, ExpectedRevision: payload.ExpectedRevision}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "PinFoundryDashboard"); err != nil {
			return nil, err
		}
		return dashboardOperation[*foundryv1.PinFoundryDashboardRequest]{payload: payload, req: req, workspaceID: workspaceID,
			call: func(ctx context.Context, s *Service, req *foundryv1.PinFoundryDashboardRequest, _ string) (*foundryv1.FoundryDashboardResponse, error) {
				return s.Actions.PinFoundryDashboard(ctx, req)
			}}, nil
	case "unpublish_dashboard":
		// Offered whether or not public links are on: a link taken down while they are off
		// stays down when they come back on.
		if len(input.Payload) != 0 {
			return nil, invalid("unpublish_dashboard takes an empty payload")
		}
		req := &foundryv1.UnpublishFoundryDashboardRequest{WorkspaceId: workspaceID, Id: dashboardID}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "UnpublishFoundryDashboard"); err != nil {
			return nil, err
		}
		return dashboardOperation[*foundryv1.UnpublishFoundryDashboardRequest]{payload: struct{}{}, req: req, workspaceID: workspaceID,
			call: func(ctx context.Context, s *Service, req *foundryv1.UnpublishFoundryDashboardRequest, _ string) (*foundryv1.FoundryDashboardResponse, error) {
				return s.Actions.UnpublishFoundryDashboard(ctx, req)
			}}, nil
	case "publish_dashboard":
		if !s.PublicDashboards {
			return nil, detailedToolError{code: errCodeFailedPrecondition, message: "public dashboard links are not available here"}
		}
		var payload PublishDashboardPayload
		if decodeStrict(raw, &payload) != nil || payload.ExpectedRevision < 1 {
			return nil, invalid(publishPayloadDoc)
		}
		expiry, ok := dashboardLinkExpiry(payload.ExpiresInDays)
		if !ok {
			return nil, invalid(publishPayloadDoc)
		}
		req := &foundryv1.PublishFoundryDashboardRequest{WorkspaceId: workspaceID, Id: dashboardID, ExpectedRevision: payload.ExpectedRevision,
			Expiry: expiry}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "PublishFoundryDashboard"); err != nil {
			return nil, err
		}
		// Refused before a card is shown when the caller may not publish it: who may is the
		// backend's to say, per dashboard.
		read, err := s.Foundry.GetFoundryDashboard(ctx, &foundryv1.GetFoundryDashboardRequest{WorkspaceId: workspaceID, Id: dashboardID})
		if err != nil {
			return nil, FoundryError(err)
		}
		if block, ok := publishBlocks[read.GetDashboard().GetPublishBlock()]; ok {
			return nil, detailedToolError{code: errCodeFailedPrecondition, message: block.refusal}
		}
		return dashboardOperation[*foundryv1.PublishFoundryDashboardRequest]{payload: payload, req: req, workspaceID: workspaceID,
			call: func(ctx context.Context, s *Service, req *foundryv1.PublishFoundryDashboardRequest, _ string) (*foundryv1.FoundryDashboardResponse, error) {
				return s.Actions.PublishFoundryDashboard(ctx, req)
			}}, nil
	}
	return nil, invalid("unsupported Foundry operation")
}

// dashboardOperation is a dashboard operation whose call answers the dashboard it made or changed.
type dashboardOperation[R any] struct {
	payload     any
	req         R
	workspaceID string
	call        func(ctx context.Context, s *Service, req R, confirmationID string) (*foundryv1.FoundryDashboardResponse, error)
}

func (o dashboardOperation[R]) CanonicalPayload() []byte { return mustCanonical(o.payload) }

// Execute runs with the caller's read scope (Identity.WithScope): publishing reads the tiles
// for its snapshot, which the backend checks against it.
func (o dashboardOperation[R]) Execute(ctx context.Context, s *Service, identity *Identity, confirmationID string) (any, error) {
	resp, err := o.call(identity.WithScope(ctx, o.workspaceID, false), s, o.req, confirmationID)
	if err != nil {
		return nil, err
	}
	return s.FoundryDashboard(o.workspaceID, resp.GetDashboard()), nil
}

// DeleteDashboardOperation deletes a dashboard and its public snapshot.
type DeleteDashboardOperation struct {
	req *foundryv1.DeleteFoundryDashboardRequest
}

func (o DeleteDashboardOperation) CanonicalPayload() []byte { return []byte("{}") }
func (o DeleteDashboardOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	if _, err := s.Actions.DeleteFoundryDashboard(ctx, o.req); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": true, "dashboard_id": o.req.GetId()}, nil
}
