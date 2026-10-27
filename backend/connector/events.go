package connector

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/helixdb/helix-foundry/backend/events"
	"github.com/helixdb/helix-foundry/backend/snapshot"
)

var (
	_ Pushed       = (*eventsKind)(nil)
	_ DocumentKind = (*eventsKind)(nil)
)

// eventsTable is an event source's one table: the newest piece of each id.
const eventsTable = "events"

// EventsStream is the stream of an event source with a text field: each of
// its newest pieces as a Markdown document of the source's own set.
const EventsStream = "pieces"

// reopenBatches is how many batches a read opens at once to render the
// pieces it keeps as documents.
const reopenBatches = 8

// maxEventsDescription is the most characters an event source's description holds.
const maxEventsDescription = 1000

// eventsThrough is the setting only the Source Start returns carries, never
// stored: the seq of the newest batch its full read takes.
const eventsThrough = "full_read_through"

// EventBatches is what the events kind reads of a source; Foundry's store
// implements it. Each method reads in one consistent transaction, so every
// batch it returns holds only fields of the schema it returns.
type EventBatches interface {
	// EventBatchesAfter returns the source's schema and its batches with seq
	// above after, oldest first, within budget (always at least one batch
	// when any waits).
	EventBatchesAfter(ctx context.Context, connectorID string, after int64, budget events.ReadBudget) (events.Schema, []events.Batch, error)
	// EventTable returns the schema, the stored cursor's seq (0 for none) and
	// the key of the events table's snapshot ("" for none).
	EventTable(ctx context.Context, connectorID string) (events.Schema, int64, string, error)
	// EventBacklog says what waits above after: nothing, less than one read's
	// budget, or more.
	EventBacklog(ctx context.Context, connectorID string, after int64, budget events.ReadBudget) (Backlog, error)
}

// EventsOptions wire the events kind to its sources' batches: Batches reads
// their rows, Objects holds their objects and the tables' snapshots, and
// Budget bounds one read (foundry.events max_batches_per_sync,
// max_pieces_per_sync and max_bytes_per_sync). Extracted is how many of a
// source's newest pieces its own set holds as documents
// (foundry.events.max_extracted): at least 1 wherever a source with a text
// field syncs, since its stream keeps KeepNewest(Extracted). Zero options
// make a kind that discovers but cannot sync, which is all the backend's
// catalog needs.
type EventsOptions struct {
	Batches   EventBatches
	Objects   snapshot.Store
	Budget    events.ReadBudget
	Extracted int
}

// errEventsUnwired is a sync asked of the events kind of a catalog built
// without EventsOptions.
var errEventsUnwired = errors.New("connector: this catalog's events kind discovers but cannot sync")

// EventsSettings are what a request sets of an event source: Prepare takes
// and returns these alone, each only when the request carries it, so an
// update stores just what it sets and leaves the rest as they are. The
// stored config also holds the schema the source's sends learned,
// {"schema": {...}}, which only a send, or the lock of a text field
// (events.Schema.WithText), writes.
type EventsSettings struct {
	// Description is what each piece is, for people and Foundry's agent;
	// nil when the request leaves it as it is.
	Description *string `json:"description,omitempty"`
	// TextField names the top-level field that holds each piece's text:
	// with one, the source keeps its newest pieces as documents of its own
	// set (EventsStream), which entity extraction reads. "" clears it; nil
	// leaves it as it is.
	TextField *string `json:"text_field,omitempty"`
}

// eventsStored is what a sync reads of an event source's stored settings
// beside its schema: an empty TextField is a source with no documents.
type eventsStored struct {
	Description string `json:"description"`
	TextField   string `json:"text_field"`
}

// eventsCursor is where an event source's next change read starts: the seq
// of the newest batch its table holds.
type eventsCursor struct {
	Seq *int64 `json:"seq"`
}

func eventsCursorAt(seq int64) json.RawMessage { return encodeJSON(eventsCursor{Seq: &seq}) }

