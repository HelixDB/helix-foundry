// Package slackbot is Helix Foundry's Slack bot (foundry.slack) as the
// foundry-slack-worker runs it: it turns what Slack sent, which the API
// recorded in intake (ReceiveSlackRequest), into Founder's turns, as the
// Slack user's linked Foundry user (per_person) or as the install's shared
// bot (bot mode: a service credential of the workspace, one Founder thread
// per Slack thread, anyone in the Slack workspace talking to it within
// foundry.slack.bot.per_user_per_hour), and shows those turns in Slack as
// they go. A bot's proposals are reviewed and run in two clicks by an owner
// or admin who linked their Slack account, as themselves (agent.Proposals):
// Review draws the card as them in a message only they see, Run runs it.
//
//   - HandleNext claims the oldest due intake row, one conversation's
//     messages at a time and in order, under a fenced lease: a message is
//     checked (who sent it, where), sent to Founder through its own API
//     (integration.FounderAPI) with a client token Slack's message makes, so
//     a retry is the same turn, and answered with a reply message the
//     follower then owns; a click runs, dismisses or prepares again a card,
//     stops a turn, or unlinks its clicker.
//   - FollowNext claims a reply and keeps it until its turn ends, renewing
//     its lease on every read: it folds the turn's events into the reply
//     message (chat.update, spaced per install), and posts once what they
//     have it post apart (an Analyst answer, a card, questions).
//   - Backfill reads the Slack data connector's history (foundry.slack.feed)
//     a page at a time, on Slack's one call a minute per method and
//     workspace, into the feed's buffer, and folds it into the day mirror.
//   - A relayed install (model.SlackServesRelay) serves a standalone
//     Foundry linked to its workspace, whose own worker runs its bot: the
//     API queues its requests as deliveries its install claims, and here
//     only a message from a channel shared with another organisation is
//     refused; the sweep tells conversations when their install is offline
//     and when what they sent expired (relay.go).
//   - Sweep tells Slack of uninstalls it missed, revokes the keys of
//     revoked installs' bots, purges Slack-born Founder
//     threads past their retention, prunes what Slack's terms let go and
//     what the feeds' mirror holds, deletes the data connectors of installs
//     removed past their retention, flips staged switches to the app once
//     their history is read, and closes yesterday's days once a day.
//
// On a standalone install (package local/relay), Helix holds the installs
// and relays what Slack sent them into this install's intake; the bot runs
// as hosted's does, but reaches Slack through Helix's relay (Relayed,
// transport.go), answers as the install's one owner (the app's installer
// in full; anyone they allow, read-only), and acts on cards only while the
// owner approves changes from Slack.
//
// Every Web API call goes through the slack.Client's Limiter. A rate limit,
// Slack's or the Limiter's, never makes a worker sleep: the row or the reply
// is deferred by its Retry-After. Posting is at-least-once: a worker killed
// between a post and its record posts that message once more.
package slackbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// userTTL is how long what users.info said of a Slack user is kept, in
	// memory only.
	userTTL = 10 * time.Minute
	// agentOffFor is how long an install whose Slack plan has no agent
	// sessions (feature_disabled, missing_scope) is left without
	// agents.sessions.setStatus.
	agentOffFor = 24 * time.Hour
	// maxClaims gives up a row claimed this often whatever happened to it
	// (deferred by rate limits, or its workers killed): no row is claimed
	// forever.
	maxClaims = 200
	// busyPoll is the longest a message waits behind a busy thread's turn
	// before it looks again: a turn the bot doesn't follow (one started on
	// the site) wakes no one.
	busyPoll = 30 * time.Second
	// maxMentionNames is how many users a message mentions the bot looks up
	// by name; the others keep the label Slack gave, or their id.
	maxMentionNames = 5
	// maxInputChars is the most a message Founder takes holds.
	maxInputChars = 4000
	// maxSpeakerChars is the most of a Slack user's name a bot's turn
	// says wrote it.
	maxSpeakerChars = 80
	// linkTokenTTL is how long a link prompt's token lives.
	linkTokenTTL = 10 * time.Minute
	// linkTokenPrefix starts a link prompt's token (the API's).
	linkTokenPrefix = "hx_sl_"
	// questionWait is how long after its turn ended the follower waits for
	// an Analyst question to end before it posts that it is still answering.
	questionWait = 10 * time.Minute
	// updateGap is the least time between two updates of one reply message,
	// multiplied by the install's live follows; relayedUpdateGap a relayed
	// install's, whose link calls Slack at most
	// foundry.slack.relay.calls_per_minute times (60 by default).
	updateGap        = 1500 * time.Millisecond
	relayedUpdateGap = 6 * time.Second
	// eventsPage is how many of a turn's events one read returns.
	eventsPage = 200
)

