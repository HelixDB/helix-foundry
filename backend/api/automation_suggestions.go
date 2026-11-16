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

// ListFoundryAutomationSuggestions answers the new automation page with an
// ontology's suggestions, for the callers of the other automation routes:
//   - none without a current version;
//   - the Assistant's, once written for that version (source MODEL);
//   - else the fallback from its competency questions (source QUESTIONS),
//     generating while a write is queued or running and queued less than
//     suggest.StaleAfter ago. A read with no write, or one that failed
//     suggest.RetryAfter ago, queues one, unless the Assistant is off, the
//     workspace's AI setting can't be used, or the workspace queued
//     suggest.PerHour this hour.
//
// The model's absence or failure is never an error: only the caller, the
// ontology and the store's are.
func (s *Service) ListFoundryAutomationSuggestions(ctx context.Context, req *foundryv1.ListFoundryAutomationSuggestionsRequest) (
	*foundryv1.ListFoundryAutomationSuggestionsResponse, error) {
	ws := req.GetWorkspaceId()
	if _, err := s.automationCallerOf(ctx, ws); err != nil {
		return nil, err
	}
	if req.GetOntologyId() == "" {
		return nil, status.Error(codes.InvalidArgument, "ontology_id is required")
	}
	o, err := s.liveFoundryOntology(ctx, ws, req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if o.CurrentVersionID == nil {
		return &foundryv1.ListFoundryAutomationSuggestionsResponse{}, nil
	}
	versionID := *o.CurrentVersionID
	row, err := s.deps.DB.GetFoundryAutomationSuggestions(ctx, o.ID, versionID)
	if err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "read automation suggestions: %v", err)
	}
	if out, ok := modelSuggestions(ctx, row, versionID); ok {
		return out, nil
	}
	v, err := s.deps.DB.GetOntologyVersion(ctx, o.ID, versionID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the current version: %v", err)
	}
	var definition map[string]any
	_ = json.Unmarshal(v.Definition, &definition) // a definition that doesn't read has no questions
	fallback := suggest.Fallback(o.Name, suggest.Questions(definition, suggest.MaxSuggestions))
	if row == nil || row.Status == foundrymodel.FoundryAutomationSuggestionsFailed && time.Since(row.UpdatedAt) > suggest.RetryAfter {
		if row, err = s.queueAutomationSuggestions(ctx, ws, o.ID, versionID); err != nil {
			return nil, err
		}
		if out, ok := modelSuggestions(ctx, row, versionID); ok {
			return out, nil // written meanwhile
		}
	}
	out := &foundryv1.ListFoundryAutomationSuggestionsResponse{VersionId: versionID, Suggestions: suggestionsToProto(fallback)}
	if len(fallback) > 0 {
		out.Source = foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_QUESTIONS
	}
	out.Generating = row != nil && (row.Status == foundrymodel.FoundryAutomationSuggestionsQueued || row.Status == foundrymodel.FoundryAutomationSuggestionsRunning) &&
		time.Since(row.RequestedAt) <= suggest.StaleAfter
	return out, nil
}

// queueAutomationSuggestions queues a write of the version's suggestions
// where one can run, and returns its row; nil, with no error, where none
// can: the Assistant is off, the workspace's AI setting can't be used (no
// provider, its own provider off Pro, or unreadable), or it queued
// suggest.PerHour this hour.
func (s *Service) queueAutomationSuggestions(ctx context.Context, ws, ontologyID, versionID string) (*foundrymodel.FoundryAutomationSuggestions, error) {
	if !s.deps.Cfg.Foundry.Agent.Enabled {
		return nil, nil
	}
	if err := (&ai.Resolver{DB: s.deps.DB, Deployment: s.deps.AI, Plans: s.deps.AIPlans}).Usable(ctx, ws); err != nil {
		log.From(ctx).Debug("automation suggestions: no model to write them", "workspace", ws, "err", err)
		return nil, nil
	}
	row, err := s.deps.DB.QueueFoundryAutomationSuggestions(ctx, ws, ontologyID, versionID, suggest.PerHour, suggest.RetryAfter)
	switch {
	case errors.Is(err, foundrystore.ErrLimitExceeded):
		return nil, nil
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "queue automation suggestions: %v", err)
	}
	return row, nil
}

// modelSuggestions answers with a ready row's suggestions; false for any
// other row, or one whose suggestions don't read (the fallback answers).
func modelSuggestions(ctx context.Context, row *foundrymodel.FoundryAutomationSuggestions, versionID string) (
	*foundryv1.ListFoundryAutomationSuggestionsResponse, bool) {
	if row == nil || row.Status != foundrymodel.FoundryAutomationSuggestionsReady {
		return nil, false
	}
	var kept []suggest.Suggestion
	if err := json.Unmarshal(row.Suggestions, &kept); err != nil || len(kept) == 0 {
		log.From(ctx).Warn("automation suggestions don't read", "ontology", row.OntologyID, "version", versionID, "err", err)
		return nil, false
	}
	return &foundryv1.ListFoundryAutomationSuggestionsResponse{
		Suggestions: suggestionsToProto(kept), VersionId: versionID,
		Source: foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_MODEL,
	}, true
}

func suggestionsToProto(list []suggest.Suggestion) []*foundryv1.FoundryAutomationSuggestion {
	out := make([]*foundryv1.FoundryAutomationSuggestion, 0, len(list))
	for _, s := range list {
		out = append(out, &foundryv1.FoundryAutomationSuggestion{Title: s.Title, Prompt: s.Prompt, Icon: suggestionIcons[s.Icon]})
	}
	return out
}
