package foundrysyncworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/helixdb/helix-foundry/backend/syncer"
)

// queue hands out its syncs in order and records what each attempt ended with.
type queue struct {
	mu        sync.Mutex
	pending   []*model.ConnectorSync
	outcomes  map[string]model.ConnectorSyncOutcome
	renewals  int
	loseLease bool
	// due is how many connectors the scheduler finds due; each call queues up to its limit.
	due        int
	dueCalls   int
	replaced   []*model.ReplacedSnapshot
	replacedBy time.Time
	pinned     []string
	forgotten  []string
	expired    []*model.ConnectorChangeBatch
	refreshes  []*model.FoundryOntology
	queueable  map[string]bool
	// offered are the ontologies a refresh was offered to the queue for, and
	// kinds the job each was offered as.
	offered  []string
	kinds    []model.FoundryGraphJobKind
	stranded []*model.Connector
	// strandedKinds are the kinds the release tick asked about.
	strandedKinds []string
	// purges are the purge rows, in age order; held the documents a live
	// load holds, and purgedBy the before the sweep last listed them with.
	purges   []*model.FoundryDocumentPurge
	held     []string
	purgedBy time.Time
	// eventsAfter is what the scheduler said an event source with a live sync waits.
	eventsAfter time.Duration
	// connectors are the stored connectors by id; connectorErr fails every read of one.
	connectors   map[string]*model.Connector
	connectorErr error
	// eventPurges is what the event sources' purge sees (purge_events_test.go).
	*eventPurges
	// finishErrs fail the next finishes, one each; a finish or a requeue
	// with its context done fails as the database's would. requeued are
	// the syncs given back uncounted.
	finishErrs []error
	requeued   []string
}

func (q *queue) GetConnector(_ context.Context, _, id string) (*model.Connector, error) {
	if q.connectorErr != nil {
		return nil, q.connectorErr
	}
	c, ok := q.connectors[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return c, nil
}

// ListFoundryDocumentPurges returns the purge rows before before that no live load holds, as the provider does.
func (q *queue) ListFoundryDocumentPurges(_ context.Context, before time.Time, limit int) ([]*model.FoundryDocumentPurge, error) {
	q.purgedBy = before
	var out []*model.FoundryDocumentPurge
	for _, p := range q.purges {
		if p.CreatedAt.Before(before) && !slices.Contains(q.held, p.DocumentID) && len(out) < limit {
			out = append(out, p)
		}
	}
	return out, nil
}

func (q *queue) DeleteFoundryDocumentPurge(_ context.Context, documentID string) error {
	q.forgotten = append(q.forgotten, documentID)
	q.purges = slices.DeleteFunc(q.purges, func(p *model.FoundryDocumentPurge) bool { return p.DocumentID == documentID })
	return nil
}

// prefixes is a document bucket recording deleted prefixes; refuse fails
// the next delete of each prefix it counts down.
type prefixes struct {
	deleted []string
	refuse  map[string]int
}

func (p *prefixes) DeletePrefix(_ context.Context, prefix string) error {
	if p.refuse[prefix] > 0 {
		p.refuse[prefix]--
		return errors.New("slow down")
	}
	p.deleted = append(p.deleted, prefix)
	return nil
}

func (q *queue) ListStrandedConnectorCursors(_ context.Context, kinds []string, _ int) ([]*model.Connector, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.strandedKinds = kinds
	return q.stranded, nil
}

// releases records the connectors released; refuse fails those named.
type releases struct {
	released []string
	refuse   map[string]bool
}

func (r *releases) Release(_ context.Context, c *model.Connector) error {
	if r.refuse[c.ID] {
		return errors.New("connection refused")
	}
	r.released = append(r.released, c.ID)
	return nil
}

func (q *queue) QueueDueConnectorSyncs(_ context.Context, limit int, eventsAfter time.Duration) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dueCalls++
	q.eventsAfter = eventsAfter
	n := min(q.due, limit)
	q.due -= n
	return n, nil
}

