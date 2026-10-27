// Package ai is where a Foundry workspace's model calls go: the provider an
// owner saves in Settings › AI models, the rules it must meet, the
// deployment's default (Helix's Claude when hosted), the price catalog, and
// the resolver every model call asks for its provider, models and key.
//
// A saved provider is a Settings: one struct per provider kind and a model
// per task. Its JSON is the foundry_ai_configs.settings column and never
// holds a key; keys are sealed beside it (seal.go).
package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Kind is a model provider kind, named as the ontology agent's contract
// names it.
type Kind string

const (
	KindAnthropic   Kind = "anthropic"
	KindOpenAI      Kind = "openai"
	KindAzureOpenAI Kind = "azure_openai"
	KindBedrock     Kind = "bedrock"
	KindOllama      Kind = "ollama"
	KindCompatible  Kind = "openai_compatible"
)

// Kinds are every kind, in the order the settings page offers them.
var Kinds = []Kind{KindAnthropic, KindOpenAI, KindAzureOpenAI, KindBedrock, KindOllama, KindCompatible}

// ParseKind is the kind named s.
func ParseKind(s string) (Kind, error) {
	for _, k := range Kinds {
		if string(k) == s {
			return k, nil
		}
	}
	return "", fmt.Errorf("%q is not a model provider kind", s)
}

// KeyRule is whether a kind's calls carry an API key.
type KeyRule int

const (
	// KeyRequired is a cloud provider: every call carries the key.
	KeyRequired KeyRule = iota
	// KeyOptional is an OpenAI-compatible server, which may or may not want one.
	KeyOptional
	// KeyRefused is Ollama's native API, which takes none.
	KeyRefused
)

// Key is the kind's key rule.
func (k Kind) Key() KeyRule {
	switch k {
	case KindOllama:
		return KeyRefused
	case KindCompatible:
		return KeyOptional
	default:
		return KeyRequired
	}
}

// Free reports whether the kind may price its models as free: models that
// run on the owner's own hardware.
func (k Kind) Free() bool { return k == KindOllama || k == KindCompatible }

// Provider is where a workspace's model calls go: one struct per kind.
type Provider interface {
	Kind() Kind
	provider()
}

// Anthropic is Claude at api.anthropic.com, under the owner's key.
type Anthropic struct{}

// OpenAI is api.openai.com, under the owner's key.
type OpenAI struct{}

// AzureOpenAI is an Azure OpenAI resource. Endpoint is
// https://<resource>.openai.azure.com/openai/v1 (normalized), and each
// model's name is a deployment's.
type AzureOpenAI struct{ Endpoint string }

// Bedrock is Amazon Bedrock in Region, under a Bedrock API key.
type Bedrock struct {
	Region string
	FIPS   bool
}

// Ollama is an Ollama server's native API at BaseURL, which takes no key.
// ContextTokens is the context window it is asked to load models with.
type Ollama struct {
	BaseURL       string
	ContextTokens uint32
}

// CompatAPI is the API an OpenAI-compatible server speaks.
type CompatAPI string

const (
	CompatChatCompletions CompatAPI = "chat_completions"
	CompatResponses       CompatAPI = "responses"
)

// Compatible is an OpenAI-compatible server at BaseURL (including its /v1),
// sent at most MaxCallsInFlight calls at once.
type Compatible struct {
	BaseURL          string
	API              CompatAPI
	MaxCallsInFlight uint16
}

func (Anthropic) Kind() Kind   { return KindAnthropic }
func (OpenAI) Kind() Kind      { return KindOpenAI }
func (AzureOpenAI) Kind() Kind { return KindAzureOpenAI }
func (Bedrock) Kind() Kind     { return KindBedrock }
func (Ollama) Kind() Kind      { return KindOllama }
func (Compatible) Kind() Kind  { return KindCompatible }

func (Anthropic) provider()   {}
func (OpenAI) provider()      {}
func (AzureOpenAI) provider() {}
func (Bedrock) provider()     {}
func (Ollama) provider()      {}
func (Compatible) provider()  {}

// Role is a task Foundry runs a model for.
type Role string

const (
	// RoleDraft drafts ontologies: the agent's tool-calling turns.
	RoleDraft Role = "draft"
	// RoleAsk answers questions over a load.
	RoleAsk Role = "ask"
	// RoleExtract reads documents into facts.
	RoleExtract Role = "extract"
	// RoleVerify checks the facts read.
	RoleVerify Role = "verify"
)

// Roles is a model per task.
type Roles struct {
	Draft   Model `json:"draft"`
	Ask     Model `json:"ask"`
	Extract Model `json:"extract"`
	Verify  Model `json:"verify"`
}

