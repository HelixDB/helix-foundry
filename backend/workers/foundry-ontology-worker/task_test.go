package foundryontologyworker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologygen"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sample"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// queue hands out its generations in order and records what each attempt ended with.
type queue struct {
	mu        sync.Mutex
	pending   []*model.OntologyGeneration
	outcomes  map[string]model.OntologyGenerationOutcome
	renewals  int
	loseLease bool
}

func (q *queue) ClaimOntologyGeneration(context.Context, string, time.Duration) (*model.OntologyGeneration, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil, nil
	}
	next := q.pending[0]
	q.pending = q.pending[1:]
	return next, nil
}

func (q *queue) RenewOntologyGenerationLease(context.Context, string, string, time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.renewals++
	if q.loseLease {
		return store.ErrNotFound
	}
	return nil
}

func (q *queue) FinishOntologyGeneration(_ context.Context, id, _ string, outcome model.OntologyGenerationOutcome) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.outcomes[id] = outcome
	return nil
}

type generatorFunc func(ctx context.Context, gen *model.OntologyGeneration) (ontologygen.Result, error)

func (f generatorFunc) Run(ctx context.Context, gen *model.OntologyGeneration) (ontologygen.Result, error) {
	return f(ctx, gen)
}

var now = time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)

func newTask(q *queue, g generatorFunc) *Task {
	return &Task{
		cfg:   config.OntologyConfig{Lease: 30 * time.Millisecond, Timeout: time.Minute, MaxAttempts: 3},
		queue: q, generator: g, workerID: "worker-1", now: func() time.Time { return now },
	}
}

func newQueue(gens ...*model.OntologyGeneration) *queue {
	return &queue{pending: gens, outcomes: map[string]model.OntologyGenerationOutcome{}}
}

func spent(err error) generatorFunc {
	return func(context.Context, *model.OntologyGeneration) (ontologygen.Result, error) {
		return ontologygen.Result{Format: model.OntologyFormatFoundryV1, Definition: []byte(`{"entities":[]}`), Model: "claude-opus-5-5",
			InputTokens: 900, OutputTokens: 90}, err
	}
}

func TestFoundryOntologyWorker(t *testing.T) {
	// The draft is a generated version credited to whoever asked for it.
	t.Run("stores the draft of a successful generation", func(t *testing.T) {
		user := "user-1"
		q := newQueue(&model.OntologyGeneration{ID: "g1", WorkspaceID: "ws-1", OntologyID: "ont-1", Attempts: 1, RequestedByUserID: &user})
		newTask(q, spent(nil)).drain(context.Background())
		got := q.outcomes["g1"]
		v := got.Result
		if got.Status != model.OntologyGenerationSucceeded || v == nil || v.Source != model.OntologySourceGenerated ||
			v.Format != model.OntologyFormatFoundryV1 ||
			*v.GenerationID != "g1" || v.WorkspaceID != "ws-1" || v.OntologyID != "ont-1" || *v.CreatedByUserID != "user-1" || string(v.Definition) != `{"entities":[]}` {
			t.Fatalf("outcome = %+v, version = %+v", got, v)
		}
		if got.Model != "claude-opus-5-5" || got.InputTokens != 900 || got.OutputTokens != 90 {
			t.Fatalf("usage = %+v", got)
		}
	})
	// A version reads the sets its generation read: none when it read none,
	// which is a scope, not a legacy version's NULL.
	t.Run("stores the generation's document sets as the version's scope", func(t *testing.T) {
		q := newQueue(
			&model.OntologyGeneration{ID: "g1", WorkspaceID: "ws-1", OntologyID: "ont-1", Attempts: 1,
				Input: model.OntologyGenerationInput{DocumentSetIDs: []string{"set-a", "set-b"}}},
			&model.OntologyGeneration{ID: "g2", WorkspaceID: "ws-1", OntologyID: "ont-1", Attempts: 1})
		newTask(q, spent(nil)).drain(context.Background())
		if v := q.outcomes["g1"].Result; v == nil || v.DocumentSetIDs == nil || strings.Join(*v.DocumentSetIDs, ",") != "set-a,set-b" {
			t.Fatalf("version = %+v", v)
		}
		if v := q.outcomes["g2"].Result; v == nil || v.DocumentSetIDs == nil || len(*v.DocumentSetIDs) != 0 {
			t.Fatalf("version = %+v", v)
		}
	})
	// An agent generation's draft keeps the format its generator wrote.
	t.Run("stores an agent draft as an agent bundle", func(t *testing.T) {
		q := newQueue(&model.OntologyGeneration{ID: "g1", WorkspaceID: "ws-1", Attempts: 1})
		newTask(q, func(context.Context, *model.OntologyGeneration) (ontologygen.Result, error) {
			return ontologygen.Result{Format: model.OntologyFormatAgentBundle, Definition: []byte(`{"format":"agent_bundle"}`)}, nil
		}).drain(context.Background())
		if v := q.outcomes["g1"].Result; v == nil || v.Format != model.OntologyFormatAgentBundle || string(v.Definition) != `{"format":"agent_bundle"}` {
			t.Fatalf("version = %+v", v)
		}
	})
	t.Run("retries a transient failure with backoff and keeps its tokens", func(t *testing.T) {
		q := newQueue(&model.OntologyGeneration{ID: "g1", Attempts: 2})
		newTask(q, spent(&ontologygen.Failure{Message: "Claude is unavailable; Helix will retry"})).drain(context.Background())
		got := q.outcomes["g1"]
		if got.RetryAt == nil || !got.RetryAt.Equal(now.Add(2*time.Minute)) || got.Result != nil || got.InputTokens != 900 {
			t.Fatalf("outcome = %+v", got)
		}
	})
	t.Run("fails a permanent failure at once", func(t *testing.T) {
		q := newQueue(&model.OntologyGeneration{ID: "g1", Attempts: 1})
		newTask(q, spent(&ontologygen.Failure{Message: "the generated ontology is not valid", Permanent: true})).drain(context.Background())
		got := q.outcomes["g1"]
		if got.Status != model.OntologyGenerationFailed || got.RetryAt != nil || got.Error != "the generated ontology is not valid" {
			t.Fatalf("outcome = %+v", got)
		}
	})
	t.Run("gives up after the last attempt", func(t *testing.T) {
		q := newQueue(&model.OntologyGeneration{ID: "g1", Attempts: 3})
		newTask(q, spent(errors.New("boom"))).drain(context.Background())
		got := q.outcomes["g1"]
		if got.Status != model.OntologyGenerationFailed || !strings.Contains(got.Error, "gave up after 3 attempts") {
			t.Fatalf("outcome = %+v", got)
		}
	})
	// Another worker owns a generation whose lease was lost, so this attempt must stop.
	t.Run("cancels the attempt when the lease is lost", func(t *testing.T) {
		q := newQueue(&model.OntologyGeneration{ID: "g1", Attempts: 1})
		q.loseLease = true
		newTask(q, func(ctx context.Context, _ *model.OntologyGeneration) (ontologygen.Result, error) {
			<-ctx.Done()
			return ontologygen.Result{}, ctx.Err()
		}).drain(context.Background())
		if q.renewals == 0 {
			t.Fatal("lease was never renewed")
		}
	})
}