// Store is what the bot reads and writes: Slack's tables and Founder's
// turns, actions and Analyst questions (*store.Store's methods).
type Store interface {
	GetFoundrySlackInstallByID(ctx context.Context, id string) (*model.SlackInstall, error)
	GetFoundrySlackInstallToken(ctx context.Context, installID string) (model.SlackSealedToken, error)
	ListFoundrySlackUninstalls(ctx context.Context, limit int) ([]*model.SlackInstall, error)
	MarkFoundrySlackUninstalled(ctx context.Context, installID string) error
	RevokeFoundrySlackInstall(ctx context.Context, workspaceID, installID string, uninstalled bool) error
	GetFoundrySlackLink(ctx context.Context, installID, slackUserID string) (*model.SlackLink, error)
	DeleteFoundrySlackLink(ctx context.Context, installID, slackUserID string) error
	CreateFoundrySlackLinkToken(ctx context.Context, installID, slackUserID, tokenHash string, ttl time.Duration) error
	GetFoundrySlackThread(ctx context.Context, installID, channelID, threadTS, slackUserID string) (*model.SlackThread, error)
	CreateFoundrySlackThread(ctx context.Context, m *model.SlackThread) error
	TouchFoundrySlackThread(ctx context.Context, id string) error
	PurgeFoundrySlackThreads(ctx context.Context, idle time.Duration, limit int) (int, error)
	ClaimFoundrySlackIntake(ctx context.Context, lease time.Duration) (*model.SlackIntake, error)
	SetFoundrySlackIntakeInput(ctx context.Context, id, owner, input string) error
	EndFoundrySlackIntake(ctx context.Context, id, owner string, outcome model.SlackIntakeOutcome) error
	WakeFoundrySlackIntake(ctx context.Context, turnID string) (int64, error)
	GetFoundrySlackReply(ctx context.Context, turnID string) (*model.SlackReply, error)
	ListFoundrySlackFollows(ctx context.Context, installID, channelID, threadTS string) ([]*model.SlackReply, error)
	CountFoundrySlackFollows(ctx context.Context, installID string) (int, error)
	HoldFoundrySlackReply(ctx context.Context, r *model.SlackReply) (*model.SlackReply, error)
	ReleaseFoundrySlackHold(ctx context.Context, turnID string) error
	StartFoundrySlackReply(ctx context.Context, turnID, messageTS, intakeID, intakeOwner string) error
	ClaimFoundrySlackReply(ctx context.Context, lease time.Duration) (*model.SlackReply, error)
	UpdateFoundrySlackReply(ctx context.Context, turnID, owner string, u model.SlackReplyUpdate) error
	ReleaseFoundrySlackReply(ctx context.Context, turnID, owner string, after time.Duration) error
	FinishFoundrySlackReply(ctx context.Context, turnID, owner string) error
	PruneFoundrySlack(ctx context.Context, intake, replies time.Duration, limit int) (int64, error)
	GetFoundryAgentTurn(ctx context.Context, workspaceID, id string) (*model.FoundryAgentTurn, error)
	GetFoundryAgentTurnByClientToken(ctx context.Context, workspaceID string, principal model.FoundryAgentPrincipal, clientToken string) (*model.FoundryAgentTurn, error)
	ListFoundryAgentTurns(ctx context.Context, threadID string, limit int) ([]*model.FoundryAgentTurn, error)
	ListFoundryAgentEvents(ctx context.Context, turnID string, afterSeq, limit int) ([]model.FoundryAgentEvent, error)
	GetFoundryAgentAction(ctx context.Context, workspaceID, id string) (*model.FoundryAgentAction, error)
	GetFoundryQuestion(ctx context.Context, ontologyID, id string) (*model.FoundryQuestion, error)
	GetFoundryOntology(ctx context.Context, workspaceID, id string) (*model.FoundryOntology, error)
	MarkFoundrySlackThreadBot(ctx context.Context, installID, channelID, threadTS string, at time.Time) error
	ListFoundrySlackLinks(ctx context.Context, installID string) ([]*model.SlackLink, error)

	// Bot mode: who spoke in a bot's thread, a Slack user's messages in
	// the hour, an owner's or admin's no to a proposal, and the keys of
	// revoked installs' bots, revoked with their source grants.
	AddFoundrySlackThreadPerson(ctx context.Context, threadID, slackUserID string) error
	ListFoundrySlackRetiredBots(ctx context.Context, limit int) ([]*model.SlackInstall, error)
	ForgetFoundrySlackBot(ctx context.Context, installID, credentialID string) error
	DeleteFoundryAccessPrincipal(ctx context.Context, workspaceID string, principal access.Principal) error
	CountFoundrySlackUserMessages(ctx context.Context, installID, slackUserID, intakeID string, window time.Duration) (int, error)
	DismissFoundryAgentProposal(ctx context.Context, workspaceID, id, deciderID string) (*model.FoundryAgentAction, error)

	// A standalone install's Slack users who asked its owner to use it.
	RequestFoundrySlackAccess(ctx context.Context, r *model.SlackAccessRequest) (bool, error)

	// The Slack data connector's backfill and sweeps (backfill.go), and
	// the fold's own (connector.SlackFeedStore).
	connector.SlackFeedStore
	GetConnector(ctx context.Context, workspaceID, id string) (*model.Connector, error)
	GetActiveConnectorSync(ctx context.Context, workspaceID, connectorID string) (*model.ConnectorSync, error)
	DeleteConnector(ctx context.Context, workspaceID, id string) error
	UpdateSlackAppConnector(ctx context.Context, workspaceID, id string, update model.ConnectorUpdate, sw model.SlackAppSwitch) error
	FlipSlackAppConnector(ctx context.Context, workspaceID, connectorID string, staged, flipped json.RawMessage, sealed []byte, keyID string) error
	ListFoundrySlackFeeds(ctx context.Context, after string, limit int) ([]*model.SlackFeed, error)
	ListFoundrySlackOrphans(ctx context.Context, retention time.Duration, limit int) ([]*model.SlackFeed, error)
	PruneFoundrySlackFeed(ctx context.Context, limit int) (int64, error)
	CloseFoundrySlackDays(ctx context.Context, feedID string, before time.Time, spacing time.Duration) (int, error)
	DueFoundrySlackBackfill(ctx context.Context, method model.SlackSlotMethod, limit int) ([]model.SlackBackfillWork, error)
	ReserveFoundrySlackSlot(ctx context.Context, installID string, method model.SlackSlotMethod, every time.Duration) (bool, error)
	DeferFoundrySlackSlot(ctx context.Context, installID string, method model.SlackSlotMethod, after time.Duration) error
	AdvanceFoundrySlackBackfill(ctx context.Context, step model.SlackBackfillStep, spacing time.Duration) error
	QueueFoundrySlackThreads(ctx context.Context, threads []model.SlackBackfillThread) (int, error)
	FailFoundrySlackBackfill(ctx context.Context, w model.SlackBackfillWindow, message string, maxAttempts int) (model.SlackBackfillStatus, error)
	EndFoundrySlackBackfill(ctx context.Context, w model.SlackBackfillWindow, status model.SlackBackfillStatus, message string) error
	SpendFoundrySlackGap(ctx context.Context, feedID string, max int) (bool, error)
	FoundrySlackBackfillProgress(ctx context.Context, feedID string) (model.SlackBackfillProgress, error)

	// The relay (relay.go): what waits for linked standalone installs that
	// are offline, or waited too long, and the channel messages they never
	// took.
	NoticeFoundryRelayOffline(ctx context.Context, offline, every time.Duration, limit int) ([]*model.RelayDelivery, error)
	ExpireFoundryRelayDeliveries(ctx context.Context, grace time.Duration, limit int) ([]*model.RelayDelivery, error)
	ExpireFoundryRelayFeed(ctx context.Context, ttl time.Duration, limit int) (int, error)
}

