// Package namewords finds the aliases a document can use, without the
// engine. From contract 4.23.0 an alias acts only on a document whose text
// writes its name: the name's words, in order, with nothing but what is no
// word between them, as the engine folds them (NFKC, lowercase, Turkish İ as
// i, final ς as σ; a word is a run of letters, digits and marks of a script
// written with spaces, or each letter of a script written without them). So
// the aliases one Extract needs are those its documents write, however many
// the ontology holds.
//
// Words here fold at least as much as the engine's, so whatever the engine
// finds a document writes, Text.Writes finds too: Words decomposes (NFKD),
// lowercases, drops every mark, splits at every character that is not a
// letter or digit, and makes each letter of a script written without spaces
// a word of its own, as is each part of a character that decomposes into
// several words ("½" is 1, 2). It may find more, which costs only an alias
// the engine then ignores; it never finds less, which would leave a name a
// person settled unapplied.
//
// Key is how a name is looked up in an index: a hash of its first KeyWords
// words. Text.Keys gives every key a document's words can match: each run of
// one to KeyWords words, hashed the same way. The document's keys bound the
// lookup by the document, not by the ontology.
package namewords

import (
	"hash/fnv"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// KeyWords is how many of a name's first words its Key hashes. A longer name
// shares its key with the others of its first KeyWords words and is told
// apart by Text.Writes.
const KeyWords = 4

// spaceless reports a letter of a script written without spaces between
// words, as the engine lists them (crates/extraction/src/text.rs,
// spaced_word): Thai and Lao, Myanmar, Hangul (jamo, compatibility jamo and
// syllables), Khmer, Hiragana and Katakana, and the CJK ideographs.
func spaceless(c rune) bool {
	switch {
	case c >= 0x0E00 && c <= 0x0EFF, // Thai, Lao
		c >= 0x1000 && c <= 0x109F,   // Myanmar
		c >= 0x1100 && c <= 0x11FF,   // Hangul jamo
		c >= 0x1780 && c <= 0x17FF,   // Khmer
		c >= 0x3040 && c <= 0x30FF,   // Hiragana, Katakana
		c >= 0x3130 && c <= 0x318F,   // Hangul compatibility jamo
		c >= 0x3400 && c <= 0x4DBF,   // CJK extension A
		c >= 0x4E00 && c <= 0x9FFF,   // CJK unified ideographs
		c >= 0xAC00 && c <= 0xD7AF,   // Hangul syllables
		c >= 0xF900 && c <= 0xFAFF,   // CJK compatibility ideographs
		c >= 0x20000 && c <= 0x3134F: // CJK extensions B to G
		return true
	}
	return false
}

// class is what a character, decomposed and lowercased, is to a word.
type class int

const (
	separator class = iota
	mark            // dropped: a word reads on through it
	letter          // joins the word it is in
	alone           // a word of its own
)

func classOf(c rune) class {
	switch {
	case unicode.In(c, unicode.Mn, unicode.Mc, unicode.Me):
		return mark
	case spaceless(c):
		return alone
	case unicode.IsLetter(c) || unicode.IsNumber(c) || unicode.Is(unicode.Other_Alphabetic, c):
		return letter
	}
	return separator
}

// fold is c decomposed (NFKD) and lowercased, final ς as σ, decomposed again
// in case lowercasing made something that decomposes.
func fold(c rune) []rune {
	var out []rune
	for _, d := range norm.NFKD.String(string(c)) {
		d = unicode.ToLower(d)
		if d == 'ς' {
			d = 'σ'
		}
		out = append(out, []rune(norm.NFKD.String(string(d)))...)
	}
	return out
}

// Words are s's words, folded at least as much as the engine folds them (see
// the package comment).
//
// A character that decomposes into more than one word ("½" into 1⁄2) joins no
// word beside it, as the engine reads it.
func Words(s string) []string {
	var words []string
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			words = append(words, run.String())
			run.Reset()
		}
	}
	for _, c := range s {
		if c < 0x80 {
			// ASCII folds to itself, lowercased.
			switch {
			case 'A' <= c && c <= 'Z':
				run.WriteRune(c + 'a' - 'A')
			case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
				run.WriteRune(c)
			default:
				flush()
			}
			continue
		}
		folded := fold(c)
		split := len(folded) > 1 && slices.ContainsFunc(folded, func(d rune) bool {
			k := classOf(d)
			return k == separator || k == alone
		})
		if split {
			flush()
		}
		for _, d := range folded {
			switch classOf(d) {
			case separator:
				flush()
			case mark:
			case letter:
				run.WriteRune(d)
			case alone:
				flush()
				words = append(words, string(d))
			}
		}
		if split {
			flush()
		}
	}
	flush()
	return words
}

// hash is the key of words, all of them.
func hash(words []string) int64 {
	h := fnv.New64a()
	for _, w := range words {
		_, _ = h.Write([]byte(w)) // never fails
		_, _ = h.Write([]byte{0})
	}
	return int64(h.Sum64()) // #nosec G115 -- a hash: its bits, not its value
}

// Key is the index key of a name normalized as the engine normalizes it
// (norm_name), from its first KeyWords words. A name with no word has none.
func Key(name string) (int64, bool) {
	words := Words(name)
	if len(words) == 0 {
		return 0, false
	}
	return hash(words[:min(len(words), KeyWords)]), true
}

// Text is a document's words, indexed by word.
type Text struct {
	words []string
	at    map[string][]int
}

// NewText reads text's words.
func NewText(text string) *Text {
	t := &Text{words: Words(text), at: map[string][]int{}}
	for i, w := range t.words {
		t.at[w] = append(t.at[w], i)
	}
	return t
}

// Keys are the distinct keys of every run of one to KeyWords of the text's
// words: every Key of a name the text writes is among them. Their number is
// at most KeyWords times the text's words.
func (t *Text) Keys() []int64 {
	seen := map[int64]bool{}
	var keys []int64
	for i := range t.words {
		for n := 1; n <= KeyWords && i+n <= len(t.words); n++ {
			k := hash(t.words[i : i+n])
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// Writes reports whether the text writes name: its words, in order, among
// the text's. Never false where the engine finds the name in the same text.
func (t *Text) Writes(name string) bool {
	want := Words(name)
	if len(want) == 0 {
		return false
	}
	for _, i := range t.at[want[0]] {
		if i+len(want) <= len(t.words) && slices.Equal(t.words[i:i+len(want)], want) {
			return true
		}
	}
	return false
}
