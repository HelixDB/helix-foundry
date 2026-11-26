package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Helix Foundry for Slack's bot mode: the API answers a Slack install's bot
// as a principal of its own, within its own hour and the workspace's
// questions, and Settings › Slack turns bot mode on and off. The store's
// rules on MySQL are its own tests'.

// FoundrySlackBotScope is ws-1's Slack bot's scope, as slackBot names it.
func (s *accessStore) FoundrySlackBotScope(_ context.Context, ws, credentialID string) (model.SlackBotScope, error) {
	switch {
	case s.slackBotErr != nil:
		return nil, s.slackBotErr
	case ws != "ws-1" || s.slackBot == "" || credentialID != s.slackBot:
		return nil, foundrystore.ErrNotFound
	case s.slackBotScope == nil:
		return model.SlackBotSources{}, nil
	}
	return s.slackBotScope, nil
}

// limitedFounderStore is founderStore keeping the limits each turn was
// queued under.
type limitedFounderStore struct {
	*founderStore
	limits []model.FoundryAgentLimits
}

func (s *limitedFounderStore) CreateFoundryAgentTurn(ctx context.Context, t *model.FoundryAgentTurn, thread *model.FoundryAgentThread,
	limits model.FoundryAgentLimits, first json.RawMessage) error {
	s.limits = append(s.limits, limits)
	return s.founderStore.CreateFoundryAgentTurn(ctx, t, thread, limits, first)
}

