package ontologyagent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
)

// testCluesRequest is a Clues the graph worker could send.
func testCluesRequest() CluesRequest {
	return CluesRequest{
		RequestID: "job_1:clues", Load: "job_1", OntologyTOML: "version = \"v1\"\n", MappingsTOML: "version = \"v1\"\n",
		Sources:         []Source{{ID: "src_0123456789abcdef", DisplayName: "Shop / people", Path: "cache/ab.jsonl", Fingerprint: "ab"}},
		MaxRequestBytes: 2 << 20,
	}
}

// The engine's own events (agent::graph::CluesEvent's doc tests), and those
// of a stream of each ending.
const (
	cluesStarted = `{"event":"clues_started","contract_version":"3.9.0"}`
	cluesPage    = `{"event":"clues","entities":[{"entity":"Person[sc_17]","name":"Ada Lovelace","email":"ada@acme.io"},` +
		`{"entity":"Person[sc_18]","name":null,"email":null,"linkedin_url":"linkedin.com/in/grace"},{"entity":"Person[sc_19]"}]}`
	cluesDone = `{"event":"done","entities":3,"no_clue":1,"no_name":2,"conflicting":0,"shared":1}`
)

// collect is an add that keeps every person it is handed.
func collect(people *[]CluedEntity) func(CluedEntity) error {
	return func(c CluedEntity) error {
		*people = append(*people, c)
		return nil
	}
}

// cluesStream is a CluesCall that sends events, the last ending the stream.
func cluesStream(events ...string) CluesCall {
	return func(_ context.Context, _ []byte, onEvent func(json.RawMessage) error) (json.RawMessage, error) {
		for _, e := range events[:len(events)-1] {
			if err := onEvent(json.RawMessage(e)); err != nil {
				return nil, err
			}
		}
		return json.RawMessage(events[len(events)-1]), nil
	}
}

func TestCluesContract(t *testing.T) {
	// The request the worker builds is one the agent takes, and the check has teeth.
	t.Run("builds requests the agent accepts", func(t *testing.T) {
		schema := contracttest.Schema(t, "clues_request.json")
		raw, _ := json.Marshal(testCluesRequest())
		if err := contracttest.Validate(t, schema, raw); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if err := contracttest.Validate(t, schema, []byte(`{"request_id":"r","load":"job_1"}`)); err == nil {
			t.Fatal("clues_request.json took a request with no mappings")
		}
	})

	// Every event the fixtures send is one the schema describes.
	t.Run("the fixtures are events the agent sends", func(t *testing.T) {
		schema := contracttest.Schema(t, "clues_event.json")
		for _, raw := range []string{cluesStarted, cluesPage, cluesDone, `{"event":"no_enrichment"}`,
			`{"event":"invalid_mappings","errors":["mapping contacts: no source crm"]}`, `{"event":"source_unavailable","source":"src_1"}`} {
			if err := contracttest.Validate(t, schema, []byte(raw)); err != nil {
				t.Errorf("%s: %v", raw, err)
			}
		}
	})

	// People enrichment's profiles go as the engine pins them: mapped at 0.7,
	// a JSONL file, to a turn and to Ingest alike.
	t.Run("sends the profiles at their trust", func(t *testing.T) {
		raw, _ := json.Marshal(EnrichmentSource("jobs/j/enrichment/foundry_enrichment.jsonl", "e3b0"))
		want := `{"id":"foundry_enrichment","display_name":"People enrichment","route":{"mapping":{"trust":0.7}},` +
			`"access":{"file":{"path":"jobs/j/enrichment/foundry_enrichment.jsonl","format":"jsonl"}},"fingerprint":"e3b0"}`
		if string(raw) != want {
			t.Fatalf("source = %s", raw)
		}
		table, _ := json.Marshal(Source{ID: "src_1", DisplayName: "Shop / people", Path: "cache/ab.jsonl", Fingerprint: "ab"})
		if !strings.Contains(string(table), `"trust":1`) {
			t.Fatalf("a table = %s", table)
		}
		ingest := testIngestRequest()
		ingest.Sources = append(ingest.Sources, EnrichmentSource("jobs/j/p.jsonl", "e3b0"))
		body, _ := json.Marshal(ingest)
		if err := contracttest.Validate(t, contracttest.Schema(t, "ingest_request.json"), body); err != nil {
			t.Fatalf("ingest with the profiles: %v", err)
		}
	})
}

