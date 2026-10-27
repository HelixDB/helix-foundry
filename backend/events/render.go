package events

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrCorruptRow is a row of a source's events table that no sync could have
// written, or one holding a field or type its source's schema lacks:
// rendering it fails the sync for good.
var ErrCorruptRow = errors.New("events: corrupt events-table row")

const (
	// maxValueRunes is the most of a string a line shows, its "…" included.
	maxValueRunes = 500
	// maxTitleRunes is the most a title holds, its "…" included.
	maxTitleRunes = 120
	// maxListed is how many values of an array a line shows.
	maxListed = 20
	// maxShownDepth is how deep a line's path goes, the top-level field
	// counting as 1: <field>.<key>.<key>.
	maxShownDepth = 3
	// maxNestedLines is how many lines all the fields inside objects get.
	maxNestedLines = 50
)

// Rendered is a piece as a document of its source's set.
type Rendered struct {
	// Title is the document's name: the text's first line that is not
	// blank, its spaces collapsed, at most 120 runes (ending in "…" when
	// cut); the piece's id when its text is null, absent or blank.
	Title string
	// Body is the Markdown document (Version names it).
	Body []byte
}

// Render is a row of a source's events table as its piece's Markdown
// document, textField naming the source's text field, which s must hold as a
// string (WithText):
//
//	# <title>
//
//	- Piece: <id>
//	- Occurred: <occurred_at>        (when the piece has one)
//	- <field>: <value>               (every other field, by name byte order)
//	- <field>.<key>: <value>         (inside objects, by key byte order)
//
//	<the text, verbatim>             (when it is not blank)
//
// Each field line shows a string on one line, every run of line breaks
// folded to a space, cut at 500 runes with "…"; a number as sent, byte for
// byte; a boolean; and an array of scalars as "a, b, c", the first 20 then
// ", and <n> more", or one holding an object or array as "<n> records".
// Null, an empty string and an empty array have no line. A path goes at most
// 3 deep (<field>.<key>.<key>), deeper objects having no lines, and the
// fields inside objects get 50 lines in all, the rest none. received_at is
// never shown, so a resend of the same data renders byte for byte the same,
// as does a row padded with nulls. A row no sync writes (not one JSON
// object, a key twice, no valid id, an occurred_at that is not a time, or a
// field or type s lacks) is ErrCorruptRow; a textField s does not hold as a
// string is an error of the source's settings, never of the row.
func Render(row []byte, s Schema, textField string) (Rendered, error) {
	if s.types[textField] != TypeString {
		return Rendered{}, fmt.Errorf("events: render: the source's schema holds no string field %q to be its text", textField)
	}
	values, err := rowValues(row)
	if err != nil {
		return Rendered{}, fmt.Errorf("%w: %w", ErrCorruptRow, err)
	}
	var occurred string
	for _, name := range slices.Sorted(maps.Keys(values)) {
		switch v := values[name]; name {
		case "id", "received_at":
		case "occurred_at":
			if v == nil {
				continue
			}
			at, _ := v.(string)
			if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
				return Rendered{}, fmt.Errorf("%w: its occurred_at is not a time", ErrCorruptRow)
			}
			occurred = at
		default:
			held, ok := s.types[name]
			switch got := typeOf(v); {
			case !ok:
				return Rendered{}, fmt.Errorf("%w: it holds %q, which its source's schema lacks", ErrCorruptRow, name)
			case got != TypeNull && got != held:
				return Rendered{}, fmt.Errorf("%w: it holds %q as %s, which its source's schema holds as %s", ErrCorruptRow, name, got.phrase(), held.phrase())
			}
		}
	}
	id, _ := values["id"].(string)
	if !validID(id) {
		return Rendered{}, fmt.Errorf("%w: it has no valid id", ErrCorruptRow)
	}
	text, _ := values[textField].(string)
	heading := title(text)
	if heading == "" {
		heading = id
		text = "" // blank: the document has no text
	}
	var body bytes.Buffer
	fmt.Fprintf(&body, "# %s\n\n- Piece: %s\n", heading, id)
	if occurred != "" {
		fmt.Fprintf(&body, "- Occurred: %s\n", occurred)
	}
	l := listing{body: &body}
	for _, f := range s.fields {
		if f.Name != textField {
			l.field(oneLine(f.Name), values[f.Name], 1)
		}
	}
	if text != "" {
		body.WriteByte('\n')
		body.WriteString(text)
	}
	return Rendered{Title: heading, Body: body.Bytes()}, nil
}

