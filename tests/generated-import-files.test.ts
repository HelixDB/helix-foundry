import {
  afterEach,
  beforeAll,
  afterAll,
  describe,
  expect,
  it,
  vi,
} from "vitest";
import { createServer, type Server } from "node:http";
import { once } from "node:events";
import { mkdir, readdir, readFile, writeFile, rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { startWorker } from "../apps/api/src/jobs.js";
import { runSyncJob, runUploadJob } from "../apps/api/src/data-jobs.js";
import { importSource } from "../apps/api/src/connectors.js";
import { config } from "../apps/api/src/config.js";

// Real loopback transport into native DuckDB, with no provider or database
// services. The worker and API use the actual execute/postJson code paths.
const originalExecutor = config.executor;
const executorCalls: any[] = [];
const executorServer = createServer(async (req, res) => {
  try {
    const parts: Buffer[] = [];
    for await (const part of req) parts.push(part);
    const job = JSON.parse(Buffer.concat(parts).toString());
    executorCalls.push(job);
    const result = await nativeExecute(job);
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify(result));
  } catch (e) {
    res.statusCode = 422;
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ error: (e as Error).message }));
  }
});
beforeAll(async () => {
  executorServer.listen(0, "127.0.0.1");
  await once(executorServer, "listening");
  config.executor = `http://127.0.0.1:${(executorServer.address() as any).port}`;
});
afterAll(async () => {
  config.executor = originalExecutor;
  executorServer.closeAllConnections();
  await new Promise<void>((r) => executorServer.close(() => r()));
});