func (q *queue) ListReplacedSnapshots(_ context.Context, before time.Time, _ int) ([]*model.ReplacedSnapshot, error) {
	q.replacedBy = before
	var out []*model.ReplacedSnapshot
	for _, r := range q.replaced {
		if r.ReplacedAt.Before(before) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (q *queue) DeleteReplacedSnapshot(_ context.Context, id string) error {
	q.forgotten = append(q.forgotten, id)
	return nil
}

func (q *queue) ListPinnedSnapshotKeys(context.Context) ([]string, error) { return q.pinned, nil }

func (q *queue) ListExpiredChangeBatches(context.Context, time.Time, int) ([]*model.ConnectorChangeBatch, error) {
	return q.expired, nil
}

func (q *queue) DeleteConnectorChangeBatch(_ context.Context, id string) error {
	q.forgotten = append(q.forgotten, id)
	return nil
}

// ListDueFoundryRefreshes pages q.refreshes, which are in due order.
func (q *queue) ListDueFoundryRefreshes(_ context.Context, afterDue time.Time, afterID string, limit int) ([]*model.FoundryOntology, error) {
	var out []*model.FoundryOntology
	for _, o := range q.refreshes {
		due := *o.RefreshDueAt
		if !afterDue.IsZero() && (due.Before(afterDue) || due.Equal(afterDue) && o.ID <= afterID) {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, o)
	}
	return out, nil
}

func (q *queue) QueueFoundryRefresh(_ context.Context, _, ontologyID string, job *model.FoundryGraphJob) (bool, error) {
	q.offered = append(q.offered, ontologyID)
	q.kinds = append(q.kinds, job.Kind)
	return q.queueable[ontologyID], nil
}

// objects is a bucket recording deletes.
type objects struct{ deleted []string }

func (o *objects) Delete(_ context.Context, key string) error {
	o.deleted = append(o.deleted, key)
	return nil
}

func (o *objects) DeletePrefix(_ context.Context, prefix string) error {
	o.deleted = append(o.deleted, prefix)
	return nil
}

func (q *queue) ClaimConnectorSync(context.Context, string, time.Duration) (*model.ConnectorSync, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil, nil
	}
	next := q.pending[0]
	q.pending = q.pending[1:]
	return next, nil
}

func (q *queue) RenewConnectorSyncLease(context.Context, string, string, time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.renewals++
	if q.loseLease {
		return store.ErrNotFound
	}
	return nil
}

func (q *queue) FinishConnectorSync(ctx context.Context, id, _ string, outcome model.ConnectorSyncOutcome) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(q.finishErrs) > 0 {
		err := q.finishErrs[0]
		q.finishErrs = q.finishErrs[1:]
		return err
	}
	q.outcomes[id] = outcome
	return nil
}

func (q *queue) RequeueConnectorSync(ctx context.Context, id, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	q.requeued = append(q.requeued, id)
	return nil
}

type runner func(ctx context.Context, sync *model.ConnectorSync) (syncer.Result, error)

func (r runner) Run(ctx context.Context, sync *model.ConnectorSync) (syncer.Result, error) {
	return r(ctx, sync)
}

var now = time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)

func newTask(q *queue, r runner) *Task {
	return &Task{
		cfg: config.SyncConfig{Lease: 30 * time.Millisecond, Timeout: time.Minute, MaxAttempts: 3, ScheduleBatch: 2,
			ReplacedSnapshotGrace: time.Hour, ChangeRetention: 24 * time.Hour},
		deps:  Deps{Cfg: &config.Config{Foundry: config.Foundry{Graph: config.GraphConfig{MonthlyUSD: 100}}}},
		queue: q, runner: r, workerID: "worker-1", now: func() time.Time { return now }, jitter: func() float64 { return 0 },
		snapshots: &objects{}, documents: &prefixes{}, lag: &observed{},
		spacing: model.EventSyncSpacing{After: time.Minute, PerMillionRows: 10 * time.Minute},
	}
}

// observed records a histogram's observations.
type observed struct{ values []float64 }

func (o *observed) Observe(v float64) { o.values = append(o.values, v) }

// planNamed is one of the default catalog's plans.
func planNamed(t *testing.T, code string) *foundryplan.Plan {
	t.Helper()
	c, err := foundryplan.NewCatalog(config.DefaultPlans(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Get(code)
}

func done(rows int64) runner {
	return func(context.Context, *model.ConnectorSync) (syncer.Result, error) {
		return syncer.Result{Rows: rows}, nil
	}
}

func failing(err error) runner {
	return func(context.Context, *model.ConnectorSync) (syncer.Result, error) { return syncer.Result{}, err }
}

func newQueue(syncs ...*model.ConnectorSync) *queue {
	return &queue{pending: syncs, outcomes: map[string]model.ConnectorSyncOutcome{}, eventPurges: &eventPurges{}}
}

func TestFoundrySyncWorker(t *testing.T) {
	t.Run("records rows of a successful sync", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", Attempts: 1})
		newTask(q, done(42)).drain(context.Background())
		got := q.outcomes["s1"]
		if got.Status != model.ConnectorSyncSucceeded || *got.RowsSynced != 42 {
			t.Fatalf("outcome = %+v", got)
		}
	})
	// A sync is queued as a change read; on Free, or with no cursor yet, it reads in full, and the history must say so.
	t.Run("records the mode the sync read with", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", Attempts: 1, Mode: model.ConnectorSyncChanges})
		newTask(q, func(context.Context, *model.ConnectorSync) (syncer.Result, error) {
			return syncer.Result{Mode: model.ConnectorSyncFull}, nil
		}).drain(context.Background())
		if got := q.outcomes["s1"]; got.Mode != model.ConnectorSyncFull {
			t.Fatalf("outcome mode = %q, want full", got.Mode)
		}
	})
	// A blip is retried after a backoff that doubles with each attempt.
	t.Run("retries a transient failure with backoff", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", Attempts: 2})
		newTask(q, failing(&syncer.Failure{Message: "could not reach Stripe; the sync will retry"})).drain(context.Background())
		got := q.outcomes["s1"]
		if got.RetryAt == nil || !got.RetryAt.Equal(now.Add(2*time.Minute)) || got.Error != "could not reach Stripe; the sync will retry" {
			t.Fatalf("outcome = %+v", got)
		}
	})
	t.Run("fails a permanent failure at once", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", Attempts: 1})
		newTask(q, failing(&syncer.Failure{Message: "this connector needs Foundry Pro", Permanent: true})).drain(context.Background())
		got := q.outcomes["s1"]
		if got.Status != model.ConnectorSyncFailed || got.RetryAt != nil || got.Error != "this connector needs Foundry Pro" {
			t.Fatalf("outcome = %+v", got)
		}
	})
	t.Run("gives up after the last attempt", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", Attempts: 3})
		newTask(q, failing(errors.New("boom"))).drain(context.Background())
		got := q.outcomes["s1"]
		if got.Status != model.ConnectorSyncFailed || !strings.Contains(got.Error, "gave up after 3 attempts") {
			t.Fatalf("outcome = %+v", got)
		}
	})
	t.Run("drains every due sync", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", Attempts: 1}, &model.ConnectorSync{ID: "s2", Attempts: 1})
		newTask(q, done(1)).drain(context.Background())
		if len(q.outcomes) != 2 {
			t.Fatalf("outcomes = %v", q.outcomes)
		}
	})
	// Another worker owns a sync whose lease was lost, so this attempt must stop writing.
	t.Run("cancels the attempt when the lease is lost", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", Attempts: 1})
		q.loseLease = true
		newTask(q, func(ctx context.Context, _ *model.ConnectorSync) (syncer.Result, error) {
			<-ctx.Done()
			return syncer.Result{}, ctx.Err()
		}).drain(context.Background())
		if q.renewals == 0 {
			t.Fatal("lease was never renewed")
		}
	})
}

