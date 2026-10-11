import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";
import { createServer, type Server } from "node:http";
import { createServer as tcpServer, type Socket } from "node:net";
import { once } from "node:events";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { config } from "../apps/api/src/config.js";
import { seal } from "../apps/api/src/security.js";
import { startWorker } from "../apps/api/src/jobs.js";
import {
  startChangeStreams,
  needsChangeRefresh,
  scheduledRefresh,
} from "../apps/api/src/source-sync.js";
import { FoundryClient } from "../packages/sdk/src/index.js";
const originalExecutor = config.executor;
const executor = createServer(async (req, res) => {
  try {
    const chunks: Buffer[] = [];
    for await (const c of req) chunks.push(c);
    res.setHeader("content-type", "application/json");
    res.end(
      JSON.stringify(
        await nativeExecute(JSON.parse(Buffer.concat(chunks).toString())),
      ),
    );
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
  server?: Server;
  stop?: () => Promise<void>;
  release?: () => void;
  extra?: () => Promise<void>;
}[] = [];
afterEach(async () => {
  for (const x of owned.splice(0)) {
    x.release?.();
    await x.stop?.();
    await x.app.close();
    if (x.server) {
      x.server.closeAllConnections();
      await new Promise<void>((r) => x.server!.close(() => r()));
    }
    await x.extra?.();
    for (const d of ["incoming", "datasets", "results"])
      await rm(resolve(config.dataDir, d, x.scope), {
        recursive: true,
        force: true,
      });
  }
});
async function fixture(hold = false) {
  const store = new MemoryStore(),
    app = await createApp(store),
    origin = await app.listen({ host: "127.0.0.1", port: 0 });
  const scope = (await app.inject("/api/v1/me")).json().workspaces[0].id,
    base = `/api/v1/workspaces/${scope}`;
  const entry = { app, scope } as (typeof owned)[number];
  owned.push(entry);
  let release!: () => void, entered!: () => void;
  const gate = new Promise<void>((r) => (release = r)),
    reading = new Promise<void>((r) => (entered = r));
  entry.release = release;
  const server = createServer(async (req, res) => {
    const offset = Number(
      new URL(req.url!, "http://127.0.0.1").searchParams.get("offset") || 0,
    );
    if (hold && offset === 2) {
      entered();
      await gate;
    }
    res.setHeader("content-type", "application/json");
    res.end(
      JSON.stringify({
        items: Array.from(
          { length: Math.max(0, Math.min(2, 5 - offset)) },
          (_, i) => ({ id: offset + i, note: "Refresh fixture" }),
        ),
      }),
    );
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  entry.server = server;
  const settings = {
    url: `http://127.0.0.1:${(server.address() as any).port}/rows`,
    itemsPath: "items",
    pagination: "offset",
    pageSize: 2,
  };
  const create = async (name: string) => {
    const r = await app.inject({
      method: "POST",
      url: base + "/sources",
      payload: { name, kind: "rest", config: settings },
    });
    expect(r.statusCode).toBe(200);
    return r.json();
  };
  const source = await create("Refresh source");
  const pause = async (paused: boolean) => {
    const r = await app.inject({
      method: "POST",
      url: base + `/sources/${source.id}/refresh`,
      payload: { paused },
    });
    expect(r.statusCode).toBe(200);
    return r.json();
  };
  return {
    store,
    app,
    origin,
    scope,
    base,
    entry,
    source,
    settings,
    create,
    pause,
    reading,
    release,
  };
}
describe("automatic connection refresh controls", () => {
  it("retains an unpaused manual import and active scheduling controls", async () => {
    const { app, store, scope, source, base, entry } = await fixture();
    const job = (
      await app.inject({
        method: "POST",
        url: base + `/sources/${source.id}/sync`,
        payload: {},
      })
    ).json();
    entry.stop = startWorker(store);
    await expect
      .poll(async () => (await store.get(scope, job.id))?.data.status, {
        timeout: 10000,
      })
      .toBe("succeeded");
    expect((await store.list(scope, "dataset"))[0].data.profile.rows).toBe(5);
    expect(
      scheduledRefresh(
        resource(scope, "source", "Active", {
          schedule: "* * * * *",
          nextRun: 0,
        }),
      ),
    ).toBe(true);
    expect(
      needsChangeRefresh(
        resource(scope, "source", "Active CDC", {
          datasetId: "items",
          sync: { mode: "cdc", changes: 3, applied: 1 },
        }),
      ),
    ).toBe(true);
  });
  it("releases a native PostgreSQL replication connection and lease while paused, then reconnects from the saved cursor", async () => {
    const { app, store, scope, base, entry } = await fixture();
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
    entry.extra = async () => {
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
    entry.stop = startChangeStreams(store);
    await expect.poll(() => starts, { timeout: 10000 }).toBe(1);
    await expect
      .poll(
        async () => !!(await store.get(scope, "cdc"))?.data.sync.leaseOwner,
        { timeout: 10000 },
      )
      .toBe(true);
    expect(
      (
        await app.inject({
          method: "POST",
          url: base + "/sources/cdc/refresh",
          payload: { paused: true },
        })
      ).statusCode,
    ).toBe(200);
    await expect.poll(() => sockets.size, { timeout: 15000 }).toBe(0);
    await expect
      .poll(async () => (await store.get(scope, "cdc"))?.data.sync.leaseOwner, {
        timeout: 10000,
      })
      .toBeNull();
    expect((await store.get(scope, "cdc"))?.data.sync.cursor).toBe("0/1");
    expect(
      (
        await app.inject({
          method: "POST",
          url: base + "/sources/cdc/refresh",
          payload: { paused: false },
        })
      ).statusCode,
    ).toBe(200);
    await expect.poll(() => starts, { timeout: 10000 }).toBe(2);
    expect((await store.get(scope, "cdc"))?.data.sync.cursor).toBe("0/1");
  });
  it("updates all connection tables atomically through the SDK without changing unrelated sources or allowing read-only tokens", async () => {
    const { app, store, scope, source, create, origin, base } = await fixture();
    const sibling = await create("Second table"),
      other = await create("Other connection");
    await transaction(store, scope, async (tx) => {
      for (const id of [source.id, sibling.id]) {
        const s = (await tx.get(id))!;
        tx.update(s, {
          ...s.data,
          connectionId: "supported-onboarding-connection",
        });
      }
    });
    const token = (
      await app.inject({
        method: "POST",
        url: base + "/tokens",
        payload: { name: "Fixture SDK", readOnly: false },
      })
    ).json();
    const client = new FoundryClient({
      baseUrl: origin,
      workspaceId: scope,
      token: token.token,
    });
    const paused = await client.setConnectionPaused(source.id, true);
    expect(paused.items.map((s) => s.id).sort()).toEqual(
      [source.id, sibling.id].sort(),
    );
    expect(paused.items.every((s) => s.data.paused)).toBe(true);
    expect((await store.get(scope, other.id))?.data.paused).toBeUndefined();
    const resumed = await client.setConnectionPaused(sibling.id, false);
    expect(
      resumed.items.every(
        (s) => s.data.paused === false && s.data.nextRun <= Date.now(),
      ),
    ).toBe(true);
    const readonly = (
      await app.inject({
        method: "POST",
        url: base + "/tokens",
        payload: { name: "Read only" },
      })
    ).json();
    expect(
      (
        await app.inject({
          method: "POST",
          url: base + `/sources/${source.id}/refresh`,
          headers: { authorization: "Bearer " + readonly.token },
          payload: { paused: true },
        })
      ).statusCode,
    ).toBe(403);
    const foreign = (
      await app.inject({
        method: "POST",
        url: "/api/v1/workspaces",
        payload: { name: "Other scope" },
      })
    ).json();
    expect(
      (
        await app.inject({
          method: "POST",
          url: `/api/v1/workspaces/${foreign.id}/sources/${source.id}/refresh`,
          payload: { paused: true },
        })
      ).statusCode,
    ).toBe(404);
  });
  it("holds queued automatic refreshes, preserves unrelated work, permits manual refresh while paused and resumes due work", async () => {
    const { app, store, scope, source, create, base, pause, entry } =
      await fixture();
    const other = await create("Unrelated");
    await transaction(store, scope, async (tx) =>
      tx.put(
        resource(
          scope,
          "job",
          "Automatic fixture",
          {
            type: "sync",
            sourceId: source.id,
            status: "queued",
            background: true,
          },
          "automatic",
        ),
      ),
    );
    await pause(true);
    entry.stop = startWorker(store);
    const unrelated = (
      await app.inject({
        method: "POST",
        url: base + `/sources/${other.id}/sync`,
        payload: {},
      })
    ).json();
    await expect
      .poll(async () => (await store.get(scope, unrelated.id))?.data.status, {
        timeout: 10000,
      })
      .toBe("succeeded");
    expect((await store.get(scope, "automatic"))?.data.status).toBe("queued");
    const manual = await app.inject({
      method: "POST",
      url: base + `/sources/${source.id}/sync`,
      payload: {},
    });
    expect(manual.json().id).toBe("automatic");
    expect(manual.json().data.background).toBe(false);
    await expect
      .poll(async () => (await store.get(scope, "automatic"))?.data.status, {
        timeout: 10000,
      })
      .toBe("succeeded");
    expect((await store.get(scope, source.id))?.data.paused).toBe(true);
    await pause(false);
    await expect
      .poll(
        async () =>
          (await store.list(scope, "job")).some(
            (j) =>
              j.data.sourceId === source.id &&
              j.data.background &&
              j.data.status === "succeeded",
          ),
        { timeout: 10000 },
      )
      .toBe(true);
  });
  it("lets a running import finish and promote its snapshot when automatic refresh is paused", async () => {
    const { app, store, scope, source, base, pause, entry, reading, release } =
      await fixture(true);
    const queued = (
      await app.inject({
        method: "POST",
        url: base + `/sources/${source.id}/sync`,
        payload: {},
      })
    ).json();
    entry.stop = startWorker(store);
    await reading;
    try {
      await pause(true);
      expect((await store.get(scope, queued.id))?.data.status).toBe("running");
    } finally {
      release();
    }
    await expect
      .poll(async () => (await store.get(scope, queued.id))?.data.status, {
        timeout: 10000,
      })
      .toBe("succeeded");
    expect((await store.list(scope, "dataset"))[0].data.profile.rows).toBe(5);
    expect((await store.get(scope, source.id))?.data.paused).toBe(true);
  });
  it("does not schedule paused snapshots or dirty CDC sources and retains active-source controls", () => {
    const source = resource("a", "source", "Scheduling", {
      schedule: "* * * * *",
      nextRun: 0,
      paused: true,
    });
    expect(scheduledRefresh(source)).toBe(false);
    const active = { ...source, data: { ...source.data, paused: false } };
    expect(scheduledRefresh(active)).toBe(true);
    const cdc = {
      ...source,
      data: {
        ...source.data,
        datasetId: "orders",
        sync: { mode: "cdc", changes: 3, applied: 1 },
      },
    };
    expect(needsChangeRefresh(cdc)).toBe(false);
    expect(
      needsChangeRefresh({ ...cdc, data: { ...cdc.data, paused: false } }),
    ).toBe(true);
  });
});
