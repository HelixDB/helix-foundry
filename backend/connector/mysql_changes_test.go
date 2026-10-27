package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// mysqlChangeLog is a ChangeWriter that records one change read's writes in order.
type mysqlChangeLog struct {
	noDocuments
	changes []mysqlLogged
	resets  map[string]string
}

// mysqlLogged is one write: an upsert's row, or a delete's key.
type mysqlLogged struct {
	table string
	row   []byte
	key   map[string]any
}

func (l *mysqlChangeLog) Upsert(table string, row map[string]any) error {
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return l.UpsertJSON(table, b)
}

func (l *mysqlChangeLog) UpsertJSON(table string, row []byte) error {
	l.changes = append(l.changes, mysqlLogged{table: table, row: slices.Clone(row)})
	return nil
}

func (l *mysqlChangeLog) Delete(table string, key map[string]any) error {
	l.changes = append(l.changes, mysqlLogged{table: table, key: key})
	return nil
}

func (l *mysqlChangeLog) DeleteWhere(table, column string, _ any) error {
	return fmt.Errorf("unexpected cascade on %s.%s", table, column)
}

func (l *mysqlChangeLog) Reset(table, reason string) {
	if l.resets == nil {
		l.resets = map[string]string{}
	}
	l.resets[table] = reason
}

// upserts are the rows written to table, by key.
func (l *mysqlChangeLog) upserts(t *testing.T, table string, key ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, c := range l.changes {
		if c.table == table && c.row != nil {
			out[mysqlKeyOf(t, c.row, key)] = string(c.row)
		}
	}
	return out
}

// deletes are the keys deleted from table.
func (l *mysqlChangeLog) deletes(t *testing.T, table string, key ...string) []string {
	t.Helper()
	var out []string
	for _, c := range l.changes {
		if c.table == table && c.row == nil {
			out = append(out, mysqlKeyOf(t, mustJSON(t, c.key), key))
		}
	}
	slices.Sort(out)
	return out
}

// merge applies the log to rows as the syncer's merge does: by key, the last change winning.
func (l *mysqlChangeLog) merge(t *testing.T, table string, rows map[string]string, key ...string) map[string]string {
	t.Helper()
	out := maps.Clone(rows)
	for _, c := range l.changes {
		switch {
		case c.table != table:
		case c.row != nil:
			out[mysqlKeyOf(t, c.row, key)] = string(c.row)
		default:
			delete(out, mysqlKeyOf(t, mustJSON(t, c.key), key))
		}
	}
	return out
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mysqlKeyOf is a row's key values as a JSON array, the way a delete's key encodes them.
func mysqlKeyOf(t *testing.T, row []byte, key []string) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(row))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		t.Fatalf("decode %s: %v", row, err)
	}
	values := make([]any, len(key))
	for i, k := range key {
		values[i] = fields[k]
	}
	return string(mustJSON(t, values))
}

// mysqlRawRows keeps a full read's rows as MySQL encoded them.
type mysqlRawRows struct{ rows [][]byte }

func (r *mysqlRawRows) Write(map[string]any) error { return errors.New("mysql rows arrive as JSON") }

func (r *mysqlRawRows) WriteJSON(row []byte) error {
	r.rows = append(r.rows, slices.Clone(row))
	return nil
}

// mysqlFullRead reads table as a full sync does, by key.
func mysqlFullRead(t *testing.T, kind Kind, src Source, table string, key ...string) map[string]string {
	t.Helper()
	w := &mysqlRawRows{}
	if _, err := kind.Sync(context.Background(), src, Table{Name: table}, w); err != nil {
		t.Fatalf("full read of %s: %v", table, err)
	}
	out := map[string]string{}
	for _, row := range w.rows {
		out[mysqlKeyOf(t, row, key)] = string(row)
	}
	return out
}

// mysqlSource prepares settings for the test database's root user.
func mysqlSource(t *testing.T, kind Kind, settings map[string]any, connectorID string) Source {
	t.Helper()
	settings = maps.Clone(settings)
	settings["host"], settings["port"], settings["user"], settings["disable_tls"] = "127.0.0.1", 3307, "root", true
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := kind.Prepare(Source{Settings: raw, Secret: "helix"})
	if err != nil {
		t.Fatal(err)
	}
	return Source{Settings: prepared, Secret: "helix", ConnectorID: connectorID, Captures: everyCapture}
}

