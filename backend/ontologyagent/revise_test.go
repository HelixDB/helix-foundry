package ontologyagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

type revisionServer struct {
	agentv1.UnimplementedAgentServer
	got string
}

func (s *revisionServer) Revise(_ context.Context, req *agentv1.ReviseRequest) (*agentv1.ReviseResponse, error) {
	if req.RequestJson == "refuse" {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	s.got = req.RequestJson
	return &agentv1.ReviseResponse{ResultJson: `{"status":"refused","findings":[{"code":"EditNotAllowed","item_id":null,"message":"Not editable"}],"cards":[]}`}, nil
}
func TestReviseUnaryClient(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fake := &revisionServer{}
	agentv1.RegisterAgentServer(srv, fake)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(srv.Stop)
	client, err := Dial(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	request := ReviseRequest{Definition: ReviseDefinition{OntologyTOML: "ontology", MappingsTOML: "mappings", DraftJSON: `{"draft":{"items":{}}}`, QuestionsJSON: `[]`},
		Edits: model.OntologyEdits{model.OntologyPut{ItemID: "constraint.required", TOML: "[constraints.required]"}, model.OntologyRemove{ItemID: "rule.old"}}}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	raw, err := client.Revise(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	result, err := DecodeReviseResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	if fake.got != string(body) || result.Status != ReviseRefused || len(result.Findings) != 1 {
		t.Fatalf("%s %+v", fake.got, result)
	}
	if _, err := client.Revise(ctx, []byte("refuse")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("RPC error lost: %v", err)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["eval"] != nil || string(sent["edits"]) != `[{"put":{"item_id":"constraint.required","toml":"[constraints.required]"}},{"remove":{"item_id":"rule.old"}}]` {
		t.Fatalf("wire shape %s", body)
	}
}
func TestDecodeReviseResult(t *testing.T) {
	d := ReviseDefinition{OntologyTOML: "ontology", MappingsTOML: "mappings", DraftJSON: `{}`, QuestionsJSON: `[]`}
	raw, _ := json.Marshal(ReviseResult{Status: ReviseRevised, Definition: &d, Rules: &RuleCounts{Count: 2, Evaluated: 2}, Findings: []ReviseFinding{}, Cards: []ReviseCard{}})
	if _, err := DecodeReviseResult(raw); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{`, `{}`, `{"status":"other","findings":[],"cards":[]}`, `{"status":"refused","definition":{},"findings":[],"cards":[]}`, `{"status":"revised","rules":{"count":0,"evaluated":0},"findings":[],"cards":[]}`,
		`{"status":"revised","rules":{"count":0,"evaluated":0},"definition":{"ontology_toml":"x","mappings_toml":"y","draft_json":"null","questions_json":"[]"},"findings":[],"cards":[]}`,
		`{"status":"revised","rules":{"count":0,"evaluated":0},"definition":{"ontology_toml":"x","mappings_toml":"y","draft_json":"{}","questions_json":"{}"},"findings":[],"cards":[]}`, `{"status":"revised","rules":{"count":0,"evaluated":0},"definition":{"ontology_toml":"x","mappings_toml":"y","draft_json":"bad","questions_json":"[]"},"findings":[],"cards":[]}`} {
		if _, err := DecodeReviseResult([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestReviseRuleCounts(t *testing.T) {
	for _, tc := range []struct {
		name, counts string
		ok           bool
	}{
		{"missing", "null", false}, {"unevaluated", `{"count":2,"evaluated":1}`, false},
		{"evaluated", `{"count":2,"evaluated":2}`, true}, {"empty", `{"count":0,"evaluated":0}`, true},
		{"over count", `{"count":1,"evaluated":2}`, false}, {"negative", `{"count":-1,"evaluated":0}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"status":"revised","definition":{"ontology_toml":"x","mappings_toml":"y","draft_json":"{}","questions_json":"[]"},"findings":[],"cards":[],"rules":` + tc.counts + `}`
			_, err := DecodeReviseResult([]byte(raw))
			if (err == nil) != tc.ok {
				t.Fatal(err)
			}
		})
	}
}

// The engine's own wire JSON (testdata/README.md): each result is valid by
// the vendored schema, decodes, and encodes back to the same JSON, its card
// evidence untouched.
func TestEngineReviseExamples(t *testing.T) {
	schema := contracttest.Schema(t, "revise_result.json")
	unmeasured := func(c ReviseCard) string {
		var card struct {
			Unmeasured *string `json:"unmeasured"`
		}
		_ = json.Unmarshal(c.Card, &card)
		if card.Unmeasured == nil {
			return ""
		}
		return *card.Unmeasured
	}
	for name, check := range map[string]func(t *testing.T, r ReviseResult){
		"measured": func(t *testing.T, r ReviseResult) {
			if r.Status != ReviseRevised || *r.Rules != (RuleCounts{Count: 2, Evaluated: 2}) || len(r.Cards) != 2 || len(r.Findings) != 0 ||
				r.Cards[0].Verdict != ReviseRule || r.Cards[1].Verdict != ReviseWarn || unmeasured(r.Cards[1]) != "" {
				t.Fatalf("%+v", r)
			}
		},
		"no-load": func(t *testing.T, r ReviseResult) {
			if r.Status != ReviseRevised || r.Findings[0].Code != ReviseNoComparableLoad || r.Findings[0].ItemID != nil ||
				unmeasured(r.Cards[0]) != "load" || r.Cards[0].Verdict != ReviseWarn {
				t.Fatalf("%+v", r)
			}
		},
		"entities": func(t *testing.T, r ReviseResult) {
			if r.Status != ReviseRevised || len(r.Findings) != 0 || unmeasured(r.Cards[0]) != "entities" || r.Cards[0].Verdict != ReviseWarn {
				t.Fatalf("%+v", r)
			}
		},
		"budget": func(t *testing.T, r ReviseResult) {
			if r.Status != ReviseRefused || len(r.Findings) != 1 || r.Findings[0].Code != ReviseBudget || len(r.Cards) != 0 ||
				r.Definition != nil || r.Rules != nil {
				t.Fatalf("%+v", r)
			}
		},
		"refused": func(t *testing.T, r ReviseResult) {
			if r.Status != ReviseRefused || r.Findings[0].Code != ReviseEditNotAllowed || *r.Findings[0].ItemID != "binding.x" {
				t.Fatalf("%+v", r)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/revise-" + name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			if err := contracttest.Validate(t, schema, raw); err != nil {
				t.Fatal(err)
			}
			result, err := DecodeReviseResult(raw)
			if err != nil {
				t.Fatal(err)
			}
			check(t, result)
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var before, after any
			_ = json.Unmarshal(raw, &before)
			_ = json.Unmarshal(encoded, &after)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("evidence changed: %s", encoded)
			}
		})
	}
}

// The engine's own request reads into ReviseRequest and writes back the same
// JSON: the externally tagged edits and every Eval field, ontology_toml and
// mappings_toml too.
func TestEngineReviseRequest(t *testing.T) {
	raw, err := os.ReadFile("testdata/revise-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var request ReviseRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if request.Eval == nil || request.Eval.Load != "fixture-load" || !strings.Contains(request.Eval.OntologyTOML, "[types.Place]") ||
		!strings.Contains(request.Eval.MappingsTOML, `ontology = "draft"`) {
		t.Fatalf("eval %+v", request.Eval)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var before, after any
	_ = json.Unmarshal(raw, &before)
	_ = json.Unmarshal(encoded, &after)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("request changed: %s", encoded)
	}
	if err := contracttest.Validate(t, contracttest.Schema(t, "revise_request.json"), encoded); err != nil {
		t.Fatal(err)
	}
}

func TestReviseFindingCodes(t *testing.T) {
	for _, code := range []ReviseFindingCode{ReviseEditNotAllowed, ReviseInvalidDefinition, ReviseInvalidEdit, ReviseValidation, ReviseEvaluation, ReviseLint, ReviseNoComparableLoad, ReviseBudget} {
		raw, _ := json.Marshal(ReviseResult{Status: ReviseRefused, Findings: []ReviseFinding{{Code: code, Message: "<untrusted>"}}, Cards: []ReviseCard{}})
		if _, err := DecodeReviseResult(raw); err != nil {
			t.Fatal(err)
		}
	}
	// A budget stop refuses with its one finding: no card comes from part of a
	// measurement, and nothing measured part way is a revision.
	budget := `{"code":"budget","item_id":null,"message":"over budget"}`
	card := `{"item":"constraint.cycle","verdict":"warn","card":{}}`
	for _, raw := range []string{
		`{"status":"refused","findings":[` + budget + `,{"code":"Lint","item_id":null,"message":"x"}],"cards":[]}`,
		`{"status":"refused","findings":[` + budget + `],"cards":[` + card + `]}`,
		`{"status":"revised","definition":{"ontology_toml":"x","mappings_toml":"","draft_json":"{}","questions_json":"[]"},"findings":[` + budget + `],"cards":[],"rules":{"count":0,"evaluated":0}}`,
	} {
		if _, err := DecodeReviseResult([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, finding := range []string{`null`, `{}`, `{"code":"future","item_id":null,"message":"x"}`, `{"code":"Lint","message":"x"}`, `{"code":"Lint","item_id":null}`, `{"code":"Lint","item_id":7,"message":"x"}`, `{"code":7,"item_id":null,"message":"x"}`, `{"code":"Lint","item_id":null,"message":null}`} {
		raw := `{"status":"refused","findings":[` + finding + `],"cards":[]}`
		if _, err := DecodeReviseResult([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestReviseUnionAndCards(t *testing.T) {
	for _, fields := range []string{`,"definition":null`, `,"definition":{}`, `,"rules":null`, `,"rules":{"count":0,"evaluated":0}`, `,"unknown":true`} {
		raw := `{"status":"refused","findings":[],"cards":[]` + fields + `}`
		if _, err := DecodeReviseResult([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, card := range []string{`null`, `{}`, `{"item_id":"constraint.cycle","verdict":"warn","card":{}}`, `{"item":"constraint.cycle","verdict":"future","card":{}}`, `{"item":"constraint.cycle","verdict":"warn","card":null}`, `{"item":"constraint.cycle","verdict":"warn","card":[]}`} {
		raw := `{"status":"refused","findings":[],"cards":[` + card + `]}`
		if _, err := DecodeReviseResult([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, counts := range []string{`{}`, `{"count":0}`, `{"evaluated":0}`, `{"count":null,"evaluated":0}`} {
		raw := `{"status":"revised","definition":{"ontology_toml":"x","mappings_toml":"","draft_json":"{}","questions_json":"[]"},"findings":[],"cards":[],"rules":` + counts + `}`
		if _, err := DecodeReviseResult([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestReviseRequestSchema(t *testing.T) {
	request := ReviseRequest{Definition: ReviseDefinition{OntologyTOML: "x", DraftJSON: `{}`, QuestionsJSON: `[]`},
		Edits: model.OntologyEdits{model.OntologyPut{ItemID: "constraint.cycle", TOML: "[constraints.cycle]"}, model.OntologyRemove{ItemID: "rule.old"}}}
	request.Eval = &ReviseEval{Database: "fixture-db", Load: "01JDLOAD0000000000000000AB", OntologyVersion: "foundry-v3", OntologyTOML: "version = \"draft\"", MappingsTOML: "ontology = \"draft\"", Helix: HelixTarget{URL: "http://127.0.0.1:1", Token: "fixture", MaxRequestBytes: 2097152, TimeoutSecs: 60}}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := contracttest.Validate(t, contracttest.Schema(t, "revise_request.json"), raw); err != nil {
		t.Fatal(err)
	}
}

func TestReviseDefinitionFields(t *testing.T) {
	raw, err := os.ReadFile("testdata/revise-no-load.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ontology_toml", "mappings_toml", "draft_json", "questions_json"} {
		for _, null := range []bool{false, true} {
			var value map[string]any
			_ = json.Unmarshal(raw, &value)
			definition := value["definition"].(map[string]any)
			if null {
				definition[name] = nil
			} else {
				delete(definition, name)
			}
			invalid, _ := json.Marshal(value)
			if _, err := DecodeReviseResult(invalid); err == nil {
				t.Fatalf("accepted missing/null %s", name)
			}
		}
	}
	if _, err := DecodeReviseResult([]byte(`{"status":"refused","findings":[],"cards":[]} {}`)); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}

func ExampleDecodeReviseResult() {
	refused, _ := DecodeReviseResult([]byte(`{"status":"refused","findings":[{"code":"budget","item_id":null,"message":"Measuring needs more than its budget."}],"cards":[]}`))
	fmt.Println(refused.Status, refused.Findings[0].Code, refused.Definition == nil)
	revised, _ := DecodeReviseResult([]byte(`{"status":"revised","definition":{"ontology_toml":"version = \"draft\"","mappings_toml":"","draft_json":"{}","questions_json":"[]"},` +
		`"findings":[{"code":"no_comparable_load","item_id":null,"message":"No comparable load."}],` +
		`"cards":[{"item":"constraint.cycle","verdict":"warn","card":{"unmeasured":"load"}}],"rules":{"count":1,"evaluated":1}}`))
	fmt.Println(revised.Status, revised.Cards[0].Verdict, *revised.Rules)
	_, err := DecodeReviseResult([]byte(`{"status":"revised","findings":[],"cards":[]}`))
	fmt.Println(err != nil)
	// Output:
	// refused budget true
	// revised warn {1 1}
	// true
}
