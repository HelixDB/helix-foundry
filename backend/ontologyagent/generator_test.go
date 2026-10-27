package ontologyagent

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	"github.com/helixdb/helix-foundry/backend/ontologygen"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// script is what the fake agent does for one Turn call.
type script func(stream agentv1.Agent_TurnServer) error

// fakeAgent serves ontology_agent.v1.Agent in process: each Turn call runs
// the next script (the last one again once they run out) and is recorded.
type fakeAgent struct {
	agentv1.UnimplementedAgentServer
	mu        sync.Mutex
	scripts   []script
	requests  []string
	deadlines []time.Time
	// keys are each call's model key metadata.
	keys    [][]string
	info    *agentv1.GetInfoResponse
	infoErr error
}

func (f *fakeAgent) Turn(req *agentv1.TurnRequest, stream agentv1.Agent_TurnServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	f.mu.Lock()
	run := f.scripts[min(len(f.requests), len(f.scripts)-1)]
	f.requests = append(f.requests, req.GetRequestJson())
	f.keys = append(f.keys, md.Get(ModelKeyHeader))
	deadline, _ := stream.Context().Deadline()
	f.deadlines = append(f.deadlines, deadline)
	f.mu.Unlock()
	return run(stream)
}

func (f *fakeAgent) GetInfo(context.Context, *agentv1.GetInfoRequest) (*agentv1.GetInfoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info, f.infoErr
}

func (f *fakeAgent) setInfo(info *agentv1.GetInfoResponse, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.info, f.infoErr = info, err
}

func (f *fakeAgent) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// testKey is the model key fakeDrafter resolves.
const testKey = "sk-ant-api03-generator-sentinel-0123"

// fakeDrafter resolves every workspace to Helix's Claude with testKey, or
// fails with err; a model failure f reads "the model failed: f".
type fakeDrafter struct {
	mu        sync.Mutex
	err       error
	providers [][]string
}

func (d *fakeDrafter) Draft(_ context.Context, _ string, providers []string) (Draft, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.providers = append(d.providers, providers)
	if d.err != nil {
		return Draft{}, d.err
	}
	return Draft{Models: houseTurn(), Key: NewModelKey(testKey), Failure: func(_ context.Context, f ModelFailure) error {
		return &ontologygen.Failure{Message: "the model failed: " + string(f), Permanent: f == ModelRefused}
	}}, nil
}

// send sends events, then the result unless it is empty.
func send(stream agentv1.Agent_TurnServer, events []string, result string) error {
	for _, e := range events {
		if err := stream.Send(&agentv1.TurnEvent{Kind: &agentv1.TurnEvent_EventJson{EventJson: e}}); err != nil {
			return err
		}
	}
	if result == "" {
		return nil
	}
	return stream.Send(&agentv1.TurnEvent{Kind: &agentv1.TurnEvent_ResultJson{ResultJson: result}})
}

// respond sends the test events, then a result with this status.
func respond(status string) script {
	return func(stream agentv1.Agent_TurnServer) error { return send(stream, testEvents, testResult(status, 1)) }
}

// refuse fails the call before any event, as the agent's own errors do.
func refuse(code codes.Code) script {
	return func(agentv1.Agent_TurnServer) error { return status.Error(code, "refused") }
}

// hang sends the events up to the usage one, then waits for the call to end
// and says so on cancelled.
func hang(cancelled chan<- struct{}) script {
	return func(stream agentv1.Agent_TurnServer) error {
		if err := send(stream, testEvents[:4], ""); err != nil {
			return err
		}
		<-stream.Context().Done()
		cancelled <- struct{}{}
		return stream.Context().Err()
	}
}

// memStore is a workspace with a Stripe connector whose customers table is
// synced, and the document sets of its ontology.
type memStore struct {
	workspace *workspace
	tables    []*model.ConnectorTable
	// sets are the live document sets by id, and documents each set's
	// uploaded documents, newest first.
	sets      map[string]*model.FoundryDocumentSet
	documents map[string][]*model.FoundryDocument
	// listErr, when set, is every document listing's answer.
	listErr error
	// limits are the limits the documents were listed with.
	limits []int
	// files are the workspace's documents by id, as the generation's table
	// files are read.
	files map[string]*model.FoundryDocument
	// consent is the workspace's People enrichment setting, nil when never
	// set; consentErr, when set, is every read's answer.
	consent    *model.FoundryEnrichmentConsent
	consentErr error
	// connectors are the workspace's connectors; nil is the Stripe one alone.
	connectors []*model.Connector
}

func (s *memStore) ListFoundryDocumentsByID(_ context.Context, workspaceID string, ids []string) (map[string]*model.FoundryDocument, error) {
	out := map[string]*model.FoundryDocument{}
	for _, id := range ids {
		if d, ok := s.files[id]; ok && d.WorkspaceID == workspaceID {
			out[id] = d
		}
	}
	return out, nil
}

func (s *memStore) ListFoundryDocumentSets(_ context.Context, workspaceID string) ([]*model.FoundryDocumentSet, error) {
	var out []*model.FoundryDocumentSet
	for _, set := range s.sets {
		if set.WorkspaceID == workspaceID {
			out = append(out, set)
		}
	}
	return out, nil
}

func (s *memStore) GetFoundryEnrichmentConsent(context.Context, string) (*model.FoundryEnrichmentConsent, error) {
	switch {
	case s.consentErr != nil:
		return nil, s.consentErr
	case s.consent == nil:
		return nil, foundrystore.ErrNotFound
	}
	return s.consent, nil
}

func (s *memStore) GetFoundryDocumentSet(_ context.Context, workspaceID, id string) (*model.FoundryDocumentSet, error) {
	set, ok := s.sets[id]
	if !ok || set.WorkspaceID != workspaceID {
		return nil, foundrystore.ErrNotFound
	}
	return set, nil
}

func (s *memStore) ListUploadedFoundryDocuments(_ context.Context, _, setID string, limit int) ([]*model.FoundryDocument, error) {
	s.limits = append(s.limits, limit)
	if s.listErr != nil {
		return nil, s.listErr
	}
	docs := s.documents[setID]
	return docs[:min(limit, len(docs))], nil
}

func (s *memStore) WorkspaceDeleting(context.Context, string) (bool, error) {
	if s.workspace == nil {
		return false, foundrystore.ErrNotFound
	}
	return s.workspace.Deleting, nil
}

// workspace is the host's workspace, as Foundry's store reads it.
type workspace struct {
	ID       string
	Deleting bool
}

func (s *memStore) ListConnectors(context.Context, string) ([]*model.Connector, error) {
	if s.connectors != nil {
		return s.connectors, nil
	}
	return []*model.Connector{{ID: "conn-1", WorkspaceID: "ws-1", Kind: "stripe", Name: "Stripe"}}, nil
}

func (s *memStore) ListConnectorTables(_ context.Context, _, connectorID string) ([]*model.ConnectorTable, error) {
	var out []*model.ConnectorTable
	for _, t := range s.tables {
		if t.ConnectorID == connectorID {
			out = append(out, t)
		}
	}
	return out, nil
}

// memSnapshots is the foundry data bucket.
type memSnapshots struct {
	mu      sync.Mutex
	objects map[string][]byte
	opens   int
}

func (b *memSnapshots) Put(context.Context, string, io.ReadSeeker) error {
	return errors.New("read only")
}

func (b *memSnapshots) Open(_ context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opens++
	data, ok := b.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *memSnapshots) Delete(context.Context, string) error { return nil }

// memDocuments is the documents' originals in the foundry data bucket.
type memDocuments struct {
	objects map[string]string
	// refused, when set, is every Open's answer, as S3 answers a role it denies.
	refused error
}

func (d *memDocuments) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if d.refused != nil {
		return nil, d.refused
	}
	body, ok := d.objects[key]
	if !ok {
		return nil, fmt.Errorf("get %s: %w", key, document.ErrMissing)
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

const (
	snapshotKey  = "ws/ws-1/connectors/conn-1/customers/obj-1.jsonl.gz"
	snapshotRows = "{\"id\":\"cus_1\"}\n{\"id\":\"cus_2\"}\n"
)

var testNow = time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)

type harness struct {
	generator *Generator
	agent     *fakeAgent
	ai        *fakeDrafter
	store     *memStore
	snapshots *memSnapshots
	documents *memDocuments
}

