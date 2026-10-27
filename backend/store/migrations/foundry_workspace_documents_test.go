package migrations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pressly/goose/v3"
)

// originals is the documents bucket: the originals deleted, and the keys
// whose delete fails.
type originals struct {
	deleted []string
	failing map[string]bool
}

func (o *originals) Delete(_ context.Context, key string) error {
	if o.failing[key] {
		return errors.New("s3: access denied")
	}
	o.deleted = append(o.deleted, key)
	return nil
}

func TestWorkspaceFoundryDocuments(t *testing.T) {
	ctx := context.Background()
	// prodLike is a workspace as release 1 meets it in prod: two live
	// ontologies whose sets collide by name (General twice, Contracts and
	// Cóntracts, a set named as an ontology is), a long-named pair that only
	// fits truncated, a set of no ontology, legacy versions, and an ontology
	// deleted before release 1 with its sets and documents.
	prodLike := func(t *testing.T) *sql.DB {
		t.Helper()
		db := isolatedTestDB(t)
		if err := setup(db); err != nil {
			t.Fatal(err)
		}
		if err := goose.Up(db, "."); err != nil {
			t.Fatal(err)
		}
		hostWorkspaces(t, db)
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := db.Exec(query, args...); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
		}
		ontology := func(id, name string, deleted bool) {
			t.Helper()
			if deleted {
				exec(`INSERT INTO foundry_ontologies (id, workspace_id, name, project_id, tenant_id, client_token, deleted_at)
VALUES (?, 'w', ?, 'p', ?, ?, NOW(6))`, id, name, "t-"+id, "c-"+id)
				return
			}
			exec(`INSERT INTO foundry_ontologies (id, workspace_id, name, project_id, tenant_id, client_token, live_key)
VALUES (?, 'w', ?, 'p', ?, ?, 1)`, id, name, "t-"+id, "c-"+id)
		}
		n := 0
		set := func(id string, ontologyID any, name string) {
			t.Helper()
			n++
			exec(`INSERT INTO foundry_document_sets (id, workspace_id, ontology_id, name, live_key, created_at)
VALUES (?, 'w', ?, ?, 1, '2026-10-01' + INTERVAL ? SECOND)`, id, ontologyID, name, n)
		}
		document := func(id, ontologyID, setID, status string) {
			t.Helper()
			exec(`INSERT INTO foundry_documents (id, workspace_id, ontology_id, set_id, name, media_type, size_bytes, sha256, client_token, status, s3_key)
VALUES (?, 'w', ?, ?, 'a.md', 'text/markdown', 1, ?, ?, ?, ?)`, id, ontologyID, setID, "sha-"+id, "c-"+id, status, "ws/w/documents/"+id+"/original")
		}
		version := func(id, ontologyID string, number int, scope any) {
			t.Helper()
			exec(`INSERT INTO foundry_ontology_versions (id, workspace_id, ontology_id, version, format, definition, source, document_set_ids)
VALUES (?, 'w', ?, ?, 'agent_bundle', CAST('{}' AS JSON), 'generated', CAST(? AS JSON))`, id, ontologyID, number, scope)
		}
		longName := string([]rune(strings.Repeat("Ontologie générale ", 6))[:110])
		longSet := string([]rune(strings.Repeat("Contrats signés ", 7))[:100])
		ontology("oa", "Acme Support", false)
		ontology("ob", "Billing", false)
		ontology("oc", longName, false)
		ontology("od", "D", false)
		ontology("ox", "Gone", true)
		set("sa-general", "oa", "General")
		set("sa-contracts", "oa", "Contracts")
		set("sa-billing", "oa", "Billing")
		set("sa-notes", "oa", "Notes")
		set("sb-general", "ob", "General")
		set("sb-contracts", "ob", "Cóntracts")
		set("sc-long", "oc", longSet)
		set("sd-long", "od", strings.ToUpper(longSet))
		set("s-notes", nil, "Notes")
		set("sx-old", "ox", "General")
		set("sx-failing", "ox", "Failing")
		document("da", "oa", "sa-general", "uploaded")
		document("db", "ob", "sb-contracts", "uploaded")
		document("dx-uploaded", "ox", "sx-old", "uploaded")
		document("dx-pending", "ox", "sx-old", "pending_upload")
		document("dx-deleted", "ox", "sx-old", "deleted")
		document("dx-failing", "ox", "sx-failing", "uploaded")
		version("va-1", "oa", 1, nil)
		version("va-2", "oa", 2, "[]")
		version("vb-1", "ob", 1, `["sb-contracts"]`)
		version("vx-1", "ox", 1, nil)
		return db
	}
	type snapshot struct {
		sets      map[string]string
		live      map[string]bool
		owners    map[string]string
		documents map[string]string
		scopes    map[string]string
	}
	read := func(t *testing.T, db *sql.DB) snapshot {
		t.Helper()
		s := snapshot{sets: map[string]string{}, live: map[string]bool{}, owners: map[string]string{}, documents: map[string]string{},
			scopes: map[string]string{}}
		rows, err := db.Query(`SELECT id, name, live_key IS NOT NULL, COALESCE(ontology_id, '-') FROM foundry_document_sets`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, name, owner string
			var live bool
			if err := rows.Scan(&id, &name, &live, &owner); err != nil {
				t.Fatal(err)
			}
			s.sets[id], s.live[id], s.owners[id] = name, live, owner
		}
		_ = rows.Close()
		rows, err = db.Query(`SELECT id, status, COALESCE(ontology_id, '-') FROM foundry_documents`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, status, owner string
			if err := rows.Scan(&id, &status, &owner); err != nil {
				t.Fatal(err)
			}
			s.documents[id], s.owners[id] = status, owner
		}
		_ = rows.Close()
		rows, err = db.Query(`SELECT id, COALESCE(CAST(document_set_ids AS CHAR), 'NULL') FROM foundry_ontology_versions`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, scope string
			if err := rows.Scan(&id, &scope); err != nil {
				t.Fatal(err)
			}
			s.scopes[id] = scope
		}
		_ = rows.Close()
		return s
	}
	run := func(t *testing.T, db *sql.DB, store DocumentStore, apply bool) (workspaceDocumentsPlan, string, error) {
		t.Helper()
		var out bytes.Buffer
		err := WorkspaceFoundryDocuments(ctx, db, workspacesTable{}, store, apply, &out)
		var plan workspaceDocumentsPlan
		if decodeErr := json.NewDecoder(bytes.NewReader(out.Bytes())).Decode(&plan); decodeErr != nil && err == nil {
			t.Fatalf("plan %q: %v", out.String(), decodeErr)
		}
		return plan, out.String(), err
	}
	longName := string([]rune(strings.Repeat("Ontologie générale ", 6))[:110])
	longSet := string([]rune(strings.Repeat("Contrats signés ", 7))[:100])

	// A dry run says what an apply would do and writes and deletes nothing.
	t.Run("plans without writing", func(t *testing.T) {
		db := prodLike(t)
		before := read(t, db)
		store := &originals{}
		plan, out, err := run(t, db, store, false)
		if err != nil {
			t.Fatal(err)
		}
		if after := read(t, db); fmt.Sprint(after) != fmt.Sprint(before) || len(store.deleted) != 0 {
			t.Fatalf("a dry run changed %v into %v, deleted %v", before, after, store.deleted)
		}
		if plan.Apply || len(plan.RemovedDocuments) != 3 || len(plan.RemovedSets) != 2 || len(plan.RenamedSets) != 7 ||
			len(plan.FilledScopes) != 2 || !strings.Contains(out, "re-run with --apply") {
			t.Fatalf("plan = %+v\n%s", plan, out)
		}
	})

	// The apply deletes each original before its row, renames collisions in
	// the column's collation, fills legacy scopes with what they read, and
	// leaves no row naming an ontology; a failed delete keeps its document
	// and set, and fails the run, which a re-run finishes. Then it is done.
	t.Run("applies, retries a failed delete, then does nothing", func(t *testing.T) {
		db := prodLike(t)
		store := &originals{failing: map[string]bool{"ws/w/documents/dx-failing/original": true}}
		plan, out, err := run(t, db, store, true)
		if err == nil || !strings.Contains(err.Error(), "1 originals could not be deleted") {
			t.Fatalf("a failed delete: %v\n%s", err, out)
		}
		if strings.Join(store.deleted, ",") != "ws/w/documents/dx-pending/original,ws/w/documents/dx-uploaded/original" {
			t.Fatalf("deleted %v", store.deleted)
		}
		got := read(t, db)
		for id, want := range map[string]string{"dx-uploaded": "deleted", "dx-pending": "deleted", "dx-deleted": "deleted", "dx-failing": "uploaded",
			"da": "uploaded", "db": "uploaded"} {
			if got.documents[id] != want {
				t.Fatalf("document %s is %s, want %s", id, got.documents[id], want)
			}
		}
		if got.live["sx-old"] || !got.live["sx-failing"] || got.owners["sx-failing"] != "ox" || got.owners["dx-failing"] != "ox" {
			t.Fatalf("the deleted ontology's rows: %+v", got)
		}
		wantNames := map[string]string{
			"sa-general":   "Acme Support",
			"sb-general":   "Billing 2",
			"sa-billing":   "Billing",
			"sa-contracts": "Contracts (Acme Support)",
			"sb-contracts": "Cóntracts (Billing)",
			"sa-notes":     "Notes (Acme Support)",
			"s-notes":      "Notes",
		}
		for id, want := range wantNames {
			if got.sets[id] != want {
				t.Fatalf("set %s is %q, want %q", id, got.sets[id], want)
			}
		}
		for _, id := range []string{"sc-long", "sd-long"} {
			name := got.sets[id]
			if utf8.RuneCountInString(name) > 120 || !strings.HasSuffix(name, ")") || strings.Contains(name, " )") {
				t.Fatalf("set %s is %q (%d characters)", id, name, utf8.RuneCountInString(name))
			}
		}
		// The set's own name gives way to its ontology's.
		if got.sets["sc-long"] == got.sets["sd-long"] || !strings.HasPrefix(got.sets["sc-long"], "Contrat (") ||
			!strings.Contains(got.sets["sc-long"], strings.TrimSpace(longName)) || !strings.HasSuffix(got.sets["sd-long"], " (D)") ||
			!strings.HasPrefix(got.sets["sd-long"], strings.ToUpper(longSet[:10])) {
			t.Fatalf("long names: %q, %q", got.sets["sc-long"], got.sets["sd-long"])
		}
		if got.scopes["va-1"] != `["sa-general", "sa-contracts", "sa-billing", "sa-notes"]` || got.scopes["va-2"] != "[]" ||
			got.scopes["vb-1"] != `["sb-contracts"]` || got.scopes["vx-1"] != "[]" {
			t.Fatalf("scopes = %v", got.scopes)
		}
		for id, owner := range got.owners {
			if owner != "-" && id != "sx-failing" && id != "dx-failing" {
				t.Fatalf("%s still names %s", id, owner)
			}
		}
		if !plan.Apply || plan.ReleasedSets != 9 || plan.ReleasedDocuments != 5 {
			t.Fatalf("plan = %+v", plan)
		}
		var collisions int
		if err := db.QueryRow(`SELECT COUNT(*) FROM (SELECT workspace_id, name FROM foundry_document_sets WHERE live_key=1
GROUP BY workspace_id, name HAVING COUNT(*) > 1) c`).Scan(&collisions); err != nil || collisions != 0 {
			t.Fatalf("%d names still collide (%v)", collisions, err)
		}

		// S3 lets the delete through now: the re-run finishes it.
		store.failing = nil
		plan, _, err = run(t, db, store, true)
		if err != nil || len(plan.RemovedDocuments) != 1 || plan.RemovedDocuments[0].DocumentID != "dx-failing" || len(plan.RemovedSets) != 1 ||
			len(plan.RenamedSets) != 0 || len(plan.FilledScopes) != 0 || plan.ReleasedSets != 1 || plan.ReleasedDocuments != 1 {
			t.Fatalf("the retry = %+v (%v)", plan, err)
		}
		got = read(t, db)
		if got.documents["dx-failing"] != "deleted" || got.live["sx-failing"] || got.owners["dx-failing"] != "-" {
			t.Fatalf("after the retry: %+v", got)
		}

		// Done: nothing left to do.
		before := read(t, db)
		plan, _, err = run(t, db, store, true)
		if err != nil || len(plan.RemovedDocuments)+len(plan.RemovedSets)+len(plan.RenamedSets)+len(plan.FilledScopes) != 0 ||
			plan.ReleasedSets+plan.ReleasedDocuments != 0 || fmt.Sprint(read(t, db)) != fmt.Sprint(before) {
			t.Fatalf("a third run = %+v (%v)", plan, err)
		}
	})

	// An original is deleted only where the backend's role may delete one;
	// a row naming any other key is kept and reported, never deleted.
	t.Run("deletes only a document's original", func(t *testing.T) {
		db := prodLike(t)
		if _, err := db.Exec(`UPDATE foundry_documents SET s3_key='ws/w/other/thing' WHERE id='dx-uploaded'`); err != nil {
			t.Fatal(err)
		}
		store := &originals{}
		plan, _, err := run(t, db, store, true)
		if err == nil {
			t.Fatal("a key that is no original passed")
		}
		for _, d := range plan.RemovedDocuments {
			if d.DocumentID == "dx-uploaded" && !strings.Contains(d.Error, "not the document's original") {
				t.Fatalf("plan entry %+v", d)
			}
		}
		for _, key := range store.deleted {
			if key == "ws/w/other/thing" {
				t.Fatal("deleted a key that is no original")
			}
		}
	})

	// With no deleted ontology anywhere, every row is released all the same.
	t.Run("releases every row when no ontology was deleted", func(t *testing.T) {
		db := prodLike(t)
		for _, q := range []string{`DELETE FROM foundry_documents WHERE ontology_id='ox'`, `DELETE FROM foundry_document_sets WHERE ontology_id='ox'`,
			`DELETE FROM foundry_ontology_versions WHERE ontology_id='ox'`, `DELETE FROM foundry_ontologies WHERE id='ox'`} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		plan, _, err := run(t, db, &originals{}, true)
		if err != nil {
			t.Fatal(err)
		}
		if plan.ReleasedSets != 8 || plan.ReleasedDocuments != 2 {
			t.Fatalf("released %d sets and %d documents", plan.ReleasedSets, plan.ReleasedDocuments)
		}
		for id, owner := range read(t, db).owners {
			if owner != "-" {
				t.Fatalf("%s still names %s", id, owner)
			}
		}
	})

	// Without a bucket the originals can't be deleted: the apply refuses
	// before it writes anything; a dry run still plans.
	t.Run("refuses to apply without a bucket", func(t *testing.T) {
		db := prodLike(t)
		before := read(t, db)
		if _, _, err := run(t, db, nil, true); err == nil || !strings.Contains(err.Error(), "no Foundry bucket") {
			t.Fatalf("apply without a bucket: %v", err)
		}
		if fmt.Sprint(read(t, db)) != fmt.Sprint(before) {
			t.Fatal("it wrote")
		}
		if _, _, err := run(t, db, nil, false); err != nil {
			t.Fatalf("a dry run without a bucket: %v", err)
		}
	})
}

