package filesource

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/metadata"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/apache/arrow-go/v18/parquet/schema"
	"github.com/apache/thrift/lib/go/thrift"
)

// Parquet memory is bounded in bytes: pages stream through a small buffer
// per column chunk, a page's declared size is capped, and a batch holds
// about batchTarget bytes of rows.
const (
	parquetStreamBuffer = 256 << 10
	maxUncompressedPage = 32 << 20
	maxCompressedPage   = 16 << 20
	maxSyncFooter       = 16 << 20
	batchTarget         = 64 << 20
	minBatchRows        = 64
	maxBatchRows        = 4096
	// footerTail is the footer length and the closing magic.
	footerTail = 8
	// schemaNodesPerColumn bounds a footer's schema nodes against its
	// columns: a list or map wraps a column in two groups and a struct in
	// one, so eight a column refuses only crafted schemas.
	schemaNodesPerColumn = 8
)

// Field ids in parquet.thrift that the footer check reads.
const (
	fileMetaDataSchema       = 2 // FileMetaData.schema
	schemaElementNumChildren = 5 // SchemaElement.num_children, set on groups
)

// Seconds since the Unix epoch of 0001-01-01T00:00:00Z and of the last
// second of 9999, the years a JSON timestamp is written for.
const (
	minUnixSeconds = -62135596800
	maxUnixSeconds = 253402300799
)

// nulledCounts are values written as null because JSON cannot hold them.
type nulledCounts struct {
	timestampRange, nonFinite int64
}

// conv appends one value of arr as JSON.
type conv func(dst []byte, arr arrow.Array, i int) []byte

// parquetField is one top-level column of a Parquet file.
type parquetField struct {
	name     string
	typ      Type
	nullable bool
	conv     conv
	// leaf is the column index of a primitive column, else -1.
	leaf int
}

// parquetFile is an open local Parquet file.
type parquetFile struct {
	os     *os.File
	reader *file.Reader
	arrow  *pqarrow.FileReader
	fields []parquetField
}

func (p *parquetFile) close() {
	_ = p.reader.Close() // best-effort: closes p.os, a read-only file
}

// openParquet checks a file's magic bytes and footer before decoding it:
// its leaf columns, and the memory each row group needs, must fit lim.
func openParquet(path string, lim Limits, nulled *nulledCounts) (_ *parquetFile, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = f.Close() // best-effort: read-only
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	head := make([]byte, 10)
	if _, err := f.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if c, name := sniff(head); c != codecNone || name != "" {
		return nil, because(ErrCompression, "a compressed Parquet file; store Parquet files uncompressed, their pages are compressed already")
	}
	n, err := footerLength(f, info.Size(), maxSyncFooter, ErrParquet)
	if err != nil {
		return nil, err
	}
	if string(head[:4]) != string(magicParquet) {
		return nil, because(ErrParquet, "not a Parquet file")
	}
	if err := checkFooterSchema(io.NewSectionReader(f, info.Size()-footerTail-n, n), lim.MaxColumns); err != nil {
		return nil, err
	}
	props := parquet.NewReaderProperties(memory.DefaultAllocator)
	props.BufferedStreamEnabled = true
	props.BufferSize = parquetStreamBuffer
	props.MaxUncompressedPageSize = maxUncompressedPage
	props.MaxCompressedPageSize = maxCompressedPage
	reader, err := file.NewParquetReader(f, file.WithReadProps(props))
	if err != nil {
		return nil, &reason{kind: ErrParquet, msg: "unreadable Parquet footer", cause: err}
	}
	defer func() {
		if err != nil {
			_ = reader.Close() // best-effort: read-only
		}
	}()
	md := reader.MetaData()
	if n := md.Schema.NumColumns(); n > lim.MaxColumns {
		return nil, because(ErrTooManyColumns, "more than %d columns", lim.MaxColumns)
	}
	if err := checkRowGroups(md, lim.MaxRowGroupBytes); err != nil {
		return nil, err
	}
	fr, err := pqarrow.NewFileReader(reader, pqarrow.ArrowReadProperties{BatchSize: batchRows(md), PreAllocBinaryData: true}, memory.DefaultAllocator)
	if err != nil {
		return nil, &reason{kind: ErrParquet, msg: "unreadable Parquet schema", cause: err}
	}
	fields, err := parquetFields(fr.Manifest, md.Schema, lim.MaxDepth, nulled)
	if err != nil {
		return nil, err
	}
	return &parquetFile{os: f, reader: reader, arrow: fr, fields: fields}, nil
}

