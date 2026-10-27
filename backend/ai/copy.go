package ai

import (
	"context"
	"errors"
	"slices"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
)

// Copy is what the user is told of a failure, and whether a retry could
// help.
type Copy struct {
	Message   string
	Permanent bool
	// Ops is a failure of Helix's own account, which no owner can fix: it is
	// retried and logged as an error for on-call.
	Ops bool
}

// Logged is c, after logging err as an error when c is Helix's to fix
// (Ops), for on-call.
func (c Copy) Logged(ctx context.Context, err error) Copy {
	if c.Ops {
		log.From(ctx).Error("foundry ai: a model call failed in a way only Helix can fix", "err", err)
	}
	return c
}

// settingsLink is the phrase the site links to the AI settings, wherever it
// appears in failure copy.
const settingsLink = "Settings › AI models"

// DraftFailure words a draft whose model calls failed with f, on r. Helix's
// Claude says what generations always said: a refusal is final, and every
// other failure is retried as unavailable, its key's, account's and refused
// requests raised with on-call too, since no owner can fix them.
func DraftFailure(r Resolved, f ontologyagent.ModelFailure) Copy {
	p := r.Label()
	switch {
	case f == ontologyagent.ModelRefused:
		return Copy{Message: p + " declined to draft this ontology", Permanent: true}
	case r.houseCopy():
		return house(p+" is unavailable; Helix will retry", f)
	}
	if c, ok := own(r, r.Settings.Models.Draft.Name, f); ok {
		return c
	}
	return Copy{Message: p + " is unavailable; Helix will retry"}
}

// AskFailure words a question whose model call failed with f, on r. Helix's
// Claude says what questions always said, as DraftFailure does for drafts. A
// failed question is never retried, so Permanent says only whose it is.
func AskFailure(r Resolved, f ontologyagent.ModelFailure) Copy {
	const unavailable = "the model could not be reached; ask again"
	switch {
	case f == ontologyagent.ModelRefused:
		return Copy{Message: "the model declined to answer; ask another way", Permanent: true}
	case r.houseCopy():
		return house(unavailable, f)
	}
	if c, ok := own(r, r.Settings.Models.Ask.Name, f); ok {
		return c
	}
	return Copy{Message: unavailable}
}

// DocumentFailure words a document whose extraction failed with f, on r.
// Stop says every other document of the load would fail the same way, so
// the load stops with Message: the workspace's own provider refused its key,
// its account, its models or its endpoint. Any other failure is the
// document's alone, which its recorded code says, and has no Message.
// Helix's Claude never stops a load; its key's and account's failures are
// raised with on-call (Ops).
func DocumentFailure(r Resolved, f ontologyagent.ModelFailure) (c Copy, stop bool) {
	if r.houseCopy() {
		return Copy{Ops: slices.Contains(accountFailures, f)}, false
	}
	switch f {
	case ontologyagent.ModelAuth, ontologyagent.ModelPermission, ontologyagent.ModelNotFound, ontologyagent.ModelQuota,
		ontologyagent.ModelChanged, ontologyagent.ModelPrivateHost:
	default:
		return Copy{}, false
	}
	m := r.Settings.Models
	model := m.Extract.Name
	if m.Verify.Name != model {
		model += " or " + m.Verify.Name
	}
	c, _ = own(r, model, f)
	return c, true
}

// TitleFailure says whose a title's failed model call is. A title falls
// back to its goal's first sentence, so it has no message; only Helix's
// Claude refusing its key, its account or its models is raised with
// on-call (Ops). A truncated, unsupported or too long answer is a model's
// ordinary trouble, on whoever's provider.
func TitleFailure(r Resolved, f ontologyagent.ModelFailure) Copy {
	return Copy{Ops: r.houseCopy() && slices.Contains(accountFailures, f)}
}

// houseCopy reports whether r's failures are worded as Helix's Claude's
// are: the deployment's default, but for an Ollama server, which only a
// local install's default can be (hosted never offers Ollama) and which
// its owner runs and fixes, so its failures are worded as a saved one's.
func (r Resolved) houseCopy() bool {
	_, ollama := r.Settings.Provider.(Ollama)
	return r.Source == SourceDefault && !ollama
}