// Version is a document's version: the lowercase hex SHA-256 of its body,
// so a piece keeps its version for as long as its document reads the same.
func Version(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// rowValues reads a row's top-level object, refusing anything but one JSON
// object with each key once. Numbers keep their literals (json.Number).
func rowValues(row []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(row))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("it is not a JSON object")
	}
	values := map[string]any{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := tok.(string) // an object's next token is its key
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if _, twice := values[name]; twice {
			return nil, fmt.Errorf("it holds %q twice", name)
		}
		values[name] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("it is more than one JSON object")
	}
	return values, nil
}

// typeOf is the JSON type of a value decoded with json.Number.
func typeOf(v any) Type {
	switch v.(type) {
	case string:
		return TypeString
	case json.Number:
		return TypeNumber
	case bool:
		return TypeBoolean
	case map[string]any:
		return TypeObject
	case []any:
		return TypeArray
	}
	return TypeNull
}

// listing writes a document's field lines. nested counts those of fields
// inside objects, which stop at maxNestedLines.
type listing struct {
	body   *bytes.Buffer
	nested int
}

// field writes the lines of the value at path, depth deep: one line for a
// value with something to show, and those of its keys for an object above
// maxShownDepth.
func (l *listing) field(path string, v any, depth int) {
	if object, ok := v.(map[string]any); ok {
		if depth < maxShownDepth {
			for _, key := range slices.Sorted(maps.Keys(object)) {
				l.field(path+"."+oneLine(key), object[key], depth+1)
			}
		}
		return
	}
	value := shown(v)
	if value == "" || depth > 1 && l.nested == maxNestedLines {
		return
	}
	if depth > 1 {
		l.nested++
	}
	fmt.Fprintf(l.body, "- %s: %s\n", path, value)
}

// shown is a scalar or array as a line shows it; "" for null, an empty
// string and an empty array, which have no line.
func shown(v any) string {
	switch v := v.(type) {
	case string:
		return oneLine(v)
	case json.Number:
		return string(v)
	case bool:
		return strconv.FormatBool(v)
	case []any:
		return listed(v)
	}
	return ""
}

// listed is an array as a line shows it: its first 20 scalars, null
// included, then how many more; or how many records it holds when any of
// them is an object or array.
func listed(items []any) string {
	switch {
	case len(items) == 0:
		return ""
	case slices.ContainsFunc(items, func(v any) bool { t := typeOf(v); return t == TypeObject || t == TypeArray }):
		if len(items) == 1 {
			return "1 record"
		}
		return grouped(int64(len(items))) + " records"
	}
	values := make([]string, min(len(items), maxListed))
	for i := range values {
		values[i] = "null"
		if items[i] != nil {
			values[i] = shown(items[i])
		}
	}
	list := strings.Join(values, ", ")
	if more := len(items) - maxListed; more > 0 {
		list += ", and " + grouped(int64(more)) + " more"
	}
	return list
}

// title is text's first line that is not blank, its spaces collapsed, cut
// at maxTitleRunes; "" when every line is blank.
func title(text string) string {
	for line := range strings.FieldsFuncSeq(text, lineBreak) {
		if words := strings.Fields(line); len(words) > 0 {
			return cut(strings.Join(words, " "), maxTitleRunes)
		}
	}
	return ""
}

// oneLine is s with every run of line breaks folded to one space, and none
// at either end, cut at maxValueRunes.
func oneLine(s string) string {
	return cut(strings.Join(strings.FieldsFunc(s, lineBreak), " "), maxValueRunes)
}

// lineBreak is a rune that ends a line: LF, VT, FF, CR, NEL, and the line
// and paragraph separators.
func lineBreak(r rune) bool {
	switch r {
	case '\n', '\v', '\f', '\r', '\u0085', ' ', ' ':
		return true
	}
	return false
}

// cut is s when it has at most n runes, else its first n-1 runes and "…".
func cut(s string, n int) string {
	runes, keep := 0, 0
	for i := range s {
		if runes == n-1 {
			keep = i
		}
		if runes == n {
			return s[:keep] + "…"
		}
		runes++
	}
	return s
}
