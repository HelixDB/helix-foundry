package ontologyagent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// testIngestRequest is an Ingest the graph worker could send.
func testIngestRequest() IngestRequest {
	return IngestRequest{
		RequestID: "job-1", Database: "tenant-1", Load: "01JDLOAD0000000000000000AB", OntologyTOML: "version = \"draft\"\n",
		OntologyVersion: "foundry-v3",
		Helix:           HelixTarget{URL: "http://127.0.0.1:4000", Token: "secret", MaxRequestBytes: 2 << 20, TimeoutSecs: 60},
		MappingsTOML:    "ontology = \"draft\"\n", ChunkRows: 1000,
		// Tables a sync observed, one before any change batch; a table file
		// and its upload; a table pinned before pins kept their sync; the profiles.
		Sources: []Source{
			{ID: "src_0123456789abcdef", DisplayName: "Stripe / customers", Path: "cache/k.jsonl", Fingerprint: "k",
				Observed: ObservedSync{At: time.Date(2026, 10, 20, 7, 0, 0, 123456000, time.UTC), SyncID: "01JDSYNC000000000000000000", ChangeSeq: 42}},
			{ID: "src_fedcba9876543210", DisplayName: "Stripe / refunds", Path: "cache/r.jsonl", Fingerprint: "r",
				Observed: ObservedSync{At: time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC), SyncID: "01JDSYNC000000000000000000"}},
			{ID: "01JDDOCCSV0000000000000000", DisplayName: "Exports / orders.csv", Path: "cache/o.jsonl", Fingerprint: "o",
				Observed: ObservedUpload{DocumentID: "01JDDOCCSV0000000000000000", UploadedAt: time.Date(2026, 10, 19, 12, 30, 0, 0, time.UTC)}},
			{ID: "src_0000000000000000", DisplayName: "Stripe / invoices", Path: "cache/i.jsonl", Fingerprint: "i"},
			EnrichmentSource("jobs/job-1/enrichment/foundry_enrichment.jsonl", "e"),
		},
	}
}

// decidedAt is when the test decisions were settled, as MySQL keeps it: to
// the microsecond.
var decidedAt = time.Date(2026, 10, 20, 8, 30, 0, 123456000, time.UTC)

// testDeleteLoadRequest is a DeleteLoad the graph worker could send.
func testDeleteLoadRequest() DeleteLoadRequest {
	return DeleteLoadRequest{
		RequestID: "job-2", Database: "tenant-1", Load: "01JDLOAD0000000000000000AB", Batch: 1000,
		Helix: HelixTarget{URL: "http://127.0.0.1:4000", Token: "secret", MaxRequestBytes: 2 << 20, TimeoutSecs: 60},
	}
}

// testExtractRequest is an Extract the graph worker could send.
func testExtractRequest() ExtractRequest {
	return ExtractRequest{
		RequestID: "job-1", Database: "tenant-1", Load: "01JDLOAD0000000000000000AB", OntologyTOML: "version = \"draft\"\n",
		OntologyVersion: "foundry-v3", MappingsTOML: "[extractions.contracts]\ndocuments = [\"01JDSETCONTRACTS0000000000\"]\n",
		Helix: HelixTarget{URL: "http://127.0.0.1:4000", Token: "secret", MaxRequestBytes: 2 << 20, TimeoutSecs: 60},
		Documents: []ExtractDocument{
			{DocID: "doc-1", Set: "01JDSETCONTRACTS0000000000", URI: "s3://bucket/ws/w/documents/doc-1/original", Path: "jobs/job-1/docs/doc-1.md",
				MediaType: MediaMarkdown, Artifacts: []string{"jobs/job-1/artifacts/doc-1/abc.json", "jobs/job-1/artifacts/doc-1/def.json"}},
			{DocID: "doc-2", Set: "01JDSETGENERAL000000000000", URI: "s3://bucket/ws/w/documents/doc-2/original", Path: "jobs/job-1/docs/doc-2.txt",
				MediaType: MediaPlainText, Artifacts: []string{}},
		},
		Limits: ExtractLimits{MaxUSD: 20, DocumentsInFlight: 4, MaxTextBytes: 256 << 10}, Models: houseExtractModels(),
		Decisions: []ExtractDecision{
			{Kind: model.FoundryReviewResolve, Type: "Customer", DocID: "doc-1", NormName: "acme corp", EntityKey: nil,
				DecisionID: "01JDDEC0000000000000000001", DecidedAt: decidedAt},
			{Kind: model.FoundryReviewProp, Type: "Customer", EntityKey: &testEntityKey, Prop: "tier", Value: json.RawMessage(`"gold"`),
				DecisionID: "01JDDEC0000000000000000002", DecidedAt: decidedAt.Add(time.Minute)},
		},
	}
}

