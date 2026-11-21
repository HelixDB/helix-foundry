package slack_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
)

func TestPayloadRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	mention := slack.Mention{At: slack.Conversation{TeamID: "T1", ChannelID: "C1", ThreadTS: "1.2"}, UserID: "U1", TS: "1.2", Text: "hi", EventTime: at}
	raw, err := slack.EventPayload(mention)
	require.NoError(t, err)
	p, err := slack.ReadPayload(raw)
	require.NoError(t, err)
	assert.Equal(t, slack.KindMention, p.Kind)
	got, err := slack.DecodeEvent(p.Kind, p.Data)
	require.NoError(t, err)
	assert.Equal(t, mention, got)

	stop := slack.Stop{Clicked: slack.Clicked{TeamID: "T1", UserID: "U1", ChannelID: "C1", MessageTS: "1.2", ActionTS: "1.3"}, TurnID: "01JTURN"}
	raw, err = slack.InteractionPayload(stop)
	require.NoError(t, err)
	p, err = slack.ReadPayload(raw)
	require.NoError(t, err)
	click, err := slack.DecodeInteraction(p.Kind, p.Data)
	require.NoError(t, err)
	assert.Equal(t, stop, click)
}

func TestPayloadRefusals(t *testing.T) {
	_, err := slack.EventPayload(slack.Ignored{Why: "a bot's message"})
	assert.Error(t, err, "Ignored isn't stored")
	_, err = slack.InteractionPayload(slack.Unhandled{Why: "a modal"})
	assert.Error(t, err, "Unhandled isn't stored")
	for name, raw := range map[string]string{
		"not JSON":     `{`,
		"no kind":      `{"data":{}}`,
		"another kind": `{"kind":"reaction","data":{}}`,
		"no data":      `{"kind":"dm"}`,
		"null data":    `{"kind":"dm","data":null}`,
	} {
		_, err := slack.ReadPayload([]byte(raw))
		assert.Error(t, err, name)
	}
}

func ExampleReadPayload() {
	raw, _ := slack.EventPayload(slack.Uninstalled{TeamID: "T1", EventTime: time.Unix(0, 0).UTC()})
	p, _ := slack.ReadPayload(raw)
	e, _ := slack.DecodeEvent(p.Kind, p.Data)
	fmt.Println(p.Kind, e.(slack.Uninstalled).TeamID)
	// Output: uninstalled T1
}