// footerLength reads the footer's length from the end of a file, refusing
// a footer over limit with kind.
func footerLength(r io.ReaderAt, size, limit int64, kind error) (int64, error) {
	if size < int64(len(magicParquet))+footerTail {
		return 0, because(ErrParquet, "too short to be a Parquet file")
	}
	tail := make([]byte, footerTail)
	if _, err := r.ReadAt(tail, size-footerTail); err != nil {
		return 0, err
	}
	switch string(tail[4:]) {
	case "PAR1":
	case "PARE":
		return 0, because(ErrParquet, "encrypted Parquet files are not supported")
	default:
		return 0, because(ErrParquet, "not a Parquet file, or cut short")
	}
	n := int64(binary.LittleEndian.Uint32(tail))
	if n > limit {
		return 0, because(kind, "the Parquet footer is over %s", sizeText(limit))
	}
	if n+footerTail+int64(len(magicParquet)) > size {
		return 0, because(ErrParquet, "the Parquet file is cut short")
	}
	return n, nil
}

// checkFooterSchema walks a footer's schema list before it is decoded:
// more than maxColumns columns, or schemaNodesPerColumn nodes a column, is
// ErrTooManyColumns. Decoding builds about 750 bytes of heap for a node
// written in as few as 6, so a crafted 1 MiB footer of 100,000 columns
// would otherwise take some 75 MiB before NumColumns could refuse it.
func checkFooterSchema(r io.Reader, maxColumns int) error {
	ctx := context.Background()
	p := thrift.NewTCompactProtocolConf(thrift.NewStreamTransportR(r), nil)
	unreadable := func(err error) error {
		return &reason{kind: ErrParquet, msg: "unreadable Parquet footer", cause: err}
	}
	if _, err := p.ReadStructBegin(ctx); err != nil {
		return unreadable(err)
	}
	for {
		_, typ, id, err := p.ReadFieldBegin(ctx)
		switch {
		case err != nil:
			return unreadable(err)
		case typ == thrift.STOP:
			return nil // no schema: decoding the footer refuses it
		case id != fileMetaDataSchema || typ != thrift.LIST:
			if err := thrift.SkipDefaultDepth(ctx, p, typ); err != nil {
				return unreadable(err)
			}
			continue
		}
		_, nodes, err := p.ReadListBegin(ctx)
		if err != nil {
			return unreadable(err)
		}
		columns := 0
		for range nodes {
			children, err := schemaChildren(ctx, p)
			if err != nil {
				return unreadable(err)
			}
			// A node without children is a column, as arrow reads a schema.
			if children == 0 {
				if columns++; columns > maxColumns {
					return because(ErrTooManyColumns, "more than %d columns", maxColumns)
				}
			}
		}
		if limit := schemaNodesPerColumn*maxColumns + 1; nodes > limit {
			return because(ErrTooManyColumns, "more than %d schema fields, nested groups included", limit)
		}
		return nil
	}
}

// schemaChildren reads one SchemaElement for its num_children.
func schemaChildren(ctx context.Context, p *thrift.TCompactProtocol) (int32, error) {
	if _, err := p.ReadStructBegin(ctx); err != nil {
		return 0, err
	}
	var children int32
	for {
		_, typ, id, err := p.ReadFieldBegin(ctx)
		switch {
		case err != nil:
			return 0, err
		case typ == thrift.STOP:
			return children, p.ReadStructEnd(ctx)
		case id == schemaElementNumChildren && typ == thrift.I32:
			if children, err = p.ReadI32(ctx); err != nil {
				return 0, err
			}
		default:
			if err := thrift.SkipDefaultDepth(ctx, p, typ); err != nil {
				return 0, err
			}
		}
	}
}