// newHarness drafts with a fake agent running scripts over loopback gRPC.
func newHarness(t *testing.T, scripts ...script) *harness {
	t.Helper()
	agent := &fakeAgent{scripts: scripts, info: &agentv1.GetInfoResponse{ContractVersion: ContractVersion, Providers: []string{"anthropic"}}}
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

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte(snapshotRows))
	_ = w.Close()
	store := &memStore{
		workspace: &workspace{ID: "ws-1"},
		tables:    []*model.ConnectorTable{{ConnectorID: "conn-1", WorkspaceID: "ws-1", TableName: "customers", S3Key: snapshotKey}},
	}
	snapshots := &memSnapshots{objects: map[string][]byte{snapshotKey: gz.Bytes()}}
	documents := &memDocuments{objects: map[string]string{}}
	drafter := &fakeDrafter{}
	return &harness{
		generator: &Generator{
			DB: store, Snapshots: snapshots, Documents: documents, Agent: client, AI: drafter, WorkDir: t.TempDir(),
			Limits: Limits{MaxUSD: 10, MaxSteps: 200, MaxWallSecs: 1200}, MaxWorkBytes: 1 << 20, Now: func() time.Time { return testNow },
		},
		agent: agent, ai: drafter, store: store, snapshots: snapshots, documents: documents,
	}
}

func generation() *model.OntologyGeneration {
	goal := "customer billing health"
	return &model.OntologyGeneration{
		ID: "gen-1", WorkspaceID: "ws-1", OntologyID: "ont-1", Goal: &goal, Attempts: 1,
		Input: model.OntologyGenerationInput{Tables: []model.OntologyGenerationTable{
			{ConnectorID: "conn-1", Table: "customers"}, {ConnectorID: "conn-1", Table: "refunds"},
		}},
	}
}

func asFailure(t *testing.T, err error) *ontologygen.Failure {
	t.Helper()
	var f *ontologygen.Failure
	if !errors.As(err, &f) {
		t.Fatalf("error %v is not an ontologygen.Failure", err)
	}
	return f
}

