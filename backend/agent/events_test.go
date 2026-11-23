package agent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agentquestion"
)

// everyEvent is one of each event type, with every field set, written as
// MarshalEvent writes them back (raw JSON compact, its keys in order).
func everyEvent(t *testing.T) []Event {
	t.Helper()
	question, err := agentquestion.New("q1", "Ontology", "Which ontology?", true,
		[]agentquestion.Option{{Label: "Sales", Description: "The sales graph"}, {Label: "Contracts"}}, false, "Sales")
	require.NoError(t, err)
	free, err := agentquestion.New("q2", "", "What should it be called?", false, nil, false, "")
	require.NoError(t, err)
	yes, count := true, int64(9007199254740993)
	return []Event{
		Started{Model: "claude-sonnet-4-5", Attempt: 2},
		Said{Step: 3, Text: "You have *two* ontologies."},
		TextDelta{Step: 3, Text: "You have"},
		ToolCalled{ToolUseID: "toolu_1", Name: toolAsk, Title: "Asked Sales graph", Input: json.RawMessage(`{"ontology_id":"ont1","question":"Who?"}`)},
		ToolReturned{ToolUseID: "toolu_1", Name: toolAsk, Output: map[string]any{"id": "q1", "rows": json.Number("9007199254740993")},
			Question: &QuestionRef{OntologyID: "ont1", ID: "q1", LoadID: "load1", ThreadID: "q1", Status: "answered"}},
		ToolReturned{ToolUseID: "toolu_2", Name: "helix_foundry_list_ontologies", IsError: true, Error: "the API is down"},
		ToolReturned{ToolUseID: "toolu_3", Name: toolAsk, NotRun: NotRunAsks},
		ToolReturned{ToolUseID: "toolu_4", Name: "helix_foundry_read_graph", Truncated: true},
		ActionCard{ActionID: "act1", Operation: "create_load", Target: "ontology:ws1/ont1", Payload: json.RawMessage(`{"version_id":"ver3"}`),
			PayloadHash: "hash", Display: json.RawMessage(`{"title":"Load v3"}`), TargetNamedByUser: true,
			ExpiresAt: time.Date(2026, 11, 21, 12, 0, 0, 5, time.UTC), Auto: true},
		ActionResult{ActionID: "act1", Operation: "create_load", Status: "failed", Result: json.RawMessage(`{"load_id":"load2"}`), Error: "refused",
			Auto: true},
		Proposed{ActionID: "act2", Operation: "create_dashboard"},
		Finished{Status: "done", Reason: StopAnswered},
		Finished{Status: "failed", Reason: StopFailed, Error: "the model is down"},
		Reported{Summary: "Nine invoices are overdue.", Answer: &yes, Count: &count},
		Asked{Questions: []agentquestion.Question{question, free}, Generation: &AskedGeneration{OntologyID: "ont1", OntologyName: "Sales graph",
			GenerationID: "gen1", GenerationTitle: "Draft", Round: 2}},
		Answered{TurnID: "turn1", Answers: []agentquestion.Answer{{QuestionID: "q1", Options: []string{"Sales"}},
			{QuestionID: "q2", Options: []string{}, Text: "Renewals"}, {QuestionID: "q3", Left: true}}},
	}
}

func TestDecodeEventReadsBackWhatMarshalEventWrote(t *testing.T) {
	types := map[string]bool{}
	for _, e := range everyEvent(t) {
		raw, err := MarshalEvent(e)
		require.NoError(t, err)
		got, err := DecodeEvent(raw)
		require.NoError(t, err, string(raw))
		assert.Equal(t, e, got, "a %T reads back as itself, by value", e)
		types[e.eventType()] = true
	}
	assert.Len(t, decoders, len(types), "every type of the closed set reads back, and nothing else")
	for name := range decoders {
		assert.True(t, types[name], "%s is in the test", name)
	}
}

func TestDecodeEventKeepsANumberExact(t *testing.T) {
	got, err := DecodeEvent(json.RawMessage(`{"type":"tool_result","tool_use_id":"t","name":"n","is_error":false,"output":{"n":9007199254740993}}`))
	require.NoError(t, err)
	assert.Equal(t, json.Number("9007199254740993"), got.(ToolReturned).Output.(map[string]any)["n"])
}

func TestDecodeEventIgnoresFieldsItsTypeDoesntName(t *testing.T) {
	got, err := DecodeEvent(json.RawMessage(`{"type":"finished","status":"done","reason":"a_new_reason","new_field":[1]}`))
	require.NoError(t, err)
	assert.Equal(t, Finished{Status: "done", Reason: "a_new_reason"}, got, "a reason it doesn't know is kept as written")
}

func TestDecodeEventRefusesWhatIsntAnEvent(t *testing.T) {
	for name, raw := range map[string]string{
		"not JSON":                  `{"type":`,
		"not an object":             `["started"]`,
		"trailing data":             `{"type":"started"} {}`,
		"no type":                   `{"model":"m","attempt":1}`,
		"a null type":               `{"type":null}`,
		"a type that isn't a word":  `{"type":7}`,
		"an empty type":             `{"type":""}`,
		"an unknown type":           `{"type":"thinking"}`,
		"a field of the wrong kind": `{"type":"text","step":"three","text":"hi"}`,
		"a question that breaks its rules": `{"type":"questions","questions":[{"id":"q1","text":"Which?","blocking":true,` +
			`"options":[{"label":"Only one"}]}]}`,
		"a left answer with words": `{"type":"answers","turn_id":"t","answers":[{"question_id":"q1","left":true,"text":"no"}]}`,
	} {
		_, err := DecodeEvent(json.RawMessage(raw))
		assert.Error(t, err, name)
	}
}
