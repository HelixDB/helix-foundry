package integration

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// everyTool is a user's identity on a host that grants everything, so every
// tool registers.
func everyTool() *Identity {
	return &Identity{User: true, HasPermission: func(Permission) bool { return true }}
}

// connect is a client of a server holding s's tools for identity.
func connect(t *testing.T, s *Service, identity *Identity) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	s.AddTools(server, identity)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	_, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// What an agent reads of the tools names API sources only where it can use them.
func TestToolsNameAPISourcesOnlyOnAHostWithThem(t *testing.T) {
	listed := func(s *Service) string {
		tools, err := connect(t, s, everyTool()).ListTools(context.Background(), nil)
		require.NoError(t, err)
		raw, err := json.Marshal(tools.Tools)
		require.NoError(t, err)
		return string(raw)
	}
	words := []string{"API source", "create_event_source", "event_source", "send_pieces", "send token", "kind events", "or events"}
	local := listed(&Service{FoundryURL: "http://localhost:3000"})
	for _, word := range words {
		assert.NotContains(t, local, word)
	}
	assert.Contains(t, local, "One of 12 operations: create_ontology, sync_connector,")
	assert.Contains(t, local, "generate_ontology: {goal, connector_ids, document_set_ids, no_connectors}")
	assert.Contains(t, local, "or s3.")
	hosted := listed(&Service{FoundryURL: "https://foundry.example", Events: true})
	for _, word := range words {
		assert.Contains(t, hosted, word)
	}
	// Nor a subscription and shared cluster an install hasn't.
	for _, word := range []string{"Helix subscription", "shared cluster"} {
		assert.NotContains(t, local, word)
		assert.Contains(t, hosted, word)
	}
	assert.Contains(t, hosted, "One of 13 operations: create_ontology, create_event_source, sync_connector,")
}

// Clients and MCP directories show a tool's title beside its name: every Foundry tool has one,
// wherever it is offered.
func TestEveryToolHasATitle(t *testing.T) {
	var names []string
	for _, s := range []*Service{
		{FoundryURL: "http://localhost:3000"},
		{FoundryURL: "https://foundry.example", Events: true, SignIn: true},
	} {
		listed, err := connect(t, s, everyTool()).ListTools(context.Background(), nil)
		require.NoError(t, err)
		require.NotEmpty(t, listed.Tools)
		names = names[:0]
		for _, tool := range listed.Tools {
			assert.NotEmpty(t, tool.Title, tool.Name)
			names = append(names, tool.Name)
		}
	}
	// The host with every tool lists each title's tool: none is left over.
	titled := make([]string, 0, len(toolTitles))
	for name := range toolTitles {
		titled = append(titled, name)
	}
	assert.ElementsMatch(t, titled, names)
}

// An agent picks a tool by its description, so the ones it starts from say first what they are
// for: the business's knowledge, to ask, look up and add to.
func TestEntryToolsSayWhatTheyAreFor(t *testing.T) {
	listed, err := connect(t, &Service{FoundryURL: "https://foundry.example", Events: true}, everyTool()).ListTools(context.Background(), nil)
	require.NoError(t, err)
	tools := map[string]string{}
	for _, tool := range listed.Tools {
		tools[tool.Name] = tool.Description
	}
	for name, lead := range map[string]string{
		toolFoundryListOntologies: "Start here to use what a business knows: Helix Foundry holds its customers, accounts, people, deals, documents",
		toolFoundryAsk:            "Answer a question about a business from its Foundry ontology, with every claim citing",
		toolFoundryReadGraph:      "Look up exact records in a business's Foundry ontology:",
		toolFoundrySendPieces:     "Remember something for later in a business's Foundry ontology:",
	} {
		assert.True(t, strings.HasPrefix(tools[name], lead), name)
	}
	// What each did before follows unchanged.
	assert.Contains(t, tools[toolFoundryListOntologies], "List the Foundry ontologies of one workspace, with their current version and load.")
	assert.Contains(t, tools[toolFoundryReadGraph], "Read a load of a Foundry ontology's graph, with its version's rules applied: 1 to 20 reads in one call.")
	assert.Contains(t, tools[toolFoundryAsk], "Ask a question of a Foundry ontology's load in plain language.")
	assert.Contains(t, tools[toolFoundrySendPieces], "Send pieces of data, JSON objects with an id, to a Foundry API source (a connector of kind events).")
	// A send token sees the send tool alone, so its description names no other.
	for name := range tools {
		if name != toolFoundrySendPieces {
			assert.NotContains(t, tools[toolFoundrySendPieces], name)
		}
	}
}

// The default runner answers clients that read only content, not just those
// that read structuredContent.
func TestRunPutsTheOutputInContent(t *testing.T) {
	s := &Service{}
	want := FoundryReadGuideOutput{Notes: "from the tool", MaxReads: 3}
	result, out, err := s.Run(context.Background(), "tool", func(context.Context, *Call) (any, int, error) { return want, 1, nil })
	require.NoError(t, err)
	assert.Equal(t, want, out)
	require.Len(t, result.Content, 2)
	assert.Equal(t, untrustedContentNotice, result.Content[0].(*mcp.TextContent).Text)
	var got FoundryReadGuideOutput
	require.NoError(t, json.Unmarshal([]byte(result.Content[1].(*mcp.TextContent).Text), &got))
	assert.Equal(t, want.Notes, got.Notes)
	assert.Equal(t, want.MaxReads, got.MaxReads)
}

func TestRunGivesAFailureItsStableCode(t *testing.T) {
	s := &Service{}
	_, _, err := s.Run(context.Background(), "tool", func(context.Context, *Call) (any, int, error) { return nil, 0, errors.New("private") })
	require.EqualError(t, err, "internal_error")
	_, _, err = s.Run(context.Background(), "tool", func(context.Context, *Call) (any, int, error) { return make(chan int), 1, nil })
	require.EqualError(t, err, "internal_error", "an output that can't be encoded")
}

// connectorsBackend lists one connector of each sort, and fails the test's
// call if the API source's batches are read.
type connectorsBackend struct {
	foundryv1.FoundryServiceClient
	batches int
}

