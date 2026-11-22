package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// removedFoundryDocuments are a load's members whose document is deleted:
// what the load still holds and must take out of its graph. A member with
// no extraction row may still be in the graph (an attempt stopped between
// the agent's commit and its record), so every one is listed.
const removedFoundryDocuments = ` FROM foundry_load_documents m JOIN foundry_documents d ON d.id = m.document_id
WHERE m.load_id=? AND d.status='deleted'`

// ListRemovedFoundryDocuments returns the ids of the documents the load
// must take out of its graph.
func (s *mysqlDB) ListRemovedFoundryDocuments(ctx context.Context, loadID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.document_id`+removedFoundryDocuments+` ORDER BY m.document_id`, loadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var docID string
		if err := rows.Scan(&docID); err != nil {
			return nil, err
		}
		ids = append(ids, docID)
	}
	return ids, rows.Err()
}

func (s *mysqlDB) countRemovedFoundryDocuments(ctx context.Context, loadID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+removedFoundryDocuments, loadID).Scan(&n)
	return n, err
}

// RecordFoundryDocumentRemoval forgets a deleted document of the job's load
// once the agent took it out of the graph: its membership and extraction,
// while workerID holds the lease. ErrNotFound means the lease was lost.
func (s *mysqlDB) RecordFoundryDocumentRemoval(ctx context.Context, jobID, workerID, documentID string) error {
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
		var deleted bool
		if err := tx.db.QueryRowContext(ctx, `SELECT status='deleted' FROM foundry_documents WHERE id=?`, documentID).Scan(&deleted); err != nil {
			return fmt.Errorf("read removed foundry document %s: %w", documentID, err)
		}
		if !deleted {
			return fmt.Errorf("foundry document %s is not deleted; its load %s keeps it", documentID, loadID)
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_load_extractions WHERE load_id=? AND document_id=?`, loadID, documentID); err != nil {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_load_documents WHERE load_id=? AND document_id=?`, loadID, documentID)
		return err
	})
}

// oweFoundryRemovals has each of ontologies whose live job's load (unless
// it deletes) or current load holds a deleted document take it out: an
// extract owed after the live job, or queued now. A removal spends nothing,
// so no budget holds it back. With refreshOnly, an ontology with refresh off
// keeps what it holds, as a sync's changes leave its tables. The caller
// holds the ontologies' locks.
func (s *mysqlDB) oweFoundryRemovals(ctx context.Context, ontologies []*model.FoundryOntology, refreshOnly bool) error {
	for _, o := range ontologies {
		if refreshOnly && !o.AutoRefresh {
			continue
		}
		live, err := s.liveFoundryGraphJob(ctx, o.ID)
		if err != nil {
			return err
		}
		var loads []string
		if live != nil && live.Kind != model.FoundryGraphJobDelete {
			loads = append(loads, live.LoadID)
		}
		if o.CurrentLoadID != nil {
			loads = append(loads, *o.CurrentLoadID)
		}
		holds := 0
		for _, l := range loads {
			if holds, err = s.countRemovedFoundryDocuments(ctx, l); err != nil || holds > 0 {
				break
			}
		}
		switch {
		case err != nil:
			return err
		case holds == 0:
			continue
		case live != nil:
			if _, err := s.db.ExecContext(ctx, `UPDATE foundry_ontologies SET extract_owed=TRUE WHERE id=?`, o.ID); err != nil {
				return err
			}
		default:
			if _, err := s.queueFoundryExtract(ctx, o, &model.FoundryGraphJob{ID: id.New()}, model.FoundryGraphBudget{}); err != nil {
				return err
			}
		}
	}
	return nil
}

// expireFoundryDocuments has documents of set setID that left their
// source's retention window (ids, which the caller deleted) leave every load
// of ontologies, the workspace's live ones, whose locks the caller holds:
//   - each one's live job's load (unless it deletes) and current load take
//     every deleted document out with an extract, refresh on or off
//     (oweFoundryRemovals);
//   - each of its other loads that can still be read or made current
//     (ready, partial, failed or delete_failed) holding a deleted document
//     of the set no newer at the source than the newest of ids is deleted:
//     its delete is queued now with no job live, else at the job's end.
//
// The second rule also takes a load that a live job was writing when an
// earlier window exit came, and that then ended neither current nor
// deleted, and a load holding only an older version of a document still in
// the window: an older load is a convenience, the window is not. ids not in
// the set expire nothing.
func (s *mysqlDB) expireFoundryDocuments(ctx context.Context, ontologies []*model.FoundryOntology, setID string, ids []string) error {
	var newest sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(source_at) FROM foundry_documents WHERE id IN (`+idPlaceholders(len(ids))+`) AND set_id=?`,
		append(toAnySlice(ids), setID)...).Scan(&newest); err != nil {
		return fmt.Errorf("read the newest expired document of set %s: %w", setID, err)
	}
	var deleting []*model.FoundryOntology
	for _, o := range ontologies {
		if !newest.Valid {
			break
		}
		live, err := s.liveFoundryGraphJob(ctx, o.ID)
		if err != nil {
			return err
		}
		// "" matches no load: the ids are CHAR(26).
		current, writing := "", ""
		if o.CurrentLoadID != nil {
			current = *o.CurrentLoadID
		}
		if live != nil {
			writing = live.LoadID
		}
		res, err := s.db.ExecContext(ctx, `UPDATE foundry_loads l SET l.status='deleting'
WHERE l.ontology_id=? AND l.status IN ('ready', 'partial', 'failed', 'delete_failed') AND l.id NOT IN (?, ?)
  AND EXISTS (SELECT 1 FROM foundry_load_documents m JOIN foundry_documents d ON d.id=m.document_id
    WHERE m.load_id=l.id AND m.set_id=? AND d.status='deleted' AND d.source_at <= ?)`, o.ID, current, writing, setID, newest.Time)
		if err != nil {
			return fmt.Errorf("delete the loads of ontology %s holding expired documents: %w", o.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			deleting = append(deleting, o)
		}
	}
	if err := s.oweFoundryRemovals(ctx, ontologies, false); err != nil {
		return err
	}
	// After the removals: an extract they queued goes first, and its end
	// queues the delete.
	for _, o := range deleting {
		live, err := s.liveFoundryGraphJob(ctx, o.ID)
		if err != nil {
			return err
		}
		if live != nil {
			continue
		}
		if err := s.queueNextFoundryDelete(ctx, o, id.New()); err != nil {
			return err
		}
	}
	return nil
}
