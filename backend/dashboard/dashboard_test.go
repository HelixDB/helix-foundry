package dashboard_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/dashboard"
)

const (
	countQuery = `{"start": {"type": "Invoice"}, "return": {"count": {}}}`
	groupQuery = `{"start":{"type":"Invoice"},"return":{"group_count":{"at":0,"prop":"status","limit":10}}}`
)

func tileJSON(id, query string) string {
	return fmt.Sprintf(`{"id": %q, "title": "Invoices", "ontology_id": "01ONTOLOGY", "load": {"kind": "current"}, "query": %s, "visual": {"kind": "bar"}}`, id, query)
}

func specJSON(tiles []string, rows string) string {
	return fmt.Sprintf(`{"v": 1, "tiles": [%s], "layout": {"rows": %s}}`, strings.Join(tiles, ","), rows)
}

func TestParseSpecRoundTrips(t *testing.T) {
	raw := specJSON([]string{tileJSON("t1", countQuery),
		`{"id":"t2","title":" By status ","ontology_id":"01ONTOLOGY","load":{"kind":"pinned","load_id":"01LOAD"},"query":` + groupQuery + `}`},
		`[["t1", "t2"]]`)
	spec, err := dashboard.ParseSpec([]byte(raw))
	require.NoError(t, err)
	tiles := spec.Tiles()
	require.Len(t, tiles, 2)
	require.Equal(t, dashboard.CurrentLoad{}, tiles[0].Load)
	require.Equal(t, dashboard.PinnedLoad{ID: "01LOAD"}, tiles[1].Load)
	require.Equal(t, "By status", tiles[1].Title, "titles are trimmed")
	require.Equal(t, dashboard.VisualBar, tiles[0].Visual)
	require.Equal(t, dashboard.VisualAuto, tiles[1].Visual)
	require.JSONEq(t, `{"return":{"count":{}},"start":{"type":"Invoice"}}`, string(tiles[0].Query))
	require.Equal(t, [][]string{{"t1", "t2"}}, spec.Rows())

	canonical, err := json.Marshal(spec)
	require.NoError(t, err)
	again, err := dashboard.ParseSpec(canonical)
	require.NoError(t, err)
	twice, err := json.Marshal(again)
	require.NoError(t, err)
	require.Equal(t, string(canonical), string(twice), "the canonical form is stable")
	require.Contains(t, string(canonical), `"load":{"kind":"pinned","load_id":"01LOAD"}`)
	require.Contains(t, string(canonical), `"visual":{}`)

	// What a caller gets can't change the spec.
	tiles[0].Title = "changed"
	spec.Rows()[0][0] = "changed"
	require.Equal(t, "Invoices", spec.Tiles()[0].Title)
	require.Equal(t, "t1", spec.Rows()[0][0])
}

func TestParseSpecKeepsNumbersExact(t *testing.T) {
	query := `{"start":{"type":"Invoice","where":[{"prop":"amount","op":"=","value":90071992547409931}]},"return":{"count":{}}}`
	spec, err := dashboard.ParseSpec([]byte(specJSON([]string{tileJSON("t1", query)}, `[["t1"]]`)))
	require.NoError(t, err, "a where condition with a large integer")
	require.Contains(t, string(spec.Tiles()[0].Query), "90071992547409931")
}

func TestParseSpecEmpty(t *testing.T) {
	spec, err := dashboard.ParseSpec([]byte(`{"v":1,"tiles":[],"layout":{"rows":[]}}`))
	require.NoError(t, err)
	require.Empty(t, spec.Tiles())
	out, err := json.Marshal(spec)
	require.NoError(t, err)
	require.JSONEq(t, `{"v":1,"tiles":[],"layout":{"rows":[]}}`, string(out))
}

