package api

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryagent "github.com/helixdb/helix-foundry/backend/agent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// agentHosts is a local install's helpers in memory: Ada's Mac runs Claude
// Code and is online; an old laptop ran Codex an hour ago.
type agentHosts struct {
	runtimes map[string]foundrymodel.FoundryAgentRuntime
	hosts    []foundryagent.Host
	revoked  []string
}

const macID = "01JAAAAAAAAAAAAAAAAAAAAAAA"

func newAgentHosts() *agentHosts {
	return &agentHosts{runtimes: map[string]foundrymodel.FoundryAgentRuntime{}, hosts: []foundryagent.Host{
		{ID: macID, Name: "Ada's Mac", Runtimes: []foundrymodel.FoundryAgentRuntime{foundrymodel.FoundryAgentRuntimeClaudeCode}, LastSeenAt: ago(0)},
		{ID: "01JBBBBBBBBBBBBBBBBBBBBBBB", Name: "Old laptop", Runtimes: []foundrymodel.FoundryAgentRuntime{foundrymodel.FoundryAgentRuntimeCodex},
			LastSeenAt: ago(time.Hour)},
		// Paired a moment ago, and not started yet.
		{ID: "01JDDDDDDDDDDDDDDDDDDDDDDD", Name: "New laptop", Runtimes: []foundrymodel.FoundryAgentRuntime{foundrymodel.FoundryAgentRuntimeCodex}},
	}}
}

// ago is a time d before now.
func ago(d time.Duration) *time.Time {
	t := time.Now().Add(-d)
	return &t
}

func (h *agentHosts) AgentRuntime(_ context.Context, ws string) (foundrymodel.FoundryAgentRuntime, error) {
	if r, ok := h.runtimes[ws]; ok {
		return r, nil
	}
	return foundrymodel.FoundryAgentRuntimeAPI, nil
}

func (h *agentHosts) SetAgentRuntime(_ context.Context, ws string, r foundrymodel.FoundryAgentRuntime) error {
	h.runtimes[ws] = r
	return nil
}

func (h *agentHosts) AgentRuntimeOnline(_ context.Context, r foundrymodel.FoundryAgentRuntime) (bool, error) {
	for _, host := range h.hosts {
		for _, offered := range host.Runtimes {
			if offered == r && host.Online(time.Now()) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (h *agentHosts) ListAgentHosts(context.Context) ([]foundryagent.Host, error) {
	return h.hosts, nil
}

func (h *agentHosts) CreateAgentPairing(context.Context) (string, time.Time, error) {
	return "hxp_code", time.Now().Add(10 * time.Minute), nil
}

func (h *agentHosts) RevokeAgentHost(_ context.Context, id string) (foundryagent.Host, error) {
	for _, host := range h.hosts {
		if host.ID == id {
			h.revoked = append(h.revoked, id)
			now := time.Now()
			host.RevokedAt = &now
			return host, nil
		}
	}
	return foundryagent.Host{}, foundrystore.ErrNotFound
}

// wsOwner may choose ws-1's model; anyone else may not.
type wsOwner struct{ ports.People }

func (wsOwner) MayManageAI(_ context.Context, ws, userID string) bool {
	return ws == "ws-1" && userID == person.UserID
}

func withHosts(t *testing.T) (agentFixture, *agentHosts) {
	t.Helper()
	f := newAgentFixture(t)
	hosts := newAgentHosts()
	f.s.deps.AgentHosts, f.s.deps.People = hosts, wsOwner{}
	return f, hosts
}

func TestTheRuntimeRoutesAreALocalOwnersOnly(t *testing.T) {
	get := &foundryv1.GetFoundryAgentRuntimeRequest{WorkspaceId: "ws-1"}
	hosted := newAgentFixture(t)
	_, err := hosted.s.GetFoundryAgentRuntime(personCtx, get)
	assert.Equal(t, "FOUNDRY_AGENT_HOSTS_LOCAL_ONLY", reasonOf(err), "hosted Helix has no helpers")

	f, _ := withHosts(t)
	_, err = f.s.GetFoundryAgentRuntime(ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceUser, UserID: "user-eve"}), get)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = f.s.CreateFoundryAgentHostPairing(ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceMCP, UserID: person.UserID}),
		&foundryv1.CreateFoundryAgentHostPairingRequest{WorkspaceId: "ws-1"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "only a person signed in")
	f.s.deps.Cfg.Foundry.Agent.Enabled = false
	_, err = f.s.GetFoundryAgentRuntime(personCtx, get)
	assert.Equal(t, "FOUNDRY_AGENT_DISABLED", reasonOf(err))
}

