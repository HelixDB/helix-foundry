package slack_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
)

// The apps are made from the manifests: each asks for exactly the scopes
// and events the bot needs under its stamp's flags (prod: the data connector
// and unmentioned follow-ups off, so no channel history; dev: both on),
// keeps token rotation and org-wide installs off, and sends Slack's
// requests to its stamp's door.
func TestManifests(t *testing.T) {
	for file, want := range map[string]struct {
		name, ingest string
		redirects    []any
		feed, follow bool
	}{
		"prod.yaml": {name: "Helix Foundry", ingest: "https://ingest.helix-db.com", redirects: []any{"https://foundry.helix-db.com/slack/installed"}},
		"dev.yaml": {name: "Helix Foundry (dev)", ingest: "https://ingest.dev.helix-db.com", redirects: []any{
			"https://dev.foundry.helix-db.com/slack/installed", "https://local.foundry.helix-db.com:3000/slack/installed"}, feed: true, follow: true},
	} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("manifest", file))
			require.NoError(t, err)
			m, err := yaml.Parser().Unmarshal(raw)
			require.NoError(t, err)
			get := func(path ...string) any {
				var v any = m
				for _, key := range path {
					obj, ok := v.(map[string]any)
					require.True(t, ok, "%v", path)
					v = obj[key]
				}
				return v
			}
			strs := func(path ...string) []string {
				var out []string
				for _, v := range get(path...).([]any) {
					out = append(out, v.(string))
				}
				return out
			}
			require.Equal(t, want.name, get("display_information", "name"))
			scopes, events := slack.Required(want.feed, want.follow)
			require.Equal(t, sorted(scopes), sorted(strs("oauth_config", "scopes", "bot")))
			require.Nil(t, get("oauth_config", "scopes", "user"), "no user scopes")
			require.Equal(t, sorted(events), sorted(strs("settings", "event_subscriptions", "bot_events")))
			require.Nil(t, get("settings", "event_subscriptions", "user_events"))
			require.Equal(t, false, get("settings", "token_rotation_enabled"))
			require.Equal(t, false, get("settings", "org_deploy_enabled"))
			require.Equal(t, false, get("settings", "socket_mode_enabled"))
			require.Equal(t, true, get("settings", "interactivity", "is_enabled"))
			require.Equal(t, want.ingest+"/v1/slack/events", get("settings", "event_subscriptions", "request_url"))
			require.Equal(t, want.ingest+"/v1/slack/interactivity", get("settings", "interactivity", "request_url"))
			require.Equal(t, want.redirects, get("oauth_config", "redirect_urls"))
			require.Nil(t, get("features", "assistant_view"), "agent_view replaces it")
			require.NotEmpty(t, get("features", "agent_view", "agent_description"))
			require.LessOrEqual(t, len(get("features", "agent_view", "agent_description").(string)), 300)
			require.LessOrEqual(t, len(get("features", "agent_view", "suggested_prompts").([]any)), 4)
			require.Equal(t, false, get("features", "app_home", "home_tab_enabled"))
			require.Equal(t, true, get("features", "app_home", "messages_tab_enabled"))
		})
	}
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// Required adds channel history for either flag, and the channel events for
// the data connector alone; it never asks for group DMs' history, and its
// slices are the caller's own.
func TestRequired(t *testing.T) {
	for _, c := range []struct {
		feed, follow bool
		scopes       []string
		events       []string
	}{
		{scopes: slack.Scopes, events: slack.Subscriptions},
		{follow: true, scopes: slices.Concat(slack.Scopes, slack.HistoryScopes), events: slices.Concat(slack.Subscriptions, slack.MessageSubscriptions)},
		{feed: true, scopes: slices.Concat(slack.Scopes, slack.HistoryScopes),
			events: slices.Concat(slack.Subscriptions, slack.MessageSubscriptions, slack.ChannelSubscriptions)},
		{feed: true, follow: true, scopes: slices.Concat(slack.Scopes, slack.HistoryScopes),
			events: slices.Concat(slack.Subscriptions, slack.MessageSubscriptions, slack.ChannelSubscriptions)},
	} {
		scopes, events := slack.Required(c.feed, c.follow)
		require.Equal(t, c.scopes, scopes, "feed %v follow %v", c.feed, c.follow)
		require.Equal(t, c.events, events, "feed %v follow %v", c.feed, c.follow)
		require.NotContains(t, scopes, "mpim:history")
		require.NotContains(t, events, "message.mpim")
		require.Len(t, slices.Compact(sorted(events)), len(events), "no event twice")
		scopes[0], events[0] = "changed", "changed"
		require.NotEqual(t, "changed", slack.Scopes[0])
		require.NotEqual(t, "changed", slack.Subscriptions[0])
	}
}

func ExampleRequired() {
	scopes, events := slack.Required(false, true)
	fmt.Println(scopes[len(scopes)-2:], events[len(events)-2:])
	// Output:
	// [channels:history groups:history] [message.channels message.groups]
}
