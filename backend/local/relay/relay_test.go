package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/local/directory"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	linkA  = "01LINKAAAAAAAAAAAAAAAAAAAA"
	linkB  = "01LINKBBBBBBBBBBBBBBBBBBBB"
	tokenA = "hx_rl_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokenB = "hx_rl_" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	ws     = "01WSLOCALAAAAAAAAAAAAAAAAA"
)

// dir is the install's directory, in memory.
type dir struct {
	mu      sync.Mutex
	link    *directory.RelayLink
	pending map[string][2]string // state hash: sealed verifier, key
	spent   map[string]bool
	ok      []string
	fail    error
	// revokes are the links forgotten, waiting to be revoked.
	revokes  []directory.RelayRevoke
	failLink error
}

// forgetLink keeps the link for revoking, as the directory does.
func (d *dir) forgetLink() {
	if d.link != nil {
		d.revokes = append(d.revokes, directory.RelayRevoke{LinkID: d.link.LinkID, HostedOrigin: d.link.HostedOrigin, TokenSealed: d.link.TokenSealed,
			KeyID: d.link.KeyID})
	}
}

func (d *dir) RelayRevokes(context.Context) ([]directory.RelayRevoke, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.revokes), nil
}

func (d *dir) RelayRevoked(_ context.Context, linkID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.revokes = slices.DeleteFunc(d.revokes, func(r directory.RelayRevoke) bool { return r.LinkID == linkID })
	return nil
}

// waiting is the ids of the links waiting to be revoked.
func (d *dir) waiting() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, r := range d.revokes {
		out = append(out, r.LinkID)
	}
	return out
}

func (d *dir) Owner(context.Context) (directory.User, error) {
	return directory.User{ID: "user-owner", Email: directory.OwnerEmail}, d.fail
}

func (d *dir) RelayLink(context.Context) (directory.RelayLink, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.link == nil {
		return directory.RelayLink{}, store.ErrNotFound
	}
	return *d.link, nil
}

func (d *dir) LinkRelay(_ context.Context, l directory.RelayLink) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failLink != nil {
		return d.failLink
	}
	if d.link != nil && d.link.LinkID != l.LinkID {
		d.forgetLink()
	}
	l.LinkedAt = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	d.link = &l
	return nil
}

func (d *dir) UnlinkRelay(context.Context) (directory.RelayLink, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.link == nil {
		return directory.RelayLink{}, store.ErrNotFound
	}
	l := *d.link
	d.forgetLink()
	d.link = nil
	return l, nil
}

func (d *dir) SetRelaySlackApprovals(_ context.Context, on bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.link == nil {
		return store.ErrNotFound
	}
	d.link.SlackApprovals = on
	return nil
}

func (d *dir) RelayOK(_ context.Context, linkID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ok = append(d.ok, linkID)
	return nil
}

func (d *dir) StartRelayLink(_ context.Context, stateHash string, sealed []byte, keyID string) (time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending[stateHash] = [2]string{string(sealed), keyID}
	return time.Now().Add(directory.RelayPendingTTL), nil
}

func (d *dir) SpendRelayLink(_ context.Context, stateHash string) ([]byte, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.pending[stateHash]
	if !ok || d.spent[stateHash] {
		return nil, "", store.ErrNotFound
	}
	d.spent[stateHash] = true
	return []byte(p[0]), p[1], nil
}

// db is the install's Foundry store, as the relay writes it.
type db struct {
	mu       sync.Mutex
	mirrored [][]*model.SlackInstall
	intake   []*model.SlackIntake
	rows     map[string][]model.SlackFeedRow
	gaps     map[string][]model.SlackFeedGap
	feeds    []*model.SlackFeed
	failFeed error
	failGaps error
	failQ    error
	failM    error
}

func (s *db) GetFoundrySlackInstallByID(_ context.Context, id string) (*model.SlackInstall, error) {
	return &model.SlackInstall{ID: id, WorkspaceID: ws}, nil
}

func (s *db) MirrorFoundrySlackInstalls(_ context.Context, installs []*model.SlackInstall) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failM != nil {
		return s.failM
	}
	s.mirrored = append(s.mirrored, installs)
	return nil
}

