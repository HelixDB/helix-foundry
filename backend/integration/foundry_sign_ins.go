package integration

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"time"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Connecting a kind whose sign_in is true by signing in at its provider, through the host's
// provider accounts (WorkOS Pipes, hosted): the agent starts a sign-in with helix_foundry_connect,
// the user opens its link to Foundry in their browser and authorizes there, and the agent waits
// for the outcome. The credential stays with the provider accounts: no key passes through the
// agent, and nothing here returns provider text. Starting needs no confirmation: a sign-in writes
// nothing that matters until the user clicks Authorize on a form that shows the Workspace, the
// name and the settings, and only that user can authorize or finish it.
const toolFoundryGetSignIn = "helix_foundry_get_sign_in"

// maxSignInClientToken is the longest client token the backend keeps.
const maxSignInClientToken = 64

// The kinds whose settings the agent may choose when one signs in: GitHub's repositories,
// and PostHog's project. Pipes' PostHog provider signs in to PostHog US Cloud only: EU Cloud
// and a self-hosted PostHog connect with_key, through the kind's form.
const (
	connectorKindGitHub  = "github"
	connectorKindPostHog = "posthog"
)

// posthogProject is the form of a PostHog project's id, as the backend checks it.
var posthogProject = regexp.MustCompile(`^\d+$`)

// signInSettings is a new connector's settings when it signs in: the kind's own, never a
// credential (the backend refuses one). Every setting the agent doesn't choose is the kind's
// default, an empty field, which the user changes on the connector's page. A kind not named
// here sends none, which the backend reads as its defaults too.
var signInSettings = map[string]func(FoundryConnectInput) *foundryv1.ConnectorConfig{
	connectorKindGitHub: func(in FoundryConnectInput) *foundryv1.ConnectorConfig {
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Github{Github: &foundryv1.GitHubConnectorConfig{Repos: in.Repos}}}
	},
	connectorKindPostHog: func(in FoundryConnectInput) *foundryv1.ConnectorConfig {
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Posthog{Posthog: &foundryv1.PostHogConnectorConfig{
			Region: "us", ProjectId: strings.TrimSpace(in.ProjectID)}}}
	},
	"linear": func(FoundryConnectInput) *foundryv1.ConnectorConfig {
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Linear{Linear: &foundryv1.LinearConnectorConfig{}}}
	},
	"slack": func(FoundryConnectInput) *foundryv1.ConnectorConfig {
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Slack{Slack: &foundryv1.SlackConnectorConfig{}}}
	},
	"notion": func(FoundryConnectInput) *foundryv1.ConnectorConfig {
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Notion{Notion: &foundryv1.NotionConnectorConfig{}}}
	},
	"calcom": func(FoundryConnectInput) *foundryv1.ConnectorConfig {
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Calcom{Calcom: &foundryv1.CalcomConnectorConfig{}}}
	},
	"stripe": func(FoundryConnectInput) *foundryv1.ConnectorConfig {
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Stripe{Stripe: &foundryv1.StripeConnectorConfig{}}}
	},
	"google": func(FoundryConnectInput) *foundryv1.ConnectorConfig {
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Google{Google: &foundryv1.GoogleWorkspaceConnectorConfig{}}}
	},
}

