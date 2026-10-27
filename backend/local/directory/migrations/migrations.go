// Package migrations is the schema of a standalone install's directory: its
// owner, workspaces, projects and databases. It keeps its own goose version
// table (Table), apart from Foundry's, so the two sets never share a version
// sequence and a hosted deployment never runs this one.
package migrations

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
)

// FS holds the migrations, named <version>_<name>.sql at its root.
//
//go:embed *.sql
var FS embed.FS

// Table is the goose version table of the directory's migrations.
const Table = "foundry_directory_goose_db_version"

// Provider runs the directory's migrations on db, recording them in Table.
func Provider(db *sql.DB) (*goose.Provider, error) {
	store, err := database.NewStore(database.DialectMySQL, Table)
	if err != nil {
		return nil, fmt.Errorf("directory migrations: %w", err)
	}
	return goose.NewProvider("", db, FS, goose.WithStore(store))
}
