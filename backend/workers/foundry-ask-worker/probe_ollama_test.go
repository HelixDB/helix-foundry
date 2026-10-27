package foundryaskworker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func TestProbeOllama(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	free := ai.Model{Name: "qwen3:8b", Price: ai.PriceFree{}}
	// settings are an Ollama server on this computer running one model, in a
	// window larger than the default.
	settings := ai.Settings{
		Provider: ai.Ollama{BaseURL: "http://host.docker.internal:11434", ContextTokens: 65536},
		Models:   ai.Roles{Draft: free, Ask: free, Extract: free, Verify: free},
	}
	// probing queues settings, and has the agent answer as probeResult does,
	// saying what the server read of the model.
	probing := func(t *testing.T, failing, failure, detail, ollama string) *probeHarness {
		t.Helper()
		h := newProbeHarness(t)
		raw, _ := json.Marshal(settings)
		h.queue.waiting = &model.FoundryAIConfig{ID: "cfg-o", WorkspaceID: "ws-1", Kind: "ollama", Settings: raw}
		h.agent.info.Providers = []string{"anthropic", "ollama"}
		h.agent.probe = func(req string) (string, error) {
			out, err := probeResult(failing, failure, detail)(req)
			if ollama != "" {
				out = strings.Replace(out, `"usage"`, `"ollama":`+ollama+`,"usage"`, 1)
			}
			return out, err
		}
		return h
	}

	// A passed probe keeps the digest and window the server read, which the
	// setting's runs then pin; its request tests the owner's window, keyless.
	t.Run("keeps what the server read of the model", func(t *testing.T) {
		h := probing(t, "", "", "", `{"digest":"`+digest+`","context_length":40960,"capabilities":["completion","tools"]}`)
		h.tick(t)
		if len(h.queue.finished) != 1 || h.queue.finished[0].Failure != nil || len(h.queue.released) != 0 {
			t.Fatalf("finished %+v, released %v", h.queue.finished, h.queue.released)
		}
		var report ai.ProbeReport
		if err := json.Unmarshal(h.queue.finished[0].Report, &report); err != nil {
			t.Fatal(err)
		}
		if report.Ollama == nil || *report.Ollama != (ai.OllamaReport{Digest: digest, ContextLength: 40960}) {
			t.Fatalf("report = %s", h.queue.finished[0].Report)
		}
		probe := h.agent.probes[0]
		if !strings.Contains(probe, `"provider":{"ollama":{"base_url":"http://host.docker.internal:11434"}}`) ||
			!strings.Contains(probe, `"max_calls_in_flight":1`) || !strings.Contains(probe, `"context_tokens":65536`) ||
			strings.Contains(probe, `"revision"`) || len(h.agent.probeKeys[0]) != 0 {
			t.Fatalf("probe %s, keys %v", probe, h.agent.probeKeys)
		}
		// The setting then runs on what the probe read.
		r := ai.Resolved{Source: ai.SourceWorkspace, Settings: settings, Report: report}
		if spec := r.Turn().Draft; spec.Revision != digest || spec.ContextTokens != 40960 {
			t.Fatalf("draft = %+v", spec)
		}
	})

	// A window too small for drafting fails the setting with that detail, a
	// model the server lacks with not_found; the one before stays.
	t.Run("records an Ollama check's failure", func(t *testing.T) {
		for _, tc := range []struct{ failure, detail string }{{"unsupported", "context_too_small"}, {"not_found", "none"}} {
			h := probing(t, "draft", tc.failure, tc.detail, "")
			h.tick(t)
			if len(h.queue.finished) != 1 || h.queue.finished[0].Failure == nil ||
				*h.queue.finished[0].Failure != (model.FoundryAIProbeFailure{Failure: tc.failure, Detail: tc.detail}) {
				t.Fatalf("%s: finished %+v", tc.detail, h.queue.finished)
			}
		}
	})

	// A digest the agent wouldn't take as a revision would fail every run:
	// the probe couldn't run, and is given back for another attempt. A
	// failed probe has nothing for its runs to send, so is recorded.
	t.Run("gives back a probe whose digest isn't one", func(t *testing.T) {
		h := probing(t, "", "", "", `{"digest":"qwen3:8b","context_length":40960,"capabilities":["tools"]}`)
		logs := h.tick(t)
		if len(h.queue.finished) != 0 || len(h.queue.released) != 1 || !strings.Contains(logs, "Ollama digest") {
			t.Fatalf("finished %+v, released %v, logs %s", h.queue.finished, h.queue.released, logs)
		}
		h = probing(t, "ask", "unsupported", "tools_rejected", `{"digest":"","context_length":0,"capabilities":[]}`)
		h.tick(t)
		if len(h.queue.finished) != 1 || h.queue.finished[0].Failure == nil || len(h.queue.released) != 0 {
			t.Fatalf("finished %+v, released %v", h.queue.finished, h.queue.released)
		}
	})
}
