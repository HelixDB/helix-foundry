package slack_test

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
)

const (
	actionID = "01JBACTXN00000000000000000"
	turnID   = "01JBTVRN000000000000000000"
)

var (
	payloadHash = strings.Repeat("a", 64)
	specHash    = strings.Repeat("b", 64)
)

// form is a fixture's payload as Slack posts it: a form field.
func form(t testing.TB, name string) []byte {
	return []byte(url.Values{"payload": {string(fixture(t, "interactions/"+name))}}.Encode())
}

// Every fixture reads as the click the bot acts on, or as Unhandled.
func TestParseInteractionFixtures(t *testing.T) {
	clicked := slack.Clicked{TeamID: "T0AAA1111", UserID: "U0ADA0001", ChannelID: "C0GEN0001",
		MessageTS: "1795000050.000100", ThreadTS: "1795000000.000100", ActionTS: "1795000100.123456"}
	ephemeral := clicked
	ephemeral.ThreadTS, ephemeral.Ephemeral = "", true
	grid := clicked
	grid.TeamID, grid.EnterpriseID, grid.UserID = "T0GRIDWS01", "E0ACME001", "W0GRID0001"
	// A shortcut is on a message in a thread, or at top level: its time is
	// the shortcut's action_ts.
	shortcut := clicked
	topLevel := clicked
	topLevel.ThreadTS = ""
	cases := map[string]slack.Interaction{
		"approve.json":         slack.Approve{Clicked: clicked, ActionID: actionID, PayloadHash: payloadHash},
		"approve_grid.json":    slack.Approve{Clicked: grid, ActionID: actionID, PayloadHash: payloadHash},
		"dismiss.json":         slack.Dismiss{Clicked: clicked, ActionID: actionID},
		"refresh.json":         slack.Refresh{Clicked: clicked, ActionID: actionID},
		"review.json":          slack.Review{Clicked: clicked, ActionID: actionID, PayloadHash: payloadHash},
		"run_ephemeral.json":   slack.Run{Clicked: ephemeral, ActionID: actionID, PayloadHash: payloadHash, SpecHash: specHash},
		"run_no_spec.json":     slack.Run{Clicked: clicked, ActionID: actionID, PayloadHash: payloadHash},
		"stop.json":            slack.Stop{Clicked: clicked, TurnID: turnID},
		"unlink.json":          slack.Unlink{Clicked: clicked},
		"link_button.json":     slack.Unhandled{Why: `action "foundry.link"`},
		"view_submission.json": slack.Unhandled{Why: `interaction type "view_submission"`},
		"org_install.json":     slack.Unhandled{Why: "an org-wide install"},
		"unknown_action.json":  slack.Unhandled{Why: "a button value the bot didn't write"},
		"forged_value.json":    slack.Unhandled{Why: "a foundry.approve value the bot didn't write"},
		"not_json_value.json":  slack.Unhandled{Why: "a button value the bot didn't write"},
		"two_actions.json":     slack.Unhandled{Why: "2 actions"},
		"in_view.json":         slack.Unhandled{Why: `a click in a "view"`},
		"ask_about.json":       slack.AskAbout{Clicked: shortcut, AuthorID: "U0GRACE01", Text: "Is <@U0ALAN001> renewing *Globex*?"},
		"ask_about_app.json":   slack.AskAbout{Clicked: topLevel, Text: "Deploy failed"},
		"other_shortcut.json":  slack.Unhandled{Why: `shortcut "someone.else"`},
	}
	entries, err := os.ReadDir("testdata/interactions")
	require.NoError(t, err)
	require.Len(t, cases, len(entries), "every fixture has a case")
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := slack.ParseInteraction(form(t, name))
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

// A value names exactly what its button acts on: nothing missing, nothing
// more, nothing malformed.
func TestParseInteractionValues(t *testing.T) {
	click := func(action, value string) string {
		v := ""
		if value != "" {
			v = `,"value":` + quote(value)
		}
		return url.Values{"payload": {`{"type":"block_actions","user":{"id":"U1"},"team":{"id":"T1"},` +
			`"container":{"type":"message","message_ts":"1.2","channel_id":"C1"},` +
			`"actions":[{"action_id":"` + action + `","action_ts":"3.4"` + v + `}]}`}}.Encode()
	}
	a, h, s, tu := `"a":"`+actionID+`"`, `"h":"`+payloadHash+`"`, `"s":"`+specHash+`"`, `"t":"`+turnID+`"`
	for name, c := range map[string]struct {
		action, value string
		ok            bool
	}{
		"approve":                 {slack.ActionApprove, "{" + a + "," + h + "}", true},
		"approve without hash":    {slack.ActionApprove, "{" + a + "}", false},
		"approve with spec":       {slack.ActionApprove, "{" + a + "," + h + "," + s + "}", false},
		"approve with turn":       {slack.ActionApprove, "{" + a + "," + h + "," + tu + "}", false},
		"approve lowercase id":    {slack.ActionApprove, `{"a":"` + strings.ToLower(actionID) + `",` + h + "}", false},
		"dismiss":                 {slack.ActionDismiss, "{" + a + "}", true},
		"dismiss with hash":       {slack.ActionDismiss, "{" + a + "," + h + "}", false},
		"dismiss without action":  {slack.ActionDismiss, "{}", false},
		"refresh with spec":       {slack.ActionRefresh, "{" + a + "," + s + "}", false},
		"review without hash":     {slack.ActionReview, "{" + a + "}", false},
		"run":                     {slack.ActionRun, "{" + a + "," + h + "," + s + "}", true},
		"run bad spec":            {slack.ActionRun, "{" + a + "," + h + `,"s":"x"}`, false},
		"stop":                    {slack.ActionStop, "{" + tu + "}", true},
		"stop with action":        {slack.ActionStop, "{" + tu + "," + a + "}", false},
		"stop bad turn":           {slack.ActionStop, `{"t":"x"}`, false},
		"unlink":                  {slack.ActionUnlink, "", true},
		"unlink empty object":     {slack.ActionUnlink, "{}", true},
		"unlink with value":       {slack.ActionUnlink, "{" + a + "}", false},
		"value too long":          {slack.ActionDismiss, "{" + a + `,"x":"` + strings.Repeat("x", slack.MaxValue) + `"}`, false},
		"value wrong field types": {slack.ActionDismiss, `{"a":1}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := slack.ParseInteraction([]byte(click(c.action, c.value)))
			require.NoError(t, err)
			_, unhandled := got.(slack.Unhandled)
			require.Equal(t, c.ok, !unhandled, "%#v", got)
		})
	}
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// Who, where and when are all checked before the value.
func TestParseInteractionClickFields(t *testing.T) {
	click := func(user, team, container, action string) []byte {
		return []byte(url.Values{"payload": {`{"type":"block_actions","user":{"id":"` + user + `"},"team":` + team + `,` +
			`"container":` + container + `,"channel":{"id":"C9"},"actions":[` + action + `]}`}}.Encode())
	}
	okContainer := `{"type":"message","message_ts":"1.2","channel_id":"C1"}`
	okAction := `{"action_id":"foundry.unlink","action_ts":"3.4"}`
	for name, body := range map[string][]byte{
		"bad user":       click("u", `{"id":"T1"}`, okContainer, okAction),
		"bad team":       click("U1", `{"id":""}`, okContainer, okAction),
		"bad enterprise": click("U1", `{"id":"T1","enterprise_id":"e"}`, okContainer, okAction),
		"bad message ts": click("U1", `{"id":"T1"}`, `{"type":"message","message_ts":"x","channel_id":"C1"}`, okAction),
		"bad thread ts":  click("U1", `{"id":"T1"}`, `{"type":"message","message_ts":"1.2","thread_ts":"x","channel_id":"C1"}`, okAction),
		"bad action ts":  click("U1", `{"id":"T1"}`, okContainer, `{"action_id":"foundry.unlink","action_ts":""}`),
		"no actions":     click("U1", `{"id":"T1"}`, okContainer, ""),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := slack.ParseInteraction(body)
			require.NoError(t, err)
			require.IsType(t, slack.Unhandled{}, got)
		})
	}
	// The payload's channel stands in for a container without one, and the
	// payload's enterprise for a team without one.
	got, err := slack.ParseInteraction([]byte(url.Values{"payload": {`{"type":"block_actions","user":{"id":"U1"},"team":{"id":"T1"},` +
		`"enterprise":{"id":"E1"},"container":{"type":"message","message_ts":"1.2"},"channel":{"id":"C9"},"actions":[` + okAction + `]}`}}.Encode()))
	require.NoError(t, err)
	require.Equal(t, slack.Unlink{Clicked: slack.Clicked{TeamID: "T1", EnterpriseID: "E1", UserID: "U1", ChannelID: "C9", MessageTS: "1.2", ActionTS: "3.4"}}, got)
}

// A shortcut names its team, user, channel, message and time, and an author
// only as a user id; its enterprise is the team's, else the payload's.
func TestParseShortcutFields(t *testing.T) {
	shortcut := func(fields string) []byte {
		return []byte(url.Values{"payload": {`{"type":"message_action","callback_id":"foundry.ask_about",` + fields + `}`}}.Encode())
	}
	const (
		who  = `"user":{"id":"U1"},"team":{"id":"T1"},"channel":{"id":"C1"}`
		when = `"message_ts":"1.2","action_ts":"3.4"`
		msg  = `"message":{"user":"U2","text":"hi","thread_ts":"1.1"}`
	)
	for name, c := range map[string]struct {
		fields string
		why    string
	}{
		"no message":         {who + "," + when, "a shortcut without its message"},
		"bad user":           {`"user":{"id":"u"},"team":{"id":"T1"},"channel":{"id":"C1"},` + when + "," + msg, "a shortcut without its team, user, channel or author"},
		"no channel":         {`"user":{"id":"U1"},"team":{"id":"T1"},` + when + "," + msg, "a shortcut without its team, user, channel or author"},
		"bad enterprise":     {`"user":{"id":"U1"},"team":{"id":"T1","enterprise_id":"e"},"channel":{"id":"C1"},` + when + "," + msg, "a shortcut without its team, user, channel or author"},
		"bad author":         {who + "," + when + `,"message":{"user":"<@U2>","text":"hi"}`, "a shortcut without its team, user, channel or author"},
		"bad message ts":     {who + `,"message_ts":"x","action_ts":"3.4",` + msg, "a shortcut without its message or time"},
		"no action ts":       {who + `,"message_ts":"1.2",` + msg, "a shortcut without its message or time"},
		"bad thread ts":      {who + "," + when + `,"message":{"user":"U2","text":"hi","thread_ts":"x"}`, "a shortcut without its message or time"},
		"org-wide install":   {who + "," + when + "," + msg + `,"is_enterprise_install":true`, "an org-wide install"},
		"no team":            {`"user":{"id":"U1"},"channel":{"id":"C1"},` + when + "," + msg, "an org-wide install"},
		"another shortcut":   {`"callback_id":"x",` + who + "," + when + "," + msg, `shortcut "x"`},
		"no callback at all": {`"callback_id":"",` + who + "," + when + "," + msg, `shortcut ""`},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := slack.ParseInteraction(shortcut(c.fields))
			require.NoError(t, err)
			require.Equal(t, slack.Unhandled{Why: c.why}, got)
		})
	}
	got, err := slack.ParseInteraction(shortcut(who + "," + when + "," + msg + `,"enterprise":{"id":"E1"}`))
	require.NoError(t, err)
	require.Equal(t, slack.AskAbout{Clicked: slack.Clicked{TeamID: "T1", EnterpriseID: "E1", UserID: "U1", ChannelID: "C1", MessageTS: "1.2",
		ThreadTS: "1.1", ActionTS: "3.4"}, AuthorID: "U2", Text: "hi"}, got)
}

func TestParseInteractionRefusals(t *testing.T) {
	for name, body := range map[string]string{
		"bad form":     "%zz",
		"no payload":   "x=1",
		"payload json": url.Values{"payload": {"{"}}.Encode(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := slack.ParseInteraction([]byte(body))
			require.Error(t, err)
		})
	}
}

// A click's key is the same for Slack's two deliveries of it and differs
// for another button, user or time; Unhandled has none.
func TestInteractionKey(t *testing.T) {
	approve, err := slack.ParseInteraction(form(t, "approve.json"))
	require.NoError(t, err)
	key := slack.InteractionKey(approve)
	require.Len(t, key, 66)
	require.True(t, strings.HasPrefix(key, "i:"))
	again, err := slack.ParseInteraction(form(t, "approve.json"))
	require.NoError(t, err)
	require.Equal(t, key, slack.InteractionKey(again))
	seen := map[string]bool{key: true}
	for _, name := range []string{"dismiss.json", "refresh.json", "review.json", "run_no_spec.json", "stop.json", "unlink.json", "approve_grid.json",
		"ask_about.json"} {
		in, err := slack.ParseInteraction(form(t, name))
		require.NoError(t, err)
		k := slack.InteractionKey(in)
		require.False(t, seen[k], name)
		seen[k] = true
	}
	later := approve.(slack.Approve)
	later.ActionTS = "1795000101.000000"
	require.NotEqual(t, key, slack.InteractionKey(later))
	require.Empty(t, slack.InteractionKey(slack.Unhandled{}))
	ask, err := slack.ParseInteraction(form(t, "ask_about.json"))
	require.NoError(t, err)
	app, err := slack.ParseInteraction(form(t, "ask_about_app.json"))
	require.NoError(t, err)
	require.Equal(t, slack.InteractionKey(ask), slack.InteractionKey(app), "one pick of the shortcut, whatever its message says")
}

func TestButtonValueString(t *testing.T) {
	require.Equal(t, `{"a":"`+actionID+`","h":"`+payloadHash+`"}`, slack.ButtonValue{Action: actionID, PayloadHash: payloadHash}.String())
	require.Equal(t, `{}`, slack.ButtonValue{}.String())
}

func FuzzParseInteraction(f *testing.F) {
	entries, err := os.ReadDir("testdata/interactions")
	require.NoError(f, err)
	for _, e := range entries {
		f.Add(string(fixture(f, "interactions/"+e.Name())))
	}
	f.Fuzz(func(t *testing.T, payload string) {
		got, err := slack.ParseInteraction([]byte(url.Values{"payload": {payload}}.Encode()))
		if err != nil {
			return
		}
		require.NotNil(t, got)
		if k, data, err := slack.EncodeInteraction(got); err == nil {
			back, err := slack.DecodeInteraction(k, data)
			require.NoError(t, err)
			require.Equal(t, got, back)
			require.NotEmpty(t, slack.InteractionKey(got))
		}
	})
}

func ExampleParseInteraction() {
	value := slack.ButtonValue{Turn: "01JBTVRN000000000000000000"}.String()
	payload := `{"type":"block_actions","user":{"id":"U1"},"team":{"id":"T1"},` +
		`"container":{"type":"message","message_ts":"1.2","channel_id":"C1"},` +
		`"actions":[{"action_id":"foundry.stop","action_ts":"3.4","value":` + strconv.Quote(value) + `}]}`
	in, _ := slack.ParseInteraction([]byte(url.Values{"payload": {payload}}.Encode()))
	stop := in.(slack.Stop)
	fmt.Println(stop.UserID, stop.ChannelID, stop.TurnID)
	// Output:
	// U1 C1 01JBTVRN000000000000000000
}
