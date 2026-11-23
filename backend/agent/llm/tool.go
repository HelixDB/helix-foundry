package llm

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/log"
)

// The failures CallTool reports, in words a row records.
const (
	// FailureTimeout: ctx ended before an answer read, or before a retry.
	FailureTimeout = "the model took too long"
	// FailureUnusable: the last answer called no tool, or parse refused it.
	FailureUnusable = "the model's answer wasn't usable"
	// FailureCall: the call failed without the provider saying why.
	FailureCall = "the model call failed"
)

// CallTool asks b's model to call tool, up to calls times within ctx, and
// returns the first input parse reads, what every call cost, and, when none
// read, why ("" when one did): FailureTimeout, FailureUnusable, FailureCall,
// or the provider's refusal ("the model provider refused: auth", or "the
// model provider couldn't answer: rate_limited" after the last retry).
//
// A call is made again after an answer parse refuses (or that calls no
// tool), and, after a wait that starts at wait and doubles, after a refusal
// a retry may pass (rate limited, unavailable); any other refusal, a failed
// call, or ctx's end stops at once. Only tool's first call in an answer is
// read. The cost is every call's, so the caller charges it whatever comes of
// the calls.
//
//	out, cost, failure := llm.CallTool(ctx, b, req, "suggest", 3, 2*time.Second, suggest.Parse)
func CallTool[T any](ctx context.Context, b Binding, req Request, tool string, calls int, wait time.Duration,
	parse func(json.RawMessage) (T, error)) (out T, cost int64, failure string) {
	pause, next := time.Duration(0), wait
	for range calls {
		if pause > 0 {
			timer := time.NewTimer(pause)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return out, cost, FailureTimeout
			}
		}
		var input json.RawMessage
		var failed error
		for chunk, err := range b.Model.Stream(ctx, req) {
			if err != nil {
				failed = err
				break
			}
			switch c := chunk.(type) {
			case ToolUse:
				if c.Name == tool && input == nil {
					input = c.Input
				}
			case Usage:
				cost += b.Pricer.Micros(c)
			}
		}
		var refused *Error
		pause = 0
		switch {
		case failed != nil && ctx.Err() != nil:
			// Out of time, waiting for the server or its answer: no retry
			// can pass.
			return out, cost, FailureTimeout
		case failed != nil && errors.As(failed, &refused) && refused.Retryable():
			failure = "the model provider couldn't answer: " + string(refused.Kind)
			pause, next = next, 2*next
			continue
		case failed != nil && errors.As(failed, &refused):
			return out, cost, "the model provider refused: " + string(refused.Kind)
		case failed != nil:
			return out, cost, FailureCall
		}
		read, err := parse(input)
		if err != nil {
			failure = FailureUnusable
			log.From(ctx).Info("model call: an answer that didn't read", "tool", tool, "err", err)
			continue
		}
		return read, cost, ""
	}
	return out, cost, failure
}
