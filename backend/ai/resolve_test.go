package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const sentinel = "sk-ant-api03-sentinel-key-0123456789"

func keyring(t *testing.T) *secretbox.Keyring {
	t.Helper()
	k, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// activeConfigs answers each workspace's active config; none is the default.
type activeConfigs map[string]*model.FoundryAIConfig

func (a activeConfigs) GetActiveFoundryAIConfig(_ context.Context, ws string) (*model.FoundryAIConfig, error) {
	if c, ok := a[ws]; ok {
		return c, nil
	}
	return nil, store.ErrNotFound
}

// hostedConfig is a cloud backend's config: Helix's Claude, with key.
func hostedConfig(key string) *config.Config {
	cfg := config.Defaults()
	cfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: key, Models: config.AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
	}}
	return &cfg
}

// savedConfig is workspace ws's config id, sealed with key under keys.
func savedConfig(t *testing.T, keys *secretbox.Keyring, id, ws, key string, s Settings) *model.FoundryAIConfig {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	c := &model.FoundryAIConfig{ID: id, WorkspaceID: ws, Kind: string(s.Provider.Kind()), Settings: raw}
	if key != "" {
		if c.Secret, err = SealKey(keys, id, ontologyagent.NewModelKey(key)); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// onPlan stands in for Autumn: every workspace on plan, or err.
type onPlan struct {
	plan  *foundryplan.Plan
	err   error
	reads int
}

func (o *onPlan) Status(context.Context, string) (foundryplan.Status, error) {
	o.reads++
	return foundryplan.Status{Plan: o.plan}, o.err
}

var (
	proPlan  = &foundryplan.Plan{Code: "pro", DisplayName: "Pro", AutumnPlanID: "foundry-pro"}
	freePlan = &foundryplan.Plan{Code: "free", DisplayName: "Free"}
)

func TestResolve(t *testing.T) {
	ctx := context.Background()
	hosted, err := NewDeployment(hostedConfig(sentinel))
	if err != nil {
		t.Fatal(err)
	}
	// pro is every hosted workspace on Foundry Pro, which own keys need.
	pro := NewPlans(&onPlan{plan: proPlan})

	// A workspace that brought nothing runs on Helix's Claude, with no
	// keyring involved: a keyring fault never stops the house.
	t.Run("runs a workspace without a setting on the default", func(t *testing.T) {
		r := &Resolver{DB: activeConfigs{}, Deployment: hosted}
		got, err := r.Resolve(ctx, "ws-1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != SourceDefault || got.Key.Reveal() != sentinel || got.ConfigID != "" || got.Label() != "Claude" {
			t.Fatalf("Resolve() = %+v", got)
		}
		if got.Settings.Models.Draft.Name != "claude-opus-5-5" || got.Settings.Models.Extract.Name != "claude-haiku-5-5" {
			t.Fatalf("the default's models = %+v", got.Settings.Models)
		}
	})

	// The owner's active config wins over the default, with its key opened
	// and what its probe found.
	t.Run("prefers the workspace's own provider", func(t *testing.T) {
		keys := keyring(t)
		own := savedConfig(t, keys, "cfg-1", "ws-1", "sk-ant-own-0123456789", Settings{Provider: Anthropic{}, Models: claudeRoles(t)})
		own.ProbeReport = json.RawMessage(`{"checks":[{"role":"ask","passed":true,"reasoning":false,"seconds":1.5}]}`)
		r := &Resolver{DB: activeConfigs{"ws-1": own}, Keyring: keys, Deployment: hosted, Plans: pro}
		got, err := r.Resolve(ctx, "ws-1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != SourceWorkspace || got.ConfigID != "cfg-1" || got.Key.Reveal() != "sk-ant-own-0123456789" {
			t.Fatalf("Resolve() = %+v", got)
		}
		if got.Report.Reasoning(RoleAsk) || !got.Report.Reasoning(RoleDraft) {
			t.Fatalf("reasoning: ask %v, draft %v", got.Report.Reasoning(RoleAsk), got.Report.Reasoning(RoleDraft))
		}
		other, err := r.Resolve(ctx, "ws-2")
		if err != nil || other.Source != SourceDefault {
			t.Fatalf("another workspace = %+v, %v", other, err)
		}
	})

	// A sealed key is bound to its row: copied onto another workspace's
	// config it does not open, so one workspace's key can't be spent by
	// another.
	t.Run("refuses a key sealed for another row", func(t *testing.T) {
		keys := keyring(t)
		theirs := savedConfig(t, keys, "cfg-a", "ws-a", "sk-ant-theirs-0123456789", Settings{Provider: Anthropic{}, Models: claudeRoles(t)})
		mine := savedConfig(t, keys, "cfg-b", "ws-b", "", Settings{Provider: Anthropic{}, Models: claudeRoles(t)})
		mine.Secret = theirs.Secret
		r := &Resolver{DB: activeConfigs{"ws-b": mine}, Keyring: keys, Deployment: hosted, Plans: pro}
		if _, err := r.Resolve(ctx, "ws-b"); !errors.Is(err, ErrKeyUnreadable) {
			t.Fatalf("Resolve() = %v, want ErrKeyUnreadable", err)
		}
		if _, err := OpenKey(keys, mine); !errors.Is(err, secretbox.ErrOpen) {
			t.Fatalf("OpenKey() = %v", err)
		}
	})

	// A process without the keyring can't run a workspace's own provider,
	// and says so as a retryable fault rather than a bad key.
	t.Run("needs the keyring only for a workspace's key", func(t *testing.T) {
		keys := keyring(t)
		own := savedConfig(t, keys, "cfg-1", "ws-1", "sk-ant-own-0123456789", Settings{Provider: Anthropic{}, Models: claudeRoles(t)})
		r := &Resolver{DB: activeConfigs{"ws-1": own}, Deployment: hosted, Plans: pro}
		if _, err := r.Resolve(ctx, "ws-1"); !errors.Is(err, ErrNoKeyring) {
			t.Fatalf("Resolve() = %v, want ErrNoKeyring", err)
		}
		if copy := ResolveFailure(ErrNoKeyring); copy.Permanent || !copy.Ops {
			t.Fatalf("ResolveFailure(ErrNoKeyring) = %+v", copy)
		}
	})

	// Keys go only where their kind takes one: a server that wants none
	// resolves without, and a cloud kind whose key is gone can't run.
	t.Run("resolves keys by kind", func(t *testing.T) {
		keys := keyring(t)
		ollama := savedConfig(t, keys, "cfg-o", "ws-o", "", Settings{Provider: Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: 65536},
			Models: every(Model{Name: "qwen3:32b", Price: PriceFree{}})})
		compat := savedConfig(t, keys, "cfg-c", "ws-c", "", Settings{Provider: Compatible{BaseURL: "https://llm.example.com/v1", API: CompatResponses, MaxCallsInFlight: 4},
			Models: every(Model{Name: "m", Price: PriceFree{}})})
		dropped := savedConfig(t, keys, "cfg-d", "ws-d", "", Settings{Provider: Anthropic{}, Models: claudeRoles(t)})
		r := &Resolver{DB: activeConfigs{"ws-o": ollama, "ws-c": compat, "ws-d": dropped}, Keyring: keys, Deployment: hosted, Plans: pro}
		for _, ws := range []string{"ws-o", "ws-c"} {
			if got, err := r.Resolve(ctx, ws); err != nil || !got.Key.IsZero() {
				t.Fatalf("Resolve(%s) = %+v, %v", ws, got, err)
			}
		}
		if _, err := r.Resolve(ctx, "ws-d"); !errors.Is(err, ErrKeyUnreadable) {
			t.Fatalf("Resolve() of a keyless Claude config = %v", err)
		}
	})

	// An OpenAI or Azure setting runs on its endpoint under its owner's key,
	// each Azure deployment priced as the model it deploys.
	t.Run("resolves OpenAI and Azure with their keys", func(t *testing.T) {
		keys := keyring(t)
		openai, _ := Suggested(KindOpenAI)
		azure := Settings{Provider: AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai/v1"},
			Models: every(Model{Name: "prod-sol", Price: PriceTableAs{Model: "gpt-6.1-sol"}})}
		r := &Resolver{DB: activeConfigs{
			"ws-o": savedConfig(t, keys, "cfg-o", "ws-o", "sk-proj-openai-0123456789", Settings{Provider: OpenAI{}, Models: openai}),
			"ws-a": savedConfig(t, keys, "cfg-a", "ws-a", "azure-0123456789abcdef", azure),
		}, Keyring: keys, Deployment: hosted, Plans: pro}
		got, err := r.Resolve(ctx, "ws-o")
		if err != nil || got.Key.Reveal() != "sk-proj-openai-0123456789" || got.Label() != "OpenAI" ||
			got.Turn().Endpoint.Provider != (ontologyagent.OpenAI{}) || got.Turn().Draft.Name != "gpt-6.1-sol" || got.Extract().Extract.Name != "gpt-6-luna" {
			t.Fatalf("Resolve(OpenAI) = %+v, %v", got, err)
		}
		if got, err = r.Resolve(ctx, "ws-a"); err != nil || got.Key.Reveal() != "azure-0123456789abcdef" || got.Label() != "Azure OpenAI" ||
			got.Ask().Endpoint.Provider != (ontologyagent.AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai/v1"}) ||
			got.Ask().Ask.Price != (ontologyagent.PriceTableAs{Model: "gpt-6.1-sol"}) {
			t.Fatalf("Resolve(Azure) = %+v, %v", got, err)
		}
		if _, err := r.ResolveServed(ctx, "ws-a", []string{"anthropic", "openai"}); !errors.Is(err, ErrNotServed) {
			t.Fatalf("Azure on an agent that doesn't call it = %v", err)
		}
	})

	// A Bedrock setting runs in its region, on its FIPS host if chosen,
	// under the owner's Bedrock API key; titles run on its extraction model
	// with reasoning off, as Helix's Claude's do.
	t.Run("resolves Bedrock with its region and key", func(t *testing.T) {
		keys := keyring(t)
		roles, _ := Suggested(KindBedrock)
		const key = "ABSKQmVkcm9ja0FQSUtleS0wMTIzNDU2Nzg5"
		r := &Resolver{DB: activeConfigs{
			"ws-b": savedConfig(t, keys, "cfg-b", "ws-b", key, Settings{Provider: Bedrock{Region: "us-gov-west-1", FIPS: true}, Models: roles}),
		}, Keyring: keys, Deployment: hosted, Plans: pro}
		got, err := r.ResolveServed(ctx, "ws-b", []string{"anthropic", "bedrock"})
		endpoint := ontologyagent.Bedrock{Region: "us-gov-west-1", FIPS: true}
		if err != nil || got.Key.Reveal() != key || got.Label() != "Amazon Bedrock" || got.Turn().Endpoint.Provider != endpoint ||
			got.Ask().Endpoint.Provider != endpoint || got.Extract().Endpoint.Provider != endpoint || got.Title().Endpoint.Provider != endpoint {
			t.Fatalf("Resolve(Bedrock) = %+v, %v", got, err)
		}
		if got.Turn().Draft.Name != roles.Draft.Name || got.Extract().Verify.Name != roles.Verify.Name ||
			got.Title().Title.Name != roles.Extract.Name || got.Title().Title.Reasoning || !got.Turn().Draft.Reasoning {
			t.Fatalf("Bedrock's models = %+v, %+v", got.Turn(), got.Title())
		}
		if _, err := r.ResolveServed(ctx, "ws-b", []string{"anthropic", "openai", "azure_openai"}); !errors.Is(err, ErrNotServed) {
			t.Fatalf("Bedrock on an agent that doesn't call it = %v", err)
		}
		// A Bedrock config whose key is gone is refused, never run keyless.
		keyless := savedConfig(t, keys, "cfg-k", "ws-k", "", Settings{Provider: Bedrock{Region: "us-east-1"}, Models: roles})
		r.DB = activeConfigs{"ws-k": keyless}
		if _, err := r.Resolve(ctx, "ws-k"); !errors.Is(err, ErrKeyUnreadable) {
			t.Fatalf("Resolve(keyless Bedrock) = %v", err)
		}
	})

	// A row this backend can't read stops the run for good with copy that
	// says how to fix it, rather than running on a guess.
	t.Run("refuses settings it can't read", func(t *testing.T) {
		r := &Resolver{DB: activeConfigs{"ws-1": {ID: "cfg-1", Settings: json.RawMessage(`{"v":9}`)}}, Keyring: keyring(t), Deployment: hosted, Plans: pro}
		_, err := r.Resolve(ctx, "ws-1")
		if !errors.Is(err, ErrSettingsUnreadable) || !ResolveFailure(err).Permanent {
			t.Fatalf("Resolve() = %v", err)
		}
	})

	// Hosted, a workspace's own provider needs Pro or Max, as saving it did:
	// one that left the plan stops, for good, with copy that says why and
	// how to get going again. Helix's Claude never asks Autumn, and Autumn
	// failing to say is a passing fault.
	t.Run("stops a workspace that left Pro", func(t *testing.T) {
		keys := keyring(t)
		own := savedConfig(t, keys, "cfg-1", "ws-1", "sk-ant-own-0123456789", Settings{Provider: Anthropic{}, Models: claudeRoles(t)})
		free := &onPlan{plan: freePlan}
		r := &Resolver{DB: activeConfigs{"ws-1": own}, Keyring: keys, Deployment: hosted, Plans: NewPlans(free)}
		_, err := r.Resolve(ctx, "ws-1")
		if !errors.Is(err, ErrProRequired) {
			t.Fatalf("Resolve() = %v, want ErrProRequired", err)
		}
		if copy := ResolveFailure(err); !copy.Permanent || copy.Ops || !strings.Contains(copy.Message, "needs Foundry Pro") ||
			!strings.Contains(copy.Message, "Settings › AI models") {
			t.Fatalf("ResolveFailure(ErrProRequired) = %+v", copy)
		}
		if got, err := r.Resolve(ctx, "ws-2"); err != nil || got.Source != SourceDefault || free.reads != 1 {
			t.Fatalf("Helix's Claude = %+v, %v after %d plan reads", got, err, free.reads)
		}
		for name, plans := range map[string]*Plans{"no plans sold": NewPlans(nil), "no plans": nil} {
			r.Plans = plans
			if _, err := r.Resolve(ctx, "ws-1"); !errors.Is(err, ErrProRequired) {
				t.Fatalf("%s: Resolve() = %v, want ErrProRequired", name, err)
			}
		}
		r.Plans = NewPlans(&onPlan{err: errors.New("autumn down")})
		if _, err := r.Resolve(ctx, "ws-1"); err == nil || errors.Is(err, ErrProRequired) || ResolveFailure(err).Permanent {
			t.Fatalf("Autumn down: Resolve() = %v", err)
		}
		// A local install sells no plan: its owners bring keys freely.
		cfg := config.Defaults()
		cfg.Host.Local = true
		local, err := NewDeployment(&cfg)
		if err != nil {
			t.Fatal(err)
		}
		r = &Resolver{DB: activeConfigs{"ws-1": own}, Keyring: keys, Deployment: local}
		if got, err := r.Resolve(ctx, "ws-1"); err != nil || got.Source != SourceWorkspace {
			t.Fatalf("local: Resolve() = %+v, %v", got, err)
		}
	})

	// A local install with no default has nothing to run model work on until
	// an owner chooses a provider.
	t.Run("says when a local install has no provider", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Host.Local = true
		none, err := NewDeployment(&cfg)
		if err != nil {
			t.Fatal(err)
		}
		keys := keyring(t)
		own := savedConfig(t, keys, "cfg-1", "ws-1", "sk-ant-own-0123456789", Settings{Provider: Anthropic{}, Models: claudeRoles(t)})
		r := &Resolver{DB: activeConfigs{"ws-1": own}, Keyring: keys, Deployment: none}
		if _, err := r.Resolve(ctx, "ws-2"); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("Resolve() = %v, want ErrNotConfigured", err)
		}
		if err := r.Usable(ctx, "ws-1"); err != nil {
			t.Fatalf("Usable(ws-1) = %v", err)
		}
		if err := r.Usable(ctx, "ws-2"); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("Usable(ws-2) = %v, want ErrNotConfigured", err)
		}
	})

	// Before any work is queued, a hosted workspace is refused as its worker
	// would refuse it: its own provider only on a plan that allows one, Helix's
	// Claude always, and the plan read only for its own provider.
	t.Run("refuses a hosted workspace's own provider off Pro before queueing", func(t *testing.T) {
		keys := keyring(t)
		own := savedConfig(t, keys, "cfg-1", "ws-1", "sk-ant-own-0123456789", Settings{Provider: Anthropic{}, Models: claudeRoles(t)})
		freePlans := &onPlan{plan: freePlan}
		r := &Resolver{DB: activeConfigs{"ws-1": own}, Deployment: hosted, Plans: NewPlans(freePlans)}
		if err := r.Usable(ctx, "ws-1"); !errors.Is(err, ErrProRequired) {
			t.Fatalf("own key on Free: Usable() = %v, want ErrProRequired", err)
		}
		if err := r.Usable(ctx, "ws-2"); err != nil || freePlans.reads != 1 {
			t.Fatalf("Helix's Claude: Usable() = %v, %d plan reads", err, freePlans.reads)
		}
		r.Plans = pro
		if err := r.Usable(ctx, "ws-1"); err != nil {
			t.Fatalf("own key on Pro: Usable() = %v", err)
		}
		r.Plans = NewPlans(&onPlan{err: errors.New("autumn down")})
		if err := r.Usable(ctx, "ws-1"); err == nil || errors.Is(err, ErrProRequired) {
			t.Fatalf("Autumn down: Usable() = %v, want a retryable error", err)
		}
	})

	// The key never surfaces through anything a resolution is printed or
	// encoded with.
	t.Run("never prints the key", func(t *testing.T) {
		got, _ := (&Resolver{DB: activeConfigs{}, Deployment: hosted}).Resolve(ctx, "ws-1")
		for _, s := range []string{fmtAll(got), fmtAll(hosted), fmtAll(*hosted.Default)} {
			if strings.Contains(s, sentinel) {
				t.Fatalf("printed %s", s)
			}
		}
		if _, err := json.Marshal(got); err == nil {
			t.Fatal("a resolution encodes as JSON, key and all")
		}
	})
}
