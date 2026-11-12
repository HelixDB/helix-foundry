// Package transcript reads transcripts of meetings and calls, as the
// recordings of Teams, Zoom, Google Meet and most notetakers export them:
// WebVTT (.vtt) files of timed cues, each naming its speaker in a voice tag
// (<v Ada Lovelace>) when the tool knows them. Wherever a transcript comes
// from, an upload or a connector's document (connector.WebVTT), ontologies
// read it as Text gives it: one line per turn, its start and speaker, then
// what was said.
//
// The website's port (src/shared/transcript.ts) and the mock read the same
// goldens (testdata), so a change here fails their tests until they follow.
package transcript

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Transcript is what a WebVTT file holds that ontologies read.
type Transcript struct {
	// Title is the text of the file's WEBVTT line, as in "WEBVTT - Weekly
	// sync", without the dash; "" when it has none.
	Title string
	// Notes are the lines of the NOTE blocks before the first cue, where a
	// file says what it is a transcript of (WithNote). Notes after the first
	// cue are comments, and left out.
	Notes []string
	// Turns are the cues with text, in the file's order, each run of cues
	// one speaker said in a row merged into one turn.
	Turns []Turn
}

// Turn is one speaker's words, from when they started saying them.
type Turn struct {
	// Speaker is the cue's voice (<v Ada Lovelace>); "" when it names none,
	// and then the turn is that one cue.
	Speaker string
	Start   time.Duration
	Text    string
}

// Text is a WebVTT file as ontologies read it (Render), or src as it is
// when it isn't one or holds no cue with text: a file named .vtt that holds
// something else is read as the text it is.
//
// Changing what it gives changes the text every WebVTT document is read
// as, and so reads, and pays for, each of them again; the goldens pin it.
func Text(src []byte) []byte {
	t, ok := Parse(src)
	if !ok || len(t.Turns) == 0 {
		return src
	}
	return t.Render()
}

// Render writes the transcript as ontologies read it: its title and notes,
// a line each, then a blank line and a line per turn, its start (hours,
// minutes and seconds) in brackets, its speaker, and its words:
//
//	Weekly sync
//	Recorded 2026-11-10
//
//	[00:00:03] Ada Lovelace: Let's start with the Globex renewal.
//	[00:00:09] Grace Hopper: They asked for three years.
//	[00:00:14] A turn whose cue names no speaker.
func (t Transcript) Render() []byte {
	var b strings.Builder
	header := t.Notes
	if t.Title != "" {
		header = append([]string{t.Title}, t.Notes...)
	}
	for _, line := range header {
		b.WriteString(line + "\n")
	}
	if len(header) > 0 {
		b.WriteString("\n")
	}
	for _, turn := range t.Turns {
		s := int64(turn.Start / time.Second)
		fmt.Fprintf(&b, "[%02d:%02d:%02d] ", s/3600, s/60%60, s%60)
		if turn.Speaker != "" {
			b.WriteString(turn.Speaker + ": ")
		}
		b.WriteString(turn.Text + "\n")
	}
	return []byte(b.String())
}