func readEventsCursor(cursor json.RawMessage) (int64, error) {
	var c eventsCursor
	if err := json.Unmarshal(cursor, &c); err != nil || c.Seq == nil || *c.Seq < 0 {
		return 0, fmt.Errorf("read event source cursor %s: not {\"seq\": n}", cursor)
	}
	return *c.Seq, nil
}

// eventsKind is an event source: pieces of JSON, each with an id, that people
// and their agents send to Foundry. Each send is a batch, one gzipped NDJSON
// object named by its foundry_event_batches row. Nothing changes at a
// source, so it is Pushed: a change read merges the batches above the cursor
// by id into the one table, events, every row padded to the schema the
// source has learned (Table.Dense). A source with a text field also keeps
// its newest pieces, Extracted of them by arrival, as Markdown documents of
// its own set (EventsStream): each read keeps where its newest pieces' lines
// are, then reads those lines again to render them (offerPieces).
type eventsKind struct{ EventsOptions }

func (k *eventsKind) Name() string        { return "events" }
func (k *eventsKind) DisplayName() string { return "API source" }

// Hint is empty: an event source has no credential. Its send tokens are
// shown by their own prefix.
func (k *eventsKind) Hint(string) string { return "" }

// Prepare takes a description, trimmed and at most 1,000 characters, a text
// field, a name a piece's field could have (events.ValidFieldName) or "" to
// clear it, and nothing else; no credential. Each is returned only when the
// request carries it. The schema only ever comes from sends, and the text
// field's lock as a string from the backend, under the row lock a send takes.
func (k *eventsKind) Prepare(src Source) (json.RawMessage, error) {
	if src.Secret != "" {
		return nil, invalid("an API source takes no key")
	}
	var s EventsSettings
	if len(bytes.TrimSpace(src.Settings)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(src.Settings))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return nil, invalid("an API source takes a description and a text field, and no other settings")
		}
	}
	if s.Description != nil {
		description := strings.TrimSpace(*s.Description)
		if n := utf8.RuneCountInString(description); n > maxEventsDescription {
			return nil, invalid("a description holds at most %d characters; this one holds %d", maxEventsDescription, n)
		}
		s.Description = &description
	}
	if s.TextField != nil && *s.TextField != "" {
		if err := events.ValidFieldName(*s.TextField); err != nil {
			return nil, invalid("%s", err)
		}
	}
	return encodeJSON(s), nil
}

// Streams is EventsStream when the stored settings name a text field, and
// none otherwise: clearing the field empties the set at the next sync. Its
// set is "Pieces", described by the source's description, and holds the
// source's newest Extracted pieces by arrival.
func (k *eventsKind) Streams(src Source) []Stream {
	var stored eventsStored
	_ = json.Unmarshal(orEmpty(src.Settings), &stored) // stored by Prepare and sends
	if stored.TextField == "" {
		return nil
	}
	return []Stream{{Name: EventsStream, Table: eventsTable, Title: "Pieces", Description: stored.Description, Format: Markdown,
		Keep: KeepNewest(k.Extracted)}}
}

// pieceText is the text field a read of src renders its kept pieces from, or
// "" when the read keeps no documents: src names none, docs is nil (a read
// with no stream on), or the stream is Paused. A text field the schema does
// not hold as a string is settings no backend writes (the lock comes with
// the field): it fails the sync for good, where offering nothing would
// empty the set.
func pieceText(src Source, schema events.Schema, docs DocumentWriter) (string, error) {
	var stored eventsStored
	if docs == nil || docs.Paused(EventsStream) || json.Unmarshal(orEmpty(src.Settings), &stored) != nil || stored.TextField == "" {
		return "", nil
	}
	if !slices.Contains(schema.Fields(), events.Field{Name: stored.TextField, Type: events.TypeString}) {
		return "", invalid("this source's text field %q does not hold text: choose its text field again", stored.TextField)
	}
	return stored.TextField, nil
}