// Each is every role's model, in role order.
func (r Roles) Each() []RoleModel {
	return []RoleModel{{RoleDraft, r.Draft}, {RoleAsk, r.Ask}, {RoleExtract, r.Extract}, {RoleVerify, r.Verify}}
}

// RoleModel is one role and its model.
type RoleModel struct {
	Role  Role
	Model Model
}

// Model is a model and how its calls are priced.
type Model struct {
	Name  string `json:"name"`
	Price Price  `json:"price"`
}

// Price is how a model's calls are priced: one of PriceTable, PriceTableAs,
// PriceRates and PriceFree.
type Price interface{ price() }

// PriceTable is the catalog's row for the model.
type PriceTable struct{}

// PriceTableAs is the catalog's row for another model of the same kind: an
// Azure deployment's underlying model, or a Bedrock inference profile's.
type PriceTableAs struct{ Model string }

// PriceRates are the owner's own rates, in nano-USD per token.
type PriceRates struct {
	Input     uint64
	Output    uint64
	CacheRead *uint64
}

// PriceFree counts no cost: a model on the owner's own hardware.
type PriceFree struct{}

func (PriceTable) price()   {}
func (PriceTableAs) price() {}
func (PriceRates) price()   {}
func (PriceFree) price()    {}

// Settings are a saved provider: where calls go and a model per task.
type Settings struct {
	Provider Provider
	Models   Roles
}

// settingsVersion is the settings JSON's version, its "v".
const settingsVersion = 1

// MarshalJSON writes {"v":1,"provider":{"kind":...},"models":{...}}.
func (s Settings) MarshalJSON() ([]byte, error) {
	if s.Provider == nil {
		return nil, errors.New("ai: settings have no provider")
	}
	provider, err := marshalProvider(s.Provider)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		V        int             `json:"v"`
		Provider json.RawMessage `json:"provider"`
		Models   Roles           `json:"models"`
	}{settingsVersion, provider, s.Models})
}

// DecodeSettings reads settings JSON strictly: an unknown field, kind or
// version is an error, as is a missing model.
func DecodeSettings(raw []byte) (Settings, error) {
	var out struct {
		V        int             `json:"v"`
		Provider json.RawMessage `json:"provider"`
		Models   *struct {
			Draft, Ask, Extract, Verify *Model
		} `json:"models"`
	}
	if err := strict(raw, &out); err != nil {
		return Settings{}, fmt.Errorf("ai settings: %w", err)
	}
	if out.V != settingsVersion {
		return Settings{}, fmt.Errorf("ai settings: version %d, want %d", out.V, settingsVersion)
	}
	provider, err := decodeProvider(out.Provider)
	if err != nil {
		return Settings{}, err
	}
	m := out.Models
	if m == nil || m.Draft == nil || m.Ask == nil || m.Extract == nil || m.Verify == nil {
		return Settings{}, errors.New("ai settings: models must name draft, ask, extract and verify")
	}
	return Settings{Provider: provider, Models: Roles{Draft: *m.Draft, Ask: *m.Ask, Extract: *m.Extract, Verify: *m.Verify}}, nil
}

func marshalProvider(p Provider) (json.RawMessage, error) {
	var fields any
	switch p := p.(type) {
	case Anthropic, OpenAI:
		fields = struct {
			Kind Kind `json:"kind"`
		}{p.Kind()}
	case AzureOpenAI:
		fields = struct {
			Kind     Kind   `json:"kind"`
			Endpoint string `json:"endpoint"`
		}{p.Kind(), p.Endpoint}
	case Bedrock:
		fields = struct {
			Kind   Kind   `json:"kind"`
			Region string `json:"region"`
			FIPS   bool   `json:"fips"`
		}{p.Kind(), p.Region, p.FIPS}
	case Ollama:
		fields = struct {
			Kind          Kind   `json:"kind"`
			BaseURL       string `json:"base_url"`
			ContextTokens uint32 `json:"context_tokens"`
		}{p.Kind(), p.BaseURL, p.ContextTokens}
	case Compatible:
		fields = struct {
			Kind             Kind      `json:"kind"`
			BaseURL          string    `json:"base_url"`
			API              CompatAPI `json:"api"`
			MaxCallsInFlight uint16    `json:"max_calls_in_flight"`
		}{p.Kind(), p.BaseURL, p.API, p.MaxCallsInFlight}
	default:
		return nil, fmt.Errorf("ai: unknown provider %T", p)
	}
	return json.Marshal(fields)
}

