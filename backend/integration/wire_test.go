package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// wire is a client of s's tools as a host serves them, statelessly over streamable HTTP, that
// speaks protocol 2026-07-28 itself, so a test sees exactly what goes over the wire.
type wire struct {
	t       *testing.T
	handler http.Handler
	// caps are the client capabilities each request declares; version its protocol.
	caps    map[string]any
	version string
}

func newWire(t *testing.T, s *Service, identity *Identity, caps map[string]any) *wire {
	t.Helper()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		capabilities := &mcp.ServerCapabilities{}
		AddExtensions(capabilities)
		server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ServerOptions{Capabilities: capabilities})
		s.AddTools(server, identity)
		s.AddTasks(server, identity)
		return server
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return &wire{t: t, handler: handler, caps: caps, version: "2026-07-28"}
}

// tasksClient and formClient are the capabilities of a client that takes tasks, and of one that
// asks its person through forms.
var (
	tasksClient = map[string]any{"extensions": map[string]any{TasksExtension: map[string]any{}}}
	formClient  = map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}
)

// call sends one request and returns its result, or its JSON-RPC error as {"error": …}.
func (w *wire) call(method string, params map[string]any) map[string]any {
	w.t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		mcp.MetaKeyProtocolVersion:    w.version,
		mcp.MetaKeyClientInfo:         map[string]any{"name": "wire", "version": "1"},
		mcp.MetaKeyClientCapabilities: w.caps,
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(w.t, err)
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", w.version)
	r.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		r.Header.Set("Mcp-Name", name)
	}
	if uri, ok := params["uri"].(string); ok {
		r.Header.Set("Mcp-Name", uri)
	}
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, r)
	var response struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	require.NoError(w.t, json.Unmarshal(rec.Body.Bytes(), &response), rec.Body.String())
	if response.Error != nil {
		require.Equal(w.t, http.StatusBadRequest, rec.Code, rec.Body.String())
		return map[string]any{"error": response.Error}
	}
	require.Equal(w.t, http.StatusOK, rec.Code, rec.Body.String())
	return response.Result
}

// tool calls name with arguments, and any inputResponses for a retry.
func (w *wire) tool(name string, arguments map[string]any, inputResponses map[string]any) map[string]any {
	w.t.Helper()
	params := map[string]any{"name": name, "arguments": arguments}
	if inputResponses != nil {
		params["inputResponses"] = inputResponses
	}
	return w.call("tools/call", params)
}