// A Slack install's bot is the agent's key (cred-1) the store names as
// ws-1's bot: it talks to Founder in threads of its own, on Foundry's
// worker, with what its grants give, sending foundry.slack.bot.per_hour an
// hour; it runs, dismisses and prepares no action, and no one else reads
// its threads, nor it theirs.
func TestASlackBotTalksToFounder(t *testing.T) {
	s, held := accessService(t)
	s.deps.Cfg.Foundry.Agent.Enabled = true
	s.deps.Cfg.Foundry.Agent.PerHour, s.deps.Cfg.Foundry.Slack.Bot.PerHour = 7, 99
	held.slackBot = "cred-1"
	hosts := newAgentHosts()
	hosts.set("ws-1", model.FoundryAgentRuntimeClaudeCode)
	s.deps.AgentHosts = hosts
	store := &limitedFounderStore{founderStore: &founderStore{accessStore: held, agent: newAgentStore()}}
	s.deps.DB = store
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	bot := as(agent)
	send := func(ctx context.Context, token, thread string) (*foundryv1.FoundryAgentTurnResponse, error) {
		return client.SendFoundryAgentMessage(ctx, &foundryv1.SendFoundryAgentMessageRequest{WorkspaceId: "ws-1", ClientToken: token, ThreadId: thread,
			Input: textInput("[Slack] Ada wrote:\nWho pays?"), Surface: foundryv1.FoundryAgentSurface_FOUNDRY_AGENT_SURFACE_SLACK})
	}

	sent, err := send(bot, "bot-1", "")
	require.NoError(t, err)
	turn := store.agent.turns[sent.GetTurn().GetId()]
	assert.Equal(t, model.AgentBot{CredentialID: "cred-1"}, turn.Principal)
	thread := store.agent.threads[sent.GetTurn().GetThreadId()]
	assert.Equal(t, model.AgentBot{CredentialID: "cred-1"}, thread.Principal)
	assert.Equal(t, model.FoundryAgentRuntimeAPI, thread.Runtime, "Foundry's worker, whatever the workspace's helpers run")
	require.NotNil(t, thread.Granted, "a grantee's thread")
	assert.ElementsMatch(t, []access.Target{access.ConnectorTarget("conn-1"), access.DocumentSetTarget("set-general")}, thread.Granted.Targets)
	require.Len(t, store.limits, 1)
	assert.Equal(t, 99, store.limits[0].PerHour, "the bot's hour, not a person's")

	again, err := send(bot, "bot-1", "")
	require.NoError(t, err)
	assert.Equal(t, sent.GetTurn().GetId(), again.GetTurn().GetId(), "the same token is the same message")
	next, err := send(bot, "bot-2", thread.ID)
	require.NoError(t, err)
	assert.Equal(t, thread.ID, next.GetTurn().GetThreadId())

	got, err := client.GetFoundryAgentThread(bot, &foundryv1.GetFoundryAgentThreadRequest{WorkspaceId: "ws-1", Id: thread.ID})
	require.NoError(t, err)
	assert.Len(t, got.GetTurns(), 2)
	_, err = client.ListFoundryAgentTurnEvents(bot, &foundryv1.ListFoundryAgentTurnEventsRequest{WorkspaceId: "ws-1", Id: next.GetTurn().GetId()})
	require.NoError(t, err)
	cancelled, err := client.CancelFoundryAgentTurn(bot, &foundryv1.CancelFoundryAgentTurnRequest{WorkspaceId: "ws-1", Id: next.GetTurn().GetId()})
	require.NoError(t, err)
	assert.Equal(t, string(model.FoundryAgentTurnCancelled), cancelled.GetTurn().GetStatus())
	listed, err := client.ListFoundryAgentThreads(bot, &foundryv1.ListFoundryAgentThreadsRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	require.Len(t, listed.GetThreads(), 1)
	assert.Equal(t, thread.ID, listed.GetThreads()[0].GetId())

	// A person's threads aren't the bot's, nor the bot's theirs.
	ada := as(person)
	_, err = client.GetFoundryAgentThread(ada, &foundryv1.GetFoundryAgentThreadRequest{WorkspaceId: "ws-1", Id: thread.ID})
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = send(ada, "ada-1", thread.ID)
	assert.Equal(t, codes.NotFound, status.Code(err))
	theirs, err := send(ada, "ada-2", "")
	require.NoError(t, err)
	assert.Equal(t, 7, store.limits[len(store.limits)-1].PerHour, "a person's hour")
	_, err = client.GetFoundryAgentThread(bot, &foundryv1.GetFoundryAgentThreadRequest{WorkspaceId: "ws-1", Id: theirs.GetTurn().GetThreadId()})
	assert.Equal(t, codes.NotFound, status.Code(err))
	mine, err := client.ListFoundryAgentThreads(ada, &foundryv1.ListFoundryAgentThreadsRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	require.Len(t, mine.GetThreads(), 1, "the bot's threads are never a person's list")
	assert.Equal(t, theirs.GetTurn().GetThreadId(), mine.GetThreads()[0].GetId())

	// Its actions are an owner's or admin's to run, in Slack.
	_, err = client.ExecuteFoundryAgentAction(bot, &foundryv1.ExecuteFoundryAgentActionRequest{WorkspaceId: "ws-1", Id: "a-1",
		PayloadHash: strings.Repeat("a", 64)})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = client.DismissFoundryAgentAction(bot, &foundryv1.DismissFoundryAgentActionRequest{WorkspaceId: "ws-1", Id: "a-1"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))

	_, err = client.DeleteFoundryAgentThread(bot, &foundryv1.DeleteFoundryAgentThreadRequest{WorkspaceId: "ws-1", Id: thread.ID})
	require.NoError(t, err)
	assert.NotContains(t, store.agent.threads, thread.ID)

	// Another key, or the bot once bot mode is off, is no one Founder answers.
	held.slackBot = "cred-other"
	_, err = send(bot, "bot-3", "")
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	held.slackBot, held.slackBotErr = "cred-1", errors.New("down")
	_, err = send(bot, "bot-4", "")
	assert.Equal(t, codes.Internal, status.Code(err))
}

// agentPrincipal answers a person signed in and the workspace's Slack bot,
// and no one else; agentUser only the person.
func TestAgentPrincipal(t *testing.T) {
	s, held := accessService(t)
	s.deps.Cfg.Foundry.Agent.Enabled = true
	held.slackBot = "cred-1"
	for name, tc := range map[string]struct {
		actor ports.Actor
		ws    string
		want  model.FoundryAgentPrincipal // nil: refused
	}{
		"a person":                   {person, "ws-1", model.AgentPerson{UserID: "user-1"}},
		"the workspace's bot":        {agent, "ws-1", model.AgentBot{CredentialID: "cred-1"}},
		"the bot in no workspace":    {agent, "", nil},
		"the bot in another":         {agent, "ws-2", nil},
		"another key":                {ports.Actor{Source: ports.SourceGrantee, ServiceCredentialID: "cred-9", WorkspaceID: "ws-1"}, "ws-1", nil},
		"a key naming a user too":    {ports.Actor{Source: ports.SourceGrantee, ServiceCredentialID: "cred-1", UserID: "user-1"}, "ws-1", nil},
		"the bot's key through MCP":  {ports.Actor{Source: ports.SourceMCP, ServiceCredentialID: "cred-1"}, "ws-1", nil},
		"a person sent as a grantee": {member, "ws-1", nil},
		"a person through MCP":       {ports.Actor{Source: ports.SourceMCP, UserID: "user-1"}, "ws-1", nil},
		"a person's key":             {ports.Actor{Source: ports.SourceUser, UserID: "user-1", ServiceCredentialID: "cred-1"}, "ws-1", nil},
		"no one":                     {ports.Actor{Source: ports.SourceUser}, "ws-1", nil},
	} {
		ctx := ports.WithActor(context.Background(), tc.actor)
		got, err := s.agentPrincipal(ctx, tc.ws)
		if tc.want == nil {
			assert.Equal(t, codes.PermissionDenied, status.Code(err), name)
			continue
		}
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, got, name)
	}
	_, err := s.agentUser(ports.WithActor(context.Background(), agent), "ws-1")
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "the bot is no person")
	userID, err := s.agentUser(ports.WithActor(context.Background(), person), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "user-1", userID)
	_, err = s.agentUser(ports.WithActor(context.Background(), member), "ws-1")
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	held.slackBotErr = errors.New("down")
	_, err = s.agentPrincipal(ports.WithActor(context.Background(), agent), "ws-1")
	assert.Equal(t, codes.Internal, status.Code(err))
	s.deps.Cfg.Foundry.Agent.Enabled = false
	_, err = s.agentPrincipal(ports.WithActor(context.Background(), agent), "ws-1")
	assert.Equal(t, "FOUNDRY_AGENT_DISABLED", reasonOf(err))
}

