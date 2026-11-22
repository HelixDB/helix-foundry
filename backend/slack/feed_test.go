package slack_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
)

const root = "1795000000.000100"

var (
	genPlace = slack.Place{TeamID: "T0AAA1111", ChannelID: "C0GEN0001"}
	private  = slack.Place{TeamID: "T0AAA1111", ChannelID: "C0PRIV0001", Private: true}
	legacyG  = slack.Place{TeamID: "T0AAA1111", ChannelID: "G0PRIV0001", Private: true}
	adaReply = slack.StoredMessage{TS: "1795000020.000100", ThreadTS: root, User: "U0ADA0001", Text: "and last quarter?"}
)

// feedCases are what the data connector's fixtures read as.
func feedCases() map[string]slack.Callback {
	cb := func(id string, e slack.Event) slack.Callback {
		return slack.Callback{EventID: id, AppID: "A0FOUNDRY", Event: e}
	}
	return map[string]slack.Callback{
		"channel_message.json": cb("Ev0CHANMSG1", slack.ChannelMessage{At: genPlace,
			Message: slack.StoredMessage{TS: "1795000015.000100", User: "U0ADA0001", Text: "hi"}, EventTime: eventTime}),
		"channel_message_private.json": cb("Ev0GRPMSG01", slack.ChannelMessage{At: private, Message: slack.StoredMessage{
			TS: "1795000016.000100", BotID: "B0OTHER01", AppID: "A0OTHER01", Username: "Deploys",
			BotProfile: slack.BotProfile{Name: "Deploys", AppID: "A0OTHER01"}, Subtype: "bot_message", Text: "Deployed *api*"}, EventTime: eventTime}),
		"channel_reply.json": cb("Ev0REPLY001", slack.ChannelMessage{At: genPlace, Message: adaReply, UserTeamID: "T0AAA1111", EventTime: eventTime}),
		"channel_broadcast.json": cb("Ev0BCAST001", slack.ChannelMessage{At: genPlace, Message: slack.StoredMessage{
			TS: "1795000021.000100", ThreadTS: root, User: "U0ADA0001", Subtype: "thread_broadcast", Text: "also in channel"}, EventTime: eventTime}),
		"channel_ext_shared.json": cb("Ev0EXTMSG01", slack.ChannelMessage{At: slack.Place{TeamID: "T0AAA1111", ChannelID: "C0SHARED01", ExtShared: true},
			Message: slack.StoredMessage{TS: "1795000022.000100", User: "U0EXT0001", Text: "hi from outside"}, UserTeamID: "T0OTHER01", EventTime: eventTime}),
		"channel_topic.json": cb("Ev0TOPIC001", slack.Ignored{Why: `message subtype "channel_topic"`}),
		"mpim.json":          cb("Ev0MPIM0001", slack.Ignored{Why: `message in a "mpim"`}),
		"message_replied.json": cb("Ev0REPLIED1", slack.ParentReplied{At: genPlace, TS: root, ReplyCount: 2, ReplyUsersCount: 1,
			LatestReply: "1795000020.000100"}),
		"message_changed.json": cb("Ev0CHANGED1", slack.MessageChanged{At: genPlace, Message: slack.StoredMessage{
			TS: "1795000020.000100", ThreadTS: root, User: "U0ADA0001", Text: "and last year?", Edited: slack.Edited{TS: "1795000025.000000"}},
			EventTime: eventTime}),
		"message_tombstone.json": cb("Ev0TOMBST01", slack.MessageChanged{At: genPlace, Message: slack.StoredMessage{
			TS: root, ThreadTS: root, User: "USLACKBOT", Subtype: "tombstone", Text: "This message was deleted.",
			ReplyCount: 2, ReplyUsersCount: 1, LatestReply: "1795000020.000100"}, EventTime: eventTime}),
		"message_deleted.json": cb("Ev0DELETED1", slack.MessageDeleted{At: genPlace, DeletedTS: "1795000020.000100", ThreadTS: root, EventTime: eventTime}),
		"channel_join_bot.json": cb("Ev0BOTJOIN1", slack.MemberJoined{At: genPlace, UserID: "U0BOT0001",
			JoinedAt: time.Unix(1795000028, 100000).UTC()}),
		"member_joined.json": cb("Ev0JOIN0001", slack.MemberJoined{At: genPlace, UserID: "U0ADA0001",
			JoinedAt: time.Unix(1795000014, 100000).UTC()}),
		"member_left.json":          cb("Ev0LEFT0001", slack.MemberLeft{At: private, UserID: "U0BOT0001"}),
		"channel_name_message.json": cb("Ev0CHNAME01", slack.ChannelRenamed{At: genPlace, Name: "announcements"}),
		"channel_rename.json":       cb("Ev0RENAME01", slack.ChannelRenamed{At: genPlace, Name: "announcements"}),
		"group_rename.json":         cb("Ev0GRRENAM1", slack.ChannelRenamed{At: legacyG, Name: "secret-plans"}),
		"channel_left.json":         cb("Ev0CHLEFT01", slack.ChannelGone{At: genPlace, Why: slack.GoneLeft}),
		"group_left.json":           cb("Ev0GRLEFT01", slack.ChannelGone{At: legacyG, Why: slack.GoneLeft}),
		"channel_deleted.json":      cb("Ev0CHDEL001", slack.ChannelGone{At: genPlace, Why: slack.GoneDeleted}),
		"group_deleted.json":        cb("Ev0GRDEL001", slack.ChannelGone{At: legacyG, Why: slack.GoneDeleted}),
		"channel_shared.json":       cb("Ev0SHARED01", slack.ChannelGone{At: genPlace, Why: slack.GoneShared}),
		"channel_archive.json":      cb("Ev0ARCHIVE1", slack.ChannelArchived{At: genPlace, Archived: true}),
		"channel_unarchive.json":    cb("Ev0UNARCH01", slack.ChannelArchived{At: genPlace}),
		"group_archive.json":        cb("Ev0GRARCH01", slack.ChannelArchived{At: legacyG, Archived: true}),
		"group_unarchive.json":      cb("Ev0GRUNAR01", slack.ChannelArchived{At: legacyG}),
		"app_rate_limited.json": {AppID: "A0FOUNDRY", Event: slack.RateLimited{TeamID: "T0AAA1111",
			Minute: time.Unix(1518467820, 0).UTC()}},
	}
}

