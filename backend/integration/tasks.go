package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TasksExtension is the MCP Tasks extension (protocol 2026-07-28): a client that declares it on a
// tools/call takes a task in place of the tool's result, and polls it with tasks/get until it
// ends. A host declares it in its server's capabilities (ServerCapabilities.AddExtension) on a
// server whose Foundry tools AddFoundryTools registered.
const TasksExtension = "io.modelcontextprotocol/tasks"

// Tasks are what Foundry's tools would otherwise wait on: a question (helix_foundry_ask and
// get_question), and a generation, load or sync whose get tool was asked to wait. Each is one
// Foundry resource the backend keeps, so a task holds nothing of its own: its ID names the
// resource, and tasks/get calls the get tool again, as the caller, without waiting. A server
// that never stored a task can't lose one, and any replica answers for it.
type foundryTask interface {
	// ID is the task's wire ID: "<kind>.<id>.<id>.<id>", one letter of kind then the IDs its get
	// tool takes. IDs are [A-Za-z0-9_-], so the dots can't be in them.
	ID() string
	// pollInterval is how often a client should look: about how fast the resource moves.
	pollInterval() time.Duration
	// get runs the task's get tool once, without waiting, under offer.
	get(ctx context.Context, s *Service, identity *Identity, req *mcp.CallToolRequest) (*mcp.CallToolResult, error)
	// cancel stops what the task waits on, where Foundry can: a question; the others are jobs
	// someone started on purpose, which only stop the caller watching them.
	cancel(ctx context.Context, s *Service, identity *Identity, req *mcp.CallToolRequest) error
}

type questionTask struct{ workspaceID, ontologyID, questionID string }
type generationTask struct{ workspaceID, ontologyID, generationID string }
type loadTask struct{ workspaceID, ontologyID, loadID string }
type syncTask struct{ workspaceID, connectorID, syncID string }

func (t questionTask) ID() string {
	return "q." + t.workspaceID + "." + t.ontologyID + "." + t.questionID
}
func (t generationTask) ID() string {
	return "g." + t.workspaceID + "." + t.ontologyID + "." + t.generationID
}
func (t loadTask) ID() string { return "l." + t.workspaceID + "." + t.ontologyID + "." + t.loadID }
func (t syncTask) ID() string { return "s." + t.workspaceID + "." + t.connectorID + "." + t.syncID }

func (questionTask) pollInterval() time.Duration   { return 2 * time.Second }
func (generationTask) pollInterval() time.Duration { return 10 * time.Second }
func (loadTask) pollInterval() time.Duration       { return 10 * time.Second }
func (syncTask) pollInterval() time.Duration       { return 5 * time.Second }

func (t questionTask) get(ctx context.Context, s *Service, identity *Identity, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	input := GetFoundryQuestionInput{OntologyInput: OntologyInput{WorkspaceID: t.workspaceID, OntologyID: t.ontologyID}, QuestionID: t.questionID}
	return taskToolResult(callFoundryTool(ctx, s, identity, toolFoundryGetQuestion, req, input, s.GetFoundryQuestion))
}

func (t generationTask) get(ctx context.Context, s *Service, identity *Identity, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	input := GetFoundryGenerationInput{OntologyInput: OntologyInput{WorkspaceID: t.workspaceID, OntologyID: t.ontologyID}, GenerationID: t.generationID, Wait: true}
	return taskToolResult(callFoundryTool(ctx, s, identity, toolFoundryGetGeneration, req, input, s.GetFoundryGeneration))
}

func (t loadTask) get(ctx context.Context, s *Service, identity *Identity, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	input := GetFoundryLoadInput{OntologyInput: OntologyInput{WorkspaceID: t.workspaceID, OntologyID: t.ontologyID}, LoadID: t.loadID, Wait: true}
	return taskToolResult(callFoundryTool(ctx, s, identity, toolFoundryGetLoad, req, input, s.GetFoundryLoad))
}

func (t syncTask) get(ctx context.Context, s *Service, identity *Identity, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	input := GetFoundrySyncInput{WorkspaceID: t.workspaceID, ConnectorID: t.connectorID, SyncID: t.syncID, Wait: true}
	return taskToolResult(callFoundryTool(ctx, s, identity, toolFoundryGetSync, req, input, s.GetFoundrySync))
}

func (t questionTask) cancel(ctx context.Context, s *Service, identity *Identity, req *mcp.CallToolRequest) error {
	input := GetFoundryQuestionInput{OntologyInput: OntologyInput{WorkspaceID: t.workspaceID, OntologyID: t.ontologyID}, QuestionID: t.questionID}
	_, _, err := callFoundryTool(ctx, s, identity, toolFoundryCancelQuestion, req, input, s.CancelFoundryQuestion)
	return err
}

