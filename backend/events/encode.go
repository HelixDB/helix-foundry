package events

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrCorrupt is a batch line that no send could have written, or one holding
// a field its source's schema lacks: a sync of it fails for good.
var ErrCorrupt = errors.New("events: corrupt batch line")

// receivedLayout writes received_at with six fractional digits always, so the
// same pieces encode to the same length whenever they arrive: a replay
// compares lengths before it reads the stored object.
const receivedLayout = "2006-01-02T15:04:05.000000Z"

// receivedPlaceholder has the length of every received_at, for measuring lines.
var receivedPlaceholder = strings.Repeat("0", len(receivedLayout))

// appendLine appends the piece's line of the batch object:
//
//	{"id":…,"received_at":"…","occurred_at":"…","data":{…}}
//
// and a newline, with occurred_at left out when the sender gave none.
func (p piece) appendLine(dst []byte, received string) []byte {
	dst = append(dst, `{"id":`...)
	dst = append(dst, jsonString(p.id)...)
	dst = append(dst, `,"received_at":"`...)
	dst = append(dst, received...)
	dst = append(dst, '"')
	if p.occurred != nil {
		dst = append(dst, `,"occurred_at":"`...)
		dst = p.occurred.AppendFormat(dst, time.RFC3339Nano)
		dst = append(dst, '"')
	}
	dst = append(dst, `,"data":`...)
	dst = append(dst, p.data...)
	return append(dst, "}\n"...)
}

// Object is a send encoded as its batch object, with what its batch row
// records of it beside Send.Pieces and Send.Bytes.
type Object struct {
	// Body is the object: gzip, one member, of one NDJSON line per piece in
	// the order sent. Store it as application/x-ndjson with Content-Encoding
	// gzip.
	Body []byte
	// ReceivedAt is every line's received_at, in UTC to the microsecond: the
	// row's created_at.
	ReceivedAt time.Time
	// FirstAt and LastAt are the earliest and latest piece time, its
	// occurred_at or else ReceivedAt: the row's first_at and last_at.
	FirstAt, LastAt time.Time
}

// EncodeLines encodes a send as its batch object, every line stamped
// received_at: receivedAt in UTC, truncated to the microsecond, which must
// fall in the years 1000 to 9999 (the row's DATETIME).
func EncodeLines(send Send, receivedAt time.Time) (Object, error) {
	if len(send.pieces) == 0 {
		return Object{}, errors.New("events: encode a send of no pieces")
	}
	at := receivedAt.UTC().Truncate(time.Microsecond)
	if at.Year() < minYear || at.Year() > maxYear {
		return Object{}, fmt.Errorf("events: received_at %s is outside the years %d to %d", at.Format(time.RFC3339Nano), minYear, maxYear)
	}
	received := at.Format(receivedLayout)
	obj := Object{ReceivedAt: at}
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	var line []byte
	for i, p := range send.pieces {
		line = p.appendLine(line[:0], received)
		if _, err := gz.Write(line); err != nil {
			return Object{}, fmt.Errorf("events: encode batch: %w", err)
		}
		t := at
		if p.occurred != nil {
			t = *p.occurred
		}
		if i == 0 || t.Before(obj.FirstAt) {
			obj.FirstAt = t
		}
		if i == 0 || t.After(obj.LastAt) {
			obj.LastAt = t
		}
	}
	if err := gz.Close(); err != nil {
		return Object{}, fmt.Errorf("events: encode batch: %w", err)
	}
	obj.Body = body.Bytes()
	return obj, nil
}

// Line is one piece as its batch object holds it.
type Line struct {
	id string
	// received is received_at as written, and receivedAt its time.
	received   string
	receivedAt time.Time
	// occurred is occurred_at as written; "" when the sender gave none.
	occurred string
	data     json.RawMessage
}

// ID is the piece's id.
func (l Line) ID() string { return l.id }

// ReceivedAt is when the piece arrived, in UTC to the microsecond: the
// order a source's newest pieces keep (Newer).
func (l Line) ReceivedAt() time.Time { return l.receivedAt }