func TestMySQLCapture(t *testing.T) {
	kind := kindOf(t, NewCatalog(DefaultEndpoints(), Guard{AllowPrivate: true}, S3Options{}, EventsOptions{}), "mysql").(ChangeReader)
	capture := func(settings string, table string) Capture {
		return kind.Capture(Source{Settings: json.RawMessage(settings), Captures: everyCapture}, table)
	}

	// The binlog carries every table, so CDC follows each one, cursor column or not.
	t.Run("follows every table through the binlog when cdc is on", func(t *testing.T) {
		if got := capture(`{"cdc":{"enabled":true},"cursor_columns":{"orders":"updated_at"}}`, "customers"); got != CaptureLog {
			t.Fatalf("Capture = %s, want log", got)
		}
	})
	// Without CDC only a table whose application keeps a cursor column can be followed.
	t.Run("follows a table by its cursor column", func(t *testing.T) {
		settings := `{"cursor_columns":{"orders":"updated_at"}}`
		if got := capture(settings, "orders"); got != CaptureCursor {
			t.Fatalf("Capture(orders) = %s, want cursor", got)
		}
		if got := capture(settings, "customers"); got != CaptureFull {
			t.Fatalf("Capture(customers) = %s, want full", got)
		}
	})
	// A downgrade keeps the CDC setting, so the plan, not the settings, decides.
	t.Run("follows the cursor column when the plan leaves out the log", func(t *testing.T) {
		src := Source{Settings: json.RawMessage(`{"cdc":{"enabled":true},"cursor_columns":{"orders":"updated_at"}}`),
			Captures: []Capture{CaptureFull, CaptureCursor, CaptureEvents}}
		if got := kind.Capture(src, "orders"); got != CaptureCursor {
			t.Fatalf("Capture(orders) = %s, want cursor", got)
		}
		if got := kind.Capture(src, "customers"); got != CaptureFull {
			t.Fatalf("Capture(customers) = %s, want full", got)
		}
	})
}

