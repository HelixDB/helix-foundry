package api

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// questionStore keeps questions in memory beside the foundry store. The
// MySQL tests hold the limits' and the claim's rules; this one answers with
// refusal when it is set.
type questionStore struct {
	*foundryStore
	questions []*foundrymodel.FoundryQuestion
	events    map[string][]foundrymodel.FoundryQuestionEvent
	limits    []foundrymodel.FoundryQuestionLimits
	refusal   error
	// spend is what SumFoundryQuestionSpend answers; spendSince what it was asked.
	spend      foundrymodel.FoundryQuestionSpend
	spendSince []string
}

func (s *questionStore) SumFoundryQuestionSpend(_ context.Context, workspaceID string, since time.Time) (foundrymodel.FoundryQuestionSpend, error) {
	s.spendSince = append(s.spendSince, workspaceID+" "+since.Format(time.RFC3339))
	return s.spend, nil
}

func (s *questionStore) CreateFoundryQuestion(_ context.Context, q *foundrymodel.FoundryQuestion, limits foundrymodel.FoundryQuestionLimits) error {
	s.limits = append(s.limits, limits)
	if s.refusal != nil {
		return s.refusal
	}
	for _, other := range s.questions {
		if other.OntologyID == q.OntologyID && *other.ClientToken == *q.ClientToken {
			return foundrystore.ErrConflict
		}
	}
	stored := *q
	stored.Status, stored.CreatedAt = foundrymodel.FoundryQuestionQueued, time.Now()
	s.questions = append(s.questions, &stored)
	return nil
}

