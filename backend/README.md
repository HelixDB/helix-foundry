# Helix Foundry backend

The Go module `github.com/helixdb/helix-foundry/backend`: Foundry's API and the workers behind it. They connect and sync data sources, have the ontology agent draft and revise ontologies, load versions into HelixDB, read documents, answer questions over a load and email people when their work is done. State is in MySQL (`store`, with its migrations embedded from `store/migrations`); synced tables and documents are in S3-compatible storage.

## Build and test

Go as in `go.mod`, and Docker for the test databases.

```sh
go build ./...
go vet ./...
docker compose -f compose.test.yaml up -d --wait   # MySQL on 3307 and 3309, PostgreSQL on 5433
HELIX_TEST_ROOT_DSN='root:helix@tcp(127.0.0.1:3307)/' go test ./...
docker compose -f compose.test.yaml down -v
```

`docker build .` builds the standalone image.

The generated code in `pb/gen` is committed. After changing a `.proto` under `pb/proto`, regenerate it with [buf](https://buf.build):

```sh
buf generate && buf generate --template buf.internal.gen.yaml
```

## Run it

[`../local`](../local) runs Foundry on one machine: the site, this backend, the ontology agent, MySQL, SeaweedFS and HelixDB. It runs Foundry's release images; with `FOUNDRY_SITE_IMAGE=helix-foundry/site:local` and `FOUNDRY_IMAGE=helix-foundry/foundry:local` in its `.env`, `docker compose build` there builds the site and the backend from this checkout instead. The image's `foundry init-secrets --dir DIR` writes an install's secrets once (the compose file's first service), and `local.secrets_dir` (`HELIX_LOCAL__SECRETS_DIR`) reads them; with no `foundry.ai.default.provider`, `foundry` finds its AI default on the machine (package `ai/detect`).

## Hosted Helix

Hosted Helix (foundry.helix-db.com) runs this same code as a library inside its own server. It runs the module's migrations in one sequence with its own, and supplies what differs when hosted, such as sign-in and roles, workspaces, Helix databases, plans and the provider accounts connectors sign in with, through the module's interfaces. So REST paths and JSON shapes, tables and migrations, config keys, environment variable names and metric names are shared with the hosted deployment: change them on purpose, never as a side effect.

Where the host supplies `ports.ProviderAccounts` (hosted: WorkOS Pipes), GitHub and Linear connectors can be connected by signing in at the provider instead of entering a key: the account stays with the host's broker, and the connector asks it for a token each time it reads. The sync worker checks each account weekly and deletes, through the broker, the accounts deleted connectors and failed sign-ins let go. A standalone install has no broker, and every connector takes a key.

Foundry's emails go out through the host's mail (`ports.Mail`) from the `foundry-notify-worker` (package `workers/foundry-notify-worker`), which hosted runs as a task beside the sync worker, with `foundry.notify.site_url` set to the site's origin. It tells the person who asked for an ontology's first draft to be loaded when ready how that ended, and follows up once, a day later, with someone who watched the sample and brought no data of their own. A standalone install sends no email and doesn't run it.

Self-healing audits run in the `foundry-heal-worker` (package `workers/foundry-heal-worker`), which hosted runs as a task beside the graph worker, on its ontology agent sidecar. With `foundry.heal.enabled` it audits each ontology's current load with the agent's Audit as the load becomes current and nightly, never while a graph job is live on the ontology, and stores the findings (`foundry_heal_findings`, `foundry_heal_runs`) the Suggestions page lists. Off, the default, it does nothing, so a host may list the task everywhere. A standalone install runs it only with the flag on. Known limits: Foundry stores no source fields, so no audit checks source drift; and a run makes one Audit call per 1,000 reviewed documents, about 17 minutes for 1M at 1 s a page and hours at a 1 request a second plan rate limit, stopping incomplete after 4 hours; and a run stops once a graph job starts on its ontology, so an ontology whose jobs come more often than an audit takes (a connector syncing changes every few minutes) may never complete. Audits that resume across runs would lift both.

## Agent integrations

`integration/` owns the Foundry MCP tool handlers and the ordinary HTTP Ask routes. It imports public Foundry contracts only. Hosts supply identity/authorization callbacks, database provisioning, durable confirmations and audit logging. Hosted Helix wires it into `mcpinsights`; the standalone binary registers it on its existing REST listener. Both use the existing API, question queue and workers.

HTTP clients POST `/v1/ontologies/{ontologyId}/ask` with a bearer, `Idempotency-Key` and `{"question":"..."}`, then GET the response's `Location` while it returns 202. Each call waits at most 25 seconds for a terminal question. A 200 includes `status` and the compact MCP result; it may be `cannot_answer`, `failed` or `cancelled`. Client disconnects do not cancel durable questions. See [the OpenAPI contract](../contract/foundry-integration.openapi.json), [the Python tool](../examples/ask_tool.py) and [local setup](../local/README.md#connect-your-agent).

The standalone install's confirmation table uses the directory's separate migration sequence. Hosted continues using its existing confirmation RPCs. Local setup supports connectors and documents, with API sources still disabled. `local/compose.mcp.yaml` publishes the install's listener on the host's loopback when asked. MCP and HTTP Ask on it answer only for loopback, `host.docker.internal` and the host of `local.site_origin`; the site's routes are limited by the token.

Release the shared module first, then pin that commit in hosted Helix's Go/Bazel dependencies. For development across both checkouts, an untracked Go workspace can include each backend module; this does not replace the published module pin for CI. Hosted HTTP exposure uses `foundryAskHTTP.enabled` on the existing MCP ingress, enabled in development and disabled by default. No new DNS, TLS certificate, worker or ontology-agent release is needed. Publish immutable backend/site images and update the local Compose pins after testing. Roll back image pins or disable the HTTP ingress flag; retain existing volumes and the additive local confirmation table.
