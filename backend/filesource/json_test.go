package filesource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		rows []string
	}{
		"array":                  {`[{"a":1},{"a":2}]`, []string{`{"a":1}`, `{"a":2}`}},
		"empty array":            {` [ ] `, nil},
		"empty file":             {"", nil},
		"only space":             {" \n\t ", nil},
		"envelope":               {`{"object":"list","data":[{"a":1},{"a":2}],"has_more":false,"meta":{"x":[1]},"tags":["t"]}`, []string{`{"a":1}`, `{"a":2}`}},
		"envelope key twice":     {`{"data":[{"a":1}],"data":null,"data":[{"a":2}]}`, []string{`{"a":1}`, `{"a":2}`}},
		"envelope nested object": {`{"page":{"n":1},"items":[{"a":1}]}`, []string{`{"a":1}`}},
		"envelope empty list":    {`{"data":[{"a":1}],"more":[]}`, []string{`{"a":1}`}},
		"lines":                  {"{\"a\":1}\n\n{\"a\":2}\r\n\r\n", []string{`{"a":1}`, `{"a":2}`}},
		"concatenated":           {`{"a":1}{"a":2} {"a":3}`, []string{`{"a":1}`, `{"a":2}`, `{"a":3}`}},
		"lines with list fields": {"{\"a\":1,\"items\":[{\"s\":1}]}\n{\"a\":2,\"items\":[]}", []string{`{"a":1,"items":[{"s":1}]}`, `{"a":2,"items":[]}`}},
		"single object":          {`{"a":1,"tags":["x"],"m":{"k":null}}`, []string{`{"a":1,"tags":["x"],"m":{"k":null}}`}},
		"single object, spaced":  {"\n  { \"a\" : [ 1 , 2 ] }\n", []string{`{"a":[1,2]}`}},
	} {
		t.Run(name, func(t *testing.T) {
			r := scanAll(t, tempFile(t, "x.json", []byte(tc.body)))
			require.Equal(t, tc.rows, r.raw)
		})
	}
}

// A JSON Lines file's objects are its rows: a day holding one event with a
// list of items is that event, not its items, so it merges with the other
// days' files instead of corrupting the table.
func TestJSONLinesExtension(t *testing.T) {
	event := `{"id":1,"items":[{"sku":"a"},{"sku":"b"}]}`
	for _, name := range []string{"events/2026-10-02.jsonl", "events/2026-10-02.NDJSON.gz"} {
		t.Run(name, func(t *testing.T) {
			data := []byte(event)
			if strings.HasSuffix(name, ".gz") {
				data = gzipBytes(t, data)
			}
			r := scanAll(t, tempFile(t, name, data))
			require.Equal(t, []string{event}, r.raw)
			require.Equal(t, []string{"id", "items"}, colNames(r.cols))
			s := sample(t, data, name, false, DefaultSampleLimits)
			require.Equal(t, []string{event}, rowStrings(s.Rows))
		})
	}
	t.Run("two lists of objects", func(t *testing.T) {
		r := scanAll(t, tempFile(t, "x.jsonl", []byte(`{"a":[{"x":1}],"b":[{"y":1}]}`)))
		require.Equal(t, []string{`{"a":[{"x":1}],"b":[{"y":1}]}`}, r.raw)
	})
	t.Run("an array is still read", func(t *testing.T) {
		r := scanAll(t, tempFile(t, "x.jsonl", []byte(`[{"a":1},{"a":2}]`)))
		require.Equal(t, []string{`{"a":1}`, `{"a":2}`}, r.raw)
	})
	t.Run("a .json file holds an envelope", func(t *testing.T) {
		r := scanAll(t, tempFile(t, "x.json", []byte(event)))
		require.Equal(t, []string{`{"sku":"a"}`, `{"sku":"b"}`}, r.raw)
	})
	t.Run("errors count lines", func(t *testing.T) {
		err := scanErr(t, DefaultLimits, tempFile(t, "x.jsonl", []byte(`{"items":[{"a":1},{"a":2}],"b":}`)))
		requireKind(t, err, ErrMalformed, "x.jsonl", 1)
		_, err = Sample(context.Background(), strings.NewReader("{\"a\":1}\n{\"a\":[{\"b\":1}],}"), "x.jsonl", FormatJSON, false, DefaultSampleLimits)
		requireKind(t, err, ErrMalformed, "x.jsonl", 2)
	})
}

