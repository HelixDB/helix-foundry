# Helix Foundry on your machine

One owner, no sign-in, no team and no billing: every connector and feature is on. Needs Docker with Compose v2.

```sh
./setup.sh                 # once: writes .env with this installation's secrets, and asks how AI should run
docker compose up -d --wait
open http://localhost:3000 # that name: the site answers no other host
```

Keep `.env`. `FOUNDRY_CONNECTOR_KEY` seals every database password and API key you give Foundry, so losing it makes them unreadable. `setup.sh` never overwrites it. `.env.example` names every variable.

## What runs

| Service          | What it is                                                                                      |
| ---------------- | ----------------------------------------------------------------------------------------------- |
| `site`           | The Foundry app, on 127.0.0.1:3000. The only way in.                                            |
| `foundry`        | Foundry's API, its directory of workspaces and databases, and its workers, in one process.      |
| `ontology-agent` | Drafts ontologies, reads documents and answers questions, on the AI provider you choose.        |
| `storage`        | S3-compatible store (SeaweedFS) for synced tables and documents, on 127.0.0.1:8333 for uploads. |
| `mysql`          | Foundry's database.                                                                             |
| `helix`          | HelixDB (the public helix-db image): one database every ontology's loads share.                 |
| `ollama`         | Ollama, and `ollama-pull`, which downloads its model: with the `ollama` profile only.           |

