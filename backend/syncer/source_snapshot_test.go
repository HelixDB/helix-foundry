package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// snapshotKind is a fakeKind that reads its tables at one snapshot of its
// source, which it names in the settings it binds: s1, s2, and so on.
type snapshotKind struct {
	fakeKind
	// refuse fails SourceSnapshot.
	refuse error
	opened int
	closed int
	// fail fails the next read of each table it names.
	fail map[string]error
}

func (k *snapshotKind) SourceSnapshot(_ context.Context, src connector.Source) (connector.SourceSnapshot, error) {
	if k.refuse != nil {
		return connector.SourceSnapshot{}, k.refuse
	}
	k.opened++
	src.Settings = json.RawMessage(fmt.Sprintf(`{"snapshot":"s%d"}`, k.opened))
	return connector.SourceSnapshot{Source: src, Close: func() { k.closed++ }}, nil
}

func (k *snapshotKind) Sync(ctx context.Context, src connector.Source, table connector.Table, w connector.RowWriter) (connector.Table, error) {
	if err := k.fail[table.Name]; err != nil {
		delete(k.fail, table.Name)
		k.synced, k.sources = append(k.synced, table.Name), append(k.sources, src)
		return connector.Table{}, err
	}
	return k.fakeKind.Sync(ctx, src, table, w)
}

// twoTables is a snapshotKind with tables a and b, a row each.
func twoTables() *snapshotKind {
	return &snapshotKind{fakeKind: fakeKind{
		discovery: connector.Discovery{Tables: []connector.Table{{Name: "a", PrimaryKey: []string{"id"}}, {Name: "b", PrimaryKey: []string{"id"}}}},
		rows:      map[string][]string{"a": {`{"id":1}`}, "b": {`{"id":1}`}},
	}}
}

// readWith is the settings each table's read ran with, in order.
func (k *snapshotKind) readWith() []string {
	var out []string
	for _, src := range k.sources[len(k.sources)-len(k.synced):] {
		out = append(out, string(src.Settings))
	}
	return out
}

func sourceSnapshotCount(outcome string) float64 {
	return testutil.ToFloat64(sourceSnapshots.WithLabelValues("fake", outcome))
}

