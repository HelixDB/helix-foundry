package filesource

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/apache/thrift/lib/go/thrift"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

// bzip2Lines is bzip2 of two JSON lines: ids 1 and 2, names a and b.
// The standard library has no bzip2 writer.
var bzip2Lines = mustBase64("QlpoOTFBWSZTWYDnRaQAABJZgAAQEAQwEDYjAAogADFMAACVNAG1HqMkk2M0JSsioMjwypogZB8XckU4UJCA50Wk")

func mustBase64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// tempFile writes data to a fresh file and returns it as a table file
// named name, its format from the name.
func tempFile(t *testing.T, name string, data []byte, partitions ...Partition) File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	f, _ := FormatOf(name)
	return File{Path: path, Name: name, Format: f, Partitions: partitions}
}

func gzipBytes(t testing.TB, data ...[]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, d := range data {
		w := gzip.NewWriter(&out)
		_, err := w.Write(d)
		require.NoError(t, err)
		require.NoError(t, w.Close())
	}
	return out.Bytes()
}

func zstdBytes(t testing.TB, data []byte, opts ...zstd.EOption) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := zstd.NewWriter(&out, opts...)
	require.NoError(t, err)
	_, err = w.Write(data)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return out.Bytes()
}

// zstdFrameWindow is a zstd frame declaring a window of 1<<log bytes and
// holding one raw byte: the decoder sees the window before any data.
func zstdFrameWindow(log uint) []byte {
	return []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, byte((log - 10) << 3), 0x09, 0x00, 0x00, '{'}
}

// result is a scanned table read back.
type result struct {
	cols []Column
	key  []string
	rows []map[string]any
	raw  []string
	s    *Schema
}

// scanAll scans files and reads every row, checking that each row is one
// JSON object holding exactly Columns(), in order.
func scanAll(t *testing.T, files ...File) result {
	t.Helper()
	return scanWith(t, DefaultLimits, files...)
}

func scanWith(t *testing.T, lim Limits, files ...File) result {
	t.Helper()
	s, err := Scan(context.Background(), files, lim)
	require.NoError(t, err)
	r := result{cols: s.Columns(), key: s.PrimaryKey(), s: s}
	for row, err := range s.Rows(context.Background()) {
		require.NoError(t, err)
		r.raw = append(r.raw, string(row))
		r.rows = append(r.rows, decodeRow(t, row, r.cols))
	}
	return r
}

// decodeRow parses one row, checking it holds exactly cols in order.
func decodeRow(t *testing.T, row []byte, cols []Column) map[string]any {
	t.Helper()
	require.NotContains(t, string(row), "\n")
	dec := json.NewDecoder(bytes.NewReader(row))
	dec.UseNumber()
	tok, err := dec.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), tok)
	out := map[string]any{}
	for i := 0; dec.More(); i++ {
		k, err := dec.Token()
		require.NoError(t, err)
		require.Less(t, i, len(cols), "row %s has more keys than columns", row)
		require.Equal(t, cols[i].Name, k, "row %s", row)
		var v any
		require.NoError(t, dec.Decode(&v))
		out[k.(string)] = v
	}
	require.Len(t, out, len(cols), "row %s", row)
	return out
}

// scanErr is the first error scanning files and reading their rows.
func scanErr(t *testing.T, lim Limits, files ...File) error {
	t.Helper()
	s, err := Scan(context.Background(), files, lim)
	if err != nil {
		return err
	}
	for _, err := range s.Rows(context.Background()) {
		if err != nil {
			return err
		}
	}
	return nil
}

func colTypes(cols []Column) map[string]Type {
	out := map[string]Type{}
	for _, c := range cols {
		out[c.Name] = c.Type
	}
	return out
}

