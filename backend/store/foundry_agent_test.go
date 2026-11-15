package store_test

import (
	"context"
	"database/sql"
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
	return model.FoundryAgentClaim{Runtime: model.FoundryAgentRuntimeAPI, Lease: time.Minute, Defer: time.Hour, Limits: f.limits()}
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
	found, err := f.db.GetFoundryAgentActionByPayload(ctx, turn.ID, a.PayloadHash)
	require.NoError(t, err)
	assert.Equal(t, a.ID, found.ID, "a turn's payload finds its action")
	_, err = f.db.GetFoundryAgentActionByPayload(ctx, turn.ID, strings.Repeat("e", 64))
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.db.GetFoundryAgentActionByPayload(ctx, f.turnID(9), a.PayloadHash)
	assert.ErrorIs(t, err, store.ErrNotFound, "another turn's payload is its own")
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
	redrawn := json.RawMessage(`{"ontology":{"id":"o1","name":"Renamed"}}`)
	_, err = f.db.RefreshFoundryAgentAction(ctx, f.workspaceID, agentUser, expired.ID, strings.Repeat("f", 64), redrawn, "c2", []byte("s2"), "k",
		time.Now().Add(time.Minute))
	assert.Equal(t, model.FoundryAgentChanged, refusal(t, err))
	_, err = f.db.RefreshFoundryAgentAction(ctx, f.workspaceID, agentUser, expired.ID, expired.PayloadHash, json.RawMessage(`{`), "c2", []byte("s2"), "k",
		time.Now().Add(time.Minute))
	assert.Error(t, err, "a display that isn't JSON is never stored")
	refreshed, err := f.db.RefreshFoundryAgentAction(ctx, f.workspaceID, agentUser, expired.ID, expired.PayloadHash, redrawn, "c2", []byte("s2"), "k",
		time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionPrepared, refreshed.Status)
	assert.Equal(t, "c2", refreshed.ConfirmationID)
	assert.JSONEq(t, string(redrawn), string(refreshed.Display), "the card as drawn again")
	assert.Nil(t, refreshed.DecidedAt)
	dismissed, err := f.db.DismissFoundryAgentAction(ctx, f.workspaceID, agentUser, expired.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionDismissed, dismissed.Status)
	_, err = f.db.DismissFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID)
	assert.Equal(t, model.FoundryAgentNotPrepared, refusal(t, err))
	_, err = f.db.RefreshFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, a.PayloadHash, redrawn, "c3", []byte("s3"), "k", time.Now())
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
	// The click's process, back after all, records nothing over the unknown.
	assert.ErrorIs(t, f.db.FinishFoundryAgentAction(ctx, a.ID, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionFailed, Error: "x"}, nil, nil),
		store.ErrNotFound)
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
	_, err = testDB.Exec(`INSERT INTO foundry_agent_turns (id, workspace_id, thread_id, user_id, runtime, kind, input, status, max_usd_micros)
VALUES (?, ?, ?, ?, 'api', 'message', 'hi', 'queued', 1)`, f.turnID(7), f.workspaceID, turn.ThreadID, agentUser)
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

