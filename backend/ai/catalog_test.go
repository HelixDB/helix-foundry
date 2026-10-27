package ai

import (
	"testing"
)

func TestCatalog(t *testing.T) {
	// The vendored catalog prices Claude exactly as the agent's meter did
	// before the catalog existed (llm::messages::Price::of at 7d586b1): a
	// price that moved would bill hosted runs differently.
	t.Run("prices Claude as the agent always has", func(t *testing.T) {
		c, err := VendoredCatalog()
		if err != nil {
			t.Fatal(err)
		}
		for model, want := range map[string]Entry{
			"claude-opus-5-5":   {Input: 4_000, Output: 20_000, CacheRead: 200},
			"claude-opus-5":     {Input: 5_000, Output: 25_000, CacheRead: 500},
			"claude-opus-4-8":   {Input: 5_000, Output: 25_000, CacheRead: 500},
			"claude-sonnet-5-5": {Input: 2_000, Output: 10_000, CacheRead: 200},
			"claude-haiku-5-5":  {Input: 100, Output: 500, CacheRead: 10},
		} {
			got, ok := c.Lookup(KindAnthropic, model)
			want.Kind, want.Model = KindAnthropic, model
			if !ok || got != want {
				t.Errorf("Lookup(%s) = %+v, %v; want %+v", model, got, ok, want)
			}
		}
		claude := 0
		for key := range c.entries {
			if key.kind == KindAnthropic {
				claude++
			}
		}
		if claude != 5 {
			t.Errorf("the catalog has %d Claude entries, want 5", claude)
		}
		if _, ok := c.Lookup(KindOpenAI, "claude-opus-5-5"); ok {
			t.Error("a model is priced under another kind")
		}
	})

	// OpenAI's models are priced at OpenAI's list rates, and an Azure
	// deployment priced as one of them at the same: the agent prices Azure
	// from OpenAI's rows, so Azure has none of its own.
	t.Run("prices OpenAI's models and Azure's deployments of them", func(t *testing.T) {
		c, err := VendoredCatalog()
		if err != nil {
			t.Fatal(err)
		}
		for model, want := range map[string]Entry{
			"gpt-6-astra":   {Input: 10_000, Output: 50_000, CacheRead: 1_000},
			"gpt-6.1-sol":   {Input: 2_000, Output: 10_000, CacheRead: 100},
			"gpt-6-luna":    {Input: 100, Output: 500, CacheRead: 10},
			"gpt-5.6-terra": {Input: 2_000, Output: 12_000, CacheRead: 200},
		} {
			want.Kind, want.Model = KindOpenAI, model
			for _, kind := range []Kind{KindOpenAI, KindAzureOpenAI} {
				if got, ok := c.Lookup(kind, model); !ok || got != want {
					t.Errorf("Lookup(%s, %s) = %+v, %v; want %+v", kind, model, got, ok, want)
				}
			}
		}
		if len(c.entries) != 9+14 {
			t.Errorf("the catalog has %d entries, want Claude's 5, OpenAI's 4 and Bedrock's 14", len(c.entries))
		}
		if _, ok := c.Lookup(KindAzureOpenAI, "my-deployment"); ok {
			t.Error("a deployment is priced by its own name")
		}
		if _, ok := c.Lookup(KindAnthropic, "gpt-6.1-sol"); ok {
			t.Error("an OpenAI model is priced as Claude")
		}
		if _, ok := c.Lookup(KindCompatible, "gpt-6.1-sol"); ok {
			t.Error("a custom server's model is priced as OpenAI's")
		}
		if _, err := ParseCatalog([]byte(`{"models":[{"kind":"azure_openai","model":"gpt-6.1-sol","output":1}]}`)); err == nil {
			t.Error("a catalog with Azure rows of its own parsed")
		}
	})

	// A kind's suggested models start a save priced: Claude's are Helix's
	// Claude's, so a workspace that brings its own key starts where the house
	// is. Azure deployments and custom servers' models are the owner's to name.
	t.Run("suggests priced models", func(t *testing.T) {
		c, _ := VendoredCatalog()
		for kind, provider := range map[Kind]Provider{KindAnthropic: Anthropic{}, KindOpenAI: OpenAI{}, KindBedrock: Bedrock{Region: "us-east-1"}} {
			roles, ok := Suggested(kind)
			s := Settings{Provider: provider, Models: roles}
			if !ok || len(c.Unpriced(s)) > 0 || s.Validate(Rules{}) != nil {
				t.Fatalf("Suggested(%s) = %+v, %v", kind, roles, ok)
			}
		}
		if roles, _ := Suggested(KindAnthropic); roles != claude.Models {
			t.Fatalf("Claude's suggestions = %+v, want Helix's Claude's", roles)
		}
		for _, kind := range []Kind{KindAzureOpenAI, KindOllama, KindCompatible} {
			if _, ok := Suggested(kind); ok {
				t.Errorf("Suggested(%s) names models its owner names", kind)
			}
		}
	})

	// A table price must name a model the agent can price, or every call is
	// refused as unpriced; rates and free need no row.
	t.Run("lists the roles priced by a missing row", func(t *testing.T) {
		c, _ := VendoredCatalog()
		s := Settings{Provider: AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai/v1"}, Models: Roles{
			Draft:   Model{Name: "gpt-6.1-sol", Price: PriceTable{}},
			Ask:     Model{Name: "prod-chat", Price: PriceTable{}},
			Extract: Model{Name: "prod-luna", Price: PriceTableAs{Model: "gpt-6-luna"}},
			Verify:  Model{Name: "prod-sol", Price: PriceTableAs{Model: "gpt-gone"}},
		}}
		if got := c.Unpriced(s); len(got) != 2 || got[0] != RoleAsk || got[1] != RoleVerify {
			t.Fatalf("Unpriced() = %v", got)
		}
		s.Models.Ask.Price, s.Models.Verify.Price = PriceRates{Input: 1, Output: 2}, PriceRates{Input: 1, Output: 2}
		if got := c.Unpriced(s); len(got) != 0 {
			t.Fatalf("Unpriced() = %v", got)
		}
	})

	// A Bedrock inference profile is priced as its model.
	t.Run("prices a Bedrock profile as its model", func(t *testing.T) {
		c, err := ParseCatalog([]byte(`{"models":[{"kind":"bedrock","model":"anthropic.claude-sonnet-5-5","input":2000,"output":10000,"source_url":"x"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"anthropic.claude-sonnet-5-5", "us.anthropic.claude-sonnet-5-5", "global.anthropic.claude-sonnet-5-5"} {
			if _, ok := c.Lookup(KindBedrock, id); !ok {
				t.Errorf("Lookup(%s) missed", id)
			}
		}
		if _, ok := c.Lookup(KindBedrock, "xx.anthropic.claude-sonnet-5-5"); ok {
			t.Error("an unknown prefix is priced")
		}
	})

	// Bedrock's rows are AWS's on-demand prices: Claude at Anthropic's list
	// price through a global inference profile (Sonnet's cache reads aside)
	// and 10% more in one region or through a geographic profile, rounded up
	// to a whole nano-dollar; Nova and Mistral beside them.
	t.Run("prices Bedrock's models as AWS lists them", func(t *testing.T) {
		c, err := VendoredCatalog()
		if err != nil {
			t.Fatal(err)
		}
		listed := map[string]Entry{
			"global.anthropic.claude-opus-5-5":      {Input: 4_000, Output: 20_000, CacheRead: 200},
			"anthropic.claude-opus-5-5":             {Input: 4_400, Output: 22_000, CacheRead: 220},
			"global.anthropic.claude-opus-5":        {Input: 5_000, Output: 25_000, CacheRead: 500},
			"anthropic.claude-opus-5":               {Input: 5_500, Output: 27_500, CacheRead: 550},
			"global.anthropic.claude-opus-4-8":      {Input: 5_000, Output: 25_000, CacheRead: 500},
			"anthropic.claude-opus-4-8":             {Input: 5_500, Output: 27_500, CacheRead: 550},
			"global.anthropic.claude-sonnet-5-5":    {Input: 2_000, Output: 10_000, CacheRead: 100},
			"anthropic.claude-sonnet-5-5":           {Input: 2_200, Output: 11_000, CacheRead: 110},
			"global.anthropic.claude-haiku-5-5":     {Input: 100, Output: 500, CacheRead: 10},
			"anthropic.claude-haiku-5-5":            {Input: 110, Output: 550, CacheRead: 11},
			"global.amazon.nova-2-lite-v1:0":        {Input: 300, Output: 2_500, CacheRead: 75},
			"amazon.nova-2-lite-v1:0":               {Input: 330, Output: 2_750, CacheRead: 83},
			"amazon.nova-pro-v1:0":                  {Input: 800, Output: 3_200, CacheRead: 200},
			"mistral.mistral-large-3-675b-instruct": {Input: 500, Output: 1_500, CacheRead: 500},
		}
		bedrock := 0
		for key := range c.entries {
			if key.kind == KindBedrock {
				bedrock++
			}
		}
		if bedrock != len(listed) {
			t.Errorf("the catalog has %d Bedrock entries, want %d", bedrock, len(listed))
		}
		for model, want := range listed {
			want.Kind, want.Model = KindBedrock, model
			if got, ok := c.Lookup(KindBedrock, model); !ok || got != want {
				t.Errorf("Lookup(%s) = %+v, %v; want %+v", model, got, ok, want)
			}
		}
		// A profile is priced by its own row, or else as the model it runs:
		// Claude in one region or through a geographic profile at the
		// regional rate, through a global one at the global rate.
		for _, profile := range []string{"", "us.", "eu.", "apac.", "au.", "jp.", "ca."} {
			if got, ok := c.Lookup(KindBedrock, profile+"anthropic.claude-sonnet-5-5"); !ok || got.Model != "anthropic.claude-sonnet-5-5" {
				t.Errorf("Lookup(%santhropic.claude-sonnet-5-5) = %+v, %v", profile, got, ok)
			}
		}
		if got, _ := c.Lookup(KindBedrock, "global.anthropic.claude-sonnet-5-5"); got.Model != "global.anthropic.claude-sonnet-5-5" {
			t.Errorf("a global profile is priced as %s", got.Model)
		}
		if got, ok := c.Lookup(KindBedrock, "global.amazon.nova-pro-v1:0"); !ok || got.Model != "amazon.nova-pro-v1:0" {
			t.Errorf("a global profile with no row = %+v, %v", got, ok)
		}
		// GovCloud costs more than any row, a prefix is read past once, and
		// an application inference profile's ARN names no model: each needs
		// table_as or rates.
		for _, unpriced := range []string{
			"us-gov.anthropic.claude-sonnet-5-5", "us.us.anthropic.claude-sonnet-5-5", "anthropic.claude-sonnet-5-5-v2", "claude-sonnet-5-5",
			"arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/abc",
		} {
			if got, ok := c.Lookup(KindBedrock, unpriced); ok {
				t.Errorf("Lookup(%s) = %+v", unpriced, got)
			}
		}
		// Profiles are Bedrock's alone.
		if _, ok := c.Lookup(KindAnthropic, "us.claude-opus-5-5"); ok {
			t.Error("a Claude id is read past a profile's prefix")
		}
		if _, ok := c.Lookup(KindOpenAI, "global.gpt-6.1-sol"); ok {
			t.Error("an OpenAI id is read past a profile's prefix")
		}
	})

	// Bedrock starts from Helix's Claude's models, through global inference
	// profiles, each with a row of its own; a save may price a model as
	// another's, a profile's id included.
	t.Run("suggests Claude on Bedrock", func(t *testing.T) {
		c, _ := VendoredCatalog()
		roles, ok := Suggested(KindBedrock)
		if !ok {
			t.Fatal("Bedrock suggests no models")
		}
		for i, rm := range roles.Each() {
			house := claude.Models.Each()[i].Model.Name
			if rm.Model != (Model{Name: "global.anthropic." + house, Price: PriceTable{}}) {
				t.Errorf("Suggested(bedrock).%s = %+v, want Claude's %s through a global profile", rm.Role, rm.Model, house)
			}
			if got, ok := c.Lookup(KindBedrock, rm.Model.Name); !ok || got.Model != rm.Model.Name {
				t.Errorf("%s is priced as %+v, %v; want its own row", rm.Model.Name, got, ok)
			}
		}
		s := Settings{Provider: Bedrock{Region: "eu-west-1"}, Models: Roles{
			Draft:   Model{Name: "eu.anthropic.claude-opus-5-5", Price: PriceTable{}},
			Ask:     Model{Name: "arn:aws:bedrock:eu-west-1:123456789012:application-inference-profile/abc", Price: PriceTableAs{Model: "eu.anthropic.claude-sonnet-5-5"}},
			Extract: Model{Name: "us-gov.anthropic.claude-haiku-5-5", Price: PriceTableAs{Model: "us-gov.anthropic.claude-haiku-5-5"}},
			Verify:  Model{Name: "amazon.nova-pro-v1:0", Price: PriceTable{}},
		}}
		if got := c.Unpriced(s); len(got) != 1 || got[0] != RoleExtract {
			t.Fatalf("Unpriced() = %v, want GovCloud's extract model", got)
		}
	})

	// A bad vendored catalog fails the process at start, not a run.
	t.Run("refuses a malformed catalog", func(t *testing.T) {
		for name, raw := range map[string]string{
			"no models":       `{"entries":[]}`,
			"a bare list":     `[{"kind":"anthropic","model":"m","output":1}]`,
			"an unknown kind": `{"models":[{"kind":"gemini","model":"m","output":1}]}`,
			"no output rate":  `{"models":[{"kind":"anthropic","model":"m","input":1}]}`,
			"no model":        `{"models":[{"kind":"anthropic","output":1}]}`,
			"a duplicate":     `{"models":[{"kind":"anthropic","model":"m","output":1},{"kind":"anthropic","model":"m","output":2}]}`,
		} {
			if _, err := ParseCatalog([]byte(raw)); err == nil {
				t.Errorf("%s parsed", name)
			}
		}
	})
}
