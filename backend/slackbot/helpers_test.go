package slackbot

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func TestCardState(t *testing.T) {
	now := time.Now()
	why := "no room"
	long := now.Add(-time.Hour)
	for status, want := range map[model.FoundryAgentActionStatus]slack.CardState{
		model.FoundryAgentActionPrepared:  slack.CardOpen{},
		model.FoundryAgentActionExpired:   slack.CardExpired{},
		model.FoundryAgentActionDismissed: slack.CardDismissed{By: ada},
		model.FoundryAgentActionSucceeded: slack.CardRan{By: ada},
		model.FoundryAgentActionFailed:    slack.CardRan{By: ada, Failed: true, Error: why},
		model.FoundryAgentActionUnknown:   slack.CardRan{By: ada, Failed: true, Error: "Foundry lost its answer, so it may or may not have taken effect"},
	} {
		a := &model.FoundryAgentAction{Status: status, ExpiresAt: now.Add(time.Hour)}
		if status == model.FoundryAgentActionFailed {
			a.Error = &why
		}
		assert.Equal(t, want, cardState(a, ada, now), status)
	}
	assert.Equal(t, slack.CardExpired{}, cardState(&model.FoundryAgentAction{Status: model.FoundryAgentActionPrepared, ExpiresAt: long}, ada, now))
	running := now.Add(-time.Second)
	assert.Nil(t, cardState(&model.FoundryAgentAction{Status: model.FoundryAgentActionExecuting, DecidedAt: &running}, ada, now),
		"a click running it draws nothing new")
	assert.IsType(t, slack.CardRan{}, cardState(&model.FoundryAgentAction{Status: model.FoundryAgentActionExecuting, DecidedAt: &long}, ada, now),
		"a click that ran too long lost its answer")
}

func TestMemberOf(t *testing.T) {
	install := &model.SlackInstall{TeamID: team}
	member := slack.User{ID: ada, TeamID: team}
	assert.True(t, memberOf(member, install, ""))
	assert.True(t, memberOf(member, install, team))
	assert.False(t, memberOf(member, install, "T0OTHER"), "a message of another team")
	assert.False(t, memberOf(slack.User{ID: ada, TeamID: team, Restricted: true}, install, ""))
	assert.False(t, memberOf(slack.User{ID: ada, TeamID: team, Stranger: true}, install, ""))
	grid := &model.SlackInstall{TeamID: team, EnterpriseID: "E0ORG"}
	gridMember := slack.User{ID: ada, TeamID: "T0SESSION", EnterpriseID: "E0ORG", EnterpriseTeams: []string{"T0X", team}}
	assert.True(t, memberOf(gridMember, grid, "T0SESSION"), "in Grid, a user's team is their session's")
	assert.False(t, memberOf(slack.User{ID: ada, EnterpriseID: "E0ORG", EnterpriseTeams: []string{"T0X"}}, grid, ""), "another workspace of the org")
	assert.False(t, memberOf(slack.User{ID: ada, EnterpriseID: "E0OTHER", EnterpriseTeams: []string{team}}, grid, ""), "another org")
}

