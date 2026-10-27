// Package directory is a standalone Foundry install's directory: its one
// owner, the workspaces they make, and the projects and databases (tenants)
// ontologies load into, on tables of its own (package migrations). Service
// serves them as helix.foundry.directory.v1.DirectoryService, the routes the
// Foundry site calls of a hosted control plane, with the same JSON.
//
// The owner is a member, and the owner, of every workspace: nobody else
// signs in to a standalone install.
package directory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store"
)

// OwnerEmail is the owner's address. A standalone install sends no mail.
const OwnerEmail = "owner@localhost"

// ownerName is the name the owner is shown by.
const ownerName = "Owner"

// The statuses of a workspace or database row. Foundry's store treats a
// workspace that is deleting or deleted as gone (store.Workspaces).
const (
	StatusActive   = "active"
	StatusDeleting = "deleting"
	StatusDeleted  = "deleted"
)

// User is a row of users: the owner.
type User struct {
	ID          string
	Email       string
	DisplayName string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Workspace is a row of workspaces.
type Workspace struct {
	ID          string
	Slug        string
	DisplayName string
	Region      string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// Live reports whether the workspace is neither deleted nor being deleted.
func (w Workspace) Live() bool {
	return w.DeletedAt == nil && w.Status != StatusDeleting && w.Status != StatusDeleted
}

// Project is a row of projects.
type Project struct {
	ID          string
	WorkspaceID string
	Slug        string
	DisplayName string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// Tenant is a row of tenants: a database on the install's HelixDB.
type Tenant struct {
	ID          string
	WorkspaceID string
	ProjectID   string
	Name        string
	Slug        string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// Deleted reports whether the database's deletion was recorded.
func (t Tenant) Deleted() bool { return t.DeletedAt != nil || t.Status == StatusDeleted }

// Store is the directory's tables. Its lookups return store.ErrNotFound for
// a row that is absent.
type Store struct{ db *sql.DB }

// NewStore is the directory on db, migrated (package migrations).
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// SeedOwner writes the owner's row unless it is there already: every start
// runs it.
func (s *Store) SeedOwner(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users (id, email, display_name) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE id = id`, id.New(), OwnerEmail, ownerName)
	if err != nil {
		return fmt.Errorf("seed owner: %w", err)
	}
	return nil
}

const userColumns = `id, email, display_name, created_at, updated_at`

func scanUser(row *sql.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, store.ErrNotFound
	}
	return u, err
}

// Owner is the owner's row.
func (s *Store) Owner(ctx context.Context) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`, OwnerEmail))
}

// User is the user with id.
func (s *Store) User(ctx context.Context, id string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

const workspaceColumns = `id, slug, display_name, region, status, created_at, updated_at, deleted_at`

type scanner interface{ Scan(dest ...any) error }

func scanWorkspace(row scanner) (Workspace, error) {
	var w Workspace
	var deleted sql.NullTime
	err := row.Scan(&w.ID, &w.Slug, &w.DisplayName, &w.Region, &w.Status, &w.CreatedAt, &w.UpdatedAt, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, store.ErrNotFound
	}
	if deleted.Valid {
		w.DeletedAt = &deleted.Time
	}
	return w, err
}

// Workspace is the workspace with id, deleted or not.
func (s *Store) Workspace(ctx context.Context, id string) (Workspace, error) {
	return scanWorkspace(s.db.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspaces WHERE id = ?`, id))
}

// ListWorkspaces lists up to limit live workspaces, newest first, after the
// one with id after (none: from the newest).
func (s *Store) ListWorkspaces(ctx context.Context, after string, limit int) ([]Workspace, error) {
	return list(ctx, s.db, scanWorkspace, "workspaces", workspaceColumns,
		`deleted_at IS NULL AND status NOT IN ('deleting', 'deleted')`, after, limit)
}

// AnyWorkspace reports whether the install has a workspace, deleted or not.
func (s *Store) AnyWorkspace(ctx context.Context) (bool, error) {
	var has bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces)`).Scan(&has)
	return has, err
}

// CreateWorkspace inserts w, active: store.ErrConflict when its slug is taken.
func (s *Store) CreateWorkspace(ctx context.Context, w Workspace) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspaces (id, slug, display_name, region, status) VALUES (?, ?, ?, ?, ?)`,
		w.ID, w.Slug, w.DisplayName, w.Region, StatusActive)
	if isDuplicate(err) {
		return store.ErrConflict
	}
	return err
}

const projectColumns = `id, workspace_id, slug, display_name, created_at, updated_at, deleted_at`

func scanProject(row scanner) (Project, error) {
	var p Project
	var deleted sql.NullTime
	err := row.Scan(&p.ID, &p.WorkspaceID, &p.Slug, &p.DisplayName, &p.CreatedAt, &p.UpdatedAt, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, store.ErrNotFound
	}
	if deleted.Valid {
		p.DeletedAt = &deleted.Time
	}
	return p, err
}

// Project is the project with id, deleted or not.
func (s *Store) Project(ctx context.Context, id string) (Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
}

// ListProjects lists up to limit of a workspace's live projects, newest
// first, after the one with id after.
func (s *Store) ListProjects(ctx context.Context, workspaceID, after string, limit int) ([]Project, error) {
	return list(ctx, s.db, scanProject, "projects", projectColumns, `workspace_id = ? AND deleted_at IS NULL`, after, limit, workspaceID)
}

// MaxSlug is the longest slug a workspace, project or database takes.
const MaxSlug = 64

// CreateProject inserts a project of a live workspace with slug and name,
// unless the workspace has one with that slug: then with the next free
// numeric suffix on both, "crm-2" and "CRM 2". It holds the workspace's row
// while it chooses, so two creations never pick the same suffix. A workspace
// that is absent or not live is NotFound.
func (s *Store) CreateProject(ctx context.Context, workspaceID, slug, name string) (Project, error) {
	var created string
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		var live bool
		err := tx.QueryRowContext(ctx, `SELECT deleted_at IS NULL AND status NOT IN ('deleting', 'deleted')
FROM workspaces WHERE id = ? FOR UPDATE`, workspaceID).Scan(&live)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !live {
			return status.Error(codes.NotFound, "workspace not found")
		}
		if err != nil {
			return fmt.Errorf("lock workspace: %w", err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT slug FROM projects WHERE workspace_id = ?`, workspaceID)
		if err != nil {
			return fmt.Errorf("list project slugs: %w", err)
		}
		taken := map[string]bool{}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				_ = rows.Close()
				return err
			}
			taken[strings.ToLower(s)] = true
		}
		if err := rows.Close(); err != nil {
			return err
		}
		candidateSlug, candidateName := slug, name
		for n := 2; taken[strings.ToLower(candidateSlug)]; n++ {
			suffix := strconv.Itoa(n)
			candidateSlug = strings.TrimRight(truncate(slug, MaxSlug-len(suffix)-1), "-_") + "-" + suffix
			candidateName = name + " " + suffix
		}
		created = id.New()
		_, err = tx.ExecContext(ctx, `INSERT INTO projects (id, workspace_id, slug, display_name) VALUES (?, ?, ?, ?)`,
			created, workspaceID, candidateSlug, candidateName)
		return err
	})
	if err != nil {
		return Project{}, err
	}
	return s.Project(ctx, created)
}