func (b *connectorsBackend) ListConnectors(context.Context, *foundryv1.ListConnectorsRequest, ...grpc.CallOption) (*foundryv1.ListConnectorsResponse, error) {
	return &foundryv1.ListConnectorsResponse{Connectors: []*foundryv1.Connector{
		{Id: "stripe-1", Kind: "stripe", Name: "Stripe"},
		{Id: "source-1", Kind: "events", Name: "Source", Config: &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Events{Events: &foundryv1.EventsConnectorConfig{}}}},
	}}, nil
}

func (b *connectorsBackend) ListFoundryEventBatches(context.Context, *foundryv1.ListFoundryEventBatchesRequest, ...grpc.CallOption) (*foundryv1.ListFoundryEventBatchesResponse, error) {
	b.batches++
	return &foundryv1.ListFoundryEventBatchesResponse{}, nil
}

// A host without API sources doesn't serve their batches, so listing connectors
// neither asks to call that method nor calls it, whatever the connectors are.
func TestListConnectorsReadsBatchesOnlyOnAHostWithAPISources(t *testing.T) {
	for _, events := range []bool{false, true} {
		b := &connectorsBackend{}
		var authorized []string
		identity := &Identity{Authorize: func(_ context.Context, _ any, _ Grant, methods ...string) error {
			authorized = methods
			return nil
		}}
		out, count, err := (&Service{Foundry: b, Events: events}).ListFoundryConnectors(context.Background(), identity, &Call{}, FoundryWorkspaceInput{WorkspaceID: "workspace-1"})
		require.NoError(t, err)
		assert.Equal(t, 2, count)
		if events {
			assert.Contains(t, authorized, "ListFoundryEventBatches")
			assert.Equal(t, 1, b.batches)
			assert.NotNil(t, out.Connectors[1].EventSource)
			continue
		}
		assert.NotContains(t, authorized, "ListFoundryEventBatches")
		assert.Zero(t, b.batches)
		assert.Nil(t, out.Connectors[1].EventSource)
	}
}

// What the backend relayed keeps a number no JSON reader would, as a string of it.
func TestUntrustedJSONKeepsNumbersExact(t *testing.T) {
	got := UntrustedJSON(`{"big":9007199254740993,"low":-9007199254740993,"safe":9007199254740991,"rate":0.5,"huge":1e400,"list":[12345678901234567890]}`)
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, `{"big":"9007199254740993","low":"-9007199254740993","safe":9007199254740991,"rate":0.5,"huge":"1e400","list":["12345678901234567890"]}`, string(encoded))
	assert.Equal(t, map[string]string{"opaque_response": `{"a":1} trailing`}, UntrustedJSON(`{"a":1} trailing`))
	assert.Equal(t, map[string]string{"opaque_response": "not json"}, UntrustedJSON("not json"))
}

type graphBackend struct {
	foundryv1.FoundryServiceClient
	reads string
}

func (b *graphBackend) GetFoundryLoad(context.Context, *foundryv1.GetFoundryLoadRequest, ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error) {
	return &foundryv1.FoundryLoadResponse{Load: &foundryv1.FoundryLoad{Status: "ready"}}, nil
}

func (b *graphBackend) ReadFoundryGraph(_ context.Context, r *foundryv1.ReadFoundryGraphRequest, _ ...grpc.CallOption) (*foundryv1.ReadFoundryGraphResponse, error) {
	b.reads = r.GetReadsJson()
	return &foundryv1.ReadFoundryGraphResponse{ResultJson: `[{"count":9007199254740993}]`}, nil
}

// The SDK reads a call's arguments and a tool's output through float64s, which round a large
// integer. A read's filter reaches the backend as written, and a result reaches the client exact
// in content and structuredContent alike.
func TestReadGraphKeepsLargeIntegersExactBothWays(t *testing.T) {
	b := &graphBackend{}
	identity := everyTool()
	identity.Authorize = func(context.Context, any, Grant, ...string) error { return nil }
	session := connect(t, &Service{Foundry: b}, identity)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: toolFoundryReadGraph, Arguments: json.RawMessage(
		`{"workspace_id":"workspace-1","ontology_id":"ontology-1","load_id":"load-1","reads":[{"query":{"value":9007199254740993}}]}`)})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Content)
	assert.JSONEq(t, `[{"query":{"value":9007199254740993}}]`, b.reads)
	assert.Contains(t, b.reads, "9007199254740993")

	text := result.Content[len(result.Content)-1].(*mcp.TextContent).Text
	assert.Contains(t, text, `"count":"9007199254740993"`)
	structured, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	assert.Contains(t, string(structured), `"count":"9007199254740993"`)
}

type documentsBackend struct {
	foundryv1.FoundryServiceClient
	readBy   string
	setCalls int
}

func (b *documentsBackend) ListFoundryOntologyDocuments(context.Context, *foundryv1.ListFoundryOntologyDocumentsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryDocumentsResponse, error) {
	return &foundryv1.ListFoundryDocumentsResponse{}, nil
}

func (b *documentsBackend) ListFoundryDocuments(context.Context, *foundryv1.ListFoundryDocumentsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryDocumentsResponse, error) {
	b.setCalls++
	return &foundryv1.ListFoundryDocumentsResponse{}, nil
}

func (b *documentsBackend) ListFoundryDocumentSets(context.Context, *foundryv1.ListFoundryDocumentSetsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryDocumentSetsResponse, error) {
	return &foundryv1.ListFoundryDocumentSetsResponse{Sets: []*foundryv1.FoundryDocumentSet{
		{Id: "set-1", ReadBy: []*foundryv1.FoundryDocumentSetReader{{OntologyId: b.readBy}}},
	}}, nil
}

