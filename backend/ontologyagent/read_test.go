package ontologyagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
)

// everyRead is one read of each kind the contract has, both evidence reads
// included, and readResults answers them in order: an entity with its
// derived facts, flags, live violations and failed parts, a pattern's rows, a
// proof, a page of violations, a fact's evidence, an entity unknown to the
// load, and a query's count.
const (
	everyRead = `[{"entity":{"type":"Customer","id":"acme"}},` +
		`{"pattern":{"pred":"relation:located_in","args":[{"type":"Office","id":"soho"},null]}},` +
		`{"explain":{"pred":"derived:blocked","args":[{"type":"Customer","id":"acme"}]}},` +
		`{"violations":{"constraint":"no_self_report","entity":null,"after":null,"limit":50}},` +
		`{"evidence":{"fact":{"link":"party_to","subject":{"type":"Customer","id":"acme"},"object":{"type":"Contract","id":"msa"}},"limit":10}},` +
		`{"evidence":{"entity":{"type":"Customer","id":"acme"},"prop":"amount"}},` +
		`{"query":{"start":{"type":"Invoice","where":[{"prop":"status","op":"=","value":"open"}]},` +
		`"steps":[{"via":"billed_to","dir":"out","to":"Customer"}],"filters":[{"at":1,"not_holds":"blocked"}],"return":{"count":{"distinct_at":1}}}}]`
	readResults = `[{"ok":{"kind":"entity","entity":{"type":"Customer","id":"acme"},` +
		`"derived":[{"pred":"derived:owes","holder":null,"args":[{"type":"Customer","id":"acme"},{"type":"Invoice","id":"in_1"}],"basis":"derived","rules":["owes"]}],` +
		`"flags":[{"name":"overdue","rules":["overdue"]}],` +
		`"violations":[{"constraint":"credit_limit","severity":"warn","entities":[{"type":"Customer","id":"acme"}],"stored":true}],` +
		`"errors":[{"pred":"derived:blocked","code":"budget","message":"too deep","limit":64},{"pred":"violations","code":"hub","message":"a hub","limit":10000},` +
		`{"pred":"relation:sells_to","code":"invalid","message":"another ontology's facts"}]}},` +
		`{"ok":{"kind":"pattern","rows":[{"args":[{"type":"Office","id":"soho"},{"type":"City","id":"london"}],"holder":"located_in","basis":"both","rules":["in_city"]}]}},` +
		`{"ok":{"kind":"explain","steps":[{"pred":"link:party_to","args":[{"type":"Customer","id":"acme"},{"type":"Contract","id":"msa"}],"via":"stated","premises":[]},` +
		`{"pred":"link:disputes","args":[{"type":"Customer","id":"acme"},null],"via":"not","premises":[]},` +
		`{"pred":"derived:blocked","args":[{"type":"Customer","id":"acme"}],"via":"rule:blocked","premises":[0,1]}],"root":2}},` +
		`{"ok":{"kind":"violations","records":[{"violation_key":"k1","constraint":"no_self_report","version":"v3","severity":"block",` +
		`"entities":[{"type":"Person","id":"ada"}],"opened_by":"doc:d1"}],"next":"k1"}},` +
		`{"ok":{"kind":"evidence","items":[{"kind":"quote","doc_id":"d1","section":2,"start":10,"end":42,"quote":"Acme signed","confidence":0.9},` +
		`{"kind":"row","source":"src_1","mapping":"contracts","row_key":"k1","confidence":1}]}},` +
		`{"error":{"code":"unknown_entity","message":"no Customer acme"}},` +
		`{"ok":{"kind":"query","query_echo":{"start":{"type":"Invoice","where":[{"prop":"status","op":"=","value":"open"}],"ids":null,"text":null},` +
		`"steps":[{"via":"billed_to","dir":"out","to":"Customer","where":[]}],"filters":[{"at":1,"not_holds":"blocked"}],"return":{"count":{"distinct_at":1}}},` +
		`"result":{"kind":"count","count":3},"stats":{"matched":12,"patterns":3,"terminal":"rust"}}}]`
)

// readResult is a Read result with these results.
func readResult(results string) string {
	return `{"as_of":"2026-10-19","results":` + results + `,"stats":{"instances":120,"wall_ms":35}}`
}

func ExampleParseReads() {
	reads, err := ParseReads(`[{"entity":{"type":"Customer","id":"c-1"}},{"violations":{"constraint":"no_self_report","limit":100}}]`)
	if err != nil {
		panic(err)
	}
	body, _ := json.Marshal(reads)
	fmt.Println(reads.Len(), string(body))
	// Output:
	// 2 [{"entity":{"type":"Customer","id":"c-1"}},{"violations":{"constraint":"no_self_report","limit":100}}]
}

