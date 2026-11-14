package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ontologygen"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// AnswerOntologyGeneration queues a generation waiting on draftRev with
// answers, as the store's conditional update does.
func (s *generationStore) AnswerOntologyGeneration(_ context.Context, ontologyID, id string, draftRev uint64, answers []foundrymodel.OntologyAnswer) error {
	for _, g := range s.generations {
		if g.ID == id && g.OntologyID == ontologyID && g.Status == foundrymodel.OntologyGenerationNeedsInput && g.Questions.DraftRev == draftRev {
			g.Status, g.Questions.Answers, g.Attempts = foundrymodel.OntologyGenerationQueued, answers, 0
			return nil
		}
	}
	return foundrystore.ErrNotFound
}

// CancelOntologyGeneration ends a live generation as cancelled.
func (s *generationStore) CancelOntologyGeneration(_ context.Context, ontologyID, id string) error {
	if g := s.live(ontologyID); g != nil && g.ID == id {
		now := time.Now()
		g.Status, g.FinishedAt, g.TurnState = foundrymodel.OntologyGenerationCancelled, &now, nil
		return nil
	}
	return foundrystore.ErrNotFound
}

func TestOntologyGenerationQuestionRPCs(t *testing.T) {
	ctx := ports.WithActor(context.Background(), person)
	askedAt := time.Date(2026, time.November, 10, 9, 0, 0, 0, time.UTC)
	monthly := "Monthly"
	// waiting is a service whose ontology ont-1 has gen-1 waiting on two
	// questions of draft revision 3, one blocking.
	waiting := func(t *testing.T) (*Service, *generationStore) {
		s, store := generationService(t, 50)
		s.deps.Cfg.Foundry.Ontology.AnswerDays = 7
		store.generations = append(store.generations, &foundrymodel.OntologyGeneration{
			ID: "gen-1", WorkspaceID: "ws-1", OntologyID: "ont-1", Status: foundrymodel.OntologyGenerationNeedsInput, Attempts: 2,
			TurnState: []byte(`{"secret":"Acme Corp's excerpt"}`),
			Questions: &foundrymodel.OntologyQuestions{DraftRev: 3, AskedAt: askedAt, Asked: []foundrymodel.OntologyQuestion{
				{ID: "q1", Items: []string{"type.Customer"}, Text: "Is a customer an account? <b>bold</b>", Options: []string{}, Blocking: true},
				{ID: "q2", Items: []string{}, Text: "How often do they renew?", Options: []string{"Monthly", "Yearly"}, Default: &monthly},
			}},
		})
		return s, store
	}
	answer := func(s *Service, req *foundryv1.AnswerOntologyGenerationRequest) (*foundryv1.OntologyGeneration, error) {
		if req.WorkspaceId == "" {
			req.WorkspaceId, req.OntologyId, req.Id = "ws-1", "ont-1", "gen-1"
		}
		resp, err := s.AnswerOntologyGeneration(ctx, req)
		return resp.GetGeneration(), err
	}

	// The questions show as the agent wrote them, with when Helix answers
	// them itself; the session state never leaves.
	t.Run("shows the questions a generation waits on", func(t *testing.T) {
		s, _ := waiting(t)
		resp, err := s.GetOntologyGeneration(ctx, &foundryv1.GetOntologyGenerationRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "gen-1"})
		if err != nil {
			t.Fatal(err)
		}
		gen := resp.GetGeneration()
		if gen.GetStatus() != foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT || !gen.GetAskedAt().AsTime().Equal(askedAt) ||
			!gen.GetAnswersDueAt().AsTime().Equal(askedAt.Add(7*24*time.Hour)) || len(gen.GetQuestions()) != 2 {
			t.Fatalf("generation = %+v", gen)
		}
		q1, q2 := gen.GetQuestions()[0], gen.GetQuestions()[1]
		if q1.GetId() != "q1" || q1.GetText() != "Is a customer an account? <b>bold</b>" || !q1.GetBlocking() || q1.DefaultAnswer != nil || q1.Answer != nil ||
			q2.GetDefaultAnswer() != "Monthly" || strings.Join(q2.GetOptions(), ",") != "Monthly,Yearly" || q2.GetBlocking() {
			t.Fatalf("questions = %+v", gen.GetQuestions())
		}
		if strings.Contains(gen.String(), "Acme Corp") {
			t.Fatalf("the session state left the backend: %s", gen)
		}
		list, err := s.ListOntologyGenerations(ctx, &foundryv1.ListOntologyGenerationsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || len(list.GetGenerations()) != 1 || len(list.GetGenerations()[0].GetQuestions()) != 2 {
			t.Fatalf("ListOntologyGenerations = (%+v, %v)", list, err)
		}
	})

	t.Run("queues the generation with the answers", func(t *testing.T) {
		s, store := waiting(t)
		gen, err := answer(s, &foundryv1.AnswerOntologyGenerationRequest{
			Answers: []*foundryv1.OntologyGenerationAnswer{{QuestionId: "q1", Text: " Yes, one account each "}}, UseDefaults: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		stored := store.generations[0]
		want := []foundrymodel.OntologyAnswer{{QuestionID: "q1", Text: "Yes, one account each"}, {QuestionID: "q2", Text: "Monthly"}}
		if stored.Status != foundrymodel.OntologyGenerationQueued || len(stored.Questions.Answers) != 2 || stored.Questions.Answers[0] != want[0] ||
			stored.Questions.Answers[1] != want[1] {
			t.Fatalf("stored = %+v, answers %+v", stored, stored.Questions.Answers)
		}
		if gen.GetStatus() != foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_QUEUED || gen.AnswersDueAt != nil ||
			gen.GetQuestions()[0].GetAnswer() != "Yes, one account each" || gen.GetQuestions()[1].GetAnswer() != "Monthly" {
			t.Fatalf("answered generation = %+v", gen)
		}
		if _, err := answer(s, &foundryv1.AnswerOntologyGenerationRequest{UseDefaults: true}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("answered twice: %v", err)
		}
	})

	// Every question can take the agent's default, so a person who can't
	// answer still moves the generation on.
	t.Run("answers every question with the agent's defaults", func(t *testing.T) {
		s, store := waiting(t)
		if _, err := answer(s, &foundryv1.AnswerOntologyGenerationRequest{UseDefaults: true}); err != nil {
			t.Fatal(err)
		}
		if got := store.generations[0].Questions.Answers; len(got) != 2 || got[0].Text != ontologygen.NoPreference || got[1].Text != "Monthly" {
			t.Fatalf("answers = %+v", got)
		}
	})

	t.Run("refuses answers the agent can't take", func(t *testing.T) {
		for name, req := range map[string]*foundryv1.AnswerOntologyGenerationRequest{
			"a blocking question left out": {Answers: []*foundryv1.OntologyGenerationAnswer{{QuestionId: "q2", Text: "Yearly"}}},
			"a question not asked":         {Answers: []*foundryv1.OntologyGenerationAnswer{{QuestionId: "q9", Text: "x"}}, UseDefaults: true},
			"a blank answer":               {Answers: []*foundryv1.OntologyGenerationAnswer{{QuestionId: "q1", Text: "  "}}},
			"no id":                        {WorkspaceId: "ws-1", OntologyId: "ont-1", UseDefaults: true},
		} {
			s, store := waiting(t)
			if _, err := answer(s, req); status.Code(err) != codes.InvalidArgument || store.generations[0].Status != foundrymodel.OntologyGenerationNeedsInput {
				t.Errorf("%s: err = %v, status %s", name, err, store.generations[0].Status)
			}
		}
	})

	t.Run("refuses a generation that isn't waiting", func(t *testing.T) {
		s, store := waiting(t)
		store.generations[0].Status = foundrymodel.OntologyGenerationRunning
		if _, err := answer(s, &foundryv1.AnswerOntologyGenerationRequest{UseDefaults: true}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("a running generation: %v", err)
		}
		if _, err := answer(s, &foundryv1.AnswerOntologyGenerationRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "gen-9", UseDefaults: true}); status.Code(err) != codes.NotFound {
			t.Fatalf("no such generation: %v", err)
		}
		if _, err := answer(s, &foundryv1.AnswerOntologyGenerationRequest{WorkspaceId: "ws-2", OntologyId: "ont-2", Id: "gen-1", UseDefaults: true}); status.Code(err) != codes.NotFound {
			t.Fatalf("another workspace's: %v", err)
		}
		// Answered with defaults by the worker between the read and the write.
		s, store = waiting(t)
		s.deps.DB = &racingStore{generationStore: store}
		if _, err := answer(s, &foundryv1.AnswerOntologyGenerationRequest{UseDefaults: true}); status.Code(err) != codes.FailedPrecondition ||
			!strings.Contains(status.Convert(err).Message(), "meanwhile") {
			t.Fatalf("answered meanwhile: %v", err)
		}
	})

	// Answering runs the model, so it needs a provider as generating does.
	t.Run("needs an AI provider", func(t *testing.T) {
		s, store := waiting(t)
		s.deps.AI, s.deps.DB = deployment(t, aiConfig(modeLocal, true)), noAIChosen{store}
		if _, err := answer(s, &foundryv1.AnswerOntologyGenerationRequest{UseDefaults: true}); reason(err) != "FOUNDRY_AI_NOT_CONFIGURED" ||
			store.generations[0].Status != foundrymodel.OntologyGenerationNeedsInput {
			t.Fatalf("AnswerOntologyGeneration() = %v", err)
		}
	})

	// A waiting generation holds its ontology until answered or cancelled.
	t.Run("holds the ontology, until cancelled", func(t *testing.T) {
		s, store := waiting(t)
		_, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if status.Code(err) != codes.AlreadyExists || !strings.Contains(status.Convert(err).Message(), "waiting for answers") {
			t.Fatalf("GenerateOntology beside a waiting one: %v", err)
		}
		resp, err := s.CancelOntologyGeneration(ctx, &foundryv1.CancelOntologyGenerationRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "gen-1"})
		if err != nil || resp.GetGeneration().GetStatus() != foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_CANCELLED ||
			resp.GetGeneration().AnswersDueAt != nil || store.generations[0].TurnState != nil {
			t.Fatalf("CancelOntologyGeneration = (%+v, %v)", resp, err)
		}
		_, err = s.CancelOntologyGeneration(ctx, &foundryv1.CancelOntologyGenerationRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "gen-1"})
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("cancelled twice: %v", err)
		}
		for name, req := range map[string]*foundryv1.CancelOntologyGenerationRequest{
			"no such generation":  {WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "gen-9"},
			"another workspace's": {WorkspaceId: "ws-2", OntologyId: "ont-2", Id: "gen-1"},
			"no id":               {WorkspaceId: "ws-1", OntologyId: "ont-1"},
		} {
			if _, err := s.CancelOntologyGeneration(ctx, req); status.Code(err) != codes.NotFound && status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); err != nil {
			t.Fatalf("GenerateOntology once cancelled: %v", err)
		}
	})
}

// racingStore is a store in which the worker answers the generation with
// defaults between the handler's read and its write.
type racingStore struct{ *generationStore }

func (s *racingStore) AnswerOntologyGeneration(context.Context, string, string, uint64, []foundrymodel.OntologyAnswer) error {
	return foundrystore.ErrNotFound
}