// Discover lists the one table, events, keyed by id, with the columns of the
// schema the settings hold. It reads nothing, so it never fails on the
// network and leaves nothing unavailable.
func (k *eventsKind) Discover(_ context.Context, src Source) (Discovery, error) {
	var stored struct {
		Schema events.Schema `json:"schema"`
	}
	if len(src.Settings) > 0 {
		if err := json.Unmarshal(src.Settings, &stored); err != nil {
			return Discovery{}, fmt.Errorf("read event source settings: %w", err)
		}
	}
	return Discovery{Tables: []Table{{Name: eventsTable, Columns: eventColumns(stored.Schema), PrimaryKey: []string{"id"}, Dense: true}}}, nil
}

// eventColumns are an events table's columns under schema s, each typed with
// the JSON type it holds: id and received_at are on every row, occurred_at
// and every field null where a piece has none.
func eventColumns(s events.Schema) []Column {
	fields := s.Columns()
	columns := make([]Column, len(fields))
	for i, f := range fields {
		columns[i] = Column{Name: f.Name, Type: f.Type.String(), Nullable: f.Name != "id" && f.Name != "received_at"}
	}
	return columns
}

func (k *eventsKind) Capture(_ Source, table string) Capture {
	if table == eventsTable {
		return CaptureEvents
	}
	return CaptureFull
}

// tableCursor is the seq the table's rows stand at: the stored cursor's, or 0
// for a table without a snapshot, which holds nothing whatever the cursor
// says, so a full read of it replays from the first batch.
func tableCursor(stored int64, key string) int64 {
	if key == "" {
		return 0
	}
	return stored
}

// Start walks the batches above the table's cursor that one read takes, as
// the full read will, and returns the newest one's seq as the cursor, or
// the table's when none waits. The Source it returns carries that seq, and
// the full read takes exactly the batches up to it: EventBatchesAfter cuts at
// the budget by the schema it reads, which a send may grow meanwhile, so the
// full read cannot simply walk again.
func (k *eventsKind) Start(ctx context.Context, src Source, _ []Table) (Start, error) {
	if k.Batches == nil || k.Objects == nil {
		return Start{}, errEventsUnwired
	}
	_, stored, key, err := k.Batches.EventTable(ctx, src.ConnectorID)
	if err != nil {
		return Start{}, err
	}
	through := tableCursor(stored, key)
	_, batches, err := k.Batches.EventBatchesAfter(ctx, src.ConnectorID, through, k.Budget)
	if err != nil {
		return Start{}, err
	}
	if len(batches) > 0 {
		through = batches[len(batches)-1].Seq
	}
	var settings map[string]json.RawMessage
	if len(src.Settings) > 0 {
		if err := json.Unmarshal(src.Settings, &settings); err != nil {
			return Start{}, fmt.Errorf("read event source settings: %w", err)
		}
	}
	if settings == nil {
		settings = map[string]json.RawMessage{}
	}
	settings[eventsThrough] = encodeJSON(through)
	read := src
	read.Settings = encodeJSON(settings)
	return Start{Cursor: eventsCursorAt(through), Source: &read}, nil
}

// Sync is SyncDocuments with no stream on: rows only.
func (k *eventsKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	return k.SyncDocuments(ctx, src, table, w, nil)
}

