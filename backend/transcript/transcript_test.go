package transcript

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/*.txt from Text")

// TestGoldens pins what ontologies read of each testdata/*.vtt: its .txt.
// The website's port reads the same files (tests/unit/transcript.test.ts).
func TestGoldens(t *testing.T) {
	files, err := filepath.Glob("testdata/*.vtt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no goldens: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			got := Text(src)
			golden := strings.TrimSuffix(file, ".vtt") + ".txt"
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("Text(%s):\n%s\nwant:\n%s", file, got, want)
			}
		})
	}
}

func TestParse(t *testing.T) {
	src, err := os.ReadFile("testdata/header.vtt")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := Parse(src)
	if !ok {
		t.Fatal("header.vtt is WebVTT")
	}
	if got.Title != "Weekly sync" {
		t.Errorf("Title = %q", got.Title)
	}
	want := []string{"Recorded 2026-11-10", "Attendees: Ada Lovelace, Grace Hopper", "Organizer: ada@example.com"}
	if strings.Join(got.Notes, "|") != strings.Join(want, "|") {
		t.Errorf("Notes = %q, want %q", got.Notes, want)
	}
	if len(got.Turns) != 4 {
		t.Fatalf("Turns = %+v", got.Turns)
	}
	first := got.Turns[0]
	if first.Speaker != "Ada Lovelace" || first.Start != time.Second || !strings.HasPrefix(first.Text, "Can everyone hear me? Globex's renewal") {
		t.Errorf("Turns[0] = %+v", first)
	}
	if got.Turns[1].Speaker != "Grace Hopper" || got.Turns[1].Start != 7*time.Second {
		t.Errorf("Turns[1] = %+v", got.Turns[1])
	}
}

func TestNotWebVTT(t *testing.T) {
	for _, src := range []string{"", "webvtt\n", "WEBVTTX\n", "1\n00:00:01,000 --> 00:00:02,000\nHi\n", " WEBVTT\n"} {
		if _, ok := Parse([]byte(src)); ok {
			t.Errorf("Parse(%q) is WebVTT", src)
		}
		if got := Text([]byte(src)); string(got) != src {
			t.Errorf("Text(%q) = %q, want it as it is", src, got)
		}
	}
	for _, src := range []string{"WEBVTT", "WEBVTT\n", "WEBVTT\tTitle\n", "\ufeffWEBVTT\r\n"} {
		if _, ok := Parse([]byte(src)); !ok {
			t.Errorf("Parse(%q) isn't WebVTT", src)
		}
	}
}

func TestTimestamp(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"00:00:03.120":  3120 * time.Millisecond,
		"01:02:03.000":  time.Hour + 2*time.Minute + 3*time.Second,
		"00:04,500":     4500 * time.Millisecond,
		"1:02:03":       time.Hour + 2*time.Minute + 3*time.Second,
		"02:03.5":       2*time.Minute + 3500*time.Millisecond,
		"100:00:00.000": 100 * time.Hour,
		"00:00:01.2345": 1234 * time.Millisecond,
	} {
		if got, ok := timestamp(in); !ok || got != want {
			t.Errorf("timestamp(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "3", "00:60.000", "00:00:61.000", "00:000:01", "a:00:01", "00:00:01.x", "00:00:01.0.0", "-1:00:00", "1:2:3:4"} {
		if got, ok := timestamp(in); ok {
			t.Errorf("timestamp(%q) = %v, want none", in, got)
		}
	}
}

func TestDecode(t *testing.T) {
	for in, want := range map[string]string{
		"a &amp; b":                "a & b",
		"&lt;&gt;&quot;&apos;":     `<>"'`,
		"&#38;&#x26;&#X26;":        "&&&",
		"x&lrm;y&rlm;z":            "xyz",
		"&copy; &amp no;":          "&copy; &amp no;",
		"&#0; &#xD800; &#x110000;": "&#0; &#xD800; &#x110000;",
		"&# &#x; &;":               "&# &#x; &;",
		"trailing &":               "trailing &",
		"&averyveryverylongname;":  "&averyveryverylongname;",
	} {
		if got := decode(in); got != want {
			t.Errorf("decode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRender(t *testing.T) {
	got := Transcript{Turns: []Turn{
		{Speaker: "Ada", Start: 3*time.Hour + 4*time.Minute + 5*time.Second + 900*time.Millisecond, Text: "Hello."},
		{Start: 0, Text: "No speaker."},
	}}.Render()
	if want := "[03:04:05] Ada: Hello.\n[00:00:00] No speaker.\n"; string(got) != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
	got = Transcript{Title: "Sync", Turns: []Turn{{Speaker: "Ada", Text: "Hi."}}}.Render()
	if want := "Sync\n\n[00:00:00] Ada: Hi.\n"; string(got) != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

func TestWithNote(t *testing.T) {
	cue := "00:00:01.000 --> 00:00:02.000\n<v Ada>Hi.\n"
	for _, c := range []struct{ name, src, want string }{
		{"after the WEBVTT line", "WEBVTT\n\n" + cue,
			"WEBVTT\n\nNOTE\nWeekly sync.vtt\nOwner: ada@example.com\n\n" + cue},
		{"after the header's lines", "\ufeffWEBVTT - Sync\r\nKind: captions\r\n\r\n" + strings.ReplaceAll(cue, "\n", "\r\n"),
			"WEBVTT - Sync\nKind: captions\n\nNOTE\nWeekly sync.vtt\nOwner: ada@example.com\n\n" + cue},
		{"before a cue right under the header", "WEBVTT\nid\n" + cue,
			"WEBVTT\n\nNOTE\nWeekly sync.vtt\nOwner: ada@example.com\n\nid\n" + cue},
		{"before a timing line right under the header", "WEBVTT\n" + cue,
			"WEBVTT\n\nNOTE\nWeekly sync.vtt\nOwner: ada@example.com\n\n" + cue},
		{"at the end of an empty file", "WEBVTT",
			"WEBVTT\n\nNOTE\nWeekly sync.vtt\nOwner: ada@example.com\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := WithNote([]byte(c.src), "Weekly sync.vtt", "", "  Owner:  ada@example.com ")
			if !ok || string(got) != c.want {
				t.Errorf("WithNote = %q, %v\nwant %q", got, ok, c.want)
			}
			parsed, _ := Parse(got)
			if strings.Join(parsed.Notes, "|") != "Weekly sync.vtt|Owner: ada@example.com" {
				t.Errorf("Notes = %q", parsed.Notes)
			}
		})
	}
	if got, _ := WithNote([]byte("WEBVTT\n\n"+cue), "a --> b"); !strings.Contains(string(got), "NOTE\na -> b\n") {
		t.Errorf("an arrow is kept in a note: %q", got)
	}
	if got, _ := WithNote([]byte("WEBVTT\n\n" + cue)); string(got) != "WEBVTT\n\n"+cue {
		t.Errorf("WithNote of no lines = %q", got)
	}
	if got, ok := WithNote([]byte("not vtt"), "x"); ok || string(got) != "not vtt" {
		t.Errorf("WithNote of a file that isn't WebVTT = %q, %v", got, ok)
	}
}
