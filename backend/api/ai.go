package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// A workspace's model provider (Settings › AI models). Everyone reads where
// the workspace's model calls go; owners change it. A save is tested by the
// foundry-ask-worker's probe before it is used, so the setting in force
// stays until the test passes; going back to the default is immediate.
// Hosted, bringing a key needs Foundry Pro or Max; a local install sells no
// plan and has no Helix key, so there it is always allowed.

// foundryDomain names the refusals Foundry's site routes by reason.
const foundryDomain = "foundry.helix-db.com"

// foundryPrecondition is a refusal the owner can act on, named by reason.
func foundryPrecondition(workspaceID, reason, msg string) error {
	st := status.New(codes.FailedPrecondition, msg)
	detail, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: reason, Domain: foundryDomain, Metadata: map[string]string{"workspace_id": workspaceID},
	})
	if err != nil {
		return st.Err()
	}
	return detail.Err()
}

// requireAI refuses model work the workers would refuse: no provider (only a
// local install without a default), or a hosted workspace's own provider off
// Pro and Max. It opens no key.
func (s *Service) requireAI(ctx context.Context, workspaceID string) error {
	err := (&ai.Resolver{DB: s.deps.DB, Deployment: s.deps.AI, Plans: s.deps.AIPlans}).Usable(ctx, workspaceID)
	switch {
	case errors.Is(err, ai.ErrNotConfigured):
		return foundryPrecondition(workspaceID, "FOUNDRY_AI_NOT_CONFIGURED", ai.ResolveFailure(err).Message)
	case errors.Is(err, ai.ErrProRequired):
		return billingPrecondition(workspaceID, "FOUNDRY_PRO_REQUIRED", "Bringing your own model key needs Foundry Pro")
	case err != nil:
		return status.Errorf(codes.Unavailable, "read the AI setting: %v", err)
	}
	return nil
}

// mayManageAI reports whether the caller may change ws's AI setting, so may
// see its keys' hints. Any doubt hides them; the page still loads.
func (s *Service) mayManageAI(ctx context.Context, workspaceID string) bool {
	return s.deps.People != nil && s.deps.People.MayManageAI(ctx, workspaceID, actor(ctx).UserID)
}

// GetFoundryAI returns where the workspace's model calls go. Authz has
// checked the caller may read the workspace.
func (s *Service) GetFoundryAI(ctx context.Context, req *foundryv1.GetFoundryAIRequest) (*foundryv1.FoundryAIResponse, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	return s.foundryAIResponse(ctx, req.GetWorkspaceId(), s.mayManageAI(ctx, req.GetWorkspaceId()))
}

