package connector

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/helixdb/helix-foundry/backend/config"
)

var _ PassKind = (*slackKind)(nil)

// slackTokenRE is a bot token. A user token (xoxp-) reads as its person,
// their direct messages included, so Foundry takes none.
var slackTokenRE = regexp.MustCompile(`^xoxb-[A-Za-z0-9-]+$`)

const (
	defaultSlackHistoryDays = 90
	// An internal app keeps Slack's full page sizes: 200 is what Slack
	// suggests for its lists, and history and replies return up to 999
	// messages a page (replies pages only under 1,000).
	slackListPage    = "200"
	slackHistoryPage = "999"
	maxSlackPages    = 20_000
	// slackMessageOverhead is what a kept message costs beyond its text.
	slackMessageOverhead = 256
	// slackDocumentMargin is the most a pass leaves of its time once it
	// stops fetching documents, for the syncer to write what it read: a
	// quarter of the time it had, at most this.
	slackDocumentMargin = 15 * time.Minute
)

// slackIntervals space one read's calls of each Web API method under
// Slack's limit for it, which is per method, app and workspace: Tier 2
// allows about 20 calls a minute, Tier 3 50 and Tier 4 100. auth.test is
// called once a read. Tests set them to 0.
var slackIntervals = map[string]time.Duration{
	"auth.test":             600 * time.Millisecond,
	"users.list":            3 * time.Second,         // Tier 2
	"conversations.list":    3 * time.Second,         // Tier 2
	"users.conversations":   1200 * time.Millisecond, // Tier 3
	"conversations.history": 1200 * time.Millisecond, // Tier 3
	"conversations.replies": 1200 * time.Millisecond, // Tier 3
	"conversations.members": 600 * time.Millisecond,  // Tier 4
}

// slackTextBudget bounds the message text one read keeps for documents.
// Past it, the oldest days' messages are dropped, and a day whose document
// is then wanted is read again. Tests lower it.
var slackTextBudget = 64 << 20

// slackDayBytes bounds what one day keeps of its messages, and of its
// document's text: the most foundry.documents.max_bytes may be, past which
// the syncer cuts a document. Tests lower it.
var slackDayBytes = config.MaxDocumentBytes

// slackNow is the clock the history window is counted from. Tests set it.
var slackNow = time.Now

var slackObjects = []saasTable{
	{"users", "user_id"},
	{"channels", "channel_id"},
	// Discover keys a channel's members by (channel_id, user_id).
	{"channel_members", "channel_id"},
	// The top-level messages and thread broadcasts conversations.history
	// lists, each with the UTC day it was sent on and that day's
	// channel_days row. A thread's replies show only in its day's document,
	// and replies to a thread started before the window in neither: a read
	// of every thread's replies would be a conversations.replies call each.
	{"messages", "message_id"},
	{"channel_days", "channel_day_id"},
}

var slackForeignKeys = map[string][]ForeignKey{
	"channels":        {foreignKey("creator_id", "users", "user_id")},
	"channel_members": {foreignKey("channel_id", "channels", "channel_id"), foreignKey("user_id", "users", "user_id")},
	"messages": {foreignKey("channel_id", "channels", "channel_id"), foreignKey("user_id", "users", "user_id"),
		foreignKey("channel_day_id", "channel_days", "channel_day_id")},
	"channel_days": {foreignKey("channel_id", "channels", "channel_id")},
}

// slackSkipped are the subtypes Slack posts about a channel itself, which
// say nothing anyone wrote: neither rows nor documents show them.
var slackSkipped = map[string]bool{
	"channel_join": true, "channel_leave": true, "channel_topic": true, "channel_purpose": true, "channel_name": true,
	"channel_archive": true, "channel_unarchive": true, "bot_add": true, "bot_remove": true,
}

// slackRejected are the errors of a token Slack no longer takes.
var slackRejected = map[string]bool{
	"invalid_auth": true, "not_authed": true, "account_inactive": true, "token_revoked": true, "token_expired": true,
}

var (
	// errMissingScope is a method the app has no scope for; the RefusedError
	// wrapping it says which scope to add.
	errMissingScope = errors.New("slack: missing_scope")
	// errChannelGone is a channel the app was removed from, or that was
	// deleted, since it was listed: a read skips it.
	errChannelGone = errors.New("slack: the channel is gone")
	// errThreadGone is a thread deleted since its parent was listed: it has no replies.
	errThreadGone = errors.New("slack: the thread is gone")
)

// slackGone are the errors of what changed since a read listed it.
var slackGone = map[string]error{"not_in_channel": errChannelGone, "channel_not_found": errChannelGone, "thread_not_found": errThreadGone}

// slackScopes are the scopes a token has, as the X-OAuth-Scopes header of
// auth.test's answer lists them. Slack sends it with every answer; nil, for
// an answer without it, has every scope, so the calls find out.
type slackScopes map[string]bool

func (s slackScopes) has(scope string) bool { return s == nil || s[scope] }

// types are the conversation types the token may list: public channels,
// and private ones with groups:read. Direct messages never.
func (s slackScopes) types() string {
	if s.has("groups:read") {
		return "public_channel,private_channel"
	}
	return "public_channel"
}

// slackHistoryScope is the scope the history of a public or a private channel needs.
func slackHistoryScope(private bool) string {
	if private {
		return "groups:history"
	}
	return "channels:history"
}

// slackAuth is auth.test's answer: the workspace, the bot user's handle
// and the token's scopes.
type slackAuth struct {
	team, bot string
	scopes    slackScopes
}

// slackConversations is the channel_days table's stream: each channel day's messages.
var slackConversations = Stream{Name: "conversations", Table: "channel_days", Title: "Conversations", Format: Markdown,
	Description: "Each finished UTC day of one Slack channel the app is in, as Markdown: a heading with the channel and the UTC day, " +
		"the channel's name and id, then each message posted that day in time order as \"**Name** HH:MM: text\", in UTC, " +
		"with the replies of a thread it started listed beneath it, dated when sent on a later day. Replies show only here, " +
		"so replies to a thread started before the kept days are left out, and a reply edited later reads as first written " +
		"until its day changes again. A day too long for one document keeps its earliest messages. People are named " +
		"by their Slack real name, else their handle, and mentions read @Name and #channel."}