// checkRowGroups estimates each row group's read memory from the footer: a
// page of each leaf (capped at the page limit) and its stream buffer.
func checkRowGroups(md *metadata.FileMetaData, limit int64) error {
	leaves := md.Schema.NumColumns()
	for rg := 0; rg < md.NumRowGroups(); rg++ {
		group := md.RowGroup(rg)
		need := int64(leaves) * parquetStreamBuffer
		for c := 0; c < leaves; c++ {
			chunk, err := group.ColumnChunk(c)
			if err != nil {
				return &reason{kind: ErrParquet, msg: "unreadable Parquet footer", cause: err}
			}
			need += min(int64(maxUncompressedPage), max(chunk.TotalUncompressedSize(), 0))
		}
		if need > limit {
			return because(ErrParquet, "row group %d needs about %d MiB to read; write smaller row groups or fewer columns", rg+1, need>>20)
		}
	}
	return nil
}

// batchRows aims a batch at batchTarget bytes of decoded rows.
func batchRows(md *metadata.FileMetaData) int64 {
	var size int64
	for rg := 0; rg < md.NumRowGroups(); rg++ {
		size += md.RowGroup(rg).TotalByteSize()
	}
	if size <= 0 || md.NumRows <= 0 {
		return maxBatchRows
	}
	return min(max(int64(float64(batchTarget)*float64(md.NumRows)/float64(size)), minBatchRows), maxBatchRows)
}

// parquetFields builds each top-level column's type and converter.
func parquetFields(m *pqarrow.SchemaManifest, sc *schema.Schema, maxDepth int, nulled *nulledCounts) ([]parquetField, error) {
	fields := make([]parquetField, len(m.Fields))
	for i := range m.Fields {
		sf := &m.Fields[i]
		b := convBuilder{schema: sc, maxDepth: maxDepth, nulled: nulled, column: sf.Field.Name}
		c, t, err := b.build(sf, sf.Field.Type, 1)
		if err != nil {
			return nil, err
		}
		fields[i] = parquetField{name: sf.Field.Name, typ: t, conv: c, leaf: sf.ColIndex,
			nullable: sf.Field.Nullable || t == TypeFloat || t == TypeDate || t == TypeTime || t == TypeTimestamp || t == TypeTimestampTZ}
	}
	return fields, nil
}

type convBuilder struct {
	schema   *schema.Schema
	maxDepth int
	nulled   *nulledCounts
	// column is the top-level column being built, for messages.
	column string
}

// build makes the converter for values of type dt described by sf, which
// sit at nesting depth (a row's columns are at 1).
func (b convBuilder) build(sf *pqarrow.SchemaField, dt arrow.DataType, depth int) (conv, Type, error) {
	c, t, err := b.value(sf, dt, depth)
	if err != nil {
		return nil, 0, err
	}
	return func(dst []byte, arr arrow.Array, i int) []byte {
		if arr.IsNull(i) {
			return append(dst, "null"...)
		}
		return c(dst, arr, i)
	}, t, nil
}

