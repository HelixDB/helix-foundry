package foundrygraphworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/enrichment"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// enrichmentState is the store's People enrichment: the workspace's consent,
// each load's step, and what the job's attempts did with them.
type enrichmentState struct {
	consent *model.FoundryEnrichmentConsent
	steps   map[string]*model.FoundryLoadEnrichment
	starts  int
	// parks are the times the job was parked until.
	parks []time.Time
	// recorded is what Ingest bound: applied, then unmatched.
	recorded []int64
}

func (s *memStore) step(loadID string) *model.FoundryLoadEnrichment {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.enrich.steps[loadID]; ok {
		copied := *row
		return &copied
	}
	return nil
}

func (s *memStore) StartFoundryLoadEnrichment(_ context.Context, _ string, job *model.FoundryGraphJob) (*model.FoundryLoadEnrichment, error) {
	s.mu.Lock()
	s.enrich.starts++
	if _, ok := s.enrich.steps[job.LoadID]; !ok {
		s.enrich.steps[job.LoadID] = &model.FoundryLoadEnrichment{
			LoadID: job.LoadID, WorkspaceID: job.WorkspaceID, OntologyID: job.OntologyID, JobID: job.ID, Status: model.FoundryEnrichmentPreparing,
		}
	}
	s.mu.Unlock()
	return s.step(job.LoadID), nil
}

func (s *memStore) GetFoundryLoadEnrichment(_ context.Context, loadID string) (*model.FoundryLoadEnrichment, error) {
	if row := s.step(loadID); row != nil {
		return row, nil
	}
	return nil, foundrystore.ErrNotFound
}

func (s *memStore) GetFoundryEnrichmentConsent(context.Context, string) (*model.FoundryEnrichmentConsent, error) {
	if s.enrich.consent == nil {
		return nil, foundrystore.ErrNotFound
	}
	return s.enrich.consent, nil
}

func (s *memStore) GetLatestFoundryEnrichmentResults(context.Context, string) (*model.FoundryLoadEnrichment, error) {
	return nil, foundrystore.ErrNotFound
}

func (s *memStore) GetLatestFoundryEnrichmentApplied(context.Context, string) (*model.FoundryLoadEnrichment, error) {
	return nil, foundrystore.ErrNotFound
}

func (s *memStore) GetFoundryEnrichmentLedger(context.Context, string, time.Time) (model.FoundryEnrichmentLedger, error) {
	return model.FoundryEnrichmentLedger{}, nil
}

func (s *memStore) GetFoundryEnrichmentProvider(_ context.Context, name string) (*model.FoundryEnrichmentProvider, error) {
	return &model.FoundryEnrichmentProvider{Provider: name}, nil
}

func (s *memStore) QueueFoundryLoadEnrichment(_ context.Context, loadID, _ string, q model.FoundryEnrichmentQueue) (*model.FoundryLoadEnrichment, error) {
	s.mu.Lock()
	row := s.enrich.steps[loadID]
	row.Status, row.PlanKey, row.CarriedKey, row.Counts, row.ClaimBy = model.FoundryEnrichmentQueued, &q.PlanKey, &q.CarriedKey, q.Counts, &q.ClaimBy
	s.mu.Unlock()
	return s.step(loadID), nil
}

func (s *memStore) EndPreparedFoundryLoadEnrichment(_ context.Context, loadID, _ string, p model.FoundryEnrichmentPrepared) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.enrich.steps[loadID]
	applied := p.Applied
	row.Status, row.StoppedBy, row.Counts, row.Applied = p.End.Status(), p.End.StoppedBy(), p.Counts, &applied
	return nil
}

func (s *memStore) ApplyFoundryLoadEnrichment(_ context.Context, loadID, _ string, applied model.FoundryEnrichmentApplied) (model.FoundryEnrichmentApplied, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.enrich.steps[loadID]
	if row.Applied == nil {
		row.Applied = &applied
	}
	return *row.Applied, nil
}

func (s *memStore) EndBusyFoundryLoadEnrichment(_ context.Context, loadID, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row := s.enrich.steps[loadID]; row.Status == model.FoundryEnrichmentQueued {
		busy := model.FoundryEnrichmentStopBusy
		row.Status, row.StoppedBy = model.FoundryEnrichmentPartial, &busy
	}
	return nil
}

func (s *memStore) RecordFoundryLoadEnrichmentIngest(_ context.Context, loadID, _ string, applied, unmatched int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enrich.recorded = append(s.enrich.recorded, applied, unmatched)
	s.enrich.steps[loadID].Counts.Applied, s.enrich.steps[loadID].Counts.Unmatched = applied, unmatched
	return nil
}

