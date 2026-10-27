package enrichment

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// sweepDB answers the sweep's listings with what it holds, and records what
// the sweep did.
type sweepDB struct {
	sweeps      []model.FoundryEnrichmentSweep
	callsBefore time.Time
	superseded  time.Time
	wholeBefore time.Time
	cleared     []string
	purges      []*model.FoundryEnrichmentPurge
	advance     map[string]model.FoundryEnrichmentPurgeStep
	queued      []string
}

func (d *sweepDB) ListFoundryEnrichmentSweeps(_ context.Context, callsBefore, supersededBefore, wholeBefore time.Time, _ int) ([]model.FoundryEnrichmentSweep, error) {
	d.callsBefore, d.superseded, d.wholeBefore = callsBefore, supersededBefore, wholeBefore
	return d.sweeps, nil
}

func (d *sweepDB) ClearFoundryEnrichmentFiles(_ context.Context, sw model.FoundryEnrichmentSweep) error {
	d.cleared = append(d.cleared, sw.LoadID)
	return nil
}

func (d *sweepDB) ListFoundryEnrichmentPurges(_ context.Context, maxAttempts, _ int) ([]*model.FoundryEnrichmentPurge, error) {
	if maxAttempts != purgeAttempts {
		return nil, errBoom
	}
	return d.purges, nil
}

func (d *sweepDB) AdvanceFoundryEnrichmentPurge(_ context.Context, ontologyID string, load *model.FoundryGraphJob, deleteJobID string,
	retryAfter time.Duration, maxAttempts int) (model.FoundryEnrichmentPurgeStep, error) {
	if retryAfter != 24*time.Hour || maxAttempts != 3 || load.ID == "" || deleteJobID == "" || load.ID == deleteJobID {
		return "", errBoom
	}
	step, ok := d.advance[ontologyID]
	if !ok {
		return "", store.ErrNotFound
	}
	if step == model.FoundryEnrichmentPurgeQueued {
		d.queued = append(d.queued, load.ID)
	}
	return step, nil
}

