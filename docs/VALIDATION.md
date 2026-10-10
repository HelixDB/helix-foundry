# Implementation and validation

This is an executable single-server MVP. Validation results below are from the local development machine on 23–24 September 2026; they are not a production SLA.

## Verified paths

- Native HelixDB v0.0.6 persistence, indexed workspace identities, conditional atomic metadata writes, stale-write rejection, native object relationships, and relationship deletion.
- Real DuckDB Parquet ingestion, profiling, aggregation, input filtering, visual DAG compilation, isolation from files/network/undeclared inputs, and rejected multi-statement SQL.
- Owner bootstrap, local sessions, one-use invitations, role checks, scoped read-only tokens, cross-workspace API denial, and cross-origin mutation denial.
- Golden customers/orders: exact metrics and key checks, reference coverage, source freshness guards, preserved manual descriptions, schema drift staging, resumable AI checkpoints, manual objects, and relationship edits surviving rematerialization.
- MySQL 8.4: information_schema discovery, keys/columns, bounded streaming, large unsigned integers, DECIMAL(30,8), temporal strings, Unicode, nulls, preservation through Parquet, schema changes, authentication/TLS errors, and a deliberately interrupted connection. Scheduling and duplicate job coalescing are tested separately with controlled connectors.
- REST offset pagination and cross-origin credential protection; S3-compatible listing and streaming with a local protocol fixture.
- Native Ollama, Claude Messages tool-output, and OpenAI Responses JSON-schema adapters have protocol tests. Provider errors do not trigger fallback.
- Chromium: login, empty and connected states, all six Add data options including MySQL/TLS, tested business-model review, publication, saved dashboard selection and deep links, related-record navigation, previewed edits and history, unchanged snapshot results, and viewer controls. Desktop and 390px mobile layouts were visually inspected; modal Escape/focus restoration and mobile navigation are covered.
- Docker image builds and the four data-platform services start. A Compose installation ingested and queried sample data. Backup/restore of its Helix and Parquet volumes retained one workspace, three datasets, and 340 rows. That run used the earlier `backup.sh` and `restore.sh`, which copied `.env` into the backup; the current scripts are covered in the next section.

## Connect, understand, and act redesign

The redesign passed 35 default unit/API/workflow tests and two browser acceptance tests, plus TypeScript checking and the production build. The default run skips the three opt-in Helix/MySQL integration tests; connector implementations were not changed by the redesign. The browser tests use an isolated workspace and remove its data afterward.

New coverage verifies answer/build automatic routing, accounted model usage, saved routing decisions on resumption, malformed output, call limits, cancellation, viewer denial before generation, authorized resource context and follow-ups, hosted masking, stable logical record routes, effective-value filters, and cross-workspace record denial. Existing tests continue to cover stale approvals and edits, definition rollback, refresh-preserved overlays, and publication validation. A live local Qwen request from an order record automatically chose an answer, executed SQL, and returned 35 delayed orders with source citations. Published views, record actions, data inspection, and the browser reference workflow do not require inference.

## Local AI workflow

A clean reference workspace completed the goal-to-review workflow using the digest-pinned local `qwen3:4b`. Given the sample datasets and a goal about customer revenue and delayed orders, it generated three catalog entries, one SQL pipeline, Customer and Order ontology types, a relationship, and a two-widget dashboard. Automatic validation repaired the ontology and dashboard drafts; all ten final checks passed without manual component edits. The reviewed bundle was then published through the API:

- 32 customer objects and 240 order objects.
- 240 customer/order relationships, covering all 32 customers.
- Revenue grouped by region and exactly 35 delayed orders.
- Every published query executed against declared snapshot versions.
- Six local model calls, 14,516 reported tokens, and approximately 153 seconds from the first stage to the ready proposal. Publication time is separate.

Dataset-name references are resolved only when they uniquely identify an authorized catalog entry. Chart presentation is inferred from executed result shape when a generated metric has multiple rows. These are technical corrections, not invented data or a template masquerading as model output. Business assumptions remain visible for human review. No hosted model calls were used for this workflow.

## Benchmark

On an Apple Silicon M1 Pro development machine with 16 GB RAM:

| Workload                              | Measurement |
| ------------------------------------- | ----------: |
| NDJSON to Parquet, 1,000,000 rows     |      472 ms |
| Concurrent analytical HTTP clients    |          25 |
| Executor slots                        |           2 |
| Median response including queue time  |    1,690 ms |
| Slowest response including queue time |    3,139 ms |

This benchmark excludes inference and measures the executor workload, not 25 browser sessions. The recorded 149 MB RSS is the benchmark client only and must not be represented as combined platform memory. Run `BENCH_HTTP=1 pnpm benchmark` with the executor available to reproduce it. Hardware, warm caches, storage, and query shape change results.

