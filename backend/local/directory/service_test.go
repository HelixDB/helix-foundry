package directory_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/helixdb/helix-foundry/backend/local/directory"
	directoryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/directory/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
)

func wantCode(t *testing.T, err error, want codes.Code, contains string) {
	t.Helper()
	if status.Code(err) != want || !strings.Contains(status.Convert(err).Message(), contains) {
		t.Fatalf("err = %v, want %s containing %q", err, want, contains)
	}
}

func newService(st *directory.Store, helixURL string) *directory.Service {
	return directory.NewService(st, helixURL, http.DefaultClient)
}

func createWorkspace(t *testing.T, ctx context.Context, svc *directory.Service, slug string) *directoryv1.Workspace {
	t.Helper()
	w, err := svc.CreateWorkspace(ctx, &directoryv1.CreateWorkspaceRequest{Slug: slug, Region: directory.Region})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestSeedOwnerIsIdempotent(t *testing.T) {
	st := needDB(t)
	first, err := st.Owner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SeedOwner(context.Background()); err != nil {
		t.Fatal(err)
	}
	again, err := st.Owner(context.Background())
	if err != nil || again.ID != first.ID || again.Email != directory.OwnerEmail || again.DisplayName != "Owner" {
		t.Fatalf("owner = %+v, %v; first %+v", again, err, first)
	}
}

func TestWhoami(t *testing.T) {
	st := needDB(t)
	svc := newService(st, "")
	ctx := asOwner(t, st)

	got, err := svc.Whoami(ctx, &directoryv1.WhoamiRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetUser().GetEmail() != directory.OwnerEmail || got.GetUser().GetDisplayName() != "Owner" || len(got.GetWorkspaces()) != 0 {
		t.Fatalf("whoami = %v", got)
	}

	// Every live workspace is one of the owner's, as its owner.
	a, b := createWorkspace(t, ctx, svc, "acme"), createWorkspace(t, ctx, svc, "globex")
	if _, err := testDB.Exec(`UPDATE workspaces SET deleted_at = NOW(6) WHERE id = ?`, b.GetId()); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Whoami(ctx, &directoryv1.WhoamiRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetWorkspaces()) != 1 {
		t.Fatalf("memberships = %v", got.GetWorkspaces())
	}
	m := got.GetWorkspaces()[0]
	if m.GetWorkspaceId() != a.GetId() || m.GetRole() != directoryv1.WorkspaceRole_WORKSPACE_ROLE_OWNER || m.GetUser().GetId() != got.GetUser().GetId() {
		t.Fatalf("membership = %v", m)
	}

	// No caller, or one that is no user, is not signed in.
	_, err = svc.Whoami(context.Background(), &directoryv1.WhoamiRequest{})
	wantCode(t, err, codes.Unauthenticated, "")
	_, err = svc.Whoami(ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceUser, UserID: "nobody"}), &directoryv1.WhoamiRequest{})
	wantCode(t, err, codes.Unauthenticated, "identity no longer exists")
}

