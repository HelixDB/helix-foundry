package store_test

import (
	"context"
	"strings"
	"testing"

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