// SyncDocuments reads the table in full: the batches Start walked, newest
// first and each one's lines last first, writing each id's first sighting,
// its newest piece, then every row of the table's snapshot whose id it has
// not written. A source without a snapshot is replayed from its batches
// alone. Every row is padded to the newest schema read, which holds every
// field of every batch read, and the table returned has that schema's
// columns. With a text field, and docs neither nil nor Paused, each row
// written also offers its piece to a window of the Extracted newest by
// arrival, which holds where the piece's line is and never its row; the
// read then offers the window's pieces as documents (offerPieces), reading
// their batches and the downloaded snapshot again. The rows are the same
// either way.
func (k *eventsKind) SyncDocuments(ctx context.Context, src Source, table Table, w RowWriter, docs DocumentWriter) (Table, error) {
	if k.Batches == nil || k.Objects == nil {
		return Table{}, errEventsUnwired
	}
	var bound struct {
		Through *int64 `json:"full_read_through"` // eventsThrough
	}
	if err := json.Unmarshal(src.Settings, &bound); err != nil || bound.Through == nil {
		return Table{}, errors.New("a full read of an event source takes the Source its Start returned")
	}
	schema, stored, key, err := k.Batches.EventTable(ctx, src.ConnectorID)
	if err != nil {
		return Table{}, err
	}
	// Each read is cut by its own schema, so a later one may take fewer
	// batches than Start's did: read on until Start's bound.
	var batches []events.Batch
	for after := tableCursor(stored, key); after < *bound.Through; {
		read, next, err := k.Batches.EventBatchesAfter(ctx, src.ConnectorID, after, k.Budget)
		if err != nil {
			return Table{}, err
		}
		if len(next) == 0 {
			return Table{}, fmt.Errorf("event source batches %d to %d are gone", after+1, *bound.Through)
		}
		schema = read
		for _, b := range next {
			if b.Seq > *bound.Through {
				break
			}
			batches = append(batches, b)
		}
		after = next[len(next)-1].Seq
	}
	textField, err := pieceText(src, schema, docs)
	if err != nil {
		return Table{}, err
	}
	// window keeps where the newest pieces' lines are; nil when the read
	// keeps no documents.
	var window *events.Window[pieceLine]
	if textField != "" {
		window = events.Newest[pieceLine](k.Extracted)
	}
	// seen holds the 16-byte hash of each id written: a full read of a large
	// table keeps no rows in memory.
	seen := map[[16]byte]bool{}
	for i := len(batches) - 1; i >= 0; i-- {
		lines, err := k.batchLines(ctx, batches[i])
		if err != nil {
			return Table{}, err
		}
		for j := len(lines) - 1; j >= 0; j-- {
			if h := idHash(lines[j].ID()); !seen[h] {
				seen[h] = true
				row, err := lines[j].Row(schema)
				if err != nil {
					return Table{}, corruptBatch(batches[i].Seq, err)
				}
				if err := w.WriteJSON(row); err != nil {
					return Table{}, err
				}
				if window != nil {
					window.Push(lines[j].ReceivedAt(), lines[j].ID(), pieceLine{batch: i, line: j})
				}
			}
		}
	}
	path := ""
	if key != "" {
		if path, err = snapshot.Download(ctx, k.Objects, key, ""); err != nil {
			return Table{}, fmt.Errorf("read the snapshot of an event source's table: %w", err)
		}
		// The documents read it again, so it stays until the read ends.
		defer func() { _ = os.Remove(path) }() // best-effort: scratch file
		if err := copySnapshot(path, key, schema, func(id string) bool { return seen[idHash(id)] }, w, window); err != nil {
			return Table{}, err
		}
	}
	if window != nil {
		if err := k.offerPieces(ctx, window.Pieces(), batches, path, key, schema, textField, docs); err != nil {
			return Table{}, err
		}
	}
	table.Columns = eventColumns(schema)
	return table, nil
}

// pieceLine is where a read finds a piece it keeps again: line line of the
// read's batch batch, or, when batch is fromSnapshot, row line of the
// table's snapshot, both from 0.
type pieceLine struct{ batch, line int }

const fromSnapshot = -1

