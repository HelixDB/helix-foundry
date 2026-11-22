package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The bot's principal: a Slack install's bot in bot mode
// (migrations/20261215000000_foundry_agent_principals.sql).

func (f agentFixture) bot() model.AgentBot {
	return model.AgentBot{CredentialID: id26("CRED-" + f.tag)}
}

// sendAs queues message n as principal in thread (a new thread numbered n
// when 0), under limits.
func (f agentFixture) sendAs(principal model.FoundryAgentPrincipal, n, thread int, limits model.FoundryAgentLimits) error {
	token := fmt.Sprintf("send-%d", n)
	t := &model.FoundryAgentTurn{ID: f.turnID(n), WorkspaceID: f.workspaceID, Principal: principal, Kind: model.FoundryAgentTurnMessage,
		Surface: model.FoundryAgentSurfaceSlack, Input: fmt.Sprintf("message %d", n), ClientToken: &token, MaxUSDMicros: 500_000}
	var newThread *model.FoundryAgentThread
	if thread == 0 {
		t.ThreadID = f.threadID(n)
		newThread = &model.FoundryAgentThread{ID: t.ThreadID, WorkspaceID: f.workspaceID, Principal: principal, Title: "Ask", Runtime: model.FoundryAgentRuntimeAPI}
	} else {
		t.ThreadID = f.threadID(thread)
	}
	return f.db.CreateFoundryAgentTurn(context.Background(), t, newThread, limits, nil)
}

// propose records the bot's proposal n of turn, as its worker does: no
// confirmation, and no display but its operation.
func (f agentFixture) propose(turn *model.FoundryAgentTurn, n int, expires time.Time) (*model.FoundryAgentAction, error) {
	a := &model.FoundryAgentAction{
		ID: id26(fmt.Sprintf("AP-%s-%03d-", f.tag, n)), TurnID: turn.ID, ThreadID: turn.ThreadID, WorkspaceID: f.workspaceID, Principal: turn.Principal,
		Operation: "create_load", Target: "ontology:ws/o", Payload: json.RawMessage(`{"version_id":"v1"}`),
		PayloadHash: strings.Repeat(fmt.Sprintf("%x", n%16), 64), Display: json.RawMessage(`{}`), Approval: model.AdminApproval{},
		Status: model.FoundryAgentActionPrepared, ExpiresAt: expires,
	}
	return f.db.RecordFoundryAgentAction(context.Background(), "w1", a, 10, func(a *model.FoundryAgentAction) (json.RawMessage, error) {
		return json.Marshal(map[string]string{"type": "action_card", "action_id": a.ID, "operation": a.Operation})
	})
}