// A container killed mid-sync leaves its downloads on the pod's volume.
// New hands the syncer the allowances of work no person started by kind,
// and the events kind the window of each API source's newest pieces: the
// syncer pauses an API source's set by the one, and the kind keeps the other.
func TestNewWiresAPISourceExtraction(t *testing.T) {
	cfg := config.Defaults()
	cfg.Foundry.Events.MaxExtracted = 7
	task, err := New(Deps{Cfg: &cfg, DB: store.Unimplemented{}})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := task.runner.(*syncer.Syncer)
	if !ok {
		t.Fatalf("runner = %T", task.runner)
	}
	if want := cfg.Foundry.SetAllowances(); len(want) == 0 || !maps.Equal(s.Allowances, want) {
		t.Fatalf("allowances = %v, want %v", s.Allowances, want)
	}
	kind, err := s.Catalog.Kind(model.ConnectorKindEvents)
	if err != nil {
		t.Fatal(err)
	}
	streams := kind.(connector.DocumentKind).Streams(connector.Source{Settings: json.RawMessage(`{"description":"Replies","text_field":"reply"}`)})
	if len(streams) != 1 || streams[0].Name != connector.EventsStream || streams[0].Keep.Newest() != 7 {
		t.Fatalf("streams = %+v", streams)
	}
}

func TestSweepTemp(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"foundry-s3-123/a.parquet", "foundry-snapshot-1.jsonl.gz", "foundry-pieces-1", "other/keep.txt"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	task := newTask(newQueue(), nil)
	task.tempDir = dir
	task.sweepTemp(context.Background())
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "other" {
		t.Fatalf("left %v", entries)
	}
}

