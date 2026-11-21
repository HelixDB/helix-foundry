package slack_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
)

// card is a message using every block and element.
func card(t testing.TB) slack.Message {
	s := site(t)
	page, err := s.Page("/slack/link#t=x")
	require.NoError(t, err)
	return slack.Message{
		Text: s.Text("Delete the dashboard Revenue?"),
		Blocks: []slack.Block{
			slack.Header{Text: "Delete a dashboard"},
			slack.Section{Text: slack.Bold(s.Text("Revenue")), Fields: []slack.Mrkdwn{s.Text("Tiles: 4"), s.Text("Owner: ada@acme.com")}},
			slack.Section{Fields: []slack.Mrkdwn{s.Text("only fields")}, Accessory: &slack.Button{ActionID: slack.ActionLink, Text: "Open", URL: &page}},
			slack.Divider{},
			slack.Actions{Elements: []slack.Button{
				{ActionID: slack.ActionApprove, Text: "Run", Value: slack.ButtonValue{Action: actionID, PayloadHash: payloadHash}, Style: slack.StyleDanger,
					Confirm: &slack.Confirm{Title: "Delete?", Text: s.Text("This can't be undone."), Confirm: "Delete", Deny: "Cancel", Style: slack.StyleDanger}},
				{ActionID: slack.ActionDismiss, Text: "Dismiss", Value: slack.ButtonValue{Action: actionID}},
				{ActionID: slack.ActionUnlink, Text: "Unlink"},
			}},
			slack.Context{Elements: []slack.Mrkdwn{s.Text("Only "), slack.UserMention("U0ADA0001")}},
		},
	}
}

func TestBlocksJSON(t *testing.T) {
	m := card(t)
	require.NoError(t, m.Check())
	got, err := json.Marshal(m.Blocks)
	require.NoError(t, err)
	want := `[` +
		`{"type":"header","text":{"type":"plain_text","text":"Delete a dashboard","emoji":true}},` +
		`{"type":"section","text":{"type":"mrkdwn","text":"*Revenue*","verbatim":true},"fields":[{"type":"mrkdwn","text":"Tiles: 4","verbatim":true},{"type":"mrkdwn","text":"Owner: ` + "`ada@acme.com`" + `","verbatim":true}]},` +
		`{"type":"section","fields":[{"type":"mrkdwn","text":"only fields","verbatim":true}],"accessory":{"type":"button","action_id":"foundry.link","text":{"type":"plain_text","text":"Open","emoji":true},"url":"https://foundry.helix-db.com/slack/link#t=x"}},` +
		`{"type":"divider"},` +
		`{"type":"actions","elements":[` +
		`{"type":"button","action_id":"foundry.approve","text":{"type":"plain_text","text":"Run","emoji":true},"value":"{\"a\":\"` + actionID + `\",\"h\":\"` + payloadHash + `\"}","style":"danger",` +
		`"confirm":{"title":{"type":"plain_text","text":"Delete?","emoji":true},"text":{"type":"mrkdwn","text":"This can't be undone.","verbatim":true},"confirm":{"type":"plain_text","text":"Delete","emoji":true},"deny":{"type":"plain_text","text":"Cancel","emoji":true},"style":"danger"}},` +
		`{"type":"button","action_id":"foundry.dismiss","text":{"type":"plain_text","text":"Dismiss","emoji":true},"value":"{\"a\":\"` + actionID + `\"}"},` +
		`{"type":"button","action_id":"foundry.unlink","text":{"type":"plain_text","text":"Unlink","emoji":true}}]},` +
		`{"type":"context","elements":[{"type":"mrkdwn","text":"Only ","verbatim":true},{"type":"mrkdwn","text":"<@U0ADA0001>","verbatim":true}]}` +
		`]`
	require.JSONEq(t, want, string(got))
	requireVerbatim(t, got)
}

