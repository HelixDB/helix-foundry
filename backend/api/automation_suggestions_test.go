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
// suggestions row, with the workspace's AI setting, connectors, sets and the
// ontology's audit findings.
type suggestionStore struct {
	foundrystore.Unimplemented
	ontology *foundrymodel.FoundryOntology
	version  *foundrymodel.OntologyVersion
	row      *foundrymodel.FoundryAutomationSuggestions
	rowErr   error
	// queued counts the queue calls; queueErr refuses them.
	queued   int
	queueErr error
	// versionReads counts the reads of the version's definition.
	versionReads int
	// active is the workspace's own AI setting; nil runs the default.
	active *foundrymodel.FoundryAIConfig
	// The setup: the version's generation (gen-1, its tables' connectors),
	// the workspace's connectors and sets, and the ontology's findings;
	// setupErr fails the read named (and "version", the definition's).
	generation *foundrymodel.OntologyGeneration
	connectors []*foundrymodel.Connector
	sets       []*foundrymodel.FoundryDocumentSet
	findings   []*foundrymodel.FoundryHealFinding
	setupErr   map[string]error
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
	if err := s.setupErr["version"]; err != nil {
		return nil, err
	}
	if s.version == nil || ontologyID != s.ontology.ID || id != s.version.ID {
		return nil, foundrystore.ErrNotFound
	}
	return s.version, nil
}

func (s *suggestionStore) GetOntologyVersionSources(_ context.Context, ontologyID, id string) (*foundrymodel.OntologyVersionSources, error) {
	if err := s.setupErr["sources"]; err != nil {
		return nil, err
	}
	if s.version == nil || ontologyID != s.ontology.ID || id != s.version.ID {
		return nil, foundrystore.ErrNotFound
	}
	return &foundrymodel.OntologyVersionSources{GenerationID: s.version.GenerationID, DocumentSetIDs: *s.version.DocumentSetIDs}, nil
}

func (s *suggestionStore) GetOntologyGeneration(_ context.Context, ontologyID, id string) (*foundrymodel.OntologyGeneration, error) {
	if err := s.setupErr["generation"]; err != nil {
		return nil, err
	}
	if s.generation == nil || ontologyID != s.ontology.ID || id != s.generation.ID {
		return nil, foundrystore.ErrNotFound
	}
	return s.generation, nil
}

func (s *suggestionStore) ListConnectors(_ context.Context, ws string) ([]*foundrymodel.Connector, error) {
	if ws != "ws-1" {
		return nil, errors.New("another workspace's connectors")
	}
	return s.connectors, s.setupErr["connectors"]
}

func (s *suggestionStore) ListFoundryDocumentSets(_ context.Context, ws string) ([]*foundrymodel.FoundryDocumentSet, error) {
	if ws != "ws-1" {
		return nil, errors.New("another workspace's sets")
	}
	return s.sets, s.setupErr["sets"]
}

func (s *suggestionStore) ListFoundryHealFindings(_ context.Context, ontologyID string) ([]*foundrymodel.FoundryHealFinding, error) {
	if ontologyID != s.ontology.ID {
		return nil, errors.New("another ontology's findings")
	}
	return s.findings, s.setupErr["findings"]
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

// suggestionService is hosted Foundry with automations and Founder
// on, Helix's Claude the workspace's model, and Customer contracts' current
// version holding three competency questions, drafted from Stripe and
// reading the Contracts set; the workspace also has a Shop database and a
// Notes set it doesn't read, and nothing is found by an audit.
func suggestionService(t *testing.T) (*Service, *suggestionStore) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Foundry.Automations.Enabled, cfg.Foundry.Agent.Enabled = true, true
	store := &suggestionStore{
		ontology: &foundrymodel.FoundryOntology{ID: "ont-1", WorkspaceID: "ws-1", Name: "Customer contracts", CurrentVersionID: new("ver-1")},
		version: &foundrymodel.OntologyVersion{ID: "ver-1", OntologyID: "ont-1", Format: foundrymodel.OntologyFormatAgentBundle,
			Definition: contractsDefinition(t), GenerationID: new("gen-1"), DocumentSetIDs: &[]string{"set-contracts"}},
		generation: &foundrymodel.OntologyGeneration{ID: "gen-1", Input: foundrymodel.OntologyGenerationInput{
			Tables: []foundrymodel.OntologyGenerationTable{{ConnectorID: "con-stripe", Table: "customers"}}}},
		connectors: []*foundrymodel.Connector{{ID: "con-stripe", Name: "Stripe", Kind: "stripe"}, {ID: "con-shop", Name: "Shop database", Kind: "postgres"}},
		sets: []*foundrymodel.FoundryDocumentSet{{ID: "set-contracts", Name: "Contracts", Documents: 3},
			{ID: "set-notes", Name: "Notes", Documents: 5}},
		setupErr: map[string]error{},
	}
	return &Service{deps: &runtimeDeps{Cfg: &cfg, DB: store, People: dashboardPeople{"user-ada": ports.RoleMember}, AI: hostedAI(t)}}, store
}

