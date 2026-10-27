package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// postgresChangeLog records a change read, keeping each row's bytes as the kind wrote them.
type postgresChangeLog struct {
	noDocuments
	upserts map[string][]string
	// deletes are each deleted key encoded as JSON.
	deletes map[string][]string
	resets  map[string]string
}

func newPostgresChangeLog() *postgresChangeLog {
	return &postgresChangeLog{upserts: map[string][]string{}, deletes: map[string][]string{}, resets: map[string]string{}}
}

func (c *postgresChangeLog) Upsert(table string, row map[string]any) error {
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return c.UpsertJSON(table, b)
}

func (c *postgresChangeLog) UpsertJSON(table string, row []byte) error {
	c.upserts[table] = append(c.upserts[table], string(row))
	return nil
}

func (c *postgresChangeLog) Delete(table string, key map[string]any) error {
	b, err := json.Marshal(key)
	if err != nil {
		return err
	}
	c.deletes[table] = append(c.deletes[table], string(b))
	return nil
}

func (c *postgresChangeLog) DeleteWhere(table, column string, _ any) error {
	return fmt.Errorf("unexpected DeleteWhere(%s, %s)", table, column)
}

func (c *postgresChangeLog) Reset(table, reason string) { c.resets[table] = reason }

// postgresRawRows collects a full read's rows exactly as PostgreSQL encoded them.
type postgresRawRows struct{ rows []string }

func (r *postgresRawRows) Write(row map[string]any) error {
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return r.WriteJSON(b)
}

func (r *postgresRawRows) WriteJSON(row []byte) error {
	r.rows = append(r.rows, string(row))
	return nil
}

// postgresFullRead is every row of table as a full read writes it.
func postgresFullRead(t *testing.T, src Source, table string) []string {
	t.Helper()
	sink := &postgresRawRows{}
	if _, err := postgresTestKind().Sync(context.Background(), src, Table{Name: table}, sink); err != nil {
		t.Fatal(err)
	}
	return sink.rows
}

func postgresTestKind() *postgresKind { return &postgresKind{guard: Guard{AllowPrivate: true}} }

func postgresTestSettings(schema string) map[string]any {
	return map[string]any{
		"host": testPostgresHost, "port": testPostgresPort, "database": "helix_connector_test",
		"user": "helix", "schema": schema, "disable_tls": true,
	}
}

