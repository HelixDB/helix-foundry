package integration

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// contextBackend answers get_context's reads: one ontology, its loads and its versions by id.
type contextBackend struct {
	foundryv1.FoundryServiceClient
	ontology *foundryv1.FoundryOntology
	loads    map[string]*foundryv1.FoundryLoad
	versions map[string]*foundryv1.OntologyVersion
	read     []string
}

func (b *contextBackend) GetFoundryOntology(context.Context, *foundryv1.GetFoundryOntologyRequest, ...grpc.CallOption) (*foundryv1.FoundryOntologyResponse, error) {
	return &foundryv1.FoundryOntologyResponse{Ontology: b.ontology}, nil
}

func (b *contextBackend) GetFoundryLoad(_ context.Context, req *foundryv1.GetFoundryLoadRequest, _ ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error) {
	return &foundryv1.FoundryLoadResponse{Load: b.loads[req.GetId()]}, nil
}

func (b *contextBackend) GetOntologyVersion(_ context.Context, req *foundryv1.GetOntologyVersionRequest, _ ...grpc.CallOption) (*foundryv1.OntologyVersionResponse, error) {
	b.read = append(b.read, req.GetId())
	version, ok := b.versions[req.GetId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "version not found")
	}
	return &foundryv1.OntologyVersionResponse{Version: version}, nil
}

// bundle is an agent bundle whose draft holds items, each a TOML snippet by item id.
func bundle(t *testing.T, id, ontologyTOML string, items map[string]string) *foundryv1.OntologyVersion {
	t.Helper()
	drafted := map[string]any{}
	for item, snippet := range items {
		drafted[item] = map[string]any{"toml": snippet, "rationale": "Asked for in the goal."}
	}
	definition, err := structpb.NewStruct(map[string]any{
		"format": "agent_bundle", "status": "done", "ontology_toml": ontologyTOML, "mappings_toml": "",
		"draft": map[string]any{"ontology_toml": ontologyTOML, "draft": map[string]any{"rev": 2.0, "items": drafted}},
	})
	require.NoError(t, err)
	return &foundryv1.OntologyVersion{Id: id, Version: 1, Format: foundryv1.OntologyFormat_ONTOLOGY_FORMAT_AGENT_BUNDLE, Definition: definition}
}

var contextArguments = map[string]any{"workspace_id": "workspace-1", "ontology_id": "ontology-1"}

// What an agent asks is answered from the current load, so get_context describes the version that
// load holds, even when a newer one is current, and suggests the questions the Analyst does: the
// draft's competency questions in their ids' natural order, each once, the first four.
func TestGetContextDescribesTheCurrentLoadsVersion(t *testing.T) {
	loaded := bundle(t, "version-1", "[types.Account]\ndescription = \"A customer.\"\n", map[string]string{
		"cq.CQ10":   "[cq.CQ10]\nquestion = \"Which invoices are overdue?\"\n",
		"cq.CQ2":    "[cq.CQ2]\nquestion = \"  Who are each account's people?  \"\npath = [\"Account\"]\n",
		"cq.CQ1":    "[cq.CQ1]\nquestion = \"Which plan does each account have?\"\n",
		"cq.CQ3":    "[cq.CQ3]\nquestion = \"Which plan does each account have?\"\n",
		"cq.CQ4":    "[cq.CQ4]\nquestion = \"   \"\n",
		"cq.CQ5":    "[cq.CQ5\nquestion = \"Not TOML\"\n",
		"cq.CQ6":    "[cq.CQ6]\nquestion = \"When does each term end?\"\n",
		"cq.CQ7":    "[cq.CQ7]\nquestion = \"Past the first four.\"\n",
		"type.Plan": "[types.Plan]\ndescription = \"A tier.\"\n",
	})
	backend := &contextBackend{
		ontology: &foundryv1.FoundryOntology{
			Id: "ontology-1", Name: "Accounts", CurrentVersionId: "version-2", CurrentLoad: &foundryv1.FoundryOntologyCurrentLoad{Id: "load-1", Version: "foundry-v1"},
		},
		loads:    map[string]*foundryv1.FoundryLoad{"load-1": {Id: "load-1", VersionId: "version-1", Status: "ready", Current: true}},
		versions: map[string]*foundryv1.OntologyVersion{"version-1": loaded, "version-2": bundle(t, "version-2", "[types.Deal]\n", nil)},
	}

	out := called(t, &Service{Foundry: backend}, toolFoundryGetContext, contextArguments)
	assert.Equal(t, "untrusted", out["content_trust"])
	assert.Equal(t, "Accounts", out["ontology"].(map[string]any)["name"])
	assert.Equal(t, "version-1", out["version"].(map[string]any)["id"])
	assert.Equal(t, "agent_bundle", out["version"].(map[string]any)["format"])
	assert.NotContains(t, out["version"], "definition", "the schema is ontology_toml, once")
	assert.Equal(t, "[types.Account]\ndescription = \"A customer.\"\n", out["ontology_toml"])
	assert.Equal(t, []any{
		"Which plan does each account have?", "Who are each account's people?", "When does each term end?", "Which invoices are overdue?",
	}, out["example_questions"])
	assert.NotContains(t, mustJSON(t, out), "Asked for in the goal", "the rest of the draft stays out")
	assert.Equal(t, []string{"version-1"}, backend.read)

	// Nothing loaded yet: the current version, which a load would hold.
	backend.ontology.CurrentLoad, backend.read = nil, nil
	out = called(t, &Service{Foundry: backend}, toolFoundryGetContext, contextArguments)
	assert.Equal(t, "version-2", out["version"].(map[string]any)["id"])
	assert.Equal(t, "[types.Deal]\n", out["ontology_toml"])
	assert.Equal(t, []any{}, out["example_questions"])

	// No version at all: the ontology alone.
	backend.ontology.CurrentVersionId, backend.read = "", nil
	out = called(t, &Service{Foundry: backend}, toolFoundryGetContext, contextArguments)
	assert.NotContains(t, out, "version")
	assert.NotContains(t, out, "ontology_toml")
	assert.Equal(t, []any{}, out["example_questions"])
	assert.Empty(t, backend.read)
}

