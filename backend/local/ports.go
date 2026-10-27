package local

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/local/directory"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
)

// Directory is what the ports read of the install's directory;
// *directory.Store is it.
type Directory interface {
	Owner(ctx context.Context) (directory.User, error)
	User(ctx context.Context, id string) (directory.User, error)
	Workspace(ctx context.Context, id string) (directory.Workspace, error)
	Project(ctx context.Context, id string) (directory.Project, error)
	Tenant(ctx context.Context, id string) (directory.Tenant, error)
}

var _ Directory = (*directory.Store)(nil)

// Databases are the install's databases (ports.Databases), every one on
// the HelixDB at HelixURL.
type Databases struct {
	Directory Directory
	// HelixURL is the HelixDB's origin, such as http://helix:8080.
	HelixURL string
}

var _ ports.Databases = Databases{}

func (d Databases) Get(ctx context.Context, tenantID string) (ports.Database, error) {
	t, err := d.Directory.Tenant(ctx, tenantID)
	if err != nil {
		return ports.Database{}, err
	}
	return ports.Database{
		TenantID: t.ID, WorkspaceID: t.WorkspaceID, ProjectID: t.ProjectID, Name: t.Name,
		Deleted: t.DeletedAt != nil, Retired: t.Status == directory.StatusDeleted,
	}, nil
}

// Live is the database unless it is absent or deleted (NotFound) or its
// project is deleted (FailedPrecondition). A standalone install never
// pauses a database's queries.
func (d Databases) Live(ctx context.Context, tenantID string) (ports.Database, error) {
	db, err := d.Get(ctx, tenantID)
	if errors.Is(err, store.ErrNotFound) || err == nil && (db.Deleted || db.Retired) {
		return ports.Database{}, status.Error(codes.NotFound, "tenant not found")
	}
	if err != nil {
		return ports.Database{}, status.Errorf(codes.Internal, "get tenant: %v", err)
	}
	p, err := d.Directory.Project(ctx, db.ProjectID)
	if errors.Is(err, store.ErrNotFound) || err == nil && p.DeletedAt != nil {
		return ports.Database{}, status.Error(codes.FailedPrecondition, "the database's project is deleted")
	}
	if err != nil {
		return ports.Database{}, status.Errorf(codes.Internal, "get project: %v", err)
	}
	return db, nil
}

// Endpoint is path on the install's HelixDB, for a database that is not
// deleted.
func (d Databases) Endpoint(ctx context.Context, tenantID, path string) (string, error) {
	if path == "" || path[0] != '/' {
		return "", status.Error(codes.Internal, "invalid database route")
	}
	db, err := d.Get(ctx, tenantID)
	if errors.Is(err, store.ErrNotFound) || err == nil && (db.Deleted || db.Retired) {
		return "", status.Error(codes.NotFound, "tenant not found")
	}
	if err != nil {
		return "", status.Errorf(codes.Unavailable, "resolve database: %v", err)
	}
	return strings.TrimSuffix(d.HelixURL, "/") + path, nil
}

// GraphReader runs the API's read queries as the directory's own
// ExecuteDatabaseReadQuery does, with its checks and limits.
type GraphReader struct{ Directory *directory.Service }

var _ ports.GraphReader = GraphReader{}

func (g GraphReader) ReadQuery(ctx context.Context, tenantID string, query []byte) (int, []byte, error) {
	resp, err := g.Directory.ReadQuery(ctx, tenantID, query)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, resp.Body, nil
}

// People is the install's one person, the owner (ports.People).
type People struct{ Directory Directory }

var _ ports.People = People{}

// MayManageAI is true for the owner in a workspace that is live: they may
// do everything the install serves.
func (p People) MayManageAI(ctx context.Context, workspaceID, userID string) bool {
	owner, err := p.Directory.Owner(ctx)
	if err != nil || userID == "" || owner.ID != userID {
		return false
	}
	w, err := p.Directory.Workspace(ctx, workspaceID)
	return err == nil && w.Live()
}

// DisplayName is the user's name, else their email.
func (p People) DisplayName(ctx context.Context, userID string) (string, error) {
	u, err := p.Directory.User(ctx, userID)
	if err != nil {
		return "", err
	}
	if u.DisplayName != "" {
		return u.DisplayName, nil
	}
	return u.Email, nil
}