func TestSweeper(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ids := 0
	newSweeper := func(db *sweepDB, store *memStore) *Sweeper {
		return &Sweeper{
			DB: db, Store: store, Cfg: config.EnrichmentConfig{RawRetentionDays: 90}, GraphLease: 2 * time.Minute,
			Now:   func() time.Time { return now },
			NewID: func() string { ids++; return string(rune('a' + ids)) },
		}
	}

	// Each ended step loses exactly what the provider says it no longer
	// needs, batch files included, and only then are its keys cleared; the
	// raw calls are kept for the retention, and results for a day past a
	// newer step's.
	t.Run("deletes what ended steps no longer need", func(t *testing.T) {
		store := newMemStore()
		l := LoadLayout("ws", "ont", "load-1")
		keep := LoadLayout("ws", "ont", "load-2")
		for _, key := range []string{l.Plan(), l.BatchResults(0), l.BatchResults(1), l.Calls(0), l.Calls(1), l.Carried(), keep.Plan(), keep.Calls(0)} {
			store.objects[key] = []byte("x")
		}
		db := &sweepDB{sweeps: []model.FoundryEnrichmentSweep{
			{LoadID: "load-1", WorkspaceID: "ws", OntologyID: "ont", Batches: 2, Plan: ptr(l.Plan()), Carried: ptr(l.Carried()), Calls: true},
		}}
		cleared, err := newSweeper(db, store).Files(context.Background())
		if err != nil || cleared != 1 || !slices.Equal(db.cleared, []string{"load-1"}) {
			t.Fatalf("cleared %d %v, %v", cleared, db.cleared, err)
		}
		var left []string
		for key := range store.objects {
			left = append(left, key)
		}
		slices.Sort(left)
		if want := []string{keep.Calls(0), keep.Plan()}; !slices.Equal(left, want) {
			t.Fatalf("left %v, want %v", left, want)
		}
		if !db.callsBefore.Equal(now.AddDate(0, 0, -90)) || !db.superseded.Equal(now.Add(-24*time.Hour)) {
			t.Fatalf("listed calls before %s and superseded before %s", db.callsBefore, db.superseded)
		}
	})

	// A step purged as its ontology was deleted mid-prepare may have had
	// its plan and carried files written by an attempt that lost its job,
	// which its row never names: the purge deletes every file of its
	// layout, and nothing of another load's. That attempt writes until its
	// lease renewal stops it, so whole layouts are listed only a graph
	// lease past their step's end.
	t.Run("deletes a purged step's files its row never named", func(t *testing.T) {
		store := newMemStore()
		l := LoadLayout("ws", "ont", "load-1")
		keep := LoadLayout("ws", "ont", "load-2")
		for _, key := range []string{l.Plan(), l.Carried(), l.Results(), l.Profiles(), keep.Plan(), keep.Carried()} {
			store.objects[key] = []byte("x")
		}
		db := &sweepDB{sweeps: []model.FoundryEnrichmentSweep{
			{LoadID: "load-1", WorkspaceID: "ws", OntologyID: "ont", Carried: ptr(l.Carried()), Calls: true, WholeLayout: true},
		}}
		cleared, err := newSweeper(db, store).Files(context.Background())
		if err != nil || cleared != 1 {
			t.Fatalf("cleared %d, %v", cleared, err)
		}
		var left []string
		for key := range store.objects {
			left = append(left, key)
		}
		slices.Sort(left)
		if want := []string{keep.Carried(), keep.Plan()}; !slices.Equal(left, want) {
			t.Fatalf("left %v, want %v", left, want)
		}
		if !db.wholeBefore.Equal(now.Add(-2 * time.Minute)) {
			t.Fatalf("listed whole layouts of steps ended before %s", db.wholeBefore)
		}
	})

	// A step whose files could not all be deleted keeps its keys, so the
	// next sweep tries again.
	t.Run("keeps the keys of a step it could not clear", func(t *testing.T) {
		store := &failingDelete{memStore: newMemStore()}
		db := &sweepDB{sweeps: []model.FoundryEnrichmentSweep{{LoadID: "load-1", WorkspaceID: "ws", OntologyID: "ont", Plan: ptr("plan")}}}
		s := newSweeper(db, nil)
		s.Store = store
		if cleared, err := s.Files(context.Background()); err != nil || cleared != 0 || len(db.cleared) != 0 {
			t.Fatalf("cleared %d %v, %v", cleared, db.cleared, err)
		}
	})

	// Purges wait past a live job, queue a clean load with ids of their
	// own, and give up loudly after the last attempt; one done meanwhile is
	// skipped.
	t.Run("moves purges on", func(t *testing.T) {
		db := &sweepDB{
			purges: []*model.FoundryEnrichmentPurge{{OntologyID: "a"}, {OntologyID: "b"}, {OntologyID: "c"}, {OntologyID: "gone"}},
			advance: map[string]model.FoundryEnrichmentPurgeStep{
				"a": model.FoundryEnrichmentPurgeWaiting, "b": model.FoundryEnrichmentPurgeQueued, "c": model.FoundryEnrichmentPurgeExhausted,
			},
		}
		steps, err := newSweeper(db, newMemStore()).Purges(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := map[model.FoundryEnrichmentPurgeStep]int{
			model.FoundryEnrichmentPurgeWaiting: 1, model.FoundryEnrichmentPurgeQueued: 1, model.FoundryEnrichmentPurgeExhausted: 1,
		}
		if !maps.Equal(steps, want) || len(db.queued) != 1 {
			t.Fatalf("steps = %v, queued %v", steps, db.queued)
		}
	})
}

// failingDelete is a bucket that refuses deletes.
type failingDelete struct{ *memStore }

func (failingDelete) Delete(context.Context, string) error { return errBoom }
