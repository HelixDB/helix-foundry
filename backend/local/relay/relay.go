// Package relay is a standalone install's side of Helix's Slack relay (the
// spec's §10): the owner links the install to a Helix workspace (StartLink,
// CompleteLink, as OAuth's PKCE does), and from then on Slack reaches the
// install's own bot through Helix. Relay is all of it:
//
//   - the owner's link, which Settings › Slack drives (api.RelayLinker):
//     its hx_rl_ token sealed in the install's directory with the
//     connectors' keyring, never shown;
//   - a component the install always runs (Start): idle while unlinked,
//     reading the directory every idlePoll, and while linked claiming what
//     Slack sent the link's installs, a long poll at a time, through Helix's
//     door. Each claim mirrors the link's installs into the install's store
//     (with no token: their calls go through the relay), queues each of the
//     bot's deliveries as an intake row of the same id and dedupe key, which
//     the install's foundry-slack-worker handles as hosted's does, appends
//     the data connector's channel messages and gaps to its feeds (connector
//     §11), takes a reply in a thread the bot follows as a message, as
//     hosted takes its own (api.FollowSlackReply, only while the install's
//     foundry.slack.follow is on), and acks what it took on the next claim: a delivery not acked
//     comes again, and its dedupe key keeps it one row. It backs off from
//     a second to a minute, jittered, while Helix can't be reached, and
//     forgets a link Helix revoked. Linked or not, it revokes the links the
//     install forgot (unlinked, or replaced by a new link) until Helix
//     answers, at most every revokeRetry while Helix doesn't;
//   - the bot's way to Slack (Transport, a slackbot.Relayed), and the API's
//     (Slack).
//
// Helix vouches for who clicked in Slack: a linked install runs what the
// owner's Slack account approves through it, unless the owner turns
// approving changes from Slack off (SetSlackApprovals).
package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/helixdb/helix-foundry/backend/api"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/local/directory"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/keytoken"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/slackbot"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// idlePoll is how often an unlinked install reads whether it was linked.
	idlePoll = 10 * time.Second
	// minBackoff and maxBackoff bound the wait after a claim that failed,
	// doubling between them.
	minBackoff = time.Second
	maxBackoff = time.Minute
	// claimWait is how long one claim waits at Helix for something to
	// arrive, and claimSlack how much longer its answer may take.
	claimWait  = config.MaxSlackRelayClaimWait
	claimSlack = 15 * time.Second
	// callFor bounds every other call to the door.
	callFor = 15 * time.Second
	// maxAcks is the most ids one claim acks; the rest wait for the next.
	maxAcks = 2048
	// revokeRetry is how long the component waits to revoke forgotten links
	// again after Helix didn't answer.
	revokeRetry = time.Minute
	// maxAnswer is the most of the door's answer read: a claim's feed is at
	// most 4 MiB of payloads, with its JSON around them.
	maxAnswer = 16 << 20
	// tokenContext and verifierContext seal the link's token and a pending
	// link's PKCE verifier with the connectors' keyring.
	tokenContext    = "foundry-relay"
	verifierContext = "foundry-relay-verifier"
	// returnPath is the install's Settings › Slack, where Helix sends the
	// owner back to: the link's redirect URI, and every install's return.
	returnPath = "/settings/slack"
)

// Directory is the install's directory, which keeps its link
// (directory.Store's methods).
type Directory interface {
	Owner(ctx context.Context) (directory.User, error)
	RelayLink(ctx context.Context) (directory.RelayLink, error)
	LinkRelay(ctx context.Context, l directory.RelayLink) error
	UnlinkRelay(ctx context.Context) (directory.RelayLink, error)
	SetRelaySlackApprovals(ctx context.Context, on bool) error
	RelayOK(ctx context.Context, linkID string) error
	StartRelayLink(ctx context.Context, stateHash string, verifierSealed []byte, keyID string) (time.Time, error)
	SpendRelayLink(ctx context.Context, stateHash string) ([]byte, string, error)
	RelayRevokes(ctx context.Context) ([]directory.RelayRevoke, error)
	RelayRevoked(ctx context.Context, linkID string) error
}

