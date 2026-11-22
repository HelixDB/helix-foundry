package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/slack"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// fakeLinker is a standalone install's link to Helix: its Slack calls go
// to slack, as bot "xoxb-relayed".
type fakeLinker struct {
	state    RelayState
	codes    []string
	err      error
	unlinked bool
	slack    *slack.Client
	// installs are the workspaces it started adding Slack for.
	installs []string
}

func (l *fakeLinker) StartSlackInstall(_ context.Context, workspaceID string) (string, error) {
	if !l.state.Linked {
		return "", foundrystore.ErrNotFound
	}
	l.installs = append(l.installs, workspaceID)
	return "https://slack.com/oauth/v2/authorize?state=relayed", l.err
}

func (l *fakeLinker) Slack(_ context.Context, install *model.SlackInstall) (*slack.Client, slack.Bot, error) {
	return l.slack, slack.Bot{InstallID: install.ID, Token: "xoxb-relayed"}, l.err
}

func (l *fakeLinker) Relay(context.Context) (RelayState, error) { return l.state, l.err }

func (l *fakeLinker) StartLink(context.Context) (string, error) {
	return "https://foundry.example.com/relay/link?state=s", l.err
}

func (l *fakeLinker) CompleteLink(_ context.Context, code, state string) error {
	if l.err != nil {
		return l.err
	}
	l.codes = append(l.codes, code+" "+state)
	l.state = RelayState{Linked: true, HostedOrigin: "https://ingest.helix-db.com", AccountEmail: "ada@acme.com",
		LinkedAt: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC), SlackApprovals: true}
	return nil
}

func (l *fakeLinker) SetSlackApprovals(_ context.Context, on bool) error {
	if !l.state.Linked {
		return foundrystore.ErrNotFound
	}
	l.state.SlackApprovals = on
	return l.err
}

func (l *fakeLinker) Unlink(context.Context) error {
	l.unlinked, l.state = true, RelayState{}
	return l.err
}

