package namewords_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/pkg/namewords"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A name's words, the name as the engine normalizes it: the engine's words,
// each folded on its own. A Thai mark on a Latin letter splits it from its
// word, as the engine reads it, and is dropped as every mark is.
func ExampleWords() {
	fmt.Println(namewords.Words("acme inc.tm profit 東京 zürich 31 2 ジョン スミス acme\u0E31"))
	// Output: [acme inc tm profit 東 京 zurich 31 2 シ ョ ン ス ミ ス acm e]
}

// A document's words: its text normalized as a name is, then read as words.
// A symbol that folds to letters joins the word beside it, and a Thai mark
// on a Latin letter splits it from its word, as in the engine.
func ExampleNewText() {
	text := namewords.NewText("ＡＣＭＥ, Inc.™️ — Slack™ 3½")
	fmt.Println(text.Writes("acme inc.tm"), text.Writes("slacktm"), text.Writes("31 2"), text.Writes("slack"))
	thai := namewords.NewText("Acme\u0E31")
	fmt.Println(thai.Writes("acm"), thai.Writes("acme"))
	// Output:
	// true true true false
	// true false
}

func ExampleText_Writes() {
	text := namewords.NewText("Paid by ACME, Inc. on Monday.")
	fmt.Println(text.Writes("acme inc"), text.Writes("acme corp"))
	// Output: true false
}

// Every pair the engine finds a text writes (crates/extraction/src/text.rs,
// WordIndex and its doc tests, and Alias::written_in) is found here: the
// names as the engine normalizes them (norm_name), the texts as written.
func TestWritesWhatTheEngineFinds(t *testing.T) {
	for _, c := range []struct{ text, name string }{
		{"ACME inc. paid; Acme-Inc and Acmeinc.", "acme inc"},
		{"Paid by ACME, Inc. on Monday.", "acme inc"},
		{"AT&T wrote.", "at&t"},
		{"東京海上は2023年に", "東京海上"},
		{"삼성전자는 말했다", "삼성전자"},
		{"\u1109\u1161\u11B7\u1109\u1165\u11BC", "삼성"},
		{"Acme社は", "acme"},
		{"ﾄｳｷｮｳ株式会社", "トウキョウ"},
		{"Zu\u0308rich and ZÜRICH", "zürich"},
		{"İSTANBUL", "istanbul"},
		{"ＡＣＭＥ, proﬁt", "acme profit"},
		{"a a a", "a a"},
		{"ม้าวิ่ง", "ม้า"},
		{"葛\U000E0100飾区", "葛飾区"},
		{"3½ hours", "31 2"},
		{"Node.js and .NET", "node.js"},
		{"Node.js and .NET", ".net"},
		{"J.K. Rowling wrote", "j k rowling"},
		{"ΟΔΥΣΣΕΥΣ sailed", "οδυσσευσ"},
		{"Ⅻ Corp", "xii corp"},
		{"Café Noir", "cafe noir"},
		{"Cafe\u0301 Noir", "café noir"},
		{"C++ and C#", "c++"},
		// Folded before words are read, on both sides (the engine's WordIndex doc tests).
		{"トヨタ自動車㈱は", "トヨタ自動車 株"},
		{"ジョン・スミス氏は来日した", "ジョン スミス"},
		{"ジョン･スミス", "ジョン スミス"},
		{"ทัวร์กำแพงเพชร", "กําแพงเพชร"},
		{"از دیجی\u200cکالا خرید", "دیجی کالا"},
		{"Slack™ wrote.", "slacktm"},
		{"Acme Inc.™ Group", "acme inc tm group"},
		// A mark of a script written without spaces splits a Latin word.
		{"Acme\u0E31", "acm"},
		{"Acme\u0E31 Corp", "acm e"},
		{"한국 \u1112\u1161\u11AB\u1100\u116E\u11A8", "한국 한국"},
		// A letter Unicode 15.0 reads as a prefix of what follows it.
		{"x\U00011A3A y", "x\U00011A3A"},
		{"Acme\U00011A3A東京", "acme\U00011A3A東京"},
		// A Thai mark the engine orders before 30 marks and Go's
		// normalization after them.
		{"xa" + strings.Repeat("\u0301", 30) + "\u0E48 corp", "x\u00E1\u0E48" + strings.Repeat("\u0301", 29)},
	} {
		assert.True(t, namewords.NewText(c.text).Writes(c.name), "%q writes %q", c.text, c.name)
		key, ok := namewords.Key(c.name)
		require.True(t, ok, c.name)
		assert.Contains(t, namewords.NewText(c.text).Keys(), key, "%q holds the key of %q", c.text, c.name)
	}
}

