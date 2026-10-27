package document

import "bytes"

// BlankMDX is an MDX document as ontologies read it: Markdown, with its code
// overwritten by spaces. Ontologies never compile or run MDX; they read the
// prose. Blanked are:
//
//   - ESM blocks: a line outside code and outside a paragraph that starts
//     with import or export, and every line after it up to the next blank
//     line;
//   - expressions: from a { outside code to the } that closes it, braces
//     inside JavaScript strings, template literals and comments aside. A {
//     that nothing closes is text, as is every { up to where looking for
//     its } gave up, and a \{.
//
// Left as they are: fenced code blocks (``` or ~~~), inline code spans, a
// leading YAML frontmatter block (--- to ---), and JSX markup, whose
// expressions are blanked like any other.
//
// Every blanked byte but CR and LF becomes a space, so the result is
// exactly as long as src, its lines start at the same offsets, and offsets
// into it are offsets into src: a mark on the text Helix sent falls on the
// same bytes of the original. It is valid UTF-8 when src is, since every
// blanked run starts and ends on an ASCII byte. It runs in time linear in
// src, whatever src holds.
//
// Changing what is blanked changes the text every MDX document is read as,
// and so reads, and pays for, each of them again; the golden tests pin it.
func BlankMDX(src []byte) []byte {
	b := mdxBlanker{src: src, out: bytes.Clone(src), closers: codeCloses(src)}
	b.run()
	return b.out
}

type mdxBlanker struct {
	src, out []byte
	// closers maps the start of each backtick run to the start of the next
	// run as long, if any.
	closers map[int]int
	// paragraph is the extent of the paragraph last measured, which code
	// spans may not leave.
	paragraph struct{ start, end int }
	// textUntil is where the last expression that nothing closed gave up:
	// every { before it is text, so no byte is read twice looking for one.
	textUntil int
}

func (b *mdxBlanker) run() {
	pos := b.frontmatter()
	inParagraph := false
	for pos < len(b.src) {
		end := lineEnd(b.src, pos)
		line := b.src[pos:end]
		switch fence := fenceOpen(line); {
		case fence != nil:
			pos, inParagraph = b.skipFence(pos, fence), false
		case len(bytes.TrimSpace(line)) == 0:
			pos, inParagraph = next(b.src, end), false
		case !inParagraph && esmStart(line):
			pos = b.blankESM(pos)
		default:
			pos, inParagraph = b.line(pos), true
		}
	}
}

// frontmatter skips a YAML frontmatter block at the very start: a --- line,
// through the next --- line. Without a closing line it is not frontmatter.
func (b *mdxBlanker) frontmatter() int {
	first := lineEnd(b.src, 0)
	if !dashes(b.src[:first]) {
		return 0
	}
	for pos := next(b.src, first); pos < len(b.src); {
		end := lineEnd(b.src, pos)
		if dashes(b.src[pos:end]) {
			return next(b.src, end)
		}
		pos = next(b.src, end)
	}
	return 0
}

func dashes(line []byte) bool { return string(bytes.TrimRight(line, " \t\r")) == "---" }

// skipFence skips a fenced code block that opens on the line at pos, through
// its closing fence, or to the end of the document if none closes it.
func (b *mdxBlanker) skipFence(pos int, fence []byte) int {
	pos = next(b.src, lineEnd(b.src, pos))
	for pos < len(b.src) {
		end := lineEnd(b.src, pos)
		if fenceCloses(b.src[pos:end], fence) {
			return next(b.src, end)
		}
		pos = next(b.src, end)
	}
	return pos
}

// blankESM blanks the ESM block that starts on the line at pos, up to the
// next blank line, which it leaves.
func (b *mdxBlanker) blankESM(pos int) int {
	for pos < len(b.src) {
		end := lineEnd(b.src, pos)
		if len(bytes.TrimSpace(b.src[pos:end])) == 0 {
			return pos
		}
		b.blank(pos, end)
		pos = next(b.src, end)
	}
	return pos
}

// line reads the Markdown line at pos, blanking its expressions and skipping
// its code spans, either of which may run on over later lines, and returns
// the start of the line after the one it ended on.
func (b *mdxBlanker) line(pos int) int {
	for pos < len(b.src) {
		switch c := b.src[pos]; {
		case c == '\n':
			return pos + 1
		case c == '\\' && pos+1 < len(b.src) && b.src[pos+1] != '\n':
			pos += 2
		case c == '`':
			pos = b.codeSpan(pos)
		case c == '{' && pos >= b.textUntil:
			end, closed := expressionEnd(b.src, pos)
			if !closed {
				b.textUntil = end
				pos++
				continue
			}
			b.blank(pos, end)
			pos = end
		default:
			pos++
		}
	}
	return pos
}

// codeSpan skips the code span whose backticks start at pos, which the next
// run of as many backticks in its paragraph closes. A run nothing closes is
// text.
func (b *mdxBlanker) codeSpan(pos int) int {
	n := run(b.src, pos, '`')
	closer, ok := b.closers[pos]
	if !ok || closer >= b.paragraphEnd(pos) {
		return pos + n
	}
	return closer + n
}

// paragraphEnd is where the paragraph holding pos ends: at the next blank
// line or fence, or the end of the document. Positions only move forward,
// so the paragraph last measured answers until pos leaves it.
func (b *mdxBlanker) paragraphEnd(pos int) int {
	if pos >= b.paragraph.start && pos < b.paragraph.end {
		return b.paragraph.end
	}
	end := lineEnd(b.src, pos)
	for end < len(b.src) {
		start := end + 1
		lineStop := lineEnd(b.src, start)
		line := b.src[start:lineStop]
		if len(bytes.TrimSpace(line)) == 0 || fenceOpen(line) != nil {
			break
		}
		end = lineStop
	}
	b.paragraph.start, b.paragraph.end = pos, end
	return end
}