func listSuggestions(t *testing.T, s *Service) *foundryv1.ListFoundryAutomationSuggestionsResponse {
	t.Helper()
	out, err := s.ListFoundryAutomationSuggestions(site("user-ada"), &foundryv1.ListFoundryAutomationSuggestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
	require.NoError(t, err)
	return out
}

// ofKind is the answer's suggestions of kind.
func ofKind(out *foundryv1.ListFoundryAutomationSuggestionsResponse, kind foundryv1.FoundryAutomationSuggestionKind) []*foundryv1.FoundryAutomationSuggestion {
	list := []*foundryv1.FoundryAutomationSuggestion{}
	for _, s := range out.GetSuggestions() {
		if s.GetKind() == kind {
			list = append(list, s)
		}
	}
	return list
}

func suggestionTitles(list []*foundryv1.FoundryAutomationSuggestion) []string {
	titles := []string{}
	for _, s := range list {
		titles = append(titles, s.GetTitle())
	}
	return titles
}

func suggestionSourcesOf(list []*foundryv1.FoundryAutomationSuggestion) []foundryv1.FoundryAutomationSuggestionSource {
	out := []foundryv1.FoundryAutomationSuggestionSource{}
	for _, s := range list {
		out = append(out, s.GetSource())
	}
	return out
}

const (
	kindQuestion  = foundryv1.FoundryAutomationSuggestionKind_FOUNDRY_AUTOMATION_SUGGESTION_KIND_QUESTION
	kindDashboard = foundryv1.FoundryAutomationSuggestionKind_FOUNDRY_AUTOMATION_SUGGESTION_KIND_DASHBOARD
	kindOntology  = foundryv1.FoundryAutomationSuggestionKind_FOUNDRY_AUTOMATION_SUGGESTION_KIND_ONTOLOGY
	kindWorkflow  = foundryv1.FoundryAutomationSuggestionKind_FOUNDRY_AUTOMATION_SUGGESTION_KIND_WORKFLOW

	sourceModel     = foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_MODEL
	questionsSource = foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_QUESTIONS
	sourceSetup     = foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_SETUP
	sourceFixed     = foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_FIXED
)

var fallbackTitles = []string{"Customers are under contract", "Invoices are overdue", "When does each term end", "Monday brief"}

// allKinds is the kinds of a full answer, in order.
var allKinds = []foundryv1.FoundryAutomationSuggestionKind{kindQuestion, kindQuestion, kindDashboard, kindDashboard, kindOntology, kindOntology,
	kindWorkflow, kindWorkflow, kindWorkflow, kindWorkflow}

func kindsOf(out *foundryv1.ListFoundryAutomationSuggestionsResponse) []foundryv1.FoundryAutomationSuggestionKind {
	kinds := []foundryv1.FoundryAutomationSuggestionKind{}
	for _, s := range out.GetSuggestions() {
		kinds = append(kinds, s.GetKind())
	}
	return kinds
}

func TestAutomationSuggestionsFirstReadQueuesAWrite(t *testing.T) {
	s, store := suggestionService(t)
	out := listSuggestions(t, s)
	assert.Equal(t, allKinds, kindsOf(out), "every kind has its count meanwhile")
	assert.Equal(t, fallbackTitles, suggestionTitles(ofKind(out, kindWorkflow)), "the competency questions answer, then fixed examples")
	assert.Equal(t, questionsSource, out.GetSource(), "the first workflow's")
	assert.True(t, out.GetGenerating())
	assert.Equal(t, "ver-1", out.GetVersionId())
	first := ofKind(out, kindWorkflow)[0]
	assert.Equal(t, `Every Monday, answer: "Which customers are under contract?" from Customer contracts, and email me the answer.`, first.GetPrompt())
	assert.Equal(t, foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_REPORT, first.GetIcon())
	assert.Equal(t, "Workflow", first.GetTopic())
	question := out.GetSuggestions()[0]
	assert.Equal(t, "Which customers are under contract?", question.GetPrompt())
	assert.Equal(t, "Question", question.GetTopic())
	assert.Equal(t, questionsSource, question.GetSource())
	assert.Equal(t, foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_UNSPECIFIED, question.GetIcon())
	assert.Equal(t, "Build me a dashboard that answers: When does each term end?", ofKind(out, kindDashboard)[0].GetPrompt())
	assert.Equal(t, 1, store.queued)
	assert.Equal(t, 1, store.versionReads)

	// Polls while it is queued, then running, wait without queueing again,
	// and answer from the questions the row holds, never reading the
	// version's definition again.
	out = listSuggestions(t, s)
	assert.True(t, out.GetGenerating())
	assert.Equal(t, fallbackTitles, suggestionTitles(ofKind(out, kindWorkflow)))
	store.row.Status = foundrymodel.FoundryAutomationSuggestionsRunning
	assert.True(t, listSuggestions(t, s).GetGenerating())
	assert.Equal(t, 1, store.queued)
	assert.Equal(t, 1, store.versionReads, "the version is read once, when queued")

	// Queued past suggest.StaleAfter, the worker is taken to be down.
	store.row.RequestedAt = time.Now().Add(-suggest.StaleAfter - time.Second)
	out = listSuggestions(t, s)
	assert.False(t, out.GetGenerating())
	assert.Equal(t, fallbackTitles, suggestionTitles(ofKind(out, kindWorkflow)))
	assert.Equal(t, 1, store.queued)
}

// readyRow is a ready row holding a dashboard, a change, two workflows, an
// item of no kind and a dashboard that doesn't say so.
func readyRow() *foundrymodel.FoundryAutomationSuggestions {
	return &foundrymodel.FoundryAutomationSuggestions{OntologyID: "ont-1", VersionID: "ver-1", Status: foundrymodel.FoundryAutomationSuggestionsReady,
		Questions: []string{"Which customers are under contract?"},
		Suggestions: json.RawMessage(`[
		  {"kind":"dashboard","title":"Revenue overview","prompt":"Build me a dashboard of revenue by month.","topic":"Revenue"},
		  {"kind":"ontology","title":"Renewal dates","prompt":"Draft a new version that adds a renewal date to Contract.","topic":"Renewals"},
		  {"kind":"workflow","title":"Renewal radar","prompt":"Every Monday at 9am, list renewals due in 30 days and email me.","topic":"Renewals","icon":"watch"},
		  {"kind":"workflow","title":"Invoice watch","prompt":"Every day, email me new overdue invoices.","topic":"Invoices","icon":"alert"},
		  {"title":"Old","prompt":"Every day, email me.","icon":"report"},
		  {"kind":"dashboard","title":"Unsaid","prompt":"Show me revenue.","topic":"Revenue"}]`)}
}

func TestAutomationSuggestionsReady(t *testing.T) {
	s, store := suggestionService(t)
	store.connectors, store.sets = nil, nil // nothing unread
	store.row = readyRow()
	out := listSuggestions(t, s)
	assert.Equal(t, sourceModel, out.GetSource())
	assert.False(t, out.GetGenerating())
	assert.Equal(t, allKinds, kindsOf(out))
	assert.Equal(t, []string{"Customers are under contract", "What it holds"}, suggestionTitles(ofKind(out, kindQuestion)),
		"the row's questions, then fixed")
	assert.Equal(t, []foundryv1.FoundryAutomationSuggestionSource{questionsSource, sourceFixed}, suggestionSourcesOf(ofKind(out, kindQuestion)))
	assert.Equal(t, []string{"Revenue overview", "Customers are under contract"}, suggestionTitles(ofKind(out, kindDashboard)),
		"the model's, then from the questions; the one that doesn't say dashboard is dropped")
	assert.Equal(t, []string{"Renewal dates", "Add missing details"}, suggestionTitles(ofKind(out, kindOntology)))
	workflows := ofKind(out, kindWorkflow)
	assert.Equal(t, []string{"Renewal radar", "Invoice watch", "Customers are under contract", "Monday brief"}, suggestionTitles(workflows),
		"the item of no kind is dropped")
	assert.Equal(t, []foundryv1.FoundryAutomationSuggestionSource{sourceModel, sourceModel, questionsSource, sourceFixed}, suggestionSourcesOf(workflows))
	assert.Equal(t, foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_WATCH, workflows[0].GetIcon())
	assert.Equal(t, foundryv1.FoundryAutomationSuggestionIcon_FOUNDRY_AUTOMATION_SUGGESTION_ICON_ALERT, workflows[1].GetIcon())
	assert.Equal(t, "Renewals", workflows[0].GetTopic())
	assert.Equal(t, "ver-1", out.GetVersionId())
	assert.Zero(t, store.versionReads, "ready, the version isn't read")
	assert.Zero(t, store.queued)

	// Every icon, kind and source has its enum value.
	for i, icon := range suggest.Icons {
		assert.EqualValues(t, i+1, suggestionIcons[icon], icon)
	}
	for i, kind := range suggest.Kinds {
		assert.EqualValues(t, i+1, suggestionKinds[kind], kind)
	}
	for i, source := range []suggest.Source{suggest.SourceModel, suggest.SourceQuestions, suggest.SourceSetup, suggest.SourceFixed} {
		assert.EqualValues(t, i+1, suggestionSources[source], source)
	}

	// Suggestions that don't read fall back.
	store.row.Suggestions = json.RawMessage(`{"not":"a list"}`)
	out = listSuggestions(t, s)
	assert.Equal(t, questionsSource, out.GetSource())
	assert.False(t, out.GetGenerating())
	assert.Equal(t, []string{"Customers are under contract", "Monday brief", "Fresh data daily", "Load check"}, suggestionTitles(ofKind(out, kindWorkflow)),
		"from the questions the row holds")
	assert.Zero(t, store.versionReads)

	// A row of items written before kinds keeps none: fallbacks answer.
	store.row.Suggestions = json.RawMessage(`[{"title":"Renewal radar","prompt":"Every Monday, email me renewals.","icon":"watch"}]`)
	out = listSuggestions(t, s)
	assert.Equal(t, questionsSource, out.GetSource())
	assert.Len(t, out.GetSuggestions(), suggest.MaxSuggestions)
}

// A write a read queues may have ended meanwhile: the queue returns its row.
func TestAutomationSuggestionsWrittenMeanwhile(t *testing.T) {
	s, store := suggestionService(t)
	store.row = readyRow()
	store.row.Status, store.row.UpdatedAt = foundrymodel.FoundryAutomationSuggestionsFailed, time.Now().Add(-2*suggest.RetryAfter)
	ready := readyRow()
	s.deps.DB = &queueReturns{suggestionStore: store, row: ready}
	out := listSuggestions(t, s)
	assert.Equal(t, sourceModel, out.GetSource())
	assert.False(t, out.GetGenerating())
}

// queueReturns answers a queue with row.
type queueReturns struct {
	*suggestionStore
	row *foundrymodel.FoundryAutomationSuggestions
}

func (q *queueReturns) QueueFoundryAutomationSuggestions(context.Context, string, string, string, []string, int, time.Duration) (
	*foundrymodel.FoundryAutomationSuggestions, error) {
	return q.row, nil
}

func TestAutomationSuggestionsFromSetup(t *testing.T) {
	s, store := suggestionService(t)
	out := listSuggestions(t, s)
	changes := ofKind(out, kindOntology)
	assert.Equal(t, []foundryv1.FoundryAutomationSuggestionSource{sourceSetup, sourceSetup}, suggestionSourcesOf(changes))
	assert.Equal(t, "Draft a new version of Customer contracts that also reads Shop database.", changes[0].GetPrompt())
	assert.Equal(t, "New source", changes[0].GetTopic())
	assert.Equal(t, "Draft a new version of Customer contracts that also reads the documents in Notes.", changes[1].GetPrompt())
	assert.Equal(t, "New documents", changes[1].GetTopic())

	// Open "unmatched" findings come first, most claims first.
	store.findings = []*foundrymodel.FoundryHealFinding{
		{Kind: "unmatched", Status: foundrymodel.FoundryHealOpen, Support: 3, Finding: json.RawMessage(`{"signal":{"canonical":"works_with"}}`)},
		{Kind: "unmatched", Status: foundrymodel.FoundryHealOpen, Support: 12, Finding: json.RawMessage(`{"signal":{"canonical":"supplier_of"}}`)},
	}
	store.row = readyRow() // the model's change comes after
	changes = ofKind(listSuggestions(t, s), kindOntology)
	assert.Equal(t, "Draft a new version of Customer contracts with a link for “supplier of”, which 12 claims in your documents state.", changes[0].GetPrompt())
	assert.Equal(t, "Missing link", changes[0].GetTopic())
	assert.Equal(t, "Add the “works with” link", changes[1].GetTitle())

	// A version a person wrote has no generation: every connection is unread.
	store.findings, store.version.GenerationID = nil, nil
	changes = ofKind(listSuggestions(t, s), kindOntology)
	assert.Equal(t, []string{"Add Stripe", "Add Shop database"}, suggestionTitles(changes))
	// And one whose generation is gone reads the same.
	store.version.GenerationID = new("gen-gone")
	assert.Equal(t, []string{"Add Stripe", "Add Shop database"}, suggestionTitles(ofKind(listSuggestions(t, s), kindOntology)))
}

func TestAutomationSuggestionsAfterAFailure(t *testing.T) {
	s, store := suggestionService(t)
	failed := &foundrymodel.FoundryAutomationSuggestions{OntologyID: "ont-1", VersionID: "ver-1", Status: foundrymodel.FoundryAutomationSuggestionsFailed,
		Questions:   []string{"Which customers are under contract?", "How many invoices are overdue?", "When does each term end?"},
		RequestedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now().Add(-time.Minute)}
	store.row = failed
	out := listSuggestions(t, s)
	assert.Equal(t, fallbackTitles, suggestionTitles(ofKind(out, kindWorkflow)))
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
		suggestionTitles(ofKind(out, kindWorkflow)))
	assert.Len(t, store.row.Questions, 5, "the row keeps every question the fallback may need")
}

