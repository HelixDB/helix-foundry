package integration

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
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

// contractSchema is the contract's component schema name, resolved with the components it refers
// to, so a route's answer can be checked against it.
func contractSchema(t *testing.T, name string) *jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile("../../contract/foundry-integration.openapi.json")
	require.NoError(t, err)
	var contract struct {
		Components struct {
			Schemas json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(raw, &contract))
	var defs map[string]*jsonschema.Schema
	rewritten := strings.ReplaceAll(string(contract.Components.Schemas), "#/components/schemas/", "#/$defs/")
	require.NoError(t, json.Unmarshal([]byte(rewritten), &defs))
	require.Contains(t, defs, name)
	resolved, err := (&jsonschema.Schema{Ref: "#/$defs/" + name, Defs: defs}).Resolve(nil)
	require.NoError(t, err)
	return resolved
}

// Each read route's answer is what the contract publishes for it: a client generated from the
// contract decodes it.
func TestHTTPAnswersMatchTheContract(t *testing.T) {
	for _, c := range []struct{ method, path, body, schema string }{
		{"GET", "/v1/ontologies", "", "OntologyList"},
		{"GET", "/v1/ontologies/ontology-1", "", "OntologyDetail"},
		{"GET", "/v1/ontologies/ontology-1/schema", "", "Schema"},
		{"POST", "/v1/ontologies/ontology-1/reads", `{"reads":[{"entity":{"type":"Person","name":"Ada"}}]}`, "ReadResult"},
		{"GET", "/v1/ontologies/ontology-1/loads/load-1", "", "LoadDetail"},
		{"GET", "/v1/ontologies/ontology-1/documents", "", "DocumentList"},
		{"GET", "/v1/ontologies/ontology-1/documents/doc-1", "", "DocumentText"},
		{"GET", "/v1/ontologies/ontology-1/questions", "", "QuestionList"},
		{"GET", "/v1/ontologies/ontology-1/questions?bogus=1", "", "Error"},
	} {
		t.Run(c.path, func(t *testing.T) {
			h := readsFixture(t, false, false)
			w := h.do(t, c.method, c.path, c.body)
			if c.schema != "Error" {
				require.Equal(t, 200, w.Code, w.Body.String())
			}
			var answer any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &answer))
			assert.NoError(t, contractSchema(t, c.schema).Validate(answer), w.Body.String())
		})
	}
}

// Each operation takes a host's bearer, or a partner app's token holding any one of the scopes
// the route needs (HTTPScopes), which the partnerOAuth scheme lists; its 401 and 403 name the
// codes a bearer refusal answers with.
func TestHTTPSecurityMatchesTheContract(t *testing.T) {
	raw, err := os.ReadFile("../../contract/foundry-integration.openapi.json")
	require.NoError(t, err)
	var contract struct {
		Paths map[string]map[string]struct {
			Security []map[string][]string `json:"security"`
		} `json:"paths"`
		Components struct {
			SecuritySchemes struct {
				PartnerOAuth struct {
					Type  string `json:"type"`
					Flows struct {
						AuthorizationCode struct {
							AuthorizationURL string            `json:"authorizationUrl"`
							TokenURL         string            `json:"tokenUrl"`
							RefreshURL       string            `json:"refreshUrl"`
							Scopes           map[string]string `json:"scopes"`
						} `json:"authorizationCode"`
					} `json:"flows"`
				} `json:"partnerOAuth"`
			} `json:"securitySchemes"`
			Responses map[string]struct {
				Description string `json:"description"`
			} `json:"responses"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(raw, &contract))
	for _, route := range httpRoutes {
		operation, ok := contract.Paths[route.pattern][strings.ToLower(route.method)]
		require.True(t, ok, route.pattern)
		want := []map[string][]string{{"bearerAuth": {}}}
		for _, scope := range HTTPScopes(route.tool) {
			want = append(want, map[string][]string{"partnerOAuth": {scope}})
		}
		assert.Equal(t, want, operation.Security, "%s %s", route.method, route.pattern)
	}
	oauth := contract.Components.SecuritySchemes.PartnerOAuth
	assert.Equal(t, "oauth2", oauth.Type)
	flow := oauth.Flows.AuthorizationCode
	assert.True(t, strings.HasSuffix(flow.AuthorizationURL, "/apps/oauth/authorize"), flow.AuthorizationURL)
	assert.True(t, strings.HasSuffix(flow.TokenURL, "/apps/oauth/token"), flow.TokenURL)
	assert.Equal(t, flow.TokenURL, flow.RefreshURL)
	assert.ElementsMatch(t, []string{HTTPScopeRead, HTTPScopeAsk}, slices.Collect(maps.Keys(flow.Scopes)))
	assert.Contains(t, contract.Components.Responses["Unauthorized"].Description, "invalid_token")
	assert.Contains(t, contract.Components.Responses["Unauthorized"].Description, "unauthenticated")
	assert.Contains(t, contract.Components.Responses["Forbidden"].Description, "insufficient_scope")
}
