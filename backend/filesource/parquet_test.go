package filesource

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/extensions"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/apache/arrow-go/v18/parquet/schema"
	"github.com/stretchr/testify/require"
)

func jsonRecord(t testing.TB, sc *arrow.Schema, rows string) arrow.RecordBatch {
	t.Helper()
	rec, _, err := array.RecordFromJSON(memory.DefaultAllocator, sc, strings.NewReader(rows))
	require.NoError(t, err)
	return rec
}

func col(name string, dt arrow.DataType) arrow.Field {
	return arrow.Field{Name: name, Type: dt, Nullable: true}
}

// typesSchema covers every Arrow type a Parquet writer produces.
func typesSchema(t testing.TB) *arrow.Schema {
	jsonType, err := extensions.NewJSONType(arrow.BinaryTypes.String)
	require.NoError(t, err)
	return arrow.NewSchema([]arrow.Field{
		col("b", arrow.FixedWidthTypes.Boolean),
		col("i8", arrow.PrimitiveTypes.Int8),
		col("i16", arrow.PrimitiveTypes.Int16),
		col("i32", arrow.PrimitiveTypes.Int32),
		col("i64", arrow.PrimitiveTypes.Int64),
		col("u8", arrow.PrimitiveTypes.Uint8),
		col("u16", arrow.PrimitiveTypes.Uint16),
		col("u32", arrow.PrimitiveTypes.Uint32),
		col("u64", arrow.PrimitiveTypes.Uint64),
		col("f16", arrow.FixedWidthTypes.Float16),
		col("f32", arrow.PrimitiveTypes.Float32),
		col("f64", arrow.PrimitiveTypes.Float64),
		col("d10", &arrow.Decimal128Type{Precision: 10, Scale: 2}),
		col("d20", &arrow.Decimal128Type{Precision: 20, Scale: 4}),
		col("d40", &arrow.Decimal256Type{Precision: 40, Scale: 1}),
		col("date", arrow.FixedWidthTypes.Date32),
		col("t32", arrow.FixedWidthTypes.Time32ms),
		col("t64us", arrow.FixedWidthTypes.Time64us),
		col("t64ns", arrow.FixedWidthTypes.Time64ns),
		col("tstz", &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}),
		col("ts", &arrow.TimestampType{Unit: arrow.Millisecond}),
		col("far", &arrow.TimestampType{Unit: arrow.Second}),
		col("old", arrow.FixedWidthTypes.Date32),
		col("s", arrow.BinaryTypes.String),
		col("ls", arrow.BinaryTypes.LargeString),
		col("bin", arrow.BinaryTypes.Binary),
		col("lbin", arrow.BinaryTypes.LargeBinary),
		col("fsb", &arrow.FixedSizeBinaryType{ByteWidth: 3}),
		col("uuid", extensions.NewUUIDType()),
		col("js", jsonType),
		col("list", arrow.ListOf(arrow.PrimitiveTypes.Int32)),
		col("llist", arrow.LargeListOf(arrow.BinaryTypes.String)),
		col("fsl", arrow.FixedSizeListOf(2, arrow.PrimitiveTypes.Int32)),
		col("st", arrow.StructOf(col("a", arrow.PrimitiveTypes.Int32), col("b", arrow.BinaryTypes.String))),
		col("los", arrow.ListOf(arrow.StructOf(col("x", arrow.PrimitiveTypes.Int32)))),
		col("mstr", arrow.MapOf(arrow.BinaryTypes.String, arrow.PrimitiveTypes.Int32)),
		col("mint", arrow.MapOf(arrow.PrimitiveTypes.Int32, arrow.BinaryTypes.String)),
		col("dict", &arrow.DictionaryType{IndexType: arrow.PrimitiveTypes.Int32, ValueType: arrow.BinaryTypes.String}),
		col("nul", arrow.Null),
	}, nil)
}

