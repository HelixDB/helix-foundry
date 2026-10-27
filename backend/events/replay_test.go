package events

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestSameSend(t *testing.T) {
	first := time.Date(2026, 10, 23, 18, 25, 1, 123456000, time.UTC)
	retry := first.Add(7*time.Second + 654321*time.Nanosecond)
	at := time.Date(2026, 10, 23, 18, 20, 0, 500000000, time.UTC)
	stored := func(t *testing.T, pieces ...Piece) []byte {
		t.Helper()
		obj, err := EncodeLines(validSend(t, pieces...), first)
		if err != nil {
			t.Fatal(err)
		}
		return obj.Body
	}
	original := []Piece{
		{ID: "o-17", OccurredAt: &at, Data: []byte(`{"amount":12.5,"customer":"c_1"}`)},
		{ID: "o-18", Data: []byte(`{"amount":3}`)},
	}

	// A retry after a timeout carries the same pieces and arrives later: it
	// must learn its first send was stored, not store a second batch.
	t.Run("matches its own send stored earlier", func(t *testing.T) {
		send := validSend(t, original...)
		obj, err := EncodeLines(send, retry)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(gunzip(t, obj.Body))) != send.Bytes() {
			t.Fatal("a later arrival encodes to another size")
		}
		same, err := SameSend(bytes.NewReader(stored(t, original...)), send)
		if err != nil || !same {
			t.Fatalf("same %v, err %v", same, err)
		}
	})

	// A sender's client may write the same instant in another zone or
	// with another precision: it is the same piece.
	t.Run("matches the same instant written another way", func(t *testing.T) {
		zoned := at.In(time.FixedZone("+05:30", 5*60*60+30*60)).Add(999 * time.Nanosecond)
		send := validSend(t, Piece{ID: "o-17", OccurredAt: &zoned, Data: original[0].Data}, original[1])
		same, err := SameSend(bytes.NewReader(stored(t, original...)), send)
		if err != nil || !same {
			t.Fatalf("same %v, err %v", same, err)
		}
	})

	// Data compacts before compare, as before store, so whitespace alone is
	// not another send.
	t.Run("matches data that differs only in whitespace", func(t *testing.T) {
		send := validSend(t, Piece{ID: "o-17", OccurredAt: &at, Data: []byte(`{ "amount" : 12.5 , "customer" : "c_1" }`)}, original[1])
		same, err := SameSend(bytes.NewReader(stored(t, original...)), send)
		if err != nil || !same {
			t.Fatalf("same %v, err %v", same, err)
		}
	})

	// A client token reused for other pieces must be refused, so each field
	// that makes a piece what it is makes a different send: its id, its
	// time or its absence, any byte of its data (12.50 is not 12.5: the
	// engine reads the text), and the pieces' order and number.
	other := at.Add(time.Microsecond)
	for name, pieces := range map[string][]Piece{
		"another id":          {{ID: "o-19", OccurredAt: &at, Data: original[0].Data}, original[1]},
		"another occurred_at": {{ID: "o-17", OccurredAt: &other, Data: original[0].Data}, original[1]},
		"no occurred_at":      {{ID: "o-17", Data: original[0].Data}, original[1]},
		"an occurred_at":      {original[0], {ID: "o-18", OccurredAt: &at, Data: original[1].Data}},
		"a data byte":         {{ID: "o-17", OccurredAt: &at, Data: []byte(`{"amount":12.5,"customer":"c_2"}`)}, original[1]},
		"a number's text":     {{ID: "o-17", OccurredAt: &at, Data: []byte(`{"amount":12.50,"customer":"c_1"}`)}, original[1]},
		"the order":           {original[1], original[0]},
		"one piece fewer":     {original[0]},
		"one piece more":      {original[0], original[1], {ID: "o-20", Data: []byte(`{}`)}},
	} {
		t.Run("tells apart "+name, func(t *testing.T) {
			same, err := SameSend(bytes.NewReader(stored(t, original...)), validSend(t, pieces...))
			if err != nil || same {
				t.Fatalf("same %v, err %v", same, err)
			}
		})
	}

	// Equal sizes do not make equal sends: the same bytes split into other
	// pieces are another send.
	t.Run("tells apart pieces of the same total size", func(t *testing.T) {
		two := validSend(t, Piece{ID: "a", Data: []byte(`{}`)}, Piece{ID: "b", Data: []byte(`{}`)})
		one := stored(t, Piece{ID: "a", Data: []byte(`{"s":"` + strings.Repeat("a", 59) + `"}`)})
		if int64(len(gunzip(t, one))) != two.Bytes() {
			t.Fatalf("sizes %d and %d", len(gunzip(t, one)), two.Bytes())
		}
		same, err := SameSend(bytes.NewReader(one), two)
		if err != nil || same {
			t.Fatalf("same %v, err %v", same, err)
		}
	})

	// An object that cannot be read whole is an error the sender retries,
	// never a verdict: "different" would refuse a retry its first send
	// stored, and "same" would report pieces stored that were not.
	t.Run("fails on an object that is not gzip", func(t *testing.T) {
		if _, err := SameSend(bytes.NewReader([]byte("not gzip")), validSend(t, original...)); err == nil {
			t.Fatal("no error")
		}
	})
	t.Run("fails on a truncated object", func(t *testing.T) {
		body := stored(t, original...)
		if _, err := SameSend(bytes.NewReader(body[:len(body)-6]), validSend(t, original...)); err == nil {
			t.Fatal("no error")
		}
	})
	t.Run("fails on a corrupt line of the same size", func(t *testing.T) {
		raw := []byte(gunzip(t, stored(t, original...)))
		raw[0] = '['
		var body bytes.Buffer
		gz := gzip.NewWriter(&body)
		_, _ = gz.Write(raw)
		_ = gz.Close()
		if _, err := SameSend(&body, validSend(t, original...)); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("got %v, want ErrCorrupt", err)
		}
	})
	t.Run("fails on an object whose read fails", func(t *testing.T) {
		body := stored(t, original...)
		r := io.MultiReader(bytes.NewReader(body[:20]), iotestErr{})
		if _, err := SameSend(r, validSend(t, original...)); err == nil {
			t.Fatal("no error")
		}
	})
}

// iotestErr is a store's body failing mid-read.
type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
