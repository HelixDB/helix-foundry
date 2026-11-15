package namewords

import (
	"testing"
	"unicode"

	"github.com/rivo/uniseg"
	"github.com/stretchr/testify/assert"
)

// uniseg's grapheme clusters are the engine's (gcb_test.go, from the engine's
// tables) for every character Go knows, as far as a normalized name's
// clusters depend on them: what joins the cluster before it (rules GB9 and
// GB9a) and what joins the cluster after it (GB9b, a prefix), but for
// U+11A3A, which engineWords cuts as the engine does. The other values
// (controls, joiners, regional indicators, pictographs) are of characters
// normName replaces by a space, and Hangul's have not changed.
func TestUnisegClustersAreTheEngines(t *testing.T) {
	for c := rune(0); c <= unicode.MaxRune; c++ {
		if c >= 0xD800 && c <= 0xDFFF || !unicode.In(c, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z, unicode.Cc, unicode.Cf, unicode.Co) {
			continue
		}
		after, _, _, _ := uniseg.FirstGraphemeClusterInString("a"+string(c), -1)
		if unicode.Is(gcbJoins, c) != (after != "a") {
			t.Errorf("U+%04X joins the cluster before it to one side only", c)
		}
		before, _, _, _ := uniseg.FirstGraphemeClusterInString(string(c)+"a", -1)
		if c != 0x11A3A && unicode.Is(gcbPrepend, c) != (before != string(c)) {
			t.Errorf("U+%04X is a prefix to one side only", c)
		}
	}
	before, _, _, _ := uniseg.FirstGraphemeClusterInString("\U00011A3A ", -1)
	assert.Equal(t, "\U00011A3A ", before, "U+11A3A is a prefix to uniseg")
	assert.False(t, unicode.Is(gcbPrepend, 0x11A3A), "and no prefix to the engine")
	assert.Equal(t, []string{"x\U00011A3A", "y"}, engineWords("x\U00011A3A y"))
	assert.Equal(t, []string{"\U00011A3Á", "y"}, engineWords("\U00011A3Á y"))
	assert.Equal(t, []string{"\U00011A3A\U00011A3A", "東"}, engineWords("\U00011A3A\U00011A3A東"))
}
