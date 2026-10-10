import { createHash, randomUUID } from "node:crypto";
import pg from "pg";
import mysql from "mysql2";
import ZongJi from "@vlasky/zongji";
import {
  LogicalReplicationService,
  PgoutputPlugin,
} from "pg-logical-replication";
import type { Pgoutput } from "pg-logical-replication";
import { CronExpressionParser } from "cron-parser";
import type { Resource } from "../../../packages/shared/src/index.js";
import { now, transaction, type Store } from "./store.js";
import { postgresPoolConfig } from "./postgres-config.js";
import { unseal } from "./security.js";

const FALLBACK = "*/5 * * * *";
// Hourly keeps full SaaS API snapshots well within their rate limits.
// PostHog re-reads recent event history, so it refreshes less often.
const fallback = (kind: string) =>
  kind === "posthog"
    ? "0 */6 * * *"
    : kind === "stripe" || kind === "workos"
      ? "0 * * * *"
      : FALLBACK;
const quote = (s: string) => '"' + s.replaceAll('"', '""') + '"';
const hash = (s: string) => createHash("sha256").update(s).digest("hex");
export const databaseSource = (kind: string) =>
  kind === "postgres" || kind === "mysql";
const database = databaseSource;
const nextRun = (schedule: string) =>
  CronExpressionParser.parse(schedule, { tz: "UTC" }).next().getTime();
const mysqlConfig = (c: Record<string, any>) => ({
  host: c.host,
  port: Number(c.port || 3306),
  user: c.user,
  password: c.password,
  database: c.database,
  connectTimeout: 10000,
  supportBigNumbers: true,
  bigNumberStrings: true,
  dateStrings: true,
  ...(c.tls
    ? { ssl: { rejectUnauthorized: true, ...(c.ca ? { ca: c.ca } : {}) } }
    : {}),
});

// Legacy schedules remain a fallback; they never override a working change stream.
export function sourceSyncDefaults(kind: string, schedule?: string | null) {
  return {
    schedule: schedule || null,
    nextRun: database(kind) ? null : nextRun(schedule || fallback(kind)),
    sync: database(kind)
      ? { mode: "auto", status: "checking" }
      : {
          mode: "snapshot",
          status: "ready",
          reason: "This source does not expose a database change stream.",
        },
  };
}

export function scheduledRefresh(source: Resource) {
  if (source.data.sync?.mode === "cdc" || source.data.sync?.mode === "auto")
    return false;
  return (
    !!(source.data.schedule || source.data.sync?.mode === "snapshot") &&
    typeof source.data.nextRun === "number" &&
    Number.isFinite(source.data.nextRun) &&
    source.data.nextRun <= Date.now()
  );
}
export function nextRefresh(source: Resource) {
  return nextRun(source.data.schedule || fallback(source.data.kind));
}

