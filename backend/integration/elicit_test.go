package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prepareSync prepares sync_connector on connector-1 and returns execute's arguments.
func prepareSync(t *testing.T, w *wire) map[string]any {
	t.Helper()
	args := map[string]any{"operation": "sync_connector", "target": "connector:workspace-1/connector-1", "payload": map[string]any{}}
	prepared := w.tool(toolPrepareFoundry, args, nil)["structuredContent"].(map[string]any)
	require.NotEmpty(t, prepared["confirmation_id"])
	args["confirmation_id"], args["confirmation_token"] = prepared["confirmation_id"], prepared["confirmation_token"]
	return args
}

// A client that asks its person through forms gets execute's ask in place of the action: nothing
// runs, nor is the confirmation spent, until the client calls again with the person's yes.
func TestExecuteAsksThePersonFirst(t *testing.T) {
	b := newJobsBackend()
	w := newWire(t, jobsService(b), jobsIdentity(), formClient)
	args := prepareSync(t, w)

	ask := w.tool(toolExecuteFoundry, args, nil)
	assert.Equal(t, "input_required", ask["resultType"])
	assert.Nil(t, ask["content"])
	request := ask["inputRequests"].(map[string]any)[confirmInput].(map[string]any)
	assert.Equal(t, "elicitation/create", request["method"])
	params := request["params"].(map[string]any)
	assert.Equal(t, "form", params["mode"])
	assert.Equal(t, "Run the Helix Foundry action sync_connector on connector:workspace-1/connector-1?", params["message"])
	assert.Equal(t, map[string]any{"type": "object", "properties": map[string]any{}}, params["requestedSchema"])
	assert.Equal(t, 0, b.synced)

	declined := w.tool(toolExecuteFoundry, args, map[string]any{confirmInput: map[string]any{"action": "decline"}})
	assert.Equal(t, true, declined["isError"])
	assert.Contains(t, declined["content"].([]any)[0].(map[string]any)["text"], "declined")
	cancelled := w.tool(toolExecuteFoundry, args, map[string]any{confirmInput: map[string]any{"action": "cancel"}})
	assert.Equal(t, true, cancelled["isError"])
	assert.Equal(t, 0, b.synced)

	ran := w.tool(toolExecuteFoundry, args, map[string]any{confirmInput: map[string]any{"action": "accept", "content": map[string]any{}}})
	assert.NotEqual(t, true, ran["isError"], ran)
	assert.Equal(t, "sync_connector", ran["structuredContent"].(map[string]any)["operation"])
	assert.Equal(t, 1, b.synced)

	again := w.tool(toolExecuteFoundry, args, map[string]any{confirmInput: map[string]any{"action": "accept", "content": map[string]any{}}})
	assert.Equal(t, true, again["isError"], "a yes doesn't spend a confirmation twice")
	assert.Equal(t, 1, b.synced)
}

// Execute asks before it validates, as nothing runs on the ask: an action refused after the
// person's yes runs nothing and spends nothing.
func TestExecuteRefusesAfterTheYesAndRunsNothing(t *testing.T) {
	b := newJobsBackend()
	w := newWire(t, jobsService(b), jobsIdentity(), formClient)
	args := prepareSync(t, w)
	args["target"] = "connector:workspace-2/connector-1"
	assert.Equal(t, "input_required", w.tool(toolExecuteFoundry, args, nil)["resultType"])
	refused := w.tool(toolExecuteFoundry, args, map[string]any{confirmInput: map[string]any{"action": "accept", "content": map[string]any{}}})
	assert.Equal(t, true, refused["isError"])
	assert.Equal(t, 0, b.synced)
}

