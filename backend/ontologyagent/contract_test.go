package ontologyagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
)

// testResult is a TurnResult the agent could send: status is its JSON, and an
// approved result carries a bundle. Its draft asks one question.
func testResult(status string, rev uint64) string {
	bundle := "null"
	if status == `"approved"` {
		bundle = `{"version":"2026-10-18.1","parent":null,"sha256":"abc","ontology_toml":"version = \"2026-10-18.1\"\n",
			"mappings_toml":"","review_json":"{}","impact":[{"item":"type.Customer","effect":"needs_remap"}],
			"agent_version":{"prompt":"p1","model":"claude-opus-5-5","effort":"high","tools":"t1"}}`
	}
	return fmt.Sprintf(`{"status":%s,"state":{"draft_rev":%d,"opaque":[1,2,3]},
		"draft":{"ontology_toml":"version = \"draft\"\n","mappings_toml":"ontology = \"draft\"\n","draft":{"rev":%d,
			"base_version":null,"items":{},"left_out":[],"acknowledged":[],"lint":[],
			"questions":[{"id":"q1","items":["type.Customer"],"text":"Is a customer an account?","options":[],"default":null,"blocking":false}]}},
		"bundle":%s,"summary":"built a draft","usage":{"input":1000,"cache_read":200,"cache_write":300,"output":400,"usd":1.25}}`,
		status, rev, rev, bundle)
}

// testEvents are a turn's events from start to finish.
var testEvents = []string{
	`{"event":"turn_started","contract_version":"4.1.1","model":{"provider":"anthropic","name":"claude-opus-5-5"}}`,
	`{"event":"progress","text":"reading the sources"}`,
	`{"event":"tool_call","name":"profile","args_summary":"src_1"}`,
	`{"event":"usage","usage":{"input":10,"cache_read":0,"cache_write":0,"output":5,"usd":0.5}}`,
	`{"event":"turn_finished","status":"done"}`,
}