func TestCreateWorkspace(t *testing.T) {
	st := needDB(t)
	svc := newService(st, "")
	ctx := asOwner(t, st)

	w, err := svc.CreateWorkspace(ctx, &directoryv1.CreateWorkspaceRequest{Slug: "  Acme ", Region: "local", DisplayName: proto.String(" Acme Inc ")})
	if err != nil {
		t.Fatal(err)
	}
	if w.GetSlug() != "acme" || w.GetDisplayName() != "Acme Inc" || w.GetRegion() != "local" ||
		w.GetStatus() != directoryv1.ResourceStatus_RESOURCE_STATUS_ACTIVE || w.GetTimestamps().GetCreatedAt() == nil {
		t.Fatalf("workspace = %v", w)
	}
	// Without a display name it is named by its slug.
	if w := createWorkspace(t, ctx, svc, "initech"); w.GetDisplayName() != "initech" {
		t.Fatalf("display name = %q", w.GetDisplayName())
	}

	for name, tc := range map[string]struct {
		req  *directoryv1.CreateWorkspaceRequest
		code codes.Code
		msg  string
	}{
		"taken slug":     {&directoryv1.CreateWorkspaceRequest{Slug: "ACME", Region: "local"}, codes.AlreadyExists, `"acme" already exists`},
		"no slug":        {&directoryv1.CreateWorkspaceRequest{Region: "local"}, codes.InvalidArgument, "slug: is required"},
		"reserved slug":  {&directoryv1.CreateWorkspaceRequest{Slug: "login", Region: "local"}, codes.InvalidArgument, "is reserved"},
		"bad slug":       {&directoryv1.CreateWorkspaceRequest{Slug: "-acme", Region: "local"}, codes.InvalidArgument, "must start with"},
		"long slug":      {&directoryv1.CreateWorkspaceRequest{Slug: strings.Repeat("a", 65), Region: "local"}, codes.InvalidArgument, "at most 64"},
		"empty name":     {&directoryv1.CreateWorkspaceRequest{Slug: "x", Region: "local", DisplayName: proto.String(" ")}, codes.InvalidArgument, "display_name: is required"},
		"long name":      {&directoryv1.CreateWorkspaceRequest{Slug: "x", Region: "local", DisplayName: proto.String(strings.Repeat("é", 256))}, codes.InvalidArgument, "at most 255"},
		"no region":      {&directoryv1.CreateWorkspaceRequest{Slug: "x"}, codes.InvalidArgument, "region is required"},
		"another region": {&directoryv1.CreateWorkspaceRequest{Slug: "x", Region: "us-east-1"}, codes.InvalidArgument, `region "us-east-1" is not enabled`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateWorkspace(ctx, tc.req)
			wantCode(t, err, tc.code, tc.msg)
		})
	}
}

func TestGetWorkspace(t *testing.T) {
	st := needDB(t)
	svc := newService(st, "")
	ctx := asOwner(t, st)
	w := createWorkspace(t, ctx, svc, "acme")

	got, err := svc.GetWorkspace(ctx, &directoryv1.GetWorkspaceRequest{Id: w.GetId()})
	if err != nil || !proto.Equal(got, w) {
		t.Fatalf("get = %v, %v; want %v", got, err, w)
	}
	_, err = svc.GetWorkspace(ctx, &directoryv1.GetWorkspaceRequest{Id: "01J00000000000000000000000"})
	wantCode(t, err, codes.NotFound, "workspace not found")
	_, err = svc.GetWorkspace(ctx, &directoryv1.GetWorkspaceRequest{})
	wantCode(t, err, codes.InvalidArgument, "id is required")
}

