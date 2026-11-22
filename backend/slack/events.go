package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Event is a request to the events URL: a closed set. ParseEvent reads the
// events the bot acts on into their own types, the channel events the data
// connector keeps into FeedEvents, Slack's url_verification into
// URLVerification, and everything else into Ignored (bot messages, edits and
// deletes in DMs, group DMs, events of types the app doesn't subscribe to).
type Event interface{ slackEvent() }

// Conversation is where a message was said.
type Conversation struct {
	// TeamID is the install's team: the envelope's authorizations[0].team_id,
	// else its context_team_id, else its team_id (Enterprise Grid sends one
	// event per multi-workspace channel, for one of the workspaces).
	TeamID string `json:"team_id"`
	// EnterpriseID is the Grid org's id, "" outside one.
	EnterpriseID string `json:"enterprise_id,omitempty"`
	ChannelID    string `json:"channel_id"`
	// ThreadTS is the thread's root: the message's own ts at top level.
	ThreadTS string `json:"thread_ts"`
	// ExtShared is the envelope's is_ext_shared_channel: a channel shared
	// with other organisations, where the bot doesn't answer.
	ExtShared bool `json:"ext_shared,omitempty"`
}

// Mention is an app_mention: someone @mentioned the bot in a channel.
type Mention struct {
	At     Conversation `json:"at"`
	UserID string       `json:"user_id"`
	TS     string       `json:"ts"`
	// UserTeamID is the event's user_team: the sender's own team, "" when
	// Slack leaves it out.
	UserTeamID string `json:"user_team_id,omitempty"`
	// Text is the message as Slack sent it: raw mrkdwn (InputText reads it).
	Text      string    `json:"text"`
	EventTime time.Time `json:"event_time"`
}

// DirectMessage is a message.im someone wrote: in the bot's DM or its agent
// container, with no subtype.
type DirectMessage struct {
	At         Conversation `json:"at"`
	UserID     string       `json:"user_id"`
	TS         string       `json:"ts"`
	UserTeamID string       `json:"user_team_id,omitempty"`
	Text       string       `json:"text"`
	EventTime  time.Time    `json:"event_time"`
}

// SessionStopped is agent_session_stopped: UserID clicked the agent
// session's stop button in the thread At names. The app must stop the work
// and set the session's status itself.
type SessionStopped struct {
	At     Conversation `json:"at"`
	UserID string       `json:"user_id"`
}

// Uninstalled is app_uninstalled: the app was removed from TeamID.
type Uninstalled struct {
	TeamID    string    `json:"team_id"`
	EventTime time.Time `json:"event_time"`
}

// TokensRevoked is tokens_revoked: some of TeamID's tokens were revoked; Bot
// when the bot's token is among them (the only token the app holds).
type TokensRevoked struct {
	TeamID    string    `json:"team_id"`
	Bot       bool      `json:"bot"`
	EventTime time.Time `json:"event_time"`
}

// URLVerification is the handshake Slack sends when the events URL is set:
// answer with Challenge.
type URLVerification struct{ Challenge string }

// Ignored is a request the bot does nothing with, and why (for logs: it
// never holds what anyone wrote).
type Ignored struct{ Why string }

func (Mention) slackEvent()         {}
func (DirectMessage) slackEvent()   {}
func (SessionStopped) slackEvent()  {}
func (Uninstalled) slackEvent()     {}
func (TokensRevoked) slackEvent()   {}
func (URLVerification) slackEvent() {}
func (Ignored) slackEvent()         {}

// Callback is one request to the events URL, read.
type Callback struct {
	// EventID is Slack's id of the event, unique to it and kept across
	// Slack's retries: what intake dedupes on (EventKey). "" for a
	// URLVerification or an Ignored request that names none.
	EventID string
	// AppID is the app the event is for (api_app_id), "" when Slack leaves
	// it out.
	AppID string
	Event Event
}

var (
	// idRE is a Slack id: a team, enterprise, user, channel or app.
	idRE = regexp.MustCompile(`^[A-Z0-9]{1,32}$`)
	// tsRE is a message timestamp.
	tsRE = regexp.MustCompile(`^[0-9]{1,16}\.[0-9]{1,9}$`)
	// eventIDRE is an event id ("Ev" and letters and digits).
	eventIDRE = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
)

