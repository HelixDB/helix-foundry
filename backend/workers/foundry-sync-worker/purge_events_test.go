package foundrysyncworker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// eventPurges keeps the deleted event sources a purge sees as the provider
// does, and is also the bucket their objects are in. purgeable is the
// batches of every purgeable source in (source, seq) order, and deleted the
// purgeable sources in the order they were deleted. Every delete, of an
// object, a folder or a row, is logged to deletes in order, and refuse fails
// those it names, as logged, or a list ("list batches", "list sources",
// "prune").
type eventPurges struct {
	purgeable []*model.FoundryEventBatch
	deleted   []model.FoundryEventSourcePurge
	refuse    map[string]error
	deletes   []string
	// bounds are the bound, cursor and limit of every list and of the prune.
	bounds []purgeBound
	// expiredSyncs is how many syncs the prune deletes.
	expiredSyncs int
}

type purgeBound struct {
	call   string
	before time.Time
	// after is the connector the sources' list went on after.
	after string
	limit int
}

// logged logs a delete and fails it if refuse names it.
func (p *eventPurges) logged(what string) error {
	p.deletes = append(p.deletes, what)
	return p.refuse[what]
}

func (p *eventPurges) ListFoundryEventBatchPurges(_ context.Context, before time.Time, limit int) ([]*model.FoundryEventBatch, error) {
	p.bounds = append(p.bounds, purgeBound{call: "batches", before: before, limit: limit})
	if err := p.refuse["list batches"]; err != nil {
		return nil, err
	}
	return slices.Clone(p.purgeable[:min(limit, len(p.purgeable))]), nil
}

func (p *eventPurges) DeleteFoundryEventBatch(_ context.Context, id string, _ time.Time) error {
	if err := p.logged("row " + id); err != nil {
		return err
	}
	p.purgeable = slices.DeleteFunc(p.purgeable, func(b *model.FoundryEventBatch) bool { return b.ID == id })
	return nil
}

// ListFoundryEventSourcePurges lists, as the provider does, the deleted
// sources after the cursor in the order they were deleted, each saying
// whether it has batches left.
func (p *eventPurges) ListFoundryEventSourcePurges(_ context.Context, before time.Time, after model.FoundryEventSourcePurge, limit int) (
	[]model.FoundryEventSourcePurge, error) {
	p.bounds = append(p.bounds, purgeBound{call: "sources", before: before, after: after.ConnectorID, limit: limit})
	if err := p.refuse["list sources"]; err != nil {
		return nil, err
	}
	var out []model.FoundryEventSourcePurge
	for _, s := range p.deleted {
		later := s.DeletedAt.After(after.DeletedAt) || s.DeletedAt.Equal(after.DeletedAt) && s.ConnectorID > after.ConnectorID
		if after.ConnectorID != "" && !later || len(out) == limit {
			continue
		}
		s.Batches = slices.ContainsFunc(p.purgeable, func(b *model.FoundryEventBatch) bool { return b.ConnectorID == s.ConnectorID })
		out = append(out, s)
	}
	return out, nil
}

func (p *eventPurges) FinishFoundryEventSourcePurge(_ context.Context, s model.FoundryEventSourcePurge, _ time.Time) error {
	return p.logged("finish " + s.ConnectorID)
}

func (p *eventPurges) DeleteExpiredEventSyncs(_ context.Context, before time.Time, limit int) (int, error) {
	p.bounds = append(p.bounds, purgeBound{call: "syncs", before: before, limit: limit})
	if err := p.refuse["prune"]; err != nil {
		return 0, err
	}
	return p.expiredSyncs, nil
}

func (p *eventPurges) Delete(_ context.Context, key string) error { return p.logged("object " + key) }

func (p *eventPurges) DeletePrefix(_ context.Context, prefix string) error {
	return p.logged("prefix " + prefix)
}

// purgeTask is a task whose queue and bucket are p, with the grace an hour
// and the change retention a day.
func purgeTask(p *eventPurges) *Task {
	q := newQueue()
	q.eventPurges = p
	task := newTask(q, nil)
	task.snapshots = p
	return task
}

// purgeBatches are n batches, seqs 1 to n, of deleted source connectorID of
// workspace W, each with its object under the source's folder.
func purgeBatches(connectorID string, n int) []*model.FoundryEventBatch {
	out := make([]*model.FoundryEventBatch, n)
	for i := range out {
		id := fmt.Sprintf("%s-%d", connectorID, i+1)
		out[i] = &model.FoundryEventBatch{ID: id, WorkspaceID: "W", ConnectorID: connectorID, Seq: int64(i + 1),
			S3Key: "ws/W/events/" + connectorID + "/" + id + ".jsonl.gz"}
	}
	return out
}

