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

// relayVerifier is a PKCE verifier of the tests' codes.
var relayVerifier = strings.Repeat("v", 43)

func (f slackFixture) relayCode(n int, workspaceID string) *model.RelayCode {
	return &model.RelayCode{CodeHash: sha(fmt.Sprintf("code-%s-%d", f.tag, n)), WorkspaceID: workspaceID, UserID: agentUser,
		CodeChallenge: model.RelayCodeChallenge(relayVerifier), RedirectURI: "http://localhost:3000/settings/slack", Name: fmt.Sprintf("Ada's Mac %d", n)}
}

func (f slackFixture) linkID(n int) string { return id26(fmt.Sprintf("RL-%s-%d-", f.tag, n)) }

// linked links local install n to workspaceID, as its code's redeem does.
func (f slackFixture) linked(t *testing.T, n int, workspaceID string) *model.RelayLink {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.db.CreateFoundryRelayCode(ctx, f.relayCode(n, workspaceID), 10*time.Minute))
	l, err := f.db.RedeemFoundryRelayCode(ctx, f.relayCode(n, workspaceID).CodeHash, relayVerifier, f.linkID(n), sha(fmt.Sprintf("token-%s-%d", f.tag, n)))
	require.NoError(t, err)
	return l
}

// relayed installs team for local workspace local through link.
func (f slackFixture) relayed(t *testing.T, n int, link *model.RelayLink, local, team string) *model.SlackInstall {
	t.Helper()
	in := f.install(n, link.WorkspaceID, team)
	in.Destination = model.SlackServesRelay{LinkID: link.ID, LocalWorkspaceID: local}
	i, reinstalled, err := f.db.InstallFoundrySlack(context.Background(), in, sealAs("xoxb-relay"))
	require.NoError(t, err)
	require.False(t, reinstalled)
	return i
}

func (f slackFixture) delivery(n int, installID, conversation string) *model.RelayDelivery {
	return &model.RelayDelivery{ID: id26(fmt.Sprintf("RD-%s-%d-", f.tag, n)), InstallID: installID, DedupeKey: model.SlackEventDedupe(fmt.Sprintf("Ev%s%d", f.tag, n)),
		Kind: model.SlackIntakeEvent, Conversation: conversation, SlackUserID: "UADA", Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))}
}

func TestFoundryRelayLinks(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "rlink")
	other := id26("SWS-rlink-other")
	createWorkspace(t, other)

	// Codes are checked, kept once each, and spent once, with their verifier.
	bad := f.relayCode(1, f.workspaceID)
	bad.RedirectURI = "https://evil.example/settings/slack"
	assert.Error(t, f.db.CreateFoundryRelayCode(ctx, bad, time.Minute))
	assert.Error(t, f.db.CreateFoundryRelayCode(ctx, f.relayCode(1, f.workspaceID), 0))
	require.NoError(t, f.db.CreateFoundryRelayCode(ctx, f.relayCode(1, f.workspaceID), time.Minute))
	assert.ErrorIs(t, f.db.CreateFoundryRelayCode(ctx, f.relayCode(1, f.workspaceID), time.Minute), store.ErrConflict)
	code := f.relayCode(1, f.workspaceID).CodeHash
	for _, args := range [][4]string{
		{"nothex", relayVerifier, f.linkID(1), sha("t1")}, {code, "short", f.linkID(1), sha("t1")},
		{code, relayVerifier, "", sha("t1")}, {code, relayVerifier, f.linkID(1), "t1"},
	} {
		_, err := f.db.RedeemFoundryRelayCode(ctx, args[0], args[1], args[2], args[3])
		assert.Error(t, err)
		assert.NotErrorIs(t, err, store.ErrNotFound, "bad input is the caller's mistake: %v", args)
	}
	_, err := f.db.RedeemFoundryRelayCode(ctx, code, strings.Repeat("w", 43), f.linkID(1), sha("t1"))
	assert.ErrorIs(t, err, store.ErrNotFound, "another verifier")
	link, err := f.db.RedeemFoundryRelayCode(ctx, code, relayVerifier, f.linkID(1), sha("t1"))
	require.NoError(t, err, "a wrong verifier leaves the code unspent")
	assert.Equal(t, model.RelayLink{ID: f.linkID(1), WorkspaceID: f.workspaceID, CreatedByUserID: agentUser, Name: "Ada's Mac 1",
		RedirectOrigin: "http://localhost:3000", CreatedAt: link.CreatedAt}, *link)
	_, err = f.db.RedeemFoundryRelayCode(ctx, code, relayVerifier, f.linkID(2), sha("t2"))
	assert.ErrorIs(t, err, store.ErrNotFound, "spent")

	// An expired code, a deleted workspace's, and a token taken redeem nothing.
	require.NoError(t, f.db.CreateFoundryRelayCode(ctx, f.relayCode(2, f.workspaceID), time.Minute))
	dbExec(t, `UPDATE foundry_relay_codes SET expires_at=NOW(6) - INTERVAL 1 SECOND WHERE code_hash=?`, f.relayCode(2, f.workspaceID).CodeHash)
	_, err = f.db.RedeemFoundryRelayCode(ctx, f.relayCode(2, f.workspaceID).CodeHash, relayVerifier, f.linkID(2), sha("t2"))
	assert.ErrorIs(t, err, store.ErrNotFound, "expired")
	gone := id26("SWS-rlink-gone")
	createWorkspace(t, gone)
	require.NoError(t, f.db.CreateFoundryRelayCode(ctx, f.relayCode(3, gone), time.Minute))
	deleteWorkspace(t, gone)
	_, err = f.db.RedeemFoundryRelayCode(ctx, f.relayCode(3, gone).CodeHash, relayVerifier, f.linkID(3), sha("t3"))
	assert.ErrorIs(t, err, store.ErrNotFound, "its workspace is gone")
	require.NoError(t, f.db.CreateFoundryRelayCode(ctx, f.relayCode(4, f.workspaceID), time.Minute))
	_, err = f.db.RedeemFoundryRelayCode(ctx, f.relayCode(4, f.workspaceID).CodeHash, relayVerifier, f.linkID(4), sha("t1"))
	assert.ErrorIs(t, err, store.ErrConflict)
	assert.True(t, dbTrue(t, `SELECT used_at IS NULL FROM foundry_relay_codes WHERE code_hash=?`, f.relayCode(4, f.workspaceID).CodeHash),
		"a refused redeem spends nothing")

	// Its token finds it while it lives; its workspace lists it.
	got, err := f.db.GetFoundryRelayLinkByToken(ctx, sha("t1"))
	require.NoError(t, err)
	assert.Equal(t, link.ID, got.ID)
	assert.Equal(t, "http://localhost:3000/settings/slack", got.ReturnTo())
	second := f.linked(t, 5, f.workspaceID)
	f.linked(t, 6, other)
	links, err := f.db.ListFoundryRelayLinks(ctx, f.workspaceID)
	require.NoError(t, err)
	require.Len(t, links, 2)
	assert.Equal(t, []string{second.ID, link.ID}, []string{links[0].ID, links[1].ID}, "newest first")

	// Contact is written at most once a minute.
	assert.Error(t, f.db.SeeFoundryRelayLink(ctx, link.ID, strings.Repeat("1", 65)))
	require.NoError(t, f.db.SeeFoundryRelayLink(ctx, link.ID, "203.0.113.7"))
	require.NoError(t, f.db.SeeFoundryRelayLink(ctx, link.ID, "203.0.113.8"))
	got, err = f.db.GetFoundryRelayLinkByToken(ctx, sha("t1"))
	require.NoError(t, err)
	require.NotNil(t, got.LastSeenAt)
	assert.Equal(t, "203.0.113.7", got.LastSeenAddr, "within the minute")
	dbExec(t, `UPDATE foundry_relay_links SET last_seen_at=NOW(6) - INTERVAL 61 SECOND WHERE id=?`, link.ID)
	require.NoError(t, f.db.SeeFoundryRelayLink(ctx, link.ID, "203.0.113.8"))
	got, err = f.db.GetFoundryRelayLinkByToken(ctx, sha("t1"))
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.8", got.LastSeenAddr)

	// Revoking it, from its own workspace only, ends it and what it served.
	i := f.relayed(t, 1, link, "local-1", "T1")
	require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(1, i.ID, ""), time.Minute, model.RelaySeen{ChannelID: "C1", UserIDs: []string{"UADA"}}))
	require.NoError(t, f.db.SetFoundryRelayFeeds(ctx, link.ID, []model.RelayFeedOn{{LocalWorkspaceID: "local-1", On: true}}))
	_, err = f.db.AppendFoundryRelayFeed(ctx, i.ID, model.RelayFeedAppend{Rows: []model.SlackFeedRow{relayFeedRow("Ev1", "C1")}, At: time.Now()}, relayLimits)
	require.NoError(t, err)
	require.NoError(t, f.db.CreateFoundrySlackRelayInstallState(ctx, link.ID, "local-2", sha("state-r1"), time.Minute))
	assert.ErrorIs(t, f.db.RevokeFoundryRelayLink(ctx, other, link.ID), store.ErrNotFound)
	require.NoError(t, f.db.RevokeFoundryRelayLink(ctx, f.workspaceID, link.ID))
	assert.ErrorIs(t, f.db.RevokeFoundryRelayLink(ctx, f.workspaceID, link.ID), store.ErrNotFound)
	_, err = f.db.GetFoundryRelayLinkByToken(ctx, sha("t1"))
	assert.ErrorIs(t, err, store.ErrNotFound)
	revoked, err := f.db.GetFoundrySlackInstallByID(ctx, i.ID)
	require.NoError(t, err)
	assert.NotNil(t, revoked.RevokedAt)
	assert.Nil(t, revoked.UninstalledAt, "the sweep tells Slack")
	uninstalls, err := f.db.ListFoundrySlackUninstalls(ctx, 10)
	require.NoError(t, err)
	require.Len(t, uninstalls, 1)
	assert.Equal(t, i.ID, uninstalls[0].ID)
	assert.True(t, dbTrue(t, `SELECT status='expired' AND payload IS NULL AND noticed_at IS NULL FROM foundry_relay_deliveries WHERE install_id=?`, i.ID))
	expired, err := f.db.ExpireFoundryRelayDeliveries(ctx, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, expired, "a revoke's deliveries are told nothing")
	for _, table := range []string{"foundry_relay_feed", "foundry_relay_channels", "foundry_relay_users"} {
		assert.True(t, dbTrue(t, `SELECT COUNT(*)=0 FROM `+table+` WHERE install_id=?`, i.ID), table)
	}
	_, err = f.db.SpendFoundrySlackInstallState(ctx, sha("state-r1"), agentUser)
	assert.ErrorIs(t, err, store.ErrNotFound, "an install the link started is forgotten")
}