func TestParseSpecRefuses(t *testing.T) {
	tile := tileJSON("t1", countQuery)
	many := make([]string, dashboard.MaxTiles+1)
	rows := make([]string, len(many))
	for i := range many {
		id := fmt.Sprintf("t%d", i)
		many[i], rows[i] = tileJSON(id, countQuery), fmt.Sprintf(`[%q]`, id)
	}
	cases := map[string]struct{ raw, want string }{
		"not JSON":        {`{`, "not {v, tiles, layout}"},
		"unknown field":   {`{"v":1,"tiles":[],"layout":{"rows":[]},"x":1}`, "not {v, tiles, layout}"},
		"two values":      {`{"v":1,"tiles":[],"layout":{"rows":[]}} {}`, "more than one"},
		"version":         {`{"v":2,"tiles":[],"layout":{"rows":[]}}`, "this Foundry reads v 1"},
		"too many tiles":  {specJSON(many, "["+strings.Join(rows, ",")+"]"), "at most 20 tiles"},
		"too large":       {strings.Repeat(" ", dashboard.MaxSpecBytes+1), "at most 128 KiB"},
		"tile id":         {specJSON([]string{tileJSON("T 1", countQuery)}, `[["T 1"]]`), "lowercase"},
		"duplicate id":    {specJSON([]string{tile, tile}, `[["t1"]]`), "two tiles have the id t1"},
		"empty title":     {specJSON([]string{strings.Replace(tile, `"Invoices"`, `"  "`, 1)}, `[["t1"]]`), "title"},
		"long title":      {specJSON([]string{strings.Replace(tile, `"Invoices"`, `"`+strings.Repeat("a", 81)+`"`, 1)}, `[["t1"]]`), "title"},
		"control title":   {specJSON([]string{strings.Replace(tile, `"Invoices"`, `"a\u0007b"`, 1)}, `[["t1"]]`), "title"},
		"ontology id":     {specJSON([]string{strings.Replace(tile, "01ONTOLOGY", "a/b", 1)}, `[["t1"]]`), "ontology_id"},
		"load kind":       {specJSON([]string{strings.Replace(tile, `"current"`, `"latest"`, 1)}, `[["t1"]]`), "{kind: current}"},
		"current with id": {specJSON([]string{strings.Replace(tile, `{"kind": "current"}`, `{"kind":"current","load_id":"x"}`, 1)}, `[["t1"]]`), "no load_id"},
		"pinned no id":    {specJSON([]string{strings.Replace(tile, `{"kind": "current"}`, `{"kind":"pinned"}`, 1)}, `[["t1"]]`), "needs its load_id"},
		"query missing":   {specJSON([]string{strings.Replace(tile, countQuery, `null`, 1)}, `[["t1"]]`), "JSON object"},
		"query array":     {specJSON([]string{tileJSON("t1", `[]`)}, `[["t1"]]`), "JSON object"},
		"query shape":     {specJSON([]string{tileJSON("t1", `{"start":{"type":"Invoice"}}`)}, `[["t1"]]`), "not a Query"},
		"query field":     {specJSON([]string{tileJSON("t1", `{"start":{"type":"Invoice"},"return":{"count":{}},"drop":1}`)}, `[["t1"]]`), "not a Query"},
		"query too long": {specJSON([]string{tileJSON("t1", `{"start":{"type":"`+strings.Repeat("a", dashboard.MaxQueryBytes)+`"},"return":{"count":{}}}`)}, `[["t1"]]`),
			"at most 3072 bytes"},
		"visual":       {specJSON([]string{strings.Replace(tile, `"bar"`, `"pie"`, 1)}, `[["t1"]]`), "table, bar, line or figure"},
		"empty row":    {specJSON([]string{tile}, `[["t1"], []]`), "a row has 1 to 3"},
		"wide row":     {specJSON([]string{tile}, `[["t1","t1","t1","t1"]]`), "a row has 1 to 3"},
		"unknown tile": {specJSON([]string{tile}, `[["t1","t9"]]`), "which is no tile"},
		"placed twice": {specJSON([]string{tile}, `[["t1"],["t1"]]`), "twice"},
		"left out":     {specJSON([]string{tile}, `[]`), "leaves out tile t1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := dashboard.ParseSpec([]byte(c.raw))
			require.ErrorContains(t, err, c.want)
		})
	}
}

func TestParseTile(t *testing.T) {
	tile, err := dashboard.ParseTile([]byte(tileJSON("t1", groupQuery)))
	require.NoError(t, err)
	require.Equal(t, "t1", tile.ID)
	raw, err := json.Marshal(tile)
	require.NoError(t, err)
	again, err := dashboard.ParseTile(raw)
	require.NoError(t, err)
	require.Equal(t, tile, again)
	require.JSONEq(t, `{"query":`+string(tile.Query)+`}`, string(tile.Read()))

	_, err = dashboard.ParseTile([]byte(`{"id":"t1","extra":1}`))
	require.ErrorContains(t, err, "not {id, title")
	_, err = dashboard.ParseTile([]byte(strings.Repeat(" ", dashboard.MaxSpecBytes+1)))
	require.ErrorContains(t, err, "at most")
}

func TestReadsFitOneCall(t *testing.T) {
	tiles := make([]dashboard.Tile, 0, dashboard.MaxTiles)
	long := `{"start":{"type":"` + strings.Repeat("a", dashboard.MaxQueryBytes-60) + `"},"return":{"count":{}}}`
	for i := range dashboard.MaxTiles {
		tile, err := dashboard.ParseTile([]byte(tileJSON(fmt.Sprintf("t%d", i), long)))
		require.NoError(t, err)
		tiles = append(tiles, tile)
	}
	reads := dashboard.Reads(tiles)
	require.Less(t, len(reads), 64<<10, "the largest tiles' reads fit one ReadFoundryGraph call")
	var parsed []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(reads), &parsed))
	require.Len(t, parsed, dashboard.MaxTiles)
	require.Equal(t, "[]", dashboard.Reads(nil))
}

