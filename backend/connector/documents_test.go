package connector

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

// documentKinds are, for each kind that carries documents, settings that
// turn every one of its streams on, and the key of each of its objects as
// Discover gives it. Every DocumentKind must be listed.
var documentKinds = map[string]struct {
	settings json.RawMessage
	keys     map[string][]string
}{
	"granola": {settings: json.RawMessage(`{"transcripts":true}`), keys: map[string][]string{
		"notes": {"note_id"}, "note_attendees": {"note_id", "email"},
		"note_folders": {"note_id", "folder_id"}, "folders": {"folder_id"}, "spaces": {"space_id"}}},
	"events": {settings: json.RawMessage(`{"description":"Replies to customers","text_field":"reply"}`), keys: map[string][]string{
		"events": {"id"}}},
	"slack": {settings: json.RawMessage(`{"history_days":90}`), keys: map[string][]string{
		"users": {"user_id"}, "channels": {"channel_id"}, "channel_members": {"channel_id", "user_id"},
		"messages": {"message_id"}, "channel_days": {"channel_day_id"}}},
}

// streamProblems lists what keeps streams from being synced: a stream the
// syncer can't keep (Stream.Problem), a name used twice, or a table that is
// not one of the kind's objects or isn't keyed by one column.
func streamProblems(streams []Stream, keys map[string][]string) []string {
	var problems []string
	for i, s := range streams {
		if p := s.Problem(); p != "" {
			problems = append(problems, fmt.Sprintf("%s: %s", s.Name, p))
		}
		if slices.ContainsFunc(streams[:i], func(o Stream) bool { return o.Name == s.Name }) {
			problems = append(problems, s.Name+": named twice")
		}
		key, ok := keys[s.Table]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: table %q is not one of the kind's objects", s.Name, s.Table))
		case len(key) != 1:
			problems = append(problems, fmt.Sprintf("%s: table %s is keyed by %v, not one column", s.Name, s.Table, key))
		}
	}
	return problems
}

// TestStreamCatalog checks every stream any settings can turn on, without
// the network: the syncer refuses at run time what this catches here. The
// events kind keeps its sources' newest pieces as the sync worker sets it.
func TestStreamCatalog(t *testing.T) {
	for _, k := range NewCatalog(DefaultEndpoints(), Guard{}, S3Options{}, EventsOptions{Extracted: 500}).Kinds() {
		dk, ok := k.(DocumentKind)
		if !ok {
			continue
		}
		kind, listed := documentKinds[k.Name()]
		if !listed {
			t.Errorf("%s carries documents: add settings that turn every stream on to documentKinds", k.Name())
			continue
		}
		streams := dk.Streams(Source{Settings: kind.settings})
		if len(streams) == 0 {
			t.Errorf("%s: the settings turn no stream on", k.Name())
		}
		for _, p := range streamProblems(streams, kind.keys) {
			t.Errorf("%s: %s", k.Name(), p)
		}
	}
	t.Run("finds each problem", func(t *testing.T) {
		keys := map[string][]string{"notes": {"note_id"}, "attendees": {"note_id", "email"}}
		ok := Stream{Name: "notes", Table: "notes", Title: "Notes", Format: Markdown}
		if p := streamProblems([]Stream{ok, {Name: "transcripts", Table: "notes", Title: "Transcripts", Format: PlainText}}, keys); p != nil {
			t.Fatalf("problems = %q", p)
		}
		bad := []Stream{
			ok, ok,
			{Name: "Notes", Table: "notes", Title: "Notes", Format: Markdown},
			{Name: "raw", Table: "notes", Title: "Raw"},
			{Name: "untitled", Table: "notes", Title: " ", Format: Markdown},
			{Name: "people", Table: "attendees", Title: "People", Format: Markdown},
			{Name: "files", Table: "files", Title: "Files", Format: Markdown},
		}
		want := []string{
			"notes: named twice",
			"Notes: its name must match ^[a-z][a-z0-9_]{0,31}$",
			"raw: it has no format (0)",
			"untitled: its title must be 1 to 120 characters, with no space around them",
			"people: table attendees is keyed by [note_id email], not one column",
			`files: table "files" is not one of the kind's objects`,
		}
		if got := streamProblems(bad, keys); !slices.Equal(got, want) {
			t.Fatalf("problems = %q", got)
		}
	})
}

func TestKeep(t *testing.T) {
	if (Keep{}).Newest() != 0 || KeepNewest(1).Newest() != 1 || KeepNewest(1000).Newest() != 1000 {
		t.Fatal("Newest is not the n kept")
	}
	for _, n := range []int{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("KeepNewest(%d) did not panic", n)
				}
			}()
			KeepNewest(n)
		}()
	}
	if (noDocuments{}).Paused("notes") {
		t.Fatal("a read that keeps no documents pauses a stream")
	}
}

func ExampleKeepNewest() {
	pieces := Stream{Name: "pieces", Table: "events", Title: "Pieces", Format: Markdown, Keep: KeepNewest(500)}
	notes := Stream{Name: "notes", Table: "notes", Title: "Notes", Format: Markdown}
	fmt.Println(pieces.Keep.Newest(), notes.Keep.Newest())
	// Output: 500 0
}