// The bot, which a whole Slack workspace asks through, asks within the
// workspace's questions, not a grantee's share of them; another key keeps
// its share.
func TestASlackBotAsksWithinTheWorkspacesQuestions(t *testing.T) {
	s, held := accessService(t)
	held.slackBot = "cred-1"
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	ask := s.deps.Cfg.Foundry.Ask
	_, err := client.AskFoundryQuestion(as(agent), &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
		Question: "How many customers?", ClientToken: "bot-1"})
	require.NoError(t, err)
	limits := held.limits[len(held.limits)-1]
	assert.Equal(t, ask.PerHour, limits.GranteePerHour)
	assert.Equal(t, ask.MonthlyMicros(), limits.GranteeCapMicros)
	listed, err := client.ListFoundryQuestions(as(agent), &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
	require.NoError(t, err)
	assert.Equal(t, ask.MonthlyMicros(), listed.GetBudget().GetMonthlyUsdMicros())
	assert.Equal(t, int32(ask.PerHour), listed.GetBudget().GetHourlyLimit())

	held.slackBot = ""
	_, err = client.AskFoundryQuestion(as(agent), &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
		Question: "How many customers?", ClientToken: "key-1"})
	require.NoError(t, err)
	limits = held.limits[len(held.limits)-1]
	assert.Equal(t, ask.GranteePerHour, limits.GranteePerHour, "another key keeps a grantee's share")
	assert.Equal(t, ask.GranteeMonthlyMicros(), limits.GranteeCapMicros)

	held.slackBotErr = errors.New("down")
	_, err = client.AskFoundryQuestion(as(agent), &foundryv1.AskFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
		Question: "How many customers?", ClientToken: "key-2"})
	assert.Equal(t, codes.Internal, status.Code(err))
	_, err = client.ListFoundryQuestions(as(agent), &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
	assert.Equal(t, codes.Internal, status.Code(err))
	// The host's caller and a person read no install.
	_, err = client.ListFoundryQuestions(as(person), &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
	require.NoError(t, err)
}

// botSlackStore is slackStore with bot mode.
type botSlackStore struct {
	*slackStore
	// deleted are the principals whose grants were dropped.
	deleted           []access.Principal
	setErr, deleteErr error
	// raced, when set, is the mode another click leaves just before this
	// one's write.
	raced model.SlackIdentityMode
}

func (s *botSlackStore) SetFoundrySlackMode(_ context.Context, ws, installID string, mode model.SlackIdentityMode) (model.SlackIdentityMode, error) {
	if s.setErr != nil {
		return nil, s.setErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range s.installs {
		if i.ID != installID || i.WorkspaceID != ws || i.RevokedAt != nil {
			continue
		}
		if s.raced != nil {
			i.Mode, s.raced = s.raced, nil
		}
		was := i.Mode
		bot, wasBot := was.(model.SlackSharedBot)
		if next, ok := mode.(model.SlackSharedBot); ok && wasBot && bot.CredentialID != next.CredentialID {
			return nil, foundrystore.ErrConflict
		}
		i.Mode = mode
		return was, nil
	}
	return nil, foundrystore.ErrNotFound
}

func (s *botSlackStore) DeleteFoundryAccessPrincipal(_ context.Context, _ string, p access.Principal) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, p)
	return nil
}