func colNames(cols []Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// anyRecord skips requireKind's record check.
const anyRecord = -1

// requireKind checks err is an *Error of kind naming file and record.
func requireKind(t *testing.T, err error, kind error, file string, record int64) {
	t.Helper()
	require.ErrorIs(t, err, kind)
	var e *Error
	require.True(t, errors.As(err, &e), "%v is not an *Error", err)
	require.Equal(t, file, e.File)
	if record != anyRecord {
		require.Equal(t, record, e.Record, "%v", err)
	}
}

// peakHeap runs fn and returns how far the heap rose above where it
// started, sampled every millisecond. The collector runs often meanwhile,
// so the peak is close to live memory rather than garbage awaiting a cycle.
func peakHeap(t *testing.T, fn func()) uint64 {
	t.Helper()
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > base && m.HeapAlloc-base > peak.Load() {
				peak.Store(m.HeapAlloc - base)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	fn()
	close(stop)
	wg.Wait()
	return peak.Load()
}

// repeatReader yields chunk forever after head, up to n bytes in all.
type repeatReader struct {
	head  []byte
	chunk []byte
	left  int64
	off   int
}

func repeat(head []byte, chunk string, n int64) *repeatReader {
	return &repeatReader{head: head, chunk: []byte(chunk), left: n}
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n := 0
	for n < len(p) {
		if len(r.head) > 0 {
			c := copy(p[n:], r.head)
			r.head = r.head[c:]
			n += c
			continue
		}
		c := copy(p[n:], r.chunk[r.off:])
		r.off = (r.off + c) % len(r.chunk)
		n += c
	}
	r.left -= int64(n)
	return n, nil
}

// parquetBytes writes records as one Parquet file.
func parquetBytes(t testing.TB, sc *arrow.Schema, props *parquet.WriterProperties, aprops pqarrow.ArrowWriterProperties, recs ...arrow.RecordBatch) []byte {
	t.Helper()
	var buf bytes.Buffer
	if props == nil {
		props = parquet.NewWriterProperties()
	}
	w, err := pqarrow.NewFileWriter(sc, &buf, props, aprops)
	require.NoError(t, err)
	for _, r := range recs {
		require.NoError(t, w.Write(r))
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// craftedFooter writes a Parquet footer field by field, as an attacker
// could: columns INT32 leaves under the root, each inside depth groups of
// one child. rowGroupsFirst puts the (empty) row group list before the
// schema, which thrift allows and no writer does.
func craftedFooter(t testing.TB, columns, depth int, rowGroupsFirst bool) []byte {
	t.Helper()
	ctx := context.Background()
	buf := thrift.NewTMemoryBuffer()
	p := thrift.NewTCompactProtocolConf(buf, nil)
	must := func(err error) { require.NoError(t, err) }
	field := func(typ thrift.TType, id int16, write func()) {
		must(p.WriteFieldBegin(ctx, "", typ, id))
		write()
		must(p.WriteFieldEnd(ctx))
	}
	node := func(children int32, name string) {
		must(p.WriteStructBegin(ctx, ""))
		if children == 0 {
			field(thrift.I32, 1, func() { must(p.WriteI32(ctx, 1)) }) // type INT32
		}
		field(thrift.STRING, 4, func() { must(p.WriteString(ctx, name)) })
		if children > 0 {
			field(thrift.I32, 5, func() { must(p.WriteI32(ctx, children)) })
		}
		must(p.WriteFieldStop(ctx))
		must(p.WriteStructEnd(ctx))
	}
	rowGroups := func() {
		field(thrift.LIST, 4, func() {
			must(p.WriteListBegin(ctx, thrift.STRUCT, 0))
			must(p.WriteListEnd(ctx))
		})
	}
	must(p.WriteStructBegin(ctx, ""))
	field(thrift.I32, 1, func() { must(p.WriteI32(ctx, 1)) })
	if rowGroupsFirst {
		rowGroups()
	}
	field(thrift.LIST, 2, func() {
		must(p.WriteListBegin(ctx, thrift.STRUCT, 1+columns*(depth+1)))
		node(int32(columns), "schema")
		for c := range columns {
			for range depth {
				node(1, "g")
			}
			node(0, strconv.FormatInt(int64(c), 36))
		}
		must(p.WriteListEnd(ctx))
	})
	field(thrift.I64, 3, func() { must(p.WriteI64(ctx, 0)) })
	if !rowGroupsFirst {
		rowGroups()
	}
	must(p.WriteFieldStop(ctx))
	must(p.WriteStructEnd(ctx))
	must(p.Flush(ctx))
	return buf.Bytes()
}

// parquetWithFooter frames a footer as a Parquet file with no data.
func parquetWithFooter(footer []byte) []byte {
	out := append([]byte("PAR1"), footer...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(footer)))
	return append(out, "PAR1"...)
}

func osWrite(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }

func removeFile(path string) error { return os.Remove(path) }

// esc writes JSON \u escapes as ~u, since some editors turn escape text
// into the characters it names.
var esc = strings.NewReplacer("~u", "\\u").Replace

// replacement is U+FFFD as normalize writes it.
const replacement = string(utf8.RuneError)
