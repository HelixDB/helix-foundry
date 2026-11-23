package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	botKey   = "cred1"
	approver = "user-grace"
)

// botMessage is a message the bot read in Slack.
func botMessage(text string) *model.FoundryAgentTurn {
	turn := message("[Slack] Bea wrote:\n" + text)
	turn.Principal, turn.Surface = model.AgentBot{CredentialID: botKey}, model.FoundryAgentSurfaceSlack
	return turn
}

// ClaimFoundryAgentProposal claims a bot's prepared proposal as the store
// does, refusing as it refuses.
func (s *memStore) ClaimFoundryAgentProposal(_ context.Context, _, id, payloadHash, approverID string) (*model.FoundryAgentAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.actions {
		if _, bot := a.Principal.(model.AgentBot); a.ID != id || !bot {
			continue
		}
		switch {
		case a.Status != model.FoundryAgentActionPrepared:
			return nil, model.FoundryAgentRefusal{Reason: model.FoundryAgentNotPrepared, Detail: "the action is " + string(a.Status)}
		case a.PayloadHash != payloadHash:
			return nil, model.FoundryAgentRefusal{Reason: model.FoundryAgentChanged, Detail: "the action shown is not the one prepared"}
		}
		now := time.Now()
		a.Status, a.DecidedBy, a.DecidedAt = model.FoundryAgentActionExecuting, &approverID, &now
		return a, nil
	}
	return nil, foundrystore.ErrNotFound
}

func (b *backend) CreateFoundryAutomation(_ context.Context, r *foundryv1.CreateFoundryAutomationRequest, _ ...grpc.CallOption) (
	*foundryv1.FoundryAutomationResponse, error) {
	revision := int64(1)
	if b.unrevised {
		revision = 0
	}
	return &foundryv1.FoundryAutomationResponse{Automation: &foundryv1.FoundryAutomation{Id: "wf1", Name: r.GetName(), SpecJson: r.GetSpecJson(),
		Revision: revision}}, nil
}

func (b *backend) ApproveFoundryAutomation(_ context.Context, r *foundryv1.ApproveFoundryAutomationRequest, _ ...grpc.CallOption) (
	*foundryv1.FoundryAutomationResponse, error) {
	if b.approveErr != nil {
		return nil, b.approveErr
	}
	b.approved = append(b.approved, r)
	return &foundryv1.FoundryAutomationResponse{}, nil
}

// A bot's turn opens a grantee session of its key, offers BotTools, is told
// it is the shared bot, and proposes what it would change: nothing is drawn,
// confirmed or run, and its event names the action alone.
func TestABotsTurnReadsAsItsKeyAndOnlyProposes(t *testing.T) {
	h := newHarness(t,
		call("toolu_1", toolPrepare, `{"operation":"update_dashboard","target":"dashboard:ws1/dash1","payload":{"expected_revision":2,"name":"Revenue by month"}}`),
		call("toolu_2", toolPrepare, `{"operation":"create_dashboard","target":"workspace:ws1","payload":{"name":"Pipeline"}}`),
		say("An owner or admin will review both."))
	h.runner.ProposalTTL = 24 * time.Hour
	// A bot's reach is its own: the API reading its key's access (for the
	// channels it reads, threadReach) as the whole view changes nothing.
	h.backend.grantee = false
	turn := botMessage("Rename the revenue dashboard and make me a pipeline dashboard")
	h.db.turns = []*model.FoundryAgentTurn{turn}
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)

	assert.Equal(t, []ports.Actor{{Source: ports.SourceGrantee, ServiceCredentialID: botKey, WorkspaceID: ws}}, h.host.actors, "it acts as its key")
	require.Len(t, h.host.confirms, 1)
	assert.IsType(t, integration.ConfirmByAdmin{}, h.host.confirms[0])
	var offered []string
	for _, tool := range h.model.requests[0].Tools {
		offered = append(offered, tool.Name)
	}
	assert.ElementsMatch(t, slices.Collect(maps.Keys(BotTools)), offered)
	system := h.model.requests[0].System
	assert.Contains(t, system, botPrompt)
	assert.Contains(t, system, slackPrompt)
	assert.NotContains(t, system, granteePrompt)

	require.Len(t, h.db.actions, 2)
	before := time.Now()
	for i, a := range h.db.actions {
		assert.Equal(t, model.AgentBot{CredentialID: botKey}, a.Principal)
		assert.Equal(t, model.AdminApproval{}, a.Approval)
		assert.NoError(t, a.ValidateApproval())
		assert.Equal(t, model.FoundryAgentActionPrepared, a.Status, "a proposal waits, a new dashboard too")
		assert.JSONEq(t, `{}`, string(a.Display), "nothing is drawn as the bot")
		assert.WithinDuration(t, before.Add(24*time.Hour), a.ExpiresAt, time.Minute)
		told := toolResultOf(t, h, i+1)
		assert.False(t, told.IsError, told.Content)
		assert.Contains(t, told.Content, `"status":"awaiting_admin"`)
		assert.Contains(t, told.Content, a.ID)
	}
	assert.Equal(t, `{"expected_revision":2,"name":"Revenue by month"}`, string(h.db.actions[0].Payload))
	assert.Empty(t, h.backend.created, "nothing runs at once for a bot")
	assert.Empty(t, h.host.service.Confirmations.(*confirmations).kept, "nothing is confirmed as the bot")
	assert.NotContains(t, h.db.eventTypes(), "action_card")
	var proposals []string
	for _, e := range h.db.events {
		if strings.Contains(string(e), `"action_proposal"`) {
			proposals = append(proposals, string(e))
		}
	}
	require.Len(t, proposals, 2)
	assert.JSONEq(t, `{"type":"action_proposal","action_id":"`+h.db.actions[0].ID+`","operation":"update_dashboard"}`, proposals[0],
		"the action alone")
}

