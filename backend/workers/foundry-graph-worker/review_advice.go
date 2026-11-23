package foundrygraphworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/reviewadvice"
	"github.com/helixdb/helix-foundry/backend/reviewcards"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The review advisor: beside the indexer, every PollInterval while
// foundry.reviews.advise is on, it claims one load owed advice at a time
// (store/foundry_review_advice.go) and asks the workspace's verify model
// about the open name and value cards the rules leave undecided
// (backend/reviewadvice), cards_per_call a call, in the walk's order. It
// stores what came of every card it looked at: rules (a rule covers it),
// skipped (its review is gone, or it is too large to send whole), none (the
// model picked nothing, or never answered usably) or advised.
//
// Hosted, it advises only on Helix's Claude for a workspace on a sold plan,
// so neither Free nor an owner's key pays for it; locally, on the install's
// model, which its .env may turn off. It never calls Ollama, nor a server
// that takes one call at a time: those queue calls in this process with
// Founder's and questions', which a person waits on.
//
// Its reads share each database's pace with the indexer's
// (store.ReserveFoundryReviewRead), and its spend is the workspace's
// monthly advice budget, charged whatever comes of a call. Its store has no
// way to settle or decide anything (adviceStore): only a person does.

const (
	// adviceLease is a claim's lease, renewed to a call's timeout plus
	// adviceLeaseMargin before each call.
	adviceLease       = 2 * time.Minute
	adviceLeaseMargin = 30 * time.Second
	// adviceCalls is the most calls one batch makes: one, and two more for
	// an answer that doesn't read or a provider's passing refusal.
	adviceCalls = 3
	// adviceRetryWait is the wait before a call after a passing refusal,
	// doubled for the next.
	adviceRetryWait = 2 * time.Second
	// adviceTimeout bounds one batch's calls; an OpenAI-compatible server's
	// own call timeout may be longer.
	adviceTimeout = 90 * time.Second
	// adviceRecheck is when a load stopped for its plan or its model is
	// looked at again.
	adviceRecheck = time.Hour
	// adviceAttempts is how many claims in a row the provider may fail
	// before the load stops, failed, until it is owed again.
	adviceAttempts = 3
	// adviceScan is how many cards a claim looks at at most, as a multiple
	// of max_per_load: most may be covered by rules.
	adviceScan = 10
	// adviceCharsPerToken estimates a prompt's tokens from its length, on
	// the generous side, for a batch's worst case against the budget.
	adviceCharsPerToken = 3
)

var reviewAdviceCards = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_review_advice_cards_total",
	Help: "Review cards the advisor stored, by outcome: advised, none (the model picked nothing, or never answered usably), rules " +
		"(a rule covers it) or skipped (its review is gone, or it is too large to send whole).",
}, []string{"outcome"})

var reviewAdviceSpend = promauto.NewCounter(prometheus.CounterOpts{
	Name: "helix_foundry_review_advice_usd_micros_total",
	Help: "What review advice's model calls spent, in millionths of a US dollar.",
})

var reviewAdviceStops = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_review_advice_stops_total",
	Help: "Claims of a load's review advice that ended, by why: done, limit, budget, plan, no_model, failed, or backoff.",
}, []string{"why"})

// adviceStore is what the advisor reads and writes of Foundry's store: its
// claim, the cards and decisions it reads, the read pace it shares, and its
// spend. It holds no settle or decision write.
type adviceStore interface {
	graphproxy.Store
	ClaimFoundryReviewAdvice(ctx context.Context, workerID string, lease time.Duration) (*model.FoundryReviewAdviceClaim, error)
	RenewFoundryReviewAdvice(ctx context.Context, workerID string, claim model.FoundryReviewAdviceClaim, lease time.Duration) (bool, error)
	FinishFoundryReviewAdvice(ctx context.Context, workerID string, claim model.FoundryReviewAdviceClaim, why model.FoundryReviewAdviceStop,
		recheck time.Duration) (bool, error)
	BackOffFoundryReviewAdvice(ctx context.Context, workerID string, claim model.FoundryReviewAdviceClaim) (bool, error)
	StoreFoundryReviewAdvice(ctx context.Context, workerID string, claim model.FoundryReviewAdviceClaim, rows []model.FoundryReviewAdviceRow) (bool, error)
	ChargeFoundryReviewAdvice(ctx context.Context, workspaceID string, usdMicros int64) error
	SumFoundryReviewAdviceSpend(ctx context.Context, workspaceID string, month time.Time) (int64, error)
	ListFoundryReviewCards(ctx context.Context, ontologyID, loadID string, filter model.FoundryReviewCardFilter,
		after *model.FoundryReviewCardCursor, limit int) ([]*model.FoundryReviewListedCard, error)
	ListFoundryReviewNameDecisions(ctx context.Context, ontologyID string, names [][2]string) ([]*model.FoundryReviewNameDecisions, error)
	ReserveFoundryReviewRead(ctx context.Context, tenantID string, gap time.Duration) (time.Duration, error)
}