func TestFoundryRelayInstalls(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "rinst")
	link := f.linked(t, 1, f.workspaceID)

	// A relayed install serves its link's local workspace, beside the
	// workspace's own.
	relayed := f.relayed(t, 1, link, "local-1", "T1")
	assert.Equal(t, model.SlackServesRelay{LinkID: link.ID, LocalWorkspaceID: "local-1"}, relayed.Destination)
	_, err := f.db.GetFoundrySlackInstall(ctx, f.workspaceID)
	assert.ErrorIs(t, err, store.ErrNotFound, "a relayed install isn't the workspace's own")
	own, reinstalled, err := f.db.InstallFoundrySlack(ctx, f.install(2, f.workspaceID, "T2"), sealAs("xoxb-own"))
	require.NoError(t, err)
	require.False(t, reinstalled)
	assert.Equal(t, model.SlackServesWorkspace{}, own.Destination)
	mine, err := f.db.GetFoundrySlackInstall(ctx, f.workspaceID)
	require.NoError(t, err)
	assert.Equal(t, own.ID, mine.ID)
	byTeam, err := f.db.GetFoundrySlackInstallByTeam(ctx, "T1")
	require.NoError(t, err)
	assert.Equal(t, relayed.Destination, byTeam.Destination, "Slack's requests find the relayed install")
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=2 FROM foundry_slack_slots WHERE install_id=?`, relayed.ID), "relayed installs have their slots")
	f.relayed(t, 3, link, "local-2", "T3")

	// The rules hold per destination.
	relay := func(n int, l *model.RelayLink, local, team string) (*model.SlackInstall, bool, error) {
		in := f.install(n, f.workspaceID, team)
		in.Destination = model.SlackServesRelay{LinkID: l.ID, LocalWorkspaceID: local}
		return f.db.InstallFoundrySlack(ctx, in, sealAs("x"))
	}
	_, _, err = relay(4, link, "local-1", "T4")
	assert.Equal(t, model.SlackInstalled, slackRefusal(t, err), "the local workspace holds a team")
	_, _, err = relay(4, link, "local-3", "T1")
	assert.Equal(t, model.SlackTeamTaken, slackRefusal(t, err), "the team serves another local workspace")
	_, _, err = f.db.InstallFoundrySlack(ctx, f.install(4, f.workspaceID, "T4"), sealAs("x"))
	assert.Equal(t, model.SlackInstalled, slackRefusal(t, err), "the workspace's own holds T2")
	same, reinstalled, err := relay(5, link, "local-1", "T1")
	require.NoError(t, err)
	assert.True(t, reinstalled, "the same team for the same local workspace, through the same link")
	assert.Equal(t, relayed.ID, same.ID)
	// Through another link (the old one's revoke at Helix may have failed),
	// the install is new: the old one is gone here, and the new link's local
	// install mirrors one it never revoked.
	relinked := f.linked(t, 2, f.workspaceID)
	again, reinstalled, err := relay(6, relinked, "local-1", "T1")
	require.NoError(t, err)
	assert.False(t, reinstalled, "a new install through the newer link")
	assert.NotEqual(t, relayed.ID, again.ID)
	assert.Equal(t, model.SlackServesRelay{LinkID: relinked.ID, LocalWorkspaceID: "local-1"}, again.Destination, "through the newer link")
	assert.True(t, dbTrue(t, `SELECT revoked_at IS NOT NULL AND uninstalled_at IS NOT NULL FROM foundry_slack_installs WHERE id=?`, relayed.ID),
		"the old install is revoked, with no apps.uninstall to send: Slack keeps the app for the new one")
	assert.Zero(t, dbCount(t, `SELECT COUNT(*) FROM foundry_slack_install_tokens WHERE install_id=?`, relayed.ID))
	oldLink, err := f.db.ListFoundryRelayInstalls(ctx, link.ID)
	require.NoError(t, err)
	assert.Len(t, oldLink, 1, "the old link keeps only its other install")

	// A link must be the workspace's and live.
	other := id26("SWS-rinst-other")
	createWorkspace(t, other)
	foreign := f.linked(t, 3, other)
	_, _, err = relay(7, foreign, "local-9", "T9")
	assert.ErrorIs(t, err, store.ErrNotFound, "another workspace's link")
	require.NoError(t, f.db.RevokeFoundryRelayLink(ctx, other, foreign.ID))
	in := f.install(7, other, "T9")
	in.Destination = model.SlackServesRelay{LinkID: foreign.ID, LocalWorkspaceID: "local-9"}
	_, _, err = f.db.InstallFoundrySlack(ctx, in, sealAs("x"))
	assert.ErrorIs(t, err, store.ErrNotFound, "a revoked link")
	in.Destination = model.SlackServesRelay{LinkID: foreign.ID}
	_, _, err = f.db.InstallFoundrySlack(ctx, in, sealAs("x"))
	assert.ErrorContains(t, err, "names its link and local workspace")

	// The link lists its live installs, and finds each by local workspace.
	listed, err := f.db.ListFoundryRelayInstalls(ctx, relinked.ID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, again.ID, listed[0].ID)
	found, err := f.db.GetFoundryRelayInstall(ctx, relinked.ID, "local-1")
	require.NoError(t, err)
	assert.Equal(t, again.ID, found.ID)
	_, err = f.db.GetFoundryRelayInstall(ctx, link.ID, "local-1")
	assert.ErrorIs(t, err, store.ErrNotFound, "the install moved to the newer link")

	// A row naming a link without a local workspace fails its read.
	dbExec(t, `UPDATE foundry_slack_installs SET relay_workspace_id=NULL, revoked_at=NOW(6) WHERE id=?`, relayed.ID)
	_, err = f.db.GetFoundrySlackInstallByID(ctx, relayed.ID)
	assert.ErrorContains(t, err, "names its link and local workspace")
}

func TestFoundrySlackRelayInstallState(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "rstate")
	link := f.linked(t, 1, f.workspaceID)

	assert.Error(t, f.db.CreateFoundrySlackRelayInstallState(ctx, link.ID, "", sha("s1"), time.Minute))
	assert.Error(t, f.db.CreateFoundrySlackRelayInstallState(ctx, link.ID, "local-1", "s1", time.Minute))
	assert.Error(t, f.db.CreateFoundrySlackRelayInstallState(ctx, link.ID, "local-1", sha("s1"), 0))
	assert.ErrorIs(t, f.db.CreateFoundrySlackRelayInstallState(ctx, f.linkID(9), "local-1", sha("s1"), time.Minute), store.ErrNotFound)
	require.NoError(t, f.db.CreateFoundrySlackRelayInstallState(ctx, link.ID, "local-1", sha("s1"), time.Minute))
	assert.ErrorIs(t, f.db.CreateFoundrySlackRelayInstallState(ctx, link.ID, "local-1", sha("s1"), time.Minute), store.ErrConflict)

	_, err := f.db.SpendFoundrySlackInstallState(ctx, sha("s1"), "user-grace")
	assert.ErrorIs(t, err, store.ErrNotFound, "the link's creator spends it")
	st, err := f.db.SpendFoundrySlackInstallState(ctx, sha("s1"), agentUser)
	require.NoError(t, err)
	assert.Equal(t, f.workspaceID, st.WorkspaceID)
	assert.Equal(t, model.SlackServesRelay{LinkID: link.ID, LocalWorkspaceID: "local-1"}, st.Destination)
	assert.Equal(t, "http://localhost:3000/settings/slack", st.ReturnTo, "back to the link's own origin")

	require.NoError(t, f.db.CreateFoundrySlackInstallState(ctx, f.workspaceID, agentUser, sha("s2"), "", time.Minute))
	st, err = f.db.SpendFoundrySlackInstallState(ctx, sha("s2"), agentUser)
	require.NoError(t, err)
	assert.Equal(t, model.SlackServesWorkspace{}, st.Destination)
	assert.Empty(t, st.ReturnTo)

	require.NoError(t, f.db.RevokeFoundryRelayLink(ctx, f.workspaceID, link.ID))
	assert.ErrorIs(t, f.db.CreateFoundrySlackRelayInstallState(ctx, link.ID, "local-1", sha("s3"), time.Minute), store.ErrNotFound)
}

func TestFoundryRelayDeliveries(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "rdel")
	link := f.linked(t, 1, f.workspaceID)
	i := f.relayed(t, 5, link, "local-1", "TR1")
	own := f.installed(t)
	conv := model.SlackConversation(i.ID, "C1", "1.1", "UADA")
	seen := model.RelaySeen{ChannelID: "C1", UserIDs: []string{"UADA", "UGRACE"}}

	// Queued only for a live relayed install, once per dedupe key.
	assert.ErrorIs(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(1, own.ID, conv), time.Minute, seen), store.ErrNotFound,
		"the workspace's own install takes intake")
	bad := f.delivery(1, i.ID, conv)
	bad.Payload = json.RawMessage(`"text"`)
	assert.Error(t, f.db.QueueFoundryRelayDelivery(ctx, bad, time.Minute, seen))
	assert.Error(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(1, i.ID, conv), time.Minute, model.RelaySeen{ChannelID: "#general"}))
	assert.Error(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(1, i.ID, conv), 0, seen))
	waiting, err := f.db.FoundryRelayWaiting(ctx, link.ID)
	require.NoError(t, err)
	assert.False(t, waiting)
	require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(1, i.ID, conv), time.Minute, seen))
	assert.ErrorIs(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(1, i.ID, conv), time.Minute, seen), store.ErrConflict)
	require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(2, i.ID, conv), time.Minute, model.RelaySeen{}))
	require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(3, i.ID, ""), time.Minute, model.RelaySeen{}))
	usage, err := f.db.GetFoundryRelayUsage(ctx, link.ID, time.Now())
	require.NoError(t, err)
	assert.Equal(t, int64(3), usage.Deliveries)
	ago, known, err := f.db.SeenFoundryRelayChannel(ctx, i.ID, "C1")
	require.NoError(t, err)
	assert.True(t, known)
	assert.Less(t, ago, time.Minute, "just now")
	known, err = f.db.KnowsFoundryRelayUsers(ctx, i.ID, []string{"UGRACE", "UADA", "UADA"})
	require.NoError(t, err)
	assert.True(t, known)

	// A claim polls without a lock, then leases within its budget, oldest
	// first, from its own link only.
	waiting, err = f.db.FoundryRelayWaiting(ctx, link.ID)
	require.NoError(t, err)
	assert.True(t, waiting)
	otherLink := f.linked(t, 2, f.workspaceID)
	none, err := f.db.LeaseFoundryRelay(ctx, otherLink.ID, time.Minute, model.RelayBudget{Deliveries: 10})
	require.NoError(t, err)
	assert.Empty(t, none.Deliveries)
	_, err = f.db.LeaseFoundryRelay(ctx, link.ID, time.Minute, model.RelayBudget{})
	assert.Error(t, err)
	_, err = f.db.LeaseFoundryRelay(ctx, link.ID, 0, model.RelayBudget{Deliveries: 1})
	assert.Error(t, err)
	claim, err := f.db.LeaseFoundryRelay(ctx, link.ID, time.Minute, model.RelayBudget{Deliveries: 2, FeedRows: 10, FeedBytes: 1 << 20})
	require.NoError(t, err)
	require.Len(t, claim.Deliveries, 2)
	assert.Empty(t, claim.Feed, "no feed held")
	d := claim.Deliveries[0]
	assert.Equal(t, f.delivery(1, i.ID, conv).ID, d.ID)
	assert.Equal(t, link.ID, d.LinkID)
	assert.Equal(t, "local-1", d.LocalWorkspaceID)
	assert.Equal(t, model.RelayLeased, d.Status)
	assert.Equal(t, 1, d.Attempts)
	assert.JSONEq(t, `{"n":1}`, string(d.Payload))
	assert.Equal(t, conv, d.Conversation)
	assert.Equal(t, "UADA", d.SlackUserID)
	claim, err = f.db.LeaseFoundryRelay(ctx, link.ID, time.Minute, model.RelayBudget{Deliveries: 10})
	require.NoError(t, err)
	require.Len(t, claim.Deliveries, 1, "leased ones wait for their lease")
	waiting, err = f.db.FoundryRelayWaiting(ctx, link.ID)
	require.NoError(t, err)
	assert.False(t, waiting)

	// A lease that runs out unacked is claimed again.
	dbExec(t, `UPDATE foundry_relay_deliveries SET lease_until=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, d.ID)
	waiting, err = f.db.FoundryRelayWaiting(ctx, link.ID)
	require.NoError(t, err)
	assert.True(t, waiting)
	claim, err = f.db.LeaseFoundryRelay(ctx, link.ID, time.Minute, model.RelayBudget{Deliveries: 10})
	require.NoError(t, err)
	require.Len(t, claim.Deliveries, 1)
	assert.Equal(t, 2, claim.Deliveries[0].Attempts)

	// Acks take their own link's leased deliveries, and drop the payloads.
	n, err := f.db.AckFoundryRelay(ctx, link.ID, nil)
	require.NoError(t, err)
	assert.Zero(t, n)
	_, err = f.db.AckFoundryRelay(ctx, link.ID, make([]string, 2049))
	assert.Error(t, err)
	n, err = f.db.AckFoundryRelay(ctx, otherLink.ID, []string{d.ID})
	require.NoError(t, err)
	assert.Zero(t, n, "another link's")
	n, err = f.db.AckFoundryRelay(ctx, link.ID, []string{d.ID, f.delivery(2, i.ID, conv).ID, "unknown"})
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	n, err = f.db.AckFoundryRelay(ctx, link.ID, []string{d.ID})
	require.NoError(t, err)
	assert.Zero(t, n, "acked once")
	assert.True(t, dbTrue(t, `SELECT status='acked' AND payload IS NULL AND finished_at IS NOT NULL FROM foundry_relay_deliveries WHERE id=?`, d.ID))

	// Expiry: queued rows past their time, leased ones past it and the grace;
	// each conversation is told once.
	for k := 4; k <= 7; k++ {
		c := conv
		if k == 7 {
			c = ""
		}
		require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(k, i.ID, c), time.Minute, model.RelaySeen{}))
	}
	dbExec(t, `UPDATE foundry_relay_deliveries SET expires_at=NOW(6) - INTERVAL 1 SECOND WHERE status IN ('queued', 'leased')`)
	_, err = f.db.ExpireFoundryRelayDeliveries(ctx, -1, 10)
	assert.Error(t, err)
	expired, err := f.db.ExpireFoundryRelayDeliveries(ctx, time.Minute, 10)
	require.NoError(t, err)
	var told []string
	for _, e := range expired {
		told = append(told, e.ID)
		assert.NotEmpty(t, e.Payload, "the worker reads where to reply")
	}
	assert.ElementsMatch(t, []string{f.delivery(4, i.ID, conv).ID, f.delivery(7, i.ID, "").ID}, told, "one per conversation, and each click")
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=4 FROM foundry_relay_deliveries WHERE status='expired' AND payload IS NULL`))
	expired, err = f.db.ExpireFoundryRelayDeliveries(ctx, time.Minute, 10)
	require.NoError(t, err)
	assert.Empty(t, expired, "a leased row has the grace to be acked")
	dbExec(t, `UPDATE foundry_relay_deliveries SET expires_at=NOW(6) - INTERVAL 30 SECOND WHERE id=?`, f.delivery(3, i.ID, "").ID)
	expired, err = f.db.ExpireFoundryRelayDeliveries(ctx, 10*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, model.RelayLeased, expired[0].Status, "as it was")
}

func TestFoundryRelayOfflineNotice(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "rnote")
	link := f.linked(t, 1, f.workspaceID)
	i := f.relayed(t, 1, link, "local-1", "T1")
	conv := model.SlackConversation(i.ID, "C1", "1.1", "UADA")
	for k, c := range []string{conv, conv, "", ""} {
		require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(k, i.ID, c), time.Hour, model.RelaySeen{}))
	}
	_, err := f.db.NoticeFoundryRelayOffline(ctx, 0, time.Minute, 10)
	assert.Error(t, err)

	// A link seen lately is online.
	require.NoError(t, f.db.SeeFoundryRelayLink(ctx, link.ID, "203.0.113.7"))
	told, err := f.db.NoticeFoundryRelayOffline(ctx, 90*time.Second, 10*time.Minute, 10)
	require.NoError(t, err)
	assert.Empty(t, told)

	// Offline: each conversation is told once, each click too.
	dbExec(t, `UPDATE foundry_relay_links SET last_seen_at=NOW(6) - INTERVAL 2 MINUTE WHERE id=?`, link.ID)
	told, err = f.db.NoticeFoundryRelayOffline(ctx, 90*time.Second, 10*time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, told, 3)
	assert.Equal(t, f.delivery(0, i.ID, conv).ID, told[0].ID)
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=4 FROM foundry_relay_deliveries WHERE noticed_at IS NOT NULL`))
	told, err = f.db.NoticeFoundryRelayOffline(ctx, 90*time.Second, 10*time.Minute, 10)
	require.NoError(t, err)
	assert.Empty(t, told, "told already")

	// A later message in a conversation told lately isn't told again.
	require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(9, i.ID, conv), time.Hour, model.RelaySeen{}))
	told, err = f.db.NoticeFoundryRelayOffline(ctx, 90*time.Second, 10*time.Minute, 10)
	require.NoError(t, err)
	assert.Empty(t, told)
	dbExec(t, `UPDATE foundry_relay_deliveries SET noticed_at=NOW(6) - INTERVAL 11 MINUTE WHERE noticed_at IS NOT NULL`)
	told, err = f.db.NoticeFoundryRelayOffline(ctx, 90*time.Second, 10*time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, told, 1)
	assert.Equal(t, f.delivery(9, i.ID, conv).ID, told[0].ID)

	// One that expired before it was told is told it wasn't delivered, never
	// that it waits.
	require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(10, i.ID, ""), time.Hour, model.RelaySeen{}))
	dbExec(t, `UPDATE foundry_relay_deliveries SET expires_at=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, f.delivery(10, i.ID, "").ID)
	told, err = f.db.NoticeFoundryRelayOffline(ctx, 90*time.Second, 10*time.Minute, 10)
	require.NoError(t, err)
	assert.Empty(t, told)
	expired, err := f.db.ExpireFoundryRelayDeliveries(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Nil(t, expired[0].NoticedAt, "never told it waits")

	// A never-seen link is offline; a revoked one is told nothing.
	l2 := f.linked(t, 2, f.workspaceID)
	i2 := f.relayed(t, 2, l2, "local-2", "T2")
	require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(20, i2.ID, ""), time.Hour, model.RelaySeen{}))
	require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(21, i2.ID, ""), time.Hour, model.RelaySeen{}))
	told, err = f.db.NoticeFoundryRelayOffline(ctx, 90*time.Second, 10*time.Minute, 1)
	require.NoError(t, err)
	assert.Len(t, told, 1, "within its limit")
	require.NoError(t, f.db.RevokeFoundryRelayLink(ctx, f.workspaceID, l2.ID))
	told, err = f.db.NoticeFoundryRelayOffline(ctx, 90*time.Second, 10*time.Minute, 10)
	require.NoError(t, err)
	assert.Empty(t, told)
}

var relayLimits = model.RelayFeedLimits{TTL: time.Hour, MaxRows: 100}

func relayFeedRow(event, channel string) model.SlackFeedRow {
	return model.SlackFeedRow{DedupeKey: model.SlackEventDedupe(event), ChannelID: channel, Day: model.SlackDay(time.Now()), Kind: model.SlackRowMessage,
		Source: model.SlackFeedFromEvent, Payload: json.RawMessage(`{"text":"hello"}`)}
}

func feedIDs(items []model.RelayFeedItem) []string {
	var ids []string
	for _, it := range items {
		switch v := it.(type) {
		case model.RelayFeedRow:
			ids = append(ids, v.ID)
		case model.RelayFeedGap:
			ids = append(ids, v.ID)
		}
	}
	return ids
}

func TestFoundryRelayFeed(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "rfeed")
	link := f.linked(t, 1, f.workspaceID)
	i := f.relayed(t, 5, link, "local-1", "TR1")
	own := f.installed(t)
	at := time.Now().UTC()
	rows := func(events ...string) model.RelayFeedAppend {
		a := model.RelayFeedAppend{At: at}
		for _, e := range events {
			a.Rows = append(a.Rows, relayFeedRow(e, "C1"))
		}
		return a
	}
	count := func() int64 {
		var n int64
		require.NoError(t, testDB.QueryRow(`SELECT relay_feed_rows FROM foundry_slack_installs WHERE id=?`, i.ID).Scan(&n))
		return n
	}

	// Held only for a live relayed install whose local connector reads it.
	_, err := f.db.AppendFoundryRelayFeed(ctx, i.ID, model.RelayFeedAppend{At: at}, relayLimits)
	assert.Error(t, err)
	_, err = f.db.AppendFoundryRelayFeed(ctx, i.ID, rows("Ev1"), model.RelayFeedLimits{TTL: time.Hour})
	assert.Error(t, err)
	out, err := f.db.AppendFoundryRelayFeed(ctx, i.ID, rows("Ev1"), relayLimits)
	require.NoError(t, err)
	assert.Equal(t, model.SlackAppendUnbound, out.Outcome, "its feed is off")
	require.NoError(t, f.db.SetFoundryRelayFeeds(ctx, link.ID, []model.RelayFeedOn{{LocalWorkspaceID: "local-1", On: true},
		{LocalWorkspaceID: "local-unknown", On: true}}))
	require.NoError(t, f.db.SetFoundryRelayFeeds(ctx, link.ID, []model.RelayFeedOn{{LocalWorkspaceID: "local-1", On: true}}), "on again is a no-op")
	listed, err := f.db.ListFoundryRelayInstalls(ctx, link.ID)
	require.NoError(t, err)
	assert.True(t, listed[0].FeedOn)
	out, err = f.db.AppendFoundryRelayFeed(ctx, own.ID, rows("Ev1"), relayLimits)
	require.NoError(t, err)
	assert.Equal(t, model.SlackAppendUnbound, out.Outcome, "the workspace's own install keeps its own feed")

	require.NoError(t, f.db.RecordFoundryRelayChannels(ctx, i.ID, []string{"C1", "C9"}))
	out, err = f.db.AppendFoundryRelayFeed(ctx, i.ID, rows("Ev1", "Ev2"), relayLimits)
	require.NoError(t, err)
	assert.Equal(t, model.RelayFeedAppended{Outcome: model.SlackAppended, Rows: 2}, out)
	out, err = f.db.AppendFoundryRelayFeed(ctx, i.ID, rows("Ev2"), relayLimits)
	require.NoError(t, err)
	assert.Equal(t, model.RelayFeedAppended{Outcome: model.SlackAppendDuplicate}, out)
	assert.Equal(t, int64(2), count())
	usage, err := f.db.GetFoundryRelayUsage(ctx, link.ID, at)
	require.NoError(t, err)
	assert.Equal(t, int64(2), usage.FeedRows)
	assert.Equal(t, int64(2*len(`{"text":"hello"}`)), usage.FeedBytes)

	// Past the cap, rows become a gap of the minute in each channel; gaps
	// that meet merge.
	full, err := f.db.AppendFoundryRelayFeed(ctx, i.ID, model.RelayFeedAppend{Rows: []model.SlackFeedRow{relayFeedRow("Ev3", "C1"), relayFeedRow("Ev4", "C2"),
		relayFeedRow("Ev5", "C2")}, At: at}, model.RelayFeedLimits{TTL: time.Hour, MaxRows: 3})
	require.NoError(t, err)
	assert.Equal(t, model.RelayFeedAppended{Outcome: model.SlackAppendFull, Rows: 2}, full)
	assert.Equal(t, int64(2), count(), "gaps aren't counted")
	next := model.SlackMinuteGap("C1", at.Add(time.Minute))
	out, err = f.db.AppendFoundryRelayFeed(ctx, i.ID, model.RelayFeedAppend{Gaps: []model.SlackFeedGap{next}, At: at}, relayLimits)
	require.NoError(t, err)
	assert.Equal(t, model.RelayFeedAppended{Outcome: model.SlackAppended, Rows: 1}, out)
	out, err = f.db.AppendFoundryRelayFeed(ctx, i.ID, model.RelayFeedAppend{Gaps: []model.SlackFeedGap{next}, At: at}, relayLimits)
	require.NoError(t, err)
	assert.Equal(t, model.SlackAppendDuplicate, out.Outcome, "held already")
	every := model.SlackMinuteGap("", at.Add(time.Hour))
	_, err = f.db.AppendFoundryRelayFeed(ctx, i.ID, model.RelayFeedAppend{Gaps: []model.SlackFeedGap{every}, At: at}, relayLimits)
	require.NoError(t, err)
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=3 FROM foundry_relay_feed WHERE install_id=? AND kind='gap'`, i.ID), "C1, C2 and every channel")

	// A claim takes deliveries, then the feed within its bytes.
	require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(1, i.ID, ""), time.Minute, model.RelaySeen{}))
	claim, err := f.db.LeaseFoundryRelay(ctx, link.ID, time.Minute, model.RelayBudget{Deliveries: 10, FeedRows: 10, FeedBytes: 1})
	require.NoError(t, err)
	require.Len(t, claim.Deliveries, 1)
	require.Len(t, claim.Feed, 1, "the first item alone is past the bytes")
	row, ok := claim.Feed[0].(model.RelayFeedRow)
	require.True(t, ok)
	assert.Equal(t, i.ID, row.InstallID)
	assert.Equal(t, "local-1", row.LocalWorkspaceID)
	want := relayFeedRow("Ev1", "C1")
	assert.JSONEq(t, string(want.Payload), string(row.Row.Payload))
	row.Row.Payload = want.Payload
	assert.Equal(t, want, row.Row)
	claim2, err := f.db.LeaseFoundryRelay(ctx, link.ID, time.Minute, model.RelayBudget{Deliveries: 1})
	require.NoError(t, err)
	assert.Empty(t, claim2.Feed, "a claim may take no feed")
	claim2, err = f.db.LeaseFoundryRelay(ctx, link.ID, time.Minute, model.RelayBudget{Deliveries: 1, FeedRows: 10, FeedBytes: 1 << 20})
	require.NoError(t, err)
	require.Len(t, claim2.Feed, 4)
	var gaps []model.SlackFeedGap
	for _, it := range claim2.Feed {
		if g, ok := it.(model.RelayFeedGap); ok {
			assert.Equal(t, "local-1", g.LocalWorkspaceID)
			gaps = append(gaps, g.Gap)
		}
	}
	first := model.SlackMinuteGap("C1", at)
	assert.ElementsMatch(t, []model.SlackFeedGap{{ChannelID: "C1", From: first.From, To: next.To}, model.SlackMinuteGap("C2", at), every}, gaps)

	// A new gap merged into one a claim holds is that one.
	again, err := f.db.AppendFoundryRelayFeed(ctx, i.ID, model.RelayFeedAppend{Gaps: []model.SlackFeedGap{first}, At: at}, relayLimits)
	require.NoError(t, err)
	assert.Equal(t, 1, again.Rows, "claimed gaps aren't merged into")
	again, err = f.db.AppendFoundryRelayFeed(ctx, i.ID, model.RelayFeedAppend{Gaps: []model.SlackFeedGap{next}, At: at}, relayLimits)
	require.NoError(t, err)
	assert.Equal(t, 1, again.Rows)
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=1 FROM foundry_relay_feed WHERE kind='gap' AND channel_id='C1'`), "the claimed gap holds both")

	// Acks delete the feed they took, and uncount its rows.
	n, err := f.db.AckFoundryRelay(ctx, link.ID, append(feedIDs(claim.Feed), feedIDs(claim2.Feed)...))
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, int64(0), count())
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=0 FROM foundry_relay_feed`))

	// Rows held past their time become a gap per channel; gaps stay.
	_, err = f.db.AppendFoundryRelayFeed(ctx, i.ID, model.RelayFeedAppend{Rows: []model.SlackFeedRow{relayFeedRow("Ev6", "C1"), relayFeedRow("Ev7", "C1"),
		relayFeedRow("Ev8", "C3")}, Gaps: []model.SlackFeedGap{model.SlackMinuteGap("C9", at)}, At: at}, relayLimits)
	require.NoError(t, err)
	dbExec(t, `UPDATE foundry_relay_feed SET expires_at=NOW(6) - INTERVAL 1 SECOND`)
	dbExec(t, `UPDATE foundry_relay_feed SET lease_until=NOW(6) + INTERVAL 1 MINUTE WHERE dedupe_key=?`, model.SlackEventDedupe("Ev7"))
	_, err = f.db.ExpireFoundryRelayFeed(ctx, 0, 10)
	assert.Error(t, err)
	expired, err := f.db.ExpireFoundryRelayFeed(ctx, time.Hour, 10)
	require.NoError(t, err)
	assert.Equal(t, 2, expired, "a row under a live lease waits")
	assert.Equal(t, int64(1), count())
	var payload string
	require.NoError(t, testDB.QueryRow(`SELECT payload FROM foundry_relay_feed WHERE kind='gap' AND channel_id='C1'`).Scan(&payload))
	var span struct{ From, To time.Time }
	require.NoError(t, json.Unmarshal([]byte(payload), &span))
	assert.True(t, span.From.Before(at.Add(-model.RelayFeedLateness+time.Second)), "from before the row arrived: %s", span.From)
	assert.True(t, span.To.After(at), "to past it: %s", span.To)
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=3 FROM foundry_relay_feed WHERE kind='gap'`), "C1, C3 and C9")
	expired, err = f.db.ExpireFoundryRelayFeed(ctx, time.Hour, 10)
	require.NoError(t, err)
	assert.Zero(t, expired, "gaps never expire")

	// Turning the feed off drops what it held.
	require.NoError(t, f.db.SetFoundryRelayFeeds(ctx, link.ID, []model.RelayFeedOn{{LocalWorkspaceID: "local-1"}}))
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=0 FROM foundry_relay_feed`))
	assert.Equal(t, int64(0), count())
	waiting, err := f.db.FoundryRelayWaiting(ctx, link.ID)
	require.NoError(t, err)
	assert.False(t, waiting)
}

