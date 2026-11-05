package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// accessStore adds grants and what current loads hold to the question store.
type accessStore struct {
	*questionStore
	grants []foundrymodel.FoundryAccessGrant
	// inputs are each ontology's current load as grants see it, and olderLoads
	// its other loads, by load.
	inputs     map[string]foundrymodel.FoundryLoadInputs
	olderLoads map[string]foundrymodel.FoundryLoadInputs
	// heldSets is what the last list of an ontology's documents was kept to.
	heldSets *[]string
	// holdings are what each load holds, document by document.
	holdings map[string]access.Holdings
	// granteeSpend is what each grantee's questions charged to the month.
	granteeSpend map[access.Principal]foundrymodel.FoundryQuestionSpend
	// versionInputs are what a new load of each version would hold.
	versionInputs map[string]access.Inputs
	// holding are the ontologies whose current loads hold each document.
	holding map[string][]string
	// restricted are each workspace's restricted members.
	restricted map[string][]string
	teams      []foundrymodel.FoundryTeam
}

func (s *accessStore) ListFoundryAccessGrants(_ context.Context, workspaceID string, principals []access.Principal) ([]foundrymodel.FoundryAccessGrant, error) {
	var out []foundrymodel.FoundryAccessGrant
	for _, g := range s.grants {
		if g.WorkspaceID == workspaceID && (len(principals) == 0 || slices.Contains(principals, g.Principal)) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *accessStore) ListFoundryCurrentLoadInputs(_ context.Context, workspaceID string, ontologyIDs []string) (map[string]foundrymodel.FoundryLoadInputs, error) {
	out := map[string]foundrymodel.FoundryLoadInputs{}
	for id, in := range s.inputs {
		if o, ok := s.ontologies[id]; ok && o.WorkspaceID == workspaceID && (len(ontologyIDs) == 0 || slices.Contains(ontologyIDs, id)) {
			out[id] = in
		}
	}
	return out, nil
}

func (s *accessStore) ListFoundryLoadInputs(_ context.Context, workspaceID string, loadIDs []string) (map[string]foundrymodel.FoundryLoadInputs, error) {
	out := map[string]foundrymodel.FoundryLoadInputs{}
	for id, in := range s.olderLoads {
		if slices.Contains(loadIDs, id) {
			out[id] = in
		}
	}
	for id, in := range s.inputs {
		if o, ok := s.ontologies[id]; ok && o.WorkspaceID == workspaceID && slices.Contains(loadIDs, in.LoadID) {
			out[in.LoadID] = in
		}
	}
	return out, nil
}

func (s *accessStore) ListFoundryOntologyDocuments(ctx context.Context, workspaceID, currentLoadID string, scope []string, heldSets *[]string,
	filter foundrymodel.FoundryDocumentFilter, pageToken string, limit int) ([]*foundrymodel.FoundryDocument, string, error) {
	s.heldSets = heldSets
	docs, next, err := s.questionStore.ListFoundryOntologyDocuments(ctx, workspaceID, currentLoadID, scope, heldSets, filter, pageToken, limit)
	if err != nil || heldSets == nil {
		return docs, next, err
	}
	return slices.DeleteFunc(slices.Clone(docs), func(d *foundrymodel.FoundryDocument) bool { return !slices.Contains(*heldSets, d.SetID) }), next, nil
}

func (s *accessStore) FoundryLoadHoldings(_ context.Context, _, loadID string) (access.Holdings, error) {
	h, ok := s.holdings[loadID]
	if !ok {
		return access.Holdings{}, foundrystore.ErrNotFound
	}
	return h, nil
}

func (s *accessStore) SumFoundryGranteeQuestionSpend(_ context.Context, _ string, grantee access.Principal, _ time.Time) (foundrymodel.FoundryQuestionSpend, error) {
	return s.granteeSpend[grantee], nil
}

// SetFoundryAccessGrants refuses a connector or set named "missing", as the
// store refuses one that isn't the workspace's.
func (s *accessStore) SetFoundryAccessGrants(_ context.Context, workspaceID string, p access.Principal, targets []access.Target, by *string) error {
	for _, t := range targets {
		if t.ConnectorID() == "missing" || t.DocumentSetID() == "missing" {
			return foundrystore.ErrNotFound
		}
	}
	s.grants = slices.DeleteFunc(s.grants, func(g foundrymodel.FoundryAccessGrant) bool { return g.WorkspaceID == workspaceID && g.Principal == p })
	for i, t := range targets {
		s.grants = append(s.grants, foundrymodel.FoundryAccessGrant{ID: fmt.Sprintf("grant-%d", i), WorkspaceID: workspaceID, Principal: p, Target: t,
			CreatedByUserID: by, CreatedAt: time.Now()})
	}
	if len(targets) > 0 {
		switch p.Kind {
		case access.PrincipalUser:
			s.restrict(workspaceID, p.ID)
		case access.PrincipalTeam:
			for _, t := range s.teams {
				if t.ID == p.ID {
					s.restrict(workspaceID, t.MemberUserIDs...)
				}
			}
		}
	}
	return nil
}

func (s *accessStore) restrict(workspaceID string, userIDs ...string) {
	for _, userID := range userIDs {
		if !slices.Contains(s.restricted[workspaceID], userID) {
			s.restricted[workspaceID] = append(s.restricted[workspaceID], userID)
		}
	}
}

func (s *accessStore) FoundryVersionInputs(_ context.Context, _, _, versionID string) (access.Inputs, error) {
	in, ok := s.versionInputs[versionID]
	if !ok {
		return access.Inputs{}, foundrystore.ErrNotFound
	}
	return in, nil
}

func (s *accessStore) ListFoundryOntologiesHoldingDocument(_ context.Context, _, documentID string) ([]string, error) {
	return s.holding[documentID], nil
}

func (s *accessStore) IsFoundryMemberRestricted(_ context.Context, workspaceID, userID string) (bool, error) {
	return slices.Contains(s.restricted[workspaceID], userID), nil
}

func (s *accessStore) ListFoundryRestrictedMembers(_ context.Context, workspaceID string) ([]string, error) {
	return s.restricted[workspaceID], nil
}

func (s *accessStore) SetFoundryMemberRestricted(_ context.Context, workspaceID, userID string, restricted bool, _ *string) error {
	if !restricted && slices.ContainsFunc(s.grants, func(g foundrymodel.FoundryAccessGrant) bool {
		return g.WorkspaceID == workspaceID && (g.Principal == access.Principal{Kind: access.PrincipalUser, ID: userID} ||
			g.Principal.Kind == access.PrincipalTeam && slices.ContainsFunc(s.teams, func(t foundrymodel.FoundryTeam) bool {
				return t.ID == g.Principal.ID && slices.Contains(t.MemberUserIDs, userID)
			}))
	}) {
		return foundrystore.ErrConflict
	}
	s.restricted[workspaceID] = slices.DeleteFunc(s.restricted[workspaceID], func(u string) bool { return u == userID })
	if restricted {
		s.restricted[workspaceID] = append(s.restricted[workspaceID], userID)
	}
	return nil
}

func (s *accessStore) ListFoundryTeams(_ context.Context, workspaceID string) ([]foundrymodel.FoundryTeam, error) {
	var out []foundrymodel.FoundryTeam
	for _, t := range s.teams {
		if t.WorkspaceID == workspaceID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *accessStore) FoundryTeamsOf(_ context.Context, workspaceID, userID string) ([]string, error) {
	var out []string
	for _, t := range s.teams {
		if t.WorkspaceID == workspaceID && slices.Contains(t.MemberUserIDs, userID) {
			out = append(out, t.ID)
		}
	}
	return out, nil
}

func (s *accessStore) CreateFoundryTeam(_ context.Context, team *foundrymodel.FoundryTeam) error {
	for _, t := range s.teams {
		if t.WorkspaceID == team.WorkspaceID && t.Name == team.Name {
			return foundrystore.ErrConflict
		}
	}
	s.teams = append(s.teams, *team)
	return nil
}

func (s *accessStore) UpdateFoundryTeam(_ context.Context, workspaceID, teamID, name string, members []string) error {
	for _, t := range s.teams {
		if t.WorkspaceID == workspaceID && t.ID != teamID && t.Name == name {
			return foundrystore.ErrConflict
		}
	}
	for i := range s.teams {
		if s.teams[i].ID == teamID && s.teams[i].WorkspaceID == workspaceID {
			s.teams[i].Name, s.teams[i].MemberUserIDs = name, members
			if slices.ContainsFunc(s.grants, func(g foundrymodel.FoundryAccessGrant) bool {
				return g.Principal == access.Principal{Kind: access.PrincipalTeam, ID: teamID}
			}) {
				s.restrict(workspaceID, members...)
			}
			return nil
		}
	}
	return foundrystore.ErrNotFound
}

func (s *accessStore) DeleteFoundryTeam(_ context.Context, workspaceID, teamID string) error {
	n := len(s.teams)
	s.teams = slices.DeleteFunc(s.teams, func(t foundrymodel.FoundryTeam) bool { return t.ID == teamID && t.WorkspaceID == workspaceID })
	if len(s.teams) == n {
		return foundrystore.ErrNotFound
	}
	return nil
}

var (
	stripeCustomers = access.TableRef{ConnectorID: "conn-1", Table: "customers"}
	pgPeople        = access.TableRef{ConnectorID: "conn-pg", Table: "people"}
	agentKey        = access.Principal{Kind: access.PrincipalServiceCredential, ID: "cred-1"}
	// agent is the agent's key, admitted through its grants alone.
	agent = ports.Actor{Source: ports.SourceGrantee, ServiceCredentialID: "cred-1", WorkspaceID: "ws-1"}
	// member is a person admitted through their grants alone.
	member = ports.Actor{Source: ports.SourceGrantee, UserID: "user-9", WorkspaceID: "ws-1"}
)

// accessService serves ws-1: Billing (ont-1), whose current load (load-1, of
// ver-1) holds Stripe's customers and General's documents, and Payroll
// (ont-3, load-3), which holds PostgreSQL's people alone. The agent's key holds
// Stripe and General, so it reads Billing but not Payroll; user-9 holds
// General alone, and so holds part of Billing.
func accessService(t *testing.T) (*Service, *accessStore) {
	t.Helper()
	s, questions := questionService(t)
	store := &accessStore{questionStore: questions, holding: map[string][]string{}, restricted: map[string][]string{}}
	payroll := "load-3"
	store.ontologies["ont-3"] = &foundrymodel.FoundryOntology{ID: "ont-3", WorkspaceID: "ws-1", Name: "Payroll", ProjectID: "proj-1", TenantID: "tenant-9",
		CurrentLoadID: &payroll, CreatedAt: time.Now().Add(-time.Hour)}
	store.loads = append(store.loads, &foundrymodel.FoundryLoad{ID: "load-3", OntologyID: "ont-3", VersionID: "ver-3", Status: foundrymodel.FoundryLoadReady})
	store.versions = append(store.versions,
		&foundrymodel.OntologyVersion{ID: "ver-1", WorkspaceID: "ws-1", OntologyID: "ont-1", Format: foundrymodel.OntologyFormatAgentBundle,
			Definition: json.RawMessage(`{"format":"agent_bundle","ontology_toml":"","draft":{"draft":{"items":{"Customer":{"card":{"rows":3}},"Plan":{}}}}}`)},
		&foundrymodel.OntologyVersion{ID: "ver-0", WorkspaceID: "ws-1", OntologyID: "ont-1", Format: foundrymodel.OntologyFormatAgentBundle,
			Definition: json.RawMessage(`{"format":"agent_bundle"}`)},
		&foundrymodel.OntologyVersion{ID: "ver-3", WorkspaceID: "ws-1", OntologyID: "ont-3", Format: foundrymodel.OntologyFormatAgentBundle,
			Definition: json.RawMessage(`{"format":"agent_bundle"}`)})
	general := access.SetRef{SetID: "set-general"}
	store.inputs = map[string]foundrymodel.FoundryLoadInputs{
		"ont-1": {LoadID: "load-1", VersionID: "ver-1", Inputs: access.Inputs{Tables: []access.TableRef{stripeCustomers}, Sets: []access.SetRef{general}}},
		"ont-3": {LoadID: "load-3", VersionID: "ver-3", Inputs: access.Inputs{Tables: []access.TableRef{pgPeople}}},
	}
	store.grants = []foundrymodel.FoundryAccessGrant{
		{ID: "g1", WorkspaceID: "ws-1", Principal: agentKey, Target: access.ConnectorTarget("conn-1")},
		{ID: "g2", WorkspaceID: "ws-1", Principal: agentKey, Target: access.DocumentSetTarget("set-general")},
		{ID: "g3", WorkspaceID: "ws-1", Principal: access.Principal{Kind: access.PrincipalUser, ID: "user-9"}, Target: access.DocumentSetTarget("set-general")},
		{ID: "g4", WorkspaceID: "ws-2", Principal: agentKey, Target: access.ConnectorTarget("conn-pg")},
	}
	s.deps.DB = store
	return s, store
}

// served serves s as a host does (ActorInterceptor, then Register) and
// returns a client and the call's context for each Actor.
func served(t *testing.T, s *Service) (*grpc.ClientConn, func(ports.Actor) context.Context) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.UnaryInterceptor(ActorInterceptor()))
	s.Register(srv)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, func(a ports.Actor) context.Context { return AttachActor(context.Background(), a) }
}

