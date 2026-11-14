package integration

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/document"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/sample"
)

// SampleDatabase is where the host serves the sample company's tables, which add_sample_data
// connects: a hosted stamp's sample RDS, verified with sample.RDSCA, or a local install's acme-db,
// which serves no TLS. Its login is sample's, the same everywhere. A host without one leaves
// Service.SampleDatabase nil, and add_sample_data is refused there.
type SampleDatabase struct {
	Host       string
	DisableTLS bool
}

// config is the connector's configuration, as the website's sample tour sends it.
func (d SampleDatabase) config() *foundryv1.DatabaseConnectorConfig {
	config := &foundryv1.DatabaseConnectorConfig{
		Host: d.Host, Port: sample.DatabasePort, Database: sample.DatabaseName, User: sample.DatabaseUser, Password: sample.DatabasePassword, DisableTls: d.DisableTLS,
	}
	if !d.DisableTLS {
		config.Ca = sample.RDSCA
	}
	return config
}

// reads says whether a connector reads the sample database, as the website's isSampleConnector
// tells: a PostgreSQL connector on its host, database and user, whatever it is called.
func (d SampleDatabase) reads(c *foundryv1.Connector) bool {
	postgres := c.GetConfig().GetPostgres()
	return postgres != nil && postgres.GetHost() == d.Host && postgres.GetDatabase() == sample.DatabaseName && postgres.GetUser() == sample.DatabaseUser
}

// AddSampleDataOperation adds the sample company's data to a workspace for one ontology, as the
// website's sample tour does before it generates: a connector to its database, with its tables
// synced, and its documents in a set of their own. Each part the workspace already has is kept,
// so running it again finishes what an earlier run left undone and makes nothing twice.
type AddSampleDataOperation struct{ workspaceID, ontologyID string }

func (o AddSampleDataOperation) CanonicalPayload() []byte { return mustCanonical(map[string]any{}) }

type AddSampleDataResult struct {
	ConnectorID   string                  `json:"connector_id"`
	SyncID        string                  `json:"sync_id"`
	DocumentSetID string                  `json:"document_set_id"`
	Documents     []FoundryDocumentOutput `json:"documents"`
	Next          string                  `json:"next"`
}

// sampleUploads sends the sample's documents to the presigned URLs the backend gives, as a
// browser would: each is at most 50 KiB.
var sampleUploads = &http.Client{Timeout: 30 * time.Second}

