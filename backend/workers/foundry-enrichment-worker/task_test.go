package foundryenrichmentworker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// queue hands out its steps in order; each run blocks until its step is
// released or the worker stops, and records how it ended.
type queue struct {
	mu       sync.Mutex
	pending  []*model.FoundryLoadEnrichment
	started  chan string
	release  map[string]chan struct{}
	stopped  []string
	panicOn  string
	claimErr error
}

func newQueue(loads ...string) *queue {
	q := &queue{started: make(chan string, len(loads)), release: map[string]chan struct{}{}}
	for _, load := range loads {
		q.pending = append(q.pending, &model.FoundryLoadEnrichment{LoadID: load})
		q.release[load] = make(chan struct{})
	}
	return q
}

func (q *queue) Claim(context.Context) (*model.FoundryLoadEnrichment, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.claimErr != nil || len(q.pending) == 0 {
		return nil, q.claimErr
	}
	next := q.pending[0]
	q.pending = q.pending[1:]
	return next, nil
}

func (q *queue) Run(stopping context.Context, step *model.FoundryLoadEnrichment, _ time.Time) error {
	q.started <- step.LoadID
	if step.LoadID == q.panicOn {
		panic("boom")
	}
	select {
	case <-q.release[step.LoadID]:
		return nil
	case <-stopping.Done():
		q.mu.Lock()
		defer q.mu.Unlock()
		q.stopped = append(q.stopped, step.LoadID)
		return nil
	}
}

func (q *queue) left() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// sweepLog counts sweeps of files and purges, and the deadline each was
// given.
type sweepLog struct {
	mu                  sync.Mutex
	files               int
	runs                int
	filesBy, purgesBy   time.Time
	filesErr, purgesErr error
	filesTook           time.Duration
}

func (s *sweepLog) Files(ctx context.Context) (int, error) {
	time.Sleep(s.filesTook)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files++
	s.filesBy, _ = ctx.Deadline()
	return 0, s.filesErr
}

func (s *sweepLog) Purges(ctx context.Context) (map[model.FoundryEnrichmentPurgeStep]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs++
	s.purgesBy, _ = ctx.Deadline()
	return nil, s.purgesErr
}

func newTask(q *queue, s *sweepLog, maxRunning int) *Task {
	return &Task{
		cfg: config.EnrichmentConfig{MaxRunning: maxRunning, PollInterval: 10 * time.Millisecond}, steps: q, sweeper: s,
		now: time.Now, slots: make(chan struct{}, maxRunning),
	}
}

// waitStarted waits for n steps to start.
func waitStarted(t *testing.T, q *queue, n int) []string {
	t.Helper()
	var loads []string
	for range n {
		select {
		case load := <-q.started:
			loads = append(loads, load)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %v started", loads)
		}
	}
	return loads
}

func TestTask(t *testing.T) {
	// One replica runs at most max_running steps at once, each beside the
	// others; a step that ends frees its slot for the next claim.
	t.Run("runs at most max_running steps at once", func(t *testing.T) {
		q := newQueue("a", "b", "c", "d")
		task := newTask(q, &sweepLog{}, 3)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		task.claim(ctx)
		waitStarted(t, q, 3)
		if q.left() != 1 {
			t.Fatalf("%d steps left unclaimed, want 1", q.left())
		}
		task.claim(ctx)
		if q.left() != 1 {
			t.Fatal("claimed past the limit")
		}
		close(q.release["a"])
		deadline := time.Now().Add(5 * time.Second)
		for q.left() == 1 && time.Now().Before(deadline) {
			task.claim(ctx)
			time.Sleep(time.Millisecond)
		}
		if got := waitStarted(t, q, 1); got[0] != "d" {
			t.Fatalf("started %v after a slot freed", got)
		}
		cancel()
		task.running.Wait()
	})

	// SIGTERM: no step is claimed any more, and Stop returns once every
	// running step has handed over.
	t.Run("stops claiming and waits for running steps on SIGTERM", func(t *testing.T) {
		q := newQueue("a", "b", "c")
		task := newTask(q, &sweepLog{}, 2)
		started := make(chan error, 1)
		go func() { started <- task.Start(context.Background()) }()
		waitStarted(t, q, 2)
		if err := task.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := <-started; err != nil {
			t.Fatal(err)
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		if len(q.stopped) != 2 || len(q.pending) != 1 {
			t.Fatalf("handed over %v, %d left", q.stopped, len(q.pending))
		}
	})

	// A step that panics doesn't take the worker down: its lease lapses and
	// another claim resumes it, and its slot is free again.
	t.Run("survives a step that panics", func(t *testing.T) {
		q := newQueue("a", "b")
		q.panicOn = "a"
		close(q.release["b"])
		task := newTask(q, &sweepLog{}, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		task.claim(ctx)
		waitStarted(t, q, 1)
		task.running.Wait()
		task.claim(ctx)
		if got := waitStarted(t, q, 1); got[0] != "b" {
			t.Fatalf("started %v after a panic", got)
		}
		task.running.Wait()
	})

	// A claim that fails frees its slot: the next tick tries again.
	t.Run("a failed claim frees its slot", func(t *testing.T) {
		q := newQueue("a")
		q.claimErr = errors.New("db down")
		task := newTask(q, &sweepLog{}, 1)
		task.claim(context.Background())
		q.claimErr = nil
		close(q.release["a"])
		task.claim(context.Background())
		waitStarted(t, q, 1)
		task.running.Wait()
	})

	// The sweep runs files and purges each time, even when one fails.
	t.Run("sweeps files and purges", func(t *testing.T) {
		s := &sweepLog{filesErr: errors.New("files unavailable"), purgesErr: errors.New("purges unavailable")}
		task := newTask(newQueue(), s, 1)
		task.sweep(context.Background())
		if s.files != 1 || s.runs != 1 {
			t.Fatalf("swept files %d times and purges %d", s.files, s.runs)
		}
	})

	// Purges remove profiles whose consent was withdrawn: a file sweep that
	// takes its whole time must not leave them an expired context. Each
	// gets its own timeout, from when it starts.
	t.Run("gives purges their own timeout", func(t *testing.T) {
		s := &sweepLog{filesTook: 20 * time.Millisecond}
		task := newTask(newQueue(), s, 1)
		task.sweep(context.Background())
		if s.filesBy.IsZero() || !s.purgesBy.After(s.filesBy) || s.purgesBy.Sub(s.filesBy) < s.filesTook {
			t.Fatalf("files had until %s, purges until %s", s.filesBy, s.purgesBy)
		}
	})
}
