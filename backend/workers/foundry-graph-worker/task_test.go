package foundrygraphworker

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/boost"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/enrichment"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func (s *memStore) GetActiveFoundryAIConfig(context.Context, string) (*model.FoundryAIConfig, error) {
	if s.aiConfig == nil {
		return nil, foundrystore.ErrNotFound
	}
	return s.aiConfig, nil
}

// fakeAgent serves ontology_agent.v1.Agent (the vendored contract) in process. Each
// Ingest, Extract and DeleteLoad call runs its script and is recorded.
type fakeAgent struct {
	agentv1.UnimplementedAgentServer
	mu         sync.Mutex
	info       *agentv1.GetInfoResponse
	ingest     func(req string, stream agentv1.Agent_IngestServer) error
	extract    func(req string, stream agentv1.Agent_ExtractServer) error
	deleteLoad func(req string, stream agentv1.Agent_DeleteLoadServer) error
	clues      func(req string, stream agentv1.Agent_CluesServer) error
	ingests    []string
	extracts   []string
	deletes    []string
	// schemas, by request schema name, refuse a request as the agent would:
	// INVALID_ARGUMENT, before any event.
	schemas    map[string]*jsonschema.Resolved
	cluesCalls []string
	// keys are each Extract's model key metadata.
	keys [][]string
}

// refused is the agent's answer to a request its schema refuses, or nil.
func (f *fakeAgent) refused(schema, req string) error {
	s, ok := f.schemas[schema]
	if !ok {
		return nil
	}
	var instance any
	if err := json.Unmarshal([]byte(req), &instance); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.Validate(instance); err != nil {
		return status.Errorf(codes.InvalidArgument, "%s: %v", schema, err)
	}
	return nil
}

func (f *fakeAgent) GetInfo(context.Context, *agentv1.GetInfoRequest) (*agentv1.GetInfoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info, nil
}

func (f *fakeAgent) Ingest(req *agentv1.IngestRequest, stream agentv1.Agent_IngestServer) error {
	f.mu.Lock()
	f.ingests = append(f.ingests, req.GetRequestJson())
	script := f.ingest
	f.mu.Unlock()
	if err := f.refused("ingest_request.json", req.GetRequestJson()); err != nil {
		return err
	}
	return script(req.GetRequestJson(), stream)
}

func (f *fakeAgent) Extract(req *agentv1.ExtractRequest, stream agentv1.Agent_ExtractServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	f.mu.Lock()
	f.extracts = append(f.extracts, req.GetRequestJson())
	f.keys = append(f.keys, md.Get(ontologyagent.ModelKeyHeader))
	script := f.extract
	f.mu.Unlock()
	if err := f.refused("extract_request.json", req.GetRequestJson()); err != nil {
		return err
	}
	return script(req.GetRequestJson(), stream)
}

func (f *fakeAgent) DeleteLoad(req *agentv1.DeleteLoadRequest, stream agentv1.Agent_DeleteLoadServer) error {
	f.mu.Lock()
	f.deletes = append(f.deletes, req.GetRequestJson())
	script := f.deleteLoad
	f.mu.Unlock()
	if err := f.refused("delete_load_request.json", req.GetRequestJson()); err != nil {
		return err
	}
	return script(req.GetRequestJson(), stream)
}

func (f *fakeAgent) deleteCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func (f *fakeAgent) calls() (ingests, extracts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ingests...), append([]string(nil), f.extracts...)
}

func ingestEvents(stream agentv1.Agent_IngestServer, events []string, result string) error {
	for _, e := range events {
		if err := stream.Send(&agentv1.IngestEvent{Kind: &agentv1.IngestEvent_EventJson{EventJson: e}}); err != nil {
			return err
		}
	}
	if result == "" {
		return nil
	}
	return stream.Send(&agentv1.IngestEvent{Kind: &agentv1.IngestEvent_ResultJson{ResultJson: result}})
}

func deleteEvents(stream agentv1.Agent_DeleteLoadServer, events []string, result string) error {
	for _, e := range events {
		if err := stream.Send(&agentv1.DeleteLoadEvent{Kind: &agentv1.DeleteLoadEvent_EventJson{EventJson: e}}); err != nil {
			return err
		}
	}
	if result == "" {
		return nil
	}
	return stream.Send(&agentv1.DeleteLoadEvent{Kind: &agentv1.DeleteLoadEvent_ResultJson{ResultJson: result}})
}

// deleteDone writes one batch through the proxy and reports the load gone.
func deleteDone(req string, stream agentv1.Agent_DeleteLoadServer) error {
	if err := writeThroughProxy(req); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return deleteEvents(stream, []string{
		`{"event":"delete_started","contract_version":"3.8.0"}`, `{"event":"deleted","label":"OWNS","element":"edge","count":2}`,
		`{"event":"deleted","label":"Customer","element":"node","count":1}`, `{"event":"delete_finished"}`,
	}, `{"status":"done","nodes":1,"edges":2}`)
}

func extractEvents(stream agentv1.Agent_ExtractServer, events []string, result string) error {
	for _, e := range events {
		if err := stream.Send(&agentv1.ExtractEvent{Kind: &agentv1.ExtractEvent_EventJson{EventJson: e}}); err != nil {
			return err
		}
	}
	if result == "" {
		return nil
	}
	return stream.Send(&agentv1.ExtractEvent{Kind: &agentv1.ExtractEvent_ResultJson{ResultJson: result}})
}

// writeThroughProxy is the agent writing one chunk to the database: through
// the proxy the request names, with its token.
func writeThroughProxy(request string) error {
	var req struct {
		Helix ontologyagent.HelixTarget `json:"helix"`
	}
	if err := json.Unmarshal([]byte(request), &req); err != nil {
		return err
	}
	httpReq, err := http.NewRequest(http.MethodPost, req.Helix.URL+"/v2/query", strings.NewReader(`{"request_type":"write","query":{}}`))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+req.Helix.Token)
	httpReq.Header.Set("X-Helix-Require-Writer", "true")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy answered %d", resp.StatusCode)
	}
	return nil
}

const (
	// documentCounts are what each document of the fixtures put in the graph.
	documentCounts = `{"entities":2,"mentions":3,"facts":1,"reviews":1,"unresolved":1,"rejected":0,"attributes":4,"attribute_reviews":3,` +
		`"prop_conflicts":2,"created":1,"identifiers_unverified":5,"attributes_over_entries":6,"resolved":{"user":7,"adjudicated":8,"identifier":9,"name":2,"alias":10,"created":1}}`
	totals   = `{"ingested":1,"out_of_scope":0,"already_ingested":1,"duplicate":1,"failed":1,"counts":` + documentCounts + `,"swept":0,"unchecked":0,"stale_decisions":0,"usd_micros":1500,"input_tokens":900,"output_tokens":80}`
	counts   = `"counts":` + documentCounts
	usage    = `"usage":{"usd_micros":1500,"input_tokens":900,"output_tokens":80,"replayed":false,"phases":{"entity":{"calls":1,"replayed":0,"usd_micros":1500,"input_tokens":900,"output_tokens":80},"link":{"calls":0,"replayed":0,"usd_micros":0,"input_tokens":0,"output_tokens":0},"verify":{"calls":0,"replayed":0,"usd_micros":0,"input_tokens":0,"output_tokens":0},"adjudicate":{"calls":0,"replayed":0,"usd_micros":0,"input_tokens":0,"output_tokens":0}}}`
	noUsage  = `"usage":{"usd_micros":0,"input_tokens":0,"output_tokens":0,"replayed":true,"phases":{"entity":{"calls":0,"replayed":1,"usd_micros":0,"input_tokens":0,"output_tokens":0},"link":{"calls":0,"replayed":0,"usd_micros":0,"input_tokens":0,"output_tokens":0},"verify":{"calls":0,"replayed":0,"usd_micros":0,"input_tokens":0,"output_tokens":0},"adjudicate":{"calls":0,"replayed":0,"usd_micros":0,"input_tokens":0,"output_tokens":0}}}`
	doneInfo = ontologyagent.ContractVersion
)

func documentEvent(doc, outcome, use string) string {
	return `{"event":"document","doc_id":"` + doc + `","outcome":` + outcome + `,"text_bytes":120,"sections":2,` + counts + `,` + use + `}`
}

// ingestReport is the ingest crate's report of the customers mapping: its
// resolve pass opens two violation records and resolves one, and the sweep
// of the customers it created opens one, so the tables leave two open.
const ingestReport = `{"mappings":[{"mapping":"customers","source":"src_1","snapshot":"s1","rows":2,` +
	`"create":{"loaded":1,"skipped":0,"empty":0,"violations_opened":{"count":0,"examples":[]},"violations_resolved":{"count":0,"examples":[]}},` +
	`"resolve":{"loaded":1,"skipped":0,"empty":0,"violations_opened":{"count":2,"examples":[]},"violations_resolved":{"count":1,"examples":[]}}}],` +
	`"created":{"sweep":"swept","entities":2,"opened":{"count":1,"examples":[]},"updated":{"count":0,"examples":[]},` +
	`"closed":{"count":0,"examples":[]},"self_loops":{"count":0,"examples":[]},"unchecked":{"count":0,"examples":[]}}}`

// ingestDone writes a chunk through the proxy and reports both chunks of the
// customers mapping.
func ingestDone(req string, stream agentv1.Agent_IngestServer) error {
	return ingestReporting(ingestReport)(req, stream)
}

// ingestReporting is ingestDone ending with report.
func ingestReporting(report string) func(string, agentv1.Agent_IngestServer) error {
	return func(req string, stream agentv1.Agent_IngestServer) error {
		if err := writeThroughProxy(req); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		return ingestEvents(stream, []string{
			`{"event":"ingest_started","contract_version":"3.8.0"}`, `{"event":"indexes_ready"}`,
			`{"event":"chunk","report":{"mapping":"customers","pass":"create","rows":2}}`,
			`{"event":"chunk","report":{"mapping":"customers","pass":"resolve","rows":2}}`,
			`{"event":"sweep_page","entities":2}`, `{"event":"ingest_finished"}`,
		}, `{"status":"done","report":`+report+`}`)
	}
}

// extracted ends doc-a ingested with an artifact, doc-b a duplicate of it,
// doc-c failed retryably and doc-d already held.
var extracted = []string{
	`{"event":"extract_started","contract_version":"3.8.0","extractor":"haiku","verifier":"sol","prompt_version":"7"}`,
	`{"event":"artifact","doc_id":"doc-a","key":"key_a","json":"{\"sections\":[1]}"}`,
	documentEvent("doc-a", `{"kind":"ingested"}`, usage),
	documentEvent("doc-b", `{"kind":"duplicate","of":"doc-a"}`, noUsage),
	documentEvent("doc-c", `{"kind":"failed","code":"model","model_failure":"unavailable","retryable":true}`, noUsage),
	documentEvent("doc-d", `{"kind":"already_ingested"}`, noUsage),
	`{"event":"extract_finished"}`,
}

// extractAll ends the documents as extracted does, and the call done.
func extractAll(_ string, stream agentv1.Agent_ExtractServer) error {
	return extractEvents(stream, extracted, `{"status":"done","totals":`+totals+`}`)
}

func startAgent(t *testing.T, agent *fakeAgent) *ontologyagent.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentv1.RegisterAgentServer(server, agent)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(server.Stop)
	client, err := ontologyagent.Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// memStore is the workspace's ontology, its draft version and generation,
// its tenant on a shared cluster, the synced customers table and the
// ontology's documents; it records the job's progress and extractions.
type memStore struct {
	// aiConfig is the workspace's AI setting; nil is Helix's Claude.
	aiConfig *model.FoundryAIConfig
	mu       sync.Mutex
	pins     map[string][]model.FoundryLoadSource
	ontology *model.FoundryOntology
	tenant   *ports.Database
	// gate, when set, is why the database takes no queries now.
	gate          string
	decisions     []*model.FoundryReviewDecision
	decisionErr   error
	decisionLoads []string
	version       *model.OntologyVersion
	generation    *model.OntologyGeneration
	tables        []*model.ConnectorTable
	pending       []*model.FoundryDocument
	// pendingFor are the loads the attempt asked for the documents of.
	pendingFor  []string
	spent       int64
	updates     []model.FoundryGraphJobUpdate
	extractions map[string]*model.FoundryDocumentExtraction
	kept        map[string]bool
	recordErr   error
	renewErr    error
	// renewHangs makes each renewal wait for its context, as a database that
	// stopped answering does.
	renewHangs bool
	finished   []model.FoundryGraphJobOutcome
	claims     []*model.FoundryGraphJob
	deleted    []string
	// events is the order of boost, synced, write and unboost.
	events []string
	// acquireErr is AcquireLoadBoost's answer; acquired and released
	// are the boosts written and taken back.
	acquireErr error
	acquired   []model.FoundryLoadBoost
	released   []model.FoundryLoadBoost
	// syncedAfter is how many reads of the synced revision come before the
	// gateway has the boost's; syncReads counts them.
	syncedAfter int
	syncReads   int
	// sweeps are SweepLoadBoosts' answers in turn, then 0;
	// sweepErr fails every sweep.
	sweeps     []int
	sweepErr   error
	sweepCalls int
	// scope is the version's document sets; files the workspace's
	// documents by id, and sets its sets.
	scope []string
	files map[string]*model.FoundryDocument
	sets  []*model.FoundryDocumentSet
	// enrich is the loads' People enrichment steps (enrichment_test.go).
	enrich enrichmentState
	// spends are ws-1's connector sets' month spend by set
	// (FoundryDocumentSetSpend): each document recorded adds what it spent
	// to its set's, to its kind's workspace spend on every set, and to the
	// month's. spendReads are the reads' workspaces and sets in turn;
	// spendHook, when set, runs before each read, numbered from 1, under the
	// store's lock, and its error fails the read.
	spends     map[string]*model.FoundryDocumentSetSpend
	spendReads []spendRead
	spendHook  func(read int) error
	// loads are the ontology's loads by id (GetFoundryLoad), and prior what
	// each recorded, by load and document (ListFoundryDocumentExtractions),
	// which priorErr fails. sumErr fails SumFoundryGraphJobSpend.
	loads     map[string]*model.FoundryLoad
	loadReads int
	prior     map[string]map[string]*model.FoundryDocumentExtraction
	priorErr  error
	sumErr    error
}

// spendRead is one FoundryDocumentSetSpend read.
type spendRead struct {
	workspaceID string
	setIDs      []string
}

func (s *memStore) GetOntologyVersionScope(context.Context, string, string) ([]string, error) {
	return s.scope, nil
}

func (s *memStore) ListFoundryDocumentsByID(_ context.Context, _ string, ids []string) (map[string]*model.FoundryDocument, error) {
	out := map[string]*model.FoundryDocument{}
	for _, id := range ids {
		if d, ok := s.files[id]; ok {
			out[id] = d
		}
	}
	return out, nil
}

func (s *memStore) ListFoundryDocumentSets(context.Context, string) ([]*model.FoundryDocumentSet, error) {
	return s.sets, nil
}

// boostRevision is the revision of the cluster's gateway access that carries a boost.
const boostRevision = 7

func (s *memStore) record(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *memStore) AcquireLoadBoost(_ context.Context, _ string, b model.FoundryLoadBoost) (ports.BoostChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acquireErr != nil {
		return ports.BoostChange{}, s.acquireErr
	}
	s.acquired = append(s.acquired, b)
	s.events = append(s.events, "boost")
	return ports.BoostChange{ClusterID: "shared", Revision: boostRevision}, nil
}

func (s *memStore) ReleaseLoadBoost(ctx context.Context, b model.FoundryLoadBoost) error {
	if err := ctx.Err(); err != nil {
		return err // a release on an ended context removes nothing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, b)
	s.events = append(s.events, "unboost")
	return nil
}

func (s *memStore) SyncedRevision(_ context.Context, clusterID string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if clusterID != "shared" {
		return 0, foundrystore.ErrNotFound
	}
	s.syncReads++
	if s.syncReads <= s.syncedAfter {
		return boostRevision - 1, nil
	}
	s.events = append(s.events, "synced")
	return boostRevision, nil
}

func (s *memStore) SweepLoadBoosts(context.Context, int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepCalls++
	if s.sweepErr != nil {
		return 0, s.sweepErr
	}
	if len(s.sweeps) == 0 {
		return 0, nil
	}
	n := s.sweeps[0]
	s.sweeps = s.sweeps[1:]
	return n, nil
}

// DeleteFoundryDocument records the document as <workspace>/<id>.
func (s *memStore) DeleteFoundryDocument(_ context.Context, workspaceID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, workspaceID+"/"+id)
	return nil
}

func (s *memStore) SetFoundryGraphJobSources(_ context.Context, id, _ string, sources []model.FoundryLoadSource) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pins == nil {
		s.pins = map[string][]model.FoundryLoadSource{}
	}
	s.pins[id] = sources
	return nil
}

func (s *memStore) ListConnectors(context.Context, string) ([]*model.Connector, error) {
	return []*model.Connector{{ID: "conn-1", WorkspaceID: "ws-1", Kind: "stripe", Name: "Stripe"}}, nil
}

func (s *memStore) ListConnectorTables(context.Context, string, string) ([]*model.ConnectorTable, error) {
	return s.tables, nil
}

func (s *memStore) GetFoundryOntology(_ context.Context, workspaceID, id string) (*model.FoundryOntology, error) {
	if s.ontology == nil || s.ontology.ID != id || s.ontology.WorkspaceID != workspaceID {
		return nil, foundrystore.ErrNotFound
	}
	return s.ontology, nil
}

func (s *memStore) Get(_ context.Context, id string) (ports.Database, error) {
	if s.tenant.TenantID != id {
		return ports.Database{}, foundrystore.ErrNotFound
	}
	return *s.tenant, nil
}

func (s *memStore) Live(ctx context.Context, id string) (ports.Database, error) {
	return s.Get(ctx, id)
}

func (s *memStore) Endpoint(_ context.Context, id, path string) (string, error) {
	if s.gate != "" {
		return "", status.Error(codes.FailedPrecondition, s.gate)
	}
	return "https://abc.cluster.example.com" + path, nil
}

func (s *memStore) GetOntologyVersion(_ context.Context, ontologyID, id string) (*model.OntologyVersion, error) {
	if s.version == nil || s.version.ID != id || s.version.OntologyID != ontologyID {
		return nil, foundrystore.ErrNotFound
	}
	return s.version, nil
}

func (s *memStore) GetOntologyGeneration(_ context.Context, _, id string) (*model.OntologyGeneration, error) {
	if s.generation == nil || s.generation.ID != id {
		return nil, foundrystore.ErrNotFound
	}
	return s.generation, nil
}