func TestParseReads(t *testing.T) {
	// Every kind of the contract is a read, and what the agent gets is what
	// was checked: a duplicate key is written once, as JSON keeps the last.
	t.Run("takes each kind and writes each read again", func(t *testing.T) {
		reads, err := ParseReads(everyRead)
		if err != nil || reads.Len() != 7 {
			t.Fatalf("reads = (%d, %v)", reads.Len(), err)
		}
		if want := []string{"entity", "pattern", "explain", "violations", "evidence", "evidence", "query"}; strings.Join(reads.kinds, ",") != strings.Join(want, ",") {
			t.Fatalf("kinds = %v", reads.kinds)
		}
		reads, err = ParseReads(`[{"entity":{"id":"a"},"entity":{"id":"b"}}]`)
		if body, _ := json.Marshal(reads); err != nil || string(body) != `[{"entity":{"id":"b"}}]` {
			t.Fatalf("reads = %s, %v", body, err)
		}
		full := "[" + strings.TrimSuffix(strings.Repeat(`{"entity":{}},`, MaxReads), ",") + "]"
		if reads, err := ParseReads(full); err != nil || reads.Len() != MaxReads {
			t.Fatalf("%d reads = (%d, %v)", MaxReads, reads.Len(), err)
		}
	})

	// The caller's reads reach the agent only in the contract's shape and
	// within its count; their fields are the agent's to check.
	t.Run("refuses reads outside the contract", func(t *testing.T) {
		for name, raw := range map[string]string{
			"not JSON":             `[`,
			"an object":            `{"entity":{}}`,
			"no reads":             `[]`,
			"too many reads":       "[" + strings.Repeat(`{"entity":{}},`, MaxReads) + `{"entity":{}}]`,
			"a null read":          `[null]`,
			"a read of two kinds":  `[{"entity":{},"pattern":{}}]`,
			"an unknown kind":      `[{"search":{}}]`,
			"a kind not an object": `[{"entity":"Customer"}]`,
			"a null kind":          `[{"entity":null}]`,
			"a read past the size": `[{"entity":{"id":"` + strings.Repeat("a", MaxReadsBytes) + `"}}]`,
		} {
			if _, err := ParseReads(raw); err == nil {
				t.Errorf("%s: parsed", name)
			}
		}
	})
}