// noDatabases stands in for the host's databases where none is read.
type noDatabases struct{ ports.Databases }

// The worker drafts with the agent, on its settings, and resolves each
// workspace's AI setting.
// sampleGeneration asks for the sample from connector conn-k's tables and
// its documents' set.
func sampleGeneration(tables ...string) *model.OntologyGeneration {
	gen := &model.OntologyGeneration{ID: "g1", WorkspaceID: "ws-1", OntologyID: "ont-1", Attempts: 1,
		Input: model.OntologyGenerationInput{Sample: sample.Acme, DocumentSetIDs: []string{"set-k"}}}
	for _, name := range tables {
		gen.Input.Tables = append(gen.Input.Tables, model.OntologyGenerationTable{ConnectorID: "conn-k", Table: name, SyncID: "sync-1"})
	}
	return gen
}

func TestFoundryOntologyWorkerSample(t *testing.T) {
	never := func(context.Context, *model.OntologyGeneration) (ontologygen.Result, error) {
		t.Fatal("the sample called the agent")
		return ontologygen.Result{}, nil
	}
	// The sample's draft is stored at once, naming the connector's tables.
	t.Run("stores the sample's draft for its tables, with no model", func(t *testing.T) {
		q := newQueue(sampleGeneration(sample.Tables...))
		newTask(q, never).drain(context.Background())
		got := q.outcomes["g1"]
		v := got.Result
		if got.Status != model.OntologyGenerationSucceeded || v == nil || v.Format != model.OntologyFormatAgentBundle ||
			v.Source != model.OntologySourceGenerated || strings.Join(*v.DocumentSetIDs, ",") != "set-k" {
			t.Fatalf("outcome = %+v, version = %+v", got, v)
		}
		accounts := ontologyagent.TableSourceID("conn-k", "accounts")
		if !strings.Contains(string(v.Definition), `source = \"`+accounts+`\"`) || strings.Contains(string(v.Definition), "{{") {
			t.Fatalf("definition does not name the connector's tables")
		}
		if got.Model != "" || got.InputTokens != 0 || got.OutputTokens != 0 {
			t.Fatalf("usage = %+v", got)
		}
	})
	// A table the generation didn't record fails the sample for good.
	t.Run("fails a sample missing a table, for good", func(t *testing.T) {
		q := newQueue(sampleGeneration("accounts", "contacts"))
		newTask(q, never).drain(context.Background())
		got := q.outcomes["g1"]
		if got.Status != model.OntologyGenerationFailed || got.RetryAt != nil || got.Error != "the sample's database is missing a table: add the sample again" {
			t.Fatalf("outcome = %+v", got)
		}
	})
}

func TestFoundryOntologyWorkerGenerator(t *testing.T) {
	cfg := config.Defaults()
	cfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: "sk-test", Models: config.AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
	}}
	task, err := New(Deps{Cfg: &cfg, DB: store.Unimplemented{}, Databases: noDatabases{}})
	if err != nil {
		t.Fatal(err)
	}
	gen, ok := task.generator.(*ontologyagent.Generator)
	if !ok || gen.AI == nil || gen.WorkDir != "/work" || gen.MaxWorkBytes != cfg.Foundry.Ontology.Agent.MaxWorkBytes || gen.Limits.MaxWallSecs != 600 ||
		task.reviser == nil {
		t.Fatalf("generator = %T %+v", task.generator, gen)
	}
	if err := task.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Without the default's model, a cloud worker doesn't start.
	cfg.Foundry.AI.Default = config.AIDefault{}
	if _, err := New(Deps{Cfg: &cfg, DB: store.Unimplemented{}, Databases: noDatabases{}}); err == nil {
		t.Fatal("a worker started without Helix's Claude")
	}
}