func (s *memStore) ListFoundryLoadDecisions(_ context.Context, loadID, after string, docIDs []string, limit int) ([]*model.FoundryReviewDecision, error) {
	s.decisionLoads = append(s.decisionLoads, loadID)
	if s.decisionErr != nil {
		return nil, s.decisionErr
	}
	var out []*model.FoundryReviewDecision
	for _, d := range s.decisions {
		if d.OntologyID == "ont-1" && d.ID > after {
			out = append(out, d)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (s *memStore) ListPendingFoundryDocuments(_ context.Context, loadID string) ([]*model.FoundryDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingFor = append(s.pendingFor, loadID)
	return s.pending, nil
}

func (s *memStore) SumFoundryGraphJobSpend(context.Context, string, time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spent, s.sumErr
}

func (s *memStore) FoundryDocumentSetSpend(_ context.Context, workspaceID string, setIDs []string, _ time.Time) (
	map[string]model.FoundryDocumentSetSpend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spendReads = append(s.spendReads, spendRead{workspaceID, slices.Clone(setIDs)})
	if s.spendHook != nil {
		if err := s.spendHook(len(s.spendReads)); err != nil {
			return nil, err
		}
	}
	out := map[string]model.FoundryDocumentSetSpend{}
	for _, id := range setIDs {
		if spend, ok := s.spends[id]; ok && workspaceID == "ws-1" {
			out[id] = *spend
		}
	}
	return out, nil
}

func (s *memStore) GetFoundryLoad(_ context.Context, ontologyID, id string) (*model.FoundryLoad, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadReads++
	load, ok := s.loads[id]
	if !ok || load.OntologyID != ontologyID {
		return nil, foundrystore.ErrNotFound
	}
	return load, nil
}

func (s *memStore) ListFoundryDocumentExtractions(_ context.Context, loadID string, ids []string) (map[string]*model.FoundryDocumentExtraction, error) {
	if s.priorErr != nil {
		return nil, s.priorErr
	}
	out := map[string]*model.FoundryDocumentExtraction{}
	for _, id := range ids {
		if e, ok := s.prior[loadID][id]; ok {
			out[id] = e
		}
	}
	return out, nil
}

func (s *memStore) UpdateFoundryGraphJob(_ context.Context, _, _ string, update model.FoundryGraphJobUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, update)
	return nil
}

func (s *memStore) RecordFoundryDocumentExtraction(_ context.Context, _, _ string, e *model.FoundryDocumentExtraction, keep bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordErr != nil {
		return s.recordErr
	}
	s.extractions[e.DocumentID], s.kept[e.DocumentID] = e, keep
	s.spent += e.USDMicros
	for _, doc := range s.pending {
		set, ok := s.spends[doc.SetID]
		if doc.ID != e.DocumentID || !ok {
			continue
		}
		set.USDMicros += e.USDMicros
		for _, other := range s.spends {
			if other.ConnectorKind == set.ConnectorKind {
				other.WorkspaceUSDMicros += e.USDMicros
			}
		}
	}
	return nil
}

func (s *memStore) ClaimFoundryGraphJob(context.Context, string, time.Duration) (*model.FoundryGraphJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.claims) == 0 {
		return nil, nil
	}
	job := s.claims[0]
	s.claims = s.claims[1:]
	return job, nil
}

func (s *memStore) RenewFoundryGraphJobLease(ctx context.Context, _, _ string, _ time.Duration) error {
	if s.renewHangs {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.renewErr
}

func (s *memStore) FinishFoundryGraphJob(ctx context.Context, _, _ string, outcome model.FoundryGraphJobOutcome) (model.FoundryGraphJobFinish, error) {
	if err := ctx.Err(); err != nil {
		return "", err // a finish on an ended context records nothing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = append(s.finished, outcome)
	return model.FoundryGraphJobEnded, nil
}

func (s *memStore) lastUpdate() model.FoundryGraphJobUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates[len(s.updates)-1]
}

// bucket is the Foundry data bucket: snapshots, documents and artifacts.
type bucket struct {
	mu      sync.Mutex
	objects map[string][]byte
	// refused, when set, is every Open's answer, as S3 answers a role it denies.
	refused error
	// opened are the keys read, in turn.
	opened []string
}

func (b *bucket) put(key string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = data
}

func (b *bucket) get(key string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objects[key]
	return data, ok
}

func (b *bucket) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if b.refused != nil {
		return nil, b.refused
	}
	b.mu.Lock()
	b.opened = append(b.opened, key)
	b.mu.Unlock()
	data, ok := b.get(key)
	if !ok {
		return nil, fmt.Errorf("get %s: %w", key, document.ErrMissing)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *bucket) Put(_ context.Context, key string, body io.ReadSeeker) error {
	data, err := io.ReadAll(body)
	b.put(key, data)
	return err
}

func (b *bucket) Delete(context.Context, string) error { return nil }

func (b *bucket) List(_ context.Context, prefix string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var keys []string
	for k := range b.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (b *bucket) OpenArtifact(_ context.Context, key string) ([]byte, error) {
	data, ok := b.get(key)
	if !ok {
		return nil, document.ErrMissing
	}
	return data, nil
}

func (b *bucket) PutArtifact(_ context.Context, key string, artifact []byte) error {
	b.put(key, artifact)
	return nil
}

// gateway is the tenant's gateway behind the proxy. It answers with the next
// of answers, then 200, and records each query in the store's events.
type gateway struct {
	mu      sync.Mutex
	tenants []string
	writer  []string
	answers []helixgateway.Response
	store   *memStore
}

func (g *gateway) Query(_ context.Context, _, tenantID string, _ []byte, extra http.Header) (helixgateway.Response, error) {
	g.store.record("write")
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tenants = append(g.tenants, tenantID)
	g.writer = append(g.writer, extra.Get("X-Helix-Require-Writer"))
	if len(g.answers) > 0 {
		answer := g.answers[0]
		g.answers = g.answers[1:]
		return answer, nil
	}
	return helixgateway.Response{StatusCode: 200, ContentType: "application/json", Body: []byte(`{}`)}, nil
}

type harness struct {
	task    *Task
	store   *memStore
	bucket  *bucket
	agent   *fakeAgent
	gateway *gateway
	workDir string
	ai      *ai.Resolver
}

// houseKey is Helix's Claude's key in the tests.
const houseKey = "sk-ant-api03-house-sentinel-0123456789"

// decodeExtract reads an Extract the agent got (readExtract).
func decodeExtract(t *testing.T, raw string) ontologyagent.ExtractRequest {
	t.Helper()
	req, err := readExtract(raw)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// readExtract reads an Extract request but its models, which the worker
// writes and never reads back.
func readExtract(raw string) (ontologyagent.ExtractRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return ontologyagent.ExtractRequest{}, err
	}
	delete(fields, "models")
	rest, _ := json.Marshal(fields)
	var req ontologyagent.ExtractRequest
	err := json.Unmarshal(rest, &req)
	return req, err
}

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write([]byte(s))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg := config.Defaults()
	cfg.Foundry.Sync.DataBucket = "foundry-data"
	cfg.Foundry.Graph.Agent.WorkDir = t.TempDir()
	cfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: houseKey, Models: config.AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
	}}
	gen := "gen-1"
	def, _ := json.Marshal(ontologyagent.Definition{
		Format: "agent_bundle", Status: ontologyagent.StatusDone, OntologyTOML: "version = \"draft\"\n", MappingsTOML: "ontology = \"draft\"\n",
		Draft: json.RawMessage(`{}`), Questions: json.RawMessage(`[]`),
	})
	store := &memStore{
		ontology: &model.FoundryOntology{ID: "ont-1", WorkspaceID: "ws-1", TenantID: "tenant-1", ProjectID: "proj-1"},
		tenant:   &ports.Database{TenantID: "tenant-1", WorkspaceID: "ws-1", ProjectID: "proj-1"},
		version: &model.OntologyVersion{
			ID: "ver-3", OntologyID: "ont-1", Version: 3, Format: model.OntologyFormatAgentBundle, GenerationID: &gen, Definition: def,
		},
		generation: &model.OntologyGeneration{ID: gen, OntologyID: "ont-1", Input: model.OntologyGenerationInput{
			Tables: []model.OntologyGenerationTable{{ConnectorID: "conn-1", Table: "customers"}},
		}},
		tables: []*model.ConnectorTable{{ConnectorID: "conn-1", TableName: "customers", S3Key: firstPin.S3Key,
			SyncID: firstPin.SyncID, SyncedAt: firstPin.SyncedAt, ChangeSeq: firstPin.ChangeSeq}},
		extractions: map[string]*model.FoundryDocumentExtraction{}, kept: map[string]bool{},
		enrich: enrichmentState{steps: map[string]*model.FoundryLoadEnrichment{}},
	}
	b := &bucket{objects: map[string][]byte{
		"ws/ws-1/connectors/conn-1/customers/s1.jsonl.gz": gz(t, `{"id":"cus_1","name":"Acme"}`+"\n"),
	}}
	for _, doc := range []struct{ id, ext, set string }{
		{"doc-a", "md", "set-contracts"}, {"doc-b", "md", "set-contracts"}, {"doc-c", "txt", "set-general"}, {"doc-d", "md", "set-general"},
	} {
		media := model.FoundryMediaMarkdown
		if doc.ext == "txt" {
			media = model.FoundryMediaPlainText
		}
		store.pending = append(store.pending, &model.FoundryDocument{
			ID: doc.id, WorkspaceID: "ws-1", SetID: doc.set, MediaType: media, S3Key: document.Key("ws-1", doc.id),
		})
		b.objects[document.Key("ws-1", doc.id)] = []byte("# " + doc.id + "\nAcme signed.")
	}
	// Every request the worker sends is one the vendored schemas take.
	schemas := map[string]*jsonschema.Resolved{}
	for _, name := range []string{"ingest_request.json", "extract_request.json", "delete_load_request.json"} {
		schemas[name] = contracttest.Schema(t, name)
	}
	agent := &fakeAgent{
		info:   &agentv1.GetInfoResponse{ContractVersion: doneInfo, Providers: []string{"anthropic"}},
		ingest: ingestDone, extract: extractAll, deleteLoad: deleteDone, schemas: schemas,
	}
	resolver, err := ai.NewResolver(&cfg, store, proPlans(true))
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{store: store}
	task := &Task{
		cfg: cfg.Foundry.Graph, queue: store, boosts: store, workerID: "w1", closeFn: func() error { return nil },
		now: func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }, refreshLag: &observed{},
	}
	task.runner = &runner{
		db: store, dbs: store, boosts: store, agent: startAgent(t, agent), ai: resolver, documents: b, cfg: &cfg, bucket: "foundry-data", workerID: "w1",
		now: time.Now, gateway: g, syncWait: time.Minute, syncPoll: time.Millisecond, maxRequestBytes: ontologyagent.MaxRequestBytes,
		snapshots: &ontologyagent.SnapshotCache{
			DB: store, Snapshots: b, WorkDir: cfg.Foundry.Graph.Agent.WorkDir, Dir: ontologyagent.GraphCache, MaxBytes: 1 << 20, Now: time.Now,
		},
		enrichment: &enrichment.Load{DB: store, Files: &enrichment.Files{Store: b}, Cfg: cfg.Foundry.Enrichment, Pro: proPlans(true), Now: time.Now},
	}
	return &harness{task: task, store: store, bucket: b, agent: agent, gateway: g, workDir: cfg.Foundry.Graph.Agent.WorkDir, ai: resolver}
}

// firstPin is the harness's synced table as a load pins it: its snapshot,
// and the sync that last observed it.
var firstPin = model.FoundryLoadSource{ConnectorID: "conn-1", Table: "customers", S3Key: "ws/ws-1/connectors/conn-1/customers/s1.jsonl.gz",
	SyncID: "01JDSYNC000000000000000001", SyncedAt: time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC), ChangeSeq: 7}

// firstObserved is what an Ingest of firstPin says of when its rows were observed.
const firstObserved = `{"sync":{"at":"2026-10-18T11:00:00Z","sync_id":"01JDSYNC000000000000000001","change_seq":7}}`

// loadJob is a load job: its load is its own id.
func loadJob() *model.FoundryGraphJob {
	return &model.FoundryGraphJob{
		ID: "job-1", WorkspaceID: "ws-1", OntologyID: "ont-1", VersionID: "ver-3", LoadID: "job-1", Kind: model.FoundryGraphJobLoad,
		Phase: model.FoundryGraphJobProvisioning, Attempts: 1,
	}
}

// extractJob is an extract into load-0, an earlier load job's.
func extractJob() *model.FoundryGraphJob {
	job := loadJob()
	job.ID, job.LoadID, job.Kind, job.Phase = "job-2", "load-0", model.FoundryGraphJobExtract, model.FoundryGraphJobDocuments
	return job
}

// deleteJob deletes load-0.
func deleteJob() *model.FoundryGraphJob {
	job := loadJob()
	job.ID, job.LoadID, job.Kind, job.Phase = "job-3", "load-0", model.FoundryGraphJobDelete, model.FoundryGraphJobDeleting
	return job
}

// run runs one attempt and returns the outcome it records.
func (h *harness) run(t *testing.T, job *model.FoundryGraphJob) model.FoundryGraphJobOutcome {
	t.Helper()
	h.task.process(context.Background(), job)
	if len(h.store.finished) == 0 {
		t.Fatal("the attempt recorded no outcome")
	}
	return h.store.finished[len(h.store.finished)-1]
}

