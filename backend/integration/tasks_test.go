package integration

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// jobsBackend holds one question, generation, load and sync, each moved on by a test, and counts
// how often each was read.
type jobsBackend struct {
	foundryv1.FoundryServiceClient
	mu         sync.Mutex
	question   *foundryv1.FoundryQuestion
	generation *foundryv1.OntologyGeneration
	load       *foundryv1.FoundryLoad
	sync       *foundryv1.ConnectorSync
	reads      int
	cancelled  []string
	synced     int
}

var jobCreated = timestamppb.New(time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC))

func newJobsBackend() *jobsBackend {
	return &jobsBackend{
		question:   &foundryv1.FoundryQuestion{Id: "question-1", Status: "queued", CreatedAt: jobCreated, ResultJson: `{"status":"answered","answer":{"blocks":[]},"citations":[]}`},
		generation: &foundryv1.OntologyGeneration{Id: "generation-1", Status: foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_RUNNING, CreatedAt: jobCreated},
		load:       &foundryv1.FoundryLoad{Id: "load-1", Status: "writing", CreatedAt: jobCreated},
		sync:       &foundryv1.ConnectorSync{Id: "sync-1", ConnectorId: "connector-1", Status: foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_QUEUED, CreatedAt: jobCreated},
	}
}

func (b *jobsBackend) set(change func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	change()
}

func (b *jobsBackend) AskFoundryQuestion(context.Context, *foundryv1.AskFoundryQuestionRequest, ...grpc.CallOption) (*foundryv1.FoundryQuestionResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &foundryv1.FoundryQuestionResponse{Question: proto.Clone(b.question).(*foundryv1.FoundryQuestion)}, nil
}

func (b *jobsBackend) GetFoundryQuestion(_ context.Context, r *foundryv1.GetFoundryQuestionRequest, _ ...grpc.CallOption) (*foundryv1.FoundryQuestionResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	if r.Id != b.question.Id {
		return nil, status.Error(codes.NotFound, "question not found")
	}
	return &foundryv1.FoundryQuestionResponse{Question: proto.Clone(b.question).(*foundryv1.FoundryQuestion)}, nil
}

// jobsActions are jobsBackend's actions: cancelling its question and syncing its connector.
type jobsActions struct {
	FoundryActionAPI
	*jobsBackend
}

func (a jobsActions) CancelFoundryQuestion(ctx context.Context, r *foundryv1.CancelFoundryQuestionRequest, opts ...grpc.CallOption) (*foundryv1.FoundryQuestionResponse, error) {
	return a.jobsBackend.CancelFoundryQuestion(ctx, r, opts...)
}

func (a jobsActions) SyncConnector(ctx context.Context, r *foundryv1.SyncConnectorRequest, opts ...grpc.CallOption) (*foundryv1.ConnectorSyncResponse, error) {
	return a.jobsBackend.SyncConnector(ctx, r, opts...)
}

func (b *jobsBackend) CancelFoundryQuestion(_ context.Context, r *foundryv1.CancelFoundryQuestionRequest, _ ...grpc.CallOption) (*foundryv1.FoundryQuestionResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelled = append(b.cancelled, r.Id)
	b.question.Status = "cancelled"
	return &foundryv1.FoundryQuestionResponse{Question: proto.Clone(b.question).(*foundryv1.FoundryQuestion)}, nil
}

func (b *jobsBackend) GetOntologyGeneration(context.Context, *foundryv1.GetOntologyGenerationRequest, ...grpc.CallOption) (*foundryv1.OntologyGenerationResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	return &foundryv1.OntologyGenerationResponse{Generation: proto.Clone(b.generation).(*foundryv1.OntologyGeneration)}, nil
}

func (b *jobsBackend) GetFoundryLoad(context.Context, *foundryv1.GetFoundryLoadRequest, ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	return &foundryv1.FoundryLoadResponse{Load: proto.Clone(b.load).(*foundryv1.FoundryLoad)}, nil
}

func (b *jobsBackend) GetConnectorSync(context.Context, *foundryv1.GetConnectorSyncRequest, ...grpc.CallOption) (*foundryv1.ConnectorSyncResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	return &foundryv1.ConnectorSyncResponse{Sync: proto.Clone(b.sync).(*foundryv1.ConnectorSync)}, nil
}

