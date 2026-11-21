package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// slackFixture is a workspace, Ada its member, and the Slack team T1.
type slackFixture struct {
	agentFixture
}

func newSlackFixture(t *testing.T, tag string) slackFixture {
	t.Helper()
	return slackFixture{newAgentFixture(t, tag)}
}

func (f slackFixture) installID(n int) string { return id26(fmt.Sprintf("SI-%s-%d-", f.tag, n)) }

func (f slackFixture) install(n int, workspaceID, team string) *model.SlackInstall {
	return &model.SlackInstall{ID: f.installID(n), WorkspaceID: workspaceID, TeamID: team, TeamName: "Acme " + team, AppID: "A1", BotUserID: "UBOT",
		Scopes: []string{"chat:write", "users:read"}, InstalledByUserID: agentUser, InstalledBySlackUserID: "UADA"}
}

func sealAs(tag string) func(string) (model.SlackSealedToken, error) {
	return func(id string) (model.SlackSealedToken, error) {
		return model.SlackSealedToken{Sealed: []byte(tag + ":" + id), KeyID: "k1"}, nil
	}
}

// installed installs team T1 for the fixture's workspace.
func (f slackFixture) installed(t *testing.T) *model.SlackInstall {
	t.Helper()
	i, reinstalled, err := f.db.InstallFoundrySlack(context.Background(), f.install(1, f.workspaceID, "T1"), sealAs("xoxb-1"))
	require.NoError(t, err)
	require.False(t, reinstalled)
	return i
}

func slackRefusal(t *testing.T, err error) model.SlackRefusalReason {
	t.Helper()
	var r model.SlackRefusal
	require.ErrorAs(t, err, &r)
	return r.Reason
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// dbTrue reports a boolean SQL expression over the test schema.
func dbTrue(t *testing.T, query string, args ...any) bool {
	t.Helper()
	var ok bool
	require.NoError(t, testDB.QueryRow(query, args...).Scan(&ok))
	return ok
}

func dbExec(t *testing.T, query string, args ...any) {
	t.Helper()
	_, err := testDB.Exec(query, args...)
	require.NoError(t, err)
}

func TestFoundrySlackInstall(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "install")
	other := id26("SWS-install-other")
	createWorkspace(t, other)

	first := f.installed(t)
	assert.Equal(t, f.installID(1), first.ID)
	assert.Equal(t, []string{"chat:write", "users:read"}, first.Scopes)
	assert.Equal(t, model.SlackPerPerson{}, first.Mode, "L1 installs act per person")
	assert.Nil(t, first.RevokedAt)
	token, err := f.db.GetFoundrySlackInstallToken(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, "xoxb-1:"+first.ID, string(token.Sealed), "sealed under the install's id")
	for _, get := range []func() (*model.SlackInstall, error){
		func() (*model.SlackInstall, error) { return f.db.GetFoundrySlackInstall(ctx, f.workspaceID) },
		func() (*model.SlackInstall, error) { return f.db.GetFoundrySlackInstallByTeam(ctx, "T1") },
		func() (*model.SlackInstall, error) { return f.db.GetFoundrySlackInstallByID(ctx, first.ID) },
	} {
		got, err := get()
		require.NoError(t, err)
		assert.Equal(t, first.ID, got.ID)
	}

	// The same team for the same workspace is a reinstall: its id stays, and
	// it takes the new names, scopes, installers and token.
	again := f.install(2, f.workspaceID, "T1")
	again.TeamName, again.Scopes, again.InstalledBySlackUserID, again.EnterpriseID = "Acme Renamed", []string{"chat:write"}, "UGRACE", "E1"
	re, reinstalled, err := f.db.InstallFoundrySlack(ctx, again, sealAs("xoxb-2"))
	require.NoError(t, err)
	assert.True(t, reinstalled)
	assert.Equal(t, first.ID, re.ID)
	assert.Equal(t, "Acme Renamed", re.TeamName)
	assert.Equal(t, []string{"chat:write"}, re.Scopes)
	assert.Equal(t, "UGRACE", re.InstalledBySlackUserID)
	assert.Equal(t, "E1", re.EnterpriseID)
	assert.True(t, re.UpdatedAt.After(first.UpdatedAt))
	token, err = f.db.GetFoundrySlackInstallToken(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, "xoxb-2:"+first.ID, string(token.Sealed), "the reinstall's token replaces the first")

	// The team is taken for another workspace; the workspace holds one team.
	_, _, err = f.db.InstallFoundrySlack(ctx, f.install(3, other, "T1"), sealAs("x"))
	assert.Equal(t, model.SlackTeamTaken, slackRefusal(t, err))
	_, _, err = f.db.InstallFoundrySlack(ctx, f.install(4, f.workspaceID, "T2"), sealAs("x"))
	assert.Equal(t, model.SlackInstalled, slackRefusal(t, err))
	_, err = f.db.GetFoundrySlackInstallByID(ctx, f.installID(4))
	assert.ErrorIs(t, err, store.ErrNotFound, "a refused install writes nothing")

	// Bad input never reaches the table.
	bad := f.install(5, other, "T5")
	bad.TeamID = ""
	_, _, err = f.db.InstallFoundrySlack(ctx, bad, sealAs("x"))
	assert.Error(t, err)
	_, _, err = f.db.InstallFoundrySlack(ctx, f.install(5, other, "T5"), func(string) (model.SlackSealedToken, error) {
		return model.SlackSealedToken{}, errors.New("no key")
	})
	assert.ErrorContains(t, err, "no key")
	_, _, err = f.db.InstallFoundrySlack(ctx, f.install(5, other, "T5"), func(string) (model.SlackSealedToken, error) {
		return model.SlackSealedToken{Sealed: []byte("s")}, nil
	})
	assert.Error(t, err, "a token sealed under no key")
	_, err = f.db.GetFoundrySlackInstall(ctx, other)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// A deleted workspace installs nothing.
	gone := id26("SWS-install-gone")
	createWorkspace(t, gone)
	deleteWorkspace(t, gone)
	_, _, err = f.db.InstallFoundrySlack(ctx, f.install(6, gone, "T6"), sealAs("x"))
	assert.ErrorIs(t, err, store.ErrNotFound)

	// An id taken by another install is a conflict.
	_, _, err = f.db.InstallFoundrySlack(ctx, f.install(1, other, "T7"), sealAs("x"))
	assert.ErrorIs(t, err, store.ErrConflict)

	for _, get := range []func() error{
		func() error { _, err := f.db.GetFoundrySlackInstallByTeam(ctx, "T9"); return err },
		func() error { _, err := f.db.GetFoundrySlackInstallToken(ctx, f.installID(9)); return err },
	} {
		assert.ErrorIs(t, get(), store.ErrNotFound)
	}
}

// Two workspaces installing one team at once: one wins, the other is told
// the team is taken.
func TestFoundrySlackInstallRace(t *testing.T) {
	f := newSlackFixture(t, "race")
	workspaces := make([]string, 4)
	for i := range workspaces {
		workspaces[i] = id26(fmt.Sprintf("SWS-race-%d-", i))
		createWorkspace(t, workspaces[i])
	}
	errs := make([]error, len(workspaces))
	var wg sync.WaitGroup
	for i, ws := range workspaces {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = f.db.InstallFoundrySlack(context.Background(), f.install(i, ws, "TRACE"), sealAs("x"))
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
			continue
		}
		assert.Equal(t, model.SlackTeamTaken, slackRefusal(t, err))
	}
	assert.Equal(t, 1, won)
}