type slackKind struct {
	base string
	http *http.Client
}

func (k *slackKind) Name() string        { return "slack" }
func (k *slackKind) DisplayName() string { return "Slack" }

// SlackSettings are a Slack connector's settings: how many days of channel
// history it keeps.
type SlackSettings struct {
	HistoryDays int `json:"history_days"`
}

func (k *slackKind) Prepare(src Source) (json.RawMessage, error) {
	if !slackTokenRE.MatchString(strings.TrimSpace(src.Secret)) {
		return nil, invalid("enter a Slack bot token (xoxb-…)")
	}
	var s SlackSettings
	decoder := json.NewDecoder(bytes.NewReader(orEmpty(src.Settings)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return nil, invalid("this connector takes only its token and how many days of history to keep")
	}
	switch {
	case s.HistoryDays == 0:
		s.HistoryDays = defaultSlackHistoryDays
	case s.HistoryDays < 1 || s.HistoryDays > 365:
		return nil, invalid("keep 1 to 365 days of history")
	}
	return json.Marshal(s)
}

// Streams are the conversations, always.
func (k *slackKind) Streams(Source) []Stream { return []Stream{slackConversations} }

// Discover proves the token with auth.test, which also lists its scopes,
// then probes each object with a one-item read: users.list for users,
// conversations.list for channels, and users.conversations for the
// channels the app is in, which channel_members needs. Messages and days
// also need conversations.history of a channel the app is in: of a public
// and of a private one, those there are, as each needs its own scope. A
// method the app has no scope for makes its objects unavailable, saying
// which scope to add; one kind of channel whose history it may not read,
// while it may the other's, is a warning that says so. An app in no
// channel yet lists them with a warning, since only an invite shows what
// it may read.
func (k *slackKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	c := k.client(src.Secret)
	auth, err := c.auth(ctx)
	if err != nil {
		return Discovery{}, err
	}
	var (
		mu      sync.Mutex
		reasons = map[string]string{}
		joined  = map[bool]slackChannel{} // by is_private: the first channel of each kind the app is in
	)
	readable, err := probeAll(ctx, []string{"users", "channels", "joined"}, func(ctx context.Context, probe string) (bool, error) {
		var err error
		switch probe {
		case "users":
			_, _, err = c.call(ctx, "users.list", url.Values{"limit": {"1"}})
		case "channels":
			_, _, err = c.call(ctx, "conversations.list", url.Values{"limit": {"1"}, "types": {auth.scopes.types()}})
		default:
			// A filtered page may be empty though more follow, so they are paged for.
			err = c.pages(ctx, "users.conversations", "channels", url.Values{"limit": {slackListPage}, "types": {auth.scopes.types()}},
				func(raw json.RawMessage) error {
					var ch slackChannel
					if json.Unmarshal(raw, &ch) != nil || ch.ID == "" {
						return errors.New("slack users.conversations: a listed channel has no id")
					}
					if _, ok := joined[ch.IsPrivate]; !ok {
						joined[ch.IsPrivate] = ch
					}
					if len(joined) == 2 {
						return errStopPaging
					}
					return nil
				})
		}
		var refusedErr *RefusedError
		if errors.Is(err, errMissingScope) && errors.As(err, &refusedErr) {
			mu.Lock()
			reasons[probe] = refusedErr.Message
			mu.Unlock()
			return false, nil
		}
		return err == nil, err
	})
	if err != nil {
		return Discovery{}, err
	}
	history := map[bool]string{} // by is_private: why the app may not read that kind's history
	readsHistory := false
	for _, private := range []bool{false, true} {
		ch, ok := joined[private]
		if !ok {
			continue
		}
		if scope := slackHistoryScope(private); !auth.scopes.has(scope) {
			history[private] = "add the " + scope + " scope to the app and reinstall it"
			continue
		}
		_, _, err := c.call(ctx, "conversations.history", url.Values{"channel": {ch.ID}, "limit": {"1"}})
		var refusedErr *RefusedError
		switch {
		case errors.Is(err, errMissingScope) && errors.As(err, &refusedErr):
			history[private] = refusedErr.Message
		case errors.Is(err, errChannelGone):
			// Gone since it was listed, which says nothing of the scopes.
		case err != nil:
			return Discovery{}, err
		default:
			readsHistory = true
		}
	}
	// In no channel yet, the scopes alone say whether it may read history.
	if len(joined) == 0 && !auth.scopes.has(slackHistoryScope(false)) && !auth.scopes.has(slackHistoryScope(true)) {
		history[false] = "add the " + slackHistoryScope(false) + " scope to the app and reinstall it"
	}
	d := saasDiscovery(slackObjects, func(name string) bool {
		switch name {
		case "users", "channels":
			return readable[name]
		case "channel_members":
			return readable["channels"] && readable["joined"]
		}
		return readable["channels"] && readable["joined"] && (readsHistory || len(history) == 0)
	}, slackForeignKeys)
	if len(d.Tables) == 0 {
		return Discovery{}, refused(nil, "this token cannot read Slack users or channels; add the app's scopes and test again")
	}
	for i, u := range d.Unavailable {
		switch u.Name {
		case "users", "channels":
			d.Unavailable[i].Reason = reasons[u.Name]
		case "channel_members":
			d.Unavailable[i].Reason = cmp.Or(reasons["joined"], reasons["channels"])
		default:
			d.Unavailable[i].Reason = cmp.Or(reasons["joined"], history[false], history[true], reasons["channels"])
		}
	}
	for i, t := range d.Tables {
		if t.Name == "channel_members" {
			d.Tables[i].PrimaryKey = []string{"channel_id", "user_id"}
		}
		d.Tables[i].Windowed = WindowedTable("slack", t.Name)
	}
	if !readable["channels"] || !readable["joined"] {
		return d, nil
	}
	if len(joined) == 0 {
		d.Warnings = append(d.Warnings, "The app is in no channels yet: invite it to each channel to sync with /invite @"+auth.bot+", then test again")
	}
	for _, private := range []bool{false, true} {
		if readsHistory && history[private] != "" {
			kind := map[bool]string{false: "public", true: "private"}[private]
			d.Warnings = append(d.Warnings, "The app cannot read the messages of the "+kind+" channels it is in: "+history[private]+", then test again")
		}
	}
	return d, nil
}

func (k *slackKind) Hint(secret string) string {
	if hint := keyHint(strings.TrimSpace(secret), 0); hint != "…" {
		return "xoxb" + hint
	}
	return "…"
}

// Sync and SyncDocuments read one table through SyncPass. The syncer
// reads a PassKind's tables in one pass and calls neither.
func (k *slackKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	return k.SyncDocuments(ctx, src, table, w, noDocuments{})
}

func (k *slackKind) SyncDocuments(ctx context.Context, src Source, table Table, rows RowWriter, docs DocumentWriter) (Table, error) {
	_, err := k.SyncPass(ctx, src, []Table{table}, map[string]RowWriter{table.Name: rows}, docs)
	return table, err
}

// SyncPass reads users and channels from their lists, then each channel
// the app is in: its members, and its messages of the window, the
// history_days UTC days before today and today (today's day gets its row,
// and its document once it is over), with one
// conversations.history page per 999 messages, of each kind of channel
// whose history the token's scopes allow. The messages make the
// channel_days rows and, when that table is read, its documents, offered
// most recently active first once every channel is read; a day's thread
// replies are read only when its document is wanted, and users.list, to
// name people, once a document is (or the users table is read). Near
// ctx's deadline (slackDocumentMargin), the days are still offered, so the
// ones the stream holds stay, but none is fetched: they wait for the next
// sync.
func (k *slackKind) SyncPass(ctx context.Context, src Source, tables []Table, rows map[string]RowWriter, docs DocumentWriter) ([]Table, error) {
	p, err := k.pass(ctx, src, tables, rows, docs)
	if err != nil {
		return tables, err
	}
	return tables, p.run(ctx)
}

// pass starts a read of tables, full or of changes, on the UTC day it is
// now: its window is the history_days days before that day, and that day.
func (k *slackKind) pass(ctx context.Context, src Source, tables []Table, rows map[string]RowWriter, docs DocumentWriter) (*slackPass, error) {
	var s SlackSettings
	_ = json.Unmarshal(orEmpty(src.Settings), &s) // stored by Prepare
	now := slackNow().UTC()
	p := &slackPass{c: k.client(src.Secret), read: map[string]bool{}, rows: rows, docs: docs, channels: map[string]string{},
		days: map[string]*slackDay{}, historyDays: cmp.Or(s.HistoryDays, defaultSlackHistoryDays), today: now.Format(time.DateOnly)}
	p.window = now.Truncate(24*time.Hour).AddDate(0, 0, -p.historyDays)
	for _, table := range tables {
		if !slices.ContainsFunc(slackObjects, func(o saasTable) bool { return o.name == table.Name }) {
			return nil, invalid("Slack has no object %q", table.Name)
		}
		p.read[table.Name] = true
	}
	p.keep = p.read["channel_days"] && !docs.Paused(slackConversations.Name)
	if deadline, ok := ctx.Deadline(); ok {
		p.stop = deadline.Add(-min(time.Until(deadline)/4, slackDocumentMargin))
	}
	return p, nil
}

// slackPass is one read, a SyncPass or a change read: the tables it reads,
// the names its documents give people and channels, and each channel day
// it reads.
type slackPass struct {
	c    *slackClient
	read map[string]bool
	rows map[string]RowWriter
	docs DocumentWriter
	// keep is set when the pass keeps messages for documents: their table
	// is read and their stream is not paused.
	keep   bool
	team   string
	scopes slackScopes
	// people names user ids by real name, else handle, once users.list is
	// read, and is nil before; byID is set when the app may not read users,
	// so documents name people by id.
	people map[string]string
	byID   bool
	// channels names channel ids: the channels the app is in, and every
	// channel conversations.list lists once listed is set. A change read
	// reads no channels table, so it sets nameChannels and lists them once
	// a document mentions one it can't name, as a full read, which lists
	// them, names it.
	channels     map[string]string
	listed       bool
	nameChannels bool
	days         map[string]*slackDay
	kept         int // the bytes of messages days keep
	historyDays  int
	// window is the first instant the pass reads history from: midnight
	// UTC, historyDays before today.
	window time.Time
	// today is the UTC day the pass runs on, YYYY-MM-DD. Its days, and any
	// later one a clock ahead of the pass's lists, get rows but no document
	// until they are over: a channel still talking would otherwise replace
	// its document, and have it extracted again, at every sync.
	today string
	// stop is when the pass stops fetching documents; zero for never. late
	// is the earliest day whose document it then wanted but did not fetch.
	stop time.Time
	late string
}

func (p *slackPass) run(ctx context.Context) error {
	history := p.read["messages"] || p.read["channel_days"]
	if p.read["channels"] || p.read["channel_members"] || history {
		if err := p.auth(ctx); err != nil {
			return err
		}
	}
	if p.read["users"] {
		if err := p.users(ctx); err != nil {
			return err
		}
	}
	if p.read["channels"] {
		if err := p.listChannels(ctx); err != nil {
			return err
		}
	}
	if !p.read["channel_members"] && !history {
		return nil
	}
	member, err := p.joined(ctx)
	if err != nil {
		return err
	}
	// A channel the app was removed from, or that was deleted, since it was
	// listed is skipped: the rows of it already written stay, and its days
	// are left out.
	for _, ch := range member {
		if p.read["channel_members"] {
			err := p.c.pages(ctx, "conversations.members", "members", url.Values{"channel": {ch.ID}, "limit": {slackListPage}},
				func(raw json.RawMessage) error {
					var user string
					if json.Unmarshal(raw, &user) != nil || user == "" {
						return fmt.Errorf("slack conversations.members: channel %s lists a member without an id", ch.ID)
					}
					return p.rows["channel_members"].Write(map[string]any{"channel_id": ch.ID, "user_id": user})
				})
			if errors.Is(err, errChannelGone) {
				continue
			}
			if err != nil {
				return err
			}
		}
		// A kind of channel whose history the app may not read is left out;
		// Discover warned of it.
		if !history || !p.scopes.has(slackHistoryScope(ch.IsPrivate)) {
			continue
		}
		if err := p.history(ctx, ch, p.window); err != nil && !errors.Is(err, errChannelGone) {
			return err
		}
	}
	return p.channelDays(ctx)
}

// auth reads the workspace and the token's scopes with auth.test; without
// users:read, documents name people by id. Discover makes a table
// unavailable that no history can be read for; a read of history by a
// token that lost both scopes since is refused with the fix.
func (p *slackPass) auth(ctx context.Context) error {
	auth, err := p.c.auth(ctx)
	if err != nil {
		return err
	}
	p.team, p.scopes, p.byID = auth.team, auth.scopes, !auth.scopes.has("users:read")
	if (p.read["messages"] || p.read["channel_days"]) && !p.scopes.has("channels:history") && !p.scopes.has("groups:history") {
		return refused(errMissingScope, "add the channels:history scope to the app and reinstall it")
	}
	return nil
}

// listChannels reads conversations.list for every channel's name, writing
// the channels table's rows when it is read. A channel the app is in keeps
// the name users.conversations gave it.
func (p *slackPass) listChannels(ctx context.Context) error {
	p.listed = true
	return p.c.pages(ctx, "conversations.list", "channels", url.Values{"limit": {slackListPage}, "types": {p.scopes.types()}},
		func(raw json.RawMessage) error {
			var ch slackChannel
			if json.Unmarshal(raw, &ch) != nil || ch.ID == "" {
				return errors.New("slack conversations.list: a listed channel has no id")
			}
			if _, ok := p.channels[ch.ID]; !ok {
				p.channels[ch.ID] = ch.Name
			}
			if !p.read["channels"] {
				return nil
			}
			return p.rows["channels"].Write(ch.row())
		})
}

// joined lists the channels the app is in with users.conversations, of the
// types the token may list, naming them.
func (p *slackPass) joined(ctx context.Context) ([]slackChannel, error) {
	var member []slackChannel
	err := p.c.pages(ctx, "users.conversations", "channels", url.Values{"limit": {slackListPage}, "types": {p.scopes.types()}},
		func(raw json.RawMessage) error {
			var ch slackChannel
			if json.Unmarshal(raw, &ch) != nil || ch.ID == "" {
				return errors.New("slack users.conversations: a listed channel has no id")
			}
			p.channels[ch.ID] = ch.Name
			member = append(member, ch)
			return nil
		})
	return member, err
}

// history reads ch's messages from since on (message). A channel the app
// was removed from, or that was deleted, since it was listed loses the days
// it read, and the error wraps errChannelGone.
func (p *slackPass) history(ctx context.Context, ch slackChannel, since time.Time) error {
	err := p.c.history(ctx, ch.ID, since, time.Time{}, func(m slackMessage) error { return p.message(ch, m) })
	if errors.Is(err, errChannelGone) {
		for id, d := range p.days {
			if d.channel.ID == ch.ID {
				p.kept -= d.bytes
				delete(p.days, id)
			}
		}
	}
	return err
}

// channelDays writes the row of each day the pass read, when channel_days
// is read, and offers its document once the day is over. A change read
// first lists the channels, for their names, when a day mentions one it
// can't name.
func (p *slackPass) channelDays(ctx context.Context) error {
	if !p.read["channel_days"] {
		return nil
	}
	// The syncer keeps the first documents offered up to its cap, so the
	// most recently active come first.
	days := slices.SortedFunc(maps.Values(p.days), func(a, b *slackDay) int {
		return cmp.Or(b.active.Compare(a.active), strings.Compare(a.id(), b.id()))
	})
	for _, d := range days {
		if err := p.rows["channel_days"].Write(d.row()); err != nil {
			return err
		}
		if d.day >= p.today {
			continue
		}
		if p.nameChannels && !p.listed && slices.ContainsFunc(slices.Collect(maps.Keys(d.mentioned)), func(id string) bool {
			_, named := p.channels[id]
			return !named
		}) {
			if err := p.listChannels(ctx); err != nil && !errors.Is(err, errMissingScope) {
				return err
			}
		}
		version, named := p.version(d)
		if !named {
			// Until a document needs users.list, a day the stream holds with
			// these messages keeps the names it was written with, unless
			// they were ids. Another is offered at its version without
			// names, which no document has: Want answers the same for any
			// version but the one held, and its Put carries the whole one.
			unnamed := strings.TrimSuffix(version, "?")
			if held, ok := p.docs.Held(slackConversations.Name, d.id()); ok && strings.HasPrefix(held, unnamed) && held != unnamed+"ids" {
				version = held
			}
		}
		if !p.docs.Want(slackConversations.Name, d.id(), version) {
			continue
		}
		if !p.stop.IsZero() && !time.Now().Before(p.stop) {
			if p.late == "" || d.day < p.late {
				p.late = d.day
			}
			continue
		}
		if !named {
			// Documents name people by id when the app may not read users.
			err := p.users(ctx)
			switch {
			case errors.Is(err, errMissingScope):
				p.people, p.byID = nil, true
			case err != nil:
				return err
			}
			version, _ = p.version(d)
		}
		text, ok, err := p.document(ctx, d)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		doc := Document{ExternalID: d.id(), Version: version, At: d.active, Title: "#" + d.channel.Name + ", " + d.day,
			URL: "https://app.slack.com/client/" + p.team + "/" + d.channel.ID, Text: text}
		if err := p.docs.Put(ctx, slackConversations.Name, doc); err != nil {
			return err
		}
	}
	return nil
}

// users reads users.list for the names documents give people, writing the
// users table's rows when it is read.
func (p *slackPass) users(ctx context.Context) error {
	p.people = map[string]string{}
	return p.c.pages(ctx, "users.list", "members", url.Values{"limit": {slackListPage}}, func(raw json.RawMessage) error {
		var u slackUser
		if json.Unmarshal(raw, &u) != nil || u.ID == "" {
			return errors.New("slack users.list: a listed user has no id")
		}
		p.people[u.ID] = cmp.Or(u.RealName, u.Profile.RealName, u.Name, u.ID)
		if !p.read["users"] {
			return nil
		}
		return p.rows["users"].Write(u.row())
	})
}

// message writes a listed message's row and counts it into its channel
// day, keeping it for the day's document while the pass keeps messages
// and the day's are not dropped. When the kept messages pass
// slackTextBudget, the least recently active days' are dropped down to
// three quarters of it, so the drop's sort is rare.
func (p *slackPass) message(ch slackChannel, m slackMessage) error {
	if p.read["messages"] {
		if err := p.rows["messages"].Write(m.row(ch.ID)); err != nil {
			return err
		}
	}
	if !p.read["channel_days"] {
		return nil
	}
	day := m.at.Format(time.DateOnly)
	d := p.days[ch.ID+":"+day]
	if d == nil {
		d = &slackDay{channel: ch, day: day, first: m.at, last: m.at, active: m.at, users: map[string]bool{}, mentioned: map[string]bool{},
			dropped: !p.keep || day >= p.today}
		p.days[d.id()] = d
	}
	d.count++
	if m.at.Before(d.first) {
		d.first = m.at
	}
	if m.at.After(d.last) {
		d.last = m.at
	}
	// A thread's replies make its parent's day as recent as the latest of them.
	active := m.at
	if latest, ok := slackTime(m.LatestReply); ok && latest.After(active) {
		active = latest
	}
	if active.After(d.active) {
		d.active = active
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{m.TS, m.Edited.TS, m.Text, strconv.Itoa(m.ReplyCount), m.LatestReply,
		m.ThreadTS, m.Subtype, m.User, m.BotID, m.Username, m.BotProfile.Name}, "\x00")))
	for i := range d.sum {
		d.sum[i] ^= digest[i]
	}
	// The names its line takes from users.list and the channel lists.
	if m.User != "" {
		d.users[m.User] = true
	}
	for _, span := range slackTokenSpan.FindAllStringSubmatch(m.Text, -1) {
		switch target := slackUnescape.Replace(span[1]); {
		case strings.HasPrefix(target, "@"):
			d.users[target[1:]] = true
		case strings.HasPrefix(target, "#") && span[2] == "":
			d.mentioned[target[1:]] = true
		}
	}
	if d.dropped {
		return nil
	}
	if p.kept += d.keep(m); p.kept <= slackTextBudget {
		return nil
	}
	kept := slices.SortedFunc(maps.Values(p.days), func(a, b *slackDay) int { return b.active.Compare(a.active) })
	kept = slices.DeleteFunc(kept, func(d *slackDay) bool { return d.dropped })
	for ; p.kept > slackTextBudget*3/4; kept = kept[:len(kept)-1] {
		oldest := kept[len(kept)-1]
		p.kept -= oldest.bytes
		oldest.messages, oldest.bytes, oldest.cut, oldest.dropped = nil, 0, 0, true
	}
	return nil
}

