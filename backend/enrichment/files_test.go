package enrichment

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// errBoom is a failure the fixtures inject: a crash, as far as the step can
// tell.
var errBoom = errors.New("boom")

// memStore is an in-memory bucket. failPut fails the next Put of a key it
// matches, once, before anything is stored.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	failPut func(key string) bool
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (s *memStore) Put(_ context.Context, key string, body io.ReadSeeker) error {
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPut != nil && s.failPut(key) {
		s.failPut = nil
		return errBoom
	}
	s.objects[key] = raw
	return nil
}

func (s *memStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("get %s: %w", key, snapshot.ErrMissing)
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (s *memStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func (s *memStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

func ExampleLoadLayout() {
	l := LoadLayout("ws_1", "ont_1", "load_1")
	fmt.Println(l.Plan())
	fmt.Println(l.Calls(7))
	fmt.Println(l.BatchResults(7))
	fmt.Println(l.Profiles())
	// Output:
	// ws/ws_1/enrichment/ont_1/loads/load_1/plan.jsonl.gz
	// ws/ws_1/enrichment/ont_1/loads/load_1/calls/000007.jsonl.gz
	// ws/ws_1/enrichment/ont_1/loads/load_1/results/000007.jsonl.gz
	// ws/ws_1/enrichment/ont_1/loads/load_1/profiles.jsonl.gz
}

func TestFiles(t *testing.T) {
	ctx := context.Background()

	// A step's files are gzipped JSON lines, read back one at a time, and
	// the digest of what was uploaded is the digest of the object. One
	// being written has no name on disk: a worker that dies writing it
	// leaves no one's profile behind.
	t.Run("writes gzipped lines and digests the object", func(t *testing.T) {
		store := newMemStore()
		files := &Files{Store: store, Dir: t.TempDir()}
		w, err := files.Create()
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(Result{Outcome: Accepted, ProfileID: "li:ada"}); err != nil {
			t.Fatal(err)
		}
		if entries, _ := os.ReadDir(files.Dir); len(entries) != 0 {
			t.Fatalf("a file being written is in %s: %v", files.Dir, entries)
		}
		w.Close()
		applied, err := Put(ctx, files, "k", []Result{{Outcome: NotFound}, {Outcome: Accepted, ProfileID: "li:ada"}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := ReadAll[Result](ctx, store, "k")
		if err != nil || len(got) != 2 || got[1].ProfileID != "li:ada" {
			t.Fatalf("read %+v, %v", got, err)
		}
		digest, err := Digest(ctx, store, "k")
		if err != nil || digest != applied {
			t.Fatalf("digest %+v, uploaded %+v (%v)", digest, applied, err)
		}
		if _, err := gzip.NewReader(bytes.NewReader(store.objects["k"])); err != nil {
			t.Fatalf("the object is not gzip: %v", err)
		}
		entries, _ := os.ReadDir(files.Dir)
		if len(entries) != 0 {
			t.Fatalf("temporary files left: %v", entries)
		}
	})

	// A reader can tell a file never written from one it can't read.
	t.Run("reports a missing file", func(t *testing.T) {
		_, err := ReadAll[Result](ctx, newMemStore(), "none")
		if !errors.Is(err, snapshot.ErrMissing) {
			t.Fatalf("err = %v", err)
		}
		store := newMemStore()
		store.objects["junk"] = []byte("not gzip")
		if _, err := ReadAll[Result](ctx, store, "junk"); err == nil || errors.Is(err, snapshot.ErrMissing) {
			t.Fatalf("junk read as %v", err)
		}
	})

	// The applied file is staged decompressed, as the engine reads it; the
	// zero-byte file is no profiles. Bytes that changed since the load chose
	// them are refused, so a retry never ingests other profiles.
	t.Run("stages the applied file and refuses changed bytes", func(t *testing.T) {
		store := newMemStore()
		files := &Files{Store: store, Dir: t.TempDir()}
		applied, err := Put(ctx, files, "profiles", []map[string]string{{"profile_id": "li:ada"}})
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "attempt", "foundry_enrichment.jsonl")
		if err := Stage(ctx, store, applied, path); err != nil {
			t.Fatal(err)
		}
		if raw, _ := os.ReadFile(path); string(raw) != `{"profile_id":"li:ada"}`+"\n" {
			t.Fatalf("staged %q", raw)
		}
		if err := Stage(ctx, store, model.FoundryEnrichmentNone, path); err != nil {
			t.Fatal(err)
		}
		if info, _ := os.Stat(path); info.Size() != 0 {
			t.Fatalf("the zero-byte file staged %d bytes", info.Size())
		}
		if _, err := Put(ctx, files, "profiles", []map[string]string{{"profile_id": "li:bob"}}); err != nil {
			t.Fatal(err)
		}
		if err := Stage(ctx, store, applied, path); err == nil {
			t.Fatal("staged bytes the load didn't choose")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("a refused file was left staged")
		}
	})

	// A published profile holds only what the item maps, its id and the
	// clues it binds by: never the returned email, websites, skills,
	// education, avatar, bio or phone, nor metadata.
	t.Run("slims a profile to what the item maps", func(t *testing.T) {
		row := ada()
		row.ProfileID, row.ClueEmail, row.ClueName, row.CurrentTitle, row.LocationCountry = "li:ada-lovelace", "ada@acme.io", "Ada Lovelace", "VP", "UK"
		row.Score, row.CallID, row.LoadID = 96, "call", "load"
		item := &ontologyagent.Enrichment{Name: "people", Props: map[string]string{"title": "current_title", "country": "location_country"}}
		slim, err := Slim(row, item)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for k := range slim {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if want := []string{"clue_email", "clue_name", "current_title", "location_country", "profile_id"}; !slices.Equal(keys, want) {
			t.Fatalf("slim = %v, want %v", keys, want)
		}
		item.WorkHistory = &ontologyagent.EnrichmentWorkHistory{Link: "worked_at"}
		if slim, _ := Slim(row, item); slim["experience"] == nil {
			t.Fatal("work history dropped the experience")
		}
		item.Props["score"] = "score"
		if _, err := Slim(row, item); err == nil {
			t.Fatal("slimmed to a metadata field")
		}
	})
}