func (s *memStore) ParkFoundryGraphJob(_ context.Context, _, _ string, until time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enrich.parks = append(s.enrich.parks, until)
	return false, nil
}

func (f *fakeAgent) Clues(req *agentv1.CluesRequest, stream agentv1.Agent_CluesServer) error {
	f.mu.Lock()
	f.cluesCalls = append(f.cluesCalls, req.GetRequestJson())
	script := f.clues
	f.mu.Unlock()
	return script(req.GetRequestJson(), stream)
}

// proPlans answers whether a workspace is on Foundry Pro.
type proPlans bool

func (p proPlans) Status(context.Context, string) (foundryplan.Status, error) {
	if p {
		return foundryplan.Status{Plan: &foundryplan.Plan{Code: "pro", AutumnPlanID: "foundry-pro", Rank: 1}}, nil
	}
	return foundryplan.Status{Plan: &foundryplan.Plan{Code: "free"}}, nil
}

// cluesOf streams the people Clues found, with their counts.
func cluesOf(people ...ontologyagent.CluedEntity) func(string, agentv1.Agent_CluesServer) error {
	return func(_ string, stream agentv1.Agent_CluesServer) error {
		page, _ := json.Marshal(map[string]any{"event": "clues", "entities": people})
		done, _ := json.Marshal(map[string]any{"event": "done", "entities": len(people), "no_clue": 0, "no_name": 0, "conflicting": 0, "shared": 0})
		return cluesEvents(stream, `{"event":"clues_started","contract_version":"3.9.0"}`, string(page), string(done))
	}
}

func cluesEvents(stream agentv1.Agent_CluesServer, events ...string) error {
	for _, e := range events {
		if err := stream.Send(&agentv1.CluesEvent{EventJson: e}); err != nil {
			return err
		}
	}
	return nil
}

// The people in the tables: Ada can be looked up; Cher has no full name.
var (
	ada  = ontologyagent.CluedEntity{Entity: "Person[c_17]", Name: "Ada Lovelace", Email: "ada@acme.io"}
	cher = ontologyagent.CluedEntity{Entity: "Person[c_18]", Name: "Cher", Email: "cher@acme.io"}
)

// enrichingVersion makes the harness's version enrich its people, with the
// workspace's consent, Helix's flag and Foundry Pro all on: what each test
// turns off is its point.
func enrichingVersion(t *testing.T, h *harness) {
	t.Helper()
	def, _ := json.Marshal(ontologyagent.Definition{
		Format: "agent_bundle", Status: ontologyagent.StatusDone, OntologyTOML: "version = \"draft\"\n",
		MappingsTOML: "ontology = \"draft\"\n[enrichments.people]\ntype = \"Person\"\nclues = { email = \"email\" }\n" +
			"props = { current_title = \"current_title\" }\n",
		Draft: json.RawMessage(`{}`), Questions: json.RawMessage(`[]`),
	})
	h.store.version.Definition = def
	h.store.enrich.consent = &model.FoundryEnrichmentConsent{Enabled: true, MonthlyCreditCap: 50_000}
	h.task.runner.enrichment.Cfg.Enabled = true
	h.agent.clues = cluesOf(ada)
}

// profilesSource is the foundry_enrichment source an Ingest request sends,
// nil when it sends none.
func profilesSource(t *testing.T, request string) map[string]any {
	t.Helper()
	var req struct {
		Sources []map[string]any `json:"sources"`
	}
	if err := json.Unmarshal([]byte(request), &req); err != nil {
		t.Fatal(err)
	}
	for _, s := range req.Sources {
		if s["id"] == ontologyagent.EnrichmentSourceID {
			return s
		}
	}
	return nil
}

