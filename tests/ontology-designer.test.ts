import { beforeAll, afterAll, afterEach, expect, it } from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { config } from "../apps/api/src/config.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, transaction } from "../apps/api/src/store.js";
import { datasetTable } from "../packages/shared/src/index.js";
import { FoundryClient } from "../packages/sdk/src/index.js";

const originalExecutor = config.executor;
const calls: any[] = [];
let afterExecute: (() => Promise<void>) | undefined;
const executor = createServer(async (req, res) => {
  try {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(chunk);
    const job = JSON.parse(Buffer.concat(chunks).toString());
    calls.push(job);
    res.setHeader("content-type", "application/json");
    const result = await nativeExecute(job);
    const hook = afterExecute;
    afterExecute = undefined;
    if (hook) await hook();
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
  afterExecute = undefined;
});
async function fixture() {
  const store = new MemoryStore(),
    app = await createApp(store),
    origin = await app.listen({ host: "127.0.0.1", port: 0 });
  const scope = (await app.inject("/api/v1/me")).json().workspaces[0].id;
  installations.push({ app, scopes: [scope] });
  const base = `/api/v1/workspaces/${scope}`;
  const post = async (path: string, payload: any) => {
    const r = await app.inject({ method: "POST", url: base + path, payload });
    expect(r.statusCode, r.body).toBe(200);
    return r.json();
  };
  const customers = await post("/ingest", {
    name: "Customers",
    rows: [
      { id: "A", name: "Acme" },
      { id: "B", name: "Beta" },
    ],
  });
  const orders = await post("/ingest", {
    name: "Orders",
    rows: [
      { id: "O1", customer: "A", amount: 20 },
      { id: "O2", customer: "A", amount: 30 },
      { id: "O3", customer: "B", amount: 40 },
    ],
  });
  const token = await post("/tokens", { name: "Designer", readOnly: false });
  const client = new FoundryClient({
    baseUrl: origin,
    workspaceId: scope,
    token: token.token,
  });
  const existing = {
    name: "Existing",
    datasetId: customers.id,
    primaryKey: "id",
    properties: ["id", "name"],
    description: "Accepted type",
  };
  const original = {
    summary: "Accepted workspace",
    assumptions: ["Keep accepted meaning"],
    catalog: [
      {
        datasetId: customers.id,
        description: "Existing catalog",
        tags: ["accepted"],
      },
    ],
    pipelines: [],
    ontology: [existing],
    relationships: [],
    dashboards: [
      {
        name: "Total",
        widgets: [
          {
            title: "Total",
            type: "metric",
            inputs: [orders.id],
            sql: `SELECT sum(amount) total FROM ${datasetTable(orders.id)}`,
          },
        ],
      },
    ],
    actions: [{ name: "Edit name", objectType: "Existing", fields: ["name"] }],
  };
  const save = async (bundle: any) =>
    (await post("/definitions/import", { version: 1, proposals: [bundle] }))
      .items[0];
  const publish = async (p: any) => {
    expect(
      p.data.validation.valid,
      JSON.stringify(p.data.validation.checks),
    ).toBe(true);
    return post(`/proposals/${p.id}/publish`, { hash: p.data.hash });
  };
  const customer = {
    name: "Customer",
    datasetId: customers.id,
    primaryKey: "id",
    properties: ["id", "name"],
    description: "Customer mapping",
  };
  const order = {
    name: "Order",
    datasetId: orders.id,
    primaryKey: "id",
    properties: ["id", "customer", "amount"],
    description: "Order mapping",
  };
  const relationships = [
    {
      name: "Placed by",
      fromType: "Order",
      toType: "Customer",
      fromColumn: "customer",
      toColumn: "id",
    },
  ];
  return {
    store,
    app,
    origin,
    scope,
    base,
    post,
    customers,
    orders,
    client,
    original,
    save,
    publish,
    mappings: [existing, customer, order],
    relationships,
  };
}
it("starts explicitly empty and does not adopt an unrelated unaccepted proposal", async () => {
  const f = await fixture();
  await f.save(f.original);
  const current = await f.client.ontologyDefinitions();
  expect(current.activeGeneration).toBeNull();
  expect(current.bundle.ontology).toEqual([]);
  expect(current.bundle.dashboards).toEqual([]);
});
it("creates a real mapping review preserving accepted assets and publishes exact linked native records", async () => {
  const f = await fixture();
  await f.publish(await f.save(f.original));
  const before = await f.client.ontologyDefinitions();
  const p = await f.client.draftOntology(
    before.activeGeneration,
    f.mappings,
    f.relationships,
  );
  expect(p.data.validation.valid).toBe(true);
  for (const key of [
    "catalog",
    "pipelines",
    "dashboards",
    "actions",
    "assumptions",
  ])
    expect(p.data.bundle[key]).toEqual((before.bundle as any)[key]);
  expect((await f.client.ontologyDefinitions()).activeGeneration).toBe(
    before.activeGeneration,
  );
  await f.publish(p);
  const after = await f.client.ontologyDefinitions();
  expect(after.activeGeneration).not.toBe(before.activeGeneration);
  expect(after.bundle.ontology).toEqual(f.mappings);
  expect(after.bundle.relationships).toEqual(f.relationships);
  const objects = (
    await f.app.inject(f.base + "/resources/object?limit=100")
  ).json().items;
  const customer = objects.find(
    (o: any) => o.data.type === "Customer" && o.data.effective.id === "A",
  );
  expect(customer).toBeTruthy();
  const record = (
    await f.app.inject(
      f.base + `/records/${encodeURIComponent(customer.data.logicalId)}`,
    )
  ).json();
  expect(record.record.data.effective.name).toBe("Acme");
  expect(record.related.map((r: any) => r.data.effective.id).sort()).toEqual([
    "O1",
    "O2",
  ]);
});
it("refuses stale designer baselines before running SQL or writing a draft", async () => {
  const f = await fixture();
  await f.publish(await f.save(f.original));
  const old = await f.client.ontologyDefinitions();
  await f.publish(
    await f.save({ ...f.original, summary: "New accepted generation" }),
  );
  const count = (await f.store.list(f.scope, "proposal")).length,
    n = calls.length;
  await expect(
    f.client.draftOntology(old.activeGeneration, f.mappings, f.relationships),
  ).rejects.toMatchObject({ status: 409 });
  expect(calls.length).toBe(n);
  expect((await f.store.list(f.scope, "proposal")).length).toBe(count);
});
it("keeps native duplicate-key validation reviewable without publishing invalid mappings", async () => {
  const f = await fixture();
  await f.publish(await f.save(f.original));
  const current = await f.client.ontologyDefinitions();
  const duplicate = await f.post("/ingest", {
    name: "Duplicate customers",
    rows: [
      { id: "A", name: "First" },
      { id: "A", name: "Second" },
    ],
  });
  const p = await f.client.draftOntology(
    current.activeGeneration,
    [...f.original.ontology, { ...f.mappings[1], datasetId: duplicate.id }],
    [],
  );
  expect(p.data.validation.valid).toBe(false);
  expect(
    p.data.validation.checks.some(
      (c: any) => !c.passed && /key|unique/i.test(c.detail),
    ),
  ).toBe(true);
  expect((await f.client.ontologyDefinitions()).activeGeneration).toBe(
    current.activeGeneration,
  );
  expect(p.data.bundle.ontology[1].datasetId).toBe(duplicate.id);
  const retested = await f.post(`/proposals/${p.id}/validate`, {});
  expect(retested.data.validation.valid).toBe(false);
  expect(retested.data.bundle.ontology[1].datasetId).toBe(duplicate.id);
});
it("read-only SDK users can inspect but cannot create mappings; foreign workspace source IDs stay unavailable", async () => {
  const f = await fixture();
  await f.publish(await f.save(f.original));
  const token = await f.post("/tokens", {
    name: "Readonly designer",
    readOnly: true,
  });
  const readonly = new FoundryClient({
    baseUrl: f.origin,
    workspaceId: f.scope,
    token: token.token,
  });
  const current = await readonly.ontologyDefinitions();
  expect(current.bundle.ontology).toEqual(f.original.ontology);
  await expect(
    readonly.draftOntology(
      current.activeGeneration,
      f.mappings,
      f.relationships,
    ),
  ).rejects.toMatchObject({ status: 403 });
  const other = (
    await f.app.inject({
      method: "POST",
      url: "/api/v1/workspaces",
      payload: { name: "Foreign" },
    })
  ).json();
  installations[installations.length - 1].scopes.push(other.id);
  const r = await f.app.inject({
    method: "POST",
    url: `/api/v1/workspaces/${other.id}/ontology/draft`,
    payload: {
      expectedGeneration: null,
      ontology: [f.mappings[1]],
      relationships: [],
    },
  });
  expect(r.statusCode).toBe(200);
  expect(r.json().data.validation.valid).toBe(false);
  expect(
    (await f.store.get(other.id, "workspace"))?.data.activeGeneration,
  ).toBeNull();
});
it("publication during actual native validation cannot persist a deceptively fresh old-baseline draft", async () => {
  const f = await fixture();
  await f.publish(await f.save(f.original));
  const current = await f.client.ontologyDefinitions();
  const next = await f.save({
    ...f.original,
    summary: "Concurrent publication",
  });
  const before = (await f.store.list(f.scope, "proposal")).length;
  afterExecute = async () => {
    await f.publish(next);
  };
  await expect(
    f.client.draftOntology(
      current.activeGeneration,
      f.mappings,
      f.relationships,
    ),
  ).rejects.toMatchObject({ status: 409 });
  expect((await f.store.list(f.scope, "proposal")).length).toBe(before);
  expect((await f.client.ontologyDefinitions()).activeGeneration).not.toBe(
    current.activeGeneration,
  );
});
it("existing public publication and native query remain a positive control", async () => {
  const f = await fixture();
  await f.publish(await f.save(f.original));
  const r: any = await f.client.query(
    [f.orders.id],
    `SELECT sum(amount) total FROM ${datasetTable(f.orders.id)}`,
  );
  expect(r.rows).toEqual([{ total: "90" }]);
});
