package connector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func (k *mysqlKind) Capture(src Source, table string) Capture {
	var s DatabaseSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return CaptureFull
	}
	switch {
	case logsChanges(s, src):
		return CaptureLog
	case s.CursorColumns[table] != "" && src.Allows(CaptureCursor):
		return CaptureCursor
	}
	return CaptureFull
}

// logsChanges reports whether changes come from the binlog: CDC is on and the plan allows it.
func logsChanges(s DatabaseSettings, src Source) bool {
	return s.CDC != nil && src.Allows(CaptureLog)
}

func (k *mysqlKind) Start(ctx context.Context, src Source, tables []Table) (Start, error) {
	var s DatabaseSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return Start{}, invalid("database settings are malformed")
	}
	if logsChanges(s, src) {
		return k.startBinlog(ctx, s, src)
	}
	cursor, err := k.cursorChanges(ctx, s, src, tables, mysqlCursors{}, nil)
	return Start{Cursor: cursor}, err
}

func (k *mysqlKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	var s DatabaseSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return nil, invalid("database settings are malformed")
	}
	if logsChanges(s, src) {
		return k.binlogChanges(ctx, s, src, tables, cursor, w)
	}
	var prev mysqlCursors
	if err := json.Unmarshal(cursor, &prev); err != nil {
		return nil, ErrCursorExpired
	}
	return k.cursorChanges(ctx, s, src, tables, prev, w)
}

// mysqlCursors is a cursor-column connector's change cursor.
type mysqlCursors struct {
	Tables map[string]mysqlColumnCursor `json:"tables"`
}

type mysqlColumnCursor struct {
	Column string `json:"column"`
	// After is the value the next read starts past, as MySQL prints it; empty
	// while an integer column holds no value.
	After string `json:"after,omitempty"`
}

// cursorChanges reads every row whose cursor column moved past prev, in one
// read-only snapshot, and returns where the next read starts. With a nil w it
// only takes the cursor a full read starts from.
//  1. Take the time cutoff before the snapshot opens, so a row written in
//     between is read now and again next time.
//  2. Per table: an integer cursor moves to the column's highest value in the
//     snapshot; a time cursor to the cutoff.
//  3. A table that is new to the cursor, or whose column changed, is read in full.
func (k *mysqlKind) cursorChanges(ctx context.Context, s DatabaseSettings, src Source, tables []Table,
	prev mysqlCursors, w ChangeWriter) (json.RawMessage, error) {
	db := k.open(s, src.Secret)
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, connectFailure(err)
	}
	defer conn.Close()
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = t.Name
	}
	columns, err := mysqlColumnsOf(ctx, conn, s.Database, names)
	if err != nil {
		return nil, connectFailure(err)
	}
	// A DATETIME holds whatever zone the application wrote, local or UTC, so
	// the cutoff is the earlier of the two; a row read twice changes nothing.
	var cutoff string
	if err := conn.QueryRowContext(ctx, fmt.Sprintf("SELECT LEAST(NOW(6), UTC_TIMESTAMP(6)) - INTERVAL %d MICROSECOND",
		ChangeOverlap.Microseconds())).Scan(&cutoff); err != nil {
		return nil, connectFailure(err)
	}
	if _, err := conn.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		return nil, connectFailure(err)
	}
	if _, err := conn.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY"); err != nil {
		return nil, connectFailure(err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }() // ends the read-only snapshot
	next := mysqlCursors{Tables: map[string]mysqlColumnCursor{}}
	for _, t := range tables {
		column := s.CursorColumns[t.Name]
		if column == "" {
			continue
		}
		var col mysqlColumn
		for _, c := range columns[t.Name] {
			if c.Name == column {
				col = c
			}
		}
		kind := mysqlCursorKind(col.DataType)
		if kind == "" {
			return nil, invalid("cursor column %s of table %s is missing or holds neither a time nor an integer", column, t.Name)
		}
		cursor := mysqlColumnCursor{Column: column, After: cutoff}
		if kind == mysqlCursorInt {
			var highest sql.NullString
			if err := conn.QueryRowContext(ctx, "SELECT MAX("+mysqlIdentifier(column)+") FROM "+mysqlIdentifier(t.Name)).Scan(&highest); err != nil {
				return nil, fmt.Errorf("read the highest %s of %s: %w", column, t.Name, err)
			}
			cursor.After = highest.String
		}
		old, seen := prev.Tables[t.Name]
		if kind == mysqlCursorInt && cursor.After == "" {
			cursor.After = old.After
		}
		next.Tables[t.Name] = cursor
		switch {
		case w == nil:
		case len(t.PrimaryKey) == 0:
			w.Reset(t.Name, "no primary key")
		case !seen || old.Column != column:
			w.Reset(t.Name, "cursor column changed")
		default:
			if err := mysqlReadPastCursor(ctx, conn, t.Name, columns[t.Name], column, kind, old.After, w); err != nil {
				return nil, err
			}
		}
	}
	return json.Marshal(next)
}