// soldPlans says which hosted workspaces are on a sold plan; *ai.Plans.
type soldPlans interface {
	Sold(ctx context.Context, workspaceID string) (bool, error)
}

// advisor writes review advice; one per worker.
type advisor struct {
	db      adviceStore
	dbs     ports.Databases
	gateway graphproxy.Querier
	ai      resolver
	// plans gates a hosted workspace; local installs sell none and need none.
	plans    soldPlans
	local    bool
	models   func(r ai.Resolved, catalog *ai.Catalog, role ai.Role) (llm.Binding, error)
	catalog  *ai.Catalog
	cfg      config.ReviewAdviceConfig
	workerID string
	now      func() time.Time
	// readGap is the least time between two reads of one database;
	// retryWait the first wait before a call after a passing refusal.
	readGap   time.Duration
	retryWait time.Duration
}

// tick advises claimed loads, one at a time, until none is due.
func (a *advisor) tick(ctx context.Context) {
	if !a.cfg.Enabled {
		return
	}
	for ctx.Err() == nil {
		c, err := a.db.ClaimFoundryReviewAdvice(ctx, a.workerID, adviceLease)
		if err != nil {
			if ctx.Err() == nil {
				log.From(ctx).Warn("review advice: claim failed", "err", err)
			}
			return
		}
		if c == nil {
			return
		}
		a.advise(log.Into(ctx, log.From(ctx).With("load", c.LoadID, "ontology", c.OntologyID)), *c)
	}
}

// adviceModel is the model a claim asks: bound, its name as stored with
// its answers, and its calls' timeout.
type adviceModel struct {
	llm.Binding
	name    string
	timeout time.Duration
}

// advise advises one claimed load's cards until none is left, a limit or
// the budget stops it, or a failure backs it off.
func (a *advisor) advise(ctx context.Context, c model.FoundryReviewAdviceClaim) {
	binding, b, ok := a.gate(ctx, c)
	if !ok {
		return
	}
	var after *model.FoundryReviewCardCursor
	sent, looked := 0, 0
	for ctx.Err() == nil {
		if sent >= a.cfg.MaxPerLoad || looked >= adviceScan*a.cfg.MaxPerLoad {
			a.finish(ctx, c, model.FoundryReviewAdviceLimit, 0)
			return
		}
		cards, err := a.db.ListFoundryReviewCards(ctx, c.OntologyID, c.LoadID, model.FoundryReviewCardFilter{
			Lane: reviewcards.LaneDecide, Kinds: []reviewcards.Kind{reviewcards.KindName, reviewcards.KindValue}, Unadvised: true,
		}, after, 2*a.cfg.CardsPerCall)
		if err != nil {
			log.From(ctx).Warn("review advice: list the load's cards failed", "err", err)
			a.backOff(ctx, c, false)
			return
		}
		if len(cards) == 0 {
			a.finish(ctx, c, model.FoundryReviewAdviceDone, 0)
			return
		}
		looked += len(cards)
		last := cards[len(cards)-1]
		after = &model.FoundryReviewCardCursor{PerDecision: last.PerDecision, Facts: last.Facts, KindOrder: last.Kind.KindOrder(),
			Shuffle: last.Shuffle, ID: last.ID}
		ask, done, ok := a.sort(ctx, c, binding, cards)
		if !ok {
			return
		}
		if len(done) > 0 && !a.store(ctx, c, done) {
			return
		}
		for len(ask) > 0 {
			n := min(len(ask), a.cfg.CardsPerCall, a.cfg.MaxPerLoad-sent)
			if n <= 0 {
				break
			}
			if !a.ask(ctx, c, b, ask[:n]) {
				return
			}
			sent += n
			ask = ask[n:]
		}
	}
}