func TestGenerator(t *testing.T) {
	ctx := context.Background()

	// A first build reads the synced table from a local file and stores the agent's draft as it came.
	t.Run("stores a finished draft", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		result, err := h.generator.Run(ctx, generation())
		if err != nil {
			t.Fatal(err)
		}
		if result.Format != model.OntologyFormatAgentBundle || result.Model != "claude-opus-5-5" || result.ModelProvider != "anthropic" ||
			result.InputTokens != 1500 || result.OutputTokens != 400 {
			t.Fatalf("result = %+v", result)
		}
		var def Definition
		if err := json.Unmarshal(result.Definition, &def); err != nil {
			t.Fatal(err)
		}
		if def.Format != "agent_bundle" || def.Status != StatusDone || def.OntologyTOML != "version = \"draft\"\n" ||
			def.MappingsTOML != "ontology = \"draft\"\n" || def.Summary == nil || *def.Summary != "built a draft" ||
			def.Usage.USD != 1.25 || !strings.Contains(string(def.Draft), `"rev":1`) || !strings.Contains(string(def.Questions), `"q1"`) ||
			strings.Contains(string(def.Draft), `"questions"`) {
			t.Fatalf("definition = %s (questions are kept once, at the top level)", result.Definition)
		}

		requests := h.agent.calls()
		if len(requests) != 1 {
			t.Fatalf("agent called %d times", len(requests))
		}
		if err := contracttest.Validate(t, contracttest.Schema(t, "turn_request.json"), []byte(requests[0])); err != nil {
			t.Fatalf("request %s: %v", requests[0], err)
		}
		var req struct {
			SessionID string            `json:"session_id"`
			RequestID string            `json:"request_id"`
			State     json.RawMessage   `json:"state"`
			Sources   []json.RawMessage `json:"sources"`
			Input     json.RawMessage   `json:"input"`
			Limits    Limits            `json:"limits"`
			Models    json.RawMessage   `json:"models"`
		}
		if err := json.Unmarshal([]byte(requests[0]), &req); err != nil {
			t.Fatal(err)
		}
		models, _ := json.Marshal(houseTurn())
		if req.SessionID != "gen-1" || req.RequestID != "gen-1" || string(req.State) != "null" || req.Limits != h.generator.Limits ||
			string(req.Input) != `{"start":{"workflow":"customer billing health","goals":["customer billing health"]}}` ||
			string(req.Models) != string(models) {
			t.Fatalf("request = %+v", req)
		}
		// The key the setting resolved to goes in the call's metadata alone,
		// checked against the providers the agent calls.
		if len(h.agent.keys) != 1 || len(h.agent.keys[0]) != 1 || h.agent.keys[0][0] != testKey || strings.Contains(requests[0], testKey) {
			t.Fatalf("keys = %v", h.agent.keys)
		}
		if len(h.ai.providers) != 1 || len(h.ai.providers[0]) != 1 || h.ai.providers[0][0] != "anthropic" {
			t.Fatalf("resolved for providers %v", h.ai.providers)
		}
		keySum := sha256.Sum256([]byte(snapshotKey))
		path := "cache/" + hex.EncodeToString(keySum[:]) + ".jsonl"
		idSum := sha256.Sum256([]byte("conn-1\x00customers"))
		wantSource := `{"id":"src_` + hex.EncodeToString(idSum[:8]) + `","display_name":"Stripe / customers",` +
			`"route":{"mapping":{"trust":1}},"access":{"file":{"path":"` + path + `","format":"jsonl"}},"fingerprint":"` +
			hex.EncodeToString(keySum[:]) + `"}`
		if len(req.Sources) != 1 || string(req.Sources[0]) != wantSource {
			t.Fatalf("sources = %s, want %s (a table no longer synced is left out)", req.Sources, wantSource)
		}
		if got, err := os.ReadFile(filepath.Join(h.generator.WorkDir, path)); err != nil || string(got) != snapshotRows {
			t.Fatalf("cached snapshot = (%q, %v)", got, err)
		}
	})

	// A load needs every rule evaluated, so the version keeps the turn's counts.
	t.Run("copies the turn's rule counts into the version", func(t *testing.T) {
		withRules := strings.Replace(testResult(`"done"`, 1), `"summary"`, `"rules":{"count":4,"evaluated":3},"summary"`, 1)
		h := newHarness(t, func(stream agentv1.Agent_TurnServer) error { return send(stream, testEvents, withRules) })
		result, err := h.generator.Run(ctx, generation())
		if err != nil {
			t.Fatal(err)
		}
		var def Definition
		if err := json.Unmarshal(result.Definition, &def); err != nil {
			t.Fatal(err)
		}
		if def.Rules == nil || *def.Rules != (RuleCounts{Count: 4, Evaluated: 3}) || !strings.Contains(string(result.Definition), `"rules":{"count":4,"evaluated":3}`) {
			t.Fatalf("definition = %s", result.Definition)
		}
		// A turn that reports no counts stores none, which no load accepts.
		h = newHarness(t, respond(`"done"`))
		if result, err = h.generator.Run(ctx, generation()); err != nil || !strings.Contains(string(result.Definition), `"rules":null`) {
			t.Fatalf("without counts = (%s, %v)", result.Definition, err)
		}
	})

	// The draft routes document sets to the types they fill (a Contract
	// linked to a Customer), so each set goes as one source, routed to
	// extraction, with its newest documents in files only this generation
	// uses. Its id is the set's, which routes and Extract name it by.
	t.Run("drafts for each document set as one source", func(t *testing.T) {
		var staged map[string]string
		var h *harness
		h = newHarness(t, func(stream agentv1.Agent_TurnServer) error {
			staged = map[string]string{}
			for _, name := range []string{"doc-md.md", "doc-mdx.md", "doc-txt.txt", "doc-inv.txt"} {
				body, _ := os.ReadFile(filepath.Join(h.generator.WorkDir, "generations", "gen-1", name))
				staged[name] = string(body)
			}
			return send(stream, testEvents, testResult(`"done"`, 1))
		})
		h.documents.objects["ws/ws-1/documents/doc-md/original"] = "# MSA\nAcme Corp buys from us."
		h.documents.objects["ws/ws-1/documents/doc-txt/original"] = "Globex renews yearly."
		h.documents.objects["ws/ws-1/documents/doc-mdx/original"] = "import X from 'x'\n\n# Guide\nInitech pays {total}.\n"
		h.documents.objects["ws/ws-1/documents/doc-pdf/original"] = "%PDF-1.7"
		h.documents.objects["ws/ws-1/documents/doc-inv/original"] = "%PDF-1.7 the invoice"
		invoiceText := "ws/ws-1/documents/doc-inv/read/standard-ab.txt"
		h.documents.objects[invoiceText] = "Invoice 1042\n\fTerms\n"
		h.store.sets = map[string]*model.FoundryDocumentSet{
			"set-contracts": {ID: "set-contracts", WorkspaceID: "ws-1", Name: "Contracts", Description: "Signed customer contracts"},
			// A connector's set is named as its connection's tables are.
			"set-empty": {ID: "set-empty", WorkspaceID: "ws-1", Name: "Notes",
				Owner: &model.FoundryDocumentSetOwner{ConnectorID: "conn-1", ConnectorName: "Team Granola", Stream: "notes"}},
			"set-gone":  {ID: "set-gone", WorkspaceID: "ws-1", Name: "Gone"},
			"set-other": {ID: "set-other", WorkspaceID: "ws-2", Name: "Another workspace's"},
		}
		h.store.documents = map[string][]*model.FoundryDocument{
			"set-contracts": {
				{ID: "doc-md", Name: "msa.md", MediaType: model.FoundryMediaMarkdown, SHA256: "aa", S3Key: "ws/ws-1/documents/doc-md/original"},
				{ID: "doc-mdx", Name: "guide.mdx", MediaType: model.FoundryMediaMDX, SHA256: "ee", S3Key: "ws/ws-1/documents/doc-mdx/original"},
				{ID: "doc-txt", Name: "renewals.txt", MediaType: model.FoundryMediaPlainText, SHA256: "bb", S3Key: "ws/ws-1/documents/doc-txt/original"},
				// A read PDF is staged as the text its read stored.
				{ID: "doc-inv", Name: "invoice.pdf", MediaType: model.FoundryMediaPDF, SHA256: "ff", S3Key: "ws/ws-1/documents/doc-inv/original",
					Read: &model.FoundryFileRead{Status: model.FoundryFileReadSucceeded, ArtifactKey: &invoiceText}},
				// The listing returns only documents with text; one that slips through is still not staged.
				{ID: "doc-pdf", Name: "scan.pdf", MediaType: model.FoundryMediaPDF, SHA256: "dd", S3Key: "ws/ws-1/documents/doc-pdf/original"},
			},
			"set-gone":  {{ID: "doc-gone", Name: "gone.md", MediaType: model.FoundryMediaMarkdown, S3Key: "ws/ws-1/documents/doc-gone/original"}},
			"set-other": {{ID: "doc-md", Name: "msa.md", MediaType: model.FoundryMediaMarkdown, S3Key: "ws/ws-1/documents/doc-md/original"}},
		}
		gen := generation()
		gen.Input.DocumentSetIDs = []string{"set-contracts", "set-empty", "set-deleted", "set-gone", "set-other"}
		if _, err := h.generator.Run(ctx, gen); err != nil {
			t.Fatal(err)
		}
		request := h.agent.calls()[0]
		if err := contracttest.Validate(t, contracttest.Schema(t, "turn_request.json"), []byte(request)); err != nil {
			t.Fatalf("request %s: %v", request, err)
		}
		var req struct {
			Sources []json.RawMessage `json:"sources"`
		}
		if err := json.Unmarshal([]byte(request), &req); err != nil {
			t.Fatal(err)
		}
		// The fingerprint hashes the text staged, not the original, so an MDX
		// blanked otherwise is measured afresh.
		mdx := "                 \n\n# Guide\nInitech pays        .\n"
		textHash := func(text string) string { sum := sha256.Sum256([]byte(text)); return hex.EncodeToString(sum[:]) }
		fingerprint := sha256.Sum256([]byte("doc-md " + textHash("# MSA\nAcme Corp buys from us.") + "\ndoc-mdx " + textHash(mdx) +
			"\ndoc-txt " + textHash("Globex renews yearly.") + "\ndoc-inv " + textHash("Invoice 1042\n\fTerms\n") + "\n"))
		want := `{"id":"set-contracts","display_name":"Contracts","description":"Signed customer contracts","route":"extraction",` +
			`"access":{"documents":{"files":[` +
			`{"doc_id":"doc-md","path":"generations/gen-1/doc-md.md","media_type":"markdown","title":"msa.md"},` +
			`{"doc_id":"doc-mdx","path":"generations/gen-1/doc-mdx.md","media_type":"markdown","title":"guide.mdx"},` +
			`{"doc_id":"doc-txt","path":"generations/gen-1/doc-txt.txt","media_type":"plain_text","title":"renewals.txt"},` +
			`{"doc_id":"doc-inv","path":"generations/gen-1/doc-inv.txt","media_type":"plain_text","title":"invoice.pdf"}]}},` +
			`"fingerprint":"` + hex.EncodeToString(fingerprint[:]) + `"}`
		if len(req.Sources) != 4 || string(req.Sources[1]) != want {
			t.Fatalf("sources = %s, want the table then %s and both empty live sets", req.Sources, want)
		}
		emptyFingerprint := sha256.Sum256(nil)
		for i, set := range []struct{ id, name string }{{"set-empty", "Team Granola / Notes"}, {"set-gone", "Gone"}} {
			want := `{"id":"` + set.id + `","display_name":"` + set.name + `","route":"extraction",` +
				`"access":{"documents":{"files":[]}},"fingerprint":"` + hex.EncodeToString(emptyFingerprint[:]) + `"}`
			if string(req.Sources[i+2]) != want {
				t.Fatalf("empty source = %s, want %s", req.Sources[i+2], want)
			}
		}
		if staged["doc-md.md"] != "# MSA\nAcme Corp buys from us." || staged["doc-mdx.md"] != mdx || staged["doc-txt.txt"] != "Globex renews yearly." ||
			staged["doc-inv.txt"] != "Invoice 1042\n\fTerms\n" {
			t.Fatalf("the agent found %q", staged)
		}
		for _, limit := range h.store.limits {
			if limit != MaxSetDocuments {
				t.Fatalf("listed a set's documents with limit %d, not %d", limit, MaxSetDocuments)
			}
		}
		if _, err := os.Stat(filepath.Join(h.generator.WorkDir, "generations", "gen-1")); !os.IsNotExist(err) {
			t.Fatalf("the generation's documents outlived it: %v", err)
		}
	})

	// A document S3 refuses (a role that can't list sees a missing key so)
	// can't be told from a transient failure, nor can a failed listing;
	// another attempt may read them.
	t.Run("retries when a document set can't be read", func(t *testing.T) {
		for name, fail := range map[string]func(h *harness){
			"document refused": func(h *harness) { h.documents.refused = errors.New("api error AccessDenied: Access Denied") },
			"listing failed":   func(h *harness) { h.store.listErr = errors.New("connection reset") },
		} {
			h := newHarness(t, respond(`"done"`))
			h.store.sets = map[string]*model.FoundryDocumentSet{"set-1": {ID: "set-1", WorkspaceID: "ws-1", Name: "General"}}
			h.store.documents = map[string][]*model.FoundryDocument{
				"set-1": {{ID: "doc-md", Name: "msa.md", MediaType: model.FoundryMediaMarkdown, S3Key: "k"}},
			}
			fail(h)
			gen := generation()
			gen.Input.DocumentSetIDs = []string{"set-1"}
			_, err := h.generator.Run(ctx, gen)
			if f := asFailure(t, err); f.Permanent || !strings.Contains(f.Message, "documents") || len(h.agent.calls()) != 0 {
				t.Fatalf("%s: failure %+v after %d calls", name, f, len(h.agent.calls()))
			}
		}
	})

	// A draft with open questions is still a draft; the website shows the questions.
	t.Run("stores a draft that needs input, with its questions", func(t *testing.T) {
		for status, want := range map[string]StatusKind{`"needs_input"`: StatusNeedsInput, `"budget_exhausted"`: StatusBudgetExhausted} {
			h := newHarness(t, respond(status))
			result, err := h.generator.Run(ctx, generation())
			if err != nil {
				t.Fatalf("%s: %v", status, err)
			}
			var def Definition
			if err := json.Unmarshal(result.Definition, &def); err != nil {
				t.Fatal(err)
			}
			var questions []struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(def.Questions, &questions); err != nil || def.Status != want ||
				len(questions) != 1 || questions[0].ID != "q1" || questions[0].Text != "Is a customer an account?" {
				t.Fatalf("%s: definition %s", status, result.Definition)
			}
		}
	})

	t.Run("briefs the agent without a goal", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		gen := generation()
		gen.Goal = nil
		if _, err := h.generator.Run(ctx, gen); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(h.agent.calls()[0], `"input":{"start":{"workflow":"`+defaultWorkflow+`","goals":[]}}`) {
			t.Fatalf("request = %s", h.agent.calls()[0])
		}
	})

	// The setting the workspace resolved to says what its model's failure
	// means; the agent's result says only which failure it was.
	t.Run("words a model's failure as the setting's copy does", func(t *testing.T) {
		for reason, permanent := range map[string]bool{"unavailable": false, "refused": true, "auth": false} {
			h := newHarness(t, respond(`{"model_failed":{"reason":"`+reason+`"}}`))
			result, err := h.generator.Run(ctx, generation())
			if f := asFailure(t, err); f.Message != "the model failed: "+reason || f.Permanent != permanent {
				t.Fatalf("%s: failure %+v", reason, f)
			}
			if result.Model != "claude-opus-5-5" || result.InputTokens != 1500 {
				t.Fatalf("%s: result %+v keeps what it ran on and spent", reason, result)
			}
		}
	})

	// A setting that can't run fails as the resolver words it, before any
	// file is read or the agent called.
	t.Run("stops at a setting that can't run", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		h.ai.err = &ontologygen.Failure{Message: "No AI provider is set up.", Permanent: true}
		_, err := h.generator.Run(ctx, generation())
		if f := asFailure(t, err); f.Message != "No AI provider is set up." || !f.Permanent || len(h.agent.calls()) != 0 || h.snapshots.opens != 0 {
			t.Fatalf("failure %+v, %d calls", f, len(h.agent.calls()))
		}
	})

	// The model recorded is the one the turn says it started on.
	t.Run("records the model the turn started on", func(t *testing.T) {
		started := `{"event":"turn_started","contract_version":"4.1.1","model":{"provider":"bedrock","name":"us.anthropic.claude-opus-5-5"}}`
		h := newHarness(t, func(stream agentv1.Agent_TurnServer) error {
			return send(stream, append([]string{started}, testEvents[1:]...), testResult(`"done"`, 1))
		})
		result, err := h.generator.Run(ctx, generation())
		if err != nil || result.Model != "us.anthropic.claude-opus-5-5" || result.ModelProvider != "bedrock" {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})

	// Retryable failures leave the generation for another attempt, which sends the same request.
	t.Run("retries what another attempt can fix", func(t *testing.T) {
		for name, s := range map[string]script{
			"model unavailable":  respond(`{"model_failed":{"reason":"unavailable"}}`),
			"source unavailable": respond(`{"source_unavailable":{"source":"src_1"}}`),
			"unavailable":        refuse(codes.Unavailable),
			"busy":               refuse(codes.ResourceExhausted),
			"internal":           refuse(codes.Internal),
			"stream ends early":  func(stream agentv1.Agent_TurnServer) error { return send(stream, testEvents[:1], "") },
		} {
			h := newHarness(t, s)
			result, err := h.generator.Run(ctx, generation())
			if f := asFailure(t, err); f.Permanent || f.Message == "" {
				t.Fatalf("%s: failure %+v", name, f)
			}
			if result.Format != model.OntologyFormatAgentBundle || result.Definition != nil {
				t.Fatalf("%s: result %+v", name, result)
			}
		}
	})

	t.Run("fails what a retry cannot fix", func(t *testing.T) {
		for name, s := range map[string]script{
			"refusal":          respond(`{"model_failed":{"reason":"refused"}}`),
			"invalid input":    respond(`{"invalid_input":{"errors":[]}}`),
			"stale draft":      respond(`"stale_draft"`),
			"approval":         respond(`"approved"`),
			"invalid argument": refuse(codes.InvalidArgument),
			"unimplemented":    refuse(codes.Unimplemented),
			"unpriced models":  refuse(codes.FailedPrecondition),
			"unreadable result": func(stream agentv1.Agent_TurnServer) error {
				return send(stream, nil, `{"status":"thinking"}`)
			},
		} {
			h := newHarness(t, s)
			if f := asFailure(t, func() error { _, err := h.generator.Run(ctx, generation()); return err }()); !f.Permanent {
				t.Fatalf("%s: failure %+v", name, f)
			}
		}
	})

	// The agent couldn't read a file: the next attempt downloads it again.
	t.Run("downloads the files again after a source was unavailable", func(t *testing.T) {
		h := newHarness(t, respond(`{"source_unavailable":{"source":"src_1"}}`), respond(`"done"`))
		if _, err := h.generator.Run(ctx, generation()); err == nil {
			t.Fatal("the first attempt succeeded")
		}
		if _, err := h.generator.Run(ctx, generation()); err != nil {
			t.Fatal(err)
		}
		requests := h.agent.calls()
		if h.snapshots.opens != 2 || len(requests) != 2 || requests[0] != requests[1] {
			t.Fatalf("downloads %d; requests differ: %v", h.snapshots.opens, requests)
		}
	})

	t.Run("reuses cached files across attempts", func(t *testing.T) {
		h := newHarness(t, refuse(codes.Unavailable), respond(`"done"`))
		_, _ = h.generator.Run(ctx, generation())
		if _, err := h.generator.Run(ctx, generation()); err != nil {
			t.Fatal(err)
		}
		if requests := h.agent.calls(); h.snapshots.opens != 1 || requests[0] != requests[1] {
			t.Fatalf("downloads %d; a retry must send the same request", h.snapshots.opens)
		}
	})

	// The agent (tonic) won't encode a result past 32 MiB and ends the stream
	// with OUT_OF_RANGE after its events; retrying rebuilds the same draft.
	t.Run("fails a draft larger than the message limit", func(t *testing.T) {
		for name, code := range map[string]codes.Code{"agent's limit": codes.OutOfRange, "client's limit": codes.ResourceExhausted} {
			h := newHarness(t, func(stream agentv1.Agent_TurnServer) error {
				if err := send(stream, testEvents[:1], ""); err != nil {
					return err
				}
				return status.Error(code, "Error, encoded message length too large")
			})
			_, err := h.generator.Run(ctx, generation())
			if f := asFailure(t, err); !f.Permanent || !strings.Contains(f.Message, "larger than 32 MiB") {
				t.Fatalf("%s: failure %+v", name, f)
			}
		}
	})

	// Readers fetch a version through gRPC clients with a 4 MiB limit.
	t.Run("fails a draft too large to store", func(t *testing.T) {
		big := strings.Replace(testResult(`"done"`, 1), `"summary":"built a draft"`, `"summary":"`+strings.Repeat("x", maxDefinitionBytes)+`"`, 1)
		h := newHarness(t, func(stream agentv1.Agent_TurnServer) error { return send(stream, testEvents, big) })
		result, err := h.generator.Run(ctx, generation())
		if f := asFailure(t, err); !f.Permanent || !strings.Contains(f.Message, "up to 3 MiB") || result.Definition != nil {
			t.Fatalf("failure %+v", f)
		}
	})

	// A deadline or a lost lease ends the call, and the agent stops; the attempt is retried and its spend kept.
	t.Run("ends the agent's turn when the run is cancelled", func(t *testing.T) {
		for name, stop := range map[string]func(context.Context) (context.Context, context.CancelFunc){
			"deadline": func(ctx context.Context) (context.Context, context.CancelFunc) {
				return context.WithTimeout(ctx, 300*time.Millisecond)
			},
			"cancel": context.WithCancel,
		} {
			cancelled := make(chan struct{}, 1)
			h := newHarness(t, hang(cancelled))
			runCtx, cancel := stop(ctx)
			if name == "cancel" {
				time.AfterFunc(300*time.Millisecond, cancel)
			}
			result, err := h.generator.Run(runCtx, generation())
			cancel()
			select {
			case <-cancelled:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s: the agent's stream was not cancelled", name)
			}
			if f := asFailure(t, err); f.Permanent || result.InputTokens != 10 || result.OutputTokens != 5 {
				t.Fatalf("%s: failure %+v, result %+v", name, f, result)
			}
		}
	})

	// Downloads run under the attempt's deadline; the call gets the agent's cap plus a tail of its own.
	t.Run("bounds the agent's call by its wall-time cap", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		before := time.Now()
		if _, err := h.generator.Run(ctx, generation()); err != nil {
			t.Fatal(err)
		}
		want := before.Add(time.Duration(h.generator.Limits.MaxWallSecs)*time.Second + TurnTail)
		if got := h.agent.deadlines[0]; got.Before(want) || got.After(want.Add(5*time.Second)) {
			t.Fatalf("the call's deadline is %s, want about %s", got, want)
		}
	})

	// After a rollout the worker can claim a generation before its sidecar listens.
	t.Run("waits for an agent that is still starting", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := lis.Addr().String()
		_ = lis.Close()
		client, err := Dial(addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		h.generator.Agent = client
		srv := grpc.NewServer()
		agentv1.RegisterAgentServer(srv, h.agent)
		t.Cleanup(srv.Stop)
		time.AfterFunc(500*time.Millisecond, func() {
			if lis, err := net.Listen("tcp", addr); err == nil {
				_ = srv.Serve(lis)
			}
		})
		if _, err := h.generator.Run(ctx, generation()); err != nil {
			t.Fatalf("a starting agent failed the attempt: %v", err)
		}
	})

	// A failed attempt is still billed: its tokens come from the agent's last usage event.
	t.Run("counts a failed attempt's tokens", func(t *testing.T) {
		h := newHarness(t, func(stream agentv1.Agent_TurnServer) error {
			if err := send(stream, testEvents[:4], ""); err != nil {
				return err
			}
			return status.Error(codes.Unavailable, "shutting down")
		})
		result, err := h.generator.Run(ctx, generation())
		if err == nil || result.InputTokens != 10 || result.OutputTokens != 5 {
			t.Fatalf("result %+v, err %v", result, err)
		}
	})

	t.Run("checks the agent's contract first", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		h.agent.setInfo(&agentv1.GetInfoResponse{ContractVersion: "0.9.0"}, nil)
		_, err := h.generator.Run(ctx, generation())
		if f := asFailure(t, err); !f.Permanent || len(h.agent.calls()) != 0 {
			t.Fatalf("failure %+v after %d calls", f, len(h.agent.calls()))
		}
		h.agent.setInfo(nil, status.Error(codes.Unavailable, "starting"))
		_, err = h.generator.Run(ctx, generation())
		if f := asFailure(t, err); f.Permanent {
			t.Fatalf("an agent still starting failed the generation for good: %+v", f)
		}
	})

	t.Run("fails for good without synced tables or a workspace", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		h.store.tables = nil
		_, err := h.generator.Run(ctx, generation())
		if f := asFailure(t, err); !f.Permanent || !strings.Contains(f.Message, "synced") {
			t.Fatalf("failure %+v", f)
		}
		h.store.workspace = nil
		_, err = h.generator.Run(ctx, generation())
		if f := asFailure(t, err); !f.Permanent || f.Message != "the workspace was deleted" || len(h.agent.calls()) != 0 {
			t.Fatalf("failure %+v", f)
		}
	})

	t.Run("retries when a snapshot can't be read", func(t *testing.T) {
		for name, data := range map[string][]byte{"missing": nil, "not gzip": []byte("{}\n"), "truncated": {0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff, 1}} {
			h := newHarness(t, respond(`"done"`))
			h.snapshots.objects[snapshotKey] = data
			if data == nil {
				delete(h.snapshots.objects, snapshotKey)
			}
			_, err := h.generator.Run(ctx, generation())
			if f := asFailure(t, err); f.Permanent || len(h.agent.calls()) != 0 {
				t.Fatalf("%s: failure %+v", name, f)
			}
			entries, _ := os.ReadDir(filepath.Join(h.generator.WorkDir, "cache"))
			if len(entries) != 0 {
				t.Fatalf("%s: a failed download left %d files", name, len(entries))
			}
		}
	})

	// A pod runs one attempt at a time, so what this attempt does not read is
	// only taking the volume's room: leftovers of crashed downloads included.
	t.Run("removes cache files the attempt does not use", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		cache := filepath.Join(h.generator.WorkDir, "cache")
		if err := os.MkdirAll(filepath.Join(cache, "stale-dir"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"old.jsonl", "recent.jsonl", "x.jsonl.123.tmp"} {
			if err := os.WriteFile(filepath.Join(cache, name), []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := h.generator.Run(ctx, generation()); err != nil {
			t.Fatal(err)
		}
		entries, _ := os.ReadDir(cache)
		keySum := sha256.Sum256([]byte(snapshotKey))
		if len(entries) != 1 || entries[0].Name() != hex.EncodeToString(keySum[:])+".jsonl" {
			t.Fatalf("cache holds %v", entries)
		}
	})
}

// TestSnapshotCacheBudget bounds the snapshots one attempt unpacks: the
// volume they land on is only so big.
func TestSnapshotCacheBudget(t *testing.T) {
	ctx := context.Background()
	rows := strings.Repeat("{\"id\":\"cus_1\"}\n", 10) // 150 bytes
	gzipped := func(body string) []byte {
		var gz bytes.Buffer
		w := gzip.NewWriter(&gz)
		_, _ = w.Write([]byte(body))
		_ = w.Close()
		return gz.Bytes()
	}
	newCache := func(t *testing.T, maxBytes int64) (*SnapshotCache, []model.OntologyGenerationTable) {
		store := &memStore{workspace: &workspace{ID: "ws-1"}, tables: []*model.ConnectorTable{
			{ConnectorID: "conn-1", TableName: "a", S3Key: "key-a.jsonl.gz"}, {ConnectorID: "conn-1", TableName: "b", S3Key: "key-b.jsonl.gz"},
		}}
		snapshots := &memSnapshots{objects: map[string][]byte{"key-a.jsonl.gz": gzipped(rows), "key-b.jsonl.gz": gzipped(rows)}}
		return &SnapshotCache{DB: store, Snapshots: snapshots, WorkDir: t.TempDir(), Dir: GenerationCache, MaxBytes: maxBytes, Now: func() time.Time { return testNow }},
			[]model.OntologyGenerationTable{{ConnectorID: "conn-1", Table: "a"}, {ConnectorID: "conn-1", Table: "b"}}
	}
	t.Run("reads tables that fit exactly", func(t *testing.T) {
		cache, tables := newCache(t, 300)
		sources, err := cache.Sources(ctx, "ws-1", tables, nil)
		if err != nil || len(sources) != 2 {
			t.Fatalf("sources = %v, err = %v", sources, err)
		}
		// Cached files count at their size and are not downloaded again.
		if _, err := cache.Sources(ctx, "ws-1", tables, nil); err != nil {
			t.Fatal(err)
		}
		if opens := cache.Snapshots.(*memSnapshots).opens; opens != 2 {
			t.Fatalf("opened %d snapshots", opens)
		}
	})
	t.Run("fails for good over the budget", func(t *testing.T) {
		cache, tables := newCache(t, 299)
		_, err := cache.Sources(ctx, "ws-1", tables, nil)
		if f := asFailure(t, err); !f.Permanent || f.Message != "the tables chosen hold more than 299 bytes once unpacked; choose fewer tables" {
			t.Fatalf("failure %+v", f)
		}
		entries, _ := os.ReadDir(filepath.Join(cache.WorkDir, "cache"))
		if len(entries) != 1 {
			t.Fatalf("cache holds %d files; the one over the budget must not stay", len(entries))
		}
	})
	// Local mode runs the ontology and graph workers in one process on one
	// files root, at once: neither prunes the other's files.
	t.Run("keeps another worker's files on a shared files root", func(t *testing.T) {
		generation, tables := newCache(t, 300)
		graph := *generation
		graph.Dir = GraphCache
		loaded, err := graph.Sources(ctx, "ws-1", tables[:1], nil)
		if err != nil {
			t.Fatal(err)
		}
		drafted, err := generation.Sources(ctx, "ws-1", tables[1:], nil)
		if err != nil {
			t.Fatal(err)
		}
		if loaded[0].Path == drafted[0].Path || !strings.HasPrefix(loaded[0].Path, GraphCache+"/") {
			t.Fatalf("paths %q and %q", loaded[0].Path, drafted[0].Path)
		}
		for _, src := range []Source{loaded[0], drafted[0]} {
			if _, err := os.Stat(filepath.Join(generation.WorkDir, src.Path)); err != nil {
				t.Fatalf("%s: %v", src.Path, err)
			}
		}
	})
	t.Run("refuses a directory that is not a worker's", func(t *testing.T) {
		for _, dir := range []string{"", ".", "../cache", "generations"} {
			cache, tables := newCache(t, 300)
			cache.Dir = dir
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("Dir %q: no panic", dir)
					}
				}()
				_, _ = cache.Sources(ctx, "ws-1", tables, nil)
			}()
		}
	})
	t.Run("counts files already cached", func(t *testing.T) {
		cache, tables := newCache(t, 300)
		if _, err := cache.Sources(ctx, "ws-1", tables, nil); err != nil {
			t.Fatal(err)
		}
		cache.MaxBytes = 149
		_, err := cache.Sources(ctx, "ws-1", tables[:1], nil)
		if f := asFailure(t, err); !f.Permanent {
			t.Fatalf("failure %+v", f)
		}
	})
}

