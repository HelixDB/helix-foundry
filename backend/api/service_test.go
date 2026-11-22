package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/config"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// A hosted API with foundry.slack on refuses to start without a way to read
// users' emails, which linking a Slack account compares; with one, with
// Slack off, or on a standalone install (whose front end refuses every Slack
// method) it starts.
func TestNewSlackNeedsEmails(t *testing.T) {
	build := func(slack, local bool, emails UserEmails) error {
		cfg := config.Defaults()
		cfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: "sk-ant-helix-0123456789", Models: config.AIModels{
			Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5"}}
		cfg.Foundry.Slack.Enabled = slack
		cfg.Host.Local = local
		s, err := New(Deps{Cfg: &cfg, DB: foundrystore.Unimplemented{}, Emails: emails})
		if err == nil {
			require.NoError(t, s.Close())
		}
		return err
	}
	err := build(true, false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Deps.Emails")
	assert.NoError(t, build(true, false, slackPeople{}), "hosted, with emails")
	assert.NoError(t, build(false, false, nil), "Slack off")
	assert.NoError(t, build(true, true, nil), "a standalone install")
}
