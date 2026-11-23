package llm

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolAnswer is one call's stream: its chunks, then err when not nil.
type toolAnswer struct {
	chunks []Chunk
	err    error
}

// toolModel answers its calls in order, the last again once they run out,
// and records when each was made.
type toolModel struct {
	mu      sync.Mutex
	answers []toolAnswer
	at      []time.Time
}

func (m *toolModel) Stream(context.Context, Request) iter.Seq2[Chunk, error] {
	m.mu.Lock()
	a := m.answers[min(len(m.at), len(m.answers)-1)]
	m.at = append(m.at, time.Now())
	m.mu.Unlock()
	return func(yield func(Chunk, error) bool) {
		for _, c := range a.chunks {
			if !yield(c, nil) {
				return
			}
		}
		if a.err != nil {
			yield(nil, a.err)
		}
	}
}

// A call reads 1,000 tokens and writes 500: 2,000 micros at these prices.
var (
	toolUsed    = Usage{In: 1000, Out: 500}
	toolBinding = func(m Model) Binding { return Binding{Model: m, Pricer: Pricer{Input: 1000, Output: 2000}} }
)

func called(input string) toolAnswer {
	return toolAnswer{chunks: []Chunk{ToolUse{ID: "t1", Name: "pick", Input: json.RawMessage(input)}, toolUsed, Stop{Reason: StopToolUse}}}
}

// parseOK reads {"ok": true} as "read", anything else as unusable.
func parseOK(raw json.RawMessage) (string, error) {
	var in struct{ OK bool }
	if json.Unmarshal(raw, &in) != nil || !in.OK {
		return "", errors.New("not ok")
	}
	return "read", nil
}

func callTool(m Model, calls int, wait time.Duration) (string, int64, string) {
	return CallTool(context.Background(), toolBinding(m), Request{}, "pick", calls, wait, parseOK)
}

func TestCallToolRetriesAnAnswerThatDoesntRead(t *testing.T) {
	m := &toolModel{answers: []toolAnswer{called(`{}`), called(`{"ok": false}`), called(`{"ok": true}`)}}
	out, cost, failure := callTool(m, 3, time.Millisecond)
	assert.Equal(t, "read", out, "the third answer reads")
	assert.Empty(t, failure)
	assert.Len(t, m.at, 3)
	assert.EqualValues(t, 6000, cost, "every call is charged")

	m = &toolModel{answers: []toolAnswer{called(`{}`)}}
	out, cost, failure = callTool(m, 3, time.Millisecond)
	assert.Len(t, m.at, 3, "calls at most")
	assert.Empty(t, out)
	assert.Equal(t, FailureUnusable, failure)
	assert.EqualValues(t, 6000, cost)

	// An answer without the tool's call, or with another tool's, is one that
	// doesn't read; only the tool's first call is read.
	m = &toolModel{answers: []toolAnswer{
		{chunks: []Chunk{Text{Text: "Here you go"}, toolUsed}},
		{chunks: []Chunk{ToolUse{Name: "other", Input: json.RawMessage(`{"ok": true}`)}}},
		{chunks: []Chunk{ToolUse{Name: "pick", Input: json.RawMessage(`{}`)}, ToolUse{Name: "pick", Input: json.RawMessage(`{"ok": true}`)}}},
	}}
	_, _, failure = callTool(m, 3, time.Millisecond)
	assert.Equal(t, FailureUnusable, failure)
	assert.Len(t, m.at, 3)
}

func TestCallToolRetriesOnlyWhatARetryMayPass(t *testing.T) {
	busy := toolAnswer{err: &Error{Kind: ErrRateLimited, Err: errors.New("slow down")}}
	m := &toolModel{answers: []toolAnswer{busy, called(`{"ok": true}`)}}
	out, _, failure := callTool(m, 3, time.Millisecond)
	assert.Equal(t, "read", out)
	assert.Empty(t, failure)
	assert.Len(t, m.at, 2)

	m = &toolModel{answers: []toolAnswer{busy}}
	_, _, failure = callTool(m, 3, time.Millisecond)
	assert.Len(t, m.at, 3)
	assert.Equal(t, "the model provider couldn't answer: rate_limited", failure)

	for _, c := range []struct {
		err  error
		want string
	}{
		{&Error{Kind: ErrAuth, Err: errors.New("bad key")}, "the model provider refused: auth"},
		{errors.New("connection reset"), FailureCall},
	} {
		m = &toolModel{answers: []toolAnswer{{chunks: []Chunk{toolUsed}, err: c.err}}}
		_, cost, failure := callTool(m, 3, time.Millisecond)
		assert.Len(t, m.at, 1, c.want)
		assert.Equal(t, c.want, failure)
		assert.EqualValues(t, 2000, cost, "a failed call's usage is charged")
	}
}

// A passing refusal is retried after a wait that grows: 20ms, then 40ms.
func TestCallToolWaitsBeforeARetry(t *testing.T) {
	busy := toolAnswer{err: &Error{Kind: ErrUnavailable, Err: errors.New("overloaded")}}
	m := &toolModel{answers: []toolAnswer{busy}}
	_, _, _ = callTool(m, 3, 20*time.Millisecond)
	require.Len(t, m.at, 3)
	assert.GreaterOrEqual(t, m.at[1].Sub(m.at[0]), 20*time.Millisecond)
	assert.GreaterOrEqual(t, m.at[2].Sub(m.at[1]), 40*time.Millisecond)

	// A wait that would pass ctx's end ends the calls.
	m = &toolModel{answers: []toolAnswer{busy}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, failure := CallTool(ctx, toolBinding(m), Request{}, "pick", 3, time.Hour, parseOK)
	assert.Len(t, m.at, 1)
	assert.Equal(t, FailureTimeout, failure)
}

// waitingModel waits for its context's end, as a call waiting for a busy
// server's slot does, and then refuses as the gate does: unavailable.
type waitingModel struct{ calls int }

func (m *waitingModel) Stream(ctx context.Context, _ Request) iter.Seq2[Chunk, error] {
	m.calls++
	return func(yield func(Chunk, error) bool) {
		<-ctx.Done()
		yield(nil, &Error{Kind: ErrUnavailable, Err: ctx.Err()})
	}
}

// Out of time, a refusal a retry might pass isn't retried.
func TestCallToolDoesntRetryOutOfTime(t *testing.T) {
	m := &waitingModel{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	out, cost, failure := CallTool(ctx, toolBinding(m), Request{}, "pick", 3, time.Millisecond, parseOK)
	assert.Equal(t, 1, m.calls)
	assert.Empty(t, out)
	assert.Equal(t, FailureTimeout, failure)
	assert.Zero(t, cost)
}
