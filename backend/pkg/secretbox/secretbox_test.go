package secretbox

import (
	"bytes"
	"errors"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, keyBytes) }

func TestKeyring(t *testing.T) {
	// A sealed value opens with the same key and context.
	t.Run("round trips", func(t *testing.T) {
		ring, err := NewKeyring("k1", map[string][]byte{"k1": key(1)})
		if err != nil {
			t.Fatal(err)
		}
		sealed, keyID, err := ring.Seal([]byte("sk_live_123"), "connector:c1")
		if err != nil {
			t.Fatal(err)
		}
		if keyID != "k1" || bytes.Contains(sealed, []byte("sk_live_123")) {
			t.Fatalf("sealed under %q leaks plaintext: %q", keyID, sealed)
		}
		got, err := ring.Open(sealed, keyID, "connector:c1")
		if err != nil || string(got) != "sk_live_123" {
			t.Fatalf("Open = (%q, %v)", got, err)
		}
	})
	// Rotation: rows sealed before the switch keep opening with the previous key.
	t.Run("opens values sealed under a previous key", func(t *testing.T) {
		old, _ := NewKeyring("k1", map[string][]byte{"k1": key(1)})
		sealed, keyID, _ := old.Seal([]byte("secret"), "ctx")
		rotated, err := NewKeyring("k2", map[string][]byte{"k1": key(1), "k2": key(2)})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := rotated.Open(sealed, keyID, "ctx"); err != nil || string(got) != "secret" {
			t.Fatalf("Open = (%q, %v)", got, err)
		}
		if _, newID, _ := rotated.Seal([]byte("secret"), "ctx"); newID != "k2" {
			t.Fatalf("new values sealed under %q, want k2", newID)
		}
	})
	// A ciphertext copied to another row, pointed at another key, or edited must not open.
	t.Run("refuses a moved, re-keyed or tampered value", func(t *testing.T) {
		ring, _ := NewKeyring("k1", map[string][]byte{"k1": key(1), "k2": key(2)})
		sealed, keyID, _ := ring.Seal([]byte("secret"), "connector:c1")
		tampered := append([]byte(nil), sealed...)
		tampered[len(tampered)-1] ^= 1
		for name, open := range map[string]func() ([]byte, error){
			"other context": func() ([]byte, error) { return ring.Open(sealed, keyID, "connector:c2") },
			"other key id":  func() ([]byte, error) { return ring.Open(sealed, "k2", "connector:c1") },
			"unknown key":   func() ([]byte, error) { return ring.Open(sealed, "k9", "connector:c1") },
			"flipped byte":  func() ([]byte, error) { return ring.Open(tampered, keyID, "connector:c1") },
			"truncated":     func() ([]byte, error) { return ring.Open(sealed[:5], keyID, "connector:c1") },
		} {
			if _, err := open(); !errors.Is(err, ErrOpen) {
				t.Errorf("%s: Open error = %v, want ErrOpen", name, err)
			}
		}
	})
	t.Run("rejects a keyring without its current key or with a short key", func(t *testing.T) {
		if _, err := NewKeyring("k2", map[string][]byte{"k1": key(1)}); err == nil {
			t.Error("accepted a missing current key")
		}
		if _, err := NewKeyring("k1", map[string][]byte{"k1": []byte("short")}); err == nil {
			t.Error("accepted a short key")
		}
	})
}
