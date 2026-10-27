package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
)

// agentVersion stores an agent draft of ont-1 whose mappings are
// mappingsTOML and whose scope is scope.
func agentVersion(t *testing.T, store *foundryStore, id, mappingsTOML string, scope ...string) {
	t.Helper()
	def, err := json.Marshal(ontologyagent.Definition{Format: string(foundrymodel.OntologyFormatAgentBundle), Status: ontologyagent.StatusDone,
		MappingsTOML: mappingsTOML})
	if err != nil {
		t.Fatal(err)
	}
	store.versions = append(store.versions, &foundrymodel.OntologyVersion{
		ID: id, WorkspaceID: "ws-1", OntologyID: "ont-1", Format: foundrymodel.OntologyFormatAgentBundle, Definition: def, DocumentSetIDs: &scope,
	})
}

func TestFoundryDocumentSetRPCs(t *testing.T) {
	ctx := context.Background()
	createSet := func(s *Service, name, description string) (*foundryv1.FoundryDocumentSetResponse, error) {
		return s.CreateFoundryDocumentSet(ports.WithActor(ctx, person), &foundryv1.CreateFoundryDocumentSetRequest{
			WorkspaceId: "ws-1", Name: name, Description: description,
		})
	}
	listSets := func(t *testing.T, s *Service) []string {
		t.Helper()
		resp, err := s.ListFoundryDocumentSets(ctx, &foundryv1.ListFoundryDocumentSetsRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, set := range resp.GetSets() {
			out = append(out, set.GetName()+"="+set.GetDescription())
		}
		return out
	}

	// Sets are the workspace's: a new ontology brings none.
	t.Run("creates an ontology with no set", func(t *testing.T) {
		s, store, _ := foundryService(t)
		if _, err := s.CreateFoundryOntology(ctx, &foundryv1.CreateFoundryOntologyRequest{
			WorkspaceId: "ws-1", Name: "Support", TenantId: "tenant-9", ClientToken: "ont-new",
		}); err != nil {
			t.Fatal(err)
		}
		if len(store.sets) != 1 {
			t.Fatalf("the store holds %d sets", len(store.sets))
		}
	})

	// Sets are named, trimmed, and unique per workspace; the agent reads the description.
	t.Run("creates, lists, renames and describes sets", func(t *testing.T) {
		s, store, _ := foundryService(t)
		created, err := createSet(s, "  Contracts ", " Signed customer contracts ")
		if err != nil {
			t.Fatal(err)
		}
		set := created.GetSet()
		if set.GetName() != "Contracts" || set.GetDescription() != "Signed customer contracts" || set.GetId() == "" ||
			set.GetDocuments() != 0 || set.GetCreatedAt() == nil {
			t.Fatalf("set = %+v", set)
		}
		if stored := store.sets[1]; stored.CreatedByUserID == nil || *stored.CreatedByUserID != "user-1" || stored.WorkspaceID != "ws-1" {
			t.Fatalf("stored = %+v", stored)
		}
		if _, err := createSet(s, "contracts", ""); status.Code(err) != codes.AlreadyExists || !strings.Contains(err.Error(), "workspace") {
			t.Fatalf("a second set named contracts: %v", err)
		}
		name := "Agreements"
		renamed, err := s.UpdateFoundryDocumentSet(ctx, &foundryv1.UpdateFoundryDocumentSetRequest{
			WorkspaceId: "ws-1", SetId: set.GetId(), Name: &name,
		})
		if err != nil || renamed.GetSet().GetName() != "Agreements" || renamed.GetSet().GetDescription() != "Signed customer contracts" {
			t.Fatalf("rename = (%+v, %v); the description stays", renamed, err)
		}
		cleared := ""
		if _, err := s.UpdateFoundryDocumentSet(ctx, &foundryv1.UpdateFoundryDocumentSetRequest{
			WorkspaceId: "ws-1", SetId: set.GetId(), Description: &cleared,
		}); err != nil {
			t.Fatal(err)
		}
		if got := listSets(t, s); strings.Join(got, ",") != "General=,Agreements=" {
			t.Fatalf("sets = %v", got)
		}
		general := " " + foundrymodel.FoundryGeneralSetName + " "
		if _, err := s.UpdateFoundryDocumentSet(ctx, &foundryv1.UpdateFoundryDocumentSetRequest{
			WorkspaceId: "ws-1", SetId: set.GetId(), Name: &general,
		}); status.Code(err) != codes.AlreadyExists || !strings.Contains(err.Error(), `named "General"`) {
			t.Fatalf("renamed onto another set's name: %v", err)
		}
		if _, err := s.UpdateFoundryDocumentSet(ctx, &foundryv1.UpdateFoundryDocumentSetRequest{
			WorkspaceId: "ws-1", SetId: "set-gone", Name: &name,
		}); status.Code(err) != codes.NotFound {
			t.Fatalf("renamed a set the workspace doesn't have: %v", err)
		}
	})

	// The ontology-scoped routes the website still calls act on the
	// workspace's sets, for a live ontology of the workspace, as before.
	t.Run("serves the ontology-scoped aliases with the workspace's sets", func(t *testing.T) {
		s, store, _ := foundryService(t)
		aliased, err := s.CreateFoundryOntologyDocumentSet(ctx, &foundryv1.CreateFoundryOntologyDocumentSetRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Name: "Notes"})
		if err != nil {
			t.Fatal(err)
		}
		list, err := s.ListFoundryOntologyDocumentSets(ctx, &foundryv1.ListFoundryOntologyDocumentSetsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || len(list.GetSets()) != 2 || list.GetSets()[1].GetId() != aliased.GetSet().GetId() {
			t.Fatalf("aliased list = (%+v, %v)", list, err)
		}
		if got := listSets(t, s); strings.Join(got, ",") != "General=,Notes=" {
			t.Fatalf("the workspace's sets = %v", got)
		}
		if _, err := s.CreateFoundryOntologyDocumentSet(ctx, &foundryv1.CreateFoundryOntologyDocumentSetRequest{WorkspaceId: "ws-2", OntologyId: "ont-1", Name: "x"}); status.Code(err) != codes.NotFound {
			t.Fatalf("another workspace's ontology: %v", err)
		}
		now := time.Now()
		store.ontologies["ont-1"].DeletedAt = &now
		for name, call := range map[string]func() error{
			"create": func() error {
				_, err := s.CreateFoundryOntologyDocumentSet(ctx, &foundryv1.CreateFoundryOntologyDocumentSetRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Name: "Late"})
				return err
			},
			"list": func() error {
				_, err := s.ListFoundryOntologyDocumentSets(ctx, &foundryv1.ListFoundryOntologyDocumentSetsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
				return err
			},
			"update": func() error {
				_, err := s.UpdateFoundryOntologyDocumentSet(ctx, &foundryv1.UpdateFoundryOntologyDocumentSetRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", SetId: "set-general"})
				return err
			},
			"delete": func() error {
				_, err := s.DeleteFoundryOntologyDocumentSet(ctx, &foundryv1.DeleteFoundryOntologyDocumentSetRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", SetId: "set-general"})
				return err
			},
		} {
			if err := call(); status.Code(err) != codes.NotFound {
				t.Fatalf("%s through a deleted ontology: %v", name, err)
			}
		}
		if _, err := createSet(s, "Workspace's own", ""); err != nil {
			t.Fatalf("the workspace route needs no ontology: %v", err)
		}
	})

	// Names and descriptions are bounded.
	t.Run("refuses a set it can't take", func(t *testing.T) {
		s, _, _ := foundryService(t)
		long := strings.Repeat("x", maxDocumentSetNameChars+1)
		for name, args := range map[string][2]string{
			"no name":              {"  ", ""},
			"a long name":          {long, ""},
			"a long description":   {"Contracts", strings.Repeat("y", maxDocumentSetDescriptionChars+1)},
			"a name of characters": {strings.Repeat("é", maxDocumentSetNameChars+1), ""},
		} {
			if _, err := createSet(s, args[0], args[1]); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if _, err := createSet(s, strings.Repeat("é", maxDocumentSetNameChars), ""); err != nil {
			t.Fatalf("a name of %d characters: %v", maxDocumentSetNameChars, err)
		}
		empty, longer := " ", strings.Repeat("y", maxDocumentSetDescriptionChars+1)
		for name, update := range map[string]*foundryv1.UpdateFoundryDocumentSetRequest{
			"an empty name":      {WorkspaceId: "ws-1", SetId: "set-general", Name: &empty},
			"a long name":        {WorkspaceId: "ws-1", SetId: "set-general", Name: &long},
			"a long description": {WorkspaceId: "ws-1", SetId: "set-general", Description: &longer},
		} {
			if _, err := s.UpdateFoundryDocumentSet(ctx, update); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("update with %s: %v", name, err)
			}
		}
		if _, err := s.CreateFoundryDocumentSet(ctx, &foundryv1.CreateFoundryDocumentSetRequest{Name: "x"}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("no workspace: %v", err)
		}
	})

	// Each set may be a source of a generation, so a workspace creates a bounded number.
	t.Run("caps the sets of a workspace", func(t *testing.T) {
		s, _, _ := foundryService(t)
		for i := range maxDocumentSets - 1 {
			if _, err := createSet(s, "Set "+string(rune('A'+i)), ""); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := createSet(s, "One more", ""); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("past the cap: %v", err)
		}
	})

	// A set holding a document stays; once empty it goes, and its name is free.
	t.Run("deletes only an empty set", func(t *testing.T) {
		s, store, _ := foundryService(t)
		created, err := createSet(s, "Contracts", "")
		if err != nil {
			t.Fatal(err)
		}
		setID := created.GetSet().GetId()
		store.documents = append(store.documents, &foundrymodel.FoundryDocument{ID: "doc-1", WorkspaceID: "ws-1", SetID: setID,
			Status: foundrymodel.FoundryDocumentUploaded})
		del := &foundryv1.DeleteFoundryDocumentSetRequest{WorkspaceId: "ws-1", SetId: setID}
		if _, err := s.DeleteFoundryDocumentSet(ctx, del); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("deleted a set holding a document: %v", err)
		}
		list, err := s.ListFoundryDocumentSets(ctx, &foundryv1.ListFoundryDocumentSetsRequest{WorkspaceId: "ws-1"})
		if err != nil || list.GetSets()[1].GetDocuments() != 1 {
			t.Fatalf("list = (%+v, %v)", list, err)
		}
		store.documents[0].Status = foundrymodel.FoundryDocumentDeleted
		if _, err := s.DeleteFoundryDocumentSet(ctx, del); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteFoundryDocumentSet(ctx, del); status.Code(err) != codes.NotFound {
			t.Fatalf("deleted twice: %v", err)
		}
		if _, err := createSet(s, "Contracts", ""); err != nil {
			t.Fatalf("the name of a deleted set: %v", err)
		}
	})

	// The Data tree says which ontologies read each folder: those whose
	// reading version (the current load's, else the current one) has it in
	// scope, and whether that version's routes read it for what it holds.
	t.Run("lists the ontologies that read each set", func(t *testing.T) {
		s, store, _ := foundryService(t)
		contracts, err := createSet(s, "Contracts", "")
		if err != nil {
			t.Fatal(err)
		}
		contractsID := contracts.GetSet().GetId()
		readBy := func() map[string][]*foundryv1.FoundryDocumentSetReader {
			t.Helper()
			list, err := s.ListFoundryDocumentSets(ctx, &foundryv1.ListFoundryDocumentSetsRequest{WorkspaceId: "ws-1"})
			if err != nil {
				t.Fatal(err)
			}
			out := map[string][]*foundryv1.FoundryDocumentSetReader{}
			for _, set := range list.GetSets() {
				out[set.GetId()] = set.GetReadBy()
			}
			return out
		}
		if got := readBy(); len(got["set-general"])+len(got[contractsID]) != 0 {
			t.Fatalf("with no version, no ontology reads a set: %v", got)
		}
		agentVersion(t, store, "v-contracts", "[extractions.contracts]\ndocuments = [\""+contractsID+"\"]\n", contractsID, "set-general")
		agentVersion(t, store, "v-all", "[extractions.all]\ndocuments = \"all\"\n", "set-general")
		agentVersion(t, store, "v-broken", "[extractions.contracts\n", contractsID)
		o := store.ontologies["ont-1"]
		current := "v-contracts"
		o.CurrentVersionID = &current
		got := readBy()
		if len(got[contractsID]) != 1 || got[contractsID][0].GetOntologyId() != "ont-1" || !got[contractsID][0].GetRouted() {
			t.Fatalf("Contracts = %v: in scope and routed", got[contractsID])
		}
		if len(got["set-general"]) != 1 || got["set-general"][0].GetRouted() {
			t.Fatalf("General = %v: in scope, stored but not routed", got["set-general"])
		}
		store.loads = append(store.loads, &foundrymodel.FoundryLoad{ID: "load-all", OntologyID: "ont-1", VersionID: "v-all", Status: foundrymodel.FoundryLoadReady})
		loadID := "load-all"
		o.CurrentLoadID = &loadID
		got = readBy()
		if len(got[contractsID]) != 0 || len(got["set-general"]) != 1 || !got["set-general"][0].GetRouted() {
			t.Fatalf("the current load's version reads uploads: %v", got)
		}
		o.CurrentLoadID, current = nil, "v-broken"
		got = readBy()
		if len(got[contractsID]) != 1 || got[contractsID][0].GetRouted() {
			t.Fatalf("mappings that can't be read route nothing: %v", got)
		}
		missing := "load-gone"
		o.CurrentLoadID = &missing
		if _, err := s.ListFoundryDocumentSets(ctx, &foundryv1.ListFoundryDocumentSetsRequest{WorkspaceId: "ws-1"}); status.Code(err) != codes.Internal {
			t.Fatalf("a current load that can't be read: %v", err)
		}
	})

	// A connector's set and its documents are its syncs' to write: a
	// person's change is refused with where to make it, and changes nothing.
	// They take no place among the workspace's sets, its names, its document
	// cap or its upload list, and an upload is no repeat of their bytes.
	t.Run("leaves a connector's set and documents to its syncs", func(t *testing.T) {
		s, store, bucket := foundryService(t)
		store.sets = append(store.sets, &foundrymodel.FoundryDocumentSet{ID: "set-notes", WorkspaceID: "ws-1", Name: "Notes",
			Owner: &foundrymodel.FoundryDocumentSetOwner{ConnectorID: "conn-1", ConnectorName: "Team Granola", Stream: "notes"}})
		source := &foundrymodel.FoundryDocumentSource{ExternalID: "not_1", Version: "1|a", At: time.Now()}
		store.documents = append(store.documents,
			&foundrymodel.FoundryDocument{ID: "note-1", WorkspaceID: "ws-1", SetID: "set-notes", Name: "Standup", SHA256: strings.Repeat("a", 64),
				Status: foundrymodel.FoundryDocumentUploaded, Source: source},
			// Its set went with its connector.
			&foundrymodel.FoundryDocument{ID: "note-gone", WorkspaceID: "ws-1", SetID: "set-gone", Name: "Old", Status: foundrymodel.FoundryDocumentUploaded,
				Source: source},
			&foundrymodel.FoundryDocument{ID: "doc-1", WorkspaceID: "ws-1", SetID: "set-general", Name: "msa.md", ClientToken: new("up-1"),
				Status: foundrymodel.FoundryDocumentUploaded})
		const set, doc = "this set belongs to the Team Granola connection; change its settings there",
			"this document belongs to the Team Granola connection; change its settings there"
		name := "Meetings"
		for _, c := range []struct {
			what, want string
			call       func() error
		}{
			{"rename the set", set, func() error {
				_, err := s.UpdateFoundryDocumentSet(ctx, &foundryv1.UpdateFoundryDocumentSetRequest{WorkspaceId: "ws-1", SetId: "set-notes", Name: &name})
				return err
			}},
			{"describe the set", set, func() error {
				_, err := s.UpdateFoundryDocumentSet(ctx, &foundryv1.UpdateFoundryDocumentSetRequest{WorkspaceId: "ws-1", SetId: "set-notes",
					Description: &name})
				return err
			}},
			{"delete the set", set, func() error {
				_, err := s.DeleteFoundryDocumentSet(ctx, &foundryv1.DeleteFoundryDocumentSetRequest{WorkspaceId: "ws-1", SetId: "set-notes"})
				return err
			}},
			{"upload into the set", set, func() error {
				_, err := s.CreateFoundryDocument(ctx, &foundryv1.CreateFoundryDocumentRequest{WorkspaceId: "ws-1", Name: "x.md",
					MediaType: "text/markdown", SizeBytes: 10, Sha256: strings.Repeat("b", 64), ClientToken: "up-2", SetId: "set-notes"})
				return err
			}},
			{"move an upload into the set", set, func() error {
				_, err := s.MoveFoundryDocument(ctx, &foundryv1.MoveFoundryDocumentRequest{WorkspaceId: "ws-1", Id: "doc-1", SetId: "set-notes"})
				return err
			}},
			{"move a document out of the set", doc, func() error {
				_, err := s.MoveFoundryDocument(ctx, &foundryv1.MoveFoundryDocumentRequest{WorkspaceId: "ws-1", Id: "note-1", SetId: "set-general"})
				return err
			}},
			{"rename a document", doc, func() error {
				_, err := s.UpdateFoundryDocument(ctx, &foundryv1.UpdateFoundryDocumentRequest{WorkspaceId: "ws-1", Id: "note-1", Name: "Mine.md"})
				return err
			}},
			{"delete a document", doc, func() error {
				_, err := s.DeleteFoundryDocument(ctx, &foundryv1.DeleteFoundryDocumentRequest{WorkspaceId: "ws-1", Id: "note-1"})
				return err
			}},
			{"delete a document whose connector is gone", "this document belongs to a connection; change its settings there", func() error {
				_, err := s.DeleteFoundryDocument(ctx, &foundryv1.DeleteFoundryDocumentRequest{WorkspaceId: "ws-1", Id: "note-gone"})
				return err
			}},
		} {
			if err := c.call(); status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != c.want {
				t.Errorf("%s: %v, want FailedPrecondition %q", c.what, err, c.want)
			}
		}
		note, _ := store.GetFoundryDocument(ctx, "ws-1", "note-1")
		if note.Name != "Standup" || note.SetID != "set-notes" || note.Status != foundrymodel.FoundryDocumentUploaded || len(bucket.prefixes) != 0 {
			t.Fatalf("a refused change changed the document: %+v, prefixes removed %v", note, bucket.prefixes)
		}
		if got := listSets(t, s); len(got) != 2 || got[1] != "Notes=" {
			t.Fatalf("a refused change changed the set: %v", got)
		}

		if _, err := createSet(s, "notes", ""); err != nil {
			t.Fatalf("a connector's set name taken: %v", err)
		}
		for i := range maxDocumentSets - 2 {
			if _, err := createSet(s, "Set "+string(rune('A'+i)), ""); err != nil {
				t.Fatalf("a connector's set took a place: %v", err)
			}
		}
		listed, err := s.ListFoundryDocuments(ctx, &foundryv1.ListFoundryDocumentsRequest{WorkspaceId: "ws-1"})
		if err != nil || len(listed.GetDocuments()) != 1 || listed.GetDocuments()[0].GetId() != "doc-1" {
			t.Fatalf("the workspace's list = (%v, %v), want its upload only", listed, err)
		}
		listed, err = s.ListFoundryDocuments(ctx, &foundryv1.ListFoundryDocumentsRequest{WorkspaceId: "ws-1", SetId: "set-notes"})
		if err != nil || len(listed.GetDocuments()) != 1 || listed.GetDocuments()[0].GetId() != "note-1" {
			t.Fatalf("the set's list = (%v, %v), want its document", listed, err)
		}
		same, err := s.CreateFoundryDocument(ctx, &foundryv1.CreateFoundryDocumentRequest{WorkspaceId: "ws-1", Name: "standup.md",
			MediaType: "text/markdown", SizeBytes: 10, Sha256: strings.Repeat("a", 64), ClientToken: "up-3", SetId: "set-general"})
		if err != nil || same.GetExisting() || same.GetDocument().GetId() == "note-1" {
			t.Fatalf("an upload of a connector document's bytes = (%v, %v), want a new upload", same, err)
		}
	})

	// A connector's set says whose it is and how many documents it keeps
	// (the deployment's cap), and each of its documents where it comes from; a
	// person's set and an upload say neither.
	t.Run("says which sets and documents are a connector's", func(t *testing.T) {
		s, store, _ := foundryService(t)
		s.deps.Cfg.Foundry.Documents.MaxPerStream = 250
		store.sets = append(store.sets, &foundrymodel.FoundryDocumentSet{ID: "set-notes", WorkspaceID: "ws-1", Name: "Notes",
			Owner: &foundrymodel.FoundryDocumentSetOwner{ConnectorID: "conn-1", ConnectorName: "Team Granola", Stream: "notes"}})
		at := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
		store.documents = append(store.documents,
			&foundrymodel.FoundryDocument{ID: "note-1", WorkspaceID: "ws-1", SetID: "set-notes", Name: "Standup", Status: foundrymodel.FoundryDocumentUploaded,
				Source: &foundrymodel.FoundryDocumentSource{ExternalID: "not_1", Version: "1|2026-10-02", At: at, URL: new("https://notes.granola.ai/d/1")}},
			&foundrymodel.FoundryDocument{ID: "note-2", WorkspaceID: "ws-1", SetID: "set-notes", Name: "Retro", Status: foundrymodel.FoundryDocumentUploaded,
				Source: &foundrymodel.FoundryDocumentSource{ExternalID: "not_2", Version: "1|2026-10-03", At: at}},
			&foundrymodel.FoundryDocument{ID: "doc-1", WorkspaceID: "ws-1", SetID: "set-general", Name: "msa.md", ClientToken: new("up-1"),
				Status: foundrymodel.FoundryDocumentUploaded})

		sets, err := s.ListFoundryDocumentSets(ctx, &foundryv1.ListFoundryDocumentSetsRequest{WorkspaceId: "ws-1"})
		if err != nil || len(sets.GetSets()) != 2 {
			t.Fatalf("sets = (%v, %v)", sets, err)
		}
		want := &foundryv1.FoundryDocumentSetConnector{ConnectorId: "conn-1", Stream: "notes", MaxDocuments: 250, ConnectorName: "Team Granola"}
		if general, notes := sets.GetSets()[0], sets.GetSets()[1]; general.GetConnector() != nil || !proto.Equal(notes.GetConnector(), want) ||
			notes.GetName() != "Notes" {
			t.Fatalf("General's connector = %v, Notes' = %v (%q), want none and %v", general.GetConnector(), notes.GetConnector(), notes.GetName(), want)
		}

		listed, err := s.ListFoundryDocuments(ctx, &foundryv1.ListFoundryDocumentsRequest{WorkspaceId: "ws-1", SetId: "set-notes"})
		if err != nil || len(listed.GetDocuments()) != 2 {
			t.Fatalf("the set's list = (%v, %v)", listed, err)
		}
		withURL := &foundryv1.FoundryDocumentSource{ExternalId: "not_1", Version: "1|2026-10-02", Url: "https://notes.granola.ai/d/1", At: timestamppb.New(at)}
		if got := listed.GetDocuments()[0].GetSource(); !proto.Equal(got, withURL) {
			t.Fatalf("source = %v, want %v", got, withURL)
		}
		noURL := &foundryv1.FoundryDocumentSource{ExternalId: "not_2", Version: "1|2026-10-03", At: timestamppb.New(at)}
		if got := listed.GetDocuments()[1].GetSource(); !proto.Equal(got, noURL) {
			t.Fatalf("a source with no page = %v, want %v", got, noURL)
		}
		upload, err := s.GetFoundryDocument(ctx, &foundryv1.GetFoundryDocumentRequest{WorkspaceId: "ws-1", Id: "doc-1"})
		if err != nil || upload.GetDocument().GetSource() != nil {
			t.Fatalf("an upload = (%v, %v), want no source", upload, err)
		}
	})

	// ids reads the documents another page names, of either origin and
	// deleted or not, in the order asked, on one page of at most 100; it
	// can't be mixed with a set or a page token.
	t.Run("reads documents by id", func(t *testing.T) {
		s, store, _ := foundryService(t)
		store.documents = append(store.documents,
			&foundrymodel.FoundryDocument{ID: "note-1", WorkspaceID: "ws-1", SetID: "set-notes", Status: foundrymodel.FoundryDocumentDeleted,
				Source: &foundrymodel.FoundryDocumentSource{ExternalID: "not_1", Version: "1|a", At: time.Now()}},
			&foundrymodel.FoundryDocument{ID: "doc-1", WorkspaceID: "ws-1", SetID: "set-general", ClientToken: new("up-1"), Status: foundrymodel.FoundryDocumentUploaded},
			&foundrymodel.FoundryDocument{ID: "theirs", WorkspaceID: "ws-2", SetID: "set-x", ClientToken: new("up-2"), Status: foundrymodel.FoundryDocumentUploaded})
		read := func(req *foundryv1.ListFoundryDocumentsRequest) ([]string, error) {
			req.WorkspaceId = "ws-1"
			resp, err := s.ListFoundryDocuments(ctx, req)
			var ids []string
			for _, d := range resp.GetDocuments() {
				ids = append(ids, d.GetId()+"/"+d.GetStatus())
			}
			if resp.GetNextPageToken() != "" {
				t.Fatalf("ids paged: %q", resp.GetNextPageToken())
			}
			return ids, err
		}
		got, err := read(&foundryv1.ListFoundryDocumentsRequest{Ids: []string{"doc-1", "missing", "note-1", "theirs", "doc-1"}})
		if want := []string{"doc-1/uploaded", "note-1/deleted"}; err != nil || !slices.Equal(got, want) {
			t.Fatalf("by id = (%v, %v), want %v", got, err, want)
		}
		hundred := make([]string, maxFoundryDocumentIDs)
		for i := range hundred {
			hundred[i] = "doc-1"
		}
		if got, err := read(&foundryv1.ListFoundryDocumentsRequest{Ids: hundred}); err != nil || len(got) != 1 {
			t.Fatalf("100 ids = (%v, %v)", got, err)
		}
		for name, req := range map[string]*foundryv1.ListFoundryDocumentsRequest{
			"101 ids":        {Ids: append(hundred, "doc-1")},
			"ids and a set":  {Ids: []string{"doc-1"}, SetId: "set-general"},
			"ids and a page": {Ids: []string{"doc-1"}, PageToken: "p"},
		} {
			if _, err := read(req); status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s: %v, want InvalidArgument", name, err)
			}
		}
		if _, err := s.ListFoundryDocuments(ctx, &foundryv1.ListFoundryDocumentsRequest{Ids: []string{"doc-1"}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("ids with no workspace: %v, want InvalidArgument", err)
		}
		store.listErr = errors.New("database down")
		if _, err := read(&foundryv1.ListFoundryDocumentsRequest{Ids: []string{"doc-1"}}); status.Code(err) != codes.Internal {
			t.Fatalf("an outage: %v, want Internal", err)
		}
	})

	// The website still on the ontology routes warns of an upload its
	// ontology won't read: the alias's create says so, the workspace's doesn't.
	t.Run("says on the alias whether the ontology reads an upload's set", func(t *testing.T) {
		s, store, _ := foundryService(t)
		contracts, err := createSet(s, "Contracts", "")
		if err != nil {
			t.Fatal(err)
		}
		contractsID := contracts.GetSet().GetId()
		agentVersion(t, store, "v-contracts", "[extractions.contracts]\ndocuments = [\""+contractsID+"\"]\n", contractsID, "set-general")
		current := "v-contracts"
		store.ontologies["ont-1"].CurrentVersionID = &current
		// upload goes through ont-1's alias, which says whether its current
		// version routes the set; the workspace's create names no ontology.
		upload := func(set, tag string) *foundryv1.CreateFoundryOntologyDocumentResponse {
			t.Helper()
			resp, err := s.CreateFoundryOntologyDocument(ctx, &foundryv1.CreateFoundryOntologyDocumentRequest{
				WorkspaceId: "ws-1", OntologyId: "ont-1", Name: "msa.md", MediaType: "text/markdown", SizeBytes: 10,
				Sha256: strings.Repeat(tag, 64), ClientToken: "up-" + tag, SetId: set,
			})
			if err != nil {
				t.Fatal(err)
			}
			return resp
		}
		if !upload(contractsID, "1").GetRouted() || upload("set-general", "2").GetRouted() {
			t.Fatal("the current version routes only Contracts")
		}
		if _, err := s.CreateFoundryOntologyDocument(ctx, &foundryv1.CreateFoundryOntologyDocumentRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-2", Name: "msa.md", MediaType: "text/markdown", SizeBytes: 10,
			Sha256: strings.Repeat("4", 64), ClientToken: "up-4", SetId: "set-general",
		}); status.Code(err) != codes.NotFound {
			t.Fatalf("another workspace's ontology: %v", err)
		}
	})
}

// spendStore answers what work no person started spent on sets from spend,
// for ws-1 only, and records each read: its set ids and month.
type spendStore struct {
	*foundryStore
	spend  map[string]foundrymodel.FoundryDocumentSetSpend
	reads  [][]string
	months []time.Time
	err    error
}

func (s *spendStore) FoundryDocumentSetSpend(_ context.Context, workspaceID string, setIDs []string, month time.Time) (
	map[string]foundrymodel.FoundryDocumentSetSpend, error) {
	s.reads, s.months = append(s.reads, setIDs), append(s.months, month)
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]foundrymodel.FoundryDocumentSetSpend{}
	for _, id := range setIDs {
		if spend, ok := s.spend[id]; ok && workspaceID == "ws-1" {
			out[id] = spend
		}
	}
	return out, nil
}

