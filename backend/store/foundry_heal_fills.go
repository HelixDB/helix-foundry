package store

import (
	"context"
	"database/sql"
)

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