type GetFoundrySignInInput struct {
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"Helix workspace identifier. May be empty when the caller has only one workspace (a service key always does)."`
	SignInID    string `json:"sign_in_id" jsonschema:"The sign-in's id, as helix_foundry_connect returned it."`
	Wait        bool   `json:"wait,omitempty" jsonschema:"Wait up to 25 s while it is pending, authorizing or finishing, then return it either way; call again until done."`
}

type FoundrySignInOutput struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Status is pending, authorizing, finishing, connected, declined, failed or expired; done
	// says it will not change without the user trying again.
	Status string `json:"status"`
	Done   bool   `json:"done"`
	// Failure is a fixed code, set when failed: provider_error, code_expired,
	// not_this_workspace, token_rejected, settings_invalid or unavailable.
	Failure     string `json:"failure,omitempty"`
	ConnectorID string `json:"connector_id,omitempty"`
	// DiscoveryProblem is Foundry's account of what the connector it made can't read.
	DiscoveryProblem string `json:"discovery_problem,omitempty"`
	IdentityChanged  bool   `json:"identity_changed,omitempty"`
	// Mine is false when another member started it: only they can authorize it.
	Mine      bool   `json:"mine"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

type GetFoundrySignInOutput struct {
	ContentTrust string              `json:"content_trust"`
	SignIn       FoundrySignInOutput `json:"sign_in"`
	// URL is the sign-in's link while the user may still authorize it.
	URL string `json:"url,omitempty"`
	// Connector is the connector it made or signed in again, once connected.
	Connector *FoundryConnectorOutput `json:"connector,omitempty"`
	// InstallURL installs the Helix Foundry GitHub App, which a signed-in GitHub connector needs
	// on an account or organization to read its private repositories.
	InstallURL string `json:"install_url,omitempty"`
	Next       string `json:"next"`
}

// signInTools is whether the identity gets the sign-in tools: a user who may manage
// connectors, on a host that offers signing in (SignIn) and names its website.
func (s *Service) signInTools(identity *Identity) bool {
	return s.SignIn && s.FoundryURL != "" && !identity.Agent && !identity.ServiceKey && identity.EventSource == nil &&
		identity.HasPermission(PermissionWorkspaceConnectorsManage)
}

// addFoundryGetSignInTool registers helix_foundry_get_sign_in, for an identity signInTools admits.
func (s *Service) addFoundryGetSignInTool(server *mcp.Server, identity *Identity) {
	addFoundryTool(s, server, identity, tool(toolFoundryGetSignIn,
		"Get a connector sign-in: pending, authorizing, finishing, connected (with the connector), declined, failed (with a fixed failure code) or expired. "+
			"With wait, waits up to 25 s while it isn't done; call again until it is."), s.GetFoundrySignIn)
}

// startSignIn starts a sign-in of a new connector of a kind that signs in, or of one whose
// account needs signing in again, and links its form on the website. FoundryConnect authorized it.
func (s *Service) startSignIn(ctx context.Context, kind *foundryv1.ConnectorKind, input FoundryConnectInput) (FoundryConnectOutput, int, error) {
	if err := signInSettingsRefusal(kind.GetKind(), input); err != nil {
		return FoundryConnectOutput{}, 0, err
	}
	req := &foundryv1.StartConnectorSignInRequest{
		WorkspaceId: input.WorkspaceID, Kind: kind.GetKind(), Name: strings.TrimSpace(input.Name),
		ConnectorId: input.ConnectorID, ClientToken: input.ClientToken,
	}
	if req.ConnectorId != "" {
		// Signing a connector in again keeps its name and settings, as the
		// backend requires: it takes neither.
		if req.Name != "" || len(input.Repos) > 0 || input.ProjectID != "" {
			return FoundryConnectOutput{}, 0, detailedToolError{code: errCodeInvalidArguments,
				message: "signing a connector in again keeps its name and settings; change them on its page"}
		}
	} else {
		if settings := signInSettings[kind.GetKind()]; settings != nil {
			req.Config = settings(input)
		}
		if req.Name == "" {
			req.Name = kind.GetDisplayName()
		}
	}
	started, err := s.Foundry.StartConnectorSignIn(ctx, req)
	if err != nil {
		return FoundryConnectOutput{}, 0, FoundryError(err)
	}
	signIn := started.GetSignIn()
	display := kind.GetDisplayName()
	return FoundryConnectOutput{
		URL: s.signInLink(input.WorkspaceID, signIn), Kind: kind.GetKind(), DisplayName: display, Available: kind.GetAvailable(),
		SignInID: signIn.GetId(), ExpiresAt: protoTimestamp(signIn.GetExpiresAt()),
		Next: "Open url in the user's browser (open <url> on macOS, xdg-open <url> on Linux), or give it to them: Foundry shows the " + display +
			" form for this sign-in, where they click Connect with " + display + " and authorize Helix Foundry at " + display + ". " +
			"Only this user can authorize it, until expires_at. Then call " + toolFoundryGetSignIn + " with wait until done.",
	}, 1, nil
}

// GetFoundrySignIn reads a sign-in, waiting while it isn't done when asked to, with the connector
// it made once connected.
func (s *Service) GetFoundrySignIn(
	ctx context.Context, identity *Identity, call *Call, input GetFoundrySignInInput,
) (GetFoundrySignInOutput, int, error) {
	started := time.Now() // the wait is bounded from here
	call.ResourceKind, call.ResourceID = "connector_sign_in", input.SignInID
	if !ValidIDs([]string{input.WorkspaceID, input.SignInID}) {
		return GetFoundrySignInOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.GetConnectorSignInRequest{WorkspaceId: input.WorkspaceID, Id: input.SignInID}
	if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "GetConnectorSignIn", "GetConnector", "ListConnectorKinds"); err != nil {
		return GetFoundrySignInOutput{}, 0, err
	}
	get := func(ctx context.Context) (*foundryv1.ConnectorSignIn, error) {
		response, err := s.Foundry.GetConnectorSignIn(ctx, req)
		return response.GetSignIn(), err
	}
	signIn, err := get(ctx)
	if err == nil && input.Wait {
		signIn, err = waitWhile(ctx, s, started, signIn, func(si *foundryv1.ConnectorSignIn) bool { return !signInDone(si.GetStatus()) }, get)
	}
	if err != nil {
		return GetFoundrySignInOutput{}, 0, FoundryError(err)
	}
	out := GetFoundrySignInOutput{ContentTrust: contentTrustUntrusted, SignIn: FoundrySignIn(signIn)}
	display := signIn.GetKind()
	kinds, err := s.Foundry.ListConnectorKinds(ctx, &foundryv1.ListConnectorKindsRequest{WorkspaceId: input.WorkspaceID})
	if err != nil {
		return GetFoundrySignInOutput{}, 0, FoundryError(err)
	}
	if kind := connectorKind(kinds.GetKinds(), signIn.GetKind()); kind != nil {
		display = kind.GetDisplayName()
		out.InstallURL = kind.GetGithub().GetInstallUrl()
	}
	status := signIn.GetStatus()
	if status != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_CONNECTED &&
		status != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_EXPIRED {
		out.URL = s.signInLink(input.WorkspaceID, signIn)
	}
	if status == foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_CONNECTED && signIn.GetConnectorId() != "" {
		response, err := s.Foundry.GetConnector(ctx, &foundryv1.GetConnectorRequest{WorkspaceId: input.WorkspaceID, Id: signIn.GetConnectorId()})
		if err != nil {
			return GetFoundrySignInOutput{}, 0, FoundryError(err)
		}
		connector := s.FoundryConnector(input.WorkspaceID, response.GetConnector())
		out.Connector = &connector
	}
	out.Next = signInNext(signIn, display, out)
	return out, 1, nil
}

// signInSettingsRefusal refuses the settings a kind doesn't take: repos are GitHub's and
// project_id PostHog's.
func signInSettingsRefusal(kind string, input FoundryConnectInput) error {
	refuse := func(message string) error {
		return detailedToolError{code: errCodeInvalidArguments, message: message}
	}
	switch {
	case len(input.Repos) > 0 && kind != connectorKindGitHub:
		return refuse("repos are GitHub's only")
	case input.ProjectID != "" && kind != connectorKindPostHog:
		return refuse("project_id is PostHog's only")
	case input.ProjectID != "" && !posthogProject.MatchString(strings.TrimSpace(input.ProjectID)):
		return refuse("the PostHog project id is a number, found in its project settings")
	}
	return nil
}

// signInNext says what the agent does next, in Foundry's own words: never the provider's.
func signInNext(signIn *foundryv1.ConnectorSignIn, display string, out GetFoundrySignInOutput) string {
	again := " Ask the user whether to try again: the same url works until expires_at, and " + toolFoundryGetSignIn + " with wait follows it."
	var next string
	switch signIn.GetStatus() {
	case foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_PENDING:
		next = "The user hasn't opened the link yet: open url in their browser, or give it to them, then call again with wait."
	case foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_AUTHORIZING:
		next = "The user is authorizing at " + display + ": call again with wait."
	case foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_FINISHING:
		next = "Foundry is finishing the sign-in: call again with wait."
	case foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_CONNECTED:
		next = display + " is connected and syncs now: once " + toolFoundryListConnectors + " shows its last_sync, follow it with " + toolFoundryGetSync + " with wait."
		if out.Connector != nil && out.Connector.LastSync != nil {
			next = display + " is connected: follow its sync with " + toolFoundryGetSync + " (connector.last_sync.id) with wait."
		}
		if signIn.GetIdentityChanged() {
			next += " It now reads with a different " + display + " account than before; tell the user."
		}
		if signIn.GetDiscoveryProblem() != "" {
			next += " It can't read all it names (discovery_problem): tell the user."
			if out.InstallURL != "" {
				next += " A repository the app can't see needs Helix Foundry installed on its account or organization: give the user install_url, then sync_connector once it is installed."
			}
		}
	case foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_DECLINED:
		next = "The user declined at " + display + "." + again
	case foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_FAILED:
		next = signInFailureText(signIn.GetFailure(), display) + again
		if signIn.GetFailure() == foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_SETTINGS_INVALID {
			next = signInFailureText(signIn.GetFailure(), display) + " Start a new sign-in with settings Foundry takes."
		}
	case foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_EXPIRED:
		next = "This sign-in ran out. Ask the user whether to start again with " + toolFoundryConnect + "."
		if signIn.GetConnectorId() != "" {
			next = "This sign-in ran out. Ask the user whether to start again with " + toolFoundryConnect + " and connector_id " + signIn.GetConnectorId() + "."
		}
	default:
		next = "Call again with wait."
	}
	if !signIn.GetMine() && !signInDone(signIn.GetStatus()) {
		next = "Another member started this sign-in: only they can open its link and authorize it. " + next
	}
	return next
}

// signInFailureText words a failure code: the backend sends codes, never the provider's text.
func signInFailureText(failure foundryv1.ConnectorSignInFailure, display string) string {
	switch failure {
	case foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_CODE_EXPIRED:
		return "The sign-in took too long to come back from " + display + ", or came back twice."
	case foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_NOT_THIS_WORKSPACE:
		return "The " + display + " account signed in isn't this workspace's, or wasn't signed in by the user who started it, so Foundry deleted it."
	case foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_TOKEN_REJECTED:
		return display + " refused the access it had just granted."
	case foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_SETTINGS_INVALID:
		return "Foundry refused the connector's settings."
	case foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_UNAVAILABLE:
		return "Signing in to " + display + " was unavailable for a moment."
	default:
		return display + " or WorkOS answered the sign-in with an error."
	}
}

func signInDone(status foundryv1.ConnectorSignInStatus) bool {
	switch status {
	case foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_CONNECTED, foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_DECLINED,
		foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_FAILED, foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_EXPIRED:
		return true
	}
	return false
}

func FoundrySignIn(signIn *foundryv1.ConnectorSignIn) FoundrySignInOutput {
	out := FoundrySignInOutput{
		ID: signIn.GetId(), Kind: signIn.GetKind(), Name: signIn.GetName(),
		Status: EnumName(signIn.GetStatus().String(), "CONNECTOR_SIGN_IN_STATUS_"), Done: signInDone(signIn.GetStatus()),
		ConnectorID: signIn.GetConnectorId(), DiscoveryProblem: signIn.GetDiscoveryProblem(), IdentityChanged: signIn.GetIdentityChanged(),
		Mine: signIn.GetMine(), CreatedAt: protoTimestamp(signIn.GetCreatedAt()), ExpiresAt: protoTimestamp(signIn.GetExpiresAt()),
	}
	if signIn.GetStatus() == foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_FAILED {
		out.Failure = EnumName(signIn.GetFailure().String(), "CONNECTOR_SIGN_IN_FAILURE_")
	}
	return out
}

// signInLink is the website's Add data form of the sign-in's kind, bound to it.
func (s *Service) signInLink(workspaceID string, signIn *foundryv1.ConnectorSignIn) string {
	link := url.Values{"add": {signIn.GetKind()}, "sign_in": {signIn.GetId()}, "workspace": {workspaceID}}
	return s.FoundryURL + "/data?" + link.Encode()
}

// FoundryConnectorAccount is a signed-in connector's account, without its label: the provider
// identity is text whoever holds the account writes.
func FoundryConnectorAccount(a *foundryv1.ConnectorAccount) *FoundryConnectorAccountOutput {
	if a == nil {
		return nil
	}
	out := &FoundryConnectorAccountOutput{
		State: EnumName(a.GetState().String(), "CONNECTOR_ACCOUNT_STATE_"), UserID: a.GetUserId(), ConnectedBy: a.GetConnectedBy(),
		ConnectedAt: protoTimestamp(a.GetConnectedAt()),
	}
	if a.GetBroken() != foundryv1.ConnectorAccountBroken_CONNECTOR_ACCOUNT_BROKEN_UNSPECIFIED {
		out.Broken, out.BrokenAt = EnumName(a.GetBroken().String(), "CONNECTOR_ACCOUNT_BROKEN_"), protoTimestamp(a.GetBrokenAt())
	}
	return out
}

func connectorKind(kinds []*foundryv1.ConnectorKind, kind string) *foundryv1.ConnectorKind {
	for _, k := range kinds {
		if k.GetKind() == kind {
			return k
		}
	}
	return nil
}
