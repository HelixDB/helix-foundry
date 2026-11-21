package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/helixdb/helix-foundry/backend/events"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// piecesHarness is an event harness with Notes, a source whose text field is
// note, and the rows of its table in the order a sync wrote them.
type piecesHarness struct {
	*eventHarness
	notes string
	rows  map[string][]byte
	order []string
}

func newPiecesHarness(t *testing.T) *piecesHarness {
	t.Helper()
	h := &piecesHarness{eventHarness: newEventHarness(t, nil), rows: map[string][]byte{}}
	resp, err := h.s.CreateConnector(ports.WithActor(context.Background(), person), &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Notes",
		Config: eventsText(proto.String("What agents learned"), proto.String("note"))})
	if err != nil {
		t.Fatal(err)
	}
	h.notes = resp.GetConnector().GetId()
	return h
}

// note sends one batch of pieces, each "id=text", a minute after the last.
func (h *piecesHarness) note(t *testing.T, pieces ...string) {
	t.Helper()
	h.clock.advance(time.Minute)
	sent := make([]string, len(pieces))
	for i, p := range pieces {
		id, text, _ := strings.Cut(p, "=")
		sent[i] = fmt.Sprintf(`%s={"note":%q}`, id, text)
	}
	if _, err := h.send(h.notes, "", sent...); err != nil {
		t.Fatal(err)
	}
}

// sync merges the source's waiting batches into its table as a sync does,
// the newest piece of each id, then moves its cursor past them.
func (h *piecesHarness) sync(t *testing.T) {
	t.Helper()
	state, err := h.store.state("ws-1", h.notes)
	if err != nil {
		t.Fatal(err)
	}
	cursor := state.Cursor
	for _, b := range h.store.sends {
		if b.ConnectorID != h.notes || b.Seq <= state.Cursor {
			continue
		}
		lines, err := events.Lines(bytes.NewReader(h.bucket.objects[b.S3Key]), events.Batch{ID: b.ID, Seq: b.Seq, Key: b.S3Key, Pieces: b.Pieces, Bytes: b.Bytes})
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			row, err := l.Row(state.Schema)
			if err != nil {
				t.Fatal(err)
			}
			if _, held := h.rows[l.ID()]; !held {
				h.order = append(h.order, l.ID())
			}
			h.rows[l.ID()] = row
		}
		cursor = b.Seq
	}
	rows := make([]string, len(h.order))
	for i, id := range h.order {
		rows[i] = string(h.rows[id])
	}
	h.table(t, fmt.Sprintf("snap-%d", cursor), rows...)
	h.store.rows[h.notes].ChangeCursor = fmt.Appendf(nil, `{"seq":%d}`, cursor)
}

