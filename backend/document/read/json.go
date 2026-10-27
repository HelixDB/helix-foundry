package read

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// jsonFrame is one open object or array while a document is walked.
type jsonFrame struct {
	array bool
	// reachable reports a value whose path from the document goes through
	// objects only: the engine catalogs its arrays of objects.
	reachable bool
	// keyNext reports an object whose next token is a key.
	keyNext bool
	// items and objects count an array's items, and those that are objects.
	items, objects int64
}

// jsonWalk walks a JSON stream token by token: its nesting depth, the
// values at its top level, and its lists of records.
type jsonWalk struct {
	dec      *json.Decoder
	maxDepth int
	stack    []jsonFrame
	// lists and records count the arrays of objects reachable through
	// objects, and the objects they hold.
	lists, records int64
}

// errDepth is a stream nested past maxDepth.
var errDepth = errors.New("nested too deeply")

// value walks one top-level value whole, returning whether it is an object.
// It returns io.EOF when the stream holds no more, and io.ErrUnexpectedEOF
// when it ends inside a value: Token reports both as io.EOF.
func (w *jsonWalk) value() (object bool, err error) {
	for {
		token, err := w.dec.Token()
		if errors.Is(err, io.EOF) && len(w.stack) > 0 {
			return false, io.ErrUnexpectedEOF
		}
		if err != nil {
			return false, err
		}
		delim, isDelim := token.(json.Delim)
		if len(w.stack) > 0 {
			top := &w.stack[len(w.stack)-1]
			if !top.array && top.keyNext && !isDelim {
				top.keyNext = false // a key; its value follows
				continue
			}
		}
		if isDelim && (delim == '}' || delim == ']') {
			done := w.stack[len(w.stack)-1]
			w.stack = w.stack[:len(w.stack)-1]
			if done.array && done.reachable && done.items == done.objects {
				w.lists++
				w.records += done.objects
			}
			if len(w.stack) == 0 {
				return !done.array, nil
			}
			if top := &w.stack[len(w.stack)-1]; !top.array {
				top.keyNext = true
			}
			continue
		}
		// The start of a value: a scalar, or an object or array to walk.
		reachable := true
		if len(w.stack) > 0 {
			top := &w.stack[len(w.stack)-1]
			if top.array {
				top.items++
				if delim == '{' && isDelim {
					top.objects++
				}
				reachable = false
			} else {
				top.keyNext = !isDelim // a container's close sets it
				reachable = top.reachable
			}
		}
		if !isDelim {
			if len(w.stack) == 0 {
				return false, nil
			}
			continue
		}
		if len(w.stack) == w.maxDepth {
			return false, errDepth
		}
		w.stack = append(w.stack, jsonFrame{array: delim == '[', reachable: reachable, keyNext: delim == '{'})
	}
}

// newJSONWalk walks data, which must be UTF-8 text, as the engine's parser
// requires.
func newJSONWalk(data []byte, maxDepth int) (*jsonWalk, *model.FoundryFileUnreadable) {
	if !utf8.Valid(data) {
		bad := unreadable(model.FoundryFileReadUnsupported, "it is not UTF-8 text")
		return nil, &bad
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return &jsonWalk{dec: dec, maxDepth: maxDepth}, nil
}

// syntax is the outcome of a walk's error: past the depth, or not JSON.
func (w *jsonWalk) syntax(err error, where string) model.FoundryFileUnreadable {
	if errors.Is(err, errDepth) {
		return unreadable(model.FoundryFileReadTooLarge, "%sit is nested more than %d levels deep", where, w.maxDepth)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return unreadable(model.FoundryFileReadUnsupported, "%sit is not valid JSON: it ends before its last value does", where)
	}
	return unreadable(model.FoundryFileReadUnsupported, "%sit is not valid JSON at byte %d: %v", where, w.dec.InputOffset(), err)
}

// CheckJSON reads a JSON document as the engine catalogs it: one value,
// whose arrays of objects reached through objects (the document itself when
// it is one) are its lists of records, and returns the records they hold.
// A document with no list of records can't be loaded, and is unreadable.
func CheckJSON(data []byte, maxDepth int) (records int64, bad *model.FoundryFileUnreadable) {
	w, bad := newJSONWalk(data, maxDepth)
	if bad != nil {
		return 0, bad
	}
	if _, err := w.value(); err != nil {
		if errors.Is(err, io.EOF) {
			out := unreadable(model.FoundryFileReadUnsupported, "it is empty")
			return 0, &out
		}
		out := w.syntax(err, "")
		return 0, &out
	}
	switch _, err := w.dec.Token(); {
	case err == nil:
		out := unreadable(model.FoundryFileReadUnsupported, "it holds more than one JSON value; name a JSON Lines file .jsonl")
		return 0, &out
	case !errors.Is(err, io.EOF):
		out := w.syntax(err, "")
		return 0, &out
	}
	if w.lists == 0 {
		out := unreadable(model.FoundryFileReadUnsupported,
			"it holds no list of records: Helix loads the arrays of objects a JSON file holds, at its top or under its objects' keys")
		return 0, &out
	}
	return w.records, nil
}

// CheckJSONLines reads JSON Lines as the engine does: every value an
// object, each a record of at most maxRecord bytes, and returns how many.
func CheckJSONLines(data []byte, maxDepth, maxRecord int) (records int64, bad *model.FoundryFileUnreadable) {
	w, bad := newJSONWalk(data, maxDepth)
	if bad != nil {
		return 0, bad
	}
	for {
		start := w.dec.InputOffset()
		object, err := w.value()
		if errors.Is(err, io.EOF) {
			return records, nil
		}
		records++
		where := fmt.Sprintf("record %d: ", records)
		if err != nil {
			out := w.syntax(err, where)
			return 0, &out
		}
		if !object {
			out := unreadable(model.FoundryFileReadUnsupported, "%sit is not a JSON object", where)
			return 0, &out
		}
		if w.dec.InputOffset()-start > int64(maxRecord) {
			out := unreadable(model.FoundryFileReadTooLarge, "%sit is larger than %d KiB", where, maxRecord>>10)
			return 0, &out
		}
	}
}
