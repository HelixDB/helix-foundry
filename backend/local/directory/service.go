package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	directoryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/directory/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
)

// Region is a standalone install's one workspace region.
const Region = "local"

// Query limits, as a hosted control plane's query broker sets them.
const (
	MaxQueryRequestBytes  = 8 << 20
	MaxQueryResponseBytes = 32 << 20
	QueryTimeout          = 3 * time.Minute
)

// queryUnavailable is all a caller is told of a query the database did not
// answer; the log keeps the cause.
const queryUnavailable = "database query broker is unavailable"

// Pages are 50 rows unless the request asks for 1 to 200.
const (
	defaultPage = 50
	maxPage     = 200
)

const (
	maxWorkspaceName = 255
	maxProjectName   = 512
	maxTenantName    = 128
)

// reservedSlugs are workspace slugs the site's own paths use.
var reservedSlugs = []string{
	"access", "account", "api", "auth", "billing-success", "blog", "content", "create-workspace", "explorer",
	"invitations", "invited", "login", "papi", "personal", "privacy", "terms", "visualizer",
}

// Service serves helix.foundry.directory.v1.DirectoryService for the owner,
// the ports.Actor on each call's context (the install's front end
// authenticates every call and checks the workspace of those that name one).
type Service struct {
	store *Store
	// helixURL is the install's HelixDB, whose /v2/query every read goes to.
	helixURL string
	helix    helixgateway.Client
}

var _ directoryv1.DirectoryServiceServer = (*Service)(nil)

// NewService serves the directory in st, and reads databases at helixURL
// (an origin, such as http://helix:8080) through client.
func NewService(st *Store, helixURL string, client helixgateway.Doer) *Service {
	return &Service{
		store:    st,
		helixURL: strings.TrimSuffix(helixURL, "/"),
		helix:    helixgateway.Client{HTTP: client, Keyless: true, MaxResponseBytes: MaxQueryResponseBytes},
	}
}

// caller is the user the call is made as.
func caller(ctx context.Context) (string, error) {
	a, ok := ports.ActorFrom(ctx)
	if !ok || a.UserID == "" {
		return "", status.Error(codes.Unauthenticated, "authenticated user session is required")
	}
	return a.UserID, nil
}

func (s *Service) Whoami(ctx context.Context, _ *directoryv1.WhoamiRequest) (*directoryv1.WhoamiResponse, error) {
	userID, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	u, err := s.store.User(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Error(codes.Unauthenticated, "identity no longer exists")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get user: %v", err)
	}
	user := userProto(u)
	out := &directoryv1.WhoamiResponse{User: user}
	for after := ""; ; {
		page, err := s.store.ListWorkspaces(ctx, after, maxPage)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list workspaces: %v", err)
		}
		for _, w := range page {
			out.Workspaces = append(out.Workspaces, &directoryv1.WorkspaceMember{
				WorkspaceId: w.ID, User: user, Role: directoryv1.WorkspaceRole_WORKSPACE_ROLE_OWNER,
				Timestamps: timestamps(w.CreatedAt, w.UpdatedAt, nil),
			})
		}
		if len(page) < maxPage {
			return out, nil
		}
		after = page[len(page)-1].ID
	}
}

func (s *Service) ListWorkspaces(ctx context.Context, req *directoryv1.ListWorkspacesRequest) (*directoryv1.ListWorkspacesResponse, error) {
	after, size, err := pageOf(req.GetPage())
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListWorkspaces(ctx, after, size+1)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list workspaces: %v", err)
	}
	rows, next := cut(rows, size, func(w Workspace) string { return w.ID })
	out := &directoryv1.ListWorkspacesResponse{NextPageToken: next}
	for _, w := range rows {
		out.Workspaces = append(out.Workspaces, workspaceProto(w))
	}
	return out, nil
}

