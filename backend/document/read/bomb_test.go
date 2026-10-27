package read

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/compress"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// TestTablesParquetBomb reads Parquet files that fit the 3 MiB table cap
// and decode to hundreds of MiB: just under and just over the 256 MiB of
// rows a read may write, and a gigabyte. The heap must stay far below the
// document worker's 3 GiB pod. Building the largest takes a gigabyte, so it
// runs on request, in a container with the pod's limit:
//
//	HELIX_READ_BOMB=1 go test ./document/read -run TestTablesParquetBomb -v
func TestTablesParquetBomb(t *testing.T) {
	if os.Getenv("HELIX_READ_BOMB") == "" {
		t.Skip("set HELIX_READ_BOMB=1 to read the Parquet bombs")
	}
	sc := arrow.NewSchema([]arrow.Field{{Name: "id", Type: arrow.PrimitiveTypes.Int64}, {Name: "s", Type: arrow.BinaryTypes.String}}, nil)
	value := strings.Repeat("x", 4<<10)
	bomb := func(rows int) []byte {
		ids := array.NewInt64Builder(memory.DefaultAllocator)
		strs := array.NewStringBuilder(memory.DefaultAllocator)
		for i := range rows {
			ids.Append(int64(i))
			strs.Append(value)
		}
		rec := array.NewRecordBatch(sc, []arrow.Array{ids.NewArray(), strs.NewArray()}, int64(rows))
		return parquetBytes(t, sc, compress.Codecs.Zstd, rec)
	}
	for _, tc := range []struct {
		name string
		rows int
		want string
	}{
		{"just under the rows limit", 64_000, "read"},
		{"just over the rows limit", 66_000, "too_large"},
		{"a gigabyte of rows", 240_000, "too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bomb(tc.rows)
			if len(data) > 3<<20 {
				t.Fatalf("the bomb is %d bytes, past the 3 MiB table cap", len(data))
			}
			runtime.GC()
			var peak atomic.Uint64
			done := make(chan struct{})
			go func() {
				var m runtime.MemStats
				for {
					select {
					case <-done:
						return
					case <-time.After(5 * time.Millisecond):
						runtime.ReadMemStats(&m)
						if m.HeapInuse > peak.Load() {
							peak.Store(m.HeapInuse)
						}
					}
				}
			}()
			s := newStore()
			outcome, err := tables(t, s).Read(context.Background(), s.upload("bomb.parquet", model.FoundryMediaParquet, data))
			close(done)
			if err != nil {
				t.Fatal(err)
			}
			got := "read"
			if bad, ok := outcome.(model.FoundryFileUnreadable); ok {
				got = string(bad.Error.Code)
			}
			t.Logf("%d-byte file, %d rows: %s, peak heap %d MiB", len(data), tc.rows, got, peak.Load()>>20)
			if got != tc.want {
				t.Fatalf("outcome = %+v, want %s", outcome, tc.want)
			}
			if peak.Load() > 1<<30 {
				t.Fatalf("peak heap %d MiB, past 1 GiB", peak.Load()>>20)
			}
		})
	}
}