func (b *jobsBackend) SyncConnector(context.Context, *foundryv1.SyncConnectorRequest, ...grpc.CallOption) (*foundryv1.ConnectorSyncResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.synced++
	return &foundryv1.ConnectorSyncResponse{Sync: proto.Clone(b.sync).(*foundryv1.ConnectorSync)}, nil
}

// jobsIdentity is a user who may do anything in workspace-1 and nothing anywhere else.
func jobsIdentity() *Identity {
	identity := everyTool()
	identity.Authorize = func(_ context.Context, r any, _ Grant, _ ...string) error {
		if r.(interface{ GetWorkspaceId() string }).GetWorkspaceId() != "workspace-1" {
			return status.Error(codes.PermissionDenied, "refused")
		}
		return nil
	}
	identity.WithScope = func(ctx context.Context, _ string, _ bool) context.Context { return ctx }
	return identity
}

// jobsService waits long enough that a test which waited would notice.
func jobsService(b *jobsBackend) *Service {
	return &Service{Foundry: b, Actions: jobsActions{jobsBackend: b}, Confirmations: &memConfirmations{}, FoundryURL: "https://foundry.example",
		Wait: 300 * time.Millisecond, Polls: []time.Duration{10 * time.Millisecond}}
}

var askArgs = map[string]any{"workspace_id": "workspace-1", "ontology_id": "ontology-1", "question": "How many customers?", "client_token": "token-1"}

func TestServerDeclaresTasks(t *testing.T) {
	w := newWire(t, jobsService(newJobsBackend()), jobsIdentity(), tasksClient)
	discovered := w.call("server/discover", nil)
	assert.Contains(t, discovered["capabilities"].(map[string]any)["extensions"], TasksExtension)
}

// A client that takes tasks gets a live question as a task at once, polls it while it runs, and
// gets the tool's result, answer card and all, once it ends.
func TestAskHandsALiveQuestionToATask(t *testing.T) {
	b := newJobsBackend()
	w := newWire(t, jobsService(b), jobsIdentity(), tasksClient)
	started := time.Now()
	answer := w.tool(toolFoundryAsk, askArgs, nil)
	assert.Less(t, time.Since(started), 250*time.Millisecond, "the call didn't wait")
	assert.Equal(t, "task", answer["resultType"])
	assert.Equal(t, "q.workspace-1.ontology-1.question-1", answer["taskId"])
	assert.Equal(t, "working", answer["status"])
	assert.Equal(t, "Question queued", answer["statusMessage"])
	assert.Equal(t, "2026-11-24T09:30:00Z", answer["createdAt"])
	assert.NotEmpty(t, answer["lastUpdatedAt"])
	assert.Contains(t, answer, "ttlMs")
	assert.Nil(t, answer["ttlMs"])
	assert.EqualValues(t, 2000, answer["pollIntervalMs"])
	assert.NotContains(t, answer, "content")

	state := w.call("tasks/get", map[string]any{"taskId": answer["taskId"]})
	assert.Equal(t, "complete", state["resultType"])
	assert.Equal(t, "working", state["status"])
	assert.Equal(t, "2026-11-24T09:30:00Z", state["createdAt"])
	assert.NotContains(t, state, "result")

	b.set(func() { b.question.Status = "answered" })
	state = w.call("tasks/get", map[string]any{"taskId": answer["taskId"]})
	assert.Equal(t, "completed", state["status"])
	assert.Equal(t, "2026-11-24T09:30:00Z", state["createdAt"], "a completed task keeps when its resource was made")
	result := state["result"].(map[string]any)
	assert.NotEqual(t, true, result["isError"])
	content := result["content"].([]any)
	require.Len(t, content, 2)
	assert.Equal(t, untrustedContentNotice, content[0].(map[string]any)["text"])
	assert.Equal(t, "answered", result["structuredContent"].(map[string]any)["status"])
	assert.Contains(t, result["_meta"], appMetaKey, "the answer card rides on the task's result")
}

