package api

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// ontologyStore adds ontologies, their versions and current pointers to the
// connector store.
type ontologyStore struct {
	*connectorStore
	ontologies map[string]*foundrymodel.FoundryOntology
	versions   []*foundrymodel.OntologyVersion
}

// GetActiveFoundryAIConfig: no workspace here chose a provider, so model work
// runs on the deployment's default.
func (s *ontologyStore) GetActiveFoundryAIConfig(context.Context, string) (*foundrymodel.FoundryAIConfig, error) {
	return nil, foundrystore.ErrNotFound
}

func (s *ontologyStore) ListConnectors(_ context.Context, workspaceID string) ([]*foundrymodel.Connector, error) {
	var out []*foundrymodel.Connector
	for _, c := range s.rows {
		if c.WorkspaceID == workspaceID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *ontologyStore) GetFoundryOntology(_ context.Context, workspaceID, id string) (*foundrymodel.FoundryOntology, error) {
	o, ok := s.ontologies[id]
	if !ok || o.WorkspaceID != workspaceID {
		return nil, foundrystore.ErrNotFound
	}
	copied := *o
	return &copied, nil
}

func (s *ontologyStore) CreateOntologyVersion(_ context.Context, v *foundrymodel.OntologyVersion) error {
	next := 1
	for _, existing := range s.versions {
		if existing.OntologyID == v.OntologyID {
			next++
		}
	}
	copied := *v
	copied.Version, copied.CreatedAt = next, time.Now()
	s.versions = append(s.versions, &copied)
	v.Version = next
	return nil
}

func (s *ontologyStore) GetOntologyVersion(_ context.Context, ontologyID, id string) (*foundrymodel.OntologyVersion, error) {
	for _, v := range s.versions {
		if v.ID == id && v.OntologyID == ontologyID {
			return v, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *ontologyStore) GetCurrentOntologyVersion(ctx context.Context, ontologyID string) (*foundrymodel.OntologyVersion, error) {
	o, ok := s.ontologies[ontologyID]
	if !ok || o.CurrentVersionID == nil {
		return nil, foundrystore.ErrNotFound
	}
	return s.GetOntologyVersion(ctx, ontologyID, *o.CurrentVersionID)
}

func (s *ontologyStore) ListOntologyVersions(_ context.Context, ontologyID string, limit int) ([]*foundrymodel.OntologyVersion, error) {
	var out []*foundrymodel.OntologyVersion
	for i := len(s.versions) - 1; i >= 0 && len(out) < limit; i-- {
		if s.versions[i].OntologyID == ontologyID {
			listed := *s.versions[i]
			listed.Definition = nil
			out = append(out, &listed)
		}
	}
	return out, nil
}

// GetOntologyVersionScope is a version's stored scope; a legacy version
// (nil) reads none here, as no set of this fake names an ontology.
func (s *ontologyStore) GetOntologyVersionScope(ctx context.Context, ontologyID, id string) ([]string, error) {
	v, err := s.GetOntologyVersion(ctx, ontologyID, id)
	if err != nil {
		return nil, err
	}
	if v.DocumentSetIDs == nil {
		return []string{}, nil
	}
	return *v.DocumentSetIDs, nil
}

func (s *ontologyStore) GetOntologyVersionMappings(ctx context.Context, ontologyID, id string) (string, error) {
	v, err := s.GetOntologyVersion(ctx, ontologyID, id)
	if err != nil || v.Format != foundrymodel.OntologyFormatAgentBundle {
		return "", err
	}
	var def struct {
		MappingsTOML string `json:"mappings_toml"`
	}
	_ = json.Unmarshal(v.Definition, &def) // a draft that doesn't decode routes nothing
	return def.MappingsTOML, nil
}

func (s *ontologyStore) SetCurrentOntologyVersion(_ context.Context, ontologyID, versionID string, expected *string) error {
	o, ok := s.ontologies[ontologyID]
	if !ok || o.DeletedAt != nil {
		return foundrystore.ErrConflict
	}
	current := o.CurrentVersionID
	if (expected == nil) != (current == nil) || expected != nil && *expected != *current {
		return foundrystore.ErrConflict
	}
	o.CurrentVersionID = &versionID
	return nil
}

// ontologyService has one Stripe connector with a synced customers table, and
// ontology ont-1 of ws-1 and ont-2 of ws-2.
func ontologyService(t *testing.T) (*Service, *ontologyStore) {
	t.Helper()
	store := &ontologyStore{connectorStore: &connectorStore{rows: map[string]*foundrymodel.Connector{
		"conn-1": {ID: "conn-1", WorkspaceID: "ws-1", Kind: "stripe", Config: []byte(`{}`)},
	}, tables: []*foundrymodel.ConnectorTable{{
		ConnectorID: "conn-1", WorkspaceID: "ws-1", TableName: "customers",
		Columns: []byte(`[{"name":"customer_id"},{"name":"email"}]`),
	}}}, ontologies: map[string]*foundrymodel.FoundryOntology{
		"ont-1": {ID: "ont-1", WorkspaceID: "ws-1", Name: "Billing", ProjectID: "proj-1", TenantID: "tenant-1"},
		"ont-2": {ID: "ont-2", WorkspaceID: "ws-2", Name: "Other", ProjectID: "proj-2", TenantID: "tenant-2"},
	}}
	return &Service{deps: &runtimeDeps{DB: store, AI: hostedAI(t)}}, store
}

func definition(t *testing.T, table string) *structpb.Struct {
	t.Helper()
	var s structpb.Struct
	raw := `{"entities":[{"name":"Customer","description":"","source":{"connector_id":"conn-1","table":"` + table +
		`"},"primary_key":"customer_id","properties":[{"name":"email","type":"string","description":""}]}],"relationships":[],"rules":[],"constraints":[]}`
	if err := protojson.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func TestFoundryOntologyRPCs(t *testing.T) {
	ctx := context.Background()

	t.Run("stores a valid version without making it current", func(t *testing.T) {
		s, _ := ontologyService(t)
		resp, err := s.CreateOntologyVersion(ports.WithActor(ctx, person), &foundryv1.CreateOntologyVersionRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", Definition: definition(t, "customers"),
		})
		if err != nil {
			t.Fatal(err)
		}
		v := resp.GetVersion()
		if v.GetVersion() != 1 || v.GetIsCurrent() || v.GetSource() != foundryv1.OntologySource_ONTOLOGY_SOURCE_MANUAL ||
			v.GetCreatedByUserId() != "user-1" || len(v.GetDefinition().GetFields()["entities"].GetListValue().GetValues()) != 1 {
			t.Fatalf("version = %+v", v)
		}
		current, err := s.GetCurrentOntologyVersion(ctx, &foundryv1.GetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || current.GetVersion() != nil {
			t.Fatalf("GetCurrentOntologyVersion = (%+v, %v), want no current version", current, err)
		}
	})
	// A hand-written Definition v1 can't be loaded, so it reads no document
	// set; every version says which sets it reads, a legacy one included.
	t.Run("says which document sets each version reads", func(t *testing.T) {
		s, store := ontologyService(t)
		resp, err := s.CreateOntologyVersion(ctx, &foundryv1.CreateOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Definition: definition(t, "customers")})
		if err != nil {
			t.Fatal(err)
		}
		if stored := store.versions[0]; stored.DocumentSetIDs == nil || len(*stored.DocumentSetIDs) != 0 || len(resp.GetVersion().GetDocumentSetIds()) != 0 {
			t.Fatalf("a Definition v1 stores scope %v, answers %v", stored.DocumentSetIDs, resp.GetVersion().GetDocumentSetIds())
		}
		scope := []string{"set-a", "set-b"}
		store.versions = append(store.versions,
			&foundrymodel.OntologyVersion{ID: "scoped", WorkspaceID: "ws-1", OntologyID: "ont-1", Version: 2, Format: foundrymodel.OntologyFormatAgentBundle,
				Definition: []byte(`{}`), DocumentSetIDs: &scope},
			&foundrymodel.OntologyVersion{ID: "legacy", WorkspaceID: "ws-1", OntologyID: "ont-1", Version: 3, Format: foundrymodel.OntologyFormatAgentBundle,
				Definition: []byte(`{}`)})
		got, err := s.GetOntologyVersion(ctx, &foundryv1.GetOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "scoped"})
		if err != nil || strings.Join(got.GetVersion().GetDocumentSetIds(), ",") != "set-a,set-b" {
			t.Fatalf("GetOntologyVersion = (%+v, %v)", got, err)
		}
		list, err := s.ListOntologyVersions(ctx, &foundryv1.ListOntologyVersionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || len(list.GetVersions()) != 3 ||
			strings.Join(list.GetVersions()[1].GetDocumentSetIds(), ",") != "set-a,set-b" || len(list.GetVersions()[0].GetDocumentSetIds()) != 0 {
			t.Fatalf("ListOntologyVersions = (%+v, %v): a legacy version reads its ontology's sets, none here", list, err)
		}
	})
	// An entity on a table the workspace never synced cannot be saved.
	t.Run("rejects a version naming an unsynced table", func(t *testing.T) {
		s, store := ontologyService(t)
		_, err := s.CreateOntologyVersion(ctx, &foundryv1.CreateOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Definition: definition(t, "invoices")})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "invoices") {
			t.Fatalf("CreateOntologyVersion error = %v", err)
		}
		if len(store.versions) != 0 {
			t.Fatalf("stored %d versions", len(store.versions))
		}
	})
	// Choosing the current version is a compare-and-set, and moving back is a rollback.
	t.Run("sets the current version only from the expected one", func(t *testing.T) {
		s, _ := ontologyService(t)
		create := func() string {
			resp, err := s.CreateOntologyVersion(ctx, &foundryv1.CreateOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Definition: definition(t, "customers")})
			if err != nil {
				t.Fatal(err)
			}
			return resp.GetVersion().GetId()
		}
		first, second := create(), create()
		if _, err := s.SetCurrentOntologyVersion(ctx, &foundryv1.SetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: first}); err != nil {
			t.Fatal(err)
		}
		_, err := s.SetCurrentOntologyVersion(ctx, &foundryv1.SetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: second})
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("stale SetCurrentOntologyVersion error = %v, want FailedPrecondition", err)
		}
		if _, err := s.SetCurrentOntologyVersion(ctx, &foundryv1.SetCurrentOntologyVersionRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: second, ExpectedCurrentVersionId: first,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetCurrentOntologyVersion(ctx, &foundryv1.SetCurrentOntologyVersionRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: first, ExpectedCurrentVersionId: second,
		}); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		current, err := s.GetCurrentOntologyVersion(ctx, &foundryv1.GetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || current.GetVersion().GetId() != first || !current.GetVersion().GetIsCurrent() {
			t.Fatalf("GetCurrentOntologyVersion = (%+v, %v)", current, err)
		}
		list, err := s.ListOntologyVersions(ctx, &foundryv1.ListOntologyVersionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || len(list.GetVersions()) != 2 || list.GetVersions()[0].GetDefinition() != nil || !list.GetVersions()[1].GetIsCurrent() {
			t.Fatalf("ListOntologyVersions = (%+v, %v)", list, err)
		}
	})
	// Re-selecting the version that is already current is not a conflict.
	t.Run("treats choosing the current version again as done", func(t *testing.T) {
		s, _ := ontologyService(t)
		resp, err := s.CreateOntologyVersion(ctx, &foundryv1.CreateOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Definition: definition(t, "customers")})
		if err != nil {
			t.Fatal(err)
		}
		id := resp.GetVersion().GetId()
		for range 2 {
			expected := ""
			if current, _ := s.GetCurrentOntologyVersion(ctx, &foundryv1.GetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); current.GetVersion() != nil {
				expected = current.GetVersion().GetId()
			}
			if _, err := s.SetCurrentOntologyVersion(ctx, &foundryv1.SetCurrentOntologyVersionRequest{
				WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: id, ExpectedCurrentVersionId: expected,
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	// An agent draft is returned as the generator stored it, and says so, so the website renders either shape.
	t.Run("returns an agent draft as stored, with its format", func(t *testing.T) {
		s, store := ontologyService(t)
		manual, err := s.CreateOntologyVersion(ctx, &foundryv1.CreateOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Definition: definition(t, "customers")})
		if err != nil {
			t.Fatal(err)
		}
		if manual.GetVersion().GetFormat() != foundryv1.OntologyFormat_ONTOLOGY_FORMAT_FOUNDRY_V1 {
			t.Fatalf("a hand-written version has format %v", manual.GetVersion().GetFormat())
		}
		draft := `{"format":"agent_bundle","status":"needs_input","ontology_toml":"version = \"draft\"\n","questions":[]}`
		store.versions = append(store.versions, &foundrymodel.OntologyVersion{
			ID: "agent-1", WorkspaceID: "ws-1", OntologyID: "ont-1", Version: 2, Format: foundrymodel.OntologyFormatAgentBundle, Source: foundrymodel.OntologySourceGenerated,
			Definition: []byte(draft), CreatedAt: time.Now(),
		})
		got, err := s.GetOntologyVersion(ctx, &foundryv1.GetOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "agent-1"})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := protojson.Marshal(got.GetVersion().GetDefinition())
		if err != nil {
			t.Fatal(err)
		}
		var want, have any
		_ = json.Unmarshal([]byte(draft), &want)
		_ = json.Unmarshal(raw, &have)
		if got.GetVersion().GetFormat() != foundryv1.OntologyFormat_ONTOLOGY_FORMAT_AGENT_BUNDLE || !reflect.DeepEqual(want, have) {
			t.Fatalf("version = %+v, definition %s", got.GetVersion(), raw)
		}
		// It can be made current like any version.
		if _, err := s.SetCurrentOntologyVersion(ctx, &foundryv1.SetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: "agent-1"}); err != nil {
			t.Fatal(err)
		}
		current, err := s.GetCurrentOntologyVersion(ctx, &foundryv1.GetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || current.GetVersion().GetFormat() != foundryv1.OntologyFormat_ONTOLOGY_FORMAT_AGENT_BUNDLE {
			t.Fatalf("GetCurrentOntologyVersion = (%+v, %v)", current, err)
		}
		list, err := s.ListOntologyVersions(ctx, &foundryv1.ListOntologyVersionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || list.GetVersions()[0].GetFormat() != foundryv1.OntologyFormat_ONTOLOGY_FORMAT_AGENT_BUNDLE ||
			list.GetVersions()[1].GetFormat() != foundryv1.OntologyFormat_ONTOLOGY_FORMAT_FOUNDRY_V1 {
			t.Fatalf("ListOntologyVersions = (%+v, %v)", list, err)
		}
	})
	// Agent drafts come from generations; editing one by hand isn't supported yet.
	t.Run("refuses to save an agent draft by hand", func(t *testing.T) {
		s, store := ontologyService(t)
		var def structpb.Struct
		if err := protojson.Unmarshal([]byte(`{"format":"agent_bundle","ontology_toml":""}`), &def); err != nil {
			t.Fatal(err)
		}
		_, err := s.CreateOntologyVersion(ctx, &foundryv1.CreateOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Definition: &def})
		if status.Code(err) != codes.FailedPrecondition || len(store.versions) != 0 {
			t.Fatalf("CreateOntologyVersion error = %v, %d stored", err, len(store.versions))
		}
	})
	// A version belongs to its ontology: another workspace naming it, or another ontology, finds nothing.
	t.Run("does not find another ontology's version", func(t *testing.T) {
		s, _ := ontologyService(t)
		resp, err := s.CreateOntologyVersion(ctx, &foundryv1.CreateOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Definition: definition(t, "customers")})
		if err != nil {
			t.Fatal(err)
		}
		for name, req := range map[string]*foundryv1.GetOntologyVersionRequest{
			"another workspace": {WorkspaceId: "ws-2", OntologyId: "ont-1", Id: resp.GetVersion().GetId()},
			"another ontology":  {WorkspaceId: "ws-2", OntologyId: "ont-2", Id: resp.GetVersion().GetId()},
			"no ontology":       {WorkspaceId: "ws-1", Id: resp.GetVersion().GetId()},
		} {
			_, err = s.GetOntologyVersion(ctx, req)
			if code := status.Code(err); code != codes.NotFound && code != codes.InvalidArgument {
				t.Fatalf("%s: GetOntologyVersion error = %v", name, err)
			}
		}
	})
	// A deleted ontology's versions are gone with it.
	t.Run("refuses a deleted ontology", func(t *testing.T) {
		s, store := ontologyService(t)
		now := time.Now()
		store.ontologies["ont-1"].DeletedAt = &now
		_, err := s.CreateOntologyVersion(ctx, &foundryv1.CreateOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Definition: definition(t, "customers")})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("CreateOntologyVersion error = %v, want NotFound", err)
		}
		_, err = s.GetCurrentOntologyVersion(ctx, &foundryv1.GetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("GetCurrentOntologyVersion error = %v, want NotFound", err)
		}
	})
}
