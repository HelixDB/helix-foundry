package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// ownKey is a workspace's own key: it must reach no response, error, log
// or row but the sealed column.
const ownKey = "sk-ant-api03-sentinel-own-key-4f2a"

// hostMode is where Foundry runs: hosted, or a standalone install.
type hostMode string

const (
	modeCloud hostMode = "cloud"
	modeLocal hostMode = "local"
)

// aiConfig is a cloud or local foundry.ai, Helix's Claude the default
// unless noDefault.
func aiConfig(mode hostMode, noDefault bool, kinds ...string) *config.Config {
	cfg := config.Defaults()
	cfg.Host.Local = mode == modeLocal
	if !noDefault {
		cfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: "sk-ant-helix-0123456789", Models: config.AIModels{
			Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
		}}
	}
	if len(kinds) > 0 {
		cfg.Foundry.AI.Kinds = kinds
	}
	if mode == modeLocal {
		cfg.Foundry.AI.AllowPrivateHosts = true
	}
	return &cfg
}

func deployment(t testing.TB, cfg *config.Config) *ai.Deployment {
	t.Helper()
	d, err := ai.NewDeployment(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// hostedAI is hosted Foundry's foundry.ai: Helix's Claude, Claude offered.
func hostedAI(t testing.TB) *ai.Deployment { return deployment(t, aiConfig(modeCloud, false)) }

// aiStore keeps one workspace's AI setting in memory.
type aiStore struct {
	foundrystore.Unimplemented
	view     foundrymodel.FoundryAIView
	saved    []*foundrymodel.FoundryAIConfig
	perHour  []int
	defaults []string
	// full refuses every save as over the hourly cap.
	full bool
	// deleting refuses every write, as a workspace being deleted does.
	deleting bool
}

// aiPeople knows user-owner as ws-1's owner, who may manage its AI (a
// plain member and anyone else may not), and every user as Ada.
type aiPeople struct{}

func (aiPeople) MayManageAI(_ context.Context, ws, userID string) bool {
	return ws == "ws-1" && userID == "user-owner"
}

func (aiPeople) DisplayName(context.Context, string) (string, error) { return "Ada Lovelace", nil }

func (s *aiStore) GetFoundryAIView(context.Context, string) (foundrymodel.FoundryAIView, error) {
	return s.view, nil
}

func (s *aiStore) GetActiveFoundryAIConfig(context.Context, string) (*foundrymodel.FoundryAIConfig, error) {
	if s.view.Active == nil {
		return nil, foundrystore.ErrNotFound
	}
	return s.view.Active, nil
}

func (s *aiStore) CreateFoundryAIConfig(_ context.Context, c *foundrymodel.FoundryAIConfig, perHour int) error {
	s.perHour = append(s.perHour, perHour)
	if s.deleting {
		return foundrystore.ErrNotFound
	}
	if s.full {
		return foundrystore.ErrLimitExceeded
	}
	c.ProbeState, c.CreatedAt = foundrymodel.FoundryAIProbeQueued, time.Now()
	s.saved = append(s.saved, c)
	s.view.Pending = c
	return nil
}

func (s *aiStore) ActivateFoundryAIDefault(_ context.Context, _, userID string) error {
	if s.deleting {
		return foundrystore.ErrNotFound
	}
	s.defaults = append(s.defaults, userID)
	s.view = foundrymodel.FoundryAIView{}
	return nil
}

func ptr[T any](v T) *T { return &v }

// fakeDNS answers every name with a public address, 20.0.0.1, but those
// starting "inward.", which resolve to 10.0.0.5: a save's host check is
// tested without the network.
func fakeDNS(t *testing.T) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true})
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			if q.Type == dnsmessage.TypeA {
				addr := [4]byte{20, 0, 0, 1}
				if strings.HasPrefix(q.Name.String(), "inward.") {
					addr = [4]byte{10, 0, 0, 5}
				}
				_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					dnsmessage.AResource{A: addr})
			}
			if out, err := b.Finish(); err == nil {
				_, _ = pc.WriteTo(out, from)
			}
		}
	}()
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
}

func aiKeyring(t *testing.T) *secretbox.Keyring {
	t.Helper()
	k, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// aiService is the backend with cfg's foundry.ai, the workspace on plan.
func aiService(t *testing.T, cfg *config.Config, plan string) (*Service, *aiStore) {
	t.Helper()
	store := &aiStore{}
	s := &Service{deps: &runtimeDeps{Cfg: cfg, DB: store, People: aiPeople{}, AI: deployment(t, cfg), ConnectorKeys: aiKeyring(t), AIResolver: fakeDNS(t)}}
	if !cfg.Host.Local {
		onPlans(s, &stubFoundryPlans{status: held(plan)})
	}
	return s, store
}

// onPlans has s read workspaces' plans from plans, the billing system's stand-in.
func onPlans(s *Service, plans ports.Plans) {
	s.deps.Plans, s.deps.AIPlans = plans, ai.NewPlans(plans)
}

// countedPlans counts its reads of plans.
type countedPlans struct {
	*stubFoundryPlans
	reads int
}

func (c *countedPlans) Status(ctx context.Context, ws string) (foundryplan.Status, error) {
	c.reads++
	return c.stubFoundryPlans.Status(ctx, ws)
}

// owner is ws-1's owner, signed in to the site.
var owner = ports.Actor{Source: ports.SourceUser, UserID: "user-owner"}

func claudeChoice() *foundryv1.UpdateFoundryAIRequest_Provider {
	return &foundryv1.UpdateFoundryAIRequest_Provider{Provider: &foundryv1.FoundryAIProvider{
		Kind: &foundryv1.FoundryAIProvider_Anthropic{Anthropic: &foundryv1.FoundryAIAnthropic{}},
	}}
}

func reason(err error) string {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}
	return ""
}

// savedClaude is an active Claude config of the workspace, sealed with key.
func savedClaude(t *testing.T, keys *secretbox.Keyring, id, key string) *foundrymodel.FoundryAIConfig {
	t.Helper()
	roles, _ := ai.Suggested(ai.KindAnthropic)
	raw, err := json.Marshal(ai.Settings{Provider: ai.Anthropic{}, Models: roles})
	if err != nil {
		t.Fatal(err)
	}
	c := &foundrymodel.FoundryAIConfig{ID: id, WorkspaceID: "ws-1", Kind: "anthropic", Settings: raw, ProbeState: foundrymodel.FoundryAIProbePassed,
		SecretHint: ptr("sk-ant…4f2a"), CreatedBy: "user-owner", CreatedAt: time.Now()}
	if c.Secret, err = ai.SealKey(keys, id, ontologyagent.NewModelKey(key)); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGetFoundryAI(t *testing.T) {
	ctx := ports.WithActor(context.Background(), owner)

	// A workspace that never chose runs on Helix's Claude, and an owner may
	// bring a Claude key, starting from the house's models.
	t.Run("shows Helix's Claude and what an owner may bring", func(t *testing.T) {
		s, _ := aiService(t, aiConfig(modeCloud, false), "pro")
		out, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		got := out.GetAi()
		if got.GetActive() != nil || got.GetPending() != nil || got.GetLastFailed() != nil || got.GetProRequired() || got.GetPrivateHostsAllowed() {
			t.Fatalf("FoundryAI = %v", got)
		}
		def := got.GetDefault()
		if def.GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_ANTHROPIC || def.GetLabel() != "Claude" ||
			def.GetModels().GetDraft().GetName() != "claude-opus-5-5" || def.GetModels().GetDraft().GetTable() == nil {
			t.Fatalf("default = %v", def)
		}
		if len(got.GetOffered()) != 1 || got.GetOffered()[0].GetKey() != foundryv1.FoundryAIKey_FOUNDRY_AI_KEY_REQUIRED ||
			got.GetOffered()[0].GetSuggested().GetExtract().GetName() != "claude-haiku-5-5" {
			t.Fatalf("offered = %v", got.GetOffered())
		}
	})

	// The page says a key needs Pro before an owner tries, and still loads
	// when the billing system can't say.
	t.Run("says when bringing a key needs Pro", func(t *testing.T) {
		s, _ := aiService(t, aiConfig(modeCloud, false), "free")
		out, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
		if err != nil || !out.GetAi().GetProRequired() {
			t.Fatalf("Free: %v, %v", out, err)
		}
		onPlans(s, &stubFoundryPlans{err: status.Error(codes.Unavailable, "billing down")})
		if out, err = s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"}); err != nil || out.GetAi().GetProRequired() {
			t.Fatalf("billing down: %v, %v", out, err)
		}
	})

	// The page polls while a probe runs: a sold plan is read from the billing system once
	// a minute, and Free every time, so an upgrade shows at once.
	t.Run("reads a sold plan from the billing system once a minute", func(t *testing.T) {
		for plan, want := range map[string]int{"pro": 1, "free": 3} {
			s, _ := aiService(t, aiConfig(modeCloud, false), plan)
			counted := &countedPlans{stubFoundryPlans: &stubFoundryPlans{status: held(plan)}}
			onPlans(s, counted)
			for range 3 {
				if _, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"}); err != nil {
					t.Fatal(err)
				}
			}
			if counted.reads != want {
				t.Errorf("%s: %d reads of the billing system for 3 polls, want %d", plan, counted.reads, want)
			}
		}
	})

	// The setting, a pending test and the last failure show which key each
	// holds by its hint alone.
	t.Run("shows the configs without their keys", func(t *testing.T) {
		s, store := aiService(t, aiConfig(modeCloud, false), "pro")
		keys := s.deps.ConnectorKeys
		store.view.Active = savedClaude(t, keys, "cfg-1", ownKey)
		failed := savedClaude(t, keys, "cfg-2", ownKey)
		failed.Secret, failed.ProbeState, failed.ProbeFailure, failed.ProbeDetail = nil, foundrymodel.FoundryAIProbeFailed, ptr("auth"), ptr("none")
		store.view.LastFailed = failed
		out, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		active, last := out.GetAi().GetActive(), out.GetAi().GetLastFailed()
		if active.GetId() != "cfg-1" || !active.GetHasKey() || active.GetKeyHint() != "sk-ant…4f2a" || active.GetLabel() != "Claude" ||
			active.GetCreatedByName() != "Ada Lovelace" || active.GetProbeState() != foundryv1.FoundryAIProbeState_FOUNDRY_AI_PROBE_STATE_PASSED ||
			active.GetProvider().GetAnthropic() == nil {
			t.Fatalf("active = %v", active)
		}
		if last.GetHasKey() || last.GetProbeFailure() != "auth" || last.GetProbeDetail() != "none" {
			t.Fatalf("last failed = %v", last)
		}
		raw, _ := protojson.Marshal(out)
		if strings.Contains(string(raw), ownKey) || strings.Contains(string(raw), "sentinel") {
			t.Fatalf("the response holds the key: %s", raw)
		}
		// Only those who may change the setting see a key's hint.
		for name, actor := range map[string]ports.Actor{"a member": {Source: ports.SourceUser, UserID: "user-member"}, "no member": {Source: ports.SourceUser, UserID: "user-x"}, "no actor": {}} {
			out, err := s.GetFoundryAI(ports.WithActor(ctx, actor), &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
			if err != nil || !out.GetAi().GetActive().GetHasKey() || out.GetAi().GetActive().GetKeyHint() != "" {
				t.Fatalf("%s: active = %v, %v", name, out.GetAi().GetActive(), err)
			}
		}
	})

	// A local install with no default says so, and allows endpoints on the
	// same machine.
	t.Run("shows a local install with no provider", func(t *testing.T) {
		s, _ := aiService(t, aiConfig(modeLocal, true), "")
		out, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		if out.GetAi().GetDefault() != nil || !out.GetAi().GetPrivateHostsAllowed() || out.GetAi().GetProRequired() {
			t.Fatalf("FoundryAI = %v", out.GetAi())
		}
	})
}