func TestContract(t *testing.T) {
	// The constant the worker checks the agent against is the vendored schemas' version.
	t.Run("names the vendored contract version", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join(contracttest.Dir(t), "VERSION"))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Fields(string(raw))[0]; got != ContractVersion {
			t.Fatalf("schema/VERSION is %q, ContractVersion is %q", got, ContractVersion)
		}
	})

	// Every request the generator can build reaches the agent in a shape it accepts.
	t.Run("builds requests the agent accepts", func(t *testing.T) {
		schema := contracttest.Schema(t, "turn_request.json")
		sources := []Source{
			{ID: "src_0123456789abcdef", DisplayName: "Stripe / customers", Path: "cache/ab.jsonl", Fingerprint: "ws/w/connectors/c/customers/1.jsonl.gz",
				Observed: ObservedSync{At: time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC), SyncID: "01JDSYNC", ChangeSeq: 3}},
			{ID: "src_fedcba9876543210", DisplayName: "Stripe / invoices", Path: "cache/cd.jsonl", Fingerprint: "ws/w/connectors/c/invoices/1.jsonl.gz"},
			{ID: "01JDDOCCSV", DisplayName: "Exports / orders.csv", Path: "cache/ef.jsonl", Fingerprint: "ef",
				Observed: ObservedUpload{DocumentID: "01JDDOCCSV", UploadedAt: time.Date(2026, 10, 19, 12, 30, 0, 0, time.UTC)}},
			{Kind: SourceDocuments, ID: "01JDSETCONTRACTS0000000000", DisplayName: "Contracts", Description: "Signed contracts",
				Documents:   []DocumentFile{{DocID: "doc-1", Path: "generations/g/doc-1.md", MediaType: MediaMarkdown, Title: "msa.md"}},
				Fingerprint: "ab12"},
			{Kind: SourceDocuments, ID: "01JDSETGENERAL000000000000", DisplayName: "General",
				Documents:   []DocumentFile{{DocID: "doc-2", Path: "generations/g/doc-2.txt", MediaType: MediaPlainText, Title: "notes.txt"}},
				Fingerprint: "cd34"},
		}
		for name, start := range map[string]Start{
			"with goals":    {Workflow: "We bill customers monthly.", Goals: []string{"Which customers churn?"}},
			"without goals": {Workflow: "We bill customers monthly.", Goals: []string{}},
		} {
			body, err := json.Marshal(TurnRequest{
				SessionID: "01GEN", RequestID: "01GEN", Sources: sources, Input: Input{Start: start},
				Limits: Limits{MaxUSD: 10, MaxSteps: 200, MaxWallSecs: 1200}, Models: houseTurn(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := contracttest.Validate(t, schema, body); err != nil {
				t.Errorf("%s: %s does not match turn_request.json: %v", name, body, err)
			}
		}
		// The check has teeth: a start without its workflow, or a turn
		// without its models, is refused.
		models, _ := json.Marshal(houseTurn())
		if err := contracttest.Validate(t, schema, []byte(`{"session_id":"s","request_id":"r","sources":[],"input":{"start":{"goals":[]}},
			"limits":{"max_usd":1,"max_steps":1,"max_wall_secs":1},"models":`+string(models)+`}`)); err == nil {
			t.Error("turn_request.json accepted a start without a workflow")
		}
		if err := contracttest.Validate(t, schema, []byte(`{"session_id":"s","request_id":"r","sources":[],"input":{"start":{"workflow":"w","goals":[]}},
			"limits":{"max_usd":1,"max_steps":1,"max_wall_secs":1}}`)); err == nil {
			t.Error("turn_request.json accepted a turn without its models")
		}
	})

	// The fixtures the runner tests use are results and events the agent could send.
	t.Run("test fixtures match the result and event schemas", func(t *testing.T) {
		results, events := contracttest.Schema(t, "turn_result.json"), contracttest.Schema(t, "event.json")
		for _, status := range []string{`"done"`, `"needs_input"`, `"approved"`, `"budget_exhausted"`, `"stale_draft"`,
			`{"source_unavailable":{"source":"src_1"}}`, `{"model_failed":{"reason":"unavailable"}}`, `{"model_failed":{"reason":"refused"}}`, `{"model_failed":{"reason":"auth"}}`,
			`{"invalid_input":{"errors":[{"item":"type.X","kind":"not_found","message":"no such item","hint":"check the id"}]}}`} {
			if err := contracttest.Validate(t, results, []byte(testResult(status, 2))); err != nil {
				t.Errorf("result %s: %v", status, err)
			}
		}
		for _, event := range testEvents {
			if err := contracttest.Validate(t, events, []byte(event)); err != nil {
				t.Errorf("event %s: %v", event, err)
			}
		}
	})

	t.Run("decodes each status", func(t *testing.T) {
		for status, want := range map[string]Status{
			`"done"`:             {Kind: StatusDone},
			`"needs_input"`:      {Kind: StatusNeedsInput},
			`"approved"`:         {Kind: StatusApproved},
			`"budget_exhausted"`: {Kind: StatusBudgetExhausted},
			`"stale_draft"`:      {Kind: StatusStaleDraft},
			`{"source_unavailable":{"source":"src_1"}}`: {Kind: StatusSourceUnavailable, Source: "src_1"},
			`{"model_failed":{"reason":"unavailable"}}`: {Kind: StatusModelFailed, Failure: ModelUnavailable},
			`{"model_failed":{"reason":"refused"}}`:     {Kind: StatusModelFailed, Failure: ModelRefused},
			`{"model_failed":{"reason":"quota"}}`:       {Kind: StatusModelFailed, Failure: ModelQuota},
			`{"invalid_input":{"errors":[]}}`:           {Kind: StatusInvalidInput},
		} {
			got, err := DecodeTurnResult([]byte(testResult(status, 7)))
			if err != nil {
				t.Fatalf("%s: %v", status, err)
			}
			if got.Status != want || string(got.State) != `{"draft_rev":7,"opaque":[1,2,3]}` ||
				got.OntologyTOML != "version = \"draft\"\n" || got.MappingsTOML != "ontology = \"draft\"\n" ||
				!strings.Contains(string(got.Questions), `"id":"q1"`) || !strings.Contains(string(got.Draft), `"rev":7`) ||
				got.Summary == nil || *got.Summary != "built a draft" {
				t.Fatalf("%s: got %+v", status, got)
			}
			if got.Usage != (Usage{Input: 1000, CacheRead: 200, CacheWrite: 300, Output: 400, USD: 1.25}) {
				t.Fatalf("%s: usage %+v", status, got.Usage)
			}
			if (want.Kind == StatusApproved) != (got.Bundle != nil) {
				t.Fatalf("%s: bundle %s", status, got.Bundle)
			}
		}
	})

	// The rule counts come through as sent; a result without them has none.
	t.Run("decodes the rule counts", func(t *testing.T) {
		got, err := DecodeTurnResult([]byte(strings.Replace(testResult(`"done"`, 1), `"summary"`, `"rules":{"count":3,"evaluated":2},"summary"`, 1)))
		if err != nil || got.Rules == nil || *got.Rules != (RuleCounts{Count: 3, Evaluated: 2}) {
			t.Fatalf("rules = (%+v, %v)", got.Rules, err)
		}
		if got, err := DecodeTurnResult([]byte(testResult(`"done"`, 1))); err != nil || got.Rules != nil {
			t.Fatalf("without rules = (%+v, %v)", got.Rules, err)
		}
	})

	// A result the worker can't act on is refused rather than half-read.
	t.Run("refuses results it cannot act on", func(t *testing.T) {
		for name, data := range map[string]string{
			"not JSON":                  `{"status":`,
			"no status":                 strings.Replace(testResult(`"done"`, 1), `"status":"done",`, ``, 1),
			"an unknown status":         testResult(`"thinking"`, 1),
			"an unknown tagged status":  testResult(`{"exploded":{}}`, 1),
			"an unknown model failure":  testResult(`{"model_failed":{"reason":"tired"}}`, 1),
			"3.x's api failure":         testResult(`{"model_failed":{"reason":"api"}}`, 1),
			"a null state":              strings.Replace(testResult(`"done"`, 1), `"state":{"draft_rev":1,"opaque":[1,2,3]}`, `"state":null`, 1),
			"no rendered draft":         strings.Replace(testResult(`"done"`, 1), `"ontology_toml":"version = \"draft\"\n",`, ``, 1),
			"approved without bundle":   strings.Replace(testResult(`"done"`, 1), `"status":"done"`, `"status":"approved"`, 1),
			"a bundle without approval": strings.Replace(testResult(`"approved"`, 1), `"status":"approved"`, `"status":"done"`, 1),
			"more rules evaluated than drafted": strings.Replace(testResult(`"done"`, 1), `"summary"`,
				`"rules":{"count":2,"evaluated":3},"summary"`, 1),
		} {
			if _, err := DecodeTurnResult([]byte(data)); err == nil {
				t.Errorf("%s: decoded", name)
			}
		}
	})
}

func ExampleSource_MarshalJSON() {
	table, _ := json.Marshal(Source{ID: "src_1", DisplayName: "Stripe / customers", Path: "cache/ab.jsonl", Fingerprint: "ws/w/1.jsonl.gz"})
	fmt.Println(string(table))
	set, _ := json.Marshal(Source{
		Kind: SourceDocuments, ID: "01JDSET", DisplayName: "Contracts", Description: "Signed contracts", Fingerprint: "ab12",
		Documents: []DocumentFile{{DocID: "doc-1", Path: "generations/g/doc-1.md", MediaType: MediaMarkdown, Title: "msa.md"}},
	})
	fmt.Println(string(set))
	// A table says the sync its snapshot was taken at, and a table file its upload.
	synced, _ := json.Marshal(Source{ID: "src_1", DisplayName: "Stripe / customers", Path: "cache/ab.jsonl", Fingerprint: "ab",
		Observed: ObservedSync{At: time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC), SyncID: "01JDSYNC", ChangeSeq: 42}})
	fmt.Println(string(synced))
	uploaded, _ := json.Marshal(Source{ID: "01JDDOC", DisplayName: "Exports / orders.csv", Path: "cache/cd.jsonl", Fingerprint: "cd",
		Observed: ObservedUpload{DocumentID: "01JDDOC", UploadedAt: time.Date(2026, 10, 19, 12, 30, 0, 500000, time.UTC)}})
	fmt.Println(string(uploaded))
	// Output:
	// {"id":"src_1","display_name":"Stripe / customers","route":{"mapping":{"trust":1}},"access":{"file":{"path":"cache/ab.jsonl","format":"jsonl"}},"fingerprint":"ws/w/1.jsonl.gz"}
	// {"id":"01JDSET","display_name":"Contracts","description":"Signed contracts","route":"extraction","access":{"documents":{"files":[{"doc_id":"doc-1","path":"generations/g/doc-1.md","media_type":"markdown","title":"msa.md"}]}},"fingerprint":"ab12"}
	// {"id":"src_1","display_name":"Stripe / customers","route":{"mapping":{"trust":1}},"access":{"file":{"path":"cache/ab.jsonl","format":"jsonl"}},"fingerprint":"ab","observed":{"sync":{"at":"2026-10-20T06:00:00Z","sync_id":"01JDSYNC","change_seq":42}}}
	// {"id":"01JDDOC","display_name":"Exports / orders.csv","route":{"mapping":{"trust":1}},"access":{"file":{"path":"cache/cd.jsonl","format":"jsonl"}},"fingerprint":"cd","observed":{"upload":{"document_id":"01JDDOC","uploaded_at":"2026-10-19T12:30:00.0005Z"}}}
}

func TestTurnRuleCounts(t *testing.T) {
	raw := strings.Replace(testResult(`"done"`, 1), `"summary":`, `"rules":{"count":1,"evaluated":2},"summary":`, 1)
	if _, err := DecodeTurnResult([]byte(raw)); err == nil {
		t.Fatal("invalid rule counts accepted")
	}
}
