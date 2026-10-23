# Developer API

All routes use `/api/v1`. There are no accounts or sign-in: a request without an `Authorization` header acts as the person using this computer, who owns every workspace. Scripts and the SDK send `Authorization: Bearer TOKEN` with a token created in **Settings → Developer tools** (POST `/workspaces/:workspaceId/tokens` with `{ name, readOnly }`). Tokens are scoped to a workspace, can be read-only, and cannot create workspaces. An invalid or revoked token, or a non-Bearer `Authorization` header, is rejected with 401 rather than treated as the local person. The `Host` header must be a loopback name or the `APP_ORIGIN` host, and mutation requests from browsers must have the configured `APP_ORIGIN` or its loopback equivalent.

Workspace routes begin `/workspaces/:workspaceId`.

Each area owns its section below; add new routes to your own section.

### Shared (foundation)

| Method / path                                         | Input / result                 |
| ----------------------------------------------------- | ------------------------------ |
| GET `/resources/:kind?offset=0&limit=100&search=term` | `{ items: Resource[], total }` |

### Workspace: home, activity, reviews, settings

| Method / path                                           | Input / result                                                                                                                                                                                                                                                        |
| ------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| GET `/overview`                                         | Workspace and current catalog, sources, runs, proposals, published assets                                                                                                                                                                                             |
| GET `/home`                                             | Workspace home summary: connection health, pipeline builds, open findings, pending reviews, ontology snapshot, recent activity, dashboards                                                                                                                            |
| GET `/home/insights`                                    | “Some things we noticed”: up to three findings from up to three drivers, scan state, coverage and priorities. Never queries rows; selected-driver dependency changes queue a background scan, at most once per 15 minutes; active scans coalesce changes              |
| POST `/home/insights/scan`                              | Editor or owner. `{}`; check again now or retry, bypassing the automatic cooldown; an active scan is reused                                                                                                                                                           |
| POST / DELETE `/home/insights/:id/dismiss`              | `{ version }`; dismiss for yourself, or undo. It returns only if it gets more severe, grows by half or reverses                                                                                                                                                       |
| PUT `/home/insights/priorities`                         | Owner. `{ drivers }`: up to three candidate driver ids, or `[]` for the automatic choice                                                                                                                                                                              |
| PUT `/home/insights/settings`                           | Owner. Partial `InsightThresholdsSchema`, merged into saved settings (omitted fields are preserved): period and segment minimums, change thresholds, `minZ`, top-K size; segment checks use a z floor of 4; cohort settings are reserved while that check is deferred |
| GET `/activity`                                         | Activity feed (jobs, runs, edits, publications) newest first; `kind` (comma-separated: sync, upload, pipeline, model, job, run, setup, edit, publish), `limit` (max 200), `offset`                                                                                    |
| GET / PUT `/provider`                                   | Model provider settings; keys are never returned. `azure` and `bedrock` take an endpoint as `baseUrl`, and a deployment name or model ID as `model` (see the GUIDE)                                                                                                   |
| GET `/events`                                           | Authenticated server-sent events: `{runs, jobs, reviews}` with active or last-24h runs and jobs (id, kind, name, times and status fields only) and the pending review count; sent when something changes                                                              |
| PUT `/proposals/:id`                                    | Complete `Bundle`; creates and validates a new proposal                                                                                                                                                                                                               |
| POST `/proposals/:id/validate`                          | `{}`; creates a fresh validated proposal                                                                                                                                                                                                                              |
| GET `/proposals/:id/comparison`                         | Read-only definition comparison against the active published bundle; asset changes, metadata and validation staleness; available to read-only tokens                                                                                                                  |
| POST `/proposals/:id/publish`                           | `{ hash }`; atomically publish the reviewed bundle                                                                                                                                                                                                                    |
| POST `/proposals/:id/reject`                            | `{}`; reject an unpublished proposal                                                                                                                                                                                                                                  |
| POST `/rollback`                                        | `{}`; revalidate and publish previous definitions, retaining business edits                                                                                                                                                                                           |
| GET / POST `/definitions/export`, `/definitions/import` | Versioned bundles; imports become proposals                                                                                                                                                                                                                           |

