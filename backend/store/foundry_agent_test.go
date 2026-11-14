package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// agentFixture is a workspace whose user Ada talks to the Assistant, $0.50 a
// turn under a $1 month.
type agentFixture struct {
	db          store.Store
	workspaceID string
	tag         string
}

const agentUser = "user-ada"

func newAgentFixture(t *testing.T, tag string) agentFixture {
	t.Helper()
	f := agentFixture{db: newTestStore(t), workspaceID: id26("AWS-" + tag), tag: tag}
	createWorkspace(t, f.workspaceID)
	return f
}

func (f agentFixture) limits() model.FoundryAgentLimits {
	now := time.Now().UTC()
	return model.FoundryAgentLimits{Since: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), CapMicros: 1_000_000, PerHour: 30, Running: 5}
}

func (f agentFixture) claim() model.FoundryAgentClaim {
	return model.FoundryAgentClaim{Lease: time.Minute, Defer: time.Hour, Limits: f.limits()}
}

func (f agentFixture) turnID(n int) string   { return id26(fmt.Sprintf("AT-%s-%03d-", f.tag, n)) }
func (f agentFixture) threadID(n int) string { return id26(fmt.Sprintf("AH-%s-%03d-", f.tag, n)) }

// send queues message n in thread (a new thread numbered n when 0).
func (f agentFixture) send(n, thread int) error {
	token := fmt.Sprintf("send-%d", n)
	t := &model.FoundryAgentTurn{
		ID: f.turnID(n), WorkspaceID: f.workspaceID, UserID: agentUser, Kind: model.FoundryAgentTurnMessage,
		Input: fmt.Sprintf("message %d", n), ClientToken: &token, MaxUSDMicros: 500_000,
	}
	var newThread *model.FoundryAgentThread
	if thread == 0 {
		t.ThreadID = f.threadID(n)
		newThread = &model.FoundryAgentThread{ID: t.ThreadID, WorkspaceID: f.workspaceID, UserID: agentUser, Title: "Load it", Runtime: model.FoundryAgentRuntimeAPI}
	} else {
		t.ThreadID = f.threadID(thread)
	}
	return f.db.CreateFoundryAgentTurn(context.Background(), t, newThread, f.limits())
}

func (f agentFixture) claimed(t *testing.T) *model.FoundryAgentTurn {
	t.Helper()
	turn, err := f.db.ClaimFoundryAgentTurn(context.Background(), "w1", f.claim())
	require.NoError(t, err)
	require.NotNil(t, turn)
	return turn
}

func (f agentFixture) settle(t *testing.T, turn *model.FoundryAgentTurn) {
	t.Helper()
	require.NoError(t, f.db.SettleFoundryAgentTurn(context.Background(), turn.ID, "w1",
		model.FoundryAgentTurnOutcome{Status: model.FoundryAgentTurnDone, Spend: model.FoundryGraphJobSpend{USDMicros: 1000}, Steps: 2}))
}

// prepare records an action of turn, as its worker does.
func (f agentFixture) prepare(turn *model.FoundryAgentTurn, n int, expires time.Time) (*model.FoundryAgentAction, error) {
	a := &model.FoundryAgentAction{
		ID: id26(fmt.Sprintf("AA-%s-%03d-", f.tag, n)), TurnID: turn.ID, ThreadID: turn.ThreadID, WorkspaceID: f.workspaceID, UserID: agentUser,
		Operation: "create_load", Target: "ontology:ws/o", Payload: json.RawMessage(`{"version_id":"v1"}`),
		PayloadHash: strings.Repeat(fmt.Sprint(n%10), 64), Display: json.RawMessage(`{}`), ConfirmationID: "conf", TokenSealed: []byte("sealed"),
		KeyID: "k", Status: model.FoundryAgentActionPrepared, ExpiresAt: expires,
	}
	return f.db.RecordFoundryAgentAction(context.Background(), "w1", a, 3, func(a *model.FoundryAgentAction) (json.RawMessage, error) {
		return json.Marshal(map[string]string{"type": "action_card", "action_id": a.ID})
	})
}

func refusal(t *testing.T, err error) model.FoundryAgentRefusalReason {
	t.Helper()
	var r model.FoundryAgentRefusal
	require.ErrorAs(t, err, &r)
	return r.Reason
}