// roundTrip is e marshalled and read back into its own type.
func roundTrip(e slack.FeedEvent) (slack.FeedEvent, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	back := reflect.New(reflect.TypeOf(e))
	if err := json.Unmarshal(b, back.Interface()); err != nil {
		return nil, err
	}
	return back.Elem().Interface().(slack.FeedEvent), nil
}

// Every feed event a fixture reads as keeps itself through JSON, which is
// how intake stores it.
func TestFeedEventsRoundTrip(t *testing.T) {
	for name, c := range feedCases() {
		fe, ok := c.Event.(slack.FeedEvent)
		if !ok {
			continue
		}
		back, err := roundTrip(fe)
		require.NoError(t, err, name)
		require.Equal(t, fe, back, name)
	}
	// A stored reason Slack never gives is refused.
	var gone slack.ChannelGone
	require.Error(t, json.Unmarshal([]byte(`{"at":{},"why":"moved"}`), &gone))
	require.NoError(t, json.Unmarshal([]byte(`{"at":{},"why":"shared"}`), &gone))
	require.Equal(t, slack.GoneShared, gone.Why)
}

// A message of conversations.history reads into the same StoredMessage as
// the event of it, so the backfill and the events write the same rows.
func TestStoredMessageFromHistory(t *testing.T) {
	var m slack.StoredMessage
	require.NoError(t, json.Unmarshal([]byte(`{"type":"message","user":"U0ADA0001","text":"and last quarter?","ts":"1795000020.000100",
		"thread_ts":"1795000000.000100","parent_user_id":"U0BOB0001","blocks":[]}`), &m))
	require.Equal(t, adaReply, m)
	// Nothing Slack left out is written back.
	b, err := json.Marshal(slack.StoredMessage{TS: "1.2", Text: ""})
	require.NoError(t, err)
	require.JSONEq(t, `{"ts":"1.2","text":""}`, string(b))
}

