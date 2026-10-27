The revise-*.json files are the engine's own wire JSON, contract 3.7.0, as
`agent_server::revise` serializes them (the engine's
crates/agent-server/tests/revise.rs fixtures; one JSON value per file, as
written):

- revise-request.json: a Request with `eval` (the fixture load, its
  `ontology_toml` and its read-only proxy) and two put edits; its
  `mappings_toml`, required from 3.12.0, is the engine's fixture target's
  (a version with no People enrichment item);
- revise-measured.json: that request revised on the comparable fixture load:
  a rule card and a constraint card capped at warn by the violations it adds;
- revise-no-load.json: a constraint edit with no `eval`: the
  no_comparable_load finding and an `unmeasured: "load"` card;
- revise-entities.json: the same edit measured with no reads left: an
  `unmeasured: "entities"` card;
- revise-budget.json: the measured request with a spent rules work budget:
  a refusal with one `budget` finding and no cards;
- revise-refused.json: an edit of an item Revise may not edit:
  EditNotAllowed.