const typesRows = `[
{"b":true,"i8":-8,"i16":-16,"i32":-32,"i64":-64,"u8":8,"u16":16,"u32":32,"u64":18446744073709551615,
 "f16":1.5,"f32":2.25,"f64":3.125,"d10":"12.30","d20":"1234567890123456.7890","d40":"123456789012345678901234567890123456789.5",
 "date":"2026-10-01","t32":"13:04:05.123","t64us":"13:04:05.000001","t64ns":"00:00:00",
 "tstz":"2026-10-01T12:00:00.123456Z","ts":"2026-10-01T12:00:00.120","far":253402300800,"old":-719528,
 "s":"héllo","ls":"x","bin":"AAEC","lbin":"","fsb":"AQID","uuid":"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
 "js":"{\"k\": [1, 2]}","list":[1,null,3],"llist":["a"],"fsl":[1,2],"st":{"a":1,"b":"x"},"los":[{"x":1},null],
 "mstr":[{"key":"k1","value":1},{"key":"k2","value":2}],"mint":[{"key":1,"value":"a"}],"dict":"red","nul":null},
{"b":false,"f32":"NaN","f64":"-Inf","js":"not json","st":null,"far":-62135596800,"old":-719162,"u64":5,"list":[]}
]`

var typesWant = []string{
	`{"b":true,"i8":-8,"i16":-16,"i32":-32,"i64":-64,"u8":8,"u16":16,"u32":32,"u64":"18446744073709551615",` +
		`"f16":1.5,"f32":2.25,"f64":3.125,"d10":12.30,"d20":"1234567890123456.7890","d40":"123456789012345678901234567890123456789.5",` +
		`"date":"2026-10-01","t32":"13:04:05.123","t64us":"13:04:05.000001","t64ns":"00:00:00",` +
		`"tstz":"2026-10-01T12:00:00.123456Z","ts":"2026-10-01T12:00:00.12","far":null,"old":null,` +
		`"s":"héllo","ls":"x","bin":"AAEC","lbin":"","fsb":"AQID","uuid":"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",` +
		`"js":{"k":[1,2]},"list":[1,null,3],"llist":["a"],"fsl":[1,2],"st":{"a":1,"b":"x"},"los":[{"x":1},null],` +
		`"mstr":{"k1":1,"k2":2},"mint":[{"key":1,"value":"a"}],"dict":"red","nul":null}`,
	`{"b":false,"i8":null,"i16":null,"i32":null,"i64":null,"u8":null,"u16":null,"u32":null,"u64":5,` +
		`"f16":null,"f32":null,"f64":null,"d10":null,"d20":null,"d40":null,` +
		`"date":null,"t32":null,"t64us":null,"t64ns":null,"tstz":null,"ts":null,"far":"0001-01-01T00:00:00","old":"0001-01-01",` +
		`"s":null,"ls":null,"bin":null,"lbin":null,"fsb":null,"uuid":null,` +
		`"js":"not json","list":[],"llist":null,"fsl":null,"st":null,"los":null,"mstr":null,"mint":null,"dict":null,"nul":null}`,
}

var typesTypes = map[string]Type{
	"b": TypeBoolean, "i8": TypeInteger, "i16": TypeInteger, "i32": TypeInteger, "i64": TypeInteger, "u8": TypeInteger,
	"u16": TypeInteger, "u32": TypeInteger, "u64": TypeInteger, "f16": TypeFloat, "f32": TypeFloat, "f64": TypeFloat,
	"d10": TypeDecimal, "d20": TypeDecimal, "d40": TypeDecimal, "date": TypeDate, "t32": TypeTime, "t64us": TypeTime,
	"t64ns": TypeTime, "tstz": TypeTimestampTZ, "ts": TypeTimestamp, "far": TypeTimestamp, "old": TypeDate,
	"s": TypeString, "ls": TypeString, "bin": TypeBinary, "lbin": TypeBinary, "fsb": TypeBinary, "uuid": TypeString,
	"js": TypeJSON, "list": TypeJSON, "llist": TypeJSON, "fsl": TypeJSON, "st": TypeJSON, "los": TypeJSON,
	"mstr": TypeJSON, "mint": TypeJSON, "dict": TypeString, "nul": TypeNull,
}

