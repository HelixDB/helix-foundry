// Package slack is the pure Slack half of Helix Foundry's Slack bot: what
// Slack sends Foundry and what Foundry sends back, with no store and no
// Founder in it.
//
//   - Verify checks a request's v0 signature within five minutes.
//   - ParseEvent and ParseInteraction read the events and interactivity
//     requests into closed sets of the events and clicks the bot acts on;
//     everything else is Ignored or Unhandled.
//   - Site.Text makes model, document and tool strings safe mrkdwn (Mrkdwn):
//     escaped, links only to Foundry's own site, other URLs and domains in
//     inline code, never a mass mention.
//   - The Block Kit types marshal every mrkdwn text object with verbatim
//     set, and Client sends every message with parse=none and no unfurls.
//   - Client calls the Web API, through a Limiter per install and method and
//     per channel; a rate limit, Slack's or the Limiter's, is a
//     *RateLimitedError saying when to try again, so no caller sleeps.
//
// The manifests the dev and prod apps are made from are in manifest/; a test
// keeps their scopes and events equal to Scopes and Subscriptions.
package slack

// Scopes are the bot scopes the app asks for, and every install must hold:
// mentions and direct messages in, posting and agent status out, and who a
// Slack user is (their email, for linking).
var Scopes = []string{
	"app_mentions:read",
	"im:history",
	"im:write",
	"chat:write",
	"assistant:write",
	"users:read",
	"users:read.email",
}

// Subscriptions are the bot events the app subscribes to: the events
// ParseEvent reads into an Event it acts on.
var Subscriptions = []string{
	"app_mention",
	"message.im",
	"app_uninstalled",
	"tokens_revoked",
	"agent_session_stopped",
}
