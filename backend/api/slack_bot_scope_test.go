package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The Slack bot's scope decides how the API reads its key (the agent actor,
// cred-1): the whole workspace's bot is an access.Reader, which reads what
// an unrestricted member reads with no grant at all, and changes nothing; a
// bot of its sources is a grantee, narrowed to them.

// workspaceBot is accessService with cred-1 ws-1's Slack bot of scope, and
// none of its grants.
func workspaceBot(t *testing.T, scope model.SlackBotScope) (*grpc.ClientConn, *accessStore, func(ports.Actor) context.Context) {
	t.Helper()
	s, held := accessService(t)
	held.slackBot, held.slackBotScope = "cred-1", scope
	held.grants = nil
	conn, as := served(t, s)
	return conn, held, as
}

func TestAWorkspaceBotReadsTheWholeWorkspace(t *testing.T) {
	conn, held, as := workspaceBot(t, model.SlackBotWorkspace{})
	client, bot := foundryv1.NewFoundryServiceClient(conn), as(agent)

	listed, err := client.ListFoundryOntologies(bot, &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	var names []string
	for _, o := range listed.GetOntologies() {
		names = append(names, o.GetName())
	}
	assert.Contains(t, names, "Billing")
	assert.Contains(t, names, "Payroll", "an ontology no grant holds")
	got, err := client.GetFoundryOntology(bot, &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-3"})
	require.NoError(t, err)
	assert.Equal(t, "tenant-9", got.GetOntology().GetTenantId(), "read as a member reads it, not narrowed")
	_, err = client.GetFoundryLoad(bot, &foundryv1.GetFoundryLoadRequest{WorkspaceId: "ws-1", OntologyId: "ont-3", Id: "load-3"})
	require.NoError(t, err)

	mine, err := client.GetMyFoundryAccess(bot, &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	assert.True(t, mine.GetReadsWorkspace())
	assert.False(t, mine.GetGrantee())
	assert.False(t, mine.GetWorkflows(), "a reader runs no workflow")
	assert.Equal(t, []string{"ont-1", "ont-3"}, mine.GetOntologyIds())
	assert.Empty(t, mine.GetPartialOntologyIds())

	// Its question is its own, of the whole load, within the workspace's
	// questions.
	asked, err := client.AskFoundryQuestion(bot, &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-3",
		Question: "Who is paid most?", ClientToken: "bot-1"})
	require.NoError(t, err)
	q := held.questions[len(held.questions)-1]
	assert.Equal(t, asked.GetQuestion().GetId(), q.ID)
	assert.Equal(t, &agentKey, q.Grantee, "the bot's own")
	assert.Nil(t, q.Seen, "the whole load")
	assert.Equal(t, "load-3", q.LoadID)
	limits := held.limits[len(held.limits)-1]
	assert.Equal(t, limits.PerHour, limits.GranteePerHour, "the workspace's questions")
	_, err = client.GetFoundryQuestion(bot, &foundryv1.GetFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-3", Id: q.ID})
	require.NoError(t, err)

	// A person's question is theirs: the bot neither reads nor cancels it.
	theirs, err := client.AskFoundryQuestion(as(person), &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-3",
		Question: "Who joined last?", ClientToken: "ada-1"})
	require.NoError(t, err)
	_, err = client.CancelFoundryQuestion(bot, &foundryv1.CancelFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-3",
		Id: theirs.GetQuestion().GetId()})
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = client.CancelFoundryQuestion(bot, &foundryv1.CancelFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-3", Id: q.ID})
	require.NoError(t, err, "its own")
}

// A reader is admitted only where access.ReaderAdmits, in its own
// workspace: every write, prepare and run, every method a grantee is
// refused, and workflows, are refused before any handler.
func TestAWorkspaceBotChangesNothing(t *testing.T) {
	conn, _, as := workspaceBot(t, model.SlackBotWorkspace{})
	client, bot := foundryv1.NewFoundryServiceClient(conn), as(agent)
	refused := 0
	for _, m := range servedMethods(t) {
		if access.ReaderAdmits(fullMethod(m)) {
			continue
		}
		refused++
		req := newMessage(t, m.Input())
		if f := m.Input().Fields().ByName("workspace_id"); f != nil {
			req.ProtoReflect().Set(f, protoreflect.ValueOfString("ws-1"))
		}
		if f := m.Input().Fields().ByName("ontology_id"); f != nil {
			req.ProtoReflect().Set(f, protoreflect.ValueOfString("ont-1"))
		}
		err := conn.Invoke(bot, fullMethod(m), req, newMessage(t, m.Output()))
		assert.Equal(t, codes.PermissionDenied, status.Code(err), "%s", m.Name())
	}
	assert.Greater(t, refused, 60)
	for _, m := range []string{
		foundryv1.FoundryService_ExecuteFoundryAgentAction_FullMethodName, foundryv1.FoundryService_CreateFoundryLoad_FullMethodName,
		foundryv1.FoundryService_StartFoundryWorkflowRun_FullMethodName, foundryv1.FoundryService_ListFoundryWorkflows_FullMethodName,
		foundryv1.FoundryService_SetFoundryAccessGrants_FullMethodName, foundryv1.FoundryService_UpdateFoundrySlack_FullMethodName,
	} {
		assert.False(t, access.ReaderAdmits(m), m)
	}

	// Only its own workspace.
	_, err := client.ListFoundryOntologies(bot, &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-2"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = client.AskFoundryQuestion(bot, &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-2", OntologyId: "ont-2", Question: "Hi?",
		ClientToken: "bot-2"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	// Its actions are an owner's or admin's to run, in Slack.
	_, err = client.ExecuteFoundryAgentAction(bot, &foundryv1.ExecuteFoundryAgentActionRequest{WorkspaceId: "ws-1", Id: "a-1"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
}

// A bot of its sources reads only them, as before scopes; the same key no
// install names is a grantee too; and a scope that can't be read refuses
// the call rather than guess.
func TestASourcesBotIsStillAGrantee(t *testing.T) {
	conn, held, as := workspaceBot(t, model.SlackBotSources{})
	client, bot := foundryv1.NewFoundryServiceClient(conn), as(agent)
	listed, err := client.ListFoundryOntologies(bot, &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	assert.Empty(t, listed.GetOntologies(), "no grants, nothing read")
	_, err = client.GetFoundryOntology(bot, &foundryv1.GetFoundryOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-3"})
	assert.Equal(t, codes.NotFound, status.Code(err))
	mine, err := client.GetMyFoundryAccess(bot, &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	assert.True(t, mine.GetGrantee())
	assert.False(t, mine.GetReadsWorkspace())

	held.grants = []model.FoundryAccessGrant{{ID: "g1", WorkspaceID: "ws-1", Principal: agentKey, Target: access.ConnectorTarget("conn-1")},
		{ID: "g2", WorkspaceID: "ws-1", Principal: agentKey, Target: access.DocumentSetTarget("set-general")}}
	listed, err = client.ListFoundryOntologies(bot, &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	require.Len(t, listed.GetOntologies(), 1)
	assert.Equal(t, "Billing", listed.GetOntologies()[0].GetName())

	held.slackBot = ""
	mine, err = client.GetMyFoundryAccess(bot, &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	assert.True(t, mine.GetGrantee(), "a key no install names")
	assert.False(t, mine.GetReadsWorkspace())

	held.slackBot, held.slackBotErr = "cred-1", foundrystore.ErrConflict
	_, err = client.ListFoundryOntologies(bot, &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws-1"})
	assert.Equal(t, codes.Internal, status.Code(err))
}

// The host's own call for the bot's key (its discovery, sent as the key
// through the MCP rather than as a grantee) learns it reads the workspace,
// so its tools are offered with no grant.
func TestAWorkspaceBotsKeySaysWhatItReads(t *testing.T) {
	conn, held, as := workspaceBot(t, model.SlackBotWorkspace{})
	client := foundryv1.NewFoundryServiceClient(conn)
	key := as(ports.Actor{Source: ports.SourceMCP, ServiceCredentialID: "cred-1", WorkspaceID: "ws-1"})
	mine, err := client.GetMyFoundryAccess(key, &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	assert.True(t, mine.GetReadsWorkspace())
	assert.Len(t, mine.GetOntologyIds(), 2)
	held.slackBotScope = model.SlackBotSources{}
	mine, err = client.GetMyFoundryAccess(key, &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	assert.False(t, mine.GetReadsWorkspace())
	assert.False(t, mine.GetHasGrants())
}

// The whole workspace's bot talks to Founder in threads that hold no grants,
// as a member's don't: one it ran then is no thread of its sources once its
// scope narrows.
func TestAWorkspaceBotsThreadsHoldTheWholeView(t *testing.T) {
	s, held := accessService(t)
	s.deps.Cfg.Foundry.Agent.Enabled = true
	held.slackBot, held.slackBotScope = "cred-1", model.SlackBotWorkspace{}
	store := &founderStore{accessStore: held, agent: newAgentStore()}
	s.deps.DB = store
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	bot := as(agent)
	sent, err := client.SendFoundryAgentMessage(bot, &foundryv1.SendFoundryAgentMessageRequest{WorkspaceId: "ws-1", ClientToken: "bot-1",
		Input: textInput("[Slack] Ada wrote:\nWho pays?"), Surface: foundryv1.FoundryAgentSurface_FOUNDRY_AGENT_SURFACE_SLACK})
	require.NoError(t, err)
	thread := store.agent.threads[sent.GetTurn().GetThreadId()]
	assert.Equal(t, model.AgentBot{CredentialID: "cred-1"}, thread.Principal)
	assert.Nil(t, thread.Granted, "the whole view's thread")
	_, err = client.GetFoundryAgentThread(bot, &foundryv1.GetFoundryAgentThreadRequest{WorkspaceId: "ws-1", Id: thread.ID})
	require.NoError(t, err)

	held.slackBotScope = model.SlackBotSources{}
	_, err = client.GetFoundryAgentThread(bot, &foundryv1.GetFoundryAgentThreadRequest{WorkspaceId: "ws-1", Id: thread.ID})
	assert.Equal(t, codes.NotFound, status.Code(err), "narrowed, the bot no longer reads it")
	_, err = client.SendFoundryAgentMessage(bot, &foundryv1.SendFoundryAgentMessageRequest{WorkspaceId: "ws-1", ClientToken: "bot-2",
		ThreadId: thread.ID, Input: textInput("[Slack] Ada wrote:\nAnd now?"), Surface: foundryv1.FoundryAgentSurface_FOUNDRY_AGENT_SURFACE_SLACK})
	assert.Equal(t, codes.NotFound, status.Code(err))
}
