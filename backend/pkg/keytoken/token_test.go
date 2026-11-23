package keytoken

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func ExampleNewEventSource() {
	token, _ := NewEventSource()
	fmt.Println(strings.HasPrefix(token, "hx_ev_"), len(token), ValidateEventSource(token))
	// Output: true 70 <nil>
}

func ExampleNewRelay() {
	token, _ := NewRelay()
	fmt.Println(strings.HasPrefix(token, "hx_rl_"), len(token), ValidateRelay(token), ValidateEventSource(token) != nil)
	// Output: true 70 <nil> true
}

func TestEventSourceToken(t *testing.T) {
	t.Parallel()

	// A send token is shown once and stored only as its hash, so every mint
	// must be the documented format and unguessable from the one before.
	t.Run("mints distinct tokens in the documented format", func(t *testing.T) {
		t.Parallel()
		format := regexp.MustCompile(`^hx_ev_[0-9a-f]{64}$`)
		seen := map[string]bool{}
		for range 64 {
			token, err := NewEventSource()
			if err != nil {
				t.Fatalf("new event source token: %v", err)
			}
			if !format.MatchString(token) {
				t.Fatalf("token %q does not match %s", token, format)
			}
			if err := ValidateEventSource(token); err != nil {
				t.Fatalf("freshly minted token rejected: %v", err)
			}
			if seen[token] {
				t.Fatalf("token %q minted twice", token)
			}
			seen[token] = true
		}
	})

	// Only hx_ev_ and exactly 64 lowercase hex digits is a send token; anything
	// near it is a typo or a forgery and must fail before any lookup.
	valid := eventSourcePrefix + strings.Repeat("0123456789abcdef", 4)
	if err := ValidateEventSource(valid); err != nil {
		t.Fatalf("ValidateEventSource(%q): %v", valid, err)
	}
	for _, tc := range []struct{ name, token string }{
		{"empty", ""},
		{"prefix only", eventSourcePrefix},
		{"no prefix", strings.Repeat("a", randomSuffixSize)},
		{"uppercase prefix", "HX_EV_" + valid[len(eventSourcePrefix):]},
		{"prefix without its underscore", "hx_ev" + valid[len(eventSourcePrefix):]},
		{"one char short", valid[:len(valid)-1]},
		{"one char long", valid + "a"},
		{"uppercase hex", eventSourcePrefix + strings.Repeat("A", randomSuffixSize)},
		{"non hex", valid[:len(valid)-1] + "g"},
		{"a multibyte character in 64 bytes", valid[:len(valid)-2] + "é"},
		{"leading space", " " + valid[:len(valid)-1]},
		{"trailing newline", valid[:len(valid)-1] + "\n"},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateEventSource(tc.token); err == nil {
				t.Fatalf("ValidateEventSource accepted %q", tc.token)
			}
		})
	}
}

// A relay token is its own kind: an event source's token, or anything near
// one, is never a relay token, and the other way round.
func TestRelayToken(t *testing.T) {
	t.Parallel()
	format := regexp.MustCompile(`^hx_rl_[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for range 64 {
		token, err := NewRelay()
		if err != nil {
			t.Fatalf("new relay token: %v", err)
		}
		if !format.MatchString(token) || ValidateRelay(token) != nil || seen[token] {
			t.Fatalf("relay token %q: not the format, refused, or minted twice", token)
		}
		seen[token] = true
	}
	suffix := strings.Repeat("0123456789abcdef", 4)
	for _, tc := range []struct{ name, token string }{
		{"an event source's", eventSourcePrefix + suffix},
		{"empty", ""},
		{"prefix only", relayPrefix},
		{"short", relayPrefix + suffix[1:]},
		{"long", relayPrefix + suffix + "0"},
		{"uppercase hex", relayPrefix + strings.ToUpper(suffix)},
		{"non hex", relayPrefix + suffix[1:] + "z"},
	} {
		if err := ValidateRelay(tc.token); err == nil {
			t.Errorf("ValidateRelay accepted %s: %q", tc.name, tc.token)
		}
	}
	if err := ValidateEventSource(relayPrefix + suffix); err == nil {
		t.Error("ValidateEventSource accepted a relay token")
	}
}