func TestLoad(t *testing.T) {
	// A load ingests the generation's tables through the proxy, then extracts every pending document.
	t.Run("ingests the tables, then extracts the documents", func(t *testing.T) {
		h := newHarness(t)
		outcome := h.run(t, loadJob())
		if outcome.Status != model.FoundryGraphJobSucceeded || outcome.RetryAt != nil || !outcome.IngestDone || outcome.Error != "" {
			t.Fatalf("outcome = %+v", outcome)
		}
		if outcome.Progress != (model.FoundryGraphJobProgress{ChunksDone: 2, DocumentsDone: 4, DocumentsTotal: 4}) {
			t.Fatalf("progress = %+v", outcome.Progress)
		}
		ingests, extracts := h.agent.calls()
		if len(ingests) != 1 || len(extracts) != 1 {
			t.Fatalf("calls = %d ingests, %d extracts", len(ingests), len(extracts))
		}
		// The sources are checked below as sent: a Source is written, never read.
		var ingest struct {
			ontologyagent.IngestRequest
			Sources json.RawMessage `json:"sources"`
		}
		if err := json.Unmarshal([]byte(ingests[0]), &ingest); err != nil {
			t.Fatal(err)
		}
		if ingest.Database != "tenant-1" || ingest.Load != "job-1" || ingest.OntologyVersion != "foundry-v3" || ingest.RequestID != "job-1" ||
			ingest.ChunkRows != 1000 || ingest.MappingsTOML != "ontology = \"draft\"\n" || ingest.Helix.MaxRequestBytes != 2<<20 ||
			ingest.Helix.TimeoutSecs != 60 || !strings.HasPrefix(ingest.Helix.URL, "http://127.0.0.1:") || ingest.Helix.Token == "" {
			t.Fatalf("ingest request = %s", ingests[0])
		}
		var sources []map[string]any
		_ = json.Unmarshal([]byte(gjson(t, ingests[0], "sources")), &sources)
		if len(sources) != 1 || !strings.HasPrefix(sources[0]["id"].(string), "src_") {
			t.Fatalf("sources = %v", sources)
		}
		// The agent's write reached the ontology's tenant, as a writer, through the proxy.
		if len(h.gateway.tenants) != 1 || h.gateway.tenants[0] != "tenant-1" || h.gateway.writer[0] != "true" {
			t.Fatalf("gateway saw %v %v", h.gateway.tenants, h.gateway.writer)
		}

		extract := decodeExtract(t, extracts[0])
		if len(extract.Documents) != 4 || extract.Limits.MaxUSD != 20 || extract.Limits.DocumentsInFlight != 4 ||
			extract.Limits.MaxTextBytes != 1<<20 || extract.Database != "tenant-1" || extract.Load != "job-1" ||
			extract.MappingsTOML != "ontology = \"draft\"\n" {
			t.Fatalf("extract request = %s: the version's mappings scope the documents", extracts[0])
		}
		// Each document goes with its set, which the mappings' routes name.
		for i, want := range []string{"set-contracts", "set-contracts", "set-general", "set-general"} {
			if got := extract.Documents[i].Set; got != want {
				t.Fatalf("document %s is in set %q, want %q", extract.Documents[i].DocID, got, want)
			}
		}
		if len(h.store.pendingFor) != 1 || h.store.pendingFor[0] != "job-1" {
			t.Fatalf("documents pending for %v, want the load's", h.store.pendingFor)
		}
		c := extract.Documents[2]
		if c.DocID != "doc-c" || c.MediaType != ontologyagent.MediaPlainText || c.Path != "jobs/job-1/docs/doc-c.txt" ||
			c.URI != "s3://foundry-data/ws/ws-1/documents/doc-c/original" {
			t.Fatalf("document = %+v", c)
		}
		// The artifact is saved where the next run finds it.
		saved, ok := h.bucket.get("ws/ws-1/documents/doc-a/extractions/key_a.json.gz")
		if !ok || string(saved) != `{"sections":[1]}` {
			t.Fatalf("artifact = %q, %t", saved, ok)
		}
		a := h.store.extractions["doc-a"]
		if a.Status != model.FoundryExtractionExtracted || a.Counts.Mentions != 3 || a.Counts.Sections != 2 || a.USDMicros != 1500 ||
			a.InputTokens != 900 || a.ArtifactKey == nil || *a.ArtifactKey != "ws/ws-1/documents/doc-a/extractions/key_a.json.gz" {
			t.Fatalf("doc-a = %+v", a)
		}
		if b := h.store.extractions["doc-b"]; b.Status != model.FoundryExtractionDuplicate || *b.DuplicateOf != "doc-a" {
			t.Fatalf("doc-b = %+v", b)
		}
		if c := h.store.extractions["doc-c"]; c.Status != model.FoundryExtractionFailed || c.Error.Code != model.FoundryExtractionModel || !c.Error.Retryable {
			t.Fatalf("doc-c = %+v", c)
		}
		if !h.store.kept["doc-d"] || h.store.kept["doc-a"] {
			t.Fatalf("kept = %v: only the document the database already held keeps its row", h.store.kept)
		}
		var report map[string]json.RawMessage
		if err := json.Unmarshal(outcome.Report, &report); err != nil || report["ingest"] == nil || report["extract"] == nil {
			t.Fatalf("report = %s", outcome.Report)
		}
		if last := h.store.lastUpdate(); last.Phase != model.FoundryGraphJobFinishing {
			t.Fatalf("last phase = %s", last.Phase)
		}
		// The violation records the tables left open, as the ingest report
		// counts them, go on the load with the first update past Ingest and
		// with every one after it; none before Ingest ended.
		for i, u := range h.store.updates {
			past := u.Phase != model.FoundryGraphJobIngest
			if past != (u.IngestViolations != nil) || past && *u.IngestViolations != 2 {
				t.Fatalf("update %d at %s records %v violations, want 2 on every update past Ingest and none before", i, u.Phase, u.IngestViolations)
			}
		}
		if _, err := os.Stat(filepath.Join(h.workDir, "jobs", "job-1")); !os.IsNotExist(err) {
			t.Fatalf("the job's files outlived it: %v", err)
		}
	})

	// An extract goes straight to the documents of the load it targets;
	// every artifact saved for a document goes back as a file, since the
	// agent replays each model call it finds in any of them.
	t.Run("extracts into its load and replays saved artifacts", func(t *testing.T) {
		h := newHarness(t)
		h.bucket.put("ws/ws-1/documents/doc-a/extractions/key_a.json.gz", []byte(`{"saved":true}`))
		h.bucket.put("ws/ws-1/documents/doc-a/extractions/key_old.json.gz", []byte(`{"older":true}`))
		h.bucket.put("ws/ws-1/documents/doc-a/extractions/nested/x.json.gz", []byte(`{}`))
		var staged []byte
		h.agent.extract = func(req string, stream agentv1.Agent_ExtractServer) error {
			staged, _ = os.ReadFile(filepath.Join(h.workDir, "jobs/job-2/artifacts/doc-a/key_a.json"))
			return extractAll(req, stream)
		}
		outcome := h.run(t, extractJob())
		ingests, extracts := h.agent.calls()
		if outcome.Status != model.FoundryGraphJobSucceeded || len(ingests) != 0 || len(extracts) != 1 || !outcome.IngestDone {
			t.Fatalf("outcome = %+v after %d ingests", outcome, len(ingests))
		}
		extract := decodeExtract(t, extracts[0])
		if extract.Load != "load-0" || extract.RequestID != "job-2" || h.store.pendingFor[0] != "load-0" {
			t.Fatalf("extract request %s, documents pending for %v: want load-0's", extracts[0], h.store.pendingFor)
		}
		got := slices.Sorted(slices.Values(extract.Documents[0].Artifacts))
		if !slices.Equal(got, []string{"jobs/job-2/artifacts/doc-a/key_a.json", "jobs/job-2/artifacts/doc-a/key_old.json"}) {
			t.Fatalf("artifacts = %v", got)
		}
		if string(staged) != `{"saved":true}` {
			t.Fatalf("the agent found %q", staged)
		}
		if got := extract.Documents[1].Artifacts; got == nil || len(got) != 0 {
			t.Fatalf("a document without artifacts sends [] = %v", got)
		}
	})

	// A document no route of the version reads is recorded out of scope, so
	// the load stops waiting for it; it cost nothing.
	t.Run("records a document out of scope", func(t *testing.T) {
		h := newHarness(t)
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			events := []string{extracted[0]}
			for _, doc := range []string{"doc-a", "doc-b", "doc-c", "doc-d"} {
				events = append(events, documentEvent(doc, `{"kind":"out_of_scope"}`, noUsage))
			}
			return extractEvents(stream, append(events, `{"event":"extract_finished"}`), `{"status":"done","totals":`+totals+`}`)
		}
		outcome := h.run(t, extractJob())
		if outcome.Status != model.FoundryGraphJobSucceeded || outcome.Progress.DocumentsDone != 4 {
			t.Fatalf("outcome = %+v", outcome)
		}
		a := h.store.extractions["doc-a"]
		if a == nil || a.Status != model.FoundryExtractionOutOfScope || a.Error != nil || a.DuplicateOf != nil || a.USDMicros != 0 ||
			a.Counts.Sections != 2 || h.store.kept["doc-a"] {
			t.Fatalf("doc-a = %+v", a)
		}
	})

	// A retry skips the chunks an earlier attempt wrote, and its report
	// doesn't count their records: the load's count stays unknown. An attempt
	// past Ingest leaves the count the first one recorded.
	t.Run("records no violation count the ingest report did not make", func(t *testing.T) {
		for name, tc := range map[string]struct {
			phase  model.FoundryGraphJobPhase
			report string
		}{
			"a retried ingest": {model.FoundryGraphJobIngest, strings.Replace(ingestReport, `"skipped":0`, `"skipped":1`, 1)},
			"the tables in":    {model.FoundryGraphJobDocuments, ingestReport},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.agent.ingest = ingestReporting(tc.report)
				job := loadJob()
				job.Phase = tc.phase
				if outcome := h.run(t, job); outcome.Status != model.FoundryGraphJobSucceeded {
					t.Fatalf("outcome = %+v", outcome)
				}
				for i, u := range h.store.updates {
					if u.IngestViolations != nil {
						t.Fatalf("update %d at %s records %d violations", i, u.Phase, *u.IngestViolations)
					}
				}
			})
		}
	})

	// A fresh empty load, or one that already reached its end, owes no sweep.
	t.Run("calls no Extract without pending documents", func(t *testing.T) {
		for _, phase := range []model.FoundryGraphJobPhase{model.FoundryGraphJobProvisioning, model.FoundryGraphJobIngest, model.FoundryGraphJobFinishing} {
			t.Run(string(phase), func(t *testing.T) {
				h := newHarness(t)
				h.store.pending = nil
				job := loadJob()
				job.Phase = phase
				outcome := h.run(t, job)
				if _, extracts := h.agent.calls(); outcome.Status != model.FoundryGraphJobSucceeded || len(extracts) != 0 {
					t.Fatalf("outcome = %+v, %d extracts", outcome, len(extracts))
				}
			})
		}
	})

	// Document events can all land before the run's final sweep fails or is
	// interrupted. The next attempt must reach Extract's end even with none owed.
	t.Run("sweeps after an interrupted Extract with no documents pending", func(t *testing.T) {
		for _, tc := range []struct{ name, result string }{
			{"stream interrupted", ""},
			{"cancelled", `{"status":"cancelled","totals":` + totals + `}`},
			{"store failed", `{"status":"store_failed","error":"sweep failed"}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				result := tc.result
				h := newHarness(t)
				h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
					if err := extractEvents(stream, extracted[:len(extracted)-1], result); err != nil {
						return err
					}
					if result == "" {
						return status.Error(codes.Unavailable, "stream interrupted")
					}
					return nil
				}
				job := loadJob()
				outcome := h.run(t, job)
				if outcome.Status == model.FoundryGraphJobSucceeded || len(h.store.extractions) != 4 {
					t.Fatalf("outcome = %+v, extractions = %d", outcome, len(h.store.extractions))
				}
				job.Phase = h.store.updates[len(h.store.updates)-1].Phase
				job.Attempts++
				h.store.pending = nil
				// A sweep makes no model calls, even when no budget remains.
				job.Spend.USDMicros = int64(h.task.cfg.MaxJobUSD * 1e6)
				h.store.spent = int64(h.task.cfg.MonthlyUSD * 1e6)
				h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
					return extractEvents(stream, nil, `{"status":"done","totals":`+totals+`}`)
				}
				outcome = h.run(t, job)
				ingests, extracts := h.agent.calls()
				if outcome.Status != model.FoundryGraphJobSucceeded || len(ingests) != 1 || len(extracts) != 2 {
					t.Fatalf("outcome = %+v, %d ingests, %d extracts", outcome, len(ingests), len(extracts))
				}
				request := decodeExtract(t, extracts[1])
				if gjson(t, extracts[1], "documents") != "[]" || request.MappingsTOML != "ontology = \"draft\"\n" ||
					request.RequestID != job.ID || request.Load != job.LoadID || request.OntologyVersion != "foundry-v3" || request.Limits.MaxUSD != 0 {
					t.Fatalf("sweep request = %s", extracts[1])
				}
			})
		}
	})

	// An attempt can commit some documents and stop before the run's end,
	// which is where the engine sweeps them, with others still pending. Once
	// the budget is spent the pending documents still go, with nothing to
	// spend, so the run reaches its end: what replays commits, and what needs
	// a model call waits.
	t.Run("sweeps with documents pending once the budget is spent", func(t *testing.T) {
		monthSpent := func(h *harness, _ *model.FoundryGraphJob) { h.store.spent = int64(h.task.cfg.MonthlyUSD * 1e6) }
		capSpent := func(h *harness, job *model.FoundryGraphJob) { job.Spend.USDMicros = int64(h.task.cfg.MaxJobUSD * 1e6) }
		overBudget := `{"status":"budget_exhausted","totals":` + totals + `}`
		for name, tc := range map[string]struct {
			spend   func(*harness, *model.FoundryGraphJob)
			events  []string
			result  string
			status  model.FoundryGraphJobStatus
			message string
		}{
			"the month spent": {monthSpent, nil, overBudget, model.FoundryGraphJobBudgetExhausted, "this month's $100.00"},
			"the cap spent":   {capSpent, nil, overBudget, model.FoundryGraphJobBudgetExhausted, "the load spent its $20.00 cap"},
			"every one replays": {monthSpent, []string{documentEvent("doc-d", `{"kind":"ingested"}`, noUsage)},
				`{"status":"done","totals":` + totals + `}`, model.FoundryGraphJobSucceeded, ""},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
					if err := extractEvents(stream, extracted[:len(extracted)-2], ""); err != nil {
						return err
					}
					return status.Error(codes.Unavailable, "stream interrupted")
				}
				job := loadJob()
				if outcome := h.run(t, job); outcome.RetryAt == nil || len(h.store.extractions) != 3 {
					t.Fatalf("outcome = %+v, extractions = %d", outcome, len(h.store.extractions))
				}
				job.Phase = h.store.lastUpdate().Phase
				job.Attempts++
				h.store.pending = h.store.pending[3:]
				tc.spend(h, job)
				h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
					return extractEvents(stream, tc.events, tc.result)
				}
				outcome := h.run(t, job)
				_, extracts := h.agent.calls()
				if outcome.Status != tc.status || !strings.Contains(outcome.Error, tc.message) || len(extracts) != 2 {
					t.Fatalf("outcome = %+v after %d extracts", outcome, len(extracts))
				}
				request := decodeExtract(t, extracts[1])
				if len(request.Documents) != 1 || request.Documents[0].DocID != "doc-d" || request.Limits.MaxUSD != 0 {
					t.Fatalf("the retry sends %s: the pending document, with nothing to spend", extracts[1])
				}
			})
		}
	})

	// A refresh runs whatever the month has left: with nothing, its
	// documents go with nothing to spend, those whose saved artifacts replay
	// commit, and the load is done when every one replayed. Otherwise it
	// ends budget_exhausted, its other documents waiting.
	t.Run("sends a refresh's documents with nothing to spend once the month is spent", func(t *testing.T) {
		replayed := func(docs ...string) []string {
			var events []string
			for _, doc := range docs {
				events = append(events, documentEvent(doc, `{"kind":"ingested"}`, noUsage))
			}
			return events
		}
		for name, tc := range map[string]struct {
			events  []string
			result  string
			status  model.FoundryGraphJobStatus
			message string
		}{
			"every one replays": {replayed("doc-a", "doc-b", "doc-c", "doc-d"), `{"status":"done","totals":` + totals + `}`,
				model.FoundryGraphJobSucceeded, ""},
			"one has no saved extraction": {replayed("doc-a", "doc-b", "doc-c"), `{"status":"budget_exhausted","totals":` + totals + `}`,
				model.FoundryGraphJobBudgetExhausted, "work no one started"},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.store.spent = int64(h.task.cfg.MonthlyUSD * 1e6)
				h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
					return extractEvents(stream, tc.events, tc.result)
				}
				job := loadJob()
				job.Origin = model.FoundryLoadByRefresh
				outcome := h.run(t, job)
				ingests, extracts := h.agent.calls()
				if outcome.Status != tc.status || !strings.Contains(outcome.Error, tc.message) || len(ingests) != 1 || len(extracts) != 1 {
					t.Fatalf("outcome = %+v after %d ingests, %d extracts", outcome, len(ingests), len(extracts))
				}
				request := decodeExtract(t, extracts[0])
				if len(request.Documents) != 4 || request.Limits.MaxUSD != 0 || len(h.store.extractions) != len(tc.events) {
					t.Fatalf("sent %d documents with max_usd %v; %d recorded", len(request.Documents), request.Limits.MaxUSD, len(h.store.extractions))
				}
			})
		}
	})

	// gRPC would refuse a request past the agent's message limit as it
	// refuses a full agent, which is retried; no retry sends less.
	t.Run("fails for good a request past the agent's message limit", func(t *testing.T) {
		h := newHarness(t)
		h.task.runner.maxRequestBytes = 1000
		outcome := h.run(t, loadJob())
		if _, extracts := h.agent.calls(); outcome.Status != model.FoundryGraphJobFailed || outcome.RetryAt != nil ||
			outcome.Error != "this version reads more documents than one load can send" || len(extracts) != 0 || !outcome.IngestDone {
			t.Fatalf("outcome = %+v after %d extracts", outcome, len(extracts))
		}
	})

	t.Run("retries a failed empty sweep", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending = nil
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, nil, `{"status":"store_failed","error":"sweep failed"}`)
		}
		job := loadJob()
		job.Phase = model.FoundryGraphJobDocuments
		outcome := h.run(t, job)
		if _, extracts := h.agent.calls(); outcome.RetryAt == nil || len(extracts) != 1 ||
			h.store.updates[len(h.store.updates)-1].Phase != model.FoundryGraphJobDocuments {
			t.Fatalf("outcome = %+v, %d extracts", outcome, len(extracts))
		}
	})

	// A document whose original is gone is left out and marked deleted in
	// its workspace, so no later finish owes it.
	t.Run("leaves out and deletes a document whose original is gone", func(t *testing.T) {
		h := newHarness(t)
		delete(h.bucket.objects, document.Key("ws-1", "doc-b"))
		h.run(t, loadJob())
		_, extracts := h.agent.calls()
		extract := decodeExtract(t, extracts[0])
		if len(extract.Documents) != 3 || len(h.store.deleted) != 1 || h.store.deleted[0] != "ws-1/doc-b" {
			t.Fatalf("sent %d documents, deleted %v", len(extract.Documents), h.store.deleted)
		}
	})

	// MDX goes to the agent as Markdown with its code blanked to spaces of
	// the same length, so the sections and marks it returns fall on the
	// original's bytes.
	t.Run("sends an MDX document as Markdown with its code blanked", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending[1].MediaType = model.FoundryMediaMDX
		original := "import Chart from './chart'\n\n# Terms\nAcme pays {total}.\n"
		h.bucket.objects[document.Key("ws-1", "doc-b")] = []byte(original)
		var staged []byte
		h.agent.extract = func(req string, stream agentv1.Agent_ExtractServer) error {
			staged, _ = os.ReadFile(filepath.Join(h.workDir, "jobs/job-1/docs/doc-b.md"))
			return extractAll(req, stream)
		}
		h.run(t, loadJob())
		_, extracts := h.agent.calls()
		extract := decodeExtract(t, extracts[0])
		if b := extract.Documents[1]; b.DocID != "doc-b" || b.MediaType != ontologyagent.MediaMarkdown || b.Path != "jobs/job-1/docs/doc-b.md" {
			t.Fatalf("document = %+v", b)
		}
		if want := "                           \n\n# Terms\nAcme pays        .\n"; string(staged) != want || len(staged) != len(original) {
			t.Fatalf("staged %q, want %q", staged, want)
		}
	})

	// A PDF goes to the agent as the text its read stored, as plain text:
	// its pages split by form feeds, never its original's bytes. The URI
	// still names the original.
	t.Run("sends a read PDF's text as plain text", func(t *testing.T) {
		h := newHarness(t)
		text := "Invoice 1042\n\fTerms\n"
		key := document.TextKey("ws-1", "doc-b", model.FoundryFileReadStandard, strings.Repeat("a", 64))
		h.store.pending[1].MediaType = model.FoundryMediaPDF
		h.store.pending[1].Read = &model.FoundryFileRead{Status: model.FoundryFileReadSucceeded, ArtifactKey: &key}
		h.bucket.objects[document.Key("ws-1", "doc-b")] = []byte("%PDF-1.7 the original")
		h.bucket.objects[key] = []byte(text)
		var staged []byte
		h.agent.extract = func(req string, stream agentv1.Agent_ExtractServer) error {
			staged, _ = os.ReadFile(filepath.Join(h.workDir, "jobs/job-1/docs/doc-b.txt"))
			return extractAll(req, stream)
		}
		if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		_, extracts := h.agent.calls()
		extract := decodeExtract(t, extracts[0])
		b := extract.Documents[1]
		if b.DocID != "doc-b" || b.MediaType != ontologyagent.MediaPlainText || b.Path != "jobs/job-1/docs/doc-b.txt" ||
			b.URI != "s3://foundry-data/ws/ws-1/documents/doc-b/original" || string(staged) != text {
			t.Fatalf("document = %+v, staged %q", b, staged)
		}
	})

	// A PDF whose read failed is never sent: the load records it failed
	// with the read's code, for good, and the document's read says why.
	t.Run("records a PDF whose read failed with the read's code", func(t *testing.T) {
		for _, code := range []model.FoundryFileReadErrorCode{model.FoundryFileReadUnsupported, model.FoundryFileReadTooLarge} {
			h := newHarness(t)
			h.store.pending[1].MediaType = model.FoundryMediaPDF
			h.store.pending[1].Read = &model.FoundryFileRead{Status: model.FoundryFileReadFailed,
				Error: &model.FoundryFileReadError{Code: code, Message: "Scanned PDF: no text to read"}}
			if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded {
				t.Fatalf("%s: outcome = %+v", code, outcome)
			}
			_, extracts := h.agent.calls()
			extract := decodeExtract(t, extracts[0])
			got := h.store.extractions["doc-b"]
			if len(extract.Documents) != 3 || got == nil || got.Status != model.FoundryExtractionFailed ||
				*got.Error != (model.FoundryExtractionError{Code: model.FoundryExtractionErrorCode(code), Retryable: false}) {
				t.Fatalf("%s: sent %d documents, recorded %+v", code, len(extract.Documents), got)
			}
		}
	})

	// A read PDF whose text is gone is treated as a document whose original
	// is: its prefix went with a delete, so it is left out and deleted.
	t.Run("leaves out and deletes a read PDF whose text is gone", func(t *testing.T) {
		h := newHarness(t)
		key := document.TextKey("ws-1", "doc-b", model.FoundryFileReadStandard, strings.Repeat("a", 64))
		h.store.pending[1].MediaType = model.FoundryMediaPDF
		h.store.pending[1].Read = &model.FoundryFileRead{Status: model.FoundryFileReadSucceeded, ArtifactKey: &key}
		h.run(t, loadJob())
		if len(h.store.deleted) != 1 || h.store.deleted[0] != "ws-1/doc-b" {
			t.Fatalf("deleted %v", h.store.deleted)
		}
	})

	// Only documents with text reach the agent: table files, and a PDF with
	// no finished read, end failed as unsupported, for good.
	t.Run("records a document of a type ontologies don't read as unsupported", func(t *testing.T) {
		for _, media := range []model.FoundryMediaType{
			model.FoundryMediaJSON, model.FoundryMediaCSV, model.FoundryMediaTSV, model.FoundryMediaJSONL, model.FoundryMediaParquet, model.FoundryMediaPDF,
		} {
			h := newHarness(t)
			h.store.pending[1].MediaType = media
			if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded {
				t.Fatalf("%s: outcome = %+v", media, outcome)
			}
			_, extracts := h.agent.calls()
			extract := decodeExtract(t, extracts[0])
			for _, d := range extract.Documents {
				if d.DocID == "doc-b" {
					t.Fatalf("%s: sent to the agent as %s", media, d.MediaType)
				}
			}
			got := h.store.extractions["doc-b"]
			if len(extract.Documents) != 3 || got == nil || got.Status != model.FoundryExtractionFailed || h.store.kept["doc-b"] ||
				*got.Error != (model.FoundryExtractionError{Code: model.FoundryExtractionUnsupported, Retryable: false}) {
				t.Fatalf("%s: sent %d documents, recorded %+v", media, len(extract.Documents), got)
			}
		}
	})

	// Only unsupported documents pending, and no sweep owed: nothing reaches
	// the agent.
	t.Run("calls no Extract when every pending document is unsupported", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending = h.store.pending[:1]
		h.store.pending[0].MediaType = model.FoundryMediaPDF
		outcome := h.run(t, loadJob())
		if _, extracts := h.agent.calls(); outcome.Status != model.FoundryGraphJobSucceeded || len(extracts) != 0 ||
			h.store.extractions["doc-a"].Error.Code != model.FoundryExtractionUnsupported {
			t.Fatalf("outcome = %+v after %d extracts, recorded %+v", outcome, len(extracts), h.store.extractions["doc-a"])
		}
	})

	// Recording an unsupported document after the lease is lost writes
	// nothing and sends nothing.
	t.Run("retries when an unsupported document can't be recorded", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending[0].MediaType = model.FoundryMediaPDF
		h.store.recordErr = errors.New("db down")
		outcome := h.run(t, extractJob())
		if _, extracts := h.agent.calls(); outcome.RetryAt == nil || len(extracts) != 0 {
			t.Fatalf("outcome = %+v after %d extracts", outcome, len(extracts))
		}
	})

	// A read S3 refuses (as it refuses a missing key to a role that can't
	// list) is retried: taking it for a missing original would delete every
	// document of the ontology.
	t.Run("retries a refused read and deletes nothing", func(t *testing.T) {
		h := newHarness(t)
		h.bucket.refused = errors.New("api error AccessDenied: Access Denied")
		outcome := h.run(t, extractJob())
		if _, extracts := h.agent.calls(); outcome.RetryAt == nil || len(extracts) != 0 || len(h.store.deleted) != 0 {
			t.Fatalf("outcome = %+v after %d extracts, deleted %v", outcome, len(extracts), h.store.deleted)
		}
	})
}

// gjson returns one top-level field of a JSON object as raw JSON.
func gjson(t *testing.T, raw, field string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return string(m[field])
}

func TestLoadOutcomes(t *testing.T) {
	ingestResult := func(result string) func(string, agentv1.Agent_IngestServer) error {
		return func(_ string, stream agentv1.Agent_IngestServer) error { return ingestEvents(stream, nil, result) }
	}
	extractResult := func(result string) func(string, agentv1.Agent_ExtractServer) error {
		return func(_ string, stream agentv1.Agent_ExtractServer) error { return extractEvents(stream, nil, result) }
	}
	refuseIngest := func(code codes.Code) func(string, agentv1.Agent_IngestServer) error {
		return func(string, agentv1.Agent_IngestServer) error { return status.Error(code, "refused") }
	}
	type want int
	const (
		retried want = iota
		failed
		budget
	)
	for name, tc := range map[string]struct {
		setup func(*harness)
		want  want
		says  string
	}{
		"the synced data changed mid-load": {func(h *harness) {
			h.agent.ingest = ingestResult(`{"status":"sources_changed","streams":[]}`)
		}, failed, "start a new load"},
		"a load of another version": {func(h *harness) {
			h.agent.ingest = ingestResult(`{"status":"load_mismatch","stored_version":"foundry-v2"}`)
		}, failed, "foundry-v2"},
		"a load being deleted": {func(h *harness) { h.agent.ingest = ingestResult(`{"status":"load_unknown"}`) }, failed, "being deleted"},
		"an extract into a load of another version": {func(h *harness) {
			h.agent.extract = extractResult(`{"status":"load_mismatch","stored_version":"foundry-v2"}`)
		}, failed, "foundry-v2"},
		"an extract into a load the database lacks": {func(h *harness) {
			h.agent.extract = extractResult(`{"status":"load_unknown"}`)
		}, failed, "does not hold this load"},
		"mappings that don't fit": {func(h *harness) {
			h.agent.ingest = ingestResult(`{"status":"invalid_mappings","errors":["customers: no column email"]}`)
		}, failed, "customers: no column email"},
		"a source it can't read": {func(h *harness) { h.agent.ingest = ingestResult(`{"status":"source_unavailable","source":"src_1"}`) }, retried, "synced table"},
		"a write in doubt": {func(h *harness) {
			h.agent.ingest = ingestResult(`{"status":"outcome_unknown","mapping":"m","pass":"create","chunk":0,"error":"t"}`)
		}, retried, "settles"},
		"a cancelled load":         {func(h *harness) { h.agent.ingest = ingestResult(`{"status":"cancelled","report":{}}`) }, retried, "stopped"},
		"a store that refused":     {func(h *harness) { h.agent.ingest = ingestResult(`{"status":"store_failed","error":"no"}`) }, retried, "refused"},
		"an unreadable result":     {func(h *harness) { h.agent.ingest = ingestResult(`{"status":"exploded"}`) }, failed, "can't read"},
		"a busy database":          {func(h *harness) { h.agent.ingest = refuseIngest(codes.Aborted) }, retried, "another job"},
		"a refused request":        {func(h *harness) { h.agent.ingest = refuseIngest(codes.InvalidArgument) }, failed, "refused"},
		"an agent that can't load": {func(h *harness) { h.agent.ingest = refuseIngest(codes.Unimplemented) }, failed, ontologyagent.ContractVersion},
		"a full agent":             {func(h *harness) { h.agent.ingest = refuseIngest(codes.ResourceExhausted) }, retried, "unavailable"},
		"models the agent can't run": {func(h *harness) {
			h.agent.extract = func(string, agentv1.Agent_ExtractServer) error {
				return status.Error(codes.FailedPrecondition, "this server extracts no documents")
			}
		}, failed, "can't run these models"},
		"a stream without a result": {func(h *harness) {
			h.agent.ingest = func(_ string, stream agentv1.Agent_IngestServer) error { return ingestEvents(stream, nil, "") }
		}, retried, "unavailable"},
		"another contract": {func(h *harness) {
			h.agent.info = &agentv1.GetInfoResponse{ContractVersion: "1.0.0"}
		}, failed, "contract 1.0.0"},
		"extraction out of budget": {func(h *harness) {
			h.agent.extract = extractResult(`{"status":"budget_exhausted","totals":` + totals + `}`)
		}, budget, "budget"},
		"a document write in doubt": {func(h *harness) {
			h.agent.extract = extractResult(`{"status":"outcome_unknown","doc_id":"doc-a","error":"t"}`)
		}, retried, "settles"},
		"a cancelled extraction": {func(h *harness) {
			h.agent.extract = extractResult(`{"status":"cancelled","totals":` + totals + `}`)
		}, retried, "stopped"},
		"a deleted ontology": {func(h *harness) {
			now := time.Now()
			h.store.ontology.DeletedAt = &now
		}, failed, "deleted"},
		"a paused database": {func(h *harness) {
			h.store.gate = "the database's queries are paused"
		}, retried, "paused"},
		"a cluster mid-change": {func(h *harness) {
			h.store.gate = "its cluster is not active"
		}, retried, "not active"},
		"a database moved to another workspace": {func(h *harness) {
			h.store.tenant.WorkspaceID = "ws-2"
		}, failed, "not in its workspace"},
		"documents the agent left unfinished": {func(h *harness) {
			h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
				return extractEvents(stream, []string{documentEvent("doc-a", `{"kind":"ingested"}`, usage)}, `{"status":"done","totals":`+totals+`}`)
			}
		}, retried, "unfinished"},
		"a version that is gone": {func(h *harness) { h.store.version = nil }, failed, "gone"},
		// With no budget left the documents still go, with nothing to spend; what needs a model call waits.
		"a job past its cap": {func(h *harness) {
			h.task.runner.cfg.Foundry.Graph.MaxJobUSD = 0
			h.agent.extract = extractResult(`{"status":"budget_exhausted","totals":` + totals + `}`)
		}, budget, "cap"},
		"a month spent": {func(h *harness) {
			h.store.spent = 100_000_000
			h.agent.extract = extractResult(`{"status":"budget_exhausted","totals":` + totals + `}`)
		}, budget, "this month"},
		"the month's share of work no one started spent": {func(h *harness) {
			h.store.spent = 80_000_000
			h.agent.extract = extractResult(`{"status":"budget_exhausted","totals":` + totals + `}`)
		}, budget, "the $80.00 of this month's $100.00 that work no one started may use"},
		"a lost lease": {func(h *harness) {
			h.store.recordErr = foundrystore.ErrNotFound
		}, retried, "lease"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)
			outcome := h.run(t, loadJob())
			switch tc.want {
			case retried:
				if outcome.RetryAt == nil {
					t.Fatalf("outcome = %+v, want a retry", outcome)
				}
			case failed:
				if outcome.RetryAt != nil || outcome.Status != model.FoundryGraphJobFailed {
					t.Fatalf("outcome = %+v, want failed", outcome)
				}
			case budget:
				if outcome.RetryAt != nil || outcome.Status != model.FoundryGraphJobBudgetExhausted {
					t.Fatalf("outcome = %+v, want budget_exhausted", outcome)
				}
			}
			if !strings.Contains(outcome.Error, tc.says) {
				t.Fatalf("error %q does not say %q", outcome.Error, tc.says)
			}
		})
	}

	// The cap sent is what is left of the job's and the month's budget,
	// whichever is less. Work no person started has the month less a job's
	// cap ($80 of the default $100), which a person's load may spend.
	t.Run("sends what is left of the budget", func(t *testing.T) {
		for name, tc := range map[string]struct {
			requester *string
			spent     int64
			maxUSD    float64
		}{
			"a person's load":     {new("user-1"), 95_500_000, 4.5},
			"work no one started": {nil, 75_500_000, 4.5},
			"a person's load past what work no one started may use": {new("user-1"), 80_000_000, 18},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.store.spent = tc.spent
				job := loadJob()
				job.RequestedByUserID = tc.requester
				job.Spend.USDMicros = 2_000_000
				h.run(t, job)
				_, extracts := h.agent.calls()
				extract := decodeExtract(t, extracts[0])
				if extract.Limits.MaxUSD != tc.maxUSD {
					t.Fatalf("max_usd = %v, want %v", extract.Limits.MaxUSD, tc.maxUSD)
				}
			})
		}
	})

	// A source the agent could not read is downloaded afresh next time.
	t.Run("forgets the cached snapshot a source failed on", func(t *testing.T) {
		h := newHarness(t)
		h.agent.ingest = ingestResult(`{"status":"source_unavailable","source":"src_1"}`)
		h.run(t, loadJob())
		entries, err := os.ReadDir(filepath.Join(h.workDir, ontologyagent.GraphCache))
		if err != nil || len(entries) != 0 {
			t.Fatalf("cache holds %d files, err %v; want it there and empty", len(entries), err)
		}
	})

	// Retries back off from the configured wait, doubling, and stop at the cap.
	t.Run("backs off and gives up", func(t *testing.T) {
		h := newHarness(t)
		h.agent.ingest = refuseIngest(codes.Unavailable)
		job := loadJob()
		job.Attempts = 2
		outcome := h.run(t, job)
		if want := h.task.now().Add(2 * h.task.cfg.RetryBackoff); outcome.RetryAt == nil || !outcome.RetryAt.Equal(want) {
			t.Fatalf("retry at %v, want %v", outcome.RetryAt, want)
		}
		job.Attempts = h.task.cfg.MaxAttempts
		outcome = h.run(t, job)
		if outcome.Status != model.FoundryGraphJobFailed || !strings.Contains(outcome.Error, "gave up after 5 attempts") {
			t.Fatalf("outcome = %+v", outcome)
		}
	})

	// An ended job may queue what follows it: the worker names the jobs,
	// bounds an extract by this month's budget and says how many loads stay.
	t.Run("hands an ended job's next jobs their ids, the month's budget and the loads kept", func(t *testing.T) {
		h := newHarness(t)
		h.agent.ingest = ingestResult(`{"status":"sources_changed","streams":[]}`)
		next := h.run(t, loadJob()).Next
		if next.ExtractID == "" || next.DeleteID == "" || next.ExtractID == next.DeleteID || next.RetainLoads != 5 ||
			next.Budget != (model.FoundryGraphBudget{Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), CapMicros: 100_000_000, ReserveMicros: 20_000_000}) {
			t.Fatalf("next = %+v", next)
		}
	})

	// An index still building over the database's data is no fault of the
	// job's: it waits the backoff's floor and keeps its attempts, even its last.
	t.Run("retries an index still building without counting the attempt", func(t *testing.T) {
		for name, setup := range map[string]func(*harness){
			"ingest":  func(h *harness) { h.agent.ingest = ingestResult(`{"status":"indexes_pending","label":"Customer"}`) },
			"extract": func(h *harness) { h.agent.extract = extractResult(`{"status":"indexes_pending","label":"Customer"}`) },
		} {
			h := newHarness(t)
			setup(h)
			job := loadJob()
			job.Attempts = h.task.cfg.MaxAttempts
			outcome := h.run(t, job)
			if want := h.task.now().Add(h.task.cfg.RetryBackoff); outcome.RetryAt == nil || !outcome.RetryAt.Equal(want) || !outcome.Uncounted ||
				!strings.Contains(outcome.Error, "indexing") {
				t.Fatalf("%s: outcome = %+v, want an uncounted retry at %v", name, outcome, want)
			}
		}
	})

	// A retry after the tables are in skips them; the job records them as loaded.
	t.Run("keeps the tables loaded across a retry", func(t *testing.T) {
		h := newHarness(t)
		h.agent.extract = extractResult(`{"status":"store_failed","error":"no"}`)
		outcome := h.run(t, loadJob())
		if outcome.RetryAt == nil || !outcome.IngestDone {
			t.Fatalf("outcome = %+v", outcome)
		}
	})
}

// addSource gives the harness an API source named name, whose own set is
// set, with what work no one started spent on that set this month, and n of
// its pieces pending in the job's load after what is pending, newest first:
// <set>-p0, <set>-p1, and so on. Every API source's set carries the
// workspace's spend on them all; one with no pieces stands for a source
// deleted this month, whose spend still counts.
func (h *harness) addSource(set, name string, spent int64, n int) {
	if h.store.spends == nil {
		h.store.spends = map[string]*model.FoundryDocumentSetSpend{}
	}
	h.store.spends[set] = &model.FoundryDocumentSetSpend{SetID: set, ConnectorKind: model.ConnectorKindEvents, ConnectorName: name, USDMicros: spent}
	var workspace int64
	for _, s := range h.store.spends {
		if s.ConnectorKind == model.ConnectorKindEvents {
			workspace += s.USDMicros
		}
	}
	for _, s := range h.store.spends {
		if s.ConnectorKind == model.ConnectorKindEvents {
			s.WorkspaceUSDMicros = workspace
		}
	}
	for i := range n {
		id := fmt.Sprintf("%s-p%d", set, i)
		h.store.pending = append(h.store.pending, &model.FoundryDocument{
			ID: id, WorkspaceID: "ws-1", SetID: set, Name: "Piece " + id, MediaType: model.FoundryMediaMarkdown, S3Key: document.Key("ws-1", id),
			Source: &model.FoundryDocumentSource{
				ExternalID: fmt.Sprintf("pc_%d", i), Version: "sha", At: time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Minute),
			},
		})
		h.bucket.put(document.Key("ws-1", id), []byte("# Piece "+id+"\nAcme renewed."))
	}
}

// pieces are the ids of set's pieces from up to but not including to.
func pieces(set string, from, to int) []string {
	var ids []string
	for i := from; i < to; i++ {
		ids = append(ids, fmt.Sprintf("%s-p%d", set, i))
	}
	return ids
}

// extractRequests are the Extract requests the agent got, in turn.
func (h *harness) extractRequests(t *testing.T) []ontologyagent.ExtractRequest {
	t.Helper()
	_, raw := h.agent.calls()
	out := make([]ontologyagent.ExtractRequest, len(raw))
	for i, r := range raw {
		out[i] = decodeExtract(t, r)
	}
	return out
}

// docIDs are a request's documents' ids, in order.
func docIDs(r ontologyagent.ExtractRequest) []string {
	ids := []string{}
	for _, d := range r.Documents {
		ids = append(ids, d.DocID)
	}
	return ids
}

// extractCosting is the engine under its budget: each document without a
// saved artifact costs perDoc, and the rest replay at $0. It checks
// limits.max_usd before each group of limits.documents_in_flight it starts,
// as it checks before every model call with that many documents in flight,
// and ends budget_exhausted at the first group with a model call that what
// it spent leaves no room for: so it overshoots by at most a group.
func extractCosting(perDoc int64) func(string, agentv1.Agent_ExtractServer) error {
	paid := strings.ReplaceAll(usage, `"usd_micros":1500`, fmt.Sprintf(`"usd_micros":%d`, perDoc))
	return func(req string, stream agentv1.Agent_ExtractServer) error {
		request, err := readExtract(req)
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		events, spent := []string{extracted[0]}, int64(0)
		for docs := range slices.Chunk(request.Documents, int(request.Limits.DocumentsInFlight)) {
			calls := slices.ContainsFunc(docs, func(d ontologyagent.ExtractDocument) bool { return len(d.Artifacts) == 0 })
			if calls && float64(spent) >= request.Limits.MaxUSD*1e6 {
				return extractEvents(stream, events, `{"status":"budget_exhausted","totals":`+totals+`}`)
			}
			for _, d := range docs {
				if len(d.Artifacts) > 0 {
					events = append(events, documentEvent(d.DocID, `{"kind":"ingested"}`, noUsage))
					continue
				}
				events = append(events, documentEvent(d.DocID, `{"kind":"ingested"}`, paid))
				spent += perDoc
			}
		}
		return extractEvents(stream, append(events, `{"event":"extract_finished"}`), `{"status":"done","totals":`+totals+`}`)
	}
}

// heldDocuments reads helix_foundry_graph_documents_held_total{reason="set_allowance"}.
func heldDocuments() float64 {
	return testutil.ToFloat64(documentsHeld.WithLabelValues("set_allowance"))
}

// The defaults give an API source $5 a month of extraction no one started,
// and its workspace's API sources $20 together (config.Foundry.SetAllowances).
func TestSetAllowance(t *testing.T) {
	const (
		sourceSpent = `the API source "Support" spent its $5.00 of this month's extraction by work no one started; ` +
			`its newest pieces wait for next month or a load a person starts`
		workspaceSpent = `the workspace's API sources spent their $20.00 of this month's extraction by work no one started; ` +
			`the newest pieces of "Support" wait for next month or a load a person starts`
	)
	refresh := func(h *harness, refreshes *string) *model.FoundryGraphJob {
		job := loadJob()
		job.Origin = model.FoundryLoadByRefresh
		h.store.loads = map[string]*model.FoundryLoad{job.LoadID: {
			ID: job.LoadID, OntologyID: "ont-1", Origin: model.FoundryLoadByRefresh, RefreshesLoadID: refreshes,
		}}
		return job
	}
	recorded := func(status model.FoundryExtractionStatus) *model.FoundryDocumentExtraction {
		return &model.FoundryDocumentExtraction{Status: status}
	}

	// Each source's pieces go in an Extract of their own, newest first, at
	// most the least of what the job's and the month's budget leave once
	// the attempt's recorded spend is counted and what the source has left;
	// a job of pieces alone sends nothing else, not even a sweep.
	t.Run("extracts each API source in an Extract of its own within what is left", func(t *testing.T) {
		for name, tc := range map[string]struct {
			monthSpent, jobSpent int64
			want                 []float64
		}{
			"each source's allowance the least": {0, 0, []float64{4, 0.5}},
			"the month's the least":             {79_750_000, 0, []float64{0.25, 0.247}},
			"the job's the least":               {0, 19_900_000, []float64{0.1, 0.097}},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.store.pending = nil
				h.addSource("set-a", "Support", 1_000_000, 2)
				h.addSource("set-b", "Sales", 4_500_000, 2)
				h.store.spent = tc.monthSpent
				h.agent.extract = extractCosting(1500)
				job := extractJob()
				job.Spend.USDMicros = tc.jobSpent
				outcome := h.run(t, job)
				if outcome.Status != model.FoundryGraphJobSucceeded || outcome.Progress.DocumentsDone != 4 || outcome.Progress.DocumentsTotal != 4 {
					t.Fatalf("outcome = %+v", outcome)
				}
				requests := h.extractRequests(t)
				if len(requests) != 2 {
					t.Fatalf("%d Extracts, want one per source", len(requests))
				}
				for i, want := range []struct {
					id   string
					docs []string
				}{{"job-2:set-a", pieces("set-a", 0, 2)}, {"job-2:set-b", pieces("set-b", 0, 2)}} {
					r := requests[i]
					if r.RequestID != want.id || !slices.Equal(docIDs(r), want.docs) || r.Load != "load-0" || r.Limits.MaxUSD != tc.want[i] {
						t.Fatalf("Extract %d is %s of %v with max_usd %v, want %s of %v with %v", i, r.RequestID, docIDs(r), r.Limits.MaxUSD,
							want.id, want.docs, tc.want[i])
					}
				}
				var report map[string]json.RawMessage
				if err := json.Unmarshal(outcome.Report, &report); err != nil || report["extract:set-a"] == nil || report["extract:set-b"] == nil {
					t.Fatalf("report = %s", outcome.Report)
				}
				// One read splits the sets; each source's own read comes just before its Extract.
				want := []spendRead{{"ws-1", []string{"set-a", "set-b"}}, {"ws-1", []string{"set-a"}}, {"ws-1", []string{"set-b"}}}
				if !reflect.DeepEqual(h.store.spendReads, want) {
					t.Fatalf("spend reads = %+v, want %+v", h.store.spendReads, want)
				}
				if h.store.lastUpdate().Phase != model.FoundryGraphJobFinishing {
					t.Fatalf("last phase = %s", h.store.lastUpdate().Phase)
				}
			})
		}
	})

	// A refresh sends the uploads, other kinds' documents and what the load
	// it refreshes recorded first, under the job's budget alone; then each
	// source's other pieces.
	t.Run("sends the rest and a refresh's replays first, then each API source", func(t *testing.T) {
		h := newHarness(t)
		job := refresh(h, new("load-0"))
		h.addSource("set-a", "Support", 0, 5)
		h.store.spends["set-notes"] = &model.FoundryDocumentSetSpend{SetID: "set-notes", ConnectorKind: "granola", ConnectorName: "Granola"}
		h.store.pending = append(h.store.pending, &model.FoundryDocument{
			ID: "note-1", WorkspaceID: "ws-1", SetID: "set-notes", MediaType: model.FoundryMediaMarkdown, S3Key: document.Key("ws-1", "note-1"),
			Source: &model.FoundryDocumentSource{ExternalID: "n1", Version: "v1", At: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		})
		h.bucket.put(document.Key("ws-1", "note-1"), []byte("# Standup"))
		h.store.prior = map[string]map[string]*model.FoundryDocumentExtraction{"load-0": {
			"set-a-p0": recorded(model.FoundryExtractionExtracted), "set-a-p1": recorded(model.FoundryExtractionDuplicate),
			"set-a-p2": recorded(model.FoundryExtractionOutOfScope), "set-a-p3": recorded(model.FoundryExtractionFailed),
		}}
		for _, id := range pieces("set-a", 0, 4) {
			h.bucket.put("ws/ws-1/documents/"+id+"/extractions/k.json.gz", []byte(`{}`))
		}
		h.agent.extract = extractCosting(1500)
		outcome := h.run(t, job)
		if outcome.Status != model.FoundryGraphJobSucceeded || outcome.Progress.DocumentsDone != 10 || outcome.Progress.DocumentsTotal != 10 {
			t.Fatalf("outcome = %+v", outcome)
		}
		requests := h.extractRequests(t)
		if len(requests) != 2 {
			t.Fatalf("%d Extracts", len(requests))
		}
		first := append([]string{"doc-a", "doc-b", "doc-c", "doc-d"}, append(pieces("set-a", 0, 3), "note-1")...)
		if r := requests[0]; r.RequestID != "job-1" || !slices.Equal(docIDs(r), first) || r.Limits.MaxUSD != 20 {
			t.Fatalf("the first Extract is %s of %v with max_usd %v", r.RequestID, docIDs(r), r.Limits.MaxUSD)
		}
		// The replays cost nothing: the source has all of its $5 left.
		if r := requests[1]; r.RequestID != "job-1:set-a" || !slices.Equal(docIDs(r), pieces("set-a", 3, 5)) || r.Limits.MaxUSD != 5 {
			t.Fatalf("the source's Extract is %s of %v with max_usd %v", r.RequestID, docIDs(r), r.Limits.MaxUSD)
		}
	})

	// Only a refresh replays, and only what the load it refreshes recorded
	// extracted, duplicate or out of scope: an extract's pieces are new to
	// its load, and what another ontology's load extracted counts against
	// the source again.
	t.Run("replays only what the load a refresh replaces recorded", func(t *testing.T) {
		otherOntology := map[string]map[string]*model.FoundryDocumentExtraction{"load-b": {"set-a-p0": recorded(model.FoundryExtractionExtracted)}}
		replaced := map[string]map[string]*model.FoundryDocumentExtraction{"load-0": {"set-a-p0": recorded(model.FoundryExtractionExtracted)}}
		for name, tc := range map[string]struct {
			job       func(*harness) *model.FoundryGraphJob
			prior     map[string]map[string]*model.FoundryDocumentExtraction
			free      []string
			loadReads int
		}{
			"a refresh": {func(h *harness) *model.FoundryGraphJob { return refresh(h, new("load-0")) }, replaced, []string{"set-a-p0"}, 1},
			"a refresh of a load that recorded none of them, though another ontology's did": {
				func(h *harness) *model.FoundryGraphJob { return refresh(h, new("load-0")) }, otherOntology, nil, 1},
			"a refresh of no load": {func(h *harness) *model.FoundryGraphJob { return refresh(h, nil) }, replaced, nil, 1},
			"an extract into a refresh": {func(*harness) *model.FoundryGraphJob {
				job := extractJob()
				job.Origin = model.FoundryLoadByRefresh
				return job
			}, replaced, nil, 0},
			"a load no one asked for that no refresh queued": {func(*harness) *model.FoundryGraphJob {
				job := loadJob()
				job.Origin = model.FoundryLoadByUser
				return job
			}, replaced, nil, 0},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.store.pending = nil
				h.addSource("set-a", "Support", 0, 2)
				h.store.prior = tc.prior
				h.agent.extract = extractCosting(1500)
				job := tc.job(h)
				if outcome := h.run(t, job); outcome.Status != model.FoundryGraphJobSucceeded {
					t.Fatalf("outcome = %+v", outcome)
				}
				requests := h.extractRequests(t)
				if want := 1 + min(len(tc.free), 1); len(requests) != want {
					t.Fatalf("%d Extracts, want %d", len(requests), want)
				}
				if tc.free != nil {
					if !slices.Equal(docIDs(requests[0]), tc.free) || requests[0].RequestID != job.ID {
						t.Fatalf("the first Extract is %s of %v: want the replays alone", requests[0].RequestID, docIDs(requests[0]))
					}
					requests = requests[1:]
				}
				if want := pieces("set-a", len(tc.free), 2); !slices.Equal(docIDs(requests[0]), want) || requests[0].RequestID != job.ID+":set-a" {
					t.Fatalf("the source's Extract is %s of %v: want %v", requests[0].RequestID, docIDs(requests[0]), want)
				}
				if h.store.loadReads != tc.loadReads {
					t.Fatalf("read the load %d times, want %d", h.store.loadReads, tc.loadReads)
				}
			})
		}
	})

	// A source with nothing left of its allowance, or of its workspace's,
	// keeps its pieces back: never downloaded, sent or recorded. The job
	// ends budget_exhausted, saying why, and is not retried: ended
	// succeeded with pieces it never recorded, its load's end would queue it
	// again at once, and so on every tick.
	t.Run("holds an API source whose month is spent", func(t *testing.T) {
		for name, tc := range map[string]struct {
			spent, deleted int64
			says           string
		}{
			"its allowance spent":          {5_000_000, 0, sourceSpent},
			"its allowance overspent":      {5_400_000, 0, sourceSpent},
			"the workspace's spent":        {1_000_000, 19_000_000, workspaceSpent},
			"a millionth of a dollar left": {4_999_999, 0, ""},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.addSource("set-a", "Support", tc.spent, 3)
				h.addSource("set-gone", "Deleted", tc.deleted, 0)
				h.agent.extract = extractCosting(1500)
				before := heldDocuments()
				outcome := h.run(t, loadJob())
				requests := h.extractRequests(t)
				if tc.says == "" {
					if outcome.Status != model.FoundryGraphJobSucceeded || len(requests) != 2 || requests[1].Limits.MaxUSD != 0.000001 ||
						outcome.HeldSets != nil {
						t.Fatalf("outcome = %+v after %d Extracts", outcome, len(requests))
					}
					return
				}
				if outcome.Status != model.FoundryGraphJobBudgetExhausted || outcome.RetryAt != nil || outcome.Uncounted || outcome.Error != tc.says ||
					!slices.Equal(outcome.HeldSets, []string{"set-a"}) {
					t.Fatalf("outcome = %+v, want budget_exhausted saying %q, holding set-a", outcome, tc.says)
				}
				if len(requests) != 1 || !slices.Equal(docIDs(requests[0]), []string{"doc-a", "doc-b", "doc-c", "doc-d"}) {
					t.Fatalf("%d Extracts: want only the uploads'", len(requests))
				}
				for _, id := range pieces("set-a", 0, 3) {
					if slices.Contains(h.bucket.opened, document.Key("ws-1", id)) || h.store.extractions[id] != nil {
						t.Fatalf("%s was downloaded or recorded", id)
					}
				}
				if held := heldDocuments() - before; held != 3 {
					t.Fatalf("held %v documents, want 3", held)
				}
			})
		}
	})

	// The engine checks max_usd before each model call, with the documents
	// in flight already started: a source's spend ends within its allowance
	// and those documents. Its next job holds the rest.
	t.Run("stops high-cost pieces within the allowance and the documents in flight", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending = nil
		h.addSource("set-a", "Support", 4_000_000, 10)
		const perDoc = 300_000
		h.agent.extract = extractCosting(perDoc)
		outcome := h.run(t, extractJob())
		if outcome.Status != model.FoundryGraphJobBudgetExhausted || outcome.Error != sourceSpent {
			t.Fatalf("outcome = %+v", outcome)
		}
		inFlight := int64(h.task.runner.cfg.Foundry.Graph.DocumentsInFlight)
		spent := h.store.spends["set-a"].USDMicros
		if requests := h.extractRequests(t); len(requests) != 1 || requests[0].Limits.MaxUSD != 1 || spent <= 5_000_000 ||
			spent > 5_000_000+inFlight*perDoc {
			t.Fatalf("%d Extracts; the source spent %d µ$, want past $5 by at most %d documents", len(requests), spent, inFlight)
		}
		recorded := len(h.store.extractions)
		h.store.pending = slices.DeleteFunc(h.store.pending, func(d *model.FoundryDocument) bool { return h.store.extractions[d.ID] != nil })
		before := heldDocuments()
		next := extractJob()
		next.ID = "job-5"
		outcome = h.run(t, next)
		requests := h.extractRequests(t)
		if outcome.Status != model.FoundryGraphJobBudgetExhausted || outcome.Error != sourceSpent || len(requests) != 2 ||
			len(requests[1].Documents) != 0 || len(h.store.extractions) != recorded || h.store.spends["set-a"].USDMicros != spent {
			t.Fatalf("the next job: outcome = %+v after %d Extracts, %d recorded", outcome, len(requests), len(h.store.extractions))
		}
		if held := heldDocuments() - before; held != float64(10-recorded) {
			t.Fatalf("held %v, want %d", held, 10-recorded)
		}
	})

	// An attempt owing the sweep that holds every piece still sweeps: with
	// no documents and nothing to spend.
	t.Run("sweeps with every piece held", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending = nil
		h.addSource("set-a", "Support", 5_000_000, 2)
		h.agent.extract = extractCosting(1500)
		outcome := h.run(t, extractJob())
		requests := h.extractRequests(t)
		if outcome.Status != model.FoundryGraphJobBudgetExhausted || outcome.Error != sourceSpent || len(requests) != 1 ||
			requests[0].RequestID != "job-2" || len(requests[0].Documents) != 0 || requests[0].Limits.MaxUSD != 0 {
			t.Fatalf("outcome = %+v after %d Extracts", outcome, len(requests))
		}
		if _, raw := h.agent.calls(); gjson(t, raw[0], "documents") != "[]" {
			t.Fatalf("the sweep sends %s", raw[0])
		}
	})

	// A person's job spends no allowance: the pieces go with everything
	// else, under the job's and the month's budget.
	t.Run("applies no allowance to a person's job", func(t *testing.T) {
		h := newHarness(t)
		h.addSource("set-a", "Support", 5_000_000, 3)
		h.agent.extract = extractCosting(1500)
		job := loadJob()
		job.RequestedByUserID = new("user-1")
		outcome := h.run(t, job)
		requests := h.extractRequests(t)
		if outcome.Status != model.FoundryGraphJobSucceeded || len(requests) != 1 || len(requests[0].Documents) != 7 ||
			requests[0].Limits.MaxUSD != 20 || len(h.store.spendReads) != 0 {
			t.Fatalf("outcome = %+v after %d Extracts, %d spend reads", outcome, len(requests), len(h.store.spendReads))
		}
	})

	// A refresh replays what the load it refreshes held, and holds the
	// source's new pieces: it ends partial, and holding what that load held
	// it replaces it all the same (FinishFoundryGraphJob; the provider's
	// "makes a partial refresh current once it holds what it replaces").
	t.Run("ends a refresh that holds pieces back partial, holding what its load held", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending = nil
		job := refresh(h, new("load-0"))
		h.addSource("set-a", "Support", 5_000_000, 4)
		h.store.prior = map[string]map[string]*model.FoundryDocumentExtraction{"load-0": {
			"set-a-p0": recorded(model.FoundryExtractionExtracted), "set-a-p1": recorded(model.FoundryExtractionDuplicate),
		}}
		for _, id := range pieces("set-a", 0, 2) {
			h.bucket.put("ws/ws-1/documents/"+id+"/extractions/k.json.gz", []byte(`{}`))
		}
		h.agent.extract = extractCosting(300_000)
		outcome := h.run(t, job)
		requests := h.extractRequests(t)
		if outcome.Status != model.FoundryGraphJobBudgetExhausted || outcome.Error != sourceSpent || len(requests) != 1 ||
			!slices.Equal(docIDs(requests[0]), pieces("set-a", 0, 2)) || !slices.Equal(outcome.HeldSets, []string{"set-a"}) {
			t.Fatalf("outcome = %+v after %d Extracts", outcome, len(requests))
		}
		for _, id := range pieces("set-a", 0, 2) {
			if e := h.store.extractions[id]; e == nil || e.Status != model.FoundryExtractionExtracted || e.USDMicros != 0 {
				t.Fatalf("%s recorded %+v, want replayed at $0", id, e)
			}
		}
		for _, id := range pieces("set-a", 2, 4) {
			if h.store.extractions[id] != nil {
				t.Fatalf("%s recorded", id)
			}
		}
	})

	// A source its allowance stops lets the next have its turn; the job's or
	// the month's budget stopping one ends the run, as for the first Extract.
	t.Run("an allowance's stop goes on to the next source; the budget's ends the run", func(t *testing.T) {
		// held are the sources the stop left unfinished, which the outcome
		// hands on (HeldSets): every one the run did not reach.
		for name, tc := range map[string]struct {
			monthSpent, jobSpent int64
			maxUSD               []float64
			says                 string
			held                 []string
		}{
			"the allowance":               {0, 0, []float64{0.3, 5}, `the API source "Support" spent its $5.00`, []string{"set-a"}},
			"the month":                   {79_800_000, 0, []float64{0.2}, reachedBudget, []string{"set-a", "set-b"}},
			"the job's cap, spent before": {0, 20_000_000, []float64{0}, "the load spent its $20.00 cap", []string{"set-a", "set-b"}},
			"the month's share of work no one started, spent before": {80_000_000, 0, []float64{0},
				"the $80.00 of this month's $100.00 that work no one started may use", []string{"set-a", "set-b"}},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.store.pending = nil
				h.addSource("set-a", "Support", 4_700_000, 5)
				h.addSource("set-b", "Sales", 0, 5)
				h.store.spent = tc.monthSpent
				h.agent.extract = extractCosting(300_000)
				job := loadJob()
				job.Spend.USDMicros = tc.jobSpent
				outcome := h.run(t, job)
				requests := h.extractRequests(t)
				if outcome.Status != model.FoundryGraphJobBudgetExhausted || !strings.Contains(outcome.Error, tc.says) || len(requests) != len(tc.maxUSD) ||
					!slices.Equal(outcome.HeldSets, tc.held) {
					t.Fatalf("outcome = %+v after %d Extracts", outcome, len(requests))
				}
				for i, r := range requests {
					if r.Limits.MaxUSD != tc.maxUSD[i] {
						t.Fatalf("Extract %d max_usd = %v, want %v", i, r.Limits.MaxUSD, tc.maxUSD[i])
					}
				}
				if len(requests) == 2 {
					for _, id := range pieces("set-b", 0, 5) {
						if h.store.extractions[id] == nil {
							t.Fatalf("%s of the next source was not extracted", id)
						}
					}
				}
			})
		}
	})

	// A stop in the Extract of the rest, before any source's, leaves every
	// source unfinished; a person's job, and a run that ends, hold none.
	t.Run("hands a stop before the sources' Extracts every source", func(t *testing.T) {
		h := newHarness(t)
		h.addSource("set-a", "Support", 0, 2)
		h.store.spent = 80_000_000
		h.agent.extract = extractCosting(300_000)
		outcome := h.run(t, loadJob())
		if requests := h.extractRequests(t); outcome.Status != model.FoundryGraphJobBudgetExhausted || len(requests) != 1 ||
			len(requests[0].Documents) != 4 || !slices.Equal(outcome.HeldSets, []string{"set-a"}) {
			t.Fatalf("outcome = %+v after %d Extracts", outcome, len(requests))
		}
		h = newHarness(t)
		h.addSource("set-a", "Support", 5_000_000, 2)
		h.store.spent = 99_800_000
		h.agent.extract = extractCosting(300_000)
		job := loadJob()
		job.RequestedByUserID = new("user-1")
		if outcome := h.run(t, job); outcome.Status != model.FoundryGraphJobBudgetExhausted || outcome.HeldSets != nil {
			t.Fatalf("a person's job: outcome = %+v", outcome)
		}
	})

	// The workspace's allowance is the sources' together: what one spends
	// in an attempt leaves the next less, read just before its Extract.
	t.Run("shares the workspace's allowance between the sources of one attempt", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending = nil
		h.addSource("set-a", "Support", 0, 5)
		h.addSource("set-b", "Sales", 0, 5)
		h.addSource("set-gone", "Deleted", 19_000_000, 0)
		h.agent.extract = extractCosting(300_000)
		before := heldDocuments()
		outcome := h.run(t, extractJob())
		requests := h.extractRequests(t)
		if outcome.Status != model.FoundryGraphJobBudgetExhausted || outcome.Error != workspaceSpent || len(requests) != 1 ||
			requests[0].Limits.MaxUSD != 1 || requests[0].RequestID != "job-2:set-a" {
			t.Fatalf("outcome = %+v after %d Extracts", outcome, len(requests))
		}
		if held := heldDocuments() - before; held != 5 {
			t.Fatalf("held %v, want the next source's 5", held)
		}
	})

	t.Run("retries what it can't read, with nothing sent", func(t *testing.T) {
		down := errors.New("db down")
		for name, setup := range map[string]func(h *harness) *model.FoundryGraphJob{
			"the sets' spend": func(h *harness) *model.FoundryGraphJob {
				h.store.spendHook = func(int) error { return down }
				return loadJob()
			},
			"a source's spend": func(h *harness) *model.FoundryGraphJob {
				h.store.spendHook = func(read int) error {
					if read == 2 {
						return down
					}
					return nil
				}
				return loadJob()
			},
			"a source whose set went": func(h *harness) *model.FoundryGraphJob {
				h.store.spendHook = func(read int) error {
					if read == 2 {
						delete(h.store.spends, "set-a")
					}
					return nil
				}
				return loadJob()
			},
			"the refresh's load": func(h *harness) *model.FoundryGraphJob {
				job := refresh(h, new("load-0"))
				h.store.loads = nil
				return job
			},
			"what the refreshed load recorded": func(h *harness) *model.FoundryGraphJob {
				h.store.priorErr = down
				return refresh(h, new("load-0"))
			},
			"the month's spend, just before a source's Extract": func(h *harness) *model.FoundryGraphJob {
				h.store.spendHook = func(read int) error {
					if read == 2 {
						h.store.sumErr = down
					}
					return nil
				}
				return loadJob()
			},
			// An extract reads no snapshot: only the pieces' downloads fail.
			"a source's pieces": func(h *harness) *model.FoundryGraphJob {
				h.bucket.refused = down
				return extractJob()
			},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.store.pending = nil
				h.addSource("set-a", "Support", 0, 2)
				outcome := h.run(t, setup(h))
				if requests := h.extractRequests(t); outcome.RetryAt == nil || len(requests) != 0 || len(h.store.extractions) != 0 {
					t.Fatalf("outcome = %+v after %d Extracts", outcome, len(requests))
				}
			})
		}
	})

	// A piece trimmed from its source's window between the listing and its
	// download is gone: it is marked deleted, and a source left with none
	// sends no Extract.
	t.Run("leaves out pieces whose originals are gone", func(t *testing.T) {
		h := newHarness(t)
		h.addSource("set-a", "Support", 0, 2)
		for _, id := range pieces("set-a", 0, 2) {
			delete(h.bucket.objects, document.Key("ws-1", id))
		}
		outcome := h.run(t, loadJob())
		if requests := h.extractRequests(t); outcome.Status != model.FoundryGraphJobSucceeded || len(requests) != 1 ||
			!slices.Equal(h.store.deleted, []string{"ws-1/set-a-p0", "ws-1/set-a-p1"}) {
			t.Fatalf("outcome = %+v after %d Extracts, deleted %v", outcome, len(requests), h.store.deleted)
		}
	})

	// Done means every piece a source's Extract was sent ended.
	t.Run("retries a source's Extract that left pieces unfinished", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending = nil
		h.addSource("set-a", "Support", 0, 2)
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{documentEvent("set-a-p0", `{"kind":"ingested"}`, usage)}, `{"status":"done","totals":`+totals+`}`)
		}
		if outcome := h.run(t, loadJob()); outcome.RetryAt == nil || !strings.Contains(outcome.Error, "unfinished") {
			t.Fatalf("outcome = %+v", outcome)
		}
	})
}