// document is the text of a channel day: a heading and the channel, then
// each message in time order with the replies of the thread it started,
// read now, beneath it. A thread broadcast shows in its thread when the
// day holds the thread's parent, else on its own. A day whose messages
// were dropped is read again; ok is false when none are left, or its
// channel is gone since it was listed. A thread gone since has no replies.
// Once the text is over slackDayBytes, no more is read or written, since
// the syncer cuts it; a day that kept only its earliest messages and ends
// below it says how many it left out.
func (p *slackPass) document(ctx context.Context, d *slackDay) (_ []byte, ok bool, _ error) {
	messages, cut := d.messages, d.cut
	if d.dropped {
		var again slackDay
		start := d.first.Truncate(24 * time.Hour)
		err := p.c.history(ctx, d.channel.ID, start, start.Add(24*time.Hour-time.Microsecond), func(m slackMessage) error {
			again.keep(m)
			return nil
		})
		switch {
		case errors.Is(err, errChannelGone):
			return nil, false, nil
		case err != nil || len(again.messages) == 0:
			return nil, false, err
		}
		messages, cut = again.messages, again.cut
	}
	slices.SortFunc(messages, func(a, b slackMessage) int { return cmp.Or(a.at.Compare(b.at), strings.Compare(a.TS, b.TS)) })
	parents := map[string]bool{}
	for _, m := range messages {
		parents[m.TS] = true
	}
	var text strings.Builder
	fmt.Fprintf(&text, "# #%s, %s\n\nChannel: #%s (%s)\n", d.channel.Name, d.day, d.channel.Name, d.channel.ID)
	for _, m := range messages {
		if text.Len() > slackDayBytes {
			return []byte(text.String()), true, nil
		}
		if m.Subtype == "thread_broadcast" && m.ThreadTS != m.TS && parents[m.ThreadTS] {
			continue
		}
		text.WriteString("\n")
		p.line(&text, "", "", m, d.day)
		if m.ReplyCount == 0 || text.Len() > slackDayBytes {
			continue
		}
		replies, err := p.c.replies(ctx, d.channel.ID, m.TS, slackDayBytes-text.Len())
		switch {
		case errors.Is(err, errChannelGone):
			return nil, false, nil
		case errors.Is(err, errThreadGone):
			replies = nil
		case err != nil:
			return nil, false, err
		}
		for _, r := range replies {
			p.line(&text, "  - ", "    ", r, d.day)
		}
	}
	if cut > 0 && text.Len() <= slackDayBytes {
		fmt.Fprintf(&text, "\n[Left out: %d later messages of this day]\n", cut)
	}
	return []byte(text.String()), true, nil
}

