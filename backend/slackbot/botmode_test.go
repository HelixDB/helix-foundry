package slackbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// botCred is the bot-mode install's service credential.
const botCred = "01JBCRED00000000000000BOT1"

// people is the workspace's members as ports.People says: Ada owns it,
// Grace is a member; err fails Member.
type people struct {
	mu    sync.Mutex
	roles map[string]ports.Role
	err   error
}

func (p *people) MayManageAI(context.Context, string, string) bool { return false }

func (p *people) DisplayName(_ context.Context, userID string) (string, error) {
	if userID == adaID {
		return "Ada Lovelace", nil
	}
	return "", store.ErrNotFound
}

func (p *people) Member(_ context.Context, _, userID string) (ports.Member, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ports.Member{Role: p.roles[userID]}, p.err
}

// approver is agent.Proposals as the tests need it: a proposal's card
// drawn from display (create_load's by default), run by the store's claim
// and end, succeeding with its continuation; err refuses both calls, and
// spec is the workflow steps a review shows.
type approver struct {
	w       *world
	mu      sync.Mutex
	err     error
	display map[string]any
	spec    string
	runs    int
}

func (a *approver) set(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

func (a *approver) Review(ctx context.Context, ws, actionID, _ string) (agent.ProposalReview, error) {
	a.mu.Lock()
	err, spec := a.err, a.spec
	a.mu.Unlock()
	if err != nil {
		return agent.ProposalReview{}, err
	}
	act, err := a.w.db.GetFoundryAgentAction(ctx, ws, actionID)
	if err != nil {
		return agent.ProposalReview{}, err
	}
	switch shown := act.Shown(time.Now()); shown {
	case model.FoundryAgentActionPrepared:
	case model.FoundryAgentActionExpired:
		return agent.ProposalReview{}, model.FoundryAgentRefusal{Reason: model.FoundryAgentExpired, Detail: "the proposal expired"}
	default:
		return agent.ProposalReview{}, model.FoundryAgentRefusal{Reason: model.FoundryAgentNotPrepared, Detail: "the proposal is " + string(shown)}
	}
	display, ok := a.display[act.Operation]
	if !ok {
		display = agent.CreateLoadDisplay{Ontology: agent.Ref{ID: "ont_1", Name: "Sales"}, Version: agent.VersionRef{ID: "ver_3", Number: 3}}
	}
	raw, err := json.Marshal(display)
	return agent.ProposalReview{Action: act, Display: raw, SpecHash: spec}, err
}

func (a *approver) Run(ctx context.Context, ws, actionID, payloadHash, specHash, approverID string) (agent.ProposalRun, error) {
	reviewed, err := a.Review(ctx, ws, actionID, approverID)
	switch {
	case err != nil:
		return agent.ProposalRun{}, err
	case reviewed.SpecHash != specHash:
		return agent.ProposalRun{}, model.FoundryAgentRefusal{Reason: model.FoundryAgentChanged, Detail: "it changed"}
	}
	act, err := a.w.db.ClaimFoundryAgentProposal(ctx, ws, actionID, payloadHash, approverID)
	if err != nil {
		return agent.ProposalRun{}, err
	}
	end := model.FoundryAgentActionEnd{Status: model.FoundryAgentActionSucceeded, Result: json.RawMessage(`{"id":"load_1"}`)}
	first, err := agent.MarshalEvent(agent.ActionResult{ActionID: act.ID, Operation: act.Operation, Status: "succeeded"})
	if err != nil {
		return agent.ProposalRun{}, err
	}
	next := &model.FoundryAgentTurn{ID: id.New(), WorkspaceID: ws, ThreadID: act.ThreadID, Kind: model.FoundryAgentTurnActionResult, Input: act.ID,
		MaxUSDMicros: 1_000_000}
	if err := a.w.db.FinishFoundryAgentAction(ctx, act.ID, end, next, first); err != nil {
		return agent.ProposalRun{}, err
	}
	done, err := a.w.db.GetFoundryAgentAction(ctx, ws, act.ID)
	if err != nil {
		return agent.ProposalRun{}, err
	}
	a.mu.Lock()
	a.runs++
	a.mu.Unlock()
	out := agent.ProposalRun{Action: done, End: end}
	if next.Principal != nil {
		out.Continuation = next
	}
	return out, nil
}

// newBotWorld is newWorld with its install in bot mode, approvals by the
// workspace's owners and admins (Ada), and with mutating its options.
func newBotWorld(t *testing.T, with ...func(*Options)) (*world, *people, *approver) {
	t.Helper()
	ppl := &people{roles: map[string]ports.Role{adaID: ports.RoleOwner, graceID: ports.RoleMember}}
	appr := &approver{display: map[string]any{}}
	w := newWorld(t, append([]func(*Options){func(o *Options) { o.People, o.Proposals = ppl, appr }}, with...)...)
	appr.w = w
	_, err := w.db.SetFoundrySlackMode(w.ctx, w.ws, w.install.ID, model.SlackSharedBot{CredentialID: botCred, Scope: model.SlackBotWorkspace{}})
	require.NoError(t, err)
	w.install, err = w.db.GetFoundrySlackInstallByID(w.ctx, w.install.ID)
	require.NoError(t, err)
	return w, ppl, appr
}

// botMessage queues a message to the bot from user, as the API records one
// in bot mode: its Slack thread one conversation, whoever wrote it.
func (w *world) botMessage(user, channel, threadTS, msgTS, text string) string {
	w.t.Helper()
	at := slack.Conversation{TeamID: team, ChannelID: channel, ThreadTS: threadTS}
	var e slack.Event = slack.DirectMessage{At: at, UserID: user, TS: msgTS, Text: text, EventTime: time.Now()}
	if !inDM(channel) {
		e = slack.Mention{At: at, UserID: user, TS: msgTS, Text: text, EventTime: time.Now()}
	}
	return w.queueEvent(e, user, model.SlackConversation(w.install.ID, channel, threadTS, ""))
}

// propose records an action of operation the bot's turn proposed, waiting
// until expires, with its event.
func (r *stubRun) propose(operation string, expires time.Time) *model.FoundryAgentAction {
	r.w.t.Helper()
	a, err := r.w.db.RecordFoundryAgentAction(r.w.ctx, r.worker, &model.FoundryAgentAction{ID: id.New(), TurnID: r.turn.ID, ThreadID: r.turn.ThreadID,
		WorkspaceID: r.w.ws, Principal: r.turn.Principal, Operation: operation, Target: "workspace:" + r.w.ws,
		Payload: json.RawMessage(`{"version_id":"ver_3"}`), PayloadHash: fmt.Sprintf("%064x", time.Now().UnixNano()), Display: json.RawMessage(`{}`),
		Approval: model.AdminApproval{}, Status: model.FoundryAgentActionPrepared, ExpiresAt: expires}, 5,
		func(a *model.FoundryAgentAction) (json.RawMessage, error) {
			return agent.MarshalEvent(agent.Proposed{ActionID: a.ID, Operation: a.Operation})
		})
	require.NoError(r.w.t, err)
	return a
}

// proposed is a turn of Alan's in #general's thread ts(1) that proposed
// one action of operation per expiry, its reply done: the actions, and
// their cards' ts.
func (w *world) proposed(operation string, expiries ...time.Time) ([]*model.FoundryAgentAction, []string) {
	w.t.Helper()
	w.botMessage(alan, general, ts(1), ts(1), "<@U0BOT> load Sales")
	w.settle()
	turn := w.last()
	run := w.claimTurn(turn.ID)
	var actions []*model.FoundryAgentAction
	for _, expires := range expiries {
		actions = append(actions, run.propose(operation, expires))
	}
	run.finish(agent.StopAnswered, "")
	w.until("the proposals' reply", w.replyDone(turn.ID))
	r, err := w.db.GetFoundrySlackReply(w.ctx, turn.ID)
	require.NoError(w.t, err)
	var cards []string
	for _, a := range actions {
		require.True(w.t, model.ValidSlackTS(r.Posted["a:"+a.ID]), "a card for each")
		cards = append(cards, r.Posted["a:"+a.ID])
	}
	return actions, cards
}

// cardShows is the newest update of the message at messageTS, "" for none.
func (w *world) cardShows(messageTS string) string {
	out := ""
	for _, c := range w.slack.of("chat.update") {
		if c.args["ts"] == messageTS {
			out = c.text()
		}
	}
	return out
}

func TestBotModeAnswersAnyoneInOneThread(t *testing.T) {
	w, _, _ := newBotWorld(t)
	w.botMessage(alan, general, ts(1), ts(1), "<@U0BOT> how many renewals?")
	w.settle()
	first := w.only()
	assert.Equal(t, model.AgentBot{CredentialID: botCred}, first.Principal)
	assert.Equal(t, "[Slack] alan wrote:\nhow many renewals?", first.Input, "the bot's thread is everyone's: it says who wrote it")
	assert.Equal(t, model.FoundryAgentSurfaceSlack, first.Surface)
	assert.False(t, w.told(alan, "Link your Slack account"), "no one links to talk to the bot")
	require.Len(t, w.slack.posts("Working"), 1)
	w.claimTurn(first.ID).finish(agent.StopAnswered, "")
	w.until("the first reply", w.replyDone(first.ID))

	// Someone else in the same Slack thread continues the same Founder thread.
	w.botMessage(grace, general, ts(1), ts(2), "<@U0BOT> and last month?")
	w.settle()
	second := w.last()
	assert.Equal(t, first.ThreadID, second.ThreadID)
	assert.Equal(t, "[Slack] grace wrote:\nand last month?", second.Input)
	assert.Equal(t, 1, w.threads())
	mapping, err := w.db.GetFoundrySlackThread(w.ctx, w.install.ID, general, ts(1), "")
	require.NoError(t, err)
	for user, want := range map[string]bool{alan: true, grace: true, ada: false} {
		spoke, err := w.db.IsFoundrySlackThreadPerson(w.ctx, mapping.ID, user)
		require.NoError(t, err)
		assert.Equal(t, want, spoke, user)
	}
	w.claimTurn(second.ID).finish(agent.StopAnswered, "")
	w.until("the second reply", w.replyDone(second.ID))

	// A reply the API took unmentioned is the thread's too.
	w.queueEvent(slack.ThreadReply{At: slack.Conversation{TeamID: team, ChannelID: general, ThreadTS: ts(1)}, UserID: alan, TS: ts(3),
		Text: "and the year before?", EventTime: time.Now()}, alan, model.SlackConversation(w.install.ID, general, ts(1), ""))
	w.settle()
	third := w.last()
	assert.Equal(t, first.ThreadID, third.ThreadID)
	assert.Equal(t, "[Slack] alan wrote:\nand the year before?", third.Input)

	// A direct message is a thread of its own, as the bot too, whoever wrote it.
	w.botMessage(ada, dmAda, ts(9), ts(9), "hello")
	w.settle()
	dm := w.last()
	assert.NotEqual(t, first.ThreadID, dm.ThreadID)
	assert.Equal(t, model.AgentBot{CredentialID: botCred}, dm.Principal)
	assert.Equal(t, 2, w.threads())
}

// The bot says who wrote a message on one line, its name cut, and leaves
// room for it in Founder's limit.
func TestBotModeSaysWhoWroteIt(t *testing.T) {
	w, _, _ := newBotWorld(t)
	w.slack.mu.Lock()
	w.slack.users[alan]["profile"] = map[string]any{"display_name": "Alan\nTuring\x07 " + strings.Repeat("x", 100)}
	w.slack.mu.Unlock()
	w.botMessage(alan, general, ts(1), ts(1), "<@U0BOT> hi")
	w.settle()
	name := "Alan Turing " + strings.Repeat("x", 67) + "…"
	assert.Equal(t, "[Slack] "+name+" wrote:\nhi", w.only().Input)

	prefix := len([]rune("[Slack] " + name + " wrote:\n"))
	w.botMessage(alan, general, ts(2), ts(2), "<@U0BOT> "+strings.Repeat("y", maxInputChars-prefix+1))
	w.settle()
	assert.True(t, w.told(alan, fmt.Sprintf("keep it under %d characters", maxInputChars-prefix)))
	assert.Len(t, w.turns(), 1)
}

func TestSpeaker(t *testing.T) {
	for name, c := range map[string]struct {
		user slack.User
		want string
	}{
		"display name":       {slack.User{ID: "U1", DisplayName: "Ada"}, "Ada"},
		"lines and controls": {slack.User{ID: "U1", DisplayName: "Ada\n\tLovelace\x00!"}, "Ada Lovelace !"},
		"only controls":      {slack.User{ID: "U1", DisplayName: "\x01\x02", RealName: ""}, "U1"},
		"long":               {slack.User{ID: "U1", DisplayName: strings.Repeat("é", 81)}, strings.Repeat("é", 79) + "…"},
		"just fits":          {slack.User{ID: "U1", DisplayName: strings.Repeat("é", 80)}, strings.Repeat("é", 80)},
	} {
		assert.Equal(t, c.want, speaker(c.user), name)
	}
}

// One Slack user sends the bot at most foundry.slack.bot.per_user_per_hour
// messages an hour; an unmentioned reply past it is dropped unsaid.
func TestBotModeCapsEachPersonsHour(t *testing.T) {
	w, _, _ := newBotWorld(t, func(o *Options) { o.Cfg.Bot.PerUserPerHour = 2 })
	for n := 1; n <= 3; n++ {
		w.botMessage(alan, general, ts(n), ts(n), "<@U0BOT> question "+fmt.Sprint(n))
	}
	w.settle()
	assert.Len(t, w.turns(), 2)
	assert.True(t, w.told(alan, "You've sent me 2 messages in the last hour"))
	w.botMessage(grace, general, ts(4), ts(4), "<@U0BOT> mine")
	w.settle()
	assert.Len(t, w.turns(), 3, "another person's hour is their own")

	w.queueEvent(slack.ThreadReply{At: slack.Conversation{TeamID: team, ChannelID: general, ThreadTS: ts(1)}, UserID: alan, TS: ts(5),
		Text: "and?", EventTime: time.Now()}, alan, model.SlackConversation(w.install.ID, general, ts(1), ""))
	w.settle()
	assert.Len(t, w.turns(), 3)
	capped := 0
	for _, c := range w.slack.of("chat.postEphemeral") {
		if strings.Contains(c.text(), "You've sent me") {
			capped++
		}
	}
	assert.Equal(t, 1, capped, "an unmentioned reply past the cap is dropped unsaid")
}

// A bot whose key Foundry refuses says bot mode is off: at its session, at
// the API, and when its turn fails for that.
func TestBotModeKeyRefused(t *testing.T) {
	w, _, _ := newBotWorld(t)
	w.sessions.refuse(botCred, status.Error(codes.PermissionDenied, "bot key has database permissions"))
	w.botMessage(alan, general, ts(1), ts(1), "<@U0BOT> hi")
	w.settle()
	assert.True(t, w.told(alan, "Bot mode is off: an owner or admin can turn it on again in Settings › Slack."))
	assert.Empty(t, w.turns())

	w.sessions.refuse(botCred, nil)
	w.founder.mu.Lock()
	w.founder.sendErr = status.Error(codes.PermissionDenied, "Founder answers people signed in to Foundry, and a Slack install's bot")
	w.founder.mu.Unlock()
	w.botMessage(grace, general, ts(2), ts(2), "<@U0BOT> hi")
	w.settle()
	assert.True(t, w.told(grace, "Bot mode is off"))
	assert.Empty(t, w.turns())

	w.founder.mu.Lock()
	w.founder.sendErr = nil
	w.founder.mu.Unlock()
	w.botMessage(ada, general, ts(3), ts(3), "<@U0BOT> hi")
	w.settle()
	turn := w.only()
	w.claimTurn(turn.ID).finish(agent.StopFailed, agent.BotKeyRefused)
	w.until("the reply", w.replyDone(turn.ID))
	updates := w.slack.of("chat.update")
	assert.Contains(t, updates[len(updates)-1].text(), "This bot's key was revoked or changed. Bot mode is off")

	// A workspace gone revokes the install.
	w.sessions.refuse(botCred, status.Error(codes.NotFound, "workspace not found"))
	w.botMessage(ada, general, ts(4), ts(4), "<@U0BOT> hi")
	w.settle()
	got, err := w.db.GetFoundrySlackInstallByID(w.ctx, w.install.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.RevokedAt)
}

// A Slack thread whose Founder thread read a source taken from the bot
// since says so at once and ends, rather than retrying a send Founder
// refuses for good; a new Slack thread starts afresh.
func TestBotThreadWhoseSourcesWereTakenEnds(t *testing.T) {
	w, _, _ := newBotWorld(t, func(o *Options) { o.Cfg.MaxAttempts = 3 })
	w.botMessage(alan, general, ts(1), ts(1), "<@U0BOT> how many renewals?")
	w.settle()
	first := w.only()
	w.claimTurn(first.ID).finish(agent.StopAnswered, "")
	w.until("the reply", w.replyDone(first.ID))

	w.founder.mu.Lock()
	w.founder.sendErr = status.Error(codes.NotFound, "thread not found")
	sends := w.founder.sends
	w.founder.mu.Unlock()
	row := w.botMessage(grace, general, ts(1), ts(2), "<@U0BOT> and last month?")
	w.settle()
	assert.Equal(t, "done", w.row(row).status)
	assert.True(t, w.told(grace, "I can't go on with this conversation"))
	w.founder.mu.Lock()
	assert.Equal(t, sends+1, w.founder.sends, "Founder is asked once")
	w.founder.sendErr = nil
	w.founder.mu.Unlock()

	w.botMessage(grace, general, ts(3), ts(3), "<@U0BOT> how many renewals?")
	w.settle()
	assert.NotEqual(t, first.ThreadID, w.last().ThreadID)
}

// botKeys is the host's bots' keys: those it revoked, and a failure to
// give.
type botKeys struct {
	mu      sync.Mutex
	revoked []string
	fail    error
}

func (k *botKeys) Create(context.Context, string, string, string) (string, error) {
	return "", errors.New("the sweep makes no key")
}

func (k *botKeys) Get(context.Context, string, string) (ports.BotCredential, error) {
	return ports.BotCredential{}, errors.New("the sweep reads no key")
}

func (k *botKeys) Revoke(_ context.Context, _, credentialID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail != nil {
		return k.fail
	}
	k.revoked = append(k.revoked, credentialID)
	return nil
}

// Slack's own uninstall leaves the bot's key on the revoked install: the
// sweep revokes it where the host makes keys, drops its source grants, and
// forgets it, which lifts the host's lock; a failure is tried again.
func TestSweepRevokesARemovedInstallsBotKey(t *testing.T) {
	keys := &botKeys{fail: errBroken}
	w, _, _ := newBotWorld(t, func(o *Options) { o.BotKeys = keys })
	bot := access.Principal{Kind: access.PrincipalServiceCredential, ID: botCred}
	require.NoError(t, w.db.SetFoundryAccessGrants(w.ctx, w.ws, bot, []access.Target{access.EnrichmentTarget()}, nil))
	outcome, err := w.db.UninstallFoundrySlackTeam(w.ctx, team, id.New(), model.SlackEventDedupe("Ev"+id.New()), time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, model.SlackUninstallDone, outcome)
	grants := func() int {
		got, err := w.db.ListFoundryAccessGrants(w.ctx, w.ws, []access.Principal{bot})
		require.NoError(t, err)
		return len(got)
	}

	w.bot.Sweep(w.ctx)
	locked, err := w.db.IsFoundrySlackBotCredential(w.ctx, botCred)
	require.NoError(t, err)
	assert.True(t, locked, "the host refused: the key stays named for the next sweep")
	assert.Equal(t, 1, grants())

	keys.mu.Lock()
	keys.fail = nil
	keys.mu.Unlock()
	w.bot.Sweep(w.ctx)
	assert.Equal(t, []string{botCred}, keys.revoked)
	assert.Zero(t, grants(), "its source grants go with it")
	locked, err = w.db.IsFoundrySlackBotCredential(w.ctx, botCred)
	require.NoError(t, err)
	assert.False(t, locked)
	w.bot.Sweep(w.ctx)
	assert.Len(t, keys.revoked, 1, "once")
}

// Without the host's keys (a standalone install, which makes none) the
// sweep leaves a revoked install's bot key be.
func TestSweepLeavesBotKeysWithoutTheHosts(t *testing.T) {
	w, _, _ := newBotWorld(t)
	require.NoError(t, w.db.RevokeFoundrySlackInstall(w.ctx, w.ws, w.install.ID, true))
	w.bot.Sweep(w.ctx)
	locked, err := w.db.IsFoundrySlackBotCredential(w.ctx, botCred)
	require.NoError(t, err)
	assert.True(t, locked)
}

func TestProposalsAreReviewedAndRunByAnOwnerOrAdmin(t *testing.T) {
	w, _, appr := newBotWorld(t)
	actions, cards := w.proposed("create_load", time.Now().Add(time.Hour))
	a, cardTS := actions[0], cards[0]
	posted := w.slack.posts("Load a version")
	require.Len(t, posted, 1, "its card once")
	assert.Contains(t, posted[0].text(), slack.ActionReview)
	assert.Contains(t, posted[0].text(), "Proposed for \\u003c@U0ALAN\\u003e")
	assert.NotContains(t, posted[0].text(), "Sales", "what it acts on shows only to its reviewer")
	statuses := w.slack.of("agents.sessions.setStatus")
	assert.Equal(t, "suspended", statuses[len(statuses)-1].args["status"], "a proposal waits")

	review := func(user string) {
		w.click(slack.Review{Clicked: w.clicked(user, general, ts(1), cardTS), ActionID: a.ID, PayloadHash: a.PayloadHash}, user)
		w.settle()
	}
	// Alan isn't linked: he's asked to, and shown nothing of it.
	review(alan)
	assert.True(t, w.told(alan, "review and run the bot's proposals as yourself"))
	assert.False(t, w.told(alan, "Load v3"))
	// Grace is linked, but only a member.
	review(grace)
	assert.True(t, w.told(grace, "Only an owner or admin of the Foundry workspace"))
	assert.False(t, w.told(grace, "Load v3"))
	// Ada owns the workspace: the card drawn as her, with Run as her.
	review(ada)
	var drawn string
	for _, c := range w.slack.of("chat.postEphemeral") {
		if c.args["user"] == ada && strings.Contains(c.text(), "Load v3 of") {
			drawn = c.text()
		}
	}
	require.NotEmpty(t, drawn, "the card, only for Ada")
	assert.Contains(t, drawn, slack.ActionRun)
	assert.Contains(t, drawn, a.PayloadHash)
	assert.Contains(t, drawn, "Runs as you (Ada Lovelace)")
	assert.Equal(t, model.FoundryAgentActionPrepared, w.action(a.ID).Status, "a review runs nothing")

	// Run, from the message only she sees.
	run := func(user string) {
		k := w.clicked(user, general, ts(1), ts(77))
		k.Ephemeral = true
		w.click(slack.Run{Clicked: k, ActionID: a.ID, PayloadHash: a.PayloadHash}, user)
		w.settle()
	}
	run(grace)
	assert.Equal(t, 0, appr.runs, "a member runs nothing")
	run(ada)
	assert.True(t, w.told(ada, "Ran it as you: it succeeded."))
	assert.True(t, w.told(ada, "It returned:"))
	got := w.action(a.ID)
	assert.Equal(t, model.FoundryAgentActionSucceeded, got.Status)
	require.NotNil(t, got.DecidedBy)
	assert.Equal(t, adaID, *got.DecidedBy)
	shows := w.cardShows(cardTS)
	assert.Contains(t, shows, "Run by \\u003c@U0ADA\\u003e: succeeded.")
	assert.NotContains(t, shows, slack.ActionReview)
	next := w.continued(a)
	require.NotNil(t, next, "the bot goes on in the thread")
	assert.Equal(t, alan, next.SlackUserID, "the continuation is the asker's reply")
	assert.Equal(t, ts(1), next.ThreadTS)
	w.until("the continuation's first message", func() bool { return w.continued(a).MessageTS != nil })
	continuation, err := w.db.GetFoundryAgentTurn(w.ctx, w.ws, next.TurnID)
	require.NoError(t, err)
	assert.Equal(t, model.AgentBot{CredentialID: botCred}, continuation.Principal)

	// Run again: it ran once, and says by whom.
	run(ada)
	assert.Equal(t, 1, appr.runs)
	assert.True(t, w.told(ada, "It was already run by \\u003c@U0ADA\\u003e."))
}

// action is the workspace's action id as the store has it.
func (w *world) action(id string) *model.FoundryAgentAction {
	w.t.Helper()
	a, err := w.db.GetFoundryAgentAction(w.ctx, w.ws, id)
	require.NoError(w.t, err)
	return a
}

func TestProposalsDismissedExpiredAndRefused(t *testing.T) {
	w, ppl, appr := newBotWorld(t)
	actions, cards := w.proposed("create_load", time.Now().Add(time.Hour), time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	dismissed, expired, other := actions[0], actions[1], actions[2]

	// Dismiss: an owner's or admin's no.
	dismiss := func(user string) {
		w.click(slack.Dismiss{Clicked: w.clicked(user, general, ts(1), cards[0]), ActionID: dismissed.ID}, user)
		w.settle()
	}
	dismiss(grace)
	assert.True(t, w.told(grace, "Only an owner or admin"))
	assert.Equal(t, model.FoundryAgentActionPrepared, w.action(dismissed.ID).Status)
	dismiss(ada)
	assert.Equal(t, model.FoundryAgentActionDismissed, w.action(dismissed.ID).Status)
	assert.Contains(t, w.cardShows(cards[0]), "Dismissed by \\u003c@U0ADA\\u003e.")
	updates := len(w.slack.of("chat.update"))
	dismiss(ada)
	assert.Len(t, w.slack.of("chat.update"), updates+1, "dismissed twice, its card drawn again")
	assert.Contains(t, w.cardShows(cards[0]), "Dismissed by \\u003c@U0ADA\\u003e.")

	// Expired: said so, its card too.
	w.click(slack.Review{Clicked: w.clicked(ada, general, ts(1), cards[1]), ActionID: expired.ID, PayloadHash: expired.PayloadHash}, ada)
	w.settle()
	assert.True(t, w.told(ada, "This proposal expired: ask the bot again."))
	assert.Contains(t, w.cardShows(cards[1]), "Expired")

	review := func(hash string) {
		w.click(slack.Review{Clicked: w.clicked(ada, general, ts(1), cards[2]), ActionID: other.ID, PayloadHash: hash}, ada)
		w.settle()
	}
	review(strings.Repeat("0", 64))
	assert.True(t, w.told(ada, "This card isn't the proposal's"))
	for err, says := range map[error]string{
		model.FoundryAgentRefusal{Reason: model.FoundryAgentChanged}:    "It changed since you reviewed it",
		model.FoundryAgentRefusal{Reason: model.FoundryAgentThreadBusy}: "still answering in this thread",
		status.Error(codes.FailedPrecondition, "you may not load this"): "You may not load this.",
		store.ErrNotFound: "This proposal is gone.",
	} {
		appr.set(err)
		w.click(slack.Run{Clicked: w.clicked(ada, general, ts(1), ts(88)), ActionID: other.ID, PayloadHash: other.PayloadHash}, ada)
		w.settle()
		assert.True(t, w.told(ada, says), says)
	}
	// Anything else is tried again, then given up with a word.
	appr.set(errors.New("the store went away"))
	in := w.click(slack.Review{Clicked: w.clicked(ada, general, ts(1), cards[2]), ActionID: other.ID, PayloadHash: other.PayloadHash}, ada)
	w.settle()
	assert.Equal(t, "failed", w.row(in).status)
	assert.True(t, w.told(ada, "Foundry couldn't do that just now"))
	appr.set(nil)

	// Who may approve can't be read: tried again.
	ppl.mu.Lock()
	ppl.err = errors.New("hydrate failed")
	ppl.mu.Unlock()
	// (Clicked in a direct message, which asks Slack nothing first.)
	in = w.click(slack.Review{Clicked: w.clicked(ada, dmAda, "", ts(99)), ActionID: other.ID, PayloadHash: other.PayloadHash}, ada)
	require.Equal(t, 1, w.handle())
	assert.Equal(t, "queued", w.row(in).status)
	assert.Equal(t, 1, w.row(in).failures)
	ppl.mu.Lock()
	ppl.err = nil
	ppl.mu.Unlock()
	w.settle()
	assert.Equal(t, model.FoundryAgentActionPrepared, w.action(other.ID).Status)
}

// A proposal's clicks need bot mode, a host that approves, the proposal,
// and a channel no other organisation shares.
func TestProposalsWhereNoOneApproves(t *testing.T) {
	t.Run("no approvals on this host", func(t *testing.T) {
		w, _, _ := newBotWorld(t, func(o *Options) { o.People = nil })
		w.click(slack.Review{Clicked: w.clicked(ada, general, ts(1), ts(2)), ActionID: id.New(), PayloadHash: strings.Repeat("a", 64)}, ada)
		w.settle()
		assert.True(t, w.told(ada, "can't be approved on this Foundry"))
	})
	t.Run("a proposal gone", func(t *testing.T) {
		w, _, _ := newBotWorld(t)
		w.click(slack.Dismiss{Clicked: w.clicked(ada, general, ts(1), ts(2)), ActionID: id.New()}, ada)
		w.click(slack.Review{Clicked: w.clicked(ada, general, ts(1), ts(3)), ActionID: id.New(), PayloadHash: strings.Repeat("a", 64)}, ada)
		w.settle()
		assert.True(t, w.told(ada, "This action is gone."), "a dismissal of no action is a card's")
		assert.True(t, w.told(ada, "This proposal is gone."))
	})
	t.Run("a channel shared with another organisation", func(t *testing.T) {
		w, _, appr := newBotWorld(t)
		actions, cards := w.proposed("create_load", time.Now().Add(time.Hour))
		w.slack.mu.Lock()
		w.slack.shared[general] = true
		w.slack.mu.Unlock()
		w.click(slack.Run{Clicked: w.clicked(ada, general, ts(1), cards[0]), ActionID: actions[0].ID, PayloadHash: actions[0].PayloadHash}, ada)
		w.settle()
		assert.True(t, w.told(ada, "doesn't act in channels shared with other organisations"))
		assert.Equal(t, 0, appr.runs)
	})
	t.Run("a workflow's steps", func(t *testing.T) {
		w, _, appr := newBotWorld(t)
		appr.display["create_workflow"] = agent.CreateAutomationDisplay{Name: "Weekly renewals"}
		appr.spec = strings.Repeat("5", 64)
		actions, cards := w.proposed("create_workflow", time.Now().Add(time.Hour))
		w.click(slack.Review{Clicked: w.clicked(ada, general, ts(1), cards[0]), ActionID: actions[0].ID, PayloadHash: actions[0].PayloadHash}, ada)
		w.settle()
		assert.True(t, w.told(ada, "can’t run from Slack"))
		for _, c := range w.slack.of("chat.postEphemeral") {
			assert.NotContains(t, c.text(), slack.ActionRun, "no Run for steps Slack can't show")
		}
		// A Run without the steps reviewed runs nothing.
		w.click(slack.Run{Clicked: w.clicked(ada, general, ts(1), ts(9)), ActionID: actions[0].ID, PayloadHash: actions[0].PayloadHash}, ada)
		w.settle()
		assert.True(t, w.told(ada, "It changed since you reviewed it"))
		assert.Equal(t, 0, appr.runs)
	})
	t.Run("per person", func(t *testing.T) {
		w := newWorld(t)
		w.click(slack.Dismiss{Clicked: w.clicked(ada, general, ts(1), ts(2)), ActionID: id.New()}, ada)
		w.settle()
		assert.True(t, w.told(ada, "This action is gone."))
	})
}

// A bot's turn is stopped by whoever asked, or by a linked owner or admin;
// it then says only that it stopped.
func TestBotStopByAskerOrAdmin(t *testing.T) {
	for name, viaSlack := range map[string]bool{"stop button": false, "Slack's stop": true} {
		t.Run(name, func(t *testing.T) {
			w, _, _ := newBotWorld(t)
			w.botMessage(alan, general, ts(1), ts(1), "<@U0BOT> count everything")
			w.settle()
			turn := w.only()
			w.claimTurn(turn.ID)
			stop := func(user string) {
				if viaSlack {
					w.queueEvent(slack.SessionStopped{At: slack.Conversation{TeamID: team, ChannelID: general, ThreadTS: ts(1)}, UserID: user}, user, "")
				} else {
					w.click(slack.Stop{Clicked: w.clicked(user, general, ts(1), ts(2)), TurnID: turn.ID}, user)
				}
				w.settle()
			}
			turnStatus := func() model.FoundryAgentTurnStatus {
				got, err := w.db.GetFoundryAgentTurn(w.ctx, w.ws, turn.ID)
				require.NoError(t, err)
				return got.Status
			}
			stop(grace)
			assert.True(t, w.told(grace, "Only the person who asked, or an owner or admin of the Foundry workspace, can stop"))
			assert.Equal(t, model.FoundryAgentTurnRunning, turnStatus())
			stop(ada)
			assert.Equal(t, model.FoundryAgentTurnCancelled, turnStatus())
			w.until("the reply", w.replyDone(turn.ID))
			updates := w.slack.of("chat.update")
			final := updates[len(updates)-1].text()
			assert.Contains(t, final, "Stopped.")
			assert.NotContains(t, final, "Stopped by", "an owner may have stopped it, not its asker")
		})
	}
	t.Run("its asker", func(t *testing.T) {
		w, _, _ := newBotWorld(t)
		w.botMessage(alan, general, ts(1), ts(1), "<@U0BOT> count everything")
		w.settle()
		turn := w.only()
		w.claimTurn(turn.ID)
		w.click(slack.Stop{Clicked: w.clicked(alan, general, ts(1), ts(2)), TurnID: turn.ID}, alan)
		w.settle()
		got, err := w.db.GetFoundryAgentTurn(w.ctx, w.ws, turn.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryAgentTurnCancelled, got.Status, "no link needed")
	})
}

// flakyStore fails the calls named in failing, and passes the others on.
type flakyStore struct {
	Store
	mu      sync.Mutex
	failing map[string]bool
}

func (s *flakyStore) set(method string, fails bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[method] = fails
}

func (s *flakyStore) fails(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing[method] {
		return errBroken
	}
	return nil
}

func (s *flakyStore) CountFoundrySlackUserMessages(ctx context.Context, installID, user, intakeID string, window time.Duration) (int, error) {
	if err := s.fails("CountFoundrySlackUserMessages"); err != nil {
		return 0, err
	}
	return s.Store.CountFoundrySlackUserMessages(ctx, installID, user, intakeID, window)
}

func (s *flakyStore) AddFoundrySlackThreadPerson(ctx context.Context, threadID, user string) error {
	if err := s.fails("AddFoundrySlackThreadPerson"); err != nil {
		return err
	}
	return s.Store.AddFoundrySlackThreadPerson(ctx, threadID, user)
}

func (s *flakyStore) GetFoundrySlackLink(ctx context.Context, installID, user string) (*model.SlackLink, error) {
	if err := s.fails("GetFoundrySlackLink"); err != nil {
		return nil, err
	}
	return s.Store.GetFoundrySlackLink(ctx, installID, user)
}

func (s *flakyStore) DismissFoundryAgentProposal(ctx context.Context, ws, id, decider string) (*model.FoundryAgentAction, error) {
	if err := s.fails("DismissFoundryAgentProposal"); err != nil {
		return nil, err
	}
	return s.Store.DismissFoundryAgentProposal(ctx, ws, id, decider)
}

func (s *flakyStore) ListFoundrySlackLinks(ctx context.Context, installID string) ([]*model.SlackLink, error) {
	if err := s.fails("ListFoundrySlackLinks"); err != nil {
		return nil, err
	}
	return s.Store.ListFoundrySlackLinks(ctx, installID)
}

func (s *flakyStore) GetFoundryAgentAction(ctx context.Context, ws, id string) (*model.FoundryAgentAction, error) {
	if err := s.fails("GetFoundryAgentAction"); err != nil {
		return nil, err
	}
	return s.Store.GetFoundryAgentAction(ctx, ws, id)
}

func (s *flakyStore) GetFoundrySlackReply(ctx context.Context, turnID string) (*model.SlackReply, error) {
	if err := s.fails("GetFoundrySlackReply"); err != nil {
		return nil, err
	}
	return s.Store.GetFoundrySlackReply(ctx, turnID)
}

func (s *flakyStore) ReleaseFoundrySlackHold(ctx context.Context, turnID string) error {
	if err := s.fails("ReleaseFoundrySlackHold"); err != nil {
		return err
	}
	return s.Store.ReleaseFoundrySlackHold(ctx, turnID)
}

func (s *flakyStore) SetFoundrySlackIntakeInput(ctx context.Context, id, owner, input string) error {
	if err := s.fails("SetFoundrySlackIntakeInput"); err != nil {
		return err
	}
	return s.Store.SetFoundrySlackIntakeInput(ctx, id, owner, input)
}

// A bot-mode row the store fails is tried again, and then does what it
// would have; nothing it did twice shows twice.
func TestBotModeStoreFailuresAreRetried(t *testing.T) {
	// retried handles row once with method failing: it is queued again,
	// one failure counted; then everything settles with it healed.
	retried := func(t *testing.T, w *world, flaky *flakyStore, method, row string) {
		t.Helper()
		flaky.set(method, true)
		w.handle()
		assert.Equal(t, 1, w.row(row).failures, method)
		assert.Equal(t, "queued", w.row(row).status, method)
		flaky.set(method, false)
		w.settle()
		assert.Equal(t, "done", w.row(row).status, method)
	}
	newFlaky := func(t *testing.T) (*world, *flakyStore) {
		w, _, _ := newBotWorld(t)
		flaky := &flakyStore{Store: w.db, failing: map[string]bool{}}
		w.bot.db = flaky
		return w, flaky
	}
	t.Run("a message's count and its thread's people", func(t *testing.T) {
		for _, method := range []string{"CountFoundrySlackUserMessages", "AddFoundrySlackThreadPerson"} {
			w, flaky := newFlaky(t)
			retried(t, w, flaky, method, w.botMessage(alan, general, ts(1), ts(1), "<@U0BOT> hi"))
			assert.Len(t, w.turns(), 1, "%s: the same turn again", method)
			mapping, err := w.db.GetFoundrySlackThread(w.ctx, w.install.ID, general, ts(1), "")
			require.NoError(t, err)
			spoke, err := w.db.IsFoundrySlackThreadPerson(w.ctx, mapping.ID, alan)
			require.NoError(t, err)
			assert.True(t, spoke, method)
		}
	})
	t.Run("a dismissal", func(t *testing.T) {
		for _, method := range []string{"DismissFoundryAgentProposal", "GetFoundryAgentAction", "GetFoundrySlackReply"} {
			w, flaky := newFlaky(t)
			actions, cards := w.proposed("create_load", time.Now().Add(time.Hour))
			retried(t, w, flaky, method, w.click(slack.Dismiss{Clicked: w.clicked(ada, general, ts(1), cards[0]), ActionID: actions[0].ID}, ada))
			assert.Equal(t, model.FoundryAgentActionDismissed, w.action(actions[0].ID).Status, method)
		}
	})
	t.Run("a run's word kept, and who ran it", func(t *testing.T) {
		for _, method := range []string{"SetFoundrySlackIntakeInput", "ListFoundrySlackLinks"} {
			w, flaky := newFlaky(t)
			actions, cards := w.proposed("create_load", time.Now().Add(time.Hour))
			click := func() string {
				return w.click(slack.Run{Clicked: w.clicked(ada, general, ts(1), cards[0]), ActionID: actions[0].ID, PayloadHash: actions[0].PayloadHash}, ada)
			}
			if method == "ListFoundrySlackLinks" {
				click()
				w.settle()
			}
			retried(t, w, flaky, method, click())
			assert.True(t, w.told(ada, "It was already run"), method)
		}
	})
	t.Run("a continuation not handed over is followed once its hold runs out", func(t *testing.T) {
		w, flaky := newFlaky(t)
		actions, cards := w.proposed("create_load", time.Now().Add(time.Hour))
		flaky.set("ReleaseFoundrySlackHold", true)
		w.click(slack.Run{Clicked: w.clicked(ada, general, ts(1), cards[0]), ActionID: actions[0].ID, PayloadHash: actions[0].PayloadHash}, ada)
		w.settle()
		r := w.continued(actions[0])
		require.NotNil(t, r, "held")
		assert.Nil(t, r.MessageTS)
		assert.True(t, r.NextPollAt.After(time.Now().Add(30*time.Minute)), "for its hold")
	})
}

// A retry of a Run that ran says again what the first attempt kept, and
// runs nothing.
func TestARunsRetrySaysWhatItKept(t *testing.T) {
	w, _, appr := newBotWorld(t)
	actions, cards := w.proposed("create_load", time.Now().Add(time.Hour))
	w.slack.queue("chat.postEphemeral", 429, "1", `{"ok":false,"error":"ratelimited"}`)
	in := w.click(slack.Run{Clicked: w.clicked(ada, general, ts(1), cards[0]), ActionID: actions[0].ID, PayloadHash: actions[0].PayloadHash}, ada)
	require.Equal(t, 1, w.handle())
	assert.Equal(t, "queued", w.row(in).status, "deferred by Slack")
	w.settle()
	assert.Equal(t, 1, appr.runs)
	assert.True(t, w.told(ada, "Ran it as you: it succeeded."))
	assert.False(t, w.told(ada, "It was already run"))
}

func TestRanAs(t *testing.T) {
	long := `{"x":"` + strings.Repeat("é", 2000) + `"}`
	for name, c := range map[string]struct {
		end  model.FoundryAgentActionEnd
		want string
	}{
		"succeeded": {model.FoundryAgentActionEnd{Status: model.FoundryAgentActionSucceeded, Result: json.RawMessage(`{"id":"l1"}`)},
			"Ran it as you: it succeeded.\nIt returned: {\"id\":\"l1\"}"},
		"returned nothing": {model.FoundryAgentActionEnd{Status: model.FoundryAgentActionSucceeded, Result: json.RawMessage(`{}`)},
			"Ran it as you: it succeeded."},
		"saved, not approved": {model.FoundryAgentActionEnd{Status: model.FoundryAgentActionSucceeded, Error: "it was saved, but not approved",
			Result: json.RawMessage(`null`)}, "Ran it as you: it was saved, but not approved"},
		"failed": {model.FoundryAgentActionEnd{Status: model.FoundryAgentActionFailed, Error: " no room "}, "Ran it as you: it failed: no room"},
		"unknown": {model.FoundryAgentActionEnd{Status: model.FoundryAgentActionUnknown},
			"Ran it as you, but Foundry lost its answer, so it may or may not have taken effect: check in Foundry before running it again."},
	} {
		assert.Equal(t, c.want, ranAs(c.end), name)
	}
	got := ranAs(model.FoundryAgentActionEnd{Status: model.FoundryAgentActionSucceeded, Result: json.RawMessage(long)})
	assert.Equal(t, proposalMax, len([]rune(strings.TrimPrefix(got, "Ran it as you: it succeeded.\nIt returned: "))))
	assert.True(t, strings.HasSuffix(got, "…"))
}

// already says how a proposal no longer waiting stands, and by whom.
func TestAlready(t *testing.T) {
	w := newWorld(t)
	now := time.Now()
	decided := now.Add(-time.Second)
	for name, c := range map[string]struct {
		a    *model.FoundryAgentAction
		by   string
		want string
	}{
		"ran":       {&model.FoundryAgentAction{Status: model.FoundryAgentActionSucceeded}, ada, "It was already run by <@U0ADA>."},
		"dismissed": {&model.FoundryAgentAction{Status: model.FoundryAgentActionDismissed}, "", "It was already dismissed."},
		"expired":   {&model.FoundryAgentAction{Status: model.FoundryAgentActionExpired}, ada, "This proposal expired: ask the bot again."},
		"running":   {&model.FoundryAgentAction{Status: model.FoundryAgentActionExecuting, DecidedAt: &decided}, ada, "It's being run by <@U0ADA> now."},
	} {
		assert.Equal(t, c.want, w.bot.already(c.a, c.by).Text.String(), name)
	}
}

// Where a proposal's card is, and what redrawing it does when it can't.
func TestProposalCardsWhereTheyAre(t *testing.T) {
	w, _, _ := newBotWorld(t)
	actions, cards := w.proposed("create_load", time.Now().Add(time.Hour))
	a := actions[0]
	c := claim{install: w.install, bot: slack.Bot{InstallID: w.install.ID, Token: "xoxb-token"}}
	k := w.clicked(ada, dmAda, "", ts(70))

	channel, thread, asker := w.bot.proposalThread(w.ctx, a, k)
	assert.Equal(t, []string{general, ts(1), alan}, []string{channel, thread, asker})
	other := *a
	other.TurnID = id.New()
	channel, thread, asker = w.bot.proposalThread(w.ctx, &other, k)
	assert.Equal(t, []string{dmAda, ts(70), ada}, []string{channel, thread, asker}, "no reply: where it was clicked")
	flaky := &flakyStore{Store: w.db, failing: map[string]bool{"GetFoundrySlackReply": true}}
	w.bot.db = flaky
	channel, _, _ = w.bot.proposalThread(w.ctx, a, k)
	assert.Equal(t, dmAda, channel, "a reply that can't be read: where it was clicked")
	assert.ErrorIs(t, w.bot.redrawProposal(w.ctx, c, a, ada, k), errBroken)
	flaky.set("GetFoundrySlackReply", false)

	updates := func() int { return len(w.slack.of("chat.update")) }
	before := updates()
	running := time.Now()
	executing := *a
	executing.Status, executing.DecidedAt = model.FoundryAgentActionExecuting, &running
	require.NoError(t, w.bot.redrawProposal(w.ctx, c, &executing, ada, k), "a run going on")
	ephemeral := k
	ephemeral.Ephemeral = true
	require.NoError(t, w.bot.redrawProposal(w.ctx, c, &other, ada, ephemeral), "nowhere to draw it")
	unknown := *a
	unknown.Operation = "set_assistant_runtime"
	require.NoError(t, w.bot.redrawProposal(w.ctx, c, &unknown, ada, k), "an operation with no proposal card")
	assert.Equal(t, before, updates())

	w.slack.queue("chat.update", 200, "", `{"ok":false,"error":"message_not_found"}`)
	require.NoError(t, w.bot.redrawProposal(w.ctx, c, a, ada, k), "a card gone")
	w.slack.queue("chat.update", 200, "", `{"ok":false,"error":"internal_error"}`)
	assert.Error(t, w.bot.redrawProposal(w.ctx, c, a, ada, k))
	assert.Equal(t, cards[0], w.slack.of("chat.update")[before].args["ts"])

	who, err := w.bot.slackUserOf(w.ctx, c, nil)
	require.NoError(t, err)
	assert.Empty(t, who)
	who, err = w.bot.slackUserOf(w.ctx, c, ptr("user_nobody"))
	require.NoError(t, err)
	assert.Empty(t, who, "no one linked them")
	who, err = w.bot.slackUserOf(w.ctx, c, ptr(graceID))
	require.NoError(t, err)
	assert.Equal(t, grace, who)
}

// admin is a linked owner or admin; it reads nothing where no one approves.
func TestAdmin(t *testing.T) {
	w, ppl, _ := newBotWorld(t)
	c := claim{install: w.install}
	for user, want := range map[string]bool{ada: true, grace: false, alan: false} {
		_, ok, err := w.bot.admin(w.ctx, c, user)
		require.NoError(t, err)
		assert.Equal(t, want, ok, user)
	}
	ppl.mu.Lock()
	ppl.roles[graceID] = ports.RoleAdmin
	ppl.mu.Unlock()
	userID, ok, err := w.bot.admin(w.ctx, c, grace)
	require.NoError(t, err)
	assert.True(t, ok, "an admin")
	assert.Equal(t, graceID, userID)
	flaky := &flakyStore{Store: w.db, failing: map[string]bool{"GetFoundrySlackLink": true}}
	w.bot.db = flaky
	_, _, err = w.bot.admin(w.ctx, c, ada)
	assert.ErrorIs(t, err, errBroken, "the link unread")
	flaky.set("GetFoundrySlackLink", false)
	ppl.mu.Lock()
	ppl.err = errBroken
	ppl.mu.Unlock()
	_, _, err = w.bot.admin(w.ctx, c, ada)
	assert.ErrorIs(t, err, errBroken, "the role unread")
	w.bot.people = nil
	_, ok, err = w.bot.admin(w.ctx, c, ada)
	assert.NoError(t, err)
	assert.False(t, ok, "no one approves here")
}

// The follower posts a proposal's card as the proposal stands when it gets
// to it: being run, it still offers Review (which then says how it stands);
// gone, or of an operation with no card, nothing.
func TestProposalCardsAsTheyStand(t *testing.T) {
	w, _, _ := newBotWorld(t)
	w.botMessage(alan, general, ts(1), ts(1), "<@U0BOT> load Sales")
	w.settle()
	turn := w.only()
	run := w.claimTurn(turn.ID)
	running := run.propose("create_load", time.Now().Add(time.Hour))
	gone := run.propose("pause_workflow", time.Now().Add(time.Hour))
	unknown := run.propose("set_assistant_runtime", time.Now().Add(time.Hour))
	run.finish(agent.StopAnswered, "")
	_, err := testDB.Exec(`UPDATE foundry_agent_actions SET status='executing', decided_at=NOW(6) WHERE id=?`, running.ID)
	require.NoError(t, err)
	_, err = testDB.Exec(`DELETE FROM foundry_agent_actions WHERE id=?`, gone.ID)
	require.NoError(t, err)
	w.until("the reply", w.replyDone(turn.ID))
	posted := w.slack.posts("Load a version")
	require.Len(t, posted, 1)
	assert.Contains(t, posted[0].text(), slack.ActionReview)
	assert.Empty(t, w.slack.posts("Pause a workflow"))
	r, err := w.db.GetFoundrySlackReply(w.ctx, turn.ID)
	require.NoError(t, err)
	assert.False(t, model.ValidSlackTS(r.Posted["a:"+unknown.ID]), "no card for it")
}

// A review drawn as an admin Foundry has no name for runs as "you"; one
// whose card can't be drawn is given up.
func TestReviewsDrawnAsTheirReviewer(t *testing.T) {
	w, ppl, appr := newBotWorld(t)
	ppl.mu.Lock()
	ppl.roles[graceID] = ports.RoleAdmin
	ppl.mu.Unlock()
	actions, cards := w.proposed("create_load", time.Now().Add(time.Hour))
	w.click(slack.Review{Clicked: w.clicked(grace, general, ts(1), cards[0]), ActionID: actions[0].ID, PayloadHash: actions[0].PayloadHash}, grace)
	w.settle()
	assert.True(t, w.told(grace, "Runs as you. Nothing runs until you click Load."))

	appr.display["create_load"] = []int{1}
	in := w.click(slack.Review{Clicked: w.clicked(grace, general, ts(1), cards[0]), ActionID: actions[0].ID, PayloadHash: actions[0].PayloadHash}, grace)
	w.settle()
	r := w.row(in)
	assert.Equal(t, "failed", r.status)
	assert.Contains(t, r.error.String, "read the create_load card")
}