// A bot whose key its host refuses fails for good, in words the Slack bot
// tells the channel; one whose session couldn't open is retried.
func TestABotsTurnFailsForGoodOnceItsKeyIsRefused(t *testing.T) {
	run := func(err error) *Failure {
		h := newHarness(t)
		h.host.err = err
		_, got := h.runner.Run(context.Background(), botMessage("Hi"))
		var f *Failure
		require.ErrorAs(t, got, &f)
		return f
	}
	for _, code := range []codes.Code{codes.PermissionDenied, codes.NotFound, codes.Unauthenticated} {
		f := run(status.Error(code, "bot key has database permissions"))
		assert.True(t, f.Permanent, code.String())
		assert.Equal(t, BotKeyRefused, f.Message)
	}
	f := run(status.Error(codes.Unavailable, "hydrate failed"))
	assert.False(t, f.Permanent)
	assert.NotEqual(t, BotKeyRefused, f.Message)

	h := newHarness(t)
	turn := message("Hi")
	turn.Principal = nil
	_, err := h.runner.Run(context.Background(), turn)
	var nobody *Failure
	require.ErrorAs(t, err, &nobody)
	assert.True(t, nobody.Permanent, "a turn of no principal never runs")
	assert.Empty(t, h.host.actors)
}

// A bot's thread holds what its key's grants give it: never a team's, which
// no key belongs to.
func TestABotReadsByItsKeysGrantsAlone(t *testing.T) {
	h := newHarness(t, say("Hello."))
	h.runner.ProposalTTL = time.Hour
	stripe, general := access.ConnectorTarget("stripe"), access.DocumentSetTarget("general")
	h.db.grants = []model.FoundryAccessGrant{
		{Principal: access.Principal{Kind: access.PrincipalServiceCredential, ID: botKey}, Target: stripe},
		{Principal: access.Principal{Kind: access.PrincipalTeam, ID: "sales"}, Target: general},
		{Principal: access.Principal{Kind: access.PrincipalUser, ID: botKey}, Target: general},
	}
	h.db.teams = map[string][]string{botKey: {"sales"}}
	_, err := h.runner.Run(context.Background(), botMessage("Hi"))
	require.NoError(t, err)
	thread, err := h.db.GetFoundryAgentThread(context.Background(), ws, "thread1")
	require.NoError(t, err)
	require.NotNil(t, thread.Granted)
	assert.Equal(t, []access.Target{stripe}, thread.Granted.Targets)

	// A thread with the whole view isn't the bot's to read on.
	h = newHarness(t, say("Hello."))
	h.db.thread("thread1").Granted = nil
	_, err = h.runner.Run(context.Background(), botMessage("Hi"))
	var f *Failure
	require.ErrorAs(t, err, &f)
}