// accountFailures are a provider refusing the key, the account or the
// models a call names: on Helix's Claude, on-call's to fix.
var accountFailures = []ontologyagent.ModelFailure{
	ontologyagent.ModelAuth, ontologyagent.ModelPermission, ontologyagent.ModelNotFound, ontologyagent.ModelQuota,
}

// house is Helix's Claude's copy, msg, for every failure but a refusal. One
// that isn't the provider's passing trouble (its key, account or a request
// it refused) is raised with on-call too.
func house(msg string, f ontologyagent.ModelFailure) Copy {
	switch f {
	case ontologyagent.ModelUnavailable, ontologyagent.ModelRateLimited, ontologyagent.ModelMalformed, ontologyagent.ModelTimeout:
		return Copy{Message: msg}
	}
	return Copy{Message: msg, Ops: true}
}

// own words the failures of a workspace's own provider that say more than
// that it is unavailable, for a call to model; false for the rest.
func own(r Resolved, model string, f ontologyagent.ModelFailure) (Copy, bool) {
	p := r.Label()
	msg := map[ontologyagent.ModelFailure]string{
		ontologyagent.ModelAuth:           p + " refused the API key. An owner can enter a new one in " + settingsLink + ".",
		ontologyagent.ModelPermission:     p + " refused access to " + model + ". An owner can choose another model in " + settingsLink + ".",
		ontologyagent.ModelNotFound:       p + " has no model " + model + ". An owner can choose another in " + settingsLink + ".",
		ontologyagent.ModelQuota:          "The " + p + " account is out of credit or quota.",
		ontologyagent.ModelContextTooLong: "This was too long for " + model + ".",
		ontologyagent.ModelUnsupported:    model + " can’t do what this task needs (tool calls or structured answers).",
		ontologyagent.ModelTruncated:      "The model ran out of room for its answer.",
		ontologyagent.ModelChanged: "The Ollama model changed since it was tested. An owner can test it again in " +
			settingsLink + ".",
		ontologyagent.ModelPrivateHost: "The endpoint resolves to a private address.",
	}[f]
	if _, ok := r.Settings.Provider.(Ollama); ok {
		// An Ollama server's models are the ones its owner pulled, loaded in
		// the context window the owner chose.
		switch f {
		case ontologyagent.ModelNotFound:
			msg = "Ollama has no model " + model + ". Pull it on the Ollama server with ollama pull, or an owner can choose another in " +
				settingsLink + "."
		case ontologyagent.ModelContextTooLong:
			msg = "This was too long for " + model + " in its context window. An owner can raise the window, or choose a model with a larger one, in " +
				settingsLink + "."
		}
	}
	if msg == "" {
		// unavailable, rate_limited, malformed and timeout pass with time.
		return Copy{}, false
	}
	return Copy{Message: msg, Permanent: true}, true
}

// ResolveFailure words an error of Resolver.Resolve for the user.
func ResolveFailure(err error) Copy {
	switch {
	case errors.Is(err, ErrNotConfigured):
		return Copy{Message: "No AI provider is set up. An owner can choose one in " + settingsLink + ".", Permanent: true}
	case errors.Is(err, ErrKeyUnreadable):
		return Copy{Message: "The stored API key can't be opened. An owner can enter it again in " + settingsLink + ".", Permanent: true}
	case errors.Is(err, ErrProRequired):
		return Copy{Message: "Bringing your own model key needs Foundry Pro. An owner can upgrade, or go back to Helix's Claude in " + settingsLink + ".",
			Permanent: true}
	case errors.Is(err, ErrSettingsUnreadable):
		return Copy{Message: "The saved AI settings can't be read. An owner can save them again in " + settingsLink + ".", Permanent: true}
	case errors.Is(err, ErrNoKeyring):
		return Copy{Message: "AI settings can't be read; Helix will retry", Ops: true}
	case errors.Is(err, ErrNotServed):
		return Copy{Message: "The ontology agent can't call this AI provider yet; Helix will retry", Ops: true}
	default:
		return Copy{Message: "AI settings can't be read; Helix will retry"}
	}
}