func TestLocalRelay(t *testing.T) {
	f, _ := newRelayFixture(t)
	get := func(ctx context.Context) (*foundryv1.FoundryRelayResponse, error) {
		return f.s.GetFoundryRelay(ctx, &foundryv1.GetFoundryRelayRequest{WorkspaceId: slackWS})
	}
	_, err := get(as("user-1"))
	assert.Equal(t, "FOUNDRY_RELAY_LOCAL_ONLY", reasonOf(err), "hosted Helix has no link of its own")

	linker := &fakeLinker{}
	f.s.deps.Relay = linker
	_, err = get(as("user-2"))
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = f.s.GetFoundryRelay(as("user-1"), &foundryv1.GetFoundryRelayRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	got, err := get(as("user-1"))
	require.NoError(t, err)
	assert.Equal(t, &foundryv1.FoundryRelay{}, got.GetRelay())

	started, err := f.s.StartFoundryRelayLink(as("user-1"), &foundryv1.StartFoundryRelayLinkRequest{WorkspaceId: slackWS})
	require.NoError(t, err)
	assert.Equal(t, "https://foundry.example.com/relay/link?state=s", started.GetAuthorizeUrl())

	complete := func(code, state string) (*foundryv1.FoundryRelayResponse, error) {
		return f.s.CompleteFoundryRelayLink(as("user-1"), &foundryv1.CompleteFoundryRelayLinkRequest{WorkspaceId: slackWS, Code: code, State: state})
	}
	_, err = complete("", relayState)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = complete("code", "short")
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	linker.err = foundrystore.ErrNotFound
	_, err = complete("code", relayState)
	assert.Equal(t, "FOUNDRY_RELAY_LINK_EXPIRED", reasonOf(err))
	linker.err = errors.New("Helix unreachable")
	_, err = complete("code", relayState)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	linker.err = nil
	got, err = complete("code", relayState)
	require.NoError(t, err)
	assert.Equal(t, []string{"code " + relayState}, linker.codes)
	assert.True(t, got.GetRelay().GetLinked())
	assert.Equal(t, "ada@acme.com", got.GetRelay().GetAccountEmail())
	assert.True(t, got.GetRelay().GetSlackApprovals())
	assert.Nil(t, got.GetRelay().GetLastOkAt())
	ok := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	linker.state.LastOKAt = &ok
	got, err = f.s.UpdateFoundryRelay(as("user-1"), &foundryv1.UpdateFoundryRelayRequest{WorkspaceId: slackWS, SlackApprovals: false})
	require.NoError(t, err)
	assert.False(t, got.GetRelay().GetSlackApprovals())
	assert.Equal(t, ok, got.GetRelay().GetLastOkAt().AsTime())

	got, err = f.s.UnlinkFoundryRelay(as("user-1"), &foundryv1.UnlinkFoundryRelayRequest{WorkspaceId: slackWS})
	require.NoError(t, err)
	assert.True(t, linker.unlinked)
	assert.False(t, got.GetRelay().GetLinked())
	_, err = f.s.UpdateFoundryRelay(as("user-1"), &foundryv1.UpdateFoundryRelayRequest{WorkspaceId: slackWS, SlackApprovals: true})
	assert.Equal(t, codes.NotFound, status.Code(err), "unlinked")

	linker.err = errors.New("disk full")
	for name, call := range map[string]func() error{
		"get": func() error { _, err := get(as("user-1")); return err },
		"start": func() error {
			_, err := f.s.StartFoundryRelayLink(as("user-1"), &foundryv1.StartFoundryRelayLinkRequest{WorkspaceId: slackWS})
			return err
		},
		"unlink": func() error {
			_, err := f.s.UnlinkFoundryRelay(as("user-1"), &foundryv1.UnlinkFoundryRelayRequest{WorkspaceId: slackWS})
			return err
		},
	} {
		assert.Equal(t, codes.Unavailable, status.Code(call()), name)
	}
	linker.state.Linked = true
	_, err = f.s.UpdateFoundryRelay(as("user-1"), &foundryv1.UpdateFoundryRelayRequest{WorkspaceId: slackWS})
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

func TestSetFoundrySlackAccess(t *testing.T) {
	f, r := newRelayFixture(t)
	set := func(slackUser string, access foundryv1.FoundrySlackLinkAccess) (*foundryv1.FoundrySlackAccessResponse, error) {
		return f.s.SetFoundrySlackAccess(as("user-1"), &foundryv1.SetFoundrySlackAccessRequest{WorkspaceId: slackWS, SlackUserId: slackUser, Access: access})
	}
	read, none := foundryv1.FoundrySlackLinkAccess_FOUNDRY_SLACK_LINK_ACCESS_READ, foundryv1.FoundrySlackLinkAccess_FOUNDRY_SLACK_LINK_ACCESS_NONE
	_, err := set("U0BOB0002", read)
	assert.Equal(t, "FOUNDRY_SLACK_LOCAL_ONLY", reasonOf(err))
	f.cfg.Host.Local = true
	for name, access := range map[string]foundryv1.FoundrySlackLinkAccess{
		"unspecified": foundryv1.FoundrySlackLinkAccess_FOUNDRY_SLACK_LINK_ACCESS_UNSPECIFIED,
		"full":        foundryv1.FoundrySlackLinkAccess_FOUNDRY_SLACK_LINK_ACCESS_FULL,
	} {
		_, err := set("U0BOB0002", access)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), name)
	}
	_, err = set("not a user", read)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = f.s.SetFoundrySlackAccess(as("user-2"), &foundryv1.SetFoundrySlackAccessRequest{WorkspaceId: slackWS, SlackUserId: "U0BOB0002", Access: read})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = set("U0BOB0002", read)
	assert.Equal(t, codes.NotFound, status.Code(err), "no install")

	install := r.install(t, f.keys, model.SlackPerPerson{})
	r.slackStore.links = []*model.SlackLink{{InstallID: install.ID, SlackUserID: slackAda, UserID: "user-1", Access: model.SlackFullAccess{}}}
	_, err = set(slackAda, none)
	assert.Equal(t, "FOUNDRY_SLACK_OWNER_LINK", reasonOf(err), "the owner's own link isn't changed here")
	_, err = set("U0BOB0002", read)
	assert.Equal(t, codes.NotFound, status.Code(err), "Bob never asked")

	// Bob asks, is allowed read-only, and shows so in the list.
	r.requests = []*model.SlackAccessRequest{{InstallID: install.ID, SlackUserID: "U0BOB0002", SlackName: "Bob", CreatedAt: time.Now()}}
	listed, err := f.s.ListFoundrySlackLinks(as("user-1"), &foundryv1.ListFoundrySlackLinksRequest{WorkspaceId: slackWS})
	require.NoError(t, err)
	require.Len(t, listed.GetRequests(), 1)
	assert.Equal(t, "Bob", listed.GetRequests()[0].GetSlackName())
	allowed, err := set("U0BOB0002", read)
	require.NoError(t, err)
	assert.Equal(t, read, allowed.GetLink().GetAccess())
	assert.Equal(t, "user-1", allowed.GetLink().GetUserId(), "as the owner")
	listed, err = f.s.ListFoundrySlackLinks(as("user-1"), &foundryv1.ListFoundrySlackLinksRequest{WorkspaceId: slackWS})
	require.NoError(t, err)
	assert.Empty(t, listed.GetRequests())
	require.Len(t, listed.GetLinks(), 2)
	assert.Equal(t, foundryv1.FoundrySlackLinkAccess_FOUNDRY_SLACK_LINK_ACCESS_FULL, listed.GetLinks()[0].GetAccess())

	// Removed, Bob's link is gone; a request is dropped the same way.
	out, err := set("U0BOB0002", none)
	require.NoError(t, err)
	assert.Nil(t, out.GetLink())
	_, err = r.GetFoundrySlackLink(context.Background(), install.ID, "U0BOB0002")
	assert.ErrorIs(t, err, foundrystore.ErrNotFound)
	r.requests = []*model.SlackAccessRequest{{InstallID: install.ID, SlackUserID: "U0CAT0003", SlackName: "Cat"}}
	_, err = set("U0CAT0003", none)
	require.NoError(t, err)
	assert.Empty(t, r.requests)
}
