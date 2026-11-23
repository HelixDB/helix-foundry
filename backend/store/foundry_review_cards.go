package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/reviewcards"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The review cards index (migration 20261218000000): what each live load
// asks a person to decide, built per document by the graph worker's indexer
// (backend/reviewcards) and read by the review walk.
//
// A live load is the current load of a live ontology, or one a queued or
// running job other than a delete writes. Its documents are owed a rebuild
// (foundry_review_owed) exactly as they change: each Extract that records
// or removes one owes it, in the same transaction
// (RecordFoundryDocumentExtraction, RecordFoundryDocumentRemoval). What can
// change cards past a document (a value decision's resend, the engine's
// sweep at the end of an Extract run, an apply's diff, a load made current,
// a builder upgrade, drift) asks for a full pass instead: one request a load
// (foundry_review_loads.full_owed_at), so requests coalesce, which owes the
// load's extracted documents 500 at a time while fewer than 500 are owed.
// A pass starts at most FullPassEvery after the last ended, but at once for
// a load never indexed or indexed by an older builder.
//
// The indexer claims owed documents SKIP LOCKED for a lease (due_at), reads
// them from the graph and stores their cards only while its lease holds, so
// a slow worker never stores an older graph over a newer one and replicas
// split the work. A document owed again meanwhile is owed after the store.
// Replicas share each database's read pace (foundry_review_reads), so the
// indexer reads a database at most once a gap however many run.
//
// A card is open while no decision decides it (openFoundryReviewCard) and no
// spot check drew it. The open count is kept on foundry_review_loads,
// recounted when a settle, a removal, a draw or a store marked it dirty, so
// the summary reads rows only. A load is indexing until a pass of this
// builder indexed every document it owed (version): never "nothing to
// review" before its first full index, and never again for the documents a
// routine pass owes.

const (
	// foundryReviewClaimRows is how many due rows one claim reads, and of
	// those how many loads and documents a load it keeps.
	foundryReviewClaimRows  = 200
	foundryReviewClaimLoads = 4
	foundryReviewClaimDocs  = 20
	// foundryReviewPassChunk is how many documents a running pass owes at
	// a time, once its load has fewer than that many owed.
	foundryReviewPassChunk = 500
	// foundryReviewClearChunk is how many rows a cleanup deletes a statement.
	foundryReviewClearChunk = 5000
	// foundryReviewCountCap is one past the most the open count says.
	foundryReviewCountCap = 10001
	// foundryReviewSetAside is where a quarantined document's due_at goes:
	// no claim reaches it until it is owed again.
	foundryReviewSetAside = "9999-12-31 00:00:00"
	// foundryReviewParked is how long a load whose ontology or database
	// can't be found waits before its next pass.
	foundryReviewParked = time.Hour
)

// openFoundryReviewCard is the SQL condition, over the card with alias c,
// that it is open: no decision decides it (foundryDecidesCard) and no spot
// check drew it, the only definition of "open". A held claim a spot check
// drew is that spot check, not a card to decide. The two kinds of card are
// read apart, so each reads its own index.
func openFoundryReviewCard(c string) string {
	return `(CASE ` + c + `.kind WHEN 'release' THEN NOT EXISTS (SELECT 1 FROM foundry_review_decisions d WHERE ` + foundryDecidesCard("d", c, true) + `)
 ELSE NOT EXISTS (SELECT 1 FROM foundry_review_decisions d WHERE ` + foundryDecidesCard("d", c, false) + `) END
 AND NOT EXISTS (SELECT 1 FROM foundry_review_draws w WHERE w.load_id=` + c + `.load_id AND w.kind='held'
  AND w.doc_id=` + c + `.doc_id AND w.claim_section=` + c + `.claim_section AND w.claim_start=` + c + `.claim_start
  AND w.claim_end=` + c + `.claim_end AND w.claim_key=` + c + `.claim_key))`
}

// foundryDecidesCard is the SQL condition that decision d decides card c
// (aliases), of a held claim (claim) or of a name or a value: the only
// definition of deciding a card, which openFoundryReviewCard and a settle
// that expects its card open (foundryDecidedElsewhere) both read. A held
// claim is decided by a rejection at its place under its key or of the
// statement it reads, or an approval of the statement it reads now (an
// approval of what it said before is not: the load refuses it, and the card
// asks to be read again), as foundryClaimAt reads a claim; a name or a value
// by a decision of its identity (identity_key, the decisions' own
// expression). Another document's link of a name (an alias) decides no card.
func foundryDecidesCard(d, c string, claim bool) string {
	if !claim {
		return d + `.ontology_id=` + c + `.ontology_id AND ` + d + `.identity_key=` + c + `.identity_key`
	}
	return d + `.ontology_id=` + c + `.ontology_id AND ` + d + `.kind IN ('release', 'reject') AND ` + d + `.doc_id=` + c + `.doc_id
    AND ` + d + `.claim_section=` + c + `.claim_section AND ` + d + `.claim_start=` + c + `.claim_start AND ` + d + `.claim_end=` + c + `.claim_end
    AND (` + d + `.statement_hash=` + c + `.statement_hash OR (` + d + `.kind='reject' AND ` + d + `.claim_key=` + c + `.claim_key))`
}

// foundryReviewPlaceFree is the SQL condition that no spot check of load
// column `load` and no random label of ontology column `ontology` (from any
// load) is at the place of the row with alias t, its link included.
func foundryReviewPlaceFree(t, load, ontology string) string {
	return `NOT EXISTS (SELECT 1 FROM foundry_review_draws w WHERE w.load_id=` + load + ` AND w.doc_id=` + t + `.doc_id
  AND w.claim_section=` + t + `.claim_section AND w.claim_start=` + t + `.claim_start AND w.claim_end=` + t + `.claim_end
  AND COALESCE(w.link, '')=COALESCE(` + t + `.link, ''))
AND NOT EXISTS (SELECT 1 FROM foundry_review_labels l WHERE l.ontology_id=` + ontology + ` AND l.drawn='random' AND l.doc_id=` + t + `.doc_id
  AND l.claim_section=` + t + `.claim_section AND l.claim_start=` + t + `.claim_start AND l.claim_end=` + t + `.claim_end
  AND COALESCE(l.link, '')=COALESCE(` + t + `.link, ''))`
}

// foundryReviewOwe is the upsert of an owed row: owed again now, due now
// (a lease held keeps it, a backoff too, but a quarantine ends), and its
// failed reads forgotten. due_at goes before attempts, which it reads.
var foundryReviewOwe = ` ON DUPLICATE KEY UPDATE owed_at=NOW(6), due_at=IF(attempts >= ` + strconv.Itoa(model.FoundryReviewQuarantine) +
	`, NOW(6), GREATEST(due_at, NOW(6))), attempts=0`

// oweFoundryReviewDocument owes a document of load loadID a rebuild of its
// cards, and gives the load its row, in the caller's transaction. The owed
// row goes first, as the indexer locks owed rows before the load's.
func (s *mysqlDB) oweFoundryReviewDocument(ctx context.Context, ontologyID, loadID, docID string) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_owed (load_id, doc_id) VALUES (?, ?)`+foundryReviewOwe, loadID, docID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_loads (load_id, ontology_id) VALUES (?, ?)
ON DUPLICATE KEY UPDATE load_id=load_id`, loadID, ontologyID)
	return err
}