func TestNewSpec(t *testing.T) {
	tile, err := dashboard.ParseTile([]byte(tileJSON("t1", countQuery)))
	require.NoError(t, err)
	spec, err := dashboard.NewSpec([]dashboard.Tile{tile}, [][]string{{"t1"}})
	require.NoError(t, err)
	require.Len(t, spec.Tiles(), 1)
	_, err = dashboard.NewSpec([]dashboard.Tile{tile}, nil)
	require.ErrorContains(t, err, "leaves out")
}

func TestParseVisualKind(t *testing.T) {
	for _, k := range []string{"", "table", "bar", "line", "figure"} {
		got, err := dashboard.ParseVisualKind(k)
		require.NoError(t, err)
		require.Equal(t, dashboard.VisualKind(k), got)
	}
	_, err := dashboard.ParseVisualKind("bars")
	require.Error(t, err)
}

func TestPublic(t *testing.T) {
	long := strings.Repeat("é", 1200)
	answer := `{"kind":"rows","total":2,"rows":[
	  {"cells":[{"type":"Customer","id":"scus_1","name":"Globex"}, {"untrusted":"` + long + `"}, 12345678901234567890],"edges":[{"fact":"f1"}]},
	  {"cells":[{"type":"Customer","id":"scus_2","name":"Initech","extra":1}, null, 2.5],"edges":[]}]}`
	out, err := dashboard.Public(json.RawMessage(answer))
	require.NoError(t, err)
	var got struct {
		Kind  string `json:"kind"`
		Total int    `json:"total"`
		Rows  []struct {
			Cells []json.RawMessage `json:"cells"`
			Edges any               `json:"edges"`
		} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, "rows", got.Kind)
	require.Equal(t, 2, got.Total)
	require.JSONEq(t, `{"type":"Customer","name":"Globex"}`, string(got.Rows[0].Cells[0]), "an entity loses its stored identity")
	require.Equal(t, []any{}, got.Rows[0].Edges, "rows lose their edges")
	var untrusted map[string]string
	require.NoError(t, json.Unmarshal(got.Rows[0].Cells[1], &untrusted))
	require.Equal(t, 1000, len([]rune(untrusted["untrusted"])))
	require.True(t, strings.HasSuffix(untrusted["untrusted"], "…"))
	require.Equal(t, "12345678901234567890", string(got.Rows[0].Cells[2]), "numbers stay exact")
	require.JSONEq(t, `{"type":"Customer","id":"scus_2","name":"Initech","extra":1}`, string(got.Rows[1].Cells[0]),
		"only an entity cell, exactly, is an entity")

	for _, ok := range []string{
		`{"kind":"count","count":3}`,
		`{"kind":"group_count","groups":[{"value":"paid","count":2}],"total_groups":1}`,
		`{"kind":"aggregate","value":null,"values":0}`,
		`{"kind":"grouped_aggregate","groups":[],"total_groups":0}`,
	} {
		out, err := dashboard.Public(json.RawMessage(ok))
		require.NoError(t, err)
		require.JSONEq(t, ok, string(out))
	}
	for _, bad := range []string{`[]`, `null`, `{"kind":"entity"}`, `{"kind":"rows","rows":{}}`, `{"kind":"rows","rows":[{"edges":[]}]}`, `{"kind":"rows","rows":[1]}`} {
		_, err := dashboard.Public(json.RawMessage(bad))
		require.Error(t, err, bad)
	}
}

