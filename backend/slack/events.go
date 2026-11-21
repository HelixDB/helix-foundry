package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Event is a request to the events URL: a closed set. ParseEvent reads the
// events the bot acts on into their own types, Slack's url_verification into
// URLVerification, and everything else into Ignored (bot messages, edits,
// deletes, joins, events of types the bot doesn't subscribe to).
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
	Event               json.RawMessage `json:"event"`
	Authorizations      []struct {
		TeamID       string `json:"team_id"`
		EnterpriseID string `json:"enterprise_id"`
	} `json:"authorizations"`
}

// inner is the fields of an event the bot reads.
type inner struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	ChannelType string `json:"channel_type"`
	User        string `json:"user"`
	UserTeam    string `json:"user_team"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	Channel     string `json:"channel"`
	// Edited is set on a message that was edited.
	Edited json.RawMessage `json:"edited"`
	Tokens struct {
		Bot []string `json:"bot"`
	} `json:"tokens"`
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
	cb.Event = readEvent(ev, team, enterprise, env.ExtShared, at)
	return cb, nil
}

// readEvent reads the inner event of an event_callback for team.
func readEvent(ev inner, team, enterprise string, extShared bool, at time.Time) Event {
	conversation := func() (Conversation, bool) {
		thread := ev.ThreadTS
		if thread == "" {
			thread = ev.TS
		}
		c := Conversation{TeamID: team, EnterpriseID: enterprise, ChannelID: ev.Channel, ThreadTS: thread, ExtShared: extShared}
		return c, idRE.MatchString(ev.Channel) && tsRE.MatchString(thread)
	}
	switch ev.Type {
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
	case "app_mention", "message":
	default:
		return Ignored{Why: fmt.Sprintf("event type %q", ev.Type)}
	}
	switch {
	case ev.Type == "message" && ev.ChannelType != "im":
		return Ignored{Why: fmt.Sprintf("message in a %q", ev.ChannelType)}
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

// EventKey is the intake dedupe key of an event: "e:" and its id.
func EventKey(eventID string) string { return "e:" + eventID }
