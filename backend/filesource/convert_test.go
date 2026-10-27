package filesource

import (
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/extensions"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/apache/arrow-go/v18/parquet/schema"
	"github.com/stretchr/testify/require"
)

// schemaField describes dt the way a manifest does, without Parquet leaves.
func schemaField(name string, dt arrow.DataType) *pqarrow.SchemaField {
	sf := &pqarrow.SchemaField{Field: &arrow.Field{Name: name, Type: dt}, ColIndex: -1}
	switch t := dt.(type) {
	case *arrow.MapType:
		entries := pqarrow.SchemaField{Field: &arrow.Field{Name: "entries"}, ColIndex: -1,
			Children: []pqarrow.SchemaField{*schemaField("key", t.KeyType()), *schemaField("value", t.ItemType())}}
		sf.Children = []pqarrow.SchemaField{entries}
	case arrow.ListLikeType:
		sf.Children = []pqarrow.SchemaField{*schemaField("element", t.Elem())}
	case *arrow.StructType:
		for _, f := range t.Fields() {
			sf.Children = append(sf.Children, *schemaField(f.Name, f.Type))
		}
	}
	return sf
}

func fromJSON(t *testing.T, dt arrow.DataType, values string) arrow.Array {
	t.Helper()
	arr, _, err := array.FromJSON(memory.DefaultAllocator, dt, strings.NewReader(values))
	require.NoError(t, err)
	return arr
}

// render converts every value of arr as a column of type dt.
func render(t *testing.T, b convBuilder, dt arrow.DataType, arr arrow.Array) ([]string, Type) {
	t.Helper()
	c, typ, err := b.build(schemaField("c", dt), dt, 1)
	require.NoError(t, err)
	var out []string
	for i := 0; i < arr.Len(); i++ {
		out = append(out, string(c(nil, arr, i)))
	}
	return out, typ
}

func builder() convBuilder {
	return convBuilder{maxDepth: 64, nulled: &nulledCounts{}, column: "c"}
}

func TestConverters(t *testing.T) {
	union := arrow.DenseUnionOf([]arrow.Field{col("a", arrow.PrimitiveTypes.Int32)}, []arrow.UnionTypeCode{0})
	jsonType, err := extensions.NewJSONType(arrow.BinaryTypes.String)
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		dt     arrow.DataType
		values string
		arr    func(t *testing.T) arrow.Array
		want   []string
		typ    Type
	}{
		"date64":          {dt: arrow.FixedWidthTypes.Date64, values: `[86400000, null]`, want: []string{`"1970-01-02"`, `null`}, typ: TypeDate},
		"decimal32":       {dt: &arrow.Decimal32Type{Precision: 9, Scale: 2}, values: `["12.30", "-0.05"]`, want: []string{`12.30`, `-0.05`}, typ: TypeDecimal},
		"decimal64":       {dt: &arrow.Decimal64Type{Precision: 18, Scale: 3}, values: `["123456789012345.678"]`, want: []string{`"123456789012345.678"`}, typ: TypeDecimal},
		"month interval":  {dt: arrow.FixedWidthTypes.MonthInterval, values: `[{"months":1},{"months":-2},{"months":0}]`, want: []string{`"P1M"`, `"P-2M"`, `"PT0S"`}, typ: TypeString},
		"day time":        {dt: arrow.FixedWidthTypes.DayTimeInterval, values: `[{"days":2,"milliseconds":3500},{"days":0,"milliseconds":-1}]`, want: []string{`"P2DT3.5S"`, `"PT-0.001S"`}, typ: TypeString},
		"month day nano":  {dt: arrow.FixedWidthTypes.MonthDayNanoInterval, values: `[{"months":1,"days":2,"nanoseconds":3500000000}]`, want: []string{`"P1M2DT3.5S"`}, typ: TypeString},
		"duration s":      {dt: arrow.FixedWidthTypes.Duration_s, values: `[90, -5]`, want: []string{`"PT90S"`, `"PT-5S"`}, typ: TypeString},
		"duration ms":     {dt: arrow.FixedWidthTypes.Duration_ms, values: `[1500]`, want: []string{`"PT1.5S"`}, typ: TypeString},
		"duration us":     {dt: arrow.FixedWidthTypes.Duration_us, values: `[-2500001]`, want: []string{`"PT-2.500001S"`}, typ: TypeString},
		"duration ns":     {dt: arrow.FixedWidthTypes.Duration_ns, values: `[1, -9223372036854775808]`, want: []string{`"PT0.000000001S"`, `"PT-9223372036.854775808S"`}, typ: TypeString},
		"timestamp range": {dt: arrow.FixedWidthTypes.Timestamp_us, values: `[253402300800000000, -62135596800000001, 0]`, want: []string{`null`, `null`, `"1970-01-01T00:00:00Z"`}, typ: TypeTimestampTZ},
		"time range":      {dt: arrow.FixedWidthTypes.Time32s, values: `[90000, 3661]`, want: []string{`null`, `"01:01:01"`}, typ: TypeTime},
		"string view":     {dt: arrow.BinaryTypes.StringView, values: `["a", null]`, want: []string{`"a"`, `null`}, typ: TypeString},
		"binary view":     {dt: arrow.BinaryTypes.BinaryView, values: `["AAE="]`, want: []string{`"AAE="`}, typ: TypeBinary},
		"list view":       {dt: arrow.ListViewOf(arrow.PrimitiveTypes.Int32), values: `[[1,2],[]]`, want: []string{`[1,2]`, `[]`}, typ: TypeJSON},
		"json too deep": {dt: jsonType, arr: func(t *testing.T) arrow.Array {
			deep := strings.Repeat("[", 70) + strings.Repeat("]", 70)
			return array.NewExtensionArrayWithStorage(jsonType, fromJSON(t, arrow.BinaryTypes.String, `["`+deep+`", "{\"a\": 1}"]`))
		}, want: []string{`"` + strings.Repeat("[", 70) + strings.Repeat("]", 70) + `"`, `{"a":1}`}, typ: TypeJSON},
		"bool8": {dt: extensions.NewBool8Type(), arr: func(t *testing.T) arrow.Array {
			return array.NewExtensionArrayWithStorage(extensions.NewBool8Type(), fromJSON(t, arrow.PrimitiveTypes.Int8, `[1, 0, null]`))
		}, want: []string{`true`, `false`, `null`}, typ: TypeBoolean},
		"opaque": {dt: extensions.NewOpaqueType(arrow.PrimitiveTypes.Int32, "t", "v"), arr: func(t *testing.T) arrow.Array {
			return array.NewExtensionArrayWithStorage(extensions.NewOpaqueType(arrow.PrimitiveTypes.Int32, "t", "v"), fromJSON(t, arrow.PrimitiveTypes.Int32, `[7]`))
		}, want: []string{`7`}, typ: TypeInteger},
		"union in opaque":  {dt: extensions.NewOpaqueType(union, "t", "v")},
		"union":            {dt: union},
		"union in list":    {dt: arrow.ListOf(union)},
		"union in struct":  {dt: arrow.StructOf(col("u", union))},
		"union map key":    {dt: arrow.MapOf(union, arrow.PrimitiveTypes.Int32)},
		"union map item":   {dt: arrow.MapOf(arrow.BinaryTypes.String, union)},
		"union dictionary": {dt: &arrow.DictionaryType{IndexType: arrow.PrimitiveTypes.Int8, ValueType: union}},
	} {
		t.Run(name, func(t *testing.T) {
			b := builder()
			if tc.want == nil {
				_, _, err := b.build(schemaField("c", tc.dt), tc.dt, 1)
				require.ErrorIs(t, err, ErrParquet)
				require.Contains(t, err.Error(), "column c has type")
				return
			}
			arr := tc.arr
			if arr == nil {
				arr = func(t *testing.T) arrow.Array { return fromJSON(t, tc.dt, tc.values) }
			}
			got, typ := render(t, b, tc.dt, arr(t))
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.typ, typ)
		})
	}
}

