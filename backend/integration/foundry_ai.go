package integration

import (
	"context"
	"net/url"
	"strings"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// toolFoundryGetAISettings (foundry.go) says where a workspace's model calls
// go: which provider and models its generations, loads, questions and
// Assistant run on, how a save's test went, and whether the Assistant can
// run on it. It is read-only: a provider is chosen, and its key entered, in
// Foundry's Settings › AI models (settings_url), never through an agent.

// FoundryAISettingOutput is one setting: the deployment's default, or a
// provider an owner saved. Keys and their hints are never returned.
type FoundryAISettingOutput struct {
	// Source is "default" (the deployment's: Helix's Claude when hosted) or
	// "workspace" (an owner's).
	Source string `json:"source"`
	// Kind is the provider's: anthropic, openai, azure_openai, bedrock,
	// ollama or openai_compatible.
	Kind string `json:"kind"`
	// Provider names it as Foundry does: "Claude", or a custom server's host.
	Provider string                `json:"provider"`
	Models   FoundryAIModelsOutput `json:"models"`
	// Test is a save's test: queued, running, passed, failed or superseded;
	// empty for the default, which takes none.
	Test string `json:"test,omitempty"`
	// Failure is why a failed test failed: the model failure and its check's
	// detail, such as "auth" or "not_found/tools_rejected".
	Failure string `json:"failure,omitempty"`
	// Assistant is whether Foundry's Assistant can run on it: passed, auth,
	// refused or no_tool_call from its tool check, or unchecked (it runs, and
	// says why if it can't).
	Assistant string `json:"assistant"`
}

// FoundryAIModelsOutput are the models each kind of work runs on, by name.
type FoundryAIModelsOutput struct {
	Draft   string `json:"draft"`
	Ask     string `json:"ask"`
	Extract string `json:"extract"`
	Verify  string `json:"verify"`
}

type GetFoundryAISettingsOutput struct {
	ContentTrust string `json:"content_trust"`
	// InForce is the setting model work runs on now; absent when there is
	// none (a local install no owner has set up), and model work waits.
	InForce *FoundryAISettingOutput `json:"in_force,omitempty"`
	// Pending is a save waiting for its test: it replaces InForce once it passes.
	Pending *FoundryAISettingOutput `json:"pending,omitempty"`
	// LastFailed is the newest save whose test failed after the setting last
	// changed; InForce stayed.
	LastFailed *FoundryAISettingOutput `json:"last_failed,omitempty"`
	// ProRequired: the workspace's plan doesn't include bringing its own key.
	ProRequired bool `json:"pro_required,omitempty"`
	// SettingsURL opens Settings › AI models, where an owner changes it.
	SettingsURL string `json:"settings_url,omitempty"`
}

func (s *Service) GetFoundryAISettings(
	ctx context.Context, identity *Identity, call *Call, input FoundryWorkspaceInput,
) (GetFoundryAISettingsOutput, int, error) {
	call.ResourceKind, call.ResourceID = "workspace", input.WorkspaceID
	if !ValidIDs([]string{input.WorkspaceID}) {
		return GetFoundryAISettingsOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.GetFoundryAIRequest{WorkspaceId: input.WorkspaceID}
	if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "GetFoundryAI"); err != nil {
		return GetFoundryAISettingsOutput{}, 0, err
	}
	response, err := s.Foundry.GetFoundryAI(ctx, req)
	if err != nil {
		return GetFoundryAISettingsOutput{}, 0, FoundryError(err)
	}
	a := response.GetAi()
	out := GetFoundryAISettingsOutput{ContentTrust: contentTrustUntrusted, ProRequired: a.GetProRequired(),
		Pending: foundryAISetting(a.GetPending()), LastFailed: foundryAISetting(a.GetLastFailed())}
	switch active, def := a.GetActive(), a.GetDefault(); {
	case active != nil:
		out.InForce = foundryAISetting(active)
	case def != nil:
		out.InForce = &FoundryAISettingOutput{Source: "default", Kind: foundryAIKind(def.GetKind()), Provider: def.GetLabel(),
			Models: foundryAIModels(def.GetModels()), Assistant: "unchecked"}
	}
	if s.FoundryURL != "" {
		out.SettingsURL = s.FoundryURL + "/settings/ai?" + url.Values{"workspace": {input.WorkspaceID}}.Encode()
	}
	return out, 1, nil
}

// foundryAISetting is a saved setting as the tool returns it; nil for none.
func foundryAISetting(c *foundryv1.FoundryAIConfig) *FoundryAISettingOutput {
	if c == nil {
		return nil
	}
	out := &FoundryAISettingOutput{
		Source: "workspace", Kind: foundryAIKind(c.GetKind()), Provider: c.GetLabel(), Models: foundryAIModels(c.GetModels()),
		Test:      strings.ToLower(strings.TrimPrefix(c.GetProbeState().String(), "FOUNDRY_AI_PROBE_STATE_")),
		Assistant: "unchecked",
	}
	if c.GetProbeFailure() != "" {
		out.Failure = strings.Trim(c.GetProbeFailure()+"/"+c.GetProbeDetail(), "/")
	}
	if check := c.GetAssistantCheck(); check != foundryv1.FoundryAIAssistantCheck_FOUNDRY_AI_ASSISTANT_CHECK_UNSPECIFIED {
		out.Assistant = strings.ToLower(strings.TrimPrefix(check.String(), "FOUNDRY_AI_ASSISTANT_CHECK_"))
	}
	return out
}

func foundryAIKind(k foundryv1.FoundryAIKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "FOUNDRY_AI_KIND_"))
}

func foundryAIModels(m *foundryv1.FoundryAIModels) FoundryAIModelsOutput {
	return FoundryAIModelsOutput{Draft: m.GetDraft().GetName(), Ask: m.GetAsk().GetName(), Extract: m.GetExtract().GetName(), Verify: m.GetVerify().GetName()}
}
