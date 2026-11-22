package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/slack"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Channel messages and channel events Slack sends the app (message.channels,
// message.groups and the channel lifecycle events): the Slack data
// connector's feed (foundry.slack.feed), and the bot's answers to replies
// that don't mention it (foundry.slack.follow).

// slackInstallCached is how long the feed's intake keeps an install it
// found by team: hits only, so a new install is found at once.
const slackInstallCached = 30 * time.Second

var slackFeedTaken = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_slack_feed_events_total",
	Help: "Channel messages and channel events Slack sent the app, by outcome: appended, duplicate, full (recorded as a gap), unbound " +
		"(no connection reads the team), followed (an unmentioned reply taken), rate_limited (Slack's app_rate_limited), busy (refused: " +
		"the replica's feed slots were taken), disabled, or dropped_<why> (ext_shared, unknown_team, not_ours, unread).",
}, []string{"outcome"})

// errSlackFeedBusy is a channel event refused because the replica's feed
// slots (foundry.slack.feed.backend_in_flight) are all taken: the door
// answers it 5xx at once, and Slack retries.
var errSlackFeedBusy = errors.New("the replica takes no more channel events just now")

// slackFeedIntake is what the API holds for channel events: the slots
// that keep their appends off the connections mentions and clicks need,
// and the installs it found by team. Its zero value is ready: the slots
// are made at the first event, from foundry.slack.feed.
type slackFeedIntake struct {
	once     sync.Once
	slots    chan struct{}
	mu       sync.Mutex
	installs map[string]slackInstallHit
}

// slackInstallHit is an install found by team, until at.
type slackInstallHit struct {
	install *model.SlackInstall
	until   time.Time
}

// install is the live install of team (else of its Grid org, as a click's
// is found), from the cache while fresh unless fresh is set; nil when
// there is none. cached says it came from the cache.
func (f *slackFeedIntake) install(ctx context.Context, db foundrystore.Store, team, enterprise string, fresh bool) (
	install *model.SlackInstall, cached bool, err error) {
	now := time.Now()
	f.mu.Lock()
	hit, ok := f.installs[team]
	f.mu.Unlock()
	if ok && !fresh && now.Before(hit.until) {
		return hit.install, true, nil
	}
	install, err = db.GetFoundrySlackInstallByTeam(ctx, team)
	if errors.Is(err, foundrystore.ErrNotFound) && enterprise != "" {
		install, err = db.GetFoundrySlackInstallByEnterprise(ctx, enterprise)
	}
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || errors.Is(err, foundrystore.ErrConflict):
		f.mu.Lock()
		delete(f.installs, team)
		f.mu.Unlock()
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	f.mu.Lock()
	if f.installs == nil {
		f.installs = map[string]slackInstallHit{}
	}
	f.installs[team] = slackInstallHit{install: install, until: now.Add(slackInstallCached)}
	f.mu.Unlock()
	return install, false, nil
}

// receiveSlackFeed takes a verified channel event (e, of the request
// whose event id is eventID): with one slot of the replica's feed slots
// (none free is errSlackFeedBusy, at once), it finds the team's install
// and, wherever foundry.slack.feed is on, appends what the connection
// reading it keeps (slackFeedRows) to its feed; wherever
// foundry.slack.follow is on, a reply in a thread the bot follows is
// taken as a message (followSlackReply). Slack's app_rate_limited is
// recorded on the install, and as gaps of its feed. A channel shared with
// another organisation, and a team with no install, are dropped; the
// app's own bot's messages are kept without their text.
func (s *Service) receiveSlackFeed(ctx context.Context, eventID string, e slack.FeedEvent) (*foundryv1.ReceiveSlackRequestResponse, string, error) {
	cfg := s.deps.Cfg.Foundry.Slack
	if !cfg.ReadsHistory() {
		slackFeedTaken.WithLabelValues("disabled").Inc()
		return slackOK(), "ignored", nil
	}
	f := &s.slackFeed
	f.once.Do(func() { f.slots = make(chan struct{}, max(cfg.Feed.BackendInFlight, 1)) })
	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	default:
		slackFeedTaken.WithLabelValues("busy").Inc()
		return nil, "", errSlackFeedBusy
	}
	team, enterprise, ext := slackFeedPlace(e)
	if ext {
		slackFeedTaken.WithLabelValues("dropped_ext_shared").Inc()
		return slackOK(), "feed", nil
	}
	install, cached, err := f.install(ctx, s.deps.DB, team, enterprise, false)
	if err != nil {
		return nil, "", err
	}
	if install == nil {
		slackFeedTaken.WithLabelValues("dropped_unknown_team").Inc()
		return slackOK(), "feed", nil
	}
	if limited, ok := e.(slack.RateLimited); ok {
		if _, err := s.deps.DB.RecordFoundrySlackEventsLimited(ctx, install.ID, limited.Minute, cfg.Feed.Spacing); err != nil &&
			!errors.Is(err, foundrystore.ErrNotFound) {
			return nil, "", err
		}
		log.From(ctx).Warn("slack dropped a team's events past its hourly limit", "install_id", install.ID, "minute", limited.Minute)
		slackFeedTaken.WithLabelValues("rate_limited").Inc()
		return slackOK(), "feed", nil
	}
	if !model.ValidSlackID(eventID) {
		return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusBadRequest}, "malformed", nil
	}
	if cfg.Feed.Enabled {
		rows := slackFeedRows(e, eventID, install, cfg.AppID, s.deps.Cfg.Foundry.Documents.MaxBytes)
		if len(rows) == 0 {
			slackFeedTaken.WithLabelValues("dropped_not_ours").Inc()
		} else {
			batch := model.SlackFeedBatch{Rows: rows, At: time.Now()}
			limits := model.SlackFeedLimits{Spacing: cfg.Feed.Spacing, MaxRows: int64(cfg.Feed.MaxBufferedRows)}
			appended, err := s.deps.DB.AppendFoundrySlackFeed(ctx, install.ID, batch, limits)
			if err == nil && appended.Outcome == model.SlackAppendUnbound && cached {
				// The cached install may be gone, and the team installed again.
				var again *model.SlackInstall
				if again, _, err = f.install(ctx, s.deps.DB, team, enterprise, true); err == nil && again != nil && again.ID != install.ID {
					install = again
					appended, err = s.deps.DB.AppendFoundrySlackFeed(ctx, install.ID, batch, limits)
				}
			}
			if err != nil {
				return nil, "", err
			}
			slackFeedTaken.WithLabelValues(string(appended.Outcome)).Inc()
		}
	}
	if m, ok := e.(slack.ChannelMessage); ok && cfg.Follow.Enabled {
		if err := s.followSlackReply(ctx, eventID, install, m); err != nil {
			return nil, "", err
		}
	}
	return slackOK(), "feed", nil
}

