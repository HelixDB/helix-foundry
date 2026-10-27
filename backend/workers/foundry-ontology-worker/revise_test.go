package foundryontologyworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

type revisionFixture struct {
	store.Unimplemented
	parent *model.OntologyVersion
	// loaded is the current load's version.
	loaded   *model.OntologyVersion
	ontology *model.FoundryOntology
	load     *model.FoundryLoad
	// workspaceDeleted is the workspace's deletion, begun.
	workspaceDeleted bool
	request          *ontologyagent.ReviseRequest
	reply            json.RawMessage
	err              error
	infoErr          error
	version          string
	// evalErr is openEvaluation's error; evals counts evaluations opened.
	evalErr  error
	evals    int
	releases int
	block    bool
}

type evaluationFixture struct {
	*revisionFixture
	paused     bool
	acquired   int
	released   int
	boost      model.FoundryLoadBoost
	acquireErr error
}

func (f *evaluationFixture) Get(context.Context, string) (ports.Database, error) {
	return ports.Database{TenantID: "tenant", WorkspaceID: "ws"}, nil
}
func (f *evaluationFixture) Live(ctx context.Context, id string) (ports.Database, error) {
	return f.Get(ctx, id)
}
func (f *evaluationFixture) Endpoint(_ context.Context, _, path string) (string, error) {
	if f.paused {
		return "", status.Error(codes.FailedPrecondition, "the database's queries are paused")
	}
	return "https://gateway.cluster.example" + path, nil
}
func (f *evaluationFixture) AcquireLoadBoost(_ context.Context, _ string, b model.FoundryLoadBoost) (ports.BoostChange, error) {
	f.acquired++
	f.boost = b
	return ports.BoostChange{ClusterID: "cluster", Revision: 1}, f.acquireErr
}
func (f *evaluationFixture) ReleaseLoadBoost(_ context.Context, b model.FoundryLoadBoost) error {
	f.released++
	if b != f.boost {
		return errors.New("wrong release")
	}
	return nil
}
func (f *evaluationFixture) SweepLoadBoosts(context.Context, int) (int, error) { return 0, nil }
func (f *evaluationFixture) SyncedRevision(context.Context, string) (uint64, error) {
	return 1, nil
}

