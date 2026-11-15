// Package dashboard is what a Foundry dashboard is: its spec, a set of tiles
// each showing one typed query of an ontology's load as a table or chart, laid
// out in rows; and its public snapshot, the tiles' outputs read once by the
// person who published it.
//
// A tile's query is the ontology agent's typed Query (read_request.json
// $defs/Query): the same query an Analyst answer's visual records, and the
// same `query` read Foundry's ReadFoundryGraph runs over one load with its
// version's rules applied. It only reads, its rows and groups are bounded by
// the contract (at most 200 rows, 100 groups), and every read runs over the
// one load it names (foundry_load), as the reader who runs it: in Foundry the
// person viewing the dashboard, and for a snapshot its publisher. ParseSpec
// checks a query against the contract's schema; whether it fits an
// ontology's types is the agent's to say when it is read.
//
// Nothing here talks to a store, a model or the network: the API, the MCP's
// operations and the snapshot all check and write specs through it.
package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	protoschemas "github.com/helixdb/helix-foundry/backend/pb/proto"
)

// SpecVersion is the version of the spec this package reads and writes.
const SpecVersion = 1

const (
	// MaxTiles is the most tiles a dashboard has: as many as one
	// ReadFoundryGraph call reads, so the tiles of one load are read at once.
	MaxTiles = ontologyagent.MaxReads
	// MaxTilesPerRow is the most tiles side by side.
	MaxTilesPerRow = 3
	// MaxTitleChars bounds a tile's title, as an Analyst visual's.
	MaxTitleChars = 80
	// MaxQueryBytes bounds one tile's query, written canonically: MaxTiles of
	// them, each wrapped as a read, fit one call's ontologyagent.MaxReadsBytes.
	MaxQueryBytes = 3 << 10
	// MaxSpecBytes bounds a spec as a caller sends it.
	MaxSpecBytes = 128 << 10
)

var (
	// tileIDPattern is a tile's id: short, lowercase, unique in its spec.
	tileIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	// idPattern is an ontology's or a load's id.
	idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// Spec is a dashboard's tiles and how they are laid out. Only ParseSpec and
// NewSpec make one, so every Spec holds.
type Spec struct {
	tiles  []Tile
	layout [][]string
}

// Tiles are the spec's tiles, in the order they were given.
func (s Spec) Tiles() []Tile { return slices.Clone(s.tiles) }

// Rows are the layout: each row the ids of its 1 to MaxTilesPerRow tiles,
// every tile in exactly one row.
func (s Spec) Rows() [][]string {
	out := make([][]string, len(s.layout))
	for i, row := range s.layout {
		out[i] = slices.Clone(row)
	}
	return out
}

// NewTargets are the tiles of s that read an ontology or load from's tile of
// the same id doesn't: tiles new to s, and tiles moved to another ontology
// or load. What a tile reads is checked when it is first given; a tile kept
// as it was, retitled or with a new query keeps reading what was checked,
// even once its pinned load is deleted, so it never blocks an edit of
// another tile.
//
//	before, _ := dashboard.ParseSpec(stored)
//	after, _ := dashboard.ParseSpec(edited)
//	for _, t := range after.NewTargets(before) { /* check t's ontology and load */ }
func (s Spec) NewTargets(from Spec) []Tile {
	return slices.DeleteFunc(s.Tiles(), func(t Tile) bool {
		return slices.ContainsFunc(from.tiles, func(f Tile) bool { return f.ID == t.ID && f.OntologyID == t.OntologyID && f.Load == t.Load })
	})
}

// Tile is one table or chart: a typed query of one ontology's load.
type Tile struct {
	ID         string
	Title      string
	OntologyID string
	Load       Load
	// Query is the typed Query, written canonically (keys sorted, numbers as
	// written).
	Query  json.RawMessage
	Visual VisualKind
}

// Load is the load a tile reads: a closed set.
type Load interface{ load() }

// CurrentLoad reads whichever load of the ontology is current when the tile
// is read.
type CurrentLoad struct{}

// PinnedLoad reads one load, whichever is current.
type PinnedLoad struct{ ID string }

func (CurrentLoad) load() {}
func (PinnedLoad) load()  {}

// VisualKind is how a tile shows its query's output: an Analyst visual's
// kind (ask_result.json VisualKind), a hint the site falls back from to a
// table when the output's shape doesn't suit it. VisualAuto is no hint: the
// shape decides.
type VisualKind string

const (
	VisualAuto   VisualKind = ""
	VisualTable  VisualKind = "table"
	VisualBar    VisualKind = "bar"
	VisualLine   VisualKind = "line"
	VisualFigure VisualKind = "figure"
)

// ParseVisualKind reads a visual kind, "" being VisualAuto.
func ParseVisualKind(raw string) (VisualKind, error) {
	switch k := VisualKind(raw); k {
	case VisualAuto, VisualTable, VisualBar, VisualLine, VisualFigure:
		return k, nil
	}
	return "", fmt.Errorf("visual kind is %q; it is table, bar, line or figure, or left out", raw)
}

// wireSpec is a spec as JSON:
//
//	{"v": 1,
//	 "tiles": [{"id": "t1", "title": "Invoices by status", "ontology_id": "…",
//	            "load": {"kind": "current"} | {"kind": "pinned", "load_id": "…"},
//	            "query": {…typed Query…}, "visual": {"kind": "bar"}}],
//	 "layout": {"rows": [["t1", "t2"], ["t3"]]}}
type wireSpec struct {
	V      int        `json:"v"`
	Tiles  []wireTile `json:"tiles"`
	Layout wireLayout `json:"layout"`
}

type wireTile struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	OntologyID string          `json:"ontology_id"`
	Load       wireLoad        `json:"load"`
	Query      json.RawMessage `json:"query"`
	Visual     wireVisual      `json:"visual"`
}

