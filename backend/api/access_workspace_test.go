package api

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// A workspace grant gives every connector, table and set, named or not, but
// People enrichment: its holder reads every load that applied no profiles,
// part of one that did, every set's documents, and loses nothing when a
// connector goes. It names nothing.
func TestWorkspaceGrant(t *testing.T) {
	s, store := accessService(t)
	ctx := ports.WithActor(context.Background(), person)
	key := &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_SERVICE_CREDENTIAL, Id: "cred-1"}
	everything := []*foundryv1.FoundryAccessTarget{{Kind: foundryv1.FoundryAccessTarget_KIND_WORKSPACE}}
	set, err := s.SetFoundryAccessGrants(ctx, &foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: key, Targets: everything})
	if err != nil || len(set.GetGrants()) != 1 {
		t.Fatalf("%+v, %v", set, err)
	}
	if g := set.GetGrants()[0].GetTarget(); g.GetKind() != foundryv1.FoundryAccessTarget_KIND_WORKSPACE || g.GetConnectorId() != "" || g.GetTable() != "" ||
		g.GetDocumentSetId() != "" {
		t.Fatalf("the grant reads back as %+v", g)
	}
	for _, bad := range []*foundryv1.FoundryAccessTarget{
		{Kind: foundryv1.FoundryAccessTarget_KIND_WORKSPACE, ConnectorId: "conn-1"},
		{Kind: foundryv1.FoundryAccessTarget_KIND_WORKSPACE, Table: "customers"},
		{Kind: foundryv1.FoundryAccessTarget_KIND_WORKSPACE, DocumentSetId: "set-general"},
	} {
		if _, err := s.SetFoundryAccessGrants(ctx, &foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: key,
			Targets: []*foundryv1.FoundryAccessTarget{bad}}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%+v: %v", bad, err)
		}
	}

	readers := func(t *testing.T) (whole, part map[string]bool) {
		t.Helper()
		got, err := s.ListFoundryAccess(ctx, &foundryv1.ListFoundryAccessRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		whole, part = map[string]bool{}, map[string]bool{}
		for _, o := range got.GetOntologies() {
			whole[o.GetOntologyId()] = slices.ContainsFunc(o.GetReaders(), func(p *foundryv1.FoundryPrincipal) bool { return p.GetId() == "cred-1" })
			part[o.GetOntologyId()] = slices.ContainsFunc(o.GetPartialReaders(), func(p *foundryv1.FoundryPrincipal) bool { return p.GetId() == "cred-1" })
		}
		return whole, part
	}
	if whole, _ := readers(t); !whole["ont-1"] || !whole["ont-3"] {
		t.Fatalf("reads Billing and Payroll, whose PostgreSQL connector no grant names: %v", whole)
	}

	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	mine, err := client.GetMyFoundryAccess(as(agent), &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
	if err != nil || !mine.GetHasGrants() || !slices.Equal(mine.GetOntologyIds(), []string{"ont-1", "ont-3"}) || len(mine.GetPartialOntologyIds()) != 0 {
		t.Fatalf("%+v, %v", mine, err)
	}

	t.Run("every set's documents", func(t *testing.T) {
		store.sets = append(store.sets, &foundrymodel.FoundryDocumentSet{ID: "set-hr", WorkspaceID: "ws-1", Name: "HR"},
			&foundrymodel.FoundryDocumentSet{ID: "set-notes", WorkspaceID: "ws-1", Name: "Notes", Owner: &foundrymodel.FoundryDocumentSetOwner{ConnectorID: "conn-later"}})
		store.documents = append(store.documents,
			&foundrymodel.FoundryDocument{ID: "doc-general", WorkspaceID: "ws-1", SetID: "set-general", Name: "contract.md", MediaType: foundrymodel.FoundryMediaMarkdown},
			&foundrymodel.FoundryDocument{ID: "doc-hr", WorkspaceID: "ws-1", SetID: "set-hr", Name: "salaries.md", MediaType: foundrymodel.FoundryMediaMarkdown},
			&foundrymodel.FoundryDocument{ID: "doc-note", WorkspaceID: "ws-1", SetID: "set-notes", Name: "standup.md", MediaType: foundrymodel.FoundryMediaMarkdown})
		got, err := client.ListFoundryDocuments(as(agent), &foundryv1.ListFoundryDocumentsRequest{WorkspaceId: "ws-1", Ids: []string{"doc-general", "doc-hr", "doc-note"}})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, d := range got.GetDocuments() {
			ids = append(ids, d.GetId())
		}
		if !slices.Equal(ids, []string{"doc-general", "doc-hr", "doc-note"}) {
			t.Fatalf("named %v: an uploaded set and a connector's, added after the grant", ids)
		}
	})

	t.Run("a connector deleted costs it nothing", func(t *testing.T) {
		got, err := s.PreviewFoundryAccessChange(ctx, &foundryv1.PreviewFoundryAccessChangeRequest{WorkspaceId: "ws-1",
			Change: &foundryv1.FoundryAccessChange{Change: &foundryv1.FoundryAccessChange_DeleteConnector{DeleteConnector: "conn-1"}}})
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range got.GetLosses() {
			if l.GetPrincipal().GetId() == "cred-1" {
				t.Fatalf("the workspace grant loses %s", l.GetOntologyId())
			}
		}
	})

	t.Run("never People enrichment", func(t *testing.T) {
		payroll := store.inputs["ont-3"]
		defer func() { store.inputs["ont-3"] = payroll }()
		enriched := payroll
		enriched.Inputs.Enrichment = true
		store.inputs["ont-3"] = enriched
		whole, part := readers(t)
		if whole["ont-3"] || !part["ont-3"] || !whole["ont-1"] {
			t.Fatalf("a load that applied profiles is read in part: whole %v, part %v", whole, part)
		}
		if _, err := s.SetFoundryAccessGrants(ctx, &foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: key,
			Targets: append(everything, &foundryv1.FoundryAccessTarget{Kind: foundryv1.FoundryAccessTarget_KIND_ENRICHMENT})}); err != nil {
			t.Fatal(err)
		}
		if whole, _ := readers(t); !whole["ont-3"] {
			t.Fatalf("with People enrichment granted too, it reads it whole: %v", whole)
		}
	})
}

