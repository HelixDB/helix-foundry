-- +goose Up
-- What a grantee's access to a load is decided from (package access). A
-- load's tables and table files are its version's generation's inputs, and
-- its documents foundry_load_documents; two things more are recorded here.

-- The loads made since grants could cover them: only these may be read
-- through grants. A load made before holds review decisions whose
-- documents were never recorded, so none is shareable until loaded again.
CREATE TABLE foundry_load_access (
  load_id    CHAR(26)    NOT NULL PRIMARY KEY,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
);

-- The documents a review decision's value came from: those its review
-- quoted, each time it was settled. A load applies a prop decision's value
-- whether or not it holds those documents, so a grantee covers the
-- decision only by covering them. Never removed with the decision: a load
-- keeps its copy of the decision (foundry_load_decisions).
CREATE TABLE foundry_review_decision_documents (
  decision_id CHAR(26) NOT NULL,
  document_id CHAR(26) NOT NULL,
  PRIMARY KEY (decision_id, document_id)
);

-- +goose Down
DROP TABLE foundry_review_decision_documents;
DROP TABLE foundry_load_access;
