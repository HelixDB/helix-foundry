// Package slack is the pure Slack half of Helix Foundry's Slack bot: what
// Slack sends Foundry and what Foundry sends back, with no store and no
// Founder in it.
//
//   - Verify checks a request's v0 signature within five minutes.
//   - ParseEvent and ParseInteraction read the events and interactivity
//     requests into closed sets of the events, clicks and the message
//     shortcut the bot acts on; everything else is Ignored or Unhandled.
//   - Site.Text makes model, document and tool strings safe mrkdwn (Mrkdwn):
//     escaped, links only to Foundry's own site, other URLs and domains in
//     inline code, never a mass mention.
//   - The Block Kit types marshal every mrkdwn text object with verbatim
//     set, and Client sends every message with parse=none and no unfurls.
//   - Client calls the Web API, through a Limiter per install and method and
//     per channel; a rate limit, Slack's or the Limiter's, is a
//     *RateLimitedError saying when to try again, so no caller sleeps.
//   - ParseEvent also reads the channel events the Slack data connector
//     keeps (FeedEvent), and Classify sorts a request for the door's gates
//     (bot, reply, feed) before it is verified.
//
// The manifests the dev and prod apps are made from are in manifest/; a test
// keeps their scopes and events equal to Required under each stamp's flags.
package slack

import "slices"

// Scopes are the bot scopes the app always asks for, and every install must
// hold: mentions and direct messages in, posting and agent status out, who
// a Slack user is (their email, for linking), and whether a channel a card
// is clicked in is shared with another organisation (conversations.info).
var Scopes = []string{
	"app_mentions:read",
	"im:history",
	"im:write",
	"chat:write",
	"assistant:write",
	"users:read",
	"users:read.email",
	"channels:read",
	"groups:read",
	"mpim:read",
}

// Subscriptions are the bot events the app always subscribes to: the events
// ParseEvent reads into an Event the bot acts on.
var Subscriptions = []string{
	"app_mention",
	"message.im",
	"app_uninstalled",
	"tokens_revoked",
	"agent_session_stopped",
}

// ShortcutScopes are the scopes of the app's message shortcut, "Ask Founder
// about this" (CallbackAskAbout): Slack shows an install an app's shortcuts
// only once it holds commands. Required asks for them, but nothing waits on
// them: an install without them answers as before, and the data connector
// never needs them (FeedScopes).
var ShortcutScopes = []string{"commands"}

// HistoryScopes are the scopes to read the messages of the public and
// private channels the bot is in, asked for only where the data connector
// (foundry.slack.feed) or unmentioned follow-ups (foundry.slack.follow) are
// on. Direct and group messages are never read beyond the bot's own DMs.
var HistoryScopes = []string{"channels:history", "groups:history"}

// MessageSubscriptions are the events of every message in the channels the
// bot is in, subscribed to with HistoryScopes.
var MessageSubscriptions = []string{"message.channels", "message.groups"}

// ChannelSubscriptions are the channel lifecycle events the data connector
// reads (ParseEvent's ChannelRenamed, ChannelGone, ChannelArchived,
// MemberJoined and MemberLeft), subscribed to only where it is on. They need
// only channels:read and groups:read, which Scopes holds. channel_unshared
// isn't among them: the connector's hourly directory read finds a channel
// no longer shared.
var ChannelSubscriptions = []string{
	"member_joined_channel",
	"member_left_channel",
	"channel_left",
	"group_left",
	"channel_rename",
	"group_rename",
	"channel_deleted",
	"group_deleted",
	"channel_shared",
	"channel_archive",
	"channel_unarchive",
	"group_archive",
	"group_unarchive",
}

// Required is what an install must hold and subscribe to where the data
// connector is on (feed) and where unmentioned follow-ups are (follow):
// Scopes, ShortcutScopes and Subscriptions always; HistoryScopes and
// MessageSubscriptions with either; ChannelSubscriptions with feed. The
// slices are the caller's own.
func Required(feed, follow bool) (scopes, events []string) {
	scopes, events = slices.Concat(Scopes, ShortcutScopes), slices.Clone(Subscriptions)
	if feed || follow {
		scopes, events = append(scopes, HistoryScopes...), append(events, MessageSubscriptions...)
	}
	if feed {
		events = append(events, ChannelSubscriptions...)
	}
	return scopes, events
}

// FeedScopes are the scopes of Required the data connector reads with: all
// but ShortcutScopes, so an install that lacks only those keeps syncing.
// The slice is the caller's own.
func FeedScopes(feed, follow bool) []string {
	scopes, _ := Required(feed, follow)
	return slices.DeleteFunc(scopes, func(s string) bool { return slices.Contains(ShortcutScopes, s) })
}
