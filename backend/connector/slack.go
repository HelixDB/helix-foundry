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
	// slackChannelTypes are the conversations read: channels, never direct messages.
	slackChannelTypes = "public_channel,private_channel"
	// slackMessageOverhead is what a kept message costs beyond its text.
	slackMessageOverhead = 256
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

// slackNow is the clock the history window is counted from. Tests set it.
var slackNow = time.Now

var slackObjects = []saasTable{
	{"users", "user_id"},
	{"channels", "channel_id"},
	// Discover keys a channel's members by (channel_id, user_id).
	{"channel_members", "channel_id"},
	{"messages", "message_id"},
	{"channel_days", "channel_day_id"},
}

var slackForeignKeys = map[string][]ForeignKey{
	"channels":        {foreignKey("creator_id", "users", "user_id")},
	"channel_members": {foreignKey("channel_id", "channels", "channel_id"), foreignKey("user_id", "users", "user_id")},
	"messages":        {foreignKey("channel_id", "channels", "channel_id"), foreignKey("user_id", "users", "user_id")},
	"channel_days":    {foreignKey("channel_id", "channels", "channel_id")},
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

// errMissingScope is a method the app has no scope for; the RefusedError
// wrapping it says which scope to add.
var errMissingScope = errors.New("slack: missing_scope")

// slackConversations is the channel_days table's stream: each channel day's messages.
var slackConversations = Stream{Name: "conversations", Table: "channel_days", Title: "Conversations", Format: Markdown,
	Description: "Each day of one Slack channel the app is in, as Markdown: a heading with the channel and the UTC day, " +
		"the channel's name and id, then each message posted that day in time order as \"**Name** HH:MM: text\", in UTC, " +
		"with the replies of a thread it started listed beneath it, dated when sent on a later day. People are named " +
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

// Discover proves the token with auth.test, then probes each object with a
// one-item read: users.list for users, conversations.list for channels,
// and conversations.history of a channel the app is in for its members,
// messages and days, which also need channels. A method the app has no
// scope for makes its objects unavailable, saying which scope to add. An
// app in no channel yet lists them with a warning, since only an invite
// shows what it may read.
func (k *slackKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	c := k.client(src.Secret)
	auth, err := c.call(ctx, "auth.test", nil)
	if err != nil {
		return Discovery{}, err
	}
	var bot string
	_ = json.Unmarshal(auth["user"], &bot) // the bot user's handle, for the warning
	var (
		mu      sync.Mutex
		reasons = map[string]string{}
		alone   bool
	)
	readable, err := probeAll(ctx, []string{"users", "channels", "messages"}, func(ctx context.Context, probe string) (bool, error) {
		var err error
		switch probe {
		case "users":
			_, err = c.call(ctx, "users.list", url.Values{"limit": {"1"}})
		case "channels":
			_, err = c.call(ctx, "conversations.list", url.Values{"limit": {"1"}, "types": {slackChannelTypes}})
		default:
			// A filtered page may be empty though more follow, so one is paged for.
			var channel string
			err = c.pages(ctx, "users.conversations", "channels", url.Values{"limit": {slackListPage}, "types": {slackChannelTypes}},
				func(raw json.RawMessage) error {
					var ch slackChannel
					if json.Unmarshal(raw, &ch) != nil || ch.ID == "" {
						return errors.New("slack users.conversations: a listed channel has no id")
					}
					channel = ch.ID
					return errStopPaging
				})
			if err == nil && channel != "" {
				_, err = c.call(ctx, "conversations.history", url.Values{"channel": {channel}, "limit": {"1"}})
			}
			mu.Lock()
			alone = err == nil && channel == ""
			mu.Unlock()
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
	d := saasDiscovery(slackObjects, func(name string) bool {
		if name == "users" || name == "channels" {
			return readable[name]
		}
		return readable["channels"] && readable["messages"]
	}, slackForeignKeys)
	if len(d.Tables) == 0 {
		return Discovery{}, refused(nil, "this token cannot read Slack users or channels; add the app's scopes and test again")
	}
	for i, u := range d.Unavailable {
		d.Unavailable[i].Reason = reasons[u.Name]
		if u.Name != "users" && u.Name != "channels" {
			d.Unavailable[i].Reason = cmp.Or(reasons["messages"], reasons["channels"])
		}
	}
	for i, t := range d.Tables {
		if t.Name == "channel_members" {
			d.Tables[i].PrimaryKey = []string{"channel_id", "user_id"}
		}
		d.Tables[i].Windowed = WindowedTable("slack", t.Name)
	}
	if alone && readable["channels"] {
		d.Warnings = append(d.Warnings, "The app is in no channels yet: invite it to each channel to sync with /invite @"+bot+", then test again")
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
// history_days UTC days before today and today, with one
// conversations.history page per 999 messages. The messages make the
// channel_days rows and, when that table is read, its documents, offered
// newest first once every channel is read; a day's thread replies are read
// only when its document is wanted. Users are read whenever documents are,
// to name people.
func (k *slackKind) SyncPass(ctx context.Context, src Source, tables []Table, rows map[string]RowWriter, docs DocumentWriter) ([]Table, error) {
	var s SlackSettings
	_ = json.Unmarshal(orEmpty(src.Settings), &s) // stored by Prepare
	p := &slackPass{c: k.client(src.Secret), read: map[string]bool{}, rows: rows, docs: docs,
		people: map[string]string{}, channels: map[string]string{}, days: map[string]*slackDay{}}
	for _, table := range tables {
		if !slices.ContainsFunc(slackObjects, func(o saasTable) bool { return o.name == table.Name }) {
			return tables, invalid("Slack has no object %q", table.Name)
		}
		p.read[table.Name] = true
	}
	p.keep = p.read["channel_days"] && !docs.Paused(slackConversations.Name)
	return tables, p.run(ctx, cmp.Or(s.HistoryDays, defaultSlackHistoryDays))
}

// slackPass is one SyncPass: the tables it reads, the names its documents
// give people and channels, and each channel day of the window.
type slackPass struct {
	c    *slackClient
	read map[string]bool
	rows map[string]RowWriter
	docs DocumentWriter
	// keep is set when the pass keeps messages for documents: their table
	// is read and their stream is not paused.
	keep     bool
	team     string
	people   map[string]string // user id: real name, else handle
	channels map[string]string // channel id: name
	days     map[string]*slackDay
	kept     int // the bytes of messages days keep
}

func (p *slackPass) run(ctx context.Context, historyDays int) error {
	c := p.c
	if p.read["channel_days"] {
		auth, err := c.call(ctx, "auth.test", nil)
		if err != nil {
			return err
		}
		_ = json.Unmarshal(auth["team_id"], &p.team) // for the documents' links
	}
	if p.read["users"] || p.keep {
		err := c.pages(ctx, "users.list", "members", url.Values{"limit": {slackListPage}}, func(raw json.RawMessage) error {
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
		// Documents name people by id when the app may not read users.
		if err != nil && (p.read["users"] || !errors.Is(err, errMissingScope)) {
			return err
		}
	}
	if p.read["channels"] {
		err := c.pages(ctx, "conversations.list", "channels", url.Values{"limit": {slackListPage}, "types": {slackChannelTypes}},
			func(raw json.RawMessage) error {
				var ch slackChannel
				if json.Unmarshal(raw, &ch) != nil || ch.ID == "" {
					return errors.New("slack conversations.list: a listed channel has no id")
				}
				p.channels[ch.ID] = ch.Name
				return p.rows["channels"].Write(ch.row())
			})
		if err != nil {
			return err
		}
	}
	if !p.read["channel_members"] && !p.read["messages"] && !p.read["channel_days"] {
		return nil
	}
	var member []slackChannel
	err := c.pages(ctx, "users.conversations", "channels", url.Values{"limit": {slackListPage}, "types": {slackChannelTypes}},
		func(raw json.RawMessage) error {
			var ch slackChannel
			if json.Unmarshal(raw, &ch) != nil || ch.ID == "" {
				return errors.New("slack users.conversations: a listed channel has no id")
			}
			p.channels[ch.ID] = ch.Name
			member = append(member, ch)
			return nil
		})
	if err != nil {
		return err
	}
	since := slackNow().UTC().Truncate(24*time.Hour).AddDate(0, 0, -historyDays)
	for _, ch := range member {
		if p.read["channel_members"] {
			err := c.pages(ctx, "conversations.members", "members", url.Values{"channel": {ch.ID}, "limit": {slackListPage}},
				func(raw json.RawMessage) error {
					var user string
					if json.Unmarshal(raw, &user) != nil || user == "" {
						return fmt.Errorf("slack conversations.members: channel %s lists a member without an id", ch.ID)
					}
					return p.rows["channel_members"].Write(map[string]any{"channel_id": ch.ID, "user_id": user})
				})
			if err != nil {
				return err
			}
		}
		if p.read["messages"] || p.read["channel_days"] {
			if err := c.history(ctx, ch.ID, since, time.Time{}, func(m slackMessage) error { return p.message(ch, m) }); err != nil {
				return err
			}
		}
	}
	if !p.read["channel_days"] {
		return nil
	}
	// The syncer keeps the first documents offered up to its cap, so the newest come first.
	days := slices.SortedFunc(maps.Values(p.days), func(a, b *slackDay) int {
		return cmp.Or(b.last.Compare(a.last), strings.Compare(a.id(), b.id()))
	})
	for _, d := range days {
		if err := p.rows["channel_days"].Write(d.row()); err != nil {
			return err
		}
		version := d.version()
		if !p.docs.Want(slackConversations.Name, d.id(), version) {
			continue
		}
		text, ok, err := p.document(ctx, d)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		doc := Document{ExternalID: d.id(), Version: version, At: d.last, Title: "#" + d.channel.Name + ", " + d.day,
			URL: "https://app.slack.com/client/" + p.team + "/" + d.channel.ID, Text: text}
		if err := p.docs.Put(ctx, slackConversations.Name, doc); err != nil {
			return err
		}
	}
	return nil
}

// message writes a listed message's row and counts it into its channel
// day, keeping it for the day's document while the pass keeps messages
// and the day's are not dropped. When the kept messages pass
// slackTextBudget, the oldest days' are dropped down to three quarters of
// it, so the drop's sort is rare.
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
		d = &slackDay{channel: ch, day: day, first: m.at, last: m.at, dropped: !p.keep}
		p.days[d.id()] = d
	}
	d.count++
	if m.at.Before(d.first) {
		d.first = m.at
	}
	if m.at.After(d.last) {
		d.last = m.at
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{m.TS, m.Edited.TS, m.Text, strconv.Itoa(m.ReplyCount), m.LatestReply}, "\x00")))
	for i := range d.sum {
		d.sum[i] ^= digest[i]
	}
	if d.dropped {
		return nil
	}
	d.messages = append(d.messages, m)
	d.bytes += len(m.Text) + slackMessageOverhead
	p.kept += len(m.Text) + slackMessageOverhead
	if p.kept <= slackTextBudget {
		return nil
	}
	kept := slices.SortedFunc(maps.Values(p.days), func(a, b *slackDay) int { return b.last.Compare(a.last) })
	kept = slices.DeleteFunc(kept, func(d *slackDay) bool { return d.dropped })
	for ; p.kept > slackTextBudget*3/4; kept = kept[:len(kept)-1] {
		oldest := kept[len(kept)-1]
		p.kept -= oldest.bytes
		oldest.messages, oldest.bytes, oldest.dropped = nil, 0, true
	}
	return nil
}

// document is the text of a channel day: a heading and the channel, then
// each message in time order with the replies of the thread it started,
// read now, beneath it. A thread broadcast shows in its thread when the
// day holds the thread's parent, else on its own. A day whose messages
// were dropped is read again; ok is false when none are left.
func (p *slackPass) document(ctx context.Context, d *slackDay) (_ []byte, ok bool, _ error) {
	messages := d.messages
	if d.dropped {
		start := d.first.Truncate(24 * time.Hour)
		err := p.c.history(ctx, d.channel.ID, start, start.Add(24*time.Hour-time.Microsecond), func(m slackMessage) error {
			messages = append(messages, m)
			return nil
		})
		if err != nil || len(messages) == 0 {
			return nil, false, err
		}
	}
	slices.SortFunc(messages, func(a, b slackMessage) int { return cmp.Or(a.at.Compare(b.at), strings.Compare(a.TS, b.TS)) })
	parents := map[string]bool{}
	for _, m := range messages {
		parents[m.TS] = true
	}
	var text strings.Builder
	fmt.Fprintf(&text, "# #%s, %s\n\nChannel: #%s (%s)\n", d.channel.Name, d.day, d.channel.Name, d.channel.ID)
	for _, m := range messages {
		if m.Subtype == "thread_broadcast" && m.ThreadTS != m.TS && parents[m.ThreadTS] {
			continue
		}
		text.WriteString("\n")
		p.line(&text, "", "", m, d.day)
		if m.ReplyCount == 0 {
			continue
		}
		replies, err := p.c.replies(ctx, d.channel.ID, m.TS)
		if err != nil {
			return nil, false, err
		}
		for _, r := range replies {
			p.line(&text, "  - ", "    ", r, d.day)
		}
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
	sum         [sha256.Size]byte
	messages    []slackMessage
	bytes       int
	// dropped is a day whose messages are not kept, to be read again if its
	// document is wanted.
	dropped bool
}

func (d *slackDay) id() string { return d.channel.ID + ":" + d.day }

// version is the day's document's version: a render revision, to bump
// whenever its text changes for the same messages, then 40 hex digits of a
// SHA-256 of the channel's name and what conversations.history says of each
// message (its ts, edit, text, reply count and latest reply), which
// changes when one is posted, edited, deleted or replied to. The messages'
// digests combine by XOR, so the order Slack lists them in doesn't count.
// An edited reply changes none of it.
func (d *slackDay) version() string {
	sum := sha256.Sum256(append([]byte(d.channel.Name+"\x00"), d.sum[:]...))
	return "1|" + hex.EncodeToString(sum[:20])
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
	return map[string]any{
		"message_id": channel + ":" + m.TS, "channel_id": channel, "ts": m.TS, "thread_ts": orNull(m.ThreadTS),
		"user_id": orNull(m.User), "bot_id": orNull(m.BotID), "subtype": orNull(m.Subtype), "text": m.Text,
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
// but those slackSkipped names; conversations.replies lists the parent first.
func (c *slackClient) replies(ctx context.Context, channel, parent string) ([]slackMessage, error) {
	var replies []slackMessage
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
		fields, err := c.call(ctx, method, q)
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
// with the scope to add, wrapping errMissingScope.
func (c *slackClient) call(ctx context.Context, method string, query url.Values) (map[string]json.RawMessage, error) {
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
		return nil, refused(err, "could not reach Slack; try again")
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
		return nil, refused(nil, "this token has used up its Slack rate limit; try again %s", when)
	case status != http.StatusOK:
		return nil, refused(nil, "Slack returned HTTP %d for %s", status, method)
	case decodeErr != nil:
		return nil, fmt.Errorf("decode slack %s: %w", method, decodeErr)
	case answer.OK:
		return fields, nil
	case slackRejected[answer.Error]:
		return nil, refused(nil, "Slack rejected this token; check it and enter it again")
	case answer.Error == "missing_scope":
		return nil, refused(errMissingScope, "add the %s scope to the app and reinstall it", cmp.Or(answer.Needed, "missing"))
	}
	return nil, refused(nil, "Slack returned %s for %s", cmp.Or(answer.Error, "an error"), method)
}