func TestParquetTypes(t *testing.T) {
	sc := typesSchema(t)
	for name, aprops := range map[string]pqarrow.ArrowWriterProperties{
		"parquet types": pqarrow.DefaultWriterProps(),
		"arrow schema":  pqarrow.NewArrowWriterProperties(pqarrow.WithStoreSchema()),
	} {
		t.Run(name, func(t *testing.T) {
			data := parquetBytes(t, sc, nil, aprops, jsonRecord(t, sc, typesRows))
			r := scanAll(t, tempFile(t, "x.parquet", data))
			require.Equal(t, typesWant, r.raw)
			require.Equal(t, typesTypes, colTypes(r.cols))
			require.Equal(t, map[string]int64{"timestamp_range": 2, "non_finite": 2}, r.s.Nulled())

			cols, rows, err := ParquetFooter(context.Background(), bytes.NewReader(data), int64(len(data)), DefaultSampleLimits)
			require.NoError(t, err)
			require.Equal(t, int64(2), rows)
			require.Equal(t, r.cols, cols)
		})
	}
}

// INTERVAL is a converted type on FIXED_LEN_BYTE_ARRAY(12), which arrow
// reads as plain fixed-size binary; Helix writes it as a duration.
func TestParquetInterval(t *testing.T) {
	node, err := schema.NewPrimitiveNodeConverted("span", parquet.Repetitions.Optional, parquet.Types.FixedLenByteArray,
		schema.ConvertedTypes.Interval, 12, 0, 0, -1)
	require.NoError(t, err)
	root, err := schema.NewGroupNode("schema", parquet.Repetitions.Required, schema.FieldList{node}, -1)
	require.NoError(t, err)
	interval := func(months, days, millis uint32) parquet.FixedLenByteArray {
		v := binary.LittleEndian.AppendUint32(nil, months)
		v = binary.LittleEndian.AppendUint32(v, days)
		return binary.LittleEndian.AppendUint32(v, millis)
	}
	var buf bytes.Buffer
	w := file.NewParquetWriter(&buf, root)
	rg, err := w.AppendRowGroupChecked()
	require.NoError(t, err)
	cw, err := rg.NextColumn()
	require.NoError(t, err)
	_, err = cw.(*file.FixedLenByteArrayColumnChunkWriter).WriteBatch(
		[]parquet.FixedLenByteArray{interval(1, 2, 3500), interval(0, 0, 0), interval(math.MaxUint32, 0, 1)}, []int16{1, 1, 0, 1}, nil)
	require.NoError(t, err)
	require.NoError(t, cw.Close())
	require.NoError(t, rg.Close())
	require.NoError(t, w.Close())

	r := scanAll(t, tempFile(t, "x.parquet", buf.Bytes()))
	require.Equal(t, []Column{{Name: "span", Type: TypeString, Nullable: true}}, r.cols)
	require.Equal(t, []string{`{"span":"P1M2DT3.5S"}`, `{"span":"PT0S"}`, `{"span":null}`, `{"span":"P4294967295MT0.001S"}`}, r.raw)
	cols, _, err := ParquetFooter(context.Background(), bytes.NewReader(buf.Bytes()), int64(buf.Len()), DefaultSampleLimits)
	require.NoError(t, err)
	require.Equal(t, r.cols, cols)
}

func TestParquetInt96(t *testing.T) {
	sc := arrow.NewSchema([]arrow.Field{col("ts", arrow.FixedWidthTypes.Timestamp_ns)}, nil)
	rec := jsonRecord(t, sc, `[{"ts":"2026-10-01T12:00:00.5Z"},{"ts":null}]`)
	data := parquetBytes(t, sc, nil, pqarrow.NewArrowWriterProperties(pqarrow.WithDeprecatedInt96Timestamps(true)), rec)
	r := scanAll(t, tempFile(t, "x.parquet", data))
	require.Equal(t, []string{`{"ts":"2026-10-01T12:00:00.5Z"}`, `{"ts":null}`}, r.raw)
	require.Equal(t, TypeTimestampTZ, r.cols[0].Type)
}