// envelope is the outer JSON of a request to the events URL.
type envelope struct {
	Type                string          `json:"type"`
	Challenge           string          `json:"challenge"`
	TeamID              string          `json:"team_id"`
	EnterpriseID        string          `json:"enterprise_id"`
	ContextTeamID       string          `json:"context_team_id"`
	ContextEnterpriseID string          `json:"context_enterprise_id"`
	APIAppID            string          `json:"api_app_id"`
	EventID             string          `json:"event_id"`
	EventTime           int64           `json:"event_time"`
	ExtShared           bool            `json:"is_ext_shared_channel"`
	MinuteRateLimited   int64           `json:"minute_rate_limited"`
	Event               json.RawMessage `json:"event"`
	Authorizations      []struct {
		TeamID       string `json:"team_id"`
		EnterpriseID string `json:"enterprise_id"`
	} `json:"authorizations"`
}

// inner is the fields of an event the bot reads.
type inner struct {
	Type        string     `json:"type"`
	Subtype     string     `json:"subtype"`
	ChannelType string     `json:"channel_type"`
	User        string     `json:"user"`
	UserTeam    string     `json:"user_team"`
	BotID       string     `json:"bot_id"`
	Text        string     `json:"text"`
	TS          string     `json:"ts"`
	ThreadTS    string     `json:"thread_ts"`
	EventTS     string     `json:"event_ts"`
	Channel     channelRef `json:"channel"`
	// Edited is set on a message that was edited.
	Edited json.RawMessage `json:"edited"`
	Tokens struct {
		Bot []string `json:"bot"`
	} `json:"tokens"`
}

// channelRef is an event's channel: its id, as most events send it, or an
// object with its id and name, as channel_rename and group_rename do.
type channelRef struct{ ID, Name string }

func (c *channelRef) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '{' {
		var o struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		err := json.Unmarshal(b, &o)
		*c = channelRef(o)
		return err
	}
	return json.Unmarshal(b, &c.ID)
}

// ParseEvent reads a request to the events URL. It returns an error only for
// a body that isn't a request Slack sends (not JSON, a url_verification
// without a challenge, an event_callback without an event or its id); a
// well-formed request the bot doesn't act on, or whose fields aren't what
// Slack sends, is Ignored, so Slack isn't made to retry it.
func ParseEvent(body []byte) (Callback, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Callback{}, fmt.Errorf("slack: decode event: %w", err)
	}
	switch env.Type {
	case "url_verification":
		if env.Challenge == "" {
			return Callback{}, errors.New("slack: url_verification without a challenge")
		}
		return Callback{Event: URLVerification{Challenge: env.Challenge}}, nil
	case "app_rate_limited":
		if !idRE.MatchString(env.TeamID) || env.MinuteRateLimited <= 0 {
			return Callback{AppID: env.APIAppID, Event: Ignored{Why: "app_rate_limited without its team or minute"}}, nil
		}
		return Callback{AppID: env.APIAppID, Event: RateLimited{TeamID: env.TeamID, Minute: time.Unix(env.MinuteRateLimited, 0).UTC()}}, nil
	case "event_callback":
	default:
		return Callback{Event: Ignored{Why: fmt.Sprintf("request type %q", env.Type)}}, nil
	}
	if !eventIDRE.MatchString(env.EventID) || len(env.Event) == 0 {
		return Callback{}, errors.New("slack: event_callback without an event or its id")
	}
	var ev inner
	if err := json.Unmarshal(env.Event, &ev); err != nil {
		return Callback{}, fmt.Errorf("slack: decode inner event: %w", err)
	}
	cb := Callback{EventID: env.EventID, AppID: env.APIAppID}
	team, enterprise := env.TeamID, env.EnterpriseID
	switch {
	case len(env.Authorizations) > 0 && env.Authorizations[0].TeamID != "":
		team, enterprise = env.Authorizations[0].TeamID, env.Authorizations[0].EnterpriseID
	case env.ContextTeamID != "":
		team, enterprise = env.ContextTeamID, env.ContextEnterpriseID
	}
	if enterprise == "" {
		enterprise = env.EnterpriseID
	}
	if !idRE.MatchString(team) || (enterprise != "" && !idRE.MatchString(enterprise)) {
		cb.Event = Ignored{Why: "no team"}
		return cb, nil
	}
	at := time.Unix(env.EventTime, 0).UTC()
	cb.Event = readEvent(ev, env.Event, team, enterprise, env.ExtShared, at)
	return cb, nil
}

