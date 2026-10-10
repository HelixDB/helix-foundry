# Operating Helix Foundry

## Installation profiles

`./scripts/setup.sh --compose` starts the local-model profile and downloads the tested Qwen3 4B model. The app is available on `http://localhost:3001`. On Apple Silicon, native Ollama can use Metal: run the data services without `--profile local` and set `MODEL_ENDPOINT=http://host.docker.internal:11434` in `.env`.

Keep `.env`, data volumes, and backups private. The application encrypts connector/provider credentials using `ENCRYPTION_KEY`; losing that key makes those credentials unrecoverable. Do not rotate it by simply replacing the environment value. Helix Foundry has no sign-in, so do not admit other devices. The API refuses a non-loopback `HOST` unless `ALLOW_NETWORK_ACCESS=true`, which Compose sets because it publishes the app on `127.0.0.1` only. Do not expose it to a network through a reverse proxy, tunnel, or port forward. Internal HelixDB, Ollama, and executor ports are not published by Compose.

## Diagnostics and schema migration

`pnpm doctor` checks the database, executor, model endpoint, and local secrets. `/api/health` is a process health check. Startup applies the indexed version-1 metadata schema idempotently; `node --import tsx scripts/migrate.ts` applies it explicitly. Startup rejects database schema versions newer than the application.

Jobs claim leases and periodically renew them. A worker restart reclaims expired leases. AI stages save definitions and usage after each completed stage; a failed/canceled run can be resumed through Activity or the API. Input changes invalidate cached stages. Failed connector/pipeline jobs retry three times with bounded backoff. A stopped worker does not stop published dashboard queries.

## Backup, restore, and upgrades

Database synchronization prefers native CDC. PostgreSQL needs logical WAL, a replication-capable login, a primary key/usable replica identity, table publication permission, and available replication slots. Foundry creates a publication and slot scoped to each selected table. MySQL needs row-format binary logging, SELECT access, and replication client/slave privileges. Read-only logins still import through the scheduled fallback. Foundry never changes server-wide database configuration or writes business records back to sources.

The capture service runs independently of inference, saves its dirty counter and log position atomically in HelixDB, and acknowledges PostgreSQL only after that durable write. One queued snapshot coalesces multiple commits; its starting counter prevents concurrent changes from being marked applied accidentally. Existing complete snapshots stay available during imports. Reconnects reconcile a new snapshot, including after restoration of old metadata; schema changes still need review. Transient stream failures retry from saved positions, then fall back to scheduled refreshes. This is native log capture with full-snapshot materialization, not row-at-a-time replication: existing two-million-row import and executor limits still apply.

Monitor source-side WAL/binlog disk retention. Configure PostgreSQL's `max_slot_wal_keep_size` and a suitable MySQL binlog expiration policy for the installation; the application does not alter either server setting. Stop capture before manually removing Foundry-owned slots/publications. Keep their names from the source's sync metadata when decommissioning a source or changing its endpoint. Do not drop other applications' replication resources.

1. Run `./scripts/backup.sh PATH`. It stops writers and HelixDB, archives the data volumes, saves the encryption configuration, then restarts services.
2. Copy the backup to private off-host storage.
3. For a replacement installation, use `./scripts/restore.sh PATH --replace`. This replaces that installation's data volumes and `.env`.
4. Set `COMPOSE_PROJECT_NAME` when using a nondefault project name; use the same name throughout backup and restore.
5. Before upgrades, retain the old application commit, lockfile, image, and backup. Rebuild and run diagnostics, sample ingestion, a saved query, and proposal validation before using the installation again. Revert the application plus matching data backup if an upgrade fails.

Model weights are reproducible and excluded from application backups. Parquet versions and staged artifacts are immutable; unsuccessful previews can consume disk. There is no automatic artifact garbage collector in this MVP. Remove unreferenced files only during offline maintenance, after checking proposal/run previews and version records.

## Deployment limits

Two DuckDB jobs execute concurrently with a 512 MB per-process memory setting and a 120-second timeout. The executor container has a 2 GB memory cap and an internal-only network; see [Memory](#memory) for the other services. One worker performs one inference job at a time. Use one worker replica in this single-server release.

The supported runtime is Node.js 24. There are no accounts: each installation serves one person on their own computer, who owns every workspace. API tokens are workspace scoped, optionally read-only, and revocable. See `VALIDATION.md` for benchmark scope and unverified deployment combinations.

## Memory

HelixDB, the bundled model, the app, the worker and the executor share Docker's memory. Compose gives each service a budget and a place in the order the kernel stops them, so that when memory runs out the model or a single request fails with an error and HelixDB keeps running.

| Service    | Budget in `compose.yaml` | Override in `.env`         | Measured (1.25 M-item workspace)                                         | Stopped first when Docker runs out |
| ---------- | ------------------------ | -------------------------- | ------------------------------------------------------------------------ | ---------------------------------- |
| `ollama`   | 6 GB limit               | `OLLAMA_MEMORY_LIMIT`      | 3.3 GB with `qwen3:4b` loaded                                            | 1st (`oom_score_adj: 500`)         |
| `app`      | No limit                 | —                          | 0.15–0.26 GB; 3.2 GB loading the graph of a 0.5 M-record workspace (#47) | 2nd (`300`, with the executor)     |
| `executor` | 2 GB limit               | —                          | about 70 MB idle                                                         | 2nd (`300`)                        |
| `worker`   | No limit                 | —                          | about 0.55 GB                                                            | 3rd (`200`)                        |
| `helix`    | 2 GB reserved, no limit  | `HELIX_MEMORY_RESERVATION` | 1.8–2.0 GB                                                               | Last (`0`)                         |

- The app and worker have no memory limit on purpose. Node sizes its heap from the container limit, so a 2 GB limit caps the heap near 1.1 GB, and opening the graph explorer on a large workspace then crashes the app ("JavaScript heap out of memory"). Their `oom_score_adj` still puts them ahead of HelixDB when memory runs out.

- `qwen3:4b` needs about 3.3 GB with an 8K context, and more for the 16K and 32K contexts used for long prompts. A prompt that does not fit in `OLLAMA_MEMORY_LIMIT` fails with an error; raise the limit on a machine with room to spare.
- The model is unloaded 30 seconds after each answer instead of Ollama's default five minutes, so its memory comes back between questions. The next question reloads it in a few seconds. Set `MODEL_KEEP_ALIVE` (an Ollama duration such as `5m`, or `-1` to keep it loaded) to change this.
- Give Docker at least 8 GB when you use local AI. Setup warns before Local is chosen on less, and the in-container doctor reports the memory. With less, use Claude or OpenAI, or native Ollama on macOS (`MODEL_ENDPOINT`), which runs outside Docker's memory.
- If the kernel still stops HelixDB, Docker restarts it, and the app says "The database stopped because Docker ran out of memory" instead of "Cannot reach Helix". The app learns this from the kernel's `oom_kill` counter in `/proc/vmstat`, so it needs no access to Docker itself.