// An API source's set holds its newest foundry.events.max_extracted pieces
// and says what work no person started spent on it this month, against its
// allowance, from one read for the whole list; any other connector's set is
// as it was, and costs no read.
func TestFoundryDocumentSetAllowances(t *testing.T) {
	ctx := context.Background()
	pieces := func(id, source string) *foundrymodel.FoundryDocumentSet {
		return &foundrymodel.FoundryDocumentSet{ID: id, WorkspaceID: "ws-1", Name: "Pieces", Documents: 3,
			Owner: &foundrymodel.FoundryDocumentSetOwner{ConnectorID: source, ConnectorKind: foundrymodel.ConnectorKindEvents, ConnectorName: "Replies", Stream: "pieces"}}
	}
	serve := func(t *testing.T, sets ...*foundrymodel.FoundryDocumentSet) (*Service, *spendStore) {
		t.Helper()
		s, store, _ := foundryService(t)
		s.deps.Cfg.Foundry.Documents.MaxPerStream = 2000
		store.sets = append(store.sets, sets...)
		spend := &spendStore{foundryStore: store, spend: map[string]foundrymodel.FoundryDocumentSetSpend{
			"set-replies": {SetID: "set-replies", ConnectorKind: foundrymodel.ConnectorKindEvents, USDMicros: 1_250_000, WorkspaceUSDMicros: 3_000_000},
		}}
		s.deps.DB = spend
		return s, spend
	}
	list := func(t *testing.T, s *Service) []*foundryv1.FoundryDocumentSet {
		t.Helper()
		resp, err := s.ListFoundryDocumentSets(ctx, &foundryv1.ListFoundryDocumentSetsRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetSets()
	}

	t.Run("says an API source's set keeps its newest 500, and this month's spend against $5", func(t *testing.T) {
		granola := &foundrymodel.FoundryDocumentSet{ID: "set-notes", WorkspaceID: "ws-1", Name: "Notes",
			Owner: &foundrymodel.FoundryDocumentSetOwner{ConnectorID: "conn-granola", ConnectorKind: "granola", ConnectorName: "Team Granola", Stream: "notes"}}
		s, store := serve(t, granola, pieces("set-replies", "ev-1"), pieces("set-orders", "ev-2"))
		sets := list(t, s)
		if len(sets) != 4 || sets[0].GetConnector() != nil {
			t.Fatalf("sets = %v", sets)
		}
		for i, want := range []*foundryv1.FoundryDocumentSetConnector{
			{ConnectorId: "conn-granola", Stream: "notes", MaxDocuments: 2000, ConnectorName: "Team Granola", ConnectorKind: "granola"},
			{ConnectorId: "ev-1", Stream: "pieces", MaxDocuments: 500, ConnectorName: "Replies", ConnectorKind: "events",
				MonthSystemUsdMicros: 1_250_000, MaxMonthSystemUsdMicros: proto.Int64(5_000_000)},
			// No spend row yet is nothing spent.
			{ConnectorId: "ev-2", Stream: "pieces", MaxDocuments: 500, ConnectorName: "Replies", ConnectorKind: "events",
				MaxMonthSystemUsdMicros: proto.Int64(5_000_000)},
		} {
			if got := sets[i+1].GetConnector(); !proto.Equal(got, want) {
				t.Fatalf("set %s connector = %v, want %v", sets[i+1].GetId(), got, want)
			}
		}
		if len(store.reads) != 1 || !slices.Equal(store.reads[0], []string{"set-replies", "set-orders"}) {
			t.Fatalf("spend reads = %v, want one of the API sources' sets", store.reads)
		}
		if now := time.Now().UTC(); store.months[0].UTC().Year() != now.Year() || store.months[0].UTC().Month() != now.Month() {
			t.Fatalf("read month %v, want this UTC month", store.months[0])
		}
	})
	t.Run("follows foundry.events max_extracted and max_system_usd_per_source_month", func(t *testing.T) {
		s, _ := serve(t, pieces("set-replies", "ev-1"))
		s.deps.Cfg.Foundry.Events.MaxExtracted, s.deps.Cfg.Foundry.Events.MaxSystemUSDPerSourceMonth = 120, 2.5
		c := list(t, s)[1].GetConnector()
		if c.GetMaxDocuments() != 120 || c.GetMaxMonthSystemUsdMicros() != 2_500_000 || c.GetMonthSystemUsdMicros() != 1_250_000 {
			t.Fatalf("connector = %v", c)
		}
	})
	t.Run("reads no spend for a list with no API source's set", func(t *testing.T) {
		granola := &foundrymodel.FoundryDocumentSet{ID: "set-notes", WorkspaceID: "ws-1", Name: "Notes",
			Owner: &foundrymodel.FoundryDocumentSetOwner{ConnectorID: "conn-granola", ConnectorKind: "granola", ConnectorName: "Team Granola", Stream: "notes"}}
		s, store := serve(t, granola)
		if sets := list(t, s); len(sets) != 2 || sets[1].GetConnector().MaxMonthSystemUsdMicros != nil || len(store.reads) != 0 {
			t.Fatalf("sets = %v, %d spend reads", sets, len(store.reads))
		}
		created, err := s.CreateFoundryDocumentSet(ctx, &foundryv1.CreateFoundryDocumentSetRequest{WorkspaceId: "ws-1", Name: "Contracts"})
		if err != nil || created.GetSet().GetConnector() != nil || len(store.reads) != 0 {
			t.Fatalf("create = (%v, %v), %d spend reads", created, err, len(store.reads))
		}
	})
	// A set is paused as the syncer pauses it: it holds documents, and its
	// allowance, its workspace's, or the month's share for work no person
	// started is spent. The month's graph spend is read once a list.
	for _, tc := range []struct {
		name                    string
		documents               int
		usd, workspace, monthly int64
		paused                  bool
	}{
		{"nothing spent", 3, 0, 0, 0, false},
		{"its allowance a millionth short", 3, 4_999_999, 4_999_999, 0, false},
		{"its allowance spent", 3, 5_000_000, 5_000_000, 0, true},
		{"its allowance spent, holding no documents", 0, 5_000_000, 5_000_000, 0, false},
		{"its workspace's spent", 3, 1_000_000, 20_000_000, 0, true},
		{"the month's share for work no person started spent", 3, 0, 0, 80_000_000, true},
		{"the month's share a millionth short", 3, 0, 0, 79_999_999, false},
	} {
		t.Run("says whether the set is paused with "+tc.name, func(t *testing.T) {
			s, store := serve(t, pieces("set-replies", "ev-1"))
			for i := range tc.documents {
				store.documents = append(store.documents, &foundrymodel.FoundryDocument{ID: fmt.Sprint("piece-", i), WorkspaceID: "ws-1", SetID: "set-replies",
					Status: foundrymodel.FoundryDocumentUploaded})
			}
			s.deps.Cfg.Foundry.Events.MaxSystemUSDPerSourceMonth, s.deps.Cfg.Foundry.Events.MaxSystemUSDPerWorkspaceMonth = 5, 20
			s.deps.Cfg.Foundry.Graph.MonthlyUSD, s.deps.Cfg.Foundry.Graph.MaxJobUSD = 100, 20
			store.spend["set-replies"] = foundrymodel.FoundryDocumentSetSpend{SetID: "set-replies", ConnectorKind: foundrymodel.ConnectorKindEvents,
				USDMicros: tc.usd, WorkspaceUSDMicros: tc.workspace}
			store.spent = tc.monthly
			if c := list(t, s)[1].GetConnector(); c.GetPaused() != tc.paused || c.GetMonthSystemUsdMicros() != tc.usd {
				t.Fatalf("connector = %v, want paused %v", c, tc.paused)
			}
		})
	}
	t.Run("fails when the spend can't be read", func(t *testing.T) {
		s, store := serve(t, pieces("set-replies", "ev-1"))
		store.err = errors.New("connection reset")
		if _, err := s.ListFoundryDocumentSets(ctx, &foundryv1.ListFoundryDocumentSetsRequest{WorkspaceId: "ws-1"}); status.Code(err) != codes.Internal {
			t.Fatalf("list = %v, want Internal", err)
		}
	})
}
