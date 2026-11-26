package api

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

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
// workspace's AI setting; hourErr refuses an hour's check, startErr a
// start, finishErr a finish. A start reserves its worst case, as the
// store's does; spends are the finishes' charges.
type researchStore struct {
	foundrystore.Unimplemented
	mu        sync.Mutex
	rows      map[string]*foundrymodel.FoundryCompanyResearch
	limits    []foundrymodel.FoundryCompanyResearchLimits
	spends    []foundrymodel.FoundryCompanyResearchSpend
	hourErr   error
	startErr  error
	finishErr error
	active    *foundrymodel.FoundryAIConfig
}

func (s *researchStore) CheckFoundryCompanyResearchHour(context.Context, string, int) error {
	return s.hourErr
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
	row.USDMicros = limits.WorstUSDMicros
	s.rows[r.ID] = &row
	return nil
}

func (s *researchStore) FinishFoundryCompanyResearch(_ context.Context, ws, id string, result json.RawMessage, spend foundrymodel.FoundryCompanyResearchSpend) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spends = append(s.spends, spend)
	row, ok := s.rows[id]
	switch {
	case !ok || row.WorkspaceID != ws:
		return foundrystore.ErrNotFound
	case s.finishErr != nil:
		return s.finishErr
	case spend.CutShort:
		row.Result, row.USDMicros = result, max(row.USDMicros, spend.USDMicros)
	default:
		row.Result, row.USDMicros = result, spend.USDMicros
	}
	return nil
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

// fakeSites reads one page of any domain it holds; fetched are the domains
// read.
type fakeSites struct {
	sites   map[string]companyresearch.Site
	fetched []string
}

func (f *fakeSites) Fetch(_ context.Context, domain string) companyresearch.Site {
	f.fetched = append(f.fetched, domain)
	if site, ok := f.sites[domain]; ok {
		return site
	}
	return companyresearch.Site{Pages: []companyresearch.Page{}}
}

// researchAnswer is one model call's: a tool call with its input, an
// error, or (hang) the input's usage and then nothing until its context
// ends.
type researchAnswer struct {
	tool, input string
	err         error
	hang        bool
}

// researchModelFake answers its calls in order, the last again once they
// run out, and records the requests.
type researchModelFake struct {
	mu       sync.Mutex
	answers  []researchAnswer
	requests []llm.Request
}