// purgeSources are deleted sources of workspace W, deleted a minute apart
// in the order given, the last two hours ago.
func purgeSources(connectorIDs ...string) []model.FoundryEventSourcePurge {
	out := make([]model.FoundryEventSourcePurge, len(connectorIDs))
	for i, c := range connectorIDs {
		out[i] = model.FoundryEventSourcePurge{WorkspaceID: "W", ConnectorID: c,
			DeletedAt: now.Add(-2*time.Hour - time.Duration(len(connectorIDs)-i)*time.Minute)}
	}
	return out
}

func TestPurgeEventSources(t *testing.T) {
	// A batch's row is what names its object to delete, so the row goes only
	// after its object. A source's folder is emptied of orphans only once
	// none of its rows is left, and its tokens go last. A source that never
	// held a batch or a token may still hold an orphan, a send whose commit
	// failed, so the purge empties its folder too.
	t.Run("deletes each batch's object then its row, then every emptied source's folder and tokens", func(t *testing.T) {
		p := &eventPurges{purgeable: append(purgeBatches("A", 2), purgeBatches("B", 1)...), deleted: purgeSources("A", "B", "C")}
		task := purgeTask(p)
		task.purgeEvents(context.Background())
		want := []string{
			"object ws/W/events/A/A-1.jsonl.gz", "row A-1", "object ws/W/events/A/A-2.jsonl.gz", "row A-2",
			"object ws/W/events/B/B-1.jsonl.gz", "row B-1",
			"prefix ws/W/events/A/", "finish A", "prefix ws/W/events/B/", "finish B", "prefix ws/W/events/C/", "finish C",
		}
		if !slices.Equal(p.deletes, want) {
			t.Fatalf("deletes = %v, want %v", p.deletes, want)
		}
		if len(p.purgeable) != 0 || task.sourcesPurged.ConnectorID != "C" {
			t.Fatalf("left %d batches, cursor at %q", len(p.purgeable), task.sourcesPurged.ConnectorID)
		}

		// Nothing stored says a folder is done, so the pass's cursor does:
		// the next tick lists only sources deleted since.
		p.deletes, p.bounds = nil, nil
		p.deleted = append(p.deleted, model.FoundryEventSourcePurge{WorkspaceID: "W", ConnectorID: "D", DeletedAt: now.Add(-90 * time.Minute)})
		task.purgeEvents(context.Background())
		if want := []string{"prefix ws/W/events/D/", "finish D"}; !slices.Equal(p.deletes, want) {
			t.Fatalf("next tick deletes = %v, want %v", p.deletes, want)
		}
		if p.bounds[1].after != "C" {
			t.Fatalf("the sources were listed after %q, want C", p.bounds[1].after)
		}
	})

	// A source deleted moments ago may still be read by a sync or restored
	// by hand, so only sources deleted past the grace are listed. One tick
	// deletes a bounded amount, and a source with more batches than that
	// keeps its folder and tokens until a later tick empties it.
	t.Run("purges past the grace, 500 batches a tick", func(t *testing.T) {
		p := &eventPurges{purgeable: purgeBatches("A", sweepBatch+1), deleted: purgeSources("A")}
		task := purgeTask(p)
		task.purgeEvents(context.Background())
		grace := now.Add(-time.Hour)
		want := []purgeBound{{call: "batches", before: grace, limit: 500}, {call: "sources", before: grace, limit: 500},
			{call: "syncs", before: now.Add(-24 * time.Hour), limit: 500}}
		if !slices.Equal(p.bounds, want) {
			t.Fatalf("bounds = %v, want %v", p.bounds, want)
		}
		if len(p.deletes) != 2*sweepBatch || len(p.purgeable) != 1 || task.sourcesPurged.ConnectorID != "" {
			t.Fatalf("first tick: %d deletes, %d batches left, cursor at %q", len(p.deletes), len(p.purgeable), task.sourcesPurged.ConnectorID)
		}

		p.deletes = nil
		task.purgeEvents(context.Background())
		if want := []string{"object ws/W/events/A/A-501.jsonl.gz", "row A-501", "prefix ws/W/events/A/", "finish A"}; !slices.Equal(p.deletes, want) {
			t.Fatalf("second tick deletes = %v, want %v", p.deletes, want)
		}
	})

	// Every deleted source is listed again by a new worker, so one tick goes
	// through a bounded number of them, the next from where it stopped.
	t.Run("goes through 500 deleted sources a tick, the next tick from where it stopped", func(t *testing.T) {
		ids := make([]string, sweepBatch+1)
		for i := range ids {
			ids[i] = fmt.Sprintf("S%03d", i)
		}
		p := &eventPurges{deleted: purgeSources(ids...)}
		task := purgeTask(p)
		task.purgeEvents(context.Background())
		if len(p.deletes) != 2*sweepBatch || task.sourcesPurged.ConnectorID != ids[sweepBatch-1] {
			t.Fatalf("first tick: %d deletes, cursor at %q", len(p.deletes), task.sourcesPurged.ConnectorID)
		}

		p.deletes, p.bounds = nil, nil
		task.purgeEvents(context.Background())
		if want := []string{"prefix ws/W/events/S500/", "finish S500"}; !slices.Equal(p.deletes, want) || p.bounds[1].after != ids[sweepBatch-1] {
			t.Fatalf("second tick deletes = %v after %q", p.deletes, p.bounds[1].after)
		}
	})

	// A row deleted before its object would leave the object to the folder's
	// delete alone; a batch whose object stays keeps its row, and its source
	// keeps everything, until a later tick deletes the object.
	t.Run("keeps a batch's row and its source while its object stays", func(t *testing.T) {
		p := &eventPurges{purgeable: purgeBatches("A", 2), deleted: purgeSources("A"),
			refuse: map[string]error{"object ws/W/events/A/A-1.jsonl.gz": errors.New("SlowDown")}}
		task := purgeTask(p)
		task.purgeEvents(context.Background())
		if want := []string{"object ws/W/events/A/A-1.jsonl.gz", "object ws/W/events/A/A-2.jsonl.gz", "row A-2"}; !slices.Equal(p.deletes, want) {
			t.Fatalf("deletes = %v, want %v", p.deletes, want)
		}

		p.deletes, p.refuse = nil, nil
		task.purgeEvents(context.Background())
		if want := []string{"object ws/W/events/A/A-1.jsonl.gz", "row A-1", "prefix ws/W/events/A/", "finish A"}; !slices.Equal(p.deletes, want) {
			t.Fatalf("next tick deletes = %v, want %v", p.deletes, want)
		}
	})

	// A row that can't be deleted is listed again next tick, and its object
	// deleted again, which S3 takes as done; its source waits for it.
	t.Run("keeps a source whose batch row stays", func(t *testing.T) {
		p := &eventPurges{purgeable: purgeBatches("A", 2), deleted: purgeSources("A"),
			refuse: map[string]error{"row A-1": errors.New("deadlock")}}
		task := purgeTask(p)
		task.purgeEvents(context.Background())
		want := []string{"object ws/W/events/A/A-1.jsonl.gz", "row A-1", "object ws/W/events/A/A-2.jsonl.gz", "row A-2"}
		if !slices.Equal(p.deletes, want) {
			t.Fatalf("deletes = %v, want %v", p.deletes, want)
		}
		if len(p.purgeable) != 1 || task.sourcesPurged.ConnectorID != "" {
			t.Fatalf("left %v, cursor at %q", p.purgeable, task.sourcesPurged.ConnectorID)
		}
	})

	// A source whose folder or tokens can't be deleted is visited again next
	// tick, so the cursor stops before it; the sources after it are visited
	// all the same, and again next tick, which deletes nothing new.
	for _, refused := range []string{"prefix ws/W/events/A/", "finish A"} {
		t.Run(fmt.Sprintf("visits a source again while %q fails, and goes on past it", refused), func(t *testing.T) {
			p := &eventPurges{deleted: purgeSources("A", "B"), refuse: map[string]error{refused: errors.New("AccessDenied")}}
			task := purgeTask(p)
			task.purgeEvents(context.Background())
			if !slices.Contains(p.deletes, "finish B") || task.sourcesPurged.ConnectorID != "" {
				t.Fatalf("deletes = %v, cursor at %q", p.deletes, task.sourcesPurged.ConnectorID)
			}

			p.deletes, p.refuse = nil, nil
			task.purgeEvents(context.Background())
			want := []string{"prefix ws/W/events/A/", "finish A", "prefix ws/W/events/B/", "finish B"}
			if !slices.Equal(p.deletes, want) || task.sourcesPurged.ConnectorID != "B" {
				t.Fatalf("next tick deletes = %v, cursor at %q", p.deletes, task.sourcesPurged.ConnectorID)
			}
		})
	}

	// Each step reads on its own, so a list that fails holds back neither
	// the next step nor the prune of old syncs.
	t.Run("goes on to the next step past a list that fails", func(t *testing.T) {
		p := &eventPurges{purgeable: purgeBatches("A", 1), deleted: purgeSources("A", "B"),
			refuse: map[string]error{"list batches": errors.New("timeout")}}
		purgeTask(p).purgeEvents(context.Background())
		if want := []string{"prefix ws/W/events/B/", "finish B"}; !slices.Equal(p.deletes, want) {
			t.Fatalf("deletes = %v, want %v", p.deletes, want)
		}

		p = &eventPurges{purgeable: purgeBatches("A", 1), deleted: purgeSources("A", "B"),
			refuse: map[string]error{"list sources": errors.New("timeout")}}
		purgeTask(p).purgeEvents(context.Background())
		if want := []string{"object ws/W/events/A/A-1.jsonl.gz", "row A-1"}; !slices.Equal(p.deletes, want) {
			t.Fatalf("deletes = %v, want %v", p.deletes, want)
		}
		if calls := len(p.bounds); calls != 3 || p.bounds[2].call != "syncs" {
			t.Fatalf("bounds = %v, want the prune after both lists", p.bounds)
		}
	})
}