// A Definition v1 version has no ontology.toml and no competency questions: its definition is its schema.
func TestGetContextGivesADefinitionV1sDefinition(t *testing.T) {
	definition, err := structpb.NewStruct(map[string]any{"entities": []any{map[string]any{"name": "Customer"}}})
	require.NoError(t, err)
	backend := &contextBackend{
		ontology: &foundryv1.FoundryOntology{Id: "ontology-1", CurrentVersionId: "version-1"},
		versions: map[string]*foundryv1.OntologyVersion{"version-1": {Id: "version-1", Version: 1, Definition: definition}},
	}
	out := called(t, &Service{Foundry: backend}, toolFoundryGetContext, contextArguments)
	assert.Equal(t, "foundry_v1", out["version"].(map[string]any)["format"])
	assert.Equal(t, map[string]any{"entities": []any{map[string]any{"name": "Customer"}}}, out["definition"])
	assert.NotContains(t, out, "ontology_toml")
	assert.Equal(t, []any{}, out["example_questions"])
}

// A grantee reads only the version of the load its grants cover: with nothing loaded, it learns of
// the ontology alone, and no other version is read for it.
func TestGetContextTellsAGranteeWithNoLoadNothingMore(t *testing.T) {
	backend := &contextBackend{
		ontology: &foundryv1.FoundryOntology{Id: "ontology-1", CurrentVersionId: "version-1"},
		versions: map[string]*foundryv1.OntologyVersion{"version-1": bundle(t, "version-1", "[types.Secret]\n", nil)},
	}
	grantee := reader()
	grantee.IsGrantee = func(context.Context) bool { return true }
	result, err := connect(t, &Service{Foundry: backend}, grantee).CallTool(context.Background(), &mcp.CallToolParams{Name: toolFoundryGetContext, Arguments: contextArguments})
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	out := result.StructuredContent.(map[string]any)
	assert.NotContains(t, out, "version")
	assert.NotContains(t, out, "ontology_toml")
	assert.Empty(t, backend.read)
}

// A schema past get_version's cap would crowd out the agent's context: it is refused with where to open it.
func TestGetContextRefusesASchemaPastTheCap(t *testing.T) {
	backend := &contextBackend{
		ontology: &foundryv1.FoundryOntology{Id: "ontology-1", CurrentVersionId: "version-1"},
		versions: map[string]*foundryv1.OntologyVersion{"version-1": bundle(t, "version-1", strings.Repeat("#", maxFoundryDefinitionBytes+1), nil)},
	}
	result, err := connect(t, &Service{Foundry: backend, FoundryURL: "https://foundry.example"}, reader()).
		CallTool(context.Background(), &mcp.CallToolParams{Name: toolFoundryGetContext, Arguments: contextArguments})
	require.NoError(t, err)
	require.True(t, result.IsError)
	text := result.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, "past the 256 KiB this tool returns")
	assert.Contains(t, text, "https://foundry.example/ontology/ontology-1/versions/version-1")
}

func TestNaturalCompareOrdersDigitsByValue(t *testing.T) {
	ids := []string{"CQ10", "CQ2", "CQ1", "Q1", "CQ01", "CQ", "CQ2a"}
	slices.SortFunc(ids, naturalCompare)
	assert.Equal(t, []string{"CQ", "CQ01", "CQ1", "CQ2", "CQ2a", "CQ10", "Q1"}, ids)
}