func TestGetAndUpdateTheRuntime(t *testing.T) {
	f, hosts := withHosts(t)
	got, err := f.s.GetFoundryAgentRuntime(personCtx, &foundryv1.GetFoundryAgentRuntimeRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	r := got.GetRuntime()
	assert.Equal(t, "api", r.GetRuntime())
	assert.Equal(t, []string{"claude_code"}, r.GetOnline(), "only an online helper's runtimes")
	require.Len(t, r.GetHosts(), 3)
	assert.True(t, r.GetHosts()[0].GetOnline())
	assert.False(t, r.GetHosts()[1].GetOnline())
	assert.Equal(t, []string{"codex"}, r.GetHosts()[1].GetRuntimes())
	assert.False(t, r.GetHosts()[2].GetOnline(), "a helper that only paired isn't running")
	assert.Nil(t, r.GetHosts()[2].GetLastSeenAt(), "nor was it ever seen")

	_, err = f.s.UpdateFoundryAgentRuntime(personCtx, &foundryv1.UpdateFoundryAgentRuntimeRequest{WorkspaceId: "ws-1", Runtime: "gemini"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = f.s.UpdateFoundryAgentRuntime(personCtx, &foundryv1.UpdateFoundryAgentRuntimeRequest{WorkspaceId: "ws-1", Runtime: "codex"})
	assert.Equal(t, "FOUNDRY_AGENT_RUNTIME_UNAVAILABLE", reasonOf(err), "no configuration closes Codex to its computer yet")
	hosts.hosts = nil
	updated, err := f.s.UpdateFoundryAgentRuntime(personCtx, &foundryv1.UpdateFoundryAgentRuntimeRequest{WorkspaceId: "ws-1", Runtime: "claude_code"})
	require.NoError(t, err)
	assert.Equal(t, "claude_code", updated.GetRuntime().GetRuntime(), "a helper need not be online to be chosen")
	assert.Equal(t, foundrymodel.FoundryAgentRuntimeClaudeCode, hosts.runtimes["ws-1"])
}

func TestPairAndRevokeAHelper(t *testing.T) {
	f, hosts := withHosts(t)
	paired, err := f.s.CreateFoundryAgentHostPairing(personCtx, &foundryv1.CreateFoundryAgentHostPairingRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	assert.Equal(t, "hxp_code", paired.GetCode())
	assert.Equal(t, "npx -y @helixdb/foundry-agent-host@0.1.0 pair hxp_code", paired.GetCommand(), "a helper's default port needs no --foundry")
	f.s.deps.AgentHostURL = foundryagent.DefaultHelperFoundry
	paired, err = f.s.CreateFoundryAgentHostPairing(personCtx, &foundryv1.CreateFoundryAgentHostPairingRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	assert.Equal(t, "npx -y @helixdb/foundry-agent-host@0.1.0 pair hxp_code", paired.GetCommand())
	// FOUNDRY_MCP_PORT moved the listener: the command says where.
	f.s.deps.AgentHostURL = "http://127.0.0.1:8090"
	paired, err = f.s.CreateFoundryAgentHostPairing(personCtx, &foundryv1.CreateFoundryAgentHostPairingRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	assert.Equal(t, "npx -y @helixdb/foundry-agent-host@0.1.0 pair hxp_code --foundry http://127.0.0.1:8090", paired.GetCommand())

	_, err = f.s.RevokeFoundryAgentHost(personCtx, &foundryv1.RevokeFoundryAgentHostRequest{WorkspaceId: "ws-1", Id: "../x"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = f.s.RevokeFoundryAgentHost(personCtx, &foundryv1.RevokeFoundryAgentHostRequest{WorkspaceId: "ws-1", Id: "01JCCCCCCCCCCCCCCCCCCCCCCC"})
	assert.Equal(t, codes.NotFound, status.Code(err))
	revoked, err := f.s.RevokeFoundryAgentHost(personCtx, &foundryv1.RevokeFoundryAgentHostRequest{WorkspaceId: "ws-1", Id: macID})
	require.NoError(t, err)
	assert.NotNil(t, revoked.GetHost().GetRevokedAt())
	assert.False(t, revoked.GetHost().GetOnline())
	assert.Equal(t, []string{macID}, hosts.revoked)
}

func TestASendRunsOnItsThreadsRuntime(t *testing.T) {
	send := func(f agentFixture, token, thread string) (*foundryv1.FoundryAgentTurnResponse, error) {
		return f.s.SendFoundryAgentMessage(personCtx, &foundryv1.SendFoundryAgentMessageRequest{WorkspaceId: "ws-1", Input: textInput("Load it"),
			ClientToken: token, ThreadId: thread})
	}
	t.Run("a new thread takes the workspace's, on a helper's subscription", func(t *testing.T) {
		f, hosts := withHosts(t)
		hosts.runtimes["ws-1"] = foundrymodel.FoundryAgentRuntimeClaudeCode
		// The workspace's own model needn't run: the helper's CLI does.
		f.s.deps.AI = deployment(t, aiConfig(modeLocal, true))
		resp, err := send(f, "c1", "")
		require.NoError(t, err)
		assert.Equal(t, "claude_code", resp.GetThread().GetRuntime())
		assert.Equal(t, foundrymodel.FoundryAgentRuntimeClaudeCode, f.store.threads[resp.GetThread().GetId()].Runtime)
	})
	t.Run("an 'api' thread still needs the workspace's model", func(t *testing.T) {
		f, _ := withHosts(t)
		f.s.deps.AI = deployment(t, aiConfig(modeLocal, true))
		_, err := send(f, "c1", "")
		assert.Equal(t, "FOUNDRY_AI_NOT_CONFIGURED", reasonOf(err))
	})
	t.Run("a helper's thread waits for its helper", func(t *testing.T) {
		f, hosts := withHosts(t)
		hosts.runtimes["ws-1"] = foundrymodel.FoundryAgentRuntimeCodex
		_, err := send(f, "c1", "")
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Equal(t, "FOUNDRY_AGENT_HOST_OFFLINE", reasonOf(err))
		assert.Empty(t, f.store.turns, "nothing is queued")

		// An existing thread keeps its runtime, whatever the workspace's is now.
		hosts.runtimes["ws-1"] = foundrymodel.FoundryAgentRuntimeAPI
		f.store.threads["th-codex"] = &foundrymodel.FoundryAgentThread{ID: "th-codex", WorkspaceID: "ws-1", UserID: person.UserID,
			Runtime: foundrymodel.FoundryAgentRuntimeCodex}
		_, err = send(f, "c2", "th-codex")
		assert.Equal(t, "FOUNDRY_AGENT_HOST_OFFLINE", reasonOf(err))
		f.store.threads["th-claude"] = &foundrymodel.FoundryAgentThread{ID: "th-claude", WorkspaceID: "ws-1", UserID: person.UserID,
			Runtime: foundrymodel.FoundryAgentRuntimeClaudeCode}
		_, err = send(f, "c3", "th-claude")
		require.NoError(t, err)
	})
	t.Run("a host without helpers runs every new thread on its worker", func(t *testing.T) {
		f := newAgentFixture(t)
		resp, err := send(f, "c1", "")
		require.NoError(t, err)
		assert.Equal(t, "api", resp.GetThread().GetRuntime())
		f.store.threads["th-claude"] = &foundrymodel.FoundryAgentThread{ID: "th-claude", WorkspaceID: "ws-1", UserID: person.UserID,
			Runtime: foundrymodel.FoundryAgentRuntimeClaudeCode}
		_, err = send(f, "c2", "th-claude")
		assert.Equal(t, "FOUNDRY_AGENT_HOST_OFFLINE", reasonOf(err))
	})
}