// UpdateFoundryAI records an owner's change. use_default makes the
// deployment's default the setting at once. A provider is checked in this
// order, then queued for its probe:
//  1. its kind is one foundry.ai.kinds offers, and, hosted, the workspace
//     is on a plan with own keys (FOUNDRY_PRO_REQUIRED);
//  2. its fields, under the deployment's URL rules, model names and prices;
//  3. unless private hosts are allowed, its hosts resolve to public addresses;
//  4. its key is given, kept from the setting where it still belongs to the
//     endpoint (ai.CanReuseKey), or not needed;
//  5. every model priced by table is in the agent's catalog
//     (FOUNDRY_AI_PRICE_REQUIRED);
//  6. a key to seal needs the keyring (UNAVAILABLE);
//  7. the workspace has saves left this hour (RESOURCE_EXHAUSTED).
//
// Authz has checked the caller is an owner. The key reaches no log, error
// or row but the sealed column.
func (s *Service) UpdateFoundryAI(ctx context.Context, req *foundryv1.UpdateFoundryAIRequest) (*foundryv1.FoundryAIResponse, error) {
	ws, userID, d := req.GetWorkspaceId(), actor(ctx).UserID, s.deps.AI
	switch {
	case ws == "":
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	case userID == "":
		return nil, status.Error(codes.PermissionDenied, "only a signed-in owner can change the AI settings")
	}
	var choice *foundryv1.FoundryAIProvider
	switch c := req.GetChoice().(type) {
	case *foundryv1.UpdateFoundryAIRequest_UseDefault:
		switch {
		case d.Current() == nil:
			return nil, status.Error(codes.FailedPrecondition, "this install has no default AI provider; choose one")
		case req.GetModels() != nil || req.GetApiKey() != "":
			return nil, status.Error(codes.InvalidArgument, "use_default takes no models or key")
		}
		switch err := s.deps.DB.ActivateFoundryAIDefault(ctx, ws, userID); {
		case errors.Is(err, foundrystore.ErrNotFound):
			return nil, status.Error(codes.FailedPrecondition, "the workspace is being deleted")
		case err != nil:
			return nil, status.Errorf(codes.Internal, "use the default AI provider: %v", err)
		}
		return s.foundryAIResponse(ctx, ws, true)
	case *foundryv1.UpdateFoundryAIRequest_Provider:
		choice = c.Provider
	default:
		return nil, status.Error(codes.InvalidArgument, "choose use_default or a provider")
	}

	p, err := providerFromProto(choice)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	kind := p.Kind()
	if !d.Offers(kind) {
		return nil, status.Errorf(codes.InvalidArgument, "%s is not offered here", ai.Label(p))
	}
	if !d.Local {
		plan, err := s.workspacePlan(ctx, ws)
		if err != nil {
			return nil, err
		}
		if !plan.Sold() {
			return nil, billingPrecondition(ws, "FOUNDRY_PRO_REQUIRED", "Bringing your own model key needs Foundry Pro")
		}
	}
	models, err := modelsFromProto(kind, req.GetModels())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	settings := ai.Settings{Provider: p, Models: models}
	if err := settings.Validate(d.Rules); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	settings = settings.Normalized(d.Rules)
	if !d.Rules.AllowPrivate {
		if err := settings.CheckHosts(ctx, connector.Guard{Resolver: s.deps.AIResolver}); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}

	key, kept, err := s.foundryAIKey(ctx, ws, settings, req.GetApiKey())
	if err != nil {
		return nil, err
	}
	// Refusals name the role, never the model: a key pasted into a model's
	// name is not echoed back.
	if i := slices.IndexFunc(settings.Models.Each(), func(rm ai.RoleModel) bool { return !key.IsZero() && rm.Model.Name == key.Reveal() }); i >= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "models.%s.name is the API key; enter the model's name", settings.Models.Each()[i].Role)
	}
	if unpriced := d.Catalog.Unpriced(settings); len(unpriced) > 0 {
		return nil, foundryPrecondition(ws, "FOUNDRY_AI_PRICE_REQUIRED", fmt.Sprintf("Helix has no price for the %s model; enter its prices", unpriced[0]))
	}
	c := &foundrymodel.FoundryAIConfig{ID: id.New(), WorkspaceID: ws, Kind: string(kind), CreatedBy: userID}
	if !key.IsZero() || kept != nil {
		keys := s.deps.ConnectorKeys
		if keys == nil {
			return nil, status.Error(codes.Unavailable, "API keys can't be stored here: the keyring is not configured")
		}
		hint := ai.Hint(key)
		c.SecretHint = &hint
		if kept != nil {
			// Opened under the setting's row, sealed under the new one's.
			if key, err = ai.OpenKey(keys, kept); err != nil {
				return nil, status.Error(codes.InvalidArgument, "Enter the API key for this endpoint.")
			}
			c.SecretHint = kept.SecretHint
		}
		if c.Secret, err = ai.SealKey(keys, c.ID, key); err != nil {
			return nil, status.Errorf(codes.Internal, "seal the API key: %v", err)
		}
	}
	if c.Settings, err = json.Marshal(settings); err != nil {
		return nil, status.Errorf(codes.Internal, "encode the AI settings: %v", err)
	}
	perHour := s.deps.Cfg.Foundry.AI.Probe.PerHour
	switch err := s.deps.DB.CreateFoundryAIConfig(ctx, c, perHour); {
	case errors.Is(err, foundrystore.ErrLimitExceeded):
		return nil, status.Errorf(codes.ResourceExhausted, "a workspace may save its AI settings %d times an hour; try again later", perHour)
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.FailedPrecondition, "the workspace is being deleted")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "save the AI settings: %v", err)
	}
	return s.foundryAIResponse(ctx, ws, true)
}