func TestShutdown(t *testing.T) {
	// hangIngest blocks the load until its call is cancelled, as a long load does.
	hangIngest := func(started chan struct{}) func(string, agentv1.Agent_IngestServer) error {
		return func(_ string, stream agentv1.Agent_IngestServer) error {
			close(started)
			<-stream.Context().Done()
			return stream.Context().Err()
		}
	}

	// A deploy stops an attempt that did nothing wrong: it is retried after
	// the backoff's floor and given back, even on the job's last attempt, so
	// deploys never use a job's attempts up.
	t.Run("retries a stopped attempt without counting it", func(t *testing.T) {
		for _, attempts := range []int{1, 5} {
			h := newHarness(t)
			h.task.cfg.ShutdownDrain = 50 * time.Millisecond
			started := make(chan struct{})
			h.agent.ingest = hangIngest(started)
			job := loadJob()
			job.Attempts = attempts
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				h.task.process(ctx, job)
				close(done)
			}()
			<-started
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the attempt did not stop after the drain")
			}
			if len(h.store.finished) != 1 {
				t.Fatalf("attempt %d: finished = %+v, want one retry", attempts, h.store.finished)
			}
			outcome := h.store.finished[0]
			if want := h.task.now().Add(h.task.cfg.RetryBackoff); outcome.RetryAt == nil || !outcome.RetryAt.Equal(want) || !outcome.Uncounted {
				t.Fatalf("attempt %d: outcome = %+v, want an interrupted retry at %v", attempts, outcome, want)
			}
		}
	})

	// The process's shutdown grace is shorter than the drain; Stop must
	// still wait for the attempt to record its outcome, and close the
	// agent's connection only after, or every deploy cuts a load off.
	t.Run("waits past a short deadline for the attempt to finish", func(t *testing.T) {
		h := newHarness(t)
		h.task.cfg.ShutdownDrain = 200 * time.Millisecond
		started := make(chan struct{})
		h.agent.ingest = hangIngest(started)
		h.store.claims = []*model.FoundryGraphJob{loadJob()}
		finishedAtClose := -1
		h.task.closeFn = func() error {
			h.store.mu.Lock()
			defer h.store.mu.Unlock()
			finishedAtClose = len(h.store.finished)
			return nil
		}
		ran := make(chan error, 1)
		go func() { ran <- h.task.Start(context.Background()) }()
		<-started
		h.store.mu.Lock() // the claim, after Start's setup, is ordered before Stop
		claimed := len(h.store.claims) == 0
		h.store.mu.Unlock()
		if !claimed {
			t.Fatal("the job was not claimed")
		}
		expired, cancel := context.WithDeadline(context.Background(), time.Now())
		defer cancel()
		if err := h.task.Stop(expired); err != nil {
			t.Fatalf("stop: %v", err)
		}
		if finishedAtClose != 1 {
			t.Fatalf("the agent's connection closed with %d outcomes recorded, want 1", finishedAtClose)
		}
		if outcome := h.store.finished[0]; outcome.RetryAt == nil || !outcome.Uncounted {
			t.Fatalf("outcome = %+v", outcome)
		}
		if err := <-ran; err != nil {
			t.Fatalf("start: %v", err)
		}
	})
}

