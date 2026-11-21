package slackbot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/agentquestion"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func agentQuestion(id, text string) agentquestion.Question {
	return agentquestion.Question{ID: id, Text: text, Blocking: true,
		Choice: &agentquestion.Choice{Options: []agentquestion.Option{{Label: "Europe"}, {Label: "Americas"}}}}
}

// only is the one turn the workspace has.
func (w *world) only() *model.FoundryAgentTurn {
	w.t.Helper()
	turns := w.turns()
	require.Len(w.t, turns, 1)
	return turns[0]
}

// last is the newest turn.
func (w *world) last() *model.FoundryAgentTurn {
	w.t.Helper()
	turns := w.turns()
	require.NotEmpty(w.t, turns)
	return turns[len(turns)-1]
}

// threads counts the install's Slack thread mappings.
func (w *world) threads() int {
	var n int
	require.NoError(w.t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_slack_threads WHERE install_id=?`, w.install.ID).Scan(&n))
	return n
}

// told is what the bot told user privately (an ephemeral, or a DM message)
// holding text.
func (w *world) told(user, text string) bool {
	for _, c := range append(w.slack.of("chat.postEphemeral"), w.slack.of("chat.postMessage")...) {
		if (c.method == "chat.postMessage" || c.args["user"] == user) && strings.Contains(c.text(), text) {
			return true
		}
	}
	return false
}

func TestMessageStartsTurnAndReplyFollowsIt(t *testing.T) {
	w := newWorld(t)
	in := w.message(ada, dmAda, ts(1), ts(1), "Which customers *renewed*? Ask &lt;all&gt;")
	require.Equal(t, 1, w.handle())

	turn := w.only()
	assert.Equal(t, model.FoundryAgentSurfaceSlack, turn.Surface)
	assert.Equal(t, adaID, turn.UserID)
	assert.Equal(t, "Which customers *renewed*? Ask <all>", turn.Input)
	assert.JSONEq(t, `{"time_zone":"Europe/London"}`, string(turn.PageContext))
	assert.Equal(t, 1, w.threads())
	r := w.row(in)
	assert.Equal(t, "done", r.status)
	var payload *string
	require.NoError(t, testDB.QueryRow(`SELECT payload FROM foundry_slack_intake WHERE id=?`, in).Scan(&payload))
	assert.Nil(t, payload, "a handled row keeps no payload")

	working := w.slack.posts("Working")
	require.Len(t, working, 1)
	assert.Equal(t, ts(1), working[0].args["thread_ts"])
	assert.Equal(t, "none", working[0].args["parse"])
	assert.Equal(t, false, working[0].args["unfurl_links"])
	statuses := w.slack.of("agents.sessions.setStatus")
	require.Len(t, statuses, 1)
	assert.Equal(t, "processing", statuses[0].args["status"])
	reply, err := w.db.GetFoundrySlackReply(w.ctx, turn.ID)
	require.NoError(t, err)
	require.NotNil(t, reply.MessageTS)

	// The follower shows the live turn, then its answer.
	run := w.claimTurn(turn.ID)
	run.emit(agent.ToolCalled{ToolUseID: "t1", Name: "helix_foundry_read_graph", Title: "Reading Sales", Input: json.RawMessage(`{"secret":"x"}`)})
	var following sync.WaitGroup
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	follow, err := w.bot.NextFollow(w.ctx)
	require.NoError(t, err)
	require.NotNil(t, follow)
	following.Go(func() { follow(ctx) })
	require.Eventually(t, func() bool {
		for _, c := range w.slack.of("chat.update") {
			if strings.Contains(c.text(), "Reading Sales") {
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)
	for _, c := range w.slack.of("chat.update") {
		assert.NotContains(t, c.text(), "secret", "a tool's input never shows")
		assert.Equal(t, *reply.MessageTS, c.args["ts"])
	}
	run.emit(agent.Said{Step: 1, Text: "Acme renewed <b>, see https://evil.example/x"})
	run.finish(agent.StopAnswered, "")
	following.Wait()
	assert.True(t, w.replyDone(turn.ID)())
	updates := w.slack.of("chat.update")
	final := updates[len(updates)-1].text()
	assert.Contains(t, final, "Acme renewed &lt;b&gt;")
	assert.Contains(t, final, "`https://evil.example/x`")
	assert.NotContains(t, final, slack.ActionStop)
	statuses = w.slack.of("agents.sessions.setStatus")
	assert.Equal(t, "active", statuses[len(statuses)-1].args["status"])
}

func TestMentionNamesPeopleAndFollowUpsKeepTheThread(t *testing.T) {
	w := newWorld(t)
	w.message(ada, general, ts(1), ts(1), "<@U0BOT> what did <@U0GRACE> and <@U0NOBODY|bob> sell in <#C0SALES|sales>?")
	w.settle()
	first := w.only()
	assert.Equal(t, "what did @grace and @bob sell in #sales?", first.Input)
	w.claimTurn(first.ID).finish(agent.StopAnswered, "")
	w.until("the first reply", w.replyDone(first.ID))

	w.message(ada, general, ts(1), ts(2), "<@U0BOT> and last month?")
	w.settle()
	second := w.last()
	assert.NotEqual(t, first.ID, second.ID)
	assert.Equal(t, first.ThreadID, second.ThreadID, "a reply in the Slack thread continues its Founder thread")
	assert.Equal(t, 1, w.threads())

	// Grace in the same Slack thread has a Founder thread of her own.
	w.message(grace, general, ts(1), ts(3), "<@U0BOT> and mine?")
	w.settle()
	third := w.last()
	assert.NotEqual(t, first.ThreadID, third.ThreadID)
	assert.Equal(t, graceID, third.UserID)
	assert.Equal(t, 2, w.threads())
}

func TestWhoAndWhatTheBotRefuses(t *testing.T) {
	w := newWorld(t)
	ext := slack.Conversation{TeamID: team, ChannelID: general, ThreadTS: ts(1), ExtShared: true}
	w.queueEvent(slack.Mention{At: ext, UserID: ada, TS: ts(1), Text: "hi"}, ada, model.SlackConversation(w.install.ID, general, ts(1), ada))
	w.message(guest, general, ts(2), ts(2), "hi")
	w.message("U0NOBODY", general, ts(3), ts(3), "hi")
	other := slack.Conversation{TeamID: team, ChannelID: general, ThreadTS: ts(4)}
	w.queueEvent(slack.Mention{At: other, UserID: ada, UserTeamID: "T0OTHER", TS: ts(4), Text: "hi"}, ada,
		model.SlackConversation(w.install.ID, general, ts(4), ada))
	w.message(ada, dmAda, ts(5), ts(5), "<@U0BOT>")
	w.message(ada, dmAda, ts(6), ts(6), strings.Repeat("a", maxInputChars+1))
	w.settle()
	assert.Empty(t, w.turns())
	assert.True(t, w.told(ada, "doesn't answer in channels shared with other organisations"))
	assert.True(t, w.told(guest, "answers members of Acme only"))
	assert.True(t, w.told("U0NOBODY", "answers members of Acme only"))
	assert.True(t, w.told(ada, "answers members of Acme only"), "a message from another team")
	assert.True(t, w.told(ada, "Ask me something about your Foundry data."))
	assert.True(t, w.told(ada, "too long"))
	for _, c := range w.slack.of("chat.postEphemeral") {
		assert.Equal(t, "none", c.args["parse"])
	}
}

func TestUnlinkedPersonGetsALinkPrompt(t *testing.T) {
	w := newWorld(t)
	w.message(alan, "D0ALAN", ts(1), ts(1), "hello")
	w.message(alan, general, ts(2), ts(2), "<@U0BOT> hello")
	w.settle()
	assert.Empty(t, w.turns())

	dm := w.slack.posts("Link your Slack account")
	require.Len(t, dm, 1)
	assert.Equal(t, "D0ALAN", dm[0].args["channel"])
	ephemeral := w.slack.of("chat.postEphemeral")
	require.Len(t, ephemeral, 1)
	assert.Equal(t, alan, ephemeral[0].args["user"])
	link := ""
	for _, block := range ephemeral[0].args["blocks"].([]any) {
		if b := block.(map[string]any); b["type"] == "actions" {
			link = b["elements"].([]any)[0].(map[string]any)["url"].(string)
		}
	}
	prefix := siteURL + "/slack/link?workspace=" + w.ws + "#t=hx_sl_"
	require.True(t, strings.HasPrefix(link, prefix), link)
	token := link[strings.Index(link, "#t=")+3:]
	live, err := w.db.GetFoundrySlackLinkToken(w.ctx, hashOf(token))
	require.NoError(t, err, "the newest prompt's token is live")
	assert.Equal(t, alan, live.SlackUserID)
	assert.Contains(t, w.slack.posts("Link your Slack account")[0].text(), "verbatim")
}

func TestMemberRemovedIsUnlinked(t *testing.T) {
	w := newWorld(t)
	w.sessions.refuse(adaID, status.Error(codes.PermissionDenied, "not a member"))
	w.message(ada, dmAda, ts(1), ts(1), "hello")
	w.settle()
	assert.Empty(t, w.turns())
	_, err := w.db.GetFoundrySlackLink(w.ctx, w.install.ID, ada)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.True(t, w.told(ada, "your link was removed"))
}

func TestGrantRefusalKeepsTheLink(t *testing.T) {
	w := newWorld(t)
	w.founder.sendErr = status.Error(codes.PermissionDenied, "your access doesn't reach this ontology")
	w.message(ada, dmAda, ts(1), ts(1), "hello")
	w.settle()
	_, err := w.db.GetFoundrySlackLink(w.ctx, w.install.ID, ada)
	assert.NoError(t, err)
	assert.True(t, w.told(ada, "Your access doesn't reach this ontology."))
}

func TestWorkspaceGoneRevokesTheInstall(t *testing.T) {
	w := newWorld(t)
	w.sessions.refuse(adaID, status.Error(codes.NotFound, "workspace not found"))
	w.message(ada, dmAda, ts(1), ts(1), "hello")
	w.handle()
	var open int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_slack_intake`).Scan(&open))
	assert.Zero(t, open, "revoking the install deleted its open rows, the one handled among them")
	got, err := w.db.GetFoundrySlackInstallByID(w.ctx, w.install.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.RevokedAt)
	assert.Nil(t, got.UninstalledAt, "the sweep tells Slack")

	// Rows of a revoked install end without a word to Slack.
	calls := len(w.slack.calls)
	later := w.message(grace, general, ts(2), ts(2), "hello")
	assert.Equal(t, 1, w.handle())
	assert.Equal(t, "done", w.row(later).status)
	assert.Len(t, w.slack.calls, calls)
}

func TestAPIRefusalsAndFailures(t *testing.T) {
	w := newWorld(t, func(o *Options) { o.Cfg.MaxAttempts = 2 })
	w.founder.sendErr = status.Error(codes.ResourceExhausted, "you've sent 30 messages in the last hour")
	first := w.message(ada, dmAda, ts(1), ts(1), "hello")
	w.settle()
	assert.Equal(t, "done", w.row(first).status)
	assert.True(t, w.told(ada, "You've sent 30 messages in the last hour."))

	w.founder.sendErr = status.Error(codes.Unavailable, "the database is down")
	second := w.message(ada, dmAda, ts(2), ts(2), "hello")
	w.handle()
	r := w.row(second)
	assert.Equal(t, "queued", r.status, "a fault is tried again")
	assert.Equal(t, 1, r.attempts)
	w.settle()
	r = w.row(second)
	assert.Equal(t, "failed", r.status)
	assert.Contains(t, r.error.String, "gave up after")
	assert.True(t, w.told(ada, "Founder couldn't take this message; try again."))
}

func TestInputIsKeptForRetries(t *testing.T) {
	w := newWorld(t)
	w.founder.sendErr = status.Error(codes.Unavailable, "down")
	in := w.message(ada, dmAda, ts(1), ts(1), "about <@U0GRACE>")
	w.handle()
	var input string
	require.NoError(t, testDB.QueryRow(`SELECT JSON_UNQUOTE(JSON_EXTRACT(payload, '$.input')) FROM foundry_slack_intake WHERE id=?`, in).Scan(&input))
	assert.Equal(t, "about @grace", input)
	// Grace renames herself: the retry sends what the first attempt read.
	w.slack.mu.Lock()
	w.slack.users[grace]["profile"] = map[string]any{"display_name": "gracie"}
	w.slack.mu.Unlock()
	w.bot.users = map[[2]string]cachedUser{}
	w.founder.sendErr = nil
	w.until("the retry", func() bool { return len(w.turns()) == 1 })
	assert.Equal(t, "about @grace", w.only().Input)
}

func TestBusyThreadWaitsAndRunsInOrder(t *testing.T) {
	w := newWorld(t)
	m1 := w.message(ada, dmAda, ts(10), ts(10), "first")
	m2 := w.message(ada, dmAda, ts(10), ts(11), "second")
	m3 := w.message(ada, dmAda, ts(10), ts(12), "third")

	// Two workers claim at once: one Slack thread is one conversation, so
	// only its oldest message runs.
	var claims atomic.Int32
	var runs []func(context.Context)
	var mu sync.Mutex
	var claiming sync.WaitGroup
	for range 2 {
		claiming.Go(func() {
			run, err := w.bot.NextIntake(w.ctx)
			assert.NoError(t, err)
			if run != nil {
				claims.Add(1)
				mu.Lock()
				runs = append(runs, run)
				mu.Unlock()
			}
		})
	}
	claiming.Wait()
	require.EqualValues(t, 1, claims.Load())
	runs[0](w.ctx)
	t1 := w.only()
	assert.Equal(t, "first", t1.Input)
	assert.Equal(t, "done", w.row(m1).status)

	// The second waits on the first's turn; the third waits behind it.
	assert.Equal(t, 1, w.handle())
	r2 := w.row(m2)
	assert.Equal(t, "queued", r2.status)
	assert.Equal(t, t1.ID, r2.waits.String)
	assert.Equal(t, 0, w.row(m3).attempts)
	assert.Equal(t, 0, w.handle())

	// The first turn ends: its follower wakes the second.
	w.claimTurn(t1.ID).finish(agent.StopAnswered, "")
	w.until("the second message's turn", func() bool { return len(w.turns()) == 2 })
	w.until("the third message waiting", func() bool { return w.row(m3).waits.Valid })
	turns := w.turns()
	require.Len(t, turns, 2)
	assert.Equal(t, "second", turns[1].Input)
	assert.Equal(t, t1.ThreadID, turns[1].ThreadID)
	assert.Equal(t, turns[1].ID, w.row(m3).waits.String)

	w.claimTurn(turns[1].ID).finish(agent.StopAnswered, "")
	w.until("the third message's turn", func() bool { return len(w.turns()) == 3 })
	third := w.last()
	assert.Equal(t, "third", third.Input)
	assert.Equal(t, t1.ThreadID, third.ThreadID)
	assert.Equal(t, 1, w.threads(), "one Slack thread, one Founder thread")
}

func TestBusyPastItsWaitIsTurnedAway(t *testing.T) {
	w := newWorld(t, func(o *Options) { o.Cfg.BusyWait = 200 * time.Millisecond })
	w.message(ada, dmAda, ts(1), ts(1), "first")
	w.handle()
	second := w.message(ada, dmAda, ts(1), ts(2), "second")
	time.Sleep(250 * time.Millisecond)
	w.settle()
	assert.Equal(t, "done", w.row(second).status)
	assert.True(t, w.told(ada, "Founder is still answering in this thread"))
	assert.Len(t, w.turns(), 1)
}

// question makes an Analyst question of a Sales ontology, live.
func (w *world) question() *model.FoundryQuestion {
	w.t.Helper()
	ontology := &model.FoundryOntology{ID: id.New(), WorkspaceID: w.ws, Name: "Sales", ProjectID: "proj", TenantID: id.New(),
		ClientToken: id.New(), CreatedByUserID: ptr(adaID)}
	require.NoError(w.t, w.db.CreateFoundryOntology(w.ctx, ontology))
	q := &model.FoundryQuestion{ID: id.New(), WorkspaceID: w.ws, OntologyID: ontology.ID, LoadID: "load_1", VersionID: "ver_1", Question: "how many?",
		MaxUSDMicros: 1000}
	q.ThreadID = q.ID
	require.NoError(w.t, w.db.CreateFoundryQuestion(w.ctx, q, model.FoundryQuestionLimits{Since: time.Now().Add(-time.Hour), CapMicros: 1e9, PerHour: 100}))
	return q
}

func TestAnalystAnswerIsPostedOnceItEnds(t *testing.T) {
	w := newWorld(t)
	w.message(ada, dmAda, ts(1), ts(1), "how many invoices?")
	w.handle()
	turn := w.only()
	q := w.question()
	run := w.claimTurn(turn.ID)
	run.emit(agent.ToolReturned{ToolUseID: "t1", Name: "helix_foundry_ask", Question: &agent.QuestionRef{OntologyID: q.OntologyID, ID: q.ID}})
	w.follow(300 * time.Millisecond)
	assert.Empty(t, w.slack.posts("Answer from"), "a live question's answer waits")

	result := `{"status":"answered","answer":{"blocks":[{"para":"There are 12 invoices [c1]."}],"visuals":[]},` +
		`"citations":[{"marker":"c1","evidence_id":"e1","target":{"result":{"evidence_id":"e1"}}}],"queries":[],` +
		`"usage":{"input":1,"cache_read":0,"cache_write":0,"output":1,"usd":0.01}}`
	_, err := testDB.Exec(`UPDATE foundry_questions SET status='answered', result=?, finished_at=NOW(6) WHERE id=?`, result, q.ID)
	require.NoError(t, err)
	run.finish(agent.StopAnswered, "")
	w.until("the reply", w.replyDone(turn.ID))
	answers := w.slack.posts("Answer from")
	require.Len(t, answers, 1)
	assert.Contains(t, answers[0].text(), "There are 12 invoices")
	assert.Contains(t, answers[0].text(), siteURL+"/ask/"+q.OntologyID)
	r, err := w.db.GetFoundrySlackReply(w.ctx, turn.ID)
	require.NoError(t, err)
	assert.True(t, model.ValidSlackTS(r.Posted["q:"+q.ID]))
}

func TestCardsRunDismissAndExpire(t *testing.T) {
	w := newWorld(t)
	w.message(ada, dmAda, ts(1), ts(1), "load Sales")
	w.settle()
	turn := w.only()
	run := w.claimTurn(turn.ID)
	runCard := run.card(time.Now().Add(time.Hour))
	dismissCard := run.card(time.Now().Add(time.Hour))
	expiredCard := run.card(time.Now().Add(-time.Minute))
	run.finish(agent.StopAnswered, "")
	w.until("the reply", w.replyDone(turn.ID))
	cards := w.slack.posts("Load v3 of")
	require.Len(t, cards, 3, "each card once")
	statuses := w.slack.of("agents.sessions.setStatus")
	assert.Equal(t, "suspended", statuses[len(statuses)-1].args["status"], "cards wait on their person")
	// lastOf is the newest update of a's card.
	lastOf := func(a *model.FoundryAgentAction) string {
		r, err := w.db.GetFoundrySlackReply(w.ctx, turn.ID)
		require.NoError(t, err)
		out := ""
		for _, c := range w.slack.of("chat.update") {
			if c.args["ts"] == r.Posted["a:"+a.ID] {
				out = c.text()
			}
		}
		return out
	}
	cardTS := func(a *model.FoundryAgentAction) string {
		r, err := w.db.GetFoundrySlackReply(w.ctx, turn.ID)
		require.NoError(t, err)
		return r.Posted["a:"+a.ID]
	}

	// Only its person acts on a card.
	w.click(slack.Approve{Clicked: w.clicked(grace, dmAda, ts(1), cardTS(runCard)), ActionID: runCard.ID, PayloadHash: runCard.PayloadHash}, grace)
	w.settle()
	assert.True(t, w.told(grace, "Only the person this was prepared for"))
	w.click(slack.Approve{Clicked: w.clicked(alan, dmAda, ts(1), cardTS(runCard)), ActionID: runCard.ID, PayloadHash: runCard.PayloadHash}, alan)
	w.settle()
	assert.True(t, w.told(alan, "Only the person this was prepared for"))

	// Expired: Prepare again, then it can run.
	w.click(slack.Approve{Clicked: w.clicked(ada, dmAda, ts(1), cardTS(expiredCard)), ActionID: expiredCard.ID, PayloadHash: expiredCard.PayloadHash}, ada)
	w.settle()
	assert.Contains(t, lastOf(expiredCard), slack.ActionRefresh)
	w.click(slack.Refresh{Clicked: w.clicked(ada, dmAda, ts(1), cardTS(expiredCard)), ActionID: expiredCard.ID}, ada)
	w.settle()
	assert.Contains(t, lastOf(expiredCard), slack.ActionApprove)

	// Dismiss.
	w.click(slack.Dismiss{Clicked: w.clicked(ada, dmAda, ts(1), cardTS(dismissCard)), ActionID: dismissCard.ID}, ada)
	w.settle()
	assert.Contains(t, lastOf(dismissCard), "Dismissed")
	assert.Contains(t, lastOf(dismissCard), "\\u003c@U0ADA\\u003e")

	// Run: the card says so, and the turn the agent goes on in is followed.
	w.click(slack.Approve{Clicked: w.clicked(ada, dmAda, ts(1), cardTS(runCard)), ActionID: runCard.ID, PayloadHash: runCard.PayloadHash}, ada)
	w.settle()
	assert.Contains(t, lastOf(runCard), "succeeded")
	next := w.last()
	assert.Equal(t, model.FoundryAgentTurnActionResult, next.Kind)
	assert.Equal(t, model.FoundryAgentSurfaceSlack, next.Surface, "a continuation is read where its action's turn was")
	r, err := w.db.GetFoundrySlackReply(w.ctx, next.ID)
	require.NoError(t, err)
	assert.Equal(t, ts(1), r.ThreadTS)
	assert.NotNil(t, r.MessageTS)

	// A card clicked again redraws as it stands.
	w.click(slack.Dismiss{Clicked: w.clicked(ada, dmAda, ts(1), cardTS(runCard)), ActionID: runCard.ID}, ada)
	w.settle()
	assert.Contains(t, lastOf(runCard), "succeeded")
	assert.Contains(t, lastOf(runCard), "Ran")
}

func TestQuestionsAreAnsweredByReplying(t *testing.T) {
	w := newWorld(t)
	w.message(ada, dmAda, ts(1), ts(1), "set up my ontology")
	w.settle()
	turn := w.only()
	run := w.claimTurn(turn.ID)
	run.ask("Which region?")
	run.finish(agent.StopAsked, "")
	w.until("the reply", w.replyDone(turn.ID))
	asked := w.slack.posts("Which region?")
	require.Len(t, asked, 1)
	assert.Contains(t, asked[0].text(), "1. Europe")
	statuses := w.slack.of("agents.sessions.setStatus")
	assert.Equal(t, "suspended", statuses[len(statuses)-1].args["status"])

	w.message(ada, dmAda, ts(1), ts(2), "2")
	w.settle()
	answer := w.last()
	assert.Equal(t, "2", answer.Input)
	assert.Equal(t, turn.ThreadID, answer.ThreadID)
}

func TestStopButtonAndAgentSessionStop(t *testing.T) {
	for _, viaSlack := range []bool{false, true} {
		t.Run(fmt.Sprint("slack's stop button: ", viaSlack), func(t *testing.T) {
			w := newWorld(t)
			w.message(ada, general, ts(1), ts(1), "<@U0BOT> count everything")
			w.settle()
			turn := w.only()
			w.claimTurn(turn.ID)
			stop := func(user string) {
				if viaSlack {
					w.queueEvent(slack.SessionStopped{At: slack.Conversation{TeamID: team, ChannelID: general, ThreadTS: ts(1)}, UserID: user}, user, "")
					return
				}
				w.click(slack.Stop{Clicked: w.clicked(user, general, ts(1), ts(2)), TurnID: turn.ID}, user)
			}
			stop(grace)
			w.settle()
			assert.True(t, w.told(grace, "Only the person who asked can stop"))
			got, err := w.db.GetFoundryAgentTurn(w.ctx, w.ws, turn.ID)
			require.NoError(t, err)
			assert.Equal(t, model.FoundryAgentTurnRunning, got.Status)

			stop(ada)
			w.settle()
			got, err = w.db.GetFoundryAgentTurn(w.ctx, w.ws, turn.ID)
			require.NoError(t, err)
			assert.Equal(t, model.FoundryAgentTurnCancelled, got.Status)
			w.until("the reply", w.replyDone(turn.ID))
			updates := w.slack.of("chat.update")
			assert.Contains(t, updates[len(updates)-1].text(), "Stopped by")
			statuses := w.slack.of("agents.sessions.setStatus")
			assert.Equal(t, "active", statuses[len(statuses)-1].args["status"])
		})
	}
}

func TestUnlinkClick(t *testing.T) {
	w := newWorld(t)
	w.click(slack.Unlink{Clicked: w.clicked(ada, dmAda, "", ts(5))}, ada)
	w.settle()
	_, err := w.db.GetFoundrySlackLink(w.ctx, w.install.ID, ada)
	assert.ErrorIs(t, err, store.ErrNotFound)
	updates := w.slack.of("chat.update")
	require.Len(t, updates, 1)
	assert.Equal(t, ts(5), updates[0].args["ts"])
	assert.Contains(t, updates[0].text(), "Unlinked")

	// On an ephemeral message, or for a bot's proposal, the clicker is told.
	k := w.clicked(grace, general, ts(1), ts(6))
	k.Ephemeral = true
	w.click(slack.Unlink{Clicked: k}, grace)
	w.click(slack.Review{Clicked: w.clicked(grace, general, ts(1), ts(7)), ActionID: id.New(), PayloadHash: strings.Repeat("a", 64)}, grace)
	w.click(slack.Run{Clicked: w.clicked(grace, general, ts(1), ts(8)), ActionID: id.New(), PayloadHash: strings.Repeat("a", 64)}, grace)
	w.settle()
	assert.True(t, w.told(grace, "Unlinked"))
	assert.True(t, w.told(grace, "isn't on for this Slack workspace"))
}

func TestRateLimitsDeferNeverLose(t *testing.T) {
	w := newWorld(t)
	w.slack.queue("chat.postMessage", http.StatusTooManyRequests, "1", `{"ok":false,"error":"ratelimited"}`)
	in := w.message(ada, dmAda, ts(1), ts(1), "hello")
	w.handle()
	r := w.row(in)
	assert.Equal(t, "queued", r.status)
	assert.WithinDuration(t, time.Now().Add(time.Second), r.next, 900*time.Millisecond)
	turn := w.only()
	assert.Equal(t, 0, w.handle(), "deferred, not tried at once")

	w.until("the reply's message", func() bool {
		reply, err := w.db.GetFoundrySlackReply(w.ctx, turn.ID)
		return err == nil && reply.MessageTS != nil
	})
	assert.Len(t, w.turns(), 1, "the retry is the same turn")
	assert.Len(t, w.slack.posts("Working"), 2, "one refused, one posted")

	// The follower's update is refused too: the reply waits Retry-After.
	w.slack.queue("chat.update", http.StatusTooManyRequests, "1", `{"ok":false,"error":"ratelimited"}`)
	w.claimTurn(turn.ID).finish(agent.StopAnswered, "")
	w.follow(5 * time.Second)
	reply, err := w.db.GetFoundrySlackReply(w.ctx, turn.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SlackReplyFollowing, reply.Status)
	assert.True(t, reply.NextPollAt.After(time.Now().Add(500*time.Millisecond)))
	w.until("the reply", w.replyDone(turn.ID))
}

// crashing is the store a worker killed partway uses: once, when crash
// matches a write, the worker's goroutine ends there, its lease held.
type crashing struct {
	Store
	start  func(turnID string) bool
	update func(u model.SlackReplyUpdate) bool
}

func (c *crashing) StartFoundrySlackReply(ctx context.Context, turnID, messageTS, intakeID, owner string) error {
	if c.start != nil && c.start(turnID) {
		c.start = nil
		runtime.Goexit()
	}
	return c.Store.StartFoundrySlackReply(ctx, turnID, messageTS, intakeID, owner)
}

func (c *crashing) UpdateFoundrySlackReply(ctx context.Context, turnID, owner string, u model.SlackReplyUpdate) error {
	if c.update != nil && c.update(u) {
		c.update = nil
		runtime.Goexit()
	}
	return c.Store.UpdateFoundrySlackReply(ctx, turnID, owner, u)
}

// killed runs run on a goroutine of its own, which may end early.
func killed(run func(context.Context), ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx)
	}()
	<-done
}

func TestCrashMidIntakePostsAtMostOnceMore(t *testing.T) {
	db := &crashing{start: func(string) bool { return true }}
	w := newWorld(t, func(o *Options) { db.Store, o.DB, o.Cfg.Lease = o.DB, db, time.Second })
	in := w.message(ada, dmAda, ts(1), ts(1), "hello")
	run, err := w.bot.NextIntake(w.ctx)
	require.NoError(t, err)
	killed(run, w.ctx)
	assert.Equal(t, "running", w.row(in).status, "killed between the post and its record")
	assert.Len(t, w.slack.posts("Working"), 1)
	assert.Equal(t, 0, w.handle(), "its lease holds it")

	time.Sleep(1100 * time.Millisecond)
	w.until("the reclaimed row", func() bool { return w.row(in).status == "done" })
	assert.Len(t, w.turns(), 1)
	assert.Len(t, w.slack.posts("Working"), 2, "at most one duplicate")
	reply, err := w.db.GetFoundrySlackReply(w.ctx, w.only().ID)
	require.NoError(t, err)
	assert.NotNil(t, reply.MessageTS)
}

func TestCrashMidFollowPostsAtMostOnceMore(t *testing.T) {
	db := &crashing{}
	w := newWorld(t, func(o *Options) { db.Store, o.DB, o.Cfg.Lease = o.DB, db, time.Second })
	w.message(ada, dmAda, ts(1), ts(1), "load Sales")
	w.handle()
	turn := w.only()
	run := w.claimTurn(turn.ID)
	card := run.card(time.Now().Add(time.Hour))
	run.finish(agent.StopAnswered, "")
	db.update = func(u model.SlackReplyUpdate) bool { return model.ValidSlackTS(u.Posted["a:"+card.ID]) }
	time.Sleep(1100 * time.Millisecond) // the channel's limit, after "Working…"
	follow, err := w.bot.NextFollow(w.ctx)
	require.NoError(t, err)
	killed(follow, w.ctx)
	assert.Len(t, w.slack.posts("Load v3 of"), 1, "posted, then killed before its record")
	reply, err := w.db.GetFoundrySlackReply(w.ctx, turn.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SlackPostPending, reply.Posted["a:"+card.ID])
	assert.Equal(t, 0, w.follow(time.Second), "its lease holds it")

	time.Sleep(1100 * time.Millisecond)
	w.until("the reclaimed follow", w.replyDone(turn.ID))
	assert.Len(t, w.slack.posts("Load v3 of"), 2, "at most one duplicate")
}

func TestFollowEndsForTurnsThatEndWithoutFinishing(t *testing.T) {
	w := newWorld(t)
	w.message(ada, dmAda, ts(1), ts(1), "hello")
	w.handle()
	turn := w.only()
	run := w.claimTurn(turn.ID)
	run.emit(agent.Said{Step: 1, Text: "Half an answer"})
	require.NoError(t, w.db.SettleFoundryAgentTurn(w.ctx, turn.ID, run.worker, model.FoundryAgentTurnOutcome{Status: model.FoundryAgentTurnFailed,
		Error: "the model went away"}))
	w.until("the reply", w.replyDone(turn.ID))
	updates := w.slack.of("chat.update")
	assert.Contains(t, updates[len(updates)-1].text(), "the model went away")
}

func TestLongRepliesOverflowIntoMoreMessages(t *testing.T) {
	w := newWorld(t)
	w.message(ada, dmAda, ts(1), ts(1), "write a lot")
	w.handle()
	turn := w.only()
	run := w.claimTurn(turn.ID)
	for i := range slack.MaxReplyBlocks + 5 {
		run.emit(agent.Said{Step: i + 1, Text: fmt.Sprintf("Paragraph %d.", i)})
	}
	run.finish(agent.StopAnswered, "")
	w.until("the reply", w.replyDone(turn.ID))
	reply, err := w.db.GetFoundrySlackReply(w.ctx, turn.ID)
	require.NoError(t, err)
	second := reply.Posted["m:1"]
	require.True(t, model.ValidSlackTS(second))
	posted := w.slack.posts(fmt.Sprintf("Paragraph %d.", slack.MaxReplyBlocks+4))
	require.Len(t, posted, 1)
	updates := w.slack.of("chat.update")
	assert.Equal(t, *reply.MessageTS, updates[len(updates)-1].args["ts"])
	assert.Contains(t, updates[len(updates)-1].text(), "Paragraph 0.")
}

func TestSweep(t *testing.T) {
	w := newWorld(t)
	w.message(ada, dmAda, ts(1), ts(1), "hello")
	w.handle()
	turn := w.only()
	w.claimTurn(turn.ID).finish(agent.StopAnswered, "")
	w.until("the reply", w.replyDone(turn.ID))

	// A thread idle past retention is purged.
	_, err := testDB.Exec(`UPDATE foundry_slack_threads SET last_at=NOW(6) - INTERVAL 31 DAY`)
	require.NoError(t, err)
	// Slack refuses the uninstall, then rate limits it, then takes it.
	require.NoError(t, w.db.RevokeFoundrySlackInstall(w.ctx, w.ws, w.install.ID, false))
	w.slack.queue("apps.uninstall", http.StatusOK, "", `{"ok":false,"error":"internal_error"}`)
	w.bot.Sweep(w.ctx)
	assert.Equal(t, 0, w.threads())
	thread, err := w.db.GetFoundryAgentThread(w.ctx, w.ws, turn.ThreadID)
	if err == nil {
		assert.Empty(t, thread.Title)
	}
	got, err := w.db.GetFoundrySlackInstallByID(w.ctx, w.install.ID)
	require.NoError(t, err)
	assert.Nil(t, got.UninstalledAt)

	// Slack's Tier 1 limit holds the next sweep's call back: nothing is sent.
	w.bot.Sweep(w.ctx)
	require.Len(t, w.slack.of("apps.uninstall"), 1)
	got, err = w.db.GetFoundrySlackInstallByID(w.ctx, w.install.ID)
	require.NoError(t, err)
	assert.Nil(t, got.UninstalledAt)
}

func TestSweepTakesAGoneTokenAsUninstalled(t *testing.T) {
	w := newWorld(t)
	require.NoError(t, w.db.RevokeFoundrySlackInstall(w.ctx, w.ws, w.install.ID, false))
	w.slack.queue("apps.uninstall", http.StatusOK, "", `{"ok":false,"error":"invalid_auth"}`)
	w.bot.Sweep(w.ctx)
	got, err := w.db.GetFoundrySlackInstallByID(w.ctx, w.install.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.UninstalledAt)
	_, err = w.db.GetFoundrySlackInstallToken(w.ctx, w.install.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	call := w.slack.of("apps.uninstall")[0]
	assert.Equal(t, "1.2", call.args["client_id"])
}

func TestStoreFailuresAreCountedNotFatal(t *testing.T) {
	w := newWorld(t)
	broken := &brokenStore{Store: w.db}
	w.bot.db = broken
	w.bot.Sweep(w.ctx)
	run, err := w.bot.NextIntake(w.ctx)
	assert.Error(t, err)
	assert.Nil(t, run)
	run, err = w.bot.NextFollow(w.ctx)
	assert.Error(t, err)
	assert.Nil(t, run)
}

// brokenStore fails every call the sweep and claims make.
type brokenStore struct{ Store }

var errBroken = fmt.Errorf("the database is down")

func (brokenStore) ListFoundrySlackUninstalls(context.Context, int) ([]*model.SlackInstall, error) {
	return nil, errBroken
}
func (brokenStore) PurgeFoundrySlackThreads(context.Context, time.Duration, int) (int, error) {
	return 0, errBroken
}
func (brokenStore) PruneFoundrySlack(context.Context, time.Duration, time.Duration, int) (int64, error) {
	return 0, errBroken
}
func (brokenStore) ClaimFoundrySlackIntake(context.Context, time.Duration) (*model.SlackIntake, error) {
	return nil, errBroken
}
func (brokenStore) ClaimFoundrySlackReply(context.Context, time.Duration) (*model.SlackReply, error) {
	return nil, errBroken
}

func ptr[T any](v T) *T { return &v }
