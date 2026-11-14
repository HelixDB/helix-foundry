package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// aiBackend answers GetFoundryAI with ai, or err.
type aiBackend struct {
	foundryv1.FoundryServiceClient
	ai  *foundryv1.FoundryAI
	err error
}

func (b *aiBackend) GetFoundryAI(_ context.Context, r *foundryv1.GetFoundryAIRequest, _ ...grpc.CallOption) (*foundryv1.FoundryAIResponse, error) {
	if r.GetWorkspaceId() != "workspace-1" {
		return nil, status.Error(codes.NotFound, "no such workspace")
	}
	return &foundryv1.FoundryAIResponse{Ai: b.ai}, b.err
}

func aiModels(ask string) *foundryv1.FoundryAIModels {
	return &foundryv1.FoundryAIModels{Draft: &foundryv1.FoundryAIModel{Name: "draft-m"}, Ask: &foundryv1.FoundryAIModel{Name: ask},
		Extract: &foundryv1.FoundryAIModel{Name: "extract-m"}, Verify: &foundryv1.FoundryAIModel{Name: "verify-m"}}
}

func TestGetAISettings(t *testing.T) {
	b := &aiBackend{ai: &foundryv1.FoundryAI{
		Active: &foundryv1.FoundryAIConfig{Id: "cfg-1", Kind: foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OPENAI_COMPATIBLE, Label: "llm.example.com",
			Models: aiModels("qwen3"), KeyHint: "sk-…4f2a", HasKey: true, ProbeState: foundryv1.FoundryAIProbeState_FOUNDRY_AI_PROBE_STATE_PASSED,
			AssistantCheck: foundryv1.FoundryAIAssistantCheck_FOUNDRY_AI_ASSISTANT_CHECK_NO_TOOL_CALL},
		Pending: &foundryv1.FoundryAIConfig{Id: "cfg-2", Kind: foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_BEDROCK, Label: "Amazon Bedrock",
			Models: aiModels("global.anthropic.claude-sonnet-5-5"), ProbeState: foundryv1.FoundryAIProbeState_FOUNDRY_AI_PROBE_STATE_RUNNING},
		LastFailed: &foundryv1.FoundryAIConfig{Id: "cfg-0", Kind: foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OPENAI, Label: "OpenAI",
			Models: aiModels("gpt-6.1-sol"), ProbeState: foundryv1.FoundryAIProbeState_FOUNDRY_AI_PROBE_STATE_FAILED, ProbeFailure: "auth", ProbeDetail: "none"},
		Default:     &foundryv1.FoundryAIDefault{Kind: foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_ANTHROPIC, Label: "Claude", Models: aiModels("claude-sonnet-5-5")},
		ProRequired: true,
	}}
	identity := everyTool()
	var methods []string
	identity.Authorize = func(_ context.Context, _ any, grant Grant, m ...string) error {
		assert.Equal(t, foundryUserOnly, grant, "no service key reads it")
		methods = append(methods, m...)
		return nil
	}
	session := connect(t, &Service{Foundry: b, FoundryURL: "https://foundry.example.com"}, identity)
	out := callTool(t, session, toolFoundryGetAISettings, `{"workspace_id":"workspace-1"}`)
	assert.Equal(t, []string{"GetFoundryAI"}, methods)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "4f2a", "a key's hint never reaches an agent")
	assert.Equal(t, map[string]any{
		"source": "workspace", "kind": "openai_compatible", "provider": "llm.example.com", "test": "passed", "assistant": "no_tool_call",
		"models": map[string]any{"draft": "draft-m", "ask": "qwen3", "extract": "extract-m", "verify": "verify-m"},
	}, out["in_force"])
	assert.Equal(t, "running", out["pending"].(map[string]any)["test"])
	assert.Equal(t, "unchecked", out["pending"].(map[string]any)["assistant"])
	assert.Equal(t, "auth/none", out["last_failed"].(map[string]any)["failure"])
	assert.Equal(t, true, out["pro_required"])
	assert.Equal(t, "https://foundry.example.com/settings/ai?workspace=workspace-1", out["settings_url"])
	assert.Equal(t, contentTrustUntrusted, out["content_trust"])

	// No setting saved: the default is in force; none at all, nothing is.
	b.ai = &foundryv1.FoundryAI{Default: &foundryv1.FoundryAIDefault{Kind: foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_ANTHROPIC, Label: "Claude", Models: aiModels("claude-sonnet-5-5")}}
	out = callTool(t, session, toolFoundryGetAISettings, `{"workspace_id":"workspace-1"}`)
	assert.Equal(t, map[string]any{
		"source": "default", "kind": "anthropic", "provider": "Claude", "assistant": "unchecked",
		"models": map[string]any{"draft": "draft-m", "ask": "claude-sonnet-5-5", "extract": "extract-m", "verify": "verify-m"},
	}, out["in_force"])
	b.ai = &foundryv1.FoundryAI{}
	out = callTool(t, (connect(t, &Service{Foundry: b}, identity)), toolFoundryGetAISettings, `{"workspace_id":"workspace-1"}`)
	assert.NotContains(t, out, "in_force")
	assert.NotContains(t, out, "settings_url", "a stamp that names no website links nowhere")

	// A workspace the caller can't read fails as Foundry answered it.
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: toolFoundryGetAISettings, Arguments: json.RawMessage(`{"workspace_id":"workspace-2"}`)})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}
