package foundryagentworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/automation/suggest"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// suggestionLeaseMargin is how much longer a write's lease is than its
	// calls' timeout: for the store's reads and writes around them.
	suggestionLeaseMargin = suggest.StaleAfter - suggest.Timeout
	// suggestionLease is the lease a write is claimed under, for calls
	// within suggest.Timeout; one with longer calls renews it at once.
	suggestionLease = suggest.Timeout + suggestionLeaseMargin
	// suggestionAttempts is how many times a write is claimed before it
	// fails: a lease lost to a crash counts.
	suggestionAttempts = 3
	// suggestionCalls is the most calls one write makes: one, and two more
	// for an answer that doesn't read or a provider's passing refusal.
	suggestionCalls = 3
	// suggestionRetryWait is the wait before a call after a provider's
	// passing refusal (rate limited, overloaded), doubled for the next.
	suggestionRetryWait = 2 * time.Second
	// suggestionWriters is how many writes a worker makes at once: each is
	// mostly a wait on its model, so a backlog across workspaces drains
	// before readers stop waiting (suggest.StaleAfter).
	suggestionWriters = 3
	// charsPerToken estimates a prompt's tokens from its length, on the
	// generous side, for a write's worst case against the budget.
	charsPerToken = 3
)

// suggestionStore is what a write of suggestions reads and writes.
type suggestionStore interface {
	ClaimFoundryAutomationSuggestions(ctx context.Context, workerID string, lease time.Duration) (*model.FoundryAutomationSuggestions, error)
	FinishFoundryAutomationSuggestions(ctx context.Context, ontologyID, versionID, workerID string, end model.FoundryAutomationSuggestionsEnd) error
	RenewFoundryAutomationSuggestionsLease(ctx context.Context, ontologyID, versionID, workerID string, lease time.Duration) error
	ChargeFoundryAutomationSuggestions(ctx context.Context, workspaceID string, usdMicros int64) error
	SumFoundryAssistantSpend(ctx context.Context, workspaceID string, since time.Time) (int64, error)
	GetFoundryOntology(ctx context.Context, workspaceID, id string) (*model.FoundryOntology, error)
	GetOntologyVersion(ctx context.Context, ontologyID, id string) (*model.OntologyVersion, error)
	GetOntologyGeneration(ctx context.Context, ontologyID, id string) (*model.OntologyGeneration, error)
	GetOntologyVersionScope(ctx context.Context, ontologyID, versionID string) ([]string, error)
	ListConnectors(ctx context.Context, workspaceID string) ([]*model.Connector, error)
	ListFoundryDocumentSets(ctx context.Context, workspaceID string) ([]*model.FoundryDocumentSet, error)
}

// resolver is the workspace's model provider; *ai.Resolver.
type resolver interface {
	Resolve(ctx context.Context, workspaceID string) (ai.Resolved, error)
}

// suggester writes Founder's suggestions of every kind (questions,
// dashboards, changes to the ontology and workflows) in one call per
// version, a few versions at a time, on each workspace's extraction model,
// its cheapest task model, charged to Founder's monthly budget (package
// suggest).
type suggester struct {
	db       suggestionStore
	ai       resolver
	models   func(r ai.Resolved, catalog *ai.Catalog, role ai.Role) (llm.Binding, error)
	catalog  *ai.Catalog
	monthly  int64
	workerID string
	now      func() time.Time
	// writers is how many writes run at once; retryWait the first wait
	// before a call after a passing refusal.
	writers   int
	retryWait time.Duration
}

// drain writes due suggestions, s.writers at a time, until none is due.
func (s *suggester) drain(ctx context.Context) {
	var writers sync.WaitGroup
	for range max(s.writers, 1) {
		writers.Go(func() {
			for ctx.Err() == nil {
				row, err := s.db.ClaimFoundryAutomationSuggestions(ctx, s.workerID, suggestionLease)
				if err != nil {
					log.From(ctx).Error("claim automation suggestions failed", "err", err)
					return
				}
				if row == nil {
					return
				}
				s.write(ctx, row)
			}
		})
	}
	writers.Wait()
}

// errNotCurrent is a row whose version is no longer its ontology's current
// one: the page never reads it.
var errNotCurrent = errors.New("the version is no longer current")

