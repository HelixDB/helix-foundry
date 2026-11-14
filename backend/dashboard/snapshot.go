package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// A snapshot is what a published dashboard shows anyone with its link: the
// spec's tiles with the outputs its publisher's reads gave, and nothing that
// names the workspace, its ontologies, loads or people. It is written once,
// at publish (and each republish), and served as it was written.

const (
	// MaxOutputBytes bounds one tile's output as a snapshot keeps it.
	MaxOutputBytes = 512 << 10
	// MaxSnapshotBytes bounds a whole snapshot.
	MaxSnapshotBytes = 2 << 20
	// maxStringChars is the most characters of any string an output keeps,
	// as an Analyst answer's recorded outputs keep: longer ones are cut with
	// "…".
	maxStringChars = 1000
)

// outputKinds are the kinds of a query's answer (read_result.json Answer2).
var outputKinds = map[string]bool{"rows": true, "count": true, "group_count": true, "aggregate": true, "grouped_aggregate": true}

// ErrTooLarge is a snapshot, or one of its tile's outputs, past its bound.
var ErrTooLarge = errors.New("dashboard: snapshot too large")

// Snapshot is a published dashboard as its link shows it (the JSON the public
// route serves):
//
//	{"v": 1, "name": "…", "description": "…", "published_at": "2026-11-14T09:00:00Z",
//	 "layout": {"rows": [["t1"]]},
//	 "tiles": [{"id": "t1", "title": "…", "visual": {"kind": "bar"}, "query": {…},
//	            "as_of": "2026-11-14", "output": {"kind": "group_count", …}}]}
type Snapshot struct {
	V           int            `json:"v"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	PublishedAt string         `json:"published_at"`
	Layout      wireLayout     `json:"layout"`
	Tiles       []SnapshotTile `json:"tiles"`
}

// SnapshotTile is one tile with its output.
type SnapshotTile struct {
	ID     string          `json:"id"`
	Title  string          `json:"title"`
	Visual wireVisual      `json:"visual"`
	Query  json.RawMessage `json:"query"`
	// AsOf is the day the load's rules were read as of.
	AsOf   string          `json:"as_of"`
	Output json.RawMessage `json:"output"`
}

// Read is one tile's read at publish: the day it was read as of, and its
// query's answer (read_result.json's ok).
type Read struct {
	AsOf   string
	Answer json.RawMessage
}

// NewSnapshot is spec's snapshot, from reads, one per tile by its id: each
// answer checked to be a query's and made public (Public). It is
// ErrTooLarge past MaxOutputBytes for a tile or MaxSnapshotBytes in all.
func NewSnapshot(name, description string, spec Spec, reads map[string]Read, at time.Time) ([]byte, error) {
	s := Snapshot{V: SpecVersion, Name: name, Description: description, PublishedAt: at.UTC().Format(time.RFC3339),
		Layout: wireLayout{Rows: spec.Rows()}, Tiles: make([]SnapshotTile, 0, len(spec.tiles))}
	for _, t := range spec.tiles {
		read, ok := reads[t.ID]
		if !ok {
			return nil, fmt.Errorf("tile %s has no read", t.ID)
		}
		output, err := Public(read.Answer)
		if err != nil {
			return nil, fmt.Errorf("tile %s: %w", t.ID, err)
		}
		if len(output) > MaxOutputBytes {
			return nil, fmt.Errorf("%w: tile %s's output is %d KiB, past %d KiB: ask for fewer rows or columns", ErrTooLarge, t.ID, len(output)>>10, MaxOutputBytes>>10)
		}
		s.Tiles = append(s.Tiles, SnapshotTile{ID: t.ID, Title: t.Title, Visual: wireVisual{Kind: string(t.Visual)}, Query: t.Query,
			AsOf: read.AsOf, Output: output})
	}
	out, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if len(out) > MaxSnapshotBytes {
		return nil, fmt.Errorf("%w: the snapshot is %d KiB, past %d KiB: show fewer rows", ErrTooLarge, len(out)>>10, MaxSnapshotBytes>>10)
	}
	return out, nil
}

// Public is a query's answer as anyone may see it: of one of the answer
// kinds, every string cut to maxStringChars characters, each row's entities
// by type and name only (their stored identity left out), and no row's edges.
// Numbers stay exactly as the engine wrote them.
func Public(answer json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(answer))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil || out == nil {
		return nil, errors.New("the read's answer is not a JSON object")
	}
	kind, _ := out["kind"].(string)
	if !outputKinds[kind] {
		return nil, fmt.Errorf("the read's answer is of kind %q, not a query's", kind)
	}
	if kind == "rows" {
		rows, ok := out["rows"].([]any)
		if !ok {
			return nil, errors.New("the read's rows are not a list")
		}
		for i, r := range rows {
			row, ok := r.(map[string]any)
			cells, cellsOK := row["cells"].([]any)
			if !ok || !cellsOK {
				return nil, errors.New("a row of the read has no cells")
			}
			for j, c := range cells {
				if e, ok := c.(map[string]any); ok && entityCell(e) {
					cells[j] = map[string]any{"type": e["type"], "name": e["name"]}
				}
			}
			rows[i] = map[string]any{"cells": cells}
		}
	}
	return json.Marshal(cut(out))
}

// entityCell reports whether a cell is an entity (read_result.json
// EntityCell): exactly its type, id and name, all strings.
func entityCell(c map[string]any) bool {
	if len(c) != 3 {
		return false
	}
	for _, k := range []string{"type", "id", "name"} {
		if _, ok := c[k].(string); !ok {
			return false
		}
	}
	return true
}

// cut is v with every string cut to maxStringChars characters.
func cut(v any) any {
	switch x := v.(type) {
	case string:
		if utf8.RuneCountInString(x) <= maxStringChars {
			return x
		}
		return string([]rune(x)[:maxStringChars-1]) + "…"
	case []any:
		for i := range x {
			x[i] = cut(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = cut(x[k])
		}
	}
	return v
}
