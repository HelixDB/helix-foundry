package api

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/agentquestion"
	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// founderStore is accessService's store with Founder's threads and turns
// (agentStore's) and dashboards beside it.
type founderStore struct {
	*accessStore
	agent      *agentStore
	dashboards map[string]*foundrymodel.FoundryDashboard
	// listedSince is what the last thread list was kept to.
	listedSince time.Time
}

// CreateFoundryAgentTurn stamps a new thread's creation, as MySQL does.
func (s *founderStore) CreateFoundryAgentTurn(ctx context.Context, t *foundrymodel.FoundryAgentTurn, thread *foundrymodel.FoundryAgentThread,
	limits foundrymodel.FoundryAgentLimits, first json.RawMessage) error {
	if thread != nil && thread.CreatedAt.IsZero() {
		thread.CreatedAt = time.Now()
	}
	return s.agent.CreateFoundryAgentTurn(ctx, t, thread, limits, first)
}

func (s *founderStore) GetFoundryAgentTurnByClientToken(ctx context.Context, ws, userID, token string) (*foundrymodel.FoundryAgentTurn, error) {
	return s.agent.GetFoundryAgentTurnByClientToken(ctx, ws, userID, token)
}

func (s *founderStore) GetFoundryAgentTurn(ctx context.Context, ws, id string) (*foundrymodel.FoundryAgentTurn, error) {
	return s.agent.GetFoundryAgentTurn(ctx, ws, id)
}

func (s *founderStore) GetFoundryAgentThread(ctx context.Context, ws, id string) (*foundrymodel.FoundryAgentThread, error) {
	return s.agent.GetFoundryAgentThread(ctx, ws, id)
}

func (s *founderStore) ListFoundryAgentTurns(ctx context.Context, threadID string, limit int) ([]*foundrymodel.FoundryAgentTurn, error) {
	return s.agent.ListFoundryAgentTurns(ctx, threadID, limit)
}

func (s *founderStore) ListFoundryAgentActions(ctx context.Context, threadID string) ([]*foundrymodel.FoundryAgentAction, error) {
	return s.agent.ListFoundryAgentActions(ctx, threadID)
}

func (s *founderStore) ListFoundryAgentEvents(ctx context.Context, turnID string, afterSeq, limit int) ([]foundrymodel.FoundryAgentEvent, error) {
	return s.agent.ListFoundryAgentEvents(ctx, turnID, afterSeq, limit)
}

func (s *founderStore) ListFoundryAgentThreads(_ context.Context, ws, userID string, createdSince time.Time, _ string, _ int) (
	[]*foundrymodel.FoundryAgentThread, string, error) {
	s.listedSince = createdSince
	var out []*foundrymodel.FoundryAgentThread
	for _, t := range s.agent.threads {
		if t.WorkspaceID == ws && t.UserID == userID && !t.CreatedAt.Before(createdSince) {
			out = append(out, t)
		}
	}
	return out, "", nil
}

func (s *founderStore) DeleteFoundryAgentThread(_ context.Context, ws, userID, id string) error {
	t, ok := s.agent.threads[id]
	if !ok || t.WorkspaceID != ws || t.UserID != userID {
		return foundrystore.ErrNotFound
	}
	delete(s.agent.threads, id)
	return nil
}

func (s *founderStore) CancelFoundryAgentTurn(_ context.Context, ws, userID, id string) (*foundrymodel.FoundryAgentTurn, error) {
	t, ok := s.agent.turns[id]
	switch {
	case !ok || t.WorkspaceID != ws || t.UserID != userID:
		return nil, foundrystore.ErrNotFound
	case !t.Status.Live():
		return nil, foundrystore.ErrConflict
	}
	t.Status = foundrymodel.FoundryAgentTurnCancelled
	return t, nil
}

func (s *founderStore) GetFoundryDashboard(_ context.Context, ws, id string) (*foundrymodel.FoundryDashboard, error) {
	d, ok := s.dashboards[id]
	if !ok || d.WorkspaceID != ws {
		return nil, foundrystore.ErrNotFound
	}
	return d, nil
}