func TestStart(t *testing.T) {
	// Each attempt removes what it staged as it ends, but a worker that died
	// (an OOM kill) left its last attempt's on the pod's volume: documents,
	// and the profiles People enrichment chose, which nothing else removes
	// before the pod goes. A worker starting removes them, and keeps the
	// snapshot cache.
	t.Run("removes what a worker that died left staged", func(t *testing.T) {
		h := newHarness(t)
		staged := filepath.Join(h.workDir, "jobs", "job-dead", "enrichment", ontologyagent.EnrichmentSourceID+".jsonl")
		cached := filepath.Join(h.workDir, ontologyagent.GraphCache, "kept")
		for _, path := range []string{staged, cached} {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"clue_email":"ada@acme.io"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		stopped, stop := context.WithCancel(context.Background())
		stop() // start, then stop at once
		if err := h.task.Start(stopped); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(h.workDir, "jobs")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the staging is still there: %v", err)
		}
		if _, err := os.Stat(cached); err != nil {
			t.Fatalf("the snapshot cache went too: %v", err)
		}
	})
}

func TestLeaseFence(t *testing.T) {
	// fastLease makes the fence (lease less the Helix timeout) 300ms, with renewals every 100ms.
	fastLease := func(h *harness) {
		h.task.cfg.HelixTimeoutSecs, h.task.cfg.Lease = 1, 1300*time.Millisecond
	}

	// With the database unreachable no renewal holds and the lease runs out;
	// a replacement worker may then claim the job. The attempt must stop,
	// and stop writing, before that can happen, or two agents write one
	// database.
	t.Run("stops an attempt whose lease can't be renewed", func(t *testing.T) {
		h := newHarness(t)
		fastLease(h)
		h.store.renewErr = errors.New("dial tcp: connection refused")
		lateWrite := make(chan error, 1)
		h.agent.ingest = func(req string, stream agentv1.Agent_IngestServer) error {
			<-stream.Context().Done()
			lateWrite <- writeThroughProxy(req)
			return stream.Context().Err()
		}
		started := time.Now()
		outcome := h.run(t, loadJob())
		if took := time.Since(started); took >= h.task.cfg.Lease {
			t.Fatalf("the attempt ran %s, past its lease of %s", took, h.task.cfg.Lease)
		}
		if outcome.RetryAt == nil {
			t.Fatalf("outcome = %+v, want a retry", outcome)
		}
		if err := <-lateWrite; err == nil {
			t.Fatal("a write after the fence reached the database")
		}
		if len(h.gateway.tenants) != 0 {
			t.Fatalf("the gateway saw %v", h.gateway.tenants)
		}
	})

	// A database that stops answering hangs the renewal rather than failing
	// it; the renewal must not hold the fence back past the lease.
	t.Run("stops an attempt whose renewal hangs", func(t *testing.T) {
		h := newHarness(t)
		fastLease(h)
		h.store.renewHangs = true
		h.agent.ingest = func(_ string, stream agentv1.Agent_IngestServer) error {
			<-stream.Context().Done()
			return stream.Context().Err()
		}
		started := time.Now()
		if outcome := h.run(t, loadJob()); outcome.RetryAt == nil {
			t.Fatalf("outcome = %+v, want a retry", outcome)
		}
		if took := time.Since(started); took >= h.task.cfg.Lease {
			t.Fatalf("the attempt ran %s, past its lease of %s", took, h.task.cfg.Lease)
		}
	})

	// A renewal that finds the job another worker's stops the attempt at once.
	t.Run("stops an attempt another worker owns", func(t *testing.T) {
		h := newHarness(t)
		fastLease(h)
		h.store.renewErr = foundrystore.ErrNotFound
		h.agent.ingest = func(_ string, stream agentv1.Agent_IngestServer) error {
			<-stream.Context().Done()
			return stream.Context().Err()
		}
		started := time.Now()
		if outcome := h.run(t, loadJob()); outcome.RetryAt == nil {
			t.Fatalf("outcome = %+v, want a retry", outcome)
		}
		if took := time.Since(started); took >= h.task.cfg.Lease {
			t.Fatalf("the attempt ran %s after losing its lease", took)
		}
	})

	// Each renewal moves the fence, so a load longer than one lease runs on.
	t.Run("keeps an attempt whose renewals hold", func(t *testing.T) {
		h := newHarness(t)
		fastLease(h)
		h.agent.ingest = func(req string, stream agentv1.Agent_IngestServer) error {
			select {
			case <-time.After(700 * time.Millisecond):
			case <-stream.Context().Done():
				return stream.Context().Err()
			}
			return ingestDone(req, stream)
		}
		if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
	})
}