func TestParquetCodecsAndRowGroups(t *testing.T) {
	sc := arrow.NewSchema([]arrow.Field{col("id", arrow.PrimitiveTypes.Int64), col("name", arrow.BinaryTypes.String)}, nil)
	var rows strings.Builder
	rows.WriteString("[")
	for i := range 35 {
		if i > 0 {
			rows.WriteString(",")
		}
		fmt.Fprintf(&rows, `{"id":%d,"name":"n%d"}`, i, i)
	}
	rows.WriteString("]")
	for _, codec := range []compress.Compression{compress.Codecs.Snappy, compress.Codecs.Gzip, compress.Codecs.Zstd, compress.Codecs.Brotli, compress.Codecs.Lz4Raw, compress.Codecs.Uncompressed} {
		t.Run(codec.String(), func(t *testing.T) {
			props := parquet.NewWriterProperties(parquet.WithCompression(codec), parquet.WithMaxRowGroupLength(10))
			data := parquetBytes(t, sc, props, pqarrow.DefaultWriterProps(), jsonRecord(t, sc, rows.String()))
			r := scanAll(t, tempFile(t, "x.parquet", data))
			require.Len(t, r.rows, 35)
			require.Equal(t, `{"id":34,"name":"n34"}`, r.raw[34])
			require.Equal(t, []string{"id"}, r.key)
		})
	}
}

func TestParquetEmptyAndErrors(t *testing.T) {
	sc := arrow.NewSchema([]arrow.Field{col("id", arrow.PrimitiveTypes.Int64)}, nil)
	good := parquetBytes(t, sc, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, sc, `[{"id":1},{"id":2}]`))
	t.Run("no rows", func(t *testing.T) {
		data := parquetBytes(t, sc, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, sc, `[]`))
		r := scanAll(t, tempFile(t, "x.parquet", data))
		require.Empty(t, r.rows)
		require.Equal(t, []Column{{Name: "id", Type: TypeInteger, Nullable: true}}, r.cols)
	})
	corrupt := bytes.Clone(good)
	footer := int(corrupt[len(corrupt)-8]) | int(corrupt[len(corrupt)-7])<<8
	for i := len(corrupt) - 8 - footer; i < len(corrupt)-8; i++ {
		corrupt[i] = 0xff
	}
	encrypted := bytes.Clone(good)
	copy(encrypted[len(encrypted)-4:], "PARE")
	hugeFooter := bytes.Clone(good)
	copy(hugeFooter[len(hugeFooter)-8:], []byte{0, 0, 0, 2})
	for name, tc := range map[string]struct {
		data []byte
		kind error
		msg  string
	}{
		"corrupt footer":     {corrupt, ErrParquet, "unreadable Parquet footer"},
		"truncated":          {good[:len(good)-30], ErrParquet, "not a Parquet file, or cut short"},
		"csv named parquet":  {[]byte("id,name\n1,a\n2,b\n"), ErrParquet, "not a Parquet file"},
		"too short":          {[]byte("PAR1PAR1"), ErrParquet, "too short to be a Parquet file"},
		"encrypted":          {encrypted, ErrParquet, "encrypted Parquet files are not supported"},
		"footer over 16 MiB": {hugeFooter, ErrParquet, "the Parquet footer is over 16 MiB"},
		"footer past start":  {append([]byte("PAR1"), []byte{0xff, 0, 0, 0, 'P', 'A', 'R', '1'}...), ErrParquet, "cut short"},
		"bad head":           {append([]byte("XXXX"), good[4:]...), ErrParquet, "not a Parquet file"},
		"gzip parquet":       {gzipBytes(t, good), ErrCompression, "a compressed Parquet file"},
	} {
		t.Run(name, func(t *testing.T) {
			err := scanErr(t, DefaultLimits, tempFile(t, "x.parquet", tc.data))
			requireKind(t, err, tc.kind, "x.parquet", 0)
			require.Contains(t, err.Error(), tc.msg)
		})
	}
	t.Run("missing file", func(t *testing.T) {
		_, err := Scan(context.Background(), []File{{Path: "/nonexistent/x", Name: "x.parquet", Format: FormatParquet}}, DefaultLimits)
		require.Error(t, err)
	})
	t.Run("file changed between passes", func(t *testing.T) {
		f := tempFile(t, "x.parquet", good)
		s, err := Scan(context.Background(), []File{f}, DefaultLimits)
		require.NoError(t, err)
		require.NoError(t, osWrite(f.Path, []byte("garbage")))
		for _, err := range s.Rows(context.Background()) {
			require.ErrorIs(t, err, ErrParquet)
		}
	})
}