// An event source's table tells the agent what each of its pieces is, in the
// source's own words; another kind's table has no description to give.
func TestTableSourceDescription(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte(snapshotRows))
	_ = w.Close()
	store := &memStore{workspace: &workspace{ID: "ws-1"},
		connectors: []*model.Connector{
			{ID: "conn-1", WorkspaceID: "ws-1", Kind: "stripe", Name: "Stripe", Config: []byte(`{}`)},
			{ID: "ev-1", WorkspaceID: "ws-1", Kind: "events", Name: "Orders feed",
				Config: []byte(`{"description":"An order placed in the shop","schema":{"amount":"number"}}`)},
		},
		tables: []*model.ConnectorTable{
			{ConnectorID: "conn-1", TableName: "customers", S3Key: "key-a.jsonl.gz"}, {ConnectorID: "ev-1", TableName: "events", S3Key: "key-b.jsonl.gz"},
		}}
	cache := &SnapshotCache{DB: store, Snapshots: &memSnapshots{objects: map[string][]byte{"key-a.jsonl.gz": gz.Bytes(), "key-b.jsonl.gz": gz.Bytes()}},
		WorkDir: t.TempDir(), Dir: GenerationCache, MaxBytes: 1 << 20, Now: func() time.Time { return testNow }}
	sources, err := cache.Sources(context.Background(), "ws-1",
		[]model.OntologyGenerationTable{{ConnectorID: "conn-1", Table: "customers"}, {ConnectorID: "ev-1", Table: "events"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, src := range sources {
		descriptor, err := json.Marshal(src)
		if err != nil {
			t.Fatal(err)
		}
		var sent struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal(descriptor, &sent)
		got[src.DisplayName] = sent.Description
	}
	if want := map[string]string{"Stripe / customers": "", "Orders feed / events": "An order placed in the shop"}; !maps.Equal(got, want) {
		t.Fatalf("descriptions sent = %v, want %v", got, want)
	}
}

// A load's sources say when their data was observed: each table the sync
// its snapshot was pinned at, read from the same row as the snapshot and
// kept on the pin so every attempt says the same; each table file its
// upload. An Ingest records it on the transaction of each chunk.
func TestPinnedObserved(t *testing.T) {
	ctx := context.Background()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte(snapshotRows))
	_ = w.Close()
	// A sync's time as MySQL hands it back, in the server's zone.
	synced := time.Date(2026, 10, 20, 8, 0, 0, 123456000, time.FixedZone("PDT", -7*3600))
	uploaded := time.Date(2026, 10, 19, 12, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	rowsKey := "ws/ws-1/documents/doc-csv/rows.jsonl.gz"
	tables := []model.OntologyGenerationTable{{ConnectorID: "conn-1", Table: "customers"}, {ConnectorID: "conn-1", Table: "refunds"}}
	files := []DocumentTable{{SetName: "Exports", Document: &model.FoundryDocument{
		ID: "doc-csv", Name: "orders.csv", MediaType: model.FoundryMediaCSV, UploadedAt: &uploaded,
		Read: &model.FoundryFileRead{Status: model.FoundryFileReadSucceeded, ArtifactKey: &rowsKey},
	}}}
	newCache := func(t *testing.T) *SnapshotCache {
		store := &memStore{workspace: &workspace{ID: "ws-1"}, tables: []*model.ConnectorTable{
			{ConnectorID: "conn-1", TableName: "customers", S3Key: "s1-customers.jsonl.gz", SyncID: "sync-1", SyncedAt: synced, ChangeSeq: 42},
			{ConnectorID: "conn-1", TableName: "refunds", S3Key: "s1-refunds.jsonl.gz", SyncID: "sync-1", SyncedAt: synced},
		}}
		objects := map[string][]byte{rowsKey: gz.Bytes()}
		for _, key := range []string{"s1-customers.jsonl.gz", "s1-refunds.jsonl.gz", "s2-customers.jsonl.gz"} {
			objects[key] = gz.Bytes()
		}
		return &SnapshotCache{DB: store, Snapshots: &memSnapshots{objects: objects}, WorkDir: t.TempDir(), Dir: GraphCache, MaxBytes: 1 << 20,
			Now: func() time.Time { return testNow }}
	}
	observed := func(t *testing.T, sources []Source) []string {
		t.Helper()
		out := make([]string, 0, len(sources))
		for _, src := range sources {
			descriptor, err := json.Marshal(src)
			if err != nil {
				t.Fatal(err)
			}
			var sent struct {
				Observed json.RawMessage `json:"observed"`
			}
			_ = json.Unmarshal(descriptor, &sent)
			out = append(out, string(sent.Observed))
		}
		return out
	}
	want := []string{
		`{"sync":{"at":"2026-10-20T15:00:00.123456Z","sync_id":"sync-1","change_seq":42}}`,
		`{"sync":{"at":"2026-10-20T15:00:00.123456Z","sync_id":"sync-1","change_seq":0}}`,
		`{"upload":{"document_id":"doc-csv","uploaded_at":"2026-10-19T10:30:00Z"}}`,
	}

	t.Run("pins each table's sync and says it", func(t *testing.T) {
		cache := newCache(t)
		var recorded []model.FoundryLoadSource
		sources, pins, err := cache.Pinned(ctx, "ws-1", tables, files, nil, func(p []model.FoundryLoadSource) error {
			recorded = p
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		wantPins := []model.FoundryLoadSource{
			{ConnectorID: "conn-1", Table: "customers", S3Key: "s1-customers.jsonl.gz", SyncID: "sync-1", SyncedAt: synced.UTC(), ChangeSeq: 42},
			{ConnectorID: "conn-1", Table: "refunds", S3Key: "s1-refunds.jsonl.gz", SyncID: "sync-1", SyncedAt: synced.UTC()},
		}
		if !reflect.DeepEqual(recorded, wantPins) || !reflect.DeepEqual(pins, wantPins) {
			t.Fatalf("recorded %+v, returned %+v, want %+v", recorded, pins, wantPins)
		}
		if got := observed(t, sources); !slices.Equal(got, want) {
			t.Fatalf("observed = %v, want %v", got, want)
		}
		// A pin survives the JSON column it is kept in whole.
		raw, _ := json.Marshal(pins)
		var stored []model.FoundryLoadSource
		if err := json.Unmarshal(raw, &stored); err != nil || !reflect.DeepEqual(stored, wantPins) {
			t.Fatalf("stored %s reads back as %+v, %v", raw, stored, err)
		}
	})

	t.Run("a retry says the pinned sync after another sync", func(t *testing.T) {
		cache := newCache(t)
		_, pins, err := cache.Pinned(ctx, "ws-1", tables, files, nil, func([]model.FoundryLoadSource) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		cache.DB.(*memStore).tables[0] = &model.ConnectorTable{ConnectorID: "conn-1", TableName: "customers", S3Key: "s2-customers.jsonl.gz",
			SyncID: "sync-2", SyncedAt: synced.Add(time.Hour), ChangeSeq: 43}
		sources, again, err := cache.Pinned(ctx, "ws-1", tables, files, pins, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := observed(t, sources); !slices.Equal(got, want) || !reflect.DeepEqual(again, pins) {
			t.Fatalf("observed = %v, pins %+v; want %v and the first pins", got, again, want)
		}
	})

	// A job pinned before pins kept their sync says nothing rather than
	// a sync it doesn't know; the table file still says its upload.
	t.Run("a pin without its sync says nothing", func(t *testing.T) {
		cache := newCache(t)
		old := []model.FoundryLoadSource{{ConnectorID: "conn-1", Table: "customers", S3Key: "s1-customers.jsonl.gz"}}
		sources, _, err := cache.Pinned(ctx, "ws-1", tables[:1], files, old, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := observed(t, sources); !slices.Equal(got, []string{"", want[2]}) {
			t.Fatalf("observed = %v", got)
		}
	})

	// A table file without its upload time says nothing rather than a zero time.
	t.Run("a table file without its upload time says nothing", func(t *testing.T) {
		cache := newCache(t)
		unstamped := *files[0].Document
		unstamped.UploadedAt = nil
		sources, _, err := cache.Pinned(ctx, "ws-1", tables, []DocumentTable{{SetName: "Exports", Document: &unstamped}}, nil,
			func([]model.FoundryLoadSource) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if got := observed(t, sources); !slices.Equal(got, []string{want[0], want[1], ""}) {
			t.Fatalf("observed = %v", got)
		}
		ingest := IngestRequest{RequestID: "job-1", Database: "tenant-1", Load: "job-1", OntologyTOML: "o", OntologyVersion: "foundry-v1",
			Helix: HelixTarget{URL: "http://127.0.0.1:1", Token: "t", MaxRequestBytes: 1, TimeoutSecs: 1}, MappingsTOML: "m", ChunkRows: 1,
			Sources: sources}
		raw, _ := json.Marshal(ingest)
		if err := contracttest.Validate(t, contracttest.Schema(t, "ingest_request.json"), raw); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	})

	// Every source of an Ingest is one the agent takes, observed or not.
	t.Run("an ingest of them is one the agent takes", func(t *testing.T) {
		cache := newCache(t)
		sources, _, err := cache.Pinned(ctx, "ws-1", tables, files, nil, func([]model.FoundryLoadSource) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		ingest := IngestRequest{RequestID: "job-1", Database: "tenant-1", Load: "job-1", OntologyTOML: "o", OntologyVersion: "foundry-v1",
			Helix: HelixTarget{URL: "http://127.0.0.1:1", Token: "t", MaxRequestBytes: 1, TimeoutSecs: 1}, MappingsTOML: "m", ChunkRows: 1,
			Sources: append(sources, Source{ID: "src_none", DisplayName: "No sync", Path: "cache/x.jsonl", Fingerprint: "x"})}
		raw, _ := json.Marshal(ingest)
		if err := contracttest.Validate(t, contracttest.Schema(t, "ingest_request.json"), raw); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	})
}

func TestGeneratorCopiesRuleCounts(t *testing.T) {
	h := newHarness(t, func(stream agentv1.Agent_TurnServer) error {
		raw := strings.Replace(testResult(`"done"`, 1), `"summary":`, `"rules":{"count":3,"evaluated":2},"summary":`, 1)
		return send(stream, testEvents, raw)
	})
	result, err := h.generator.Run(context.Background(), generation())
	if err != nil {
		t.Fatal(err)
	}
	var def Definition
	if err := json.Unmarshal(result.Definition, &def); err != nil {
		t.Fatal(err)
	}
	if def.Rules == nil || def.Rules.Count != 3 || def.Rules.Evaluated != 2 {
		t.Fatalf("rule counts lost: %+v", def.Rules)
	}
}

// A load ingests only a finished agent draft whose rules were all evaluated and
// whose generation names its tables; CreateFoundryLoad and the MCP's versions
// both give the customer the same reason for any other.
func TestLoadRefusal(t *testing.T) {
	generation := "gen-1"
	draft := func(definition string) *model.OntologyVersion {
		return &model.OntologyVersion{ID: "v-1", Format: model.OntologyFormatAgentBundle, GenerationID: &generation, Definition: json.RawMessage(definition)}
	}
	orphan := draft(`{"status":"done","rules":{"count":1,"evaluated":1}}`)
	orphan.GenerationID = nil
	for name, tc := range map[string]struct {
		version *model.OntologyVersion
		refusal string
	}{
		"a finished draft whose rules were all evaluated": {draft(`{"status":"done","rules":{"count":2,"evaluated":2}}`), ""},
		// A revision is a manual version, and loads like the draft it revised.
		"a revision of a draft": {func() *model.OntologyVersion {
			v := draft(`{"status":"done","parent_version_id":"v-0","rules":{"count":2,"evaluated":2}}`)
			v.Source = model.OntologySourceManual
			return v
		}(), ""},
		"a draft with no rules":              {draft(`{"status":"done","rules":{"count":0,"evaluated":0}}`), ""},
		"a hand-written version":             {&model.OntologyVersion{ID: "v-1", Format: model.OntologyFormatFoundryV1, Definition: json.RawMessage(`{}`)}, "only an ontology agent draft can be loaded; generate one"},
		"a draft that needs input":           {draft(`{"status":"needs_input","rules":{"count":1,"evaluated":1}}`), "this draft ended as needs_input; only a finished draft can be loaded"},
		"a draft out of budget":              {draft(`{"status":"budget_exhausted"}`), "this draft ended as budget_exhausted; only a finished draft can be loaded"},
		"a draft that records no evaluation": {draft(`{"status":"done","rules":null}`), "this draft records no evaluation of its rules; generate it again"},
		"a draft with rules not evaluated":   {draft(`{"status":"done","rules":{"count":3,"evaluated":2}}`), "2 of this draft's 3 rules were evaluated against data; only a draft whose rules all were can be loaded"},
		"a draft that counts more evaluated": {draft(`{"status":"done","rules":{"count":1,"evaluated":2}}`), "2 of this draft's 1 rules were evaluated against data; only a draft whose rules all were can be loaded"},
		"a draft with no generation":         {orphan, "this draft has no generation to name its tables"},
	} {
		t.Run(name, func(t *testing.T) {
			refusal, err := LoadRefusal(tc.version)
			if err != nil || refusal != tc.refusal {
				t.Fatalf("LoadRefusal = (%q, %v), want %q", refusal, err, tc.refusal)
			}
		})
	}

	// A definition that doesn't read is no refusal of the customer's: the caller fails it as internal.
	t.Run("errs on a definition that doesn't read", func(t *testing.T) {
		if refusal, err := LoadRefusal(draft(`{`)); err == nil || refusal != "" {
			t.Fatalf("LoadRefusal = (%q, %v)", refusal, err)
		}
	})
}

func TestGeneratorDocumentSources(t *testing.T) {
	ctx := context.Background()
	rowsKey := document.RowsKey("ws-1", "doc-csv")
	jsonKey := document.Key("ws-1", "doc-json")
	// files are a set's table files whose reads succeeded: a CSV converted
	// to gzipped rows, and a JSON document kept as it is.
	files := func(h *harness) {
		h.store.sets = map[string]*model.FoundryDocumentSet{
			"set-1": {ID: "set-1", WorkspaceID: "ws-1", Name: "Exports"},
			"set-2": {ID: "set-2", WorkspaceID: "ws-1", Name: "Elsewhere"},
		}
		read := func(key string) *model.FoundryFileRead {
			return &model.FoundryFileRead{Status: model.FoundryFileReadSucceeded, ArtifactKey: &key}
		}
		h.store.files = map[string]*model.FoundryDocument{
			"doc-csv": {ID: "doc-csv", WorkspaceID: "ws-1", SetID: "set-1", Name: "orders.csv", MediaType: model.FoundryMediaCSV,
				Status: model.FoundryDocumentUploaded, Read: read(rowsKey)},
			"doc-json": {ID: "doc-json", WorkspaceID: "ws-1", SetID: "set-1", Name: "invoices.json", MediaType: model.FoundryMediaJSON,
				Status: model.FoundryDocumentUploaded, Read: read(jsonKey)},
			"doc-moved": {ID: "doc-moved", WorkspaceID: "ws-1", SetID: "set-2", Name: "moved.csv", MediaType: model.FoundryMediaCSV,
				Status: model.FoundryDocumentUploaded, Read: read("k")},
			"doc-deleted": {ID: "doc-deleted", WorkspaceID: "ws-1", SetID: "set-1", Name: "gone.csv", MediaType: model.FoundryMediaCSV,
				Status: model.FoundryDocumentDeleted, Read: read("k")},
		}
		var gz bytes.Buffer
		w := gzip.NewWriter(&gz)
		_, _ = w.Write([]byte(`{"id":1,"total":10.5}` + "\n"))
		_ = w.Close()
		h.snapshots.objects[rowsKey] = gz.Bytes()
		h.snapshots.objects[jsonKey] = []byte(`{"data":[{"id":"in_1"}]}`)
	}
	recorded := []model.OntologyGenerationDocument{{ID: "doc-csv", SetID: "set-1"}, {ID: "doc-json", SetID: "set-1"},
		{ID: "doc-moved", SetID: "set-1"}, {ID: "doc-deleted", SetID: "set-1"}}
	// sources reads the turn request's sources, checked against the vendored schema.
	sources := func(t *testing.T, h *harness) []map[string]any {
		t.Helper()
		request := h.agent.calls()[0]
		if err := contracttest.Validate(t, contracttest.Schema(t, "turn_request.json"), []byte(request)); err != nil {
			t.Fatalf("request %s: %v", request, err)
		}
		var req struct {
			Sources []map[string]any `json:"sources"`
		}
		if err := json.Unmarshal([]byte(request), &req); err != nil {
			t.Fatal(err)
		}
		return req.Sources
	}

	// A table file is a table of its own beside the synced ones: its source
	// id is its document id, it is named "Set / file", and its file is its
	// read, decompressed, in JSON Lines or JSON. A file deleted or moved
	// out of the sets since is left out.
	t.Run("drafts from table files beside the synced tables", func(t *testing.T) {
		var cached map[string]string
		var h *harness
		h = newHarness(t, func(stream agentv1.Agent_TurnServer) error {
			cached = map[string]string{}
			entries, _ := os.ReadDir(filepath.Join(h.generator.WorkDir, "cache"))
			for _, e := range entries {
				body, _ := os.ReadFile(filepath.Join(h.generator.WorkDir, "cache", e.Name()))
				cached[filepath.Ext(e.Name())] += string(body)
			}
			return send(stream, testEvents, testResult(`"done"`, 1))
		})
		files(h)
		gen := generation()
		gen.Input.DocumentSetIDs, gen.Input.Documents = []string{"set-1"}, recorded
		if _, err := h.generator.Run(ctx, gen); err != nil {
			t.Fatal(err)
		}
		got := sources(t, h)
		if len(got) != 4 || got[0]["id"] != TableSourceID("conn-1", "customers") {
			t.Fatalf("sources = %v: the synced table, two files, then the set", got)
		}
		hash := func(key string) string { sum := sha256.Sum256([]byte(key)); return hex.EncodeToString(sum[:]) }
		for i, want := range []struct{ id, name, ext, format, key string }{
			{"doc-csv", "Exports / orders.csv", ".jsonl", "jsonl", rowsKey},
			{"doc-json", "Exports / invoices.json", ".json", "json", jsonKey},
		} {
			file := map[string]any{"path": "cache/" + hash(want.key) + want.ext, "format": want.format}
			s := got[i+1]
			access, _ := s["access"].(map[string]any)
			if s["id"] != want.id || s["display_name"] != want.name || s["fingerprint"] != hash(want.key) ||
				fmt.Sprint(access["file"]) != fmt.Sprint(file) || fmt.Sprint(s["route"]) != "map[mapping:map[trust:1]]" {
				t.Fatalf("source %d = %v", i+1, s)
			}
		}
		if cached[".jsonl"] != snapshotRows+`{"id":1,"total":10.5}`+"\n" && cached[".jsonl"] != `{"id":1,"total":10.5}`+"\n"+snapshotRows {
			t.Fatalf("cached JSON Lines %q", cached[".jsonl"])
		}
		if cached[".json"] != `{"data":[{"id":"in_1"}]}` {
			t.Fatalf("cached JSON %q", cached[".json"])
		}
	})

	// A workspace may have only files: a generation drafts from them with
	// no synced table.
	t.Run("drafts from table files alone", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		files(h)
		gen := generation()
		gen.Input = model.OntologyGenerationInput{DocumentSetIDs: []string{"set-1"}, Documents: recorded}
		if _, err := h.generator.Run(ctx, gen); err != nil {
			t.Fatal(err)
		}
		got := sources(t, h)
		if len(got) != 3 || got[0]["id"] != "doc-csv" || got[1]["id"] != "doc-json" || got[2]["route"] != "extraction" {
			t.Fatalf("sources = %v", got)
		}
	})

	// Connector tables gone before the run leave the files: the generation
	// drafts from what is left rather than failing.
	t.Run("drafts from the files when the synced tables are gone", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		files(h)
		gen := generation()
		gen.Input = model.OntologyGenerationInput{Tables: []model.OntologyGenerationTable{{ConnectorID: "conn-1", Table: "refunds"}},
			DocumentSetIDs: []string{"set-1"}, Documents: recorded}
		if _, err := h.generator.Run(ctx, gen); err != nil {
			t.Fatal(err)
		}
		if got := sources(t, h); len(got) != 3 || got[0]["id"] != "doc-csv" {
			t.Fatalf("sources = %v", got)
		}
	})

	// A workspace may have only documents: a generation drafts from a set's
	// text, routed to extraction, with no table at all.
	t.Run("drafts from text documents alone", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		h.store.sets = map[string]*model.FoundryDocumentSet{"set-1": {ID: "set-1", WorkspaceID: "ws-1", Name: "Contracts"}}
		h.store.documents = map[string][]*model.FoundryDocument{
			"set-1": {{ID: "doc-md", Name: "msa.md", MediaType: model.FoundryMediaMarkdown, S3Key: "ws/ws-1/documents/doc-md/original"}},
		}
		h.documents.objects["ws/ws-1/documents/doc-md/original"] = "# MSA\nAcme Corp buys from us."
		gen := generation()
		gen.Input = model.OntologyGenerationInput{DocumentSetIDs: []string{"set-1"}}
		if _, err := h.generator.Run(ctx, gen); err != nil {
			t.Fatal(err)
		}
		got := sources(t, h)
		if len(got) != 1 || got[0]["id"] != "set-1" || got[0]["route"] != "extraction" {
			t.Fatalf("sources = %v", got)
		}
	})

	// With no table left and no text in the sets, there is nothing to draft
	// from: the generation fails for good, without calling the agent.
	t.Run("fails for good with nothing left to read", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		files(h)
		h.store.files["doc-csv"].Status = model.FoundryDocumentDeleted
		h.store.files["doc-json"].SetID = "set-2"
		gen := generation()
		gen.Input = model.OntologyGenerationInput{DocumentSetIDs: []string{"set-1"}, Documents: recorded}
		_, err := h.generator.Run(ctx, gen)
		if f := asFailure(t, err); !f.Permanent || !strings.Contains(f.Message, "nothing to generate from is left") || len(h.agent.calls()) != 0 {
			t.Fatalf("failure %+v after %d calls", f, len(h.agent.calls()))
		}
	})
}

// People enrichment's profiles reach a turn only with Helix's flag and the
// workspace's consent both on, as the engine's descriptor gate expects; the
// engine refuses enrichment edits in a turn without them.
func TestGeneratorEnrichmentSource(t *testing.T) {
	ctx := context.Background()
	sources := func(t *testing.T, h *harness) []json.RawMessage {
		t.Helper()
		var req struct {
			Sources []json.RawMessage `json:"sources"`
		}
		calls := h.agent.calls()
		if err := json.Unmarshal([]byte(calls[len(calls)-1]), &req); err != nil {
			t.Fatal(err)
		}
		return req.Sources
	}
	allowed := &model.FoundryEnrichmentConsent{Enabled: true, MonthlyCreditCap: 50000}
	for name, tc := range map[string]struct {
		flag    bool
		consent *model.FoundryEnrichmentConsent
		want    bool
	}{
		"flag and consent on": {flag: true, consent: allowed, want: true},
		"flag off":            {flag: false, consent: allowed},
		"consent never set":   {flag: true},
		"consent turned off":  {flag: true, consent: &model.FoundryEnrichmentConsent{Enabled: false}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, respond(`"done"`))
			h.generator.Enrichment, h.store.consent = tc.flag, tc.consent
			if _, err := h.generator.Run(ctx, generation()); err != nil {
				t.Fatal(err)
			}
			got := sources(t, h)
			calls := h.agent.calls()
			if err := contracttest.Validate(t, contracttest.Schema(t, "turn_request.json"), []byte(calls[0])); err != nil {
				t.Fatalf("request: %v", err)
			}
			has := strings.Contains(string(got[len(got)-1]), `"id":"foundry_enrichment"`)
			if has != tc.want {
				t.Fatalf("sources = %s, want the profiles: %t", got, tc.want)
			}
			if !tc.want {
				return
			}
			want := `{"id":"foundry_enrichment","display_name":"People enrichment","route":{"mapping":{"trust":0.7}},` +
				`"access":{"file":{"path":"generations/gen-1/foundry_enrichment.jsonl","format":"jsonl"}},` +
				`"fingerprint":"` + model.FoundryEnrichmentNone.SHA256 + `"}`
			if string(got[len(got)-1]) != want {
				t.Fatalf("profiles = %s", got[len(got)-1])
			}
			// The zero-byte file goes with the turn's other files.
			if _, err := os.Stat(filepath.Join(h.generator.WorkDir, "generations", "gen-1")); !os.IsNotExist(err) {
				t.Fatalf("the turn's files outlived it: %v", err)
			}
		})
	}

	// A setting Helix can't read fails the attempt for a retry, rather than
	// drafting as if consent were off.
	t.Run("retries when the setting can't be read", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		h.generator.Enrichment, h.store.consentErr = true, errors.New("mysql is down")
		_, err := h.generator.Run(ctx, generation())
		if f := asFailure(t, err); f.Permanent || len(h.agent.calls()) != 0 {
			t.Fatalf("failure = %+v after %d turns", f, len(h.agent.calls()))
		}
	})

	// The profiles are no data of the workspace's: with no table and no
	// text in the sets, there is still nothing to draft from.
	t.Run("fails for good with only the profiles left", func(t *testing.T) {
		h := newHarness(t, respond(`"done"`))
		h.generator.Enrichment, h.store.consent = true, allowed
		h.store.sets = map[string]*model.FoundryDocumentSet{"set-1": {ID: "set-1", WorkspaceID: "ws-1", Name: "Empty"}}
		gen := generation()
		gen.Input = model.OntologyGenerationInput{DocumentSetIDs: []string{"set-1"}}
		_, err := h.generator.Run(ctx, gen)
		if f := asFailure(t, err); !f.Permanent || !strings.Contains(f.Message, "nothing to generate from is left") || len(h.agent.calls()) != 0 {
			t.Fatalf("failure %+v after %d calls", f, len(h.agent.calls()))
		}
	})
}