func TestNewSnapshot(t *testing.T) {
	spec, err := dashboard.ParseSpec([]byte(specJSON([]string{tileJSON("t1", countQuery), tileJSON("t2", groupQuery)}, `[["t1"],["t2"]]`)))
	require.NoError(t, err)
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.FixedZone("x", 3600))
	read := func(result string) json.RawMessage {
		return json.RawMessage(`{"kind":"query","query_echo":{"start":{"type":"Invoice","where":[]},"steps":[],"filters":[],"return":{"count":{"distinct_at":null}}},` +
			`"result":` + result + `,"stats":{"matched":3}}`)
	}
	reads := map[string]dashboard.Read{
		"t1": {AsOf: "2026-11-14", Answer: read(`{"kind":"count","count":3}`)},
		"t2": {AsOf: "2026-11-14", Answer: read(`{"kind":"group_count","groups":[],"total_groups":0}`)},
	}
	raw, err := dashboard.NewSnapshot("Revenue", "Weekly", spec, reads, at)
	require.NoError(t, err)
	var s dashboard.Snapshot
	require.NoError(t, json.Unmarshal(raw, &s))
	require.Equal(t, 1, s.V)
	require.Equal(t, "2026-11-14T08:00:00Z", s.PublishedAt)
	require.Len(t, s.Tiles, 2)
	require.Equal(t, "t1", s.Tiles[0].ID)
	require.JSONEq(t, `{"kind":"count","count":3}`, string(s.Tiles[0].Output))
	require.Contains(t, string(s.Tiles[0].Query), `"distinct_at":null`, "the query as the engine read it")
	require.NotContains(t, string(raw), "01ONTOLOGY", "a snapshot names no ontology")

	delete(reads, "t2")
	_, err = dashboard.NewSnapshot("Revenue", "", spec, reads, at)
	require.ErrorContains(t, err, "tile t2 has no read")

	reads["t2"] = dashboard.Read{Answer: json.RawMessage(`{"kind":"view"}`)}
	_, err = dashboard.NewSnapshot("Revenue", "", spec, reads, at)
	require.ErrorContains(t, err, "not a query read's")
	reads["t2"] = dashboard.Read{Answer: read(`{"kind":"entity"}`)}
	_, err = dashboard.NewSnapshot("Revenue", "", spec, reads, at)
	require.ErrorContains(t, err, "not a query's")

	var rows []string
	for range 300 {
		rows = append(rows, `{"cells":["`+strings.Repeat("x", 990)+`","`+strings.Repeat("y", 990)+`"],"edges":[]}`)
	}
	reads["t2"] = dashboard.Read{Answer: read(`{"kind":"rows","total":300,"rows":[` + strings.Join(rows, ",") + `]}`)}
	_, err = dashboard.NewSnapshot("Revenue", "", spec, reads, at)
	require.True(t, errors.Is(err, dashboard.ErrTooLarge), "a tile past its bound: %v", err)
}

func ExampleParseSpec() {
	spec, err := dashboard.ParseSpec([]byte(`{"v": 1,
	  "tiles": [{"id": "invoices", "title": "Invoices", "ontology_id": "01ONTOLOGY", "load": {"kind": "current"},
	             "query": {"start": {"type": "Invoice"}, "return": {"count": {}}}, "visual": {"kind": "figure"}}],
	  "layout": {"rows": [["invoices"]]}}`))
	if err != nil {
		panic(err)
	}
	fmt.Println(dashboard.Reads(spec.Tiles()))
	// Output: [{"query":{"return":{"count":{}},"start":{"type":"Invoice"}}}]
}

func TestFromQuestion(t *testing.T) {
	result := `{"status":"answered","answer":{"blocks":[],"visuals":[{"evidence_id":"e2","kind":"bar","title":"  By\nstatus  "},{"evidence_id":"e3","kind":"pie"}]},
	  "citations":[],"queries":[{"evidence_id":"e2","query":` + groupQuery + `,"output":{"kind":"group_count","groups":[],"total_groups":0}},
	  {"evidence_id":"e3","query":` + countQuery + `},{"evidence_id":"e4","query":{"start":{}}}],"usage":{}}`
	v, err := dashboard.FromQuestion(json.RawMessage(result), "e2")
	require.NoError(t, err)
	require.Equal(t, dashboard.VisualBar, v.Visual)
	require.Equal(t, "By status", v.Title)
	require.JSONEq(t, groupQuery, string(v.Query))

	v, err = dashboard.FromQuestion(json.RawMessage(result), "e3")
	require.NoError(t, err)
	require.Equal(t, dashboard.VisualAuto, v.Visual, "an unknown kind is no hint")
	require.Empty(t, v.Title)

	_, err = dashboard.FromQuestion(json.RawMessage(result), "e4")
	require.ErrorContains(t, err, "not a Query")
	_, err = dashboard.FromQuestion(json.RawMessage(result), "e9")
	require.ErrorContains(t, err, "no query result e9")
	_, err = dashboard.FromQuestion(json.RawMessage(`{"status":"cannot_answer","queries":[]}`), "e2")
	require.ErrorContains(t, err, "no answer")
	_, err = dashboard.FromQuestion(json.RawMessage(`[`), "e2")
	require.Error(t, err)
}

func TestTileTitle(t *testing.T) {
	require.Equal(t, "a b", dashboard.TileTitle(" a\u0007\tb "))
	long := dashboard.TileTitle(strings.Repeat("x", 100))
	require.Equal(t, dashboard.MaxTitleChars, len([]rune(long)))
	require.True(t, strings.HasSuffix(long, "…"))
	require.Empty(t, dashboard.TileTitle(" \n "))
}
