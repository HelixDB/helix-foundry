package ontologyagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
)

// askRequest is a question's request as the ask worker builds it.
func askRequest() AskRequest {
	return AskRequest{
		RequestID: "q1", Database: "tenant-1", Load: "load-1", OntologyVersion: "foundry-v3", OntologyTOML: "o", MappingsTOML: "m",
		DraftJSON: `{"items":{}}`, QuestionsJSON: `[]`, Goal: "track contracts",
		Catalog:  []CatalogEntry{{ID: "src_1", Kind: CatalogTable, Name: "Stripe / customers"}},
		Question: "Which customers?", History: []AskTurn{},
		Helix: HelixTarget{URL: "http://127.0.0.1:1", Token: "t", MaxRequestBytes: 10, TimeoutSecs: 30},
		AsOf:  "2026-10-19T12:00:00Z", Limits: Limits{MaxUSD: 0.5, MaxSteps: 10, MaxWallSecs: 120}, Models: houseAskModels(),
	}
}

func TestAskRequest(t *testing.T) {
	// Every request the worker can build is one the agent accepts: with or
	// without a focus, a thread's full history, and both kinds of source.
	t.Run("builds requests the agent accepts", func(t *testing.T) {
		schema := contracttest.Schema(t, "ask_request.json")
		focused := askRequest()
		focused.Focus = &EntityRef{Type: "Customer", ID: "scus_1"}
		threaded := askRequest()
		threaded.Catalog = append(threaded.Catalog, CatalogEntry{ID: "set_1", Kind: CatalogDocumentSet, Name: "Contracts"})
		for range MaxAskHistory {
			threaded.History = append(threaded.History, AskTurn{Question: "Who signed?", Answer: "Acme [c1]."})
		}
		for name, req := range map[string]AskRequest{"plain": askRequest(), "focused": focused, "threaded": threaded} {
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := contracttest.Validate(t, schema, body); err != nil {
				t.Errorf("%s: %s does not match ask_request.json: %v", name, body, err)
			}
		}
		plain, _ := json.Marshal(askRequest())
		if strings.Contains(string(plain), `"focus"`) {
			t.Fatalf("a request without a focus sends one: %s", plain)
		}
	})

	// The check has teeth: a focus is an entity by type and id, nothing else.
	t.Run("the schema refuses a focus that isn't an entity", func(t *testing.T) {
		body, _ := json.Marshal(askRequest())
		loose := strings.Replace(string(body), `"question":`, `"focus":{"type":"Customer","key":"c1"},"question":`, 1)
		if err := contracttest.Validate(t, contracttest.Schema(t, "ask_request.json"), []byte(loose)); err == nil {
			t.Fatalf("ask_request.json accepted %s", loose)
		}
	})
}

func TestParseFocus(t *testing.T) {
	// The website names an entity as reads do; the agent refuses anything
	// else, so the backend refuses it when the question is asked.
	t.Run("reads an entity by type and id", func(t *testing.T) {
		got, err := ParseFocus(` {"type":"Customer","id":"scus_1"} `)
		if err != nil || got != (EntityRef{Type: "Customer", ID: "scus_1"}) {
			t.Fatalf("focus = %+v, %v", got, err)
		}
	})

	t.Run("refuses anything else", func(t *testing.T) {
		for name, raw := range map[string]string{
			"not JSON":       `{`,
			"an array":       `["Customer","c1"]`,
			"no id":          `{"type":"Customer"}`,
			"an empty type":  `{"type":"","id":"c1"}`,
			"an extra field": `{"type":"Customer","id":"c1","key":"c1"}`,
			"two values":     `{"type":"Customer","id":"c1"}{}`,
			"null":           `null`,
		} {
			if _, err := ParseFocus(raw); err == nil {
				t.Errorf("%s: parsed %s", name, raw)
			}
		}
	})
}

// A question names its tables as the generation that drafted the version
// did: the first 8 bytes of SHA-256 of connector id, NUL, table.
func TestTableSourceID(t *testing.T) {
	t.Run("hashes the connector and the table", func(t *testing.T) {
		if got := TableSourceID("conn-1", "customers"); got != "src_093a2e7cf27f9d93" {
			t.Fatalf("id = %q", got)
		}
		if TableSourceID("conn-1", "customers") == TableSourceID("conn-2", "customers") {
			t.Fatal("ids are distinct across tables")
		}
	})
}

const (
	askUsage   = `"usage":{"input":10,"cache_read":0,"cache_write":0,"output":5,"usd":0.03}`
	askQueries = `"queries":[{"evidence_id":"e2","query":{"start":{"type":"Customer","where":[],"ids":null,"text":null},"steps":[],"filters":[],` +
		`"return":{"count":{"distinct_at":0}}}}]`
	// citeEveryTarget cites a target of each kind.
	citeEveryTarget = `{"marker":"c1","evidence_id":"e1","target":{"entity":{"type":"Customer","id":"acme"}}},` +
		`{"marker":"c2","evidence_id":"e1","target":{"fact":{"link":"party_to","subject":{"type":"Customer","id":"acme"},"object":{"type":"Contract","id":"msa"}}}},` +
		`{"marker":"c3","evidence_id":"e3","target":{"derived":{"pred":"derived:overdue","args":[{"type":"Invoice","id":"in_1"}]}}},` +
		`{"marker":"c4","evidence_id":"e4","target":{"row":{"source":"src_1","mapping":"customers","row_key":"cus_1"}}},` +
		`{"marker":"c5","evidence_id":"e4","target":{"quote":{"doc_id":"d1","section":2,"start":4,"end":9}}},` +
		`{"marker":"c6","evidence_id":"e5","target":{"violation":{"constraint":"credit_limit","key":"v1"}}},` +
		`{"marker":"c7","evidence_id":"e2","target":{"result":{"evidence_id":"e2"}}},` +
		`{"marker":"c8","evidence_id":"e4","target":{"row":{"source":"foundry_enrichment","mapping":"people","row_key":""}}},` +
		`{"marker":"c9","evidence_id":"e6","target":{"version":{"tx":"01JDTX0000000000000000000A","at":"2026-10-20T08:30:00.123456Z",` +
		`"of":{"entity":{"type":"Customer","id":"acme"}}}}},` +
		`{"marker":"c10","evidence_id":"e6","target":{"version":{"tx":"01JDTX0000000000000000000B","at":"2026-10-20T09:30:00+01:00",` +
		`"of":{"prop":{"entity":{"type":"Customer","id":"acme"},"prop":"tier"}}}}},` +
		`{"marker":"c11","evidence_id":"e6","target":{"version":{"tx":"01JDTX0000000000000000000C","at":"2026-10-20T08:30:00Z",` +
		`"of":{"fact":{"link":"party_to","subject":{"type":"Customer","id":"acme"},"object":{"type":"Contract","id":"msa"}}}}}},` +
		`{"marker":"c12","evidence_id":"e7","target":{"version":{"tx":"01JDTX0000000000000000000D","at":"2026-10-21T00:00:00Z",` +
		`"of":{"record":{"constraint":"credit_limit","key":"v1"}}}}}`
)