func (m *researchModelFake) Stream(ctx context.Context, r llm.Request) iter.Seq2[llm.Chunk, error] {
	m.mu.Lock()
	a := m.answers[min(len(m.requests), len(m.answers)-1)]
	m.requests = append(m.requests, r)
	m.mu.Unlock()
	return func(yield func(llm.Chunk, error) bool) {
		if a.hang {
			if yield(llm.Usage{In: 1000}, nil) {
				<-ctx.Done()
				yield(nil, ctx.Err())
			}
			return
		}
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
		Sites:  &fakeSites{sites: map[string]companyresearch.Site{"acme.com": {Pages: []companyresearch.Page{{URL: "https://acme.com/", Text: "Acme sells anvils."}}}}},
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
	assert.EqualValues(t, 2000, row.USDMicros, "its call is charged, its reservation replaced: 1,000 in, 500 out")
	var stored companyresearch.Research
	require.NoError(t, json.Unmarshal(row.Result, &stored))
	assert.Equal(t, "Acme", stored.CompanyName)

	limits := store.limits[0]
	assert.Equal(t, 20, limits.PerUserPerHour)
	assert.EqualValues(t, 2_000_000, limits.MonthlyMicros)
	assert.Equal(t, 1, limits.Since.Day())
	prompt := len(req.System) + len(req.Messages[0].Content[0].(llm.Text).Text) + len(companyresearch.ResearchSchema)
	worst := 2 * llm.Pricer{Input: 1000, Output: 2000}.Micros(llm.Usage{In: int64(prompt/3 + 1), Out: companyresearch.ResearchMaxOutput})
	assert.Equal(t, worst, limits.WorstUSDMicros, "two calls at the prompt read and the largest output")
	assert.Equal(t, []foundrymodel.FoundryCompanyResearchSpend{{USDMicros: 2000}}, store.spends)

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
		if name != "a member" {
			assert.Equal(t, "company research is for the workspace's owners and admins, signed in to Foundry as themselves",
				status.Convert(err).Message(), name)
		}
	}
	_, err := research(s, throughMCP("user-ada"), "acme.com")
	require.NoError(t, err, "an admin's agent, as them")

	for _, domain := range []string{"", "gmail.com", "10.0.0.1", "acme"} {
		_, err := research(s, site("user-ada"), domain)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), domain)
	}
	_, err = s.ResearchFoundryCompany(site("user-ada"), &foundryv1.ResearchFoundryCompanyRequest{Domain: "acme.com"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "no workspace")

	// The hour is checked before the site is read, and again, with the
	// month, as the call starts: the store's to count.
	sites := s.deps.Sites.(*fakeSites)
	read, started := len(sites.fetched), len(store.limits)
	store.hourErr = foundrystore.ErrLimitExceeded
	_, err = research(s, site("user-ada"), "acme.com")
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	assert.Len(t, sites.fetched, read, "a caller out of calls reads no site")
	assert.Len(t, store.limits, started, "nor starts a call")
	store.hourErr = errors.New("down")
	_, err = research(s, site("user-ada"), "acme.com")
	assert.Equal(t, codes.Unavailable, status.Code(err))
	store.hourErr = nil
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

// A model that runs out of time, or whose caller goes, is UNAVAILABLE,
// saying how long it had; its call is recorded cut short, so its row keeps
// the reservation over the input it reported.
func TestResearchFoundryCompanyCutShort(t *testing.T) {
	for name, cut := range map[string]func(context.Context) (context.Context, context.CancelFunc){
		"its deadline": func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, 50*time.Millisecond)
		},
		"its caller gone": func(ctx context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(ctx)
			time.AfterFunc(50*time.Millisecond, cancel)
			return ctx, cancel
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, store, _, _ := researchService(t, researchAnswer{hang: true})
			ctx, cancel := cut(site("user-ada"))
			defer cancel()
			_, err := research(s, ctx, "acme.com")
			assert.Equal(t, codes.Unavailable, status.Code(err))
			assert.Equal(t, "FOUNDRY_RESEARCH_FAILED", reasonOf(err))
			assert.Contains(t, status.Convert(err).Message(), "didn’t answer within 55 seconds")
			assert.Equal(t, []foundrymodel.FoundryCompanyResearchSpend{{USDMicros: 1000, CutShort: true}}, store.spends, "what it reported, cut short")
			rows := store.ofKind(foundrymodel.FoundryCompanyResearchOfSite)
			require.Len(t, rows, 1)
			assert.Equal(t, store.limits[0].WorstUSDMicros, rows[0].USDMicros, "the reservation, larger than the input reported")
			assert.Nil(t, rows[0].Result)
		})
	}

	// A goal cut short is charged as one.
	s, store, model, _ := researchService(t, researchAnswer{tool: companyresearch.ResearchTool, input: acmeResearch})
	r, err := research(s, site("user-ada"), "acme.com")
	require.NoError(t, err)
	model.answers = append(model.answers, researchAnswer{hang: true})
	ctx, cancel := context.WithTimeout(site("user-ada"), 50*time.Millisecond)
	defer cancel()
	_, err = draft(s, ctx, r.GetId(), nil, "")
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "Foundry couldn’t write a goal: the model")
	assert.Equal(t, foundrymodel.FoundryCompanyResearchSpend{USDMicros: 1000, CutShort: true}, store.spends[1])
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

	// A goal whose spend can't be recorded is still answered: it was paid
	// for, and its reservation stays charged.
	store.finishErr = errors.New("down")
	model.answers = append(model.answers, researchAnswer{tool: companyresearch.GoalTool, input: acmeGoal})
	paid, err := draft(s, site("user-ada"), r.GetId(), nil, "")
	require.NoError(t, err)
	assert.Equal(t, "Acme orders", paid.GetName())
	store.finishErr = nil

	// A draft the model fails is charged, and UNAVAILABLE.
	model.answers = append(model.answers, researchAnswer{tool: companyresearch.GoalTool, input: `{"goal":" ","name":"x"}`})
	_, err = draft(s, site("user-ada"), r.GetId(), nil, "")
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Equal(t, "FOUNDRY_RESEARCH_FAILED", reasonOf(err))
	assert.Len(t, store.ofKind(foundrymodel.FoundryCompanyResearchGoal), 3)
}

// A summary the person corrected replaces the research's in the goal's
// prompt; a blank one keeps it, and one past MaxSummary is refused.
func TestDraftFoundryGoalCorrectedSummary(t *testing.T) {
	s, _, model, _ := researchService(t, researchAnswer{tool: companyresearch.ResearchTool, input: acmeResearch},
		researchAnswer{tool: companyresearch.GoalTool, input: acmeGoal},
		researchAnswer{tool: companyresearch.GoalTool, input: acmeGoal})
	r, err := research(s, site("user-ada"), "acme.com")
	require.NoError(t, err)
	ask := func(summary string) error {
		_, err := s.DraftFoundryGoal(site("user-ada"), &foundryv1.DraftFoundryGoalRequest{WorkspaceId: "ws-1", ResearchId: r.GetId(), Summary: summary})
		return err
	}

	require.NoError(t, ask("  Acme sells anvils to cartoon coyotes.  "))
	user := model.requests[1].Messages[0].Content[0].(llm.Text).Text
	assert.Contains(t, user, "What it does: Acme sells anvils to cartoon coyotes.\n")
	assert.NotContains(t, user, r.GetSummary())

	require.NoError(t, ask(" "))
	assert.Contains(t, model.requests[2].Messages[0].Content[0].(llm.Text).Text, "What it does: "+r.GetSummary()+"\n")

	err = ask(strings.Repeat("s", companyresearch.MaxSummary+1))
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Len(t, model.requests, 3, "a refused summary never reached the model")
}
