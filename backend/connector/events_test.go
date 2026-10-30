package connector

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/events"
	"github.com/helixdb/helix-foundry/backend/snapshot"
)

// eventObjects is the bucket: batch objects and table snapshots by key. An
// error in fail is what reading that key returns. opens counts the reads of
// each key; after a key's first read, an error in again is what reading it
// returns, and a body in reopened what it holds. A read renders its pieces
// from several goroutines, so it is safe for concurrent use.
type eventObjects struct {
	mu       sync.Mutex
	objects  map[string][]byte
	fail     map[string]error
	opens    map[string]int
	again    map[string]error
	reopened map[string][]byte
}

func (o *eventObjects) Put(_ context.Context, key string, body io.ReadSeeker) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, err := io.ReadAll(body)
	o.objects[key] = b
	return err
}

func (o *eventObjects) Open(_ context.Context, key string) (io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opens[key]++
	if err := o.fail[key]; err != nil {
		return nil, err
	}
	if err := o.again[key]; err != nil && o.opens[key] > 1 {
		return nil, err
	}
	b, ok := o.objects[key]
	if body, swapped := o.reopened[key]; swapped && o.opens[key] > 1 {
		b = body
	}
	if !ok {
		return nil, fmt.Errorf("get s3://bucket/%s: %w", key, snapshot.ErrMissing)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (o *eventObjects) Delete(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.objects, key)
	return nil
}

// opened is how many times key was read.
func (o *eventObjects) opened(key string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opens[key]
}

// eventSource is one event source as the provider holds it: its learned
// schema, its batches with their objects in the bucket, the seq its stored
// cursor holds and its table's snapshot key. reads counts EventBatchesAfter;
// fail, when set, is what every read returns.
type eventSource struct {
	schema  events.Schema
	batches []events.Batch
	cursor  int64
	key     string
	objects *eventObjects
	reads   int
	fail    error
}

func newEventSource() *eventSource {
	return &eventSource{objects: &eventObjects{objects: map[string][]byte{}, fail: map[string]error{}, opens: map[string]int{},
		again: map[string]error{}, reopened: map[string][]byte{}}}
}

func (s *eventSource) EventTable(context.Context, string) (events.Schema, int64, string, error) {
	return s.schema, s.cursor, s.key, s.fail
}

// waiting are up to limit batches above after, oldest first, as the provider's index read returns them.
func (s *eventSource) waiting(after int64, limit int) []events.Batch {
	var out []events.Batch
	for _, b := range s.batches {
		if b.Seq > after && len(out) < limit {
			out = append(out, b)
		}
	}
	return out
}

func (s *eventSource) EventBatchesAfter(_ context.Context, _ string, after int64, budget events.ReadBudget) (events.Schema, []events.Batch, error) {
	s.reads++
	return s.schema, budget.Cut(s.schema, s.waiting(after, budget.Batches)), s.fail
}

func (s *eventSource) EventBacklog(_ context.Context, _ string, after int64, budget events.ReadBudget) (Backlog, error) {
	return budget.Backlog(s.schema, s.waiting(after, budget.Batches+1)), s.fail
}

// send stores pieces, each "<id> <data>", as the source's next batch, its
// fields merged into the schema as a send's transaction does. Batch n is
// received at 12:00:0n.
func (s *eventSource) send(t *testing.T, pieces ...string) events.Batch {
	t.Helper()
	var in []events.Piece
	for _, p := range pieces {
		id, data, _ := strings.Cut(p, " ")
		in = append(in, events.Piece{ID: id, Data: []byte(data)})
	}
	send, err := events.Validate(in, events.Limits{MaxPieces: 500, MaxBytes: 1 << 20, MaxPieceBytes: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := s.schema.Merge(send, 200)
	if err != nil {
		t.Fatal(err)
	}
	seq := int64(len(s.batches) + 1)
	obj, err := events.EncodeLines(send, time.Date(2026, 10, 24, 12, 0, int(seq), 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	b := events.Batch{ID: fmt.Sprintf("batch-%d", seq), Seq: seq, Key: fmt.Sprintf("ws/ws-1/events/conn-1/batch-%d.jsonl.gz", seq),
		Pieces: send.Pieces(), Bytes: send.Bytes()}
	s.objects.objects[b.Key] = obj.Body
	s.batches, s.schema = append(s.batches, b), merged.Schema
	return b
}

// snapshot stores rows as the table's snapshot, as a sync that merged up to
// seq left it.
func (s *eventSource) snapshot(t *testing.T, seq int64, rows ...string) {
	t.Helper()
	w, err := snapshot.NewWriter(t.TempDir(), 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	for _, r := range rows {
		if err := w.WriteJSON([]byte(r)); err != nil {
			t.Fatal(err)
		}
	}
	s.cursor, s.key = seq, "ws/ws-1/connectors/conn-1/events/snapshot.jsonl.gz"
	if err := w.Upload(context.Background(), s.objects, s.key); err != nil {
		t.Fatal(err)
	}
}

// kind reads the source within budget; a source with a text field keeps
// its newest 3 pieces as documents.
func (s *eventSource) kind(budget events.ReadBudget) *eventsKind {
	return &eventsKind{EventsOptions{Batches: s, Objects: s.objects, Budget: budget, Extracted: 3}}
}

// pieceChanges is a change log that takes the columns a read declares, as
// the syncer's change sets do, and offers documents to its DocumentWriter:
// none, unless a test sets one.
type pieceChanges struct {
	*fileChangeLog
	DocumentWriter
	columns map[string][]Column
}

func newPieceChanges() *pieceChanges {
	return &pieceChanges{fileChangeLog: newFileChangeLog(), DocumentWriter: noDocuments{}, columns: map[string][]Column{}}
}

func (c *pieceChanges) Columns(table string, columns []Column) { c.columns[table] = columns }

// eventRow is a piece's row as batch seq stores it: received at 12:00:0<seq>,
// with no occurred_at, and fields its data and padding, written out.
func eventRow(id string, seq int, fields string) string {
	return fmt.Sprintf(`{"id":%q,"received_at":"2026-10-24T12:00:%02d.000000Z","occurred_at":null%s}`, id, seq, fields)
}

// wide is a budget no test's backlog reaches.
var wide = events.ReadBudget{Batches: 1000, Pieces: 50000, Bytes: 128 << 20}

var testSource = Source{ConnectorID: "conn-1", Settings: json.RawMessage(`{"description":"orders"}`)}

func TestEventsKind(t *testing.T) {
	// The kind joins the catalog last, so every other kind keeps its place in
	// the website's list, and the plan catalog knows its name.
	t.Run("is the catalog's last kind", func(t *testing.T) {
		kinds := NewCatalog(DefaultEndpoints(), Guard{}, S3Options{}, EventsOptions{}).Kinds()
		last := kinds[len(kinds)-1]
		if last.Name() != "events" || last.DisplayName() != "API source" || last.Hint("anything") != "" {
			t.Fatalf("last kind = %s %q", last.Name(), last.DisplayName())
		}
		if names := KindNames(); names[len(names)-1] != "events" {
			t.Fatalf("KindNames = %v", names)
		}
	})
	// A request sets the description and the text field and nothing else:
	// the schema is what sends learned, and a request that carried one could
	// unlock a type or drop a field the source's rows hold. Each comes back
	// only when the request carries it, so an update that leaves one out
	// stores nothing over it, and "" is a value that clears it.
	for _, tc := range []struct {
		name, settings, secret, want, refusal string
	}{
		{name: "trims the description", settings: `{"description":"  Orders from the shop\n"}`, want: `{"description":"Orders from the shop"}`},
		{name: "takes no settings as none set", settings: ``, want: `{}`},
		{name: "takes null as none set", settings: `null`, want: `{}`},
		{name: "leaves out a null description", settings: `{"description":null}`, want: `{}`},
		{name: "keeps a blank description, which clears it", settings: `{"description":"  "}`, want: `{"description":""}`},
		{name: "takes 1,000 characters", settings: `{"description":"` + strings.Repeat("é", 1000) + `"}`, want: `{"description":"` + strings.Repeat("é", 1000) + `"}`},
		{name: "refuses 1,001 characters", settings: `{"description":"` + strings.Repeat("é", 1001) + `"}`, refusal: "this one holds 1001"},
		{name: "takes a text field alone", settings: `{"text_field":"reply"}`, want: `{"text_field":"reply"}`},
		{name: "takes a text field with the description", settings: `{"description":"x","text_field":"reply"}`, want: `{"description":"x","text_field":"reply"}`},
		{name: "keeps an empty text field, which clears it", settings: `{"text_field":""}`, want: `{"text_field":""}`},
		{name: "leaves out a null text field", settings: `{"text_field":null}`, want: `{}`},
		{name: "keeps a text field byte for byte", settings: `{"text_field":" Reply text "}`, want: `{"text_field":" Reply text "}`},
		{name: "takes a 64-byte text field", settings: `{"text_field":"` + strings.Repeat("x", 64) + `"}`, want: `{"text_field":"` + strings.Repeat("x", 64) + `"}`},
		{name: "refuses a 65-byte text field", settings: `{"text_field":"` + strings.Repeat("x", 65) + `"}`, refusal: "a field name is 1 to 64 bytes; this one is 65"},
		{name: "refuses a text field with a control character", settings: `{"text_field":"re\nply"}`, refusal: "a field name has no control characters"},
		{name: "refuses a text field with a backtick", settings: "{\"text_field\":\"re`ply\"}", refusal: "a field name has no backticks"},
		{name: "refuses the id as the text field", settings: `{"text_field":"id"}`, refusal: "id is a column of every piece"},
		{name: "refuses received_at as the text field", settings: `{"text_field":"received_at"}`, refusal: "received_at is a column of every piece"},
		{name: "refuses a text field that is not a string", settings: `{"text_field":7}`, refusal: "no other settings"},
		{name: "refuses a schema", settings: `{"description":"x","schema":{"amount":"number"}}`, refusal: "no other settings"},
		{name: "refuses another setting", settings: `{"full_read_through":3}`, refusal: "no other settings"},
		{name: "refuses settings that are not an object", settings: `["x"]`, refusal: "no other settings"},
		{name: "refuses a key", settings: `{"description":"x"}`, secret: "k", refusal: "takes no key"},
	} {
		t.Run("prepare "+tc.name, func(t *testing.T) {
			got, err := (&eventsKind{}).Prepare(Source{Settings: json.RawMessage(tc.settings), Secret: tc.secret})
			if tc.refusal != "" {
				if msg := invalidMessage(t, err); !strings.Contains(msg, tc.refusal) {
					t.Fatalf("refusal = %q, want %q", msg, tc.refusal)
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Fatalf("Prepare = (%s, %v), want %s", got, err, tc.want)
			}
		})
	}
	// A stored text field turns the pieces stream on, keeping the newest
	// Extracted; with none, or "", the stream is off and its set empties.
	for _, tc := range []struct {
		name, settings string
		on             bool
	}{
		{"a text field", `{"description":"Replies to customers","schema":{"reply":"string"},"text_field":"reply"}`, true},
		{"a text field and no description", `{"text_field":"reply"}`, true},
		{"no text field", `{"description":"Replies to customers","schema":{"reply":"string"}}`, false},
		{"a cleared text field", `{"description":"Replies to customers","text_field":""}`, false},
		{"no settings", ``, false},
	} {
		t.Run("streams with "+tc.name, func(t *testing.T) {
			streams := (&eventsKind{EventsOptions{Extracted: 500}}).Streams(Source{Settings: json.RawMessage(tc.settings)})
			if !tc.on {
				if streams != nil {
					t.Fatalf("streams = %+v", streams)
				}
				return
			}
			var stored eventsStored
			_ = json.Unmarshal([]byte(tc.settings), &stored)
			want := Stream{Name: "pieces", Table: "events", Title: "Pieces", Description: stored.Description, Format: Markdown, Keep: KeepNewest(500)}
			if len(streams) != 1 || streams[0] != want || streams[0].Problem() != "" {
				t.Fatalf("streams = %+v, want %+v", streams, want)
			}
		})
	}
	// Discovery is the stored schema as one Dense table keyed by id, typed
	// for generation, read without the network: the backend's catalog, which
	// reads no batches, discovers the same.
	t.Run("discovers one dense table typed from the stored schema", func(t *testing.T) {
		d, err := (&eventsKind{}).Discover(context.Background(), Source{
			Settings: json.RawMessage(`{"description":"x","schema":{"tags":"array","amount":"number","note":"null"}}`)})
		if err != nil || len(d.Tables) != 1 || len(d.Unavailable) != 0 {
			t.Fatalf("Discover = (%+v, %v)", d, err)
		}
		table := d.Tables[0]
		want := []Column{{Name: "id", Type: "string"}, {Name: "received_at", Type: "string"}, {Name: "occurred_at", Type: "string", Nullable: true},
			{Name: "amount", Type: "number", Nullable: true}, {Name: "note", Type: "null", Nullable: true}, {Name: "tags", Type: "array", Nullable: true}}
		if table.Name != "events" || !table.Dense || !slices.Equal(table.PrimaryKey, []string{"id"}) || !slices.Equal(table.Columns, want) {
			t.Fatalf("table = %+v", table)
		}
	})
	// A source no piece has reached yet has only the envelope's columns.
	t.Run("discovers the envelope before any send", func(t *testing.T) {
		d, err := (&eventsKind{}).Discover(context.Background(), Source{Settings: json.RawMessage(`{"description":""}`)})
		if err != nil || len(d.Tables) != 1 || len(d.Tables[0].Columns) != 3 {
			t.Fatalf("Discover = (%+v, %v)", d, err)
		}
	})
	// A stored type no send could have learned is corrupt config, never a
	// table with a column quietly missing.
	t.Run("refuses a stored schema it cannot read", func(t *testing.T) {
		if _, err := (&eventsKind{}).Discover(context.Background(), Source{Settings: json.RawMessage(`{"schema":{"at":"date"}}`)}); err == nil {
			t.Fatal("Discover succeeded")
		}
	})
	// Only the events table is followed; it is read through its batches.
	t.Run("captures events", func(t *testing.T) {
		k := &eventsKind{}
		if k.Capture(Source{}, "events") != CaptureEvents || k.Capture(Source{}, "other") != CaptureFull {
			t.Fatal("capture")
		}
	})
	// A failed read of the source's rows, such as MySQL timing out, says
	// nothing about its pieces: every step passes it on, to be retried.
	t.Run("passes a failed read on to be retried", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		k := src.kind(wide)
		ctx := context.Background()
		start, err := k.Start(ctx, Source{ConnectorID: "conn-1"}, nil)
		if err != nil || start.Source == nil {
			t.Fatalf("Start of a source with no settings = (%+v, %v)", start, err)
		}
		src.fail = errors.New("connection reset")
		_, startErr := k.Start(ctx, testSource, nil)
		_, syncErr := k.Sync(ctx, *start.Source, Table{Name: "events"}, &fileRawRows{})
		_, changesErr := k.Changes(ctx, testSource, nil, eventsCursorAt(0), newPieceChanges())
		_, backlogErr := k.Backlog(ctx, testSource, eventsCursorAt(0))
		for _, err := range []error{startErr, syncErr, changesErr, backlogErr} {
			var invalidErr *InvalidError
			if !errors.Is(err, src.fail) || errors.As(err, &invalidErr) {
				t.Fatalf("error = %v, want the read's, retryable", err)
			}
		}
	})
	// The backend's catalog discovers only: a sync asked of it is an error,
	// never a nil dereference.
	t.Run("refuses to sync without its reads", func(t *testing.T) {
		k := &eventsKind{}
		ctx := context.Background()
		_, startErr := k.Start(ctx, testSource, nil)
		_, syncErr := k.Sync(ctx, testSource, Table{Name: "events"}, &fileRawRows{})
		_, changesErr := k.Changes(ctx, testSource, nil, eventsCursorAt(0), newPieceChanges())
		_, backlogErr := k.Backlog(ctx, testSource, eventsCursorAt(0))
		for _, err := range []error{startErr, syncErr, changesErr, backlogErr} {
			if !errors.Is(err, errEventsUnwired) {
				t.Fatalf("error = %v, want errEventsUnwired", err)
			}
		}
	})
}

func TestEventsChanges(t *testing.T) {
	ctx := context.Background()
	// A merge pads a Dense table's stored rows to the columns its read
	// declares, and generation reads their types: a read declares its own
	// schema's, and pads every row it writes to it, so a field an earlier
	// piece lacks is null and never absent.
	t.Run("upserts each batch in seq order, padded to its read's schema", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		src.send(t, `b {"x":2,"y":"s"}`, `a {"x":3}`)
		changes := newPieceChanges()
		next, err := src.kind(wide).Changes(ctx, testSource, nil, eventsCursorAt(0), changes)
		if err != nil || string(next) != `{"seq":2}` {
			t.Fatalf("Changes = (%s, %v)", next, err)
		}
		want := []string{eventRow("a", 1, `,"x":1,"y":null`), eventRow("b", 2, `,"x":2,"y":"s"`), eventRow("a", 2, `,"x":3,"y":null`)}
		if got := changes.upserts["events"]; !slices.Equal(got, want) {
			t.Fatalf("upserts = %q", got)
		}
		if got := changes.columns["events"]; !slices.Equal(got, eventColumns(src.schema)) || got[4] != (Column{Name: "y", Type: "string", Nullable: true}) {
			t.Fatalf("columns = %+v", got)
		}
	})
	// The cursor holds the newest batch merged: the next read starts after it.
	t.Run("reads only the batches above the cursor", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		src.send(t, `b {"x":2}`)
		changes := newPieceChanges()
		next, err := src.kind(wide).Changes(ctx, testSource, nil, eventsCursorAt(1), changes)
		if err != nil || string(next) != `{"seq":2}` || !slices.Equal(changes.upserts["events"], []string{eventRow("b", 2, `,"x":2`)}) {
			t.Fatalf("Changes = (%s, %v), upserts %q", next, err, changes.upserts["events"])
		}
	})
	// Nothing waiting is nothing to merge, and the cursor stays exactly as stored.
	t.Run("keeps the cursor when nothing waits", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		cursor := json.RawMessage(`{"seq": 1}`)
		changes := newPieceChanges()
		next, err := src.kind(wide).Changes(ctx, testSource, nil, cursor, changes)
		if err != nil || string(next) != string(cursor) || !changes.empty() {
			t.Fatalf("Changes = (%s, %v), changes %+v", next, err, changes.fileChangeLog)
		}
	})
	// One read holds its rows and their parsed fields at once: each bound of
	// the budget stops it at a batch, and the cursor says where the next
	// read starts. Three batches of two pieces each.
	for _, tc := range []struct {
		name   string
		budget func(events.Schema, []events.Batch) events.ReadBudget
		want   string
	}{
		{"batches", func(events.Schema, []events.Batch) events.ReadBudget {
			return events.ReadBudget{Batches: 2, Pieces: wide.Pieces, Bytes: wide.Bytes}
		}, `{"seq":2}`},
		{"pieces", func(events.Schema, []events.Batch) events.ReadBudget {
			return events.ReadBudget{Batches: wide.Batches, Pieces: 3, Bytes: wide.Bytes}
		}, `{"seq":1}`},
		{"bytes", func(s events.Schema, b []events.Batch) events.ReadBudget {
			perPiece := s.NullFill() + 64*int64(s.Len())
			return events.ReadBudget{Batches: wide.Batches, Pieces: wide.Pieces, Bytes: b[0].Bytes + b[1].Bytes + 4*perPiece}
		}, `{"seq":2}`},
	} {
		t.Run("stops at the "+tc.name+" budget", func(t *testing.T) {
			src := newEventSource()
			src.send(t, `a {"x":1}`, `b {"x":2}`)
			src.send(t, `c {"x":3}`, `d {"x":4}`)
			src.send(t, `e {"x":5}`, `f {"x":6}`)
			next, err := src.kind(tc.budget(src.schema, src.batches)).Changes(ctx, testSource, nil, eventsCursorAt(0), newPieceChanges())
			if err != nil || string(next) != tc.want {
				t.Fatalf("Changes = (%s, %v), want %s", next, err, tc.want)
			}
		})
	}
	// A backlog larger than one read is merged over several, and every
	// piece is read exactly once: none lost between reads, none read twice.
	t.Run("splits a backlog across reads without losing or repeating a piece", func(t *testing.T) {
		src := newEventSource()
		for i := range 7 {
			src.send(t, fmt.Sprintf(`p%d {"x":%d}`, i, i))
		}
		k := src.kind(events.ReadBudget{Batches: 3, Pieces: wide.Pieces, Bytes: wide.Bytes})
		cursor := eventsCursorAt(0)
		var ids []string
		for range 4 {
			changes := newPieceChanges()
			next, err := k.Changes(ctx, testSource, nil, cursor, changes)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range changes.upserts["events"] {
				var r struct{ ID string }
				_ = json.Unmarshal([]byte(row), &r)
				ids = append(ids, r.ID)
			}
			cursor = next
		}
		if string(cursor) != `{"seq":7}` || !slices.Equal(ids, []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6"}) {
			t.Fatalf("cursor %s, pieces %v", cursor, ids)
		}
	})
	// Every batch's object is stored before its row commits, and S3 reads
	// are consistent after a write: a missing object will never appear, so
	// retrying only delays telling the owner.
	t.Run("fails for good on a missing batch object", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		gone := src.send(t, `b {"x":2}`)
		delete(src.objects.objects, gone.Key)
		_, err := src.kind(wide).Changes(ctx, testSource, nil, eventsCursorAt(0), newPieceChanges())
		if msg := invalidMessage(t, err); msg != "batch 2 of this source is missing from storage" {
			t.Fatalf("message = %q", msg)
		}
	})
	// S3 throttles and drops connections; the next attempt reads the same
	// batch again. A cut stream reads the same whether the object or the
	// connection was cut, so it too is retried.
	for name, broken := range map[string]func(*eventObjects, string){
		"a throttled read": func(o *eventObjects, key string) {
			o.fail[key] = errors.New("SlowDown: please reduce your request rate")
		},
		"a cut stream": func(o *eventObjects, key string) { o.objects[key] = o.objects[key][:len(o.objects[key])/2] },
	} {
		t.Run("retries "+name, func(t *testing.T) {
			src := newEventSource()
			b := src.send(t, `a {"x":1}`, `b {"x":2}`)
			broken(src.objects, b.Key)
			_, err := src.kind(wide).Changes(ctx, testSource, nil, eventsCursorAt(0), newPieceChanges())
			var invalidErr *InvalidError
			if err == nil || errors.As(err, &invalidErr) {
				t.Fatalf("error = %v, want a retryable one", err)
			}
		})
	}
	// An object that is not the lines its row records was not written by a
	// send: merging what it holds could drop or invent pieces, so the sync
	// fails for good and the owner is told.
	for name, corrupt := range map[string]func(t *testing.T, src *eventSource){
		"an object that is not gzip": func(_ *testing.T, src *eventSource) {
			src.objects.objects[src.batches[0].Key] = []byte(`{"id":"a","x":1}` + "\n")
		},
		"an empty object": func(_ *testing.T, src *eventSource) { src.objects.objects[src.batches[0].Key] = nil },
		"a stream whose checksum fails": func(_ *testing.T, src *eventSource) {
			body := slices.Clone(src.objects.objects[src.batches[0].Key])
			body[len(body)-8] ^= 0xff // the trailer's CRC-32
			src.objects.objects[src.batches[0].Key] = body
		},
		"an object longer than its row records":  func(_ *testing.T, src *eventSource) { src.batches[0].Bytes-- },
		"an object shorter than its row records": func(_ *testing.T, src *eventSource) { src.batches[0].Bytes++ },
		"fewer lines than its row records":       func(_ *testing.T, src *eventSource) { src.batches[0].Pieces++ },
		"a line no send writes": func(t *testing.T, src *eventSource) {
			line := []byte(`{"id":"a","x":1}` + "\n")
			src.objects.objects[src.batches[0].Key] = gzipped(t, line)
			src.batches[0].Bytes, src.batches[0].Pieces = int64(len(line)), 1
		},
		"a line holding a field its schema lacks": func(_ *testing.T, src *eventSource) { src.schema = events.Schema{} },
	} {
		t.Run("fails for good on "+name, func(t *testing.T) {
			src := newEventSource()
			src.send(t, `a {"x":1}`)
			corrupt(t, src)
			changes := newPieceChanges()
			_, err := src.kind(wide).Changes(ctx, testSource, nil, eventsCursorAt(0), changes)
			if msg := invalidMessage(t, err); !strings.HasPrefix(msg, "batch 1 of this source can't be read: ") {
				t.Fatalf("message = %q", msg)
			}
			if len(changes.upserts["events"]) != 0 {
				t.Fatalf("upserts = %q", changes.upserts["events"])
			}
		})
	}
	// Without declared columns a merge would not pad the stored rows, and a
	// field missing from the oldest rows is never cataloged.
	t.Run("refuses a writer that cannot take columns", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		changes := newFileChangeLog()
		if _, err := src.kind(wide).Changes(ctx, testSource, nil, eventsCursorAt(0), changes); err == nil || !changes.empty() {
			t.Fatalf("Changes = %v, changes %+v", err, changes)
		}
	})
	// The cursor is only ever Start's or a change read's; one in any other
	// form is read again from the table, in full.
	t.Run("expires a cursor it cannot read", func(t *testing.T) {
		for _, cursor := range []string{`{"lsn":"0/1"}`, `{"seq":-1}`, `"1"`, ``} {
			if _, err := newEventSource().kind(wide).Changes(ctx, testSource, nil, json.RawMessage(cursor), newPieceChanges()); !errors.Is(err, ErrCursorExpired) {
				t.Fatalf("Changes(%s) = %v, want ErrCursorExpired", cursor, err)
			}
		}
	})
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEventsFullRead(t *testing.T) {
	ctx := context.Background()
	// fullRead is what the syncer does: Start, then Sync of the one table
	// with the Source Start returned.
	fullRead := func(t *testing.T, k *eventsKind) (json.RawMessage, []string, Table) {
		t.Helper()
		start, err := k.Start(ctx, testSource, nil)
		if err != nil || start.Source == nil {
			t.Fatalf("Start = (%+v, %v)", start, err)
		}
		rows := &fileRawRows{}
		table, err := k.Sync(ctx, *start.Source, Table{Name: "events", PrimaryKey: []string{"id"}, Dense: true}, rows)
		if err != nil {
			t.Fatal(err)
		}
		return start.Cursor, rows.rows, table
	}
	// A full read is the snapshot with the batches above its cursor applied:
	// the newest piece of each id wins, and every other id keeps its stored
	// row, padded to the fields sends have added since. Batches already in
	// the snapshot are never read again.
	t.Run("keeps each id's newest piece and the snapshot's rows for the rest", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":0}`, `b {"x":0}`)
		src.send(t, `c {"x":0}`)
		src.snapshot(t, 2, eventRow("a", 1, `,"x":0`), eventRow("b", 1, `,"x":0`), eventRow("c", 2, `,"x":0`))
		src.objects.fail[src.batches[0].Key] = errors.New("batch 1 is in the snapshot: never read")
		src.objects.fail[src.batches[1].Key] = errors.New("batch 2 is in the snapshot: never read")
		src.send(t, `a {"x":10}`, `d {"x":4,"y":"new"}`)
		src.send(t, `d {"x":5}`)
		cursor, rows, table := fullRead(t, src.kind(wide))
		want := []string{
			eventRow("d", 4, `,"x":5,"y":null`), eventRow("a", 3, `,"x":10,"y":null`),
			eventRow("b", 1, `,"x":0,"y":null`), eventRow("c", 2, `,"x":0,"y":null`),
		}
		if string(cursor) != `{"seq":4}` || !slices.Equal(rows, want) {
			t.Fatalf("cursor %s, rows %q", cursor, rows)
		}
		if !slices.Equal(table.Columns, eventColumns(src.schema)) || !table.Dense || !slices.Equal(table.PrimaryKey, []string{"id"}) {
			t.Fatalf("table = %+v", table)
		}
	})
	// A snapshot row that lacks no field keeps its bytes, so its hash, and
	// the comparison with the stored table finds no change in it.
	t.Run("keeps the bytes of a snapshot row that lacks no field", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"y":"s","x":1}`)
		stored := `{"id":"a","received_at":"2026-10-24T12:00:01.000000Z","occurred_at":null,"y":"s","x":1}`
		src.snapshot(t, 1, stored)
		cursor, rows, _ := fullRead(t, src.kind(wide))
		if string(cursor) != `{"seq":1}` || !slices.Equal(rows, []string{stored}) {
			t.Fatalf("cursor %s, rows %q", cursor, rows)
		}
	})
	// A source's first sync has no snapshot: its table is its batches
	// replayed, at most one read's worth, and Start's cursor is the newest
	// batch that read took, so the next change read starts after it.
	t.Run("replays the batches within the budget without a snapshot", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		src.send(t, `b {"x":2}`, `a {"x":3}`)
		src.send(t, `c {"x":4}`)
		cursor, rows, _ := fullRead(t, src.kind(events.ReadBudget{Batches: 2, Pieces: wide.Pieces, Bytes: wide.Bytes}))
		if string(cursor) != `{"seq":2}` || !slices.Equal(rows, []string{eventRow("a", 2, `,"x":3`), eventRow("b", 2, `,"x":2`)}) {
			t.Fatalf("cursor %s, rows %q", cursor, rows)
		}
	})
	// A table without a snapshot holds nothing, whatever the cursor says:
	// replaying from the cursor would lose every piece before it.
	t.Run("replays from the first batch when the table has no snapshot", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		src.send(t, `b {"x":2}`)
		src.cursor = 2
		cursor, rows, _ := fullRead(t, src.kind(wide))
		if string(cursor) != `{"seq":2}` || !slices.Equal(rows, []string{eventRow("b", 2, `,"x":2`), eventRow("a", 1, `,"x":1`)}) {
			t.Fatalf("cursor %s, rows %q", cursor, rows)
		}
	})
	// A send may land between Start and Sync. Its batch is past Start's
	// cursor, so the next change read takes it, and the full read must not;
	// its new fields make each read's budget cut sooner, yet the full read
	// must still take every batch up to Start's cursor, or the pieces
	// between would never be merged.
	t.Run("reads to Start's cursor however a send grows the schema meanwhile", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		src.send(t, `b {"x":2}`)
		perPiece := func(s events.Schema) int64 { return s.NullFill() + 64*int64(s.Len()) }
		budget := events.ReadBudget{Batches: wide.Batches, Pieces: wide.Pieces, Bytes: src.batches[0].Bytes + src.batches[1].Bytes + 2*perPiece(src.schema)}
		k := src.kind(budget)
		start, err := k.Start(ctx, testSource, nil)
		if err != nil || string(start.Cursor) != `{"seq":2}` {
			t.Fatalf("Start = (%+v, %v)", start, err)
		}
		var wider []string
		for i := range 10 {
			wider = append(wider, fmt.Sprintf(`"f%d":%d`, i, i))
		}
		src.send(t, `c {`+strings.Join(wider, ",")+`}`)
		if len(budget.Cut(src.schema, src.batches[:2])) != 1 {
			t.Fatal("the grown schema should cut a read after one batch")
		}
		reads := src.reads
		rows := &fileRawRows{}
		table, err := k.Sync(ctx, *start.Source, Table{Name: "events"}, rows)
		if err != nil {
			t.Fatal(err)
		}
		padding := `,"f0":null,"f1":null,"f2":null,"f3":null,"f4":null,"f5":null,"f6":null,"f7":null,"f8":null,"f9":null`
		if want := []string{eventRow("b", 2, padding+`,"x":2`), eventRow("a", 1, padding+`,"x":1`)}; !slices.Equal(rows.rows, want) {
			t.Fatalf("rows = %q", rows.rows)
		}
		if src.reads-reads != 2 || len(table.Columns) != 14 {
			t.Fatalf("reads = %d, columns = %+v", src.reads-reads, table.Columns)
		}
	})
	// With nothing waiting, the cursor stays and the table is its snapshot.
	t.Run("keeps the table's cursor when nothing waits", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		src.snapshot(t, 1, eventRow("a", 1, `,"x":1`))
		cursor, rows, _ := fullRead(t, src.kind(wide))
		if string(cursor) != `{"seq":1}` || !slices.Equal(rows, []string{eventRow("a", 1, `,"x":1`)}) {
			t.Fatalf("cursor %s, rows %q", cursor, rows)
		}
	})
	// A batch read in full is read as a change read reads it.
	t.Run("fails for good on a missing batch object", func(t *testing.T) {
		src := newEventSource()
		gone := src.send(t, `a {"x":1}`)
		delete(src.objects.objects, gone.Key)
		start, err := src.kind(wide).Start(ctx, testSource, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = src.kind(wide).Sync(ctx, *start.Source, Table{Name: "events"}, &fileRawRows{})
		if msg := invalidMessage(t, err); msg != "batch 1 of this source is missing from storage" {
			t.Fatalf("message = %q", msg)
		}
	})
	// The table's snapshot is the syncer's own object, read as its merges
	// read it: failing to read it is retried.
	t.Run("retries a snapshot it cannot read", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		src.snapshot(t, 1, eventRow("a", 1, `,"x":1`))
		src.objects.fail[src.key] = errors.New("connection reset")
		start, err := src.kind(wide).Start(ctx, testSource, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = src.kind(wide).Sync(ctx, *start.Source, Table{Name: "events"}, &fileRawRows{})
		var invalidErr *InvalidError
		if err == nil || errors.As(err, &invalidErr) {
			t.Fatalf("error = %v, want a retryable one", err)
		}
	})
	// Only a deleted source's purge removes batches. A full read that finds
	// Start's gone fails, and the next attempt finds the source deleted.
	t.Run("fails when Start's batches are gone", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		k := src.kind(wide)
		start, err := k.Start(ctx, testSource, nil)
		if err != nil {
			t.Fatal(err)
		}
		src.batches = nil
		rows := &fileRawRows{}
		if _, err := k.Sync(ctx, *start.Source, Table{Name: "events"}, rows); err == nil || len(rows.rows) != 0 {
			t.Fatalf("Sync = %v, rows %q", err, rows.rows)
		}
	})
	// A snapshot row that is not a piece's would be copied into the table
	// as some other id's: the read stops instead.
	t.Run("refuses a snapshot row that is not a piece's", func(t *testing.T) {
		for _, row := range []string{`{"received_at":"2026-10-24T12:00:01.000000Z","x":1}`, `{"id":7,"x":1}`, `[1]`} {
			src := newEventSource()
			src.send(t, `a {"x":1}`)
			src.snapshot(t, 1, row)
			start, err := src.kind(wide).Start(ctx, testSource, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := src.kind(wide).Sync(ctx, *start.Source, Table{Name: "events"}, &fileRawRows{}); err == nil {
				t.Fatalf("Sync of snapshot row %s succeeded", row)
			}
		}
	})
	// Only Start knows which batches the full read takes.
	t.Run("refuses a full read without Start's Source", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"x":1}`)
		if _, err := src.kind(wide).Sync(ctx, testSource, Table{Name: "events"}, &fileRawRows{}); err == nil {
			t.Fatal("Sync succeeded")
		}
	})
}