const owned: {
  app: Awaited<ReturnType<typeof createApp>>;
  scope: string;
  server?: Server;
  stop?: () => Promise<void> | void;
}[] = [];
afterEach(async () => {
  vi.unstubAllEnvs();
  for (const x of owned.splice(0)) {
    await x.stop?.();
    await x.app.close();
    if (x.server) {
      x.server.closeAllConnections();
      await new Promise<void>((r) => x.server!.close(() => r()));
    }
    for (const dir of ["incoming", "datasets", "results"])
      await rm(resolve(config.dataDir, dir, x.scope), {
        recursive: true,
        force: true,
      });
  }
});
async function fixture() {
  const store = new MemoryStore(),
    app = await createApp(store);
  const scope = (await app.inject("/api/v1/me")).json().workspaces[0].id;
  const entry = { app, scope } as (typeof owned)[number];
  owned.push(entry);
  const dir = resolve(config.dataDir, "incoming", scope);
  await mkdir(dir, { recursive: true });
  await writeFile(
    resolve(dir, "original-upload.csv"),
    "id,note\n1,keep for retry\n",
  );
  return { store, app, scope, dir, entry, base: `/api/v1/workspaces/${scope}` };
}
async function sourceFixture(failSecond = false) {
  const f = await fixture();
  const server = createServer((req, res) => {
    res.setHeader("content-type", "application/json");
    const offset = Number(
      new URL(req.url!, "http://127.0.0.1").searchParams.get("offset") || 0,
    );
    if (failSecond && offset === 2) {
      res.statusCode = 503;
      res.end(JSON.stringify({ error: "Fixture page unavailable" }));
      return;
    }
    res.end(
      JSON.stringify({
        items: Array.from(
          { length: Math.max(0, Math.min(2, 5 - offset)) },
          (_, i) => ({ id: offset + i, note: "native snapshot" }),
        ),
      }),
    );
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  f.entry.server = server;
  const settings = {
    url: `http://127.0.0.1:${(server.address() as any).port}/rows`,
    itemsPath: "items",
    pagination: "offset",
    pageSize: 2,
  };
  const created = await f.app.inject({
    method: "POST",
    url: f.base + "/sources",
    payload: { name: "Fixture REST", kind: "rest", config: settings },
  });
  expect(created.statusCode).toBe(200);
  return { ...f, settings, source: created.json() };
}
describe("generated import input ownership", () => {
  it("removes row-ingestion inputs after real public API requests while keeping original uploads", async () => {
    const { app, base, dir } = await fixture();
    for (let i = 0; i < 3; i++) {
      const response = await app.inject({
        method: "POST",
        url: base + "/ingest",
        payload: { name: "API rows", rows: [{ id: i, note: "from request" }] },
      });
      expect(response.statusCode).toBe(200);
      const preview = await app.inject({
        method: "POST",
        url: base + `/datasets/${response.json().id}/preview`,
        payload: {},
      });
      expect(preview.json().rows).toEqual([
        { id: String(i), note: "from request" },
      ]);
      expect(await readdir(dir)).toEqual(["original-upload.csv"]);
    }
  });
  it("removes generated row input after a native executor failure and permits a new request", async () => {
    const { app, base, dir } = await fixture();
    vi.stubEnv("DUCKDB_MEMORY", "invalid-size");
    const failed = await app.inject({
      method: "POST",
      url: base + "/ingest",
      payload: { name: "Retry rows", rows: [{ id: 1 }] },
    });
    expect(failed.statusCode).toBe(422);
    expect(await readdir(dir)).toEqual(["original-upload.csv"]);
    vi.unstubAllEnvs();
    expect(
      (
        await app.inject({
          method: "POST",
          url: base + "/ingest",
          payload: { name: "Retry rows", rows: [{ id: 1 }] },
        })
      ).statusCode,
    ).toBe(200);
    expect(await readdir(dir)).toEqual(["original-upload.csv"]);
  });
  it("closes and removes a partial connector file after a real second-page HTTP failure", async () => {
    const { scope, settings, dir } = await sourceFixture(true);
    await expect(importSource(scope, "rest", settings)).rejects.toThrow("503");
    expect(await readdir(dir)).toEqual(["original-upload.csv"]);
    expect(await readFile(resolve(dir, "original-upload.csv"), "utf8")).toBe(
      "id,note\n1,keep for retry\n",
    );
  });
  it("cleans successful public source sync attempts and unchanged retries through the real worker and DuckDB", async () => {
    const { app, base, scope, source, store, entry, dir } =
      await sourceFixture();
    entry.stop = startWorker(store);
    let version: string | undefined;
    for (let i = 0; i < 2; i++) {
      const response = await app.inject({
        method: "POST",
        url: base + `/sources/${source.id}/sync`,
        payload: {},
      });
      expect(response.statusCode).toBe(200);
      await expect
        .poll(
          async () => (await store.get(scope, response.json().id))?.data.status,
          { timeout: 15000 },
        )
        .toBe("succeeded");
      const ds = (await store.list(scope, "dataset"))[0];
      if (version) expect(ds.data.activeVersion).toBe(version);
      version = ds.data.activeVersion;
      const preview = await app.inject({
        method: "POST",
        url: base + `/datasets/${ds.id}/preview`,
        payload: {},
      });
      expect(preview.json().rows).toHaveLength(5);
      expect(await readdir(dir)).toEqual(["original-upload.csv"]);
    }
  });
  it("cleans a completed connector input when native ingest fails but retains upload inputs for retry", async () => {
    const { scope, source, store, dir } = await sourceFixture();
    const job = resource(scope, "job", "Fixture sync", {
      type: "sync",
      sourceId: source.id,
      status: "running",
    });
    await transaction(store, scope, async (tx) => tx.put(job));
    vi.stubEnv("DUCKDB_MEMORY", "invalid-size");
    await expect(runSyncJob(store, scope, job)).rejects.toThrow();
    expect(await readdir(dir)).toEqual(["original-upload.csv"]);
    vi.unstubAllEnvs();
    const upload = resource(scope, "job", "Original upload", {
      type: "upload",
      file: "original-upload.csv",
      status: "running",
    });
    await transaction(store, scope, async (tx) => tx.put(upload));
    await runUploadJob(store, scope, upload);
    expect(await readFile(resolve(dir, "original-upload.csv"), "utf8")).toBe(
      "id,note\n1,keep for retry\n",
    );
  });
  it("keeps successfully produced connector inputs until their consumer finishes", async () => {
    const { scope, settings, dir } = await sourceFixture();
    const imported = await importSource(scope, "rest", settings);
    expect(
      (await readFile(resolve(dir, imported.file), "utf8"))
        .trimEnd()
        .split("\n"),
    ).toHaveLength(5);
    expect(await readdir(dir)).toContain(imported.file);
  });
});
