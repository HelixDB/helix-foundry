package snapshot

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// snapshotFile writes rows as a previous snapshot would sit on disk.
func snapshotFile(t *testing.T, rows ...string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for _, r := range rows {
		_, _ = gz.Write([]byte(r + "\n"))
	}
	_ = gz.Close()
	path := filepath.Join(t.TempDir(), "prev.jsonl.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// written fills a new writer with rows and hashes it by key.
func written(t *testing.T, key []string, rows ...map[string]any) *Writer {
	t.Helper()
	w, err := NewWriter(t.TempDir(), 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	for _, r := range rows {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Hash(key); err != nil {
		t.Fatal(err)
	}
	return w
}

func newBatch(t *testing.T) *BatchWriter {
	t.Helper()
	b, err := NewBatchWriter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// batchLines reads a batch back as its JSON lines, sorted for comparison.
func batchLines(t *testing.T, b *BatchWriter) []string {
	t.Helper()
	if err := b.gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.buf.Flush(); err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := eachLine(b.file.Name(), func(line []byte) error {
		lines = append(lines, string(line))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(lines)
	return lines
}

func writerLines(t *testing.T, w *Writer) []string {
	t.Helper()
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := eachLine(w.file.Name(), func(line []byte) error {
		lines = append(lines, string(line))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(lines)
	return lines
}

func TestDigest(t *testing.T) {
	key := []string{"id"}
	// A source may page its rows in any order; the same rows must read as the same table.
	t.Run("ignores row order", func(t *testing.T) {
		a := written(t, key, map[string]any{"id": 1, "n": "a"}, map[string]any{"id": 2, "n": "b"})
		b := written(t, key, map[string]any{"id": 2, "n": "b"}, map[string]any{"id": 1, "n": "a"})
		if !bytes.Equal(Digest(a.pairs, true), Digest(b.pairs, true)) {
			t.Fatal("digests differ for the same rows")
		}
	})
	t.Run("moves when a value changes", func(t *testing.T) {
		a := written(t, key, map[string]any{"id": 1, "n": "a"})
		b := written(t, key, map[string]any{"id": 1, "n": "z"})
		if bytes.Equal(Digest(a.pairs, true), Digest(b.pairs, true)) {
			t.Fatal("digests match for changed rows")
		}
	})
	// A database returns 1 as 1 and an API as 1.0 decoded to a float; both are one key.
	t.Run("hashes equal keys equally whatever their encoding", func(t *testing.T) {
		fromJSON, _, err := pairOf([]byte(`{"id":1,"n":"a"}`), key)
		if err != nil {
			t.Fatal(err)
		}
		fromGo, err := KeyHash(key, map[string]any{"id": json.Number("1")})
		if err != nil || fromJSON.Key != fromGo {
			t.Fatalf("keys differ: %x vs %x (%v)", fromJSON.Key, fromGo, err)
		}
		escaped, _, _ := pairOf([]byte(`{"id":"a<b"}`), key)
		plain, _, _ := pairOf([]byte(`{"id":"a<b"}`), key)
		if escaped.Key != plain.Key {
			t.Fatal("an escaped string hashed differently")
		}
	})
}

func TestCompare(t *testing.T) {
	key := []string{"id"}
	// The batch is what an engine applying changes would read: new rows whole, deleted rows by key.
	t.Run("finds inserts, updates and deletes", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":1,"n":"a"}`, `{"id":2,"n":"b"}`, `{"id":3,"n":"c"}`)
		w := written(t, key, map[string]any{"id": 1, "n": "a"}, map[string]any{"id": 2, "n": "B"}, map[string]any{"id": 4, "n": "d"})
		batch := newBatch(t)
		counts, err := Compare(prev, w, false, batch)
		if err != nil {
			t.Fatal(err)
		}
		if counts != (Counts{Inserts: 1, Updates: 1, Deletes: 1}) {
			t.Fatalf("counts = %+v", counts)
		}
		want := []string{
			`{"op":"delete","key":{"id":3}}`,
			`{"op":"upsert","key":{"id":2},"row":{"id":2,"n":"B"}}`,
			`{"op":"upsert","key":{"id":4},"row":{"id":4,"n":"d"}}`,
		}
		if got := batchLines(t, batch); !slices.Equal(got, want) {
			t.Fatalf("batch = %v", got)
		}
	})
	t.Run("finds nothing in an unchanged table", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":1,"n":"a"}`)
		counts, err := Compare(prev, written(t, key, map[string]any{"id": 1, "n": "a"}), false, newBatch(t))
		if err != nil || counts.Changed() {
			t.Fatalf("Compare = (%+v, %v)", counts, err)
		}
	})
	// Rows leave a time window while they still exist at the source.
	t.Run("reports no deletes for a windowed table", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":1,"n":"a"}`, `{"id":2,"n":"b"}`)
		counts, err := Compare(prev, written(t, key, map[string]any{"id": 2, "n": "b"}), true, newBatch(t))
		if err != nil || counts.Changed() {
			t.Fatalf("Compare = (%+v, %v)", counts, err)
		}
	})
	// A source paging while rows arrive can return a row twice; its last copy is the row.
	t.Run("takes the last copy of a repeated key", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":1,"n":"a"}`)
		w := written(t, key, map[string]any{"id": 1, "n": "a"}, map[string]any{"id": 1, "n": "b"})
		counts, err := Compare(prev, w, false, newBatch(t))
		if err != nil || counts != (Counts{Updates: 1}) {
			t.Fatalf("Compare = (%+v, %v)", counts, err)
		}
	})
}

func TestMerge(t *testing.T) {
	key := []string{"id"}
	mergeVersioned := func(t *testing.T, prev string, set *ChangeSet, version string) (*Writer, Counts, []string) {
		t.Helper()
		w, err := NewWriter(t.TempDir(), 100, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		w.key = key
		batch := newBatch(t)
		counts, err := Merge(prev, set, w, batch, version, nil)
		if err != nil {
			t.Fatal(err)
		}
		return w, counts, batchLines(t, batch)
	}
	mergeInto := func(t *testing.T, prev string, set *ChangeSet) (*Writer, Counts, []string) {
		t.Helper()
		return mergeVersioned(t, prev, set, "")
	}
	newSet := func(t *testing.T) *ChangeSet {
		t.Helper()
		set, err := NewChangeSet(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = set.Close() })
		return set
	}
	// Merging must leave the table a full read would have produced.
	t.Run("applies upserts and deletes onto the previous rows", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":1,"n":"a"}`, `{"id":2,"n":"b"}`, `{"id":3,"n":"c"}`)
		set := newSet(t)
		_ = set.Upsert(map[string]any{"id": 2, "n": "B"})
		_ = set.Upsert(map[string]any{"id": 4, "n": "d"})
		_ = set.Delete(map[string]any{"id": 3})
		w, counts, batch := mergeInto(t, prev, set)
		if counts != (Counts{Inserts: 1, Updates: 1, Deletes: 1}) {
			t.Fatalf("counts = %+v", counts)
		}
		if got := writerLines(t, w); !slices.Equal(got, []string{`{"id":1,"n":"a"}`, `{"id":2,"n":"B"}`, `{"id":4,"n":"d"}`}) {
			t.Fatalf("rows = %v", got)
		}
		if len(batch) != 3 {
			t.Fatalf("batch = %v", batch)
		}
	})
	// A replayed export carries rows older than the ones stored; they must not
	// undo newer details, and an old delete must not remove a newer row.
	t.Run("keeps a stored row newer than the change", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":1,"n":"new","v":5}`, `{"id":2,"n":"kept","v":7}`, `{"id":3,"n":"old","v":1}`)
		set := newSet(t)
		_ = set.Upsert(map[string]any{"id": 1, "n": "stale", "v": 4})
		_ = set.DeleteVersion(map[string]any{"id": 2}, 6)
		_ = set.Upsert(map[string]any{"id": 3, "n": "newer", "v": 2})
		w, counts, batch := mergeVersioned(t, prev, set, "v")
		want := []string{`{"id":1,"n":"new","v":5}`, `{"id":2,"n":"kept","v":7}`, `{"id":3,"n":"newer","v":2}`}
		if got := writerLines(t, w); counts != (Counts{Updates: 1}) || !slices.Equal(got, want) || len(batch) != 1 {
			t.Fatalf("counts = %+v, rows = %v, batch = %v", counts, got, batch)
		}
		set = newSet(t)
		_ = set.DeleteVersion(map[string]any{"id": 2}, 7)
		if _, counts, _ := mergeVersioned(t, prev, set, "v"); counts != (Counts{Deletes: 1}) {
			t.Fatalf("a delete as new as the row: counts = %+v", counts)
		}
	})
	// The last change to a key is the source's state, whatever came before it in the same read.
	t.Run("keeps the last change to a key", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":1,"n":"a"}`)
		set := newSet(t)
		_ = set.Upsert(map[string]any{"id": 1, "n": "x"})
		_ = set.Delete(map[string]any{"id": 1})
		_ = set.Upsert(map[string]any{"id": 1, "n": "y"})
		w, counts, _ := mergeInto(t, prev, set)
		if counts != (Counts{Updates: 1}) || !slices.Equal(writerLines(t, w), []string{`{"id":1,"n":"y"}`}) {
			t.Fatalf("counts = %+v", counts)
		}
	})
	// Overlapping reads and re-fetches return rows that did not change; they must not refresh a graph.
	t.Run("counts a row read again unchanged as no change", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":1,"n":"a"}`)
		set := newSet(t)
		_ = set.Upsert(map[string]any{"id": 1, "n": "a"})
		_, counts, batch := mergeInto(t, prev, set)
		if counts.Changed() || len(batch) != 0 {
			t.Fatalf("counts = %+v, batch = %v", counts, batch)
		}
	})
	t.Run("ignores a delete of a row that was never there", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":1,"n":"a"}`)
		set := newSet(t)
		_ = set.Delete(map[string]any{"id": 9})
		_, counts, _ := mergeInto(t, prev, set)
		if counts.Changed() {
			t.Fatalf("counts = %+v", counts)
		}
	})
	// WorkOS sends no member events when a group goes; the cascade removes them by column.
	t.Run("deletes every row a cascade names", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":1,"group":"g1"}`, `{"id":2,"group":"g1"}`, `{"id":3,"group":"g2"}`)
		set := newSet(t)
		_ = set.DeleteWhere("group", "g1")
		w, counts, batch := mergeInto(t, prev, set)
		if counts != (Counts{Deletes: 2}) || !slices.Equal(writerLines(t, w), []string{`{"id":3,"group":"g2"}`}) {
			t.Fatalf("counts = %+v, rows = %v", counts, writerLines(t, w))
		}
		if !strings.Contains(strings.Join(batch, "\n"), `"key":{"id":1}`) {
			t.Fatalf("batch = %v", batch)
		}
	})
	// A row the cascade names but the set upserts again survives: the upsert is newer.
	t.Run("keeps an upsert over a cascade", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":1,"group":"g1"}`)
		set := newSet(t)
		_ = set.DeleteWhere("group", "g1")
		_ = set.Upsert(map[string]any{"id": 1, "group": "g1", "n": "back"})
		w, _, _ := mergeInto(t, prev, set)
		if got := writerLines(t, w); len(got) != 1 || !strings.Contains(got[0], "back") {
			t.Fatalf("rows = %v", got)
		}
	})
	// A database's own JSON is kept byte for byte, so its hash matches a full read's.
	t.Run("keeps a database's row bytes", func(t *testing.T) {
		prev := snapshotFile(t, `{"id": 1, "n": "a"}`)
		set := newSet(t)
		_ = set.UpsertJSON([]byte(`{"id": 1, "n": "a"}`))
		_, counts, _ := mergeInto(t, prev, set)
		if counts.Changed() {
			t.Fatalf("counts = %+v", counts)
		}
	})
}

