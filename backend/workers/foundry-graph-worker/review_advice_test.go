package foundrygraphworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/reviewadvice"
	"github.com/helixdb/helix-foundry/backend/reviewcards"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// adviceMem is the advisor's store: one claim of the harness's load, its
// decide cards in the walk's order, and what the advisor did with them.
// Its ontology lookups are memStore's.
type adviceMem struct {
	*memStore
	mu       sync.Mutex
	claim    *model.FoundryReviewAdviceClaim
	cards    []*model.FoundryReviewListedCard
	names    []*model.FoundryReviewNameDecisions
	rows     map[reviewcards.ID]model.FoundryReviewAdviceRow
	finished []model.FoundryReviewAdviceStop
	recheck  []time.Duration
	backoffs int
	renewed  []time.Duration
	charged  int64
	spent    int64
	// leaseLost makes every store find its lease lost; storeErr fails it.
	leaseLost bool
	storeErr  error
}

func (m *adviceMem) ClaimFoundryReviewAdvice(context.Context, string, time.Duration) (*model.FoundryReviewAdviceClaim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.claim
	m.claim = nil
	return c, nil
}

func (m *adviceMem) RenewFoundryReviewAdvice(_ context.Context, _ string, _ model.FoundryReviewAdviceClaim, lease time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.renewed = append(m.renewed, lease)
	return true, nil
}

func (m *adviceMem) FinishFoundryReviewAdvice(_ context.Context, _ string, _ model.FoundryReviewAdviceClaim, why model.FoundryReviewAdviceStop,
	recheck time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finished = append(m.finished, why)
	m.recheck = append(m.recheck, recheck)
	return true, nil
}

func (m *adviceMem) BackOffFoundryReviewAdvice(context.Context, string, model.FoundryReviewAdviceClaim) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.backoffs++
	return true, nil
}

func (m *adviceMem) StoreFoundryReviewAdvice(_ context.Context, _ string, _ model.FoundryReviewAdviceClaim, rows []model.FoundryReviewAdviceRow) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.storeErr != nil || m.leaseLost {
		return false, m.storeErr
	}
	for _, r := range rows {
		if _, ok := m.rows[r.CardID]; !ok {
			m.rows[r.CardID] = r
		}
	}
	return true, nil
}

func (m *adviceMem) ChargeFoundryReviewAdvice(_ context.Context, _ string, usdMicros int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.charged += usdMicros
	return nil
}

func (m *adviceMem) SumFoundryReviewAdviceSpend(context.Context, string, time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spent + m.charged, nil
}