func (generationTask) cancel(context.Context, *Service, *Identity, *mcp.CallToolRequest) error {
	return nil
}
func (loadTask) cancel(context.Context, *Service, *Identity, *mcp.CallToolRequest) error { return nil }
func (syncTask) cancel(context.Context, *Service, *Identity, *mcp.CallToolRequest) error { return nil }

// maxTaskID is the extension's bound on a task's ID; a resource whose IDs make a longer one is
// waited on as before, never handed out as a task.
const maxTaskID = 128

// parseTaskID reads a task ID back, refusing anything ID didn't make.
func parseTaskID(id string) (foundryTask, bool) {
	parts := strings.Split(id, ".")
	if len(id) > maxTaskID || len(parts) != 4 || !ValidIDs(parts[1:]) {
		return nil, false
	}
	a, b, c := parts[1], parts[2], parts[3]
	switch parts[0] {
	case "q":
		return questionTask{a, b, c}, true
	case "g":
		return generationTask{a, b, c}, true
	case "l":
		return loadTask{a, b, c}, true
	case "s":
		return syncTask{a, b, c}, true
	}
	return nil, false
}

// taskOffer is a call's way to hand its wait to a task: present in ctx when the client takes
// tasks (a tools/call that declared the extension) or on tasks/get, which must never wait. A
// handler that would wait sets it (waitOrTask) and returns what it read; the call then answers
// with the task.
type taskOffer struct {
	task      foundryTask
	createdAt time.Time
	message   string
}

type taskOfferKey struct{}

func offerOf(ctx context.Context) *taskOffer {
	offer, _ := ctx.Value(taskOfferKey{}).(*taskOffer)
	return offer
}

// waitOrTask is waitWhile, except that a call whose client takes tasks doesn't wait: while live
// holds of current, it hands task to the call's offer and returns current as it is.
func waitOrTask[T interface{ GetCreatedAt() *timestamppb.Timestamp }](
	ctx context.Context, s *Service, started time.Time, current T, live func(T) bool, get func(context.Context) (T, error),
	task foundryTask, message func(T) string,
) (T, error) {
	offer := offerOf(ctx)
	if offer == nil || len(task.ID()) > maxTaskID {
		return waitWhile(ctx, s, started, current, live, get)
	}
	offer.createdAt = current.GetCreatedAt().AsTime()
	if live(current) {
		offer.task, offer.message = task, message(current)
	}
	return current, nil
}

// clientTakesTasks reports whether req's client declared the Tasks extension, which it does only
// on protocol 2026-07-28 or later.
func clientTakesTasks(req *mcp.CallToolRequest) bool {
	caps := req.ClientCapabilities()
	if caps == nil {
		return false
	}
	_, ok := caps.Extensions[TasksExtension]
	return ok
}

// taskStatus is a task's status: working until its resource ends, then completed with the tool's
// result, whether the resource succeeded or not (the result says which). Foundry never needs a
// client's input, and a cancelled question is a result too.
type taskStatus string

const (
	taskWorking   taskStatus = "working"
	taskCompleted taskStatus = "completed"
)

// taskFields are what a task answer and tasks/get both carry. TTLMs is always null: the task is
// its resource, which outlives any poll.
type taskFields struct {
	TaskID         string     `json:"taskId"`
	Status         taskStatus `json:"status"`
	StatusMessage  string     `json:"statusMessage,omitempty"`
	CreatedAt      string     `json:"createdAt"`
	LastUpdatedAt  string     `json:"lastUpdatedAt"`
	TTLMs          *int64     `json:"ttlMs"`
	PollIntervalMs int64      `json:"pollIntervalMs"`
}

// taskAnswer is a tools/call's result when it hands its wait to a task.
type taskAnswer struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
	taskFields
}

// taskState is tasks/get's result: the task, and the tool's result once it completed.
type taskState struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
	taskFields
	Result *mcp.CallToolResult `json:"result,omitempty"`
}

// taskAck is tasks/update's and tasks/cancel's result: nothing but that it was taken.
type taskAck struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
}

type taskParams struct {
	mcp.ParamsBase
	TaskID string `json:"taskId"`
}

func fieldsOf(offer *taskOffer, now time.Time) taskFields {
	return taskFields{
		TaskID: offer.task.ID(), Status: taskWorking, StatusMessage: offer.message,
		CreatedAt: offer.createdAt.UTC().Format(time.RFC3339Nano), LastUpdatedAt: now.UTC().Format(time.RFC3339Nano),
		PollIntervalMs: offer.task.pollInterval().Milliseconds(),
	}
}

