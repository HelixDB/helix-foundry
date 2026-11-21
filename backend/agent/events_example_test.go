package agent_test

import (
	"encoding/json"
	"fmt"

	"github.com/helixdb/helix-foundry/backend/agent"
)

// A turn's stored events read back as the closed set, by value, for a
// reader to switch on.
func ExampleDecodeEvent() {
	for _, raw := range []string{
		`{"type":"started","model":"claude-sonnet-4-5","attempt":1}`,
		`{"type":"tool_call","tool_use_id":"toolu_1","name":"helix_foundry_ask","title":"Asked Sales graph","input":{}}`,
		`{"type":"text","step":1,"text":"Nine invoices are overdue."}`,
		`{"type":"finished","status":"done","reason":"answered"}`,
		`{"type":"thinking"}`,
	} {
		event, err := agent.DecodeEvent(json.RawMessage(raw))
		switch e := event.(type) {
		case agent.Started:
			fmt.Println("started on", e.Model)
		case agent.ToolCalled:
			fmt.Println("status:", e.Title)
		case agent.Said:
			fmt.Println("said:", e.Text)
		case agent.Finished:
			fmt.Println("finished:", e.Reason)
		default:
			fmt.Println("error:", err)
		}
	}
	// Output:
	// started on claude-sonnet-4-5
	// status: Asked Sales graph
	// said: Nine invoices are overdue.
	// finished: answered
	// error: agent: unknown event type "thinking"
}