// botKeys is the host's service credentials for Slack bots.
type botKeys struct {
	keys    map[string]*botKey
	made    []string // "<workspace> <name> <by>"
	revoked []string
	n       int
	// Errors each call answers with, when set; refuse fails one key's revoke.
	createErr, getErr, revokeErr error
	refuse                       string
}

type botKey struct {
	workspaceID, name string
	usable            bool
}

func (k *botKeys) Create(_ context.Context, ws, name, by string) (string, error) {
	if k.createErr != nil {
		return "", k.createErr
	}
	k.n++
	id := fmt.Sprintf("cred-bot-%d", k.n)
	k.keys[id] = &botKey{workspaceID: ws, name: name, usable: true}
	k.made = append(k.made, ws+" "+name+" "+by)
	return id, nil
}

func (k *botKeys) Get(_ context.Context, ws, id string) (ports.BotCredential, error) {
	if k.getErr != nil {
		return ports.BotCredential{}, k.getErr
	}
	key, ok := k.keys[id]
	if !ok || key.workspaceID != ws {
		return ports.BotCredential{}, foundrystore.ErrNotFound
	}
	return ports.BotCredential{Name: key.name, Usable: key.usable}, nil
}

func (k *botKeys) Revoke(_ context.Context, _, id string) error {
	if k.revokeErr != nil {
		return k.revokeErr
	}
	if id == k.refuse {
		return errors.New("refused")
	}
	k.revoked = append(k.revoked, id)
	if key, ok := k.keys[id]; ok {
		key.usable = false
	}
	return nil
}

// newBotFixture is a Slack fixture with ws-1's install in mode, bot mode
// stored, and the host's bot keys.
func newBotFixture(t *testing.T, mode model.SlackIdentityMode) (slackFixture, *botSlackStore, *botKeys) {
	t.Helper()
	f := newSlackFixture(t)
	f.db.install(t, f.keys, mode)
	db, keys := &botSlackStore{slackStore: f.db}, &botKeys{keys: map[string]*botKey{}}
	if bot, ok := mode.(model.SlackSharedBot); ok {
		keys.keys[bot.CredentialID] = &botKey{workspaceID: slackWS, name: "Slack bot (Acme)", usable: true}
	}
	f.s.deps.DB, f.s.deps.BotCredentials = db, keys
	return f, db, keys
}

func updateSlack(f slackFixture, user string, mode foundryv1.FoundrySlackIdentityMode) (*foundryv1.FoundrySlack, error) {
	resp, err := f.s.UpdateFoundrySlack(as(user), &foundryv1.UpdateFoundrySlackRequest{WorkspaceId: slackWS, IdentityMode: mode})
	return resp.GetSlack(), err
}

func updateSlackScope(f slackFixture, user string, mode foundryv1.FoundrySlackIdentityMode, scope foundryv1.FoundrySlackBotScope) (*foundryv1.FoundrySlack, error) {
	resp, err := f.s.UpdateFoundrySlack(as(user), &foundryv1.UpdateFoundrySlackRequest{WorkspaceId: slackWS, IdentityMode: mode, BotScope: scope})
	return resp.GetSlack(), err
}

const (
	botMode        = foundryv1.FoundrySlackIdentityMode_FOUNDRY_SLACK_IDENTITY_MODE_BOT
	perPersonMode  = foundryv1.FoundrySlackIdentityMode_FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON
	workspaceScope = foundryv1.FoundrySlackBotScope_FOUNDRY_SLACK_BOT_SCOPE_WORKSPACE
	sourcesScope   = foundryv1.FoundrySlackBotScope_FOUNDRY_SLACK_BOT_SCOPE_SOURCES
)