var testEntityKey = "scus_1"

// testCounts are a document's counts as the agent sends them.
const testCounts = `{"entities":2,"mentions":3,"facts":1,"reviews":1,"unresolved":1,"rejected":0,"attributes":4,"attribute_reviews":1,` +
	`"prop_conflicts":1,"created":1,"identifiers_unverified":2,"attributes_over_entries":3,` +
	`"resolved":{"user":0,"adjudicated":0,"identifier":1,"name":1,"alias":0,"created":1}}`

func TestGraphContract(t *testing.T) {
	// The requests the worker builds are ones the agent takes.
	t.Run("builds requests the schemas accept", func(t *testing.T) {
		for name, tc := range map[string]struct {
			schema  string
			request any
		}{
			"ingest":      {"ingest_request.json", testIngestRequest()},
			"extract":     {"extract_request.json", testExtractRequest()},
			"delete load": {"delete_load_request.json", testDeleteLoadRequest()},
		} {
			raw, err := json.Marshal(tc.request)
			if err != nil {
				t.Fatal(err)
			}
			if err := contracttest.Validate(t, contracttest.Schema(t, tc.schema), raw); err != nil {
				t.Fatalf("%s request %s: %v", name, raw, err)
			}
		}
	})

	// A request missing what the agent needs is one the schemas refuse, so the check above means something.
	t.Run("the request schemas refuse an incomplete request", func(t *testing.T) {
		if err := contracttest.Validate(t, contracttest.Schema(t, "ingest_request.json"), []byte(`{"request_id":"j"}`)); err == nil {
			t.Fatal("the ingest schema took a request with no database")
		}
		bad := testExtractRequest()
		bad.Documents[0].MediaType = "pdf"
		raw, _ := json.Marshal(bad)
		if err := contracttest.Validate(t, contracttest.Schema(t, "extract_request.json"), raw); err == nil {
			t.Fatal("the extract schema took a pdf")
		}
		// The routes and a document's set scope what it may fill: neither may go missing.
		var unscoped map[string]any
		raw, _ = json.Marshal(testExtractRequest())
		_ = json.Unmarshal(raw, &unscoped)
		delete(unscoped, "mappings_toml")
		raw, _ = json.Marshal(unscoped)
		if err := contracttest.Validate(t, contracttest.Schema(t, "extract_request.json"), raw); err == nil {
			t.Fatal("the extract schema took a request with no mappings")
		}
		raw, _ = json.Marshal(testExtractRequest())
		_ = json.Unmarshal(raw, &unscoped)
		delete(unscoped["documents"].([]any)[0].(map[string]any), "set")
		raw, _ = json.Marshal(unscoped)
		if err := contracttest.Validate(t, contracttest.Schema(t, "extract_request.json"), raw); err == nil {
			t.Fatal("the extract schema took a document with no set")
		}
	})

	// A decision names itself by its id and time together or not at all; the
	// agent refuses the whole Extract over one alone.
	t.Run("the extract schema refuses a decision half named", func(t *testing.T) {
		schema := contracttest.Schema(t, "extract_request.json")
		for i := range testExtractRequest().Decisions {
			for _, field := range []string{"decision_id", "decided_at"} {
				var request map[string]any
				raw, _ := json.Marshal(testExtractRequest())
				_ = json.Unmarshal(raw, &request)
				delete(request["decisions"].([]any)[i].(map[string]any), field)
				raw, _ = json.Marshal(request)
				if err := contracttest.Validate(t, schema, raw); err == nil {
					t.Errorf("decision %d without %s: extract_request.json took it", i, field)
				}
			}
		}
		// Neither is a decision as earlier contracts sent it, which the agent still takes.
		var request map[string]any
		raw, _ := json.Marshal(testExtractRequest())
		_ = json.Unmarshal(raw, &request)
		for _, d := range request["decisions"].([]any) {
			delete(d.(map[string]any), "decision_id")
			delete(d.(map[string]any), "decided_at")
		}
		raw, _ = json.Marshal(request)
		if err := contracttest.Validate(t, schema, raw); err != nil {
			t.Fatalf("decisions named by neither: %v", err)
		}
	})

	// observed is a sync or an upload, each whole; the agent refuses anything else.
	t.Run("the ingest schema refuses an observed that isn't whole", func(t *testing.T) {
		schema := contracttest.Schema(t, "ingest_request.json")
		for name, observed := range map[string]string{
			"a sync with no change_seq": `{"sync":{"at":"2026-10-20T07:30:00Z","sync_id":"s1"}}`,
			"a negative change_seq":     `{"sync":{"at":"2026-10-20T07:30:00Z","sync_id":"s1","change_seq":-1}}`,
			"a sync with no time":       `{"sync":{"sync_id":"s1","change_seq":1}}`,
			"an upload with no id":      `{"upload":{"uploaded_at":"2026-10-20T07:30:00Z"}}`,
			"an unknown kind":           `{"batch":{"seq":1}}`,
			"both kinds": `{"sync":{"at":"2026-10-20T07:30:00Z","sync_id":"s1","change_seq":1},` +
				`"upload":{"document_id":"d","uploaded_at":"2026-10-20T07:30:00Z"}}`,
			"a sync with an extra field": `{"sync":{"at":"2026-10-20T07:30:00Z","sync_id":"s1","change_seq":1,"rows":3}}`,
		} {
			var request map[string]any
			raw, _ := json.Marshal(testIngestRequest())
			_ = json.Unmarshal(raw, &request)
			var value any
			_ = json.Unmarshal([]byte(observed), &value)
			request["sources"].([]any)[0].(map[string]any)["observed"] = value
			raw, _ = json.Marshal(request)
			if err := contracttest.Validate(t, schema, raw); err == nil {
				t.Errorf("%s: ingest_request.json took %s", name, observed)
			}
		}
	})

	// A load names what every node and edge carries; one the agent would refuse is one the schemas refuse.
	t.Run("the request schemas refuse a missing or unusable load", func(t *testing.T) {
		for _, load := range []string{"", "has space", "nön-ascii"} {
			ingest, extract, del := testIngestRequest(), testExtractRequest(), testDeleteLoadRequest()
			ingest.Load, extract.Load, del.Load = load, load, load
			for schema, request := range map[string]any{"ingest_request.json": ingest, "extract_request.json": extract,
				"delete_load_request.json": del} {
				raw, _ := json.Marshal(request)
				if err := contracttest.Validate(t, contracttest.Schema(t, schema), raw); err == nil {
					t.Fatalf("%s took load %q", schema, load)
				}
			}
		}
		for _, batch := range []uint64{0, MaxDeleteBatch + 1} {
			del := testDeleteLoadRequest()
			del.Batch = batch
			raw, _ := json.Marshal(del)
			if err := contracttest.Validate(t, contracttest.Schema(t, "delete_load_request.json"), raw); err == nil {
				t.Fatalf("delete_load_request.json took batch %d", batch)
			}
		}
	})

	// Each ingest result the agent may send is one the schema allows, and decodes to what the worker acts on.
	t.Run("reads every ingest result", func(t *testing.T) {
		schema := contracttest.Schema(t, "ingest_result.json")
		for raw, want := range map[string]IngestResult{
			`{"status":"done","report":{"chunks":3}}`:      {Status: IngestDone, Report: json.RawMessage(`{"chunks":3}`)},
			`{"status":"cancelled","report":{"chunks":1}}`: {Status: IngestCancelled, Report: json.RawMessage(`{"chunks":1}`)},
			`{"status":"sources_changed","streams":[{"stream":"s\u001fm","stored":"a","stored_version":"foundry-v1","snapshot":"b","version":"foundry-v2"}]}`: {
				Status: IngestSourcesChanged, Streams: []SourceChange{{Stream: "s\x1fm", Stored: "a", StoredVersion: "foundry-v1", Snapshot: "b", Version: "foundry-v2"}},
			},
			`{"status":"invalid_mappings","errors":["customers: no column email"]}`: {Status: IngestInvalidMappings, Errors: []string{"customers: no column email"}},
			`{"status":"source_unavailable","source":"src_1"}`:                      {Status: IngestSourceUnavailable, Source: "src_1"},
			`{"status":"outcome_unknown","mapping":"customers","pass":"create","chunk":2,"error":"timeout"}`: {
				Status: IngestOutcomeUnknown, Mapping: "customers", Pass: "create", Chunk: 2, Error: "timeout",
			},
			`{"status":"store_failed","error":"refused"}`:              {Status: IngestStoreFailed, Error: "refused"},
			`{"status":"load_mismatch","stored_version":"foundry-v3"}`: {Status: IngestLoadMismatch, StoredVersion: "foundry-v3"},
			`{"status":"load_unknown"}`:                                {Status: IngestLoadUnknown},
			`{"status":"indexes_pending","label":"Customer"}`:          {Status: IngestIndexesPending, Label: "Customer"},
		} {
			if err := contracttest.Validate(t, schema, []byte(raw)); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			got, err := DecodeIngestResult([]byte(raw))
			if err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("%s decoded to %s, want %s", raw, gotJSON, wantJSON)
			}
		}
	})

	// A result the worker can't act on is an error, never a guess.
	t.Run("refuses an ingest result it can't act on", func(t *testing.T) {
		for _, raw := range []string{
			`{"status":"exploded"}`, `{"status":"done"}`, `{"status":"sources_changed"}`, `{"status":"invalid_mappings"}`,
			`{"status":"source_unavailable"}`, `{"status":"outcome_unknown","mapping":"m"}`, `{"status":"store_failed"}`, `not json`,
			`{"status":"load_mismatch"}`, `{"status":"indexes_pending"}`,
		} {
			if _, err := DecodeIngestResult([]byte(raw)); err == nil {
				t.Fatalf("%s decoded", raw)
			}
		}
	})

	// Each extract result decodes; incomplete or unknown ones are refused.
	t.Run("reads every extract result", func(t *testing.T) {
		schema := contracttest.Schema(t, "extract_result.json")
		totals := `{"ingested":1,"out_of_scope":0,"already_ingested":0,"duplicate":0,"failed":0,"counts":` + testCounts + `,"swept":0,"unchecked":0,"stale_decisions":0,"usd_micros":5,"input_tokens":10,"output_tokens":2}`
		for raw, want := range map[string]ExtractStatus{
			`{"status":"done","totals":` + totals + `}`:                       ExtractDone,
			`{"status":"budget_exhausted","totals":` + totals + `}`:           ExtractBudgetExhausted,
			`{"status":"cancelled","totals":` + totals + `}`:                  ExtractCancelled,
			`{"status":"outcome_unknown","doc_id":"doc-1","error":"timeout"}`: ExtractOutcomeUnknown,
			`{"status":"store_failed","error":"refused"}`:                     ExtractStoreFailed,
			`{"status":"load_mismatch","stored_version":"foundry-v3"}`:        ExtractLoadMismatch,
			`{"status":"load_unknown"}`:                                       ExtractLoadUnknown,
			`{"status":"indexes_pending","label":"Customer"}`:                 ExtractIndexesPending,
		} {
			if err := contracttest.Validate(t, schema, []byte(raw)); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			got, err := DecodeExtractResult([]byte(raw))
			if err != nil || got.Status != want {
				t.Fatalf("%s = (%+v, %v)", raw, got, err)
			}
		}
		got, _ := DecodeExtractResult([]byte(`{"status":"outcome_unknown","doc_id":"doc-1","error":"timeout"}`))
		if got.DocID != "doc-1" || got.Error != "timeout" {
			t.Fatalf("outcome_unknown = %+v", got)
		}
		if got, _ := DecodeExtractResult([]byte(`{"status":"load_mismatch","stored_version":"foundry-v3"}`)); got.StoredVersion != "foundry-v3" {
			t.Fatalf("load_mismatch = %+v", got)
		}
		if got, _ := DecodeExtractResult([]byte(`{"status":"indexes_pending","label":"Customer"}`)); got.Label != "Customer" {
			t.Fatalf("indexes_pending = %+v", got)
		}
		for _, raw := range []string{`{"status":"done"}`, `{"status":"outcome_unknown","doc_id":"d"}`, `{"status":"store_failed"}`, `{"status":"x"}`, `[]`,
			`{"status":"load_mismatch"}`, `{"status":"indexes_pending"}`} {
			if _, err := DecodeExtractResult([]byte(raw)); err == nil {
				t.Fatalf("%s decoded", raw)
			}
		}
	})

	// Events are what the schemas describe; the worker reads artifacts and documents.
	t.Run("reads extract events", func(t *testing.T) {
		schema := contracttest.Schema(t, "extract_event.json")
		counts := `"counts":` + testCounts
		usage := `"usage":{"usd_micros":1500,"input_tokens":900,"output_tokens":80,"replayed":false,"phases":{"entity":{"calls":1,"replayed":0,"usd_micros":1500,"input_tokens":900,"output_tokens":80},"link":{"calls":0,"replayed":0,"usd_micros":0,"input_tokens":0,"output_tokens":0},"verify":{"calls":0,"replayed":0,"usd_micros":0,"input_tokens":0,"output_tokens":0},"adjudicate":{"calls":0,"replayed":0,"usd_micros":0,"input_tokens":0,"output_tokens":0}}}`
		events := []string{
			`{"event":"extract_started","contract_version":"3.8.0","extractor":"claude-haiku-5-5","verifier":"gpt-6.1-sol","prompt_version":"7"}`,
			`{"event":"artifact","doc_id":"doc-1","key":"abc_DEF-1","json":"{\"sections\":[]}"}`,
			`{"event":"document","doc_id":"doc-1","outcome":{"kind":"ingested"},"text_bytes":120,"sections":2,` + counts + `,` + usage + `}`,
			`{"event":"document","doc_id":"doc-2","outcome":{"kind":"duplicate","of":"doc-1"},"text_bytes":120,"sections":0,` + counts + `,` + usage + `}`,
			`{"event":"document","doc_id":"doc-3","outcome":{"kind":"failed","code":"too_large","retryable":false},"text_bytes":0,"sections":0,` + counts + `,` + usage + `}`,
			`{"event":"document","doc_id":"doc-4","outcome":{"kind":"already_ingested"},"text_bytes":10,"sections":1,` + counts + `,` + usage + `}`,
			`{"event":"document","doc_id":"doc-5","outcome":{"kind":"out_of_scope"},"text_bytes":10,"sections":1,` + counts + `,` + usage + `}`,
			`{"event":"document","doc_id":"doc-6","outcome":{"kind":"failed","code":"model","model_failure":"auth","retryable":true},"text_bytes":10,"sections":0,` +
				counts + `,` + usage + `}`,
			`{"event":"extract_finished"}`,
		}
		for _, raw := range events {
			if err := contracttest.Validate(t, schema, []byte(raw)); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			if _, err := EventName([]byte(raw)); err != nil {
				t.Fatal(err)
			}
		}
		a, err := DecodeArtifact([]byte(events[1]))
		if err != nil || a.DocID != "doc-1" || a.Key != "abc_DEF-1" || a.JSON != `{"sections":[]}` {
			t.Fatalf("artifact = (%+v, %v)", a, err)
		}
		ingested, err := DecodeDocumentEvent([]byte(events[2]))
		if err != nil || ingested.Outcome.Kind != OutcomeIngested || ingested.Sections != 2 || ingested.Counts.Mentions != 3 ||
			ingested.Usage.USDMicros != 1500 || ingested.Usage.InputTokens != 900 {
			t.Fatalf("ingested = (%+v, %v)", ingested, err)
		}
		if want := (DocumentCounts{Entities: 2, Mentions: 3, Facts: 1, Reviews: 1, Unresolved: 1, Attributes: 4, AttributeReviews: 1,
			PropConflicts: 1, Created: 1, IdentifiersUnverified: 2, AttributesOverEntries: 3,
			Resolved: ResolvedCounts{Identifier: 1, Name: 1, Alias: 0, Created: 1}}); ingested.Counts != want {
			t.Fatalf("counts = %+v, want %+v", ingested.Counts, want)
		}
		outOfScope, err := DecodeDocumentEvent([]byte(events[6]))
		if err != nil || outOfScope.Outcome != (DocumentOutcome{Kind: OutcomeOutOfScope}) || outOfScope.DocID != "doc-5" {
			t.Fatalf("out_of_scope = (%+v, %v)", outOfScope, err)
		}
		dup, err := DecodeDocumentEvent([]byte(events[3]))
		if err != nil || dup.Outcome.Kind != OutcomeDuplicate || dup.Outcome.Of != "doc-1" {
			t.Fatalf("duplicate = (%+v, %v)", dup, err)
		}
		failed, err := DecodeDocumentEvent([]byte(events[4]))
		if err != nil || failed.Outcome.Code != "too_large" || failed.Outcome.Retryable || failed.Outcome.ModelFailure != "" {
			t.Fatalf("failed = (%+v, %v)", failed, err)
		}
		// A model's failure says how it failed, which the worker words for the owner.
		modelFailed, err := DecodeDocumentEvent([]byte(events[7]))
		if err != nil || modelFailed.Outcome != (DocumentOutcome{Kind: OutcomeFailed, Code: "model", ModelFailure: ModelAuth, Retryable: true}) {
			t.Fatalf("model failed = (%+v, %v)", modelFailed, err)
		}
		for _, raw := range []string{
			`{"event":"document","doc_id":"d","outcome":{"kind":"failed","code":"model","retryable":true}}`,
			`{"event":"document","doc_id":"d","outcome":{"kind":"failed","code":"model","model_failure":"tired","retryable":true}}`,
			`{"event":"document","doc_id":"d","outcome":{"kind":"failed","code":"store","model_failure":"auth","retryable":true}}`,
			`{"event":"document","doc_id":"d","outcome":{"kind":"duplicate"}}`,
			`{"event":"document","doc_id":"d","outcome":{"kind":"failed","code":"weird","retryable":true}}`,
			`{"event":"document","doc_id":"d","outcome":{"kind":"failed"}}`,
			`{"event":"document","doc_id":"d","outcome":{"kind":"vanished"}}`,
			`{"event":"document","outcome":{"kind":"ingested"}}`,
		} {
			if _, err := DecodeDocumentEvent([]byte(raw)); err == nil {
				t.Fatalf("%s decoded", raw)
			}
		}
		if _, err := DecodeArtifact([]byte(`{"event":"artifact","doc_id":"d"}`)); err == nil {
			t.Fatal("an artifact without its key decoded")
		}
		if _, err := EventName([]byte(`{"chunk":1}`)); err == nil {
			t.Fatal("an unnamed event read")
		}
	})

	// Ingest's events, chunk reports included, are what the schema allows.
	t.Run("ingest events are what the schema describes", func(t *testing.T) {
		schema := contracttest.Schema(t, "ingest_event.json")
		for _, raw := range []string{
			`{"event":"ingest_started","contract_version":"3.8.0"}`, `{"event":"indexes_ready"}`,
			`{"event":"chunk","report":{"mapping":"customers","pass":"create","rows":1000}}`, `{"event":"sweep_page","entities":50}`,
			`{"event":"ingest_finished"}`,
		} {
			if err := contracttest.Validate(t, schema, []byte(raw)); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			if name, err := EventName([]byte(raw)); err != nil || !strings.Contains(raw, name) {
				t.Fatalf("%s: name %q, %v", raw, name, err)
			}
		}
	})
}

