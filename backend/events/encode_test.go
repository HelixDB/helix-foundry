package events

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// gunzip is a gzip body uncompressed.
func gunzip(t *testing.T, body []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// schemaOf is the schema these types make.
func schemaOf(types map[string]Type) Schema { return newSchema(types) }

func TestEncodeLines(t *testing.T) {
	received := time.Date(2026, 10, 23, 18, 25, 1, 123456789, time.UTC)

	// The batch object's line format is stored data: one line per
	// piece, in the order sent, keys in this order.
	t.Run("writes the spec's line for a piece", func(t *testing.T) {
		at := time.Date(2026, 10, 23, 18, 20, 0, 0, time.UTC)
		send := validSend(t,
			Piece{ID: "o-17", OccurredAt: &at, Data: []byte(`{"amount":12.5,"customer":"c_1"}`)},
			Piece{ID: "o-18", Data: []byte(`{}`)},
		)
		want := `{"id":"o-17","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":"2026-10-23T18:20:00Z","data":{"amount":12.5,"customer":"c_1"}}` + "\n" +
			`{"id":"o-18","received_at":"2026-10-23T18:25:01.123456Z","data":{}}` + "\n"
		if got := lines(t, send, received); got != want {
			t.Fatalf("got  %s\nwant %s", got, want)
		}
	})

	// A replay compares the row's bytes with the retry's before reading the
	// object, so the same pieces must encode to the same length whenever
	// they arrive: received_at always has six fractional digits.
	t.Run("encodes the same pieces to the same length at any time", func(t *testing.T) {
		send := validSend(t, Piece{ID: `a"b\c`, Data: []byte(`{"a":1}`)}, Piece{ID: "o-2", Data: []byte(`{}`)})
		for _, at := range []time.Time{
			time.Date(2026, 10, 23, 18, 25, 1, 0, time.UTC),
			time.Date(2026, 10, 23, 18, 25, 1, 999999999, time.UTC),
			time.Date(1000, 1, 1, 0, 0, 0, 1000, time.FixedZone("+01", 60*60)).Add(time.Hour),
			time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
		} {
			got := lines(t, send, at)
			if int64(len(got)) != send.Bytes() {
				t.Fatalf("at %s: %d bytes, Send.Bytes %d", at, len(got), send.Bytes())
			}
			if !strings.Contains(got, `"received_at":"`+at.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")+`"`) {
				t.Fatalf("at %s: %s", at, got)
			}
		}
	})

	// The engine reads integers up to 2^64 exactly; Foundry must not round
	// them on the way into the object.
	t.Run("keeps integers past 2^53 byte for byte", func(t *testing.T) {
		send := validSend(t, Piece{ID: "o-1", Data: []byte(`{"a":9223372036854775807,"b":18446744073709551617,"c":9007199254740993}`)})
		if got := lines(t, send, received); !strings.Contains(got, `"data":{"a":9223372036854775807,"b":18446744073709551617,"c":9007199254740993}`) {
			t.Fatalf("got %s", got)
		}
	})

	// The batch row's first_at and last_at span the pieces' times, a piece
	// without one counting as received.
	t.Run("spans the pieces' times, received_at standing in for none", func(t *testing.T) {
		early, late := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		obj, err := EncodeLines(validSend(t,
			Piece{ID: "a", OccurredAt: &late, Data: []byte(`{}`)},
			Piece{ID: "b", OccurredAt: &early, Data: []byte(`{}`)},
		), received)
		if err != nil {
			t.Fatal(err)
		}
		if !obj.FirstAt.Equal(early) || !obj.LastAt.Equal(late) {
			t.Fatalf("first %s, last %s", obj.FirstAt, obj.LastAt)
		}
		obj, err = EncodeLines(validSend(t, Piece{ID: "a", OccurredAt: &early, Data: []byte(`{}`)}, Piece{ID: "b", Data: []byte(`{}`)}), received)
		if err != nil {
			t.Fatal(err)
		}
		at := received.Truncate(time.Microsecond)
		if !obj.FirstAt.Equal(early) || !obj.LastAt.Equal(at) || !obj.ReceivedAt.Equal(at) {
			t.Fatalf("first %s, last %s, received %s", obj.FirstAt, obj.LastAt, obj.ReceivedAt)
		}
	})

	// snapshot.S3 stores objects as one gzip member, and readers may assume it.
	t.Run("writes one gzip member", func(t *testing.T) {
		obj, err := EncodeLines(validSend(t, Piece{ID: "a", Data: []byte(`{}`)}), received)
		if err != nil {
			t.Fatal(err)
		}
		r := bytes.NewReader(obj.Body)
		gz, err := gzip.NewReader(r)
		if err != nil {
			t.Fatal(err)
		}
		gz.Multistream(false)
		if _, err := io.Copy(io.Discard, gz); err != nil {
			t.Fatal(err)
		}
		if r.Len() != 0 {
			t.Fatalf("%d bytes after the first member", r.Len())
		}
	})

	// received_at is the row's created_at, a DATETIME, and must keep its
	// fixed length: a time outside 1000-9999 is a caller's bug, not a line.
	t.Run("refuses a receipt time outside the row's years", func(t *testing.T) {
		send := validSend(t, Piece{ID: "a", Data: []byte(`{}`)})
		if _, err := EncodeLines(send, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
			t.Fatal("encoded a year-10000 receipt")
		}
		if _, err := EncodeLines(send, time.Time{}); err == nil {
			t.Fatal("encoded the zero time")
		}
	})

	// Only Validate makes a Send; a zero one would store an empty batch.
	t.Run("refuses a send no rule passed", func(t *testing.T) {
		if _, err := EncodeLines(Send{}, received); err == nil {
			t.Fatal("encoded an empty send")
		}
	})
}

func TestRow(t *testing.T) {
	line := func(t *testing.T, piece Piece) Line {
		t.Helper()
		body := lines(t, validSend(t, piece), time.Date(2026, 10, 23, 18, 25, 1, 123456000, time.UTC))
		l, err := DecodeLine([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	at := time.Date(2026, 10, 23, 18, 20, 0, 0, time.UTC)

	// The snapshot row format is stored data: id, received_at,
	// occurred_at, then every schema field by byte order of name, null where
	// the piece lacks it, values as sent. Every row carries every column, so
	// the engine catalogs a field even when the oldest rows lack it.
	t.Run("writes every schema field in name order, null where absent", func(t *testing.T) {
		l := line(t, Piece{ID: "o-17", OccurredAt: &at, Data: []byte(`{"customer":"c_1","amount":12.50}`)})
		row, err := l.Row(schemaOf(map[string]Type{"note": TypeNull, "amount": TypeNumber, "customer": TypeString, "Zeta": TypeBoolean}))
		if err != nil {
			t.Fatal(err)
		}
		want := `{"id":"o-17","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":"2026-10-23T18:20:00Z","Zeta":null,"amount":12.50,"customer":"c_1","note":null}`
		if string(row) != want {
			t.Fatalf("got  %s\nwant %s", row, want)
		}
		if l.ID() != "o-17" {
			t.Fatalf("id %q", l.ID())
		}
	})

	// Every JSON type reaches the row as the sender wrote it, and each is
	// checked against the type its field holds.
	t.Run("keeps a value of every type as sent", func(t *testing.T) {
		l := line(t, Piece{ID: "o-1", Data: []byte(`{"o":{"k":[1]},"a":[{"x":null}],"t":true,"f":false,"s":"x","n":-0.0e1,"z":null}`)})
		s := schemaOf(map[string]Type{"o": TypeObject, "a": TypeArray, "t": TypeBoolean, "f": TypeBoolean, "s": TypeString, "n": TypeNumber, "z": TypeNull})
		row, err := l.Row(s)
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":null,"a":[{"x":null}],"f":false,"n":-0.0e1,"o":{"k":[1]},"s":"x","t":true,"z":null}`; string(row) != want {
			t.Fatalf("got  %s\nwant %s", row, want)
		}
		for name, other := range map[string]Type{"o": TypeArray, "a": TypeObject, "t": TypeString, "s": TypeNumber, "n": TypeBoolean} {
			t.Run(name+" as "+other.String(), func(t *testing.T) {
				types := map[string]Type{"o": TypeObject, "a": TypeArray, "t": TypeBoolean, "f": TypeBoolean, "s": TypeString, "n": TypeNumber, "z": TypeNull}
				types[name] = other
				if _, err := l.Row(schemaOf(types)); !errors.Is(err, ErrCorrupt) {
					t.Fatalf("got %v, want ErrCorrupt", err)
				}
			})
		}
	})

	// A piece sent without a time still has the column, as null.
	t.Run("writes occurred_at as null when none was sent", func(t *testing.T) {
		row, err := line(t, Piece{ID: "o-1", Data: []byte(`{}`)}).Row(Schema{})
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":null}`; string(row) != want {
			t.Fatalf("got %s", row)
		}
	})

	// A field name is JSON-encoded as snapshot rows encode it, so the row
	// decodes to the name the sender gave.
	t.Run("encodes field names as snapshot rows do", func(t *testing.T) {
		name := "a\"b\\c<d>\u2028é"
		row, err := line(t, Piece{ID: "o-1", Data: []byte(`{"a\"b\\c\u003cd>` + "\u2028" + `\u00e9":1}`)}).Row(schemaOf(map[string]Type{name: TypeNumber}))
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(row, &decoded); err != nil {
			t.Fatal(err)
		}
		if string(decoded[name]) != "1" {
			t.Fatalf("row %s", row)
		}
	})

	// EventBatches returns batches with the schema read in the same
	// transaction, so a line's field is always in it; one that is not, or
	// holds another type, is corruption, never a row quietly missing data.
	t.Run("refuses a line its schema does not cover", func(t *testing.T) {
		l := line(t, Piece{ID: "o-1", Data: []byte(`{"amount":12.5,"note":"x"}`)})
		for name, s := range map[string]Schema{
			"a field missing": schemaOf(map[string]Type{"amount": TypeNumber}),
			"another type":    schemaOf(map[string]Type{"amount": TypeString, "note": TypeString}),
			"a type unlocked": schemaOf(map[string]Type{"amount": TypeNull, "note": TypeString}),
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := l.Row(s); !errors.Is(err, ErrCorrupt) {
					t.Fatalf("got %v, want ErrCorrupt", err)
				}
			})
		}
	})

	// Validate refuses a key twice, so a line holding one is corruption,
	// not a row whose last value quietly wins.
	t.Run("refuses a field twice", func(t *testing.T) {
		l, err := DecodeLine([]byte(`{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","data":{"a":1,"a":2}}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.Row(schemaOf(map[string]Type{"a": TypeNumber})); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("got %v, want ErrCorrupt", err)
		}
	})

	// DecodeLine checks only that data starts an object; the rest is read
	// here, and a broken one is corruption too.
	t.Run("refuses data that does not decode", func(t *testing.T) {
		l := line(t, Piece{ID: "o-1", Data: []byte(`{"amount":12.5}`)})
		l.data = json.RawMessage(`{"amount":`)
		if _, err := l.Row(schemaOf(map[string]Type{"amount": TypeNumber})); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("got %v, want ErrCorrupt", err)
		}
	})
}

func TestDecodeLine(t *testing.T) {
	// A line no send could have written must stop the sync for good rather
	// than store a wrong row: each of these is ErrCorrupt.
	for name, line := range map[string]string{
		"not JSON":                            `{"id":"o-1"`,
		"an unknown key":                      `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","data":{},"extra":1}`,
		"two objects":                         `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","data":{}}{}`,
		"no id":                               `{"received_at":"2026-10-23T18:25:01.123456Z","data":{}}`,
		"an id with a space":                  `{"id":"o 1","received_at":"2026-10-23T18:25:01.123456Z","data":{}}`,
		"a short received_at":                 `{"id":"o-1","received_at":"2026-10-23T18:25:01.123Z","data":{}}`,
		"no received_at":                      `{"id":"o-1","data":{}}`,
		"a bad occurred_at":                   `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":"","data":{}}`,
		"data not an object":                  `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","data":[]}`,
		"null data":                           `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","data":null}`,
		"no data":                             `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z"}`,
		"an occurred_at with an offset":       `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":"2026-10-23T20:20:00+02:00","data":{}}`,
		"an occurred_at before the year 1000": `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":"0999-12-31T23:59:59Z","data":{}}`,
		"an occurred_at with trailing zeros":  `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":"2026-10-23T18:20:00.500Z","data":{}}`,
		"an occurred_at past the microsecond": `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":"2026-10-23T18:20:00.1234567Z","data":{}}`,
		"a received_at before the year 1000":  `{"id":"o-1","received_at":"0001-01-01T00:00:00.000000Z","data":{}}`,
		"data twice":                          `{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","data":{"a":1},"data":{"b":2}}`,
		"an id twice":                         `{"id":"o-1","id":"o-2","received_at":"2026-10-23T18:25:01.123456Z","data":{}}`,
		"a key in another case":               `{"ID":"o-1","received_at":"2026-10-23T18:25:01.123456Z","data":{}}`,
		"keys in another order":               `{"received_at":"2026-10-23T18:25:01.123456Z","id":"o-1","data":{}}`,
		"space between keys":                  `{"id":"o-1", "received_at":"2026-10-23T18:25:01.123456Z","data":{}}`,
		"an id escaped":                       `{"id":"o\u002d1","received_at":"2026-10-23T18:25:01.123456Z","data":{}}`,
		"bytes that are not UTF-8":            "{\"id\":\"o-1\",\"received_at\":\"2026-10-23T18:25:01.123456Z\",\"data\":{\"s\":\"\xff\"}}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeLine([]byte(line)); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("got %v, want ErrCorrupt", err)
			}
		})
	}

	// Lines split from an object keep their newline; it is not content.
	t.Run("accepts a line with its newline", func(t *testing.T) {
		l, err := DecodeLine([]byte(`{"id":"o-1","received_at":"2026-10-23T18:25:01.123456Z","data":{}}` + "\n"))
		if err != nil || l.ID() != "o-1" {
			t.Fatalf("got %v, %v", l, err)
		}
	})
}