func TestParquetLimits(t *testing.T) {
	t.Run("leaf columns", func(t *testing.T) {
		fields := make([]arrow.Field, 1001)
		for i := range fields {
			fields[i] = col(fmt.Sprintf("c%d", i), arrow.PrimitiveTypes.Int32)
		}
		sc := arrow.NewSchema(fields, nil)
		data := parquetBytes(t, sc, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, sc, `[{"c0":1}]`))
		err := scanErr(t, DefaultLimits, tempFile(t, "x.parquet", data))
		requireKind(t, err, ErrTooManyColumns, "x.parquet", 0)
		_, _, err = ParquetFooter(context.Background(), bytes.NewReader(data), int64(len(data)), DefaultSampleLimits)
		require.ErrorIs(t, err, ErrTooManyColumns)
	})
	t.Run("row group estimate", func(t *testing.T) {
		sc := arrow.NewSchema([]arrow.Field{col("a", arrow.PrimitiveTypes.Int32), col("b", arrow.PrimitiveTypes.Int32)}, nil)
		data := parquetBytes(t, sc, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, sc, `[{"a":1,"b":2}]`))
		lim := DefaultLimits
		lim.MaxRowGroupBytes = parquetStreamBuffer
		err := scanErr(t, lim, tempFile(t, "x.parquet", data))
		requireKind(t, err, ErrParquet, "x.parquet", 0)
		require.Contains(t, err.Error(), "row group 1 needs about 0 MiB to read; write smaller row groups or fewer columns")
	})
	t.Run("nesting", func(t *testing.T) {
		var dt arrow.DataType = arrow.PrimitiveTypes.Int32
		for range 64 {
			dt = arrow.ListOf(dt)
		}
		sc := arrow.NewSchema([]arrow.Field{col("deep", dt)}, nil)
		data := parquetBytes(t, sc, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, sc, `[{"deep":null}]`))
		err := scanErr(t, DefaultLimits, tempFile(t, "x.parquet", data))
		requireKind(t, err, ErrTooDeep, "x.parquet", 0)
		require.Contains(t, err.Error(), "column deep is nested more than 64 levels deep")
	})
}

