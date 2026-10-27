// Package events holds the rules and encodings of Foundry's event sources
// (connector kind events; people call them API sources): sources that people
// and their agents send pieces of data to, each a JSON object with an id. It
// is pure: it works on the bytes and values it is given and does no I/O. The
// backend's send path and the events kind share it, and the site's mock runs
// the same rule table (testdata/pieces.json).
//
// A send goes through it in this order:
//  1. Validate checks a batch of pieces against every rule that needs no
//     state and returns a Send.
//  2. Schema.Merge adds the send's fields to the source's schema, refusing a
//     type conflict or a field past max_fields.
//  3. Holding.Project refuses a send that would take the source's table past
//     a cap.
//  4. EncodeLines writes the batch object, and SameSend compares a stored
//     one with a retry of its send.
//
// A sync reads batches back with ReadBudget.Cut, DecodeLine and Line.Row.
//
// A source with a text field (Schema.WithText locks it as a string) keeps
// its newest pieces as Markdown documents of its own set: a sync keeps the
// newest pieces' places in a Window (Newest), then renders each piece's row
// with Render and names its version with Version.
package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Piece is one piece as its sender gave it.
type Piece struct {
	// ID names the piece in its source: a later piece with the same id
	// replaces it.
	ID string
	// OccurredAt is when it happened; nil when the sender gave no time.
	OccurredAt *time.Time
	// Data is the piece's data as the sender wrote it: one JSON object.
	Data []byte
}

// Limits are what one send may hold (foundry.events).
type Limits struct {
	// MaxPieces is max_pieces_per_send.
	MaxPieces int
	// MaxBytes is max_send_bytes: the data of every piece, compacted.
	MaxBytes int
	// MaxPieceBytes is max_piece_bytes: one piece's data, compacted.
	MaxPieceBytes int
}

// Send is a batch of pieces that every stateless rule accepts. Only Validate
// makes one, so a Send always holds 1 to Limits.MaxPieces valid pieces.
type Send struct {
	pieces []piece
	// bytes is the size of the send's batch object uncompressed: every line,
	// newlines included.
	bytes int64
}

type piece struct {
	id string
	// occurred is the sender's time in UTC to the microsecond; nil for none.
	occurred *time.Time
	// data is the sender's object after json.Compact: its number literals
	// and string escapes are byte for byte what was sent.
	data []byte
	// fields are data's top-level keys in the order written, each with the
	// type of its value.
	fields []Field
}

// Pieces is how many pieces the send holds: its batch row's events.
func (s Send) Pieces() int { return len(s.pieces) }

// Data is the data of the send's piece at index i, from 0 to Pieces()-1,
// after json.Compact: its number literals and string escapes byte for byte as
// sent, only its spaces gone. A front end that checks a send with Validate
// forwards these bytes, so the backend receives what was checked and no more
// than max_send_bytes of data. They are the send's own: the caller must not
// change them.
func (s Send) Data(i int) []byte { return s.pieces[i].data }

// Bytes is the uncompressed size of the send's batch object: its batch row's
// bytes. It does not depend on when the send arrives, because received_at
// always has the same length.
func (s Send) Bytes() int64 { return s.bytes }

// RefusalKind is the rule a refused send broke, which decides how a front end
// answers it.
type RefusalKind int

const (
	// Invalid is a piece or batch that breaks an envelope rule or a field's
	// locked type: INVALID_ARGUMENT.
	Invalid RefusalKind = iota + 1
	// TooManyFields is a send that would give its source more than
	// max_fields fields: FAILED_PRECONDITION, reason
	// FOUNDRY_EVENT_SOURCE_FIELDS.
	TooManyFields
	// Full is a send that would take its source's table past a cap:
	// FAILED_PRECONDITION, reason FOUNDRY_EVENT_SOURCE_FULL.
	Full
)

// Refusal is a send the rules refuse. Its message is written for the sender
// and names the piece and the rule. It quotes ids, keys and number literals,
// so it goes back to the sender and never into a log.
type Refusal struct {
	Kind RefusalKind
	// Piece is the 1-based place of the refused piece, which the message
	// names too; 0 for the send as a whole (its piece count, its bytes, the
	// field cap, a full source). Only Invalid refuses one piece.
	Piece   int
	Message string
}

func (r *Refusal) Error() string { return r.Message }

