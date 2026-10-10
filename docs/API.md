# Developer API

All routes use `/api/v1`. There are no accounts or sign-in: a request without an `Authorization` header acts as the person using this computer, who owns every workspace. Scripts and the SDK send `Authorization: Bearer TOKEN` with a token created in **Settings → Developer tools** (POST `/workspaces/:workspaceId/tokens` with `{ name, readOnly }`). Tokens are scoped to a workspace, can be read-only, and cannot create workspaces. An invalid or revoked token, or a non-Bearer `Authorization` header, is rejected with 401 rather than treated as the local person. The `Host` header must be a loopback name or the `APP_ORIGIN` host, and mutation requests from browsers must have the configured `APP_ORIGIN` or its loopback equivalent.

Workspace routes begin `/workspaces/:workspaceId`.

Each area owns its section below; add new routes to your own section.

### Shared (foundation)

| Method / path                                         | Input / result                 |
| ----------------------------------------------------- | ------------------------------ |
| GET `/resources/:kind?offset=0&limit=100&search=term` | `{ items: Resource[], total }` |

### Workspace: home, activity, reviews, settings

| Method / path                                           | Input / result                                                                                                                                                                                           |
| ------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| GET `/overview`                                         | Workspace and current catalog, sources, runs, proposals, published assets                                                                                                                                |
| GET `/home`                                             | Workspace home summary: connection health, pipeline builds, open findings, pending reviews, ontology snapshot, recent activity, dashboards                                                               |
| GET `/activity`                                         | Activity feed (jobs, runs, edits, publications) newest first; `kind` (comma-separated: sync, upload, pipeline, model, job, run, setup, edit, publish), `limit` (max 200), `offset`                       |
| GET / PUT `/provider`                                   | Model provider settings; keys are never returned                                                                                                                                                         |
| GET `/events`                                           | Authenticated server-sent events: `{runs, jobs, reviews}` with active or last-24h runs and jobs (id, kind, name, times and status fields only) and the pending review count; sent when something changes |
| PUT `/proposals/:id`                                    | Complete `Bundle`; creates and validates a new proposal                                                                                                                                                  |
| POST `/proposals/:id/validate`                          | `{}`; creates a fresh validated proposal                                                                                                                                                                 |
| POST `/proposals/:id/publish`                           | `{ hash }`; atomically publish the reviewed bundle                                                                                                                                                       |
| POST `/proposals/:id/reject`                            | `{}`; reject an unpublished proposal                                                                                                                                                                     |
| POST `/rollback`                                        | `{}`; revalidate and publish previous definitions, retaining business edits                                                                                                                              |
| GET / POST `/definitions/export`, `/definitions/import` | Versioned bundles; imports become proposals                                                                                                                                                              |

### Data

| Method / path                       | Input / result                                                          |
| ----------------------------------- | ----------------------------------------------------------------------- |
| POST `/ingest`                      | `{ name, rows: object[], datasetId? }`; maximum 10,000 rows per request |
| POST `/upload`                      | Multipart file; CSV, JSON, JSONL, Parquet; maximum 256 MB               |
| POST `/sources/test`                | `{ name, kind, config }`; discovery and key information                 |
| POST `/sources`                     | `{ name, kind, config, schedule? }`; encrypted source definition        |
| POST `/sources/:id/sync`            | `{}`; durable job                                                       |
| POST `/datasets/:id/preview`        | `{ offset?, limit? }`; rows and profile                                 |
| GET `/datasets/:id/export`          | Download active Parquet snapshot                                        |
| PATCH `/datasets/:id`               | `{ description }`; preserve manually authored description               |
| POST `/datasets/:id/accept-version` | `{}`; accept staged schema-changing snapshot                            |
| POST `/query`                       | `{ inputs: datasetId[], sql }`; read-only result and profile            |

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

Omitting `intent` retains the API's `build` default. `auto` persists a bounded answer/build routing stage and accounts for it in existing budgets. Context IDs are resolved server-side within the caller's workspace; secrets and inactive generations are not accepted. Read-only tokens may ask questions, but a request routed to build stops before generation. `resources/object` supports `type`, `search`, and exact `field`/`value` filters on effective workspace values. Dashboard routes use `/dashboards/:encodedName`; record routes use `/records/:logicalId`.

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