// The door holds gaps from requests no one verified: a relayed install
// keeps only those of channels it reads, of days a window keeps, and past
// its cap they widen one gap of every channel instead of growing.
func TestFoundryRelayFeedGaps(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "rgaps")
	link := f.linked(t, 1, f.workspaceID)
	i := f.relayed(t, 5, link, "local-1", "TR1")
	require.NoError(t, f.db.SetFoundryRelayFeeds(ctx, link.ID, []model.RelayFeedOn{{LocalWorkspaceID: "local-1", On: true}}))
	require.NoError(t, f.db.RecordFoundryRelayChannels(ctx, i.ID, []string{"C1"}))
	at := time.Now().UTC()
	gaps := func(gaps ...model.SlackFeedGap) model.RelayFeedAppended {
		t.Helper()
		out, err := f.db.AppendFoundryRelayFeed(ctx, i.ID, model.RelayFeedAppend{Gaps: gaps, At: at}, relayLimits)
		require.NoError(t, err)
		return out
	}
	held := func(channel string) int {
		return dbCount(t, `SELECT COUNT(*) FROM foundry_relay_feed WHERE install_id=? AND kind='gap' AND channel_id=?`, i.ID, channel)
	}

	assert.Equal(t, model.SlackAppendDuplicate, gaps(model.SlackMinuteGap("C7", at)).Outcome, "a channel it doesn't read")
	assert.Equal(t, model.SlackAppendDuplicate, gaps(model.SlackMinuteGap("C1", at.AddDate(-1, 0, 0))).Outcome, "past any window")
	assert.Equal(t, 2, gaps(model.SlackMinuteGap("C1", at), model.SlackMinuteGap("", at)).Rows)
	assert.Equal(t, 0, held("C7"))
	assert.Equal(t, 1, held("C1"))

	// A channel the feed reads is known from its first row, before any call
	// named it, so a real gap in it is held; the channel of a left row (the
	// bot removed) isn't.
	left := relayFeedRow("EvLeft", "C8")
	left.Kind = model.SlackRowLeft
	_, err := f.db.AppendFoundryRelayFeed(ctx, i.ID, model.RelayFeedAppend{Rows: []model.SlackFeedRow{relayFeedRow("EvJoin", "C7"), left}, At: at},
		relayLimits)
	require.NoError(t, err)
	assert.Equal(t, 1, gaps(model.SlackMinuteGap("C7", at), model.SlackMinuteGap("C8", at)).Rows)
	assert.Equal(t, 1, held("C7"))
	assert.Equal(t, 0, held("C8"))

	// At the cap, a gap widens the newest unclaimed gap of every channel.
	rows := make([]any, 0, 6*model.RelayFeedMaxGaps)
	for n := range model.RelayFeedMaxGaps - 2 {
		g := model.SlackMinuteGap("C1", at.Add(-time.Duration(2*n+10)*time.Minute))
		r := g.Row("C1")
		rows = append(rows, id26(fmt.Sprintf("RG%05d", n)), link.ID, i.ID, r.DedupeKey, r.Day, string(r.Payload))
	}
	dbExec(t, `INSERT INTO foundry_relay_feed (id, link_id, install_id, relay_workspace_id, dedupe_key, channel_id, day, kind, payload, bytes, expires_at)
VALUES `+strings.TrimSuffix(strings.Repeat(`(?, ?, ?, 'local-1', ?, 'C1', ?, 'gap', ?, 40, NOW(6) + INTERVAL 1 HOUR), `, model.RelayFeedMaxGaps-2), ", "), rows...)
	far := model.SlackMinuteGap("C1", at.Add(-6*time.Hour))
	assert.Equal(t, 1, gaps(far).Rows)
	assert.Equal(t, model.RelayFeedMaxGaps-1, held("C1"), "not a gap of its own")
	var payload string
	require.NoError(t, testDB.QueryRow(`SELECT payload FROM foundry_relay_feed WHERE install_id=? AND kind='gap' AND channel_id=''`, i.ID).Scan(&payload))
	var span struct{ From, To time.Time }
	require.NoError(t, json.Unmarshal([]byte(payload), &span))
	assert.True(t, span.From.Equal(far.From) && span.To.Equal(model.SlackMinuteGap("", at).To), "every channel's gap spans it: %+v", span)
	assert.Equal(t, 1, held(""))
}

