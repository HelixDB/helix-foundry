package foundryaskworker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// assessStoreFake holds one finished run of ont-1 with one call of each
// kind, and records how its assessment ended.
type assessStoreFake struct {
	foundrystore.Unimplemented
	queue    []*model.FoundryWorkflowAssessment
	spent    int64
	outcomes []model.FoundryWorkflowAssessmentOutcome
	renewErr error
	readErr  error
	noLoad   bool
}

func (s *assessStoreFake) ClaimFoundryWorkflowRunAssessment(context.Context, string, time.Duration) (*model.FoundryWorkflowAssessment, error) {
	if len(s.queue) == 0 {
		return nil, foundrystore.ErrNotFound
	}
	a := s.queue[0]
	s.queue = s.queue[1:]
	return a, nil
}
func (s *assessStoreFake) RenewFoundryWorkflowRunAssessment(context.Context, string, string, time.Duration) error {
	return s.renewErr
}
func (s *assessStoreFake) FinishFoundryWorkflowRunAssessment(_ context.Context, _, _ string, o model.FoundryWorkflowAssessmentOutcome) error {
	if err := o.Check(); err != nil {
		return err
	}
	s.outcomes = append(s.outcomes, o)
	return nil
}
func (s *assessStoreFake) SumFoundryWorkflowAssessmentSpend(context.Context, string, time.Time) (int64, error) {
	return s.spent, nil
}
func (s *assessStoreFake) GetFoundryWorkflowRun(_ context.Context, ontologyID, id string) (*model.FoundryWorkflowRun, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	in := "Who renews in March?"
	return &model.FoundryWorkflowRun{ID: id, OntologyID: ontologyID, Input: &in, StartedBy: access.Principal{Kind: access.PrincipalUser, ID: "u"},
		Finished: &model.FoundryWorkflowRunEnd{Output: "Globex renews in March."}}, nil
}
func (s *assessStoreFake) ListFoundryWorkflowRunAccesses(context.Context, string, int32, int) ([]*model.FoundryWorkflowAccess, bool, error) {
	return []*model.FoundryWorkflowAccess{
		{Seq: 1, Call: &model.FoundryWorkflowRead{LoadID: "load-1", Reads: json.RawMessage(`[{"entity":{}}]`), Result: json.RawMessage(`{"results":[]}`)}},
		{Seq: 2, Call: &model.FoundryWorkflowQuestion{LoadID: "load-1", QuestionID: "q-1"}},
		{Seq: 3, Call: &model.FoundryWorkflowQuestion{LoadID: "load-1", QuestionID: "q-gone"}},
		{Seq: 4, Call: &model.FoundryWorkflowSend{ConnectorID: "conn-1", Seq: 2, PieceIDs: []string{"p-1"}}},
		{Seq: 5, Call: &model.FoundryWorkflowUpload{DocumentID: "doc-1", SetID: "set-1"}},
	}, false, nil
}
func (s *assessStoreFake) GetFoundryQuestion(_ context.Context, _, id string) (*model.FoundryQuestion, error) {
	if id != "q-1" {
		return nil, foundrystore.ErrNotFound
	}
	return &model.FoundryQuestion{ID: id, Question: "When does Globex renew?", Status: model.FoundryQuestionAnswered,
		Result: json.RawMessage(`{"answer":"March","queries":[{"query":{},"output":{"rows":[1]}}]}`)}, nil
}
func (s *assessStoreFake) GetConnector(_ context.Context, _, id string) (*model.Connector, error) {
	return &model.Connector{ID: id, Name: "Tickets"}, nil
}
func (s *assessStoreFake) GetFoundryDocument(_ context.Context, _, id string) (*model.FoundryDocument, error) {
	return &model.FoundryDocument{ID: id, Name: "notes.md"}, nil
}
func (s *assessStoreFake) GetFoundryLoad(_ context.Context, _, id string) (*model.FoundryLoad, error) {
	if s.noLoad {
		return nil, foundrystore.ErrNotFound
	}
	return &model.FoundryLoad{ID: id, VersionID: "ver-3"}, nil
}
func (s *assessStoreFake) GetFoundryOntology(_ context.Context, _, id string) (*model.FoundryOntology, error) {
	current := "ver-4"
	return &model.FoundryOntology{ID: id, CurrentVersionID: &current}, nil
}
func (s *assessStoreFake) GetOntologyVersion(_ context.Context, _, id string) (*model.OntologyVersion, error) {
	def, _ := json.Marshal(ontologyagent.Definition{Format: "agent_bundle", Status: ontologyagent.StatusDone, OntologyTOML: "# " + id + "\n"})
	return &model.OntologyVersion{ID: id, Format: model.OntologyFormatAgentBundle, Definition: def}, nil
}
func (s *assessStoreFake) GetActiveFoundryAIConfig(context.Context, string) (*model.FoundryAIConfig, error) {
	return nil, foundrystore.ErrNotFound
}