func (s *db) QueueFoundrySlackIntake(_ context.Context, in *model.SlackIntake) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failQ != nil {
		return s.failQ
	}
	for _, q := range s.intake {
		if q.DedupeKey == in.DedupeKey {
			return store.ErrConflict
		}
	}
	s.intake = append(s.intake, in)
	return nil
}

func (s *db) ListFoundrySlackFeeds(_ context.Context, after string, limit int) ([]*model.SlackFeed, error) {
	var out []*model.SlackFeed
	for _, f := range s.feeds {
		if f.ID > after && len(out) < limit {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *db) AppendFoundrySlackFeed(_ context.Context, installID string, batch model.SlackFeedBatch, _ model.SlackFeedLimits) (model.SlackFeedAppended, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failFeed != nil {
		return model.SlackFeedAppended{}, s.failFeed
	}
	s.rows[installID] = append(s.rows[installID], batch.Rows...)
	return model.SlackFeedAppended{Outcome: model.SlackAppended, Rows: len(batch.Rows)}, nil
}

func (s *db) AppendFoundrySlackFeedGaps(_ context.Context, installID string, gaps []model.SlackFeedGap, _ time.Duration) (model.SlackFeedAppended, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failGaps != nil {
		return model.SlackFeedAppended{}, s.failGaps
	}
	s.gaps[installID] = append(s.gaps[installID], gaps...)
	return model.SlackFeedAppended{Outcome: model.SlackAppended}, nil
}

// door is Helix's door: each route answers with its handler, recording
// what it took.
type door struct {
	*httptest.Server
	mu     sync.Mutex
	took   []took
	routes map[string]func(took) (int, string)
}

// took is one request the door took: its path, bearer and JSON body.
type took struct {
	path, auth string
	body       map[string]any
}

func newDoor(t *testing.T) *door {
	d := &door{routes: map[string]func(took) (int, string){}}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := took{path: r.URL.Path, auth: r.Header.Get("Authorization")}
		_ = json.Unmarshal(raw, &c.body)
		d.mu.Lock()
		d.took = append(d.took, c)
		route := d.routes[r.URL.Path]
		d.mu.Unlock()
		code, body := http.StatusNotFound, `{"error":{"code":"not_found"}}`
		if route != nil {
			code, body = route(c)
		}
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *door) route(path string, h func(took) (int, string)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.routes[path] = h
}

func (d *door) calls(path string) []took {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []took
	for _, c := range d.took {
		if c.path == path {
			out = append(out, c)
		}
	}
	return out
}

// marshal is a claim's answer as the door writes it.
func marshal(t *testing.T, m *foundryv1.ClaimFoundryRelayDeliveriesResponse) string {
	t.Helper()
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	require.NoError(t, err)
	return string(b)
}

type fixture struct {
	r    *Relay
	dir  *dir
	db   *db
	door *door
	keys *secretbox.Keyring
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{5}, 32)})
	require.NoError(t, err)
	d := &dir{pending: map[string][2]string{}, spent: map[string]bool{}}
	s := &db{rows: map[string][]model.SlackFeedRow{}, gaps: map[string][]model.SlackFeedGap{}}
	dr := newDoor(t)
	cfg := config.Defaults().Foundry.Slack
	r, err := New(Options{Directory: d, DB: s, Keys: keys, Door: dr.URL, Site: "https://foundry.example.com", Origin: "http://localhost:3000",
		Name: "Ada's laptop", Slack: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	require.NoError(t, err)
	r.idle, r.backoff, r.retry = 10*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond
	return fixture{r: r, dir: d, db: s, door: dr, keys: keys}
}

// linked links the fixture's install as linkA, with tokenA.
func (f fixture) linked(t *testing.T) {
	t.Helper()
	sealed, keyID, err := f.keys.Seal([]byte(tokenA), tokenContext)
	require.NoError(t, err)
	require.NoError(t, f.dir.LinkRelay(context.Background(), directory.RelayLink{HostedOrigin: f.door.URL, LinkID: linkA, TokenSealed: sealed,
		KeyID: keyID, AccountEmail: "ada@acme.com", SlackApprovals: true}))
}

func TestNewRefuses(t *testing.T) {
	f := newFixture(t)
	good := f.r.o
	for name, mutate := range map[string]func(*Options){
		"no directory":    func(o *Options) { o.Directory = nil },
		"no store":        func(o *Options) { o.DB = nil },
		"no keyring":      func(o *Options) { o.Keys = nil },
		"no logger":       func(o *Options) { o.Logger = nil },
		"no name":         func(o *Options) { o.Name = "" },
		"a long name":     func(o *Options) { o.Name = strings.Repeat("n", 129) },
		"a door path":     func(o *Options) { o.Door = "https://ingest.example.com/v1" },
		"no site":         func(o *Options) { o.Site = "" },
		"an origin's ftp": func(o *Options) { o.Origin = "ftp://localhost" },
	} {
		o := good
		mutate(&o)
		_, err := New(o)
		assert.Error(t, err, name)
	}
	assert.Equal(t, "foundry-relay", f.r.Name())
}

// Linking is OAuth's PKCE: the owner's browser carries a state and the
// verifier's challenge to Helix's site, and the code Helix hands back is
// redeemed with the verifier, once, for the link's token, kept sealed.
func TestLinkingToHelix(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	state, err := f.r.Relay(ctx)
	require.NoError(t, err)
	assert.False(t, state.Linked)

	authorize, err := f.r.StartLink(ctx)
	require.NoError(t, err)
	u, err := url.Parse(authorize)
	require.NoError(t, err)
	assert.Equal(t, "https://foundry.example.com/relay/link", u.Scheme+"://"+u.Host+u.Path)
	q := u.Query()
	assert.Equal(t, "http://localhost:3000/settings/slack", q.Get("redirect_uri"))
	assert.Equal(t, "Ada's laptop", q.Get("name"))
	require.Len(t, q.Get("state"), 43)
	require.Contains(t, f.dir.pending, hashHex(q.Get("state")), "only the state's hash is kept")

	var verifier string
	f.door.route("/v1/relay/token", func(c took) (int, string) {
		verifier, _ = c.body["code_verifier"].(string)
		if c.body["code"] != "the-code" || c.auth != "" {
			return http.StatusNotFound, `{"error":{"code":"not_found"}}`
		}
		return http.StatusOK, `{"token":"` + tokenA + `","link_id":"` + linkA + `","workspace_id":"ws-helix","account_email":"ada@acme.com"}`
	})
	require.NoError(t, f.r.CompleteLink(ctx, "the-code", q.Get("state")))
	challenge := sha256.Sum256([]byte(verifier))
	assert.Equal(t, q.Get("code_challenge"), base64.RawURLEncoding.EncodeToString(challenge[:]), "the verifier is the challenge's")
	state, err = f.r.Relay(ctx)
	require.NoError(t, err)
	assert.True(t, state.Linked)
	assert.Equal(t, f.door.URL, state.HostedOrigin)
	assert.Equal(t, "ada@acme.com", state.AccountEmail)
	assert.True(t, state.SlackApprovals, "approving from Slack starts on")
	assert.NotContains(t, string(f.dir.link.TokenSealed), tokenA, "sealed")
	open, err := f.r.open(ctx)
	require.NoError(t, err)
	assert.Equal(t, tokenA, open.token)

	assert.ErrorIs(t, f.r.CompleteLink(ctx, "the-code", q.Get("state")), store.ErrNotFound, "a state is spent once")
	assert.ErrorIs(t, f.r.CompleteLink(ctx, "the-code", "unknown"), store.ErrNotFound)

	// A code Helix doesn't know, or an answer without a token.
	authorize, err = f.r.StartLink(ctx)
	require.NoError(t, err)
	u, _ = url.Parse(authorize)
	assert.ErrorIs(t, f.r.CompleteLink(ctx, "expired", u.Query().Get("state")), store.ErrNotFound)
	f.door.route("/v1/relay/token", func(took) (int, string) { return http.StatusOK, `{"token":"nope"}` })
	authorize, _ = f.r.StartLink(ctx)
	u, _ = url.Parse(authorize)
	assert.ErrorContains(t, f.r.CompleteLink(ctx, "the-code", u.Query().Get("state")), "without a relay token")
	f.door.route("/v1/relay/token", func(took) (int, string) {
		return http.StatusForbidden, `{"error":{"code":"forbidden","message":"not an admin"}}`
	})
	authorize, _ = f.r.StartLink(ctx)
	u, _ = url.Parse(authorize)
	assert.ErrorContains(t, f.r.CompleteLink(ctx, "the-code", u.Query().Get("state")), "not an admin")

	// Linking again replaces the link, and revokes the one before.
	f.door.route("/v1/relay/token", func(took) (int, string) {
		return http.StatusOK, `{"token":"` + tokenB + `","link_id":"` + linkB + `","account_email":"grace@acme.com"}`
	})
	f.door.route("/v1/relay/revoke", func(took) (int, string) { return http.StatusOK, `{}` })
	authorize, _ = f.r.StartLink(ctx)
	u, _ = url.Parse(authorize)
	require.NoError(t, f.r.CompleteLink(ctx, "the-code", u.Query().Get("state")))
	revoked := f.door.calls("/v1/relay/revoke")
	require.Len(t, revoked, 1)
	assert.Equal(t, "Bearer "+tokenA, revoked[0].auth)
	assert.Equal(t, linkB, f.dir.link.LinkID)
	assert.Empty(t, f.dir.waiting(), "Helix revoked it")
}

// A token redeemed that the install couldn't keep is revoked at once: no
// link is left live at Helix that no install holds. One Helix has no email
// for links all the same.
func TestATokenItCantKeepIsRevoked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.door.route("/v1/relay/token", func(took) (int, string) {
		return http.StatusOK, `{"token":"` + tokenA + `","link_id":"` + linkA + `"}`
	})
	f.door.route("/v1/relay/revoke", func(took) (int, string) { return http.StatusOK, `{}` })
	f.dir.failLink = errors.New("disk full")
	authorize, err := f.r.StartLink(ctx)
	require.NoError(t, err)
	u, _ := url.Parse(authorize)
	assert.ErrorContains(t, f.r.CompleteLink(ctx, "the-code", u.Query().Get("state")), "disk full")
	revoked := f.door.calls("/v1/relay/revoke")
	require.Len(t, revoked, 1)
	assert.Equal(t, "Bearer "+tokenA, revoked[0].auth)

	f.dir.failLink = nil
	authorize, _ = f.r.StartLink(ctx)
	u, _ = url.Parse(authorize)
	require.NoError(t, f.r.CompleteLink(ctx, "the-code", u.Query().Get("state")))
	state, err := f.r.Relay(ctx)
	require.NoError(t, err)
	assert.True(t, state.Linked)
	assert.Empty(t, state.AccountEmail, "Helix had no email for the account")
	assert.Len(t, f.door.calls("/v1/relay/revoke"), 1)
}

