package local

type pageInput struct {
	PageSize  int    `json:"page_size,omitempty" jsonschema:"Maximum results to return, from 1 through 100."`
	PageToken string `json:"page_token,omitempty" jsonschema:"Opaque continuation token from the previous call."`
}

type listWorkspacesInput struct{ pageInput }

type listProjectsInput struct {
	WorkspaceID string `json:"workspace_id" jsonschema:"Helix workspace identifier."`
	pageInput
}

type listDatabasesInput struct {
	ProjectID string `json:"project_id" jsonschema:"Helix project identifier."`
	pageInput
}

type workspaceOutput struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Region      string `json:"region"`
	Status      string `json:"status"`
}

type listWorkspacesOutput struct {
	ContentTrust string            `json:"content_trust"`
	Workspaces   []workspaceOutput `json:"workspaces"`
	NextPage     string            `json:"next_page_token,omitempty"`
}

type projectOutput struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

type listProjectsOutput struct {
	ContentTrust string          `json:"content_trust"`
	Projects     []projectOutput `json:"projects"`
	NextPage     string          `json:"next_page_token,omitempty"`
}

type databaseOutput struct {
	Reference   string `json:"reference"`
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ProjectID   string `json:"project_id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Status      string `json:"status"`
}

type listDatabasesOutput struct {
	ContentTrust string           `json:"content_trust"`
	Databases    []databaseOutput `json:"databases"`
	NextPage     string           `json:"next_page_token,omitempty"`
}
