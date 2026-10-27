package foundryread

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	foundryreadv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundryread/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	houseKey = "sk-ant-api03-house-sentinel-0123456789"
	ownKey   = "sk-ant-api03-own-sentinel-9876543210"
)

// titled is a title result whose model ended its turn with text.
func titled(text string) string {
	raw, _ := json.Marshal(text)
	return `{"outcome":{"text":{"text":` + string(raw) + `}},"usage":{"input":90,"cache_read":0,"cache_write":0,"output":4,"usd":0.0001}}`
}

func titleRequest() *foundryreadv1.TitleRequest {
	return &foundryreadv1.TitleRequest{WorkspaceId: "ws-1", GenerationId: "gen-1", Goal: "We sell <b>tractor parts</b>."}
}

func TestTitle(t *testing.T) {
	ctx := context.Background()

	// Helix's Claude titles on Haiku without reasoning, as titles always
	// were, its key in the call's metadata alone; the text comes back as the
	// model wrote it.
	t.Run("titles on Helix's Claude", func(t *testing.T) {
		h := newHarness(t)
		var logs bytes.Buffer
		resp, err := h.reader.Title(log.Into(ctx, slog.New(slog.NewJSONHandler(&logs, nil))), titleRequest())
		if err != nil || resp.GetText() != "Customer churn" {
			t.Fatalf("Title = %v, %v", resp, err)
		}
		if len(h.agent.titles) != 1 || fmt.Sprint(h.agent.titleKeys) != "[["+houseKey+"]]" {
			t.Fatalf("titles %v, keys %v", h.agent.titles, h.agent.titleKeys)
		}
		// The goal goes as its JSON string, as the backend's own call sent it.
		goal, _ := json.Marshal(titleRequest().GetGoal())
		want := `{"request_id":"gen-1","goal":` + string(goal) + `,"models":{"endpoint":{"provider":"anthropic","call_timeout_secs":600},` +
			`"title":{"name":"claude-haiku-5-5","price":"table","reasoning":false}}}`
		if h.agent.titles[0] != want {
			t.Fatalf("title request = %s\nwant %s", h.agent.titles[0], want)
		}
		if err := contracttest.Validate(t, contracttest.Schema(t, "title_request.json"), []byte(h.agent.titles[0])); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h.agent.titles[0]+logs.String(), houseKey) {
			t.Fatal("the key reached the request or a log")
		}
	})

	// A workspace's own provider titles on its document-reading model, with
	// its own key.
	t.Run("titles on the workspace's own key", func(t *testing.T) {
		h := newHarness(t)
		keys, _ := secretbox.NewKeyring("k1", map[string][]byte{"k1": make([]byte, 32)})
		h.ai.Keyring = keys
		settings, _ := json.Marshal(ai.Settings{Provider: ai.Anthropic{}, Models: ai.Roles{
			Draft: ai.Model{Name: "claude-opus-5-5", Price: ai.PriceTable{}}, Ask: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}},
			Extract: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}}, Verify: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}},
		}})
		c := &model.FoundryAIConfig{ID: "cfg-1", WorkspaceID: "ws-1", Kind: "anthropic", Settings: settings}
		c.Secret, _ = ai.SealKey(keys, c.ID, ontologyagent.NewModelKey(ownKey))
		h.store.ai = map[string]*model.FoundryAIConfig{"ws-1": c}
		if _, err := h.reader.Title(ctx, titleRequest()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(h.agent.titles[0], `"title":{"name":"claude-sonnet-5-5","price":"table","reasoning":false}`) ||
			fmt.Sprint(h.agent.titleKeys) != "[["+ownKey+"]]" {
			t.Fatalf("title request %s, keys %v", h.agent.titles[0], h.agent.titleKeys)
		}
		// A key that doesn't open is refused before the sidecar is called.
		c.Secret.Ciphertext[0] ^= 0xff
		if _, err := h.reader.Title(ctx, titleRequest()); status.Code(err) != codes.FailedPrecondition ||
			!strings.Contains(err.Error(), "can't be opened") || len(h.agent.titles) != 1 {
			t.Fatalf("Title = %v", err)
		}
	})

	// Whatever goes wrong is a status the backend falls back on, never the key.
	t.Run("refuses what it can't title", func(t *testing.T) {
		for name, tc := range map[string]struct {
			setup func(h *harness)
			code  codes.Code
		}{
			"no goal":                {func(h *harness) {}, codes.InvalidArgument},
			"another contract":       {func(h *harness) { h.agent.contract = "3.12.0" }, codes.FailedPrecondition},
			"a kind it doesn't call": {func(h *harness) { h.agent.providers = []string{"openai"} }, codes.FailedPrecondition},
			"a failed model": {func(h *harness) {
				h.agent.title = func(string) (string, error) {
					return `{"outcome":{"failed":{"failure":"auth"}},"usage":{"input":0,"cache_read":0,"cache_write":0,"output":0,"usd":0}}`, nil
				}
			}, codes.FailedPrecondition},
			"a busy sidecar": {func(h *harness) {
				h.agent.title = func(string) (string, error) { return "", status.Error(codes.ResourceExhausted, "8 titles") }
			}, codes.ResourceExhausted},
			"an unreadable result": {func(h *harness) {
				h.agent.title = func(string) (string, error) { return `{}`, nil }
			}, codes.Internal},
		} {
			h := newHarness(t)
			tc.setup(h)
			req := titleRequest()
			if name == "no goal" {
				req.Goal = ""
			}
			var logs bytes.Buffer
			_, err := h.reader.Title(log.Into(ctx, slog.New(slog.NewJSONHandler(&logs, nil))), req)
			if status.Code(err) != tc.code || strings.Contains(err.Error()+logs.String(), houseKey) {
				t.Errorf("%s: Title = %v", name, err)
			}
		}
	})

	// A title falls back to its goal's first sentence, so a model's ordinary
	// trouble isn't on-call's: only Helix's key, account or models are.
	t.Run("raises only Helix's account's failures with on-call", func(t *testing.T) {
		for failure, loud := range map[string]bool{
			"truncated": false, "unsupported": false, "context_too_long": false, "refused": false, "unavailable": false,
			"auth": true, "permission": true, "not_found": true, "quota": true,
		} {
			h := newHarness(t)
			h.agent.title = func(string) (string, error) {
				return `{"outcome":{"failed":{"failure":"` + failure + `"}},"usage":{"input":90,"cache_read":0,"cache_write":0,"output":32,"usd":0.0001}}`, nil
			}
			var logs bytes.Buffer
			_, err := h.reader.Title(log.Into(ctx, slog.New(slog.NewJSONHandler(&logs, nil))), titleRequest())
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("%s: Title = %v", failure, err)
			}
			if got := strings.Contains(logs.String(), `"level":"ERROR"`); got != loud {
				t.Errorf("%s: logged an error %v, want %v: %s", failure, got, loud, logs.String())
			}
		}
	})

	// Each title spends the workspace's key, so a workspace writes only a few
	// at once; the backend titles the rest with their goals.
	t.Run("caps a workspace's titles at once", func(t *testing.T) {
		h := newHarness(t)
		for range MaxTitlesPerWorkspace {
			if _, ok := h.reader.titles.acquire("ws-1"); !ok {
				t.Fatal("no slot")
			}
		}
		if _, err := h.reader.Title(ctx, titleRequest()); status.Code(err) != codes.ResourceExhausted || len(h.agent.titles) != 0 {
			t.Fatalf("Title = %v; the sidecar wrote %d", err, len(h.agent.titles))
		}
		other := titleRequest()
		other.WorkspaceId = "ws-2"
		if _, err := h.reader.Title(ctx, other); err != nil {
			t.Fatalf("another workspace: %v", err)
		}
	})

	// Titles take none of the workspace's read slots.
	t.Run("titles while the workspace's reads are full", func(t *testing.T) {
		h := newHarness(t)
		for range MaxReadsPerWorkspace {
			if _, ok := h.reader.limit.acquire("ws-1"); !ok {
				t.Fatal("no slot")
			}
		}
		if _, err := h.reader.Title(ctx, titleRequest()); err != nil {
			t.Fatal(err)
		}
	})
}