func TestSyncerSourceSnapshot(t *testing.T) {
	ctx := context.Background()
	t.Run("reads every table at one snapshot, then releases it", func(t *testing.T) {
		kind := twoTables()
		f := newFakeFixture(t, &kind.fakeKind, `{}`)
		f.syncer.Catalog = connector.NewCatalogOf(kind)
		before := sourceSnapshotCount("one")
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if got := kind.readWith(); !slices.Equal(got, []string{`{"snapshot":"s1"}`, `{"snapshot":"s1"}`}) {
			t.Fatalf("tables read with %v, want both at s1", got)
		}
		if kind.opened != 1 || kind.closed != 1 || sourceSnapshotCount("one")-before != 1 {
			t.Fatalf("opened %d, closed %d, counted %v", kind.opened, kind.closed, sourceSnapshotCount("one")-before)
		}
	})
	// A table the first attempt wrote shows that attempt's moment. It is
	// kept, so a read longer than one attempt still finishes across
	// attempts, and the read is counted resumed.
	t.Run("keeps what an earlier attempt wrote on a retry, and counts the read resumed", func(t *testing.T) {
		kind := twoTables()
		kind.fail = map[string]error{"b": &connector.RefusedError{Message: "the database is restarting"}}
		f := newFakeFixture(t, &kind.fakeKind, `{}`)
		f.syncer.Catalog = connector.NewCatalogOf(kind)
		before := sourceSnapshotCount("resumed")
		if _, err := f.syncer.Run(ctx, f.sync); err == nil {
			t.Fatal("the first attempt succeeded, want it to fail after writing a")
		}
		if kind.closed != 1 {
			t.Fatalf("closed %d after the failed attempt, want 1", kind.closed)
		}
		kind.rows["a"] = []string{`{"id":2}`}
		kind.synced = nil
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(kind.synced, []string{"b"}) || !slices.Equal(kind.readWith(), []string{`{}`}) {
			t.Fatalf("retry read %v with %v, want only b, unbound", kind.synced, kind.readWith())
		}
		if kind.opened != 1 || kind.closed != 1 {
			t.Fatalf("opened %d, closed %d, want the first attempt's snapshot only", kind.opened, kind.closed)
		}
		if got := f.snapshotRows(t, "a"); !slices.Equal(got, []string{`{"id":1}`}) {
			t.Fatalf("a = %v, want the first attempt's rows", got)
		}
		if got := sourceSnapshotCount("resumed") - before; got != 1 {
			t.Fatalf("counted %v resumed, want 1", got)
		}
	})
	t.Run("counts no outcome for a read that failed", func(t *testing.T) {
		kind := twoTables()
		kind.fail = map[string]error{"a": &connector.RefusedError{Message: "the database is restarting"}}
		f := newFakeFixture(t, &kind.fakeKind, `{}`)
		f.syncer.Catalog = connector.NewCatalogOf(kind)
		before := sourceSnapshotCount("one")
		if _, err := f.syncer.Run(ctx, f.sync); err == nil {
			t.Fatal("the read succeeded")
		}
		if kind.closed != 1 || sourceSnapshotCount("one") != before {
			t.Fatalf("closed %d, counted %v", kind.closed, sourceSnapshotCount("one")-before)
		}
	})
	// A source with no connection to spare for the snapshot (a role's
	// connection limit, a pooler's wait) reads as it did before snapshots.
	t.Run("reads a table at a time when the source has no connection for a snapshot", func(t *testing.T) {
		kind := twoTables()
		kind.refuse = &connector.RefusedError{Message: "could not connect; check the host, port and connection settings, then test again",
			Err: errors.New("FATAL: too many connections for role \"reader\" (SQLSTATE 53300)")}
		f := newFakeFixture(t, &kind.fakeKind, `{}`)
		f.syncer.Catalog = connector.NewCatalogOf(kind)
		before, one := sourceSnapshotCount("unavailable"), sourceSnapshotCount("one")
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(kind.synced, []string{"a", "b"}) || !slices.Equal(kind.readWith(), []string{`{}`, `{}`}) {
			t.Fatalf("read %v with %v, want both, unbound", kind.synced, kind.readWith())
		}
		if kind.opened != 0 || kind.closed != 0 || len(f.store.tables) != 2 {
			t.Fatalf("opened %d, closed %d, tables %v", kind.opened, kind.closed, f.store.tables)
		}
		if got, shared := sourceSnapshotCount("unavailable")-before, sourceSnapshotCount("one")-one; got != 1 || shared != 0 {
			t.Fatalf("counted %v unavailable and %v one, want one read unavailable", got, shared)
		}
	})
	// A source that takes no snapshot reads as it did before snapshots, and a
	// retry keeps what the attempt before it wrote.
	t.Run("reads a table at a time when the source takes no snapshot", func(t *testing.T) {
		kind := twoTables()
		kind.refuse = errors.New("the server refused a read-only transaction")
		kind.fail = map[string]error{"b": &connector.RefusedError{Message: "the database is restarting"}}
		f := newFakeFixture(t, &kind.fakeKind, `{}`)
		f.syncer.Catalog = connector.NewCatalogOf(kind)
		before, resumed := sourceSnapshotCount("unavailable"), sourceSnapshotCount("resumed")
		if _, err := f.syncer.Run(ctx, f.sync); err == nil {
			t.Fatal("the first attempt succeeded, want it to fail after writing a")
		}
		kind.synced = nil
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(kind.synced, []string{"b"}) || !slices.Equal(kind.readWith(), []string{`{}`}) {
			t.Fatalf("retry read %v with %v, want only b, unbound", kind.synced, kind.readWith())
		}
		if got, again := sourceSnapshotCount("unavailable")-before, sourceSnapshotCount("resumed")-resumed; got != 0 || again != 1 {
			t.Fatalf("counted %v unavailable and %v resumed, want the finished retry resumed only", got, again)
		}
	})
	t.Run("takes no snapshot to read one table", func(t *testing.T) {
		kind := twoTables()
		f := newFakeFixture(t, &kind.fakeKind, `{"tables":["a"]}`)
		f.syncer.Catalog = connector.NewCatalogOf(kind)
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if kind.opened != 0 || !slices.Equal(kind.synced, []string{"a"}) {
			t.Fatalf("opened %d, read %v", kind.opened, kind.synced)
		}
	})
}

// The test PostgreSQL of compose.test.yaml (docker compose -f compose.test.yaml up -d --wait).
const testPostgres = "postgres://helix:helix@127.0.0.1:5433/helix_connector_test?sslmode=disable"

