package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/connector"
)

func ptr[T any](v T) *T { return &v }

// claudeRoles are Claude's suggested models.
func claudeRoles(t *testing.T) Roles {
	t.Helper()
	roles, ok := Suggested(KindAnthropic)
	if !ok {
		t.Fatal("Claude suggests no models")
	}
	return roles
}

// every is a model per role, all of them m.
func every(m Model) Roles { return Roles{Draft: m, Ask: m, Extract: m, Verify: m} }

func TestSettingsJSON(t *testing.T) {
	// Every kind and every price form survives the settings column as it was
	// saved, so a config is read back exactly as the probe tested it.
	t.Run("round-trips every kind and price", func(t *testing.T) {
		for _, s := range []Settings{
			{Provider: Anthropic{}, Models: claudeRoles(t)},
			{Provider: OpenAI{}, Models: every(Model{Name: "gpt-6.1", Price: PriceRates{Input: 1250, Output: 10_000, CacheRead: ptr(uint64(125))}})},
			{Provider: AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai/v1"}, Models: every(Model{Name: "prod-gpt", Price: PriceTableAs{Model: "gpt-6.1"}})},
			{Provider: Bedrock{Region: "eu-west-1", FIPS: true}, Models: every(Model{Name: "us.anthropic.claude-sonnet-5-5", Price: PriceTable{}})},
			{Provider: Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: 65536}, Models: every(Model{Name: "qwen3:32b", Price: PriceFree{}})},
			{Provider: Compatible{BaseURL: "https://llm.example.com/v1", API: CompatResponses, MaxCallsInFlight: 4}, Models: every(Model{Name: "m", Price: PriceRates{Input: 0, Output: 1}})},
		} {
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatalf("%T: %v", s.Provider, err)
			}
			if strings.Contains(string(raw), "key") {
				t.Fatalf("settings JSON names a key: %s", raw)
			}
			back, err := DecodeSettings(raw)
			if err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			again, _ := json.Marshal(back)
			if string(again) != string(raw) {
				t.Fatalf("round trip: %s, then %s", raw, again)
			}
		}
	})

	// Settings with a part missing can't be stored.
	t.Run("refuses to encode incomplete settings", func(t *testing.T) {
		if _, err := json.Marshal(Settings{Models: claudeRoles(t)}); err == nil {
			t.Fatal("settings without a provider encoded")
		}
		if _, err := json.Marshal(Settings{Provider: Anthropic{}, Models: every(Model{Name: "m"})}); err == nil {
			t.Fatal("a model without a price encoded")
		}
	})

	// The column is written by this package alone; anything else in it is a
	// fault to surface, not a shape to guess at.
	t.Run("decodes strictly", func(t *testing.T) {
		const claude = `"draft":{"name":"m","price":"table"},"ask":{"name":"m","price":"table"},"extract":{"name":"m","price":"table"},"verify":{"name":"m","price":"table"}`
		if _, err := DecodeSettings([]byte(`{"v":1,"provider":{"kind":"anthropic"},"models":{` + claude + `}}`)); err != nil {
			t.Fatalf("a valid row: %v", err)
		}
		for name, raw := range map[string]string{
			"another version":        `{"v":2,"provider":{"kind":"anthropic"},"models":{` + claude + `}}`,
			"an unknown kind":        `{"v":1,"provider":{"kind":"gemini"},"models":{` + claude + `}}`,
			"another kind's field":   `{"v":1,"provider":{"kind":"anthropic","base_url":"https://x"},"models":{` + claude + `}}`,
			"an unknown field":       `{"v":1,"provider":{"kind":"anthropic"},"models":{` + claude + `},"api_key":"sk"}`,
			"a missing role":         `{"v":1,"provider":{"kind":"anthropic"},"models":{"draft":{"name":"m","price":"table"}}}`,
			"an unknown price":       `{"v":1,"provider":{"kind":"anthropic"},"models":{` + strings.Replace(claude, `"table"`, `"cheap"`, 1) + `}}`,
			"two prices":             `{"v":1,"provider":{"kind":"anthropic"},"models":{` + strings.Replace(claude, `"table"`, `{"table_as":"x","rates":{"input":1,"output":1}}`, 1) + `}}`,
			"trailing data":          `{"v":1,"provider":{"kind":"anthropic"},"models":{` + claude + `}} {}`,
			"a rate's unknown field": `{"v":1,"provider":{"kind":"anthropic"},"models":{` + strings.Replace(claude, `"table"`, `{"rates":{"input":1,"output":1,"cache_write":2}}`, 1) + `}}`,
		} {
			if _, err := DecodeSettings([]byte(raw)); err == nil {
				t.Errorf("%s decoded", name)
			}
		}
	})
}