func TestDeleteLoadContract(t *testing.T) {
	// Each result the agent may send is one the schema allows, and decodes to what the worker acts on.
	t.Run("reads every delete load result", func(t *testing.T) {
		schema := contracttest.Schema(t, "delete_load_result.json")
		for raw, want := range map[string]DeleteLoadResult{
			`{"status":"done","nodes":120344,"edges":402110}`:                   {Status: DeleteLoadDone, Nodes: 120344, Edges: 402110},
			`{"status":"done","nodes":0,"edges":0}`:                             {Status: DeleteLoadDone},
			`{"status":"cancelled","nodes":10,"edges":20}`:                      {Status: DeleteLoadCancelled, Nodes: 10, Edges: 20},
			`{"status":"outcome_unknown","label":"Customer","error":"timeout"}`: {Status: DeleteLoadOutcomeUnknown, Label: "Customer", Error: "timeout"},
			`{"status":"store_failed","error":"refused"}`:                       {Status: DeleteLoadStoreFailed, Error: "refused"},
		} {
			if err := contracttest.Validate(t, schema, []byte(raw)); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			got, err := DecodeDeleteLoadResult([]byte(raw))
			if err != nil || got != want {
				t.Fatalf("%s = (%+v, %v), want %+v", raw, got, err, want)
			}
		}
	})

	// A result the worker can't act on is an error, never a guess: a delete taken for done would forget a load still in the database.
	t.Run("refuses a delete load result it can't act on", func(t *testing.T) {
		for _, raw := range []string{
			`{"status":"done"}`, `{"status":"done","nodes":1}`, `{"status":"cancelled","edges":1}`, `{"status":"outcome_unknown","label":"L"}`,
			`{"status":"store_failed"}`, `{"status":"gone"}`, `not json`,
		} {
			if _, err := DecodeDeleteLoadResult([]byte(raw)); err == nil {
				t.Fatalf("%s decoded", raw)
			}
		}
	})

	// Events are what the schema describes and each names itself.
	t.Run("delete load events are what the schema describes", func(t *testing.T) {
		schema := contracttest.Schema(t, "delete_load_event.json")
		for _, raw := range []string{
			`{"event":"delete_started","contract_version":"3.8.0"}`, `{"event":"deleted","label":"OWNS","element":"edge","count":1000}`,
			`{"event":"deleted","label":"Customer","element":"node","count":3}`, `{"event":"delete_finished"}`,
		} {
			if err := contracttest.Validate(t, schema, []byte(raw)); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			if name, err := EventName([]byte(raw)); err != nil || !strings.Contains(raw, name) {
				t.Fatalf("%s: name %q, %v", raw, name, err)
			}
		}
	})
}