// postgresKind is the PostgreSQL kind under the test plans' name, fake,
// calling after once each table is read; fail fails a table's next read.
type postgresKind struct {
	connector.SnapshotKind
	after func(table string)
	fail  map[string]error
	read  []string
}

func (k *postgresKind) Name() string { return "fake" }

func (k *postgresKind) Sync(ctx context.Context, src connector.Source, table connector.Table, w connector.RowWriter) (connector.Table, error) {
	k.read = append(k.read, table.Name)
	if err := k.fail[table.Name]; err != nil {
		delete(k.fail, table.Name)
		return connector.Table{}, err
	}
	got, err := k.SnapshotKind.Sync(ctx, src, table, w)
	if k.after != nil {
		k.after(table.Name)
	}
	return got, err
}

// postgresCDCKind is postgresKind following its tables through the log.
type postgresCDCKind struct {
	*postgresKind
	connector.ChangeReader
	connector.Releaser
}

// postgresAdmin is a superuser connection to the test database.
func postgresAdmin(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testPostgres)
	if err != nil {
		t.Fatalf("connect test postgres (are the test databases up? compose.test.yaml): %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func postgresExec(t *testing.T, statements string) {
	t.Helper()
	if _, err := postgresAdmin(t).Exec(context.Background(), statements); err != nil {
		t.Fatal(err)
	}
}

// postgresFixture is a connector on tables a and b of a schema of its own,
// each at generation 1, read by a role of its own (grants, such as
// REPLICATION), so its sessions are told apart from every other test's; kind
// reads it. The returned func moves both tables a generation on in one
// transaction.
func postgresFixture(t *testing.T, code, grants string, settings map[string]any) (fixture, *postgresKind, string, func()) {
	t.Helper()
	name := fmt.Sprintf("syncer_test_%d", time.Now().UnixNano())
	postgresExec(t, fmt.Sprintf(`CREATE SCHEMA %[1]s;
CREATE TABLE %[1]s.a (id INT PRIMARY KEY, generation INT NOT NULL);
CREATE TABLE %[1]s.b (id INT PRIMARY KEY, generation INT NOT NULL);
INSERT INTO %[1]s.a VALUES (1, 1); INSERT INTO %[1]s.b VALUES (1, 1);
CREATE ROLE %[1]s LOGIN %[2]s PASSWORD 'reader';
GRANT USAGE ON SCHEMA %[1]s TO %[1]s; GRANT SELECT ON ALL TABLES IN SCHEMA %[1]s TO %[1]s;`, name, grants))
	t.Cleanup(func() {
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, testPostgres)
		if err != nil {
			return
		}
		defer conn.Close(ctx)
		_, _ = conn.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1", name)
		_, _ = conn.Exec(ctx, fmt.Sprintf("DROP SCHEMA %[1]s CASCADE; DROP OWNED BY %[1]s; DROP ROLE %[1]s", name))
	})
	config := map[string]any{"host": "127.0.0.1", "port": 5433, "database": "helix_connector_test", "user": name,
		"schema": name, "disable_tls": true}
	maps.Copy(config, settings)
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, "http://unused", "fake", string(raw), plan{code: code})
	c := f.store.connectors["conn-1"]
	if c.SecretCiphertext, c.SecretKeyID, err = f.syncer.Keys.Seal([]byte("reader"), connector.SecretContext(c.ID)); err != nil {
		t.Fatal(err)
	}
	pg, err := connector.NewCatalog(connector.DefaultEndpoints(), connector.Guard{AllowPrivate: true}, connector.S3Options{},
		connector.EventsOptions{}).Kind("postgres")
	if err != nil {
		t.Fatal(err)
	}
	kind := &postgresKind{SnapshotKind: pg.(connector.SnapshotKind), fail: map[string]error{}}
	f.syncer.Catalog = connector.NewCatalogOf(kind)
	return f, kind, name, func() {
		postgresExec(t, fmt.Sprintf("BEGIN; UPDATE %[1]s.a SET generation = generation + 1; UPDATE %[1]s.b SET generation = generation + 1; COMMIT;", name))
	}
}