// CreateWorkspace makes a workspace in the install's one region. Its slug
// is lowercase letters, numbers, "-" and "_", starting with a letter or
// number, and unique: a taken one is AlreadyExists.
func (s *Service) CreateWorkspace(ctx context.Context, req *directoryv1.CreateWorkspaceRequest) (*directoryv1.Workspace, error) {
	slug := strings.ToLower(strings.TrimSpace(req.GetSlug()))
	if err := validSlug(slug); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "slug: %v", err)
	}
	if slices.Contains(reservedSlugs, slug) {
		return nil, status.Errorf(codes.InvalidArgument, "slug: %q is reserved", slug)
	}
	name := slug
	if req.DisplayName != nil {
		name = strings.TrimSpace(req.GetDisplayName())
		if err := validName(name, maxWorkspaceName); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "display_name: %v", err)
		}
	}
	if req.GetRegion() == "" {
		return nil, status.Error(codes.InvalidArgument, "region is required")
	}
	if req.GetRegion() != Region {
		return nil, status.Errorf(codes.InvalidArgument, "region %q is not enabled", req.GetRegion())
	}
	w := Workspace{ID: id.New(), Slug: slug, DisplayName: name, Region: Region}
	err := s.store.CreateWorkspace(ctx, w)
	if errors.Is(err, store.ErrConflict) {
		return nil, status.Errorf(codes.AlreadyExists, "workspace %q already exists", slug)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create workspace: %v", err)
	}
	created, err := s.store.Workspace(ctx, w.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reload workspace: %v", err)
	}
	return workspaceProto(created), nil
}

// The workspace an install starts with.
const (
	FirstWorkspaceSlug = "my-workspace"
	FirstWorkspaceName = "My workspace"
)

// SeedWorkspace makes the owner's first workspace, "My workspace", as
// CreateWorkspace makes any, unless the install has ever had one (a deleted
// one counts: it is never made again). Every start runs it, so a new
// install opens in a workspace rather than on the page that creates one. It
// returns the workspace it made, or nil.
func (s *Service) SeedWorkspace(ctx context.Context) (*directoryv1.Workspace, error) {
	switch had, err := s.store.AnyWorkspace(ctx); {
	case err != nil:
		return nil, fmt.Errorf("seed workspace: %w", err)
	case had:
		return nil, nil
	}
	name := FirstWorkspaceName
	w, err := s.CreateWorkspace(ctx, &directoryv1.CreateWorkspaceRequest{Slug: FirstWorkspaceSlug, DisplayName: &name, Region: Region})
	if status.Code(err) == codes.AlreadyExists {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("seed workspace: %w", err)
	}
	return w, nil
}

func (s *Service) GetWorkspace(ctx context.Context, req *directoryv1.GetWorkspaceRequest) (*directoryv1.Workspace, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	w, err := s.store.Workspace(ctx, req.GetId())
	if errors.Is(err, store.ErrNotFound) || err == nil && w.DeletedAt != nil {
		return nil, status.Error(codes.NotFound, "workspace not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get workspace: %v", err)
	}
	return workspaceProto(w), nil
}

func (s *Service) ListProjects(ctx context.Context, req *directoryv1.ListProjectsRequest) (*directoryv1.ListProjectsResponse, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	after, size, err := pageOf(req.GetPage())
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListProjects(ctx, req.GetWorkspaceId(), after, size+1)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list projects: %v", err)
	}
	rows, next := cut(rows, size, func(p Project) string { return p.ID })
	out := &directoryv1.ListProjectsResponse{NextPageToken: next}
	for _, p := range rows {
		out.Projects = append(out.Projects, projectProto(p))
	}
	return out, nil
}

// CreateProject makes a project in a workspace; a slug the workspace has
// already gets the next free numeric suffix (Store.CreateProject). Without
// a display name it is named by its slug.
func (s *Service) CreateProject(ctx context.Context, req *directoryv1.CreateProjectRequest) (*directoryv1.Project, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	slug := strings.TrimSpace(req.GetSlug())
	if slug == "" {
		return nil, status.Error(codes.InvalidArgument, "slug is required")
	}
	if len(slug) > MaxSlug {
		return nil, status.Errorf(codes.InvalidArgument, "slug must be at most %d characters", MaxSlug)
	}
	name := strings.TrimSpace(req.GetDisplayName())
	if name == "" {
		name = slug
	}
	if err := validName(name, maxProjectName); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "display_name: %v", err)
	}
	p, err := s.store.CreateProject(ctx, req.GetWorkspaceId(), slug, name)
	if err != nil {
		if status.Code(err) != codes.Unknown {
			return nil, err
		}
		return nil, status.Errorf(codes.Internal, "create project: %v", err)
	}
	return projectProto(p), nil
}

func (s *Service) ListTenants(ctx context.Context, req *directoryv1.ListTenantsRequest) (*directoryv1.ListTenantsResponse, error) {
	if req.GetProjectId() == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	after, size, err := pageOf(req.GetPage())
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListTenants(ctx, req.GetProjectId(), after, size+1)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list tenants: %v", err)
	}
	rows, next := cut(rows, size, func(t Tenant) string { return t.ID })
	out := &directoryv1.ListTenantsResponse{NextPageToken: next}
	for _, t := range rows {
		out.Tenants = append(out.Tenants, tenantProto(t))
	}
	return out, nil
}

