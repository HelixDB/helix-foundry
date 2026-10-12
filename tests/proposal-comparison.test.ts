import { beforeAll, afterAll, afterEach, expect, it } from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { config } from "../apps/api/src/config.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, transaction } from "../apps/api/src/store.js";
import { ingestRows } from "../apps/api/src/datasets.js";
import { FoundryClient } from "../packages/sdk/src/index.js";

const originalExecutor = config.executor;
const calls: any[] = [];
const executor = createServer(async (req, res) => {
  try {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(chunk);
    const job = JSON.parse(Buffer.concat(chunks).toString());
    calls.push(job);
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify(await nativeExecute(job)));
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
});
async function fixture() {
  const store = new MemoryStore(),
    app = await createApp(store);
  const origin = await app.listen({ host: "127.0.0.1", port: 0 });
  const scope = (await app.inject("/api/v1/me")).json().workspaces[0].id;
  installations.push({ app, scopes: [scope] });
  const ds = await ingestRows(store, scope, "Definition rows", [
    { category: "A", profit: 10, note: "first" },
    { category: "B", profit: -5, note: "second" },
  ]);
  const base = `/api/v1/workspaces/${scope}`,
    table = ds.data.table;
  const original = {
    summary: "Original workspace",
    assumptions: ["Original assumption"],
    catalog: [
      { datasetId: ds.id, description: "Old description", tags: ["finance"] },
    ],
    pipelines: [
      {
        name: "Totals",
        description: "Original transform",
        inputs: [ds.id],
        sql: `SELECT category,profit FROM ${table}`,
        nodes: [],
        checks: [],
      },
    ],
    ontology: [
      {
        name: "Item",
        datasetId: ds.id,
        primaryKey: "category",
        properties: ["category", "profit", "note"],
      },
      {
        name: "Obsolete",
        datasetId: ds.id,
        primaryKey: "category",
        properties: ["category"],
      },
    ],
    relationships: [
      {
        name: "Contains",
        fromType: "Obsolete",
        toType: "Item",
        fromColumn: "category",
        toColumn: "category",
      },
    ],
    dashboards: [
      {
        name: "Profit",
        widgets: [
          {
            title: "Net profit",
            type: "metric",
            inputs: [ds.id],
            sql: `SELECT sum(profit) total FROM ${table}`,
          },
        ],
      },
    ],
    actions: [{ name: "Edit profit", objectType: "Item", fields: ["profit"] }],
  };
  const revised = {
    ...original,
    summary: "Revised workspace",
    assumptions: ["Revised assumption"],
    catalog: [
      {
        datasetId: ds.id,
        description: "Revised description",
        tags: ["profit"],
      },
    ],
    pipelines: [
      {
        ...original.pipelines[0],
        sql: `SELECT category,profit*2 profit FROM ${table}`,
      },
    ],
    ontology: [{ ...original.ontology[0], properties: ["category", "profit"] }],
    relationships: [],
    dashboards: [
      {
        name: "Profit",
        widgets: [
          {
            title: "Doubled",
            type: "metric",
            inputs: [ds.id],
            sql: `SELECT sum(profit)*2 total FROM ${table}`,
          },
        ],
      },
      {
        name: "Extra",
        widgets: [
          {
            title: "Rows",
            type: "metric",
            inputs: [ds.id],
            sql: `SELECT count(*) total FROM ${table}`,
          },
        ],
      },
    ],
    actions: [{ name: "Flag", objectType: "Item", fields: ["profit"] }],
  };
  const save = async (bundle: any) => {
    const r = await app.inject({
      method: "POST",
      url: base + "/definitions/import",
      payload: { version: 1, proposals: [bundle] },
    });
    expect(r.statusCode, r.body).toBe(200);
    const p = r.json().items[0];
    expect(
      p.data.validation.valid,
      JSON.stringify(p.data.validation.checks),
    ).toBe(true);
    return p;
  };
  const publish = async (p: any) => {
    const r = await app.inject({
      method: "POST",
      url: base + `/proposals/${p.id}/publish`,
      payload: { hash: p.data.hash },
    });
    expect(r.statusCode, r.body).toBe(200);
    return r.json();
  };
  return {
    store,
    app,
    origin,
    scope,
    ds,
    base,
    original,
    revised,
    save,
    publish,
  };
}
it("compares all actual published asset kinds without promoting or mutating records", async () => {
  const f = await fixture();
  await f.publish(await f.save(f.original));
  const p = await f.save(f.revised);
  const before = await f.store.list(f.scope);
  const r = await f.app.inject(f.base + `/proposals/${p.id}/comparison`);
  expect(r.statusCode, r.body).toBe(200);
  const c = r.json();
  expect(c.counts).toEqual({ added: 2, changed: 4, removed: 3 });
  expect(c.stale).toBe(false);
  expect(c.changes.map((x: any) => [x.kind, x.name, x.status])).toEqual([
    ["catalog", f.ds.id, "changed"],
    ["pipelines", "Totals", "changed"],
    ["ontology", "Item", "changed"],
    ["ontology", "Obsolete", "removed"],
    ["relationships", "Contains", "removed"],
    ["dashboards", "Extra", "added"],
    ["dashboards", "Profit", "changed"],
    ["actions", "Edit profit", "removed"],
    ["actions", "Flag", "added"],
  ]);
  expect(c.changes.find((x: any) => x.kind === "pipelines")).toMatchObject({
    fields: ["sql"],
    before: { sql: f.original.pipelines[0].sql },
    after: { sql: f.revised.pipelines[0].sql },
  });
  expect(c.metadata.map((m: any) => m.field)).toEqual([
    "summary",
    "assumptions",
  ]);
  expect(await f.store.list(f.scope)).toEqual(before);
});
it("compares an initial unpublished proposal with empty actual definitions", async () => {
  const f = await fixture(),
    p = await f.save(f.original);
  const r = await f.app.inject(f.base + `/proposals/${p.id}/comparison`);
  expect(r.statusCode, r.body).toBe(200);
  expect(r.json()).toMatchObject({
    activeGeneration: null,
    validatedGeneration: null,
    stale: false,
    counts: { added: 7, changed: 0, removed: 0 },
  });
});
it("reports exact refreshed input staleness without changing accepted generation", async () => {
  const f = await fixture();
  await f.publish(await f.save(f.original));
  const p = await f.save(f.revised),
    ws = await f.store.get(f.scope, "workspace");
  const refresh = await ingestRows(
    f.store,
    f.scope,
    f.ds.name,
    [
      { category: "A", profit: 20, note: "first" },
      { category: "B", profit: -10, note: "second" },
    ],
    undefined,
    f.ds.id,
  );
  const r = await f.app.inject(f.base + `/proposals/${p.id}/comparison`);
  expect(r.statusCode, r.body).toBe(200);
  expect(r.json()).toMatchObject({
    stale: true,
    staleInputs: [
      {
        datasetId: f.ds.id,
        validatedVersion: f.ds.data.activeVersion,
        currentVersion: refresh.data.activeVersion,
      },
    ],
  });
  expect((await f.store.get(f.scope, "workspace"))!.data.activeGeneration).toBe(
    ws!.data.activeGeneration,
  );
});
it("compares with newly published definitions and identifies a stale validated generation", async () => {
  const f = await fixture();
  await f.publish(await f.save(f.original));
  const pending = await f.save(f.revised);
  const next = {
    ...f.original,
    summary: "Another accepted view",
    dashboards: [{ name: "Third", widgets: f.original.dashboards[0].widgets }],
  };
  await f.publish(await f.save(next));
  const r = await f.app.inject(f.base + `/proposals/${pending.id}/comparison`);
  expect(r.statusCode, r.body).toBe(200);
  expect(r.json().stale).toBe(true);
  expect(r.json().activeGeneration).not.toBe(r.json().validatedGeneration);
  expect(r.json().changes).toContainEqual(
    expect.objectContaining({
      kind: "dashboards",
      name: "Third",
      status: "removed",
    }),
  );
  expect(r.json().changes).toContainEqual(
    expect.objectContaining({
      kind: "dashboards",
      name: "Profit",
      status: "added",
    }),
  );
});
it("supports read-only SDK access and denies foreign workspace proposal identities", async () => {
  const f = await fixture();
  const p = await f.save(f.original);
  const token = (
    await f.app.inject({
      method: "POST",
      url: f.base + "/tokens",
      payload: { name: "Reader", readOnly: true },
    })
  ).json().token;
  const client = new FoundryClient({
    baseUrl: f.origin,
    workspaceId: f.scope,
    token,
  });
  expect((await client.proposalComparison(p.id)).counts.added).toBe(7);
  const other = (
    await f.app.inject({
      method: "POST",
      url: "/api/v1/workspaces",
      payload: { name: "Foreign" },
    })
  ).json();
  expect(
    (
      await f.app.inject(
        `/api/v1/workspaces/${other.id}/proposals/${p.id}/comparison`,
      )
    ).statusCode,
  ).toBe(404);
});
it("refuses ambiguous duplicate names and missing published provenance rather than inventing changes", async () => {
  const f = await fixture(),
    p = await f.save(f.original);
  await transaction(f.store, f.scope, async (tx) => {
    const q = await tx.get(p.id);
    tx.update(q!, {
      ...q!.data,
      bundle: {
        ...q!.data.bundle,
        dashboards: [
          ...q!.data.bundle.dashboards,
          ...q!.data.bundle.dashboards,
        ],
      },
    });
  });
  const r = await f.app.inject(f.base + `/proposals/${p.id}/comparison`);
  expect(r.statusCode).toBe(422);
  expect(r.json().error).toContain("unique dashboards");
  await transaction(f.store, f.scope, async (tx) => {
    const ws = await tx.get("workspace");
    tx.update(ws!, { ...ws!.data, activeGeneration: "missing-generation" });
  });
  const missing = await f.app.inject(f.base + `/proposals/${p.id}/comparison`);
  expect(missing.statusCode).toBe(409);
});
it("keeps existing native publication and current queries as unchanged positive controls", async () => {
  const f = await fixture();
  await f.publish(await f.save(f.original));
  const r = await f.app.inject({
    method: "POST",
    url: f.base + "/query",
    payload: {
      inputs: [f.ds.id],
      sql: `SELECT sum(profit) total FROM ${f.ds.data.table}`,
    },
  });
  expect(r.statusCode, r.body).toBe(200);
  expect(r.json().rows).toEqual([{ total: 5 }]);
  expect(
    (await f.store.get(f.scope, "workspace"))!.data.activeGeneration,
  ).toBeTruthy();
});
