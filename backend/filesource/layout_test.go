package filesource

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const uuidPart = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

func listed(keys ...string) []Listed {
	out := make([]Listed, len(keys))
	for i, k := range keys {
		out[i] = Listed{Key: "e2e/" + k, Size: 10}
	}
	return out
}

func tableNames(ds []Dataset) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Name)
	}
	return out
}

// TestLayoutPlanCases are the layout's required key → table cases.
func TestLayoutPlanCases(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keys    []string
		tables  []string
		skipped []Skipped
	}{
		{"root files", []string{"customers.csv", "orders.json"}, []string{"customers", "orders"}, nil},
		{"spark parts", []string{
			"orders/dt=2026-10-01/part-00000-" + uuidPart + "-c000.snappy.parquet",
			"orders/dt=2026-10-01/part-00001-" + uuidPart + "-c000.snappy.parquet",
			"orders/dt=2026-10-02/part-00000-" + uuidPart + "-c000.snappy.parquet",
		}, []string{"orders"}, nil},
		{"partition at root", []string{"dt=2026-10-01/part-0.parquet"}, []string{"e2e"}, nil},
		{"dated lines", []string{"events/2026-10-01.jsonl.gz", "events/2026-10-02.jsonl.zst"}, []string{"events"}, nil},
		{"folder of tables", []string{"exports/customers.csv", "exports/orders.csv"}, []string{"exports/customers", "exports/orders"}, nil},
		{"daily dumps", []string{"daily/2026-10-01/customers.csv", "daily/2026-10-02/customers.csv"}, []string{"daily"}, nil},
		{"numbered files", []string{"sales/orders_2026_01.csv", "sales/orders_2026_02.csv"}, []string{"sales"}, nil},
		{"one file in a folder", []string{"api/invoices.json"}, []string{"api"}, nil},
		{"mixed formats", []string{"x/a.csv", "x/a.json"}, nil, []Skipped{{"x", "mixed formats (csv, json)"}}},
		{"delta", []string{"lake/t/_delta_log/0.json", "lake/t/p.parquet"}, nil, []Skipped{{"lake/t", reasonDelta}}},
		{"ignored and unsupported", []string{"notes.xlsx", "_SUCCESS", ".DS_Store", "dir/"}, nil, []Skipped{{"notes.xlsx", reasonUnsupported}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tables, skipped := Layout("e2e/", "e2e", listed(tc.keys...))
			require.Equal(t, tc.tables, tableNames(tables))
			require.Equal(t, tc.skipped, skipped)
		})
	}
}