func TestFoundrySlackDisconnect(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "disconnect")
	i := f.installed(t)
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UADA", sha("t1"), time.Minute))
	_, err := f.db.LinkFoundrySlackUser(ctx, f.workspaceID, sha("t1"), agentUser)
	require.NoError(t, err)
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UGRACE", sha("t2"), time.Minute))
	f.queue(t, i.ID, 1, "conv-a")
	f.queue(t, i.ID, 2, "conv-a")
	done := f.claim(t)
	require.NoError(t, f.db.EndFoundrySlackIntake(ctx, done.ID, *done.LeaseOwner, model.SlackIntakeEnded{}))
	_, err = f.db.HoldFoundrySlackReply(ctx, f.reply(i.ID, 1))
	require.NoError(t, err)

	assert.ErrorIs(t, f.db.RevokeFoundrySlackInstall(ctx, id26("SWS-other"), i.ID, false), store.ErrNotFound, "another workspace's")
	// apps.uninstall failed: revoked, its token kept for the sweep.
	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, i.ID, false))
	assert.ErrorIs(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, i.ID, false), store.ErrNotFound, "once")
	_, err = f.db.GetFoundrySlackInstall(ctx, f.workspaceID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	revoked, err := f.db.GetFoundrySlackInstallByID(ctx, i.ID)
	require.NoError(t, err)
	assert.NotNil(t, revoked.RevokedAt)
	assert.Nil(t, revoked.UninstalledAt)
	_, err = f.db.GetFoundrySlackInstallToken(ctx, i.ID)
	require.NoError(t, err, "the sweep still needs the token")
	pending, err := f.db.ListFoundrySlackUninstalls(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, i.ID, pending[0].ID)
	links, err := f.db.ListFoundrySlackLinks(ctx, i.ID)
	require.NoError(t, err)
	assert.Empty(t, links)
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=0 FROM foundry_slack_link_tokens WHERE install_id=?`, i.ID))
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=0 FROM foundry_slack_replies WHERE install_id=?`, i.ID))
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=1 AND MAX(status)='done' FROM foundry_slack_intake WHERE install_id=?`, i.ID),
		"open intake is deleted; a done row keeps deduping")

	// Until Slack is told, the team can't be installed again, anywhere.
	other := id26("SWS-disconnect-other")
	createWorkspace(t, other)
	_, _, err = f.db.InstallFoundrySlack(ctx, f.install(2, f.workspaceID, "T1"), sealAs("x"))
	assert.Equal(t, model.SlackDisconnecting, slackRefusal(t, err))
	_, _, err = f.db.InstallFoundrySlack(ctx, f.install(2, other, "T1"), sealAs("x"))
	assert.Equal(t, model.SlackDisconnecting, slackRefusal(t, err))

	require.NoError(t, f.db.MarkFoundrySlackUninstalled(ctx, i.ID))
	assert.ErrorIs(t, f.db.MarkFoundrySlackUninstalled(ctx, i.ID), store.ErrNotFound, "once")
	_, err = f.db.GetFoundrySlackInstallToken(ctx, i.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	pending, err = f.db.ListFoundrySlackUninstalls(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, pending)

	// Now a new install: a new id, Slack's earlier install untouched.
	fresh, reinstalled, err := f.db.InstallFoundrySlack(ctx, f.install(2, f.workspaceID, "T1"), sealAs("x"))
	require.NoError(t, err)
	assert.False(t, reinstalled)
	assert.Equal(t, f.installID(2), fresh.ID)

	// apps.uninstall succeeded: uninstalled at once, its token gone.
	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, fresh.ID, true))
	gone, err := f.db.GetFoundrySlackInstallByID(ctx, fresh.ID)
	require.NoError(t, err)
	assert.NotNil(t, gone.UninstalledAt)
	_, err = f.db.GetFoundrySlackInstallToken(ctx, fresh.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, _, err = f.db.InstallFoundrySlack(ctx, f.install(3, other, "T1"), sealAs("x"))
	require.NoError(t, err, "the team is free")
	assert.ErrorIs(t, f.db.MarkFoundrySlackUninstalled(ctx, f.installID(3)), store.ErrNotFound, "a live install isn't marked")
}

func TestFoundrySlackUninstallEvent(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "uninstall")
	i := f.installed(t)
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UADA", sha("t1"), time.Minute))
	_, err := f.db.LinkFoundrySlackUser(ctx, f.workspaceID, sha("t1"), agentUser)
	require.NoError(t, err)
	f.queue(t, i.ID, 1, "conv-a")

	_, err = f.db.UninstallFoundrySlackTeam(ctx, "T1", "", model.SlackEventDedupe("Ev1"), time.Now())
	assert.Error(t, err, "no intake id")
	_, err = f.db.UninstallFoundrySlackTeam(ctx, "T1", id26("SN-u-0"), "Ev1", time.Now())
	assert.Error(t, err, "no dedupe key")

	out, err := f.db.UninstallFoundrySlackTeam(ctx, "T9", id26("SN-u-1"), model.SlackEventDedupe("Ev1"), time.Now())
	require.NoError(t, err)
	assert.Equal(t, model.SlackUninstallUnknownTeam, out)

	// A retry of an event from before the install (a stale retry after a
	// reinstall) changes nothing.
	out, err = f.db.UninstallFoundrySlackTeam(ctx, "T1", id26("SN-u-2"), model.SlackEventDedupe("Ev2"), i.UpdatedAt.Add(-2*time.Second))
	require.NoError(t, err)
	assert.Equal(t, model.SlackUninstallStale, out)
	_, err = f.db.GetFoundrySlackInstall(ctx, f.workspaceID)
	require.NoError(t, err)
	out, err = f.db.UninstallFoundrySlackTeam(ctx, "T1", id26("SN-u-3"), model.SlackEventDedupe("Ev2"), time.Now())
	require.NoError(t, err)
	assert.Equal(t, model.SlackUninstallDuplicate, out, "the stale event was recorded")

	// Slack's event_time is whole seconds: one in the install's second counts.
	out, err = f.db.UninstallFoundrySlackTeam(ctx, "T1", id26("SN-u-4"), model.SlackEventDedupe("Ev3"), i.UpdatedAt.Truncate(time.Second))
	require.NoError(t, err)
	assert.Equal(t, model.SlackUninstallDone, out)
	gone, err := f.db.GetFoundrySlackInstallByID(ctx, i.ID)
	require.NoError(t, err)
	assert.NotNil(t, gone.RevokedAt)
	assert.NotNil(t, gone.UninstalledAt)
	_, err = f.db.GetFoundrySlackInstallToken(ctx, i.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.db.GetFoundrySlackLink(ctx, i.ID, "UADA")
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=0 FROM foundry_slack_intake WHERE install_id=? AND status='queued'`, i.ID))
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=2 FROM foundry_slack_intake WHERE install_id=? AND status='done' AND payload IS NULL`, i.ID),
		"both events stay for dedupe")
	pending, err := f.db.ListFoundrySlackUninstalls(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, pending, "Slack told us: nothing for the sweep")
	out, err = f.db.UninstallFoundrySlackTeam(ctx, "T1", id26("SN-u-5"), model.SlackEventDedupe("Ev4"), time.Now())
	require.NoError(t, err)
	assert.Equal(t, model.SlackUninstallUnknownTeam, out)
}

func TestFoundrySlackInstallStates(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "states")
	for _, bad := range []error{
		f.db.CreateFoundrySlackInstallState(ctx, "", agentUser, sha("s"), time.Minute),
		f.db.CreateFoundrySlackInstallState(ctx, f.workspaceID, "", sha("s"), time.Minute),
		f.db.CreateFoundrySlackInstallState(ctx, f.workspaceID, agentUser, "state", time.Minute),
		f.db.CreateFoundrySlackInstallState(ctx, f.workspaceID, agentUser, sha("s"), 0),
	} {
		assert.Error(t, bad)
	}
	require.NoError(t, f.db.CreateFoundrySlackInstallState(ctx, f.workspaceID, agentUser, sha("s1"), 10*time.Minute))
	assert.ErrorIs(t, f.db.CreateFoundrySlackInstallState(ctx, f.workspaceID, agentUser, sha("s1"), 10*time.Minute), store.ErrConflict)

	_, err := f.db.SpendFoundrySlackInstallState(ctx, sha("s1"), "user-eve")
	assert.ErrorIs(t, err, store.ErrNotFound, "another member's")
	st, err := f.db.SpendFoundrySlackInstallState(ctx, sha("s1"), agentUser)
	require.NoError(t, err, "another member's try left it unspent")
	assert.Equal(t, f.workspaceID, st.WorkspaceID)
	assert.Equal(t, agentUser, st.UserID)
	_, err = f.db.SpendFoundrySlackInstallState(ctx, sha("s1"), agentUser)
	assert.ErrorIs(t, err, store.ErrNotFound, "once")

	require.NoError(t, f.db.CreateFoundrySlackInstallState(ctx, f.workspaceID, agentUser, sha("s2"), time.Minute))
	dbExec(t, `UPDATE foundry_slack_install_states SET expires_at=NOW(6) - INTERVAL 1 SECOND WHERE state_hash=?`, sha("s2"))
	_, err = f.db.SpendFoundrySlackInstallState(ctx, sha("s2"), agentUser)
	assert.ErrorIs(t, err, store.ErrNotFound, "expired")
	_, err = f.db.SpendFoundrySlackInstallState(ctx, sha("s3"), agentUser)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestFoundrySlackLinks(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "links")
	i := f.installed(t)
	for _, bad := range []error{
		f.db.CreateFoundrySlackLinkToken(ctx, "", "UADA", sha("t"), time.Minute),
		f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "@ada", sha("t"), time.Minute),
		f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UADA", "t", time.Minute),
		f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UADA", sha("t"), 0),
	} {
		assert.Error(t, bad)
	}
	assert.ErrorIs(t, f.db.CreateFoundrySlackLinkToken(ctx, f.installID(9), "UADA", sha("t"), time.Minute), store.ErrNotFound)

	// One live token a Slack user: a second one spends the first's chance.
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UADA", sha("t1"), 10*time.Minute))
	tok, err := f.db.GetFoundrySlackLinkToken(ctx, sha("t1"))
	require.NoError(t, err)
	assert.Equal(t, i.ID, tok.InstallID)
	assert.Equal(t, "UADA", tok.SlackUserID)
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UADA", sha("t2"), 10*time.Minute))
	_, err = f.db.GetFoundrySlackLinkToken(ctx, sha("t1"))
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UGRACE", sha("t2"), time.Minute), store.ErrConflict)

	_, err = f.db.LinkFoundrySlackUser(ctx, f.workspaceID, sha("t2"), "")
	assert.Error(t, err)
	_, err = f.db.LinkFoundrySlackUser(ctx, id26("SWS-links-other"), sha("t2"), agentUser)
	assert.ErrorIs(t, err, store.ErrNotFound, "another workspace's install")
	_, err = f.db.LinkFoundrySlackUser(ctx, f.workspaceID, sha("t1"), agentUser)
	assert.ErrorIs(t, err, store.ErrNotFound, "a replaced token")
	link, err := f.db.LinkFoundrySlackUser(ctx, f.workspaceID, sha("t2"), agentUser)
	require.NoError(t, err)
	assert.Equal(t, model.SlackLink{InstallID: i.ID, SlackUserID: "UADA", UserID: agentUser, CreatedAt: link.CreatedAt}, *link)
	_, err = f.db.LinkFoundrySlackUser(ctx, f.workspaceID, sha("t2"), agentUser)
	assert.ErrorIs(t, err, store.ErrNotFound, "spent once")
	_, err = f.db.GetFoundrySlackLinkToken(ctx, sha("t2"))
	assert.ErrorIs(t, err, store.ErrNotFound)

	// An expired token links no one.
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UGRACE", sha("t3"), time.Minute))
	dbExec(t, `UPDATE foundry_slack_link_tokens SET expires_at=NOW(6) - INTERVAL 1 SECOND WHERE token_hash=?`, sha("t3"))
	_, err = f.db.LinkFoundrySlackUser(ctx, f.workspaceID, sha("t3"), "user-grace")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Grace links; then Ada links Grace's Slack user: Grace's link and Ada's
	// earlier one both go. One Slack user, one Foundry user.
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UGRACE", sha("t4"), time.Minute))
	_, err = f.db.LinkFoundrySlackUser(ctx, f.workspaceID, sha("t4"), "user-grace")
	require.NoError(t, err)
	links, err := f.db.ListFoundrySlackLinks(ctx, i.ID)
	require.NoError(t, err)
	assert.Len(t, links, 2)
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UGRACE", sha("t5"), time.Minute))
	_, err = f.db.LinkFoundrySlackUser(ctx, f.workspaceID, sha("t5"), agentUser)
	require.NoError(t, err)
	links, err = f.db.ListFoundrySlackLinks(ctx, i.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, "UGRACE", links[0].SlackUserID)
	assert.Equal(t, agentUser, links[0].UserID)

	require.NoError(t, f.db.DeleteFoundrySlackLink(ctx, i.ID, "UGRACE"))
	assert.ErrorIs(t, f.db.DeleteFoundrySlackLink(ctx, i.ID, "UGRACE"), store.ErrNotFound)

	// A revoked install's tokens are dead.
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UADA", sha("t6"), time.Minute))
	dbExec(t, `UPDATE foundry_slack_installs SET revoked_at=NOW(6) WHERE id=?`, i.ID)
	_, err = f.db.GetFoundrySlackLinkToken(ctx, sha("t6"))
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UADA", sha("t7"), time.Minute), store.ErrNotFound)
}

func (f slackFixture) mapping(n int, installID, agentThread string) *model.SlackThread {
	return &model.SlackThread{ID: id26(fmt.Sprintf("SM-%s-%d-", f.tag, n)), InstallID: installID, ChannelID: "C1", ThreadTS: "1712345678.000100",
		SlackUserID: "UADA", AgentThreadID: agentThread}
}

func TestFoundrySlackThreads(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "threads")
	i := f.installed(t)
	require.NoError(t, f.send(1, 0))
	m := f.mapping(1, i.ID, f.threadID(1))
	for name, bad := range map[string]func(*model.SlackThread){
		"no id":                   func(m *model.SlackThread) { m.ID = "" },
		"no install":              func(m *model.SlackThread) { m.InstallID = "" },
		"no Founder thread":       func(m *model.SlackThread) { m.AgentThreadID = "" },
		"no channel":              func(m *model.SlackThread) { m.ChannelID = "" },
		"no thread":               func(m *model.SlackThread) { m.ThreadTS = "" },
		"a user that isn't an id": func(m *model.SlackThread) { m.SlackUserID = "@ada" },
	} {
		c := *m
		bad(&c)
		assert.Error(t, f.db.CreateFoundrySlackThread(ctx, &c), name)
	}
	_, err := f.db.GetFoundrySlackThread(ctx, i.ID, "C1", m.ThreadTS, "UADA")
	assert.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, f.db.CreateFoundrySlackThread(ctx, m))
	got, err := f.db.GetFoundrySlackThread(ctx, i.ID, "C1", m.ThreadTS, "UADA")
	require.NoError(t, err)
	assert.Equal(t, f.threadID(1), got.AgentThreadID)
	_, err = f.db.GetFoundrySlackThread(ctx, i.ID, "C1", m.ThreadTS, "UGRACE")
	assert.ErrorIs(t, err, store.ErrNotFound, "per person")

	// Never overwritten while the Founder thread lives.
	require.NoError(t, f.send(2, 0))
	assert.ErrorIs(t, f.db.CreateFoundrySlackThread(ctx, f.mapping(2, i.ID, f.threadID(2))), store.ErrConflict)
	got, err = f.db.GetFoundrySlackThread(ctx, i.ID, "C1", m.ThreadTS, "UADA")
	require.NoError(t, err)
	assert.Equal(t, f.threadID(1), got.AgentThreadID, "the first mapping is kept")
	dup := f.mapping(1, i.ID, f.threadID(2))
	dup.ThreadTS = "1712345678.000999"
	assert.ErrorIs(t, f.db.CreateFoundrySlackThread(ctx, dup), store.ErrConflict, "a taken id")

	// Touch keeps it from the purge.
	dbExec(t, `UPDATE foundry_slack_threads SET last_at=NOW(6) - INTERVAL 40 DAY WHERE id=?`, m.ID)
	require.NoError(t, f.db.TouchFoundrySlackThread(ctx, m.ID))
	assert.True(t, dbTrue(t, `SELECT last_at > NOW(6) - INTERVAL 1 MINUTE FROM foundry_slack_threads WHERE id=?`, m.ID))
	assert.ErrorIs(t, f.db.TouchFoundrySlackThread(ctx, id26("SM-none")), store.ErrNotFound)

	// A deleted Founder thread counts as none, and its mapping is replaced,
	// its Slack text purged first.
	f.settle(t, f.claimed(t))
	require.NoError(t, f.db.DeleteFoundryAgentThread(ctx, f.workspaceID, agentUser, f.threadID(1)))
	_, err = f.db.GetFoundrySlackThread(ctx, i.ID, "C1", m.ThreadTS, "UADA")
	assert.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, f.db.CreateFoundrySlackThread(ctx, f.mapping(3, i.ID, f.threadID(2))))
	got, err = f.db.GetFoundrySlackThread(ctx, i.ID, "C1", m.ThreadTS, "UADA")
	require.NoError(t, err)
	assert.Equal(t, f.threadID(2), got.AgentThreadID)
	assert.True(t, dbTrue(t, `SELECT input='' FROM foundry_agent_turns WHERE id=?`, f.turnID(1)), "the old thread's text is purged")
}

// purgeable is a Slack-born thread with everything the purge takes: a
// settled turn with events, transcript, an action and page context, and a
// live answers-free second turn.
func (f slackFixture) purgeable(t *testing.T, n int, installID string) *model.SlackThread {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.send(n, 0))
	turn := f.claimed(t)
	require.NoError(t, f.db.AppendFoundryAgentEvents(ctx, turn.ID, "w1", []json.RawMessage{json.RawMessage(`{"type":"text","text":"secret"}`)}))
	require.NoError(t, f.db.AppendFoundryAgentMessages(ctx, turn.ID, "w1", []model.FoundryAgentMessage{
		{Seq: 1, TurnID: turn.ID, Role: model.FoundryAgentRoleUser, Content: json.RawMessage(`[{"text":"secret"}]`), EstTokens: 1},
	}, 10))
	_, err := f.prepare(turn, n, time.Now().Add(time.Hour))
	require.NoError(t, err)
	f.settle(t, turn)
	dbExec(t, `UPDATE foundry_agent_turns SET page_context='{"page":"x"}' WHERE id=?`, turn.ID)
	m := f.mapping(n, installID, f.threadID(n))
	m.ThreadTS = fmt.Sprintf("1712345678.%06d", n)
	require.NoError(t, f.db.CreateFoundrySlackThread(ctx, m))
	return m
}

func purged(t *testing.T, threadID string) bool {
	t.Helper()
	return dbTrue(t, `SELECT
  (SELECT title='' AND deleted_at IS NOT NULL FROM foundry_agent_threads WHERE id=?)
  AND (SELECT COUNT(*)=0 FROM foundry_agent_messages WHERE thread_id=?)
  AND (SELECT COUNT(*)=0 FROM foundry_agent_actions WHERE thread_id=?)
  AND (SELECT COUNT(*)=0 FROM foundry_agent_events e JOIN foundry_agent_turns t ON t.id=e.turn_id WHERE t.thread_id=?)
  AND (SELECT COUNT(*)>0 AND SUM(input<>'' AND kind='message')=0 AND SUM(page_context IS NOT NULL)=0 AND SUM(asked IS NOT NULL)=0
       FROM foundry_agent_turns WHERE thread_id=?)
  AND (SELECT COUNT(*)=0 FROM foundry_slack_threads WHERE agent_thread_id=?)`, threadID, threadID, threadID, threadID, threadID, threadID)
}

func TestFoundrySlackThreadPurge(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "purge")
	i := f.installed(t)
	_, err := f.db.PurgeFoundrySlackThreads(ctx, 0, 10)
	assert.Error(t, err)
	_, err = f.db.PurgeFoundrySlackThreads(ctx, time.Hour, 0)
	assert.Error(t, err)

	idle := f.purgeable(t, 1, i.ID)
	fresh := f.purgeable(t, 2, i.ID)
	deleted := f.purgeable(t, 3, i.ID)
	dbExec(t, `UPDATE foundry_slack_threads SET last_at=NOW(6) - INTERVAL 31 DAY WHERE id=?`, idle.ID)
	require.NoError(t, f.db.DeleteFoundryAgentThread(ctx, f.workspaceID, agentUser, deleted.AgentThreadID))
	var spentBefore int64
	require.NoError(t, testDB.QueryRow(`SELECT SUM(usd_micros) FROM foundry_agent_turns WHERE workspace_id=?`, f.workspaceID).Scan(&spentBefore))

	n, err := f.db.PurgeFoundrySlackThreads(ctx, model.SlackThreadIdle, 10)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.True(t, purged(t, idle.AgentThreadID), "idle 30 days")
	assert.True(t, purged(t, deleted.AgentThreadID), "deleted on the site")
	assert.False(t, purged(t, fresh.AgentThreadID))
	_, err = f.db.GetFoundrySlackThread(ctx, i.ID, "C1", fresh.ThreadTS, "UADA")
	require.NoError(t, err, "a fresh thread stays")
	var spentAfter int64
	require.NoError(t, testDB.QueryRow(`SELECT SUM(usd_micros) FROM foundry_agent_turns WHERE workspace_id=?`, f.workspaceID).Scan(&spentAfter))
	assert.Equal(t, spentBefore, spentAfter, "the turns stay for the spend sums")
	threads, _, err := f.db.ListFoundryAgentThreads(ctx, f.workspaceID, agentUser, nil, "", 10)
	require.NoError(t, err)
	require.Len(t, threads, 1, "purged threads leave the person's list")
	assert.Equal(t, fresh.AgentThreadID, threads[0].ID)

	// A revoked install's threads are due at once, a live turn cancelled
	// with what it reserved charged.
	require.NoError(t, f.send(4, 2))
	live := f.claimed(t)
	require.Equal(t, f.turnID(4), live.ID)
	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, i.ID, true))
	n, err = f.db.PurgeFoundrySlackThreads(ctx, model.SlackThreadIdle, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.True(t, purged(t, fresh.AgentThreadID))
	cancelled, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, live.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentTurnCancelled, cancelled.Status)
	assert.Zero(t, cancelled.ReservedMicros)
	assert.EqualValues(t, 500_000, cancelled.Spend.USDMicros, "its reservation is charged")
	assert.ErrorIs(t, f.db.AppendFoundryAgentEvents(ctx, live.ID, "w1", []json.RawMessage{json.RawMessage(`{}`)}), store.ErrNotFound,
		"its worker lost it")
	n, err = f.db.PurgeFoundrySlackThreads(ctx, model.SlackThreadIdle, 10)
	require.NoError(t, err)
	assert.Zero(t, n, "nothing is purged twice")
}

// A mapping touched between the purge's read and its lock is kept.
func TestFoundrySlackThreadPurgeLimit(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "purgelimit")
	i := f.installed(t)
	for n := 1; n <= 3; n++ {
		m := f.purgeable(t, n, i.ID)
		dbExec(t, `UPDATE foundry_slack_threads SET last_at=NOW(6) - INTERVAL ? DAY WHERE id=?`, 40-n, m.ID)
	}
	n, err := f.db.PurgeFoundrySlackThreads(ctx, model.SlackThreadIdle, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "up to the limit, longest idle first")
	assert.True(t, purged(t, f.threadID(1)))
	assert.True(t, purged(t, f.threadID(2)))
	assert.False(t, purged(t, f.threadID(3)))
	n, err = f.db.PurgeFoundrySlackThreads(ctx, model.SlackThreadIdle, 2)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// --- Intake ---

func (f slackFixture) intakeID(n int) string { return id26(fmt.Sprintf("SN-%s-%03d-", f.tag, n)) }

// queue queues event n in conversation conv ("" for a click).
func (f slackFixture) queue(t *testing.T, installID string, n int, conv string) {
	t.Helper()
	in := &model.SlackIntake{ID: f.intakeID(n), InstallID: installID, DedupeKey: model.SlackEventDedupe(fmt.Sprintf("Ev%d", n)),
		Kind: model.SlackIntakeEvent, SlackUserID: "UADA", Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))}
	if conv != "" {
		in.Conversation = sha(conv)
	} else {
		in.Kind, in.DedupeKey = model.SlackIntakeInteraction, model.SlackClickDedupe("T1", "UADA", fmt.Sprint(n), "foundry.stop")
	}
	require.NoError(t, f.db.QueueFoundrySlackIntake(context.Background(), in))
}

func (f slackFixture) claim(t *testing.T) *model.SlackIntake {
	t.Helper()
	in, err := f.db.ClaimFoundrySlackIntake(context.Background(), time.Minute)
	require.NoError(t, err)
	return in
}

func (f slackFixture) claimedIntake(t *testing.T, n int) *model.SlackIntake {
	t.Helper()
	in := f.claim(t)
	require.NotNil(t, in, "intake %d is due", n)
	require.Equal(t, f.intakeID(n), in.ID)
	return in
}

func TestFoundrySlackIntakeQueue(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "queue")
	i := f.installed(t)
	f.queue(t, i.ID, 1, "conv-a")
	dup := &model.SlackIntake{ID: f.intakeID(2), InstallID: i.ID, DedupeKey: model.SlackEventDedupe("Ev1"), Kind: model.SlackIntakeEvent,
		Payload: json.RawMessage(`{}`)}
	assert.ErrorIs(t, f.db.QueueFoundrySlackIntake(ctx, dup), store.ErrConflict, "Slack's retry of the same event")
	dup.Kind = "message"
	assert.Error(t, f.db.QueueFoundrySlackIntake(ctx, dup))
	_, err := f.db.ClaimFoundrySlackIntake(ctx, 0)
	assert.Error(t, err)

	in := f.claimedIntake(t, 1)
	assert.Equal(t, model.SlackIntakeRunning, in.Status)
	assert.Equal(t, 1, in.Attempts)
	assert.Equal(t, model.SlackIntakeEvent, in.Kind)
	assert.Equal(t, sha("conv-a"), in.Conversation)
	assert.Equal(t, "UADA", in.SlackUserID)
	assert.JSONEq(t, `{"n":1}`, string(in.Payload))
	assert.Nil(t, in.Input)
	require.NotNil(t, in.LeaseOwner)
	assert.Len(t, *in.LeaseOwner, 32)
	assert.NotNil(t, in.LeaseUntil)
	assert.Nil(t, f.claim(t), "nothing else is due")

	// The first attempt keeps its input; a retry reads it back verbatim.
	require.NoError(t, f.db.SetFoundrySlackIntakeInput(ctx, in.ID, *in.LeaseOwner, "Which customers renewed?"))
	assert.ErrorIs(t, f.db.SetFoundrySlackIntakeInput(ctx, in.ID, *in.LeaseOwner, "again"), store.ErrNotFound, "set once")
	require.NoError(t, f.db.EndFoundrySlackIntake(ctx, in.ID, *in.LeaseOwner, model.SlackIntakeRetry{Error: "unavailable"}))
	again := f.claimedIntake(t, 1)
	require.NotNil(t, again.Input)
	assert.Equal(t, "Which customers renewed?", *again.Input)
	assert.Equal(t, 2, again.Attempts)
	require.NotNil(t, again.Error)
	assert.Equal(t, "unavailable", *again.Error)
	assert.NotEqual(t, *in.LeaseOwner, *again.LeaseOwner, "each claim its own owner")

	// Ends that no claim can have.
	for _, bad := range []model.SlackIntakeOutcome{
		nil, model.SlackIntakeGaveUp{}, model.SlackIntakeRetry{After: -time.Second, Error: "x"}, model.SlackIntakeRetry{},
		model.SlackIntakeWait{For: time.Minute}, model.SlackIntakeWait{TurnID: "t"},
	} {
		assert.Error(t, f.db.EndFoundrySlackIntake(ctx, again.ID, *again.LeaseOwner, bad), "%#v", bad)
	}
	require.NoError(t, f.db.EndFoundrySlackIntake(ctx, again.ID, *again.LeaseOwner, model.SlackIntakeGaveUp{Error: "Founder couldn't take it"}))
	assert.True(t, dbTrue(t, `SELECT status='failed' AND payload IS NULL AND finished_at IS NOT NULL AND lease_owner IS NULL
  AND error='Founder couldn''t take it' FROM foundry_slack_intake WHERE id=?`, again.ID))
	assert.ErrorIs(t, f.db.EndFoundrySlackIntake(ctx, again.ID, *again.LeaseOwner, model.SlackIntakeEnded{}), store.ErrNotFound, "ended once")
	assert.Nil(t, f.claim(t))

	// A retry later isn't due yet.
	f.queue(t, i.ID, 3, "conv-b")
	later := f.claimedIntake(t, 3)
	require.NoError(t, f.db.EndFoundrySlackIntake(ctx, later.ID, *later.LeaseOwner, model.SlackIntakeRetry{After: time.Hour, Error: "429"}))
	assert.Nil(t, f.claim(t))
	dbExec(t, `UPDATE foundry_slack_intake SET next_attempt_at=NOW(6) WHERE id=?`, later.ID)
	done := f.claimedIntake(t, 3)
	require.NoError(t, f.db.EndFoundrySlackIntake(ctx, done.ID, *done.LeaseOwner, model.SlackIntakeEnded{}))
	assert.True(t, dbTrue(t, `SELECT status='done' AND payload IS NULL AND error IS NULL FROM foundry_slack_intake WHERE id=?`, done.ID))
}

// A conversation's rows run one at a time, oldest first; other
// conversations and clicks don't wait for it.
func TestFoundrySlackIntakeConversationOrder(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "order")
	i := f.installed(t)
	f.queue(t, i.ID, 1, "conv-a")
	f.queue(t, i.ID, 2, "conv-a")
	f.queue(t, i.ID, 3, "conv-b")
	f.queue(t, i.ID, 4, "")
	f.queue(t, i.ID, 5, "")
	// The older row is due later: the newer still waits behind it.
	dbExec(t, `UPDATE foundry_slack_intake SET next_attempt_at=NOW(6) + INTERVAL 1 HOUR WHERE id=?`, f.intakeID(3))
	f.queue(t, i.ID, 6, "conv-b")

	first := f.claimedIntake(t, 1)
	f.claimedIntake(t, 4)
	f.claimedIntake(t, 5)
	assert.Nil(t, f.claim(t), "2 waits for 1, 6 for 3")
	require.NoError(t, f.db.EndFoundrySlackIntake(ctx, first.ID, *first.LeaseOwner, model.SlackIntakeEnded{}))
	f.claimedIntake(t, 2)
	assert.Nil(t, f.claim(t))
	dbExec(t, `UPDATE foundry_slack_intake SET next_attempt_at=NOW(6) WHERE id=?`, f.intakeID(3))
	third := f.claimedIntake(t, 3)
	assert.Nil(t, f.claim(t))
	require.NoError(t, f.db.EndFoundrySlackIntake(ctx, third.ID, *third.LeaseOwner, model.SlackIntakeGaveUp{Error: "x"}))
	f.claimedIntake(t, 6)
}

// A lease that ran out is reclaimed, by the conversation's oldest row first,
// and the first claim's writes then change nothing.
func TestFoundrySlackIntakeReclaim(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "reclaim")
	i := f.installed(t)
	f.queue(t, i.ID, 1, "conv-a")
	f.queue(t, i.ID, 2, "conv-a")
	dead := f.claimedIntake(t, 1)
	assert.Nil(t, f.claim(t), "a live lease holds the conversation")
	dbExec(t, `UPDATE foundry_slack_intake SET lease_until=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, dead.ID)
	alive := f.claimedIntake(t, 1)
	assert.Equal(t, 2, alive.Attempts)
	assert.NotEqual(t, *dead.LeaseOwner, *alive.LeaseOwner)

	turn := id26("AT-reclaim-none")
	for _, write := range []error{
		f.db.SetFoundrySlackIntakeInput(ctx, dead.ID, *dead.LeaseOwner, "x"),
		f.db.EndFoundrySlackIntake(ctx, dead.ID, *dead.LeaseOwner, model.SlackIntakeEnded{}),
		f.db.EndFoundrySlackIntake(ctx, dead.ID, *dead.LeaseOwner, model.SlackIntakeGaveUp{Error: "x"}),
		f.db.EndFoundrySlackIntake(ctx, dead.ID, *dead.LeaseOwner, model.SlackIntakeRetry{Error: "x"}),
		f.db.EndFoundrySlackIntake(ctx, dead.ID, *dead.LeaseOwner, model.SlackIntakeWait{TurnID: turn, For: time.Minute}),
	} {
		assert.ErrorIs(t, write, store.ErrNotFound, "a fenced write loses after the reclaim")
	}
	assert.True(t, dbTrue(t, `SELECT status='running' AND lease_owner=? FROM foundry_slack_intake WHERE id=?`, *alive.LeaseOwner, alive.ID))
	require.NoError(t, f.db.EndFoundrySlackIntake(ctx, alive.ID, *alive.LeaseOwner, model.SlackIntakeEnded{}))
	f.claimedIntake(t, 2)
}