// testIngestReport is a done Ingest's report as the ingest crate writes it:
// contracts open one violation per pass and resolve one, and the sweep of
// what the load created opens one more and updates one.
const testIngestReport = `{"mappings":[` +
	`{"mapping":"customers","source":"src_1","snapshot":"s1","rows":2,` +
	`"create":{"loaded":1,"skipped":0,"empty":0,"written":{"entities":2,"mentions":0,"facts_created":0,"facts_updated":0,"evidence":0,"reviews":0},` +
	`"row_errors":{"count":0,"examples":[]},"optional_skipped":{},"reviews":{},"missing_entities":{"count":0,"examples":[]},` +
	`"conflicting_props":{"count":0,"examples":[]},"violations_opened":{"count":0,"examples":[]},"violations_resolved":{"count":0,"examples":[]}},` +
	`"resolve":{"loaded":0,"skipped":0,"empty":1,"written":{"entities":0,"mentions":0,"facts_created":0,"facts_updated":0,"evidence":0,"reviews":0},` +
	`"row_errors":{"count":0,"examples":[]},"optional_skipped":{},"reviews":{},"missing_entities":{"count":0,"examples":[]},` +
	`"conflicting_props":{"count":0,"examples":[]},"violations_opened":{"count":0,"examples":[]},"violations_resolved":{"count":0,"examples":[]}}},` +
	`{"mapping":"contracts","source":"src_2","snapshot":"s2","rows":3,` +
	`"create":{"loaded":1,"skipped":0,"empty":0,"written":{"entities":3,"mentions":0,"facts_created":0,"facts_updated":0,"evidence":0,"reviews":0},` +
	`"row_errors":{"count":0,"examples":[]},"optional_skipped":{},"reviews":{},"missing_entities":{"count":0,"examples":[]},` +
	`"conflicting_props":{"count":0,"examples":[]},` +
	`"violations_opened":{"count":1,"examples":[{"constraint":"unsigned","severity":"warn","entities":[{"type":"Contract","key":"sC1"}]}]},` +
	`"violations_resolved":{"count":0,"examples":[]}},` +
	`"resolve":{"loaded":2,"skipped":0,"empty":0,"written":{"entities":0,"mentions":0,"facts_created":4,"facts_updated":0,"evidence":4,"reviews":0},` +
	`"row_errors":{"count":0,"examples":[]},"optional_skipped":{},"reviews":{},"missing_entities":{"count":0,"examples":[]},` +
	`"conflicting_props":{"count":0,"examples":[]},` +
	`"violations_opened":{"count":2,"examples":[{"constraint":"one_party","severity":"warn","entities":[{"type":"Contract","key":"sC2"}]},` +
	`{"constraint":"one_party","severity":"warn","entities":[{"type":"Contract","key":"sC3"}]}]},` +
	`"violations_resolved":{"count":1,"examples":[{"constraint":"unsigned","severity":"warn","entities":[{"type":"Contract","key":"sC1"}]}]}}}],` +
	`"created":{"sweep":"swept","entities":5,` +
	`"opened":{"count":1,"examples":[{"constraint":"governed_by.required","severity":"warn","entities":[{"type":"Contract","key":"sC2"}]}]},` +
	`"updated":{"count":1,"examples":[{"constraint":"one_party","severity":"warn","entities":[{"type":"Contract","key":"sC3"}]}]},` +
	`"closed":{"count":0,"examples":[]},"self_loops":{"count":0,"examples":[]},"unchecked":{"count":0,"examples":[]}}}`

