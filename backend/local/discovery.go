package local

import (
	"context"

	"github.com/helixdb/helix-foundry/backend/integration"
	directoryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/directory/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func discoveryPage(input pageInput) (*directoryv1.Page, error) {
	size := input.PageSize
	if size == 0 {
		size = 50
	}
	if size < 1 || size > 100 || len(input.PageToken) > 1024 {
		return nil, status.Error(codes.InvalidArgument, "invalid page size or token")
	}
	return &directoryv1.Page{Size: int32(size), Token: input.PageToken}, nil
}

func discoveryTool(name, description string) *mcp.Tool {
	no := false
	return &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: &no, OpenWorldHint: &no}}
}

func addLocalDiscovery(server *mcp.Server, client directoryv1.DirectoryServiceClient) {
	mcp.AddTool(server, discoveryTool("helix_list_workspaces", "List the local owner's active Foundry workspaces."),
		func(ctx context.Context, _ *mcp.CallToolRequest, input listWorkspacesInput) (*mcp.CallToolResult, listWorkspacesOutput, error) {
			page, err := discoveryPage(input.pageInput)
			if err != nil {
				return nil, listWorkspacesOutput{}, integration.ToolError(err)
			}
			r, err := client.ListWorkspaces(ctx, &directoryv1.ListWorkspacesRequest{Page: page})
			if err != nil {
				return nil, listWorkspacesOutput{}, integration.ToolError(err)
			}
			out := listWorkspacesOutput{ContentTrust: "untrusted_data", NextPage: r.GetNextPageToken(), Workspaces: []workspaceOutput{}}
			for _, w := range r.GetWorkspaces() {
				out.Workspaces = append(out.Workspaces, workspaceOutput{ID: w.GetId(), Slug: w.GetSlug(), DisplayName: w.GetDisplayName(), Region: w.GetRegion(), Status: localStatus(w.GetStatus())})
			}
			return nil, out, nil
		})
	mcp.AddTool(server, discoveryTool("helix_list_projects", "List projects in a local Foundry workspace."),
		func(ctx context.Context, _ *mcp.CallToolRequest, input listProjectsInput) (*mcp.CallToolResult, listProjectsOutput, error) {
			page, err := discoveryPage(input.pageInput)
			if err != nil {
				return nil, listProjectsOutput{}, integration.ToolError(err)
			}
			r, err := client.ListProjects(ctx, &directoryv1.ListProjectsRequest{WorkspaceId: input.WorkspaceID, Page: page})
			if err != nil {
				return nil, listProjectsOutput{}, integration.ToolError(err)
			}
			out := listProjectsOutput{ContentTrust: "untrusted_data", NextPage: r.GetNextPageToken(), Projects: []projectOutput{}}
			for _, p := range r.GetProjects() {
				out.Projects = append(out.Projects, projectOutput{ID: p.GetId(), WorkspaceID: p.GetWorkspaceId(), Slug: p.GetSlug(), DisplayName: p.GetDisplayName()})
			}
			return nil, out, nil
		})
	mcp.AddTool(server, discoveryTool("helix_list_databases", "List local databases in a Foundry project."),
		func(ctx context.Context, _ *mcp.CallToolRequest, input listDatabasesInput) (*mcp.CallToolResult, listDatabasesOutput, error) {
			page, err := discoveryPage(input.pageInput)
			if err != nil {
				return nil, listDatabasesOutput{}, integration.ToolError(err)
			}
			r, err := client.ListTenants(ctx, &directoryv1.ListTenantsRequest{ProjectId: input.ProjectID, Page: page})
			if err != nil {
				return nil, listDatabasesOutput{}, integration.ToolError(err)
			}
			out := listDatabasesOutput{ContentTrust: "untrusted_data", NextPage: r.GetNextPageToken(), Databases: []databaseOutput{}}
			for _, d := range r.GetTenants() {
				out.Databases = append(out.Databases, databaseOutput{Reference: "tenant:" + d.GetId(), Kind: "tenant", ID: d.GetId(), WorkspaceID: d.GetWorkspaceId(), ProjectID: d.GetProjectId(), Name: d.GetName(), Slug: d.GetSlug(), Status: localStatus(d.GetStatus())})
			}
			return nil, out, nil
		})
}
