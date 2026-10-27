package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
)

// fileChangeLog is a ChangeWriter that keeps what a change read wrote: rows as
// the syncer's change set encodes them.
type fileChangeLog struct {
	noDocuments
	upserts map[string][]string
	deletes map[string][]string
	resets  map[string]string
	// versions are the versions deletes carried, as deletes lists them.
	versions map[string][]any
}

func newFileChangeLog() *fileChangeLog {
	return &fileChangeLog{upserts: map[string][]string{}, deletes: map[string][]string{}, resets: map[string]string{},
		versions: map[string][]any{}}
}

func (c *fileChangeLog) Upsert(table string, row map[string]any) error {
	c.upserts[table] = append(c.upserts[table], encodeFileRow(row))
	return nil
}

func (c *fileChangeLog) UpsertJSON(table string, row []byte) error {
	c.upserts[table] = append(c.upserts[table], string(row))
	return nil
}

func (c *fileChangeLog) Delete(table string, key map[string]any) error {
	c.deletes[table] = append(c.deletes[table], encodeFileRow(key))
	return nil
}

func (c *fileChangeLog) DeleteVersion(table string, key map[string]any, version any) error {
	c.versions[table] = append(c.versions[table], version)
	return c.Delete(table, key)
}

func (c *fileChangeLog) DeleteWhere(table, column string, value any) error {
	c.deletes[table] = append(c.deletes[table], encodeFileRow(map[string]any{column: value}))
	return nil
}

func (c *fileChangeLog) Reset(table, reason string) { c.resets[table] = reason }

func (c *fileChangeLog) empty() bool { return len(c.upserts)+len(c.deletes)+len(c.resets) == 0 }

// fileRawRows keeps each synced row as the snapshot writer encodes it, so a
// change read's upserts can be compared byte for byte.
type fileRawRows struct{ rows []string }

func (s *fileRawRows) Write(row map[string]any) error {
	s.rows = append(s.rows, encodeFileRow(row))
	return nil
}

func (s *fileRawRows) WriteJSON(row []byte) error {
	s.rows = append(s.rows, string(row))
	return nil
}