func TestMySQLCursorColumns(t *testing.T) {
	ctx := context.Background()
	kind := kindOf(t, NewCatalog(DefaultEndpoints(), Guard{AllowPrivate: true}, S3Options{}, EventsOptions{}), "mysql")
	reader := kind.(ChangeReader)
	events := Table{Name: "events", PrimaryKey: []string{"id"}}
	const eventsTable = `CREATE TABLE events (id INT PRIMARY KEY, name VARCHAR(50), amount DECIMAL(10,2),
  version BIGINT UNSIGNED NOT NULL, updated_at DATETIME(6) NOT NULL);
INSERT INTO events VALUES (1, 'first', 1.00, 1, NOW(6) - INTERVAL 1 DAY), (2, 'second', 2.00, 2, NOW(6) - INTERVAL 1 DAY);`

	// The merge compares rows by their bytes, so a changed row must arrive exactly as a full read encodes it.
	t.Run("reads rows past a time cursor as a full read encodes them", func(t *testing.T) {
		database := mysqlFixture(t)
		seedMySQL(t, database, eventsTable)
		src := mysqlSource(t, kind, map[string]any{"database": database, "cursor_columns": map[string]string{"events": "updated_at"}}, "")
		start, err := reader.Start(ctx, src, []Table{events})
		if err != nil {
			t.Fatal(err)
		}
		seedMySQL(t, database, `UPDATE events SET name = 'renamed', updated_at = NOW(6) WHERE id = 1;
INSERT INTO events VALUES (3, 'third', 3.50, 3, NOW(6));`)
		log := &mysqlChangeLog{}
		if _, err := reader.Changes(ctx, src, []Table{events}, start.Cursor, log); err != nil {
			t.Fatal(err)
		}
		full := mysqlFullRead(t, kind, src, "events", "id")
		got := log.upserts(t, "events", "id")
		if !slices.Equal(slices.Sorted(maps.Keys(got)), []string{"[1]", "[3]"}) {
			t.Fatalf("upserted keys = %v, want [1] and [3]", slices.Sorted(maps.Keys(got)))
		}
		for key, row := range got {
			if row != full[key] {
				t.Fatalf("upsert %s = %s, full read has %s", key, row, full[key])
			}
		}
	})
	// A version or sequence column moves only forward, so the cursor is the highest value read, with no overlap.
	t.Run("reads rows above an integer cursor", func(t *testing.T) {
		database := mysqlFixture(t)
		seedMySQL(t, database, eventsTable)
		src := mysqlSource(t, kind, map[string]any{"database": database, "cursor_columns": map[string]string{"events": "version"}}, "")
		start, err := reader.Start(ctx, src, []Table{events})
		if err != nil {
			t.Fatal(err)
		}
		seedMySQL(t, database, `UPDATE events SET name = 'bumped', version = 18446744073709551000 WHERE id = 2`)
		log := &mysqlChangeLog{}
		next, err := reader.Changes(ctx, src, []Table{events}, start.Cursor, log)
		if err != nil {
			t.Fatal(err)
		}
		if got := slices.Sorted(maps.Keys(log.upserts(t, "events", "id"))); !slices.Equal(got, []string{"[2]"}) {
			t.Fatalf("upserted keys = %v, want [2]", got)
		}
		again := &mysqlChangeLog{}
		if _, err := reader.Changes(ctx, src, []Table{events}, next, again); err != nil {
			t.Fatal(err)
		}
		if len(again.changes) != 0 {
			t.Fatalf("a read past the highest version wrote %d changes", len(again.changes))
		}
	})
	// The old cursor holds another column's values, which say nothing about the new one.
	t.Run("reads a table in full when its cursor column changed", func(t *testing.T) {
		database := mysqlFixture(t)
		seedMySQL(t, database, eventsTable)
		before := mysqlSource(t, kind, map[string]any{"database": database, "cursor_columns": map[string]string{"events": "version"}}, "")
		start, err := reader.Start(ctx, before, []Table{events})
		if err != nil {
			t.Fatal(err)
		}
		after := mysqlSource(t, kind, map[string]any{"database": database, "cursor_columns": map[string]string{"events": "updated_at"}}, "")
		log := &mysqlChangeLog{}
		if _, err := reader.Changes(ctx, after, []Table{events}, start.Cursor, log); err != nil {
			t.Fatal(err)
		}
		if _, ok := log.resets["events"]; !ok || len(log.changes) != 0 {
			t.Fatalf("resets = %v, changes = %d; want events read in full", log.resets, len(log.changes))
		}
	})
	// A change merges into the snapshot by key; without one the table can only be read whole.
	t.Run("reads a keyless table in full", func(t *testing.T) {
		database := mysqlFixture(t)
		seedMySQL(t, database, `CREATE TABLE audit (body TEXT, at DATETIME(6) NOT NULL)`)
		src := mysqlSource(t, kind, map[string]any{"database": database, "cursor_columns": map[string]string{"audit": "at"}}, "")
		audit := Table{Name: "audit"}
		start, err := reader.Start(ctx, src, []Table{audit})
		if err != nil {
			t.Fatal(err)
		}
		log := &mysqlChangeLog{}
		if _, err := reader.Changes(ctx, src, []Table{audit}, start.Cursor, log); err != nil {
			t.Fatal(err)
		}
		if _, ok := log.resets["audit"]; !ok {
			t.Fatalf("resets = %v, want audit read in full", log.resets)
		}
	})
	// Test checks the column, so a typo or a text column fails at setup rather than at the first sync.
	for _, column := range []string{"email", "updated_at"} {
		t.Run("rejects cursor column "+column, func(t *testing.T) {
			settings := map[string]any{"host": "127.0.0.1", "port": 3307, "database": mysqlFixture(t), "user": "root", "disable_tls": true,
				"cursor_columns": map[string]string{"customers": column}}
			_, err := discoverDatabase(t, kind, settings, "helix")
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) || !strings.Contains(invalidErr.Message, column) {
				t.Fatalf("Discover error = %v, want the column named", err)
			}
		})
	}
	// Without an index each change read scans the table, so Test shows the DDL that adds one.
	t.Run("warns of a cursor column no index starts with", func(t *testing.T) {
		database := mysqlFixture(t)
		seedMySQL(t, database, eventsTable)
		settings := map[string]any{"host": "127.0.0.1", "port": 3307, "database": database, "user": "root", "disable_tls": true,
			"cursor_columns": map[string]string{"events": "updated_at"}}
		d, err := discoverDatabase(t, kind, settings, "helix")
		if err != nil || len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "events.updated_at") {
			t.Fatalf("Discover = (%v, %v), want one warning naming events.updated_at", d.Warnings, err)
		}
		seedMySQL(t, database, d.Warnings[0][strings.Index(d.Warnings[0], "CREATE INDEX"):])
		if d, err = discoverDatabase(t, kind, settings, "helix"); err != nil || len(d.Warnings) != 0 {
			t.Fatalf("Discover after the suggested index = (%v, %v), want no warnings", d.Warnings, err)
		}
	})
}