// assessAgentFake answers Assess with result, or err, and keeps the request.
type assessAgentFake struct {
	result  string
	err     error
	request sentAssess
	raw     map[string]any
}

// sentAssess is what the tests read of a request the agent was sent.
type sentAssess struct {
	RequestID    string `json:"request_id"`
	OntologyTOML string `json:"ontology_toml"`
	Input        string `json:"input"`
	Output       string `json:"output"`
	Models       struct {
		Assess struct {
			Name string `json:"name"`
		} `json:"assess"`
	} `json:"models"`
}

func (a *assessAgentFake) Info(context.Context) (*agentv1.GetInfoResponse, error) {
	return &agentv1.GetInfoResponse{ContractVersion: ontologyagent.ContractVersion, Providers: []string{"anthropic"}}, nil
}
func (a *assessAgentFake) Assess(_ context.Context, request []byte, _ ontologyagent.ModelKey) (json.RawMessage, error) {
	if err := json.Unmarshal(request, &a.request); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(request, &a.raw)
	return json.RawMessage(a.result), a.err
}

const assessed = `{"outcome":{"assessed":{"assessment":{"verdict":"helpful","summary":"s","claims":[],"gaps":[],"suggestions":[]},"left_out":[1]}},
"usage":{"input":10,"cache_read":0,"cache_write":0,"output":5,"usd":0.0123}}`

func failedWith(failure string) string {
	return `{"outcome":{"failed":{"failure":"` + failure + `"}},"usage":{"input":1,"cache_read":0,"cache_write":0,"output":0,"usd":0.001}}`
}

func newAssessor(t *testing.T, store *assessStoreFake, agent *assessAgentFake) *assessor {
	t.Helper()
	cfg := config.Defaults()
	cfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: houseKey, Models: config.AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
	}}
	resolver, err := ai.NewResolver(&cfg, store, onPro{})
	require.NoError(t, err)
	store.queue = append(store.queue, &model.FoundryWorkflowAssessment{RunID: "run-1", WorkspaceID: "ws-1", OntologyID: "ont-1", Attempts: 1})
	return &assessor{db: store, agent: agent, ai: resolver, cfg: cfg.Foundry.Workflows.Assess, owner: "w1",
		now:   func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) },
		month: func(now time.Time) time.Time { return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC) }}
}

func TestAssessorAssessesARun(t *testing.T) {
	store, agent := &assessStoreFake{}, &assessAgentFake{result: assessed}
	newAssessor(t, store, agent).tick(context.Background())
	require.Len(t, store.outcomes, 1)
	o := store.outcomes[0]
	assert.Equal(t, model.FoundryWorkflowAssessmentDone, o.Status)
	assert.Equal(t, "helpful", o.Verdict)
	assert.Equal(t, []int32{1}, o.LeftOut)
	assert.EqualValues(t, 12300, o.USDMicros)

	r := agent.request
	assert.Equal(t, "run-1", r.RequestID)
	assert.Equal(t, "Who renews in March?", r.Input)
	assert.Equal(t, "Globex renews in March.", r.Output)
	assert.Equal(t, "# ver-3\n", r.OntologyTOML, "the version of the load it read last")
	assert.Equal(t, "claude-sonnet-5-5", r.Models.Assess.Name, "the question model")
	calls := agent.raw["calls"].([]any)
	require.Len(t, calls, 5)
	kinds := []string{}
	for _, c := range calls {
		kinds = append(kinds, c.(map[string]any)["call"].(string))
	}
	assert.Equal(t, []string{"read", "question", "question", "send", "upload"}, kinds)
	question := calls[1].(map[string]any)
	assert.Equal(t, "When does Globex renew?", question["question"])
	assert.Equal(t, "answered", question["status"])
	answer, _ := json.Marshal(question["answer"])
	assert.NotContains(t, string(answer), "output", "a question's queries go without their outputs")
	assert.Equal(t, "deleted", calls[2].(map[string]any)["status"])
	assert.Equal(t, "Tickets", calls[3].(map[string]any)["source"])
	assert.Equal(t, "notes.md", calls[4].(map[string]any)["document"])
}

func TestAssessorReadsTheCurrentVersionWithoutALoad(t *testing.T) {
	store, agent := &assessStoreFake{noLoad: true}, &assessAgentFake{result: assessed}
	newAssessor(t, store, agent).tick(context.Background())
	assert.Equal(t, "# ver-4\n", agent.request.OntologyTOML)
}