// gate decides whether the claim's workspace is advised, and on what: the
// database it reads and the model it asks. It ends the claim when not.
func (a *advisor) gate(ctx context.Context, c model.FoundryReviewAdviceClaim) (graphproxy.Binding, adviceModel, bool) {
	logger := log.From(ctx)
	if !a.local {
		sold, err := a.plans.Sold(ctx, c.WorkspaceID)
		switch {
		case err != nil:
			logger.Info("review advice: the workspace's plan can't be read now; backing off", "err", err)
			a.backOff(ctx, c, false)
			return graphproxy.Binding{}, adviceModel{}, false
		case !sold:
			a.finish(ctx, c, model.FoundryReviewAdvicePlan, adviceRecheck)
			return graphproxy.Binding{}, adviceModel{}, false
		}
	}
	resolved, err := a.ai.Resolve(ctx, c.WorkspaceID)
	b := adviceModel{timeout: adviceTimeout}
	if err == nil {
		err = advisable(resolved, a.local)
	}
	if err == nil {
		b.Binding, err = a.models(resolved, a.catalog, ai.RoleVerify)
	}
	for _, m := range resolved.Settings.Models.Each() {
		if m.Role == ai.RoleVerify {
			b.name = m.Model.Name
		}
	}
	if _, ok := resolved.Settings.Provider.(ai.Compatible); ok {
		// The owner's own server, which may load and write slowly: its
		// setting's call timeout.
		b.timeout = max(b.timeout, resolved.CallTimeout)
	}
	if err != nil {
		logger.Info("review advice: no model advises the workspace", "err", err)
		a.finish(ctx, c, model.FoundryReviewAdviceNoModel, adviceRecheck)
		return graphproxy.Binding{}, adviceModel{}, false
	}
	binding, err := graphproxy.BindRead(ctx, a.db, a.dbs, c.WorkspaceID, c.OntologyID)
	switch status.Code(err) {
	case codes.OK:
	case codes.NotFound:
		logger.Info("review advice: the load's ontology or database is gone", "err", err)
		a.finish(ctx, c, model.FoundryReviewAdviceDone, 0)
		return graphproxy.Binding{}, adviceModel{}, false
	default:
		logger.Info("review advice: the load's database can't be read now; backing off", "err", err)
		a.backOff(ctx, c, false)
		return graphproxy.Binding{}, adviceModel{}, false
	}
	return binding, b, true
}

// errNotAdvisable is a model the advisor doesn't call.
var errNotAdvisable = errors.New("review advice runs on Helix's model hosted, and never on a server taking one call at a time")

// advisable refuses a model the advisor doesn't call: hosted, any but
// Helix's own; anywhere, Ollama or an OpenAI-compatible server taking one
// call at a time, whose calls this process queues behind people's.
func advisable(r ai.Resolved, local bool) error {
	if !local && r.Source != ai.SourceDefault {
		return errNotAdvisable
	}
	switch p := r.Settings.Provider.(type) {
	case ai.Ollama:
		return errNotAdvisable
	case ai.Compatible:
		if p.MaxCallsInFlight <= 1 {
			return errNotAdvisable
		}
	}
	return nil
}

// sorted is a card the model is asked about, as sent.
type sorted struct {
	id   reviewcards.ID
	card reviewadvice.Card
}

// sort reads cards' reviews and their names' decisions, and sorts them into
// those to ask about and the rows of those not to: rules or skipped. A read
// that fails backs the claim off (ok false).
func (a *advisor) sort(ctx context.Context, c model.FoundryReviewAdviceClaim, binding graphproxy.Binding,
	cards []*model.FoundryReviewListedCard) (ask []sorted, done []model.FoundryReviewAdviceRow, ok bool) {
	var keys []string
	var names [][2]string
	for _, card := range cards {
		keys = append(keys, cardKeys(card.Card)...)
		if card.Kind == reviewcards.KindName && card.Type != nil && card.NormName != nil {
			names = append(names, [2]string{*card.Type, *card.NormName})
		}
	}
	reviews, ok := a.readReviews(ctx, c, binding, keys)
	if !ok {
		return nil, nil, false
	}
	decided, err := a.db.ListFoundryReviewNameDecisions(ctx, c.OntologyID, names)
	if err != nil {
		log.From(ctx).Warn("review advice: read the names' decisions failed; backing off", "err", err)
		a.backOff(ctx, c, false)
		return nil, nil, false
	}
	byName := map[[2]string]*reviewadvice.Names{}
	for _, n := range decided {
		out := &reviewadvice.Names{Total: n.Total, Choices: n.Choices}
		for _, d := range n.Newest {
			if d.Kind == model.FoundryReviewResolve && d.DocID != nil {
				out.Newest = append(out.Newest, reviewadvice.NameDecision{DocID: *d.DocID, EntityKey: d.EntityKey})
			}
		}
		byName[[2]string{n.Type, n.NormName}] = out
	}
	for _, card := range cards {
		var read []*reviewcards.Review
		for _, k := range cardKeys(card.Card) {
			if r := reviews[k]; r != nil {
				read = append(read, r)
			}
		}
		in := reviewadvice.Input{Kind: card.Kind, Lane: card.Lane, Review: reviews[card.ReviewKey]}
		if card.Type != nil && card.NormName != nil {
			in.Names = byName[[2]string{*card.Type, *card.NormName}]
		}
		row := model.FoundryReviewAdviceRow{CardID: card.ID}
		switch {
		case !reviewadvice.Settles(in):
			row.Outcome = model.FoundryReviewAdviceSkipped
		case !reviewadvice.Abstains(in):
			row.Outcome = model.FoundryReviewAdviceRules
		default:
			sent, ok := reviewadvice.CardOf(card.Card, read)
			if ok {
				ask = append(ask, sorted{id: card.ID, card: sent})
				continue
			}
			row.Outcome = model.FoundryReviewAdviceSkipped
		}
		done = append(done, row)
	}
	return ask, done, true
}

