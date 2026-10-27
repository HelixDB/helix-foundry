package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "write testdata/render/*.md from Render")

// storedSchema reads a config.schema.
func storedSchema(t *testing.T, stored string) Schema {
	t.Helper()
	var s Schema
	if err := json.Unmarshal([]byte(stored), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// render is Render of a row that must render.
func render(t *testing.T, row string, s Schema, textField string) Rendered {
	t.Helper()
	r, err := Render([]byte(row), s, textField)
	if err != nil {
		t.Fatalf("Render(%s): %v", row, err)
	}
	return r
}

// pieceRow is a row of piece p, received at 09:00 with no occurred_at,
// holding these fields (`"name":value,...`, or "").
func pieceRow(fields string) string {
	row := `{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null`
	if fields != "" {
		row += "," + fields
	}
	return row + "}"
}

// fieldLines is the lines a piece with these fields renders between its
// "- Piece" line and its text, under schema, text field reply.
func fieldLines(t *testing.T, schema, fields string) string {
	t.Helper()
	body := string(render(t, pieceRow(fields), storedSchema(t, schema), "reply").Body)
	_, lines, _ := strings.Cut(body, "- Piece: p\n")
	lines, _, _ = strings.Cut(lines, "\n\n")
	return lines
}

func TestRenderGoldens(t *testing.T) {
	var keys, tags []string
	for i := 59; i >= 0; i-- {
		keys = append(keys, fmt.Sprintf(`"k%02d":%d`, i, i))
	}
	for i := range 25 {
		tags = append(tags, fmt.Sprintf(`"t%02d"`, i+1))
	}
	titleLine := "Café ☕ — 日本語のテキスト 🎉 "
	var long strings.Builder
	long.WriteString("## Weekly summary\n\n")
	for i := range 120 {
		fmt.Fprintf(&long, "%d. Item %d: the customer asked about invoice INV-%04d and was told it ships soon.\n", i+1, i+1, i)
	}
	long.WriteString("\n```json\n{\"a\": 1}\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n")
	// Each golden is the document extraction reads for a piece, and every
	// row of its case must render to it byte for byte: a version that moved
	// for any of them would extract the piece again.
	for _, c := range []struct {
		name, schema, textField, title string
		rows                           []string
	}{
		{
			name: "plain", textField: "reply", title: "Thanks for the order.",
			schema: `{"amount":"number","customer":"string","note":"null","paid":"boolean","reply":"string","status":"string"}`,
			rows: []string{
				`{"id":"o-17","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00Z","amount":12.50,"customer":"c_1","note":null,"paid":true,"reply":"Thanks for the order.\n\nIt ships Monday.\n","status":"shipped\nlate"}`,
				// The same piece sent again later, in a row whose keys and
				// padding differ, as a snapshot's row may.
				`{"id":"o-17","received_at":"2026-10-24T10:30:00.123456Z","occurred_at":"2026-10-23T18:20:00Z","status":"shipped\nlate","reply":"Thanks for the order.\n\nIt ships Monday.\n","paid":true,"customer":"c_1","amount":12.50}`,
			},
		},
		{
			name: "nested", textField: "reply", title: "Nested data",
			schema: `{"a":"object","m":"object","reply":"string","z":"string","zz":"object"}`,
			rows: []string{
				`{"id":"p-1","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"f":true,"b":{"e":"deep","c":{"d":1}}},"m":{` + strings.Join(keys, ",") + `},"reply":"Nested data","z":"last","zz":{"q":1}}`,
			},
		},
		{
			name: "arrays", textField: "reply", title: "Arrays",
			schema: `{"empty":"array","flags":"array","meta":"object","nums":"array","one":"array","reply":"string","tags":"array"}`,
			rows: []string{
				`{"id":"p-2","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"empty":[],"flags":[true,false,null],"meta":{"tags":["a","b"],"none":[]},"nums":[9223372036854775807,1e-7,-0.5,2.50],"one":["only\none"],"reply":"Arrays","tags":[` + strings.Join(tags, ",") + `]}`,
			},
		},
		{
			name: "records", textField: "reply", title: "Records",
			schema: `{"items":"array","lines":"object","matrix":"array","mixed":"array","reply":"string","single":"array"}`,
			rows: []string{
				`{"id":"p-3","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"items":[{"sku":"a"},{"sku":"b"},{"sku":"c"}],"lines":{"rows":[{"x":1},{"x":2}]},"matrix":[[1,2],[3]],"mixed":[1,{"a":1}],"reply":"Records","single":[{"sku":"a"}]}`,
			},
		},
		{
			name: "long-value", textField: "reply", title: "Long value",
			schema: `{"reply":"string","summary":"string"}`,
			rows: []string{
				`{"id":"p-4","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"reply":"Long value","summary":"First line\r\nsecond line\n\n\nthird\u2028fourth ` + strings.Repeat("é", 600) + `"}`,
			},
		},
		{
			name: "unicode-title", textField: "reply", title: string([]rune(strings.Repeat(titleLine, 7))[:119]) + "…",
			schema: `{"reply":"string"}`,
			rows: []string{
				`{"id":"p-5","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"reply":` + string(jsonString("\n \n\t "+strings.Repeat(titleLine, 7)+"\nSecond line")) + `}`,
			},
		},
		{
			name: "no-text", textField: "reply", title: "p-6",
			schema: `{"n":"number","reply":"string"}`,
			rows: []string{
				`{"id":"p-6","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00.123456Z","n":1,"reply":null}`,
				`{"id":"p-6","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00.123456Z","n":1}`,
				`{"id":"p-6","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00.123456Z","n":1,"reply":""}`,
				`{"id":"p-6","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00.123456Z","n":1,"reply":" \n\t\u2028\u00a0 "}`,
			},
		},
		{
			name: "long-text", textField: "reply", title: "## Weekly summary",
			schema: `{"reply":"string","source":"string"}`,
			rows: []string{
				`{"id":"p-7","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"reply":` + string(jsonString(long.String())) + `,"source":"agent"}`,
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := storedSchema(t, c.schema)
			path := filepath.Join("testdata", "render", c.name+".md")
			if *update {
				if err := os.WriteFile(path, render(t, c.rows[0], s, c.textField).Body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for i, row := range c.rows {
				got := render(t, row, s, c.textField)
				if !bytes.Equal(got.Body, want) {
					t.Errorf("row %d renders\n%s\nwant %s:\n%s", i, got.Body, path, want)
				}
				if got.Title != c.title {
					t.Errorf("row %d titled %q, want %q", i, got.Title, c.title)
				}
			}
		})
	}

	// The text is the document's text as sent: the extraction reads it,
	// not a rendering of it.
	t.Run("ends with the text verbatim", func(t *testing.T) {
		row := `{"id":"p-7","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"reply":` + string(jsonString(long.String())) + `}`
		if body := render(t, row, storedSchema(t, `{"reply":"string"}`), "reply").Body; !bytes.HasSuffix(body, []byte("\n\n"+long.String())) {
			t.Fatalf("body ends %q", body[len(body)-80:])
		}
	})
}

func TestRenderStability(t *testing.T) {
	s := storedSchema(t, `{"a":"object","n":"number","note":"string","reply":"string","tags":"array"}`)
	version := func(row string) string { return Version(render(t, row, s, "reply").Body) }
	base := version(`{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":2}},"n":1,"reply":"Hi","tags":["a"]}`)

	// An identical resend is "kept": no write and no extraction. That holds
	// only if nothing but the piece's data reaches its document.
	for _, c := range []struct{ name, row string }{
		{"a later arrival", `{"id":"p","received_at":"2026-10-25T11:12:13.141516Z","occurred_at":null,"a":{"x":1,"y":{"z":2}},"n":1,"reply":"Hi","tags":["a"]}`},
		{"padded nulls", `{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":2}},"n":1,"note":null,"reply":"Hi","tags":["a"]}`},
		{"no occurred_at key", `{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","a":{"x":1,"y":{"z":2}},"n":1,"reply":"Hi","tags":["a"]}`},
		{"keys in another order", `{"tags":["a"],"reply":"Hi","n":1,"a":{"y":{"z":2},"x":1},"occurred_at":null,"received_at":"2026-10-24T09:00:00.000000Z","id":"p"}`},
		{"data deeper than shown", `{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":2,"deeper":{"w":3}}},"n":1,"reply":"Hi","tags":["a"]}`},
	} {
		t.Run("keeps its version with "+c.name, func(t *testing.T) {
			if got := version(c.row); got != base {
				t.Fatalf("version %s, want %s", got, base)
			}
		})
	}

	// A changed body is a new document, extracted again.
	for _, c := range []struct{ name, row string }{
		{"another text", `{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":2}},"n":1,"reply":"Hi!","tags":["a"]}`},
		{"another value", `{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":2}},"n":1.0,"reply":"Hi","tags":["a"]}`},
		{"a nested value", `{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":3}},"n":1,"reply":"Hi","tags":["a"]}`},
		{"an occurred_at", `{"id":"p","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00Z","a":{"x":1,"y":{"z":2}},"n":1,"reply":"Hi","tags":["a"]}`},
		{"another id", `{"id":"q","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"a":{"x":1,"y":{"z":2}},"n":1,"reply":"Hi","tags":["a"]}`},
	} {
		t.Run("changes its version with "+c.name, func(t *testing.T) {
			if got := version(c.row); got == base {
				t.Fatal("kept its version")
			}
		})
	}
}

func TestRenderNumbers(t *testing.T) {
	// A number reaches the document as sent: no float rounding, no
	// reformatting, in a field, an object or an array.
	for _, literal := range []string{"9223372036854775807", "-9223372036854775808", "18446744073709551617", "1e-7", "1E+2", "-0", "0.10", "1.50e300", "12.50"} {
		t.Run(literal, func(t *testing.T) {
			got := fieldLines(t, `{"l":"array","n":"number","o":"object","reply":"string"}`, `"l":[`+literal+`,`+literal+`],"n":`+literal+`,"o":{"n":`+literal+`}`)
			if want := fmt.Sprintf("- l: %[1]s, %[1]s\n- n: %[1]s\n- o.n: %[1]s\n", literal); got != want {
				t.Fatalf("got\n%swant\n%s", got, want)
			}
		})
	}
}

func TestRenderLines(t *testing.T) {
	var items, objects, wide, wider []string
	for i := range 21 {
		items = append(items, fmt.Sprint(i+1))
	}
	for range 1000 {
		objects = append(objects, "{}")
	}
	for i := range 30 {
		wide = append(wide, fmt.Sprintf(`"a%02d":%d`, i, i))
		wider = append(wider, fmt.Sprintf(`"b%02d":%d`, i, i))
	}
	keyLines := func(prefix string, from, to int) string {
		var b strings.Builder
		for i := from; i < to; i++ {
			fmt.Fprintf(&b, "- %s%02d: %d\n", prefix, i, i)
		}
		return b.String()
	}
	// Each case pins one rule of a field's lines.
	for _, c := range []struct{ name, schema, fields, want string }{
		{"orders fields by name byte order", `{"B":"number","a":"number","reply":"string","é":"number"}`, `"é":3,"a":2,"B":1`, "- B: 1\n- a: 2\n- é: 3\n"},
		{"folds every kind of line break", `{"reply":"string","s":"string"}`, `"s":"a\r\nb\n\nc\u2028d\u2029e\u0085f\u000bg\u000ch\ri"`, "- s: a b c d e f g h i\n"},
		{"keeps other spaces", `{"reply":"string","s":"string"}`, `"s":"a\tb  c"`, "- s: a\tb  c\n"},
		{"drops line breaks at either end", `{"reply":"string","s":"string"}`, `"s":"\n\na\n"`, "- s: a\n"},
		{"keeps a string of 500 runes whole", `{"reply":"string","s":"string"}`, `"s":"` + strings.Repeat("é", 500) + `"`, "- s: " + strings.Repeat("é", 500) + "\n"},
		{"cuts a string of 501 runes", `{"reply":"string","s":"string"}`, `"s":"` + strings.Repeat("é", 501) + `"`, "- s: " + strings.Repeat("é", 499) + "…\n"},
		{"cuts a string after folding it", `{"reply":"string","s":"string"}`, `"s":"` + strings.Repeat("\\n", 400) + strings.Repeat("x", 500) + `"`, "- s: " + strings.Repeat("x", 500) + "\n"},
		{"leaves out what has no value", `{"a":"null","b":"string","c":"array","d":"object","e":"string","reply":"string"}`, `"a":null,"b":"","c":[],"d":{},"e":"\n\n"`, ""},
		{"shows booleans", `{"f":"boolean","reply":"string","t":"boolean"}`, `"f":false,"t":true`, "- f: false\n- t: true\n"},
		{"lists 20 scalars whole", `{"l":"array","reply":"string"}`, `"l":[` + strings.Join(items[:20], ",") + `]`, "- l: " + strings.Join(items[:20], ", ") + "\n"},
		{"lists the first 20 of 21", `{"l":"array","reply":"string"}`, `"l":[` + strings.Join(items, ",") + `]`, "- l: " + strings.Join(items[:20], ", ") + ", and 1 more\n"},
		{"shows null in a list", `{"l":"array","reply":"string"}`, `"l":[null,"",1]`, "- l: null, , 1\n"},
		{"folds and cuts a string in a list", `{"l":"array","reply":"string"}`, `"l":["a\nb","` + strings.Repeat("x", 501) + `"]`, "- l: a b, " + strings.Repeat("x", 499) + "…\n"},
		{"counts one record", `{"l":"array","reply":"string"}`, `"l":[{}]`, "- l: 1 record\n"},
		{"counts an array of arrays as records", `{"l":"array","reply":"string"}`, `"l":[[]]`, "- l: 1 record\n"},
		{"counts records in thousands", `{"l":"array","reply":"string"}`, `"l":[` + strings.Join(objects, ",") + `]`, "- l: 1,000 records\n"},
		{"names a nested key on one line", `{"o":"object","reply":"string"}`, `"o":{"k\nx":1,"":2}`, "- o.: 2\n- o.k x: 1\n"},
		{"cuts a long nested key", `{"o":"object","reply":"string"}`, `"o":{"` + strings.Repeat("k", 501) + `":1}`, "- o." + strings.Repeat("k", 499) + "…: 1\n"},
		{"shows a nested key twice once", `{"o":"object","reply":"string"}`, `"o":{"k":1,"k":2}`, "- o.k: 2\n"},
		{"goes 3 deep and no deeper", `{"o":"object","reply":"string"}`, `"o":{"a":{"b":1,"c":{"d":1},"l":[1,{"x":1}]}}`, "- o.a.b: 1\n- o.a.l: 2 records\n"},
		{"gives fields inside objects 50 lines in all", `{"a":"object","b":"object","c":"number","reply":"string"}`, `"a":{` + strings.Join(wide, ",") + `},"b":{` + strings.Join(wider, ",") + `},"c":1`, keyLines("a.a", 0, 30) + keyLines("b.b", 0, 20) + "- c: 1\n"},
		{"counts no top-level field in the 50", `{"a":"object","b":"number","c":"array","reply":"string"}`, `"a":{` + strings.Join(wide, ",") + `,` + strings.Join(wider, ",") + `},"b":2,"c":[3]`, keyLines("a.a", 0, 30) + keyLines("a.b", 0, 20) + "- b: 2\n- c: 3\n"},
		{"counts only lines shown in the 50", `{"a":"object","reply":"string"}`, `"a":{"0":null,"1":"","2":[],"3":{}` + "," + strings.Join(wide, ",") + `,` + strings.Join(wider, ",") + `}`, keyLines("a.a", 0, 30) + keyLines("a.b", 0, 20)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := fieldLines(t, c.schema, c.fields); got != c.want {
				t.Fatalf("got\n%q\nwant\n%q", got, c.want)
			}
		})
	}
}

func TestRenderTitle(t *testing.T) {
	// A title names the document in Foundry and heads it: one line of the
	// text, short enough to read in a list.
	for _, c := range []struct{ name, text, want string }{
		{"is the first line", "Hello\nworld", "Hello"},
		{"skips blank lines", "\n \r\n\t\nHello", "Hello"},
		{"collapses spaces", "  Hello \t  world  ", "Hello world"},
		{"ends at any line break", "a\u2028b", "a"},
		{"keeps a heading's marks", "# Title\n\nText", "# Title"},
		{"keeps 120 runes whole", strings.Repeat("é", 120), strings.Repeat("é", 120)},
		{"cuts 121 runes", strings.Repeat("é", 121), strings.Repeat("é", 119) + "…"},
		{"cuts emoji by rune", strings.Repeat("🎉", 130) + "\nmore", strings.Repeat("🎉", 119) + "…"},
		{"is the id for a blank text", " \n\t\u00a0\u2028", "p"},
		{"is the id for an empty text", "", "p"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := render(t, pieceRow(`"reply":`+string(jsonString(c.text))), storedSchema(t, `{"reply":"string"}`), "reply")
			if r.Title != c.want {
				t.Fatalf("title %q, want %q", r.Title, c.want)
			}
			if !strings.HasPrefix(string(r.Body), "# "+c.want+"\n\n- Piece: p\n") {
				t.Fatalf("body %q", r.Body)
			}
			if utf8.RuneCountInString(r.Title) > 120 {
				t.Fatalf("a title of %d runes", utf8.RuneCountInString(r.Title))
			}
		})
	}
}

func TestRenderRefuses(t *testing.T) {
	s := storedSchema(t, `{"n":"number","reply":"string"}`)

	// A text field the schema doesn't hold as a string is settings and
	// schema out of step (WithText locks it), never a row to skip.
	for _, c := range []struct{ name, schema, textField string }{
		{"a number field", `{"reply":"number"}`, "reply"},
		{"a field only ever null", `{"reply":"null"}`, "reply"},
		{"a field the schema lacks", `{"n":"number"}`, "reply"},
		{"no text field", `{"reply":"string"}`, ""},
	} {
		t.Run("refuses as its text "+c.name, func(t *testing.T) {
			_, err := Render([]byte(pieceRow("")), storedSchema(t, c.schema), c.textField)
			if err == nil || errors.Is(err, ErrCorruptRow) {
				t.Fatalf("got %v", err)
			}
		})
	}

	// A row no sync writes fails the sync for good, never a document
	// missing or changing data.
	for _, c := range []struct{ name, row string }{
		{"an empty row", ``},
		{"a row that is not an object", `["p"]`},
		{"a row that is a string", `"p"`},
		{"a cut row", `{"id":"p"`},
		{"a cut row after a comma", `{"id":"p",`},
		{"a row with no colon", `{"id"}`},
		{"a key with no value", `{"id":}`},
		{"a key that is not a string", `{1:2}`},
		{"two values with no comma", `{"id":"p" "n":1}`},
		{"a trailing comma", `{"id":"p","n":1,}`},
		{"two rows", `{"id":"p"}{"id":"q"}`},
		{"text after the row", `{"id":"p"} x`},
		{"a key twice", `{"id":"p","n":1,"n":2}`},
		{"no id", `{"received_at":"2026-10-24T09:00:00.000000Z"}`},
		{"an id with a space", `{"id":"a b"}`},
		{"an id that is a number", `{"id":7}`},
		{"an occurred_at that is not a time", `{"id":"p","occurred_at":"yesterday"}`},
		{"an occurred_at that is a number", `{"id":"p","occurred_at":7}`},
		{"a field the schema lacks", `{"id":"p","other":1}`},
		{"a field of another type", `{"id":"p","n":"1"}`},
		{"a text of another type", `{"id":"p","reply":1}`},
		{"a bad number", `{"id":"p","n":01}`},
	} {
		t.Run("refuses "+c.name, func(t *testing.T) {
			if _, err := Render([]byte(c.row), s, "reply"); !errors.Is(err, ErrCorruptRow) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestVersion(t *testing.T) {
	// A version is stored (foundry_documents.version) and compared with the
	// next sync's: lowercase hex SHA-256, 64 characters.
	for body, want := range map[string]string{
		"":    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"abc": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
	} {
		if got := Version([]byte(body)); got != want {
			t.Errorf("Version(%q) = %s, want %s", body, got, want)
		}
	}
}