### Developer tokens

| Method / path        | Input / result                                                                 |
| -------------------- | ------------------------------------------------------------------------------ |
| GET `/tokens`        | `{ items: {id, name, readOnly, createdAt}[] }`; opaque revocation handles only |
| POST `/tokens`       | `{name, readOnly?}` → `{token, id}`; copy the token once                       |
| DELETE `/tokens/:id` | Revoke one token; `{ok:true}`                                                  |

These routes require owner permissions and a writable credential or the local browser. The SDK exposes `tokens()`, `createToken(name, readOnly)` and `revokeToken(id)`. Authentication digests and stored credential values are not included in the inventory. Revocation ids returned by older versions remain accepted.

### Data

| Method / path                                  | Input / result                                                                                                      |
| ---------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| POST `/ingest`                                 | `{ name, rows: object[], datasetId? }`; maximum 10,000 rows per request                                             |
| POST `/upload`                                 | Multipart file; CSV, JSON, JSONL, Parquet; maximum 256 MB                                                           |
| POST `/sources/test`                           | `{ name, kind, config }`; discovery and key information                                                             |
| POST `/sources`                                | `{ name, kind, config, schedule? }`; encrypted source definition                                                    |
| PUT `/sources/:id/schedule`                    | `{ schedule: string \| null }`; owner-only UTC cadence for every table in this connection                           |
| POST `/sources/:id/sync`                       | `{}`; durable job                                                                                                   |
| POST `/datasets/:id/preview`                   | `{ offset?, limit? }`; rows and profile                                                                             |
| GET `/datasets/:id/export`                     | Download active snapshot; default Parquet, `?format=csv` for every row as CSV                                       |
| PATCH `/datasets/:id`                          | `{ description }`; preserve manually authored description                                                           |
| PUT `/datasets/:id/privacy`                    | Owner only; `{ additionalColumns: string[] }`; returns effective dataset profile                                    |
| POST `/datasets/:id/versions/:version/preview` | `{ offset?, limit? }`; immutable selected snapshot rows, profile, timestamp and exact version citation; read access |
| POST `/datasets/:id/accept-version`            | `{}`; accept staged schema-changing snapshot                                                                        |
| POST `/query`                                  | `{ inputs: datasetId[], sql }`; read-only result and profile                                                        |

### Pipelines

| Method / path             | Input / result                                                       |
| ------------------------- | -------------------------------------------------------------------- |
| POST `/pipelines/:id/run` | `{}`; durable pipeline job                                           |
| POST `/pipelines/draft`   | `PipelineDefinition`; compile, execute, and create a review proposal |

### Ontology

| Method / path                  | Input / result                                                                                                                    |
| ------------------------------ | --------------------------------------------------------------------------------------------------------------------------------- |
| POST `/actions/preview`        | `{ objectId, values }`; before, after and revision                                                                                |
| POST `/actions/execute`        | `{ objectId, values, revision, idempotencyKey, revert? }`; audit event                                                            |
| POST `/actions/change/preview` | `{ operation: 'create', objectType, values }` or `{ operation: 'link' \| 'unlink', name, from, to }`; affected records and hash   |
| POST `/actions/change/execute` | `{ action, hash, idempotencyKey }`; execute the exact previewed change                                                            |
| GET `/records/:logicalId`      | Current-generation record with effective values, workspace edits, related records, and action history; stable across publications |
| GET `/objects/:id/neighbors`   | `{ ids }`; native Helix graph traversal                                                                                           |

### Analyst