func TestWords(t *testing.T) {
	assert.Equal(t, "Founder refused this.", sentence(status.Error(codes.FailedPrecondition, " ")))
	assert.Equal(t, "Not yours.", sentence(status.Error(codes.FailedPrecondition, "not yours")))
	assert.Equal(t, "Why?", sentence(status.Error(codes.FailedPrecondition, "why?")))
	assert.Equal(t, "Stop!", sentence(status.Error(codes.FailedPrecondition, "stop!")))
	for code, want := range map[codes.Code]bool{codes.FailedPrecondition: true, codes.ResourceExhausted: true, codes.PermissionDenied: true,
		codes.InvalidArgument: true, codes.Unavailable: false, codes.Internal: false, codes.NotFound: false} {
		assert.Equal(t, want, refusedInWords(status.Error(code, "x")), code)
	}
	st, err := status.New(codes.FailedPrecondition, "busy").WithDetails(&errdetails.ErrorInfo{Reason: "FOUNDRY_AGENT_THREAD_BUSY"})
	require.NoError(t, err)
	assert.Equal(t, "FOUNDRY_AGENT_THREAD_BUSY", reasonOf(st.Err()))
	assert.Empty(t, reasonOf(errors.New("plain")))
	assert.True(t, slackGone(&slack.APIError{Method: "chat.update", Code: "message_not_found"}))
	assert.False(t, slackGone(&slack.APIError{Method: "chat.update", Code: "internal_error"}))
	wait, ok := rateLimited(&slack.RateLimitedError{RetryAfter: 10 * time.Millisecond})
	assert.True(t, ok)
	assert.Equal(t, time.Second, wait, "at least a second")
	_, ok = rateLimited(errors.New("no"))
	assert.False(t, ok)
	assert.True(t, inDM("D0X"))
	assert.False(t, inDM("C0X"))
	assert.Equal(t, ts(1), threadOf(slack.Clicked{MessageTS: ts(1)}))
	assert.Equal(t, ts(2), threadOf(slack.Clicked{MessageTS: ts(1), ThreadTS: ts(2)}))
}

func TestRetryBacksOffThenGivesUp(t *testing.T) {
	b := &Bot{cfg: config.SlackConfig{MaxAttempts: 3, RetryBackoff: time.Second}}
	assert.Equal(t, model.SlackIntakeRetry{After: time.Second, Error: "x"}, b.retry(&model.SlackIntake{Attempts: 1}, errors.New("x")))
	assert.Equal(t, model.SlackIntakeRetry{After: 2 * time.Second, Error: "x"}, b.retry(&model.SlackIntake{Attempts: 2}, errors.New("x")))
	assert.Equal(t, model.SlackIntakeGaveUp{Error: "x (gave up after 3 attempts)"}, b.retry(&model.SlackIntake{Attempts: 3}, errors.New("x")))
}

func TestNewRefusesWhatItCantRunWith(t *testing.T) {
	w := newWorld(t)
	good := Options{DB: w.db, Sessions: w.sessions, Slack: w.bot.slack, Keys: w.keys, Cfg: w.cfg}
	for name, mutate := range map[string]func(*Options){
		"no store":    func(o *Options) { o.DB = nil },
		"no sessions": func(o *Options) { o.Sessions = nil },
		"no lease":    func(o *Options) { o.Cfg.Lease = 0 },
		"no site":     func(o *Options) { o.Cfg.SiteURL = "foundry.example.com/x" },
	} {
		o := good
		mutate(&o)
		_, err := New(o)
		assert.Error(t, err, name)
	}
	b, err := New(good)
	require.NoError(t, err)
	assert.NotNil(t, b.now)
}

func TestRowsTheBotCantRead(t *testing.T) {
	w := newWorld(t)
	bad := func(kind model.SlackIntakeKind, payload string) string {
		in := &model.SlackIntake{ID: id.New(), InstallID: w.install.ID, DedupeKey: model.SlackEventDedupe("Ev" + id.New()), Kind: kind,
			Payload: []byte(payload)}
		require.NoError(t, w.db.QueueFoundrySlackIntake(w.ctx, in))
		return in.ID
	}
	unknown := bad(model.SlackIntakeEvent, `{"kind":"nope","data":{}}`)
	clickAsEvent := bad(model.SlackIntakeEvent, `{"kind":"approve","data":{}}`)
	eventAsClick := bad(model.SlackIntakeInteraction, `{"kind":"dm","data":{}}`)
	unhandled := bad(model.SlackIntakeEvent, `{"kind":"uninstalled","data":{"team_id":"T0TEAM"}}`)
	claimedOften := w.message(ada, dmAda, ts(1), ts(1), "hello")
	_, err := testDB.Exec(`UPDATE foundry_slack_intake SET attempts=? WHERE id=?`, maxClaims, claimedOften)
	require.NoError(t, err)
	w.handle()
	for _, in := range []string{unknown, clickAsEvent, eventAsClick} {
		assert.Equal(t, "failed", w.row(in).status, in)
	}
	assert.Equal(t, "done", w.row(unhandled).status, "an uninstall ends in the API")
	r := w.row(claimedOften)
	assert.Equal(t, "failed", r.status)
	assert.Contains(t, r.error.String, "claimed")
	assert.Empty(t, w.turns())
}

