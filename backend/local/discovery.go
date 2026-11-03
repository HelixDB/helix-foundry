package local

import (
	"context"

	"github.com/helixdb/helix-foundry/backend/integration"
	directoryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/directory/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const resourceStatusPrefix = "RESOURCE_STATUS_"

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

func addLocalDiscovery(service *integration.Service, server *mcp.Server, identity *integration.Identity, client directoryv1.DirectoryServiceClient) {
	integration.AddReadTool(service, server, identity, "helix_list_workspaces", "List the local owner's active Foundry workspaces.",
		func(ctx context.Context, _ *integration.Identity, _ *integration.Call, input listWorkspacesInput) (listWorkspacesOutput, int, error) {
			page, err := discoveryPage(input.pageInput)
			if err != nil {
				return listWorkspacesOutput{}, 0, err
			}
			r, err := client.ListWorkspaces(ctx, &directoryv1.ListWorkspacesRequest{Page: page})
			if err != nil {
				return listWorkspacesOutput{}, 0, err
			}
			out := listWorkspacesOutput{ContentTrust: integration.ContentTrustUntrusted, NextPage: r.GetNextPageToken(), Workspaces: []workspaceOutput{}}
			for _, w := range r.GetWorkspaces() {
				out.Workspaces = append(out.Workspaces, workspaceOutput{ID: w.GetId(), Slug: w.GetSlug(), DisplayName: w.GetDisplayName(), Region: w.GetRegion(), Status: integration.EnumName(w.GetStatus().String(), resourceStatusPrefix)})
			}
			return out, len(out.Workspaces), nil
		})
	integration.AddReadTool(service, server, identity, "helix_list_projects", "List projects in a local Foundry workspace.",
		func(ctx context.Context, _ *integration.Identity, call *integration.Call, input listProjectsInput) (listProjectsOutput, int, error) {
			call.ResourceKind, call.ResourceID = "workspace", input.WorkspaceID
			page, err := discoveryPage(input.pageInput)
			if err != nil {
				return listProjectsOutput{}, 0, err
			}
			r, err := client.ListProjects(ctx, &directoryv1.ListProjectsRequest{WorkspaceId: input.WorkspaceID, Page: page})
			if err != nil {
				return listProjectsOutput{}, 0, err
			}
			out := listProjectsOutput{ContentTrust: integration.ContentTrustUntrusted, NextPage: r.GetNextPageToken(), Projects: []projectOutput{}}
			for _, p := range r.GetProjects() {
				out.Projects = append(out.Projects, projectOutput{ID: p.GetId(), WorkspaceID: p.GetWorkspaceId(), Slug: p.GetSlug(), DisplayName: p.GetDisplayName()})
			}
			return out, len(out.Projects), nil
		})
	integration.AddReadTool(service, server, identity, "helix_list_databases", "List local databases in a Foundry project.",
		func(ctx context.Context, _ *integration.Identity, call *integration.Call, input listDatabasesInput) (listDatabasesOutput, int, error) {
			call.ResourceKind, call.ResourceID = "project", input.ProjectID
			page, err := discoveryPage(input.pageInput)
			if err != nil {
				return listDatabasesOutput{}, 0, err
			}
			r, err := client.ListTenants(ctx, &directoryv1.ListTenantsRequest{ProjectId: input.ProjectID, Page: page})
			if err != nil {
				return listDatabasesOutput{}, 0, err
			}
			out := listDatabasesOutput{ContentTrust: integration.ContentTrustUntrusted, NextPage: r.GetNextPageToken(), Databases: []databaseOutput{}}
			for _, d := range r.GetTenants() {
				out.Databases = append(out.Databases, databaseOutput{Reference: "tenant:" + d.GetId(), Kind: "tenant", ID: d.GetId(), WorkspaceID: d.GetWorkspaceId(), ProjectID: d.GetProjectId(), Name: d.GetName(), Slug: d.GetSlug(), Status: integration.EnumName(d.GetStatus().String(), resourceStatusPrefix)})
			}
			return out, len(out.Databases), nil
		})
}
