package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

const foundryReviewLabelColumns = "id, ontology_id, load_id, drawn, doc_id, claim_section, claim_start, claim_end, claim_key, statement_hash, fact_key, link, confidence, verdict, decision_id, against, decided_by_user_id, decided_at"

// SaveFoundryReviewLabel keeps a person's word on a claim, l, read from load
// l.LoadID of a live ontology, and returns it as stored. One label per person
// and claim: saying it again replaces what the label says (its verdict, how
// it was drawn, what it checks and the score and load it was read at) and
// keeps its id. A label changes no decision and no load. ErrNotFound means
// the ontology is not live.
func (s *mysqlDB) SaveFoundryReviewLabel(ctx context.Context, workspaceID string, l *model.FoundryReviewLabel) error {
	if err := l.Validate(); err != nil {
		return err
	}
	return s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		if _, err := tx.lockLiveFoundryOntology(ctx, workspaceID, l.OntologyID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_review_labels
 (id, ontology_id, load_id, drawn, doc_id, claim_section, claim_start, claim_end, claim_key, statement_hash, fact_key, link, confidence, verdict, decision_id, against, decided_by_user_id)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON DUPLICATE KEY UPDATE load_id=VALUES(load_id), drawn=VALUES(drawn), statement_hash=VALUES(statement_hash), link=VALUES(link),
 confidence=VALUES(confidence), verdict=VALUES(verdict), decision_id=VALUES(decision_id), against=VALUES(against), decided_at=NOW(6)`,
			l.ID, l.OntologyID, l.LoadID, l.Drawn, l.DocID, l.Span.Section, l.Span.Start, l.Span.End, l.ClaimKey, l.StatementHash, l.FactKey,
			l.Link, l.Confidence, l.Verdict, l.DecisionID, l.Against, l.DecidedByUserID); err != nil {
			return err
		}
		// <=> is NULL-safe: a label is of a held claim's key or a fact's.
		stored, err := scanFoundryReviewLabel(tx.db.QueryRowContext(ctx, "SELECT "+foundryReviewLabelColumns+`
 FROM foundry_review_labels WHERE ontology_id=? AND decided_by_user_id=? AND doc_id=? AND claim_section=? AND claim_start=? AND claim_end=?
 AND claim_key <=> ? AND fact_key <=> ?`, l.OntologyID, l.DecidedByUserID, l.DocID, l.Span.Section, l.Span.Start, l.Span.End, l.ClaimKey, l.FactKey))
		if err != nil {
			return err
		}
		*l = *stored
		return nil
	})
}

// FoundryReviewLabelsOf lists the labels of one claim, by anyone, oldest
// first: a held claim's (claimKey) or an accepted fact's (factKey) at its
// place. A re-check reads the word it checks from them.
func (s *mysqlDB) FoundryReviewLabelsOf(ctx context.Context, ontologyID, docID string, span model.FoundryClaimSpan, claimKey, factKey *string) ([]*model.FoundryReviewLabel, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryReviewLabelColumns+`
 FROM foundry_review_labels WHERE ontology_id=? AND doc_id=? AND claim_section=? AND claim_start=? AND claim_end=?
 AND claim_key <=> ? AND fact_key <=> ? ORDER BY decided_at, id`, ontologyID, docID, span.Section, span.Start, span.End, claimKey, factKey)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundryReviewLabel)
}

// ListFoundryReviewLabels pages an ontology's labels by id, after afterID.
func (s *mysqlDB) ListFoundryReviewLabels(ctx context.Context, ontologyID, afterID string, limit int) ([]*model.FoundryReviewLabel, error) {
	if limit < 1 || limit > 10001 {
		return nil, fmt.Errorf("label limit must be 1..10001")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryReviewLabelColumns+`
 FROM foundry_review_labels WHERE ontology_id=? AND id>? ORDER BY id LIMIT ?`, ontologyID, afterID, limit)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundryReviewLabel)
}

// GetFoundryReviewDecision reads one saved decision of an ontology, with how
// the review queue drew its card. ErrNotFound means there is none.
func (s *mysqlDB) GetFoundryReviewDecision(ctx context.Context, ontologyID, id string) (*model.FoundryReviewDecision, error) {
	return scanFoundryReviewDecisionDrawn(s.db.QueryRowContext(ctx, "SELECT "+foundryReviewDecisionDrawnColumns+`
 FROM foundry_review_decisions WHERE ontology_id=? AND id=?`, ontologyID, id))
}

func scanFoundryReviewLabel(row rowScanner) (*model.FoundryReviewLabel, error) {
	var l model.FoundryReviewLabel
	var claim, hash, fact, link, decision, against sql.NullString
	var confidence sql.NullFloat64
	err := row.Scan(&l.ID, &l.OntologyID, &l.LoadID, &l.Drawn, &l.DocID, &l.Span.Section, &l.Span.Start, &l.Span.End, &claim, &hash, &fact,
		&link, &confidence, &l.Verdict, &decision, &against, &l.DecidedByUserID, &l.DecidedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	text := func(v sql.NullString) *string {
		if !v.Valid {
			return nil
		}
		return &v.String
	}
	l.ClaimKey, l.StatementHash, l.FactKey, l.Link, l.DecisionID = text(claim), text(hash), text(fact), text(link), text(decision)
	if confidence.Valid {
		l.Confidence = &confidence.Float64
	}
	if against.Valid {
		v := model.FoundryVerdict(against.String)
		l.Against = &v
	}
	if err := l.Validate(); err != nil {
		return nil, fmt.Errorf("stored label: %w", err)
	}
	return &l, nil
}