// Where no write can run, the fallbacks answer and nothing is queued.
func TestAutomationSuggestionsWithoutAModel(t *testing.T) {
	for name, setup := range map[string]func(*Service, *suggestionStore){
		"no AI provider": func(s *Service, _ *suggestionStore) { s.deps.AI = deployment(t, aiConfig(modeLocal, true)) },
		"own key off Pro": func(_ *Service, store *suggestionStore) {
			store.active = &foundrymodel.FoundryAIConfig{ID: "cfg-1", WorkspaceID: "ws-1", Kind: "anthropic"}
		},
		"Founder off":  func(s *Service, _ *suggestionStore) { s.deps.Cfg.Foundry.Agent.Enabled = false },
		"20 this hour": func(_ *Service, store *suggestionStore) { store.queueErr = foundrystore.ErrLimitExceeded },
	} {
		t.Run(name, func(t *testing.T) {
			s, store := suggestionService(t)
			setup(s, store)
			out := listSuggestions(t, s)
			assert.Equal(t, allKinds, kindsOf(out))
			assert.Equal(t, fallbackTitles, suggestionTitles(ofKind(out, kindWorkflow)))
			assert.Equal(t, questionsSource, out.GetSource())
			assert.False(t, out.GetGenerating())
			assert.Nil(t, store.row, "nothing queued")
		})
	}
}