// requestFoundryReviewPass asks for a full pass of load loadID, in the
// caller's transaction: one row, however many ask. A request during a pass
// is one more pass after it.
func (s *mysqlDB) requestFoundryReviewPass(ctx context.Context, ontologyID, loadID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_loads (load_id, ontology_id, full_owed_at) VALUES (?, ?, NOW(6))
ON DUPLICATE KEY UPDATE full_owed_at=NOW(6)`, loadID, ontologyID)
	return err
}

// markFoundryReviewsDirty has the ontology's loads' open counts recounted:
// a decision saved or removed may open or close cards.
func (s *mysqlDB) markFoundryReviewsDirty(ctx context.Context, ontologyID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_review_loads SET dirty_at=NOW(6) WHERE ontology_id=?`, ontologyID)
	return err
}

// RequestFoundryReviewPasses asks for a full pass of every current load of a
// live ontology that has no row, was indexed by an older builder (or never),
// or whose last pass ended more than reviewcards.RefreshEvery ago.
func (s *mysqlDB) RequestFoundryReviewPasses(ctx context.Context) (int, error) {
	added, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_loads (load_id, ontology_id, full_owed_at)
SELECT o.current_load_id, o.id, NOW(6) FROM foundry_ontologies o
WHERE o.deleted_at IS NULL AND o.current_load_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM foundry_review_loads r WHERE r.load_id=o.current_load_id)`)
	if err != nil {
		return 0, err
	}
	asked, err := s.db.ExecContext(ctx, `UPDATE foundry_review_loads SET full_owed_at=NOW(6)
WHERE full_owed_at IS NULL AND (version IS NULL OR version < ? OR indexed_at < NOW(6) - INTERVAL ? SECOND)`,
		reviewcards.Version, int(reviewcards.RefreshEvery.Seconds()))
	if err != nil {
		return 0, err
	}
	a, _ := added.RowsAffected()
	b, _ := asked.RowsAffected()
	return int(a + b), nil
}

// AdvanceFoundryReviewPasses starts the passes due (a request, no backoff,
// and the last pass ended reviewcards.FullPassEvery ago, unless the load was
// never indexed or by an older builder), owes each running pass's next
// chunk of the load's extracted documents while fewer than a chunk are owed,
// and ends a pass whose documents are all owed and indexed or set aside.
// The load is then of this builder's version only when the pass set none
// of its documents aside: one set aside (owed since the pass started) keeps
// the load indexing, and its pass is asked for again; setting a document
// aside takes about an hour of failed reads, so such a load is read again
// about hourly, as the daily refresh would. It reports how many passes
// started.
func (s *mysqlDB) AdvanceFoundryReviewPasses(ctx context.Context) (started int, err error) {
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_review_loads SET full_started_at=NOW(6), full_cursor=''
WHERE full_owed_at IS NOT NULL AND full_started_at IS NULL AND (retry_at IS NULL OR retry_at <= NOW(6))
  AND (version IS NULL OR version < ? OR indexed_at IS NULL OR indexed_at <= NOW(6) - INTERVAL ? SECOND)`,
		reviewcards.Version, int(reviewcards.FullPassEvery.Seconds()))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	rows, err := s.db.QueryContext(ctx, `SELECT load_id, full_cursor FROM foundry_review_loads WHERE full_started_at IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	type running struct{ load, cursor string }
	passes, err := scanAll(rows, func(r rowScanner) (running, error) {
		var p running
		return p, r.Scan(&p.load, &p.cursor)
	})
	if err != nil {
		return 0, err
	}
	for _, p := range passes {
		var owed int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM foundry_review_owed WHERE load_id=? AND attempts < ? LIMIT ?) t`,
			p.load, model.FoundryReviewQuarantine, foundryReviewPassChunk).Scan(&owed); err != nil {
			return 0, err
		}
		if owed >= foundryReviewPassChunk {
			continue
		}
		var next sql.NullString
		if err := s.db.QueryRowContext(ctx, `SELECT MAX(document_id) FROM (SELECT document_id FROM foundry_load_extractions
WHERE load_id=? AND document_id > ? ORDER BY document_id LIMIT ?) t`, p.load, p.cursor, foundryReviewPassChunk).Scan(&next); err != nil {
			return 0, err
		}
		if next.Valid {
			if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_owed (load_id, doc_id)
SELECT load_id, document_id FROM foundry_load_extractions WHERE load_id=? AND document_id > ? AND document_id <= ?`+foundryReviewOwe,
				p.load, p.cursor, next.String); err != nil {
				return 0, err
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE foundry_review_loads SET full_cursor=? WHERE load_id=? AND full_started_at IS NOT NULL`,
				next.String, p.load); err != nil {
				return 0, err
			}
			continue
		}
		if owed > 0 {
			continue
		}
		// full_owed_at and version go first, as they read full_started_at:
		// a request since the pass started is one more pass, and a document
		// this pass owed that is set aside leaves the version as it was.
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_review_loads r
SET full_owed_at=IF(full_owed_at > full_started_at, full_owed_at, NULL),
  version=IF(EXISTS (SELECT 1 FROM foundry_review_owed w WHERE w.load_id=r.load_id AND w.attempts >= ? AND w.owed_at >= r.full_started_at),
    version, ?),
  full_started_at=NULL, full_cursor=NULL, indexed_at=NOW(6), dirty_at=NOW(6), attempts=0, retry_at=NULL
WHERE load_id=? AND full_started_at IS NOT NULL`, model.FoundryReviewQuarantine, reviewcards.Version, p.load); err != nil {
			return 0, err
		}
	}
	return int(n), nil
}

// RecountFoundryReviewLoads recounts at most limit loads whose open count is
// stale: every open card of the decide lane, held claims on a name's card
// included, up to foundryReviewCountCap. It reports how many it recounted.
// The count stands only while the load's dirty mark is the one read with
// it: a settle or store that marked it in a transaction the count's read
// didn't see leaves it dirty, so it is counted again.
func (s *mysqlDB) RecountFoundryReviewLoads(ctx context.Context, limit int) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT load_id FROM foundry_review_loads WHERE counted_at IS NULL OR dirty_at > counted_at LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	loads, err := scanAll(rows, func(r rowScanner) (string, error) {
		var id string
		return id, r.Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	for _, load := range loads {
		// The dirty mark and the count, read in one statement's snapshot;
		// the update compares the mark under its lock, so a mark committed
		// since (with a decision the count didn't see) wins.
		var dirty string
		var open int
		err := s.db.QueryRowContext(ctx, `SELECT CAST(r.dirty_at AS CHAR), (SELECT COUNT(*) FROM (SELECT 1 FROM foundry_review_cards c
WHERE c.load_id=r.load_id AND c.lane='decide' AND `+openFoundryReviewCard("c")+` LIMIT ?) t) FROM foundry_review_loads r WHERE r.load_id=?`,
			foundryReviewCountCap, load).Scan(&dirty, &open)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_review_loads SET open_cards=?, more_cards=?, counted_at=dirty_at
WHERE load_id=? AND dirty_at=?`, min(open, foundryReviewCountCap-1), open >= foundryReviewCountCap, load, dirty); err != nil {
			return 0, err
		}
	}
	return len(loads), nil
}