func TestParquetFiles(t *testing.T) {
	a := arrow.NewSchema([]arrow.Field{col("id", arrow.PrimitiveTypes.Int32), col("v", arrow.PrimitiveTypes.Int32)}, nil)
	b := arrow.NewSchema([]arrow.Field{col("id", arrow.PrimitiveTypes.Int64), col("v", arrow.PrimitiveTypes.Float64), col("w", arrow.BinaryTypes.String)}, nil)
	fa := tempFile(t, "a.parquet", parquetBytes(t, a, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, a, `[{"id":1,"v":1}]`)))
	fb := tempFile(t, "b.parquet", parquetBytes(t, b, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, b, `[{"id":2,"v":1.5,"w":"x"}]`)))
	r := scanAll(t, fa, fb)
	require.Equal(t, []Column{{Name: "id", Type: TypeInteger, Nullable: true}, {Name: "v", Type: TypeFloat, Nullable: true}, {Name: "w", Type: TypeString, Nullable: true}}, r.cols)
	require.Equal(t, []string{`{"id":1,"v":1,"w":null}`, `{"id":2,"v":1.5,"w":"x"}`}, r.raw)
	require.Equal(t, []string{"id"}, r.key)

	dup := tempFile(t, "c.parquet", parquetBytes(t, a, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, a, `[{"id":1,"v":3}]`)))
	require.Nil(t, scanAll(t, fa, dup).key)
	nullID := tempFile(t, "d.parquet", parquetBytes(t, a, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, a, `[{"id":3},{"id":null}]`)))
	require.Nil(t, scanAll(t, fa, nullID).key)
	noID := arrow.NewSchema([]arrow.Field{col("v", arrow.PrimitiveTypes.Int32)}, nil)
	missing := tempFile(t, "e.parquet", parquetBytes(t, noID, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, noID, `[{"v":3}]`)))
	require.Nil(t, scanAll(t, fa, missing).key)
	emptyNoID := tempFile(t, "f.parquet", parquetBytes(t, noID, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, noID, `[]`)))
	require.Equal(t, []string{"id"}, scanAll(t, fa, emptyNoID).key)
	nested := arrow.NewSchema([]arrow.Field{col("id", arrow.StructOf(col("k", arrow.PrimitiveTypes.Int32)))}, nil)
	structID := tempFile(t, "g.parquet", parquetBytes(t, nested, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, nested, `[{"id":{"k":1}}]`)))
	require.Nil(t, scanAll(t, structID).key)
}

// countingReaderAt records the bytes and lowest offset read.
type countingReaderAt struct {
	r     io.ReaderAt
	bytes atomic.Int64
	low   atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.bytes.Add(int64(len(p)))
	if c.low.Load() == 0 || off < c.low.Load() {
		c.low.Store(off)
	}
	return c.r.ReadAt(p, off)
}

func TestParquetFooter(t *testing.T) {
	sc := arrow.NewSchema([]arrow.Field{col("id", arrow.PrimitiveTypes.Int64), col("payload", arrow.BinaryTypes.String)}, nil)
	data := parquetBytes(t, sc, nil, pqarrow.DefaultWriterProps(), jsonRecord(t, sc, `[{"id":1,"payload":"`+strings.Repeat("x", 100000)+`"}]`))
	footer := int64(int(data[len(data)-8]) | int(data[len(data)-7])<<8 | int(data[len(data)-6])<<16)
	c := &countingReaderAt{r: bytes.NewReader(data)}
	cols, rows, err := ParquetFooter(context.Background(), c, int64(len(data)), DefaultSampleLimits)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)
	require.Equal(t, []string{"id", "payload"}, colNames(cols))
	require.Equal(t, footer+8, c.bytes.Load())
	require.Equal(t, int64(len(data))-8-footer, c.low.Load())

	lim := DefaultSampleLimits
	lim.MaxFooter = footer - 1
	_, _, err = ParquetFooter(context.Background(), bytes.NewReader(data), int64(len(data)), lim)
	require.ErrorIs(t, err, ErrSampleBudget)
	_, _, err = ParquetFooter(context.Background(), bytes.NewReader([]byte("nope")), 4, lim)
	require.ErrorIs(t, err, ErrParquet)
	garbled := bytes.Clone(data)
	for i := len(garbled) - 8 - int(footer); i < len(garbled)-8; i++ {
		garbled[i] = 0xff
	}
	_, _, err = ParquetFooter(context.Background(), bytes.NewReader(garbled), int64(len(garbled)), DefaultSampleLimits)
	require.ErrorIs(t, err, ErrParquet)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = ParquetFooter(ctx, bytes.NewReader(data), int64(len(data)), DefaultSampleLimits)
	require.ErrorIs(t, err, context.Canceled)
	_, _, err = ParquetFooter(context.Background(), errReaderAt{}, 100, DefaultSampleLimits)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	tail := append(bytes.Repeat([]byte{0}, 20), 4, 0, 0, 0, 'P', 'A', 'R', '1')
	_, _, err = ParquetFooter(context.Background(), &failAfter{r: bytes.NewReader(tail), n: 1}, int64(len(tail)), DefaultSampleLimits)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