// What no text writes, a word inside a longer word included.
func TestDoesNotWriteWhatIsNotThere(t *testing.T) {
	for _, c := range []struct{ text, name string }{
		{"Acmeinc wrote.", "acme inc"},
		{"IBM wrote.", "international business machines"},
		{"acme", "acme inc"},
		{"", "acme"},
		{"ACME and profit", "acme profit"},
		{"Helio is run by the new CTO.", "mia"},
		{"Slack™ wrote.", "slack"},
	} {
		assert.False(t, namewords.NewText(c.text).Writes(c.name), "%q writes %q", c.text, c.name)
	}
	_, ok := namewords.Key(" ,. ")
	assert.False(t, ok, "a name with no word has no key")
	_, ok = namewords.Key("\u0E31 \u102C")
	assert.False(t, ok, "a name of marks alone has no word, so no key")
	assert.False(t, namewords.NewText("anything").Writes("..."))
}

// A name's key is that of its first KeyWords words, whatever follows, and
// the text's keys hold every run of one to KeyWords of its words.
func TestKeys(t *testing.T) {
	long, _ := namewords.Key("bank of america corporation holdings")
	prefix, _ := namewords.Key("bank of america corporation")
	assert.Equal(t, prefix, long)
	other, _ := namewords.Key("bank of england")
	assert.NotEqual(t, other, long)
	text := namewords.NewText("the bank of america corporation holdings")
	keys := text.Keys()
	assert.Contains(t, keys, long)
	// Six words: runs of 1 to 4 starting at each, all distinct here.
	assert.Len(t, keys, 6+5+4+3)
	assert.Len(t, namewords.NewText("a a a a a").Keys(), 4, "a run repeated is one key")
	assert.Empty(t, namewords.NewText("").Keys())
	assert.True(t, text.Writes("bank of america corporation holdings"))
	assert.False(t, text.Writes("bank of america corporation group"))
}

// A name keeps the key the code before namewords followed the engine's words
// wrote for it (e095f72a), as name_key holds it: Latin, ideographs, Hangul,
// kana, Thai, Khmer, Myanmar with a vowel sign the engine reads as a word of
// its own, Devanagari, Persian. Only a name with a mark of a script written
// without spaces on a letter of another script is keyed anew (AGENTS.md).
func TestKeysStayAsStored(t *testing.T) {
	for name, stored := range map[string]int64{
		"acme inc":                              4469446710636287933,
		"\u6771\u4eac\u6d77\u4e0a":              5534316199611900339,
		"\uc0bc\uc131\uc804\uc790":              -5280846229312718222,
		"\u30c8\u30a6\u30ad\u30e7\u30a6":        4500527990477870533,
		"\u30b8\u30e7\u30f3 \u30b9\u30df\u30b9": -2659336687061285153,
		"z\u00fcrich":                           2506042186143321930,
		"\u0e01\u0e4d\u0e32\u0e41\u0e1e\u0e07\u0e40\u0e1e\u0e0a\u0e23":              -5122843576761837602,
		"\u1797\u17b6\u179f\u17b6":                                                  9154638651645074421,
		"\u1019\u103c\u1014\u103a\u1019\u102c":                                      -6090885063908456576,
		"\u0939\u093f\u0928\u094d\u0926\u0940 \u0938\u092e\u093e\u091a\u093e\u0930": 2053135253562738914,
		"\u062f\u06cc\u062c\u06cc \u06a9\u0627\u0644\u0627":                         -2403174405466066220,
		"slacktm": 4736688344099807972,
		"31 2":    -4077657761655852447,
		"node.js": 8374130453238344660,
	} {
		key, ok := namewords.Key(name)
		assert.True(t, ok, name)
		assert.Equal(t, stored, key, "the key of %q", name)
	}
	before, _ := namewords.Key("acme\u0E31")
	assert.NotEqual(t, int64(1905436304627946365), before, "a Thai mark on a Latin letter is keyed anew")
}

// A long document's words and keys stay proportional to it.
func TestLongText(t *testing.T) {
	text := namewords.NewText(strings.Repeat("Acme paid Helio on Monday. ", 40000))
	assert.LessOrEqual(t, len(text.Keys()), namewords.KeyWords*6)
	assert.True(t, text.Writes("helio on monday acme"))
}