// ClearFoundryReviewLoads clears what belongs to at most limit loads that
// are no longer live (deleted, or neither current nor written by a job):
// their cards, documents and owed rows, a chunk a statement and outside any
// ontology's lock, then their row. A load deleted, or gone, loses its spot
// checks too. It reports how many loads it cleared.
func (s *mysqlDB) ClearFoundryReviewLoads(ctx context.Context, limit int) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.load_id FROM foundry_review_loads r
LEFT JOIN foundry_ontologies o ON o.id=r.ontology_id
WHERE o.id IS NULL OR o.deleted_at IS NOT NULL OR NOT (o.current_load_id <=> r.load_id OR EXISTS (
  SELECT 1 FROM foundry_load_jobs j WHERE j.load_id=r.load_id AND j.status IN ('queued', 'running') AND j.kind <> 'delete'))
LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	loads, err := scanAll(rows, func(r rowScanner) (string, error) {
		var id string
		return id, r.Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	for _, load := range loads {
		if err := s.clearFoundryReviewLoad(ctx, load); err != nil {
			return 0, err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_review_loads WHERE load_id=?`, load); err != nil {
			return 0, err
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_review_draws WHERE load_id IN (SELECT id FROM (
  SELECT DISTINCT w.load_id AS id FROM foundry_review_draws w LEFT JOIN foundry_loads l ON l.id=w.load_id
  WHERE l.id IS NULL OR l.status='deleted' LIMIT 50) t)`); err != nil {
		return 0, err
	}
	// A database no one has read for a day forgets its pace.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_review_reads WHERE next_at < NOW(6) - INTERVAL 1 DAY LIMIT 1000`); err != nil {
		return 0, err
	}
	return len(loads), nil
}

// clearFoundryReviewLoad deletes a load's index, a chunk at a time, but its
// row. Owed rows go first: a store in flight holds its owed rows' locks, so
// this waits for it, and its cards then go with the rest; a store after
// finds its rows gone and writes nothing. So no card outlives its load's
// row, which goes last (ClearFoundryReviewLoads) and is what a cleanup
// that stopped halfway starts from again.
func (s *mysqlDB) clearFoundryReviewLoad(ctx context.Context, load string) error {
	for _, table := range []string{"foundry_review_owed", "foundry_review_cards", "foundry_review_docs"} {
		for {
			res, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE load_id=? LIMIT ?`, load, foundryReviewClearChunk)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n < foundryReviewClearChunk {
				break
			}
		}
	}
	return nil
}

// ParkFoundryReviewLoad sets aside a load whose ontology or database can't
// be found: its cards, documents and owed rows go, and its row stays with a
// pass asked for an hour from now, so a live ontology whose database is
// gone is tried hourly, not owed again every housekeeping. A load no longer
// live is cleared by ClearFoundryReviewLoads.
func (s *mysqlDB) ParkFoundryReviewLoad(ctx context.Context, loadID string) error {
	if err := s.clearFoundryReviewLoad(ctx, loadID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_review_loads SET full_owed_at=NOW(6), full_started_at=NULL, full_cursor=NULL,
  attempts=LEAST(attempts + 1, 60000), retry_at=NOW(6) + INTERVAL ? SECOND, dirty_at=NOW(6) WHERE load_id=?`,
		int(foundryReviewParked.Seconds()), loadID)
	return err
}

// ClaimFoundryReviewDocuments claims due owed documents for workerID for
// lease: the earliest due of at most foundryReviewClaimRows rows no one else
// holds (SKIP LOCKED), keeping up to foundryReviewClaimDocs documents of each
// of up to foundryReviewClaimLoads loads. Owed rows of a load that is gone
// are deleted. A claim sets claimed_by and pushes due_at to the lease's end,
// so no other worker claims them before it; StoreFoundryReviewDocuments
// stores only while it holds.
func (s *mysqlDB) ClaimFoundryReviewDocuments(ctx context.Context, workerID string, lease time.Duration) ([]model.FoundryReviewClaim, error) {
	var claims []model.FoundryReviewClaim
	err := s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		rows, err := tx.db.QueryContext(ctx, `SELECT load_id, doc_id, owed_at FROM foundry_review_owed WHERE due_at <= NOW(6)
ORDER BY due_at LIMIT ? FOR UPDATE SKIP LOCKED`, foundryReviewClaimRows)
		if err != nil {
			return err
		}
		type due struct {
			load string
			owed model.FoundryReviewOwed
		}
		dues, err := scanAll(rows, func(r rowScanner) (due, error) {
			var d due
			return d, r.Scan(&d.load, &d.owed.DocID, &d.owed.OwedAt)
		})
		if err != nil {
			return err
		}
		byLoad := map[string]int{}
		for _, d := range dues {
			i, ok := byLoad[d.load]
			if !ok {
				if len(claims) == foundryReviewClaimLoads {
					continue
				}
				i = len(claims)
				byLoad[d.load] = i
				claims = append(claims, model.FoundryReviewClaim{LoadID: d.load})
			}
			if len(claims[i].Owed) < foundryReviewClaimDocs {
				claims[i].Owed = append(claims[i].Owed, d.owed)
			}
		}
		kept := claims[:0]
		for _, c := range claims {
			err := tx.db.QueryRowContext(ctx, `SELECT o.id, o.workspace_id, l.version_id FROM foundry_loads l
JOIN foundry_ontologies o ON o.id=l.ontology_id WHERE l.id=?`, c.LoadID).Scan(&c.OntologyID, &c.WorkspaceID, &c.VersionID)
			if errors.Is(err, sql.ErrNoRows) {
				if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_review_owed WHERE load_id=?`, c.LoadID); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			ids := c.DocIDs()
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_review_owed SET claimed_by=?, due_at=NOW(6) + INTERVAL ? MICROSECOND
WHERE load_id=? AND doc_id IN (`+idPlaceholders(len(ids))+`)`, append([]any{workerID, lease.Microseconds(), c.LoadID}, toAnySlice(ids)...)...); err != nil {
				return err
			}
			kept = append(kept, c)
		}
		claims = kept
		return nil
	})
	return claims, err
}

