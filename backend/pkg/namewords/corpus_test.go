package namewords_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/pkg/namewords"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// entry is one text of the engine's corpus (testdata/README.md): what the
// engine reads in it.
type entry struct {
	Text      string   `json:"text"`
	Norm      string   `json:"norm"`
	Words     []string `json:"words"`
	Written   []string `json:"written"`
	Unwritten []string `json:"unwritten"`
}

func corpus(t testing.TB) []entry {
	raw, err := os.ReadFile("testdata/word_corpus.json")
	require.NoError(t, err)
	var entries []entry
	require.NoError(t, json.Unmarshal(raw, &entries))
	require.Greater(t, len(entries), 2000)
	return entries
}

// joinerless are words without the combining grapheme joiner Go's
// normalization writes past 30 marks in a row, a word of joiners alone left
// out, as Words leaves them.
func joinerless(words []string) []string {
	var out []string
	for _, w := range words {
		if w = strings.ReplaceAll(w, "\u034F", ""); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// Every name the engine writes in a text, Text.Writes finds there, and its
// Key is among the text's Keys: what Foundry needs, as it sends an Extract
// only the aliases found so.
func TestWritesWhatTheEngineWrites(t *testing.T) {
	names, more := 0, 0
	for _, e := range corpus(t) {
		text := namewords.NewText(e.Text)
		keys := text.Keys()
		for _, name := range e.Written {
			names++
			assert.True(t, text.Writes(name), "%q writes %q", e.Text, name)
			key, ok := namewords.Key(name)
			if assert.True(t, ok, "%q has a key", name) {
				assert.Contains(t, keys, key, "%q holds the key of %q", e.Text, name)
			}
		}
		for _, name := range e.Unwritten {
			if text.Writes(name) {
				more++
			}
		}
	}
	t.Logf("%d names the engine writes; %d of those it doesn't write found as well", names, more)
}

// The words here are exactly the engine's, each folded on its own (Coarse):
// the normalized text, the engine's words of it, and Words of the text
// normalized here or there.
func TestReadsTheEngineWords(t *testing.T) {
	for _, e := range corpus(t) {
		norm := namewords.NormName(e.Text)
		// Past 30 marks in a row Go's normalization writes a joiner, which
		// stops marks being reordered across it and a dot counting the
		// letters before it: the words below are still the engine's.
		if !strings.Contains(norm, "\u034F") {
			assert.Equal(t, e.Norm, norm, "norm_name of %q", e.Text)
		}
		engine := joinerless(e.Words)
		require.Equal(t, engine, joinerless(namewords.EngineWords(e.Norm)), "words of %q", e.Norm)
		var folded []string
		for _, w := range engine {
			folded = append(folded, namewords.Coarse(w)...)
		}
		assert.Equal(t, folded, namewords.Words(e.Norm), "Words of %q", e.Norm)
		assert.Equal(t, folded, namewords.Words(norm), "Words of %q", norm)
	}
}

// What Words read of a text holds whatever the text: normalizing twice is
// normalizing once, so a stored name reads the same words normalized again
// or not, and every run of a text's words, as a name, is written there and
// keyed among its keys.
func FuzzWords(f *testing.F) {
	for _, e := range corpus(f) {
		f.Add(e.Text)
	}
	f.Fuzz(func(t *testing.T, s string) {
		norm := namewords.NormName(s)
		require.Equal(t, norm, namewords.NormName(norm), "norm_name of %q", s)
		words := namewords.Words(norm)
		text := namewords.NewText(s)
		keys := text.Keys()
		require.LessOrEqual(t, len(keys), namewords.KeyWords*len(words))
		for i := range words {
			for j := i + 1; j <= len(words); j++ {
				name := strings.Join(words[i:j], " ")
				if !slices.Equal(namewords.Words(name), words[i:j]) {
					continue
				}
				require.True(t, text.Writes(name), "%q writes %q", s, name)
				key, _ := namewords.Key(name)
				require.Contains(t, keys, key, "%q holds the key of %q", s, name)
			}
		}
	})
}
