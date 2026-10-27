package ontologyagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// models.go is what every model call of the contract names: the endpoint a
// request's models run on, and a ModelSpec per role (turn_request.json,
// ask_request.json, extract_request.json, title_request.json and
// probe_request.json share them). The endpoint's key never goes here: it
// travels in the call's metadata (ModelKeyHeader).

// ModelKeyHeader is the call metadata the agent takes a model key from.
const ModelKeyHeader = "x-foundry-model-key"

// ProviderKind is the kind of provider a request's models run on, as the
// contract names it.
type ProviderKind string

const (
	KindAnthropic        ProviderKind = "anthropic"
	KindOpenAI           ProviderKind = "openai"
	KindAzureOpenAI      ProviderKind = "azure_openai"
	KindBedrock          ProviderKind = "bedrock"
	KindOllama           ProviderKind = "ollama"
	KindOpenAICompatible ProviderKind = "openai_compatible"
)

// ProviderKinds are every kind the contract declares.
var ProviderKinds = []ProviderKind{KindAnthropic, KindOpenAI, KindAzureOpenAI, KindBedrock, KindOllama, KindOpenAICompatible}

// Provider is who serves a request's models, and where: one of Anthropic,
// OpenAI, AzureOpenAI, Bedrock, Ollama and OpenAICompatible.
type Provider interface {
	Kind() ProviderKind
	// wire is the provider as the contract writes it.
	wire() any
}

// Anthropic is Anthropic's API.
type Anthropic struct{}

// OpenAI is OpenAI's API.
type OpenAI struct{}

// AzureOpenAI is an Azure OpenAI resource at Endpoint, its
// https://<resource>.openai.azure.com/openai/v1; each model is a deployment.
type AzureOpenAI struct{ Endpoint string }

// Bedrock is Amazon Bedrock's runtime in Region, its FIPS endpoint if FIPS.
type Bedrock struct {
	Region string
	FIPS   bool
}

// Ollama is an Ollama server's native API at BaseURL.
type Ollama struct{ BaseURL string }

// CompatAPI is the OpenAI API an OpenAI-compatible server speaks.
type CompatAPI string

const (
	CompatChatCompletions CompatAPI = "chat_completions"
	CompatResponses       CompatAPI = "responses"
)

// OpenAICompatible is a server at BaseURL (its version path included)
// speaking API.
type OpenAICompatible struct {
	BaseURL string
	API     CompatAPI
}

func (Anthropic) Kind() ProviderKind        { return KindAnthropic }
func (OpenAI) Kind() ProviderKind           { return KindOpenAI }
func (AzureOpenAI) Kind() ProviderKind      { return KindAzureOpenAI }
func (Bedrock) Kind() ProviderKind          { return KindBedrock }
func (Ollama) Kind() ProviderKind           { return KindOllama }
func (OpenAICompatible) Kind() ProviderKind { return KindOpenAICompatible }

func (Anthropic) wire() any { return KindAnthropic }
func (OpenAI) wire() any    { return KindOpenAI }
func (p AzureOpenAI) wire() any {
	return map[ProviderKind]any{KindAzureOpenAI: struct {
		Endpoint string `json:"endpoint"`
	}{p.Endpoint}}
}
func (p Bedrock) wire() any {
	return map[ProviderKind]any{KindBedrock: struct {
		Region string `json:"region"`
		FIPS   bool   `json:"fips"`
	}{p.Region, p.FIPS}}
}
func (p Ollama) wire() any {
	return map[ProviderKind]any{KindOllama: struct {
		BaseURL string `json:"base_url"`
	}{p.BaseURL}}
}
func (p OpenAICompatible) wire() any {
	return map[ProviderKind]any{KindOpenAICompatible: struct {
		BaseURL string    `json:"base_url"`
		API     CompatAPI `json:"api"`
	}{p.BaseURL, p.API}}
}

// Endpoint is the endpoint every model of a request is called on.
type Endpoint struct {
	Provider Provider
	// CallTimeoutSecs bounds one JSON or text call's attempt, from
	// connecting to its last byte.
	CallTimeoutSecs uint32
	// MaxCallsInFlight bounds the endpoint's calls at once across every
	// request; 0 is no bound.
	MaxCallsInFlight uint16
}

// MarshalJSON writes the endpoint as the contract does.
func (e Endpoint) MarshalJSON() ([]byte, error) {
	if e.Provider == nil {
		return nil, errors.New("ontologyagent: an endpoint has no provider")
	}
	out := struct {
		Provider         any     `json:"provider"`
		CallTimeoutSecs  uint32  `json:"call_timeout_secs"`
		MaxCallsInFlight *uint16 `json:"max_calls_in_flight,omitempty"`
	}{Provider: e.Provider.wire(), CallTimeoutSecs: e.CallTimeoutSecs}
	if e.MaxCallsInFlight > 0 {
		out.MaxCallsInFlight = &e.MaxCallsInFlight
	}
	return json.Marshal(out)
}

// Price is what a model's tokens cost: one of PriceTable, PriceTableAs,
// PriceRates and PriceFree.
type Price interface{ wire() any }

// PriceTable is the model's row in Helix's price table (catalog.json).
type PriceTable struct{}

// PriceTableAs is the row of Model, another model of the same kind.
type PriceTableAs struct{ Model string }