func TestUpdateFoundryAI(t *testing.T) {
	ctx := ports.WithActor(context.Background(), owner)
	update := func(mutate ...func(*foundryv1.UpdateFoundryAIRequest)) *foundryv1.UpdateFoundryAIRequest {
		req := &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1", Choice: claudeChoice(), ApiKey: ownKey}
		for _, m := range mutate {
			m(req)
		}
		return req
	}

	// A Pro owner's Claude key is sealed under its own row and queued for
	// its test; the setting in force is untouched until it passes. Models
	// left out get Claude's suggestions.
	t.Run("saves a key for its test", func(t *testing.T) {
		s, store := aiService(t, aiConfig(modeCloud, false), "pro")
		out, err := s.UpdateFoundryAI(ctx, update(func(r *foundryv1.UpdateFoundryAIRequest) {
			r.Models = &foundryv1.FoundryAIModels{Draft: &foundryv1.FoundryAIModel{Name: "claude-opus-5"}}
		}))
		if err != nil {
			t.Fatal(err)
		}
		if len(store.saved) != 1 || store.perHour[0] != 10 {
			t.Fatalf("saved %d configs, per hour %v", len(store.saved), store.perHour)
		}
		c := store.saved[0]
		if c.WorkspaceID != "ws-1" || c.CreatedBy != "user-owner" || c.Kind != "anthropic" || *c.SecretHint != "sk-ant…4f2a" {
			t.Fatalf("saved = %+v", c)
		}
		key, err := ai.OpenKey(s.deps.ConnectorKeys, c)
		if err != nil || key.Reveal() != ownKey {
			t.Fatalf("the sealed key opens to %q, %v", key.Reveal(), err)
		}
		settings, err := ai.DecodeSettings(c.Settings)
		if err != nil || settings.Models.Draft.Name != "claude-opus-5" || settings.Models.Ask.Name != "claude-sonnet-5-5" {
			t.Fatalf("settings = %+v, %v", settings, err)
		}
		if strings.Contains(string(c.Settings), ownKey) || bytes.Contains(c.Secret.Ciphertext, []byte(ownKey)) {
			t.Fatal("the key is stored in the clear")
		}
		pending := out.GetAi().GetPending()
		if pending.GetId() != c.ID || pending.GetProbeState() != foundryv1.FoundryAIProbeState_FOUNDRY_AI_PROBE_STATE_QUEUED || out.GetAi().GetActive() != nil {
			t.Fatalf("response = %v", out.GetAi())
		}
	})

	// A blank key keeps the setting's where it still belongs to the
	// endpoint: opened under the setting's row, sealed under the new one's.
	t.Run("keeps the setting's key when left blank", func(t *testing.T) {
		s, store := aiService(t, aiConfig(modeCloud, false), "max")
		store.view.Active = savedClaude(t, s.deps.ConnectorKeys, "cfg-1", ownKey)
		if _, err := s.UpdateFoundryAI(ctx, update(func(r *foundryv1.UpdateFoundryAIRequest) { r.ApiKey = "" })); err != nil {
			t.Fatal(err)
		}
		c := store.saved[0]
		key, err := ai.OpenKey(s.deps.ConnectorKeys, c)
		if err != nil || key.Reveal() != ownKey || *c.SecretHint != "sk-ant…4f2a" || c.ID == "cfg-1" {
			t.Fatalf("kept key %q, %v, hint %v", key.Reveal(), err, c.SecretHint)
		}
		if bytes.Equal(c.Secret.Ciphertext, store.view.Active.Secret.Ciphertext) {
			t.Fatal("the key was copied, not sealed again under the new row")
		}
	})

	// Going back to Helix's Claude is immediate, needs no plan, and takes
	// nothing else.
	t.Run("goes back to the default", func(t *testing.T) {
		s, store := aiService(t, aiConfig(modeCloud, false), "free")
		store.view.Active = savedClaude(t, s.deps.ConnectorKeys, "cfg-1", ownKey)
		useDefault := func(r *foundryv1.UpdateFoundryAIRequest) {
			r.Choice, r.ApiKey = &foundryv1.UpdateFoundryAIRequest_UseDefault{UseDefault: &emptypb.Empty{}}, ""
		}
		out, err := s.UpdateFoundryAI(ctx, update(useDefault))
		if err != nil || out.GetAi().GetActive() != nil || len(store.defaults) != 1 || store.defaults[0] != "user-owner" {
			t.Fatalf("UpdateFoundryAI() = %v, %v; defaults %v", out, err, store.defaults)
		}
		_, err = s.UpdateFoundryAI(ctx, update(useDefault, func(r *foundryv1.UpdateFoundryAIRequest) { r.ApiKey = ownKey }))
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("use_default with a key = %v", err)
		}
		local, _ := aiService(t, aiConfig(modeLocal, true), "")
		if _, err := local.UpdateFoundryAI(ctx, update(useDefault)); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("use_default with no default = %v", err)
		}
	})

	// Each check refuses in its turn, with the code and reason the site
	// routes on, and before anything is stored.
	compatible := func(base string) func(*foundryv1.UpdateFoundryAIRequest) {
		return func(r *foundryv1.UpdateFoundryAIRequest) {
			r.Choice = &foundryv1.UpdateFoundryAIRequest_Provider{Provider: &foundryv1.FoundryAIProvider{
				Kind: &foundryv1.FoundryAIProvider_OpenaiCompatible{OpenaiCompatible: &foundryv1.FoundryAICompatible{
					BaseUrl: base, Api: foundryv1.FoundryAICompatAPI_FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS,
				}}}}
			r.Models = &foundryv1.FoundryAIModels{
				Draft: &foundryv1.FoundryAIModel{Name: "m", Price: &foundryv1.FoundryAIModel_Free{Free: &emptypb.Empty{}}},
				Ask:   &foundryv1.FoundryAIModel{Name: "m", Price: &foundryv1.FoundryAIModel_Free{Free: &emptypb.Empty{}}},
				Extract: &foundryv1.FoundryAIModel{Name: "m", Price: &foundryv1.FoundryAIModel_Rates{Rates: &foundryv1.FoundryAIRates{
					InputUsdPerMtok: "0.075", OutputUsdPerMtok: "0.3",
				}}},
				Verify: &foundryv1.FoundryAIModel{Name: "m", Price: &foundryv1.FoundryAIModel_Free{Free: &emptypb.Empty{}}},
			}
		}
	}
	unpriced := func(r *foundryv1.UpdateFoundryAIRequest) {
		r.Models = &foundryv1.FoundryAIModels{Ask: &foundryv1.FoundryAIModel{Name: "claude-next-sentinel"}}
	}
	// keyAsModel is an owner's key pasted into a model's name.
	keyAsModel := func(key string) func(*foundryv1.UpdateFoundryAIRequest) {
		return func(r *foundryv1.UpdateFoundryAIRequest) {
			r.ApiKey = "plain-sentinel-key-0123456789abcdef"
			r.Models = &foundryv1.FoundryAIModels{Draft: &foundryv1.FoundryAIModel{Name: key}}
		}
	}
	// refusals is what each refusal says, so each is the check meant.
	refusals := map[string]string{
		"no choice":                 "choose use_default or a provider",
		"a kind not offered":        "is not offered here",
		"a Free workspace":          "needs Foundry Pro",
		"Free before pricing":       "needs Foundry Pro",
		"a model name with a space": "models.draft.name must be printable",
		"a bad rate":                "models.extract.price.rates.input_usd_per_mtok must be a price",
		"a private host":            "the endpoint resolves to a private address",
		"http hosted":               "provider.base_url must be https",
		"no key to send or keep":    "Enter the API key for this endpoint.",
		"a key with a newline":      "printable characters",
		"an unpriced model":         "Helix has no price for the ask model; enter its prices",
		"a key as a model":          "models.draft.name looks like an API key",
		"the key as a model":        "models.draft.name is the API key",
		"no keyring":                "keyring is not configured",
		"over the hourly cap":       "10 times an hour",
		"a key for Ollama":          "Ollama takes no API key",
	}
	for name, tc := range map[string]struct {
		cfg    *config.Config
		plan   string
		mutate []func(*foundryv1.UpdateFoundryAIRequest)
		noKeys bool
		full   bool
		code   codes.Code
		reason string
	}{
		"no choice":           {aiConfig(modeCloud, false), "pro", []func(*foundryv1.UpdateFoundryAIRequest){func(r *foundryv1.UpdateFoundryAIRequest) { r.Choice = nil }}, false, false, codes.InvalidArgument, ""},
		"a kind not offered":  {aiConfig(modeCloud, false), "pro", []func(*foundryv1.UpdateFoundryAIRequest){compatible("https://llm.example.com/v1")}, false, false, codes.InvalidArgument, ""},
		"a Free workspace":    {aiConfig(modeCloud, false), "free", nil, false, false, codes.FailedPrecondition, "FOUNDRY_PRO_REQUIRED"},
		"Free before pricing": {aiConfig(modeCloud, false), "free", []func(*foundryv1.UpdateFoundryAIRequest){unpriced}, false, false, codes.FailedPrecondition, "FOUNDRY_PRO_REQUIRED"},
		"a model name with a space": {aiConfig(modeCloud, false), "pro", []func(*foundryv1.UpdateFoundryAIRequest){func(r *foundryv1.UpdateFoundryAIRequest) {
			r.Models = &foundryv1.FoundryAIModels{Draft: &foundryv1.FoundryAIModel{Name: "claude opus"}}
			r.ApiKey = ""
		}}, false, false, codes.InvalidArgument, ""},
		"a bad rate":             {aiConfig(modeCloud, false, "anthropic", "openai_compatible"), "pro", []func(*foundryv1.UpdateFoundryAIRequest){compatible("https://llm.example.com/v1"), func(r *foundryv1.UpdateFoundryAIRequest) { r.Models.Extract.GetRates().InputUsdPerMtok = "1e3" }}, false, false, codes.InvalidArgument, ""},
		"a private host":         {aiConfig(modeCloud, false, "anthropic", "openai_compatible"), "pro", []func(*foundryv1.UpdateFoundryAIRequest){compatible("https://localhost/v1")}, false, false, codes.InvalidArgument, ""},
		"http hosted":            {aiConfig(modeCloud, false, "anthropic", "openai_compatible"), "pro", []func(*foundryv1.UpdateFoundryAIRequest){compatible("http://llm.example.com/v1")}, false, false, codes.InvalidArgument, ""},
		"no key to send or keep": {aiConfig(modeCloud, false), "pro", []func(*foundryv1.UpdateFoundryAIRequest){func(r *foundryv1.UpdateFoundryAIRequest) { r.ApiKey = "" }}, false, false, codes.InvalidArgument, ""},
		"a key with a newline":   {aiConfig(modeCloud, false), "pro", []func(*foundryv1.UpdateFoundryAIRequest){func(r *foundryv1.UpdateFoundryAIRequest) { r.ApiKey = "sk-ant\nx" + ownKey }}, false, false, codes.InvalidArgument, ""},
		"an unpriced model":      {aiConfig(modeCloud, false), "pro", []func(*foundryv1.UpdateFoundryAIRequest){unpriced}, false, false, codes.FailedPrecondition, "FOUNDRY_AI_PRICE_REQUIRED"},
		"a key as a model":       {aiConfig(modeCloud, false), "pro", []func(*foundryv1.UpdateFoundryAIRequest){keyAsModel(ownKey)}, false, false, codes.InvalidArgument, ""},
		"the key as a model":     {aiConfig(modeCloud, false), "pro", []func(*foundryv1.UpdateFoundryAIRequest){keyAsModel("plain-sentinel-key-0123456789abcdef")}, false, false, codes.InvalidArgument, ""},
		"no keyring":             {aiConfig(modeCloud, false), "pro", nil, true, false, codes.Unavailable, ""},
		"over the hourly cap":    {aiConfig(modeCloud, false), "pro", nil, false, true, codes.ResourceExhausted, ""},
		"a key for Ollama": {aiConfig(modeLocal, true, "ollama"), "", []func(*foundryv1.UpdateFoundryAIRequest){func(r *foundryv1.UpdateFoundryAIRequest) {
			r.Choice = &foundryv1.UpdateFoundryAIRequest_Provider{Provider: &foundryv1.FoundryAIProvider{
				Kind: &foundryv1.FoundryAIProvider_Ollama{Ollama: &foundryv1.FoundryAIOllama{BaseUrl: "http://127.0.0.1:11434"}}}}
			r.Models = &foundryv1.FoundryAIModels{
				Draft:   &foundryv1.FoundryAIModel{Name: "qwen3:32b", Price: &foundryv1.FoundryAIModel_Free{Free: &emptypb.Empty{}}},
				Ask:     &foundryv1.FoundryAIModel{Name: "qwen3:32b", Price: &foundryv1.FoundryAIModel_Free{Free: &emptypb.Empty{}}},
				Extract: &foundryv1.FoundryAIModel{Name: "qwen3:32b", Price: &foundryv1.FoundryAIModel_Free{Free: &emptypb.Empty{}}},
				Verify:  &foundryv1.FoundryAIModel{Name: "qwen3:32b", Price: &foundryv1.FoundryAIModel_Free{Free: &emptypb.Empty{}}},
			}
		}}, false, false, codes.InvalidArgument, ""},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			s, store := aiService(t, tc.cfg, tc.plan)
			store.full = tc.full
			if tc.noKeys {
				s.deps.ConnectorKeys = nil
			}
			_, err := s.UpdateFoundryAI(ctx, update(tc.mutate...))
			if status.Code(err) != tc.code || reason(err) != tc.reason {
				t.Fatalf("UpdateFoundryAI() = %v (reason %q), want %v %q", err, reason(err), tc.code, tc.reason)
			}
			if want := refusals[name]; !strings.Contains(status.Convert(err).Message(), want) {
				t.Fatalf("UpdateFoundryAI() = %v, want a refusal saying %q", err, want)
			}
			if strings.Contains(status.Convert(err).Message(), "sentinel") {
				t.Fatalf("the error names the key: %v", err)
			}
			if len(store.saved) > 0 {
				t.Fatalf("a refused save was stored: %+v", store.saved)
			}
		})
	}

	// A call with no signed-in user is refused before anything is read.
	t.Run("refuses a signed-out caller", func(t *testing.T) {
		s, store := aiService(t, aiConfig(modeCloud, false), "pro")
		_, err := s.UpdateFoundryAI(context.Background(), update())
		if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "signed-in owner") || len(store.saved) > 0 {
			t.Fatalf("UpdateFoundryAI() = %v, saved %+v", err, store.saved)
		}
	})

	// A workspace being deleted dropped its keys and takes no new setting.
	t.Run("refuses a workspace being deleted", func(t *testing.T) {
		s, store := aiService(t, aiConfig(modeCloud, false), "pro")
		store.deleting = true
		for name, req := range map[string]*foundryv1.UpdateFoundryAIRequest{
			"a provider":  update(),
			"the default": {WorkspaceId: "ws-1", Choice: &foundryv1.UpdateFoundryAIRequest_UseDefault{UseDefault: &emptypb.Empty{}}},
		} {
			if _, err := s.UpdateFoundryAI(ctx, req); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "being deleted") {
				t.Errorf("%s: UpdateFoundryAI() = %v", name, err)
			}
		}
	})

	// A local install sells no plan and has no Helix key: an owner brings
	// one freely, and a server on the same machine may be its endpoint.
	t.Run("lets a local owner bring any offered provider", func(t *testing.T) {
		s, store := aiService(t, aiConfig(modeLocal, true, "anthropic", "openai_compatible"), "")
		if _, err := s.UpdateFoundryAI(ctx, update()); err != nil {
			t.Fatalf("Claude: %v", err)
		}
		if _, err := s.UpdateFoundryAI(ctx, update(compatible("http://host.docker.internal:8000/v1/"), func(r *foundryv1.UpdateFoundryAIRequest) { r.ApiKey = "" })); err != nil {
			t.Fatalf("a local server: %v", err)
		}
		settings, err := ai.DecodeSettings(store.saved[1].Settings)
		if err != nil {
			t.Fatal(err)
		}
		server := settings.Provider.(ai.Compatible)
		if server.BaseURL != "http://host.docker.internal:8000/v1" || server.MaxCallsInFlight != ai.DefaultCallsInFlight || store.saved[1].Secret != nil {
			t.Fatalf("the local server = %+v, secret %v", server, store.saved[1].Secret)
		}
		if rates := settings.Models.Extract.Price.(ai.PriceRates); rates.Input != 75 || rates.Output != 300 {
			t.Fatalf("rates = %+v", rates)
		}
	})

	// Nothing the save logs carries the key.
	t.Run("logs no key", func(t *testing.T) {
		var out bytes.Buffer
		ctx := log.Into(ports.WithActor(context.Background(), owner), slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
		s, _ := aiService(t, aiConfig(modeCloud, false), "pro")
		onPlans(s, &stubFoundryPlans{status: held("pro")})
		if _, err := s.UpdateFoundryAI(ctx, update()); err != nil {
			t.Fatal(err)
		}
		onPlans(s, &stubFoundryPlans{err: status.Error(codes.Unavailable, "billing down")})
		_, _ = s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
		if strings.Contains(out.String(), "sentinel") {
			t.Fatalf("logged the key: %s", out.String())
		}
	})
}

