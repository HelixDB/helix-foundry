package integration

import (
	"context"
	"net/url"
	"time"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Where a local install's Assistant runs (Service.AgentHosts): Foundry's own
// worker on the workspace's model, or a helper on the owner's computer that
// drives the Claude Code or Codex CLI they signed in to, on their
// subscription. toolFoundryGetAssistantRuntime reads it; three operations
// change it, each confirmed: set_assistant_runtime, pair_assistant_helper
// (a one-time code the helper's pair command takes) and
// revoke_assistant_helper. A host without helpers (hosted Helix) registers
// none of them and offers none of the operations.

// toolFoundryGetAssistantRuntime says where the Assistant runs.
const toolFoundryGetAssistantRuntime = "helix_foundry_get_assistant_runtime"

// AssistantHostOutput is a helper paired with the install.
type AssistantHostOutput struct {
	ID string `json:"id"`
	// Name is the machine's, as the helper gave it.
	Name string `json:"name"`
	// Runtimes is what it offers: claude_code, codex.
	Runtimes []string `json:"runtimes"`
	Version  string   `json:"version,omitempty"`
	Platform string   `json:"platform,omitempty"`
	// LastSeenAt is its last call; empty until its first after pairing,
	// which starts nothing.
	LastSeenAt string `json:"last_seen_at,omitempty"`
	// Online is whether it called in the last 30 seconds.
	Online bool `json:"online"`
	// Accounts is what its CLIs are signed in to, as it last found them.
	Accounts []AssistantHostAccount `json:"accounts"`
}

// AssistantHostAccount is what one of a helper's CLIs is signed in to:
// State signed_in, signed_out or unknown, Subscription the plan when signed
// in on one, Version the CLI's (Codex's), and Outdated a Codex older than
// Foundry runs, which runs no turn until updated.
type AssistantHostAccount struct {
	Runtime      string `json:"runtime"`
	State        string `json:"state"`
	Subscription string `json:"subscription,omitempty"`
	Version      string `json:"version,omitempty"`
	Outdated     bool   `json:"outdated,omitempty"`
}

type GetAssistantRuntimeOutput struct {
	ContentTrust string `json:"content_trust"`
	// Runtime is where the workspace's new Assistant threads run: api
	// (Foundry's own worker, on the workspace's model, read with
	// helix_foundry_get_ai_settings), claude_code or codex (a helper, on the
	// owner's subscription). A thread keeps the runtime it started with.
	Runtime string `json:"runtime"`
	// Online is the helper runtimes a helper online runs now: a thread of
	// one not listed can't be sent to until its helper runs.
	Online []string              `json:"online"`
	Hosts  []AssistantHostOutput `json:"hosts"`
	// CodexConfirmed is whether the owner confirmed, in Settings › AI
	// models, what codex runs with (their own Codex setup, its shell off, on
	// their ChatGPT plan): codex can't be set before.
	CodexConfirmed bool `json:"codex_confirmed"`
	// CodexMinVersion is the oldest Codex a helper runs it on.
	CodexMinVersion string `json:"codex_min_version,omitempty"`
	// SettingsURL opens Settings › AI models, where the owner chooses it too.
	SettingsURL string `json:"settings_url,omitempty"`
}

func (s *Service) GetAssistantRuntime(
	ctx context.Context, identity *Identity, call *Call, input FoundryWorkspaceInput,
) (GetAssistantRuntimeOutput, int, error) {
	call.ResourceKind, call.ResourceID = "workspace", input.WorkspaceID
	if !ValidIDs([]string{input.WorkspaceID}) {
		return GetAssistantRuntimeOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.GetFoundryAgentRuntimeRequest{WorkspaceId: input.WorkspaceID}
	if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "GetFoundryAgentRuntime"); err != nil {
		return GetAssistantRuntimeOutput{}, 0, err
	}
	response, err := s.Foundry.GetFoundryAgentRuntime(ctx, req)
	if err != nil {
		return GetAssistantRuntimeOutput{}, 0, FoundryError(err)
	}
	r := response.GetRuntime()
	out := GetAssistantRuntimeOutput{ContentTrust: contentTrustUntrusted, Runtime: r.GetRuntime(), Online: r.GetOnline(), Hosts: []AssistantHostOutput{},
		CodexConfirmed: r.GetCodexConfirmedAt() != nil, CodexMinVersion: r.GetCodexMinVersion()}
	if out.Online == nil {
		out.Online = []string{}
	}
	for _, h := range r.GetHosts() {
		out.Hosts = append(out.Hosts, assistantHost(h))
	}
	if s.FoundryURL != "" {
		out.SettingsURL = s.FoundryURL + "/settings/ai?" + url.Values{"workspace": {input.WorkspaceID}}.Encode()
	}
	return out, len(out.Hosts), nil
}

func assistantHost(h *foundryv1.FoundryAgentHost) AssistantHostOutput {
	out := AssistantHostOutput{ID: h.GetId(), Name: h.GetName(), Runtimes: h.GetRuntimes(), Version: h.GetVersion(), Platform: h.GetPlatform(),
		Online: h.GetOnline()}
	if h.GetLastSeenAt() != nil {
		out.LastSeenAt = h.GetLastSeenAt().AsTime().UTC().Format(time.RFC3339)
	}
	if out.Runtimes == nil {
		out.Runtimes = []string{}
	}
	out.Accounts = []AssistantHostAccount{}
	for _, a := range h.GetAccounts() {
		out.Accounts = append(out.Accounts, AssistantHostAccount{Runtime: a.GetRuntime(), State: a.GetState(), Subscription: a.GetSubscription(),
			Version: a.GetVersion(), Outdated: a.GetOutdated()})
	}
	return out
}

// SetAssistantRuntimePayload is set_assistant_runtime's.
type SetAssistantRuntimePayload struct {
	Runtime string `json:"runtime"`
}

// SetAssistantRuntimeOperation sets where a workspace's new threads run.
type SetAssistantRuntimeOperation struct {
	payload SetAssistantRuntimePayload
	req     *foundryv1.UpdateFoundryAgentRuntimeRequest
}

func (o SetAssistantRuntimeOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }
func (o SetAssistantRuntimeOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	response, err := s.Actions.UpdateFoundryAgentRuntime(ctx, o.req)
	if err != nil {
		return nil, err
	}
	return map[string]any{"runtime": response.GetRuntime().GetRuntime(), "online": response.GetRuntime().GetOnline()}, nil
}

// PairAssistantHelperOperation makes a one-time code that pairs a helper.
type PairAssistantHelperOperation struct {
	req *foundryv1.CreateFoundryAgentHostPairingRequest
}

func (o PairAssistantHelperOperation) CanonicalPayload() []byte { return []byte("{}") }
func (o PairAssistantHelperOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	response, err := s.Actions.CreateFoundryAgentHostPairing(ctx, o.req)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"code": response.GetCode(), "expires_at": response.GetExpiresAt().AsTime().UTC().Format(time.RFC3339),
		"command": response.GetCommand(),
		"next":    "Run command on the computer that has Claude Code or Codex signed in (it pairs within ten minutes, once), then its install command to keep it running.",
	}, nil
}

