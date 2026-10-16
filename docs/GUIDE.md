# Helix Foundry guide

This guide is the detailed companion to the [README](../README.md). It covers what Helix Foundry does, how to connect each kind of source, how AI and review work, how the services fit together, and how to develop and test the code. To install it, start with the README or the [agent runbook](../AGENTS.md). To run an installation day to day, see [operations](OPERATIONS.md).

## Contents

- [Features](#features)
- [Finding your way around](#finding-your-way-around)
- [Guided setup and sample data](#guided-setup-and-sample-data)
- [Connecting sources](#connecting-sources)
  - [Hosted databases: Neon, Supabase and PlanetScale](#hosted-databases-neon-supabase-and-planetscale)
  - [PostgreSQL and MySQL](#postgresql-and-mysql)
  - [Neo4j](#neo4j)
  - [SQLite](#sqlite)
  - [Stripe](#stripe)
  - [WorkOS](#workos)
  - [PostHog](#posthog)
  - [REST APIs, files, S3 and the ingestion API](#rest-apis-files-s3-and-the-ingestion-api)
  - [Sync and change capture](#sync-and-change-capture)
- [AI and review](#ai-and-review)
- [Architecture](#architecture)
- [Security](#security)
- [Local development](#local-development)
- [Testing](#testing)
- [Backups and upgrades](#backups-and-upgrades)

## Features

Helix Foundry is an AI-assisted data workspace that runs on your own computer, built on HelixDB and DuckDB. You connect data, confirm a suggested ontology or describe an outcome, inspect tested proposals, and publish a shared ontology with key metrics on Home. Local inference is the default; Claude and OpenAI are explicit alternatives.

- **Sources:** PostgreSQL, MySQL and Neo4j discovery and streamed snapshot ingestion, SQLite database files, sign-in for Neon, Supabase and PlanetScale, Stripe, WorkOS and PostHog, REST pagination, S3-compatible file imports, CSV, JSON, JSONL and Parquet uploads, and a batch ingestion API.
- **Data:** immutable Parquet snapshots, profiles, key candidates, sensitivity hints, schema-drift review, version history and exports.
- **AI assistance:** generated catalog descriptions, executable pipeline SQL, ontology mappings and relationships, Home key metrics and action suggestions.
- **Pipelines and questions:** pipelines defined as node graphs (DAGs) that compile to SQL, or as editable SQL, with before and after previews. Read-only AI questions return executed results with snapshot citations.
- **Review:** evidence and query previews, bounded model repair, stale-proposal detection, atomic generation publication and definition rollback.
- **Ontology:** real ontology objects and native relationships in HelixDB, with audited edit overlays that survive rematerialisation.
- **Security:** workspace-scoped, optionally read-only, revocable API tokens, encrypted connector and model credentials, loopback host checks, and origin checks for browser changes.
- **Jobs:** persistent jobs with leases and crash recovery, scheduled source refreshes, background repair drafts, query isolation and budget controls.
- **Developer access:** OpenAPI at `/api/docs` and `/api/openapi.json`, a TypeScript SDK, definition import and export, and backup, restore and upgrade scripts.

## Finding your way around

The sidebar has **Home**, **Ontology**, **Data** and **Analyst**, with **Settings** at the bottom. Switch workspaces from the menu at the top of the sidebar.

- **Home** shows the workspace's key metrics: growth and analytics metrics picked by AI from your datasets' schemas, or by code until AI is set up. They are recomputed only when an input dataset gets a new version. Ask anything about your data from the box at the top. Home also shows connections, data health, items that need attention, where you left off, and an overview of the ontology. Old dashboard links open Home.
- **Ontology** opens on the **Explorer**, which shows your records and how they connect, as a graph or a table. **Object types** lists the published business types. Open a record to see its related records and source history. Edits to a record need a before and after preview and explicit application. Workspace edits do not change imported snapshots, Home's metrics or source systems. Old `/records` links open the Explorer. In the graph, **Find a record** searches the whole current workspace once you type at least two characters, including records the graph does not show. Choose a result with the mouse, or with the arrow keys and Enter. A record already in the graph is selected in place; any other record opens its neighbourhood, where **Open record** shows its details. **Show the best-connected records** returns to the overview. Search shows up to eight matches and their total; refine the text when there are more. Search does not edit records or imported data.
- **Data** brings every connection option into one **Add data** flow. Open a dataset to see a sample, and expand **Details** for schemas, profiles, versions, lineage, exports and connection settings. Choose **Snapshot** on a dataset to inspect current, historical or schema-changing pending rows and their selected profile/timestamp. The list shows the current and pending snapshots first, then the latest 30 historical ones by time, and counts any older ones it leaves out. Selection is read-only: it does not accept a schema, change current snapshots, or change views, exports and queries. Historical snapshots retain exact version citations; empty snapshots still show their typed schema. The SDK offers `previewDatasetVersion(datasetId, versionId, offset, limit)`. The old Advanced page now opens Data.
- **Analyst** holds your chats with the assistant. The assistant decides whether a request needs an executed answer or a tested proposal. Follow-ups keep the selected resource. Routing uses the same model-call and token budget as the rest of the run, and a saved routing decision is reused on resume when the inputs are unchanged. Read-only API tokens can investigate but cannot create definitions or change records. While the assistant works, each reply shows its major steps in plain words (understand the question, analyze the data, check the results, write the answer), as the server reports them, with **Show steps** for every step's time and the query used. Once there are enough comparable answers, it also gives the usual time range and says when an answer is taking longer than usual. The answer leads with its statement and puts the quantities the result rows back in bold. Rankings, trends and small breakdowns open as a chart, with the table, its measure first and keys last, one switch away.
- **Proposals** (`/proposals`) is where definitions from the assistant wait for review before publication. Guided setup publishes its validated build of the confirmed ontology automatically.
- **Settings** has **AI & models** (choose Local, Claude, OpenAI, Azure OpenAI, Amazon Bedrock or Custom and enter an API key) and **Developer tools** (create API tokens, inspect their access level, and revoke unused tokens). Other AI settings are set through the API; see [AI and review](#ai-and-review).

## Guided setup and sample data

While a workspace's setup is incomplete, opening the app goes straight to setup. There is no account to create. Setup has three steps.

1. **AI.** Choose **Local** (the default, using `qwen3:4b`), **Claude**, **OpenAI**, **Azure OpenAI**, **Amazon Bedrock** or **Custom**. Claude and OpenAI need an API key, and their default models are `claude-sonnet-4-6` and `gpt-5.4-mini`. Azure OpenAI takes the resource's endpoint (`https://RESOURCE.openai.azure.com`), a deployment name and the resource's API key. Amazon Bedrock takes an AWS region, a model ID and an Amazon Bedrock API key. See [AI and review](#ai-and-review) for Azure OpenAI, Amazon Bedrock and Custom. **Continue** opens the Data step straight away, and the provider is prepared in the background. For Local, the app checks the release-pinned model, downloads it through Ollama if it is missing, verifies its digest and tests structured output. If preparation fails, the later steps show the error with **Retry AI connection** and **Change AI choice**. Setup still works without AI; AI only adds descriptions.
2. **Data.** Connect your sources and let them import completely, or choose **Continue with sample data**. Nothing is seeded automatically.
3. **Ontology.** Helix Foundry computes a suggested ontology from the data: entities consolidated across sources by matching keys, and the links between them. The suggestion is deterministic. When AI is ready, a background run rewrites only its summary, descriptions and link names. Rename or exclude entities, then click **Looks right — build my workspace**. The build runs as code without model calls, shows its progress in plain stages, and publishes the workspace. Home then opens with key metrics.

Progress is saved on the server. If you close the browser, background work continues, and reopening the app returns you to the same step.

**Sample data** models Loopwell, a fictional B2B feedback and roadmap SaaS company, as it would look connected from PlanetScale, WorkOS, PostHog, Stripe and an S3 bucket. It has 11 linked datasets in which the same customers appear in every system, with realistic gaps to reconcile into one ontology: billing emails that differ from logins, help desk requests without an account, and mistyped campaign codes. The datasets are PlanetScale accounts and users; WorkOS organizations, users and SSO connections; PostHog events; Stripe customers, subscriptions and invoices; and `support_tickets.csv` and `marketing_campaigns.csv` from S3. The sample is generated on your computer, so nothing is downloaded.

The [onboarding notes](onboarding.md) list the onboarding API routes, SDK methods and import limits.

## Connecting sources

Choose **Data → Add data**, enter credentials, click **Test connection**, select tables or S3 files, and connect. You can select several tables or files in one connection, up to 100. Each selection keeps its own source record and complete snapshot. Imports must finish completely within the limits: two million streamed records, 1,000 REST pages and 256 MB per upload.

### Hosted databases: Neon, Supabase and PlanetScale

Start by choosing where your database is hosted. Provider sign-in requires registering an OAuth integration first. Without one, use a Neon API key, a Supabase personal access token or a PlanetScale service token. Database credentials stay on the server while you choose tables. Direct connections and the other source types remain available. See [hosting integration setup](HOSTING.md) for configuration and provider-specific requirements.

### PostgreSQL and MySQL

**PostgreSQL** accepts a single masked connection URL, including its database username and password; individual fields are also available. Imports use a read-only snapshot transaction and a server cursor.

**MySQL** supports configurable, verified TLS with a custom CA. Large integers and decimals are preserved as strings, and timestamps are read as strings. Transformations can cast them explicitly.

### Neo4j

Neo4j connects over the HTTP Query API of Neo4j 5 or later: the server's HTTP URL on port 7474, a database name, and a username and password. Queries are sent in read mode. From the Docker install, `localhost` is the app's own container, so a Neo4j server running on this computer is reached as `http://host.docker.internal:7474`; this name comes from Docker Desktop. With `pnpm dev`, use `http://localhost:7474`. **Test connection** lists every label and relationship type with its count. Each selected label becomes a dataset of its nodes, keyed by Neo4j's element id in a column named for the label (`customer_id` for `Customer`), and each selected relationship type becomes a dataset of its relationships, with the start and end node ids in columns named for their labels, so the suggested ontology links them. A property with the same name as one of those columns is kept with a `_property` suffix. Integers beyond JavaScript's safe range, and dates, times and durations, are preserved as strings. Neo4j exposes no change stream, so imports are complete snapshots that refresh hourly by default.

### SQLite

A SQLite database is a single file, so instead of a host and password you choose the file (`.db`, `.sqlite`, `.sqlite3` or `.db3`, up to 256 MB) and click **Read file**. Foundry keeps the copy with the workspace's data and lists its tables with their row counts. Each selected table becomes a dataset, and the file's declared primary and foreign keys link them in the suggested ontology, as a database connection's keys do. Views, virtual tables such as full-text indexes, and SQLite's own tables are not offered. Integers beyond JavaScript's safe range are kept as strings and blobs as base64 text; SQLite lets a column hold mixed types, and such a column becomes text.

Copy the file with the application that writes it closed, so the copy includes its latest changes: a database in WAL mode may keep recent writes in a separate `-wal` file, which is not uploaded. Encrypted databases are not supported. The file is opened read-only, and Foundry only reads its tables. An uploaded file does not change, so SQLite sources do not refresh on a schedule; to update them, upload a new copy.

### Stripe

Stripe takes a restricted API key with read access. **Test connection** lists the objects the key can read: customers, products, prices, coupons, promotion codes, subscriptions and their items, invoices and their line items, charges, payment intents, refunds, disputes, payouts and balance transactions. Each selected object becomes its own dataset.

IDs are renamed to `<object>_id` columns (for example `customer_id`), references such as `customer` become the matching `_id` column, and Unix timestamps become ISO timestamps, so relationships line up across objects. Nested fields such as `metadata` stay as JSON. Stripe imports are complete snapshots that refresh hourly by default.

### WorkOS

WorkOS takes an API key for the environment to import. It offers organizations, users, organization memberships, invitations, SSO connections, directories, directory users, directory groups and directory group members. IDs are renamed to `<object>_id` columns; existing `organization_id`, `user_id` and `directory_id` references already match. Lists that WorkOS requires to be filtered are fetched per organization, directory or group.

### PostHog

PostHog takes a personal API key with Query Read and Project Read, plus the US, EU or self-hosted region. It offers persons (with `email` pulled out of properties), person distinct IDs, groups, events, and daily event counts per person.

Events and daily counts cover the last 30 days by default (up to 365). They are read one UTC day at a time with keyset-paged SQL queries. Identified users carry your application's user ID as a distinct ID, so `person_distinct_ids` is how PostHog people join to your users. PostHog refreshes every six hours by default.

Stripe, WorkOS and PostHog appear beside the database hosts in **Add data**.

### REST APIs, files, S3 and the ingestion API

- **REST** supports unpaginated, next-link, cursor and offset responses. Set the items path and the pagination fields to match the API.
- **S3-compatible storage** imports selected files from a bucket.
- **Uploads** accept CSV, JSON, JSONL and Parquet files up to 256 MB. Each upload becomes a durable background job, and you can choose several files together.
- **The ingestion API** (`POST /api/v1/workspaces/:w/ingest`) accepts up to 10,000 rows per request. Ingestion replaces complete snapshots; it is not an append API.

### Sync and change capture

Database imports automatically try PostgreSQL logical replication or MySQL row-binlog capture. Foundry creates its own PostgreSQL publication and slot when permitted, and never modifies server-wide settings. Native change capture coalesces committed changes into complete, bounded-memory snapshot imports, including updates, deletes and truncations. This release does not yet apply row deltas directly, so large or rapidly changing tables may lag during materialisation.

If replication is unavailable, snapshots refresh every five minutes (or on a UTC schedule chosen by an owner in **Data → connection → Details**, or through the API), with a visible reason and automatic retries. REST and S3 sources also use snapshot refreshes; uploads and SQLite files stay explicit. Owners can change snapshot cadence without re-entering credentials, using preset intervals or a custom UTC cron expression that runs at most once a minute; Automatic restores the connector default. The change applies to every table in the connection. Working CDC continues independently, while the saved cadence applies if snapshot fallback is needed. Existing queued/running imports finish normally, and you can still refresh by hand with **Refresh** on the Data page or **Sync now** on the connection's page. Automatic refreshes of the sources that setup's workspace build reads wait until the build finishes, for up to 15 minutes. Schema changes are staged for review. Business record overrides and publication protections remain separate. Writing back to sources, per-row AI enrichment and distributed compute are not supported yet.

[Operations](OPERATIONS.md) lists the database permissions change capture needs and how to manage WAL and binlog retention.

## AI and review

**Providers.** Local inference through Ollama is the default. Claude, OpenAI, Azure OpenAI, Amazon Bedrock and Custom are explicit choices, and there is no fallback from one provider to another. Hosted keys are entered in the browser, encrypted, and tested before the provider is activated. If a replacement provider fails its test, the existing settings stay active.

**Azure OpenAI.** In **Settings → AI & models → Azure OpenAI** (also available during setup), enter the resource's endpoint (`https://<resource>.openai.azure.com`, under **Keys and Endpoint** on the resource), the deployment name (under **Deployments**) and the resource's API key. The app calls the Responses API at `<endpoint>/openai/v1/responses`, adding `/openai/v1` when the endpoint does not end with it, with the deployment name as the model and the key in Azure's `api-key` header. The endpoint must use `https://`, without credentials or a query string, and redirects are refused. A saved key is retained only while the endpoint and the deployment are unchanged; enter the key again when changing either. Azure OpenAI uses the hosted-provider data controls, and hands-free voice stays off because it needs OpenAI's own API. Through the API, send `provider: "azure"` with the endpoint as `baseUrl` and the deployment name as `model`.

**Amazon Bedrock.** In **Settings → AI & models → Amazon Bedrock** (also available during setup), enter the AWS region (such as `us-east-1`), a model ID and an Amazon Bedrock API key (create one under **API keys** in the Bedrock console). Use a model your account can use in that region. Many models, including Claude, need an inference profile ID, such as `us.anthropic.claude-sonnet-4-6`, rather than the base model ID. The app calls the Converse API at `https://bedrock-runtime.<region>.amazonaws.com/model/<model>/converse` with the key as a bearer token, and asks the model to call a tool for structured output, so the model must support tool use (Claude and Amazon Nova do). The key is sent only to AWS's own Bedrock Runtime endpoints, and redirects are refused. It keeps working when you change the region or the model. Amazon Bedrock uses the hosted-provider data controls, and hands-free voice stays off. AWS access keys and SSO profiles are not supported; use an API key. Through the API, send `provider: "bedrock"` with the region's endpoint as `baseUrl` and the model ID as `model`. The API also accepts FIPS endpoints (`bedrock-runtime-fips.<region>.amazonaws.com`), VPC endpoints (`vpce-….bedrock-runtime.<region>.vpce.amazonaws.com`) and AWS China endpoints (`….amazonaws.com.cn`).

**Custom models.** In **Settings → AI & models → Custom** (also available during setup), enter an HTTP(S) API base URL including its version, a model ID or routing alias, and choose **OpenAI Chat Completions** or **OpenAI Responses**. The endpoint must support structured JSON output with `json_schema`. Enter an API key when the endpoint requires bearer authentication; it is optional for unauthenticated self-hosted models. Credentials belong in the key field, not the URL. A saved key is retained only when the Custom endpoint is unchanged; enter the key again when changing endpoints. Custom providers use the hosted-provider data controls and do not enable OpenAI voice sessions or Ollama model downloads.

Click **Test and save AI** to save a Custom provider. The connection is tested in the background and becomes active only after it succeeds. Some common setups:

- **Azure OpenAI:** base URL `https://<resource>.openai.azure.com/openai/v1`, the **OpenAI Responses** format, the resource's key as the API key, and the deployment name as the model. The dedicated **Azure OpenAI** provider above is simpler: it takes the endpoint as the resource shows it and needs no format choice.
- **An OpenAI-compatible server on this computer**, such as vLLM or LM Studio: from the Docker install, `localhost` is the app's own container, so use `http://host.docker.internal:<port>/v1` (a name Docker Desktop provides). With `pnpm dev`, use `http://127.0.0.1:<port>/v1`.
- **Reasoning models**, such as OpenAI's o-series and gpt-5, need the **OpenAI Responses** format. The Chat Completions format sends `max_tokens`, which they reject.

**Context and privacy.** AI sees bounded schema, profile and sample context. Samples sent to a hosted provider mask detected sensitive columns unless `allowRawData` is set to `true` in the provider settings (an API-only setting, off by default). Detection is a heuristic, so review your source policies before using a hosted provider. Owners can open a dataset’s **Details → Hosted AI privacy**, select additional columns and **Save privacy**. Detected sensitive columns cannot be disabled here. The choices persist across snapshots. Marked columns are masked in sample rows sent to hosted AI unless raw data is allowed, and Home leaves them out of its key metrics with any AI. Previews, queries and exports still show them to people who can open the dataset, and hands-free voice still speaks answers, which can include their values.

**Limits.** Local worker calls run one at a time, with two repair attempts per failing asset group. Budgets are set through the API, with `PUT /api/v1/workspaces/:w/provider` (owner only). Send `provider` and `model` along with the budgets: fields you leave out return to their defaults, a saved API key is kept when the provider does not change (and, for Custom, its endpoint; for Azure OpenAI, its endpoint and deployment; for Amazon Bedrock, across regions and models), and the settings are tested before they are saved.

- `maxCalls`: model calls per run, 1 to 20, default 20. Each retry of a run gets a fresh call budget.
- `timeoutSeconds`: the time budget for each run and the timeout for each model call, 30 to 1,800 seconds, default 600.
- `dailyTokens`: tokens per workspace per UTC day, 1,000 to 10,000,000, default 200,000.

**Hands-free voice.** With the OpenAI provider, and a project that has access to GPT-Live (`gpt-live-1`), a small blue orb appears after the Ask box on Home and above the chat composer. Pressing it asks for the microphone and starts a voice conversation; the orb glows for as long as the session is connected. GPT-Live holds the conversation and hands business questions to Foundry, which answers each with an ordinary read-only Analyst run in the same chat, so tables, charts and sources appear as usual. GPT-Live then speaks the verified result; it is told never to state workspace figures the backend did not return. Follow-ups continue without pressing Send, and starting voice inside a chat keeps that chat's context. Typing pauses the microphone until the text is sent or cleared. Press the orb or Escape, say that you want to stop, or leave the chat to end voice. Voice never builds or changes anything. A one-row answer is said aloud with its numbers; a table gets a one-sentence headline and stays in the chat. Voice is available only with the OpenAI provider, and it sends each answer to OpenAI so it can be spoken; setup's OpenAI card says this. The OpenAI key stays on the server, which creates each session and controls it over a server-side WebSocket. Voice is billed by OpenAI per second of session, plus the model calls that interpret each spoken request.

**Validation.** Model-generated definitions are schema-checked and executed against the data. A passing syntax and quality check does not prove business meaning, so proposals show their assumptions and unmatched relationships for review. Guided setup publishes only what passes validation, built from the ontology you confirmed. Model quality and latency depend on your hardware and the selected model. The default Qwen digest is recorded in [`models.json`](models.json), and installation verifies it before activation.

**Definition changes.** Each unpublished proposal shows the added, changed and removed catalog entries, pipelines, object types, relationships, dashboards and actions compared with the currently published definitions. Expand an entry to read the exact current and proposed definitions, including changed SQL. Summary and assumption changes are shown separately. Before the first publication, every proposed asset is an addition. A warning identifies proposals whose input snapshots or published generation changed since validation; retest before publishing. This comparison describes definitions, not affected record counts or semantic SQL equivalence, and opening it changes no data.

**Publishing and rollback.** Publishing checks the exact proposal hash and input versions, then switches generations. Each object type and relationship is one executor query, whose rows the worker reads back and stages in batches; a type whose record count differs from its snapshot stops the publication, and the previous generation stays active. Revalidating creates a new reviewable proposal. Rollback revalidates and rematerialises the previous definitions against current snapshots, while preserving manual actions and their audit history. Failed staging artifacts can be removed during offline maintenance, after checking that no publication references them.

## Architecture

| Service  | Responsibility                                                                |
| -------- | ----------------------------------------------------------------------------- |
| App      | React UI, loopback-only API, API token checks, publication and actions        |
| Worker   | Durable AI runs, scheduled imports, pipeline execution, proactive proposals   |
| Executor | Disposable DuckDB child processes, declared inputs, isolated SELECT execution |
| HelixDB  | Workspace metadata, API tokens, jobs, proposals, ontology records and edges   |
| Ollama   | Local structured inference with persistent model storage                      |

In Docker Compose, the worker runs as its own service. In development (`pnpm dev`), it runs inside the API process. Only the app publishes a port, on `127.0.0.1`. The executor sits alone on an internal network with no outside access.

Data lives in three Docker volumes:

| Volume                       | Contents                                                                                                                                                |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `helix-foundry_helix-data`   | HelixDB metadata: workspaces, ontology, jobs, API tokens and encrypted credentials                                                                      |
| `helix-foundry_foundry-data` | Parquet snapshots, artifacts and staged uploads; also the executor's spill files (`scratch`) and publication files (`results`), which backups leave out |
| `helix-foundry_model-data`   | Ollama model weights (not backed up; they can be downloaded again)                                                                                      |

**HelixDB.** Queries use the HelixDB TypeScript SDK 3.2 and `/v2/query`. A scope revision is compared and advanced inside each write batch, so stale metadata writes fail as a unit. Dataset artifacts and staged ontology generations stay invisible until the publication pointer changes. Source imports remain separate from manual object overlays.

**Executor isolation.** The executor loads approved input snapshots, disables external access and extension installation, locks its configuration, and wraps generated SQL as a single SELECT. It has no connector or API credentials. Two DuckDB jobs run at a time, each with a 512 MB memory setting and spill files on the data volume beyond it, which only DuckDB's own bound (90% of the free disk space) limits unless `DUCKDB_SPILL_LIMIT` is set. By default, a publication query may write up to 2 GB of result rows, and queries have two minutes and imports, pipelines and publication queries 15 minutes, including time waiting for a turn; `.env` can change each limit (see [operations](OPERATIONS.md#deployment-limits)). In Compose the executor container is limited to 2 GB of memory and 128 processes, drops all Linux capabilities and cannot gain new privileges. Never expose its endpoint.

## Security

Helix Foundry is local-first: one person per computer, with no accounts and no sign-in. Because of that, the API only listens on this computer.

- `HOST` must be a loopback address unless `ALLOW_NETWORK_ACCESS=true`. Compose sets that flag because it publishes the port on `127.0.0.1` only.
- Do not expose the app to a network or put it behind a reverse proxy, tunnel or port forward. HelixDB, Ollama and the executor must remain private.
- Requests whose `Host` is not a loopback name or the configured origin's host are rejected with 403, which blocks DNS-rebinding attacks. Browser changes (anything other than GET, HEAD and OPTIONS) must come from the configured origin, or its loopback equivalent on the same port.
- Requests are rate limited to 240 per minute per IP by default (`RATE_LIMIT_MAX`).
- A request without an `Authorization` header acts as the local owner. Scripts and the SDK send `Authorization: Bearer TOKEN` with a token from **Settings → Developer tools**. Tokens are workspace-scoped, read-only by default and revocable. An invalid or revoked token is rejected with 401 rather than treated as the local owner.
- Connector and provider credentials are encrypted with AES-256-GCM using a key derived from `ENCRYPTION_KEY`. Losing the key makes them unrecoverable. Do not rotate it by replacing the value.

## Local development

Use Node.js 24, pnpm 10.7 and Docker. Native Ollama is optional, and useful on Apple Silicon because Docker cannot use Metal.

```sh
pnpm install
./scripts/setup.sh
# HelixDB on 6996. Use a port not occupied by another Helix project.
docker run -d --name helix-foundry-dev -p 127.0.0.1:6996:8080 \
  -e HELIX_DATA_DIR=/var/lib/helix -v helix-foundry-dev:/var/lib/helix \
  ghcr.io/helixdb/helixdb:v0.0.12@sha256:f22b50fdab8fadbf8b5a881e656f293fdf5c688bad018f503b8dbc2f0f29543d
ollama serve &   # or run it in its own terminal; skip if Ollama is already running
# Optional: the app also pulls the model when you choose Local.
ollama pull qwen3:4b
pnpm dev
```

Open **http://localhost:5173**. The Vite dev server proxies `/api` to the API on 3001, and the executor listens on 3002. Use 5173 rather than 3001 during development: the API may also serve an old build on 3001, and saves from it fail the origin check.

- The development HelixDB uses the image pinned in `compose.yaml`; `pnpm test` fails if the copy above, or those in the README and AGENTS.md, differ from it. A newer image upgrades the `helix-foundry-dev` volume one way (v0.0.10 and later upgrade the index storage, and older images then refuse it), so copy the volume before you change the tag.
- `./scripts/setup.sh` without flags writes `.env` with development URLs (`APP_ORIGIN=http://localhost:5173`, HelixDB on 6996, executor on 3002, Ollama on 11434) and two generated keys. It never overwrites an existing `.env`.
- `pnpm doctor` checks HelixDB, the executor, the model endpoint and the local keys, using the development URLs in `.env`. It is for development only; for a Docker install, use the in-container check in the [agent runbook](../AGENTS.md#6-verify).
- Startup applies the metadata schema idempotently; `pnpm migrate` applies it explicitly.
- `pnpm build:sdk` builds JavaScript and TypeScript declarations for the SDK and shared contracts. Run `pnpm -C packages/shared pack` and `pnpm -C packages/sdk pack` to package the client. See the [API reference](API.md) for SDK usage.
- To move the development ports, set `PORT`, `EXECUTOR_PORT`, `EXECUTOR_URL`, `APP_ORIGIN` and `HELIX_URL` in `.env`, and pass `VITE_PORT` and `API_PROXY` in the shell, because Vite does not read `.env`. Keep `APP_ORIGIN` equal to the URL Vite prints.

## Testing

```sh
pnpm typecheck
pnpm test
TEST_HELIX=1 pnpm test       # requires the configured HelixDB instance
TEST_MYSQL=1 pnpm test       # fixture MySQL on localhost:33077; see tests/mysql.test.ts
TEST_NEO4J=1 pnpm test       # Neo4j 5 on localhost:37474, user neo4j, password foundry-test-only; see tests/neo4j.test.ts
pnpm build
pnpm test:browser            # starts an isolated API, executor and browser installation
ONBOARDING_LOCAL=1 pnpm exec vitest run tests/onboarding.local.test.ts   # real local Qwen check
BENCH_HTTP=1 pnpm benchmark  # 1M rows, 25 concurrent executor clients
```

- CI runs `pnpm install --frozen-lockfile`, `pnpm typecheck`, `pnpm test` and `pnpm build` on Node 24 for every push and pull request.
- `pnpm test` uses in-memory stores and temporary loopback ports, so it needs no running services.
- `TEST_HELIX=1` writes and then deletes a temporary scope in whatever HelixDB `HELIX_URL` points to.
- `TEST_CDC_DATABASES=1` and `TEST_ONBOARDING_DATABASES=1` enable database integration suites; see `tests/source-sync.test.ts` and `tests/onboarding-connectors.test.ts`.
- `pnpm test:browser` uses ports 3101, 3102 and 5174 with an in-memory metadata store, `.data/browser-isolated` and a deterministic provider fixture. Set `PW_SLOT` (1 to 9) to shift the ports by 10 for parallel runs.
- The benchmark needs the executor running. See [validation notes](VALIDATION.md) for recorded results.

## Backups and upgrades

```sh
./scripts/upgrade.sh                                                  # backs up first, then pulls and rebuilds
./scripts/backup.sh ~/helix-foundry-backups/$(date +%Y%m%d-%H%M%S)   # stops writers, snapshots volumes, restarts
./scripts/restore.sh ~/helix-foundry-backups/TIMESTAMP --replace
```

Backups leave out `ENCRYPTION_KEY` and `INTERNAL_KEY`. They record a fingerprint of the key, the app commit and the HelixDB image, so keep a separate copy of `.env`, for example in a password manager. `backup.sh --include-key` adds `.env` encrypted with a passphrase. Backups made by an older `backup.sh` hold both keys in plain text in `config.env`, often in the repository's `backups/` folder; `backup.sh` and `upgrade.sh` warn about them, so delete them once you have a new backup and a separate copy of `.env`. Keep backups private and outside the repository; `backup.sh` refuses a folder inside it. Restore replaces the selected installation's data. It keeps `.env` when the key matches, takes your copy with `--env-file`, and refuses a backup made with another key or in a newer HelixDB storage format. It checks the archives before it stops anything, and replaces `.env` only once the data is restored, keeping the old one as `.env.pre-restore-TIME`; delete that file once the restore is confirmed.

Upgrade with `upgrade.sh`, which refuses to continue unless a fresh backup succeeds. If the rebuild fails, run it again: it finishes the upgrade. To go back, run the two commands it printed: `git reset --keep` with the old commit, then the copy of `restore.sh` saved in the backup (`bash BACKUP/restore.sh BACKUP --replace`), which also reads the backup in a checkout of a release from before `upgrade.sh`. HelixDB upgrades its storage one way (v0.0.10 and later upgrade the index storage, and v0.0.9 and earlier then refuse the database), so when a release changes the HelixDB image, `upgrade.sh` warns and asks first. Keep the backup and the old commit, and run smoke checks (diagnostics, sample ingestion, a saved query and proposal validation) before using the installation again. Never remove volumes during an ordinary upgrade.

See [operations](OPERATIONS.md) for the full procedure and deployment limits, and [validation notes](VALIDATION.md) for tested coverage and current limits.

From a dataset's details, **Export CSV** downloads every row of the current
snapshot for spreadsheets or other tools. **Export Parquet** preserves the
native column types. CSV includes headers even for an empty dataset; nested
values are represented as JSON, with nulls as empty fields.