func TestEventsBacklog(t *testing.T) {
	// What waits beyond the stored cursor decides the next sync: none, after
	// the spacing, or at once when one read cannot take it all. Three
	// batches, read two at a time.
	for _, tc := range []struct {
		name  string
		after int64
		want  Backlog
	}{
		{"none", 3, BacklogNone},
		{"some", 1, BacklogSome},
		{"more", 0, BacklogMore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newEventSource()
			for i := range 3 {
				src.send(t, fmt.Sprintf(`p%d {"x":%d}`, i, i))
			}
			got, err := src.kind(events.ReadBudget{Batches: 2, Pieces: wide.Pieces, Bytes: wide.Bytes}).Backlog(context.Background(), testSource, eventsCursorAt(tc.after))
			if err != nil || got != tc.want {
				t.Fatalf("Backlog = (%v, %v), want %v", got, err, tc.want)
			}
		})
	}
	// The cursor was just stored by this sync; one it cannot read is an error.
	t.Run("refuses a cursor it cannot read", func(t *testing.T) {
		if _, err := newEventSource().kind(wide).Backlog(context.Background(), testSource, json.RawMessage(`{}`)); err == nil {
			t.Fatal("Backlog succeeded")
		}
	})
}

// textSource is testSource whose pieces' reply is their text.
var textSource = Source{ConnectorID: "conn-1", Settings: json.RawMessage(`{"description":"orders","text_field":"reply"}`)}