func TestFoundrySyncSchedule(t *testing.T) {
	syncOnce := func(t *testing.T, r runner) model.ConnectorSyncOutcome {
		t.Helper()
		q := newQueue(&model.ConnectorSync{ID: "s1", Attempts: 3})
		newTask(q, r).drain(context.Background())
		return q.outcomes["s1"]
	}
	result := func(plan *foundryplan.Plan, every *int) runner {
		return func(context.Context, *model.ConnectorSync) (syncer.Result, error) {
			return syncer.Result{Plan: plan, SyncEverySeconds: every}, nil
		}
	}
	seconds := func(n int) *int { return &n }
	// A connector syncs again one plan interval after its last sync ended.
	t.Run("schedules the next sync at the plan's interval", func(t *testing.T) {
		got := syncOnce(t, result(planNamed(t, "pro"), nil))
		if got.Schedule == nil || !got.Schedule.NextSyncAt.Equal(now.Add(time.Hour)) || got.Schedule.Paused != "" {
			t.Fatalf("schedule = %+v", got.Schedule)
		}
	})
	// The finish writes this schedule only while the connector still has the interval it came from.
	t.Run("keeps an owner's longer interval", func(t *testing.T) {
		got := syncOnce(t, result(planNamed(t, "pro"), seconds(3*3600)))
		if !got.Schedule.NextSyncAt.Equal(now.Add(3*time.Hour)) || got.Schedule.Every == nil || *got.Schedule.Every != 3*3600 {
			t.Fatalf("schedule = %+v", got.Schedule)
		}
	})
	// A downgrade lowers what an owner's choice may be; the plan's shortest wins.
	t.Run("raises an interval below the plan's shortest", func(t *testing.T) {
		got := syncOnce(t, result(planNamed(t, "free"), seconds(3600)))
		if !got.Schedule.NextSyncAt.Equal(now.Add(24 * time.Hour)) {
			t.Fatalf("schedule = %+v", got.Schedule)
		}
	})
	// The wait between queueing and the first claim is how far scheduled syncs fall behind;
	// a retry's wait is backoff, not lag.
	t.Run("measures how long a scheduled sync waited for its first claim", func(t *testing.T) {
		q := newQueue(
			&model.ConnectorSync{ID: "s1", Origin: model.ConnectorSyncSchedule, Attempts: 1, CreatedAt: now.Add(-90 * time.Second)},
			&model.ConnectorSync{ID: "s2", Origin: model.ConnectorSyncSchedule, Attempts: 2, CreatedAt: now.Add(-time.Hour)},
			&model.ConnectorSync{ID: "s3", Origin: model.ConnectorSyncManual, Attempts: 1, CreatedAt: now.Add(-time.Hour)},
		)
		task := newTask(q, result(planNamed(t, "pro"), nil))
		task.drain(context.Background())
		if got := task.lag.(*observed).values; !slices.Equal(got, []float64{90}) {
			t.Fatalf("lag observations = %v, want [90]", got)
		}
	})
	// PostHog's query API forbids recurring exports, so its connectors sync at most daily on any plan.
	t.Run("keeps a kind's own floor over the plan's interval", func(t *testing.T) {
		got := syncOnce(t, func(context.Context, *model.ConnectorSync) (syncer.Result, error) {
			return syncer.Result{Plan: planNamed(t, "max"), MinEvery: 24 * time.Hour}, nil
		})
		if !got.Schedule.NextSyncAt.Equal(now.Add(24 * time.Hour)) {
			t.Fatalf("schedule = %+v", got.Schedule)
		}
	})
	t.Run("schedules nothing for a manual-only connector", func(t *testing.T) {
		got := syncOnce(t, result(planNamed(t, "pro"), seconds(0)))
		if got.Schedule == nil || got.Schedule.NextSyncAt != nil {
			t.Fatalf("schedule = %+v", got.Schedule)
		}
	})
	// Connectors created together must not all sync in the same second forever.
	t.Run("stretches the interval by up to five percent", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", Attempts: 1})
		task := newTask(q, result(planNamed(t, "pro"), nil))
		task.jitter = func() float64 { return 0.5 }
		task.drain(context.Background())
		if got := q.outcomes["s1"].Schedule.NextSyncAt; !got.Equal(now.Add(time.Hour + 90*time.Second)) {
			t.Fatalf("next = %v", got)
		}
	})
	// The same refusal comes back every time; repeating it only spams the source.
	t.Run("pauses the schedule on a failure that is for good", func(t *testing.T) {
		got := syncOnce(t, failing(&syncer.Failure{Message: "Stripe rejected this key", Permanent: true}))
		if got.Schedule == nil || got.Schedule.NextSyncAt != nil || got.Schedule.Paused != "Stripe rejected this key" {
			t.Fatalf("schedule = %+v", got.Schedule)
		}
	})
	// Nothing tells the schedule a plan was bought again, so it checks back daily.
	t.Run("checks back daily on a plan the workspace no longer has", func(t *testing.T) {
		got := syncOnce(t, failing(&syncer.Failure{Message: "this connector needs Foundry Pro", Permanent: true, PlanRequired: true}))
		if got.Schedule == nil || !got.Schedule.NextSyncAt.Equal(now.Add(24*time.Hour)) || got.Schedule.Paused != "this connector needs Foundry Pro" {
			t.Fatalf("schedule = %+v", got.Schedule)
		}
	})
	// An outage that outlasted the retries is not the owner's to fix; the next interval may succeed.
	t.Run("keeps the schedule after giving up on an outage", func(t *testing.T) {
		got := syncOnce(t, func(context.Context, *model.ConnectorSync) (syncer.Result, error) {
			return syncer.Result{Plan: planNamed(t, "pro")}, errors.New("boom")
		})
		if got.Schedule == nil || !got.Schedule.NextSyncAt.Equal(now.Add(time.Hour)) || got.Schedule.Paused != "" {
			t.Fatalf("schedule = %+v", got.Schedule)
		}
	})
	// A full batch may mean more are due; the tick keeps going until a short one.
	// An event source due while its sync is live waits sync_after, a minute.
	t.Run("queues due connectors until none is left", func(t *testing.T) {
		q := newQueue()
		q.due = 5
		newTask(q, nil).schedule(context.Background())
		if q.due != 0 || q.dueCalls != 3 || q.eventsAfter != time.Minute {
			t.Fatalf("due = %d, calls = %d, events after %v", q.due, q.dueCalls, q.eventsAfter)
		}
	})
}

