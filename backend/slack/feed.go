package slack

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// FeedEvent is an event the Slack data connector keeps, read by ParseEvent
// from the message.channels and message.groups events and the channel
// lifecycle events (ChannelSubscriptions): a closed set. Every FeedEvent is
// an Event. None says whether it is Helix's own bot's, an excluded channel's
// or a team's with no data connector: intake decides that with the install
// at hand.
type FeedEvent interface {
	Event
	feedEvent()
}

// Place is the channel a feed event is about.
type Place struct {
	// TeamID and EnterpriseID are the install's team and Grid org, read as
	// Conversation's are.
	TeamID       string `json:"team_id"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	ChannelID    string `json:"channel_id"`
	// Private is a private channel: a message's channel_type "group", a
	// membership event's "G", a group_ event.
	Private bool `json:"private,omitempty"`
	// ExtShared is the envelope's is_ext_shared_channel: a channel shared
	// with another organisation, which the connector never keeps.
	ExtShared bool `json:"ext_shared,omitempty"`
}

// StoredMessage is a channel message as the connector keeps it: the fields
// of Slack's message object it reads, under Slack's own names, so a message
// of conversations.history or conversations.replies decodes into it as an
// event's does. Every id and ts in it is valid or empty; its text is
// Slack's raw mrkdwn.
type StoredMessage struct {
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`
	User     string `json:"user,omitempty"`
	BotID    string `json:"bot_id,omitempty"`
	// AppID is the app a bot's message came from.
	AppID      string     `json:"app_id,omitempty"`
	Username   string     `json:"username,omitempty"`
	BotProfile BotProfile `json:"bot_profile,omitzero"`
	Subtype    string     `json:"subtype,omitempty"`
	Text       string     `json:"text"`
	Edited     Edited     `json:"edited,omitzero"`
	// ReplyCount, ReplyUsersCount and LatestReply are a thread root's.
	ReplyCount      int    `json:"reply_count,omitempty"`
	ReplyUsersCount int    `json:"reply_users_count,omitempty"`
	LatestReply     string `json:"latest_reply,omitempty"`
}

// BotProfile is the bot a message came from.
type BotProfile struct {
	Name  string `json:"name,omitempty"`
	AppID string `json:"app_id,omitempty"`
}

// Edited is when a message was last edited: TS empty for never.
type Edited struct {
	TS string `json:"ts,omitempty"`
}

// valid reports a message whose ts is one and whose other ids and ts are
// valid or empty.
func (m StoredMessage) valid() bool {
	for _, id := range []string{m.User, m.BotID, m.AppID, m.BotProfile.AppID} {
		if id != "" && !idRE.MatchString(id) {
			return false
		}
	}
	for _, ts := range []string{m.ThreadTS, m.LatestReply, m.Edited.TS} {
		if ts != "" && !tsRE.MatchString(ts) {
			return false
		}
	}
	return tsRE.MatchString(m.TS) && m.ReplyCount >= 0 && m.ReplyUsersCount >= 0
}

// ChannelMessage is a new message in a channel: one with no subtype, or a
// subtype someone writes (thread_broadcast, bot_message, file_share,
// me_message and the like), never one SkippedSubtype names.
type ChannelMessage struct {
	At      Place         `json:"at"`
	Message StoredMessage `json:"message"`
	// UserTeamID is the event's user_team: the sender's own team, "" when
	// Slack leaves it out.
	UserTeamID string    `json:"user_team_id,omitempty"`
	EventTime  time.Time `json:"event_time"`
}

// MessageChanged is message_changed: Message is the message as it now is.
// A thread root deleted while it has replies becomes a tombstone (Subtype
// "tombstone"), which keeps its replies.
type MessageChanged struct {
	At        Place         `json:"at"`
	Message   StoredMessage `json:"message"`
	EventTime time.Time     `json:"event_time"`
}

// MessageDeleted is message_deleted: the message DeletedTS is gone, in the
// thread ThreadTS ("" for a message in no thread). A thread root with
// replies comes as a MessageChanged to a tombstone, or as this; the fold
// keeps a tombstone either way.
type MessageDeleted struct {
	At        Place     `json:"at"`
	DeletedTS string    `json:"deleted_ts"`
	ThreadTS  string    `json:"thread_ts,omitempty"`
	EventTime time.Time `json:"event_time"`
}