// Prices travel as dollars per million tokens and are stored as whole
// nano-dollars per token.
func TestFoundryAIPrices(t *testing.T) {
	for usd, nanos := range map[string]uint64{"0": 0, "15": 15_000, "3.75": 3_750, "0.075": 75, "9999999.999": 9_999_999_999} {
		got, err := nanosPerToken(usd)
		if err != nil || got != nanos {
			t.Errorf("nanosPerToken(%q) = %d, %v; want %d", usd, got, err, nanos)
		}
		if back := usdPerMTok(nanos); back != usd {
			t.Errorf("usdPerMTok(%d) = %q, want %q", nanos, back, usd)
		}
	}
	for _, bad := range []string{"", "-1", "1e3", "0.0001", "1.", ".5", "12345678", "NaN", "Inf"} {
		if _, err := nanosPerToken(bad); err == nil {
			t.Errorf("nanosPerToken(%q) accepted", bad)
		}
	}
}

// Model work is refused where its worker would refuse it: a local install
// with no default whose workspace chose none, or a hosted workspace's own key
// off Pro.
func TestRequireAI(t *testing.T) {
	ctx := ports.WithActor(context.Background(), owner)
	s, store := aiService(t, aiConfig(modeLocal, true), "")
	err := s.requireAI(ctx, "ws-1")
	if status.Code(err) != codes.FailedPrecondition || reason(err) != "FOUNDRY_AI_NOT_CONFIGURED" ||
		!strings.Contains(status.Convert(err).Message(), "Settings › AI models") {
		t.Fatalf("requireAI() = %v", err)
	}
	store.view.Active = savedClaude(t, s.deps.ConnectorKeys, "cfg-1", ownKey)
	if err := s.requireAI(ctx, "ws-1"); err != nil {
		t.Fatalf("with a provider chosen: %v", err)
	}
	hosted, hostedStore := aiService(t, aiConfig(modeCloud, false), "free")
	if err := hosted.requireAI(ctx, "ws-1"); err != nil {
		t.Fatalf("hosted: %v", err)
	}
	// Hosted, a workspace's own key off Pro is refused before anything is
	// queued, with the reason the site gates on, as its worker would refuse it.
	hostedStore.view.Active = savedClaude(t, hosted.deps.ConnectorKeys, "cfg-1", ownKey)
	if err := hosted.requireAI(ctx, "ws-1"); status.Code(err) != codes.FailedPrecondition || reason(err) != "FOUNDRY_PRO_REQUIRED" {
		t.Fatalf("own key on Free: requireAI() = %v", err)
	}
	onPlans(hosted, &stubFoundryPlans{status: held("pro")})
	if err := hosted.requireAI(ctx, "ws-1"); err != nil {
		t.Fatalf("own key on Pro: %v", err)
	}

	// Each RPC that starts model work asks first: drafts, questions and
	// document uploads, which their loads read.
	none := deployment(t, aiConfig(modeLocal, true))
	gen, _ := generationService(t, 50)
	gen.deps.AI, gen.deps.DB = none, noAIChosen{gen.deps.DB}
	_, err = gen.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
	if reason(err) != "FOUNDRY_AI_NOT_CONFIGURED" {
		t.Fatalf("GenerateOntology() = %v", err)
	}
	asks, _ := questionService(t)
	asks.deps.AI, asks.deps.DB = none, noAIChosen{asks.deps.DB}
	if _, err := ask(asks, "token-1", "Which customers churned?"); reason(err) != "FOUNDRY_AI_NOT_CONFIGURED" {
		t.Fatalf("AskFoundryQuestion() = %v", err)
	}
	docs, _, _ := foundryService(t)
	created, err := docs.CreateFoundryDocument(ctx, &foundryv1.CreateFoundryDocumentRequest{
		WorkspaceId: "ws-1", Name: "contract.md", MediaType: "text/markdown", SizeBytes: 120,
		Sha256: strings.Repeat("ab", 32), ClientToken: "up-1", SetId: "set-general",
	})
	if err != nil {
		t.Fatal(err)
	}
	docs.deps.AI, docs.deps.DB = none, noAIChosen{docs.deps.DB}
	_, err = docs.CompleteFoundryDocumentUpload(ctx, &foundryv1.CompleteFoundryDocumentUploadRequest{
		WorkspaceId: "ws-1", Id: created.GetDocument().GetId(),
	})
	if reason(err) != "FOUNDRY_AI_NOT_CONFIGURED" {
		t.Fatalf("CompleteFoundryDocumentUpload() = %v", err)
	}
}

