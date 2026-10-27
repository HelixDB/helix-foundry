-- +goose Up
-- Additive defaults are safe for deploys that apply a schema diff.
ALTER TABLE foundry_load_extractions
 ADD COLUMN attributes INT NOT NULL DEFAULT 0,
 ADD COLUMN attribute_reviews INT NOT NULL DEFAULT 0,
 ADD COLUMN prop_conflicts INT NOT NULL DEFAULT 0,
 ADD COLUMN created INT NOT NULL DEFAULT 0,
 ADD COLUMN identifiers_unverified INT NOT NULL DEFAULT 0,
 ADD COLUMN attributes_over_entries INT NOT NULL DEFAULT 0,
 ADD COLUMN resolved_user INT NOT NULL DEFAULT 0,
 ADD COLUMN resolved_adjudicated INT NOT NULL DEFAULT 0,
 ADD COLUMN resolved_identifier INT NOT NULL DEFAULT 0,
 ADD COLUMN resolved_name INT NOT NULL DEFAULT 0,
 ADD COLUMN resolved_alias INT NOT NULL DEFAULT 0,
 ADD COLUMN resolved_created INT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE foundry_load_extractions
 DROP COLUMN attributes,
 DROP COLUMN attribute_reviews,
 DROP COLUMN prop_conflicts,
 DROP COLUMN created,
 DROP COLUMN identifiers_unverified,
 DROP COLUMN attributes_over_entries,
 DROP COLUMN resolved_user,
 DROP COLUMN resolved_adjudicated,
 DROP COLUMN resolved_identifier,
 DROP COLUMN resolved_name,
 DROP COLUMN resolved_alias,
 DROP COLUMN resolved_created;