// A job reclaimed past its attempts, after attempts that never finished, fails without running.
func TestReclaimedPastAttempts(t *testing.T) {
	h := newHarness(t)
	job := loadJob()
	job.Attempts = h.task.cfg.MaxAttempts + 1
	outcome := h.run(t, job)
	if ingests, _ := h.agent.calls(); outcome.Status != model.FoundryGraphJobFailed || outcome.RetryAt != nil || len(ingests) != 0 ||
		!strings.Contains(outcome.Error, "gave up") {
		t.Fatalf("outcome = %+v after %d ingests", outcome, len(ingests))
	}
}

// The task claims jobs until none is due, and refuses to start without a database.
func TestDrain(t *testing.T) {
	h := newHarness(t)
	h.store.claims = []*model.FoundryGraphJob{loadJob()}
	h.task.drain(context.Background())
	if len(h.store.finished) != 1 || h.store.finished[0].Status != model.FoundryGraphJobSucceeded {
		t.Fatalf("finished = %+v", h.store.finished)
	}
	if _, err := New(Deps{Cfg: &config.Config{}}); err == nil || !strings.Contains(err.Error(), "db") {
		t.Fatalf("New without a db = %v", err)
	}
}

func TestDeleteLoad(t *testing.T) {
	// A delete job removes its load through the proxy with no ontology or
	// mappings, a batch per request, and reports what it removed.
	t.Run("deletes the job's load through the proxy", func(t *testing.T) {
		h := newHarness(t)
		h.store.version = nil
		doneBefore := testutil.ToFloat64(loadDeletes.WithLabelValues("done"))
		outcome := h.run(t, deleteJob())
		if outcome.Status != model.FoundryGraphJobSucceeded || outcome.RetryAt != nil || outcome.Error != "" {
			t.Fatalf("outcome = %+v", outcome)
		}
		deletes := h.agent.deleteCalls()
		if ingests, extracts := h.agent.calls(); len(deletes) != 1 || len(ingests) != 0 || len(extracts) != 0 {
			t.Fatalf("calls = %d deletes, %d ingests, %d extracts", len(deletes), len(ingests), len(extracts))
		}
		var req ontologyagent.DeleteLoadRequest
		if err := json.Unmarshal([]byte(deletes[0]), &req); err != nil {
			t.Fatal(err)
		}
		if req.RequestID != "job-3" || req.Database != "tenant-1" || req.Load != "load-0" || req.Batch != 1000 ||
			!strings.HasPrefix(req.Helix.URL, "http://127.0.0.1:") || req.Helix.Token == "" || req.Helix.TimeoutSecs != 60 {
			t.Fatalf("delete request = %s", deletes[0])
		}
		if len(h.gateway.tenants) != 1 || h.gateway.tenants[0] != "tenant-1" || h.gateway.writer[0] != "true" {
			t.Fatalf("gateway saw %v %v", h.gateway.tenants, h.gateway.writer)
		}
		var report map[string]json.RawMessage
		if err := json.Unmarshal(outcome.Report, &report); err != nil || string(report["delete"]) != `{"status":"done","nodes":1,"edges":2}` {
			t.Fatalf("report = %s", outcome.Report)
		}
		if h.store.lastUpdate().Phase != model.FoundryGraphJobDeleting {
			t.Fatalf("phase = %s", h.store.lastUpdate().Phase)
		}
		if got := testutil.ToFloat64(loadDeletes.WithLabelValues("done")) - doneBefore; got != 1 {
			t.Fatalf("done deletes counted %v", got)
		}
	})

	// A deleted ontology holds its database until its loads are gone from
	// it; its delete jobs still bind, and nothing else does.
	t.Run("deletes a load of a deleted ontology", func(t *testing.T) {
		h := newHarness(t)
		now := time.Now()
		h.store.ontology.DeletedAt = &now
		if outcome := h.run(t, deleteJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if outcome := h.run(t, extractJob()); outcome.Status != model.FoundryGraphJobFailed || !strings.Contains(outcome.Error, "deleted") {
			t.Fatalf("an extract of a deleted ontology: %+v", outcome)
		}
	})

	// DeleteLoad is idempotent and resumable: whatever stopped it, a retry
	// goes on from what is left. A refusal or a result it can't read fails
	// the job, which leaves the load delete_failed and raises the metric.
	t.Run("retries what a retry can finish and fails the rest", func(t *testing.T) {
		result := func(result string) func(string, agentv1.Agent_DeleteLoadServer) error {
			return func(_ string, stream agentv1.Agent_DeleteLoadServer) error { return deleteEvents(stream, nil, result) }
		}
		refuse := func(code codes.Code) func(string, agentv1.Agent_DeleteLoadServer) error {
			return func(string, agentv1.Agent_DeleteLoadServer) error { return status.Error(code, "refused") }
		}
		for name, tc := range map[string]struct {
			script func(string, agentv1.Agent_DeleteLoadServer) error
			retry  bool
			says   string
		}{
			"cancelled":          {result(`{"status":"cancelled","nodes":1,"edges":0}`), true, "stopped"},
			"a write in doubt":   {result(`{"status":"outcome_unknown","label":"Customer","error":"timeout"}`), true, "may not have landed"},
			"a store refusal":    {result(`{"status":"store_failed","error":"no"}`), true, "refused a delete"},
			"a busy database":    {refuse(codes.Aborted), true, "another job"},
			"a shutting agent":   {refuse(codes.Unavailable), true, "unavailable"},
			"a full agent":       {refuse(codes.ResourceExhausted), true, "unavailable"},
			"a refused request":  {refuse(codes.InvalidArgument), false, "refused the request to delete a load"},
			"an older agent":     {refuse(codes.Unimplemented), false, ontologyagent.ContractVersion},
			"an unreadable done": {result(`{"status":"done"}`), false, "can't read"},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.agent.deleteLoad = tc.script
				failedBefore := testutil.ToFloat64(loadDeletes.WithLabelValues("failed"))
				outcome := h.run(t, deleteJob())
				switch {
				case tc.retry && outcome.RetryAt == nil:
					t.Fatalf("outcome = %+v, want a retry", outcome)
				case !tc.retry && (outcome.RetryAt != nil || outcome.Status != model.FoundryGraphJobFailed):
					t.Fatalf("outcome = %+v, want failed", outcome)
				case !strings.Contains(outcome.Error, tc.says):
					t.Fatalf("error %q does not say %q", outcome.Error, tc.says)
				}
				want := 1.0
				if tc.retry {
					want = 0
				}
				if got := testutil.ToFloat64(loadDeletes.WithLabelValues("failed")) - failedBefore; got != want {
					t.Fatalf("failed deletes counted %v, want %v", got, want)
				}
			})
		}
	})

	// A database moved out of the workspace can't be cleaned up from here;
	// the delete fails rather than retry.
	t.Run("fails a delete in a database moved away", func(t *testing.T) {
		h := newHarness(t)
		h.store.tenant.WorkspaceID = "ws-2"
		outcome := h.run(t, deleteJob())
		if outcome.Status != model.FoundryGraphJobFailed || len(h.agent.deleteCalls()) != 0 {
			t.Fatalf("outcome = %+v after %d deletes", outcome, len(h.agent.deleteCalls()))
		}
	})
}

// boosted turns foundry.graph.load_rate_limit on at 50 requests a second, burst 100.
func boosted(h *harness) {
	h.task.runner.cfg.Foundry.Graph.LoadRateLimit = config.GraphLoadRateLimit{RequestsPerSecond: 50, Burst: 100}
}

// wantReleased asserts the attempt's boost was taken back exactly once, as written.
func wantReleased(t *testing.T, h *harness) {
	t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if len(h.store.acquired) != 1 || len(h.store.released) != 1 || h.store.released[0] != h.store.acquired[0] {
		t.Fatalf("acquired %+v, released %+v", h.store.acquired, h.store.released)
	}
	if last := h.store.events[len(h.store.events)-1]; last != "unboost" {
		t.Fatalf("events = %v: something reached the database after the boost was taken back", h.store.events)
	}
}

// scripted is an agent call that writes once through the proxy, closes
// started when it isn't nil, then sends events and result, or, when result
// is "", waits for the attempt to end.
func scripted[S grpc.ServerStream](started chan struct{}, events []string, result string,
	send func(S, []string, string) error) func(string, S) error {
	return func(req string, stream S) error {
		if err := writeThroughProxy(req); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		if started != nil {
			close(started)
		}
		if result == "" {
			<-stream.Context().Done()
			return stream.Context().Err()
		}
		return send(stream, events, result)
	}
}

// jobKind is a kind of job as the rate limit tests run it. script makes its
// agent call scripted, ending with result; done, fails and retries are the
// results it succeeds, fails and is retried on.
type jobKind struct {
	job                  func() *model.FoundryGraphJob
	script               func(h *harness, started chan struct{}, result string)
	done, fails, retries string
}

// jobKinds are a load, an extract and a delete. Every kind writes the
// database, so each attempt holds a boost of its own job.
var jobKinds = map[string]jobKind{
	"load": {
		job: loadJob,
		script: func(h *harness, started chan struct{}, result string) {
			h.agent.ingest = scripted(started, nil, result, ingestEvents)
		},
		done:    `{"status":"done","report":{"chunks":2}}`,
		fails:   `{"status":"invalid_mappings","errors":["customers: no column email"]}`,
		retries: `{"status":"store_failed","error":"no"}`,
	},
	"extract": {
		job: extractJob,
		// Done means every document ended: each result follows them.
		script: func(h *harness, started chan struct{}, result string) {
			h.agent.extract = scripted(started, extracted, result, extractEvents)
		},
		done:    `{"status":"done","totals":` + totals + `}`,
		fails:   `{"status":"load_unknown"}`,
		retries: `{"status":"store_failed","error":"no"}`,
	},
	"delete": {
		job: deleteJob,
		script: func(h *harness, started chan struct{}, result string) {
			h.agent.deleteLoad = scripted(started, nil, result, deleteEvents)
		},
		done:    `{"status":"done","nodes":1,"edges":2}`,
		fails:   `{"status":"done"}`,
		retries: `{"status":"store_failed","error":"no"}`,
	},
}

// wantWrittenUnder asserts the attempt's one write landed between its boost
// reaching the gateway and the boost being taken back.
func wantWrittenUnder(t *testing.T, h *harness) {
	t.Helper()
	wantReleased(t, h)
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if events := strings.Join(h.store.events, ","); events != "boost,synced,write,unboost" {
		t.Fatalf("events = %s", events)
	}
}