// requireVerbatim fails unless every mrkdwn text object in the JSON has
// verbatim set.
func requireVerbatim(t testing.TB, data []byte) {
	t.Helper()
	var doc any
	require.NoError(t, json.Unmarshal(data, &doc))
	found := 0
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if v["type"] == "mrkdwn" {
				found++
				require.Equal(t, true, v["verbatim"], "%v", v)
			}
			for _, e := range v {
				walk(e)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(doc)
	require.NotZero(t, found)
}

func TestMessageCheck(t *testing.T) {
	s := site(t)
	ok := func(blocks ...slack.Block) slack.Message { return slack.Message{Text: s.Text("x"), Blocks: blocks} }
	text := s.Text("x")
	long := func(n int) slack.Mrkdwn { return s.Text(strings.Repeat("x", n)) }
	button := slack.Button{ActionID: slack.ActionDismiss, Text: "Dismiss"}
	confirming := func(c slack.Confirm) slack.Message {
		return ok(slack.Actions{Elements: []slack.Button{{ActionID: slack.ActionDismiss, Text: "x", Confirm: &c}}})
	}
	longPage, err := s.Page("/" + strings.Repeat("x", slack.MaxButtonURL))
	require.NoError(t, err)
	page, err := s.Page("/" + strings.Repeat("x", slack.MaxButtonURL-len(origin)-1))
	require.NoError(t, err)
	// Each limit holds at its edge.
	require.NoError(t, confirming(slack.Confirm{Title: slack.Plain(strings.Repeat("é", slack.MaxConfirmTitle)), Text: long(slack.MaxConfirmText),
		Confirm: slack.Plain(strings.Repeat("x", slack.MaxConfirmButton)), Deny: slack.Plain(strings.Repeat("x", slack.MaxConfirmButton))}).Check())
	require.NoError(t, ok(slack.Actions{Elements: []slack.Button{{ActionID: slack.ActionOpen, Text: "x", URL: &page}}}).Check())
	require.NoError(t, ok().Check())
	require.NoError(t, ok(slack.Section{Text: long(slack.MaxSectionText)}).Check())
	many := make([]slack.Block, slack.MaxBlocks)
	for i := range many {
		many[i] = slack.Divider{}
	}
	require.NoError(t, ok(many...).Check())
	for name, m := range map[string]slack.Message{
		"no text":            {Blocks: []slack.Block{slack.Divider{}}},
		"text too long":      {Text: long(slack.MaxMessageText + 1)},
		"too many blocks":    ok(append(many, slack.Divider{})...),
		"nil block":          ok(nil),
		"empty section":      ok(slack.Section{}),
		"long section":       ok(slack.Section{Text: long(slack.MaxSectionText + 1)}),
		"too many fields":    ok(slack.Section{Fields: make([]slack.Mrkdwn, slack.MaxSectionFields+1)}),
		"empty field":        ok(slack.Section{Text: text, Fields: []slack.Mrkdwn{{}}}),
		"long field":         ok(slack.Section{Text: text, Fields: []slack.Mrkdwn{long(slack.MaxFieldText + 1)}}),
		"bad accessory":      ok(slack.Section{Text: text, Accessory: &slack.Button{ActionID: "x", Text: "x"}}),
		"empty context":      ok(slack.Context{}),
		"empty context el":   ok(slack.Context{Elements: []slack.Mrkdwn{{}}}),
		"long context el":    ok(slack.Context{Elements: []slack.Mrkdwn{long(slack.MaxSectionText + 1)}}),
		"too much context":   ok(slack.Context{Elements: make([]slack.Mrkdwn, slack.MaxContextItems+1)}),
		"empty header":       ok(slack.Header{}),
		"long header":        ok(slack.Header{Text: slack.Plain(strings.Repeat("é", slack.MaxHeaderText+1))}),
		"no buttons":         ok(slack.Actions{}),
		"too many buttons":   ok(slack.Actions{Elements: make([]slack.Button, slack.MaxActionElements+1)}),
		"foreign action":     ok(slack.Actions{Elements: []slack.Button{{ActionID: "other", Text: "x"}}}),
		"no button text":     ok(slack.Actions{Elements: []slack.Button{{ActionID: slack.ActionDismiss}}}),
		"long button text":   ok(slack.Actions{Elements: []slack.Button{{ActionID: slack.ActionDismiss, Text: slack.Plain(strings.Repeat("x", slack.MaxButtonText+1))}}}),
		"long value":         ok(slack.Actions{Elements: []slack.Button{{ActionID: slack.ActionDismiss, Text: "x", Value: slack.ButtonValue{Action: strings.Repeat("x", slack.MaxValue)}}}}),
		"bad style":          ok(slack.Actions{Elements: []slack.Button{{ActionID: slack.ActionDismiss, Text: "x", Style: "green"}}}),
		"half confirmation":  ok(slack.Actions{Elements: []slack.Button{{ActionID: slack.ActionDismiss, Text: "x", Confirm: &slack.Confirm{Title: "x"}}}}),
		"long confirm title": confirming(slack.Confirm{Title: slack.Plain(strings.Repeat("é", slack.MaxConfirmTitle+1)), Text: text, Confirm: "x", Deny: "x"}),
		"long confirm text":  confirming(slack.Confirm{Title: "x", Text: long(slack.MaxConfirmText + 1), Confirm: "x", Deny: "x"}),
		"long confirm":       confirming(slack.Confirm{Title: "x", Text: text, Confirm: slack.Plain(strings.Repeat("x", slack.MaxConfirmButton+1)), Deny: "x"}),
		"long deny":          confirming(slack.Confirm{Title: "x", Text: text, Confirm: "x", Deny: slack.Plain(strings.Repeat("x", slack.MaxConfirmButton+1))}),
		"long URL":           ok(slack.Actions{Elements: []slack.Button{{ActionID: slack.ActionOpen, Text: "x", URL: &longPage}}}),
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, m.Check())
		})
	}
	require.NoError(t, ok(slack.Actions{Elements: []slack.Button{button}}).Check())
}

func ExampleMrkdwn_MarshalJSON() {
	site, _ := slack.NewSite("https://foundry.helix-db.com")
	b, _ := json.Marshal(slack.Section{Text: site.Text("Linked to ada@acme.com")})
	fmt.Println(string(b))
	// Output:
	// {"type":"section","text":{"type":"mrkdwn","text":"Linked to `ada@acme.com`","verbatim":true}}
}
