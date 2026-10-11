import { beforeAll, afterAll, afterEach, expect, it } from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { mkdir, writeFile, rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { config } from "../apps/api/src/config.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, transaction } from "../apps/api/src/store.js";
import { ingestRows, ingestFile } from "../apps/api/src/datasets.js";
import { FoundryClient } from "../packages/sdk/src/index.js";

const originalExecutor = config.executor;
let afterQuery: (() => Promise<void>) | undefined;
const calls: any[] = [];
const executor = createServer(async (req, res) => {
  try {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(chunk);
    const job = JSON.parse(Buffer.concat(chunks).toString());
    calls.push(job);
    res.setHeader("content-type", "application/json");
    const result = await nativeExecute(job);
    if (job.mode === "query" && afterQuery) {
      const hook = afterQuery;
      afterQuery = undefined;
      await hook();
    }
    res.end(JSON.stringify(result));
  } catch (error) {
    res.statusCode = 422;
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ error: (error as Error).message }));
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
  await new Promise<void>((done) => executor.close(() => done()));
});
const installations: {
  app: Awaited<ReturnType<typeof createApp>>;
  scopes: string[];
}[] = [];
afterEach(async () => {
  for (const { app, scopes } of installations.splice(0)) {
    await app.close();
    for (const scope of scopes)
      for (const kind of ["incoming", "datasets", "results"])
        await rm(resolve(config.dataDir, kind, scope), {
          recursive: true,
          force: true,
        });
  }
  calls.length = 0;
  afterQuery = undefined;
});
async function fixture() {
  const store = new MemoryStore(),
    app = await createApp(store);
  const origin = await app.listen({ host: "127.0.0.1", port: 0 });
  const scope = (await app.inject("/api/v1/me")).json().workspaces[0].id;
  installations.push({ app, scopes: [scope] });
  const dataset = await ingestRows(store, scope, "Accounts", [
    { id: 1, amount: 150 },
    { id: 2, amount: 250 },
  ]);
  const base = `/api/v1/workspaces/${scope}`;
  const request = (method: any, path: string, payload?: unknown) =>
    app.inject({ method, url: base + path, payload: payload as any });
  const spec = {
    title: "My accounts",
    why: "Defined by owner",
    kind: "kpi",
    datasetId: dataset.id,
    measure: "count",
    format: "number",
    scale: "none",
  };
  return {
    app,
    store,
    origin,
    scope,
    base,
    dataset,
    request,
    spec,
    endpoint: "/home/pinned-metrics",
  };
}
it("pins exact human counts, sums and averages through native queries independently of automatic regeneration", async () => {
  const f = await fixture();
  const count = await f.request("POST", f.endpoint, { spec: f.spec });
  expect(count.statusCode, count.body).toBe(200);
  expect(count.json()).toMatchObject({
    revision: 1,
    spec: f.spec,
    version: f.dataset.data.activeVersion,
    result: { value: 2 },
  });
  const sum = await f.request("POST", f.endpoint, {
    spec: {
      ...f.spec,
      title: "Net total",
      measure: "sum",
      column: "amount",
      format: "currency",
      scale: "cents",
    },
  });
  expect(sum.statusCode, sum.body).toBe(200);
  expect(sum.json().result.value).toBe(4);
  const avg = await f.request("POST", f.endpoint, {
    spec: {
      ...f.spec,
      title: "Average amount",
      measure: "avg",
      column: "amount",
    },
  });
  expect(avg.statusCode, avg.body).toBe(200);
  expect(avg.json().result.value).toBe(200);
  const before = await f.store.list(f.scope, "pinnedMetric");
  expect(
    (await f.request("POST", "/home/metrics/regenerate", {})).statusCode,
  ).toBe(200);
  expect(await f.store.list(f.scope, "pinnedMetric")).toEqual(before);
  expect((await f.request("GET", f.endpoint)).json().items).toHaveLength(3);
});
it("reruns saved definitions against refreshed immutable current snapshots with exact citations", async () => {
  const f = await fixture();
  const pin = await f.request("POST", f.endpoint, { spec: f.spec });
  expect(pin.statusCode, pin.body).toBe(200);
  const before = await f.store.get(f.scope, pin.json().id);
  const changed = await ingestRows(
    f.store,
    f.scope,
    f.dataset.name,
    [{ id: 3, amount: 99 }],
    undefined,
    f.dataset.id,
  );
  const result = await f.request("GET", f.endpoint);
  expect(result.statusCode, result.body).toBe(200);
  expect(result.json().items[0]).toMatchObject({
    spec: f.spec,
    version: changed.data.activeVersion,
    result: { value: 1 },
  });
  expect(await f.store.get(f.scope, pin.json().id)).toEqual(before);
  expect(result.json().datasets[0].numericColumns).toContain("amount");
});
it("protects edits and deletion with revisions, including concurrent real public requests", async () => {
  const f = await fixture(),
    created = await f.request("POST", f.endpoint, { spec: f.spec });
  expect(created.statusCode, created.body).toBe(200);
  const id = created.json().id;
  const replies = await Promise.all(
    ["First", "Second"].map((title) =>
      f.request("PUT", f.endpoint + "/" + id, {
        revision: 1,
        spec: { ...f.spec, title },
      }),
    ),
  );
  expect(replies.map((r) => r.statusCode).sort()).toEqual([200, 409]);
  const saved = (await f.request("GET", f.endpoint)).json().items[0];
  expect(saved.revision).toBe(2);
  expect(
    (await f.request("DELETE", f.endpoint + "/" + id, { revision: 1 }))
      .statusCode,
  ).toBe(409);
  expect(
    (await f.request("DELETE", f.endpoint + "/" + id, { revision: 2 }))
      .statusCode,
  ).toBe(200);
  expect((await f.request("GET", f.endpoint)).json().items).toEqual([]);
});
it("rejects unsupported and sensitive definitions without silently rewriting the user's choice", async () => {
  const f = await fixture();
  for (const spec of [
    { ...f.spec, measure: "sum" },
    { ...f.spec, measure: "sum", column: "missing" },
    { ...f.spec, column: "amount" },
    { ...f.spec, format: "currency" },
    { ...f.spec, filter: { column: "id", value: "1" } },
    { ...f.spec, timeColumn: "amount" },
  ]) {
    expect([400, 422]).toContain(
      (await f.request("POST", f.endpoint, { spec })).statusCode,
    );
  }
  expect(await f.store.list(f.scope, "pinnedMetric")).toEqual([]);
  const created = await f.request("POST", f.endpoint, {
    spec: { ...f.spec, measure: "sum", column: "amount" },
  });
  expect(created.statusCode, created.body).toBe(200);
  await transaction(f.store, f.scope, async (tx) => {
    const d = (await tx.get(f.dataset.id))!;
    tx.update(d, {
      ...d.data,
      profile: {
        ...d.data.profile,
        columns: d.data.profile.columns.map((c: any) =>
          c.name === "amount" ? { ...c, sensitive: true } : c,
        ),
      },
    });
  });
  calls.length = 0;
  const result = (await f.request("GET", f.endpoint)).json();
  expect(result.datasets[0].numericColumns).not.toContain("amount");
  expect(result.items[0].result.error).toContain("no longer available");
  expect(calls).toHaveLength(0);
  expect(await f.store.list(f.scope, "pinnedMetric")).toHaveLength(1);
});
it("supports the real SDK and read-only tokens while enforcing workspace/kind ownership", async () => {
  const f = await fixture();
  const token = (
    await f.request("POST", "/tokens", { name: "Owner SDK", readOnly: false })
  ).json().token;
  const sdk = new FoundryClient({
    baseUrl: f.origin,
    workspaceId: f.scope,
    token,
  });
  const pin = await sdk.pinMetric(f.spec as any);
  expect(pin.result.value).toBe(2);
  const edited = await sdk.updatePinnedMetric(pin.id, pin.revision, {
    ...f.spec,
    title: "SDK title",
  } as any);
  expect(edited.revision).toBe(2);
  const viewerToken = (
    await f.request("POST", "/tokens", { name: "Viewer SDK", readOnly: true })
  ).json().token;
  const viewer = new FoundryClient({
    baseUrl: f.origin,
    workspaceId: f.scope,
    token: viewerToken,
  });
  expect((await viewer.pinnedMetrics()).items[0].spec.title).toBe("SDK title");
  await expect(viewer.pinMetric(f.spec as any)).rejects.toMatchObject({
    status: 403,
  });
  await expect(
    viewer.removePinnedMetric(pin.id, edited.revision),
  ).rejects.toMatchObject({ status: 403 });
  expect(
    (
      await f.request("PUT", f.endpoint + "/" + f.dataset.id, {
        revision: 1,
        spec: f.spec,
      })
    ).statusCode,
  ).toBe(404);
  const foreign = (
    await f.app.inject({
      method: "POST",
      url: "/api/v1/workspaces",
      payload: { name: "Other owner workspace" },
    })
  ).json().id;
  expect(
    (
      await f.app.inject({
        method: "DELETE",
        url: `/api/v1/workspaces/${foreign}/home/pinned-metrics/${pin.id}`,
        payload: { revision: 2 },
      })
    ).statusCode,
  ).toBe(404);
  await sdk.removePinnedMetric(pin.id, edited.revision);
  expect((await sdk.pinnedMetrics()).items).toEqual([]);
});
it("rejects a current-data change during query validation and keeps absent-generation definitions visible as errors", async () => {
  const f = await fixture();
  afterQuery = async () =>
    transaction(f.store, f.scope, async (tx) => {
      const d = (await tx.get(f.dataset.id))!;
      tx.update(d, { ...d.data, activeVersion: "new-during-check" });
    });
  const raced = await f.request("POST", f.endpoint, { spec: f.spec });
  expect(raced.statusCode, raced.body).toBe(409);
  expect(await f.store.list(f.scope, "pinnedMetric")).toEqual([]);
  await transaction(f.store, f.scope, async (tx) => {
    const d = (await tx.get(f.dataset.id))!;
    tx.update(d, { ...d.data, activeVersion: f.dataset.data.activeVersion });
  });
  const created = await f.request("POST", f.endpoint, { spec: f.spec });
  expect(created.statusCode, created.body).toBe(200);
  await transaction(f.store, f.scope, async (tx) => {
    const d = (await tx.get(f.dataset.id))!;
    tx.update(d, { ...d.data, generation: "retired" });
  });
  calls.length = 0;
  const result = await f.request("GET", f.endpoint);
  expect(result.json().datasets).toEqual([]);
  expect(result.json().items[0].result.error).toContain("no longer available");
  expect(calls).toHaveLength(0);
});
it("keeps explicit date-window and cents semantics on native CSV dates, including empty averages", async () => {
  const f = await fixture();
  const dir = resolve(config.dataDir, "incoming", f.scope);
  await mkdir(dir, { recursive: true });
  await writeFile(
    resolve(dir, "dated.csv"),
    "id,amount,created_at\n1,100,2026-07-01\n2,200,2026-07-20\n3,300,2026-08-20\n",
  );
  const dated = await ingestFile(
    f.store,
    f.scope,
    "Dated revenue",
    "dated.csv",
  );
  const spec = {
    ...f.spec,
    datasetId: dated.id,
    measure: "sum",
    column: "amount",
    timeColumn: "created_at",
    scale: "cents",
    format: "currency",
  };
  const result = await f.request("POST", f.endpoint, { spec });
  expect(result.statusCode, result.body).toBe(200);
  expect(result.json().spec).toEqual(spec);
  expect(result.json().result).toEqual({
    value: 3,
    previous: 3,
    asOf: "2026-08-20",
  });
  expect(
    (await f.request("GET", f.endpoint))
      .json()
      .datasets.find((d: any) => d.id === dated.id).timeColumns,
  ).toContain("created_at");
  await writeFile(resolve(dir, "empty.csv"), "");
  const empty = await ingestFile(
    f.store,
    f.scope,
    "Empty average",
    "empty.csv",
    undefined,
    undefined,
    undefined,
    [{ name: "amount", type: "integer" }],
  );
  const average = await f.request("POST", f.endpoint, {
    spec: { ...f.spec, datasetId: empty.id, measure: "avg", column: "amount" },
  });
  expect(average.statusCode, average.body).toBe(200);
  expect(average.json().result.value).toBeNull();
  // All-null numeric input is rejected rather than inferred as a number.
  const nulls = await ingestRows(f.store, f.scope, "Missing amounts", [
    { amount: null },
  ]);
  expect(
    (
      await f.request("POST", f.endpoint, {
        spec: {
          ...f.spec,
          datasetId: nulls.id,
          measure: "avg",
          column: "amount",
        },
      })
    ).statusCode,
  ).toBe(422);
});
it("never labels a query against a newer snapshot with the previously captured citation", async () => {
  const f = await fixture();
  const saved = await f.request("POST", f.endpoint, { spec: f.spec });
  expect(saved.statusCode, saved.body).toBe(200);
  const newer = await ingestRows(
    f.store,
    f.scope,
    f.dataset.name,
    [{ id: 3, amount: 900 }],
    undefined,
    f.dataset.id,
  );
  await transaction(f.store, f.scope, async (tx) => {
    const d = (await tx.get(f.dataset.id))!;
    tx.update(d, { ...d.data, activeVersion: f.dataset.data.activeVersion });
  });
  const originalGet = f.store.get.bind(f.store);
  let intervened = false;
  f.store.get = async (scope, id) => {
    if (!intervened && scope === f.scope && id === f.dataset.id) {
      intervened = true;
      await transaction(f.store, f.scope, async (tx) => {
        const d = (await tx.get(id))!;
        tx.update(d, { ...d.data, activeVersion: newer.data.activeVersion });
      });
    }
    return originalGet(scope, id);
  };
  try {
    const checked = await f.request("GET", f.endpoint);
    expect(checked.statusCode, checked.body).toBe(200);
    expect(checked.json().items[0].result).toEqual({
      error:
        "The dataset changed while computing this metric; refresh to retry",
    });
    expect(calls.at(-1).inputs[0].version).toBe(newer.data.activeVersion);
  } finally {
    f.store.get = originalGet;
  }
  const current = (await f.request("GET", f.endpoint)).json().items[0];
  expect(current.version).toBe(newer.data.activeVersion);
  expect(current.result.value).toBe(1);
});
it("caps the persistent workspace pin collection at eight without touching datasets", async () => {
  const f = await fixture();
  const before = await f.store.get(f.scope, f.dataset.id);
  for (let i = 0; i < 8; i++) {
    const response = await f.request("POST", f.endpoint, {
      spec: { ...f.spec, title: `Owner number ${i + 1}` },
    });
    expect(response.statusCode, response.body).toBe(200);
  }
  expect(
    (await f.request("POST", f.endpoint, { spec: f.spec })).statusCode,
  ).toBe(409);
  expect((await f.request("GET", f.endpoint)).json().items).toHaveLength(8);
  expect(await f.store.get(f.scope, f.dataset.id)).toEqual(before);
});
it("retains the existing automatic metric contract as an unchanged base control", async () => {
  const f = await fixture();
  const result = await f.request("GET", "/home/metrics");
  expect(result.statusCode, result.body).toBe(200);
  expect(result.json().source).toBe("computed");
});
