package integration

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/url"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Foundry's MCP Apps: views that a host offering MCP Apps (the io.modelcontextprotocol/ui
// extension) draws in place of a tool's text. A tool names its view in _meta.ui.resourceUri, and
// the view is one HTML document served as that ui:// resource. `pnpm mcp-apps` builds it from the
// website's own components (src/mcp-apps) into apps/, committed, since hosted Helix builds this
// module from its source. A host without MCP Apps ignores both, and every agent reads what it read
// before: what only a view needs rides in the tool result's _meta, which hosts keep out of the
// model's context. No view calls a tool through its host: addFoundryTool makes every tool's
// visibility "model", which a host enforces.
const (
	// appMIMEType is an MCP App's HTML.
	appMIMEType = "text/html;profile=mcp-app"
	// answerAppURI is a question's view: its answer, with the charts and tables the website draws.
	answerAppURI = "ui://helix-foundry/answer"
	// appMetaKey names what a view reads beside a tool's output in its result's _meta.
	appMetaKey = "com.helix-db/foundry"
)

//go:embed apps/answer.html
var answerAppHTML string

// answerApp is the _meta of a tool whose result the answer view draws.
var answerApp = mcp.Meta{"ui": map[string]any{"resourceUri": answerAppURI}}

// viewOutput is a tool output with what its view reads beyond the agent's output (appView), which
// addFoundryTool puts in the result's _meta under appMetaKey, beside the SDK's own keys.
type viewOutput interface{ appView() any }

// addFoundryApps serves the views of the tools AddFoundryTools registers.
func addFoundryApps(server *mcp.Server) {
	server.AddResource(&mcp.Resource{
		URI: answerAppURI, Name: "foundry_answer", Title: "Foundry answer", MIMEType: appMIMEType,
		Description: "A Foundry question's answer, with its charts and tables.",
	}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: answerAppURI, MIMEType: appMIMEType, Text: answerAppHTML,
			// No csp: the view's script and styles are inline and it loads nothing, so the
			// host's restrictive default suits it.
			Meta: mcp.Meta{"ui": map[string]any{"prefersBorder": true}},
		}}}, nil
	})
}

// questionView is what the answer view reads beside a question: the outputs its visuals show,
// which the agent's output leaves out (FoundryQuestion), and the question's thread in Foundry.
type questionView struct {
	AskURL  string                     `json:"ask_url,omitempty"`
	Outputs map[string]json.RawMessage `json:"outputs,omitempty"`
}

func (o FoundryQuestionOutput) appView() any { return o.view }

// questionView is q's view in the workspace's ontology: its thread's link on a stamp that names
// the website.
func (s *Service) questionView(workspaceID, ontologyID string, q *foundryv1.FoundryQuestion) questionView {
	view := questionView{Outputs: ontologyagent.VisualOutputs(json.RawMessage(q.GetResultJson()))}
	if link := s.FoundryLink(workspaceID, "ask", ontologyID); link != "" {
		view.AskURL = link + "&" + url.Values{"thread": {q.GetThreadId()}}.Encode()
	}
	return view
}