// StoreFoundryReviewDocuments stores what workerID read of its claim, in one
// transaction, only while every claimed row is still its own and its lease
// unexpired: stored false means the lease was lost and nothing was written,
// as another worker may store a newer read. Each document's cards replace
// those it had; one the graph no longer holds loses its cards and its
// document row. A document owed since the claim (owed_at later than read)
// is owed again at once; the rest are no longer owed. The load's backoff
// ends and its count is marked dirty.
func (s *mysqlDB) StoreFoundryReviewDocuments(ctx context.Context, workerID string, claim model.FoundryReviewClaim,
	indexed []model.FoundryReviewIndexed) (stored bool, err error) {
	ids := claim.DocIDs()
	if len(ids) == 0 {
		return false, fmt.Errorf("a review cards store of no documents")
	}
	err = s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		rows, err := tx.db.QueryContext(ctx, `SELECT claimed_by <=> ? AND due_at > NOW(6) FROM foundry_review_owed
WHERE load_id=? AND doc_id IN (`+idPlaceholders(len(ids))+`) FOR UPDATE`, append([]any{workerID, claim.LoadID}, toAnySlice(ids)...)...)
		if err != nil {
			return err
		}
		held, err := scanAll(rows, func(r rowScanner) (bool, error) {
			var mine bool
			return mine, r.Scan(&mine)
		})
		if err != nil || len(held) != len(ids) || slices.Contains(held, false) {
			return err
		}
		for _, doc := range indexed {
			if !slices.Contains(ids, doc.DocID) {
				return fmt.Errorf("document %s of load %s was not claimed", doc.DocID, claim.LoadID)
			}
			if err := tx.replaceFoundryReviewDocument(ctx, claim, doc); err != nil {
				return err
			}
		}
		for _, o := range claim.Owed {
			if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_review_owed WHERE load_id=? AND doc_id=? AND owed_at=?`,
				claim.LoadID, o.DocID, o.OwedAt); err != nil {
				return err
			}
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_review_owed SET claimed_by=NULL, due_at=NOW(6)
WHERE load_id=? AND doc_id IN (`+idPlaceholders(len(ids))+`)`, append([]any{claim.LoadID}, toAnySlice(ids)...)...); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_review_loads SET dirty_at=NOW(6), attempts=0, retry_at=NULL WHERE load_id=?`,
			claim.LoadID); err != nil {
			return err
		}
		stored = true
		return nil
	})
	return stored, err
}

// replaceFoundryReviewDocument replaces one document's cards and its row.
func (s *mysqlDB) replaceFoundryReviewDocument(ctx context.Context, claim model.FoundryReviewClaim, doc model.FoundryReviewIndexed) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_review_cards WHERE load_id=? AND doc_id=?`, claim.LoadID, doc.DocID); err != nil {
		return err
	}
	if !doc.Present {
		_, err := s.db.ExecContext(ctx, `DELETE FROM foundry_review_docs WHERE load_id=? AND doc_id=?`, claim.LoadID, doc.DocID)
		return err
	}
	const columns = 25
	for chunk := range slices.Chunk(doc.Cards, 200) {
		args := make([]any, 0, columns*len(chunk))
		for _, c := range chunk {
			if c.DocID != doc.DocID {
				return fmt.Errorf("a card of document %s stored as %s's", c.DocID, doc.DocID)
			}
			keys, err := json.Marshal(c.ReviewKeys)
			if err != nil {
				return err
			}
			var onCard any
			if c.OnCard != nil {
				onCard = c.OnCard[:]
			}
			args = append(args, claim.LoadID, c.ID[:], claim.OntologyID, string(c.Kind), string(c.Lane), c.Kind.KindOrder(), onCard, c.DocID,
				c.Type, c.SubjectType, c.ObjectType, c.NormName, c.EntityKey, c.Prop, c.ClaimKey, c.ClaimSection, c.ClaimStart, c.ClaimEnd,
				c.StatementHash, c.Link, c.ReviewKey, string(keys), c.ReviewCount, c.Confidence, c.Band)
			args = append(args, c.Blocked, c.Facts, c.PerDecision, c.Shuffle)
		}
		row := "(" + idPlaceholders(columns+4) + ")"
		if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_cards (load_id, card_id, ontology_id, kind, lane, kind_order, on_card, doc_id,
 type, subject_type, object_type, norm_name, entity_key, prop, claim_key, claim_section, claim_start, claim_end, statement_hash, link, review_key,
 review_keys, review_count, confidence, band, blocked, facts, per_decision, shuffle) VALUES `+strings.TrimSuffix(strings.Repeat(row+",", len(chunk)), ","),
			args...); err != nil {
			return fmt.Errorf("store review cards: %w", err)
		}
	}
	q := doc.Quotes
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_docs (load_id, doc_id, version, q_09, q_08, q_07, q_06, q_low, q_none, indexed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(6))
ON DUPLICATE KEY UPDATE version=VALUES(version), q_09=VALUES(q_09), q_08=VALUES(q_08), q_07=VALUES(q_07), q_06=VALUES(q_06),
  q_low=VALUES(q_low), q_none=VALUES(q_none), indexed_at=VALUES(indexed_at)`,
		claim.LoadID, doc.DocID, reviewcards.Version, q[0], q[1], q[2], q[3], q[4], q[5])
	return err
}

// BackOffFoundryReviewDocument puts off a document of workerID's claim whose
// read fails on its own: due again after 30 s doubling with each failure, at
// most an hour, and set aside after model.FoundryReviewQuarantine failures,
// which quarantined reports. A row no longer workerID's is left alone.
func (s *mysqlDB) BackOffFoundryReviewDocument(ctx context.Context, workerID, loadID, docID string) (quarantined bool, err error) {
	// due_at goes before attempts, which it reads.
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_review_owed
SET due_at=IF(attempts + 1 >= ?, ?, NOW(6) + INTERVAL LEAST(3600, 30 * POW(2, attempts)) SECOND), attempts=attempts + 1, claimed_by=NULL
WHERE load_id=? AND doc_id=? AND claimed_by=?`, model.FoundryReviewQuarantine, foundryReviewSetAside, loadID, docID, workerID); err != nil {
		return false, err
	}
	var attempts int
	err = s.db.QueryRowContext(ctx, `SELECT attempts FROM foundry_review_owed WHERE load_id=? AND doc_id=?`, loadID, docID).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return attempts >= model.FoundryReviewQuarantine, err
}

// BackOffFoundryReviewLoad puts off a load whose database can't be read now
// (paused, starting or unreachable): its owed documents, and the start of a
// pass, wait 30 s doubling with each failure in a row, at most an hour.
func (s *mysqlDB) BackOffFoundryReviewLoad(ctx context.Context, loadID string) error {
	var wait int
	if err := s.db.QueryRowContext(ctx, `SELECT LEAST(3600, 30 * POW(2, LEAST(attempts, 7))) FROM foundry_review_loads WHERE load_id=?`,
		loadID).Scan(&wait); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	wait = max(wait, 30)
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_review_owed SET claimed_by=NULL, due_at=GREATEST(due_at, NOW(6) + INTERVAL ? SECOND)
WHERE load_id=? AND attempts < ?`, wait, loadID, model.FoundryReviewQuarantine); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_review_loads SET attempts=LEAST(attempts + 1, 60000), retry_at=NOW(6) + INTERVAL ? SECOND
WHERE load_id=?`, wait, loadID)
	return err
}

// ListFoundryClaimDecisions lists the ontology's release and reject
// decisions of held claims in documents docIDs, through its documents'
// index.
func (s *mysqlDB) ListFoundryClaimDecisions(ctx context.Context, ontologyID string, docIDs []string) ([]*model.FoundryReviewDecision, error) {
	if len(docIDs) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryReviewDecisionDrawnColumns+` FROM foundry_review_decisions