## Guided onboarding validation (24 September 2026)

The resumable setup adds durable provider preparation and upload/import progress, explicit source selections, grounded outcome recommendations, inline revision and tested preview, and publication-bound completion. The suite passes 46 default tests, three isolated browser tests, four MySQL/PostgreSQL integration checks, and a real pinned-Qwen onboarding evaluation. TypeScript and the production build pass.

The local evaluation completed in 117 seconds with one recommendation call, five build/repair calls, 7,631 reported model tokens, ten passing validation checks, and one published dashboard. It used no hosted provider. Browser tests use a deterministic provider fixture plus real DuckDB; those are separate evidence from the real model run. Both desktop and 390px setup/review layouts were inspected. Existing publication, related-record edits, saved view selection and viewer screens still pass.

Browser acceptance now starts a separate installation on ports 3101/3102/5174 with its own metadata store and data directory. It cannot bootstrap or seed the live installation. No demo account or live sample data was created during this change. See [onboarding](onboarding.md) for routes, idempotency, recovery and fixture commands.

## Backup, restore and upgrade scripts (10 October 2026)

The current `scripts/backup.sh`, `restore.sh` and `upgrade.sh` leave the keys out of backups, and `upgrade.sh` backs up before every upgrade. They have not yet run against a real Docker or Compose installation.

What was validated: `tests/scripts.test.ts` runs the scripts against a stub `docker` (`tests/fixtures/fake-docker.sh`) in throwaway git checkouts with a bare upstream. It checks the Docker calls each script makes and their order, that no key, client secret or passphrase reaches a backup, the output or another command (also under `bash -x`), file modes, failure paths that must change nothing or must start the services again, finishing an unfinished upgrade, the HelixDB image checks, and going back with the commands `upgrade.sh` prints, also to a release from before `upgrade.sh` (with that release's scripts in `tests/fixtures/release-before-upgrade`). These tests ran on macOS with bash 3.2 and BSD tools, under OpenSSL 3 and LibreSSL 3.3. CI runs them on `ubuntu-latest`. The HelixDB storage facts the scripts encode come from the HelixDB release notes.

Not yet validated:

- What Docker records as the image of the HelixDB container under Compose v2 (`docker inspect --format '{{.Config.Image}}'`). The scripts compare it with the pin in `compose.yaml` by digest, then by version. If Docker recorded only an image ID, `restore.sh` would refuse new backups with "cannot tell which HelixDB version is newer", and `upgrade.sh` would never report "Already up to date".
- Archiving and restoring real volumes with `alpine:3.22`, stopping and restarting real services, and the rebuild and restart in `upgrade.sh` and `restore.sh`.
- A real upgrade across a HelixDB pin change, and going back from it.
- `--env-file <(COMMAND)` with a real password manager, and the scripts on Linux outside CI.

Before relying on them, run `backup.sh`, `restore.sh` and an `upgrade.sh` that finds the installation up to date once against a real Compose v2 installation, and check what `docker inspect --format '{{.Config.Image}}' $(docker compose ps -q helix)` prints.

## Remaining validation and MVP limits

- Claude/OpenAI live checks require supplied credentials. No hosted credentials were available, so only protocol fixtures ran.
- The combined 16 GB local-inference target and an installation constrained to 8 GB still need dedicated peak-memory/load certification. The million-row benchmark does not certify million-object ontology publication.
- PostgreSQL 16 multi-table imports, authentication/TLS failures and schema drift were verified in an isolated container. Deployment-specific certificate chains and a real S3 service still require environment integration testing.
- Onboarding supports multi-table and multi-file selection, retaining one source record per table/file. Users explicitly choose selections; related tables are suggested from discovered foreign keys.
- The Advanced ontology graph shows the first 100 objects. Records uses 50-row pages with search and exact filters; relationship selection searches the workspace with 50 results at a time. Metadata listing currently reads a scoped collection before slicing, so very large ontology deployments need storage-level pagination.
- Visual DAGs cover common relational transformations; specialized SQL remains editable. Rejected-row evidence is available for single-input pipelines that retain a candidate key. Ambiguous joins still require business review.
- Background assistance prepares repair drafts after changes. Repairs are bounded and may still need a larger model or developer edits. It never publishes automatically.
- Action history and imported values are separate. Rollback revalidates the previous definitions against current snapshots and rematerializes them, preserving manual overlays; it does not undo business actions.
- Query execution is isolated and capped; this release does not provide distributed transactions with external sources, CDC, SSO, billing, or per-record LLM enrichment.
