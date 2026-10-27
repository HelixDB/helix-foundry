package ai

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
)

// What generations and questions said of Helix's Claude before providers
// (ontologyagent/generator.go:267,269 and foundry-ask-worker/run.go:392,393
// at origin/dev): its copy may never change.
const (
	houseDeclined    = "Claude declined to draft this ontology"
	houseUnavailable = "Claude is unavailable; Helix will retry"
	askUnreachable   = "the model could not be reached; ask again"
	askDeclined      = "the model declined to answer; ask another way"
)

// claude is Helix's Claude's models.
var claude = Settings{Provider: Anthropic{}, Models: Roles{
	Draft: Model{Name: "claude-opus-5-5", Price: PriceTable{}}, Ask: Model{Name: "claude-sonnet-5-5", Price: PriceTable{}},
	Extract: Model{Name: "claude-haiku-5-5", Price: PriceTable{}}, Verify: Model{Name: "claude-sonnet-5-5", Price: PriceTable{}},
}}

func TestDraftFailure(t *testing.T) {
	house := Resolved{Source: SourceDefault, Settings: claude}
	own := Resolved{Source: SourceWorkspace, Settings: claude}

	// Helix's Claude says exactly what generations said before providers:
	// a refusal is final, and everything else is retried as unavailable,
	// the failures no passing outage explains raised with on-call.
	t.Run("keeps Helix's Claude's copy", func(t *testing.T) {
		for _, f := range ontologyagent.ModelFailures {
			want := Copy{Message: houseUnavailable, Ops: true}
			switch f {
			case ontologyagent.ModelRefused:
				want = Copy{Message: houseDeclined, Permanent: true}
			case ontologyagent.ModelUnavailable, ontologyagent.ModelRateLimited, ontologyagent.ModelMalformed, ontologyagent.ModelTimeout:
				want = Copy{Message: houseUnavailable}
			}
			if got := DraftFailure(house, f); got != want {
				t.Errorf("DraftFailure(house, %s) = %+v, want %+v", f, got, want)
			}
		}
	})

	// A workspace's own provider's failures that aren't passing are the
	// owner's to fix, and say where.
	t.Run("sends the owner to the settings for their key's failures", func(t *testing.T) {
		for f, want := range map[ontologyagent.ModelFailure]string{
			ontologyagent.ModelRefused:        houseDeclined,
			ontologyagent.ModelAuth:           "Claude refused the API key. An owner can enter a new one in Settings › AI models.",
			ontologyagent.ModelPermission:     "Claude refused access to claude-opus-5-5. An owner can choose another model in Settings › AI models.",
			ontologyagent.ModelNotFound:       "Claude has no model claude-opus-5-5. An owner can choose another in Settings › AI models.",
			ontologyagent.ModelQuota:          "The Claude account is out of credit or quota.",
			ontologyagent.ModelContextTooLong: "This was too long for claude-opus-5-5.",
			ontologyagent.ModelUnsupported:    "claude-opus-5-5 can’t do what this task needs (tool calls or structured answers).",
			ontologyagent.ModelTruncated:      "The model ran out of room for its answer.",
			ontologyagent.ModelChanged:        "The Ollama model changed since it was tested. An owner can test it again in Settings › AI models.",
			ontologyagent.ModelPrivateHost:    "The endpoint resolves to a private address.",
		} {
			if got := DraftFailure(own, f); got.Message != want || !got.Permanent || got.Ops {
				t.Errorf("DraftFailure(own, %s) = %+v, want %q", f, got, want)
			}
		}
		for _, f := range []ontologyagent.ModelFailure{
			ontologyagent.ModelUnavailable, ontologyagent.ModelRateLimited, ontologyagent.ModelMalformed, ontologyagent.ModelTimeout,
		} {
			if got := DraftFailure(own, f); got != (Copy{Message: houseUnavailable}) {
				t.Errorf("DraftFailure(own, %s) = %+v: a passing failure is retried", f, got)
			}
		}
		compat := Resolved{Source: SourceWorkspace, Settings: Settings{
			Provider: Compatible{BaseURL: "https://llm.example.com/v1"}, Models: Roles{Draft: Model{Name: "m"}},
		}}
		if got := DraftFailure(compat, ontologyagent.ModelUnavailable).Message; got != "llm.example.com is unavailable; Helix will retry" {
			t.Errorf("a custom server's label: %q", got)
		}
	})
}

