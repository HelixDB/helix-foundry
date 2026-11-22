package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/keytoken"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The relay's tests run the API against relayStore: the Slack fake's rows
// and the relay's, in memory, as the store keeps them (its own tests hold
// its rules on MySQL).

const (
	relayLWS      = "01JLOCALWORKSPACE000000001" // the linked install's workspace
	relayVerify   = "a-pkce-verifier-of-forty-three-characters-or-more"
	relayRedirect = "http://localhost:3000/settings/slack"
	relayState    = "the-installs-own-state"
)

// relayStore is the Slack fake with the relay's rows.
type relayStore struct {
	*slackStore
	codes      map[string]*model.RelayCode
	codesUsed  map[string]bool
	links      map[string]*model.RelayLink // by token hash
	seen       []string                    // link id and address of each contact recorded
	acked      [][]string
	feeds      []model.RelayFeedOn
	waiting    int // claims' looks that find something, then nothing
	leases     []model.RelayClaim
	budgets    []model.RelayBudget
	deliveries []*model.RelayDelivery
	seenBy     map[string]model.RelaySeen // delivery id → what it let the install name
	channels   map[string]bool            // install/channel
	seenAgo    map[string]time.Duration   // install/channel: last seen this long ago (0: just now)
	users      map[string]bool            // install/user
	usage      map[string]model.RelayUsage
	slots      map[model.SlackSlotMethod]bool // due
	deferred   map[model.SlackSlotMethod]time.Duration
	relayFeed  []model.RelayFeedAppend
	feedOut    model.SlackAppendOutcome
	requests   []*model.SlackAccessRequest
	relayFail  error
}

func newRelayStore() *relayStore {
	return &relayStore{slackStore: newSlackStore(), codes: map[string]*model.RelayCode{}, codesUsed: map[string]bool{},
		links: map[string]*model.RelayLink{}, seenBy: map[string]model.RelaySeen{}, channels: map[string]bool{}, seenAgo: map[string]time.Duration{}, users: map[string]bool{},
		usage: map[string]model.RelayUsage{}, slots: map[model.SlackSlotMethod]bool{model.SlackSlotHistory: true, model.SlackSlotReplies: true},
		deferred: map[model.SlackSlotMethod]time.Duration{}, feedOut: model.SlackAppended}
}

// link adds a live link of slackWS made by user-1, whose token it returns.
func (r *relayStore) link(t *testing.T) (*model.RelayLink, string) {
	t.Helper()
	token, err := keytoken.NewRelay()
	require.NoError(t, err)
	l := &model.RelayLink{ID: "link-1", WorkspaceID: slackWS, CreatedByUserID: "user-1", Name: "Ada's laptop", RedirectOrigin: "http://localhost:3000",
		CreatedAt: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	r.links[hashToken(token)] = l
	return l, token
}

// relayedInstall adds a live install of the fixtures' team serving the
// link's local workspace, its token sealed as the API seals it.
func (r *relayStore) relayedInstall(t *testing.T, f slackFixture, feedOn bool) *model.SlackInstall {
	t.Helper()
	i := r.install(t, f.keys, model.SlackPerPerson{})
	i.Destination, i.FeedOn = model.SlackServesRelay{LinkID: "link-1", LocalWorkspaceID: relayLWS}, feedOn
	return i
}

func (r *relayStore) GetFoundrySlackInstall(_ context.Context, ws string) (*model.SlackInstall, error) {
	return r.live(func(i *model.SlackInstall) bool {
		_, relayed := i.Destination.(model.SlackServesRelay)
		return i.WorkspaceID == ws && !relayed
	})
}

func (r *relayStore) CreateFoundryRelayCode(_ context.Context, c *model.RelayCode, ttl time.Duration) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c.ExpiresAt = time.Now().Add(ttl)
	r.codes[c.CodeHash] = c
	return r.relayFail
}