func TestFoundryRelayProxyScope(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "rscope")
	link := f.linked(t, 1, f.workspaceID)
	i := f.relayed(t, 5, link, "local-1", "TR1")
	own := f.installed(t)

	assert.Error(t, f.db.RecordFoundryRelayChannels(ctx, i.ID, []string{"C1", "#general"}))
	assert.ErrorIs(t, f.db.RecordFoundryRelayChannels(ctx, own.ID, []string{"C1"}), store.ErrNotFound)
	many := make([]string, 0, 501)
	for k := range 501 {
		many = append(many, fmt.Sprintf("C%d", k))
	}
	require.NoError(t, f.db.RecordFoundryRelayChannels(ctx, i.ID, many))
	require.NoError(t, f.db.RecordFoundryRelayChannels(ctx, i.ID, []string{"C1"}), "seen again")
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=501 FROM foundry_relay_channels WHERE install_id=?`, i.ID))
	for channel, want := range map[string]bool{"C500": true, "C9999": false} {
		_, known, err := f.db.SeenFoundryRelayChannel(ctx, i.ID, channel)
		require.NoError(t, err)
		assert.Equal(t, want, known, channel)
	}
	_, known, err := f.db.SeenFoundryRelayChannel(ctx, own.ID, "C1")
	require.NoError(t, err)
	assert.False(t, known, "per install")
	dbExec(t, `UPDATE foundry_relay_channels SET last_at=NOW(6) - INTERVAL 2 HOUR WHERE channel_id='C1'`)
	ago, known, err := f.db.SeenFoundryRelayChannel(ctx, i.ID, "C1")
	require.NoError(t, err)
	assert.True(t, known)
	assert.InDelta(t, 2*time.Hour, ago, float64(time.Minute), "how long ago")
	dbExec(t, `UPDATE foundry_relay_channels SET last_at=NOW(6) - INTERVAL 31 DAY WHERE channel_id='C1'`)
	_, known, err = f.db.SeenFoundryRelayChannel(ctx, i.ID, "C1")
	require.NoError(t, err)
	assert.False(t, known, "not seen in 30 days")
	// Shared since: forgotten for the install alone.
	require.NoError(t, f.db.ForgetFoundryRelayChannel(ctx, i.ID, "C500"))
	_, known, err = f.db.SeenFoundryRelayChannel(ctx, i.ID, "C500")
	require.NoError(t, err)
	assert.False(t, known, "forgotten once shared")
	require.NoError(t, f.db.ForgetFoundryRelayChannel(ctx, i.ID, "C500"), "forgotten again is nothing")
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=500 FROM foundry_relay_channels WHERE install_id=?`, i.ID))

	require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(1, i.ID, ""), time.Minute, model.RelaySeen{UserIDs: []string{"UADA"}}))
	for _, users := range [][]string{nil, {"UADA", "UGRACE"}, {"UGRACE"}} {
		known, err := f.db.KnowsFoundryRelayUsers(ctx, i.ID, users)
		require.NoError(t, err)
		assert.False(t, known, "%v", users)
	}
	known, err = f.db.KnowsFoundryRelayUsers(ctx, i.ID, []string{"UADA"})
	require.NoError(t, err)
	assert.True(t, known)

	// Usage adds up per day, and only grows.
	assert.Error(t, f.db.CountFoundryRelayUsage(ctx, link.ID, model.RelayUsage{Calls: -1}))
	require.NoError(t, f.db.CountFoundryRelayUsage(ctx, link.ID, model.RelayUsage{Calls: 2, Refused: 1}))
	require.NoError(t, f.db.CountFoundryRelayUsage(ctx, link.ID, model.RelayUsage{Calls: 1}))
	usage, err := f.db.GetFoundryRelayUsage(ctx, link.ID, time.Now())
	require.NoError(t, err)
	assert.Equal(t, model.RelayUsage{Deliveries: 1, Calls: 3, Refused: 1}, usage)
	usage, err = f.db.GetFoundryRelayUsage(ctx, link.ID, time.Now().AddDate(0, 0, -2))
	require.NoError(t, err)
	assert.Zero(t, usage)

	// The prune lets go of channels and users not seen in 30 days, finished
	// deliveries after a day, and codes a day past their expiry.
	dbExec(t, `UPDATE foundry_relay_users SET last_at=NOW(6) - INTERVAL 31 DAY`)
	dbExec(t, `UPDATE foundry_relay_deliveries SET status='acked', payload=NULL, finished_at=NOW(6) - INTERVAL 25 HOUR`)
	require.NoError(t, f.db.CreateFoundryRelayCode(ctx, f.relayCode(9, f.workspaceID), time.Minute))
	dbExec(t, `UPDATE foundry_relay_codes SET expires_at=NOW(6) - INTERVAL 25 HOUR WHERE code_hash=?`, f.relayCode(9, f.workspaceID).CodeHash)
	pruned, err := f.db.PruneFoundrySlack(ctx, time.Hour, time.Hour, 1000)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, pruned, int64(4))
	for _, table := range []string{"foundry_relay_users", "foundry_relay_deliveries"} {
		assert.True(t, dbTrue(t, `SELECT COUNT(*)=0 FROM `+table), table)
	}
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=1 FROM foundry_relay_codes`), "the link's own, spent but not expired a day")
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=499 FROM foundry_relay_channels`), "C1 pruned, C500 forgotten as shared")
}

