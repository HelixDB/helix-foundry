package slack_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
)

func fixture(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", path))
	require.NoError(t, err)
	return b
}

var eventTime = time.Unix(1795000000, 0).UTC()

// Every fixture reads as the event the bot acts on, or as Ignored.
func TestParseEventFixtures(t *testing.T) {
	general := slack.Conversation{TeamID: "T0AAA1111", ChannelID: "C0GEN0001", ThreadTS: "1795000000.000100"}
	dm := slack.Conversation{TeamID: "T0AAA1111", ChannelID: "D0DM00001", ThreadTS: "1795000010.000100"}
	gridAt := slack.Conversation{TeamID: "T0GRIDWS01", EnterpriseID: "E0ACME001", ChannelID: "C0GEN0001", ThreadTS: "1795000000.000100"}
	ask := "<@U0BOT0001> which customers renewed last month?"
	cases := map[string]slack.Callback{
		"mention.json": {EventID: "Ev0MENTION1", AppID: "A0FOUNDRY", Event: slack.Mention{
			At: general, UserID: "U0ADA0001", TS: "1795000000.000100", UserTeamID: "T0AAA1111", Text: ask, EventTime: eventTime}},
		"mention_in_thread.json": {EventID: "Ev0MENTION2", AppID: "A0FOUNDRY", Event: slack.Mention{
			At:     slack.Conversation{TeamID: "T0AAA1111", ChannelID: "C0GEN0001", ThreadTS: "1794999000.000050"},
			UserID: "U0ADA0001", TS: "1795000001.000200", UserTeamID: "T0AAA1111", Text: ask, EventTime: eventTime}},
		"mention_edited.json": {EventID: "Ev0EDITED01", AppID: "A0FOUNDRY", Event: slack.Ignored{Why: "an edited message"}},
		"mention_by_bot.json": {EventID: "Ev0BOTMENT1", AppID: "A0FOUNDRY", Event: slack.Ignored{Why: "a bot's message"}},
		"mention_ext_shared.json": {EventID: "Ev0EXTSHR01", AppID: "A0FOUNDRY", Event: slack.Mention{
			At:     slack.Conversation{TeamID: "T0AAA1111", ChannelID: "C0GEN0001", ThreadTS: "1795000000.000100", ExtShared: true},
			UserID: "U0ADA0001", TS: "1795000000.000100", UserTeamID: "T0AAA1111", Text: ask, EventTime: eventTime}},
		"mention_connect_user.json": {EventID: "Ev0CONNECT1", AppID: "A0FOUNDRY", Event: slack.Mention{
			At:     slack.Conversation{TeamID: "T0AAA1111", ChannelID: "C0GEN0001", ThreadTS: "1795000000.000100", ExtShared: true},
			UserID: "U0EXT0001", TS: "1795000000.000100", UserTeamID: "T0OTHER01", Text: ask, EventTime: eventTime}},
		"mention_grid_workspace.json": {EventID: "Ev0GRID0001", AppID: "A0FOUNDRY", Event: slack.Mention{
			At: gridAt, UserID: "W0GRID0001", TS: "1795000000.000100", UserTeamID: "T0GRIDWS02", Text: ask, EventTime: eventTime}},
		"mention_grid_multi.json": {EventID: "Ev0GRID0002", AppID: "A0FOUNDRY", Event: slack.Mention{
			At: gridAt, UserID: "W0GRID0001", TS: "1795000000.000100", UserTeamID: "T0GRIDWS02", Text: ask, EventTime: eventTime}},
		"dm.json": {EventID: "Ev0DM000001", AppID: "A0FOUNDRY", Event: slack.DirectMessage{
			At: dm, UserID: "U0ADA0001", TS: "1795000010.000100", Text: "hello &lt;there&gt;", EventTime: eventTime}},
		"dm_in_agent_thread.json": {EventID: "Ev0DM000002", AppID: "A0FOUNDRY", Event: slack.DirectMessage{
			At:     slack.Conversation{TeamID: "T0AAA1111", ChannelID: "D0DM00001", ThreadTS: "1795000005.000001"},
			UserID: "U0ADA0001", TS: "1795000010.000100", Text: "hello &lt;there&gt;", EventTime: eventTime}},
		"dm_bot_message.json":      {EventID: "Ev0BOTMSG01", AppID: "A0FOUNDRY", Event: slack.Ignored{Why: `message subtype "bot_message"`}},
		"dm_own_post.json":         {EventID: "Ev0OWNPOST1", AppID: "A0FOUNDRY", Event: slack.Ignored{Why: "a bot's message"}},
		"dm_edit.json":             {EventID: "Ev0DMEDIT01", AppID: "A0FOUNDRY", Event: slack.Ignored{Why: `message subtype "message_changed"`}},
		"dm_delete.json":           {EventID: "Ev0DMDEL001", AppID: "A0FOUNDRY", Event: slack.Ignored{Why: `message subtype "message_deleted"`}},
		"member_joined.json":       {EventID: "Ev0JOIN0001", AppID: "A0FOUNDRY", Event: slack.Ignored{Why: `event type "member_joined_channel"`}},
		"channel_message.json":     {EventID: "Ev0CHANMSG1", AppID: "A0FOUNDRY", Event: slack.Ignored{Why: `message in a "channel"`}},
		"url_verification.json":    {Event: slack.URLVerification{Challenge: "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P"}},
		"app_rate_limited.json":    {Event: slack.Ignored{Why: `request type "app_rate_limited"`}},
		"app_uninstalled.json":     {EventID: "Ev0UNINST01", AppID: "A0FOUNDRY", Event: slack.Uninstalled{TeamID: "T0AAA1111", EventTime: eventTime}},
		"tokens_revoked.json":      {EventID: "Ev0REVOKE01", AppID: "A0FOUNDRY", Event: slack.TokensRevoked{TeamID: "T0AAA1111", Bot: true, EventTime: eventTime}},
		"tokens_revoked_user.json": {EventID: "Ev0REVOKE02", AppID: "A0FOUNDRY", Event: slack.TokensRevoked{TeamID: "T0AAA1111", EventTime: eventTime}},
		"agent_session_stopped.json": {EventID: "Ev0STOPPED1", AppID: "A0FOUNDRY", Event: slack.SessionStopped{
			At: slack.Conversation{TeamID: "T0AAA1111", ChannelID: "D0DM00001", ThreadTS: "1782234671.392669"}, UserID: "U0ADA0001"}},
	}
	entries, err := os.ReadDir("testdata/events")
	require.NoError(t, err)
	require.Len(t, cases, len(entries), "every fixture has a case")
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := slack.ParseEvent(fixture(t, "events/"+name))
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

// A body that isn't a request Slack sends is an error; a well-formed one
// whose fields aren't what Slack sends is Ignored.
func TestParseEventRefusals(t *testing.T) {
	for name, body := range map[string]string{
		"not json":             `{`,
		"no challenge":         `{"type":"url_verification"}`,
		"no event id":          `{"type":"event_callback","event":{"type":"app_uninstalled"},"team_id":"T1"}`,
		"bad event id":         `{"type":"event_callback","event_id":"Ev 1","event":{"type":"app_uninstalled"},"team_id":"T1"}`,
		"no event":             `{"type":"event_callback","event_id":"Ev1","team_id":"T1"}`,
		"event not an object":  `{"type":"event_callback","event_id":"Ev1","event":[1],"team_id":"T1"}`,
		"envelope wrong types": `{"type":"event_callback","event_id":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := slack.ParseEvent([]byte(body))
			require.Error(t, err)
		})
	}
	mention := func(fields string) string {
		return `{"type":"event_callback","event_id":"Ev1","team_id":"T1","event_time":1,"event":{"type":"app_mention",` + fields + `}}`
	}
	for name, c := range map[string]struct{ body, why string }{
		"no team":            {`{"type":"event_callback","event_id":"Ev1","event":{"type":"app_uninstalled"}}`, "no team"},
		"bad enterprise":     {`{"type":"event_callback","event_id":"Ev1","team_id":"T1","enterprise_id":"e-1","event":{"type":"app_uninstalled"}}`, "no team"},
		"no user":            {mention(`"ts":"1.2","channel":"C1"`), "a message without its sender or ts"},
		"bad ts":             {mention(`"user":"U1","ts":"1","channel":"C1"`), "a message without its sender or ts"},
		"bad user team":      {mention(`"user":"U1","ts":"1.2","channel":"C1","user_team":"t"`), "a message without its sender or ts"},
		"no channel":         {mention(`"user":"U1","ts":"1.2"`), "a message without its channel"},
		"bad thread":         {mention(`"user":"U1","ts":"1.2","thread_ts":"x","channel":"C1"`), "a message without its channel"},
		"stopped no user":    {`{"type":"event_callback","event_id":"Ev1","team_id":"T1","event":{"type":"agent_session_stopped","channel":"C1","thread_ts":"1.2"}}`, "agent_session_stopped without its thread or user"},
		"stopped no channel": {`{"type":"event_callback","event_id":"Ev1","team_id":"T1","event":{"type":"agent_session_stopped","user":"U1","thread_ts":"1.2"}}`, "agent_session_stopped without its thread or user"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := slack.ParseEvent([]byte(c.body))
			require.NoError(t, err)
			require.Equal(t, slack.Ignored{Why: c.why}, got.Event)
		})
	}
	// An edit set to null is no edit.
	got, err := slack.ParseEvent([]byte(mention(`"user":"U1","ts":"1.2","channel":"C1","edited":null`)))
	require.NoError(t, err)
	require.IsType(t, slack.Mention{}, got.Event)
	// The envelope's enterprise stands in when the authorization names none.
	got, err = slack.ParseEvent([]byte(`{"type":"event_callback","event_id":"Ev1","team_id":"T9","enterprise_id":"E1","authorizations":[{"team_id":"T1"}],` +
		`"event":{"type":"app_mention","user":"U1","ts":"1.2","channel":"C1"}}`))
	require.NoError(t, err)
	require.Equal(t, slack.Conversation{TeamID: "T1", EnterpriseID: "E1", ChannelID: "C1", ThreadTS: "1.2"}, got.Event.(slack.Mention).At)
}

// The subscriptions are the events ParseEvent acts on: each one's fixture
// reads as an event, not as Ignored.
func TestSubscriptionsAreParsed(t *testing.T) {
	fixtures := map[string]string{
		"app_mention": "mention.json", "message.im": "dm.json", "app_uninstalled": "app_uninstalled.json",
		"tokens_revoked": "tokens_revoked.json", "agent_session_stopped": "agent_session_stopped.json",
	}
	require.Len(t, slack.Subscriptions, len(fixtures))
	for _, sub := range slack.Subscriptions {
		got, err := slack.ParseEvent(fixture(t, "events/"+fixtures[sub]))
		require.NoError(t, err, sub)
		_, ignored := got.Event.(slack.Ignored)
		require.False(t, ignored, sub)
	}
}

func TestEventKey(t *testing.T) {
	require.Equal(t, "e:Ev0MENTION1", slack.EventKey("Ev0MENTION1"))
}

func FuzzParseEvent(f *testing.F) {
	entries, err := os.ReadDir("testdata/events")
	require.NoError(f, err)
	for _, e := range entries {
		f.Add(fixture(f, "events/"+e.Name()))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		got, err := slack.ParseEvent(body)
		if err != nil {
			return
		}
		require.NotNil(t, got.Event)
		// What it acts on, it can store and read back as it was.
		if k, data, err := slack.EncodeEvent(got.Event); err == nil {
			back, err := slack.DecodeEvent(k, data)
			require.NoError(t, err)
			require.Equal(t, got.Event, back)
		}
	})
}

func ExampleParseEvent() {
	body := []byte(`{"type":"event_callback","event_id":"Ev1","event_time":1795000000,"team_id":"T1",
		"authorizations":[{"team_id":"T1"}],"is_ext_shared_channel":false,
		"event":{"type":"message","channel_type":"im","user":"U1","text":"hi","ts":"1795000000.000100","channel":"D1"}}`)
	cb, _ := slack.ParseEvent(body)
	dm := cb.Event.(slack.DirectMessage)
	fmt.Println(slack.EventKey(cb.EventID), dm.At.TeamID, dm.At.ChannelID, dm.At.ThreadTS, dm.UserID, dm.Text)
	// Output:
	// e:Ev1 T1 D1 1795000000.000100 U1 hi
}