const tenantColumns = `id, workspace_id, project_id, name, slug, status, created_at, updated_at, deleted_at`

func scanTenant(row scanner) (Tenant, error) {
	var t Tenant
	var deleted sql.NullTime
	err := row.Scan(&t.ID, &t.WorkspaceID, &t.ProjectID, &t.Name, &t.Slug, &t.Status, &t.CreatedAt, &t.UpdatedAt, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, store.ErrNotFound
	}
	if deleted.Valid {
		t.DeletedAt = &deleted.Time
	}
	return t, err
}

// Tenant is the database with id, deleted or not.
func (s *Store) Tenant(ctx context.Context, id string) (Tenant, error) {
	return scanTenant(s.db.QueryRowContext(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE id = ?`, id))
}

// ListTenants lists up to limit of a project's databases, deleted ones too,
// newest first, after the one with id after.
func (s *Store) ListTenants(ctx context.Context, projectID, after string, limit int) ([]Tenant, error) {
	return list(ctx, s.db, scanTenant, "tenants", tenantColumns, `project_id = ?`, after, limit, projectID)
}

// CreateTenant inserts an active database named name with slug in a
// project. It holds the project's workspace's row while it checks both are
// live, so neither is deleted under it: a workspace or project that is not
// is FailedPrecondition, and a slug that is taken store.ErrConflict.
func (s *Store) CreateTenant(ctx context.Context, project Project, name, slug string) (Tenant, error) {
	created := id.New()
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		var live bool
		err := tx.QueryRowContext(ctx, `SELECT deleted_at IS NULL AND status NOT IN ('deleting', 'deleted')
FROM workspaces WHERE id = ? FOR UPDATE`, project.WorkspaceID).Scan(&live)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !live {
			return status.Error(codes.FailedPrecondition, "workspace is deleted")
		}
		if err != nil {
			return fmt.Errorf("lock workspace: %w", err)
		}
		var projectLive bool
		err = tx.QueryRowContext(ctx, `SELECT deleted_at IS NULL FROM projects WHERE id = ? FOR UPDATE`, project.ID).Scan(&projectLive)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !projectLive {
			return status.Error(codes.FailedPrecondition, "project is deleted")
		}
		if err != nil {
			return fmt.Errorf("lock project: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO tenants (id, workspace_id, project_id, name, slug, status) VALUES (?, ?, ?, ?, ?, ?)`,
			created, project.WorkspaceID, project.ID, name, slug, StatusActive)
		if isDuplicate(err) {
			return store.ErrConflict
		}
		return err
	})
	if err != nil {
		return Tenant{}, err
	}
	return s.Tenant(ctx, created)
}

// list reads up to limit rows of table where where holds, newest first,
// after the row with id after (none: from the newest). Rows created in the
// same microsecond are ordered by id.
func list[T any](ctx context.Context, db *sql.DB, scan func(scanner) (T, error), table, columns, where, after string, limit int,
	args ...any) ([]T, error) {
	query := `SELECT ` + columns + ` FROM ` + table + ` WHERE ` + where
	if after != "" {
		query += ` AND (created_at, id) < (SELECT created_at, id FROM ` + table + ` WHERE id = ?)`
		args = append(args, after)
	}
	rows, err := db.QueryContext(ctx, query+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		row, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// inTx runs fn in a transaction, committing when it returns nil.
func inTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func isDuplicate(err error) bool {
	var me *gomysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// truncate cuts s to at most n bytes; slugs are ASCII.
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