func decodeProvider(raw json.RawMessage) (Provider, error) {
	var head struct {
		Kind Kind `json:"kind"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("ai settings provider: %w", err)
	}
	// Each kind's fields, decoded strictly: the kind's own and no other.
	var p Provider
	var err error
	switch head.Kind {
	case KindAnthropic, KindOpenAI:
		err = strict(raw, &struct {
			Kind Kind `json:"kind"`
		}{})
		p = map[Kind]Provider{KindAnthropic: Anthropic{}, KindOpenAI: OpenAI{}}[head.Kind]
	case KindAzureOpenAI:
		var f struct {
			Kind     Kind   `json:"kind"`
			Endpoint string `json:"endpoint"`
		}
		err, p = strict(raw, &f), AzureOpenAI{Endpoint: f.Endpoint}
	case KindBedrock:
		var f struct {
			Kind   Kind   `json:"kind"`
			Region string `json:"region"`
			FIPS   bool   `json:"fips"`
		}
		err, p = strict(raw, &f), Bedrock{Region: f.Region, FIPS: f.FIPS}
	case KindOllama:
		var f struct {
			Kind          Kind   `json:"kind"`
			BaseURL       string `json:"base_url"`
			ContextTokens uint32 `json:"context_tokens"`
		}
		err, p = strict(raw, &f), Ollama{BaseURL: f.BaseURL, ContextTokens: f.ContextTokens}
	case KindCompatible:
		var f struct {
			Kind             Kind      `json:"kind"`
			BaseURL          string    `json:"base_url"`
			API              CompatAPI `json:"api"`
			MaxCallsInFlight uint16    `json:"max_calls_in_flight"`
		}
		err, p = strict(raw, &f), Compatible{BaseURL: f.BaseURL, API: f.API, MaxCallsInFlight: f.MaxCallsInFlight}
	default:
		return nil, fmt.Errorf("ai settings provider: unknown kind %q", head.Kind)
	}
	if err != nil {
		return nil, fmt.Errorf("ai settings provider: %w", err)
	}
	return p, nil
}

// priceJSON is a price as stored: "table", "free", {"table_as":"..."} or
// {"rates":{...}}, as the agent contract writes one.
type priceJSON struct {
	TableAs *string    `json:"table_as,omitempty"`
	Rates   *ratesJSON `json:"rates,omitempty"`
}

type ratesJSON struct {
	Input     uint64  `json:"input"`
	Output    uint64  `json:"output"`
	CacheRead *uint64 `json:"cache_read,omitempty"`
}

// MarshalJSON writes the model with its price.
func (m Model) MarshalJSON() ([]byte, error) {
	var price any
	switch p := m.Price.(type) {
	case PriceTable:
		price = "table"
	case PriceFree:
		price = "free"
	case PriceTableAs:
		price = priceJSON{TableAs: &p.Model}
	case PriceRates:
		price = priceJSON{Rates: &ratesJSON{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead}}
	default:
		return nil, fmt.Errorf("ai: model %q has no price", m.Name)
	}
	return json.Marshal(struct {
		Name  string `json:"name"`
		Price any    `json:"price"`
	}{m.Name, price})
}

// UnmarshalJSON reads a model and its price strictly.
func (m *Model) UnmarshalJSON(raw []byte) error {
	var f struct {
		Name  string          `json:"name"`
		Price json.RawMessage `json:"price"`
	}
	if err := strict(raw, &f); err != nil {
		return err
	}
	var word string
	if json.Unmarshal(f.Price, &word) == nil {
		switch word {
		case "table":
			*m = Model{Name: f.Name, Price: PriceTable{}}
			return nil
		case "free":
			*m = Model{Name: f.Name, Price: PriceFree{}}
			return nil
		}
		return fmt.Errorf("model %q: unknown price %q", f.Name, word)
	}
	var p priceJSON
	if err := strict(f.Price, &p); err != nil {
		return fmt.Errorf("model %q price: %w", f.Name, err)
	}
	switch {
	case p.TableAs != nil && p.Rates == nil:
		*m = Model{Name: f.Name, Price: PriceTableAs{Model: *p.TableAs}}
	case p.Rates != nil && p.TableAs == nil:
		*m = Model{Name: f.Name, Price: PriceRates{Input: p.Rates.Input, Output: p.Rates.Output, CacheRead: p.Rates.CacheRead}}
	default:
		return fmt.Errorf("model %q: a price is one of table, table_as, rates and free", f.Name)
	}
	return nil
}

// strict decodes raw into v, refusing unknown fields and trailing data.
func strict(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}
