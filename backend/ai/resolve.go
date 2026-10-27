package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// What resolving a workspace's provider can fail with. ResolveFailure words
// each for the user.
var (
	// ErrNotConfigured is a workspace on the default of a local install that
	// has none: model work waits for an owner to choose a provider.
	ErrNotConfigured = errors.New("ai: no model provider is set up")
	// ErrNoKeyring is a process without the keyring that opens workspace
	// keys: a deployment fault, which a retry after the fix gets past.
	ErrNoKeyring = errors.New("ai: no keyring to open the workspace's key")
	// ErrKeyUnreadable is a stored key that doesn't open: the keyring lost
	// its key, or the row was tampered with. An owner must enter it again.
	ErrKeyUnreadable = errors.New("ai: the workspace's stored key can't be opened")
	// ErrSettingsUnreadable is a stored config this backend can't read.
	ErrSettingsUnreadable = errors.New("ai: the workspace's saved settings can't be read")
	// ErrNotServed is a provider kind the ontology agent doesn't call (its
	// GetInfo's providers): a kind offered before the image that calls it
	// rolled out, a deployment fault.
	ErrNotServed = errors.New("ai: the ontology agent doesn't call this provider")
)

// Source is whose provider a call runs on.
type Source int

const (
	// SourceDefault is the deployment's: Helix's Claude when hosted.
	SourceDefault Source = iota
	// SourceWorkspace is the provider the workspace's owner saved.
	SourceWorkspace
)

// Resolved is the provider, models and key a workspace's model calls use.
type Resolved struct {
	Source   Source
	Settings Settings
	Key      ontologyagent.ModelKey
	// ConfigID is the workspace's config; "" for the default.
	ConfigID string
	// Report is what the config's probe found; empty for the default.
	Report ProbeReport
	// CallTimeout bounds each JSON or text call's attempt (foundry.ai.call_timeout).
	CallTimeout time.Duration
}

// Label names the provider for the user, as failure copy does: "Claude"
// for Anthropic, Helix's or the owner's.
func (r Resolved) Label() string { return Label(r.Settings.Provider) }

// Label names p for the user: an OpenAI-compatible server by its host.
func Label(p Provider) string {
	switch p := p.(type) {
	case Anthropic:
		return "Claude"
	case OpenAI:
		return "OpenAI"
	case AzureOpenAI:
		return "Azure OpenAI"
	case Bedrock:
		return "Amazon Bedrock"
	case Ollama:
		return "Ollama"
	case Compatible:
		if u, err := url.Parse(p.BaseURL); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
		return "the custom endpoint"
	default:
		return "the model provider"
	}
}

// ProbeReport is what a passing probe found, stored as probe_report: per
// check, never a model's text.
type ProbeReport struct {
	Checks []ProbeCheck  `json:"checks"`
	Ollama *OllamaReport `json:"ollama,omitempty"`
}

// ProbeCheck is one role's check.
type ProbeCheck struct {
	Role   string `json:"role"`
	Passed bool   `json:"passed"`
	// Reasoning is whether the model took the wire's reasoning knob; a
	// model that refused it passed with it off.
	Reasoning bool    `json:"reasoning"`
	Seconds   float64 `json:"seconds"`
}

// OllamaReport is the Ollama model's digest and context length at its probe.
type OllamaReport struct {
	Digest        string `json:"digest"`
	ContextLength uint32 `json:"context_length"`
}

// Reasoning reports whether role's model is sent its reasoning knob: on,
// unless its passing check turned it off.
func (p ProbeReport) Reasoning(role Role) bool {
	for _, c := range p.Checks {
		if c.Role == string(role) && c.Passed {
			return c.Reasoning
		}
	}
	return true
}

// ActiveConfigs reads a workspace's setting.
type ActiveConfigs interface {
	GetActiveFoundryAIConfig(ctx context.Context, workspaceID string) (*model.FoundryAIConfig, error)
}