func TestEventSourceSchedule(t *testing.T) {
	syncOnce := func(t *testing.T, sync *model.ConnectorSync, r runner) model.ConnectorSyncOutcome {
		t.Helper()
		q := newQueue(sync)
		newTask(q, r).drain(context.Background())
		return q.outcomes[sync.ID]
	}
	events := func(attempts int) *model.ConnectorSync {
		return &model.ConnectorSync{ID: "s1", Origin: model.ConnectorSyncEvents, Attempts: attempts}
	}
	// A source syncs by what waits beyond the cursor its sync stored: with
	// nothing, it keeps the sync a send scheduled meanwhile; with less than
	// one read takes, it waits out the spacing (a minute, and ten more per
	// million rows); with more, it syncs again at once. None of it is the
	// plan's interval.
	for _, c := range []struct {
		name    string
		backlog connector.Backlog
		want    model.PushedNext
	}{
		{"keeps what a send scheduled when nothing waits", connector.BacklogNone, model.PushedKeep{}},
		{"syncs again after the spacing when less than one read waits", connector.BacklogSome, model.PushedNotBefore{At: now.Add(21 * time.Minute)}},
		{"syncs again at once when more than one read waits", connector.BacklogMore, model.PushedAt{At: now}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := syncOnce(t, events(1), func(context.Context, *model.ConnectorSync) (syncer.Result, error) {
				return syncer.Result{Rows: 2_000_000, Plan: planNamed(t, "free"),
					Pushed: &syncer.PushedResult{Backlog: c.backlog, Rows: 2_000_000}}, nil
			})
			if got.Status != model.ConnectorSyncSucceeded || got.Schedule == nil {
				t.Fatalf("outcome = %+v", got)
			}
			if s := got.Schedule; s.Pushed != c.want || s.NextSyncAt != nil || s.Every != nil || s.Paused != "" {
				t.Fatalf("schedule = %+v, want pushed %+v alone", s, c.want)
			}
		})
	}
	// An outage that outlasted the retries tries again after the spacing,
	// not after Free's day. The spacing is by the rows the table holds,
	// which the attempt's own count misses when it failed before
	// publishing: a 2M-row merge failing part way would otherwise run again
	// after the floor, a minute, and hold a worker the rest of the deployment
	// waits for.
	for _, c := range []struct {
		name              string
		stored, published int64
		want              time.Duration
	}{
		{"published", 1_000_000, 1_000_000, 11 * time.Minute},
		{"failing before its table published", 2_000_000, 0, 21 * time.Minute},
		{"failing after its table grew", 1_000_000, 2_000_000, 21 * time.Minute},
	} {
		t.Run("tries again after the spacing once it gives up, "+c.name, func(t *testing.T) {
			got := syncOnce(t, events(3), func(context.Context, *model.ConnectorSync) (syncer.Result, error) {
				return syncer.Result{Rows: c.published, StoredRows: c.stored, Plan: planNamed(t, "free")}, errors.New("S3 throttled")
			})
			if got.Status != model.ConnectorSyncFailed || !strings.Contains(got.Error, "gave up after 3 attempts") || got.Schedule == nil {
				t.Fatalf("outcome = %+v", got)
			}
			want := model.PushedNotBefore{At: now.Add(c.want)}
			if s := got.Schedule; s.Pushed != want || s.NextSyncAt != nil || s.Every != nil || s.Paused != "" {
				t.Fatalf("schedule = %+v, want pushed %+v alone", s, want)
			}
		})
	}
	// Before its last attempt, a blip is retried as any sync's is.
	t.Run("retries a blip with backoff", func(t *testing.T) {
		got := syncOnce(t, events(1), failing(errors.New("S3 throttled")))
		if got.RetryAt == nil || !got.RetryAt.Equal(now.Add(time.Minute)) || got.Schedule != nil {
			t.Fatalf("outcome = %+v", got)
		}
	})
	// A batch missing from storage comes back missing every time: the
	// source pauses, as any connector does, until someone restores it.
	t.Run("pauses on a failure that is for good", func(t *testing.T) {
		got := syncOnce(t, events(1), failing(&syncer.Failure{Message: "batch 3 of this event source is missing from storage", Permanent: true}))
		if s := got.Schedule; got.Status != model.ConnectorSyncFailed || s == nil || s.Pushed != nil || s.NextSyncAt != nil ||
			s.Paused != "batch 3 of this event source is missing from storage" {
			t.Fatalf("outcome = %+v, schedule = %+v", got, s)
		}
	})
}