// DecodeLine reads one line of a batch object, its newline optional. A line
// other than one EncodeLines writes is ErrCorrupt: the line is decoded,
// written again and must come out byte for byte the same, which refuses keys
// out of order, repeated or unknown, an id no send may have, and a time in
// any form but the one stored.
func DecodeLine(line []byte) (Line, error) {
	line = bytes.TrimSuffix(line, []byte("\n"))
	if !utf8.Valid(line) {
		return Line{}, fmt.Errorf("%w: not UTF-8", ErrCorrupt)
	}
	var raw struct {
		ID         string          `json:"id"`
		ReceivedAt string          `json:"received_at"`
		OccurredAt *string         `json:"occurred_at"`
		Data       json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return Line{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	received, err := time.Parse(receivedLayout, raw.ReceivedAt)
	if err != nil || received.Year() < minYear || received.Year() > maxYear {
		return Line{}, fmt.Errorf("%w: received_at %q", ErrCorrupt, raw.ReceivedAt)
	}
	p := piece{id: raw.ID, data: raw.Data}
	l := Line{id: raw.ID, received: raw.ReceivedAt, receivedAt: received, data: raw.Data}
	if raw.OccurredAt != nil {
		at, err := time.Parse(time.RFC3339Nano, *raw.OccurredAt)
		if err == nil {
			at, err = occurredTime(at)
		}
		if err != nil {
			return Line{}, fmt.Errorf("%w: occurred_at %q", ErrCorrupt, *raw.OccurredAt)
		}
		p.occurred, l.occurred = &at, *raw.OccurredAt
	}
	if !validID(raw.ID) || len(raw.Data) == 0 || raw.Data[0] != '{' {
		return Line{}, fmt.Errorf("%w: no valid id or data", ErrCorrupt)
	}
	if again := p.appendLine(nil, raw.ReceivedAt); !bytes.Equal(again[:len(again)-1], line) {
		return Line{}, fmt.Errorf("%w: not a line a send writes", ErrCorrupt)
	}
	return l, nil
}

// Row is the line as a row of the source's events table, for UpsertJSON:
//
//	{"id":…,"received_at":"…","occurred_at":"…"|null,"<field>":…|null,…}
//
// with every field of the schema by byte order of name, null where the piece
// lacks it, and each value byte for byte as sent. The schema must hold every
// field of the line with its type (EventBatches reads both in one
// transaction, and a send's fields join the schema in its own): a line that
// breaks that, or holds a field twice, is ErrCorrupt, never a row missing or
// changing data.
func (l Line) Row(s Schema) ([]byte, error) {
	values := map[string]json.RawMessage{}
	dec := json.NewDecoder(bytes.NewReader(l.data))
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("%w: piece %q: %w", ErrCorrupt, l.id, err)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: piece %q: %w", ErrCorrupt, l.id, err)
		}
		name, _ := tok.(string) // an object's next token is its key
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("%w: piece %q: %w", ErrCorrupt, l.id, err)
		}
		t, ok := s.types[name]
		switch _, twice := values[name]; {
		case twice:
			return nil, fmt.Errorf("%w: piece %q holds %q twice", ErrCorrupt, l.id, name)
		case !ok:
			return nil, fmt.Errorf("%w: piece %q holds %q, which its source's schema lacks", ErrCorrupt, l.id, name)
		case valueType(v) != TypeNull && valueType(v) != t:
			return nil, fmt.Errorf("%w: piece %q holds %q as %s, which its source's schema holds as %s", ErrCorrupt, l.id, name, valueType(v).phrase(), t.phrase())
		}
		values[name] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("%w: piece %q: %w", ErrCorrupt, l.id, err)
	}
	row := append([]byte(`{"id":`), jsonString(l.id)...)
	row = append(row, `,"received_at":"`...)
	row = append(row, l.received...)
	row = append(row, `","occurred_at":`...)
	if l.occurred == "" {
		row = append(row, "null"...)
	} else {
		row = append(append(append(row, '"'), l.occurred...), '"')
	}
	for _, f := range s.fields {
		row = append(row, ',')
		row = append(row, f.json...)
		row = append(row, ':')
		if v, ok := values[f.Name]; ok {
			row = append(row, v...)
		} else {
			row = append(row, "null"...)
		}
	}
	return append(row, '}'), nil
}

// valueType is the type of a compacted JSON value, told by its first byte.
func valueType(v json.RawMessage) Type {
	switch v[0] {
	case '"':
		return TypeString
	case '{':
		return TypeObject
	case '[':
		return TypeArray
	case 't', 'f':
		return TypeBoolean
	case 'n':
		return TypeNull
	default:
		return TypeNumber
	}
}
