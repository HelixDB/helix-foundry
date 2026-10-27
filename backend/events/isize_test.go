package events

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/helixdb/helix-foundry/backend/snapshot"
)

// objectStore keeps snapshot objects in memory, by key.
type objectStore map[string][]byte

func (s objectStore) Put(_ context.Context, key string, body io.ReadSeeker) error {
	b, err := io.ReadAll(body)
	s[key] = b
	return err
}

func (s objectStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := s[key]
	if !ok {
		return nil, snapshot.ErrMissing
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s objectStore) Delete(_ context.Context, key string) error {
	delete(s, key)
	return nil
}

func TestISIZE(t *testing.T) {
	// The byte cap counts a table by its snapshot's size, read from four
	// bytes of the object rather than the whole of it: that must be the
	// size the snapshot writer wrote.
	t.Run("reads a snapshot.Writer file's uncompressed size", func(t *testing.T) {
		w, err := snapshot.NewWriter(t.TempDir(), 1<<20, 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = w.Close() }()
		for i := range 5000 {
			if err := w.WriteJSON(fmt.Appendf(nil, `{"id":"o-%d","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":null,"n":%d}`, i, i*i)); err != nil {
				t.Fatal(err)
			}
		}
		store := objectStore{}
		if err := w.Upload(context.Background(), store, "k"); err != nil {
			t.Fatal(err)
		}
		body := store["k"]
		got, err := ISIZE(body[len(body)-4:])
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(len(gunzip(t, body))); got != want {
			t.Fatalf("ISIZE %d, want %d", got, want)
		}
	})

	// A store may hand back more than was asked for; only the last four
	// bytes are the field.
	t.Run("reads the last four bytes of a longer tail", func(t *testing.T) {
		got, err := ISIZE([]byte{9, 9, 9, 0x10, 0x27, 0, 0})
		if err != nil || got != 10000 {
			t.Fatalf("got %d, %v", got, err)
		}
	})

	// ISIZE is the size mod 2^32; snapshots stay below the 2 GiB cap, so
	// the largest value it can hold still reads as itself.
	t.Run("reads the largest size the field holds", func(t *testing.T) {
		got, err := ISIZE([]byte{0xFF, 0xFF, 0xFF, 0xFF})
		if err != nil || got != 1<<32-1 {
			t.Fatalf("got %d, %v", got, err)
		}
	})

	// An object shorter than the field is no gzip file, and its size is
	// unknown rather than 0.
	t.Run("refuses a tail shorter than the field", func(t *testing.T) {
		if _, err := ISIZE([]byte{1, 2, 3}); err == nil {
			t.Fatal("read a size from 3 bytes")
		}
	})
}