func (b convBuilder) value(sf *pqarrow.SchemaField, dt arrow.DataType, depth int) (conv, Type, error) {
	var logical schema.LogicalType = schema.NoLogicalType{}
	if sf.IsLeaf() {
		logical = b.schema.Column(sf.ColIndex).LogicalType()
	}
	switch t := dt.(type) {
	case *arrow.NullType:
		return func(dst []byte, _ arrow.Array, _ int) []byte { return append(dst, "null"...) }, TypeNull, nil
	case *arrow.BooleanType:
		return func(dst []byte, arr arrow.Array, i int) []byte {
			return strconv.AppendBool(dst, arr.(*array.Boolean).Value(i))
		}, TypeBoolean, nil
	case *arrow.Int8Type:
		return intConv(func(arr arrow.Array, i int) int64 { return int64(arr.(*array.Int8).Value(i)) }), TypeInteger, nil
	case *arrow.Int16Type:
		return intConv(func(arr arrow.Array, i int) int64 { return int64(arr.(*array.Int16).Value(i)) }), TypeInteger, nil
	case *arrow.Int32Type:
		return intConv(func(arr arrow.Array, i int) int64 { return int64(arr.(*array.Int32).Value(i)) }), TypeInteger, nil
	case *arrow.Int64Type:
		return intConv(func(arr arrow.Array, i int) int64 { return arr.(*array.Int64).Value(i) }), TypeInteger, nil
	case *arrow.Uint8Type:
		return intConv(func(arr arrow.Array, i int) int64 { return int64(arr.(*array.Uint8).Value(i)) }), TypeInteger, nil
	case *arrow.Uint16Type:
		return intConv(func(arr arrow.Array, i int) int64 { return int64(arr.(*array.Uint16).Value(i)) }), TypeInteger, nil
	case *arrow.Uint32Type:
		return intConv(func(arr arrow.Array, i int) int64 { return int64(arr.(*array.Uint32).Value(i)) }), TypeInteger, nil
	case *arrow.Uint64Type:
		return func(dst []byte, arr arrow.Array, i int) []byte {
			v := arr.(*array.Uint64).Value(i)
			if v > math.MaxInt64 {
				return appendString(dst, strconv.FormatUint(v, 10))
			}
			return strconv.AppendUint(dst, v, 10)
		}, TypeInteger, nil
	case *arrow.Float16Type:
		return b.floatConv(func(arr arrow.Array, i int) float64 { return float64(arr.(*array.Float16).Value(i).Float32()) }, 32), TypeFloat, nil
	case *arrow.Float32Type:
		return b.floatConv(func(arr arrow.Array, i int) float64 { return float64(arr.(*array.Float32).Value(i)) }, 32), TypeFloat, nil
	case *arrow.Float64Type:
		return b.floatConv(func(arr arrow.Array, i int) float64 { return arr.(*array.Float64).Value(i) }, 64), TypeFloat, nil
	case arrow.DecimalType:
		return decimalConv(t), TypeDecimal, nil
	case *arrow.Date32Type:
		return b.timeConv(func(arr arrow.Array, i int) (int64, arrow.TimeUnit) {
			return int64(arr.(*array.Date32).Value(i)) * 86400, arrow.Second
		}, "2006-01-02"), TypeDate, nil
	case *arrow.Date64Type:
		return b.timeConv(func(arr arrow.Array, i int) (int64, arrow.TimeUnit) {
			return int64(arr.(*array.Date64).Value(i)), arrow.Millisecond
		}, "2006-01-02"), TypeDate, nil
	case *arrow.Time32Type:
		return b.clockConv(func(arr arrow.Array, i int) int64 { return int64(arr.(*array.Time32).Value(i)) }, t.Unit), TypeTime, nil
	case *arrow.Time64Type:
		return b.clockConv(func(arr arrow.Array, i int) int64 { return int64(arr.(*array.Time64).Value(i)) }, t.Unit), TypeTime, nil
	case *arrow.TimestampType:
		read := func(arr arrow.Array, i int) (int64, arrow.TimeUnit) {
			return int64(arr.(*array.Timestamp).Value(i)), t.Unit
		}
		if t.TimeZone != "" {
			return b.timeConv(read, "2006-01-02T15:04:05.999999999Z07:00"), TypeTimestampTZ, nil
		}
		return b.timeConv(read, "2006-01-02T15:04:05.999999999"), TypeTimestamp, nil
	case *arrow.StringType, *arrow.LargeStringType, *arrow.StringViewType:
		return func(dst []byte, arr arrow.Array, i int) []byte {
			return appendString(dst, arr.(interface{ Value(int) string }).Value(i))
		}, TypeString, nil
	case *arrow.BinaryType, *arrow.LargeBinaryType, *arrow.BinaryViewType:
		value := func(arr arrow.Array, i int) []byte { return arr.(interface{ Value(int) []byte }).Value(i) }
		switch logical.(type) {
		case schema.JSONLogicalType:
			return b.jsonConv(value, depth), TypeJSON, nil
		case schema.StringLogicalType, schema.EnumLogicalType:
			return func(dst []byte, arr arrow.Array, i int) []byte { return appendString(dst, string(value(arr, i))) }, TypeString, nil
		}
		return base64Conv(value), TypeBinary, nil
	case *arrow.FixedSizeBinaryType:
		value := func(arr arrow.Array, i int) []byte { return arr.(*array.FixedSizeBinary).Value(i) }
		switch logical.(type) {
		case schema.UUIDLogicalType:
			if t.ByteWidth == 16 {
				return uuidConv(value), TypeString, nil
			}
		case schema.IntervalLogicalType:
			if t.ByteWidth == 12 {
				return intervalConv(value), TypeString, nil
			}
		}
		return base64Conv(value), TypeBinary, nil
	case arrow.ExtensionType:
		return b.extension(sf, t, depth)
	case *arrow.DictionaryType:
		values, vt, err := b.build(sf, t.ValueType, depth)
		if err != nil {
			return nil, 0, err
		}
		return func(dst []byte, arr arrow.Array, i int) []byte {
			d := arr.(*array.Dictionary)
			return values(dst, d.Dictionary(), d.GetValueIndex(i))
		}, vt, nil
	case *arrow.MapType:
		return b.mapConv(sf, t, depth)
	case arrow.ListLikeType:
		return b.listConv(sf, t, depth)
	case *arrow.StructType:
		return b.structConv(sf, t, depth)
	case *arrow.MonthIntervalType:
		return func(dst []byte, arr arrow.Array, i int) []byte {
			return appendISODuration(dst, int64(arr.(*array.MonthInterval).Value(i)), 0, 0, arrow.Nanosecond)
		}, TypeString, nil
	case *arrow.DayTimeIntervalType:
		return func(dst []byte, arr arrow.Array, i int) []byte {
			v := arr.(*array.DayTimeInterval).Value(i)
			return appendISODuration(dst, 0, int64(v.Days), int64(v.Milliseconds), arrow.Millisecond)
		}, TypeString, nil
	case *arrow.MonthDayNanoIntervalType:
		return func(dst []byte, arr arrow.Array, i int) []byte {
			v := arr.(*array.MonthDayNanoInterval).Value(i)
			return appendISODuration(dst, int64(v.Months), int64(v.Days), v.Nanoseconds, arrow.Nanosecond)
		}, TypeString, nil
	case *arrow.DurationType:
		return func(dst []byte, arr arrow.Array, i int) []byte {
			return appendISODuration(dst, 0, 0, int64(arr.(*array.Duration).Value(i)), t.Unit)
		}, TypeString, nil
	}
	return nil, 0, because(ErrParquet, "column %s has type %s, which Helix cannot read", b.column, dt)
}