func TestAskFailure(t *testing.T) {
	// Helix's Claude says exactly what questions said before providers.
	t.Run("keeps Helix's Claude's copy", func(t *testing.T) {
		house := Resolved{Source: SourceDefault, Settings: claude}
		for _, f := range ontologyagent.ModelFailures {
			want := Copy{Message: askUnreachable, Ops: true}
			switch f {
			case ontologyagent.ModelRefused:
				want = Copy{Message: askDeclined, Permanent: true}
			case ontologyagent.ModelUnavailable, ontologyagent.ModelRateLimited, ontologyagent.ModelMalformed, ontologyagent.ModelTimeout:
				want = Copy{Message: askUnreachable}
			}
			if got := AskFailure(house, f); got != want {
				t.Errorf("AskFailure(house, %s) = %+v, want %+v", f, got, want)
			}
		}
	})
	// The owner's own key's failures name the question's model.
	t.Run("words the owner's provider's failures", func(t *testing.T) {
		own := Resolved{Source: SourceWorkspace, Settings: claude}
		if got := AskFailure(own, ontologyagent.ModelNotFound); got.Message != "Claude has no model claude-sonnet-5-5. An owner can choose another in Settings › AI models." {
			t.Errorf("not_found = %+v", got)
		}
		if got := AskFailure(own, ontologyagent.ModelRefused); got.Message != askDeclined {
			t.Errorf("refused = %+v", got)
		}
		if got := AskFailure(own, ontologyagent.ModelTimeout); got != (Copy{Message: askUnreachable}) {
			t.Errorf("timeout = %+v", got)
		}
	})
}

func TestDocumentFailure(t *testing.T) {
	// A failure every document of the load would meet stops it, naming the
	// models documents are read with.
	t.Run("stops a load its owner's provider refuses", func(t *testing.T) {
		own := Resolved{Source: SourceWorkspace, Settings: claude}
		for f, want := range map[ontologyagent.ModelFailure]string{
			ontologyagent.ModelAuth:     "Claude refused the API key. An owner can enter a new one in Settings › AI models.",
			ontologyagent.ModelNotFound: "Claude has no model claude-haiku-5-5 or claude-sonnet-5-5. An owner can choose another in Settings › AI models.",
			ontologyagent.ModelQuota:    "The Claude account is out of credit or quota.",
		} {
			if got, stop := DocumentFailure(own, f); !stop || got.Message != want || got.Ops {
				t.Errorf("DocumentFailure(own, %s) = %+v, %v", f, got, stop)
			}
		}
		same := own
		same.Settings.Models.Verify = same.Settings.Models.Extract
		if got, _ := DocumentFailure(same, ontologyagent.ModelPermission); !strings.Contains(got.Message, "access to claude-haiku-5-5. ") {
			t.Errorf("one model is named once: %q", got.Message)
		}
	})
	// A document's own trouble is its alone; Helix's Claude never stops a
	// load, and raises its key's failures with on-call.
	t.Run("leaves other failures to the document", func(t *testing.T) {
		own := Resolved{Source: SourceWorkspace, Settings: claude}
		house := Resolved{Source: SourceDefault, Settings: claude}
		for _, f := range ontologyagent.ModelFailures {
			if c, stop := DocumentFailure(house, f); stop || c.Message != "" {
				t.Errorf("DocumentFailure(house, %s) = %+v, %v", f, c, stop)
			}
		}
		for _, f := range []ontologyagent.ModelFailure{ontologyagent.ModelUnavailable, ontologyagent.ModelContextTooLong, ontologyagent.ModelRefused} {
			if c, stop := DocumentFailure(own, f); stop || c != (Copy{}) {
				t.Errorf("DocumentFailure(own, %s) = %+v, %v", f, c, stop)
			}
		}
		if c, _ := DocumentFailure(house, ontologyagent.ModelAuth); !c.Ops {
			t.Error("Helix's key's failure isn't raised")
		}
		if c, _ := DocumentFailure(house, ontologyagent.ModelContextTooLong); c.Ops {
			t.Error("a document too long is raised with on-call")
		}
	})
}