| Method / path                                       | Input / result                                                                                            |
| --------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| POST `/assistant/runs`                              | `{ goal, intent?: 'build' \| 'answer' \| 'auto', parentId?, context?: {page?,resourceId?} }`; durable run |
| POST `/assistant/runs/:id/cancel`                   | `{}`; cancel a run                                                                                        |
| POST `/assistant/runs/:id/resume`                   | `{}`; resume an interrupted run from saved stages                                                         |
| POST `/assistant/runs/:id/save-analysis`            | `{}`; create a dashboard proposal from an executed answer                                                 |
| GET `/live`                                         | `{ available, reason? }`; hands-free voice needs the OpenAI provider with GPT-Live (`gpt-live-1`) access  |
| POST `/live/sessions`                               | `{ sdp, chatId? }` → `{ sessionId, sdp, chatId }`; starts a GPT-Live WebRTC session for the browser       |
| POST `/live/sessions/:id/delegations/:delegationId` | `{}` → `{ status, action, question, runId, chatId }`; the Analyst run answering a delegation              |
| POST `/live/sessions/:id/context`                   | `{ chatId?, typed? }`; the chat a session moved into, or a question typed during voice                    |
| GET `/live/sessions/:id`                            | Session state and per-delegation timings (ms): resolved, enqueued, finished, answerSent, answerDelivered  |
| DELETE `/live/sessions/:id`                         | Releases the server side after the browser closes the session                                             |

### Setup: hosting providers

| Method / path                       | Input / result                                                                                                                 |
| ----------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| GET `/hosting`                      | Provider availability and connected account references; no secrets                                                             |
| POST `/hosting/:provider/authorize` | `{ returnTo?: 'onboarding' \| 'sources' }`; OAuth URL; browser only, API tokens are rejected                                   |
| GET `/hosting/:provider/callback`   | Single-use browser-bound OAuth callback                                                                                        |
| POST `/hosting/:provider/token`     | `{ apiKey }` for Neon/Supabase or `{ serviceTokenId, serviceToken }` for PlanetScale; validation and encrypted account storage |
| POST `/hosting/:provider/options`   | `{ accountId, path: string[] }`; projects, databases, branches, or readiness                                                   |
| POST `/hosting/:provider/connect`   | `{ accountId, path, password? }`; discovery and opaque connection reference                                                    |

MySQL config: `{ host, port:3306, database, user, password, table, tls, ca? }`. PostgreSQL accepts `{ connectionString, schema?, table, tls?, ca? }` or the existing individual connection fields. URL credentials are encrypted with the rest of the connector configuration. URL mode defaults to verified TLS; an explicit `sslmode=disable` or `tls:false` disables it. Supported URL options are `sslmode`, `ssl`, and `application_name`; local certificate file paths are rejected. REST config: `{ url, itemsPath?, token?, header?, pagination:'none'|'next'|'cursor'|'offset', nextPath?, cursorParam?, offsetParam?, limitParam?, pageSize? }`. S3 config: `{ bucket, key, prefix?, region?, endpoint?, accessKeyId?, secretAccessKey? }`.

PostgreSQL/MySQL import jobs attempt native CDC automatically. `source.data.sync` exposes the mode, status, reason, last captured event, and change/applied counters (`SourceSyncState` in the shared package and SDK). Working CDC ignores `schedule`; the optional UTC schedule is only the snapshot fallback, defaulting to five minutes. REST/S3 use snapshot refreshes because these connectors do not expose database logs. Uploaded files and ingestion requests remain explicit inputs. See `OPERATIONS.md` for privileges and materialization limits.

Omitting `intent` retains the API's `build` default. `auto` persists a bounded answer/build routing stage and accounts for it in existing budgets. A run's `events` record each stage with its time. Once a run is known to answer a question, `outline` lists its major steps (`understand`, `analyze`, `check` and, where result rows may be sent to the model, `write`). Questions get `estimate`, set once when they are asked: `{ low, high, samples }` in milliseconds from the latest comparable answers (same provider and model, with or without a summary, data of the same order of magnitude), or `null` with fewer than three or too wide a spread; a resumed run has none. A first, uninterrupted answer records `timing: { key, took }`. Before the summary, code checks the full result and stores `answer.check` (`AnswerCheck` in the shared package): its `kind` (`empty`, `value`, `ranking`, `trend`, `breakdown` or `table`), `label` and `measure` columns, a ranking's `order` and how many rows share first place (`leaders`), key columns (`keys`), and counts of exactly repeated rows (`duplicates`) and rows without the measure (`missing`). Context IDs are resolved server-side within the caller's workspace; secrets and inactive generations are not accepted. Read-only tokens may ask questions, but a request routed to build stops before generation. `resources/object` supports `type`, `search`, and exact `field`/`value` filters on effective workspace values. Dashboard routes use `/dashboards/:encodedName`; record routes use `/records/:logicalId`.