// servedMethods are Foundry's methods with their request and response types.
func servedMethods(t *testing.T) []protoreflect.MethodDescriptor {
	t.Helper()
	var out []protoreflect.MethodDescriptor
	for _, name := range []protoreflect.FullName{"helix.foundry.v1.FoundryService", "helix.foundry.v1.FoundryEventsService"} {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
		if err != nil {
			t.Fatal(err)
		}
		methods := d.(protoreflect.ServiceDescriptor).Methods()
		for i := range methods.Len() {
			out = append(out, methods.Get(i))
		}
	}
	return out
}

func fullMethod(m protoreflect.MethodDescriptor) string {
	return "/" + string(m.Parent().FullName()) + "/" + string(m.Name())
}

// newMessage is an empty message of the type d names.
func newMessage(t *testing.T, d protoreflect.MessageDescriptor) proto.Message {
	t.Helper()
	mt, err := protoregistry.GlobalTypes.FindMessageByName(d.FullName())
	if err != nil {
		t.Fatal(err)
	}
	return mt.New().Interface()
}

// Every method a grantee is admitted to names the workspace it is of, and
// an ontology's names the ontology, which the access check reads.
func TestGranteeRulesReadTheirRequest(t *testing.T) {
	for _, m := range servedMethods(t) {
		fields := m.Input().Fields()
		switch access.RuleOf(fullMethod(m)) {
		case access.Ontology:
			if fields.ByName("ontology_id") == nil {
				t.Errorf("%s admits a grantee to an ontology it doesn't name", m.Name())
			}
			fallthrough
		case access.Workspace:
			if fields.ByName("workspace_id") == nil {
				t.Errorf("%s admits a grantee to a workspace it doesn't name", m.Name())
			}
		}
	}
}

// A grantee reaches no handler of a method its grants can't admit, and the
// host's caller every one as before: Register serves each method through the
// check, after the server's own interceptors.
func TestGranteeDeniedMethods(t *testing.T) {
	s, _ := accessService(t)
	conn, as := served(t, s)
	denied := 0
	for _, m := range servedMethods(t) {
		if access.GranteeAdmits(fullMethod(m)) {
			continue
		}
		denied++
		req := newMessage(t, m.Input())
		if f := m.Input().Fields().ByName("workspace_id"); f != nil {
			req.ProtoReflect().Set(f, protoreflect.ValueOfString("ws-1"))
		}
		for _, who := range []ports.Actor{agent, member} {
			err := conn.Invoke(as(who), fullMethod(m), req, newMessage(t, m.Output()))
			if status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s as %+v: %v", m.Name(), who, err)
			}
		}
	}
	if denied < 60 {
		t.Fatalf("only %d methods refuse a grantee", denied)
	}
}