func TestFoundryAgentSendAndClaim(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "send")
	require.NoError(t, f.send(1, 0))
	// One live turn a thread.
	assert.Equal(t, model.FoundryAgentThreadBusy, refusal(t, f.send(2, 1)))
	// Another user's thread isn't found.
	other := &model.FoundryAgentTurn{ID: f.turnID(9), WorkspaceID: f.workspaceID, ThreadID: f.threadID(1), UserID: "user-eve",
		Kind: model.FoundryAgentTurnMessage, Input: "hi", MaxUSDMicros: 500_000}
	assert.ErrorIs(t, f.db.CreateFoundryAgentTurn(ctx, other, nil, f.limits()), store.ErrNotFound)

	turn := f.claimed(t)
	assert.Equal(t, f.turnID(1), turn.ID)
	assert.Equal(t, model.FoundryAgentTurnRunning, turn.Status)
	assert.EqualValues(t, 500_000, turn.ReservedMicros)

	// Events and the transcript are written only under the lease.
	require.NoError(t, f.db.AppendFoundryAgentEvents(ctx, turn.ID, "w1", []json.RawMessage{json.RawMessage(`{"type":"started"}`)}))
	assert.ErrorIs(t, f.db.AppendFoundryAgentEvents(ctx, turn.ID, "w2", []json.RawMessage{json.RawMessage(`{}`)}), store.ErrNotFound)
	for i := range 3 {
		require.NoError(t, f.db.AppendFoundryAgentMessages(ctx, turn.ID, "w1", []model.FoundryAgentMessage{
			{Role: model.FoundryAgentRoleUser, Content: json.RawMessage(fmt.Sprintf(`[{"type":"text","text":"%d"}]`, i)), EstTokens: 1},
		}, 2))
	}
	messages, err := f.db.ListFoundryAgentMessages(ctx, f.threadID(1))
	require.NoError(t, err)
	require.Len(t, messages, 2, "only the newest are kept")
	assert.Equal(t, []int{2, 3}, []int{messages[0].Seq, messages[1].Seq})
	assert.ErrorIs(t, f.db.AppendFoundryAgentMessages(ctx, turn.ID, "w2", messages, 10), store.ErrNotFound)

	f.settle(t, turn)
	done, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, turn.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentTurnDone, done.Status)
	assert.EqualValues(t, 1000, done.Spend.USDMicros)
	assert.Equal(t, 2, done.Steps)
	assert.Equal(t, 1, done.LastSeq)
	assert.Zero(t, done.ReservedMicros)

	// The same client token is the same send.
	assert.ErrorIs(t, f.send(1, 1), store.ErrConflict)
	// The thread is idle again; the next message joins it.
	require.NoError(t, f.send(2, 1))
	turns, err := f.db.ListFoundryAgentTurns(ctx, f.threadID(1), 10)
	require.NoError(t, err)
	require.Len(t, turns, 2)
	assert.Equal(t, f.turnID(1), turns[0].ID, "oldest first")

	threads, next, err := f.db.ListFoundryAgentThreads(ctx, f.workspaceID, agentUser, "", 10)
	require.NoError(t, err)
	assert.Empty(t, next)
	require.Len(t, threads, 1)
	none, _, err := f.db.ListFoundryAgentThreads(ctx, f.workspaceID, "user-eve", "", 10)
	require.NoError(t, err)
	assert.Empty(t, none, "a thread is private to its user")
}

func TestFoundryAgentLimits(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "limits")
	limits := f.limits()
	limits.PerHour = 1
	t1 := &model.FoundryAgentTurn{ID: f.turnID(1), WorkspaceID: f.workspaceID, ThreadID: f.threadID(1), UserID: agentUser,
		Kind: model.FoundryAgentTurnMessage, Input: "one", MaxUSDMicros: 500_000}
	require.NoError(t, f.db.CreateFoundryAgentTurn(ctx, t1, &model.FoundryAgentThread{ID: f.threadID(1), WorkspaceID: f.workspaceID, UserID: agentUser,
		Title: "one", Runtime: model.FoundryAgentRuntimeAPI}, limits))
	t2 := *t1
	t2.ID, t2.ThreadID = f.turnID(2), f.threadID(2)
	err := f.db.CreateFoundryAgentTurn(ctx, &t2, &model.FoundryAgentThread{ID: f.threadID(2), WorkspaceID: f.workspaceID, UserID: agentUser,
		Title: "two", Runtime: model.FoundryAgentRuntimeAPI}, limits)
	assert.Equal(t, model.FoundryAgentPerHour, refusal(t, err))

	// The month: one turn spent its whole cap and more.
	turn := f.claimed(t)
	require.NoError(t, f.db.SettleFoundryAgentTurn(ctx, turn.ID, "w1", model.FoundryAgentTurnOutcome{
		Status: model.FoundryAgentTurnDone, Spend: model.FoundryGraphJobSpend{USDMicros: 900_000}}))
	assert.Equal(t, model.FoundryAgentBudget, refusal(t, f.send(3, 0)))
}