// DetectFoundryAIDefault looks again for a local install's default on its
// machine (package ai/detect), so a model pulled or a server started since
// is used without a restart, and answers the workspace's AI settings. Only
// a local install that names no default provider looks. Authz has checked
// the caller may read the workspace: locally, the owner.
func (s *Service) DetectFoundryAIDefault(ctx context.Context, req *foundryv1.DetectFoundryAIDefaultRequest) (*foundryv1.FoundryAIResponse, error) {
	ws := req.GetWorkspaceId()
	switch {
	case ws == "":
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	case s.deps.LocalAI == nil || s.deps.AI.Detected == nil:
		return nil, status.Error(codes.FailedPrecondition, "only a local install that names no AI provider looks for one")
	}
	s.deps.LocalAI.Detect(ctx)
	return s.foundryAIResponse(ctx, ws, s.mayManageAI(ctx, ws))
}

// ListFoundryOllamaModels reads the models of the Ollama server at
// base_url, for the Ollama card: a local install's only. The address is
// checked as a save checks it; a server that doesn't answer, isn't Ollama
// or is too old says so in the answer, not as an error.
func (s *Service) ListFoundryOllamaModels(ctx context.Context, req *foundryv1.ListFoundryOllamaModelsRequest) (*foundryv1.ListFoundryOllamaModelsResponse, error) {
	switch {
	case req.GetWorkspaceId() == "":
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	case s.deps.LocalAI == nil || !s.deps.AI.Offers(ai.KindOllama):
		return nil, status.Error(codes.FailedPrecondition, "only a local install reads an Ollama server's models")
	}
	base, err := ai.OllamaURL(strings.TrimSpace(req.GetBaseUrl()), s.deps.AI.Rules)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	found := s.deps.LocalAI.Ollama(ctx, base)
	out := &foundryv1.FoundryOllamaServer{Version: found.Version, Problem: found.Problem}
	for _, m := range found.Models {
		out.Models = append(out.Models, &foundryv1.FoundryOllamaModel{
			Name: m.Name, SizeBytes: m.Size, ParameterSize: m.ParameterSize, Unusable: m.Unusable,
		})
	}
	return &foundryv1.ListFoundryOllamaModelsResponse{Server: out}, nil
}

// foundryAIKey is the key a save carries: the one given, or, when none is
// and it still belongs to the endpoint, the setting's, whose config it
// returns as kept. A kind that takes no key refuses one; one that needs a
// key and has neither is refused.
func (s *Service) foundryAIKey(ctx context.Context, workspaceID string, next ai.Settings, given string) (
	key ontologyagent.ModelKey, kept *foundrymodel.FoundryAIConfig, err error) {
	key = ontologyagent.NewModelKey(strings.TrimSpace(given))
	rule := next.Provider.Kind().Key()
	switch {
	case rule == ai.KeyRefused && !key.IsZero():
		return key, nil, status.Errorf(codes.InvalidArgument, "%s takes no API key", ai.Label(next.Provider))
	case len(key.Reveal()) > ai.MaxKeyBytes && next.Provider.Kind() == ai.KindBedrock:
		return ontologyagent.ModelKey{}, nil, status.Errorf(codes.InvalidArgument,
			"the API key must be at most %d characters; a short-term Bedrock key made from temporary AWS credentials is longer, so use a long-term Bedrock API key", ai.MaxKeyBytes)
	case len(key.Reveal()) > ai.MaxKeyBytes || strings.ContainsFunc(key.Reveal(), func(r rune) bool { return r > unicode.MaxASCII || !unicode.IsPrint(r) }):
		return ontologyagent.ModelKey{}, nil, status.Errorf(codes.InvalidArgument, "the API key must be at most %d printable characters", ai.MaxKeyBytes)
	case !key.IsZero() || rule == ai.KeyRefused:
		return key, nil, nil
	}
	active, err := s.deps.DB.GetActiveFoundryAIConfig(ctx, workspaceID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
	case err != nil:
		return key, nil, status.Errorf(codes.Internal, "read the AI setting: %v", err)
	case active.Secret != nil:
		if settings, err := ai.DecodeSettings(active.Settings); err == nil && ai.CanReuseKey(settings, next) {
			return key, active, nil
		}
	}
	if rule == ai.KeyRequired {
		return key, nil, status.Error(codes.InvalidArgument, "Enter the API key for this endpoint.")
	}
	return key, nil, nil
}

