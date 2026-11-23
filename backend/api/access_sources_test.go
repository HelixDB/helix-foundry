package api

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// slackMember is a member admitted through her grant of the Slack connection alone.
var slackMember = ports.Actor{Source: ports.SourceGrantee, UserID: "user-slackMember", WorkspaceID: "ws-1"}

// slackService is accessService with Chatter (ont-5), whose current load
// (load-5) holds a day of Slack's #general (C1, public) and of #deals (C2,
// private), and Ada, who holds the Slack connection. Slack shares what
// visible says with her email, and #general with everyone.
func slackService(t *testing.T, visible ...string) (*Service, *accessStore) {
	t.Helper()
	s, store := accessService(t)
	s.deps.People = accessPeople{"user-slackMember": {Role: ports.RoleMember, Email: " Ada@Example.com"}, "user-1": {Role: ports.RoleOwner}}
	store.rows["conn-slack"] = &foundrymodel.Connector{ID: "conn-slack", WorkspaceID: "ws-1", Kind: "slack", Name: "Slack", CreatedAt: time.Now()}
	chatter := "load-5"
	store.ontologies["ont-5"] = &foundrymodel.FoundryOntology{ID: "ont-5", WorkspaceID: "ws-1", Name: "Chatter", ProjectID: "proj-1", TenantID: "tenant-5",
		CurrentLoadID: &chatter, CreatedAt: time.Now()}
	store.loads = append(store.loads, &foundrymodel.FoundryLoad{ID: "load-5", OntologyID: "ont-5", VersionID: "ver-5", Version: 5, Status: foundrymodel.FoundryLoadReady})
	store.versions = append(store.versions, &foundrymodel.OntologyVersion{ID: "ver-5", WorkspaceID: "ws-1", OntologyID: "ont-5",
		Format: foundrymodel.OntologyFormatAgentBundle, Definition: json.RawMessage(`{"format":"agent_bundle","ontology_toml":"whole","mappings_toml":"every mapping"}`)})
	conversations := access.SetRef{SetID: "set-slack", ConnectorID: "conn-slack"}
	store.inputs["ont-5"] = foundrymodel.FoundryLoadInputs{LoadID: "load-5", VersionID: "ver-5", Inputs: access.Inputs{Sets: []access.SetRef{conversations}}}
	store.holdings = map[string]access.Holdings{"load-5": {Documents: []access.DocumentRef{
		{ID: "doc-general", Set: conversations, Stream: "conversations", ExternalID: "C1:2026-11-01"},
		{ID: "doc-deals", Set: conversations, Stream: "conversations", ExternalID: "C2:2026-11-01"},
	}}}
	store.sets = append(store.sets, &foundrymodel.FoundryDocumentSet{ID: "set-slack", WorkspaceID: "ws-1", Name: "Conversations",
		Owner: &foundrymodel.FoundryDocumentSetOwner{ConnectorID: "conn-slack", ConnectorKind: "slack", Stream: "conversations"}})
	for _, d := range []struct{ id, channel string }{{"doc-general", "C1"}, {"doc-deals", "C2"}} {
		store.documents = append(store.documents, &foundrymodel.FoundryDocument{ID: d.id, WorkspaceID: "ws-1", SetID: "set-slack", Name: d.id + ".md",
			MediaType: foundrymodel.FoundryMediaMarkdown, Status: foundrymodel.FoundryDocumentUploaded, S3Key: "docs/" + d.id,
			Source: &foundrymodel.FoundryDocumentSource{ExternalID: d.channel + ":2026-11-01", Version: "v1", At: time.Now()}})
		store.holding[d.id] = []string{"ont-5"}
	}
	store.members["load-5"] = []string{"doc-general", "doc-deals"}
	store.grants = append(store.grants, foundrymodel.FoundryAccessGrant{ID: "g-slack", WorkspaceID: "ws-1",
		Principal: access.Principal{Kind: access.PrincipalUser, ID: "user-slackMember"}, Target: access.ConnectorTarget("conn-slack")})
	now := time.Now()
	store.sharing = map[string][]foundrymodel.FoundrySourceSharing{
		"ada@example.com": {{ConnectorID: "conn-slack", ReadAt: &now, Visible: visible}},
		"agent":           {{ConnectorID: "conn-slack", ReadAt: &now, Visible: []string{"C1"}}},
	}
	return s, store
}