// noAIChosen is a store whose workspaces chose no AI provider.
type noAIChosen struct{ foundrystore.Store }

func (noAIChosen) GetActiveFoundryAIConfig(context.Context, string) (*foundrymodel.FoundryAIConfig, error) {
	return nil, foundrystore.ErrNotFound
}

// OpenAI and Azure OpenAI save as Claude does, under a Pro owner's key: an
// OpenAI key from OpenAI's suggestions, an Azure key for a resource's
// deployments, each priced as the model it runs.
func TestUpdateFoundryAIOpenAI(t *testing.T) {
	ctx := ports.WithActor(context.Background(), owner)
	const openaiKey = "sk-proj-sentinel-openai-key-0123456789-9b1c"
	const azureKey = "0123456789abcdef0123456789sentinel"
	cfg := func(mode hostMode) *config.Config {
		return aiConfig(mode, false, "anthropic", "openai", "azure_openai")
	}
	openai := &foundryv1.UpdateFoundryAIRequest_Provider{Provider: &foundryv1.FoundryAIProvider{
		Kind: &foundryv1.FoundryAIProvider_Openai{Openai: &foundryv1.FoundryAIOpenAI{}},
	}}
	azure := func(endpoint string) *foundryv1.UpdateFoundryAIRequest_Provider {
		return &foundryv1.UpdateFoundryAIRequest_Provider{Provider: &foundryv1.FoundryAIProvider{
			Kind: &foundryv1.FoundryAIProvider_AzureOpenai{AzureOpenai: &foundryv1.FoundryAIAzureOpenAI{Endpoint: endpoint}},
		}}
	}
	// deployments are an Azure resource's: drafts on draft, the rest on prod-luna,
	// each priced as the model it deploys.
	deployments := func(draft string) *foundryv1.FoundryAIModels {
		as := func(name, model string) *foundryv1.FoundryAIModel {
			return &foundryv1.FoundryAIModel{Name: name, Price: &foundryv1.FoundryAIModel_TableAs{TableAs: model}}
		}
		return &foundryv1.FoundryAIModels{Draft: as(draft, "gpt-6.1-sol"), Ask: as("prod-sol", "gpt-6.1-sol"),
			Extract: as("prod-luna", "gpt-6-luna"), Verify: as("prod-sol", "gpt-6.1-sol")}
	}
	saved := func(t *testing.T, s *Service, c *foundrymodel.FoundryAIConfig) (ai.Settings, string) {
		t.Helper()
		settings, err := ai.DecodeSettings(c.Settings)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ai.OpenKey(s.deps.ConnectorKeys, c)
		if err != nil {
			t.Fatal(err)
		}
		return settings, key.Reveal()
	}

	// The page offers both, OpenAI with its suggested models and Azure with
	// none: a deployment's name is its owner's.
	t.Run("offers OpenAI and Azure", func(t *testing.T) {
		s, _ := aiService(t, cfg(modeCloud), "pro")
		out, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		offered := out.GetAi().GetOffered()
		if len(offered) != 3 || offered[1].GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OPENAI ||
			offered[1].GetKey() != foundryv1.FoundryAIKey_FOUNDRY_AI_KEY_REQUIRED || offered[1].GetSuggested().GetDraft().GetName() != "gpt-6.1-sol" ||
			offered[1].GetSuggested().GetExtract().GetName() != "gpt-6-luna" || offered[1].GetSuggested().GetDraft().GetTable() == nil ||
			offered[2].GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_AZURE_OPENAI || offered[2].GetSuggested() != nil ||
			offered[2].GetKey() != foundryv1.FoundryAIKey_FOUNDRY_AI_KEY_REQUIRED {
			t.Fatalf("offered = %v", offered)
		}
		// Helix's Claude is still the default.
		if def := out.GetAi().GetDefault(); def.GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_ANTHROPIC || def.GetLabel() != "Claude" {
			t.Fatalf("default = %v", def)
		}
	})

	// An OpenAI key is sealed under its row with its hint; models left out
	// are OpenAI's suggestions, and the page shows the setting as OpenAI.
	t.Run("saves an OpenAI key", func(t *testing.T) {
		s, store := aiService(t, cfg(modeCloud), "pro")
		out, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1", Choice: openai, ApiKey: openaiKey,
			Models: &foundryv1.FoundryAIModels{Draft: &foundryv1.FoundryAIModel{Name: "gpt-6-astra"}}})
		if err != nil {
			t.Fatal(err)
		}
		c := store.saved[0]
		settings, key := saved(t, s, c)
		if c.Kind != "openai" || *c.SecretHint != "sk-proj…9b1c" || key != openaiKey || settings.Provider != (ai.OpenAI{}) ||
			settings.Models.Draft.Name != "gpt-6-astra" || settings.Models.Ask.Name != "gpt-6.1-sol" || settings.Models.Extract.Name != "gpt-6-luna" {
			t.Fatalf("saved %+v: %+v", c, settings)
		}
		pending := out.GetAi().GetPending()
		if pending.GetLabel() != "OpenAI" || pending.GetProvider().GetOpenai() == nil || pending.GetKeyHint() != "sk-proj…9b1c" ||
			pending.GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OPENAI {
			t.Fatalf("pending = %v", pending)
		}
		raw, _ := protojson.Marshal(out)
		if strings.Contains(string(raw), "sentinel") || strings.Contains(string(c.Settings), "sentinel") {
			t.Fatalf("the key leaked: %s", raw)
		}
	})

	// An Azure endpoint is stored normalized with its deployments, each
	// priced as the model it deploys; the key is the resource's.
	t.Run("saves an Azure resource's key", func(t *testing.T) {
		s, store := aiService(t, cfg(modeCloud), "max")
		out, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1",
			Choice: azure(" https://Acme.openai.azure.com/ "), ApiKey: azureKey, Models: deployments("prod-sol")})
		if err != nil {
			t.Fatal(err)
		}
		settings, key := saved(t, s, store.saved[0])
		if settings.Provider != (ai.AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai/v1"}) || key != azureKey ||
			settings.Models.Extract != (ai.Model{Name: "prod-luna", Price: ai.PriceTableAs{Model: "gpt-6-luna"}}) ||
			*store.saved[0].SecretHint != "…inel" || store.saved[0].Kind != "azure_openai" {
			t.Fatalf("saved %+v, hint %q", settings, *store.saved[0].SecretHint)
		}
		if p := out.GetAi().GetPending(); p.GetLabel() != "Azure OpenAI" ||
			p.GetProvider().GetAzureOpenai().GetEndpoint() != "https://acme.openai.azure.com/openai/v1" ||
			p.GetModels().GetDraft().GetTableAs() != "gpt-6.1-sol" {
			t.Fatalf("pending = %v", p)
		}
	})

	// A blank key keeps the setting's only where it still belongs: another
	// OpenAI save, or the same Azure resource drafting on the same
	// deployment. Moving between them needs the new endpoint's key.
	t.Run("keeps a key only for its own endpoint", func(t *testing.T) {
		s, store := aiService(t, cfg(modeCloud), "pro")
		save := func(choice *foundryv1.UpdateFoundryAIRequest_Provider, models *foundryv1.FoundryAIModels, key string) error {
			_, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1", Choice: choice, ApiKey: key, Models: models})
			if err == nil {
				store.view.Active = store.saved[len(store.saved)-1]
			}
			return err
		}
		if err := save(azure("https://acme.openai.azure.com"), deployments("prod-sol"), azureKey); err != nil {
			t.Fatal(err)
		}
		if err := save(azure("https://ACME.openai.azure.com/openai/v1/"), deployments("prod-sol"), ""); err != nil {
			t.Fatalf("the same resource and deployment: %v", err)
		}
		if _, key := saved(t, s, store.view.Active); key != azureKey {
			t.Fatalf("kept %q", key)
		}
		for name, tc := range map[string]struct {
			choice *foundryv1.UpdateFoundryAIRequest_Provider
			models *foundryv1.FoundryAIModels
		}{
			"another deployment": {azure("https://acme.openai.azure.com"), deployments("test-sol")},
			"another resource":   {azure("https://evil.openai.azure.com"), deployments("prod-sol")},
			"OpenAI":             {openai, nil},
			"Claude":             {claudeChoice(), nil},
		} {
			if err := save(tc.choice, tc.models, ""); status.Code(err) != codes.InvalidArgument ||
				!strings.Contains(err.Error(), "Enter the API key for this endpoint.") {
				t.Errorf("%s: %v", name, err)
			}
		}
		if err := save(openai, nil, openaiKey); err != nil {
			t.Fatal(err)
		}
		if err := save(openai, &foundryv1.FoundryAIModels{Draft: &foundryv1.FoundryAIModel{Name: "gpt-6-astra"}}, ""); err != nil {
			t.Fatalf("OpenAI with other models: %v", err)
		}
		if _, key := saved(t, s, store.view.Active); key != openaiKey {
			t.Fatalf("kept %q", key)
		}
	})

	// What each refuses, and why: before anything is stored.
	for name, tc := range map[string]struct {
		plan   string
		kinds  []string
		req    *foundryv1.UpdateFoundryAIRequest
		code   codes.Code
		reason string
		says   string
	}{
		"OpenAI on Free": {"free", nil, &foundryv1.UpdateFoundryAIRequest{Choice: openai, ApiKey: openaiKey},
			codes.FailedPrecondition, "FOUNDRY_PRO_REQUIRED", "needs Foundry Pro"},
		"Azure on Free": {"free", nil, &foundryv1.UpdateFoundryAIRequest{Choice: azure("https://acme.openai.azure.com"), ApiKey: azureKey, Models: deployments("prod-sol")},
			codes.FailedPrecondition, "FOUNDRY_PRO_REQUIRED", "needs Foundry Pro"},
		"OpenAI not offered": {"pro", []string{"anthropic"}, &foundryv1.UpdateFoundryAIRequest{Choice: openai, ApiKey: openaiKey},
			codes.InvalidArgument, "", "OpenAI is not offered here"},
		"Azure not offered": {"pro", []string{"anthropic", "openai"}, &foundryv1.UpdateFoundryAIRequest{Choice: azure("https://acme.openai.azure.com"), ApiKey: azureKey, Models: deployments("prod-sol")},
			codes.InvalidArgument, "", "Azure OpenAI is not offered here"},
		"an OpenAI model priced as another": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: openai, ApiKey: openaiKey, Models: &foundryv1.FoundryAIModels{
			Draft: &foundryv1.FoundryAIModel{Name: "gpt-6.1-sol-ft", Price: &foundryv1.FoundryAIModel_TableAs{TableAs: "gpt-6.1-sol"}}}},
			codes.InvalidArgument, "", "models.draft.price can be another model's only"},
		"an unpriced OpenAI model": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: openai, ApiKey: openaiKey, Models: &foundryv1.FoundryAIModels{
			Extract: &foundryv1.FoundryAIModel{Name: "gpt-sentinel-next"}}},
			codes.FailedPrecondition, "FOUNDRY_AI_PRICE_REQUIRED", "no price for the extract model"},
		"an Azure deployment by its own name": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: azure("https://acme.openai.azure.com"), ApiKey: azureKey,
			Models: func() *foundryv1.FoundryAIModels {
				m := deployments("prod-sol")
				m.Ask.Price = &foundryv1.FoundryAIModel_Table{Table: &emptypb.Empty{}}
				return m
			}()},
			codes.FailedPrecondition, "FOUNDRY_AI_PRICE_REQUIRED", "no price for the ask model"},
		"an Azure deployment of an unpriced model": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: azure("https://acme.openai.azure.com"), ApiKey: azureKey,
			Models: func() *foundryv1.FoundryAIModels {
				m := deployments("prod-sol")
				m.Verify.Price = &foundryv1.FoundryAIModel_TableAs{TableAs: "gpt-sentinel"}
				return m
			}()},
			codes.FailedPrecondition, "FOUNDRY_AI_PRICE_REQUIRED", "no price for the verify model"},
		"Azure with no deployments": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: azure("https://acme.openai.azure.com"), ApiKey: azureKey},
			codes.InvalidArgument, "", "models.draft is required"},
		"Azure off Azure": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: azure("https://sentinel.example.com"), ApiKey: azureKey, Models: deployments("prod-sol")},
			codes.InvalidArgument, "", "provider.endpoint must be an Azure OpenAI endpoint"},
		"an Azure name that resolves inward": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: azure("https://inward.openai.azure.com"), ApiKey: azureKey, Models: deployments("prod-sol")},
			codes.InvalidArgument, "", "the endpoint resolves to a private address"},
		"Azure over http": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: azure("http://acme.openai.azure.com"), ApiKey: azureKey, Models: deployments("prod-sol")},
			codes.InvalidArgument, "", "provider.endpoint must be https"},
		"Azure with no key": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: azure("https://acme.openai.azure.com"), Models: deployments("prod-sol")},
			codes.InvalidArgument, "", "Enter the API key for this endpoint."},
		"an OpenAI key as a model": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: openai, ApiKey: azureKey, Models: &foundryv1.FoundryAIModels{
			Draft: &foundryv1.FoundryAIModel{Name: openaiKey}}},
			codes.InvalidArgument, "", "models.draft.name looks like an API key"},
		"an Azure key as a deployment": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: azure("https://acme.openai.azure.com"), ApiKey: azureKey,
			Models: deployments(azureKey)},
			codes.InvalidArgument, "", "models.draft.name is the API key"},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			c := cfg(modeCloud)
			if tc.kinds != nil {
				c.Foundry.AI.Kinds = tc.kinds
			}
			s, store := aiService(t, c, tc.plan)
			tc.req.WorkspaceId = "ws-1"
			_, err := s.UpdateFoundryAI(ctx, tc.req)
			if status.Code(err) != tc.code || reason(err) != tc.reason || !strings.Contains(status.Convert(err).Message(), tc.says) {
				t.Fatalf("UpdateFoundryAI() = %v (reason %q), want %v %q saying %q", err, reason(err), tc.code, tc.reason, tc.says)
			}
			if strings.Contains(status.Convert(err).Message(), "sentinel") {
				t.Fatalf("the error names the key or model: %v", err)
			}
			if len(store.saved) > 0 {
				t.Fatalf("a refused save was stored: %+v", store.saved)
			}
		})
	}

	// A local install offers both with no plan.
	t.Run("lets a local owner bring OpenAI", func(t *testing.T) {
		s, store := aiService(t, cfg(modeLocal), "")
		if _, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1", Choice: openai, ApiKey: openaiKey}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1",
			Choice: azure("https://acme.services.ai.azure.com/openai/v1"), ApiKey: azureKey, Models: deployments("prod-sol")}); err != nil {
			t.Fatal(err)
		}
		if len(store.saved) != 2 || store.saved[0].Kind != "openai" || store.saved[1].Kind != "azure_openai" {
			t.Fatalf("saved = %+v", store.saved)
		}
	})
}

