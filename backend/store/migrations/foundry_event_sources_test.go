package migrations

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryEventSources creates foundry_event_batches and foundry_event_source_tokens.
const foundryEventSources = 20261105000000

func TestFoundryEventSourcesMigration(t *testing.T) {
	// migrated is a fresh database migrated up to and including event sources.
	migrated := func(t *testing.T) *sql.DB {
		t.Helper()
		db := isolatedTestDB(t)
		if err := setup(db); err != nil {
			t.Fatal(err)
		}
		if err := goose.UpTo(db, ".", foundryEventSources); err != nil {
			t.Fatal(err)
		}
		return db
	}
	exec := func(t *testing.T, db *sql.DB, query string, args ...any) error {
		t.Helper()
		_, err := db.Exec(query, args...)
		return err
	}
	// indexes lists a table's indexes as "name(columns) unique|index", by
	// name in the catalog's case-insensitive order.
	indexes := func(t *testing.T, db *sql.DB, table string) []string {
		t.Helper()
		rows, err := db.Query(`SELECT index_name, GROUP_CONCAT(column_name ORDER BY seq_in_index), MAX(non_unique)
FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = ?
GROUP BY index_name ORDER BY index_name`, table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var name, columns string
			var nonUnique int
			if err := rows.Scan(&name, &columns, &nonUnique); err != nil {
				t.Fatal(err)
			}
			kind := "unique"
			if nonUnique == 1 {
				kind = "index"
			}
			out = append(out, name+"("+columns+") "+kind)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	tables := func(t *testing.T, db *sql.DB) []string {
		t.Helper()
		rows, err := db.Query(`SELECT table_name FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name IN ('foundry_event_batches', 'foundry_event_source_tokens') ORDER BY table_name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			out = append(out, name)
		}
		return out
	}
	batch := func(id, connector string, seq int, token any) []any {
		return []any{id, "w", connector, seq, token, "ws/w/events/" + connector + "/" + id + ".jsonl.gz"}
	}
	const insertBatch = `INSERT INTO foundry_event_batches (id, workspace_id, connector_id, seq, client_token, s3_key, events, bytes, first_at, last_at)
VALUES (?, ?, ?, ?, ?, ?, 1, 80, NOW(6), NOW(6))`
	const insertToken = `INSERT INTO foundry_event_source_tokens (id, workspace_id, connector_id, hash, prefix, created_by)
VALUES (?, 'w', 'c', ?, 'hx_ev_0a1b2c', 'u')`

	// The writer, the change read, the replay check and the day counters
	// each read through one of these keys; a missing one is a full scan, a
	// missing unique key a duplicated seq or send.
	t.Run("creates both tables with their keys", func(t *testing.T) {
		db := migrated(t)
		if got, want := indexes(t, db, "foundry_event_batches"), []string{
			"foundry_event_batches_day_idx(workspace_id,created_at) index",
			"foundry_event_batches_seq_uq(connector_id,seq) unique",
			"foundry_event_batches_token_uq(connector_id,client_token) unique",
			"PRIMARY(id) unique",
		}; !slices.Equal(got, want) {
			t.Fatalf("foundry_event_batches indexes = %v, want %v", got, want)
		}
		if got, want := indexes(t, db, "foundry_event_source_tokens"), []string{
			"foundry_event_source_tokens_hash_uq(hash) unique",
			"foundry_event_source_tokens_source_idx(connector_id,created_at) index",
			"PRIMARY(id) unique",
		}; !slices.Equal(got, want) {
			t.Fatalf("foundry_event_source_tokens indexes = %v, want %v", got, want)
		}
	})

	// A failed deploy backs out by rolling the migration back, and the next
	// deploy applies it again.
	t.Run("rolls back and applies again", func(t *testing.T) {
		db := migrated(t)
		if err := goose.Down(db, "."); err != nil {
			t.Fatalf("roll back: %v", err)
		}
		if got := tables(t, db); len(got) != 0 {
			t.Fatalf("tables after rollback = %v, want none", got)
		}
		if err := goose.UpTo(db, ".", foundryEventSources); err != nil {
			t.Fatalf("apply again: %v", err)
		}
		if got, want := tables(t, db), []string{"foundry_event_batches", "foundry_event_source_tokens"}; !slices.Equal(got, want) {
			t.Fatalf("tables after reapply = %v, want %v", got, want)
		}
	})

	// seq order is commit order only if no seq repeats within a source; a
	// client token names one send of its source, and many sends carry none.
	t.Run("refuses a repeated seq or client token within a source", func(t *testing.T) {
		db := migrated(t)
		token := strings.Repeat("a", 32)
		for _, args := range [][]any{
			batch("b1", "c", 1, nil),
			batch("b2", "c", 2, nil),
			batch("b3", "c", 3, token),
			batch("b4", "other", 1, token),
			// ascii_bin: a token differing only in case is another token.
			batch("b5", "c", 4, strings.Repeat("A", 32)),
		} {
			if err := exec(t, db, insertBatch, args...); err != nil {
				t.Fatalf("insert %v: %v", args[0], err)
			}
		}
		if err := exec(t, db, insertBatch, batch("b6", "c", 2, nil)...); err == nil || !strings.Contains(err.Error(), "foundry_event_batches_seq_uq") {
			t.Fatalf("a repeated seq = %v, want the seq key to refuse it", err)
		}
		if err := exec(t, db, insertBatch, batch("b7", "c", 5, token)...); err == nil || !strings.Contains(err.Error(), "foundry_event_batches_token_uq") {
			t.Fatalf("a repeated client token = %v, want the token key to refuse it", err)
		}
	})

	// A token is found by its hash alone, so no two tokens may share one;
	// hashes are lowercase hex, compared byte for byte.
	t.Run("refuses a repeated token hash", func(t *testing.T) {
		db := migrated(t)
		hash := strings.Repeat("0a", 32)
		if err := exec(t, db, insertToken, "t1", hash); err != nil {
			t.Fatal(err)
		}
		if err := exec(t, db, insertToken, "t2", strings.ToUpper(hash)); err != nil {
			t.Fatalf("a hash differing in case: %v", err)
		}
		if err := exec(t, db, insertToken, "t3", hash); err == nil || !strings.Contains(err.Error(), "foundry_event_source_tokens_hash_uq") {
			t.Fatalf("a repeated hash = %v, want the hash key to refuse it", err)
		}
	})
}