// table makes rows, one a line, the snapshot of the source's events table.
func (h *piecesHarness) table(t *testing.T, name string, rows ...string) {
	t.Helper()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	for _, row := range rows {
		if _, err := w.Write([]byte(row + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	key := "ws/ws-1/connectors/" + h.notes + "/events/" + name + ".jsonl.gz"
	h.bucket.objects[key] = gz.Bytes()
	h.store.tables = slices.DeleteFunc(h.store.tables, func(table *foundrymodel.ConnectorTable) bool { return table.ConnectorID == h.notes })
	h.store.tables = append(h.store.tables, &foundrymodel.ConnectorTable{
		ConnectorID: h.notes, WorkspaceID: "ws-1", TableName: "events", S3Key: key, RowCount: int64(len(rows)), SyncID: "sync-1",
		Columns: []byte(`[{"name":"id"}]`), PrimaryKey: []byte(`["id"]`), ForeignKeys: []byte(`[]`), SyncedAt: time.Now(),
	})
}

// tableRow is a table row of piece id, received at at, whose note is text.
func tableRow(id string, at time.Time, text string) string {
	return fmt.Sprintf(`{"id":%q,"received_at":%q,"occurred_at":null,"note":%q}`, id, at.UTC().Format("2006-01-02T15:04:05.000000Z"), text)
}

func (h *piecesHarness) list(query string, limit int32) (*foundryv1.ListFoundryEventPiecesResponse, error) {
	return h.s.ListFoundryEventPieces(context.Background(), &foundryv1.ListFoundryEventPiecesRequest{
		WorkspaceId: "ws-1", ConnectorId: h.notes, Query: query, Limit: limit,
	})
}

// pieceIDs are the pieces' ids, each "+" when a sync has read it and "-"
// when not.
func pieceIDs(resp *foundryv1.ListFoundryEventPiecesResponse) []string {
	var out []string
	for _, p := range resp.GetPieces() {
		mark := "-"
		if p.GetSynced() {
			mark = "+"
		}
		out = append(out, p.GetId()+mark)
	}
	return out
}

func TestListFoundryEventPieces(t *testing.T) {
	// A piece is listed from the moment its send is stored: what no sync has
	// read comes from its batch, the rest from the source's table, and an id
	// sent again shows once, as its newest send has it.
	t.Run("lists the newest pieces, sent a moment ago or long synced, each id once", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.note(t, "n-1=The client hates long emails")
		h.note(t, "n-2=Renewal is in March", "n-3=Replaced below")
		h.sync(t)
		synced := h.clock.now()
		h.note(t, "n-3=Sent again", "n-4=First")
		h.note(t, "n-4=Second\nwith more", "n-5=Newest")
		h.bucket.opens = 0

		resp, err := h.list("", 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := pieceIDs(resp); !slices.Equal(got, []string{"n-4-", "n-5-", "n-3-", "n-2+", "n-1+"}) || resp.GetTruncated() {
			t.Fatalf("pieces %v, truncated %v", got, resp.GetTruncated())
		}
		again, newest, sent, old := resp.GetPieces()[0], resp.GetPieces()[1], resp.GetPieces()[2], resp.GetPieces()[3]
		if newest.GetTitle() != "Newest" || !strings.HasSuffix(newest.GetText(), "\nNewest") || !newest.GetReceivedAt().AsTime().Equal(h.clock.now()) {
			t.Fatalf("newest = %v", newest)
		}
		if again.GetTitle() != "Second" || !strings.Contains(again.GetText(), "with more") || strings.Contains(again.GetText(), "First") {
			t.Fatalf("a piece sent again before a sync = %v", again)
		}
		if !strings.Contains(sent.GetText(), "Sent again") || strings.Contains(sent.GetText(), "Replaced below") {
			t.Fatalf("a synced piece sent again = %v", sent)
		}
		if old.GetTitle() != "Renewal is in March" || !strings.HasPrefix(old.GetText(), "# Renewal is in March\n\n- Piece: n-2\n") || !old.GetReceivedAt().AsTime().Equal(synced) {
			t.Fatalf("a synced piece = %v", old)
		}
		// The two batches above the cursor, and the table's snapshot.
		if h.bucket.opens != 3 {
			t.Fatalf("opened %d objects", h.bucket.opens)
		}
	})

	// A query finds words, not meaning: every word, whatever its case, in
	// the piece's document.
	t.Run("lists only the pieces holding every word of the query", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.note(t, "n-1=The client hates long emails", "n-2=Renewal is in March")
		h.sync(t)
		h.note(t, "n-3=Send the client short EMAILS", "n-4=Nothing on this")
		for query, want := range map[string][]string{
			"emails":             {"n-3-", "n-1+"},
			"  Client   emails ": {"n-3-", "n-1+"},
			"LONG emails":        {"n-1+"},
			"emails march":       nil,
			"n-2":                {"n-2+"},
			"préférences":        nil,
			"mail":               {"n-3-", "n-1+"},
		} {
			resp, err := h.list(query, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := pieceIDs(resp); !slices.Equal(got, want) || resp.GetTruncated() {
				t.Errorf("%q: pieces %v, truncated %v", query, got, resp.GetTruncated())
			}
		}
	})

	// A query finds a piece by its id, its fields and its text: the lines a
	// document heads every piece with are not the piece's words.
	t.Run("does not find a piece by its document's own lines", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.note(t, "n-0=Small")
		h.store.rows[h.notes].ChangeCursor = []byte(`{"seq":1}`)
		h.table(t, "snap-1", `{"id":"n-1","received_at":"2026-10-24T12:01:00.000000Z","occurred_at":"2026-10-23T09:30:00Z","note":"Renewal call"}`,
			tableRow("n-2", h.clock.now().Add(time.Minute), "- Occurred: after the piece: a call"))
		for query, want := range map[string][]string{
			"piece": {"n-2+"}, "occurred": {"n-2+"}, "2026-10-23": nil, "#": nil, "renewal": {"n-1+"}, "n-1": {"n-1+"}, "CALL": {"n-2+", "n-1+"},
		} {
			resp, err := h.list(query, 0)
			if err != nil || !slices.Equal(pieceIDs(resp), want) || resp.GetTruncated() {
				t.Errorf("%q: pieces %v, truncated %v, %v", query, pieceIDs(resp), resp.GetTruncated(), err)
			}
		}
	})

	// With words, a list says pieces were left out only when more hold them
	// than it lists, whatever else the source holds.
	t.Run("says pieces were left out only when more were found", func(t *testing.T) {
		h := newPiecesHarness(t)
		for i := range 10 {
			h.note(t, fmt.Sprintf("old-%d=Nothing here", i))
		}
		h.note(t, "hit-1=A match")
		h.sync(t)
		h.note(t, "hit-2=A match", "new-1=Nothing here")
		h.note(t, "hit-3=A match")
		resp, err := h.list("match", 3)
		if err != nil || !slices.Equal(pieceIDs(resp), []string{"hit-3-", "hit-2-", "hit-1+"}) || resp.GetTruncated() {
			t.Fatalf("every match: %v, truncated %v, %v", pieceIDs(resp), resp.GetTruncated(), err)
		}
		if resp, err = h.list("match", 2); err != nil || !slices.Equal(pieceIDs(resp), []string{"hit-3-", "hit-2-"}) || !resp.GetTruncated() {
			t.Fatalf("one match more than the list holds: %v, truncated %v, %v", pieceIDs(resp), resp.GetTruncated(), err)
		}
		if resp, err = h.list("", 3); err != nil || len(resp.GetPieces()) != 3 || !resp.GetTruncated() {
			t.Fatalf("the newest of 14: %v, truncated %v, %v", pieceIDs(resp), resp.GetTruncated(), err)
		}
	})

	t.Run("lists 20 by default and at most 50, saying when pieces were left out", func(t *testing.T) {
		h := newPiecesHarness(t)
		for i := range 30 {
			h.note(t, fmt.Sprintf("a-%02d=Early", i), fmt.Sprintf("b-%02d=Early", i))
		}
		h.sync(t)
		for i := range 15 {
			h.note(t, fmt.Sprintf("c-%02d=Late", i), fmt.Sprintf("d-%02d=Late", i))
		}
		for limit, want := range map[int32]int{0: 20, -1: 20, 7: 7, 500: 50} {
			resp, err := h.list("", limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.GetPieces()) != want || !resp.GetTruncated() || resp.GetPieces()[0].GetId() != "c-14" {
				t.Fatalf("limit %d: %d pieces from %s, truncated %v", limit, len(resp.GetPieces()), resp.GetPieces()[0].GetId(), resp.GetTruncated())
			}
			for i, p := range resp.GetPieces()[1:] {
				if p.GetReceivedAt().AsTime().After(resp.GetPieces()[i].GetReceivedAt().AsTime()) {
					t.Fatalf("limit %d: piece %d is newer than the one before it", limit, i+1)
				}
			}
		}
		// All 90 fit no list, but a query's few do.
		resp, err := h.list("late", 50)
		if err != nil || len(resp.GetPieces()) != 30 || resp.GetTruncated() {
			t.Fatalf("the 30 late pieces: %d, truncated %v, %v", len(resp.GetPieces()), resp.GetTruncated(), err)
		}
		// More early pieces than a list holds.
		if resp, err = h.list("early", 0); err != nil || len(resp.GetPieces()) != 20 || !resp.GetTruncated() {
			t.Fatalf("the early pieces: %d, truncated %v, %v", len(resp.GetPieces()), resp.GetTruncated(), err)
		}
	})

	// A list holds 1 MiB of text, and always its newest piece.
	t.Run("holds at most 1 MiB of text, and always the newest piece", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.note(t, "n-0=Small")
		at := h.clock.now()
		large := strings.Repeat("x", 600<<10)
		h.table(t, "snap-1", tableRow("n-1", at.Add(3*time.Minute), large), tableRow("n-2", at.Add(2*time.Minute), large), tableRow("n-3", at.Add(time.Minute), "small"))
		h.store.rows[h.notes].ChangeCursor = []byte(`{"seq":1}`)
		resp, err := h.list("", 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := pieceIDs(resp); !slices.Equal(got, []string{"n-1+"}) || !resp.GetTruncated() {
			t.Fatalf("pieces %v, truncated %v", got, resp.GetTruncated())
		}
		h.table(t, "snap-2", tableRow("n-1", at.Add(3*time.Minute), strings.Repeat("y", 2<<20)), tableRow("n-3", at.Add(time.Minute), "small"))
		if resp, err = h.list("", 0); err != nil || !slices.Equal(pieceIDs(resp), []string{"n-1+"}) || len(resp.GetPieces()[0].GetText()) < 2<<20 || !resp.GetTruncated() {
			t.Fatalf("a piece past the cap alone: %v, %v", pieceIDs(resp), err)
		}
	})

	// Nothing indexes a source's pieces, so a list reads them, within
	// bounds, and says when the source holds more.
	t.Run("reads the 100 newest waiting batches", func(t *testing.T) {
		h := newPiecesHarness(t)
		for i := range eventPieceBatches + 1 {
			h.note(t, fmt.Sprintf("n-%03d=Note", i))
		}
		resp, err := h.list("", 50)
		if err != nil || len(resp.GetPieces()) != 50 || resp.GetPieces()[0].GetId() != "n-100" || !resp.GetTruncated() {
			t.Fatalf("101 waiting batches: %v, %v", pieceIDs(resp), err)
		}
		// The oldest batch is not read, so nor is its piece for the word.
		if resp, err = h.list("n-000", 0); err != nil || len(resp.GetPieces()) != 0 || !resp.GetTruncated() {
			t.Fatalf("a piece of the batch not read: %v, truncated %v, %v", pieceIDs(resp), resp.GetTruncated(), err)
		}
	})

	// A snapshot holds its rows in no order of arrival, so a part of a table
	// would be no answer: a list reads a table whole, or refuses it.
	t.Run("refuses a table past 64 MiB", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.note(t, "n-0=Small")
		h.store.rows[h.notes].ChangeCursor = []byte(`{"seq":1}`)
		at := h.clock.now()
		first, last := tableRow("first", at, "needle"), tableRow("last", at, "needle")
		pad := eventPieceTableBytes - len(first) - len(last) - len(tableRow("pad", at, "")) - 3
		h.table(t, "snap-1", first, tableRow("pad", at, strings.Repeat("x", pad)), last)
		resp, err := h.list("needle", 0)
		if err != nil || !slices.Equal(pieceIDs(resp), []string{"first+", "last+"}) || resp.GetTruncated() {
			t.Fatalf("a table of 64 MiB: %v, truncated %v, %v", pieceIDs(resp), resp.GetTruncated(), err)
		}
		// One byte more, and nothing of it is read.
		h.table(t, "snap-2", first, tableRow("pad", at, strings.Repeat("x", pad+1)), last)
		h.bucket.opens = 0
		if _, err = h.list("needle", 0); status.Code(err) != codes.FailedPrecondition || foundryReason(err) != "FOUNDRY_EVENT_SOURCE_TOO_LARGE" || h.bucket.opens != 0 {
			t.Fatalf("a table past 64 MiB = %v, after %d reads", err, h.bucket.opens)
		}
		// A gzip says its size modulo 4 GiB, so a snapshot is counted as it
		// is read too: here one larger than the size read of its key.
		large := h.bucket.objects[h.store.tables[len(h.store.tables)-1].S3Key]
		h.table(t, "snap-3", first)
		if _, err = h.list("", 0); err != nil {
			t.Fatal(err)
		}
		h.bucket.objects[h.store.tables[len(h.store.tables)-1].S3Key] = large
		if _, err = h.list("", 0); foundryReason(err) != "FOUNDRY_EVENT_SOURCE_TOO_LARGE" {
			t.Fatalf("a snapshot larger than its size said = %v", err)
		}
		h.bucket.tailErr = errors.New("s3 down")
		h.table(t, "snap-4", first)
		if _, err = h.list("", 0); status.Code(err) != codes.Unavailable {
			t.Fatalf("a table whose size can't be read = %v", err)
		}
	})

	// A piece's field may be named as one of its row's columns is, but for
	// its case: the row's own id and arrival are still the piece's.
	t.Run("reads a row's id and arrival by their own names", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.clock.advance(time.Minute)
		if _, err := h.send(h.notes, "", `n-1={"note":"Hello","Id":7,"ID":"other","Received_At":"never"}`); err != nil {
			t.Fatal(err)
		}
		h.sync(t)
		resp, err := h.list("", 0)
		if err != nil || !slices.Equal(pieceIDs(resp), []string{"n-1+"}) || !resp.GetPieces()[0].GetReceivedAt().AsTime().Equal(h.clock.now()) {
			t.Fatalf("pieces %v, %v", pieceIDs(resp), err)
		}
	})

	// A table without a snapshot holds nothing whatever the cursor says, so
	// every batch still waits, as a sync reads it.
	t.Run("reads every batch of a source with a cursor and no table", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.note(t, "n-1=First")
		h.note(t, "n-2=Second")
		h.store.rows[h.notes].ChangeCursor = []byte(`{"seq":2}`)
		resp, err := h.list("", 0)
		if err != nil || !slices.Equal(pieceIDs(resp), []string{"n-2-", "n-1-"}) || resp.GetTruncated() {
			t.Fatalf("pieces %v, truncated %v, %v", pieceIDs(resp), resp.GetTruncated(), err)
		}
	})

	t.Run("refuses a query past 200 bytes or 10 words", func(t *testing.T) {
		h := newPiecesHarness(t)
		for _, query := range []string{strings.Repeat("x", 201), "a b c d e f g h i j k"} {
			if _, err := h.list(query, 0); status.Code(err) != codes.InvalidArgument {
				t.Errorf("%q = %v", query, err)
			}
		}
		if _, err := h.list(strings.Repeat("x", 200), 0); err != nil {
			t.Fatal(err)
		}
		if _, err := h.list("a b c d e f g h i j", 0); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refuses a source without a text field", func(t *testing.T) {
		h := newPiecesHarness(t)
		_, err := h.s.ListFoundryEventPieces(context.Background(), &foundryv1.ListFoundryEventPiecesRequest{WorkspaceId: "ws-1", ConnectorId: h.source})
		if status.Code(err) != codes.FailedPrecondition || foundryReason(err) != "FOUNDRY_EVENT_SOURCE_NO_TEXT_FIELD" {
			t.Fatalf("list = %v", err)
		}
	})

	// The registry checked only the workspace, so the source must be found in it.
	t.Run("finds only the workspace's source", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.store.rows["conn-stripe"] = &foundrymodel.Connector{ID: "conn-stripe", WorkspaceID: "ws-1", Kind: "stripe", Config: []byte(`{}`)}
		for name, tc := range map[string]struct {
			req    *foundryv1.ListFoundryEventPiecesRequest
			code   codes.Code
			reason string
		}{
			"another workspace":     {&foundryv1.ListFoundryEventPiecesRequest{WorkspaceId: "ws-2", ConnectorId: h.notes}, codes.NotFound, ""},
			"no workspace":          {&foundryv1.ListFoundryEventPiecesRequest{ConnectorId: h.notes}, codes.InvalidArgument, ""},
			"no connector":          {&foundryv1.ListFoundryEventPiecesRequest{WorkspaceId: "ws-1"}, codes.InvalidArgument, ""},
			"no such connector":     {&foundryv1.ListFoundryEventPiecesRequest{WorkspaceId: "ws-1", ConnectorId: "conn-9"}, codes.NotFound, ""},
			"a connector of stripe": {&foundryv1.ListFoundryEventPiecesRequest{WorkspaceId: "ws-1", ConnectorId: "conn-stripe"}, codes.FailedPrecondition, "FOUNDRY_NOT_EVENT_SOURCE"},
		} {
			_, err := h.s.ListFoundryEventPieces(context.Background(), tc.req)
			if status.Code(err) != tc.code || foundryReason(err) != tc.reason {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	// No backend stores a text field that is not a name, or one its schema
	// does not hold as a string: the lock comes with the field.
	t.Run("fails on settings no backend writes", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.note(t, "n-1=Hello")
		h.store.rows[h.notes].Config = []byte(`{"text_field":"body","schema":{"note":"string"}}`)
		if _, err := h.list("", 0); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), `piece "n-1" of batch 1`) {
			t.Fatalf("a text field the schema lacks, of a waiting piece = %v", err)
		}
		h.sync(t)
		if _, err := h.list("", 0); status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "the API source's table") {
			t.Fatalf("a text field the schema lacks, of a synced piece = %v", err)
		}
		h.store.rows[h.notes].Config = []byte(`{"text_field":7,"schema":{"note":"string"}}`)
		if _, err := h.list("", 0); status.Code(err) != codes.Internal {
			t.Fatalf("a text field that is no name = %v", err)
		}
	})

	// A batch whose object is gone or is not what its send wrote can't be
	// read again; anything else may pass on a retry.
	t.Run("fails when a waiting batch cannot be read", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.note(t, "n-1=Hello")
		key := h.store.sends[len(h.store.sends)-1].S3Key
		object := h.bucket.objects[key]
		h.bucket.objects[key] = object[:len(object)/2]
		if _, err := h.list("", 0); status.Code(err) != codes.Unavailable {
			t.Fatalf("a cut object = %v", err)
		}
		h.bucket.objects[key] = []byte("this is not a gzip stream")
		if _, err := h.list("", 0); status.Code(err) != codes.Internal {
			t.Fatalf("an object no send wrote = %v", err)
		}
		delete(h.bucket.objects, key)
		if _, err := h.list("", 0); status.Code(err) != codes.Internal {
			t.Fatalf("a missing object = %v", err)
		}
	})

	// A sync replaces the table's snapshot, and the old one goes after its
	// grace: the next list reads the new one.
	t.Run("fails when the table cannot be read", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.note(t, "n-1=Hello")
		h.sync(t)
		key := h.store.tables[len(h.store.tables)-1].S3Key
		snapshot := h.bucket.objects[key]
		// The snapshot's size is read once, here: a key is never written again.
		if _, err := h.list("", 0); err != nil {
			t.Fatal(err)
		}
		for name, tc := range map[string]struct {
			object []byte
			code   codes.Code
		}{
			"a cut snapshot":        {snapshot[:len(snapshot)-9], codes.Unavailable},
			"a snapshot not gzip":   {[]byte("this is not a gzip stream"), codes.Unavailable},
			"a missing snapshot":    {nil, codes.Unavailable},
			"a row that is no row":  {gzipped(t, "not a row\n"), codes.Internal},
			"a row with no id":      {gzipped(t, `{"received_at":"2026-10-24T12:01:00.000000Z","note":"Hello"}`+"\n"), codes.Internal},
			"a row with no arrival": {gzipped(t, `{"id":"n-1","note":"Hello"}`+"\n"), codes.Internal},
			"a row of a field the schema lacks": {gzipped(t, `{"id":"n-1","received_at":"2026-10-24T12:01:00.000000Z","occurred_at":null,"other":1}`+"\n"),
				codes.Internal},
		} {
			if h.bucket.objects[key] = tc.object; tc.object == nil {
				delete(h.bucket.objects, key)
			}
			if _, err := h.list("", 0); status.Code(err) != tc.code {
				t.Errorf("%s = %v", name, err)
			}
		}
	})

	// A caller that left is not a bucket that failed.
	t.Run("answers a read its caller ended as the caller's", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		if err := readError(ctx, "read", errors.New("s3 down")); status.Code(err) != codes.Unavailable {
			t.Fatalf("a failed read = %v", err)
		}
		cancel()
		if err := readError(ctx, "read", context.Canceled); status.Code(err) != codes.Canceled {
			t.Fatalf("a read its caller cancelled = %v", err)
		}
		ctx, cancel = context.WithTimeout(context.Background(), 0)
		defer cancel()
		<-ctx.Done()
		if err := readError(ctx, "read", context.DeadlineExceeded); status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("a read past its deadline = %v", err)
		}
	})

	t.Run("needs the data bucket", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.s.deps.EventSends = nil
		if _, err := h.list("", 0); status.Code(err) != codes.Unavailable {
			t.Fatalf("no data bucket = %v", err)
		}
	})

	// A list may read a whole table, so the backend reads 4 at once, and one
	// of a workspace at a time: one past either waits its turn, for as long
	// as its caller does.
	t.Run("waits its turn among its workspace's lists and the backend's", func(t *testing.T) {
		h := newPiecesHarness(t)
		h.note(t, "n-1=Hello")
		e := h.s.deps.EventSends
		src, workspace := e.enter("ws-1", h.notes)
		defer e.leave(src, workspace)
		waits := func(turns chan struct{}, held int) {
			t.Helper()
			for range held {
				turns <- struct{}{}
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := h.s.ListFoundryEventPieces(ctx, &foundryv1.ListFoundryEventPiecesRequest{WorkspaceId: "ws-1", ConnectorId: h.notes})
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("a list past the turns did not wait: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			cancel()
			if err := <-done; status.Code(err) != codes.Canceled {
				t.Fatalf("a list whose caller left = %v", err)
			}
			for range held {
				<-turns
			}
		}
		waits(workspace.reading, 1)
		waits(e.reading, eventPieceLists)
		// The source is read once the list has its turn: a piece sent while it
		// waited is listed.
		workspace.reading <- struct{}{}
		listed := make(chan []string, 1)
		go func() {
			resp, _ := h.list("", 0)
			listed <- pieceIDs(resp)
		}()
		time.Sleep(50 * time.Millisecond)
		h.note(t, "n-2=Sent meanwhile")
		<-workspace.reading
		if got := <-listed; !slices.Equal(got, []string{"n-2-", "n-1-"}) {
			t.Fatalf("a list that waited: %v", got)
		}
		if resp, err := h.list("", 0); err != nil || len(resp.GetPieces()) != 2 || len(e.reading) != 0 || len(workspace.reading) != 0 {
			t.Fatalf("a list with a turn: %v, %v, %d and %d reading", pieceIDs(resp), err, len(e.reading), len(workspace.reading))
		}
		// And a source deleted while its list waited is not found.
		workspace.reading <- struct{}{}
		gone := make(chan error, 1)
		go func() {
			_, err := h.list("", 0)
			gone <- err
		}()
		time.Sleep(50 * time.Millisecond)
		h.store.mu.Lock()
		delete(h.store.rows, h.notes)
		h.store.mu.Unlock()
		<-workspace.reading
		if err := <-gone; status.Code(err) != codes.NotFound {
			t.Fatalf("a list of a source deleted while it waited = %v", err)
		}
	})
}

// gzipped is body as one gzip member.
func gzipped(t *testing.T, body string) []byte {
	t.Helper()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}