var _ Directory = (*directory.Store)(nil)

// Store is what the relay writes of the install's Foundry store
// (store.Store's methods): its mirror of the link's installs, the bot's
// intake and the threads it follows, and the data connector's feeds.
type Store interface {
	api.SlackFollowStore
	GetFoundrySlackInstallByID(ctx context.Context, id string) (*model.SlackInstall, error)
	MirrorFoundrySlackInstalls(ctx context.Context, installs []*model.SlackInstall) error
	ListFoundrySlackFeeds(ctx context.Context, after string, limit int) ([]*model.SlackFeed, error)
	AppendFoundrySlackFeed(ctx context.Context, installID string, batch model.SlackFeedBatch, limits model.SlackFeedLimits) (model.SlackFeedAppended, error)
	AppendFoundrySlackFeedGaps(ctx context.Context, installID string, gaps []model.SlackFeedGap, spacing time.Duration) (model.SlackFeedAppended, error)
}

var _ Store = store.Store(nil)

// Options are what a Relay runs with.
type Options struct {
	Directory Directory
	DB        Store
	// Keys is the connectors' keyring, which seals the link's token.
	Keys *secretbox.Keyring
	// Door is Helix's door a new link is made with (local.relay.url), and
	// Site Helix's site whose /relay/link asks an owner or admin to link
	// the install (local.relay.site_url): origins.
	Door, Site string
	// Origin is where the owner's browser reaches this install's site
	// (local.site_origin), whose Settings › Slack Helix sends them back to.
	Origin string
	// Name is the install as Helix's link page names it, such as the
	// machine's name: at most 128 bytes.
	Name string
	// Slack is the install's foundry.slack: whether its data connector
	// reads channels (feed), and how it buffers what it reads.
	Slack config.SlackConfig
	// HTTP calls the door; one without a timeout of its own when nil (each
	// call is bounded by its context).
	HTTP   *http.Client
	Logger *slog.Logger
}

// Relay is a standalone install's link to Helix's relay, and the component
// that claims what Slack sends through it.
type Relay struct {
	o      Options
	http   *http.Client
	logger *slog.Logger
	// limiter paces the Web API calls of the API's Slack client (Slack):
	// the relay's own limits answer the rest.
	limiter *slack.Limiter

	// idle is how often an unlinked install looks for a link, backoff the
	// first wait after a claim that failed, and retry the wait to revoke
	// forgotten links again: idlePoll, minBackoff and revokeRetry, but in
	// tests.
	idle, backoff, retry time.Duration

	mu sync.Mutex
	// revokeAfter is when Start next revokes the forgotten links: zero for
	// at once.
	revokeAfter time.Time
	// cancel stops a running Start, and done closes once it returned.
	cancel context.CancelFunc
	done   chan struct{}
	// installs are the local workspaces of the link's installs as the last
	// claim listed them, whose feeds the next claim reports.
	installs map[string]string
}

// New is a Relay over o, which needs every option.
func New(o Options) (*Relay, error) {
	switch {
	case o.Directory == nil || o.DB == nil || o.Keys == nil || o.Logger == nil:
		return nil, errors.New("relay: a directory, a store, a keyring and a logger are required")
	case o.Name == "" || len(o.Name) > 128:
		return nil, errors.New("relay: the install's name is 1 to 128 bytes")
	}
	for _, origin := range []string{o.Door, o.Site, o.Origin} {
		if u, err := url.Parse(origin); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" ||
			u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("relay: %q is not an http(s) origin", origin)
		}
	}
	limiter, err := slack.NewLimiter(1)
	if err != nil {
		return nil, err
	}
	client := o.HTTP
	if client == nil {
		client = &http.Client{}
	}
	return &Relay{o: o, http: client, logger: o.Logger.With("component", "relay"), limiter: limiter, idle: idlePoll, backoff: minBackoff, retry: revokeRetry,
		installs: map[string]string{}}, nil
}