// Names are fit to VARCHAR(120): the set's own name gives way first, then
// the ontology's, the suffix always stays, and no part ends in a space.
func TestFoundrySetNameWithin(t *testing.T) {
	long := strings.Repeat("é", 200)
	for _, tc := range []struct {
		name, ontology string
		general        bool
		n              int
		want           string
	}{
		{"General", "Acme", true, 1, "Acme"},
		{"General", "Acme", true, 3, "Acme 3"},
		{"Contracts", "Acme", false, 1, "Contracts (Acme)"},
		{"Contracts", "Acme", false, 2, "Contracts (Acme) 2"},
		{"Notes", "", false, 2, "Notes 2"},
		{"Notes", "", false, 1, "Notes"},
		{"General", long, true, 12, strings.Repeat("é", 117) + " 12"},
		{long, "Acme", false, 1, strings.Repeat("é", 113) + " (Acme)"},
		{"Contracts", long, false, 2, "C (" + strings.Repeat("é", 114) + ") 2"},
		{strings.Repeat("ab ", 50), "Acme", false, 1, strings.TrimSpace(strings.Repeat("ab ", 50)[:113]) + " (Acme)"},
	} {
		got := foundrySetNameWithin(tc.name, tc.ontology, tc.general, tc.n)
		if got != tc.want || utf8.RuneCountInString(got) > 120 {
			t.Errorf("foundrySetNameWithin(%.20q, %.20q, %t, %d) = %q (%d characters), want %q", tc.name, tc.ontology, tc.general, tc.n, got,
				utf8.RuneCountInString(got), tc.want)
		}
	}
}
