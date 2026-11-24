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

// botMapping maps the Slack thread n's one Founder thread in bot mode.
func (f slackFixture) botMapping(n int, installID, agentThread string) *model.SlackThread {
	m := f.mapping(n, installID, agentThread)
	m.SlackUserID, m.ThreadTS = "", "1712345678.000900"
	return m
}

// Bot mode is a credential on the install: set from per_person, refused
// over another, and, turned off, retiring every thread of the bot's.
func TestFoundrySlackMode(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "mode")
	i := f.installed(t)
	assert.Equal(t, model.SlackPerPerson{}, i.Mode)
	bot := f.bot()
	for name, mode := range map[string]model.SlackIdentityMode{
		"none": nil, "no credential": model.SlackSharedBot{}, "a credential too long": model.SlackSharedBot{CredentialID: strings.Repeat("c", 27)},
	} {
		_, err := f.db.SetFoundrySlackMode(ctx, f.workspaceID, i.ID, mode)
		assert.Error(t, err, name)
	}
	_, err := f.db.SetFoundrySlackMode(ctx, id26("AWS-other"), i.ID, model.SlackSharedBot(bot))
	assert.ErrorIs(t, err, store.ErrNotFound, "another workspace's install")

	was, err := f.db.SetFoundrySlackMode(ctx, f.workspaceID, i.ID, model.SlackSharedBot(bot))
	require.NoError(t, err)
	assert.Equal(t, model.SlackPerPerson{}, was)
	got, err := f.db.GetFoundrySlackInstall(ctx, f.workspaceID)
	require.NoError(t, err)
	assert.Equal(t, model.SlackSharedBot(bot), got.Mode)
	was, err = f.db.SetFoundrySlackMode(ctx, f.workspaceID, i.ID, model.SlackSharedBot(bot))
	require.NoError(t, err)
	assert.Equal(t, model.SlackSharedBot(bot), was, "the same mode again changes nothing")
	_, err = f.db.SetFoundrySlackMode(ctx, f.workspaceID, i.ID, model.SlackSharedBot{CredentialID: id26("CRED-other")})
	assert.ErrorIs(t, err, store.ErrConflict, "per_person comes before another bot")

	for _, c := range []struct {
		workspace, credential string
		want                  bool
	}{{f.workspaceID, bot.CredentialID, true}, {id26("AWS-other"), bot.CredentialID, false}, {f.workspaceID, id26("CRED-other"), false}} {
		live, err := f.db.IsFoundrySlackBot(ctx, c.workspace, c.credential)
		require.NoError(t, err)
		assert.Equal(t, c.want, live, "%s %s", c.workspace, c.credential)
	}
	named, err := f.db.IsFoundrySlackBotCredential(ctx, bot.CredentialID)
	require.NoError(t, err)
	assert.True(t, named)

	// The bot's thread, mapped and running, with who spoke in it; and Ada's
	// own thread, mapped per person.
	require.NoError(t, f.sendAs(bot, 1, 0, f.limits()))
	live := f.claimed(t)
	botMap := f.botMapping(1, i.ID, f.threadID(1))
	require.NoError(t, f.db.CreateFoundrySlackThread(ctx, botMap))
	require.NoError(t, f.db.AddFoundrySlackThreadPerson(ctx, botMap.ID, "UADA"))
	mine := f.purgeable(t, 2, i.ID)

	was, err = f.db.SetFoundrySlackMode(ctx, f.workspaceID, i.ID, model.SlackPerPerson{})
	require.NoError(t, err)
	assert.Equal(t, model.SlackSharedBot(bot), was, "the caller revokes the credential it returns")
	got, err = f.db.GetFoundrySlackInstall(ctx, f.workspaceID)
	require.NoError(t, err)
	assert.Equal(t, model.SlackPerPerson{}, got.Mode)
	for _, check := range []func() (bool, error){
		func() (bool, error) { return f.db.IsFoundrySlackBot(ctx, f.workspaceID, bot.CredentialID) },
		func() (bool, error) { return f.db.IsFoundrySlackBotCredential(ctx, bot.CredentialID) },
	} {
		is, err := check()
		require.NoError(t, err)
		assert.False(t, is)
	}
	_, err = f.db.GetFoundryAgentThread(ctx, f.workspaceID, f.threadID(1))
	assert.ErrorIs(t, err, store.ErrNotFound, "the bot's thread is deleted")
	cancelled, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, live.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentTurnCancelled, cancelled.Status)
	assert.Zero(t, cancelled.ReservedMicros)
	assert.EqualValues(t, 500_000, cancelled.Spend.USDMicros, "its reservation is charged")
	_, err = f.db.GetFoundrySlackThread(ctx, i.ID, "C1", botMap.ThreadTS, "")
	assert.ErrorIs(t, err, store.ErrNotFound)

	n, err := f.db.PurgeFoundrySlackThreads(ctx, model.SlackThreadIdle, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the bot's mapping is due at once, Ada's isn't")
	assert.True(t, purged(t, f.threadID(1)))
	assert.False(t, dbTrue(t, `SELECT COUNT(*)>0 FROM foundry_slack_thread_people WHERE thread_id=?`, botMap.ID), "who spoke goes with it")
	_, err = f.db.GetFoundrySlackThread(ctx, i.ID, "C1", mine.ThreadTS, "UADA")
	require.NoError(t, err)
	assert.False(t, purged(t, mine.AgentThreadID))
	was, err = f.db.SetFoundrySlackMode(ctx, f.workspaceID, i.ID, model.SlackPerPerson{})
	require.NoError(t, err)
	assert.Equal(t, model.SlackPerPerson{}, was)

	// A revoked install's credential stays named until it is revoked too.
	_, err = f.db.SetFoundrySlackMode(ctx, f.workspaceID, i.ID, model.SlackSharedBot{CredentialID: id26("CRED-two")})
	require.NoError(t, err)
	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, i.ID, false))
	is, err := f.db.IsFoundrySlackBot(ctx, f.workspaceID, id26("CRED-two"))
	require.NoError(t, err)
	assert.False(t, is, "a revoked install has no bot")
	is, err = f.db.IsFoundrySlackBotCredential(ctx, id26("CRED-two"))
	require.NoError(t, err)
	assert.True(t, is)
	_, err = f.db.SetFoundrySlackMode(ctx, f.workspaceID, i.ID, model.SlackPerPerson{})
	assert.ErrorIs(t, err, store.ErrNotFound, "a revoked install")

	// The sweep finds it, revokes it, and forgets it, which lifts the lock.
	retired, err := f.db.ListFoundrySlackRetiredBots(ctx, 10)
	require.NoError(t, err)
	require.Len(t, retired, 1)
	assert.Equal(t, i.ID, retired[0].ID)
	assert.Equal(t, model.SlackSharedBot{CredentialID: id26("CRED-two")}, retired[0].Mode)
	none, err := f.db.ListFoundrySlackRetiredBots(ctx, 0)
	require.NoError(t, err)
	assert.Empty(t, none, "up to its limit")
	assert.ErrorIs(t, f.db.ForgetFoundrySlackBot(ctx, i.ID, id26("CRED-other")), store.ErrNotFound, "another key")
	require.NoError(t, f.db.ForgetFoundrySlackBot(ctx, i.ID, id26("CRED-two")))
	assert.ErrorIs(t, f.db.ForgetFoundrySlackBot(ctx, i.ID, id26("CRED-two")), store.ErrNotFound, "forgotten already")
	is, err = f.db.IsFoundrySlackBotCredential(ctx, id26("CRED-two"))
	require.NoError(t, err)
	assert.False(t, is)
	retired, err = f.db.ListFoundrySlackRetiredBots(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, retired)
}