func TestLayoutDetails(t *testing.T) {
	t.Run("files sorted by key, formats", func(t *testing.T) {
		tables, _ := Layout("e2e/", "e2e", listed("orders/part-2.parquet", "orders/part-1.parquet", "cats.TSV", "dogs.ndjson", "eels.PQ"))
		require.Equal(t, []string{"cats", "dogs", "eels", "orders"}, tableNames(tables))
		require.Equal(t, FormatCSV, tables[0].Format)
		require.Equal(t, FormatJSON, tables[1].Format)
		require.Equal(t, FormatParquet, tables[2].Format)
		require.Equal(t, "e2e/orders/part-1.parquet", tables[3].Files[0].Key)
		require.Equal(t, "e2e/orders/part-2.parquet", tables[3].Files[1].Key)
	})
	t.Run("ignored keys", func(t *testing.T) {
		objs := listed("a/_temporary/0/x.csv", "a/.hidden.csv", "a/x.csv.crc", "a/x.csv.TMP", "a/.spark/x.csv")
		objs = append(objs, Listed{Key: "e2e/empty.csv", Size: 0}, Listed{Key: "other/x.csv", Size: 1}, Listed{Key: "e2e/", Size: 0})
		tables, skipped := Layout("e2e/", "e2e", objs)
		require.Empty(t, tables)
		require.Empty(t, skipped)
	})
	t.Run("hive segment starting with an underscore is kept", func(t *testing.T) {
		tables, _ := Layout("e2e/", "e2e", listed("t/_year=2026/a.csv"))
		require.Equal(t, []string{"t"}, tableNames(tables))
	})
	t.Run("archived", func(t *testing.T) {
		objs := listed("archive/old.csv", "archive/new.csv")
		objs[0].Archived = true
		tables, skipped := Layout("e2e/", "e2e", objs)
		require.Equal(t, []string{"archive"}, tableNames(tables))
		require.Equal(t, []Skipped{{"archive/old.csv", reasonArchived}}, skipped)
	})
	t.Run("hudi", func(t *testing.T) {
		_, skipped := Layout("e2e/", "e2e", listed("h/.hoodie/hoodie.properties", "h/2026/a.parquet"))
		require.Equal(t, []Skipped{{"h", reasonHudi}}, skipped)
	})
	t.Run("iceberg", func(t *testing.T) {
		tables, skipped := Layout("e2e/", "e2e", listed(
			"ice/metadata/v1.metadata.json", "ice/metadata/snap-1.avro", "ice/data/p.parquet", "plain.csv"))
		require.Equal(t, []string{"plain"}, tableNames(tables))
		require.Equal(t, []Skipped{{"ice/data", reasonIceberg}, {"ice/metadata", reasonIceberg}, {"ice/metadata/snap-1.avro", reasonUnsupported}}, skipped)
	})
	t.Run("metadata json without data is a table", func(t *testing.T) {
		tables, skipped := Layout("e2e/", "e2e", listed("m/metadata/v1.metadata.json"))
		require.Equal(t, []string{"m/metadata"}, tableNames(tables))
		require.Empty(t, skipped)
	})
	t.Run("lake with only its log", func(t *testing.T) {
		tables, skipped := Layout("e2e/", "e2e", listed("t/_delta_log/0.json", "t/_delta_log/1.json"))
		require.Empty(t, tables)
		require.Equal(t, []Skipped{{"t", reasonDelta}}, skipped)
	})
	t.Run("lake at the prefix root", func(t *testing.T) {
		_, skipped := Layout("e2e/", "e2e", listed("_delta_log/0.json"))
		require.Equal(t, []Skipped{{"e2e", reasonDelta}}, skipped)
	})
	t.Run("long name", func(t *testing.T) {
		long := strings.Repeat("a", 256)
		tables, skipped := Layout("e2e/", "e2e", listed(long+".csv", strings.Repeat("b", 255)+".csv"))
		require.Equal(t, []string{strings.Repeat("b", 255)}, tableNames(tables))
		require.Equal(t, []Skipped{{long, reasonNameLong}}, skipped)
	})
	t.Run("case-insensitive extensions", func(t *testing.T) {
		tables, _ := Layout("e2e/", "e2e", listed("x/A.PARQUET", "y/b.JSONL.GZ", "z/c.Csv.Zst"))
		require.Equal(t, []string{"x", "y", "z"}, tableNames(tables))
	})
	t.Run("two compression layers", func(t *testing.T) {
		tables, _ := Layout("e2e/", "e2e", listed("t/a.csv.gz.zst"))
		require.Equal(t, FormatCSV, tables[0].Format)
	})
	t.Run("unicode keys", func(t *testing.T) {
		tables, _ := Layout("e2e/", "e2e", listed("ventes/Données_2026.csv", "注文.json"))
		require.Equal(t, []string{"ventes", "注文"}, tableNames(tables))
	})
	t.Run("several families in a folder", func(t *testing.T) {
		tables, _ := Layout("e2e/", "e2e", listed("x/part-0.csv", "x/orders_1.csv", "x/orders_2.csv"))
		require.Equal(t, []string{"x", "x/orders"}, tableNames(tables))
	})
	t.Run("root with an empty family and another", func(t *testing.T) {
		tables, _ := Layout("e2e/", "e2e", listed("part-0.csv", "orders.csv"))
		require.Equal(t, []string{"e2e", "orders"}, tableNames(tables))
	})
	t.Run("empty prefix uses the fallback", func(t *testing.T) {
		tables, _ := Layout("", "bucket", []Listed{{Key: "0001.csv", Size: 1}})
		require.Equal(t, []string{"bucket"}, tableNames(tables))
	})
	t.Run("empty segments are dropped from names", func(t *testing.T) {
		tables, _ := Layout("e2e/", "e2e", listed("a//b/x.csv"))
		require.Equal(t, []string{"a/b"}, tableNames(tables))
	})
}

