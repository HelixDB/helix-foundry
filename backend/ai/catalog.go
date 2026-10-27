package ai

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	protoschemas "github.com/helixdb/helix-foundry/backend/pb/proto"
)

// Entry is one model the agent prices by table: its rates in nano-USD per
// token, which the agent's meter charges and the settings page shows.
type Entry struct {
	Kind      Kind   `json:"kind"`
	Model     string `json:"model"`
	Input     uint64 `json:"input"`
	Output    uint64 `json:"output"`
	CacheRead uint64 `json:"cache_read"`
}

type catalogKey struct {
	kind  Kind
	model string
}

// Catalog is the agent's price table (catalog.json, vendored with its
// contract): a model priced "table" must be in it, or the agent refuses its
// calls as unpriced. Saving such a model is refused first.
type Catalog struct{ entries map[catalogKey]Entry }

// ParseCatalog reads catalog.json, {"models": [...]}: entries each of a
// known kind and a model with an output rate. Fields the Go side doesn't use
// (cache writes, limits, sources) are ignored.
func ParseCatalog(raw []byte) (*Catalog, error) {
	var doc struct {
		Models *[]Entry `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("ai catalog: %w", err)
	}
	if doc.Models == nil {
		return nil, fmt.Errorf("ai catalog: no models")
	}
	c := &Catalog{entries: make(map[catalogKey]Entry, len(*doc.Models))}
	for _, e := range *doc.Models {
		if _, err := ParseKind(string(e.Kind)); err != nil {
			return nil, fmt.Errorf("ai catalog: %w", err)
		}
		if rowsOf(e.Kind) != e.Kind {
			return nil, fmt.Errorf("ai catalog: %s is priced from %s's rows and has none of its own", e.Kind, rowsOf(e.Kind))
		}
		if ModelName(e.Model) != nil || e.Output == 0 {
			return nil, fmt.Errorf("ai catalog: a %s entry has no model name or output rate", e.Kind)
		}
		key := catalogKey{e.Kind, e.Model}
		if _, ok := c.entries[key]; ok {
			return nil, fmt.Errorf("ai catalog: %s %s is listed twice", e.Kind, e.Model)
		}
		c.entries[key] = e
	}
	return c, nil
}

var vendoredCatalog = sync.OnceValues(func() (*Catalog, error) { return ParseCatalog(protoschemas.OntologyAgentCatalog) })

// VendoredCatalog is the catalog of the agent contract this backend speaks.
func VendoredCatalog() (*Catalog, error) { return vendoredCatalog() }

// bedrockProfiles are the prefixes of Bedrock's global and geographic
// inference profiles, in the agent's order: a profile's id without a row of
// its own is priced as the model it runs, the id past its prefix. GovCloud's
// us-gov. costs more than any row, so it is none of them.
var bedrockProfiles = []string{"global.", "us.", "eu.", "apac.", "au.", "jp.", "ca."}

// rowsOf is the kind whose rows price kind's models, as the agent's catalog
// reads them: an Azure deployment runs an OpenAI model, at OpenAI's rates.
func rowsOf(kind Kind) Kind {
	if kind == KindAzureOpenAI {
		return KindOpenAI
	}
	return kind
}

// Lookup is the entry for kind's model: on Azure OpenAI, the OpenAI model's.
// On Bedrock, an inference profile's id with no row of its own is looked up
// once more without its prefix, as the agent's catalog::find does.
func (c *Catalog) Lookup(kind Kind, model string) (Entry, bool) {
	kind = rowsOf(kind)
	if e, ok := c.entries[catalogKey{kind, model}]; ok {
		return e, true
	}
	if kind == KindBedrock {
		for _, prefix := range bedrockProfiles {
			if rest, ok := strings.CutPrefix(model, prefix); ok {
				e, found := c.entries[catalogKey{kind, rest}]
				return e, found
			}
		}
	}
	return Entry{}, false
}

// Unpriced are the roles whose model is priced by a catalog row the catalog
// lacks: "table" for its own name, "table_as" for the one it names.
func (c *Catalog) Unpriced(s Settings) []Role {
	var out []Role
	for _, rm := range s.Models.Each() {
		name := rm.Model.Name
		switch p := rm.Model.Price.(type) {
		case PriceTableAs:
			name = p.Model
		case PriceTable:
		default:
			continue
		}
		if _, ok := c.Lookup(s.Provider.Kind(), name); !ok {
			out = append(out, rm.Role)
		}
	}
	return out
}

// Suggested are the models a kind's settings start from, each priced from
// the catalog; false for a kind whose owner names every model, as Azure's
// deployments are. Claude's are the ones Helix's Claude runs; OpenAI's run
// Sol where Claude runs Opus and Sonnet, and Luna where it runs Haiku.
// Bedrock's are the same Claude models through global inference profiles,
// which cost Anthropic's list price, under one region's or a geographic
// profile's.
func Suggested(kind Kind) (Roles, bool) {
	switch kind {
	case KindAnthropic:
		return Roles{
			Draft:   Model{Name: "claude-opus-5-5", Price: PriceTable{}},
			Ask:     Model{Name: "claude-sonnet-5-5", Price: PriceTable{}},
			Extract: Model{Name: "claude-haiku-5-5", Price: PriceTable{}},
			Verify:  Model{Name: "claude-sonnet-5-5", Price: PriceTable{}},
		}, true
	case KindOpenAI:
		return Roles{
			Draft:   Model{Name: "gpt-6.1-sol", Price: PriceTable{}},
			Ask:     Model{Name: "gpt-6.1-sol", Price: PriceTable{}},
			Extract: Model{Name: "gpt-6-luna", Price: PriceTable{}},
			Verify:  Model{Name: "gpt-6.1-sol", Price: PriceTable{}},
		}, true
	case KindBedrock:
		return Roles{
			Draft:   Model{Name: "global.anthropic.claude-opus-5-5", Price: PriceTable{}},
			Ask:     Model{Name: "global.anthropic.claude-sonnet-5-5", Price: PriceTable{}},
			Extract: Model{Name: "global.anthropic.claude-haiku-5-5", Price: PriceTable{}},
			Verify:  Model{Name: "global.anthropic.claude-sonnet-5-5", Price: PriceTable{}},
		}, true
	default:
		return Roles{}, false
	}
}
