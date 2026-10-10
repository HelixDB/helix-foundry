# Operating Helix Foundry

## Installation profiles

`./scripts/setup.sh --compose` starts the local-model profile and downloads the tested Qwen3 4B model. The app is available on `http://localhost:3001`. On Apple Silicon, native Ollama can use Metal: run the data services without `--profile local` and set `MODEL_ENDPOINT=http://host.docker.internal:11434` in `.env`.

Keep `.env`, data volumes, and backups private. The application encrypts connector/provider credentials using `ENCRYPTION_KEY`; losing that key makes those credentials unrecoverable. Do not rotate it by simply replacing the environment value. Helix Foundry has no sign-in, so do not admit other devices. The API refuses a non-loopback `HOST` unless `ALLOW_NETWORK_ACCESS=true`, which Compose sets because it publishes the app on `127.0.0.1` only. Do not expose it to a network through a reverse proxy, tunnel, or port forward. Internal HelixDB, Ollama, and executor ports are not published by Compose.

## Diagnostics and schema migration

`pnpm doctor` checks the database, executor, model endpoint, and local secrets. `/api/health` is a process health check. Startup applies the indexed version-1 metadata schema idempotently; `node --import tsx scripts/migrate.ts` applies it explicitly. Startup rejects database schema versions newer than the application.

Jobs claim leases and periodically renew them. A worker restart reclaims expired leases. While a workspace build is queued or running, including while a restarted worker waits for its lease to expire, scheduled and CDC syncs of the sources whose datasets it reads stay queued, so the build publishes the input versions it validated. Each waits at most 15 minutes from when it was queued, however often the worker restarts. Syncs of other sources run as usual, and syncing a source by hand starts its waiting sync like a manual one. AI stages save definitions and usage after each completed stage; a failed/canceled run can be resumed through Activity or the API. Input changes invalidate cached stages. Failed connector/pipeline jobs retry three times with bounded backoff. A stopped worker does not stop published dashboard queries.

## Backup, restore, and upgrades

Database synchronization prefers native CDC. PostgreSQL needs logical WAL, a replication-capable login, a primary key/usable replica identity, table publication permission, and available replication slots. Foundry creates a publication and slot scoped to each selected table. MySQL needs row-format binary logging, SELECT access, and replication client/slave privileges. Read-only logins still import through the scheduled fallback. Foundry never changes server-wide database configuration or writes business records back to sources.

The capture service runs independently of inference, saves its dirty counter and log position atomically in HelixDB, and acknowledges PostgreSQL only after that durable write. One queued snapshot coalesces multiple commits; its starting counter prevents concurrent changes from being marked applied accidentally. Existing complete snapshots stay available during imports. Reconnects reconcile a new snapshot, including after restoration of old metadata; schema changes still need review. Transient stream failures retry from saved positions, then fall back to scheduled refreshes. This is native log capture with full-snapshot materialization, not row-at-a-time replication: existing two-million-row import and executor limits still apply.

Monitor source-side WAL/binlog disk retention. Configure PostgreSQL's `max_slot_wal_keep_size` and a suitable MySQL binlog expiration policy for the installation; the application does not alter either server setting. Stop capture before manually removing Foundry-owned slots/publications. Keep their names from the source's sync metadata when decommissioning a source or changing its endpoint. Do not drop other applications' replication resources.

1. Run `./scripts/backup.sh PATH`. It stops writers and HelixDB, archives the data volumes, saves the encryption configuration, then restarts services.
2. Copy the backup to private off-host storage.
3. For a replacement installation, use `./scripts/restore.sh PATH --replace`. This replaces that installation's data volumes and `.env`.
4. Set `COMPOSE_PROJECT_NAME` when using a nondefault project name; use the same name throughout backup and restore.
5. Before upgrades, retain the old application commit, lockfile, image, and backup. Rebuild and run diagnostics, sample ingestion, a saved query, and proposal validation before using the installation again. Revert the application plus matching data backup if an upgrade fails.

Model weights are reproducible and excluded from application backups. Parquet versions and staged artifacts are immutable; unsuccessful previews can consume disk. Publication has the executor write each object type's and relationship's rows to a file in `DATA_DIR/results`, and deletes the file once it has read it, so one exists at a time per publication. The files are uncompressed JSON lines, often several times the size of the dataset's Parquet snapshot, and limited by `EXECUTOR_RESULT_LIMIT` (see [deployment limits](#deployment-limits)). A stopped publication leaves its file behind; the worker removes files there more than ten minutes old when it starts and every ten minutes. Backups leave out `results` and the executor's `scratch`, which hold nothing a restored installation needs. There is no automatic artifact garbage collector in this MVP. Remove unreferenced files only during offline maintenance, after checking proposal/run previews and version records.

## Deployment limits

Two DuckDB jobs execute concurrently with a 512 MB per-process memory setting (`DUCKDB_MEMORY`) and a 120-second timeout.

A job that needs more memory spills to `DATA_DIR/scratch`. Foundry sets no limit on that by default. DuckDB then stops a job whose spill files would pass 90% of the free disk space, but that bound applies to each job on its own: two large jobs at once, or one alongside other writes to the data volume, can still fill the disk. Importing a large file loads the whole table and profiles every column, and can spill several times the file's Parquet size. `DUCKDB_SPILL_LIMIT` caps each job's spill files, as a size with a unit such as `20GB` or `20GiB`. A job that needs more fails with DuckDB's "Out of Memory Error", which names the `max_temp_directory_size` setting; raise or remove the limit to import it. Without a limit, the same error means the job's spill files reached 90% of the free disk space. With a limit set, keep twice that much free for spill files, since two jobs can spill at once.

A publication query also writes its result rows, as JSON lines, to the job's scratch directory, then moves them to `DATA_DIR/results` for the worker. They are not spill files, so `DUCKDB_SPILL_LIMIT` does not count them; `EXECUTOR_RESULT_LIMIT` (2 GB by default) caps each one. A larger result fails the publication with "Result exceeds EXECUTOR_RESULT_LIMIT". A relationship whose key matches many records on both sides is the usual cause, but a type with many records can reach it too; raise the limit in `.env` for one.

Nothing limits the rest: the Parquet snapshots that imports and pipelines write to `DATA_DIR/datasets`, staged artifacts, and HelixDB's own volume all grow with your data.

Each job's scratch directory is removed when the job ends, including when it is killed for its time or output limit or because the executor container is stopping. Once it is listening, the executor also removes the job directories that were in `scratch` when it started, which clears spill files left by an out-of-memory kill; an executor that cannot listen, because another one has the port, removes nothing. Nothing else in `scratch` is touched.

In Compose, set `DUCKDB_SPILL_LIMIT` and `EXECUTOR_RESULT_LIMIT` in `.env` and run the start command again (`docker compose --profile local up -d`, or without `--profile local` when the stack runs without it) to apply them; an empty value means the default. `pnpm dev` reads them from `.env` too. The executor container has a 2 GB memory cap and an internal-only network. One worker performs one inference job at a time. Use one worker replica in this single-server release.

The supported runtime is Node.js 24. There are no accounts: each installation serves one person on their own computer, who owns every workspace. API tokens are workspace scoped, optionally read-only, and revocable. See `VALIDATION.md` for benchmark scope and unverified deployment combinations.