// Resolver answers which provider, models and key a workspace's model calls
// use. Every attempt resolves again: one indexed read, and for a workspace's
// own provider, hosted, its plan (cached, Plans) and one AES-GCM open. The
// keyring and the plans matter only for those: Helix's Claude resolves
// without either.
type Resolver struct {
	DB         ActiveConfigs
	Keyring    *secretbox.Keyring
	Deployment *Deployment
	// Plans says which hosted workspaces may run on their own provider; nil
	// lets none. A local install sells no plan and needs none.
	Plans *Plans
}

// Resolve is ws's provider: its owner's active config, else the default.
// Hosted, the owner's config runs only while the workspace is on a plan that
// allows it (ErrProRequired), as saving it did.
func (r *Resolver) Resolve(ctx context.Context, workspaceID string) (Resolved, error) {
	c, err := r.DB.GetActiveFoundryAIConfig(ctx, workspaceID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if r.Deployment.Default == nil {
			return Resolved{}, ErrNotConfigured
		}
		return Resolved{
			Source: SourceDefault, Settings: r.Deployment.Default.Settings, Key: r.Deployment.Default.Key, CallTimeout: r.Deployment.CallTimeout,
		}, nil
	case err != nil:
		return Resolved{}, fmt.Errorf("read the workspace's AI setting: %w", err)
	}
	if !r.Deployment.Local {
		switch sold, err := r.Plans.Sold(ctx, workspaceID); {
		case err != nil:
			return Resolved{}, fmt.Errorf("read the workspace's Foundry plan: %w", err)
		case !sold:
			return Resolved{}, ErrProRequired
		}
	}
	settings, err := DecodeSettings(c.Settings)
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: %v", ErrSettingsUnreadable, err)
	}
	out := Resolved{Source: SourceWorkspace, Settings: settings, ConfigID: c.ID, CallTimeout: r.Deployment.CallTimeout}
	if len(c.ProbeReport) > 0 {
		if err := json.Unmarshal(c.ProbeReport, &out.Report); err != nil {
			return Resolved{}, fmt.Errorf("%w: probe report: %v", ErrSettingsUnreadable, err)
		}
	}
	if settings.Provider.Kind().Key() == KeyRefused {
		return out, nil
	}
	switch {
	case c.Secret == nil && settings.Provider.Kind().Key() == KeyOptional:
		return out, nil
	case c.Secret == nil:
		return Resolved{}, ErrKeyUnreadable
	case r.Keyring == nil:
		return Resolved{}, ErrNoKeyring
	}
	if out.Key, err = OpenKey(r.Keyring, c); err != nil {
		return Resolved{}, ErrKeyUnreadable
	}
	return out, nil
}

// ResolveServed is Resolve for a call to an agent that calls providers
// (GetInfo's): a provider of another kind is ErrNotServed.
func (r *Resolver) ResolveServed(ctx context.Context, workspaceID string, providers []string) (Resolved, error) {
	resolved, err := r.Resolve(ctx, workspaceID)
	if err != nil {
		return Resolved{}, err
	}
	if kind := resolved.Settings.Provider.Kind(); !slices.Contains(providers, string(kind)) {
		return Resolved{}, fmt.Errorf("%w: %s", ErrNotServed, kind)
	}
	return resolved, nil
}

// Usable refuses ws's model work before any is queued, as Resolve would
// refuse it when it runs: ErrNotConfigured on a local install without a
// default, ErrProRequired for a hosted workspace's own provider once it is off
// a plan that allows one. It opens no key.
//
//	err := r.Usable(ctx, "ws_1") // nil on Helix's Claude
func (r *Resolver) Usable(ctx context.Context, workspaceID string) error {
	_, err := r.DB.GetActiveFoundryAIConfig(ctx, workspaceID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if r.Deployment.Default == nil {
			return ErrNotConfigured
		}
		return nil
	case err != nil:
		return fmt.Errorf("read the workspace's AI setting: %w", err)
	case r.Deployment.Local:
		return nil
	}
	switch sold, err := r.Plans.Sold(ctx, workspaceID); {
	case err != nil:
		return fmt.Errorf("read the workspace's Foundry plan: %w", err)
	case !sold:
		return ErrProRequired
	}
	return nil
}
