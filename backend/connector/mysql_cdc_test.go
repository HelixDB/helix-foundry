package connector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// mysqlRoot runs statements on the test server as root, outside any database.
func mysqlRoot(t *testing.T, statements string) {
	t.Helper()
	db, err := sql.Open("mysql", "root:helix@tcp("+testMySQLAddr+")/?multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statements); err != nil {
		t.Fatal(err)
	}
}

// With GTIDs the source streams from the start of a binlog file and skips the
// transactions the cursor's set holds, sending heartbeats as it goes; the
// read must carry on past those to the transactions after the cursor.
func TestMySQLBinlogGTID(t *testing.T) {
	ctx := context.Background()
	root, err := sql.Open("mysql", "root:helix@tcp("+testMySQLGTIDAddr+")/?multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	database := fmt.Sprintf("gtid_%d", time.Now().UnixNano())
	if _, err := root.Exec("CREATE DATABASE " + database + "; USE " + database + `;
CREATE TABLE customers (id INT PRIMARY KEY, email VARCHAR(100));
INSERT INTO customers VALUES (1, 'a@example.com'), (2, 'b@example.com');`); err != nil {
		t.Fatalf("create database (is make start-testdb running?): %v", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE " + database) })
	kind := kindOf(t, NewCatalog(DefaultEndpoints(), Guard{AllowPrivate: true}, S3Options{}, EventsOptions{}), "mysql")
	raw, err := json.Marshal(map[string]any{"host": "127.0.0.1", "port": 3309, "database": database, "user": "root", "disable_tls": true,
		"cdc": map[string]any{"enabled": true}})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := kind.Prepare(Source{Settings: raw, Secret: "helix"})
	if err != nil {
		t.Fatal(err)
	}
	src := Source{Settings: prepared, Secret: "helix", ConnectorID: "gtid-" + database, Captures: everyCapture}
	reader := kind.(ChangeReader)
	tables := []Table{{Name: "customers", PrimaryKey: []string{"id"}}}
	start, err := reader.Start(ctx, src, tables)
	if err != nil {
		t.Fatal(err)
	}
	var cursor mysqlBinlogCursor
	if err := json.Unmarshal(start.Cursor, &cursor); err != nil || cursor.GTID == "" {
		t.Fatalf("cursor = %s, want a GTID set", start.Cursor)
	}
	if _, err := root.Exec("USE " + database + `;
INSERT INTO customers VALUES (3, 'c@example.com');
UPDATE customers SET email = 'A@example.com' WHERE id = 1;
DELETE FROM customers WHERE id = 2;`); err != nil {
		t.Fatal(err)
	}
	log := &mysqlChangeLog{}
	next, err := reader.Changes(ctx, src, tables, start.Cursor, log)
	if err != nil {
		t.Fatal(err)
	}
	if got := log.upserts(t, "customers", "id"); len(got) != 2 || got["[1]"] == "" || got["[3]"] == "" {
		t.Fatalf("upserts = %v", got)
	}
	if got := log.deletes(t, "customers", "id"); len(got) != 1 || got[0] != "[2]" {
		t.Fatalf("deletes = %v", got)
	}
	// Nothing after the cursor: the read ends on the heartbeat at the target, not the idle backstop.
	began := time.Now()
	idle := &mysqlChangeLog{}
	if _, err := reader.Changes(ctx, src, tables, next, idle); err != nil || len(idle.changes) != 0 {
		t.Fatalf("idle read = %d changes, %v", len(idle.changes), err)
	}
	if took := time.Since(began); took > 30*time.Second {
		t.Fatalf("idle read took %s", took)
	}
}

func TestMySQLBinlogChanges(t *testing.T) {
	ctx := context.Background()
	kind := kindOf(t, NewCatalog(DefaultEndpoints(), Guard{AllowPrivate: true}, S3Options{}, EventsOptions{}), "mysql")
	reader := kind.(ChangeReader)
	customers := Table{Name: "customers", PrimaryKey: []string{"id"}}
	cdc := func(t *testing.T, database string) Source {
		return mysqlSource(t, kind, map[string]any{"database": database, "cdc": map[string]any{"enabled": true}},
			fmt.Sprintf("conn_%d", time.Now().UnixNano()))
	}

	// The copy starts after the position with no lock, so a write in between is
	// in both; the change read must leave the copy equal to the database.
	t.Run("converges on a write between the position and the copy", func(t *testing.T) {
		database := mysqlFixture(t)
		src := cdc(t, database)
		seedMySQL(t, database, `INSERT INTO customers VALUES (1, 'a@example.com'), (2, 'b@example.com')`)
		start, err := reader.Start(ctx, src, []Table{customers})
		if err != nil {
			t.Fatal(err)
		}
		seedMySQL(t, database, `INSERT INTO customers VALUES (3, 'c@example.com'); UPDATE customers SET email = 'a2@example.com' WHERE id = 1`)
		copied := mysqlFullRead(t, kind, src, "customers", "id")
		if len(copied) != 3 {
			t.Fatalf("copy has %d rows, want the write after the position in it", len(copied))
		}
		log := &mysqlChangeLog{}
		if _, err := reader.Changes(ctx, src, []Table{customers}, start.Cursor, log); err != nil {
			t.Fatal(err)
		}
		if got, want := log.merge(t, "customers", copied, "id"), mysqlFullRead(t, kind, src, "customers", "id"); !maps.Equal(got, want) {
			t.Fatalf("merged = %v, want %v", got, want)
		}
		for key, row := range log.upserts(t, "customers", "id") {
			if row != copied[key] {
				t.Fatalf("upsert %s = %s, copy has %s; a replayed row must change nothing", key, row, copied[key])
			}
		}
	})
	// The merge compares rows by their bytes, so every change must arrive as a
	// full read encodes it, and a delete must carry the key the snapshot holds.
	t.Run("delivers inserts, updates and deletes as a full read encodes them", func(t *testing.T) {
		database := mysqlFixture(t)
		src := cdc(t, database)
		orders := Table{Name: "orders", PrimaryKey: []string{"id"}}
		seedMySQL(t, database, `INSERT INTO customers VALUES (1, 'a@example.com');
INSERT INTO orders VALUES (10, 1, 5.00), (11, 1, 6.50)`)
		start, err := reader.Start(ctx, src, []Table{customers, orders})
		if err != nil {
			t.Fatal(err)
		}
		copied := mysqlFullRead(t, kind, src, "orders", "id")
		seedMySQL(t, database, `INSERT INTO orders VALUES (12, 1, 7.25); UPDATE orders SET total = 9.90 WHERE id = 10; DELETE FROM orders WHERE id = 11`)
		log := &mysqlChangeLog{}
		next, err := reader.Changes(ctx, src, []Table{customers, orders}, start.Cursor, log)
		if err != nil {
			t.Fatal(err)
		}
		full := mysqlFullRead(t, kind, src, "orders", "id")
		upserts := log.upserts(t, "orders", "id")
		if !slices.Equal(slices.Sorted(maps.Keys(upserts)), []string{"[10]", "[12]"}) {
			t.Fatalf("upserted keys = %v, want [10] and [12]", slices.Sorted(maps.Keys(upserts)))
		}
		for key, row := range upserts {
			if row != full[key] {
				t.Fatalf("upsert %s = %s, full read has %s", key, row, full[key])
			}
		}
		if got := log.deletes(t, "orders", "id"); !slices.Equal(got, []string{"[11]"}) {
			t.Fatalf("deleted keys = %v, want [11]", got)
		}
		if got := log.merge(t, "orders", copied, "id"); !maps.Equal(got, full) {
			t.Fatalf("merged = %v, want %v", got, full)
		}
		var cursor struct {
			CDC struct {
				Position string `json:"position"`
			} `json:"cdc"`
		}
		if err := json.Unmarshal(next, &cursor); err != nil || cursor.CDC.Position == "" {
			t.Fatalf("next cursor %s has no position for the CDC status", next)
		}
	})
	// A row whose key changed is gone under its old key and present under the new one.
	t.Run("deletes the old key when a key changes", func(t *testing.T) {
		database := mysqlFixture(t)
		src := cdc(t, database)
		seedMySQL(t, database, `INSERT INTO customers VALUES (1, 'a@example.com')`)
		start, err := reader.Start(ctx, src, []Table{customers})
		if err != nil {
			t.Fatal(err)
		}
		seedMySQL(t, database, `UPDATE customers SET id = 7 WHERE id = 1`)
		log := &mysqlChangeLog{}
		if _, err := reader.Changes(ctx, src, []Table{customers}, start.Cursor, log); err != nil {
			t.Fatal(err)
		}
		if got := log.deletes(t, "customers", "id"); !slices.Equal(got, []string{"[1]"}) {
			t.Fatalf("deleted keys = %v, want [1]", got)
		}
		if got := slices.Sorted(maps.Keys(log.upserts(t, "customers", "id"))); !slices.Equal(got, []string{"[7]"}) {
			t.Fatalf("upserted keys = %v, want [7]", got)
		}
	})
	// A delete carries the key as the snapshot holds it, so binary keys must be
	// padded and encoded as JSON_OBJECT does, and composite keys kept whole.
	t.Run("deletes binary and composite keys by the full read's values", func(t *testing.T) {
		database := mysqlFixture(t)
		src := cdc(t, database)
		seedMySQL(t, database, `CREATE TABLE tokens (id BINARY(4) PRIMARY KEY, note VARCHAR(10));
CREATE TABLE members (team INT UNSIGNED, login VARCHAR(20), PRIMARY KEY (team, login));
INSERT INTO tokens VALUES (UNHEX('0A0B0000'), 'x'), (UNHEX('01020304'), 'y');
INSERT INTO members VALUES (4294967295, 'ann'), (1, 'bob');`)
		tokens := Table{Name: "tokens", PrimaryKey: []string{"id"}}
		members := Table{Name: "members", PrimaryKey: []string{"team", "login"}}
		start, err := reader.Start(ctx, src, []Table{tokens, members})
		if err != nil {
			t.Fatal(err)
		}
		tokensBefore := mysqlFullRead(t, kind, src, "tokens", "id")
		membersBefore := mysqlFullRead(t, kind, src, "members", "team", "login")
		seedMySQL(t, database, `DELETE FROM tokens WHERE id = UNHEX('0A0B0000'); DELETE FROM members WHERE login = 'ann';
UPDATE tokens SET note = 'z' WHERE id = UNHEX('01020304')`)
		log := &mysqlChangeLog{}
		if _, err := reader.Changes(ctx, src, []Table{tokens, members}, start.Cursor, log); err != nil {
			t.Fatal(err)
		}
		if got, want := log.merge(t, "tokens", tokensBefore, "id"), mysqlFullRead(t, kind, src, "tokens", "id"); !maps.Equal(got, want) {
			t.Fatalf("tokens merged = %v, want %v", got, want)
		}
		if got, want := log.merge(t, "members", membersBefore, "team", "login"), mysqlFullRead(t, kind, src, "members", "team", "login"); !maps.Equal(got, want) {
			t.Fatalf("members merged = %v, want %v", got, want)
		}
	})
	// Row events carry no DDL, and an ALTER can change what the rows mean, so
	// the table is read in full and the status says why.
	t.Run("reads a table in full after a statement changes it", func(t *testing.T) {
		database := mysqlFixture(t)
		src := cdc(t, database)
		start, err := reader.Start(ctx, src, []Table{customers})
		if err != nil {
			t.Fatal(err)
		}
		seedMySQL(t, database, `ALTER TABLE customers ADD COLUMN plan VARCHAR(20); INSERT INTO customers VALUES (5, 'e@example.com', 'pro')`)
		log := &mysqlChangeLog{}
		next, err := reader.Changes(ctx, src, []Table{customers}, start.Cursor, log)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := log.resets["customers"]; !ok {
			t.Fatalf("resets = %v, want customers read in full", log.resets)
		}
		var cursor mysqlBinlogCursor
		if err := json.Unmarshal(next, &cursor); err != nil || !strings.Contains(cursor.CDC.LastReset, "customers") || cursor.CDC.LastResetAt == nil {
			t.Fatalf("cdc status = %+v, want the reset of customers", cursor.CDC)
		}
	})
	// A change merges by key; a table without one can only be read whole.
	t.Run("reads a keyless table in full", func(t *testing.T) {
		database := mysqlFixture(t)
		src := cdc(t, database)
		notes := Table{Name: "notes"}
		start, err := reader.Start(ctx, src, []Table{notes})
		if err != nil {
			t.Fatal(err)
		}
		log := &mysqlChangeLog{}
		if _, err := reader.Changes(ctx, src, []Table{notes}, start.Cursor, log); err != nil {
			t.Fatal(err)
		}
		if _, ok := log.resets["notes"]; !ok {
			t.Fatalf("resets = %v, want notes read in full", log.resets)
		}
	})
	// The syncer commits the cursor only after every table is in; a crash
	// replays the batch, which must land on the same rows.
	t.Run("lands on the same rows when a cursor is replayed", func(t *testing.T) {
		database := mysqlFixture(t)
		src := cdc(t, database)
		seedMySQL(t, database, `INSERT INTO customers VALUES (1, 'a@example.com')`)
		start, err := reader.Start(ctx, src, []Table{customers})
		if err != nil {
			t.Fatal(err)
		}
		copied := mysqlFullRead(t, kind, src, "customers", "id")
		seedMySQL(t, database, `INSERT INTO customers VALUES (2, 'b@example.com'); DELETE FROM customers WHERE id = 1`)
		first, second := &mysqlChangeLog{}, &mysqlChangeLog{}
		if _, err := reader.Changes(ctx, src, []Table{customers}, start.Cursor, first); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.Changes(ctx, src, []Table{customers}, start.Cursor, second); err != nil {
			t.Fatal(err)
		}
		once := first.merge(t, "customers", copied, "id")
		if twice := second.merge(t, "customers", once, "id"); !maps.Equal(once, twice) {
			t.Fatalf("replay changed the rows: %v, then %v", once, twice)
		}
	})
	// The source purges old binlog files; a cursor in one must make the sync
	// copy again. A file older than any the server lists is refused as a purged one is.
	t.Run("expires a cursor whose binlog file is gone", func(t *testing.T) {
		database := mysqlFixture(t)
		src := cdc(t, database)
		start, err := reader.Start(ctx, src, []Table{customers})
		if err != nil {
			t.Fatal(err)
		}
		var cursor mysqlBinlogCursor
		if err := json.Unmarshal(start.Cursor, &cursor); err != nil {
			t.Fatal(err)
		}
		base, _, _ := mysqlBinlogFile(cursor.File)
		cursor.File = base + ".000000"
		seedMySQL(t, database, `INSERT INTO customers VALUES (1, 'a@example.com')`)
		before := testutil.ToFloat64(cdcResets.WithLabelValues("mysql", "purged"))
		_, err = reader.Changes(ctx, src, []Table{customers}, mustJSON(t, cursor), &mysqlChangeLog{})
		var lost *PositionLostError
		if !errors.Is(err, ErrCursorExpired) || !errors.As(err, &lost) || !strings.Contains(lost.Reason, "purged") {
			t.Fatalf("Changes error = %v, want a lost position", err)
		}
		if got := testutil.ToFloat64(cdcResets.WithLabelValues("mysql", "purged")) - before; got != 1 {
			t.Fatalf("purged resets counted = %v, want 1", got)
		}
	})
}

func TestMySQLServerID(t *testing.T) {
	// Replicas of one source must not share an id: the server keeps one
	// registration per id, and an id equal to its own stops the stream.
	t.Run("gives two connectors different ids in the top half", func(t *testing.T) {
		a, b := mysqlServerID(DatabaseSettings{}, "conn_a"), mysqlServerID(DatabaseSettings{}, "conn_b")
		if a == b || a < 1<<31 || b < 1<<31 {
			t.Fatalf("server ids = %d and %d, want two distinct ids of at least 2^31", a, b)
		}
	})
	// A customer whose own replicas sit in the derived range can choose the id.
	t.Run("uses the id the settings name", func(t *testing.T) {
		s := DatabaseSettings{CDC: &DatabaseCDC{Enabled: true, ServerID: 4242}}
		if got := mysqlServerID(s, "conn_a"); got != 4242 {
			t.Fatalf("server id = %d, want 4242", got)
		}
	})
}

func TestMySQLBinlogDiscover(t *testing.T) {
	kind := kindOf(t, NewCatalog(DefaultEndpoints(), Guard{AllowPrivate: true}, S3Options{}, EventsOptions{}), "mysql")
	settings := func(database, user string) map[string]any {
		return map[string]any{"host": "127.0.0.1", "port": 3307, "database": database, "user": user, "disable_tls": true,
			"cdc": map[string]any{"enabled": true}}
	}

	// The test server keeps a full-row binlog for 30 days and root may replicate.
	t.Run("passes a server whose binlog a change read can follow", func(t *testing.T) {
		if _, err := discoverDatabase(t, kind, settings(mysqlFixture(t), "root"), "helix"); err != nil {
			t.Fatal(err)
		}
	})
	// A minimal row image leaves columns out of the rows. The setting is global
	// on a shared server, so it is restored as soon as Test has read it.
	t.Run("refuses a minimal row image", func(t *testing.T) {
		database := mysqlFixture(t)
		mysqlRoot(t, "SET GLOBAL binlog_row_image = 'MINIMAL'")
		t.Cleanup(func() { mysqlRoot(t, "SET GLOBAL binlog_row_image = 'FULL'") })
		_, err := discoverDatabase(t, kind, settings(database, "root"), "helix")
		mysqlRoot(t, "SET GLOBAL binlog_row_image = 'FULL'")
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || !strings.Contains(refusedErr.Message, "binlog_row_image") {
			t.Fatalf("Discover error = %v, want binlog_row_image named", err)
		}
	})
	// Reading the position needs REPLICATION CLIENT and streaming needs
	// REPLICATION SLAVE; missing either, Test names the grants.
	for _, grants := range []string{"", "REPLICATION CLIENT"} {
		t.Run(fmt.Sprintf("refuses a user granted only %q", grants), func(t *testing.T) {
			database := mysqlFixture(t)
			user := "cdc_" + strings.TrimPrefix(database, "connector_test_")
			statements := fmt.Sprintf("CREATE USER '%[1]s'@'%%' IDENTIFIED BY 'reader'; GRANT SELECT ON %[2]s.* TO '%[1]s'@'%%';", user, database)
			if grants != "" {
				statements += fmt.Sprintf("GRANT %s ON *.* TO '%s'@'%%';", grants, user)
			}
			mysqlRoot(t, statements)
			t.Cleanup(func() { mysqlRoot(t, fmt.Sprintf("DROP USER '%s'@'%%'", user)) })
			_, err := discoverDatabase(t, kind, settings(database, user), "reader")
			var refusedErr *RefusedError
			if !errors.As(err, &refusedErr) || !strings.Contains(refusedErr.Message, "GRANT REPLICATION SLAVE, REPLICATION CLIENT") {
				t.Fatalf("Discover error = %v, want the grants named", err)
			}
		})
	}
}