// whileLocked holds turn's thread locked in a transaction of its own, runs
// call beside it, and runs change in the holder once call waits for the
// lock, then commits: what a send or click that committed while call waited
// must see.
func whileLocked(t *testing.T, threadID string, call func() error, change func(*sql.Tx) error) (callErr, changeErr error) {
	t.Helper()
	tx, err := testDB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var locked string
	require.NoError(t, tx.QueryRow(`SELECT id FROM foundry_agent_threads WHERE id=? FOR UPDATE`, threadID).Scan(&locked))
	done := make(chan error, 1)
	go func() { done <- call() }()
	var early error
	returned := false
	var seen []string
	waited := assert.Eventually(t, func() bool {
		select {
		case early = <-done:
			returned = true
			return true
		default:
		}
		rows, err := testDB.Query(`SELECT trx_state, COALESCE(trx_query, '') FROM information_schema.innodb_trx`)
		if err != nil {
			seen = []string{err.Error()}
			return false
		}
		defer rows.Close()
		seen = nil
		for rows.Next() {
			var state, query string
			_ = rows.Scan(&state, &query)
			seen = append(seen, state+": "+query)
			if state == "LOCK WAIT" && strings.Contains(query, "foundry_agent_threads") {
				return true
			}
		}
		return false
		// InnoDB refreshes innodb_trx only when it wasn't read for 100 ms.
	}, 10*time.Second, 200*time.Millisecond, "call waits for the thread's lock")
	require.True(t, waited, "transactions: %v", seen)
	require.False(t, returned, "call returned before it waited for the lock: %v (%v)", early, seen)
	changeErr = change(tx)
	if changeErr != nil {
		_ = tx.Rollback()
	} else {
		require.NoError(t, tx.Commit())
	}
	return <-done, changeErr
}

func TestFoundryAgentClickSeesWhatCommittedWhileItWaited(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "waited")
	require.NoError(t, f.send(1, 0))
	turn := f.claimed(t)
	a, err := f.prepare(turn, 1, time.Now().Add(5*time.Minute))
	require.NoError(t, err)
	b, err := f.prepare(turn, 2, time.Now().Add(5*time.Minute))
	require.NoError(t, err)
	f.settle(t, turn)
	click := func() error {
		_, err := f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, agentUser, b.ID, b.PayloadHash)
		return err
	}

	// Another click started first.
	clicked, changed := whileLocked(t, turn.ThreadID, click, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE foundry_agent_actions SET status='executing', decided_by=?, decided_at=NOW(6) WHERE id=?`, agentUser, a.ID)
		return err
	})
	require.NoError(t, changed)
	assert.Equal(t, model.FoundryAgentThreadBusy, refusal(t, clicked), "two clicks of a thread never run at once")
	require.NoError(t, f.db.FinishFoundryAgentAction(ctx, a.ID, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionFailed, Error: "x"}, nil, nil))

	// A message was sent first.
	clicked, changed = whileLocked(t, turn.ThreadID, click, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO foundry_agent_turns (id, workspace_id, thread_id, user_id, runtime, kind, input, status, max_usd_micros)
VALUES (?, ?, ?, ?, 'api', 'message', 'hi', 'queued', 1)`, f.turnID(7), f.workspaceID, turn.ThreadID, agentUser)
		return err
	})
	require.NoError(t, changed)
	assert.Equal(t, model.FoundryAgentThreadBusy, refusal(t, clicked), "a click waits for the turn sent before it")
	still, err := f.db.GetFoundryAgentAction(ctx, f.workspaceID, b.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionPrepared, still.Status)
}

// A send's sweep of the thread's actions and a click's end take the thread's
// lock first, so neither deadlocks the other.
func TestFoundryAgentClickEndsBesideASend(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "order")
	require.NoError(t, f.send(1, 0))
	turn := f.claimed(t)
	a, err := f.prepare(turn, 1, time.Now().Add(5*time.Minute))
	require.NoError(t, err)
	f.settle(t, turn)
	_, err = f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, a.PayloadHash)
	require.NoError(t, err)
	next := &model.FoundryAgentTurn{ID: f.turnID(2), WorkspaceID: f.workspaceID, ThreadID: turn.ThreadID, UserID: agentUser,
		Kind: model.FoundryAgentTurnActionResult, Input: a.ID, MaxUSDMicros: 500_000}
	finished, swept := whileLocked(t, turn.ThreadID, func() error {
		return f.db.FinishFoundryAgentAction(ctx, a.ID, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionFailed, Error: "no"}, next,
			json.RawMessage(`{"type":"action_result"}`))
	}, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE foundry_agent_actions SET status='unknown', error='x'