func TestLoadRateLimit(t *testing.T) {
	// A job writes at the configured limit, not the plan's few requests a
	// second: the boost of its own job reaches the gateway before the first
	// write and is taken back once the attempt is over, after its proxy closes.
	t.Run("lifts the database's limit for the attempt, then lowers it", func(t *testing.T) {
		for name, k := range jobKinds {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				boosted(h)
				h.store.syncedAfter = 2
				k.script(h, nil, k.done)
				job := k.job()
				if outcome := h.run(t, job); outcome.Status != model.FoundryGraphJobSucceeded {
					t.Fatalf("outcome = %+v", outcome)
				}
				want := model.FoundryLoadBoost{
					TenantID: "tenant-1", Holder: model.FoundryBoostLoad, JobID: job.ID, Attempt: 1, RequestsPerSecond: 50, Burst: 100,
					TTL: h.task.runner.cfg.Foundry.Graph.Timeout + boost.Margin,
				}
				if len(h.store.acquired) != 1 || h.store.acquired[0] != want {
					t.Fatalf("acquired %+v, want %+v", h.store.acquired, want)
				}
				wantWrittenUnder(t, h)
				if h.store.syncReads != 3 {
					t.Fatalf("%d sync reads, want 3", h.store.syncReads)
				}
			})
		}
	})

	// A failed attempt must not leave its database at the job's limit.
	t.Run("lowers it when the attempt fails", func(t *testing.T) {
		for name, k := range jobKinds {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				boosted(h)
				k.script(h, nil, k.fails)
				if outcome := h.run(t, k.job()); outcome.Status != model.FoundryGraphJobFailed || outcome.RetryAt != nil {
					t.Fatalf("outcome = %+v", outcome)
				}
				wantWrittenUnder(t, h)
			})
		}
	})

	// Nor one that waits for a retry: the next attempt lifts it again.
	t.Run("lowers it when the attempt is retried", func(t *testing.T) {
		for name, k := range jobKinds {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				boosted(h)
				k.script(h, nil, k.retries)
				if outcome := h.run(t, k.job()); outcome.RetryAt == nil {
					t.Fatalf("outcome = %+v", outcome)
				}
				wantWrittenUnder(t, h)
			})
		}
	})

	// A deploy stops the attempt; the release runs on a context of its own.
	t.Run("lowers it when the worker stops", func(t *testing.T) {
		for name, k := range jobKinds {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				boosted(h)
				h.task.cfg.ShutdownDrain = 50 * time.Millisecond
				started := make(chan struct{})
				k.script(h, started, "")
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					h.task.process(ctx, k.job())
					close(done)
				}()
				<-started
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("the attempt did not stop")
				}
				if outcome := h.store.finished[0]; outcome.RetryAt == nil || !outcome.Uncounted {
					t.Fatalf("outcome = %+v", outcome)
				}
				wantWrittenUnder(t, h)
			})
		}
	})

	// An attempt fenced off its lease takes its own boost back; the
	// database's guard keeps it from taking a later attempt's.
	t.Run("lowers it when the attempt loses its lease", func(t *testing.T) {
		for name, k := range jobKinds {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				boosted(h)
				h.task.cfg.HelixTimeoutSecs, h.task.cfg.Lease = 1, 1300*time.Millisecond
				h.store.renewErr = foundrystore.ErrNotFound
				k.script(h, nil, "")
				if outcome := h.run(t, k.job()); outcome.RetryAt == nil {
					t.Fatalf("outcome = %+v", outcome)
				}
				wantWrittenUnder(t, h)
			})
		}
	})
	// Staff set the database's limit on purpose: the load runs at it, with
	// no wait for a sync that never comes.
	t.Run("runs at a staff override", func(t *testing.T) {
		h := newHarness(t)
		boosted(h)
		h.store.acquireErr = foundrystore.ErrConflict
		if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if h.store.syncReads != 0 || len(h.store.released) != 0 {
			t.Fatalf("%d sync reads and %d releases under a staff override", h.store.syncReads, len(h.store.released))
		}
	})

	// A gateway sync that is slow (a push cluster's, or an agent stuck
	// behind a deploy) costs the load its boost for a while, not the attempt.
	t.Run("writes at the gateway's limit once the wait for the sync is spent", func(t *testing.T) {
		h := newHarness(t)
		boosted(h)
		h.task.runner.syncWait = 30 * time.Millisecond
		h.store.syncedAfter = 1 << 30
		started := time.Now()
		if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if h.store.syncReads < 2 || time.Since(started) > 5*time.Second {
			t.Fatalf("%d sync reads in %s", h.store.syncReads, time.Since(started))
		}
		wantReleased(t, h)
	})

	// The boost is written only while the attempt holds its job: one that
	// lost it writes nothing more and is retried.
	t.Run("retries an attempt that no longer holds its job", func(t *testing.T) {
		h := newHarness(t)
		boosted(h)
		h.store.acquireErr = foundrystore.ErrNotFound
		outcome := h.run(t, loadJob())
		if outcome.RetryAt == nil || !strings.Contains(outcome.Error, "no longer holds") {
			t.Fatalf("outcome = %+v", outcome)
		}
		if ingests, _ := h.agent.calls(); len(ingests) != 0 || len(h.gateway.tenants) != 0 || len(h.store.released) != 0 {
			t.Fatalf("%d ingests, %d writes, %d releases", len(ingests), len(h.gateway.tenants), len(h.store.released))
		}
	})

	// A boost that failed to write may still have committed: the load runs
	// at the database's own limit, and the release takes back whatever landed.
	t.Run("runs at the database's own limit when lifting it fails", func(t *testing.T) {
		h := newHarness(t)
		boosted(h)
		h.store.acquireErr = errors.New("dial tcp: connection refused")
		if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if h.store.syncReads != 0 || len(h.store.released) != 1 {
			t.Fatalf("%d sync reads and %d releases", h.store.syncReads, len(h.store.released))
		}
	})

	// Off, the database keeps its own limit and nothing is written.
	t.Run("leaves the limit alone when off", func(t *testing.T) {
		h := newHarness(t)
		if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if len(h.store.acquired) != 0 || len(h.store.released) != 0 || h.store.syncReads != 0 {
			t.Fatalf("acquired %v, released %v, %d sync reads", h.store.acquired, h.store.released, h.store.syncReads)
		}
	})

	// A gateway pod that lags the boost, or a bucket that ran dry, turns a
	// write away before running it; the proxy sends it again, and the agent
	// sees it land.
	t.Run("sends a turned-away write again", func(t *testing.T) {
		h := newHarness(t)
		boosted(h)
		h.gateway.answers = []helixgateway.Response{{
			StatusCode: 503, ContentType: "application/json", Body: []byte(`{"error":"rate_limit_unavailable","msg":"tenant rate limit is unavailable"}`),
		}}
		if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if len(h.gateway.tenants) != 2 {
			t.Fatalf("the gateway saw %d writes, want the refused one and its resend", len(h.gateway.tenants))
		}
	})
}

func TestSweep(t *testing.T) {
	// A worker that died mid-attempt never took its boost back; the sweep
	// removes lapsed ones a batch at a time until none is left.
	t.Run("sweeps until a batch comes back short", func(t *testing.T) {
		h := newHarness(t)
		h.store.sweeps = []int{boost.SweepLimit, boost.SweepLimit, 3}
		h.task.sweep(context.Background())
		if h.store.sweepCalls != 3 {
			t.Fatalf("swept %d times, want 3", h.store.sweepCalls)
		}
	})

	// A failed sweep waits for the next tick rather than spin.
	t.Run("stops on an error", func(t *testing.T) {
		h := newHarness(t)
		h.store.sweepErr = errors.New("dial tcp: connection refused")
		h.task.sweep(context.Background())
		if h.store.sweepCalls != 1 {
			t.Fatalf("swept %d times, want 1", h.store.sweepCalls)
		}
	})

	// The worker sweeps when it starts, beside its claims.
	t.Run("sweeps when the worker starts", func(t *testing.T) {
		h := newHarness(t)
		h.task.cfg.HTTP.Listen = "127.0.0.1:0"
		ran := make(chan error, 1)
		go func() { ran <- h.task.Start(context.Background()) }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			h.store.mu.Lock()
			calls := h.store.sweepCalls
			h.store.mu.Unlock()
			if calls > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the worker did not sweep")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := h.task.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := <-ran; err != nil {
			t.Fatal(err)
		}
	})
}

// Decisions are filtered to the staged documents, ontology schema and load
// snapshot; choices do not re-commit a current load's existing documents.
func TestExtractReviewDecisions(t *testing.T) {
	h := newHarness(t)
	var def ontologyagent.Definition
	if err := json.Unmarshal(h.store.version.Definition, &def); err != nil {
		t.Fatal(err)
	}
	def.OntologyTOML = "[types.Customer]\nprops = { name = \"string\" }"
	h.store.version.Definition, _ = json.Marshal(def)
	ptr := func(s string) *string { return &s }
	// Settled at a time MySQL hands back in the server's zone, to the microsecond.
	settled := time.Date(2026, 10, 17, 1, 2, 3, 456789000, time.FixedZone("PDT", -7*3600))
	resolve := func(id, doc string) *model.FoundryReviewDecision {
		return &model.FoundryReviewDecision{ID: id, OntologyID: "ont-1", Kind: model.FoundryReviewResolve, Type: "Customer", DocID: ptr(doc), NormName: ptr("variant"), EntityKey: ptr("customer 1"), DecidedByUserID: "user", DecidedAt: settled}
	}
	h.store.decisions = []*model.FoundryReviewDecision{
		resolve("1", "doc-a"), resolve("2", "other-doc"),
		{ID: "3", OntologyID: "ont-1", Kind: model.FoundryReviewProp, Type: "Customer", EntityKey: ptr("customer 1"), Prop: ptr("name"), Value: json.RawMessage(`"Chosen"`), DecidedByUserID: "user",
			DecidedAt: settled.Add(time.Minute)},
		resolve("4", "doc-b"), resolve("5", "doc-a"),
	}
	h.store.decisions[3].EntityKey = nil
	h.store.decisions[4].Type = "Removed"
	removedProp := *h.store.decisions[2]
	removedProp.ID = "7"
	removedProp.Prop = ptr("removed")
	h.store.decisions = append(h.store.decisions, &removedProp)
	// Settled while Customer was keyed: the engine reviews its name again.
	stale := resolve("8", "doc-a")
	stale.NormName, stale.EntityKey = ptr("stale variant"), ptr("s42|i7")
	h.store.decisions = append(h.store.decisions, stale)
	outcome := h.run(t, extractJob())
	if outcome.Status != model.FoundryGraphJobSucceeded {
		t.Fatalf("%+v", outcome)
	}
	c := h.store.extractions["doc-a"].Counts
	if c.ResolvedUser != 7 || c.ResolvedAdjudicated != 8 || c.ResolvedIdentifier != 9 || c.Attributes != 4 || c.AttributeReviews != 3 || c.PropConflicts != 2 || c.IdentifiersUnverified != 5 || c.AttributesOverEntries != 6 {
		t.Fatalf("counts lost: %+v", c)
	}

	if len(h.store.decisionLoads) != 1 || h.store.decisionLoads[0] != "load-0" {
		t.Fatalf("wrong decision snapshot: %v", h.store.decisionLoads)
	}
	_, calls := h.agent.calls()
	if len(calls) != 1 {
		t.Fatalf("%d calls", len(calls))
	}
	req := decodeExtract(t, calls[0])
	if len(req.Decisions) != 4 {
		t.Fatalf("%+v", req.Decisions)
	}
	if req.Decisions[0].DocID != "doc-a" || req.Decisions[1].Kind != model.FoundryReviewProp || req.Decisions[2].EntityKey != nil || req.Decisions[2].DocID != "doc-b" ||
		*req.Decisions[3].EntityKey != "s42|i7" {
		t.Fatalf("%+v", req.Decisions)
	}
	// Each names itself by its id and when it was settled, in UTC, so the
	// load tells when a value it overwrites was decided.
	for i, want := range []struct{ id, at string }{
		{"1", `"2026-10-17T08:02:03.456789Z"`}, {"3", `"2026-10-17T08:03:03.456789Z"`},
		{"4", `"2026-10-17T08:02:03.456789Z"`}, {"8", `"2026-10-17T08:02:03.456789Z"`},
	} {
		at, _ := json.Marshal(req.Decisions[i].DecidedAt)
		if req.Decisions[i].DecisionID != want.id || string(at) != want.at || !strings.Contains(calls[0], `"decided_at":`+want.at) {
			t.Fatalf("decision %d = %s at %s, want %s at %s", i, req.Decisions[i].DecisionID, at, want.id, want.at)
		}
	}
	if strings.Contains(calls[0], `"decided_by_user_id"`) || strings.Contains(calls[0], "other-doc") {
		t.Fatal("audit or other-document data sent")
	}
}

// A decision settled under an earlier version can stop fitting: its prop's
// type or its type's key changed. A prop decision that no longer fits is
// left out and the rest go. A resolve whose chosen entity the changed key no
// longer reads goes too: the engine counts it stale and reviews its name
// again, where left out the name would be matched another way.
func TestExtractReviewDecisionsLeftOutOnceStale(t *testing.T) {
	h := newHarness(t)
	ptr := func(s string) *string { return &s }
	settled := time.Date(2026, 10, 17, 8, 2, 3, 0, time.UTC)
	prop := func(id, entity, name, value string) *model.FoundryReviewDecision {
		return &model.FoundryReviewDecision{ID: id, OntologyID: "ont-1", Kind: model.FoundryReviewProp, Type: "Customer",
			EntityKey: ptr(entity), Prop: ptr(name), Value: json.RawMessage(value), DecidedByUserID: "user", DecidedAt: settled}
	}
	link := &model.FoundryReviewDecision{ID: "4", OntologyID: "ont-1", Kind: model.FoundryReviewResolve, Type: "Customer",
		DocID: ptr("doc-a"), NormName: ptr("helio inc"), EntityKey: ptr("helio"), DecidedByUserID: "user", DecidedAt: settled}
	// Its own name: the engine refuses two resolves of one name in one
	// document, as the decisions' identity key does.
	notInData := *link
	notInData.ID, notInData.NormName, notInData.EntityKey = "5", ptr("acme corp"), nil
	h.store.decisions = []*model.FoundryReviewDecision{
		// Settled while Customer was named and tier a string.
		prop("1", "helio", "tier", `"gold"`), prop("2", "sacme", "tier", `"gold"`), link,
		// What fits the version now: a keyed customer, an integer tier.
		prop("3", "sacme", "tier", `3`), &notInData,
	}
	toml := "[types.Customer]\nprops = { account = \"string\", tier = \"integer\" }\nkey = [\"account\"]"
	decisions, err := h.task.runner.decisions(context.Background(), extractJob(), toml, []ontologyagent.ExtractDocument{{DocID: "doc-a"}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(decisions)
	const at = `"decided_at":"2026-10-17T08:02:03Z"`
	want := `[{"kind":"resolve","type":"Customer","doc_id":"doc-a","norm_name":"helio inc","entity_key":"helio","decision_id":"4",` + at + `},` +
		`{"kind":"prop","type":"Customer","entity_key":"sacme","prop":"tier","value":3,"decision_id":"3",` + at + `},` +
		`{"kind":"resolve","type":"Customer","doc_id":"doc-a","norm_name":"acme corp","entity_key":null,"decision_id":"5",` + at + `}]`
	if string(got) != want {
		t.Fatalf("decisions = %s, want %s", got, want)
	}
}

// A version whose key prop changed type reads no entity_key settled under
// the old one: a resolve to such an entity is still sent, for the engine to
// review its name again (decision::Choice::Stale). One the engine would
// refuse the whole Extract over, or of a type the version dropped, is not.
func TestExtractReviewDecisionsSendStaleResolvesOfAChangedKey(t *testing.T) {
	h := newHarness(t)
	ptr := func(s string) *string { return &s }
	settled := time.Date(2026, 10, 17, 8, 2, 3, 0, time.UTC)
	resolve := func(id, ty, name, key string) *model.FoundryReviewDecision {
		return &model.FoundryReviewDecision{ID: id, OntologyID: "ont-1", Kind: model.FoundryReviewResolve, Type: ty,
			DocID: ptr("doc-a"), NormName: ptr(name), EntityKey: ptr(key), DecidedByUserID: "user", DecidedAt: settled}
	}
	blankDoc := resolve("5", "Customer", "blank doc", "s42")
	blankDoc.DocID = ptr(" ")
	h.store.decisions = []*model.FoundryReviewDecision{
		// Settled while Customer was keyed by an integer account.
		resolve("1", "Customer", "helio inc", "i42"),
		// Settled under the version loaded: still the same entity.
		resolve("2", "Customer", "acme", "s42"),
		// A type the version dropped, a name the engine never normalizes to,
		// and no document: left out.
		resolve("3", "Supplier", "helio inc", "i42"), resolve("4", "Customer", "Helio Inc", "i42"), blankDoc,
	}
	toml := "[types.Customer]\nprops = { account = \"string\" }\nkey = [\"account\"]"
	decisions, err := h.task.runner.decisions(context.Background(), extractJob(), toml, []ontologyagent.ExtractDocument{{DocID: "doc-a"}, {DocID: " "}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(decisions)
	const at = `"decided_at":"2026-10-17T08:02:03Z"`
	want := `[{"kind":"resolve","type":"Customer","doc_id":"doc-a","norm_name":"helio inc","entity_key":"i42","decision_id":"1",` + at + `},` +
		`{"kind":"resolve","type":"Customer","doc_id":"doc-a","norm_name":"acme","entity_key":"s42","decision_id":"2",` + at + `}]`
	if string(got) != want {
		t.Fatalf("decisions = %s, want %s", got, want)
	}
}

func TestExtractReviewDecisionCap(t *testing.T) {
	for _, n := range []int{10000, 10001} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			h := newHarness(t)
			ptr := func(s string) *string { return &s }
			for i := 0; i < n; i++ {
				h.store.decisions = append(h.store.decisions, &model.FoundryReviewDecision{ID: fmt.Sprintf("%026d", i+1), OntologyID: "ont-1", Kind: model.FoundryReviewProp, Type: "Customer", EntityKey: ptr(fmt.Sprint(i)), Prop: ptr("name"), Value: json.RawMessage(`null`), DecidedByUserID: "user",
					DecidedAt: time.Date(2026, 10, 17, 8, 2, 3, 0, time.UTC)})
			}
			decisions, err := h.task.runner.decisions(context.Background(), loadJob(), "[types.Customer]\nprops = { name = \"string\" }", []ontologyagent.ExtractDocument{{DocID: "doc-a"}})
			if n == 10000 {
				if err != nil || len(decisions) != n {
					t.Fatalf("%d %v", len(decisions), err)
				}
			} else {
				var f *failure
				if !errors.As(err, &f) || !f.Permanent || decisions != nil {
					t.Fatalf("%v", err)
				}
			}
		})
	}
}

func TestExtractReviewDecisionFailures(t *testing.T) {
	h := newHarness(t)
	h.store.decisionErr = errors.New("db unavailable")
	_, err := h.task.runner.decisions(context.Background(), loadJob(), "[types.Customer]", nil)
	var f *failure
	if !errors.As(err, &f) || f.Permanent {
		t.Fatalf("%v", err)
	}
	h.store.decisionErr = nil
	_, err = h.task.runner.decisions(context.Background(), loadJob(), "[invalid", nil)
	if !errors.As(err, &f) || !f.Permanent {
		t.Fatalf("%v", err)
	}
	// decided_at is NOT NULL: a decision without its time is a broken row,
	// refused rather than sent as decided in year 1.
	ptr := func(s string) *string { return &s }
	h.store.decisions = []*model.FoundryReviewDecision{{ID: "1", OntologyID: "ont-1", Kind: model.FoundryReviewProp, Type: "Customer",
		EntityKey: ptr("c1"), Prop: ptr("name"), Value: json.RawMessage(`"Chosen"`), DecidedByUserID: "user"}}
	decisions, err := h.task.runner.decisions(context.Background(), loadJob(), "[types.Customer]\nprops = { name = \"string\" }", []ontologyagent.ExtractDocument{{DocID: "doc-a"}})
	if !errors.As(err, &f) || !f.Permanent || decisions != nil {
		t.Fatalf("%+v, %v", decisions, err)
	}
}

func TestLoadTableFiles(t *testing.T) {
	// withFiles records two of the set's table files on the version's
	// generation: a CSV read to rows, and a JSON document kept as it is.
	withFiles := func(t *testing.T, mappings string) *harness {
		t.Helper()
		h := newHarness(t)
		def, _ := json.Marshal(ontologyagent.Definition{
			Format: "agent_bundle", Status: ontologyagent.StatusDone, OntologyTOML: "version = \"draft\"\n", MappingsTOML: mappings,
			Draft: json.RawMessage(`{}`), Questions: json.RawMessage(`[]`),
		})
		h.store.version.Definition = def
		rows, original := document.RowsKey("ws-1", "doc-csv"), document.Key("ws-1", "doc-json")
		uploaded := time.Date(2026, 10, 17, 9, 15, 0, 0, time.UTC)
		h.store.generation.Input.DocumentSetIDs = []string{"set-files"}
		h.store.generation.Input.Documents = []model.OntologyGenerationDocument{{ID: "doc-csv", SetID: "set-files"}, {ID: "doc-json", SetID: "set-files"}}
		h.store.scope = []string{"set-files", "set-contracts", "set-general"}
		h.store.sets = []*model.FoundryDocumentSet{{ID: "set-files", WorkspaceID: "ws-1", Name: "Exports"}}
		h.store.files = map[string]*model.FoundryDocument{
			"doc-csv": {ID: "doc-csv", WorkspaceID: "ws-1", SetID: "set-files", Name: "orders.csv", MediaType: model.FoundryMediaCSV,
				Status: model.FoundryDocumentUploaded, UploadedAt: &uploaded,
				Read: &model.FoundryFileRead{Status: model.FoundryFileReadSucceeded, ArtifactKey: &rows}},
			"doc-json": {ID: "doc-json", WorkspaceID: "ws-1", SetID: "set-files", Name: "invoices.json", MediaType: model.FoundryMediaJSON,
				Status: model.FoundryDocumentUploaded, UploadedAt: &uploaded,
				Read: &model.FoundryFileRead{Status: model.FoundryFileReadSucceeded, ArtifactKey: &original}},
		}
		h.bucket.objects[rows] = gz(t, `{"id":1}`+"\n")
		h.bucket.objects[original] = []byte(`{"data":[{"id":"in_1"}]}`)
		return h
	}
	ingestSources := func(t *testing.T, h *harness) []map[string]any {
		t.Helper()
		ingests, _ := h.agent.calls()
		if len(ingests) != 1 {
			t.Fatalf("%d ingests", len(ingests))
		}
		var sources []map[string]any
		_ = json.Unmarshal([]byte(gjson(t, ingests[0], "sources")), &sources)
		return sources
	}

	// A load reads the generation's table files beside its synced tables,
	// each from its read, named by its document id.
	t.Run("ingests the table files the generation recorded", func(t *testing.T) {
		h := withFiles(t, "[mappings.orders]\nsource = \"doc-csv\"\n")
		if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		sources := ingestSources(t, h)
		if len(sources) != 3 || sources[1]["id"] != "doc-csv" || sources[1]["display_name"] != "Exports / orders.csv" ||
			sources[2]["id"] != "doc-json" || fmt.Sprint(sources[2]["access"]) == "" {
			t.Fatalf("sources = %v", sources)
		}
		if file := sources[2]["access"].(map[string]any)["file"].(map[string]any); file["format"] != "json" {
			t.Fatalf("the JSON file is read as %v", file["format"])
		}
		// Each table file says its upload, and the synced table its sync.
		ingests, _ := h.agent.calls()
		var observed []struct {
			Observed json.RawMessage `json:"observed"`
		}
		_ = json.Unmarshal([]byte(gjson(t, ingests[0], "sources")), &observed)
		for i, want := range []string{firstObserved,
			`{"upload":{"document_id":"doc-csv","uploaded_at":"2026-10-17T09:15:00Z"}}`,
			`{"upload":{"document_id":"doc-json","uploaded_at":"2026-10-17T09:15:00Z"}}`} {
			if got := string(observed[i].Observed); got != want {
				t.Fatalf("source %d observed %s, want %s", i, got, want)
			}
		}
	})

	// A file the version maps that was deleted, or moved out of the
	// version's sets, fails the load for good, naming it: the version maps
	// a table that is no longer there until it is revised.
	t.Run("fails for good when a file the version maps is gone", func(t *testing.T) {
		for name, gone := range map[string]func(*model.FoundryDocument){
			"deleted": func(d *model.FoundryDocument) { d.Status = model.FoundryDocumentDeleted },
			"moved":   func(d *model.FoundryDocument) { d.SetID = "set-elsewhere" },
		} {
			h := withFiles(t, "[mappings.orders]\nsource = \"doc-csv\"\n")
			gone(h.store.files["doc-csv"])
			outcome := h.run(t, loadJob())
			ingests, _ := h.agent.calls()
			if outcome.Status != model.FoundryGraphJobFailed || outcome.RetryAt != nil || len(ingests) != 0 ||
				!strings.Contains(outcome.Error, `the file "orders.csv" this version maps was deleted or moved out of its document sets`) {
				t.Fatalf("%s: outcome = %+v after %d ingests", name, outcome, len(ingests))
			}
		}
	})

	// A gone file the version doesn't map is simply left out.
	t.Run("leaves out a gone file the version doesn't map", func(t *testing.T) {
		h := withFiles(t, "[mappings.orders]\nsource = \"doc-csv\"\n")
		h.store.files["doc-json"].Status = model.FoundryDocumentDeleted
		if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if sources := ingestSources(t, h); len(sources) != 2 || sources[1]["id"] != "doc-csv" {
			t.Fatalf("sources = %v", sources)
		}
	})

	// A version drafted from documents alone has no table: the load still
	// ingests, with no source, which creates the load the extraction writes
	// into, then extracts.
	t.Run("loads a version with no table", func(t *testing.T) {
		h := newHarness(t)
		h.store.generation.Input = model.OntologyGenerationInput{DocumentSetIDs: []string{"set-contracts", "set-general"}}
		outcome := h.run(t, loadJob())
		ingests, extracts := h.agent.calls()
		if outcome.Status != model.FoundryGraphJobSucceeded || len(ingests) != 1 || len(extracts) != 1 || gjson(t, ingests[0], "sources") != "[]" {
			t.Fatalf("outcome = %+v after %d ingests (%v), %d extracts", outcome, len(ingests), ingests, len(extracts))
		}
	})
}

// missingSnapshots answers every snapshot read as gone from the store.
type missingSnapshots struct{ *bucket }

func (missingSnapshots) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("get: %w", snapshot.ErrMissing)
}