// Relay is the link as it stands.
func (r *Relay) Relay(ctx context.Context) (api.RelayState, error) {
	l, err := r.o.Directory.RelayLink(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return api.RelayState{}, nil
	case err != nil:
		return api.RelayState{}, err
	}
	return api.RelayState{Linked: true, HostedOrigin: l.HostedOrigin, AccountEmail: l.AccountEmail, LinkedAt: l.LinkedAt, LastOKAt: l.LastOKAt,
		SlackApprovals: l.SlackApprovals}, nil
}

// random is n random bytes, base64url without padding.
func random(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// hashHex is s's SHA-256 in lowercase hex.
func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// StartLink starts linking the install: a state and a PKCE verifier, kept
// (sealed) for directory.RelayPendingTTL, and Helix's page asking an owner
// or admin to link the install, by its name, to one of their workspaces,
// sending them back to Settings › Slack with a code for the state.
func (r *Relay) StartLink(ctx context.Context) (string, error) {
	state, err := random(32)
	if err != nil {
		return "", err
	}
	verifier, err := random(32)
	if err != nil {
		return "", err
	}
	sealed, keyID, err := r.o.Keys.Seal([]byte(verifier), verifierContext)
	if err != nil {
		return "", fmt.Errorf("seal the link's verifier: %w", err)
	}
	if _, err := r.o.Directory.StartRelayLink(ctx, hashHex(state), sealed, keyID); err != nil {
		return "", fmt.Errorf("keep the link's state: %w", err)
	}
	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"redirect_uri": {r.o.Origin + returnPath}, "name": {r.o.Name}}
	return r.o.Site + "/relay/link?" + query.Encode(), nil
}

// CompleteLink redeems the code Helix handed back for state with its
// verifier, at the door it was started for, and keeps the token as the
// install's link, approving changes from Slack on: store.ErrNotFound for a
// state unknown, spent or expired, or a code Helix doesn't know (expired,
// spent). A link it replaces is revoked at Helix (revokeForgotten); a token
// redeemed that it couldn't keep is revoked at once, best effort.
func (r *Relay) CompleteLink(ctx context.Context, code, state string) error {
	sealed, keyID, err := r.o.Directory.SpendRelayLink(ctx, hashHex(state))
	if err != nil {
		return err
	}
	verifier, err := r.o.Keys.Open(sealed, keyID, verifierContext)
	if err != nil {
		return fmt.Errorf("open the link's verifier: %w", err)
	}
	var redeemed foundryv1.RedeemFoundryRelayCodeResponse
	err = r.call(ctx, r.o.Door, "", "/v1/relay/token", &foundryv1.RedeemFoundryRelayCodeRequest{Code: code, CodeVerifier: string(verifier)}, &redeemed, callFor)
	var refused *doorError
	switch {
	case errors.As(err, &refused) && refused.status == http.StatusNotFound:
		return store.ErrNotFound
	case err != nil:
		return fmt.Errorf("redeem the code: %w", err)
	case keytoken.ValidateRelay(redeemed.GetToken()) != nil:
		return errors.New("Helix answered without a relay token")
	}
	tokenSealed, tokenKey, err := r.o.Keys.Seal([]byte(redeemed.GetToken()), tokenContext)
	if err == nil {
		err = r.o.Directory.LinkRelay(ctx, directory.RelayLink{HostedOrigin: r.o.Door, LinkID: redeemed.GetLinkId(), TokenSealed: tokenSealed,
			KeyID: tokenKey, AccountEmail: redeemed.GetAccountEmail(), SlackApprovals: true})
	}
	if err != nil {
		if err := r.call(context.WithoutCancel(ctx), r.o.Door, redeemed.GetToken(), "/v1/relay/revoke", nil, nil, callFor); err != nil {
			r.logger.Warn("revoke a relay token this install couldn't keep failed; it can be revoked in Helix's Settings › Slack", "err", err)
		}
		return fmt.Errorf("keep the link: %w", err)
	}
	r.revokeForgotten(ctx)
	return nil
}

// SetSlackApprovals turns approving changes from Slack on or off:
// store.ErrNotFound while unlinked.
func (r *Relay) SetSlackApprovals(ctx context.Context, on bool) error {
	return r.o.Directory.SetRelaySlackApprovals(ctx, on)
}