// line writes one message as "**Name** HH:MM: text" after bullet, its
// date before the time when it was sent on another day than day, and its
// text's later lines after indent.
func (p *slackPass) line(b *strings.Builder, bullet, indent string, m slackMessage, day string) {
	when := m.at.Format("15:04")
	if m.at.Format(time.DateOnly) != day {
		when = m.at.Format("2006-01-02 15:04")
	}
	who := cmp.Or(m.BotProfile.Name, m.Username, m.BotID, "Slack")
	if m.User != "" {
		who = cmp.Or(p.people[m.User], m.User)
	}
	b.WriteString(bullet + "**" + who + "** " + when + ":")
	if said := strings.TrimSpace(p.mrkdwn(m.Text)); said != "" {
		b.WriteString(" " + strings.ReplaceAll(said, "\n", "\n"+indent))
	}
	b.WriteString("\n")
}

// slackTokenSpan is a token of Slack's mrkdwn: <target> or <target|label>.
var slackTokenSpan = regexp.MustCompile(`<([^<>|]+)(?:\|([^<>]*))?>`)

// slackUnescape undoes the three escapes Slack's text uses, in one pass.
var slackUnescape = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")

// mrkdwn writes Slack's mrkdwn tokens as a reader would see them: a user
// as @ and their name, a channel as # and its name, a special mention
// (<!here>, <!channel>, <!everyone>) as @here and so on, or its label, and
// a link as a Markdown link, or its URL without a label.
func (p *slackPass) mrkdwn(text string) string {
	var out strings.Builder
	last := 0
	for _, m := range slackTokenSpan.FindAllStringSubmatchIndex(text, -1) {
		out.WriteString(slackUnescape.Replace(text[last:m[0]]))
		last = m[1]
		target, label := slackUnescape.Replace(text[m[2]:m[3]]), ""
		if m[4] >= 0 {
			label = slackUnescape.Replace(text[m[4]:m[5]])
		}
		switch {
		case strings.HasPrefix(target, "@"):
			out.WriteString("@" + cmp.Or(p.people[target[1:]], label, target[1:]))
		case strings.HasPrefix(target, "#"):
			out.WriteString("#" + cmp.Or(label, p.channels[target[1:]], target[1:]))
		case strings.HasPrefix(target, "!"):
			keyword, _, _ := strings.Cut(target[1:], "^")
			out.WriteString(cmp.Or(label, "@"+keyword))
		case label != "":
			out.WriteString("[" + label + "](" + target + ")")
		default:
			out.WriteString(target)
		}
	}
	out.WriteString(slackUnescape.Replace(text[last:]))
	return out.String()
}

