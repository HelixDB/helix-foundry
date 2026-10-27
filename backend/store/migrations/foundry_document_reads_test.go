package migrations

import (
	"bytes"
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestBackfillFoundryDocumentReads(t *testing.T) {
	ctx := context.Background()
	// uploads is a workspace's documents as the backfill meets them: every
	// type uploaded, a table file still waiting for its bytes, a deleted
	// one, one already read, and one of an ontology deleted before
	// documents were the workspace's.
	uploads := func(t *testing.T) *sql.DB {
		t.Helper()
		db := isolatedTestDB(t)
		if err := setup(db); err != nil {
			t.Fatal(err)
		}
		if err := goose.Up(db, "."); err != nil {
			t.Fatal(err)
		}
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := db.Exec(query, args...); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
		}
		exec(`INSERT INTO foundry_ontologies (id, workspace_id, name, project_id, tenant_id, client_token, deleted_at)
VALUES ('gone', 'w', 'Gone', 'p', 't', 'c', NOW(6))`)
		document := func(id, mediaType, status string, ontologyID any) {
			t.Helper()
			exec(`INSERT INTO foundry_documents (id, workspace_id, ontology_id, set_id, name, media_type, size_bytes, sha256, client_token, status, s3_key)
VALUES (?, 'w', ?, 's', 'f', ?, 1, ?, ?, ?, ?)`, id, ontologyID, mediaType, "sha-"+id, "c-"+id, status, "ws/w/documents/"+id+"/original")
		}
		for id, mediaType := range map[string]string{
			"md": "text/markdown", "mdx": "text/mdx", "txt": "text/plain", "json": "application/json", "csv": "text/csv",
			"tsv": "text/tab-separated-values", "jsonl": "application/x-ndjson", "parquet": "application/vnd.apache.parquet", "pdf": "application/pdf",
		} {
			document(id, mediaType, "uploaded", nil)
		}
		document("pending", "text/csv", "pending_upload", nil)
		document("deleted", "text/csv", "deleted", nil)
		document("read", "text/csv", "uploaded", nil)
		exec(`INSERT INTO foundry_document_reads (document_id, mode, workspace_id, status, row_count, artifact_key)
VALUES ('read', 'standard', 'w', 'succeeded', 3, 'ws/w/documents/read/read/rows.jsonl.gz')`)
		document("orphan", "text/csv", "uploaded", "gone")
		return db
	}
	reads := func(t *testing.T, db *sql.DB) []string {
		t.Helper()
		rows, err := db.Query(`SELECT CONCAT(document_id, ' ', status) FROM foundry_document_reads WHERE mode='standard' ORDER BY document_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}

	// Every uploaded table file and PDF with no read gets a queued one, so
	// the worker reads it; text is never read this way, and a read already
	// there keeps what it read.
	t.Run("queues a read for each unread table file and PDF", func(t *testing.T) {
		db := uploads(t)
		var out bytes.Buffer
		if err := BackfillFoundryDocumentReads(ctx, db, true, &out); err != nil {
			t.Fatal(err)
		}
		want := []string{"csv queued", "json queued", "jsonl queued", "parquet queued", "pdf queued", "read succeeded", "tsv queued"}
		if got := reads(t, db); !slices.Equal(got, want) {
			t.Fatalf("reads = %v, want %v", got, want)
		}
		if !strings.Contains(out.String(), "queued 6 document reads") {
			t.Fatalf("output = %q", out.String())
		}
	})

	// A dry run says what it would queue and writes nothing.
	t.Run("writes nothing on a dry run", func(t *testing.T) {
		db := uploads(t)
		var out bytes.Buffer
		if err := BackfillFoundryDocumentReads(ctx, db, false, &out); err != nil {
			t.Fatal(err)
		}
		if got := reads(t, db); !slices.Equal(got, []string{"read succeeded"}) {
			t.Fatalf("reads = %v", got)
		}
		if !strings.Contains(out.String(), "6 documents would get a queued read") {
			t.Fatalf("output = %q", out.String())
		}
	})

	// A step may run again: the second run finds nothing left to queue and
	// changes no read.
	t.Run("runs again without changing a read", func(t *testing.T) {
		db := uploads(t)
		if err := BackfillFoundryDocumentReads(ctx, db, true, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE foundry_document_reads SET status='running', attempts=1, claim_token='tok', claimed_until=NOW(6)
WHERE document_id='csv'`); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := BackfillFoundryDocumentReads(ctx, db, true, &out); err != nil {
			t.Fatal(err)
		}
		if got := reads(t, db); !slices.Contains(got, "csv running") || len(got) != 7 {
			t.Fatalf("reads = %v", got)
		}
		if !strings.Contains(out.String(), "queued 0 document reads") {
			t.Fatalf("output = %q", out.String())
		}
	})
}
