package slack_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
)

// The apps are made from the manifests: each asks for exactly the scopes
// and events the bot needs, keeps token rotation and org-wide installs off,
// and sends Slack's requests to its stamp's door.
func TestManifests(t *testing.T) {
	for file, want := range map[string]struct {
		name, ingest string
		redirects    []any
	}{
		"prod.yaml": {"Helix Foundry", "https://ingest.helix-db.com", []any{"https://foundry.helix-db.com/slack/installed"}},
		"dev.yaml": {"Helix Foundry (dev)", "https://ingest.dev.helix-db.com", []any{
			"https://dev.foundry.helix-db.com/slack/installed", "https://local.foundry.helix-db.com:3000/slack/installed"}},
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
			require.Equal(t, sorted(slack.Scopes), sorted(strs("oauth_config", "scopes", "bot")))
			require.Nil(t, get("oauth_config", "scopes", "user"), "no user scopes")
			require.Equal(t, sorted(slack.Subscriptions), sorted(strs("settings", "event_subscriptions", "bot_events")))
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