func TestApprovalsUnlinkAndInstalls(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	assert.ErrorIs(t, f.r.SetSlackApprovals(ctx, false), store.ErrNotFound)
	_, err := f.r.StartSlackInstall(ctx, ws)
	assert.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, f.r.Unlink(ctx), "unlinked, it does nothing")
	assert.Empty(t, f.door.took)

	f.linked(t)
	require.NoError(t, f.r.SetSlackApprovals(ctx, false))
	state, err := f.r.Relay(ctx)
	require.NoError(t, err)
	assert.False(t, state.SlackApprovals)
	approvals, err := f.r.Transport().SlackApprovals(ctx)
	require.NoError(t, err)
	assert.False(t, approvals)

	f.door.route("/v1/relay/slack-install", func(took) (int, string) {
		return http.StatusOK, `{"authorize_url":"https://slack.com/oauth/v2/authorize?state=s"}`
	})
	authorize, err := f.r.StartSlackInstall(ctx, ws)
	require.NoError(t, err)
	assert.Equal(t, "https://slack.com/oauth/v2/authorize?state=s", authorize)
	started := f.door.calls("/v1/relay/slack-install")
	require.Len(t, started, 1)
	assert.Equal(t, "Bearer "+tokenA, started[0].auth)
	assert.Equal(t, map[string]any{"local_workspace_id": ws, "return_to": "http://localhost:3000/settings/slack"}, started[0].body)

	client, bot, err := f.r.Slack(ctx, &model.SlackInstall{ID: "inst-1"})
	require.NoError(t, err)
	assert.NotNil(t, client)
	assert.Equal(t, ws, bot.Token, "the relayed bot names its local workspace")

	// Unlinking forgets the link and its installs here, then revokes it at
	// Helix; Helix failing to answer doesn't keep it, but its token waits to
	// be revoked.
	f.door.route("/v1/relay/revoke", func(took) (int, string) { return http.StatusServiceUnavailable, `{}` })
	require.NoError(t, f.r.Unlink(ctx))
	assert.Nil(t, f.dir.link)
	require.NotEmpty(t, f.db.mirrored)
	assert.Empty(t, f.db.mirrored[len(f.db.mirrored)-1], "no install is left")
	assert.Len(t, f.door.calls("/v1/relay/revoke"), 1)
	assert.Equal(t, []string{linkA}, f.dir.waiting())

	// Linked again to the same Helix, Slack isn't added while the old link
	// is live there: Helix would take the install for the old link's.
	sealed, keyID, err := f.keys.Seal([]byte(tokenB), tokenContext)
	require.NoError(t, err)
	require.NoError(t, f.dir.LinkRelay(ctx, directory.RelayLink{HostedOrigin: f.door.URL, LinkID: linkB, TokenSealed: sealed, KeyID: keyID}))
	_, err = f.r.StartSlackInstall(ctx, ws)
	assert.ErrorContains(t, err, "earlier link")
	assert.Len(t, f.door.calls("/v1/relay/slack-install"), 1, "nothing started")
	assert.Equal(t, "Bearer "+tokenA, f.door.calls("/v1/relay/revoke")[1].auth, "it tried again first")

	// Once Helix answers, or says the token is revoked already, it starts.
	f.door.route("/v1/relay/revoke", func(took) (int, string) { return http.StatusUnauthorized, `{"error":{"code":"invalid_token"}}` })
	_, err = f.r.StartSlackInstall(ctx, ws)
	require.NoError(t, err)
	assert.Empty(t, f.dir.waiting())
	assert.Len(t, f.door.calls("/v1/relay/slack-install"), 2)
}

