package agent

import (
	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// keepMessages is how many messages a thread's transcript keeps.
const keepMessages = 200

// earlierResult stands in for a tool's result from an earlier turn: the model
// calls the tool again if it still needs it.
const earlierResult = "[An earlier result, left out to save room: call the tool again if you need it.]"

// notRun answers a tool call whose attempt stopped before running it.
const notRun = "[Not run: the turn stopped before this call ran.]"

// entry is one transcript message and the turn that wrote it.
type entry struct {
	turnID string
	msg    llm.Message
}

// entries reads a thread's stored transcript.
func entries(stored []model.FoundryAgentMessage) ([]entry, error) {
	out := make([]entry, 0, len(stored))
	for _, m := range stored {
		blocks, err := llm.UnmarshalBlocks(m.Content)
		if err != nil {
			return nil, err
		}
		out = append(out, entry{turnID: m.TurnID, msg: llm.Message{Role: llm.Role(m.Role), Content: blocks}})
	}
	return out, nil
}

// estTokens is about how many tokens a message costs: four bytes a token.
func estTokens(m llm.Message) int {
	raw, _ := llm.MarshalBlocks(m.Content) // known blocks only: it can't fail
	return len(raw)/4 + 1
}

// toolUses are a message's tool calls.
func toolUses(m llm.Message) []llm.ToolUse {
	var out []llm.ToolUse
	for _, b := range m.Content {
		if u, ok := b.(llm.ToolUse); ok {
			out = append(out, u)
		}
	}
	return out
}

// answers reports whether m answers every call of uses.
func answers(m llm.Message, uses []llm.ToolUse) bool {
	if m.Role != llm.User {
		return false
	}
	answered := map[string]bool{}
	for _, b := range m.Content {
		if r, ok := b.(llm.ToolResult); ok {
			answered[r.ToolUseID] = true
		}
	}
	for _, u := range uses {
		if !answered[u.ID] {
			return false
		}
	}
	return true
}

// prompt is the transcript a call of turnID sends, within about budget tokens:
//  1. it starts at a person's message (the store keeps the newest messages
//     only, so the oldest kept may be a tool's result);
//  2. earlier turns keep their text and calls but not their results, which
//     stand in as earlierResult, nor their reasoning, which only the turn
//     that wrote it sends back;
//  3. a call an earlier attempt or turn left unanswered is answered notRun,
//     unless it ends the transcript: the loop runs that one;
//  4. past budget, the oldest turns are left out, whole, but the first
//     message stays.
func prompt(all []entry, turnID string, budget int) []llm.Message {
	start := 0
	for start < len(all) && !(all[start].msg.Role == llm.User && len(toolUses(all[start].msg)) == 0 && !hasResults(all[start].msg)) {
		start++
	}
	var kept []entry
	for i := start; i < len(all); i++ {
		e := all[i]
		if e.turnID != turnID {
			e.msg = earlier(e.msg)
		}
		kept = append(kept, e)
		if uses := toolUses(e.msg); len(uses) > 0 && i+1 < len(all) && !answers(all[i+1].msg, uses) {
			results := make([]llm.Block, 0, len(uses))
			for _, u := range uses {
				results = append(results, llm.ToolResult{ToolUseID: u.ID, Content: notRun, IsError: true})
			}
			kept = append(kept, entry{turnID: e.turnID, msg: llm.Message{Role: llm.User, Content: results}})
		}
	}
	total := 0
	for _, e := range kept {
		total += estTokens(e.msg)
	}
	// Drop whole turns after the first message, oldest first, never the turn running.
	for total > budget && len(kept) > 1 && kept[1].turnID != turnID {
		drop := kept[1].turnID
		for len(kept) > 1 && kept[1].turnID == drop {
			total -= estTokens(kept[1].msg)
			kept = append(kept[:1], kept[2:]...)
		}
	}
	out := make([]llm.Message, 0, len(kept))
	for _, e := range kept {
		// An earlier turn's message of reasoning alone sends nothing.
		if len(e.msg.Content) > 0 {
			out = append(out, e.msg)
		}
	}
	return out
}

func hasResults(m llm.Message) bool {
	for _, b := range m.Content {
		if _, ok := b.(llm.ToolResult); ok {
			return true
		}
	}
	return false
}

// earlier is an earlier turn's message as later turns send it.
func earlier(m llm.Message) llm.Message {
	out := llm.Message{Role: m.Role, Content: make([]llm.Block, 0, len(m.Content))}
	for _, b := range m.Content {
		switch b := b.(type) {
		case llm.Reasoning:
		case llm.ToolResult:
			out.Content = append(out.Content, llm.ToolResult{ToolUseID: b.ToolUseID, Content: earlierResult, IsError: b.IsError})
		default:
			out.Content = append(out.Content, b)
		}
	}
	return out
}
