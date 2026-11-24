package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

const foundryHealFillColumns = `f.id, f.ontology_id, f.load_id, f.fingerprint, f.from_version_id, f.to_version_id, f.state, f.why,
       f.links_added, v.version`

const foundryHealFillFrom = ` FROM foundry_heal_fills f JOIN foundry_ontology_versions v ON v.id=f.to_version_id`

// A self-healing suggestion's "Add this link" moves the ontology's current
// load in place to the version that adds the link (contract 4.32.0's fold),
// when that version only adds: foundry_loads.version_id then names the
// version the load's marker records, not the one it was created with.
// foundry_heal_fills records each move, per load and suggestion. The engine
// moves the marker first, then Foundry moves version_id and the live jobs'
// version_id in one transaction (moveFoundryLoadVersion); a crash between
// leaves the marker ahead, which ReconcileFoundryLoadMarker puts right from
// the recorded row, never from a marker no row records.

// moveFoundryLoadVersion moves load loadID from version from to version to,
// with its jobs that are queued or running at from, so no attempt runs at a
// version the load no longer records: moved reports the load's row was at
// from. The caller holds the ontology's lock, or runs it in the transaction
// that read the fill row it acts on.
func (s *mysqlDB) moveFoundryLoadVersion(ctx context.Context, loadID, from, to string) (moved bool, err error) {
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_loads SET version_id=? WHERE id=? AND version_id=?`, to, loadID, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE foundry_load_jobs SET version_id=? WHERE load_id=? AND version_id=? AND status IN ('queued', 'running')`,
		to, loadID, from)
	return err == nil, err
}

// foundryMoveFailures is how many resend jobs may fail with a move owed
// before the move is given up as needing a load (failFoundryLoadMove).
const foundryMoveFailures = 3

