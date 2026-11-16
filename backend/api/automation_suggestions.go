package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/automation/suggest"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// suggestionIcons is each suggest.Icon's enum value.
var suggestionIcons = map[suggest.Icon]foundryv1.FoundryAutomationSuggestionIcon{
	suggest.Briefing: foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_BRIEFING,
	suggest.Watch:    foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_WATCH,
	suggest.Check:    foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_CHECK,
	suggest.Alert:    foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_ALERT,
	suggest.Refresh:  foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_REFRESH,
	suggest.Report:   foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_REPORT,
}

// suggestionKinds is each suggest.Kind's enum value.
var suggestionKinds = map[suggest.Kind]foundryv1.FoundryAutomationSuggestionKind{
	suggest.Question:  foundryv1.FoundryAutomationSuggestionKind_FOUNDRY_AUTOMATION_SUGGESTION_KIND_QUESTION,
	suggest.Dashboard: foundryv1.FoundryAutomationSuggestionKind_FOUNDRY_AUTOMATION_SUGGESTION_KIND_DASHBOARD,
	suggest.Ontology:  foundryv1.FoundryAutomationSuggestionKind_FOUNDRY_AUTOMATION_SUGGESTION_KIND_ONTOLOGY,
	suggest.Workflow:  foundryv1.FoundryAutomationSuggestionKind_FOUNDRY_AUTOMATION_SUGGESTION_KIND_WORKFLOW,
}

// suggestionSources is each suggest.Source's enum value.
var suggestionSources = map[suggest.Source]foundryv1.FoundryAutomationSuggestionSource{
	suggest.SourceModel:     foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_MODEL,
	suggest.SourceQuestions: foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_QUESTIONS,
	suggest.SourceSetup:     foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_SETUP,
	suggest.SourceFixed:     foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_FIXED,
}

// ListFoundryAutomationSuggestions answers Founder's new chat and the new
// workflow page with an ontology's suggestions (suggest.Fill), for any
// person of the workspace (personCallerOf: a grantee may not), workflows
// left out where the deployment has none:
//   - without a current version, suggest.Starter's fixed examples;
//   - else each kind filled from the version's competency questions, the
//     ontology's setup (its open audit findings, and the connections and
//     document sets it doesn't read, read on every answer), Founder's
//     suggestions once written for the version, and fixed examples. The
//     answer's source is its first workflow's; it is generating while a
//     write is queued or running and was queued less than
//     suggest.StaleAfter ago. A read with no write, or one that failed
//     suggest.RetryAfter ago, queues one, unless Founder is off, the
//     workspace's AI setting can't be used, or the workspace queued
//     suggest.PerHour this hour.
//
// The model's absence or failure is never an error: only the caller, the
// ontology and the store's are.
func (s *Service) ListFoundryAutomationSuggestions(ctx context.Context, req *foundryv1.ListFoundryAutomationSuggestionsRequest) (
	*foundryv1.ListFoundryAutomationSuggestionsResponse, error) {
	ws := req.GetWorkspaceId()
	if _, err := s.personCallerOf(ctx, ws); err != nil {
		return nil, err
	}
	if req.GetOntologyId() == "" {
		return nil, status.Error(codes.InvalidArgument, "ontology_id is required")
	}
	o, err := s.liveFoundryOntology(ctx, ws, req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	workflows := s.deps.Cfg.Foundry.Automations.Enabled
	if o.CurrentVersionID == nil {
		return suggestionsResponse("", suggest.Starter(o.Name, workflows), false), nil
	}
	versionID := *o.CurrentVersionID
	row, err := s.deps.DB.GetFoundryAutomationSuggestions(ctx, o.ID, versionID)
	if err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "read workflow suggestions: %v", err)
	}
	setup, err := s.automationSuggestionSetup(ctx, ws, o.ID, versionID)
	if err != nil {
		return nil, err
	}
	// A row holds the version's questions, so a poll while it is written
	// never reads the version, which may be large.
	var questions []string
	if row != nil {
		questions = row.Questions
	} else {
		v, err := s.deps.DB.GetOntologyVersion(ctx, o.ID, versionID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read the current version: %v", err)
		}
		var definition map[string]any
		_ = json.Unmarshal(v.Definition, &definition) // a definition that doesn't read has no questions
		questions = suggest.Questions(definition, suggest.MaxQuestions)
	}
	if row == nil || row.Status == foundrymodel.FoundryAutomationSuggestionsFailed && time.Since(row.UpdatedAt) > suggest.RetryAfter {
		if row, err = s.queueAutomationSuggestions(ctx, ws, o.ID, versionID, questions); err != nil {
			return nil, err
		}
	}
	generating := row != nil && (row.Status == foundrymodel.FoundryAutomationSuggestionsQueued || row.Status == foundrymodel.FoundryAutomationSuggestionsRunning) &&
		time.Since(row.RequestedAt) <= suggest.StaleAfter
	return suggestionsResponse(versionID, suggest.Fill(o.Name, questions, writtenSuggestions(ctx, row), setup, workflows), generating), nil
}

