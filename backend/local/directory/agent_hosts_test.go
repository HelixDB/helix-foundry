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
	assert.Nil(t, host.LastSeenAt, "pairing starts nothing: the helper is seen once it runs")
	assert.False(t, host.Online(time.Now()))

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
		{Name: "", Runtimes: claude}, {Name: "  ", Runtimes: claude}, {Name: strings.Repeat("n", 129), Runtimes: claude}, {Name: "Tab\there", Runtimes: claude},
		// A C1 control, a bidi override or isolate, and bytes that aren't
		// UTF-8 could make the owner's settings show one helper as another.
		{Name: "Next\u0085line", Runtimes: claude}, {Name: "Ada\u202esoM", Runtimes: claude}, {Name: "Ada\u2066Mac", Runtimes: claude},
		{Name: "Bad\xffbyte", Runtimes: claude},
		{Name: "Api", Runtimes: []model.FoundryAgentRuntime{model.FoundryAgentRuntimeAPI}}, {Name: "V", Version: strings.Repeat("v", 65), Runtimes: claude},
		// A version and platform are printable ASCII.
		{Name: "V", Version: "0.1.0\u202e", Runtimes: claude}, {Name: "P", Platform: "darwin-ärm64", Runtimes: claude},
	} {
		_, _, err := st.PairAgentHost(ctx, fresh, bad)
		assert.ErrorIs(t, err, directory.ErrInvalidAgentHost, "%+v", bad)
	}
	named, _, err := st.PairAgentHost(ctx, fresh, agent.Host{Name: "Café の Mac", Runtimes: claude})
	require.NoError(t, err, "a refused pairing spends no code, and a name may be any language's")
	assert.Equal(t, "Café の Mac", named.Name)

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
	assert.False(t, online, "a helper that only paired runs nothing yet")
	require.NoError(t, st.TouchAgentHost(ctx, host.ID))
	online, err = st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeClaudeCode)
	require.NoError(t, err)
	assert.True(t, online, "a helper that called is online")
	online, err = st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeCodex)
	require.NoError(t, err)
	assert.False(t, online, "it offers Claude Code only")

	paired, err := st.AgentHost(ctx, host.ID)
	require.NoError(t, err)
	assert.Empty(t, paired.Accounts, "a helper says what its CLIs are signed in to once it calls")

	codex := []model.FoundryAgentRuntime{model.FoundryAgentRuntimeCodex}
	plus := agent.HostAccount{Runtime: model.FoundryAgentRuntimeCodex, State: agent.AccountSignedIn, Subscription: "plus", Version: agent.MinCodexVersion}
	runs, err := st.SeeAgentHost(ctx, host.ID, codex, "0.2.0", []agent.HostAccount{plus})
	require.NoError(t, err)
	assert.Equal(t, codex, runs)
	seen, err := st.AgentHost(ctx, host.ID)
	require.NoError(t, err)
	assert.Equal(t, codex, seen.Runtimes)
	assert.Equal(t, "0.2.0", seen.Version)
	assert.Equal(t, []agent.HostAccount{plus}, seen.Accounts)
	_, err = st.SeeAgentHost(ctx, host.ID, []model.FoundryAgentRuntime{"gemini"}, "1", nil)
	assert.ErrorIs(t, err, directory.ErrInvalidAgentHost)
	// What its CLIs are signed in to is checked as it is said, and nothing
	// of a refused call is kept.
	for name, accounts := range map[string][]agent.HostAccount{
		"a runtime it doesn't offer": {{Runtime: model.FoundryAgentRuntimeClaudeCode, State: agent.AccountSignedIn}},
		"a runtime twice": {
			{Runtime: model.FoundryAgentRuntimeCodex, State: agent.AccountSignedIn},
			{Runtime: model.FoundryAgentRuntimeCodex, State: agent.AccountSignedOut}},
		"an unknown state":                    {{Runtime: model.FoundryAgentRuntimeCodex, State: "maybe"}},
		"a subscription while signed out":     {{Runtime: model.FoundryAgentRuntimeCodex, State: agent.AccountSignedOut, Subscription: "pro"}},
		"a subscription past 32 bytes":        {{Runtime: model.FoundryAgentRuntimeCodex, State: agent.AccountSignedIn, Subscription: strings.Repeat("x", 33)}},
		"a subscription with a control byte":  {{Runtime: model.FoundryAgentRuntimeCodex, State: agent.AccountSignedIn, Subscription: "pro\n"}},
		"a subscription with a bidi override": {{Runtime: model.FoundryAgentRuntimeCodex, State: agent.AccountSignedIn, Subscription: "max\u202e"}},
		"a subscription past ASCII":           {{Runtime: model.FoundryAgentRuntimeCodex, State: agent.AccountSignedIn, Subscription: "plüs"}},
		"a version past 64 bytes":             {{Runtime: model.FoundryAgentRuntimeCodex, State: agent.AccountSignedIn, Version: strings.Repeat("1", 65)}},
		"a version with a control byte":       {{Runtime: model.FoundryAgentRuntimeCodex, State: agent.AccountSignedIn, Version: "0.169.0\n"}},
	} {
		_, err := st.SeeAgentHost(ctx, host.ID, codex, "0.2.1", accounts)
		assert.ErrorIs(t, err, directory.ErrInvalidAgentHost, name)
	}
	kept, err := st.AgentHost(ctx, host.ID)
	require.NoError(t, err)
	assert.Equal(t, seen.Accounts, kept.Accounts)
	assert.Equal(t, "0.2.0", kept.Version)
	// A call that says nothing of its CLIs leaves none known, and a Codex of
	// no known version isn't run.
	runs, err = st.SeeAgentHost(ctx, host.ID, codex, "0.2.0", nil)
	require.NoError(t, err)
	assert.Empty(t, runs)
	kept, err = st.AgentHost(ctx, host.ID)
	require.NoError(t, err)
	assert.Empty(t, kept.Accounts)
	assert.Empty(t, kept.Runtimes)
	// An outdated Codex is offered, and said, but not run: it isn't online.
	old := agent.HostAccount{Runtime: model.FoundryAgentRuntimeCodex, State: agent.AccountSignedIn, Subscription: "plus", Version: "0.150.0"}
	both := []model.FoundryAgentRuntime{model.FoundryAgentRuntimeClaudeCode, model.FoundryAgentRuntimeCodex}
	runs, err = st.SeeAgentHost(ctx, host.ID, both, "0.2.0", []agent.HostAccount{old})
	require.NoError(t, err)
	assert.Equal(t, []model.FoundryAgentRuntime{model.FoundryAgentRuntimeClaudeCode}, runs)
	online, err = st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeCodex)
	require.NoError(t, err)
	assert.False(t, online, "an outdated Codex runs nothing")
	kept, err = st.AgentHost(ctx, host.ID)
	require.NoError(t, err)
	assert.Equal(t, []agent.HostAccount{old}, kept.Accounts, "what it said of the outdated Codex is shown")

	signedOut := agent.HostAccount{Runtime: model.FoundryAgentRuntimeCodex, State: agent.AccountSignedOut, Version: "0.170.0"}
	_, err = st.SeeAgentHost(ctx, host.ID, codex, "0.2.0", []agent.HostAccount{signedOut})
	require.NoError(t, err)
	listed, err := st.ListAgentHosts(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, []agent.HostAccount{signedOut}, listed[0].Accounts)

	_, err = testDB.Exec(`UPDATE foundry_local_agent_hosts SET last_seen_at = ?`, time.Now().UTC().Add(-agent.HostOnlineFor-time.Second))
	require.NoError(t, err)
	online, err = st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeCodex)
	require.NoError(t, err)
	assert.False(t, online, "a helper not seen for 30 seconds is offline")
	require.NoError(t, st.TouchAgentHost(ctx, host.ID))
	online, err = st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeCodex)
	require.NoError(t, err)
	assert.True(t, online)
}

