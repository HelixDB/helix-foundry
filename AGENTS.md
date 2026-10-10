# Helix Foundry agent runbook

This runbook is for coding agents (Claude Code, Codex, Cursor and others) that are asked to set up Helix Foundry on someone's computer or to work on its code. People should start with [README.md](README.md).

## Goal

Install and start Helix Foundry with Docker Compose, prove that it is healthy, and give the user a URL to open. You are done when this command returns `{"ok":true,"service":"helix-foundry"}`:

```sh
curl -fsS http://localhost:<PORT>/api/health
```

`<PORT>` is 3001 unless you chose another port. Guided setup in the browser (AI → Data → Ontology) belongs to the user, unless they ask you to do part of it.

## Ground rules

- Run every command from the repository root.
- Ask the user before anything destructive or disruptive: removing containers, volumes, images or files, restoring a backup, stopping or killing a process you did not start, or installing software.
- Never print, `cat`, paste into chat or commit `.env` or a backup's `config.env`. They hold the encryption key. Do not open `.env` with an editor or file-reading tool either, because that puts the keys in your transcript. Use the `grep` and `sed` commands in this runbook, which change or show only the non-secret `PUBLIC_PORT`, `PUBLIC_ORIGIN` and `MODEL_ENDPOINT` lines.
- Never delete, regenerate or overwrite an existing `.env`. Losing `ENCRYPTION_KEY` makes stored credentials unrecoverable.
- If a port is busy, choose another port. Do not stop whatever is using it.
- Do not ask for, type or store the user's Claude or OpenAI keys or database passwords. The user enters them in the browser.
- Do not install software (Docker, Ollama, Node, browsers) without asking, and never use `sudo`.
- Shell variables usually do not survive between your tool calls. Every block below that needs the app's port reads it from `.env` again. Never assume 3001: another program, or a development copy of Helix Foundry, may be listening there.
- Re-running these steps is safe. `scripts/setup.sh` never overwrites an existing `.env`, and step 2 keeps the port of an installation that is already running.

## 1. Get the code

```sh
git clone https://github.com/helixdb/helix-foundry.git
cd helix-foundry
```

Skip the clone if you are already in a checkout (it contains `compose.yaml` and `scripts/setup.sh`). If the clone fails with "could not read Username", "Repository not found" or another access error, stop and ask the user for the right repository URL or the path to their copy.

## 2. Preflight checks

### Docker and tools

```sh
docker version --format 'Docker {{.Server.Version}}'
docker compose version
git --version && openssl version && bash --version | head -n 1
```

- `docker: command not found`: Docker is not installed. Stop and ask the user to install Docker Desktop, or Docker Engine with the Compose plugin. Do not install it yourself.
- "Cannot connect to the Docker daemon": Docker is not running. Ask the user to start Docker Desktop or the Docker service, then retry.
- "permission denied" on `docker.sock` (Linux): the user's account cannot use Docker. Stop and ask the user to fix it. Never retry with `sudo`.
- `docker compose version` must report v2. The compose file needs the Compose v2 plugin, not the old `docker-compose`. It has been checked with v2.29. If only `docker-compose` exists, ask the user to install the Compose v2 plugin.
- Node.js and pnpm are not needed for the Docker install.

### Existing installation

```sh
test -f .env && echo ".env exists" || echo "no .env yet"
docker volume ls --filter name=helix-foundry_ --format '{{.Name}}'
docker compose ls --all --filter name=helix-foundry
docker ps --filter label=com.docker.compose.project=helix-foundry \
  --filter label=com.docker.compose.service=app --format 'app running: {{.Status}} {{.Ports}}'
```

The Compose project name is always `helix-foundry`, whichever folder it runs from, so every checkout on this computer shares the same containers and volumes.

