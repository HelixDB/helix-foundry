package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The helper itself (agent-host/, built) against this front: it pairs with
// a code the owner made, claims a turn, and runs it on a stand-in for
// Claude Code that calls Foundry's tools with the turn's token. It runs
// with FOUNDRY_HELPER_E2E naming the helper's directory, built and with its
// dependencies installed (pnpm install && pnpm build there), and a test
// MySQL; without either it skips.
func TestFrontWithTheHelper(t *testing.T) {
	helper := os.Getenv("FOUNDRY_HELPER_E2E")
	if helper == "" {
		t.Skip("set FOUNDRY_HELPER_E2E to the built agent-host directory")
	}
	rest, _, _, _ := startFront(t, "http://helix:8080")
	var ws map[string]any
	require.Equal(t, 200, call(t, "POST", rest+"/v1/workspaces", siteToken, map[string]any{"slug": "helper-e2e", "displayName": "Helper", "region": "local"}, &ws))
	agentURL := rest + "/v1/workspaces/" + ws["id"].(string) + "/foundry/agent"
	var pairing struct{ Code string }
	require.Equal(t, 200, call(t, "POST", agentURL+"/host-pairings", siteToken, map[string]any{}, &pairing))

	dir := t.TempDir()
	env := append(os.Environ(), "FOUNDRY_AGENT_HOST_CONFIG="+filepath.Join(dir, "agent-host.json"), "FOUNDRY_AGENT_HOST_STATE="+filepath.Join(dir, "state"),
		"FAKE_CLAUDE_LOG="+filepath.Join(dir, "claude.log"), "FAKE_CLAUDE_SCENARIO=answer")
	cli := filepath.Join(helper, "dist", "cli.js")
	pair := exec.Command("node", cli, "pair", pairing.Code, "--foundry", rest, "--name", "Test machine")
	pair.Env = env
	out, err := pair.CombinedOutput()
	require.NoError(t, err, string(out))

	require.Equal(t, 200, call(t, "PATCH", agentURL+"/runtime", siteToken, map[string]any{"runtime": "claude_code"}, nil))
	var sent struct{ Turn map[string]any }
	require.Equal(t, 200, call(t, "POST", agentURL+"/turns", siteToken, map[string]any{"text": "Which ontologies do I have?", "clientToken": "c-1"}, &sent))
	turnID := sent.Turn["id"].(string)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := exec.CommandContext(ctx, "node", cli, "run", "--claude", filepath.Join(helper, "test", "fake-claude.mjs"))
	run.Env = env
	var logs strings.Builder
	run.Stdout, run.Stderr = &logs, &logs
	require.NoError(t, run.Start())
	defer func() { cancel(); _ = run.Wait() }()

	var events struct {
		Status string
		Events []struct{ EventJSON string }
	}
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		require.Equal(t, 200, call(t, "GET", agentURL+"/turns/"+turnID+"/events", siteToken, nil, &events))
		if events.Status == "done" || events.Status == "failed" {
			break
		}
	}
	require.Equal(t, "done", events.Status, logs.String())
	var kinds []string
	var said string
	for _, e := range events.Events {
		var head struct{ Type, Text, Name string }
		require.NoError(t, json.Unmarshal([]byte(e.EventJSON), &head))
		kinds = append(kinds, head.Type)
		if head.Type == "text" {
			said = head.Text
		}
	}
	// Foundry, not the helper, shows the tool call its CLI made.
	assert.Equal(t, []string{"started", "tool_call", "tool_result", "text", "finished"}, kinds)
	assert.Equal(t, "You have Sales graph.", said)
	var thread struct{ Thread map[string]any }
	require.Equal(t, 200, call(t, "GET", agentURL+"/threads/"+sent.Turn["threadId"].(string), siteToken, nil, &thread))
	assert.Equal(t, "claude_code", thread.Thread["runtime"])
}