// A custom OpenAI-compatible server saves as Claude does under a Pro owner,
// but its key is optional and its models are the owner's to name and price:
// at the server's rates, or free. Its base URL is stored in normal form, and
// a blank key keeps the stored one only for the same server.
func TestUpdateFoundryAICompatible(t *testing.T) {
	ctx := ports.WithActor(context.Background(), owner)
	const serverKey = "sk-gw-sentinel-0123456789abcdef-7c3e"
	cfg := func(mode hostMode) *config.Config { return aiConfig(mode, false, "anthropic", "openai_compatible") }
	const chat, responses = foundryv1.FoundryAICompatAPI_FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS, foundryv1.FoundryAICompatAPI_FOUNDRY_AI_COMPAT_API_RESPONSES
	server := func(base string, api foundryv1.FoundryAICompatAPI, calls uint32) *foundryv1.UpdateFoundryAIRequest_Provider {
		return &foundryv1.UpdateFoundryAIRequest_Provider{Provider: &foundryv1.FoundryAIProvider{
			Kind: &foundryv1.FoundryAIProvider_OpenaiCompatible{OpenaiCompatible: &foundryv1.FoundryAICompatible{BaseUrl: base, Api: api, MaxCallsInFlight: calls}},
		}}
	}
	free := func(name string) *foundryv1.FoundryAIModel {
		return &foundryv1.FoundryAIModel{Name: name, Price: &foundryv1.FoundryAIModel_Free{Free: &emptypb.Empty{}}}
	}
	// typed are the models an owner names: drafts at the server's rates, the rest free.
	typed := func() *foundryv1.FoundryAIModels {
		return &foundryv1.FoundryAIModels{
			Draft: &foundryv1.FoundryAIModel{Name: "llama-4-maverick", Price: &foundryv1.FoundryAIModel_Rates{Rates: &foundryv1.FoundryAIRates{
				InputUsdPerMtok: "0.27", OutputUsdPerMtok: "0.85",
			}}},
			Ask: free("llama-4-maverick"), Extract: free("qwen3-32b"), Verify: free("qwen3-32b"),
		}
	}
	save := func(s *Service, choice *foundryv1.UpdateFoundryAIRequest_Provider, models *foundryv1.FoundryAIModels, key string) (*foundryv1.FoundryAIResponse, error) {
		return s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1", Choice: choice, ApiKey: key, Models: models})
	}
	opened := func(t *testing.T, s *Service, c *foundrymodel.FoundryAIConfig) string {
		t.Helper()
		key, err := ai.OpenKey(s.deps.ConnectorKeys, c)
		if err != nil {
			t.Fatal(err)
		}
		return key.Reveal()
	}

	// The page offers a custom server with an optional key and no suggested
	// models, hosted and locally.
	t.Run("offers a custom server", func(t *testing.T) {
		for _, mode := range []hostMode{modeCloud, modeLocal} {
			s, _ := aiService(t, cfg(mode), "pro")
			out, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
			if err != nil {
				t.Fatal(err)
			}
			offered := out.GetAi().GetOffered()
			if len(offered) != 2 || offered[1].GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OPENAI_COMPATIBLE ||
				offered[1].GetKey() != foundryv1.FoundryAIKey_FOUNDRY_AI_KEY_OPTIONAL || offered[1].GetSuggested() != nil {
				t.Fatalf("%s: offered = %v", mode, offered)
			}
			if out.GetAi().GetPrivateHostsAllowed() != (mode == modeLocal) {
				t.Fatalf("%s: private hosts allowed = %v", mode, out.GetAi().GetPrivateHostsAllowed())
			}
		}
	})

	// A Pro owner's server is stored in normal form, sent four calls at once
	// unless the owner says, its key sealed with its hint; the page names it
	// by its host.
	t.Run("saves a server with its key", func(t *testing.T) {
		s, store := aiService(t, cfg(modeCloud), "pro")
		out, err := save(s, server(" HTTPS://LLM.Example.com:443/v1/ ", responses, 0), typed(), serverKey)
		if err != nil {
			t.Fatal(err)
		}
		c := store.saved[0]
		settings, err := ai.DecodeSettings(c.Settings)
		if err != nil {
			t.Fatal(err)
		}
		if c.Kind != "openai_compatible" || settings.Provider != (ai.Compatible{BaseURL: "https://llm.example.com/v1", API: ai.CompatResponses, MaxCallsInFlight: 4}) ||
			opened(t, s, c) != serverKey || *c.SecretHint != "sk-gw…7c3e" {
			t.Fatalf("saved %+v: %+v", c, settings)
		}
		if settings.Models.Draft != (ai.Model{Name: "llama-4-maverick", Price: ai.PriceRates{Input: 270, Output: 850}}) ||
			settings.Models.Verify != (ai.Model{Name: "qwen3-32b", Price: ai.PriceFree{}}) {
			t.Fatalf("models = %+v", settings.Models)
		}
		p := out.GetAi().GetPending()
		compat := p.GetProvider().GetOpenaiCompatible()
		if p.GetLabel() != "llm.example.com" || p.GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OPENAI_COMPATIBLE || !p.GetHasKey() ||
			p.GetKeyHint() != "sk-gw…7c3e" || compat.GetBaseUrl() != "https://llm.example.com/v1" || compat.GetApi() != responses ||
			compat.GetMaxCallsInFlight() != 4 || p.GetModels().GetDraft().GetRates().GetOutputUsdPerMtok() != "0.85" ||
			p.GetModels().GetAsk().GetFree() == nil {
			t.Fatalf("pending = %v", p)
		}
		raw, _ := protojson.Marshal(out)
		if strings.Contains(string(raw), "sentinel") || strings.Contains(string(c.Settings), "sentinel") {
			t.Fatalf("the key leaked: %s", raw)
		}
	})

	// A server that takes no key saves with none.
	t.Run("saves a server without a key", func(t *testing.T) {
		s, store := aiService(t, cfg(modeCloud), "max")
		out, err := save(s, server("https://llm.example.com/v1", chat, 64), typed(), "")
		if err != nil {
			t.Fatal(err)
		}
		if c := store.saved[0]; c.Secret != nil || c.SecretHint != nil || out.GetAi().GetPending().GetHasKey() || out.GetAi().GetPending().GetKeyHint() != "" {
			t.Fatalf("saved %+v, pending %v", c, out.GetAi().GetPending())
		}
	})

	// A blank key keeps the stored one for the same server, whatever its API,
	// calls at once or models; another URL, path or port, or another kind's
	// key, is never sent to it: the save has no key.
	t.Run("keeps a key only for the same server", func(t *testing.T) {
		s, store := aiService(t, cfg(modeCloud), "pro")
		keep := func(choice *foundryv1.UpdateFoundryAIRequest_Provider, models *foundryv1.FoundryAIModels, key string) *foundrymodel.FoundryAIConfig {
			t.Helper()
			if _, err := save(s, choice, models, key); err != nil {
				t.Fatal(err)
			}
			store.view.Active = store.saved[len(store.saved)-1]
			return store.view.Active
		}
		keep(server("https://llm.example.com/v1", chat, 4), typed(), serverKey)
		other := typed()
		other.Draft = free("deepseek-v4")
		c := keep(server("https://LLM.example.com:443/v1/", responses, 16), other, "")
		if opened(t, s, c) != serverKey || *c.SecretHint != "sk-gw…7c3e" || bytes.Equal(c.Secret.Ciphertext, store.saved[0].Secret.Ciphertext) {
			t.Fatalf("the same server kept %+v", c)
		}
		for name, base := range map[string]string{
			"another host": "https://evil.example.com/v1", "another path": "https://llm.example.com/openai/v1", "another port": "https://llm.example.com:8443/v1",
		} {
			store.view.Active = c
			if moved := keep(server(base, chat, 4), typed(), ""); moved.Secret != nil || moved.SecretHint != nil {
				t.Errorf("%s: the key went along: %+v", name, moved)
			}
		}
		store.view.Active = savedClaude(t, s.deps.ConnectorKeys, "cfg-claude", ownKey)
		if moved := keep(server("https://llm.example.com/v1", chat, 4), typed(), ""); moved.Secret != nil {
			t.Errorf("a Claude key went to a custom server: %+v", moved)
		}
	})

	// What each refuses, and why: before anything is stored, never echoing
	// what was typed.
	// priced are the typed models, role's replaced by named.
	priced := func(role ai.Role, named *foundryv1.FoundryAIModel) *foundryv1.FoundryAIModels {
		m := typed()
		switch role {
		case ai.RoleDraft:
			m.Draft = named
		case ai.RoleExtract:
			m.Extract = named
		case ai.RoleVerify:
			m.Verify = named
		}
		return m
	}
	for name, tc := range map[string]struct {
		plan   string
		kinds  []string
		req    *foundryv1.UpdateFoundryAIRequest
		code   codes.Code
		reason string
		says   string
	}{
		"a server on Free": {"free", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://llm.example.com/v1", chat, 4), Models: typed()},
			codes.FailedPrecondition, "FOUNDRY_PRO_REQUIRED", "needs Foundry Pro"},
		"a server not offered": {"pro", []string{"anthropic", "openai"}, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://llm.example.com/v1", chat, 4), Models: typed()},
			codes.InvalidArgument, "", "llm.example.com is not offered here"},
		"plain http": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("http://llm.example.com/v1", chat, 4), Models: typed()},
			codes.InvalidArgument, "", "provider.base_url must be https"},
		"a private address": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://10.0.0.5/v1", chat, 4), Models: typed()},
			codes.InvalidArgument, "", "the endpoint resolves to a private address"},
		"the metadata address": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://169.254.169.254/latest", chat, 4), Models: typed()},
			codes.InvalidArgument, "", "the endpoint resolves to a private address"},
		"a name that resolves inward": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://inward.llm.example.com/v1", chat, 4), Models: typed()},
			codes.InvalidArgument, "", "the endpoint resolves to a private address"},
		"this machine": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://localhost:8000/v1", chat, 4), Models: typed()},
			codes.InvalidArgument, "", "the endpoint resolves to a private address"},
		"a name read as an address": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://2130706433/v1", chat, 4), Models: typed()},
			codes.InvalidArgument, "", "provider.base_url must be an http or https URL"},
		"a user in the URL": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://sentinel:pw@llm.example.com/v1", chat, 4), Models: typed()},
			codes.InvalidArgument, "", "provider.base_url must have no user"},
		"a key in the URL": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://llm.example.com/v1?api_key=sentinel", chat, 4), Models: typed()},
			codes.InvalidArgument, "", "provider.base_url must have no user"},
		"no API": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://llm.example.com/v1", foundryv1.FoundryAICompatAPI_FOUNDRY_AI_COMPAT_API_UNSPECIFIED, 4), Models: typed()},
			codes.InvalidArgument, "", "provider.api must be chat_completions or responses"},
		"65 calls at once": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://llm.example.com/v1", chat, 65), Models: typed()},
			codes.InvalidArgument, "", "provider.max_calls_in_flight must be 1 to 64"},
		"no models": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://llm.example.com/v1", chat, 4)},
			codes.InvalidArgument, "", "models.draft is required"},
		"a model with no price": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://llm.example.com/v1", chat, 4),
			Models: priced(ai.RoleDraft, &foundryv1.FoundryAIModel{Name: "llama-sentinel"})},
			codes.FailedPrecondition, "FOUNDRY_AI_PRICE_REQUIRED", "Helix has no price for the draft model; enter its prices"},
		"an OpenAI model by Helix's table": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://llm.example.com/v1", chat, 4),
			Models: priced(ai.RoleVerify, &foundryv1.FoundryAIModel{Name: "gpt-6.1-sol", Price: &foundryv1.FoundryAIModel_Table{Table: &emptypb.Empty{}}})},
			codes.FailedPrecondition, "FOUNDRY_AI_PRICE_REQUIRED", "no price for the verify model"},
		"a model priced as another": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://llm.example.com/v1", chat, 4),
			Models: priced(ai.RoleExtract, &foundryv1.FoundryAIModel{Name: "qwen3-32b", Price: &foundryv1.FoundryAIModel_TableAs{TableAs: "gpt-6-luna"}})},
			codes.InvalidArgument, "", "models.extract.price can be another model's only"},
		"no output rate": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://llm.example.com/v1", chat, 4),
			Models: priced(ai.RoleDraft, &foundryv1.FoundryAIModel{Name: "llama-4-maverick", Price: &foundryv1.FoundryAIModel_Rates{Rates: &foundryv1.FoundryAIRates{
				InputUsdPerMtok: "0.27", OutputUsdPerMtok: "0",
			}}})},
			codes.InvalidArgument, "", "models.draft.price.rates.output must be more than 0"},
		"the key as a model": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: server("https://llm.example.com/v1", chat, 4), ApiKey: "gateway-sentinel-0123456789abcdef",
			Models: priced(ai.RoleDraft, free("gateway-sentinel-0123456789abcdef"))},
			codes.InvalidArgument, "", "models.draft.name is the API key"},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			c := cfg(modeCloud)
			if tc.kinds != nil {
				c.Foundry.AI.Kinds = tc.kinds
			}
			s, store := aiService(t, c, tc.plan)
			tc.req.WorkspaceId = "ws-1"
			_, err := s.UpdateFoundryAI(ctx, tc.req)
			if status.Code(err) != tc.code || reason(err) != tc.reason || !strings.Contains(status.Convert(err).Message(), tc.says) {
				t.Fatalf("UpdateFoundryAI() = %v (reason %q), want %v %q saying %q", err, reason(err), tc.code, tc.reason, tc.says)
			}
			if strings.Contains(status.Convert(err).Message(), "sentinel") {
				t.Fatalf("the error echoes what was typed: %v", err)
			}
			if len(store.saved) > 0 {
				t.Fatalf("a refused save was stored: %+v", store.saved)
			}
		})
	}

	// A local install sells no plan and allows servers on this machine and
	// its network, over http, with or without a key.
	t.Run("lets a local owner bring a server on this machine", func(t *testing.T) {
		s, store := aiService(t, cfg(modeLocal), "")
		for base, want := range map[string]string{
			"http://host.docker.internal:1234/v1/": "http://host.docker.internal:1234/v1",
			"http://localhost:11434/v1":            "http://localhost:11434/v1",
			"http://10.0.0.5:8000/v1":              "http://10.0.0.5:8000/v1",
		} {
			store.saved = nil
			if _, err := save(s, server(base, chat, 1), typed(), ""); err != nil {
				t.Fatalf("%s: %v", base, err)
			}
			settings, err := ai.DecodeSettings(store.saved[0].Settings)
			if err != nil || settings.Provider != (ai.Compatible{BaseURL: want, API: ai.CompatChatCompletions, MaxCallsInFlight: 1}) || store.saved[0].Secret != nil {
				t.Fatalf("%s: saved %+v, %v", base, settings, err)
			}
		}
		if _, err := save(s, server("http://host.docker.internal:4000/v1", responses, 0), typed(), serverKey); err != nil || opened(t, s, store.saved[1]) != serverKey {
			t.Fatalf("a keyed local gateway: %v", err)
		}
	})

	// Nothing a save or the page logs carries the key.
	t.Run("logs no key", func(t *testing.T) {
		var out bytes.Buffer
		ctx := log.Into(ports.WithActor(context.Background(), owner), slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
		s, _ := aiService(t, cfg(modeCloud), "pro")
		if _, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1",
			Choice: server("https://llm.example.com/v1", chat, 4), Models: typed(), ApiKey: serverKey}); err != nil {
			t.Fatal(err)
		}
		_, _ = s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
		if strings.Contains(out.String(), "sentinel") {
			t.Fatalf("logged the key: %s", out.String())
		}
	})
}

