package filesource_test

import (
	"errors"
	"fmt"

	"github.com/helixdb/helix-foundry/backend/filesource"
)

func ExampleFormatOf() {
	for _, key := range []string{"orders/part-0.snappy.parquet", "events/2026-10-01.jsonl.gz", "notes.xlsx"} {
		f, ok := filesource.FormatOf(key)
		fmt.Println(key, f, ok)
	}
	// Output:
	// orders/part-0.snappy.parquet parquet true
	// events/2026-10-01.jsonl.gz json true
	// notes.xlsx unknown false
}

func ExampleLayout() {
	objs := []filesource.Listed{
		{Key: "e2e/customers.csv", Size: 10},
		{Key: "e2e/orders/dt=2026-10-01/part-00000-0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0-c000.snappy.parquet", Size: 10},
		{Key: "e2e/events/2026-10-01.jsonl.gz", Size: 10},
		{Key: "e2e/events/2026-10-02.jsonl.zst", Size: 10},
		{Key: "e2e/notes.xlsx", Size: 10},
		{Key: "e2e/_SUCCESS", Size: 0},
	}
	tables, skipped := filesource.Layout("e2e/", "e2e", objs)
	for _, t := range tables {
		fmt.Println(t.Name, t.Format, len(t.Files))
	}
	for _, s := range skipped {
		fmt.Printf("%s: %s\n", s.Name, s.Reason)
	}
	// Output:
	// customers csv 1
	// events json 2
	// orders parquet 1
	// notes.xlsx: unsupported file type
}

func ExamplePartitions() {
	for _, p := range filesource.Partitions("orders/dt=2026-10-01/region=eu%2Fwest/x=__HIVE_DEFAULT_PARTITION__/part-0.parquet") {
		if p.Value == nil {
			fmt.Println(p.Key, "null")
			continue
		}
		fmt.Println(p.Key, *p.Value)
	}
	// Output:
	// dt 2026-10-01
	// region eu/west
	// x null
}

func ExampleMerge() {
	a := []filesource.Column{{Name: "id", Type: filesource.TypeInteger}, {Name: "amount", Type: filesource.TypeInteger}}
	b := []filesource.Column{{Name: "id", Type: filesource.TypeInteger}, {Name: "amount", Type: filesource.TypeFloat}, {Name: "note", Type: filesource.TypeString}}
	for _, c := range filesource.Merge(a, b) {
		fmt.Println(c.Name, c.Type, c.Nullable)
	}
	// Output:
	// id integer false
	// amount float false
	// note string true
}

func ExampleError() {
	err := error(&filesource.Error{File: "orders/p.json", Record: 12, Err: filesource.ErrNotObject})
	fmt.Println(err)
	fmt.Println(errors.Is(err, filesource.ErrNotObject))
	// Output:
	// orders/p.json record 12: not a JSON object
	// true
}

func ExampleCompression() {
	for _, head := range [][]byte{{0x1f, 0x8b, 8, 0}, []byte("PK\x03\x04"), []byte("id,name\n"), []byte("PAR1")} {
		fmt.Printf("%q\n", filesource.Compression(head))
	}
	// Output:
	// "gzip"
	// "zip"
	// ""
	// ""
}
