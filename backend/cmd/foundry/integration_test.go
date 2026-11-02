package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func localMCPCall(t *testing.T, base, method string, params any) (map[string]any, bool) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(t, err)
	req, err := http.NewRequest("POST", base+"/mcp", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+siteToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var envelope struct {
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	require.Nil(t, envelope.Error, string(raw))
	if method != "tools/call" {
		return envelope.Result, false
	}
	failed, _ := envelope.Result["isError"].(bool)
	if failed {
		return envelope.Result, true
	}
	result, ok := envelope.Result["structuredContent"].(map[string]any)
	require.True(t, ok, string(raw))
	return result, false
}

func TestFrontAgentIntegration(t *testing.T) {
	rest, _, _, _ := startFront(t, "http://helix:8080")
	for _, path := range []string{"/mcp", "/v1/ontologies/unknown/questions/unknown"} {
		assert.Equal(t, http.StatusUnauthorized, call(t, "GET", rest+path, "", nil, nil))
	}
	var ws map[string]any
	require.Equal(t, 200, call(t, "POST", rest+"/v1/workspaces", siteToken, map[string]any{"slug": "mcp-test", "displayName": "MCP test", "region": "local"}, &ws))
	workspaceID := ws["id"].(string)
	_, failed := localMCPCall(t, rest, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}})
	require.False(t, failed)
	listed, _ := localMCPCall(t, rest, "tools/list", map[string]any{})
	names := map[string]bool{}
	for _, tool := range listed["tools"].([]any) {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, name := range []string{"helix_list_workspaces", "helix_list_projects", "helix_list_databases", "helix_foundry_ask", "helix_foundry_read_graph", "helix_foundry_get_question", "helix_prepare_foundry_operation", "helix_execute_foundry_operation", "helix_foundry_complete_document_upload"} {
		require.True(t, names[name], name)
	}
	require.False(t, names["helix_foundry_send_pieces"])
	tool := func(name string, args map[string]any) (map[string]any, bool) {
		return localMCPCall(t, rest, "tools/call", map[string]any{"name": name, "arguments": args})
	}
	workspaces, failed := tool("helix_list_workspaces", map[string]any{})
	require.False(t, failed)
	require.Len(t, workspaces["workspaces"], 1)
	kinds, failed := tool("helix_foundry_list_connector_kinds", map[string]any{"workspace_id": workspaceID})
	require.False(t, failed)
	for _, kind := range kinds["kinds"].([]any) {
		require.NotEqual(t, "events", kind.(map[string]any)["kind"])
	}
	operation := map[string]any{"operation": "create_ontology", "target": "workspace:" + workspaceID, "payload": map[string]any{"name": "MCP ontology", "database": map[string]any{"name": "MCP database", "slug": "mcp-database", "project_name": "MCP project"}}}
	confirmation, failed := tool("helix_prepare_foundry_operation", operation)
	require.False(t, failed, confirmation)
	operation["confirmation_id"] = confirmation["confirmation_id"]
	operation["confirmation_token"] = confirmation["confirmation_token"]
	created, failed := tool("helix_execute_foundry_operation", operation)
	require.False(t, failed, created)
	ontologyID := created["result"].(map[string]any)["id"].(string)
	_, failed = tool("helix_execute_foundry_operation", operation)
	require.True(t, failed, "confirmation must be one-use")
	ontology, failed := tool("helix_foundry_get_ontology", map[string]any{"workspace_id": workspaceID, "ontology_id": ontologyID})
	require.False(t, failed, ontology)
	setOperation := map[string]any{"operation": "create_document_set", "target": "ontology:" + workspaceID + "/" + ontologyID, "payload": map[string]any{"name": "MCP documents", "description": "Uploaded through an agent"}}
	confirmation, failed = tool("helix_prepare_foundry_operation", setOperation)
	require.False(t, failed, confirmation)
	setOperation["confirmation_id"] = confirmation["confirmation_id"]
	setOperation["confirmation_token"] = confirmation["confirmation_token"]
	set, failed := tool("helix_execute_foundry_operation", setOperation)
	require.False(t, failed, set)
	_, failed = tool("helix_foundry_list_documents", map[string]any{"workspace_id": workspaceID, "ontology_id": ontologyID})
	require.False(t, failed)
	// No provider is configured in this fresh installation: HTTP must surface
	// the same actionable prerequisite refusal as MCP, rather than queue work.
	req, err := http.NewRequest("POST", rest+"/v1/ontologies/"+ontologyID+"/ask", bytes.NewBufferString(`{"question":"What is here?"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+siteToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "front-test-question")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 412, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "failed_precondition")
	// The new interfaces reject DNS-rebinding hosts even with a valid token.
	req, err = http.NewRequest("GET", rest+"/mcp", nil)
	require.NoError(t, err)
	req.Host = "attacker.example"
	req.Header.Set("Authorization", "Bearer "+siteToken)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 403, resp.StatusCode)
}