func TestRevisionEvaluationProxyAndBoost(t *testing.T) {
	for _, mode := range []string{"enabled", "disabled", "staff", "lost lease", "paused"} {
		t.Run(mode, func(t *testing.T) {
			base, _, q := newRevisionFixture(t)
			f := &evaluationFixture{revisionFixture: base}
			base.ontology.TenantID = "tenant"
			cfg := &config.Config{}
			cfg.Foundry.Graph.HelixTimeoutSecs = 60
			cfg.Foundry.Ontology.Timeout = time.Minute
			if mode != "disabled" {
				cfg.Foundry.Graph.LoadRateLimit = config.GraphLoadRateLimit{RequestsPerSecond: 20, Burst: 40}
			}
			if mode == "staff" {
				f.acquireErr = store.ErrConflict
			}
			if mode == "lost lease" {
				f.acquireErr = store.ErrNotFound
			}
			f.paused = mode == "paused"
			// The pod's read-only proxy, over a gateway that counts what reaches it.
			forwarded := 0
			proxy, err := graphproxy.ServeReadOnly(queryFunc(func(context.Context, string, string, []byte, http.Header) (helixgateway.Response, error) {
				forwarded++
				return helixgateway.Response{StatusCode: http.StatusOK}, nil
			}), 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = proxy.Close() })
			r := newReviser(f, f, f, cfg, "worker-1", f, proxy)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			evaluation, release, err := r.openEvaluation(ctx, q.pending[0], f.load)
			if mode == "lost lease" {
				if err == nil || release != nil {
					t.Fatal("lost lease served proxy")
				}
				return
			}
			// Bind's status, and its reason, reach run (TestReviseWorker "queries paused").
			if mode == "paused" {
				if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "paused") || release != nil || f.acquired != 0 {
					t.Fatalf("a paused database: %v, boosts %d", err, f.acquired)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if evaluation.Database != "tenant" || evaluation.Load != "load" || evaluation.OntologyVersion != "foundry-v7" || evaluation.Helix.Token == "" {
				t.Fatalf("%+v", evaluation)
			}
			for _, tc := range []struct {
				body    string
				durable bool
			}{{`{"request_type":"write"}`, false}, {`{"request_type":"read"}`, true}} {
				req, err := http.NewRequest(http.MethodPost, evaluation.Helix.URL+"/v2/query", strings.NewReader(tc.body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+evaluation.Helix.Token)
				if tc.durable {
					req.Header.Set("X-Helix-Await-Durable", "true")
				}
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode < 400 || forwarded != 0 {
					t.Fatalf("forwarded evaluation write: %d, %d forwarded", response.StatusCode, forwarded)
				}
			}
			cancel()
			release()
			if mode == "enabled" && (f.acquired != 1 || f.released != 1 || f.boost.Holder != model.FoundryBoostRevision || f.boost.JobID != "revise" ||
				f.boost.Attempt != 1 || f.boost.TTL <= time.Minute) {
				t.Fatalf("boost lifecycle %+v", f)
			}
			if mode == "disabled" && (f.acquired != 0 || f.released != 0) {
				t.Fatal("disabled boost acquired")
			}
			if mode == "staff" && f.released != 0 {
				t.Fatal("released staff override")
			}
		})
	}
}

func (f *revisionFixture) WorkspaceDeleting(context.Context, string) (bool, error) {
	return f.workspaceDeleted, nil
}
func (f *revisionFixture) GetFoundryOntology(context.Context, string, string) (*model.FoundryOntology, error) {
	return f.ontology, nil
}
func (f *revisionFixture) GetOntologyVersion(_ context.Context, _, id string) (*model.OntologyVersion, error) {
	for _, v := range []*model.OntologyVersion{f.parent, f.loaded} {
		if v != nil && v.ID == id {
			return v, nil
		}
	}
	return nil, store.ErrNotFound
}
func (f *revisionFixture) GetFoundryLoad(context.Context, string, string) (*model.FoundryLoad, error) {
	if f.load == nil {
		return nil, store.ErrNotFound
	}
	return f.load, nil
}
func (f *revisionFixture) Info(context.Context) (*agentv1.GetInfoResponse, error) {
	return &agentv1.GetInfoResponse{ContractVersion: f.version}, f.infoErr
}
func (f *revisionFixture) Revise(ctx context.Context, raw []byte) (json.RawMessage, error) {
	if f.request != nil {
		panic("duplicate call")
	}
	f.request = &ontologyagent.ReviseRequest{}
	if err := json.Unmarshal(raw, f.request); err != nil {
		return nil, err
	}
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.reply, f.err
}
func (f *revisionFixture) evaluation(_ context.Context, _ *model.OntologyGeneration, load *model.FoundryLoad) (ontologyagent.ReviseEval, func(), error) {
	if f.evalErr != nil {
		return ontologyagent.ReviseEval{}, nil, f.evalErr
	}
	f.evals++
	return ontologyagent.ReviseEval{Database: "tenant", Load: load.ID, OntologyVersion: load.OntologyVersion(), Helix: ontologyagent.HelixTarget{URL: "http://127.0.0.1", Token: "attempt"}}, func() { f.releases++ }, nil
}
func newRevisionFixture(t *testing.T) (*revisionFixture, *Task, *queue) {
	t.Helper()
	current := "load"
	raw, _ := json.Marshal(ontologyagent.Definition{Format: "agent_bundle", Status: ontologyagent.StatusDone, Rules: &ontologyagent.RuleCounts{Count: 9, Evaluated: 9}, OntologyTOML: "old ontology", MappingsTOML: "old mappings", Draft: json.RawMessage(`{"draft":{"items":{"rule.old":{}}}}`), Questions: json.RawMessage(`[]`)})
	d := ontologyagent.ReviseDefinition{OntologyTOML: "new ontology", MappingsTOML: "new mappings", DraftJSON: `{"draft":{"items":{"constraint.new":{"verdict":"warn","card":{"rev":1,"sample":"No comparable load; no entities measured.","instances":0,"confirmed":0,"added":0,"contradicted":0,"unmeasurable":false,"precision":null,"failure_rate_bound":null,"violations":[],"subjects":0,"violation_rate":null,"review_facts":0,"unrepresentable":0,"counterexamples":[],"reason":"No comparable load; capped at warn.","unmeasured":"load"}}}}}`, QuestionsJSON: `[]`}
	reply, _ := json.Marshal(ontologyagent.ReviseResult{Status: ontologyagent.ReviseRevised, Definition: &d, Rules: &ontologyagent.RuleCounts{Count: 2, Evaluated: 2}, Findings: []ontologyagent.ReviseFinding{{Code: ontologyagent.ReviseNoComparableLoad, Message: "No comparable load"}}, Cards: []ontologyagent.ReviseCard{{Item: "constraint.new", Verdict: ontologyagent.ReviseWarn, Card: json.RawMessage(`{"rev":1,"sample":"No comparable load; no entities measured.","instances":0,"confirmed":0,"added":0,"contradicted":0,"unmeasurable":false,"precision":null,"failure_rate_bound":null,"violations":[],"subjects":0,"violation_rate":null,"review_facts":0,"unrepresentable":0,"counterexamples":[],"reason":"No comparable load; capped at warn.","unmeasured":"load"}`)}}})
	loaded, _ := json.Marshal(ontologyagent.Definition{Format: "agent_bundle", Status: ontologyagent.StatusDone, OntologyTOML: "loaded ontology", MappingsTOML: "loaded mappings"})
	f := &revisionFixture{parent: &model.OntologyVersion{ID: "parent", Format: model.OntologyFormatAgentBundle, Definition: raw},
		loaded:   &model.OntologyVersion{ID: "loaded", Version: 7, Format: model.OntologyFormatAgentBundle, Definition: loaded},
		ontology: &model.FoundryOntology{ID: "o", WorkspaceID: "ws", CurrentLoadID: &current}, load: &model.FoundryLoad{ID: current, VersionID: "loaded", Version: 7, Status: model.FoundryLoadReady},
		version: ontologyagent.ContractVersion, reply: reply}
	user := "user"
	q := newQueue(&model.OntologyGeneration{ID: "revise", WorkspaceID: "ws", OntologyID: "o", Attempts: 1, RequestedByUserID: &user, Input: model.OntologyGenerationInput{Revise: &model.OntologyRevision{ParentVersionID: "parent", Edits: model.OntologyEdits{model.OntologyRemove{ItemID: "rule.old"}}}}})
	task := newTask(q, nil)
	task.reviser = &reviser{db: f, agent: f, openEvaluation: f.evaluation}
	return f, task, q
}

// A revised version reads its parent's sets (the revision's input) and the
// sets its revised routes name; mappings that can't be read add none.
func TestRevisionScope(t *testing.T) {
	for _, tc := range []struct {
		name, mappings, want string
	}{
		{"adds the sets the revised routes name", "[extractions.docs]\ndocuments = [\"set-new\", \"set-parent\"]\n", "set-parent,set-new,set-parent"},
		{"keeps the parent's sets when the routes can't be read", "new mappings", "set-parent"},
		{"adds nothing for every document", "[extractions.docs]\ndocuments = \"all\"\n", "set-parent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, task, q := newRevisionFixture(t)
			var reply ontologyagent.ReviseResult
			if err := json.Unmarshal(f.reply, &reply); err != nil {
				t.Fatal(err)
			}
			reply.Definition.MappingsTOML = tc.mappings
			f.reply, _ = json.Marshal(reply)
			q.pending[0].Input.DocumentSetIDs = []string{"set-parent"}
			task.process(context.Background(), q.pending[0])
			v := q.outcomes["revise"].Result
			if v == nil || v.DocumentSetIDs == nil || strings.Join(*v.DocumentSetIDs, ",") != tc.want {
				t.Fatalf("version = %+v", v)
			}
		})
	}
}