// What the person answered reaches the action only through the call that carries it: a call whose
// client asks, but which carries no answer, never runs (the ask always comes first).
func TestPersonAnswerNeedsTheAnswer(t *testing.T) {
	request := func(responses mcp.InputResponseMap) context.Context {
		return context.WithValue(context.Background(), toolRequestKey{}, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
			InputResponses: responses,
			Meta:           mcp.Meta{mcp.MetaKeyProtocolVersion: "2026-07-28", mcp.MetaKeyClientCapabilities: formClient},
		}})
	}
	assert.ErrorIs(t, personAnswer(request(nil)), errNotAsked)
	assert.ErrorIs(t, personAnswer(request(mcp.InputResponseMap{confirmInput: &mcp.ElicitResult{Action: "decline"}})), ErrDeclined)
	assert.NoError(t, personAnswer(request(mcp.InputResponseMap{confirmInput: &mcp.ElicitResult{Action: "accept"}})))
	assert.NoError(t, personAnswer(context.Background()), "an action a person runs with a click is no tool call")
}

// A client that can't ask through a form (none, URL only, or an older protocol) runs on the
// agent's word, as before.
func TestExecuteRunsAtOnceForAClientThatCantAsk(t *testing.T) {
	for name, client := range map[string]struct {
		caps    map[string]any
		version string
	}{
		"no elicitation": {map[string]any{}, "2026-07-28"},
		"URL only":       {map[string]any{"elicitation": map[string]any{"url": map[string]any{}}}, "2026-07-28"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newJobsBackend()
			w := newWire(t, jobsService(b), jobsIdentity(), client.caps)
			w.version = client.version
			ran := w.tool(toolExecuteFoundry, prepareSync(t, w), nil)
			assert.NotEqual(t, true, ran["isError"], ran)
			assert.NotContains(t, ran, "inputRequests")
			assert.Equal(t, 1, b.synced)
		})
	}
}

func TestPersonConfirmsAsksOnlyAFormClientOnTheNewProtocol(t *testing.T) {
	request := func(version string, caps map[string]any, responses mcp.InputResponseMap) *mcp.CallToolRequest {
		return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{InputResponses: responses, Meta: mcp.Meta{
			mcp.MetaKeyProtocolVersion: version, mcp.MetaKeyClientCapabilities: caps,
		}}}
	}
	bare := map[string]any{"elicitation": map[string]any{}}
	both := map[string]any{"elicitation": map[string]any{"form": map[string]any{}, "url": map[string]any{}}}
	for _, caps := range []map[string]any{bare, both, formClient} {
		ask, err := PersonConfirms(request("2026-07-28", caps, nil), "Run it?")
		require.NoError(t, err)
		require.NotNil(t, ask, caps)
	}
	for _, version := range []string{"2025-11-25", "2025-06-18"} {
		ask, err := PersonConfirms(request(version, formClient, nil), "Run it?")
		assert.Nil(t, ask, version)
		assert.NoError(t, err)
	}
	ask, err := PersonConfirms(request("2026-07-28", formClient, mcp.InputResponseMap{confirmInput: &mcp.ElicitResult{Action: "accept"}}), "Run it?")
	assert.Nil(t, ask)
	assert.NoError(t, err)
	_, err = PersonConfirms(request("2026-07-28", formClient, mcp.InputResponseMap{confirmInput: &mcp.ElicitResult{Action: "decline"}}), "Run it?")
	assert.ErrorIs(t, err, ErrDeclined)
	// An answer to another ask is no answer to this one.
	ask, _ = PersonConfirms(request("2026-07-28", formClient, mcp.InputResponseMap{"other": &mcp.ElicitResult{Action: "accept"}}), "Run it?")
	assert.NotNil(t, ask)

	long, _ := PersonConfirms(request("2026-07-28", formClient, nil), strings.Repeat("é", maxConfirmMessage+10))
	message := long.InputRequests[confirmInput].(*mcp.ElicitParams).Message
	assert.Equal(t, maxConfirmMessage, len([]rune(message)))
	assert.True(t, strings.HasSuffix(message, "…"))
}

func TestConfirmMessageShowsThePayload(t *testing.T) {
	assert.Equal(t, "Run it?", ConfirmMessage("Run it?", nil))
	assert.Equal(t, "Run it?", ConfirmMessage("Run it?", map[string]any{}))
	assert.Equal(t, "Run it?\n\n{\n  \"goal\": \"Customers\"\n}", ConfirmMessage("Run it?", map[string]any{"goal": "Customers"}))
	assert.Equal(t, "Run it?", ConfirmMessage("Run it?", func() {}), "a payload that isn't JSON is left out")
}
