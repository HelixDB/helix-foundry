// Package namewords finds the aliases a document can use, without the
// engine. From contract 4.23.0 an alias acts only on a document whose text
// writes its name: the engine normalizes the text as it normalizes names
// (norm_name), reads it as words and looks for the name's words in a row
// (crates/extraction/src/text.rs, WordIndex). So the aliases one Extract
// needs are those its documents write, however many the ontology holds.
//
// Words are the engine's words, each then folded further on its own. This
// package reads words exactly as the engine does (norm_name and words, ported
// below: grapheme clusters, a letter of a script written without spaces a
// word with its marks, a cluster that folds to more than one word split from
// the words beside it), and only then folds each word by itself (coarse):
// decomposed (NFKD) with every mark dropped, and a word of a script written
// without spaces read as one word for each letter left, as this package read
// them before it followed the engine's words, so a name's Key is what it was
// unless the engine's words of it changed it. The fold of a word depends on
// that word alone, so Words of any string are the engine's words of it, each
// folded: wherever the engine finds a name's words in a row in a text,
// Text.Writes finds the folded ones in a row too, and the name's Key is among
// the text's Keys. Folding further may find more than the engine (a name with
// a mark where the text has none), which costs only an alias the engine then
// ignores; it never finds less, which would leave a name a person settled
// unapplied.
//
// What is left between the two is their Unicode tables. Go's (package
// unicode, golang.org/x/text/unicode/norm before Go 1.27, rivo/uniseg) are
// Unicode 15.0; the engine's (Rust's char, unicode-normalization,
// unicode-segmentation) are Unicode 17.0. The one rule of grapheme clusters
// added since that changes words (GB9c, conjoined) is applied here from the
// engine's own tables (incb.go). A character first assigned after 15.0 is no
// letter here, so it separates where the engine reads a letter, and a word
// holding one splits here; it splits on both sides, text and name, so the
// name is still found unless that character is all it is. A character added
// after 15.0 whose normalization the engine knows and Go doesn't (a new
// composite letter) is the one way to miss a name. The engine's tests write a
// corpus of what it reads (testdata/word_corpus.json, copied from the engine;
// see testdata/README.md) and the tests here check these words against it.
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
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"golang.org/x/text/unicode/norm"
)

// KeyWords is how many of a name's first words its Key hashes. A longer name
// shares its key with the others of its first KeyWords words and is told
// apart by Text.Writes.
const KeyWords = 4

// alphanumeric is Rust's char::is_alphanumeric: the Alphabetic property
// (letters, letter numbers and Other_Alphabetic, which holds many marks) or
// a number.
func alphanumeric(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsNumber(c) || unicode.Is(unicode.Other_Alphabetic, c)
}

// mark is the engine's is_combining_mark: General_Category Mark.
func mark(c rune) bool {
	return unicode.In(c, unicode.M)
}

// spacedWord is the engine's spaced_word: a character that continues a word
// in a script written with spaces between words. Scripts written without
// them are those the engine lists: Thai and Lao, Myanmar, Hangul (jamo,
// compatibility jamo and syllables), Khmer, Hiragana and Katakana, and the
// CJK ideographs.
func spacedWord(c rune) bool {
	switch {
	case !alphanumeric(c) && !mark(c),
		c >= 0x0E00 && c <= 0x0EFF,   // Thai, Lao
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
		return false
	}
	return true
}

// fold is the engine's fold: NFKC, Turkish İ as I, lowercase, final ς as σ,
// then NFC. Rust's char::to_lowercase maps one character to several only
// for İ, which goes first, so Go's one-to-one unicode.ToLower is the same.
// Bytes that are no UTF-8 become U+FFFD first, as a Rust string holds them
// (norm leaves what follows them unnormalized), which separates words.
func fold(s string) string {
	var b strings.Builder
	for _, c := range norm.NFKC.String(strings.ToValidUTF8(s, "\uFFFD")) {
		if c == 'İ' {
			c = 'I'
		}
		c = unicode.ToLower(c)
		if c == 'ς' {
			c = 'σ'
		}
		b.WriteRune(c)
	}
	return norm.NFC.String(b.String())
}