// observed records a histogram's observations.
type observed struct{ values []float64 }

func (o *observed) Observe(v float64) { o.values = append(o.values, v) }

// The queue wait of a refresh's first attempt says whether the deployment's graph
// workers keep up; a user's load and a retry's backoff are not that wait.
func TestRefreshLag(t *testing.T) {
	h := newHarness(t)
	queued := h.task.now().Add(-5 * time.Minute)
	for _, job := range []*model.FoundryGraphJob{
		{Origin: model.FoundryLoadByRefresh, Attempts: 1},
		{Origin: model.FoundryLoadByRefresh, Attempts: 2},
		{Origin: model.FoundryLoadByUser, Attempts: 1},
	} {
		j := loadJob()
		j.Origin, j.Attempts, j.CreatedAt = job.Origin, job.Attempts, queued
		h.run(t, j)
	}
	if got := h.task.refreshLag.(*observed).values; !slices.Equal(got, []float64{300}) {
		t.Fatalf("refresh lag observations = %v, want [300]", got)
	}
}

func TestLoadPins(t *testing.T) {
	first := []model.FoundryLoadSource{firstPin}
	// observed is what the job's one Ingest said of each source's observation.
	observed := func(t *testing.T, h *harness) []string {
		t.Helper()
		ingests, _ := h.agent.calls()
		if len(ingests) != 1 {
			t.Fatalf("%d ingests", len(ingests))
		}
		var req struct {
			Sources []struct {
				Observed json.RawMessage `json:"observed"`
			} `json:"sources"`
		}
		if err := json.Unmarshal([]byte(ingests[0]), &req); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range req.Sources {
			out = append(out, string(s.Observed))
		}
		return out
	}
	// A sync between attempts must not change the data under a load.
	t.Run("pins the snapshots its first attempt reads", func(t *testing.T) {
		h := newHarness(t)
		if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if got := h.store.pins["job-1"]; !reflect.DeepEqual(got, first) {
			t.Fatalf("pins = %+v", got)
		}
		if got := observed(t, h); !slices.Equal(got, []string{firstObserved}) {
			t.Fatalf("observed = %v, want the pinned sync", got)
		}
	})
	// The retry says the sync of the snapshot it reads, not the newer one.
	t.Run("reads the pinned snapshots after a sync replaced them", func(t *testing.T) {
		h := newHarness(t)
		h.store.tables = []*model.ConnectorTable{{ConnectorID: "conn-1", TableName: "customers", S3Key: "ws/ws-1/connectors/conn-1/customers/s2.jsonl.gz",
			SyncID: "01JDSYNC000000000000000002", SyncedAt: firstPin.SyncedAt.Add(time.Hour), ChangeSeq: 8}}
		job := loadJob()
		job.Sources = first
		if outcome := h.run(t, job); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if _, repinned := h.store.pins["job-1"]; repinned {
			t.Fatal("a retry pinned again")
		}
		if got := observed(t, h); !slices.Equal(got, []string{firstObserved}) {
			t.Fatalf("observed = %v, want the pinned sync", got)
		}
	})
	// A job pinned before pins kept their sync ingests with no observed,
	// which the agent takes, rather than a sync it can't name.
	t.Run("a pin without its sync sends no observed", func(t *testing.T) {
		h := newHarness(t)
		job := loadJob()
		job.Sources = []model.FoundryLoadSource{{ConnectorID: firstPin.ConnectorID, Table: firstPin.Table, S3Key: firstPin.S3Key}}
		if outcome := h.run(t, job); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if got := observed(t, h); !slices.Equal(got, []string{""}) {
			t.Fatalf("observed = %v, want none", got)
		}
	})
	// The sweep spares a replaced snapshot only for its grace until a load pins
	// it, so the pin can't wait for a download that may take longer.
	t.Run("pins the snapshots before downloading them", func(t *testing.T) {
		h := newHarness(t)
		h.task.runner.snapshots.Snapshots = missingSnapshots{h.bucket}
		if outcome := h.run(t, loadJob()); outcome.Status == model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v, want the download to fail", outcome)
		}
		if got := h.store.pins["job-1"]; !reflect.DeepEqual(got, first) {
			t.Fatalf("pins = %+v, want them recorded before the download", got)
		}
	})
	// Only a sweep racing the pin can lose it; the load can't read what it started from.
	t.Run("fails for good when a pinned snapshot is gone", func(t *testing.T) {
		h := newHarness(t)
		h.task.runner.snapshots.Snapshots = missingSnapshots{h.bucket}
		job := loadJob()
		job.Sources = first
		outcome := h.run(t, job)
		if outcome.Status != model.FoundryGraphJobFailed || !strings.Contains(outcome.Error, "start a new load") {
			t.Fatalf("outcome = %+v", outcome)
		}
	})
}

// ownKey is a workspace's own Anthropic key in the tests.
const ownKey = "sk-ant-api03-own-sentinel-9876543210"

// withOwnKey makes the workspace's setting its owner's Claude, sealed under
// a test keyring.
func withOwnKey(t *testing.T, h *harness) {
	t.Helper()
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	h.ai.Keyring = keys
	settings, err := json.Marshal(ai.Settings{Provider: ai.Anthropic{}, Models: ai.Roles{
		Draft: ai.Model{Name: "claude-opus-5-5", Price: ai.PriceTable{}}, Ask: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}},
		Extract: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}}, Verify: ai.Model{Name: "claude-opus-5", Price: ai.PriceTable{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	c := &model.FoundryAIConfig{ID: "cfg-1", WorkspaceID: "ws-1", Kind: "anthropic", Settings: settings}
	if c.Secret, err = ai.SealKey(keys, c.ID, ontologyagent.NewModelKey(ownKey)); err != nil {
		t.Fatal(err)
	}
	h.store.aiConfig = c
}

// runLogged runs an attempt of job, every log line of it captured, and
// returns its outcome, the log and what it left in the job's rows.
func (h *harness) runLogged(t *testing.T, job *model.FoundryGraphJob) (model.FoundryGraphJobOutcome, string, string) {
	t.Helper()
	var logs bytes.Buffer
	h.task.process(log.Into(context.Background(), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))), job)
	if len(h.store.finished) == 0 {
		t.Fatal("the attempt recorded no outcome")
	}
	rows, err := json.Marshal(struct {
		Finished    []model.FoundryGraphJobOutcome
		Extractions map[string]*model.FoundryDocumentExtraction
	}{h.store.finished, h.store.extractions})
	if err != nil {
		t.Fatal(err)
	}
	return h.store.finished[len(h.store.finished)-1], logs.String(), string(rows)
}

// extractFailing ends doc-a ingested and doc-b failed with the model's
// failure, then the call done.
func extractFailing(failure string) func(string, agentv1.Agent_ExtractServer) error {
	return func(_ string, stream agentv1.Agent_ExtractServer) error {
		return extractEvents(stream, []string{
			documentEvent("doc-a", `{"kind":"ingested"}`, usage),
			documentEvent("doc-b", `{"kind":"failed","code":"model","model_failure":"`+failure+`","retryable":true}`, noUsage),
			documentEvent("doc-c", `{"kind":"ingested"}`, usage),
			documentEvent("doc-d", `{"kind":"ingested"}`, usage),
		}, `{"status":"done","totals":`+totals+`}`)
	}
}

func TestModelKeys(t *testing.T) {
	// Helix's Claude extracts with its key in the call's metadata alone.
	t.Run("extracts on Helix's Claude", func(t *testing.T) {
		h := newHarness(t)
		outcome, logs, rows := h.runLogged(t, extractJob())
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		_, extracts := h.agent.calls()
		const house = `"models":{"endpoint":{"provider":"anthropic","call_timeout_secs":600},` +
			`"extract":{"name":"claude-haiku-5-5","price":"table","reasoning":true},"verify":{"name":"claude-sonnet-5-5","price":"table","reasoning":true}}`
		if !strings.Contains(extracts[0], house) || fmt.Sprint(h.agent.keys) != "[["+houseKey+"]]" {
			t.Fatalf("extract %s, keys %v", extracts[0], h.agent.keys)
		}
		if strings.Contains(extracts[0]+logs+rows, houseKey) {
			t.Fatal("the key reached a request, a log or a row")
		}
	})

	// A workspace's own key extracts on its models. A document its provider
	// refused stops the load as partial with the owner's copy, the document
	// keeping its code: every other would fail alike. No key leaks.
	t.Run("stops a load the owner's key can't run", func(t *testing.T) {
		h := newHarness(t)
		withOwnKey(t, h)
		h.agent.extract = extractFailing("auth")
		outcome, logs, rows := h.runLogged(t, extractJob())
		if outcome.Status != model.FoundryGraphJobBudgetExhausted ||
			outcome.Error != "Claude refused the API key. An owner can enter a new one in Settings › AI models." {
			t.Fatalf("outcome = %+v", outcome)
		}
		if e := h.store.extractions["doc-b"]; e == nil || e.Error == nil || e.Error.Code != model.FoundryExtractionModel || !e.Error.Retryable {
			t.Fatalf("doc-b = %+v", e)
		}
		if _, ok := h.store.extractions["doc-d"]; ok {
			t.Fatal("the load went on past a refused key")
		}
		_, extracts := h.agent.calls()
		if !strings.Contains(extracts[0], `"verify":{"name":"claude-opus-5","price":"table","reasoning":true}`) ||
			fmt.Sprint(h.agent.keys) != "[["+ownKey+"]]" {
			t.Fatalf("extract %s, keys %v", extracts[0], h.agent.keys)
		}
		for _, secret := range []string{ownKey, houseKey} {
			if strings.Contains(extracts[0]+logs+rows, secret) {
				t.Fatalf("a key reached a request, a log or a row")
			}
		}
	})

	// A job no person started extracts each API source in an Extract of its
	// own, each on the owner's key. A key refused there stops the load as a
	// spent budget does, holding the sources it left unfinished: the one
	// refused and every one after it.
	t.Run("holds the API sources a refused key left unfinished", func(t *testing.T) {
		h := newHarness(t)
		withOwnKey(t, h)
		h.store.pending = nil
		h.addSource("set-a", "Support", 0, 2)
		h.addSource("set-b", "Sales", 0, 2)
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{
				documentEvent("set-a-p0", `{"kind":"failed","code":"model","model_failure":"auth","retryable":true}`, noUsage),
			}, `{"status":"done","totals":`+totals+`}`)
		}
		outcome := h.run(t, extractJob())
		requests := h.extractRequests(t)
		if outcome.Status != model.FoundryGraphJobBudgetExhausted ||
			outcome.Error != "Claude refused the API key. An owner can enter a new one in Settings › AI models." ||
			len(requests) != 1 || requests[0].RequestID != "job-2:set-a" || !slices.Equal(outcome.HeldSets, []string{"set-a", "set-b"}) {
			t.Fatalf("outcome = %+v after %d Extracts", outcome, len(requests))
		}
		if fmt.Sprint(h.agent.keys) != "[["+ownKey+"]]" {
			t.Fatalf("keys = %v", h.agent.keys)
		}
	})

	// A document's own trouble, or Helix's key's, never stops the load; the
	// house's is raised with on-call.
	t.Run("goes on past a document's own failure", func(t *testing.T) {
		for name, tc := range map[string]struct {
			own     bool
			failure string
			raised  bool
		}{
			"Helix's key refused":       {false, "auth", true},
			"a document too long":       {true, "context_too_long", false},
			"the owner's provider down": {true, "unavailable", false},
		} {
			h := newHarness(t)
			if tc.own {
				withOwnKey(t, h)
			}
			h.agent.extract = extractFailing(tc.failure)
			outcome, logs, rows := h.runLogged(t, extractJob())
			if outcome.Status != model.FoundryGraphJobSucceeded || len(h.store.extractions) != 4 {
				t.Fatalf("%s: outcome = %+v, %d documents", name, outcome, len(h.store.extractions))
			}
			if raised := strings.Contains(logs, `"level":"ERROR"`); raised != tc.raised {
				t.Fatalf("%s: raised %v: %s", name, raised, logs)
			}
			if strings.Contains(logs+rows, ownKey) || strings.Contains(logs+rows, houseKey) {
				t.Fatalf("%s: a key reached a log or a row", name)
			}
		}
	})

	// A setting that can't run fails the load before Extract is called.
	t.Run("stops at a setting that can't run", func(t *testing.T) {
		h := newHarness(t)
		withOwnKey(t, h)
		h.store.aiConfig.Secret.Ciphertext[0] ^= 0xff
		outcome, logs, _ := h.runLogged(t, extractJob())
		_, extracts := h.agent.calls()
		if outcome.Status != model.FoundryGraphJobFailed || !strings.HasPrefix(outcome.Error, "The stored API key can't be opened") ||
			len(extracts) != 0 || strings.Contains(logs, ownKey) {
			t.Fatalf("outcome = %+v, %d extracts", outcome, len(extracts))
		}
	})
}