WHERE thread_id=? AND status='executing' AND decided_at < NOW(6) - INTERVAL 1 HOUR`, turn.ThreadID)
		return err
	})
	require.NoError(t, swept)
	require.NoError(t, finished)
	queued, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, next.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentRuntimeAPI, queued.Runtime, "it takes its thread's runtime")
}

// A double click on Send in a thread is the same turn, not a busy thread.
func TestFoundryAgentDoubleSendInAThread(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "double")
	require.NoError(t, f.send(1, 0))
	f.settle(t, f.claimed(t))
	sent, changed := whileLocked(t, f.threadID(1), func() error { return f.send(2, 1) }, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO foundry_agent_turns (id, workspace_id, thread_id, user_id, runtime, kind, input, status, client_token, max_usd_micros)
VALUES (?, ?, ?, ?, 'api', 'message', 'message 2', 'queued', 'send-2', 1)`, f.turnID(9), f.workspaceID, f.threadID(1), agentUser)
		return err
	})
	require.NoError(t, changed)
	assert.ErrorIs(t, sent, store.ErrConflict)
	// Another message while the turn is live is still refused as busy.
	assert.Equal(t, model.FoundryAgentThreadBusy, refusal(t, f.send(3, 1)))
	_, err := f.db.GetFoundryAgentTurnByClientToken(ctx, f.workspaceID, agentUser, "send-2")
	require.NoError(t, err)
}

// A turn whose attempt died is claimed again; the month pays the dead
// reservation, which isn't the turn's own spend.
func TestFoundryAgentReclaimCountsADeadReservationLost(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "lapse")
	require.NoError(t, f.send(1, 0))
	first := f.claimed(t)
	assert.Zero(t, first.LostMicros)
	_, err := testDB.Exec(`UPDATE foundry_agent_turns SET lease_until = NOW(6) - INTERVAL 1 SECOND WHERE id=?`, first.ID)
	require.NoError(t, err)
	again := f.claimed(t)
	assert.Equal(t, first.ID, again.ID)
	assert.Equal(t, 2, again.Attempts)
	assert.EqualValues(t, 500_000, again.Spend.USDMicros, "the month pays the dead attempt's reservation")
	assert.EqualValues(t, 500_000, again.LostMicros)
	assert.EqualValues(t, 500_000, again.ReservedMicros)
	f.settle(t, again)
	done, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, first.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 501_000, done.Spend.USDMicros)
	assert.EqualValues(t, 500_000, done.LostMicros)
}

func TestFoundryAgentClaimTakesItsRuntimeOnly(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "runtime")
	turn := &model.FoundryAgentTurn{ID: f.turnID(1), WorkspaceID: f.workspaceID, ThreadID: f.threadID(1), UserID: agentUser,
		Kind: model.FoundryAgentTurnMessage, Input: "hi", MaxUSDMicros: 500_000}
	require.NoError(t, f.db.CreateFoundryAgentTurn(ctx, turn, &model.FoundryAgentThread{ID: f.threadID(1), WorkspaceID: f.workspaceID,
		UserID: agentUser, Title: "hi", Runtime: model.FoundryAgentRuntimeClaudeCode}, f.limits()))
	none, err := f.db.ClaimFoundryAgentTurn(ctx, "w1", f.claim())
	require.NoError(t, err)
	assert.Nil(t, none, "the API's worker leaves a helper's turns")
	claim := f.claim()
	claim.Runtime = model.FoundryAgentRuntimeClaudeCode
	got, err := f.db.ClaimFoundryAgentTurn(ctx, "w1", claim)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.FoundryAgentRuntimeClaudeCode, got.Runtime)
	claim.Runtime = ""
	_, err = f.db.ClaimFoundryAgentTurn(ctx, "w1", claim)
	assert.Error(t, err, "a claim names its runtime")
}

