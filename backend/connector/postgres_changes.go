package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// postgresCursor is where the next change read resumes. Slot and LSN follow
// the log, and Relations hold each logged table's definition as of LSN.
// Columns hold each cursor-column table's lower bound, null to read every row.
type postgresCursor struct {
	Slot      string             `json:"slot,omitempty"`
	LSN       string             `json:"lsn,omitempty"`
	Relations map[string]string  `json:"relations,omitempty"`
	Columns   map[string]*string `json:"columns,omitempty"`
	CDC       *postgresCDCStatus `json:"cdc,omitempty"`
}

// postgresCursorTypes are the information_schema types a cursor column may have.
var postgresCursorTypes = map[string]bool{
	"timestamp with time zone": true, "timestamp without time zone": true, "date": true,
	"smallint": true, "integer": true, "bigint": true,
}

func postgresIntegerType(typ string) bool {
	return typ == "smallint" || typ == "integer" || typ == "bigint"
}

func (k *postgresKind) Capture(src Source, table string) Capture {
	s, err := decodePostgresSettings(src.Settings)
	if err != nil || slices.Contains(s.Keyless, table) {
		return CaptureFull
	}
	if s.CDC != nil && src.Allows(CaptureLog) && !slices.Contains(s.Unpublished, table) {
		return CaptureLog
	}
	if s.CursorColumns[table] != "" && src.Allows(CaptureCursor) {
		return CaptureCursor
	}
	return CaptureFull
}

// split divides followed tables between the log and their cursor columns.
func (k *postgresKind) split(src Source, tables []Table) (logged, cursored []Table) {
	for _, t := range tables {
		switch k.Capture(src, t.Name) {
		case CaptureLog:
			logged = append(logged, t)
		case CaptureCursor:
			cursored = append(cursored, t)
		}
	}
	return logged, cursored
}

func (k *postgresKind) Start(ctx context.Context, src Source, tables []Table) (Start, error) {
	s, err := decodePostgresSettings(src.Settings)
	if err != nil {
		return Start{}, err
	}
	logged, cursored := k.split(src, tables)
	conn, err := k.connect(ctx, s.DatabaseSettings, src.Secret)
	if err != nil {
		return Start{}, connectFailure(err)
	}
	defer conn.Close(context.Background())
	var start Start
	var cur postgresCursor
	if len(logged) > 0 {
		if start, err = k.startLog(ctx, conn, src, s, logged, &cur); err != nil {
			return Start{}, err
		}
	} else if src.ConnectorID != "" {
		// A slot left from when the log was read would hold WAL forever now that nothing reads it.
		if err := dropSlot(ctx, conn, postgresSlot(src.ConnectorID)); err != nil {
			return Start{}, err
		}
	}
	if len(cursored) > 0 {
		if cur.Columns, err = startColumns(ctx, conn, s, cursored); err != nil {
			if start.Close != nil {
				start.Close()
			}
			return Start{}, err
		}
	}
	if start.Cursor, err = json.Marshal(cur); err != nil {
		return Start{}, fmt.Errorf("encode postgres cursor: %w", err)
	}
	return start, nil
}

func (k *postgresKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	s, err := decodePostgresSettings(src.Settings)
	if err != nil {
		return nil, err
	}
	var cur postgresCursor
	if err := json.Unmarshal(cursor, &cur); err != nil {
		return nil, fmt.Errorf("%w: unreadable cursor: %v", ErrCursorExpired, err)
	}
	logged, cursored := k.split(src, tables)
	// CDC turned on or off since the cursor was taken: the full read's Start
	// creates or drops the slot.
	if (len(logged) > 0) != (cur.Slot != "") {
		return nil, ErrCursorExpired
	}
	conn, err := k.connect(ctx, s.DatabaseSettings, src.Secret)
	if err != nil {
		return nil, connectFailure(err)
	}
	defer conn.Close(context.Background())
	if len(logged) > 0 {
		if err := k.readLog(ctx, conn, src, s, logged, &cur, w); err != nil {
			return nil, err
		}
	}
	prev := cur.Columns
	cur.Columns = nil
	if len(cursored) > 0 {
		if cur.Columns, err = readColumns(ctx, conn, s, cursored, prev, w); err != nil {
			return nil, err
		}
	}
	next, err := json.Marshal(cur)
	if err != nil {
		return nil, fmt.Errorf("encode postgres cursor: %w", err)
	}
	return next, nil
}

// postgresTimeBounds is ChangeOverlap before now, as a timestamptz, a
// timestamp and a date. A timestamp without time zone may hold the server's
// local time or UTC, so its bound is the earlier of the two: an overlap read
// twice changes nothing, a skipped change waits for reconcile.
func postgresTimeBounds(ctx context.Context, conn *pgx.Conn) (map[string]*string, error) {
	var tz, local, date string
	err := conn.QueryRow(ctx, `SELECT to_json(now() - d)#>>'{}', to_json(l - d)#>>'{}', to_json((l - d)::date)#>>'{}'
FROM (SELECT make_interval(secs => $1) AS d, LEAST(localtimestamp, now() AT TIME ZONE 'UTC') AS l) b`,
		ChangeOverlap.Seconds()).Scan(&tz, &local, &date)
	if err != nil {
		return nil, fmt.Errorf("read the database clock: %w", err)
	}
	return map[string]*string{"timestamp with time zone": &tz, "timestamp without time zone": &local, "date": &date}, nil
}

