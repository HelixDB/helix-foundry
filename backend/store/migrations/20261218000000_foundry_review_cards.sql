-- +goose Up
-- Review cards: an index of what each live load asks a person to decide, so
-- the review walk pages it, counts what is open and draws its spot checks
-- without reading the whole load's reviews in a browser. The graph worker
-- builds it (backend/reviewcards, workers/foundry-graph-worker's indexer)
-- from each document's reviews, read from the load's database; nothing reads
-- it into a load, and it is rebuilt from the graph whenever it is missing or
-- of an older builder version, so every row here can be dropped and made
-- again.
--
-- foundry_review_loads is one row per live load: its full pass (a request,
-- the pass running and its cursor through the load's extractions), the
-- builder version and time of the last pass that ended, its open count
-- (recounted when dirty_at passes counted_at, at most 10,000 and whether
-- more), and its backoff while its database can't be read.
--
-- foundry_review_owed is the documents of a load whose cards are owed a
-- rebuild: one row a document, owed again in place (owed_at), due at due_at,
-- claimed by a worker (claimed_by) for a lease that due_at ends, and how many
-- reads of it failed (attempts; 8 sets it aside until it is owed again).
--
-- foundry_review_docs is each indexed document of a load: its builder
-- version and how many quotes of facts it holds in each score band, which a
-- spot check's quote is drawn from.
--
-- foundry_review_cards is a card a load asks about, by card_id (the SHA-256
-- of its decision identity, so a review keyed again keeps its card): a name
-- (kind resolve), a value (prop) or a held claim (release), in its lane
-- (decide, fyi, or pool: a held claim no approval writes, drawn for spot
-- checks only), with what the walk orders it by (per_decision, facts,
-- kind_order, shuffle). A held claim that waits on a name points at the
-- name's card (on_card). identity_key is foundry_review_decisions'
-- identity_key expression over the columns that kind's decision fills, NULL
-- elsewhere exactly as the decisions' CHECK pins them, so a decision and its
-- card join on it whatever display columns hold; a held claim joins its
-- decisions by place instead.
--
-- foundry_review_draws is a load's spot checks, insert-only: each at its
-- place (document, section, byte range and link), a held claim with its key
-- and the hash of its statement, a fact's quote with its fact's key.
--
-- foundry_review_decisions.name_hash is the hash of a resolve's type and
-- name, indexed with its ontology and kind, so the decisions of a name are
-- read by an index of about 150 bytes a key; a reader compares type and
-- norm_name too, so two names never merge on a collision.
-- foundry_review_labels_place_idx reads the labels at a place.
--
-- Additive: five new tables, a virtual column and two indexes; nothing here
-- changes or deletes a row.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the tables, the column and the indexes stay: an
-- older binary neither reads nor writes them. After a roll forward the
-- indexer finds rows of loads it no longer owns and clears them, and its
-- version check rebuilds every current load. The Down is for a local
-- database.
CREATE TABLE foundry_review_loads (
  load_id         CHAR(26)          NOT NULL PRIMARY KEY,
  ontology_id     CHAR(26)          NOT NULL,
  version         SMALLINT UNSIGNED NULL,
  full_owed_at    DATETIME(6)       NULL,
  full_started_at DATETIME(6)       NULL,
  full_cursor     CHAR(26)          NULL,
  indexed_at      DATETIME(6)       NULL,
  open_cards      INT UNSIGNED      NOT NULL DEFAULT 0,
  more_cards      BOOLEAN           NOT NULL DEFAULT FALSE,
  dirty_at        DATETIME(6)       NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  counted_at      DATETIME(6)       NULL,
  attempts        SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  retry_at        DATETIME(6)       NULL,
  INDEX foundry_review_loads_ontology_idx (ontology_id),
  CONSTRAINT foundry_review_loads_shape_chk CHECK (
    (full_started_at IS NULL) = (full_cursor IS NULL) AND
    (full_started_at IS NULL OR full_owed_at IS NOT NULL) AND
    open_cards <= 10000
  )
);

CREATE TABLE foundry_review_owed (
  load_id    CHAR(26)          NOT NULL,
  doc_id     CHAR(26)          NOT NULL,
  owed_at    DATETIME(6)       NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  due_at     DATETIME(6)       NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  claimed_by VARCHAR(64)       NULL,
  attempts   SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (load_id, doc_id),
  INDEX foundry_review_owed_due_idx (due_at)
);

CREATE TABLE foundry_review_docs (
  load_id    CHAR(26)          NOT NULL,
  doc_id     CHAR(26)          NOT NULL,
  version    SMALLINT UNSIGNED NOT NULL,
  q_09       INT UNSIGNED      NOT NULL,
  q_08       INT UNSIGNED      NOT NULL,
  q_07       INT UNSIGNED      NOT NULL,
  q_06       INT UNSIGNED      NOT NULL,
  q_low      INT UNSIGNED      NOT NULL,
  q_none     INT UNSIGNED      NOT NULL,
  indexed_at DATETIME(6)       NOT NULL,
  PRIMARY KEY (load_id, doc_id)
);

CREATE TABLE foundry_review_cards (
  load_id        CHAR(26)         NOT NULL,
  card_id        BINARY(32)       NOT NULL,
  ontology_id    CHAR(26)         NOT NULL,
  kind           VARCHAR(8)       CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  lane           VARCHAR(8)       CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  kind_order     TINYINT UNSIGNED NOT NULL,
  on_card        BINARY(32)       NULL,
  doc_id         CHAR(26)         NOT NULL,
  type           VARCHAR(120)     COLLATE utf8mb4_bin NULL,
  subject_type   VARCHAR(120)     COLLATE utf8mb4_bin NULL,
  object_type    VARCHAR(120)     COLLATE utf8mb4_bin NULL,
  norm_name      VARCHAR(512)     COLLATE utf8mb4_bin NULL,
  entity_key     VARCHAR(512)     COLLATE utf8mb4_bin NULL,
  prop           VARCHAR(120)     COLLATE utf8mb4_bin NULL,
  claim_key      CHAR(64)         CHARACTER SET ascii COLLATE ascii_bin NULL,
  claim_section  INT UNSIGNED     NULL,
  claim_start    INT UNSIGNED     NULL,
  claim_end      INT UNSIGNED     NULL,
  statement_hash CHAR(64)         CHARACTER SET ascii COLLATE ascii_bin NULL,
  link           VARCHAR(255)     COLLATE utf8mb4_bin NULL,
  review_key     VARCHAR(256)     COLLATE utf8mb4_bin NOT NULL,
  review_keys    JSON             NOT NULL,
  review_count   INT UNSIGNED     NOT NULL,
  confidence     DOUBLE           NULL,
  band           VARCHAR(4)       CHARACTER SET ascii COLLATE ascii_bin NULL,
  blocked        INT UNSIGNED     NOT NULL,
  facts          INT UNSIGNED     NOT NULL,
  per_decision   DOUBLE           NOT NULL,
  shuffle        INT UNSIGNED     NOT NULL,
  -- foundry_review_decisions.identity_key's expression over kind-gated
  -- values: NULL exactly where the decisions' CHECK pins NULL.
  identity_key BINARY(32) GENERATED ALWAYS AS (IF(kind='release', NULL, UNHEX(SHA2(CAST(JSON_ARRAY(
    kind, type, IF(kind='resolve', doc_id, NULL), IF(kind='resolve', norm_name, NULL),
    IF(kind='prop', entity_key, NULL), IF(kind='prop', prop, NULL), NULL, NULL, NULL, NULL
  ) AS CHAR), 256)))) STORED,
  PRIMARY KEY (load_id, card_id),
  INDEX foundry_review_cards_walk_idx (load_id, lane, per_decision DESC, facts DESC, kind_order, shuffle, card_id),
  INDEX foundry_review_cards_doc_idx (load_id, doc_id),
  INDEX foundry_review_cards_pool_idx (load_id, kind, band, shuffle),
  CONSTRAINT foundry_review_cards_shape_chk CHECK (
    (kind='release') = (band IS NOT NULL) AND
    (lane='pool') <= (kind='release') AND
    ((kind='resolve' AND lane IN ('decide', 'fyi') AND kind_order=1 AND type IS NOT NULL AND norm_name IS NOT NULL
       AND on_card IS NULL AND claim_key IS NULL) OR
     (kind='prop' AND lane IN ('decide', 'fyi') AND kind_order=2 AND type IS NOT NULL AND entity_key IS NOT NULL AND prop IS NOT NULL
       AND on_card IS NULL AND claim_key IS NULL) OR
     (kind='release' AND lane IN ('decide', 'pool') AND kind_order=0 AND claim_key IS NOT NULL AND statement_hash IS NOT NULL
       AND claim_section IS NOT NULL AND claim_start < claim_end AND (on_card IS NULL OR lane='decide')))
  )
);

CREATE TABLE foundry_review_draws (
  load_id        CHAR(26)     NOT NULL,
  seq            INT UNSIGNED NOT NULL,
  kind           VARCHAR(8)   CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  band           VARCHAR(4)   CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  doc_id         CHAR(26)     NOT NULL,
  claim_section  INT UNSIGNED NOT NULL,
  claim_start    INT UNSIGNED NOT NULL,
  claim_end      INT UNSIGNED NOT NULL,
  link           VARCHAR(255) COLLATE utf8mb4_bin NULL,
  confidence     DOUBLE       NULL,
  claim_key      CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NULL,
  statement_hash CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NULL,
  fact_key       VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
  drawn_at       DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (load_id, seq),
  INDEX foundry_review_draws_place_idx (load_id, doc_id, claim_section, claim_start, claim_end),
  CONSTRAINT foundry_review_draws_shape_chk CHECK (
    claim_start < claim_end AND
    band IN ('0.9', '0.8', '0.7', '0.6', 'low', 'none') AND
    ((kind='held' AND claim_key IS NOT NULL AND statement_hash IS NOT NULL AND fact_key IS NULL) OR
     (kind='quote' AND claim_key IS NULL AND statement_hash IS NULL AND fact_key IS NOT NULL))
  )
);

ALTER TABLE foundry_review_decisions
  ADD COLUMN name_hash BINARY(32) GENERATED ALWAYS AS (UNHEX(SHA2(CAST(JSON_ARRAY(type, norm_name) AS CHAR), 256))) VIRTUAL,
  ADD INDEX foundry_review_decisions_names_idx (ontology_id, kind, name_hash);

ALTER TABLE foundry_review_labels
  ADD INDEX foundry_review_labels_place_idx (ontology_id, doc_id, claim_section, claim_start, claim_end);

-- +goose Down
ALTER TABLE foundry_review_labels
  DROP INDEX foundry_review_labels_place_idx;
ALTER TABLE foundry_review_decisions
  DROP INDEX foundry_review_decisions_names_idx,
  DROP COLUMN name_hash;
DROP TABLE foundry_review_draws;
DROP TABLE foundry_review_cards;
DROP TABLE foundry_review_docs;
DROP TABLE foundry_review_owed;
DROP TABLE foundry_review_loads;