// A title falls back to its goal's first sentence: only Helix's Claude
// refusing its key, account or models is on-call's, and a truncated answer
// is no one's.
func TestTitleFailure(t *testing.T) {
	house := Resolved{Source: SourceDefault, Settings: claude}
	own := Resolved{Source: SourceWorkspace, Settings: claude}
	for _, f := range ontologyagent.ModelFailures {
		want := Copy{}
		switch f {
		case ontologyagent.ModelAuth, ontologyagent.ModelPermission, ontologyagent.ModelNotFound, ontologyagent.ModelQuota:
			want = Copy{Ops: true}
		}
		if got := TitleFailure(house, f); got != want {
			t.Errorf("TitleFailure(house, %s) = %+v, want %+v", f, got, want)
		}
		if got := TitleFailure(own, f); got != (Copy{}) {
			t.Errorf("TitleFailure(own, %s) = %+v: the owner's provider is no one's on-call", f, got)
		}
	}
}

func TestResolveFailure(t *testing.T) {
	// A setting that can't run is the owner's to fix, except a missing
	// keyring or a kind the agent doesn't call yet, which are Helix's.
	for err, want := range map[error]Copy{
		ErrNotConfigured:      {Message: "No AI provider is set up. An owner can choose one in Settings › AI models.", Permanent: true},
		ErrKeyUnreadable:      {Message: "The stored API key can't be opened. An owner can enter it again in Settings › AI models.", Permanent: true},
		ErrSettingsUnreadable: {Message: "The saved AI settings can't be read. An owner can save them again in Settings › AI models.", Permanent: true},
		ErrProRequired: {Message: "Bringing your own model key needs Foundry Pro. An owner can upgrade, or go back to Helix's Claude in Settings › AI models.",
			Permanent: true},
		ErrNoKeyring:          {Message: "AI settings can't be read; Helix will retry", Ops: true},
		ErrNotServed:          {Message: "The ontology agent can't call this AI provider yet; Helix will retry", Ops: true},
		errors.New("db down"): {Message: "AI settings can't be read; Helix will retry"},
	} {
		if got := ResolveFailure(err); got != want {
			t.Errorf("ResolveFailure(%v) = %+v, want %+v", err, got, want)
		}
	}
}

// Only a failure Helix must fix is logged as an error.
func TestCopyLogged(t *testing.T) {
	var out bytes.Buffer
	ctx := log.Into(context.Background(), slog.New(slog.NewJSONHandler(&out, nil)))
	Copy{Message: "passing"}.Logged(ctx, errors.New("quiet"))
	if out.Len() > 0 {
		t.Fatalf("a passing failure logged %s", out.String())
	}
	if c := (Copy{Message: "Helix's", Ops: true}).Logged(ctx, errors.New("loud")); c.Message != "Helix's" ||
		!strings.Contains(out.String(), `"level":"ERROR"`) || !strings.Contains(out.String(), "loud") {
		t.Fatalf("Logged = %+v, log %s", c, out.String())
	}
}

// Copy names each provider as its owner knows it.
func TestLabel(t *testing.T) {
	for p, want := range map[Provider]string{
		Anthropic{}:   "Claude",
		OpenAI{}:      "OpenAI",
		AzureOpenAI{}: "Azure OpenAI",
		Bedrock{}:     "Amazon Bedrock",
		Ollama{}:      "Ollama",
		Compatible{BaseURL: "https://llm.example.com:8443/v1"}: "llm.example.com",
		Compatible{}: "the custom endpoint",
		nil:          "the model provider",
	} {
		if got := Label(p); got != want {
			t.Errorf("Label(%#v) = %q, want %q", p, got, want)
		}
	}
}