func TestGranteeAdmission(t *testing.T) {
	s, _ := accessService(t)
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)

	t.Run("only in its own workspace", func(t *testing.T) {
		_, err := client.ListFoundryOntologies(as(agent), &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-2"})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatal(err)
		}
		_, err = client.GetFoundryOntology(as(agent), &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-2", OntologyId: "ont-2"})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatal(err)
		}
	})
	t.Run("only on an ontology whose current load it covers", func(t *testing.T) {
		for _, id := range []string{"ont-3", "ont-404", ""} {
			_, err := client.GetFoundryOntology(as(agent), &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: id})
			if want := codes.NotFound; id == "" && status.Code(err) != codes.PermissionDenied || id != "" && status.Code(err) != want {
				t.Fatalf("%q: %v", id, err)
			}
		}
		got, err := client.GetFoundryOntology(as(agent), &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil {
			t.Fatal(err)
		}
		o := got.GetOntology()
		if o.GetName() != "Billing" || o.GetCurrentLoad().GetId() != "load-1" || o.GetTenantId() != "" || o.GetProjectId() != "" ||
			o.GetDatabaseName() != "" || o.GetCurrentVersionId() != "" || o.GetLastLoad() != nil || o.GetCounts() != nil {
			t.Fatalf("a grantee sees %+v", o)
		}
	})
	t.Run("the host's caller as before", func(t *testing.T) {
		got, err := client.GetFoundryOntology(as(person), &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-3"})
		if err != nil || got.GetOntology().GetTenantId() != "tenant-9" {
			t.Fatalf("%+v, %v", got, err)
		}
	})
	t.Run("a grantee's actor names one principal of one workspace, with nothing more", func(t *testing.T) {
		for name, a := range map[string]ports.Actor{
			"no principal":    {Source: ports.SourceGrantee, WorkspaceID: "ws-1"},
			"two principals":  {Source: ports.SourceGrantee, WorkspaceID: "ws-1", UserID: "u", ServiceCredentialID: "c"},
			"no workspace":    {Source: ports.SourceGrantee, UserID: "u"},
			"a query scope":   {Source: ports.SourceGrantee, WorkspaceID: "ws-1", UserID: "u", QueryRead: ports.AllProjects()},
			"a write scope":   {Source: ports.SourceGrantee, WorkspaceID: "ws-1", UserID: "u", QueryWrite: ports.SelectedProjects([]string{"p"})},
			"a send token":    {Source: ports.SourceGrantee, WorkspaceID: "ws-1", UserID: "u", EventSourceTokenID: "t"},
			"a user's key":    {Source: ports.SourceUser, WorkspaceID: "ws-1", UserID: "u", ServiceCredentialID: "c"},
			"a send token's":  {Source: ports.SourceEventSource, WorkspaceID: "ws-1", EventSourceTokenID: "t", ServiceCredentialID: "c"},
			"a user over MCP": {Source: ports.SourceMCP, WorkspaceID: "ws-1", UserID: "u", ServiceCredentialID: "c"},
		} {
			_, err := client.ListFoundryOntologies(as(a), &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1"})
			if status.Code(err) != codes.Unauthenticated {
				t.Errorf("%s: %v", name, err)
			}
		}
		key := ports.Actor{Source: ports.SourceMCP, WorkspaceID: "ws-1", ServiceCredentialID: "cred-1", QueryRead: ports.AllProjects()}
		if _, err := client.ListFoundryOntologies(as(key), &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1"}); err != nil {
			t.Fatalf("a key over MCP is the host's caller: %v", err)
		}
	})
}

// What each method a grantee may call answers it: its own workspace's
// ontologies it covers, their current loads and those loads' versions, and
// its own questions.
func TestGranteeAnswers(t *testing.T) {
	s, store := accessService(t)
	reader := &fakeFoundryRead{result: `{"as_of":"2026-10-30","results":[{"ok":{}}]}`}
	s.deps.FoundryRead = reader
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	ctx := as(agent)

	t.Run("ontologies", func(t *testing.T) {
		got, err := client.ListFoundryOntologies(ctx, &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1"})
		if err != nil || len(got.GetOntologies()) != 1 || got.GetOntologies()[0].GetId() != "ont-1" || got.GetOntologies()[0].GetTenantId() != "" {
			t.Fatalf("%+v, %v", got, err)
		}
		none, err := client.ListFoundryOntologies(as(ports.Actor{Source: ports.SourceGrantee, WorkspaceID: "ws-1", UserID: "nobody"}),
			&foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1"})
		if err != nil || len(none.GetOntologies()) != 0 {
			t.Fatalf("no grants: %+v, %v", none, err)
		}
	})
	t.Run("the load's version, without its cards", func(t *testing.T) {
		got, err := client.GetOntologyVersion(ctx, &foundryv1.GetOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "ver-1"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.GetVersion().GetEvidenceWithheld() || strings.Contains(fmt.Sprint(got.GetVersion().GetDefinition()), "card") {
			t.Fatalf("%v", got.GetVersion().GetDefinition())
		}
		for _, id := range []string{"ver-0", "ver-404"} {
			if _, err := client.GetOntologyVersion(ctx, &foundryv1.GetOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: id}); status.Code(err) != codes.NotFound {
				t.Fatalf("%s: %v", id, err)
			}
		}
		if _, err := client.GetCurrentOntologyVersion(ctx, &foundryv1.GetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("current version: %v", err)
		}
		host, err := client.GetOntologyVersion(as(person), &foundryv1.GetOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "ver-1"})
		if err != nil || !strings.Contains(fmt.Sprint(host.GetVersion().GetDefinition()), "card") {
			t.Fatalf("the host's caller keeps a draft's cards: %v", err)
		}
	})
	t.Run("the current load only, without its jobs", func(t *testing.T) {
		loads, err := client.ListFoundryLoads(ctx, &foundryv1.ListFoundryLoadsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || len(loads.GetLoads()) != 1 {
			t.Fatalf("%+v, %v", loads, err)
		}
		l := loads.GetLoads()[0]
		if l.GetId() != "load-1" || !l.GetCurrent() || l.GetJobs() != nil || l.GetUsdMicros() != 0 || l.GetSources() != nil {
			t.Fatalf("%+v", l)
		}
		one, err := client.GetFoundryLoad(ctx, &foundryv1.GetFoundryLoadRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "load-1"})
		if err != nil || one.GetLoad().GetId() != "load-1" {
			t.Fatalf("%+v, %v", one, err)
		}
		if _, err := client.GetFoundryLoad(ctx, &foundryv1.GetFoundryLoadRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "load-0"}); status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
	})
	t.Run("reads the current load only", func(t *testing.T) {
		reads := `[{"entity":{"id":{"type":"Customer","id":"c1"}}}]`
		if _, err := client.ReadFoundryGraph(ctx, &foundryv1.ReadFoundryGraphRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", LoadId: "load-0",
			ReadsJson: reads}); status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
		if _, err := client.ReadFoundryGraph(ctx, &foundryv1.ReadFoundryGraphRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", LoadId: "load-1",
			ReadsJson: reads}); err != nil {
			t.Fatal(err)
		}
		if len(reader.requests) != 1 || reader.requests[0].GetLoadId() != "load-1" {
			t.Fatalf("read %+v", reader.requests)
		}
	})
	t.Run("asks the current load, in its own threads", func(t *testing.T) {
		hostAsked, err := client.AskFoundryQuestion(as(person), &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			Question: "How many customers?", ClientToken: "host-1"})
		if err != nil {
			t.Fatal(err)
		}
		mine, err := client.AskFoundryQuestion(ctx, &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			Question: "How many customers?", ClientToken: "agent-1"})
		if err != nil {
			t.Fatal(err)
		}
		stored := store.questions[len(store.questions)-1]
		if stored.Grantee == nil || *stored.Grantee != agentKey || stored.LoadID != "load-1" || stored.AskedByUserID != nil {
			t.Fatalf("stored %+v", stored)
		}
		for name, req := range map[string]*foundryv1.AskFoundryQuestionRequest{
			"another load":      {LoadId: "load-0", ClientToken: "agent-2"},
			"another's thread":  {ThreadId: hostAsked.GetQuestion().GetId(), ClientToken: "agent-3"},
			"another's token":   {ClientToken: "host-1"},
			"another ontology":  {OntologyId: "ont-3", ClientToken: "agent-4"},
			"another workspace": {WorkspaceId: "ws-2", OntologyId: "ont-2", ClientToken: "agent-5"},
		} {
			req.Question = "How many customers?"
			if req.WorkspaceId == "" {
				req.WorkspaceId = "ws-1"
			}
			if req.OntologyId == "" {
				req.OntologyId = "ont-1"
			}
			_, err := client.AskFoundryQuestion(ctx, req)
			if code := status.Code(err); code != codes.NotFound && code != codes.InvalidArgument && code != codes.PermissionDenied {
				t.Errorf("%s: %v", name, err)
			}
		}
		again, err := client.AskFoundryQuestion(ctx, &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			Question: "And then?", ClientToken: "agent-6", ThreadId: mine.GetQuestion().GetId()})
		if err != nil || again.GetQuestion().GetThreadId() != mine.GetQuestion().GetId() {
			t.Fatalf("its own thread: %+v, %v", again, err)
		}

		list, err := client.ListFoundryQuestions(ctx, &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || len(list.GetQuestions()) != 2 {
			t.Fatalf("its own questions: %+v, %v", list, err)
		}
		all, err := client.ListFoundryQuestions(as(person), &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || len(all.GetQuestions()) != 3 {
			t.Fatalf("the host's caller sees every question: %+v, %v", all, err)
		}
		theirs := hostAsked.GetQuestion().GetId()
		if _, err := client.GetFoundryQuestion(ctx, &foundryv1.GetFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: theirs}); status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
		if _, err := client.ListFoundryQuestionEvents(ctx, &foundryv1.ListFoundryQuestionEventsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: theirs}); status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
		if _, err := client.CancelFoundryQuestion(ctx, &foundryv1.CancelFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: theirs}); status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
		if _, err := client.GetFoundryQuestion(ctx, &foundryv1.GetFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: mine.GetQuestion().GetId()}); err != nil {
			t.Fatal(err)
		}
		if got, err := client.CancelFoundryQuestion(ctx, &foundryv1.CancelFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: mine.GetQuestion().GetId()}); err != nil || got.GetQuestion().GetStatus() != "cancelled" {
			t.Fatalf("%+v, %v", got, err)
		}
		// An answer about another load reads while its grants cover that load,
		// and not otherwise, whatever they cover of the current one.
		older := *store.questions[len(store.questions)-1]
		older.ID, older.LoadID = "older-covered", "load-0"
		uncovered := older
		uncovered.ID, uncovered.LoadID = "older-uncovered", "load-other"
		gone := older
		gone.ID, gone.LoadID = "older-gone", "load-gone"
		store.questions = append(store.questions, &older, &uncovered, &gone)
		store.olderLoads = map[string]foundrymodel.FoundryLoadInputs{
			"load-0":     {LoadID: "load-0", Inputs: store.inputs["ont-1"].Inputs},
			"load-other": {LoadID: "load-other", Inputs: access.Inputs{Sets: []access.SetRef{{SetID: "set-no-one-holds"}}}},
		}
		get := func(id string) error {
			_, err := client.GetFoundryQuestion(ctx, &foundryv1.GetFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: id})
			return err
		}
		if err := get("older-covered"); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"older-uncovered", "older-gone"} {
			if err := get(id); status.Code(err) != codes.NotFound {
				t.Fatalf("%s: %v", id, err)
			}
			if _, err := client.ListFoundryQuestionEvents(ctx, &foundryv1.ListFoundryQuestionEventsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: id}); status.Code(err) != codes.NotFound {
				t.Fatalf("%s's events: %v", id, err)
			}
			if _, err := client.CancelFoundryQuestion(ctx, &foundryv1.CancelFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: id}); status.Code(err) != codes.NotFound {
				t.Fatalf("%s's cancel: %v", id, err)
			}
		}
		// Its own budget, not what the workspace spends.
		store.granteeSpend = map[access.Principal]foundrymodel.FoundryQuestionSpend{agentKey: {SpentMicros: 7, ReservedMicros: 3}}
		listed, err := client.ListFoundryQuestions(ctx, &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, q := range listed.GetQuestions() {
			ids = append(ids, q.GetId())
		}
		if !slices.Contains(ids, "older-covered") || slices.Contains(ids, "older-uncovered") || slices.Contains(ids, "older-gone") {
			t.Fatalf("listed %v", ids)
		}
		ask := s.deps.Cfg.Foundry.Ask
		if b := listed.GetBudget(); b.GetSpentUsdMicros() != 7 || b.GetReservedUsdMicros() != 3 ||
			b.GetMonthlyUsdMicros() != ask.GranteeMonthlyMicros() || b.GetHourlyLimit() != int32(ask.GranteePerHour) {
			t.Fatalf("budget %+v", b)
		}
		hostList, err := client.ListFoundryQuestions(as(person), &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || hostList.GetBudget().GetMonthlyUsdMicros() != ask.MonthlyMicros() || len(hostList.GetQuestions()) != 6 {
			t.Fatalf("the host's caller keeps every question and the workspace's budget: %+v, %v", hostList.GetBudget(), err)
		}
		// Asking again with the token of a question it can no longer read
		// doesn't read it either.
		token := "older"
		uncovered.ClientToken = &token
		if _, err := client.AskFoundryQuestion(ctx, &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			Question: uncovered.Question, ClientToken: token}); status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
		// Its grants taken away, its questions go with the ontology.
		store.grants = store.grants[2:]
		if _, err := client.GetFoundryQuestion(ctx, &foundryv1.GetFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: mine.GetQuestion().GetId()}); status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
	})
}