func TestUpdateFoundrySlack(t *testing.T) {
	t.Run("bot mode makes the bot's key, once", func(t *testing.T) {
		f, db, keys := newBotFixture(t, model.SlackPerPerson{})
		got, err := updateSlack(f, "user-3", botMode)
		require.NoError(t, err)
		assert.Equal(t, []string{"ws-1 Slack bot (Acme) user-3"}, keys.made)
		assert.Equal(t, model.SlackSharedBot{CredentialID: "cred-bot-1", Scope: model.SlackBotWorkspace{}}, db.installs[0].Mode)
		assert.Equal(t, botMode, got.GetInstall().GetIdentityMode())
		assert.Equal(t, "cred-bot-1", got.GetInstall().GetBot().GetCredentialId())
		assert.Equal(t, "Slack bot (Acme)", got.GetInstall().GetBot().GetCredentialName())
		assert.False(t, got.GetInstall().GetBot().GetBroken())
		_, err = updateSlack(f, "user-1", botMode)
		require.NoError(t, err)
		assert.Len(t, keys.made, 1, "the mode it has")
		assert.Empty(t, keys.revoked)
	})
	t.Run("a new bot reads the workspace unless asked otherwise", func(t *testing.T) {
		f, db, _ := newBotFixture(t, model.SlackPerPerson{})
		got, err := updateSlack(f, "user-1", botMode)
		require.NoError(t, err)
		assert.Equal(t, model.SlackBotWorkspace{}, db.installs[0].Mode.(model.SlackSharedBot).Scope)
		assert.Equal(t, workspaceScope, got.GetInstall().GetBot().GetScope())

		f, db, _ = newBotFixture(t, model.SlackPerPerson{})
		got, err = updateSlackScope(f, "user-1", botMode, sourcesScope)
		require.NoError(t, err)
		assert.Equal(t, model.SlackSharedBot{CredentialID: "cred-bot-1", Scope: model.SlackBotSources{}}, db.installs[0].Mode)
		assert.Equal(t, sourcesScope, got.GetInstall().GetBot().GetScope())
	})
	t.Run("a bot's scope changes in place", func(t *testing.T) {
		f, db, keys := newBotFixture(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotWorkspace{}})
		got, err := updateSlackScope(f, "user-3", botMode, sourcesScope)
		require.NoError(t, err)
		assert.Equal(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotSources{}}, db.installs[0].Mode)
		assert.Equal(t, sourcesScope, got.GetInstall().GetBot().GetScope())
		assert.Empty(t, keys.made, "the same key")
		assert.Empty(t, keys.revoked)
		assert.Empty(t, db.deleted, "its grants stay, for its sources")
		got, err = updateSlack(f, "user-1", botMode)
		require.NoError(t, err)
		assert.Equal(t, sourcesScope, got.GetInstall().GetBot().GetScope(), "no scope keeps it")
		got, err = updateSlackScope(f, "user-1", botMode, workspaceScope)
		require.NoError(t, err)
		assert.Equal(t, workspaceScope, got.GetInstall().GetBot().GetScope())
		assert.Equal(t, model.SlackBotWorkspace{}, db.installs[0].Mode.(model.SlackSharedBot).Scope)
	})
	t.Run("a scope that can't be set", func(t *testing.T) {
		f, db, _ := newBotFixture(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotWorkspace{}})
		for name, tc := range map[string]struct {
			mode  foundryv1.FoundrySlackIdentityMode
			scope foundryv1.FoundrySlackBotScope
			user  string
			want  codes.Code
		}{
			"per person with a scope": {perPersonMode, sourcesScope, "user-1", codes.InvalidArgument},
			"an unknown scope":        {botMode, 9, "user-1", codes.InvalidArgument},
			"a member":                {botMode, sourcesScope, "user-2", codes.PermissionDenied},
		} {
			_, err := updateSlackScope(f, tc.user, tc.mode, tc.scope)
			assert.Equal(t, tc.want, status.Code(err), name)
		}
		_, err := f.s.UpdateFoundrySlack(ports.WithActor(context.Background(), agent), &foundryv1.UpdateFoundrySlackRequest{WorkspaceId: slackWS,
			IdentityMode: botMode, BotScope: workspaceScope})
		assert.Equal(t, codes.PermissionDenied, status.Code(err), "a key, the bot's own included")
		assert.Equal(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotWorkspace{}}, db.installs[0].Mode, "unchanged")
		// A member reads it.
		resp, err := f.s.GetFoundrySlack(as("user-2"), &foundryv1.GetFoundrySlackRequest{WorkspaceId: slackWS})
		require.NoError(t, err)
		assert.Equal(t, workspaceScope, resp.GetSlack().GetInstall().GetBot().GetScope())

		// Another click turned bot mode off and on again meanwhile: the newer
		// bot keeps its scope.
		db.raced = model.SlackSharedBot{CredentialID: "cred-newer", Scope: model.SlackBotWorkspace{}}
		_, err = updateSlackScope(f, "user-1", botMode, sourcesScope)
		assert.Equal(t, codes.Aborted, status.Code(err))
		assert.Equal(t, model.SlackSharedBot{CredentialID: "cred-newer", Scope: model.SlackBotWorkspace{}}, db.installs[0].Mode)
		for name, tc := range map[string]struct {
			err  error
			want codes.Code
		}{
			"the install went": {foundrystore.ErrNotFound, codes.NotFound},
			"the store down":   {errors.New("down"), codes.Internal},
		} {
			db.setErr = tc.err
			_, err = updateSlackScope(f, "user-1", botMode, sourcesScope)
			assert.Equal(t, tc.want, status.Code(err), name)
		}
	})
	t.Run("a long team name is cut to the key's 64 characters", func(t *testing.T) {
		f, _, keys := newBotFixture(t, model.SlackPerPerson{})
		f.db.installs[0].TeamName = strings.Repeat("é", 80)
		got, err := updateSlack(f, "user-1", botMode)
		require.NoError(t, err)
		name := got.GetInstall().GetBot().GetCredentialName()
		assert.Equal(t, 64, utf8.RuneCountInString(name))
		assert.True(t, strings.HasPrefix(name, "Slack bot (éé"))
		assert.True(t, strings.HasSuffix(name, "é…)"))
		assert.Len(t, keys.made, 1)
		f.db.installs[0].TeamName = strings.Repeat("a", 52) // exactly 64
		f.db.installs[0].Mode = model.SlackPerPerson{}
		got, err = updateSlack(f, "user-1", botMode)
		require.NoError(t, err)
		assert.Equal(t, "Slack bot ("+strings.Repeat("a", 52)+")", got.GetInstall().GetBot().GetCredentialName())
	})
	t.Run("per person revokes the key first, then retires the bot", func(t *testing.T) {
		f, db, keys := newBotFixture(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotWorkspace{}})
		got, err := updateSlack(f, "user-1", perPersonMode)
		require.NoError(t, err)
		assert.Equal(t, []string{"cred-bot-0"}, keys.revoked)
		assert.Equal(t, []access.Principal{{Kind: access.PrincipalServiceCredential, ID: "cred-bot-0"}}, db.deleted)
		assert.Equal(t, model.SlackPerPerson{}, db.installs[0].Mode)
		assert.Equal(t, perPersonMode, got.GetInstall().GetIdentityMode())
		assert.Nil(t, got.GetInstall().GetBot())
		_, err = updateSlack(f, "user-1", perPersonMode)
		require.NoError(t, err)
		assert.Len(t, keys.revoked, 1, "the mode it has")
	})
	t.Run("a key that can't be revoked leaves bot mode on, to try again", func(t *testing.T) {
		f, db, keys := newBotFixture(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotWorkspace{}})
		keys.revokeErr = errors.New("down")
		_, err := updateSlack(f, "user-1", perPersonMode)
		assert.Equal(t, codes.Unavailable, status.Code(err))
		assert.Equal(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotWorkspace{}}, db.installs[0].Mode)
		keys.revokeErr, db.deleteErr = nil, errors.New("down")
		_, err = updateSlack(f, "user-1", perPersonMode)
		assert.Equal(t, codes.Unavailable, status.Code(err), "nor its grants dropped")
		assert.Equal(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotWorkspace{}}, db.installs[0].Mode)
		db.deleteErr = nil
		_, err = updateSlack(f, "user-1", perPersonMode)
		require.NoError(t, err)
		assert.Equal(t, []string{"cred-bot-0", "cred-bot-0"}, keys.revoked, "a revoked key revoked again")
		assert.Equal(t, model.SlackPerPerson{}, db.installs[0].Mode)
	})
	t.Run("clicks that race", func(t *testing.T) {
		// Another click made the bot first: this one's key goes unused.
		f, db, keys := newBotFixture(t, model.SlackPerPerson{})
		db.raced = model.SlackSharedBot{CredentialID: "cred-other", Scope: model.SlackBotWorkspace{}}
		keys.keys["cred-other"] = &botKey{workspaceID: slackWS, name: "Slack bot (Acme)", usable: true}
		got, err := updateSlack(f, "user-1", botMode)
		require.NoError(t, err)
		assert.Equal(t, []string{"cred-bot-1"}, keys.revoked)
		assert.Equal(t, "cred-other", got.GetInstall().GetBot().GetCredentialId())
		// Another click turned it off and on again: both keys go.
		db.raced = model.SlackSharedBot{CredentialID: "cred-newer", Scope: model.SlackBotWorkspace{}}
		_, err = updateSlack(f, "user-1", perPersonMode)
		require.NoError(t, err)
		assert.Equal(t, []string{"cred-bot-1", "cred-other", "cred-newer"}, keys.revoked)
		assert.Equal(t, model.SlackPerPerson{}, db.installs[0].Mode)
		// The other key's revoke failing is logged: the mode is off.
		db.installs[0].Mode, db.raced = model.SlackSharedBot{CredentialID: "cred-a", Scope: model.SlackBotWorkspace{}}, model.SlackSharedBot{CredentialID: "cred-b", Scope: model.SlackBotWorkspace{}}
		keys.revoked, keys.refuse = nil, "cred-b"
		_, err = updateSlack(f, "user-1", perPersonMode)
		require.NoError(t, err)
		assert.Equal(t, []string{"cred-a"}, keys.revoked)
		assert.Equal(t, model.SlackPerPerson{}, db.installs[0].Mode)
	})
	t.Run("an unused key is revoked when the write fails", func(t *testing.T) {
		for name, tc := range map[string]struct {
			err  error
			want codes.Code
		}{
			"the install went": {foundrystore.ErrNotFound, codes.NotFound},
			"the store down":   {errors.New("down"), codes.Internal},
		} {
			f, db, keys := newBotFixture(t, model.SlackPerPerson{})
			db.setErr = tc.err
			_, err := updateSlack(f, "user-1", botMode)
			assert.Equal(t, tc.want, status.Code(err), name)
			assert.Equal(t, []string{"cred-bot-1"}, keys.revoked, name)
			// Its revoke failing too is logged.
			keys.revokeErr = errors.New("down")
			_, err = updateSlack(f, "user-1", botMode)
			assert.Equal(t, tc.want, status.Code(err), name)
		}
	})
	t.Run("turning it off fails in the store", func(t *testing.T) {
		for name, tc := range map[string]struct {
			err  error
			want codes.Code
		}{
			"the install went": {foundrystore.ErrNotFound, codes.NotFound},
			"the store down":   {errors.New("down"), codes.Internal},
		} {
			f, db, _ := newBotFixture(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotWorkspace{}})
			db.setErr = tc.err
			_, err := updateSlack(f, "user-1", perPersonMode)
			assert.Equal(t, tc.want, status.Code(err), name)
		}
	})
	t.Run("a host without bot keys", func(t *testing.T) {
		f, _, _ := newBotFixture(t, model.SlackPerPerson{})
		f.s.deps.BotCredentials = nil
		_, err := updateSlack(f, "user-1", botMode)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_BOT_HOSTED_ONLY", reason)
		_, err = updateSlack(f, "user-1", perPersonMode)
		require.NoError(t, err, "the mode it has")
		// A bot it holds still turns off: its grants are dropped.
		f, db, _ := newBotFixture(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotWorkspace{}})
		f.s.deps.BotCredentials = nil
		got, err := updateSlack(f, "user-1", perPersonMode)
		require.NoError(t, err)
		assert.Equal(t, perPersonMode, got.GetInstall().GetIdentityMode())
		assert.Equal(t, []access.Principal{{Kind: access.PrincipalServiceCredential, ID: "cred-bot-0"}}, db.deleted)
	})
	t.Run("refused", func(t *testing.T) {
		f, db, keys := newBotFixture(t, model.SlackPerPerson{})
		_, err := f.s.UpdateFoundrySlack(as("user-1"), &foundryv1.UpdateFoundrySlackRequest{IdentityMode: botMode})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "no workspace")
		for _, mode := range []foundryv1.FoundrySlackIdentityMode{foundryv1.FoundrySlackIdentityMode_FOUNDRY_SLACK_IDENTITY_MODE_UNSPECIFIED, 9} {
			_, err = updateSlack(f, "user-1", mode)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), mode)
		}
		_, err = updateSlack(f, "user-2", botMode)
		assert.Equal(t, codes.PermissionDenied, status.Code(err), "a member")
		_, err = f.s.UpdateFoundrySlack(ports.WithActor(context.Background(), agent), &foundryv1.UpdateFoundrySlackRequest{WorkspaceId: slackWS,
			IdentityMode: botMode})
		assert.Equal(t, codes.PermissionDenied, status.Code(err), "a key")
		keys.createErr = errors.New("down")
		_, err = updateSlack(f, "user-1", botMode)
		assert.Equal(t, codes.Unavailable, status.Code(err))
		assert.Equal(t, model.SlackPerPerson{}, db.installs[0].Mode)
		db.fail = errors.New("down")
		_, err = updateSlack(f, "user-1", botMode)
		assert.Equal(t, codes.Internal, status.Code(err))
		db.fail = nil
		db.installs = nil
		_, err = updateSlack(f, "user-1", botMode)
		assert.Equal(t, codes.NotFound, status.Code(err), "no install")
		f.cfg.Foundry.Slack.Enabled = false
		_, err = updateSlack(f, "user-1", botMode)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_DISABLED", reason)
	})
}