// Where the deployment has no workflows, Founder's page still has its
// suggestions, without workflows.
func TestAutomationSuggestionsWithoutWorkflows(t *testing.T) {
	s, store := suggestionService(t)
	s.deps.Cfg.Foundry.Automations.Enabled = false
	out := listSuggestions(t, s)
	assert.Equal(t, allKinds[:6], kindsOf(out))
	assert.Equal(t, foundryv1.FoundryAutomationSuggestionSource_FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_UNSPECIFIED, out.GetSource(), "no workflow, no source")
	assert.True(t, out.GetGenerating(), "Founder still writes the other kinds")
	assert.Equal(t, 1, store.queued)

	store.ontology.CurrentVersionID = nil
	assert.Len(t, listSuggestions(t, s).GetSuggestions(), 6)
}

func TestAutomationSuggestionsWithoutAVersion(t *testing.T) {
	s, store := suggestionService(t)
	store.ontology.CurrentVersionID = nil
	out := listSuggestions(t, s)
	assert.Equal(t, allKinds, kindsOf(out))
	for _, x := range out.GetSuggestions() {
		assert.Equal(t, sourceFixed, x.GetSource(), x.GetTitle())
		assert.Contains(t, x.GetPrompt(), "Customer contracts")
	}
	assert.Equal(t, "Help me connect my data and draft the first version of Customer contracts.", ofKind(out, kindOntology)[0].GetPrompt())
	assert.Equal(t, sourceFixed, out.GetSource())
	assert.False(t, out.GetGenerating())
	assert.Empty(t, out.GetVersionId())
	assert.Zero(t, store.queued)
	assert.Zero(t, store.versionReads)

	// A Definition v1 has no questions: fixed examples until Founder writes.
	s, store = suggestionService(t)
	store.version.Format, store.version.Definition = foundrymodel.OntologyFormatFoundryV1, json.RawMessage(`{"entities":[]}`)
	out = listSuggestions(t, s)
	assert.Equal(t, allKinds, kindsOf(out))
	assert.Equal(t, []foundryv1.FoundryAutomationSuggestionSource{sourceFixed, sourceFixed}, suggestionSourcesOf(ofKind(out, kindQuestion)))
	assert.Equal(t, sourceFixed, out.GetSource())
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
	require.NoError(t, list(site("user-ada"), "ws-1", "ont-1"), "workflows off, the route still answers")
	s.deps.Cfg.Foundry.Automations.Enabled = true
	s.deps.People = failingPeople{}
	assert.Equal(t, codes.Unavailable, status.Code(list(site("user-ada"), "ws-1", "ont-1")), "the person's role can't be read")
	s.deps.People = dashboardPeople{"user-ada": ports.RoleMember}

	// The store failing is the only other error.
	store.rowErr = errors.New("down")
	assert.Equal(t, codes.Internal, status.Code(list(site("user-ada"), "ws-1", "ont-1")))
	store.rowErr = nil
	for _, read := range []string{"findings", "sources", "generation", "connectors", "sets"} {
		store.setupErr[read] = errors.New("down")
		assert.Equal(t, codes.Internal, status.Code(list(site("user-ada"), "ws-1", "ont-1")), read)
		delete(store.setupErr, read)
	}
	store.row, store.queueErr = nil, errors.New("down")
	assert.Equal(t, codes.Unavailable, status.Code(list(site("user-ada"), "ws-1", "ont-1")))
	store.queueErr, store.setupErr["version"] = nil, errors.New("down")
	assert.Equal(t, codes.Internal, status.Code(list(site("user-ada"), "ws-1", "ont-1")), "the current version can't be read")
}

// failingPeople can't read anyone's membership.
type failingPeople struct{ dashboardPeople }

func (failingPeople) Member(context.Context, string, string) (ports.Member, error) {
	return ports.Member{}, errors.New("down")
}
