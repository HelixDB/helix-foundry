package ai_test

import (
	"fmt"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
)

func ExampleParseKind() {
	kind, err := ai.ParseKind("azure_openai")
	fmt.Println(kind, err)
	_, err = ai.ParseKind("gemini")
	fmt.Println(err)
	// Output:
	// azure_openai <nil>
	// "gemini" is not a model provider kind
}

func ExampleModelURL() {
	hosted, local := ai.Rules{}, ai.Rules{AllowPrivate: true}
	u, _ := ai.ModelURL("https://LLM.example.com/v1/", hosted)
	fmt.Println(u)
	_, err := ai.ModelURL("http://127.0.0.1:11434", hosted)
	fmt.Println(err)
	u, _ = ai.ModelURL("http://127.0.0.1:11434", local)
	fmt.Println(u)
	// Output:
	// https://llm.example.com/v1
	// provider.base_url must be https
	// http://127.0.0.1:11434
}

// An OpenAI-compatible server's base URL is stored in one form, which key
// reuse compares, and its host is one the agent reads as Go does.
func ExampleModelURL_normalForm() {
	hosted := ai.Rules{}
	u, _ := ai.ModelURL("HTTPS://LLM.Example.com:443/openai/v1/", hosted)
	fmt.Println(u)
	_, err := ai.ModelURL("https://192.0.2.1/v1", hosted)
	fmt.Println(err)
	_, err = ai.ModelURL("https://0x7f.1/v1", ai.Rules{AllowPrivate: true})
	fmt.Println(err)
	// Output:
	// https://llm.example.com/openai/v1
	// the endpoint resolves to a private address
	// provider.base_url must be an http or https URL with a host
}

func ExampleAzureEndpoint() {
	endpoint, _ := ai.AzureEndpoint("https://acme.openai.azure.com/")
	fmt.Println(endpoint)
	_, err := ai.AzureEndpoint("https://acme.example.com")
	fmt.Println(err)
	// Output:
	// https://acme.openai.azure.com/openai/v1
	// provider.endpoint must be an Azure OpenAI endpoint, like https://<resource>.openai.azure.com
}

func ExampleCanReuseKey() {
	claude := ai.Settings{Provider: ai.Anthropic{}}
	openai := ai.Settings{Provider: ai.OpenAI{}}
	fmt.Println(ai.CanReuseKey(claude, claude), ai.CanReuseKey(claude, openai))
	// Output: true false
}

func ExampleCatalog_Lookup() {
	catalog, _ := ai.VendoredCatalog()
	e, ok := catalog.Lookup(ai.KindAnthropic, "claude-haiku-5-5")
	fmt.Println(e.Input, e.Output, ok)
	_, ok = catalog.Lookup(ai.KindAnthropic, "claude-unknown")
	fmt.Println(ok)
	// Output:
	// 100 500 true
	// false
}

// A Bedrock inference profile's id is priced by its own row, or else as
// the model it runs: Claude through a global profile at Anthropic's list
// price, through a geographic one at Bedrock's regional rate.
func ExampleCatalog_Lookup_bedrock() {
	catalog, _ := ai.VendoredCatalog()
	for _, id := range []string{"global.anthropic.claude-sonnet-5-5", "eu.anthropic.claude-sonnet-5-5", "us-gov.anthropic.claude-sonnet-5-5"} {
		e, ok := catalog.Lookup(ai.KindBedrock, id)
		fmt.Printf("%q %d %d %v\n", e.Model, e.Input, e.Output, ok)
	}
	// Output:
	// "global.anthropic.claude-sonnet-5-5" 2000 10000 true
	// "anthropic.claude-sonnet-5-5" 2200 11000 true
	// "" 0 0 false
}

func ExampleHint() {
	fmt.Println(ai.Hint(ontologyagent.NewModelKey("sk-ant-api03-0123456789abcdef4f2a")))
	fmt.Println(ai.Hint(ontologyagent.NewModelKey("0123456789abcdef0123456789ab4f2a")))
	fmt.Println(ai.Hint(ontologyagent.NewModelKey("short")))
	// Output:
	// sk-ant…4f2a
	// …4f2a
	// …
}

func ExampleDraftFailure() {
	house := ai.Resolved{Source: ai.SourceDefault, Settings: ai.Settings{Provider: ai.Anthropic{}}}
	fmt.Printf("%+v\n", ai.DraftFailure(house, ontologyagent.ModelAuth))
	own := ai.Resolved{Source: ai.SourceWorkspace, Settings: ai.Settings{Provider: ai.Anthropic{}}}
	fmt.Printf("%+v\n", ai.DraftFailure(own, ontologyagent.ModelAuth))
	// Output:
	// {Message:Claude is unavailable; Helix will retry Permanent:false Ops:true}
	// {Message:Claude refused the API key. An owner can enter a new one in Settings › AI models. Permanent:true Ops:false}
}
