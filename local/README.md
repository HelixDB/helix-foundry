# Helix Foundry on your machine

One owner, no sign-in, no team and no billing: every connector and feature is on. Needs Docker with Compose v2. Nothing to set up first:

```sh
docker compose up -d --wait  # from the repository's root, or from local/
open http://localhost:3000   # that name: the site answers no other host
```

It opens in your first workspace, **My workspace**, made as the install first starts.

**Secrets.** The first start writes this install's secrets (the site's token, the store's secret, MySQL's password and the connector key) into the Compose volume `secrets`, and every start reads them from there; nothing is printed or written to the checkout. Keep that volume: the connector key seals every database password and API key you give Foundry, is never made again once written, and losing it makes them unreadable. `docker compose down -v` deletes it with the data.

**No analytics.** A local build loads no tracker and sends no usage data anywhere; only the hosted site, foundry.helix-db.com, records product analytics.

**An install set up with `setup.sh`** (before 0.1.1) keeps its `local/.env`: its first start of this version copies the `FOUNDRY_*` secrets there into the volume, so MySQL's password and the key that sealed its credentials still match. Keep running it from `local/` if its `.env` sets `COMPOSE_PROFILES` or `COMPOSE_FILE` (Ollama in Docker, the GPU file): Compose reads those from the project's own `.env` only.

**Overrides** are optional, in `local/.env` (copy [`.env.example`](.env.example)): Compose reads it for this file whether it runs from the root or from here. Each is below.

## What runs

| Service          | What it is                                                                                      |
| ---------------- | ----------------------------------------------------------------------------------------------- |
| `site`           | The Foundry app, on 127.0.0.1:3000. The only way in.                                            |
| `foundry`        | Foundry's API, its directory of workspaces and databases, and its workers, in one process.      |
| `ontology-agent` | Drafts ontologies, reads documents and answers questions, on the AI provider you choose.        |
| `storage`        | S3-compatible store (SeaweedFS) for synced tables and documents, on 127.0.0.1:8333 for uploads. |
| `mysql`          | Foundry's database.                                                                             |
| `helix`          | HelixDB (the public helix-db image): one database every ontology's loads share.                 |
| `acme-db`        | PostgreSQL holding Acme Corp, the made-up sample company Use the sample connects to; no volume. |
| `ollama`         | Ollama, and `ollama-pull`, which downloads its model: with the `ollama` profile only.           |
| `init-secrets`   | Writes the install's secrets into the `secrets` volume once, then exits, before the rest start. |

Data lives in the Compose volumes `mysql`, `storage`, `helix` and `work`, its secrets in `secrets` (and Ollama's models in `ollama`). `docker compose down` keeps them; `docker compose down -v` deletes everything, the connector key with it.

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

## Images

`docker compose up` pulls Foundry's release images: `foundry-site` 0.1.2, `foundry` 0.1.1 and `ontology-agent` 0.1.0. To run the site and the backend built from this checkout instead, set `FOUNDRY_SITE_IMAGE=helix-foundry/site:local` and `FOUNDRY_IMAGE=helix-foundry/foundry:local` in `local/.env` and run `docker compose build` first.

The backend and ontology agent must speak the same agent contract. A running
agent's health check only says it is serving; it does not check that its
contract matches the backend. To check a running local stack using a backend
image built from this checkout, run:

```sh
docker compose exec foundry /usr/local/bin/foundry check-agent
```

This compares the sidecar's reported contract with the contract compiled into
that backend image. It makes no model calls and reads no workspace data. A
mismatch or an unavailable agent returns a nonzero exit code; use a matching
agent image before starting graph work. The command is part of the backend
built from this checkout and is not available in older released images. Use
matching release images when running the published release; don't assume the
release agent matches a backend built from a newer checkout. For a non-default
loopback address pass `--addr 127.0.0.1:PORT`. See the [agent contract](../backend/pb/proto/ontology_agent/v1/README.md)
for the version required by this checkout.

## Connecting databases

Foundry connects from inside Docker: a database on this machine is `host.docker.internal`. Private addresses are allowed.

## Ports

The site must stay on `http://localhost:3000` and the store on `http://localhost:8333`: the site refuses any other host, and documents upload to the address Foundry signs. To move them, edit `local/compose.yaml`:

- **The site:** the host side of its port, `FOUNDRY_ORIGIN`, and the store's `-s3.allowedOrigins`, which lets the site's page upload.
- **The store:** the `pod`'s port, the store's `-s3.port` and its health check's `nc -z 127.0.0.1 <port>`, and `HELIX_LOCAL__S3__ENDPOINT: http://localhost:<port>` under foundry's `environment` (its config is in the image; `HELIX_*` variables override it).