// postgresTestSource prepares and discovers settings as the syncer does, and
// returns the source a sync runs with and the tables it discovered.
func postgresTestSource(t *testing.T, settings map[string]any, password, connectorID string) (Source, []Table) {
	t.Helper()
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	k := postgresTestKind()
	prepared, err := k.Prepare(Source{Settings: raw, Secret: password})
	if err != nil {
		t.Fatal(err)
	}
	src := Source{Settings: prepared, Secret: password, ConnectorID: connectorID, ForSync: true, Captures: everyCapture}
	d, err := k.Discover(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if d.Settings != nil {
		src.Settings = d.Settings
	}
	return src, d.Tables
}

// postgresFollowed are the tables Capture follows by method.
func postgresFollowed(src Source, tables []Table, method Capture) []Table {
	var out []Table
	for _, t := range tables {
		if postgresTestKind().Capture(src, t.Name) == method {
			out = append(out, t)
		}
	}
	return out
}

func TestPostgresCursorColumns(t *testing.T) {
	ctx := context.Background()
	events := `CREATE TABLE events (id INT PRIMARY KEY, updated_at TIMESTAMPTZ NOT NULL, version BIGINT NOT NULL, body TEXT);
INSERT INTO events VALUES (1, '2020-01-01T00:00:00Z', 5, 'old');`
	followed := func(t *testing.T, column string) (string, Source, []Table) {
		t.Helper()
		schema := postgresFixture(t)
		seedPostgres(t, schema, events)
		settings := postgresTestSettings(schema)
		settings["cursor_columns"] = map[string]string{"events": column}
		src, tables := postgresTestSource(t, settings, "helix", "")
		return schema, src, postgresFollowed(src, tables, CaptureCursor)
	}

	// A Pro sync reads only rows the application stamped since the last read,
	// written with the bytes a full read gives them so unchanged rows merge as no change.
	t.Run("reads rows stamped since the cursor as a full read writes them", func(t *testing.T) {
		schema, src, tables := followed(t, "updated_at")
		if names := tableNames(Discovery{Tables: tables}); !slices.Equal(names, []string{"events"}) {
			t.Fatalf("followed tables = %v", names)
		}
		k := postgresTestKind()
		start, err := k.Start(ctx, src, tables)
		if err != nil {
			t.Fatal(err)
		}
		seedPostgres(t, schema, "INSERT INTO events VALUES (2, now(), 6, 'new'); UPDATE events SET body = 'quiet' WHERE id = 1;")
		log := newPostgresChangeLog()
		if _, err := k.Changes(ctx, src, tables, start.Cursor, log); err != nil {
			t.Fatal(err)
		}
		var fresh string
		for _, row := range postgresFullRead(t, src, "events") {
			if strings.Contains(row, `"id":2`) {
				fresh = row
			}
		}
		if !slices.Equal(log.upserts["events"], []string{fresh}) {
			t.Fatalf("upserts = %v, want only %s", log.upserts["events"], fresh)
		}
	})
	// An integer column has no clock to overlap, so the next read resumes from
	// the highest value read; re-reading that row changes nothing.
	t.Run("resumes an integer cursor from the highest value read", func(t *testing.T) {
		schema, src, tables := followed(t, "version")
		k := postgresTestKind()
		start, err := k.Start(ctx, src, tables)
		if err != nil {
			t.Fatal(err)
		}
		seedPostgres(t, schema, "INSERT INTO events VALUES (2, now(), 7, 'new'), (3, now(), 4, 'stale');")
		log := newPostgresChangeLog()
		next, err := k.Changes(ctx, src, tables, start.Cursor, log)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, row := range log.upserts["events"] {
			var r struct{ ID json.Number }
			_ = json.Unmarshal([]byte(row), &r)
			ids = append(ids, r.ID.String())
		}
		slices.Sort(ids)
		if !slices.Equal(ids, []string{"1", "2"}) {
			t.Fatalf("read ids = %v, want 1 (at the cursor) and 2 (past it)", ids)
		}
		var cur postgresCursor
		if err := json.Unmarshal(next, &cur); err != nil || cur.Columns["events"] == nil || *cur.Columns["events"] != "7" {
			t.Fatalf("next cursor = %s", next)
		}
	})
	// A table the cursor does not know yet was not followed before, so its
	// snapshot may miss changes; it is read in full once.
	t.Run("reads a newly followed table in full", func(t *testing.T) {
		_, src, tables := followed(t, "updated_at")
		log := newPostgresChangeLog()
		next, err := postgresTestKind().Changes(ctx, src, tables, json.RawMessage(`{"columns":{}}`), log)
		if err != nil {
			t.Fatal(err)
		}
		var cur postgresCursor
		if _, reset := log.resets["events"]; !reset || json.Unmarshal(next, &cur) != nil || cur.Columns["events"] == nil {
			t.Fatalf("resets = %v, next = %s", log.resets, next)
		}
	})
	// Test refuses a column a change read can't filter on, so a typo fails at setup.
	for _, c := range []struct{ column, want string }{
		{"changed_at", "has no column changed_at"},
		{"body", "is text"},
	} {
		t.Run("refuses cursor column "+c.column, func(t *testing.T) {
			schema := postgresFixture(t)
			seedPostgres(t, schema, events)
			settings := postgresTestSettings(schema)
			settings["cursor_columns"] = map[string]string{"events": c.column}
			_, err := discoverDatabase(t, kindOf(t, NewCatalog(DefaultEndpoints(), Guard{AllowPrivate: true}, S3Options{}, EventsOptions{}), "postgres"), settings, "helix")
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) || !strings.Contains(invalidErr.Message, c.want) {
				t.Fatalf("Discover error = %v, want %q", err, c.want)
			}
		})
	}
	// Without an index each change read scans the table, so Test shows the DDL that adds one.
	t.Run("warns of a cursor column no index starts with", func(t *testing.T) {
		schema := postgresFixture(t)
		seedPostgres(t, schema, events)
		settings := postgresTestSettings(schema)
		settings["cursor_columns"] = map[string]string{"events": "updated_at"}
		k := kindOf(t, NewCatalog(DefaultEndpoints(), Guard{AllowPrivate: true}, S3Options{}, EventsOptions{}), "postgres")
		d, err := discoverDatabase(t, k, settings, "helix")
		if err != nil || len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "events.updated_at") {
			t.Fatalf("Discover = (%v, %v), want one warning naming events.updated_at", d.Warnings, err)
		}
		seedPostgres(t, schema, d.Warnings[0][strings.Index(d.Warnings[0], "CREATE INDEX"):])
		if d, err = discoverDatabase(t, k, settings, "helix"); err != nil || len(d.Warnings) != 0 {
			t.Fatalf("Discover after the suggested index = (%v, %v), want no warnings", d.Warnings, err)
		}
		settings["cursor_columns"] = map[string]string{"events": "id"}
		if d, err = discoverDatabase(t, k, settings, "helix"); err != nil || len(d.Warnings) != 0 {
			t.Fatalf("Discover with the primary key = (%v, %v), want no warnings", d.Warnings, err)
		}
	})
	// Merging needs a key; a keyless table can't be followed whatever its columns.
	t.Run("reads a table without a primary key in full", func(t *testing.T) {
		schema := postgresFixture(t)
		seedPostgres(t, schema, "CREATE TABLE audit (at TIMESTAMPTZ NOT NULL)")
		settings := postgresTestSettings(schema)
		settings["cursor_columns"] = map[string]string{"audit": "at"}
		src, _ := postgresTestSource(t, settings, "helix", "")
		if got := postgresTestKind().Capture(src, "audit"); got != CaptureFull {
			t.Fatalf("Capture(audit) = %s, want full", got)
		}
	})
}

// postgresTestConn is a connection to the test database outside any schema.
func postgresTestConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), fmt.Sprintf("postgres://helix:helix@%s:%d/helix_connector_test?sslmode=disable",
		testPostgresHost, testPostgresPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}
