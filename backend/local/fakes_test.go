package local

import (
	"context"
	"time"

	"github.com/helixdb/helix-foundry/backend/local/directory"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// fakeDirectory is a directory in memory; err, when set, is every read's.
type fakeDirectory struct {
	owner      *directory.User
	users      map[string]directory.User
	workspaces map[string]directory.Workspace
	projects   map[string]directory.Project
	tenants    map[string]directory.Tenant
	err        error
}

func get[T any](m map[string]T, id string, err error) (T, error) {
	var zero T
	if err != nil {
		return zero, err
	}
	v, ok := m[id]
	if !ok {
		return zero, store.ErrNotFound
	}
	return v, nil
}

func (f *fakeDirectory) Owner(context.Context) (directory.User, error) {
	if f.err != nil {
		return directory.User{}, f.err
	}
	if f.owner == nil {
		return directory.User{}, store.ErrNotFound
	}
	return *f.owner, nil
}

func (f *fakeDirectory) User(_ context.Context, id string) (directory.User, error) {
	return get(f.users, id, f.err)
}

func (f *fakeDirectory) Workspace(_ context.Context, id string) (directory.Workspace, error) {
	return get(f.workspaces, id, f.err)
}

func (f *fakeDirectory) Project(_ context.Context, id string) (directory.Project, error) {
	return get(f.projects, id, f.err)
}

func (f *fakeDirectory) Tenant(_ context.Context, id string) (directory.Tenant, error) {
	return get(f.tenants, id, f.err)
}

// fakeOntologies are ontologies by workspace and id.
type fakeOntologies map[[2]string]*model.FoundryOntology

func (f fakeOntologies) GetFoundryOntology(_ context.Context, workspaceID, id string) (*model.FoundryOntology, error) {
	o, ok := f[[2]string{workspaceID, id}]
	if !ok {
		return nil, store.ErrNotFound
	}
	return o, nil
}

// testDirectory has the owner, a live workspace ws with project p and
// database t, and a deleted workspace gone.
func testDirectory() *fakeDirectory {
	owner := directory.User{ID: "owner", Email: directory.OwnerEmail, DisplayName: "Owner"}
	gone := time.Now()
	return &fakeDirectory{
		owner: &owner,
		users: map[string]directory.User{"owner": owner, "nameless": {ID: "nameless", Email: "n@localhost"}},
		workspaces: map[string]directory.Workspace{
			"ws":       {ID: "ws", Status: directory.StatusActive},
			"gone":     {ID: "gone", Status: directory.StatusActive, DeletedAt: &gone},
			"deleting": {ID: "deleting", Status: directory.StatusDeleting},
		},
		projects: map[string]directory.Project{
			"p":    {ID: "p", WorkspaceID: "ws"},
			"dead": {ID: "dead", WorkspaceID: "ws", DeletedAt: &gone},
		},
		tenants: map[string]directory.Tenant{
			"t":       {ID: "t", WorkspaceID: "ws", ProjectID: "p", Name: "Shop", Status: directory.StatusActive},
			"orphan":  {ID: "orphan", WorkspaceID: "ws", ProjectID: "dead", Status: directory.StatusActive},
			"deleted": {ID: "deleted", WorkspaceID: "ws", ProjectID: "p", Status: directory.StatusActive, DeletedAt: &gone},
			"retired": {ID: "retired", WorkspaceID: "ws", ProjectID: "p", Status: directory.StatusDeleted},
		},
	}
}
