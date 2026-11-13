package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/sample"
)

// sampleBackend is a workspace as add_sample_data finds and leaves it: its connectors, sets and
// documents by SHA-256, with presigned uploads to store.
type sampleBackend struct {
	foundryv1.FoundryServiceClient
	foundryv1.FoundryEventsServiceClient
	store      string
	connectors []*foundryv1.Connector
	sets       []*foundryv1.FoundryDocumentSet
	documents  map[string]*foundryv1.FoundryDocument
	pending    map[string]bool
	created    []*foundryv1.CreateConnectorRequest
	syncs      int
	tokens     []string
	completed  []string
}

func (b *sampleBackend) ListConnectors(context.Context, *foundryv1.ListConnectorsRequest, ...grpc.CallOption) (*foundryv1.ListConnectorsResponse, error) {
	return &foundryv1.ListConnectorsResponse{Connectors: b.connectors}, nil
}

func (b *sampleBackend) CreateConnector(_ context.Context, req *foundryv1.CreateConnectorRequest, _ ...grpc.CallOption) (*foundryv1.ConnectorResponse, error) {
	b.created = append(b.created, req)
	connector := &foundryv1.Connector{Id: "connector-acme", Name: req.GetName(), Config: req.GetConfig()}
	b.connectors = append(b.connectors, connector)
	return &foundryv1.ConnectorResponse{Connector: connector}, nil
}

func (b *sampleBackend) SyncConnector(_ context.Context, req *foundryv1.SyncConnectorRequest, _ ...grpc.CallOption) (*foundryv1.ConnectorSyncResponse, error) {
	b.syncs++
	return &foundryv1.ConnectorSyncResponse{Sync: &foundryv1.ConnectorSync{Id: "sync-1", ConnectorId: req.GetId(), Status: foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_QUEUED}}, nil
}

func (b *sampleBackend) ListFoundryDocumentSets(context.Context, *foundryv1.ListFoundryDocumentSetsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryDocumentSetsResponse, error) {
	return &foundryv1.ListFoundryDocumentSetsResponse{Sets: b.sets}, nil
}

func (b *sampleBackend) CreateFoundryOntologyDocumentSet(_ context.Context, req *foundryv1.CreateFoundryOntologyDocumentSetRequest, _ ...grpc.CallOption) (*foundryv1.FoundryDocumentSetResponse, error) {
	set := &foundryv1.FoundryDocumentSet{Id: "set-acme", Name: req.GetName(), Description: req.GetDescription()}
	b.sets = append(b.sets, set)
	return &foundryv1.FoundryDocumentSetResponse{Set: set}, nil
}

// CreateFoundryOntologyDocument returns the workspace's document of the same bytes, else a new one
// whose bytes go to the store, as the backend does.
func (b *sampleBackend) CreateFoundryOntologyDocument(_ context.Context, req *foundryv1.CreateFoundryOntologyDocumentRequest, _ ...grpc.CallOption) (*foundryv1.CreateFoundryOntologyDocumentResponse, error) {
	b.tokens = append(b.tokens, req.GetClientToken())
	if doc, ok := b.documents[req.GetSha256()]; ok && !b.pending[doc.GetId()] {
		return &foundryv1.CreateFoundryOntologyDocumentResponse{Document: doc}, nil
	}
	doc := &foundryv1.FoundryDocument{Id: "document-" + req.GetSha256()[:8], Name: req.GetName(), SetId: req.GetSetId(), MediaType: req.GetMediaType()}
	b.documents[req.GetSha256()], b.pending[doc.GetId()] = doc, true
	return &foundryv1.CreateFoundryOntologyDocumentResponse{Document: doc, Upload: &foundryv1.FoundryDocumentUpload{
		Url: b.store + "/" + req.GetSha256(), Method: http.MethodPut, Headers: map[string]string{"x-amz-checksum-sha256": req.GetSha256()},
	}}, nil
}

func (b *sampleBackend) CompleteFoundryOntologyDocumentUpload(_ context.Context, req *foundryv1.CompleteFoundryOntologyDocumentUploadRequest, _ ...grpc.CallOption) (*foundryv1.FoundryDocumentReadsResponse, error) {
	b.completed = append(b.completed, req.GetId())
	delete(b.pending, req.GetId())
	for _, doc := range b.documents {
		if doc.GetId() == req.GetId() {
			return &foundryv1.FoundryDocumentReadsResponse{Document: doc}, nil
		}
	}
	return nil, status.Error(codes.NotFound, "document not found")
}

