-- +goose Up
-- A question's client token is compared exactly. HTTP Ask's Idempotency-Key is
-- the caller's own, and under the server's collation Order-1 and order-1 would
-- be one question.
ALTER TABLE foundry_questions
  MODIFY COLUMN client_token VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL;

-- +goose Down
-- Refused while two of an ontology's tokens differ only in case or accents.
ALTER TABLE foundry_questions
  MODIFY COLUMN client_token VARCHAR(64) NULL;