func (o AddSampleDataOperation) Execute(ctx context.Context, s *Service, identity *Identity, confirmationID string) (any, error) {
	ws, id := o.workspaceID, o.ontologyID
	out := AddSampleDataResult{Documents: []FoundryDocumentOutput{}}

	connectors, err := s.Foundry.ListConnectors(ctx, &foundryv1.ListConnectorsRequest{WorkspaceId: ws})
	if err != nil {
		return nil, err
	}
	var connector *foundryv1.Connector
	if i := slices.IndexFunc(connectors.GetConnectors(), s.SampleDatabase.reads); i >= 0 {
		connector = connectors.GetConnectors()[i]
	} else {
		created, err := s.Actions.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: ws, Name: sample.ConnectorName, Config: &foundryv1.ConnectorConfig{
			Kind: &foundryv1.ConnectorConfig_Postgres{Postgres: s.SampleDatabase.config()},
		}})
		if err != nil {
			return nil, err
		}
		connector = created.GetConnector()
	}
	out.ConnectorID = connector.GetId()
	// Its tables are synced once: a sync that succeeded or is under way is the one to follow.
	switch last := connector.GetLastSync(); last.GetStatus() {
	case foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_SUCCEEDED, foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_QUEUED,
		foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_RUNNING:
		out.SyncID = last.GetId()
	default:
		synced, err := s.Actions.SyncConnector(ctx, &foundryv1.SyncConnectorRequest{WorkspaceId: ws, Id: connector.GetId()})
		if err != nil {
			return nil, err
		}
		out.SyncID = synced.GetSync().GetId()
	}

	// The documents' set is the website's: an upload set by the sample's name.
	sets, err := s.Foundry.ListFoundryDocumentSets(ctx, &foundryv1.ListFoundryDocumentSetsRequest{WorkspaceId: ws})
	if err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(sets.GetSets(), func(set *foundryv1.FoundryDocumentSet) bool {
		return set.GetName() == sample.SetName && set.GetConnector() == nil
	}); i >= 0 {
		out.DocumentSetID = sets.GetSets()[i].GetId()
	} else {
		created, err := s.Actions.CreateFoundryOntologyDocumentSet(ctx, &foundryv1.CreateFoundryOntologyDocumentSetRequest{
			WorkspaceId: ws, OntologyId: id, Name: sample.SetName, Description: sample.SetDescription,
		})
		if err != nil {
			return nil, err
		}
		out.DocumentSetID = created.GetSet().GetId()
	}

	files, err := fs.ReadDir(sample.Documents, "acme")
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		body, err := fs.ReadFile(sample.Documents, "acme/"+file.Name())
		if err != nil {
			return nil, err
		}
		mediaType, _ := document.MediaTypeOf(file.Name())
		sum := sha256.Sum256(body)
		// One token per document of this run: a retry of the run finds what it made, and another
		// run never takes this one's, a deleted document included.
		token := sha256.Sum256([]byte(confirmationID + "\x00" + file.Name()))
		created, err := s.Actions.CreateFoundryOntologyDocument(ctx, &foundryv1.CreateFoundryOntologyDocumentRequest{
			WorkspaceId: ws, OntologyId: id, SetId: out.DocumentSetID, Name: file.Name(), MediaType: string(mediaType),
			SizeBytes: int64(len(body)), Sha256: hex.EncodeToString(sum[:]), ClientToken: hex.EncodeToString(token[:16]),
		})
		if err != nil {
			return nil, fmt.Errorf("adding %s: %w", file.Name(), err)
		}
		doc := created.GetDocument()
		// The workspace already holding these bytes returns their document, with nothing to send.
		if upload := created.GetUpload(); upload != nil {
			if err := sendSampleDocument(ctx, upload, body); err != nil {
				return nil, fmt.Errorf("uploading %s: %w", file.Name(), err)
			}
			completed, err := s.Actions.CompleteFoundryOntologyDocumentUpload(identity.WithScope(ctx, ws, true),
				&foundryv1.CompleteFoundryOntologyDocumentUploadRequest{WorkspaceId: ws, OntologyId: id, Id: doc.GetId()})
			if err != nil {
				return nil, fmt.Errorf("completing %s: %w", file.Name(), err)
			}
			doc = completed.GetDocument()
		}
		out.Documents = append(out.Documents, FoundryDocument(doc))
	}
	out.Next = "Follow the sync with " + toolFoundryGetSync + " (connector_id, sync_id, wait) until it succeeds. Then prepare generate_ontology on this ontology with " +
		`{"sample": "acme", "connector_ids": ["` + out.ConnectorID + `"], "document_set_ids": ["` + out.DocumentSetID + `"]}: the ready-made draft, in seconds. ` +
		"Follow it with " + toolFoundryGetGeneration + ", make its result_version_id current with set_current_version, load it with create_load and follow " + toolFoundryGetLoad +
		", then ask one of " + toolFoundryGetContext + "'s example questions with " + toolFoundryAsk + "."
	// A document whose bytes another set already held stays there, so the sample's draft, which
	// reads only its own set, leaves it out: say which, without moving what someone put there.
	if elsewhere := slices.DeleteFunc(slices.Clone(out.Documents), func(d FoundryDocumentOutput) bool { return d.SetID == out.DocumentSetID }); len(elsewhere) > 0 {
		names := make([]string, 0, len(elsewhere))
		for _, d := range elsewhere {
			names = append(names, d.Name)
		}
		out.Next += fmt.Sprintf(" %d of the sample's documents were already in another set of the workspace, as documents' set_id says (%s): "+
			"the sample's draft reads only document_set_id, so its loads leave them out unless the user agrees to move them there with move_document.",
			len(elsewhere), strings.Join(names, ", "))
	}
	return out, nil
}

// sendSampleDocument PUTs a document's bytes to the store with the headers its presigned URL signs.
func sendSampleDocument(ctx context.Context, upload *foundryv1.FoundryDocumentUpload, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, cmp.Or(upload.GetMethod(), http.MethodPut), upload.GetUrl(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	for name, value := range upload.GetHeaders() {
		req.Header.Set(name, value)
	}
	resp, err := sampleUploads.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the store answered %s", resp.Status)
	}
	return nil
}
