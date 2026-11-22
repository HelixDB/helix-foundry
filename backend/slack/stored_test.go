package slack_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
)

// Every stored kind reads back as what was stored, and each kind belongs
// to events or clicks, never both.
func TestEncodeDecode(t *testing.T) {
	at := slack.Conversation{TeamID: "T1", ChannelID: "C1", ThreadTS: "1.2", ExtShared: true}
	clicked := slack.Clicked{TeamID: "T1", EnterpriseID: "E1", UserID: "U1", ChannelID: "C1", MessageTS: "1.2", ThreadTS: "1.1", Ephemeral: true, ActionTS: "3.4"}
	events := map[slack.Kind]slack.Event{
		slack.KindMention:       slack.Mention{At: at, UserID: "U1", TS: "1.2", UserTeamID: "T1", Text: "<@U9> hi", EventTime: eventTime},
		slack.KindDM:            slack.DirectMessage{At: at, UserID: "U1", TS: "1.2", Text: "hi", EventTime: eventTime},
		slack.KindReply:         slack.ThreadReply{At: at, UserID: "U1", TS: "1.3", UserTeamID: "T1", Text: "and then?", EventTime: eventTime},
		slack.KindStopped:       slack.SessionStopped{At: at, UserID: "U1"},
		slack.KindUninstalled:   slack.Uninstalled{TeamID: "T1", EventTime: eventTime},
		slack.KindTokensRevoked: slack.TokensRevoked{TeamID: "T1", Bot: true, EventTime: eventTime},
	}
	clicks := map[slack.Kind]slack.Interaction{
		slack.KindApprove: slack.Approve{Clicked: clicked, ActionID: actionID, PayloadHash: payloadHash},
		slack.KindDismiss: slack.Dismiss{Clicked: clicked, ActionID: actionID},
		slack.KindRefresh: slack.Refresh{Clicked: clicked, ActionID: actionID},
		slack.KindReview:  slack.Review{Clicked: clicked, ActionID: actionID, PayloadHash: payloadHash},
		slack.KindRun:     slack.Run{Clicked: clicked, ActionID: actionID, PayloadHash: payloadHash, SpecHash: specHash},
		slack.KindStop:    slack.Stop{Clicked: clicked, TurnID: turnID},
		slack.KindUnlink:  slack.Unlink{Clicked: clicked},
	}
	for want, e := range events {
		k, data, err := slack.EncodeEvent(e)
		require.NoError(t, err)
		require.Equal(t, want, k)
		parsed, err := slack.ParseKind(string(k))
		require.NoError(t, err)
		back, err := slack.DecodeEvent(parsed, data)
		require.NoError(t, err)
		require.Equal(t, e, back)
		_, err = slack.DecodeInteraction(k, data)
		require.Error(t, err)
		_, err = slack.DecodeEvent(k, []byte("{"))
		require.Error(t, err)
	}
	for want, in := range clicks {
		k, data, err := slack.EncodeInteraction(in)
		require.NoError(t, err)
		require.Equal(t, want, k)
		parsed, err := slack.ParseKind(string(k))
		require.NoError(t, err)
		back, err := slack.DecodeInteraction(parsed, data)
		require.NoError(t, err)
		require.Equal(t, in, back)
		_, err = slack.DecodeEvent(k, data)
		require.Error(t, err)
		_, err = slack.DecodeInteraction(k, []byte("{"))
		require.Error(t, err)
	}
	for _, e := range []slack.Event{slack.Ignored{Why: "x"}, slack.URLVerification{Challenge: "c"}, nil,
		slack.ChannelMessage{}, slack.RateLimited{TeamID: "T1"}} {
		_, _, err := slack.EncodeEvent(e)
		require.Error(t, err)
	}
	for _, in := range []slack.Interaction{slack.Unhandled{Why: "x"}, nil} {
		_, _, err := slack.EncodeInteraction(in)
		require.Error(t, err)
	}
	for _, bad := range []string{"", "Mention", "ignored", "click"} {
		_, err := slack.ParseKind(bad)
		require.Error(t, err, bad)
	}
}