func TestAWorkspacesRuntimeAndItsCodexConfirmation(t *testing.T) {
	st := needHosts(t)
	ctx := context.Background()
	setting, err := st.AgentRuntime(ctx, "ws-1")
	require.NoError(t, err)
	assert.Equal(t, agent.RuntimeSetting{Runtime: model.FoundryAgentRuntimeAPI}, setting, "the worker runs a workspace's threads unless the owner chose a helper")

	_, err = st.SetAgentRuntime(ctx, "ws-1", model.FoundryAgentRuntimeCodex, false)
	assert.ErrorIs(t, err, agent.ErrCodexUnconfirmed, "codex needs the owner's confirmation")
	setting, err = st.AgentRuntime(ctx, "ws-1")
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentRuntimeAPI, setting.Runtime, "a refused choice changes nothing")

	before := time.Now().UTC().Add(-time.Second)
	set, err := st.SetAgentRuntime(ctx, "ws-1", model.FoundryAgentRuntimeCodex, true)
	require.NoError(t, err)
	require.NotNil(t, set.CodexConfirmedAt)
	assert.True(t, set.CodexConfirmedAt.After(before))
	setting, err = st.AgentRuntime(ctx, "ws-1")
	require.NoError(t, err)
	assert.Equal(t, set, setting)

	// Kept when another is chosen, so codex is confirmed once.
	set, err = st.SetAgentRuntime(ctx, "ws-1", model.FoundryAgentRuntimeClaudeCode, false)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentRuntimeClaudeCode, set.Runtime)
	assert.Equal(t, setting.CodexConfirmedAt, set.CodexConfirmedAt)
	again, err := st.SetAgentRuntime(ctx, "ws-1", model.FoundryAgentRuntimeCodex, true)
	require.NoError(t, err)
	assert.Equal(t, setting.CodexConfirmedAt, again.CodexConfirmedAt, "the first confirmation is kept")
	_, err = st.SetAgentRuntime(ctx, "ws-1", "gemini", false)
	assert.Error(t, err)

	// Each workspace confirms its own.
	_, err = st.SetAgentRuntime(ctx, "ws-2", model.FoundryAgentRuntimeCodex, false)
	assert.ErrorIs(t, err, agent.ErrCodexUnconfirmed)
	_, err = testDB.Exec(`INSERT INTO foundry_local_agent_settings (workspace_id, runtime) VALUES ('ws-3', 'codex')`)
	assert.Error(t, err, "the table holds no unconfirmed codex")
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
	byClaim, err := st.AgentTurnClaim(ctx, host.ID, claim.LeaseOwner)
	require.NoError(t, err)
	assert.Equal(t, "turn-1", byClaim.TurnID)
	_, err = st.AgentTurnClaim(ctx, "another-host", claim.LeaseOwner)
	assert.ErrorIs(t, err, store.ErrNotFound)
	// Another claim of the same turn by the same helper (a slot that took it
	// again after this one's lease lapsed) is its own.
	again := claim
	again.LeaseOwner = "agent-host:" + host.ID + ":bb"
	_, err = st.CreateAgentTurnToken(ctx, again)
	require.NoError(t, err)
	byClaim, err = st.AgentTurnClaim(ctx, host.ID, claim.LeaseOwner)
	require.NoError(t, err)
	assert.Equal(t, claim.LeaseOwner, byClaim.LeaseOwner, "a claim reads its own token, not the newest of its turn")
	_, err = st.AgentTurnClaim(ctx, host.ID, "agent-host:"+host.ID+":cc")
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
	require.NoError(t, st.TouchAgentHost(ctx, other.ID))
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
	_, err = st.CreateAgentTurnToken(ctx, directory.AgentTurnToken{HostID: host.ID, TurnID: "turn-2", WorkspaceID: "ws-1",
		LeaseOwner: "agent-host:y", ExpiresAt: time.Now().Add(time.Minute), Deadline: time.Now().Add(time.Minute)})
	assert.ErrorIs(t, err, store.ErrNotFound, "a revoked helper's claim gets no token")
	var minted int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_local_agent_turn_tokens WHERE turn_id = 'turn-2'`).Scan(&minted))
	assert.Zero(t, minted)
	online, err := st.AgentRuntimeOnline(ctx, model.FoundryAgentRuntimeClaudeCode)
	require.NoError(t, err)
	assert.True(t, online, "the other helper is still online")
	listed, err = st.ListAgentHosts(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, other.ID, listed[0].ID)
	_, err = st.SeeAgentHost(ctx, host.ID, claude, "0.1.0", nil)
	require.NoError(t, err)
	still, err := st.AgentHost(ctx, host.ID)
	require.NoError(t, err)
	assert.NotNil(t, still.RevokedAt, "a revoked helper's calls see nothing")
}
