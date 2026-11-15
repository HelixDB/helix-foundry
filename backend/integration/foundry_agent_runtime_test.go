package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// runtimeBackend is a local install's runtime: Claude Code online on one helper.
type runtimeBackend struct {
	foundryv1.FoundryServiceClient
	methods []string
	updated *foundryv1.UpdateFoundryAgentRuntimeRequest
}

var seen = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

func (b *runtimeBackend) GetFoundryAgentRuntime(context.Context, *foundryv1.GetFoundryAgentRuntimeRequest, ...grpc.CallOption) (*foundryv1.FoundryAgentRuntimeResponse, error) {
	b.methods = append(b.methods, "GetFoundryAgentRuntime")
	return &foundryv1.FoundryAgentRuntimeResponse{Runtime: &foundryv1.FoundryAgentRuntime{Runtime: "api", Online: []string{"claude_code"},
		Hosts: []*foundryv1.FoundryAgentHost{{Id: "host-1", Name: "Ada's Mac", Runtimes: []string{"claude_code"}, Version: "0.1.0", Platform: "darwin-arm64",
			LastSeenAt: timestamppb.New(seen), Online: true,
			Accounts: []*foundryv1.FoundryAgentHostAccount{{Runtime: "claude_code", State: "signed_in", Subscription: "max"}}}}}}, nil
}

func (b *runtimeBackend) UpdateFoundryAgentRuntime(_ context.Context, r *foundryv1.UpdateFoundryAgentRuntimeRequest, _ ...grpc.CallOption) (*foundryv1.FoundryAgentRuntimeResponse, error) {
	b.methods, b.updated = append(b.methods, "UpdateFoundryAgentRuntime"), r
	return &foundryv1.FoundryAgentRuntimeResponse{Runtime: &foundryv1.FoundryAgentRuntime{Runtime: r.GetRuntime(), Online: []string{"claude_code"}}}, nil
}

func (b *runtimeBackend) CreateFoundryAgentHostPairing(context.Context, *foundryv1.CreateFoundryAgentHostPairingRequest, ...grpc.CallOption) (*foundryv1.FoundryAgentHostPairingResponse, error) {
	b.methods = append(b.methods, "CreateFoundryAgentHostPairing")
	return &foundryv1.FoundryAgentHostPairingResponse{Code: "hxp_code", ExpiresAt: timestamppb.New(seen), Command: "npx -y @helixdb/foundry-agent-host@0.1.0 pair hxp_code"}, nil
}

func (b *runtimeBackend) RevokeFoundryAgentHost(_ context.Context, r *foundryv1.RevokeFoundryAgentHostRequest, _ ...grpc.CallOption) (*foundryv1.FoundryAgentHostResponse, error) {
	b.methods = append(b.methods, "RevokeFoundryAgentHost")
	return &foundryv1.FoundryAgentHostResponse{Host: &foundryv1.FoundryAgentHost{Id: r.GetId(), Name: "Ada's Mac", LastSeenAt: timestamppb.New(seen),
		RevokedAt: timestamppb.New(seen)}}, nil
}

func runtimeService(b *runtimeBackend, hosts bool) *Service {
	return &Service{Foundry: b, Actions: FoundryClients{FoundryServiceClient: b}, Confirmations: &memConfirmations{}, FoundryURL: "http://localhost:3000",
		AgentHosts: hosts}
}

