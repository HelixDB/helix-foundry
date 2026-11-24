package api

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/companyresearch"
	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// researchStore keeps company research's rows in memory, with the
// workspace's AI setting; startErr refuses a start, finishErr a finish.
type researchStore struct {
	foundrystore.Unimplemented
	mu        sync.Mutex
	rows      map[string]*foundrymodel.FoundryCompanyResearch
	limits    []foundrymodel.FoundryCompanyResearchLimits
	startErr  error
	finishErr error
	active    *foundrymodel.FoundryAIConfig
}

func (s *researchStore) GetActiveFoundryAIConfig(context.Context, string) (*foundrymodel.FoundryAIConfig, error) {
	if s.active == nil {
		return nil, foundrystore.ErrNotFound
	}
	return s.active, nil
}

func (s *researchStore) StartFoundryCompanyResearch(_ context.Context, r *foundrymodel.FoundryCompanyResearch, limits foundrymodel.FoundryCompanyResearchLimits) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limits = append(s.limits, limits)
	if s.startErr != nil {
		return s.startErr
	}
	row := *r
	s.rows[r.ID] = &row
	return nil
}

func (s *researchStore) FinishFoundryCompanyResearch(_ context.Context, ws, id string, result json.RawMessage, usdMicros int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok || row.WorkspaceID != ws {
		return foundrystore.ErrNotFound
	}
	row.Result, row.USDMicros = result, usdMicros
	return s.finishErr
}

func (s *researchStore) GetFoundryCompanyResearch(_ context.Context, ws, id string) (*foundrymodel.FoundryCompanyResearch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok || row.WorkspaceID != ws || row.Kind != foundrymodel.FoundryCompanyResearchOfSite || row.Result == nil {
		return nil, foundrystore.ErrNotFound
	}
	out := *row
	return &out, nil
}

