package ontologyagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// probe.go is the Title and Probe RPCs' contract (schema/title_request.json,
// title_result.json, probe_request.json, probe_result.json): a generation's
// title from its goal, and a test of whether an endpoint's models can do
// what each role needs before a workspace's settings are used.

// TitleRequest is one title's request.
type TitleRequest struct {
	// RequestID is what is titled, the same on every attempt.
	RequestID string      `json:"request_id"`
	Goal      string      `json:"goal"`
	Models    TitleModels `json:"models"`
}

// TitleResult is how a title call ended: Text when the model ended its
// turn with it, else Failure.
type TitleResult struct {
	Text    string
	Failure ModelFailure
	Usage   Usage
}

// DecodeTitleResult reads a title result, refusing one that is neither a
// text nor a failure the contract names.
func DecodeTitleResult(data []byte) (TitleResult, error) {
	var raw struct {
		Outcome struct {
			Text *struct {
				Text *string `json:"text"`
			} `json:"text"`
			Failed *struct {
				Failure ModelFailure `json:"failure"`
			} `json:"failed"`
		} `json:"outcome"`
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return TitleResult{}, fmt.Errorf("read title result: %w", err)
	}
	o := raw.Outcome
	switch {
	case raw.Usage == nil:
		return TitleResult{}, errors.New("title result has no usage")
	case o.Text != nil && o.Failed == nil && o.Text.Text != nil:
		return TitleResult{Text: *o.Text.Text, Usage: *raw.Usage}, nil
	case o.Failed != nil && o.Text == nil && o.Failed.Failure != "":
		return TitleResult{Failure: o.Failed.Failure, Usage: *raw.Usage}, nil
	}
	return TitleResult{}, errors.New("title result is neither a text nor a failure")
}

// ProbeRole is a role a probe checks a model for.
type ProbeRole string

const (
	ProbeDraft   ProbeRole = "draft"
	ProbeAsk     ProbeRole = "ask"
	ProbeExtract ProbeRole = "extract"
	ProbeVerify  ProbeRole = "verify"
	ProbeTitle   ProbeRole = "title"
)

// ProbeRoles are every role, in the order a probe checks them.
var ProbeRoles = []ProbeRole{ProbeDraft, ProbeAsk, ProbeExtract, ProbeVerify, ProbeTitle}

// ProbeCheck is a role's model, to check.
type ProbeCheck struct {
	Role  ProbeRole `json:"role"`
	Model ModelSpec `json:"model"`
}

// ProbeLimits are hard caps on a whole probe.
type ProbeLimits struct {
	MaxUSD      float64 `json:"max_usd"`
	MaxWallSecs uint32  `json:"max_wall_secs"`
}

// ProbeRequest checks an endpoint's models, one check per role.
type ProbeRequest struct {
	// RequestID is the settings tested, the same on every attempt.
	RequestID string       `json:"request_id"`
	Endpoint  Endpoint     `json:"endpoint"`
	Checks    []ProbeCheck `json:"checks"`
	Limits    ProbeLimits  `json:"limits"`
}

// ProbeDetail is what a failed check saw.
type ProbeDetail string

const (
	ProbeToolsRejected   ProbeDetail = "tools_rejected"
	ProbeToolCallMissing ProbeDetail = "tool_call_missing"
	ProbeSchemaRejected  ProbeDetail = "schema_rejected"
	ProbeJSONInvalid     ProbeDetail = "json_invalid"
	ProbeTextMissing     ProbeDetail = "text_missing"
	ProbeContextTooSmall ProbeDetail = "context_too_small"
	ProbeUnpriced        ProbeDetail = "unpriced"
	ProbeNoDetail        ProbeDetail = "none"
)

var probeDetails = []ProbeDetail{
	ProbeToolsRejected, ProbeToolCallMissing, ProbeSchemaRejected, ProbeJSONInvalid, ProbeTextMissing, ProbeContextTooSmall,
	ProbeUnpriced, ProbeNoDetail,
}

// ProbeCheckResult is one check's outcome: Passed, or Failure and Detail.
type ProbeCheckResult struct {
	Role   ProbeRole
	Passed bool
	// Seconds is how long a passed check took; Reasoning whether it passed
	// with the reasoning knob sent, which the role's model then runs with.
	Seconds   float64
	Reasoning bool
	Failure   ModelFailure
	Detail    ProbeDetail
}

// OllamaInfo is what an Ollama server said of its model.
type OllamaInfo struct {
	Digest        string   `json:"digest"`
	ContextLength uint64   `json:"context_length"`
	Capabilities  []string `json:"capabilities"`
}

// ProbeResult is what each check found, and what the probe spent.
type ProbeResult struct {
	Checks []ProbeCheckResult
	// Ollama is what an Ollama server said; nil for any other provider.
	Ollama *OllamaInfo
	Usage  Usage
}

// Failed is the first failed check, or nil when every check passed.
func (r ProbeResult) Failed() *ProbeCheckResult {
	for i := range r.Checks {
		if !r.Checks[i].Passed {
			return &r.Checks[i]
		}
	}
	return nil
}

// DecodeProbeResult reads a probe result, refusing one whose checks aren't
// exactly the roles asked for, in their order, each passed or failed as the
// contract names it.
func DecodeProbeResult(data []byte, asked []ProbeRole) (ProbeResult, error) {
	var raw struct {
		Checks []struct {
			Role    ProbeRole `json:"role"`
			Outcome struct {
				Passed *struct {
					Seconds   *float64 `json:"seconds"`
					Reasoning *bool    `json:"reasoning"`
				} `json:"passed"`
				Failed *struct {
					Failure ModelFailure `json:"failure"`
					Detail  ProbeDetail  `json:"detail"`
				} `json:"failed"`
			} `json:"outcome"`
		} `json:"checks"`
		Ollama *OllamaInfo `json:"ollama"`
		Usage  *Usage      `json:"usage"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ProbeResult{}, fmt.Errorf("read probe result: %w", err)
	}
	if raw.Usage == nil || len(raw.Checks) != len(asked) {
		return ProbeResult{}, fmt.Errorf("probe result has %d checks of %d, or no usage", len(raw.Checks), len(asked))
	}
	out := ProbeResult{Ollama: raw.Ollama, Usage: *raw.Usage}
	for i, c := range raw.Checks {
		passed, failed := c.Outcome.Passed, c.Outcome.Failed
		switch {
		case c.Role != asked[i]:
			return ProbeResult{}, fmt.Errorf("probe check %d is %q, not %q", i, c.Role, asked[i])
		case passed != nil && failed == nil && passed.Seconds != nil && passed.Reasoning != nil:
			out.Checks = append(out.Checks, ProbeCheckResult{Role: c.Role, Passed: true, Seconds: *passed.Seconds, Reasoning: *passed.Reasoning})
		case failed != nil && passed == nil && failed.Failure != "" && slices.Contains(probeDetails, failed.Detail):
			out.Checks = append(out.Checks, ProbeCheckResult{Role: c.Role, Failure: failed.Failure, Detail: failed.Detail})
		default:
			return ProbeResult{}, fmt.Errorf("probe check %s is neither passed nor failed", c.Role)
		}
	}
	return out, nil
}