// Parse reads a WebVTT file, as browsers do but forgiving what tools get
// wrong: false when it doesn't start with WEBVTT, as every WebVTT file
// must. Within it:
//
//   - a cue is a block with a timing line ("00:01.000 --> 00:04.000"),
//     first or after an identifier line; its start may have hours or not,
//     and a comma for the dot; a cue whose start can't be read is left out;
//   - a cue's speaker is its first voice tag's name (<v Ada>, <v.loud
//     Ada>); its tags are taken out, its character references decoded
//     (&amp;, &lt;, &gt;, &quot;, &apos;, &nbsp;, numeric ones; &lrm; and
//     &rlm; are dropped), and its lines joined, every run of white space
//     one space;
//   - NOTE blocks before the first cue are its notes; STYLE and REGION
//     blocks, and any other block without a timing line, are left out.
//
// A line holding only white space ends a block, as an empty one does.
func Parse(src []byte) (Transcript, bool) {
	lines := strings.Split(normalize(src), "\n")
	first, ok := signature(lines[0])
	if !ok {
		return Transcript{}, false
	}
	t := Transcript{Title: collapse(strings.TrimPrefix(collapse(first), "-"))}
	var blocks [][]string
	var block []string
	for _, line := range lines[1:] {
		if collapse(line) == "" {
			if len(block) > 0 {
				blocks = append(blocks, block)
			}
			block = nil
			continue
		}
		block = append(block, line)
	}
	if len(block) > 0 {
		blocks = append(blocks, block)
	}
	// The header is the lines after WEBVTT up to the first blank line, unless
	// a timing line comes first: the cue it starts is the header's last
	// lines, from its identifier, the line before it, when there is one.
	if len(lines) > 1 && collapse(lines[1]) != "" && len(blocks) > 0 {
		header := blocks[0]
		blocks = blocks[1:]
		for i, line := range header {
			if strings.Contains(line, "-->") {
				blocks = append([][]string{header[max(i-1, 0):]}, blocks...)
				break
			}
		}
	}
	var last *Turn
	cued := false
	for _, block := range blocks {
		switch {
		case keyword(block[0], "NOTE"):
			if !cued {
				for i, line := range block {
					if i == 0 {
						line = strings.TrimPrefix(line, "NOTE")
					}
					if line = collapse(line); line != "" {
						t.Notes = append(t.Notes, line)
					}
				}
			}
			continue
		case keyword(block[0], "STYLE"), keyword(block[0], "REGION"):
			continue
		}
		timing := 0
		if !strings.Contains(block[0], "-->") {
			if len(block) < 2 || !strings.Contains(block[1], "-->") {
				continue
			}
			timing = 1
		}
		cued = true
		start, ok := timestamp(collapse(strings.SplitN(block[timing], "-->", 2)[0]))
		if !ok {
			continue
		}
		speaker, words := cueText(strings.Join(block[timing+1:], "\n"))
		if words == "" {
			continue
		}
		if last != nil && speaker != "" && last.Speaker == speaker {
			last.Text += " " + words
			continue
		}
		t.Turns = append(t.Turns, Turn{Speaker: speaker, Start: start, Text: words})
		last = &t.Turns[len(t.Turns)-1]
	}
	return t, true
}

// WithNote is a WebVTT file with a NOTE block of lines after its header,
// where Parse reads them as notes: what the file is a transcript of, for a
// connector to say where it found it. Each line is one note, its white
// space collapsed and its arrows ("-->", which a note can't hold) made
// "->"; empty ones are left out. False, with src as it is, for a file that
// isn't WebVTT. Line ends are made LF.
func WithNote(src []byte, lines ...string) ([]byte, bool) {
	text := normalize(src)
	all := strings.Split(text, "\n")
	if _, ok := signature(all[0]); !ok {
		return src, false
	}
	var note []string
	for _, line := range lines {
		if line = collapse(strings.ReplaceAll(line, "-->", "->")); line != "" {
			note = append(note, line)
		}
	}
	if len(note) == 0 {
		return []byte(text), true
	}
	block := "NOTE\n" + strings.Join(note, "\n") + "\n"
	// After the header: its first line and those after it up to a blank
	// line, unless one of them is a cue's, whose block it then goes before.
	end := 1
	for end < len(all) && collapse(all[end]) != "" {
		if strings.Contains(all[end], "-->") {
			end = max(end-1, 1)
			break
		}
		end++
	}
	head, rest := strings.Join(all[:end], "\n"), strings.Join(all[end:], "\n")
	rest = strings.TrimLeft(rest, "\n")
	if rest == "" {
		return []byte(head + "\n\n" + block), true
	}
	return []byte(head + "\n\n" + block + "\n" + rest), true
}