func TestStoppedSync(t *testing.T) {
	// ran records whether the attempt ran.
	ran := func(ran *bool) runner {
		return func(context.Context, *model.ConnectorSync) (syncer.Result, error) {
			*ran = true
			return syncer.Result{}, nil
		}
	}
	hourly := 3600
	// An attempt that kills its pod never finishes, and its lease's expiry
	// hands it out again with one more attempt. The last attempt still runs,
	// on any kind.
	for _, origin := range []model.ConnectorSyncOrigin{model.ConnectorSyncSchedule, model.ConnectorSyncEvents} {
		t.Run("runs the last attempt, origin "+string(origin), func(t *testing.T) {
			q := newQueue(&model.ConnectorSync{ID: "s1", ConnectorID: "c1", Origin: origin, Attempts: 3})
			var didRun bool
			newTask(q, ran(&didRun)).drain(context.Background())
			if !didRun || q.outcomes["s1"].Status != model.ConnectorSyncSucceeded {
				t.Fatalf("ran = %v, outcome = %+v", didRun, q.outcomes["s1"])
			}
		})
	}
	// Past the last attempt, the sync fails for good without running, on
	// any kind, pausing the connector under the interval it has, so the
	// guard that keeps an owner's newer schedule lets the pause through.
	for _, c := range []struct {
		origin   model.ConnectorSyncOrigin
		attempts int
	}{{model.ConnectorSyncSchedule, 4}, {model.ConnectorSyncManual, 7}, {model.ConnectorSyncEvents, 4}} {
		t.Run(fmt.Sprintf("stops at attempt %d, origin %s", c.attempts, c.origin), func(t *testing.T) {
			q := newQueue(&model.ConnectorSync{ID: "s1", ConnectorID: "c1", Origin: c.origin, Attempts: c.attempts})
			q.connectors = map[string]*model.Connector{"c1": {ID: "c1", Kind: "stripe", SyncEverySeconds: &hourly}}
			var didRun bool
			newTask(q, ran(&didRun)).drain(context.Background())
			got := q.outcomes["s1"]
			want := fmt.Sprintf("the sync stopped its worker after %d attempts; Helix paused it", c.attempts-1)
			if s := got.Schedule; didRun || got.Status != model.ConnectorSyncFailed || got.RetryAt != nil || got.Error != want ||
				s == nil || s.Paused != want || s.NextSyncAt != nil || s.Pushed != nil || s.Every == nil || *s.Every != hourly {
				t.Fatalf("ran = %v, outcome = %+v, schedule = %+v", didRun, got, s)
			}
		})
	}
	// A deleted connector has no schedule left to pause; its sync still ends.
	t.Run("ends a deleted connector's sync", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", ConnectorID: "gone", Attempts: 4})
		var didRun bool
		newTask(q, ran(&didRun)).drain(context.Background())
		if got := q.outcomes["s1"]; didRun || got.Status != model.ConnectorSyncFailed || got.Schedule == nil || got.Schedule.Every != nil {
			t.Fatalf("ran = %v, outcome = %+v", didRun, got)
		}
	})
	// Without the connector's interval the pause could miss it; the lease
	// runs out, and the next claim tries again.
	t.Run("leaves the sync to its lease when the connector can't be read", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", ConnectorID: "c1", Attempts: 4})
		q.connectorErr = errors.New("database unavailable")
		var didRun bool
		newTask(q, ran(&didRun)).drain(context.Background())
		if _, finished := q.outcomes["s1"]; didRun || finished {
			t.Fatalf("ran = %v, finished = %v", didRun, finished)
		}
	})
	// A deploy stops the worker mid-sync: the attempt it cut short is given
	// back uncounted, not left to its lease, whose expiry would count it,
	// so a few deploys during one sync never pause its connector.
	t.Run("gives back uncounted an attempt the worker's stop cuts short", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", ConnectorID: "c1", Origin: model.ConnectorSyncEvents, Attempts: 3})
		ctx, stop := context.WithCancel(context.Background())
		newTask(q, func(runCtx context.Context, _ *model.ConnectorSync) (syncer.Result, error) {
			stop()
			<-runCtx.Done()
			return syncer.Result{}, &syncer.Failure{Message: "the sync was stopped", Err: runCtx.Err()}
		}).drain(ctx)
		if _, finished := q.outcomes["s1"]; finished || !slices.Equal(q.requeued, []string{"s1"}) {
			t.Fatalf("outcome %+v, requeued %v", q.outcomes["s1"], q.requeued)
		}
	})
	// An attempt that ended as the worker stopped, or whose finish meets a
	// passing error, still has its outcome recorded: one left to its lease
	// would come back counted, and past the last attempt pause a connector
	// whose sync did nothing wrong.
	t.Run("records an outcome past the worker's stop and a passing error", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", ConnectorID: "c1", Attempts: 3})
		q.finishErrs = []error{errors.New("Deadlock found when trying to get lock")}
		ctx, stop := context.WithCancel(context.Background())
		newTask(q, func(context.Context, *model.ConnectorSync) (syncer.Result, error) {
			stop()
			return syncer.Result{Rows: 7}, nil
		}).drain(ctx)
		if got := q.outcomes["s1"]; got.Status != model.ConnectorSyncSucceeded || *got.RowsSynced != 7 || len(q.requeued) != 0 {
			t.Fatalf("outcome %+v, requeued %v", got, q.requeued)
		}
	})
	// A lease another worker holds now is final: retrying the finish would
	// only fail again.
	t.Run("stops trying to finish once the lease is lost", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", ConnectorID: "c1", Attempts: 1})
		q.finishErrs = []error{store.ErrNotFound, nil}
		newTask(q, done(1)).drain(context.Background())
		if _, finished := q.outcomes["s1"]; finished || len(q.finishErrs) != 1 {
			t.Fatalf("finished %v, %d finish errors left", finished, len(q.finishErrs))
		}
	})
	// A database down for every try leaves the sync to its lease.
	t.Run("gives up a finish after its tries", func(t *testing.T) {
		q := newQueue(&model.ConnectorSync{ID: "s1", ConnectorID: "c1", Attempts: 1})
		down := errors.New("connection refused")
		q.finishErrs = []error{down, down, down, nil}
		newTask(q, done(1)).drain(context.Background())
		if _, finished := q.outcomes["s1"]; finished || len(q.finishErrs) != 1 {
			t.Fatalf("finished %v, %d finish errors left", finished, len(q.finishErrs))
		}
	})
	// A first read that killed its pod stored no cursor, so nothing else
	// would drop the replication slot its Start took, and the database
	// keeps WAL for it until its disk fills. With a cursor stored, the
	// release tick drops it once the connector is paused. A source that
	// refuses the release still has its sync stopped.
	t.Run("gives back what a first read took", func(t *testing.T) {
		q := newQueue(
			&model.ConnectorSync{ID: "s1", ConnectorID: "first", Attempts: 4},
			&model.ConnectorSync{ID: "s2", ConnectorID: "followed", Attempts: 4},
			&model.ConnectorSync{ID: "s3", ConnectorID: "stripe", Attempts: 4},
			&model.ConnectorSync{ID: "s4", ConnectorID: "refused", Attempts: 4},
		)
		q.connectors = map[string]*model.Connector{
			"first":    {ID: "first", Kind: "postgres"},
			"followed": {ID: "followed", Kind: "postgres", ChangeCursor: []byte(`{"slot":"helix_followed"}`)},
			"stripe":   {ID: "stripe", Kind: "stripe"},
			"refused":  {ID: "refused", Kind: "postgres"},
		}
		task := newTask(q, nil)
		r := &releases{refuse: map[string]bool{"refused": true}}
		task.releaser, task.releaseKinds = r, []string{"postgres"}
		task.drain(context.Background())
		if !slices.Equal(r.released, []string{"first"}) || len(q.outcomes) != 4 || q.outcomes["s4"].Status != model.ConnectorSyncFailed {
			t.Fatalf("released = %v, outcomes = %v", r.released, q.outcomes)
		}
	})
}

