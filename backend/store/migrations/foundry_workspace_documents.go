package migrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"io"
	"strings"
	"unicode"

	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/store"
)

// DocumentStore deletes a Foundry document's original (document.Store).
type DocumentStore interface {
	Delete(ctx context.Context, key string) error
}

// maxFoundrySetNameChars is foundry_document_sets.name's VARCHAR(120).
const maxFoundrySetNameChars = 120

// workspaceDocumentsPlan is what WorkspaceFoundryDocuments did, or with
// apply false would do.
type workspaceDocumentsPlan struct {
	Apply bool `json:"apply"`
	// RemovedDocuments are the documents of deleted ontologies: the original
	// is deleted from S3, then the row is marked deleted.
	RemovedDocuments []removedFoundryDocument `json:"removed_documents"`
	// RemovedSets are the live sets of deleted ontologies, marked deleted
	// unless a document of theirs could not be removed (Kept).
	RemovedSets []removedFoundrySet `json:"removed_sets"`
	// RenamedSets are live sets whose name another live set of the
	// workspace has, in the column's collation.
	RenamedSets []renamedFoundrySet `json:"renamed_sets"`
	// FilledScopes are legacy versions (NULL document_set_ids), given their
	// ontology's live sets: what they read until now.
	FilledScopes []filledFoundryScope `json:"filled_scopes"`
	// ReleasedSets and ReleasedDocuments count the rows whose ontology_id is
	// nulled: they are the workspace's.
	ReleasedSets      int64 `json:"released_sets"`
	ReleasedDocuments int64 `json:"released_documents"`
}

type removedFoundryDocument struct {
	WorkspaceID string `json:"workspace_id"`
	OntologyID  string `json:"ontology_id"`
	DocumentID  string `json:"document_id"`
	Status      string `json:"status"`
	// Error is why its original could not be deleted; the row is kept, and a
	// re-run retries it.
	Error string `json:"error,omitempty"`
}

type removedFoundrySet struct {
	WorkspaceID string `json:"workspace_id"`
	OntologyID  string `json:"ontology_id"`
	SetID       string `json:"set_id"`
	Name        string `json:"name"`
	// Kept is a set left live: a document of it could not be removed.
	Kept bool `json:"kept,omitempty"`
}

type renamedFoundrySet struct {
	WorkspaceID string `json:"workspace_id"`
	SetID       string `json:"set_id"`
	OntologyID  string `json:"ontology_id,omitempty"`
	From        string `json:"from"`
	To          string `json:"to"`
}

type filledFoundryScope struct {
	OntologyID     string   `json:"ontology_id"`
	VersionID      string   `json:"version_id"`
	DocumentSetIDs []string `json:"document_set_ids"`
}