func TestReadRequest(t *testing.T) {
	// foundry-read's request, with a read of every kind, is one the agent
	// accepts.
	t.Run("builds a request the agent accepts", func(t *testing.T) {
		reads, err := ParseReads(everyRead)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(ReadRequest{
			RequestID: "r-1", Database: "tenant-1", Load: "load-1", OntologyTOML: "version = \"x\"\n", OntologyVersion: "foundry-v3",
			Helix: HelixTarget{URL: "http://127.0.0.1:1", Token: "t", MaxRequestBytes: 1 << 21, TimeoutSecs: 10},
			AsOf:  "2026-10-19", Budget: DefaultReadBudget, Reads: reads,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := contracttest.Validate(t, contracttest.Schema(t, "read_request.json"), body); err != nil {
			t.Fatalf("%s does not match read_request.json: %v", body, err)
		}
	})

	// The schema check has teeth: reads ParseReads lets through for the agent
	// to judge are refused there, as the agent refuses them.
	t.Run("leaves the reads' fields to the agent", func(t *testing.T) {
		schema := contracttest.Schema(t, "read_request.json")
		for name, read := range map[string]string{
			"an entity with no id":   `{"entity":{"type":"Customer"}}`,
			"a filter of two tests":  `{"query":{"start":{"type":"T"},"filters":[{"at":0,"holds":"a","violates":"b"}],"return":{"count":{"distinct_at":0}}}}`,
			"a filter's extra field": `{"query":{"start":{"type":"T"},"filters":[{"at":0,"holds":"a","x":1}],"return":{"count":{"distinct_at":0}}}}`,
			"a page of no kind":      `{"violations":{"page":2}}`,
		} {
			reads, err := ParseReads("[" + read + "]")
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			body, _ := json.Marshal(ReadRequest{RequestID: "r", Database: "d", Load: "l", OntologyTOML: "o", OntologyVersion: "v",
				Helix: HelixTarget{URL: "u", Token: "t", MaxRequestBytes: 1, TimeoutSecs: 1}, AsOf: "2026-10-19", Budget: DefaultReadBudget, Reads: reads})
			if err := contracttest.Validate(t, schema, body); err == nil {
				t.Errorf("%s: read_request.json accepted %s", name, read)
			}
		}
	})
}

func TestCheckReadResult(t *testing.T) {
	everyKind, err := ParseReads(everyRead)
	if err != nil {
		t.Fatal(err)
	}
	oneQuery, err := ParseReads(`[{"query":{"start":{"type":"Invoice"},"return":{"count":{"distinct_at":0}}}}]`)
	if err != nil {
		t.Fatal(err)
	}

	// An answer of each kind, and failed parts of an entity read, are what
	// the agent sends and are relayed.
	t.Run("takes an answer of each kind", func(t *testing.T) {
		result := readResult(readResults)
		if err := contracttest.Validate(t, contracttest.Schema(t, "read_result.json"), []byte(result)); err != nil {
			t.Fatalf("the fixture does not match read_result.json: %v", err)
		}
		if err := CheckReadResult([]byte(result), everyKind); err != nil {
			t.Fatal(err)
		}
	})

	// A property's evidence ends with the document quotes stating a value of
	// it, each with the value, and a proof step may name the property test it
	// stands for (contract 3.x): both reach the caller as the agent wrote them.
	t.Run("takes a property's quotes with their values", func(t *testing.T) {
		reads, err := ParseReads(`[{"evidence":{"entity":{"type":"Contract","id":"msa"},"prop":"amount"}},{"explain":{"pred":"derived:large","args":[{"type":"Contract","id":"msa"}]}}]`)
		if err != nil {
			t.Fatal(err)
		}
		result := readResult(`[{"ok":{"kind":"evidence","items":[{"kind":"row","source":"src_1","mapping":"contracts","row_key":"k1","confidence":1},` +
			`{"kind":"quote","doc_id":"d1","section":0,"start":4,"end":16,"quote":"total $1,200","confidence":0.8,"value":1200}]}},` +
			`{"ok":{"kind":"explain","steps":[{"pred":"derived:large","args":[{"type":"Contract","id":"msa"}],"via":"rule:large","premises":[],"test":"amount > 1000"}],"root":0}}]`)
		if err := contracttest.Validate(t, contracttest.Schema(t, "read_result.json"), []byte(result)); err != nil {
			t.Fatalf("the fixture does not match read_result.json: %v", err)
		}
		if err := CheckReadResult([]byte(result), reads); err != nil {
			t.Fatal(err)
		}
	})

	// Each failure code comes with its own fields; the website shows budget
	// as "too deep to compute live", too_broad with its count.
	t.Run("takes each failure with its fields", func(t *testing.T) {
		pointer := "/steps/0/via"
		for raw, want := range map[string]ReadFailure{
			`{"code":"budget","message":"too deep","limit":64}`:                      {Code: ReadBudgetExceeded, Message: "too deep", Limit: 64},
			`{"code":"hub","message":"a hub","limit":10000}`:                         {Code: ReadHub, Message: "a hub", Limit: 10000},
			`{"code":"too_broad","message":"too broad","limit":20000,"count":31000}`: {Code: ReadTooBroad, Message: "too broad", Limit: 20000, Count: 31000},
			`{"code":"unknown_entity","message":"no Invoice x"}`:                     {Code: ReadUnknownEntity, Message: "no Invoice x"},
			`{"code":"invalid","message":"no link via","pointer":"/steps/0/via"}`:    {Code: ReadInvalid, Message: "no link via", Pointer: &pointer},
			`{"code":"invalid","message":"no type X"}`:                               {Code: ReadInvalid, Message: "no type X"},
		} {
			result := readResult(`[{"error":` + raw + `}]`)
			if err := contracttest.Validate(t, contracttest.Schema(t, "read_result.json"), []byte(result)); err != nil {
				t.Fatalf("%s does not match read_result.json: %v", raw, err)
			}
			if err := CheckReadResult([]byte(result), oneQuery); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			got, err := decodeReadFailure(json.RawMessage(raw), readFailureCodes)
			if err != nil || got.Code != want.Code || got.Message != want.Message || got.Limit != want.Limit || got.Count != want.Count ||
				(got.Pointer == nil) != (want.Pointer == nil) || got.Pointer != nil && *got.Pointer != *want.Pointer {
				t.Fatalf("%s: %+v, %v", raw, got, err)
			}
		}
	})

	// The schema check has teeth: a failure without its code's fields is
	// refused there too.
	t.Run("the schema refuses a failure missing its fields", func(t *testing.T) {
		raw := readResult(`[{"error":{"code":"too_broad","message":"m","limit":1}}]`)
		if err := contracttest.Validate(t, contracttest.Schema(t, "read_result.json"), []byte(raw)); err == nil {
			t.Fatalf("read_result.json accepted %s", raw)
		}
	})

	// A result that doesn't answer each read, in order and whole, is not
	// relayed: the website would show one read's answer as another's.
	t.Run("refuses a result that doesn't answer its reads", func(t *testing.T) {
		count := `{"ok":{"kind":"query","query_echo":{},"result":{"kind":"count","count":1},"stats":{}}}`
		entity := func(errors string) string {
			return readResult(`[{"ok":{"kind":"entity","entity":{"type":"C","id":"c"},"derived":[],"flags":[],"violations":[],"errors":[` + errors + `]}}]`)
		}
		oneEntity, err := ParseReads(`[{"entity":{"type":"C","id":"c"}}]`)
		if err != nil {
			t.Fatal(err)
		}
		for name, tc := range map[string]struct {
			result string
			reads  Reads
		}{
			"not JSON":                   {`{`, oneQuery},
			"no as_of":                   {`{"results":[` + count + `],"stats":{"instances":0,"wall_ms":0}}`, oneQuery},
			"no stats":                   {`{"as_of":"d","results":[` + count + `]}`, oneQuery},
			"stats with no wall time":    {`{"as_of":"d","results":[` + count + `],"stats":{"instances":0}}`, oneQuery},
			"fewer results":              {readResult(`[]`), oneQuery},
			"more results":               {readResult(`[` + count + `,` + count + `]`), oneQuery},
			"neither ok nor error":       {readResult(`[{}]`), oneQuery},
			"both ok and error":          {readResult(`[{"ok":{"kind":"query"},"error":{"code":"hub","message":"m","limit":1}}]`), oneQuery},
			"an answer of another kind":  {readResult(`[{"ok":{"kind":"pattern","rows":[]}}]`), oneQuery},
			"an answer of no kind":       {readResult(`[{"ok":{"count":1}}]`), oneQuery},
			"an unknown error code":      {readResult(`[{"error":{"code":"oops","message":"m"}}]`), oneQuery},
			"an error with no message":   {readResult(`[{"error":{"code":"unknown_entity"}}]`), oneQuery},
			"a budget with no limit":     {readResult(`[{"error":{"code":"budget","message":"m"}}]`), oneQuery},
			"a hub with no limit":        {readResult(`[{"error":{"code":"hub","message":"m"}}]`), oneQuery},
			"too broad with no count":    {readResult(`[{"error":{"code":"too_broad","message":"m","limit":1}}]`), oneQuery},
			"a part that is too broad":   {entity(`{"pred":"derived:x","code":"too_broad","message":"m","limit":1,"count":2}`), oneEntity},
			"a part of no predicate":     {entity(`{"code":"budget","message":"m","limit":1}`), oneEntity},
			"a part's budget, no limit":  {entity(`{"pred":"derived:x","code":"budget","message":"m"}`), oneEntity},
			"a part with an unknown one": {entity(`{"pred":"derived:x","code":"unknown_entity","message":"m"}`), oneEntity},
		} {
			if err := CheckReadResult([]byte(tc.result), tc.reads); err == nil {
				t.Errorf("%s: accepted %s", name, tc.result)
			}
		}
	})
}

// readAgent answers Read with result, or err, and keeps the request.
type readAgent struct {
	agentv1.UnimplementedAgentServer
	got    string
	result string
	err    error
}

func (a *readAgent) Read(_ context.Context, req *agentv1.ReadRequest) (*agentv1.ReadResponse, error) {
	a.got = req.GetRequestJson()
	if a.err != nil {
		return nil, a.err
	}
	return &agentv1.ReadResponse{ResultJson: a.result}, nil
}

func TestClientRead(t *testing.T) {
	// One unary call carries the request as sent and the result as written;
	// the agent's refusals keep their gRPC code.
	t.Run("relays the request, the result and the agent's status", func(t *testing.T) {
		agent := &readAgent{result: `{"as_of":"d","results":[]}`}
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer()
		agentv1.RegisterAgentServer(srv, agent)
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		client, err := Dial(lis.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })

		got, err := client.Read(context.Background(), []byte(`{"request_id":"r"}`))
		if err != nil || string(got) != agent.result || agent.got != `{"request_id":"r"}` {
			t.Fatalf("read = (%s, %v), sent %s", got, err, agent.got)
		}
		agent.err = status.Error(codes.FailedPrecondition, "load_unknown")
		if _, err := client.Read(context.Background(), []byte(`{}`)); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("a refused read: %v", err)
		}
	})
}
