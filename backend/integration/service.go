// Package integration exposes Foundry's operations to agent tools and HTTP clients.
// Authentication, authorization and hosting-specific resources are supplied by the host.
package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Grant string

const (
	UserOnly    Grant = "user"
	QueryRead   Grant = "database.query.read"
	QueryWrite  Grant = "database.query.write"
	ProjectRead Grant = "project.read"
)

type Permission string

const (
	PermissionWorkspaceRead             Permission = "workspace.read"
	PermissionDatabaseQueryRead         Permission = "database.query.read"
	PermissionDatabaseQueryWrite        Permission = "database.query.write"
	PermissionWorkspaceConnectorsManage Permission = "workspace.connectors.manage"
	PermissionWorkspaceOntologyManage   Permission = "workspace.ontology.manage"
)

// Identity contains only trusted, host-derived identity and authorization decisions.
// None of its callbacks may trust actor fields from a client's request.
type Identity struct {
	User, Agent, ServiceKey bool
	WorkspaceID             string
	EventSource             *EventSourceIdentity
	HasPermission           func(Permission) bool
	Allows                  func(workspaceID, projectID string, grant Grant) bool
	Authorize               func(context.Context, any, Grant, ...string) error
	WithScope               func(context.Context, string, bool) context.Context
	// FoundryGrants says source grants give it something, though it reads no database: it
	// discovers the read tools, and each call is decided as it is made.
	FoundryGrants bool
	// AsGrantee is ctx for a call made as a grantee of source grants, and IsGrantee whether Authorize
	// admitted ctx as one. Nil on a host without grants.
	AsGrantee func(context.Context) context.Context
	IsGrantee func(context.Context) bool
}

type EventSourceIdentity struct{ WorkspaceID, ConnectorID string }

type Directory interface {
	// AuthorizeCreate refuses a create_ontology on a new database, at prepare, unless the caller
	// ctx carries may make every call the website's wizard makes: CreateProject when projectID
	// is empty, CreateTenant and CreateFoundryOntology. ValidateFoundryOperation has already
	// refused every caller but a user. A host whose new databases need a subscription refuses
	// one that lapsed here too (FoundryNoHelixSubscription), before a confirmation is spent.
	AuthorizeCreate(ctx context.Context, workspaceID, projectID string) error
	// TenantWorkspace reports a database that doesn't exist with codes.NotFound; any other error
	// is a lookup that failed, which says nothing of the database.
	TenantWorkspace(ctx context.Context, tenantID string) (string, error)
	CreateProject(ctx context.Context, workspaceID, slug, name string) (string, error)
	CreateDatabase(ctx context.Context, projectID, name, slug string) (string, error)
}

type ConfirmationBinding struct {
	ID, Token, Operation, Target, Audience, PayloadHash string
}

type Confirmation struct {
	ID, Token string
	ExpiresAt time.Time
}

// Confirmations must bind the authenticated principal in addition to Binding.
// Consume must atomically spend the token before returning success.
type Confirmations interface {
	Prepare(context.Context, ConfirmationBinding) (Confirmation, error)
	Consume(context.Context, ConfirmationBinding) error
}

// Call is what a tool tells its host's audit of itself. QuestionID is set as soon as a question is
// known to exist, so it survives the call failing afterwards.
type Call struct{ ResourceKind, ResourceID, QuestionID string }
type Invocation func(context.Context, *Call) (any, int, error)
type Runner func(context.Context, string, Invocation) (*mcp.CallToolResult, any, error)

type Service struct {
	Foundry                 FoundryBackendAPI
	Actions                 FoundryActionAPI
	Directory               Directory
	Confirmations           Confirmations
	Documents               config.DocumentsConfig
	FoundryURL, ResourceURI string
	EventsURL               *url.URL
	Events                  bool
	Wait                    time.Duration
	Polls                   []time.Duration
	RunTool                 Runner
	Logger                  *slog.Logger
	// SignIn is whether the host offers connecting a connector by signing in at its provider
	// (hosted: pipes.enabled). Only then are the sign-in tools registered; a local install
	// has no provider accounts and leaves it false.
	SignIn bool
}

func (s *Service) AddTools(server *mcp.Server, identity *Identity) {
	s.AddFoundryTools(server, identity)
}
func (s *Service) AddSendTool(server *mcp.Server, identity *Identity) {
	s.AddFoundrySendTool(server, identity)
}

// AddReadTool registers a host's own read-only tool, run as Foundry's tools are: under Run, so it
// logs its call, and its content starts with the notice that what it returns is untrusted.
func AddReadTool[In, Out any](
	s *Service, server *mcp.Server, identity *Identity, name, description string,
	handle func(context.Context, *Identity, *Call, In) (Out, int, error),
) {
	addFoundryTool(s, server, identity, tool(name, description), handle)
}

func (s *Service) Run(ctx context.Context, name string, invoke Invocation) (*mcp.CallToolResult, any, error) {
	if s.RunTool != nil {
		return s.RunTool(ctx, name, invoke)
	}
	started := time.Now()
	call := &Call{}
	out, count, err := invoke(ctx, call)
	if s.Logger != nil {
		code := ""
		if err != nil {
			code = string(stableErrorCode(err))
		}
		s.Logger.Info("Foundry tool call", "tool", name, "resource_kind", call.ResourceKind, "resource_id", call.ResourceID,
			"duration_ms", time.Since(started).Milliseconds(), "result_count", count, "error_code", code)
	}
	if err != nil {
		return nil, nil, ToolError(err)
	}
	// The SDK sets structuredContent from out, but adds its JSON to content only when content is
	// empty. A client that reads only content needs it after the notice, as the spec suggests.
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, nil, ToolError(errCodeInternal)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: untrustedContentNotice},
		&mcp.TextContent{Text: string(encoded)},
	}}, out, nil
}

type ConfirmationOutput = confirmationOutput