// intervalConv writes a Parquet INTERVAL, three little-endian unsigned
// counts of months, days and milliseconds, as an ISO 8601 duration.
func intervalConv(value func(arrow.Array, int) []byte) conv {
	return func(dst []byte, arr arrow.Array, i int) []byte {
		v := value(arr, i)
		count := func(at int) int64 { return int64(binary.LittleEndian.Uint32(v[at:])) }
		return appendISODuration(dst, count(0), count(4), count(8), arrow.Millisecond)
	}
}

func intConv(value func(arrow.Array, int) int64) conv {
	return func(dst []byte, arr arrow.Array, i int) []byte { return strconv.AppendInt(dst, value(arr, i), 10) }
}

// floatConv writes NaN and infinities, which JSON cannot hold, as null.
func (b convBuilder) floatConv(value func(arrow.Array, int) float64, bits int) conv {
	return func(dst []byte, arr arrow.Array, i int) []byte {
		f := value(arr, i)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			b.nulled.nonFinite++
			return append(dst, "null"...)
		}
		return appendFloat(dst, f, bits)
	}
}

// exactDecimalDigits is the most digits a float64 holds exactly, so a
// reader parsing the number cannot change it.
const exactDecimalDigits = 15

// decimalConv writes a decimal's exact text: a number when a float64
// holds it exactly, else a string.
func decimalConv(t arrow.DecimalType) conv {
	scale, asNumber := t.GetScale(), t.GetPrecision() <= exactDecimalDigits
	return func(dst []byte, arr arrow.Array, i int) []byte {
		var s string
		switch a := arr.(type) {
		case *array.Decimal32:
			s = a.Value(i).ToString(scale)
		case *array.Decimal64:
			s = a.Value(i).ToString(scale)
		case *array.Decimal128:
			s = a.Value(i).ToString(scale)
		case *array.Decimal256:
			s = a.Value(i).ToString(scale)
		}
		if asNumber {
			return append(dst, s...)
		}
		return appendString(dst, s)
	}
}

