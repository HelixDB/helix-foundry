package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostMCP sends one JSON-RPC call to /mcp with bearer, and returns its HTTP
// status and envelope.
func hostMCP(t *testing.T, base, bearer, method string, params any) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(t, err)
	req, err := http.NewRequest("POST", base+"/mcp", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var envelope map[string]any
	_ = json.Unmarshal(raw, &envelope)
	return resp.StatusCode, envelope
}

// A helper pairs with a code the owner made, claims the turn of a thread
// that runs on its CLI, calls the Assistant's tools with the turn's token
// alone, reports its text and how it ended, and stops working once revoked.
func TestFrontRunsAHelpersTurns(t *testing.T) {
	rest, _, _, _ := startFront(t, "http://helix:8080")
	var ws map[string]any
	require.Equal(t, 200, call(t, "POST", rest+"/v1/workspaces", siteToken, map[string]any{"slug": "helper-test", "displayName": "Helper test", "region": "local"}, &ws))
	workspaceID := ws["id"].(string)
	agentURL := rest + "/v1/workspaces/" + workspaceID + "/foundry/agent"

	// The owner makes a code; the helper spends it, once.
	var pairing struct{ Code, Command, ExpiresAt string }
	require.Equal(t, 200, call(t, "POST", agentURL+"/host-pairings", siteToken, map[string]any{}, &pairing))
	require.True(t, strings.HasPrefix(pairing.Code, "hxp_"), pairing.Code)
	assert.Equal(t, "npx -y @helixdb/foundry-agent-host@0.1.0 pair "+pairing.Code, pairing.Command)
	var paired struct {
		HostID string `json:"host_id"`
		Token  string `json:"token"`
	}
	hello := map[string]any{"code": pairing.Code, "name": "Ada's Mac", "runtimes": []string{"claude_code"}, "version": "0.1.0", "platform": "darwin-arm64"}
	require.Equal(t, 200, call(t, "POST", rest+"/agent-host/pair", "", hello, &paired))
	require.True(t, strings.HasPrefix(paired.Token, "hxh_"))
	var refused struct{ Error struct{ Code string } }
	assert.Equal(t, 404, call(t, "POST", rest+"/agent-host/pair", "", hello, &refused))
	assert.Equal(t, "pairing_invalid", refused.Error.Code)
	hello["code"], hello["extra"] = pairing.Code, true
	assert.Equal(t, 400, call(t, "POST", rest+"/agent-host/pair", "", hello, &refused), "fields are checked")

	// Paired, it runs nothing until it is started: it isn't online yet.
	var runtime struct {
		Runtime struct {
			Runtime string
			Online  []string
			Hosts   []map[string]any
		}
	}
	require.Equal(t, 200, call(t, "GET", agentURL+"/runtime", siteToken, nil, &runtime))
	require.Len(t, runtime.Runtime.Hosts, 1)
	assert.Equal(t, false, runtime.Runtime.Hosts[0]["online"])
	assert.Nil(t, runtime.Runtime.Hosts[0]["lastSeenAt"], "never seen")
	assert.Empty(t, runtime.Runtime.Online)

	var me map[string]string
	signedIn := []map[string]any{{"runtime": "claude_code", "state": "signed_in", "subscription": "max"}}
	require.Equal(t, 200, call(t, "POST", rest+"/agent-host/hello", paired.Token, map[string]any{"runtimes": []string{"claude_code"}, "version": "0.1.0", "accounts": signedIn}, &me))
	assert.Equal(t, map[string]string{"host_id": paired.HostID, "name": "Ada's Mac"}, me)
	assert.Equal(t, 400, call(t, "POST", rest+"/agent-host/hello", paired.Token, map[string]any{"runtimes": []string{"api"}}, &refused))
	assert.Equal(t, 400, call(t, "POST", rest+"/agent-host/hello", paired.Token, map[string]any{"runtimes": []string{"claude_code"}, "version": "0.1.0",
		"accounts": []map[string]any{{"runtime": "codex", "state": "signed_in"}}}, &refused), "an account is a runtime's it offers")
	assert.Equal(t, "invalid_arguments", refused.Error.Code)

	// The site token doesn't claim, and a helper's token doesn't reach the API.
	assert.Equal(t, 401, call(t, "POST", rest+"/agent-host/claim", siteToken, map[string]any{"runtimes": []string{"claude_code"}}, &refused))
	assert.Equal(t, 401, call(t, "GET", rest+"/v1/whoami", paired.Token, nil, nil))
	status, _ := hostMCP(t, rest, paired.Token, "tools/list", map[string]any{})
	assert.Equal(t, 401, status, "a helper's token calls no tool")

	// The workspace's new threads run on the helper.
	require.Equal(t, 200, call(t, "PATCH", agentURL+"/runtime", siteToken, map[string]any{"runtime": "claude_code"}, &runtime))
	assert.Equal(t, "claude_code", runtime.Runtime.Runtime)
	assert.Equal(t, []string{"claude_code"}, runtime.Runtime.Online)
	require.Len(t, runtime.Runtime.Hosts, 1)
	assert.Equal(t, "Ada's Mac", runtime.Runtime.Hosts[0]["name"])
	assert.Equal(t, []any{map[string]any{"runtime": "claude_code", "state": "signed_in", "subscription": "max"}}, runtime.Runtime.Hosts[0]["accounts"],
		"the owner sees what its CLI is signed in to")

	var sent struct {
		Turn   map[string]any
		Thread map[string]any
	}
	require.Equal(t, 200, call(t, "POST", agentURL+"/turns", siteToken, map[string]any{"text": "Which ontologies do I have?", "clientToken": "c-1"}, &sent))
	assert.Equal(t, "claude_code", sent.Thread["runtime"])
	turnID, threadID := sent.Turn["id"].(string), sent.Thread["id"].(string)

	var claimed struct {
		Turn struct {
			ID       string `json:"id"`
			ClaimID  string `json:"claim_id"`
			Runtime  string `json:"runtime"`
			MaxSteps int    `json:"max_steps"`
			MCPToken string `json:"mcp_token"`
			System   string `json:"system"`
			Prompt   string `json:"prompt"`
		}
	}
	claim := map[string]any{"runtimes": []string{"claude_code"}, "version": "0.1.0", "accounts": signedIn}
	require.Equal(t, 200, call(t, "POST", rest+"/agent-host/claim", paired.Token, claim, &claimed))
	assert.Equal(t, turnID, claimed.Turn.ID)
	assert.Equal(t, "claude_code", claimed.Turn.Runtime)
	assert.Equal(t, 20, claimed.Turn.MaxSteps)
	assert.Contains(t, claimed.Turn.System, workspaceID)
	assert.Equal(t, "Which ontologies do I have?", claimed.Turn.Prompt)
	turnToken := claimed.Turn.MCPToken
	require.True(t, strings.HasPrefix(turnToken, "hxr_"))

	// The turn's token calls the Assistant's tools, and nothing else.
	status, listed := hostMCP(t, rest, turnToken, "tools/list", map[string]any{})
	require.Equal(t, 200, status, listed)
	names := map[string]bool{}
	for _, tool := range listed["result"].(map[string]any)["tools"].([]any) {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, name := range []string{"helix_foundry_list_ontologies", "helix_foundry_prepare_operation", "helix_foundry_get_assistant_runtime", "helix_list_databases"} {
		assert.True(t, names[name], name)
	}
	for _, name := range []string{"helix_foundry_execute_operation", "helix_foundry_complete_document_upload", "helix_list_workspaces", "helix_foundry_cancel_question"} {
		assert.False(t, names[name], name)
	}
	status, called := hostMCP(t, rest, turnToken, "tools/call", map[string]any{"name": "helix_foundry_list_ontologies",
		"arguments": map[string]any{"workspace_id": workspaceID}})
	require.Equal(t, 200, status, called)
	assert.Nil(t, called["error"], called)
	// A prepare is a card for the person: the CLI never holds a confirmation.
	status, prepared := hostMCP(t, rest, turnToken, "tools/call", map[string]any{"name": "helix_foundry_prepare_operation",
		"arguments": map[string]any{"operation": "set_assistant_runtime", "target": "workspace:" + workspaceID, "payload": map[string]any{"runtime": "api"}}})
	require.Equal(t, 200, status, prepared)
	raw, _ := json.Marshal(prepared)
	assert.Contains(t, string(raw), "awaiting_person")
	assert.NotContains(t, string(raw), "hlxc_")
	assert.Equal(t, 403, call(t, "GET", rest+"/v1/ontologies/x/questions/y", turnToken, nil, nil), "a turn's token reaches /mcp alone")
	assert.Equal(t, 401, call(t, "GET", rest+"/v1/whoami", turnToken, nil, nil))
	status, _ = hostMCP(t, rest, "hxr_guess", "tools/list", map[string]any{})
	assert.Equal(t, 401, status)

	// A claim is named by its id: one malformed is refused, one the helper
	// doesn't hold (another claim's, or one that lapsed) is lost.
	assert.Regexp(t, `^[0-9a-f]{16}$`, claimed.Turn.ClaimID)
	assert.Equal(t, 400, call(t, "POST", rest+"/agent-host/claims/"+turnID+"/renew", paired.Token, map[string]any{}, &refused))
	assert.Equal(t, 410, call(t, "POST", rest+"/agent-host/claims/0123456789abcdef/renew", paired.Token, map[string]any{}, &refused))
	assert.Equal(t, "turn_lost", refused.Error.Code)

	// What the CLI's model writes is shown, then the turn ends.
	turnURL := rest + "/agent-host/claims/" + claimed.Turn.ClaimID
	assert.Equal(t, 204, call(t, "POST", turnURL+"/text", paired.Token, map[string]any{"step": 1, "text": "You have"}, nil))
	assert.Equal(t, 204, call(t, "POST", turnURL+"/text", paired.Token, map[string]any{"step": 1, "text": "You have none yet.", "final": true}, nil))
	assert.Equal(t, 400, call(t, "POST", turnURL+"/text", paired.Token, map[string]any{"step": 99, "text": "x"}, &refused))
	var renewed map[string]any
	assert.Equal(t, 200, call(t, "POST", turnURL+"/renew", paired.Token, map[string]any{}, &renewed))
	assert.NotEmpty(t, renewed["lease_expires_at"])
	assert.Equal(t, 400, call(t, "POST", turnURL+"/finish", paired.Token, map[string]any{"end": "stopped", "reason": "budget"}, &refused))
	assert.Equal(t, 204, call(t, "POST", turnURL+"/finish", paired.Token, map[string]any{"end": "answered", "steps": 1, "session_id": "session-1"}, nil))
	assert.Equal(t, 410, call(t, "POST", turnURL+"/text", paired.Token, map[string]any{"step": 1, "text": "late"}, &refused))
	assert.Equal(t, "turn_lost", refused.Error.Code)
	status, _ = hostMCP(t, rest, turnToken, "tools/list", map[string]any{})
	assert.Equal(t, 401, status, "a finished turn's token stops working")

	var events struct {
		Status string
		Events []struct{ EventJSON string }
	}
	require.Equal(t, 200, call(t, "GET", agentURL+"/turns/"+turnID+"/events", siteToken, nil, &events))
	assert.Equal(t, "done", events.Status)
	var kinds []string
	for _, e := range events.Events {
		var head struct{ Type string }
		require.NoError(t, json.Unmarshal([]byte(e.EventJSON), &head))
		kinds = append(kinds, head.Type)
	}
	assert.Equal(t, []string{"started", "tool_call", "tool_result", "tool_call", "action_card", "tool_result", "text", "finished"}, kinds)

	// The helper asks which threads it keeps sessions of are gone: a deleted
	// one is, and so is one that never was.
	var gone struct{ Gone []string }
	asked := map[string]any{"threads": []map[string]any{{"workspace_id": workspaceID, "thread_id": threadID},
		{"workspace_id": workspaceID, "thread_id": "01JZZZZZZZZZZZZZZZZZZZZZZZ"}}}
	require.Equal(t, 200, call(t, "POST", rest+"/agent-host/threads/gone", paired.Token, asked, &gone))
	assert.Equal(t, []string{"01JZZZZZZZZZZZZZZZZZZZZZZZ"}, gone.Gone)
	assert.Equal(t, 400, call(t, "POST", rest+"/agent-host/threads/gone", paired.Token,
		map[string]any{"threads": []map[string]any{{"workspace_id": "../x", "thread_id": threadID}}}, &refused))
	tooMany := make([]map[string]any, 101)
	for i := range tooMany {
		tooMany[i] = map[string]any{"workspace_id": workspaceID, "thread_id": threadID}
	}
	assert.Equal(t, 400, call(t, "POST", rest+"/agent-host/threads/gone", paired.Token, map[string]any{"threads": tooMany}, &refused))
	assert.Equal(t, 401, call(t, "POST", rest+"/agent-host/threads/gone", siteToken, asked, &refused), "a helper's route takes its token alone")
	require.Equal(t, 200, call(t, "DELETE", agentURL+"/threads/"+threadID, siteToken, nil, nil))
	require.Equal(t, 200, call(t, "POST", rest+"/agent-host/threads/gone", paired.Token, asked, &gone))
	assert.Equal(t, []string{threadID, "01JZZZZZZZZZZZZZZZZZZZZZZZ"}, gone.Gone, "a deleted thread is gone")

	// Revoked, the helper claims nothing, and its threads wait for another.
	var revoked map[string]any
	require.Equal(t, 200, call(t, "POST", agentURL+"/hosts/"+paired.HostID+":revoke", siteToken, map[string]any{}, &revoked))
	assert.Equal(t, 401, call(t, "POST", rest+"/agent-host/claim", paired.Token, claim, &refused))
	assert.Equal(t, "unauthenticated", refused.Error.Code)
	var offline map[string]any
	assert.Equal(t, 400, call(t, "POST", agentURL+"/turns", siteToken, map[string]any{"text": "And now?", "clientToken": "c-2"}, &offline))
	raw, _ = json.Marshal(offline)
	assert.Contains(t, string(raw), "FOUNDRY_AGENT_HOST_OFFLINE")
	assert.Equal(t, 401, call(t, "POST", rest+"/agent-host/threads/gone", paired.Token, asked, &refused))
}

// A helper revoked while its claim waits for a turn is refused, never handed
// one: the person's message goes to a helper still paired.
func TestFrontRefusesARevokedHelpersWaitingClaim(t *testing.T) {
	rest, _, _, _ := startFront(t, "http://helix:8080")
	var ws map[string]any
	require.Equal(t, 200, call(t, "POST", rest+"/v1/workspaces", siteToken, map[string]any{"slug": "revoke-wait", "displayName": "Revoke", "region": "local"}, &ws))
	agentURL := rest + "/v1/workspaces/" + ws["id"].(string) + "/foundry/agent"
	pairOne := func(name string) (string, string) {
		var pairing struct{ Code string }
		require.Equal(t, 200, call(t, "POST", agentURL+"/host-pairings", siteToken, map[string]any{}, &pairing))
		var paired struct {
			HostID string `json:"host_id"`
			Token  string `json:"token"`
		}
		require.Equal(t, 200, call(t, "POST", rest+"/agent-host/pair", "", map[string]any{"code": pairing.Code, "name": name,
			"runtimes": []string{"claude_code"}, "version": "0.1.0", "platform": "linux-x64"}, &paired))
		return paired.HostID, paired.Token
	}
	lostID, lost := pairOne("Lost laptop")
	_, kept := pairOne("Desk")
	claim := map[string]any{"runtimes": []string{"claude_code"}, "version": "0.1.0", "accounts": []any{}}
	require.Equal(t, 200, call(t, "POST", rest+"/agent-host/hello", kept, claim, nil))

	type answer struct {
		status int
		body   string
		err    error
	}
	waited := make(chan answer, 1)
	body, err := json.Marshal(claim)
	require.NoError(t, err)
	go func() {
		req, err := http.NewRequest("POST", rest+"/agent-host/claim", bytes.NewReader(body))
		if err != nil {
			waited <- answer{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+lost)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			waited <- answer{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		waited <- answer{resp.StatusCode, string(raw), err}
	}()
	time.Sleep(300 * time.Millisecond) // its claim is waiting
	require.Equal(t, 200, call(t, "POST", agentURL+"/hosts/"+lostID+":revoke", siteToken, map[string]any{}, nil))
	require.Equal(t, 200, call(t, "PATCH", agentURL+"/runtime", siteToken, map[string]any{"runtime": "claude_code"}, nil))
	var sent struct{ Turn map[string]any }
	require.Equal(t, 200, call(t, "POST", agentURL+"/turns", siteToken, map[string]any{"text": "Secret plans", "clientToken": "c-1"}, &sent))
	select {
	case got := <-waited:
		require.NoError(t, got.err)
		assert.Equal(t, 401, got.status, "the revoked helper's waiting claim is refused")
		assert.Contains(t, got.body, "unauthenticated")
		assert.NotContains(t, got.body, "Secret plans", "and is told nothing of the turn")
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked helper's claim kept waiting")
	}
	var claimed struct{ Turn struct{ ID string } }
	require.Equal(t, 200, call(t, "POST", rest+"/agent-host/claim", kept, claim, &claimed))
	assert.Equal(t, sent.Turn["id"], claimed.Turn.ID, "the paired helper takes it")
}
