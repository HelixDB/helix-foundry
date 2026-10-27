package ai

import (
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
)

// Every member reads a config's hint, so it leaves at least minHidden
// characters of the key out: a short key shows its type prefix, or its last
// four characters, only while that still holds, and one too short shows
// nothing.
func TestHint(t *testing.T) {
	for key, want := range map[string]string{
		"sk-ant-api03-" + strings.Repeat("x", 90) + "4f2a": "sk-ant…4f2a",
		"sk-proj-0123456789abcdef4f2a":                     "sk-proj…4f2a",
		"0123456789abcdef0123456789ab4f2a":                 "…4f2a",
		"  0123456789abcdef4f2a  ":                         "…4f2a",
		"my-org-a1b2c3d4e5f6g7h8i":                         "…7h8i",
		"my-org-a1b2c3d4":                                  "…",
		"0123456789abcde4f2a":                              "…",
		"short":                                            "…",
		"":                                                 "…",
	} {
		got := Hint(ontologyagent.NewModelKey(key))
		if got != want {
			t.Errorf("Hint(%q) = %q, want %q", key, got, want)
		}
		if shown := len(got) - len("…"); got != "…" && len(strings.TrimSpace(key))-shown < minHidden {
			t.Errorf("Hint(%q) = %q leaves out fewer than %d characters", key, got, minHidden)
		}
	}
}