// pieceDocs is a DocumentWriter that says yes to every piece of the pieces
// stream unless paused or refused, and records each offer, "<id>
// <version>", and each Put, which returns fail. held is the version it
// holds of each id. onWant, when set, runs at each Want.
type pieceDocs struct {
	paused bool
	refuse map[string]bool
	held   map[string]string
	fail   error
	offers []string
	puts   []Document
	onWant func()
}

func (d *pieceDocs) Want(stream, id, version string) bool {
	if d.onWant != nil {
		d.onWant()
	}
	d.offers = append(d.offers, id+" "+version)
	return stream == EventsStream && !d.paused && !d.refuse[id]
}

func (d *pieceDocs) Put(_ context.Context, stream string, doc Document) error {
	if stream != EventsStream {
		return fmt.Errorf("a Put to stream %s", stream)
	}
	d.puts = append(d.puts, doc)
	return d.fail
}

func (d *pieceDocs) Drop(_, id string) error { return fmt.Errorf("a Drop of piece %s", id) }

func (d *pieceDocs) Held(_, id string) (string, bool) {
	version, ok := d.held[id]
	return version, ok
}

func (d *pieceDocs) Paused(string) bool { return d.paused }

// replyDoc is the document of a piece whose data is only its reply, a
// single line, sent in batch seq (received at 12:00:<seq>).
func replyDoc(id, reply string, seq int) Document {
	body := []byte(fmt.Sprintf("# %s\n\n- Piece: %s\n\n%s", reply, id, reply))
	return Document{ExternalID: id, Version: events.Version(body), At: time.Date(2026, 10, 24, 12, 0, seq, 0, time.UTC), Title: reply, Text: body}
}