| What you found                                                                                       | What it means and what to do                                                                                                                                                                                                                                         |
| ---------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| No `.env`, no `helix-foundry_` volumes, no project                                                   | Fresh install. Continue.                                                                                                                                                                                                                                             |
| `.env` exists                                                                                        | Existing install. Keep `.env` and its settings, and continue. `setup.sh` will leave it alone.                                                                                                                                                                        |
| No `.env`, but `helix-foundry_helix-data` or `helix-foundry_foundry-data` exists                     | Stop and ask the user. That data was written with a key from an earlier `.env`. Ask whether they have that file or a backup `config.env`. A new key leaves the workspaces in place but makes their stored credentials unreadable. (`helix-foundry_model-data` alone holds only model weights and is safe to reuse.) |
| A `helix-foundry` project is listed whose CONFIG FILES path is not this folder's `compose.yaml`      | Stop and ask the user which checkout to use. Starting from this folder would take over and recreate the running containers, and use their volumes with this folder's `.env`.                                                                                        |
| `app running:` is printed and the project is this folder                                             | Helix Foundry is already running here. Keep its port (the number before `->`), skip the port check, and continue at step 5 unless the user asked you to upgrade.                                                                                                    |

If `.env` exists, check that its keys are set without printing them:

```sh
grep -Eq '^ENCRYPTION_KEY=.{32,}$' .env && grep -Eq '^INTERNAL_KEY=.{32,}$' .env \
  && echo "keys ok" || echo "keys missing or too short"
```