// A bot's grants are read again around each tool call, as a restricted
// member's: a grant taken away during one fails the turn for good.
func TestABotsGrantsAreReadAroundEachCall(t *testing.T) {
	h := newHarness(t, call("toolu_1", "helix_foundry_list_ontologies", `{"workspace_id":"ws1"}`), say("Sales graph."))
	stripe := access.ConnectorTarget("stripe")
	key := access.Principal{Kind: access.PrincipalServiceCredential, ID: botKey}
	h.db.grants = []model.FoundryAccessGrant{{Principal: key, Target: stripe}}
	h.backend.onCall = func() { h.db.grants = nil }
	_, err := h.runner.Run(context.Background(), botMessage("What do you read?"))
	var f *Failure
	require.ErrorAs(t, err, &f)
	assert.True(t, f.Permanent)
}

func TestTurnReach(t *testing.T) {
	h := newHarness(t)
	session, err := h.host.Session(context.Background(), ports.Actor{}, integration.ConfirmByCaller{})
	require.NoError(t, err)
	reach, err := turnReach(session, botMessage("Hi"))
	require.NoError(t, err)
	assert.Equal(t, ReachBot, reach)
	reach, err = turnReach(session, message("Hi"))
	require.NoError(t, err)
	assert.Equal(t, ReachWorkspace, reach)
	h.backend.grantee = true
	reach, err = turnReach(session, message("Hi"))
	require.NoError(t, err)
	assert.Equal(t, ReachGrants, reach)
	nobody := message("Hi")
	nobody.Principal = nil
	reach, err = turnReach(session, nobody)
	var f *Failure
	require.ErrorAs(t, err, &f)
	assert.True(t, f.Permanent)
	assert.Equal(t, ReachGrants, reach, "the narrowest")
}

// Each reach offers its tools: a bot's are a restricted member's and
// prepare, which proposes; it proposes Founder's operations but a local
// install's own.
func TestEachReachOffersItsTools(t *testing.T) {
	assert.Equal(t, Tools, ReachWorkspace.tools())
	assert.Equal(t, GranteeTools, ReachGrants.tools())
	assert.Equal(t, BotTools, ReachBot.tools())
	want := maps.Clone(GranteeTools)
	want[toolPrepare] = true
	assert.Equal(t, want, BotTools)
	for name := range BotTools {
		assert.True(t, Tools[name], name)
	}
	assert.False(t, GranteeTools[toolPrepare], "BotTools is its own map")
	for op := range Operations {
		local := op == "set_assistant_runtime" || op == "revoke_assistant_helper"
		assert.Equal(t, !local, BotOperations[op], op)
	}
	assert.Len(t, BotOperations, len(Operations)-2)
}