// ParentReplied is message_replied, which the Events API sends as a hidden
// message with a nested message and no subtype: the thread root TS's reply
// fields only, never a new message.
type ParentReplied struct {
	At              Place  `json:"at"`
	TS              string `json:"ts"`
	ReplyCount      int    `json:"reply_count"`
	ReplyUsersCount int    `json:"reply_users_count"`
	LatestReply     string `json:"latest_reply,omitempty"`
}

// ChannelRenamed is channel_rename, group_rename, or a channel_name or
// group_name message: the channel is now called Name.
type ChannelRenamed struct {
	At   Place  `json:"at"`
	Name string `json:"name"`
}

// GoneReason is why a channel left what the connector reads: a closed set,
// whose JSON refuses any other value.
type GoneReason string

// The reasons a channel is gone.
const (
	// GoneDeleted is channel_deleted or group_deleted.
	GoneDeleted GoneReason = "deleted"
	// GoneShared is channel_shared: now shared with another organisation.
	GoneShared GoneReason = "shared"
	// GoneLeft is channel_left or group_left: the bot was removed.
	GoneLeft GoneReason = "left"
)

// UnmarshalText reads a GoneReason, refusing any other value.
func (r *GoneReason) UnmarshalText(b []byte) error {
	switch v := GoneReason(b); v {
	case GoneDeleted, GoneShared, GoneLeft:
		*r = v
		return nil
	}
	return fmt.Errorf("slack: unknown gone reason %q", b)
}

// ChannelGone is a channel the connector must stop reading and purge.
type ChannelGone struct {
	At  Place      `json:"at"`
	Why GoneReason `json:"why"`
}

// ChannelArchived is channel_archive or group_archive (Archived), or their
// unarchive.
type ChannelArchived struct {
	At       Place `json:"at"`
	Archived bool  `json:"archived"`
}

// MemberJoined is member_joined_channel, or a channel_join or group_join
// message: UserID joined the channel at JoinedAt. Intake keeps only the
// bot's own join (Slack doesn't document member_joined_channel for it, so
// the join message stands in).
type MemberJoined struct {
	At       Place     `json:"at"`
	UserID   string    `json:"user_id"`
	JoinedAt time.Time `json:"joined_at"`
}

// MemberLeft is member_left_channel: UserID left the channel. Intake keeps
// only the bot's, which is the channel gone.
type MemberLeft struct {
	At     Place  `json:"at"`
	UserID string `json:"user_id"`
}

// RateLimited is app_rate_limited: Slack dropped TeamID's events to the app
// in the minute starting at Minute, past its 30,000 an hour.
type RateLimited struct {
	TeamID string    `json:"team_id"`
	Minute time.Time `json:"minute"`
}

func (ChannelMessage) slackEvent()  {}
func (MessageChanged) slackEvent()  {}
func (MessageDeleted) slackEvent()  {}
func (ParentReplied) slackEvent()   {}
func (ChannelRenamed) slackEvent()  {}
func (ChannelGone) slackEvent()     {}
func (ChannelArchived) slackEvent() {}
func (MemberJoined) slackEvent()    {}
func (MemberLeft) slackEvent()      {}
func (RateLimited) slackEvent()     {}

func (ChannelMessage) feedEvent()  {}
func (MessageChanged) feedEvent()  {}
func (MessageDeleted) feedEvent()  {}
func (ParentReplied) feedEvent()   {}
func (ChannelRenamed) feedEvent()  {}
func (ChannelGone) feedEvent()     {}
func (ChannelArchived) feedEvent() {}
func (MemberJoined) feedEvent()    {}
func (MemberLeft) feedEvent()      {}
func (RateLimited) feedEvent()     {}