func TestInstallWithoutItsTokenEndsRows(t *testing.T) {
	w := newWorld(t)
	_, err := testDB.Exec(`DELETE FROM foundry_slack_install_tokens`)
	require.NoError(t, err)
	in := w.message(ada, dmAda, ts(1), ts(1), "hello")
	w.handle()
	assert.Equal(t, "done", w.row(in).status)
	assert.Empty(t, w.slack.calls)
}

func TestSlackRefusingTheChannelGivesUp(t *testing.T) {
	w := newWorld(t)
	w.slack.queue("chat.postEphemeral", http.StatusOK, "", `{"ok":false,"error":"channel_not_found"}`)
	in := w.message(alan, general, ts(1), ts(1), "hello")
	w.handle()
	r := w.row(in)
	assert.Equal(t, "failed", r.status)
	assert.Contains(t, r.error.String, "channel_not_found")
}

func TestAgentStatusOffForAnInstallWithoutAgents(t *testing.T) {
	w := newWorld(t)
	w.slack.queue("agents.sessions.setStatus", http.StatusOK, "", `{"ok":false,"error":"feature_disabled"}`)
	w.message(ada, dmAda, ts(1), ts(1), "hello")
	w.handle()
	w.message(ada, "D0OTHER", ts(2), ts(2), "hello again")
	w.handle()
	assert.Len(t, w.slack.of("agents.sessions.setStatus"), 1, "not asked again for a day")
	assert.Len(t, w.turns(), 2)
}

func TestClicksOnWhatIsGone(t *testing.T) {
	w := newWorld(t)
	gone := w.click(slack.Approve{Clicked: w.clicked(ada, dmAda, ts(1), ts(2)), ActionID: id.New(), PayloadHash: hashOf("x")}, ada)
	stale := w.click(slack.Stop{Clicked: w.clicked(ada, dmAda, ts(1), ts(3)), TurnID: id.New()}, ada)
	w.settle()
	assert.Equal(t, "done", w.row(gone).status)
	assert.Equal(t, "done", w.row(stale).status)
	assert.True(t, w.told(ada, "This action is gone."))
	assert.False(t, w.told(ada, "Only the person who asked"), "a stop of a reply no longer followed says nothing")
}

func TestFollowEndsWhenSlackOrTheInstallGoes(t *testing.T) {
	w := newWorld(t)
	w.message(ada, dmAda, ts(1), ts(1), "hello")
	w.handle()
	turn := w.only()
	w.claimTurn(turn.ID).finish(agent.StopAnswered, "")
	w.slack.queue("chat.update", http.StatusOK, "", `{"ok":false,"error":"message_not_found"}`)
	w.follow(5 * time.Second)
	assert.True(t, w.replyDone(turn.ID)(), "a message Slack lost ends its follow")

	w.message(ada, dmAda, ts(2), ts(2), "again")
	w.settle()
	next := w.last()
	r, err := w.db.GetFoundrySlackReply(w.ctx, next.ID)
	require.NoError(t, err)
	require.NotNil(t, r.MessageTS)
	_, err = testDB.Exec(`UPDATE foundry_slack_installs SET revoked_at=NOW(6)`)
	require.NoError(t, err)
	w.follow(5 * time.Second)
	assert.True(t, w.replyDone(next.ID)(), "a revoked install's follow ends")
}