// RevokeAssistantHelperPayload is revoke_assistant_helper's.
type RevokeAssistantHelperPayload struct {
	HostID string `json:"host_id"`
}

// RevokeAssistantHelperOperation unpairs a helper.
type RevokeAssistantHelperOperation struct {
	payload RevokeAssistantHelperPayload
	req     *foundryv1.RevokeFoundryAgentHostRequest
}

func (o RevokeAssistantHelperOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }
func (o RevokeAssistantHelperOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	response, err := s.Actions.RevokeFoundryAgentHost(ctx, o.req)
	if err != nil {
		return nil, err
	}
	return assistantHost(response.GetHost()), nil
}

// validateAssistantOperation checks one of the runtime's operations, on
// target workspace:<workspace_id>, as the user it is for. ok is false for
// any other operation.
func (s *Service) validateAssistantOperation(ctx context.Context, identity *Identity, input FoundryOperationInput, raw []byte) (FoundryOperation, bool, error) {
	invalid := func(message string) error { return detailedToolError{code: errCodeInvalidArguments, message: message} }
	switch input.Operation {
	case "set_assistant_runtime", "pair_assistant_helper", "revoke_assistant_helper":
	default:
		return nil, false, nil
	}
	switch {
	case !s.AgentHosts:
		return nil, true, detailedToolError{code: errCodeFailedPrecondition, message: "Founder runs on a subscription only on a local install"}
	case !identity.User || identity.ServiceKey || identity.Agent:
		return nil, true, errCodeNotFound
	}
	workspaceID, ok := cutTarget(input.Target, "workspace")
	if !ok {
		return nil, true, invalid(input.Operation + " takes target workspace:<workspace_id>")
	}
	switch input.Operation {
	case "set_assistant_runtime":
		var payload SetAssistantRuntimePayload
		if decodeStrict(raw, &payload) != nil || !model.FoundryAgentRuntime(payload.Runtime).Valid() {
			return nil, true, invalid("set_assistant_runtime takes payload {runtime}: api, claude_code or codex")
		}
		req := &foundryv1.UpdateFoundryAgentRuntimeRequest{WorkspaceId: workspaceID, Runtime: payload.Runtime}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "UpdateFoundryAgentRuntime"); err != nil {
			return nil, true, err
		}
		if payload.Runtime == string(model.FoundryAgentRuntimeCodex) {
			// What codex runs with is confirmed in Settings, never through a card.
			current, err := s.Foundry.GetFoundryAgentRuntime(ctx, &foundryv1.GetFoundryAgentRuntimeRequest{WorkspaceId: workspaceID})
			if err != nil {
				return nil, true, FoundryError(err)
			}
			if current.GetRuntime().GetCodexConfirmedAt() == nil {
				return nil, true, detailedToolError{code: errCodeFailedPrecondition,
					message: "codex runs with the owner's own Codex setup, on their ChatGPT plan: they choose it first in Settings › AI models, where they confirm that"}
			}
		}
		return SetAssistantRuntimeOperation{payload: payload, req: req}, true, nil
	case "pair_assistant_helper":
		if len(input.Payload) != 0 {
			return nil, true, invalid("pair_assistant_helper takes an empty payload")
		}
		req := &foundryv1.CreateFoundryAgentHostPairingRequest{WorkspaceId: workspaceID}
		return PairAssistantHelperOperation{req: req}, true, s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "CreateFoundryAgentHostPairing")
	default:
		var payload RevokeAssistantHelperPayload
		if decodeStrict(raw, &payload) != nil || !ValidIDs([]string{payload.HostID}) {
			return nil, true, invalid("revoke_assistant_helper takes payload {host_id}")
		}
		req := &foundryv1.RevokeFoundryAgentHostRequest{WorkspaceId: workspaceID, Id: payload.HostID}
		return RevokeAssistantHelperOperation{payload: payload, req: req}, true, s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "RevokeFoundryAgentHost")
	}
}