// Workers claiming at once each take a row of their own, and a conversation
// one row at a time.
func TestFoundrySlackIntakeConcurrentClaims(t *testing.T) {
	f := newSlackFixture(t, "concurrent")
	i := f.installed(t)
	for n := 1; n <= 5; n++ {
		f.queue(t, i.ID, n, "conv-a")
	}
	for n := 6; n <= 8; n++ {
		f.queue(t, i.ID, n, fmt.Sprintf("conv-%d", n))
	}
	claimed := make([]*model.SlackIntake, 8)
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for w := range claimed {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed[w], errs[w] = f.db.ClaimFoundrySlackIntake(context.Background(), time.Minute)
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for w, in := range claimed {
		if errs[w] != nil {
			// A claim that lost a race for a lock may deadlock; the worker
			// claims again on its next poll.
			continue
		}
		if in == nil {
			continue
		}
		assert.False(t, seen[in.ID], "%s claimed twice", in.ID)
		seen[in.ID] = true
	}
	for n := 2; n <= 5; n++ {
		assert.False(t, seen[f.intakeID(n)], "conversation row %d ran beside its elder", n)
	}
	assert.LessOrEqual(t, len(seen), 4)
}

// A message that found its thread busy waits on the busy turn; the turn's
// end wakes it, and the conversation's later messages wait behind it.
func TestFoundrySlackIntakeWaitsOnTurn(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "wait")
	i := f.installed(t)
	require.NoError(t, f.send(1, 0))
	busy := f.turnID(1)
	f.queue(t, i.ID, 1, "conv-a")
	f.queue(t, i.ID, 2, "conv-a")
	in := f.claimedIntake(t, 1)
	require.NoError(t, f.db.EndFoundrySlackIntake(ctx, in.ID, *in.LeaseOwner, model.SlackIntakeWait{TurnID: busy, For: 15 * time.Minute}))
	assert.True(t, dbTrue(t, `SELECT status='queued' AND waits_on_turn=? AND next_attempt_at > NOW(6) + INTERVAL 14 MINUTE AND lease_owner IS NULL
  FROM foundry_slack_intake WHERE id=?`, busy, in.ID))
	assert.Nil(t, f.claim(t), "waiting, and 2 waits behind it")

	woken, err := f.db.WakeFoundrySlackIntake(ctx, f.turnID(9))
	require.NoError(t, err)
	assert.Zero(t, woken)
	woken, err = f.db.WakeFoundrySlackIntake(ctx, busy)
	require.NoError(t, err)
	assert.EqualValues(t, 1, woken)
	back := f.claimedIntake(t, 1)
	assert.Nil(t, back.WaitsOnTurn, "woken: the wait is cleared")

	// A turn that ended before the wait was written wakes it at once.
	f.settle(t, f.claimed(t))
	require.NoError(t, f.db.EndFoundrySlackIntake(ctx, back.ID, *back.LeaseOwner, model.SlackIntakeWait{TurnID: busy, For: 15 * time.Minute}))
	again := f.claimedIntake(t, 1)
	assert.Nil(t, again.WaitsOnTurn)

	// A wait that runs out is claimed with its turn still named.
	require.NoError(t, f.send(2, 1))
	require.NoError(t, f.db.EndFoundrySlackIntake(ctx, again.ID, *again.LeaseOwner, model.SlackIntakeWait{TurnID: f.turnID(2), For: time.Minute}))
	dbExec(t, `UPDATE foundry_slack_intake SET next_attempt_at=NOW(6) WHERE id=?`, again.ID)
	ranOut := f.claimedIntake(t, 1)
	require.NotNil(t, ranOut.WaitsOnTurn)
	assert.Equal(t, f.turnID(2), *ranOut.WaitsOnTurn)
	require.NoError(t, f.db.EndFoundrySlackIntake(ctx, ranOut.ID, *ranOut.LeaseOwner, model.SlackIntakeEnded{}))
	assert.True(t, dbTrue(t, `SELECT waits_on_turn IS NULL FROM foundry_slack_intake WHERE id=?`, ranOut.ID))
	f.claimedIntake(t, 2)
}