// slackDay is one channel's messages on one UTC day of the window, as
// conversations.history lists them: their count, first and last times, the
// XOR of each one's digest, and the messages themselves unless dropped.
type slackDay struct {
	channel     slackChannel
	day         string // YYYY-MM-DD
	count       int
	first, last time.Time
	// active is the day's latest activity: its last message, or the latest
	// reply to a thread one of them started, when later. It is its
	// document's time and orders the stream.
	active time.Time
	sum    [sha256.Size]byte
	// users are the users its messages name, as authors and @mentions, and
	// mentioned the channels they mention without a label.
	users, mentioned map[string]bool
	messages         []slackMessage
	bytes            int
	// cut counts the latest messages left out of messages, past slackDayBytes.
	cut int
	// dropped is a day whose messages are not kept, to be read again if its
	// document is wanted.
	dropped bool
}

// keep adds m, listed after every message d keeps since history pages come
// newest first, and leaves out the latest while the rest are over
// slackDayBytes: a day keeps its earliest messages, a little more than one
// document holds. It returns how many bytes d's messages grew by.
func (d *slackDay) keep(m slackMessage) int {
	before := d.bytes
	d.messages = append(d.messages, m)
	d.bytes += len(m.Text) + slackMessageOverhead
	for len(d.messages) > 1 {
		latest := len(d.messages[0].Text) + slackMessageOverhead
		if d.bytes-latest <= slackDayBytes {
			break
		}
		d.messages, d.bytes, d.cut = d.messages[1:], d.bytes-latest, d.cut+1
	}
	return d.bytes - before
}

