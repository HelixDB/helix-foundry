package integration

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The published contract (contract/foundry-integration.openapi.json) names exactly the routes the
// handler serves, each with the query parameters it takes, so neither can change without the other.
func TestHTTPRoutesMatchTheContract(t *testing.T) {
	raw, err := os.ReadFile("../../contract/foundry-integration.openapi.json")
	require.NoError(t, err)
	var contract struct {
		Paths map[string]map[string]struct {
			Parameters []struct{ Name, In string } `json:"parameters"`
		} `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(raw, &contract))
	published := map[string][]string{}
	for path, operations := range contract.Paths {
		for method, operation := range operations {
			query := []string{}
			for _, p := range operation.Parameters {
				if p.In == "query" {
					query = append(query, p.Name)
				}
			}
			slices.Sort(query)
			published[strings.ToUpper(method)+" "+path] = query
		}
	}
	served := map[string][]string{}
	for _, route := range httpRoutes {
		query := append([]string{}, route.query...)
		slices.Sort(query)
		served[route.method+" "+route.pattern] = query
	}
	assert.Equal(t, published, served)
}