// write writes a claimed row's suggestions and ends it: ready with what
// Parse kept of each kind, or failed saying why. What its calls spent is charged
// whatever comes of the row. A store read that fails, or a worker's stop
// before the model answered, leaves it to its lease, to be claimed again.
func (s *suggester) write(ctx context.Context, row *model.FoundryAutomationSuggestions) {
	logger := log.From(ctx).With("ontology", row.OntologyID, "version", row.VersionID, "attempt", row.Attempts)
	ctx = log.Into(ctx, logger)
	if row.Attempts > suggestionAttempts {
		s.finish(ctx, row, model.FoundryAutomationSuggestionsEnd{Failure: fmt.Sprintf("stopped without finishing %d times", row.Attempts-1)})
		return
	}
	in, err := s.input(ctx, row)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		s.finish(ctx, row, model.FoundryAutomationSuggestionsEnd{Failure: "the ontology or its version is gone"})
		return
	case errors.Is(err, errNotCurrent):
		s.finish(ctx, row, model.FoundryAutomationSuggestionsEnd{Failure: errNotCurrent.Error()})
		return
	case err != nil:
		logger.Warn("read what automation suggestions are written from failed; the lease lapses and it is claimed again", "err", err)
		return
	}
	resolved, err := s.ai.Resolve(ctx, row.WorkspaceID)
	var binding llm.Binding
	if err == nil {
		binding, err = s.models(resolved, s.catalog, ai.RoleExtract)
	}
	if err != nil {
		logger.Info("automation suggestions: no usable model", "err", err)
		s.finish(ctx, row, model.FoundryAutomationSuggestionsEnd{Failure: "no usable model"})
		return
	}
	system, user := suggest.Prompt(in)
	req := llm.Request{
		System: system, MaxOutput: suggest.MaxOutputTokens,
		Messages: []llm.Message{{Role: llm.User, Content: []llm.Block{llm.Text{Text: user}}}},
		Tools:    []llm.Tool{{Name: suggest.ToolName, Description: suggest.ToolDescription, InputSchema: suggest.ToolSchema}},
	}
	now := s.now().UTC()
	spent, err := s.db.SumFoundryAssistantSpend(ctx, row.WorkspaceID, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		logger.Warn("read Founder's spend failed; the lease lapses and it is claimed again", "err", err)
		return
	}
	prompt := int64((len(system)+len(user)+len(suggest.ToolSchema))/charsPerToken + 1)
	worst := suggestionCalls * binding.Pricer.Micros(llm.Usage{In: prompt, Out: suggest.MaxOutputTokens})
	if spent+worst > s.monthly {
		s.finish(ctx, row, model.FoundryAutomationSuggestionsEnd{Failure: "Founder's monthly budget is spent"})
		return
	}
	timeout := suggest.Timeout
	switch resolved.Settings.Provider.(type) {
	case ai.Ollama, ai.Compatible:
		// The owner's own server, whose calls this process queues (Ollama's
		// one at a time, Founder's turns among them), and a local
		// model may load and write slowly: the setting's call timeout.
		timeout = max(timeout, resolved.CallTimeout)
	}
	if lease := timeout + suggestionLeaseMargin; lease > suggestionLease {
		if err := s.db.RenewFoundryAutomationSuggestionsLease(ctx, row.OntologyID, row.VersionID, s.workerID, lease); err != nil {
			logger.Warn("renew the lease of automation suggestions failed; it lapses and it is claimed again", "err", err)
			return
		}
	}
	end, cost := s.call(ctx, binding, req, timeout)
	s.charge(ctx, row, cost)
	if ctx.Err() != nil && len(end.Suggestions) == 0 {
		// The worker is stopping before the model answered: the row is
		// left to its lease, and the next worker writes it.
		logger.Info("automation suggestions: stopped before the write ended; it is claimed again")
		return
	}
	s.finish(ctx, row, end)
}

// call makes the write's calls within timeout: one, and up to two more
// after an answer Parse keeps nothing of, or, after a growing wait, a
// refusal a retry may pass. It returns the end to record, and what every
// call cost.
func (s *suggester) call(ctx context.Context, b llm.Binding, req llm.Request, timeout time.Duration) (model.FoundryAutomationSuggestionsEnd, int64) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cost int64
	why := ""
	wait, next := time.Duration(0), s.retryWait
	for range suggestionCalls {
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return model.FoundryAutomationSuggestionsEnd{Failure: "the model took too long"}, cost
			}
		}
		var input json.RawMessage
		var failed error
		for chunk, err := range b.Model.Stream(ctx, req) {
			if err != nil {
				failed = err
				break
			}
			switch c := chunk.(type) {
			case llm.ToolUse:
				if c.Name == suggest.ToolName && input == nil {
					input = c.Input
				}
			case llm.Usage:
				cost += b.Pricer.Micros(c)
			}
		}
		var refused *llm.Error
		wait = 0
		switch {
		case failed != nil && ctx.Err() != nil:
			// Out of time, waiting for the server or its answer: no retry
			// can pass.
			return model.FoundryAutomationSuggestionsEnd{Failure: "the model took too long"}, cost
		case failed != nil && errors.As(failed, &refused) && refused.Retryable():
			why = "the model provider couldn't answer: " + string(refused.Kind)
			wait, next = next, 2*next
			continue
		case failed != nil && errors.As(failed, &refused):
			return model.FoundryAutomationSuggestionsEnd{Failure: "the model provider refused: " + string(refused.Kind)}, cost
		case failed != nil:
			return model.FoundryAutomationSuggestionsEnd{Failure: "the model call failed"}, cost
		}
		kept, err := suggest.Parse(input)
		if err != nil {
			why = "the model's answer wasn't usable"
			log.From(ctx).Info("automation suggestions: an answer Parse kept nothing of", "err", err)
			continue
		}
		raw, err := json.Marshal(kept)
		if err != nil {
			return model.FoundryAutomationSuggestionsEnd{Failure: "the suggestions couldn't be stored"}, cost
		}
		return model.FoundryAutomationSuggestionsEnd{Suggestions: raw}, cost
	}
	return model.FoundryAutomationSuggestionsEnd{Failure: why}, cost
}

