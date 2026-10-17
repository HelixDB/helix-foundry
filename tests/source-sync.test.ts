import { it, expect, vi } from "vitest";
import { randomUUID } from "node:crypto";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import pg from "pg";
import mysql from "mysql2/promise";
import { execute as localExecute } from "../services/executor/src/engine.js";
vi.mock("../apps/api/src/executor.js", () => ({
  execute: (job: any) => localExecute(job),
}));
import { config } from "../apps/api/src/config.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { seal } from "../apps/api/src/security.js";
import { startWorker } from "../apps/api/src/jobs.js";
import {
  sourceSyncDefaults,
  prepareSourceSync,
  checkpointSource,
  needsChangeRefresh,
  scheduledRefresh,
} from "../apps/api/src/source-sync.js";

it("prefers CDC over even an expired legacy schedule and fences checkpoint writers by workspace and lease", async () => {
  const store = new MemoryStore(),
    source = resource(
      "a",
      "source",
      "Orders",
      {
        schedule: "* * * * *",
        nextRun: 0,
        datasetId: "orders",
        sync: {
          mode: "cdc",
          changes: 1,
          applied: 0,
          cursor: "0/1",
          leaseOwner: "worker",
          leaseUntil: Date.now() + 10000,
        },
      },
      "source",
    );
  await transaction(store, "a", async (tx) => {
    tx.put(source);
  });
  expect(scheduledRefresh(source)).toBe(false);
  expect(needsChangeRefresh(source)).toBe(true);
  await expect(
    checkpointSource(store, "b", "source", "worker", "0/2", true),
  ).rejects.toThrow();
  await expect(
    checkpointSource(store, "a", "source", "other", "0/2", true),
  ).rejects.toThrow();
  await checkpointSource(store, "a", "source", "worker", "0/2", true);
  expect((await store.get("a", "source"))?.data.sync).toMatchObject({
    cursor: "0/2",
    changes: 2,
    applied: 0,
  });
  expect(sourceSyncDefaults("postgres").sync.mode).toBe("auto");
  expect(sourceSyncDefaults("rest").sync.mode).toBe("snapshot");
});

it("turns a Neo4j source live when the server has CDC, and keeps scheduled refreshes without it", async () => {
  const store = new MemoryStore(),
    scope = "a",
    connection = {
      url: "http://neo4j.test:7474/",
      database: "neo4j",
      user: "neo4j",
      password: "secret",
      table: "Customer",
      schema: "node",
    };
  const make = (id: string) =>
    resource(
      scope,
      "source",
      id,
      { kind: "neo4j", secret: seal(connection), datasetId: id },
      id,
    );
  const withCdc = make("with-cdc"),
    withoutCdc = make("without-cdc");
  await transaction(store, scope, async (tx) => {
    tx.put(withCdc);
    tx.put(withoutCdc);
  });
  // A server that answers db.cdc.current goes live from that identifier.
  vi.stubGlobal(
    "fetch",
    async () => Response.json({ data: { fields: ["id"], values: [["c1"]] } }),
  );
  await prepareSourceSync(store, scope, withCdc);
  expect((await store.get(scope, "with-cdc"))?.data.sync).toMatchObject({
    mode: "cdc",
    cursor: "c1",
    changes: 0,
    applied: 0,
  });
  vi.unstubAllGlobals();
  // One that returns no identifier keeps its scheduled snapshot refreshes.
  vi.stubGlobal(
    "fetch",
    async () => Response.json({ data: { fields: [], values: [] } }),
  );
  await prepareSourceSync(store, scope, withoutCdc);
  expect((await store.get(scope, "without-cdc"))?.data.sync).toMatchObject({
    mode: "snapshot",
    reason: expect.stringContaining("Change Data Capture"),
  });
  vi.unstubAllGlobals();
});