// A live install's bot is no retired one, and its key isn't forgotten.
func TestFoundrySlackRetiredBotsAreRevokedInstalls(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "retired")
	i := f.installed(t)
	bot := f.bot()
	_, err := f.db.SetFoundrySlackMode(ctx, f.workspaceID, i.ID, model.SlackSharedBot(bot))
	require.NoError(t, err)
	retired, err := f.db.ListFoundrySlackRetiredBots(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, retired)
	assert.ErrorIs(t, f.db.ForgetFoundrySlackBot(ctx, i.ID, bot.CredentialID), store.ErrNotFound)
	live, err := f.db.IsFoundrySlackBot(ctx, f.workspaceID, bot.CredentialID)
	require.NoError(t, err)
	assert.True(t, live)
}

// Who spoke to the bot is kept per bot-mode Slack thread.
func TestFoundrySlackThreadPeople(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "people")
	i := f.installed(t)
	require.NoError(t, f.sendAs(f.bot(), 1, 0, f.limits()))
	botMap := f.botMapping(1, i.ID, f.threadID(1))
	require.NoError(t, f.db.CreateFoundrySlackThread(ctx, botMap))
	require.NoError(t, f.send(2, 0))
	mine := f.mapping(2, i.ID, f.threadID(2))
	require.NoError(t, f.db.CreateFoundrySlackThread(ctx, mine))

	assert.Error(t, f.db.AddFoundrySlackThreadPerson(ctx, botMap.ID, "@ada"), "not a Slack id")
	assert.ErrorIs(t, f.db.AddFoundrySlackThreadPerson(ctx, mine.ID, "UADA"), store.ErrNotFound, "a person's mapping")
	assert.ErrorIs(t, f.db.AddFoundrySlackThreadPerson(ctx, id26("SM-none"), "UADA"), store.ErrNotFound)
	require.NoError(t, f.db.AddFoundrySlackThreadPerson(ctx, botMap.ID, "UADA"))
	require.NoError(t, f.db.AddFoundrySlackThreadPerson(ctx, botMap.ID, "UADA"), "again changes nothing")
	for user, want := range map[string]bool{"UADA": true, "UGRACE": false} {
		is, err := f.db.IsFoundrySlackThreadPerson(ctx, botMap.ID, user)
		require.NoError(t, err)
		assert.Equal(t, want, is, user)
	}
	is, err := f.db.IsFoundrySlackThreadPerson(ctx, mine.ID, "UADA")
	require.NoError(t, err)
	assert.False(t, is)
}