// failFoundryLoadMove counts a failed resend job against the move its load
// owes, if any: the move stays owed, for the next job that ends to send
// (the fold is idempotent at its version), until foundryMoveFailures jobs
// have failed with it, when its fill row reads needs_load (why
// move_failed, the job's error) and the request ends. The caller holds the
// ontology's lock.
func (s *mysqlDB) failFoundryLoadMove(ctx context.Context, job *model.FoundryGraphJob) error {
	var owed bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_load_resends
WHERE load_id=? AND document_id='' AND reason=? AND status='owed')`, job.LoadID, string(model.FoundryResendMove)).Scan(&owed); err != nil || !owed {
		return err
	}
	why := ""
	if job.Error != nil {
		why = *job.Error
	}
	why = why[:min(len(why), 200)]
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET failures=failures+1, last_error=NULLIF(?, '')
WHERE load_id=? AND state='moving'`, why, job.LoadID); err != nil {
		return err
	}
	given, err := json.Marshal([]model.FoundryFoldWhy{{Kind: "move_failed", Item: why}})
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET state='needs_load', why=?
WHERE load_id=? AND state='moving' AND failures>=?`, string(given), job.LoadID, foundryMoveFailures)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	var moving bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_heal_fills WHERE load_id=? AND state='moving')`, job.LoadID).
		Scan(&moving); err != nil || moving {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE foundry_load_resends SET status='done', ended_at=NOW(6), job_id=?
WHERE load_id=? AND document_id='' AND reason=? AND status='owed'`, job.ID, job.LoadID, string(model.FoundryResendMove))
	return err
}

// ReconcileFoundryLoadMarker puts back load loadID whose marker records
// storedVersion (the engine's word, "foundry-v<N>", from a load_mismatch)
// where Foundry still records the version it moved from: only when a fill
// row of the load records that move (from its version now, to storedVersion),
// so it never adopts a marker no move of Foundry's wrote. The move's resend
// row stays owed: sent again, the engine says moved at the new version and
// what the load gained. reconciled reports the load moved here; the caller
// retries what met the mismatch once.
func (s *mysqlDB) ReconcileFoundryLoadMarker(ctx context.Context, loadID, storedVersion string) (reconciled bool, err error) {
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var workspaceID, ontologyID string
		if err := tx.db.QueryRowContext(ctx, `SELECT workspace_id, ontology_id FROM foundry_loads WHERE id=?`, loadID).
			Scan(&workspaceID, &ontologyID); err != nil {
			if err == sql.ErrNoRows {
				return nil
			}
			return err
		}
		if _, err := tx.lockLiveFoundryOntology(ctx, workspaceID, ontologyID); err != nil {
			if err == ErrNotFound {
				return nil
			}
			return err
		}
		var from, to string
		err := tx.db.QueryRowContext(ctx, `SELECT f.from_version_id, f.to_version_id FROM foundry_heal_fills f
JOIN foundry_loads l ON l.id=f.load_id AND l.version_id=f.from_version_id
JOIN foundry_ontology_versions v ON v.id=f.to_version_id
WHERE f.load_id=? AND f.state IN ('moving', 'moved') AND CONCAT('foundry-v', v.version)=?
ORDER BY f.created_at DESC, f.id DESC LIMIT 1`, loadID, storedVersion).Scan(&from, &to)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		reconciled, err = tx.moveFoundryLoadVersion(ctx, loadID, from, to)
		return err
	})
	return reconciled, err
}

// SetCurrentOntologyVersionFilling makes versionID the ontology's current
// version, as SetCurrentOntologyVersion does (expected is the version the
// caller saw current, nil for none; ErrConflict when another won), and then
// owes the current load a move in place to it for the suggestion
// fingerprint ("Add this link"): a fill row in state moving, a 'move' resend
// row, and a resend job (resend's id) queued behind the live job. A version
// already current is made current again, and the move owed. The load moves
// only if it is an agent draft's, ready or partial, and not at the version
// already; then Foundry's own checks: the version reads the same document
// sets and the same tables as the load's. One that fails them is recorded
// needs_load, why saying what, and nothing is owed; the engine judges the
// rest when the move is sent (FoundryFoldWhy). The same suggestion pressed
// again for the load replaces its row. The returned row is nil when there is
// no load to move.
func (s *mysqlDB) SetCurrentOntologyVersionFilling(ctx context.Context, workspaceID, ontologyID, versionID string, expected *string,
	fingerprint string, resend *model.FoundryGraphJob) (fill *model.FoundryHealFill, err error) {
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, ontologyID)
		if err != nil {
			if err == ErrNotFound {
				return ErrConflict
			}
			return err
		}
		live, err := tx.liveFoundryGraphJob(ctx, ontologyID)
		if err != nil {
			return err
		}
		if !(o.CurrentVersionID != nil && *o.CurrentVersionID == versionID && (expected == nil || *expected == versionID)) {
			if !sameVersion(o.CurrentVersionID, expected) {
				return ErrConflict
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ontologies SET current_version_id=? WHERE id=?`, versionID, ontologyID); err != nil {
				return err
			}
			o.CurrentVersionID = &versionID
			if err := tx.pruneFoundryReviewDecisions(ctx, ontologyID, versionID); err != nil {
				return fmt.Errorf("prune review decisions: %w", err)
			}
		}
		if o.CurrentLoadID == nil {
			return nil
		}
		load, err := tx.GetFoundryLoad(ctx, ontologyID, *o.CurrentLoadID)
		if err != nil {
			return err
		}
		if load.Status != model.FoundryLoadReady && load.Status != model.FoundryLoadPartial || load.VersionID == versionID {
			return nil
		}
		why, err := tx.foundryMoveRefusals(ctx, ontologyID, load.VersionID, versionID)
		if err != nil {
			return err
		}
		state, whyJSON := model.FoundryFillMoving, any(nil)
		if len(why) > 0 {
			state = model.FoundryFillNeedsLoad
			raw, err := json.Marshal(why)
			if err != nil {
				return err
			}
			whyJSON = string(raw)
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_heal_fills (id, ontology_id, load_id, fingerprint, from_version_id, to_version_id, state, why)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE from_version_id=VALUES(from_version_id), to_version_id=VALUES(to_version_id), state=VALUES(state), why=VALUES(why),
  links_added=NULL, stop_reason=NULL, active_key=NULL, last_error=NULL`,
			newID(), ontologyID, load.ID, fingerprint, load.VersionID, versionID, string(state), whyJSON); err != nil {
			return err
		}
		if fill, err = tx.foundryHealFill(ctx, load.ID, fingerprint); err != nil {
			return err
		}
		if state != model.FoundryFillMoving {
			return nil
		}
		if err := tx.oweFoundryResends(ctx, load.ID, model.FoundryResendMove, []string{""}); err != nil {
			return err
		}
		_, err = tx.queueFoundryResendBehind(ctx, o, live, resend)
		return err
	})
	return fill, err
}

// sameVersion reports whether the ontology's current version is the one
// expected (nil for none).
func sameVersion(current, expected *string) bool {
	if current == nil || expected == nil {
		return current == nil && expected == nil
	}
	return *current == *expected
}

// foundryMoveRefusals is what keeps a load of version from from moving to
// version to that Foundry can tell without the engine: the document sets
// the versions read, and the tables and table files their generations read.
// None when it may be sent.
func (s *mysqlDB) foundryMoveRefusals(ctx context.Context, ontologyID, from, to string) ([]model.FoundryFoldWhy, error) {
	var why []model.FoundryFoldWhy
	read := func(id string) (*model.OntologyVersion, *model.OntologyGeneration, error) {
		v, err := s.GetOntologyVersion(ctx, ontologyID, id)
		if err != nil {
			return nil, nil, err
		}
		if v.Format != model.OntologyFormatAgentBundle || v.GenerationID == nil {
			return v, nil, nil
		}
		g, err := s.GetOntologyGeneration(ctx, ontologyID, *v.GenerationID)
		if err == ErrNotFound {
			return v, nil, nil
		}
		return v, g, err
	}
	fv, fg, err := read(from)
	if err != nil {
		return nil, err
	}
	tv, tg, err := read(to)
	if err != nil {
		return nil, err
	}
	sets := func(v *model.OntologyVersion) []string {
		if v.DocumentSetIDs == nil {
			return nil
		}
		out := slices.Clone(*v.DocumentSetIDs)
		slices.Sort(out)
		return out
	}
	if tv.Format != model.OntologyFormatAgentBundle || tg == nil || fg == nil {
		why = append(why, model.FoundryFoldWhy{Kind: "not_a_draft", Item: "version"})
		return why, nil
	}
	if !slices.Equal(sets(fv), sets(tv)) {
		why = append(why, model.FoundryFoldWhy{Kind: "document_sets", Item: "document_set_ids"})
	}
	same := func(a, b any) (bool, error) {
		x, err := json.Marshal(a)
		if err != nil {
			return false, err
		}
		y, err := json.Marshal(b)
		return bytes.Equal(x, y), err
	}
	for _, c := range []struct {
		item string
		a, b any
	}{{"tables", fg.Input.Tables, tg.Input.Tables}, {"table_files", fg.Input.Documents, tg.Input.Documents}} {
		ok, err := same(c.a, c.b)
		if err != nil {
			return nil, err
		}
		if !ok {
			why = append(why, model.FoundryFoldWhy{Kind: "sources", Item: c.item})
		}
	}
	return why, nil
}

// foundryHealFill reads a load's fill row of a suggestion.
func (s *mysqlDB) foundryHealFill(ctx context.Context, loadID, fingerprint string) (*model.FoundryHealFill, error) {
	return scanFoundryHealFill(s.db.QueryRowContext(ctx, `SELECT `+foundryHealFillColumns+foundryHealFillFrom+`
WHERE f.load_id=? AND f.fingerprint=?`, loadID, fingerprint))
}

// FoundryLoadMoveOwed is the move a load owes (a 'move' resend row) and the
// fill row it is of: the newest moving one, nil when none is owed.
func (s *mysqlDB) FoundryLoadMoveOwed(ctx context.Context, loadID string) (*model.FoundryHealFill, error) {
	fill, err := scanFoundryHealFill(s.db.QueryRowContext(ctx, `SELECT `+foundryHealFillColumns+foundryHealFillFrom+`
JOIN foundry_load_resends w ON w.load_id=f.load_id AND w.document_id='' AND w.reason=? AND w.status='owed'
WHERE f.load_id=? AND f.state=? ORDER BY f.updated_at DESC, f.id DESC LIMIT 1`, string(model.FoundryResendMove), loadID, string(model.FoundryFillMoving)))
	if err == ErrNotFound {
		return nil, nil
	}
	return fill, err
}

// RecordFoundryLoadMove records what the engine said of the move of fill's
// load, while workerID holds job jobID's lease (ErrNotFound when it lost it,
// and nothing was written), in one transaction: moved, the load's row and
// its live jobs' (this one's too) at the new version
// (moveFoundryLoadVersion), and the links it gained; not additive, or a
// marker at a version no move of Foundry's records, needs_load with why.
// Either way the move's request ends, and so does any other moving row of
// the load from the same version: one move answers them. A move the load
// already made (its row at the new version, as after a reconcile) records
// the links alone.
func (s *mysqlDB) RecordFoundryLoadMove(ctx context.Context, jobID, workerID string, fill *model.FoundryHealFill, outcome model.FoundryFoldOutcome) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var loadID string
		if err := tx.db.QueryRowContext(ctx, `SELECT load_id FROM foundry_load_jobs`+ownedFoundryGraphJob+` FOR UPDATE`, jobID, workerID).
			Scan(&loadID); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if loadID != fill.LoadID {
			return fmt.Errorf("fill %s is of load %s, not the job's %s", fill.ID, fill.LoadID, loadID)
		}
		switch outcome.State {
		case model.FoundryFillMoved:
			if _, err := tx.moveFoundryLoadVersion(ctx, loadID, fill.FromVersionID, fill.ToVersionID); err != nil {
				return err
			}
			links, err := json.Marshal(outcome.LinksAdded)
			if err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET state='moved', links_added=?, why=NULL
WHERE load_id=? AND from_version_id=? AND to_version_id=? AND state='moving'`, string(links), loadID, fill.FromVersionID, fill.ToVersionID); err != nil {
				return err
			}
		case model.FoundryFillNeedsLoad:
			why, err := json.Marshal(outcome.Why)
			if err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET state='needs_load', why=?