// TestParquetFooterSchema refuses a footer by its schema list before
// decoding it, which costs about 750 bytes of heap a node.
func TestParquetFooterSchema(t *testing.T) {
	ctx := context.Background()
	footerErr := func(footer []byte) error {
		data := parquetWithFooter(footer)
		_, _, err := ParquetFooter(ctx, bytes.NewReader(data), int64(len(data)), DefaultSampleLimits)
		return err
	}
	t.Run("many columns, in little memory", func(t *testing.T) {
		footer := craftedFooter(t, 100_000, 0, false)
		require.Less(t, len(footer), 1<<20, "within the sample's footer budget")
		var err error
		heap := peakHeap(t, func() { err = footerErr(footer) })
		require.ErrorIs(t, err, ErrTooManyColumns)
		require.Equal(t, "more than 1000 columns", err.Error())
		require.Less(t, heap, uint64(8<<20))
		err = scanErr(t, DefaultLimits, tempFile(t, "x.parquet", parquetWithFooter(footer)))
		requireKind(t, err, ErrTooManyColumns, "x.parquet", 0)
	})
	t.Run("deep groups", func(t *testing.T) {
		err := footerErr(craftedFooter(t, 1, 8000, false))
		require.ErrorIs(t, err, ErrTooManyColumns)
		require.Equal(t, "more than 8001 schema fields, nested groups included", err.Error())
		err = scanErr(t, DefaultLimits, tempFile(t, "x.parquet", parquetWithFooter(craftedFooter(t, 1, 8000, false))))
		requireKind(t, err, ErrTooManyColumns, "x.parquet", 0)
		require.NoError(t, checkFooterSchema(bytes.NewReader(craftedFooter(t, 1000, 7, false)), 1000), "eight nodes a column pass")
	})
	t.Run("fields in any order", func(t *testing.T) {
		data := parquetWithFooter(craftedFooter(t, 3, 0, true))
		cols, rows, err := ParquetFooter(ctx, bytes.NewReader(data), int64(len(data)), DefaultSampleLimits)
		require.NoError(t, err)
		require.Equal(t, int64(0), rows)
		require.Equal(t, []string{"0", "1", "2"}, colNames(cols))
	})
	t.Run("cut short", func(t *testing.T) {
		footer := craftedFooter(t, 2, 1, true)
		refused := 0
		for n := range len(footer) {
			err := checkFooterSchema(bytes.NewReader(footer[:n]), 1000)
			if err != nil {
				require.ErrorIs(t, err, ErrParquet, "cut at %d", n)
				refused++
			}
		}
		require.Greater(t, refused, len(footer)/2)
		require.NoError(t, checkFooterSchema(bytes.NewReader([]byte{0}), 1000), "a footer without a schema is left to the decoder")
	})
}

type errReaderAt struct{}

func (errReaderAt) ReadAt([]byte, int64) (int, error) { return 0, io.ErrUnexpectedEOF }

// failAfter serves n reads, then fails.
type failAfter struct {
	r io.ReaderAt
	n int
}

func (f *failAfter) ReadAt(p []byte, off int64) (int, error) {
	if f.n == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	f.n--
	return f.r.ReadAt(p, off)
}