// copySnapshot writes every row of the table's snapshot, downloaded from
// key to path, whose id written does not hold, each padded to schema's
// columns as a change read's merge pads a Dense table's rows:
// `,"<name>":null` appended before its closing brace for each it lacks, so a
// row that lacks none keeps its bytes. With a window, each row written also
// offers the window its piece, by its received_at and its row's place.
func copySnapshot(path, key string, schema events.Schema, written func(id string) bool, w RowWriter, window *events.Window[pieceLine]) error {
	columns := schema.Columns()
	names := make([][]byte, len(columns))
	for i, c := range columns {
		var name bytes.Buffer
		enc := json.NewEncoder(&name)
		enc.SetEscapeHTML(false) // as rows write names
		_ = enc.Encode(c.Name)   // a string always encodes
		names[i] = bytes.TrimSuffix(name.Bytes(), []byte("\n"))
	}
	notPiece := fmt.Errorf("read snapshot %s: a row is not a piece's", key)
	return snapshotRows(path, key, func(n int, line []byte) error {
		var fields map[string]json.RawMessage
		var id string
		if jerr := json.Unmarshal(line, &fields); jerr != nil || json.Unmarshal(fields["id"], &id) != nil || !bytes.HasSuffix(line, []byte("}")) {
			return notPiece
		}
		if written(id) {
			return nil
		}
		if window != nil {
			var received string
			if json.Unmarshal(fields["received_at"], &received) != nil {
				return notPiece
			}
			at, err := time.Parse(time.RFC3339Nano, received)
			if err != nil {
				return notPiece
			}
			window.Push(at, id, pieceLine{batch: fromSnapshot, line: n})
		}
		row := slices.Clone(line[:len(line)-1])
		for i, c := range columns {
			if _, ok := fields[c.Name]; !ok {
				row = append(append(append(row, ','), names[i]...), ":null"...)
			}
		}
		return w.WriteJSON(append(row, '}'))
	})
}

// snapshotRows calls fn with each row of the table's snapshot, downloaded
// from key to path, and its place among the rows from 0, in the order the
// snapshot holds them; the first error fn returns ends the read.
func snapshotRows(path, key string, fn func(n int, row []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // best-effort: only read
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<16))
	if err != nil {
		return fmt.Errorf("read snapshot %s: %w", key, err)
	}
	r := bufio.NewReaderSize(gz, 1<<16)
	for n := 0; ; {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSuffix(line, []byte("\n")); len(line) > 0 {
			if ferr := fn(n, line); ferr != nil {
				return ferr
			}
			n++
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read snapshot %s: %w", key, err)
		}
	}
}

// Changes merges the batches above the cursor that one read takes: it
// declares the typed columns of the schema it read, then upserts every
// line of each batch in seq order, padded to that schema. The last piece of
// an id wins, as a full read's does. It returns the newest batch's seq, or
// the cursor unchanged when none waits, and never resets the table. With a
// text field, and the stream not Paused, it also keeps where each id's last
// line is (one entry per id, so at most max_pieces_per_sync), then offers
// the Extracted newest of those pieces by arrival as documents, reading only
// their batches again (offerPieces); the rest of the read's pieces are never
// offered.
func (k *eventsKind) Changes(ctx context.Context, src Source, _ []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	if k.Batches == nil || k.Objects == nil {
		return nil, errEventsUnwired
	}
	columns, ok := w.(ColumnWriter)
	if !ok {
		return nil, errors.New("an event source's change read declares its columns: the writer must be a ColumnWriter")
	}
	after, err := readEventsCursor(cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCursorExpired, err)
	}
	schema, batches, err := k.Batches.EventBatchesAfter(ctx, src.ConnectorID, after, k.Budget)
	if err != nil {
		return nil, err
	}
	columns.Columns(eventsTable, eventColumns(schema))
	if len(batches) == 0 {
		return cursor, nil
	}
	textField, err := pieceText(src, schema, w)
	if err != nil {
		return nil, err
	}
	// last is where each id's last line is, by its hash; nil when the read
	// keeps no documents.
	var last map[[16]byte]events.Kept[pieceLine]
	if textField != "" {
		last = map[[16]byte]events.Kept[pieceLine]{}
	}
	for i, b := range batches {
		lines, err := k.batchLines(ctx, b)
		if err != nil {
			return nil, err
		}
		for j, l := range lines {
			row, err := l.Row(schema)
			if err != nil {
				return nil, corruptBatch(b.Seq, err)
			}
			if err := w.UpsertJSON(eventsTable, row); err != nil {
				return nil, err
			}
			if last != nil {
				last[idHash(l.ID())] = events.Kept[pieceLine]{ReceivedAt: l.ReceivedAt(), ID: l.ID(), Loc: pieceLine{batch: i, line: j}}
			}
		}
	}
	if last != nil {
		window := events.Newest[pieceLine](k.Extracted)
		for _, p := range last {
			window.Push(p.ReceivedAt, p.ID, p.Loc)
		}
		if err := k.offerPieces(ctx, window.Pieces(), batches, "", "", schema, textField, w); err != nil {
			return nil, err
		}
	}
	return eventsCursorAt(batches[len(batches)-1].Seq), nil
}

