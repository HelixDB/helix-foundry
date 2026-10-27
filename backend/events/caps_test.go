package events

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHoldingProject(t *testing.T) {
	roomy := int64(1 << 40)

	// A source may fill its table exactly; one piece more must be refused
	// at send, because past the cap a sync fails for good.
	t.Run("accepts a send that fills the rows and refuses one more piece", func(t *testing.T) {
		h := Holding{TableRows: 5, PendingPieces: 3}
		caps := Caps{MaxRows: 10, MaxBytes: roomy}
		c, err := h.Project(validSend(t, Piece{ID: "a", Data: []byte(`{}`)}, Piece{ID: "b", Data: []byte(`{}`)}), Schema{}, caps)
		if err != nil || c.Rows != 10 {
			t.Fatalf("rows %d, err %v", c.Rows, err)
		}
		_, err = h.Project(validSend(t, Piece{ID: "a", Data: []byte(`{}`)}, Piece{ID: "b", Data: []byte(`{}`)}, Piece{ID: "c", Data: []byte(`{}`)}), Schema{}, caps)
		var r *Refusal
		if !errors.As(err, &r) || r.Kind != Full || r.Piece != 0 || r.Message != "This source holds 8 pieces, and one source holds at most 10: this send's 3 would pass that. Send to a new source." {
			t.Fatalf("got %v", err)
		}
	})

	// The byte projection is the table's bytes, the padding of fields its
	// rows lack, and each waiting piece's line with its padding and slack.
	// At the cap a send passes; a byte over, it is refused.
	t.Run("accepts a send that fills the bytes and refuses one byte more", func(t *testing.T) {
		h := Holding{TableRows: 2, TableBytes: 1000, TableFields: []string{"id", "received_at", "occurred_at", "a"}, PendingPieces: 1, PendingBytes: 100}
		send := validSend(t, Piece{ID: "o-1", Data: []byte(`{"b":1}`)})
		merged, err := schemaOf(map[string]Type{"a": TypeNumber}).Merge(send, 200)
		if err != nil {
			t.Fatal(err)
		}
		// ,"a":null and ,"b":null are 9 bytes each; the table's 2 rows lack b.
		want := 1000 + 2*9 + 100 + send.Bytes() + 2*(18+rowSlack)
		c, err := h.Project(send, merged.Schema, Caps{MaxRows: 10, MaxBytes: want})
		if err != nil || c.Bytes != want {
			t.Fatalf("bytes %d, want %d, err %v", c.Bytes, want, err)
		}
		_, err = h.Project(send, merged.Schema, Caps{MaxRows: 10, MaxBytes: want - 1})
		var r *Refusal
		wantMsg := fmt.Sprintf("This source's table would hold %s bytes with this send, and one source holds at most %s. Send to a new source.", grouped(want), grouped(want-1))
		if !errors.As(err, &r) || r.Kind != Full || r.Piece != 0 || r.Message != wantMsg {
			t.Fatalf("got %v", err)
		}
	})

	// A field learned by a send no sync has merged yet is padded into every
	// stored row at the next merge, whichever send added it; a later send
	// adding nothing must still count that padding.
	t.Run("counts padding for fields learned since the table's last merge", func(t *testing.T) {
		after := schemaOf(map[string]Type{"a": TypeNumber, "late": TypeString})
		h := Holding{TableRows: 1000, TableBytes: 50_000, TableFields: []string{"id", "received_at", "occurred_at", "a"}}
		got := h.Capacity(after, Caps{MaxRows: 10_000, MaxBytes: roomy}).Bytes
		if want := int64(50_000 + 1000*len(`,"late":null`)); got != want {
			t.Fatalf("bytes %d, want %d", got, want)
		}
	})

	// ISIZE reads a 4 GiB snapshot, the largest config allows, as 0: a
	// table with rows is never empty, so it counts as full, never as room.
	t.Run("counts a table with rows that reads as empty as 4 GiB", func(t *testing.T) {
		got := Holding{TableRows: 5, TableBytes: 0, TableFields: []string{"id", "received_at", "occurred_at"}}.Capacity(Schema{}, Caps{MaxRows: 100, MaxBytes: 4 << 30})
		if got.Bytes != 4<<30 || got.Percent() != 100 {
			t.Fatalf("got %+v", got)
		}
		if empty := (Holding{}).Capacity(Schema{}, Caps{MaxRows: 100, MaxBytes: 4 << 30}); empty.Bytes != 0 {
			t.Fatalf("an empty source holds %d bytes", empty.Bytes)
		}
	})

	// What a send answers and what a list shows next must agree: the
	// capacity after a send is the capacity of the source with its batch
	// waiting.
	t.Run("agrees with the capacity of the source once the send waits", func(t *testing.T) {
		h := Holding{TableRows: 3, TableBytes: 300, PendingPieces: 2, PendingBytes: 200}
		send := validSend(t, Piece{ID: "o-1", Data: []byte(`{"x":true}`)})
		merged, err := Schema{}.Merge(send, 200)
		if err != nil {
			t.Fatal(err)
		}
		caps := Caps{MaxRows: 100, MaxBytes: roomy}
		projected, err := h.Project(send, merged.Schema, caps)
		if err != nil {
			t.Fatal(err)
		}
		listed := Holding{TableRows: 3, TableBytes: 300, PendingPieces: 3, PendingBytes: 200 + send.Bytes()}.Capacity(merged.Schema, caps)
		if projected != listed {
			t.Fatalf("send says %+v, list says %+v", projected, listed)
		}
	})

	// The promise of the byte cap: what a merge then writes never passes
	// the projection, whatever the pieces: with or without a time, with any
	// subset of the fields, resent ids, and fields that arrive after the
	// table's rows were written (the merge pads those rows).
	t.Run("bounds the bytes a merge writes", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(1, 2))
		names := []string{"a", "bb", `q"`, "é", "long_field_name", "z"}
		randomSend := func(fields []string) Send {
			var pieces []Piece
			for i := range 1 + rng.IntN(20) {
				data := []byte("{")
				for _, name := range fields {
					if rng.IntN(2) == 0 {
						continue
					}
					if len(data) > 1 {
						data = append(data, ',')
					}
					data = append(append(data, jsonString(name)...), fmt.Sprintf(":%d", rng.IntN(1000))...)
				}
				data = append(data, '}')
				p := Piece{ID: fmt.Sprintf("p%d-%d", i, rng.IntN(5)), Data: data}
				if rng.IntN(2) == 0 {
					at := time.Date(2026, 1, 1, 0, 0, rng.IntN(3600), rng.IntN(1e9), time.UTC)
					p.OccurredAt = &at
				}
				pieces = append(pieces, p)
			}
			// Ids repeat across sends, not within one.
			slices.SortFunc(pieces, func(a, b Piece) int { return strings.Compare(a.ID, b.ID) })
			pieces = slices.CompactFunc(pieces, func(a, b Piece) bool { return a.ID == b.ID })
			return validSend(t, pieces...)
		}
		received := time.Date(2026, 10, 23, 18, 25, 1, 0, time.UTC)
		// merge writes the rows of sends, under schema s, over a table whose
		// rows each gain padding bytes, as a dense merge pads them to s.
		merge := func(table map[string][]byte, padding int64, s Schema, sends ...Send) map[string][]byte {
			out := map[string][]byte{}
			for id, row := range table {
				out[id] = append(slices.Clone(row), make([]byte, padding)...)
			}
			for _, send := range sends {
				obj, err := EncodeLines(send, received)
				if err != nil {
					t.Fatal(err)
				}
				for _, raw := range strings.Split(strings.TrimSuffix(gunzip(t, obj.Body), "\n"), "\n") {
					l, err := DecodeLine([]byte(raw))
					if err != nil {
						t.Fatal(err)
					}
					row, err := l.Row(s)
					if err != nil {
						t.Fatal(err)
					}
					out[l.ID()] = row
				}
			}
			return out
		}
		size := func(table map[string][]byte) (n int64) {
			for _, row := range table {
				n += int64(len(row) + 1)
			}
			return n
		}
		for round := range 200 {
			// A table merged from the first sends, under the fields seen so far.
			fields := names[:1+rng.IntN(len(names)-1)]
			var s Schema
			var firstSends []Send
			for range 1 + rng.IntN(3) {
				send := randomSend(fields)
				merged, err := s.Merge(send, 200)
				if err != nil {
					t.Fatal(err)
				}
				s, firstSends = merged.Schema, append(firstSends, send)
			}
			table, early := merge(nil, 0, s, firstSends...), s
			h := Holding{TableRows: int64(len(table)), TableBytes: size(table)}
			for _, f := range s.Columns() {
				h.TableFields = append(h.TableFields, f.Name)
			}
			// Then sends that wait, with any field, some new.
			var waiting []Send
			for range 1 + rng.IntN(3) {
				send := randomSend(names)
				merged, err := s.Merge(send, 200)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.Project(send, merged.Schema, Caps{MaxRows: 1 << 40, MaxBytes: 1 << 40}); err != nil {
					t.Fatal(err)
				}
				h.PendingPieces += int64(send.Pieces())
				h.PendingBytes += send.Bytes()
				s, waiting = merged.Schema, append(waiting, send)
			}
			projected := h.Capacity(s, Caps{MaxRows: 1 << 40, MaxBytes: 1 << 40})
			var padding int64
			for _, f := range s.fields {
				if _, ok := early.types[f.Name]; !ok {
					padding += nullFill(f)
				}
			}
			after := merge(table, padding, s, waiting...)
			if got := size(after); got > projected.Bytes {
				t.Fatalf("round %d: a merge wrote %d bytes, the projection allowed %d", round, got, projected.Bytes)
			}
			if got := int64(len(after)); got > projected.Rows {
				t.Fatalf("round %d: a merge wrote %d rows, the projection allowed %d", round, got, projected.Rows)
			}
		}
	})
}