// OpenAI and Azure OpenAI fail as Claude does, under their own names: an
// Azure call's model is the owner's deployment, so the copy names it.
func TestOpenAICopy(t *testing.T) {
	openai := Resolved{Source: SourceWorkspace, Settings: Settings{Provider: OpenAI{}, Models: Roles{
		Draft: Model{Name: "gpt-6.1-sol", Price: PriceTable{}}, Ask: Model{Name: "gpt-6.1-sol", Price: PriceTable{}},
		Extract: Model{Name: "gpt-6-luna", Price: PriceTable{}}, Verify: Model{Name: "gpt-6.1-sol", Price: PriceTable{}},
	}}}
	azure := Resolved{Source: SourceWorkspace, Settings: Settings{Provider: AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai/v1"}, Models: Roles{
		Draft: Model{Name: "prod-sol", Price: PriceTableAs{Model: "gpt-6.1-sol"}}, Ask: Model{Name: "prod-sol", Price: PriceTableAs{Model: "gpt-6.1-sol"}},
		Extract: Model{Name: "prod-luna", Price: PriceTableAs{Model: "gpt-6-luna"}}, Verify: Model{Name: "prod-sol", Price: PriceTableAs{Model: "gpt-6.1-sol"}},
	}}}
	for _, tc := range []struct {
		name string
		got  Copy
		want Copy
	}{
		{"OpenAI declined", DraftFailure(openai, ontologyagent.ModelRefused), Copy{Message: "OpenAI declined to draft this ontology", Permanent: true}},
		{"OpenAI down", DraftFailure(openai, ontologyagent.ModelUnavailable), Copy{Message: "OpenAI is unavailable; Helix will retry"}},
		{"OpenAI rate limited", DraftFailure(openai, ontologyagent.ModelRateLimited), Copy{Message: "OpenAI is unavailable; Helix will retry"}},
		{"OpenAI's key", DraftFailure(openai, ontologyagent.ModelAuth),
			Copy{Message: "OpenAI refused the API key. An owner can enter a new one in Settings › AI models.", Permanent: true}},
		{"OpenAI's quota", DraftFailure(openai, ontologyagent.ModelQuota), Copy{Message: "The OpenAI account is out of credit or quota.", Permanent: true}},
		{"OpenAI's model", DraftFailure(openai, ontologyagent.ModelPermission),
			Copy{Message: "OpenAI refused access to gpt-6.1-sol. An owner can choose another model in Settings › AI models.", Permanent: true}},
		{"Azure down", DraftFailure(azure, ontologyagent.ModelTimeout), Copy{Message: "Azure OpenAI is unavailable; Helix will retry"}},
		{"Azure's key", DraftFailure(azure, ontologyagent.ModelAuth),
			Copy{Message: "Azure OpenAI refused the API key. An owner can enter a new one in Settings › AI models.", Permanent: true}},
		{"Azure's deployment", DraftFailure(azure, ontologyagent.ModelNotFound),
			Copy{Message: "Azure OpenAI has no model prod-sol. An owner can choose another in Settings › AI models.", Permanent: true}},
		{"Azure's quota", AskFailure(azure, ontologyagent.ModelQuota), Copy{Message: "The Azure OpenAI account is out of credit or quota.", Permanent: true}},
		{"Azure's question", AskFailure(azure, ontologyagent.ModelUnsupported),
			Copy{Message: "prod-sol can’t do what this task needs (tool calls or structured answers).", Permanent: true}},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.name, tc.got, tc.want)
		}
	}
	if got, stop := DocumentFailure(azure, ontologyagent.ModelNotFound); !stop ||
		got.Message != "Azure OpenAI has no model prod-luna or prod-sol. An owner can choose another in Settings › AI models." {
		t.Errorf("an Azure load = %+v, %v", got, stop)
	}
	if got, stop := DocumentFailure(openai, ontologyagent.ModelAuth); !stop || !strings.HasPrefix(got.Message, "OpenAI refused the API key.") {
		t.Errorf("an OpenAI load = %+v, %v", got, stop)
	}
	if got := TitleFailure(openai, ontologyagent.ModelAuth); got != (Copy{}) {
		t.Errorf("an OpenAI title = %+v: the owner's provider is no one's on-call", got)
	}
}

