# Helix Foundry on your machine

One owner, no sign-in, no team and no billing: every connector and feature is on. Needs Docker with Compose v2. Nothing to set up first:

```sh
docker compose up -d --wait  # from the repository's root, or from local/
open http://localhost:3000   # that name: the site answers no other host
```

It opens in your first workspace, **My workspace**, made as the install first starts.

**Secrets.** The first start writes this install's secrets (the site's token, the store's secret, MySQL's password and the connector key) into the Compose volume `secrets`, and every start reads them from there; nothing is printed or written to the checkout. Keep that volume: the connector key seals every database password and API key you give Foundry, is never made again once written, and losing it makes them unreadable. `docker compose down -v` deletes it with the data. Back it up with the data: [Back up and restore](#back-up-and-restore).

**No analytics.** A local build loads no tracker and sends no usage data anywhere; only the hosted site, foundry.helix-db.com, records product analytics.

**An install set up with `setup.sh`** (before 0.1.1) keeps its `local/.env`: its first start of this version copies the `FOUNDRY_*` secrets there into the volume, so MySQL's password and the key that sealed its credentials still match. Keep running it from `local/` if its `.env` sets `COMPOSE_PROFILES` or `COMPOSE_FILE` (Ollama in Docker, the GPU file): Compose reads those from the project's own `.env` only.

**Overrides** are optional, in `local/.env` (copy [`.env.example`](.env.example)): Compose reads it for this file whether it runs from the root or from here. Each is below.

## What runs

| Service          | What it is                                                                                      |
| ---------------- | ----------------------------------------------------------------------------------------------- |
| `site`           | The Foundry app, on 127.0.0.1:3000. The browser interface.                                      |
| `foundry`        | Foundry's API and MCP, its directory and workers, in one process.                               |
| `ontology-agent` | Drafts ontologies, reads documents and answers questions, on the AI provider you choose.        |
| `storage`        | S3-compatible store (SeaweedFS) for synced tables and documents, on 127.0.0.1:8333 for uploads. |
| `mysql`          | Foundry's database.                                                                             |
| `helix`          | HelixDB (the public helix-db image): one database every ontology's loads share.                 |
| `acme-db`        | PostgreSQL holding Acme Corp, the made-up sample company Use the sample connects to; no volume. |
| `ollama`         | Ollama, and `ollama-pull`, which downloads its model: with the `ollama` profile only.           |
| `init-secrets`   | Writes the install's secrets into the `secrets` volume once, then exits, before the rest start. |

Data lives in the Compose volumes `mysql`, `storage`, `helix` and `work`, its secrets in `secrets` (and Ollama's models in `ollama`). `docker compose down` keeps them; `docker compose down -v` deletes everything, the connector key with it. [Back up and restore](#back-up-and-restore) copies them.

## AI

Drafting ontologies, reading documents and answering questions run on an AI provider. With none named, Foundry picks one as it starts, and looks again whenever **Settings › AI models** opens, so a model pulled or a server started later is used without a restart. It takes the first of:

1. **Claude**, with the `ANTHROPIC_API_KEY` of the shell that ran `docker compose up` (Compose passes it to Foundry; it is never stored or logged).
2. **OpenAI**, with that shell's `OPENAI_API_KEY`.
3. **Ollama**: the bundled one (below), else Ollama on this computer, with a model that calls tools and loads drafting's 65,536-token window; the most capable such model runs every task. Pull one first (`ollama pull qwen3.5:4b`).
4. **LM Studio** on this computer serving a model, at `http://host.docker.internal:1234/v1`: the model it has loaded, on every task.

Otherwise none yet: data, connectors and Explore work, and drafting, uploading documents and asking wait for a provider, with a link to Settings › AI models. That page says which default was found and why ("Using qwen3.5:4b from Ollama on this computer"), and shows the power of the rest: Ollama (bundled, on this computer or another server, choosing among the models the server has, with their sizes, or one not pulled yet), Claude, OpenAI, Azure OpenAI or Amazon Bedrock with a key, or an OpenAI-compatible server, each tested before Foundry uses it, or back to the install's default. A key entered there is stored sealed with the connector key, never in `.env`.

To name the install's default instead, set `FOUNDRY_AI_PROVIDER` (in the shell or `local/.env`) and the fields its kind takes, then run `docker compose up -d` again; `none` is no provider and no search. An empty `FOUNDRY_AI_PROVIDER` passes none of the other `FOUNDRY_AI_*`. Switching provider, set each task's model (`FOUNDRY_AI_DRAFT_MODEL`, `FOUNDRY_AI_ASK_MODEL`, `FOUNDRY_AI_EXTRACT_MODEL` and `FOUNDRY_AI_VERIFY_MODEL`, each of which wins over `FOUNDRY_AI_MODEL`) to the new provider's, or delete them.

- **Claude or OpenAI:** `anthropic` or `openai`, `FOUNDRY_AI_API_KEY`, and each task's model (Claude's are `claude-opus-5-5` to draft, `claude-sonnet-5-5` to ask and verify, `claude-haiku-5-5` to extract).
- **Ollama:** `ollama`, `FOUNDRY_AI_BASE_URL` (`http://127.0.0.1:11434` for the bundled one, `http://host.docker.internal:11434` for this computer's) and `FOUNDRY_AI_MODEL`; `FOUNDRY_AI_CONTEXT_TOKENS` is the window it loads models with, 65,536 when empty, up to 1,048,576.
- **An OpenAI-compatible server** (LM Studio, vLLM, Ollama's `/v1` or a gateway such as LiteLLM): `openai_compatible`, `FOUNDRY_AI_BASE_URL` including `/v1`, `FOUNDRY_AI_API` (`chat_completions`, which most servers speak and the default, or `responses`), `FOUNDRY_AI_MODEL` as the server names it, and `FOUNDRY_AI_API_KEY` if it takes one. Every task runs on that model, with no cost counted. The model must call tools and answer in structured JSON (vLLM needs `--enable-auto-tool-choice` and its model's `--tool-call-parser`); small local models may stop drafts early.
- **Azure OpenAI:** `azure_openai`, `FOUNDRY_AI_ENDPOINT` (`https://<resource>.openai.azure.com`), `FOUNDRY_AI_API_KEY` and each task's deployment as its model.
- **Amazon Bedrock:** `bedrock`, `FOUNDRY_AI_REGION`, a long-term Bedrock API key as `FOUNDRY_AI_API_KEY`, and a model or inference-profile ID that supports tool use per task. Claude through Bedrock's global inference profiles (`global.anthropic.claude-sonnet-5-5`) may run requests in any AWS commercial region; a geography's (`eu.anthropic.claude-sonnet-5-5`) stays within it, and GovCloud has none.

Foundry runs in Docker, so a server on this computer is `host.docker.internal`, not `localhost`, and must answer on that address: turn on LM Studio's "Serve on Local Network". **On Linux, start Ollama with `OLLAMA_HOST=172.17.0.1`**, Docker's bridge address, which `host.docker.internal` is (`ip -4 addr show docker0` shows it if yours differs), so Docker can reach it and your network can't. That address exists once Docker runs, so start Ollama after it, and give the `ollama` command the same `OLLAMA_HOST`. `OLLAMA_HOST=0.0.0.0` works too, but opens Ollama, which takes no key, to every machine that can reach this one: use it only behind a firewall.

### Ollama in Docker

`docker compose --profile ollama up -d --wait` runs Ollama next to Foundry: nothing it is sent leaves this machine. The first start downloads its model in the background (`docker compose logs -f ollama-pull` shows it); once it is there, Foundry finds it (open Settings › AI models, or start again). The model is `qwen3.5:4b` for now, a candidate until a real draft on a GPU picks the smallest model that finishes one. It takes 5.7 GB of memory with drafting's 65,536-token window, so give Docker at least 10 GB; `qwen3.5:9b` takes 8.8 GB. Small models pass Foundry's test but may stop a draft early.

- **GPU.** On a machine with an NVIDIA GPU and the NVIDIA Container Toolkit, `compose.gpu.yaml` gives Ollama the GPUs: from local/, `docker compose -f compose.yaml -f compose.gpu.yaml --profile ollama up -d`. On a Mac, Docker has no GPU: Ollama on this computer uses Apple's.
- **The model.** `FOUNDRY_OLLAMA_MODEL`, pinned by `FOUNDRY_OLLAMA_DIGEST` when set: `ollama-pull` fetches it only when the volume lacks it, and stops with an error when it isn't that digest. It never changes by itself. To update it, run `docker compose exec ollama ollama pull <model>`: with a digest set, the next start's `ollama-pull` stops, naming the new digest to set (or empty it). To use another model, set `FOUNDRY_OLLAMA_MODEL` and run `docker compose --profile ollama up -d`, then choose it in Settings › AI models.
- **Memory.** Ollama loads one model at a time, runs one call at a time, and keeps the model loaded for 10 minutes after the last call. When memory runs out, Ollama is the first process killed, so a model call fails and HelixDB and MySQL keep running. A larger context window takes more memory: Ollama loads models with `FOUNDRY_AI_CONTEXT_TOKENS`, drafting's 65,536 when empty.
- **Only Foundry reaches it.** Ollama's API takes no key, so it listens on the agent's loopback alone: no other container, and nothing outside Docker, can reach it.

## Questions

Each read of the graph a question makes may run 30 s and read 100,000 matches into memory, and each of its Helix requests ends at 30 s; the whole question has 299 s, the most the ontology agent allows. That answers reads over a month of one type's rows (50,000 orders of a 300,000-order load took 6 to 8 s a read), where the hosted caps, the agent's own 5 s and 20,000, stop them. Higher caps don't answer everything: a read's time grows with every row of its type, not only the month's, so a larger load reaches them again, and no read counts the entities linked more than once (how many customers ordered twice, say), whatever the caps. A `helix_query` read, which the agent writes in Helix's own query language, runs inside Helix and only the Helix timeout and the question's time bound it.

A read stopped at a cap fails its step, which says which cap and its value, and the agent then reads narrower or answers that it can't:

| The step says                                                                                                | Its setting                                                          |
| ------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------- |
| `too slow: the read ran past limits.read.wall_ms, 30000 ms`                                                  | `foundry.ask.read_wall_ms`, in milliseconds, at most `max_wall_secs` |
| `too broad: the query would read … matches into memory, more than limits.read.scan_entities, 100000, allows` | `foundry.ask.read_scan_entities`                                     |
| `the store did not answer within 30s: each graph request ends at helix.timeout_secs`                         | `foundry.ask.helix_timeout_secs`                                     |

To change one, add its variable to foundry's `environment` in `local/compose.yaml` (`HELIX_FOUNDRY__ASK__READ_WALL_MS`, `HELIX_FOUNDRY__ASK__READ_SCAN_ENTITIES`, `HELIX_FOUNDRY__ASK__HELIX_TIMEOUT_SECS`) and run `docker compose up -d` again. A read's wall time past the Helix timeout helps only a read of several requests, so raise both together. The question's lease, `foundry.ask.lease` (3 minutes), must stay at least twice the Helix timeout, or foundry won't start: past 90 s, raise it too (`HELIX_FOUNDRY__ASK__LEASE`, such as `5m`). More matches take more memory, and the answer to one Helix request is at most 32 MiB. These caps need `foundry` 0.1.2 and `ontology-agent` 0.1.1 or newer: older images read every question with the agent's own caps.

## Images

`docker compose up` pulls Foundry's release images: `foundry-site` 0.1.3, `foundry` 0.1.2 and `ontology-agent` 0.1.1. To run the site and the backend built from this checkout instead, set `FOUNDRY_SITE_IMAGE=helix-foundry/site:local` and `FOUNDRY_IMAGE=helix-foundry/foundry:local` in `local/.env` and run `docker compose build` first.

## Back up and restore

**What a backup is.** The volumes `mysql`, `storage`, `helix`, `work` and `secrets`, all five copied together with the stack stopped, and the images that wrote them. The store, the files and the graph refer to each other: volumes copied at different times, or while Foundry runs, may not match. `acme-db` keeps nothing, and Ollama's models (`ollama`) download again.

**Secrets.** `secrets` holds the site's token, the store's secret, MySQL's password and the connector key. Without it the restored data doesn't open: MySQL keeps the password it was first started with, and the connector key seals every database password and API key in the store. Whoever has it and the rest can read every one of them, so keep `secrets.tar.gz` encrypted, or apart from the other archives, and your `local/.env`, if you made one, with it.

**Back up** from where you run Foundry: the repository's root, or `local/` for a `setup.sh` install that must run from there (above). `docker compose down` stops everything first, giving a load or question in flight up to two minutes to finish or be handed back, and the archives go into a folder only you can read. The block stops at the first step that fails, and the stack stays stopped until it runs to the end:

```sh
backup=~/foundry-backup-$(date +%F-%H%M)
(
  set -eu -o pipefail
  mkdir -m 700 "$backup"
  docker compose config --images | sort -u > "$backup/images.txt"
  docker compose down
  for v in mysql storage helix work secrets; do
    docker volume inspect "helix-foundry_$v" > /dev/null
    docker run --rm -v "helix-foundry_$v:/volume:ro" alpine:3.22 tar -czf - -C /volume . > "$backup/$v.tar.gz"
    gzip -t "$backup/$v.tar.gz"
  done
  docker compose up -d --wait
)
```

**Restore** onto the images the backup lists: the same release of this repository, with the same `local/.env`, run from the same place. If you keep `secrets.tar.gz` apart or encrypted, put it back in the backup's folder, decrypted, first. Then check the backup. This changes nothing, and prints `backup ok` only when the images match and every archive is whole:

```sh
backup=~/foundry-backup-2026-11-02-1413
(
  set -eu -o pipefail
  docker compose config --images | sort -u | diff - "$backup/images.txt"
  for v in mysql storage helix work secrets; do gzip -t "$backup/$v.tar.gz"; done
  echo backup ok
)
```

Every volume goes back under its own name before the stack first starts, so on a machine with an install, `docker compose down -v` deletes that first, once the check passes. Then restore, in the same shell (it reads `$backup`); it stops at the first step that fails. A start before the restore writes new secrets and an empty store: `docker compose down -v`, and restore again.

```sh
(
  set -eu -o pipefail
  docker compose create
  for v in mysql storage helix work secrets; do
    docker run --rm -i -v "helix-foundry_$v:/volume" alpine:3.22 tar -xzf - -C /volume < "$backup/$v.tar.gz"
  done
  docker compose up -d --wait
)
```

**Check it**, after a restore and in a rehearsal on another machine now and then:

- `docker compose logs init-secrets` says `kept` for each of the four secrets, and `wrote` none.
- The site opens on your workspaces, with their connections, documents and ontologies. Sync a connection: it syncs only if the restored connector key opens its credential.
- A document opens, and the Explorer shows what each ontology loaded.

**Upgrades.** Back up before you upgrade (a new release, or another `FOUNDRY_*IMAGE`): its first start migrates the store, and migrations only go forward, so the release before must not run on it again. Restore a backup only onto the images it lists, then upgrade again if you need to.

## Connecting databases

Foundry connects from inside Docker: a database on this machine is `host.docker.internal`. Private addresses are allowed.

## Ports

The site must stay on `http://localhost:3000` and the store on `http://localhost:8333`: the site refuses any other host, and documents upload to the address Foundry signs. To move them, edit `local/compose.yaml`:

- **The site:** the host side of its port, `FOUNDRY_ORIGIN`, `HELIX_LOCAL__SITE_ORIGIN` under foundry's `environment` (the links agents are given start there), and the store's `-s3.allowedOrigins`, which lets the site's page upload.
- **The store:** the `pod`'s port, the store's `-s3.port` and its health check's `nc -z 127.0.0.1 <port>`, and `HELIX_LOCAL__S3__ENDPOINT: http://localhost:<port>` under foundry's `environment` (its config is in the image; `HELIX_*` variables override it).

## Connect your agent

These need `foundry` 0.1.2 and `foundry-site` 0.1.3 or newer, the images `docker compose up` pulls: older ones have no MCP and no **Set up with your agent**. `docker compose exec -T foundry curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/mcp` prints 401 where the MCP is served, and 404 where it isn't: don't publish the port for an image that prints 404, which serves its probes and metrics there without a token.

The MCP and HTTP Ask aren't published until you ask, so a port taken here never stops the app. From `local/`:

```sh
docker compose -f compose.yaml -f compose.mcp.yaml up -d --wait
```

This restarts the containers and keeps the data. To keep it on, set `COMPOSE_FILE=compose.yaml:compose.mcp.yaml` in `local/.env` and run Compose from `local/`. If `127.0.0.1:8081` is taken, set `FOUNDRY_MCP_PORT` in `local/.env` to a free port, and use that port wherever an agent's setup or this page says 8081: the site's prompt names 8081, fixed when its image is built, and says this too.

The local MCP is `http://localhost:8081/mcp`. It and HTTP Ask answer only for `localhost`, `127.0.0.1`, `::1`, `host.docker.internal` and the host of `HELIX_LOCAL__SITE_ORIGIN`. It supports the Foundry workflow: discovery, connectors, documents, ontology generation and revision, loading, graph reads and Ask. API sources and cloud administration are unavailable locally. The site’s **Set up with your agent** prompt carries your workspace and current setup into the agent.

Use the install’s existing owner token as a bearer. From the repository root, load it into the environment without printing it:

```sh
export FOUNDRY_LOCAL_TOKEN="$(docker compose exec -T foundry cat /secrets/site-token)"
```

Configure the MCP client to read `FOUNDRY_LOCAL_TOKEN` and send `Authorization: Bearer <token>`. The token acts as the owner of every local workspace. Keep it in the client’s environment, out of prompts and browser code. No OAuth sign-in is required. The API port is published only on loopback; tools run on this computer.

Changes use `helix_prepare_foundry_operation` followed by `helix_execute_foundry_operation` after confirmation. The confirmation lasts five minutes, binds the exact operation and payload, and can be consumed once: one that has expired or doesn't match fails with `failed_precondition`, so prepare it again. After a lost response or an already-consumed confirmation, read the current state before retrying: token consumption does not prove the operation finished.

## Ask over HTTP

Use `POST http://localhost:8081/v1/ontologies/<ontology-id>/ask` with the same bearer, `Content-Type: application/json`, an `Idempotency-Key` of at most 64 bytes of UTF-8, compared exactly, and `{"question":"What is in the data?"}`. The ontology must have a ready or partial current load and an AI provider configured.

A request waits up to 25 seconds. A terminal question returns 200; a pending question returns 202 and a relative `Location`. GET that location with the same bearer until `done` is true. Inspect `status`, including `cannot_answer`, `failed` and `cancelled`. Reuse the idempotency key after a lost POST response; it returns the original question and load. Disconnecting stops the wait, not the durable question.

Answers and citations use the same compact result as MCP, without query output rows; an integer past 2^53 comes as a string of its digits, which JSON numbers would round. See [the HTTP contract](../contract/foundry-integration.openapi.json) and [the Python tool example](../examples/ask_tool.py). Hosted, only the development stamp serves these routes for now, on `https://mcp.dev.helix-db.com`, with an existing service credential granted query-read access to the ontology’s project; `https://mcp.helix-db.com` doesn't serve them yet.
