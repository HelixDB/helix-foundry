package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// postgresCDCFixture is a schema holding customersAndOrders, a publication of
// publish (tables of the schema), and a connector id whose slot is dropped
// when the test ends, since a leaked slot holds WAL.
func postgresCDCFixture(t *testing.T, publish string) (schema, publication, connectorID string) {
	t.Helper()
	schema = postgresFixture(t)
	suffix := strings.TrimPrefix(schema, "connector_test_")
	publication, connectorID = "pub_"+suffix, "conn_"+suffix
	if publish != "" {
		seedPostgres(t, schema, fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE %s", publication, publish))
	}
	conn := postgresTestConn(t)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = conn.Exec(ctx, "DROP PUBLICATION IF EXISTS "+publication)
		_, _ = conn.Exec(ctx, "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = $1", postgresSlot(connectorID))
	})
	return schema, publication, connectorID
}

func postgresCDCSettings(schema, publication string) map[string]any {
	settings := postgresTestSettings(schema)
	settings["cdc"] = map[string]any{"enabled": true, "publication": publication}
	return settings
}

func postgresSlotExists(t *testing.T, slot string) bool {
	t.Helper()
	var exists bool
	err := postgresTestConn(t).QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = $1)", slot).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

// postgresRowWithID picks the full read's row whose id is id.
func postgresRowWithID(t *testing.T, rows []string, id int) string {
	t.Helper()
	for _, row := range rows {
		if strings.Contains(row, fmt.Sprintf(`"id":%d,`, id)) {
			return row
		}
	}
	t.Fatalf("no row with id %d in %v", id, rows)
	return ""
}