// enrichedReport is the ingest report of a load that applied one profile,
// which bound its person.
const enrichedReport = `{"mappings":[{"mapping":"customers","source":"src_1","snapshot":"s1","rows":2,` +
	`"create":{"loaded":1,"skipped":0,"empty":0,"unmatched":0,"violations_opened":{"count":0,"examples":[]},"violations_resolved":{"count":0,"examples":[]}},` +
	`"resolve":{"loaded":1,"skipped":0,"empty":0,"unmatched":0,"violations_opened":{"count":0,"examples":[]},"violations_resolved":{"count":0,"examples":[]}}},` +
	`{"mapping":"enrichment_people","source":"foundry_enrichment","snapshot":"p1","rows":2,` +
	`"create":{"loaded":0,"skipped":0,"empty":1,"unmatched":0,"violations_opened":{"count":0,"examples":[]},"violations_resolved":{"count":0,"examples":[]}},` +
	`"resolve":{"loaded":1,"skipped":0,"empty":0,"unmatched":1,"violations_opened":{"count":0,"examples":[]},"violations_resolved":{"count":0,"examples":[]}}}],` +
	`"created":{"sweep":"swept","entities":2,"opened":{"count":0,"examples":[]},"updated":{"count":0,"examples":[]},` +
	`"closed":{"count":0,"examples":[]},"self_loops":{"count":0,"examples":[]},"unchecked":{"count":0,"examples":[]}}}`

