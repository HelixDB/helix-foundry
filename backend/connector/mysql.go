package connector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

// The MySQL driver looks dialers up by network name in a process-wide
// registry, so each guard policy gets one registered name.
const (
	mysqlGuardedNet = "helix-foundry-guarded"
	mysqlPrivateNet = "helix-foundry-private"
)

var registerMySQLDialers sync.Once

type mysqlKind struct {
	guard Guard
}

func (k *mysqlKind) Name() string        { return "mysql" }
func (k *mysqlKind) DisplayName() string { return "MySQL" }

func (k *mysqlKind) Prepare(src Source) (json.RawMessage, error) {
	s, err := prepareDatabase(src, 3306, "")
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

func (k *mysqlKind) open(s DatabaseSettings, password string) *sql.DB {
	registerMySQLDialers.Do(func() {
		mysql.RegisterDialContext(mysqlGuardedNet, func(ctx context.Context, addr string) (net.Conn, error) {
			return Guard{}.DialContext(ctx, "tcp", addr)
		})
		mysql.RegisterDialContext(mysqlPrivateNet, func(ctx context.Context, addr string) (net.Conn, error) {
			return Guard{AllowPrivate: true}.DialContext(ctx, "tcp", addr)
		})
	})
	cfg := mysql.NewConfig()
	cfg.Net = mysqlGuardedNet
	if k.guard.AllowPrivate {
		cfg.Net = mysqlPrivateNet
	}
	cfg.Addr = net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	cfg.User, cfg.Passwd, cfg.DBName = s.User, password, s.Database
	cfg.TLS = s.tlsConfig()
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 10*time.Second, 30*time.Second, 30*time.Second
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		// NewConnector only fails on an invalid config, which Prepare rules out.
		panic(err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	return db
}

func (k *mysqlKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	var s DatabaseSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return Discovery{}, invalid("database settings are malformed")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db := k.open(s, src.Secret)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return Discovery{}, connectFailure(err)
	}

	tables := newTableSet()
	if err := sqlRows(ctx, db, `SELECT TABLE_NAME FROM information_schema.TABLES
WHERE TABLE_SCHEMA=? AND TABLE_TYPE='BASE TABLE'`, []any{s.Database}, func(r *sql.Rows) error {
		var name string
		if err := r.Scan(&name); err != nil {
			return err
		}
		tables.table(name)
		return nil
	}); err != nil {
		return Discovery{}, connectFailure(err)
	}
	if err := sqlRows(ctx, db, `SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA=? ORDER BY TABLE_NAME, ORDINAL_POSITION`, []any{s.Database}, func(r *sql.Rows) error {
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
	// Primary keys and same-database foreign keys; a reference into another database is dropped.
	if err := sqlRows(ctx, db, `SELECT TABLE_NAME, COLUMN_NAME, CONSTRAINT_NAME, REFERENCED_TABLE_SCHEMA,
       REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME
FROM information_schema.KEY_COLUMN_USAGE
WHERE TABLE_SCHEMA=? ORDER BY TABLE_NAME, CONSTRAINT_NAME, ORDINAL_POSITION`, []any{s.Database}, func(r *sql.Rows) error {
		var table, column, constraint string
		var refSchema, refTable, refColumn sql.NullString
		if err := r.Scan(&table, &column, &constraint, &refSchema, &refTable, &refColumn); err != nil {
			return err
		}
		t, ok := tables.byName[table]
		switch {
		case !ok:
		case constraint == "PRIMARY":
			t.PrimaryKey = append(t.PrimaryKey, column)
		case refTable.Valid && refSchema.String == s.Database:
			t.ForeignKeys = append(t.ForeignKeys, ForeignKey{Column: column, ReferencedTable: refTable.String, ReferencedColumn: refColumn.String})
		}
		return nil
	}); err != nil {
		return Discovery{}, connectFailure(err)
	}
	for _, table := range slices.Sorted(maps.Keys(s.CursorColumns)) {
		if err := checkMySQLCursorColumn(tables.byName[table], table, s.CursorColumns[table]); err != nil {
			return Discovery{}, err
		}
	}
	// A sync needs only the tables; the binlog's settings are checked when the customer tests.
	if s.CDC != nil && !src.ForSync {
		if err := k.checkBinlog(ctx, db, s, src); err != nil {
			return Discovery{}, err
		}
	}
	d, err := tables.discovery(s.Tables)
	if err != nil || len(s.CursorColumns) == 0 || src.ForSync {
		return d, err
	}
	indexed := map[string]bool{}
	if err := sqlRows(ctx, db, `SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA=? AND SEQ_IN_INDEX=1`, []any{s.Database}, func(r *sql.Rows) error {
		var table, column string
		err := r.Scan(&table, &column)
		indexed[table+"."+column] = true
		return err
	}); err != nil {
		return Discovery{}, connectFailure(err)
	}
	d.Warnings = unindexedCursorColumns(s.CursorColumns, indexed, func(table, column string) string {
		return fmt.Sprintf("CREATE INDEX helix_cursor_%s ON `%s` (`%s`)", column, table, column)
	})
	return d, nil
}

// queryer is a *sql.DB or a *sql.Conn holding a transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func sqlRows(ctx context.Context, q queryer, query string, args []any, each func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
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

func (k *mysqlKind) Hint(string) string { return "password set" }

func (k *mysqlKind) Target(settings json.RawMessage) (string, error) { return databaseTarget(settings) }

// Sync copies one table from a consistent snapshot, with MySQL encoding each
// row as a JSON object.
func (k *mysqlKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	return table, k.copyRows(ctx, src, table.Name, w)
}

func (k *mysqlKind) copyRows(ctx context.Context, src Source, table string, w RowWriter) error {
	var s DatabaseSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return invalid("database settings are malformed")
	}
	db := k.open(s, src.Secret)
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return connectFailure(err)
	}
	defer conn.Close()
	var columns []string
	if err := sqlRows(ctx, conn, `SELECT COLUMN_NAME FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION`, []any{s.Database, table}, func(r *sql.Rows) error {
		var name string
		if err := r.Scan(&name); err != nil {
			return err
		}
		columns = append(columns, name)
		return nil
	}); err != nil {
		return connectFailure(err)
	}
	if len(columns) == 0 {
		return invalid("table %q is not in this database, or the user cannot read it", table)
	}
	if _, err := conn.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		return connectFailure(err)
	}
	if _, err := conn.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY"); err != nil {
		return connectFailure(err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }() // ends the read-only snapshot
	return sqlRows(ctx, conn, mysqlRowSelect(table, columns), nil,
		func(r *sql.Rows) error {
			var row []byte
			if err := r.Scan(&row); err != nil {
				return err
			}
			return w.WriteJSON(row)
		})
}

// mysqlRowSelect reads each row of table as MySQL encodes it in JSON. Change
// reads fetch rows with it too, so a row's bytes match its full read.
func mysqlRowSelect(table string, columns []string) string {
	pairs := make([]string, len(columns))
	for i, c := range columns {
		pairs[i] = "'" + strings.ReplaceAll(strings.ReplaceAll(c, `\`, `\\`), "'", `\'`) + "', " + mysqlIdentifier(c)
	}
	return "SELECT JSON_OBJECT(" + strings.Join(pairs, ", ") + ") FROM " + mysqlIdentifier(table)
}

func mysqlIdentifier(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }
