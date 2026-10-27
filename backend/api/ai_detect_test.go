package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/helixdb/helix-foundry/backend/ai/detect"
	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
)

// fakeOllama is an Ollama server with one model that calls tools, once
// pulled is true.
func fakeOllama(t *testing.T, pulled *atomic.Bool) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/version":
			_, _ = w.Write([]byte(`{"version":"0.40.10"}`))
		case "/api/tags":
			if !pulled.Load() {
				_, _ = w.Write([]byte(`{"models":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"models":[{"name":"llama3.2:1b","size":1321098329,"details":{"parameter_size":"1.2B"}},
				{"name":"nomic-embed-text:latest","size":274302450,"details":{"parameter_size":"137M"}}]}`))
		case "/api/show":
			if strings.Contains(r.URL.RawQuery+readAll(r), "nomic") {
				_, _ = w.Write([]byte(`{"capabilities":["embedding"],"model_info":{}}`))
				return
			}
			_, _ = w.Write([]byte(`{"capabilities":["completion","tools"],"model_info":{"llama.context_length":131072}}`))
		}
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func readAll(r *http.Request) string {
	var b strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

// closed is an address nothing listens on.
func closed(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

// detecting is a local install that names no default and looks for one on
// this machine: only base, an Ollama server.
func detecting(t *testing.T, base string) (*Service, *aiStore) {
	t.Helper()
	cfg := aiConfig(modeLocal, true, "ollama", "anthropic")
	cfg.Host.DetectedAI = &config.LiveAIDefault{}
	s, store := aiService(t, cfg, "")
	s.deps.LocalAI = &detect.Detector{Ollamas: []string{base}, LMStudio: closed(t) + "/v1", Live: cfg.Host.DetectedAI, Check: s.deps.AI.CheckDefault}
	s.deps.LocalAI.Detect(context.Background())
	return s, store
}

// The settings page reads what was found and why; opening it looks again,
// so a model pulled since is the default without a restart, and going back
// to the default is going back to it.
func TestDetectFoundryAIDefault(t *testing.T) {
	ctx := ports.WithActor(context.Background(), owner)
	var pulled atomic.Bool
	s, store := detecting(t, fakeOllama(t, &pulled))
	got, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
	if err != nil {
		t.Fatal(err)
	}
	if a := got.GetAi(); a.GetDefault() != nil || !strings.HasPrefix(a.GetDetected().GetSummary(), "No AI provider was found.") || a.GetDetected().GetDetectedAt() == nil {
		t.Fatalf("before: %v", a)
	}
	if _, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1",
		Choice: &foundryv1.UpdateFoundryAIRequest_UseDefault{UseDefault: &emptypb.Empty{}}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("use_default with none: %v", err)
	}
	if err := s.requireAI(ctx, "ws-1"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("model work with none: %v", err)
	}

	pulled.Store(true)
	got, err = s.DetectFoundryAIDefault(ctx, &foundryv1.DetectFoundryAIDefaultRequest{WorkspaceId: "ws-1"})
	if err != nil {
		t.Fatal(err)
	}
	a := got.GetAi()
	if d := a.GetDefault(); d.GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OLLAMA || d.GetLabel() != "Ollama" ||
		d.GetModels().GetDraft().GetName() != "llama3.2:1b" || d.GetModels().GetDraft().GetFree() == nil {
		t.Fatalf("default = %v", d)
	}
	if !strings.HasPrefix(a.GetDetected().GetSummary(), "Using llama3.2:1b from the Ollama server at 127.0.0.1:") {
		t.Fatalf("detected = %v", a.GetDetected())
	}
	if err := s.requireAI(ctx, "ws-1"); err != nil {
		t.Fatalf("model work on what was found: %v", err)
	}
	if _, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1",
		Choice: &foundryv1.UpdateFoundryAIRequest_UseDefault{UseDefault: &emptypb.Empty{}}}); err != nil || len(store.defaults) != 1 {
		t.Fatalf("use_default: %v, %d activations", err, len(store.defaults))
	}
}