func TestPeopleEnrichment(t *testing.T) {
	// A load whose people need looking up parks at provisioning until the
	// step's claim_by, giving its attempt back: nothing is written yet, and
	// the job is not finished.
	t.Run("parks while its people are looked up", func(t *testing.T) {
		h := newHarness(t)
		enrichingVersion(t, h)
		before := testutil.ToFloat64(jobs.WithLabelValues("parked"))
		start := time.Now()
		h.task.process(context.Background(), loadJob())
		if len(h.store.finished) != 0 || len(h.store.enrich.parks) != 1 {
			t.Fatalf("finished %v, parked %v: a parked attempt has no outcome", h.store.finished, h.store.enrich.parks)
		}
		if until := h.store.enrich.parks[0]; until.Before(start.Add(5*time.Minute)) || until.After(time.Now().Add(5*time.Minute)) {
			t.Fatalf("parked until %s, want the step's claim_by, 5 minutes on", until)
		}
		if got := testutil.ToFloat64(jobs.WithLabelValues("parked")); got != before+1 {
			t.Fatalf("parked metric = %v, want %v", got, before+1)
		}
		ingests, _ := h.agent.calls()
		if len(ingests) != 0 || len(h.gateway.tenants) != 0 {
			t.Fatalf("a parked load wrote: %d ingests, %d writes", len(ingests), len(h.gateway.tenants))
		}
		step := h.store.step("job-1")
		if step.Status != model.FoundryEnrichmentQueued || step.Counts.ToLookUp != 1 {
			t.Fatalf("step = %s %+v", step.Status, step.Counts)
		}
		if _, ok := h.bucket.get("ws/ws-1/enrichment/ont-1/loads/job-1/plan.jsonl.gz"); !ok {
			t.Fatal("no plan for the worker")
		}
		// Clues read the tables Ingest will, with the load's id and the proxy's request limit.
		// Sources are read as sent: a Source is written, never read, so its observed has no reading.
		var clues struct {
			ontologyagent.CluesRequest
			Sources []struct {
				ID string `json:"id"`
			} `json:"sources"`
		}
		if err := json.Unmarshal([]byte(h.agent.cluesCalls[0]), &clues); err != nil {
			t.Fatal(err)
		}
		if clues.Load != "job-1" || clues.MaxRequestBytes != 2<<20 || len(clues.Sources) != 1 || !strings.HasPrefix(clues.Sources[0].ID, "src_") {
			t.Fatalf("clues request = %s", h.agent.cluesCalls[0])
		}
	})

	// Clues reads the table files the generation recorded beside its synced
	// tables, as Ingest does: the same sources, each from its read, in its
	// format. A file the version maps that is gone fails the load for good
	// before Clues, as Ingest would.
	withFiles := func(t *testing.T) *harness {
		t.Helper()
		h := newHarness(t)
		enrichingVersion(t, h)
		def, _ := json.Marshal(ontologyagent.Definition{
			Format: "agent_bundle", Status: ontologyagent.StatusDone, OntologyTOML: "version = \"draft\"\n",
			MappingsTOML: "ontology = \"draft\"\n[mappings.orders]\nsource = \"doc-csv\"\n" +
				"[enrichments.people]\ntype = \"Person\"\nclues = { email = \"email\" }\nprops = { current_title = \"current_title\" }\n",
			Draft: json.RawMessage(`{}`), Questions: json.RawMessage(`[]`),
		})
		h.store.version.Definition = def
		rows, original := document.RowsKey("ws-1", "doc-csv"), document.Key("ws-1", "doc-json")
		h.store.generation.Input.DocumentSetIDs = []string{"set-files"}
		h.store.generation.Input.Documents = []model.OntologyGenerationDocument{{ID: "doc-csv", SetID: "set-files"}, {ID: "doc-json", SetID: "set-files"}}
		h.store.scope = []string{"set-files"}
		h.store.sets = []*model.FoundryDocumentSet{{ID: "set-files", WorkspaceID: "ws-1", Name: "Exports"}}
		h.store.files = map[string]*model.FoundryDocument{
			"doc-csv": {ID: "doc-csv", WorkspaceID: "ws-1", SetID: "set-files", Name: "orders.csv", MediaType: model.FoundryMediaCSV,
				Status: model.FoundryDocumentUploaded, Read: &model.FoundryFileRead{Status: model.FoundryFileReadSucceeded, ArtifactKey: &rows}},
			"doc-json": {ID: "doc-json", WorkspaceID: "ws-1", SetID: "set-files", Name: "invoices.json", MediaType: model.FoundryMediaJSON,
				Status: model.FoundryDocumentUploaded, Read: &model.FoundryFileRead{Status: model.FoundryFileReadSucceeded, ArtifactKey: &original}},
		}
		h.bucket.objects[rows] = gz(t, `{"id":1,"email":"ada@acme.io"}`+"\n")
		h.bucket.objects[original] = []byte(`{"data":[{"id":"in_1"}]}`)
		return h
	}
	t.Run("reads the table files Ingest reads", func(t *testing.T) {
		h := withFiles(t)
		h.task.process(context.Background(), loadJob())
		if len(h.store.enrich.parks) != 1 || len(h.agent.cluesCalls) != 1 {
			t.Fatalf("parked %v after %d Clues calls", h.store.enrich.parks, len(h.agent.cluesCalls))
		}
		var clues struct {
			Sources []map[string]any `json:"sources"`
		}
		if err := json.Unmarshal([]byte(h.agent.cluesCalls[0]), &clues); err != nil {
			t.Fatal(err)
		}
		if len(clues.Sources) != 3 || !strings.HasPrefix(fmt.Sprint(clues.Sources[0]["id"]), "src_") ||
			clues.Sources[1]["id"] != "doc-csv" || clues.Sources[2]["id"] != "doc-json" ||
			clues.Sources[2]["access"].(map[string]any)["file"].(map[string]any)["format"] != "json" {
			t.Fatalf("clues sources = %v", clues.Sources)
		}
		// Woken with the profiles chosen, Ingest reads the same tables and files, and the profiles.
		key := "ws/ws-1/enrichment/ont-1/loads/job-1/profiles.jsonl.gz"
		h.bucket.put(key, gz(t, `{"profile_id":"li:ada","clue_email":"ada@acme.io","clue_name":"Ada Lovelace"}`+"\n"))
		step := h.store.enrich.steps["job-1"]
		step.Status, step.ProfilesKey, step.Counts.Profiles = model.FoundryEnrichmentDone, &key, 1
		woken := loadJob()
		woken.Sources = h.store.pins["job-1"]
		if outcome := h.run(t, woken); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		ingests, _ := h.agent.calls()
		var ingest struct {
			Sources []map[string]any `json:"sources"`
		}
		if err := json.Unmarshal([]byte(ingests[0]), &ingest); err != nil {
			t.Fatal(err)
		}
		if len(ingest.Sources) != 4 || ingest.Sources[3]["id"] != ontologyagent.EnrichmentSourceID ||
			fmt.Sprint(ingest.Sources[:3]) != fmt.Sprint(clues.Sources) {
			t.Fatalf("ingest sources = %v, want Clues' %v and the profiles", ingest.Sources, clues.Sources)
		}
	})
	t.Run("fails for good before Clues when a file the version maps is gone", func(t *testing.T) {
		h := withFiles(t)
		h.store.files["doc-csv"].Status = model.FoundryDocumentDeleted
		outcome := h.run(t, loadJob())
		ingests, _ := h.agent.calls()
		if outcome.Status != model.FoundryGraphJobFailed || outcome.RetryAt != nil || len(ingests) != 0 || len(h.agent.cluesCalls) != 0 ||
			!strings.Contains(outcome.Error, `the file "orders.csv" this version maps was deleted or moved out of its document sets`) {
			t.Fatalf("outcome = %+v after %d ingests and %d Clues calls", outcome, len(ingests), len(h.agent.cluesCalls))
		}
		if len(h.store.enrich.parks) != 0 {
			t.Fatalf("parked %v", h.store.enrich.parks)
		}
	})

	// Woken once the worker published, the load applies its profiles: staged
	// as foundry_enrichment at 0.7 in the attempt's own directory, removed
	// once Ingest returns, and what they bound is recorded.
	t.Run("applies the published profiles and removes the staged file", func(t *testing.T) {
		h := newHarness(t)
		enrichingVersion(t, h)
		profile := `{"profile_id":"li:ada","clue_email":"ada@acme.io","clue_name":"Ada Lovelace","current_title":"VP Engineering"}` + "\n"
		object := gz(t, profile)
		h.bucket.put("ws/ws-1/enrichment/ont-1/loads/job-1/profiles.jsonl.gz", object)
		key := "ws/ws-1/enrichment/ont-1/loads/job-1/profiles.jsonl.gz"
		h.store.enrich.steps["job-1"] = &model.FoundryLoadEnrichment{
			LoadID: "job-1", WorkspaceID: "ws-1", OntologyID: "ont-1", JobID: "job-1", Status: model.FoundryEnrichmentDone,
			ProfilesKey: &key, Counts: model.FoundryEnrichmentCounts{People: 1, ToLookUp: 1, Found: 1, Profiles: 1},
		}
		var staged string
		var stagedAt string
		h.agent.ingest = func(req string, stream agentv1.Agent_IngestServer) error {
			if source := profilesSource(t, req); source != nil {
				stagedAt = source["access"].(map[string]any)["file"].(map[string]any)["path"].(string)
				raw, _ := os.ReadFile(filepath.Join(h.workDir, stagedAt))
				staged = string(raw)
			}
			return ingestReporting(enrichedReport)(req, stream)
		}
		outcome := h.run(t, loadJob())
		if outcome.Status != model.FoundryGraphJobSucceeded || len(h.agent.cluesCalls) != 0 {
			t.Fatalf("outcome = %+v after %d Clues calls: an ended step is woken, not prepared again", outcome, len(h.agent.cluesCalls))
		}
		ingests, _ := h.agent.calls()
		source := profilesSource(t, ingests[0])
		sum := sha256.Sum256(object)
		if source == nil || source["route"].(map[string]any)["mapping"].(map[string]any)["trust"] != 0.7 ||
			source["fingerprint"] != hex.EncodeToString(sum[:]) {
			t.Fatalf("profiles source = %v", source)
		}
		if staged != profile || !strings.HasPrefix(stagedAt, filepath.Join("jobs", "job-1")) {
			t.Fatalf("staged %q at %s, want the profiles' bytes in the attempt's directory", staged, stagedAt)
		}
		if _, err := os.Stat(filepath.Join(h.workDir, "jobs", "job-1", "enrichment")); !os.IsNotExist(err) {
			t.Fatalf("the staged profiles outlived the attempt: %v", err)
		}
		if applied := h.store.step("job-1").Applied; applied == nil || applied.Key != key {
			t.Fatalf("applied = %+v", applied)
		}
		if got := h.store.enrich.recorded; len(got) != 2 || got[0] != 1 || got[1] != 1 {
			t.Fatalf("recorded %v, want 1 applied and 1 unmatched", got)
		}
	})

	// A sync landing while the people are looked up changes nothing under
	// the load: Clues pinned the snapshots it read before the job parked, and
	// the job, claimed again with those pins, ingests them with the profiles
	// its step chose, with no second Clues. The sync's rows wait for the
	// next load.
	t.Run("ingests the snapshots Clues read after a sync, with the profiles chosen", func(t *testing.T) {
		h := newHarness(t)
		enrichingVersion(t, h)
		h.task.process(context.Background(), loadJob())
		if len(h.store.enrich.parks) != 1 {
			t.Fatalf("parked %v, want the load waiting for its lookups", h.store.enrich.parks)
		}
		if got := h.store.pins["job-1"]; !reflect.DeepEqual(got, []model.FoundryLoadSource{firstPin}) {
			t.Fatalf("pins = %+v, want the snapshot Clues read", got)
		}
		// sources are a request's sources as it sent them.
		type sources struct {
			Sources []struct {
				ID          string          `json:"id"`
				Fingerprint string          `json:"fingerprint"`
				Observed    json.RawMessage `json:"observed"`
			} `json:"sources"`
		}
		var clues sources
		if err := json.Unmarshal([]byte(h.agent.cluesCalls[0]), &clues); err != nil {
			t.Fatal(err)
		}
		synced := "ws/ws-1/connectors/conn-1/customers/s2.jsonl.gz"
		h.bucket.put(synced, gz(t, `{"id":"cus_1","name":"Acme"}`+"\n"+`{"id":"cus_2","name":"Initech"}`+"\n"))
		h.store.tables[0].S3Key, h.store.tables[0].SyncID, h.store.tables[0].ChangeSeq = synced, "01JDSYNC000000000000000002", 8
		key := "ws/ws-1/enrichment/ont-1/loads/job-1/profiles.jsonl.gz"
		object := gz(t, `{"profile_id":"li:ada","clue_email":"ada@acme.io","clue_name":"Ada Lovelace","current_title":"VP Engineering"}`+"\n")
		h.bucket.put(key, object)
		step := h.store.enrich.steps["job-1"]
		step.Status, step.ProfilesKey, step.Counts.Profiles = model.FoundryEnrichmentDone, &key, 1

		woken := loadJob()
		woken.Sources = h.store.pins["job-1"]
		h.store.pins = nil
		outcome := h.run(t, woken)
		if outcome.Status != model.FoundryGraphJobSucceeded || len(h.agent.cluesCalls) != 1 {
			t.Fatalf("outcome = %+v after %d Clues calls", outcome, len(h.agent.cluesCalls))
		}
		if _, repinned := h.store.pins["job-1"]; repinned {
			t.Fatal("the woken job pinned again")
		}
		ingests, _ := h.agent.calls()
		var req sources
		if err := json.Unmarshal([]byte(ingests[0]), &req); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(object)
		if len(req.Sources) != 2 || req.Sources[0].ID != clues.Sources[0].ID || req.Sources[0].Fingerprint != clues.Sources[0].Fingerprint ||
			req.Sources[1].Fingerprint != hex.EncodeToString(sum[:]) {
			t.Fatalf("ingest sources %+v, want the snapshot Clues read (%+v) and the chosen profiles", req.Sources, clues.Sources)
		}
		// The rows say the sync Clues read them at; the profiles, no sync.
		if string(req.Sources[0].Observed) != firstObserved || string(clues.Sources[0].Observed) != firstObserved || req.Sources[1].Observed != nil {
			t.Fatalf("observed: ingest %s and %s, clues %s; want %s and none", req.Sources[0].Observed, req.Sources[1].Observed,
				clues.Sources[0].Observed, firstObserved)
		}
	})

	// A refresh parked for its lookups is claimed again at attempt 1, having
	// given its attempt back: its wait to be woken is not the queue wait of
	// a refresh, which only its first claim records.
	t.Run("records a parked refresh's queue wait once", func(t *testing.T) {
		h := newHarness(t)
		enrichingVersion(t, h)
		queued := h.task.now().Add(-5 * time.Minute)
		first := loadJob()
		first.Origin, first.CreatedAt = model.FoundryLoadByRefresh, queued
		h.task.process(context.Background(), first)
		if len(h.store.enrich.parks) != 1 {
			t.Fatalf("parked %v, want the refresh waiting for its lookups", h.store.enrich.parks)
		}
		step := h.store.enrich.steps["job-1"]
		step.Status = model.FoundryEnrichmentDone
		woken := loadJob()
		woken.Origin, woken.CreatedAt, woken.Sources = model.FoundryLoadByRefresh, queued, h.store.pins["job-1"]
		if outcome := h.run(t, woken); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if got := h.task.refreshLag.(*observed).values; !slices.Equal(got, []float64{300}) {
			t.Fatalf("refresh lag observations = %v, want [300]", got)
		}
	})

	// With no one to look up, the step ends at once and the load goes on in
	// the same attempt: no park, no wait.
	t.Run("doesn't park with no one to look up", func(t *testing.T) {
		h := newHarness(t)
		enrichingVersion(t, h)
		h.agent.clues = cluesOf(cher)
		outcome := h.run(t, loadJob())
		if outcome.Status != model.FoundryGraphJobSucceeded || len(h.store.enrich.parks) != 0 {
			t.Fatalf("outcome = %+v, parks %v", outcome, h.store.enrich.parks)
		}
		step := h.store.step("job-1")
		if step.Status != model.FoundryEnrichmentDone || *step.Applied != model.FoundryEnrichmentNone || step.Counts.NoName != 1 {
			t.Fatalf("step = %s %+v %+v", step.Status, step.Applied, step.Counts)
		}
		ingests, _ := h.agent.calls()
		if source := profilesSource(t, ingests[0]); source == nil || source["fingerprint"] != model.FoundryEnrichmentNone.SHA256 {
			t.Fatalf("profiles source = %v, want the zero-byte file", source)
		}
	})

	// Lookups that aren't allowed never park the load: it goes on with what
	// earlier loads found (here, nothing), and without consent Clues isn't
	// even called.
	for name, tc := range map[string]struct {
		block  func(h *harness)
		status model.FoundryEnrichmentStatus
		clues  int
	}{
		"the flag off":    {func(h *harness) { h.task.runner.enrichment.Cfg.Enabled = false }, model.FoundryEnrichmentPartial, 1},
		"off Pro":         {func(h *harness) { h.task.runner.enrichment.Pro = proPlans(false) }, model.FoundryEnrichmentPartial, 1},
		"consent off":     {func(h *harness) { h.store.enrich.consent.Enabled = false }, model.FoundryEnrichmentOff, 0},
		"consent not set": {func(h *harness) { h.store.enrich.consent = nil }, model.FoundryEnrichmentOff, 0},
	} {
		t.Run("loads without parking with "+name, func(t *testing.T) {
			h := newHarness(t)
			enrichingVersion(t, h)
			tc.block(h)
			outcome := h.run(t, loadJob())
			if outcome.Status != model.FoundryGraphJobSucceeded || len(h.store.enrich.parks) != 0 || len(h.agent.cluesCalls) != tc.clues {
				t.Fatalf("outcome = %+v, parks %v, %d Clues calls", outcome, h.store.enrich.parks, len(h.agent.cluesCalls))
			}
			if step := h.store.step("job-1"); step.Status != tc.status || *step.Applied != model.FoundryEnrichmentNone {
				t.Fatalf("step = %s %+v", step.Status, step.Applied)
			}
		})
	}

	// What Clues finds wrong with the tables or the mappings is what Ingest
	// would: the attempt fails as Ingest's would, and the step waits for
	// the next attempt.
	t.Run("fails the load on invalid mappings, as Ingest would", func(t *testing.T) {
		h := newHarness(t)
		enrichingVersion(t, h)
		h.agent.clues = func(_ string, stream agentv1.Agent_CluesServer) error {
			return cluesEvents(stream, `{"event":"clues_started","contract_version":"3.9.0"}`,
				`{"event":"invalid_mappings","errors":["mapping contacts: no source crm"]}`)
		}
		outcome := h.run(t, loadJob())
		if outcome.Status != model.FoundryGraphJobFailed || !strings.Contains(outcome.Error, "no source crm") {
			t.Fatalf("outcome = %+v", outcome)
		}
		if ingests, _ := h.agent.calls(); len(ingests) != 0 || h.store.step("job-1").Status != model.FoundryEnrichmentPreparing {
			t.Fatalf("%d ingests, step %s", len(ingests), h.store.step("job-1").Status)
		}
	})

	// A table Clues couldn't read is downloaded again by the retry.
	t.Run("retries an unreadable table and forgets it", func(t *testing.T) {
		h := newHarness(t)
		enrichingVersion(t, h)
		h.agent.clues = func(_ string, stream agentv1.Agent_CluesServer) error {
			return cluesEvents(stream, `{"event":"clues_started","contract_version":"3.9.0"}`, `{"event":"source_unavailable","source":"src_1"}`)
		}
		outcome := h.run(t, loadJob())
		if outcome.RetryAt == nil || outcome.Status != "" {
			t.Fatalf("outcome = %+v", outcome)
		}
		cached, _ := filepath.Glob(filepath.Join(h.workDir, ontologyagent.GraphCache, "*.jsonl"))
		if len(cached) != 0 {
			t.Fatalf("cache kept %v", cached)
		}
	})

	// A call a retry may get through (the sidecar busy or restarting) retries
	// the attempt, as Ingest's does; one the sidecar refuses for good fails
	// only the step, and the load goes on with the profiles found before.
	t.Run("retries a busy sidecar, and loads past a refusal", func(t *testing.T) {
		h := newHarness(t)
		enrichingVersion(t, h)
		h.agent.clues = func(string, agentv1.Agent_CluesServer) error { return status.Error(codes.Unavailable, "shutting down") }
		if outcome := h.run(t, loadJob()); outcome.RetryAt == nil || h.store.step("job-1").Status != model.FoundryEnrichmentPreparing {
			t.Fatalf("outcome = %+v, step %s", outcome, h.store.step("job-1").Status)
		}
		h.agent.clues = func(string, agentv1.Agent_CluesServer) error { return status.Error(codes.InvalidArgument, "refused") }
		outcome := h.run(t, loadJob())
		step := h.store.step("job-1")
		if outcome.Status != model.FoundryGraphJobSucceeded || step.Status != model.FoundryEnrichmentFailed || *step.Applied != model.FoundryEnrichmentNone {
			t.Fatalf("outcome = %+v, step %s %+v", outcome, step.Status, step.Applied)
		}
	})

	// More people than a step may hold (foundry.enrichment.max_people) fail
	// only the step: Clues is read no further, and the load goes on with
	// the profiles found before.
	t.Run("loads past more people than its step holds", func(t *testing.T) {
		h := newHarness(t)
		enrichingVersion(t, h)
		h.task.runner.enrichment.Cfg.MaxPeople = 1
		h.agent.clues = cluesOf(ada, cher)
		outcome := h.run(t, loadJob())
		step := h.store.step("job-1")
		if outcome.Status != model.FoundryGraphJobSucceeded || len(h.store.enrich.parks) != 0 ||
			step.Status != model.FoundryEnrichmentFailed || *step.Applied != model.FoundryEnrichmentNone {
			t.Fatalf("outcome = %+v, parks %v, step %s %+v", outcome, h.store.enrich.parks, step.Status, step.Applied)
		}
	})

	// Profiles that can't be staged (their object changed since the load
	// chose it) are retried; the staged file never outlives the attempt.
	t.Run("retries profiles it can't stage, leaving nothing behind", func(t *testing.T) {
		h := newHarness(t)
		enrichingVersion(t, h)
		key := "ws/ws-1/enrichment/ont-1/loads/job-1/profiles.jsonl.gz"
		h.bucket.put(key, gz(t, "{}\n"))
		h.store.enrich.steps["job-1"] = &model.FoundryLoadEnrichment{
			LoadID: "job-1", WorkspaceID: "ws-1", OntologyID: "ont-1", JobID: "job-1", Status: model.FoundryEnrichmentDone,
			Applied: &model.FoundryEnrichmentApplied{Key: key, SHA256: strings.Repeat("0", 64)},
		}
		outcome := h.run(t, loadJob())
		if outcome.RetryAt == nil || !strings.Contains(outcome.Error, "People enrichment's profiles") {
			t.Fatalf("outcome = %+v", outcome)
		}
		if ingests, _ := h.agent.calls(); len(ingests) != 0 {
			t.Fatal("ingested without the load's profiles")
		}
		if _, err := os.Stat(filepath.Join(h.workDir, "jobs", "job-1", "enrichment")); !os.IsNotExist(err) {
			t.Fatalf("a half-staged file outlived the attempt: %v", err)
		}
	})

	// After a rollback of the sidecar, a parked job meets another contract:
	// it fails for good before its step is touched or anything is written.
	t.Run("fails a parked load cleanly on another contract", func(t *testing.T) {
		h := newHarness(t)
		enrichingVersion(t, h)
		claimBy := time.Now().Add(time.Minute)
		h.store.enrich.steps["job-1"] = &model.FoundryLoadEnrichment{
			LoadID: "job-1", WorkspaceID: "ws-1", OntologyID: "ont-1", JobID: "job-1", Status: model.FoundryEnrichmentQueued, ClaimBy: &claimBy,
		}
		h.agent.info = &agentv1.GetInfoResponse{ContractVersion: "3.8.0"}
		outcome := h.run(t, loadJob())
		if outcome.Status != model.FoundryGraphJobFailed || !strings.Contains(outcome.Error, "contract 3.8.0") {
			t.Fatalf("outcome = %+v", outcome)
		}
		if ingests, _ := h.agent.calls(); len(ingests) != 0 || h.store.enrich.starts != 0 || len(h.store.enrich.parks) != 0 {
			t.Fatalf("%d ingests, %d step reads, parks %v", len(ingests), h.store.enrich.starts, h.store.enrich.parks)
		}
	})

	// A version without an item loads as it always has: no step, no source.
	t.Run("leaves a version without enrichment alone", func(t *testing.T) {
		h := newHarness(t)
		outcome := h.run(t, loadJob())
		ingests, _ := h.agent.calls()
		if outcome.Status != model.FoundryGraphJobSucceeded || h.store.enrich.starts != 0 || profilesSource(t, ingests[0]) != nil {
			t.Fatalf("outcome = %+v, %d steps started", outcome, h.store.enrich.starts)
		}
	})
}

// The enrichment package's Load is what the runner calls: the store must be
// the job store it reads and writes.
var _ enrichment.JobStore = (*memStore)(nil)
