import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";
import { createServer } from "node:http";
import { createServer as tcpServer, type Socket } from "node:net";
import { seal } from "../apps/api/src/security.js";
import { once } from "node:events";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { CronExpressionParser } from "cron-parser";
import { createApp } from "../apps/api/src/app.js";
import { config } from "../apps/api/src/config.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { startWorker } from "../apps/api/src/jobs.js";
import {
  nextRefresh,
  scheduledRefresh,
  startChangeStreams,
} from "../apps/api/src/source-sync.js";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { FoundryClient } from "../packages/sdk/src/index.js";

const originalExecutor = config.executor;
const executor = createServer(async (req, res) => {
  try {
    const chunks: Buffer[] = [];
    for await (const c of req) chunks.push(c);
    const result = await nativeExecute(
      JSON.parse(Buffer.concat(chunks).toString()),
    );
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify(result));
  } catch (e) {
    res.statusCode = 422;
    res.end(JSON.stringify({ error: (e as Error).message }));
  }
});
beforeAll(async () => {
  executor.listen(0, "127.0.0.1");
  await once(executor, "listening");
  config.executor = `http://127.0.0.1:${(executor.address() as any).port}`;
});
afterAll(async () => {
  config.executor = originalExecutor;
  executor.closeAllConnections();
  await new Promise<void>((r) => executor.close(() => r()));
});
const owned: {
  app: Awaited<ReturnType<typeof createApp>>;
  scope: string;
  server: ReturnType<typeof createServer>;
  stop?: () => Promise<void>;
  extra?: () => Promise<void>;
}[] = [];
afterEach(async () => {
  for (const f of owned.splice(0)) {
    await f.stop?.();
    await f.extra?.();
    await f.app.close();
    f.server.closeAllConnections();
    await new Promise<void>((r) => f.server.close(() => r()));
    for (const kind of ["incoming", "datasets", "results"])
      await rm(resolve(config.dataDir, kind, f.scope), {
        recursive: true,
        force: true,
      });
  }
});
async function fixture() {
  const store = new MemoryStore(),
    app = await createApp(store),
    origin = await app.listen({ host: "127.0.0.1", port: 0 });
  const scope = (await app.inject("/api/v1/me")).json().workspaces[0].id,
    base = `/api/v1/workspaces/${scope}`;
  let reads = 0;
  const server = createServer((req, res) => {
    reads++;
    res.setHeader("content-type", "application/json");
    res.end(
      JSON.stringify([
        { id: 1, amount: 20 },
        { id: 2, amount: 30 },
      ]),
    );
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const owner = { app, scope, server } as (typeof owned)[number];
  owned.push(owner);
  const create = async (name: string, schedule?: string) => {
    const r = await app.inject({
      method: "POST",
      url: base + "/sources",
      payload: {
        name,
        kind: "rest",
        config: {
          url: `http://127.0.0.1:${(server.address() as any).port}/rows`,
        },
        schedule,
      },
    });
    expect(r.statusCode, r.body).toBe(200);
    return r.json();
  };
  const source = await create("Cadence acceptance");
  const schedule = (value: string | null, id = source.id) =>
    app.inject({
      method: "PUT",
      url: base + `/sources/${id}/schedule`,
      payload: { schedule: value },
    });
  const sync = async () => {
    const r = await app.inject({
      method: "POST",
      url: base + `/sources/${source.id}/sync`,
      payload: {},
    });
    expect(r.statusCode, r.body).toBe(200);
    owner.stop ||= startWorker(store);
    await expect
      .poll(async () => (await store.get(scope, r.json().id))?.data.status, {
        timeout: 10000,
      })
      .toBe("succeeded");
    return (await store.list(scope, "dataset"))[0];
  };
  return {
    store,
    app,
    origin,
    scope,
    base,
    owner,
    source,
    schedule,
    create,
    sync,
    reads: () => reads,
  };
}
describe("credential-preserving connection cadence", () => {
  it("retains existing creation schedules and actual manual snapshot imports", async () => {
    const f = await fixture(),
      custom = await f.create("Daily existing schedule", "0 9 * * *");
    expect(custom.data.schedule).toBe("0 9 * * *");
    expect(custom.data.nextRun).toBeGreaterThan(Date.now());
    expect((await f.sync()).data.profile.rows).toBe(2);
  });
  it("changes all connection tables through the SDK while preserving credentials, datasets and unrelated sources", async () => {
    const f = await fixture(),
      sibling = await f.create("Second table"),
      unrelated = await f.create("Other connection");
    const ds = await f.sync();
    await f.owner.stop!();
    f.owner.stop = undefined;
    await transaction(f.store, f.scope, async (tx) => {
      for (const id of [f.source.id, sibling.id]) {
        const s = (await tx.get(id))!;
        tx.update(s, { ...s.data, connectionId: "onboarding-group" });
      }
    });
    const before = await f.store.get(f.scope, f.source.id),
      beforeSibling = await f.store.get(f.scope, sibling.id),
      reads = f.reads();
    const token = (
      await f.app.inject({
        method: "POST",
        url: f.base + "/tokens",
        payload: { name: "Cadence SDK", readOnly: false },
      })
    ).json();
    const sdk = new FoundryClient({
      baseUrl: f.origin,
      workspaceId: f.scope,
      token: token.token,
    });
    const result = await sdk.setConnectionSchedule(sibling.id, "*/15 * * * *");
    expect(result.data.schedule).toBe("*/15 * * * *");
    expect(result.data.secret).toBeUndefined();
    for (const original of [before!, beforeSibling!]) {
      const current = (await f.store.get(f.scope, original.id))!;
      expect(current.data.schedule).toBe("*/15 * * * *");
      expect(current.data.secret).toBe(original.data.secret);
      expect(current.data.sync).toEqual(original.data.sync);
      expect(current.data.nextRun).toBe(
        CronExpressionParser.parse("*/15 * * * *", { tz: "UTC" })
          .next()
          .getTime(),
      );
    }
    expect(
      (await f.store.get(f.scope, unrelated.id))!.data.schedule,
    ).toBeNull();
    expect((await f.store.get(f.scope, ds.id))!.data.activeVersion).toBe(
      ds.data.activeVersion,
    );
    expect(f.reads()).toBe(reads); // Saving does not rediscover or request credentials.
    expect((await f.sync()).data.activeVersion).toBe(ds.data.activeVersion);
  });
  it("resets Automatic to the connector default and rejects invalid cron without mutation", async () => {
    const f = await fixture();
    expect((await f.schedule("0 9 * * 1-5")).statusCode).toBe(200);
    const before = (await f.store.get(f.scope, f.source.id))!;
    const invalid = await f.schedule("not a cron");
    expect(invalid.statusCode, invalid.body).toBe(400);
    expect((await f.store.get(f.scope, f.source.id))!).toEqual(before);
    expect((await f.schedule(null)).statusCode).toBe(200);
    const current = (await f.store.get(f.scope, f.source.id))!;
    expect(current.data.schedule).toBeNull();
    expect(current.data.nextRun).toBe(nextRefresh(current));
  });
  it("uses the saved cadence in the real worker without replacing a connection", async () => {
    const f = await fixture(),
      before = (await f.store.get(f.scope, f.source.id))!;
    const r = await f.schedule("* * * * *");
    expect(r.statusCode, r.body).toBe(200);
    // Every minute is the most frequent cadence: make the saved one due now
    // instead of waiting for the next minute.
    const due = Date.now() - 1;
    await transaction(f.store, f.scope, async (tx) => {
      const s = (await tx.get(f.source.id))!;
      tx.update(s, { ...s.data, nextRun: due });
    });
    f.owner.stop = startWorker(f.store);
    await expect
      .poll(
        async () =>
          (await f.store.list(f.scope, "job")).find(
            (j) => j.data.sourceId === f.source.id && j.data.background,
          )?.data.status,
        { timeout: 10000 },
      )
      .toBe("succeeded");
    const ds = (await f.store.list(f.scope, "dataset"))[0];
    expect(ds.data.profile.rows).toBe(2);
    const current = (await f.store.get(f.scope, f.source.id))!;
    expect(current.id).toBe(before.id);
    expect(current.data.secret).toBe(before.data.secret);
    expect(current.data.schedule).toBe("* * * * *");
    // The worker re-armed the source from the saved cadence: the next whole
    // minute after it queued the refresh.
    expect(current.data.nextRun).toBeGreaterThan(due);
    expect(current.data.nextRun % 60000).toBe(0);
    expect(current.data.nextRun).toBeLessThanOrEqual(Date.now() + 60000);
  });
  it("keeps CDC and capability checks independent of a saved snapshot fallback cadence", async () => {
    const f = await fixture();
    for (const mode of ["cdc", "auto"]) {
      const s = (await f.store.get(f.scope, f.source.id))!;
      const sync = {
        mode,
        status: mode === "cdc" ? "live" : "checking",
        leaseOwner: "existing-capture",
        leaseUntil: Date.now() + 30000,
        cursor: "0/42",
        changes: 12,
        applied: 10,
      };
      await transaction(f.store, f.scope, async (tx) =>
        tx.update(s, { ...s.data, kind: "postgres", sync }),
      );
      const r = await f.schedule("0 9 * * *");
      expect(r.statusCode, r.body).toBe(200);
      const changed = (await f.store.get(f.scope, f.source.id))!;
      expect(changed.data.sync).toEqual(sync);
      expect(changed.data.nextRun).toBeNull();
      expect(scheduledRefresh(changed)).toBe(false);
      // The ordinary fallback transition picks up this saved cadence later.
      expect(
        nextRefresh({
          ...changed,
          data: { ...changed.data, sync: { mode: "snapshot" } },
        }),
      ).toBe(
        CronExpressionParser.parse("0 9 * * *", { tz: "UTC" }).next().getTime(),
      );
    }
  });
  it("requires an owner and confines mutations to the requested workspace", async () => {
    const f = await fixture(),
      token = (
        await f.app.inject({
          method: "POST",
          url: f.base + "/tokens",
          payload: { name: "Read only" },
        })
      ).json();
    expect(
      (
        await f.app.inject({
          method: "PUT",
          url: f.base + `/sources/${f.source.id}/schedule`,
          headers: { authorization: "Bearer " + token.token },
          payload: { schedule: null },
        })
      ).statusCode,
    ).toBe(403);
    expect((await f.schedule(null, "foreign-source")).statusCode).toBe(404);
    const other = (
      await f.app.inject({
        method: "POST",
        url: "/api/v1/workspaces",
        payload: { name: "Foreign cadence" },
      })
    ).json();
    expect(
      (
        await f.app.inject({
          method: "PUT",
          url: `/api/v1/workspaces/${other.id}/sources/${f.source.id}/schedule`,
          payload: { schedule: null },
        })
      ).statusCode,
    ).toBe(404);
  });
  it("keeps the installed PostgreSQL replication client connected when its snapshot fallback schedule changes", async () => {
    const { app, store, scope, base, owner } = await fixture();
    const sockets = new Set<Socket>();
    let starts = 0;
    const frame = (tag: string, payload: Buffer) => {
      const header = Buffer.alloc(5);
      header.write(tag);
      header.writeInt32BE(payload.length + 4, 1);
      return Buffer.concat([header, payload]);
    };
    // A local wire fixture exercises the installed pg/logical-replication client.
    // It accepts startup and START_REPLICATION, but supplies no database changes.
    const server = tcpServer((socket) => {
      sockets.add(socket);
      socket.on("close", () => sockets.delete(socket));
      socket.on("error", () => {});
      let startup = true;
      let pending: Buffer = Buffer.alloc(0);
      socket.on("data", (chunk) => {
        pending = Buffer.concat([pending, chunk]);
        while (pending.length >= (startup ? 4 : 5)) {
          const length = pending.readInt32BE(startup ? 0 : 1);
          if (pending.length < length + (startup ? 0 : 1)) return;
          if (startup) {
            const request = pending.subarray(0, length);
            pending = pending.subarray(length);
            if (request.readInt32BE(4) === 80877103) {
              socket.write("N");
              continue;
            }
            startup = false;
            socket.write(frame("R", Buffer.alloc(4)));
            socket.write(frame("K", Buffer.alloc(8)));
            socket.write(frame("Z", Buffer.from("I")));
          } else {
            const tag = pending.toString("utf8", 0, 1);
            const body = pending.subarray(5, length + 1);
            pending = pending.subarray(length + 1);
            if (tag === "Q") {
              expect(body.toString()).toContain("START_REPLICATION");
              expect(body.toString()).toContain("0/1");
              starts++;
              socket.write(frame("W", Buffer.alloc(3)));
            } else if (tag === "X") socket.end();
          }
        }
      });
    });
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    owner.extra = async () => {
      for (const socket of sockets) socket.destroy();
      await new Promise<void>((r) => server.close(() => r()));
    };
    await transaction(store, scope, async (tx) =>
      tx.put(
        resource(
          scope,
          "source",
          "Native CDC fixture",
          {
            kind: "postgres",
            secret: seal({
              host: "127.0.0.1",
              port: (server.address() as any).port,
              user: "fixture",
              password: "fixture",
              database: "fixture",
              table: "items",
            }),
            datasetId: "cdc-items",
            sync: {
              mode: "cdc",
              slot: "fixture",
              publication: "fixture",
              cursor: "0/1",
              changes: 0,
              applied: 0,
            },
          },
          "cdc",
        ),
      ),
    );
    owner.stop = startChangeStreams(store);
    await expect.poll(() => starts, { timeout: 10000 }).toBe(1);
    await expect
      .poll(
        async () => !!(await store.get(scope, "cdc"))?.data.sync.leaseOwner,
        { timeout: 10000 },
      )
      .toBe(true);
    const before = (await store.get(scope, "cdc"))!;
    const update = await app.inject({
      method: "PUT",
      url: base + "/sources/cdc/schedule",
      payload: { schedule: "0 9 * * *" },
    });
    expect(update.statusCode, update.body).toBe(200);
    const after = (await store.get(scope, "cdc"))!;
    expect(after.data.sync).toEqual(before.data.sync);
    expect(after.data.secret).toBe(before.data.secret);
    expect(after.data.schedule).toBe("0 9 * * *");
    expect(after.data.nextRun).toBeNull();
    expect(sockets.size).toBe(1);
    expect(starts).toBe(1);
    // A subsequent lease tick also keeps the same native connection and cursor.
    await expect
      .poll(async () => (await store.get(scope, "cdc"))!.data.sync.leaseUntil, {
        timeout: 10000,
      })
      .toBeGreaterThan(after.data.sync.leaseUntil);
    expect(sockets.size).toBe(1);
    expect(starts).toBe(1);
    expect((await store.get(scope, "cdc"))!.data.sync.cursor).toBe("0/1");
  });
});

it("changes legacy operation-group cadence atomically without changing secrets, checkpoints, jobs or unrelated imports", async () => {
  const f = await fixture(),
    sibling = await f.create("Legacy cadence sibling"),
    unrelated = await f.create("Separate legacy cadence");
  await transaction(f.store, f.scope, async (tx) => {
    const a = (await tx.get(f.source.id))!,
      b = (await tx.get(sibling.id))!;
    tx.put(
      resource(
        f.scope,
        "operation",
        "Legacy shared import",
        { result: [{ source: { id: a.id } }, { source: { id: b.id } }] },
        "legacy-schedule-operation",
      ),
    );
    tx.put(
      resource(
        f.scope,
        "operation",
        "Other legacy import",
        { result: [{ source: { id: unrelated.id } }] },
        "other-legacy-schedule-operation",
      ),
    );
    for (const s of [a, b])
      tx.update(s, {
        ...s.data,
        paused: true,
        sync: {
          ...s.data.sync,
          cursor: "saved-legacy-cursor",
          leaseOwner: "retained-legacy-lease",
        },
      });
    tx.put(
      resource(
        f.scope,
        "job",
        "Retained legacy sync",
        { type: "sync", status: "queued", sourceId: b.id },
        "legacy-schedule-job",
      ),
    );
  });
  const before = await f.store.list(f.scope, "source"),
    jobs = await f.store.list(f.scope, "job"),
    reads = f.reads();
  const token = (
    await f.app.inject({
      method: "POST",
      url: f.base + "/tokens",
      payload: { name: "Legacy cadence SDK", readOnly: false },
    })
  ).json();
  const sdk = new FoundryClient({
    baseUrl: f.origin,
    workspaceId: f.scope,
    token: token.token,
  });
  await sdk.setConnectionSchedule(sibling.id, "0 8 * * 1-5");
  for (const old of before) {
    const current = (await f.store.get(f.scope, old.id))!;
    if (old.id === unrelated.id) expect(current).toEqual(old);
    else {
      expect(current.data.schedule).toBe("0 8 * * 1-5");
      expect(current.data.secret).toBe(old.data.secret);
      expect(current.data.sync).toEqual(old.data.sync);
      expect(current.data.paused).toBe(true);
      expect(current.data.nextRun).toBe(
        CronExpressionParser.parse("0 8 * * 1-5", { tz: "UTC" })
          .next()
          .getTime(),
      );
    }
  }
  expect(await f.store.list(f.scope, "job")).toEqual(jobs);
  expect(f.reads()).toBe(reads);
});