"keys missing or too short" usually means `.env` was copied from `.env.example`. See [troubleshooting](#troubleshooting).

### Ports

Compose publishes only the app, on `127.0.0.1`, port 3001 by default (or `PUBLIC_PORT` from `.env`). HelixDB, the executor and Ollama publish no ports, so other services on 6996, 3002 or 11434 do not matter for the Docker install.

Skip this check if Helix Foundry is already running from this folder (see above). Otherwise run this block as one command. It tests the configured port first, then a few alternatives, and also looks at ports held by other containers, which `lsof` cannot see on Linux without root:

```sh
CONF=$(sed -n 's/^PUBLIC_PORT=//p' .env 2>/dev/null | tail -n 1); CONF=${CONF:-3001}
busy() {
  if command -v ss >/dev/null 2>&1; then [ -n "$(ss -ltnH "sport = :$1")" ] && return 0
  else lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1 && return 0; fi
  docker ps --format '{{.Ports}}' | grep -q ":$1->"
}
found=""
for p in "$CONF" 3005 3011 3021 3031 3041; do busy "$p" || { found=$p; break; }; done
if [ -z "$found" ]; then echo "No free port found: ask the user which port to use"
elif [ "$found" = "$CONF" ]; then echo "Port $CONF is free: keep it"
else echo "Port $CONF is busy: use $found"; fi
```

If it prints `use <port>`, remember that number for step 3.

### Disk and memory

```sh
uname -sm
docker info --format 'Docker memory: {{.MemTotal}} bytes, CPUs: {{.NCPU}}'
df -h .
```

- The first run downloads several GB: the built app image (about 0.9 GB), HelixDB (about 160 MB), Ollama, and the `qwen3:4b` model (roughly 2.5 GB). If disk space is tight, tell the user before you continue.
- Local AI was developed and benchmarked on a 16 GB machine, and the executor alone may use up to 2 GB. Peak memory has not yet been certified for 16 GB or 8 GB installations. Compose gives each service a memory budget, and the bundled model uses about 3.3 GB while loaded (see [docs/OPERATIONS.md](docs/OPERATIONS.md#memory)). Use the `MemTotal` figure:
  - Below 8000000000 bytes: warn the user that local AI and a large workspace do not fit together in Docker: questions may fail and the database may restart. Offer path B or C below. Setup in the browser shows the same warning when Local is chosen.
  - From 8000000000 to 16000000000 bytes: continue, and mention in your report that local AI has not been certified at this memory size.
  - Above 16000000000 bytes: continue.

### Choose a model path

| Path                    | When to use it                                                                                                         | Start command                                                   |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------- |
| **A. Ollama in Docker** | The default, on any OS.                                                                                                | `docker compose --profile local up -d --build`                  |
| **B. Native Ollama**    | macOS with Docker Desktop, when Ollama is already running. Faster on Apple Silicon, because Docker cannot use Metal.   | `docker compose up -d --build`, with `MODEL_ENDPOINT` in `.env` |
| **C. Hosted AI only**   | The user says they will use Claude or OpenAI and wants to skip the model download.                                     | `docker compose up -d --build`, with no `MODEL_ENDPOINT`        |

Decide like this, and say which path you used in your report:

```sh
uname -s
docker info --format '{{.OperatingSystem}}'
command -v ollama
curl -fsS --max-time 5 -o /dev/null http://127.0.0.1:11434/api/tags && echo "Ollama is running"
```

- Use **path B** only when all three hold: `uname -s` prints `Darwin`, Docker reports `Docker Desktop`, and Ollama is running. Native Ollama keeps the model in the user's own Ollama, and the app downloads `qwen3:4b` there if it is missing.
- If Ollama is installed but not running, ask the user whether to start it (with the Ollama app, or `open -a Ollama` if they agree) or to use path A. Do not start `ollama serve` from your own shell: it can stop when your session ends, and the app then loses its model without an obvious error.
- Otherwise use **path A**. That includes Linux, where `compose.yaml` has no `host.docker.internal` mapping, and other Docker runtimes such as OrbStack or Colima, where path B is untested.
- Use **path C** only when the user asks for it.
- Do not install Ollama without asking.

On path C, choosing Local during setup fails with "Local AI is unavailable. Start Ollama and retry." That is expected. The user picks Claude or OpenAI instead. Setup also works with no AI at all; AI only adds descriptions and picks Home's key metrics.

## 3. Create and adjust `.env`

```sh
./scripts/setup.sh
```

On the first run this prints `Generated local secrets in .env`. It writes `ENCRYPTION_KEY` and `INTERNAL_KEY` (64 hex characters each) to a file only the user can read. If `.env` already exists, it does nothing.

Do not run `cp .env.example .env` first. `setup.sh` then skips key generation, and Compose refuses to start.

**Other port.** If step 2 printed `use <port>`, set both lines to that port. Replace `3005` in the first line with it. The block updates the lines if they exist and adds them if not, without printing the keys and without changing the file's permissions, and it is safe to run twice:

```sh
PORT=3005   # the port step 2 printed
tmp=$(mktemp .env.XXXXXX) && sed -e "s|^PUBLIC_PORT=.*|PUBLIC_PORT=$PORT|" \
  -e "s|^PUBLIC_ORIGIN=.*|PUBLIC_ORIGIN=http://localhost:$PORT|" .env > "$tmp" \
  && cat "$tmp" > .env && rm -f "$tmp"
grep -q '^PUBLIC_PORT=' .env || echo "PUBLIC_PORT=$PORT" >> .env
grep -q '^PUBLIC_ORIGIN=' .env || echo "PUBLIC_ORIGIN=http://localhost:$PORT" >> .env
grep -E '^PUBLIC_(PORT|ORIGIN)=' .env
```

The last command shows only those two lines, and they must name the same port. The temporary `.env.XXXXXX` file matches `.gitignore` and `.dockerignore`.

**Path B.** Point the app at native Ollama:

```sh
grep -q '^MODEL_ENDPOINT=' .env || echo 'MODEL_ENDPOINT=http://host.docker.internal:11434' >> .env
```

**Paths A and C.** Make sure `MODEL_ENDPOINT` is not set, because it overrides the bundled Ollama:

```sh
grep -E '^MODEL_ENDPOINT=' .env || echo "MODEL_ENDPOINT not set"
```

The other lines `setup.sh` writes (`APP_ORIGIN`, `HELIX_URL`, `EXECUTOR_URL`, `OLLAMA_URL`, `DATA_DIR`) are only for `pnpm dev`. Compose ignores them, so leave them alone.

## 4. Start the stack

Path A (the same as `./scripts/setup.sh --compose`):

```sh
docker compose --profile local up -d --build
```

Paths B and C:

```sh
docker compose up -d --build
```

The first run builds the image (it installs dependencies and builds the web UI) and pulls images, which takes several minutes. Give the command a long timeout, at least 15 minutes. If your tool caps how long a command may run (Claude Code allows 10 minutes in the foreground), either:

- run the start command in the background, wait for it to exit, and check that its exit code is 0, or
- split it into three shorter commands, each with its own timeout. They are shown for path A; on paths B and C, leave out `--profile local`:

  ```sh
  docker compose --profile local pull --ignore-buildable
  docker compose build
  docker compose --profile local up -d
  ```

The start command returns once the containers have started. At that point the app may not be healthy yet, and the model may still be downloading.

## 5. Wait until it is ready

Run this as one command with a timeout of at least 6 minutes:

```sh
PORT=$(sed -n 's/^PUBLIC_PORT=//p' .env | tail -n 1); PORT=${PORT:-3001}
end=$((SECONDS + 300)); ok=""
while [ "$SECONDS" -lt "$end" ]; do
  curl -fsS --max-time 5 "http://localhost:$PORT/api/health" 2>/dev/null && { ok=1; break; }
  sleep 5
done
if [ -n "$ok" ]; then printf '\nReady on port %s\n' "$PORT"; else echo "NOT READY after 5 minutes on port $PORT"; fi
```

It waits up to five minutes and always ends with `Ready on port …` or `NOT READY …`. An HTTP 200 with `{"ok":true,"service":"helix-foundry"}` means the API is up and HelixDB answered at startup, because the app initialises its schema in HelixDB before it starts listening. On first boot, the app and worker can exit and restart while HelixDB is still starting. Docker restarts them automatically.

If it is still not ready after five minutes, collect the state and use [troubleshooting](#troubleshooting):

```sh
docker compose --profile local ps -a
docker compose --profile local logs --tail 100 app helix
```

## 6. Verify

1. **Health:** the command in step 5 returned `{"ok":true,"service":"helix-foundry"}`.
2. **Containers:** `docker compose --profile local ps` shows `app` as `(healthy)`, and `worker`, `executor` and `helix` running. On path A, `ollama` is running too. The health check runs every 15 seconds, so `health: starting` for the first part of a minute is normal.
3. **Model (path A):** `docker compose --profile local ps -a model-init` shows `Exited (0)` once `qwen3:4b` is downloaded. While it is still running, the download is in progress. The user does not need to wait: if the model is still missing when they choose Local, the app downloads it itself.
4. **Deeper check** from inside the app container (optional on paths A and C, required on path B). It checks HelixDB, the executor, the model endpoint and the key lengths:

   ```sh
   docker compose exec app node --import tsx scripts/doctor.ts
   ```

   Expect `✓ HelixDB available`, `✓ DuckDB executor available` and `✓ Local model available`. On path A it also prints `Docker memory`; a line starting with `!` means Docker has less than the 8 GB local AI needs, so tell the user. `✗ Local model` is expected on path C. On path B it means the container cannot reach native Ollama through `host.docker.internal`: tell the user, and offer to switch to path A by removing the `MODEL_ENDPOINT` line and running `docker compose --profile local up -d`. If the command itself cannot run, rely on checks 1 and 2.

Do not run `pnpm doctor` on the host to check a Docker install. It reads the development URLs from `.env` and reports failures against a healthy stack.

## 7. Report back to the user

Tell the user the URL, the path you used, and what they do next. Never include the contents of `.env`. For example:

```text
Helix Foundry is running at http://localhost:3001

Next, in your browser:
1. Open the URL. There is no account or sign-in.
2. AI: choose Local (runs on this computer), or Claude or OpenAI and paste your API key.
3. Data: connect a source, or choose "Continue with sample data" to try it with a fictional company.
4. Ontology: review the suggested ontology, then click "Looks right — build my workspace".
Home then opens with your key metrics.

The local model is still downloading in the background (or: is ready).
Your encryption key is in .env. Back up with ./scripts/backup.sh <folder outside
the repo>, and keep .env and backups private.
```

Mention anything unusual: a port other than 3001, native Ollama, low memory, or warnings you saw in the logs.

## Optional: load the sample data for the user

Only do this when the user asks. Requests from this computer without an `Authorization` header act as the local owner, so plain `curl` works. Send JSON bodies with a content type.

These calls change data, so first make sure they reach this installation and not another program. Every block reads the port from `.env` again, because shell variables do not carry over between tool calls:

```sh
PORT=$(sed -n 's/^PUBLIC_PORT=//p' .env | tail -n 1); PORT=${PORT:-3001}
docker compose ps app --format '{{.Status}}  {{.Ports}}'
curl -fsS "http://localhost:$PORT/api/v1/me"
```

Continue only if the first line shows `(healthy)` and `127.0.0.1:<PORT>->3001/tcp` with the same port. If it does not, stop: something else is answering on that port. Otherwise note `workspaces[0].id` from the reply and use it in place of `WORKSPACE_ID`:

```sh
PORT=$(sed -n 's/^PUBLIC_PORT=//p' .env | tail -n 1); PORT=${PORT:-3001}
BASE=http://localhost:$PORT/api/v1; W=WORKSPACE_ID
curl -fsS -X POST "$BASE/workspaces/$W/onboarding/provider" \
  -H 'content-type: application/json' \
  -d '{"provider":"local","model":"qwen3:4b","advanceToData":true}'
curl -fsS --max-time 540 -X POST "$BASE/workspaces/$W/demo" \
  -H 'content-type: application/json' -d '{"advance":true}'
curl -fsS -X POST "$BASE/workspaces/$W/onboarding/ontology" \
  -H 'content-type: application/json' -d '{}'
```

- Call `provider` before `demo`. In the other order, it moves setup back to the Data step. Skip the `provider` call on path C.
- `demo` returns only after all 11 sample datasets are imported. Until then, the `ontology` call returns 409 "Sample data is still loading."
- The last call returns the setup state. The suggested ontology is under `ontology`, with its `hash`.

Then stop, and ask the user to review the suggestion in the browser and click **Looks right — build my workspace**. If they asked you to accept it as it is:

```sh
PORT=$(sed -n 's/^PUBLIC_PORT=//p' .env | tail -n 1); PORT=${PORT:-3001}
BASE=http://localhost:$PORT/api/v1; W=WORKSPACE_ID
curl -fsS -X POST "$BASE/workspaces/$W/onboarding/ontology/confirm" \
  -H 'content-type: application/json' -d '{"hash":"ONTOLOGY_HASH"}'
for i in $(seq 1 60); do
  curl -fsS --max-time 5 "$BASE/workspaces/$W/onboarding" | grep -q '"complete":true' && { echo "complete"; break; }
  sleep 5
done
```

The loop waits up to five minutes and prints `complete` once the workspace is published. If it ends without printing anything, read `buildRun` (its status and stage) from `GET /api/v1/workspaces/WORKSPACE_ID/onboarding` and tell the user. Once it is complete, Home shows the workspace. If there are several workspaces, the browser may open a different one than `workspaces[0]`.

## Troubleshooting

| Symptom                                                                                  | Cause                                                                                            | Fix                                                                                                                                                                                                                          |
| ---------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `required variable ENCRYPTION_KEY (or INTERNAL_KEY) is missing a value: Run scripts/setup.sh first` | No `.env`, or `.env` was copied from `.env.example` with blank keys. Compose may name either key. | With no `.env`, run `./scripts/setup.sh`. With blank keys and no `helix-foundry_` volumes, ask the user, rename the file to `.env.bak`, run `./scripts/setup.sh`, then copy any `PUBLIC_*` or `MODEL_ENDPOINT` lines across. |
| `Bind for 127.0.0.1:3001 failed: port is already allocated`, or `address already in use` | Another program uses the port.                                                                   | Set `PUBLIC_PORT` and `PUBLIC_ORIGIN` to a free port (step 3), then run the start command again.                                                                                                                             |
| Pages load, but every save fails with `403 Origin is not allowed`                        | `PUBLIC_PORT` changed without `PUBLIC_ORIGIN`, or the app was opened on a different port.        | Set both to the same port, run the start command again, and open exactly the `PUBLIC_ORIGIN` URL.                                                                                                                            |
| `403 Host is not allowed`                                                                | The app was opened by LAN IP or another hostname.                                                | Use `localhost` or `127.0.0.1`. Do not try to allow other hosts.                                                                                                                                                             |
| Health check never passes; `app` keeps restarting                                        | HelixDB was not ready at first boot, or the keys are missing or too short.                       | Wait a minute and check `docker compose logs --tail 100 app helix`. "Run ./scripts/setup.sh to generate ENCRYPTION_KEY and INTERNAL_KEY" means the keys are the problem: see the first row.                                  |
| Setup says "Local AI is unavailable. Start Ollama and retry."                            | Started without `--profile local` and without `MODEL_ENDPOINT`, or native Ollama is not running. | Path A: `docker compose --profile local up -d`. Path B: start Ollama and check `MODEL_ENDPOINT`. Or choose Claude or OpenAI.                                                                                                 |
| `model-init` keeps running or restarting                                                 | The model download is slow or was interrupted.                                                   | Wait, and check `docker compose --profile local logs --tail 50 model-init`. It retries on failure.                                                                                                                                  |
| "Default model digest does not match the tested release"                                 | The registry's `qwen3:4b` no longer matches the pinned digest in `docs/models.json`.             | Tell the user. They can choose Claude or OpenAI instead.                                                                                                                                                                     |
| Code changes or `git pull` have no effect                                                | Compose reused the old `helix-foundry:local` image.                                              | Start with `--build`.                                                                                                                                                                                                        |
| Editing `APP_ORIGIN`, `HELIX_URL` or `OLLAMA_URL` in `.env` has no effect                | Those lines are only for `pnpm dev`.                                                             | Use `PUBLIC_PORT`, `PUBLIC_ORIGIN` and `MODEL_ENDPOINT` for the Docker install.                                                                                                                                              |
| Values in `.env` seem to be ignored                                                      | The same variable is exported in the shell, which overrides `.env`.                              | Run `env` and look for `PUBLIC_PORT`, `PUBLIC_ORIGIN` or `MODEL_ENDPOINT`; unset any you find.                                                                                                                                      |
| On Linux, the app cannot reach `http://host.docker.internal:11434`                       | `host.docker.internal` is a Docker Desktop name, and `compose.yaml` adds no mapping for it.      | Use path A on Linux.                                                                                                                                                                                                         |
| `pnpm doctor` reports failures against a running Docker install                          | It checks the development URLs, and Compose publishes no internal ports.                         | Use `/api/health` and the in-container doctor from step 6.                                                                                                                                                                   |
| Startup fails with "Database schema is newer than this application"                      | The code is older than the data, for example after a downgrade.                                  | Ask the user. Check out the newer release again, or restore the backup that matches this release.                                                                                                                            |
| "The database stopped because Docker ran out of memory"                                  | Docker's memory filled up and the kernel stopped HelixDB. Docker restarts it.                    | Wait a minute and retry. Then give Docker more memory, or switch to Claude or OpenAI. See [docs/OPERATIONS.md](docs/OPERATIONS.md#memory). Do not raise the memory limits in `.env` above what Docker has.                    |
| `ollama` or `model-init` keeps running after stopping, or a network is left behind       | The stack was started with `--profile local` but stopped without it.                             | Pass the same `--profile local` to `stop`, `down`, `logs` and `ps`.                                                                                                                                                          |

## Operations

Use `docker compose --profile local` on path A and plain `docker compose` on paths B and C.

```sh
docker compose --profile local ps -a                        # status
docker compose --profile local logs --tail 100 app worker   # recent logs (JSON, auth headers redacted)
docker compose --profile local stop                         # stop
docker compose --profile local up -d                        # start again
docker compose --profile local restart                      # restart
```

Do not run `logs -f` from an agent tool. It follows the logs and never returns.

Every long-running service uses `restart: unless-stopped`, so the stack comes back after a reboot or Docker restart unless it was stopped.

**Back up.** Keep backups outside the repository. `backups/` is not in `.gitignore` or `.dockerignore`, so a backup inside the repository could be committed or copied into the image.

```sh
./scripts/backup.sh "$HOME/helix-foundry-backups/$(date +%Y%m%d-%H%M%S)"
```

It briefly stops `app`, `worker`, `executor` and `helix`, archives the `helix-data` and `foundry-data` volumes with the `alpine:3.22` image, copies `.env` to `config.env`, and starts the services again. Model weights are not included; they can be downloaded again.

**Upgrade.** Tell the user first, because the backup stops the app briefly.

```sh
./scripts/backup.sh "$HOME/helix-foundry-backups/$(date +%Y%m%d-%H%M%S)"
git pull
docker compose --profile local up -d --build
```

Then repeat steps 5 and 6. Never remove volumes during an upgrade. Keep the old commit and backup so you can return to them. Startup refuses a schema newer than the app, so going back needs the matching backup.

**Restore.** Only with the user's explicit approval. It wipes both data volumes and overwrites `.env`:

```sh
./scripts/restore.sh PATH --replace
```

The backup must contain `helix-data.tgz`, `foundry-data.tgz` and `config.env`. If the stack uses a non-default `COMPOSE_PROJECT_NAME`, set the same name for both backup and restore.

**Remove completely.** Only when the user asks, and after offering a backup. This deletes every workspace, all imported data, stored credentials and the downloaded model:

```sh
docker compose --profile local down -v --rmi all
```

Then the user can delete `.env` and the repository.

## Never do

- Expose the app beyond this computer. Do not use a reverse proxy, tunnel or port forward, change the `127.0.0.1:` binding in `compose.yaml`, or set `PUBLIC_ORIGIN` to a public hostname (for example, to get an OAuth callback). There is no sign-in.
- Publish the HelixDB, executor or Ollama ports.
- Print, paste, log or commit `.env`, `config.env` or a backup.
- Delete or regenerate `.env` on an existing install, or "rotate" `ENCRYPTION_KEY` by replacing its value.
- Run `docker compose down -v`, `docker volume rm helix-foundry_*` or `restore.sh --replace` without the user's explicit approval. Never remove volumes during an upgrade.
- Store backups inside the repository.
- Run more than one worker, for example with `--scale worker=2`.
- Install it on a shared or multi-user server, or let other devices use it. Each installation serves one person on their own computer.
- Stop or kill processes that are using the ports you need.
- Drop replication slots or publications on the user's databases that belong to other applications.

## Development setup

Use this section when the user asks you to change the code rather than only run the app.

**Toolchain.** Node.js 24 (`.node-version`) and pnpm 10.7.0 (`packageManager` in `package.json`). pnpm only warns on an older Node, so check `node -v` and switch with fnm, nvm or Volta if it is below 24.

```sh
pnpm install
./scripts/setup.sh
```

The development `.env` points at the web app on 5173, HelixDB on 6996, the executor on 3002 and Ollama on 11434. Without `.env`, the code falls back to HelixDB on 6969, so always run `setup.sh` first.

**HelixDB.** Look for an existing development HelixDB before creating one. The user may already run one under another name:

```sh
docker ps -a --filter name=helix-foundry --format '{{.Names}}  {{.Status}}  {{.Ports}}'
docker ps --filter publish=6996 --format '{{.Names}}  {{.Image}}'
lsof -nP -iTCP:6996 -sTCP:LISTEN
```

- A stopped `helix-foundry-dev` container: start it with `docker start helix-foundry-dev`.
- Port 6996 already in use: ask the user what it is. If it is their Helix Foundry development HelixDB, reuse it as it is. If not, publish a new container on another free port and set `HELIX_URL` in `.env` to match.
- Neither: create it.

```sh
docker run -d --name helix-foundry-dev -p 127.0.0.1:6996:8080 \
  -e HELIX_DATA_DIR=/var/lib/helix -v helix-foundry-dev:/var/lib/helix \
  ghcr.io/helixdb/helixdb:v0.0.6@sha256:94b29942658ebdca0a91bf15edffe921a46da3e26e1223ea333ccce58c6212dd
```

**Ollama (optional).** Check `curl -fsS --max-time 5 -o /dev/null http://127.0.0.1:11434/api/tags` first. If nothing answers, ask the user to start Ollama rather than running `ollama serve` from your own shell, which can stop when your session ends. `ollama pull qwen3:4b` is optional, because the app pulls the model when Local is chosen.

**Run.**

```sh
pnpm dev
```

This starts the executor on 3002, the API on 3001 with the background worker inside it, and Vite on 5173. Open http://localhost:5173. Do not use :3001 during development: it may serve an old build, and saves from it fail the origin check. The API exits at startup if the keys are missing or HelixDB is unreachable, so start HelixDB first. `pnpm doctor` checks HelixDB (`/readyz`), the executor (`/health`), Ollama (`/api/tags`) and the key lengths, using the development URLs in `.env`; `✗ Local model` is fine when the user works with a hosted provider. It is for this development setup only. For a Docker install, use the in-container doctor from step 6.

**Busy development ports.** Move all of them together:

- In `.env`, change `EXECUTOR_URL=http://127.0.0.1:3012`, `APP_ORIGIN=http://localhost:5183` and, if needed, `HELIX_URL` (the HelixDB port you published), and add `PORT=3011` and `EXECUTOR_PORT=3012`, which `setup.sh` does not write. Use the same `sed` and `grep -q … || echo … >> .env` pattern as step 3, and leave the two keys untouched.
- Vite reads its settings from the shell, not `.env`: `VITE_PORT=5183 API_PROXY=http://127.0.0.1:3011 pnpm dev`.

`pnpm dev` does not pass `--strictPort` to Vite, so if its port is taken it silently moves and saves fail with 403. Check that the URL Vite prints matches `APP_ORIGIN`. The `dev` entry in `.claude/launch.json` starts only Vite (5173, strict port); start the executor and API separately with `pnpm executor` and `pnpm start`.

**Checks before you finish** (the same as CI):

```sh
pnpm typecheck
pnpm test
pnpm build
```

- `pnpm test` uses in-memory stores and temporary loopback ports, so it needs no running services.
- `pnpm test:browser` starts its own API, executor and Vite on 3101, 3102 and 5174 (`PW_SLOT=1` to `9` shifts each by 10) with isolated data. It needs Playwright's Chromium; ask before downloading it with `pnpm exec playwright install chromium`.
- Opt-in suites: `TEST_HELIX=1` writes and then deletes a temporary scope in the HelixDB at `HELIX_URL`, which is the user's development instance. `TEST_MYSQL=1` needs the fixture MySQL on 127.0.0.1:33077. `TEST_CDC_DATABASES=1` and `TEST_ONBOARDING_DATABASES=1` need database fixtures. `ONBOARDING_LOCAL=1` uses real Ollama on 127.0.0.1:11434.
- Keep OAuth client secrets (`*_CLIENT_SECRET`) on the server. Never put them in frontend code.

**Layout.**

| Path                | Contents                                                     |
| ------------------- | ------------------------------------------------------------ |
| `apps/api`          | Fastify API, worker, connectors and guided setup             |
| `apps/web`          | React web app (Vite)                                         |
| `services/executor` | DuckDB query executor                                        |
| `packages/shared`   | Shared types and schemas                                     |
| `packages/sdk`      | TypeScript SDK                                               |
| `scripts`           | `setup`, `backup`, `restore`, `doctor`, `migrate`, benchmark |
| `tests`             | Vitest suites, and Playwright specs in `tests/browser`       |
| `docs`              | Guides and references                                        |

More detail: [docs/GUIDE.md](docs/GUIDE.md), [docs/OPERATIONS.md](docs/OPERATIONS.md), [docs/API.md](docs/API.md) and [docs/onboarding.md](docs/onboarding.md).