// A client that doesn't take tasks waits, as before; and a question that ended at once is its
// result, never a task.
func TestAskWaitsWithoutTasksAndAnswersAnEndedQuestionAtOnce(t *testing.T) {
	b := newJobsBackend()
	w := newWire(t, jobsService(b), jobsIdentity(), map[string]any{})
	started := time.Now()
	result := w.tool(toolFoundryAsk, askArgs, nil)
	assert.GreaterOrEqual(t, time.Since(started), 250*time.Millisecond, "the call waited")
	assert.NotEqual(t, "task", result["resultType"])
	assert.Equal(t, "queued", result["structuredContent"].(map[string]any)["status"])

	b.set(func() { b.question.Status = "answered" })
	w.caps = tasksClient
	result = w.tool(toolFoundryAsk, askArgs, nil)
	assert.Equal(t, "complete", result["resultType"])
	assert.Equal(t, "answered", result["structuredContent"].(map[string]any)["status"])
}

// get_question, and the get tools of a generation, load or sync asked to wait, hand their wait
// to a task; asked not to wait, they answer as they did.
func TestGetToolsHandTheirWaitToATask(t *testing.T) {
	cases := []struct {
		tool    string
		args    map[string]any
		taskID  string
		poll    float64
		message string
		end     func(*jobsBackend)
		ended   func(*testing.T, map[string]any)
	}{
		{toolFoundryGetQuestion, map[string]any{"workspace_id": "workspace-1", "ontology_id": "ontology-1", "question_id": "question-1"},
			"q.workspace-1.ontology-1.question-1", 2000, "Question queued",
			func(b *jobsBackend) { b.question.Status = "failed" },
			func(t *testing.T, out map[string]any) { assert.Equal(t, "failed", out["status"]) }},
		{toolFoundryGetGeneration, map[string]any{"workspace_id": "workspace-1", "ontology_id": "ontology-1", "generation_id": "generation-1", "wait": true},
			"g.workspace-1.ontology-1.generation-1", 10000, "Generation running",
			func(b *jobsBackend) {
				b.generation.Status = foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_SUCCEEDED
			},
			func(t *testing.T, out map[string]any) {
				assert.Equal(t, "succeeded", out["generation"].(map[string]any)["status"])
			}},
		{toolFoundryGetLoad, map[string]any{"workspace_id": "workspace-1", "ontology_id": "ontology-1", "load_id": "load-1", "wait": true},
			"l.workspace-1.ontology-1.load-1", 10000, "Load writing",
			func(b *jobsBackend) { b.load.Status = "ready" },
			func(t *testing.T, out map[string]any) {
				assert.Equal(t, "ready", out["load"].(map[string]any)["status"])
			}},
		{toolFoundryGetSync, map[string]any{"workspace_id": "workspace-1", "connector_id": "connector-1", "sync_id": "sync-1", "wait": true},
			"s.workspace-1.connector-1.sync-1", 5000, "Sync queued",
			func(b *jobsBackend) { b.sync.Status = foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_SUCCEEDED },
			func(t *testing.T, out map[string]any) {
				assert.Equal(t, "succeeded", out["sync"].(map[string]any)["status"])
			}},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			b := newJobsBackend()
			w := newWire(t, jobsService(b), jobsIdentity(), tasksClient)
			answer := w.tool(c.tool, c.args, nil)
			assert.Equal(t, "task", answer["resultType"])
			assert.Equal(t, c.taskID, answer["taskId"])
			assert.Equal(t, c.message, answer["statusMessage"])
			assert.EqualValues(t, c.poll, answer["pollIntervalMs"])
			assert.Equal(t, "working", w.call("tasks/get", map[string]any{"taskId": c.taskID})["status"])

			b.set(func() { c.end(b) })
			state := w.call("tasks/get", map[string]any{"taskId": c.taskID})
			assert.Equal(t, "completed", state["status"])
			c.ended(t, state["result"].(map[string]any)["structuredContent"].(map[string]any))

			if _, waits := c.args["wait"]; waits {
				b := newJobsBackend()
				w := newWire(t, jobsService(b), jobsIdentity(), tasksClient)
				args := map[string]any{}
				for k, v := range c.args {
					args[k] = v
				}
				delete(args, "wait")
				assert.NotEqual(t, "task", w.tool(c.tool, args, nil)["resultType"], "a get not asked to wait answers at once")
				assert.Equal(t, 1, b.reads)
			}
		})
	}
}