// version is a version citation's target as a result writes it: tx, at and of.
func version(tx, at, of string) string {
	return answered(`{"marker":"c1","evidence_id":"e6","target":{"version":{` + strings.Join(
		slices.DeleteFunc([]string{tx, at, of}, func(field string) bool { return field == "" }), ",") + `}}}`)
}

// answered is an answered result with these citations.
func answered(citations string) string {
	return `{"status":"answered","answer":{"blocks":[{"para":"Acme [c1]"},{"list":["Globex [c2]"]}]},"citations":[` + citations + `],` +
		askQueries + `,` + askUsage + `}`
}

// askQuery is a typed query as a result records it.
const askQuery = `{"start":{"type":"Customer","where":[],"ids":null,"text":null},"steps":[],"filters":[],"return":{"count":{"distinct_at":0}}}`

// recordedOutputs are a 3.11.0 result's outputs by evidence id: one of every
// kind the Read call's query answers, rows with a cell of every kind and a
// cut, a document's values wrapped as untrusted.
var recordedOutputs = []struct{ evidence, output string }{
	{"e2", `{"kind":"rows","rows":[{"cells":[{"type":"Customer","id":"acme","name":"Acme"},{"untrusted":"Gold"},1200.5,null],` +
		`"edges":[{"step":0,"fact":{"link":"party_to","subject":{"type":"Customer","id":"acme"},"object":{"type":"Contract","id":"msa"}}}]},` +
		`{"cells":[{"type":"Customer","id":"globex","name":"Globex"},"Silver",80,true],"edges":[]}],"total":14,"cut":"12 rows left out"}`},
	{"e3", `{"kind":"count","count":42}`},
	{"e4", `{"kind":"group_count","groups":[{"value":{"untrusted":"Gold"},"count":12},{"value":null,"count":3}],"total_groups":2}`},
	{"e5", `{"kind":"aggregate","value":null,"values":0}`},
	{"e6", `{"kind":"grouped_aggregate","groups":[{"value":{"untrusted":"2026-09"},"aggregate":5400,"values":4},` +
		`{"value":{"untrusted":"2026-10"},"aggregate":null,"values":0}],"total_groups":2}`},
}

// withOutputs is an answered 3.11.0 result of three blocks showing visuals
// (a JSON array's items). Its queries are recordedOutputs' and e7, whose
// output the bound left out.
func withOutputs(visuals string) string {
	queries := make([]string, 0, len(recordedOutputs)+1)
	for _, o := range recordedOutputs {
		queries = append(queries, `{"evidence_id":"`+o.evidence+`","query":`+askQuery+`,"output":`+o.output+`}`)
	}
	queries = append(queries, `{"evidence_id":"e7","query":`+askQuery+`}`)
	return `{"status":"answered","answer":{"blocks":[{"para":"Acme leads [c1]:"},{"list":["Acme [c1]","Globex [c2]"]},{"para":"By month:"}],` +
		`"visuals":[` + visuals + `]},"citations":[{"marker":"c1","evidence_id":"e2","target":{"entity":{"type":"Customer","id":"acme"}}},` +
		`{"marker":"c2","evidence_id":"e2","target":{"result":{"evidence_id":"e2"}}}],"queries":[` + strings.Join(queries, ",") + `],` + askUsage + `}`
}

// everyVisual shows four results: a kind and a place of each, and a title
// of exactly MaxVisualTitleChars characters, some of them multibyte.
var everyVisual = `{"evidence_id":"e2","after":0,"kind":"table","title":"Customers"},{"evidence_id":"e4","after":1,"kind":"bar"},` +
	`{"evidence_id":"e6","kind":"line","title":"` + strings.Repeat("é", MaxVisualTitleChars) + `"},{"evidence_id":"e3","after":2,"kind":"figure"}`

// withOutput is withOutputs showing e2, whose output is output.
func withOutput(output string) string {
	return strings.Replace(withOutputs(`{"evidence_id":"e2"}`), recordedOutputs[0].output, output, 1)
}

