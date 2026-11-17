package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/agentquestion"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// answeringActions answers AnswerOntologyGeneration with generation, recording the request.
type answeringActions struct {
	FoundryActionAPI
	generation *foundryv1.OntologyGeneration
	got        *foundryv1.AnswerOntologyGenerationRequest
}

func (a *answeringActions) AnswerOntologyGeneration(_ context.Context, req *foundryv1.AnswerOntologyGenerationRequest, _ ...grpc.CallOption) (
	*foundryv1.OntologyGenerationResponse, error) {
	a.got = req
	return &foundryv1.OntologyGenerationResponse{Generation: a.generation}, nil
}

// answer_generation_questions passes a person's answers on as the backend takes them, checked for
// shape before a confirmation is prepared; which questions they answer is the backend's to check.
func TestAnswerGenerationQuestionsOperation(t *testing.T) {
	prepare := func(identity *Identity, payload map[string]any) (FoundryOperation, error) {
		return (&Service{}).ValidateFoundryOperation(context.Background(), identity, FoundryOperationInput{
			Operation: "answer_generation_questions", Target: "ontology:workspace-1/ontology-1", Payload: payload,
		})
	}

	t.Run("passes the answers on", func(t *testing.T) {
		var grant Grant
		var methods []string
		identity := reader()
		identity.Authorize = func(_ context.Context, _ any, g Grant, m ...string) error { grant, methods = g, m; return nil }
		op, err := prepare(identity, map[string]any{"generation_id": "generation-1", "round": 2, "use_defaults": true,
			"answers": []any{map[string]any{"question_id": "q1", "text": "  Yes, one account each\n"},
				map[string]any{"question_id": "q2", "options": []any{"Monthly", "Yearly"}}}})
		require.NoError(t, err)
		req := op.(AnswerGenerationQuestionsOperation).req
		want := &foundryv1.AnswerOntologyGenerationRequest{WorkspaceId: "workspace-1", OntologyId: "ontology-1", Id: "generation-1", Round: 2, UseDefaults: true,
			Answers: []*foundryv1.AgentQuestionAnswer{{QuestionId: "q1", Text: "Yes, one account each"}, {QuestionId: "q2", Options: []string{"Monthly", "Yearly"}}}}
		assert.True(t, proto.Equal(want, req), "request = %v", req)
		assert.Equal(t, foundryUserOnly, grant, "only a person answers")
		assert.Equal(t, []string{"AnswerOntologyGeneration"}, methods)
		assert.JSONEq(t, `{"generation_id":"generation-1","round":2,"answers":[{"question_id":"q1","options":[],"text":"Yes, one account each"},
			{"question_id":"q2","options":["Monthly","Yearly"]}],"use_defaults":true}`, string(op.CanonicalPayload()))

		// Defaults alone are a way on for a person who can't answer.
		op, err = prepare(reader(), map[string]any{"generation_id": "generation-1", "round": 1, "use_defaults": true})
		require.NoError(t, err)
		assert.Empty(t, op.(AnswerGenerationQuestionsOperation).req.GetAnswers())
	})

	t.Run("refuses a payload the backend would", func(t *testing.T) {
		answer := func(id, text string) map[string]any { return map[string]any{"question_id": id, "text": text} }
		chose := func(id string, labels ...any) map[string]any {
			return map[string]any{"question_id": id, "options": labels}
		}
		many := make([]any, maxFoundryAnswers+1)
		for i := range many {
			many[i] = answer("q"+strings.Repeat("x", i), "a")
		}
		labels := make([]any, agentquestion.MaxOptions+1)
		for i := range labels {
			labels[i] = strings.Repeat("o", i+1)
		}
		gen := func(answers ...any) map[string]any {
			return map[string]any{"generation_id": "generation-1", "round": 1, "answers": answers}
		}
		for name, payload := range map[string]map[string]any{
			"no generation":      {"round": 1, "use_defaults": true},
			"a malformed id":     {"generation_id": "a b", "round": 1, "use_defaults": true},
			"no round":           {"generation_id": "generation-1", "use_defaults": true},
			"nothing to send":    {"generation_id": "generation-1", "round": 1},
			"a question twice":   gen(answer("q1", "a"), answer("q1", "b")),
			"a blank answer":     gen(answer("q1", "  ")),
			"a long answer":      gen(answer("q1", strings.Repeat("é", agentquestion.MaxAnswerChars+1))),
			"a padded answer":    gen(answer("q1", "a"+strings.Repeat(" ", 4*agentquestion.MaxAnswerChars))),
			"no question":        gen(answer("", "a")),
			"a control id":       gen(answer("q\n1", "a")),
			"a long question id": gen(answer(strings.Repeat("q", agentquestion.MaxIDBytes+1), "a")),
			"too many answers":   gen(many...),
			"neither":            gen(chose("q1")),
			"an option twice":    gen(chose("q1", "A", "A")),
			"a blank option":     gen(chose("q1", " ")),
			"too many options":   gen(chose("q1", labels...)),
			"an unknown field":   {"generation_id": "generation-1", "round": 1, "use_defaults": true, "cancel": true},
		} {
			_, err := prepare(reader(), payload)
			require.Error(t, err, name)
			assert.Contains(t, err.Error(), "answer_generation_questions takes payload", name)
		}
		_, err := prepare(member(), map[string]any{"generation_id": "generation-1", "round": 1, "use_defaults": true})
		assert.ErrorIs(t, err, errCodeNotFound, "a caller the backend wouldn't let answer")
	})

	// The generation comes back with its questions and their answers, which are the model's text.
	t.Run("returns the generation with its questions", func(t *testing.T) {
		askedAt := time.Date(2026, time.November, 10, 9, 0, 0, 0, time.UTC)
		actions := &answeringActions{generation: &foundryv1.OntologyGeneration{
			Id: "generation-1", Status: foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_QUEUED, AskedAt: timestamppb.New(askedAt), QuestionRound: 1,
			Questions: []*foundryv1.OntologyGenerationQuestion{
				{Id: "q1", Text: "Is a customer an account?", Items: []string{"type.Customer"}, Blocking: true,
					Answer: &foundryv1.AgentQuestionAnswer{QuestionId: "q1", Text: "Yes"}},
				{Id: "q2", Header: "Renewals", Text: "How often?", Options: []*foundryv1.AgentQuestionOption{{Label: "Monthly", Description: "Every month"}, {Label: "Yearly"}},
					Multiple: true, Recommended: "Yearly", Answer: &foundryv1.AgentQuestionAnswer{QuestionId: "q2", Options: []string{"Yearly"}}},
			},
		}}
		s := &Service{Actions: actions}
		op, err := s.ValidateFoundryOperation(context.Background(), reader(), FoundryOperationInput{
			Operation: "answer_generation_questions", Target: "ontology:workspace-1/ontology-1",
			Payload: map[string]any{"generation_id": "generation-1", "round": 1, "use_defaults": true},
		})
		require.NoError(t, err)
		var scoped []any
		identity := reader()
		identity.WithScope = func(ctx context.Context, ws string, write bool) context.Context {
			scoped = []any{ws, write}
			return ctx
		}
		result, err := op.Execute(context.Background(), s, identity, "confirmation-1")
		require.NoError(t, err)
		assert.Equal(t, []any{"workspace-1", true}, scoped, "a first draft's answer is checked against the caller's write scope")
		out := result.(FoundryGenerationOutput)
		assert.Equal(t, "generation-1", actions.got.GetId())
		assert.Equal(t, "queued", out.Status)
		assert.Equal(t, "2026-11-10T09:00:00Z", out.AskedAt)
		assert.Equal(t, int32(1), out.QuestionRound)
		assert.Empty(t, out.AnswersDueAt, "an answered generation is due nothing")
		require.Len(t, out.Questions, 2)
		assert.Equal(t, FoundryGenerationQuestionOutput{ID: "q1", Text: "Is a customer an account?", Items: []string{"type.Customer"},
			Options: []FoundryQuestionOptionOutput{}, Blocking: true, Answer: &FoundryQuestionAnswerOutput{Options: []string{}, Text: "Yes"}}, out.Questions[0])
		assert.Equal(t, FoundryGenerationQuestionOutput{ID: "q2", Header: "Renewals", Text: "How often?", Items: []string{},
			Options: []FoundryQuestionOptionOutput{{Label: "Monthly", Description: "Every month"}, {Label: "Yearly"}}, Multiple: true, Recommended: "Yearly",
			Answer: &FoundryQuestionAnswerOutput{Options: []string{"Yearly"}}}, out.Questions[1])

		waiting := (&Service{}).FoundryGeneration("workspace-1", "ontology-1", &foundryv1.OntologyGeneration{
			Id: "generation-2", Status: foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT,
			AnswersDueAt: timestamppb.New(askedAt.Add(7 * 24 * time.Hour)), Questions: actions.generation.GetQuestions()[:1],
		})
		assert.Equal(t, "needs_input", waiting.Status)
		assert.Equal(t, "2026-11-17T09:00:00Z", waiting.AnswersDueAt)
		assert.Empty(t, (&Service{}).FoundryGeneration("workspace-1", "ontology-1", &foundryv1.OntologyGeneration{Id: "generation-3"}).Questions,
			"a generation that asked nothing lists nothing")
	})
}