func (r *relayStore) RedeemFoundryRelayCode(_ context.Context, codeHash, verifier, linkID, tokenHash string) (*model.RelayLink, error) {
	if r.relayFail != nil {
		return nil, r.relayFail
	}
	c, ok := r.codes[codeHash]
	if !ok || r.codesUsed[codeHash] || model.RelayCodeChallenge(verifier) != c.CodeChallenge {
		return nil, foundrystore.ErrNotFound
	}
	r.codesUsed[codeHash] = true
	origin, err := model.ParseRelayRedirect(c.RedirectURI)
	if err != nil {
		return nil, err
	}
	l := &model.RelayLink{ID: linkID, WorkspaceID: c.WorkspaceID, CreatedByUserID: c.UserID, Name: c.Name, RedirectOrigin: origin, CreatedAt: time.Now()}
	r.links[tokenHash] = l
	return l, nil
}

func (r *relayStore) GetFoundryRelayLinkByToken(_ context.Context, tokenHash string) (*model.RelayLink, error) {
	if r.relayFail != nil {
		return nil, r.relayFail
	}
	l, ok := r.links[tokenHash]
	if !ok || l.RevokedAt != nil {
		return nil, foundrystore.ErrNotFound
	}
	return l, nil
}

func (r *relayStore) ListFoundryRelayLinks(_ context.Context, ws string) ([]*model.RelayLink, error) {
	var out []*model.RelayLink
	for _, l := range r.links {
		if l.WorkspaceID == ws && l.RevokedAt == nil {
			out = append(out, l)
		}
	}
	return out, r.relayFail
}

func (r *relayStore) RevokeFoundryRelayLink(_ context.Context, ws, linkID string) error {
	for _, l := range r.links {
		if l.ID == linkID && l.WorkspaceID == ws && l.RevokedAt == nil {
			now := time.Now()
			l.RevokedAt = &now
			for _, i := range r.installs {
				if d, ok := i.Destination.(model.SlackServesRelay); ok && d.LinkID == linkID {
					i.RevokedAt = &now
				}
			}
			return nil
		}
	}
	return foundrystore.ErrNotFound
}

func (r *relayStore) SeeFoundryRelayLink(_ context.Context, linkID, addr string) error {
	r.seen = append(r.seen, linkID+" "+addr)
	return nil
}

func (r *relayStore) relayInstalls(linkID string) []*model.SlackInstall {
	var out []*model.SlackInstall
	for _, i := range r.installs {
		if d, ok := i.Destination.(model.SlackServesRelay); ok && d.LinkID == linkID && i.RevokedAt == nil {
			out = append(out, i)
		}
	}
	return out
}

func (r *relayStore) ListFoundryRelayInstalls(_ context.Context, linkID string) ([]*model.SlackInstall, error) {
	return r.relayInstalls(linkID), nil
}