func TestValidateSettings(t *testing.T) {
	hosted, local := Rules{}, Rules{AllowPrivate: true}
	valid := func(p Provider, price Price) Settings {
		return Settings{Provider: p, Models: every(Model{Name: "model-1", Price: price})}
	}
	field := func(err error) string {
		var bad *Invalid
		if errors.As(err, &bad) {
			return bad.Field
		}
		if errors.Is(err, ErrPrivateHost) {
			return "private"
		}
		return ""
	}

	// What each kind may be saved as, hosted and locally. Endpoints mirror
	// the agent's own checks, so a config the backend saves is one the agent
	// takes.
	for name, tc := range map[string]struct {
		settings Settings
		rules    Rules
		want     string // the field refused; "" accepted
	}{
		"Claude":                                {valid(Anthropic{}, PriceTable{}), hosted, ""},
		"Claude priced free":                    {valid(Anthropic{}, PriceFree{}), hosted, "models.draft.price"},
		"rates with a zero output rate":         {valid(OpenAI{}, PriceRates{Input: 1}), hosted, "models.draft.price.rates.output"},
		"no price":                              {valid(OpenAI{}, nil), hosted, "models.draft.price"},
		"OpenAI":                                {valid(OpenAI{}, PriceTable{}), hosted, ""},
		"OpenAI priced as another model":        {valid(OpenAI{}, PriceTableAs{Model: "gpt-6-luna"}), hosted, "models.draft.price"},
		"Claude priced as another model":        {valid(Anthropic{}, PriceTableAs{Model: "claude-haiku-5-5"}), hosted, "models.draft.price"},
		"OpenAI priced free":                    {valid(OpenAI{}, PriceFree{}), hosted, "models.draft.price"},
		"an Azure resource":                     {valid(AzureOpenAI{Endpoint: "https://acme.openai.azure.com"}, PriceTableAs{Model: "gpt-6.1"}), hosted, ""},
		"an Azure AI Services resource":         {valid(AzureOpenAI{Endpoint: "https://acme.cognitiveservices.azure.com/openai/v1"}, PriceRates{Input: 1, Output: 2}), hosted, ""},
		"an Azure deployment priced as nothing": {valid(AzureOpenAI{Endpoint: "https://acme.openai.azure.com"}, PriceTableAs{}), hosted, "models.draft.price.table_as"},
		"an Azure deployment priced free":       {valid(AzureOpenAI{Endpoint: "https://acme.openai.azure.com"}, PriceFree{}), hosted, "models.draft.price"},
		"an Azure suffix with no resource":      {valid(AzureOpenAI{Endpoint: "https://openai.azure.com"}, PriceTable{}), hosted, "provider.endpoint"},
		"an Azure endpoint with a user":         {valid(AzureOpenAI{Endpoint: "https://me@acme.openai.azure.com"}, PriceTable{}), hosted, "provider.endpoint"},
		"an Azure endpoint with a query":        {valid(AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai/v1?api-version=2024-10-21"}, PriceTable{}), hosted, "provider.endpoint"},
		"an Azure endpoint with a fragment":     {valid(AzureOpenAI{Endpoint: "https://acme.openai.azure.com#x"}, PriceTable{}), hosted, "provider.endpoint"},
		"an Azure endpoint's older path":        {valid(AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai"}, PriceTable{}), hosted, "provider.endpoint"},
		"an Azure endpoint with no scheme":      {valid(AzureOpenAI{Endpoint: "acme.openai.azure.com"}, PriceTable{}), hosted, "provider.endpoint"},
		"an Azure endpoint at its IP":           {valid(AzureOpenAI{Endpoint: "https://20.0.0.1"}, PriceTable{}), local, "provider.endpoint"},
		"no Azure endpoint":                     {valid(AzureOpenAI{}, PriceTable{}), hosted, "provider.endpoint"},
		"an Azure AI Foundry resource":          {valid(AzureOpenAI{Endpoint: "https://acme.services.ai.azure.com/openai/v1/"}, PriceTable{}), hosted, ""},
		"an Azure lookalike":                    {valid(AzureOpenAI{Endpoint: "https://openai.azure.com.evil.example"}, PriceTable{}), hosted, "provider.endpoint"},
		"an Azure endpoint over http":           {valid(AzureOpenAI{Endpoint: "http://acme.openai.azure.com"}, PriceTable{}), local, "provider.endpoint"},
		"an Azure endpoint with a path":         {valid(AzureOpenAI{Endpoint: "https://acme.openai.azure.com/deployments/x"}, PriceTable{}), hosted, "provider.endpoint"},
		"an Azure endpoint with a port":         {valid(AzureOpenAI{Endpoint: "https://acme.openai.azure.com:8443"}, PriceTable{}), hosted, "provider.endpoint"},
		"a Bedrock region":                      {valid(Bedrock{Region: "us-gov-west-1", FIPS: true}, PriceTable{}), hosted, ""},
		"a Bedrock region that is a host":       {valid(Bedrock{Region: "evil.example/x"}, PriceTable{}), hosted, "provider.region"},
		"Ollama on this machine":                {valid(Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: 65536}, PriceFree{}), local, ""},
		"Ollama over http hosted":               {valid(Ollama{BaseURL: "http://ollama.example.com", ContextTokens: 65536}, PriceFree{}), hosted, "provider.base_url"},
		"Ollama at a private literal":           {valid(Ollama{BaseURL: "https://10.0.0.5", ContextTokens: 65536}, PriceFree{}), hosted, "private"},
		"Ollama at the metadata address":        {valid(Ollama{BaseURL: "https://169.254.169.254", ContextTokens: 65536}, PriceFree{}), hosted, "private"},
		"Ollama with no context":                {valid(Ollama{BaseURL: "http://127.0.0.1:11434"}, PriceFree{}), local, "provider.context_tokens"},
		"a compatible server":                   {valid(Compatible{BaseURL: "https://llm.example.com/v1", API: CompatChatCompletions, MaxCallsInFlight: 4}, PriceFree{}), hosted, ""},
		"a compatible server with a user":       {valid(Compatible{BaseURL: "https://me:pw@llm.example.com/v1", API: CompatChatCompletions, MaxCallsInFlight: 4}, PriceFree{}), hosted, "provider.base_url"},
		"a compatible server with a query":      {valid(Compatible{BaseURL: "https://llm.example.com/v1?key=x", API: CompatChatCompletions, MaxCallsInFlight: 4}, PriceFree{}), hosted, "provider.base_url"},
		"a compatible server's unknown API":     {valid(Compatible{BaseURL: "https://llm.example.com/v1", API: "anthropic_messages", MaxCallsInFlight: 4}, PriceFree{}), hosted, "provider.api"},
		"a compatible server with no slots":     {valid(Compatible{BaseURL: "https://llm.example.com/v1", API: CompatResponses}, PriceFree{}), hosted, "provider.max_calls_in_flight"},
		"a file URL":                            {valid(Compatible{BaseURL: "file:///etc/passwd", API: CompatResponses, MaxCallsInFlight: 1}, PriceFree{}), local, "provider.base_url"},
		"no provider":                           {Settings{Models: every(Model{Name: "m", Price: PriceTable{}})}, hosted, "provider"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := field(tc.settings.Validate(tc.rules)); got != tc.want {
				t.Fatalf("Validate() refused %q, want %q", got, tc.want)
			}
		})
	}

	// A model name is an identifier the provider looks up: never text a
	// prompt or a key could hide in.
	t.Run("checks model names", func(t *testing.T) {
		for name, ok := range map[string]bool{
			"claude-opus-5-5":                     true,
			"us.anthropic.claude-sonnet-5-5-v1:0": true,
			"qwen3:32b":                           true,
			strings.Repeat("m", MaxModelName):     true,
			"":                                    false,
			"two words":                           false,
			"tab\tname":                           false,
			"bell\a":                              false,
			strings.Repeat("m", MaxModelName+1):   false,
			"sk-ant-api03-0123456789abcdef":       false,
			"sk-proj-0123456789abcdef":            false,
			"ABSKQmVkcm9ja0FQSUtleQ":              false,
			"bedrock-api-key-YmVkcm9jay5hbWF6b24": false,
			"gpt-6.1-sk-mini":                     true,
		} {
			if got := ModelName(name) == nil; got != ok {
				t.Errorf("ModelName(%q) ok = %v, want %v", name, got, ok)
			}
		}
	})

	// A hosted endpoint is dialed from Helix's network, so a name that
	// resolves inward is refused at save; locally it is the point.
	t.Run("refuses a host that resolves to a private address", func(t *testing.T) {
		s := valid(Compatible{BaseURL: "https://localhost/v1", API: CompatResponses, MaxCallsInFlight: 1}, PriceFree{})
		if err := s.CheckHosts(context.Background(), connector.Guard{}); !errors.Is(err, ErrPrivateHost) {
			t.Fatalf("CheckHosts() = %v", err)
		}
		if err := s.CheckHosts(context.Background(), connector.Guard{AllowPrivate: true}); err != nil {
			t.Fatalf("CheckHosts() locally = %v", err)
		}
		if hosts := valid(Anthropic{}, PriceTable{}).Hosts(); hosts != nil {
			t.Fatalf("Claude's host is Helix's to know: %v", hosts)
		}
		for want, p := range map[string]Provider{
			"acme.openai.azure.com": AzureOpenAI{Endpoint: "https://acme.openai.azure.com"},
			"10.0.0.5":              Ollama{BaseURL: "http://10.0.0.5:11434"},
		} {
			if got := valid(p, PriceFree{}).Hosts(); len(got) != 1 || got[0] != want {
				t.Errorf("Hosts() = %v, want %s", got, want)
			}
		}
	})

	// Saved endpoints are stored in one form, which key reuse compares.
	t.Run("normalizes endpoints", func(t *testing.T) {
		s := Settings{Provider: Compatible{BaseURL: "https://LLM.Example.com/v1/", API: CompatResponses, MaxCallsInFlight: 1}}
		if got := s.Normalized(hosted).Provider.(Compatible).BaseURL; got != "https://llm.example.com/v1" {
			t.Fatalf("Normalized() = %q", got)
		}
		for raw, want := range map[string]string{
			"https://Acme.openai.azure.com/":                     "https://acme.openai.azure.com/openai/v1",
			"https://acme.openai.azure.com/openai/v1/":           "https://acme.openai.azure.com/openai/v1",
			"https://ACME.cognitiveservices.azure.com/openai/v1": "https://acme.cognitiveservices.azure.com/openai/v1",
			"https://acme.services.ai.azure.com":                 "https://acme.services.ai.azure.com/openai/v1",
		} {
			a := Settings{Provider: AzureOpenAI{Endpoint: raw}}
			if got := a.Normalized(hosted).Provider.(AzureOpenAI).Endpoint; got != want {
				t.Errorf("Normalized(%s) = %q, want %q", raw, got, want)
			}
		}
	})
}

func TestCanReuseKey(t *testing.T) {
	model := func(name string) Roles { return every(Model{Name: name, Price: PriceTable{}}) }
	azure := func(endpoint, deployment string) Settings {
		return Settings{Provider: AzureOpenAI{Endpoint: endpoint}, Models: model(deployment)}
	}
	compat := func(base string) Settings {
		return Settings{Provider: Compatible{BaseURL: base, API: CompatChatCompletions, MaxCallsInFlight: 4}, Models: model("m")}
	}
	// A blank key keeps the stored one only where the stored key still
	// belongs to where the calls go: never sent to an endpoint the owner
	// didn't give it to.
	for name, tc := range map[string]struct {
		active, next Settings
		want         bool
	}{
		"Claude, other models":                {Settings{Provider: Anthropic{}, Models: model("a")}, Settings{Provider: Anthropic{}, Models: model("b")}, true},
		"OpenAI to OpenAI":                    {Settings{Provider: OpenAI{}}, Settings{Provider: OpenAI{}}, true},
		"Bedrock in another region":           {Settings{Provider: Bedrock{Region: "us-east-1"}}, Settings{Provider: Bedrock{Region: "eu-west-1"}}, true},
		"Claude to OpenAI":                    {Settings{Provider: Anthropic{}}, Settings{Provider: OpenAI{}}, false},
		"Azure, same resource and deployment": {azure("https://a.openai.azure.com", "prod"), azure("https://A.openai.azure.com/openai/v1/", "prod"), true},
		"Azure, another deployment":           {azure("https://a.openai.azure.com", "prod"), azure("https://a.openai.azure.com", "test"), false},
		"Azure, other deployments but drafts": {azure("https://a.openai.azure.com", "prod"), func() Settings {
			s := azure("https://a.openai.azure.com", "prod")
			s.Models.Extract.Name = "prod-luna"
			return s
		}(), true},
		"Azure, another host of a resource": {azure("https://a.openai.azure.com", "prod"), azure("https://a.cognitiveservices.azure.com", "prod"), false},
		"OpenAI to Azure":                   {Settings{Provider: OpenAI{}, Models: model("prod")}, azure("https://a.openai.azure.com", "prod"), false},
		"Azure to OpenAI":                   {azure("https://a.openai.azure.com", "prod"), Settings{Provider: OpenAI{}, Models: model("prod")}, false},
		"OpenAI to Claude":                  {Settings{Provider: OpenAI{}}, Settings{Provider: Anthropic{}}, false},
		"Azure, another resource":           {azure("https://a.openai.azure.com", "prod"), azure("https://b.openai.azure.com", "prod"), false},
		"compatible, same URL":              {compat("https://llm.example.com/v1"), compat("https://LLM.example.com/v1/"), true},
		"compatible, another URL":           {compat("https://llm.example.com/v1"), compat("https://evil.example.com/v1"), false},
		"compatible to OpenAI":              {compat("https://api.openai.com/v1"), Settings{Provider: OpenAI{}}, false},
		"Ollama":                            {Settings{Provider: Ollama{BaseURL: "http://127.0.0.1:11434"}}, Settings{Provider: Ollama{BaseURL: "http://127.0.0.1:11434"}}, false},
		"nothing active":                    {Settings{}, Settings{Provider: Anthropic{}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := CanReuseKey(tc.active, tc.next); got != tc.want {
				t.Fatalf("CanReuseKey() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A Bedrock region is checked as the agent's AwsRegion checks it, since the
// agent builds the runtime's host from it: AWS's commercial, GovCloud and
// China regions pass, and nothing that could steer the host does. No host
// is typed, so a save resolves none.
func TestValidateBedrock(t *testing.T) {
	hosted, local := Rules{}, Rules{AllowPrivate: true}
	settings := func(region string, fips bool) Settings {
		return Settings{Provider: Bedrock{Region: region, FIPS: fips}, Models: every(Model{Name: "global.anthropic.claude-sonnet-5-5", Price: PriceTable{}})}
	}
	for region, ok := range map[string]bool{
		"us-east-1": true, "eu-central-2": true, "ap-southeast-2": true, "ca-west-1": true, "il-central-1": true, "mx-central-1": true,
		"us-gov-west-1": true, "cn-north-1": true, "cn-northwest-1": true,
		// The agent's own refusals (models::AwsRegion), and a few more.
		"us-east": false, "US-EAST-1": false, "us-east-1.evil.example": false, "u-east-1": false, "us--1": false,
		"": false, "us-1": false, "us-east-1a": false, "us_east_1": false, "us-east-1/": false, " us-east-1": false,
		"us-east-1\n": false, "bedrock-runtime.us-east-1.amazonaws.com": false, "us-east-١": false, "usa-east-1": false,
	} {
		if err := settings(region, false).Validate(hosted); (err == nil) != ok {
			t.Errorf("Validate(region %q) = %v, want ok %v", region, err, ok)
		}
	}
	for _, rules := range []Rules{hosted, local} {
		s := settings("us-east-1", true)
		if err := s.Validate(rules); err != nil || s.Normalized(rules) != s || s.Hosts() != nil {
			t.Fatalf("a FIPS region under %+v: %v, %+v, hosts %v", rules, err, s.Normalized(rules), s.Hosts())
		}
		if err := s.CheckHosts(context.Background(), connector.Guard{}); err != nil {
			t.Fatalf("CheckHosts() = %v: Bedrock's host is AWS's", err)
		}
	}
}