func TestPostgresCDC(t *testing.T) {
	ctx := context.Background()
	k := postgresTestKind()
	// begin is a connector whose first full read has finished: its slot exists
	// and the returned cursor is what the syncer committed.
	begin := func(t *testing.T, seed string) (schema string, src Source, tables []Table, cursor json.RawMessage) {
		t.Helper()
		schema, publication, connectorID := postgresCDCFixture(t, "customers, orders")
		seedPostgres(t, schema, seed)
		src, discovered := postgresTestSource(t, postgresCDCSettings(schema, publication), "helix", connectorID)
		tables = postgresFollowed(src, discovered, CaptureLog)
		start, err := k.Start(ctx, src, tables)
		if err != nil {
			t.Fatal(err)
		}
		if start.Close != nil {
			start.Close()
		}
		return schema, src, tables, start.Cursor
	}

	// The copy and the stream must meet exactly: the full read imports the
	// slot's snapshot, so a write after Start is the next change read's, not the copy's.
	t.Run("a full read sees the slot's snapshot and the next change read the rest", func(t *testing.T) {
		schema, publication, connectorID := postgresCDCFixture(t, "customers, orders")
		seedPostgres(t, schema, "INSERT INTO customers VALUES (1, 'a@b.c')")
		src, discovered := postgresTestSource(t, postgresCDCSettings(schema, publication), "helix", connectorID)
		tables := postgresFollowed(src, discovered, CaptureLog)
		if names := tableNames(Discovery{Tables: tables}); !slices.Equal(names, []string{"customers", "orders"}) {
			t.Fatalf("logged tables = %v", names)
		}
		start, err := k.Start(ctx, src, tables)
		if err != nil {
			t.Fatal(err)
		}
		if start.Source == nil || start.Close == nil {
			t.Fatalf("Start = %+v, want a snapshot source held open", start)
		}
		seedPostgres(t, schema, "INSERT INTO customers VALUES (2, 'd@e.f')")
		copied := postgresFullRead(t, *start.Source, "customers")
		start.Close()
		if len(copied) != 1 || !strings.Contains(copied[0], `"id":1`) {
			t.Fatalf("full read = %v, want only the row from before Start", copied)
		}
		if !postgresSlotExists(t, postgresSlot(connectorID)) {
			t.Fatal("Start created no slot")
		}
		log := newPostgresChangeLog()
		if _, err := k.Changes(ctx, src, tables, start.Cursor, log); err != nil {
			t.Fatal(err)
		}
		want := postgresRowWithID(t, postgresFullRead(t, src, "customers"), 2)
		if !slices.Equal(log.upserts["customers"], []string{want}) {
			t.Fatalf("upserts = %v, want %s", log.upserts["customers"], want)
		}
	})
	// A change read must write what a full read would: a byte-identical row
	// merges as no change, and a delete's key must hash like the stored row's.
	t.Run("writes inserts and updates with a full read's bytes and deletes by key", func(t *testing.T) {
		schema, src, tables, cursor := begin(t, "INSERT INTO customers VALUES (1, 'a@b.c'), (2, 'd@e.f')")
		seedPostgres(t, schema, `INSERT INTO orders VALUES (10, 1, 12.50);
UPDATE customers SET email = 'new@b.c' WHERE id = 1; DELETE FROM customers WHERE id = 2;`)
		log := newPostgresChangeLog()
		if _, err := k.Changes(ctx, src, tables, cursor, log); err != nil {
			t.Fatal(err)
		}
		if want := postgresFullRead(t, src, "orders"); !slices.Equal(log.upserts["orders"], want) {
			t.Fatalf("orders upserts = %v, want %v", log.upserts["orders"], want)
		}
		if want := postgresFullRead(t, src, "customers"); !slices.Equal(log.upserts["customers"], want) {
			t.Fatalf("customers upserts = %v, want %v", log.upserts["customers"], want)
		}
		if !slices.Equal(log.deletes["customers"], []string{`{"id":2}`}) {
			t.Fatalf("customers deletes = %v", log.deletes["customers"])
		}
	})
	// The last change to a key wins: a row updated and then deleted before
	// the read must not come back.
	t.Run("ends a row updated then deleted in one batch deleted", func(t *testing.T) {
		schema, src, tables, cursor := begin(t, "INSERT INTO customers VALUES (1, 'a@b.c')")
		seedPostgres(t, schema, "UPDATE customers SET email = 'x@y.z' WHERE id = 1; DELETE FROM customers WHERE id = 1;")
		log := newPostgresChangeLog()
		if _, err := k.Changes(ctx, src, tables, cursor, log); err != nil {
			t.Fatal(err)
		}
		if len(log.upserts["customers"]) != 0 || !slices.Equal(log.deletes["customers"], []string{`{"id":1}`}) {
			t.Fatalf("upserts = %v, deletes = %v", log.upserts["customers"], log.deletes["customers"])
		}
	})
	// The stream does not carry a TOASTed value an update left alone; the row
	// fetched by key does, so the snapshot keeps it.
	t.Run("keeps a large value an update did not touch", func(t *testing.T) {
		schema, publication, connectorID := postgresCDCFixture(t, "")
		seedPostgres(t, schema, fmt.Sprintf(`CREATE TABLE profiles (id INT PRIMARY KEY, bio TEXT, visits INT);
INSERT INTO profiles SELECT 1, string_agg(md5(i::text), ''), 0 FROM generate_series(1, 4000) i;
CREATE PUBLICATION %s FOR TABLE profiles;`, publication))
		src, discovered := postgresTestSource(t, postgresCDCSettings(schema, publication), "helix", connectorID)
		tables := postgresFollowed(src, discovered, CaptureLog)
		start, err := k.Start(ctx, src, tables)
		if err != nil {
			t.Fatal(err)
		}
		start.Close()
		seedPostgres(t, schema, "UPDATE profiles SET visits = 1 WHERE id = 1")
		log := newPostgresChangeLog()
		if _, err := k.Changes(ctx, src, tables, start.Cursor, log); err != nil {
			t.Fatal(err)
		}
		if want := postgresFullRead(t, src, "profiles"); !slices.Equal(log.upserts["profiles"], want) || len(want[0]) < 100_000 {
			t.Fatalf("upserted %d rows, want the full row of %d bytes", len(log.upserts["profiles"]), len(want[0]))
		}
	})
	// A truncate is one message, not a delete per row, and the stream never
	// carries DDL; either way the table must be read in full.
	t.Run("reads a truncated table in full", func(t *testing.T) {
		schema, src, tables, cursor := begin(t, "INSERT INTO customers VALUES (1, 'a@b.c'); INSERT INTO orders VALUES (10, 1, 1);")
		seedPostgres(t, schema, "TRUNCATE orders")
		log := newPostgresChangeLog()
		before := testutil.ToFloat64(cdcResets.WithLabelValues("postgres", "truncate"))
		if _, err := k.Changes(ctx, src, tables, cursor, log); err != nil {
			t.Fatal(err)
		}
		if log.resets["orders"] != "truncate" || len(log.deletes["orders"]) != 0 {
			t.Fatalf("resets = %v, deletes = %v", log.resets, log.deletes)
		}
		if got := testutil.ToFloat64(cdcResets.WithLabelValues("postgres", "truncate")) - before; got != 1 {
			t.Fatalf("truncate resets counted = %v, want 1", got)
		}
	})
	t.Run("reads a table whose definition changed in full", func(t *testing.T) {
		schema, src, tables, cursor := begin(t, "INSERT INTO customers VALUES (1, 'a@b.c')")
		seedPostgres(t, schema, "ALTER TABLE customers ADD COLUMN name TEXT; INSERT INTO customers VALUES (2, 'd@e.f', 'D');")
		log := newPostgresChangeLog()
		next, err := k.Changes(ctx, src, tables, cursor, log)
		if err != nil {
			t.Fatal(err)
		}
		if log.resets["customers"] != "schema" {
			t.Fatalf("resets = %v", log.resets)
		}
		// The new definition is the baseline, so the next read follows the table again.
		seedPostgres(t, schema, "INSERT INTO customers VALUES (3, 'g@h.i', 'G')")
		again := newPostgresChangeLog()
		if _, err := k.Changes(ctx, src, tables, next, again); err != nil {
			t.Fatal(err)
		}
		if len(again.resets) != 0 || len(again.upserts["customers"]) != 1 {
			t.Fatalf("second read resets = %v, upserts = %v", again.resets, again.upserts)
		}
	})
	// The position is confirmed only once the syncer committed the cursor, so
	// a sync that dies after reading reads the same changes again.
	t.Run("reads a batch the syncer did not commit again", func(t *testing.T) {
		schema, src, tables, cursor := begin(t, "")
		seedPostgres(t, schema, "INSERT INTO customers VALUES (1, 'a@b.c'), (2, 'd@e.f')")
		first, second := newPostgresChangeLog(), newPostgresChangeLog()
		if _, err := k.Changes(ctx, src, tables, cursor, first); err != nil {
			t.Fatal(err)
		}
		if _, err := k.Changes(ctx, src, tables, cursor, second); err != nil {
			t.Fatal(err)
		}
		if len(first.upserts["customers"]) != 2 || !slices.Equal(first.upserts["customers"], second.upserts["customers"]) {
			t.Fatalf("first read = %v, replay = %v", first.upserts, second.upserts)
		}
	})
	// A slot dropped by hand or lost to max_slot_wal_keep_size no longer holds
	// the changes, so the syncer must copy the tables again.
	t.Run("expires the cursor when the slot is gone", func(t *testing.T) {
		_, src, tables, cursor := begin(t, "")
		if _, err := postgresTestConn(t).Exec(ctx, "SELECT pg_drop_replication_slot($1)", postgresSlot(src.ConnectorID)); err != nil {
			t.Fatal(err)
		}
		before := testutil.ToFloat64(cdcResets.WithLabelValues("postgres", "missing"))
		_, err := k.Changes(ctx, src, tables, cursor, newPostgresChangeLog())
		var lost *PositionLostError
		if !errors.Is(err, ErrCursorExpired) || !errors.As(err, &lost) || lost.Reason != "missing" {
			t.Fatalf("Changes error = %v, want a lost position", err)
		}
		if got := testutil.ToFloat64(cdcResets.WithLabelValues("postgres", "missing")) - before; got != 1 {
			t.Fatalf("missing resets counted = %v, want 1", got)
		}
	})
	// A reconcile's full read must not lose what the slot holds: it keeps the
	// slot and resumes from its confirmed position, re-reading rather than skipping.
	t.Run("keeps a healthy slot across a full read", func(t *testing.T) {
		schema, src, tables, cursor := begin(t, "")
		seedPostgres(t, schema, "INSERT INTO customers VALUES (1, 'a@b.c')")
		start, err := k.Start(ctx, src, tables)
		if err != nil {
			t.Fatal(err)
		}
		if start.Source != nil || start.Close != nil {
			t.Fatalf("Start = %+v, want the slot kept without a new snapshot", start)
		}
		var before, after postgresCursor
		_ = json.Unmarshal(cursor, &before)
		_ = json.Unmarshal(start.Cursor, &after)
		if after.Slot != before.Slot || after.LSN != before.LSN {
			t.Fatalf("cursor = %s, want the slot's confirmed position %s", start.Cursor, cursor)
		}
		log := newPostgresChangeLog()
		if _, err := k.Changes(ctx, src, tables, start.Cursor, log); err != nil {
			t.Fatal(err)
		}
		if len(log.upserts["customers"]) != 1 {
			t.Fatalf("upserts = %v, want the insert replayed", log.upserts)
		}
	})
	// Every path that stops reading the log gives the slot back, or it would
	// hold the customer's WAL forever.
	t.Run("drops the slot on release", func(t *testing.T) {
		_, src, tables, cursor := begin(t, "")
		next, err := k.Changes(ctx, src, tables, cursor, newPostgresChangeLog())
		if err != nil {
			t.Fatal(err)
		}
		if got := testutil.ToFloat64(cdcRetainedBytes.WithLabelValues(src.ConnectorID)); got < 0 {
			t.Fatalf("retained bytes = %v", got)
		}
		if err := k.Release(ctx, src, next); err != nil {
			t.Fatal(err)
		}
		if postgresSlotExists(t, postgresSlot(src.ConnectorID)) {
			t.Fatal("the slot survived Release")
		}
		// A released slot holds nothing, so its gauge must not linger at its last value.
		if cdcRetainedBytes.DeleteLabelValues(src.ConnectorID) {
			t.Fatal("retained bytes still reported after Release")
		}
	})
	t.Run("drops the slot at the first full read after CDC is turned off", func(t *testing.T) {
		schema, src, _, _ := begin(t, "")
		settings := postgresTestSettings(schema)
		settings["cursor_columns"] = map[string]string{"customers": "id"}
		pro, discovered := postgresTestSource(t, settings, "helix", src.ConnectorID)
		if _, err := k.Start(ctx, pro, postgresFollowed(pro, discovered, CaptureCursor)); err != nil {
			t.Fatal(err)
		}
		if postgresSlotExists(t, postgresSlot(src.ConnectorID)) {
			t.Fatal("the slot survived a full read without CDC")
		}
	})
	// A downgrade keeps the CDC setting, so the plan alone must stop the slot
	// holding WAL and hand the table to its cursor column.
	t.Run("drops the slot and follows the cursor column on a plan without the log", func(t *testing.T) {
		_, src, _, _ := begin(t, "")
		if !postgresSlotExists(t, postgresSlot(src.ConnectorID)) {
			t.Fatal("the first full read left no slot")
		}
		var s postgresSettings
		if err := json.Unmarshal(src.Settings, &s); err != nil {
			t.Fatal(err)
		}
		s.CursorColumns = map[string]string{"customers": "id"}
		pro := src
		pro.Captures = []Capture{CaptureFull, CaptureCursor, CaptureEvents}
		var err error
		if pro.Settings, err = json.Marshal(s); err != nil {
			t.Fatal(err)
		}
		if got := k.Capture(pro, "customers"); got != CaptureCursor {
			t.Fatalf("Capture(customers) = %s, want cursor", got)
		}
		if _, err := k.Start(ctx, pro, []Table{{Name: "customers", PrimaryKey: []string{"id"}}}); err != nil {
			t.Fatal(err)
		}
		if postgresSlotExists(t, postgresSlot(src.ConnectorID)) {
			t.Fatal("the slot survived a full read on a plan without the log")
		}
	})
	// A read must leave the slot free, or the next one can't take it; and an
	// idle read must not wait out the server's keepalive interval.
	t.Run("reads an idle slot back to back", func(t *testing.T) {
		_, src, tables, cursor := begin(t, "")
		began := time.Now()
		for i := 0; i < 3; i++ {
			next, err := k.Changes(ctx, src, tables, cursor, newPostgresChangeLog())
			if err != nil {
				t.Fatalf("read %d: %v", i, err)
			}
			cursor = next
		}
		if took := time.Since(began); took > 10*time.Second {
			t.Fatalf("three idle reads took %s", took)
		}
	})
}