func TestIngestViolations(t *testing.T) {
	edit := func(from, to string, n int) string { return strings.Replace(testIngestReport, from, to, n) }
	for _, tc := range []struct {
		name, report string
		want         *uint64
	}{
		// 0+0 and 1+2 opened, 1 resolved; the sweep opens 1 and updates 1: 3 open.
		{"every pass and the sweep", testIngestReport, new(uint64(3))},
		{"the sweep closes what passes opened", edit(`"closed":{"count":0`, `"closed":{"count":2`, 1), new(uint64(1))},
		{"a load with no violations", `{"mappings":[],"created":{"sweep":"swept","entities":0,"opened":{"count":0,"examples":[]},"closed":{"count":0,"examples":[]}}}`, new(uint64(0))},
		// A retry skips the chunks an earlier run wrote and doesn't count their records.
		{"a pass skipped chunks", edit(`"skipped":0`, `"skipped":1`, 1), nil},
		{"the sweep ran in an earlier call", `{"mappings":[],"created":{"sweep":"already_swept"}}`, nil},
		{"the sweep stopped", edit(`"sweep":"swept"`, `"sweep":"stopped"`, 1), nil},
		{"more closed than opened", edit(`"closed":{"count":0`, `"closed":{"count":4`, 1), nil},
		{"a pass without its count", edit(`"violations_resolved":{"count":1,`, `"violations_resolved":{`, 1), nil},
		{"a pass without its skips", edit(`"skipped":0,`, ``, 1), nil},
		{"no mappings", `{"created":{"sweep":"swept","opened":{"count":0},"closed":{"count":0}}}`, nil},
		{"a negative count", edit(`"opened":{"count":1`, `"opened":{"count":-1`, 1), nil},
		{"not a report", `{"chunks":2}`, nil},
		{"not JSON", `{`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := IngestViolations(json.RawMessage(tc.report))
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("IngestViolations = %v, want %v", got, tc.want)
			}
		})
	}
}