it.skipIf(process.env.TEST_CDC_DATABASES !== "1").each(["postgres", "mysql"])(
  "captures %s commits and resumes while retaining complete snapshots",
  async (kind) => {
    const credentials = {
      host: "127.0.0.1",
      port: kind === "postgres" ? 54877 : 33077,
      user: kind === "postgres" ? "postgres" : "root",
      password: "foundry-test-only",
      database: "foundry_test",
    };
    const db =
      kind === "postgres"
        ? new pg.Client(credentials)
        : await mysql.createConnection(credentials);
    if (kind === "postgres") await (db as pg.Client).connect();
    const sql = (text: string) => (db as any).query(text);
    const scope = "cdc-" + randomUUID(),
      store = new MemoryStore();
    const table = "cdc_" + kind;
    const connection =
      kind === "postgres"
        ? {
            connectionString:
              "postgresql://postgres:foundry-test-only@127.0.0.1:54877/foundry_test?sslmode=disable",
            table,
            schema: "public",
          }
        : { ...credentials, table };
    const source = resource(scope, "source", "Orders", {
      kind,
      secret: seal(connection),
      ...sourceSyncDefaults(kind),
    });
    let stop: () => void = () => {};
    try {
      await sql(`DROP TABLE IF EXISTS ${table}`);
      await sql(
        `CREATE TABLE ${table} (id BIGINT PRIMARY KEY, amount DECIMAL(24,4), label VARCHAR(100), optional VARCHAR(30), happened_at TIMESTAMP)`,
      );
      await sql(
        `INSERT INTO ${table} VALUES (9007199254740993,1234567890123456.7890,'東京 café',NULL,'2026-01-02 03:04:05'),(2,20,'remove',NULL,NULL)`,
      );
      await transaction(store, "global", async (tx) => {
        tx.put(resource("global", "workspaceRef", "Test", {}, scope));
      });
      await transaction(store, scope, async (tx) => {
        tx.put(resource(scope, "workspace", "Test", {}, "workspace"));
        tx.put(source);
        tx.put(
          resource(scope, "job", "Import", {
            type: "sync",
            sourceId: source.id,
            status: "queued",
          }),
        );
      });
      stop = startWorker(store);
      const dataset = async () => {
        const s = await store.get(scope, source.id);
        return s?.data.datasetId
          ? store.get(scope, s.data.datasetId)
          : undefined;
      };
      await expect
        .poll(async () => (await dataset())?.data.profile.rows, {
          timeout: 20000,
        })
        .toBe(2);
      expect((await store.get(scope, source.id))?.data.sync.mode).toBe("cdc");
      // One committed change batch includes insert, update, delete and exact large numeric values.
      await sql("BEGIN");
      await sql(
        `UPDATE ${table} SET label='updated 東京' WHERE id=9007199254740993`,
      );
      await sql(`DELETE FROM ${table} WHERE id=2`);
      await sql(`INSERT INTO ${table} VALUES (3,30,'added',NULL,NULL)`);
      await sql("COMMIT");
      await expect
        .poll(
          async () =>
            (await dataset())?.data.profile.sample
              .map((r: any) => r.label)
              .sort(),
          { timeout: 25000 },
        )
        .toEqual(["added", "updated 東京"]);
      const row = (await dataset())!.data.profile.sample.find(
        (r: any) => r.label === "updated 東京",
      );
      expect(String(row.id)).toBe("9007199254740993");
      expect(String(row.amount)).toBe("1234567890123456.7890");
      expect(row.optional).toBe(null);
      const before = (await store.get(scope, source.id))!.data.sync.cursor;
      stop();
      await new Promise((r) => setTimeout(r, 750));
      await sql(`INSERT INTO ${table} VALUES (4,40,'while stopped',NULL,NULL)`);
      stop = startWorker(store);
      await expect
        .poll(async () => (await dataset())?.data.profile.rows, {
          timeout: 25000,
        })
        .toBe(3);
      expect((await store.get(scope, source.id))!.data.sync.cursor).not.toEqual(
        before,
      );
      // Truncation is a change too: no stale records may remain.
      await sql(`TRUNCATE TABLE ${table}`);
      await expect
        .poll(
          async () =>
            (await dataset())?.data.pendingVersion ||
            (await dataset())?.data.profile.rows,
          { timeout: 25000 },
        )
        .not.toBe(3);
      const ds = (await dataset())!;
      const latest = await store.get(
        scope,
        ds.data.pendingVersion || ds.data.activeVersion,
      );
      expect(latest?.data.profile.rows).toBe(0);
    } finally {
      stop();
      await new Promise((r) => setTimeout(r, 750));
      if (kind === "postgres") {
        const s = await store.get(scope, source.id),
          sync = s?.data.sync;
        if (sync?.slot)
          await (db as pg.Client).query(
            "SELECT pg_drop_replication_slot($1) FROM pg_replication_slots WHERE slot_name=$1 AND NOT active",
            [sync.slot],
          );
        if (sync?.publication)
          await sql(`DROP PUBLICATION IF EXISTS "${sync.publication}"`);
      }
      await sql(`DROP TABLE IF EXISTS ${table}`);
      await db.end();
      await rm(resolve(config.dataDir, "datasets", scope), {
        recursive: true,
        force: true,
      });
      await rm(resolve(config.dataDir, "incoming", scope), {
        recursive: true,
        force: true,
      });
    }
  },
);

it.skipIf(process.env.TEST_CDC_DATABASES !== "1")(
  "uses scheduled fallback when a PostgreSQL table cannot support logical replication",
  async () => {
    const db = new pg.Client({
      host: "127.0.0.1",
      port: 54877,
      user: "postgres",
      password: "foundry-test-only",
      database: "foundry_test",
    });
    await db.connect();
    try {
      await db.query("CREATE TABLE IF NOT EXISTS cdc_no_key (label TEXT)");
      const store = new MemoryStore(),
        scope = randomUUID();
      const source = resource(scope, "source", "No key", {
        kind: "postgres",
        secret: seal({
          connectionString:
            "postgresql://postgres:foundry-test-only@127.0.0.1:54877/foundry_test?sslmode=disable",
          table: "cdc_no_key",
        }),
        ...sourceSyncDefaults("postgres"),
      });
      await transaction(store, scope, async (tx) => {
        tx.put(source);
      });
      const result = await prepareSourceSync(store, scope, source);
      expect(result.data.sync.mode).toBe("snapshot");
      expect(result.data.sync.reason).toContain("primary key");
      expect(result.data.nextRun).toBeGreaterThan(Date.now());
    } finally {
      await db.query("DROP TABLE cdc_no_key");
      await db.end();
    }
  },
);