// sampleStore is the bucket the presigned URLs name: it keeps each PUT's bytes by its path, after
// checking the header the URL signs.
type sampleStore struct {
	mu     sync.Mutex
	bodies map[string][]byte
	refuse bool
}

func (s *sampleStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	if s.refuse || r.Method != http.MethodPut || r.Header.Get("x-amz-checksum-sha256") != hex.EncodeToString(sum[:]) {
		http.Error(w, "SignatureDoesNotMatch", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies[strings.TrimPrefix(r.URL.Path, "/")] = body
}

func newSampleWorkspace(t *testing.T) (*Service, *sampleBackend, *sampleStore) {
	t.Helper()
	store := &sampleStore{bodies: map[string][]byte{}}
	server := httptest.NewServer(store)
	t.Cleanup(server.Close)
	backend := &sampleBackend{store: server.URL, documents: map[string]*foundryv1.FoundryDocument{}, pending: map[string]bool{}}
	return &Service{Foundry: backend, Actions: backend, SampleDatabase: &SampleDatabase{Host: "sample.example.rds.amazonaws.com"}}, backend, store
}

func runAddSampleData(t *testing.T, s *Service, confirmationID string) (AddSampleDataResult, error) {
	t.Helper()
	identity := reader()
	identity.WithScope = func(ctx context.Context, _ string, _ bool) context.Context { return ctx }
	op, err := s.ValidateFoundryOperation(context.Background(), identity, FoundryOperationInput{
		Operation: "add_sample_data", Target: "ontology:workspace-1/ontology-1", Payload: map[string]any{},
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(op.CanonicalPayload()))
	result, err := op.Execute(context.Background(), s, identity, confirmationID)
	if err != nil {
		return AddSampleDataResult{}, err
	}
	return result.(AddSampleDataResult), nil
}

// A workspace without the sample gets all of it, as the website's tour makes it: the database
// connected with the sample's read-only login and verified with the RDS bundle, its tables'
// first sync, and every document in the sample's set, its bytes in the store.
func TestAddSampleDataMakesTheWebsitesSample(t *testing.T) {
	s, backend, store := newSampleWorkspace(t)
	out, err := runAddSampleData(t, s, "confirmation-1")
	require.NoError(t, err)

	require.Len(t, backend.created, 1)
	assert.Equal(t, "Acme Corp", backend.created[0].GetName())
	postgres := backend.created[0].GetConfig().GetPostgres()
	assert.Equal(t, "sample.example.rds.amazonaws.com", postgres.GetHost())
	assert.Equal(t, int32(5432), postgres.GetPort())
	assert.Equal(t, []string{"acme", "acme_reader", "acme"}, []string{postgres.GetDatabase(), postgres.GetUser(), postgres.GetPassword()})
	assert.Equal(t, sample.RDSCA, postgres.GetCa())
	assert.False(t, postgres.GetDisableTls())
	assert.Equal(t, 1, backend.syncs)

	require.Len(t, backend.sets, 1)
	assert.Equal(t, "Acme Corp sample", backend.sets[0].GetName())
	assert.Equal(t, sample.SetDescription, backend.sets[0].GetDescription())

	files, err := fs.ReadDir(sample.Documents, "acme")
	require.NoError(t, err)
	require.Len(t, out.Documents, len(files))
	assert.Len(t, files, 9)
	assert.Len(t, store.bodies, len(files), "every document's bytes reached the store")
	assert.Len(t, backend.completed, len(files))
	for _, file := range files {
		body, err := fs.ReadFile(sample.Documents, "acme/"+file.Name())
		require.NoError(t, err)
		sum := sha256.Sum256(body)
		assert.Equal(t, body, store.bodies[hex.EncodeToString(sum[:])], file.Name())
	}
	for _, doc := range out.Documents {
		assert.Equal(t, "set-acme", doc.SetID)
	}
	tokens := map[string]bool{}
	for _, token := range backend.tokens {
		assert.LessOrEqual(t, len(token), 64)
		tokens[token] = true
	}
	assert.Len(t, tokens, len(files), "each document has its own client token")

	assert.Equal(t, "connector-acme", out.ConnectorID)
	assert.Equal(t, "sync-1", out.SyncID)
	assert.Equal(t, "set-acme", out.DocumentSetID)
	assert.Contains(t, out.Next, `{"sample": "acme", "connector_ids": ["connector-acme"], "document_set_ids": ["set-acme"]}`)
}

// Running it again makes nothing twice: the connector, its synced tables, the set and the
// documents the workspace holds are the ones it returns.
func TestAddSampleDataKeepsWhatTheWorkspaceHas(t *testing.T) {
	s, backend, store := newSampleWorkspace(t)
	_, err := runAddSampleData(t, s, "confirmation-1")
	require.NoError(t, err)
	backend.connectors[0].LastSync = &foundryv1.ConnectorSync{Id: "sync-1", Status: foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_SUCCEEDED}
	// A set of the sample's name that a connection writes is not the sample's.
	backend.sets = append([]*foundryv1.FoundryDocumentSet{{Id: "set-granola", Name: "Acme Corp sample", Connector: &foundryv1.FoundryDocumentSetConnector{ConnectorId: "connector-granola"}}}, backend.sets...)
	uploaded := len(store.bodies)
	store.bodies = map[string][]byte{}

	out, err := runAddSampleData(t, s, "confirmation-2")
	require.NoError(t, err)
	assert.Len(t, backend.created, 1, "no second connector")
	assert.Equal(t, 1, backend.syncs, "a succeeded sync is the one to follow")
	assert.Len(t, backend.sets, 2, "no second set")
	assert.Equal(t, "set-acme", out.DocumentSetID)
	assert.Empty(t, store.bodies, "nothing is uploaded again")
	assert.Len(t, out.Documents, uploaded)
	assert.Equal(t, "sync-1", out.SyncID)

	// A sync that failed is run again.
	backend.connectors[0].LastSync.Status = foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_FAILED
	_, err = runAddSampleData(t, s, "confirmation-3")
	require.NoError(t, err)
	assert.Equal(t, 2, backend.syncs)
}

// A store that refuses the bytes fails the run, naming the document, and completes nothing.
func TestAddSampleDataFailsWhenTheStoreRefuses(t *testing.T) {
	s, backend, store := newSampleWorkspace(t)
	store.refuse = true
	_, err := runAddSampleData(t, s, "confirmation-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uploading ")
	assert.Contains(t, err.Error(), "403")
	assert.Empty(t, backend.completed)
}

// A local install's acme-db serves no TLS, so its connector carries no CA; a host with no sample
// database refuses the operation, and it takes no payload.
func TestAddSampleDataDependsOnTheHost(t *testing.T) {
	assert.Empty(t, SampleDatabase{Host: "acme-db", DisableTLS: true}.config().GetCa())
	assert.True(t, SampleDatabase{Host: "acme-db", DisableTLS: true}.config().GetDisableTls())

	prepare := func(s *Service, payload map[string]any) error {
		_, err := s.ValidateFoundryOperation(context.Background(), reader(), FoundryOperationInput{
			Operation: "add_sample_data", Target: "ontology:workspace-1/ontology-1", Payload: payload,
		})
		return err
	}
	err := prepare(&Service{}, map[string]any{})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	err = prepare(&Service{SampleDatabase: &SampleDatabase{Host: "acme-db"}}, map[string]any{"sample": "acme"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add_sample_data takes target ontology:<workspace_id>/<ontology_id> and an empty payload")
}

// A sample document whose bytes the workspace already held in another set stays where someone put
// it; the result says the sample's draft leaves it out unless the user agrees to move it.
func TestAddSampleDataSaysWhichDocumentsAnotherSetHolds(t *testing.T) {
	s, backend, _ := newSampleWorkspace(t)
	const name = "Acme Corp plans and pricing 2026.md"
	body, err := fs.ReadFile(sample.Documents, "acme/"+name)
	require.NoError(t, err)
	sum := sha256.Sum256(body)
	backend.documents[hex.EncodeToString(sum[:])] = &foundryv1.FoundryDocument{Id: "document-pricing", Name: "pricing.md", SetId: "set-general"}

	out, err := runAddSampleData(t, s, "confirmation-1")
	require.NoError(t, err)
	var general []string
	for _, doc := range out.Documents {
		if doc.SetID != out.DocumentSetID {
			general = append(general, doc.ID)
		}
	}
	assert.Equal(t, []string{"document-pricing"}, general)
	assert.Contains(t, out.Next, "1 of the sample's documents were already in another set of the workspace, as documents' set_id says (pricing.md)")
	assert.Contains(t, out.Next, "move_document")

	// With every document in the sample's set, nothing is said of another.
	s, _, _ = newSampleWorkspace(t)
	out, err = runAddSampleData(t, s, "confirmation-1")
	require.NoError(t, err)
	assert.NotContains(t, out.Next, "another set")
}