func ExampleIngestViolations() {
	report := `{"mappings":[{"mapping":"contracts",` +
		`"create":{"skipped":0,"violations_opened":{"count":1,"examples":[]},"violations_resolved":{"count":0,"examples":[]}},` +
		`"resolve":{"skipped":0,"violations_opened":{"count":2,"examples":[]},"violations_resolved":{"count":1,"examples":[]}}}],` +
		`"created":{"sweep":"swept","opened":{"count":1,"examples":[]},"closed":{"count":0,"examples":[]}}}`
	fmt.Println(*IngestViolations(json.RawMessage(report)))
	// A retry that skipped a chunk an earlier attempt wrote doesn't count that chunk's records.
	retried := strings.Replace(report, `"skipped":0`, `"skipped":1`, 1)
	fmt.Println(IngestViolations(json.RawMessage(retried)))
	// Output:
	// 3
	// <nil>
}
func TestExtractDecisionWire(t *testing.T) {
	key := "customer-1"
	const named = `"decision_id":"01JDDEC","decided_at":"2026-10-20T08:30:00.123456Z"`
	for _, tc := range []struct {
		decision ExtractDecision
		want     string
	}{
		{ExtractDecision{Kind: model.FoundryReviewResolve, Type: "Customer", DocID: "doc", NormName: "variant", EntityKey: &key, DecisionID: "01JDDEC", DecidedAt: decidedAt},
			`{"kind":"resolve","type":"Customer","doc_id":"doc","norm_name":"variant","entity_key":"customer-1",` + named + `}`},
		{ExtractDecision{Kind: model.FoundryReviewResolve, Type: "Customer", DocID: "doc", NormName: "variant", DecisionID: "01JDDEC", DecidedAt: decidedAt},
			`{"kind":"resolve","type":"Customer","doc_id":"doc","norm_name":"variant","entity_key":null,` + named + `}`},
		{ExtractDecision{Kind: model.FoundryReviewProp, Type: "Customer", EntityKey: &key, Prop: "name", Value: json.RawMessage(`null`), DecisionID: "01JDDEC", DecidedAt: decidedAt},
			`{"kind":"prop","type":"Customer","entity_key":"customer-1","prop":"name","value":null,` + named + `}`},
	} {
		got, err := json.Marshal(tc.decision)
		if err != nil || string(got) != tc.want {
			t.Fatalf("%s %v, want %s", got, err, tc.want)
		}
	}
	body, err := json.Marshal(ExtractRequest{Models: houseExtractModels()})
	if err != nil || strings.Contains(string(body), `"decisions"`) {
		t.Fatalf("empty optional decisions: %s %v", body, err)
	}
}