// A set's documents are authorized as the call that lists them, and a key reaches only a set the
// ontology its grant is on reads.
func TestListDocumentsOfASet(t *testing.T) {
	list := func(identity *Identity, b *documentsBackend, set string) ([]string, error) {
		var authorized []string
		identity.Authorize = func(_ context.Context, _ any, _ Grant, methods ...string) error {
			authorized = methods
			return nil
		}
		_, _, err := (&Service{Foundry: b}).ListFoundryDocuments(context.Background(), identity, &Call{}, ListFoundryDocumentsInput{
			OntologyInput: OntologyInput{WorkspaceID: "workspace-1", OntologyID: "ontology-a"}, SetID: set,
		})
		return authorized, err
	}

	authorized, err := list(&Identity{}, &documentsBackend{}, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"ListFoundryOntologyDocuments"}, authorized)

	// A user: any set of the Workspace, as on the website, on the call's own check.
	user := &documentsBackend{readBy: "ontology-b"}
	authorized, err = list(&Identity{User: true}, user, "set-1")
	require.NoError(t, err)
	assert.Contains(t, authorized, "ListFoundryDocuments")
	assert.Equal(t, 1, user.setCalls)

	// A key: only a set the ontology reads.
	other := &documentsBackend{readBy: "ontology-b"}
	_, err = list(&Identity{ServiceKey: true}, other, "set-1")
	require.ErrorIs(t, err, errCodeNotFound)
	assert.Zero(t, other.setCalls)
	_, err = list(&Identity{ServiceKey: true}, &documentsBackend{readBy: "ontology-a"}, "another-set")
	require.ErrorIs(t, err, errCodeNotFound)

	reads := &documentsBackend{readBy: "ontology-a"}
	_, err = list(&Identity{ServiceKey: true}, reads, "set-1")
	require.NoError(t, err)
	assert.Equal(t, 1, reads.setCalls)
}

type createDirectory struct {
	Directory
	authorized int
}

func (d *createDirectory) AuthorizeCreate(context.Context, string, string) error {
	d.authorized++
	return nil
}

// A new database is a user's to make, whatever the host's check reads.
func TestCreateOntologyOnANewDatabaseIsUserOnly(t *testing.T) {
	all := func(Permission) bool { return true }
	input := FoundryOperationInput{Operation: "create_ontology", Target: "workspace:workspace-1", Payload: map[string]any{
		"name": "Ontology", "database": map[string]any{"name": "Database", "slug": "database", "project_name": "Project"},
	}}
	for name, identity := range map[string]*Identity{
		"a service key":         {ServiceKey: true, HasPermission: all},
		"an agent registration": {Agent: true, HasPermission: all},
		"no user":               {HasPermission: all},
		"a user who can't manage ontologies": {User: true, HasPermission: func(p Permission) bool {
			return p != PermissionWorkspaceOntologyManage
		}},
	} {
		t.Run(name, func(t *testing.T) {
			directory := &createDirectory{}
			_, err := (&Service{Directory: directory}).ValidateFoundryOperation(context.Background(), identity, input)
			require.ErrorIs(t, err, errCodeNotFound)
			assert.Zero(t, directory.authorized)
		})
	}
	directory := &createDirectory{}
	operation, err := (&Service{Directory: directory}).ValidateFoundryOperation(context.Background(), &Identity{User: true, HasPermission: all}, input)
	require.NoError(t, err)
	assert.IsType(t, NewDatabaseOntologyOperation{}, operation)
	assert.Equal(t, 1, directory.authorized)
}

type tenantDirectory struct {
	Directory
	err error
}

func (d tenantDirectory) TenantWorkspace(context.Context, string) (string, error) { return "", d.err }

// Told a database doesn't exist, an agent may make another, so a failed lookup must not say so.
func TestCreateOntologyTellsAMissingDatabaseFromAFailedLookup(t *testing.T) {
	input := FoundryOperationInput{Operation: "create_ontology", Target: "tenant:tenant-1", Payload: map[string]any{"name": "Ontology"}}
	identity := &Identity{User: true, HasPermission: func(Permission) bool { return true }}
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"it doesn't exist", status.Error(codes.NotFound, "database not found"), errCodeNotFound},
		{"the store failed", errors.New("connection reset"), errCodeBackendUnavailable},
		{"the host's backend is down", status.Error(codes.Unavailable, "down"), errCodeBackendUnavailable},
		{"the call was cut", context.DeadlineExceeded, errCodeBackendUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&Service{Directory: tenantDirectory{err: tc.err}}).ValidateFoundryOperation(context.Background(), identity, input)
			assert.ErrorIs(t, err, tc.want)
		})
	}
}

// ontologyBackend answers get_ontology's reads once all of them are in flight, so a test fails
// unless they run at once.
type ontologyBackend struct {
	foundryv1.FoundryServiceClient
	missing bool
	mu      sync.Mutex
	calls   int
	all     chan struct{}
}

func (b *ontologyBackend) arrive(ctx context.Context) error {
	b.mu.Lock()
	b.calls++
	if b.calls == 5 {
		close(b.all)
	}
	b.mu.Unlock()
	select {
	case <-b.all:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return status.Error(codes.Internal, "the reads ran one at a time")
	}
}

func (b *ontologyBackend) GetFoundryOntology(context.Context, *foundryv1.GetFoundryOntologyRequest, ...grpc.CallOption) (*foundryv1.FoundryOntologyResponse, error) {
	if b.missing {
		return nil, status.Error(codes.NotFound, "ontology not found")
	}
	return &foundryv1.FoundryOntologyResponse{Ontology: &foundryv1.FoundryOntology{Id: "ontology-1"}}, nil
}

func (b *ontologyBackend) GetCurrentOntologyVersion(ctx context.Context, _ *foundryv1.GetCurrentOntologyVersionRequest, _ ...grpc.CallOption) (*foundryv1.OntologyVersionResponse, error) {
	return &foundryv1.OntologyVersionResponse{}, b.arrive(ctx)
}

func (b *ontologyBackend) ListOntologyVersions(ctx context.Context, _ *foundryv1.ListOntologyVersionsRequest, _ ...grpc.CallOption) (*foundryv1.ListOntologyVersionsResponse, error) {
	return &foundryv1.ListOntologyVersionsResponse{}, b.arrive(ctx)
}

func (b *ontologyBackend) ListFoundryLoads(ctx context.Context, _ *foundryv1.ListFoundryLoadsRequest, _ ...grpc.CallOption) (*foundryv1.ListFoundryLoadsResponse, error) {
	return &foundryv1.ListFoundryLoadsResponse{}, b.arrive(ctx)
}

func (b *ontologyBackend) ListOntologyGenerations(ctx context.Context, _ *foundryv1.ListOntologyGenerationsRequest, _ ...grpc.CallOption) (*foundryv1.ListOntologyGenerationsResponse, error) {
	return &foundryv1.ListOntologyGenerationsResponse{}, b.arrive(ctx)
}

