package integration

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// generate_ontology's sample takes the sample company's ready-made draft. It is passed on only as
// the backend takes it: "acme", from exactly one connector and one document set, and no API source.
func TestGenerateOntologyTakesTheSampleAsTheBackendDoes(t *testing.T) {
	prepare := func(payload map[string]any) (FoundryOperation, error) {
		return (&Service{Events: true}).ValidateFoundryOperation(context.Background(), reader(), FoundryOperationInput{
			Operation: "generate_ontology", Target: "ontology:workspace-1/ontology-1", Payload: payload,
		})
	}
	sample := map[string]any{"goal": "Acme's accounts", "connector_ids": []any{"connector-1"}, "document_set_ids": []any{"set-1"}, "sample": "acme"}
	op, err := prepare(sample)
	require.NoError(t, err)
	assert.Equal(t, "acme", op.(GenerateOntologyOperation).req.GetSample())
	assert.Equal(t, []string{"connector-1"}, op.(GenerateOntologyOperation).req.GetConnectorIds())
	assert.Contains(t, string(op.CanonicalPayload()), `"sample":"acme"`)

	// Without it, a generation is the ontology agent's, as before.
	op, err = prepare(map[string]any{"goal": "Acme's accounts"})
	require.NoError(t, err)
	assert.Empty(t, op.(GenerateOntologyOperation).req.GetSample())
	assert.NotContains(t, string(op.CanonicalPayload()), "sample")

	for name, payload := range map[string]map[string]any{
		"another sample":     {"connector_ids": []any{"connector-1"}, "document_set_ids": []any{"set-1"}, "sample": "globex"},
		"no connector":       {"document_set_ids": []any{"set-1"}, "sample": "acme"},
		"two connectors":     {"connector_ids": []any{"connector-1", "connector-2"}, "document_set_ids": []any{"set-1"}, "sample": "acme"},
		"no document set":    {"connector_ids": []any{"connector-1"}, "sample": "acme"},
		"an API source":      {"connector_ids": []any{"connector-1"}, "document_set_ids": []any{"set-1"}, "sample": "acme", "event_sources": []any{map[string]any{"connector_id": "events-1", "mode": "structured"}}},
		"not a string":       {"connector_ids": []any{"connector-1"}, "document_set_ids": []any{"set-1"}, "sample": true},
		"with no_connectors": {"no_connectors": true, "document_set_ids": []any{"set-1"}, "sample": "acme"},
	} {
		_, err := prepare(payload)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), `with sample "acme" exactly one connector, one document set and no event_sources`, name)
	}
}
