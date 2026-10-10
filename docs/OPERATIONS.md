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

Model weights are reproducible and excluded from application backups. Parquet versions and staged artifacts are immutable; unsuccessful previews can consume disk. Publication has the executor write each object type's and relationship's rows to `DATA_DIR/results` as JSON lines, about the size of the table, and the reader deletes each file once it has read it; the worker removes files there older than ten minutes, which a stopped publication leaves behind, when it starts and every ten minutes. There is no automatic artifact garbage collector in this MVP. Remove unreferenced files only during offline maintenance, after checking proposal/run previews and version records.

## Deployment limits

Two DuckDB jobs execute concurrently with a 512 MB per-process memory setting (`DUCKDB_MEMORY`) and a 120-second timeout. A job that needs more memory spills to `DATA_DIR/scratch`, up to `DUCKDB_SPILL_LIMIT` (2 GB by default) per job, so keep twice that free on the data volume. Each job's scratch directory is removed when the job ends, including when it is killed for its time or output limit, and the executor empties `scratch` when it starts, which clears spill files left by a container stop or an out-of-memory kill. In Compose, change both settings in the executor's `environment` in `compose.yaml`. The executor container has a 2 GB memory cap and an internal-only network. One worker performs one inference job at a time. Use one worker replica in this single-server release.

The supported runtime is Node.js 24. There are no accounts: each installation serves one person on their own computer, who owns every workspace. API tokens are workspace scoped, optionally read-only, and revocable. See `VALIDATION.md` for benchmark scope and unverified deployment combinations.