func TestFoundrySyncRelease(t *testing.T) {
	// A refused release must not stop the others: one dead database can't
	// leave every other customer's slot holding WAL.
	t.Run("releases each stranded connector and goes on past a refusal", func(t *testing.T) {
		q := newQueue()
		q.stranded = []*model.Connector{{ID: "c1", Kind: "postgres"}, {ID: "c2", Kind: "postgres"}, {ID: "c3", Kind: "postgres"}}
		task := newTask(q, nil)
		r := &releases{refuse: map[string]bool{"c2": true}}
		task.releaser, task.releaseKinds = r, []string{"postgres"}
		task.release(context.Background())
		if !slices.Equal(r.released, []string{"c1", "c3"}) || !slices.Equal(q.strandedKinds, []string{"postgres"}) {
			t.Fatalf("released = %v, asked for kinds %v", r.released, q.strandedKinds)
		}
	})
	t.Run("asks nothing when no kind holds anything", func(t *testing.T) {
		q := newQueue()
		task := newTask(q, nil)
		task.releaser = &releases{}
		task.release(context.Background())
		if q.strandedKinds != nil {
			t.Fatalf("listed stranded cursors for kinds %v", q.strandedKinds)
		}
	})
}

func TestFoundrySyncSweep(t *testing.T) {
	// A replaced snapshot may still be read by a load; only an old, unpinned one goes.
	t.Run("deletes replaced snapshots past the grace that no load pins", func(t *testing.T) {
		q := newQueue()
		old := now.Add(-2 * time.Hour)
		q.replaced = []*model.ReplacedSnapshot{
			{ID: "r1", S3Key: "free", ReplacedAt: old},
			{ID: "r2", S3Key: "pinned", ReplacedAt: old},
			{ID: "r3", S3Key: "young", ReplacedAt: now.Add(-time.Minute)},
		}
		q.pinned = []string{"pinned"}
		task := newTask(q, nil)
		task.sweep(context.Background())
		store := task.snapshots.(*objects)
		if !q.replacedBy.Equal(now.Add(-time.Hour)) || len(store.deleted) != 1 || store.deleted[0] != "free" || len(q.forgotten) != 1 || q.forgotten[0] != "r1" {
			t.Fatalf("deleted = %v, forgotten = %v, before = %v", store.deleted, q.forgotten, q.replacedBy)
		}
	})
	// A purged document goes once the grace has passed and no live load holds
	// it; one whose prefix fails to delete is tried again at the next tick.
	t.Run("deletes purged documents past the grace that no load holds, retrying a failure", func(t *testing.T) {
		q := newQueue()
		old := now.Add(-2 * time.Hour)
		q.purges = []*model.FoundryDocumentPurge{
			{DocumentID: "d1", WorkspaceID: "ws-1", CreatedAt: old},
			{DocumentID: "d2", WorkspaceID: "ws-1", CreatedAt: old},
			{DocumentID: "d3", WorkspaceID: "ws-2", CreatedAt: old},
			{DocumentID: "d4", WorkspaceID: "ws-1", CreatedAt: now.Add(-time.Minute)},
		}
		q.held = []string{"d2"}
		task := newTask(q, nil)
		store := &prefixes{refuse: map[string]int{"ws/ws-2/documents/d3/": 1}}
		task.documents = store
		purged := testutil.ToFloat64(documentsPurged)
		task.sweep(context.Background())
		if !q.purgedBy.Equal(now.Add(-time.Hour)) || !slices.Equal(store.deleted, []string{"ws/ws-1/documents/d1/"}) ||
			!slices.Equal(q.forgotten, []string{"d1"}) {
			t.Fatalf("deleted = %v, forgotten = %v, before = %v", store.deleted, q.forgotten, q.purgedBy)
		}
		task.sweep(context.Background())
		if !slices.Equal(store.deleted, []string{"ws/ws-1/documents/d1/", "ws/ws-2/documents/d3/"}) || !slices.Equal(q.forgotten, []string{"d1", "d3"}) ||
			testutil.ToFloat64(documentsPurged)-purged != 2 {
			t.Fatalf("deleted = %v, forgotten = %v", store.deleted, q.forgotten)
		}
	})
	t.Run("deletes change batches past their retention", func(t *testing.T) {
		q := newQueue()
		key := "ws/changes/b1"
		q.expired = []*model.ConnectorChangeBatch{{ID: "b1", S3Key: &key}, {ID: "b2"}}
		task := newTask(q, nil)
		task.sweep(context.Background())
		if store := task.snapshots.(*objects); len(store.deleted) != 1 || store.deleted[0] != key || len(q.forgotten) != 2 {
			t.Fatalf("deleted = %v, forgotten = %v", store.deleted, q.forgotten)
		}
	})
}