func TestReadClues(t *testing.T) {
	ctx := context.Background()

	// Every person on every page is handed on in entity order, absent fields
	// empty, with the stream's counts.
	t.Run("hands on the people and the counts", func(t *testing.T) {
		var sent []byte
		call := func(ctx context.Context, request []byte, onEvent func(json.RawMessage) error) (json.RawMessage, error) {
			sent = request
			return cluesStream(cluesStarted, cluesPage, cluesDone)(ctx, request, onEvent)
		}
		var people []CluedEntity
		end, err := ReadClues(ctx, call, testCluesRequest(), collect(&people))
		if err != nil {
			t.Fatal(err)
		}
		want := []CluedEntity{
			{Entity: "Person[sc_17]", Name: "Ada Lovelace", Email: "ada@acme.io"},
			{Entity: "Person[sc_18]", LinkedInURL: "linkedin.com/in/grace"},
			{Entity: "Person[sc_19]"},
		}
		if !reflect.DeepEqual(people, want) {
			t.Fatalf("people = %+v", people)
		}
		if end != (CluesDone{Entities: 3, NoClue: 1, NoName: 2, Shared: 1}) {
			t.Fatalf("end = %+v", end)
		}
		var request map[string]any
		if json.Unmarshal(sent, &request) != nil || request["load"] != "job_1" || request["max_request_bytes"] != float64(2<<20) {
			t.Fatalf("request = %s", sent)
		}
	})

	// Each other ending says why nobody was sent.
	t.Run("reads each other ending", func(t *testing.T) {
		for raw, want := range map[string]CluesEnd{
			`{"event":"no_enrichment"}`:                                                        CluesNoItem{},
			`{"event":"invalid_mappings","errors":["a","b"]}`:                                  CluesInvalidMappings{Errors: []string{"a", "b"}},
			`{"event":"source_unavailable","source":"src_1"}`:                                  CluesSourceUnavailable{Source: "src_1"},
			`{"event":"done","entities":0,"no_clue":0,"no_name":0,"conflicting":0,"shared":0}`: CluesDone{},
		} {
			var people []CluedEntity
			end, err := ReadClues(ctx, cluesStream(cluesStarted, raw), testCluesRequest(), collect(&people))
			if err != nil || len(people) != 0 || !reflect.DeepEqual(end, want) {
				t.Errorf("%s = %v %+v %v", raw, people, end, err)
			}
		}
	})

	// A stream that breaks the contract is refused rather than half-read.
	t.Run("refuses a stream out of contract", func(t *testing.T) {
		for name, events := range map[string][]string{
			"no start":           {cluesPage, cluesDone},
			"two starts":         {cluesStarted, cluesStarted, cluesDone},
			"an unknown event":   {cluesStarted, `{"event":"chunk"}`, cluesDone},
			"an unknown ending":  {cluesStarted, `{"event":"finished"}`},
			"a short count":      {cluesStarted, cluesPage, strings.Replace(cluesDone, `"entities":3`, `"entities":4`, 1)},
			"an unreadable page": {cluesStarted, `{"event":"clues","entities":7}`, cluesDone},
		} {
			if _, err := ReadClues(ctx, cluesStream(events...), testCluesRequest(), collect(new([]CluedEntity))); err == nil {
				t.Errorf("%s: read", name)
			}
		}
	})

	// A failed call is the call's error, for the caller to classify.
	t.Run("hands a failed call back", func(t *testing.T) {
		busy := status.Error(codes.ResourceExhausted, "busy")
		call := func(context.Context, []byte, func(json.RawMessage) error) (json.RawMessage, error) { return nil, busy }
		if _, err := ReadClues(ctx, call, testCluesRequest(), collect(new([]CluedEntity))); !errors.Is(err, busy) {
			t.Fatalf("err = %v", err)
		}
	})

	// add refusing a person ends the call with its error: no page after it
	// is read.
	t.Run("stops at add's error", func(t *testing.T) {
		full := errors.New("full")
		var added int
		_, err := ReadClues(ctx, cluesStream(cluesStarted, cluesPage, cluesPage, cluesDone), testCluesRequest(), func(CluedEntity) error {
			if added == 1 {
				return full
			}
			added++
			return nil
		})
		if !errors.Is(err, full) || added != 1 {
			t.Fatalf("err = %v after %d people", err, added)
		}
	})
}

// cluesServer streams events, then ends the call with end (nil: cleanly).
type cluesServer struct {
	agentv1.UnimplementedAgentServer
	events []string
	end    error
	got    string
}

func (s *cluesServer) Clues(req *agentv1.CluesRequest, stream grpc.ServerStreamingServer[agentv1.CluesEvent]) error {
	s.got = req.GetRequestJson()
	for _, e := range s.events {
		if err := stream.Send(&agentv1.CluesEvent{EventJson: e}); err != nil {
			return err
		}
	}
	return s.end
}

func TestClientClues(t *testing.T) {
	serve := func(t *testing.T, s *cluesServer) *Client {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer()
		agentv1.RegisterAgentServer(srv, s)
		go func() { _ = srv.Serve(listener) }()
		t.Cleanup(srv.Stop)
		client, err := Dial(listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The ending event ends the call; the events before it go to onEvent.
	t.Run("returns the ending event", func(t *testing.T) {
		s := &cluesServer{events: []string{cluesStarted, cluesPage, cluesDone}}
		client := serve(t, s)
		var people []CluedEntity
		end, err := ReadClues(ctx, client.Clues, testCluesRequest(), collect(&people))
		if err != nil || len(people) != 3 || end.(CluesDone).Entities != 3 || !strings.Contains(s.got, `"request_id":"job_1:clues"`) {
			t.Fatalf("people %v, end %+v, err %v, sent %s", people, end, err, s.got)
		}
	})

	// A stream that stops before its end, or the agent's refusal, is an error.
	t.Run("a stream cut short is no result", func(t *testing.T) {
		client := serve(t, &cluesServer{events: []string{cluesStarted, cluesPage}})
		if _, err := client.Clues(ctx, []byte(`{}`), func(json.RawMessage) error { return nil }); !errors.Is(err, ErrNoResult) {
			t.Fatalf("err = %v", err)
		}
		client = serve(t, &cluesServer{end: status.Error(codes.Unavailable, "shutting down")})
		if _, err := client.Clues(ctx, []byte(`{}`), func(json.RawMessage) error { return nil }); status.Code(err) != codes.Unavailable {
			t.Fatalf("err = %v", err)
		}
	})
}