func TestFormatOf(t *testing.T) {
	for key, want := range map[string]Format{
		"a.parquet": FormatParquet, "a.pq": FormatParquet, "a.json": FormatJSON, "a.jsonl": FormatJSON,
		"a.ndjson": FormatJSON, "a.csv": FormatCSV, "a.tsv": FormatCSV, "a.csv.gz": FormatCSV,
		"a.csv.gzip": FormatCSV, "a.json.zst": FormatJSON, "a.json.zstd": FormatJSON, "a.csv.bz2": FormatCSV,
		"dir.v2/a.CSV.GZ.ZST": FormatCSV, "a.csv.gz.zst.bz2": FormatCSV,
	} {
		f, ok := FormatOf(key)
		require.True(t, ok, key)
		require.Equal(t, want, f, key)
	}
	for _, key := range []string{"a.xlsx", "a", "a.gz", "a.txt.gz", ".csv.x"} {
		_, ok := FormatOf(key)
		require.False(t, ok, key)
	}
	require.Equal(t, "unknown", Format(0).String())
	require.True(t, isTSV("x/a.TSV.gz"))
	require.False(t, isTSV("x/a.csv"))
	require.False(t, isTSV("x/a.xlsx"))
}

func TestPartitions(t *testing.T) {
	ps := Partitions("orders/dt=2026-10-01/k=a%2Fb/n=__HIVE_DEFAULT_PARTITION__/bad=%zz/x=y=z/part=0.csv")
	require.Len(t, ps, 5)
	require.Equal(t, "dt", ps[0].Key)
	require.Equal(t, "2026-10-01", *ps[0].Value)
	require.Equal(t, "a/b", *ps[1].Value)
	require.Nil(t, ps[2].Value)
	require.Equal(t, "%zz", *ps[3].Value)
	require.Equal(t, "x", ps[4].Key)
	require.Equal(t, "y=z", *ps[4].Value)
	require.Empty(t, Partitions("a.csv"))
	require.Empty(t, Partitions("=v/a.csv"))
}

func TestMergeAndTypes(t *testing.T) {
	for _, tc := range []struct{ a, b, want Type }{
		{TypeInteger, TypeInteger, TypeInteger},
		{TypeNull, TypeDate, TypeDate},
		{TypeDate, TypeNull, TypeDate},
		{TypeInteger, TypeFloat, TypeFloat},
		{TypeDecimal, TypeInteger, TypeFloat},
		{TypeDate, TypeTimestamp, TypeTimestamp},
		{TypeTimestamp, TypeDate, TypeTimestamp},
		{TypeJSON, TypeString, TypeJSON},
		{TypeBoolean, TypeJSON, TypeJSON},
		{TypeBoolean, TypeInteger, TypeString},
		{TypeTimestamp, TypeTimestampTZ, TypeString},
		{TypeBinary, TypeString, TypeString},
	} {
		require.Equal(t, tc.want, join(tc.a, tc.b), "%s %s", tc.a, tc.b)
	}
	for i, n := range typeNames {
		require.Equal(t, n, Type(i).String())
	}
	require.Equal(t, "unknown", Type(200).String())
	merged := Merge(
		[]Column{{Name: "a", Type: TypeInteger}, {Name: "a", Type: TypeInteger}},
		[]Column{{Name: "a", Type: TypeInteger, Nullable: true}, {Name: "b", Type: TypeString}},
	)
	require.Equal(t, []Column{{Name: "a", Type: TypeInteger, Nullable: true}, {Name: "b", Type: TypeString, Nullable: true}}, merged)
	require.Nil(t, Merge())
}