func TestFoundrySyncRefresh(t *testing.T) {
	// Each due refresh is offered to the queue, which checks under the ontology's lock that no job is live.
	t.Run("offers each due refresh", func(t *testing.T) {
		q := newQueue()
		due := now.Add(-time.Minute)
		q.refreshes = []*model.FoundryOntology{{ID: "o1", WorkspaceID: "w", RefreshDueAt: &due}, {ID: "o2", WorkspaceID: "w", RefreshDueAt: &due}}
		q.queueable = map[string]bool{"o1": true}
		newTask(q, nil).refresh(context.Background())
		if !slices.Equal(q.offered, []string{"o1", "o2"}) {
			t.Fatalf("offered = %v", q.offered)
		}
	})
	// With apply off in a deployment, every refresh builds a new load.
	t.Run("builds every refresh with apply off", func(t *testing.T) {
		q := newQueue()
		due := now.Add(-time.Minute)
		q.refreshes = []*model.FoundryOntology{{ID: "o1", WorkspaceID: "w", RefreshDueAt: &due}}
		newTask(q, nil).refresh(context.Background())
		if !slices.Equal(q.kinds, []model.FoundryGraphJobKind{""}) {
			t.Fatalf("kinds = %v", q.kinds)
		}
	})
	// With apply on, the tick asks for an apply; the queue decides if it can.
	t.Run("asks for an apply with apply on", func(t *testing.T) {
		q := newQueue()
		due := now.Add(-time.Minute)
		q.refreshes = []*model.FoundryOntology{{ID: "o1", WorkspaceID: "w", RefreshDueAt: &due}}
		task := newTask(q, nil)
		task.deps.Cfg.Foundry.Graph.Apply = true
		task.refresh(context.Background())
		if !slices.Equal(q.kinds, []model.FoundryGraphJobKind{model.FoundryGraphJobApply}) {
			t.Fatalf("kinds = %v", q.kinds)
		}
	})
	// Held-back refreshes stay due; a full page of them must not hide the rest.
	t.Run("pages past refreshes it can't queue yet", func(t *testing.T) {
		q := newQueue()
		earlier, later := now.Add(-time.Hour), now.Add(-time.Minute)
		q.refreshes = []*model.FoundryOntology{
			{ID: "busy1", WorkspaceID: "w", RefreshDueAt: &earlier}, {ID: "busy2", WorkspaceID: "w", RefreshDueAt: &earlier},
			{ID: "ready", WorkspaceID: "w", RefreshDueAt: &later},
		}
		q.queueable = map[string]bool{"ready": true}
		newTask(q, nil).refresh(context.Background())
		if len(q.offered) != 3 {
			t.Fatalf("offered %d refreshes, want all 3 across pages of 2", len(q.offered))
		}
	})
}
