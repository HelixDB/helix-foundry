package ontologyagent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologygen"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	houseKey = "sk-ant-api03-house-sentinel-0123456789"
	ownKey   = "sk-ant-api03-own-sentinel-9876543210"
)

// setting is the workspace's AI setting; nil is Helix's Claude.
type setting struct{ config *model.FoundryAIConfig }

func (s setting) GetActiveFoundryAIConfig(context.Context, string) (*model.FoundryAIConfig, error) {
	if s.config == nil {
		return nil, foundrystore.ErrNotFound
	}
	return s.config, nil
}

// onPro stands in for Autumn: every workspace is on Foundry Pro, so its own
// provider runs.
type onPro struct{}

func (onPro) Status(context.Context, string) (foundryplan.Status, error) {
	return foundryplan.Status{Plan: &foundryplan.Plan{Code: "pro", AutumnPlanID: "foundry-pro", Rank: 1}}, nil
}

// A draft runs on what the workspace's AI setting resolves to, its key in
// the turn's metadata alone; a failed attempt words the model's failure as
// that setting's copy does, and leaves the key out of its logs, its failure
// and what the worker stores of it.
func TestGeneratorOnTheResolver(t *testing.T) {
	cfg := config.Defaults()
	cfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: houseKey, Models: config.AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
	}}
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	own, err := json.Marshal(ai.Settings{Provider: ai.Anthropic{}, Models: ai.Roles{
		Draft: ai.Model{Name: "claude-opus-5", Price: ai.PriceTable{}}, Ask: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}},
		Extract: ai.Model{Name: "claude-haiku-5-5", Price: ai.PriceTable{}}, Verify: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ownConfig := &model.FoundryAIConfig{ID: "cfg-1", WorkspaceID: "ws-1", Kind: "anthropic", Settings: own}
	if ownConfig.Secret, err = ai.SealKey(keys, ownConfig.ID, ontologyagent.NewModelKey(ownKey)); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		config  *model.FoundryAIConfig
		key     string
		model   string
		failure string
		raised  bool
	}{
		"Helix's Claude":  {nil, houseKey, "claude-opus-5-5", "Claude is unavailable; Helix will retry", true},
		"the owner's key": {ownConfig, ownKey, "claude-opus-5", "Claude refused the API key. An owner can enter a new one in Settings › AI models.", false},
	} {
		t.Run(name, func(t *testing.T) {
			resolver, err := ai.NewResolver(&cfg, setting{tc.config}, onPro{})
			if err != nil {
				t.Fatal(err)
			}
			resolver.Keyring = keys
			g, calls, sent := ontologyagent.NewTestGenerator(t, `{"model_failed":{"reason":"auth"}}`)
			g.AI = resolver
			var logs bytes.Buffer
			ctx := log.Into(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)))
			result, err := g.Run(ctx, ontologyagent.TestGeneration())
			var f *ontologygen.Failure
			if !errors.As(err, &f) || f.Message != tc.failure || f.Permanent == tc.raised {
				t.Fatalf("failure = %v", err)
			}
			if raised := strings.Contains(logs.String(), `"level":"ERROR"`); raised != tc.raised {
				t.Fatalf("raised %v: %s", raised, logs.String())
			}
			requests := calls()
			if len(requests) != 1 || !strings.Contains(requests[0], `"draft":{"name":"`+tc.model+`"`) ||
				fmt.Sprint(sent()) != "[["+tc.key+"]]" || result.ModelProvider != "anthropic" {
				t.Fatalf("requests %v, keys %v, result %+v", requests, sent(), result)
			}
			// What the worker logs and stores of the attempt: its error, the
			// failure's message, the model and the tokens.
			stored := fmt.Sprintf("%v %s %+v %s", err, f.Message, result, logs.String())
			for _, secret := range []string{houseKey, ownKey} {
				if strings.Contains(stored, secret) || strings.Contains(requests[0], secret) {
					t.Fatalf("a key reached a request, a log or the stored attempt: %s", stored)
				}
			}
		})
	}
}

// onFree stands in for Autumn: every workspace is on Free.
type onFree struct{}

func (onFree) Status(context.Context, string) (foundryplan.Status, error) {
	return foundryplan.Status{Plan: &foundryplan.Plan{Code: "free"}}, nil
}

// A hosted workspace that left Pro after an owner brought a key drafts on
// it no more: the generation fails for good, before any model call, saying
// how to get going again.
func TestGeneratorAfterLeavingPro(t *testing.T) {
	cfg := config.Defaults()
	cfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: houseKey, Models: config.AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
	}}
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := json.Marshal(ai.Settings{Provider: ai.Anthropic{}, Models: ai.Roles{
		Draft: ai.Model{Name: "claude-opus-5-5", Price: ai.PriceTable{}}, Ask: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}},
		Extract: ai.Model{Name: "claude-haiku-5-5", Price: ai.PriceTable{}}, Verify: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	c := &model.FoundryAIConfig{ID: "cfg-1", WorkspaceID: "ws-1", Kind: "anthropic", Settings: settings}
	if c.Secret, err = ai.SealKey(keys, c.ID, ontologyagent.NewModelKey(ownKey)); err != nil {
		t.Fatal(err)
	}
	resolver, err := ai.NewResolver(&cfg, setting{c}, onFree{})
	if err != nil {
		t.Fatal(err)
	}
	resolver.Keyring = keys
	g, calls, _ := ontologyagent.NewTestGenerator(t, `{"model_failed":{"reason":"auth"}}`)
	g.AI = resolver
	_, err = g.Run(context.Background(), ontologyagent.TestGeneration())
	var f *ontologygen.Failure
	if !errors.As(err, &f) || !f.Permanent || !strings.HasPrefix(f.Message, "Bringing your own model key needs Foundry Pro.") {
		t.Fatalf("failure = %v", err)
	}
	if len(calls()) != 0 {
		t.Fatalf("the agent was called: %v", calls())
	}
}
