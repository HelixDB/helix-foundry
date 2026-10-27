package filesource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func lines(n int, format string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, format, i, i)
	}
	return b.String()
}

func sample(t *testing.T, data []byte, name string, truncated bool, lim SampleLimits) Sampled {
	t.Helper()
	f, _ := FormatOf(name)
	s, err := Sample(context.Background(), bytes.NewReader(data), name, f, truncated, lim)
	require.NoError(t, err)
	return s
}

func rowStrings(rows [][]byte) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = string(r)
	}
	return out
}

func TestSampleJSON(t *testing.T) {
	body := lines(50, "{\"id\":%d,\"name\":\"n%d\"}\n") + `{"id":51,"late":true}` + "\n"
	t.Run("whole file", func(t *testing.T) {
		s := sample(t, []byte(body), "x.jsonl", false, DefaultSampleLimits)
		require.False(t, s.Truncated)
		require.Equal(t, []Column{{Name: "id", Type: TypeInteger}, {Name: "name", Type: TypeString, Nullable: true}, {Name: "late", Type: TypeBoolean, Nullable: true}}, s.Columns)
		require.Equal(t, []string{`{"id":1,"name":"n1","late":null}`, `{"id":2,"name":"n2","late":null}`, `{"id":3,"name":"n3","late":null}`,
			`{"id":4,"name":"n4","late":null}`, `{"id":5,"name":"n5","late":null}`}, rowStrings(s.Rows))
	})
	t.Run("cut mid-line", func(t *testing.T) {
		cut := body[:len(body)/2+3]
		s := sample(t, []byte(cut), "x.jsonl", true, DefaultSampleLimits)
		require.True(t, s.Truncated)
		require.Len(t, s.Rows, 5)
		require.Equal(t, []string{"id", "name"}, colNames(s.Columns))
	})
	t.Run("cut gzip", func(t *testing.T) {
		gz := gzipBytes(t, []byte(strings.Repeat(body, 50)))
		s := sample(t, gz[:len(gz)/2], "x.jsonl.gz", true, DefaultSampleLimits)
		require.True(t, s.Truncated)
		require.Len(t, s.Rows, 5)
	})
	t.Run("cut array", func(t *testing.T) {
		s := sample(t, []byte(`[{"a":1},{"a":2},{"a":`), "x.json", true, DefaultSampleLimits)
		require.Equal(t, []string{`{"a":1}`, `{"a":2}`}, rowStrings(s.Rows))
	})
	t.Run("cut envelope", func(t *testing.T) {
		s := sample(t, []byte(`{"object":"list","data":[{"a":1},{"a":2},{"a":3`), "x.json", true, DefaultSampleLimits)
		require.Equal(t, []string{`{"a":1}`, `{"a":2}`}, rowStrings(s.Rows))
		require.True(t, s.Truncated)
	})
	t.Run("cut object without lists", func(t *testing.T) {
		s := sample(t, []byte(`{"a":1,"b":"long`), "x.json", true, DefaultSampleLimits)
		require.Empty(t, s.Rows)
		require.Empty(t, s.Columns)
	})
	t.Run("decompressed cap", func(t *testing.T) {
		lim := DefaultSampleLimits
		lim.MaxDecompressed = 1000
		s := sample(t, []byte(body), "x.jsonl", false, lim)
		require.True(t, s.Truncated)
		require.Len(t, s.Rows, 5)
	})
	t.Run("record cap", func(t *testing.T) {
		lim := DefaultSampleLimits
		lim.MaxRecords = 10
		s := sample(t, []byte(body), "x.jsonl", false, lim)
		require.True(t, s.Truncated)
		require.Equal(t, []string{"id", "name"}, colNames(s.Columns))
	})
	t.Run("large rows are not previewed", func(t *testing.T) {
		big := `{"id":0,"text":"` + strings.Repeat("x", 5000) + "\"}\n"
		s := sample(t, []byte(big+lines(3, "{\"id\":%d,\"text\":\"t%d\"}\n")), "x.jsonl", false, DefaultSampleLimits)
		require.Len(t, s.Rows, 3)
		for _, r := range s.Rows {
			require.LessOrEqual(t, len(r), DefaultSampleLimits.MaxRowBytes)
		}
		// A row under the limit as read can go over it once missing columns are added.
		lim := DefaultSampleLimits
		lim.MaxRowBytes = 20
		wide := sample(t, []byte("{\"a\":1}\n{\"b\":2,\"c\":3,\"d\":4}\n"), "x.jsonl", false, lim)
		require.Empty(t, wide.Rows)
	})
	t.Run("errors", func(t *testing.T) {
		_, err := Sample(context.Background(), strings.NewReader("{\"a\":1}\n{\"a\":x}\n"), "x.jsonl", FormatJSON, false, DefaultSampleLimits)
		requireKind(t, err, ErrMalformed, "x.jsonl", 2)
		_, err = Sample(context.Background(), strings.NewReader(`{"a":[{"x":1}],"b":[{"y":1}]}`), "x.json", FormatJSON, false, DefaultSampleLimits)
		requireKind(t, err, ErrAmbiguousJSON, "x.json", 0)
		_, err = Sample(context.Background(), strings.NewReader(`{"a":`+strings.Repeat("[", 70)), "x.json", FormatJSON, true, DefaultSampleLimits)
		require.NoError(t, err)
		_, err = Sample(context.Background(), strings.NewReader(`{"a":`+strings.Repeat("[", 70)+strings.Repeat("]", 70)+"}"), "x.json", FormatJSON, false, DefaultSampleLimits)
		requireKind(t, err, ErrTooDeep, "x.json", 1)
		_, err = Sample(context.Background(), strings.NewReader("PAR1"), "x.parquet", FormatParquet, false, DefaultSampleLimits)
		require.ErrorIs(t, err, ErrParquet)
		_, err = Sample(context.Background(), bytes.NewReader(append([]byte("PK\x03\x04"), body...)), "x.jsonl", FormatJSON, false, DefaultSampleLimits)
		require.ErrorIs(t, err, ErrCompression)
		_, err = Sample(context.Background(), bytes.NewReader([]byte("42")), "x.json", FormatJSON, false, DefaultSampleLimits)
		require.ErrorIs(t, err, ErrMalformed)
		_, err = Sample(context.Background(), bytes.NewReader(gzipBytes(t, []byte(body))[:30]), "x.jsonl.gz", FormatJSON, false, DefaultSampleLimits)
		require.ErrorIs(t, err, ErrMalformed)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = Sample(ctx, strings.NewReader(body), "x.jsonl", FormatJSON, false, DefaultSampleLimits)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// A body that breaks off (a reset connection) is not bad data: Sample
// returns the reader's error as it is, so the caller neither shows it to
// the customer nor names the file corrupt.
func TestSampleReadErrors(t *testing.T) {
	reset := errors.New("read tcp 10.0.12.34:51234->52.1.2.3:443: connection reset by peer")
	body := lines(200, "{\"id\":%d,\"name\":\"n%d\"}\n")
	gz := gzipBytes(t, []byte(body))
	for name, tc := range map[string]struct {
		file      string
		data      []byte
		truncated bool
	}{
		"json":            {"x.jsonl", []byte(body), false},
		"json range":      {"x.jsonl", []byte(body), true},
		"csv":             {"x.csv", []byte("id,name\n" + lines(200, "%d,n%d\n")), false},
		"gzip":            {"x.jsonl.gz", gz, false},
		"gzip range":      {"x.jsonl.gz", gz, true},
		"gzip header":     {"x.jsonl.gz", gz[:4], false},
		"zstd":            {"x.jsonl.zst", zstdBytes(t, []byte(body)), false},
		"before any data": {"x.jsonl", nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := FormatOf(tc.file)
			r := io.MultiReader(bytes.NewReader(tc.data[:len(tc.data)/2]), errReader{reset})
			_, err := Sample(context.Background(), r, tc.file, f, tc.truncated, DefaultSampleLimits)
			require.ErrorIs(t, err, reset)
			var e *Error
			require.False(t, errors.As(err, &e), "%v", err)
			require.False(t, isUnreadable(err), "%v", err)
		})
	}
}

func TestSampleCSV(t *testing.T) {
	body := "id,name,amount\n" + lines(20, "%d,n%d,1.5\n")
	t.Run("whole file", func(t *testing.T) {
		s := sample(t, []byte(body), "x.csv", false, DefaultSampleLimits)
		require.False(t, s.Truncated)
		require.Equal(t, []Column{{Name: "id", Type: TypeInteger}, {Name: "name", Type: TypeString}, {Name: "amount", Type: TypeFloat}}, s.Columns)
		require.Equal(t, `{"id":1,"name":"n1","amount":1.5}`, string(s.Rows[0]))
		require.Len(t, s.Rows, 5)
	})
	t.Run("cut drops the last line", func(t *testing.T) {
		s := sample(t, []byte("id,name\n1,a\n2,bb"), "x.csv", true, DefaultSampleLimits)
		require.True(t, s.Truncated)
		require.Equal(t, []string{`{"id":1,"name":"a"}`}, rowStrings(s.Rows))
		whole := sample(t, []byte("id,name\n1,a\n2,bb"), "x.csv", false, DefaultSampleLimits)
		require.Len(t, whole.Rows, 2)
	})
	t.Run("record cap", func(t *testing.T) {
		lim := DefaultSampleLimits
		lim.MaxRecords = 3
		s := sample(t, []byte(body), "x.csv", false, lim)
		require.True(t, s.Truncated)
		require.Len(t, s.Rows, 3)
	})
	t.Run("windows-1252", func(t *testing.T) {
		s := sample(t, []byte("caf\xe9\ncr\xe8me\n"), "x.csv", false, DefaultSampleLimits)
		require.Equal(t, []string{`{"café":"crème"}`}, rowStrings(s.Rows))
		cut := sample(t, []byte("a\nb\n\xc3"), "x.csv", true, DefaultSampleLimits)
		require.Equal(t, []string{`{"a":"b"}`}, rowStrings(cut.Rows))
	})
	t.Run("short and long rows", func(t *testing.T) {
		lim := DefaultSampleLimits
		lim.MaxRowBytes = 30
		s := sample(t, []byte("a,b\n1\n"+strings.Repeat("x", 40)+",2\n3,4\n"), "x.csv", false, lim)
		require.Equal(t, []string{`{"a":"1","b":null}`, `{"a":"3","b":4}`}, rowStrings(s.Rows))
		lim.MaxRowBytes = 12
		tight := sample(t, []byte("a,b\n1,2\n"), "x.csv", false, lim)
		require.Empty(t, tight.Rows)
	})
	t.Run("empty", func(t *testing.T) {
		s := sample(t, nil, "x.csv", false, DefaultSampleLimits)
		require.Empty(t, s.Columns)
		require.Empty(t, s.Rows)
	})
	t.Run("errors", func(t *testing.T) {
		_, err := Sample(context.Background(), strings.NewReader("a\n1,2\n"), "x.csv", FormatCSV, false, DefaultSampleLimits)
		requireKind(t, err, ErrExtraFields, "x.csv", 1)
		_, err = Sample(context.Background(), strings.NewReader(strings.Repeat("c,", 1000)+"c\n"), "x.csv", FormatCSV, false, DefaultSampleLimits)
		requireKind(t, err, ErrTooManyColumns, "x.csv", 0)
	})
}

func TestSampleKey(t *testing.T) {
	for name, tc := range map[string]struct {
		file, body string
		key        []string
	}{
		"json unique":    {"x.jsonl", "{\"id\":1}\n{\"id\":2}\n", []string{"id"}},
		"json repeated":  {"x.jsonl", "{\"id\":1}\n{\"id\":1}\n", nil},
		"json missing":   {"x.jsonl", "{\"id\":1}\n{\"x\":1}\n", nil},
		"csv unique":     {"x.csv", "id,name\n1,a\n2,b\n", []string{"id"}},
		"csv empty id":   {"x.csv", "id,name\n1,a\n,b\n", nil},
		"csv short row":  {"x.csv", "name,id\na,1\nb\n", nil},
		"csv without id": {"x.csv", "name\na\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.key, sample(t, []byte(tc.body), tc.file, false, DefaultSampleLimits).PrimaryKey)
		})
	}
}
