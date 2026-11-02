package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// snapshotKind is a fakeKind that reads its tables at one snapshot of its
// source, which it names in the settings it binds: s1, s2, and so on.
type snapshotKind struct {
	fakeKind
	// refuse fails SourceSnapshot; apart makes Close say a table was read apart.
	refuse error
	apart  bool
	opened int
	closed int
	// fail fails the next read of each table it names.
	fail map[string]error
}

func (k *snapshotKind) SourceSnapshot(_ context.Context, src connector.Source) (connector.SourceSnapshot, error) {
	if k.refuse != nil {
		return connector.SourceSnapshot{}, k.refuse
	}
	k.opened++
	src.Settings = json.RawMessage(fmt.Sprintf(`{"snapshot":"s%d"}`, k.opened))
	return connector.SourceSnapshot{Source: src, Close: func() bool {
		k.closed++
		return !k.apart
	}}, nil
}

func (k *snapshotKind) Sync(ctx context.Context, src connector.Source, table connector.Table, w connector.RowWriter) (connector.Table, error) {
	if err := k.fail[table.Name]; err != nil {
		delete(k.fail, table.Name)
		k.synced, k.sources = append(k.synced, table.Name), append(k.sources, src)
		return connector.Table{}, err
	}
	return k.fakeKind.Sync(ctx, src, table, w)
}

// twoTables is a snapshotKind with tables a and b, a row each.
func twoTables() *snapshotKind {
	return &snapshotKind{fakeKind: fakeKind{
		discovery: connector.Discovery{Tables: []connector.Table{{Name: "a", PrimaryKey: []string{"id"}}, {Name: "b", PrimaryKey: []string{"id"}}}},
		rows:      map[string][]string{"a": {`{"id":1}`}, "b": {`{"id":1}`}},
	}}
}

// readWith is the settings each table's read ran with, in order.
func (k *snapshotKind) readWith() []string {
	var out []string
	for _, src := range k.sources[len(k.sources)-len(k.synced):] {
		out = append(out, string(src.Settings))
	}
	return out
}

func sourceSnapshotCount(outcome string) float64 {
	return testutil.ToFloat64(sourceSnapshots.WithLabelValues("fake", outcome))
}

func TestSyncerSourceSnapshot(t *testing.T) {
	ctx := context.Background()
	t.Run("reads every table at one snapshot, then releases it", func(t *testing.T) {
		kind := twoTables()
		f := newFakeFixture(t, &kind.fakeKind, `{}`)
		f.syncer.Catalog = connector.NewCatalogOf(kind)
		before := sourceSnapshotCount("one")
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if got := kind.readWith(); !slices.Equal(got, []string{`{"snapshot":"s1"}`, `{"snapshot":"s1"}`}) {
			t.Fatalf("tables read with %v, want both at s1", got)
		}
		if kind.opened != 1 || kind.closed != 1 || sourceSnapshotCount("one")-before != 1 {
			t.Fatalf("opened %d, closed %d, counted %v", kind.opened, kind.closed, sourceSnapshotCount("one")-before)
		}
	})
	// A table the first attempt wrote shows that attempt's moment; keeping
	// it beside one the retry reads would mix two.
	t.Run("reads every table again on a retry", func(t *testing.T) {
		kind := twoTables()
		kind.fail = map[string]error{"b": &connector.RefusedError{Message: "the database is restarting"}}
		f := newFakeFixture(t, &kind.fakeKind, `{}`)
		f.syncer.Catalog = connector.NewCatalogOf(kind)
		if _, err := f.syncer.Run(ctx, f.sync); err == nil {
			t.Fatal("the first attempt succeeded, want it to fail after writing a")
		}
		if kind.closed != 1 {
			t.Fatalf("closed %d after the failed attempt, want 1", kind.closed)
		}
		kind.rows["a"] = []string{`{"id":2}`}
		kind.synced = nil
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(kind.synced, []string{"a", "b"}) || !slices.Equal(kind.readWith(), []string{`{"snapshot":"s2"}`, `{"snapshot":"s2"}`}) {
			t.Fatalf("retry read %v with %v, want a and b at s2", kind.synced, kind.readWith())
		}
		if got := f.snapshotRows(t, "a"); !slices.Equal(got, []string{`{"id":2}`}) {
			t.Fatalf("a = %v, want the retry's rows", got)
		}
	})
	t.Run("counts no outcome for a read that failed", func(t *testing.T) {
		kind := twoTables()
		kind.fail = map[string]error{"a": &connector.RefusedError{Message: "the database is restarting"}}
		f := newFakeFixture(t, &kind.fakeKind, `{}`)
		f.syncer.Catalog = connector.NewCatalogOf(kind)
		before := sourceSnapshotCount("one")
		if _, err := f.syncer.Run(ctx, f.sync); err == nil {
			t.Fatal("the read succeeded")
		}
		if kind.closed != 1 || sourceSnapshotCount("one") != before {
			t.Fatalf("closed %d, counted %v", kind.closed, sourceSnapshotCount("one")-before)
		}
	})
	t.Run("says when the source refused a table the snapshot", func(t *testing.T) {
		kind := twoTables()
		kind.apart = true
		f := newFakeFixture(t, &kind.fakeKind, `{}`)
		f.syncer.Catalog = connector.NewCatalogOf(kind)
		before := sourceSnapshotCount("apart")
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if got := sourceSnapshotCount("apart") - before; got != 1 || len(f.store.tables) != 2 {
			t.Fatalf("counted %v apart, tables %v", got, f.store.tables)
		}
	})
	// A source that takes no snapshot reads as it did before snapshots, and a
	// retry keeps what the attempt before it wrote.
	t.Run("reads a table at a time when the source takes no snapshot", func(t *testing.T) {
		kind := twoTables()
		kind.refuse = errors.New("pg_export_snapshot is not supported")
		kind.fail = map[string]error{"b": &connector.RefusedError{Message: "the database is restarting"}}
		f := newFakeFixture(t, &kind.fakeKind, `{}`)
		f.syncer.Catalog = connector.NewCatalogOf(kind)
		before := sourceSnapshotCount("unavailable")
		if _, err := f.syncer.Run(ctx, f.sync); err == nil {
			t.Fatal("the first attempt succeeded, want it to fail after writing a")
		}
		kind.synced = nil
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(kind.synced, []string{"b"}) || !slices.Equal(kind.readWith(), []string{`{}`}) {
			t.Fatalf("retry read %v with %v, want only b, unbound", kind.synced, kind.readWith())
		}
		if got := sourceSnapshotCount("unavailable") - before; got != 1 {
			t.Fatalf("counted %v unavailable, want the finished read only", got)
		}
	})
	t.Run("takes no snapshot to read one table", func(t *testing.T) {
		kind := twoTables()
		f := newFakeFixture(t, &kind.fakeKind, `{"tables":["a"]}`)
		f.syncer.Catalog = connector.NewCatalogOf(kind)
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if kind.opened != 0 || !slices.Equal(kind.synced, []string{"a"}) {
			t.Fatalf("opened %d, read %v", kind.opened, kind.synced)
		}
	})
}
