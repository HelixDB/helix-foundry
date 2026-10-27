package ai

import (
	"errors"
	"strings"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// MaxKeyBytes is the longest API key accepted: the agent refuses a longer
// model key header. Providers' long-term keys are far shorter; a short-term
// Bedrock key made from temporary AWS credentials, whose presigned URL
// carries their session token, is not, so Bedrock is offered with long-term
// keys.
const MaxKeyBytes = 512

// keyContext binds a sealed key to its config row, so a ciphertext copied
// to another row, or another workspace's, does not open.
func keyContext(configID string) string { return "foundry-ai:" + configID }

// SealKey seals key for config configID under the keyring's current key.
func SealKey(keys *secretbox.Keyring, configID string, key ontologyagent.ModelKey) (*model.FoundryAISecret, error) {
	if key.IsZero() {
		return nil, errors.New("ai: no key to seal")
	}
	sealed, keyID, err := keys.Seal([]byte(key.Reveal()), keyContext(configID))
	if err != nil {
		return nil, err
	}
	return &model.FoundryAISecret{Ciphertext: sealed, KeyID: keyID}, nil
}

// OpenKey opens config c's sealed key; the zero key when it holds none.
// secretbox.ErrOpen when the key doesn't open under c's row.
func OpenKey(keys *secretbox.Keyring, c *model.FoundryAIConfig) (ontologyagent.ModelKey, error) {
	if c.Secret == nil {
		return ontologyagent.ModelKey{}, nil
	}
	plain, err := keys.Open(c.Secret.Ciphertext, c.Secret.KeyID, keyContext(c.ID))
	if err != nil {
		return ontologyagent.ModelKey{}, err
	}
	return ontologyagent.NewModelKey(string(plain)), nil
}

// minHidden is the fewest characters of a key its hint leaves out, so the
// rest can't be guessed: every member of the workspace reads hints.
const minHidden = 16

// Hint shows which key a config holds without revealing it: its type
// prefix, up to two dash-separated parts, and its last four characters, as
// connectors' hints are, while at least minHidden characters stay hidden.
// A key too short for that shows its last four, or nothing.
func Hint(key ontologyagent.ModelKey) string {
	k := strings.TrimSpace(key.Reveal())
	if len(k) < minHidden+4 {
		return "…"
	}
	last := k[len(k)-4:]
	parts := strings.SplitN(k, "-", 3)
	if len(parts) == 3 && len(parts[0])+len(parts[1]) <= 8 {
		// The prefix and the dash after it are shown.
		if prefix := parts[0] + "-" + parts[1]; len(k)-len(prefix)-1-len(last) >= minHidden {
			return prefix + "…" + last
		}
	}
	return "…" + last
}
