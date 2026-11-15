package directory_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/local/directory"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// needHosts is the directory with no helper, pairing or turn token.
func needHosts(t *testing.T) *directory.Store {
	t.Helper()
	st := needDB(t)
	for _, table := range []string{"foundry_local_agent_turn_tokens", "foundry_local_agent_hosts", "foundry_local_agent_pairings", "foundry_local_agent_settings"} {
		_, err := testDB.Exec("DELETE FROM " + table)
		require.NoError(t, err)
	}
	return st
}

var claude = []model.FoundryAgentRuntime{model.FoundryAgentRuntimeClaudeCode}

func pair(t *testing.T, st *directory.Store, name string) (agent.Host, string) {
	t.Helper()
	code, expires, err := st.CreateAgentPairing(context.Background())
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(code, directory.PairingPrefix))
	assert.WithinDuration(t, time.Now().Add(directory.PairingTTL), expires, 5*time.Second)
	host, token, err := st.PairAgentHost(context.Background(), code, agent.Host{Name: name, Version: "0.1.0", Platform: "darwin-arm64", Runtimes: claude})
	require.NoError(t, err)
	return host, token
}

func TestAPairingCodePairsOneHelperOnce(t *testing.T) {
	st := needHosts(t)
	ctx := context.Background()
	code, _, err := st.CreateAgentPairing(ctx)
	require.NoError(t, err)
	host, token, err := st.PairAgentHost(ctx, code, agent.Host{Name: " Ada's Mac ", Version: "0.1.0", Platform: "darwin-arm64",
		Runtimes: []model.FoundryAgentRuntime{model.FoundryAgentRuntimeCodex, model.FoundryAgentRuntimeClaudeCode, model.FoundryAgentRuntimeCodex}})
	require.NoError(t, err)
	assert.Equal(t, "Ada's Mac", host.Name)
	assert.Equal(t, []model.FoundryAgentRuntime{model.FoundryAgentRuntimeClaudeCode, model.FoundryAgentRuntimeCodex}, host.Runtimes)
	assert.True(t, strings.HasPrefix(token, directory.HostTokenPrefix))
	assert.True(t, host.Online(time.Now()))

	_, _, err = st.PairAgentHost(ctx, code, agent.Host{Name: "Again", Runtimes: claude})
	assert.ErrorIs(t, err, store.ErrNotFound, "a code pairs once")
	_, _, err = st.PairAgentHost(ctx, "hxp_unknown", agent.Host{Name: "Guess", Runtimes: claude})
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, _, err = st.PairAgentHost(ctx, token, agent.Host{Name: "Token", Runtimes: claude})
	assert.ErrorIs(t, err, store.ErrNotFound, "only a pairing code pairs")

	expired, _, err := st.CreateAgentPairing(ctx)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_local_agent_pairings SET expires_at = ? WHERE used_at IS NULL`, time.Now().UTC().Add(-time.Second))
	require.NoError(t, err)
	_, _, err = st.PairAgentHost(ctx, expired, agent.Host{Name: "Late", Runtimes: claude})
	assert.ErrorIs(t, err, store.ErrNotFound)

	fresh, _, err := st.CreateAgentPairing(ctx)
	require.NoError(t, err)
	for _, bad := range []agent.Host{
		{Name: "", Runtimes: claude}, {Name: strings.Repeat("n", 129), Runtimes: claude}, {Name: "Tab\there", Runtimes: claude},
		{Name: "Api", Runtimes: []model.FoundryAgentRuntime{model.FoundryAgentRuntimeAPI}}, {Name: "V", Version: strings.Repeat("v", 65), Runtimes: claude},
	} {
		_, _, err := st.PairAgentHost(ctx, fresh, bad)
		assert.ErrorIs(t, err, directory.ErrInvalidAgentHost, "%+v", bad)
	}
	_, _, err = st.PairAgentHost(ctx, fresh, agent.Host{Name: "Valid", Runtimes: claude})
	require.NoError(t, err, "a refused pairing spends no code")

	got, err := st.AgentHostByToken(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, host.ID, got.ID)
	_, err = st.AgentHostByToken(ctx, token+"x")
	assert.ErrorIs(t, err, store.ErrNotFound)
	var stored string
	require.NoError(t, testDB.QueryRow(`SELECT token_hash FROM foundry_local_agent_hosts WHERE id = ?`, host.ID).Scan(&stored))
	assert.NotContains(t, stored, token[len(directory.HostTokenPrefix):], "only the token's hash is kept")
	assert.Len(t, stored, 64)
}

func TestAHelpersRuntimesAndPresence(t *testing.T) {
	st := needHosts(t)
	ctx := context.Background()
	online, err := st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeClaudeCode)
	require.NoError(t, err)
	assert.False(t, online)
	_, err = st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeAPI)
	assert.Error(t, err)

	host, _ := pair(t, st, "Ada's Mac")
	online, err = st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeClaudeCode)
	require.NoError(t, err)
	assert.True(t, online)
	online, err = st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeCodex)
	require.NoError(t, err)
	assert.False(t, online, "it offers Claude Code only")

	require.NoError(t, st.SeeAgentHost(ctx, host.ID, []model.FoundryAgentRuntime{model.FoundryAgentRuntimeCodex}, "0.2.0"))
	seen, err := st.AgentHost(ctx, host.ID)
	require.NoError(t, err)
	assert.Equal(t, []model.FoundryAgentRuntime{model.FoundryAgentRuntimeCodex}, seen.Runtimes)
	assert.Equal(t, "0.2.0", seen.Version)
	assert.ErrorIs(t, st.SeeAgentHost(ctx, host.ID, []model.FoundryAgentRuntime{"gemini"}, "1"), directory.ErrInvalidAgentHost)

	_, err = testDB.Exec(`UPDATE foundry_local_agent_hosts SET last_seen_at = ?`, time.Now().UTC().Add(-agent.HostOnlineFor-time.Second))
	require.NoError(t, err)
	online, err = st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeCodex)
	require.NoError(t, err)
	assert.False(t, online, "a helper not seen for 30 seconds is offline")
	require.NoError(t, st.TouchAgentHost(ctx, host.ID))
	online, err = st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeCodex)
	require.NoError(t, err)
	assert.True(t, online)

	runtime, err := st.AgentRuntime(ctx, "ws-1")
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentRuntimeAPI, runtime, "the worker runs a workspace's threads unless the owner chose a helper")
	require.NoError(t, st.SetAgentRuntime(ctx, "ws-1", model.FoundryAgentRuntimeCodex))
	require.NoError(t, st.SetAgentRuntime(ctx, "ws-1", model.FoundryAgentRuntimeClaudeCode))
	runtime, err = st.AgentRuntime(ctx, "ws-1")
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentRuntimeClaudeCode, runtime)
	assert.Error(t, st.SetAgentRuntime(ctx, "ws-1", "gemini"))
}

func TestATurnsTokenWorksForItsClaimAlone(t *testing.T) {
	st := needHosts(t)
	ctx := context.Background()
	host, hostToken := pair(t, st, "Ada's Mac")
	claim := directory.AgentTurnToken{HostID: host.ID, TurnID: "turn-1", WorkspaceID: "ws-1", LeaseOwner: "agent-host:" + host.ID + ":aa",
		ExpiresAt: time.Now().Add(time.Minute), Deadline: time.Now().Add(4 * time.Minute)}
	_, err := st.CreateAgentTurnToken(ctx, directory.AgentTurnToken{HostID: host.ID})
	assert.Error(t, err, "a token names its claim")
	token, err := st.CreateAgentTurnToken(ctx, claim)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(token, directory.TurnTokenPrefix))

	got, err := st.AgentTurnToken(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, "turn-1", got.TurnID)
	assert.Equal(t, claim.LeaseOwner, got.LeaseOwner)
	_, err = st.AgentTurnToken(ctx, hostToken)
	assert.ErrorIs(t, err, store.ErrNotFound, "a helper's own token calls no tool")
	byClaim, err := st.AgentTurnClaim(ctx, host.ID, "turn-1")
	require.NoError(t, err)
	assert.Equal(t, claim.LeaseOwner, byClaim.LeaseOwner)
	_, err = st.AgentTurnClaim(ctx, "another-host", "turn-1")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Calls and questions are counted, up to the turn's.
	for range 3 {
		require.NoError(t, st.SpendAgentTurnCall(ctx, token, false, 4, 1))
	}
	require.NoError(t, st.SpendAgentTurnCall(ctx, token, true, 4, 1))
	assert.ErrorIs(t, st.SpendAgentTurnCall(ctx, token, false, 4, 1), directory.ErrTurnCallsSpent)
	other, err := st.CreateAgentTurnToken(ctx, claim)
	require.NoError(t, err)
	require.NoError(t, st.SpendAgentTurnCall(ctx, other, true, 4, 1))
	assert.ErrorIs(t, st.SpendAgentTurnCall(ctx, other, true, 4, 1), directory.ErrTurnAsksSpent)
	require.NoError(t, st.SpendAgentTurnCall(ctx, other, false, 4, 1), "a refused question spends nothing")

	// It lives with the claim's lease, and dies with its turn.
	_, err = testDB.Exec(`UPDATE foundry_local_agent_turn_tokens SET expires_at = ?`, time.Now().UTC().Add(-time.Second))
	require.NoError(t, err)
	_, err = st.AgentTurnToken(ctx, token)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, st.SpendAgentTurnCall(ctx, token, false, 4, 1), store.ErrNotFound)
	require.NoError(t, st.ExtendAgentTurnTokens(ctx, "turn-1", claim.LeaseOwner, time.Now().Add(time.Minute)))
	_, err = st.AgentTurnToken(ctx, token)
	assert.ErrorIs(t, err, store.ErrNotFound, "a lapsed claim isn't renewed")

	live, err := st.CreateAgentTurnToken(ctx, claim)
	require.NoError(t, err)
	require.NoError(t, st.ExtendAgentTurnTokens(ctx, "turn-1", claim.LeaseOwner, time.Now().Add(2*time.Minute)))
	got, err = st.AgentTurnToken(ctx, live)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(2*time.Minute), got.ExpiresAt, 5*time.Second)
	require.NoError(t, st.EndAgentTurnTokens(ctx, "turn-1"))
	_, err = st.AgentTurnToken(ctx, live)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestRevokingAHelperStopsItAndItsTurns(t *testing.T) {
	st := needHosts(t)
	ctx := context.Background()
	host, hostToken := pair(t, st, "Ada's Mac")
	other, _ := pair(t, st, "Old laptop")
	turnToken, err := st.CreateAgentTurnToken(ctx, directory.AgentTurnToken{HostID: host.ID, TurnID: "turn-1", WorkspaceID: "ws-1",
		LeaseOwner: "agent-host:x", ExpiresAt: time.Now().Add(time.Minute), Deadline: time.Now().Add(time.Minute)})
	require.NoError(t, err)

	listed, err := st.ListAgentHosts(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	assert.Equal(t, other.ID, listed[0].ID, "newest first")

	revoked, err := st.RevokeAgentHost(ctx, host.ID)
	require.NoError(t, err)
	require.NotNil(t, revoked.RevokedAt)
	assert.False(t, revoked.Online(time.Now()))
	again, err := st.RevokeAgentHost(ctx, host.ID)
	require.NoError(t, err)
	assert.Equal(t, revoked.RevokedAt.UnixMicro(), again.RevokedAt.UnixMicro(), "revoking again changes nothing")
	_, err = st.RevokeAgentHost(ctx, "01JZZZZZZZZZZZZZZZZZZZZZZZ")
	assert.ErrorIs(t, err, store.ErrNotFound)

	_, err = st.AgentHostByToken(ctx, hostToken)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = st.AgentTurnToken(ctx, turnToken)
	assert.ErrorIs(t, err, store.ErrNotFound)
	online, err := st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeClaudeCode)
	require.NoError(t, err)
	assert.True(t, online, "the other helper is still online")
	listed, err = st.ListAgentHosts(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, other.ID, listed[0].ID)
	require.NoError(t, st.SeeAgentHost(ctx, host.ID, claude, "0.1.0"))
	still, err := st.AgentHost(ctx, host.ID)
	require.NoError(t, err)
	assert.NotNil(t, still.RevokedAt, "a revoked helper's calls see nothing")
}