// A Slack user's messages to a bot-mode install are counted in the window
// before a row came, that row and others' not counted, and the same for a
// retry of it later.
func TestCountFoundrySlackUserMessages(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "percap")
	i := f.installed(t)
	createWorkspace(t, id26("AWS-percap2"))
	other, _, err := f.db.InstallFoundrySlack(ctx, f.install(2, id26("AWS-percap2"), "T2"), sealAs("xoxb-2"))
	require.NoError(t, err)
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	row := func(n int, install, user string, kind model.SlackIntakeKind, ago time.Duration) {
		t.Helper()
		in := &model.SlackIntake{ID: f.intakeID(n), InstallID: install, DedupeKey: model.SlackEventDedupe(fmt.Sprintf("EvCap%d", n)), Kind: kind,
			SlackUserID: user, Payload: json.RawMessage(`{}`)}
		if kind != model.SlackIntakeEvent {
			in.DedupeKey = model.SlackClickDedupe("T1", user, fmt.Sprint(n), string(kind))
		}
		require.NoError(t, f.db.QueueFoundrySlackIntake(ctx, in))
		_, err := testDB.Exec(`UPDATE foundry_slack_intake SET created_at=? WHERE id=?`, at.Add(-ago), in.ID)
		require.NoError(t, err)
	}
	row(1, i.ID, "UADA", model.SlackIntakeEvent, 0) // the row counted from
	row(2, i.ID, "UADA", model.SlackIntakeEvent, time.Minute)
	row(3, i.ID, "UADA", model.SlackIntakeEvent, 59*time.Minute)
	row(4, i.ID, "UADA", model.SlackIntakeEvent, 61*time.Minute)    // before the hour
	row(5, i.ID, "UADA", model.SlackIntakeEvent, -time.Minute)      // after the row
	row(6, i.ID, "UGRACE", model.SlackIntakeEvent, time.Minute)     // someone else
	row(7, other.ID, "UADA", model.SlackIntakeEvent, time.Minute)   // another install
	row(8, i.ID, "UADA", model.SlackIntakeInteraction, time.Minute) // a click
	row(9, i.ID, "UADA", model.SlackIntakeEvent, 0)                 // at the same moment
	row(10, i.ID, "UADA", model.SlackIntakeShortcut, 2*time.Minute) // a shortcut asks the bot too
	n, err := f.db.CountFoundrySlackUserMessages(ctx, i.ID, "UADA", f.intakeID(1), time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 4, n, "rows 2, 3, 9 and 10")
	n, err = f.db.CountFoundrySlackUserMessages(ctx, i.ID, "UADA", f.intakeID(1), 2*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 5, n, "row 4 within two hours")
	n, err = f.db.CountFoundrySlackUserMessages(ctx, other.ID, "UADA", f.intakeID(1), time.Hour)
	require.NoError(t, err)
	assert.Zero(t, n, "the row isn't that install's")
	n, err = f.db.CountFoundrySlackUserMessages(ctx, i.ID, "UADA", id26("SN-none"), time.Hour)
	require.NoError(t, err)
	assert.Zero(t, n, "no such row")
	_, err = f.db.CountFoundrySlackUserMessages(ctx, i.ID, "UADA", f.intakeID(1), 0)
	assert.Error(t, err)
}