// CreateTenant makes a database in a project. Every database is queried at
// the install's one HelixDB, so its slug is unique across the install: a
// taken one is AlreadyExists.
func (s *Service) CreateTenant(ctx context.Context, req *directoryv1.CreateTenantRequest) (*directoryv1.CreateTenantResponse, error) {
	name, slug := strings.TrimSpace(req.GetName()), strings.TrimSpace(req.GetSlug())
	switch {
	case req.GetProjectId() == "":
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	case name == "":
		return nil, status.Error(codes.InvalidArgument, "name is required")
	case slug == "":
		return nil, status.Error(codes.InvalidArgument, "slug is required")
	case utf8.RuneCountInString(name) > maxTenantName:
		return nil, status.Errorf(codes.InvalidArgument, "name must be at most %d characters", maxTenantName)
	case len(slug) > MaxSlug:
		return nil, status.Errorf(codes.InvalidArgument, "slug must be at most %d characters", MaxSlug)
	}
	project, err := s.store.Project(ctx, req.GetProjectId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "project not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "lookup project: %v", err)
	}
	if project.DeletedAt != nil {
		return nil, status.Error(codes.FailedPrecondition, "project is deleted")
	}
	t, err := s.store.CreateTenant(ctx, project, name, slug)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrConflict):
		return nil, status.Errorf(codes.AlreadyExists, "tenant %q already exists in cluster", slug)
	case status.Code(err) != codes.Unknown:
		return nil, err
	default:
		return nil, status.Errorf(codes.Internal, "create tenant: %v", err)
	}
	return &directoryv1.CreateTenantResponse{Tenant: tenantProto(t)}, nil
}

func (s *Service) ExecuteDatabaseReadQuery(ctx context.Context, req *directoryv1.ExecuteDatabaseReadQueryRequest) (*directoryv1.ExecuteDatabaseReadQueryResponse, error) {
	if req.GetDatabase() == nil {
		return nil, status.Error(codes.InvalidArgument, "database is required")
	}
	if req.GetDatabase().GetTenantId() == "" {
		return nil, status.Error(codes.InvalidArgument, "database reference variant is required")
	}
	resp, err := s.ReadQuery(ctx, req.GetDatabase().GetTenantId(), req.GetQueryJson())
	if err != nil {
		return nil, err
	}
	return &directoryv1.ExecuteDatabaseReadQueryResponse{
		ResponseJson: resp.Body, StatusCode: int32(resp.StatusCode), ContentType: resp.ContentType,
	}, nil
}

// ReadQuery runs query, a HelixDB read request ({"request_type": "read",
// "query": ...}), on a live database and returns its answer, with a hosted
// query broker's limits. Its errors are gRPC statuses: InvalidArgument for a
// request that is not a read, ResourceExhausted past a limit, NotFound and
// FailedPrecondition for a database that is absent or deleted, Unavailable
// when the database did not answer.
func (s *Service) ReadQuery(ctx context.Context, tenantID string, query []byte) (helixgateway.Response, error) {
	if len(query) > MaxQueryRequestBytes {
		return helixgateway.Response{}, status.Error(codes.ResourceExhausted, "query request exceeds broker limit")
	}
	// A map, not a struct, which would match keys in any case and take the
	// last of duplicates, so the request_type checked could differ from the
	// one HelixDB reads.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(query, &envelope); err != nil || len(envelope["query"]) == 0 {
		return helixgateway.Response{}, status.Error(codes.InvalidArgument, "query_json must be a valid v3 query request")
	}
	var requestType string
	if _, alias := envelope["requestType"]; alias || json.Unmarshal(envelope["request_type"], &requestType) != nil || requestType != "read" {
		return helixgateway.Response{}, status.Error(codes.InvalidArgument, `query request_type must be "read"`)
	}
	// Written again from the object checked, as graphproxy does: a duplicate
	// request_type HelixDB might read in place of this one is gone.
	query, err := json.Marshal(envelope)
	if err != nil {
		return helixgateway.Response{}, status.Error(codes.InvalidArgument, "query_json must be a valid v3 query request")
	}
	t, err := s.store.Tenant(ctx, tenantID)
	if errors.Is(err, store.ErrNotFound) {
		return helixgateway.Response{}, status.Error(codes.NotFound, "tenant not found")
	}
	if err != nil {
		return helixgateway.Response{}, status.Errorf(codes.Internal, "get tenant: %v", err)
	}
	if t.Deleted() {
		return helixgateway.Response{}, status.Errorf(codes.FailedPrecondition, "tenant %s is deleted", t.ID)
	}
	resp, err := s.helix.Query(ctx, s.helixURL+"/v2/query", t.ID, query, nil)
	if errors.Is(err, helixgateway.ErrResponseTooLarge) {
		return helixgateway.Response{}, status.Error(codes.ResourceExhausted, "query response exceeds broker limit")
	}
	if err != nil {
		log.From(ctx).Warn("read query: the database did not answer", "tenant", t.ID, "err", err)
		return helixgateway.Response{}, status.Error(codes.Unavailable, queryUnavailable)
	}
	return resp, nil
}

