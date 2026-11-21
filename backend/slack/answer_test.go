package slack_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const askUsage = `"usage":{"input":10,"cache_read":0,"cache_write":0,"output":5,"usd":0.03}`

// answeredResult is an answered result: blocks, citations, queries and
// visuals as JSON.
func answeredResult(blocks, citations, queries, visuals string) json.RawMessage {
	return json.RawMessage(`{"status":"answered","answer":{"blocks":` + blocks + `,"visuals":` + visuals + `},"citations":` + citations +
		`,"queries":` + queries + `,` + askUsage + `}`)
}

// asked is a question of askPage on the Sales ontology.
func asked(status model.FoundryQuestionStatus, result json.RawMessage) slack.Question {
	return slack.Question{QuestionPage: askPage, OntologyName: "Sales", Status: status, Result: result}
}

func TestAnswer(t *testing.T) {
	s := site(t)
	query := `{"start":{"type":"Invoice"},"return":{"count":{}}}`
	full := answeredResult(
		`[{"para":"There are 1234 invoices [c1], see the contract [c2] and acme.com [c9]."},`+
			`{"list":["Paid: 40 [c3]","Open: <b>3</b> [c4]","Ada's profile [c5]"]},{"para":"[c6]"},{"para":"The rest."}]`,
		`[{"marker":"c1","evidence_id":"e1","target":{"result":{"evidence_id":"e1"}}},`+
			`{"marker":"c2","evidence_id":"e2","target":{"quote":{"doc_id":"doc_1","section":1,"start":4,"end":9}}},`+
			`{"marker":"c3","evidence_id":"e3","target":{"entity":{"type":"Invoice","id":"inv_1"}}},`+
			`{"marker":"c4","evidence_id":"e3","target":{"violation":{"constraint":"paid","key":"k1"}}},`+
			`{"marker":"c5","evidence_id":"e3","target":{"row":{"source":"foundry_enrichment","mapping":"people","row_key":""}}},`+
			`{"marker":"c6","evidence_id":"e3","target":{"entity":{"type":"Invoice","id":"inv_2"}}}]`,
		`[{"evidence_id":"e1","query":`+query+`,"output":{"kind":"count","count":1234}},`+
			`{"evidence_id":"e2","query":`+query+`,"output":{"kind":"aggregate","value":1234567.891234,"values":40,"skipped":2,"cut":"only the first 40"}},`+
			`{"evidence_id":"e3","query":`+query+`,"output":{"kind":"group_count","groups":[{"value":{"untrusted":"paid"},"count":40}],"total_groups":1}},`+
			`{"evidence_id":"e4","query":`+query+`,"output":{"kind":"rows","rows":[],"total":0}},`+
			`{"evidence_id":"e5","query":`+query+`,"output":{"kind":"aggregate","value":null,"values":0}},`+
			`{"evidence_id":"e6","query":`+query+`}]`,
		`[{"evidence_id":"e1","after":0,"kind":"figure","title":"Invoices <all>"},`+
			`{"evidence_id":"e2","after":0,"title":"Total billed"},`+
			`{"evidence_id":"e3","after":1,"kind":"bar","title":"By status"},`+
			`{"evidence_id":"e4","kind":"table"}]`)
	noTitles := answeredResult(`[{"para":"Counts."}]`, `[]`,
		`[{"evidence_id":"e1","query":`+query+`,"output":{"kind":"count","count":7}},`+
			`{"evidence_id":"e5","query":`+query+`,"output":{"kind":"aggregate","value":null,"values":1}}]`,
		`[{"evidence_id":"e1"},{"evidence_id":"e5","kind":"line"}]`)
	long := answeredResult(`[`+strings.TrimSuffix(strings.Repeat(`{"para":"A paragraph."},`, 60), ",")+`]`, `[]`, `[]`, `[]`)
	golden(t, "answer", map[string][]slack.Message{
		"answered": s.Answer(asked(model.FoundryQuestionAnswered, full)),
		"answered without titles": s.Answer(slack.Question{QuestionPage: slack.QuestionPage{WorkspaceID: "ws_1", OntologyID: "ont_1"},
			Status: model.FoundryQuestionAnswered, Result: noTitles}),
		"long": s.Answer(asked(model.FoundryQuestionAnswered, long)),
		"cannot answer": s.Answer(asked(model.FoundryQuestionCannotAnswer, json.RawMessage(
			`{"status":"cannot_answer","reason":"No renewal dates.","gap":"Contracts need a renewal date.","queries":[],`+askUsage+`}`))),
		"cannot answer without a gap": s.Answer(asked(model.FoundryQuestionCannotAnswer, json.RawMessage(
			`{"status":"cannot_answer","reason":"No renewal dates.","gap":" ","queries":[],`+askUsage+`}`))),
		"budget exhausted": s.Answer(asked(model.FoundryQuestionFailed, json.RawMessage(`{"status":"budget_exhausted","queries":[],`+askUsage+`}`))),
		"failed with a result": s.Answer(asked(model.FoundryQuestionFailed, json.RawMessage(
			`{"status":"failed","reason":"model_rate_limited","queries":[],`+askUsage+`}`))),
		"failed with an error": s.Answer(slack.Question{QuestionPage: askPage, OntologyName: "Sales", Status: model.FoundryQuestionFailed,
			Error: "Helix's model account is out of credit", Result: json.RawMessage(`{"status":"failed","reason":"model_quota","queries":[],` + askUsage + `}`)}),
		"failed without a result": s.Answer(slack.Question{QuestionPage: askPage, OntologyName: "Sales", Status: model.FoundryQuestionFailed,
			Error: "the load was deleted"}),
		"failed without a word": s.Answer(asked(model.FoundryQuestionFailed, nil)),
		"cancelled":             s.Answer(asked(model.FoundryQuestionCancelled, nil)),
		"still running":         s.Answer(asked(model.FoundryQuestionRunning, nil)),
		"queued":                s.Answer(asked(model.FoundryQuestionQueued, nil)),
		"unreadable":            s.Answer(asked(model.FoundryQuestionAnswered, json.RawMessage(`{"status":"answered"}`))),
		"unnamed ontology":      s.Answer(slack.Question{QuestionPage: askPage, Status: model.FoundryQuestionCancelled}),
	})
	require.Len(t, s.Answer(asked(model.FoundryQuestionAnswered, long)), 2)
}

