package read

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/klauspost/compress/zstd"

	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// store is the documents bucket in memory: objects by key, the writes it
// took, and a put that fails when putErr is set.
type store struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
	puts    int
	putErr  error
}

func newStore() *store { return &store{objects: map[string][]byte{}, types: map[string]string{}} }

func (s *store) Open(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, document.ErrMissing
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *store) PutOnce(_ context.Context, key, contentType string, body io.ReadSeeker) error {
	if s.putErr != nil {
		return s.putErr
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	if _, ok := s.objects[key]; !ok {
		s.objects[key], s.types[key] = data, contentType
	}
	return nil
}

// upload stores data as a document of mediaType named name, and returns it.
func (s *store) upload(name string, mediaType model.FoundryMediaType, data []byte) *model.FoundryDocument {
	doc := &model.FoundryDocument{
		ID: "doc-1", WorkspaceID: "ws-1", Name: name, MediaType: mediaType, SizeBytes: int64(len(data)), S3Key: document.Key("ws-1", "doc-1"),
		Status: model.FoundryDocumentUploaded,
	}
	s.objects[doc.S3Key] = data
	return doc
}

// rows is what a read stored at the rows key, one row a line.
func (s *store) rows(t *testing.T) string {
	t.Helper()
	data, ok := s.objects[document.RowsKey("ws-1", "doc-1")]
	if !ok {
		t.Fatal("no rows stored")
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func tables(t *testing.T, s *store) *Tables {
	return &Tables{Store: s, TempDir: t.TempDir(), Limits: DocumentLimits}
}

// readOK reads doc and wants rows read.
func readOK(t *testing.T, r *Tables, doc *model.FoundryDocument) model.FoundryTableRead {
	t.Helper()
	outcome, err := r.Read(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	read, ok := outcome.(model.FoundryTableRead)
	if !ok {
		t.Fatalf("outcome = %+v, want rows", outcome)
	}
	return read
}

// readBad reads doc and wants it unreadable.
func readBad(t *testing.T, r *Tables, doc *model.FoundryDocument) model.FoundryFileReadError {
	t.Helper()
	outcome, err := r.Read(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	bad, ok := outcome.(model.FoundryFileUnreadable)
	if !ok {
		t.Fatalf("outcome = %+v, want unreadable", outcome)
	}
	return bad.Error
}

func TestTables(t *testing.T) {
	// A CSV becomes typed rows, with its delimiter, quoting, line endings
	// and charset sniffed as a synced S3 file's are, stored once beside it.
	t.Run("converts a CSV to typed rows", func(t *testing.T) {
		for _, tc := range []struct {
			name, csv, rows string
		}{
			{"semicolons and a BOM", "\xef\xbb\xbfid;name;amount;active\n1;Acme;10.50;true\n2;Globex;;false\n",
				`{"id":1,"name":"Acme","amount":10.50,"active":true}` + "\n" + `{"id":2,"name":"Globex","amount":null,"active":false}` + "\n"},
			{"quoted commas and line breaks, CRLF", "id,note\r\n1,\"a, b\"\r\n2,\"line\nbreak\"\r\n",
				`{"id":1,"note":"a, b"}` + "\n" + `{"id":2,"note":"line\nbreak"}` + "\n"},
			{"Latin-1 text", "id,city\n1,M\xfcnchen\n", `{"id":1,"city":"München"}` + "\n"},
			{"a header alone", "id,name\n", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore()
				read := readOK(t, tables(t, s), s.upload("orders.csv", model.FoundryMediaCSV, []byte(tc.csv)))
				if read.ArtifactKey != document.RowsKey("ws-1", "doc-1") || read.Rows != int64(strings.Count(tc.rows, "\n")) {
					t.Fatalf("read = %+v", read)
				}
				if got := s.rows(t); got != tc.rows {
					t.Fatalf("rows\n got %q\nwant %q", got, tc.rows)
				}
				if s.types[read.ArtifactKey] != "application/gzip" {
					t.Fatalf("stored as %s", s.types[read.ArtifactKey])
				}
			})
		}
	})

	// A TSV reads tabs by its type, even after a rename changed its name.
	t.Run("reads a TSV by its type", func(t *testing.T) {
		for _, name := range []string{"orders.tsv", "orders renamed.txt", "orders"} {
			s := newStore()
			readOK(t, tables(t, s), s.upload(name, model.FoundryMediaTSV, []byte("id\tname\n1\tAcme, Inc\n")))
			if got := s.rows(t); got != `{"id":1,"name":"Acme, Inc"}`+"\n" {
				t.Fatalf("%s: rows %q", name, got)
			}
		}
	})

	// A CSV filesource can't read fails for good, naming the record.
	t.Run("refuses a ragged CSV", func(t *testing.T) {
		s := newStore()
		bad := readBad(t, tables(t, s), s.upload("orders.csv", model.FoundryMediaCSV, []byte("id,name\n1,a\n2,b,extra\n")))
		if bad.Code != model.FoundryFileReadUnsupported || !strings.Contains(bad.Message, "orders.csv record 2") {
			t.Fatalf("error = %+v", bad)
		}
		if len(s.objects) != 1 {
			t.Fatal("stored rows of an unreadable file")
		}
	})

	// Parquet becomes rows with its types: integers, text, timestamps and
	// nested values as JSON.
	t.Run("converts Parquet to rows", func(t *testing.T) {
		sc := arrow.NewSchema([]arrow.Field{
			{Name: "id", Type: arrow.PrimitiveTypes.Int64},
			{Name: "name", Type: arrow.BinaryTypes.String, Nullable: true},
			{Name: "placed_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}},
			{Name: "tags", Type: arrow.ListOf(arrow.BinaryTypes.String), Nullable: true},
		}, nil)
		rec := jsonRecord(t, sc, `[{"id":1,"name":"Acme","placed_at":"2026-10-01T09:30:00Z","tags":["a","b"]},`+
			`{"id":2,"name":null,"placed_at":"2026-10-02T10:00:00Z","tags":null}]`)
		s := newStore()
		read := readOK(t, tables(t, s), s.upload("orders.parquet", model.FoundryMediaParquet, parquetBytes(t, sc, compress.Codecs.Snappy, rec)))
		want := `{"id":1,"name":"Acme","placed_at":"2026-10-01T09:30:00Z","tags":["a","b"]}` + "\n" +
			`{"id":2,"name":null,"placed_at":"2026-10-02T10:00:00Z","tags":null}` + "\n"
		if got := s.rows(t); read.Rows != 2 || got != want {
			t.Fatalf("read %d rows\n got %q\nwant %q", read.Rows, got, want)
		}
	})

	// Bytes that are not Parquet fail for good.
	t.Run("refuses a file that is not Parquet", func(t *testing.T) {
		s := newStore()
		bad := readBad(t, tables(t, s), s.upload("orders.parquet", model.FoundryMediaParquet, []byte("id,name\n1,a\n")))
		if bad.Code != model.FoundryFileReadUnsupported || !strings.Contains(bad.Message, "Parquet") {
			t.Fatalf("error = %+v", bad)
		}
	})

	// JSON Lines is checked and sent as it is: the original is what loads
	// read, and nothing more is stored.
	t.Run("checks JSON Lines and keeps the original", func(t *testing.T) {
		s := newStore()
		doc := s.upload("events.jsonl", model.FoundryMediaJSONL, []byte("{\"id\":1}\n\n{\"id\":2,\"tags\":[{\"a\":1}]}\n"))
		read := readOK(t, tables(t, s), doc)
		if read.ArtifactKey != doc.S3Key || read.Rows != 2 || s.puts != 0 {
			t.Fatalf("read = %+v after %d puts", read, s.puts)
		}
	})

	// A JSON Lines file the engine can't read fails for good, naming the
	// record.
	t.Run("refuses a bad JSON Lines record", func(t *testing.T) {
		for _, tc := range []struct {
			name, data string
			code       model.FoundryFileReadErrorCode
			message    string
		}{
			{"not JSON", "{\"id\":1}\n{\"id\":\n", model.FoundryFileReadUnsupported, "record 2: it is not valid JSON"},
			{"broken JSON", "{\"id\":1}\n{\"id\" 2}\n", model.FoundryFileReadUnsupported, "record 2: it is not valid JSON at byte"},
			{"not an object", "{\"id\":1}\n[1,2]\n", model.FoundryFileReadUnsupported, "record 2: it is not a JSON object"},
			{"a scalar", "{\"id\":1}\n3\n", model.FoundryFileReadUnsupported, "record 2: it is not a JSON object"},
			{"a record past 1 MiB", "{\"id\":1}\n{\"s\":\"" + strings.Repeat("a", 1<<20) + "\"}\n", model.FoundryFileReadTooLarge, "record 2: it is larger than 1024 KiB"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore()
				bad := readBad(t, tables(t, s), s.upload("events.jsonl", model.FoundryMediaJSONL, []byte(tc.data)))
				if bad.Code != tc.code || !strings.Contains(bad.Message, tc.message) {
					t.Fatalf("error = %+v, want %s %q", bad, tc.code, tc.message)
				}
			})
		}
	})

	// A file compressed with any codec is refused before it is read: its
	// size was checked compressed, and it would grow past it.
	t.Run("refuses a compressed file", func(t *testing.T) {
		gz := func(data []byte) []byte {
			var b bytes.Buffer
			w := gzip.NewWriter(&b)
			_, _ = w.Write(data)
			_ = w.Close()
			return b.Bytes()
		}
		enc, _ := zstd.NewWriter(nil)
		for _, tc := range []struct {
			name      string
			mediaType model.FoundryMediaType
			data      []byte
			codec     string
		}{
			{"gzip CSV", model.FoundryMediaCSV, gz([]byte("id\n1\n")), "gzip"},
			{"zstd JSON Lines", model.FoundryMediaJSONL, enc.EncodeAll([]byte("{\"id\":1}\n"), nil), "zstd"},
			{"zipped JSON", model.FoundryMediaJSON, []byte("PK\x03\x04[{\"id\":1}]"), "zip"},
		} {
			s := newStore()
			bad := readBad(t, tables(t, s), s.upload("f", tc.mediaType, tc.data))
			if bad.Code != model.FoundryFileReadUnsupported || bad.Message != "it is compressed with "+tc.codec+"; upload the file uncompressed" {
				t.Fatalf("%s: error = %+v", tc.name, bad)
			}
		}
	})

	// Converted rows can grow far past the file: empty cells under long
	// column names. Past the limit the file is too large, and nothing is
	// stored.
	t.Run("refuses rows past the limit", func(t *testing.T) {
		var csv strings.Builder
		for i := range 50 {
			if i > 0 {
				csv.WriteByte(',')
			}
			csv.WriteString(strings.Repeat("c", 200) + string(rune('a'+i%26)) + string(rune('a'+i/26)))
		}
		csv.WriteByte('\n')
		for range 2000 {
			csv.WriteString(strings.Repeat(",", 49) + "\n")
		}
		s := newStore()
		r := tables(t, s)
		r.Limits.MaxDecompressedBytes = 1 << 20
		bad := readBad(t, r, s.upload("wide.csv", model.FoundryMediaCSV, []byte(csv.String())))
		if bad.Code != model.FoundryFileReadTooLarge || bad.Message != "its rows come to more than 1 MiB as JSON" || s.puts != 0 {
			t.Fatalf("error = %+v after %d puts", bad, s.puts)
		}
	})

	// A file whose bytes are gone (deleted under the worker) can't be read;
	// a store that fails is a failure to retry, not the file's.
	t.Run("tells a missing file from a failing store", func(t *testing.T) {
		s := newStore()
		doc := s.upload("orders.csv", model.FoundryMediaCSV, []byte("id\n1\n"))
		delete(s.objects, doc.S3Key)
		if bad := readBad(t, tables(t, s), doc); bad.Code != model.FoundryFileReadUnsupported {
			t.Fatalf("error = %+v", bad)
		}
		s = newStore()
		s.putErr = errors.New("s3 is down")
		doc = s.upload("orders.csv", model.FoundryMediaCSV, []byte("id\n1\n"))
		if outcome, err := tables(t, s).Read(context.Background(), doc); err == nil {
			t.Fatalf("outcome = %+v with no error", outcome)
		}
	})

	// A read writes its rows once: reading the file again stores nothing
	// new and reads the same.
	t.Run("reads a file again the same", func(t *testing.T) {
		s := newStore()
		doc := s.upload("orders.csv", model.FoundryMediaCSV, []byte("id,name\n1,Acme\n"))
		first := readOK(t, tables(t, s), doc)
		rows := s.rows(t)
		if second := readOK(t, tables(t, s), doc); second != first || s.rows(t) != rows {
			t.Fatalf("second read %+v, first %+v", second, first)
		}
	})

	// The worker hands each reader its own kind only.
	t.Run("reads no other kind", func(t *testing.T) {
		s := newStore()
		if _, err := tables(t, s).Read(context.Background(), s.upload("a.md", model.FoundryMediaMarkdown, []byte("# a"))); err == nil {
			t.Fatal("read Markdown as a table")
		}
	})
}

func TestCheckJSON(t *testing.T) {
	// Each JSON shape is read as the engine catalogs it: the arrays of
	// objects reached through objects are lists of records, wherever they
	// are, and each counts its records; arrays inside records are fields
	// of them, not lists.
	for _, tc := range []struct {
		name, data string
		records    int64
	}{
		{"an array of records", `[{"id":1},{"id":2}]`, 2},
		{"records under a key", `{"object":"list","data":[{"id":1},{"id":2},{"id":3}],"has_more":false}`, 3},
		{"two lists and a nested array", `{"orders":[{"id":1,"lines":[{"sku":"a"},{"sku":"b"}]}],"customers":[{"id":7},{"id":8}]}`, 3},
		{"records deep in objects", `{"a":{"b":{"c":[{"id":1}]}}}`, 1},
		{"an empty list", `{"data":[]}`, 0},
		{"a list beside scalars and nulls", `{"n":1,"s":"x","b":true,"z":null,"f":1.5e3,"data":[{"id":1}]}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records, bad := CheckJSON([]byte(tc.data), 64)
			if bad != nil || records != tc.records {
				t.Fatalf("CheckJSON = (%d, %+v), want %d", records, bad, tc.records)
			}
		})
	}
	// A document with no list of records can't be loaded, and one the
	// engine's parser refuses can't be read: both fail for good, saying
	// why.
	for _, tc := range []struct {
		name, data string
		code       model.FoundryFileReadErrorCode
		message    string
	}{
		{"an object without a list", `{"id":1,"name":"Acme"}`, model.FoundryFileReadUnsupported, "it holds no list of records"},
		{"an array of scalars", `[1,2,3]`, model.FoundryFileReadUnsupported, "it holds no list of records"},
		{"an array of arrays", `[[{"id":1}]]`, model.FoundryFileReadUnsupported, "it holds no list of records"},
		{"a list mixing records and scalars", `{"data":[{"id":1},2]}`, model.FoundryFileReadUnsupported, "it holds no list of records"},
		{"a scalar", `42`, model.FoundryFileReadUnsupported, "it holds no list of records"},
		{"two values", `[{"id":1}] [{"id":2}]`, model.FoundryFileReadUnsupported, "it holds more than one JSON value"},
		{"trailing garbage", `[{"id":1}] }`, model.FoundryFileReadUnsupported, "it is not valid JSON"},
		{"cut short", `[{"id":1}`, model.FoundryFileReadUnsupported, "it ends before its last value does"},
		{"broken", `[{"id" 1}]`, model.FoundryFileReadUnsupported, "it is not valid JSON at byte"},
		{"empty", ``, model.FoundryFileReadUnsupported, "it is empty"},
		{"not UTF-8", "[{\"name\":\"M\xfcnchen\"}]", model.FoundryFileReadUnsupported, "it is not UTF-8 text"},
		{"a byte order mark", "\xef\xbb\xbf[{\"id\":1}]", model.FoundryFileReadUnsupported, "it is not valid JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, bad := CheckJSON([]byte(tc.data), 64)
			if bad == nil || bad.Error.Code != tc.code || !strings.Contains(bad.Error.Message, tc.message) {
				t.Fatalf("CheckJSON = %+v, want %s %q", bad, tc.code, tc.message)
			}
		})
	}
}

func TestJSONDepth(t *testing.T) {
	// nested is records nested depth levels deep, the list's own bracket
	// level 1.
	nested := func(depth int) string {
		inner := strings.Repeat(`{"a":`, depth-2) + "1" + strings.Repeat("}", depth-2)
		return `[{"x":` + inner + `}]`
	}
	// Nesting stops at 64 levels, half the engine parser's 128: one more
	// is too large, as JSON and as a JSON Lines record.
	t.Run("reads 64 levels and refuses 65", func(t *testing.T) {
		if _, bad := CheckJSON([]byte(nested(64)), 64); bad != nil {
			t.Fatalf("64 levels: %+v", bad)
		}
		_, bad := CheckJSON([]byte(nested(65)), 64)
		if bad == nil || bad.Error.Code != model.FoundryFileReadTooLarge || bad.Error.Message != "it is nested more than 64 levels deep" {
			t.Fatalf("65 levels: %+v", bad)
		}
		line := strings.Repeat(`{"a":`, 64) + "1" + strings.Repeat("}", 64)
		if _, bad := CheckJSONLines([]byte(line+"\n"), 64, 1<<20); bad != nil {
			t.Fatalf("a 64-level record: %+v", bad)
		}
		_, bad = CheckJSONLines([]byte("{}\n{\"b\":"+line+"}\n"), 64, 1<<20)
		if bad == nil || bad.Error.Code != model.FoundryFileReadTooLarge || bad.Error.Message != "record 2: it is nested more than 64 levels deep" {
			t.Fatalf("a 65-level record: %+v", bad)
		}
	})
	// The walk keeps one frame a level, so a megabyte of brackets is
	// refused at the limit, not after.
	t.Run("stops at the limit", func(t *testing.T) {
		_, bad := CheckJSON([]byte(strings.Repeat("[", 1<<20)), 64)
		if bad == nil || bad.Error.Code != model.FoundryFileReadTooLarge {
			t.Fatalf("a megabyte of brackets: %+v", bad)
		}
	})
}

// jsonRecord builds one record batch of sc from a JSON array of rows.
func jsonRecord(t *testing.T, sc *arrow.Schema, rows string) arrow.RecordBatch {
	t.Helper()
	rec, _, err := array.RecordFromJSON(memory.DefaultAllocator, sc, strings.NewReader(rows))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// parquetBytes writes records as one Parquet file compressed with codec.
func parquetBytes(t testing.TB, sc *arrow.Schema, codec compress.Compression, recs ...arrow.RecordBatch) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := pqarrow.NewFileWriter(sc, &buf, parquet.NewWriterProperties(parquet.WithCompression(codec)), pqarrow.DefaultWriterProps())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