func TestDecodeAskResult(t *testing.T) {
	// An answer cites a target of every kind the agent can show; each comes
	// through whole, for the website to open.
	t.Run("reads an answer citing every kind of target", func(t *testing.T) {
		raw := answered(citeEveryTarget)
		if err := contracttest.Validate(t, contracttest.Schema(t, "ask_result.json"), []byte(raw)); err != nil {
			t.Fatalf("the fixture does not match ask_result.json: %v", err)
		}
		got, err := DecodeAskResult([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		in1, acme := EntityRef{Type: "Invoice", ID: "in_1"}, EntityRef{Type: "Customer", ID: "acme"}
		msa := FactTarget{Link: "party_to", Subject: acme, Object: EntityRef{Type: "Contract", ID: "msa"}}
		at := func(raw string) time.Time {
			parsed, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				t.Fatal(err)
			}
			return parsed
		}
		want := []CitationTarget{
			{Entity: &EntityRef{Type: "Customer", ID: "acme"}},
			{Fact: &FactTarget{Link: "party_to", Subject: EntityRef{Type: "Customer", ID: "acme"}, Object: EntityRef{Type: "Contract", ID: "msa"}}},
			{Derived: &DerivedTarget{Pred: "derived:overdue", Args: []EntityRef{in1}}},
			{Row: &RowTarget{Source: "src_1", Mapping: "customers", RowKey: "cus_1"}},
			{Quote: &QuoteTarget{DocID: "d1", Section: 2, Start: 4, End: 9}},
			{Violation: &ViolationTarget{Constraint: "credit_limit", Key: "v1"}},
			{Result: &ResultTarget{EvidenceID: "e2"}},
			{Row: &RowTarget{Source: EnrichmentSourceID, Mapping: "people"}},
			{Version: &VersionTarget{Tx: "01JDTX0000000000000000000A", At: at("2026-10-20T08:30:00.123456Z"), Of: VersionOf{Entity: &acme}}},
			{Version: &VersionTarget{Tx: "01JDTX0000000000000000000B", At: at("2026-10-20T09:30:00+01:00"),
				Of: VersionOf{Prop: &PropTarget{Entity: acme, Prop: "tier"}}}},
			{Version: &VersionTarget{Tx: "01JDTX0000000000000000000C", At: at("2026-10-20T08:30:00Z"), Of: VersionOf{Fact: &msa}}},
			{Version: &VersionTarget{Tx: "01JDTX0000000000000000000D", At: at("2026-10-21T00:00:00Z"),
				Of: VersionOf{Record: &ViolationTarget{Constraint: "credit_limit", Key: "v1"}}}},
		}
		if len(got.Citations) != len(want) {
			t.Fatalf("citations = %+v", got.Citations)
		}
		for i, c := range got.Citations {
			if c.Marker != fmt.Sprintf("c%d", i+1) || !reflect.DeepEqual(c.Target, want[i]) {
				t.Fatalf("citation %d = %+v, want %+v", i, c, want[i])
			}
		}
		// The same instant written in another offset is the same version time.
		if !got.Citations[9].Target.Version.At.Equal(at("2026-10-20T08:30:00Z")) {
			t.Fatalf("at = %s", got.Citations[9].Target.Version.At)
		}
		switch {
		case got.Status != AskAnswered || len(got.Answer.Blocks) != 2 || *got.Answer.Blocks[0].Para != "Acme [c1]" ||
			got.Answer.Blocks[1].List[0] != "Globex [c2]":
			t.Fatalf("answer = %+v", got)
		case len(got.Queries) != 1 || got.Queries[0].EvidenceID != "e2" || got.Usage.USD != 0.03 || got.Usage.Input != 10:
			t.Fatalf("queries %+v, usage %+v", got.Queries, got.Usage)
		}
	})

	// A question that can't be answered says why and what is missing; the
	// website offers to draft a version with the gap.
	t.Run("reads cannot_answer's reason and gap", func(t *testing.T) {
		raw := `{"status":"cannot_answer","reason":"No link says who signed.","gap":"A signed_by link.",` + askQueries + `,` + askUsage + `}`
		if err := contracttest.Validate(t, contracttest.Schema(t, "ask_result.json"), []byte(raw)); err != nil {
			t.Fatalf("the fixture does not match ask_result.json: %v", err)
		}
		got, err := DecodeAskResult([]byte(raw))
		if err != nil || got.Status != AskCannotAnswer || got.Reason != "No link says who signed." || got.Gap != "A signed_by link." ||
			got.Answer != nil || got.Citations != nil {
			t.Fatalf("result = %+v, %v", got, err)
		}
	})

	// A cap that ended the question first leaves only what it spent.
	t.Run("reads budget_exhausted", func(t *testing.T) {
		raw := `{"status":"budget_exhausted","queries":[],` + askUsage + `}`
		if err := contracttest.Validate(t, contracttest.Schema(t, "ask_result.json"), []byte(raw)); err != nil {
			t.Fatalf("the fixture does not match ask_result.json: %v", err)
		}
		got, err := DecodeAskResult([]byte(raw))
		if err != nil || got.Status != AskBudgetExhausted || got.Usage.Output != 5 {
			t.Fatalf("result = %+v, %v", got, err)
		}
	})

	// Each reason a question fails for is one the worker words for the user.
	t.Run("reads each failure reason", func(t *testing.T) {
		schema := contracttest.Schema(t, "ask_result.json")
		for _, reason := range AskFailures {
			raw := `{"status":"failed","reason":"` + string(reason) + `","queries":[],` + askUsage + `}`
			if err := contracttest.Validate(t, schema, []byte(raw)); err != nil {
				t.Fatalf("%s does not match ask_result.json: %v", reason, err)
			}
			if got, err := DecodeAskResult([]byte(raw)); err != nil || got.Status != AskFailed || got.Failure != reason {
				t.Fatalf("%s: %+v, %v", reason, got, err)
			}
		}
	})

	// A model call's failure reads as the contract's model failure, so the
	// worker words it as its provider's copy does; the rest are no model's.
	t.Run("reads which failures are a model call's", func(t *testing.T) {
		models := 0
		for _, reason := range AskFailures {
			f, ok := reason.ModelFailure()
			if !ok {
				continue
			}
			models++
			want := "model_" + string(f)
			switch f {
			case ModelUnavailable:
				want = "model_failed"
			case ModelChanged:
				want = "model_changed"
			}
			if string(reason) != want {
				t.Errorf("%s reads as %s", reason, f)
			}
		}
		// Every model failure but context_too_long, which ends a question budget_exhausted.
		if models != len(ModelFailures)-1 {
			t.Errorf("%d reasons are a model's, want %d", models, len(ModelFailures)-1)
		}
		if _, ok := AskNoAnswer.ModelFailure(); ok {
			t.Error("no_answer reads as a model failure")
		}
		// The contract's reasons, and no other.
		var schema struct {
			Defs map[string]struct {
				OneOf []struct {
					Const string `json:"const"`
				} `json:"oneOf"`
			} `json:"$defs"`
		}
		raw, _ := os.ReadFile(filepath.Join(contracttest.Dir(t), "ask_result.json"))
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range schema.Defs["Failure"].OneOf {
			names = append(names, c.Const)
		}
		if len(names) != len(AskFailures) {
			t.Errorf("ask_result.json names %v, the worker %v", names, AskFailures)
		}
	})

	// A 3.11.0 answer shows up to four query results, each from the output
	// recorded for its query, kept as the agent wrote it.
	t.Run("reads an answer's visuals and its queries' outputs", func(t *testing.T) {
		raw := withOutputs(everyVisual)
		if err := contracttest.Validate(t, contracttest.Schema(t, "ask_result.json"), []byte(raw)); err != nil {
			t.Fatalf("the fixture does not match ask_result.json: %v", err)
		}
		got, err := DecodeAskResult([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		at := func(n uint32) *uint32 { return &n }
		want := []AskVisual{
			{EvidenceID: "e2", After: at(0), Kind: VisualTable, Title: "Customers"},
			{EvidenceID: "e4", After: at(1), Kind: VisualBar},
			{EvidenceID: "e6", Kind: VisualLine, Title: strings.Repeat("é", MaxVisualTitleChars)},
			{EvidenceID: "e3", After: at(2), Kind: VisualFigure},
		}
		if !reflect.DeepEqual(got.Answer.Visuals, want) {
			t.Fatalf("visuals = %+v", got.Answer.Visuals)
		}
		if len(got.Queries) != len(recordedOutputs)+1 || got.Queries[len(recordedOutputs)].Output != nil {
			t.Fatalf("queries = %+v", got.Queries)
		}
		for i, o := range recordedOutputs {
			if q := got.Queries[i]; q.EvidenceID != o.evidence || string(q.Output) != o.output {
				t.Fatalf("query %d = %s %s, want %s %s", i, q.EvidenceID, q.Output, o.evidence, o.output)
			}
		}
	})

	// A visual needs only its result: the website places it after the last
	// block, in the kind the output suits. A null is the same as leaving a
	// field out, as is a null output.
	t.Run("reads a visual of only its result", func(t *testing.T) {
		raw := strings.Replace(withOutputs(`{"evidence_id":"e5","after":null,"kind":null,"title":null}`), `"evidence_id":"e7","query":`+askQuery,
			`"evidence_id":"e7","query":`+askQuery+`,"output":null`, 1)
		if err := contracttest.Validate(t, contracttest.Schema(t, "ask_result.json"), []byte(raw)); err != nil {
			t.Fatalf("the fixture does not match ask_result.json: %v", err)
		}
		got, err := DecodeAskResult([]byte(raw))
		if err != nil || !reflect.DeepEqual(got.Answer.Visuals, []AskVisual{{EvidenceID: "e5"}}) || got.Queries[len(recordedOutputs)].Output != nil {
			t.Fatalf("result = %+v, %v", got, err)
		}
	})

	// Every result stored before 3.11.0 still reads, and reads as one with no
	// outputs and no visuals, as MySQL hands it back: keys reordered, spaced.
	t.Run("reads a 3.10.0 result as stored", func(t *testing.T) {
		schema := contracttest.Schema(t, "ask_result.json")
		stored := `{"usage": {"usd": 0.03, "input": 10, "output": 5, "cache_read": 0, "cache_write": 0}, "answer": {"blocks": [{"para": "Two [c1]."}]}, ` +
			`"status": "answered", "queries": [{"query": {"ids": null, "start": {"ids": null, "text": null, "type": "Customer", "where": []}, ` +
			`"steps": [], "return": {"count": {"distinct_at": 0}}, "filters": []}, "evidence_id": "e2"}], ` +
			`"citations": [{"marker": "c1", "target": {"result": {"evidence_id": "e2"}}, "evidence_id": "e2"}]}`
		// The query's stray "ids" is a field the schema leaves open; MySQL keeps it.
		stored = strings.Replace(stored, `{"query": {"ids": null, `, `{"query": {`, 1)
		for name, raw := range map[string]string{
			"stored": stored, "every target": answered(citeEveryTarget),
			"cannot_answer": `{"status":"cannot_answer","reason":"r","gap":"g",` + askQueries + `,` + askUsage + `}`,
			"failed":        `{"status":"failed","reason":"no_answer","queries":[],` + askUsage + `}`,
		} {
			if err := contracttest.Validate(t, schema, []byte(raw)); err != nil {
				t.Fatalf("%s does not match ask_result.json: %v", name, err)
			}
			got, err := DecodeAskResult([]byte(raw))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got.Answer != nil && got.Answer.Visuals != nil {
				t.Fatalf("%s: visuals = %+v", name, got.Answer.Visuals)
			}
			for _, q := range got.Queries {
				if q.Output != nil {
					t.Fatalf("%s: output = %s", name, q.Output)
				}
			}
		}
	})

	// The schema checks have teeth: what the contract refuses, it refuses.
	t.Run("the schema refuses a result the agent can't send", func(t *testing.T) {
		schema := contracttest.Schema(t, "ask_result.json")
		for name, raw := range map[string]string{
			"an unknown failure":      `{"status":"failed","reason":"tired","queries":[],` + askUsage + `}`,
			"a quote with no section": answered(`{"marker":"c1","evidence_id":"e1","target":{"quote":{"doc_id":"d1","start":0,"end":4}}}`),
			"a violation by id":       answered(`{"marker":"c1","evidence_id":"e1","target":{"violation":{"constraint":"c","id":"v1"}}}`),
			"five visuals":            withOutputs(everyVisual + `,{"evidence_id":"e5"}`),
			"a visual of markup":      withOutputs(`{"evidence_id":"e2","html":"<b>"}`),
			"a visual of no result":   withOutputs(`{"after":0}`),
			"a pie":                   withOutputs(`{"evidence_id":"e2","kind":"pie"}`),
			"a long title":            withOutputs(`{"evidence_id":"e2","title":"` + strings.Repeat("x", MaxVisualTitleChars+1) + `"}`),
			"a visual before blocks":  withOutputs(`{"evidence_id":"e2","after":-1}`),
			"an output of no kind":    withOutput(`{"count":1}`),
			"a count of rows":         withOutput(`{"kind":"count","rows":[]}`),
		} {
			if err := contracttest.Validate(t, schema, []byte(raw)); err == nil {
				t.Errorf("%s: ask_result.json accepted %s", name, raw)
			}
		}
	})

	// A version is a transaction, its time and exactly one thing it is of,
	// named by its kind; the schema takes nothing else.
	t.Run("the schema refuses a version the agent can't send", func(t *testing.T) {
		schema := contracttest.Schema(t, "ask_result.json")
		const tx, at = `"tx":"01JDTX"`, `"at":"2026-10-20T08:30:00Z"`
		entity := `"of":{"entity":{"type":"Customer","id":"acme"}}`
		if err := contracttest.Validate(t, schema, []byte(version(tx, at, entity))); err != nil {
			t.Fatalf("a whole version: %v", err)
		}
		for name, raw := range map[string]string{
			"no tx":                 version("", at, entity),
			"no at":                 version(tx, "", entity),
			"no of":                 version(tx, at, ""),
			"a tx not text":         version(`"tx":7`, at, entity),
			"an extra field":        version(tx, at, entity+`,"subject":"acme"`),
			"of two things":         version(tx, at, `"of":{"entity":{"type":"C","id":"c"},"record":{"constraint":"c","key":"k"}}`),
			"of nothing":            version(tx, at, `"of":{}`),
			"of a violation":        version(tx, at, `"of":{"violation":{"constraint":"c","key":"k"}}`),
			"of a prop by its name": version(tx, at, `"of":{"prop":{"entity":{"type":"C","id":"c"}}}`),
			"of a fact half":        version(tx, at, `"of":{"fact":{"link":"l","subject":{"type":"C","id":"c"}}}`),
		} {
			if err := contracttest.Validate(t, schema, []byte(raw)); err == nil {
				t.Errorf("%s: ask_result.json accepted %s", name, raw)
			}
		}
	})

	// A result the worker can't vouch for is not stored for the website.
	t.Run("refuses what the contract doesn't allow", func(t *testing.T) {
		const tail = `"queries":[],` + askUsage + `}`
		for name, raw := range map[string]string{
			"no status":                   `{` + tail,
			"an unknown status":           `{"status":"maybe",` + tail,
			"no usage":                    `{"status":"budget_exhausted","queries":[]}`,
			"no queries":                  `{"status":"budget_exhausted",` + askUsage + `}`,
			"answered with no answer":     `{"status":"answered","citations":[],` + tail,
			"answered with no blocks":     `{"status":"answered","answer":{"blocks":[]},"citations":[],` + tail,
			"answered with no citations":  `{"status":"answered","answer":{"blocks":[{"para":"x"}]},` + tail,
			"an answer on a failure":      `{"status":"failed","reason":"no_answer","answer":{"blocks":[{"para":"x"}]},` + tail,
			"cannot_answer with no gap":   `{"status":"cannot_answer","reason":"r",` + tail,
			"cannot_answer with no why":   `{"status":"cannot_answer","gap":"g",` + tail,
			"cannot_answer, a null why":   `{"status":"cannot_answer","reason":null,"gap":"g",` + tail,
			"a failure with no reason":    `{"status":"failed",` + tail,
			"an unknown failure":          `{"status":"failed","reason":"tired",` + tail,
			"a block of both":             `{"status":"answered","answer":{"blocks":[{"para":"x","list":["y"]}]},"citations":[],` + tail,
			"a block of neither":          `{"status":"answered","answer":{"blocks":[{}]},"citations":[],` + tail,
			"a citation with no marker":   answered(`{"marker":"","evidence_id":"e1","target":{"result":{"evidence_id":"e1"}}}`),
			"a citation with no evidence": answered(`{"marker":"c1","evidence_id":"","target":{"result":{"evidence_id":"e1"}}}`),
			"a marker cited twice": answered(`{"marker":"c1","evidence_id":"e1","target":{"result":{"evidence_id":"e1"}}},` +
				`{"marker":"c1","evidence_id":"e2","target":{"result":{"evidence_id":"e2"}}}`),
			"no target":               answered(`{"marker":"c1","evidence_id":"e1","target":{}}`),
			"two targets":             answered(`{"marker":"c1","evidence_id":"e1","target":{"entity":{"type":"C","id":"c"},"result":{"evidence_id":"e1"}}}`),
			"an entity not an object": answered(`{"marker":"c1","evidence_id":"e1","target":{"entity":"acme"}}`),
			"an entity with no id":    answered(`{"marker":"c1","evidence_id":"e1","target":{"entity":{"type":"Customer"}}}`),
			"a fact with no object":   answered(`{"marker":"c1","evidence_id":"e1","target":{"fact":{"link":"l","subject":{"type":"C","id":"c"}}}}`),
			"a derived with no pred":  answered(`{"marker":"c1","evidence_id":"e1","target":{"derived":{"pred":"","args":[]}}}`),
			"a derived half an arg":   answered(`{"marker":"c1","evidence_id":"e1","target":{"derived":{"pred":"derived:x","args":[{"type":"C"}]}}}`),
			"a row with no key":       answered(`{"marker":"c1","evidence_id":"e1","target":{"row":{"source":"s","mapping":"m","row_key":""}}}`),
			"a profile's row with its key": answered(`{"marker":"c1","evidence_id":"e1","target":{"row":{"source":"foundry_enrichment",` +
				`"mapping":"people","row_key":"li:ada"}}}`),
			"a quote backwards":        answered(`{"marker":"c1","evidence_id":"e1","target":{"quote":{"doc_id":"d1","section":0,"start":9,"end":4}}}`),
			"a quote of no document":   answered(`{"marker":"c1","evidence_id":"e1","target":{"quote":{"doc_id":"","section":0,"start":0,"end":4}}}`),
			"a violation with no key":  answered(`{"marker":"c1","evidence_id":"e1","target":{"violation":{"constraint":"c"}}}`),
			"a result of no evidence":  answered(`{"marker":"c1","evidence_id":"e1","target":{"result":{"evidence_id":""}}}`),
			"a version of no tx":       version(`"tx":""`, `"at":"2026-10-20T08:30:00Z"`, `"of":{"entity":{"type":"C","id":"c"}}`),
			"a version with no tx":     version("", `"at":"2026-10-20T08:30:00Z"`, `"of":{"entity":{"type":"C","id":"c"}}`),
			"a version with no at":     version(`"tx":"t1"`, "", `"of":{"entity":{"type":"C","id":"c"}}`),
			"a version of an empty at": version(`"tx":"t1"`, `"at":""`, `"of":{"entity":{"type":"C","id":"c"}}`),
			"a version at no time":     version(`"tx":"t1"`, `"at":"yesterday"`, `"of":{"entity":{"type":"C","id":"c"}}`),
			"a version at the zero time": version(`"tx":"t1"`, `"at":"0001-01-01T00:00:00Z"`,
				`"of":{"entity":{"type":"C","id":"c"}}`),
			"a version of nothing":     version(`"tx":"t1"`, `"at":"2026-10-20T08:30:00Z"`, ""),
			"a version of no kind":     version(`"tx":"t1"`, `"at":"2026-10-20T08:30:00Z"`, `"of":{}`),
			"a version of a violation": version(`"tx":"t1"`, `"at":"2026-10-20T08:30:00Z"`, `"of":{"violation":{"constraint":"c","key":"k"}}`),
			"a version of two things": version(`"tx":"t1"`, `"at":"2026-10-20T08:30:00Z"`,
				`"of":{"entity":{"type":"C","id":"c"},"fact":{"link":"l","subject":{"type":"C","id":"c"},"object":{"type":"C","id":"d"}}}`),
			"a version of half an entity": version(`"tx":"t1"`, `"at":"2026-10-20T08:30:00Z"`, `"of":{"entity":{"type":"C"}}`),
			"a version of a nameless prop": version(`"tx":"t1"`, `"at":"2026-10-20T08:30:00Z"`,
				`"of":{"prop":{"entity":{"type":"C","id":"c"},"prop":""}}`),
			"a version of a half entity's prop": version(`"tx":"t1"`, `"at":"2026-10-20T08:30:00Z"`,
				`"of":{"prop":{"entity":{"id":"c"},"prop":"tier"}}`),
			"a version of a fact with no object": version(`"tx":"t1"`, `"at":"2026-10-20T08:30:00Z"`,
				`"of":{"fact":{"link":"l","subject":{"type":"C","id":"c"}}}`),
			"a version of a record with no key": version(`"tx":"t1"`, `"at":"2026-10-20T08:30:00Z"`,
				`"of":{"record":{"constraint":"c"}}`),
			"a version beside an entity": answered(`{"marker":"c1","evidence_id":"e1","target":{"entity":{"type":"C","id":"c"},` +
				`"version":{"tx":"t1","at":"2026-10-20T08:30:00Z","of":{"entity":{"type":"C","id":"c"}}}}}`),
			"a query with no evidence": `{"status":"budget_exhausted","queries":[{"evidence_id":"","query":{}}],` + askUsage + `}`,
			"a query with no query":    `{"status":"budget_exhausted","queries":[{"evidence_id":"e1"}],` + askUsage + `}`,
			"a query not an object":    `{"status":"budget_exhausted","queries":[{"evidence_id":"e1","query":"count"}],` + askUsage + `}`,
			"two queries of one evidence": `{"status":"budget_exhausted","queries":[{"evidence_id":"e1","query":{}},{"evidence_id":"e1","query":{}}],` +
				askUsage + `}`,
			"not json": `nope`,
		} {
			if _, err := DecodeAskResult([]byte(raw)); err == nil {
				t.Errorf("%s: decoded %s", name, raw)
			}
		}
	})

	// A visual the agent's check would have refused is not drawn from what
	// the backend stores: each refusal, at its edge.
	t.Run("refuses a visual the agent would not send", func(t *testing.T) {
		title := func(n int) string { return `{"evidence_id":"e2","title":"` + strings.Repeat("x", n) + `"}` }
		if _, err := DecodeAskResult([]byte(withOutputs(title(MaxVisualTitleChars)))); err != nil {
			t.Fatalf("a title of %d characters: %v", MaxVisualTitleChars, err)
		}
		if _, err := DecodeAskResult([]byte(withOutputs(`{"evidence_id":"e2","after":2}`))); err != nil {
			t.Fatalf("after the last block: %v", err)
		}
		for name, raw := range map[string]string{
			"five visuals":                withOutputs(everyVisual + `,{"evidence_id":"e5"}`),
			"a result of no query":        withOutputs(`{"evidence_id":"e9"}`),
			"a citation's evidence":       withOutputs(`{"evidence_id":"e1"}`),
			"no evidence":                 withOutputs(`{"after":0}`),
			"a query with no output":      withOutputs(`{"evidence_id":"e7"}`),
			"a result shown twice":        withOutputs(`{"evidence_id":"e2","kind":"table"},{"evidence_id":"e2","kind":"bar"}`),
			"after a block past the last": withOutputs(`{"evidence_id":"e2","after":3}`),
			"after a negative block":      withOutputs(`{"evidence_id":"e2","after":-1}`),
			"after a block that isn't":    withOutputs(`{"evidence_id":"e2","after":"first"}`),
			"an unknown kind":             withOutputs(`{"evidence_id":"e2","kind":"pie"}`),
			"a long title":                withOutputs(title(MaxVisualTitleChars + 1)),
			"a title not text":            withOutputs(`{"evidence_id":"e2","title":["x"]}`),
			"visuals not a list":          strings.Replace(withOutputs(`{"evidence_id":"e2"}`), `"visuals":[{"evidence_id":"e2"}]`, `"visuals":{"e2":{}}`, 1),
			"a null output shown": strings.Replace(withOutputs(`{"evidence_id":"e7"}`), `"evidence_id":"e7","query":`+askQuery,
				`"evidence_id":"e7","query":`+askQuery+`,"output":null`, 1),
		} {
			if _, err := DecodeAskResult([]byte(raw)); err == nil {
				t.Errorf("%s: decoded %s", name, raw)
			}
		}
	})

	// An output is the Read call's query answer, whole and within the
	// agent's bounds, whether a visual shows it or not.
	t.Run("refuses an output that isn't a query answer", func(t *testing.T) {
		row := `{"cells":[1],"edges":[]}`
		rows := func(n int) string {
			return `{"kind":"rows","rows":[` + strings.Repeat(row+",", n-1) + row + `],"total":300}`
		}
		group := `{"value":"Gold","count":1}`
		groups := func(n int) string {
			return `{"kind":"group_count","groups":[` + strings.Repeat(group+",", n-1) + group + `],"total_groups":300}`
		}
		for _, raw := range []string{rows(maxOutputRows), groups(maxOutputGroups), `{"kind":"aggregate","value":2.5,"values":2}`} {
			if _, err := DecodeAskResult([]byte(withOutput(raw))); err != nil {
				t.Fatalf("%.80s: %v", raw, err)
			}
		}
		for name, output := range map[string]string{
			"not an object":                   `[1]`,
			"a string":                        `"12 rows"`,
			"no kind":                         `{"count":1}`,
			"an unknown kind":                 `{"kind":"table","rows":[],"total":0}`,
			"rows with no total":              `{"kind":"rows","rows":[]}`,
			"rows with no rows":               `{"kind":"rows","total":0}`,
			"rows of null":                    `{"kind":"rows","rows":null,"total":0}`,
			"too many rows":                   rows(maxOutputRows + 1),
			"a row with no cells":             `{"kind":"rows","rows":[{"edges":[]}],"total":1}`,
			"a row with no edges":             `{"kind":"rows","rows":[{"cells":[]}],"total":1}`,
			"a row not an object":             `{"kind":"rows","rows":[[1]],"total":1}`,
			"a negative total":                `{"kind":"rows","rows":[],"total":-1}`,
			"a count with no count":           `{"kind":"count"}`,
			"a fractional count":              `{"kind":"count","count":1.5}`,
			"an aggregate with no value":      `{"kind":"aggregate","values":1}`,
			"an aggregate of text":            `{"kind":"aggregate","value":"12","values":1}`,
			"an aggregate with no values":     `{"kind":"aggregate","value":1}`,
			"groups with no total":            `{"kind":"group_count","groups":[]}`,
			"too many groups":                 groups(maxOutputGroups + 1),
			"a group with no value":           `{"kind":"group_count","groups":[{"count":1}],"total_groups":1}`,
			"a group with no count":           `{"kind":"group_count","groups":[{"value":"Gold"}],"total_groups":1}`,
			"an aggregate group of counts":    `{"kind":"grouped_aggregate","groups":[{"value":"Gold","count":1}],"total_groups":1}`,
			"an aggregate group of no values": `{"kind":"grouped_aggregate","groups":[{"value":"Gold","aggregate":1}],"total_groups":1}`,
			"a cut not text":                  `{"kind":"count","count":1,"cut":12}`,
		} {
			if _, err := DecodeAskResult([]byte(withOutput(output))); err == nil {
				t.Errorf("%s: decoded %s", name, output)
			}
		}
		// One no visual shows is refused all the same.
		unshown := strings.Replace(withOutputs(`{"evidence_id":"e2"}`), recordedOutputs[1].output, `{"kind":"count"}`, 1)
		if _, err := DecodeAskResult([]byte(unshown)); err == nil {
			t.Errorf("decoded %s", unshown)
		}
	})
}

func TestWithoutOutputs(t *testing.T) {
	// A list carries every answer but none of their outputs: everything else
	// is as stored, visuals included, still a result the contract allows.
	t.Run("leaves out the queries' outputs and nothing else", func(t *testing.T) {
		raw := withOutputs(everyVisual)
		got := WithoutOutputs(json.RawMessage(raw))
		// A listed answer's visuals name outputs it no longer has: the website
		// gets the question to draw them.
		if _, err := DecodeAskResult(got); err == nil || !strings.Contains(err.Error(), "whose output was not recorded") {
			t.Fatalf("a listed result's visuals still find their outputs: %v", err)
		}
		if err := contracttest.Validate(t, contracttest.Schema(t, "ask_result.json"), got); err != nil {
			t.Fatalf("%s does not match ask_result.json: %v", got, err)
		}
		var want, have map[string]any
		if err := json.Unmarshal([]byte(raw), &want); err != nil {
			t.Fatal(err)
		}
		for _, q := range want["queries"].([]any) {
			delete(q.(map[string]any), "output")
		}
		if err := json.Unmarshal(got, &have); err != nil || !reflect.DeepEqual(have, want) {
			t.Fatalf("listed = %s, %v", got, err)
		}
	})

	// A result with nothing to leave out, such as every 3.10.0 result, comes
	// back as it was stored, byte for byte; so does anything that isn't a
	// result. A usage's output tokens are not an output.
	t.Run("returns what has no output as it is", func(t *testing.T) {
		for _, raw := range []string{
			answered(citeEveryTarget), `{"status":"budget_exhausted","queries":[],` + askUsage + `}`, `nope`, ``, `null`, `[]`,
			`{"status":"failed","reason":"no_answer","queries":"output",` + askUsage + `}`,
		} {
			if got := WithoutOutputs(json.RawMessage(raw)); string(got) != raw {
				t.Fatalf("%s came back as %s", raw, got)
			}
		}
	})

	// The text that stays is as stored: markup-like text in an answer, data
	// to the website, is not escaped.
	t.Run("keeps the rest's text as it was", func(t *testing.T) {
		raw := strings.Replace(withOutputs(`{"evidence_id":"e2"}`), `"para":"Acme leads [c1]:"`, `"para":"A & B <x> [c1]:"`, 1)
		if got := WithoutOutputs(json.RawMessage(raw)); !strings.Contains(string(got), `"A & B <x> [c1]:"`) {
			t.Fatalf("listed as %s", got)
		}
	})

	// "output" in an answer's text, or in a query, isn't an output: only the
	// queries' own outputs are left out.
	t.Run("leaves the word output where it is not one", func(t *testing.T) {
		raw := strings.Replace(answered(`{"marker":"c1","evidence_id":"e1","target":{"entity":{"type":"Customer","id":"\"output\""}}}`),
			`"para":"Acme [c1]"`, `"para":"\"output\" [c1]"`, 1)
		got := WithoutOutputs(json.RawMessage(raw))
		var want, have any
		if json.Unmarshal([]byte(raw), &want) != nil || json.Unmarshal(got, &have) != nil || !reflect.DeepEqual(have, want) {
			t.Fatalf("%s came back as %s", raw, got)
		}
	})
}

func TestDecodeAskEvent(t *testing.T) {
	failed := "no type `Planet`"
	// Each event the agent streams decodes. A tool result carries the
	// evidence a read was recorded as, or why a call failed, or, for the
	// answer that ends the question, neither.
	t.Run("reads every event the agent sends", func(t *testing.T) {
		schema := contracttest.Schema(t, "ask_event.json")
		for raw, want := range map[string]AskEvent{
			`{"event":"started","model":"claude-sonnet-5-5","provider":"anthropic"}`: AskStartedEvent{
				Model: ModelRef{Provider: KindAnthropic, Name: "claude-sonnet-5-5"},
			},
			`{"event":"tool_call","call_id":"c1","tool":"query","input":{"start":{"type":"Customer"}}}`: AskToolCallEvent{
				CallID: "c1", Tool: "query", Input: json.RawMessage(`{"start":{"type":"Customer"}}`),
			},
			`{"event":"tool_result","call_id":"c1","summary":"2 rows","evidence_id":"e1"}`: AskToolResultEvent{
				CallID: "c1", Summary: "2 rows", EvidenceID: "e1",
			},
			"{\"event\":\"tool_result\",\"call_id\":\"c2\",\"summary\":\"find_entities\",\"error\":\"no type `Planet`\"}": AskToolResultEvent{
				CallID: "c2", Summary: "find_entities", Error: &failed,
			},
			`{"event":"tool_result","call_id":"c3","summary":"answer: ended the question"}`: AskToolResultEvent{
				CallID: "c3", Summary: "answer: ended the question",
			},
			`{"event":"finished"}`: AskFinishedEvent{},
		} {
			if err := contracttest.Validate(t, schema, []byte(raw)); err != nil {
				t.Fatalf("%s does not match ask_event.json: %v", raw, err)
			}
			got, err := DecodeAskEvent([]byte(raw))
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: %+v, %v", raw, got, err)
			}
		}
	})

	// The schema check has teeth: a tool result names its call and summary.
	t.Run("the schema refuses a result of no call", func(t *testing.T) {
		raw := `{"event":"tool_result","summary":"s","evidence_id":"e1"}`
		if err := contracttest.Validate(t, contracttest.Schema(t, "ask_event.json"), []byte(raw)); err == nil {
			t.Fatalf("ask_event.json accepted %s", raw)
		}
	})

	// An event the worker can't vouch for ends the attempt rather than be
	// stored for the website.
	t.Run("refuses an event the contract doesn't allow", func(t *testing.T) {
		for name, raw := range map[string]string{
			"no kind":                        `{}`,
			"an unknown kind":                `{"event":"thinking"}`,
			"a start with no model":          `{"event":"started"}`,
			"a start with no provider":       `{"event":"started","model":"claude-sonnet-5-5"}`,
			"a start of an unknown provider": `{"event":"started","model":"m","provider":"gemini"}`,
			"a call with no id":              `{"event":"tool_call","tool":"query","input":{}}`,
			"a call with no tool":            `{"event":"tool_call","call_id":"c1","input":{}}`,
			"a call with no input":           `{"event":"tool_call","call_id":"c1","tool":"query"}`,
			"a call with a null input":       `{"event":"tool_call","call_id":"c1","tool":"query","input":null}`,
			"a result with no call":          `{"event":"tool_result","evidence_id":"e1","summary":""}`,
			"a result with no summary":       `{"event":"tool_result","call_id":"c1","evidence_id":"e1"}`,
			"a result of empty evidence":     `{"event":"tool_result","call_id":"c1","summary":"s","evidence_id":""}`,
			"evidence and an error at once":  `{"event":"tool_result","call_id":"c1","summary":"s","evidence_id":"e1","error":"no"}`,
			"an array":                       `[]`,
			"a string":                       `"started"`,
			"not json":                       `nope`,
		} {
			if _, err := DecodeAskEvent([]byte(raw)); err == nil {
				t.Errorf("%s: decoded %s", name, raw)
			}
		}
	})
}

func TestAnswerText(t *testing.T) {
	// A thread's earlier answers reach the agent as plain text, markers kept.
	t.Run("writes the blocks as plain text", func(t *testing.T) {
		result := json.RawMessage(`{"status":"answered","answer":{"blocks":[{"para":"Two customers [c1]."},{"list":["Acme [c2]","Globex"]},` +
			`{"para":"Done."}]},"citations":[],"queries":[],` + askUsage + `}`)
		if got, want := AnswerText(result), "Two customers [c1].\n\n- Acme [c2]\n- Globex\n\nDone."; got != want {
			t.Fatalf("text = %q, want %q", got, want)
		}
	})

	// A visual's rows are often not in the text, so the history says, after
	// the block each visual follows, what it showed and of which query: a
	// later question can refer to its rows, and the agent read them again.
	// The outputs stay out.
	t.Run("says what each visual showed, after its block", func(t *testing.T) {
		got := AnswerText(json.RawMessage(withOutputs(everyVisual)))
		shown := func(what string) string {
			return "[Shown here, not in the text: " + what + " of the result of the query " + askQuery + "]"
		}
		want := strings.Join([]string{
			"Acme leads [c1]:", shown(`a table titled "Customers",`),
			"- Acme [c1]\n- Globex [c2]", shown("a bar chart"),
			"By month:", shown(`a line chart titled "` + strings.Repeat("é", MaxVisualTitleChars) + `",`), shown("a figure"),
		}, "\n\n")
		if got != want {
			t.Fatalf("text = %q, want %q", got, want)
		}
		if strings.Contains(got, "Gold") || strings.Contains(got, "2026-09") {
			t.Fatalf("text %q carries an output", got)
		}
	})

	// A query as stored, its spacing perhaps changed, comes back compact,
	// and past maxHistoryQueryChars cut with an ellipsis; a visual of no
	// kind is a table or a chart, and a title is quoted.
	t.Run("writes a visual's query compact and bounded", func(t *testing.T) {
		long := `{"start": {"type": "` + strings.Repeat("x", maxHistoryQueryChars) + `"}}`
		raw := strings.Replace(withOutputs(`{"evidence_id":"e2","title":"Say \"hi\""}`), `"evidence_id":"e2","query":`+askQuery, `"evidence_id":"e2","query":`+long, 1)
		got := AnswerText(json.RawMessage(raw))
		query := `{"start":{"type":"` + strings.Repeat("x", maxHistoryQueryChars-len(`{"start":{"type":"`)) + "…"
		want := "By month:\n\n[Shown here, not in the text: a table or a chart titled \"Say \\\"hi\\\"\", of the result of the query " + query + "]"
		if !strings.HasSuffix(got, want) {
			t.Fatalf("text = %q, want it to end %q", got, want)
		}
	})

	t.Run("is empty without an answer", func(t *testing.T) {
		if AnswerText(json.RawMessage(`{"status":"budget_exhausted","queries":[],`+askUsage+`}`)) != "" || AnswerText(json.RawMessage(`nope`)) != "" {
			t.Fatal("no answer is no text")
		}
	})
}

func ExampleDecodeAskEvent() {
	e, err := DecodeAskEvent([]byte(`{"event":"tool_result","call_id":"c1","evidence_id":"e1","summary":"2 rows"}`))
	fmt.Println(e.Kind(), e.(AskToolResultEvent).EvidenceID, err)
	e, err = DecodeAskEvent([]byte(`{"event":"tool_result","call_id":"c2","summary":"answer: ended the question"}`))
	fmt.Printf("%q %v\n", e.(AskToolResultEvent).EvidenceID, err)
	_, err = DecodeAskEvent([]byte(`{"event":"tool_result","call_id":"c3","evidence_id":"e2","error":"refused","summary":""}`))
	fmt.Println(err)
	// Output:
	// tool_result e1 <nil>
	// "" <nil>
	// tool_result event has both an evidence id and an error
}

func ExampleDecodeAskResult() {
	r, err := DecodeAskResult([]byte(`{"status":"answered","answer":{"blocks":[{"para":"Acme signed [c1]."}]},` +
		`"citations":[{"marker":"c1","evidence_id":"e1","target":{"row":{"source":"src_1","mapping":"contracts","row_key":"k1"}}}],` +
		`"queries":[],"usage":{"input":1200,"cache_read":0,"cache_write":0,"output":300,"usd":0.0081}}`))
	fmt.Println(r.Status, r.Citations[0].Target.Row.RowKey, r.Usage.USD, err)
	r, err = DecodeAskResult([]byte(`{"status":"failed","reason":"load_unknown","queries":[],` +
		`"usage":{"input":0,"cache_read":0,"cache_write":0,"output":0,"usd":0}}`))
	fmt.Println(r.Status, r.Failure, err)
	// Output:
	// answered k1 0.0081 <nil>
	// failed load_unknown <nil>
}

func ExampleWithoutOutputs() {
	listed := WithoutOutputs(json.RawMessage(`{"status":"answered","answer":{"blocks":[{"para":"42 customers [c1]."}],` +
		`"visuals":[{"evidence_id":"e1","kind":"figure"}]},"citations":[{"marker":"c1","evidence_id":"e1","target":{"result":{"evidence_id":"e1"}}}],` +
		`"queries":[{"evidence_id":"e1","query":{},"output":{"kind":"count","count":42}}],` +
		`"usage":{"input":0,"cache_read":0,"cache_write":0,"output":0,"usd":0}}`))
	var r struct {
		Answer  AskAnswer  `json:"answer"`
		Queries []AskQuery `json:"queries"`
	}
	err := json.Unmarshal(listed, &r)
	fmt.Println(r.Answer.Visuals[0].EvidenceID, r.Queries[0].EvidenceID, r.Queries[0].Output == nil, err)
	// Output:
	// e1 e1 true <nil>
}

func ExampleParseFocus() {
	focus, err := ParseFocus(`{"type":"Customer","id":"scus_1"}`)
	fmt.Println(focus.Type, focus.ID, err)
	_, err = ParseFocus(`{"type":"Customer","key":"scus_1"}`)
	fmt.Println(err)
	// Output:
	// Customer scus_1 <nil>
	// a focus is {"type": ..., "id": ...}, both set
}

func ExampleAnswerText() {
	fmt.Println(AnswerText(json.RawMessage(`{"status":"answered","answer":{"blocks":[{"para":"Two signed [c1]:"},` +
		`{"list":["Acme [c2]","Globex [c3]"]}],"visuals":[{"evidence_id":"e1","after":0,"kind":"bar","title":"Signed by month"}]},"citations":[],` +
		`"queries":[{"evidence_id":"e1","query":{"start":{"type":"Contract"}},"output":{"kind":"count","count":2}}],` +
		`"usage":{"input":0,"cache_read":0,"cache_write":0,"output":0,"usd":0}}`)))
	// Output:
	// Two signed [c1]:
	//
	// [Shown here, not in the text: a bar chart titled "Signed by month", of the result of the query {"start":{"type":"Contract"}}]
	//
	// - Acme [c2]
	// - Globex [c3]
}

func ExampleTableSourceID() {
	fmt.Println(TableSourceID("conn-1", "customers"))
	// Output: src_093a2e7cf27f9d93
}