// What a load did with the profiles is read from its expansion mapping
// alone; the work history's misses are roles, not people.
func TestEnrichmentApplied(t *testing.T) {
	report := `{"mappings":[{"mapping":"contacts","rows":9,"create":{"unmatched":0},"resolve":{"unmatched":0}},` +
		`{"mapping":"enrichment_people","rows":412,"create":{"unmatched":0},"resolve":{"unmatched":3}},` +
		`{"mapping":"enrichment_people_experience","rows":1800,"create":{"unmatched":0},"resolve":{"unmatched":95}}],` +
		`"created":{"sweep":"swept"}}`
	if applied, unmatched, ok := EnrichmentApplied(json.RawMessage(report), "people"); !ok || applied != 409 || unmatched != 3 {
		t.Fatalf("applied %d, unmatched %d, ok %t", applied, unmatched, ok)
	}
	for name, raw := range map[string]string{
		"another item":          report,
		"no report":             `null`,
		"more misses than rows": `{"mappings":[{"mapping":"enrichment_staff","rows":1,"create":{"unmatched":1},"resolve":{"unmatched":1}}]}`,
		"an unreadable report":  `{"mappings":7}`,
	} {
		if _, _, ok := EnrichmentApplied(json.RawMessage(raw), "staff"); ok {
			t.Errorf("%s: read", name)
		}
	}
}
