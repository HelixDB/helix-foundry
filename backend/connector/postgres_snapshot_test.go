package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// generations are two tables, each at generation 1; nextGeneration moves
// both to 2 in one transaction.
const (
	generations = `CREATE TABLE a (id INT PRIMARY KEY, generation INT NOT NULL);
CREATE TABLE b (id INT PRIMARY KEY, generation INT NOT NULL);
INSERT INTO a VALUES (1, 1); INSERT INTO b VALUES (1, 1);`
	nextGeneration = `BEGIN; UPDATE a SET generation = 2; UPDATE b SET generation = 2; COMMIT;`
)

func generation(n int) []string { return []string{fmt.Sprintf(`{"id":1,"generation":%d}`, n)} }

// postgresHolding is the pids of role's client sessions idle in a
// transaction: a snapshot held open between reads.
func postgresHolding(t *testing.T, role string) []int32 {
	t.Helper()
	var pids []int32
	if err := pgRows(context.Background(), postgresTestConn(t), `SELECT pid FROM pg_stat_activity
WHERE usename = $1 AND backend_type = 'client backend' AND state LIKE 'idle in transaction%'`, []any{role}, func(r pgx.Rows) error {
		var pid int32
		err := r.Scan(&pid)
		pids = append(pids, pid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return pids
}

// waitPostgresGone waits for role's client sessions to end: the server ends
// a closed connection's backend a moment after the client goes.
func waitPostgresGone(t *testing.T, role string) {
	t.Helper()
	conn := postgresTestConn(t)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var open int
		if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
WHERE usename = $1 AND backend_type = 'client backend'`, role).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions of %s are still open", open, role)
		}
	}
}

// postgresGenerations is a source on generations that role reads, its
// password "reader", so its sessions are told apart from every other test's.
func postgresGenerations(t *testing.T) (schema, role string, src Source) {
	t.Helper()
	schema = postgresFixture(t)
	seedPostgres(t, schema, generations)
	role = postgresReader(t, schema)
	settings := postgresTestSettings(schema)
	settings["user"] = role
	src, _ = postgresTestSource(t, settings, "reader", "")
	return schema, role, src
}

func TestPostgresSourceSnapshot(t *testing.T) {
	ctx := context.Background()
	k := postgresTestKind()

	// The issue's interleaving: a transaction that commits between two
	// tables' reads shows in both or in neither.
	t.Run("reads every table at the full read's snapshot, then lets it go", func(t *testing.T) {
		schema, role, src := postgresGenerations(t)
		snap, err := k.SourceSnapshot(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		if held := postgresHolding(t, role); len(held) != 1 {
			t.Fatalf("sessions holding a snapshot = %v, want one", held)
		}
		a := postgresFullRead(t, snap.Source, "a")
		seedPostgres(t, schema, nextGeneration)
		b := postgresFullRead(t, snap.Source, "b")
		if !slices.Equal(a, generation(1)) || !slices.Equal(b, generation(1)) {
			t.Fatalf("a = %v, b = %v, want both at generation 1", a, b)
		}
		// A read of its own, as every table's was before, sees the commit.
		if own := postgresFullRead(t, src, "b"); !slices.Equal(own, generation(2)) {
			t.Fatalf("b read alone = %v, want generation 2", own)
		}
		snap.Close()
		waitPostgresGone(t, role)
	})
	// Every table is read on the snapshot's own transaction, so a role
	// limited to one connection (or a pooler with one server connection)
	// reads as it did a table at a time.
	t.Run("reads every table over the snapshot's one connection", func(t *testing.T) {
		schema, role, src := postgresGenerations(t)
		if _, err := postgresTestConn(t).Exec(ctx, "ALTER ROLE "+role+" CONNECTION LIMIT 1"); err != nil {
			t.Fatal(err)
		}
		waitPostgresGone(t, role) // Discover's session, which the limit would count
		snap, err := k.SourceSnapshot(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		// The limit is in force: a connection of a table's own is refused.
		if _, err := k.Sync(ctx, src, Table{Name: "a"}, &postgresRawRows{}); err == nil {
			t.Fatal("a second connection was let in, want the role's limit refusing it")
		}
		a := postgresFullRead(t, snap.Source, "a")
		seedPostgres(t, schema, nextGeneration)
		b := postgresFullRead(t, snap.Source, "b")
		snap.Close()
		if !slices.Equal(a, generation(1)) || !slices.Equal(b, generation(1)) {
			t.Fatalf("a = %v, b = %v, want both at generation 1", a, b)
		}
		waitPostgresGone(t, role)
	})
	// The holding transaction sits idle while each table is published; a
	// server's idle transaction timeout must not end it.
	t.Run("holds the snapshot past the server's idle transaction timeout", func(t *testing.T) {
		schema, role, src := postgresGenerations(t)
		if _, err := postgresTestConn(t).Exec(ctx, "ALTER ROLE "+role+" SET idle_in_transaction_session_timeout = '300ms'"); err != nil {
			t.Fatal(err)
		}
		snap, err := k.SourceSnapshot(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		a := postgresFullRead(t, snap.Source, "a")
		seedPostgres(t, schema, nextGeneration)
		time.Sleep(time.Second)
		b := postgresFullRead(t, snap.Source, "b")
		snap.Close()
		if !slices.Equal(a, generation(1)) || !slices.Equal(b, generation(1)) {
			t.Fatalf("a = %v, b = %v, want both at the snapshot's generation 1", a, b)
		}
		waitPostgresGone(t, role)
	})
	// A snapshot whose session ended (a terminated backend, a dropped
	// connection) fails the table's read: the sync's retry reads on, and
	// nothing reads around it at another moment unannounced.
	t.Run("fails a table's read once the snapshot's session ended", func(t *testing.T) {
		_, role, src := postgresGenerations(t)
		snap, err := k.SourceSnapshot(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		defer snap.Close()
		held := postgresHolding(t, role)
		if len(held) != 1 {
			t.Fatalf("sessions holding a snapshot = %v, want one", held)
		}
		if _, err := postgresTestConn(t).Exec(ctx, "SELECT pg_terminate_backend($1)", held[0]); err != nil {
			t.Fatal(err)
		}
		waitPostgresGone(t, role)
		if _, err := k.Sync(ctx, snap.Source, Table{Name: "a"}, &postgresRawRows{}); err == nil {
			t.Fatal("read a on an ended snapshot, want it to fail")
		}
	})
	// The tables share a transaction, not an exported snapshot, so servers
	// and PostgreSQL-compatible databases that can't export one read at one
	// snapshot too.
	t.Run("needs no exported snapshot", func(t *testing.T) {
		database, role := postgresDatabaseWithoutExport(t)
		settings := postgresTestSettings("public")
		settings["database"], settings["user"] = database, role
		src, _ := postgresTestSource(t, settings, "reader", "")
		snap, err := k.SourceSnapshot(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		a, b := postgresFullRead(t, snap.Source, "a"), postgresFullRead(t, snap.Source, "b")
		snap.Close()
		if !slices.Equal(a, generation(1)) || !slices.Equal(b, generation(1)) {
			t.Fatalf("a = %v, b = %v", a, b)
		}
		waitPostgresGone(t, role)
	})
	// CDC's full read must meet its slot's stream exactly, so it keeps the
	// slot's snapshot, holds no other, and never reads around it.
	t.Run("keeps the replication slot's snapshot", func(t *testing.T) {
		schema, publication, connectorID := postgresCDCFixture(t, "customers, orders")
		src, discovered := postgresTestSource(t, postgresCDCSettings(schema, publication), "helix", connectorID)
		start, err := k.Start(ctx, src, postgresFollowed(src, discovered, CaptureLog))
		if err != nil {
			t.Fatal(err)
		}
		defer start.Close()
		snap, err := k.SourceSnapshot(ctx, *start.Source)
		if err != nil {
			t.Fatal(err)
		}
		snap.Close()
		if !bytes.Equal(snap.Source.Settings, start.Source.Settings) || snap.Source.held != nil {
			t.Fatalf("SourceSnapshot bound %s, want the slot's %s", snap.Source.Settings, start.Source.Settings)
		}
		s, err := decodePostgresSettings(snap.Source.Settings)
		if err != nil {
			t.Fatal(err)
		}
		s.Snapshot = "00000099-00000099-1"
		lost := snap.Source
		if lost.Settings, err = json.Marshal(s); err != nil {
			t.Fatal(err)
		}
		if _, err := k.Sync(ctx, lost, Table{Name: "customers"}, &postgresRawRows{}); err == nil || !strings.Contains(err.Error(), "import snapshot") {
			t.Fatalf("Sync error = %v, want the slot's snapshot refused", err)
		}
	})
}

// postgresDatabaseWithoutExport is a database of its own, whose PUBLIC may
// not call pg_export_snapshot, holding generations in public for a role
// that reads it, its password "reader". Revoking it there leaves every other
// test's database as it is.
func postgresDatabaseWithoutExport(t *testing.T) (database, role string) {
	t.Helper()
	ctx := context.Background()
	admin := postgresTestConn(t)
	suffix := time.Now().UnixNano()
	database, role = fmt.Sprintf("no_export_%d", suffix), fmt.Sprintf("no_export_%d", suffix)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'reader'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+database+" WITH (FORCE)")
		_, _ = admin.Exec(context.Background(), "DROP ROLE "+role)
	})
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://helix:helix@%s:%d/%s?sslmode=disable", testPostgresHost, testPostgresPort, database))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, generations+"GRANT SELECT ON a, b TO "+role+";"+
		"REVOKE EXECUTE ON FUNCTION pg_catalog.pg_export_snapshot() FROM PUBLIC;"); err != nil {
		t.Fatal(err)
	}
	return database, role
}
