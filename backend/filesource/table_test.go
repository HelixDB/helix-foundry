package filesource

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/stretchr/testify/require"
)

func value(s string) *string { return &s }

func TestScanFiles(t *testing.T) {
	t.Run("schema union with nulls", func(t *testing.T) {
		r := scanAll(t,
			tempFile(t, "a.jsonl", []byte(`{"id":1,"a":1}`)),
			tempFile(t, "b.jsonl.gz", gzipBytes(t, []byte(`{"id":2,"b":"x","a":2.5}`))),
			tempFile(t, "c.json", []byte(`[]`)),
		)
		require.Equal(t, []Column{
			{Name: "id", Type: TypeInteger},
			{Name: "a", Type: TypeFloat},
			{Name: "b", Type: TypeString, Nullable: true},
		}, r.cols)
		require.Equal(t, []string{`{"id":1,"a":1,"b":null}`, `{"id":2,"a":2.5,"b":"x"}`}, r.raw)
	})
	t.Run("no files", func(t *testing.T) {
		r := scanAll(t)
		require.Empty(t, r.cols)
		require.Empty(t, r.rows)
		require.Nil(t, r.key)
	})
	t.Run("unknown format", func(t *testing.T) {
		err := scanErr(t, DefaultLimits, File{Path: "x", Name: "x.bin"})
		requireKind(t, err, ErrMalformed, "x.bin", 0)
	})
	// A local file that cannot be opened is the reader's failure, not the
	// data's: it stays a bare error, never an *Error naming a temp path.
	t.Run("missing file", func(t *testing.T) {
		for _, name := range []string{"x.json", "x.csv", "x.parquet"} {
			f, _ := FormatOf(name)
			_, err := Scan(context.Background(), []File{{Path: "/nonexistent/x", Name: name, Format: f}}, DefaultLimits)
			require.ErrorIs(t, err, fs.ErrNotExist)
			var e *Error
			require.False(t, errors.As(err, &e), "%s: %v", name, err)
		}
	})
	t.Run("files removed before rows", func(t *testing.T) {
		pq := parquetBytes(t, arrow.NewSchema([]arrow.Field{col("a", arrow.PrimitiveTypes.Int32)}, nil), nil, pqarrow.DefaultWriterProps())
		for _, f := range []File{tempFile(t, "x.json", []byte(`[{"a":1}]`)), tempFile(t, "x.csv", []byte("a\n1\n")), tempFile(t, "x.parquet", pq)} {
			s, err := Scan(context.Background(), []File{f}, DefaultLimits)
			require.NoError(t, err)
			require.NoError(t, osWrite(f.Path, nil))
			require.NoError(t, removeFile(f.Path))
			for _, err := range s.Rows(context.Background()) {
				require.ErrorIs(t, err, fs.ErrNotExist)
				var e *Error
				require.False(t, errors.As(err, &e), "%s: %v", f.Name, err)
			}
		}
	})
	t.Run("stop early", func(t *testing.T) {
		for _, f := range []File{tempFile(t, "x.jsonl", []byte(twoLines)), tempFile(t, "x.csv", []byte("a\n1\n2\n"))} {
			s, err := Scan(context.Background(), []File{f, f}, DefaultLimits)
			require.NoError(t, err)
			n := 0
			for range s.Rows(context.Background()) {
				n++
				break
			}
			require.Equal(t, 1, n)
		}
	})
}

func TestPartitionColumns(t *testing.T) {
	r := scanAll(t,
		tempFile(t, "t/dt=2026-10-01/n=1/region=eu/a.csv", []byte("id,region\n1,x\n2,y\n"),
			Partition{"dt", value("2026-10-01")}, Partition{"n", value("1")}, Partition{"region", value("eu")}, Partition{"flag", value("true")}),
		tempFile(t, "t/dt=2026-10-02/n=x/b.csv", []byte("id,region\n3,z\n"),
			Partition{"dt", value("2026-10-02")}, Partition{"n", value("x")}, Partition{"flag", nil}),
		tempFile(t, "t/c.csv", []byte("id,region\n4,w\n"), Partition{"n", value("")}, Partition{"n", value("7")}),
	)
	require.Equal(t, []Column{
		{Name: "id", Type: TypeInteger},
		{Name: "region", Type: TypeString},
		{Name: "dt", Type: TypeString, Nullable: true},
		{Name: "n", Type: TypeString},
		{Name: "flag", Type: TypeBoolean, Nullable: true},
	}, r.cols)
	require.Equal(t, []string{
		`{"id":1,"region":"x","dt":"2026-10-01","n":"1","flag":true}`,
		`{"id":2,"region":"y","dt":"2026-10-01","n":"1","flag":true}`,
		`{"id":3,"region":"z","dt":"2026-10-02","n":"x","flag":null}`,
		`{"id":4,"region":"w","dt":null,"n":"7","flag":null}`,
	}, r.raw)
	require.Equal(t, []string{"id"}, r.key)

	typed := scanAll(t, tempFile(t, "a.jsonl", []byte(`{"v":1}`), Partition{"year", value("2026")}))
	require.Equal(t, []string{`{"v":1,"year":2026}`}, typed.raw)
	require.Equal(t, TypeInteger, typed.cols[1].Type)

	lim := DefaultLimits
	lim.MaxColumns = 1
	err := scanErr(t, lim, tempFile(t, "a.jsonl", []byte(`{"v":1}`), Partition{"year", value("2026")}))
	requireKind(t, err, ErrTooManyColumns, "a.jsonl", 0)
}