func TestJSONErrors(t *testing.T) {
	deep := func(n int) string { return `{"a":` + strings.Repeat("[", n) + strings.Repeat("]", n) + "}" }
	for name, tc := range map[string]struct {
		body   string
		kind   error
		record int64
		msg    string
	}{
		"two lists":            {`{"a":[{"x":1}],"b":[{"y":1}]}`, ErrAmbiguousJSON, 0, "several lists of records (a, b); Helix reads one list per file"},
		"top-level scalar":     {`42`, ErrMalformed, 0, "not JSON"},
		"top-level string":     {`"x"`, ErrMalformed, 0, "not JSON"},
		"scalar element":       {`[{"a":1},5]`, ErrNotObject, 2, "x.json record 2: not a JSON object"},
		"null element":         {`[{"a":1},null]`, ErrNotObject, 2, "not a JSON object"},
		"array line":           {"{\"a\":1}\n[1,2]\n", ErrNotObject, 2, "not a JSON object"},
		"scalar line":          {"{\"a\":1}\n7\n", ErrNotObject, 2, "not a JSON object"},
		"truncated record":     {"{\"a\":1}\n{\"a\":", ErrMalformed, 2, "the file ends inside a record"},
		"truncated array":      {`[{"a":1},{"a":2}`, ErrMalformed, 3, "the file ends inside a record"},
		"malformed middle":     {"{\"a\":1}\n{\"a\":x}\n{\"a\":3}\n", ErrMalformed, 2, "malformed JSON: invalid character 'x'"},
		"data after array":     {`[{"a":1}] [{"a":2}]`, ErrMalformed, 2, "more data after the records"},
		"stray close":          {"{\"a\":1}\n]", ErrMalformed, 2, "malformed JSON"},
		"bad element in list":  {`{"data":[{"a":1},2]}`, ErrNotObject, 2, "not a JSON object"},
		"truncated envelope":   {`{"data":[{"a":1},{"a":2},{"a"`, ErrMalformed, 3, "the file ends inside a record"},
		"malformed first":      {`{"a":}`, ErrMalformed, 1, "malformed JSON"},
		"too deep":             {deep(64), ErrTooDeep, 1, "nested more than 64 levels deep"},
		"too deep in envelope": {`{"data":[` + deep(64) + `]}`, ErrTooDeep, 1, "nested more than 64 levels deep"},
		"too many columns":     {"{" + manyKeys(1001) + "}", ErrTooManyColumns, 1, "more than 1000 columns"},
	} {
		t.Run(name, func(t *testing.T) {
			err := scanErr(t, DefaultLimits, tempFile(t, "x.json", []byte(tc.body)))
			requireKind(t, err, tc.kind, "x.json", tc.record)
			require.Contains(t, err.Error(), tc.msg)
		})
	}
	t.Run("depth 64 reads", func(t *testing.T) {
		r := scanAll(t, tempFile(t, "x.json", []byte(deep(63))))
		require.Equal(t, TypeJSON, r.cols[0].Type)
	})
}

func manyKeys(n int) string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf(`"k%d":%d`, i, i)
	}
	return strings.Join(keys, ",")
}

func TestJSONRecordSize(t *testing.T) {
	record := func(size int) string {
		// {"a":"…"} is 8 bytes around the string's contents.
		return `{"a":"` + strings.Repeat("x", size-8) + `"}`
	}
	r := scanAll(t, tempFile(t, "x.jsonl", []byte(record(16<<20)+"\n"+record(10))))
	require.Len(t, r.rows, 2)
	err := scanErr(t, DefaultLimits, tempFile(t, "x.jsonl", []byte(record(10)+"\n"+record(16<<20+1))))
	requireKind(t, err, ErrRecordTooLarge, "x.jsonl", 2)
	require.Contains(t, err.Error(), "a record over 16 MiB")
}