// timeConv writes a date or timestamp in layout, UTC. A year outside
// 0001-9999 has no such text and is written as null.
func (b convBuilder) timeConv(value func(arrow.Array, int) (int64, arrow.TimeUnit), layout string) conv {
	return func(dst []byte, arr arrow.Array, i int) []byte {
		v, unit := value(arr, i)
		t, ok := unitTime(v, unit)
		if !ok {
			b.nulled.timestampRange++
			return append(dst, "null"...)
		}
		dst = append(dst, '"')
		dst = t.AppendFormat(dst, layout)
		return append(dst, '"')
	}
}

// unitTime is v units after the Unix epoch, when its year is 0001-9999.
func unitTime(v int64, unit arrow.TimeUnit) (time.Time, bool) {
	var t time.Time
	switch unit {
	case arrow.Second:
		if v < minUnixSeconds || v > maxUnixSeconds {
			return t, false
		}
		t = time.Unix(v, 0)
	case arrow.Millisecond:
		if v < minUnixSeconds*1e3 || v > maxUnixSeconds*1e3+999 {
			return t, false
		}
		t = time.UnixMilli(v)
	case arrow.Microsecond:
		if v < minUnixSeconds*1e6 || v > maxUnixSeconds*1e6+999999 {
			return t, false
		}
		t = time.UnixMicro(v)
	default:
		// Every int64 of nanoseconds falls in 1677-2262.
		t = time.Unix(0, v)
	}
	return t.UTC(), true
}

// clockConv writes a time of day as 15:04:05 with trailing zeros of the
// fraction dropped. A value outside one day is written as null.
func (b convBuilder) clockConv(value func(arrow.Array, int) int64, unit arrow.TimeUnit) conv {
	return func(dst []byte, arr arrow.Array, i int) []byte {
		v := value(arr, i)
		perDay := int64(86400) * int64(time.Second/unit.Multiplier())
		if v < 0 || v >= perDay {
			b.nulled.timestampRange++
			return append(dst, "null"...)
		}
		d := time.Duration(v) * unit.Multiplier()
		t := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).Add(d)
		dst = append(dst, '"')
		dst = t.AppendFormat(dst, "15:04:05.999999999")
		return append(dst, '"')
	}
}

func base64Conv(value func(arrow.Array, int) []byte) conv {
	return func(dst []byte, arr arrow.Array, i int) []byte {
		dst = append(dst, '"')
		dst = base64.StdEncoding.AppendEncode(dst, value(arr, i))
		return append(dst, '"')
	}
}

// uuidConv writes a 16-byte UUID in the canonical lower-case form.
func uuidConv(value func(arrow.Array, int) []byte) conv {
	return func(dst []byte, arr arrow.Array, i int) []byte {
		dst = append(dst, '"')
		for j, c := range value(arr, i) {
			if j == 4 || j == 6 || j == 8 || j == 10 {
				dst = append(dst, '-')
			}
			dst = append(dst, hexDigits[c>>4], hexDigits[c&0xf])
		}
		return append(dst, '"')
	}
}

// jsonConv writes a JSON-annotated value as the JSON it holds, or as a
// string when it is not valid JSON or is nested too deeply.
func (b convBuilder) jsonConv(value func(arrow.Array, int) []byte, depth int) conv {
	return func(dst []byte, arr arrow.Array, i int) []byte {
		raw := value(arr, i)
		if json.Valid(raw) {
			n := len(dst)
			out, err := normalize(dst, raw, depth, b.maxDepth)
			if err == nil {
				return out
			}
			dst = out[:n]
		}
		return appendString(dst, string(raw))
	}
}