// ofKind is the store's rows of kind.
func (s *researchStore) ofKind(kind foundrymodel.FoundryCompanyResearchKind) []*foundrymodel.FoundryCompanyResearch {
	out := []*foundrymodel.FoundryCompanyResearch{}
	for _, r := range s.rows {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// fakeSites reads one page of any domain it holds.
type fakeSites map[string]companyresearch.Site

func (f fakeSites) Fetch(_ context.Context, domain string) companyresearch.Site {
	if site, ok := f[domain]; ok {
		return site
	}
	return companyresearch.Site{Pages: []companyresearch.Page{}}
}

// researchAnswer is one model call's: a tool call with its input, or an
// error.
type researchAnswer struct {
	tool, input string
	err         error
}

// researchModelFake answers its calls in order, the last again once they
// run out, and records the requests.
type researchModelFake struct {
	mu       sync.Mutex
	answers  []researchAnswer
	requests []llm.Request
}

func (m *researchModelFake) Stream(_ context.Context, r llm.Request) iter.Seq2[llm.Chunk, error] {
	m.mu.Lock()
	a := m.answers[min(len(m.requests), len(m.answers)-1)]
	m.requests = append(m.requests, r)
	m.mu.Unlock()
	return func(yield func(llm.Chunk, error) bool) {
		if a.err != nil {
			yield(llm.Usage{In: 1000}, nil)
			yield(nil, a.err)
			return
		}
		for _, c := range []llm.Chunk{llm.ToolUse{ID: "t1", Name: a.tool, Input: json.RawMessage(a.input)}, llm.Usage{In: 1000, Out: 500},
			llm.Stop{Reason: llm.StopToolUse}} {
			if !yield(c, nil) {
				return
			}
		}
	}
}

const acmeResearch = `{"company_name":"Acme","summary":"Acme sells anvils to coyotes.","suggested_name":"Acme operations","questions":[
  {"question":"Who will use it?","best_guess":"Sales","options":["Sales","Support"]},
  {"question":"Which questions matter most?","best_guess":"Which orders are late","options":[]}]}`

const acmeGoal = `{"goal":"For sales.\nWhich orders are late?","name":"Acme orders"}`

// researchService is hosted Foundry, Helix's Claude the workspace's model
// at 1,000 nano-USD a token in and 2,000 out, Ada its admin and Alan a
// member, and acme.com's site one page.
func researchService(t *testing.T, answers ...researchAnswer) (*Service, *researchStore, *researchModelFake, *[]ai.Role) {
	t.Helper()
	cfg := config.Defaults()
	store := &researchStore{rows: map[string]*foundrymodel.FoundryCompanyResearch{}}
	model := &researchModelFake{answers: answers}
	roles := &[]ai.Role{}
	s := &Service{deps: &runtimeDeps{Cfg: &cfg, DB: store, AI: hostedAI(t),
		People: dashboardPeople{"user-ada": ports.RoleAdmin, "user-owner": ports.RoleOwner, "user-alan": ports.RoleMember},
		Sites:  fakeSites{"acme.com": {Pages: []companyresearch.Page{{URL: "https://acme.com/", Text: "Acme sells anvils."}}}},
		ResearchModels: func(_ ai.Resolved, _ *ai.Catalog, role ai.Role) (llm.Binding, error) {
			*roles = append(*roles, role)
			return llm.Binding{Model: model, Pricer: llm.Pricer{Input: 1000, Output: 2000}}, nil
		},
	}}
	return s, store, model, roles
}

func research(s *Service, ctx context.Context, domain string) (*foundryv1.FoundryCompanyResearch, error) {
	out, err := s.ResearchFoundryCompany(ctx, &foundryv1.ResearchFoundryCompanyRequest{WorkspaceId: "ws-1", Domain: domain})
	return out.GetResearch(), err
}

func TestResearchFoundryCompany(t *testing.T) {
	s, store, model, roles := researchService(t, researchAnswer{tool: companyresearch.ResearchTool, input: acmeResearch})
	got, err := research(s, site("user-ada"), "https://www.Acme.com/about")
	require.NoError(t, err)
	assert.Equal(t, "acme.com", got.GetDomain())
	assert.Equal(t, "Acme", got.GetCompanyName())
	assert.Equal(t, "Acme operations", got.GetSuggestedName())
	assert.Equal(t, []string{"https://acme.com/"}, got.GetSources())
	assert.True(t, got.GetFetched())
	require.Len(t, got.GetQuestions(), 2)
	assert.Equal(t, "q1", got.GetQuestions()[0].GetId())
	assert.Equal(t, []string{"Sales", "Support"}, got.GetQuestions()[0].GetOptions())
	assert.Equal(t, []ai.Role{ai.RoleExtract}, *roles, "the workspace's extraction model")

	require.Len(t, model.requests, 1)
	req := model.requests[0]
	assert.Equal(t, companyresearch.ResearchTool, req.Tools[0].Name)
	assert.Contains(t, req.Messages[0].Content[0].(llm.Text).Text, "<website>\nPage: https://acme.com/\nAcme sells anvils.")

	row := store.rows[got.GetId()]
	require.NotNil(t, row, "the research is stored under the id answered")
	assert.Equal(t, "user-ada", row.UserID)
	assert.EqualValues(t, 2000, row.USDMicros, "its call is charged: 1,000 in, 500 out")
	var stored companyresearch.Research
	require.NoError(t, json.Unmarshal(row.Result, &stored))
	assert.Equal(t, "Acme", stored.CompanyName)

	limits := store.limits[0]
	assert.Equal(t, 20, limits.PerUserPerHour)
	assert.EqualValues(t, 2_000_000, limits.MonthlyMicros)
	assert.Equal(t, 1, limits.Since.Day())
	worst := 2 * (int64(companyresearch.ResearchPromptBytes/3+1)*1000 + companyresearch.ResearchMaxOutput*2000) / 1000
	assert.Equal(t, worst, limits.WorstUSDMicros, "two calls at the largest prompt and output")

	// A site that can't be read is researched from the domain alone.
	unread, err := research(s, site("user-owner"), "initech.com")
	require.NoError(t, err)
	assert.False(t, unread.GetFetched())
	assert.Empty(t, unread.GetSources())
	assert.Contains(t, model.requests[1].Messages[0].Content[0].(llm.Text).Text, "guess from the domain alone")
}

func TestResearchFoundryCompanyRefusals(t *testing.T) {
	s, store, model, _ := researchService(t, researchAnswer{tool: companyresearch.ResearchTool, input: acmeResearch})
	for name, ctx := range map[string]context.Context{
		"a member": site("user-alan"),
		"a key":    asActor(ports.Actor{Source: ports.SourceMCP, ServiceCredentialID: "key-1", WorkspaceID: "ws-1"}),
		"no one":   context.Background(),
		"a grantee": access.WithCaller(asActor(ports.Actor{Source: ports.SourceGrantee, UserID: "user-ada", WorkspaceID: "ws-1"}),
			access.Grantee{WorkspaceID: "ws-1", Principal: access.Principal{Kind: access.PrincipalUser, ID: "user-ada"}}),
	} {
		_, err := research(s, ctx, "acme.com")
		assert.Equal(t, codes.PermissionDenied, status.Code(err), name)
	}
	_, err := research(s, throughMCP("user-ada"), "acme.com")
	require.NoError(t, err, "an admin's agent, as them")

	for _, domain := range []string{"", "gmail.com", "10.0.0.1", "acme"} {
		_, err := research(s, site("user-ada"), domain)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), domain)
	}
	_, err = s.ResearchFoundryCompany(site("user-ada"), &foundryv1.ResearchFoundryCompanyRequest{Domain: "acme.com"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "no workspace")

	// The hour, then the month, are the store's to count.
	store.startErr = foundrystore.ErrLimitExceeded
	_, err = research(s, site("user-ada"), "acme.com")
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	store.startErr = foundrystore.ErrBudgetSpent
	_, err = research(s, site("user-ada"), "acme.com")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Equal(t, "FOUNDRY_RESEARCH_BUDGET", reasonOf(err))
	store.startErr = errors.New("down")
	_, err = research(s, site("user-ada"), "acme.com")
	assert.Equal(t, codes.Unavailable, status.Code(err))
	store.startErr = nil
	assert.Len(t, model.requests, 1, "no refused call reached the model")

	// No provider, or the workspace's own key off Pro, as requireAI.
	s.deps.AI = deployment(t, aiConfig(modeLocal, true))
	_, err = research(s, site("user-ada"), "acme.com")
	assert.Equal(t, "FOUNDRY_AI_NOT_CONFIGURED", reasonOf(err))
	s.deps.AI = hostedAI(t)
	store.active = &foundrymodel.FoundryAIConfig{ID: "cfg-1", WorkspaceID: "ws-1", Kind: "anthropic"}
	_, err = research(s, site("user-ada"), "acme.com")
	assert.Equal(t, "FOUNDRY_PRO_REQUIRED", reasonOf(err))
	assert.Len(t, model.requests, 1)
}

// A model that fails is UNAVAILABLE, FOUNDRY_RESEARCH_FAILED, and what its
// calls spent is recorded, with no research to read.
func TestResearchFoundryCompanyModelFailure(t *testing.T) {
	for name, answer := range map[string]researchAnswer{
		"an unusable answer": {tool: companyresearch.ResearchTool, input: `{"company_name":"","summary":"","questions":[]}`},
		"a refusal":          {err: &llm.Error{Kind: llm.ErrAuth, Err: errors.New("bad key")}},
	} {
		t.Run(name, func(t *testing.T) {
			s, store, model, _ := researchService(t, answer)
			_, err := research(s, site("user-ada"), "acme.com")
			assert.Equal(t, codes.Unavailable, status.Code(err))
			assert.Equal(t, "FOUNDRY_RESEARCH_FAILED", reasonOf(err))
			assert.Contains(t, status.Convert(err).Message(), "Foundry couldn’t research acme.com")
			rows := store.ofKind(foundrymodel.FoundryCompanyResearchOfSite)
			require.Len(t, rows, 1)
			assert.Nil(t, rows[0].Result)
			assert.Positive(t, rows[0].USDMicros, "what was spent counts")
			assert.LessOrEqual(t, len(model.requests), researchCalls)
		})
	}

	// A finish the store refuses after a research is UNAVAILABLE: the
	// research couldn't be kept for its goal.
	s, store, _, _ := researchService(t, researchAnswer{tool: companyresearch.ResearchTool, input: acmeResearch})
	store.finishErr = errors.New("down")
	_, err := research(s, site("user-ada"), "acme.com")
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

func draft(s *Service, ctx context.Context, researchID string, answers map[string]string, data string) (*foundryv1.DraftFoundryGoalResponse, error) {
	req := &foundryv1.DraftFoundryGoalRequest{WorkspaceId: "ws-1", ResearchId: researchID, Data: data}
	for id, a := range answers {
		req.Answers = append(req.Answers, &foundryv1.FoundryResearchAnswer{QuestionId: id, Answer: a})
	}
	return s.DraftFoundryGoal(ctx, req)
}

func TestDraftFoundryGoal(t *testing.T) {
	s, store, model, _ := researchService(t, researchAnswer{tool: companyresearch.ResearchTool, input: acmeResearch},
		researchAnswer{tool: companyresearch.GoalTool, input: acmeGoal})
	r, err := research(s, site("user-ada"), "acme.com")
	require.NoError(t, err)

	got, err := draft(s, site("user-owner"), r.GetId(), map[string]string{"q1": "The sales team", "q2": ""}, "Stripe: customers (id, email)")
	require.NoError(t, err)
	assert.Equal(t, "For sales.\nWhich orders are late?", got.GetGoal())
	assert.Equal(t, "Acme orders", got.GetName())
	user := model.requests[1].Messages[0].Content[0].(llm.Text).Text
	assert.Contains(t, user, "<answers>\nQ: Who will use it?\nA: The sales team\n</answers>")
	assert.Contains(t, user, "<data>\nStripe: customers (id, email)\n</data>")
	goals := store.ofKind(foundrymodel.FoundryCompanyResearchGoal)
	require.Len(t, goals, 1)
	assert.Nil(t, goals[0].Result, "a goal row records its spend alone")
	assert.EqualValues(t, 2000, goals[0].USDMicros)
	assert.Equal(t, "acme.com", goals[0].Domain)
	assert.Equal(t, "user-owner", goals[0].UserID)

	_, err = draft(s, site("user-ada"), "nope", nil, "")
	assert.Equal(t, codes.NotFound, status.Code(err), "an unknown research")
	store.rows[r.GetId()].WorkspaceID = "ws-2"
	_, err = draft(s, site("user-ada"), r.GetId(), nil, "")
	assert.Equal(t, codes.NotFound, status.Code(err), "another workspace's")
	store.rows[r.GetId()].WorkspaceID = "ws-1"
	_, err = draft(s, site("user-ada"), "", nil, "")
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "no research named")

	for name, tc := range map[string]struct {
		answers map[string]string
		data    string
	}{
		"an unknown question": {map[string]string{"q9": "x"}, ""},
		"a long answer":       {map[string]string{"q1": strings.Repeat("a", companyresearch.MaxAnswer+1)}, ""},
		"long data":           {nil, strings.Repeat("d", companyresearch.MaxData+1)},
	} {
		_, err := draft(s, site("user-ada"), r.GetId(), tc.answers, tc.data)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), name)
	}
	_, err = draft(s, site("user-alan"), r.GetId(), nil, "")
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "a member")
	store.startErr = foundrystore.ErrLimitExceeded
	_, err = draft(s, site("user-ada"), r.GetId(), nil, "")
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	store.startErr = nil
	assert.Len(t, model.requests, 2, "no refused draft reached the model")

	// A draft the model fails is charged, and UNAVAILABLE.
	model.answers = append(model.answers, researchAnswer{tool: companyresearch.GoalTool, input: `{"goal":" ","name":"x"}`})
	_, err = draft(s, site("user-ada"), r.GetId(), nil, "")
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, "FOUNDRY_RESEARCH_FAILED", reasonOf(err))
	assert.Len(t, store.ofKind(foundrymodel.FoundryCompanyResearchGoal), 2)
}