func TestJSONValues(t *testing.T) {
	body := `{"i":9223372036854775807,"u":18446744073709551615,"n":-9223372036854775808,"big":1e400,"neg":-1e400,` +
		`"tiny":1e-400,"f":1.5,"e":"~u00e9~ud83d~ude00","lone":"~ud800x","low":"~udc00","bad":"` + "\xff" + `","esc":"a\"b\\c\n",` +
		`"t":true,"z":null,"o":{"k":[1,{"d":2}]},"dup":1,"dup":2}`
	r := scanAll(t, tempFile(t, "x.json", []byte(esc(body))))
	require.Equal(t, []string{esc(`{"i":9223372036854775807,"u":18446744073709551615,"n":-9223372036854775808,"big":"1e400","neg":"-1e400",` +
		`"tiny":1e-400,"f":1.5,"e":"~u00e9~ud83d~ude00","lone":"` + replacement + `x","low":"` + replacement + `","bad":"` + replacement + `","esc":"a\"b\\c\n",` +
		`"t":true,"z":null,"o":{"k":[1,{"d":2}]},"dup":2}`)}, r.raw)
	require.Equal(t, map[string]Type{
		"i": TypeInteger, "u": TypeFloat, "n": TypeInteger, "big": TypeFloat, "neg": TypeFloat, "tiny": TypeFloat,
		"f": TypeFloat, "e": TypeString, "lone": TypeString, "low": TypeString, "bad": TypeString, "esc": TypeString,
		"t": TypeBoolean, "z": TypeNull, "o": TypeJSON, "dup": TypeInteger,
	}, colTypes(r.cols))
	require.True(t, json.Valid([]byte(r.raw[0])))
}

func TestJSONColumnsAcrossRecords(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&b, "{\"id\":%d,\"a\":%d}\n", i, i)
	}
	b.WriteString(`{"id":5001,"a":1.5,"late":true}`)
	r := scanAll(t, tempFile(t, "x.jsonl", []byte(b.String())))
	require.Len(t, r.rows, 5001)
	require.Equal(t, []Column{
		{Name: "id", Type: TypeInteger},
		{Name: "a", Type: TypeFloat},
		{Name: "late", Type: TypeBoolean, Nullable: true},
	}, r.cols)
	require.Nil(t, r.rows[0]["late"])
	require.Equal(t, true, r.rows[5000]["late"])
	require.Equal(t, []string{"id"}, r.key)

	missing := scanAll(t, tempFile(t, "y.jsonl", []byte("{\"id\":1,\"a\":\"x\"}\n{\"id\":2}\n{\"id\":3,\"a\":null}")))
	require.Equal(t, []Column{{Name: "id", Type: TypeInteger}, {Name: "a", Type: TypeString, Nullable: true}}, missing.cols)
	require.Nil(t, missing.rows[1]["a"])
}