// Unlink forgets the link, which removes the install's Slack installs, then
// revokes it at Helix: Helix removes them from Slack. While Helix doesn't
// answer, the component revokes it later (revokeForgotten). Unlinked, it
// does nothing.
func (r *Relay) Unlink(ctx context.Context) error {
	switch _, err := r.o.Directory.UnlinkRelay(ctx); {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	if err := r.o.DB.MirrorFoundrySlackInstalls(ctx, nil); err != nil {
		return fmt.Errorf("remove the link's Slack installs: %w", err)
	}
	r.revokeForgotten(ctx)
	return nil
}

// revokeForgotten revokes at Helix each link the install forgot, forgetting
// it for good once Helix revoked it (or refuses its token, revoked
// already), or when its token can't be opened; one Helix doesn't answer
// waits, and Start tries it again after revokeRetry. It returns the Helix
// origins of the links still waiting.
func (r *Relay) revokeForgotten(ctx context.Context) []string {
	forgotten, err := r.o.Directory.RelayRevokes(ctx)
	if err != nil {
		r.logger.Warn("read the relay links to revoke failed", "err", err)
		return nil
	}
	var waiting []string
	for _, f := range forgotten {
		token, err := r.o.Keys.Open(f.TokenSealed, f.KeyID, tokenContext)
		if err != nil {
			r.logger.Warn("open a forgotten relay token failed; it can be revoked in Helix's Settings › Slack", "link_id", f.LinkID, "err", err)
		} else {
			err = r.call(ctx, f.HostedOrigin, string(token), "/v1/relay/revoke", nil, nil, callFor)
			var refused *doorError
			if err != nil && !(errors.As(err, &refused) && refused.status == http.StatusUnauthorized) {
				r.logger.Warn("revoke a forgotten relay link at Helix failed; it is tried again", "link_id", f.LinkID, "err", err)
				waiting = append(waiting, f.HostedOrigin)
				continue
			}
		}
		if err := r.o.Directory.RelayRevoked(ctx, f.LinkID); err != nil {
			r.logger.Warn("forget a revoked relay link failed", "link_id", f.LinkID, "err", err)
		}
	}
	r.mu.Lock()
	r.revokeAfter = time.Time{}
	if len(waiting) > 0 {
		r.revokeAfter = time.Now().Add(r.retry)
	}
	r.mu.Unlock()
	return waiting
}

// StartSlackInstall starts adding Helix's Slack app for the local workspace
// workspaceID: Slack's page to send the owner to, which Helix finishes and
// sends back to Settings › Slack; store.ErrNotFound while unlinked. While a
// link the install forgot still waits to be revoked at the same Helix, it
// starts nothing: Helix would take the install for the old link's.
func (r *Relay) StartSlackInstall(ctx context.Context, workspaceID string) (string, error) {
	l, err := r.open(ctx)
	if err != nil {
		return "", err
	}
	if slices.Contains(r.revokeForgotten(ctx), l.link.HostedOrigin) {
		return "", errors.New("Helix hasn't revoked this Foundry's earlier link yet; try again once Helix can be reached")
	}
	var started foundryv1.StartFoundryRelaySlackInstallResponse
	if err := r.call(ctx, l.link.HostedOrigin, l.token, "/v1/relay/slack-install",
		&foundryv1.StartFoundryRelaySlackInstallRequest{LocalWorkspaceId: workspaceID, ReturnTo: r.o.Origin + returnPath}, &started, callFor); err != nil {
		return "", err
	}
	return started.GetAuthorizeUrl(), nil
}

// Transport is the bot's way to Slack through the link: as it stands at
// each call.
func (r *Relay) Transport() *slackbot.Relayed {
	return &slackbot.Relayed{DB: r.o.DB, HTTP: r.http, Access: func(ctx context.Context) (slackbot.RelayAccess, error) {
		l, err := r.open(ctx)
		if err != nil {
			return slackbot.RelayAccess{}, err
		}
		return slackbot.RelayAccess{Door: l.link.HostedOrigin, Token: l.token, SlackApprovals: l.link.SlackApprovals}, nil
	}}
}

// Slack is a Web API client that calls Slack through the link, and
// install's bot it calls as (api.RelayLinker).
func (r *Relay) Slack(ctx context.Context, install *model.SlackInstall) (*slack.Client, slack.Bot, error) {
	t := r.Transport()
	client, err := slack.NewClient(slack.ClientOptions{HTTP: t.Client(), Limiter: r.limiter})
	if err != nil {
		return nil, slack.Bot{}, err
	}
	bot, err := t.Bot(ctx, install.ID)
	return client, bot, err
}

// linked is the link with its token opened.
type linked struct {
	link  directory.RelayLink
	token string
}

// open is the link with its token opened: store.ErrNotFound while unlinked.
func (r *Relay) open(ctx context.Context) (linked, error) {
	l, err := r.o.Directory.RelayLink(ctx)
	if err != nil {
		return linked{}, err
	}
	token, err := r.o.Keys.Open(l.TokenSealed, l.KeyID, tokenContext)
	if err != nil {
		return linked{link: l}, fmt.Errorf("open the relay token: %w", err)
	}
	return linked{link: l, token: string(token)}, nil
}

// doorError is the door's refusal of a call: its HTTP status, and its
// code, reason and message when it said them.
type doorError struct {
	status                int
	code, reason, message string
	retryAfter            time.Duration
}

func (e *doorError) Error() string {
	if e.code == "" {
		return fmt.Sprintf("Helix answered HTTP %d", e.status)
	}
	return fmt.Sprintf("Helix answered HTTP %d %s: %s", e.status, e.code, e.message)
}

// call posts req (nil for no body) to the door's path, bearing token when
// set, within d, and reads its answer into resp (when not nil), field names
// as the protos name them.
func (r *Relay) call(ctx context.Context, door, token, path string, req, resp proto.Message, d time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var body []byte
	if req != nil {
		var err error
		if body, err = (protojson.MarshalOptions{UseProtoNames: true}).Marshal(req); err != nil {
			return err
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, door+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if req != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	httpResp, err := r.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(httpResp.Body, maxAnswer))
	if err != nil {
		return err
	}
	if httpResp.StatusCode != http.StatusOK {
		e := &doorError{status: httpResp.StatusCode}
		var said struct {
			Error struct{ Code, Reason, Message string } `json:"error"`
		}
		if json.Unmarshal(answer, &said) == nil {
			e.code, e.reason, e.message = said.Error.Code, said.Error.Reason, said.Error.Message
		}
		if seconds, err := time.ParseDuration(httpResp.Header.Get("Retry-After") + "s"); err == nil && seconds > 0 {
			e.retryAfter = seconds
		}
		return e
	}
	if resp == nil {
		return nil
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(answer, resp)
}

// Name is the component's.
func (r *Relay) Name() string { return "foundry-relay" }

// Start claims through the link until ctx ends or Stop: idle while
// unlinked, reading the directory every idlePoll; linked, one claim after
// another, acking what each took on the next, backing off while Helix
// can't be reached, and forgetting a link Helix revoked.
func (r *Relay) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.cancel, r.done = cancel, make(chan struct{})
	done := r.done
	r.mu.Unlock()
	defer close(done)
	defer cancel()
	var acks []string
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		r.mu.Lock()
		due := !time.Now().Before(r.revokeAfter)
		r.mu.Unlock()
		if due {
			r.revokeForgotten(ctx)
		}
		l, err := r.open(ctx)
		if errors.Is(err, store.ErrNotFound) {
			acks = nil
			wait(ctx, r.idle)
			continue
		}
		var taken []string
		if err == nil {
			taken, err = r.claim(ctx, l, acks)
		}
		var refused *doorError
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.As(err, &refused) && refused.status == http.StatusUnauthorized:
			r.forget(ctx, l.link.LinkID)
			acks, backoff = nil, 0
			continue
		case errors.As(err, &refused) && refused.status == http.StatusTooManyRequests && refused.retryAfter > 0:
			// Another claim of this link waits (a second install of it, or one
			// cut off): wait as the door says.
			wait(ctx, refused.retryAfter)
			continue
		case err != nil:
			backoff = min(max(backoff*2, r.backoff), maxBackoff)
			r.logger.Warn("a relay claim failed; it is tried again", "err", err, "after", backoff.String())
			// Jittered between half the back-off and all of it.
			wait(ctx, backoff/2+time.Duration(mrand.Int64N(int64(backoff/2)+1)))
			continue
		}
		acks, backoff = taken, 0
	}
	return nil
}

