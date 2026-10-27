package api

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
)

// An Ollama server is a local install's alone: saved with no key, its
// models free unless priced, in the default context window unless set.
func TestUpdateFoundryAIOllama(t *testing.T) {
	ctx := ports.WithActor(context.Background(), owner)
	local := func() *config.Config { return aiConfig(modeLocal, true, "ollama", "anthropic") }
	server := func(base string, window uint32) *foundryv1.UpdateFoundryAIRequest_Provider {
		return &foundryv1.UpdateFoundryAIRequest_Provider{Provider: &foundryv1.FoundryAIProvider{
			Kind: &foundryv1.FoundryAIProvider_Ollama{Ollama: &foundryv1.FoundryAIOllama{BaseUrl: base, ContextTokens: window}},
		}}
	}
	// pulled names one model for every role, given no price.
	pulled := func(name string) *foundryv1.FoundryAIModels {
		m := func() *foundryv1.FoundryAIModel { return &foundryv1.FoundryAIModel{Name: name} }
		return &foundryv1.FoundryAIModels{Draft: m(), Ask: m(), Extract: m(), Verify: m()}
	}
	save := func(base string, window uint32, models *foundryv1.FoundryAIModels, key string) *foundryv1.UpdateFoundryAIRequest {
		return &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1", Choice: server(base, window), Models: models, ApiKey: key}
	}

	// The settings page offers Ollama first, keyless and with no suggested
	// models: the owner names what they pulled.
	t.Run("offers Ollama first, with no key", func(t *testing.T) {
		s, _ := aiService(t, local(), "")
		out, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		offered := out.GetAi().GetOffered()
		if len(offered) != 2 || offered[0].GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OLLAMA ||
			offered[0].GetKey() != foundryv1.FoundryAIKey_FOUNDRY_AI_KEY_NONE || offered[0].GetSuggested() != nil || !out.GetAi().GetPrivateHostsAllowed() {
			t.Fatalf("FoundryAI = %v", out.GetAi())
		}
	})

	// A server on this computer is stored normalized, with no key, the
	// default window and its models free; the page shows it as Ollama.
	t.Run("saves a server on this computer", func(t *testing.T) {
		s, store := aiService(t, local(), "")
		out, err := s.UpdateFoundryAI(ctx, save(" http://host.docker.internal:11434/ ", 0, pulled(" qwen3:8b "), ""))
		if err != nil {
			t.Fatal(err)
		}
		c := store.saved[0]
		settings, err := ai.DecodeSettings(c.Settings)
		if err != nil {
			t.Fatal(err)
		}
		if c.Kind != "ollama" || c.Secret != nil || c.SecretHint != nil ||
			settings.Provider != (ai.Ollama{BaseURL: "http://host.docker.internal:11434", ContextTokens: ai.DefaultContextTokens}) {
			t.Fatalf("saved %+v: %+v", c, settings)
		}
		for _, rm := range settings.Models.Each() {
			if rm.Model != (ai.Model{Name: "qwen3:8b", Price: ai.PriceFree{}}) {
				t.Fatalf("%s = %+v", rm.Role, rm.Model)
			}
		}
		p := out.GetAi().GetPending()
		if p.GetLabel() != "Ollama" || p.GetHasKey() || p.GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OLLAMA ||
			p.GetProvider().GetOllama().GetContextTokens() != ai.DefaultContextTokens || p.GetModels().GetAsk().GetFree() == nil {
			t.Fatalf("pending = %v", p)
		}
	})

	// An owner may set a larger window, and track a model's cost at their
	// own rates.
	t.Run("keeps a window and rates the owner set", func(t *testing.T) {
		s, store := aiService(t, local(), "")
		models := pulled("qwen3:32b")
		models.Draft.Price = &foundryv1.FoundryAIModel_Rates{Rates: &foundryv1.FoundryAIRates{InputUsdPerMtok: "0.01", OutputUsdPerMtok: "0.02"}}
		models.Ask.Price = &foundryv1.FoundryAIModel_Free{Free: &emptypb.Empty{}}
		if _, err := s.UpdateFoundryAI(ctx, save("https://gpu.example.com/ollama", 131072, models, "")); err != nil {
			t.Fatal(err)
		}
		settings, _ := ai.DecodeSettings(store.saved[0].Settings)
		if settings.Provider.(ai.Ollama).ContextTokens != 131072 || settings.Models.Draft.Price != (ai.PriceRates{Input: 10, Output: 20}) ||
			settings.Models.Ask.Price != (ai.PriceFree{}) {
			t.Fatalf("saved %+v", settings)
		}
	})

	// A blank key never keeps another provider's: Ollama takes none.
	t.Run("drops the setting's key", func(t *testing.T) {
		s, store := aiService(t, local(), "")
		store.view.Active = savedClaude(t, s.deps.ConnectorKeys, "cfg-1", ownKey)
		if _, err := s.UpdateFoundryAI(ctx, save("http://127.0.0.1:11434", 0, pulled("qwen3:8b"), "")); err != nil {
			t.Fatal(err)
		}
		if c := store.saved[0]; c.Secret != nil || c.SecretHint != nil {
			t.Fatalf("an Ollama config kept a key: %+v", c)
		}
	})

	// Each refusal says what to fix, stores nothing and never echoes a key.
	tablePriced := pulled("qwen3:8b")
	tablePriced.Extract.Price = &foundryv1.FoundryAIModel_Table{Table: &emptypb.Empty{}}
	for name, tc := range map[string]struct {
		req  *foundryv1.UpdateFoundryAIRequest
		want string
	}{
		"a key":                  {save("http://127.0.0.1:11434", 0, pulled("qwen3:8b"), ownKey), "Ollama takes no API key"},
		"a small window":         {save("http://127.0.0.1:11434", 8192, pulled("qwen3:8b"), ""), "provider.context_tokens must be 65536 to 1048576"},
		"a window no draft fits": {save("http://127.0.0.1:11434", 32768, pulled("qwen3:8b"), ""), "provider.context_tokens must be 65536 to 1048576"},
		"a huge window":          {save("http://127.0.0.1:11434", 1<<21, pulled("qwen3:8b"), ""), "provider.context_tokens must be 65536 to 1048576"},
		"the /v1 path":           {save("http://127.0.0.1:11434/v1", 0, pulled("qwen3:8b"), ""), "without /api or /v1"},
		"no server":              {save("", 0, pulled("qwen3:8b"), ""), "provider.base_url"},
		"a table price":          {save("http://127.0.0.1:11434", 0, tablePriced, ""), "models.extract.price must be free or your own rates"},
		"no model for a role":    {save("http://127.0.0.1:11434", 0, &foundryv1.FoundryAIModels{Draft: &foundryv1.FoundryAIModel{Name: "qwen3:8b"}}, ""), "models.ask is required"},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			s, store := aiService(t, local(), "")
			_, err := s.UpdateFoundryAI(ctx, tc.req)
			if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), tc.want) {
				t.Fatalf("UpdateFoundryAI() = %v, want INVALID_ARGUMENT saying %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "sentinel") || len(store.saved) > 0 {
				t.Fatalf("err %v, saved %+v", err, store.saved)
			}
		})
	}

	// Hosted Foundry never takes an Ollama server, even from a deployment
	// that somehow lists the kind, and before it reads the plan.
	t.Run("refuses Ollama hosted", func(t *testing.T) {
		s, store := aiService(t, aiConfig(modeCloud, false), "pro")
		req := save("https://ollama.example.com", 0, pulled("qwen3:8b"), "")
		for _, kinds := range [][]ai.Kind{{ai.KindAnthropic}, {ai.KindAnthropic, ai.KindOllama}} {
			s.deps.AI.Kinds = kinds
			_, err := s.UpdateFoundryAI(ctx, req)
			if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "Ollama is not offered here" || len(store.saved) > 0 {
				t.Fatalf("kinds %v: UpdateFoundryAI() = %v", kinds, err)
			}
		}
		out, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		for _, offer := range out.GetAi().GetOffered() {
			if offer.GetKind() == foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OLLAMA {
				t.Fatalf("hosted offers Ollama: %v", out.GetAi().GetOffered())
			}
		}
	})

	// An install whose default is an Ollama server shows it, and runs model
	// work on it before any owner chooses.
	t.Run("shows an Ollama default", func(t *testing.T) {
		cfg := local()
		cfg.Foundry.AI.Default = config.AIDefault{Provider: "ollama", BaseURL: "http://127.0.0.1:11434", Models: config.AIModels{
			Draft: "qwen3:32b", Ask: "qwen3:8b", Extract: "qwen3:8b", Verify: "qwen3:8b",
		}}
		s, _ := aiService(t, cfg, "")
		out, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		def := out.GetAi().GetDefault()
		if def.GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OLLAMA || def.GetLabel() != "Ollama" ||
			def.GetModels().GetDraft().GetName() != "qwen3:32b" || def.GetModels().GetAsk().GetFree() == nil {
			t.Fatalf("default = %v", def)
		}
		if err := s.requireAI(ctx, "ws-1"); err != nil {
			t.Fatalf("requireAI() = %v", err)
		}
	})
}