// ListFoundryReviewCards lists the cards after the cursor, those advised
// left out when asked.
func (m *adviceMem) ListFoundryReviewCards(_ context.Context, _, _ string, filter model.FoundryReviewCardFilter, after *model.FoundryReviewCardCursor,
	limit int) ([]*model.FoundryReviewListedCard, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	from := 0
	if after != nil {
		from = slices.IndexFunc(m.cards, func(c *model.FoundryReviewListedCard) bool { return c.ID == after.ID }) + 1
	}
	var out []*model.FoundryReviewListedCard
	for _, c := range m.cards[from:] {
		if _, ok := m.rows[c.ID]; (ok && filter.Unadvised) || !slices.Contains(filter.Kinds, c.Kind) || len(out) == limit {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (m *adviceMem) ListFoundryReviewNameDecisions(context.Context, string, [][2]string) ([]*model.FoundryReviewNameDecisions, error) {
	return m.names, nil
}

func (m *adviceMem) ReserveFoundryReviewRead(context.Context, string, time.Duration) (time.Duration, error) {
	return 0, nil
}

func (m *adviceMem) outcomes() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for _, c := range m.cards {
		if r, ok := m.rows[c.ID]; ok {
			out[c.ReviewKey] = string(r.Outcome)
		}
	}
	return out
}

// adviceGraph answers reviews by key from its reviews, as the database
// holds them; status, when set, answers every read.
type adviceGraph struct {
	mu      sync.Mutex
	reviews map[string]map[string]any
	status  int
	reads   [][]string
}

func (g *adviceGraph) Query(_ context.Context, _, _ string, body []byte, _ http.Header) (helixgateway.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var req struct {
		Parameters struct {
			Keys []string `json:"keys"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return helixgateway.Response{}, err
	}
	g.reads = append(g.reads, req.Parameters.Keys)
	if g.status != 0 {
		return helixgateway.Response{StatusCode: g.status, Body: []byte(`{"error":"rate_limit_unavailable"}`)}, nil
	}
	answer := map[string][]map[string]any{"reviews": {}, "review_concerns": {}, "concerned": {}}
	for i, k := range req.Parameters.Keys {
		r, ok := g.reviews[k]
		if !ok {
			continue
		}
		review := map[string]any{"$id": i + 1}
		for f, v := range r {
			if f != "concerned" {
				review[f] = v
			}
		}
		answer["reviews"] = append(answer["reviews"], review)
		for j, e := range r["concerned"].([]map[string]any) {
			id := 1000*(i+1) + j
			answer["review_concerns"] = append(answer["review_concerns"], map[string]any{"$from": i + 1, "$to": id})
			row := map[string]any{"$id": id}
			for f, v := range e {
				row[f] = v
			}
			answer["concerned"] = append(answer["concerned"], row)
		}
	}
	b, _ := json.Marshal(answer)
	return helixgateway.Response{StatusCode: 200, Body: b}, nil
}

// adviceScript is the model: each call's answer, from the cards sent (the
// last again once they run out), and every request.
type adviceScript struct {
	mu       sync.Mutex
	answers  []func(cards []string) ([]llm.Chunk, error)
	requests []llm.Request
}

// picks answers o1 for every card sent, citing e1, at 2,000 micros a call.
func picks(cards []string) ([]llm.Chunk, error) {
	var advice []map[string]any
	for _, c := range cards {
		advice = append(advice, map[string]any{"card": c, "choice": "o1", "rationale": "Quote e1 names it.", "cites": []string{"e1"}})
	}
	input, _ := json.Marshal(map[string]any{"advice": advice})
	return []llm.Chunk{llm.ToolUse{ID: "t1", Name: reviewadvice.ToolName, Input: input}, llm.Usage{In: 1000, Out: 500}}, nil
}

func unusable([]string) ([]llm.Chunk, error) {
	return []llm.Chunk{llm.Text{Text: "I think o1."}, llm.Usage{In: 1000, Out: 500}}, nil
}

func refusing(kind llm.ErrorKind) func([]string) ([]llm.Chunk, error) {
	return func([]string) ([]llm.Chunk, error) {
		return []llm.Chunk{llm.Usage{In: 1000, Out: 0}}, &llm.Error{Kind: kind, Err: errors.New("no")}
	}
}

func (m *adviceScript) Stream(_ context.Context, r llm.Request) iter.Seq2[llm.Chunk, error] {
	m.mu.Lock()
	answer := m.answers[min(len(m.requests), len(m.answers)-1)]
	m.requests = append(m.requests, r)
	m.mu.Unlock()
	var sent struct {
		Cards []struct {
			Card string `json:"card"`
		} `json:"cards"`
	}
	_ = json.Unmarshal([]byte(r.Messages[0].Content[0].(llm.Text).Text), &sent)
	var ids []string
	for _, c := range sent.Cards {
		ids = append(ids, c.Card)
	}
	chunks, err := answer(ids)
	return func(yield func(llm.Chunk, error) bool) {
		for _, c := range chunks {
			if !yield(c, nil) {
				return
			}
		}
		if err != nil {
			yield(nil, err)
		}
	}
}

// adviceResolver resolves every workspace to resolved, or fails with err.
type adviceResolver struct {
	resolved ai.Resolved
	err      error
}

func (r adviceResolver) Resolve(context.Context, string) (ai.Resolved, error) {
	return r.resolved, r.err
}
func (r adviceResolver) ResolveServed(context.Context, string, []string) (ai.Resolved, error) {
	return r.resolved, r.err
}

type advicePlans struct {
	sold bool
	err  error
}

func (p advicePlans) Sold(context.Context, string) (bool, error) { return p.sold, p.err }

// helixClaude is Helix's own Claude, verifying on claude-sonnet.
var helixClaude = ai.Resolved{Source: ai.SourceDefault, Settings: ai.Settings{Provider: ai.Anthropic{},
	Models: ai.Roles{Verify: ai.Model{Name: "claude-sonnet-5-5"}}}}

type adviceTest struct {
	a     *advisor
	mem   *adviceMem
	graph *adviceGraph
	model *adviceScript
	roles []ai.Role
}

// newAdvisor advises the harness's load hosted on Helix's Claude for a
// workspace on a sold plan, two cards a call, with cards.
func newAdvisor(t *testing.T, cards ...testCard) *adviceTest {
	t.Helper()
	h := newHarness(t)
	at := &adviceTest{
		mem: &adviceMem{memStore: h.store, rows: map[reviewcards.ID]model.FoundryReviewAdviceRow{},
			claim: &model.FoundryReviewAdviceClaim{LoadID: "load-1", WorkspaceID: "ws-1", OntologyID: "ont-1", OwedAt: time.Now()}},
		graph: &adviceGraph{reviews: map[string]map[string]any{}},
		model: &adviceScript{answers: []func([]string) ([]llm.Chunk, error){picks}},
	}
	for i, c := range cards {
		at.add(i, c)
	}
	at.a = &advisor{
		db: at.mem, dbs: h.store, gateway: at.graph, ai: adviceResolver{resolved: helixClaude}, plans: advicePlans{sold: true},
		models: func(_ ai.Resolved, _ *ai.Catalog, role ai.Role) (llm.Binding, error) {
			at.roles = append(at.roles, role)
			return llm.Binding{Model: at.model, Pricer: llm.Pricer{Input: 1000, Output: 2000}}, nil
		},
		cfg:      config.ReviewAdviceConfig{Enabled: true, MonthlyUSD: 10, MaxPerLoad: 200, CardsPerCall: 2},
		workerID: "w1", now: time.Now, retryWait: time.Millisecond,
	}
	return at
}

// testCard is a card and its review as the graph holds it (nil: gone).
type testCard struct {
	kind   reviewcards.Kind
	key    string
	review map[string]any
}

func (at *adviceTest) add(i int, c testCard) {
	card := &reviewcards.Card{Kind: c.kind, Lane: reviewcards.LaneDecide, ReviewKey: c.key, ReviewKeys: []string{c.key}, DocID: "doc-a",
		Shuffle: uint32(i)}
	card.ID[0], card.ID[1] = byte(i), 1
	if c.kind == reviewcards.KindName {
		ty, norm := "Customer", "name-"+c.key
		card.Type, card.NormName = &ty, &norm
	}
	at.mem.cards = append(at.mem.cards, &model.FoundryReviewListedCard{Card: card})
	if c.review != nil {
		at.graph.reviews[c.key] = c.review
	}
}

// tied is a name review between n candidates, every one found.
func tied(key string, n int) testCard {
	var candidates []string
	var concerned []map[string]any
	for i := range n {
		k := fmt.Sprintf("%s-k%d", key, i+1)
		candidates = append(candidates, k)
		concerned = append(concerned, map[string]any{"$label": "Customer", "entity_key": k, "name": "Globex " + k})
	}
	return testCard{kind: reviewcards.KindName, key: key, review: map[string]any{"review_key": key, "reason": "ambiguous", "type": "Customer",
		"doc_id": "doc-a", "norm_name": "name-" + key, "candidates": candidates, "section": 0, "start": 1, "end": 6, "quote": "Globex signed.",
		"concerned": concerned}}
}

// stated is a value review whose quotes state values.
func stated(key string, values ...string) testCard {
	var quotes []string
	for i, v := range values {
		quotes = append(quotes, fmt.Sprintf(`{"value": %q, "doc_id": "doc-%d", "quote": "Term: %s", "span": {"section": %d, "start": 0, "end": 5}}`,
			v, i, v, i))
	}
	return testCard{kind: reviewcards.KindValue, key: key, review: map[string]any{"review_key": key, "reason": "prop_conflict", "type": "Contract",
		"entity_key": "ck-" + key, "prop": "term", "doc_id": "doc-a", "section": 0, "start": 0, "end": 5, "values": "[" + strings.Join(quotes, ",") + "]",
		"concerned": []map[string]any{{"$label": "Contract", "entity_key": "ck-" + key}}}}
}

func TestAdvisorAdvisesWhereTheRulesAbstain(t *testing.T) {
	at := newAdvisor(t,
		tied("n-tied", 2), tied("n-one", 1), stated("v-split", "12", "24"), stated("v-major", "12", "12", "24"),
		testCard{kind: reviewcards.KindName, key: "n-gone"}, tied("n-ten", 10), tied("n-tied-2", 3), stated("v-split-2", "a", "b", "c"))
	at.a.tick(context.Background())

	assert.Equal(t, map[string]string{"n-tied": "advised", "n-one": "rules", "v-split": "advised", "v-major": "rules", "n-gone": "skipped",
		"n-ten": "skipped", "n-tied-2": "advised", "v-split-2": "advised"}, at.mem.outcomes())
	assert.Equal(t, []model.FoundryReviewAdviceStop{model.FoundryReviewAdviceDone}, at.mem.finished)
	assert.Equal(t, []ai.Role{ai.RoleVerify}, at.roles, "the verify model")
	// The cards to ask about, two a call, in the walk's order.
	require.Len(t, at.model.requests, 2)
	for i, want := range [][]string{{"name-n-tied", "12"}, {"name-n-tied-2", "\"a\""}} {
		user := at.model.requests[i].Messages[0].Content[0].(llm.Text).Text
		for _, w := range want {
			assert.Contains(t, user, w, "call %d", i)
		}
		assert.Equal(t, 2, strings.Count(user, `"card":`), "call %d", i)
		assert.Equal(t, reviewadvice.MaxOutputTokens, at.model.requests[i].MaxOutput)
	}
	assert.EqualValues(t, 4000, at.mem.charged, "each call charged")
	assert.Equal(t, []time.Duration{adviceTimeout + adviceLeaseMargin, adviceTimeout + adviceLeaseMargin}, at.mem.renewed,
		"the lease renewed before each call")
	row := at.mem.rows[at.mem.cards[0].ID]
	assert.Equal(t, "claude-sonnet-5-5", row.Model)
	assert.Equal(t, reviewadvice.PromptVersion, row.PromptVersion)
	assert.Len(t, row.Offered, 3, "two candidates and not in data")
	assert.Equal(t, row.Offered[0], *row.Choice)
	assert.Equal(t, "Quote e1 names it.", row.Rationale)
	assert.Equal(t, []reviewadvice.Cite{{DocID: "doc-a", Section: 0, Start: 1, End: 6}}, row.Cites)
	value := at.mem.rows[at.mem.cards[2].ID]
	assert.Equal(t, "12", *value.Choice.Prop.Text)
}

func TestAdvisorMakesNoCallForRules(t *testing.T) {
	at := newAdvisor(t, tied("n-one", 1), stated("v-major", "12", "12", "24"))
	at.a.tick(context.Background())
	assert.Empty(t, at.model.requests)
	assert.Zero(t, at.mem.charged)
	assert.Equal(t, map[string]string{"n-one": "rules", "v-major": "rules"}, at.mem.outcomes())
	assert.Equal(t, []model.FoundryReviewAdviceStop{model.FoundryReviewAdviceDone}, at.mem.finished)
}

func TestAdvisorStopsAtItsLimits(t *testing.T) {
	var cards []testCard
	for i := range 5 {
		cards = append(cards, tied(fmt.Sprintf("n%d", i), 2))
	}
	at := newAdvisor(t, cards...)
	at.a.cfg.MaxPerLoad = 3
	at.a.tick(context.Background())
	assert.Len(t, at.model.requests, 2, "two, then one")
	assert.Equal(t, 3, len(at.mem.rows), "max_per_load sent")
	assert.Equal(t, []model.FoundryReviewAdviceStop{model.FoundryReviewAdviceLimit}, at.mem.finished)

	// The scan cap: cards rules cover count too.
	cards = nil
	for i := range 30 {
		cards = append(cards, tied(fmt.Sprintf("r%d", i), 1))
	}
	at = newAdvisor(t, cards...)
	at.a.cfg.MaxPerLoad, at.a.cfg.CardsPerCall = 2, 2
	at.a.tick(context.Background())
	assert.Empty(t, at.model.requests)
	assert.Len(t, at.mem.rows, 20, "ten times max_per_load looked at")
	assert.Equal(t, []model.FoundryReviewAdviceStop{model.FoundryReviewAdviceLimit}, at.mem.finished)
}

func TestAdvisorStaysWithinTheBudget(t *testing.T) {
	at := newAdvisor(t, tied("n1", 2))
	at.mem.spent = 9_999_000
	at.a.tick(context.Background())
	assert.Empty(t, at.model.requests)
	require.Equal(t, []model.FoundryReviewAdviceStop{model.FoundryReviewAdviceBudget}, at.mem.finished)
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	assert.InDelta(t, float64(time.Until(next)), float64(at.mem.recheck[0]), float64(time.Minute), "until the 1st of next month")
	assert.Empty(t, at.mem.rows)
}

func TestAdvisorGate(t *testing.T) {
	for name, c := range map[string]struct {
		setup  func(*advisor)
		stop   model.FoundryReviewAdviceStop
		backed bool
	}{
		"hosted Free":               {func(a *advisor) { a.plans = advicePlans{} }, model.FoundryReviewAdvicePlan, false},
		"a plan that can't be read": {func(a *advisor) { a.plans = advicePlans{err: errors.New("autumn down")} }, "", true},
		"hosted on an own key": {func(a *advisor) {
			r := helixClaude
			r.Source = ai.SourceWorkspace
			a.ai = adviceResolver{resolved: r}
		}, model.FoundryReviewAdviceNoModel, false},
		"no model resolves": {func(a *advisor) { a.ai = adviceResolver{err: ai.ErrNotConfigured} }, model.FoundryReviewAdviceNoModel, false},
		"local on Ollama": {func(a *advisor) {
			a.local, a.plans = true, nil
			a.ai = adviceResolver{resolved: ai.Resolved{Source: ai.SourceWorkspace, Settings: ai.Settings{Provider: ai.Ollama{BaseURL: "http://127.0.0.1:11434"}}}}
		}, model.FoundryReviewAdviceNoModel, false},
		"a server taking one call at a time": {func(a *advisor) {
			a.local, a.plans = true, nil
			a.ai = adviceResolver{resolved: ai.Resolved{Source: ai.SourceWorkspace, Settings: ai.Settings{Provider: ai.Compatible{
				BaseURL: "http://host.docker.internal:8000/v1", API: ai.CompatChatCompletions, MaxCallsInFlight: 1}}}}
		}, model.FoundryReviewAdviceNoModel, false},
		"a database gone": {func(a *advisor) { a.dbs.(*memStore).tenant.Deleted = true }, model.FoundryReviewAdviceDone, false},
	} {
		t.Run(name, func(t *testing.T) {
			at := newAdvisor(t, tied("n1", 2))
			c.setup(at.a)
			at.a.tick(context.Background())
			assert.Empty(t, at.model.requests, "no call")
			assert.Empty(t, at.mem.rows)
			if c.backed {
				assert.Equal(t, 1, at.mem.backoffs)
				assert.Empty(t, at.mem.finished)
				return
			}
			require.Equal(t, []model.FoundryReviewAdviceStop{c.stop}, at.mem.finished)
			if c.stop != model.FoundryReviewAdviceDone {
				assert.Equal(t, adviceRecheck, at.mem.recheck[0], "looked at again in an hour")
			}
		})
	}

	// Locally, the install's own model advises, with no plan.
	at := newAdvisor(t, tied("n1", 2))
	at.a.local, at.a.plans = true, nil
	at.a.ai = adviceResolver{resolved: ai.Resolved{Source: ai.SourceWorkspace, Settings: ai.Settings{Provider: ai.OpenAI{}}}}
	at.a.tick(context.Background())
	assert.Len(t, at.model.requests, 1)
	assert.Equal(t, map[string]string{"n1": "advised"}, at.mem.outcomes())
}

func TestAdvisorRetriesAndStoresNone(t *testing.T) {
	// A passing refusal, then an answer.
	at := newAdvisor(t, tied("n1", 2))
	at.model.answers = []func([]string) ([]llm.Chunk, error){refusing(llm.ErrRateLimited), picks}
	at.a.tick(context.Background())
	assert.Len(t, at.model.requests, 2)
	assert.Equal(t, map[string]string{"n1": "advised"}, at.mem.outcomes())

	// An unusable answer, then one that reads.
	at = newAdvisor(t, tied("n1", 2))
	at.model.answers = []func([]string) ([]llm.Chunk, error){unusable, picks}
	at.a.tick(context.Background())
	assert.Len(t, at.model.requests, 2)
	assert.Equal(t, map[string]string{"n1": "advised"}, at.mem.outcomes())

	// Unusable three times: none for the batch, every call charged.
	at = newAdvisor(t, tied("n1", 2), tied("n2", 2))
	at.model.answers = []func([]string) ([]llm.Chunk, error){unusable}
	at.a.tick(context.Background())
	assert.Len(t, at.model.requests, adviceCalls)
	assert.Equal(t, map[string]string{"n1": "none", "n2": "none"}, at.mem.outcomes())
	assert.EqualValues(t, 3*2000, at.mem.charged)
	assert.Len(t, at.mem.rows[at.mem.cards[0].ID].Offered, 3, "none keeps what was offered")
	assert.Equal(t, []model.FoundryReviewAdviceStop{model.FoundryReviewAdviceDone}, at.mem.finished)

	// The model answering one card of two: the other is none.
	at = newAdvisor(t, tied("n1", 2), tied("n2", 2))
	at.model.answers = []func([]string) ([]llm.Chunk, error){func(cards []string) ([]llm.Chunk, error) { return picks(cards[:1]) }}
	at.a.tick(context.Background())
	assert.Equal(t, map[string]string{"n1": "advised", "n2": "none"}, at.mem.outcomes())
}

func TestAdvisorBacksOffThenFails(t *testing.T) {
	at := newAdvisor(t, tied("n1", 2))
	at.model.answers = []func([]string) ([]llm.Chunk, error){refusing(llm.ErrAuth)}
	at.a.tick(context.Background())
	assert.Len(t, at.model.requests, 1, "a refusal no retry passes")
	assert.Equal(t, 1, at.mem.backoffs)
	assert.Empty(t, at.mem.finished)
	assert.Empty(t, at.mem.rows)
	assert.EqualValues(t, 1000, at.mem.charged, "what the refused call used is charged")

	// The third claim in a row the provider fails stops the load.
	at.mem.claim = &model.FoundryReviewAdviceClaim{LoadID: "load-1", WorkspaceID: "ws-1", OntologyID: "ont-1", Attempts: adviceAttempts - 1}
	at.a.tick(context.Background())
	assert.Equal(t, 1, at.mem.backoffs)
	assert.Equal(t, []model.FoundryReviewAdviceStop{model.FoundryReviewAdviceFailed}, at.mem.finished)

	// A read the database turns away backs off, and is no provider's
	// failure.
	at = newAdvisor(t, tied("n1", 2))
	at.graph.status = http.StatusServiceUnavailable
	at.mem.claim.Attempts = adviceAttempts - 1
	at.a.tick(context.Background())
	assert.Equal(t, 1, at.mem.backoffs)
	assert.Empty(t, at.mem.finished)
	assert.Empty(t, at.model.requests)
}

// What a call spent is charged whatever comes of its batch: a lost lease, a
// failed store, the worker stopping.
func TestAdvisorChargesWhatItSpent(t *testing.T) {
	at := newAdvisor(t, tied("n1", 2))
	at.mem.leaseLost = true
	at.a.tick(context.Background())
	assert.EqualValues(t, 2000, at.mem.charged)
	assert.Empty(t, at.mem.finished, "a lost lease ends nothing")

	at = newAdvisor(t, tied("n1", 2))
	at.mem.storeErr = errors.New("deadlock")
	at.a.tick(context.Background())
	assert.EqualValues(t, 2000, at.mem.charged)

	at = newAdvisor(t, tied("n1", 2))
	ctx, cancel := context.WithCancel(context.Background())
	at.model.answers = []func([]string) ([]llm.Chunk, error){func([]string) ([]llm.Chunk, error) {
		cancel()
		return []llm.Chunk{llm.Usage{In: 1000, Out: 500}}, context.Canceled
	}}
	at.a.tick(ctx)
	assert.EqualValues(t, 2000, at.mem.charged)
	assert.Empty(t, at.mem.rows, "the batch is asked again by the next claim")
	assert.Empty(t, at.mem.finished)
	assert.Zero(t, at.mem.backoffs)
}

// On an OpenAI-compatible server with a long call timeout, the lease is
// renewed past it before the call.
func TestAdvisorRenewsTheLeaseForALongCall(t *testing.T) {
	at := newAdvisor(t, tied("n1", 2))
	at.a.local, at.a.plans = true, nil
	at.a.ai = adviceResolver{resolved: ai.Resolved{Source: ai.SourceWorkspace, CallTimeout: 5 * time.Minute, Settings: ai.Settings{
		Provider: ai.Compatible{BaseURL: "http://host.docker.internal:8000/v1", API: ai.CompatChatCompletions, MaxCallsInFlight: 4}}}}
	at.a.tick(context.Background())
	assert.Equal(t, []time.Duration{5*time.Minute + adviceLeaseMargin}, at.mem.renewed)
	assert.Equal(t, map[string]string{"n1": "advised"}, at.mem.outcomes())
}

// Stop waits for the advisor's tick: Start returns once its call has
// ended and been charged.
func TestStopWaitsForTheAdvisor(t *testing.T) {
	h := newHarness(t)
	at := newAdvisor(t, tied("n1", 2))
	h.task.advice = at.a
	called := make(chan struct{})
	at.model.answers = []func([]string) ([]llm.Chunk, error){func([]string) ([]llm.Chunk, error) {
		close(called)
		time.Sleep(50 * time.Millisecond)
		return []llm.Chunk{llm.Usage{In: 1000, Out: 500}}, context.Canceled
	}}
	ran := make(chan error, 1)
	go func() { ran <- h.task.Start(context.Background()) }()
	<-called
	require.NoError(t, h.task.Stop(context.Background()))
	require.NoError(t, <-ran)
	assert.EqualValues(t, 2000, at.mem.charged, "the call in flight was charged before Start returned")
}