A proposal contains `bundle`, `validation`, `hash`, and `status`. The validation report includes executed checks, previews, and exact input versions. Treat HTTP 409 as a request to refresh/review; do not blindly retry publication or actions with changed payloads. Reusing an action key with a different payload is rejected.

`Resource` is `{ id, workspaceId, kind, name, description, revision, createdAt, updatedAt, data }`. Shared schemas and types are in `packages/shared/src/index.ts`. The interactive OpenAPI document at `/api/docs` describes the running API.

Build and package both workspace libraries with `pnpm build:sdk`, `pnpm -C packages/shared pack`, and `pnpm -C packages/sdk pack`. Install the resulting tarballs in your client project:

```ts
import { FoundryClient } from "@helix-foundry/sdk";

const foundry = new FoundryClient({
  baseUrl: "http://localhost:3001",
  workspaceId: process.env.FOUNDRY_WORKSPACE!,
  token: process.env.FOUNDRY_TOKEN!,
});

const dataset = await foundry.ingest("Orders", [
  { order_id: "o1", customer_id: "c1", amount: 42 },
]);
const run = await foundry.requestAssistance("Create a revenue dashboard", {
  resourceId: dataset.id,
});
// Use foundry.ask(question) or foundry.build(goal) for an explicit intent.
// Review the resulting proposal and its validation before calling publish().
```

**Definition comparison.** `FoundryClient.proposalComparison(id)` returns `activeGeneration`, `validatedGeneration`, `stale`, exact `staleInputs`, `counts`, `changes` and `metadata`. Each asset change has `kind`, `name`, `status` (`added`, `changed`, `removed`), `before`, `after` and changed top-level `fields`. Catalog identity is `datasetId`; other asset identities are their exact names. Arrays retain order and SQL is compared as text. Missing active publication provenance returns 409; ambiguous duplicate asset names return 422. The endpoint does not execute, validate, publish or estimate affected records.

CSV downloads use one immutable active snapshot and include its version in
`x-foundry-version`. Column names are headers. Non-null fields are quoted,
nulls are empty fields, and nested values are JSON within a quoted field.
Integer values retain their exact decimal representation. Large exports use
bounded batches and the executor's existing result-size and time limits.

Owners can use `FoundryClient.setDatasetPrivacy(datasetId, additionalColumns)` or `PUT /datasets/:id/privacy` to mark additional columns sensitive. Policy adds to heuristic detection and cannot disable detected protection. It applies to future hosted AI sample context and Home metric suggestions, persists independently of immutable version profiles, and follows ordinary refreshes, accepted schema changes and stable pipeline dataset identities. Names absent after a schema change remain protected if they return; they can be removed explicitly. Unknown new names are rejected. Local AI and hosted `allowRawData: true` retain their existing behavior. This does not restrict authorized dataset preview, query or export access or recall requests already sent.

### Change a connection refresh schedule

Owners can change snapshot cadence in **Data → connection → Details**, through `PUT /sources/:id/schedule`, or with `FoundryClient.setConnectionSchedule(sourceId, schedule)`. A cron expression uses UTC; `null` restores the connector's Automatic default. The change applies atomically to every source table sharing that connection, including older groups recorded by import operations, and preserves credentials, source identities, existing dataset snapshots, jobs and live-capture state. It does not retest or reconnect the source.

For snapshot mode, the next refresh is recalculated from the saved cadence. You can still refresh by hand: **Refresh** on the Data page, **Sync now** on the connection's page, or `POST /sources/:id/sync`. A refresh already running or queued can finish on its existing lifecycle. Live CDC and capability checks remain independent of cron; their next scheduled snapshot stays unset until snapshot fallback is used. The saved expression then governs fallback refreshes. Automatic uses five minutes for ordinary REST/S3/database fallback, one hour for Stripe, WorkOS and Neo4j, and six hours for PostHog. A schedule runs at most once a minute: a six-field expression's leading seconds field may name only one second. Invalid or more frequent expressions return 400 without changing the connection; non-owners cannot save a schedule.