// A restricted member talks to Founder in their own threads from their
// restriction on, read-only: the page they send names only what their grants
// show, they relay no draft's answers, and Founder's cards, runtime and
// helpers stay refused. A caller the host sent on as a grantee is refused.
func TestRestrictedMembersTalkToFounder(t *testing.T) {
	s, held := accessService(t)
	s.deps.Cfg.Foundry.Agent.Enabled = true
	s.deps.People = accessPeople{"user-9": {Role: ports.RoleMember}}
	restrictedAt := time.Now().Add(-time.Hour)
	held.restricted["ws-1"] = []string{"user-9"}
	held.restrictedAt = map[string]time.Time{"user-9": restrictedAt}
	pinned := time.Now()
	store := &founderStore{accessStore: held, agent: newAgentStore(), dashboards: map[string]*foundrymodel.FoundryDashboard{
		"dash-pinned": {ID: "dash-pinned", WorkspaceID: "ws-1", Name: "Revenue", PinnedAt: &pinned},
		"dash-draft":  {ID: "dash-draft", WorkspaceID: "ws-1", Name: "Draft"},
	}}
	s.deps.DB = store
	// A thread, with its turn, from before the restriction.
	store.agent.threads["th-old"] = &foundrymodel.FoundryAgentThread{ID: "th-old", WorkspaceID: "ws-1", UserID: "user-9", Kind: foundrymodel.FoundryAgentThreadChat,
		Title: "Load Billing", CreatedAt: restrictedAt.Add(-time.Hour)}
	store.agent.turns["turn-old"] = &foundrymodel.FoundryAgentTurn{ID: "turn-old", WorkspaceID: "ws-1", ThreadID: "th-old", UserID: "user-9",
		Kind: foundrymodel.FoundryAgentTurnMessage, Input: "Load Billing", Status: foundrymodel.FoundryAgentTurnQueued}

	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	eve := as(ports.Actor{Source: ports.SourceUser, UserID: "user-9", WorkspaceID: "ws-1"})
	send := func(token string, page *foundryv1.FoundryAgentPageContext) (*foundryv1.FoundryAgentTurnResponse, error) {
		return client.SendFoundryAgentMessage(eve, &foundryv1.SendFoundryAgentMessageRequest{WorkspaceId: "ws-1", ClientToken: token,
			Input: textInput("Who pays?"), PageContext: page})
	}

	t.Run("sends, keeping the page its grants show", func(t *testing.T) {
		sent, err := send("eve-1", &foundryv1.FoundryAgentPageContext{OntologyId: "ont-1", LoadId: "load-1", DashboardId: "dash-pinned"})
		require.NoError(t, err)
		page := sent.GetTurn().GetPageContext()
		assert.Equal(t, "ont-1", page.GetOntologyId())
		assert.Equal(t, "load-1", page.GetLoadId())
		assert.Equal(t, "dash-pinned", page.GetDashboardId())
		assert.Equal(t, "user-9", store.agent.turns[sent.GetTurn().GetId()].UserID)
	})
	t.Run("drops what its grants don't show", func(t *testing.T) {
		sent, err := send("eve-2", &foundryv1.FoundryAgentPageContext{OntologyId: "ont-3", DashboardId: "dash-draft", NewAutomation: true})
		require.NoError(t, err)
		assert.Nil(t, sent.GetTurn().GetPageContext(), "an ungranted ontology, an unpinned dashboard and a new automation")
		sent, err = send("eve-3", &foundryv1.FoundryAgentPageContext{OntologyId: "ont-1", LoadId: "load-0"})
		require.NoError(t, err)
		assert.Equal(t, "ont-1", sent.GetTurn().GetPageContext().GetOntologyId())
		assert.Empty(t, sent.GetTurn().GetPageContext().GetLoadId(), "a load its grants don't cover")
	})

	var mine *foundryv1.FoundryAgentTurn
	t.Run("lists, reads, polls, cancels and deletes its own threads from its restriction on", func(t *testing.T) {
		listed, err := client.ListFoundryAgentThreads(eve, &foundryv1.ListFoundryAgentThreadsRequest{WorkspaceId: "ws-1"})
		require.NoError(t, err)
		var ids []string
		for _, th := range listed.GetThreads() {
			ids = append(ids, th.GetId())
		}
		assert.Len(t, ids, 3)
		assert.NotContains(t, ids, "th-old")
		assert.True(t, store.listedSince.Equal(restrictedAt), "kept to its restriction in the store's query")

		_, err = client.GetFoundryAgentThread(eve, &foundryv1.GetFoundryAgentThreadRequest{WorkspaceId: "ws-1", Id: "th-old"})
		assert.Equal(t, codes.NotFound, status.Code(err))
		_, err = client.ListFoundryAgentTurnEvents(eve, &foundryv1.ListFoundryAgentTurnEventsRequest{WorkspaceId: "ws-1", Id: "turn-old"})
		assert.Equal(t, codes.NotFound, status.Code(err))
		_, err = client.CancelFoundryAgentTurn(eve, &foundryv1.CancelFoundryAgentTurnRequest{WorkspaceId: "ws-1", Id: "turn-old"})
		assert.Equal(t, codes.NotFound, status.Code(err))
		_, err = client.DeleteFoundryAgentThread(eve, &foundryv1.DeleteFoundryAgentThreadRequest{WorkspaceId: "ws-1", Id: "th-old"})
		assert.Equal(t, codes.NotFound, status.Code(err))
		_, err = send("eve-old", nil)
		require.NoError(t, err, "a new thread")
		_, err = client.SendFoundryAgentMessage(eve, &foundryv1.SendFoundryAgentMessageRequest{WorkspaceId: "ws-1", ThreadId: "th-old",
			ClientToken: "eve-into-old", Input: textInput("And now?")})
		assert.Equal(t, codes.NotFound, status.Code(err), "nor sent into")
		assert.Equal(t, foundrymodel.FoundryAgentTurnQueued, store.agent.turns["turn-old"].Status)
		assert.Contains(t, store.agent.threads, "th-old")

		sent, err := send("eve-4", nil)
		require.NoError(t, err)
		mine = sent.GetTurn()
		got, err := client.GetFoundryAgentThread(eve, &foundryv1.GetFoundryAgentThreadRequest{WorkspaceId: "ws-1", Id: mine.GetThreadId()})
		require.NoError(t, err)
		assert.Len(t, got.GetTurns(), 1)
		_, err = client.ListFoundryAgentTurnEvents(eve, &foundryv1.ListFoundryAgentTurnEventsRequest{WorkspaceId: "ws-1", Id: mine.GetId()})
		require.NoError(t, err)
		cancelled, err := client.CancelFoundryAgentTurn(eve, &foundryv1.CancelFoundryAgentTurnRequest{WorkspaceId: "ws-1", Id: mine.GetId()})
		require.NoError(t, err)
		assert.Equal(t, string(foundrymodel.FoundryAgentTurnCancelled), cancelled.GetTurn().GetStatus())
		_, err = client.DeleteFoundryAgentThread(eve, &foundryv1.DeleteFoundryAgentThreadRequest{WorkspaceId: "ws-1", Id: mine.GetThreadId()})
		require.NoError(t, err)
		assert.NotContains(t, store.agent.threads, mine.GetThreadId())
	})

	t.Run("relays no draft's answers", func(t *testing.T) {
		store.agent.threads["th-asked"] = &foundrymodel.FoundryAgentThread{ID: "th-asked", WorkspaceID: "ws-1", UserID: "user-9",
			Kind: foundrymodel.FoundryAgentThreadChat, CreatedAt: time.Now()}
		store.agent.turns["turn-asked"] = &foundrymodel.FoundryAgentTurn{ID: "turn-asked", WorkspaceID: "ws-1", ThreadID: "th-asked", UserID: "user-9",
			Kind: foundrymodel.FoundryAgentTurnMessage, Status: foundrymodel.FoundryAgentTurnDone, Asked: &foundrymodel.FoundryAgentAsked{
				Questions:  []agentquestion.Question{{ID: "q1", Text: "Which plans count?", Blocking: true}},
				Generation: &foundrymodel.FoundryAgentAskedGeneration{OntologyID: "ont-1", GenerationID: "gen-1", Round: 1}}}
		_, err := client.SendFoundryAgentMessage(eve, &foundryv1.SendFoundryAgentMessageRequest{WorkspaceId: "ws-1", ThreadId: "th-asked", ClientToken: "eve-answers",
			Input: &foundryv1.SendFoundryAgentMessageRequest_Answers{Answers: &foundryv1.FoundryAgentAnswers{TurnId: "turn-asked",
				Answers: []*foundryv1.AgentQuestionAnswer{{QuestionId: "q1", Text: "All of them"}}}}})
		assert.Equal(t, "FOUNDRY_AGENT_QUESTIONS_CLOSED", reasonOf(err))
	})

	t.Run("cards, the runtime and helpers stay refused", func(t *testing.T) {
		refused := []protoreflect.Name{"ExecuteFoundryAgentAction", "RefreshFoundryAgentAction", "DismissFoundryAgentAction", "GetFoundryAgentRuntime",
			"UpdateFoundryAgentRuntime", "CreateFoundryAgentHostPairing", "RevokeFoundryAgentHost"}
		seen := 0
		for _, m := range servedMethods(t) {
			if !slices.Contains(refused, m.Name()) {
				continue
			}
			seen++
			req := newMessage(t, m.Input())
			if f := m.Input().Fields().ByName("workspace_id"); f != nil {
				req.ProtoReflect().Set(f, protoreflect.ValueOfString("ws-1"))
			}
			err := conn.Invoke(eve, fullMethod(m), req, newMessage(t, m.Output()))
			assert.Equal(t, codes.PermissionDenied, status.Code(err), m.Name())
		}
		assert.Equal(t, len(refused), seen)
	})

	t.Run("a caller the host sent on as a grantee is refused", func(t *testing.T) {
		for _, who := range []ports.Actor{member, agent} {
			_, err := client.ListFoundryAgentThreads(as(who), &foundryv1.ListFoundryAgentThreadsRequest{WorkspaceId: "ws-1"})
			assert.Equal(t, codes.PermissionDenied, status.Code(err), "%+v", who)
		}
	})

	t.Run("not once an owner or admin", func(t *testing.T) {
		s.deps.People = accessPeople{"user-9": {Role: ports.RoleAdmin, RawAccess: true}}
		defer func() { s.deps.People = accessPeople{"user-9": {Role: ports.RoleMember}} }()
		got, err := client.GetFoundryAgentThread(eve, &foundryv1.GetFoundryAgentThreadRequest{WorkspaceId: "ws-1", Id: "th-old"})
		require.NoError(t, err)
		assert.Equal(t, "th-old", got.GetThread().GetId())
	})
}

