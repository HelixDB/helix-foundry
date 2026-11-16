package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/automation/suggest"
	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// suggestionStore is one ontology, its current version and the version's
// suggestions row, with the workspace's AI setting.
type suggestionStore struct {
	foundrystore.Unimplemented
	ontology *foundrymodel.FoundryOntology
	version  *foundrymodel.OntologyVersion
	row      *foundrymodel.FoundryAutomationSuggestions
	rowErr   error
	// queued counts the queue calls; queueErr refuses them.
	queued   int
	queueErr error
	// versionReads counts the reads of the version.
	versionReads int
	// active is the workspace's own AI setting; nil runs the default.
	active *foundrymodel.FoundryAIConfig
}

func (s *suggestionStore) GetFoundryOntology(_ context.Context, ws, id string) (*foundrymodel.FoundryOntology, error) {
	if ws != "ws-1" || s.ontology == nil || id != s.ontology.ID {
		return nil, foundrystore.ErrNotFound
	}
	o := *s.ontology
	return &o, nil
}

func (s *suggestionStore) GetOntologyVersion(_ context.Context, ontologyID, id string) (*foundrymodel.OntologyVersion, error) {
	s.versionReads++
	if s.version == nil || ontologyID != s.ontology.ID || id != s.version.ID {
		return nil, foundrystore.ErrNotFound
	}
	return s.version, nil
}

func (s *suggestionStore) GetFoundryAutomationSuggestions(_ context.Context, ontologyID, versionID string) (*foundrymodel.FoundryAutomationSuggestions, error) {
	if s.rowErr != nil {
		return nil, s.rowErr
	}
	if s.row == nil || s.row.OntologyID != ontologyID || s.row.VersionID != versionID {
		return nil, foundrystore.ErrNotFound
	}
	row := *s.row
	return &row, nil
}

func (s *suggestionStore) QueueFoundryAutomationSuggestions(_ context.Context, ws, ontologyID, versionID string, questions []string, perHour int,
	retryAfter time.Duration) (*foundrymodel.FoundryAutomationSuggestions, error) {
	s.queued++
	if s.queueErr != nil {
		return nil, s.queueErr
	}
	if ws != "ws-1" || perHour != suggest.PerHour || retryAfter != suggest.RetryAfter {
		return nil, errors.New("queued for the wrong workspace or limits")
	}
	s.row = &foundrymodel.FoundryAutomationSuggestions{OntologyID: ontologyID, VersionID: versionID, WorkspaceID: ws, Questions: questions,
		Status: foundrymodel.FoundryAutomationSuggestionsQueued, RequestedAt: time.Now(), UpdatedAt: time.Now()}
	return s.row, nil
}

func (s *suggestionStore) GetActiveFoundryAIConfig(context.Context, string) (*foundrymodel.FoundryAIConfig, error) {
	if s.active == nil {
		return nil, foundrystore.ErrNotFound
	}
	return s.active, nil
}

// contractsDefinition is an agent bundle with three competency questions.
func contractsDefinition(t *testing.T) json.RawMessage {
	t.Helper()
	items := map[string]any{}
	for id, q := range map[string]string{"CQ1": "Which customers are under contract?", "CQ2": "How many invoices are overdue?",
		"CQ3": "When does each term end?"} {
		items["cq."+id] = map[string]any{"toml": "[cq." + id + "]\nquestion = \"" + q + "\"\n"}
	}
	raw, err := json.Marshal(map[string]any{"format": "agent_bundle", "ontology_toml": "[types.Contract]\n",
		"draft": map[string]any{"draft": map[string]any{"items": items}}})
	require.NoError(t, err)
	return raw
}

