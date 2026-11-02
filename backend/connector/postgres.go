package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

type postgresKind struct {
	guard Guard
}

func (k *postgresKind) Name() string        { return "postgres" }
func (k *postgresKind) DisplayName() string { return "PostgreSQL" }

func (k *postgresKind) Prepare(src Source) (json.RawMessage, error) {
	s, err := prepareDatabase(src, 5432, "public")
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

// postgresSettings are the stored settings plus what Discover and Start work
// out for one sync. Discover recomputes Keyless and Unpublished every time,
// and Snapshot only ever travels from Start to the full read.
type postgresSettings struct {
	DatabaseSettings
	// Keyless are the tables without a primary key; they are always read in full.
	Keyless []string `json:"keyless,omitempty"`
	// Unpublished are, with CDC on, the keyed tables the publication leaves
	// out; they keep the cursor-column method.
	Unpublished []string `json:"unpublished,omitempty"`
	// Snapshot is the replication slot's exported snapshot, which the full read imports.
	Snapshot string `json:"snapshot,omitempty"`
}

func decodePostgresSettings(raw json.RawMessage) (postgresSettings, error) {
	var s postgresSettings
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, invalid("database settings are malformed")
	}
	return s, nil
}

// config dials through the guard: pgx resolves the host with the guard's
// lookup, then dials the vetted address.
func (k *postgresKind) config(s DatabaseSettings, password string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = s.Host, uint16(s.Port), s.Database, s.User, password
	cfg.TLSConfig = s.tlsConfig()
	cfg.Fallbacks = nil
	cfg.ConnectTimeout = 10 * time.Second
	cfg.RuntimeParams = map[string]string{"application_name": "helix-foundry"}
	cfg.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
		addrs, err := k.guard.Resolve(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(addrs))
		for i, a := range addrs {
			out[i] = a.String()
		}
		return out, nil
	}
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return k.guard.DialContext(ctx, network, addr)
	}
	return cfg, nil
}

func (k *postgresKind) connect(ctx context.Context, s DatabaseSettings, password string) (*pgx.Conn, error) {
	cfg, err := k.config(s, password)
	if err != nil {
		return nil, err
	}
	return pgx.ConnectConfig(ctx, cfg)
}

func (k *postgresKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	var s DatabaseSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return Discovery{}, invalid("database settings are malformed")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := k.connect(ctx, s, src.Secret)
	if err != nil {
		return Discovery{}, connectFailure(err)
	}
	defer conn.Close(context.Background())

	tables := newTableSet()
	if err := pgRows(ctx, conn, `SELECT table_name FROM information_schema.tables
WHERE table_schema=$1 AND table_type='BASE TABLE'`, []any{s.Schema}, func(r pgx.Rows) error {
		var name string
		if err := r.Scan(&name); err != nil {
			return err
		}
		tables.table(name)
		return nil
	}); err != nil {
		return Discovery{}, connectFailure(err)
	}
	if err := pgRows(ctx, conn, `SELECT table_name, column_name, data_type, is_nullable FROM information_schema.columns
WHERE table_schema=$1 ORDER BY table_name, ordinal_position`, []any{s.Schema}, func(r pgx.Rows) error {
		var table, column, typ, nullable string
		if err := r.Scan(&table, &column, &typ, &nullable); err != nil {
			return err
		}
		if t, ok := tables.byName[table]; ok {
			t.Columns = append(t.Columns, Column{Name: column, Type: typ, Nullable: nullable == "YES"})
		}
		return nil
	}); err != nil {
		return Discovery{}, connectFailure(err)
	}
	// Keys come from pg_catalog: information_schema shows constraints only to a
	// table's owner or a user with more than SELECT, and customers connect readers.
	if err := pgRows(ctx, conn, `SELECT c.relname, a.attname
FROM pg_constraint con
JOIN pg_class c ON c.oid=con.conrelid
JOIN pg_namespace n ON n.oid=c.relnamespace
CROSS JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
JOIN pg_attribute a ON a.attrelid=con.conrelid AND a.attnum=k.attnum
WHERE con.contype='p' AND n.nspname=$1
ORDER BY c.relname, k.ord`, []any{s.Schema}, func(r pgx.Rows) error {
		var table, column string
		if err := r.Scan(&table, &column); err != nil {
			return err
		}
		if t, ok := tables.byName[table]; ok {
			t.PrimaryKey = append(t.PrimaryKey, column)
		}
		return nil
	}); err != nil {
		return Discovery{}, connectFailure(err)
	}
	if err := pgRows(ctx, conn, `SELECT c.relname, a.attname, rc.relname, ra.attname
FROM pg_constraint con
JOIN pg_class c ON c.oid=con.conrelid
JOIN pg_namespace n ON n.oid=c.relnamespace
JOIN pg_class rc ON rc.oid=con.confrelid
JOIN pg_namespace rn ON rn.oid=rc.relnamespace
CROSS JOIN LATERAL unnest(con.conkey, con.confkey) WITH ORDINALITY AS k(attnum, refattnum, ord)
JOIN pg_attribute a ON a.attrelid=con.conrelid AND a.attnum=k.attnum
JOIN pg_attribute ra ON ra.attrelid=con.confrelid AND ra.attnum=k.refattnum
WHERE con.contype='f' AND n.nspname=$1 AND rn.nspname=$1
ORDER BY c.relname, con.conname, k.ord`, []any{s.Schema}, func(r pgx.Rows) error {
		var table, column, refTable, refColumn string
		if err := r.Scan(&table, &column, &refTable, &refColumn); err != nil {
			return err
		}
		if t, ok := tables.byName[table]; ok {
			t.ForeignKeys = append(t.ForeignKeys, ForeignKey{Column: column, ReferencedTable: refTable, ReferencedColumn: refColumn})
		}
		return nil
	}); err != nil {
		return Discovery{}, connectFailure(err)
	}
	d, err := tables.discovery(s.Tables)
	if err != nil || s.CDC == nil && len(s.CursorColumns) == 0 {
		return d, err
	}
	return k.discoverFollowing(ctx, conn, src, s, d)
}