type wireLoad struct {
	Kind   string `json:"kind"`
	LoadID string `json:"load_id,omitempty"`
}

type wireVisual struct {
	Kind string `json:"kind,omitempty"`
}

type wireLayout struct {
	Rows [][]string `json:"rows"`
}

// ParseSpec reads a spec from JSON, refusing anything but the shape above:
// an unknown field, a version other than SpecVersion, more than MaxTiles
// tiles, a tile id that isn't unique or doesn't fit, a title of more than
// MaxTitleChars characters or with control characters, an ontology or load
// id that isn't one, a query that isn't the contract's Query or is longer
// than MaxQueryBytes, an unknown visual kind, or a layout that doesn't place
// every tile exactly once in rows of 1 to MaxTilesPerRow. Its errors say
// what is wrong for whoever wrote the spec: a person or an agent.
//
//	spec, err := dashboard.ParseSpec([]byte(`{"v": 1, "tiles": [], "layout": {"rows": []}}`))
func ParseSpec(raw []byte) (Spec, error) {
	if len(raw) > MaxSpecBytes {
		return Spec{}, fmt.Errorf("a spec is at most %d KiB", MaxSpecBytes>>10)
	}
	var w wireSpec
	if err := decodeStrict(raw, &w); err != nil {
		return Spec{}, fmt.Errorf("the spec is not {v, tiles, layout}: %w", err)
	}
	if w.V != SpecVersion {
		return Spec{}, fmt.Errorf("the spec's v is %d; this Foundry reads v %d", w.V, SpecVersion)
	}
	tiles := make([]Tile, 0, len(w.Tiles))
	for i, wt := range w.Tiles {
		t, err := parseTile(wt)
		if err != nil {
			return Spec{}, fmt.Errorf("tile %d: %w", i, err)
		}
		tiles = append(tiles, t)
	}
	return NewSpec(tiles, w.Layout.Rows)
}

// ParseTile reads one tile from JSON, as a tile of a spec.
func ParseTile(raw []byte) (Tile, error) {
	if len(raw) > MaxSpecBytes {
		return Tile{}, fmt.Errorf("a tile is at most %d KiB", MaxSpecBytes>>10)
	}
	var wt wireTile
	if err := decodeStrict(raw, &wt); err != nil {
		return Tile{}, fmt.Errorf("the tile is not {id, title, ontology_id, load, query, visual}: %w", err)
	}
	return parseTile(wt)
}

func parseTile(wt wireTile) (Tile, error) {
	t := Tile{ID: wt.ID, Title: strings.TrimSpace(wt.Title), OntologyID: wt.OntologyID}
	switch {
	case !tileIDPattern.MatchString(t.ID):
		return Tile{}, fmt.Errorf("id %q must be 1 to 32 lowercase letters, digits, dashes or underscores, starting with a letter or digit", t.ID)
	case t.Title == "" || utf8.RuneCountInString(t.Title) > MaxTitleChars || strings.ContainsFunc(t.Title, unicode.IsControl):
		return Tile{}, fmt.Errorf("tile %s's title must be 1 to %d characters, with no control characters", t.ID, MaxTitleChars)
	case !idPattern.MatchString(t.OntologyID):
		return Tile{}, fmt.Errorf("tile %s's ontology_id is not an id", t.ID)
	}
	switch wt.Load.Kind {
	case "current":
		if wt.Load.LoadID != "" {
			return Tile{}, fmt.Errorf("tile %s's load is {kind: current} with no load_id", t.ID)
		}
		t.Load = CurrentLoad{}
	case "pinned":
		if !idPattern.MatchString(wt.Load.LoadID) {
			return Tile{}, fmt.Errorf("tile %s's pinned load needs its load_id", t.ID)
		}
		t.Load = PinnedLoad{ID: wt.Load.LoadID}
	default:
		return Tile{}, fmt.Errorf("tile %s's load is {kind: current} or {kind: pinned, load_id}", t.ID)
	}
	query, err := CanonicalQuery(wt.Query)
	if err != nil {
		return Tile{}, fmt.Errorf("tile %s's query: %w", t.ID, err)
	}
	t.Query = query
	if t.Visual, err = ParseVisualKind(wt.Visual.Kind); err != nil {
		return Tile{}, fmt.Errorf("tile %s's %w", t.ID, err)
	}
	return t, nil
}