// charge enters what a write's calls spent in the workspace's ledger, with
// a bound of its own, whether the write ended, lost its lease or was cut
// short: it was spent, and the month's budget counts it.
func (s *suggester) charge(ctx context.Context, row *model.FoundryAutomationSuggestions, cost int64) {
	if cost == 0 {
		return
	}
	suggestionSpend.Add(float64(cost))
	chargeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := s.db.ChargeFoundryAutomationSuggestions(chargeCtx, row.WorkspaceID, cost); err != nil {
		log.From(ctx).Error("charge automation suggestions failed: the budget misses what they spent", "err", err, "usd_micros", cost)
	}
}

// finish records end, with a bound of its own, so a write that ended as
// the worker stops is kept; a lost lease records nothing.
func (s *suggester) finish(ctx context.Context, row *model.FoundryAutomationSuggestions, end model.FoundryAutomationSuggestionsEnd) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	err := s.db.FinishFoundryAutomationSuggestions(finishCtx, row.OntologyID, row.VersionID, s.workerID, end)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		suggestionWrites.WithLabelValues("lost").Inc()
	case err != nil:
		log.From(ctx).Warn("finish automation suggestions failed; the lease lapses and it is claimed again", "err", err)
	default:
		suggestionWrites.WithLabelValues(string(end.Status())).Inc()
	}
}

// input is what the row's prompt is made of: its ontology, the version's
// schema and questions, the goal and connections of the generation that
// drafted it, and the document sets it reads. ErrNotFound means the
// ontology or its version is gone, errNotCurrent that another version is
// current now.
func (s *suggester) input(ctx context.Context, row *model.FoundryAutomationSuggestions) (suggest.Input, error) {
	o, err := s.db.GetFoundryOntology(ctx, row.WorkspaceID, row.OntologyID)
	switch {
	case err != nil:
		return suggest.Input{}, err
	case o.DeletedAt != nil:
		return suggest.Input{}, foundrystore.ErrNotFound
	case o.CurrentVersionID == nil || *o.CurrentVersionID != row.VersionID:
		return suggest.Input{}, errNotCurrent
	}
	v, err := s.db.GetOntologyVersion(ctx, row.OntologyID, row.VersionID)
	if err != nil {
		return suggest.Input{}, err
	}
	in := suggest.Input{Ontology: o.Name, Version: suggest.VersionOf(v.Format, v.Definition)}
	var connectorIDs []string
	if v.GenerationID != nil {
		g, err := s.db.GetOntologyGeneration(ctx, row.OntologyID, *v.GenerationID)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
		case err != nil:
			return suggest.Input{}, err
		default:
			in.Goal, in.Title = deref(g.Goal), deref(g.Title)
			for _, table := range g.Input.Tables {
				if !slices.Contains(connectorIDs, table.ConnectorID) {
					connectorIDs = append(connectorIDs, table.ConnectorID)
				}
			}
		}
	}
	if len(connectorIDs) > 0 {
		connectors, err := s.db.ListConnectors(ctx, row.WorkspaceID)
		if err != nil {
			return suggest.Input{}, err
		}
		for _, c := range connectors {
			if slices.Contains(connectorIDs, c.ID) {
				in.Connectors = append(in.Connectors, suggest.Connector{Name: c.Name, Kind: c.Kind})
			}
		}
	}
	scope, err := s.db.GetOntologyVersionScope(ctx, row.OntologyID, row.VersionID)
	if err != nil {
		return suggest.Input{}, err
	}
	if len(scope) > 0 {
		sets, err := s.db.ListFoundryDocumentSets(ctx, row.WorkspaceID)
		if err != nil {
			return suggest.Input{}, err
		}
		for _, set := range sets {
			if slices.Contains(scope, set.ID) {
				in.Sets = append(in.Sets, set.DisplayName())
			}
		}
	}
	return in, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