// samePuts fails the test unless docs Put want, in order, each after its
// own offer.
func samePuts(t *testing.T, docs *pieceDocs, want ...Document) {
	t.Helper()
	same := slices.EqualFunc(docs.puts, want, func(a, b Document) bool {
		return a.ExternalID == b.ExternalID && a.Version == b.Version && a.At.Equal(b.At) && a.Title == b.Title && a.URL == b.URL &&
			bytes.Equal(a.Text, b.Text)
	})
	var offers []string
	for _, d := range want {
		offers = append(offers, d.ExternalID+" "+d.Version)
	}
	if !same || !slices.Equal(docs.offers, offers) {
		var got []string
		for _, d := range docs.puts {
			got = append(got, fmt.Sprintf("%s@%s %q", d.ExternalID, d.At.Format(time.TimeOnly), d.Text))
		}
		t.Fatalf("puts = %q, offers = %q", got, docs.offers)
	}
}

// discardRows counts the rows a read writes and keeps none.
type discardRows struct{ n int }

func (r *discardRows) Write(map[string]any) error { r.n++; return nil }
func (r *discardRows) WriteJSON([]byte) error     { r.n++; return nil }

func TestEventsDocuments(t *testing.T) {
	ctx := context.Background()
	// fullRead is the syncer's full read of src with docs: Start, then
	// SyncDocuments of the one table with the Source Start returned.
	fullRead := func(k *eventsKind, src Source, rows RowWriter, docs DocumentWriter) error {
		start, err := k.Start(ctx, src, nil)
		if err != nil {
			return err
		}
		_, err = k.SyncDocuments(ctx, *start.Source, Table{Name: "events", PrimaryKey: []string{"id"}, Dense: true}, rows, docs)
		return err
	}
	// changes is a change read of src from cursor with docs.
	changes := func(k *eventsKind, src Source, cursor int64, docs DocumentWriter) (*pieceChanges, error) {
		w := newPieceChanges()
		w.DocumentWriter = docs
		_, err := k.Changes(ctx, src, nil, eventsCursorAt(cursor), w)
		return w, err
	}
	// mixed is a source whose snapshot holds p0 to p3, through batch 2, in
	// an order other than their arrival's, and whose suffix holds q: its
	// newest 3 are q, then p2 and p3, which arrived together.
	mixed := func(t *testing.T) *eventSource {
		src := newEventSource()
		src.send(t, `p0 {"reply":"zero"}`, `p1 {"reply":"one"}`)
		src.send(t, `p2 {"reply":"two"}`, `p3 {"reply":"three"}`)
		src.snapshot(t, 2, eventRow("p0", 1, `,"reply":"zero"`), eventRow("p3", 2, `,"reply":"three"`), eventRow("p1", 1, `,"reply":"one"`),
			eventRow("p2", 2, `,"reply":"two"`))
		src.objects.fail[src.batches[0].Key] = errors.New("batch 1 is in the snapshot: never read")
		src.objects.fail[src.batches[1].Key] = errors.New("batch 2 is in the snapshot: never read")
		src.send(t, `q {"reply":"queued"}`)
		return src
	}

	// A full read keeps the newest 3 of the suffix and the snapshot by
	// arrival, ties to the lower id, and offers them newest first, though
	// the snapshot holds them in another order. The snapshot is downloaded
	// once; the suffix's batch is opened again for its kept piece.
	t.Run("full read offers the newest of the suffix and snapshot, newest first", func(t *testing.T) {
		src := mixed(t)
		docs := &pieceDocs{}
		if err := fullRead(src.kind(wide), textSource, &fileRawRows{}, docs); err != nil {
			t.Fatal(err)
		}
		samePuts(t, docs, replyDoc("q", "queued", 3), replyDoc("p2", "two", 2), replyDoc("p3", "three", 2))
		if src.objects.opened(src.key) != 1 || src.objects.opened(src.batches[2].Key) != 2 {
			t.Fatalf("opens = %v", src.objects.opens)
		}
	})
	// The documents never change the rows: with them, without them, or
	// paused, a full read writes the same bytes in the same order.
	t.Run("full read writes the same rows with or without documents", func(t *testing.T) {
		read := func(src Source, docs DocumentWriter) []string {
			rows := &fileRawRows{}
			if err := fullRead(mixed(t).kind(wide), src, rows, docs); err != nil {
				t.Fatal(err)
			}
			return rows.rows
		}
		plain := read(testSource, nil)
		want := []string{eventRow("q", 3, `,"reply":"queued"`), eventRow("p0", 1, `,"reply":"zero"`), eventRow("p3", 2, `,"reply":"three"`),
			eventRow("p1", 1, `,"reply":"one"`), eventRow("p2", 2, `,"reply":"two"`)}
		if !slices.Equal(plain, want) {
			t.Fatalf("rows = %q", plain)
		}
		for name, got := range map[string][]string{
			"documents": read(textSource, &pieceDocs{}), "paused": read(textSource, &pieceDocs{paused: true}),
			"no stream": read(textSource, nil),
		} {
			if !slices.Equal(got, plain) {
				t.Fatalf("%s: rows = %q", name, got)
			}
		}
	})
	// The window keeps where each piece's line is, never its row: while the
	// read offers its newest 128 pieces, 8 MiB of rows, the heap holds less
	// than a quarter of that.
	t.Run("full read keeps the places of the newest pieces, not their rows", func(t *testing.T) {
		src := newEventSource()
		k := src.kind(wide)
		k.Extracted = 128
		text := strings.Repeat("x", 64<<10)
		for i := range 256 {
			src.send(t, fmt.Sprintf(`p%03d {"reply":%q}`, i, text))
		}
		var base, during runtime.MemStats
		docs := &pieceDocs{onWant: func() {
			if during.NumGC == 0 {
				runtime.GC()
				runtime.ReadMemStats(&during)
			}
		}}
		rows := &discardRows{}
		runtime.GC()
		runtime.ReadMemStats(&base)
		if err := fullRead(k, textSource, rows, docs); err != nil {
			t.Fatal(err)
		}
		if rows.n != 256 || len(docs.puts) != 128 || docs.puts[0].ExternalID != "p255" || docs.puts[127].ExternalID != "p128" {
			t.Fatalf("rows = %d, puts = %d", rows.n, len(docs.puts))
		}
		grown := int64(during.HeapAlloc) - int64(base.HeapAlloc)
		if grown > 2<<20 {
			t.Fatalf("the heap grew %d bytes by the first offer: the read holds rows", grown)
		}
		t.Logf("the heap grew %d bytes by the first offer", grown)
	})
	// A change read offers each id once, at its last line, so a piece sent
	// again within one read keeps its later body, as its row does.
	t.Run("change read offers the later body of an id sent twice", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"reply":"first"}`)
		src.send(t, `b {"reply":"other"}`, `a {"reply":"second"}`)
		docs := &pieceDocs{}
		w, err := changes(src.kind(wide), textSource, 0, docs)
		if err != nil {
			t.Fatal(err)
		}
		samePuts(t, docs, replyDoc("a", "second", 2), replyDoc("b", "other", 2))
		if len(w.upserts["events"]) != 3 {
			t.Fatalf("upserts = %q", w.upserts["events"])
		}
	})
	// A piece the stream holds at the same version is offered before the
	// rest, which stay newest first, so the writer moves it in place before
	// a new piece can trim it out of the window (connector.KeepNewest). A
	// held piece whose body changed is one of the rest.
	t.Run("offers the pieces held at their version first, then the rest newest first", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"reply":"first"}`, `b {"reply":"second"}`)
		src.send(t, `a {"reply":"first"}`, `c {"reply":"third"}`)
		src.send(t, `d {"reply":"fourth"}`)
		k := src.kind(wide)
		k.Extracted = 4
		held := map[string]string{"a": replyDoc("a", "first", 1).Version, "b": replyDoc("b", "was second", 1).Version}
		want := []Document{replyDoc("a", "first", 2), replyDoc("d", "fourth", 3), replyDoc("c", "third", 2), replyDoc("b", "second", 1)}
		docs := &pieceDocs{held: held}
		if _, err := changes(k, textSource, 0, docs); err != nil {
			t.Fatal(err)
		}
		samePuts(t, docs, want...)
		docs = &pieceDocs{held: held}
		if err := fullRead(k, textSource, &fileRawRows{}, docs); err != nil {
			t.Fatal(err)
		}
		samePuts(t, docs, want...)
	})
	// A change read past the window offers only its newest 3, newest
	// first, and opens again only the batches that hold them.
	t.Run("change read offers only its newest window and opens only its batches again", func(t *testing.T) {
		src := newEventSource()
		for i := 1; i <= 5; i++ {
			src.send(t, fmt.Sprintf(`p%d {"reply":"r%d"}`, i, i))
		}
		docs := &pieceDocs{}
		w, err := changes(src.kind(wide), textSource, 0, docs)
		if err != nil {
			t.Fatal(err)
		}
		samePuts(t, docs, replyDoc("p5", "r5", 5), replyDoc("p4", "r4", 4), replyDoc("p3", "r3", 3))
		var opens []int
		for _, b := range src.batches {
			opens = append(opens, src.objects.opened(b.Key))
		}
		if !slices.Equal(opens, []int{1, 1, 2, 2, 2}) || len(w.upserts["events"]) != 5 {
			t.Fatalf("opens = %v, upserts = %d", opens, len(w.upserts["events"]))
		}
	})
	// An identical resend renders the same, so its document keeps its
	// version and moves to its new arrival: the writer updates it in place.
	t.Run("change read offers an identical resend at its version and new arrival", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"reply":"same"}`)
		k := src.kind(wide)
		first := &pieceDocs{}
		if _, err := changes(k, textSource, 0, first); err != nil {
			t.Fatal(err)
		}
		src.send(t, `a {"reply":"same"}`)
		again := &pieceDocs{}
		if _, err := changes(k, textSource, 1, again); err != nil {
			t.Fatal(err)
		}
		samePuts(t, first, replyDoc("a", "same", 1))
		samePuts(t, again, replyDoc("a", "same", 2))
	})
	// A field a later piece adds pads earlier rows with null, which renders
	// as nothing: the earlier piece keeps its version in every read.
	t.Run("a field a later piece adds keeps an earlier piece's version", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"reply":"same"}`)
		k := src.kind(wide)
		before := &pieceDocs{}
		if _, err := changes(k, textSource, 0, before); err != nil {
			t.Fatal(err)
		}
		src.send(t, `b {"reply":"later","priority":1}`)
		after, full := &pieceDocs{}, &pieceDocs{}
		if _, err := changes(k, textSource, 0, after); err != nil {
			t.Fatal(err)
		}
		if err := fullRead(k, textSource, &fileRawRows{}, full); err != nil {
			t.Fatal(err)
		}
		samePuts(t, before, replyDoc("a", "same", 1))
		for _, docs := range []*pieceDocs{after, full} {
			if len(docs.puts) != 2 || docs.puts[1].ExternalID != "a" || docs.puts[1].Version != before.puts[0].Version {
				t.Fatalf("puts = %+v", docs.puts)
			}
		}
	})
	// A paused stream takes nothing: the read writes its rows, opens no
	// batch again, and neither renders nor offers a document. So does a
	// source without a text field.
	for name, c := range map[string]struct {
		src  Source
		docs *pieceDocs
	}{
		"a paused stream":      {textSource, &pieceDocs{paused: true}},
		"no text field":        {testSource, &pieceDocs{}},
		"a cleared text field": {Source{ConnectorID: "conn-1", Settings: json.RawMessage(`{"text_field":""}`)}, &pieceDocs{}},
	} {
		t.Run("change and full reads offer nothing for "+name, func(t *testing.T) {
			src := newEventSource()
			src.send(t, `a {"reply":"one"}`)
			src.send(t, `b {"reply":"two"}`)
			k := src.kind(wide)
			w, err := changes(k, c.src, 0, c.docs)
			if err != nil || len(w.upserts["events"]) != 2 {
				t.Fatalf("Changes = %v, upserts %q", err, w.upserts["events"])
			}
			src.snapshot(t, 1, eventRow("a", 1, `,"reply":"one"`))
			rows := &fileRawRows{}
			if err := fullRead(k, c.src, rows, c.docs); err != nil || len(rows.rows) != 2 {
				t.Fatalf("full read = %v, rows %q", err, rows.rows)
			}
			if len(c.docs.offers) != 0 {
				t.Fatalf("offers = %q", c.docs.offers)
			}
			// The change read opens each batch once, and the full read the
			// one above the snapshot's cursor and the snapshot.
			if src.objects.opened(src.batches[0].Key) != 1 || src.objects.opened(src.batches[1].Key) != 2 || src.objects.opened(src.key) != 1 {
				t.Fatalf("opens = %v", src.objects.opens)
			}
		})
	}
	// A batch read again is read as the first pass read it: one gone from
	// storage, or whose line is another piece's, fails the sync for good,
	// and S3 throttling is retried. Nothing is offered.
	for _, c := range []struct {
		name    string
		broken  func(o *eventObjects, key string)
		refusal string
	}{
		{"fails for good on a batch gone when read again", func(o *eventObjects, key string) {
			o.again[key] = fmt.Errorf("get s3://bucket/%s: %w", key, snapshot.ErrMissing)
		}, "batch 2 of this source is missing from storage"},
		{"fails for good on a batch whose line is another piece's when read again", func(o *eventObjects, key string) {
			o.reopened[key] = o.objects[strings.Replace(key, "batch-2", "batch-1", 1)]
		}, `batch 2 of this source can't be read: its line 1 is piece "a", not "b"`},
		{"retries a throttled read of a batch read again", func(o *eventObjects, key string) {
			o.again[key] = errors.New("SlowDown: please reduce your request rate")
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, read := range []string{"change", "full"} {
				src := newEventSource()
				src.send(t, `a {"reply":"one"}`)
				src.send(t, `b {"reply":"one"}`)
				c.broken(src.objects, src.batches[1].Key)
				docs := &pieceDocs{}
				var err error
				if read == "change" {
					_, err = changes(src.kind(wide), textSource, 0, docs)
				} else {
					err = fullRead(src.kind(wide), textSource, &fileRawRows{}, docs)
				}
				var invalidErr *InvalidError
				switch {
				case c.refusal != "" && invalidMessage(t, err) != c.refusal:
					t.Fatalf("%s read: message = %q", read, invalidMessage(t, err))
				case c.refusal == "" && (err == nil || errors.As(err, &invalidErr)):
					t.Fatalf("%s read: error = %v, want a retryable one", read, err)
				case len(docs.offers) != 0:
					t.Fatalf("%s read: offers = %q", read, docs.offers)
				}
			}
		})
	}
	// The backend locks a text field as a string with the field itself: a
	// stored field the schema holds otherwise is settings no backend
	// writes, and fails the sync for good rather than empty the set.
	t.Run("fails for good on a text field its schema does not hold as text", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"reply":7}`)
		_, changeErr := changes(src.kind(wide), textSource, 0, &pieceDocs{})
		fullErr := fullRead(src.kind(wide), textSource, &fileRawRows{}, &pieceDocs{})
		for _, err := range []error{changeErr, fullErr} {
			if msg := invalidMessage(t, err); msg != `this source's text field "reply" does not hold text: choose its text field again` {
				t.Fatalf("message = %q", msg)
			}
		}
	})
	// Want decides each piece: one it refuses is not Put, and the others
	// are. A Put's error, such as the store failing, ends the read with it.
	t.Run("puts only what Want takes, and passes a Put's error on", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"reply":"one"}`, `b {"reply":"two"}`)
		docs := &pieceDocs{refuse: map[string]bool{"a": true}}
		if _, err := changes(src.kind(wide), textSource, 0, docs); err != nil || len(docs.offers) != 2 || len(docs.puts) != 1 ||
			docs.puts[0].ExternalID != "b" {
			t.Fatalf("Changes = %v, offers %q, puts %+v", err, docs.offers, docs.puts)
		}
		failing := &pieceDocs{fail: errors.New("store documents: connection reset")}
		_, changeErr := changes(src.kind(wide), textSource, 0, failing)
		fullErr := fullRead(src.kind(wide), textSource, &fileRawRows{}, failing)
		for _, err := range []error{changeErr, fullErr} {
			if !errors.Is(err, failing.fail) {
				t.Fatalf("error = %v, want the Put's", err)
			}
		}
		if len(failing.puts) != 2 {
			t.Fatalf("puts = %d, want the first of each read", len(failing.puts))
		}
	})
	// A source no piece has reached keeps no documents: a full read offers
	// none, so the syncer removes any its set holds. Its text field is in
	// its schema, locked with the field before any piece carries it.
	t.Run("full read of a source with no pieces offers nothing", func(t *testing.T) {
		src := newEventSource()
		var err error
		if src.schema, err = src.schema.WithText("reply", 200); err != nil {
			t.Fatal(err)
		}
		docs := &pieceDocs{}
		rows := &fileRawRows{}
		if err := fullRead(src.kind(wide), textSource, rows, docs); err != nil || len(rows.rows) != 0 || len(docs.offers) != 0 {
			t.Fatalf("full read = %v, rows %q, offers %q", err, rows.rows, docs.offers)
		}
	})
	// The window orders a snapshot's rows by received_at, which a row the
	// syncer wrote always holds: a read that keeps documents stops at a row
	// without it, or holding a field its schema lacks, rather than misplace
	// or misrender a piece. A read that keeps none copies the rows as it
	// always has.
	for _, c := range []struct {
		name, row, refusal string
	}{
		{"no received_at", `{"id":"a","occurred_at":null,"reply":"one"}`, "a row is not a piece's"},
		{"a received_at that is not a time", `{"id":"a","received_at":"yesterday","occurred_at":null,"reply":"one"}`, "a row is not a piece's"},
		{"a received_at that is not text", `{"id":"a","received_at":7,"occurred_at":null,"reply":"one"}`, "a row is not a piece's"},
		{"a field its schema lacks", eventRow("a", 1, `,"reply":"one","zzz":1`), `it holds "zzz", which its source's schema lacks`},
	} {
		t.Run("full read that keeps documents stops at a snapshot row with "+c.name, func(t *testing.T) {
			src := newEventSource()
			src.send(t, `a {"reply":"one"}`)
			src.snapshot(t, 1, c.row)
			docs := &pieceDocs{}
			err := fullRead(src.kind(wide), textSource, &fileRawRows{}, docs)
			var invalidErr *InvalidError
			if err == nil || !strings.Contains(err.Error(), c.refusal) || errors.As(err, &invalidErr) || len(docs.puts) != 0 {
				t.Fatalf("error = %v, puts %d", err, len(docs.puts))
			}
			rows := &fileRawRows{}
			if err := fullRead(src.kind(wide), testSource, rows, nil); err != nil || len(rows.rows) != 1 {
				t.Fatalf("full read without documents = %v, rows %q", err, rows.rows)
			}
		})
	}
	// A snapshot is the syncer's own object: one that is not gzip fails the
	// read, to be retried, whether or not it keeps documents.
	t.Run("full read retries a snapshot that is not gzip", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"reply":"one"}`)
		src.snapshot(t, 1, eventRow("a", 1, `,"reply":"one"`))
		src.objects.objects[src.key] = []byte(eventRow("a", 1, `,"reply":"one"`))
		for _, docs := range []DocumentWriter{nil, &pieceDocs{}} {
			err := fullRead(src.kind(wide), textSource, &fileRawRows{}, docs)
			var invalidErr *InvalidError
			if err == nil || errors.As(err, &invalidErr) {
				t.Fatalf("error = %v, want a retryable one", err)
			}
		}
	})
	// A piece whose text field is null, absent or blank is still a
	// document, titled by its id.
	t.Run("offers a piece without text, titled by its id", func(t *testing.T) {
		src := newEventSource()
		src.send(t, `a {"reply":"text"}`, `b {"reply":null}`, `c {"other":1}`)
		docs := &pieceDocs{}
		if _, err := changes(src.kind(wide), textSource, 0, docs); err != nil {
			t.Fatal(err)
		}
		if len(docs.puts) != 3 || docs.puts[1].Title != "b" || string(docs.puts[2].Text) != "# c\n\n- Piece: c\n- other: 1\n" {
			t.Fatalf("puts = %+v", docs.puts)
		}
	})
}