// Amazon Bedrock fails as Claude does, under its own name: a model the
// account has no access to in the region, or one that can't call tools,
// names the model ID the owner chose, an inference profile's included.
func TestBedrockCopy(t *testing.T) {
	bedrock := Resolved{Source: SourceWorkspace, Settings: Settings{Provider: Bedrock{Region: "us-east-1"}, Models: Roles{
		Draft: Model{Name: "us.anthropic.claude-opus-5-5", Price: PriceTable{}}, Ask: Model{Name: "us.anthropic.claude-sonnet-5-5", Price: PriceTable{}},
		Extract: Model{Name: "amazon.nova-lite-v1:0", Price: PriceRates{Input: 60, Output: 240}}, Verify: Model{Name: "us.anthropic.claude-sonnet-5-5", Price: PriceTable{}},
	}}}
	for _, tc := range []struct {
		name string
		got  Copy
		want Copy
	}{
		{"a guardrail", DraftFailure(bedrock, ontologyagent.ModelRefused), Copy{Message: "Amazon Bedrock declined to draft this ontology", Permanent: true}},
		{"throttled", DraftFailure(bedrock, ontologyagent.ModelRateLimited), Copy{Message: "Amazon Bedrock is unavailable; Helix will retry"}},
		{"down", DraftFailure(bedrock, ontologyagent.ModelUnavailable), Copy{Message: "Amazon Bedrock is unavailable; Helix will retry"}},
		{"its key", DraftFailure(bedrock, ontologyagent.ModelAuth),
			Copy{Message: "Amazon Bedrock refused the API key. An owner can enter a new one in Settings › AI models.", Permanent: true}},
		{"no model access", DraftFailure(bedrock, ontologyagent.ModelPermission),
			Copy{Message: "Amazon Bedrock refused access to us.anthropic.claude-opus-5-5. An owner can choose another model in Settings › AI models.", Permanent: true}},
		{"no such model in the region", AskFailure(bedrock, ontologyagent.ModelNotFound),
			Copy{Message: "Amazon Bedrock has no model us.anthropic.claude-sonnet-5-5. An owner can choose another in Settings › AI models.", Permanent: true}},
		{"its quota", AskFailure(bedrock, ontologyagent.ModelQuota), Copy{Message: "The Amazon Bedrock account is out of credit or quota.", Permanent: true}},
		{"no tool use", DraftFailure(bedrock, ontologyagent.ModelUnsupported),
			Copy{Message: "us.anthropic.claude-opus-5-5 can’t do what this task needs (tool calls or structured answers).", Permanent: true}},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.name, tc.got, tc.want)
		}
	}
	if got, stop := DocumentFailure(bedrock, ontologyagent.ModelPermission); !stop ||
		got.Message != "Amazon Bedrock refused access to amazon.nova-lite-v1:0 or us.anthropic.claude-sonnet-5-5. An owner can choose another model in Settings › AI models." {
		t.Errorf("a Bedrock load = %+v, %v", got, stop)
	}
	if got, stop := DocumentFailure(bedrock, ontologyagent.ModelRateLimited); stop || got != (Copy{}) {
		t.Errorf("a throttled Bedrock document = %+v, %v: the next attempt may pass", got, stop)
	}
	if got := TitleFailure(bedrock, ontologyagent.ModelAuth); got != (Copy{}) {
		t.Errorf("a Bedrock title = %+v: the owner's provider is no one's on-call", got)
	}
}