// normalize is a file's text: valid UTF-8, without a byte order mark, its
// lines ended by LF.
func normalize(src []byte) string {
	text := strings.TrimPrefix(strings.ToValidUTF8(string(src), "\uFFFD"), "\uFEFF")
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// signature is the text after WEBVTT on a file's first line, which must be
// WEBVTT, alone or followed by white space; false for any other line.
func signature(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "WEBVTT")
	return rest, ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

// keyword reports a line that is word, alone or followed by white space.
func keyword(line, word string) bool {
	rest, ok := strings.CutPrefix(line, word)
	return ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

// cueText is a cue's speaker, from its first voice tag, and its words: its
// tags taken out (a word a tag splits stays whole: <i>Glo</i>bex), its
// references decoded, and its white space, the line breaks among it too,
// collapsed. A < that nothing closes is text.
func cueText(payload string) (speaker, words string) {
	var b strings.Builder
	for i := 0; i < len(payload); {
		if payload[i] != '<' {
			j := strings.IndexByte(payload[i:], '<')
			if j < 0 {
				j = len(payload) - i
			}
			b.WriteString(decode(payload[i : i+j]))
			i += j
			continue
		}
		end := strings.IndexByte(payload[i:], '>')
		if end < 0 {
			b.WriteString(decode(payload[i:]))
			break
		}
		if tag := payload[i+1 : i+end]; speaker == "" && (tag == "v" || strings.HasPrefix(tag, "v.") || keyword(tag, "v")) {
			if k := strings.IndexAny(tag, " \t\n"); k >= 0 {
				speaker = collapse(decode(tag[k:]))
			}
		}
		i += end + 1
	}
	return speaker, collapse(b.String())
}

// space is the white space collapse folds: ASCII's, and the no-break space
// &nbsp; is.
const space = " \t\n\v\f\r\u00a0"

// collapse trims s of white space and makes each run of it one space.
func collapse(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(space, r) }), " ")
}

// references are the named character references WebVTT tools write.
var references = map[string]string{"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "apos": "'", "nbsp": "\u00a0", "lrm": "", "rlm": ""}

// decode decodes s's character references: the named ones of references,
// each closed by ;, and numeric ones (&#38; or &#x26;) of a character other
// than NUL or a surrogate. Any other & is text.
func decode(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '&')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		s = s[i:]
		end := strings.IndexByte(s, ';')
		if end < 0 || end > 10 {
			b.WriteByte('&')
			s = s[1:]
			continue
		}
		name := s[1:end]
		if r, ok := references[name]; ok {
			b.WriteString(r)
			s = s[end+1:]
			continue
		}
		if r, ok := numeric(name); ok {
			b.WriteRune(r)
			s = s[end+1:]
			continue
		}
		b.WriteByte('&')
		s = s[1:]
	}
}

// numeric is the character of a numeric reference's name: #38 or #x26.
func numeric(name string) (rune, bool) {
	digits, base := strings.TrimPrefix(name, "#"), 10
	if digits == name || digits == "" {
		return 0, false
	}
	if hex, ok := strings.CutPrefix(strings.ToLower(digits), "x"); ok {
		digits, base = hex, 16
	}
	if digits == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, base, 32)
	if err != nil || n == 0 || n > utf8.MaxRune || n >= 0xd800 && n <= 0xdfff {
		return 0, false
	}
	return rune(n), true
}

// timestamp reads a cue's start: [hours:]minutes:seconds[.fraction], with
// minutes and seconds of one or two digits under 60 and a dot or a comma
// before the fraction, of which milliseconds count.
func timestamp(s string) (time.Duration, bool) {
	whole, fraction, _ := strings.Cut(strings.ReplaceAll(s, ",", "."), ".")
	parts := strings.Split(whole, ":")
	if len(parts) < 2 || len(parts) > 3 || strings.Contains(fraction, ".") {
		return 0, false
	}
	var units []int64
	for i, part := range parts {
		if part == "" || !digitsOnly(part) || i > 0 && len(part) > 2 || len(part) > 9 {
			return 0, false
		}
		n, _ := strconv.ParseInt(part, 10, 64)
		if i > 0 && n >= 60 {
			return 0, false
		}
		units = append(units, n)
	}
	if len(units) == 2 {
		units = append([]int64{0}, units...)
	}
	ms := int64(0)
	if fraction != "" {
		if !digitsOnly(fraction) {
			return 0, false
		}
		fraction = (fraction + "000")[:3]
		ms, _ = strconv.ParseInt(fraction, 10, 64)
	}
	return time.Duration(units[0])*time.Hour + time.Duration(units[1])*time.Minute +
		time.Duration(units[2])*time.Second + time.Duration(ms)*time.Millisecond, true
}

func digitsOnly(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}