func TestPostgresCDCChecks(t *testing.T) {
	local := NewCatalog(DefaultEndpoints(), Guard{AllowPrivate: true}, S3Options{}, EventsOptions{})
	refusal := func(t *testing.T, settings map[string]any, password string) string {
		t.Helper()
		_, err := discoverDatabase(t, kindOf(t, local, "postgres"), settings, password)
		var invalidErr *InvalidError
		var refusedErr *RefusedError
		switch {
		case errors.As(err, &invalidErr):
			return invalidErr.Message
		case errors.As(err, &refusedErr):
			return refusedErr.Message
		}
		t.Fatalf("Discover error = %v, want a refusal", err)
		return ""
	}

	// Updates and deletes to a published table without a replica identity fail
	// in the customer's own application, so CDC is refused and the table named.
	t.Run("refuses a publication holding a table without a replica identity", func(t *testing.T) {
		schema, publication, _ := postgresCDCFixture(t, "customers, notes")
		msg := refusal(t, postgresCDCSettings(schema, publication), "helix")
		if !strings.Contains(msg, "replica identity") || !strings.Contains(msg, schema+".notes") {
			t.Fatalf("refusal = %q, want notes named", msg)
		}
	})
	t.Run("refuses a publication that does not exist and shows how to create it", func(t *testing.T) {
		schema, publication, _ := postgresCDCFixture(t, "")
		if msg := refusal(t, postgresCDCSettings(schema, publication), "helix"); !strings.Contains(msg, "CREATE PUBLICATION") {
			t.Fatalf("refusal = %q", msg)
		}
	})
	// After a downgrade the settings keep CDC; a publication the customer then
	// drops must not stop a sync that reads no log.
	t.Run("skips the log checks on a sync whose plan leaves out the log", func(t *testing.T) {
		schema, publication, connectorID := postgresCDCFixture(t, "")
		raw, err := json.Marshal(postgresCDCSettings(schema, publication))
		if err != nil {
			t.Fatal(err)
		}
		k := postgresTestKind()
		prepared, err := k.Prepare(Source{Settings: raw, Secret: "helix"})
		if err != nil {
			t.Fatal(err)
		}
		src := Source{Settings: prepared, Secret: "helix", ConnectorID: connectorID, ForSync: true,
			Captures: []Capture{CaptureFull, CaptureCursor, CaptureEvents}}
		if _, err := k.Discover(context.Background(), src); err != nil {
			t.Fatalf("Discover on a plan without the log = %v", err)
		}
		src.Captures = everyCapture
		if _, err := k.Discover(context.Background(), src); err == nil {
			t.Fatal("Discover on a plan with the log passed without the publication")
		}
	})
	// A change read sees only what the publication sends: without inserts new
	// rows never arrive, without truncates emptied tables keep their rows.
	for _, publish := range []string{"insert", "update, delete", "insert, update, delete"} {
		t.Run("refuses a publication that publishes only "+publish, func(t *testing.T) {
			schema, publication, _ := postgresCDCFixture(t, "")
			seedPostgres(t, schema, fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE customers WITH (publish = '%s')", publication, publish))
			if msg := refusal(t, postgresCDCSettings(schema, publication), "helix"); !strings.Contains(msg, "inserts, updates, deletes and truncates") {
				t.Fatalf("refusal = %q", msg)
			}
		})
	}
	// A row filter hides changes to rows outside it, which a full read copies;
	// such a table falls back to its cursor column, and Test says why.
	t.Run("follows a row-filtered table by its cursor column and warns", func(t *testing.T) {
		schema, publication, connectorID := postgresCDCFixture(t, "")
		seedPostgres(t, schema, fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE customers WHERE (id > 0), orders", publication))
		settings := postgresCDCSettings(schema, publication)
		settings["cursor_columns"] = map[string]string{"customers": "id"}
		d, err := discoverDatabase(t, postgresTestKind(), settings, "helix")
		if err != nil || len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "filters the rows of customers") {
			t.Fatalf("Discover = (%v, %v)", d.Warnings, err)
		}
		src, _ := postgresTestSource(t, settings, "helix", connectorID)
		k := postgresTestKind()
		if got := k.Capture(src, "customers"); got != CaptureCursor {
			t.Fatalf("Capture(customers) = %s, want cursor", got)
		}
		if got := k.Capture(src, "orders"); got != CaptureLog {
			t.Fatalf("Capture(orders) = %s, want log", got)
		}
	})
	// The slot needs REPLICATION; a read-only user is told the grant it lacks.
	t.Run("refuses a user that may not replicate", func(t *testing.T) {
		schema, publication, _ := postgresCDCFixture(t, "customers")
		settings := postgresCDCSettings(schema, publication)
		settings["user"] = postgresReader(t, schema)
		if msg := refusal(t, settings, "reader"); !strings.Contains(msg, "WITH REPLICATION") {
			t.Fatalf("refusal = %q", msg)
		}
	})
	// Every managed host needs a setting and a restart; the test database has
	// logical decoding on, so the decision is checked on what Test read.
	t.Run("names the setting when logical decoding is off", func(t *testing.T) {
		setup := postgresLogSetup{version: 160000, walLevel: "replica", maxSlots: 10}
		_, err := setup.problem(DatabaseSettings{User: "app", Schema: "public", CDC: &DatabaseCDC{Enabled: true, Publication: "helix_foundry"}}, []string{"orders"})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) || !strings.Contains(invalidErr.Message, "wal_level = logical") {
			t.Fatalf("problem = %v", err)
		}
	})
	// PostgreSQL 18 drops a slot idle for longer than the timeout, and a slot
	// sits idle between syncs, so a short one would force a full copy every sync.
	t.Run("refuses an idle slot timeout shorter than a day", func(t *testing.T) {
		settings := DatabaseSettings{User: "app", Schema: "public", CDC: &DatabaseCDC{Enabled: true, Publication: "helix_foundry"}}
		setup := postgresLogSetup{version: 180000, walLevel: "logical", maxSlots: 10, publication: true, publishesChanges: true,
			published: []string{"orders"}, idleSlotTimeout: pgSettingDuration(3600, "s")}
		_, err := setup.problem(settings, []string{"orders"})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) || !strings.Contains(invalidErr.Message, "idle_replication_slot_timeout is 1h0m0s") {
			t.Fatalf("problem = %v", err)
		}
		setup.idleSlotTimeout = pgSettingDuration(2, "d")
		if _, err := setup.problem(settings, []string{"orders"}); err != nil {
			t.Fatalf("problem with a two-day timeout = %v", err)
		}
	})
	// A chosen table the publication leaves out keeps Pro's method instead of
	// failing the connector; a keyless one is always read in full.
	t.Run("follows only published tables with a key through the log", func(t *testing.T) {
		schema, publication, connectorID := postgresCDCFixture(t, "customers")
		settings := postgresCDCSettings(schema, publication)
		settings["cursor_columns"] = map[string]string{"orders": "id"}
		src, _ := postgresTestSource(t, settings, "helix", connectorID)
		k := postgresTestKind()
		got := map[string]Capture{}
		for _, table := range []string{"customers", "orders", "notes"} {
			got[table] = k.Capture(src, table)
		}
		if got["customers"] != CaptureLog || got["orders"] != CaptureCursor || got["notes"] != CaptureFull {
			t.Fatalf("Capture = %v", got)
		}
	})
}
