package api

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/helixdb/helix-foundry/backend/agentquestion"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// AnswerOntologyGeneration queues a generation waiting on draftRev with
// answers, as the store's conditional update does.
func (s *generationStore) AnswerOntologyGeneration(_ context.Context, ontologyID, id string, draftRev uint64, answers []agentquestion.Answer) error {
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
	// waiting is a service whose ontology ont-1 has gen-1 waiting on its
	// first round of two questions, of draft revision 3, one blocking.
	waiting := func(t *testing.T) (*Service, *generationStore) {
		s, store := generationService(t, 50)
		s.deps.Cfg.Foundry.Ontology.AnswerDays = 7
		store.generations = append(store.generations, &foundrymodel.OntologyGeneration{
			ID: "gen-1", WorkspaceID: "ws-1", OntologyID: "ont-1", Status: foundrymodel.OntologyGenerationNeedsInput, Attempts: 2,
			TurnState: []byte(`{"secret":"Acme Corp's excerpt"}`),
			Questions: &foundrymodel.OntologyQuestions{Round: 1, DraftRev: 3, AskedAt: askedAt, Asked: []foundrymodel.OntologyQuestion{
				{Question: agentquestion.Question{ID: "q1", Text: "Is a customer an account? <b>bold</b>", Blocking: true}, Items: []string{"type.Customer"}},
				{Question: agentquestion.Question{ID: "q2", Header: "Renewals", Text: "How often do they renew?", Choice: &agentquestion.Choice{
					Options: []agentquestion.Option{{Label: "Monthly", Description: "Every month"}, {Label: "Yearly"}}}, Recommended: "Monthly"}},
			}},
		})
		return s, store
	}
	answer := func(s *Service, req *foundryv1.AnswerOntologyGenerationRequest) (*foundryv1.OntologyGeneration, error) {
		if req.WorkspaceId == "" {
			req.WorkspaceId, req.OntologyId, req.Id = "ws-1", "ont-1", "gen-1"
		}
		if req.Round == 0 {
			req.Round = 1
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
			!gen.GetAnswersDueAt().AsTime().Equal(askedAt.Add(7*24*time.Hour)) || len(gen.GetQuestions()) != 2 || gen.GetQuestionRound() != 1 {
			t.Fatalf("generation = %+v", gen)
		}
		want := []*foundryv1.OntologyGenerationQuestion{
			{Id: "q1", Text: "Is a customer an account? <b>bold</b>", Blocking: true, Items: []string{"type.Customer"}},
			{Id: "q2", Header: "Renewals", Text: "How often do they renew?", Recommended: "Monthly",
				Options: []*foundryv1.AgentQuestionOption{{Label: "Monthly", Description: "Every month"}, {Label: "Yearly"}}},
		}
		for i, q := range gen.GetQuestions() {
			if !proto.Equal(q, want[i]) {
				t.Errorf("question %d = %+v, want %+v", i, q, want[i])
			}
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
			Answers: []*foundryv1.AgentQuestionAnswer{{QuestionId: "q1", Text: " Yes, one account each "}}, UseDefaults: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		stored := store.generations[0]
		want := []agentquestion.Answer{{QuestionID: "q1", Options: []string{}, Text: "Yes, one account each"}, {QuestionID: "q2", Left: true}}
		if stored.Status != foundrymodel.OntologyGenerationQueued || !reflect.DeepEqual(stored.Questions.Answers, want) {
			t.Fatalf("stored = %+v, answers %+v", stored, stored.Questions.Answers)
		}
		if gen.GetStatus() != foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_QUEUED || gen.AnswersDueAt != nil ||
			gen.GetQuestions()[0].GetAnswer().GetText() != "Yes, one account each" || !gen.GetQuestions()[1].GetLeftToAgent() {
			t.Fatalf("answered generation = %+v", gen)
		}
		if _, err := answer(s, &foundryv1.AnswerOntologyGenerationRequest{UseDefaults: true}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("answered twice: %v", err)
		}
	})

	// Every question can be left to the agent, so a person who can't answer
	// still moves the generation on; what the agent takes is its own choice,
	// so nothing is stored or shown as the person's answer.
	t.Run("leaves every question to the agent", func(t *testing.T) {
		s, store := waiting(t)
		gen, err := answer(s, &foundryv1.AnswerOntologyGenerationRequest{UseDefaults: true,
			Answers: []*foundryv1.AgentQuestionAnswer{{QuestionId: "q2", Options: []string{"Yearly"}}}})
		if err != nil {
			t.Fatal(err)
		}
		want := []agentquestion.Answer{{QuestionID: "q1", Left: true}, {QuestionID: "q2", Options: []string{"Yearly"}}}
		if got := store.generations[0].Questions.Answers; !reflect.DeepEqual(got, want) {
			t.Fatalf("answers = %+v, want %+v", got, want)
		}
		if q := gen.GetQuestions(); !q[0].GetLeftToAgent() || q[0].GetAnswer() != nil || q[1].GetLeftToAgent() || q[1].GetAnswer() == nil {
			t.Fatalf("shown = %+v", q)
		}
	})

	t.Run("takes an option, or the person's own words", func(t *testing.T) {
		for _, a := range []*foundryv1.AgentQuestionAnswer{{QuestionId: "q2", Options: []string{"Yearly"}}, {QuestionId: "q2", Text: "Every two years"}} {
			s, store := waiting(t)
			if _, err := answer(s, &foundryv1.AnswerOntologyGenerationRequest{
				Answers: []*foundryv1.AgentQuestionAnswer{{QuestionId: "q1", Text: "Yes"}, a}}); err != nil {
				t.Fatal(err)
			}
			if got := store.generations[0].Questions.Answers[1]; !reflect.DeepEqual(got.Options, append([]string{}, a.GetOptions()...)) || got.Text != a.GetText() {
				t.Errorf("stored %+v for %+v", got, a)
			}
		}
	})

	// A stale page's answers name the round they answer: once the agent asks
	// again, the same question ids may mean other questions.
	t.Run("refuses answers for another round", func(t *testing.T) {
		s, store := waiting(t)
		_, err := answer(s, &foundryv1.AnswerOntologyGenerationRequest{Round: 2, UseDefaults: true})
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "round 2") ||
			store.generations[0].Status != foundrymodel.OntologyGenerationNeedsInput {
			t.Fatalf("another round: %v", err)
		}
	})

	t.Run("refuses answers the agent can't take", func(t *testing.T) {
		for name, req := range map[string]*foundryv1.AnswerOntologyGenerationRequest{
			"a blocking question left out": {Answers: []*foundryv1.AgentQuestionAnswer{{QuestionId: "q2", Options: []string{"Yearly"}}}},
			"a question not asked":         {Answers: []*foundryv1.AgentQuestionAnswer{{QuestionId: "q9", Text: "x"}}, UseDefaults: true},
			"a blank answer":               {Answers: []*foundryv1.AgentQuestionAnswer{{QuestionId: "q1", Text: "  "}}},
			"an option not offered":        {Answers: []*foundryv1.AgentQuestionAnswer{{QuestionId: "q1", Text: "Yes"}, {QuestionId: "q2", Options: []string{"Weekly"}}}},
			"two options of one":           {Answers: []*foundryv1.AgentQuestionAnswer{{QuestionId: "q1", Text: "Yes"}, {QuestionId: "q2", Options: []string{"Monthly", "Yearly"}}}},
			"an option and words of one":   {Answers: []*foundryv1.AgentQuestionAnswer{{QuestionId: "q1", Text: "Yes"}, {QuestionId: "q2", Options: []string{"Monthly"}, Text: "x"}}},
			"an option to a free one":      {Answers: []*foundryv1.AgentQuestionAnswer{{QuestionId: "q1", Options: []string{"Monthly"}}}},
			"no id":                        {WorkspaceId: "ws-1", OntologyId: "ont-1", UseDefaults: true},
		} {
			s, store := waiting(t)
			if _, err := answer(s, req); status.Code(err) != codes.InvalidArgument || store.generations[0].Status != foundrymodel.OntologyGenerationNeedsInput {
				t.Errorf("%s: err = %v, status %s", name, err, store.generations[0].Status)
			}
		}
	})

	// A client that sends no round can never answer by reading the
	// questions again: its request is wrong, not stale.
	t.Run("refuses answers without a round", func(t *testing.T) {
		s, store := waiting(t)
		for _, round := range []int32{0, -1} {
			_, err := s.AnswerOntologyGeneration(ctx, &foundryv1.AnswerOntologyGenerationRequest{
				WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "gen-1", Round: round, UseDefaults: true})
			if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "round is required") ||
				store.generations[0].Status != foundrymodel.OntologyGenerationNeedsInput {
				t.Errorf("round %d: %v", round, err)
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

	// A first draft that loads once drafted writes the ontology's database,
	// so only a caller who may write it answers, as only one may ask for it.
	t.Run("needs the database's write to answer a first draft that loads", func(t *testing.T) {
		writer := ports.Actor{Source: ports.SourceUser, UserID: "user-1", WorkspaceID: "ws-1",
			QueryRead: ports.AllProjects(), QueryWrite: ports.SelectedProjects([]string{"proj-1"})}
		otherProject := writer
		otherProject.QueryWrite = ports.SelectedProjects([]string{"proj-2"})
		for as, want := range map[*ports.Actor]codes.Code{&person: codes.PermissionDenied, &otherProject: codes.PermissionDenied, &writer: codes.OK} {
			s, store := waiting(t)
			store.generations[0].Input.LoadWhenReady = true
			_, err := s.AnswerOntologyGeneration(ports.WithActor(context.Background(), *as), &foundryv1.AnswerOntologyGenerationRequest{
				WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "gen-1", Round: 1, UseDefaults: true})
			if status.Code(err) != want || (want == codes.OK) != (store.generations[0].Status == foundrymodel.OntologyGenerationQueued) {
				t.Errorf("as %+v: err = %v, status %s", *as, err, store.generations[0].Status)
			}
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

func (s *racingStore) AnswerOntologyGeneration(context.Context, string, string, uint64, []agentquestion.Answer) error {
	return foundrystore.ErrNotFound
}
