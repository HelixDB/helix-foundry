package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
)

const (
	keyBytes   = 32
	nonceBytes = 12
)

// ErrOpen is deliberately non-specific: a wrong key, a wrong context and a
// tampered ciphertext are indistinguishable to the caller.
var ErrOpen = errors.New("secretbox: cannot open sealed value")

var keyIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// Keyring seals small secrets for storage under rotating AES-256-GCM keys.
// New values use the current key; older rows open with any key still held.
type Keyring struct {
	current string
	aeads   map[string]cipher.AEAD
}

// NewKeyring takes 32-byte keys by id; current must be one of them.
func NewKeyring(current string, keys map[string][]byte) (*Keyring, error) {
	if _, ok := keys[current]; !ok {
		return nil, fmt.Errorf("secretbox: current key %q is not in the keyring", current)
	}
	aeads := make(map[string]cipher.AEAD, len(keys))
	for id, key := range keys {
		if !keyIDRE.MatchString(id) {
			return nil, fmt.Errorf("secretbox: key id %q is invalid", id)
		}
		if len(key) != keyBytes {
			return nil, fmt.Errorf("secretbox: key %q must be %d bytes", id, keyBytes)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("secretbox: key %q: %w", id, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("secretbox: key %q: %w", id, err)
		}
		aeads[id] = aead
	}
	return &Keyring{current: current, aeads: aeads}, nil
}

// Seal encrypts plaintext under the current key. context binds the result to
// where it is stored (for example a row id), so a ciphertext copied elsewhere
// does not open. Store the returned key id beside the ciphertext.
func (k *Keyring) Seal(plaintext []byte, context string) (sealed []byte, keyID string, err error) {
	nonce := make([]byte, nonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", fmt.Errorf("secretbox: nonce: %w", err)
	}
	aead := k.aeads[k.current]
	return aead.Seal(nonce, nonce, plaintext, associatedData(k.current, context)), k.current, nil
}

// Open decrypts a value sealed under keyID with the same context.
func (k *Keyring) Open(sealed []byte, keyID, context string) ([]byte, error) {
	aead, ok := k.aeads[keyID]
	if !ok || len(sealed) < nonceBytes+aead.Overhead() {
		return nil, ErrOpen
	}
	plaintext, err := aead.Open(nil, sealed[:nonceBytes], sealed[nonceBytes:], associatedData(keyID, context))
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}

// associatedData binds the key id too, so editing the stored key id column
// cannot redirect a ciphertext to another key.
func associatedData(keyID, context string) []byte {
	return []byte("helix.secretbox\x00v1\x00" + keyID + "\x00" + context)
}