// A sink allows by its turn's reach, and only a bot's proposes: in its own
// workspace, once per payload while it waits, within the thread's cards.
func TestABotsSinkOnlyProposes(t *testing.T) {
	proposal := integration.ProposedAction{Operation: "create_load", Target: "ontology:ws1/ont1", Payload: []byte(`{"version_id":"ver3"}`),
		PayloadHash: strings.Repeat("b", 64)}
	sinkOf := func(h *harness, reach Reach) *actionSink {
		return &actionSink{db: h.db, keys: h.keys, turn: botMessage("Load it"), workerID: "w1", maxPending: 3, reach: reach, proposalTTL: time.Hour}
	}
	h := newHarness(t)
	for reach, allowed := range map[Reach]map[string]bool{ReachWorkspace: Operations, ReachBot: BotOperations, ReachGrants: {}} {
		sink := sinkOf(h, reach)
		for op := range Operations {
			assert.Equal(t, allowed[op], sink.Allows(op), "%v %s", reach, op)
		}
	}

	for _, reach := range []Reach{ReachGrants, ReachWorkspace} {
		_, err := sinkOf(h, reach).Propose(context.Background(), proposal)
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
	}
	keepsNone := sinkOf(h, ReachBot)
	keepsNone.proposalTTL = 0
	_, err := keepsNone.Propose(context.Background(), proposal)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	for _, target := range []string{"ontology:ws2/ont1", "workspace:ws2", "tenant:t1"} {
		elsewhere := proposal
		elsewhere.Target = target
		_, err = sinkOf(h, ReachBot).Propose(context.Background(), elsewhere)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), target)
	}
	assert.Empty(t, h.db.actions)

	sink := sinkOf(h, ReachBot)
	first, err := sink.Propose(context.Background(), proposal)
	require.NoError(t, err)
	again, err := sink.Propose(context.Background(), proposal)
	require.NoError(t, err)
	assert.Equal(t, first, again, "a replayed attempt finds the proposal")
	require.Len(t, h.db.actions, 1)
	h.db.actions[0].Status = model.FoundryAgentActionDismissed
	_, err = sink.Propose(context.Background(), proposal)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "already dismissed")

	// The store's refusals, a lost lease and its failures reach the model as
	// the person's turn's do.
	for _, c := range []struct {
		err  error
		code codes.Code
	}{
		{model.FoundryAgentRefusal{Reason: model.FoundryAgentTooManyCards, Detail: "3 actions already wait"}, codes.FailedPrecondition},
		{foundrystore.ErrNotFound, codes.Unknown},
		{errors.New("the store went away"), codes.Unknown},
	} {
		h := newHarness(t)
		failing := &actionSink{db: failingRecord{memStore: h.db, err: c.err}, turn: botMessage("Load it"), reach: ReachBot, proposalTTL: time.Hour}
		_, err := failing.Propose(context.Background(), proposal)
		require.Error(t, err)
		assert.Equal(t, c.code, status.Code(err), c.err.Error())
		if errors.Is(c.err, foundrystore.ErrNotFound) {
			assert.ErrorIs(t, err, ErrLost)
		}
	}
}

// failingRecord refuses every action recorded with err.
type failingRecord struct {
	*memStore
	err error
}

func (f failingRecord) RecordFoundryAgentAction(context.Context, string, *model.FoundryAgentAction, int,
	func(*model.FoundryAgentAction) (json.RawMessage, error)) (*model.FoundryAgentAction, error) {
	return nil, f.err
}

// A bot asks its own questions, as anyone in the thread may answer them, but
// relays no draft's: an owner or admin answers those in Foundry.
func TestABotRelaysNoDraftsQuestions(t *testing.T) {
	h := newHarness(t,
		call("toolu_1", toolAskPerson, `{"generation":{"ontology_id":"ont1","generation_id":"gen1"}}`),
		say("An owner or admin answers the draft's questions."))
	turn := botMessage("What does the draft need?")
	h.db.turns = []*model.FoundryAgentTurn{turn}
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	result := toolResultOf(t, h, 1)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content, "can't answer a draft's questions")
	assert.Empty(t, h.backend.answered)
}

// The turn a bot goes on from a run in says an owner or admin ran its
// proposal, and only whether it did.
func TestABotsContinuationSaysAnOwnerOrAdminRanIt(t *testing.T) {
	h := newHarness(t, say("It ran."))
	failed := model.FoundryAgentProposalFailed
	h.db.actions = []*model.FoundryAgentAction{{ID: "act1", Operation: "create_load", Principal: model.AgentBot{CredentialID: botKey},
		Status: model.FoundryAgentActionFailed, Result: json.RawMessage(`{"operation":"create_load","status":"failed","decided_by":"user-grace"}`), Error: &failed}}
	turn := botMessage("act1")
	turn.Kind, turn.Input = model.FoundryAgentTurnActionResult, "act1"
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	text := h.model.requests[0].Messages[0].Content[0].(llm.Text).Text
	assert.Contains(t, text, "[Foundry] An owner or admin ran the create_load action you proposed (act1): it failed.")
	assert.Contains(t, text, failed)
	assert.NotContains(t, text, "The person ran")
}

// proposalHarness is a harness whose store holds a bot's proposal of
// operation on target with payload, prepared for an hour, and whose
// sessions have Founder's API.
func proposalHarness(t *testing.T, operation, target, payload string) (*harness, Proposals) {
	t.Helper()
	h := newHarness(t)
	h.host.founder = h.backend
	h.db.actions = []*model.FoundryAgentAction{{ID: "act1", TurnID: "turn1", ThreadID: "thread1", WorkspaceID: ws, Principal: model.AgentBot{CredentialID: botKey},
		Operation: operation, Target: target, Payload: json.RawMessage(payload), PayloadHash: strings.Repeat("a", 64), Display: json.RawMessage(`{}`),
		Approval: model.AdminApproval{}, Status: model.FoundryAgentActionPrepared, ExpiresAt: time.Now().Add(time.Hour)}}
	return h, Proposals{DB: h.db, Sessions: h.host, Databases: databases{}, MaxMicros: 500_000}
}

