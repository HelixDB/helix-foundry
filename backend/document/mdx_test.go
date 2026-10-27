package document

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBlankMDX(t *testing.T) {
	// The blanked text is what every MDX document is read as, and what its
	// extraction is paid for: each case pins one rule. Each · in want is one
	// byte blanked to a space (a blanked é is two), so the goldens show what
	// was overwritten.
	for _, tc := range []struct{ name, src, want string }{
		{"keeps prose", "# Pricing\n\nAcme pays $10.\n", "# Pricing\n\nAcme pays $10.\n"},
		{"blanks an import", "import X from './x'\n\n# Hi\n", "···················\n\n# Hi\n"},
		{"blanks an export up to the blank line", "export const meta = {\n  a: 1,\n}\n\nText\n", "·····················\n·······\n·\n\nText\n"},
		{"leaves import inside a paragraph", "We buy\nimport duties\n", "We buy\nimport duties\n"},
		{"leaves a word that only starts with import", "importance\n", "importance\n"},
		{"blanks an inline expression", "Total: {price * 2} today\n", "Total: ··········· today\n"},
		{"blanks a JSX attribute expression", "<Card href={url} />\n", "<Card href=····· />\n"},
		{"keeps JSX markup", "<Note>\nRead *this*.\n</Note>\n", "<Note>\nRead *this*.\n</Note>\n"},
		{"blanks nested braces", "a {f({b: {c: 1}})} z\n", "a ················ z\n"},
		{"blanks an expression over lines", "a {\n  x\n} z\n", "a ·\n···\n· z\n"},
		{"skips braces in strings", "a {'}' + \"{\"} z\n", "a ··········· z\n"},
		{"skips braces in template literals", "a {`}${ {b: 1}.b }{`} z\n", "a ··················· z\n"},
		{"skips braces in comments", "a {/* } */} z\n", "a ········· z\n"},
		{"skips braces in line comments", "a {x // }\n} z\n", "a ·······\n· z\n"},
		{"leaves an unbalanced brace", "a { b\n\nc\n", "a { b\n\nc\n"},
		{"leaves a brace whose string never ends", "a {'b\nc {d} e\n", "a {'b\nc ··· e\n"},
		{"leaves an escaped brace", "a \\{b} c\n", "a \\{b} c\n"},
		{"leaves fenced code", "```js\nimport x from 'y'\nconst a = {b}\n```\n{c}\n", "```js\nimport x from 'y'\nconst a = {b}\n```\n···\n"},
		{"leaves a tilde fence", "~~~\n{a}\n~~~\n", "~~~\n{a}\n~~~\n"},
		{"leaves an unclosed fence to the end", "```\n{a}\n", "```\n{a}\n"},
		{"needs a fence as long to close", "````\n```\n{a}\n````\n{b}\n", "````\n```\n{a}\n````\n···\n"},
		{"leaves inline code", "Use `{a}` and ``{`b`}`` then {c}\n", "Use `{a}` and ``{`b`}`` then ···\n"},
		{"leaves a backtick nothing closes", "a ` {b}\n\nc `\n", "a ` ···\n\nc `\n"},
		{"leaves frontmatter", "---\ntitle: {a}\n---\n{b}\n", "---\ntitle: {a}\n---\n···\n"},
		{"needs frontmatter closed", "---\n{a}\n", "---\n···\n"},
		{"keeps CRLF", "import a from 'b'\r\n\r\n{x}\r\n", "·················\r\n\r\n···\r\n"},
		{"blanks multibyte text inside", "é {'ü'} ñ\n", "é ······ ñ\n"},
		{"keeps an empty document", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := strings.ReplaceAll(tc.want, "·", " ")
			if got := string(BlankMDX([]byte(tc.src))); got != want {
				t.Errorf("BlankMDX(%q)\n got %q\nwant %q", tc.src, got, want)
			}
		})
	}
}

// Marks fall on the original's bytes, so the blanked text must be exactly as
// long as it, with every newline in place, and valid UTF-8 whenever it is:
// for any input, however malformed.
func TestBlankMDXKeepsOffsets(t *testing.T) {
	alphabet := []string{"{", "}", "`", "```", "~~~", "'", "\"", "\\", "/", "*", "$", "\n", "\r\n", " ", "a", "é", "日",
		"import ", "export ", "---", "<A b={c}>", "//", "/*", "*/", "${", "\n\n"}
	check := func(t *testing.T, src []byte) {
		t.Helper()
		got := BlankMDX(src)
		if len(got) != len(src) {
			t.Fatalf("%q: %d bytes, want %d", src, len(got), len(src))
		}
		for i := range src {
			if (src[i] == '\n' || src[i] == '\r') != (got[i] == '\n' || got[i] == '\r') || got[i] != src[i] && got[i] != ' ' {
				t.Fatalf("%q: byte %d is %q, from %q", src, i, got[i], src[i])
			}
		}
		if utf8.Valid(src) && !utf8.Valid(got) {
			t.Fatalf("%q: blanked to invalid UTF-8 %q", src, got)
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		var b strings.Builder
		for range rng.IntN(40) {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		check(t, []byte(b.String()))
	}
	// Arbitrary bytes, not only the tokens above.
	for range 2000 {
		src := make([]byte, rng.IntN(64))
		for i := range src {
			src[i] = byte(rng.UintN(256))
		}
		check(t, src)
	}
}

// A document can be a megabyte of anything: reading it stays linear, even
// built to make every brace or backtick look for its closer to the end.
func TestBlankMDXIsLinear(t *testing.T) {
	for name, src := range map[string]string{
		"unclosed braces":        strings.Repeat("{", 1<<20),
		"braces before a quote":  strings.Repeat("{", 1<<20-2) + "'\n",
		"distinct backtick runs": backtickRuns(1 << 20),
		"nested templates":       strings.Repeat("{`${", 1<<18),
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			if got := BlankMDX([]byte(src)); len(got) != len(src) {
				t.Fatalf("%d bytes, want %d", len(got), len(src))
			}
			if took := time.Since(start); took > 2*time.Second {
				t.Fatalf("took %s", took)
			}
		})
	}
}

// backtickRuns is about n bytes of backtick runs of growing length, none of
// which repeats, in one paragraph.
func backtickRuns(n int) string {
	var b bytes.Buffer
	for length := 1; b.Len() < n; length++ {
		b.WriteString(strings.Repeat("`", length))
		b.WriteString(" a ")
	}
	return b.String()
}

func ExampleBlankMDX() {
	src := "import Chart from './chart'\n\n# Revenue\n\nAcme grew <Chart data={rows} /> {2 * 3}%.\n"
	fmt.Printf("%q\n", BlankMDX([]byte(src)))
	// Output:
	// "                           \n\n# Revenue\n\nAcme grew <Chart data=       />        %.\n"
}
