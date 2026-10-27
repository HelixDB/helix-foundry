package ontologyagent

import "testing"

// NewTestGenerator is a Generator over newHarness's fakes, for the external
// tests that draft on the ai package's resolver: the agent answers every
// turn with a result of status (its JSON), and calls and keys are each
// Turn's request and model key metadata.
func NewTestGenerator(t *testing.T, status string) (g *Generator, calls func() []string, keys func() [][]string) {
	h := newHarness(t, respond(status))
	return h.generator, h.agent.calls, func() [][]string {
		h.agent.mu.Lock()
		defer h.agent.mu.Unlock()
		return append([][]string(nil), h.agent.keys...)
	}
}

// TestGeneration is a generation of the fakes' workspace, ws-1.
var TestGeneration = generation
