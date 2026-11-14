package namewords_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/pkg/namewords"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ExampleWords() {
	fmt.Println(namewords.Words("ＡＣＭＥ, Inc.™️ — proﬁt 東京 İstanbul Zürich ﾄｳ 3½"))
	// Output: [acme inc tm profit 東 京 istanbul zurich ト ウ 3 1 2]
}

func ExampleText_Writes() {
	text := namewords.NewText("Paid by ACME, Inc. on Monday.")
	fmt.Println(text.Writes("acme inc"), text.Writes("acme corp"))
	// Output: true false
}

// Every pair the engine finds a text writes (crates/extraction/src/text.rs,
// find_words and Alias::written_in) is found here: the names as the engine
// normalizes them (norm_name), the texts as written.
func TestWritesWhatTheEngineFinds(t *testing.T) {
	for _, c := range []struct{ text, name string }{
		{"ACME inc. paid; Acme-Inc and Acmeinc.", "acme inc"},
		{"Paid by ACME, Inc. on Monday.", "acme inc"},
		{"AT&T wrote.", "at&t"},
		{"東京海上は2023年に", "東京海上"},
		{"삼성전자는 말했다", "삼성전자"},
		{"삼성", "삼성"},
		{"Acme社は", "acme"},
		{"Acme™️ Inc", "acme inc"},
		{"ﾄｳｷｮｳ株式会社", "トウキョウ"},
		{"Zürich and ZÜRICH", "zürich"},
		{"İSTANBUL", "istanbul"},
		{"ＡＣＭＥ, proﬁt", "acme profit"},
		{"a a a", "a a"},
		{"ม้าวิ่ง", "ม้า"},
		{"葛\U000E0100飾区", "葛飾区"},
		{"3½ hours", "3 1 2"},
		{"Node.js and .NET", "node.js"},
		{"Node.js and .NET", ".net"},
		{"J.K. Rowling wrote", "j k rowling"},
		{"ΟΔΥΣΣΕΥΣ sailed", "οδυσσευσ"},
		{"Ⅻ Corp", "xii corp"},
		{"Café Noir", "cafe noir"},
		{"Café Noir", "café noir"},
		{"C++ and C#", "c++"},
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
	} {
		assert.False(t, namewords.NewText(c.text).Writes(c.name), "%q writes %q", c.text, c.name)
	}
	_, ok := namewords.Key(" ,. ")
	assert.False(t, ok, "a name with no word has no key")
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

// A long document's words and keys stay proportional to it.
func TestLongText(t *testing.T) {
	text := namewords.NewText(strings.Repeat("Acme paid Helio on Monday. ", 40000))
	assert.LessOrEqual(t, len(text.Keys()), namewords.KeyWords*6)
	assert.True(t, text.Writes("helio on monday acme"))
}