// discoverFollowing checks what a change read needs and records, in the
// settings this sync runs with, the tables that can't be followed.
func (k *postgresKind) discoverFollowing(ctx context.Context, conn *pgx.Conn, src Source, s DatabaseSettings, d Discovery) (Discovery, error) {
	if err := checkCursorColumns(d, s.CursorColumns); err != nil {
		return Discovery{}, err
	}
	if len(s.CursorColumns) > 0 && !src.ForSync {
		indexed := map[string]bool{}
		if err := pgRows(ctx, conn, `SELECT c.relname, a.attname FROM pg_index i
JOIN pg_class c ON c.oid = i.indrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
WHERE n.nspname = $1`, []any{s.Schema}, func(r pgx.Rows) error {
			var table, column string
			err := r.Scan(&table, &column)
			indexed[table+"."+column] = true
			return err
		}); err != nil {
			return Discovery{}, connectFailure(err)
		}
		d.Warnings = unindexedCursorColumns(s.CursorColumns, indexed, func(table, column string) string {
			return fmt.Sprintf("CREATE INDEX ON %s (%s)", pgx.Identifier{s.Schema, table}.Sanitize(), pgx.Identifier{column}.Sanitize())
		})
	}
	ps := postgresSettings{DatabaseSettings: s}
	var keyed []string
	for _, t := range d.Tables {
		switch {
		case len(t.PrimaryKey) == 0:
			ps.Keyless = append(ps.Keyless, t.Name)
		case len(s.Tables) == 0 || slices.Contains(s.Tables, t.Name):
			keyed = append(keyed, t.Name)
		}
	}
	// A sync on a plan without the log reads no log, so a dropped publication
	// or a revoked grant there must not stop it.
	if s.CDC != nil && (!src.ForSync || src.Allows(CaptureLog)) {
		var warnings []string
		var err error
		if ps.Unpublished, warnings, err = k.checkLog(ctx, conn, src, s, keyed); err != nil {
			return Discovery{}, err
		}
		if !src.ForSync {
			d.Warnings = append(d.Warnings, warnings...)
		}
	}
	settings, err := json.Marshal(ps)
	if err != nil {
		return Discovery{}, fmt.Errorf("encode postgres settings: %w", err)
	}
	d.Settings = settings
	return d, nil
}

func pgRows(ctx context.Context, conn *pgx.Conn, query string, args []any, each func(pgx.Rows) error) error {
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (k *postgresKind) Hint(string) string { return "password set" }

func (k *postgresKind) Target(settings json.RawMessage) (string, error) {
	return databaseTarget(settings)
}

// Sync copies one table from a consistent snapshot. PostgreSQL encodes each
// row as JSON itself, so no column type needs mapping here.
func (k *postgresKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	return table, k.copyRows(ctx, src, table.Name, w)
}

// pgRowJSON is how every read encodes a row of the table aliased t, so a row
// a change read writes has the bytes a full read gives it.
const pgRowJSON = "row_to_json(t)::text"

// pgSnapshotRE is the shape of an exported snapshot's name, which SET
// TRANSACTION SNAPSHOT takes only as a literal.
var pgSnapshotRE = regexp.MustCompile(`^[0-9A-Fa-f-]+$`)

func (k *postgresKind) copyRows(ctx context.Context, src Source, table string, w RowWriter) error {
	s, err := decodePostgresSettings(src.Settings)
	if err != nil {
		return err
	}
	if s.Snapshot != "" && !pgSnapshotRE.MatchString(s.Snapshot) {
		return fmt.Errorf("snapshot name %q is malformed", s.Snapshot)
	}
	conn, err := k.connect(ctx, s.DatabaseSettings, src.Secret)
	if err != nil {
		return connectFailure(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return connectFailure(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // a no-op after Commit
	if s.Snapshot != "" {
		if _, err := tx.Exec(ctx, "SET TRANSACTION SNAPSHOT '"+s.Snapshot+"'"); err != nil {
			return fmt.Errorf("import snapshot %s: %w", s.Snapshot, err)
		}
	}
	rows, err := tx.Query(ctx, "SELECT "+pgRowJSON+" FROM "+pgx.Identifier{s.Schema, table}.Sanitize()+" t")
	if err != nil {
		return fmt.Errorf("read %s.%s: %w", s.Schema, table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var row []byte
		if err := rows.Scan(&row); err != nil {
			return fmt.Errorf("read %s.%s: %w", s.Schema, table, err)
		}
		if err := w.WriteJSON(row); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s.%s: %w", s.Schema, table, err)
	}
	return tx.Commit(ctx)
}