// foundryAIResponse reads what the AI settings show. Only pro_required
// needs the billing system, read through AIPlans, which keeps a sold plan a minute for
// the page's polling: when it can't say, the page still loads, and a save
// decides.
// foundryAIResponse is ws's AI settings; hints shows the keys' hints, for
// those who may change them.
func (s *Service) foundryAIResponse(ctx context.Context, workspaceID string, hints bool) (*foundryv1.FoundryAIResponse, error) {
	d := s.deps.AI
	view, err := s.deps.DB.GetFoundryAIView(ctx, workspaceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the AI settings: %v", err)
	}
	out := &foundryv1.FoundryAI{PrivateHostsAllowed: d.Rules.AllowPrivate}
	for _, slot := range []struct {
		from *foundrymodel.FoundryAIConfig
		to   **foundryv1.FoundryAIConfig
	}{{view.Active, &out.Active}, {view.Pending, &out.Pending}, {view.LastFailed, &out.LastFailed}} {
		if slot.from == nil {
			continue
		}
		if *slot.to, err = s.foundryAIConfigProto(ctx, slot.from, hints); err != nil {
			return nil, err
		}
	}
	if def := d.Current(); def != nil {
		out.Default = &foundryv1.FoundryAIDefault{
			Kind: kindToProto[def.Settings.Provider.Kind()], Models: modelsToProto(def.Settings.Models), Label: ai.Label(def.Settings.Provider),
		}
	}
	if d.Detected != nil {
		found := d.Detected.Load()
		out.Detected = &foundryv1.FoundryAIDetected{Summary: found.Summary}
		if !found.At.IsZero() {
			out.Detected.DetectedAt = timestamppb.New(found.At)
		}
	}
	for _, kind := range d.Kinds {
		if !d.Offers(kind) {
			continue
		}
		offer := &foundryv1.FoundryAIOffer{Kind: kindToProto[kind], Key: keyRuleToProto[kind.Key()]}
		if suggested, ok := ai.Suggested(kind); ok {
			offer.Suggested = modelsToProto(suggested)
		}
		out.Offered = append(out.Offered, offer)
	}
	if !d.Local {
		switch sold, err := s.deps.AIPlans.Sold(ctx, workspaceID); {
		case err != nil:
			log.From(ctx).Warn("ai settings: foundry plan unreadable; pro_required left unset", "workspace", workspaceID, "err", err)
		default:
			out.ProRequired = !sold
		}
	}
	return &foundryv1.FoundryAIResponse{Ai: out}, nil
}

func (s *Service) foundryAIConfigProto(ctx context.Context, c *foundrymodel.FoundryAIConfig, hints bool) (*foundryv1.FoundryAIConfig, error) {
	settings, err := ai.DecodeSettings(c.Settings)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the AI config %s: %v", c.ID, err)
	}
	out := &foundryv1.FoundryAIConfig{
		Id: c.ID, Kind: kindToProto[settings.Provider.Kind()], Provider: providerToProto(settings.Provider),
		Models: modelsToProto(settings.Models), HasKey: c.Secret != nil, ProbeState: probeStateToProto[c.ProbeState],
		CreatedAt: timestamppb.New(c.CreatedAt), Label: ai.Label(settings.Provider),
	}
	if c.SecretHint != nil && hints {
		out.KeyHint = *c.SecretHint
	}
	if c.ProbeFailure != nil {
		out.ProbeFailure = *c.ProbeFailure
	}
	if c.ProbeDetail != nil {
		out.ProbeDetail = *c.ProbeDetail
	}
	if s.deps.People != nil {
		name, err := s.deps.People.DisplayName(ctx, c.CreatedBy)
		if err != nil {
			log.From(ctx).Warn("ai settings: saver unreadable", "config", c.ID, "err", err)
		}
		out.CreatedByName = name
	}
	return out, nil
}