func TestReviseWorker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*revisionFixture, *Task, *queue)
		failed  bool
		version bool
		eval    bool
		called  bool
		retry   bool
	}{
		{name: "version with parent", version: true, eval: true, called: true},
		{name: "preview", change: func(_ *revisionFixture, _ *Task, q *queue) { q.pending[0].Input.Revise.DryRun = true }, eval: true, called: true},
		{name: "refusal", change: func(f *revisionFixture, _ *Task, _ *queue) {
			f.reply = json.RawMessage(`{"status":"refused","findings":[{"code":"EditNotAllowed","item_id":"type.Customer","message":"Not editable"}],"cards":[]}`)
		}, failed: true, eval: true, called: true},
		{name: "no current load", change: func(f *revisionFixture, _ *Task, _ *queue) { f.ontology.CurrentLoadID = nil }, version: true, called: true},
		{name: "deleting load", change: func(f *revisionFixture, _ *Task, _ *queue) { f.load.Status = model.FoundryLoadDeleting }, version: true, called: true},
		{name: "partial load", change: func(f *revisionFixture, _ *Task, _ *queue) { f.load.Status = model.FoundryLoadPartial }, version: true, called: true, eval: true},
		{name: "deleted load", change: func(f *revisionFixture, _ *Task, _ *queue) { f.load = nil }, version: true, called: true},
		{name: "agent unavailable", change: func(f *revisionFixture, _ *Task, _ *queue) { f.infoErr = errors.New("unavailable") }, retry: true},
		{name: "deleted workspace", change: func(f *revisionFixture, _ *Task, _ *queue) { f.workspaceDeleted = true }, failed: true},
		{name: "deleted ontology", change: func(f *revisionFixture, _ *Task, _ *queue) { f.ontology.DeletedAt = &now }, failed: true},
		{name: "invalid stored edit", change: func(_ *revisionFixture, _ *Task, q *queue) { q.pending[0].Input.Revise.Edits[0] = nil }, eval: true, failed: true},
		{name: "oversized result", change: func(f *revisionFixture, _ *Task, _ *queue) { f.reply = json.RawMessage(strings.Repeat("x", (3<<20)+1)) }, eval: true, called: true, failed: true},
		{name: "wrong contract", change: func(f *revisionFixture, _ *Task, _ *queue) { f.version = "other" }, failed: true},
		{name: "unavailable", change: func(f *revisionFixture, _ *Task, _ *queue) { f.err = errors.New("unavailable") }, called: true, eval: true, retry: true},
		// A graph error a retry may clear, shutdown and capacity are no result: retried, never stored as a refusal.
		{name: "graph unavailable", change: func(f *revisionFixture, _ *Task, _ *queue) {
			f.err = status.Error(codes.Unavailable, "a read timed out")
		}, called: true, eval: true, retry: true},
		{name: "at capacity", change: func(f *revisionFixture, _ *Task, _ *queue) {
			f.err = status.Error(codes.ResourceExhausted, "two revisions")
		}, called: true, eval: true, retry: true},
		{name: "over budget", change: func(f *revisionFixture, _ *Task, _ *queue) {
			f.reply = json.RawMessage(`{"status":"refused","findings":[{"code":"budget","item_id":null,"message":"Measuring needs more than its budget."}],"cards":[]}`)
		}, failed: true, eval: true, called: true},
		// A database deleted for good can't be measured: the revision goes on
		// with no comparable load. Paused queries may lift: retried, saying so.
		{name: "database deleted", change: func(f *revisionFixture, _ *Task, _ *queue) {
			f.evalErr = status.Error(codes.NotFound, "the ontology's database was deleted")
		}, version: true, called: true},
		{name: "queries paused", change: func(f *revisionFixture, _ *Task, _ *queue) {
			f.evalErr = status.Error(codes.FailedPrecondition, "queries for this database are paused; resume them to query it")
		}, retry: true},
		{name: "database unreachable", change: func(f *revisionFixture, _ *Task, _ *queue) {
			f.evalErr = status.Error(codes.Unavailable, "get tenant: timeout")
		}, retry: true},
		{name: "loaded version not a draft", change: func(f *revisionFixture, _ *Task, _ *queue) { f.loaded.Format = model.OntologyFormatFoundryV1 }, failed: true},
		{name: "loaded version unreadable", change: func(f *revisionFixture, _ *Task, _ *queue) { f.loaded = nil }, retry: true},
		{name: "unimplemented", change: func(f *revisionFixture, _ *Task, _ *queue) { f.err = status.Error(codes.Unimplemented, "no revise") }, failed: true, called: true, eval: true},
		{name: "malformed", change: func(f *revisionFixture, _ *Task, _ *queue) { f.reply = json.RawMessage(`{"status":"revised"}`) }, failed: true, called: true, eval: true},
		{name: "missing parent", change: func(f *revisionFixture, _ *Task, _ *queue) { f.parent = nil }, failed: true},
		{name: "unfinished parent", change: func(f *revisionFixture, _ *Task, _ *queue) {
			f.parent.Definition = json.RawMessage(`{"status":"needs_input"}`)
		}, failed: true},
		{name: "no agent configured", change: func(_ *revisionFixture, t *Task, _ *queue) { t.reviser = nil }, failed: true},
		{name: "cancel", change: func(f *revisionFixture, t *Task, _ *queue) { f.block = true; t.cfg.Timeout = 15 * time.Millisecond }, called: true, eval: true, retry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, task, q := newRevisionFixture(t)
			if tc.change != nil {
				tc.change(f, task, q)
			}
			// Process only one attempt: retry scheduling belongs to the queue.
			gen := q.pending[0]
			task.process(context.Background(), gen)
			got := q.outcomes["revise"]
			if (got.Status == model.OntologyGenerationFailed) != tc.failed || (got.RetryAt != nil) != tc.retry || (got.Result != nil) != tc.version {
				t.Fatalf("outcome %+v", got)
			}
			if (f.evals == 1) != tc.eval || f.releases != f.evals || (f.request != nil) != tc.called {
				t.Fatalf("evals %d releases %d called %v", f.evals, f.releases, f.request != nil)
			}
			// The eval carries the load version's files as stored, never the
			// draft's: the engine reads what People enrichment filled on the
			// load from that version's mappings.
			if f.request != nil && f.request.Eval != nil && (f.request.Eval.Load != "load" || f.request.Eval.OntologyVersion != "foundry-v7" ||
				f.request.Eval.OntologyTOML != "loaded ontology" || f.request.Eval.MappingsTOML != "loaded mappings") {
				t.Fatalf("eval is not the current load's, with its version's ontology and mappings: %+v", f.request.Eval)
			}
			if f.request != nil && (f.request.Definition.OntologyTOML != "old ontology" || f.request.Definition.MappingsTOML != "old mappings") {
				t.Fatalf("definition is not the parent draft's: %+v", f.request.Definition)
			}
			if tc.retry && tc.called && strings.HasPrefix(tc.name, "graph") && got.Error != "the ontology agent could not finish the revision; Helix will retry" {
				t.Fatalf("retry error %q", got.Error)
			}
			if tc.name == "queries paused" && got.Error != "queries for this database are paused; resume them to query it" {
				t.Fatalf("the reason was lost: %q", got.Error)
			}
			if tc.name == "database deleted" && f.request.Eval != nil {
				t.Fatalf("measured a deleted database: %+v", f.request.Eval)
			}
			if tc.version {
				v := got.Result
				var def ontologyagent.Definition
				if err := json.Unmarshal(v.Definition, &def); err != nil {
					t.Fatal(err)
				}
				var draft struct {
					Draft struct {
						Items map[string]struct {
							Verdict string          `json:"verdict"`
							Card    json.RawMessage `json:"card"`
						} `json:"items"`
					} `json:"draft"`
				}
				if err := json.Unmarshal(def.Draft, &draft); err != nil {
					t.Fatal(err)
				}
				item := draft.Draft.Items["constraint.new"]
				var report ontologyagent.ReviseResult
				if err := json.Unmarshal(got.ReviseResult, &report); err != nil {
					t.Fatal(err)
				}
				if item.Verdict != "warn" || string(item.Card) != string(report.Cards[0].Card) {
					t.Fatal("stored draft lost its verdict or nested card")
				}
				if def.Rules == nil || def.Rules.Count != 2 || def.Rules.Evaluated != 2 || def.ParentVersionID != "parent" || def.OntologyTOML != "new ontology" || v.Source != model.OntologySourceManual || v.GenerationID == nil || *v.GenerationID != "revise" || *v.CreatedByUserID != "user" {
					t.Fatalf("bad version %+v / %+v", v, def)
				}
			}
			if len(got.ReviseResult) > 0 {
				var report map[string]json.RawMessage
				if err := json.Unmarshal(got.ReviseResult, &report); err != nil {
					t.Fatal(err)
				}
				if report["definition"] != nil || report["findings"] == nil || report["cards"] == nil {
					t.Fatalf("bad report %s", got.ReviseResult)
				}
			}
			if tc.name == "refusal" && string(got.ReviseResult) != `{"status":"refused","findings":[{"code":"EditNotAllowed","item_id":"type.Customer","message":"Not editable"}],"cards":[]}` {
				t.Fatalf("refusal lost: %s", got.ReviseResult)
			}
		})
	}
}

type queryFunc func(context.Context, string, string, []byte, http.Header) (helixgateway.Response, error)

func (f queryFunc) Query(c context.Context, e, t string, b []byte, h http.Header) (helixgateway.Response, error) {
	return f(c, e, t, b, h)
}
func TestRevisionRefusesUnevaluatedRules(t *testing.T) {
	for _, preview := range []bool{false, true} {
		t.Run(fmt.Sprint(preview), func(t *testing.T) {
			f, task, q := newRevisionFixture(t)
			var reply ontologyagent.ReviseResult
			if err := json.Unmarshal(f.reply, &reply); err != nil {
				t.Fatal(err)
			}
			reply.Rules = &ontologyagent.RuleCounts{Count: 2, Evaluated: 1}
			f.reply, _ = json.Marshal(reply)
			q.pending[0].Input.Revise.DryRun = preview
			task.process(context.Background(), q.pending[0])
			got := q.outcomes["revise"]
			if got.Result != nil || got.Status != model.OntologyGenerationFailed {
				t.Fatalf("accepted incomplete evaluation: %+v", got)
			}
		})
	}
}