var _ Store = store.Store(nil)

// Approver reviews and runs a bot's proposals as an owner or admin
// (agent.Proposals): Review draws its card and checks it as them, Run runs
// it as them, once.
type Approver interface {
	Review(ctx context.Context, workspaceID, actionID, approverID string) (agent.ProposalReview, error)
	Run(ctx context.Context, workspaceID, actionID, payloadHash, specHash, approverID string) (agent.ProposalRun, error)
}

var _ Approver = agent.Proposals{}

// Options are what a Bot runs with.
type Options struct {
	DB Store
	// Sessions opens a linked person's Session, whose Founder the bot calls
	// as them.
	Sessions integration.SessionHost
	// Slack calls the Web API, through its Limiter: a standalone install's
	// through Relayed (its Client).
	Slack *slack.Client
	// Keys is the connectors' keyring: it opens hosted installs' bot tokens
	// (Direct), and seals a flipped connector's empty secret.
	Keys *secretbox.Keyring
	// Relayed reaches Slack through Helix's relay: a standalone install's,
	// whose Slack users its owner chooses (the spec's §10.5); nil hosted,
	// where each install's own token reaches it (Direct).
	Relayed *Relayed
	// Cfg is foundry.slack: its site, lease, poll interval, attempts,
	// back-off, busy wait and retention; the follow-ups', and the feed's
	// and its backfill's.
	Cfg config.SlackConfig
	// Objects holds the Slack data connector's day mirror; nil leaves the
	// backfill, the daily close and the staged switches' flips off, as
	// does foundry.slack.feed off.
	Objects snapshot.Store
	// WebAPI is Slack's Web API the backfill reads history from, hosted:
	// https://slack.com/api when empty.
	WebAPI string
	// People says who owns or administers a workspace, and their names:
	// bot mode's approvers, and who else may stop the bot's turns. Nil, as
	// Proposals, refuses every approval.
	People ports.People
	// Proposals reviews and runs the bot's proposals as their approver.
	Proposals Approver
	// BotKeys revokes the keys of revoked installs' bots, hosted, where
	// bot mode makes them; nil leaves them be (a standalone install, which
	// makes none).
	BotKeys ports.BotCredentials
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Bot handles Slack's intake and follows Founder's turns into Slack. It is
// safe for concurrent use: the worker runs several of its calls at once.
type Bot struct {
	db       Store
	sessions integration.SessionHost
	slack    *slack.Client
	keys     *secretbox.Keyring
	// transport opens installs' bots and the backfill's callers; relayed is
	// it on a standalone install, nil hosted.
	transport Transport
	relayed   *Relayed
	// updateGap is the least time between two updates of one reply
	// message, before the install's live follows multiply it.
	updateGap time.Duration
	site      slack.Site
	cfg       config.SlackConfig
	now       func() time.Time
	// people and proposals are bot mode's approvals; nil refuses them.
	people    ports.People
	proposals Approver
	// botKeys revokes retired bots' keys; nil leaves them be.
	botKeys ports.BotCredentials

	mu sync.Mutex
	// users is what users.info said, by install and user, kept userTTL.
	users map[[2]string]cachedUser
	// agentOff is, by install, until when its threads get no agent status.
	agentOff map[string]time.Time

	// feed folds the data connector's feeds and reads their mirror; nil
	// while the feed is off.
	feed *connector.SlackFeedOptions
	// closed is the last day whose days the sweep closed (feeds).
	closed time.Time
}

type cachedUser struct {
	user slack.User
	at   time.Time
}

// New is a Bot. It needs every option but Now.
func New(o Options) (*Bot, error) {
	switch {
	case o.DB == nil || o.Sessions == nil || o.Slack == nil || o.Keys == nil:
		return nil, errors.New("slackbot: a store, a session host, a Slack client and a keyring are required")
	case o.Relayed != nil && (o.Relayed.DB == nil || o.Relayed.Access == nil):
		return nil, errors.New("slackbot: a relayed bot reads its installs and its link")
	case o.Cfg.Lease <= 0 || o.Cfg.PollInterval <= 0 || o.Cfg.MaxAttempts < 1 || o.Cfg.RetryBackoff <= 0 || o.Cfg.BusyWait <= 0 ||
		o.Cfg.Retention.Threads <= 0:
		return nil, errors.New("slackbot: foundry.slack's lease, poll interval, attempts, back-off, busy wait and thread retention are positive")
	}
	site, err := slack.NewSite(o.Cfg.SiteURL)
	if err != nil {
		return nil, fmt.Errorf("slackbot: foundry.slack.site_url: %w", err)
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	b := &Bot{db: o.DB, sessions: o.Sessions, slack: o.Slack, keys: o.Keys, site: site, cfg: o.Cfg, now: now, people: o.People, proposals: o.Proposals,
		botKeys: o.BotKeys, users: map[[2]string]cachedUser{}, agentOff: map[string]time.Time{}, transport: Direct{DB: o.DB, Keys: o.Keys, WebAPI: o.WebAPI},
		updateGap: updateGap}
	if o.Relayed != nil {
		// Every call of a relayed install shares its link's calls a minute:
		// a reply is updated a quarter as often.
		b.transport, b.relayed, b.updateGap = *o.Relayed, o.Relayed, relayedUpdateGap
	}
	if o.Objects != nil && o.Cfg.Feed.Enabled {
		f, bf := o.Cfg.Feed, o.Cfg.Backfill
		if f.Spacing <= 0 || f.ReadBudget.Rows < 1 || f.ReadBudget.Bytes < 1 || bf.HistoryPerMinute < 1 || bf.RepliesPerMinute < 1 ||
			bf.PageLimit < 1 || bf.RepliesDays < 1 || bf.MaxAttempts < 1 || bf.RetryBackoff <= 0 {
			return nil, errors.New("slackbot: foundry.slack.feed's spacing and read budget, and foundry.slack.backfill's pace, page, " +
				"replies days, attempts and back-off are positive")
		}
		b.feed = &connector.SlackFeedOptions{Feed: o.DB, Objects: o.Objects, Budget: model.SlackFeedBudget{Rows: f.ReadBudget.Rows, Bytes: f.ReadBudget.Bytes},
			HelixAppID: o.Cfg.AppID, RepliesDays: bf.RepliesDays}
	}
	return b, nil
}

// token is an install's bot as its transport opens it: store.ErrNotFound
// once it has none.
func (b *Bot) token(ctx context.Context, installID string) (slack.Bot, error) {
	return b.transport.Bot(ctx, installID)
}

// user is what users.info says of userID in bot's install, from the cache
// while it is fresh.
func (b *Bot) user(ctx context.Context, bot slack.Bot, userID string) (slack.User, error) {
	key := [2]string{bot.InstallID, userID}
	now := b.now()
	b.mu.Lock()
	cached, ok := b.users[key]
	b.mu.Unlock()
	if ok && now.Sub(cached.at) < userTTL {
		return cached.user, nil
	}
	u, err := b.slack.UserInfo(ctx, bot, userID)
	if err != nil {
		return slack.User{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, c := range b.users {
		if now.Sub(c.at) >= userTTL {
			delete(b.users, k)
		}
	}
	b.users[key] = cachedUser{user: u, at: now}
	return u, nil
}

// inDM reports a direct message's channel: Slack's IM ids start with D.
func inDM(channel string) bool { return strings.HasPrefix(channel, "D") }

// tell sends user a message only they read where they are: an ephemeral one
// in a channel, a plain one in the bot's direct message (thread threadTS,
// "" at top level), where only they and the bot are.
func (b *Bot) tell(ctx context.Context, bot slack.Bot, channel, threadTS, user string, m slack.Message) error {
	if inDM(channel) {
		_, err := b.slack.PostMessage(ctx, bot, slack.Post{Channel: channel, ThreadTS: threadTS, Message: m})
		return err
	}
	return b.slack.PostEphemeral(ctx, bot, slack.Ephemeral{Channel: channel, User: user, ThreadTS: threadTS, Message: m})
}

// say is text as a message of one section.
func (b *Bot) say(text string) slack.Message {
	m := b.site.Text(text)
	return slack.Message{Text: slack.Cut(m, slack.MaxMessageText), Blocks: []slack.Block{slack.Section{Text: slack.Cut(m, slack.MaxSectionText)}}}
}

// setAgentStatus sets a thread's agent session, unless the install's Slack
// has none: feature_disabled or missing_scope leave the install without
// them for agentOffFor. Every other failure fails that call only.
func (b *Bot) setAgentStatus(ctx context.Context, bot slack.Bot, channel, threadTS string, s slack.AgentStatus) {
	b.mu.Lock()
	until, off := b.agentOff[bot.InstallID]
	b.mu.Unlock()
	if off && b.now().Before(until) {
		return
	}
	err := b.slack.SetAgentStatus(ctx, bot, channel, threadTS, s)
	switch {
	case slack.IsCode(err, "feature_disabled", "missing_scope"):
		b.mu.Lock()
		b.agentOff[bot.InstallID] = b.now().Add(agentOffFor)
		b.mu.Unlock()
	case err != nil:
		log.From(ctx).Debug("slack agent status not set", "install_id", bot.InstallID, "status", s.String(), "err", err)
	}
}

// reasonOf is an API error's ErrorInfo reason, "" for none.
func reasonOf(err error) string {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}
	return ""
}

// rateLimited is how long a rate limit holds a call back, false when err
// isn't one.
func rateLimited(err error) (time.Duration, bool) {
	var rl *slack.RateLimitedError
	if errors.As(err, &rl) {
		return max(rl.RetryAfter, time.Second), true
	}
	return 0, false
}

// slackGone reports a Web API answer that says the bot can't act there any
// more: its token was revoked, the channel or message is gone or closed to
// it, or Helix's relay refused the call. Trying again changes nothing.
func slackGone(err error) bool {
	var api *slack.APIError
	if errors.As(err, &api) && strings.HasPrefix(api.Code, relayRefusal) {
		// Helix's relay refused it for good (Relayed).
		return true
	}
	return slack.IsCode(err, "invalid_auth", "account_inactive", "token_revoked", "token_expired", "not_authed", "channel_not_found",
		"not_in_channel", "is_archived", "message_not_found", "cant_update_message", "user_not_in_channel", "no_permission")
}

// slackRefused reports a Web API answer refusing what was sent for good:
// an error code other than Slack's own faults (internal_error, fatal_error,
// service_unavailable, request_timeout), which trying again gets past. No
// answer, or an HTTP error, isn't one.
func slackRefused(err error) bool {
	var api *slack.APIError
	return errors.As(err, &api) && api.Code != "" &&
		!slices.Contains([]string{"internal_error", "fatal_error", "service_unavailable", "request_timeout"}, api.Code)
}

// sentence is an API refusal's message as Slack shows it: its first letter
// capital, ending in a full stop.
func sentence(err error) string {
	msg := strings.TrimSpace(status.Convert(err).Message())
	if msg == "" {
		return "Founder refused this."
	}
	msg = strings.ToUpper(msg[:1]) + msg[1:]
	if !strings.HasSuffix(msg, ".") && !strings.HasSuffix(msg, "?") && !strings.HasSuffix(msg, "!") {
		msg += "."
	}
	return msg
}

// refusedInWords reports an API answer the person reads in its own words:
// a rule of theirs or their workspace's, never a fault.
func refusedInWords(err error) bool {
	switch status.Code(err) {
	case codes.FailedPrecondition, codes.ResourceExhausted, codes.PermissionDenied, codes.InvalidArgument:
		return true
	}
	return false
}