func TestFoundryAgentClaimDefersAndCancels(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "defer")
	require.NoError(t, f.send(1, 0))
	require.NoError(t, f.send(2, 0))
	claim := f.claim()
	claim.Limits.Running = 1
	first, err := f.db.ClaimFoundryAgentTurn(ctx, "w1", claim)
	require.NoError(t, err)
	require.NotNil(t, first)
	second, err := f.db.ClaimFoundryAgentTurn(ctx, "w1", claim)
	require.NoError(t, err)
	assert.Nil(t, second, "the workspace runs one at a time")

	cancelled, err := f.db.CancelFoundryAgentTurn(ctx, f.workspaceID, agentUser, first.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentTurnCancelled, cancelled.Status)
	assert.EqualValues(t, 500_000, cancelled.Spend.USDMicros, "a cancelled run is charged its reservation")
	assert.ErrorIs(t, f.db.RenewFoundryAgentTurnLease(ctx, first.ID, "w1", time.Minute), store.ErrNotFound)
	_, err = f.db.CancelFoundryAgentTurn(ctx, f.workspaceID, "user-eve", f.turnID(2))
	assert.ErrorIs(t, err, store.ErrNotFound)

	// A deleted thread's queued turn is cancelled, not run.
	require.NoError(t, f.db.DeleteFoundryAgentThread(ctx, f.workspaceID, agentUser, f.threadID(2)))
	gone, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, f.turnID(2))
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentTurnCancelled, gone.Status)
	_, err = f.db.GetFoundryAgentThread(ctx, f.workspaceID, f.threadID(2))
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, f.db.DeleteFoundryAgentThread(ctx, f.workspaceID, agentUser, f.threadID(2)), store.ErrNotFound)
}

func TestFoundryAgentActions(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "actions")
	require.NoError(t, f.send(1, 0))
	turn := f.claimed(t)
	a, err := f.prepare(turn, 1, time.Now().Add(5*time.Minute))
	require.NoError(t, err)
	// The same payload again in the turn is the same action, and no second card.
	again, err := f.prepare(turn, 1, time.Now().Add(5*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, a.ID, again.ID)
	events, err := f.db.ListFoundryAgentEvents(ctx, turn.ID, 0, 10)
	require.NoError(t, err)
	assert.Len(t, events, 1)
	expired, err := f.prepare(turn, 2, time.Now().Add(-time.Second))
	require.NoError(t, err)
	for n := 3; n <= 4; n++ {
		_, err = f.prepare(turn, n, time.Now().Add(5*time.Minute))
		require.NoError(t, err)
	}
	_, err = f.prepare(turn, 5, time.Now().Add(5*time.Minute))
	assert.Equal(t, model.FoundryAgentTooManyCards, refusal(t, err), "three live cards at most")

	// A click waits for the turn to end.
	_, err = f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, a.PayloadHash)
	assert.Equal(t, model.FoundryAgentThreadBusy, refusal(t, err))
	f.settle(t, turn)

	_, err = f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, "user-eve", a.ID, a.PayloadHash)
	assert.ErrorIs(t, err, store.ErrNotFound, "only its person may run it")
	_, err = f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, strings.Repeat("f", 64))
	assert.Equal(t, model.FoundryAgentChanged, refusal(t, err))
	_, err = f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, agentUser, expired.ID, expired.PayloadHash)
	assert.Equal(t, model.FoundryAgentExpired, refusal(t, err))
	marked, err := f.db.GetFoundryAgentAction(ctx, f.workspaceID, expired.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionExpired, marked.Status, "the expiry is kept")

	running, err := f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, a.PayloadHash)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionExecuting, running.Status)
	_, err = f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, a.PayloadHash)
	assert.Equal(t, model.FoundryAgentNotPrepared, refusal(t, err), "a double click runs once")
	// A send waits for the click.
	assert.Equal(t, model.FoundryAgentThreadBusy, refusal(t, f.send(2, 1)))

	next := &model.FoundryAgentTurn{ID: f.turnID(3), WorkspaceID: f.workspaceID, ThreadID: turn.ThreadID, UserID: agentUser,
		Kind: model.FoundryAgentTurnActionResult, Input: a.ID, MaxUSDMicros: 500_000}
	require.NoError(t, f.db.FinishFoundryAgentAction(ctx, a.ID, model.FoundryAgentActionEnd{
		Status: model.FoundryAgentActionSucceeded, Result: json.RawMessage(`{"load":"l1"}`)}, next, json.RawMessage(`{"type":"action_result"}`)))
	assert.ErrorIs(t, f.db.FinishFoundryAgentAction(ctx, a.ID, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionFailed, Error: "x"}, nil, nil),
		store.ErrNotFound, "it ends once")
	queued, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, next.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentTurnQueued, queued.Status)
	assert.Equal(t, 1, queued.LastSeq)

	// Refresh and dismiss: the expired card comes back, then is dismissed.
	_, err = f.db.RefreshFoundryAgentAction(ctx, f.workspaceID, agentUser, expired.ID, strings.Repeat("f", 64), "c2", []byte("s2"), "k", time.Now().Add(time.Minute))
	assert.Equal(t, model.FoundryAgentChanged, refusal(t, err))
	refreshed, err := f.db.RefreshFoundryAgentAction(ctx, f.workspaceID, agentUser, expired.ID, expired.PayloadHash, "c2", []byte("s2"), "k",
		time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionPrepared, refreshed.Status)
	assert.Equal(t, "c2", refreshed.ConfirmationID)
	assert.Nil(t, refreshed.DecidedAt)
	dismissed, err := f.db.DismissFoundryAgentAction(ctx, f.workspaceID, agentUser, expired.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionDismissed, dismissed.Status)
	_, err = f.db.DismissFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID)
	assert.Equal(t, model.FoundryAgentNotPrepared, refusal(t, err))
	_, err = f.db.RefreshFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, a.PayloadHash, "c3", []byte("s3"), "k", time.Now())
	assert.Equal(t, model.FoundryAgentNotPrepared, refusal(t, err), "a run action is never prepared again")

	actions, err := f.db.ListFoundryAgentActions(ctx, turn.ThreadID)
	require.NoError(t, err)
	assert.Len(t, actions, 4)
}