// PriceRates are per-token rates in nano-USD; a cache rate left nil is the
// input rate.
type PriceRates struct {
	Input      uint64
	Output     uint64
	CacheRead  *uint64
	CacheWrite *uint64
}

// PriceFree is a model that costs nothing: local or self-hosted.
type PriceFree struct{}

func (PriceTable) wire() any     { return "table" }
func (PriceFree) wire() any      { return "free" }
func (p PriceTableAs) wire() any { return map[string]string{"table_as": p.Model} }
func (p PriceRates) wire() any {
	return map[string]any{"rates": struct {
		Input      uint64  `json:"input"`
		Output     uint64  `json:"output"`
		CacheRead  *uint64 `json:"cache_read,omitempty"`
		CacheWrite *uint64 `json:"cache_write,omitempty"`
	}{p.Input, p.Output, p.CacheRead, p.CacheWrite}}
}

// ModelSpec is what a request runs a role on.
type ModelSpec struct {
	// Name is the model as the provider names it.
	Name  string
	Price Price
	// Reasoning sends the provider's reasoning knob.
	Reasoning bool
	// MaxOutputTokens caps one response under the task's own ceiling; 0 is
	// no cap of the spec's.
	MaxOutputTokens uint32
	// ContextTokens is Ollama's num_ctx, elsewhere a bound the prompt is
	// checked against; 0 is none.
	ContextTokens uint32
	// Revision is the Ollama model digest the settings were tested on; ""
	// for none.
	Revision string
}

// MarshalJSON writes the spec as the contract does.
func (s ModelSpec) MarshalJSON() ([]byte, error) {
	if s.Price == nil {
		return nil, fmt.Errorf("ontologyagent: model %q has no price", s.Name)
	}
	out := struct {
		Name            string  `json:"name"`
		Price           any     `json:"price"`
		Reasoning       bool    `json:"reasoning"`
		MaxOutputTokens *uint32 `json:"max_output_tokens,omitempty"`
		ContextTokens   *uint32 `json:"context_tokens,omitempty"`
		Revision        string  `json:"revision,omitempty"`
	}{Name: s.Name, Price: s.Price.wire(), Reasoning: s.Reasoning, Revision: s.Revision}
	if s.MaxOutputTokens > 0 {
		out.MaxOutputTokens = &s.MaxOutputTokens
	}
	if s.ContextTokens > 0 {
		out.ContextTokens = &s.ContextTokens
	}
	return json.Marshal(out)
}

// TurnModels are a turn's models: one drafts, and its extraction previews
// extract and verify.
type TurnModels struct {
	Endpoint Endpoint  `json:"endpoint"`
	Draft    ModelSpec `json:"draft"`
	Extract  ModelSpec `json:"extract"`
	Verify   ModelSpec `json:"verify"`
}

// AskModels are a question's model.
type AskModels struct {
	Endpoint Endpoint  `json:"endpoint"`
	Ask      ModelSpec `json:"ask"`
}

// ExtractModels are a document extraction's models.
type ExtractModels struct {
	Endpoint Endpoint  `json:"endpoint"`
	Extract  ModelSpec `json:"extract"`
	Verify   ModelSpec `json:"verify"`
}

// TitleModels are the model a generation's title is written by.
type TitleModels struct {
	Endpoint Endpoint  `json:"endpoint"`
	Title    ModelSpec `json:"title"`
}

// ModelRef is a model that ran, as events name it.
type ModelRef struct {
	Provider ProviderKind `json:"provider"`
	Name     string       `json:"name"`
}

// ModelFailure is why a model call failed for good: a turn's model_failed
// reason, a document's model failure, a title's or a probe check's failure.
type ModelFailure string

const (
	// ModelRefused is a model that declined the request.
	ModelRefused ModelFailure = "refused"
	// ModelUnavailable is a provider unavailable, overloaded or broken off,
	// past the agent's retries.
	ModelUnavailable    ModelFailure = "unavailable"
	ModelAuth           ModelFailure = "auth"
	ModelPermission     ModelFailure = "permission"
	ModelNotFound       ModelFailure = "not_found"
	ModelRateLimited    ModelFailure = "rate_limited"
	ModelQuota          ModelFailure = "quota"
	ModelContextTooLong ModelFailure = "context_too_long"
	ModelUnsupported    ModelFailure = "unsupported"
	ModelTruncated      ModelFailure = "truncated"
	ModelMalformed      ModelFailure = "malformed"
	ModelTimeout        ModelFailure = "timeout"
	ModelChanged        ModelFailure = "model_changed"
	ModelPrivateHost    ModelFailure = "private_host"
)

// ModelFailures are every ModelFailure the contract names.
var ModelFailures = []ModelFailure{
	ModelRefused, ModelUnavailable, ModelAuth, ModelPermission, ModelNotFound, ModelRateLimited, ModelQuota,
	ModelContextTooLong, ModelUnsupported, ModelTruncated, ModelMalformed, ModelTimeout, ModelChanged, ModelPrivateHost,
}

// UnmarshalJSON refuses a failure the contract doesn't name.
func (f *ModelFailure) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	if !slices.Contains(ModelFailures, ModelFailure(name)) {
		return fmt.Errorf("unknown model failure %q", name)
	}
	*f = ModelFailure(name)
	return nil
}