func TestPrimaryKey(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		key   []string
	}{
		"unique":            {map[string]string{"a.jsonl": "{\"id\":1}\n{\"id\":2}", "b.jsonl": `{"id":3}`}, []string{"id"}},
		"string and number": {map[string]string{"a.jsonl": "{\"id\":1}\n{\"id\":\"1\"}"}, nil},
		"escaped strings":   {map[string]string{"a.jsonl": "{\"id\":\"aA\"}\n{\"id\":\"a\\u0041\"}"}, nil},
		"duplicate":         {map[string]string{"a.jsonl": `{"id":1}`, "b.jsonl": `{"id":1}`}, nil},
		"null":              {map[string]string{"a.jsonl": "{\"id\":1}\n{\"id\":null}"}, nil},
		"missing in a row":  {map[string]string{"a.jsonl": "{\"id\":1}\n{\"x\":2}"}, nil},
		"missing in a file": {map[string]string{"a.jsonl": `{"id":1}`, "b.jsonl": `{"x":1}`}, nil},
		"object id":         {map[string]string{"a.jsonl": `{"id":{"k":1}}`}, nil},
		"no id":             {map[string]string{"a.jsonl": `{"x":1}`}, nil},
		"csv unique":        {map[string]string{"a.csv": "id\n1\n2\n", "b.csv": "id\n3\n"}, []string{"id"}},
		"csv duplicate":     {map[string]string{"a.csv": "id\n1\n2\n1\n"}, nil},
		"csv empty":         {map[string]string{"a.csv": "id,x\n1,a\n,b\n"}, nil},
		"csv no id column":  {map[string]string{"a.csv": "id\n1\n", "b.csv": "x\n1\n"}, nil},
		"csv header only":   {map[string]string{"a.csv": "id\n"}, nil},
		"id as partition":   {map[string]string{"a.jsonl": `{"x":1}`}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			var files []File
			for _, n := range []string{"a.jsonl", "b.jsonl", "a.csv", "b.csv"} {
				if body, ok := tc.files[n]; ok {
					files = append(files, tempFile(t, n, []byte(body), Partition{"id", value("1")}))
				}
			}
			require.Equal(t, tc.key, scanAll(t, files...).key)
		})
	}
	t.Run("too many ids to prove", func(t *testing.T) {
		fs := newFileStats(10, newTableStats(10).seed)
		fs.ids = make([]uint64, maxKeyRows)
		fs.addID('j', []byte("1"))
		require.True(t, fs.idNull)
		ts := newTableStats(10)
		ts.ids = map[uint64]struct{}{1: {}}
		require.NoError(t, ts.merge(&fileStats{ids: make([]uint64, maxKeyRows)}))
		require.True(t, ts.idDropped)
		require.NoError(t, ts.merge(&fileStats{ids: []uint64{2}}))
	})
}

func TestRecoveredPanic(t *testing.T) {
	err := safe(FormatParquet, func() error { panic("index out of range") })
	require.ErrorIs(t, err, ErrParquet)
	require.Equal(t, "the file could not be decoded", err.Error())
	require.ErrorIs(t, safe(FormatJSON, func() error { panic(1) }), ErrMalformed)
	require.NoError(t, safe(FormatCSV, func() error { return nil }))
}

// TestRowsProperty checks that every row of random tables is one object
// holding exactly Columns(), across formats and files.
func TestRowsProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 9))
	values := []string{`1`, `-2.5`, `"s"`, `true`, `null`, `{"k":[1]}`, `[]`, `1e400`, esc(`"~u00e9"`)}
	for i := range 50 {
		var files []File
		for f := range 1 + rng.IntN(3) {
			var lines []string
			for range rng.IntN(20) {
				var fields []string
				for k := range rng.IntN(6) {
					fields = append(fields, fmt.Sprintf(`"k%d":%s`, k+rng.IntN(4), values[rng.IntN(len(values))]))
				}
				lines = append(lines, "{"+strings.Join(fields, ",")+"}")
			}
			body := []byte(strings.Join(lines, "\n"))
			name := fmt.Sprintf("t/f%d.jsonl", f)
			if rng.IntN(2) == 0 {
				body, name = gzipBytes(t, body), name+".gz"
			}
			files = append(files, tempFile(t, name, body, Partition{"p", value(fmt.Sprint(i))}))
		}
		scanAll(t, files...) // checks every row against Columns()
	}
}