func cursorColumnType(t Table, column string) string {
	for _, c := range t.Columns {
		if c.Name == column {
			return c.Type
		}
	}
	return ""
}

func startColumns(ctx context.Context, conn *pgx.Conn, s postgresSettings, tables []Table) (map[string]*string, error) {
	bounds, err := postgresTimeBounds(ctx, conn)
	if err != nil {
		return nil, err
	}
	out := map[string]*string{}
	for _, t := range tables {
		if out[t.Name], err = startColumn(ctx, conn, s, t, bounds); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// startColumn is a cursor-column table's first lower bound: the clock for a
// time column, the highest value for an integer one.
func startColumn(ctx context.Context, conn *pgx.Conn, s postgresSettings, t Table, bounds map[string]*string) (*string, error) {
	column := s.CursorColumns[t.Name]
	typ := cursorColumnType(t, column)
	if !postgresIntegerType(typ) {
		return bounds[typ], nil
	}
	var highest *string
	err := conn.QueryRow(ctx, "SELECT to_json(max(t."+pgx.Identifier{column}.Sanitize()+"))#>>'{}' FROM "+
		pgx.Identifier{s.Schema, t.Name}.Sanitize()+" t").Scan(&highest)
	if err != nil {
		return nil, fmt.Errorf("read the highest %s.%s: %w", t.Name, column, err)
	}
	return highest, nil
}

// readColumns writes every row whose cursor column reached the table's lower
// bound in prev and returns the next bounds. The bounds are taken in the
// read's own transaction, before any row: now() is the transaction's start.
func readColumns(ctx context.Context, conn *pgx.Conn, s postgresSettings, tables []Table, prev map[string]*string, w ChangeWriter) (map[string]*string, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, connectFailure(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // a no-op after Commit
	bounds, err := postgresTimeBounds(ctx, conn)
	if err != nil {
		return nil, err
	}
	next := map[string]*string{}
	for _, t := range tables {
		column := s.CursorColumns[t.Name]
		typ := cursorColumnType(t, column)
		if !postgresCursorTypes[typ] {
			w.Reset(t.Name, "cursor column")
			continue
		}
		lower, known := prev[t.Name]
		if !known {
			w.Reset(t.Name, "new")
			if next[t.Name], err = startColumn(ctx, conn, s, t, bounds); err != nil {
				return nil, err
			}
			continue
		}
		highest, err := readColumn(ctx, conn, s.Schema, t.Name, column, typ, lower, w)
		if err != nil {
			return nil, err
		}
		next[t.Name] = bounds[typ]
		if postgresIntegerType(typ) {
			next[t.Name] = highest
		}
	}
	return next, tx.Commit(ctx)
}

// readColumn writes table's rows at or past lower and returns the highest
// integer cursor value seen, or lower when no row raised it.
func readColumn(ctx context.Context, conn *pgx.Conn, schema, table, column, typ string, lower *string, w ChangeWriter) (*string, error) {
	col := "t." + pgx.Identifier{column}.Sanitize()
	query := "SELECT " + pgRowJSON + ", to_json(" + col + ")#>>'{}' FROM " + pgx.Identifier{schema, table}.Sanitize() + " t"
	var args []any
	if lower != nil {
		query += " WHERE " + col + " >= CAST($1::text AS " + typ + ")"
		args = append(args, *lower)
	}
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read changes to %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	highest := lower
	for rows.Next() {
		var row []byte
		var value *string
		if err := rows.Scan(&row, &value); err != nil {
			return nil, fmt.Errorf("read changes to %s.%s: %w", schema, table, err)
		}
		if err := w.UpsertJSON(table, row); err != nil {
			return nil, err
		}
		if postgresIntegerType(typ) && value != nil && (highest == nil || integerAbove(*value, *highest)) {
			highest = value
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read changes to %s.%s: %w", schema, table, err)
	}
	return highest, nil
}

func integerAbove(a, b string) bool {
	x, errA := strconv.ParseInt(a, 10, 64)
	y, errB := strconv.ParseInt(b, 10, 64)
	return errA == nil && errB == nil && x > y
}

// unindexedCursorColumns warns of each cursor column no index leads with,
// which makes every change read scan its table; fix is the DDL that adds one.
func unindexedCursorColumns(columns map[string]string, indexed map[string]bool, fix func(table, column string) string) []string {
	var warnings []string
	for _, table := range slices.Sorted(maps.Keys(columns)) {
		if column := columns[table]; !indexed[table+"."+column] {
			warnings = append(warnings, fmt.Sprintf("no index starts with cursor column %s.%s, so every change read scans the whole table: %s",
				table, column, fix(table, column)))
		}
	}
	return warnings
}

// checkCursorColumns refuses a cursor column a change read could not filter on.
func checkCursorColumns(d Discovery, columns map[string]string) error {
	tables := make([]string, 0, len(columns))
	for table := range columns {
		tables = append(tables, table)
	}
	slices.Sort(tables)
	for _, table := range tables {
		column := columns[table]
		i := slices.IndexFunc(d.Tables, func(t Table) bool { return t.Name == table })
		if i < 0 {
			return invalid("cursor column %s names table %s, which is not in this schema", column, table)
		}
		switch typ := cursorColumnType(d.Tables[i], column); {
		case typ == "":
			return invalid("table %s has no column %s to use as its cursor column", table, column)
		case !postgresCursorTypes[typ]:
			return invalid("cursor column %s.%s is %s; choose a timestamp, date or integer column", table, column, typ)
		}
	}
	return nil
}