// tasks/get is the get tool's call, as the caller: a resource they can't read, or one that isn't
// what the ID says, completes as that tool's error; an ID the server never made is Invalid params.
func TestTasksGetIsTheCallersOwnCall(t *testing.T) {
	b := newJobsBackend()
	w := newWire(t, jobsService(b), jobsIdentity(), tasksClient)
	refused := w.call("tasks/get", map[string]any{"taskId": "q.workspace-2.ontology-1.question-1"})
	assert.Equal(t, "completed", refused["status"])
	assert.Equal(t, true, refused["result"].(map[string]any)["isError"])

	otherConnector := w.call("tasks/get", map[string]any{"taskId": "s.workspace-1.connector-2.sync-1"})
	assert.Equal(t, true, otherConnector["result"].(map[string]any)["isError"])
	assert.Contains(t, otherConnector["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"], "not_found")

	for _, id := range []string{"", "q", "x.a.b.c", "q.a.b", "q.a.b.c.d", "q.a b.c.d", "q.a.b.c/d", "q." + strings.Repeat("a", 127) + ".b.c"} {
		for _, method := range []string{"tasks/get", "tasks/update", "tasks/cancel"} {
			failed := w.call(method, map[string]any{"taskId": id})
			require.Contains(t, failed, "error", "%s %q", method, id)
			assert.EqualValues(t, -32602, failed["error"].(map[string]any)["code"], "%s %q", method, id)
		}
	}
}

// Cancelling a question's task stops the question; a generation's, load's or sync's only stops
// the caller watching, since someone started it on purpose. Both are acknowledged.
func TestTasksCancelStopsOnlyAQuestion(t *testing.T) {
	b := newJobsBackend()
	w := newWire(t, jobsService(b), jobsIdentity(), tasksClient)
	ack := w.call("tasks/cancel", map[string]any{"taskId": "q.workspace-1.ontology-1.question-1"})
	assert.Equal(t, "complete", ack["resultType"])
	assert.Equal(t, []string{"question-1"}, b.cancelled)
	state := w.call("tasks/get", map[string]any{"taskId": "q.workspace-1.ontology-1.question-1"})
	assert.Equal(t, "completed", state["status"])
	assert.Equal(t, "cancelled", state["result"].(map[string]any)["structuredContent"].(map[string]any)["status"])

	for _, id := range []string{"g.workspace-1.ontology-1.generation-1", "l.workspace-1.ontology-1.load-1", "s.workspace-1.connector-1.sync-1"} {
		assert.Equal(t, "complete", w.call("tasks/cancel", map[string]any{"taskId": id})["resultType"])
	}
	assert.Equal(t, []string{"question-1"}, b.cancelled)

	refused := w.call("tasks/cancel", map[string]any{"taskId": "q.workspace-2.ontology-1.question-1"})
	assert.Equal(t, "complete", refused["resultType"], "a cancel the caller may not make is acknowledged, and changes nothing")
	assert.Equal(t, []string{"question-1"}, b.cancelled)
	assert.Equal(t, "complete", w.call("tasks/update", map[string]any{"taskId": "q.workspace-1.ontology-1.question-1", "inputResponses": map[string]any{}})["resultType"])
}

func TestTaskIDsReadBack(t *testing.T) {
	for _, task := range []foundryTask{
		questionTask{"w", "o", "q"}, generationTask{"w-1", "o_2", "g"}, loadTask{"w", "o", "l"}, syncTask{"w", "c", "s"},
	} {
		parsed, ok := parseTaskID(task.ID())
		require.True(t, ok, task.ID())
		assert.Equal(t, task, parsed)
	}
}

// A question whose IDs make a task ID longer than the extension allows is waited on, as before.
func TestAnOverlongTaskIDIsWaitedOn(t *testing.T) {
	b := newJobsBackend()
	long := strings.Repeat("o", 120)
	identity := jobsIdentity()
	w := newWire(t, jobsService(b), identity, tasksClient)
	args := map[string]any{"workspace_id": "workspace-1", "ontology_id": long, "question": "How many?", "client_token": "token-1"}
	started := time.Now()
	result := w.tool(toolFoundryAsk, args, nil)
	assert.NotEqual(t, "task", result["resultType"])
	assert.GreaterOrEqual(t, time.Since(started), 250*time.Millisecond)
}