func (d *slackDay) id() string { return d.channel.ID + ":" + d.day }

// version is d's document's version, "2|<content>|<people>": a render
// revision, to bump whenever its text changes for the same messages and
// names; 40 hex digits of a SHA-256 of the channel's name, what
// conversations.history says of each message (its ts, edit, text, thread,
// reply count, latest reply, subtype and author) and the names of the
// channels they mention without a label; then "ids" when people are named
// by id, else 16 hex digits of a SHA-256 of the name of each user they
// name. It changes when a message is posted, edited, deleted or replied
// to, or a name its messages render changes. The messages' digests
// combine by XOR, so the order Slack lists them in doesn't count. A
// reply's edit, or the name of who wrote it, changes none of it. named is
// false while users.list is unread and d names users: people is then "?".
func (p *slackPass) version(d *slackDay) (_ string, named bool) {
	content := sha256.New()
	content.Write([]byte(d.channel.Name + "\x00"))
	content.Write(d.sum[:])
	for _, id := range slices.Sorted(maps.Keys(d.mentioned)) {
		content.Write([]byte(id + "\x00" + p.channels[id] + "\x00"))
	}
	version := "2|" + hex.EncodeToString(content.Sum(nil)[:20]) + "|"
	switch {
	case p.byID:
		return version + "ids", true
	case p.people == nil && len(d.users) > 0:
		return version + "?", false
	}
	people := sha256.New()
	for _, id := range slices.Sorted(maps.Keys(d.users)) {
		people.Write([]byte(id + "\x00" + p.people[id] + "\x00"))
	}
	return version + hex.EncodeToString(people.Sum(nil)[:8]), true
}

func (d *slackDay) row() map[string]any {
	return map[string]any{
		"channel_day_id": d.id(), "channel_id": d.channel.ID, "day": d.day, "message_count": d.count,
		"first_message_at": d.first.Format(time.RFC3339), "last_message_at": d.last.Format(time.RFC3339),
	}
}

// slackUser is a member of users.list.
type slackUser struct {
	ID       string `json:"id"`
	TeamID   string `json:"team_id"`
	Name     string `json:"name"`
	RealName string `json:"real_name"`
	TZ       string `json:"tz"`
	Deleted  bool   `json:"deleted"`
	IsBot    bool   `json:"is_bot"`
	IsAdmin  bool   `json:"is_admin"`
	IsOwner  bool   `json:"is_owner"`
	Updated  int64  `json:"updated"`
	Profile  struct {
		RealName    string `json:"real_name"`
		DisplayName string `json:"display_name"`
		Email       string `json:"email"` // only with users:read.email
		Title       string `json:"title"`
	} `json:"profile"`
}

