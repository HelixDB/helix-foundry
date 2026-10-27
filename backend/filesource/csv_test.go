package filesource

import (
	"context"
	"encoding/csv"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCSVReading(t *testing.T) {
	for name, tc := range map[string]struct {
		file string
		body string
		rows []string
	}{
		"bom and crlf":    {"x.csv", "\xef\xbb\xbfid,name\r\n1,a\r\n2,b\r\n", []string{`{"id":1,"name":"a"}`, `{"id":2,"name":"b"}`}},
		"quoted newlines": {"x.csv", "id,note\n1,\"two\nlines\"\n2,\"say \"\"hi\"\"\"\n", []string{`{"id":1,"note":"two\nlines"}`, `{"id":2,"note":"say \"hi\""}`}},
		"semicolons":      {"x.csv", "id;amount\n1;1,5\n2;2,5\n", []string{`{"id":1,"amount":"1,5"}`, `{"id":2,"amount":"2,5"}`}},
		"tabs":            {"x.csv", "id\tname\n1\ta b\n", []string{`{"id":1,"name":"a b"}`}},
		"pipes":           {"x.csv", "id|name\n1|a,b\n2|c\n", []string{`{"id":1,"name":"a,b"}`, `{"id":2,"name":"c"}`}},
		"excel sep":       {"x.csv", "sep=;\r\nid;name\r\n1;a,b\r\n", []string{`{"id":1,"name":"a,b"}`}},
		"excel sep lf":    {"x.csv", "sep=|\nid|name\n1|a\n", []string{`{"id":1,"name":"a"}`}},
		"tsv":             {"x.tsv", "id\tname\n1\ta;b,c|d\n", []string{`{"id":1,"name":"a;b,c|d"}`}},
		"single column":   {"x.csv", "name\na\nb\n", []string{`{"name":"a"}`, `{"name":"b"}`}},
		"header only":     {"x.csv", "id,name\n", nil},
		"empty":           {"x.csv", "", nil},
		"blank lines":     {"x.csv", "id\n\n1\n\n", []string{`{"id":1}`}},
		"short row":       {"x.csv", "a,b,c\n1\n2,3\n", []string{`{"a":1,"b":null,"c":null}`, `{"a":2,"b":3,"c":null}`}},
		"empty is null":   {"x.csv", "a,b\n,x\n1,\n", []string{`{"a":null,"b":"x"}`, `{"a":1,"b":null}`}},
		"no final eol":    {"x.csv", "a\n1", []string{`{"a":1}`}},
		"lazy quote":      {"x.csv", "a,b\n5\" screen,1\n", []string{`{"a":"5\" screen","b":1}`}},
	} {
		t.Run(name, func(t *testing.T) {
			r := scanAll(t, tempFile(t, tc.file, []byte(tc.body)))
			require.Equal(t, tc.rows, r.raw)
		})
	}
}

func TestCSVHeader(t *testing.T) {
	r := scanAll(t, tempFile(t, "x.csv", []byte(" id , ,name,name,,name_2\n1,2,3,4,5,6\n")))
	require.Equal(t, []string{"id", "column_2", "name", "name_2", "column_5", "name_2_2"}, colNames(r.cols))
	header := scanAll(t, tempFile(t, "y.csv", []byte("id,name\n")))
	require.Equal(t, []Column{{Name: "id"}, {Name: "name"}}, header.cols)
	empty := scanAll(t, tempFile(t, "z.csv", nil))
	require.Empty(t, empty.cols)
}

func TestCSVTypes(t *testing.T) {
	body := "b,i,f,mixed,zeros,big,neg,exp,word,boolint,dot,inf,nan\n" +
		"true,1,1.5,1,007,9223372036854775808,-5,1e5,x,true,.5,1e400,NaN\n" +
		"FALSE,-2,2,1.5,8,1,-0,-1.5E-3,y,1,5.,1,1\n" +
		",,,,,,,,,,,,\n"
	r := scanAll(t, tempFile(t, "x.csv", []byte(body)))
	require.Equal(t, map[string]Type{
		"b": TypeBoolean, "i": TypeInteger, "f": TypeFloat, "mixed": TypeFloat, "zeros": TypeString,
		"big": TypeString, "neg": TypeInteger, "exp": TypeFloat, "word": TypeString, "boolint": TypeString,
		"dot": TypeString, "inf": TypeString, "nan": TypeString,
	}, colTypes(r.cols))
	require.Equal(t, `{"b":true,"i":1,"f":1.5,"mixed":1,"zeros":"007","big":"9223372036854775808","neg":-5,"exp":1e5,"word":"x","boolint":"true","dot":".5","inf":"1e400","nan":"NaN"}`, r.raw[0])
	require.Equal(t, `{"b":false,"i":-2,"f":2,"mixed":1.5,"zeros":"8","big":"1","neg":-0,"exp":-1.5E-3,"word":"y","boolint":"1","dot":"5.","inf":"1","nan":"1"}`, r.raw[1])
	for _, c := range r.cols {
		require.True(t, c.Nullable, c.Name)
	}
	for _, v := range []string{"--5", "-", "1e", "1e+", "1.", "01.5", "1.5e5.5", "+1"} {
		require.Equal(t, TypeString, csvType(v), v)
	}
	require.Equal(t, TypeFloat, csvType("1e+5"))
	require.Equal(t, TypeFloat, csvType("0.0"))
}

func TestCSVTypesAcrossFiles(t *testing.T) {
	r := scanAll(t,
		tempFile(t, "a.csv", []byte("id,amount,flag\n1,5,true\n")),
		tempFile(t, "b.csv", []byte("id,amount,flag\n2,5.5,7\n")),
	)
	require.Equal(t, map[string]Type{"id": TypeInteger, "amount": TypeFloat, "flag": TypeString}, colTypes(r.cols))
	require.Equal(t, []string{`{"id":1,"amount":5,"flag":"true"}`, `{"id":2,"amount":5.5,"flag":"7"}`}, r.raw)
}

func TestCSVWindows1252(t *testing.T) {
	r := scanAll(t, tempFile(t, "x.csv", []byte("id,caf\xe9\n1,cr\xe8me \x80\n")))
	require.Equal(t, []string{"id", "café"}, colNames(r.cols))
	require.Equal(t, []string{`{"id":1,"café":"crème €"}`}, r.raw)
	// Invalid bytes in the header alone switch too.
	h := scanAll(t, tempFile(t, "y.csv", []byte("caf\xe9\n1\n")))
	require.Equal(t, []string{"café"}, colNames(h.cols))
}

func TestCSVErrors(t *testing.T) {
	t.Run("extra field", func(t *testing.T) {
		err := scanErr(t, DefaultLimits, tempFile(t, "x.csv", []byte("a,b\n1,2\n3,\"x\ny\"\n4,5,6\n")))
		requireKind(t, err, ErrExtraFields, "x.csv", 3)
		require.Equal(t, "x.csv record 3: line 5 has 3 fields; the header has 2", err.Error())
	})
	t.Run("too many columns", func(t *testing.T) {
		err := scanErr(t, DefaultLimits, tempFile(t, "x.csv", []byte(strings.Repeat("c,", 1000)+"c\n")))
		requireKind(t, err, ErrTooManyColumns, "x.csv", 0)
		ok := scanAll(t, tempFile(t, "y.csv", []byte(strings.Repeat("c,", 999)+"c\n")))
		require.Len(t, ok.cols, 1000)
	})
	t.Run("columns across files", func(t *testing.T) {
		lim := DefaultLimits
		lim.MaxColumns = 2
		err := scanErr(t, lim, tempFile(t, "a.csv", []byte("a,b\n1,2\n")), tempFile(t, "b.csv", []byte("c\n1\n")))
		requireKind(t, err, ErrTooManyColumns, "b.csv", 0)
	})
	t.Run("long line", func(t *testing.T) {
		// A line counts its line ending: 16 MiB with it reads, one more byte does not.
		fits := "a\n" + strings.Repeat("x", 16<<20-1) + "\n"
		r := scanAll(t, tempFile(t, "x.csv", []byte(fits)))
		require.Len(t, r.rows, 1)
		err := scanErr(t, DefaultLimits, tempFile(t, "x.csv", []byte("a\n1\n"+strings.Repeat("x", 16<<20)+"\n")))
		requireKind(t, err, ErrRecordTooLarge, "x.csv", 2)
		require.Contains(t, err.Error(), "a line over 16 MiB")
		header := scanErr(t, DefaultLimits, tempFile(t, "x.csv", []byte(strings.Repeat("x", 16<<20+1)+"\n")))
		requireKind(t, header, ErrRecordTooLarge, "x.csv", 0)
	})
}

func TestSniffDelimiter(t *testing.T) {
	for name, tc := range map[string]struct {
		prefix string
		atEOF  bool
		tsv    bool
		delim  rune
		skip   int
	}{
		"default comma":        {"abc\n", true, false, ',', 0},
		"comma":                {"a,b\n1,2\n", true, false, ',', 0},
		"semicolon wins":       {"a;b\n1,5;2\n3;4\n", true, false, ';', 0},
		"quoted delimiters":    {"\"a;b\",c\n\"1;2\",3\n", true, false, ',', 0},
		"partial last line":    {"a|b\n1|2\n3,4,5,6,7,8,9", false, false, '|', 0},
		"tsv":                  {"a,b\n", true, true, '\t', 0},
		"sep beats tsv":        {"sep=;\na;b\n", true, true, ';', 6},
		"sep at end":           {"sep=;", true, false, ';', 5},
		"sep quote is ignored": {"sep=\"\na\n", true, false, ',', 0},
		"sep with more text":   {"sep=;;\na\n", true, false, ';', 0},
		"sep invalid utf8":     {"sep=\xff\n", true, false, ',', 0},
	} {
		t.Run(name, func(t *testing.T) {
			d, skip := sniffDelimiter([]byte(tc.prefix), tc.atEOF, tc.tsv)
			require.Equal(t, tc.delim, d)
			require.Equal(t, tc.skip, skip)
		})
	}
	var many strings.Builder
	for range 30 {
		many.WriteString("a;b\n")
	}
	require.Len(t, splitLines([]byte(many.String()), false), sniffLines)
}

func TestCSVMore(t *testing.T) {
	t.Run("invalid utf-8 in a value only", func(t *testing.T) {
		r := scanAll(t, tempFile(t, "x.csv", []byte("id,name\n1,ok\n2,cr\xe8me\n")))
		require.Equal(t, []string{`{"id":1,"name":"ok"}`, `{"id":2,"name":"crème"}`}, r.raw)
	})
	t.Run("parse errors", func(t *testing.T) {
		br := newBudgetReader(context.Background(), strings.NewReader(""), newBudget(1), 1)
		err := csvError(&csv.ParseError{Line: 4, Err: csv.ErrQuote}, br)
		require.ErrorIs(t, err, ErrMalformed)
		require.Equal(t, "malformed CSV on line 4", err.Error())
		require.Equal(t, "malformed CSV", csvError(errors.New("odd"), br).Error())
	})
	t.Run("file changed between passes", func(t *testing.T) {
		f := tempFile(t, "x.csv", []byte("a\n1\n"))
		s, err := Scan(context.Background(), []File{f}, DefaultLimits)
		require.NoError(t, err)
		require.NoError(t, osWrite(f.Path, []byte(strings.Repeat("c,", 1000)+"c\n")))
		for _, err := range s.Rows(context.Background()) {
			require.ErrorIs(t, err, ErrTooManyColumns)
		}
	})
	t.Run("null id then values", func(t *testing.T) {
		r := scanAll(t, tempFile(t, "x.csv", []byte("id,x\n,a\n1,b\n2,c\n")))
		require.Nil(t, r.key)
	})
}
