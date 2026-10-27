package filesource

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

var fixturesDir = flag.String("fixtures", "", "write the S3 end-to-end fixture set to this directory")

// TestWriteE2EFixtures writes the files the dev verification uploads to a
// customer bucket under e2e/:
//
//	go test ./filesource -run TestWriteE2EFixtures -fixtures=<dir>
//
// archive/old.csv is uploaded with --storage-class GLACIER.
func TestWriteE2EFixtures(t *testing.T) {
	if *fixturesDir == "" {
		t.Skip("pass -fixtures=<dir> to write the fixture set")
	}
	writeE2EFixtures(t, *fixturesDir)
}

// TestE2EFixturesRead checks the fixture set reads as the verification
// expects: five tables, the spreadsheet skipped, every row whole.
func TestE2EFixturesRead(t *testing.T) {
	dir := t.TempDir()
	writeE2EFixtures(t, dir)
	var objs []Listed
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		objs = append(objs, Listed{Key: "e2e/" + filepath.ToSlash(rel), Size: info.Size()})
		return nil
	}))
	tables, skipped := Layout("e2e/", "e2e", objs)
	require.Equal(t, []string{"api", "archive", "customers", "events", "orders", "products"}, tableNames(tables))
	require.Equal(t, []Skipped{{"notes.xlsx", reasonUnsupported}}, skipped)
	want := map[string]struct {
		rows int
		key  []string
	}{
		"api": {3, []string{"id"}}, "archive": {2, []string{"id"}}, "customers": {1000, []string{"id"}},
		"events": {20, []string{"id"}}, "orders": {8, []string{"id"}}, "products": {4, []string{"id"}},
	}
	for _, d := range tables {
		var files []File
		for _, o := range d.Files {
			rel := strings.TrimPrefix(o.Key, "e2e/")
			files = append(files, File{Path: filepath.Join(dir, rel), Name: o.Key, Format: d.Format, Partitions: Partitions(rel)})
		}
		r := scanAll(t, files...)
		require.Len(t, r.rows, want[d.Name].rows, d.Name)
		require.Equal(t, want[d.Name].key, r.key, d.Name)
		if d.Name == "orders" {
			require.Equal(t, map[string]Type{"id": TypeInteger, "amount": TypeDecimal, "placed_at": TypeTimestampTZ,
				"lines": TypeJSON, "shipping": TypeJSON, "dt": TypeString}, colTypes(r.cols))
			require.Contains(t, r.raw[0], `"amount":12.30`)
			require.Contains(t, r.raw[0], `"placed_at":"2026-10-01T09:30:00Z"`)
			require.Contains(t, r.raw[0], `"dt":"2026-10-01"`)
		}
		if d.Name == "events" {
			require.Equal(t, []string{"id", "kind", "at", "user", "page"}, colNames(r.cols))
		}
	}
}

func writeE2EFixtures(t *testing.T, dir string) {
	t.Helper()
	write := func(rel string, data []byte) {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, data, 0o644))
	}

	var customers strings.Builder
	customers.WriteString("\xef\xbb\xbfid;name;country;lifetime_value;active\n")
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&customers, "%d;Customer %d;%s;%d.%02d;%t\n", i, i, []string{"GB", "FR", "DE", "US"}[i%4], i*7, i%100, i%3 != 0)
	}
	write("customers.csv", []byte(customers.String()))

	orders := arrow.NewSchema([]arrow.Field{
		col("id", arrow.PrimitiveTypes.Int64),
		col("amount", &arrow.Decimal128Type{Precision: 10, Scale: 2}),
		col("placed_at", &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}),
		col("lines", arrow.ListOf(arrow.StructOf(col("sku", arrow.BinaryTypes.String), col("qty", arrow.PrimitiveTypes.Int32)))),
		col("shipping", arrow.StructOf(col("city", arrow.BinaryTypes.String), col("express", arrow.FixedWidthTypes.Boolean))),
	}, nil)
	orderRows := func(day, first int) string {
		var rows []string
		for i := first; i < first+4; i++ {
			shipping := `{"city":"London","express":true}`
			if i%2 == 0 {
				shipping = "null"
			}
			rows = append(rows, fmt.Sprintf(`{"id":%d,"amount":"%d.30","placed_at":"2026-10-%02dT09:30:00Z","lines":[{"sku":"A-%d","qty":%d}],"shipping":%s}`,
				i, 10+i, day, i, i, shipping))
		}
		return "[" + strings.Join(rows, ",") + "]"
	}
	for _, o := range []struct {
		key   string
		codec compress.Compression
		day   int
	}{
		{"orders/dt=2026-10-01/part-00000-0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0-c000.snappy.parquet", compress.Codecs.Snappy, 1},
		{"orders/dt=2026-10-02/part-00000-1a2b3c4d-5e6f-4a5b-8c7d-0e1f2a3b4c5d-c000.zstd.parquet", compress.Codecs.Zstd, 2},
	} {
		props := parquet.NewWriterProperties(parquet.WithCompression(o.codec))
		write(o.key, parquetBytes(t, orders, props, pqarrow.DefaultWriterProps(), jsonRecord(t, orders, orderRows(o.day, 2+(o.day-1)*4))))
	}

	var day1, day2 bytes.Buffer
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&day1, `{"id":%d,"kind":"view","at":"2026-10-01T10:%02d:00Z","user":%d}`+"\n", i, i, i%3)
		fmt.Fprintf(&day2, `{"id":%d,"kind":"click","at":"2026-10-02T10:%02d:00Z","page":"/p/%d"}`+"\n", 10+i, i, i)
	}
	write("events/2026-10-01.jsonl.gz", gzipBytes(t, day1.Bytes()))
	write("events/2026-10-02.jsonl.zst", zstdBytes(t, day2.Bytes(), zstd.WithEncoderLevel(zstd.SpeedDefault)))

	write("products.json", []byte(`[{"id":"p1","name":"Desk","price":120.5,"tags":["office"]},{"id":"p2","name":"Chair","price":60,"tags":[]},`+
		`{"id":"p3","name":"Lamp","price":15.25,"tags":["light","office"]},{"id":"p4","name":"Rug","price":null,"tags":null}]`))
	write("api/invoices.json", []byte(`{"object":"list","url":"/v1/invoices","has_more":false,"data":[`+
		`{"id":"in_1","customer":1,"total":1200,"status":"paid"},{"id":"in_2","customer":2,"total":500,"status":"open"},`+
		`{"id":"in_3","customer":1,"total":0,"status":"void","metadata":{"note":"refund"}}]}`))
	write("_SUCCESS", []byte("ok"))
	write("notes.xlsx", []byte("PK\x03\x04 not really a spreadsheet"))
	write("archive/old.csv", []byte("id,name\n1,old\n2,older\n"))
}
