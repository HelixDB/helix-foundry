package slackbot

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// relayToken is the fake door's link token.
const relayToken = "hx_rl_test"

// fakeDoor is Helix's door as a standalone install calls Slack through it:
// each call it takes under the link's token is sent on to the fake Slack as
// JSON, and Slack's answer comes back as it is; refusals answers a method
// with the relay's own refusal instead.
type fakeDoor struct {
	*httptest.Server
	slackAPI  string
	mu        sync.Mutex
	calls     []doorCall
	approvals bool
	linked    bool
	refusals  map[string]int
}

// doorCall is one call the door took: its method and local workspace.
type doorCall struct{ method, workspace string }

func newFakeDoor(t *testing.T) *fakeDoor {
	d := &fakeDoor{approvals: true, linked: true, refusals: map[string]int{}}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ok := strings.CutPrefix(r.URL.Path, "/v1/relay/slack/")
		if r.Method != http.MethodPost || !ok || r.Header.Get("Authorization") != "Bearer "+relayToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_token","message":"no"}}`))
			return
		}
		var call struct {
			LocalWorkspaceID string          `json:"local_workspace_id"`
			Args             json.RawMessage `json:"args"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &call); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.calls = append(d.calls, doorCall{method: method, workspace: call.LocalWorkspaceID})
		refusal := d.refusals[method]
		d.mu.Unlock()
		if refusal != 0 {
			w.WriteHeader(refusal)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"the relay doesn't allow that"}}`))
			return
		}
		resp, err := http.Post(d.slackAPI+"/"+method, "application/json", bytes.NewReader(call.Args))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if after := resp.Header.Get("Retry-After"); after != "" {
			w.Header().Set("Retry-After", after)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}))
	t.Cleanup(d.Close)
	return d
}

// access is the install's link, as Relayed reads it.
func (d *fakeDoor) access(context.Context) (RelayAccess, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.linked {
		return RelayAccess{}, store.ErrNotFound
	}
	return RelayAccess{Door: d.URL, Token: relayToken, SlackApprovals: d.approvals}, nil
}

func (d *fakeDoor) approve(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.approvals = on
}

func (d *fakeDoor) took() []doorCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]doorCall(nil), d.calls...)
}

// newLocalWorld is a standalone install's world: its Slack reached through
// the fake door, Ada its owner (who installed the app, linked in full) and
// Grace allowed by her, read-only, as her.
func newLocalWorld(t *testing.T) (*world, *fakeDoor) {
	t.Helper()
	door := newFakeDoor(t)
	w := newWorld(t, func(o *Options) {
		door.slackAPI = o.WebAPI
		relayed := &Relayed{DB: o.DB, Access: door.access}
		limiter, err := slack.NewLimiter(1)
		require.NoError(t, err)
		client, err := slack.NewClient(slack.ClientOptions{HTTP: relayed.Client(), Limiter: limiter})
		require.NoError(t, err)
		o.Slack, o.Relayed = client, relayed
	})
	require.NoError(t, w.db.DeleteFoundrySlackLink(w.ctx, w.install.ID, grace))
	_, err := w.db.AllowFoundrySlackUser(w.ctx, w.install.ID, grace, adaID)
	require.NoError(t, err)
	return w, door
}

// The owner's own Slack account asks in full while they approve changes
// from Slack, and read-only while they don't; every call goes through the
// relay, as the install's local workspace.
func TestLocalOwnerAsksThroughTheRelay(t *testing.T) {
	w, door := newLocalWorld(t)
	w.message(ada, dmAda, ts(1), ts(1), "Which customers renewed?")
	w.settle()
	turn := w.only()
	assert.Equal(t, model.AgentPerson{UserID: adaID}, turn.Principal)
	assert.False(t, turn.ReadOnly)
	require.NotEmpty(t, door.took())
	for _, c := range door.took() {
		assert.Equal(t, w.ws, c.workspace, c.method)
	}
	require.Len(t, w.slack.posts("Working"), 1, "the reply went through the relay to Slack")
	assert.Equal(t, relayedUpdateGap, w.bot.updateGap)

	door.approve(false)
	w.message(ada, dmAda, ts(2), ts(2), "And last month?")
	w.settle()
	assert.True(t, w.last().ReadOnly, "approving from Slack is off: the owner's turns only read and ask")
}

// Someone the owner allowed asks read-only, as the owner, in a thread of
// their own.
func TestLocalAllowedPersonAsksReadOnly(t *testing.T) {
	w, _ := newLocalWorld(t)
	w.message(grace, general, ts(1), ts(1), "<@U0BOT> who renewed?")
	w.settle()
	turn := w.only()
	assert.True(t, turn.ReadOnly)
	assert.Equal(t, model.AgentPerson{UserID: adaID}, turn.Principal, "as the owner")
	thread, err := w.db.GetFoundrySlackThread(w.ctx, w.install.ID, general, ts(1), grace)
	require.NoError(t, err, "the thread is theirs")
	assert.Equal(t, turn.ThreadID, thread.AgentThreadID)
}

