// Package local runs Foundry on one machine: a standalone install, with one
// owner and one HelixDB, and no billing. It implements the ports Foundry
// needs of its host (package ports) over the install's directory (package
// directory), and its front end: the site token that authenticates every
// call as the owner, and the API's methods the install serves.
//
// What a standalone install has:
//
//   - one owner, who signs in with the site token the Foundry site shares
//     with the install (Authenticator), and may do everything the install
//     serves in every workspace they make;
//   - one HelixDB, at local.helix_url, every database is queried at;
//   - one plan, sold to nobody: Plans and PlanReader are nil, so every
//     workspace is on the catalog's first plan, which the install's
//     configuration makes allow every connector kind;
//   - no rate-limit boosts (Boosts is nil), no People enrichment, and no
//     door for event sources' senders (their send tokens are refused).
package local

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pressly/goose/v3"

	"github.com/helixdb/helix-foundry/backend/local/directory"
	directorymigrations "github.com/helixdb/helix-foundry/backend/local/directory/migrations"
	"github.com/helixdb/helix-foundry/backend/store"
	foundrymigrations "github.com/helixdb/helix-foundry/backend/store/migrations"
)

// Migrate applies the directory's migrations, then Foundry's (which keep
// goose's default version table, goose_db_version, as a hosted deployment's
// do). Each set records its own versions, so a re-run applies only what is
// new.
func Migrate(ctx context.Context, db *sql.DB) error {
	dir, err := directorymigrations.Provider(db)
	if err != nil {
		return err
	}
	if _, err := dir.Up(ctx); err != nil {
		return fmt.Errorf("directory migrations: %w", err)
	}
	foundry, err := goose.NewProvider(goose.DialectMySQL, db, foundrymigrations.FS)
	if err != nil {
		return fmt.Errorf("foundry migrations: %w", err)
	}
	if _, err := foundry.Up(ctx); err != nil {
		return fmt.Errorf("foundry migrations: %w", err)
	}
	return nil
}

// Workspaces is the directory's workspaces table as Foundry's store reads
// and locks it (store.Workspaces).
type Workspaces struct{}

var _ store.Workspaces = Workspaces{}

func (Workspaces) Table() string { return "workspaces" }

// Lock locks a workspace's row in q's transaction unless it is deleted.
func (Workspaces) Lock(ctx context.Context, q store.Executor, id string) error {
	var locked string
	err := q.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id = ? AND deleted_at IS NULL FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	return err
}

// Deleting reports whether a workspace is deleted or being deleted.
func (Workspaces) Deleting(ctx context.Context, q store.Executor, id string) (bool, error) {
	var deleting bool
	err := q.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL OR status IN (?, ?) FROM workspaces WHERE id = ?`,
		directory.StatusDeleting, directory.StatusDeleted, id).Scan(&deleting)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.ErrNotFound
	}
	return deleting, err
}
