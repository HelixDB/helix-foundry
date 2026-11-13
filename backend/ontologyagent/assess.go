package ontologyagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
)

// assess.go is the agent's Assess (contract 4.15.0): how much what an agent
// read of an ontology made its answer to one request, a workflow run's
// (schema/assess_request.json, schema/assess_result.json).

// MaxAssessCalls is the most calls an assessment's request carries: as many
// as a run records.
const MaxAssessCalls = 1000

// AssessModels is the model an assessment runs on: the workspace's question
// model.
type AssessModels struct {
	Endpoint Endpoint  `json:"endpoint"`
	Assess   ModelSpec `json:"assess"`
}

// AssessRequest is one assessment's request.
type AssessRequest struct {
	// RequestID is the run assessed, the same on every attempt.
	RequestID    string       `json:"request_id"`
	OntologyTOML string       `json:"ontology_toml"`
	Input        string       `json:"input"`
	Output       string       `json:"output"`
	Calls        []AssessCall `json:"calls"`
	Models       AssessModels `json:"models"`
}

// AssessCall is one call the run recorded, as the contract's tagged Call:
// exactly one of its kinds, by Call ("read", "question", "send", "upload").
type AssessCall struct {
	Call string `json:"call"`
	Seq  int32  `json:"seq"`
	// A read's reads, and its result when the run kept it.
	Reads  json.RawMessage `json:"reads,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	// A question, how it ended and its answer when it has one.
	Question string          `json:"question,omitempty"`
	Status   string          `json:"status,omitempty"`
	Answer   json.RawMessage `json:"answer,omitempty"`
	// A send's API source, by name, and its pieces' ids.
	Source string   `json:"source,omitempty"`
	Pieces []string `json:"pieces,omitempty"`
	// An upload's document, by name.
	Document string `json:"document,omitempty"`
}

// MarshalJSON writes the fields its kind has, and none other, as the
// contract's Call takes them.
func (c AssessCall) MarshalJSON() ([]byte, error) {
	out := map[string]any{"call": c.Call, "seq": c.Seq}
	switch c.Call {
	case "read":
		out["reads"] = c.Reads
		if len(c.Result) > 0 {
			out["result"] = c.Result
		}
	case "question":
		out["question"], out["status"] = c.Question, c.Status
		if len(c.Answer) > 0 {
			out["answer"] = c.Answer
		}
	case "send":
		out["source"], out["pieces"] = c.Source, append([]string{}, c.Pieces...)
	case "upload":
		out["document"] = c.Document
	default:
		return nil, fmt.Errorf("unknown call kind %q", c.Call)
	}
	return json.Marshal(out)
}

// Verdicts are the contract's verdicts.
var Verdicts = []string{"essential", "helpful", "marginal", "unused", "misleading"}

// AssessResult is how an assessment ended: Assessment (the engine's, as it
// wrote it) with its Verdict and the calls whose results it LeftOut, else
// Failure.
type AssessResult struct {
	Verdict    string
	Assessment json.RawMessage
	LeftOut    []int32
	Failure    ModelFailure
	Usage      Usage
}

// DecodeAssessResult reads an assessment's result, refusing one that is
// neither an assessment with a verdict the contract names nor a failure.
func DecodeAssessResult(data []byte) (AssessResult, error) {
	var raw struct {
		Outcome struct {
			Assessed *struct {
				Assessment json.RawMessage `json:"assessment"`
				LeftOut    []int32         `json:"left_out"`
			} `json:"assessed"`
			Failed *struct {
				Failure ModelFailure `json:"failure"`
			} `json:"failed"`
		} `json:"outcome"`
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return AssessResult{}, fmt.Errorf("read assess result: %w", err)
	}
	o := raw.Outcome
	switch {
	case raw.Usage == nil:
		return AssessResult{}, errors.New("assess result has no usage")
	case o.Failed != nil && o.Assessed == nil && o.Failed.Failure != "":
		return AssessResult{Failure: o.Failed.Failure, Usage: *raw.Usage}, nil
	case o.Assessed == nil || o.Failed != nil:
		return AssessResult{}, errors.New("assess result is neither an assessment nor a failure")
	}
	var verdict struct {
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(o.Assessed.Assessment, &verdict); err != nil || !slices.Contains(Verdicts, verdict.Verdict) {
		return AssessResult{}, fmt.Errorf("assessment has no verdict the contract names: %q", verdict.Verdict)
	}
	return AssessResult{Verdict: verdict.Verdict, Assessment: o.Assessed.Assessment, LeftOut: o.Assessed.LeftOut, Usage: *raw.Usage}, nil
}

// Assess assesses a workflow run (an AssessRequest), key the endpoint's:
// one unary call, which returns the AssessResult or the agent's gRPC status.
// A model's failure is a result, not an error.
func (c *Client) Assess(ctx context.Context, request []byte, key ModelKey) (json.RawMessage, error) {
	resp, err := c.agent.Assess(withKey(ctx, key), &agentv1.AssessRequest{RequestJson: string(request)})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(resp.GetResultJson()), nil
}