func (b *ontologyBackend) ListFoundryDocumentSets(ctx context.Context, _ *foundryv1.ListFoundryDocumentSetsRequest, _ ...grpc.CallOption) (*foundryv1.ListFoundryDocumentSetsResponse, error) {
	return &foundryv1.ListFoundryDocumentSetsResponse{}, b.arrive(ctx)
}

func TestGetOntologyReadsAtOnceOnceItExists(t *testing.T) {
	identity := &Identity{
		Authorize: func(context.Context, any, Grant, ...string) error { return nil },
		Allows:    func(string, string, Grant) bool { return true },
	}
	input := OntologyInput{WorkspaceID: "workspace-1", OntologyID: "ontology-1"}
	b := &ontologyBackend{all: make(chan struct{})}
	out, _, err := (&Service{Foundry: b}).GetFoundryOntology(context.Background(), identity, &Call{}, input)
	require.NoError(t, err)
	assert.Equal(t, "ontology-1", out.Ontology.ID)
	assert.Equal(t, 5, b.calls)

	missing := &ontologyBackend{missing: true, all: make(chan struct{})}
	_, _, err = (&Service{Foundry: missing}).GetFoundryOntology(context.Background(), identity, &Call{}, input)
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
	assert.Zero(t, missing.calls, "an ontology that isn't there costs one read")
}

// A host's own tool runs as Foundry's do: it logs its call, and says its output is untrusted.
func TestAddReadToolRunsUnderRun(t *testing.T) {
	var logs bytes.Buffer
	s := &Service{Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	type input struct {
		Name string `json:"name"`
	}
	type output struct {
		Hello string `json:"hello"`
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	AddReadTool(s, server, everyTool(), "host_tool", "Host tool", "A host's tool.", func(_ context.Context, _ *Identity, call *Call, in input) (output, int, error) {
		call.ResourceKind, call.ResourceID = "thing", in.Name
		if in.Name == "bad" {
			return output{}, 0, status.Error(codes.Internal, "private")
		}
		return output{Hello: in.Name}, 1, nil
	})
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	_, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer func() { _ = session.Close() }()

	listed, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, 1)
	assert.True(t, listed.Tools[0].Annotations.ReadOnlyHint)
	assert.Equal(t, "Host tool", listed.Tools[0].Title)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "host_tool", Arguments: map[string]any{"name": "ada"}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Len(t, result.Content, 2)
	assert.Equal(t, untrustedContentNotice, result.Content[0].(*mcp.TextContent).Text)
	assert.JSONEq(t, `{"hello":"ada"}`, result.Content[1].(*mcp.TextContent).Text)
	assert.Contains(t, logs.String(), "Foundry tool call")
	assert.Contains(t, logs.String(), "tool=host_tool")
	assert.Contains(t, logs.String(), "resource_id=ada")

	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "host_tool", Arguments: map[string]any{"name": "bad"}})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.NotContains(t, result.Content[0].(*mcp.TextContent).Text, "private")
	assert.Contains(t, logs.String(), "error_code=internal_error")
}

// grantBackend answers as Foundry does for a key its own permissions refuse: one list for what
// it may read through its own grants, another for what source grants give it.
type grantBackend struct {
	FoundryBackendAPI
	granted      []*foundryv1.FoundryOntology
	grantedErr   error
	currentErr   error
	loadRequests int
}

type grantCtx struct{}

func (b *grantBackend) ListFoundryOntologies(ctx context.Context, _ *foundryv1.ListFoundryOntologiesRequest, _ ...grpc.CallOption) (*foundryv1.ListFoundryOntologiesResponse, error) {
	if ctx.Value(grantCtx{}) == nil {
		return &foundryv1.ListFoundryOntologiesResponse{Ontologies: []*foundryv1.FoundryOntology{{Id: "01JZZZZZZZZZZZZZZZZZZZZZZA", ProjectId: "own"}}}, nil
	}
	return &foundryv1.ListFoundryOntologiesResponse{Ontologies: b.granted}, b.grantedErr
}

func (b *grantBackend) GetFoundryOntology(context.Context, *foundryv1.GetFoundryOntologyRequest, ...grpc.CallOption) (*foundryv1.FoundryOntologyResponse, error) {
	return &foundryv1.FoundryOntologyResponse{Ontology: &foundryv1.FoundryOntology{PartialAccess: true, CurrentLoad: &foundryv1.FoundryOntologyCurrentLoad{Id: "load"}}}, nil
}

func (b *grantBackend) GetFoundryLoad(context.Context, *foundryv1.GetFoundryLoadRequest, ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error) {
	b.loadRequests++
	return &foundryv1.FoundryLoadResponse{Load: &foundryv1.FoundryLoad{Id: "load", VersionId: "version"}}, nil
}

func (b *grantBackend) GetOntologyVersion(context.Context, *foundryv1.GetOntologyVersionRequest, ...grpc.CallOption) (*foundryv1.OntologyVersionResponse, error) {
	return &foundryv1.OntologyVersionResponse{Version: &foundryv1.OntologyVersion{Id: "version"}}, nil
}

func (b *grantBackend) GetCurrentOntologyVersion(context.Context, *foundryv1.GetCurrentOntologyVersionRequest, ...grpc.CallOption) (*foundryv1.OntologyVersionResponse, error) {
	return nil, b.currentErr
}

func (b *grantBackend) ListOntologyVersions(context.Context, *foundryv1.ListOntologyVersionsRequest, ...grpc.CallOption) (*foundryv1.ListOntologyVersionsResponse, error) {
	return &foundryv1.ListOntologyVersionsResponse{}, nil
}

func (b *grantBackend) ListFoundryLoads(context.Context, *foundryv1.ListFoundryLoadsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryLoadsResponse, error) {
	return &foundryv1.ListFoundryLoadsResponse{}, nil
}

func (b *grantBackend) ListOntologyGenerations(context.Context, *foundryv1.ListOntologyGenerationsRequest, ...grpc.CallOption) (*foundryv1.ListOntologyGenerationsResponse, error) {
	return &foundryv1.ListOntologyGenerationsResponse{}, nil
}

func (b *grantBackend) ListFoundryDocumentSets(context.Context, *foundryv1.ListFoundryDocumentSetsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryDocumentSetsResponse, error) {
	return &foundryv1.ListFoundryDocumentSetsResponse{}, nil
}