// A bot's threads and turns are its own: kept as its principal, apart from
// every person's, with a per-hour count of their own.
func TestFoundryAgentBotTurns(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "botturns")
	bot := f.bot()
	limits := f.limits()
	limits.PerHour = 1
	require.NoError(t, f.sendAs(bot, 1, 0, limits))
	turn, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, f.turnID(1))
	require.NoError(t, err)
	assert.Equal(t, bot, turn.Principal)
	thread, err := f.db.GetFoundryAgentThread(ctx, f.workspaceID, f.threadID(1))
	require.NoError(t, err)
	assert.Equal(t, bot, thread.Principal)

	// The per-hour count is the principal's own.
	assert.Equal(t, model.FoundryAgentPerHour, refusal(t, f.sendAs(bot, 2, 0, limits)))
	require.NoError(t, f.sendAs(agentPerson, 3, 0, limits), "Ada's hour is hers")
	assert.Equal(t, model.FoundryAgentPerHour, refusal(t, f.sendAs(agentPerson, 4, 0, limits)))

	// A person never reaches the bot's thread, nor the bot a person's.
	assert.ErrorIs(t, f.sendAs(agentPerson, 5, 1, f.limits()), store.ErrNotFound)
	assert.ErrorIs(t, f.sendAs(bot, 5, 3, f.limits()), store.ErrNotFound)
	threads, _, err := f.db.ListFoundryAgentThreads(ctx, f.workspaceID, bot, nil, "", 10)
	require.NoError(t, err)
	require.Len(t, threads, 1)
	assert.Equal(t, f.threadID(1), threads[0].ID)
	threads, _, err = f.db.ListFoundryAgentThreads(ctx, f.workspaceID, agentPerson, nil, "", 10)
	require.NoError(t, err)
	require.Len(t, threads, 1)
	assert.Equal(t, f.threadID(3), threads[0].ID)
	// A credential id that is someone's user id still names the bot alone.
	twin := model.AgentPerson{UserID: bot.CredentialID}
	threads, _, err = f.db.ListFoundryAgentThreads(ctx, f.workspaceID, twin, nil, "", 10)
	require.NoError(t, err)
	assert.Empty(t, threads)

	sent, err := f.db.GetFoundryAgentTurnByClientToken(ctx, f.workspaceID, bot, "send-1")
	require.NoError(t, err)
	assert.Equal(t, f.turnID(1), sent.ID)
	_, err = f.db.GetFoundryAgentTurnByClientToken(ctx, f.workspaceID, twin, "send-1")
	assert.ErrorIs(t, err, store.ErrNotFound)

	_, err = f.db.CancelFoundryAgentTurn(ctx, f.workspaceID, agentPerson, f.turnID(1))
	assert.ErrorIs(t, err, store.ErrNotFound)
	cancelled, err := f.db.CancelFoundryAgentTurn(ctx, f.workspaceID, bot, f.turnID(1))
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentTurnCancelled, cancelled.Status)
	assert.ErrorIs(t, f.db.DeleteFoundryAgentThread(ctx, f.workspaceID, agentPerson, f.threadID(1)), store.ErrNotFound)
	require.NoError(t, f.db.DeleteFoundryAgentThread(ctx, f.workspaceID, bot, f.threadID(1)))

	// No principal, an empty one, or a turn not its new thread's, is refused.
	for name, p := range map[string]model.FoundryAgentPrincipal{"none": nil, "no id": model.AgentBot{}, "no user": model.AgentPerson{}} {
		assert.Error(t, f.sendAs(p, 6, 0, f.limits()), name)
		_, _, err := f.db.ListFoundryAgentThreads(ctx, f.workspaceID, p, nil, "", 10)
		assert.Error(t, err, name)
		_, err = f.db.GetFoundryAgentTurnByClientToken(ctx, f.workspaceID, p, "send-1")
		assert.Error(t, err, name)
		_, err = f.db.CancelFoundryAgentTurn(ctx, f.workspaceID, p, f.turnID(1))
		assert.Error(t, err, name)
		assert.Error(t, f.db.DeleteFoundryAgentThread(ctx, f.workspaceID, p, f.threadID(1)), name)
	}
	mixed := &model.FoundryAgentTurn{ID: f.turnID(7), WorkspaceID: f.workspaceID, ThreadID: f.threadID(7), Principal: bot,
		Kind: model.FoundryAgentTurnMessage, Input: "hi", MaxUSDMicros: 500_000}
	assert.Error(t, f.db.CreateFoundryAgentTurn(ctx, mixed, &model.FoundryAgentThread{ID: f.threadID(7), WorkspaceID: f.workspaceID,
		Principal: agentPerson, Title: "x", Runtime: model.FoundryAgentRuntimeAPI}, f.limits(), nil), "a bot's turn in Ada's new thread")

	// A row with a principal kind no binary knows fails its read.
	dbExec(t, `UPDATE foundry_agent_threads SET principal_kind='team' WHERE id=?`, f.threadID(3))
	dbExec(t, `UPDATE foundry_agent_turns SET principal_kind='team' WHERE id=?`, f.turnID(3))
	_, err = f.db.GetFoundryAgentThread(ctx, f.workspaceID, f.threadID(3))
	assert.ErrorContains(t, err, "unknown foundry agent principal kind")
	_, err = f.db.GetFoundryAgentTurn(ctx, f.workspaceID, f.turnID(3))
	assert.ErrorContains(t, err, "unknown foundry agent principal kind")
}

