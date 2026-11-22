// Package keytoken mints and checks Foundry's bearer tokens that anyone may
// try to guess at a public door: event sources' send tokens (hx_ev_) and the
// Slack relay's link tokens (hx_rl_).
package keytoken

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	eventSourcePrefix = "hx_ev_"
	relayPrefix       = "hx_rl_"
	randomSuffixSize  = 64
)

// NewEventSource creates an event source's send-only token: 256 bits from
// crypto/rand as 64 lowercase hex digits after hx_ev_. It sends pieces to
// that one source and does nothing else.
func NewEventSource() (string, error) { return newToken(eventSourcePrefix) }

// ValidateEventSource verifies the event-source-token wire format. It runs
// before any lookup, so a malformed bearer never reaches the database.
func ValidateEventSource(token string) error {
	return validate("event source", eventSourcePrefix, token)
}

// NewRelay creates a Slack relay link's token, which a local Foundry install
// claims Slack's requests and calls Slack through Helix with: 256 bits from
// crypto/rand as 64 lowercase hex digits after hx_rl_. It reaches only its
// link's relayed installs, and nothing else of Helix.
func NewRelay() (string, error) { return newToken(relayPrefix) }

// ValidateRelay verifies the relay-token wire format, before any lookup.
func ValidateRelay(token string) error { return validate("relay", relayPrefix, token) }

// newToken is prefix and 64 random lowercase hex digits.
func newToken(prefix string) (string, error) {
	buf := make([]byte, randomSuffixSize/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token suffix: %w", err)
	}
	return prefix + hex.EncodeToString(buf), nil
}

// validate refuses a token that isn't prefix and 64 lowercase hex digits.
func validate(kind, prefix, token string) error {
	if !strings.HasPrefix(token, prefix) {
		return fmt.Errorf("%s token must start with %q", kind, prefix)
	}
	suffix := token[len(prefix):]
	if len(suffix) != randomSuffixSize {
		return fmt.Errorf("%s token must contain %d characters after %q", kind, randomSuffixSize, prefix)
	}
	for _, char := range suffix {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return fmt.Errorf("%s token suffix must contain only lowercase hexadecimal characters", kind)
		}
	}
	return nil
}