// slackFeedPlace is the team and Grid org a feed event is of, and whether
// its channel is shared with another organisation.
func slackFeedPlace(e slack.FeedEvent) (team, enterprise string, extShared bool) {
	var at slack.Place
	switch e := e.(type) {
	case slack.RateLimited:
		return e.TeamID, "", false
	case slack.ChannelMessage:
		at = e.At
	case slack.MessageChanged:
		at = e.At
	case slack.MessageDeleted:
		at = e.At
	case slack.ParentReplied:
		at = e.At
	case slack.ChannelRenamed:
		at = e.At
	case slack.ChannelGone:
		at = e.At
	case slack.ChannelArchived:
		at = e.At
	case slack.MemberJoined:
		at = e.At
	case slack.MemberLeft:
		at = e.At
	}
	return at.TeamID, at.EnterpriseID, at.ExtShared
}

// slackFeedRows are the buffer rows a feed keeps of e, an event of
// install's team whose event id is eventID (each row's dedupe key), as the
// fold reads them (connector/slack_fold.go): a message, a change, a
// delete, a root's reply fields, a rename or an archive (channel), the
// bot's own join (joined; the join message and member_joined_channel are
// kept once a minute) and its removal (left). Another member's join or
// leave is none. A message's text is cut at maxText bytes, and Helix's own
// bot's (by its user, bot id or app, appID) is kept without its text, so a
// thread's counts are Slack's.
func slackFeedRows(e slack.FeedEvent, eventID string, install *model.SlackInstall, appID string, maxText int64) []model.SlackFeedRow {
	key := model.SlackEventDedupe(eventID)
	message := func(at slack.Place, m slack.StoredMessage, kind model.SlackFeedRowKind) []model.SlackFeedRow {
		ours := func(id, of string) bool { return id != "" && id == of }
		if ours(m.User, install.BotUserID) || ours(m.BotID, install.BotID) || ours(m.AppID, appID) || ours(m.BotProfile.AppID, appID) {
			m.Text = ""
		}
		if int64(len(m.Text)) > maxText {
			cut := int(maxText)
			for cut > 0 && !utf8.RuneStart(m.Text[cut]) {
				cut--
			}
			m.Text = m.Text[:cut]
		}
		// A reply lands on its root's day; a root, a message in no thread
		// and a broadcast on their own.
		day := m.TS
		if m.ThreadTS != "" && m.ThreadTS != m.TS && m.Subtype != "thread_broadcast" {
			day = m.ThreadTS
		}
		payload, _ := json.Marshal(m) // plain fields
		return []model.SlackFeedRow{{DedupeKey: key, ChannelID: at.ChannelID, Day: model.SlackDay(slackTSTime(day)), Kind: kind,
			Source: model.SlackFeedFromEvent, Payload: payload}}
	}
	row := func(at slack.Place, day time.Time, kind model.SlackFeedRowKind, payload any) []model.SlackFeedRow {
		raw, _ := json.Marshal(payload) // plain fields
		return []model.SlackFeedRow{{DedupeKey: key, ChannelID: at.ChannelID, Day: model.SlackDay(day), Kind: kind, Source: model.SlackFeedFromEvent,
			Payload: raw}}
	}
	switch e := e.(type) {
	case slack.ChannelMessage:
		return message(e.At, e.Message, model.SlackRowMessage)
	case slack.MessageChanged:
		return message(e.At, e.Message, model.SlackRowChanged)
	case slack.MessageDeleted:
		day := e.DeletedTS
		if e.ThreadTS != "" {
			day = e.ThreadTS
		}
		return row(e.At, slackTSTime(day), model.SlackRowDeleted, map[string]string{"deleted_ts": e.DeletedTS, "thread_ts": e.ThreadTS})
	case slack.ParentReplied:
		return row(e.At, slackTSTime(e.TS), model.SlackRowReplied, e)
	case slack.ChannelRenamed:
		return row(e.At, time.Now(), model.SlackRowChannel, map[string]string{"channel_name": e.Name})
	case slack.ChannelArchived:
		return row(e.At, time.Now(), model.SlackRowChannel, map[string]bool{"archived": e.Archived})
	case slack.ChannelGone:
		return row(e.At, time.Now(), model.SlackRowLeft, map[string]string{"why": string(e.Why)})
	case slack.MemberLeft:
		if e.UserID != install.BotUserID {
			return nil
		}
		return row(e.At, time.Now(), model.SlackRowLeft, map[string]string{"why": string(slack.GoneLeft)})
	case slack.MemberJoined:
		if e.UserID != install.BotUserID {
			return nil
		}
		rows := row(e.At, e.JoinedAt, model.SlackRowJoined, e)
		rows[0].DedupeKey = model.SlackBackfillDedupe(e.At.ChannelID, e.JoinedAt.UTC().Truncate(time.Minute).Format(time.RFC3339), "",
			model.SlackRowJoined)
		return rows
	}
	return nil
}

