package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

const foundryHealFillColumns = `f.id, f.ontology_id, f.load_id, f.fingerprint, f.from_version_id, f.to_version_id, f.state, f.why,
       f.links_added, v.version, f.stop_reason, f.docs_owed, f.docs_left, f.docs_filled, f.docs_nothing, f.docs_refused, f.docs_native,
       f.reviews_taken, f.facts_added, f.spent_usd, f.cap_usd, f.paused_until, f.failures`

const foundryHealFillFrom = ` FROM foundry_heal_fills f JOIN foundry_ontology_versions v ON v.id=f.to_version_id`

// A self-healing suggestion's "Add this link" moves the ontology's current
// load in place to the version that adds the link (contract 4.33.0's fold),
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

// failFoundryLoadMove counts a failed resend job that sent its load's move
// (moveSent, model.FoundryGraphJobOutcome.MoveSent) against that move, if
// still owed: one that failed before it reached the move counts nothing. The
// move stays owed and backs off (paused_until, by
// model.FoundryFillBackoff), after which the heal worker queues a resend for
// it (DueFoundryHealFills, ResumeFoundryFill), as a job's end queues none for
// a move alone; the fold is idempotent at its version. Once
// foundryMoveFailures jobs have failed with it, its fill row reads needs_load
// (why move_failed, the job's error) and the request ends. The caller holds
// the ontology's lock.
func (s *mysqlDB) failFoundryLoadMove(ctx context.Context, job *model.FoundryGraphJob, moveSent bool) error {
	if !moveSent {
		return nil
	}
	var owed bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_load_resends
WHERE load_id=? AND document_id='' AND reason=? AND status='owed')`, job.LoadID, string(model.FoundryResendMove)).Scan(&owed); err != nil || !owed {
		return err
	}
	why := ""
	if job.Error != nil {
		why = *job.Error
	}
	// Cut on a character's edge: a cut through one is no valid UTF-8, which
	// MySQL refuses.
	for len(why) > 200 || !utf8.ValidString(why) {
		why = why[:min(len(why), 200)]
		for !utf8.ValidString(why) {
			why = why[:len(why)-1]
		}
	}
	// MySQL assigns left to right: the backoff reads the count just raised.
	b := model.FoundryFillBackoff
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET failures=failures+1, last_error=NULLIF(?, ''),
  paused_until=NOW(6) + INTERVAL (CASE failures WHEN 1 THEN ? WHEN 2 THEN ? ELSE ? END) MICROSECOND
WHERE load_id=? AND state='moving'`, why, b[0].Microseconds(), b[1].Microseconds(), b[2].Microseconds(), job.LoadID); err != nil {
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
// in whatever state, so it never adopts a marker no move of Foundry's wrote,
// and a move given up on (needs_load) after its marker moved is still put
// right. Such a row is moving again, its move owed: sent again, the engine
// says moved at the new version and what the load gained, which records the
// links the load's Extracts ask apart. reconciled reports the load moved here;
// the caller retries what met the mismatch once.
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
		var id, from, to, state string
		err := tx.db.QueryRowContext(ctx, `SELECT f.id, f.from_version_id, f.to_version_id, f.state FROM foundry_heal_fills f
JOIN foundry_loads l ON l.id=f.load_id AND l.version_id=f.from_version_id
JOIN foundry_ontology_versions v ON v.id=f.to_version_id
WHERE f.load_id=? AND CONCAT('foundry-v', v.version)=?
ORDER BY f.created_at DESC, f.id DESC LIMIT 1`, loadID, storedVersion).Scan(&id, &from, &to, &state)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if reconciled, err = tx.moveFoundryLoadVersion(ctx, loadID, from, to); err != nil || !reconciled {
			return err
		}
		if state == string(model.FoundryFillMoving) || state == string(model.FoundryFillMoved) {
			return nil
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET state='moving', why=NULL, failures=0, stop_reason=NULL, active_key=NULL,
  paused_until=NULL WHERE id=?`, id); err != nil {
			return err
		}
		return tx.oweFoundryResends(ctx, loadID, model.FoundryResendMove, []string{""})
	})
	return reconciled, err
}

// FillRefusal is a press of a suggestion's "Add this link" that can't move
// the current load now, and why, in words for the person.
type FillRefusal struct{ Why string }

func (r *FillRefusal) Error() string { return r.Why }

// SetCurrentOntologyVersionFilling makes versionID the ontology's current
// version, as SetCurrentOntologyVersion does (expected is the version the
// caller saw current, nil for none; ErrConflict when another won), and then
// owes the current load a move in place to it for the suggestion
// fingerprint ("Add this link"): a fill row in state moving, a 'move' resend
// row, and a resend job (resend's id) queued behind the live job. A version
// already current is made current again, and the move owed. The load moves
// only if it is an agent draft's, ready or partial; one at the version
// already moved there for another suggestion, and this one joins that move,
// moved (joinFoundryLoadMove). Otherwise Foundry's own checks: the version reads the same document
// sets and the same tables as the load's. One that fails them is recorded
// needs_load, why saying what, and nothing is owed; the engine judges the
// rest when the move is sent (FoundryFoldWhy). The same suggestion pressed
// again replaces its row only where that row needs a load and no fold of it
// failed (FillRefusal otherwise), and no press goes through while a move of
// the load is under way: a fold that may have moved the marker keeps the
// versions repair reads (ReconcileFoundryLoadMarker). The returned row is nil
// when there is no load to move.
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
		if load.Status != model.FoundryLoadReady && load.Status != model.FoundryLoadPartial {
			return nil
		}
		var held string
		var failures int
		switch err := tx.db.QueryRowContext(ctx, `SELECT state, failures FROM foundry_heal_fills WHERE load_id=? AND fingerprint=?`, load.ID, fingerprint).
			Scan(&held, &failures); {
		case err == sql.ErrNoRows:
		case err != nil:
			return err
		case load.VersionID == versionID && (held != string(model.FoundryFillNeedsLoad) || failures > 0):
			// Pressed again at the version its row records: nothing changes.
			return nil
		}
		// A move under way, or one that may have moved the marker unrecorded,
		// keeps the versions it was sent with: repair reads them.
		var moving bool
		if err := tx.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_heal_fills WHERE load_id=? AND state='moving')`, load.ID).
			Scan(&moving); err != nil {
			return err
		}
		if moving {
			return &FillRefusal{Why: "the current load is moving to another version; try again once it has"}
		}
		switch {
		case held == "":
		case held != string(model.FoundryFillNeedsLoad):
			return &FillRefusal{Why: "the current load holds this suggestion's link already"}
		case failures > 0:
			return &FillRefusal{Why: "the current load's last move to add this link failed; start a new load"}
		}
		if load.VersionID == versionID {
			fill, err = tx.joinFoundryLoadMove(ctx, ontologyID, load.ID, fingerprint)
			return err
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

// joinFoundryLoadMove records suggestion fingerprint's "Add this link" on
// load loadID that is at the version pressed already: moved, as the load's
// move that gained the suggestion's link (its finding's canonical) is, so
// the heal worker fills the suggestion's documents next. Of several such
// moves, the newest. A load at that version that no move of it brought the
// link was made with it, its documents asked it already: FillRefusal. The
// caller holds the ontology's lock.
func (s *mysqlDB) joinFoundryLoadMove(ctx context.Context, ontologyID, loadID, fingerprint string) (*model.FoundryHealFill, error) {
	var canonical sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT JSON_UNQUOTE(JSON_EXTRACT(finding, '$.signal.canonical')) FROM foundry_heal_findings
WHERE ontology_id=? AND fingerprint=?`, ontologyID, fingerprint).Scan(&canonical); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	var from, to, links string
	err := s.db.QueryRowContext(ctx, `SELECT from_version_id, to_version_id, links_added FROM foundry_heal_fills
WHERE load_id=? AND fingerprint<>? AND links_added IS NOT NULL AND JSON_CONTAINS(links_added, JSON_QUOTE(?))
ORDER BY created_at DESC, id DESC LIMIT 1`, loadID, fingerprint, canonical.String).Scan(&from, &to, &links)
	if err == sql.ErrNoRows || !canonical.Valid {
		return nil, &FillRefusal{Why: "the current load is at this version already and was made with this link: its documents were asked it"}
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_heal_fills (id, ontology_id, load_id, fingerprint, from_version_id, to_version_id, state,
  links_added) VALUES (?, ?, ?, ?, ?, ?, 'moved', ?)
ON DUPLICATE KEY UPDATE from_version_id=VALUES(from_version_id), to_version_id=VALUES(to_version_id), state='moved', why=NULL,
  links_added=VALUES(links_added), stop_reason=NULL, active_key=NULL, last_error=NULL, failures=0, paused_until=NULL`,
		newID(), ontologyID, loadID, fingerprint, from, to, links); err != nil {
		return nil, err
	}
	return s.foundryHealFill(ctx, loadID, fingerprint)
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
// fill row it is of: nil when none is owed, or while it backs off after a
// failed job (failFoundryLoadMove), so a job that runs meanwhile for
// something else doesn't send it. Of several, first one to the version the
// load records now (moved already, as a reconcile leaves it, so recording it
// costs nothing), then one from it, then the newest.
func (s *mysqlDB) FoundryLoadMoveOwed(ctx context.Context, loadID string) (*model.FoundryHealFill, error) {
	fill, err := scanFoundryHealFill(s.db.QueryRowContext(ctx, `SELECT `+foundryHealFillColumns+foundryHealFillFrom+`
JOIN foundry_load_resends w ON w.load_id=f.load_id AND w.document_id='' AND w.reason=? AND w.status='owed'
JOIN foundry_loads l ON l.id=f.load_id
WHERE f.load_id=? AND f.state=? AND (f.paused_until IS NULL OR f.paused_until<=NOW(6))
ORDER BY f.to_version_id=l.version_id DESC, f.from_version_id=l.version_id DESC, f.updated_at DESC, f.id DESC LIMIT 1`,
		string(model.FoundryResendMove), loadID, string(model.FoundryFillMoving)))
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
// marker at a version no move of Foundry's records, needs_load with why. A
// move the load already made (its row at the new version, as after a
// reconcile) records the links alone; a load whose row records neither
// version records nothing (ErrConflict), for the job to try again. Every
// other row moving from the same version is answered too: moved where it was
// to the same version, else needs_load (the load moved elsewhere, why
// load_moved and the version). The move's request ends once no row moves.
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
			moved, err := tx.moveFoundryLoadVersion(ctx, loadID, fill.FromVersionID, fill.ToVersionID)
			if err != nil {
				return err
			}
			if !moved {
				var at string
				if err := tx.db.QueryRowContext(ctx, `SELECT version_id FROM foundry_loads WHERE id=?`, loadID).Scan(&at); err != nil {
					return err
				}
				if at != fill.ToVersionID {
					return fmt.Errorf("%w: load %s records version %s, neither of its move's", ErrConflict, loadID, at)
				}
			}
			links, err := json.Marshal(outcome.LinksAdded)
			if err != nil {
				return err
			}
			// A move's failures and backoff are its own: its collect starts
			// with none (FailFoundryFillCollect counts its own).
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET state='moved', links_added=?, why=NULL, failures=0,
  paused_until=NULL, last_error=NULL
WHERE load_id=? AND from_version_id=? AND to_version_id=? AND state='moving'`, string(links), loadID, fill.FromVersionID, fill.ToVersionID); err != nil {
				return err
			}
			var version int
			if err := tx.db.QueryRowContext(ctx, `SELECT version FROM foundry_ontology_versions WHERE id=?`, fill.ToVersionID).Scan(&version); err != nil {
				return err
			}
			elsewhere, err := json.Marshal([]model.FoundryFoldWhy{{Kind: "load_moved", Item: strconv.Itoa(version)}})
			if err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET state='needs_load', why=?
WHERE load_id=? AND from_version_id=? AND state='moving'`, string(elsewhere), loadID, fill.FromVersionID); err != nil {
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

// FoundryAskedApart is what every Extract of load loadID asks apart
// (contract 4.33.0's asked_apart): the links the load's own moves in place
// added (every fill row that records links_added, whatever its fill came to:
// a collect given up on leaves the load at the version its move reached), and, for a refresh or rebuild of another load, that load's too, and
// so on back: a rebuild replays what each document was asked, at no cost.
// None for a load a person made at a version (a load in full asks every link
// of each document), even one another load moved to: its documents' saved
// work for the other load holds no call of those links together. Sorted,
// without repeats.
func (s *mysqlDB) FoundryAskedApart(ctx context.Context, loadID string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for at := loadID; at != "" && !seen[at]; {
		seen[at] = true
		rows, err := s.db.QueryContext(ctx, `SELECT links_added FROM foundry_heal_fills
WHERE load_id=? AND links_added IS NOT NULL`, at)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var raw string
			var links []string
			if err := rows.Scan(&raw); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if err := json.Unmarshal([]byte(raw), &links); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("heal fill links of load %s: %w", at, err)
			}
			out = append(out, links...)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		var refreshes sql.NullString
		if err := s.db.QueryRowContext(ctx, `SELECT refreshes_load_id FROM foundry_loads WHERE id=?`, at).Scan(&refreshes); err != nil &&
			err != sql.ErrNoRows {
			return nil, err
		}
		at = refreshes.String
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
	return scanFoundryHealFillWith(scanner)
}

// scanFoundryHealFillWith reads foundryHealFillColumns, then more.
func scanFoundryHealFillWith(scanner rowScanner, more ...any) (*model.FoundryHealFill, error) {
	var f model.FoundryHealFill
	var state string
	var why, links, stop sql.NullString
	var paused sql.NullTime
	c := &f.Counts
	if err := scanner.Scan(append([]any{&f.ID, &f.OntologyID, &f.LoadID, &f.Fingerprint, &f.FromVersionID, &f.ToVersionID, &state, &why,
		&links, &f.ToVersion, &stop, &c.Owed, &c.Left, &c.Filled, &c.Nothing, &c.Refused, &c.Native, &c.ReviewsTaken, &c.FactsAdded,
		&f.SpentUSD, &f.CapUSD, &paused, &f.Failures}, more...)...); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	f.State = model.FoundryHealFillState(state)
	f.StopReason = stop.String
	if paused.Valid {
		at := paused.Time.UTC()
		f.PausedUntil = &at
	}
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