func refuse(kind RefusalKind, format string, args ...any) *Refusal {
	return &Refusal{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// refusePiece refuses the piece at index, from 0: its Piece is index+1, and
// its message names the piece by that place and by its id when the id is one.
func refusePiece(kind RefusalKind, index int, id string, format string, args ...any) *Refusal {
	name := fmt.Sprintf("piece %d", index+1)
	if validID(id) {
		name += fmt.Sprintf(" (id %q)", id)
	}
	return &Refusal{Kind: kind, Piece: index + 1, Message: name + ": " + fmt.Sprintf(format, args...)}
}

const (
	maxIDBytes  = 128
	maxKeyBytes = 64
	// maxDepth is how deep data may nest, its own object counting as 1. The
	// engine's serde_json refuses past 128, and one bad record stops the
	// engine's whole read.
	maxDepth = 64
	// minYear and maxYear bound a piece's time to MySQL's DATETIME range.
	minYear, maxYear = 1000, 9999
)

// reserved are the row's own columns: data may not carry them.
var reserved = map[string]string{
	"id":          "send it as the piece's id",
	"received_at": "Foundry sets it when the piece arrives",
	"occurred_at": "send it as the piece's occurred_at",
}

// Validate checks a send against every rule that needs no state: the number
// of pieces, each piece's id, time and data, and the send's size. It checks
// the pieces in order and returns the first rule broken as a *Refusal of kind
// Invalid, whose Piece is the refused piece's place, or 0 for the send's count
// or size. A send is stored whole or not at all, so one bad piece refuses it.
// The schema's rules come after, in Schema.Merge.
func Validate(pieces []Piece, limits Limits) (Send, error) {
	if len(pieces) == 0 || len(pieces) > limits.MaxPieces {
		held := "none"
		if len(pieces) > 0 {
			held = grouped(int64(len(pieces)))
		}
		return Send{}, refuse(Invalid, "a send holds 1 to %s pieces; this one holds %s", grouped(int64(limits.MaxPieces)), held)
	}
	send := Send{pieces: make([]piece, 0, len(pieces))}
	first := make(map[string]int, len(pieces))
	var data int
	// line holds each piece's line, measured with a received_at of the
	// fixed length every line has.
	var line []byte
	for i, p := range pieces {
		if !validID(p.ID) {
			return Send{}, refusePiece(Invalid, i, p.ID, "id must be 1 to %d printable ASCII characters with no spaces", maxIDBytes)
		}
		if j, ok := first[p.ID]; ok {
			return Send{}, &Refusal{Kind: Invalid, Piece: i + 1, Message: fmt.Sprintf("piece %d repeats the id of piece %d", i+1, j+1)}
		}
		first[p.ID] = i
		v := piece{id: p.ID}
		if p.OccurredAt != nil {
			t, err := occurredTime(*p.OccurredAt)
			if err != nil {
				return Send{}, refusePiece(Invalid, i, p.ID, "%s", err)
			}
			v.occurred = &t
		}
		var err error
		if v.data, v.fields, err = checkData(p.Data, limits.MaxPieceBytes); err != nil {
			return Send{}, refusePiece(Invalid, i, p.ID, "%s", err)
		}
		data += len(v.data)
		line = v.appendLine(line[:0], receivedPlaceholder)
		send.bytes += int64(len(line))
		send.pieces = append(send.pieces, v)
	}
	if data > limits.MaxBytes {
		return Send{}, refuse(Invalid, "a send holds at most %s bytes of data, compacted; this one holds %s", grouped(int64(limits.MaxBytes)), grouped(int64(data)))
	}
	return send, nil
}

// ParseOccurredAt reads a piece's occurred_at as a front end receives it,
// RFC 3339 text, for the piece at index (from 0) with this id. It refuses
// text that is not RFC 3339 and a time outside the years 1000 to 9999 in
// UTC, as Validate would, with the same *Refusal.
func ParseOccurredAt(index int, id, text string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, refusePiece(Invalid, index, id, "occurred_at is not an RFC 3339 time")
	}
	if t, err = occurredTime(t); err != nil {
		return time.Time{}, refusePiece(Invalid, index, id, "%s", err)
	}
	return t, nil
}

// occurredTime is t as a piece stores it: in UTC, to the microsecond, as a
// DATETIME(6) holds it. Truncating never moves a time across a year, so a
// time in range stays in range.
func occurredTime(t time.Time) (time.Time, error) {
	t = t.UTC().Truncate(time.Microsecond)
	if t.Year() < minYear || t.Year() > maxYear {
		return time.Time{}, fmt.Errorf("occurred_at must be in the years %d to %d (UTC)", minYear, maxYear)
	}
	return t, nil
}

// validID is 1 to 128 bytes, each printable ASCII other than space.
func validID(id string) bool {
	return len(id) >= 1 && len(id) <= maxIDBytes && strings.IndexFunc(id, func(r rune) bool { return r < 0x21 || r > 0x7E }) < 0
}

// checkData returns a piece's data compacted, and its top-level fields, if it
// is one engine-safe JSON object within maxBytes.
func checkData(raw []byte, maxBytes int) ([]byte, []Field, error) {
	// encoding/json accepts invalid UTF-8 in strings, and the engine does not.
	if !utf8.Valid(raw) {
		return nil, nil, errors.New("data is not valid UTF-8")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, nil, errors.New("data is not valid JSON")
	}
	data := compact.Bytes()
	if data[0] != '{' {
		return nil, nil, errors.New("data is not a JSON object")
	}
	if len(data) > maxBytes {
		return nil, nil, fmt.Errorf("data is %s bytes compacted; a piece holds at most %s", grouped(int64(len(data))), grouped(int64(maxBytes)))
	}
	fields, err := scan(data)
	if err != nil {
		return nil, nil, err
	}
	return data, fields, nil
}

// scan checks valid, compacted JSON in one token pass for what the engine's
// serde_json refuses and encoding/json accepts (nesting past maxDepth, a
// number out of float64's range, a lone surrogate escape), and the rules of
// top-level keys. It returns the top-level fields in the order written.
func scan(data []byte) ([]Field, error) {
	// frame is one open object or array; key marks an object expecting a key.
	type frame struct{ object, key bool }
	var (
		stack  []frame
		fields []Field
		seen   = map[string]bool{}
		// prev is where the previous token ended: data[prev:end] is a
		// token's text, after the ',' or ':' before it (compacted JSON has
		// no space).
		prev int64
		// name is the top-level key whose value comes next.
		name string
	)
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return fields, nil
		}
		if err != nil {
			// Unreachable: json.Compact has accepted data.
			return nil, fmt.Errorf("data is not valid JSON: %w", err)
		}
		end := dec.InputOffset()
		text := data[prev:end]
		prev = end
		if s, ok := tok.(string); ok {
			if err := pairedSurrogates(text); err != nil {
				return nil, err
			}
			if top := len(stack) - 1; top >= 0 && stack[top].object && stack[top].key {
				stack[top].key = false
				if top == 0 {
					if err := checkKey(s); err != nil {
						return nil, err
					}
					if seen[s] {
						return nil, fmt.Errorf("data has the top-level key %q twice", s)
					}
					seen[s], name = true, s
				}
				continue
			}
		}
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			stack = stack[:len(stack)-1]
		} else {
			if len(stack) == 1 {
				fields = append(fields, Field{Name: name, Type: tokenType(tok)})
			}
			switch v := tok.(type) {
			case json.Delim:
				if len(stack) == maxDepth {
					return nil, fmt.Errorf("data nests deeper than %d", maxDepth)
				}
				stack = append(stack, frame{object: v == '{', key: v == '{'})
				continue
			case json.Number:
				if f, err := strconv.ParseFloat(v.String(), 64); err != nil || math.IsInf(f, 0) {
					return nil, fmt.Errorf("data holds %s, which is not a finite number", clip(v.String()))
				}
			}
		}
		// A value has ended: an object around it expects a key next.
		if top := len(stack) - 1; top >= 0 && stack[top].object {
			stack[top].key = true
		}
	}
}