// NewSpec makes a spec of tiles, each already checked (ParseTile, or a spec's
// Tiles), laid out in rows: every tile in exactly one row of 1 to
// MaxTilesPerRow, no more than MaxTiles tiles, ids unique.
func NewSpec(tiles []Tile, rows [][]string) (Spec, error) {
	if len(tiles) > MaxTiles {
		return Spec{}, fmt.Errorf("a dashboard has at most %d tiles, not %d", MaxTiles, len(tiles))
	}
	placed := make(map[string]bool, len(tiles))
	for _, t := range tiles {
		if _, ok := placed[t.ID]; ok {
			return Spec{}, fmt.Errorf("two tiles have the id %s", t.ID)
		}
		placed[t.ID] = false
	}
	for i, row := range rows {
		if len(row) == 0 || len(row) > MaxTilesPerRow {
			return Spec{}, fmt.Errorf("layout row %d has %d tiles; a row has 1 to %d", i, len(row), MaxTilesPerRow)
		}
		for _, id := range row {
			done, known := placed[id]
			switch {
			case !known:
				return Spec{}, fmt.Errorf("layout row %d names %q, which is no tile", i, id)
			case done:
				return Spec{}, fmt.Errorf("layout places tile %s twice", id)
			}
			placed[id] = true
		}
	}
	for _, t := range tiles {
		if !placed[t.ID] {
			return Spec{}, fmt.Errorf("layout leaves out tile %s: each tile is in exactly one row", t.ID)
		}
	}
	s := Spec{tiles: slices.Clone(tiles), layout: make([][]string, len(rows))}
	for i, row := range rows {
		s.layout[i] = slices.Clone(row)
	}
	return s, nil
}

// MarshalJSON writes the spec canonically: the same spec, the same bytes.
func (s Spec) MarshalJSON() ([]byte, error) {
	w := wireSpec{V: SpecVersion, Tiles: make([]wireTile, 0, len(s.tiles)), Layout: wireLayout{Rows: s.Rows()}}
	for _, t := range s.tiles {
		w.Tiles = append(w.Tiles, t.wire())
	}
	return json.Marshal(w)
}

// MarshalJSON writes the tile as a spec writes it.
func (t Tile) MarshalJSON() ([]byte, error) { return json.Marshal(t.wire()) }

func (t Tile) wire() wireTile {
	out := wireTile{ID: t.ID, Title: t.Title, OntologyID: t.OntologyID, Query: t.Query, Visual: wireVisual{Kind: string(t.Visual)}}
	switch l := t.Load.(type) {
	case CurrentLoad:
		out.Load = wireLoad{Kind: "current"}
	case PinnedLoad:
		out.Load = wireLoad{Kind: "pinned", LoadID: l.ID}
	}
	return out
}

// Read is the tile's query as one read of ReadFoundryGraph's reads:
// {"query": …}.
func (t Tile) Read() json.RawMessage {
	out := make([]byte, 0, len(t.Query)+10)
	out = append(out, `{"query":`...)
	out = append(out, t.Query...)
	return append(out, '}')
}

// Reads are tiles' queries as ReadFoundryGraph's reads_json, in order: at
// most MaxTiles of them, which fit one call.
func Reads(tiles []Tile) string {
	parts := make([][]byte, 0, len(tiles))
	for _, t := range tiles {
		parts = append(parts, t.Read())
	}
	return "[" + string(bytes.Join(parts, []byte{','})) + "]"
}

// CanonicalQuery checks raw is the contract's Query (read_request.json
// $defs/Query) and writes it canonically: keys sorted, numbers exactly as
// written, at most MaxQueryBytes.
func CanonicalQuery(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return nil, errors.New("a query is a JSON object: the typed Query of Foundry's graph reads")
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return nil, fmt.Errorf("a query is a JSON object: %w", err)
	}
	schema, err := querySchema()
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(instance); err != nil {
		return nil, fmt.Errorf("not a Query of the read contract (helix_foundry_read_guide gives its schema): %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var exact any
	if err := decoder.Decode(&exact); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(exact)
	if err != nil {
		return nil, err
	}
	if len(canonical) > MaxQueryBytes {
		return nil, fmt.Errorf("a query is at most %d bytes, not %d", MaxQueryBytes, len(canonical))
	}
	return canonical, nil
}

// querySchema is read_request.json's Query, resolved once.
var querySchema = sync.OnceValues(func() (*jsonschema.Resolved, error) {
	var contract struct {
		Schema string                        `json:"$schema"`
		Defs   map[string]*jsonschema.Schema `json:"$defs"`
	}
	if err := json.Unmarshal(protoschemas.OntologyAgentReadRequest, &contract); err != nil {
		return nil, fmt.Errorf("read the read contract: %w", err)
	}
	if contract.Defs["Query"] == nil {
		return nil, errors.New("the read contract has no Query")
	}
	root := &jsonschema.Schema{Schema: contract.Schema, Ref: "#/$defs/Query", Defs: contract.Defs}
	return root.Resolve(nil)
})

// decodeStrict reads raw into v, refusing unknown fields and anything after
// the one value.
func decodeStrict(raw []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}
