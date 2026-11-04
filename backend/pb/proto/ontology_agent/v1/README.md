# Vendored ontology agent contract

These files are copied unchanged from the ontology agent's source, `proto/ontology_agent/v1/`, at commit `a48d3a0b82ea5086c65f7760cef4ffc82084a293` (contract 4.4.1: 4.4.0, plus `Read` and `Ask` taking `visibility`, what a reader holds of a load, the `view` read and the `not_visible` failure, and a query's rows taking `offset`, which a reader of part of a load can't use, all additive. 4.4.0 added Audit and Check, and an optional Revise origin). Its `schema/catalog.json` prices OpenAI's and Amazon Bedrock's models as well as Anthropic's; Azure deployments are priced from OpenAI's rows, and a Bedrock inference profile with no row of its own as the model it runs. The `ontology-agent` image Foundry runs beside its workers must be built from a commit that serves this contract.

| File | What it is |
|---|---|
| `agent.proto` | `ontology_agent.v1.Agent`: `Turn` streams events then one result; `Ingest` creates a load and fills it, `Extract` adds documents to a load and `DeleteLoad` removes one, the same way; `Read` reads a load with its version's rules applied, and `Compare` two loads of one version, both unary; `Ask` answers one question over a load, streaming events then one result; `Clues` streams the people a load's `[enrichments]` item could look up; `Revise` dry-runs edits of a draft against a load, unary; `Audit` audits a page of a load's documents (the foundry-heal-worker calls it) and `Check` a self-healing draft, both unary; `Title` writes a generation's title and `Probe` checks an endpoint's models for each role, both unary; `GetInfo` reports the contract version and the provider kinds the build calls. `Turn`, `Ask`, `Extract`, `Title` and `Probe` run on the models their request names, the endpoint's key in the call's `x-foundry-model-key` metadata |
| `schema/turn_request.json`, `schema/event.json`, `schema/turn_result.json` | JSON Schemas of a turn's request, events and result |
| `schema/ingest_request.json`, `schema/ingest_event.json`, `schema/ingest_result.json` | the same for `Ingest` |
| `schema/extract_request.json`, `schema/extract_event.json`, `schema/extract_result.json` | the same for `Extract` |
| `schema/delete_load_request.json`, `schema/delete_load_event.json`, `schema/delete_load_result.json` | the same for `DeleteLoad` |
| `schema/clues_request.json`, `schema/clues_event.json` | People enrichment's `Clues`: the people a load could look up, streamed as events with no separate result |
| `schema/read_request.json`, `schema/read_result.json` | a `Read`'s request and result; a request's `visibility` is what a reader holds of the load, and `view` reads the version as it sees it |
| `schema/compare_request.json`, `schema/compare_result.json` | a `Compare`'s request and result |
| `schema/audit_request.json`, `schema/audit_result.json` | an `Audit`'s request and findings |
| `schema/check_request.json`, `schema/check_result.json` | a `Check`'s request and draft gate result |
| `schema/ask_request.json`, `schema/ask_event.json`, `schema/ask_result.json` | the same as `Ingest`'s for `Ask`, whose request takes `visibility` as `Read`'s does |
| `schema/revise_request.json`, `schema/revise_result.json` | a `Revise`'s request and result |
| `schema/title_request.json`, `schema/title_result.json` | a `Title`'s request and result |
| `schema/probe_request.json`, `schema/probe_result.json` | a `Probe`'s request and result |
| `schema/catalog.json` | the models the agent prices from its table, with their rates in nano-USD per token; the backend embeds it (`ai/catalog.go`) to refuse unpriced models when settings are saved. Its rows may change without the contract's version |
| `schema/VERSION` | the contract version and a hash of the schemas |

Do not edit them here. To move to a new agent build:

1. Copy `agent.proto` and every file in `schema/` from the agent's commit and update the commit above.
2. Set `ontologyagent.ContractVersion` (`ontologyagent/contract.go`) to the first field of `schema/VERSION`.
3. From the module's root, run `buf generate && buf generate --template buf.internal.gen.yaml`, then `go test ./ontologyagent/`. The tests check every request the workers and foundry-read build against the request schemas, the fixtures the decoders are tested on against the result and event schemas, and the version against `VERSION`.
4. Build the agent image from that commit and deploy it with this backend.

Each generation, graph job, read, question, title and probe checks the sidecar's `GetInfo.contract_version` first, and that `GetInfo.providers` holds the workspace's provider kind. While it differs from `ContractVersion`, every generation, graph job and question the workers claim fails for good and every read is refused, so deploy the image and the backend together.