// tokenType is the JSON type of the value a token starts.
func tokenType(tok json.Token) Type {
	switch v := tok.(type) {
	case json.Delim:
		if v == '{' {
			return TypeObject
		}
		return TypeArray
	case string:
		return TypeString
	case json.Number:
		return TypeNumber
	case bool:
		return TypeBoolean
	default:
		return TypeNull
	}
}

// pairedSurrogates refuses a string's text with a lone surrogate escape: a
// \uD800-\uDBFF not followed by a \uDC00-\uDFFF escape, or the latter alone.
// encoding/json decodes either to U+FFFD, and serde_json refuses both. text
// is valid JSON, so every escape is complete.
func pairedSurrogates(text []byte) error {
	for i := 0; i < len(text); i++ {
		if text[i] != '\\' {
			continue
		}
		if text[i+1] != 'u' {
			i++ // a one-character escape, such as \\ or \"
			continue
		}
		code, _ := strconv.ParseUint(string(text[i+2:i+6]), 16, 16)
		lone := code >= 0xDC00 && code <= 0xDFFF
		if code >= 0xD800 && code <= 0xDBFF {
			var low uint64
			if next := text[i+6:]; len(next) >= 6 && next[0] == '\\' && next[1] == 'u' {
				low, _ = strconv.ParseUint(string(next[2:6]), 16, 16)
			}
			if lone = low < 0xDC00 || low > 0xDFFF; !lone {
				i += 6 // the pair's low half
			}
		}
		if lone {
			return fmt.Errorf("data has a %s escape with no pair", text[i:i+6])
		}
		i += 5
	}
	return nil
}

// checkKey applies the rules of a top-level key, which becomes a column: 1
// to 64 bytes, no control character, no backtick (prompts show field names
// in code spans), and not one of the row's own columns.
func checkKey(key string) error {
	switch {
	case len(key) < 1 || len(key) > maxKeyBytes:
		return fmt.Errorf("data has a top-level key of %d bytes; a key is 1 to %d bytes", len(key), maxKeyBytes)
	case strings.IndexFunc(key, unicode.IsControl) >= 0:
		return errors.New("data has a top-level key with a control character")
	case strings.ContainsRune(key, '`'):
		return errors.New("data has a top-level key with a backtick")
	}
	if fix, ok := reserved[key]; ok {
		return fmt.Errorf("data may not have a top-level %s; %s", key, fix)
	}
	return nil
}

// clip shortens a number literal for a message: one may be as long as its piece.
func clip(literal string) string {
	const most = 24
	if len(literal) <= most {
		return literal
	}
	return literal[:most] + "…"
}

// grouped writes a count, never negative, with commas between thousands.
func grouped(n int64) string {
	digits := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(digits[i])
	}
	return b.String()
}