// A grantee that holds only some of a load's sources reads that part: the
// version as the agent projects it, and reads and questions told what it holds.
func TestGranteePartial(t *testing.T) {
	s, store := accessService(t)
	const view = `{"as_of":"2026-10-30","results":[{"ok":{"kind":"view","ontology_toml":"projected","mappings_toml":"held mappings","partial":["Customer"]}}]}`
	reader := &fakeFoundryRead{result: view}
	s.deps.FoundryRead = reader
	mixed := "load-4"
	store.ontologies["ont-4"] = &foundrymodel.FoundryOntology{ID: "ont-4", WorkspaceID: "ws-1", Name: "Mixed", ProjectID: "proj-1", TenantID: "tenant-4",
		CurrentLoadID: &mixed, CreatedAt: time.Now()}
	store.loads = append(store.loads, &foundrymodel.FoundryLoad{ID: "load-4", OntologyID: "ont-4", VersionID: "ver-4", Version: 4, Status: foundrymodel.FoundryLoadReady})
	generation := "gen-4"
	store.versions = append(store.versions, &foundrymodel.OntologyVersion{ID: "ver-4", WorkspaceID: "ws-1", OntologyID: "ont-4",
		Format: foundrymodel.OntologyFormatAgentBundle, GenerationID: &generation,
		Definition: json.RawMessage(`{"format":"agent_bundle","ontology_toml":"whole","mappings_toml":"every mapping","draft":{"draft":{"items":{"secret":{}}}},"questions":[{"id":"Q1"}],"summary":"what it found","usage":{"usd":1}}`)})
	general, hr := access.SetRef{SetID: "set-general"}, access.SetRef{SetID: "set-hr"}
	store.inputs["ont-4"] = foundrymodel.FoundryLoadInputs{LoadID: "load-4", VersionID: "ver-4",
		Inputs: access.Inputs{Tables: []access.TableRef{stripeCustomers, pgPeople}, Sets: []access.SetRef{general, hr}}}
	store.holdings = map[string]access.Holdings{"load-4": {
		Tables:    []access.TableRef{stripeCustomers, pgPeople},
		Files:     []access.DocumentRef{{ID: "file-general", Set: general}, {ID: "file-hr", Set: hr}},
		Documents: []access.DocumentRef{{ID: "doc-general", Set: general}, {ID: "doc-hr", Set: hr}},
		Decisions: []access.PropDecision{{Type: "Customer", Prop: "plan", Sets: []access.SetRef{hr}, Recorded: true}},
	}}
	store.sets = append(store.sets, &foundrymodel.FoundryDocumentSet{ID: "set-general", WorkspaceID: "ws-1"}, &foundrymodel.FoundryDocumentSet{ID: "set-hr", WorkspaceID: "ws-1"})
	store.documents = append(store.documents,
		&foundrymodel.FoundryDocument{ID: "doc-general", WorkspaceID: "ws-1", SetID: "set-general", Name: "general.md", MediaType: foundrymodel.FoundryMediaMarkdown,
			Status: foundrymodel.FoundryDocumentUploaded, S3Key: "docs/general"},
		&foundrymodel.FoundryDocument{ID: "doc-hr", WorkspaceID: "ws-1", SetID: "set-hr", Name: "hr.md", MediaType: foundrymodel.FoundryMediaMarkdown,
			Status: foundrymodel.FoundryDocumentUploaded, S3Key: "docs/hr"})
	store.members["load-4"] = []string{"doc-general", "doc-hr"}
	store.holding = map[string][]string{"doc-general": {"ont-4"}, "doc-hr": {"ont-4"}}
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	ctx := as(agent)

	t.Run("listed as part", func(t *testing.T) {
		got, err := client.ListFoundryOntologies(ctx, &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1"})
		if err != nil || len(got.GetOntologies()) != 2 {
			t.Fatalf("%+v, %v", got, err)
		}
		partial := map[string]bool{}
		for _, o := range got.GetOntologies() {
			partial[o.GetId()] = o.GetPartialAccess()
		}
		if !partial["ont-4"] || partial["ont-1"] {
			t.Fatalf("%v", partial)
		}
		my, err := client.GetMyFoundryAccess(ctx, &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
		if err != nil || !slices.Equal(my.GetOntologyIds(), []string{"ont-1", "ont-4"}) || !slices.Equal(my.GetPartialOntologyIds(), []string{"ont-4"}) {
			t.Fatalf("%+v, %v", my, err)
		}
	})
	t.Run("the version as it sees it", func(t *testing.T) {
		got, err := client.GetOntologyVersion(ctx, &foundryv1.GetOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-4", Id: "ver-4"})
		if err != nil {
			t.Fatal(err)
		}
		fields := got.GetVersion().GetDefinition().GetFields()
		if fields["ontology_toml"].GetStringValue() != "projected" || fields["mappings_toml"].GetStringValue() != "held mappings" ||
			fields["draft"] != nil || fields["questions"] != nil || fields["summary"] != nil || fields["usage"] != nil || fields["format"].GetStringValue() != "agent_bundle" ||
			got.GetVersion().GetGenerationId() != "" || len(got.GetVersion().GetDocumentSetIds()) != 0 {
			t.Fatalf("%+v", got.GetVersion())
		}
		read := reader.requests[len(reader.requests)-1]
		if read.GetReadsJson() != `[{"view":{}}]` || read.GetOntologyToml() != "whole" || read.GetMappingsToml() != "every mapping" {
			t.Fatalf("%+v", read)
		}
		// What it reads says what it holds, and what it doesn't is left out.
		want := `{"sources":["` + ontologyagent.TableSourceID("conn-1", "customers") + `","file-general"],"documents":{"only":["doc-general"]},"sets":["set-general"],"enrichment":false,"withheld":[{"type":"Customer","prop":"plan"}]}`
		if read.GetVisibilityJson() != want {
			t.Fatalf("visibility %s, want %s", read.GetVisibilityJson(), want)
		}
		// One whose agent answers nothing readable gives nothing away.
		reader.result = `{"results":[]}`
		if _, err := client.GetOntologyVersion(ctx, &foundryv1.GetOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-4", Id: "ver-4"}); status.Code(err) != codes.Internal {
			t.Fatal(err)
		}
		reader.result = view
	})
	t.Run("reads told what it holds", func(t *testing.T) {
		before := len(reader.requests)
		reader.result = `{"as_of":"2026-10-30","results":[{"ok":{}}]}`
		defer func() { reader.result = view }()
		if _, err := client.ReadFoundryGraph(ctx, &foundryv1.ReadFoundryGraphRequest{WorkspaceId: "ws-1", OntologyId: "ont-4", LoadId: "load-4", ReadsJson: `[{"query":{}}]`}); err != nil {
			t.Fatal(err)
		}
		read := reader.requests[before]
		if read.GetVisibilityJson() == "" || read.GetMappingsToml() != "every mapping" {
			t.Fatalf("%+v", read)
		}
		// A load it holds all of is read whole, as it was.
		if _, err := client.ReadFoundryGraph(ctx, &foundryv1.ReadFoundryGraphRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", LoadId: "load-1", ReadsJson: `[{"query":{}}]`}); err != nil {
			t.Fatal(err)
		}
		if last := reader.requests[len(reader.requests)-1]; last.GetVisibilityJson() != "" || last.GetMappingsToml() != "" {
			t.Fatalf("%+v", last)
		}
	})
	t.Run("only documents it holds", func(t *testing.T) {
		list, err := client.ListFoundryOntologyDocuments(ctx, &foundryv1.ListFoundryOntologyDocumentsRequest{WorkspaceId: "ws-1", OntologyId: "ont-4"})
		if err != nil || len(list.GetDocuments()) != 1 || list.GetDocuments()[0].GetId() != "doc-general" {
			t.Fatalf("%+v, %v", list, err)
		}
		if _, err := client.GetFoundryOntologyDocument(ctx, &foundryv1.GetFoundryOntologyDocumentRequest{WorkspaceId: "ws-1", OntologyId: "ont-4", Id: "doc-general"}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.GetFoundryOntologyDocument(ctx, &foundryv1.GetFoundryOntologyDocumentRequest{WorkspaceId: "ws-1", OntologyId: "ont-4", Id: "doc-hr"}); status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
		if got := store.heldSets; got == nil || !slices.Contains(*got, "set-general") || slices.Contains(*got, "set-hr") {
			t.Fatalf("listed by sets %v", got)
		}
		if _, err := client.GetFoundryDocumentContent(ctx, &foundryv1.GetFoundryDocumentContentRequest{WorkspaceId: "ws-1", Id: "doc-hr"}); status.Code(err) != codes.NotFound {
			t.Fatalf("the text of a document it doesn't hold: %v", err)
		}
		// Which other document one duplicates may be one it doesn't hold.
		other := "doc-hr"
		proto := ontologyDocumentProto(access.WithCaller(context.Background(), access.Grantee{WorkspaceID: "ws-1", Principal: agentKey}),
			&foundrymodel.FoundryDocument{ID: "doc-general"}, &foundrymodel.FoundryDocumentExtraction{DuplicateOf: &other})
		if proto.GetExtraction().DuplicateOf != nil {
			t.Fatalf("%+v", proto.GetExtraction())
		}
		host := ontologyDocumentProto(context.Background(), &foundrymodel.FoundryDocument{ID: "doc-general"}, &foundrymodel.FoundryDocumentExtraction{DuplicateOf: &other})
		if host.GetExtraction().GetDuplicateOf() != other {
			t.Fatalf("the host's caller: %+v", host.GetExtraction())
		}
	})
	t.Run("a question it asks reads from the start", func(t *testing.T) {
		asked, err := client.AskFoundryQuestion(ctx, &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-4", Question: "How many?", ClientToken: "part-1"})
		if err != nil {
			t.Fatal(err)
		}
		stored := store.questions[len(store.questions)-1]
		if stored.Seen == nil || !slices.Contains(stored.Seen.Sets, general) || slices.Contains(stored.Seen.Sets, hr) {
			t.Fatalf("seen %+v", stored.Seen)
		}
		if _, err := client.GetFoundryQuestion(ctx, &foundryv1.GetFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-4", Id: asked.GetQuestion().GetId()}); err != nil {
			t.Fatal(err)
		}
		again, err := client.AskFoundryQuestion(ctx, &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-4", Question: "How many?", ClientToken: "part-1"})
		if err != nil || again.GetQuestion().GetId() != asked.GetQuestion().GetId() {
			t.Fatalf("%+v, %v", again, err)
		}
	})
	t.Run("an answer reads while its grants hold what it read", func(t *testing.T) {
		token := "seen"
		q := &foundrymodel.FoundryQuestion{ID: "q-seen", WorkspaceID: "ws-1", OntologyID: "ont-4", LoadID: "load-4", VersionID: "ver-4", ThreadID: "q-seen",
			Question: "x", ClientToken: &token, Grantee: &agentKey, Seen: &access.Inputs{Tables: []access.TableRef{stripeCustomers}, Sets: []access.SetRef{general}},
			Status: foundrymodel.FoundryQuestionAnswered}
		store.questions = append(store.questions, q)
		get := func() error {
			_, err := client.GetFoundryQuestion(ctx, &foundryv1.GetFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-4", Id: "q-seen"})
			return err
		}
		if err := get(); err != nil {
			t.Fatal(err)
		}
		q.Seen.Sets = []access.SetRef{hr}
		if err := get(); status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
	})
	t.Run("owners see who reads part", func(t *testing.T) {
		list, err := client.ListFoundryAccess(as(person), &foundryv1.ListFoundryAccessRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range list.GetOntologies() {
			if o.GetOntologyId() == "ont-4" && (len(o.GetReaders()) != 0 || len(o.GetPartialReaders()) != 2) {
				t.Fatalf("%+v", o)
			}
		}
	})
	t.Run("a load it can't tell the holdings of", func(t *testing.T) {
		delete(store.holdings, "load-4")
		if _, err := client.GetOntologyVersion(ctx, &foundryv1.GetOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-4", Id: "ver-4"}); status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
	})
	t.Run("nothing of a load whose holdings can't be told at all", func(t *testing.T) {
		l := store.inputs["ont-4"]
		l.Inputs.Unknown = true
		store.inputs["ont-4"] = l
		if _, err := client.GetFoundryOntology(ctx, &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-4"}); status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
	})
}

// A grantee names documents only by id, and only those in sets it holds.
func TestGranteeDocuments(t *testing.T) {
	s, store := accessService(t)
	store.sets = append(store.sets, &foundrymodel.FoundryDocumentSet{ID: "set-hr", WorkspaceID: "ws-1", Name: "HR"},
		&foundrymodel.FoundryDocumentSet{ID: "set-notes", WorkspaceID: "ws-1", Name: "Notes", Owner: &foundrymodel.FoundryDocumentSetOwner{ConnectorID: "conn-1"}})
	store.documents = append(store.documents,
		&foundrymodel.FoundryDocument{ID: "doc-general", WorkspaceID: "ws-1", SetID: "set-general", Name: "contract.md", MediaType: foundrymodel.FoundryMediaMarkdown},
		&foundrymodel.FoundryDocument{ID: "doc-hr", WorkspaceID: "ws-1", SetID: "set-hr", Name: "salaries.md", MediaType: foundrymodel.FoundryMediaMarkdown},
		&foundrymodel.FoundryDocument{ID: "doc-note", WorkspaceID: "ws-1", SetID: "set-notes", Name: "standup.md", MediaType: foundrymodel.FoundryMediaMarkdown})
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	if _, err := client.ListFoundryDocuments(as(agent), &foundryv1.ListFoundryDocumentsRequest{WorkspaceId: "ws-1"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("listing a set: %v", err)
	}
	got, err := client.ListFoundryDocuments(as(agent), &foundryv1.ListFoundryDocumentsRequest{WorkspaceId: "ws-1", Ids: []string{"doc-general", "doc-hr", "doc-note"}})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range got.GetDocuments() {
		names = append(names, d.GetId())
	}
	if !slices.Equal(names, []string{"doc-general", "doc-note"}) {
		t.Fatalf("named %v: General, and Stripe's own set through its connector", names)
	}
	host, err := client.ListFoundryDocuments(as(person), &foundryv1.ListFoundryDocumentsRequest{WorkspaceId: "ws-1", Ids: []string{"doc-hr"}})
	if err != nil || len(host.GetDocuments()) != 1 {
		t.Fatalf("the host's caller: %+v, %v", host, err)
	}
}

func TestGetMyFoundryAccess(t *testing.T) {
	s, _ := accessService(t)
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	for name, tc := range map[string]struct {
		who                ports.Actor
		grantee, hasGrants bool
		ontologies         []string
	}{
		"an agent's key, as a grantee":         {agent, true, true, []string{"ont-1"}},
		"a member, as a grantee":               {member, true, true, []string{"ont-1"}},
		"a grantee with nothing":               {ports.Actor{Source: ports.SourceGrantee, UserID: "nobody", WorkspaceID: "ws-1"}, true, false, nil},
		"the host's member, holding grants":    {ports.Actor{Source: ports.SourceUser, UserID: "user-9", WorkspaceID: "ws-1"}, false, true, []string{"ont-1"}},
		"the host's key, holding grants":       {ports.Actor{Source: ports.SourceMCP, ServiceCredentialID: "cred-1", WorkspaceID: "ws-1"}, false, true, []string{"ont-1"}},
		"the host's caller, holding no grants": {person, false, false, nil},
		"no user or key":                       {ports.Actor{Source: ports.SourceEventSource, EventSourceTokenID: "t", WorkspaceID: "ws-1"}, false, false, nil},
	} {
		got, err := client.GetMyFoundryAccess(as(tc.who), &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
		if err != nil || got.GetGrantee() != tc.grantee || got.GetHasGrants() != tc.hasGrants || !slices.Equal(got.GetOntologyIds(), tc.ontologies) {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	if _, err := client.GetMyFoundryAccess(as(person), &foundryv1.GetMyFoundryAccessRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
}

func TestListAndSetFoundryAccess(t *testing.T) {
	s, store := accessService(t)
	ctx := ports.WithActor(context.Background(), person)
	got, err := s.ListFoundryAccess(ctx, &foundryv1.ListFoundryAccessRequest{WorkspaceId: "ws-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetGrants()) != 3 || len(got.GetOntologies()) != 2 {
		t.Fatalf("%+v", got)
	}
	billing := got.GetOntologies()[0]
	if billing.GetOntologyId() != "ont-1" || billing.GetLoadId() != "load-1" || len(billing.GetReaders()) != 1 ||
		billing.GetReaders()[0].GetKind() != foundryv1.FoundryPrincipal_KIND_SERVICE_CREDENTIAL ||
		billing.GetInputs().GetTables()[0].GetTable() != "customers" || billing.GetInputs().GetDocumentSets()[0].GetDocumentSetId() != "set-general" {
		t.Fatalf("billing %+v", billing)
	}
	if payroll := got.GetOntologies()[1]; len(payroll.GetReaders()) != 0 {
		t.Fatalf("payroll %+v", payroll)
	}

	team := &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_TEAM, Id: "team-1"}
	// Grants go only to a team the workspace has, or a member of it, who would
	// otherwise hold them on joining.
	one := []*foundryv1.FoundryAccessTarget{{Kind: foundryv1.FoundryAccessTarget_KIND_CONNECTOR, ConnectorId: "conn-1"}}
	if _, err := s.SetFoundryAccessGrants(ctx, &foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: team, Targets: one}); status.Code(err) != codes.NotFound {
		t.Fatalf("no such team: %v", err)
	}
	store.teams = append(store.teams, foundrymodel.FoundryTeam{ID: "team-1", WorkspaceID: "ws-1", Name: "One"})
	s.deps.People = accessPeople{}
	gone := &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_USER, Id: "gone"}
	if _, err := s.SetFoundryAccessGrants(ctx, &foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: gone, Targets: one}); status.Code(err) != codes.NotFound {
		t.Fatalf("no such member: %v", err)
	}
	if _, err := s.SetFoundryAccessGrants(ctx, &foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: gone}); err != nil {
		t.Fatalf("clearing what someone gone holds: %v", err)
	}
	set, err := s.SetFoundryAccessGrants(ctx, &foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: team, Targets: []*foundryv1.FoundryAccessTarget{
		{Kind: foundryv1.FoundryAccessTarget_KIND_CONNECTOR, ConnectorId: "conn-1"},
		{Kind: foundryv1.FoundryAccessTarget_KIND_TABLE, ConnectorId: "conn-pg", Table: "people"},
		{Kind: foundryv1.FoundryAccessTarget_KIND_CONNECTOR, ConnectorId: "conn-1"},
		{Kind: foundryv1.FoundryAccessTarget_KIND_ENRICHMENT},
	}})
	if err != nil || len(set.GetGrants()) != 3 || set.GetGrants()[0].GetCreatedByUserId() != "user-1" {
		t.Fatalf("a repeated target is one grant: %+v, %v", set, err)
	}
	after, _ := s.ListFoundryAccess(ctx, &foundryv1.ListFoundryAccessRequest{WorkspaceId: "ws-1"})
	if readers := after.GetOntologies()[1].GetReaders(); len(readers) != 1 || readers[0].GetId() != "team-1" {
		t.Fatalf("the team reads Payroll: %+v", readers)
	}
	cleared, err := s.SetFoundryAccessGrants(ctx, &foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: team})
	if err != nil || len(cleared.GetGrants()) != 0 || len(store.grants) != 4 {
		t.Fatalf("none takes them all away: %+v, %v", cleared, err)
	}

	tooMany := make([]*foundryv1.FoundryAccessTarget, maxGrantsPerPrincipal+1)
	for i := range tooMany {
		tooMany[i] = &foundryv1.FoundryAccessTarget{Kind: foundryv1.FoundryAccessTarget_KIND_TABLE, ConnectorId: "conn-1", Table: fmt.Sprint(i)}
	}
	for name, tc := range map[string]struct {
		req  *foundryv1.SetFoundryAccessGrantsRequest
		code codes.Code
	}{
		"no workspace":        {&foundryv1.SetFoundryAccessGrantsRequest{Principal: team}, codes.InvalidArgument},
		"no principal":        {&foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1"}, codes.InvalidArgument},
		"a principal no kind": {&foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: &foundryv1.FoundryPrincipal{Id: "x"}}, codes.InvalidArgument},
		"a principal no id": {&foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1",
			Principal: &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_USER}}, codes.InvalidArgument},
		"a target of no kind": {&foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: team,
			Targets: []*foundryv1.FoundryAccessTarget{{ConnectorId: "conn-1"}}}, codes.InvalidArgument},
		"a table without its connector": {&foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: team,
			Targets: []*foundryv1.FoundryAccessTarget{{Kind: foundryv1.FoundryAccessTarget_KIND_TABLE, Table: "people"}}}, codes.InvalidArgument},
		"too many": {&foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: team, Targets: tooMany}, codes.InvalidArgument},
		"a connector not the workspace's": {&foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: team,
			Targets: []*foundryv1.FoundryAccessTarget{{Kind: foundryv1.FoundryAccessTarget_KIND_CONNECTOR, ConnectorId: "missing"}}}, codes.NotFound},
	} {
		if _, err := s.SetFoundryAccessGrants(ctx, tc.req); status.Code(err) != tc.code {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.ListFoundryAccess(ctx, &foundryv1.ListFoundryAccessRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
}

func TestPreviewFoundryAccessChange(t *testing.T) {
	s, store := accessService(t)
	ctx := ports.WithActor(context.Background(), person)
	preview := func(change *foundryv1.FoundryAccessChange) ([]string, error) {
		got, err := s.PreviewFoundryAccessChange(ctx, &foundryv1.PreviewFoundryAccessChangeRequest{WorkspaceId: "ws-1", Change: change})
		var out []string
		for _, l := range got.GetLosses() {
			out = append(out, l.GetPrincipal().GetId()+" "+l.GetOntologyId())
		}
		return out, err
	}
	store.versionInputs = map[string]access.Inputs{
		"ver-2":  {Tables: []access.TableRef{stripeCustomers, pgPeople}},
		"ver-1b": {Tables: []access.TableRef{stripeCustomers}},
	}
	store.versions = append(store.versions,
		&foundrymodel.OntologyVersion{ID: "ver-2", OntologyID: "ont-1", Definition: json.RawMessage(`{"mappings_toml":""}`)},
		&foundrymodel.OntologyVersion{ID: "ver-1b", OntologyID: "ont-1", Definition: json.RawMessage(`{"mappings_toml":"[enrichments.people]\nentity = \"Person\"\n"}`)})
	for name, tc := range map[string]struct {
		change *foundryv1.FoundryAccessChange
		losses []string
	}{
		"a version reading a table the agent lacks": {&foundryv1.FoundryAccessChange{Change: &foundryv1.FoundryAccessChange_LoadVersion_{
			LoadVersion: &foundryv1.FoundryAccessChange_LoadVersion{OntologyId: "ont-1", VersionId: "ver-2"}}}, []string{"cred-1 ont-1", "user-9 ont-1"}},
		"a version that enriches people": {&foundryv1.FoundryAccessChange{Change: &foundryv1.FoundryAccessChange_LoadVersion_{
			LoadVersion: &foundryv1.FoundryAccessChange_LoadVersion{OntologyId: "ont-1", VersionId: "ver-1b"}}}, []string{"cred-1 ont-1", "user-9 ont-1"}},
		"deleting the connector the agent holds": {&foundryv1.FoundryAccessChange{Change: &foundryv1.FoundryAccessChange_DeleteConnector{
			DeleteConnector: "conn-1"}}, []string{"cred-1 ont-1"}},
		"moving a document of Billing's into a set it lacks": {&foundryv1.FoundryAccessChange{Change: &foundryv1.FoundryAccessChange_MoveDocument_{
			MoveDocument: &foundryv1.FoundryAccessChange_MoveDocument{DocumentId: "doc-1", DocumentSetId: "set-hr"}}}, []string{"cred-1 ont-1"}},
		"moving a document no load holds": {&foundryv1.FoundryAccessChange{Change: &foundryv1.FoundryAccessChange_MoveDocument_{
			MoveDocument: &foundryv1.FoundryAccessChange_MoveDocument{DocumentId: "doc-loose", DocumentSetId: "set-hr"}}}, nil},
	} {
		store.holding = map[string][]string{"doc-1": {"ont-1"}}
		store.sets = append(store.sets, &foundrymodel.FoundryDocumentSet{ID: "set-hr", WorkspaceID: "ws-1", Name: "HR"})
		got, err := preview(tc.change)
		slices.Sort(got)
		if err != nil || !slices.Equal(got, tc.losses) {
			t.Errorf("%s: %v, %v", name, got, err)
		}
	}
	for name, change := range map[string]*foundryv1.FoundryAccessChange{
		"no change":       {},
		"an unknown one":  {Change: &foundryv1.FoundryAccessChange_DeleteConnector{DeleteConnector: "conn-404"}},
		"an unknown set":  {Change: &foundryv1.FoundryAccessChange_MoveDocument_{MoveDocument: &foundryv1.FoundryAccessChange_MoveDocument{DocumentSetId: "set-404"}}},
		"another version": {Change: &foundryv1.FoundryAccessChange_LoadVersion_{LoadVersion: &foundryv1.FoundryAccessChange_LoadVersion{OntologyId: "ont-1", VersionId: "ver-404"}}},
	} {
		if _, err := preview(change); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A partial reader left with nothing is a loss that keeps no part; one left with some isn't.
	store.grants = append(store.grants, foundrymodel.FoundryAccessGrant{ID: "g5", WorkspaceID: "ws-1",
		Principal: access.Principal{Kind: access.PrincipalUser, ID: "user-part"}, Target: access.ConnectorTarget("conn-1")})
	lost, err := s.PreviewFoundryAccessChange(ctx, &foundryv1.PreviewFoundryAccessChangeRequest{WorkspaceId: "ws-1",
		Change: &foundryv1.FoundryAccessChange{Change: &foundryv1.FoundryAccessChange_DeleteConnector{DeleteConnector: "conn-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string]bool{}
	for _, l := range lost.GetLosses() {
		parts[l.GetPrincipal().GetId()] = l.GetKeepsPart()
	}
	if keeps, listed := parts["user-part"]; !listed || keeps {
		t.Fatalf("a partial reader left with nothing: %v", parts)
	}
	if _, listed := parts["user-9"]; listed {
		t.Fatalf("a partial reader who keeps its part: %v", parts)
	}
	store.grants = nil
	if got, err := preview(&foundryv1.FoundryAccessChange{}); err != nil || got != nil {
		t.Fatalf("a workspace that grants nothing loses nothing: %v, %v", got, err)
	}
}

// accessPeople are ws-1's members, by user.
type accessPeople map[string]ports.Member

func (accessPeople) MayManageAI(context.Context, string, string) bool    { return false }
func (accessPeople) DisplayName(context.Context, string) (string, error) { return "", nil }
func (p accessPeople) Member(_ context.Context, ws, userID string) (ports.Member, error) {
	if ws != "ws-1" {
		return ports.Member{}, nil
	}
	return p[userID], nil
}

// A restricted member is a grantee on every call while their role is
// member, holding their teams' grants too.
func TestRestrictedMembers(t *testing.T) {
	s, store := accessService(t)
	people := accessPeople{"user-9": {Role: ports.RoleMember}, "user-1": {Role: ports.RoleOwner, RawAccess: true}}
	s.deps.People = people
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	eve := ports.Actor{Source: ports.SourceUser, UserID: "user-9", WorkspaceID: "ws-1", QueryRead: ports.SelectedProjects([]string{"proj-1"})}
	viaMCP := eve
	viaMCP.Source = ports.SourceMCP

	list := func(t *testing.T, who ports.Actor) []string {
		t.Helper()
		got, err := client.ListFoundryOntologies(as(who), &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, o := range got.GetOntologies() {
			ids = append(ids, o.GetId())
		}
		slices.Sort(ids)
		return ids
	}
	if got := list(t, eve); !slices.Equal(got, []string{"ont-1", "ont-3"}) {
		t.Fatalf("unrestricted, the host's caller: %v", got)
	}
	if _, err := s.SetFoundryMemberRestricted(ports.WithActor(context.Background(), person),
		&foundryv1.SetFoundryMemberRestrictedRequest{WorkspaceId: "ws-1", UserId: "user-9", Restricted: true}); err != nil {
		t.Fatal(err)
	}
	for _, who := range []ports.Actor{eve, viaMCP} {
		if got := list(t, who); !slices.Equal(got, []string{"ont-1"}) {
			t.Fatalf("restricted, holding General alone, reads the part of Billing it holds: %v", got)
		}
		if _, err := client.ListConnectors(as(who), &foundryv1.ListConnectorsRequest{WorkspaceId: "ws-1"}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("restricted, the Data area: %v", err)
		}
	}
	elsewhere, err := client.ListFoundryOntologies(as(ports.Actor{Source: ports.SourceUser, UserID: "user-9", WorkspaceID: "ws-2"}),
		&foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-2"})
	if err != nil || len(elsewhere.GetOntologies()) != 1 || elsewhere.GetOntologies()[0].GetTenantId() == "" {
		t.Fatalf("restricted in ws-1 only: another workspace's call reads as the host's caller: %+v, %v", elsewhere, err)
	}

	t.Run("holding a team's grants", func(t *testing.T) {
		store.teams = []foundrymodel.FoundryTeam{{ID: "team-1", WorkspaceID: "ws-1", Name: "Billing", MemberUserIDs: []string{"user-9"}}}
		store.grants = append(store.grants, foundrymodel.FoundryAccessGrant{WorkspaceID: "ws-1", Principal: access.Principal{Kind: access.PrincipalTeam, ID: "team-1"},
			Target: access.ConnectorTarget("conn-1")})
		if got := list(t, eve); !slices.Equal(got, []string{"ont-1"}) {
			t.Fatalf("with the team's Stripe: %v", got)
		}
		mine, err := client.GetMyFoundryAccess(as(eve), &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
		if err != nil || !mine.GetGrantee() || !slices.Equal(mine.GetOntologyIds(), []string{"ont-1"}) {
			t.Fatalf("%+v, %v", mine, err)
		}
		asked, err := client.AskFoundryQuestion(as(eve), &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			Question: "Who pays?", ClientToken: "eve-1"})
		if err != nil {
			t.Fatal(err)
		}
		stored := store.questions[len(store.questions)-1]
		if stored.ID != asked.GetQuestion().GetId() || stored.Grantee == nil || stored.Grantee.ID != "user-9" || stored.AskedByUserID == nil {
			t.Fatalf("stored %+v", stored)
		}
	})
	t.Run("not once an owner or admin", func(t *testing.T) {
		people["user-9"] = ports.Member{Role: ports.RoleAdmin, RawAccess: true}
		if got := list(t, eve); len(got) != 2 {
			t.Fatalf("promoted: %v", got)
		}
		people["user-9"] = ports.Member{Role: ports.RoleMember}
		if got := list(t, eve); len(got) != 1 {
			t.Fatalf("a member again: %v", got)
		}
	})
	t.Run("whatever the host can't say", func(t *testing.T) {
		s.deps.People = nil
		defer func() { s.deps.People = people }()
		if got := list(t, eve); len(got) != 1 {
			t.Fatalf("no People: %v", got)
		}
	})
	t.Run("lifted", func(t *testing.T) {
		store.teams = nil
		if _, err := s.SetFoundryAccessGrants(ports.WithActor(context.Background(), person), &foundryv1.SetFoundryAccessGrantsRequest{
			WorkspaceId: "ws-1", Principal: &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_USER, Id: "user-9"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetFoundryMemberRestricted(ports.WithActor(context.Background(), person),
			&foundryv1.SetFoundryMemberRestrictedRequest{WorkspaceId: "ws-1", UserId: "user-9"}); err != nil {
			t.Fatal(err)
		}
		if got := list(t, eve); len(got) != 2 {
			t.Fatalf("lifted: %v", got)
		}
	})
}

// Sources are the limit: a member given any is restricted to them, stays so
// when they go, and a member they couldn't confine isn't given any.
func TestGrantsRestrict(t *testing.T) {
	s, store := accessService(t)
	ctx := ports.WithActor(context.Background(), person)
	s.deps.People = accessPeople{
		"plain": {Role: ports.RoleMember}, "joiner": {Role: ports.RoleMember}, "querying": {Role: ports.RoleMember, RawAccess: true},
		"admin": {Role: ports.RoleAdmin, RawAccess: true}, "owner": {Role: ports.RoleOwner, RawAccess: true},
	}
	one := []*foundryv1.FoundryAccessTarget{{Kind: foundryv1.FoundryAccessTarget_KIND_CONNECTOR, ConnectorId: "conn-1"}}
	user := func(id string) *foundryv1.FoundryPrincipal {
		return &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_USER, Id: id}
	}
	team := func(id string) *foundryv1.FoundryPrincipal {
		return &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_TEAM, Id: id}
	}
	set := func(p *foundryv1.FoundryPrincipal, targets []*foundryv1.FoundryAccessTarget) error {
		_, err := s.SetFoundryAccessGrants(ctx, &foundryv1.SetFoundryAccessGrantsRequest{WorkspaceId: "ws-1", Principal: p, Targets: targets})
		return err
	}
	lift := func(id string) error {
		_, err := s.SetFoundryMemberRestricted(ctx, &foundryv1.SetFoundryMemberRestrictedRequest{WorkspaceId: "ws-1", UserId: id})
		return err
	}
	restricted := func(id string) bool { return slices.Contains(store.restricted["ws-1"], id) }

	t.Run("a member", func(t *testing.T) {
		if err := set(user("plain"), one); err != nil {
			t.Fatal(err)
		}
		if !restricted("plain") {
			t.Fatal("given sources, a member is restricted to them")
		}
		if err := lift("plain"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("while holding sources it can't be lifted: %v", err)
		}
		if err := set(user("plain"), nil); err != nil {
			t.Fatal(err)
		}
		if !restricted("plain") {
			t.Fatal("their last source gone, they stay restricted")
		}
		if err := lift("plain"); err != nil {
			t.Fatalf("holding none, it can be: %v", err)
		}
		if restricted("plain") {
			t.Fatal("lifted")
		}
	})
	for name, who := range map[string]string{"a member with raw access": "querying", "an admin": "admin", "an owner": "owner"} {
		t.Run(name+" is given nothing", func(t *testing.T) {
			if err := set(user(who), one); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("%v", err)
			}
			if restricted(who) || slices.ContainsFunc(store.grants, func(g foundrymodel.FoundryAccessGrant) bool { return g.Principal.ID == who }) {
				t.Fatalf("recorded anyway: %v", store.grants)
			}
			if err := set(user(who), nil); err != nil {
				t.Fatalf("taking away what they hold is always allowed: %v", err)
			}
		})
	}
	t.Run("taking some away from someone given raw access since", func(t *testing.T) {
		two := []access.Target{access.ConnectorTarget("conn-1"), access.ConnectorTarget("conn-pg")}
		for i, target := range two {
			store.grants = append(store.grants, foundrymodel.FoundryAccessGrant{ID: fmt.Sprintf("raw-%d", i), WorkspaceID: "ws-1",
				Principal: access.Principal{Kind: access.PrincipalUser, ID: "querying"}, Target: target})
		}
		if err := set(user("querying"), one); err != nil {
			t.Fatalf("narrowing what they hold is allowed: %v", err)
		}
		if err := set(user("querying"), []*foundryv1.FoundryAccessTarget{
			{Kind: foundryv1.FoundryAccessTarget_KIND_CONNECTOR, ConnectorId: "conn-1"},
			{Kind: foundryv1.FoundryAccessTarget_KIND_ENRICHMENT},
		}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("adding isn't: %v", err)
		}
		table := []*foundryv1.FoundryAccessTarget{{Kind: foundryv1.FoundryAccessTarget_KIND_TABLE, ConnectorId: "conn-1", Table: "customers"}}
		if err := set(user("querying"), table); err != nil {
			t.Fatalf("a connector down to one of its tables is narrowing: %v", err)
		}
		if err := set(user("querying"), []*foundryv1.FoundryAccessTarget{{Kind: foundryv1.FoundryAccessTarget_KIND_TABLE, ConnectorId: "conn-1", Table: "invoices"}}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("another table is adding: %v", err)
		}
		if err := set(user("querying"), nil); err != nil {
			t.Fatal(err)
		}
		store.restricted["ws-1"] = slices.DeleteFunc(store.restricted["ws-1"], func(u string) bool { return u == "querying" })
	})
	t.Run("a team", func(t *testing.T) {
		store.teams = append(store.teams,
			foundrymodel.FoundryTeam{ID: "team-ok", WorkspaceID: "ws-1", Name: "Ok", MemberUserIDs: []string{"plain", "admin", "owner"}},
			foundrymodel.FoundryTeam{ID: "team-raw", WorkspaceID: "ws-1", Name: "Raw", MemberUserIDs: []string{"plain", "querying"}})
		if err := set(team("team-raw"), one); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("a team with a member grants can't confine: %v", err)
		}
		if restricted("plain") {
			t.Fatal("a refused grant restricts no one")
		}
		if err := set(team("team-ok"), one); err != nil {
			t.Fatalf("owners and admins may sit on a team: %v", err)
		}
		if !restricted("plain") {
			t.Fatal("a team's members are restricted to what it's given")
		}
		if err := lift("plain"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("while on a team holding sources: %v", err)
		}
		update := func(members ...string) error {
			_, err := s.UpdateFoundryTeam(ctx, &foundryv1.UpdateFoundryTeamRequest{WorkspaceId: "ws-1", TeamId: "team-ok", Name: "Ok", MemberUserIds: members})
			return err
		}
		if err := update("plain", "admin", "owner", "querying"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("joining with raw access: %v", err)
		}
		if err := update("plain", "joiner"); err != nil {
			t.Fatal(err)
		}
		if !restricted("joiner") {
			t.Fatal("whoever joins is restricted")
		}
		// Given raw access since, a member already on the team doesn't stop a rename.
		s.deps.People.(accessPeople)["joiner"] = ports.Member{Role: ports.RoleMember, RawAccess: true}
		if err := update("plain", "joiner"); err != nil {
			t.Fatalf("no one joins: %v", err)
		}
		if err := update("plain"); err != nil {
			t.Fatal(err)
		}
		if !restricted("joiner") {
			t.Fatal("whoever leaves stays restricted")
		}
		// A team with no grants takes anyone, and restricts no one.
		if _, err := s.UpdateFoundryTeam(ctx, &foundryv1.UpdateFoundryTeamRequest{WorkspaceId: "ws-1", TeamId: "team-raw", Name: "Raw", MemberUserIds: []string{"querying"}}); err != nil {
			t.Fatal(err)
		}
		if restricted("querying") {
			t.Fatal("a team holding nothing restricts no one")
		}
	})
}

func TestSetFoundryMemberRestricted(t *testing.T) {
	s, store := accessService(t)
	ctx := ports.WithActor(context.Background(), person)
	s.deps.People = accessPeople{
		"member": {Role: ports.RoleMember}, "querying": {Role: ports.RoleMember, RawAccess: true},
		"admin": {Role: ports.RoleAdmin, RawAccess: true}, "owner": {Role: ports.RoleOwner, RawAccess: true},
	}
	for name, tc := range map[string]struct {
		req  *foundryv1.SetFoundryMemberRestrictedRequest
		code codes.Code
	}{
		"a plain member":        {&foundryv1.SetFoundryMemberRestrictedRequest{WorkspaceId: "ws-1", UserId: "member", Restricted: true}, codes.OK},
		"one who queries":       {&foundryv1.SetFoundryMemberRestrictedRequest{WorkspaceId: "ws-1", UserId: "querying", Restricted: true}, codes.FailedPrecondition},
		"an admin":              {&foundryv1.SetFoundryMemberRestrictedRequest{WorkspaceId: "ws-1", UserId: "admin", Restricted: true}, codes.FailedPrecondition},
		"an owner":              {&foundryv1.SetFoundryMemberRestrictedRequest{WorkspaceId: "ws-1", UserId: "owner", Restricted: true}, codes.FailedPrecondition},
		"no member":             {&foundryv1.SetFoundryMemberRestrictedRequest{WorkspaceId: "ws-1", UserId: "stranger", Restricted: true}, codes.NotFound},
		"lifting, without asks": {&foundryv1.SetFoundryMemberRestrictedRequest{WorkspaceId: "ws-1", UserId: "admin"}, codes.OK},
		"no member named":       {&foundryv1.SetFoundryMemberRestrictedRequest{WorkspaceId: "ws-1", Restricted: true}, codes.InvalidArgument},
		"no workspace":          {&foundryv1.SetFoundryMemberRestrictedRequest{UserId: "member", Restricted: true}, codes.InvalidArgument},
	} {
		if _, err := s.SetFoundryMemberRestricted(ctx, tc.req); status.Code(err) != tc.code {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !slices.Equal(store.restricted["ws-1"], []string{"member"}) {
		t.Fatalf("restricted %v", store.restricted)
	}
	s.deps.People = nil
	if _, err := s.SetFoundryMemberRestricted(ctx, &foundryv1.SetFoundryMemberRestrictedRequest{WorkspaceId: "ws-1", UserId: "member", Restricted: true}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("an install has no members: %v", err)
	}
	got, err := s.ListFoundryAccess(ctx, &foundryv1.ListFoundryAccessRequest{WorkspaceId: "ws-1"})
	if err != nil || !slices.Equal(got.GetRestrictedUserIds(), []string{"member"}) {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestFoundryTeams(t *testing.T) {
	s, store := accessService(t)
	ctx := ports.WithActor(context.Background(), person)
	s.deps.People = accessPeople{"ada": {Role: ports.RoleMember}, "grace": {Role: ports.RoleAdmin}}
	created, err := s.CreateFoundryTeam(ctx, &foundryv1.CreateFoundryTeamRequest{WorkspaceId: "ws-1", Name: "  Finance ", MemberUserIds: []string{"ada", "grace", "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	team := created.GetTeam()
	if team.GetName() != "Finance" || !slices.Equal(team.GetMemberUserIds(), []string{"ada", "grace"}) || team.GetCreatedByUserId() != "user-1" {
		t.Fatalf("%+v", team)
	}
	updated, err := s.UpdateFoundryTeam(ctx, &foundryv1.UpdateFoundryTeamRequest{WorkspaceId: "ws-1", TeamId: team.GetId(), Name: "Billing", MemberUserIds: []string{"grace"}})
	if err != nil || updated.GetTeam().GetName() != "Billing" || !slices.Equal(updated.GetTeam().GetMemberUserIds(), []string{"grace"}) {
		t.Fatalf("%+v, %v", updated, err)
	}
	listed, err := s.ListFoundryAccess(ctx, &foundryv1.ListFoundryAccessRequest{WorkspaceId: "ws-1"})
	if err != nil || len(listed.GetTeams()) != 1 || listed.GetTeams()[0].GetId() != team.GetId() {
		t.Fatalf("%+v, %v", listed, err)
	}
	long := strings.Repeat("x", maxFoundryTeamName+1)
	for name, tc := range map[string]struct {
		call func() error
		code codes.Code
	}{
		"a name taken": {func() error {
			_, err := s.CreateFoundryTeam(ctx, &foundryv1.CreateFoundryTeamRequest{WorkspaceId: "ws-1", Name: "Billing"})
			return err
		}, codes.AlreadyExists},
		"renamed to a name taken": {func() error {
			other, _ := s.CreateFoundryTeam(ctx, &foundryv1.CreateFoundryTeamRequest{WorkspaceId: "ws-1", Name: "Ops"})
			_, err := s.UpdateFoundryTeam(ctx, &foundryv1.UpdateFoundryTeamRequest{WorkspaceId: "ws-1", TeamId: other.GetTeam().GetId(), Name: "Billing"})
			return err
		}, codes.AlreadyExists},
		"no name": {func() error {
			_, err := s.CreateFoundryTeam(ctx, &foundryv1.CreateFoundryTeamRequest{WorkspaceId: "ws-1", Name: "  "})
			return err
		}, codes.InvalidArgument},
		"a name too long": {func() error {
			_, err := s.CreateFoundryTeam(ctx, &foundryv1.CreateFoundryTeamRequest{WorkspaceId: "ws-1", Name: long})
			return err
		}, codes.InvalidArgument},
		"no workspace": {func() error {
			_, err := s.CreateFoundryTeam(ctx, &foundryv1.CreateFoundryTeamRequest{Name: "X"})
			return err
		}, codes.InvalidArgument},
		"a stranger": {func() error {
			_, err := s.CreateFoundryTeam(ctx, &foundryv1.CreateFoundryTeamRequest{WorkspaceId: "ws-1", Name: "X", MemberUserIds: []string{"stranger"}})
			return err
		}, codes.InvalidArgument},
		"too many members": {func() error {
			_, err := s.CreateFoundryTeam(ctx, &foundryv1.CreateFoundryTeamRequest{WorkspaceId: "ws-1", Name: "X", MemberUserIds: make([]string, maxFoundryTeamMembers+1)})
			return err
		}, codes.InvalidArgument},
		"another's team": {func() error {
			_, err := s.UpdateFoundryTeam(ctx, &foundryv1.UpdateFoundryTeamRequest{WorkspaceId: "ws-1", TeamId: "team-404", Name: "X"})
			return err
		}, codes.NotFound},
		"deleting another's team": {func() error {
			_, err := s.DeleteFoundryTeam(ctx, &foundryv1.DeleteFoundryTeamRequest{WorkspaceId: "ws-1", TeamId: "team-404"})
			return err
		}, codes.NotFound},
		"deleting no team": {func() error {
			_, err := s.DeleteFoundryTeam(ctx, &foundryv1.DeleteFoundryTeamRequest{WorkspaceId: "ws-1"})
			return err
		}, codes.InvalidArgument},
	} {
		if err := tc.call(); status.Code(err) != tc.code {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.DeleteFoundryTeam(ctx, &foundryv1.DeleteFoundryTeamRequest{WorkspaceId: "ws-1", TeamId: team.GetId()}); err != nil {
		t.Fatal(err)
	}
	for len(store.teams) < maxFoundryTeams {
		store.teams = append(store.teams, foundrymodel.FoundryTeam{ID: fmt.Sprint(len(store.teams)), WorkspaceID: "ws-1", Name: fmt.Sprint(len(store.teams))})
	}
	if _, err := s.CreateFoundryTeam(ctx, &foundryv1.CreateFoundryTeamRequest{WorkspaceId: "ws-1", Name: "One more"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("past the cap: %v", err)
	}
	s.deps.People = nil
	if _, err := s.CreateFoundryTeam(ctx, &foundryv1.CreateFoundryTeamRequest{WorkspaceId: "ws-1", Name: "Y", MemberUserIds: []string{"ada"}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("an install has no members: %v", err)
	}
}

// A grantee reads the documents a load its grants cover holds: their text,
// their ontology's list of them and their page there, and no others.
func TestGranteeOntologyDocuments(t *testing.T) {
	s, store := accessService(t)
	bucket := s.deps.Documents.(*documentBucket)
	bucket.content = map[string]string{"docs/held": "# Held\n", "docs/loose": "# Loose\n"}
	store.documents = append(store.documents,
		&foundrymodel.FoundryDocument{ID: "doc-held", WorkspaceID: "ws-1", SetID: "set-general", Name: "held.md", MediaType: foundrymodel.FoundryMediaMarkdown,
			Status: foundrymodel.FoundryDocumentUploaded, S3Key: "docs/held"},
		&foundrymodel.FoundryDocument{ID: "doc-loose", WorkspaceID: "ws-1", SetID: "set-general", Name: "loose.md", MediaType: foundrymodel.FoundryMediaMarkdown,
			Status: foundrymodel.FoundryDocumentUploaded, S3Key: "docs/loose"})
	store.members["load-1"] = []string{"doc-held"}
	store.holding = map[string][]string{"doc-held": {"ont-1"}, "doc-loose": {"ont-3"}}
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)

	text, err := client.GetFoundryDocumentContent(as(agent), &foundryv1.GetFoundryDocumentContentRequest{WorkspaceId: "ws-1", Id: "doc-held"})
	if err != nil || text.GetText() != "# Held\n" {
		t.Fatalf("%+v, %v", text, err)
	}
	if _, err := client.GetFoundryDocumentContent(as(agent), &foundryv1.GetFoundryDocumentContentRequest{WorkspaceId: "ws-1", Id: "doc-loose"}); status.Code(err) != codes.NotFound {
		t.Fatalf("a document only Payroll's load holds: %v", err)
	}
	if _, err := client.GetFoundryDocumentContent(as(person), &foundryv1.GetFoundryDocumentContentRequest{WorkspaceId: "ws-1", Id: "doc-loose"}); err != nil {
		t.Fatalf("the host's caller reads any: %v", err)
	}
	if _, err := client.GetFoundryDocumentBytes(as(agent), &foundryv1.GetFoundryDocumentBytesRequest{WorkspaceId: "ws-1", Id: "doc-held"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a document's bytes: %v", err)
	}
	got, err := client.GetFoundryOntologyDocument(as(agent), &foundryv1.GetFoundryOntologyDocumentRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "doc-held"})
	if err != nil || got.GetDocument().GetId() != "doc-held" {
		t.Fatalf("%+v, %v", got, err)
	}
	if _, err := client.GetFoundryOntologyDocument(as(agent), &foundryv1.GetFoundryOntologyDocumentRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "doc-loose"}); status.Code(err) != codes.NotFound {
		t.Fatalf("another load's document: %v", err)
	}
	listed, err := client.ListFoundryOntologyDocuments(as(agent), &foundryv1.ListFoundryOntologyDocumentsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
	if err != nil || len(listed.GetDocuments()) != 1 || listed.GetDocuments()[0].GetId() != "doc-held" {
		t.Fatalf("the load's documents, not the scope's waiting ones: %+v, %v", listed, err)
	}
}