// slackTSTime is a Slack ts as a time; the Unix epoch for one that isn't
// (ParseEvent reads only valid ones).
func slackTSTime(ts string) time.Time {
	secs, frac, _ := strings.Cut(ts, ".")
	s, _ := strconv.ParseInt(secs, 10, 64)
	frac = (frac + "000000")[:6]
	us, _ := strconv.ParseInt(frac, 10, 64)
	return time.Unix(s, us*1000).UTC()
}

// followSlackReply applies the bot's rule for replies that don't mention
// it to m, a channel message of install's team: in a thread the bot posted
// in for its person (a mapping of per_person mode), a message from anyone
// but the bot and the thread's people records that someone else spoke
// (others_at); a reply from a mapping's person is taken, as a message of
// their conversation, while the bot posted there last apart from them and
// within foundry.slack.follow.window. Bot mode's threads are L2's.
func (s *Service) followSlackReply(ctx context.Context, eventID string, install *model.SlackInstall, m slack.ChannelMessage) error {
	msg := m.Message
	if msg.ThreadTS == "" || msg.ThreadTS == msg.TS || msg.User == install.BotUserID || (install.BotID != "" && msg.BotID == install.BotID) {
		return nil
	}
	if _, perPerson := install.Mode.(model.SlackPerPerson); !perPerson {
		return nil
	}
	mappings, err := s.deps.DB.ListFoundrySlackThreadMappings(ctx, install.ID, m.At.ChannelID, msg.ThreadTS)
	if err != nil || len(mappings) == 0 {
		return err
	}
	var mine *model.SlackThread
	for _, t := range mappings {
		if t.SlackUserID == msg.User {
			mine = t
		}
	}
	if mine == nil {
		return s.deps.DB.MarkFoundrySlackThreadOthers(ctx, install.ID, m.At.ChannelID, msg.ThreadTS, m.EventTime)
	}
	reply, ok := m.Reply(install.BotUserID)
	window := s.deps.Cfg.Foundry.Slack.Follow.Window
	if !ok || mine.BotAt == nil || (mine.OthersAt != nil && !mine.BotAt.After(*mine.OthersAt)) || m.EventTime.Sub(*mine.BotAt) > window {
		return nil
	}
	payload, err := slack.EventPayload(reply)
	if err != nil {
		return err
	}
	in := &model.SlackIntake{ID: id.New(), InstallID: install.ID, DedupeKey: model.SlackEventDedupe(eventID), Kind: model.SlackIntakeEvent,
		Conversation: model.SlackConversation(install.ID, m.At.ChannelID, msg.ThreadTS, msg.User), SlackUserID: msg.User, Payload: payload}
	switch err := s.deps.DB.QueueFoundrySlackIntake(ctx, in); {
	case errors.Is(err, foundrystore.ErrConflict):
		return nil
	case err != nil:
		return err
	}
	slackFeedTaken.WithLabelValues("followed").Inc()
	return nil
}
