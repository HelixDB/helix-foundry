package local

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/config"
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

// MCP and HTTP Ask answer for an explicit list of names, so a page can't reach them through a name it
// rebinds to loopback. A wrong token shows the guard passed: it answers 401, after the guard's 403.
func TestIntegrationHandlerAnswersOnlyThisInstallsHosts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth, err := NewAuthenticator(strings.Repeat("t", MinSiteToken), &fakeDirectory{owner: &directory.User{ID: "owner"}}, fakeOntologies{}, logger)
	require.NoError(t, err)
	handler := IntegrationHandler(auth, nil, nil, config.DocumentsConfig{}, "http://foundry.internal:3000", logger)
	for host, allowed := range map[string]bool{
		"localhost": true, "localhost:8081": true, "LOCALHOST:8081": true, "127.0.0.1": true, "127.0.0.1:8081": true,
		"[::1]": true, "[::1]:8081": true,
		// A container's name for this machine, and the host the install's site is configured at.
		"host.docker.internal:8081": true, "foundry.internal:8081": true,
		"attacker.example": false, "attacker.example:8081": false, "localhost.attacker.example": false, "127.0.0.1.nip.io:8081": false,
		"192.168.1.20:8081": false, "[fe80::1]:8081": false, "": false,
		// A DNS search suffix can resolve a single label, so one is no safer than any other name.
		"pod:8081": false, "foundry:8081": false,
	} {
		for _, path := range []string{"/mcp", "/v1/ontologies/ontology-1/questions/question-1"} {
			t.Run(host+path, func(t *testing.T) {
				r := httptest.NewRequest("POST", path, nil)
				r.Host = host
				r.Header.Set("Authorization", "Bearer wrong")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if allowed {
					assert.Equal(t, http.StatusUnauthorized, w.Code)
					return
				}
				assertRefusal(t, w, http.StatusForbidden, "permission_denied")
				assert.Contains(t, w.Body.String(), "invalid host")
			})
		}
	}
}

// assertRefusal checks a refusal is the contract's error, with the request ID it gives in its header.
func assertRefusal(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	assert.Equal(t, wantStatus, w.Code, w.Body.String())
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	assert.Equal(t, wantCode, body.Error.Code)
	assert.NotEmpty(t, body.Error.Message)
	assert.NotEmpty(t, body.Error.RequestID)
	assert.Equal(t, body.Error.RequestID, w.Header().Get("X-Request-ID"))
}

// Every refusal in front of the tools and HTTP Ask is the contract's error. No directory or
// connection stands behind this handler, so a malformed id can only be refused before a lookup.
func TestIntegrationHandlerRefusalsAreTheContractsError(t *testing.T) {
	token := strings.Repeat("t", MinSiteToken)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	owner := &directory.User{ID: "owner"}
	auth, err := NewAuthenticator(token, &fakeDirectory{owner: owner}, fakeOntologies{}, logger)
	require.NoError(t, err)
	handler := IntegrationHandler(auth, nil, nil, config.DocumentsConfig{}, "http://localhost:3000", logger)
	call := func(method, path, bearer string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Host = "localhost:8081"
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	w := call("GET", "/v1/ontologies/ontology-1/questions/question-1", "wrong", nil)
	assertRefusal(t, w, http.StatusUnauthorized, "unauthenticated")
	assert.Equal(t, "Bearer", w.Header().Get("WWW-Authenticate"))
	assertRefusal(t, call("POST", "/mcp", "", nil), http.StatusUnauthorized, "unauthenticated")

	w = call("POST", "/v1/ontologies/ontology-1/ask", token, map[string]string{"Sec-Fetch-Site": "cross-site"})
	assertRefusal(t, w, http.StatusForbidden, "permission_denied")
	assert.Contains(t, w.Body.String(), "cross-origin")

	assertRefusal(t, call("GET", "/v1/ontologies/bad!id/questions/question-1", token, nil), http.StatusBadRequest, "invalid_arguments")
}
