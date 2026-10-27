package filesource

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
)

// jsonShape is how a JSON file holds its records.
type jsonShape uint8

const (
	// shapeLines is JSON Lines, or objects one after another; an empty
	// file is lines with none.
	shapeLines jsonShape = iota
	// shapeArray is one top-level array of objects.
	shapeArray
	// shapeEnvelope is one object holding the records in an array under key,
	// as APIs answer: {"object":"list","data":[...]}.
	shapeEnvelope
	// shapeSingle is one object that is the only record.
	shapeSingle
)

type jsonLayout struct {
	shape jsonShape
	key   string
}

// field is one key of a record, in file order; raw is valid until the next record.
type field struct {
	key string
	raw json.RawMessage
}

// spaceChunk is how much leading text skipSpace reads at a time.
const spaceChunk = 4 << 10

// skipSpace finds the first non-space byte of the text br reads. It returns
// a reader starting at that byte and its offset in br, or first == 0 for
// text that is only space.
func skipSpace(br *budgetReader) (first byte, r io.Reader, base int64, err error) {
	buf := make([]byte, spaceChunk)
	for {
		n, err := br.Read(buf)
		if i := slices.IndexFunc(buf[:n], func(c byte) bool { return !isSpace(c) }); i >= 0 {
			return buf[i], io.MultiReader(bytes.NewReader(buf[i:n]), br), br.read - int64(n-i), nil
		}
		if errors.Is(err, io.EOF) {
			return 0, nil, br.read, nil
		}
		if err != nil {
			return 0, nil, 0, err
		}
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// jsonRecords streams the records of one JSON text.
type jsonRecords struct {
	dec    *json.Decoder
	br     *budgetReader
	base   int64
	layout jsonLayout
	// maxRecord is the largest record, in bytes, from { to }.
	maxRecord int64
	// cut is a sample's prefix of a file: input ending early ends the records.
	cut bool
	// truncated is set when cut input ended the records.
	truncated bool
	state     recordState
	// record counts the records read so far.
	record int64
	fields []field
	index  map[string]int
}

type recordState uint8

const (
	stateStart recordState = iota
	stateArray
	stateLines
	stateTrailing
	stateDone
)

// newJSONRecords reads the records br holds in layout.
func newJSONRecords(br *budgetReader, layout jsonLayout, maxRecord int, cut bool) (*jsonRecords, error) {
	j := &jsonRecords{br: br, layout: layout, maxRecord: int64(maxRecord), cut: cut, index: map[string]int{}}
	first, r, base, err := skipSpace(br)
	if err != nil {
		return nil, j.fail(err)
	}
	if first == 0 {
		j.state = stateDone
		return j, nil
	}
	j.dec, j.base = json.NewDecoder(r), base
	j.dec.UseNumber()
	return j, nil
}

// next reads the next record into fields; false is the end of the records.
func (j *jsonRecords) next() (bool, error) {
	for {
		var err error
		switch j.state {
		case stateDone:
			return false, nil
		case stateStart:
			err = j.start()
		case stateArray:
			if j.dec.More() {
				return j.object()
			}
			if err = j.expect(']'); err == nil && j.layout.shape == shapeEnvelope {
				err = j.seekKey()
			} else if err == nil {
				j.state = stateTrailing
			}
		case stateLines:
			if j.dec.More() && (j.layout.shape == shapeLines || j.record == 0) {
				return j.object()
			}
			j.state = stateTrailing
		case stateTrailing:
			if _, err = j.dec.Token(); errors.Is(err, io.EOF) {
				j.state = stateDone
				return false, nil
			}
			if err == nil {
				err = because(ErrMalformed, "more data after the records")
			}
		}
		if err != nil {
			if j.ended(err) {
				return false, nil
			}
			return false, j.fail(err)
		}
	}
}

func (j *jsonRecords) start() error {
	switch j.layout.shape {
	case shapeArray:
		if err := j.expect('['); err != nil {
			return err
		}
		j.state = stateArray
		return nil
	case shapeEnvelope:
		if err := j.expect('{'); err != nil {
			return err
		}
		return j.seekKey()
	}
	j.state = stateLines
	return nil
}

// seekKey moves through an envelope to the next array under the records
// key, skipping every other value.
func (j *jsonRecords) seekKey() error {
	for j.dec.More() {
		t, err := j.dec.Token()
		if err != nil {
			return err
		}
		if t == j.layout.key {
			v, err := j.dec.Token()
			if err != nil {
				return err
			}
			if v == json.Delim('[') {
				j.state = stateArray
				return nil
			}
			if err := skipValue(j.dec, v); err != nil {
				return err
			}
			continue
		}
		var skip json.RawMessage
		if err := j.dec.Decode(&skip); err != nil {
			return err
		}
	}
	if err := j.expect('}'); err != nil {
		return err
	}
	j.state = stateTrailing
	return nil
}

func (j *jsonRecords) expect(d json.Delim) error {
	t, err := j.dec.Token()
	if err != nil {
		return err
	}
	if t != d {
		return because(ErrMalformed, "malformed JSON: expected %q", string(d))
	}
	return nil
}

// object reads one record into fields. A key seen twice keeps its first
// position and its last value.
func (j *jsonRecords) object() (bool, error) {
	fail := func(err error) (bool, error) {
		if j.ended(err) {
			return false, nil
		}
		return false, j.fail(err)
	}
	t, err := j.dec.Token()
	if err != nil {
		return fail(err)
	}
	if t != json.Delim('{') {
		return fail(ErrNotObject)
	}
	open := j.dec.InputOffset() - 1
	j.fields = j.fields[:0]
	clear(j.index)
	for j.dec.More() {
		t, err := j.dec.Token()
		if err != nil {
			return fail(err)
		}
		key, _ := t.(string) // inside an object the decoder only yields keys here
		i, dup := j.index[key]
		if !dup {
			i = len(j.fields)
			j.index[key] = i
			if i < cap(j.fields) {
				j.fields = j.fields[:i+1]
				j.fields[i].key = key
			} else {
				j.fields = append(j.fields, field{key: key})
			}
		}
		f := &j.fields[i]
		f.raw = f.raw[:0]
		if err := j.dec.Decode(&f.raw); err != nil {
			return fail(err)
		}
	}
	if err := j.expect('}'); err != nil {
		return fail(err)
	}
	end := j.dec.InputOffset()
	if end-open > j.maxRecord {
		return fail(because(ErrRecordTooLarge, "a record over %s", sizeText(j.maxRecord)))
	}
	j.br.mark(j.base + end)
	j.record++
	return true, nil
}

// ended reports cut input running out, which ends a sample cleanly.
func (j *jsonRecords) ended(err error) bool {
	if !j.cut {
		return false
	}
	if errors.Is(err, errCut) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(j.br.cause(), errCut) {
		j.truncated = true
		j.state = stateDone
		return true
	}
	return false
}

// fail gives err its place: the record being read.
func (j *jsonRecords) fail(err error) error {
	j.state = stateDone
	return recordError(jsonError(err, j.br), j.record+1)
}

// recordError marks unreadable data with its record. Other errors stay bare.
func recordError(err error, record int64) error {
	if !isUnreadable(err) {
		return err
	}
	return &Error{Record: record, Err: err}
}

// jsonError turns a decoder error into this package's kinds. The budget
// reader's own error wins: the decoder only passes it on.
func jsonError(err error, br *budgetReader) error {
	if c := br.cause(); c != nil {
		return c
	}
	var syntax *json.SyntaxError
	var r *reason
	switch {
	case errors.As(err, &r), isContextError(err):
		return err
	case errors.Is(err, ErrNotObject):
		return err
	case errors.As(err, &syntax):
		return &reason{kind: ErrMalformed, msg: "malformed JSON: " + syntax.Error(), cause: err}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return because(ErrMalformed, "the file ends inside a record")
	}
	return &reason{kind: ErrMalformed, msg: "malformed JSON", cause: err}
}

// skipValue consumes the rest of a value whose first token was t.
func skipValue(dec *json.Decoder, t json.Token) error {
	d, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	for dec.More() {
		if d == '{' {
			if _, err := dec.Token(); err != nil {
				return err
			}
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

// detection is what the start of a JSON text says about its shape.
type detection struct {
	first byte
	// lines is a JSON Lines file (by its name) starting with an object:
	// each object is a row, so its first is not read for records.
	lines bool
	// candidates are the top-level keys of a first object whose value is an
	// array starting with an object, in file order.
	candidates []string
	// more is another value after the first object.
	more bool
	// complete is the first value read to its end.
	complete bool
	// elements counts objects read inside candidate arrays.
	elements int64
}

// detectJSON reads far enough into a JSON text to choose its shape. For a
// text starting with "{" that is the whole first value: its candidate
// arrays may be all of the file, so each element ends a record. lines is a
// JSON Lines file, whose objects are rows however many it holds.
func detectJSON(br *budgetReader, maxRecord int, cut, lines bool) (det detection, err error) {
	first, r, base, err := skipSpace(br)
	if err != nil || first != '{' || lines {
		return detection{first: first, lines: lines && first == '{'}, err
	}
	det.first = first
	dec := json.NewDecoder(r)
	dec.UseNumber()
	j := &jsonRecords{dec: dec, br: br, base: base, maxRecord: int64(maxRecord), cut: cut}
	err = det.walk(j)
	if err != nil && j.ended(err) {
		return det, nil
	}
	return det, err
}

func (det *detection) walk(j *jsonRecords) error {
	dec := j.dec
	if _, err := dec.Token(); err != nil {
		return err
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := t.(string) // inside an object the decoder only yields keys here
		v, err := dec.Token()
		if err != nil {
			return err
		}
		if v != json.Delim('[') {
			if err := skipValue(dec, v); err != nil {
				return err
			}
			continue
		}
		var raw json.RawMessage
		for i := 0; dec.More(); i++ {
			raw = raw[:0]
			if err := dec.Decode(&raw); err != nil {
				return err
			}
			if i == 0 && raw[0] == '{' && !slices.Contains(det.candidates, key) {
				det.candidates = append(det.candidates, key)
			}
			if slices.Contains(det.candidates, key) {
				det.elements++
				j.br.mark(j.base + dec.InputOffset())
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	det.complete = true
	det.more = dec.More()
	return nil
}

// layout chooses the shape a detection found. A cut first object (a sample
// of a big envelope) reads its first list of records, else as lines.
func (det detection) layout() (jsonLayout, error) {
	switch {
	case det.first == 0:
		return jsonLayout{shape: shapeLines}, nil
	case det.first == '[':
		return jsonLayout{shape: shapeArray}, nil
	case det.first != '{':
		return jsonLayout{}, because(ErrMalformed, "not JSON: a JSON file holds an array or objects")
	case det.lines:
		return jsonLayout{shape: shapeLines}, nil
	case !det.complete && len(det.candidates) > 0:
		return jsonLayout{shape: shapeEnvelope, key: det.candidates[0]}, nil
	case !det.complete, det.more:
		return jsonLayout{shape: shapeLines}, nil
	case len(det.candidates) == 1:
		return jsonLayout{shape: shapeEnvelope, key: det.candidates[0]}, nil
	case len(det.candidates) > 1:
		return jsonLayout{}, because(ErrAmbiguousJSON, "several lists of records (%s); Helix reads one list per file", strings.Join(det.candidates, ", "))
	}
	return jsonLayout{shape: shapeSingle}, nil
}

// jsonType is the type of one raw JSON value.
func jsonType(raw []byte) Type {
	switch raw[0] {
	case 'n':
		return TypeNull
	case 't', 'f':
		return TypeBoolean
	case '"':
		return TypeString
	case '{', '[':
		return TypeJSON
	}
	if bytes.ContainsAny(raw, ".eE") {
		return TypeFloat
	}
	if _, err := strconv.ParseInt(string(raw), 10, 64); err != nil {
		return TypeFloat
	}
	return TypeInteger
}