func (u slackUser) row() map[string]any {
	row := map[string]any{
		"user_id": u.ID, "team_id": orNull(u.TeamID), "name": orNull(u.Name), "real_name": orNull(cmp.Or(u.RealName, u.Profile.RealName)),
		"display_name": orNull(u.Profile.DisplayName), "email": orNull(u.Profile.Email), "title": orNull(u.Profile.Title),
		"is_bot": u.IsBot, "is_admin": u.IsAdmin, "is_owner": u.IsOwner, "deleted": u.Deleted, "tz": orNull(u.TZ), "updated_at": nil,
	}
	if u.Updated > 0 {
		row["updated_at"] = time.Unix(u.Updated, 0).UTC().Format(time.RFC3339)
	}
	return row
}

// slackChannel is a channel of conversations.list or users.conversations.
type slackChannel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsPrivate  bool   `json:"is_private"`
	IsArchived bool   `json:"is_archived"`
	IsMember   bool   `json:"is_member"`
	Creator    string `json:"creator"`
	Created    int64  `json:"created"`
	NumMembers *int   `json:"num_members"`
	Topic      struct {
		Value string `json:"value"`
	} `json:"topic"`
	Purpose struct {
		Value string `json:"value"`
	} `json:"purpose"`
}

func (ch slackChannel) row() map[string]any {
	row := map[string]any{
		"channel_id": ch.ID, "name": orNull(ch.Name), "is_private": ch.IsPrivate, "is_archived": ch.IsArchived, "is_member": ch.IsMember,
		"topic": orNull(ch.Topic.Value), "purpose": orNull(ch.Purpose.Value), "creator_id": orNull(ch.Creator),
		"created_at": nil, "num_members": nil,
	}
	if ch.Created > 0 {
		row["created_at"] = time.Unix(ch.Created, 0).UTC().Format(time.RFC3339)
	}
	if ch.NumMembers != nil {
		row["num_members"] = *ch.NumMembers
	}
	return row
}

// slackMessage is a message of conversations.history or conversations.replies.
type slackMessage struct {
	TS         string `json:"ts"`
	ThreadTS   string `json:"thread_ts"`
	User       string `json:"user"`
	BotID      string `json:"bot_id"`
	Username   string `json:"username"`
	BotProfile struct {
		Name string `json:"name"`
	} `json:"bot_profile"`
	Subtype string `json:"subtype"`
	Text    string `json:"text"`
	Edited  struct {
		TS string `json:"ts"`
	} `json:"edited"`
	ReplyCount      int    `json:"reply_count"`
	ReplyUsersCount int    `json:"reply_users_count"`
	LatestReply     string `json:"latest_reply"`
	at              time.Time
}

func (m slackMessage) row(channel string) map[string]any {
	day := m.at.Format(time.DateOnly)
	return map[string]any{
		"message_id": channel + ":" + m.TS, "channel_id": channel, "day": day, "channel_day_id": channel + ":" + day,
		"ts": m.TS, "thread_ts": orNull(m.ThreadTS), "user_id": orNull(m.User), "bot_id": orNull(m.BotID), "subtype": orNull(m.Subtype), "text": m.Text,
		"sent_at": m.at.Format(time.RFC3339), "edited_at": slackStamp(m.Edited.TS), "reply_count": m.ReplyCount,
		"reply_users_count": m.ReplyUsersCount, "latest_reply_at": slackStamp(m.LatestReply),
	}
}

// slackTime reads a Slack ts, seconds and microseconds since the epoch
// ("1700000000.000100"); ok is false for anything else.
func slackTime(ts string) (_ time.Time, ok bool) {
	seconds, micros, found := strings.Cut(ts, ".")
	s, errSeconds := strconv.ParseInt(seconds, 10, 64)
	u, errMicros := strconv.ParseInt(micros, 10, 64)
	if !found || errSeconds != nil || errMicros != nil || s < 0 || u < 0 || len(micros) != 6 {
		return time.Time{}, false
	}
	return time.Unix(s, u*1000).UTC(), true
}

// slackStamp is a ts as an RFC 3339 time, or null for none.
func slackStamp(ts string) any {
	t, ok := slackTime(ts)
	if !ok {
		return nil
	}
	return t.Format(time.RFC3339)
}

// orNull is s, or null when the source gave it empty or left it out.
func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// slackClient is one read's calls, each method paced under its own limit.
type slackClient struct {
	k     *slackKind
	token string
	pace  map[string]*rate.Limiter
}

func (k *slackKind) client(secret string) *slackClient {
	pace := map[string]*rate.Limiter{}
	for method, interval := range slackIntervals {
		pace[method] = rate.NewLimiter(rate.Every(interval), 1)
	}
	return &slackClient{k: k, token: strings.TrimSpace(secret), pace: pace}
}

// history lists a channel's messages from oldest to latest (zero for
// none), newest first, as conversations.history pages them, and passes
// each to each, but those slackSkipped names. A message without a valid ts
// fails the read, since it keys the message's row.
func (c *slackClient) history(ctx context.Context, channel string, oldest, latest time.Time, each func(slackMessage) error) error {
	ts := func(t time.Time) string { return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000) }
	query := url.Values{"channel": {channel}, "oldest": {ts(oldest)}, "inclusive": {"true"}, "limit": {slackHistoryPage}}
	if !latest.IsZero() {
		query.Set("latest", ts(latest))
	}
	return c.pages(ctx, "conversations.history", "messages", query, func(raw json.RawMessage) error {
		var m slackMessage
		var ok bool
		if json.Unmarshal(raw, &m) == nil {
			m.at, ok = slackTime(m.TS)
		}
		switch {
		case !ok:
			return fmt.Errorf("slack conversations.history: channel %s lists a message without a valid ts", channel)
		case slackSkipped[m.Subtype], m.at.Before(oldest), !latest.IsZero() && m.at.After(latest):
			return nil
		}
		return each(m)
	})
}