// Under a workspace grant only People enrichment is new: anything else
// given to someone with raw access since is narrowing, and is allowed.
func TestWorkspaceGrantOnlyWhatsNew(t *testing.T) {
	s, store := accessService(t)
	ctx := ports.WithActor(context.Background(), person)
	s.deps.People = accessPeople{"querying": {Role: ports.RoleMember, RawAccess: true}}
	querying := &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_USER, Id: "querying"}
	set := func(targets ...*foundryv1.FoundryAccessTarget) error {
		_, err := s.SetFoundryAccessGrants(ctx, &foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: querying, Targets: targets})
		return err
	}
	workspace := &foundryv1.FoundryAccessTarget{Kind: foundryv1.FoundryAccessTarget_KIND_WORKSPACE}
	connector := &foundryv1.FoundryAccessTarget{Kind: foundryv1.FoundryAccessTarget_KIND_CONNECTOR, ConnectorId: "conn-1"}
	if err := set(workspace); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("raw access can't be confined to everything: %v", err)
	}
	store.grants = append(store.grants, foundrymodel.FoundryAccessGrant{ID: "raw-ws", WorkspaceID: "ws-1",
		Principal: access.Principal{Kind: access.PrincipalUser, ID: "querying"}, Target: access.WorkspaceTarget()})
	if err := set(workspace, connector, &foundryv1.FoundryAccessTarget{Kind: foundryv1.FoundryAccessTarget_KIND_DOCUMENT_SET, DocumentSetId: "set-general"}); err != nil {
		t.Fatalf("sources the workspace grant gives are nothing new: %v", err)
	}
	if err := set(workspace, &foundryv1.FoundryAccessTarget{Kind: foundryv1.FoundryAccessTarget_KIND_ENRICHMENT}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("People enrichment is new: %v", err)
	}
	if err := set(connector); err != nil {
		t.Fatalf("everything down to a connector is narrowing: %v", err)
	}
	if err := set(workspace); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a connector up to everything is adding: %v", err)
	}
}