Data lives in the Compose volumes `mysql`, `storage`, `helix` and `work` (and Ollama's models in `ollama`). `docker compose down` keeps them; `docker compose down -v` deletes everything.

## AI

Drafting ontologies, reading documents and answering questions run on an AI provider. `setup.sh` asks which, and makes it the install's default (`FOUNDRY_AI_*` in `.env`):

- **Ollama in Docker.** A local model next to Foundry: nothing it is sent leaves this machine. `setup.sh` turns on the `ollama` profile (`COMPOSE_PROFILES=ollama`); the first `docker compose up` downloads the model in the background (`docker compose logs -f ollama-pull` shows it), and drafts fail until it is there. The default model is `qwen3.5:4b` for now, a candidate until a real draft on a GPU picks the smallest model that finishes one. It takes 5.7 GB of memory with drafting's 65,536-token context window, so give Docker at least 10 GB (`setup.sh` warns below that); `qwen3.5:9b` takes 8.8 GB. Small models pass Foundry's test but may stop a draft early.
- **Ollama on this computer.** The Ollama you install from ollama.com and run yourself, reached from Docker as `http://host.docker.internal:11434`. Pull the model first (`ollama pull qwen3.5:4b`). **On Linux, start Ollama with `OLLAMA_HOST=172.17.0.1`**, Docker's bridge address, which `host.docker.internal` is (`ip -4 addr show docker0` shows it if yours differs), so Docker can reach it and your network can't. That address exists once Docker runs, so start Ollama after it, and give the `ollama` command the same `OLLAMA_HOST`. `OLLAMA_HOST=0.0.0.0` works too, but opens Ollama, which takes no key, to every machine that can reach this one: use it only behind a firewall.
- **Claude or OpenAI, with your API key.** `setup.sh` offers the `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` in your shell if there is one.
- **An OpenAI-compatible server**: LM Studio, vLLM, Ollama's `/v1` or a gateway such as LiteLLM. `setup.sh` asks for its base URL (including `/v1`), its API format (Chat Completions, which most servers speak, or Responses), the model as the server names it, and its key if it takes one. Every task runs on that model, with no cost counted. Foundry runs in Docker, so a server on this computer is `host.docker.internal`, not `localhost`: LM Studio is `http://host.docker.internal:1234/v1`, vLLM `http://host.docker.internal:8000/v1`, Ollama `http://host.docker.internal:11434/v1`. The server must answer on that address: turn on LM Studio's "Serve on Local Network", and on Linux start the server listening on `0.0.0.0` (Ollama: `OLLAMA_HOST=0.0.0.0`). The model must call tools and answer in structured JSON (vLLM needs `--enable-auto-tool-choice` and its model's `--tool-call-parser`); Settings › AI models tests it before Foundry uses it, and small local models may still stop drafts early.
- **Amazon Bedrock, with a Bedrock API key.** `setup.sh` asks for the region (`FOUNDRY_AI_REGION`), the key (offering the `AWS_BEARER_TOKEN_BEDROCK` in your shell) and a model or inference-profile ID that supports tool use (`FOUNDRY_AI_MODEL`); without one, Claude's models through Bedrock's global inference profiles, one per task, which may run requests in any AWS commercial region (a geography's profiles, like `eu.anthropic.claude-sonnet-5-5`, stay within it; GovCloud has none, so enter a model there). Use a long-term key: the default lasts as long as it does.
- **None yet.** Data, connectors and Explore work; drafting, uploading documents and asking wait until you choose a provider, and say so with a link to Settings › AI models.

Either way, **Settings › AI models** shows where AI runs and what each task sends, and switches it: Ollama (bundled, on this computer or another server, with its model and context window), Claude, OpenAI, Azure OpenAI or Amazon Bedrock with a key, an OpenAI-compatible server, tested before Foundry uses it, or back to the install's default. A key entered there is stored sealed with `FOUNDRY_CONNECTOR_KEY`, never in `.env`. To change the install's default, edit `.env` and run `docker compose up -d` again; an empty `FOUNDRY_AI_PROVIDER` switches it off, whatever else `.env` sets. Switching provider, set each task's model (`FOUNDRY_AI_DRAFT_MODEL`, `FOUNDRY_AI_ASK_MODEL`, `FOUNDRY_AI_EXTRACT_MODEL` and `FOUNDRY_AI_VERIFY_MODEL`, each of which wins over `FOUNDRY_AI_MODEL`) to the new provider's, or delete them.

### Ollama in Docker

- **GPU.** On a machine with an NVIDIA GPU and the NVIDIA Container Toolkit, `compose.gpu.yaml` gives Ollama the GPUs: `setup.sh` adds `COMPOSE_FILE=compose.yaml:compose.gpu.yaml` to `.env` when Docker has the NVIDIA runtime, or run `docker compose -f compose.yaml -f compose.gpu.yaml up -d`. On a Mac, Docker has no GPU: Ollama on this computer uses Apple's.
- **The model.** `FOUNDRY_OLLAMA_MODEL` in `.env`, pinned by `FOUNDRY_OLLAMA_DIGEST`: `ollama-pull` fetches it only when the volume lacks it, and stops with an error when it isn't that digest. It never changes by itself. To update it, run `docker compose exec ollama ollama pull <model>`: the next start's `ollama-pull` stops, naming the new digest to set in `FOUNDRY_OLLAMA_DIGEST` (or empty it). To use another model, set both and run `docker compose up -d`, then choose it in Settings › AI models or set `FOUNDRY_AI_MODEL`.
- **Memory.** Ollama loads one model at a time, runs one call at a time, and keeps the model loaded for 10 minutes after the last call. When memory runs out, Ollama is the first process killed, so a model call fails and HelixDB and MySQL keep running. A larger context window takes more memory: Ollama loads models with `FOUNDRY_AI_CONTEXT_TOKENS`, drafting's 65,536 when empty.
- **Only Foundry reaches it.** Ollama's API takes no key, so it listens on the agent's loopback alone: no other container, and nothing outside Docker, can reach it.

## Connecting databases

Foundry connects from inside Docker: a database on this machine is `host.docker.internal`. Private addresses are allowed.

## Ports

The site must stay on `http://localhost:3000` and the store on `http://localhost:8333`: the site refuses any other host, and documents upload to the address Foundry signs. To move them, edit `compose.yaml`:

- **The site:** the host side of its port, `FOUNDRY_ORIGIN`, and the store's `-s3.allowedOrigins`, which lets the site's page upload.
- **The store:** the `pod`'s port, the store's `-s3.port` and its health check's `nc -z 127.0.0.1 <port>`, and `HELIX_LOCAL__S3__ENDPOINT: http://localhost:<port>` under foundry's `environment` (its config is in the image; `HELIX_*` variables override it).