// Anyone else asks the owner once, and gets no link; the app's installer
// gets the owner's link.
func TestLocalStrangersAskTheOwnerOnce(t *testing.T) {
	w, _ := newLocalWorld(t)
	w.message(alan, "D0ALAN", ts(1), ts(1), "hello")
	w.message(alan, general, ts(2), ts(2), "<@U0BOT> hello again")
	w.settle()
	assert.Empty(t, w.turns())
	assert.Empty(t, w.slack.posts("Link your Slack account"), "only the installer is offered a link")
	asked := 0
	for _, c := range append(w.slack.of("chat.postMessage"), w.slack.of("chat.postEphemeral")...) {
		if strings.Contains(c.text(), "hasn't let you use it yet") {
			asked++
		}
	}
	assert.Equal(t, 1, asked, "told once")
	requests, err := w.db.ListFoundrySlackAccessRequests(w.ctx, w.install.ID)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, alan, requests[0].SlackUserID)
	assert.Equal(t, "alan", requests[0].SlackName, "as Slack shows them")

	require.NoError(t, w.db.DeleteFoundrySlackLink(w.ctx, w.install.ID, ada))
	w.message(ada, dmAda, ts(3), ts(3), "hello")
	w.settle()
	assert.Len(t, w.slack.posts("Link your Slack account"), 1, "the installer links as the owner")
}

// On a standalone install only the owner's own Slack account acts on a
// card, and only while they approve changes from Slack.
func TestLocalCardsAreTheOwnersWhileApproving(t *testing.T) {
	w, door := newLocalWorld(t)
	w.message(ada, dmAda, ts(1), ts(1), "load Sales")
	w.settle()
	turn := w.only()
	run := w.claimTurn(turn.ID)
	card := run.card(time.Now().Add(time.Hour))
	run.finish(agent.StopAnswered, "")
	w.until("the reply", w.replyDone(turn.ID))
	r, err := w.db.GetFoundrySlackReply(w.ctx, turn.ID)
	require.NoError(t, err)
	cardTS := r.Posted["a:"+card.ID]
	require.NotEmpty(t, cardTS)
	approve := func(user string) {
		w.click(slack.Approve{Clicked: w.clicked(user, dmAda, ts(1), cardTS), ActionID: card.ID, PayloadHash: card.PayloadHash}, user)
		w.settle()
	}

	approve(grace)
	assert.True(t, w.told(grace, "Only the person this was prepared for"), "a read-only link acts as the owner, but never on a card")
	door.approve(false)
	approve(ada)
	assert.True(t, w.told(ada, "Approving changes from Slack is off"))
	stored, err := w.db.GetFoundryAgentAction(w.ctx, w.ws, card.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionPrepared, stored.Status)
	assert.Empty(t, w.slack.of("conversations.info"), "a direct message is never shared")

	door.approve(true)
	approve(ada)
	stored, err = w.db.GetFoundryAgentAction(w.ctx, w.ws, card.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionSucceeded, stored.Status)
}

// A relayed install asks Slack, through the relay, whether a channel it
// was sent a card in has been shared with another organisation since: Helix
// relays the click without asking.
func TestLocalClicksInChannelsSharedSince(t *testing.T) {
	w, door := newLocalWorld(t)
	a, cardTS := w.cardInThread(general)
	w.slack.mu.Lock()
	w.slack.shared[general] = true
	w.slack.mu.Unlock()
	w.click(slack.Approve{Clicked: w.clicked(ada, general, ts(1), cardTS), ActionID: a.ID, PayloadHash: a.PayloadHash}, ada)
	w.settle()
	assert.True(t, w.told(ada, "doesn't act in channels shared with other organisations"))
	stored, err := w.db.GetFoundryAgentAction(w.ctx, w.ws, a.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentActionPrepared, stored.Status)
	assert.Contains(t, door.took(), doorCall{method: "conversations.info", workspace: w.ws}, "asked through the relay")
}

// A call the relay refuses for good is Slack's refusal: the row gives up,
// and isn't tried again.
func TestLocalRelayRefusalGivesUp(t *testing.T) {
	w, door := newLocalWorld(t)
	door.mu.Lock()
	door.refusals["chat.postEphemeral"] = http.StatusForbidden
	door.mu.Unlock()
	in := w.message(alan, general, ts(1), ts(1), "<@U0BOT> hello")
	w.settle()
	r := w.row(in)
	assert.Equal(t, "failed", r.status)
	assert.Contains(t, r.error.String, "relay_forbidden")
}

// While the install isn't linked, nothing reaches Slack: a row is tried
// again.
func TestLocalUnlinkedRetries(t *testing.T) {
	w, door := newLocalWorld(t)
	door.mu.Lock()
	door.linked = false
	door.mu.Unlock()
	in := w.message(ada, dmAda, ts(1), ts(1), "hello")
	require.Equal(t, 1, w.handle())
	r := w.row(in)
	assert.Equal(t, "queued", r.status)
	assert.Equal(t, 1, r.failures)
	approvals, err := w.bot.relayed.SlackApprovals(w.ctx)
	require.NoError(t, err)
	assert.False(t, approvals, "an unlinked install approves nothing")
}