// WorkspaceFoundryDocuments moves Foundry's documents and document sets from
// their ontologies onto their workspace (release 1 of the three that move
// them). It runs after release 1's binary is
// out, which already reads and writes them per workspace and hides the rows
// of deleted ontologies, and again later. In order:
//  1. The documents of deleted ontologies: each original is deleted from S3
//     first (documents), then its row is marked deleted, so a re-run retries
//     a delete that failed. Then their live sets with no document left are
//     marked deleted.
//  2. Live sets whose name another live set of the workspace has, as
//     GROUP BY finds them in the column's collation, are renamed: "General"
//     to its ontology's name, any other "<name> (<ontology name>)", with
//     " 2", " 3", … while the name is still taken, fit to the column's 120
//     characters. A set of no ontology keeps its name if it is free once the
//     others are renamed. Each workspace is renamed under its lock, which
//     creating or renaming a set takes too.
//  3. Legacy versions (NULL scope) get their ontology's live sets, which is
//     what release 1 reads for them; a deleted ontology's get none.
//  4. ontology_id is nulled on the remaining sets and documents: all but the
//     live ones of deleted ontologies that step 1 could not remove.
//
// Everything but the S3 deletes runs in one transaction, rolled back when
// apply is false: a dry run deletes and writes nothing, and reports what an
// apply would do if every delete succeeded. The plan is printed as JSON. A
// re-run once it is done does nothing. A failed S3 delete is reported, and
// fails the run once the rest is done.
//
// Run it once every pod runs release 1: an older binary still writes
// versions with no scope and rows naming their ontology. The transaction
// locks the rows it changes across every workspace, a dry run's included,
// so a request that needs one of them waits, and one of the two may lose a
// deadlock and fail (the step can simply run again); run it when Foundry is
// quiet.
func WorkspaceFoundryDocuments(ctx context.Context, db *sql.DB, workspaces store.Workspaces, documents DocumentStore, apply bool, out io.Writer) error {
	plan := workspaceDocumentsPlan{Apply: apply, RemovedDocuments: []removedFoundryDocument{}, RemovedSets: []removedFoundrySet{},
		RenamedSets: []renamedFoundrySet{}, FilledScopes: []filledFoundryScope{}}
	failed, err := deleteDeletedOntologyOriginals(ctx, db, documents, apply, &plan)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // after a commit, a no-op
	for _, step := range []func(context.Context, *sql.Tx, *workspaceDocumentsPlan) error{
		removeDeletedOntologyRows,
		func(ctx context.Context, tx *sql.Tx, plan *workspaceDocumentsPlan) error {
			return renameCollidingFoundrySets(ctx, tx, workspaces, plan)
		},
		fillLegacyFoundryScopes, releaseFoundryDocuments,
	} {
		if err := step(ctx, tx, &plan); err != nil {
			return err
		}
	}
	if apply {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(plan); err != nil {
		return fmt.Errorf("print plan: %w", err)
	}
	verb := "would remove"
	if apply {
		verb = "removed"
	}
	removedSets := 0
	for _, set := range plan.RemovedSets {
		if !set.Kept {
			removedSets++
		}
	}
	fmt.Fprintf(out, "%s %d documents and %d sets of deleted ontologies, renamed %d sets, filled %d scopes, released %d sets and %d documents",
		verb, len(plan.RemovedDocuments)-failed, removedSets, len(plan.RenamedSets), len(plan.FilledScopes), plan.ReleasedSets,
		plan.ReleasedDocuments)
	if !apply {
		fmt.Fprint(out, "; re-run with --apply to write")
	}
	fmt.Fprintln(out)
	if failed > 0 {
		return fmt.Errorf("%d originals could not be deleted; their documents are kept: run the step again", failed)
	}
	return nil
}

// deleteDeletedOntologyOriginals starts step 1: it lists the documents of
// deleted ontologies in the plan and, with apply, deletes each one's
// original. It returns how many it could not delete, which keep an Error.
func deleteDeletedOntologyOriginals(ctx context.Context, db *sql.DB, documents DocumentStore, apply bool, plan *workspaceDocumentsPlan) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT d.workspace_id, d.ontology_id, d.id, d.status, d.s3_key FROM foundry_documents d
JOIN foundry_ontologies o ON o.id = d.ontology_id AND o.deleted_at IS NOT NULL
WHERE d.status <> 'deleted' ORDER BY d.workspace_id, d.id`)
	if err != nil {
		return 0, fmt.Errorf("find the documents of deleted ontologies: %w", err)
	}
	var keys []string
	for rows.Next() {
		var d removedFoundryDocument
		var key string
		if err := rows.Scan(&d.WorkspaceID, &d.OntologyID, &d.DocumentID, &d.Status, &key); err != nil {
			_ = rows.Close()
			return 0, err
		}
		plan.RemovedDocuments = append(plan.RemovedDocuments, d)
		keys = append(keys, key)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if apply && len(keys) > 0 && documents == nil {
		return 0, errors.New("documents of deleted ontologies need their originals deleted, and this deployment has no Foundry bucket")
	}
	failed := 0
	for i := range plan.RemovedDocuments {
		d := &plan.RemovedDocuments[i]
		switch {
		case keys[i] != document.Key(d.WorkspaceID, d.DocumentID):
			// Only an original: the backend's role may delete nothing else.
			d.Error = fmt.Sprintf("s3_key %q is not the document's original", keys[i])
		case !apply:
			continue
		default:
			if err := documents.Delete(ctx, keys[i]); err != nil {
				d.Error = err.Error()
			}
		}
		if d.Error != "" {
			failed++
		}
	}
	return failed, nil
}

// removeDeletedOntologyRows ends step 1: the documents whose originals are
// gone are marked deleted, then the sets of deleted ontologies left with no
// document that is not.
func removeDeletedOntologyRows(ctx context.Context, tx *sql.Tx, plan *workspaceDocumentsPlan) error {
	for _, d := range plan.RemovedDocuments {
		if d.Error != "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE foundry_documents SET status='deleted', deleted_at=NOW(6) WHERE id=? AND status<>'deleted'`,
			d.DocumentID); err != nil {
			return fmt.Errorf("mark document %s deleted: %w", d.DocumentID, err)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.workspace_id, s.ontology_id, s.id, s.name FROM foundry_document_sets s
JOIN foundry_ontologies o ON o.id = s.ontology_id AND o.deleted_at IS NOT NULL
WHERE s.live_key = 1 ORDER BY s.workspace_id, s.id`)
	if err != nil {
		return fmt.Errorf("find the sets of deleted ontologies: %w", err)
	}
	for rows.Next() {
		var set removedFoundrySet
		if err := rows.Scan(&set.WorkspaceID, &set.OntologyID, &set.SetID, &set.Name); err != nil {
			_ = rows.Close()
			return err
		}
		plan.RemovedSets = append(plan.RemovedSets, set)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range plan.RemovedSets {
		set := &plan.RemovedSets[i]
		var left int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_documents WHERE set_id=? AND status<>'deleted'`, set.SetID).
			Scan(&left); err != nil {
			return err
		}
		if set.Kept = left > 0; set.Kept {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE foundry_document_sets SET deleted_at=NOW(6), live_key=NULL WHERE id=? AND live_key=1`,
			set.SetID); err != nil {
			return fmt.Errorf("mark set %s deleted: %w", set.SetID, err)
		}
	}
	return nil
}

// renameCollidingFoundrySets is step 2.
func renameCollidingFoundrySets(ctx context.Context, tx *sql.Tx, workspaces store.Workspaces, plan *workspaceDocumentsPlan) error {
	const live = `s.live_key = 1 AND NOT EXISTS (SELECT 1 FROM foundry_ontologies gone WHERE gone.id = s.ontology_id AND gone.deleted_at IS NOT NULL)`
	rows, err := tx.QueryContext(ctx, `SELECT s.workspace_id, s.name FROM foundry_document_sets s WHERE `+live+`
GROUP BY s.workspace_id, s.name HAVING COUNT(*) > 1 ORDER BY s.workspace_id`)
	if err != nil {
		return fmt.Errorf("find colliding set names: %w", err)
	}
	type collision struct{ workspaceID, name string }
	var collisions []collision
	for rows.Next() {
		var c collision
		if err := rows.Scan(&c.workspaceID, &c.name); err != nil {
			_ = rows.Close()
			return err
		}
		collisions = append(collisions, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range collisions {
		// Creating or renaming a set checks names under this lock.
		var locked string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM `+workspaces.Table()+` WHERE id=? FOR UPDATE`, c.workspaceID).Scan(&locked); err != nil &&
			!errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock workspace %s: %w", c.workspaceID, err)
		}
		// Sets of an ontology are renamed first; one of none may then keep its
		// name. A locking read: it sees a rename that committed after this
		// transaction's snapshot, before the lock, and holds the sets until
		// the commit. A locking clause locks only the outer query's rows, so
		// the ontology's name is a subquery; Vitess cannot parse FOR UPDATE OF.
		rows, err := tx.QueryContext(ctx, `SELECT s.id, s.ontology_id, (SELECT o.name FROM foundry_ontologies o WHERE o.id = s.ontology_id),
  s.name, s.name = ? FROM foundry_document_sets s
WHERE s.workspace_id = ? AND s.name = ? AND `+live+` ORDER BY s.ontology_id IS NULL, s.created_at, s.id FOR UPDATE`,
			model.FoundryGeneralSetName, c.workspaceID, c.name)
		if err != nil {
			return fmt.Errorf("read colliding sets of workspace %s: %w", c.workspaceID, err)
		}
		type member struct {
			id, name                 string
			ontologyID, ontologyName sql.NullString
			general                  bool
		}
		var members []member
		for rows.Next() {
			var m member
			if err := rows.Scan(&m.id, &m.ontologyID, &m.ontologyName, &m.name, &m.general); err != nil {
				_ = rows.Close()
				return err
			}
			members = append(members, m)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(members) < 2 {
			continue // renamed apart since the GROUP BY read
		}
		for _, m := range members {
			var to string
			for n := 1; ; n++ {
				to = foundrySetNameWithin(m.name, m.ontologyName.String, m.general, n)
				// A locking read: it sees what committed since this transaction's first read.
				var taken int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_document_sets s
WHERE s.workspace_id = ? AND s.name = ? AND s.id <> ? AND `+live+` FOR SHARE`, c.workspaceID, to, m.id).Scan(&taken); err != nil {
					return err
				}
				if taken == 0 {
					break
				}
			}
			if to == m.name {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE foundry_document_sets SET name=? WHERE id=?`, to, m.id); err != nil {
				return fmt.Errorf("rename set %s: %w", m.id, err)
			}
			plan.RenamedSets = append(plan.RenamedSets, renamedFoundrySet{
				WorkspaceID: c.workspaceID, SetID: m.id, OntologyID: m.ontologyID.String, From: m.name, To: to,
			})
		}
	}
	return nil
}

