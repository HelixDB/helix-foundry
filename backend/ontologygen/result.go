// Package ontologygen is what a Foundry ontology generation comes to (Result,
// or a Failure worded for the customer), and the one-line title a
// generation gets from its goal (Titler). The ontology agent drafts
// (ontologyagent.Generator); no model is called from here.
package ontologygen

import (
	"encoding/json"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Result is a generation's draft and what producing it cost. Tokens are set
// even when the attempt fails, because a failed answer is still billed.
type Result struct {
	// Format is the shape of Definition: an agent bundle.
	Format     model.OntologyFormat
	Definition json.RawMessage
	// Model drafted it, served by a provider of kind ModelProvider
	// (anthropic, ...).
	Model         string
	ModelProvider string
	InputTokens   int
	OutputTokens  int
}

// Failure is a failed attempt with a message for the customer. Permanent
// failures are not retried.
type Failure struct {
	Message   string
	Permanent bool
	Err       error
}

func (f *Failure) Error() string {
	if f.Err == nil {
		return f.Message
	}
	return f.Message + ": " + f.Err.Error()
}

func (f *Failure) Unwrap() error { return f.Err }
