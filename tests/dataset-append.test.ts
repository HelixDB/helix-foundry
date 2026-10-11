import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { readdir, rm, writeFile, readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { createApp } from "../apps/api/src/app.js";
import { config } from "../apps/api/src/config.js";
import { MemoryStore } from "../apps/api/src/store.js";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { FoundryClient } from "../packages/sdk/src/index.js";
import { startWorker } from "../apps/api/src/jobs.js";
import { pipelineId } from "../apps/api/src/datasets.js";
import { datasetTable } from "../packages/shared/src/index.js";

const originalExecutor = config.executor;
let gate:
  | { reached: number; release: Promise<void>; ready: () => void }
  | undefined;
const executor = createServer(async (req, res) => {
  try {
    const chunks: Buffer[] = [];
    for await (const c of req) chunks.push(c);
    const job = JSON.parse(Buffer.concat(chunks).toString());
    if (gate && job.mode === "query" && job.persist) {
      gate.reached++;
      if (gate.reached === 2) gate.ready();
      await gate.release;
    }
    const result = await nativeExecute(job);
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
  stop?: () => Promise<void>;
  server?: ReturnType<typeof createServer>;
}[] = [];
afterEach(async () => {
  gate = undefined;
  for (const f of owned.splice(0)) {
    await f.stop?.();
    if (f.server) {
      f.server.closeAllConnections();
      await new Promise<void>((r) => f.server!.close(() => r()));
    }
    await f.app.close();
    for (const d of ["incoming", "datasets", "results"])
      await rm(resolve(config.dataDir, d, f.scope), {
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
  owned.push({ app, scope });
  const ingest = async (rows: any[], datasetId?: string) => {
    const r = await app.inject({
      method: "POST",
      url: base + "/ingest",
      payload: { name: "Append acceptance", rows, datasetId },
    });
    expect(r.statusCode, r.body).toBe(200);
    return r.json();
  };
  const ds = await ingest([
    { id: 1, amount: 10, region: "North" },
    { id: 2, amount: 20, region: "South" },
  ]);
  const append = (
    rows: any[],
    expectedVersion = ds.data.activeVersion,
    id = ds.id,
  ) =>
    app.inject({
      method: "POST",
      url: base + `/datasets/${id}/append`,
      payload: { expectedVersion, rows },
    });
  const preview = async (id = ds.id) => {
    const r = await app.inject({
      method: "POST",
      url: base + `/datasets/${id}/preview`,
      payload: { limit: 500 },
    });
    expect(r.statusCode, r.body).toBe(200);
    return r.json();
  };
  const files = async (kind: string) =>
    (await readdir(resolve(config.dataDir, kind, scope))).sort();
  return {
    store,
    app,
    scope,
    base,
    origin,
    ds,
    ingest,
    append,
    preview,
    files,
  };
}

describe("immutable append ingestion", () => {
  it("retains the existing replacement workflow and unchanged snapshot optimization", async () => {
    const f = await fixture();
    const replacement = await f.ingest(
      [{ id: 9, amount: 90, region: "West" }],
      f.ds.id,
    );
    expect((await f.preview()).rows).toEqual([
      { id: "9", amount: "90", region: "West" },
    ]);
    const unchanged = await f.ingest(
      [{ id: 9, amount: 90, region: "West" }],
      f.ds.id,
    );
    expect(unchanged.data.activeVersion).toBe(replacement.data.activeVersion);
    expect(unchanged.data.versions).toHaveLength(2);
  });
  it("appends SDK batches without losing original rows or mutating historical snapshots", async () => {
    const f = await fixture();
    const before = await readFile(
      resolve(
        config.dataDir,
        "datasets",
        f.scope,
        f.ds.data.activeVersion + ".parquet",
      ),
    );
    const token = (
      await f.app.inject({
        method: "POST",
        url: f.base + "/tokens",
        payload: { name: "Batch writer", readOnly: false },
      })
    ).json();
    const sdk = new FoundryClient({
      baseUrl: f.origin,
      workspaceId: f.scope,
      token: token.token,
    });
    const first = await sdk.appendDatasetRows(
      f.ds.id,
      f.ds.data.activeVersion,
      [{ id: 3, amount: 30, region: "East" }],
    );
    const next = await sdk.appendDatasetRows(
      f.ds.id,
      first.data.activeVersion,
      [{ id: 4, amount: 40 }],
    );
    expect(next.name).toBe(f.ds.name);
    expect(next.data.profile.rows).toBe(4);
    expect(next.data.pendingVersion).toBeNull();
    expect(next.data.versions).toEqual([
      next.data.activeVersion,
      first.data.activeVersion,
      f.ds.data.activeVersion,
    ]);
    expect((await f.preview()).rows).toEqual([
      { id: "1", amount: "10", region: "North" },
      { id: "2", amount: "20", region: "South" },
      { id: "3", amount: "30", region: "East" },
      { id: "4", amount: "40", region: null },
    ]);
    expect(
      await readFile(
        resolve(
          config.dataDir,
          "datasets",
          f.scope,
          f.ds.data.activeVersion + ".parquet",
        ),
      ),
    ).toEqual(before);
    expect(await f.files("datasets")).toHaveLength(3);
    // Old replacement ingestion creates its own durable input; append leaves no new input behind.
    expect(await f.files("incoming")).toHaveLength(1);
    const query = await f.app.inject({
      method: "POST",
      url: f.base + "/query",
      payload: {
        inputs: [f.ds.id],
        sql: `SELECT sum(amount) amount FROM "${datasetTable(f.ds.id)}"`,
      },
    });
    expect(query.statusCode).toBe(200);
    expect(query.json().rows[0].amount).toBe("100");
  });
  it("preserves exact existing BIGINT values when incoming numbers infer a different type", async () => {
    const f = await fixture();
    const boundary = "foundry-append-fixture",
      body = Buffer.from(
        `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="large.csv"\r\nContent-Type: text/csv\r\n\r\namount,region\n9007199254740993,North\n\r\n--${boundary}--\r\n`,
      );
    const uploaded = await f.app.inject({
      method: "POST",
      url: f.base + "/upload",
      headers: { "content-type": `multipart/form-data; boundary=${boundary}` },
      payload: body,
    });
    expect(uploaded.statusCode, uploaded.body).toBe(200);
    const ds = uploaded.json();
    expect(
      ds.data.profile.columns.find((c: any) => c.name === "amount").type,
    ).toBe("BIGINT");
    const appended = await f.append(
      [{ amount: 1.5, region: "South" }],
      ds.data.activeVersion,
      ds.id,
    );
    expect(appended.statusCode, appended.body).toBe(200);
    expect((await f.preview(ds.id)).rows[0].amount).toBe("9007199254740993");
    expect(
      appended.json().data.profile.columns.map((c: any) => [c.name, c.type]),
    ).toEqual(ds.data.profile.columns.map((c: any) => [c.name, c.type]));
  });
  it("rejects stale versions, schema additions and incompatible casts while preserving files and metadata", async () => {
    const f = await fixture(),
      before = await f.files("datasets"),
      inputs = await f.files("incoming");
    const keep = resolve(config.dataDir, "incoming", f.scope, "unrelated.csv");
    await writeFile(keep, "owned elsewhere");
    for (const [rows, version, status] of [
      [[{ id: 3 }], "stale-version", 409],
      [[{ id: 3, unknown: 1 }], f.ds.data.activeVersion, 400],
      [
        [{ id: "cannot-cast", amount: 30, region: "East" }],
        f.ds.data.activeVersion,
        422,
      ],
    ] as const) {
      const r = await f.append([...rows], version);
      expect(r.statusCode, r.body).toBe(status);
      expect(await f.files("datasets")).toEqual(before);
      expect(await f.files("incoming")).toEqual(
        [...inputs, "unrelated.csv"].sort(),
      );
      expect((await f.store.get(f.scope, f.ds.id))!.data).toEqual(f.ds.data);
    }
    expect(await readFile(keep, "utf8")).toBe("owned elsewhere");
  });
  it("lets one concurrent public append win and asks the other caller to reload instead of losing rows", async () => {
    const f = await fixture();
    let release!: () => void, ready!: () => void;
    const reached = new Promise<void>((r) => (ready = r));
    gate = {
      reached: 0,
      ready,
      release: new Promise<void>((r) => (release = r)),
    };
    const first = f.append([{ id: 3, amount: 30, region: "East" }]),
      second = f.append([{ id: 4, amount: 40, region: "West" }]);
    try {
      await Promise.race([
        reached,
        new Promise((_, reject) =>
          setTimeout(
            () =>
              reject(
                new Error(
                  "Both native append requests did not reach the fixture",
                ),
              ),
            3000,
          ),
        ),
      ]);
      release();
      const results = await Promise.all([first, second]);
      expect(results.map((r) => r.statusCode).sort()).toEqual([200, 409]);
      const current = (await f.store.get(f.scope, f.ds.id))!;
      expect(current.data.profile.rows).toBe(3);
      expect(current.data.versions).toHaveLength(2);
      expect((await f.preview()).rows.slice(0, 2)).toEqual([
        { id: "1", amount: "10", region: "North" },
        { id: "2", amount: "20", region: "South" },
      ]);
      expect(await f.files("datasets")).toHaveLength(2);
      expect(await f.files("incoming")).toHaveLength(1);
      const loser = results.find((r) => r.statusCode === 409)!;
      expect(loser.json().error).toContain("reload");
    } finally {
      release();
      gate = undefined;
      await Promise.allSettled([first, second]);
    }
  });
  it("enforces write access and excludes automatic sources, published outputs and unresolved schema reviews", async () => {
    const f = await fixture();
    const token = (
      await f.app.inject({
        method: "POST",
        url: f.base + "/tokens",
        payload: { name: "Reader" },
      })
    ).json();
    expect(
      (
        await f.app.inject({
          method: "POST",
          url: f.base + `/datasets/${f.ds.id}/append`,
          headers: { authorization: "Bearer " + token.token },
          payload: {
            expectedVersion: f.ds.data.activeVersion,
            rows: [{ id: 3 }],
          },
        })
      ).statusCode,
    ).toBe(403);
    expect(
      (await f.append([{ id: 3 }], f.ds.data.activeVersion, "foreign-dataset"))
        .statusCode,
    ).toBe(404);
    const staged = await f.ingest(
      [{ id: 3, amount: 30, region: "North", extra: 1 }],
      f.ds.id,
    );
    expect(staged.data.pendingVersion).toBeTruthy();
    expect((await f.append([{ id: 3 }])).statusCode).toBe(409);

    const name = "Append output",
      draft = await f.app.inject({
        method: "POST",
        url: f.base + "/pipelines/draft",
        payload: {
          name,
          inputs: [f.ds.id],
          sql: `SELECT * FROM "${datasetTable(f.ds.id)}"`,
        },
      });
    expect(draft.statusCode, draft.body).toBe(200);
    const proposal = draft.json();
    const published = await f.app.inject({
      method: "POST",
      url: f.base + `/proposals/${proposal.id}/publish`,
      payload: { hash: proposal.data.hash },
    });
    expect(published.statusCode, published.body).toBe(200);
    const derived = (await f.store.get(f.scope, pipelineId(name)))!;
    expect(
      (await f.append([{ id: 3 }], derived.data.activeVersion, derived.id))
        .statusCode,
    ).toBe(400);
  });
  it("keeps native REST source snapshots under their automatic refresh lifecycle", async () => {
    const f = await fixture(),
      owner = owned.find((x) => x.scope === f.scope)!;
    const server = createServer((req, res) => {
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify([{ id: 1, amount: 10, region: "North" }]));
    });
    owner.server = server;
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    const source = (
      await f.app.inject({
        method: "POST",
        url: f.base + "/sources",
        payload: {
          name: "Automatic rows",
          kind: "rest",
          config: {
            url: `http://127.0.0.1:${(server.address() as any).port}/rows`,
          },
        },
      })
    ).json();
    const sync = await f.app.inject({
      method: "POST",
      url: f.base + `/sources/${source.id}/sync`,
      payload: {},
    });
    expect(sync.statusCode, sync.body).toBe(200);
    owner.stop = startWorker(f.store);
    await expect
      .poll(
        async () => (await f.store.get(f.scope, sync.json().id))?.data.status,
        { timeout: 10000 },
      )
      .toBe("succeeded");
    const connected = (await f.store.list(f.scope, "dataset")).find(
      (d) => d.data.sourceId === source.id,
    )!;
    expect(connected.data.profile.rows).toBe(1);
    expect(
      (await f.append([{ id: 2 }], connected.data.activeVersion, connected.id))
        .statusCode,
    ).toBe(400);
    expect((await f.store.get(f.scope, connected.id))!.data.activeVersion).toBe(
      connected.data.activeVersion,
    );
  });
  it("grows a dataset beyond one ingestion request through two bounded SDK batches", async () => {
    const f = await fixture(),
      token = (
        await f.app.inject({
          method: "POST",
          url: f.base + "/tokens",
          payload: { name: "Large batch writer", readOnly: false },
        })
      ).json();
    const sdk = new FoundryClient({
      baseUrl: f.origin,
      workspaceId: f.scope,
      token: token.token,
    });
    let current = f.ds;
    for (const offset of [0, 10000]) {
      current = await sdk.appendDatasetRows(
        f.ds.id,
        current.data.activeVersion,
        Array.from({ length: 10000 }, (_, i) => ({
          id: offset + i + 3,
          amount: 1,
          region: "Batch",
        })),
      );
    }
    expect(current.data.profile.rows).toBe(20002);
    expect(current.data.versions).toHaveLength(3);
    const count = await f.app.inject({
      method: "POST",
      url: f.base + "/query",
      payload: {
        inputs: [f.ds.id],
        sql: `SELECT count(*) AS "rows", sum(amount) amount FROM "${datasetTable(f.ds.id)}"`,
      },
    });
    expect(count.statusCode).toBe(200);
    expect(count.json().rows).toEqual([{ rows: "20002", amount: "20030" }]);
    const oversized = await f.append(
      Array.from({ length: 10001 }, () => ({ id: 1 })),
      current.data.activeVersion,
    );
    expect(oversized.statusCode).toBe(400);
    expect((await f.store.get(f.scope, f.ds.id))!.data.activeVersion).toBe(
      current.data.activeVersion,
    );
  });
  it("casts explicit and entirely omitted values to typed NULL without introducing columns", async () => {
    const f = await fixture(),
      r = await f.append([{}, { id: null, amount: null, region: null }]);
    expect(r.statusCode, r.body).toBe(200);
    expect(
      r.json().data.profile.columns.map((c: any) => [c.name, c.type]),
    ).toEqual(f.ds.data.profile.columns.map((c: any) => [c.name, c.type]));
    expect((await f.preview()).rows.slice(2)).toEqual([
      { id: null, amount: null, region: null },
      { id: null, amount: null, region: null },
    ]);
    expect(r.json().data.profile.duplicateRows).toBe(1);
  });
});