// grantKey is a key with no permissions of its own, on a host with grants.
func grantKey(permissions ...Permission) *Identity {
	return &Identity{
		ServiceKey: true, WorkspaceID: "01JZZZZZZZZZZZZZZZZZZZZZZW",
		HasPermission: func(p Permission) bool { return slices.Contains(permissions, p) },
		Allows:        func(_, project string, _ Grant) bool { return project == "own" },
		AsGrantee:     func(ctx context.Context) context.Context { return context.WithValue(ctx, grantCtx{}, true) },
		IsGrantee:     func(context.Context) bool { return true },
		Authorize:     func(context.Context, any, Grant, ...string) error { return nil },
	}
}

// Grants stand in for database.query.read in what a key discovers, and nothing else.
func TestSourceGrantsListTheReadToolsOnly(t *testing.T) {
	tools := func(identity *Identity) []string {
		listed, err := connect(t, &Service{}, identity).ListTools(context.Background(), nil)
		require.NoError(t, err)
		names := make([]string, 0, len(listed.Tools))
		for _, tool := range listed.Tools {
			names = append(names, tool.Name)
		}
		return names
	}
	bare := grantKey()
	assert.NotContains(t, tools(bare), toolFoundryListOntologies)
	assert.NotContains(t, tools(bare), toolFoundryReadGraph)
	granted := grantKey()
	granted.FoundryGrants = true
	names := tools(granted)
	assert.Contains(t, names, toolFoundryListOntologies)
	assert.Contains(t, names, toolFoundryReadGraph)
	assert.Contains(t, names, toolFoundryAsk)
	assert.NotContains(t, names, toolFoundryConnectorLink)
}

func TestAKeyListsWhatSourceGrantsGiveIt(t *testing.T) {
	workspace := "01JZZZZZZZZZZZZZZZZZZZZZZW"
	own, other := "01JZZZZZZZZZZZZZZZZZZZZZZA", "01JZZZZZZZZZZZZZZZZZZZZZZB"
	ids := func(out ListFoundryOntologiesOutput) []string {
		got := make([]string, 0, len(out.Ontologies))
		for _, o := range out.Ontologies {
			got = append(got, o.ID)
		}
		return got
	}
	list := func(b *grantBackend, identity *Identity, page string) (ListFoundryOntologiesOutput, error) {
		out, _, err := (&Service{Foundry: b}).ListFoundryOntologies(context.Background(), identity, &Call{},
			ListFoundryOntologiesInput{WorkspaceID: workspace, PageToken: page})
		return out, err
	}
	b := &grantBackend{granted: []*foundryv1.FoundryOntology{{Id: own, PartialAccess: true}, {Id: other, PartialAccess: true}}}
	out, err := list(b, grantKey(), "")
	require.NoError(t, err)
	assert.Equal(t, []string{own, other}, ids(out), "what it holds both ways shows once")
	assert.False(t, out.Ontologies[0].PartialAccess)
	assert.True(t, out.Ontologies[1].PartialAccess)
	out, err = list(b, grantKey(), "next")
	require.NoError(t, err)
	assert.Equal(t, []string{own}, ids(out), "grants list on the first page only")

	// Foundry can't say what grants give: a key with its own grant keeps its list, and one
	// reading only through grants gets the failure.
	failing := &grantBackend{grantedErr: status.Error(codes.Unavailable, "down")}
	out, err = list(failing, grantKey(PermissionDatabaseQueryRead), "")
	require.NoError(t, err)
	assert.Equal(t, []string{own}, ids(out))
	_, err = list(failing, grantKey(), "")
	require.Error(t, err)

	// A host without grants lists as it did.
	plain := grantKey()
	plain.AsGrantee = nil
	out, err = list(b, plain, "")
	require.NoError(t, err)
	assert.Equal(t, []string{own}, ids(out))
}

func TestAGranteeGetsTheOntologyItsGrantsCover(t *testing.T) {
	input := OntologyInput{WorkspaceID: "01JZZZZZZZZZZZZZZZZZZZZZZW", OntologyID: "01JZZZZZZZZZZZZZZZZZZZZZZA"}
	get := func(b *grantBackend, identity *Identity) (GetFoundryOntologyOutput, error) {
		out, _, err := (&Service{Foundry: b}).GetFoundryOntology(context.Background(), identity, &Call{}, input)
		return out, err
	}
	denied := errors.New("denied")
	// Refused the owner's reads, admitted to the grantee's: only the load it holds, and its version.
	onlyGrantee := func(_ context.Context, _ any, _ Grant, methods ...string) error {
		if slices.Contains(methods, "ListOntologyVersions") {
			return denied
		}
		return nil
	}
	key := grantKey()
	key.Authorize = onlyGrantee
	b := &grantBackend{}
	out, err := get(b, key)
	require.NoError(t, err)
	assert.True(t, out.Ontology.PartialAccess)
	require.Len(t, out.Loads, 1)
	assert.Equal(t, "load", out.Loads[0].ID)
	require.NotNil(t, out.CurrentVersion)
	assert.Empty(t, out.Versions)
	assert.Empty(t, out.Generations)
	assert.Equal(t, 1, b.loadRequests)

	// Someone neither admits: the first refusal stands.
	key.IsGrantee = func(context.Context) bool { return false }
	_, err = get(&grantBackend{}, key)
	require.ErrorIs(t, err, denied)
	key.IsGrantee = nil
	_, err = get(&grantBackend{}, key)
	require.ErrorIs(t, err, denied)

	// A restricted member passes the host's checks; Foundry refuses what grants don't reach.
	restricted := grantKey()
	out, err = get(&grantBackend{currentErr: status.Error(codes.PermissionDenied, "no")}, restricted)
	require.NoError(t, err)
	assert.Len(t, out.Loads, 1)

	// Only a host with grants reads that refusal so.
	plain := grantKey()
	plain.IsGrantee = nil
	_, err = get(&grantBackend{currentErr: status.Error(codes.PermissionDenied, "no")}, plain)
	require.Error(t, err)
}

// reader is a user's identity on a host that grants every read.
func reader() *Identity {
	return &Identity{
		User: true, HasPermission: func(Permission) bool { return true },
		Authorize: func(context.Context, any, Grant, ...string) error { return nil },
		Allows:    func(string, string, Grant) bool { return true },
	}
}