// automationSuggestionSetup is what the ontology's current version doesn't
// read or hold yet (suggest.SetupOf): its open audit findings, and the
// workspace's connections and document sets, against the connectors of its
// generation's tables and its scope, read without its definition.
func (s *Service) automationSuggestionSetup(ctx context.Context, ws, ontologyID, versionID string) (suggest.Setup, error) {
	fail := func(err error) (suggest.Setup, error) {
		return suggest.Setup{}, status.Errorf(codes.Internal, "read what the ontology doesn't read yet: %v", err)
	}
	findings, err := s.deps.DB.ListFoundryHealFindings(ctx, ontologyID)
	if err != nil {
		return fail(err)
	}
	sources, err := s.deps.DB.GetOntologyVersionSources(ctx, ontologyID, versionID)
	if err != nil {
		return fail(err)
	}
	read := []string{}
	if sources.GenerationID != nil {
		g, err := s.deps.DB.GetOntologyGeneration(ctx, ontologyID, *sources.GenerationID)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
		case err != nil:
			return fail(err)
		default:
			for _, table := range g.Input.Tables {
				read = append(read, table.ConnectorID)
			}
		}
	}
	connectors, err := s.deps.DB.ListConnectors(ctx, ws)
	if err != nil {
		return fail(err)
	}
	sets, err := s.deps.DB.ListFoundryDocumentSets(ctx, ws)
	if err != nil {
		return fail(err)
	}
	return suggest.SetupOf(findings, connectors, read, sets, sources.DocumentSetIDs), nil
}

// queueAutomationSuggestions queues a write of the version's suggestions,
// with its questions, where one can run, and returns its row; nil, with no
// error, where none can: Founder is off, the workspace's AI setting
// can't be used (no provider, its own provider off Pro, or unreadable), or
// it queued suggest.PerHour this hour.
func (s *Service) queueAutomationSuggestions(ctx context.Context, ws, ontologyID, versionID string, questions []string) (
	*foundrymodel.FoundryAutomationSuggestions, error) {
	if !s.deps.Cfg.Foundry.Agent.Enabled {
		return nil, nil
	}
	if err := (&ai.Resolver{DB: s.deps.DB, Deployment: s.deps.AI, Plans: s.deps.AIPlans}).Usable(ctx, ws); err != nil {
		log.From(ctx).Debug("automation suggestions: no model to write them", "workspace", ws, "err", err)
		return nil, nil
	}
	row, err := s.deps.DB.QueueFoundryAutomationSuggestions(ctx, ws, ontologyID, versionID, questions, suggest.PerHour, suggest.RetryAfter)
	switch {
	case errors.Is(err, foundrystore.ErrLimitExceeded):
		return nil, nil
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "queue workflow suggestions: %v", err)
	}
	return row, nil
}

// writtenSuggestions are a ready row's suggestions; none for any other row,
// or one whose suggestions don't read. An item of no kind, or one that
// doesn't pass suggest.Suggestion.Clean (written before kinds), is
// dropped.
func writtenSuggestions(ctx context.Context, row *foundrymodel.FoundryAutomationSuggestions) []suggest.Suggestion {
	if row == nil || row.Status != foundrymodel.FoundryAutomationSuggestionsReady {
		return nil
	}
	var stored []suggest.Suggestion
	if err := json.Unmarshal(row.Suggestions, &stored); err != nil {
		log.From(ctx).Warn("automation suggestions don't read", "ontology", row.OntologyID, "version", row.VersionID, "err", err)
		return nil
	}
	kept := []suggest.Suggestion{}
	for _, s := range stored {
		if clean, err := s.Clean(); err == nil {
			kept = append(kept, clean)
		}
	}
	return kept
}

// suggestionsResponse answers with list, its source its first workflow's.
func suggestionsResponse(versionID string, list []suggest.Suggestion, generating bool) *foundryv1.ListFoundryAutomationSuggestionsResponse {
	out := &foundryv1.ListFoundryAutomationSuggestionsResponse{VersionId: versionID, Generating: generating,
		Suggestions: make([]*foundryv1.FoundryAutomationSuggestion, 0, len(list))}
	for _, s := range list {
		out.Suggestions = append(out.Suggestions, &foundryv1.FoundryAutomationSuggestion{Title: s.Title, Prompt: s.Prompt, Icon: suggestionIcons[s.Icon],
			Kind: suggestionKinds[s.Kind], Topic: s.Topic, Source: suggestionSources[s.Source]})
		if s.Kind == suggest.Workflow && out.Source == foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_UNSPECIFIED {
			out.Source = suggestionSources[s.Source]
		}
	}
	return out
}