// foundrySetNameWithin is the n-th name step 2 tries for a set named name of
// the ontology named ontology (empty for none): its ontology's name for a
// General set, "<name> (<ontology>)" for another, its own name for a set of
// no ontology; " <n>" after it from the second. It fits 120 characters: the
// set's own name gives way first, then the ontology's, and no part ends in
// a space.
func foundrySetNameWithin(name, ontology string, general bool, n int) string {
	suffix := ""
	if n > 1 {
		suffix = fmt.Sprintf(" %d", n)
	}
	room := maxFoundrySetNameChars - len([]rune(suffix))
	fit := func(s string, chars int) string {
		r := []rune(s)
		if len(r) > chars {
			r = r[:max(chars, 0)]
		}
		return strings.TrimRightFunc(string(r), unicode.IsSpace)
	}
	switch {
	case ontology == "":
		return fit(name, room) + suffix
	case general:
		return fit(ontology, room) + suffix
	}
	// At least one character of the set's own name stays.
	ontology = fit(ontology, room-len([]rune(" ()"))-1)
	tail := " (" + ontology + ")"
	return fit(name, room-len([]rune(tail))) + tail + suffix
}

// fillLegacyFoundryScopes is step 3.
func fillLegacyFoundryScopes(ctx context.Context, tx *sql.Tx, plan *workspaceDocumentsPlan) error {
	rows, err := tx.QueryContext(ctx, `SELECT v.ontology_id, v.id, o.id IS NULL OR o.deleted_at IS NOT NULL FROM foundry_ontology_versions v
LEFT JOIN foundry_ontologies o ON o.id = v.ontology_id
WHERE v.document_set_ids IS NULL ORDER BY v.ontology_id, v.version`)
	if err != nil {
		return fmt.Errorf("find legacy versions: %w", err)
	}
	type legacy struct {
		fill    filledFoundryScope
		deleted bool
	}
	var versions []legacy
	for rows.Next() {
		var v legacy
		if err := rows.Scan(&v.fill.OntologyID, &v.fill.VersionID, &v.deleted); err != nil {
			_ = rows.Close()
			return err
		}
		versions = append(versions, v)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sets := map[string][]string{}
	for _, v := range versions {
		scope, read := sets[v.fill.OntologyID]
		if !read && !v.deleted {
			rows, err := tx.QueryContext(ctx, `SELECT id FROM foundry_document_sets WHERE ontology_id=? AND live_key=1 ORDER BY created_at, id`,
				v.fill.OntologyID)
			if err != nil {
				return fmt.Errorf("read the sets of ontology %s: %w", v.fill.OntologyID, err)
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return err
				}
				scope = append(scope, id)
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		if scope == nil {
			scope = []string{}
		}
		sets[v.fill.OntologyID] = scope
		stored, err := json.Marshal(scope)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE foundry_ontology_versions SET document_set_ids=CAST(? AS JSON)
WHERE id=? AND document_set_ids IS NULL`, string(stored), v.fill.VersionID); err != nil {
			return fmt.Errorf("fill the scope of version %s: %w", v.fill.VersionID, err)
		}
		v.fill.DocumentSetIDs = scope
		plan.FilledScopes = append(plan.FilledScopes, v.fill)
	}
	return nil
}

// releaseFoundryDocuments is step 4. The deleted ontologies are read first
// and named in the updates, so no update reads, and share-locks, their rows.
func releaseFoundryDocuments(ctx context.Context, tx *sql.Tx, plan *workspaceDocumentsPlan) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM foundry_ontologies WHERE deleted_at IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("read deleted ontologies: %w", err)
	}
	var deleted []any
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		deleted = append(deleted, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Rows of a deleted ontology stay its own while they are live: step 1
	// could not remove them, and a re-run retries. With no deleted ontology,
	// every row is released.
	notDeleted := " OR TRUE"
	if len(deleted) > 0 {
		notDeleted = " OR ontology_id NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(deleted)), ",") + ")"
	}
	res, err := tx.ExecContext(ctx, `UPDATE foundry_document_sets SET ontology_id=NULL
WHERE ontology_id IS NOT NULL AND (live_key IS NULL`+notDeleted+`)`, deleted...)
	if err != nil {
		return fmt.Errorf("release sets: %w", err)
	}
	if plan.ReleasedSets, err = res.RowsAffected(); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE foundry_documents SET ontology_id=NULL
WHERE ontology_id IS NOT NULL AND (status='deleted'`+notDeleted+`)`, deleted...)
	if err != nil {
		return fmt.Errorf("release documents: %w", err)
	}
	plan.ReleasedDocuments, err = res.RowsAffected()
	return err
}