// addFoundryTasks makes server answer for Foundry's tasks: a tools/call whose client takes tasks
// answers with one in place of a wait, and tasks/get, tasks/update and tasks/cancel act on them
// as identity, each through the tool it names, so each is authorized as that tool's call is.
func (s *Service) addFoundryTasks(server *mcp.Server, identity *Identity) {
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			call, ok := req.(*mcp.CallToolRequest)
			if method != "tools/call" || !ok || !clientTakesTasks(call) {
				return next(ctx, method, req)
			}
			offer := &taskOffer{}
			result, err := next(context.WithValue(ctx, taskOfferKey{}, offer), method, req)
			if err != nil || offer.task == nil {
				return result, err
			}
			return &taskAnswer{ResultType: "task", taskFields: fieldsOf(offer, time.Now())}, nil
		}
	})
	// Methods are added once per server; AddReceivingCustomMethod fails only for a standard
	// method's name, which these are not.
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	must(mcp.AddReceivingCustomMethod(server, "tasks/get", func(ctx context.Context, ss *mcp.ServerSession, params *taskParams) (*taskState, error) {
		task, ok := parseTaskID(params.TaskID)
		if !ok {
			return nil, unknownTask(params.TaskID)
		}
		offer := &taskOffer{}
		result, err := task.get(context.WithValue(ctx, taskOfferKey{}, offer), s, identity, taskRequest(ss, params))
		if err != nil {
			return nil, err
		}
		now := time.Now()
		if offer.task != nil {
			return &taskState{ResultType: "complete", taskFields: fieldsOf(offer, now)}, nil
		}
		at := now.UTC().Format(time.RFC3339Nano)
		created := at
		if !offer.createdAt.IsZero() && offer.createdAt.Unix() != 0 {
			// A resource the call read says when it was made; a failed call read none.
			created = offer.createdAt.UTC().Format(time.RFC3339Nano)
		}
		return &taskState{ResultType: "complete", Result: result, taskFields: taskFields{
			TaskID: params.TaskID, Status: taskCompleted, CreatedAt: created, LastUpdatedAt: at, PollIntervalMs: task.pollInterval().Milliseconds(),
		}}, nil
	}))
	must(mcp.AddReceivingCustomMethod(server, "tasks/update", func(_ context.Context, _ *mcp.ServerSession, params *taskParams) (*taskAck, error) {
		if _, ok := parseTaskID(params.TaskID); !ok {
			return nil, unknownTask(params.TaskID)
		}
		// No Foundry task asks for input, so there is nothing to take.
		return &taskAck{ResultType: "complete"}, nil
	}))
	must(mcp.AddReceivingCustomMethod(server, "tasks/cancel", func(ctx context.Context, ss *mcp.ServerSession, params *taskParams) (*taskAck, error) {
		task, ok := parseTaskID(params.TaskID)
		if !ok {
			return nil, unknownTask(params.TaskID)
		}
		// Cancelling is cooperative: a question that already ended, or one the caller may not
		// cancel, stays as it is, and tasks/get says so.
		_ = task.cancel(ctx, s, identity, taskRequest(ss, params))
		return &taskAck{ResultType: "complete"}, nil
	}))
}

// taskRequest is the tools/call a task method makes of its tool: the same session and request
// metadata (the client's capabilities and protocol version), with no arguments of its own.
func taskRequest(ss *mcp.ServerSession, params *taskParams) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Session: ss, Params: &mcp.CallToolParamsRaw{Meta: params.Meta}}
}

// AddExtensions declares on caps the MCP extensions Foundry's tools take part in: Tasks.
func AddExtensions(caps *mcp.ServerCapabilities) {
	caps.AddExtension(TasksExtension, map[string]any{})
}

// unknownTask is the extension's answer to a task ID this server never made: Invalid params.
func unknownTask(id string) error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown task %q", id)}
}

// taskToolResult is a tool's result as tools/call would carry it: the tool's error as an error
// result, its output as structured content beside the text callFoundryTool wrote.
func taskToolResult[Out any](result *mcp.CallToolResult, out Out, err error) (*mcp.CallToolResult, error) {
	var wire *jsonrpc.Error
	if errors.As(err, &wire) {
		return nil, err
	}
	if err != nil {
		var failed mcp.CallToolResult
		failed.SetError(err)
		return &failed, nil
	}
	result.StructuredContent = out
	return result, nil
}
