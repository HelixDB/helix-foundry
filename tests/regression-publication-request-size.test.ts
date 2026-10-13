import { afterAll, afterEach, beforeAll, expect, it, vi } from "vitest";
import { randomUUID } from "node:crypto";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
vi.mock("../apps/api/src/executor.js", () => ({
  execute: (job: any) => localExecute(job),
}));
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { ingestRows } from "../apps/api/src/datasets.js";
import { publishProposal, saveProposal } from "../apps/api/src/assistant.js";
import { BundleSchema } from "../packages/shared/src/index.js";
import { config } from "../apps/api/src/config.js";

// HelixDB refuses a request body over its limit. Each dataset here fits alone,
// as every imported dataset did when it was written, but two do not fit in one
// request: wide tables carry large column profiles, and a catalog can describe
// hundreds of datasets.
const padding = 200_000,
  limit = 300_000;
const store = new MemoryStore(),
  scope = "request-size-" + randomUUID();
let customers: any, orders: any;

beforeAll(async () => {
  await transaction(store, scope, async (tx) =>
    tx.put(resource(scope, "workspace", "Request size", {}, "workspace")),
  );
  await transaction(store, "global", async (tx) =>
    tx.put(
      resource(
        "global",
        "member",
        "Editor",
        { role: "editor" },
        `actor:${scope}`,
      ),
    ),
  );
  customers = await ingestRows(
    store,
    scope,
    "Customers",
    [
      { id: "A", region: "EU" },
      { id: "B", region: "NA" },
    ],
    undefined,
    "customers",
  );
  orders = await ingestRows(
    store,
    scope,
    "Orders",
    [
      { id: 1, customer: "A" },
      { id: 2, customer: "B" },
    ],
    undefined,
    "orders",
  );
  for (const id of [customers.id, orders.id])
    await transaction(store, scope, async (tx) => {
      const d = (await tx.get(id))!;
      tx.update(d, { ...d.data, padding: "x".repeat(padding) });
    });
});

afterEach(() => {
  vi.restoreAllMocks();
});

afterAll(async () => {
  for (const dir of ["incoming", "datasets"])
    await rm(resolve(config.dataDir, dir, scope), {
      recursive: true,
      force: true,
    });
});

it("publishes a catalog whose datasets together pass the database's request limit", async () => {
  const p = await saveProposal(
    store,
    scope,
    BundleSchema.parse({
      summary: "Customers and orders",
      assumptions: [],
      catalog: [
        {
          datasetId: customers.id,
          description: "A customer per row",
          tags: ["customers"],
        },
        {
          datasetId: orders.id,
          description: "An order per row",
          tags: ["orders"],
        },
      ],
      pipelines: [],
      ontology: [
        {
          name: "Customer",
          datasetId: customers.id,
          primaryKey: "id",
          properties: ["id", "region"],
        },
      ],
      relationships: [],
      dashboards: [],
      actions: [],
    }),
  );
  expect(p.data.validation.valid, JSON.stringify(p.data.validation)).toBe(true);
  const commit = store.commit.bind(store);
  const sizes: number[] = [];
  vi.spyOn(store, "commit").mockImplementation(async (s, v, changes) => {
    const size = JSON.stringify(changes).length;
    sizes.push(size);
    if (size > limit)
      throw new Error("failed to read request body: length limit exceeded");
    return commit(s, v, changes);
  });
  await publishProposal(store, scope, p.id, p.data.hash, "actor");
  expect(Math.max(...sizes)).toBeLessThanOrEqual(limit);
  expect(
    (await store.get(scope, "workspace"))?.data.activeGeneration,
  ).toBeTruthy();
  for (const [d, description, tags] of [
    [customers, "A customer per row", ["customers"]],
    [orders, "An order per row", ["orders"]],
  ] as const) {
    const ds = await store.get(scope, d.id);
    expect(ds?.description).toBe(description);
    expect(ds?.data.tags).toEqual(tags);
    expect(ds?.data.padding).toHaveLength(padding);
  }
});

it("stops describing datasets once another publication replaces the generation", async () => {
  const p = await saveProposal(
    store,
    scope,
    BundleSchema.parse({
      summary: "Customers and orders, described again",
      assumptions: [],
      catalog: [
        {
          datasetId: customers.id,
          description: "Customers, described again",
          tags: ["again"],
        },
        {
          datasetId: orders.id,
          description: "Orders, described again",
          tags: ["again"],
        },
      ],
      pipelines: [],
      ontology: [
        {
          name: "Customer",
          datasetId: customers.id,
          primaryKey: "id",
          properties: ["id", "region"],
        },
      ],
      relationships: [],
      dashboards: [],
      actions: [],
    }),
  );
  expect(p.data.validation.valid, JSON.stringify(p.data.validation)).toBe(true);
  const commit = store.commit.bind(store);
  // A rollback lands as soon as the first description is written.
  vi.spyOn(store, "commit").mockImplementation(async (s, v, changes) => {
    const ok = await commit(s, v, changes);
    if (ok && changes.some((c) => "put" in c && c.put.id === customers.id)) {
      const ws = (await store.get(scope, "workspace"))!;
      await commit(s, await store.version(s), [
        {
          put: { ...ws, data: { ...ws.data, activeGeneration: "rolled-back" } },
        },
      ]);
    }
    return ok;
  });
  await publishProposal(store, scope, p.id, p.data.hash, "actor");
  expect((await store.get(scope, customers.id))?.description).toBe(
    "Customers, described again",
  );
  expect((await store.get(scope, orders.id))?.description).toBe(
    "An order per row",
  );
});