// The component revokes the links the install forgot, linked or not, until
// Helix answers; a token that can't be opened can never be, and is
// forgotten.
func TestTheRelayRevokesForgottenLinks(t *testing.T) {
	f := newFixture(t)
	sealed, keyID, err := f.keys.Seal([]byte(tokenA), tokenContext)
	require.NoError(t, err)
	f.dir.revokes = []directory.RelayRevoke{{LinkID: linkA, HostedOrigin: f.door.URL, TokenSealed: sealed, KeyID: keyID},
		{LinkID: linkB, HostedOrigin: f.door.URL, TokenSealed: []byte("garbage"), KeyID: "k1"}}
	tries := 0
	f.door.route("/v1/relay/revoke", func(took) (int, string) {
		if tries++; tries < 3 {
			return http.StatusServiceUnavailable, `{}`
		}
		return http.StatusOK, `{}`
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.r.Start(ctx) }()
	require.Eventually(t, func() bool { return len(f.dir.waiting()) == 0 }, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, f.r.Stop(context.Background()))
	require.NoError(t, <-done)
	revoked := f.door.calls("/v1/relay/revoke")
	require.Len(t, revoked, 3, "tried until Helix answered")
	for _, c := range revoked {
		assert.Equal(t, "Bearer "+tokenA, c.auth)
	}
}

// A token that can't be opened is forgotten here all the same.
func TestUnlinkForgetsATokenItCantOpen(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.dir.LinkRelay(context.Background(), directory.RelayLink{HostedOrigin: f.door.URL, LinkID: linkA, TokenSealed: []byte("garbage"),
		KeyID: "k1", AccountEmail: "ada@acme.com"}))
	require.NoError(t, f.r.Unlink(context.Background()))
	assert.Nil(t, f.dir.link)
	assert.Empty(t, f.door.calls("/v1/relay/revoke"), "no token to revoke with")
	assert.Empty(t, f.dir.waiting(), "nor ever one")
}