// suggestionService is hosted Foundry with automations and the Assistant
// on, Helix's Claude the workspace's model, and Customer contracts' current
// version holding three competency questions.
func suggestionService(t *testing.T) (*Service, *suggestionStore) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Foundry.Automations.Enabled, cfg.Foundry.Agent.Enabled = true, true
	store := &suggestionStore{
		ontology: &foundrymodel.FoundryOntology{ID: "ont-1", WorkspaceID: "ws-1", Name: "Customer contracts", CurrentVersionID: new("ver-1")},
		version: &foundrymodel.OntologyVersion{ID: "ver-1", OntologyID: "ont-1", Format: foundrymodel.OntologyFormatAgentBundle,
			Definition: contractsDefinition(t)},
	}
	return &Service{deps: &runtimeDeps{Cfg: &cfg, DB: store, People: dashboardPeople{"user-ada": ports.RoleMember}, AI: hostedAI(t)}}, store
}

func listSuggestions(t *testing.T, s *Service) *foundryv1.ListFoundryAutomationSuggestionsResponse {
	t.Helper()
	out, err := s.ListFoundryAutomationSuggestions(site("user-ada"), &foundryv1.ListFoundryAutomationSuggestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
	require.NoError(t, err)
	return out
}

func suggestionTitles(out *foundryv1.ListFoundryAutomationSuggestionsResponse) []string {
	titles := []string{}
	for _, s := range out.GetSuggestions() {
		titles = append(titles, s.GetTitle())
	}
	return titles
}

var fallbackTitles = []string{"Customers are under contract", "Invoices are overdue", "When does each term end"}

const questionsSource = foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_QUESTIONS

func TestAutomationSuggestionsFirstReadQueuesAWrite(t *testing.T) {
	s, store := suggestionService(t)
	out := listSuggestions(t, s)
	assert.Equal(t, fallbackTitles, suggestionTitles(out), "the competency questions answer meanwhile")
	assert.Equal(t, questionsSource, out.GetSource())
	assert.True(t, out.GetGenerating())
	assert.Equal(t, "ver-1", out.GetVersionId())
	assert.Equal(t, `Every Monday, answer: "Which customers are under contract?" from Customer contracts, and email me the answer.`,
		out.GetSuggestions()[0].GetPrompt())
	assert.Equal(t, foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_REPORT, out.GetSuggestions()[0].GetIcon())
	assert.Equal(t, 1, store.queued)
	assert.Equal(t, 1, store.versionReads)

	// Polls while it is queued, then running, wait without queueing again,
	// and answer from the questions the row holds, never reading the
	// version again.
	out = listSuggestions(t, s)
	assert.True(t, out.GetGenerating())
	assert.Equal(t, fallbackTitles, suggestionTitles(out))
	store.row.Status = foundrymodel.FoundryAutomationSuggestionsRunning
	assert.True(t, listSuggestions(t, s).GetGenerating())
	assert.Equal(t, 1, store.queued)
	assert.Equal(t, 1, store.versionReads, "the version is read once, when queued")

	// Queued past suggest.StaleAfter, the worker is taken to be down.
	store.row.RequestedAt = time.Now().Add(-suggest.StaleAfter - time.Second)
	out = listSuggestions(t, s)
	assert.False(t, out.GetGenerating())
	assert.Equal(t, fallbackTitles, suggestionTitles(out))
	assert.Equal(t, 1, store.queued)
}

func TestAutomationSuggestionsReady(t *testing.T) {
	s, store := suggestionService(t)
	store.row = &foundrymodel.FoundryAutomationSuggestions{OntologyID: "ont-1", VersionID: "ver-1", Status: foundrymodel.FoundryAutomationSuggestionsReady,
		Questions: []string{"Which customers are under contract?"},
		Suggestions: json.RawMessage(`[{"title":"Renewal radar","prompt":"Every Monday at 9am, list renewals due in 30 days and email me.","icon":"watch"},
		  {"title":"Invoice watch","prompt":"Every day, email me new overdue invoices.","icon":"alert"}]`)}
	out := listSuggestions(t, s)
	assert.Equal(t, foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_MODEL, out.GetSource())
	assert.False(t, out.GetGenerating())
	assert.Equal(t, []string{"Renewal radar", "Invoice watch"}, suggestionTitles(out))
	assert.Equal(t, foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_WATCH, out.GetSuggestions()[0].GetIcon())
	assert.Equal(t, foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_ALERT, out.GetSuggestions()[1].GetIcon())
	assert.Equal(t, "ver-1", out.GetVersionId())
	assert.Zero(t, store.versionReads, "ready, the version isn't read")
	assert.Zero(t, store.queued)

	// Every icon has its enum value.
	for i, icon := range suggest.Icons {
		assert.EqualValues(t, i+1, suggestionIcons[icon], icon)
	}

	// Suggestions that don't read fall back.
	store.row.Suggestions = json.RawMessage(`{"not":"a list"}`)
	out = listSuggestions(t, s)
	assert.Equal(t, questionsSource, out.GetSource())
	assert.False(t, out.GetGenerating())
	assert.Equal(t, []string{"Customers are under contract"}, suggestionTitles(out), "from the questions the row holds")
	assert.Zero(t, store.versionReads)
}

func TestAutomationSuggestionsAfterAFailure(t *testing.T) {
	s, store := suggestionService(t)
	failed := &foundrymodel.FoundryAutomationSuggestions{OntologyID: "ont-1", VersionID: "ver-1", Status: foundrymodel.FoundryAutomationSuggestionsFailed,
		Questions:   []string{"Which customers are under contract?", "How many invoices are overdue?", "When does each term end?"},
		RequestedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now().Add(-time.Minute)}
	store.row = failed
	out := listSuggestions(t, s)
	assert.Equal(t, fallbackTitles, suggestionTitles(out))
	assert.False(t, out.GetGenerating(), "a failure stands for an hour")
	assert.Zero(t, store.queued)

	failed.UpdatedAt = time.Now().Add(-suggest.RetryAfter - time.Minute)
	out = listSuggestions(t, s)
	assert.True(t, out.GetGenerating(), "an hour on, a read queues it again")
	assert.Equal(t, 1, store.queued)
	assert.Zero(t, store.versionReads, "with the questions the row holds")
	assert.Equal(t, failed.Questions, store.row.Questions)
}

// The fallback reads past the first four questions for four titles: two
// questions that make one title leave room for a fifth.
func TestAutomationSuggestionsFallbackReadsPastRepeatedTitles(t *testing.T) {
	s, store := suggestionService(t)
	items := map[string]any{}
	for id, q := range map[string]string{"CQ1": "Which customers are under contract?", "CQ2": "What customers are under contract?",
		"CQ3": "How many invoices are overdue?", "CQ4": "When does each term end?", "CQ5": "Which contracts renew this quarter?"} {
		items["cq."+id] = map[string]any{"toml": "[cq." + id + "]\nquestion = \"" + q + "\"\n"}
	}
	raw, err := json.Marshal(map[string]any{"format": "agent_bundle", "draft": map[string]any{"draft": map[string]any{"items": items}}})
	require.NoError(t, err)
	store.version.Definition = raw
	out := listSuggestions(t, s)
	assert.Equal(t, []string{"Customers are under contract", "Invoices are overdue", "When does each term end", "Contracts renew this quarter"},
		suggestionTitles(out))
	assert.Len(t, store.row.Questions, 5, "the row keeps every question the fallback may need")
}

// Where no write can run, the fallback answers and nothing is queued.
func TestAutomationSuggestionsWithoutAModel(t *testing.T) {
	for name, setup := range map[string]func(*Service, *suggestionStore){
		"no AI provider": func(s *Service, _ *suggestionStore) { s.deps.AI = deployment(t, aiConfig(modeLocal, true)) },
		"own key off Pro": func(_ *Service, store *suggestionStore) {
			store.active = &foundrymodel.FoundryAIConfig{ID: "cfg-1", WorkspaceID: "ws-1", Kind: "anthropic"}
		},
		"the Assistant off": func(s *Service, _ *suggestionStore) { s.deps.Cfg.Foundry.Agent.Enabled = false },
		"20 this hour":      func(_ *Service, store *suggestionStore) { store.queueErr = foundrystore.ErrLimitExceeded },
	} {
		t.Run(name, func(t *testing.T) {
			s, store := suggestionService(t)
			setup(s, store)
			out := listSuggestions(t, s)
			assert.Equal(t, fallbackTitles, suggestionTitles(out))
			assert.Equal(t, questionsSource, out.GetSource())
			assert.False(t, out.GetGenerating())
			assert.Nil(t, store.row, "nothing queued")
		})
	}
}

func TestAutomationSuggestionsWithoutQuestions(t *testing.T) {
	s, store := suggestionService(t)
	store.ontology.CurrentVersionID = nil
	out := listSuggestions(t, s)
	assert.Empty(t, out.GetSuggestions())
	assert.Equal(t, foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_UNSPECIFIED, out.GetSource())
	assert.False(t, out.GetGenerating())
	assert.Empty(t, out.GetVersionId(), "no current version, no suggestions")
	assert.Zero(t, store.queued)

	// A Definition v1 has no questions: none until the Assistant writes some.
	s, store = suggestionService(t)
	store.version.Format, store.version.Definition = foundrymodel.OntologyFormatFoundryV1, json.RawMessage(`{"entities":[]}`)
	out = listSuggestions(t, s)
	assert.Empty(t, out.GetSuggestions())
	assert.Equal(t, foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_UNSPECIFIED, out.GetSource())
	assert.True(t, out.GetGenerating())
	assert.Equal(t, "ver-1", out.GetVersionId())
}

func TestAutomationSuggestionsRefusals(t *testing.T) {
	s, store := suggestionService(t)
	list := func(ctx context.Context, ws, ontology string) error {
		_, err := s.ListFoundryAutomationSuggestions(ctx, &foundryv1.ListFoundryAutomationSuggestionsRequest{WorkspaceId: ws, OntologyId: ontology})
		return err
	}
	assert.Equal(t, codes.InvalidArgument, status.Code(list(site("user-ada"), "ws-1", "")))
	assert.Equal(t, codes.InvalidArgument, status.Code(list(site("user-ada"), "", "ont-1")))
	assert.Equal(t, codes.NotFound, status.Code(list(site("user-ada"), "ws-1", "ont-9")))
	assert.Equal(t, codes.NotFound, status.Code(list(site("user-ada"), "ws-2", "ont-1")), "another workspace's ontology")
	deleted := time.Now()
	store.ontology.DeletedAt = &deleted
	assert.Equal(t, codes.NotFound, status.Code(list(site("user-ada"), "ws-1", "ont-1")), "a deleted ontology")
	store.ontology.DeletedAt = nil
	require.NoError(t, list(throughMCP("user-ada"), "ws-1", "ont-1"), "a person's agent reads them too")

	for name, ctx := range map[string]context.Context{
		"a key":    asActor(ports.Actor{Source: ports.SourceMCP, ServiceCredentialID: "key-1", WorkspaceID: "ws-1"}),
		"no one":   context.Background(),
		"an event": asActor(ports.Actor{Source: ports.SourceEventSource, WorkspaceID: "ws-1"}),
		"a grantee": access.WithCaller(asActor(ports.Actor{Source: ports.SourceGrantee, UserID: "user-ada", WorkspaceID: "ws-1"}),
			access.Grantee{WorkspaceID: "ws-1", Principal: access.Principal{Kind: access.PrincipalUser, ID: "user-ada"}}),
	} {
		assert.Equal(t, codes.PermissionDenied, status.Code(list(ctx, "ws-1", "ont-1")), name)
	}
	s.deps.Cfg.Foundry.Automations.Enabled = false
	err := list(site("user-ada"), "ws-1", "ont-1")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Equal(t, "FOUNDRY_AUTOMATIONS_DISABLED", reasonOf(err))
	s.deps.Cfg.Foundry.Automations.Enabled = true

	// The store failing is the only other error.
	store.rowErr = errors.New("down")
	assert.Equal(t, codes.Internal, status.Code(list(site("user-ada"), "ws-1", "ont-1")))
	store.rowErr, store.row, store.queueErr = nil, nil, errors.New("down")
	assert.Equal(t, codes.Unavailable, status.Code(list(site("user-ada"), "ws-1", "ont-1")))
	store.queueErr, store.version = nil, nil
	assert.Equal(t, codes.Internal, status.Code(list(site("user-ada"), "ws-1", "ont-1")), "the current version can't be read")
}