// Review draws the card and checks the action as the approver, changing
// nothing; anything not a waiting proposal of the bot's is refused.
func TestAProposalIsReviewedAsTheApprover(t *testing.T) {
	h, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{"version_id":"ver3"}`)
	reviewed, err := p.Review(context.Background(), ws, "act1", approver)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ontology":{"id":"ont1","name":"Sales graph"},"version":{"id":"ver3","number":3}}`, string(reviewed.Display))
	assert.Empty(t, reviewed.SpecHash)
	assert.Equal(t, "act1", reviewed.Action.ID)
	assert.Equal(t, []ports.Actor{{Source: ports.SourceUser, UserID: approver, WorkspaceID: ws}}, h.host.actors, "as the approver")
	assert.Equal(t, []integration.ConfirmMode{integration.ConfirmByCaller{}}, h.host.confirms)
	assert.Zero(t, h.backend.loads)
	assert.Equal(t, model.FoundryAgentActionPrepared, h.db.actions[0].Status)

	refusal := func(err error) model.FoundryAgentRefusalReason {
		var r model.FoundryAgentRefusal
		require.ErrorAs(t, err, &r)
		return r.Reason
	}
	t.Run("not a bot's", func(t *testing.T) {
		h, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{"version_id":"ver3"}`)
		h.db.actions[0].Principal = model.AgentPerson{UserID: user}
		_, err := p.Review(context.Background(), ws, "act1", approver)
		assert.ErrorIs(t, err, foundrystore.ErrNotFound)
		_, err = p.Review(context.Background(), ws, "act9", approver)
		assert.ErrorIs(t, err, foundrystore.ErrNotFound)
	})
	t.Run("expired", func(t *testing.T) {
		h, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{"version_id":"ver3"}`)
		h.db.actions[0].ExpiresAt = time.Now().Add(-time.Minute)
		_, err := p.Review(context.Background(), ws, "act1", approver)
		assert.Equal(t, model.FoundryAgentExpired, refusal(err))
	})
	t.Run("decided", func(t *testing.T) {
		h, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{"version_id":"ver3"}`)
		h.db.actions[0].Status = model.FoundryAgentActionDismissed
		_, err := p.Review(context.Background(), ws, "act1", approver)
		assert.Equal(t, model.FoundryAgentNotPrepared, refusal(err))
		assert.Contains(t, err.Error(), "dismissed")
	})
	t.Run("an unreadable payload", func(t *testing.T) {
		_, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `[`)
		_, err := p.Review(context.Background(), ws, "act1", approver)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})
	t.Run("a session that won't open", func(t *testing.T) {
		h, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{"version_id":"ver3"}`)
		h.host.err = status.Error(codes.PermissionDenied, "not a member")
		_, err := p.Review(context.Background(), ws, "act1", approver)
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
	})
	t.Run("what the approver may not do", func(t *testing.T) {
		h, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{"version_id":"ver3"}`)
		h.host.may = func(integration.Permission) bool { return false }
		_, err := p.Review(context.Background(), ws, "act1", approver)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})
	t.Run("a payload the check refuses", func(t *testing.T) {
		_, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{}`)
		_, err := p.Review(context.Background(), ws, "act1", approver)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	})
	t.Run("a card the workspace can't draw", func(t *testing.T) {
		_, p := proposalHarness(t, "create_load", "ontology:ws1/ont9", `{"version_id":"ver3"}`)
		_, err := p.Review(context.Background(), ws, "act1", approver)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Contains(t, err.Error(), "holds no ontology ont9")
	})
}