func TestPurgeFoundrySlackRelay(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "rpurge")
	link := f.linked(t, 1, f.workspaceID)
	i := f.relayed(t, 1, link, "local-1", "T1")
	require.NoError(t, f.db.CreateFoundryRelayCode(ctx, f.relayCode(2, f.workspaceID), time.Minute))
	require.NoError(t, f.db.QueueFoundryRelayDelivery(ctx, f.delivery(1, i.ID, ""), time.Minute, model.RelaySeen{ChannelID: "C1"}))

	require.NoError(t, f.db.PurgeFoundrySlack(ctx, f.workspaceID))
	_, err := f.db.GetFoundryRelayLinkByToken(ctx, sha(fmt.Sprintf("token-%s-1", f.tag)))
	assert.ErrorIs(t, err, store.ErrNotFound, "the workspace's links are revoked")
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=0 FROM foundry_relay_codes`))
	assert.True(t, dbTrue(t, `SELECT revoked_at IS NOT NULL FROM foundry_slack_installs WHERE id=?`, i.ID))
	assert.True(t, dbTrue(t, `SELECT status='expired' FROM foundry_relay_deliveries WHERE install_id=?`, i.ID))
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=0 FROM foundry_relay_channels`))
	require.NoError(t, f.db.PurgeFoundrySlack(ctx, f.workspaceID), "again changes nothing")
}

