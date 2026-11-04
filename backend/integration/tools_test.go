package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	AddReadTool(s, server, everyTool(), "host_tool", "A host's tool.", func(_ context.Context, _ *Identity, call *Call, in input) (output, int, error) {
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