// readEvent reads the inner event of an event_callback (ev, from raw) for
// team.
func readEvent(ev inner, raw json.RawMessage, team, enterprise string, extShared bool, at time.Time) Event {
	conversation := func() (Conversation, bool) {
		thread := ev.ThreadTS
		if thread == "" {
			thread = ev.TS
		}
		c := Conversation{TeamID: team, EnterpriseID: enterprise, ChannelID: ev.Channel.ID, ThreadTS: thread, ExtShared: extShared}
		return c, idRE.MatchString(ev.Channel.ID) && tsRE.MatchString(thread)
	}
	place := Place{TeamID: team, EnterpriseID: enterprise, ChannelID: ev.Channel.ID, ExtShared: extShared}
	if slices.Contains(ChannelSubscriptions, ev.Type) {
		place.Private = strings.HasPrefix(ev.Type, "group_") || ev.ChannelType == "G"
		return readChannelEvent(ev, place, at)
	}
	switch ev.Type {
	case "message":
		switch ev.ChannelType {
		case "channel", "group":
			place.Private = ev.ChannelType == "group"
			return readChannelMessage(ev, raw, place, at)
		case "im":
		default:
			return Ignored{Why: fmt.Sprintf("message in a %q", ev.ChannelType)}
		}
	case "app_uninstalled":
		return Uninstalled{TeamID: team, EventTime: at}
	case "tokens_revoked":
		return TokensRevoked{TeamID: team, Bot: len(ev.Tokens.Bot) > 0, EventTime: at}
	case "agent_session_stopped":
		c, ok := conversation()
		if !ok || !idRE.MatchString(ev.User) {
			return Ignored{Why: "agent_session_stopped without its thread or user"}
		}
		return SessionStopped{At: c, UserID: ev.User}
	case "app_mention":
	default:
		return Ignored{Why: fmt.Sprintf("event type %q", ev.Type)}
	}
	switch {
	case ev.Subtype != "":
		return Ignored{Why: fmt.Sprintf("message subtype %q", ev.Subtype)}
	case ev.BotID != "":
		return Ignored{Why: "a bot's message"}
	case len(ev.Edited) > 0 && string(ev.Edited) != "null":
		return Ignored{Why: "an edited message"}
	case !idRE.MatchString(ev.User) || !tsRE.MatchString(ev.TS) || (ev.UserTeam != "" && !idRE.MatchString(ev.UserTeam)):
		return Ignored{Why: "a message without its sender or ts"}
	}
	c, ok := conversation()
	if !ok {
		return Ignored{Why: "a message without its channel"}
	}
	if ev.Type == "app_mention" {
		return Mention{At: c, UserID: ev.User, TS: ev.TS, UserTeamID: ev.UserTeam, Text: ev.Text, EventTime: at}
	}
	return DirectMessage{At: c, UserID: ev.User, TS: ev.TS, UserTeamID: ev.UserTeam, Text: ev.Text, EventTime: at}
}

// readChannelEvent reads a channel lifecycle event (ChannelSubscriptions)
// about place.
func readChannelEvent(ev inner, place Place, at time.Time) Event {
	if !idRE.MatchString(place.ChannelID) {
		return Ignored{Why: fmt.Sprintf("%s without its channel", ev.Type)}
	}
	switch ev.Type {
	case "member_joined_channel", "member_left_channel":
		if !idRE.MatchString(ev.User) {
			return Ignored{Why: fmt.Sprintf("%s without its user", ev.Type)}
		}
		if ev.Type == "member_left_channel" {
			return MemberLeft{At: place, UserID: ev.User}
		}
		joined, ok := tsTime(ev.EventTS)
		if !ok {
			joined = at
		}
		return MemberJoined{At: place, UserID: ev.User, JoinedAt: joined}
	case "channel_rename", "group_rename":
		if ev.Channel.Name == "" {
			return Ignored{Why: fmt.Sprintf("%s without its name", ev.Type)}
		}
		return ChannelRenamed{At: place, Name: ev.Channel.Name}
	case "channel_deleted", "group_deleted":
		return ChannelGone{At: place, Why: GoneDeleted}
	case "channel_shared":
		return ChannelGone{At: place, Why: GoneShared}
	case "channel_left", "group_left":
		return ChannelGone{At: place, Why: GoneLeft}
	}
	return ChannelArchived{At: place, Archived: !strings.HasSuffix(ev.Type, "_unarchive")}
}

