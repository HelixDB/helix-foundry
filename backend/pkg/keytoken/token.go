// Package keytoken mints and checks the bearer tokens of Foundry's event
// sources.
package keytoken

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	eventSourcePrefix = "hx_ev_"
	randomSuffixSize  = 64
)

// NewEventSource creates an event source's send-only token: 256 bits from
// crypto/rand as 64 lowercase hex digits after hx_ev_. It sends pieces to
// that one source and does nothing else.
func NewEventSource() (string, error) {
	buf := make([]byte, randomSuffixSize/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token suffix: %w", err)
	}
	return eventSourcePrefix + hex.EncodeToString(buf), nil
}

// ValidateEventSource verifies the event-source-token wire format. It runs
// before any lookup, so a malformed bearer never reaches the database.
func ValidateEventSource(token string) error {
	const kind, prefix = "event source", eventSourcePrefix
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