func TestConverterNesting(t *testing.T) {
	b := builder()
	b.maxDepth = 2
	_, _, err := b.build(schemaField("c", arrow.MapOf(arrow.PrimitiveTypes.Int32, arrow.PrimitiveTypes.Int32)), arrow.MapOf(arrow.PrimitiveTypes.Int32, arrow.PrimitiveTypes.Int32), 1)
	require.ErrorIs(t, err, ErrTooDeep)
	c, _, err := b.build(schemaField("c", arrow.MapOf(arrow.BinaryTypes.String, arrow.PrimitiveTypes.Int32)), arrow.MapOf(arrow.BinaryTypes.String, arrow.PrimitiveTypes.Int32), 1)
	require.NoError(t, err)
	require.NotNil(t, c)
	_, _, err = b.build(schemaField("c", arrow.StructOf(col("s", arrow.StructOf(col("x", arrow.PrimitiveTypes.Int32))))), arrow.StructOf(col("s", arrow.StructOf(col("x", arrow.PrimitiveTypes.Int32)))), 1)
	require.ErrorIs(t, err, ErrTooDeep)

	b = builder()
	for _, dt := range []arrow.DataType{arrow.ListOf(arrow.PrimitiveTypes.Int32), arrow.StructOf(col("x", arrow.PrimitiveTypes.Int32)), arrow.MapOf(arrow.BinaryTypes.String, arrow.PrimitiveTypes.Int32)} {
		bare := &pqarrow.SchemaField{Field: &arrow.Field{Name: "c", Type: dt}, ColIndex: -1}
		_, _, err := b.build(bare, dt, 1)
		require.ErrorIs(t, err, ErrParquet, dt.String())
		require.Contains(t, err.Error(), "unreadable nested type")
	}
	m := arrow.MapOf(arrow.BinaryTypes.String, arrow.PrimitiveTypes.Int32)
	noEntries := &pqarrow.SchemaField{Field: &arrow.Field{Name: "c", Type: m}, ColIndex: -1, Children: []pqarrow.SchemaField{{ColIndex: -1}}}
	_, _, err = b.build(noEntries, m, 1)
	require.ErrorIs(t, err, ErrParquet)

	require.True(t, isStringType(&arrow.DictionaryType{IndexType: arrow.PrimitiveTypes.Int8, ValueType: arrow.BinaryTypes.LargeString}))
	require.False(t, isStringType(arrow.PrimitiveTypes.Int8))
}

func TestConverterLogicalTypes(t *testing.T) {
	enum, err := schema.NewPrimitiveNodeLogical("e", parquet.Repetitions.Optional, schema.EnumLogicalType{}, parquet.Types.ByteArray, -1, -1)
	require.NoError(t, err)
	root, err := schema.NewGroupNode("schema", parquet.Repetitions.Required, schema.FieldList{enum}, -1)
	require.NoError(t, err)
	b := builder()
	b.schema = schema.NewSchema(root)
	sf := &pqarrow.SchemaField{Field: &arrow.Field{Name: "e", Type: arrow.BinaryTypes.Binary}, ColIndex: 0}
	c, typ, err := b.build(sf, arrow.BinaryTypes.Binary, 1)
	require.NoError(t, err)
	require.Equal(t, TypeString, typ)
	require.Equal(t, `"red"`, string(c(nil, fromJSON(t, arrow.BinaryTypes.Binary, `["cmVk"]`), 0)))
}