func TestFoundryAgentStaleClickReadsUnknown(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "stale")
	require.NoError(t, f.send(1, 0))
	turn := f.claimed(t)
	a, err := f.prepare(turn, 1, time.Now().Add(5*time.Minute))
	require.NoError(t, err)
	f.settle(t, turn)
	_, err = f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, a.PayloadHash)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_agent_actions SET decided_at = NOW(6) - INTERVAL 3 MINUTE WHERE id=?`, a.ID)
	require.NoError(t, err)
	stale, err := f.db.GetFoundryAgentAction(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionUnknown, stale.Shown(time.Now()))
	// A send ends the click that lost its process, and goes ahead.
	require.NoError(t, f.send(2, 1))
	ended, err := f.db.GetFoundryAgentAction(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionUnknown, ended.Status)
}

func TestFoundryAgentClickEndsThoughATurnWentLive(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "raced")
	require.NoError(t, f.send(1, 0))
	turn := f.claimed(t)
	a, err := f.prepare(turn, 1, time.Now().Add(5*time.Minute))
	require.NoError(t, err)
	f.settle(t, turn)
	_, err = f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, a.PayloadHash)
	require.NoError(t, err)
	// Another writer queued a turn in the thread meanwhile.
	_, err = testDB.Exec(`INSERT INTO foundry_agent_turns (id, workspace_id, thread_id, user_id, kind, input, status, max_usd_micros)
VALUES (?, ?, ?, ?, 'message', 'hi', 'queued', 1)`, f.turnID(7), f.workspaceID, turn.ThreadID, agentUser)
	require.NoError(t, err)
	next := &model.FoundryAgentTurn{ID: f.turnID(8), WorkspaceID: f.workspaceID, ThreadID: turn.ThreadID, UserID: agentUser,
		Kind: model.FoundryAgentTurnActionResult, Input: a.ID, MaxUSDMicros: 500_000}
	require.NoError(t, f.db.FinishFoundryAgentAction(ctx, a.ID, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionFailed, Error: "no"},
		next, json.RawMessage(`{"type":"action_result"}`)))
	ended, err := f.db.GetFoundryAgentAction(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionFailed, ended.Status, "the click's end stands")
	_, err = f.db.GetFoundryAgentTurn(ctx, f.workspaceID, next.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}