// normName is the engine's norm_name: s folded, every run of characters
// other than letters, digits, marks, + # & @ $ and a kept dot replaced by
// one space, trimmed. A dot is kept where it starts a word (".net") or joins
// a run of two or more letters or digits to a letter or digit ("node.js").
// Normalizing a name twice gives what normalizing it once does, as the
// engine's tests check.
func normName(s string) string {
	chars := []rune(fold(s))
	out := make([]rune, 0, len(chars))
	for i, c := range chars {
		joinsDot := func() bool {
			wordStart := len(out) == 0 || out[len(out)-1] == ' '
			run := 0
			for run < len(out) && alphanumeric(out[len(out)-1-run]) {
				run++
			}
			return i+1 < len(chars) && alphanumeric(chars[i+1]) && (wordStart || run >= 2)
		}
		switch {
		case alphanumeric(c), mark(c), strings.ContainsRune("+#&@$", c), c == '.' && joinsDot():
			out = append(out, c)
		case len(out) > 0 && out[len(out)-1] != ' ':
			out = append(out, ' ')
		}
	}
	if len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// foldCluster is the engine's fold_cluster: a cluster folded, but for one
// letter that folding leaves as it is (a common ideograph, a Hangul syllable
// or a full-width kana), and with variation selectors left out.
func foldCluster(cluster string) string {
	c, size := utf8.DecodeRuneInString(cluster)
	if size == len(cluster) && (c >= 0x3041 && c <= 0x3096 || c >= 0x30A1 && c <= 0x30FA ||
		c >= 0x4E00 && c <= 0x9FFF || c >= 0xAC00 && c <= 0xD7A3) {
		return cluster
	}
	return strings.Map(func(c rune) rune {
		if c >= 0xFE00 && c <= 0xFE0F || c >= 0xE0100 && c <= 0xE01EF {
			return -1
		}
		return c
	}, fold(cluster))
}

// engineWords are the engine's words of s (text::words): read a grapheme
// cluster at a time, one whose first character is no letter or digit
// separates; any other is folded, and a word is each run of what folds to
// letters, digits and marks of a script written with spaces, or each letter,
// with its marks, of a script written without them. A cluster that folds to
// more than one word ("½" to "1⁄2") joins no word beside it.
func engineWords(s string) []string {
	var words []string
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			words = append(words, run.String())
			run.Reset()
		}
	}
	if isASCII(s) {
		// The engine's fast path, the same words: ASCII letters and digits.
		for i := 0; i < len(s); i++ {
			switch c := s[i]; {
			case 'A' <= c && c <= 'Z':
				run.WriteByte(c + 'a' - 'A')
			case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
				run.WriteByte(c)
			default:
				flush()
			}
		}
		flush()
		return words
	}
	state := -1
	for at := 0; at < len(s); {
		first, _, _, st := uniseg.FirstGraphemeClusterInString(s[at:], state)
		end := at + len(first)
		state = st
		for end < len(s) {
			if c, _ := utf8.DecodeRuneInString(s[end:]); !conjoined(s[at:end], c) {
				break
			}
			more, _, _, st := uniseg.FirstGraphemeClusterInString(s[end:], state)
			end, state = end+len(more), st
		}
		cluster := s[at:end]
		at = end
		if base, _ := utf8.DecodeRuneInString(cluster); !alphanumeric(base) {
			flush()
			continue
		}
		var folded string
		if isASCII(cluster) {
			folded = strings.ToLower(cluster)
		} else {
			folded = foldCluster(cluster)
		}
		// A letter of a script written without spaces is a word with its marks.
		if lead, _ := utf8.DecodeRuneInString(folded); folded != "" && !spacedWord(lead) {
			flush()
			words = append(words, folded)
			continue
		}
		split := strings.IndexFunc(folded, func(c rune) bool { return !spacedWord(c) }) >= 0
		if split {
			flush()
		}
		for _, c := range folded {
			switch {
			case spacedWord(c):
				run.WriteRune(c)
			case alphanumeric(c):
				flush()
				words = append(words, string(c))
			default:
				flush()
			}
		}
		if split {
			flush()
		}
	}
	flush()
	return words
}

// conjoined reports whether the engine's grapheme clusters (Unicode 17.0)
// join a cluster ending as cluster does to the next, starting with c, where
// uniseg's (Unicode 15.0) break: rule GB9c of Unicode 15.1, a consonant after
// a consonant and a linker (a virama, a coeng), with only linkers and
// extending marks between (Indic_Conjunct_Break, incb.go). It changes the
// engine's words in Khmer and Myanmar, where a stacked consonant is one word
// with the letter it stacks under, and wherever such a cluster holds a mark
// of a script written without spaces, which splits it from the words beside
// it.
func conjoined(cluster string, c rune) bool {
	if !unicode.Is(incbConsonant, c) {
		return false
	}
	linkers := 0
	for rest := cluster; rest != ""; {
		c, size := utf8.DecodeLastRuneInString(rest)
		rest = rest[:len(rest)-size]
		switch {
		case unicode.Is(incbLinker, c):
			linkers++
		case unicode.Is(incbExtend, c):
		default:
			return linkers > 0 && unicode.Is(incbConsonant, c)
		}
	}
	return false
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// coarse folds one engine word further, into the words Words reads of it:
// decomposed (NFKD) with every mark dropped, or the word as it is if that
// leaves nothing, so no word is lost; and a word of a script written without
// spaces (one letter with its marks, a Hangul syllable) is one word for each
// character that leaves, as a syllable's jamo.
func coarse(word string) []string {
	if isASCII(word) {
		return []string{word}
	}
	folded := strings.Map(func(c rune) rune {
		if mark(c) {
			return -1
		}
		return c
	}, norm.NFKD.String(word))
	if folded == "" {
		folded = word
	}
	if first, _ := utf8.DecodeRuneInString(word); spacedWord(first) {
		return []string{folded}
	}
	return strings.Split(folded, "")
}

// Words are s's words as the engine reads them, each folded further on its
// own (see the package comment). Of a name, s is the name as the engine
// normalizes it (norm_name); a document's are those of its normalized text,
// which NewText reads.
//
// Go's normalization writes a combining grapheme joiner (U+034F) after every
// 30 marks in a row (the Stream-Safe Text Format) and the engine's doesn't,
// so a joiner is left out of every word here, and a word of joiners alone
// with it: on both sides, text and name, so a run of the engine's words is
// still a run here.
func Words(s string) []string {
	var words []string
	for _, w := range engineWords(s) {
		if w = strings.ReplaceAll(w, "\u034F", ""); w != "" {
			words = append(words, coarse(w)...)
		}
	}
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

// NewText reads the words of text, a document's whole text, as the engine
// does: normalized as a name is, then read as words.
func NewText(text string) *Text {
	t := &Text{words: Words(normName(text)), at: map[string][]int{}}
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

// Writes reports whether the text writes name, a name as the engine
// normalizes it: its words, in order, among the text's. Never false where
// the engine finds the name in the same text.
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