func (r *relayStore) GetFoundryRelayInstall(_ context.Context, linkID, lws string) (*model.SlackInstall, error) {
	for _, i := range r.relayInstalls(linkID) {
		if i.Destination.(model.SlackServesRelay).LocalWorkspaceID == lws {
			return i, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (r *relayStore) AckFoundryRelay(_ context.Context, _ string, ids []string) (int, error) {
	if len(ids) > 0 {
		r.acked = append(r.acked, ids)
	}
	return len(ids), r.relayFail
}

func (r *relayStore) SetFoundryRelayFeeds(_ context.Context, _ string, feeds []model.RelayFeedOn) error {
	r.feeds = append(r.feeds, feeds...)
	return nil
}

func (r *relayStore) FoundryRelayWaiting(context.Context, string) (bool, error) {
	return len(r.leases) > 0 || r.waiting > 0, nil
}

func (r *relayStore) LeaseFoundryRelay(_ context.Context, _ string, lease time.Duration, budget model.RelayBudget) (model.RelayClaim, error) {
	r.budgets = append(r.budgets, budget)
	if r.waiting > 0 {
		r.waiting--
		return model.RelayClaim{}, nil
	}
	if len(r.leases) == 0 {
		return model.RelayClaim{}, nil
	}
	claim := r.leases[0]
	r.leases = r.leases[1:]
	return claim, nil
}

func (r *relayStore) QueueFoundryRelayDelivery(_ context.Context, d *model.RelayDelivery, ttl time.Duration, seen model.RelaySeen) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if ttl != 10*time.Minute {
		return errors.New("the delivery's lifetime is relay.delivery_ttl")
	}
	if _, err := r.live(func(i *model.SlackInstall) bool { return i.ID == d.InstallID }); err != nil {
		return err
	}
	for _, held := range r.deliveries {
		if held.DedupeKey == d.DedupeKey {
			return foundrystore.ErrConflict
		}
	}
	r.deliveries = append(r.deliveries, d)
	r.seenBy[d.ID] = seen
	return nil
}

func (r *relayStore) SeenFoundryRelayChannel(_ context.Context, installID, channelID string) (time.Duration, bool, error) {
	return r.seenAgo[installID+"/"+channelID], r.channels[installID+"/"+channelID], r.relayFail
}

func (r *relayStore) ForgetFoundryRelayChannel(_ context.Context, installID, channelID string) error {
	delete(r.channels, installID+"/"+channelID)
	return r.relayFail
}

func (r *relayStore) KnowsFoundryRelayUsers(_ context.Context, installID string, userIDs []string) (bool, error) {
	for _, u := range userIDs {
		if !r.users[installID+"/"+u] {
			return false, r.relayFail
		}
	}
	return len(userIDs) > 0, r.relayFail
}

func (r *relayStore) RecordFoundryRelayChannels(_ context.Context, installID string, channelIDs []string) error {
	for _, c := range channelIDs {
		r.channels[installID+"/"+c] = true
	}
	return nil
}

func (r *relayStore) CountFoundryRelayUsage(_ context.Context, linkID string, u model.RelayUsage) error {
	held := r.usage[linkID]
	held.Deliveries, held.Calls, held.Refused = held.Deliveries+u.Deliveries, held.Calls+u.Calls, held.Refused+u.Refused
	r.usage[linkID] = held
	return nil
}

func (r *relayStore) GetFoundryRelayUsage(_ context.Context, linkID string, _ time.Time) (model.RelayUsage, error) {
	return r.usage[linkID], nil
}

func (r *relayStore) ReserveFoundrySlackSlot(_ context.Context, _ string, method model.SlackSlotMethod, every time.Duration) (bool, error) {
	due := r.slots[method]
	r.slots[method] = false
	return due, nil
}

func (r *relayStore) DeferFoundrySlackSlot(_ context.Context, _ string, method model.SlackSlotMethod, after time.Duration) error {
	r.deferred[method] = after
	return nil
}

func (r *relayStore) CreateFoundrySlackRelayInstallState(_ context.Context, linkID, lws, hash string, ttl time.Duration) error {
	for _, l := range r.links {
		if l.ID == linkID && l.RevokedAt == nil {
			r.states[hash] = &slackState{SlackInstallState: model.SlackInstallState{StateHash: hash, WorkspaceID: l.WorkspaceID,
				UserID: l.CreatedByUserID, Destination: model.SlackServesRelay{LinkID: linkID, LocalWorkspaceID: lws}, ReturnTo: l.ReturnTo(),
				ExpiresAt: time.Now().Add(ttl)}, ttl: ttl}
			return nil
		}
	}
	return foundrystore.ErrNotFound
}

func (r *relayStore) AppendFoundryRelayFeed(_ context.Context, installID string, a model.RelayFeedAppend, limits model.RelayFeedLimits) (
	model.RelayFeedAppended, error) {
	if err := a.Validate(); err != nil {
		return model.RelayFeedAppended{}, err
	}
	if limits.TTL != 24*time.Hour || limits.MaxRows != 100_000 {
		return model.RelayFeedAppended{}, errors.New("the relay feed's limits are relay.feed_ttl and feed_max_rows")
	}
	if _, err := r.live(func(i *model.SlackInstall) bool { return i.ID == installID && (i.FeedOn || i.FollowOn) }); err != nil {
		return model.RelayFeedAppended{Outcome: model.SlackAppendUnbound}, nil
	}
	r.relayFeed = append(r.relayFeed, a)
	return model.RelayFeedAppended{Outcome: r.feedOut, Rows: len(a.Rows) + len(a.Gaps)}, nil
}

func (r *relayStore) ListFoundrySlackAccessRequests(context.Context, string) ([]*model.SlackAccessRequest, error) {
	return r.requests, nil
}

func (r *relayStore) AllowFoundrySlackUser(_ context.Context, installID, slackUserID, userID string) (*model.SlackLink, error) {
	r.requests = slices.DeleteFunc(r.requests, func(q *model.SlackAccessRequest) bool { return q.SlackUserID == slackUserID })
	_ = r.DeleteFoundrySlackLink(context.Background(), installID, slackUserID)
	l := &model.SlackLink{InstallID: installID, SlackUserID: slackUserID, UserID: userID, Access: model.SlackReadAccess{}, CreatedAt: time.Now()}
	r.slackStore.links = append(r.slackStore.links, l)
	return l, nil
}

func (r *relayStore) DeleteFoundrySlackAccessRequest(_ context.Context, _, slackUserID string) error {
	r.requests = slices.DeleteFunc(r.requests, func(q *model.SlackAccessRequest) bool { return q.SlackUserID == slackUserID })
	return nil
}

// newRelayFixture is the Slack fixture over relayStore, with the relay on.
func newRelayFixture(t *testing.T) (slackFixture, *relayStore) {
	t.Helper()
	f := newSlackFixture(t)
	r := newRelayStore()
	f.db = r.slackStore
	f.s.deps.DB = r
	f.cfg.Foundry.Slack.Relay.Enabled = true
	f.cfg.Foundry.Slack.Relay.ClaimWait, f.cfg.Foundry.Slack.Relay.ClaimPoll = 50*time.Millisecond, 10*time.Millisecond
	return f, r
}

func TestAuthorizeAndRedeemRelayLink(t *testing.T) {
	f, r := newRelayFixture(t)
	authorize := func(ctx context.Context, mutate func(*foundryv1.AuthorizeFoundryRelayLinkRequest)) (*foundryv1.AuthorizeFoundryRelayLinkResponse, error) {
		req := &foundryv1.AuthorizeFoundryRelayLinkRequest{WorkspaceId: slackWS, State: relayState, CodeChallenge: model.RelayCodeChallenge(relayVerify),
			RedirectUri: relayRedirect, Name: "Ada's laptop"}
		if mutate != nil {
			mutate(req)
		}
		return f.s.AuthorizeFoundryRelayLink(ctx, req)
	}
	for name, tc := range map[string]struct {
		ctx    context.Context
		mutate func(*foundryv1.AuthorizeFoundryRelayLinkRequest)
		code   codes.Code
	}{
		"no workspace":   {as("user-1"), func(q *foundryv1.AuthorizeFoundryRelayLinkRequest) { q.WorkspaceId = "" }, codes.InvalidArgument},
		"a member":       {as("user-2"), nil, codes.PermissionDenied},
		"a key":          {ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceUser, UserID: "user-1", ServiceCredentialID: "c"}), nil, codes.PermissionDenied},
		"a short state":  {as("user-1"), func(q *foundryv1.AuthorizeFoundryRelayLinkRequest) { q.State = "short" }, codes.InvalidArgument},
		"a state with /": {as("user-1"), func(q *foundryv1.AuthorizeFoundryRelayLinkRequest) { q.State = relayState + "/x" }, codes.InvalidArgument},
		"no challenge":   {as("user-1"), func(q *foundryv1.AuthorizeFoundryRelayLinkRequest) { q.CodeChallenge = "plain" }, codes.InvalidArgument},
		"a remote redirect": {as("user-1"), func(q *foundryv1.AuthorizeFoundryRelayLinkRequest) {
			q.RedirectUri = "https://evil.example/settings/slack"
		}, codes.InvalidArgument},
		"another path": {as("user-1"), func(q *foundryv1.AuthorizeFoundryRelayLinkRequest) { q.RedirectUri = "http://localhost:3000/" }, codes.InvalidArgument},
		"no name":      {as("user-1"), func(q *foundryv1.AuthorizeFoundryRelayLinkRequest) { q.Name = " " }, codes.InvalidArgument},
	} {
		_, err := authorize(tc.ctx, tc.mutate)
		assert.Equal(t, tc.code, status.Code(err), "%s: %v", name, err)
	}
	f.cfg.Foundry.Slack.InstallAllowlist = []string{"another"}
	_, err := authorize(as("user-1"), nil)
	assert.Equal(t, "FOUNDRY_SLACK_NOT_OFFERED", reasonOf(err))
	f.cfg.Foundry.Slack.InstallAllowlist = nil
	f.cfg.Foundry.Slack.Relay.Enabled = false
	_, err = authorize(as("user-1"), nil)
	assert.Equal(t, "FOUNDRY_RELAY_DISABLED", reasonOf(err))
	f.cfg.Foundry.Slack.Relay.Enabled = true
	assert.Empty(t, r.codes)

	// An admin authorizes; the browser goes back with the code and the state.
	resp, err := authorize(as("user-3"), nil)
	require.NoError(t, err)
	back, err := url.Parse(resp.GetRedirectUrl())
	require.NoError(t, err)
	assert.Equal(t, relayRedirect, back.Scheme+"://"+back.Host+back.Path)
	assert.Equal(t, relayState, back.Query().Get("state"))
	code := back.Query().Get("code")
	require.Len(t, r.codes, 1)
	held := r.codes[hashToken(code)]
	require.NotNil(t, held, "the code is kept as its hash")
	assert.Equal(t, "user-3", held.UserID)
	assert.WithinDuration(t, time.Now().Add(10*time.Minute), held.ExpiresAt, time.Minute)

	// Only the door redeems it, with the verifier.
	redeem := func(ctx context.Context, code, verifier string) (*foundryv1.RedeemFoundryRelayCodeResponse, error) {
		return f.s.RedeemFoundryRelayCode(ctx, &foundryv1.RedeemFoundryRelayCodeRequest{Code: code, CodeVerifier: verifier})
	}
	_, err = redeem(as("user-3"), code, relayVerify)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = redeem(door(), code, "short")
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = redeem(door(), "", relayVerify)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = redeem(door(), code, relayVerify+"-another")
	assert.Equal(t, codes.NotFound, status.Code(err), "another verifier")
	out, err := redeem(door(), code, relayVerify)
	require.NoError(t, err)
	assert.NoError(t, keytoken.ValidateRelay(out.GetToken()))
	assert.Equal(t, slackWS, out.GetWorkspaceId())
	assert.Empty(t, out.GetAccountEmail(), "user-3 has no email here")
	link := r.links[hashToken(out.GetToken())]
	require.NotNil(t, link, "the token is kept as its hash")
	assert.Equal(t, out.GetLinkId(), link.ID)
	_, err = redeem(door(), code, relayVerify)
	assert.Equal(t, codes.NotFound, status.Code(err), "once")

	// Ada's link answers her email.
	resp, err = authorize(as("user-1"), nil)
	require.NoError(t, err)
	back, _ = url.Parse(resp.GetRedirectUrl())
	out, err = redeem(door(), back.Query().Get("code"), relayVerify)
	require.NoError(t, err)
	assert.Equal(t, "Ada@Acme.com", out.GetAccountEmail())

	// A member who lost the role before the install came back links nothing.
	resp, err = authorize(as("user-3"), nil)
	require.NoError(t, err)
	back, _ = url.Parse(resp.GetRedirectUrl())
	f.s.deps.People = slackPeople{roles: map[string]ports.Role{"user-3": ports.RoleMember}}
	_, err = redeem(door(), back.Query().Get("code"), relayVerify)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	revoked := 0
	for _, l := range r.links {
		if l.RevokedAt != nil {
			revoked++
		}
	}
	assert.Equal(t, 1, revoked, "its link is revoked at once")

	r.relayFail = errors.New("mysql down")
	_, err = redeem(door(), "anything", relayVerify)
	assert.Equal(t, codes.Internal, status.Code(err))
	f.cfg.Foundry.Slack.Relay.Enabled = false
	_, err = redeem(door(), code, relayVerify)
	assert.Equal(t, "FOUNDRY_RELAY_DISABLED", reasonOf(err))
}