// replies lists the replies of the thread parent starts, in time order,
// but those slackSkipped names; conversations.replies lists the parent
// first, then the replies oldest first. It stops once they are over room bytes.
func (c *slackClient) replies(ctx context.Context, channel, parent string, room int) ([]slackMessage, error) {
	var (
		replies []slackMessage
		size    int
	)
	err := c.pages(ctx, "conversations.replies", "messages", url.Values{"channel": {channel}, "ts": {parent}, "limit": {slackHistoryPage}},
		func(raw json.RawMessage) error {
			var m slackMessage
			var ok bool
			if json.Unmarshal(raw, &m) == nil {
				m.at, ok = slackTime(m.TS)
			}
			switch {
			case !ok:
				return fmt.Errorf("slack conversations.replies: thread %s in channel %s lists a reply without a valid ts", parent, channel)
			case m.TS != parent && !slackSkipped[m.Subtype]:
				replies = append(replies, m)
				if size += len(m.Text) + slackMessageOverhead; size > room {
					return errStopPaging
				}
			}
			return nil
		})
	slices.SortFunc(replies, func(a, b slackMessage) int { return cmp.Or(a.at.Compare(b.at), strings.Compare(a.TS, b.TS)) })
	return replies, err
}

// pages calls a Slack list method a page at a time, following
// response_metadata.next_cursor, and passes each item of field to each,
// until each returns errStopPaging. An incomplete page fails the attempt:
// ending early would publish a partial table.
func (c *slackClient) pages(ctx context.Context, method, field string, query url.Values, each func(json.RawMessage) error) error {
	q := url.Values{}
	maps.Copy(q, query)
	for page := 0; ; page++ {
		if page == maxSlackPages {
			return fmt.Errorf("slack %s: more than %d pages", method, maxSlackPages)
		}
		fields, _, err := c.call(ctx, method, q)
		if err != nil {
			return err
		}
		var (
			items   []json.RawMessage
			hasMore bool
			meta    struct {
				NextCursor string `json:"next_cursor"`
			}
		)
		_ = json.Unmarshal(fields["has_more"], &hasMore)       // only history and replies say
		_ = json.Unmarshal(fields["response_metadata"], &meta) // a last page may leave it out
		switch {
		case json.Unmarshal(fields[field], &items) != nil || items == nil:
			return fmt.Errorf("slack %s: the response has no complete page of %s", method, field)
		case hasMore && meta.NextCursor == "" || meta.NextCursor != "" && meta.NextCursor == q.Get("cursor"):
			return fmt.Errorf("slack %s: a page says more follow but gives no new cursor", method)
		}
		for _, item := range items {
			if err := each(item); errors.Is(err, errStopPaging) {
				return nil
			} else if err != nil {
				return err
			}
		}
		if meta.NextCursor == "" {
			return nil
		}
		q.Set("cursor", meta.NextCursor)
	}
}

// call sends one GET of a Web API method, paced and retried by fetch, and
// returns its answer's fields once Slack says ok. A rate limit that
// outlasts the retries is refused with when to try again, so the sync
// retries rather than failing for good; so is the ratelimited error Slack
// may answer with instead. A method the app has no scope for is refused
// with the scope to add, wrapping errMissingScope, and a channel or thread
// gone since it was listed wraps errChannelGone or errThreadGone.
func (c *slackClient) call(ctx context.Context, method string, query url.Values) (map[string]json.RawMessage, http.Header, error) {
	target := c.k.base + "/" + method
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	status, header, body, err := fetchWithHeader(ctx, c.k.http, func() (*http.Request, error) {
		if err := c.pace[method].Wait(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err == nil {
			req.Header = http.Header{"Authorization": {"Bearer " + c.token}}
		}
		return req, err
	})
	if err != nil {
		return nil, nil, refused(err, "could not reach Slack; try again")
	}
	var (
		fields map[string]json.RawMessage
		answer struct {
			OK     bool   `json:"ok"`
			Error  string `json:"error"`
			Needed string `json:"needed"`
		}
	)
	decodeErr := json.Unmarshal(body, &fields)
	_ = json.Unmarshal(body, &answer)
	switch {
	case status == http.StatusTooManyRequests || answer.Error == "ratelimited":
		when := "in a few minutes"
		if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil && seconds > 0 {
			when = "after " + time.Now().Add(time.Duration(seconds)*time.Second).UTC().Format("15:04 UTC")
		}
		return nil, nil, refused(nil, "this token has used up its Slack rate limit; try again %s", when)
	case status != http.StatusOK:
		return nil, nil, refused(nil, "Slack returned HTTP %d for %s", status, method)
	case decodeErr != nil:
		return nil, nil, fmt.Errorf("decode slack %s: %w", method, decodeErr)
	case answer.OK:
		return fields, header, nil
	case slackRejected[answer.Error]:
		return nil, nil, refused(nil, "Slack rejected this token; check it and enter it again")
	case answer.Error == "missing_scope":
		return nil, nil, refused(errMissingScope, "add the %s scope to the app and reinstall it", cmp.Or(answer.Needed, "missing"))
	case slackGone[answer.Error] != nil:
		return nil, nil, refused(slackGone[answer.Error], "Slack returned %s for %s", answer.Error, method)
	}
	return nil, nil, refused(nil, "Slack returned %s for %s", cmp.Or(answer.Error, "an error"), method)
}

// auth proves the token with auth.test.
func (c *slackClient) auth(ctx context.Context) (slackAuth, error) {
	fields, header, err := c.call(ctx, "auth.test", nil)
	if err != nil {
		return slackAuth{}, err
	}
	var a slackAuth
	_ = json.Unmarshal(fields["team_id"], &a.team) // for the documents' links
	_ = json.Unmarshal(fields["user"], &a.bot)     // for the invite warning
	if listed, ok := header[http.CanonicalHeaderKey("X-OAuth-Scopes")]; ok {
		a.scopes = slackScopes{}
		for scope := range strings.SplitSeq(strings.Join(listed, ","), ",") {
			if scope = strings.TrimSpace(scope); scope != "" {
				a.scopes[scope] = true
			}
		}
	}
	return a, nil
}