// The component idles while unlinked, claims once linked (without a
// restart), takes in what each claim brings and acks it on the next, and
// forgets a link Helix refuses.
func TestTheRelayClaims(t *testing.T) {
	f := newFixture(t)
	f.r.o.Slack.Feed.Enabled = true
	f.db.feeds = []*model.SlackFeed{{ID: "feed-1", InstallID: "inst-1"}}
	day := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	claims := 0
	var claimed []took
	f.door.route("/v1/relay/claim", func(c took) (int, string) {
		claims++
		claimed = append(claimed, c)
		switch claims {
		case 1:
			return http.StatusOK, marshal(t, &foundryv1.ClaimFoundryRelayDeliveriesResponse{
				Installs: []*foundryv1.FoundryRelayInstall{
					{InstallId: "inst-1", LocalWorkspaceId: ws, TeamId: "T0TEAM", TeamName: "Acme", AppId: "A0FOUNDRY", BotUserId: "U0BOT",
						Scopes: []string{"chat:write"}, InstalledBySlackUserId: "U0ADA", CreatedAt: timestamppb.Now()},
					{InstallId: "inst-bad", LocalWorkspaceId: ws, TeamId: "not a team"},
				},
				Deliveries: []*foundryv1.FoundryRelayDelivery{
					{Id: "d1", InstallId: "inst-1", LocalWorkspaceId: ws, Kind: "event", DedupeKey: model.SlackEventDedupe("Ev1"),
						Conversation: model.SlackConversation("inst-1", "D0ADA", "1700000000.000001", "U0ADA"),
						SlackUserId:  "U0ADA", PayloadJson: `{"kind":"mention"}`},
					{Id: "d2", InstallId: "inst-1", Kind: "smoke", DedupeKey: "x"},
				},
				Feed: []*foundryv1.FoundryRelayFeedRow{
					{Id: "f1", InstallId: "inst-1", DedupeKey: validFeedKey(t), ChannelId: "C0GENERAL", Day: timestamppb.New(day), Kind: "message",
						PayloadJson: `{"ts":"1"}`},
					{Id: "f2", InstallId: "inst-1", DedupeKey: "bad", ChannelId: "C0GENERAL", Day: timestamppb.New(day), Kind: "message", PayloadJson: `{}`},
				},
				FeedGaps: []*foundryv1.FoundryRelayFeedGap{
					{Id: "g1", InstallId: "inst-1", ChannelId: "C0GENERAL", From: timestamppb.New(day), To: timestamppb.New(day.Add(time.Minute))},
					{Id: "g2", InstallId: "inst-1", From: timestamppb.New(day), To: timestamppb.New(day)},
				},
			})
		case 2:
			return http.StatusTooManyRequests, `{"error":{"code":"rate_limited"}}`
		case 3:
			return http.StatusServiceUnavailable, `{"error":{"code":"backend_unavailable"}}`
		case 4:
			return http.StatusOK, `{}`
		}
		return http.StatusUnauthorized, `{"error":{"code":"invalid_token"}}`
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.r.Start(ctx) }()
	time.Sleep(50 * time.Millisecond)
	assert.Empty(t, f.door.calls("/v1/relay/claim"), "unlinked, it claims nothing")
	f.linked(t)
	require.Eventually(t, func() bool {
		f.dir.mu.Lock()
		defer f.dir.mu.Unlock()
		return f.dir.link == nil
	}, 10*time.Second, 10*time.Millisecond, "a link Helix refuses is forgotten")
	require.NoError(t, f.r.Stop(context.Background()))
	require.NoError(t, <-done)

	require.Len(t, claimed, 5)
	assert.Equal(t, "Bearer "+tokenA, claimed[0].auth)
	assert.Nil(t, claimed[0].body["acks"], "nothing to ack yet")
	assert.Nil(t, claimed[0].body["feeds"], "no install known yet")
	for _, c := range claimed[1:4] {
		assert.ElementsMatch(t, []any{"d1", "d2", "f1", "f2", "g1", "g2"}, c.body["acks"], "what it took, acked until a claim is answered")
		assert.Equal(t, []any{map[string]any{"local_workspace_id": ws, "on": true}}, c.body["feeds"])
	}
	assert.Nil(t, claimed[4].body["acks"], "the answered claim acked them")
	assert.Equal(t, []string{linkA, linkA}, f.dir.ok, "each answered claim is the link's last contact")

	require.NotEmpty(t, f.db.mirrored)
	first := f.db.mirrored[0]
	require.Len(t, first, 1, "an install the store can't keep is left out")
	assert.Equal(t, "inst-1", first[0].ID)
	assert.Equal(t, ws, first[0].WorkspaceID)
	assert.Equal(t, "user-owner", first[0].InstalledByUserID, "the owner installed it, as their Slack account")
	assert.Empty(t, f.db.mirrored[len(f.db.mirrored)-1], "forgotten, it holds no install")

	require.Len(t, f.db.intake, 1)
	in := f.db.intake[0]
	assert.Equal(t, "d1", in.ID, "the delivery's own id")
	assert.Equal(t, model.SlackIntakeEvent, in.Kind)
	assert.Equal(t, "inst-1", in.InstallID)
	require.Len(t, f.db.rows["inst-1"], 1)
	assert.Equal(t, model.SlackFeedFromEvent, f.db.rows["inst-1"][0].Source)
	assert.Equal(t, []model.SlackFeedGap{{ChannelID: "C0GENERAL", From: day, To: day.Add(time.Minute)}}, f.db.gaps["inst-1"])
}

// validFeedKey is a feed row's dedupe key: its event's.
func validFeedKey(t *testing.T) string {
	t.Helper()
	require.True(t, model.ValidSlackFeedDedupe("e:Ev0FEED1"))
	return "e:Ev0FEED1"
}

// What fails to be taken in comes again: it isn't acked.
func TestWhatFailsComesAgain(t *testing.T) {
	f := newFixture(t)
	f.linked(t)
	l, err := f.r.open(context.Background())
	require.NoError(t, err)
	day := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	f.door.route("/v1/relay/claim", func(took) (int, string) {
		return http.StatusOK, marshal(t, &foundryv1.ClaimFoundryRelayDeliveriesResponse{
			Deliveries: []*foundryv1.FoundryRelayDelivery{{Id: "d1", InstallId: "inst-1", Kind: "event", DedupeKey: model.SlackEventDedupe("Ev1"),
				PayloadJson: `{}`}},
			Feed: []*foundryv1.FoundryRelayFeedRow{{Id: "f1", InstallId: "inst-1", DedupeKey: validFeedKey(t), ChannelId: "C0GENERAL",
				Day: timestamppb.New(day), Kind: "message", PayloadJson: `{}`}},
			FeedGaps: []*foundryv1.FoundryRelayFeedGap{{Id: "g1", InstallId: "inst-1", From: timestamppb.New(day), To: timestamppb.New(day.Add(time.Minute))}},
		})
	})
	f.db.failQ, f.db.failFeed, f.db.failGaps = errors.New("down"), errors.New("down"), errors.New("down")
	taken, err := f.r.claim(context.Background(), l, []string{"old"})
	require.NoError(t, err)
	assert.Empty(t, taken, "the old acks were taken; nothing new was")

	f.db.failM = errors.New("down")
	taken, err = f.r.claim(context.Background(), l, []string{"old"})
	assert.Error(t, err)
	assert.Empty(t, taken)
	f.db.failM = nil

	many := make([]string, maxAcks+3)
	for i := range many {
		many[i] = "a"
	}
	f.db.failQ, f.db.failFeed, f.db.failGaps = nil, nil, nil
	taken, err = f.r.claim(context.Background(), l, many)
	require.NoError(t, err)
	assert.Len(t, taken, 3+3, "the acks past one claim's wait for the next")
	assert.True(t, slices.Contains(taken, "d1"))

	f.dir.fail = errors.New("down")
	_, err = f.r.claim(context.Background(), l, nil)
	assert.ErrorContains(t, err, "owner")
}

// Stopping a relay that never started stops nothing; a link that changed
// since its claim is never forgotten.
func TestStopAndForgetEdges(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.r.Stop(context.Background()))
	f.linked(t)
	f.r.forget(context.Background(), linkB)
	assert.NotNil(t, f.dir.link, "another link's refusal")
	assert.Empty(t, f.db.mirrored)
	f.r.forget(context.Background(), linkA)
	assert.Nil(t, f.dir.link)
	require.Len(t, f.db.mirrored, 1)
	assert.Empty(t, f.dir.waiting(), "Helix revoked it already")
}
