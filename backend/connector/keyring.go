package connector

import (
	"encoding/base64"
	"fmt"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
)

// NewKeyring builds the keyring that seals connector credentials, or nil when
// connectors are not configured.
func NewKeyring(c config.ConnectorsConfig) (*secretbox.Keyring, error) {
	if !c.Enabled() {
		return nil, nil
	}
	keys := map[string][]byte{}
	for keyID, encoded := range map[string]string{c.CurrentKeyID: c.CurrentKey, c.PreviousKeyID: c.PreviousKey} {
		if keyID == "" {
			continue
		}
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("foundry.connectors key %s: %w", keyID, err)
		}
		keys[keyID] = key
	}
	return secretbox.NewKeyring(c.CurrentKeyID, keys)
}
