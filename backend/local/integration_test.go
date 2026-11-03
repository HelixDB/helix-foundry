package local

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/local/directory"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Every method the shared tools authorize or call is one an install serves, but
// for API sources' own, which only a host with them reaches (Service.Events). An
// unlisted one fails its tool at every call, so this keeps them in step.
func TestToolsUseOnlyServedMethods(t *testing.T) {
	apiSourcesOnly := map[string]bool{"ListFoundryEventBatches": true, "SendFoundryEvents": true}
	files, err := filepath.Glob("../integration/*.go")
	require.NoError(t, err)
	calls := regexp.MustCompile(`s\.(?:Foundry|Actions)\.([A-Z]\w+)\(`)
	authorizes := regexp.MustCompile(`(?s)AuthorizeFoundry\(ctx, identity, [^\n]*?, (?:foundryUserOnly|QueryRead|QueryWrite|writeGrant|grant),\s*((?:"[A-Z]\w+",?\s*)+)\)`)
	lists := regexp.MustCompile(`methods := \[\]string\{([^}]*)\}`)
	name := regexp.MustCompile(`"([A-Z]\w+)"`)
	seen := map[string]bool{}
	for _, file := range files {
		if filepath.Ext(file) != ".go" || regexp.MustCompile(`_test\.go$`).MatchString(file) {
			continue
		}
		source, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, match := range calls.FindAllSubmatch(source, -1) {
			seen[string(match[1])] = true
		}
		for _, pattern := range []*regexp.Regexp{authorizes, lists} {
			for _, match := range pattern.FindAllSubmatch(source, -1) {
				for _, method := range name.FindAllSubmatch(match[1], -1) {
					seen[string(method[1])] = true
				}
			}
		}
	}
	// The scan has to keep finding them, or it proves nothing.
	for _, method := range []string{"ListConnectors", "GetFoundryQuestion", "CreateFoundryLoad", "ListFoundryDocumentSets"} {
		require.True(t, seen[method], method)
	}
	for method := range seen {
		if apiSourcesOnly[method] {
			continue
		}
		_, served := methods[foundry(method)]
		assert.True(t, served, "%s is called by a tool but not served locally", method)
	}
}

// countingDirectory counts the workspace reads of a check.
type countingDirectory struct {
	*fakeDirectory
	workspaces int
}

func (d *countingDirectory) Workspace(ctx context.Context, id string) (directory.Workspace, error) {
	d.workspaces++
	return d.fakeDirectory.Workspace(ctx, id)
}

func TestAuthorizeToolChecksEachDistinctCheckOnce(t *testing.T) {
	const ws, ontology = "workspace-1", "ontology-1"
	dir := &countingDirectory{fakeDirectory: &fakeDirectory{workspaces: map[string]directory.Workspace{ws: {ID: ws}}}}
	auth, err := NewAuthenticator(string(make([]byte, MinSiteToken)), dir,
		fakeOntologies{{ws, ontology}: &model.FoundryOntology{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	authorize := authorizeTool(auth)
	ctx := context.Background()

	// get_ontology's seven methods are one check of the workspace.
	read := &foundryv1.GetFoundryOntologyRequest{WorkspaceId: ws, OntologyId: ontology}
	require.NoError(t, authorize(ctx, read, integration.QueryRead, "GetFoundryOntology", "GetCurrentOntologyVersion", "ListOntologyVersions",
		"ListFoundryLoads", "ListOntologyGenerations", "ListFoundryDocumentSets", "ListConnectors"))
	assert.Equal(t, 1, dir.workspaces)

	// Different checks each still run: the workspace's, then the ontology's.
	dir.workspaces = 0
	ask := &foundryv1.AskFoundryQuestionRequest{WorkspaceId: ws, OntologyId: "ontology-other"}
	err = authorize(ctx, ask, integration.QueryRead, "ListFoundryDocumentSets", "AskFoundryQuestion", "GetFoundryQuestion")
	assert.Equal(t, codes.NotFound, status.Code(err), "the ontology check runs after the workspace's")

	// A method the install doesn't serve is refused, whatever came before it.
	err = authorize(ctx, read, integration.QueryRead, "ListConnectors", "ListFoundryEventBatches")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "ListFoundryEventBatches")
}