// codeCloses maps the start of each run of backticks in src to the start of
// the next run of the same length.
func codeCloses(src []byte) map[int]int {
	type found struct{ start, length int }
	var runs []found
	for i := 0; i < len(src); {
		if src[i] != '`' {
			i++
			continue
		}
		n := run(src, i, '`')
		runs = append(runs, found{i, n})
		i += n
	}
	closers := make(map[int]int, len(runs))
	nextOfLength := map[int]int{}
	for i := len(runs) - 1; i >= 0; i-- {
		if closer, ok := nextOfLength[runs[i].length]; ok {
			closers[runs[i].start] = closer
		}
		nextOfLength[runs[i].length] = runs[i].start
	}
	return closers
}

// blank overwrites src[from:to] with spaces in out, keeping CR and LF.
func (b *mdxBlanker) blank(from, to int) {
	for i := from; i < to; i++ {
		if b.src[i] != '\n' && b.src[i] != '\r' {
			b.out[i] = ' '
		}
	}
}

// expressionEnd is the offset just past the } that closes the expression
// whose { is at open, reading JavaScript: strings, template literals (with
// their ${} expressions) and comments hide the braces in them. When nothing
// closes it, closed is false and end is where reading gave up: the end of a
// line that ended a string, or of the document.
func expressionEnd(src []byte, open int) (end int, closed bool) {
	// templates holds, for each open brace, whether it is a template
	// literal's ${, whose } goes back into the literal.
	templates := []bool{false}
	for i := open + 1; i < len(src); i++ {
		switch c := src[i]; c {
		case '{':
			templates = append(templates, false)
		case '}':
			inTemplate := templates[len(templates)-1]
			templates = templates[:len(templates)-1]
			if len(templates) == 0 {
				return i + 1, true
			}
			if inTemplate {
				stop, ok := templateEnd(src, i+1)
				if !ok {
					return stop, false
				}
				if src[stop] == '{' {
					templates = append(templates, true)
				}
				i = stop
			}
		case '"', '\'':
			stop, ok := stringEnd(src, i+1, c)
			if !ok {
				return stop, false
			}
			i = stop
		case '`':
			stop, ok := templateEnd(src, i+1)
			if !ok {
				return stop, false
			}
			if src[stop] == '{' {
				templates = append(templates, true)
			}
			i = stop
		case '/':
			switch {
			case i+1 < len(src) && src[i+1] == '/':
				i = lineEnd(src, i) - 1
			case i+1 < len(src) && src[i+1] == '*':
				close := bytes.Index(src[i+2:], []byte("*/"))
				if close < 0 {
					return len(src), false
				}
				i += 2 + close + 1
			}
		}
	}
	return len(src), false
}

// stringEnd is the offset of the quote that closes a string opened by
// quote, read from i. A string ends with its line in JavaScript: a newline
// first means it isn't one, and reading gives up there.
func stringEnd(src []byte, i int, quote byte) (int, bool) {
	for ; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case '\n':
			return i, false
		case quote:
			return i, true
		}
	}
	return len(src), false
}

// templateEnd reads a template literal from i, just after its backtick or
// the } of one of its ${}: the offset of its closing backtick, or of the {
// of its next ${.
func templateEnd(src []byte, i int) (int, bool) {
	for ; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case '`':
			return i, true
		case '$':
			if i+1 < len(src) && src[i+1] == '{' {
				return i + 1, true
			}
		}
	}
	return len(src), false
}

// esmStart reports a line that starts an ESM block: import or export at the
// start of the line, followed by a space, a tab, { or *.
func esmStart(line []byte) bool {
	for _, word := range []string{"import", "export"} {
		if rest, ok := bytes.CutPrefix(line, []byte(word)); ok && len(rest) > 0 && bytes.IndexByte([]byte(" \t{*"), rest[0]) >= 0 {
			return true
		}
	}
	return false
}

// fenceOpen is the fence a line opens, ``` or ~~~ (or longer) after at most
// three spaces, or nil. A backtick fence's info string has no backtick.
func fenceOpen(line []byte) []byte {
	trimmed := bytes.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 || trimmed[0] != '`' && trimmed[0] != '~' {
		return nil
	}
	n := run(trimmed, 0, trimmed[0])
	if n < 3 || trimmed[0] == '`' && bytes.IndexByte(trimmed[n:], '`') >= 0 {
		return nil
	}
	return trimmed[:n]
}

// fenceCloses reports a line that closes fence: as many of its character or
// more, after at most three spaces, and nothing but spaces after.
func fenceCloses(line, fence []byte) bool {
	trimmed := bytes.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return false
	}
	n := run(trimmed, 0, fence[0])
	return n >= len(fence) && len(bytes.TrimRight(trimmed[n:], " \t\r")) == 0
}

// run counts the bytes equal to c from i.
func run(src []byte, i int, c byte) int {
	n := 0
	for i+n < len(src) && src[i+n] == c {
		n++
	}
	return n
}

// lineEnd is the offset of the newline ending the line at pos, or len(src).
func lineEnd(src []byte, pos int) int {
	if i := bytes.IndexByte(src[pos:], '\n'); i >= 0 {
		return pos + i
	}
	return len(src)
}

// next is the start of the line after the one ending at end.
func next(src []byte, end int) int {
	return min(end+1, len(src))
}