var kindToProto = map[ai.Kind]foundryv1.FoundryAIKind{
	ai.KindAnthropic:   foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_ANTHROPIC,
	ai.KindOpenAI:      foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OPENAI,
	ai.KindAzureOpenAI: foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_AZURE_OPENAI,
	ai.KindBedrock:     foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_BEDROCK,
	ai.KindOllama:      foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OLLAMA,
	ai.KindCompatible:  foundryv1.FoundryAIKind_FOUNDRY_AI_KIND_OPENAI_COMPATIBLE,
}

var keyRuleToProto = map[ai.KeyRule]foundryv1.FoundryAIKey{
	ai.KeyRequired: foundryv1.FoundryAIKey_FOUNDRY_AI_KEY_REQUIRED,
	ai.KeyOptional: foundryv1.FoundryAIKey_FOUNDRY_AI_KEY_OPTIONAL,
	ai.KeyRefused:  foundryv1.FoundryAIKey_FOUNDRY_AI_KEY_NONE,
}

var probeStateToProto = map[foundrymodel.FoundryAIProbeState]foundryv1.FoundryAIProbeState{
	foundrymodel.FoundryAIProbeQueued:     foundryv1.FoundryAIProbeState_FOUNDRY_AI_PROBE_STATE_QUEUED,
	foundrymodel.FoundryAIProbeRunning:    foundryv1.FoundryAIProbeState_FOUNDRY_AI_PROBE_STATE_RUNNING,
	foundrymodel.FoundryAIProbePassed:     foundryv1.FoundryAIProbeState_FOUNDRY_AI_PROBE_STATE_PASSED,
	foundrymodel.FoundryAIProbeFailed:     foundryv1.FoundryAIProbeState_FOUNDRY_AI_PROBE_STATE_FAILED,
	foundrymodel.FoundryAIProbeSuperseded: foundryv1.FoundryAIProbeState_FOUNDRY_AI_PROBE_STATE_SUPERSEDED,
}

var compatAPIFromProto = map[foundryv1.FoundryAICompatAPI]ai.CompatAPI{
	foundryv1.FoundryAICompatAPI_FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS: ai.CompatChatCompletions,
	foundryv1.FoundryAICompatAPI_FOUNDRY_AI_COMPAT_API_RESPONSES:        ai.CompatResponses,
}

// providerFromProto reads the provider an owner chose, with the defaults of
// the fields left 0.
func providerFromProto(p *foundryv1.FoundryAIProvider) (ai.Provider, error) {
	switch k := p.GetKind().(type) {
	case *foundryv1.FoundryAIProvider_Anthropic:
		return ai.Anthropic{}, nil
	case *foundryv1.FoundryAIProvider_Openai:
		return ai.OpenAI{}, nil
	case *foundryv1.FoundryAIProvider_AzureOpenai:
		return ai.AzureOpenAI{Endpoint: strings.TrimSpace(k.AzureOpenai.GetEndpoint())}, nil
	case *foundryv1.FoundryAIProvider_Bedrock:
		return ai.Bedrock{Region: strings.TrimSpace(k.Bedrock.GetRegion()), FIPS: k.Bedrock.GetFips()}, nil
	case *foundryv1.FoundryAIProvider_Ollama:
		window := k.Ollama.GetContextTokens()
		if window == 0 {
			window = ai.DefaultContextTokens
		}
		return ai.Ollama{BaseURL: strings.TrimSpace(k.Ollama.GetBaseUrl()), ContextTokens: window}, nil
	case *foundryv1.FoundryAIProvider_OpenaiCompatible:
		c := k.OpenaiCompatible
		api, ok := compatAPIFromProto[c.GetApi()]
		if !ok {
			return nil, errors.New("provider.api must be chat_completions or responses")
		}
		calls := c.GetMaxCallsInFlight()
		switch {
		case calls == 0:
			calls = ai.DefaultCallsInFlight
		case calls > ai.MaxCallsInFlightCap:
			return nil, fmt.Errorf("provider.max_calls_in_flight must be 1 to %d", ai.MaxCallsInFlightCap)
		}
		return ai.Compatible{BaseURL: strings.TrimSpace(c.GetBaseUrl()), API: api, MaxCallsInFlight: uint16(calls)}, nil
	default:
		return nil, errors.New("provider is required")
	}
}