// Run runs a reviewed proposal once as its approver: the bot's thread
// learns only whether it ran, the approver everything, and the bot goes on.
func TestAProposalRunsOnceAsTheApprover(t *testing.T) {
	h, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{"version_id":"ver3"}`)
	ran, err := p.Run(context.Background(), ws, "act1", strings.Repeat("a", 64), "", approver)
	require.NoError(t, err)
	assert.Equal(t, 1, h.backend.loads)
	assert.Equal(t, model.FoundryAgentActionSucceeded, ran.End.Status)
	assert.Contains(t, string(ran.End.Result), `"load1"`, "the approver reads the result")
	assert.JSONEq(t, `{"operation":"create_load","status":"succeeded","decided_by":"user-grace"}`, string(ran.Action.Result), "the thread reads that it ran")
	assert.Equal(t, approver, *ran.Action.DecidedBy)
	require.NotNil(t, ran.Continuation)
	assert.Equal(t, model.AgentBot{CredentialID: botKey}, ran.Continuation.Principal)
	assert.Equal(t, model.FoundryAgentTurnActionResult, ran.Continuation.Kind)
	assert.Equal(t, "act1", ran.Continuation.Input)
	assert.Equal(t, "thread1", ran.Continuation.ThreadID)
	assert.EqualValues(t, 500_000, ran.Continuation.MaxUSDMicros)
	assert.JSONEq(t, `{"type":"action_result","action_id":"act1","operation":"create_load","status":"succeeded",`+
		`"result":{"operation":"create_load","status":"succeeded","decided_by":"user-grace"}}`, string(h.db.first), "its first event, as the action keeps it")

	_, err = p.Run(context.Background(), ws, "act1", strings.Repeat("a", 64), "", approver)
	var r model.FoundryAgentRefusal
	require.ErrorAs(t, err, &r)
	assert.Equal(t, model.FoundryAgentNotPrepared, r.Reason, "it runs once")
	assert.Equal(t, 1, h.backend.loads)

	t.Run("another payload than the one reviewed", func(t *testing.T) {
		h, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{"version_id":"ver3"}`)
		_, err := p.Run(context.Background(), ws, "act1", strings.Repeat("c", 64), "", approver)
		require.ErrorAs(t, err, &r)
		assert.Equal(t, model.FoundryAgentChanged, r.Reason)
		assert.Zero(t, h.backend.loads)
	})
	t.Run("a review refused", func(t *testing.T) {
		h, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{}`)
		_, err := p.Run(context.Background(), ws, "act1", strings.Repeat("a", 64), "", approver)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Equal(t, model.FoundryAgentActionPrepared, h.db.actions[0].Status)
	})
	t.Run("its thread went on", func(t *testing.T) {
		h, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{"version_id":"ver3"}`)
		h.db.live = true
		ran, err := p.Run(context.Background(), ws, "act1", strings.Repeat("a", 64), "", approver)
		require.NoError(t, err)
		assert.Nil(t, ran.Continuation)
		assert.Equal(t, model.FoundryAgentActionSucceeded, ran.Action.Status)
	})
	for _, c := range []struct {
		name string
		err  error
		end  model.FoundryAgentActionStatus
		told string
		why  string
	}{
		{"failed", status.Error(codes.FailedPrecondition, "the version can't be loaded"), model.FoundryAgentActionFailed, model.FoundryAgentProposalFailed,
			"the version can't be loaded"},
		{"its answer lost", status.Error(codes.Unavailable, "the backend went away"), model.FoundryAgentActionUnknown, model.FoundryAgentProposalUnknown,
			"it may or may not have run"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{"version_id":"ver3"}`)
			h.backend.loadErr = c.err
			ran, err := p.Run(context.Background(), ws, "act1", strings.Repeat("a", 64), "", approver)
			require.NoError(t, err)
			assert.Equal(t, c.end, ran.End.Status)
			assert.Contains(t, ran.End.Error, c.why, "the approver reads why")
			assert.Equal(t, c.told, *ran.Action.Error, "the thread, a fixed sentence")
			event, err := DecodeEvent(h.db.first)
			require.NoError(t, err)
			assert.Equal(t, c.told, event.(ActionResult).Error)
			assert.NotContains(t, string(h.db.first), c.why)
		})
	}
}

// A workflow a proposal makes is approved as its approver, at the revision
// their run wrote, for the steps they reviewed; nothing else is.
func TestAProposalsWorkflowIsApprovedForTheStepsReviewed(t *testing.T) {
	payload := `{"name":"Nightly load","spec":` + string(stepSpec("")) + `}`
	h, p := proposalHarness(t, "create_workflow", "workspace:ws1", payload)
	reviewed, err := p.Review(context.Background(), ws, "act1", approver)
	require.NoError(t, err)
	require.Len(t, reviewed.SpecHash, 64)
	assert.JSONEq(t, `{"name":"Nightly load"}`, string(reviewed.Display))

	_, err = p.Run(context.Background(), ws, "act1", strings.Repeat("a", 64), strings.Repeat("0", 64), approver)
	var r model.FoundryAgentRefusal
	require.ErrorAs(t, err, &r)
	assert.Equal(t, model.FoundryAgentChanged, r.Reason, "steps other than the ones reviewed")
	assert.Contains(t, r.Detail, "Review again")
	assert.Equal(t, model.FoundryAgentActionPrepared, h.db.actions[0].Status, "nothing claimed")

	ran, err := p.Run(context.Background(), ws, "act1", strings.Repeat("a", 64), reviewed.SpecHash, approver)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionSucceeded, ran.End.Status)
	assert.Empty(t, ran.End.Error)
	require.Len(t, h.backend.approved, 1)
	assert.Equal(t, &foundryv1.ApproveFoundryAutomationRequest{WorkspaceId: ws, Id: "wf1", ExpectedRevision: 1}, h.backend.approved[0])

	for _, c := range []struct {
		name  string
		setup func(h *harness)
		why   string
	}{
		{"an approval refused", func(h *harness) { h.backend.approveErr = status.Error(codes.FailedPrecondition, "its steps changed") },
			"it was saved, but not approved (its steps changed)"},
		{"no Founder API", func(h *harness) { h.host.founder = nil }, "not approved here"},
		{"no revision", func(h *harness) { h.backend.unrevised = true }, "its approval can't be read"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, p := proposalHarness(t, "create_workflow", "workspace:ws1", payload)
			c.setup(h)
			ran, err := p.Run(context.Background(), ws, "act1", strings.Repeat("a", 64), reviewed.SpecHash, approver)
			require.NoError(t, err)
			assert.Equal(t, model.FoundryAgentActionSucceeded, ran.End.Status, "saved")
			assert.Contains(t, ran.End.Error, c.why)
			assert.Empty(t, h.backend.approved)
		})
	}
}

// failingFinish fails recording an end with finishErr, or reading the action
// back once ended with readErr.
type failingFinish struct {
	*memStore
	finishErr, readErr error
}

func (f failingFinish) FinishFoundryAgentAction(ctx context.Context, id string, end model.FoundryAgentActionEnd, continuation *model.FoundryAgentTurn,
	first json.RawMessage) error {
	if f.finishErr != nil {
		return f.finishErr
	}
	return f.memStore.FinishFoundryAgentAction(ctx, id, end, continuation, first)
}

func (f failingFinish) GetFoundryAgentAction(ctx context.Context, workspaceID, id string) (*model.FoundryAgentAction, error) {
	a, err := f.memStore.GetFoundryAgentAction(ctx, workspaceID, id)
	if err == nil && a.Status != model.FoundryAgentActionPrepared && f.readErr != nil {
		return nil, f.readErr
	}
	return a, err
}

// A run whose end can't be recorded or read back says so, after it ran.
func TestAProposalRunsEndThatCantBeRecordedIsAnError(t *testing.T) {
	gone := errors.New("the store went away")
	for _, failing := range []failingFinish{{finishErr: gone}, {readErr: gone}} {
		h, p := proposalHarness(t, "create_load", "ontology:ws1/ont1", `{"version_id":"ver3"}`)
		failing.memStore = h.db
		p.DB = failing
		_, err := p.Run(context.Background(), ws, "act1", strings.Repeat("a", 64), "", approver)
		assert.EqualError(t, err, "the store went away")
		assert.Equal(t, 1, h.backend.loads, "it ran once")
	}
}

func TestAnActionThatEndedNoKnownWayFails(t *testing.T) {
	end := actionEnd(nil)
	assert.Equal(t, model.FoundryAgentActionFailed, end.Status)
	assert.Equal(t, "it ended in a way Foundry doesn't know", end.Error)
}