func TestMergeDense(t *testing.T) {
	key := []string{"id"}
	// merge applies set to prev with these dense columns, and returns the
	// rows, their digest, the counts and the batch lines.
	merge := func(t *testing.T, prev string, rows []string, version string, dense []string) ([]string, []byte, Counts, []string) {
		t.Helper()
		set, err := NewChangeSet(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = set.Close() })
		for _, r := range rows {
			if err := set.UpsertJSON([]byte(r)); err != nil {
				t.Fatal(err)
			}
		}
		w, err := NewWriter(t.TempDir(), 100, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		w.SetKey(key)
		batch := newBatch(t)
		counts, err := Merge(prev, set, w, batch, version, dense)
		if err != nil {
			t.Fatal(err)
		}
		pairs, err := w.Hash(key)
		if err != nil {
			t.Fatal(err)
		}
		return writerLines(t, w), Digest(pairs, true), counts, batchLines(t, batch)
	}
	// The engine catalogs a table from its first rows, which a merge copies
	// first: a column missing from the oldest rows would never be cataloged.
	// Padding changes no row's content, so it is no change.
	t.Run("pads every previous row to the declared columns", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":"a","x":1}`, `{"id":"b","x":2,"y":"s"}`, `{"id": "c"} `)
		insert := `{"id":"d","x":3,"y":null,"z":null}`
		dense := []string{"id", "x", "y", "z"}
		rows, digest, counts, batch := merge(t, prev, []string{insert}, "", dense)
		want := []string{
			`{"id": "c","x":null,"y":null,"z":null}`,
			`{"id":"a","x":1,"y":null,"z":null}`,
			`{"id":"b","x":2,"y":"s","z":null}`,
			insert,
		}
		if !slices.Equal(rows, want) {
			t.Fatalf("rows = %v", rows)
		}
		plainRows, plainDigest, plainCounts, plainBatch := merge(t, prev, []string{insert}, "", nil)
		if counts != plainCounts || counts != (Counts{Inserts: 1}) || !slices.Equal(batch, plainBatch) || len(batch) != 1 {
			t.Fatalf("counts = %+v against %+v, batch = %v against %v", counts, plainCounts, batch, plainBatch)
		}
		if bytes.Equal(digest, plainDigest) || slices.Equal(rows, plainRows) {
			t.Fatal("padding left the table's digest as it was")
		}
	})
	// A field name is the sender's text; written raw, a quote would end the
	// name early and corrupt the row.
	t.Run("escapes a column's name", func(t *testing.T) {
		name := `we"ird\<b>`
		rows, _, _, _ := merge(t, snapshotFile(t, `{"id":"a"}`), []string{`{"id":"b"}`}, "", []string{"id", name})
		if rows[0] != `{"id":"a","we\"ird\\<b>":null}` {
			t.Fatalf("padded row = %s", rows[0])
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(rows[0]), &fields); err != nil {
			t.Fatal(err)
		}
		if v, ok := fields[name]; !ok || v != nil {
			t.Fatalf("fields = %v", fields)
		}
	})
	// A row that already carries a column, null or not, keeps its bytes, and
	// an empty row gains no stray comma.
	t.Run("pads only the columns a row lacks", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":"a","x":null}`, `{}`, `{ "id":"w" }`)
		rows, _, counts, _ := merge(t, prev, []string{`{"id":"z","x":0}`}, "", []string{"id", "x"})
		want := []string{`{ "id":"w" ,"x":null}`, `{"id":"a","x":null}`, `{"id":"z","x":0}`, `{"id":null,"x":null}`}
		if !slices.Equal(rows, want) || counts != (Counts{Inserts: 1}) {
			t.Fatalf("rows = %v, counts = %+v", rows, counts)
		}
	})
	// A change read again after the table learned a column carries it as
	// null; it must publish, or the stored rows would stay unpadded.
	t.Run("counts a change that only adds padding as an update", func(t *testing.T) {
		rows, _, counts, batch := merge(t, snapshotFile(t, `{"id":"a","x":1}`), []string{`{"id":"a","x":1,"y":null}`}, "", []string{"id", "x", "y"})
		if !slices.Equal(rows, []string{`{"id":"a","x":1,"y":null}`}) || counts != (Counts{Updates: 1}) || len(batch) != 1 {
			t.Fatalf("rows = %v, counts = %+v, batch = %v", rows, counts, batch)
		}
	})
	// A stored row kept over an older change is a previous row too.
	t.Run("pads a stored row kept over an older change", func(t *testing.T) {
		prev := snapshotFile(t, `{"id":1,"v":5}`)
		rows, _, counts, batch := merge(t, prev, []string{`{"id":1,"n":null,"v":4}`}, "v", []string{"id", "n", "v"})
		if !slices.Equal(rows, []string{`{"id":1,"v":5,"n":null}`}) || counts.Changed() || len(batch) != 0 {
			t.Fatalf("rows = %v, counts = %+v, batch = %v", rows, counts, batch)
		}
	})
	// A null line parses as no fields; padding it would write a broken row.
	t.Run("refuses a previous row that is not an object", func(t *testing.T) {
		set, err := NewChangeSet(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = set.Close() })
		_ = set.UpsertJSON([]byte(`{"id":"b"}`))
		w := written(t, key)
		if _, err := Merge(snapshotFile(t, `null`), set, w, newBatch(t), "", []string{"id"}); err == nil || !strings.Contains(err.Error(), "not a JSON object") {
			t.Fatalf("Merge = %v", err)
		}
	})
	// Padding needs each row's parsed fields, which only a keyed merge has.
	t.Run("refuses dense columns on a keyless table", func(t *testing.T) {
		set, err := NewChangeSet(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = set.Close() })
		w := written(t, nil)
		if _, err := Merge(snapshotFile(t, `{"a":1}`), set, w, newBatch(t), "", []string{"a"}); err == nil || !strings.Contains(err.Error(), "need the table's key") {
			t.Fatalf("Merge = %v", err)
		}
	})
}
