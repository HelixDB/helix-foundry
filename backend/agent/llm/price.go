package llm

import (
	"fmt"

	"github.com/helixdb/helix-foundry/backend/ai"
)

// Pricer prices a model's calls, in nano-USD per token, as Settings › AI
// models priced the model: from Helix's catalog (its own row, or another's),
// at the owner's rates, or free.
type Pricer struct{ Input, Output, CacheRead, CacheWrite uint64 }

// NewPricer is m's price, of a provider of kind.
func NewPricer(catalog *ai.Catalog, kind ai.Kind, m ai.Model) (Pricer, error) {
	row := func(name string) (Pricer, error) {
		e, ok := catalog.Lookup(kind, name)
		if !ok {
			return Pricer{}, fmt.Errorf("llm: %s %s has no price in the catalog", kind, name)
		}
		return Pricer{Input: e.Input, Output: e.Output, CacheRead: e.CacheRead, CacheWrite: cacheWrite(e.Input)}, nil
	}
	switch p := m.Price.(type) {
	case ai.PriceTable:
		return row(m.Name)
	case ai.PriceTableAs:
		return row(p.Model)
	case ai.PriceRates:
		out := Pricer{Input: p.Input, Output: p.Output, CacheRead: p.Input, CacheWrite: cacheWrite(p.Input)}
		if p.CacheRead != nil {
			out.CacheRead = *p.CacheRead
		}
		return out, nil
	case ai.PriceFree:
		return Pricer{}, nil
	default:
		return Pricer{}, fmt.Errorf("llm: model %q has no price", m.Name)
	}
}

// cacheWrite is a five-minute prompt cache write's rate: a quarter above the
// input's, as Claude's are, rounded up. The catalog's rows of it agree; the
// Go side reads only the rates the agent's meter charges.
func cacheWrite(input uint64) uint64 { return (input*5 + 3) / 4 }

// Micros is what u cost, in millionths of a US dollar, rounded up.
func (p Pricer) Micros(u Usage) int64 {
	nanos := uint64(max(u.In, 0))*p.Input + uint64(max(u.Out, 0))*p.Output +
		uint64(max(u.CacheRead, 0))*p.CacheRead + uint64(max(u.CacheWrite, 0))*p.CacheWrite
	return int64((nanos + 999) / 1000)
}