// Settings › Slack says whom the install acts as and, in bot mode, its key
// as the host holds it now, without opening a session.
func TestGetFoundrySlackBot(t *testing.T) {
	get := func(f slackFixture) *foundryv1.FoundrySlackInstall {
		resp, err := f.s.GetFoundrySlack(as("user-2"), &foundryv1.GetFoundrySlackRequest{WorkspaceId: slackWS})
		require.NoError(t, err)
		return resp.GetSlack().GetInstall()
	}
	f, _, _ := newBotFixture(t, model.SlackPerPerson{})
	got := get(f)
	assert.Equal(t, perPersonMode, got.GetIdentityMode())
	assert.Nil(t, got.GetBot())

	f, _, keys := newBotFixture(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotWorkspace{}})
	got = get(f)
	assert.Equal(t, botMode, got.GetIdentityMode())
	assert.Equal(t, "cred-bot-0", got.GetBot().GetCredentialId())
	assert.Equal(t, "Slack bot (Acme)", got.GetBot().GetCredentialName())
	assert.False(t, got.GetBot().GetBroken())
	keys.keys["cred-bot-0"].usable = false
	assert.True(t, get(f).GetBot().GetBroken(), "revoked, expired or granted databases")
	delete(keys.keys, "cred-bot-0")
	assert.True(t, get(f).GetBot().GetBroken(), "gone")
	keys.getErr = errors.New("down")
	got = get(f)
	assert.False(t, got.GetBot().GetBroken(), "unread")
	assert.Empty(t, got.GetBot().GetCredentialName())
	f.s.deps.BotCredentials = nil
	got = get(f)
	assert.Equal(t, "cred-bot-0", got.GetBot().GetCredentialId(), "a host without bot keys")
	assert.Empty(t, got.GetBot().GetCredentialName())
}

// Disconnecting a bot-mode install revokes its key; a revoke that fails
// leaves the install disconnected.
func TestDisconnectFoundrySlackBot(t *testing.T) {
	f, db, keys := newBotFixture(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotWorkspace{}})
	_, err := f.s.DisconnectFoundrySlack(as("user-1"), &foundryv1.DisconnectFoundrySlackRequest{WorkspaceId: slackWS})
	require.NoError(t, err)
	assert.Equal(t, []string{"cred-bot-0"}, keys.revoked)
	assert.Equal(t, []access.Principal{{Kind: access.PrincipalServiceCredential, ID: "cred-bot-0"}}, db.deleted)

	f, _, keys = newBotFixture(t, model.SlackSharedBot{CredentialID: "cred-bot-0", Scope: model.SlackBotWorkspace{}})
	keys.revokeErr = errors.New("down")
	got, err := f.s.DisconnectFoundrySlack(as("user-1"), &foundryv1.DisconnectFoundrySlackRequest{WorkspaceId: slackWS})
	require.NoError(t, err)
	assert.Nil(t, got.GetSlack().GetInstall())
}