// A bot's actions are proposals: no confirmation, run or dismissed by an
// owner or admin once, and its thread learns only whether they ran.
func TestFoundryAgentProposals(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "proposals")
	bot := f.bot()
	require.NoError(t, f.sendAs(bot, 1, 0, f.limits()))
	turn := f.claimed(t)
	hour := time.Now().Add(time.Hour)
	proposals := map[int]*model.FoundryAgentAction{}
	for n := 1; n <= 6; n++ {
		a, err := f.propose(turn, n, hour)
		require.NoError(t, err)
		proposals[n] = a
	}
	p1 := proposals[1]
	assert.Equal(t, bot, p1.Principal)
	assert.Equal(t, model.AdminApproval{}, p1.Approval)
	assert.True(t, dbTrue(t, `SELECT principal_kind='service_credential' AND confirmation_id IS NULL AND confirmation_token_sealed IS NULL
  AND confirmation_key_id IS NULL FROM foundry_agent_actions WHERE id=?`, p1.ID))
	again, err := f.propose(turn, 1, hour)
	require.NoError(t, err)
	assert.Equal(t, p1.ID, again.ID, "a replayed attempt finds its proposal")

	// The approval follows from the principal, and the principal is the turn's.
	record := func(change func(*model.FoundryAgentAction)) error {
		a := &model.FoundryAgentAction{ID: id26("AP-bad"), TurnID: turn.ID, ThreadID: turn.ThreadID, WorkspaceID: f.workspaceID, Principal: bot,
			Operation: "create_load", Target: "t", Payload: json.RawMessage(`{}`), PayloadHash: strings.Repeat("e", 64), Display: json.RawMessage(`{}`),
			Approval: model.AdminApproval{}, Status: model.FoundryAgentActionPrepared, ExpiresAt: hour}
		change(a)
		_, err := f.db.RecordFoundryAgentAction(ctx, "w1", a, 10, func(*model.FoundryAgentAction) (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
		return err
	}
	assert.Error(t, record(func(a *model.FoundryAgentAction) {
		a.Approval = model.PersonApproval{ConfirmationID: "c", TokenSealed: []byte("s"), KeyID: "k"}
	}), "a bot's action with a confirmation")
	assert.Error(t, record(func(a *model.FoundryAgentAction) {
		a.Status, a.DecidedBy = model.FoundryAgentActionExecuting, new("user-grace")
	}), "a bot's action approved by no one yet")
	assert.Error(t, record(func(a *model.FoundryAgentAction) {
		a.Principal, a.Approval = agentPerson, model.PersonApproval{ConfirmationID: "c", TokenSealed: []byte("s"), KeyID: "k"}
	}), "Ada's action in the bot's turn")
	assert.Error(t, record(func(a *model.FoundryAgentAction) { a.Principal = nil }), "no principal")

	// A person's routes never reach a proposal.
	_, err = f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, bot.CredentialID, p1.ID, p1.PayloadHash)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.db.DismissFoundryAgentAction(ctx, f.workspaceID, bot.CredentialID, p1.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.db.RefreshFoundryAgentAction(ctx, f.workspaceID, bot.CredentialID, p1.ID, p1.PayloadHash, json.RawMessage(`{}`), "c", []byte("s"), "k", hour)
	assert.ErrorIs(t, err, store.ErrNotFound)

	_, err = f.db.ClaimFoundryAgentProposal(ctx, f.workspaceID, p1.ID, p1.PayloadHash, "")
	assert.Error(t, err, "run by no one")
	_, err = f.db.DismissFoundryAgentProposal(ctx, f.workspaceID, p1.ID, "")
	assert.Error(t, err, "dismissed by no one")
	_, err = f.db.ClaimFoundryAgentProposal(ctx, id26("AWS-other"), p1.ID, p1.PayloadHash, "user-grace")
	assert.ErrorIs(t, err, store.ErrNotFound, "another workspace's")
	_, err = f.db.ClaimFoundryAgentProposal(ctx, f.workspaceID, p1.ID, strings.Repeat("0", 64), "user-grace")
	assert.Equal(t, model.FoundryAgentChanged, refusal(t, err))
	_, err = f.db.ClaimFoundryAgentProposal(ctx, f.workspaceID, p1.ID, p1.PayloadHash, "user-grace")
	assert.Equal(t, model.FoundryAgentThreadBusy, refusal(t, err), "the bot is still answering")
	f.settle(t, turn)

	// Expired.
	p2 := proposals[2]
	dbExec(t, `UPDATE foundry_agent_actions SET expires_at=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, p2.ID)
	_, err = f.db.ClaimFoundryAgentProposal(ctx, f.workspaceID, p2.ID, p2.PayloadHash, "user-grace")
	assert.Equal(t, model.FoundryAgentExpired, refusal(t, err))
	expired, err := f.db.GetFoundryAgentAction(ctx, f.workspaceID, p2.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionExpired, expired.Status)

	// Dismissed, once, by whom.
	p3 := proposals[3]
	dismissed, err := f.db.DismissFoundryAgentProposal(ctx, f.workspaceID, p3.ID, "user-grace")
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionDismissed, dismissed.Status)
	assert.Equal(t, new("user-grace"), dismissed.DecidedBy)
	again, err = f.db.DismissFoundryAgentProposal(ctx, f.workspaceID, p3.ID, "user-alan")
	require.NoError(t, err)
	assert.Equal(t, new("user-grace"), again.DecidedBy, "dismissing twice returns it")
	_, err = f.db.ClaimFoundryAgentProposal(ctx, f.workspaceID, p3.ID, p3.PayloadHash, "user-grace")
	assert.Equal(t, model.FoundryAgentNotPrepared, refusal(t, err))
	_, err = f.db.DismissFoundryAgentProposal(ctx, f.workspaceID, turn.ID, "user-grace")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Run: the bot's thread keeps only whether it ran.
	run := func(n int, end model.FoundryAgentActionEnd, continuation *model.FoundryAgentTurn) *model.FoundryAgentAction {
		t.Helper()
		p := proposals[n]
		claimed, err := f.db.ClaimFoundryAgentProposal(ctx, f.workspaceID, p.ID, p.PayloadHash, "user-grace")
		require.NoError(t, err)
		assert.Equal(t, model.FoundryAgentActionExecuting, claimed.Status)
		assert.Equal(t, new("user-grace"), claimed.DecidedBy)
		_, err = f.db.DismissFoundryAgentProposal(ctx, f.workspaceID, p.ID, "user-grace")
		assert.Equal(t, model.FoundryAgentNotPrepared, refusal(t, err), "a running proposal isn't dismissed")
		require.NoError(t, f.db.FinishFoundryAgentAction(ctx, p.ID, end, continuation, json.RawMessage(`{"type":"action_result"}`)))
		got, err := f.db.GetFoundryAgentAction(ctx, f.workspaceID, p.ID)
		require.NoError(t, err)
		assert.JSONEq(t, fmt.Sprintf(`{"operation":"create_load","status":%q,"decided_by":"user-grace"}`, end.Status), string(got.Result))
		return got
	}
	ok := run(4, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionSucceeded, Result: json.RawMessage(`{"secret":"x"}`)}, nil)
	assert.Nil(t, ok.Error)
	lost := run(5, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionUnknown, Error: "the database said no: password=x"}, nil)
	assert.Equal(t, new(model.FoundryAgentProposalUnknown), lost.Error)

	// Two approvers racing run it once; the loser reads who ran it.
	p6 := proposals[6]
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, who := range []string{"user-grace", "user-alan"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = f.db.ClaimFoundryAgentProposal(ctx, f.workspaceID, p6.ID, p6.PayloadHash, who)
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		if err == nil {
			won++
		} else {
			assert.Contains(t, []model.FoundryAgentRefusalReason{model.FoundryAgentNotPrepared, model.FoundryAgentThreadBusy}, refusal(t, err))
		}
	}
	assert.Equal(t, 1, won)
	require.NoError(t, f.db.FinishFoundryAgentAction(ctx, p6.ID, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionSucceeded,
		Result: json.RawMessage(`{}`)}, nil, nil))

	// The continuation is the bot's, whatever its caller says.
	next := &model.FoundryAgentTurn{ID: f.turnID(2), WorkspaceID: f.workspaceID, ThreadID: turn.ThreadID, Principal: agentPerson,
		Kind: model.FoundryAgentTurnActionResult, Input: p1.ID, MaxUSDMicros: 500_000}
	failed := run(1, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionFailed, Error: "Grace can't load ontology o"}, next)
	assert.Equal(t, new(model.FoundryAgentProposalFailed), failed.Error)
	assert.Equal(t, bot, next.Principal, "the caller learns the principal copied")
	assert.Equal(t, model.FoundryAgentSurfaceSlack, next.Surface)
	cont, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, next.ID)
	require.NoError(t, err)
	assert.Equal(t, bot, cont.Principal)

	// A row whose approval disagrees with its principal fails its read.
	dbExec(t, `UPDATE foundry_agent_actions SET confirmation_id='c' WHERE id=?`, p2.ID)
	_, err = f.db.GetFoundryAgentAction(ctx, f.workspaceID, p2.ID)
	assert.ErrorContains(t, err, "holds no confirmation")
	dbExec(t, `UPDATE foundry_agent_actions SET principal_kind='user' WHERE id=?`, p3.ID)
	_, err = f.db.GetFoundryAgentAction(ctx, f.workspaceID, p3.ID)
	assert.ErrorContains(t, err, "has its confirmation")
	dbExec(t, `UPDATE foundry_agent_actions SET principal_kind='robot' WHERE id=?`, proposals[4].ID)
	_, err = f.db.GetFoundryAgentAction(ctx, f.workspaceID, proposals[4].ID)
	assert.ErrorContains(t, err, "unknown foundry agent principal kind")
}

// A person's refresh brings a whole confirmation.
func TestFoundryAgentRefreshIncomplete(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "refreshbad")
	require.NoError(t, f.send(1, 0))
	turn := f.claimed(t)
	a, err := f.prepare(turn, 1, time.Now().Add(time.Hour))
	require.NoError(t, err)
	for name, c := range map[string][3]string{"no id": {"", "s", "k"}, "no token": {"c", "", "k"}, "no key": {"c", "s", ""}} {
		_, err := f.db.RefreshFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, a.PayloadHash, json.RawMessage(`{}`), c[0], []byte(c[1]), c[2],
			time.Now().Add(time.Hour))
		assert.Error(t, err, name)
	}
}