// offerPieces offers a read's kept pieces to docs as documents of
// EventsStream: each with the Markdown its row renders to (events.Render,
// from textField), version the body's SHA-256, its arrival as At, and its
// title. It reads each kept line again where kept says: the read's batches
// holding one, opened again 8 at a time and each once, its line padded to
// schema; the snapshot of key downloaded to path, read once for all its
// kept rows. The documents are rendered in that order into a scratch file,
// then read back one at a time to be offered, so memory holds where each
// one is, never all of them. The pieces whose version the stream holds
// (Held) go first, then the rest, each in kept's order, newest first: a
// resend is then an update in place, or kept, before any new piece of the
// read trims the window, so no trim removes a piece the read offers again
// (connector.KeepNewest). A line that is not the kept piece's, or a row
// that does not render, is a corrupt batch (or snapshot), as the read's
// first pass treats it.
func (k *eventsKind) offerPieces(ctx context.Context, kept []events.Kept[pieceLine], batches []events.Batch, path, key string,
	schema events.Schema, textField string, docs DocumentWriter) error {
	if len(kept) == 0 {
		return nil
	}
	scratch, err := os.CreateTemp("", "foundry-pieces-*")
	if err != nil {
		return fmt.Errorf("render pieces: %w", err)
	}
	defer func() { _ = scratch.Close(); _ = os.Remove(scratch.Name()) }() // best-effort: scratch file
	// spooled is where kept piece i's title and body are in scratch, and
	// the body's version.
	type spooled struct {
		at          int64
		title, body int
		version     string
	}
	spool := make([]spooled, len(kept))
	var mu sync.Mutex
	var end int64
	write := func(i int, doc events.Rendered) error {
		mu.Lock()
		defer mu.Unlock()
		if _, err := scratch.Write(append([]byte(doc.Title), doc.Body...)); err != nil {
			return fmt.Errorf("render pieces: %w", err)
		}
		spool[i] = spooled{at: end, title: len(doc.Title), body: len(doc.Body), version: events.Version(doc.Body)}
		end += int64(len(doc.Title) + len(doc.Body))
		return nil
	}
	inBatch, inSnapshot := map[int][]int{}, map[int]int{}
	for i, p := range kept {
		if p.Loc.batch == fromSnapshot {
			inSnapshot[p.Loc.line] = i
		} else {
			inBatch[p.Loc.batch] = append(inBatch[p.Loc.batch], i)
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(reopenBatches)
	for b, picks := range inBatch {
		g.Go(func() error {
			lines, err := k.batchLines(gctx, batches[b])
			if err != nil {
				return err
			}
			for _, i := range picks {
				line := lines[kept[i].Loc.line]
				if line.ID() != kept[i].ID {
					return corruptBatch(batches[b].Seq, fmt.Errorf("its line %d is piece %q, not %q", kept[i].Loc.line+1, line.ID(), kept[i].ID))
				}
				row, err := line.Row(schema)
				var doc events.Rendered
				if err == nil {
					doc, err = events.Render(row, schema, textField)
				}
				if err != nil {
					return corruptBatch(batches[b].Seq, err)
				}
				if err := write(i, doc); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	if len(inSnapshot) > 0 {
		found := 0
		err := snapshotRows(path, key, func(n int, row []byte) error {
			i, ok := inSnapshot[n]
			if !ok {
				return nil
			}
			found++
			var piece struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(row, &piece) != nil || piece.ID != kept[i].ID {
				return fmt.Errorf("read snapshot %s: its row %d is not piece %q", key, n+1, kept[i].ID)
			}
			doc, err := events.Render(row, schema, textField)
			if err != nil {
				return fmt.Errorf("read snapshot %s: %w", key, err)
			}
			return write(i, doc)
		})
		if err == nil && found != len(inSnapshot) {
			err = fmt.Errorf("read snapshot %s: it holds %d of the %d rows kept", key, found, len(inSnapshot))
		}
		if err != nil {
			return err
		}
	}
	var held, rest []int
	for i, p := range kept {
		if version, ok := docs.Held(EventsStream, p.ID); ok && version == spool[i].version {
			held = append(held, i)
		} else {
			rest = append(rest, i)
		}
	}
	for _, i := range append(held, rest...) {
		s, p := spool[i], kept[i]
		if !docs.Want(EventsStream, p.ID, s.version) {
			continue
		}
		buf := make([]byte, s.title+s.body)
		if _, err := scratch.ReadAt(buf, s.at); err != nil {
			return fmt.Errorf("render pieces: %w", err)
		}
		if err := docs.Put(ctx, EventsStream, Document{ExternalID: p.ID, Version: s.version, At: p.ReceivedAt, Title: string(buf[:s.title]), Text: buf[s.title:]}); err != nil {
			return err
		}
	}
	return nil
}

// Backlog says what waits beyond cursor, the one the sync just stored.
func (k *eventsKind) Backlog(ctx context.Context, src Source, cursor json.RawMessage) (Backlog, error) {
	if k.Batches == nil || k.Objects == nil {
		return BacklogNone, errEventsUnwired
	}
	after, err := readEventsCursor(cursor)
	if err != nil {
		return BacklogNone, err
	}
	return k.Batches.EventBacklog(ctx, src.ConnectorID, after, k.Budget)
}

// batchLines reads batch b's object whole and decodes its lines. Every
// committed batch's object was stored before its row, and S3 reads are
// consistent after a write, so an object that is missing, or is not the
// b.Pieces lines of b.Bytes bytes its row records, fails the sync for good:
// retrying cannot bring its pieces back. Any other error, such as S3
// throttling or a dropped connection, is retried.
func (k *eventsKind) batchLines(ctx context.Context, b events.Batch) ([]events.Line, error) {
	body, err := k.Objects.Open(ctx, b.Key)
	if errors.Is(err, snapshot.ErrMissing) {
		return nil, invalid("batch %d of this source is missing from storage", b.Seq)
	}
	if err != nil {
		return nil, fmt.Errorf("read event batch %d: %w", b.Seq, err)
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	gz, err := gzip.NewReader(body)
	var data []byte
	if err == nil {
		// One byte past the recorded size tells a longer object apart
		// without reading it all; a whole one is read to its checksum.
		data, err = io.ReadAll(io.LimitReader(gz, b.Bytes+1))
	}
	var flateErr flate.CorruptInputError
	switch {
	// An empty object (io.EOF before a header) or a broken gzip stream is
	// not one a send wrote. A truncated stream reads as io.ErrUnexpectedEOF
	// whether the object or the connection was cut, so it is retried.
	case errors.Is(err, io.EOF) || errors.Is(err, gzip.ErrHeader) || errors.Is(err, gzip.ErrChecksum) || errors.As(err, &flateErr):
		return nil, corruptBatch(b.Seq, err)
	case err != nil:
		return nil, fmt.Errorf("read event batch %d: %w", b.Seq, err)
	case int64(len(data)) != b.Bytes:
		return nil, corruptBatch(b.Seq, fmt.Errorf("its object is not the %d bytes its row records", b.Bytes))
	}
	raw := bytes.Split(data, []byte("\n"))
	// A whole object ends with a newline, so the split leaves one empty tail.
	if len(raw) != b.Pieces+1 || len(raw[b.Pieces]) != 0 {
		return nil, corruptBatch(b.Seq, fmt.Errorf("its object is not the %d lines its row records", b.Pieces))
	}
	lines := make([]events.Line, b.Pieces)
	for i := range lines {
		if lines[i], err = events.DecodeLine(raw[i]); err != nil {
			return nil, corruptBatch(b.Seq, err)
		}
	}
	return lines, nil
}

// corruptBatch fails a sync for good on a batch no send could have stored.
func corruptBatch(seq int64, err error) error {
	return invalid("batch %d of this source can't be read: %v", seq, err)
}
