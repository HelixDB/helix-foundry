package integration

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// helix_foundry_connect connects a source to a Workspace through a link to the website, so no
// credential passes through an agent. A kind whose sign_in is true, where the caller may sign in
// (signInTools), starts a sign-in at its provider and links its form bound to it, which the agent
// follows with helix_foundry_get_sign_in. Every other kind, or one asked for with_key, links the
// kind's own form, where the user enters its credentials.

// FoundryConnectInput is helix_foundry_connect's input. Every field but the Workspace and the kind
// is a sign-in's, and the schema of a caller who can't sign in leaves them out (connectSchema).
type FoundryConnectInput struct {
	WorkspaceID string   `json:"workspace_id,omitempty" jsonschema:"Helix workspace identifier. May be empty when the caller has only one workspace (a service key always does)."`
	Kind        string   `json:"kind" jsonschema:"The connector's kind: stripe, workos, posthog, github, linear, calcom, granola, slack, notion, google, postgres, mysql, s3 or events (an API source)."`
	WithKey     bool     `json:"with_key,omitempty" jsonschema:"Open the kind's form to enter a key instead of signing in: for PostHog EU Cloud or a self-hosted PostHog, or when the user asks to use a key."`
	Name        string   `json:"name,omitempty" jsonschema:"A new connector's name when it signs in; the kind's display name when empty. Not with connector_id."`
	Repos       []string `json:"repos,omitempty" jsonschema:"A new GitHub connector's repositories when it signs in, as owner/name. None reads the repositories of the Helix Foundry GitHub App's installations the user can reach. Not with connector_id."`
	ProjectID   string   `json:"project_id,omitempty" jsonschema:"A new PostHog connector's project id when it signs in, a number from its project settings; empty takes the account's one project. Not with connector_id."`
	ConnectorID string   `json:"connector_id,omitempty" jsonschema:"Sign this connector in again instead of making one: one whose account needs signing in (helix_foundry_list_connectors). It keeps its name and settings."`
	ClientToken string   `json:"client_token,omitempty" jsonschema:"Up to 64 characters; a retry with the same token returns the sign-in the first call started."`
}

// signInFields are FoundryConnectInput's fields that only a sign-in takes.
var signInFields = []string{"with_key", "name", "repos", "project_id", "connector_id", "client_token"}

// FoundryConnectOutput is the link the user opens, and the sign-in it is bound to when the kind
// signs in.
type FoundryConnectOutput struct {
	URL         string `json:"url"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	// Available is false when the Workspace's plan doesn't include the kind.
	Available bool `json:"available"`
	// SignInID and ExpiresAt are the sign-in's, when the kind signs in: the agent follows it
	// with helix_foundry_get_sign_in, and its link works until it expires.
	SignInID  string `json:"sign_in_id,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Next      string `json:"next"`
}

// connectKindNoEvents is helix_foundry_connect's kind on a host without API sources; its struct
// tag is the one that has them.
const connectKindNoEvents = "The connector's kind: stripe, workos, posthog, github, linear, calcom, granola, slack, notion, google, postgres, mysql or s3."

// connectSchema is helix_foundry_connect's input schema: without API sources where the host
// has none, and with only the Workspace and the kind for a caller who can't sign in.
func connectSchema(events, signIns bool) *jsonschema.Schema {
	schema := schemaFor[FoundryConnectInput]()
	if !events {
		schema.Properties["kind"].Description = connectKindNoEvents
	}
	if !signIns {
		for _, field := range signInFields {
			delete(schema.Properties, field)
		}
	}
	return schema
}

