package integration

import (
	"testing"

	"github.com/stretchr/testify/assert"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// A document's extraction says what the load did with each decision on a
// name that met it, and how many entities a carried name linked (4.22.0).
func TestFoundryDocumentNames(t *testing.T) {
	id, reason := "dec-1", "two_rows"
	out := FoundryDocument(&foundryv1.FoundryDocument{Id: "doc-1", Extraction: &foundryv1.FoundryDocumentExtraction{
		Status: "extracted", ResolvedCarried: 3,
		Names: []*foundryv1.FoundryDocumentName{
			{Decision: "resolve", Type: "Customer", NormName: "acme", DecisionId: &id, Outcome: "written"},
			{Decision: "alias", Type: "Customer", NormName: "helio", Outcome: "refused", Reason: &reason},
		},
	}})
	assert.Equal(t, int32(3), out.Extraction.Carried)
	assert.Equal(t, []FoundryExtractionNameOutput{
		{Decision: "resolve", Type: "Customer", NormName: "acme", DecisionID: "dec-1", Outcome: "written"},
		{Decision: "alias", Type: "Customer", NormName: "helio", Outcome: "refused", Reason: "two_rows"},
	}, out.Extraction.Names)
	// None said, none listed.
	assert.Nil(t, FoundryDocument(&foundryv1.FoundryDocument{Id: "doc-2", Extraction: &foundryv1.FoundryDocumentExtraction{Status: "extracted"}}).Extraction.Names)
}