export async function prepareSourceSync(
  store: Store,
  scope: string,
  source: Resource,
) {
  if (!database(source.data.kind)) return source;
  if (
    source.data.sync?.mode === "snapshot" &&
    source.data.sync.recheckAt > Date.now()
  )
    return source;
  // A fallback is retried on the next refresh, so enabling replication needs no re-creation.
  if (source.data.sync?.mode === "cdc") return source;
  const c = unseal(source.data.secret),
    name =
      "hf_" +
      hash(scope + ":" + source.id + ":" + source.data.secret).slice(0, 36);
  let sync: Record<string, any>;
  if (source.data.kind === "postgres") {
    const client = new pg.Client({
      ...postgresPoolConfig(c),
      statement_timeout: 10000,
    });
    let createdSlot = false,
      createdPublication = false;
    try {
      await client.connect();
      const capability = (
        await client.query(
          "SELECT current_setting('wal_level') AS wal, rolreplication OR rolsuper AS replication FROM pg_roles WHERE rolname = current_user",
        )
      ).rows[0];
      if (capability?.wal !== "logical") throw new Error("logical");
      if (!capability.replication) throw new Error("replication");
      const keys = await client.query(
        "SELECT a.attname FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey) JOIN pg_class t ON t.oid=i.indrelid WHERE i.indrelid = $1::regclass AND i.indisprimary AND t.relreplident <> 'n'",
        [quote(c.schema || "public") + "." + quote(c.table)],
      );
      if (!keys.rowCount) throw new Error("primary key");
      const existingPub = (
        await client.query("SELECT 1 FROM pg_publication WHERE pubname=$1", [
          name,
        ])
      ).rowCount;
      if (!existingPub) {
        await client.query(
          `CREATE PUBLICATION ${quote(name)} FOR TABLE ${quote(c.schema || "public")}.${quote(c.table)}`,
        );
        createdPublication = true;
      }
      const tables = (
        await client.query(
          "SELECT schemaname, tablename FROM pg_publication_tables WHERE pubname=$1",
          [name],
        )
      ).rows;
      if (
        tables.length !== 1 ||
        tables[0].schemaname !== (c.schema || "public") ||
        tables[0].tablename !== c.table
      )
        throw new Error("publication scope");
      let slot = (
        await client.query(
          "SELECT confirmed_flush_lsn AS lsn, plugin, database, active FROM pg_replication_slots WHERE slot_name=$1",
          [name],
        )
      ).rows[0];
      if (!slot) {
        slot = (
          await client.query(
            "SELECT lsn FROM pg_create_logical_replication_slot($1, 'pgoutput')",
            [name],
          )
        ).rows[0];
        createdSlot = true;
      } else if (
        slot.plugin !== "pgoutput" ||
        slot.database !== postgresPoolConfig(c).database ||
        slot.active
      )
        throw new Error("slot unavailable");
      sync = {
        mode: "cdc",
        status: "starting",
        slot: name,
        publication: name,
        cursor: slot.lsn,
        changes: 0,
        applied: 0,
        reason: null,
      };
    } catch (error) {
      if (createdSlot)
        await client
          .query("SELECT pg_drop_replication_slot($1)", [name])
          .catch(() => {});
      if (createdPublication)
        await client.query(`DROP PUBLICATION ${quote(name)}`).catch(() => {});
      const message = error instanceof Error ? error.message : "";
      const reason =
        message === "logical"
          ? "Logical replication is not enabled on this PostgreSQL server."
          : message === "primary key"
            ? "This table needs a primary key for PostgreSQL change capture."
            : "PostgreSQL replication access is unavailable. Check replication privileges and permission to publish this table.";
      sync = {
        mode: "snapshot",
        status: "ready",
        reason,
        recheckAt: Date.now() + 300000,
      };
    } finally {
      await client.end().catch(() => {});
    }
  } else {
    const client = mysql.createConnection(mysqlConfig(c)).promise();
    try {
      const [variables]: any = await client.query(
        "SELECT @@log_bin AS enabled, @@binlog_format AS format, @@server_uuid AS server",
      );
      if (!variables[0].enabled || variables[0].format !== "ROW")
        throw new Error("row logging");
      let status: any;
      try {
        [status] = await client.query("SHOW BINARY LOG STATUS");
      } catch (e: any) {
        if (e.code !== "ER_PARSE_ERROR") throw e;
        [status] = await client.query("SHOW MASTER STATUS");
      }
      if (!status[0]?.File) throw new Error("log unavailable");
      sync = {
        mode: "cdc",
        status: "starting",
        server: variables[0].server,
        cursor: { file: status[0].File, position: Number(status[0].Position) },
        changes: 0,
        applied: 0,
        reason: null,
      };
    } catch (error) {
      sync = {
        mode: "snapshot",
        status: "ready",
        recheckAt: Date.now() + 300000,
        reason:
          error instanceof Error && error.message === "row logging"
            ? "Row-based binary logging is not enabled on this MySQL server."
            : "MySQL replication access is unavailable. Check replication client and replication slave privileges.",
      };
    } finally {
      await client.end().catch(() => {});
    }
  }
  return transaction(store, scope, async (tx) => {
    const current = await tx.get(source.id);
    if (!current || current.data.secret !== source.data.secret)
      throw new Error("Connection changed during setup");
    return tx.update(current, {
      ...current.data,
      sync,
      nextRun: sync.mode === "snapshot" ? nextRefresh(current) : null,
    });
  });
}

export function needsChangeRefresh(source: Resource) {
  const sync = source.data.sync;
  return (
    !!source.data.datasetId &&
    sync?.mode === "cdc" &&
    Number(sync.changes || 0) > Number(sync.applied || 0)
  );
}

// The durable dirty counter and log position are committed together. A snapshot consumes
// only the counter captured before its transaction starts; changes during import remain dirty.
export async function checkpointSource(
  store: Store,
  scope: string,
  id: string,
  owner: string,
  cursor: unknown,
  changed: boolean,
) {
  return transaction(store, scope, async (tx) => {
    const s = await tx.get(id);
    if (
      !s ||
      s.data.sync?.leaseOwner !== owner ||
      s.data.sync.mode !== "cdc" ||
      s.data.sync.leaseUntil < Date.now()
    )
      throw new Error("Change stream lease expired");
    tx.update(s, {
      ...s.data,
      sync: {
        ...s.data.sync,
        cursor,
        status: "live",
        reason: null,
        failures: 0,
        lastEvent: now(),
        changes: Number(s.data.sync.changes || 0) + Number(changed),
        leaseUntil: Date.now() + 30000,
      },
    });
  });
}