// TestParquetMemory reads files built to need a lot of memory and checks
// the heap stays bounded.
func TestParquetMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("writes hundreds of megabytes")
	}
	rng := rand.New(rand.NewPCG(1, 2))
	randomText := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = 'a' + byte(rng.IntN(26))
		}
		return string(b)
	}
	read := func(t *testing.T, f File) int {
		n := 0
		grew := peakHeap(t, func() {
			s, err := Scan(context.Background(), []File{f}, DefaultLimits)
			require.NoError(t, err)
			for _, err := range s.Rows(context.Background()) {
				require.NoError(t, err)
				n++
			}
		})
		t.Logf("heap grew %d MiB", grew>>20)
		require.Less(t, grew, uint64(512<<20))
		return n
	}
	t.Run("1000 leaf columns", func(t *testing.T) {
		fields := make([]arrow.Field, 1000)
		for i := range fields {
			fields[i] = col(fmt.Sprintf("c%d", i), arrow.PrimitiveTypes.Int64)
		}
		sc := arrow.NewSchema(fields, nil)
		b := array.NewRecordBuilder(memory.DefaultAllocator, sc)
		for _, fb := range b.Fields() {
			for r := range 5000 {
				fb.(*array.Int64Builder).Append(int64(r))
			}
		}
		rec := b.NewRecordBatch()
		b.Release()
		f := tempFile(t, "x.parquet", parquetBytes(t, sc, nil, pqarrow.DefaultWriterProps(), rec))
		require.Equal(t, 5000, read(t, f))
	})
	t.Run("8 MiB strings", func(t *testing.T) {
		sc := arrow.NewSchema([]arrow.Field{col("s", arrow.BinaryTypes.String)}, nil)
		b := array.NewRecordBuilder(memory.DefaultAllocator, sc)
		for range 20 {
			b.Field(0).(*array.StringBuilder).Append(randomText(8 << 20))
		}
		rec := b.NewRecordBatch()
		b.Release()
		// One value per page keeps each page under the 32 MiB page limit.
		props := parquet.NewWriterProperties(parquet.WithDictionaryDefault(false), parquet.WithBatchSize(1), parquet.WithDataPageSize(1<<20))
		f := tempFile(t, "x.parquet", parquetBytes(t, sc, props, pqarrow.DefaultWriterProps(), rec))
		require.Equal(t, 20, read(t, f))
		// Written as one page, the values are over it.
		big := tempFile(t, "y.parquet", parquetBytes(t, sc, parquet.NewWriterProperties(parquet.WithDictionaryDefault(false)), pqarrow.DefaultWriterProps(), rec))
		err := scanErr(t, DefaultLimits, big)
		requireKind(t, err, ErrParquet, "y.parquet", 0)
		require.Contains(t, err.Error(), "unreadable Parquet data")
	})
	t.Run("200 MB file, 64 MB row groups", func(t *testing.T) {
		sc := arrow.NewSchema([]arrow.Field{col("id", arrow.PrimitiveTypes.Int64), col("payload", arrow.BinaryTypes.String)}, nil)
		const rows, rowBytes = 200_000, 1000
		props := parquet.NewWriterProperties(parquet.WithCompression(compress.Codecs.Uncompressed),
			parquet.WithDictionaryDefault(false), parquet.WithMaxRowGroupLength(64_000))
		var buf bytes.Buffer
		w, err := pqarrow.NewFileWriter(sc, &buf, props, pqarrow.DefaultWriterProps())
		require.NoError(t, err)
		for start := 0; start < rows; start += 8000 {
			b := array.NewRecordBuilder(memory.DefaultAllocator, sc)
			for r := start; r < start+8000; r++ {
				b.Field(0).(*array.Int64Builder).Append(int64(r))
				b.Field(1).(*array.StringBuilder).Append(randomText(rowBytes))
			}
			rec := b.NewRecordBatch()
			require.NoError(t, w.WriteBuffered(rec))
			rec.Release()
			b.Release()
		}
		require.NoError(t, w.Close())
		require.Greater(t, buf.Len(), 200_000_000)
		f := tempFile(t, "x.parquet", buf.Bytes())
		buf = bytes.Buffer{}
		require.Equal(t, rows, read(t, f))
	})
}