// Only a host with helpers offers where the Assistant runs, to people.
func TestTheAssistantsRuntimeIsALocalInstallsOnly(t *testing.T) {
	names := func(s *Service, identity *Identity) []string {
		listed, err := connect(t, s, identity).ListTools(context.Background(), nil)
		require.NoError(t, err)
		var out []string
		for _, tool := range listed.Tools {
			out = append(out, tool.Name)
		}
		return out
	}
	b := &runtimeBackend{}
	assert.NotContains(t, names(runtimeService(b, false), reader()), toolFoundryGetAssistantRuntime)
	assert.Contains(t, names(runtimeService(b, true), reader()), toolFoundryGetAssistantRuntime)
	key := reader()
	key.ServiceKey = true
	assert.NotContains(t, names(runtimeService(b, true), key), toolFoundryGetAssistantRuntime)

	schema := func(s *Service) string {
		listed, err := connect(t, s, reader()).ListTools(context.Background(), nil)
		require.NoError(t, err)
		for _, tool := range listed.Tools {
			if tool.Name == toolPrepareFoundry {
				raw, err := json.Marshal(tool.InputSchema)
				require.NoError(t, err)
				return string(raw)
			}
		}
		return ""
	}
	for _, op := range []string{"set_assistant_runtime", "pair_assistant_helper", "revoke_assistant_helper"} {
		assert.NotContains(t, schema(runtimeService(b, false)), op)
		assert.Contains(t, schema(runtimeService(b, true)), op)
		_, err := runtimeService(b, false).ValidateFoundryOperation(context.Background(), reader(), FoundryOperationInput{
			Operation: op, Target: "workspace:workspace-1", Payload: map[string]any{}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "only on a local install")
	}
}

func TestGetAssistantRuntime(t *testing.T) {
	b := &runtimeBackend{}
	out := callTool(t, connect(t, runtimeService(b, true), reader()), toolFoundryGetAssistantRuntime, `{"workspace_id":"workspace-1"}`)
	assert.Equal(t, "api", out["runtime"])
	assert.Equal(t, []any{"claude_code"}, out["online"])
	assert.Equal(t, "http://localhost:3000/settings/ai?workspace=workspace-1", out["settings_url"])
	hosts := out["hosts"].([]any)
	require.Len(t, hosts, 1)
	assert.Equal(t, map[string]any{"id": "host-1", "name": "Ada's Mac", "runtimes": []any{"claude_code"}, "version": "0.1.0", "platform": "darwin-arm64",
		"last_seen_at": "2026-11-15T09:00:00Z", "online": true,
		"accounts": []any{map[string]any{"runtime": "claude_code", "state": "signed_in", "subscription": "max"}}}, hosts[0])
}

func TestTheRuntimesOperationsAreConfirmed(t *testing.T) {
	ctx := context.Background()
	b := &runtimeBackend{}
	s := runtimeService(b, true)
	run := func(op string, payload map[string]any) any {
		t.Helper()
		input := FoundryOperationInput{Operation: op, Target: "workspace:workspace-1", Payload: payload}
		prepared, _, err := s.PrepareFoundryOperation(ctx, reader(), &Call{}, input)
		require.NoError(t, err)
		out, _, err := s.ExecuteFoundryOperation(ctx, reader(), &Call{}, ExecuteFoundryInput{Operation: op, Target: input.Target, Payload: payload,
			ConfirmationID: prepared.ConfirmationID, ConfirmationToken: prepared.ConfirmationToken})
		require.NoError(t, err)
		return out.Result
	}
	assert.Equal(t, map[string]any{"runtime": "claude_code", "online": []string{"claude_code"}}, run("set_assistant_runtime", map[string]any{"runtime": "claude_code"}))
	assert.Equal(t, "claude_code", b.updated.GetRuntime())
	paired := run("pair_assistant_helper", map[string]any{}).(map[string]string)
	assert.Equal(t, "hxp_code", paired["code"])
	assert.Contains(t, paired["command"], "pair hxp_code")
	revoked := run("revoke_assistant_helper", map[string]any{"host_id": "host-1"}).(AssistantHostOutput)
	assert.Equal(t, "host-1", revoked.ID)
	assert.Equal(t, []string{"UpdateFoundryAgentRuntime", "CreateFoundryAgentHostPairing", "RevokeFoundryAgentHost"}, b.methods)

	for name, input := range map[string]FoundryOperationInput{
		"another runtime":       {Operation: "set_assistant_runtime", Target: "workspace:workspace-1", Payload: map[string]any{"runtime": "gemini"}},
		"codex, not offered":    {Operation: "set_assistant_runtime", Target: "workspace:workspace-1", Payload: map[string]any{"runtime": "codex"}},
		"an extra field":        {Operation: "set_assistant_runtime", Target: "workspace:workspace-1", Payload: map[string]any{"runtime": "api", "x": 1}},
		"an ontology target":    {Operation: "set_assistant_runtime", Target: "ontology:workspace-1/ontology-1", Payload: map[string]any{"runtime": "api"}},
		"a pairing with fields": {Operation: "pair_assistant_helper", Target: "workspace:workspace-1", Payload: map[string]any{"name": "x"}},
		"no host":               {Operation: "revoke_assistant_helper", Target: "workspace:workspace-1", Payload: map[string]any{}},
		"a bad host id":         {Operation: "revoke_assistant_helper", Target: "workspace:workspace-1", Payload: map[string]any{"host_id": "../x"}},
	} {
		_, err := s.ValidateFoundryOperation(ctx, reader(), input)
		assert.Error(t, err, name)
	}
	key := reader()
	key.ServiceKey = true
	_, err := s.ValidateFoundryOperation(ctx, key, FoundryOperationInput{Operation: "pair_assistant_helper", Target: "workspace:workspace-1", Payload: map[string]any{}})
	assert.ErrorIs(t, err, errCodeNotFound, "a key never pairs a helper")
}