func mysqlReadPastCursor(ctx context.Context, conn *sql.Conn, table string, columns []mysqlColumn, column, kind, after string, w ChangeWriter) error {
	names := make([]string, len(columns))
	for i, c := range columns {
		names[i] = c.Name
	}
	query := mysqlRowSelect(table, names) + " WHERE " + mysqlIdentifier(column)
	var args []any
	switch {
	case kind == mysqlCursorDate:
		query, args = query+" >= DATE(?)", []any{after}
	case kind == mysqlCursorTime:
		query, args = query+" > ?", []any{after}
	case after == "":
		query += " IS NOT NULL"
	default:
		// Integers bind as integers: MySQL compares an integer with a string as doubles, which lose large ids.
		if v, err := strconv.ParseInt(after, 10, 64); err == nil {
			args = []any{v}
		} else if v, err := strconv.ParseUint(after, 10, 64); err == nil {
			args = []any{v}
		} else {
			w.Reset(table, "cursor unreadable")
			return nil
		}
		query += " > ?"
	}
	if err := sqlRows(ctx, conn, query, args, func(r *sql.Rows) error {
		var row []byte
		if err := r.Scan(&row); err != nil {
			return err
		}
		return w.UpsertJSON(table, row)
	}); err != nil {
		return fmt.Errorf("read changed rows of %s: %w", table, err)
	}
	return nil
}

const (
	mysqlCursorTime = "time"
	mysqlCursorDate = "date"
	mysqlCursorInt  = "int"
)

// mysqlCursorKind is how a cursor column of this type is compared, from
// DATA_TYPE or COLUMN_TYPE; empty for a type that can't be a cursor.
func mysqlCursorKind(typ string) string {
	base, _, _ := strings.Cut(strings.ToLower(typ), "(")
	base, _, _ = strings.Cut(base, " ")
	switch base {
	case "datetime", "timestamp":
		return mysqlCursorTime
	case "date":
		return mysqlCursorDate
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
		return mysqlCursorInt
	}
	return ""
}

// checkMySQLCursorColumn refuses a cursor column the table lacks or that holds
// neither a time nor an integer. A nil t is a table the database lacks.
func checkMySQLCursorColumn(t *Table, table, column string) error {
	if t == nil {
		return invalid("cursor column %s names table %s, which is not in this database", column, table)
	}
	for _, c := range t.Columns {
		if c.Name != column {
			continue
		}
		if mysqlCursorKind(c.Type) == "" {
			return invalid("cursor column %s of table %s is %s; choose a date, time or integer column", column, table, c.Type)
		}
		return nil
	}
	return invalid("table %s has no column %s to use as its cursor", table, column)
}

// mysqlColumn is one column as information_schema.COLUMNS describes it.
type mysqlColumn struct {
	Name     string
	DataType string
	Unsigned bool
	Charset  string
	// Octets is a character or binary column's length in bytes; MySQL pads a BINARY value to it.
	Octets int64
}

// mysqlColumnsOf reads the columns of tables, each in ordinal order.
func mysqlColumnsOf(ctx context.Context, q queryer, database string, tables []string) (map[string][]mysqlColumn, error) {
	out := map[string][]mysqlColumn{}
	if len(tables) == 0 {
		return out, nil
	}
	args := []any{database}
	for _, t := range tables {
		args = append(args, t)
	}
	err := sqlRows(ctx, q, `SELECT TABLE_NAME, COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, CHARACTER_SET_NAME, CHARACTER_OCTET_LENGTH
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA=? AND TABLE_NAME IN (?`+strings.Repeat(",?", len(tables)-1)+`)
ORDER BY TABLE_NAME, ORDINAL_POSITION`, args, func(r *sql.Rows) error {
		var table, columnType string
		var c mysqlColumn
		var charset sql.NullString
		var octets sql.NullInt64
		if err := r.Scan(&table, &c.Name, &c.DataType, &columnType, &charset, &octets); err != nil {
			return err
		}
		c.DataType = strings.ToLower(c.DataType)
		c.Unsigned = strings.Contains(strings.ToLower(columnType), "unsigned")
		c.Charset, c.Octets = charset.String, octets.Int64
		out[table] = append(out[table], c)
		return nil
	})
	return out, err
}