// What isn't a channel event Slack sends is Ignored, saying why, or, where
// it can't be read at all, an error.
func TestParseFeedEventRefusals(t *testing.T) {
	event := func(fields string) string {
		return `{"type":"event_callback","event_id":"Ev1","team_id":"T1","event_time":1795000000,"event":{` + fields + `}}`
	}
	msg := func(fields string) string { return event(`"type":"message","channel_type":"channel",` + fields) }
	for name, c := range map[string]struct{ body, why string }{
		"no channel":                {msg(`"user":"U1","ts":"1.2"`), "a channel message that isn't one Slack sends"},
		"bot profile not an object": {msg(`"channel":"C1","ts":"1.2","bot_profile":"x"`), "a channel message that isn't one Slack sends"},
		"bad ts":                    {msg(`"channel":"C1","ts":"1"`), "a channel message without a valid ts or id"},
		"bad user":                  {msg(`"channel":"C1","ts":"1.2","user":"u1"`), "a channel message without a valid ts or id"},
		"bad thread":                {msg(`"channel":"C1","ts":"1.2","thread_ts":"x"`), "a channel message without a valid ts or id"},
		"bad bot":                   {msg(`"channel":"C1","ts":"1.2","bot_id":"b"`), "a channel message without a valid ts or id"},
		"bad app":                   {msg(`"channel":"C1","ts":"1.2","bot_profile":{"app_id":"a"}`), "a channel message without a valid ts or id"},
		"bad edit":                  {msg(`"channel":"C1","ts":"1.2","edited":{"ts":"x"}`), "a channel message without a valid ts or id"},
		"negative count":            {msg(`"channel":"C1","ts":"1.2","reply_count":-1`), "a channel message without a valid ts or id"},
		"changed without message":   {msg(`"channel":"C1","ts":"1.2","subtype":"message_changed","hidden":true`), "message_changed without its message"},
		"changed bad message": {msg(`"channel":"C1","ts":"1.2","subtype":"message_changed","hidden":true,"message":{"ts":"x"}`),
			"message_changed without its message"},
		"deleted without ts": {msg(`"channel":"C1","ts":"1.2","subtype":"message_deleted","hidden":true`), "message_deleted without its ts"},
		"deleted bad thread": {msg(`"channel":"C1","ts":"1.2","subtype":"message_deleted","deleted_ts":"1.1","previous_message":{"ts":"1.1","thread_ts":"y"}`),
			"message_deleted without its ts"},
		"replied without root":  {msg(`"channel":"C1","ts":"1.2","subtype":"message_replied","hidden":true`), "message_replied without its root"},
		"replied bad root":      {msg(`"channel":"C1","ts":"1.2","hidden":true,"message":{"ts":"1.1","latest_reply":"z"}`), "message_replied without its root"},
		"join without user":     {msg(`"channel":"C1","ts":"1.2","subtype":"group_join"`), "a join without its user"},
		"rename without name":   {msg(`"channel":"C1","ts":"1.2","subtype":"group_name"`), "a rename without its name"},
		"hidden other":          {msg(`"channel":"C1","ts":"1.2","subtype":"message_reposted","hidden":true`), `a hidden message of subtype "message_reposted"`},
		"hidden bare":           {msg(`"channel":"C1","ts":"1.2","hidden":true`), `a hidden message of subtype ""`},
		"bot added":             {msg(`"channel":"C1","ts":"1.2","subtype":"bot_add","bot_id":"B1"`), `message subtype "bot_add"`},
		"app home":              {event(`"type":"message","channel_type":"app_home","channel":"D1","user":"U1","ts":"1.2"`), `message in a "app_home"`},
		"no channel type":       {event(`"type":"message","channel":"C1","user":"U1","ts":"1.2"`), `message in a ""`},
		"event without channel": {event(`"type":"channel_deleted"`), "channel_deleted without its channel"},
		"join without its user": {event(`"type":"member_joined_channel","channel":"C1","channel_type":"C"`),
			"member_joined_channel without its user"},
		"leave bad user":        {event(`"type":"member_left_channel","channel":"C1","user":"u"`), "member_left_channel without its user"},
		"rename without a name": {event(`"type":"channel_rename","channel":{"id":"C1"}`), "channel_rename without its name"},
		"rename by id only":     {event(`"type":"group_rename","channel":"G1"`), "group_rename without its name"},
		"another request":       {`{"type":"block_actions"}`, `request type "block_actions"`},
		"another event":         {event(`"type":"reaction_added","channel":"C1"`), `event type "reaction_added"`},
		"rate limit no team":    {`{"type":"app_rate_limited","minute_rate_limited":1518467820}`, "app_rate_limited without its team or minute"},
		"rate limit no minute":  {`{"type":"app_rate_limited","team_id":"T1"}`, "app_rate_limited without its team or minute"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := slack.ParseEvent([]byte(c.body))
			require.NoError(t, err)
			require.Equal(t, slack.Ignored{Why: c.why}, got.Event)
		})
	}
	for name, body := range map[string]string{
		"channel a number":     event(`"type":"channel_deleted","channel":5`),
		"channel id a number":  event(`"type":"channel_rename","channel":{"id":5,"name":"x"}`),
		"rate limit bad types": `{"type":"app_rate_limited","team_id":"T1","minute_rate_limited":"soon"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := slack.ParseEvent([]byte(body))
			require.Error(t, err)
		})
	}

	at := time.Unix(1795000000, 0).UTC()
	read := func(body string) slack.Event {
		t.Helper()
		got, err := slack.ParseEvent([]byte(body))
		require.NoError(t, err)
		return got.Event
	}
	// A delete with no previous message is in the event's own thread.
	require.Equal(t, slack.MessageDeleted{At: slack.Place{TeamID: "T1", ChannelID: "C1"}, DeletedTS: "1.1", ThreadTS: "1.0", EventTime: at},
		read(msg(`"channel":"C1","ts":"1.2","thread_ts":"1.0","subtype":"message_deleted","deleted_ts":"1.1"`)))
	// A join's ts reads to the nanosecond, whatever its digits.
	require.Equal(t, slack.MemberJoined{At: slack.Place{TeamID: "T1", ChannelID: "C1"}, UserID: "U1", JoinedAt: time.Unix(1, 200000000).UTC()},
		read(msg(`"channel":"C1","ts":"1.2","subtype":"channel_join","user":"U1"`)))
	require.Equal(t, time.Unix(1, 123456789).UTC(),
		read(msg(`"channel":"C1","ts":"1.123456789","subtype":"channel_join","user":"U1"`)).(slack.MemberJoined).JoinedAt)
	// A membership event without a valid event_ts joined when it was sent.
	require.Equal(t, slack.MemberJoined{At: slack.Place{TeamID: "T1", ChannelID: "C1"}, UserID: "U1", JoinedAt: at},
		read(event(`"type":"member_joined_channel","channel":"C1","channel_type":"C","user":"U1","event_ts":"soon"`)))
	// The explicit message_replied subtype is read as Slack's hidden one is.
	require.Equal(t, slack.ParentReplied{At: slack.Place{TeamID: "T1", ChannelID: "C1"}, TS: "1.1", ReplyCount: 1},
		read(msg(`"channel":"C1","ts":"1.2","subtype":"message_replied","hidden":true,"message":{"ts":"1.1","reply_count":1}`)))
	// A null channel is none.
	require.Equal(t, slack.Ignored{Why: "channel_archive without its channel"}, read(event(`"type":"channel_archive","channel":null`)))
}

func TestSkippedSubtype(t *testing.T) {
	for _, s := range []string{"channel_join", "channel_leave", "channel_topic", "channel_purpose", "channel_name", "channel_archive",
		"channel_unarchive", "group_join", "group_leave", "group_topic", "group_purpose", "group_name", "group_archive", "group_unarchive",
		"bot_add", "bot_remove"} {
		require.True(t, slack.SkippedSubtype(s), s)
	}
	for _, s := range []string{"", "thread_broadcast", "bot_message", "file_share", "me_message", "join", "channel_", "group_convert_to_private"} {
		require.False(t, slack.SkippedSubtype(s), s)
	}
}

// Classify sorts thread replies from the rest of the channels' messages and
// events, and leaves everything else, and anything it can't read, to the
// bot's gate.
func TestClassify(t *testing.T) {
	for name, want := range map[string]slack.Class{
		"mention.json": slack.ClassBot, "mention_in_thread.json": slack.ClassBot, "dm.json": slack.ClassBot,
		"dm_in_agent_thread.json": slack.ClassBot, "mpim.json": slack.ClassBot, "app_uninstalled.json": slack.ClassBot,
		"tokens_revoked.json": slack.ClassBot, "agent_session_stopped.json": slack.ClassBot, "url_verification.json": slack.ClassBot,
		"app_rate_limited.json": slack.ClassBot,
		"channel_reply.json":    slack.ClassReply, "channel_broadcast.json": slack.ClassReply,
		"channel_message.json": slack.ClassFeed, "channel_message_private.json": slack.ClassFeed, "channel_ext_shared.json": slack.ClassFeed,
		"message_replied.json": slack.ClassFeed, "message_changed.json": slack.ClassFeed, "message_tombstone.json": slack.ClassFeed,
		"message_deleted.json": slack.ClassFeed, "channel_join_bot.json": slack.ClassFeed, "channel_topic.json": slack.ClassFeed,
		"member_joined.json": slack.ClassFeed, "member_left.json": slack.ClassFeed, "channel_rename.json": slack.ClassFeed,
		"group_archive.json": slack.ClassFeed, "channel_shared.json": slack.ClassFeed,
	} {
		require.Equal(t, want, slack.Classify(fixture(t, "events/"+name)), name)
	}
	for name, body := range map[string]string{
		"not json":         `{`,
		"empty":            ``,
		"event wrong type": `{"type":"event_callback","event":{"type":"message","channel_type":"channel","ts":1}}`,
		"no event":         `{"type":"event_callback"}`,
		"channel unshared": `{"type":"event_callback","event":{"type":"channel_unshared","channel":"C1"}}`,
		"thread not event": `{"type":"block_actions","event":{"type":"message","channel_type":"channel","ts":"1.2","thread_ts":"1.1"}}`,
	} {
		require.Equal(t, slack.ClassBot, slack.Classify([]byte(body)), name)
	}
	// A thread's root, as sent with its own thread_ts, is no reply.
	require.Equal(t, slack.ClassFeed, slack.Classify([]byte(
		`{"type":"event_callback","event":{"type":"message","channel_type":"group","ts":"1.1","thread_ts":"1.1"}}`)))
	require.Equal(t, []string{"bot", "reply", "feed", "bot"}, []string{
		slack.ClassBot.String(), slack.ClassReply.String(), slack.ClassFeed.String(), slack.Class(7).String()})
}

// Reply takes only a person's reply in a thread, outside a shared channel,
// that doesn't mention the bot.
func TestChannelMessageReply(t *testing.T) {
	base := slack.ChannelMessage{At: slack.Place{TeamID: "T1", EnterpriseID: "E1", ChannelID: "C1", Private: true},
		Message: adaReply, UserTeamID: "T1", EventTime: eventTime}
	reply, ok := base.Reply("U0BOT0001")
	require.True(t, ok)
	require.Equal(t, slack.ThreadReply{At: slack.Conversation{TeamID: "T1", EnterpriseID: "E1", ChannelID: "C1", ThreadTS: root},
		UserID: "U0ADA0001", TS: adaReply.TS, UserTeamID: "T1", Text: adaReply.Text, EventTime: eventTime}, reply)
	broadcast := base
	broadcast.Message.Subtype = "thread_broadcast"
	_, ok = broadcast.Reply("U0BOT0001")
	require.True(t, ok, "a broadcast is a reply too")

	for name, change := range map[string]func(*slack.ChannelMessage){
		"shared":          func(m *slack.ChannelMessage) { m.At.ExtShared = true },
		"a bot's":         func(m *slack.ChannelMessage) { m.Message.BotID = "B1" },
		"no sender":       func(m *slack.ChannelMessage) { m.Message.User = "" },
		"a subtype":       func(m *slack.ChannelMessage) { m.Message.Subtype = "file_share" },
		"top level":       func(m *slack.ChannelMessage) { m.Message.ThreadTS = "" },
		"the root":        func(m *slack.ChannelMessage) { m.Message.ThreadTS = m.Message.TS },
		"a mention":       func(m *slack.ChannelMessage) { m.Message.Text = "ok <@U0BOT0001> go" },
		"a named mention": func(m *slack.ChannelMessage) { m.Message.Text = "<@U0BOT0001|foundry> go" },
	} {
		m := base
		change(&m)
		_, ok := m.Reply("U0BOT0001")
		require.False(t, ok, name)
	}
	// Mentioning someone else is still a reply to the bot.
	other := base
	other.Message.Text = "ask <@U0BOB0001> too"
	_, ok = other.Reply("U0BOT0001")
	require.True(t, ok)
}

func ExampleClassify() {
	reply := []byte(`{"type":"event_callback","event":{"type":"message","channel_type":"channel","ts":"1795000020.000100","thread_ts":"1795000000.000100"}}`)
	mention := []byte(`{"type":"event_callback","event":{"type":"app_mention","ts":"1795000020.000100","thread_ts":"1795000000.000100"}}`)
	fmt.Println(slack.Classify(reply), slack.Classify(mention), slack.Classify([]byte("garbage")))
	// Output:
	// reply bot bot
}

func ExampleChannelMessage_Reply() {
	cb, _ := slack.ParseEvent([]byte(`{"type":"event_callback","event_id":"Ev1","event_time":1795000000,"team_id":"T1",
		"event":{"type":"message","channel_type":"channel","channel":"C1","user":"U1","text":"and last quarter?",
		"ts":"1795000020.000100","thread_ts":"1795000000.000100"}}`))
	reply, ok := cb.Event.(slack.ChannelMessage).Reply("U0BOT")
	fmt.Println(ok, reply.At.ChannelID, reply.At.ThreadTS, reply.UserID, reply.Text)
	// Output:
	// true C1 1795000000.000100 U1 and last quarter?
}