func providerToProto(p ai.Provider) *foundryv1.FoundryAIProvider {
	switch p := p.(type) {
	case ai.Anthropic:
		return &foundryv1.FoundryAIProvider{Kind: &foundryv1.FoundryAIProvider_Anthropic{Anthropic: &foundryv1.FoundryAIAnthropic{}}}
	case ai.OpenAI:
		return &foundryv1.FoundryAIProvider{Kind: &foundryv1.FoundryAIProvider_Openai{Openai: &foundryv1.FoundryAIOpenAI{}}}
	case ai.AzureOpenAI:
		return &foundryv1.FoundryAIProvider{Kind: &foundryv1.FoundryAIProvider_AzureOpenai{AzureOpenai: &foundryv1.FoundryAIAzureOpenAI{Endpoint: p.Endpoint}}}
	case ai.Bedrock:
		return &foundryv1.FoundryAIProvider{Kind: &foundryv1.FoundryAIProvider_Bedrock{Bedrock: &foundryv1.FoundryAIBedrock{Region: p.Region, Fips: p.FIPS}}}
	case ai.Ollama:
		return &foundryv1.FoundryAIProvider{Kind: &foundryv1.FoundryAIProvider_Ollama{Ollama: &foundryv1.FoundryAIOllama{
			BaseUrl: p.BaseURL, ContextTokens: p.ContextTokens,
		}}}
	case ai.Compatible:
		api := foundryv1.FoundryAICompatAPI_FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS
		if p.API == ai.CompatResponses {
			api = foundryv1.FoundryAICompatAPI_FOUNDRY_AI_COMPAT_API_RESPONSES
		}
		return &foundryv1.FoundryAIProvider{Kind: &foundryv1.FoundryAIProvider_OpenaiCompatible{OpenaiCompatible: &foundryv1.FoundryAICompatible{
			BaseUrl: p.BaseURL, Api: api, MaxCallsInFlight: uint32(p.MaxCallsInFlight),
		}}}
	default:
		return nil
	}
}

// modelsFromProto reads a model per task; a task left out gets the kind's
// suggestion, and a kind with none needs every one. A model on an Ollama
// server given no price is free.
func modelsFromProto(kind ai.Kind, m *foundryv1.FoundryAIModels) (ai.Roles, error) {
	suggested, ok := ai.Suggested(kind)
	var out ai.Roles
	for _, slot := range []struct {
		role ai.Role
		from *foundryv1.FoundryAIModel
		to   *ai.Model
		def  ai.Model
	}{
		{ai.RoleDraft, m.GetDraft(), &out.Draft, suggested.Draft},
		{ai.RoleAsk, m.GetAsk(), &out.Ask, suggested.Ask},
		{ai.RoleExtract, m.GetExtract(), &out.Extract, suggested.Extract},
		{ai.RoleVerify, m.GetVerify(), &out.Verify, suggested.Verify},
	} {
		if slot.from == nil {
			if !ok {
				return ai.Roles{}, fmt.Errorf("models.%s is required", slot.role)
			}
			*slot.to = slot.def
			continue
		}
		model, err := modelFromProto(slot.from)
		if err != nil {
			return ai.Roles{}, fmt.Errorf("models.%s.%w", slot.role, err)
		}
		if kind == ai.KindOllama && slot.from.GetPrice() == nil {
			model.Price = ai.PriceFree{}
		}
		*slot.to = model
	}
	return out, nil
}

