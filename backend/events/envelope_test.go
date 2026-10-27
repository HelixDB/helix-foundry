package events

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// pieceFixture is testdata/pieces.json: the envelope's rule table, which the
// site's mock runs too. Its "about" says how to read a case.
type pieceFixture struct {
	Limits struct {
		MaxPiecesPerSend int `json:"max_pieces_per_send"`
		MaxSendBytes     int `json:"max_send_bytes"`
		MaxPieceBytes    int `json:"max_piece_bytes"`
		MaxFields        int `json:"max_fields"`
	} `json:"limits"`
	Cases []struct {
		Name         string            `json:"name"`
		Schema       map[string]string `json:"schema"`
		SchemaFields int               `json:"schema_fields"`
		Pieces       []struct {
			ID         string  `json:"id"`
			Repeat     int     `json:"repeat"`
			OccurredAt *string `json:"occurred_at"`
			DataJSON   *string `json:"data_json"`
			DataBase64 *string `json:"data_base64"`
			DataSize   int     `json:"data_size"`
		} `json:"pieces"`
		Want struct {
			Refused      string            `json:"refused"`
			Piece        *int              `json:"piece"`
			Message      string            `json:"message"`
			Bytes        int64             `json:"bytes"`
			NewFields    []fixtureField    `json:"new_fields"`
			Schema       map[string]string `json:"schema"`
			SchemaFields int               `json:"schema_fields"`
		} `json:"want"`
	} `json:"cases"`
}

type fixtureField struct {
	Name string `json:"name"`
	Type Type   `json:"type"`
}

func TestPieceEnvelope(t *testing.T) {
	raw, err := os.ReadFile("testdata/pieces.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx pieceFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	limits := Limits{MaxPieces: fx.Limits.MaxPiecesPerSend, MaxBytes: fx.Limits.MaxSendBytes, MaxPieceBytes: fx.Limits.MaxPieceBytes}
	kinds := map[string]RefusalKind{"invalid": Invalid, "fields": TooManyFields}
	// Every rule a send meets before it is stored, at its boundaries, from
	// the table the site's mock shares: the backend and the mock must agree
	// on what is stored, what is refused and what the refusal says.
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var schema Schema
			if c.SchemaFields > 0 {
				schema = numberFields(c.SchemaFields)
			} else if c.Schema != nil {
				b, _ := json.Marshal(c.Schema)
				if err := json.Unmarshal(b, &schema); err != nil {
					t.Fatal(err)
				}
			}
			var pieces []Piece
			var err error
			for _, p := range c.Pieces {
				data := []byte(nil)
				switch {
				case p.DataJSON != nil:
					data = []byte(*p.DataJSON)
				case p.DataBase64 != nil:
					if data, err = base64.StdEncoding.DecodeString(*p.DataBase64); err != nil {
						t.Fatal(err)
					}
				default:
					data = []byte(`{"s":"` + strings.Repeat("a", p.DataSize-8) + `"}`)
				}
				ids := []string{p.ID}
				if p.Repeat > 0 {
					ids = ids[:0]
					for i := range p.Repeat {
						ids = append(ids, fmt.Sprint(p.ID, i+1))
					}
				}
				for _, id := range ids {
					piece := Piece{ID: id, Data: data}
					if p.OccurredAt != nil && err == nil {
						var at time.Time
						at, err = ParseOccurredAt(len(pieces), id, *p.OccurredAt)
						piece.OccurredAt = &at
					}
					pieces = append(pieces, piece)
				}
			}
			var send Send
			if err == nil {
				send, err = Validate(pieces, limits)
			}
			var merged Merged
			if err == nil {
				merged, err = schema.Merge(send, fx.Limits.MaxFields)
			}
			if c.Want.Refused != "" {
				var r *Refusal
				if !errors.As(err, &r) {
					t.Fatalf("got %v, want refused %s: %s", err, c.Want.Refused, c.Want.Message)
				}
				if r.Kind != kinds[c.Want.Refused] || r.Message != c.Want.Message {
					t.Fatalf("refused %d: %s\nwant %s: %s", r.Kind, r.Message, c.Want.Refused, c.Want.Message)
				}
				// A front end answers the refused piece's place on its own,
				// so every refusal states it, 0 included.
				if c.Want.Piece == nil || r.Piece != *c.Want.Piece {
					t.Fatalf("refused piece %d, want %v", r.Piece, c.Want.Piece)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// A front end forwards each piece's data as Validate compacted it.
			for i, p := range pieces {
				var want bytes.Buffer
				if err := json.Compact(&want, p.Data); err != nil {
					t.Fatal(err)
				}
				if got := send.Data(i); !bytes.Equal(got, want.Bytes()) {
					t.Fatalf("piece %d data %s, want %s", i+1, got, want.Bytes())
				}
			}
			if send.Bytes() != c.Want.Bytes {
				t.Errorf("bytes %d, want %d", send.Bytes(), c.Want.Bytes)
			}
			want := make([]Field, len(c.Want.NewFields))
			for i, f := range c.Want.NewFields {
				want[i] = Field(f)
			}
			if got, want := fmt.Sprint(merged.New), fmt.Sprint(want); got != want {
				t.Errorf("new fields %s, want %s", got, want)
			}
			if c.Want.SchemaFields > 0 {
				if merged.Schema.Len() != c.Want.SchemaFields {
					t.Errorf("schema of %d fields, want %d", merged.Schema.Len(), c.Want.SchemaFields)
				}
				return
			}
			got, _ := json.Marshal(merged.Schema)
			wantSchema, _ := json.Marshal(c.Want.Schema)
			if c.Want.Schema == nil {
				wantSchema = []byte("{}")
			}
			if string(got) != string(wantSchema) {
				t.Errorf("schema %s, want %s", got, wantSchema)
			}
		})
	}
}