func TestFoundryAgentClaimFailsADeletedWorkspacesTurn(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "gone")
	require.NoError(t, f.send(1, 0))
	_, err := testDB.Exec(`UPDATE workspaces SET deleted_at=NOW(6) WHERE id=?`, f.workspaceID)
	require.NoError(t, err)
	none, err := f.db.ClaimFoundryAgentTurn(ctx, "w1", f.claim())
	require.NoError(t, err)
	assert.Nil(t, none)
	failed, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, f.turnID(1))
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentTurnFailed, failed.Status)
	require.NotNil(t, failed.Error)
	assert.Equal(t, "the workspace was deleted", *failed.Error)
}

// A step's final text replaces its text_delta events in one transaction, and
// a poller past some of them reads on from the text.
func TestFoundryAgentTextSupersedesItsDeltas(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "text")
	require.NoError(t, f.send(1, 0))
	turn := f.claimed(t)
	ev := func(s string) json.RawMessage { return json.RawMessage(s) }
	require.NoError(t, f.db.AppendFoundryAgentEvents(ctx, turn.ID, "w1", []json.RawMessage{
		ev(`{"type":"started","model":"m","attempt":1}`),
		ev(`{"type":"text_delta","step":1,"text":"Hel"}`),
		ev(`{"type":"text_delta","step":1,"text":"lo"}`),
		ev(`{"type":"tool_call","tool_use_id":"a"}`),
		ev(`{"type":"text_delta","step":2,"text":"Still going"}`),
	}))
	seen, err := f.db.ListFoundryAgentEvents(ctx, turn.ID, 0, 2)
	require.NoError(t, err)
	require.Len(t, seen, 2, "a poller that read the first delta")

	assert.ErrorIs(t, f.db.FinishFoundryAgentText(ctx, turn.ID, "w2", 1, nil), store.ErrNotFound, "only under the lease")
	require.NoError(t, f.db.FinishFoundryAgentText(ctx, turn.ID, "w1", 1, []json.RawMessage{ev(`{"type":"text","step":1,"text":"Hello"}`)}))
	all, err := f.db.ListFoundryAgentEvents(ctx, turn.ID, 0, 100)
	require.NoError(t, err)
	var types []string
	for _, e := range all {
		var head struct {
			Type string
			Step int
		}
		require.NoError(t, json.Unmarshal(e.Event, &head))
		types = append(types, fmt.Sprintf("%s/%d", head.Type, head.Step))
	}
	assert.Equal(t, []string{"started/0", "tool_call/0", "text_delta/2", "text/1"}, types, "only step 1's deltas go")
	next, err := f.db.ListFoundryAgentEvents(ctx, turn.ID, seen[1].Seq, 100)
	require.NoError(t, err)
	require.Len(t, next, 3)
	assert.Equal(t, 6, next[2].Seq, "the text comes after every event before it")

	// A step that ended with no text (its words were blank) drops its deltas alone.
	require.NoError(t, f.db.FinishFoundryAgentText(ctx, turn.ID, "w1", 2, nil))
	all, err = f.db.ListFoundryAgentEvents(ctx, turn.ID, 0, 100)
	require.NoError(t, err)
	assert.Len(t, all, 3)
	f.settle(t, turn)
	done, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, turn.ID)
	require.NoError(t, err)
	assert.Equal(t, 6, done.LastSeq, "seq never goes back")
}

