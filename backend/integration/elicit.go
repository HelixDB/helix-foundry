package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// confirmInput is the key of the one input a confirmation asks for.
const confirmInput = "confirm"

// maxConfirmMessage bounds what a confirmation shows, in runes.
const maxConfirmMessage = 4000

// ErrDeclined is a person's no to a confirmation in their client: nothing ran.
var ErrDeclined = detailedToolError{code: errCodeFailedPrecondition,
	message: "the person declined in their client, so nothing ran; the confirmation is still unspent until it expires"}

// PersonConfirms asks req's person, in their own client, to confirm a change message describes,
// before an execute tool spends its confirmation. A client asks only when it takes form
// elicitation on protocol 2026-07-28 or later, where the ask is the call's input-required result
// and the client calls again with the answer (multi round-trip). It returns:
//
//   - nil, nil when the person accepted, or the client can't ask (the agent's word stands, as
//     it did before clients could ask);
//   - ask, nil when the client is to ask: the call answers with ask, changing nothing;
//   - nil, ErrDeclined when the person declined or cancelled.
//
// An answer is the client's word, as the agent's was: it adds the person's look at the change,
// never a check a client could not skip.
func PersonConfirms(req *mcp.CallToolRequest, message string) (*mcp.CallToolResult, error) {
	if !clientAsks(req) {
		return nil, nil
	}
	if answer, ok := req.Params.InputResponses[confirmInput].(*mcp.ElicitResult); ok {
		if answer.Action == "accept" {
			return nil, nil
		}
		return nil, ErrDeclined
	}
	if utf8.RuneCountInString(message) > maxConfirmMessage {
		message = string([]rune(message)[:maxConfirmMessage-1]) + "…"
	}
	return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{confirmInput: &mcp.ElicitParams{
		Mode: "form", Message: message,
		RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}}}, nil
}

// clientAsks reports whether req's client asks its person through a form, on a protocol that
// carries the ask in the result: elicitation with form, or bare (which is form).
func clientAsks(req *mcp.CallToolRequest) bool {
	caps := req.ClientCapabilities()
	if req.ProtocolVersion() < "2026-07-28" || caps == nil || caps.Elicitation == nil {
		return false
	}
	return caps.Elicitation.Form != nil || caps.Elicitation.URL == nil
}

// ConfirmMessage is what a person confirms: the change, then its payload as JSON.
func ConfirmMessage(change string, payload any) string {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil || string(encoded) == "null" || string(encoded) == "{}" {
		return change
	}
	return fmt.Sprintf("%s\n\n%s", change, encoded)
}

// foundryCall is what a Foundry tool's handler may need of its call beyond its input: the
// request, whose client may ask its person (PersonConfirms), and where to put that ask.
type foundryCall struct {
	request *mcp.CallToolRequest
	ask     *mcp.CallToolResult
}

type foundryCallKey struct{}

// askPerson is PersonConfirms for a handler under callFoundryTool: an ask goes to the call,
// which answers with it, and the handler returns nothing (asked is true).
func askPerson(ctx context.Context, message string) (asked bool, err error) {
	call, _ := ctx.Value(foundryCallKey{}).(*foundryCall)
	if call == nil || call.request == nil || call.request.Params == nil {
		return false, nil
	}
	ask, err := PersonConfirms(call.request, message)
	call.ask = ask
	return ask != nil, err
}
