package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// newLocalSlackFixture is a standalone install's API: no emails, its Slack
// reached through its link to Helix, linked.
func newLocalSlackFixture(t *testing.T) (slackFixture, *fakeLinker) {
	t.Helper()
	f := newSlackFixture(t)
	f.cfg.Host.Local = true
	f.s.deps.Emails = nil
	linker := &fakeLinker{state: RelayState{Linked: true}, slack: f.s.deps.Slack}
	f.s.deps.Relay = linker
	return f, linker
}

func TestLocalStartFoundrySlackInstall(t *testing.T) {
	start := func(f slackFixture) (*foundryv1.StartFoundrySlackInstallResponse, error) {
		return f.s.StartFoundrySlackInstall(as("user-1"), &foundryv1.StartFoundrySlackInstallRequest{WorkspaceId: slackWS})
	}
	t.Run("through Helix", func(t *testing.T) {
		f, linker := newLocalSlackFixture(t)
		got, err := start(f)
		require.NoError(t, err)
		assert.Equal(t, "https://slack.com/oauth/v2/authorize?state=relayed", got.GetAuthorizeUrl())
		assert.Equal(t, []string{slackWS}, linker.installs)
		assert.Empty(t, f.db.states, "Helix keeps the install's state")
	})
	t.Run("unlinked", func(t *testing.T) {
		f, linker := newLocalSlackFixture(t)
		linker.state = RelayState{}
		_, err := start(f)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_RELAY_NOT_LINKED", reason)
	})
	t.Run("Helix unreachable", func(t *testing.T) {
		f, linker := newLocalSlackFixture(t)
		linker.err = errors.New("unreachable")
		_, err := start(f)
		assert.Equal(t, codes.Unavailable, status.Code(err))
	})
	t.Run("installed already", func(t *testing.T) {
		f, linker := newLocalSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		_, err := start(f)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_INSTALLED", reason)
		assert.Empty(t, linker.installs)
	})
}

func TestLocalFoundrySlackLink(t *testing.T) {
	setup := func(t *testing.T, userJSON string) slackFixture {
		f, _ := newLocalSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		// Helix keeps Slack's emails from a relayed install.
		f.slack.answer("users.info", http.StatusOK, userJSON)
		f.slack.answer("conversations.open", http.StatusOK, `{"ok":true,"channel":{"id":"D0DM00001"}}`)
		f.slack.answer("chat.postMessage", http.StatusOK, `{"ok":true,"ts":"1795000099.000100"}`)
		return f
	}
	t.Run("the installer links as the owner, with no email", func(t *testing.T) {
		f := setup(t, slackUserJSON(slackAda, "", nil))
		token := f.db.linkToken("inst-1", slackAda)
		preview, err := f.s.PreviewFoundrySlackLink(as("user-1"), &foundryv1.PreviewFoundrySlackLinkRequest{WorkspaceId: slackWS, Token: token})
		require.NoError(t, err)
		assert.True(t, preview.GetMatchesYou())
		assert.Empty(t, preview.GetSlackEmailMasked())
		assert.Equal(t, "Bearer xoxb-relayed", f.slack.call("users.info").auth, "as the relayed install")
		link, err := f.s.CreateFoundrySlackLink(as("user-1"), &foundryv1.CreateFoundrySlackLinkRequest{WorkspaceId: slackWS, Token: token})
		require.NoError(t, err)
		assert.Equal(t, slackAda, link.GetLink().GetSlackUserId())
		assert.Equal(t, foundryv1.FoundrySlackLinkAccess_FOUNDRY_SLACK_LINK_ACCESS_FULL, link.GetLink().GetAccess())
		told := f.slack.call("chat.postMessage")
		assert.Equal(t, "Bearer xoxb-relayed", told.auth)
		assert.Contains(t, string(told.body), "the owner of the Foundry on your computer")
	})
	t.Run("a guest still isn't linked", func(t *testing.T) {
		f := setup(t, slackUserJSON(slackAda, "", func(u map[string]any) { u["is_restricted"] = true }))
		token := f.db.linkToken("inst-1", slackAda)
		preview, err := f.s.PreviewFoundrySlackLink(as("user-1"), &foundryv1.PreviewFoundrySlackLinkRequest{WorkspaceId: slackWS, Token: token})
		require.NoError(t, err)
		assert.Equal(t, foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER, preview.GetRefusal())
	})
	t.Run("only the installer's token links", func(t *testing.T) {
		f := setup(t, slackUserJSON("U0GRACE01", "", nil))
		token := f.db.linkToken("inst-1", "U0GRACE01")
		_, err := f.s.PreviewFoundrySlackLink(as("user-1"), &foundryv1.PreviewFoundrySlackLinkRequest{WorkspaceId: slackWS, Token: token})
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_LINK_EXPIRED", reason)
		assert.Empty(t, f.slack.called(), "Slack isn't asked")
	})
	t.Run("the relay can't be reached", func(t *testing.T) {
		f := setup(t, slackUserJSON(slackAda, "", nil))
		f.s.deps.Relay.(*fakeLinker).err = errors.New("unlinked")
		token := f.db.linkToken("inst-1", slackAda)
		_, err := f.s.PreviewFoundrySlackLink(as("user-1"), &foundryv1.PreviewFoundrySlackLinkRequest{WorkspaceId: slackWS, Token: token})
		assert.Equal(t, codes.Unavailable, status.Code(err))
	})
}