// TestJSONRecordsMalformed reads malformed text in a layout detection did
// not choose, as when a file changes between passes.
func TestJSONRecordsMalformed(t *testing.T) {
	envelope := jsonLayout{shape: shapeEnvelope, key: "data"}
	for name, tc := range map[string]struct {
		layout jsonLayout
		body   string
		msg    string
	}{
		"array expected":      {jsonLayout{shape: shapeArray}, `{"a":1}`, `malformed JSON: expected "["`},
		"envelope expected":   {envelope, `[1]`, `malformed JSON: expected "{"`},
		"envelope key":        {envelope, `{1:2}`, "malformed JSON"},
		"envelope cut at key": {envelope, `{"data"`, "the file ends inside a record"},
		"skipped value":       {envelope, `{"x":}`, "malformed JSON"},
		"records value":       {envelope, `{"data":{"k":}}`, "malformed JSON"},
		"records object key":  {envelope, `{"data":{1:2}}`, "malformed JSON"},
		"unclosed envelope":   {envelope, `{"data":[{"a":1}]`, "the file ends inside a record"},
		"envelope trailing":   {envelope, `{"data":[]}}`, "malformed JSON"},
		"record key":          {jsonLayout{shape: shapeLines}, `{"a":1,2}`, "malformed JSON"},
		"record unclosed":     {jsonLayout{shape: shapeLines}, `{"a":1 "b"}`, "malformed JSON"},
		"array unclosed":      {jsonLayout{shape: shapeArray}, `[{"a":1} 5`, "malformed JSON"},
	} {
		t.Run(name, func(t *testing.T) {
			br := newBudgetReader(context.Background(), strings.NewReader(tc.body), newBudget(1<<20), 1<<20)
			recs, err := newJSONRecords(br, tc.layout, 1<<20, false)
			require.NoError(t, err)
			for err == nil {
				var ok bool
				if ok, err = recs.next(); !ok && err == nil {
					t.Fatal("no error")
				}
			}
			require.ErrorIs(t, err, ErrMalformed)
			require.Contains(t, err.Error(), tc.msg)
		})
	}
	t.Run("envelope with a scalar under the key", func(t *testing.T) {
		br := newBudgetReader(context.Background(), strings.NewReader(`{"data":5,"data":[{"a":1}]}`), newBudget(1<<20), 1<<20)
		recs, err := newJSONRecords(br, envelope, 1<<20, false)
		require.NoError(t, err)
		ok, err := recs.next()
		require.True(t, ok)
		require.NoError(t, err)
	})
	t.Run("a cut sample with a malformed line fails", func(t *testing.T) {
		_, err := Sample(context.Background(), strings.NewReader("{\"a\":1}\n{\"a\":x}\n{\"a\":"), "x.jsonl", FormatJSON, true, DefaultSampleLimits)
		requireKind(t, err, ErrMalformed, "x.jsonl", 2)
	})
	t.Run("unclassified decoder errors", func(t *testing.T) {
		br := newBudgetReader(context.Background(), strings.NewReader(""), newBudget(1), 1)
		err := jsonError(errors.New("odd"), br)
		require.ErrorIs(t, err, ErrMalformed)
		require.Equal(t, "malformed JSON", err.Error())
	})
}

func TestJSONDetectionErrors(t *testing.T) {
	for name, body := range map[string]string{
		"first key":         `{1:2}`,
		"first value":       `{"a":}`,
		"nested value":      `{"a":{"b":}}`,
		"nested key":        `{"a":{1:2}}`,
		"list element":      `{"a":[1,}`,
		"list close":        `{"a":[1 2]}`,
		"object close":      `{"a":1 "b":2}`,
		"envelope trailing": `{"data":[{"a":1}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := scanErr(t, DefaultLimits, tempFile(t, "x.json", []byte(body)))
			requireKind(t, err, ErrMalformed, "x.json", anyRecord)
		})
	}
	_, err := Sample(context.Background(), strings.NewReader(`{"a":}`), "x.json", FormatJSON, false, DefaultSampleLimits)
	requireKind(t, err, ErrMalformed, "x.json", 1)
}

func TestJSONFileChanged(t *testing.T) {
	f := tempFile(t, "x.jsonl", []byte(`{"a":1}`))
	s, err := Scan(context.Background(), []File{f}, DefaultLimits)
	require.NoError(t, err)
	require.NoError(t, osWrite(f.Path, []byte(`{"a":`+strings.Repeat("[", 70)+strings.Repeat("]", 70)+`}`)))
	for _, err := range s.Rows(context.Background()) {
		requireKind(t, err, ErrTooDeep, "x.jsonl", 1)
	}
	r := scanAll(t, tempFile(t, "y.jsonl", []byte("{\"id\":null}\n{\"id\":1}")))
	require.Nil(t, r.key)
}
