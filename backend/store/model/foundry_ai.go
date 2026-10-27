package model

import (
	"encoding/json"
	"time"
)

// FoundryAIProbeState is where a workspace's AI config is in its test
// (foundry_ai_configs.probe_state). Only a config that passed was ever the
// setting; queued and running are pending.
type FoundryAIProbeState string

const (
	FoundryAIProbeQueued  FoundryAIProbeState = "queued"
	FoundryAIProbeRunning FoundryAIProbeState = "running"
	FoundryAIProbePassed  FoundryAIProbeState = "passed"
	FoundryAIProbeFailed  FoundryAIProbeState = "failed"
	// FoundryAIProbeSuperseded is a config a newer one, or a newer setting,
	// replaced before its probe could activate it.
	FoundryAIProbeSuperseded FoundryAIProbeState = "superseded"
)

// Pending reports a config still waiting for or in its probe.
func (s FoundryAIProbeState) Pending() bool {
	return s == FoundryAIProbeQueued || s == FoundryAIProbeRunning
}

// FoundryAISecret is an API key sealed with pkg/secretbox under KeyID; the
// two are stored and nulled together (foundry_ai_configs_secret_chk).
type FoundryAISecret struct {
	Ciphertext []byte
	KeyID      string
}

// FoundryAIConfig is one workspace model provider an owner saved: written
// once, then only its probe columns change. Settings is the ai package's
// JSON and never holds a secret.
type FoundryAIConfig struct {
	ID          string
	WorkspaceID string
	Kind        string
	Settings    json.RawMessage
	// Secret is nil for a kind that needs no key, and once the config is
	// neither the setting nor pending.
	Secret *FoundryAISecret
	// SecretHint shows which key it was, e.g. sk-ant…4f2a; it outlives Secret.
	SecretHint    *string
	ProbeState    FoundryAIProbeState
	ProbeAttempts int
	ProbeOwner    *string
	ProbeUntil    *time.Time
	// ProbeFailure and ProbeDetail say why a failed probe failed.
	ProbeFailure *string
	ProbeDetail  *string
	ProbeReport  json.RawMessage
	CreatedBy    string
	CreatedAt    time.Time
	ProbedAt     *time.Time
}

// FoundryAIView is what a workspace's AI settings show.
type FoundryAIView struct {
	// Active is the setting's config; nil is the deployment's default.
	Active *FoundryAIConfig
	// Pending is the config waiting for or in its probe.
	Pending *FoundryAIConfig
	// LastFailed is the workspace's newest config when its probe failed
	// after the setting last changed.
	LastFailed *FoundryAIConfig
}

// FoundryAIProbeFailure is why a probe failed: a ModelFailure of the agent
// contract, and the check's detail.
type FoundryAIProbeFailure struct {
	Failure string
	Detail  string
}

// FoundryAIProbeOutcome is how a probe ended: passed when Failure is nil.
// Report is stored either way.
type FoundryAIProbeOutcome struct {
	Failure *FoundryAIProbeFailure
	Report  json.RawMessage
}