// cardKeys are the reviews a card is read by: a name's settling review and
// the next of its quotes, a value's review.
func cardKeys(card *reviewcards.Card) []string {
	if card.Kind != reviewcards.KindName || len(card.ReviewKeys) == 0 {
		return []string{card.ReviewKey}
	}
	return card.ReviewKeys[:min(len(card.ReviewKeys), reviewadvice.MaxNameQuotes)]
}

// readReviews reads keys of the claim's load in one gateway read, paced as
// the indexer's: a read turned away or failed backs the claim off.
func (a *advisor) readReviews(ctx context.Context, c model.FoundryReviewAdviceClaim, binding graphproxy.Binding,
	keys []string) (map[string]*reviewcards.Review, bool) {
	body, err := reviewcards.ReviewsByKeyQuery(c.LoadID, keys)
	if err == nil && !paceRead(ctx, a.db, binding.TenantID, a.readGap) {
		return nil, false
	}
	var resp struct {
		code int
		body []byte
	}
	if err == nil {
		r, qerr := a.gateway.Query(ctx, binding.Endpoint, binding.TenantID, body, nil)
		resp.code, resp.body, err = r.StatusCode, r.Body, qerr
	}
	if err == nil && resp.code != http.StatusOK {
		err = fmt.Errorf("the database answered %d", resp.code)
	}
	var reviews map[string]*reviewcards.Review
	if err == nil {
		reviews, err = reviewcards.ParseReviewsByKey(resp.body, keys)
	}
	if err != nil {
		if ctx.Err() == nil {
			log.From(ctx).Info("review advice: the cards' reviews can't be read now; backing off", "err", err)
			a.backOff(ctx, c, false)
		}
		return nil, false
	}
	return reviews, true
}