// --- Replies ---

func (f slackFixture) reply(installID string, n int) *model.SlackReply {
	return &model.SlackReply{TurnID: f.turnID(n), InstallID: installID, ChannelID: "C1", ThreadTS: "1712345678.000100", SlackUserID: "UADA"}
}

func (f slackFixture) claimReply(t *testing.T) *model.SlackReply {
	t.Helper()
	r, err := f.db.ClaimFoundrySlackReply(context.Background(), time.Minute)
	require.NoError(t, err)
	return r
}

func TestFoundrySlackReplies(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "replies")
	i := f.installed(t)
	bad := f.reply(i.ID, 1)
	bad.ChannelID = ""
	_, err := f.db.HoldFoundrySlackReply(ctx, bad)
	assert.Error(t, err)
	_, err = f.db.ClaimFoundrySlackReply(ctx, 0)
	assert.Error(t, err)

	// Held: no follower takes it before the intake worker posts.
	f.queue(t, i.ID, 1, "conv-a")
	in := f.claimedIntake(t, 1)
	held, err := f.db.HoldFoundrySlackReply(ctx, f.reply(i.ID, 1))
	require.NoError(t, err)
	assert.Equal(t, model.SlackReplyFollowing, held.Status)
	assert.Nil(t, held.MessageTS)
	assert.True(t, dbTrue(t, `SELECT next_poll_at > NOW(6) + INTERVAL 59 MINUTE FROM foundry_slack_replies WHERE turn_id=?`, held.TurnID))
	assert.Nil(t, f.claimReply(t))
	assert.Error(t, f.db.StartFoundrySlackReply(ctx, held.TurnID, "not a ts", in.ID, *in.LeaseOwner))
	assert.ErrorIs(t, f.db.StartFoundrySlackReply(ctx, held.TurnID, "1712345678.000200", in.ID, "someone-else"), store.ErrNotFound,
		"fenced on the intake's lease")
	require.NoError(t, f.db.StartFoundrySlackReply(ctx, held.TurnID, "1712345678.000200", in.ID, *in.LeaseOwner))
	assert.ErrorIs(t, f.db.StartFoundrySlackReply(ctx, held.TurnID, "1712345678.000300", in.ID, *in.LeaseOwner), store.ErrNotFound,
		"its first message once")
	again, err := f.db.HoldFoundrySlackReply(ctx, f.reply(i.ID, 1))
	require.NoError(t, err)
	require.NotNil(t, again.MessageTS, "a retry finds the message posted")
	assert.Equal(t, "1712345678.000200", *again.MessageTS)

	// Leased for the whole follow.
	r := f.claimReply(t)
	require.NotNil(t, r)
	require.NotNil(t, r.LeaseOwner)
	owner := *r.LeaseOwner
	assert.Nil(t, f.claimReply(t), "held by its follower")
	follows, err := f.db.ListFoundrySlackFollows(ctx, i.ID, "C1", "1712345678.000100")
	require.NoError(t, err)
	require.Len(t, follows, 1)
	count, err := f.db.CountFoundrySlackFollows(ctx, i.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	seq, wait := 4, 3*time.Second
	assert.Error(t, f.db.UpdateFoundrySlackReply(ctx, r.TurnID, owner, model.SlackReplyUpdate{}), "no lease")
	require.NoError(t, f.db.UpdateFoundrySlackReply(ctx, r.TurnID, owner, model.SlackReplyUpdate{Lease: time.Minute, AfterSeq: &seq,
		Posted: map[string]string{"q:01Q": model.SlackPostPending}, UpdateAfter: &wait}))
	ts := "1712345678.000400"
	require.NoError(t, f.db.UpdateFoundrySlackReply(ctx, r.TurnID, owner, model.SlackReplyUpdate{Lease: time.Minute, MessageTS: &ts,
		Posted: map[string]string{"q:01Q": "1712345678.000500", "a:01A": model.SlackPostPending}}))
	got, err := f.db.GetFoundrySlackReply(ctx, r.TurnID)
	require.NoError(t, err)
	assert.Equal(t, 4, got.AfterSeq)
	assert.Equal(t, ts, *got.MessageTS)
	assert.Equal(t, map[string]string{"q:01Q": "1712345678.000500", "a:01A": model.SlackPostPending}, got.Posted)
	assert.True(t, dbTrue(t, `SELECT next_update_at > NOW(6) + INTERVAL 2 SECOND AND lease_until > NOW(6) + INTERVAL 50 SECOND
  FROM foundry_slack_replies WHERE turn_id=?`, r.TurnID), "the next update spaced, the lease renewed")

	// The follower died: its lease runs out, another takes over, and the
	// first's writes change nothing.
	dbExec(t, `UPDATE foundry_slack_replies SET lease_until=NOW(6) - INTERVAL 1 SECOND WHERE turn_id=?`, r.TurnID)
	taken := f.claimReply(t)
	require.NotNil(t, taken)
	assert.Equal(t, r.TurnID, taken.TurnID)
	assert.Equal(t, map[string]string{"q:01Q": "1712345678.000500", "a:01A": model.SlackPostPending}, taken.Posted,
		"the pending post is posted again")
	for _, write := range []error{
		f.db.UpdateFoundrySlackReply(ctx, r.TurnID, owner, model.SlackReplyUpdate{Lease: time.Minute, AfterSeq: &seq}),
		f.db.ReleaseFoundrySlackReply(ctx, r.TurnID, owner, 0),
		f.db.FinishFoundrySlackReply(ctx, r.TurnID, owner),
	} {
		assert.ErrorIs(t, write, store.ErrNotFound)
	}

	// Rate limited: released until Retry-After.
	assert.Error(t, f.db.ReleaseFoundrySlackReply(ctx, r.TurnID, *taken.LeaseOwner, -time.Second))
	require.NoError(t, f.db.ReleaseFoundrySlackReply(ctx, r.TurnID, *taken.LeaseOwner, 30*time.Second))
	assert.Nil(t, f.claimReply(t))
	dbExec(t, `UPDATE foundry_slack_replies SET next_poll_at=NOW(6) WHERE turn_id=?`, r.TurnID)
	last := f.claimReply(t)
	require.NotNil(t, last)
	require.NoError(t, f.db.FinishFoundrySlackReply(ctx, r.TurnID, *last.LeaseOwner))
	done, err := f.db.GetFoundrySlackReply(ctx, r.TurnID)
	require.NoError(t, err)
	assert.Equal(t, model.SlackReplyDone, done.Status)
	assert.NotNil(t, done.FinishedAt)
	assert.Nil(t, done.LeaseOwner)
	follows, err = f.db.ListFoundrySlackFollows(ctx, i.ID, "C1", "1712345678.000100")
	require.NoError(t, err)
	assert.Empty(t, follows)
	count, err = f.db.CountFoundrySlackFollows(ctx, i.ID)
	require.NoError(t, err)
	assert.Zero(t, count)
	assert.Nil(t, f.claimReply(t))
	_, err = f.db.GetFoundrySlackReply(ctx, f.turnID(9))
	assert.ErrorIs(t, err, store.ErrNotFound)

	// A held reply whose intake worker died is taken over once the hold runs out.
	_, err = f.db.HoldFoundrySlackReply(ctx, f.reply(i.ID, 2))
	require.NoError(t, err)
	dbExec(t, `UPDATE foundry_slack_replies SET next_poll_at=NOW(6) - INTERVAL 1 SECOND WHERE turn_id=?`, f.turnID(2))
	orphan := f.claimReply(t)
	require.NotNil(t, orphan)
	assert.Nil(t, orphan.MessageTS, "its follower posts the first message")
}