WHERE ontology_id=? AND kind IN ('release', 'reject') AND doc_id IN (`+idPlaceholders(len(docIDs))+`) ORDER BY id`,
		append([]any{ontologyID}, toAnySlice(docIDs)...)...)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundryReviewDecisionDrawn)
}

// foundryReviewIndexed is the SQL condition, over a load's row with alias
// r (NULL when it has none), that its index is whole: a pass of this
// builder indexed every document it owed, and the load was counted since.
// Rows a later pass or an Extract owes don't make it indexing again: its
// cards and count stand meanwhile, a document's at most a pass behind.
func foundryReviewIndexed(r string) string {
	return r + `.version >= ` + strconv.Itoa(reviewcards.Version) + ` AND ` + r + `.counted_at IS NOT NULL`
}

// ListFoundryReviewSummaries is what each live ontology of the workspace with
// a current load asks a person to decide, read from foundry_review_loads
// alone. A load is indexing until its index is first whole
// (foundryReviewIndexed).
func (s *mysqlDB) ListFoundryReviewSummaries(ctx context.Context, workspaceID string) ([]model.FoundryReviewSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.id, o.current_load_id, COALESCE(r.open_cards, 0), COALESCE(r.more_cards, FALSE),
  NOT COALESCE(`+foundryReviewIndexed("r")+`, FALSE)
FROM foundry_ontologies o LEFT JOIN foundry_review_loads r ON r.load_id=o.current_load_id
WHERE o.workspace_id=? AND o.deleted_at IS NULL AND o.current_load_id IS NOT NULL ORDER BY o.id`, workspaceID)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, func(r rowScanner) (model.FoundryReviewSummary, error) {
		var s model.FoundryReviewSummary
		return s, r.Scan(&s.OntologyID, &s.LoadID, &s.Open, &s.More, &s.Indexing)
	})
}

// FoundryReviewIndexing reports whether load loadID's index isn't whole
// yet, as ListFoundryReviewSummaries says it.
func (s *mysqlDB) FoundryReviewIndexing(ctx context.Context, loadID string) (bool, error) {
	var indexing bool
	err := s.db.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM foundry_review_loads r WHERE r.load_id=? AND `+foundryReviewIndexed("r")+`)`,
		loadID).Scan(&indexing)
	return indexing, err
}

// ReserveFoundryReviewRead takes the indexer's next read of database
// tenantID, shared by every replica: its slot is gap after the last slot
// taken, or now when that has passed. It reports how long until the slot.
// The row is locked by the upsert (an exclusive lock, even on a duplicate),
// so racing replicas take one slot each, a gap apart.
func (s *mysqlDB) ReserveFoundryReviewRead(ctx context.Context, tenantID string, gap time.Duration) (time.Duration, error) {
	var wait int64
	err := s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_review_reads (tenant_id, next_at) VALUES (?, NOW(6) + INTERVAL ? MICROSECOND)
ON DUPLICATE KEY UPDATE next_at=GREATEST(next_at, NOW(6)) + INTERVAL ? MICROSECOND`, tenantID, gap.Microseconds(), gap.Microseconds()); err != nil {
			return err
		}
		return tx.db.QueryRowContext(ctx, `SELECT TIMESTAMPDIFF(MICROSECOND, NOW(6), next_at) - ? FROM foundry_review_reads WHERE tenant_id=?`,
			gap.Microseconds(), tenantID).Scan(&wait)
	})
	return max(time.Duration(wait)*time.Microsecond, 0), err
}

// foundryReviewCardColumns are a card's columns as scanFoundryReviewCard
// reads them, over alias c.
const foundryReviewCardColumns = `c.card_id, c.kind, c.lane, c.on_card, c.doc_id, c.type, c.subject_type, c.object_type, c.norm_name, c.entity_key,
 c.prop, c.claim_key, c.claim_section, c.claim_start, c.claim_end, c.statement_hash, c.link, c.review_key, c.review_keys, c.review_count,
 c.confidence, c.band, c.blocked, c.facts, c.per_decision, c.shuffle`

func scanFoundryReviewCard(r rowScanner) (*reviewcards.Card, error) {
	var c reviewcards.Card
	var id, onCard, keys []byte
	var kind, lane string
	if err := r.Scan(&id, &kind, &lane, &onCard, &c.DocID, &c.Type, &c.SubjectType, &c.ObjectType, &c.NormName, &c.EntityKey, &c.Prop,
		&c.ClaimKey, &c.ClaimSection, &c.ClaimStart, &c.ClaimEnd, &c.StatementHash, &c.Link, &c.ReviewKey, &keys, &c.ReviewCount,
		&c.Confidence, &c.Band, &c.Blocked, &c.Facts, &c.PerDecision, &c.Shuffle); err != nil {
		return nil, err
	}
	if len(id) != len(c.ID) || onCard != nil && len(onCard) != len(c.ID) {
		return nil, fmt.Errorf("stored review card has an id of %d bytes", len(id))
	}
	copy(c.ID[:], id)
	if onCard != nil {
		var on reviewcards.ID
		copy(on[:], onCard)
		c.OnCard = &on
	}
	c.Kind, c.Lane = reviewcards.Kind(kind), reviewcards.Lane(lane)
	if err := json.Unmarshal(keys, &c.ReviewKeys); err != nil {
		return nil, fmt.Errorf("stored review card's review keys: %w", err)
	}
	return &c, nil
}