type Stream = {
  stop: () => Promise<void>;
  secret: string;
  scope: string;
  id: string;
  owner: string;
};
export function startChangeStreams(store: Store) {
  const streams = new Map<string, Stream>();
  const owner = randomUUID();
  let stopped = false,
    busy = false;
  const release = async (stream: Stream) => {
    streams.delete(stream.scope + ":" + stream.id);
    await stream.stop().catch(() => {});
    await transaction(store, stream.scope, async (tx) => {
      const s = await tx.get(stream.id);
      if (s?.data.sync?.leaseOwner === owner)
        tx.update(s, {
          ...s.data,
          sync: { ...s.data.sync, leaseOwner: null, leaseUntil: 0 },
        });
    });
  };
  const open = async (scope: string, source: Resource) => {
    const id = source.id,
      key = scope + ":" + id,
      c = unseal(source.data.secret);
    let closed = false;
    const fail = async () => {
      if (closed) return;
      closed = true;
      const stream = streams.get(key);
      if (stream) {
        streams.delete(key);
        await stream.stop().catch(() => {});
      }
      const fallback = Number(source.data.sync.failures || 0) + 1 >= 3;
      if (fallback && source.data.kind === "postgres") {
        const client = new pg.Client({
          ...postgresPoolConfig(c),
          statement_timeout: 10000,
        });
        try {
          await client.connect();
          await client.query(
            "SELECT pg_drop_replication_slot($1) FROM pg_replication_slots WHERE slot_name=$1 AND NOT active",
            [source.data.sync.slot],
          );
          await client.query(
            `DROP PUBLICATION IF EXISTS ${quote(source.data.sync.publication)}`,
          );
        } catch {
          /* Keep the owned names for cleanup/reuse on the next connection attempt. */
        } finally {
          await client.end().catch(() => {});
        }
      }
      await transaction(store, scope, async (tx) => {
        const s = await tx.get(id);
        if (!s || s.data.sync?.leaseOwner !== owner) return;
        const failures = (s.data.sync.failures || 0) + 1;
        tx.update(s, {
          ...s.data,
          nextRun: fallback ? Date.now() : null,
          sync: {
            ...s.data.sync,
            mode: fallback ? "snapshot" : "cdc",
            status: fallback ? "ready" : "retrying",
            reason: fallback
              ? "The database change stream is unavailable. Using scheduled refreshes and retrying live sync automatically."
              : "Live sync was interrupted. Reconnecting from the saved position.",
            failures,
            recheckAt: fallback ? Date.now() + 300000 : null,
            retryAt: Date.now() + Math.min(60000, 1000 * 2 ** failures),
            leaseOwner: null,
            leaseUntil: 0,
          },
        });
      });
    };
    if (source.data.kind === "postgres") {
      // The library's timed acknowledgements use the received LSN, even with auto=false.
      // Disable them: only the last durably stored cursor may ever be acknowledged.
      const service = new LogicalReplicationService(postgresPoolConfig(c), {
        acknowledge: { auto: false, timeoutSeconds: 0 },
        flowControl: { enabled: true },
      });
      let changed = false;
      let durableCursor = source.data.sync.cursor;
      streams.set(key, {
        scope,
        id,
        owner,
        secret: source.data.secret,
        stop: async () => {
          closed = true;
          await service.stop();
        },
      });
      service.on("error", () => void fail().catch(() => {}));
      service.on("start", async () => {
        // Reconcile once on reconnect/restore: the server may have advanced its slot
        // beyond a restored metadata backup. This also catches offline schema changes.
        if (!closed)
          await checkpointSource(
            store,
            scope,
            id,
            owner,
            durableCursor,
            true,
          ).catch(fail);
      });
      service.on("heartbeat", async (_lsn, _time, respond) => {
        if (respond && !closed)
          await service.acknowledge(durableCursor).catch(fail);
      });
      service.on("data", async (_lsn: string, message: Pgoutput.Message) => {
        if (closed) return;
        try {
          if (["insert", "update", "delete", "truncate"].includes(message.tag))
            changed = true;
          if (message.tag === "commit" && message.commitEndLsn) {
            await checkpointSource(
              store,
              scope,
              id,
              owner,
              message.commitEndLsn,
              changed,
            );
            durableCursor = message.commitEndLsn;
            changed = false;
            await service.acknowledge(message.commitEndLsn);
          }
        } catch {
          await fail();
        }
      });
      void service
        .subscribe(
          new PgoutputPlugin({
            protoVersion: 1,
            publicationNames: [source.data.sync.publication],
          }),
          source.data.sync.slot,
          source.data.sync.cursor,
        )
        .catch(fail)
        .catch(() => {});
    } else {
      const zongji = new ZongJi(mysqlConfig(c));
      let file = source.data.sync.cursor.file,
        changed = false;
      let chain = Promise.resolve();
      streams.set(key, {
        scope,
        id,
        owner,
        secret: source.data.secret,
        stop: async () => {
          closed = true;
          zongji.stop();
        },
      });
      zongji.on("error", () => void fail().catch(() => {}));
      zongji.on("ready", () => {
        chain = chain
          .then(async () => {
            if (!closed)
              await checkpointSource(
                store,
                scope,
                id,
                owner,
                source.data.sync.cursor,
                true,
              );
          })
          .catch(fail)
          .catch(() => {});
      });
      zongji.on("binlog", (event: any) => {
        if (closed) return;
        const kind = event.getEventName();
        if (kind === "rotate") file = event.binlogName;
        if (
          [
            "writerows",
            "updaterows",
            "deleterows",
            "partialupdaterows",
            "transactionpayload",
          ].includes(kind)
        )
          changed = true;
        const statement = kind === "query" ? String(event.query).trim() : "";
        const ddl =
          /^(ALTER|CREATE|DROP|RENAME|TRUNCATE)\b/i.test(statement) &&
          event.schema === c.database;
        if (ddl) changed = true;
        if (kind === "xid" || ddl || /^COMMIT\b/i.test(statement)) {
          const cursor = { file, position: event.nextPosition },
            dirty = changed;
          changed = false;
          zongji.connection?.pause();
          chain = chain
            .then(async () => {
              if (!closed)
                await checkpointSource(store, scope, id, owner, cursor, dirty);
            })
            .catch(fail)
            .catch(() => {})
            .finally(() => {
              if (!closed) zongji.connection?.resume();
            });
        }
      });
      zongji.start({
        serverId: parseInt(hash(scope + id).slice(0, 7), 16) + 1,
        filename: file,
        position: source.data.sync.cursor.position,
        includeSchema: { [c.database]: [c.table] },
        includeEvents: [
          "rotate",
          "tablemap",
          "writerows",
          "updaterows",
          "deleterows",
          "xid",
          "query",
          "partialupdaterows",
          "transactionpayload",
        ],
      });
    }
  };
  const tick = async () => {
    if (stopped || busy) return;
    busy = true;
    try {
      const present = new Set<string>();
      for (const w of await store.list("global", "workspaceRef")) {
        for (const source of await store.list(w.id, "source")) {
          const key = w.id + ":" + source.id;
          present.add(key);
          const stream = streams.get(key);
          if (
            stream &&
            (source.data.sync?.mode !== "cdc" ||
              source.data.secret !== stream.secret ||
              source.data.sync.leaseOwner !== owner)
          ) {
            await release(stream);
            continue;
          }
          if (
            source.data.sync?.mode !== "cdc" ||
            source.data.sync.retryAt > Date.now()
          )
            continue;
          const claimed = await transaction(store, w.id, async (tx) => {
            const s = await tx.get(source.id);
            if (
              !s ||
              s.data.sync?.mode !== "cdc" ||
              (s.data.sync.leaseOwner !== owner &&
                s.data.sync.leaseUntil > Date.now())
            )
              return false;
            tx.update(s, {
              ...s.data,
              sync: {
                ...s.data.sync,
                leaseOwner: owner,
                leaseUntil: Date.now() + 30000,
              },
            });
            return true;
          });
          if (!claimed) {
            if (stream) await release(stream);
            continue;
          }
          if (!stream && !stopped) await open(w.id, source);
        }
      }
      for (const [key, stream] of streams)
        if (!present.has(key)) await release(stream);
    } catch {
      /* leases expire if metadata storage is unavailable; never advance a log position */
    } finally {
      busy = false;
    }
  };
  const timer = setInterval(() => void tick(), 5000);
  void tick();
  return async () => {
    stopped = true;
    clearInterval(timer);
    while (busy) await new Promise((r) => setTimeout(r, 10));
    await Promise.all([...streams.values()].map(release));
  };
}