// --- Retention and a deleted workspace ---

func TestFoundrySlackPrune(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "prune")
	i := f.installed(t)
	_, err := f.db.PruneFoundrySlack(ctx, 0)
	assert.Error(t, err)
	f.queue(t, i.ID, 1, "conv-a")
	f.queue(t, i.ID, 2, "conv-b")
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UADA", sha("old"), time.Minute))
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UGRACE", sha("new"), time.Minute))
	require.NoError(t, f.db.CreateFoundrySlackInstallState(ctx, f.workspaceID, agentUser, sha("old"), time.Minute))
	require.NoError(t, f.db.CreateFoundrySlackInstallState(ctx, f.workspaceID, agentUser, sha("new"), time.Minute))
	for n := 1; n <= 2; n++ {
		_, err := f.db.HoldFoundrySlackReply(ctx, f.reply(i.ID, n))
		require.NoError(t, err)
	}
	dbExec(t, `UPDATE foundry_slack_intake SET created_at=NOW(6) - INTERVAL 8 DAY WHERE id=?`, f.intakeID(1))
	dbExec(t, `UPDATE foundry_slack_link_tokens SET expires_at=NOW(6) - INTERVAL 25 HOUR WHERE token_hash=?`, sha("old"))
	dbExec(t, `UPDATE foundry_slack_install_states SET expires_at=NOW(6) - INTERVAL 25 HOUR WHERE state_hash=?`, sha("old"))
	dbExec(t, `UPDATE foundry_slack_replies SET status='done', finished_at=NOW(6) - INTERVAL 8 DAY WHERE turn_id=?`, f.turnID(1))
	dbExec(t, `UPDATE foundry_slack_replies SET created_at=NOW(6) - INTERVAL 8 DAY WHERE turn_id=?`, f.turnID(2))

	n, err := f.db.PruneFoundrySlack(ctx, 100)
	require.NoError(t, err)
	assert.EqualValues(t, 4, n)
	assert.True(t, dbTrue(t, `SELECT
  (SELECT COUNT(*)=1 FROM foundry_slack_intake) AND (SELECT COUNT(*)=1 FROM foundry_slack_link_tokens WHERE token_hash=?)
  AND (SELECT COUNT(*)=1 FROM foundry_slack_install_states WHERE state_hash=?)
  AND (SELECT COUNT(*)=1 FROM foundry_slack_replies WHERE turn_id=?)`, sha("new"), sha("new"), f.turnID(2)),
		"what is young, and a reply still followed, stay")
	n, err = f.db.PruneFoundrySlack(ctx, 100)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestFoundrySlackPurgeWorkspace(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "purgews")
	i := f.installed(t)
	other := id26("SWS-purgews-other")
	createWorkspace(t, other)
	kept, _, err := f.db.InstallFoundrySlack(ctx, f.install(2, other, "T2"), sealAs("x"))
	require.NoError(t, err)
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, kept.ID, "UADA", sha("kept"), time.Minute))

	require.NoError(t, f.db.CreateFoundrySlackInstallState(ctx, f.workspaceID, agentUser, sha("s"), time.Minute))
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UADA", sha("t1"), time.Minute))
	_, err = f.db.LinkFoundrySlackUser(ctx, f.workspaceID, sha("t1"), agentUser)
	require.NoError(t, err)
	f.queue(t, i.ID, 1, "conv-a")
	_, err = f.db.HoldFoundrySlackReply(ctx, f.reply(i.ID, 1))
	require.NoError(t, err)
	m := f.purgeable(t, 1, i.ID)

	require.NoError(t, store.InTx(testDB, hostWorkspaces{}).PurgeFoundrySlack(ctx, f.workspaceID), "its host may run it in its own transaction")
	require.NoError(t, f.db.PurgeFoundrySlack(ctx, f.workspaceID), "again changes nothing")
	_, err = f.db.GetFoundrySlackInstall(ctx, f.workspaceID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	pending, err := f.db.ListFoundrySlackUninstalls(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the sweep tells Slack")
	assert.Equal(t, i.ID, pending[0].ID)
	_, err = f.db.GetFoundrySlackInstallToken(ctx, i.ID)
	require.NoError(t, err)
	assert.True(t, dbTrue(t, `SELECT
  (SELECT COUNT(*)=0 FROM foundry_slack_install_states WHERE workspace_id=?) AND (SELECT COUNT(*)=0 FROM foundry_slack_links WHERE install_id=?)
  AND (SELECT COUNT(*)=0 FROM foundry_slack_link_tokens WHERE install_id=?) AND (SELECT COUNT(*)=0 FROM foundry_slack_intake WHERE install_id=?)
  AND (SELECT COUNT(*)=0 FROM foundry_slack_replies WHERE install_id=?)`, f.workspaceID, i.ID, i.ID, i.ID, i.ID))
	_, err = f.db.GetFoundrySlackLinkToken(ctx, sha("kept"))
	require.NoError(t, err, "another workspace's install is untouched")

	n, err := f.db.PurgeFoundrySlackThreads(ctx, model.SlackThreadIdle, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.True(t, purged(t, m.AgentThreadID))
}

// --- Turns' surface ---

func TestFoundryAgentTurnSurface(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "surface")
	turn := func(n int, surface model.FoundryAgentSurface) *model.FoundryAgentTurn {
		return &model.FoundryAgentTurn{ID: f.turnID(n), WorkspaceID: f.workspaceID, ThreadID: f.threadID(n), UserID: agentUser,
			Kind: model.FoundryAgentTurnMessage, Surface: surface, Input: "hi", MaxUSDMicros: 500_000}
	}
	thread := func(n int) *model.FoundryAgentThread {
		return &model.FoundryAgentThread{ID: f.threadID(n), WorkspaceID: f.workspaceID, UserID: agentUser, Title: "Hi", Runtime: model.FoundryAgentRuntimeAPI}
	}
	assert.Error(t, f.db.CreateFoundryAgentTurn(ctx, turn(1, model.FoundryAgentSurface(9)), thread(1), f.limits(), nil), "an unknown surface")
	require.NoError(t, f.db.CreateFoundryAgentTurn(ctx, turn(1, model.FoundryAgentSurfaceSlack), thread(1), f.limits(), nil))
	require.NoError(t, f.db.CreateFoundryAgentTurn(ctx, turn(2, model.FoundryAgentSurfaceSite), thread(2), f.limits(), nil))
	for n, want := range map[int]model.FoundryAgentSurface{1: model.FoundryAgentSurfaceSlack, 2: model.FoundryAgentSurfaceSite} {
		got, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, f.turnID(n))
		require.NoError(t, err)
		assert.Equal(t, want, got.Surface)
	}

	// An action prepared in Slack, run from the site: its continuation is
	// read in Slack.
	slackTurn := f.claimed(t)
	require.Equal(t, f.turnID(1), slackTurn.ID)
	a, err := f.prepare(slackTurn, 1, time.Now().Add(time.Hour))
	require.NoError(t, err)
	f.settle(t, slackTurn)
	_, err = f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, a.PayloadHash)
	require.NoError(t, err)
	next := &model.FoundryAgentTurn{ID: f.turnID(3), WorkspaceID: f.workspaceID, ThreadID: slackTurn.ThreadID, UserID: agentUser,
		Kind: model.FoundryAgentTurnActionResult, Surface: model.FoundryAgentSurfaceSite, Input: a.ID, MaxUSDMicros: 500_000}
	require.NoError(t, f.db.FinishFoundryAgentAction(ctx, a.ID, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionSucceeded,
		Result: json.RawMessage(`{}`)}, next, json.RawMessage(`{"type":"action_result"}`)))
	assert.Equal(t, model.FoundryAgentSurfaceSlack, next.Surface, "the caller learns the surface copied")
	cont, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, next.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentSurfaceSlack, cont.Surface)
	assert.Equal(t, model.FoundryAgentTurnQueued, cont.Status)

	// A row with a surface no binary knows fails its read.
	dbExec(t, `UPDATE foundry_agent_turns SET surface='email' WHERE id=?`, f.turnID(2))
	_, err = f.db.GetFoundryAgentTurn(ctx, f.workspaceID, f.turnID(2))
	assert.ErrorContains(t, err, "unknown foundry agent surface")
}