// postgresHolding is the pids of role's client sessions idle in a
// transaction: a snapshot held open between reads.
func postgresHolding(t *testing.T, role string) []int32 {
	t.Helper()
	rows, err := postgresAdmin(t).Query(context.Background(), `SELECT pid FROM pg_stat_activity
WHERE usename = $1 AND backend_type = 'client backend' AND state LIKE 'idle in transaction%'`, role)
	if err != nil {
		t.Fatal(err)
	}
	pids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		t.Fatal(err)
	}
	return pids
}

// waitPostgresGone waits for role's client sessions to end: the server ends
// a closed connection's backend a moment after the client goes.
func waitPostgresGone(t *testing.T, role string) {
	t.Helper()
	conn := postgresAdmin(t)
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

func TestSyncerPostgresSnapshot(t *testing.T) {
	ctx := context.Background()
	// The issue's interleaving, through the syncer: a commit between two
	// tables' reads shows in both tables' snapshots or in neither.
	t.Run("stores both tables at one moment though both change between their reads", func(t *testing.T) {
		f, kind, role, next := postgresFixture(t, "free", "", nil)
		var held []int32
		kind.after = func(table string) {
			if table == "a" {
				held = postgresHolding(t, role)
				next()
			}
		}
		before := sourceSnapshotCount("one")
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if len(held) != 1 {
			t.Fatalf("sessions holding a snapshot during the read = %v, want one", held)
		}
		for _, table := range []string{"a", "b"} {
			if got := f.snapshotRows(t, table); !slices.Equal(got, []string{`{"id":1,"generation":1}`}) {
				t.Fatalf("%s = %v, want generation 1", table, got)
			}
		}
		if got := sourceSnapshotCount("one") - before; got != 1 {
			t.Fatalf("counted %v reads at one snapshot", got)
		}
		waitPostgresGone(t, role)
	})
	// A retry keeps the table the first attempt stored, at that attempt's
	// moment, and reads the rest at a moment of their own, so a read longer
	// than one attempt still finishes.
	t.Run("keeps what an earlier attempt stored on a retry", func(t *testing.T) {
		f, kind, role, next := postgresFixture(t, "free", "", nil)
		kind.fail["b"] = &connector.RefusedError{Message: "the database is restarting"}
		before := sourceSnapshotCount("resumed")
		if _, err := f.syncer.Run(ctx, f.sync); err == nil {
			t.Fatal("the first attempt succeeded, want it to fail after storing a")
		}
		// The failed attempt let its snapshot go too.
		waitPostgresGone(t, role)
		next()
		kind.read = nil
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(kind.read, []string{"b"}) {
			t.Fatalf("the retry read %v, want b only", kind.read)
		}
		for table, want := range map[string]string{"a": `{"id":1,"generation":1}`, "b": `{"id":1,"generation":2}`} {
			if got := f.snapshotRows(t, table); !slices.Equal(got, []string{want}) {
				t.Fatalf("%s = %v, want %s", table, got, want)
			}
		}
		if got := sourceSnapshotCount("resumed") - before; got != 1 {
			t.Fatalf("counted %v resumed, want 1", got)
		}
		waitPostgresGone(t, role)
	})
	t.Run("lets the snapshot go when a read fails", func(t *testing.T) {
		f, kind, role, _ := postgresFixture(t, "free", "", nil)
		f.syncer.MaxRows = 0
		var held []int32
		kind.after = func(string) { held = postgresHolding(t, role) }
		if _, err := f.syncer.Run(ctx, f.sync); err == nil || !strings.Contains(err.Error(), "more than 0 rows") {
			t.Fatalf("Run error = %v, want table a over the row limit", err)
		}
		if len(held) != 1 {
			t.Fatalf("sessions holding a snapshot during the read = %v, want one", held)
		}
		waitPostgresGone(t, role)
	})
	// The tables are read on the snapshot's own transaction, so a role
	// limited to one connection, which a snapshot held beside each table's
	// connection would lock out, reads every table at one moment.
	t.Run("reads at one snapshot as a role limited to one connection", func(t *testing.T) {
		f, kind, role, next := postgresFixture(t, "free", "CONNECTION LIMIT 1", nil)
		var held []int32
		kind.after = func(table string) {
			if table == "a" {
				held = postgresHolding(t, role)
				next()
			}
		}
		before := sourceSnapshotCount("one")
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if len(held) != 1 {
			t.Fatalf("sessions holding a snapshot during the read = %v, want one", held)
		}
		for _, table := range []string{"a", "b"} {
			if got := f.snapshotRows(t, table); !slices.Equal(got, []string{`{"id":1,"generation":1}`}) {
				t.Fatalf("%s = %v, want generation 1", table, got)
			}
		}
		if got := sourceSnapshotCount("one") - before; got != 1 {
			t.Fatalf("counted %v reads at one snapshot", got)
		}
		waitPostgresGone(t, role)
	})
	// A snapshot whose session ended fails the attempt; the retry keeps the
	// table stored and reads the rest at a moment of its own.
	t.Run("fails the attempt when the snapshot's session ends, and the retry reads on", func(t *testing.T) {
		f, kind, role, next := postgresFixture(t, "free", "", nil)
		kind.after = func(table string) {
			if table != "a" {
				return
			}
			for _, pid := range postgresHolding(t, role) {
				postgresExec(t, fmt.Sprintf("SELECT pg_terminate_backend(%d)", pid))
			}
			waitPostgresGone(t, role)
			next()
		}
		if _, err := f.syncer.Run(ctx, f.sync); err == nil {
			t.Fatal("the attempt succeeded, want b's read on the ended snapshot to fail")
		}
		kind.after, kind.read = nil, nil
		before := sourceSnapshotCount("resumed")
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if a, b := f.snapshotRows(t, "a"), f.snapshotRows(t, "b"); !slices.Equal(kind.read, []string{"b"}) ||
			!slices.Equal(a, []string{`{"id":1,"generation":1}`}) || !slices.Equal(b, []string{`{"id":1,"generation":2}`}) {
			t.Fatalf("retry read %v; a = %v, b = %v, want b alone read at a moment of its own", kind.read, a, b)
		}
		if got := sourceSnapshotCount("resumed") - before; got != 1 {
			t.Fatalf("counted %v resumed, want 1", got)
		}
		waitPostgresGone(t, role)
	})
	// CDC's full read keeps the slot's snapshot, which its stream starts
	// from, and holds no other; the next sync reads the changes since.
	t.Run("keeps the replication slot's snapshot with CDC", func(t *testing.T) {
		publication := fmt.Sprintf("pub_%d", time.Now().UnixNano())
		f, kind, role, next := postgresFixture(t, "max", "REPLICATION", map[string]any{
			"cdc": map[string]any{"enabled": true, "publication": publication}})
		postgresExec(t, fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE %s.a, %s.b", publication, role, role))
		t.Cleanup(func() {
			admin := postgresAdmin(t)
			_, _ = admin.Exec(context.Background(), "DROP PUBLICATION IF EXISTS "+publication)
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
				if _, err := admin.Exec(context.Background(), `SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots
WHERE slot_name = 'helix_foundry_conn_1'`); err == nil {
					return
				}
			}
		})
		pg := kind.SnapshotKind.(interface {
			connector.ChangeReader
			connector.Releaser
		})
		f.syncer.Catalog = connector.NewCatalogOf(&postgresCDCKind{postgresKind: kind, ChangeReader: pg, Releaser: pg})
		var held []int32
		kind.after = func(table string) {
			if table == "a" {
				held = postgresHolding(t, role)
				next()
			}
		}
		res, err := f.syncer.Run(ctx, f.sync)
		if err != nil {
			t.Fatal(err)
		}
		if res.Mode != model.ConnectorSyncFull || len(held) != 0 {
			t.Fatalf("mode %s, sessions holding a snapshot = %v, want a full read at the slot's snapshot alone", res.Mode, held)
		}
		for _, table := range []string{"a", "b"} {
			if got := f.snapshotRows(t, table); !slices.Equal(got, []string{`{"id":1,"generation":1}`}) {
				t.Fatalf("%s = %v, want the slot's generation 1", table, got)
			}
		}
		res, err = f.syncer.Run(ctx, newSync("sync-2"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Mode != model.ConnectorSyncChanges || !slices.Equal(slices.Sorted(slices.Values(res.Changed)), []string{"a", "b"}) {
			t.Fatalf("second sync = %+v, want the change read of both", res)
		}
		for _, table := range []string{"a", "b"} {
			if got := f.snapshotRows(t, table); !slices.Equal(got, []string{`{"id":1,"generation":2}`}) {
				t.Fatalf("%s = %v, want the change read's generation 2", table, got)
			}
		}
	})
}