func modelFromProto(m *foundryv1.FoundryAIModel) (ai.Model, error) {
	out := ai.Model{Name: strings.TrimSpace(m.GetName())}
	switch p := m.GetPrice().(type) {
	case *foundryv1.FoundryAIModel_TableAs:
		out.Price = ai.PriceTableAs{Model: strings.TrimSpace(p.TableAs)}
	case *foundryv1.FoundryAIModel_Rates:
		input, err := nanosPerToken(p.Rates.GetInputUsdPerMtok())
		if err != nil {
			return ai.Model{}, fmt.Errorf("price.rates.input_usd_per_mtok %w", err)
		}
		output, err := nanosPerToken(p.Rates.GetOutputUsdPerMtok())
		if err != nil {
			return ai.Model{}, fmt.Errorf("price.rates.output_usd_per_mtok %w", err)
		}
		rates := ai.PriceRates{Input: input, Output: output}
		if cached := p.Rates.GetCachedInputUsdPerMtok(); cached != "" {
			read, err := nanosPerToken(cached)
			if err != nil {
				return ai.Model{}, fmt.Errorf("price.rates.cached_input_usd_per_mtok %w", err)
			}
			rates.CacheRead = &read
		}
		out.Price = rates
	case *foundryv1.FoundryAIModel_Free:
		out.Price = ai.PriceFree{}
	default:
		// Table, or none given: Helix's price table.
		out.Price = ai.PriceTable{}
	}
	return out, nil
}

func modelsToProto(r ai.Roles) *foundryv1.FoundryAIModels {
	return &foundryv1.FoundryAIModels{
		Draft: modelToProto(r.Draft), Ask: modelToProto(r.Ask), Extract: modelToProto(r.Extract), Verify: modelToProto(r.Verify),
	}
}

func modelToProto(m ai.Model) *foundryv1.FoundryAIModel {
	out := &foundryv1.FoundryAIModel{Name: m.Name}
	switch p := m.Price.(type) {
	case ai.PriceTable:
		out.Price = &foundryv1.FoundryAIModel_Table{Table: &emptypb.Empty{}}
	case ai.PriceTableAs:
		out.Price = &foundryv1.FoundryAIModel_TableAs{TableAs: p.Model}
	case ai.PriceRates:
		rates := &foundryv1.FoundryAIRates{InputUsdPerMtok: usdPerMTok(p.Input), OutputUsdPerMtok: usdPerMTok(p.Output)}
		if p.CacheRead != nil {
			rates.CachedInputUsdPerMtok = usdPerMTok(*p.CacheRead)
		}
		out.Price = &foundryv1.FoundryAIModel_Rates{Rates: rates}
	case ai.PriceFree:
		out.Price = &foundryv1.FoundryAIModel_Free{Free: &emptypb.Empty{}}
	}
	return out
}

// usdPerMTokRE is a price in dollars per million tokens: at most three
// decimals, which is a whole number of nano-dollars per token.
var usdPerMTokRE = regexp.MustCompile(`^(\d{1,7})(?:\.(\d{1,3}))?$`)

// nanosPerToken reads "3.75" US dollars per million tokens as 3750
// nano-dollars per token.
func nanosPerToken(usd string) (uint64, error) {
	m := usdPerMTokRE.FindStringSubmatch(strings.TrimSpace(usd))
	if m == nil {
		return 0, errors.New("must be a price in dollars, with at most three decimals")
	}
	whole, _ := strconv.ParseUint(m[1], 10, 64)                  // at most 7 digits
	frac, _ := strconv.ParseUint(m[2]+"000"[len(m[2]):], 10, 64) // padded to 3 digits
	return whole*1000 + frac, nil
}

// usdPerMTok writes nano-dollars per token as dollars per million tokens.
func usdPerMTok(nanos uint64) string {
	s := fmt.Sprintf("%d.%03d", nanos/1000, nanos%1000)
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}
