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
// Claude Code that calls Foundry's tools with the turn's token; then, once
// the owner confirmed Codex, a turn on a stand-in for Codex's app-server
// that does the same. It runs
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
		"FAKE_CLAUDE_LOG="+filepath.Join(dir, "claude.log"), "FAKE_CLAUDE_SCENARIO=answer",
		"FAKE_CODEX_LOG="+filepath.Join(dir, "codex.log"), "FAKE_CODEX_THREADS="+filepath.Join(dir, "codex-threads.json"), "FAKE_CODEX_SCENARIO=answer")
	cli := filepath.Join(helper, "dist", "cli.js")
	fakeClaude, fakeCodex := filepath.Join(helper, "test", "fake-claude.mjs"), filepath.Join(helper, "test", "fake-codex.mjs")
	pair := exec.Command("node", cli, "pair", pairing.Code, "--foundry", rest, "--name", "Test machine", "--claude", fakeClaude, "--codex", fakeCodex)
	pair.Env = env
	out, err := pair.CombinedOutput()
	require.NoError(t, err, string(out))

	require.Equal(t, 200, call(t, "PATCH", agentURL+"/runtime", siteToken, map[string]any{"runtime": "claude_code"}, nil))
	var runtime struct {
		Runtime struct {
			Online []string
			Hosts  []map[string]any
		}
	}
	require.Equal(t, 200, call(t, "GET", agentURL+"/runtime", siteToken, nil, &runtime))
	assert.Empty(t, runtime.Runtime.Online, "paired, it runs nothing until started")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := exec.CommandContext(ctx, "node", cli, "run", "--claude", fakeClaude, "--codex", fakeCodex)
	run.Env = env
	var logs strings.Builder
	run.Stdout, run.Stderr = &logs, &logs
	require.NoError(t, run.Start())
	defer func() { cancel(); _ = run.Wait() }()
	for deadline := time.Now().Add(30 * time.Second); len(runtime.Runtime.Online) == 0 && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		require.Equal(t, 200, call(t, "GET", agentURL+"/runtime", siteToken, nil, &runtime))
	}
	require.Equal(t, []string{"claude_code", "codex"}, runtime.Runtime.Online, logs.String())

	// A turn's events, once it ended: their types, and the last text.
	ask := func(token string) (thread string, kinds []string, said string) {
		var sent struct{ Turn map[string]any }
		require.Equal(t, 200, call(t, "POST", agentURL+"/turns", siteToken, map[string]any{"text": "Which ontologies do I have?", "clientToken": token}, &sent))
		turnID := sent.Turn["id"].(string)
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
		for _, e := range events.Events {
			var head struct{ Type, Text, Name string }
			require.NoError(t, json.Unmarshal([]byte(e.EventJSON), &head))
			kinds = append(kinds, head.Type)
			if head.Type == "text" {
				said = head.Text
			}
		}
		return sent.Turn["threadId"].(string), kinds, said
	}
	threadRuntime := func(id string) any {
		var thread struct{ Thread map[string]any }
		require.Equal(t, 200, call(t, "GET", agentURL+"/threads/"+id, siteToken, nil, &thread))
		return thread.Thread["runtime"]
	}

	thread, kinds, said := ask("c-1")
	// Foundry, not the helper, shows the tool call its CLI made.
	assert.Equal(t, []string{"started", "tool_call", "tool_result", "text", "finished"}, kinds)
	assert.Equal(t, "You have Sales graph.", said)
	assert.Equal(t, "claude_code", threadRuntime(thread))

	// Codex, once its owner confirmed what it runs with.
	var refused map[string]any
	assert.Equal(t, 400, call(t, "PATCH", agentURL+"/runtime", siteToken, map[string]any{"runtime": "codex"}, &refused), "codex isn't chosen unconfirmed")
	require.Equal(t, 200, call(t, "PATCH", agentURL+"/runtime", siteToken, map[string]any{"runtime": "codex", "confirmCodex": true}, nil))
	thread, kinds, said = ask("c-2")
	// Codex doesn't wait for a step's text to be shown before its tool call
	// runs, so the two may land either way round.
	assert.ElementsMatch(t, []string{"started", "text", "tool_call", "tool_result", "text", "finished"}, kinds)
	assert.Equal(t, "You have one ontology: Sales graph.", said)
	assert.Equal(t, "codex", threadRuntime(thread))

	// The owner sees what its CLIs are signed in to, as it said.
	require.Equal(t, 200, call(t, "GET", agentURL+"/runtime", siteToken, nil, &runtime))
	require.Len(t, runtime.Runtime.Hosts, 1)
	assert.Equal(t, []any{
		map[string]any{"runtime": "claude_code", "state": "signed_in", "subscription": "max", "version": "", "outdated": false},
		map[string]any{"runtime": "codex", "state": "signed_in", "subscription": "plus", "version": "0.169.0", "outdated": false},
	}, runtime.Runtime.Hosts[0]["accounts"])
}