// ThreadReply is a reply in a channel thread the bot takes without a
// mention, while it posted there last apart from its sender: what intake
// stores for the worker to answer as a message (ChannelMessage.Reply).
// ParseEvent never returns one.
type ThreadReply struct {
	// At is the reply's thread: ThreadTS is its root.
	At         Conversation `json:"at"`
	UserID     string       `json:"user_id"`
	TS         string       `json:"ts"`
	UserTeamID string       `json:"user_team_id,omitempty"`
	Text       string       `json:"text"`
	EventTime  time.Time    `json:"event_time"`
}

func (ThreadReply) slackEvent() {}

// Reply is m as a reply the bot may take without a mention, when it is one:
// a person's (no bot id, no subtype but thread_broadcast) in a thread, not
// its root, in a channel not shared with another organisation, that doesn't
// mention botUserID (a mention reaches the bot as a Mention). Whether the
// thread is one the bot follows, and for whom, is intake's to read.
func (m ChannelMessage) Reply(botUserID string) (ThreadReply, bool) {
	msg := m.Message
	switch {
	case m.At.ExtShared, msg.BotID != "", msg.User == "", msg.Subtype != "" && msg.Subtype != "thread_broadcast",
		msg.ThreadTS == "" || msg.ThreadTS == msg.TS,
		strings.Contains(msg.Text, "<@"+botUserID+">") || strings.Contains(msg.Text, "<@"+botUserID+"|"):
		return ThreadReply{}, false
	}
	at := Conversation{TeamID: m.At.TeamID, EnterpriseID: m.At.EnterpriseID, ChannelID: m.At.ChannelID, ThreadTS: msg.ThreadTS}
	return ThreadReply{At: at, UserID: msg.User, TS: msg.TS, UserTeamID: m.UserTeamID, Text: msg.Text, EventTime: m.EventTime}, true
}

// SkippedSubtype reports a message subtype Slack posts about a channel
// itself (a join, a leave, a topic, a rename, an archive, a bot added),
// which says nothing anyone wrote: the connector keeps neither a row nor a
// document of one, whether polled or sent as an event.
func SkippedSubtype(subtype string) bool {
	switch subtype {
	case "channel_join", "channel_leave", "channel_topic", "channel_purpose", "channel_name", "channel_archive", "channel_unarchive",
		"group_join", "group_leave", "group_topic", "group_purpose", "group_name", "group_archive", "group_unarchive",
		"bot_add", "bot_remove":
		return true
	}
	return false
}

// Class is which of the door's gates a request to the events URL goes
// through: a closed set, so a flood of one can't refuse another.
type Class int

// The classes Classify sorts requests into.
const (
	// ClassBot is what the bot acts on: mentions, direct messages, lifecycle
	// events, app_rate_limited, and anything Classify can't read.
	ClassBot Class = iota
	// ClassReply is a message in a channel thread, not its root: an
	// unmentioned follow-up may be among them.
	ClassReply
	// ClassFeed is every other channel message, and the channel events the
	// connector reads.
	ClassFeed
)

// String names the class, for metrics.
func (c Class) String() string {
	switch c {
	case ClassReply:
		return "reply"
	case ClassFeed:
		return "feed"
	}
	return "bot"
}

// Classify sorts a request to the events URL by its type, its event's type
// and channel type, and its ts and thread_ts, without verifying it (the
// door classifies before the backend verifies): a message in a channel
// (channel_type channel or group) in a thread, not its root, is ClassReply;
// any other message there, and the channel events of ChannelSubscriptions,
// are ClassFeed; everything else, and anything it can't read, is ClassBot,
// which the backend answers.
func Classify(body []byte) Class {
	var req struct {
		Type  string `json:"type"`
		Event struct {
			Type        string `json:"type"`
			ChannelType string `json:"channel_type"`
			TS          string `json:"ts"`
			ThreadTS    string `json:"thread_ts"`
		} `json:"event"`
	}
	if json.Unmarshal(body, &req) != nil || req.Type != "event_callback" {
		return ClassBot
	}
	ev := req.Event
	switch {
	case ev.Type == "message" && (ev.ChannelType == "channel" || ev.ChannelType == "group"):
		if ev.ThreadTS != "" && ev.ThreadTS != ev.TS {
			return ClassReply
		}
		return ClassFeed
	case slices.Contains(ChannelSubscriptions, ev.Type):
		return ClassFeed
	}
	return ClassBot
}