// numberFields is a schema of n number fields named f1 to fn.
func numberFields(n int) Schema {
	types := map[string]Type{}
	for i := range n {
		types[fmt.Sprint("f", i+1)] = TypeNumber
	}
	return newSchema(types)
}

// validSend is the send of these pieces, which must pass every rule.
func validSend(t *testing.T, pieces ...Piece) Send {
	t.Helper()
	send, err := Validate(pieces, Limits{MaxPieces: 500, MaxBytes: 1 << 20, MaxPieceBytes: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	return send
}

// lines is a send's batch object, stamped receivedAt, uncompressed.
func lines(t *testing.T, send Send, receivedAt time.Time) string {
	t.Helper()
	obj, err := EncodeLines(send, receivedAt)
	if err != nil {
		t.Fatal(err)
	}
	return gunzip(t, obj.Body)
}

func TestValidate(t *testing.T) {
	limits := Limits{MaxPieces: 500, MaxBytes: 1 << 20, MaxPieceBytes: 256 << 10}

	// Compaction removes only space: number literals and string escapes
	// reach the table as the sender wrote them, so no value changes on the way.
	t.Run("keeps data byte for byte but its spaces", func(t *testing.T) {
		send := validSend(t, Piece{ID: "o-1", Data: []byte("{ \"n\" : 1.50 ,\n \"s\" : \"caf\\u00e9 \\/\" , \"big\" : 18446744073709551617 }")})
		got := lines(t, send, time.Date(2026, 10, 23, 18, 25, 1, 0, time.UTC))
		want := `{"id":"o-1","received_at":"2026-10-23T18:25:01.000000Z","data":{"n":1.50,"s":"caf\u00e9 \/","big":18446744073709551617}}` + "\n"
		if got != want {
			t.Fatalf("got  %s\nwant %s", got, want)
		}
	})

	// A front end hands over a time in any zone, to the nanosecond; the batch
	// row's DATETIME(6) holds UTC to the microsecond, and the line must say
	// the same.
	t.Run("stores a time in UTC to the microsecond", func(t *testing.T) {
		at := time.Date(2026, 10, 23, 20, 20, 0, 123456789, time.FixedZone("+02", 2*60*60))
		send := validSend(t, Piece{ID: "o-1", OccurredAt: &at, Data: []byte(`{}`)})
		got := lines(t, send, time.Date(2026, 10, 23, 18, 25, 1, 0, time.UTC))
		if !strings.Contains(got, `"occurred_at":"2026-10-23T18:20:00.123456Z"`) {
			t.Fatalf("got %s", got)
		}
	})

	// A protobuf Timestamp reaches years RFC 3339 text cannot write, so the
	// backend's own check, not the front end's parse, holds the DATETIME range.
	t.Run("refuses a time past the year 9999 in UTC", func(t *testing.T) {
		at := time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("-01", -60*60))
		_, err := Validate([]Piece{{ID: "o-1", OccurredAt: &at, Data: []byte(`{}`)}}, limits)
		var r *Refusal
		if !errors.As(err, &r) || r.Kind != Invalid || r.Message != `piece 1 (id "o-1"): occurred_at must be in the years 1000 to 9999 (UTC)` {
			t.Fatalf("got %v", err)
		}
	})

	// A number literal may be as long as its piece; the refusal must stay short.
	t.Run("shortens a long number literal in its refusal", func(t *testing.T) {
		_, err := Validate([]Piece{{ID: "o-1", Data: []byte(`{"n":1` + strings.Repeat("0", 400) + `}`)}}, limits)
		want := `piece 1 (id "o-1"): data holds 100000000000000000000000…, which is not a finite number`
		if err == nil || err.Error() != want {
			t.Fatalf("got %v, want %s", err, want)
		}
	})

	// The first broken rule in piece order is the one reported, so a sender
	// fixing pieces one refusal at a time converges.
	t.Run("reports the first piece that breaks a rule", func(t *testing.T) {
		_, err := Validate([]Piece{
			{ID: "o-1", Data: []byte(`{}`)},
			{ID: "o-2", Data: []byte(`[]`)},
			{ID: "bad id", Data: []byte(`{}`)},
		}, limits)
		if err == nil || err.Error() != `piece 2 (id "o-2"): data is not a JSON object` {
			t.Fatalf("got %v", err)
		}
	})

	// A front end forwards the data Validate checked, so it must be what was
	// sent with only its spaces gone: a number past int64 and float64's
	// precision, a trailing zero and a surrogate pair escape included.
	t.Run("hands over each piece's data compacted, byte for byte", func(t *testing.T) {
		send := validSend(t, Piece{ID: "o-1", Data: []byte(`{
  "big": 18446744073709551617,
  "price": 1.10,
  "face": "\ud83d\ude00 😀"
}
`)}, Piece{ID: "o-2", Data: []byte(`{}`)})
		for i, want := range []string{`{"big":18446744073709551617,"price":1.10,"face":"\ud83d\ude00 😀"}`, `{}`} {
			if got := string(send.Data(i)); got != want {
				t.Fatalf("piece %d data %s, want %s", i+1, got, want)
			}
		}
	})

	// A front end answers the refused piece's place apart from the message,
	// so every rule of one piece sets it, wherever the piece is, and a rule
	// of the whole send leaves it 0.
	year999 := time.Date(999, 12, 31, 0, 0, 0, 0, time.UTC)
	first := Piece{ID: "o-1", Data: []byte(`{}`)}
	for _, tc := range []struct {
		name   string
		limits Limits
		pieces []Piece
		want   int
	}{
		{"an id", limits, []Piece{first, {ID: "bad id", Data: []byte(`{}`)}}, 2},
		{"a repeated id, at the later piece", limits, []Piece{first, {ID: "o-2", Data: []byte(`{}`)}, first}, 3},
		{"an occurred_at", limits, []Piece{first, {ID: "o-2", OccurredAt: &year999, Data: []byte(`{}`)}}, 2},
		{"data that is not UTF-8", limits, []Piece{first, {ID: "o-2", Data: []byte("{\"s\":\"\xff\"}")}}, 2},
		{"data that is not an object", limits, []Piece{first, {ID: "o-2", Data: []byte(`{}`)}, {ID: "o-3", Data: []byte(`[]`)}}, 3},
		{"a piece's size", Limits{MaxPieces: 2, MaxBytes: 100, MaxPieceBytes: 2}, []Piece{first, {ID: "o-2", Data: []byte(`{"a":1}`)}}, 2},
		{"no pieces", limits, nil, 0},
		{"too many pieces", Limits{MaxPieces: 1, MaxBytes: 100, MaxPieceBytes: 100}, []Piece{first, {ID: "o-2", Data: []byte(`{}`)}}, 0},
		{"the send's size", Limits{MaxPieces: 2, MaxBytes: 3, MaxPieceBytes: 100}, []Piece{first, {ID: "o-2", Data: []byte(`{}`)}}, 0},
	} {
		t.Run("names the place of a refusal of "+tc.name, func(t *testing.T) {
			_, err := Validate(tc.pieces, tc.limits)
			var r *Refusal
			if !errors.As(err, &r) || r.Kind != Invalid || r.Piece != tc.want {
				t.Fatalf("got %+v, want piece %d", err, tc.want)
			}
		})
	}

	// A send's size counts its data compacted, so an indented export is not
	// refused for its whitespace.
	t.Run("counts a send's data compacted", func(t *testing.T) {
		data := []byte("{\"s\":\"" + strings.Repeat("a", 100) + "\"}" + strings.Repeat(" ", 200))
		if _, err := Validate([]Piece{{ID: "o-1", Data: data}}, Limits{MaxPieces: 1, MaxBytes: 108, MaxPieceBytes: 108}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestParseOccurredAt(t *testing.T) {
	// The front ends' text forms of one instant all store as the same UTC
	// time, to the microsecond.
	for _, text := range []string{
		"2026-10-23T18:20:00.1234567Z",
		"2026-10-23T20:20:00.123456+02:00",
		"2026-10-23T13:50:00.123456999-04:30",
	} {
		t.Run(text, func(t *testing.T) {
			got, err := ParseOccurredAt(0, "o-1", text)
			if err != nil {
				t.Fatal(err)
			}
			if want := time.Date(2026, 10, 23, 18, 20, 0, 123456000, time.UTC); !got.Equal(want) || got.Location() != time.UTC {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}

	// A refusal names the piece as Validate would, by its place from 1, in
	// its Piece and its message, and by its id only when the id is valid.
	t.Run("names the piece as Validate does", func(t *testing.T) {
		_, err := ParseOccurredAt(2, "o-17", "yesterday")
		var r *Refusal
		if !errors.As(err, &r) || r.Kind != Invalid || r.Piece != 3 || r.Message != `piece 3 (id "o-17"): occurred_at is not an RFC 3339 time` {
			t.Fatalf("got %v", err)
		}
		_, err = ParseOccurredAt(2, "bad id", "0999-01-01T00:00:00Z")
		if !errors.As(err, &r) || r.Kind != Invalid || r.Piece != 3 || r.Message != `piece 3: occurred_at must be in the years 1000 to 9999 (UTC)` {
			t.Fatalf("got %v", err)
		}
	})
}

func TestGrouped(t *testing.T) {
	// Counts in messages read as people write them; the site's mock formats
	// them the same way (en-US grouping).
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 262145: "262,145", 1048576: "1,048,576", 2147483648: "2,147,483,648"} {
		if got := grouped(n); got != want {
			t.Errorf("grouped(%d) = %s, want %s", n, got, want)
		}
	}
}
