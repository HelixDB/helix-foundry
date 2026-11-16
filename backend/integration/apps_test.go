package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// questionBackend answers ask and get_question with one question.
type questionBackend struct {
	foundryv1.FoundryServiceClient
	question *foundryv1.FoundryQuestion
}

func (b *questionBackend) AskFoundryQuestion(context.Context, *foundryv1.AskFoundryQuestionRequest, ...grpc.CallOption) (*foundryv1.FoundryQuestionResponse, error) {
	return &foundryv1.FoundryQuestionResponse{Question: proto.Clone(b.question).(*foundryv1.FoundryQuestion)}, nil
}

func (b *questionBackend) GetFoundryQuestion(context.Context, *foundryv1.GetFoundryQuestionRequest, ...grpc.CallOption) (*foundryv1.FoundryQuestionResponse, error) {
	return &foundryv1.FoundryQuestionResponse{Question: proto.Clone(b.question).(*foundryv1.FoundryQuestion)}, nil
}

// visualAnswer shows e2's output in a visual; e3's output is shown by none.
const visualAnswer = `{"status":"answered","answer":{"blocks":[{"para":"Three are paid [c1]."}],"visuals":[{"evidence_id":"e2","kind":"bar"}]},` +
	`"citations":[{"marker":"c1","evidence_id":"e2","target":{"result":{"evidence_id":"e2"}}}],` +
	`"queries":[{"evidence_id":"e2","query":{"start":{"type":"Invoice"}},"output":{"kind":"count","count":"shown-output"}},` +
	`{"evidence_id":"e3","query":{"start":{"type":"Invoice"}},"output":{"kind":"count","count":"hidden-output"}}],` +
	`"usage":{"input":1,"cache_read":0,"cache_write":0,"output":1,"usd":0.01}}`

func questionSession(t *testing.T, s *Service) *mcp.ClientSession {
	t.Helper()
	identity := everyTool()
	identity.Authorize = func(context.Context, any, Grant, ...string) error { return nil }
	return connect(t, s, identity)
}

// A host with MCP Apps finds the answer view on ask and get_question, and only there, and reads it
// as one HTML document that asks for a border and loads nothing.
func TestAskAndGetQuestionNameTheAnswerView(t *testing.T) {
	session := questionSession(t, &Service{FoundryURL: "https://foundry.example", Events: true})
	ctx := context.Background()
	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	viewed := map[string]any{}
	for _, tool := range tools.Tools {
		if tool.Meta != nil {
			viewed[tool.Name] = tool.Meta["ui"]
		}
	}
	view := map[string]any{"resourceUri": answerAppURI}
	assert.Equal(t, map[string]any{toolFoundryAsk: view, toolFoundryGetQuestion: view}, viewed)

	resources, err := session.ListResources(ctx, nil)
	require.NoError(t, err)
	require.Len(t, resources.Resources, 1)
	assert.Equal(t, answerAppURI, resources.Resources[0].URI)
	assert.Equal(t, "text/html;profile=mcp-app", resources.Resources[0].MIMEType)

	read, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: answerAppURI})
	require.NoError(t, err)
	require.Len(t, read.Contents, 1)
	html := read.Contents[0]
	assert.Equal(t, "text/html;profile=mcp-app", html.MIMEType)
	assert.True(t, strings.HasPrefix(html.Text, "<!doctype html>"), "a view is an HTML document")
	assert.Equal(t, map[string]any{"ui": map[string]any{"prefersBorder": true}}, map[string]any(html.Meta))
	// Its script and styles are inline: the host's default CSP lets it load nothing else.
	for _, external := range []string{"<script src", "<link", "<img src"} {
		assert.NotContains(t, html.Text, external)
	}
}

// An identity that can't ask gets no question tools, and so no view.
func TestNoViewWithoutTheQuestionTools(t *testing.T) {
	session := connect(t, &Service{}, &Identity{User: true, HasPermission: func(p Permission) bool { return p == PermissionWorkspaceRead }})
	assert.Nil(t, session.InitializeResult().Capabilities.Resources)
}

// The view gets the outputs the answer's visuals show and its thread's link in the result's _meta;
// the agent's output, in content and structuredContent, leaves every output out as before.
func TestAnswerViewReadsOutputsTheAgentDoesNot(t *testing.T) {
	question := &foundryv1.FoundryQuestion{Id: "question-1", ThreadId: "thread-1", LoadId: "load-1", Question: "How many are paid?",
		Status: "answered", ResultJson: visualAnswer}
	for _, call := range []mcp.CallToolParams{
		{Name: toolFoundryAsk, Arguments: map[string]any{"workspace_id": "workspace-1", "ontology_id": "ontology-1",
			"question": "How many are paid?", "client_token": "token-1"}},
		{Name: toolFoundryGetQuestion, Arguments: map[string]any{"workspace_id": "workspace-1", "ontology_id": "ontology-1",
			"question_id": "question-1"}},
	} {
		t.Run(call.Name, func(t *testing.T) {
			session := questionSession(t, &Service{Foundry: &questionBackend{question: question}, FoundryURL: "https://foundry.example"})
			result, err := session.CallTool(context.Background(), &call)
			require.NoError(t, err)
			require.False(t, result.IsError, result.Content)

			view, err := json.Marshal(result.Meta[appMetaKey])
			require.NoError(t, err)
			assert.JSONEq(t, `{"ask_url":"https://foundry.example/ask/ontology-1?workspace=workspace-1&thread=thread-1",
				"outputs":{"e2":{"kind":"count","count":"shown-output"}}}`, string(view))

			text := result.Content[len(result.Content)-1].(*mcp.TextContent).Text
			structured, err := json.Marshal(result.StructuredContent)
			require.NoError(t, err)
			for _, agent := range []string{text, string(structured)} {
				assert.Contains(t, agent, `"status":"answered"`)
				assert.NotContains(t, agent, "shown-output")
				assert.NotContains(t, agent, "hidden-output")
				assert.NotContains(t, agent, "ask_url")
			}
		})
	}
}

// A question still running, or on a stamp that names no website, gives the view what there is.
func TestAnswerViewOfARunningQuestionWithoutAWebsite(t *testing.T) {
	question := &foundryv1.FoundryQuestion{Id: "question-1", ThreadId: "question-1", Status: "running"}
	s := &Service{Foundry: &questionBackend{question: question}, Wait: time.Millisecond, Polls: []time.Duration{time.Millisecond}}
	result, err := questionSession(t, s).CallTool(context.Background(), &mcp.CallToolParams{Name: toolFoundryGetQuestion,
		Arguments: map[string]any{"workspace_id": "workspace-1", "ontology_id": "ontology-1", "question_id": "question-1"}})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{}, result.Meta[appMetaKey])
}

// A tool whose view reads nothing more sends no _meta.
func TestOtherToolsSendNoMeta(t *testing.T) {
	b := &graphBackend{}
	result, err := questionSession(t, &Service{Foundry: b}).CallTool(context.Background(), &mcp.CallToolParams{Name: toolFoundryReadGraph,
		Arguments: json.RawMessage(`{"workspace_id":"workspace-1","ontology_id":"ontology-1","load_id":"load-1","reads":[{"query":{}}]}`)})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Content)
	assert.NotContains(t, result.Meta, appMetaKey)
}