func TestAssessorSkipsPastTheMonthsBudget(t *testing.T) {
	store, agent := &assessStoreFake{spent: 19_500_000}, &assessAgentFake{result: assessed}
	newAssessor(t, store, agent).tick(context.Background())
	require.Len(t, store.outcomes, 1)
	assert.Equal(t, model.FoundryWorkflowAssessmentSkipped, store.outcomes[0].Status)
	assert.Contains(t, store.outcomes[0].Error, "$20.00")
	assert.Empty(t, agent.request.RequestID, "the agent isn't called")
}

func TestAssessorRetriesWhatARetryMayFix(t *testing.T) {
	for name, tc := range map[string]struct {
		result   string
		err      error
		attempts int
		want     model.FoundryWorkflowAssessmentStatus
		failure  string
	}{
		"an unavailable model":            {result: failedWith("unavailable"), attempts: 1, want: model.FoundryWorkflowAssessmentQueued, failure: "unavailable"},
		"an unavailable model, at last":   {result: failedWith("unavailable"), attempts: 3, want: model.FoundryWorkflowAssessmentFailed},
		"a refusal":                       {result: failedWith("refused"), attempts: 1, want: model.FoundryWorkflowAssessmentFailed, failure: "refused"},
		"an unavailable agent":            {err: status.Error(codes.Unavailable, "down"), attempts: 1, want: model.FoundryWorkflowAssessmentQueued},
		"a busy agent":                    {err: status.Error(codes.ResourceExhausted, "busy"), attempts: 1, want: model.FoundryWorkflowAssessmentQueued},
		"a request the agent refuses":     {err: status.Error(codes.InvalidArgument, "bad"), attempts: 1, want: model.FoundryWorkflowAssessmentFailed},
		"a result that isn't the agent's": {result: `{"outcome":{}}`, attempts: 1, want: model.FoundryWorkflowAssessmentFailed},
	} {
		t.Run(name, func(t *testing.T) {
			store, agent := &assessStoreFake{}, &assessAgentFake{result: tc.result, err: tc.err}
			a := newAssessor(t, store, agent)
			store.queue[0].Attempts = tc.attempts
			a.tick(context.Background())
			require.Len(t, store.outcomes, 1)
			o := store.outcomes[0]
			assert.Equal(t, tc.want, o.Status)
			assert.NotEmpty(t, o.Error)
			if tc.failure != "" {
				assert.Equal(t, tc.failure, o.Failure)
			}
			if tc.want == model.FoundryWorkflowAssessmentQueued {
				assert.Equal(t, a.now().Add(a.cfg.RetryBackoff), o.RetryAt)
			}
		})
	}
}

func TestAssessorRetriesARunItCouldNotRead(t *testing.T) {
	store, agent := &assessStoreFake{readErr: errors.New("db down")}, &assessAgentFake{result: assessed}
	newAssessor(t, store, agent).tick(context.Background())
	require.Len(t, store.outcomes, 1)
	assert.Equal(t, model.FoundryWorkflowAssessmentQueued, store.outcomes[0].Status)
	assert.True(t, strings.Contains(store.outcomes[0].Error, "couldn't be read"))
}

func TestAssessorDoesNothingWhileOff(t *testing.T) {
	store, agent := &assessStoreFake{}, &assessAgentFake{result: assessed}
	a := newAssessor(t, store, agent)
	a.cfg.Enabled = false
	a.tick(context.Background())
	assert.Empty(t, store.outcomes)
	assert.Len(t, store.queue, 1)
}

func TestAssessCallsAreTheContractsShape(t *testing.T) {
	for _, tc := range []struct {
		call ontologyagent.AssessCall
		want string
	}{
		{ontologyagent.AssessCall{Call: "read", Seq: 1, Reads: json.RawMessage(`[]`)}, `{"call":"read","reads":[],"seq":1}`},
		{ontologyagent.AssessCall{Call: "question", Seq: 2, Question: "q", Status: "failed"}, `{"call":"question","question":"q","seq":2,"status":"failed"}`},
		{ontologyagent.AssessCall{Call: "send", Seq: 3, Source: "s"}, `{"call":"send","pieces":[],"seq":3,"source":"s"}`},
		{ontologyagent.AssessCall{Call: "upload", Seq: 4, Document: "d"}, `{"call":"upload","document":"d","seq":4}`},
	} {
		got, err := json.Marshal(tc.call)
		require.NoError(t, err)
		assert.JSONEq(t, tc.want, string(got))
	}
	_, err := json.Marshal(ontologyagent.AssessCall{Call: "later"})
	assert.Error(t, err)
}
