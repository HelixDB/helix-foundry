package connector

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Format is how loads read a document (model.FoundryReadText).
type Format uint8

const (
	Markdown  Format = iota + 1 // text/markdown
	PlainText                   // text/plain
)

// MediaType is the type a document of the format is stored as; "" for none.
func (f Format) MediaType() string {
	switch f {
	case Markdown:
		return "text/markdown"
	case PlainText:
		return "text/plain"
	}
	return ""
}

// Stream is one kind of document a table's rows carry, kept per connector in a set of its own.
type Stream struct {
	Name        string // foundry_document_sets.stream, never changes: ^[a-z][a-z0-9_]{0,31}$
	Table       string // the table whose rows carry it; its key is one string column
	Title       string // the set's stored name, e.g. "Notes"
	Description string // the set's description, which the ontology agent reads
	Format      Format
	Keep        Keep // how many of its documents the set holds
}

// Keep is how many of a stream's documents its set holds. The zero value is
// the syncer's own rule: a full read keeps the first
// foundry.documents.max_per_stream it offers and removes the rest, while a
// change read may go over until the next full read, so a new document is
// never refused. KeepNewest(n) holds the set to its newest n.
type Keep struct{ newest int }

// KeepNewest is the Keep of a set that never holds more than its newest n
// documents by At, ties to the lower ExternalID by byte order, at any commit
// of any read:
//   - a publish that adds past n removes the oldest in the same transaction;
//   - a document older than a full window's oldest is never written;
//   - a held document offered again moves to its new At in place when its
//     bytes are the same, and is not extracted again;
//   - a full read offers at most n, and removes the held documents it did
//     not offer, as under the zero Keep.
//
// A read offers the documents the stream holds at the version it offers
// them at (Held) first, then the rest newest first, as under the zero Keep.
// The window knows a read only as far as its Puts have gone, and each
// publish makes its trims final, so:
//   - a held document a trim of an earlier publish of the read removed is
//     added again, as a new document, if the read then offers it; offering
//     the held versions first keeps an identical resend from being one;
//   - a held document whose row is gone keeps its place until the read
//     ends, so a read that finds rows gone may end below n until a later
//     full read offers what it trimmed.
//
// n must be at least 1, and the syncer refuses a stream whose n is over
// foundry.documents.max_per_stream.
func KeepNewest(n int) Keep {
	if n < 1 {
		panic(fmt.Sprintf("connector: KeepNewest(%d): a set keeps at least 1 document", n))
	}
	return Keep{newest: n}
}

// Newest is n of KeepNewest(n), and 0 for the zero Keep.
func (k Keep) Newest() int { return k.newest }

// streamNameRE is a stream's name, stored in an ascii column of 32.
var streamNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Problem says why the syncer can't keep the stream's set, or "" when it
// can: its name, format, title (a set's name, 1 to 120 characters) and
// description (at most 1,000).
func (s Stream) Problem() string {
	switch {
	case !streamNameRE.MatchString(s.Name):
		return "its name must match " + streamNameRE.String()
	case s.Format.MediaType() == "":
		return fmt.Sprintf("it has no format (%d)", s.Format)
	case s.Title == "" || strings.TrimSpace(s.Title) != s.Title || utf8.RuneCountInString(s.Title) > 120:
		return "its title must be 1 to 120 characters, with no space around them"
	case utf8.RuneCountInString(s.Description) > 1000:
		return "its description is over 1,000 characters"
	}
	return ""
}

// Document is the text of one row of a stream's table, as the source has it now.
type Document struct {
	ExternalID string    // the row's key value: 1-255 printable ASCII bytes, no space
	Version    string    // 1-128 printable ASCII bytes; changes whenever Text may, including the kind's rendering (prefix a render revision)
	At         time.Time // the item's time at the source (Granola: the note's created_at); orders the stream; non-zero
	Title      string    // cleaned to document.ValidName; ExternalID when empty
	URL        string    // absolute https, at most 2048 bytes; anything else is stored as none
	Text       []byte    // non-empty UTF-8; cut at foundry.documents.max_bytes at a line, with a marker
}

// DocumentWriter takes one read's documents. Like a RowWriter, it is used
// by one goroutine at a time.
type DocumentWriter interface {
	// Want says whether to fetch and Put the document. No when this read already
	// offered the id, when the stream is Paused, when the stream holds this
	// version (kept; under KeepNewest, Put decides, since a held version may
	// come with a newer At), or, in a full read, when the stream's cap is
	// reached (over_cap).
	Want(stream, externalID, version string) bool
	// Put takes a document Want said yes to, once.
	Put(ctx context.Context, stream string, doc Document) error
	// Held is the version of the document the stream holds for externalID,
	// from its live index; no I/O. ok is false when it holds none.
	Held(stream, externalID string) (version string, ok bool)
	// Paused says the stream takes no documents this sync: what work no
	// person started may spend extracting its set this month is spent
	// (syncer.Syncer.Allowances), its set holds documents, and no person
	// requested the sync. Want says no to each of its documents and
	// the sync removes none of them, so its set keeps what its graph last
	// extracted, and a kind may skip building them. It is the same all
	// sync, and false for a stream that is off; no I/O.
	Paused(stream string) bool
}

// noDocuments is the DocumentWriter of a read that keeps no documents, such
// as a PassKind's Sync, and of tests' ChangeWriters for kinds with no
// streams: it holds nothing and wants nothing, so any Put is a kind bug.
type noDocuments struct{}

func (noDocuments) Want(string, string, string) bool { return false }

func (noDocuments) Put(_ context.Context, stream string, doc Document) error {
	return &DocumentError{Stream: stream, ExternalID: doc.ExternalID, Reason: "Want did not say yes to it"}
}

func (noDocuments) Held(string, string) (string, bool) { return "", false }

func (noDocuments) Paused(string) bool { return false }

// DocumentKind is a kind some of whose tables carry documents.
type DocumentKind interface {
	Kind
	// Streams are the streams src's prepared settings turn on, in a fixed order. No network.
	Streams(src Source) []Stream
	// SyncDocuments is Sync for a table that an on stream belongs to.
	SyncDocuments(ctx context.Context, src Source, table Table, rows RowWriter, docs DocumentWriter) (Table, error)
}

// PassKind is a kind whose tables, and their documents, come from one read of
// the source. Wherever the syncer reads tables in full, it calls SyncPass once
// for all of them instead of Sync or SyncDocuments per table.
type PassKind interface {
	DocumentKind
	// SyncPass writes each table's rows to rows[table.Name] and offers the
	// documents of on streams' tables to docs. It returns the tables as written.
	SyncPass(ctx context.Context, src Source, tables []Table, rows map[string]RowWriter, docs DocumentWriter) ([]Table, error)
}

// DocumentError is a kind bug: a malformed id, version or time, empty text, a Put
// that Want did not say yes to, or a stream that is off. It fails the sync for good.
type DocumentError struct{ Stream, ExternalID, Reason string }

func (e *DocumentError) Error() string {
	if e.ExternalID == "" {
		return fmt.Sprintf("stream %s: %s", e.Stream, e.Reason)
	}
	return fmt.Sprintf("stream %s document %q: %s", e.Stream, e.ExternalID, e.Reason)
}