// Amazon Bedrock saves as Claude does, under a Pro owner's Bedrock API key:
// a region (FIPS optional), and model IDs, inference profiles' included,
// priced as their model by the table, as another model, or at the owner's
// rates.
func TestUpdateFoundryAIBedrock(t *testing.T) {
	ctx := ports.WithActor(context.Background(), owner)
	const bedrockKey = "ABSKsentinelQmVkcm9ja0FQSUtleS0wMTIz"
	cfg := func(mode hostMode) *config.Config { return aiConfig(mode, false, "anthropic", "bedrock") }
	bedrock := func(region string, fips bool) *foundryv1.UpdateFoundryAIRequest_Provider {
		return &foundryv1.UpdateFoundryAIRequest_Provider{Provider: &foundryv1.FoundryAIProvider{
			Kind: &foundryv1.FoundryAIProvider_Bedrock{Bedrock: &foundryv1.FoundryAIBedrock{Region: region, Fips: fips}},
		}}
	}
	suggested, ok := ai.Suggested(ai.KindBedrock)
	if !ok {
		t.Fatal("Bedrock suggests no models")
	}
	// base is the suggested draft model's ID without its inference profile's
	// prefix: the model the catalog prices.
	_, base, _ := strings.Cut(suggested.Draft.Name, ".")
	saved := func(t *testing.T, s *Service, c *foundrymodel.FoundryAIConfig) (ai.Settings, string) {
		t.Helper()
		settings, err := ai.DecodeSettings(c.Settings)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ai.OpenKey(s.deps.ConnectorKeys, c)
		if err != nil {
			t.Fatal(err)
		}
		return settings, key.Reveal()
	}

	// The page offers Bedrock with a key and Claude's models on it.
	t.Run("offers Bedrock with Claude suggested", func(t *testing.T) {
		s, _ := aiService(t, cfg(modeCloud), "pro")
		out, err := s.GetFoundryAI(ctx, &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		offered := out.GetAi().GetOffered()
		if len(offered) != 2 || offered[1].GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_BEDROCK ||
			offered[1].GetKey() != foundryv1.FoundryAIKey_FOUNDRY_AI_KEY_REQUIRED ||
			offered[1].GetSuggested().GetDraft().GetName() != suggested.Draft.Name || offered[1].GetSuggested().GetDraft().GetTable() == nil ||
			offered[1].GetSuggested().GetExtract().GetName() != suggested.Extract.Name {
			t.Fatalf("offered = %v", offered)
		}
	})

	// A Bedrock key is sealed under its row with its hint; the region is
	// stored as given, trimmed, and shown to everyone, since the page says
	// where the workspace's data goes. Models left out are the suggestions.
	t.Run("saves a Bedrock key and its region", func(t *testing.T) {
		s, store := aiService(t, cfg(modeCloud), "pro")
		out, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1", Choice: bedrock(" eu-west-1 ", false), ApiKey: bedrockKey})
		if err != nil {
			t.Fatal(err)
		}
		c := store.saved[0]
		settings, key := saved(t, s, c)
		if c.Kind != "bedrock" || *c.SecretHint != "…MTIz" || key != bedrockKey ||
			settings.Provider != (ai.Bedrock{Region: "eu-west-1"}) || settings.Models != suggested {
			t.Fatalf("saved %+v: %+v", c, settings)
		}
		pending := out.GetAi().GetPending()
		if pending.GetLabel() != "Amazon Bedrock" || pending.GetKind() != foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_BEDROCK ||
			pending.GetProvider().GetBedrock().GetRegion() != "eu-west-1" || pending.GetProvider().GetBedrock().GetFips() || pending.GetKeyHint() != "…MTIz" {
			t.Fatalf("pending = %v", pending)
		}
		raw, _ := protojson.Marshal(out)
		if strings.Contains(string(raw), "sentinel") || strings.Contains(string(c.Settings), "sentinel") {
			t.Fatalf("the key leaked: %s", raw)
		}
		store.view.Active, store.view.Pending = c, nil
		member, err := s.GetFoundryAI(ports.WithActor(ctx, ports.Actor{Source: ports.SourceUser, UserID: "user-member"}), &foundryv1.GetFoundryAIRequest{WorkspaceId: "ws-1"})
		if active := member.GetAi().GetActive(); err != nil || active.GetProvider().GetBedrock().GetRegion() != "eu-west-1" ||
			active.GetLabel() != "Amazon Bedrock" || active.GetKeyHint() != "" || !active.GetHasKey() {
			t.Fatalf("a member's view = %v, %v", active, err)
		}
	})

	// FIPS and every price form: an inference profile by the table, priced
	// as its model; another profile priced as the model it routes to; a
	// model the table lacks at the owner's rates; an application inference
	// profile's ARN priced as its model.
	t.Run("saves FIPS and inference profiles priced as their model", func(t *testing.T) {
		s, store := aiService(t, cfg(modeCloud), "max")
		arn := "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/a1b2c3d4e5f6"
		models := &foundryv1.FoundryAIModels{
			Draft: &foundryv1.FoundryAIModel{Name: "us." + base, Price: &foundryv1.FoundryAIModel_Table{Table: &emptypb.Empty{}}},
			Ask:   &foundryv1.FoundryAIModel{Name: "us." + base, Price: &foundryv1.FoundryAIModel_TableAs{TableAs: base}},
			Extract: &foundryv1.FoundryAIModel{Name: "amazon.nova-lite-v1:0", Price: &foundryv1.FoundryAIModel_Rates{Rates: &foundryv1.FoundryAIRates{
				InputUsdPerMtok: "0.06", OutputUsdPerMtok: "0.24", CachedInputUsdPerMtok: "0.015"}}},
			Verify: &foundryv1.FoundryAIModel{Name: arn, Price: &foundryv1.FoundryAIModel_TableAs{TableAs: "us." + base}},
		}
		out, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1", Choice: bedrock("us-east-1", true),
			ApiKey: bedrockKey, Models: models})
		if err != nil {
			t.Fatal(err)
		}
		settings, _ := saved(t, s, store.saved[0])
		m := settings.Models
		rates, _ := m.Extract.Price.(ai.PriceRates)
		if settings.Provider != (ai.Bedrock{Region: "us-east-1", FIPS: true}) || m.Draft != (ai.Model{Name: "us." + base, Price: ai.PriceTable{}}) ||
			m.Ask != (ai.Model{Name: "us." + base, Price: ai.PriceTableAs{Model: base}}) || m.Verify != (ai.Model{Name: arn, Price: ai.PriceTableAs{Model: "us." + base}}) ||
			m.Extract.Name != "amazon.nova-lite-v1:0" || rates.Input != 60 || rates.Output != 240 || rates.CacheRead == nil || *rates.CacheRead != 15 {
			t.Fatalf("saved %+v", settings)
		}
		if p := out.GetAi().GetPending(); !p.GetProvider().GetBedrock().GetFips() || p.GetModels().GetExtract().GetRates().GetOutputUsdPerMtok() != "0.24" {
			t.Fatalf("pending = %v", p)
		}
	})

	// A Bedrock API key belongs to the AWS account, whatever the region,
	// endpoint or models: a blank key keeps it among Bedrock saves, and only
	// there.
	t.Run("keeps a key across regions and models", func(t *testing.T) {
		s, store := aiService(t, cfg(modeCloud), "pro")
		save := func(choice *foundryv1.UpdateFoundryAIRequest_Provider, models *foundryv1.FoundryAIModels, key string) error {
			_, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1", Choice: choice, ApiKey: key, Models: models})
			if err == nil {
				store.view.Active = store.saved[len(store.saved)-1]
			}
			return err
		}
		if err := save(bedrock("us-east-1", false), nil, bedrockKey); err != nil {
			t.Fatal(err)
		}
		for name, tc := range map[string]struct {
			choice *foundryv1.UpdateFoundryAIRequest_Provider
			models *foundryv1.FoundryAIModels
		}{
			"another region":  {bedrock("eu-central-1", false), nil},
			"its FIPS host":   {bedrock("us-east-1", true), nil},
			"another model":   {bedrock("us-east-1", false), &foundryv1.FoundryAIModels{Draft: &foundryv1.FoundryAIModel{Name: "eu." + base}}},
			"the same choice": {bedrock("us-east-1", false), nil},
		} {
			if err := save(tc.choice, tc.models, ""); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if _, key := saved(t, s, store.view.Active); key != bedrockKey || *store.view.Active.SecretHint != "…MTIz" {
				t.Fatalf("%s kept %q", name, key)
			}
		}
		if err := save(claudeChoice(), nil, ""); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "Enter the API key for this endpoint.") {
			t.Fatalf("Claude on Bedrock's key: %v", err)
		}
		store.view.Active = savedClaude(t, s.deps.ConnectorKeys, "cfg-claude", ownKey)
		if err := save(bedrock("us-east-1", false), nil, ""); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "Enter the API key for this endpoint.") {
			t.Fatalf("Bedrock on Claude's key: %v", err)
		}
	})

	// A short-term key is bedrock-api-key- and the base64 of a presigned
	// URL, as AWS's token generator makes it. One made from an IAM user's
	// keys fits the agent's 512 bytes and saves; one made from temporary
	// credentials carries their session token, is far longer, and is
	// refused with what to use instead, before anything is stored.
	t.Run("takes a short-term key that fits, and says to use a long-term one", func(t *testing.T) {
		shortTerm := func(session string) string {
			url := "bedrock.amazonaws.com/?Action=CallWithBearerToken&X-Amz-Algorithm=AWS4-HMAC-SHA256" +
				"&X-Amz-Credential=AKIASENTINEL7EXAMPLE%2F20261024%2Fap-southeast-2%2Fbedrock%2Faws4_request&X-Amz-Date=20261024T120000Z&X-Amz-Expires=43200"
			if session != "" {
				url += "&X-Amz-Security-Token=" + session
			}
			url += "&X-Amz-SignedHeaders=host&X-Amz-Signature=" + strings.Repeat("5e", 32) + "&Version=1"
			return "bedrock-api-key-" + base64.StdEncoding.EncodeToString([]byte(url))
		}
		user, assumed := shortTerm(""), shortTerm(strings.Repeat("IQoJb3JpZ2luX2VjEPT%2F%2F", 40))
		if len(user) > ai.MaxKeyBytes || len(assumed) <= 1000 {
			t.Fatalf("an IAM user's key is %d bytes, an assumed role's %d", len(user), len(assumed))
		}
		s, store := aiService(t, cfg(modeCloud), "pro")
		if _, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1", Choice: bedrock("ap-southeast-2", false), ApiKey: user}); err != nil {
			t.Fatal(err)
		}
		if _, key := saved(t, s, store.saved[0]); key != user {
			t.Fatalf("saved %q", key)
		}
		_, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1", Choice: bedrock("ap-southeast-2", false), ApiKey: assumed})
		if msg := status.Convert(err).Message(); status.Code(err) != codes.InvalidArgument ||
			msg != "the API key must be at most 512 characters; a short-term Bedrock key made from temporary AWS credentials is longer, so use a long-term Bedrock API key" {
			t.Fatalf("an assumed role's key: %v", err)
		}
		if strings.Contains(err.Error(), assumed[len(assumed)-24:]) || len(store.saved) != 1 {
			t.Fatalf("an over-long key was echoed or stored: %v, %d saved", err, len(store.saved))
		}
	})

	// What it refuses, and why: before anything is stored.
	for name, tc := range map[string]struct {
		plan   string
		kinds  []string
		req    *foundryv1.UpdateFoundryAIRequest
		code   codes.Code
		reason string
		says   string
	}{
		"Bedrock on Free": {"free", nil, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("us-east-1", false), ApiKey: bedrockKey},
			codes.FailedPrecondition, "FOUNDRY_PRO_REQUIRED", "needs Foundry Pro"},
		"Bedrock not offered": {"pro", []string{"anthropic"}, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("us-east-1", false), ApiKey: bedrockKey},
			codes.InvalidArgument, "", "Amazon Bedrock is not offered here"},
		"no region": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("", false), ApiKey: bedrockKey},
			codes.InvalidArgument, "", "provider.region must be an AWS region"},
		"a region that is a host": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("us-east-1.sentinel.example", false), ApiKey: bedrockKey},
			codes.InvalidArgument, "", "provider.region must be an AWS region"},
		"an upper-case region": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("US-EAST-1", false), ApiKey: bedrockKey},
			codes.InvalidArgument, "", "provider.region must be an AWS region"},
		"an availability zone": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("us-east-1a", false), ApiKey: bedrockKey},
			codes.InvalidArgument, "", "provider.region must be an AWS region"},
		"no key": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("us-east-1", false)},
			codes.InvalidArgument, "", "Enter the API key for this endpoint."},
		"an unpriced model": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("us-east-1", false), ApiKey: bedrockKey, Models: &foundryv1.FoundryAIModels{
			Ask: &foundryv1.FoundryAIModel{Name: "us.anthropic.claude-sentinel-next"}}},
			codes.FailedPrecondition, "FOUNDRY_AI_PRICE_REQUIRED", "no price for the ask model"},
		"a profile priced as an unpriced model": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("us-east-1", false), ApiKey: bedrockKey,
			Models: &foundryv1.FoundryAIModels{Verify: &foundryv1.FoundryAIModel{Name: "us.meta.llama-sentinel", Price: &foundryv1.FoundryAIModel_TableAs{TableAs: "meta.llama-sentinel"}}}},
			codes.FailedPrecondition, "FOUNDRY_AI_PRICE_REQUIRED", "no price for the verify model"},
		"a GovCloud profile by the table": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("us-gov-west-1", false), ApiKey: bedrockKey,
			Models: &foundryv1.FoundryAIModels{Extract: &foundryv1.FoundryAIModel{Name: "us-gov." + base}}},
			codes.FailedPrecondition, "FOUNDRY_AI_PRICE_REQUIRED", "no price for the extract model"},
		"a Bedrock model priced free": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("us-east-1", false), ApiKey: bedrockKey,
			Models: &foundryv1.FoundryAIModels{Draft: &foundryv1.FoundryAIModel{Name: base, Price: &foundryv1.FoundryAIModel_Free{Free: &emptypb.Empty{}}}}},
			codes.InvalidArgument, "", "models.draft.price can be free only"},
		"a long-term key as a model": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("us-east-1", false), ApiKey: "ABSKother-0123456789abcdef",
			Models: &foundryv1.FoundryAIModels{Draft: &foundryv1.FoundryAIModel{Name: bedrockKey}}},
			codes.InvalidArgument, "", "models.draft.name looks like an API key"},
		"a short-term key as a model": {"pro", nil, &foundryv1.UpdateFoundryAIRequest{Choice: bedrock("us-east-1", false), ApiKey: bedrockKey,
			Models: &foundryv1.FoundryAIModels{Ask: &foundryv1.FoundryAIModel{Name: "bedrock-api-key-sentinelYmVkcm9jaw"}}},
			codes.InvalidArgument, "", "models.ask.name looks like an API key"},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			c := cfg(modeCloud)
			if tc.kinds != nil {
				c.Foundry.AI.Kinds = tc.kinds
			}
			s, store := aiService(t, c, tc.plan)
			tc.req.WorkspaceId = "ws-1"
			_, err := s.UpdateFoundryAI(ctx, tc.req)
			if status.Code(err) != tc.code || reason(err) != tc.reason || !strings.Contains(status.Convert(err).Message(), tc.says) {
				t.Fatalf("UpdateFoundryAI() = %v (reason %q), want %v %q saying %q", err, reason(err), tc.code, tc.reason, tc.says)
			}
			if strings.Contains(status.Convert(err).Message(), "sentinel") {
				t.Fatalf("the error names the key, region or model: %v", err)
			}
			if len(store.saved) > 0 {
				t.Fatalf("a refused save was stored: %+v", store.saved)
			}
		})
	}

	// A local install offers Bedrock with no plan.
	t.Run("lets a local owner bring Bedrock", func(t *testing.T) {
		s, store := aiService(t, cfg(modeLocal), "")
		if _, err := s.UpdateFoundryAI(ctx, &foundryv1.UpdateFoundryAIRequest{WorkspaceId: "ws-1", Choice: bedrock("ap-southeast-2", false), ApiKey: bedrockKey}); err != nil {
			t.Fatal(err)
		}
		if len(store.saved) != 1 || store.saved[0].Kind != "bedrock" {
			t.Fatalf("saved = %+v", store.saved)
		}
	})
}