func (b convBuilder) extension(sf *pqarrow.SchemaField, t arrow.ExtensionType, depth int) (conv, Type, error) {
	storage, st, err := b.build(sf, t.StorageType(), depth)
	if err != nil {
		return nil, 0, err
	}
	unwrap := func(arr arrow.Array) arrow.Array { return arr.(array.ExtensionArray).Storage() }
	switch t.ExtensionName() {
	case "arrow.uuid":
		return uuidConv(func(arr arrow.Array, i int) []byte { return unwrap(arr).(*array.FixedSizeBinary).Value(i) }), TypeString, nil
	case "arrow.json":
		return b.jsonConv(func(arr arrow.Array, i int) []byte {
			return []byte(unwrap(arr).(interface{ ValueStr(int) string }).ValueStr(i))
		}, depth), TypeJSON, nil
	case "arrow.bool8":
		return func(dst []byte, arr arrow.Array, i int) []byte {
			return strconv.AppendBool(dst, unwrap(arr).(*array.Int8).Value(i) != 0)
		}, TypeBoolean, nil
	}
	return func(dst []byte, arr arrow.Array, i int) []byte { return storage(dst, unwrap(arr), i) }, st, nil
}

// nested opens one more level of nesting, refusing one past the limit.
func (b convBuilder) nested(sf *pqarrow.SchemaField, depth, children int) error {
	if depth+1 > b.maxDepth {
		return because(ErrTooDeep, "column %s is nested more than %d levels deep", b.column, b.maxDepth)
	}
	if len(sf.Children) < children {
		return because(ErrParquet, "column %s has an unreadable nested type", b.column)
	}
	return nil
}

func (b convBuilder) listConv(sf *pqarrow.SchemaField, t arrow.ListLikeType, depth int) (conv, Type, error) {
	if err := b.nested(sf, depth, 1); err != nil {
		return nil, 0, err
	}
	elem, _, err := b.build(&sf.Children[0], t.Elem(), depth+1)
	if err != nil {
		return nil, 0, err
	}
	return func(dst []byte, arr arrow.Array, i int) []byte {
		list := arr.(array.ListLike)
		start, end := list.ValueOffsets(i)
		values := list.ListValues()
		dst = append(dst, '[')
		for j := start; j < end; j++ {
			if j > start {
				dst = append(dst, ',')
			}
			dst = elem(dst, values, int(j))
		}
		return append(dst, ']')
	}, TypeJSON, nil
}

func (b convBuilder) structConv(sf *pqarrow.SchemaField, t *arrow.StructType, depth int) (conv, Type, error) {
	if err := b.nested(sf, depth, t.NumFields()); err != nil {
		return nil, 0, err
	}
	keys := make([][]byte, t.NumFields())
	convs := make([]conv, t.NumFields())
	for k, f := range t.Fields() {
		c, _, err := b.build(&sf.Children[k], f.Type, depth+1)
		if err != nil {
			return nil, 0, err
		}
		keys[k], convs[k] = append(appendString(nil, f.Name), ':'), c
	}
	return func(dst []byte, arr arrow.Array, i int) []byte {
		s := arr.(*array.Struct)
		dst = append(dst, '{')
		for k, c := range convs {
			if k > 0 {
				dst = append(dst, ',')
			}
			dst = c(append(dst, keys[k]...), s.Field(k), i)
		}
		return append(dst, '}')
	}, TypeJSON, nil
}

// mapConv writes a map with string keys as an object, and any other map
// as a list of {"key":k,"value":v}.
func (b convBuilder) mapConv(sf *pqarrow.SchemaField, t *arrow.MapType, depth int) (conv, Type, error) {
	if err := b.nested(sf, depth, 1); err != nil {
		return nil, 0, err
	}
	entries := &sf.Children[0]
	stringKeys := isStringType(t.KeyType())
	inner := depth + 1
	if !stringKeys {
		inner++
		if inner > b.maxDepth {
			return nil, 0, because(ErrTooDeep, "column %s is nested more than %d levels deep", b.column, b.maxDepth)
		}
	}
	if len(entries.Children) < 2 {
		return nil, 0, because(ErrParquet, "column %s has an unreadable nested type", b.column)
	}
	key, _, err := b.build(&entries.Children[0], t.KeyType(), inner)
	if err != nil {
		return nil, 0, err
	}
	item, _, err := b.build(&entries.Children[1], t.ItemType(), inner)
	if err != nil {
		return nil, 0, err
	}
	return func(dst []byte, arr arrow.Array, i int) []byte {
		m := arr.(*array.Map)
		start, end := m.ValueOffsets(i)
		keys, items := m.Keys(), m.Items()
		open, close := byte('{'), byte('}')
		if !stringKeys {
			open, close = '[', ']'
		}
		dst = append(dst, open)
		for j := start; j < end; j++ {
			if j > start {
				dst = append(dst, ',')
			}
			if stringKeys {
				dst = key(dst, keys, int(j))
				dst = item(append(dst, ':'), items, int(j))
				continue
			}
			dst = key(append(dst, `{"key":`...), keys, int(j))
			dst = append(item(append(dst, `,"value":`...), items, int(j)), '}')
		}
		return append(dst, close)
	}, TypeJSON, nil
}