// encodeFileRow encodes a row as the snapshot writer and the change set do.
func encodeFileRow(row map[string]any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(row) // test rows always encode
	return string(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

// fileFullRead is what a sync does before it follows changes: it lists the
// tables, takes the cursor, and reads each table in full. Each table comes
// back carrying the columns and key it was written with, as the syncer
// hands a change read the stored ones.
func fileFullRead(t *testing.T, kind ChangeReader, src Source) (json.RawMessage, []Table, map[string][]string) {
	t.Helper()
	ctx := context.Background()
	src.ForSync = true
	k := kind.(Kind)
	d, err := k.Discover(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if d.Settings != nil {
		src.Settings = d.Settings
	}
	start, err := kind.Start(ctx, src, d.Tables)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string][]string{}
	for i, table := range d.Tables {
		sink := &fileRawRows{}
		written, err := k.Sync(ctx, src, table, sink)
		if err != nil {
			t.Fatalf("sync %s: %v", table.Name, err)
		}
		d.Tables[i].Columns, d.Tables[i].PrimaryKey = written.Columns, written.PrimaryKey
		rows[table.Name] = sink.rows
	}
	return start.Cursor, d.Tables, rows
}

// fileChangeRead lists the tables again and reads their changes after cursor,
// each table carrying the columns and key stored by the read before.
func fileChangeRead(t *testing.T, kind ChangeReader, src Source, cursor json.RawMessage, stored []Table) (*fileChangeLog, json.RawMessage, error) {
	t.Helper()
	src.ForSync = true
	d, err := kind.(Kind).Discover(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if d.Settings != nil {
		src.Settings = d.Settings
	}
	for i := range d.Tables {
		if j := slices.IndexFunc(stored, func(s Table) bool { return s.Name == d.Tables[i].Name }); j >= 0 {
			d.Tables[i].Columns, d.Tables[i].PrimaryKey = stored[j].Columns, stored[j].PrimaryKey
		}
	}
	log := newFileChangeLog()
	next, err := kind.Changes(context.Background(), src, d.Tables, cursor, log)
	return log, next, err
}

func (f *fakeS3) remove(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
}

// objectGets are the keys of the objects the bucket served since request n.
func (f *fakeS3) objectGets(since int) []string {
	var keys []string
	for _, r := range f.seen()[since:] {
		if r.method == http.MethodGet && !r.list && r.key != "" {
			keys = append(keys, r.key)
		}
	}
	return keys
}

func TestS3Changes(t *testing.T) {
	ctx := context.Background()

	// An hourly sync of a bucket nobody wrote to must cost one listing, not a
	// download of every file.
	t.Run("an unchanged folder reads nothing", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.s3.put("e2e/orders/2026-10-01.jsonl", "{\"id\":1,\"status\":\"paid\"}\n")
		src := h.roleSource(t, "")
		cursor, stored, _ := fileFullRead(t, h.kind, src)
		before := len(h.s3.seen())
		log, next, err := fileChangeRead(t, h.kind, src, cursor, stored)
		if err != nil {
			t.Fatal(err)
		}
		if !log.empty() || string(next) != string(cursor) {
			t.Fatalf("log = %+v, next = %s, cursor = %s", log, next, cursor)
		}
		if gets := h.s3.objectGets(before); len(gets) != 0 {
			t.Fatalf("downloaded %v", gets)
		}
	})

	// New export files land after the old ones; reading them alone must give
	// exactly the rows a full read would add, so the merged snapshot is the
	// full read's.
	t.Run("reads appended files alone and upserts the full read's rows", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.s3.put("e2e/orders/2026-10-01.jsonl", "{\"id\":1,\"status\":\"paid\",\"total\":10}\n{\"id\":2,\"status\":\"open\",\"total\":5}\n")
		src := h.roleSource(t, "")
		cursor, stored, beforeRows := fileFullRead(t, h.kind, src)
		h.s3.put("e2e/orders/2026-10-02.jsonl", "{\"status\":\"paid\",\"id\":3}\n")
		before := len(h.s3.seen())
		log, next, err := fileChangeRead(t, h.kind, src, cursor, stored)
		if err != nil {
			t.Fatal(err)
		}
		if gets := h.s3.objectGets(before); !slices.Equal(gets, []string{"e2e/orders/2026-10-02.jsonl"}) {
			t.Fatalf("downloaded %v", gets)
		}
		_, _, afterRows := fileFullRead(t, h.kind, src)
		want := afterRows["orders"][len(beforeRows["orders"]):]
		if len(log.resets) != 0 || !slices.Equal(log.upserts["orders"], want) || log.upserts["orders"][0] != `{"id":3,"status":"paid","total":null}` {
			t.Fatalf("upserts = %q, want %q (resets %v)", log.upserts["orders"], want, log.resets)
		}
		// The next read starts after the new file.
		log, _, err = fileChangeRead(t, h.kind, src, next, stored)
		if err != nil || !log.empty() {
			t.Fatalf("second read = %+v, %v", log, err)
		}
	})

	// A Hive folder's value is a column the files don't hold; an appended
	// file's rows carry their own folder's value, typed as the full read types it.
	t.Run("gives appended rows their partition", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.s3.put("e2e/orders/dt=2026-10-01/part-0.csv", "id,total\n1,10\n")
		src := h.roleSource(t, "")
		cursor, stored, beforeRows := fileFullRead(t, h.kind, src)
		h.s3.put("e2e/orders/dt=2026-10-02/part-0.csv", "id,total\n2,7\n")
		log, _, err := fileChangeRead(t, h.kind, src, cursor, stored)
		if err != nil {
			t.Fatal(err)
		}
		_, _, afterRows := fileFullRead(t, h.kind, src)
		want := afterRows["orders"][len(beforeRows["orders"]):]
		if len(log.resets) != 0 || !slices.Equal(log.upserts["orders"], want) || want[0] != `{"id":2,"total":7,"dt":"2026-10-02"}` {
			t.Fatalf("upserts = %q, want %q (resets %v)", log.upserts["orders"], want, log.resets)
		}
	})

	// Each of these makes a full read write rows the snapshot doesn't have,
	// or drop ones it has, so the table is read in full rather than merged.
	for name, change := range map[string]func(f *fakeS3){
		"a rewritten file":             func(f *fakeS3) { f.put("e2e/orders/2026-10-02.jsonl", "{\"id\":1,\"status\":\"void\",\"total\":10}\n") },
		"a removed file":               func(f *fakeS3) { f.remove("e2e/orders/2026-10-02.jsonl") },
		"a new file among the earlier": func(f *fakeS3) { f.put("e2e/orders/2026-10-03.jsonl", "{\"id\":3,\"status\":\"paid\",\"total\":1}\n") },
		"a new column": func(f *fakeS3) {
			f.put("e2e/orders/2026-10-05.jsonl", "{\"id\":5,\"status\":\"paid\",\"coupon\":\"X\"}\n")
		},
		"a wider type": func(f *fakeS3) {
			f.put("e2e/orders/2026-10-05.jsonl", "{\"id\":5,\"status\":\"paid\",\"total\":\"ten\"}\n")
		},
		"an id repeated in the new files": func(f *fakeS3) { f.put("e2e/orders/2026-10-05.jsonl", "{\"id\":5}\n{\"id\":5}\n") },
	} {
		t.Run("reads the table in full after "+name, func(t *testing.T) {
			h := newS3Harness(t, nil)
			h.s3.put("e2e/orders/2026-10-02.jsonl", "{\"id\":1,\"status\":\"paid\",\"total\":10}\n{\"id\":2,\"status\":\"open\",\"total\":5}\n")
			h.s3.put("e2e/orders/2026-10-04.jsonl", "{\"id\":4,\"status\":\"open\",\"total\":2}\n")
			src := h.roleSource(t, "")
			cursor, stored, _ := fileFullRead(t, h.kind, src)
			change(h.s3)
			log, next, err := fileChangeRead(t, h.kind, src, cursor, stored)
			if err != nil {
				t.Fatal(err)
			}
			if log.resets["orders"] == "" || len(log.upserts) != 0 {
				t.Fatalf("log = %+v", log)
			}
			// The cursor moves to the files the full read in this sync reads.
			if log, _, err := fileChangeRead(t, h.kind, src, next, stored); err != nil || !log.empty() {
				t.Fatalf("next read = %+v, %v", log, err)
			}
		})
	}

	// A table without a key can't be merged row by row, but an unchanged
	// one still needs no read.
	t.Run("reads a keyless table in full only when its files change", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.s3.put("e2e/notes/2026-10-01.jsonl", "{\"text\":\"a\"}\n")
		src := h.roleSource(t, "")
		cursor, stored, _ := fileFullRead(t, h.kind, src)
		if len(stored[0].PrimaryKey) != 0 || h.kind.Capture(src, "notes") != CaptureCursor {
			t.Fatalf("stored = %+v", stored[0])
		}
		if log, _, err := fileChangeRead(t, h.kind, src, cursor, stored); err != nil || !log.empty() {
			t.Fatalf("unchanged read = %+v, %v", log, err)
		}
		h.s3.put("e2e/notes/2026-10-02.jsonl", "{\"text\":\"b\"}\n")
		log, _, err := fileChangeRead(t, h.kind, src, cursor, stored)
		if err != nil || log.resets["notes"] == "" || len(log.upserts) != 0 {
			t.Fatalf("appended read = %+v, %v", log, err)
		}
	})

	// The rows must be encoded with the stored columns; a read that doesn't
	// know them can't append.
	t.Run("reads the table in full when its stored columns are unknown", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.s3.put("e2e/orders/2026-10-01.jsonl", "{\"id\":1}\n")
		src := h.roleSource(t, "")
		cursor, _, _ := fileFullRead(t, h.kind, src)
		h.s3.put("e2e/orders/2026-10-02.jsonl", "{\"id\":2}\n")
		log, _, err := fileChangeRead(t, h.kind, src, cursor, nil)
		if err != nil || log.resets["orders"] == "" || len(log.upserts) != 0 {
			t.Fatalf("log = %+v, %v", log, err)
		}
	})

	// A cursor the kind can't read must not be guessed at: the syncer reads
	// every table in full and starts again.
	t.Run("asks for a full read when the cursor is unreadable", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.s3.put("e2e/orders/2026-10-01.jsonl", "{\"id\":1}\n")
		src := h.roleSource(t, "")
		_, err := h.kind.Changes(ctx, src, nil, json.RawMessage(`{"position":7}`), newFileChangeLog())
		if !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("err = %v", err)
		}
	})
}