// ask asks the model about batch and stores what it said, charging what it
// spent whatever comes of it. It reports whether the claim goes on.
func (a *advisor) ask(ctx context.Context, c model.FoundryReviewAdviceClaim, b adviceModel, batch []sorted) bool {
	logger := log.From(ctx)
	cards := make([]reviewadvice.Card, len(batch))
	for i, s := range batch {
		cards[i] = s.card
	}
	system, user, err := reviewadvice.Prompt(cards)
	if err != nil {
		logger.Error("review advice: write the prompt failed", "err", err)
		a.backOff(ctx, c, false)
		return false
	}
	now := a.now().UTC()
	spent, err := a.db.SumFoundryReviewAdviceSpend(ctx, c.WorkspaceID, now)
	if err != nil {
		logger.Warn("review advice: read the month's spend failed; backing off", "err", err)
		a.backOff(ctx, c, false)
		return false
	}
	prompt := int64((len(system)+len(user)+len(reviewadvice.ToolSchema))/adviceCharsPerToken + 1)
	worst := adviceCalls * b.Pricer.Micros(llm.Usage{In: prompt, Out: reviewadvice.MaxOutputTokens})
	if spent+worst > a.cfg.MonthlyMicros() {
		next := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		a.finish(ctx, c, model.FoundryReviewAdviceBudget, next.Sub(now))
		return false
	}
	timeout := b.timeout
	if held, err := a.db.RenewFoundryReviewAdvice(ctx, a.workerID, c, timeout+adviceLeaseMargin); err != nil || !held {
		logger.Info("review advice: the claim's lease was lost before a call", "err", err)
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	advice, cost, failure := llm.CallTool(callCtx, b.Binding, llm.Request{
		System: system, MaxOutput: reviewadvice.MaxOutputTokens,
		Messages: []llm.Message{{Role: llm.User, Content: []llm.Block{llm.Text{Text: user}}}},
		Tools:    []llm.Tool{{Name: reviewadvice.ToolName, Description: reviewadvice.ToolDescription, InputSchema: reviewadvice.ToolSchema}},
	}, reviewadvice.ToolName, adviceCalls, a.retryWait, func(raw json.RawMessage) ([]reviewadvice.Advice, error) {
		return reviewadvice.Parse(raw, cards)
	})
	cancel()
	a.charge(ctx, c, cost)
	switch {
	case failure == "" || failure == llm.FailureUnusable:
	case ctx.Err() != nil:
		// The worker is stopping: the lease lapses, and the batch is asked
		// again.
		return false
	default:
		logger.Info("review advice: the model's provider failed", "failure", failure)
		a.backOff(ctx, c, true)
		return false
	}
	rows := make([]model.FoundryReviewAdviceRow, len(batch))
	for i, s := range batch {
		rows[i] = model.FoundryReviewAdviceRow{CardID: s.id, Outcome: model.FoundryReviewAdviceNone, Offered: s.card.Offered(),
			Model: b.name, PromptVersion: reviewadvice.PromptVersion}
	}
	for _, ad := range advice {
		if ad.Choice != nil {
			r := &rows[ad.Card]
			r.Outcome, r.Choice, r.Rationale, r.Cites = model.FoundryReviewAdvised, ad.Choice, ad.Rationale, ad.Cites
		}
	}
	return a.store(ctx, c, rows)
}

// store stores rows while the claim holds; a lost lease or a failed store
// ends the claim (the lease lapses).
func (a *advisor) store(ctx context.Context, c model.FoundryReviewAdviceClaim, rows []model.FoundryReviewAdviceRow) bool {
	stored, err := a.db.StoreFoundryReviewAdvice(ctx, a.workerID, c, rows)
	switch {
	case err != nil:
		log.From(ctx).Warn("review advice: store failed; the lease lapses and the load is claimed again", "err", err)
		return false
	case !stored:
		log.From(ctx).Info("review advice: the claim's lease was lost before its store", "cards", len(rows))
		return false
	}
	for _, r := range rows {
		reviewAdviceCards.WithLabelValues(string(r.Outcome)).Inc()
	}
	return true
}

// charge enters what calls spent in the workspace's ledger, with a bound of
// its own, whether the batch was stored, lost its lease or was cut short:
// it was spent, and the month's budget counts it.
func (a *advisor) charge(ctx context.Context, c model.FoundryReviewAdviceClaim, cost int64) {
	if cost <= 0 {
		return
	}
	reviewAdviceSpend.Add(float64(cost))
	chargeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := a.db.ChargeFoundryReviewAdvice(chargeCtx, c.WorkspaceID, cost); err != nil {
		log.From(ctx).Error("review advice: charge failed: the budget misses what it spent", "err", err, "usd_micros", cost)
	}
}

// finish ends the claim stopped for why, due again recheck from now (0:
// once owed again), with a bound of its own.
func (a *advisor) finish(ctx context.Context, c model.FoundryReviewAdviceClaim, why model.FoundryReviewAdviceStop, recheck time.Duration) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if _, err := a.db.FinishFoundryReviewAdvice(finishCtx, a.workerID, c, why, recheck); err != nil {
		log.From(ctx).Warn("review advice: finish failed; the lease lapses", "err", err)
		return
	}
	reviewAdviceStops.WithLabelValues(string(why)).Inc()
}

// backOff puts the claim off after a failure; the model's provider failing
// adviceAttempts claims in a row stops it, failed.
func (a *advisor) backOff(ctx context.Context, c model.FoundryReviewAdviceClaim, provider bool) {
	if provider && c.Attempts+1 >= adviceAttempts {
		a.finish(ctx, c, model.FoundryReviewAdviceFailed, 0)
		return
	}
	backCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if _, err := a.db.BackOffFoundryReviewAdvice(backCtx, a.workerID, c); err != nil {
		log.From(ctx).Warn("review advice: back off failed; the lease lapses", "err", err)
		return
	}
	reviewAdviceStops.WithLabelValues("backoff").Inc()
}