// A grantee holding a Slack connection reads only what Slack shares with
// her: the days of her channels, the ontology as part of it, and her own
// questions while Slack shares any of it with her.
func TestGranteeFollowsSlack(t *testing.T) {
	s, store := slackService(t, "C1")
	reader := &fakeFoundryRead{result: `{"asked_at":"2026-11-22","results":[{"ok":{}}]}`}
	s.deps.FoundryRead = reader
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	ctx := as(slackMember)

	my, err := client.GetMyFoundryAccess(ctx, &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
	if err != nil || !slices.Equal(my.GetOntologyIds(), []string{"ont-5"}) || !slices.Contains(my.GetPartialOntologyIds(), "ont-5") {
		t.Fatalf("/me: %+v, %v", my, err)
	}
	if _, err := client.ReadFoundryGraph(ctx, &foundryv1.ReadFoundryGraphRequest{WorkspaceId: "ws-1", OntologyId: "ont-5", LoadId: "load-5",
		ReadsJson: `[{"query":{}}]`}); err != nil {
		t.Fatal(err)
	}
	read := reader.requests[len(reader.requests)-1]
	if want := `{"sources":[],"documents":{"only":["doc-general"]},"sets":["set-slack"],"enrichment":false,"withheld":[]}`; read.GetVisibilityJson() != want {
		t.Fatalf("visibility %s, want %s", read.GetVisibilityJson(), want)
	}
	list, err := client.ListFoundryOntologyDocuments(ctx, &foundryv1.ListFoundryOntologyDocumentsRequest{WorkspaceId: "ws-1", OntologyId: "ont-5"})
	if err != nil || len(list.GetDocuments()) != 1 || list.GetDocuments()[0].GetId() != "doc-general" {
		t.Fatalf("documents %+v, %v", list, err)
	}
	if _, err := client.GetFoundryDocumentContent(ctx, &foundryv1.GetFoundryDocumentContentRequest{WorkspaceId: "ws-1", Id: "doc-deals"}); status.Code(err) != codes.NotFound {
		t.Fatalf("a private channel's day: %v", err)
	}
	named, err := client.ListFoundryDocuments(ctx, &foundryv1.ListFoundryDocumentsRequest{WorkspaceId: "ws-1", Ids: []string{"doc-general", "doc-deals"}})
	if err != nil || len(named.GetDocuments()) != 1 || named.GetDocuments()[0].GetId() != "doc-general" {
		t.Fatalf("named %+v, %v", named, err)
	}

	// Her own question stays hers: what it read is set by set, and Slack
	// still shares a channel of it with her.
	asked, err := client.AskFoundryQuestion(ctx, &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-5", Question: "Who?", ClientToken: "slack-1"})
	if err != nil {
		t.Fatal(err)
	}
	stored := store.questions[len(store.questions)-1]
	if stored.Seen == nil || !slices.Equal(stored.Seen.Sets, []access.SetRef{{SetID: "set-slack", ConnectorID: "conn-slack"}}) ||
		!slices.Equal(stored.SeenItems, []access.ItemRef{{ConnectorID: "conn-slack", Item: "C1"}}) {
		t.Fatalf("seen %+v, items %+v", stored.Seen, stored.SeenItems)
	}
	get := func() error {
		_, err := client.GetFoundryQuestion(ctx, &foundryv1.GetFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-5", Id: asked.GetQuestion().GetId()})
		return err
	}
	if err := get(); err != nil {
		t.Fatalf("her own question: %v", err)
	}
	listed, err := client.ListFoundryQuestions(ctx, &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-5"})
	if err != nil || len(listed.GetQuestions()) != 1 {
		t.Fatalf("listed %+v, %v", listed, err)
	}

	// Owners see her as reading part of it; the agent's key, holding Stripe and General, none.
	owners, err := client.ListFoundryAccess(as(person), &foundryv1.ListFoundryAccessRequest{WorkspaceId: "ws-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range owners.GetOntologies() {
		if o.GetOntologyId() == "ont-5" && (len(o.GetReaders()) != 0 || len(o.GetPartialReaders()) != 1 || o.GetPartialReaders()[0].GetId() != "user-slackMember") {
			t.Fatalf("%+v", o)
		}
	}

	// Slack shares nothing of it with her any longer (no channel, and
	// #general taken from everyone): the ontology and her question go.
	now := time.Now()
	store.sharing = map[string][]foundrymodel.FoundrySourceSharing{"ada@example.com": {{ConnectorID: "conn-slack", ReadAt: &now}}}
	if err := get(); status.Code(err) != codes.NotFound {
		t.Fatalf("a question of what she no longer reads: %v", err)
	}
	if _, err := client.GetFoundryOntology(ctx, &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-5"}); status.Code(err) != codes.NotFound {
		t.Fatalf("an ontology she reads none of: %v", err)
	}
}

// Her answer goes with the channel it read: asked while she was in #general
// and #deals, it is gone once Slack no longer lists her in #deals, though she
// still reads the ontology through #general.
func TestGranteesAnswerGoesWithItsChannel(t *testing.T) {
	s, store := slackService(t, "C1", "C2")
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	ctx := as(slackMember)
	asked, err := client.AskFoundryQuestion(ctx, &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-5", Question: "What did #deals say?",
		ClientToken: "slack-2"})
	if err != nil {
		t.Fatal(err)
	}
	stored := store.questions[len(store.questions)-1]
	if want := []access.ItemRef{{ConnectorID: "conn-slack", Item: "C1"}, {ConnectorID: "conn-slack", Item: "C2"}}; !slices.Equal(stored.SeenItems, want) {
		t.Fatalf("items %+v, want %+v", stored.SeenItems, want)
	}
	get := func() error {
		_, err := client.GetFoundryQuestion(ctx, &foundryv1.GetFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-5", Id: asked.GetQuestion().GetId()})
		return err
	}
	if err := get(); err != nil {
		t.Fatalf("while in both: %v", err)
	}
	now := time.Now()
	store.sharing["ada@example.com"] = []foundrymodel.FoundrySourceSharing{{ConnectorID: "conn-slack", ReadAt: &now, Visible: []string{"C1"}}}
	if err := get(); status.Code(err) != codes.NotFound {
		t.Fatalf("removed from #deals: %v", err)
	}
	listed, err := client.ListFoundryQuestions(ctx, &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-5"})
	if err != nil || len(listed.GetQuestions()) != 0 {
		t.Fatalf("listed %+v, %v", listed, err)
	}
	if _, err := client.GetFoundryOntology(ctx, &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-5"}); err != nil {
		t.Fatalf("she still reads #general's part: %v", err)
	}
}

// The load holds a day of #exec (C9), a channel the last read of Slack's
// permissions didn't list (the app left it, or joined it since): a member in
// every channel it listed still reads the load in part, never #exec.
func TestGranteeNeverReadsAChannelSlackNoLongerLists(t *testing.T) {
	s, store := slackService(t, "C1", "C2")
	conversations := access.SetRef{SetID: "set-slack", ConnectorID: "conn-slack"}
	h := store.holdings["load-5"]
	h.Documents = append(h.Documents, access.DocumentRef{ID: "doc-exec", Set: conversations, Stream: "conversations", ExternalID: "C9:2026-11-01"})
	store.holdings["load-5"] = h
	store.documents = append(store.documents, &foundrymodel.FoundryDocument{ID: "doc-exec", WorkspaceID: "ws-1", SetID: "set-slack", Name: "doc-exec.md",
		MediaType: foundrymodel.FoundryMediaMarkdown, Status: foundrymodel.FoundryDocumentUploaded, S3Key: "docs/doc-exec",
		Source: &foundrymodel.FoundryDocumentSource{ExternalID: "C9:2026-11-01", Version: "v1", At: time.Now()}})
	store.holding["doc-exec"] = []string{"ont-5"}
	store.members["load-5"] = append(store.members["load-5"], "doc-exec")
	reader := &fakeFoundryRead{result: `{"asked_at":"2026-11-22","results":[{"ok":{}}]}`}
	s.deps.FoundryRead = reader
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	ctx := as(slackMember)

	my, err := client.GetMyFoundryAccess(ctx, &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
	if err != nil || !slices.Contains(my.GetPartialOntologyIds(), "ont-5") {
		t.Fatalf("/me: %+v, %v", my, err)
	}
	if want := []string{"C1", "C2"}; len(my.GetSourceItems()) != 2 || my.GetSourceItems()[0].GetItemKey() != want[0] || my.GetSourceItems()[1].GetItemKey() != want[1] {
		t.Fatalf("source items %+v", my.GetSourceItems())
	}
	if _, err := client.ReadFoundryGraph(ctx, &foundryv1.ReadFoundryGraphRequest{WorkspaceId: "ws-1", OntologyId: "ont-5", LoadId: "load-5",
		ReadsJson: `[{"query":{}}]`}); err != nil {
		t.Fatal(err)
	}
	read := reader.requests[len(reader.requests)-1]
	if want := `{"sources":[],"documents":{"only":["doc-general","doc-deals"]},"sets":["set-slack"],"enrichment":false,"withheld":[]}`; read.GetVisibilityJson() != want {
		t.Fatalf("visibility %s, want %s", read.GetVisibilityJson(), want)
	}
	list, err := client.ListFoundryOntologyDocuments(ctx, &foundryv1.ListFoundryOntologyDocumentsRequest{WorkspaceId: "ws-1", OntologyId: "ont-5"})
	if err != nil || len(list.GetDocuments()) != 2 || slices.ContainsFunc(list.GetDocuments(), func(d *foundryv1.FoundryDocument) bool {
		return d.GetId() == "doc-exec"
	}) {
		t.Fatalf("documents %+v, %v", list, err)
	}
	if _, err := client.GetFoundryDocumentContent(ctx, &foundryv1.GetFoundryDocumentContentRequest{WorkspaceId: "ws-1", Id: "doc-exec"}); status.Code(err) != codes.NotFound {
		t.Fatalf("a channel Slack no longer lists: %v", err)
	}
	owners, err := client.GetMyFoundryAccess(as(person), &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
	if err != nil || len(owners.GetSourceItems()) != 0 {
		t.Fatalf("a full reader is told no items: %+v, %v", owners, err)
	}
}

func TestGranteeFollowsSlackWhole(t *testing.T) {
	for name, tc := range map[string]struct {
		visible   []string
		readAt    time.Time
		problemAt *time.Time
		// partial: she reads part; none: she reads nothing of Chatter.
		partial, none bool
	}{
		"in every channel listed she still reads it in part": {visible: []string{"C1", "C2"}, readAt: time.Now(), partial: true},
		"audiences read over a week ago hide it":             {visible: []string{"C1", "C2"}, readAt: time.Now().Add(-8 * 24 * time.Hour), none: true},
		"audiences read just inside the week count":          {visible: []string{"C1"}, readAt: time.Now().Add(-6 * 24 * time.Hour), partial: true},
		"audiences read two days ago, a read since failed, hide it": {visible: []string{"C1", "C2"}, readAt: time.Now().Add(-48 * time.Hour),
			problemAt: ptr(time.Now().Add(-time.Hour)), none: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, store := slackService(t)
			store.sharing["ada@example.com"] = []foundrymodel.FoundrySourceSharing{{ConnectorID: "conn-slack", ReadAt: &tc.readAt, ProblemAt: tc.problemAt,
				Visible: tc.visible}}
			conn, as := served(t, s)
			my, err := foundryv1.NewFoundryServiceClient(conn).GetMyFoundryAccess(as(slackMember), &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(my.GetOntologyIds(), "ont-5"); got == tc.none {
				t.Fatalf("reads ont-5: %v (%+v)", got, my)
			}
			if got := slices.Contains(my.GetPartialOntologyIds(), "ont-5"); got != tc.partial {
				t.Fatalf("part of ont-5: %v (%+v)", got, my)
			}
		})
	}
	t.Run("never read, nothing of it", func(t *testing.T) {
		s, store := slackService(t)
		store.sharing = nil
		conn, as := served(t, s)
		_, err := foundryv1.NewFoundryServiceClient(conn).GetFoundryOntology(as(slackMember), &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-5"})
		if status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
	})
	// A member Slack has no account for by her email (a guest signed in
	// with another address, or someone offboarded from Slack) reads none of
	// its channels, public ones included.
	t.Run("a member Slack doesn't know, nothing of it", func(t *testing.T) {
		s, store := slackService(t)
		s.deps.People = accessPeople{"user-slackMember": {Role: ports.RoleMember, Email: "ada@elsewhere.example"}}
		now := time.Now()
		store.sharing["ada@elsewhere.example"] = []foundrymodel.FoundrySourceSharing{{ConnectorID: "conn-slack", ReadAt: &now}}
		conn, as := served(t, s)
		_, err := foundryv1.NewFoundryServiceClient(conn).GetFoundryOntology(as(slackMember), &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-5"})
		if status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
	})
}

// Owners and admins see each Slack connection's last read and whom of the
// people it narrows it matches, never Slack's own directory.
func TestListFoundrySourceAudiences(t *testing.T) {
	s, store := slackService(t)
	s.deps.People = accessPeople{
		"user-slackMember": {Role: ports.RoleMember, Email: " Ada@Example.com"},
		"user-guest":       {Role: ports.RoleMember, Email: "guest@partner.example"},
		"user-none":        {Role: ports.RoleMember},
		"user-team":        {Role: ports.RoleMember, Email: "teammate@example.com"},
		"user-other":       {Role: ports.RoleMember, Email: "other@example.com"},
	}
	store.restricted["ws-1"] = []string{"user-slackMember", "user-guest", "user-none"}
	store.teams = append(store.teams,
		foundrymodel.FoundryTeam{ID: "team-granted", WorkspaceID: "ws-1", Name: "Sales", MemberUserIDs: []string{"user-team", "user-guest"}},
		foundrymodel.FoundryTeam{ID: "team-idle", WorkspaceID: "ws-1", Name: "Idle", MemberUserIDs: []string{"user-other"}})
	store.grants = append(store.grants, foundrymodel.FoundryAccessGrant{ID: "g-team", WorkspaceID: "ws-1",
		Principal: access.Principal{Kind: access.PrincipalTeam, ID: "team-granted"}, Target: access.ConnectorTarget("conn-slack")})
	store.sourcePeople = map[string][]foundrymodel.FoundrySourcePerson{
		"ada@example.com":       {{ConnectorID: "conn-slack"}},
		"guest@partner.example": {{ConnectorID: "conn-slack", Guest: true}},
		"teammate@example.com":  {{ConnectorID: "conn-slack"}},
	}
	store.rows["conn-slack-2"] = &foundrymodel.Connector{ID: "conn-slack-2", WorkspaceID: "ws-1", Kind: "slack", Name: "Slack too", CreatedAt: time.Now()}
	read := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	store.audiences = []foundrymodel.FoundrySourceAudience{{ConnectorID: "conn-slack", ReadAt: &read,
		Problem:       &foundrymodel.FoundrySourceAudienceProblem{Message: "add the groups:read scope", At: read.Add(time.Hour)},
		EveryoneItems: 3, MembersItems: 2}}
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)

	got, err := client.ListFoundrySourceAudiences(as(person), &foundryv1.ListFoundrySourceAudiencesRequest{WorkspaceId: "ws-1"})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*foundryv1.FoundrySourceAudience{}
	for _, a := range got.GetSources() {
		byID[a.GetConnectorId()] = a
	}
	if len(got.GetSources()) != 2 {
		t.Fatalf("only Slack connections, every one: %+v", got.GetSources())
	}
	slack := byID["conn-slack"]
	if slack.GetKind() != "slack" || !slack.GetReadAt().AsTime().Equal(read) || slack.GetEveryoneItems() != 3 || slack.GetMembersItems() != 2 ||
		slack.GetProblem().GetMessage() != "add the groups:read scope" || slack.GetStale() || slack.GetGrantees() != 4 ||
		!slices.Equal(slack.GetUnmatchedUserIds(), []string{"user-none"}) || !slices.Equal(slack.GetGuestUserIds(), []string{"user-guest"}) {
		t.Fatalf("read an hour ago, a read since failed: %+v", slack)
	}
	never := byID["conn-slack-2"]
	if never.GetReadAt() != nil || !never.GetStale() || never.GetProblem() != nil || never.GetGrantees() != 4 ||
		!slices.Equal(never.GetUnmatchedUserIds(), []string{"user-guest", "user-none", "user-slackMember", "user-team"}) {
		t.Fatalf("never read: %+v", never)
	}

	// Read two days ago with a failure since: stale.
	twoDays := time.Now().Add(-48 * time.Hour)
	store.audiences[0].ReadAt = &twoDays
	if got, err = client.ListFoundrySourceAudiences(as(person), &foundryv1.ListFoundrySourceAudiencesRequest{WorkspaceId: "ws-1"}); err != nil ||
		!got.GetSources()[0].GetStale() {
		t.Fatalf("a read two days old, a failure since: %+v, %v", got, err)
	}

	for name, who := range map[string]ports.Actor{"a member through her grants": slackMember, "an agent's key": agent} {
		if _, err := client.ListFoundrySourceAudiences(as(who), &foundryv1.ListFoundrySourceAudiencesRequest{WorkspaceId: "ws-1"}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := client.ListFoundrySourceAudiences(as(person), &foundryv1.ListFoundrySourceAudiencesRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
}