// called is a tool's structured output, as a client reads it: the SDK checked it against the
// tool's schema on the way.
func called(t *testing.T, s *Service, name string, arguments map[string]any) map[string]any {
	t.Helper()
	result, err := connect(t, s, reader()).CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// loadBackend answers get_load with one load.
type loadBackend struct {
	foundryv1.FoundryServiceClient
	load *foundryv1.FoundryLoad
}

func (b *loadBackend) GetFoundryLoad(context.Context, *foundryv1.GetFoundryLoadRequest, ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error) {
	return &foundryv1.FoundryLoadResponse{Load: b.load}, nil
}

// A load's jobs count every document as done. What came of them is in documents: twelve sets
// of which the version reads two, so most of what was uploaded added nothing to the graph, and
// the caller is told how much, in all and set by set.
func TestGetLoadCountsWhatItMadeOfItsDocuments(t *testing.T) {
	load := &foundryv1.FoundryLoad{Id: "load-1", VersionId: "version-1", Status: "ready", Jobs: []*foundryv1.FoundryGraphJob{
		{Id: "load-1", Kind: "load", Status: "succeeded", Progress: &foundryv1.FoundryGraphJobProgress{DocumentsDone: 121, DocumentsTotal: 121}},
	}}
	load.DocumentSets = append(load.DocumentSets,
		&foundryv1.FoundryLoadDocumentSet{SetId: "set-01", Routed: true, Documents: 10, Read: 8, Duplicate: 1, Failed: 1, Reviews: 30},
		&foundryv1.FoundryLoadDocumentSet{SetId: "set-02", Routed: true, Documents: 12, Read: 10, Waiting: 2, Facts: 3, Reviews: 37, Unresolved: 5})
	for _, set := range []string{"03", "04", "05", "06", "07", "08", "09", "10", "11"} {
		load.DocumentSets = append(load.DocumentSets, &foundryv1.FoundryLoadDocumentSet{SetId: "set-" + set, Documents: 10, NotRead: 10})
	}
	load.DocumentSets = append(load.DocumentSets, &foundryv1.FoundryLoadDocumentSet{SetId: "set-12", Documents: 9, NotRead: 9})
	arguments := map[string]any{"workspace_id": "workspace-1", "ontology_id": "ontology-1", "load_id": "load-1"}

	out := called(t, &Service{Foundry: &loadBackend{load: load}}, toolFoundryGetLoad, arguments)
	documents, ok := out["documents"].(map[string]any)
	require.True(t, ok, "%v", out)
	for field, want := range map[string]float64{
		"total": 121, "read": 18, "not_read_by_version": 99, "duplicate": 1, "failed": 1, "waiting": 2,
		"facts_written": 3, "left_for_review": 67, "names_unresolved": 5,
	} {
		assert.Equal(t, want, documents[field], field)
	}
	sets := documents["sets"].([]any)
	require.Len(t, sets, 12)
	// Each set says how every one of its documents ended, so "12 documents, 10 read" explains itself.
	assert.Equal(t, map[string]any{
		"set_id": "set-01", "read_by_version": true, "documents": 10.0, "read": 8.0, "not_read_by_version": 0.0, "duplicate": 1.0,
		"failed": 1.0, "waiting": 0.0, "facts_written": 0.0, "left_for_review": 30.0, "names_unresolved": 0.0,
	}, sets[0])
	assert.Equal(t, map[string]any{
		"set_id": "set-02", "read_by_version": true, "documents": 12.0, "read": 10.0, "not_read_by_version": 0.0, "duplicate": 0.0,
		"failed": 0.0, "waiting": 2.0, "facts_written": 3.0, "left_for_review": 37.0, "names_unresolved": 5.0,
	}, sets[1])
	assert.Equal(t, map[string]any{
		"set_id": "set-12", "read_by_version": false, "documents": 9.0, "read": 0.0, "not_read_by_version": 9.0, "duplicate": 0.0,
		"failed": 0.0, "waiting": 0.0, "facts_written": 0.0, "left_for_review": 0.0, "names_unresolved": 0.0,
	}, sets[11])
	for _, set := range sets {
		set := set.(map[string]any)
		assert.Equal(t, set["documents"], set["read"].(float64)+set["not_read_by_version"].(float64)+set["duplicate"].(float64)+
			set["failed"].(float64)+set["waiting"].(float64), "%v adds up", set["set_id"])
	}
	unread := 0
	for _, set := range sets {
		if !set.(map[string]any)["read_by_version"].(bool) {
			unread++
		}
	}
	assert.Equal(t, 10, unread, "the sets the version doesn't read are named")

	// A load that holds no documents, or one read in part, says nothing of them.
	out = called(t, &Service{Foundry: &loadBackend{load: &foundryv1.FoundryLoad{Id: "load-1", Status: "ready"}}}, toolFoundryGetLoad, arguments)
	assert.NotContains(t, out, "documents")

	// What the tool says of itself tells an agent to report them, and what each number is: the
	// reviews are counted as the documents were written, not as they stand, the unresolved names
	// are among them, a fact counts once a document, and absence is not "no documents".
	tools, err := connect(t, &Service{}, reader()).ListTools(context.Background(), nil)
	require.NoError(t, err)
	for _, tool := range tools.Tools {
		if tool.Name != toolFoundryGetLoad {
			continue
		}
		assert.Contains(t, tool.Description, "not_read_by_version")
		assert.Contains(t, tool.Description, "not the number still open")
		schema := mustJSON(t, tool.OutputSchema)
		for _, says := range []string{
			"This is not the number of open reviews", "stays counted until the load writes the decision",
			"overlaps left_for_review", "a fact two documents state counts twice", "absence does not say nothing was left unread",
		} {
			assert.Contains(t, schema, says)
		}
	}
}

// versionBackend answers get_version with one version, and with its generation (generationErr
// when that can't be read), counting how often the generation was read.
type versionBackend struct {
	foundryv1.FoundryServiceClient
	version       *foundryv1.OntologyVersion
	generation    *foundryv1.OntologyGeneration
	generationErr error
	generations   int
}

func (b *versionBackend) GetOntologyVersion(context.Context, *foundryv1.GetOntologyVersionRequest, ...grpc.CallOption) (*foundryv1.OntologyVersionResponse, error) {
	return &foundryv1.OntologyVersionResponse{Version: b.version}, nil
}

func (b *versionBackend) GetOntologyGeneration(_ context.Context, req *foundryv1.GetOntologyGenerationRequest, _ ...grpc.CallOption) (*foundryv1.OntologyGenerationResponse, error) {
	b.generations++
	if b.generationErr != nil || req.GetId() != b.generation.GetId() {
		return nil, cmp.Or(b.generationErr, status.Error(codes.NotFound, "generation not found"))
	}
	return &foundryv1.OntologyGenerationResponse{Generation: b.generation}, nil
}

// A draft given three sets that routes one of them reads one: the caller sees which, what the
// draft says it left out and why, and its open questions, without the rest of its working draft.
func TestGetVersionSaysWhatItLeavesUnread(t *testing.T) {
	definition := func(mappings string, draft map[string]any) *structpb.Struct {
		s, err := structpb.NewStruct(map[string]any{
			"format": "agent_bundle", "status": "done", "ontology_toml": "", "mappings_toml": mappings, "summary": "Drafted memos.",
			"questions": []any{map[string]any{"id": "Q1", "text": "Should the minutes be read too?", "default": "Yes", "blocking": false}},
			"draft":     map[string]any{"ontology_toml": "", "mappings_toml": mappings, "draft": draft},
		})
		require.NoError(t, err)
		return s
	}
	version := &foundryv1.OntologyVersion{
		Id: "version-1", Version: 2, Format: foundryv1.OntologyFormat_ONTOLOGY_FORMAT_AGENT_BUNDLE, GenerationId: "generation-1",
		DocumentSetIds: []string{"set-memos", "set-minutes", "set-calls"},
		Definition: definition("[extractions.memos]\ndocuments = [\"set-memos\"]\n", map[string]any{
			"rev":   3.0,
			"items": map[string]any{"type.Memo": map[string]any{"toml": "[types.Memo]", "rationale": "The working notes."}},
			"left_out": []any{
				map[string]any{"what": "set-minutes", "reason": "No question needs the minutes."},
				map[string]any{"what": "set-calls", "reason": "Waiting for an answer to Q1."},
			},
		}),
	}
	arguments := map[string]any{"workspace_id": "workspace-1", "ontology_id": "ontology-1", "version_id": "version-1"}

	out := called(t, &Service{Foundry: &versionBackend{version: version}}, toolFoundryGetVersion, arguments)["version"].(map[string]any)
	assert.Equal(t, []any{"set-memos", "set-minutes", "set-calls"}, out["document_set_ids"])
	assert.Equal(t, map[string]any{"read_set_ids": []any{"set-memos"}, "unread_set_ids": []any{"set-minutes", "set-calls"}}, out["document_reads"])
	got := out["definition"].(map[string]any)
	assert.NotContains(t, got, "draft", "the working draft stays out")
	assert.NotContains(t, mustJSON(t, got), "working notes")
	assert.Equal(t, []any{
		map[string]any{"what": "set-minutes", "reason": "No question needs the minutes."},
		map[string]any{"what": "set-calls", "reason": "Waiting for an answer to Q1."},
	}, got["left_out"])
	assert.Len(t, got["questions"], 1)

	assert.NotContains(t, out, "tables", "a generation that can't be read names no table")

	// A route that reads every set reads the whole scope, and a draft that left nothing out says nothing.
	version.Definition = definition("[extractions.all]\ndocuments = \"all\"\n", map[string]any{"rev": 1.0})
	out = called(t, &Service{Foundry: &versionBackend{version: version}}, toolFoundryGetVersion, arguments)["version"].(map[string]any)
	assert.Equal(t, map[string]any{"read_set_ids": []any{"set-memos", "set-minutes", "set-calls"}, "unread_set_ids": []any{}}, out["document_reads"])
	assert.NotContains(t, out["definition"], "left_out")

	// Mappings that can't be read don't say which sets are read: no set is called read or unread.
	version.Definition = definition("[extractions.memos]\ndocuments = 7\n", map[string]any{})
	out = called(t, &Service{Foundry: &versionBackend{version: version}}, toolFoundryGetVersion, arguments)["version"].(map[string]any)
	assert.NotContains(t, out, "document_reads")

	// A listed version has no definition, so nothing is said of what it reads; nor of a Definition v1, which reads no documents.
	listed := (&Service{}).FoundryVersion("workspace-1", "ontology-1", &foundryv1.OntologyVersion{Id: "version-1", DocumentSetIds: []string{"set-memos"}}, false)
	assert.Nil(t, listed.DocumentReads)
	v1, err := structpb.NewStruct(map[string]any{"entities": []any{}})
	require.NoError(t, err)
	assert.Nil(t, (&Service{}).FoundryVersion("workspace-1", "ontology-1", &foundryv1.OntologyVersion{Id: "version-0", Definition: v1}, true).DocumentReads)
}

// A draft's mappings and left-out list name a synced table by an id no tool returns: a hash of
// its connector and name. get_version says which table each id is, from the tables its
// generation drafted from, and names the table on each left-out entry about one, so a caller
// can say what was left out and pass the table to another tool.
func TestGetVersionNamesTheTablesItsDraftLeftOut(t *testing.T) {
	orders, notes := ontologyagent.TableSourceID("connector-1", "orders"), ontologyagent.TableSourceID("connector-2", "public.notes")
	draft := map[string]any{
		"rev": 1.0,
		"left_out": []any{
			map[string]any{"what": notes, "reason": "No question needs the notes."},
			map[string]any{"what": orders + " file.internal_memo", "reason": "Free text no question needs."},
			map[string]any{"what": "src_0123456789abcdef", "reason": "A table its generation didn't read."},
			map[string]any{"what": "set-minutes", "reason": "No question needs the minutes."},
			map[string]any{"what": "the " + orders + " audit columns", "reason": "An entry that doesn't start with an id."},
		},
	}
	definition, err := structpb.NewStruct(map[string]any{
		"format": "agent_bundle", "status": "done", "ontology_toml": "", "mappings_toml": "", "questions": []any{},
		"draft": map[string]any{"ontology_toml": "", "mappings_toml": "", "draft": draft},
	})
	require.NoError(t, err)
	version := func() *foundryv1.OntologyVersion {
		return &foundryv1.OntologyVersion{
			Id: "version-1", Version: 2, Format: foundryv1.OntologyFormat_ONTOLOGY_FORMAT_AGENT_BUNDLE, GenerationId: "generation-1",
			Definition: proto.Clone(definition).(*structpb.Struct),
		}
	}
	generation := &foundryv1.OntologyGeneration{Id: "generation-1", Tables: []*foundryv1.OntologyGenerationTable{
		{ConnectorId: "connector-1", Table: "orders"}, {ConnectorId: "connector-2", Table: "public.notes"},
	}}
	arguments := map[string]any{"workspace_id": "workspace-1", "ontology_id": "ontology-1", "version_id": "version-1"}
	unnamed := []any{
		map[string]any{"what": notes, "reason": "No question needs the notes."},
		map[string]any{"what": orders + " file.internal_memo", "reason": "Free text no question needs."},
		map[string]any{"what": "src_0123456789abcdef", "reason": "A table its generation didn't read."},
		map[string]any{"what": "set-minutes", "reason": "No question needs the minutes."},
		map[string]any{"what": "the " + orders + " audit columns", "reason": "An entry that doesn't start with an id."},
	}

	backend := &versionBackend{version: version(), generation: generation}
	out := called(t, &Service{Foundry: backend}, toolFoundryGetVersion, arguments)["version"].(map[string]any)
	assert.Equal(t, []any{
		map[string]any{"source_id": orders, "connector_id": "connector-1", "table": "orders"},
		map[string]any{"source_id": notes, "connector_id": "connector-2", "table": "public.notes"},
	}, out["tables"])
	assert.Equal(t, []any{
		map[string]any{"what": notes, "reason": "No question needs the notes.", "connector_id": "connector-2", "table": "public.notes"},
		map[string]any{"what": orders + " file.internal_memo", "reason": "Free text no question needs.", "connector_id": "connector-1", "table": "orders"},
		unnamed[2], unnamed[3], unnamed[4],
	}, out["definition"].(map[string]any)["left_out"])

	// A caller who may not read the generation, and a generation that is gone, keep the ids
	// as the draft wrote them: the version is still returned.
	for name, lose := range map[string]func(*versionBackend, *Identity){
		"refused": func(_ *versionBackend, identity *Identity) {
			identity.Authorize = func(_ context.Context, _ any, _ Grant, methods ...string) error {
				if slices.Contains(methods, "GetOntologyGeneration") {
					return errCodeNotFound
				}
				return nil
			}
		},
		"gone": func(b *versionBackend, _ *Identity) {
			b.generationErr = status.Error(codes.NotFound, "generation not found")
		},
	} {
		backend, identity := &versionBackend{version: version(), generation: generation}, reader()
		lose(backend, identity)
		got, _, err := (&Service{Foundry: backend}).GetFoundryVersion(context.Background(), identity, &Call{},
			GetFoundryVersionInput{OntologyInput: OntologyInput{WorkspaceID: "01JZZZZZZZZZZZZZZZZZZZZZZW", OntologyID: "01JZZZZZZZZZZZZZZZZZZZZZZA"}, VersionID: "01JZZZZZZZZZZZZZZZZZZZZZZB"})
		require.NoError(t, err, name)
		assert.Empty(t, got.Version.Tables, name)
		assert.Equal(t, unnamed, got.Version.Definition.(map[string]any)["left_out"], name)
	}

	// A version no generation drafted has no tables to name, and none is looked for.
	handWritten := version()
	handWritten.GenerationId = ""
	backend = &versionBackend{version: handWritten, generation: generation}
	out = called(t, &Service{Foundry: backend}, toolFoundryGetVersion, arguments)["version"].(map[string]any)
	assert.NotContains(t, out, "tables")
	assert.Zero(t, backend.generations)
}

// A grantee reads the version its load holds as Foundry narrows it: without what the draft left
// out, which names sources the load may not hold. The tool adds nothing back: it reads no
// generation for a grantee, so it names none of the tables the draft was given.
func TestGetVersionNamesNoTablesForAGrantee(t *testing.T) {
	// As GetOntologyVersion answers a grantee: the draft kept, its left_out removed.
	definition, err := structpb.NewStruct(map[string]any{
		"format": "agent_bundle", "status": "done", "ontology_toml": "", "questions": []any{},
		"mappings_toml": "[extractions.memos]\ndocuments = [\"set-memos\"]\n",
		"draft":         map[string]any{"ontology_toml": "", "mappings_toml": "", "draft": map[string]any{"rev": 1.0}},
	})
	require.NoError(t, err)
	backend := &versionBackend{
		version: &foundryv1.OntologyVersion{
			Id: "version-1", Version: 2, Format: foundryv1.OntologyFormat_ONTOLOGY_FORMAT_AGENT_BUNDLE, GenerationId: "generation-1",
			DocumentSetIds: []string{"set-memos", "set-minutes"}, Definition: definition,
		},
		generation: &foundryv1.OntologyGeneration{Id: "generation-1", Tables: []*foundryv1.OntologyGenerationTable{{ConnectorId: "connector-1", Table: "salaries"}}},
	}
	got, _, err := (&Service{Foundry: backend}).GetFoundryVersion(context.Background(), grantKey(), &Call{},
		GetFoundryVersionInput{OntologyInput: OntologyInput{WorkspaceID: "01JZZZZZZZZZZZZZZZZZZZZZZW", OntologyID: "01JZZZZZZZZZZZZZZZZZZZZZZA"}, VersionID: "01JZZZZZZZZZZZZZZZZZZZZZZB"})
	require.NoError(t, err)
	assert.Zero(t, backend.generations, "a grantee's read asks for no generation")
	assert.Empty(t, got.Version.Tables)
	out := mustJSON(t, got)
	assert.NotContains(t, out, "salaries")
	assert.NotContains(t, out, "left_out")
	assert.NotContains(t, out, `"tables"`)
	// Which of the sets it may see its routes read is no more than its mappings say.
	assert.Equal(t, &FoundryVersionReadsOutput{ReadSetIDs: []string{"set-memos"}, UnreadSetIDs: []string{"set-minutes"}}, got.Version.DocumentReads)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}