// ListFoundryReviewCards lists at most limit open cards of load loadID
// under filter, in the walk's order (per decision, then facts, descending;
// then kind order, shuffle and id), after cursor (nil from the first). A
// held claim on an open name's card is listed on that card (Held), not on
// its own, unless the filter leaves names out. ontologyID is the load's.
func (s *mysqlDB) ListFoundryReviewCards(ctx context.Context, ontologyID, loadID string, filter model.FoundryReviewCardFilter,
	after *model.FoundryReviewCardCursor, limit int) ([]*model.FoundryReviewListedCard, error) {
	where := []string{"c.load_id=?", "c.lane=?", openFoundryReviewCard("c")}
	args := []any{loadID, string(filter.Lane)}
	names := len(filter.Kinds) == 0 || slices.Contains(filter.Kinds, reviewcards.KindName)
	if len(filter.Kinds) > 0 {
		where = append(where, "c.kind IN ("+idPlaceholders(len(filter.Kinds))+")")
		for _, k := range filter.Kinds {
			args = append(args, string(k))
		}
	}
	if names {
		where = append(where, `(c.on_card IS NULL OR NOT EXISTS (SELECT 1 FROM foundry_review_cards n WHERE n.load_id=c.load_id
  AND n.card_id=c.on_card AND `+openFoundryReviewCard("n")+`))`)
	}
	if filter.Type != "" {
		where = append(where, "(c.type=? OR c.subject_type=? OR c.object_type=?)")
		args = append(args, filter.Type, filter.Type, filter.Type)
	}
	if filter.DocumentID != "" {
		where = append(where, "c.doc_id=?")
		args = append(args, filter.DocumentID)
	}
	if a := after; a != nil {
		where = append(where, `(c.per_decision < ? OR c.per_decision = ? AND (c.facts < ? OR c.facts = ? AND (c.kind_order > ? OR c.kind_order = ?
  AND (c.shuffle > ? OR c.shuffle = ? AND c.card_id > ?))))`)
		args = append(args, a.PerDecision, a.PerDecision, a.Facts, a.Facts, a.KindOrder, a.KindOrder, a.Shuffle, a.Shuffle, a.ID[:])
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryReviewCardColumns+" FROM foundry_review_cards c WHERE "+strings.Join(where, " AND ")+`
ORDER BY c.per_decision DESC, c.facts DESC, c.kind_order, c.shuffle, c.card_id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	cards, err := scanAll(rows, scanFoundryReviewCard)
	if err != nil {
		return nil, err
	}
	listed := make([]*model.FoundryReviewListedCard, len(cards))
	var docs []string
	var nameIDs []any
	at := map[reviewcards.ID]*model.FoundryReviewListedCard{}
	for i, c := range cards {
		listed[i] = &model.FoundryReviewListedCard{Card: c}
		if c.Kind == reviewcards.KindName {
			docs = append(docs, c.DocID)
			nameIDs = append(nameIDs, c.ID[:])
			at[c.ID] = listed[i]
		}
	}
	if len(nameIDs) == 0 {
		return listed, nil
	}
	slices.Sort(docs)
	docs = slices.Compact(docs)
	rows, err = s.db.QueryContext(ctx, "SELECT "+foundryReviewCardColumns+` FROM foundry_review_cards c
WHERE c.load_id=? AND c.doc_id IN (`+idPlaceholders(len(docs))+`) AND c.on_card IN (`+idPlaceholders(len(nameIDs))+`) AND `+
		openFoundryReviewCard("c")+` ORDER BY c.shuffle, c.card_id`, append(append([]any{loadID}, toAnySlice(docs)...), nameIDs...)...)
	if err != nil {
		return nil, err
	}
	held, err := scanAll(rows, scanFoundryReviewCard)
	if err != nil {
		return nil, err
	}
	for _, h := range held {
		if n := at[*h.OnCard]; n != nil {
			n.Held = append(n.Held, h)
		}
	}
	return listed, nil
}

// GetFoundryReviewCard reads one card of load loadID by its id, and whether
// it is open (openFoundryReviewCard). ErrNotFound means the load's index
// holds no such card.
func (s *mysqlDB) GetFoundryReviewCard(ctx context.Context, loadID string, cardID reviewcards.ID) (*reviewcards.Card, bool, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+foundryReviewCardColumns+", "+openFoundryReviewCard("c")+` FROM foundry_review_cards c
WHERE c.load_id=? AND c.card_id=?`, loadID, cardID[:])
	var open bool
	card, err := scanFoundryReviewCard(extraScanner{row, []any{&open}})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	return card, open, nil
}

// ListFoundryReviewNameDecisions reads, for each name (type and normalized
// name), the ontology's resolve decisions of it in every document: how
// many, how many choices (an entity each, or not in the data), and the
// newest 20, through the names index. A name with none is left out.
func (s *mysqlDB) ListFoundryReviewNameDecisions(ctx context.Context, ontologyID string, names [][2]string) ([]*model.FoundryReviewNameDecisions, error) {
	if len(names) == 0 {
		return nil, nil
	}
	hashes := strings.TrimSuffix(strings.Repeat("UNHEX(SHA2(CAST(JSON_ARRAY(?, ?) AS CHAR), 256)),", len(names)), ",")
	args := []any{ontologyID}
	for _, n := range names {
		args = append(args, n[0], n[1])
	}
	// Each name's decisions, newest first, with their name's total and
	// choices; a hash's other name (a collision) is left out by comparing.
	rows, err := s.db.QueryContext(ctx, `SELECT `+foundryReviewDecisionDrawnColumns+`, total, choices FROM (
  SELECT d.*, COUNT(*) OVER (PARTITION BY d.type, d.norm_name) AS total,
    ROW_NUMBER() OVER (PARTITION BY d.type, d.norm_name ORDER BY d.decided_at DESC, d.id DESC) AS n
  FROM foundry_review_decisions d
  WHERE d.ontology_id=? AND d.kind='resolve' AND d.name_hash IN (`+hashes+`)) t
JOIN (SELECT type AS ct, norm_name AS cn, COUNT(DISTINCT COALESCE(entity_key, '')) AS choices FROM foundry_review_decisions
  WHERE ontology_id=? AND kind='resolve' AND name_hash IN (`+hashes+`) GROUP BY type, norm_name) c ON c.ct=t.type AND c.cn=t.norm_name
WHERE t.n <= 20 ORDER BY t.type, t.norm_name, t.n`, append(args, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	wanted := map[[2]string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	var out []*model.FoundryReviewNameDecisions
	by := map[[2]string]*model.FoundryReviewNameDecisions{}
	for rows.Next() {
		var total, choices int
		d, err := scanDecisionWith(rows, &total, &choices)
		if err != nil {
			return nil, err
		}
		key := [2]string{d.Type, *d.NormName}
		if !wanted[key] {
			continue
		}
		n := by[key]
		if n == nil {
			n = &model.FoundryReviewNameDecisions{Type: d.Type, NormName: *d.NormName, Total: total, Choices: choices}
			by[key] = n
			out = append(out, n)
		}
		n.Newest = append(n.Newest, d)
	}
	return out, rows.Err()
}

// scanDecisionWith scans a decision with its drawn column, and extra
// columns after it.
func scanDecisionWith(row rowScanner, extra ...any) (*model.FoundryReviewDecision, error) {
	return scanDecision(extraScanner{row, extra}, true)
}

type extraScanner struct {
	rowScanner
	extra []any
}

func (e extraScanner) Scan(dest ...any) error { return e.rowScanner.Scan(append(dest, e.extra...)...) }

// FoundryReviewPrecision is the ontology's spot checks' word per band (each
// place's first random label, banded by the score it was read at, as the
// site's bandOf bands it) and how often re-checks agreed.
func (s *mysqlDB) FoundryReviewPrecision(ctx context.Context, ontologyID string) (model.FoundryReviewPrecision, error) {
	var p model.FoundryReviewPrecision
	rows, err := s.db.QueryContext(ctx, `SELECT band, SUM(verdict='right'), COUNT(*) FROM (
  SELECT verdict, CASE WHEN confidence IS NULL OR confidence <= 0 THEN 'none' WHEN confidence >= 0.9 THEN '0.9'
    WHEN confidence >= 0.8 THEN '0.8' WHEN confidence >= 0.7 THEN '0.7' WHEN confidence >= 0.6 THEN '0.6' ELSE 'low' END AS band,
    ROW_NUMBER() OVER (PARTITION BY doc_id, claim_section, claim_start, claim_end, COALESCE(link, '') ORDER BY decided_at, id) AS n
  FROM foundry_review_labels WHERE ontology_id=? AND drawn='random') t
WHERE n=1 GROUP BY band`, ontologyID)
	if err != nil {
		return p, err
	}
	bands, err := scanAll(rows, func(r rowScanner) (model.FoundryReviewBandPrecision, error) {
		var b model.FoundryReviewBandPrecision
		return b, r.Scan(&b.Band, &b.Right, &b.Total)
	})
	if err != nil {
		return p, err
	}
	for _, band := range reviewcards.Bands {
		if i := slices.IndexFunc(bands, func(b model.FoundryReviewBandPrecision) bool { return b.Band == band }); i >= 0 {
			p.Bands = append(p.Bands, bands[i])
		}
	}
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(verdict=against), 0), COUNT(*) FROM foundry_review_labels
WHERE ontology_id=? AND drawn='recheck' AND against IS NOT NULL`, ontologyID).Scan(&p.RechecksAgreed, &p.RechecksTotal)
	return p, err
}