// pageOf is a list request's cursor and page size: InvalidArgument for a
// token no list handed out.
func pageOf(p interface {
	GetSize() int32
	GetToken() string
}) (string, int, error) {
	size := int(p.GetSize())
	if size <= 0 || size > maxPage {
		size = defaultPage
	}
	token := p.GetToken()
	if token != "" && (len(token) != 26 || strings.ToUpper(token) != token) {
		return "", 0, status.Error(codes.InvalidArgument, "invalid page token")
	}
	return token, size, nil
}

// cut keeps the first size rows of a page read one past it, and returns the
// token of the next page: none when there is no row past it.
func cut[T any](rows []T, size int, idOf func(T) string) ([]T, string) {
	if len(rows) <= size {
		return rows, ""
	}
	return rows[:size], idOf(rows[size-1])
}

func validSlug(slug string) error {
	if slug == "" {
		return errors.New("is required")
	}
	if len(slug) > MaxSlug {
		return fmt.Errorf("must be at most %d characters", MaxSlug)
	}
	for i := range len(slug) {
		c := slug[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0:
		default:
			return errors.New("must start with a lowercase letter or number and contain only lowercase letters, numbers, hyphens, or underscores")
		}
	}
	return nil
}

func validName(name string, max int) error {
	switch {
	case name == "":
		return errors.New("is required")
	case !utf8.ValidString(name):
		return errors.New("must be valid UTF-8")
	case utf8.RuneCountInString(name) > max:
		return fmt.Errorf("must be at most %d characters", max)
	}
	return nil
}

func timestamps(created, updated time.Time, deleted *time.Time) *directoryv1.Timestamps {
	t := &directoryv1.Timestamps{CreatedAt: timestamppb.New(created), UpdatedAt: timestamppb.New(updated)}
	if deleted != nil {
		t.DeletedAt = timestamppb.New(*deleted)
	}
	return t
}

func resourceStatus(s string, deleted *time.Time) directoryv1.ResourceStatus {
	switch {
	case deleted != nil || s == StatusDeleted:
		return directoryv1.ResourceStatus_RESOURCE_STATUS_DELETED
	case s == StatusDeleting:
		return directoryv1.ResourceStatus_RESOURCE_STATUS_DELETING
	case s == StatusActive:
		return directoryv1.ResourceStatus_RESOURCE_STATUS_ACTIVE
	}
	return directoryv1.ResourceStatus_RESOURCE_STATUS_UNSPECIFIED
}

func userProto(u User) *directoryv1.User {
	return &directoryv1.User{Id: u.ID, Email: u.Email, DisplayName: u.DisplayName, Timestamps: timestamps(u.CreatedAt, u.UpdatedAt, nil)}
}

func workspaceProto(w Workspace) *directoryv1.Workspace {
	return &directoryv1.Workspace{
		Id: w.ID, Slug: w.Slug, Region: w.Region, Status: resourceStatus(w.Status, w.DeletedAt),
		Timestamps: timestamps(w.CreatedAt, w.UpdatedAt, w.DeletedAt), DisplayName: w.DisplayName,
	}
}

func projectProto(p Project) *directoryv1.Project {
	return &directoryv1.Project{
		Id: p.ID, WorkspaceId: p.WorkspaceID, Slug: p.Slug, DisplayName: p.DisplayName,
		Timestamps: timestamps(p.CreatedAt, p.UpdatedAt, p.DeletedAt),
	}
}

func tenantProto(t Tenant) *directoryv1.Tenant {
	return &directoryv1.Tenant{
		Id: t.ID, WorkspaceId: t.WorkspaceID, ProjectId: t.ProjectID, Name: t.Name, Slug: t.Slug,
		Status: resourceStatus(t.Status, t.DeletedAt), Timestamps: timestamps(t.CreatedAt, t.UpdatedAt, t.DeletedAt),
		QueryState: directoryv1.TenantQueryState_TENANT_QUERY_STATE_ENABLED,
	}
}