// addFoundryConnectTool registers helix_foundry_connect. A caller who can sign in gets it as a
// write, since it may start a sign-in, which destroys nothing; anyone else gets a read: a link.
func (s *Service) addFoundryConnectTool(server *mcp.Server, identity *Identity, signIns bool) {
	t := tool(toolFoundryConnect,
		"Get a link to Foundry that opens the form for a new connector of one kind, for the user to open in their browser and enter its credentials. "+
			"Credentials never pass through the agent. Then helix_foundry_list_connectors lists the new connector.")
	if signIns {
		no := false
		t = &mcp.Tool{Name: toolFoundryConnect,
			Description: "Connect a source to a workspace, or sign a connector in again: returns a link to Foundry for the user to open in their browser, " +
				"so credentials never pass through the agent. A kind whose sign_in is true (" + toolFoundryListConnectorKinds + ") connects by signing in at its provider: " +
				"follow the result's sign_in_id with " + toolFoundryGetSignIn + ". Any other kind, or with_key, opens the kind's form, where the user enters its credentials; " +
				"then helix_foundry_list_connectors lists the new connector.",
			Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, OpenWorldHint: &no},
		}
	}
	t.InputSchema = connectSchema(s.Events, signIns)
	if !signIns {
		// A link to a form is bound to no sign-in.
		output := schemaFor[FoundryConnectOutput]()
		delete(output.Properties, "sign_in_id")
		delete(output.Properties, "expires_at")
		t.OutputSchema = output
	}
	addFoundryTool(s, server, identity, t, s.FoundryConnect)
}

// FoundryConnect links the website's form of a kind the backend's catalog offers, in the
// Workspace it names: bound to a new sign-in when the kind signs in for this caller.
func (s *Service) FoundryConnect(
	ctx context.Context, identity *Identity, call *Call, input FoundryConnectInput,
) (FoundryConnectOutput, int, error) {
	call.ResourceKind, call.ResourceID = "workspace", input.WorkspaceID
	if !ValidIDs([]string{input.WorkspaceID}, input.ConnectorID) || input.Kind == "" || len(input.ClientToken) > maxSignInClientToken {
		return FoundryConnectOutput{}, 0, errCodeInvalidArguments
	}
	signIns := s.signInTools(identity)
	methods := []string{"ListConnectorKinds", "CreateConnector"}
	if signIns {
		methods = append(methods, "StartConnectorSignIn")
	}
	req := &foundryv1.ListConnectorKindsRequest{WorkspaceId: input.WorkspaceID}
	if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, methods...); err != nil {
		return FoundryConnectOutput{}, 0, err
	}
	response, err := s.Foundry.ListConnectorKinds(ctx, req)
	if err != nil {
		return FoundryConnectOutput{}, 0, FoundryError(err)
	}
	var offered []string
	for _, kind := range response.GetKinds() {
		if !s.Events && kind.GetKind() == foundrymodel.ConnectorKindEvents {
			continue
		}
		offered = append(offered, kind.GetKind())
	}
	kind := connectorKind(response.GetKinds(), input.Kind)
	if kind == nil || !s.Events && kind.GetKind() == foundrymodel.ConnectorKindEvents {
		return FoundryConnectOutput{}, 0, detailedToolError{code: errCodeInvalidArguments, message: fmt.Sprintf(
			"Foundry has no %q connector; it offers %s", input.Kind, strings.Join(offered, ", "))}
	}
	if signIns && kind.GetSignIn() && !input.WithKey {
		return s.startSignIn(ctx, kind, input)
	}
	display := kind.GetDisplayName()
	if input.Name != "" || len(input.Repos) > 0 || input.ProjectID != "" || input.ConnectorID != "" {
		return FoundryConnectOutput{}, 0, detailedToolError{code: errCodeInvalidArguments, message: display +
			" connects through its form, where the user names it and chooses its settings: name, repos, project_id and connector_id are for signing in"}
	}
	link := url.Values{"add": {kind.GetKind()}, "workspace": {input.WorkspaceID}}
	out := FoundryConnectOutput{
		URL: s.FoundryURL + "/data?" + link.Encode(), Kind: kind.GetKind(), DisplayName: display, Available: kind.GetAvailable(),
		Next: "Give the user this link to open in a browser signed in to Foundry: it opens the " + display +
			" form, where they enter the credentials and save. Then helix_foundry_list_connectors lists the new connector.",
	}
	if kind.GetKind() == foundrymodel.ConnectorKindEvents {
		out.Next = "Give the user this link to open in a browser signed in to Foundry: it opens the API source form, where they name the source " +
			"and say what its pieces are; it takes no credential, and the create_event_source operation makes one without the browser. " +
			"Then helix_foundry_list_connectors lists the new source."
	}
	if !kind.GetAvailable() {
		out.Next = "The workspace's plan doesn't include " + display + "; the link shows owners and admins how to upgrade to Foundry Pro first. " + out.Next
	}
	return out, 1, nil
}
