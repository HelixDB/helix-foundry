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

Hosted Helix (foundry.helix-db.com) runs this same code as a library inside its own server. It runs the module's migrations in one sequence with its own, and supplies what differs when hosted, such as sign-in and roles, workspaces, Helix databases and plans, through the module's interfaces. So REST paths and JSON shapes, tables and migrations, config keys, environment variable names and metric names are shared with the hosted deployment: change them on purpose, never as a side effect.

Foundry's emails go out through the host's mail (`ports.Mail`) from the `foundry-notify-worker` (package `workers/foundry-notify-worker`), which hosted runs as a task beside the sync worker, with `foundry.notify.site_url` set to the site's origin. It tells the person who asked for an ontology's first draft to be loaded when ready how that ended, and follows up once, a day later, with someone who watched the sample and brought no data of their own. A standalone install sends no email and doesn't run it.