// Stop stops Start, waiting for it within ctx.
func (r *Relay) Stop(ctx context.Context) error {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// wait waits d, or until ctx ends.
func wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// forget forgets a link Helix refused, if it is still linkID, and removes
// its installs here.
func (r *Relay) forget(ctx context.Context, linkID string) {
	if l, err := r.o.Directory.RelayLink(ctx); err != nil || l.LinkID != linkID {
		return
	}
	r.logger.Warn("Helix refused the relay token: the install is unlinked; its owner links it again in Settings › Slack")
	if _, err := r.o.Directory.UnlinkRelay(ctx); err != nil && !errors.Is(err, store.ErrNotFound) {
		r.logger.Warn("forget the revoked relay link failed", "err", err)
		return
	}
	// Helix revoked it already.
	if err := r.o.Directory.RelayRevoked(ctx, linkID); err != nil {
		r.logger.Warn("forget the revoked relay link failed", "err", err)
	}
	if err := r.o.DB.MirrorFoundrySlackInstalls(ctx, nil); err != nil {
		r.logger.Warn("remove the revoked link's Slack installs failed", "err", err)
	}
}

// claim acks acks and claims what waits for the link, and takes it in:
// the link's installs mirrored, the deliveries queued, the feed appended. It
// returns the ids taken, for the next claim to ack; an error leaves them to
// come again.
func (r *Relay) claim(ctx context.Context, l linked, acks []string) ([]string, error) {
	feeds, err := r.feeds(ctx)
	if err != nil {
		return nil, err
	}
	req := &foundryv1.ClaimFoundryRelayDeliveriesRequest{Acks: acks[:min(len(acks), maxAcks)], WaitMs: int32(claimWait / time.Millisecond), Feeds: feeds}
	var got foundryv1.ClaimFoundryRelayDeliveriesResponse
	if err := r.call(ctx, l.link.HostedOrigin, l.token, "/v1/relay/claim", req, &got, claimWait+claimSlack); err != nil {
		return acks, err
	}
	if err := r.o.Directory.RelayOK(ctx, l.link.LinkID); err != nil {
		r.logger.Warn("record the relay's last contact failed", "err", err)
	}
	taken := slices.Clone(acks[min(len(acks), maxAcks):])
	if err := r.mirror(ctx, got.GetInstalls()); err != nil {
		return taken, err
	}
	for _, d := range got.GetDeliveries() {
		if r.deliver(ctx, d) {
			taken = append(taken, d.GetId())
		}
	}
	return append(taken, r.appendFeed(ctx, got.GetFeed(), got.GetFeedGaps())...), nil
}

// feeds is, for each local workspace of the link's installs, whether its
// Slack data connector reads its install (only with the install's own
// foundry.slack.feed on, and a live feed reading it), and whether it takes
// unmentioned follow-ups (its own foundry.slack.follow), which Helix then
// relays the thread replies of, data connector or not.
func (r *Relay) feeds(ctx context.Context) ([]*foundryv1.FoundryRelayFeedOn, error) {
	r.mu.Lock()
	installs := make(map[string]string, len(r.installs))
	for id, ws := range r.installs {
		installs[id] = ws
	}
	r.mu.Unlock()
	if len(installs) == 0 {
		return nil, nil
	}
	read := map[string]bool{}
	if r.o.Slack.Feed.Enabled {
		for after := ""; ; {
			page, err := r.o.DB.ListFoundrySlackFeeds(ctx, after, 100)
			if err != nil {
				return nil, fmt.Errorf("read the feeds: %w", err)
			}
			for _, f := range page {
				read[f.InstallID] = true
			}
			if len(page) < 100 {
				break
			}
			after = page[len(page)-1].ID
		}
	}
	out := make([]*foundryv1.FoundryRelayFeedOn, 0, len(installs))
	for id, ws := range installs {
		out = append(out, &foundryv1.FoundryRelayFeedOn{LocalWorkspaceId: ws, On: read[id], Follow: r.o.Slack.Follow.Enabled})
	}
	slices.SortFunc(out, func(a, b *foundryv1.FoundryRelayFeedOn) int {
		return strings.Compare(a.GetLocalWorkspaceId(), b.GetLocalWorkspaceId())
	})
	return out, nil
}

// mirror makes the install's Slack installs the link's, as the claim listed
// them, each installed by the install's owner as its Slack account. One
// Helix lists that the store can't keep is left out, and logged.
func (r *Relay) mirror(ctx context.Context, listed []*foundryv1.FoundryRelayInstall) error {
	owner, err := r.o.Directory.Owner(ctx)
	if err != nil {
		return fmt.Errorf("read the install's owner: %w", err)
	}
	installs := make([]*model.SlackInstall, 0, len(listed))
	workspaces := make(map[string]string, len(listed))
	for _, in := range listed {
		m := &model.SlackInstall{
			ID: in.GetInstallId(), WorkspaceID: in.GetLocalWorkspaceId(), TeamID: in.GetTeamId(), EnterpriseID: in.GetEnterpriseId(),
			TeamName: in.GetTeamName(), AppID: in.GetAppId(), BotUserID: in.GetBotUserId(), BotID: in.GetBotId(), InstallerAdmin: in.GetInstallerAdmin(),
			Scopes: in.GetScopes(), InstalledByUserID: owner.ID, InstalledBySlackUserID: in.GetInstalledBySlackUserId(), Mode: model.SlackPerPerson{},
		}
		if err := m.Validate(); err != nil {
			r.logger.Warn("Helix listed a Slack install this Foundry can't keep; it is left out", "install_id", in.GetInstallId(), "err", err)
			continue
		}
		installs = append(installs, m)
		workspaces[m.ID] = m.WorkspaceID
	}
	if err := r.o.DB.MirrorFoundrySlackInstalls(ctx, installs); err != nil {
		return fmt.Errorf("mirror the link's Slack installs: %w", err)
	}
	r.mu.Lock()
	r.installs = workspaces
	r.mu.Unlock()
	return nil
}

// deliver queues a delivery as the bot's intake row of the same id and
// dedupe key, and reports whether it was taken: queued now or before, or
// one the install can never take (logged), which is acked all the same.
func (r *Relay) deliver(ctx context.Context, d *foundryv1.FoundryRelayDelivery) bool {
	in := &model.SlackIntake{ID: d.GetId(), InstallID: d.GetInstallId(), DedupeKey: d.GetDedupeKey(), Conversation: d.GetConversation(),
		SlackUserID: d.GetSlackUserId(), Payload: json.RawMessage(d.GetPayloadJson())}
	kind, err := model.ParseSlackIntakeKind(d.GetKind())
	if in.Kind = kind; err == nil {
		err = in.Validate()
	}
	if err != nil {
		r.logger.Warn("Helix relayed a Slack request this Foundry can't take; it is dropped", "delivery", d.GetId(), "err", err)
		return true
	}
	switch err := r.o.DB.QueueFoundrySlackIntake(ctx, in); {
	case err == nil || errors.Is(err, store.ErrConflict):
		return true
	default:
		r.logger.Warn("queue a relayed Slack request failed; it comes again", "delivery", d.GetId(), "err", err)
		return false
	}
}

// appendFeed appends the data connector's channel messages and gaps to the
// feeds reading their installs, an install's at a time, once it took the
// replies among them its bot follows (follow), and returns the ids taken:
// appended, or that no feed reads (unbound), or that the install can never
// take (logged). A store failure leaves an install's to come again.
func (r *Relay) appendFeed(ctx context.Context, rows []*foundryv1.FoundryRelayFeedRow, gaps []*foundryv1.FoundryRelayFeedGap) []string {
	type batch struct {
		ids  []string
		rows []model.SlackFeedRow
	}
	byInstall := map[string]*batch{}
	var taken []string
	for _, row := range rows {
		kind, err := model.ParseSlackFeedRowKind(row.GetKind())
		m := model.SlackFeedRow{DedupeKey: row.GetDedupeKey(), ChannelID: row.GetChannelId(), Day: model.SlackDay(row.GetDay().AsTime()), Kind: kind,
			Source: model.SlackFeedFromEvent, Payload: json.RawMessage(row.GetPayloadJson())}
		if err == nil {
			err = m.Validate()
		}
		if err != nil {
			r.logger.Warn("Helix relayed a Slack channel message this Foundry can't keep; it is dropped", "item", row.GetId(), "err", err)
			taken = append(taken, row.GetId())
			continue
		}
		b := byInstall[row.GetInstallId()]
		if b == nil {
			b = &batch{}
			byInstall[row.GetInstallId()] = b
		}
		b.ids, b.rows = append(b.ids, row.GetId()), append(b.rows, m)
	}
	limits := model.SlackFeedLimits{Spacing: r.o.Slack.Feed.Spacing, MaxRows: int64(r.o.Slack.Feed.MaxBufferedRows)}
	for _, installID := range slices.Sorted(maps.Keys(byInstall)) {
		b := byInstall[installID]
		if err := r.follow(ctx, installID, b.rows); err != nil {
			r.logger.Warn("take relayed Slack replies failed; they come again", "install_id", installID, "err", err)
			continue
		}
		if _, err := r.o.DB.AppendFoundrySlackFeed(ctx, installID, model.SlackFeedBatch{Rows: b.rows, At: time.Now()}, limits); err != nil {
			r.logger.Warn("append relayed Slack channel messages failed; they come again", "install_id", installID, "err", err)
			continue
		}
		taken = append(taken, b.ids...)
	}
	for _, g := range gaps {
		gap := model.SlackFeedGap{ChannelID: g.GetChannelId(), From: g.GetFrom().AsTime(), To: g.GetTo().AsTime()}
		if err := gap.Validate(); err != nil {
			r.logger.Warn("Helix relayed a Slack feed gap this Foundry can't keep; it is dropped", "item", g.GetId(), "err", err)
			taken = append(taken, g.GetId())
			continue
		}
		if _, err := r.o.DB.AppendFoundrySlackFeedGaps(ctx, g.GetInstallId(), []model.SlackFeedGap{gap}, r.o.Slack.Feed.Spacing); err != nil {
			r.logger.Warn("record a relayed Slack feed gap failed; it comes again", "install_id", g.GetInstallId(), "err", err)
			continue
		}
		taken = append(taken, g.GetId())
	}
	return taken
}

// follow applies the bot's rule for replies that don't mention it
// (api.FollowSlackReply) to an install's relayed channel messages, while
// the install's foundry.slack.follow is on: Helix follows no relayed
// thread. A relayed message is at its own ts, and carries no sender's team
// (the bot reads the sender's own); an install gone, or one this install
// no longer holds, follows nothing.
func (r *Relay) follow(ctx context.Context, installID string, rows []model.SlackFeedRow) error {
	if !r.o.Slack.Follow.Enabled {
		return nil
	}
	install, err := r.o.DB.GetFoundrySlackInstallByID(ctx, installID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("read the install: %w", err)
	case install.RevokedAt != nil:
		return nil
	}
	for _, row := range rows {
		var msg slack.StoredMessage
		if row.Kind != model.SlackRowMessage || json.Unmarshal(row.Payload, &msg) != nil || !model.ValidSlackTS(msg.TS) {
			continue
		}
		secs, frac, _ := strings.Cut(msg.TS, ".")
		s, _ := strconv.ParseInt(secs, 10, 64)
		us, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)
		m := slack.ChannelMessage{At: slack.Place{TeamID: install.TeamID, EnterpriseID: install.EnterpriseID, ChannelID: row.ChannelID}, Message: msg,
			EventTime: time.Unix(s, us*1000).UTC()}
		if _, err := api.FollowSlackReply(ctx, r.o.DB, r.o.Slack.Follow.Window, row.DedupeKey, install, m); err != nil {
			return err
		}
	}
	return nil
}