// foundryReviewRecheckSampled is the SQL condition that the row with alias
// t is sampled for re-checking: reviewcards.Rechecked of its id.
func foundryReviewRecheckSampled(t string) string {
	return `CONV(LEFT(SHA2(CONCAT('recheck', CHAR(31), ` + t + `.id), 256), 8), 16, 10) < 214748365`
}

// ListFoundryReviewRechecks lists what userID may re-check of the ontology,
// at most limit of each: the decisions on held claims made from the walk and
// the random labels that came with no decision, made by someone else and
// sampled (reviewcards.Rechecked), at places userID has labelled nothing (a
// decision's place has no link, so any of its links counts).
func (s *mysqlDB) ListFoundryReviewRechecks(ctx context.Context, ontologyID, userID string, limit int) ([]*model.FoundryReviewDecision, []*model.FoundryReviewLabel, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryReviewDecisionDrawnColumns+` FROM foundry_review_decisions d
WHERE d.ontology_id=? AND d.kind IN ('release', 'reject') AND d.drawn IS NOT NULL AND d.decided_by_user_id<>? AND `+foundryReviewRecheckSampled("d")+`
  AND NOT EXISTS (SELECT 1 FROM foundry_review_labels m WHERE m.ontology_id=d.ontology_id AND m.decided_by_user_id=? AND m.doc_id=d.doc_id
    AND m.claim_section=d.claim_section AND m.claim_start=d.claim_start AND m.claim_end=d.claim_end)
ORDER BY d.id LIMIT ?`, ontologyID, userID, userID, limit)
	if err != nil {
		return nil, nil, err
	}
	decisions, err := scanAll(rows, scanFoundryReviewDecisionDrawn)
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT "+prefixed("l", foundryReviewLabelColumns)+` FROM foundry_review_labels l
WHERE l.ontology_id=? AND l.drawn='random' AND l.decision_id IS NULL AND l.decided_by_user_id<>? AND `+foundryReviewRecheckSampled("l")+`
  AND NOT EXISTS (SELECT 1 FROM foundry_review_labels m WHERE m.ontology_id=l.ontology_id AND m.decided_by_user_id=? AND m.doc_id=l.doc_id
    AND m.claim_section=l.claim_section AND m.claim_start=l.claim_start AND m.claim_end=l.claim_end AND COALESCE(m.link, '')=COALESCE(l.link, ''))
ORDER BY l.id LIMIT ?`, ontologyID, userID, userID, limit)
	if err != nil {
		return nil, nil, err
	}
	labels, err := scanAll(rows, scanFoundryReviewLabel)
	return decisions, labels, err
}

// ListFoundryReviewLabelsAt lists the ontology's labels, from any load, at
// the documents and places of draws (a place's link aside: the caller
// matches it).
func (s *mysqlDB) ListFoundryReviewLabelsAt(ctx context.Context, ontologyID string, draws []model.FoundryReviewDraw) ([]*model.FoundryReviewLabel, error) {
	if len(draws) == 0 {
		return nil, nil
	}
	places := strings.TrimSuffix(strings.Repeat("(l.doc_id=? AND l.claim_section=? AND l.claim_start=? AND l.claim_end=?) OR ", len(draws)), " OR ")
	args := []any{ontologyID}
	for _, d := range draws {
		args = append(args, d.DocID, d.Span.Section, d.Span.Start, d.Span.End)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+prefixed("l", foundryReviewLabelColumns)+` FROM foundry_review_labels l
WHERE l.ontology_id=? AND (`+places+`) ORDER BY l.id`, args...)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundryReviewLabel)
}

// foundryReviewDrawColumns are a draw's columns over alias w, with whether it
// is done and, for a held claim, its card's review while there is one.
const foundryReviewDrawColumns = `w.load_id, w.seq, w.kind, w.band, w.doc_id, w.claim_section, w.claim_start, w.claim_end, w.link, w.confidence,
 w.claim_key, w.statement_hash, w.fact_key,
 EXISTS (SELECT 1 FROM foundry_review_labels l WHERE l.ontology_id=? AND l.drawn='random' AND l.doc_id=w.doc_id
   AND l.claim_section=w.claim_section AND l.claim_start=w.claim_start AND l.claim_end=w.claim_end AND COALESCE(l.link, '')=COALESCE(w.link, '')),
 (SELECT c.review_key FROM foundry_review_cards c WHERE c.load_id=w.load_id AND c.doc_id=w.doc_id AND c.kind='release'
   AND c.claim_section=w.claim_section AND c.claim_start=w.claim_start AND c.claim_end=w.claim_end AND c.claim_key=w.claim_key LIMIT 1)`

// ListFoundryReviewDraws lists load loadID's spot checks in the order drawn,
// each with whether it is done and a held claim's review while its card is
// there. ontologyID is the load's.
func (s *mysqlDB) ListFoundryReviewDraws(ctx context.Context, ontologyID, loadID string) ([]model.FoundryReviewDraw, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryReviewDrawColumns+` FROM foundry_review_draws w WHERE w.load_id=? ORDER BY w.seq`,
		ontologyID, loadID)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, func(r rowScanner) (model.FoundryReviewDraw, error) {
		var d model.FoundryReviewDraw
		var kind string
		var review sql.NullString
		err := r.Scan(&d.LoadID, &d.Seq, &kind, &d.Band, &d.DocID, &d.Span.Section, &d.Span.Start, &d.Span.End, &d.Link, &d.Confidence,
			&d.ClaimKey, &d.StatementHash, &d.FactKey, &d.Done, &review)
		d.Kind, d.ReviewKey = model.FoundryReviewDrawKind(kind), review.String
		return d, err
	})
}

// FoundryReviewDrawPools is what each band's next spot check of load loadID
// is drawn from: its held claims at places no draw or random label holds,
// and its quotes of facts less those drawn. A labelled quote is passed over
// when it is picked, so the quotes may be a little more than are left.
func (s *mysqlDB) FoundryReviewDrawPools(ctx context.Context, loadID string) ([6]reviewcards.Pool, error) {
	var pools [6]reviewcards.Pool
	band := func(name string) int { return slices.Index(reviewcards.Bands[:], name) }
	rows, err := s.db.QueryContext(ctx, `SELECT c.band, COUNT(*) FROM foundry_review_cards c