// Founder's tools reach Foundry as the person: an integration.Service whose
// Foundry is the API served as a host serves it narrows a restricted member's
// calls to their grants, whatever the session's Identity lets through, and
// their questions are theirs, under a grantee's limits.
func TestFounderToolsReadAsARestrictedMember(t *testing.T) {
	s, held := accessService(t)
	s.deps.People = accessPeople{"user-9": {Role: ports.RoleMember}}
	held.restricted["ws-1"] = []string{"user-9"}
	// Stripe besides General: all of Billing, so an ask reads it whole.
	held.grants = append(held.grants, foundrymodel.FoundryAccessGrant{ID: "g5", WorkspaceID: "ws-1",
		Principal: access.Principal{Kind: access.PrincipalUser, ID: "user-9"}, Target: access.ConnectorTarget("conn-1")})
	conn, as := served(t, s)
	tools := &integration.Service{Foundry: foundryv1.NewFoundryServiceClient(conn), Wait: time.Millisecond}
	identity := &integration.Identity{User: true, FoundryGrants: true,
		HasPermission: func(integration.Permission) bool { return true },
		Authorize:     func(context.Context, any, integration.Grant, ...string) error { return nil },
		Allows:        func(string, string, integration.Grant) bool { return true },
		AsGrantee:     func(ctx context.Context) context.Context { return ctx },
		IsGrantee:     func(context.Context) bool { return true }}
	ctx := as(ports.Actor{Source: ports.SourceUser, UserID: "user-9", WorkspaceID: "ws-1"})

	listed, _, err := tools.ListFoundryOntologies(ctx, identity, &integration.Call{}, integration.ListFoundryOntologiesInput{WorkspaceID: "ws-1"})
	require.NoError(t, err)
	var ids []string
	for _, o := range listed.Ontologies {
		ids = append(ids, o.ID)
	}
	assert.Equal(t, []string{"ont-1"}, ids, "Billing, which its grants reach; not Payroll")

	_, _, err = tools.AskFoundryQuestion(ctx, identity, &integration.Call{}, integration.AskFoundryInput{
		OntologyInput: integration.OntologyInput{WorkspaceID: "ws-1", OntologyID: "ont-3"}, Question: "Who is paid most?", ClientToken: "founder-0"})
	assert.Error(t, err, "Payroll isn't theirs to ask")

	asked, _, err := tools.AskFoundryQuestion(ctx, identity, &integration.Call{}, integration.AskFoundryInput{
		OntologyInput: integration.OntologyInput{WorkspaceID: "ws-1", OntologyID: "ont-1"}, Question: "Who pays?", ClientToken: "founder-1"})
	require.NoError(t, err)
	assert.Equal(t, "load-1", asked.LoadID)
	stored := held.questions[len(held.questions)-1]
	require.NotNil(t, stored.Grantee)
	assert.Equal(t, "user-9", stored.Grantee.ID)
	assert.Equal(t, "load-1", stored.LoadID)
	limits := held.limits[len(held.limits)-1]
	assert.Equal(t, s.deps.Cfg.Foundry.Ask.GranteePerHour, limits.GranteePerHour, "a grantee's hourly limit binds it")
}
