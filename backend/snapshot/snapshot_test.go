package snapshot

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 is a path-style object store that keeps every PUT body in memory.
// It serves suffix ranges unless ignoreRange, lists two keys a page unless
// denyList, and refuses to delete the keys in refuse; ranges, lists and
// deletes record the requests it saw.
type fakeS3 struct {
	mu          sync.Mutex
	objects     map[string][]byte
	ignoreRange bool
	denyList    bool
	refuse      map[string]bool
	ranges      []string
	lists       int
	deletes     int
}

func newS3Store(t *testing.T) (*S3, *fakeS3) {
	t.Helper()
	const bucket = "/foundry-data/"
	fake := &fakeS3{objects: map[string][]byte{}, refuse: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch {
		case r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			fake.objects[r.URL.Path] = body
		case r.Method == http.MethodDelete:
			delete(fake.objects, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Query().Has("delete"):
			fake.deletes++
			var req struct {
				Objects []struct{ Key string } `xml:"Object"`
			}
			if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `<DeleteResult>`)
			for _, o := range req.Objects {
				if fake.refuse[o.Key] {
					fmt.Fprintf(w, `<Error><Key>%s</Key><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`, o.Key)
					continue
				}
				delete(fake.objects, bucket+o.Key)
			}
			_, _ = io.WriteString(w, `</DeleteResult>`)
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			fake.lists++
			if fake.denyList {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
				return
			}
			var keys []string
			for path := range fake.objects {
				key := strings.TrimPrefix(path, bucket)
				if strings.HasPrefix(key, r.URL.Query().Get("prefix")) && key > r.URL.Query().Get("continuation-token") {
					keys = append(keys, key)
				}
			}
			slices.Sort(keys)
			page, more := keys, false
			if len(keys) > 2 {
				page, more = keys[:2], true
			}
			fmt.Fprintf(w, `<ListBucketResult><KeyCount>%d</KeyCount><IsTruncated>%t</IsTruncated>`, len(page), more)
			if more {
				fmt.Fprintf(w, `<NextContinuationToken>%s</NextContinuationToken>`, page[len(page)-1])
			}
			for _, k := range page {
				fmt.Fprintf(w, `<Contents><Key>%s</Key><Size>%d</Size></Contents>`, k, len(fake.objects[bucket+k]))
			}
			_, _ = io.WriteString(w, `</ListBucketResult>`)
		case r.Method == http.MethodGet:
			body, ok := fake.objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`))
				return
			}
			if rng := r.Header.Get("Range"); rng != "" {
				fake.ranges = append(fake.ranges, rng)
				n, err := strconv.Atoi(strings.TrimPrefix(rng, "bytes=-"))
				if err == nil && !fake.ignoreRange {
					start := max(len(body)-n, 0)
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
					w.WriteHeader(http.StatusPartialContent)
					body = body[start:]
				}
			}
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(server.Close)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(server.URL), UsePathStyle: true, Region: "us-east-2",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	})
	return NewS3(client, "foundry-data"), fake
}

func readLines(t *testing.T, r io.Reader) []string {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	scanner := bufio.NewScanner(gz)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestSnapshot(t *testing.T) {
	// A reader can tell an object that was never written from a failure to
	// read it: an enrichment step resumes from the files it finds.
	t.Run("reports a missing object as ErrMissing", func(t *testing.T) {
		store, _ := newS3Store(t)
		if _, err := store.Open(context.Background(), "ws/w/none.jsonl.gz"); !errors.Is(err, ErrMissing) {
			t.Fatalf("open missing = %v, want ErrMissing", err)
		}
	})

	// API rows and database rows land as one gzipped NDJSON object per table.
	t.Run("uploads rows as gzipped ndjson", func(t *testing.T) {
		store, fake := newS3Store(t)
		w, err := NewWriter(t.TempDir(), 10, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		if err := w.Write(map[string]any{"customer_id": "cus_1", "note": "<b>&</b>"}); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteJSON([]byte(`{"customer_id":"cus_2","email":"a@b.c"}`)); err != nil {
			t.Fatal(err)
		}
		key := Key("ws-1", "conn-1", "public/orders", "sync-1")
		if err := w.Upload(context.Background(), store, key); err != nil {
			t.Fatal(err)
		}

		if key != "ws/ws-1/connectors/conn-1/public%2Forders/sync-1.jsonl.gz" {
			t.Fatalf("key = %q", key)
		}
		body, ok := fake.objects["/foundry-data/"+key]
		if !ok {
			t.Fatalf("objects = %v", fake.objects)
		}
		lines := readLines(t, strings.NewReader(string(body)))
		want := []string{`{"customer_id":"cus_1","note":"<b>&</b>"}`, `{"customer_id":"cus_2","email":"a@b.c"}`}
		if !slices.Equal(lines, want) || w.Rows() != 2 {
			t.Fatalf("lines = %q, rows = %d", lines, w.Rows())
		}
		opened, err := store.Open(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		if got := readLines(t, opened); !slices.Equal(got, want) {
			t.Fatalf("read back %q", got)
		}
		if err := store.Delete(context.Background(), key); err != nil || len(fake.objects) != 0 {
			t.Fatalf("Delete = %v, objects = %v", err, fake.objects)
		}
	})
	// Columns of an API source are only known from its rows.
	t.Run("collects columns in the order first seen", func(t *testing.T) {
		w, err := NewWriter(t.TempDir(), 10, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		_ = w.Write(map[string]any{"id": 1, "b": 2, "a": 3})
		_ = w.Write(map[string]any{"id": 2, "c": 4})
		if got := w.Columns(); !slices.Equal(got, []string{"a", "b", "id", "c"}) {
			t.Fatalf("columns = %v", got)
		}
	})
	t.Run("stops at the row limit", func(t *testing.T) {
		w, err := NewWriter(t.TempDir(), 1, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		if err := w.WriteJSON([]byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteJSON([]byte(`{}`)); !errors.Is(err, ErrRowLimit) {
			t.Fatalf("second row error = %v, want ErrRowLimit", err)
		}
	})
	// Rows count uncompressed with their newline: 9 bytes fit {"a":1}\n once.
	t.Run("stops at the byte limit", func(t *testing.T) {
		w, err := NewWriter(t.TempDir(), 10, 9)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		if err := w.WriteJSON([]byte(`{"a":1}`)); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteJSON([]byte(`{}`)); !errors.Is(err, ErrByteLimit) || w.Rows() != 1 {
			t.Fatalf("second row error = %v, rows = %d, want ErrByteLimit", err, w.Rows())
		}
		exact, err := NewWriter(t.TempDir(), 10, 16)
		if err != nil {
			t.Fatal(err)
		}
		defer exact.Close()
		if err := exact.Write(map[string]any{"a": 1}); err != nil {
			t.Fatal(err)
		}
		if err := exact.WriteJSON([]byte(`{"b":2}`)); err != nil {
			t.Fatalf("a row ending exactly at the limit: %v", err)
		}
		if err := exact.Write(map[string]any{}); !errors.Is(err, ErrByteLimit) {
			t.Fatalf("Write past the limit = %v", err)
		}
	})
	// A sample needs the first rows only, so what follows them is never decoded.
	t.Run("reads only the first rows", func(t *testing.T) {
		store, fake := newS3Store(t)
		var body bytes.Buffer
		gz := gzip.NewWriter(&body)
		_, _ = gz.Write([]byte("{\"id\":1}\n{\"id\":2}\nnot json\n"))
		_ = gz.Close()
		fake.objects["/foundry-data/k"] = body.Bytes()

		rows, err := Head(context.Background(), store, "k", 2)
		if err != nil || len(rows) != 2 || rows[1]["id"] != json.Number("2") {
			t.Fatalf("Head(2) = (%v, %v)", rows, err)
		}
		if _, err := Head(context.Background(), store, "k", 3); err == nil {
			t.Fatal("Head(3) decoded a line that is not JSON")
		}
	})
}

func TestTail(t *testing.T) {
	ctx := context.Background()
	// A snapshot's uncompressed size is in its last four bytes; a send reads
	// it without downloading the snapshot.
	t.Run("reads a snapshot's gzip trailer with one ranged read", func(t *testing.T) {
		store, fake := newS3Store(t)
		w, err := NewWriter(t.TempDir(), 10, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		_ = w.WriteJSON([]byte(`{"id":"o-1","amount":12.5}`))
		_ = w.WriteJSON([]byte(`{"id":"o-2","amount":null}`))
		if err := w.Upload(ctx, store, "ws/w/k.jsonl.gz"); err != nil {
			t.Fatal(err)
		}
		tail, err := store.Tail(ctx, "ws/w/k.jsonl.gz", 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(tail) != 4 || int64(binary.LittleEndian.Uint32(tail)) != w.bytes || !slices.Equal(fake.ranges, []string{"bytes=-4"}) {
			t.Fatalf("tail = %x for %d bytes, ranges = %v", tail, w.bytes, fake.ranges)
		}
	})
	t.Run("reads all of an object shorter than the tail", func(t *testing.T) {
		store, fake := newS3Store(t)
		fake.objects["/foundry-data/k"] = []byte("abc")
		if tail, err := store.Tail(ctx, "k", 4); err != nil || string(tail) != "abc" {
			t.Fatalf("Tail = (%q, %v)", tail, err)
		}
	})
	// A send tells a snapshot it can't read from one that is gone.
	t.Run("reports a missing object as ErrMissing", func(t *testing.T) {
		store, _ := newS3Store(t)
		if _, err := store.Tail(ctx, "none", 4); !errors.Is(err, ErrMissing) {
			t.Fatalf("Tail = %v, want ErrMissing", err)
		}
	})
	// A store that sent the whole object would hand back its first bytes, not
	// its trailer, and a snapshot can be gigabytes.
	t.Run("refuses a store that ignored the range", func(t *testing.T) {
		store, fake := newS3Store(t)
		fake.ignoreRange = true
		fake.objects["/foundry-data/k"] = []byte("abcdefgh")
		if tail, err := store.Tail(ctx, "k", 4); err == nil || errors.Is(err, ErrMissing) {
			t.Fatalf("Tail = (%q, %v), want a refusal", tail, err)
		}
	})
	t.Run("refuses an empty tail without a request", func(t *testing.T) {
		store, fake := newS3Store(t)
		fake.objects["/foundry-data/k"] = []byte("abc")
		for _, n := range []int64{0, -1} {
			if _, err := store.Tail(ctx, "k", n); err == nil {
				t.Fatalf("Tail(%d) succeeded", n)
			}
		}
		if len(fake.ranges) != 0 {
			t.Fatalf("ranges = %v", fake.ranges)
		}
	})
}

func TestDeletePrefix(t *testing.T) {
	ctx := context.Background()
	// objects holds a deleted source's batches and orphans beside another
	// source whose id shares the first's as a prefix.
	objects := func(fake *fakeS3) {
		for _, k := range []string{"a", "b", "c", "d", "e"} {
			fake.objects["/foundry-data/ws/w/events/c1/"+k+".jsonl.gz"] = []byte(k)
		}
		fake.objects["/foundry-data/ws/w/events/c10/a.jsonl.gz"] = []byte("x")
		fake.objects["/foundry-data/ws/w/events/c2/a.jsonl.gz"] = []byte("y")
	}
	remaining := func(fake *fakeS3) []string {
		keys := slices.Collect(maps.Keys(fake.objects))
		slices.Sort(keys)
		return keys
	}
	// The purge removes a deleted source's orphans, however many pages they fill.
	t.Run("deletes every object in the folder, page by page", func(t *testing.T) {
		store, fake := newS3Store(t)
		objects(fake)
		if err := store.DeletePrefix(ctx, "ws/w/events/c1/"); err != nil {
			t.Fatal(err)
		}
		want := []string{"/foundry-data/ws/w/events/c10/a.jsonl.gz", "/foundry-data/ws/w/events/c2/a.jsonl.gz"}
		if got := remaining(fake); !slices.Equal(got, want) || fake.lists != 3 || fake.deletes != 3 {
			t.Fatalf("remaining = %v, lists = %d, deletes = %d", got, fake.lists, fake.deletes)
		}
	})
	// A prefix that is not a folder would delete a neighbour's objects, and
	// an empty one the whole bucket.
	t.Run("refuses a prefix that is not a folder", func(t *testing.T) {
		for _, prefix := range []string{"", "/", "//", "ws/w/events/c1"} {
			store, fake := newS3Store(t)
			objects(fake)
			if err := store.DeletePrefix(ctx, prefix); err == nil {
				t.Fatalf("DeletePrefix(%q) succeeded", prefix)
			}
			if len(fake.objects) != 7 || fake.lists != 0 {
				t.Fatalf("DeletePrefix(%q): remaining = %v, lists = %d", prefix, remaining(fake), fake.lists)
			}
		}
	})
	// S3 answers a batch delete with 200 and lists the keys it refused; those
	// must fail the call, or the purge would forget objects it never removed.
	t.Run("fails on a key S3 refused", func(t *testing.T) {
		store, fake := newS3Store(t)
		objects(fake)
		fake.refuse["ws/w/events/c1/b.jsonl.gz"] = true
		err := store.DeletePrefix(ctx, "ws/w/events/c1/")
		if err == nil || !strings.Contains(err.Error(), "ws/w/events/c1/b.jsonl.gz: AccessDenied") {
			t.Fatalf("DeletePrefix = %v", err)
		}
		if _, ok := fake.objects["/foundry-data/ws/w/events/c1/a.jsonl.gz"]; ok || fake.objects["/foundry-data/ws/w/events/c1/b.jsonl.gz"] == nil {
			t.Fatalf("remaining = %v", remaining(fake))
		}
	})
	// A role without s3:ListBucket sees no folder at all; that must not read
	// as a folder already empty.
	t.Run("fails when the folder cannot be listed", func(t *testing.T) {
		store, fake := newS3Store(t)
		objects(fake)
		fake.denyList = true
		if err := store.DeletePrefix(ctx, "ws/w/events/c1/"); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
			t.Fatalf("DeletePrefix = %v", err)
		}
		if len(fake.objects) != 7 || fake.deletes != 0 {
			t.Fatalf("remaining = %v, deletes = %d", remaining(fake), fake.deletes)
		}
	})
	t.Run("sends no delete for an empty folder", func(t *testing.T) {
		store, fake := newS3Store(t)
		if err := store.DeletePrefix(ctx, "ws/w/events/c1/"); err != nil || fake.lists != 1 || fake.deletes != 0 {
			t.Fatalf("DeletePrefix = %v, lists = %d, deletes = %d", err, fake.lists, fake.deletes)
		}
	})
}