WHERE c.load_id=? AND c.kind='release' AND `+foundryReviewPlaceFree("c", "c.load_id", "c.ontology_id")+` GROUP BY c.band`, loadID)
	if err != nil {
		return pools, err
	}
	type count struct {
		band string
		n    int
	}
	scan := func(r rowScanner) (count, error) {
		var c count
		return c, r.Scan(&c.band, &c.n)
	}
	held, err := scanAll(rows, scan)
	if err != nil {
		return pools, err
	}
	for _, c := range held {
		if b := band(c.band); b >= 0 {
			pools[b].Held = c.n
		}
	}
	var q [6]int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(q_09), 0), COALESCE(SUM(q_08), 0), COALESCE(SUM(q_07), 0), COALESCE(SUM(q_06), 0),
  COALESCE(SUM(q_low), 0), COALESCE(SUM(q_none), 0) FROM foundry_review_docs WHERE load_id=?`, loadID).Scan(&q[0], &q[1], &q[2], &q[3], &q[4], &q[5]); err != nil {
		return pools, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT band, COUNT(*) FROM foundry_review_draws WHERE load_id=? AND kind='quote' GROUP BY band`, loadID)
	if err != nil {
		return pools, err
	}
	drawn, err := scanAll(rows, scan)
	if err != nil {
		return pools, err
	}
	for _, c := range drawn {
		if b := band(c.band); b >= 0 {
			q[b] -= c.n
		}
	}
	for b := range pools {
		pools[b].Quotes = max(q[b], 0)
	}
	return pools, nil
}

// foundryReviewQuoteColumns are the columns of foundry_review_docs that
// count each band's quotes, in reviewcards.Bands' order.
var foundryReviewQuoteColumns = [6]string{"q_09", "q_08", "q_07", "q_06", "q_low", "q_none"}

// FoundryReviewQuoteDocuments are the documents of load loadID a quote of
// band is drawn from, at most limit, in the order to try them: from the one
// holding the band's target-th quote (by document id, each weighted by how
// many of its quotes are in the band), then those after it, then those
// before.
func (s *mysqlDB) FoundryReviewQuoteDocuments(ctx context.Context, loadID string, band, target, limit int) ([]string, error) {
	q := foundryReviewQuoteColumns[band]
	rows, err := s.db.QueryContext(ctx, `SELECT doc_id FROM (
  SELECT doc_id, SUM(`+q+`) OVER (ORDER BY doc_id) AS upto FROM foundry_review_docs WHERE load_id=? AND `+q+` > 0) t
ORDER BY upto <= ?, doc_id LIMIT ?`, loadID, target, limit)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, func(r rowScanner) (string, error) {
		var id string
		return id, r.Scan(&id)
	})
}

// FoundryReviewPlacesTaken are the places of document docID that a spot
// check of load loadID or a random label of the ontology (any load) holds,
// each as reviewcards' place key: section, start, end and link.
func (s *mysqlDB) FoundryReviewPlacesTaken(ctx context.Context, ontologyID, loadID, docID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT claim_section, claim_start, claim_end, COALESCE(link, '') FROM foundry_review_draws
WHERE load_id=? AND doc_id=?
UNION SELECT claim_section, claim_start, claim_end, COALESCE(link, '') FROM foundry_review_labels
WHERE ontology_id=? AND doc_id=? AND drawn='random'`, loadID, docID, ontologyID, docID)
	if err != nil {
		return nil, err
	}
	places, err := scanAll(rows, func(r rowScanner) (string, error) {
		var section, start, end uint32
		var link string
		err := r.Scan(&section, &start, &end, &link)
		return FoundryReviewPlace(section, start, end, link), err
	})
	taken := map[string]bool{}
	for _, p := range places {
		taken[p] = true
	}
	return taken, err
}

// FoundryReviewPlace is a claim's place within its document, as
// FoundryReviewPlacesTaken keys it.
func FoundryReviewPlace(section, start, end uint32, link string) string {
	return fmt.Sprintf("%d\x1f%d\x1f%d\x1f%s", section, start, end, link)
}

// DrawFoundryHeldClaim draws spot check seq of load loadID from band's held
// claims: the first by shuffle at a place no draw or random label holds.
// drawn false means the band had none; ErrConflict that seq was drawn by
// someone else first. The load's count is marked dirty: a drawn claim is no
// card to decide.
func (s *mysqlDB) DrawFoundryHeldClaim(ctx context.Context, loadID string, seq, band int) (drawn bool, err error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_draws (load_id, seq, kind, band, doc_id, claim_section, claim_start, claim_end,
  link, confidence, claim_key, statement_hash)
SELECT c.load_id, ?, 'held', c.band, c.doc_id, c.claim_section, c.claim_start, c.claim_end, c.link, c.confidence, c.claim_key, c.statement_hash
FROM foundry_review_cards c WHERE c.load_id=? AND c.kind='release' AND c.band=? AND `+foundryReviewPlaceFree("c", "c.load_id", "c.ontology_id")+`
ORDER BY c.shuffle, c.card_id LIMIT 1`, seq, loadID, reviewcards.Bands[band])
	if err != nil {
		return false, duplicateConflict(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE foundry_review_loads SET dirty_at=NOW(6) WHERE load_id=?`, loadID)
	return true, err
}

// InsertFoundryReviewQuoteDraw stores spot check d, a fact's quote.
// ErrConflict means its seq was drawn by someone else first.
func (s *mysqlDB) InsertFoundryReviewQuoteDraw(ctx context.Context, d model.FoundryReviewDraw) error {
	if d.Kind != model.FoundryReviewDrawQuote || d.FactKey == nil || d.Span.Start >= d.Span.End || !slices.Contains(reviewcards.Bands[:], d.Band) {
		return fmt.Errorf("spot check %d of load %s is no quote draw", d.Seq, d.LoadID)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_draws (load_id, seq, kind, band, doc_id, claim_section, claim_start, claim_end,
  link, confidence, fact_key) VALUES (?, ?, 'quote', ?, ?, ?, ?, ?, ?, ?, ?)`, d.LoadID, d.Seq, d.Band, d.DocID, d.Span.Section, d.Span.Start,
		d.Span.End, d.Link, d.Confidence, d.FactKey)
	return duplicateConflict(err)
}

// duplicateConflict is ErrConflict for a duplicate key, err otherwise.
func duplicateConflict(err error) error {
	if isDuplicateKey(err) {
		return ErrConflict
	}
	return err
}

// ListFoundryReviewDecisionsByID reads the ontology's saved decisions of
// ids (each at most once, in id order); one removed or replaced (a changed
// choice is a new id) is missing. With withdraws, an id the ontology no
// longer holds whose withdraw the current load still shows (pending) comes
// back as that withdraw, as ListFoundryShownDecisions lists it.
func (s *mysqlDB) ListFoundryReviewDecisionsByID(ctx context.Context, ontologyID string, ids []string, withdraws bool) ([]*model.FoundryReviewDecision, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryReviewDecisionDrawnColumns+` FROM foundry_review_decisions
WHERE ontology_id=? AND id IN (`+idPlaceholders(len(ids))+`) ORDER BY id`, append([]any{ontologyID}, toAnySlice(ids)...)...)
	if err != nil {
		return nil, err
	}
	saved, err := scanAll(rows, scanFoundryReviewDecisionDrawn)
	if err != nil || !withdraws {
		return saved, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT "+foundryReviewDecisionColumns+` FROM foundry_load_decisions
WHERE load_id=(SELECT current_load_id FROM foundry_ontologies WHERE id=?) AND kind=?
  AND id IN (`+idPlaceholders(len(ids))+`) ORDER BY id`, append([]any{ontologyID, string(model.FoundryReviewWithdraw)}, toAnySlice(ids)...)...)
	if err != nil {
		return nil, err
	}
	shown, err := scanAll(rows, scanFoundryReviewDecision)
	if err != nil || len(shown) == 0 {
		return saved, err
	}
	all := append(saved, shown...)
	slices.SortFunc(all, func(a, b *model.FoundryReviewDecision) int { return strings.Compare(a.ID, b.ID) })
	return all, nil
}