func TestCapacityWarning(t *testing.T) {
	caps := Caps{MaxRows: 2_000_000, MaxBytes: 2 << 30}

	// From 80% full every answer warns, so people move to a new source
	// before sends are refused; below it, nothing is said.
	t.Run("warns from 80% full", func(t *testing.T) {
		if w := (Capacity{Rows: 1_599_999, Caps: caps}).Warning(); w != "" {
			t.Fatalf("warned at 79%%: %s", w)
		}
		if w := (Capacity{Rows: 1_600_000, Caps: caps}).Warning(); w == "" {
			t.Fatal("no warning at 80%")
		}
	})

	// The warning names the fuller count, with the numbers people need.
	t.Run("names the fuller count", func(t *testing.T) {
		rows := Capacity{Rows: 1_660_000, Bytes: 1 << 30, Caps: caps}
		if w, want := rows.Warning(), "This source is 83% full (1,660,000 of 2,000,000 pieces). Once it is full, sends are refused: send new pieces to a new source before then."; w != want {
			t.Fatalf("got  %s\nwant %s", w, want)
		}
		bytes := Capacity{Rows: 10, Bytes: 1_932_735_284, Caps: caps}
		if w, want := bytes.Warning(), "This source is 90% full (1,932,735,284 of 2,147,483,648 bytes). Once it is full, sends are refused: send new pieces to a new source before then."; w != want {
			t.Fatalf("got  %s\nwant %s", w, want)
		}
		tie := Capacity{Rows: 1_700_000, Bytes: 1_825_361_101, Caps: caps}
		if w, want := tie.Warning(), "This source is 85% full (1,700,000 of 2,000,000 pieces). Once it is full, sends are refused: send new pieces to a new source before then."; w != want {
			t.Fatalf("got  %s\nwant %s", w, want)
		}
	})

	// A full source reads 100%, and one an operator's lowered cap left over
	// it reads past 100: Percent never hides it.
	t.Run("reads past 100% over a cap", func(t *testing.T) {
		if p := (Capacity{Rows: 2_000_000, Caps: caps}).Percent(); p != 100 {
			t.Fatalf("got %d", p)
		}
		if p := (Capacity{Rows: 3_000_000, Caps: caps}).Percent(); p != 150 {
			t.Fatalf("got %d", p)
		}
	})
}