WHERE load_id=? AND from_version_id=? AND to_version_id=? AND state='moving'`, string(why), loadID, fill.FromVersionID, fill.ToVersionID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("a move ends moved or needs_load, not %s", outcome.State)
		}
		// Another move the load owes (another suggestion, or a version made
		// current since) keeps its request owed for the next job.
		var others bool
		if err := tx.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_heal_fills WHERE load_id=? AND state='moving')`, loadID).
			Scan(&others); err != nil || others {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_resends SET status='done', ended_at=NOW(6), job_id=?
WHERE load_id=? AND document_id='' AND reason=? AND status='owed'`, jobID, loadID, string(model.FoundryResendMove))
		return err
	})
}

// FoundryAskedApart is what every Extract of a load of versionID asks apart
// (contract 4.32.0's asked_apart): the links each move in place along the
// version's chain added, a move to versionID and the moves to the versions
// those moved from, and so on back. Sorted, without repeats; none for a
// version no load moved to.
func (s *mysqlDB) FoundryAskedApart(ctx context.Context, ontologyID, versionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT from_version_id, to_version_id, links_added FROM foundry_heal_fills
WHERE ontology_id=? AND links_added IS NOT NULL`, ontologyID)
	if err != nil {
		return nil, err
	}
	type move struct{ from, links string }
	into := map[string][]move{}
	for rows.Next() {
		var from, to, links string
		if err := rows.Scan(&from, &to, &links); err != nil {
			_ = rows.Close()
			return nil, err
		}
		into[to] = append(into[to], move{from, links})
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	seen, queue := map[string]bool{versionID: true}, []string{versionID}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for _, m := range into[v] {
			var links []string
			if err := json.Unmarshal([]byte(m.links), &links); err != nil {
				return nil, fmt.Errorf("heal fill links of version %s: %w", v, err)
			}
			out = append(out, links...)
			if !seen[m.from] {
				seen[m.from] = true
				queue = append(queue, m.from)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// ListFoundryHealFills is a load's fill rows, by fingerprint.
func (s *mysqlDB) ListFoundryHealFills(ctx context.Context, loadID string) (map[string]*model.FoundryHealFill, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+foundryHealFillColumns+foundryHealFillFrom+` WHERE f.load_id=?`, loadID)
	if err != nil {
		return nil, err
	}
	fills, err := scanAll(rows, scanFoundryHealFill)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*model.FoundryHealFill, len(fills))
	for _, f := range fills {
		out[f.Fingerprint] = f
	}
	return out, nil
}

// carryFoundryHealFills brings loadID, as it becomes the ontology's current
// load in place of was, the moves was had under way or made from the version
// loadID is of: a rebuild or refresh queued at that version before the move,
// or a person's load of it. Each joins loadID's fills moving, owed a move
// there, so the new load moves too. A load made current with no fill rows
// of its own otherwise shows each suggestion as it stands. The caller holds
// the ontology's lock.
func (s *mysqlDB) carryFoundryHealFills(ctx context.Context, ontologyID, was, loadID string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT f.fingerprint, f.from_version_id, f.to_version_id FROM foundry_heal_fills f
JOIN foundry_loads l ON l.id=? AND l.version_id=f.from_version_id
WHERE f.load_id=? AND f.ontology_id=? AND f.state IN ('moving', 'moved')`, loadID, was, ontologyID)
	if err != nil {
		return err
	}
	type carried struct{ fingerprint, from, to string }
	var fills []carried
	for rows.Next() {
		var c carried
		if err := rows.Scan(&c.fingerprint, &c.from, &c.to); err != nil {
			_ = rows.Close()
			return err
		}
		fills = append(fills, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(fills) == 0 {
		return err
	}
	for _, c := range fills {
		if _, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_heal_fills (id, ontology_id, load_id, fingerprint, from_version_id, to_version_id, state)
VALUES (?, ?, ?, ?, ?, ?, 'moving')`, newID(), ontologyID, loadID, c.fingerprint, c.from, c.to); err != nil {
			return err
		}
	}
	return s.oweFoundryResends(ctx, loadID, model.FoundryResendMove, []string{""})
}

func scanFoundryHealFill(scanner rowScanner) (*model.FoundryHealFill, error) {
	var f model.FoundryHealFill
	var state string
	var why, links sql.NullString
	if err := scanner.Scan(&f.ID, &f.OntologyID, &f.LoadID, &f.Fingerprint, &f.FromVersionID, &f.ToVersionID, &state, &why, &links,
		&f.ToVersion); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	f.State = model.FoundryHealFillState(state)
	for _, col := range []struct {
		src sql.NullString
		dst any
	}{{why, &f.Why}, {links, &f.LinksAdded}} {
		if !col.src.Valid {
			continue
		}
		if err := json.Unmarshal([]byte(col.src.String), col.dst); err != nil {
			return nil, fmt.Errorf("heal fill %s: %w", f.ID, err)
		}
	}
	return &f, nil
}