// readChannelMessage reads a message event in a channel (ev, from raw) at
// place: a new message, a change, a delete, a root's new reply, a rename or
// a join; Ignored for the rest of what Slack posts about a channel itself
// (SkippedSubtype) and what isn't a message Slack sends.
func readChannelMessage(ev inner, raw json.RawMessage, place Place, at time.Time) Event {
	var m struct {
		StoredMessage
		Hidden    bool           `json:"hidden"`
		Name      string         `json:"name"`
		DeletedTS string         `json:"deleted_ts"`
		Message   *StoredMessage `json:"message"`
		Previous  *StoredMessage `json:"previous_message"`
	}
	if json.Unmarshal(raw, &m) != nil || !idRE.MatchString(place.ChannelID) {
		return Ignored{Why: "a channel message that isn't one Slack sends"}
	}
	switch {
	case m.Subtype == "message_changed":
		if m.Message == nil || !m.Message.valid() {
			return Ignored{Why: "message_changed without its message"}
		}
		return MessageChanged{At: place, Message: *m.Message, EventTime: at}
	case m.Subtype == "message_deleted":
		thread := m.ThreadTS
		if m.Previous != nil && m.Previous.ThreadTS != "" {
			thread = m.Previous.ThreadTS
		}
		if !tsRE.MatchString(m.DeletedTS) || (thread != "" && !tsRE.MatchString(thread)) {
			return Ignored{Why: "message_deleted without its ts"}
		}
		return MessageDeleted{At: place, DeletedTS: m.DeletedTS, ThreadTS: thread, EventTime: at}
	case m.Subtype == "message_replied" || m.Hidden && m.Subtype == "" && m.Message != nil:
		if m.Message == nil || !m.Message.valid() {
			return Ignored{Why: "message_replied without its root"}
		}
		r := m.Message
		return ParentReplied{At: place, TS: r.TS, ReplyCount: r.ReplyCount, ReplyUsersCount: r.ReplyUsersCount, LatestReply: r.LatestReply}
	case !m.valid():
		return Ignored{Why: "a channel message without a valid ts or id"}
	case m.Subtype == "channel_join" || m.Subtype == "group_join":
		joined, _ := tsTime(m.TS) // a valid ts reads
		if !idRE.MatchString(m.User) {
			return Ignored{Why: "a join without its user"}
		}
		return MemberJoined{At: place, UserID: m.User, JoinedAt: joined}
	case m.Subtype == "channel_name" || m.Subtype == "group_name":
		if m.Name == "" {
			return Ignored{Why: "a rename without its name"}
		}
		return ChannelRenamed{At: place, Name: m.Name}
	case SkippedSubtype(m.Subtype):
		return Ignored{Why: fmt.Sprintf("message subtype %q", m.Subtype)}
	case m.Hidden:
		return Ignored{Why: fmt.Sprintf("a hidden message of subtype %q", m.Subtype)}
	}
	return ChannelMessage{At: place, Message: m.StoredMessage, UserTeamID: ev.UserTeam, EventTime: at}
}

// tsTime is a message ts as a time; ok is false for anything else.
func tsTime(ts string) (_ time.Time, ok bool) {
	if !tsRE.MatchString(ts) {
		return time.Time{}, false
	}
	seconds, fraction, _ := strings.Cut(ts, ".")
	s, _ := strconv.ParseInt(seconds, 10, 64)                         // at most 16 digits: it reads
	nanos, _ := strconv.ParseInt((fraction + "00000000")[:9], 10, 64) // nine digits: it reads
	return time.Unix(s, nanos).UTC(), true
}

// EventKey is the intake dedupe key of an event: "e:" and its id.
func EventKey(eventID string) string { return "e:" + eventID }