func TestValidateAndRevokeRelayToken(t *testing.T) {
	f, r := newRelayFixture(t)
	link, token := r.link(t)
	got, err := f.s.ValidateFoundryRelayToken(door(), &foundryv1.ValidateFoundryRelayTokenRequest{TokenSha256: hashToken(token)})
	require.NoError(t, err)
	assert.Equal(t, &foundryv1.ValidateFoundryRelayTokenResponse{Valid: true, LinkId: link.ID, WorkspaceId: slackWS}, got)
	got, err = f.s.ValidateFoundryRelayToken(door(), &foundryv1.ValidateFoundryRelayTokenRequest{TokenSha256: hashToken("other")})
	require.NoError(t, err)
	assert.False(t, got.GetValid())
	_, err = f.s.ValidateFoundryRelayToken(door(), &foundryv1.ValidateFoundryRelayTokenRequest{TokenSha256: "abc"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = f.s.ValidateFoundryRelayToken(as("user-1"), &foundryv1.ValidateFoundryRelayTokenRequest{TokenSha256: hashToken(token)})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	r.relayFail = errors.New("mysql down")
	_, err = f.s.ValidateFoundryRelayToken(door(), &foundryv1.ValidateFoundryRelayTokenRequest{TokenSha256: hashToken(token)})
	assert.Equal(t, codes.Unavailable, status.Code(err), "the door answers 503, not 401")
	r.relayFail = nil

	install := r.relayedInstall(t, f, false)
	_, err = f.s.RevokeFoundryRelayToken(door(), &foundryv1.RevokeFoundryRelayTokenRequest{TokenSha256: hashToken(token)})
	require.NoError(t, err)
	assert.NotNil(t, link.RevokedAt)
	assert.NotNil(t, install.RevokedAt, "its installs go with it")
	_, err = f.s.RevokeFoundryRelayToken(door(), &foundryv1.RevokeFoundryRelayTokenRequest{TokenSha256: hashToken(token)})
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestClaimFoundryRelayDeliveries(t *testing.T) {
	f, r := newRelayFixture(t)
	_, token := r.link(t)
	install := r.relayedInstall(t, f, true)
	claim := func(ctx context.Context, mutate func(*foundryv1.ClaimFoundryRelayDeliveriesRequest)) (*foundryv1.ClaimFoundryRelayDeliveriesResponse, error) {
		req := &foundryv1.ClaimFoundryRelayDeliveriesRequest{TokenSha256: hashToken(token), WaitMs: 20_000, ClientAddress: "203.0.113.9"}
		if mutate != nil {
			mutate(req)
		}
		return f.s.ClaimFoundryRelayDeliveries(ctx, req)
	}
	for name, mutate := range map[string]func(*foundryv1.ClaimFoundryRelayDeliveriesRequest){
		"a negative wait": func(q *foundryv1.ClaimFoundryRelayDeliveriesRequest) { q.WaitMs = -1 },
		"a negative max":  func(q *foundryv1.ClaimFoundryRelayDeliveriesRequest) { q.Max = -1 },
		"too many":        func(q *foundryv1.ClaimFoundryRelayDeliveriesRequest) { q.Max = 11 },
		"too many acks":   func(q *foundryv1.ClaimFoundryRelayDeliveriesRequest) { q.Acks = make([]string, 2049) },
		"a feed of no workspace": func(q *foundryv1.ClaimFoundryRelayDeliveriesRequest) {
			q.Feeds = []*foundryv1.FoundryRelayFeedOn{{LocalWorkspaceId: "ws one", On: true}}
		},
	} {
		_, err := claim(door(), mutate)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), name)
	}
	_, err := claim(door(), func(q *foundryv1.ClaimFoundryRelayDeliveriesRequest) { q.TokenSha256 = hashToken("revoked") })
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	_, err = claim(as("user-1"), nil)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))

	// Nothing arrives: the claim waits relay.claim_wait (not the 20 s asked)
	// and answers the installs, the feed held off while hosted's is.
	start := time.Now()
	out, err := claim(door(), func(q *foundryv1.ClaimFoundryRelayDeliveriesRequest) {
		q.Acks = []string{"d-0"}
		q.Feeds = []*foundryv1.FoundryRelayFeedOn{{LocalWorkspaceId: relayLWS, On: true, Follow: true}}
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Empty(t, out.GetDeliveries())
	assert.EqualValues(t, 1, out.GetAcked())
	assert.Equal(t, [][]string{{"d-0"}}, r.acked)
	assert.Equal(t, []model.RelayFeedOn{{LocalWorkspaceID: relayLWS, On: false}}, r.feeds, "hosted's feed and follow-ups are off")
	assert.Equal(t, []string{"link-1 203.0.113.9"}, r.seen)
	require.Len(t, out.GetInstalls(), 1)
	got := out.GetInstalls()[0]
	assert.Equal(t, install.ID, got.GetInstallId())
	assert.Equal(t, relayLWS, got.GetLocalWorkspaceId())
	assert.Equal(t, slackTeam, got.GetTeamId())
	assert.Equal(t, slackAda, got.GetInstalledBySlackUserId())
	assert.True(t, got.GetFeedOn())
	assert.False(t, out.GetHostedFeed() || out.GetHostedFollow())

	// Something arrives after a claim that lost the race: deliveries, then
	// the feed, within the batch while hosted's feed is on.
	f.cfg.Foundry.Slack.Feed.Enabled = true
	r.budgets, r.feeds, r.waiting = nil, nil, 1
	day := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	r.leases = []model.RelayClaim{{
		Deliveries: []*model.RelayDelivery{{ID: "d-1", InstallID: install.ID, LocalWorkspaceID: relayLWS, DedupeKey: "e:Ev1", Kind: model.SlackIntakeEvent,
			Conversation: "c-1", SlackUserID: slackAda, Payload: json.RawMessage(`{"a":1}`)}},
		Feed: []model.RelayFeedItem{
			model.RelayFeedRow{ID: "f-1", InstallID: install.ID, LocalWorkspaceID: relayLWS, Row: model.SlackFeedRow{DedupeKey: "e:Ev2", ChannelID: "C1",
				Day: day, Kind: model.SlackRowMessage, Payload: json.RawMessage(`{"ts":"1.2"}`)}},
			model.RelayFeedGap{ID: "f-2", InstallID: install.ID, LocalWorkspaceID: relayLWS, Gap: model.SlackFeedGap{ChannelID: "C1", From: day,
				To: day.Add(time.Minute)}},
		},
	}}
	out, err = claim(door(), func(q *foundryv1.ClaimFoundryRelayDeliveriesRequest) {
		q.Max = 3
		q.Feeds = []*foundryv1.FoundryRelayFeedOn{{LocalWorkspaceId: relayLWS, On: true, Follow: true}}
	})
	require.NoError(t, err)
	assert.Equal(t, []model.RelayFeedOn{{LocalWorkspaceID: relayLWS, On: true}}, r.feeds, "follow-ups only while hosted's are on")
	assert.Equal(t, []model.RelayBudget{{Deliveries: 3, FeedRows: 500, FeedBytes: 4 << 20}, {Deliveries: 3, FeedRows: 500, FeedBytes: 4 << 20}}, r.budgets)
	require.Len(t, out.GetDeliveries(), 1)
	assert.Equal(t, &foundryv1.FoundryRelayDelivery{Id: "d-1", InstallId: install.ID, LocalWorkspaceId: relayLWS, Kind: "event", DedupeKey: "e:Ev1",
		Conversation: "c-1", SlackUserId: slackAda, PayloadJson: `{"a":1}`}, out.GetDeliveries()[0])
	require.Len(t, out.GetFeed(), 1)
	assert.Equal(t, "C1", out.GetFeed()[0].GetChannelId())
	assert.Equal(t, day, out.GetFeed()[0].GetDay().AsTime())
	assert.Equal(t, "message", out.GetFeed()[0].GetKind())
	require.Len(t, out.GetFeedGaps(), 1)
	assert.Equal(t, day.Add(time.Minute), out.GetFeedGaps()[0].GetTo().AsTime())
	assert.True(t, out.GetHostedFeed())
	f.cfg.Foundry.Slack.Follow.Enabled = true
	r.feeds = nil
	_, err = claim(door(), func(q *foundryv1.ClaimFoundryRelayDeliveriesRequest) {
		q.WaitMs = 0
		q.Feeds = []*foundryv1.FoundryRelayFeedOn{{LocalWorkspaceId: relayLWS, Follow: true}}
	})
	require.NoError(t, err)
	assert.Equal(t, []model.RelayFeedOn{{LocalWorkspaceID: relayLWS, Follow: true}}, r.feeds, "follow-ups with no data connector")

	// A claim whose caller goes answers what it has.
	gone, cancel := context.WithCancel(door())
	cancel()
	out, err = claim(gone, nil)
	require.NoError(t, err)
	assert.Empty(t, out.GetDeliveries())

	r.relayFail = errors.New("mysql down")
	_, err = claim(door(), nil)
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

func TestRelayedInstallIntake(t *testing.T) {
	f, r := newRelayFixture(t)
	r.link(t)
	install := r.relayedInstall(t, f, false)
	receive := func(route foundryv1.SlackRoute, body []byte) int32 {
		t.Helper()
		resp, err := f.s.ReceiveSlackRequest(door(), signed(route, body))
		require.NoError(t, err)
		return resp.GetHttpStatus()
	}
	mention := fixture(t, "events/mention.json")
	assert.EqualValues(t, http.StatusOK, receive(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, mention))
	require.Len(t, r.deliveries, 1, "a relayed install's request waits for its install")
	assert.Empty(t, r.intake, "and is never Helix's bot's")
	d := r.deliveries[0]
	assert.Equal(t, install.ID, d.InstallID)
	assert.Equal(t, model.SlackIntakeEvent, d.Kind)
	assert.NotEmpty(t, d.Conversation)
	seen := r.seenBy[d.ID]
	assert.NotEmpty(t, seen.ChannelID)
	assert.Contains(t, seen.UserIDs, d.SlackUserID)
	assert.EqualValues(t, http.StatusOK, receive(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, mention))
	assert.Len(t, r.deliveries, 1, "Slack's retry is a duplicate")

	dm := fixture(t, "events/dm.json")
	assert.EqualValues(t, http.StatusOK, receive(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, dm))
	assert.Len(t, r.deliveries, 2)
	stop := fixture(t, "events/agent_session_stopped.json")
	assert.EqualValues(t, http.StatusOK, receive(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, stop))
	require.Len(t, r.deliveries, 3)
	assert.Empty(t, r.deliveries[2].Conversation, "a stop waits behind nothing")
	r.deliveries = r.deliveries[:2]

	// A shared channel's mention is Helix's to refuse.
	shared := fixture(t, "events/mention_ext_shared.json")
	assert.EqualValues(t, http.StatusOK, receive(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, shared))
	assert.Len(t, r.deliveries, 2)
	assert.Len(t, r.intake, 1)

	// A click on the install's message is its install's.
	click := clickBody(t, "approve.json")
	assert.EqualValues(t, http.StatusOK, receive(foundryv1.SlackRoute_SLACK_ROUTE_INTERACTIVITY, click))
	require.Len(t, r.deliveries, 3)
	clicked := r.deliveries[2]
	assert.Equal(t, model.SlackIntakeInteraction, clicked.Kind)
	assert.Equal(t, model.RelaySeen{UserIDs: []string{clicked.SlackUserID}}, r.seenBy[clicked.ID])

	// With the relay off nothing is kept for it.
	f.cfg.Foundry.Slack.Relay.Enabled = false
	assert.EqualValues(t, http.StatusOK, receive(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/mention_in_thread.json")))
	assert.Len(t, r.deliveries, 3)
	assert.Len(t, r.intake, 1)
}

func TestRelaySeen(t *testing.T) {
	at := slack.Conversation{TeamID: slackTeam, ChannelID: "C0CHAN001", ThreadTS: "1.2"}
	assert.Equal(t, model.RelaySeen{ChannelID: "C0CHAN001", UserIDs: []string{slackAda, "U0BOB0002", "W0GRID003"}},
		relaySeen(at, slackAda, "ask <@U0BOB0002> and <@W0GRID003|grid>, not <@B123>, <!here> or <#C1|x>"))
	at.ExtShared = true
	assert.Equal(t, model.RelaySeen{UserIDs: []string{slackAda}}, relaySeen(at, slackAda, ""), "a shared channel is never one to name")
}