// Only a local install that names no default looks, and only a local
// install reads Ollama servers: hosted, both are refused, as they are where
// the install's .env names its provider.
func TestLocalAIRoutesRefused(t *testing.T) {
	ctx := ports.WithActor(context.Background(), owner)
	hosted, _ := aiService(t, aiConfig(modeCloud, false), "pro")
	named, _ := aiService(t, aiConfig(modeLocal, false, "ollama", "anthropic"), "")
	named.deps.LocalAI = &detect.Detector{Live: &config.LiveAIDefault{}}
	for name, s := range map[string]*Service{"hosted": hosted, "an install that names its default": named} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.DetectFoundryAIDefault(ctx, &foundryv1.DetectFoundryAIDefaultRequest{WorkspaceId: "ws-1"}); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("detect: %v", err)
			}
			got, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
			if err != nil || got.GetAi().GetDetected() != nil || got.GetAi().GetDefault() == nil {
				t.Fatalf("settings: %v, %v", got, err)
			}
		})
	}
	if _, err := hosted.ListFoundryOllamaModels(ctx, &foundryv1.ListFoundryOllamaModelsRequest{WorkspaceId: "ws-1", BaseUrl: "http://127.0.0.1:11434"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("hosted models: %v", err)
	}
	for _, req := range []*foundryv1.DetectFoundryAIDefaultRequest{{}} {
		if _, err := named.DetectFoundryAIDefault(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("no workspace: %v", err)
		}
	}
}

// The Ollama card lists a server's models, each with why Foundry can't run
// on it; an address a save would refuse is refused here too.
func TestListFoundryOllamaModels(t *testing.T) {
	ctx := ports.WithActor(context.Background(), owner)
	var pulled atomic.Bool
	pulled.Store(true)
	base := fakeOllama(t, &pulled)
	s, _ := detecting(t, base)
	got, err := s.ListFoundryOllamaModels(ctx, &foundryv1.ListFoundryOllamaModelsRequest{WorkspaceId: "ws-1", BaseUrl: " " + base + "/ "})
	if err != nil {
		t.Fatal(err)
	}
	srv := got.GetServer()
	if srv.GetVersion() != "0.40.10" || srv.GetProblem() != "" || len(srv.GetModels()) != 2 {
		t.Fatalf("server = %v", srv)
	}
	if m := srv.GetModels()[0]; m.GetName() != "llama3.2:1b" || m.GetSizeBytes() != 1321098329 || m.GetParameterSize() != "1.2B" || m.GetUnusable() != "" {
		t.Fatalf("first = %v", m)
	}
	if m := srv.GetModels()[1]; m.GetName() != "nomic-embed-text:latest" || m.GetUnusable() != "Not a chat model." {
		t.Fatalf("second = %v", m)
	}
	// A server that doesn't answer is an answer that says so.
	got, err = s.ListFoundryOllamaModels(ctx, &foundryv1.ListFoundryOllamaModelsRequest{WorkspaceId: "ws-1", BaseUrl: closed(t)})
	if err != nil || !strings.HasPrefix(got.GetServer().GetProblem(), "Nothing answered at") {
		t.Fatalf("closed: %v, %v", got, err)
	}
	for _, bad := range []string{"", "ftp://x", base + "/v1", base + "/api", "http://u:p@x:11434", "http://x:11434?q=1"} {
		if _, err := s.ListFoundryOllamaModels(ctx, &foundryv1.ListFoundryOllamaModelsRequest{WorkspaceId: "ws-1", BaseUrl: bad}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if _, err := s.ListFoundryOllamaModels(ctx, &foundryv1.ListFoundryOllamaModelsRequest{BaseUrl: base}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no workspace: %v", err)
	}
	// Without Ollama offered, there's no card to list for.
	cfg := aiConfig(modeLocal, true, "anthropic")
	noOllama, _ := aiService(t, cfg, "")
	noOllama.deps.LocalAI = s.deps.LocalAI
	if _, err := noOllama.ListFoundryOllamaModels(ctx, &foundryv1.ListFoundryOllamaModelsRequest{WorkspaceId: "ws-1", BaseUrl: base}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no Ollama offered: %v", err)
	}
}