// A helper's thread runs on its owner's subscription: its turns are stored
// with a cap of 0, spend nothing of the month's budget (even one spent), are
// claimed by their runtime alone, and keep the CLI's session under the lease.
func TestFoundryAgentSubscriptionTurns(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "subs")
	limits := f.limits()
	limits.CapMicros = 1
	token := "sub-1"
	turn := &model.FoundryAgentTurn{ID: f.turnID(1), WorkspaceID: f.workspaceID, ThreadID: f.threadID(1), UserID: agentUser,
		Kind: model.FoundryAgentTurnMessage, Input: "load it", ClientToken: &token, MaxUSDMicros: 500_000}
	thread := &model.FoundryAgentThread{ID: f.threadID(1), WorkspaceID: f.workspaceID, UserID: agentUser, Title: "Load it",
		Runtime: model.FoundryAgentRuntimeClaudeCode}
	require.NoError(t, f.db.CreateFoundryAgentTurn(ctx, turn, thread, limits), "a $0 turn fits any budget")
	stored, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, turn.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentRuntimeClaudeCode, stored.Runtime)
	assert.Zero(t, stored.MaxUSDMicros)

	// An 'api' claim never takes it; its own runtime's does.
	none, err := f.db.ClaimFoundryAgentTurn(ctx, "w1", f.claim())
	require.NoError(t, err)
	assert.Nil(t, none)
	claim := f.claim()
	claim.Runtime = model.FoundryAgentRuntimeCodex
	none, err = f.db.ClaimFoundryAgentTurn(ctx, "w1", claim)
	require.NoError(t, err)
	assert.Nil(t, none)
	claim.Runtime = model.FoundryAgentRuntimeClaudeCode
	claimed, err := f.db.ClaimFoundryAgentTurn(ctx, "host-1", claim)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Zero(t, claimed.ReservedMicros)

	// The session is kept only under the lease.
	assert.ErrorIs(t, f.db.SetFoundryAgentThreadSession(ctx, claimed.ID, "host-2", "s-1"), store.ErrNotFound)
	require.NoError(t, f.db.SetFoundryAgentThreadSession(ctx, claimed.ID, "host-1", "s-1"))
	got, err := f.db.GetFoundryAgentThread(ctx, f.workspaceID, f.threadID(1))
	require.NoError(t, err)
	assert.Equal(t, "s-1", got.RuntimeSessionID)
	assert.Error(t, f.db.SetFoundryAgentThreadSession(ctx, claimed.ID, "host-1", strings.Repeat("s", 129)))

	// Its action's continuation is a $0 turn too.
	a, err := f.prepare(claimed, 1, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, f.db.SettleFoundryAgentTurn(ctx, claimed.ID, "host-1", model.FoundryAgentTurnOutcome{Status: model.FoundryAgentTurnDone, Steps: 1}))
	_, err = f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, a.PayloadHash)
	require.NoError(t, err)
	next := &model.FoundryAgentTurn{ID: f.turnID(2), WorkspaceID: f.workspaceID, ThreadID: f.threadID(1), UserID: agentUser,
		Kind: model.FoundryAgentTurnActionResult, Input: a.ID, MaxUSDMicros: 500_000}
	require.NoError(t, f.db.FinishFoundryAgentAction(ctx, a.ID, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionSucceeded,
		Result: json.RawMessage(`{}`)}, next, json.RawMessage(`{"type":"action_result"}`)))
	queued, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, next.ID)
	require.NoError(t, err)
	assert.Zero(t, queued.MaxUSDMicros)
	assert.Equal(t, model.FoundryAgentRuntimeClaudeCode, queued.Runtime)

	// The table refuses an 'api' turn without a cap, and a helper's with one.
	_, err = testDB.Exec(`INSERT INTO foundry_agent_turns (id, workspace_id, thread_id, user_id, runtime, kind, input, status, max_usd_micros)
VALUES (?, ?, ?, ?, 'api', 'message', 'x', 'queued', 0)`, f.turnID(3), f.workspaceID, f.threadID(7), agentUser)
	assert.Error(t, err)
	_, err = testDB.Exec(`INSERT INTO foundry_agent_turns (id, workspace_id, thread_id, user_id, runtime, kind, input, status, max_usd_micros)
VALUES (?, ?, ?, ?, 'codex', 'message', 'x', 'queued', 5)`, f.turnID(4), f.workspaceID, f.threadID(8), agentUser)
	assert.Error(t, err)
}
