package ontologyagent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// A key carried in a struct must never reach a log, an error, an event or a
// stored row, however it is printed or encoded.
func TestModelKey(t *testing.T) {
	const secret = "sk-ant-api03-sentinel-0123456789"
	key := NewModelKey(secret)

	// Every fmt verb, and every way a struct holding the key prints.
	t.Run("prints redacted", func(t *testing.T) {
		type holder struct {
			Key ModelKey
			key ModelKey
		}
		h := holder{Key: key, key: key}
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T %v"} {
			for _, value := range []any{key, &key, h, &h, []ModelKey{key}, map[string]ModelKey{"k": key}} {
				if got := fmt.Sprintf(format, value); strings.Contains(got, secret) {
					t.Fatalf("Sprintf(%q, %T) = %q", format, value, got)
				}
			}
		}
		if got := fmt.Errorf("call failed with %v", key).Error(); strings.Contains(got, secret) {
			t.Fatalf("error = %q", got)
		}
		if key.String() != "[redacted]" || fmt.Sprint(key) != "[redacted]" {
			t.Fatalf("key prints as %q", fmt.Sprint(key))
		}
	})

	// Both of slog's handlers, given the key alone or inside a struct.
	t.Run("logs redacted", func(t *testing.T) {
		var out bytes.Buffer
		for _, h := range []slog.Handler{slog.NewJSONHandler(&out, nil), slog.NewTextHandler(&out, nil)} {
			slog.New(h).Info("probe", "key", key, "request", struct {
				Key ModelKey
				key ModelKey
			}{key, key}, slog.Group("g", "k", key))
		}
		if strings.Contains(out.String(), secret) {
			t.Fatalf("log = %s", out.String())
		}
	})

	// JSON refuses it outright, so it can't land in a request body or a row.
	t.Run("refuses to encode", func(t *testing.T) {
		if raw, err := json.Marshal(struct{ Key ModelKey }{key}); err == nil {
			t.Fatalf("json = %s", raw)
		}
		if raw, err := json.Marshal(map[string]any{"key": key}); err == nil {
			t.Fatalf("json = %s", raw)
		}
	})

	// Only Reveal hands the key out; an empty key is no key.
	t.Run("reveals only on request", func(t *testing.T) {
		if key.Reveal() != secret || key.IsZero() {
			t.Fatalf("Reveal() = %q, IsZero() = %v", key.Reveal(), key.IsZero())
		}
		if none := NewModelKey(""); !none.IsZero() || none.Reveal() != "" || none != (ModelKey{}) {
			t.Fatalf("an empty key = %#v", none)
		}
	})
}