func TestListWorkspacesPages(t *testing.T) {
	st := needDB(t)
	svc := newService(st, "")
	ctx := asOwner(t, st)
	var made []string
	for _, slug := range []string{"a", "b", "c"} {
		made = append(made, createWorkspace(t, ctx, svc, slug).GetId())
	}

	first, err := svc.ListWorkspaces(ctx, &directoryv1.ListWorkspacesRequest{Page: &directoryv1.Page{Size: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.GetWorkspaces()) != 2 || first.GetWorkspaces()[0].GetId() != made[2] || first.GetNextPageToken() == "" {
		t.Fatalf("first page = %v", first)
	}
	rest, err := svc.ListWorkspaces(ctx, &directoryv1.ListWorkspacesRequest{Page: &directoryv1.Page{Size: 2, Token: first.GetNextPageToken()}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.GetWorkspaces()) != 1 || rest.GetWorkspaces()[0].GetId() != made[0] || rest.GetNextPageToken() != "" {
		t.Fatalf("last page = %v", rest)
	}
	// A size past 200 is the default page, which holds them all.
	all, err := svc.ListWorkspaces(ctx, &directoryv1.ListWorkspacesRequest{Page: &directoryv1.Page{Size: 1000}})
	if err != nil || len(all.GetWorkspaces()) != 3 {
		t.Fatalf("all = %v, %v", all, err)
	}
	_, err = svc.ListWorkspaces(ctx, &directoryv1.ListWorkspacesRequest{Page: &directoryv1.Page{Token: "not-a-token"}})
	wantCode(t, err, codes.InvalidArgument, "invalid page token")
}

func TestProjects(t *testing.T) {
	st := needDB(t)
	svc := newService(st, "")
	ctx := asOwner(t, st)
	ws := createWorkspace(t, ctx, svc, "acme").GetId()
	other := createWorkspace(t, ctx, svc, "globex").GetId()

	create := func(slug, name string) *directoryv1.Project {
		t.Helper()
		p, err := svc.CreateProject(ctx, &directoryv1.CreateProjectRequest{WorkspaceId: ws, Slug: slug, DisplayName: name})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// A slug the workspace has gets the next free suffix, and its name too.
	crm := create("crm", "CRM")
	crm2 := create("crm", "CRM")
	crm3 := create("CRM", "CRM")
	for p, want := range map[*directoryv1.Project][2]string{crm: {"crm", "CRM"}, crm2: {"crm-2", "CRM 2"}, crm3: {"CRM-3", "CRM 3"}} {
		if p.GetSlug() != want[0] || p.GetDisplayName() != want[1] || p.GetWorkspaceId() != ws {
			t.Fatalf("project = %v, want %v", p, want)
		}
	}
	// Another workspace's slugs are its own; no name is the slug.
	if p, err := svc.CreateProject(ctx, &directoryv1.CreateProjectRequest{WorkspaceId: other, Slug: "crm"}); err != nil || p.GetSlug() != "crm" || p.GetDisplayName() != "crm" {
		t.Fatalf("other = %v, %v", p, err)
	}
	// A suffix keeps the slug within its length.
	long := create(strings.Repeat("s", 64), "Long")
	if again := create(strings.Repeat("s", 64), "Long"); len(again.GetSlug()) != 64 || again.GetSlug() == long.GetSlug() {
		t.Fatalf("suffixed slug = %q", again.GetSlug())
	}

	list, err := svc.ListProjects(ctx, &directoryv1.ListProjectsRequest{WorkspaceId: ws})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetProjects()) != 5 || list.GetProjects()[2].GetId() != crm3.GetId() {
		t.Fatalf("projects = %v", list)
	}
	// A deleted project is not listed.
	if _, err := testDB.Exec(`UPDATE projects SET deleted_at = NOW(6) WHERE id = ?`, crm.GetId()); err != nil {
		t.Fatal(err)
	}
	if list, err := svc.ListProjects(ctx, &directoryv1.ListProjectsRequest{WorkspaceId: ws}); err != nil || len(list.GetProjects()) != 4 {
		t.Fatalf("projects after delete = %v, %v", list, err)
	}

	_, err = svc.CreateProject(ctx, &directoryv1.CreateProjectRequest{WorkspaceId: "01J00000000000000000000000", Slug: "x"})
	wantCode(t, err, codes.NotFound, "workspace not found")
	_, err = svc.CreateProject(ctx, &directoryv1.CreateProjectRequest{WorkspaceId: ws})
	wantCode(t, err, codes.InvalidArgument, "slug is required")
	_, err = svc.CreateProject(ctx, &directoryv1.CreateProjectRequest{Slug: "x"})
	wantCode(t, err, codes.InvalidArgument, "workspace_id is required")
	_, err = svc.ListProjects(ctx, &directoryv1.ListProjectsRequest{})
	wantCode(t, err, codes.InvalidArgument, "workspace_id is required")
}

func TestTenants(t *testing.T) {
	st := needDB(t)
	svc := newService(st, "")
	ctx := asOwner(t, st)
	ws := createWorkspace(t, ctx, svc, "acme").GetId()
	project, err := svc.CreateProject(ctx, &directoryv1.CreateProjectRequest{WorkspaceId: ws, Slug: "crm", DisplayName: "CRM"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.CreateProject(ctx, &directoryv1.CreateProjectRequest{WorkspaceId: ws, Slug: "ops", DisplayName: "Ops"})
	if err != nil {
		t.Fatal(err)
	}

	made, err := svc.CreateTenant(ctx, &directoryv1.CreateTenantRequest{ProjectId: project.GetId(), Name: " Shop ", Slug: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	tn := made.GetTenant()
	if tn.GetName() != "Shop" || tn.GetSlug() != "shop" || tn.GetWorkspaceId() != ws || tn.GetProjectId() != project.GetId() ||
		tn.GetStatus() != directoryv1.ResourceStatus_RESOURCE_STATUS_ACTIVE || tn.GetQueryState() != directoryv1.TenantQueryState_TENANT_QUERY_STATE_ENABLED {
		t.Fatalf("tenant = %v", tn)
	}
	// A slug is the install's: another project cannot take it.
	_, err = svc.CreateTenant(ctx, &directoryv1.CreateTenantRequest{ProjectId: other.GetId(), Name: "Shop", Slug: "shop"})
	wantCode(t, err, codes.AlreadyExists, `"shop" already exists`)

	second, err := svc.CreateTenant(ctx, &directoryv1.CreateTenantRequest{ProjectId: project.GetId(), Name: "Billing", Slug: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	// Deleted databases are listed too, newest first.
	if _, err := testDB.Exec(`UPDATE tenants SET deleted_at = NOW(6), status = 'deleted' WHERE id = ?`, tn.GetId()); err != nil {
		t.Fatal(err)
	}
	list, err := svc.ListTenants(ctx, &directoryv1.ListTenantsRequest{ProjectId: project.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.GetTenants()) != 2 || list.GetTenants()[0].GetId() != second.GetTenant().GetId() ||
		list.GetTenants()[1].GetStatus() != directoryv1.ResourceStatus_RESOURCE_STATUS_DELETED || list.GetTenants()[1].GetTimestamps().GetDeletedAt() == nil {
		t.Fatalf("tenants = %v", list)
	}

	for name, tc := range map[string]struct {
		req  *directoryv1.CreateTenantRequest
		code codes.Code
		msg  string
	}{
		"no project":      {&directoryv1.CreateTenantRequest{Name: "n", Slug: "s"}, codes.InvalidArgument, "project_id is required"},
		"no name":         {&directoryv1.CreateTenantRequest{ProjectId: project.GetId(), Slug: "s"}, codes.InvalidArgument, "name is required"},
		"no slug":         {&directoryv1.CreateTenantRequest{ProjectId: project.GetId(), Name: "n"}, codes.InvalidArgument, "slug is required"},
		"long name":       {&directoryv1.CreateTenantRequest{ProjectId: project.GetId(), Name: strings.Repeat("n", 129), Slug: "s"}, codes.InvalidArgument, "at most 128"},
		"long slug":       {&directoryv1.CreateTenantRequest{ProjectId: project.GetId(), Name: "n", Slug: strings.Repeat("s", 65)}, codes.InvalidArgument, "at most 64"},
		"unknown project": {&directoryv1.CreateTenantRequest{ProjectId: "01J00000000000000000000000", Name: "n", Slug: "s"}, codes.NotFound, "project not found"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateTenant(ctx, tc.req)
			wantCode(t, err, tc.code, tc.msg)
		})
	}

	// Neither a deleted project nor a deleted workspace takes a database.
	if _, err := testDB.Exec(`UPDATE projects SET deleted_at = NOW(6) WHERE id = ?`, other.GetId()); err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateTenant(ctx, &directoryv1.CreateTenantRequest{ProjectId: other.GetId(), Name: "n", Slug: "s"})
	wantCode(t, err, codes.FailedPrecondition, "project is deleted")
	if _, err := testDB.Exec(`UPDATE workspaces SET status = 'deleting' WHERE id = ?`, ws); err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateTenant(ctx, &directoryv1.CreateTenantRequest{ProjectId: project.GetId(), Name: "n", Slug: "s"})
	wantCode(t, err, codes.FailedPrecondition, "workspace is deleted")

	_, err = svc.ListTenants(ctx, &directoryv1.ListTenantsRequest{})
	wantCode(t, err, codes.InvalidArgument, "project_id is required")
}

func TestExecuteDatabaseReadQuery(t *testing.T) {
	st := needDB(t)
	var got *http.Request
	var gotBody string
	helix := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if strings.Contains(gotBody, "huge") {
			_, _ = w.Write([]byte(strings.Repeat("x", directory.MaxQueryResponseBytes+1)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"rows":[]}`))
	}))
	defer helix.Close()
	svc := newService(st, helix.URL+"/")
	ctx := asOwner(t, st)
	ws := createWorkspace(t, ctx, svc, "acme").GetId()
	project, err := svc.CreateProject(ctx, &directoryv1.CreateProjectRequest{WorkspaceId: ws, Slug: "crm"})
	if err != nil {
		t.Fatal(err)
	}
	made, err := svc.CreateTenant(ctx, &directoryv1.CreateTenantRequest{ProjectId: project.GetId(), Name: "Shop", Slug: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	tenant := made.GetTenant().GetId()
	query := func(tenantID, body string) (*directoryv1.ExecuteDatabaseReadQueryResponse, error) {
		return svc.ExecuteDatabaseReadQuery(ctx, &directoryv1.ExecuteDatabaseReadQueryRequest{
			Database: &directoryv1.DatabaseReference{TenantId: tenantID}, QueryJson: []byte(body),
		})
	}

	// The database's answer comes back as it gave it; the request goes to
	// /v2/query for the tenant, with no key.
	read := `{"request_type":"read","query":{"q":1}}`
	resp, err := query(tenant, read)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatusCode() != http.StatusTeapot || string(resp.GetResponseJson()) != `{"rows":[]}` || resp.GetContentType() != "application/json" {
		t.Fatalf("response = %v", resp)
	}
	if got.URL.Path != "/v2/query" || got.Header.Get("X-Helix-Tenant-ID") != tenant || got.Header.Get("Authorization") != "" || gotBody != read {
		t.Fatalf("request = %s %v %q", got.URL, got.Header, gotBody)
	}

	_, err = svc.ExecuteDatabaseReadQuery(ctx, &directoryv1.ExecuteDatabaseReadQueryRequest{})
	wantCode(t, err, codes.InvalidArgument, "database is required")
	_, err = query("", read)
	wantCode(t, err, codes.InvalidArgument, "variant is required")
	_, err = query(tenant, `{"request_type":"read"}`)
	wantCode(t, err, codes.InvalidArgument, "valid v3 query request")
	_, err = query(tenant, `{"request_type":"write","query":{}}`)
	wantCode(t, err, codes.InvalidArgument, `must be "read"`)
	_, err = query(tenant, `{"request_type":"read","query":"`+strings.Repeat("x", directory.MaxQueryRequestBytes)+`"}`)
	wantCode(t, err, codes.ResourceExhausted, "request exceeds")
	_, err = query(tenant, `{"request_type":"read","query":"huge"}`)
	wantCode(t, err, codes.ResourceExhausted, "response exceeds")
	_, err = query("01J00000000000000000000000", read)
	wantCode(t, err, codes.NotFound, "tenant not found")

	if _, err := testDB.Exec(`UPDATE tenants SET deleted_at = NOW(6) WHERE id = ?`, tenant); err != nil {
		t.Fatal(err)
	}
	_, err = query(tenant, read)
	wantCode(t, err, codes.FailedPrecondition, "is deleted")

	// A database that does not answer is Unavailable, and says no more.
	if _, err := testDB.Exec(`UPDATE tenants SET deleted_at = NULL WHERE id = ?`, tenant); err != nil {
		t.Fatal(err)
	}
	helix.Close()
	_, err = query(tenant, read)
	wantCode(t, err, codes.Unavailable, "database query broker is unavailable")
}