func TestFormatNumberFigures(t *testing.T) {
	s := site(t)
	for value, want := range map[string]string{
		"0": "0", "7": "7", "1234": "1,234", "-1234567": "-1,234,567", "12.5": "12.5", "0.123456": "0.1235",
		"-0.00001": "0", "1234.50001": "1,234.5", "1e20": "100,000,000,000,000,000,000", "-999.99999": "-1,000", "1e15": "1,000,000,000,000,000",
	} {
		result := answeredResult(`[{"para":"x"}]`, `[]`,
			`[{"evidence_id":"e1","query":{"start":{}},"output":{"kind":"aggregate","value":`+value+`,"values":1}}]`,
			`[{"evidence_id":"e1","title":"T"}]`)
		messages := s.Answer(asked(model.FoundryQuestionAnswered, result))
		section, ok := messages[0].Blocks[2].(slack.Section)
		require.True(t, ok, value)
		require.Equal(t, "*T*: "+want, section.Text.String(), value)
	}
}

func ExampleSite_Answer() {
	s, _ := slack.NewSite("https://foundry.helix-db.com")
	result := `{"status":"answered","answer":{"blocks":[{"para":"Globex renewed [c1]."}]},` +
		`"citations":[{"marker":"c1","evidence_id":"e1","target":{"quote":{"doc_id":"d1","section":0,"start":0,"end":6}}}],` +
		`"queries":[],"usage":{"input":1,"cache_read":0,"cache_write":0,"output":1,"usd":0}}`
	for _, m := range s.Answer(slack.Question{
		QuestionPage: slack.QuestionPage{WorkspaceID: "w1", OntologyID: "o1", LoadID: "l1", ThreadID: "q1"},
		OntologyName: "Sales", Status: model.FoundryQuestionAnswered, Result: json.RawMessage(result),
	}) {
		for _, b := range m.Blocks {
			switch b := b.(type) {
			case slack.Context:
				fmt.Println(b.Elements[0])
			case slack.Section:
				fmt.Println(b.Text)
			}
		}
	}
	// Output:
	// Answer from *Sales* · <https://foundry.helix-db.com/ask/o1?workspace=w1&amp;thread=q1|Open in Foundry>
	// Globex renewed <https://foundry.helix-db.com/ontology/o1/documents/d1?load=l1&amp;workspace=w1#s0-0-6|[1]>.
}