func (s *questionStore) GetFoundryQuestion(_ context.Context, ontologyID, id string) (*foundrymodel.FoundryQuestion, error) {
	for _, q := range s.questions {
		if q.ID == id && q.OntologyID == ontologyID {
			copied := *q
			return &copied, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *questionStore) GetFoundryQuestionByClientToken(_ context.Context, ontologyID, token string) (*foundrymodel.FoundryQuestion, error) {
	for _, q := range s.questions {
		if q.OntologyID == ontologyID && *q.ClientToken == token {
			copied := *q
			return &copied, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *questionStore) ListFoundryQuestions(_ context.Context, ontologyID, threadID, pageToken string, limit int) ([]*foundrymodel.FoundryQuestion, string, error) {
	if err := s.listPage(pageToken); err != nil {
		return nil, "", err
	}
	var out []*foundrymodel.FoundryQuestion
	for i := len(s.questions) - 1; i >= 0; i-- {
		if q := s.questions[i]; q.OntologyID == ontologyID && (threadID == "" || q.ThreadID == threadID) && len(out) < limit {
			out = append(out, q)
		}
	}
	return out, "", nil
}

func (s *questionStore) ListFoundryQuestionEvents(_ context.Context, questionID string, afterSeq, limit int) ([]foundrymodel.FoundryQuestionEvent, error) {
	var out []foundrymodel.FoundryQuestionEvent
	for _, e := range s.events[questionID] {
		if e.Seq > afterSeq && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *questionStore) CancelFoundryQuestion(_ context.Context, ontologyID, id string) (*foundrymodel.FoundryQuestion, error) {
	for _, q := range s.questions {
		if q.ID != id || q.OntologyID != ontologyID {
			continue
		}
		switch {
		case q.Status == foundrymodel.FoundryQuestionCancelled:
		case !q.Status.Live():
			return nil, foundrystore.ErrConflict
		default:
			now := time.Now()
			q.Status, q.FinishedAt = foundrymodel.FoundryQuestionCancelled, &now
		}
		copied := *q
		return &copied, nil
	}
	return nil, foundrystore.ErrNotFound
}

func questionService(t *testing.T) (*Service, *questionStore) {
	t.Helper()
	s, foundry, _ := foundryService(t)
	store := &questionStore{foundryStore: foundry, events: map[string][]foundrymodel.FoundryQuestionEvent{}}
	current := "load-1"
	foundry.ontologies["ont-1"].CurrentLoadID = &current
	foundry.loads = append(foundry.loads,
		&foundrymodel.FoundryLoad{ID: "load-1", OntologyID: "ont-1", VersionID: "ver-1", Status: foundrymodel.FoundryLoadReady},
		&foundrymodel.FoundryLoad{ID: "load-0", OntologyID: "ont-1", VersionID: "ver-0", Status: foundrymodel.FoundryLoadPartial},
		&foundrymodel.FoundryLoad{ID: "load-w", OntologyID: "ont-1", VersionID: "ver-2", Status: foundrymodel.FoundryLoadWriting},
		&foundrymodel.FoundryLoad{ID: "load-d", OntologyID: "ont-1", VersionID: "ver-0", Status: foundrymodel.FoundryLoadDeleted},
	)
	s.deps.DB = store
	return s, store
}

func ask(s *Service, token, question string, mutate ...func(*foundryv1.AskFoundryQuestionRequest)) (*foundryv1.FoundryQuestionResponse, error) {
	req := &foundryv1.AskFoundryQuestionRequest{
		WorkspaceId: "ws-1", OntologyId: "ont-1", Question: question, ClientToken: token,
	}
	for _, m := range mutate {
		m(req)
	}
	return s.AskFoundryQuestion(ports.WithActor(context.Background(), person), req)
}

func TestAskFoundryQuestion(t *testing.T) {
	// A question asks the current load by default, capped at foundry.ask.max_usd,
	// under the workspace's limits from foundry.ask; the same click is the same question.
	// Its focus is kept as the entity the agent takes, by type and id.
	t.Run("queues a question of the current load", func(t *testing.T) {
		s, store := questionService(t)
		resp, err := ask(s, "c1", "  Which customers signed?  ", func(r *foundryv1.AskFoundryQuestionRequest) {
			r.FocusJson = ` {"id":"scus_1", "type":"Customer"} `
		})
		if err != nil {
			t.Fatal(err)
		}
		q := resp.GetQuestion()
		if q.GetId() == "" || q.GetThreadId() != q.GetId() || q.GetLoadId() != "load-1" || q.GetVersionId() != "ver-1" ||
			q.GetQuestion() != "Which customers signed?" || q.GetStatus() != "queued" || q.GetFocusJson() != `{"type":"Customer","id":"scus_1"}` ||
			q.GetAskedByUserId() != "user-1" {
			t.Fatalf("question = %+v", q)
		}
		stored := store.questions[0]
		if stored.WorkspaceID != "ws-1" || stored.MaxUSDMicros != 500_000 {
			t.Fatalf("stored = %+v", stored)
		}
		limits := store.limits[0]
		if limits.CapMicros != 30_000_000 || limits.PerHour != 30 || limits.Running != 2 || limits.Since.Day() != 1 {
			t.Fatalf("limits = %+v", limits)
		}
		again, err := ask(s, "c1", "Which customers signed?")
		if err != nil || again.GetQuestion().GetId() != q.GetId() || len(store.questions) != 1 {
			t.Fatalf("retry = (%+v, %v)", again, err)
		}
		if _, err := ask(s, "c1", "Something else?"); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("token reused for another question: %v", err)
		}
	})

	t.Run("asks a chosen load and continues a thread", func(t *testing.T) {
		s, _ := questionService(t)
		first, err := ask(s, "c1", "Who?", func(r *foundryv1.AskFoundryQuestionRequest) { r.LoadId = "load-0" })
		if err != nil || first.GetQuestion().GetLoadId() != "load-0" || first.GetQuestion().GetVersionId() != "ver-0" {
			t.Fatalf("first = (%+v, %v)", first, err)
		}
		thread := first.GetQuestion().GetId()
		second, err := ask(s, "c2", "And when?", func(r *foundryv1.AskFoundryQuestionRequest) { r.ThreadId = thread })
		if err != nil || second.GetQuestion().GetThreadId() != thread {
			t.Fatalf("second = (%+v, %v)", second, err)
		}
		// A thread is named by its first question, not a later one.
		if _, err := ask(s, "c3", "Then?", func(r *foundryv1.AskFoundryQuestionRequest) { r.ThreadId = second.GetQuestion().GetId() }); status.Code(err) != codes.NotFound {
			t.Fatalf("a later question as thread: %v", err)
		}
		if _, err := ask(s, "c4", "Then?", func(r *foundryv1.AskFoundryQuestionRequest) { r.ThreadId = "nope" }); status.Code(err) != codes.NotFound {
			t.Fatalf("unknown thread: %v", err)
		}
	})

	t.Run("refuses a load that can't be asked", func(t *testing.T) {
		s, store := questionService(t)
		for load, want := range map[string]codes.Code{"load-w": codes.FailedPrecondition, "load-d": codes.NotFound, "nope": codes.NotFound} {
			if _, err := ask(s, "c-"+load, "Who?", func(r *foundryv1.AskFoundryQuestionRequest) { r.LoadId = load }); status.Code(err) != want {
				t.Fatalf("%s: %v, want %s", load, err, want)
			}
		}
		store.ontologies["ont-1"].CurrentLoadID = nil
		if _, err := ask(s, "c-none", "Who?"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("no current load: %v", err)
		}
		if _, err := ask(s, "c-x", "Who?", func(r *foundryv1.AskFoundryQuestionRequest) { r.WorkspaceId, r.OntologyId = "ws-2", "ont-1" }); status.Code(err) != codes.NotFound {
			t.Fatalf("another workspace's ontology: %v", err)
		}
	})

	// A bad question, token or focus is refused before it is queued and
	// charged: the agent would refuse a focus that isn't an entity.
	t.Run("checks the question, its focus and token", func(t *testing.T) {
		s, _ := questionService(t)
		for name, mutate := range map[string]func(*foundryv1.AskFoundryQuestionRequest){
			"no question":      func(r *foundryv1.AskFoundryQuestionRequest) { r.Question = "   " },
			"long question":    func(r *foundryv1.AskFoundryQuestionRequest) { r.Question = strings.Repeat("é", maxQuestionChars+1) },
			"no token":         func(r *foundryv1.AskFoundryQuestionRequest) { r.ClientToken = "" },
			"focus not object": func(r *foundryv1.AskFoundryQuestionRequest) { r.FocusJson = `["Customer"]` },
			"focus not json":   func(r *foundryv1.AskFoundryQuestionRequest) { r.FocusJson = `{` },
			"focus by key":     func(r *foundryv1.AskFoundryQuestionRequest) { r.FocusJson = `{"type":"Customer","key":"c1"}` },
			"focus of no id":   func(r *foundryv1.AskFoundryQuestionRequest) { r.FocusJson = `{"type":"Customer"}` },
			"focus too large": func(r *foundryv1.AskFoundryQuestionRequest) {
				r.FocusJson = `{"type":"Customer","id":"` + strings.Repeat("x", maxQuestionFocusBytes) + `"}`
			},
		} {
			if _, err := ask(s, "c1", "Who?", mutate); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if _, err := ask(s, "c1", strings.Repeat("é", maxQuestionChars)); err != nil {
			t.Fatalf("a question of %d characters: %v", maxQuestionChars, err)
		}
	})

	// Past the month's budget is a precondition the user can't retry past;
	// past the hour's count, a limit that lifts.
	t.Run("maps the workspace's refusals", func(t *testing.T) {
		s, store := questionService(t)
		store.refusal = foundrymodel.FoundryQuestionRefusal{Budget: true, Reason: "the workspace has spent this month's $30.00 of questions"}
		if _, err := ask(s, "c1", "Who?"); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "$30.00") {
			t.Fatalf("budget: %v", err)
		}
		store.refusal = foundrymodel.FoundryQuestionRefusal{Reason: "the workspace has asked 30 questions in the last hour"}
		if _, err := ask(s, "c1", "Who?"); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("hour: %v", err)
		}
		store.refusal = foundrystore.ErrNotFound
		if _, err := ask(s, "c1", "Who?"); status.Code(err) != codes.NotFound {
			t.Fatalf("workspace gone: %v", err)
		}
	})
}

func TestFoundryQuestionReads(t *testing.T) {
	ctx := context.Background()
	s, store := questionService(t)
	resp, err := ask(s, "c1", "Who?")
	if err != nil {
		t.Fatal(err)
	}
	id := resp.GetQuestion().GetId()
	store.questions[0].Status, store.questions[0].LastSeq = foundrymodel.FoundryQuestionRunning, 3
	for seq := 1; seq <= 3; seq++ {
		store.events[id] = append(store.events[id], foundrymodel.FoundryQuestionEvent{Seq: seq, Event: json.RawMessage(`{"event":"tool_call"}`)})
	}

	t.Run("gets and lists the ontology's questions", func(t *testing.T) {
		got, err := s.GetFoundryQuestion(ctx, &foundryv1.GetFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: id})
		if err != nil || got.GetQuestion().GetStatus() != "running" || got.GetQuestion().GetLastSeq() != 3 {
			t.Fatalf("get = (%+v, %v)", got, err)
		}
		for _, req := range []*foundryv1.GetFoundryQuestionRequest{
			{WorkspaceId: "ws-2", OntologyId: "ont-2", Id: id}, {WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "nope"},
		} {
			if _, err := s.GetFoundryQuestion(ctx, req); status.Code(err) != codes.NotFound {
				t.Fatalf("%+v: %v", req, err)
			}
		}
		list, err := s.ListFoundryQuestions(ctx, &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", ThreadId: id})
		if err != nil || len(list.GetQuestions()) != 1 {
			t.Fatalf("list = (%+v, %v)", list, err)
		}
		if _, err := s.ListFoundryQuestions(ctx, &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", PageToken: "bad"}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("bad page token: %v", err)
		}
		q := list.GetQuestions()[0]
		if q.GetAttempts() != 0 || q.GetReservedMicros() != 0 || q.GetFocusJson() != "" {
			t.Fatalf("question = %+v", q)
		}
	})

	// The list carries the workspace's month, as the claim counts it, and
	// the caps foundry.ask sets, so the page can show spend against budget.
	t.Run("lists with the workspace's questions budget", func(t *testing.T) {
		store.spend, store.spendSince = foundrymodel.FoundryQuestionSpend{SpentMicros: 1_250_000, ReservedMicros: 500_000}, nil
		list, err := s.ListFoundryQuestions(ctx, &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		if len(store.spendSince) != 1 || store.spendSince[0] != "ws-1 "+month {
			t.Fatalf("spend read %v", store.spendSince)
		}
		want := &foundryv1.FoundryQuestionBudget{
			MonthlyUsdMicros: 30_000_000, SpentUsdMicros: 1_250_000, ReservedUsdMicros: 500_000, QuestionUsdMicros: 500_000,
			MaxSteps: 20, MaxWallSecs: 240, ConcurrentLimit: 2, HourlyLimit: 30,
		}
		if !proto.Equal(list.GetBudget(), want) {
			t.Fatalf("budget = %+v", list.GetBudget())
		}
	})

	// A page of answers leaves out their queries' outputs, up to 512 KiB
	// each; the question itself keeps them for its visuals. A 3.10.0 result,
	// with none, lists as stored.
	t.Run("lists answers without their outputs, and gets one whole", func(t *testing.T) {
		s, store := questionService(t)
		const usage = `"usage":{"input":1,"cache_read":0,"cache_write":0,"output":1,"usd":0.01}`
		visual := `{"status":"answered","answer":{"blocks":[{"para":"42 customers [c1]."}],"visuals":[{"evidence_id":"e1","kind":"figure"}]},` +
			`"citations":[{"marker":"c1","evidence_id":"e1","target":{"result":{"evidence_id":"e1"}}}],` +
			`"queries":[{"evidence_id":"e1","query":{"start":{"type":"Customer"}},"output":{"kind":"count","count":42}}],` + usage + `}`
		old := `{"status": "answered", "answer": {"blocks": [{"para": "Two [c1]."}]}, "citations": [], ` +
			`"queries": [{"query": {"start": {"type": "Customer"}}, "evidence_id": "e1"}], ` + usage + `}`
		ids := map[string]string{}
		for token, result := range map[string]string{"c-visual": visual, "c-old": old} {
			asked, err := ask(s, token, "How many customers?")
			if err != nil {
				t.Fatal(err)
			}
			id := asked.GetQuestion().GetId()
			ids[token] = id
			for _, q := range store.questions {
				if q.ID == id {
					q.Status, q.Result = foundrymodel.FoundryQuestionAnswered, json.RawMessage(result)
				}
			}
		}
		list, err := s.ListFoundryQuestions(ctx, &foundryv1.ListFoundryQuestionsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil {
			t.Fatal(err)
		}
		listed := map[string]string{}
		for _, q := range list.GetQuestions() {
			listed[q.GetId()] = q.GetResultJson()
		}
		var got, want any
		without := strings.Replace(visual, `,"output":{"kind":"count","count":42}`, "", 1)
		if json.Unmarshal([]byte(listed[ids["c-visual"]]), &got) != nil || json.Unmarshal([]byte(without), &want) != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("listed %s, want it without its output", listed[ids["c-visual"]])
		}
		if listed[ids["c-old"]] != old {
			t.Fatalf("a 3.10.0 result listed as %s", listed[ids["c-old"]])
		}
		// The question comes whole, as stored: listing it left it alone.
		one, err := s.GetFoundryQuestion(ctx, &foundryv1.GetFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: ids["c-visual"]})
		if err != nil || one.GetQuestion().GetResultJson() != visual {
			t.Fatalf("get = (%s, %v), want the whole result", one.GetQuestion().GetResultJson(), err)
		}
	})

	t.Run("polls events after a seq with the question's status", func(t *testing.T) {
		events, err := s.ListFoundryQuestionEvents(ctx, &foundryv1.ListFoundryQuestionEventsRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", Id: id, AfterSeq: 1,
		})
		if err != nil || len(events.GetEvents()) != 2 || events.GetEvents()[0].GetSeq() != 2 || events.GetStatus() != "running" ||
			events.GetLastSeq() != 3 || events.GetEvents()[0].GetEventJson() != `{"event":"tool_call"}` {
			t.Fatalf("events = (%+v, %v)", events, err)
		}
		if _, err := s.ListFoundryQuestionEvents(ctx, &foundryv1.ListFoundryQuestionEventsRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", Id: id, AfterSeq: -1,
		}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("negative seq: %v", err)
		}
		if _, err := s.ListFoundryQuestionEvents(ctx, &foundryv1.ListFoundryQuestionEventsRequest{
			WorkspaceId: "ws-2", OntologyId: "ont-2", Id: id,
		}); status.Code(err) != codes.NotFound {
			t.Fatalf("another ontology: %v", err)
		}
	})

	t.Run("cancels a live question once, and not one that ended", func(t *testing.T) {
		cancel := func(id string) (*foundryv1.FoundryQuestionResponse, error) {
			return s.CancelFoundryQuestion(ctx, &foundryv1.CancelFoundryQuestionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: id})
		}
		// Another workspace's ontology can't name this one's question.
		if _, err := s.CancelFoundryQuestion(ctx, &foundryv1.CancelFoundryQuestionRequest{
			WorkspaceId: "ws-2", OntologyId: "ont-2", Id: id,
		}); status.Code(err) != codes.NotFound || store.questions[0].Status != foundrymodel.FoundryQuestionRunning {
			t.Fatalf("another ontology: %v, status %s", err, store.questions[0].Status)
		}
		got, err := cancel(id)
		if err != nil || got.GetQuestion().GetStatus() != "cancelled" || got.GetQuestion().GetFinishedAt() == nil {
			t.Fatalf("cancel = (%+v, %v)", got, err)
		}
		if again, err := cancel(id); err != nil || again.GetQuestion().GetStatus() != "cancelled" {
			t.Fatalf("again = (%+v, %v)", again, err)
		}
		answered, err := ask(s, "c2", "When?")
		if err != nil {
			t.Fatal(err)
		}
		store.questions[1].Status = foundrymodel.FoundryQuestionAnswered
		if _, err := cancel(answered.GetQuestion().GetId()); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("answered: %v", err)
		}
		if _, err := cancel("nope"); status.Code(err) != codes.NotFound {
			t.Fatalf("unknown: %v", err)
		}
		if _, err := cancel(""); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("no id: %v", err)
		}
	})
}