func TestFoundrySlackAccess(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "access")
	i := f.installed(t)
	request := func(user, name string) (bool, error) {
		return f.db.RequestFoundrySlackAccess(ctx, &model.SlackAccessRequest{InstallID: i.ID, SlackUserID: user, SlackName: name})
	}

	// A Slack user asks once.
	_, err := request("UGRACE", "")
	assert.Error(t, err)
	asked, err := request("UGRACE", "Grace Hopper")
	require.NoError(t, err)
	assert.True(t, asked)
	asked, err = request("UGRACE", "Grace B. Hopper")
	require.NoError(t, err)
	assert.False(t, asked, "once")
	_, err = f.db.RequestFoundrySlackAccess(ctx, &model.SlackAccessRequest{InstallID: f.installID(9), SlackUserID: "UGRACE", SlackName: "Grace"})
	assert.ErrorIs(t, err, store.ErrNotFound)
	requests, err := f.db.ListFoundrySlackAccessRequests(ctx, i.ID)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, "Grace Hopper", requests[0].SlackName)

	// The owner links their own Slack account in full; the people they allow
	// act as them, read-only, beside it.
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UADA", sha("link-ada"), time.Minute))
	owner, err := f.db.LinkFoundrySlackUser(ctx, f.workspaceID, sha("link-ada"), agentUser)
	require.NoError(t, err)
	assert.Equal(t, model.SlackFullAccess{}, owner.Access)
	asked, err = request("UADA", "Ada")
	require.NoError(t, err)
	assert.False(t, asked, "a linked user asks nothing")
	_, err = f.db.AllowFoundrySlackUser(ctx, i.ID, "UADA", agentUser)
	assert.ErrorIs(t, err, store.ErrConflict, "the owner's own link is never made read-only")
	_, err = f.db.AllowFoundrySlackUser(ctx, i.ID, "", agentUser)
	assert.Error(t, err)
	_, err = f.db.AllowFoundrySlackUser(ctx, f.installID(9), "UGRACE", agentUser)
	assert.ErrorIs(t, err, store.ErrNotFound)
	grace, err := f.db.AllowFoundrySlackUser(ctx, i.ID, "UGRACE", agentUser)
	require.NoError(t, err)
	assert.Equal(t, model.SlackReadAccess{}, grace.Access)
	assert.Equal(t, agentUser, grace.UserID)
	_, err = f.db.AllowFoundrySlackUser(ctx, i.ID, "UGRACE", agentUser)
	require.NoError(t, err, "allowed again")
	_, err = f.db.AllowFoundrySlackUser(ctx, i.ID, "UALAN", agentUser)
	require.NoError(t, err, "without a request")
	requests, err = f.db.ListFoundrySlackAccessRequests(ctx, i.ID)
	require.NoError(t, err)
	assert.Empty(t, requests, "allowing answers the request")
	links, err := f.db.ListFoundrySlackLinks(ctx, i.ID)
	require.NoError(t, err)
	assert.Len(t, links, 3)

	// Relinking the owner keeps the people they allowed; their own Slack
	// account moves.
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, i.ID, "UADA2", sha("link-ada2"), time.Minute))
	_, err = f.db.LinkFoundrySlackUser(ctx, f.workspaceID, sha("link-ada2"), agentUser)
	require.NoError(t, err)
	links, err = f.db.ListFoundrySlackLinks(ctx, i.ID)
	require.NoError(t, err)
	got := map[string]model.SlackLinkAccess{}
	for _, l := range links {
		got[l.SlackUserID] = l.Access
	}
	assert.Equal(t, map[string]model.SlackLinkAccess{"UADA2": model.SlackFullAccess{}, "UGRACE": model.SlackReadAccess{}, "UALAN": model.SlackReadAccess{}},
		got)

	// A request can be dropped unanswered; a disconnect drops them all.
	_, err = request("UBOB", "Bob")
	require.NoError(t, err)
	require.NoError(t, f.db.DeleteFoundrySlackAccessRequest(ctx, i.ID, "UBOB"))
	assert.ErrorIs(t, f.db.DeleteFoundrySlackAccessRequest(ctx, i.ID, "UBOB"), store.ErrNotFound)
	_, err = request("UBOB", "Bob")
	require.NoError(t, err)
	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, i.ID, true))
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=0 FROM foundry_slack_access_requests`))
	_, err = request("UCAROL", "Carol")
	assert.ErrorIs(t, err, store.ErrNotFound, "a revoked install takes no requests")
	_, err = f.db.AllowFoundrySlackUser(ctx, i.ID, "UGRACE", agentUser)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// A link whose access no binary knows fails its read.
	i2, _, err := f.db.InstallFoundrySlack(ctx, f.install(2, f.workspaceID, "T2"), sealAs("x"))
	require.NoError(t, err)
	dbExec(t, `INSERT INTO foundry_slack_links (install_id, slack_user_id, user_id, access) VALUES (?, 'UX', 'u', 'admin')`, i2.ID)
	_, err = f.db.GetFoundrySlackLink(ctx, i2.ID, "UX")
	assert.ErrorContains(t, err, "unknown Slack link access")
}

func TestMirrorFoundrySlackInstalls(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "mirror")
	live := func() []string {
		rows, err := testDB.Query(`SELECT id FROM foundry_slack_installs WHERE revoked_at IS NULL ORDER BY id`)
		require.NoError(t, err)
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			ids = append(ids, id)
		}
		return ids
	}
	a := f.install(1, f.workspaceID, "T1")
	a.BotID, a.InstallerAdmin = "B1", true

	relayedOne := f.install(9, f.workspaceID, "T9")
	relayedOne.Destination = model.SlackServesRelay{LinkID: "l", LocalWorkspaceID: "w"}
	assert.Error(t, f.db.MirrorFoundrySlackInstalls(ctx, []*model.SlackInstall{relayedOne}), "a local install serves its own workspace")
	broken := f.install(9, f.workspaceID, "")
	assert.Error(t, f.db.MirrorFoundrySlackInstalls(ctx, []*model.SlackInstall{broken}))

	require.NoError(t, f.db.MirrorFoundrySlackInstalls(ctx, []*model.SlackInstall{a}))
	got, err := f.db.GetFoundrySlackInstall(ctx, f.workspaceID)
	require.NoError(t, err)
	assert.Equal(t, a.ID, got.ID)
	assert.Equal(t, "B1", got.BotID)
	assert.True(t, got.InstallerAdmin)
	_, err = f.db.GetFoundrySlackInstallToken(ctx, a.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "its calls go through the relay")
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=2 FROM foundry_slack_slots WHERE install_id=?`, a.ID))

	// Listed again, it takes what Helix says now.
	a.TeamName, a.Scopes = "Acme Renamed", []string{"chat:write", "channels:history"}
	require.NoError(t, f.db.MirrorFoundrySlackInstalls(ctx, []*model.SlackInstall{a}))
	got, err = f.db.GetFoundrySlackInstall(ctx, f.workspaceID)
	require.NoError(t, err)
	assert.Equal(t, "Acme Renamed", got.TeamName)
	assert.Equal(t, []string{"chat:write", "channels:history"}, got.Scopes)

	// Left out, it is revoked and uninstalled with what it held, and stays so.
	require.NoError(t, f.db.CreateFoundrySlackLinkToken(ctx, a.ID, "UADA", sha("mirror-link"), time.Minute))
	require.NoError(t, f.db.MirrorFoundrySlackInstalls(ctx, nil))
	assert.Empty(t, live())
	assert.True(t, dbTrue(t, `SELECT uninstalled_at IS NOT NULL FROM foundry_slack_installs WHERE id=?`, a.ID))
	assert.True(t, dbTrue(t, `SELECT COUNT(*)=0 FROM foundry_slack_link_tokens`))
	require.NoError(t, f.db.MirrorFoundrySlackInstalls(ctx, []*model.SlackInstall{a}))
	assert.Empty(t, live(), "a revoked install stays revoked")

	// A new install of the team replaces the workspace's; another team's
	// replaces it in turn.
	b := f.install(2, f.workspaceID, "T1")
	require.NoError(t, f.db.MirrorFoundrySlackInstalls(ctx, []*model.SlackInstall{b}))
	assert.Equal(t, []string{b.ID}, live())
	c := f.install(3, f.workspaceID, "T2")
	require.NoError(t, f.db.MirrorFoundrySlackInstalls(ctx, []*model.SlackInstall{c}))
	assert.Equal(t, []string{c.ID}, live())

	// One of a workspace that is gone is left out.
	gone := id26("SWS-mirror-gone")
	createWorkspace(t, gone)
	deleteWorkspace(t, gone)
	d := f.install(4, gone, "T4")
	require.NoError(t, f.db.MirrorFoundrySlackInstalls(ctx, []*model.SlackInstall{c, d}))
	assert.Equal(t, []string{c.ID}, live())
}