func isStringType(dt arrow.DataType) bool {
	switch t := dt.(type) {
	case *arrow.StringType, *arrow.LargeStringType, *arrow.StringViewType:
		return true
	case *arrow.DictionaryType:
		return isStringType(t.ValueType)
	}
	return false
}

// appendISODuration writes months, days and an amount of unit as an ISO
// 8601 duration such as "P1M2DT3.5S".
func appendISODuration(dst []byte, months, days, amount int64, unit arrow.TimeUnit) []byte {
	dst = append(dst, '"', 'P')
	if months != 0 {
		dst = append(strconv.AppendInt(dst, months, 10), 'M')
	}
	if days != 0 {
		dst = append(strconv.AppendInt(dst, days, 10), 'D')
	}
	if amount != 0 || (months == 0 && days == 0) {
		dst = append(appendSeconds(append(dst, 'T'), amount, unit), 'S')
	}
	return append(dst, '"')
}

// appendSeconds writes amount units as decimal seconds without trailing zeros.
func appendSeconds(dst []byte, amount int64, unit arrow.TimeUnit) []byte {
	per := uint64(time.Second / unit.Multiplier())
	abs := uint64(amount)
	if amount < 0 {
		dst = append(dst, '-')
		abs = -abs
	}
	dst = strconv.AppendUint(dst, abs/per, 10)
	frac := abs % per
	if frac == 0 {
		return dst
	}
	digits := fmt.Sprintf("%0*d", len(strconv.FormatUint(per, 10))-1, frac)
	for digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
	}
	return append(append(dst, '.'), digits...)
}

// parquetRows reads one Parquet file's rows a row group at a time.
type parquetRows struct {
	ctx  context.Context
	file *parquetFile
	// colOf maps a schema column to its field in this file, -1 for none.
	colOf []int
	group int
	rr    pqarrow.RecordReader
	batch arrow.RecordBatch
	row   int
}

func (p *parquetRows) next() (bool, error) {
	for {
		if p.batch != nil && p.row+1 < int(p.batch.NumRows()) {
			p.row++
			return true, nil
		}
		if err := p.ctx.Err(); err != nil {
			return false, err
		}
		if p.rr != nil && p.rr.Next() {
			p.batch, p.row = p.rr.RecordBatch(), -1
			continue
		}
		if p.rr != nil {
			err := p.rr.Err()
			p.rr.Release()
			p.rr, p.batch = nil, nil
			if err != nil && !errors.Is(err, io.EOF) {
				return false, parquetError(err)
			}
		}
		if p.group == p.file.reader.NumRowGroups() {
			return false, nil
		}
		rr, err := p.file.arrow.GetRecordReader(p.ctx, nil, []int{p.group})
		if err != nil {
			return false, parquetError(err)
		}
		p.rr = rr
		p.group++
	}
}

func (p *parquetRows) appendValue(dst []byte, column int) ([]byte, error) {
	f := p.colOf[column]
	if f < 0 {
		return append(dst, "null"...), nil
	}
	return p.file.fields[f].conv(dst, p.batch.Column(f), p.row), nil
}

func (p *parquetRows) close() {
	if p.rr != nil {
		p.rr.Release()
	}
	p.file.close()
}

func parquetError(err error) error {
	if isContextError(err) {
		return err
	}
	return &reason{kind: ErrParquet, msg: "unreadable Parquet data", cause: err}
}
