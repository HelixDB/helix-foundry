package ontologyagent

import (
	"errors"
	"fmt"
	"log/slog"
)

// redacted is what a ModelKey prints as, however it is printed.
const redacted = "[redacted]"

// ModelKey is a model provider's API key on its way to the agent, which
// takes it in the x-foundry-model-key metadata of the one call that needs
// it, never in a request body. It prints as [redacted] through fmt (every
// verb, %#v included) and slog, and refuses to encode as JSON, so a key
// carried in a struct can't reach a log, an event or a stored row. The zero
// value is no key: the deployment's default, or a provider that needs none.
//
// The key sits behind a pointer: fmt can't call Format on a ModelKey that is
// an unexported field, and prints such a field by reflection, which shows a
// pointer's address, never what it points to.
type ModelKey struct{ v *string }

// NewModelKey wraps key; an empty key is the zero ModelKey.
func NewModelKey(key string) ModelKey {
	if key == "" {
		return ModelKey{}
	}
	return ModelKey{v: &key}
}

// IsZero reports no key.
func (k ModelKey) IsZero() bool { return k.v == nil }

// Reveal is the key itself, for the metadata header and for sealing it; ""
// for no key.
func (k ModelKey) Reveal() string {
	if k.v == nil {
		return ""
	}
	return *k.v
}

func (ModelKey) String() string               { return redacted }
func (ModelKey) GoString() string             { return redacted }
func (ModelKey) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (ModelKey) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (ModelKey) MarshalJSON() ([]byte, error) { return nil, errModelKeyJSON }
func (ModelKey) MarshalText() ([]byte, error) { return nil, errModelKeyJSON }

var errModelKeyJSON = errors.New("a model key is never encoded")