func TestFoundryAgentTurnReadOnly(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "readonly")
	person := model.AgentPerson{UserID: agentUser}
	turn := &model.FoundryAgentTurn{ID: f.turnID(1), WorkspaceID: f.workspaceID, ThreadID: f.threadID(1), Principal: person,
		Kind: model.FoundryAgentTurnMessage, Surface: model.FoundryAgentSurfaceSlack, ReadOnly: true, Input: "hi", MaxUSDMicros: 500_000}
	require.NoError(t, f.db.CreateFoundryAgentTurn(ctx, turn, &model.FoundryAgentThread{ID: f.threadID(1), WorkspaceID: f.workspaceID,
		Principal: person, Title: "Hi", Runtime: model.FoundryAgentRuntimeAPI}, f.limits(), nil))
	got, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, turn.ID)
	require.NoError(t, err)
	assert.True(t, got.ReadOnly)

	// A continuation is as read-only as its action's turn, whatever its
	// caller says.
	claimed := f.claimed(t)
	require.Equal(t, turn.ID, claimed.ID)
	assert.True(t, claimed.ReadOnly)
	a, err := f.prepare(claimed, 1, time.Now().Add(time.Hour))
	require.NoError(t, err)
	f.settle(t, claimed)
	_, err = f.db.ClaimFoundryAgentAction(ctx, f.workspaceID, agentUser, a.ID, a.PayloadHash)
	require.NoError(t, err)
	next := &model.FoundryAgentTurn{ID: f.turnID(2), WorkspaceID: f.workspaceID, ThreadID: turn.ThreadID, Principal: person,
		Kind: model.FoundryAgentTurnActionResult, Input: a.ID, MaxUSDMicros: 500_000}
	require.NoError(t, f.db.FinishFoundryAgentAction(ctx, a.ID, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionSucceeded,
		Result: json.RawMessage(`{}`)}, next, json.RawMessage(`{"type":"action_result"}`)))
	assert.True(t, next.ReadOnly, "the caller learns it")
	cont, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, next.ID)
	require.NoError(t, err)
	assert.True(t, cont.ReadOnly)
}
