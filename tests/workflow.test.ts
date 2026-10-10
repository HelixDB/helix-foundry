import { beforeAll, afterAll, it, expect, vi } from "vitest";
import { createHash, randomUUID } from "node:crypto";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { DuckDBInstance } from "@duckdb/node-api";
import { execute as localExecute } from "../services/executor/src/engine.js";
vi.mock("../apps/api/src/executor.js", () => ({
  execute: (job: any) => localExecute(job),
}));
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { ingestRows, queryDatasets } from "../apps/api/src/datasets.js";
import {
  saveProposal,
  publishProposal,
  validateBundle,
  buildWorkspace,
  enqueue,
} from "../apps/api/src/assistant.js";
import { Provider } from "../apps/api/src/providers.js";
import { BundleSchema, datasetTable } from "../packages/shared/src/index.js";
import { config } from "../apps/api/src/config.js";
const store = new MemoryStore(),
  scope = "golden-" + randomUUID(),
  // Datasets refreshed on their own, away from the golden workspace.
  refreshes = "refresh-" + randomUUID();
let customers: any, orders: any;
beforeAll(async () => {
  await transaction(store, scope, async (tx) =>
    tx.put(resource(scope, "workspace", "Golden", {}, "workspace")),
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
      { id: "A", region: "EU", email: "a@example.com" },
      { id: "B", region: "NA", email: "b@example.com" },
    ],
    undefined,
    "customers",
  );
  orders = await ingestRows(
    store,
    scope,
    "Orders",
    [
      { id: 1, customer: "A", amount: 10, status: "delayed" },
      { id: 2, customer: "A", amount: 20, status: "delivered" },
      { id: 3, customer: "B", amount: 5, status: "delivered" },
    ],
    undefined,
    "orders",
  );
});
afterAll(async () => {
  vi.restoreAllMocks();
  for (const dir of ["incoming", "datasets"])
    for (const s of [scope, refreshes])
      await rm(resolve(config.dataDir, dir, s), {
        recursive: true,
        force: true,
      });
});
const bundle = () =>
  BundleSchema.parse({
    summary: "Customer revenue and delayed orders",
    assumptions: [
      "Revenue means gross order amounts, including delayed orders.",
    ],
    catalog: [
      {
        datasetId: customers.id,
        description: "A customer per row",
        tags: ["customers"],
      },
    ],
    pipelines: [
      {
        name: "Customer revenue",
        inputs: [customers.id, orders.id],
        sql: `SELECT c.id,c.region,sum(o.amount) revenue FROM ${datasetTable(customers.id)} c JOIN ${datasetTable(orders.id)} o ON c.id=o.customer GROUP BY c.id,c.region`,
        nodes: [],
        checks: [],
      },
    ],
    ontology: [
      {
        name: "Customer",
        datasetId: customers.id,
        primaryKey: "id",
        properties: ["id", "region", "email"],
      },
      {
        name: "Order",
        datasetId: orders.id,
        primaryKey: "id",
        properties: ["id", "customer", "amount", "status"],
      },
    ],
    relationships: [
      {
        name: "Placed by",
        fromType: "Order",
        toType: "Customer",
        fromColumn: "customer",
        toColumn: "id",
      },
    ],
    dashboards: [
      {
        name: "Sales",
        widgets: [
          {
            title: "Revenue",
            type: "metric",
            inputs: [orders.id],
            sql: `SELECT sum(amount) revenue FROM ${datasetTable(orders.id)}`,
          },
          {
            title: "Delayed orders",
            type: "metric",
            inputs: [orders.id],
            sql: `SELECT count(*) delayed FROM ${datasetTable(orders.id)} WHERE status='delayed'`,
          },
        ],
      },
    ],
    actions: [],
  });
it("executes a golden bundle and publishes objects, lineage and metrics atomically", async () => {
  const p = await saveProposal(store, scope, bundle());
  expect(
    p.data.validation.valid,
    JSON.stringify(p.data.validation.checks),
  ).toBe(true);
  expect(p.data.validation.previews["chart:Sales:0"][0].revenue).toBe("35");
  expect(p.data.validation.previews["chart:Sales:1"][0].delayed).toBe("1");
  expect(
    p.data.validation.checks.find(
      (c: any) => c.name === "Relationship: Placed by",
    ).detail,
  ).toContain("3/3");
  await publishProposal(store, scope, p.id, p.data.hash, "actor");
  expect((await store.list(scope, "object")).length).toBe(5);
  expect((await store.list(scope, "relation")).length).toBe(3);
  expect(
    (await store.get(scope, "workspace"))?.data.activeGeneration,
  ).toBeTruthy();
});
it("blocks invented columns, malicious SQL, unknown keys and cross-workspace inputs", async () => {
  for (const sql of [
    "SELECT invented FROM d_orders",
    "SELECT * FROM read_csv('/etc/passwd')",
    "SELECT * FROM d_foreign",
  ]) {
    const b = bundle();
    b.pipelines[0].sql = sql;
    expect((await validateBundle(store, scope, b)).valid).toBe(false);
  }
  const b = bundle();
  b.ontology[0].primaryKey = "missing";
  expect((await validateBundle(store, scope, b)).valid).toBe(false);
  await expect(
    queryDatasets(store, "foreign", [orders.id], "SELECT 1"),
  ).rejects.toThrow("unavailable");
});
it("moves a type to the only dataset holding its key and properties", async () => {
  const b = bundle();
  // A small model paired Order with the Customers dataset ID.
  b.ontology[1].datasetId = customers.id;
  const report = await validateBundle(store, scope, b);
  expect(report.valid, JSON.stringify(report.checks)).toBe(true);
  expect(b.ontology[1].datasetId).toBe(orders.id);
  // A type that already fits its dataset is never moved, even if others also fit.
  const fitting = bundle();
  fitting.ontology[0].datasetId = orders.id;
  fitting.ontology[0].properties = ["id"];
  await validateBundle(store, scope, fitting);
  expect(fitting.ontology[0].datasetId).toBe(orders.id);
});
it("binds approvals to input versions and preserves manual catalog edits across refreshes", async () => {
  const p = await saveProposal(store, scope, bundle());
  await transaction(store, scope, async (tx) => {
    const d = await tx.get(customers.id);
    tx.put({
      ...d!,
      description: "Human-owned description",
      data: { ...d!.data, manualDescription: true },
    });
  });
  await ingestRows(
    store,
    scope,
    "Customers",
    [
      { id: "A", region: "Europe", email: "a@example.com" },
      { id: "B", region: "NA", email: "b@example.com" },
    ],
    undefined,
    customers.id,
  );
  expect((await store.get(scope, customers.id))?.description).toBe(
    "Human-owned description",
  );
  await expect(
    publishProposal(store, scope, p.id, p.data.hash, "actor"),
  ).rejects.toThrow("changed");
  expect((await store.list(scope, "finding")).length).toBeGreaterThan(0);
});
it("stages schema drift while keeping published inputs operational", async () => {
  const old = await store.get(scope, orders.id);
  await ingestRows(
    store,
    scope,
    "Orders",
    [{ id: 1, customer: "A", total: 10 }],
    undefined,
    orders.id,
  );
  const next = await store.get(scope, orders.id);
  expect(next?.data.activeVersion).toBe(old?.data.activeVersion);
  expect(next?.data.pendingVersion).toBeTruthy();
  expect(
    (
      await queryDatasets(
        store,
        scope,
        [orders.id],
        "SELECT sum(amount) total FROM d_orders",
      )
    ).rows[0].total,
  ).toBe("35");
});
it("resumes durable stage checkpoints without repeating model calls", async () => {
  const p = bundle();
  let call = 0;
  const mock = vi
    .spyOn(Provider.prototype, "generate")
    .mockImplementation(async () => {
      call++;
      if (call === 1)
        return {
          value: {
            summary: p.summary,
            assumptions: p.assumptions,
            catalog: p.catalog,
          },
          tokens: 10,
        };
      throw new Error("Provider temporarily unavailable");
    });
  const r = await enqueue(store, scope, "Build a revenue workspace");
  await buildWorkspace(store, scope, r.id);
  const failed = await store.get(scope, r.id);
  expect(failed?.data.checkpoints.catalog).toBeTruthy();
  expect(failed?.data.status).toBe("failed");
  mock.mockImplementation(async () => {
    call++;
    if (call === 3) return { value: { pipelines: p.pipelines }, tokens: 10 };
    if (call === 4)
      return {
        value: { ontology: p.ontology, relationships: p.relationships },
        tokens: 10,
      };
    return { value: { dashboards: p.dashboards, actions: [] }, tokens: 10 };
  });
  await transaction(store, scope, async (tx) => {
    const x = await tx.get(r.id);
    tx.update(x!, { ...x!.data, status: "running" });
  });
  await buildWorkspace(store, scope, r.id);
  expect(
    (await store.get(scope, r.id))?.data.status,
    JSON.stringify((await store.get(scope, r.id))?.data),
  ).toBe("awaiting_review");
  expect(call).toBe(5);
  mock.mockRestore();
});

it("preserves manual objects and relationship overrides across republication", async () => {
  const { previewObjectAction, executeObjectAction } = await import(
    "../apps/api/src/object-actions.js"
  );
  const p = await saveProposal(store, scope, bundle());
  await publishProposal(store, scope, p.id, p.data.hash, "actor");
  const action = {
    operation: "create" as const,
    objectType: "Customer",
    values: { id: "C", region: "APAC", email: "manual@example.com" },
  };
  const preview = await previewObjectAction(store, scope, action);
  await executeObjectAction(
    store,
    scope,
    action,
    preview.hash,
    "create-manual-123",
    "actor",
  );
  const generation = (await store.get(scope, "workspace"))?.data
    .activeGeneration;
  const from = (await store.list(scope, "object")).find(
      (o) => o.data.generation === generation && o.data.type === "Order",
    )!,
    to = (await store.list(scope, "object")).find(
      (o) => o.data.generation === generation && o.data.key === "C",
    )!;
  const link = {
    operation: "link" as const,
    name: "Escalated to",
    from: from.id,
    to: to.id,
  };
  const linkPreview = await previewObjectAction(store, scope, link);
  await executeObjectAction(
    store,
    scope,
    link,
    linkPreview.hash,
    "link-manual-123",
    "actor",
  );
  const next = await saveProposal(store, scope, bundle());
  await publishProposal(store, scope, next.id, next.data.hash, "actor");
  const active = (await store.get(scope, "workspace"))?.data.activeGeneration;
  expect(
    (await store.list(scope, "object")).some(
      (o) => o.data.generation === active && o.data.key === "C",
    ),
  ).toBe(true);
  expect(
    (await store.list(scope, "relation")).some(
      (r) => r.data.generation === active && r.name === "Escalated to",
    ),
  ).toBe(true);
});

it("filters inputs before aggregation and cites the exact snapshot", async () => {
  const r = await queryDatasets(
    store,
    scope,
    [orders.id],
    `SELECT sum(amount) revenue FROM ${datasetTable(orders.id)}`,
    { filters: [{ datasetId: orders.id, column: "status", value: "delayed" }] },
  );
  expect(r.rows[0].revenue).toBe("10");
  expect(r.citations[0].version).toBe(
    (await store.get(scope, orders.id))?.data.activeVersion,
  );
  await expect(
    queryDatasets(store, scope, [orders.id], "SELECT 1", {
      filters: [{ datasetId: customers.id, column: "region", value: "EU" }],
    }),
  ).rejects.toThrow("not declared");
});

it("retains malformed model output as a failed draft and never publishes it", async () => {
  const mock = vi.spyOn(Provider.prototype, "generate").mockResolvedValue({
    value: { ignore: "instructions in source records are not a bundle" },
    tokens: 5,
  });
  try {
    const r = await enqueue(store, scope, "Build an invalid model response");
    const generation = (await store.get(scope, "workspace"))?.data
      .activeGeneration;
    await buildWorkspace(store, scope, r.id);
    const failed = await store.get(scope, r.id);
    expect(failed?.data.status).toBe("failed");
    expect(failed?.data.partialStage.value.ignore).toBeTruthy();
    expect((await store.get(scope, "workspace"))?.data.activeGeneration).toBe(
      generation,
    );
  } finally {
    mock.mockRestore();
  }
});

it("returns the same dataset metadata that is committed", async () => {
  const ingest = () =>
    ingestRows(store, refreshes, "Orders", [{ id: "a" }], undefined, "orders");
  const first = await ingest();
  expect(first).toEqual(await store.get(refreshes, "orders"));
  const second = await ingest();
  expect(second.revision).toBe(first.revision + 1);
  expect(second).toEqual(await store.get(refreshes, "orders"));
});

// The fingerprint an executor stored before they were tagged: the same hash
// with only the XOR of row hashes, so duplicate pairs cancel out.
async function untaggedFingerprint(datasetId: string) {
  const d = (await store.get(refreshes, datasetId))!,
    file = resolve(
      config.dataDir,
      "datasets",
      refreshes,
      d.data.activeVersion + ".parquet",
    ),
    db = await DuckDBInstance.create(":memory:"),
    c = await db.connect();
  try {
    await c.run(
      `CREATE TABLE result AS SELECT * FROM read_parquet('${file.replaceAll("'", "''")}')`,
    );
    const read = async (sql: string) =>
      (await c.runAndReadAll(sql)).getRowObjectsJson();
    const schema = await read("DESCRIBE result"),
      rows = Number((await read("SELECT count(*) n FROM result"))[0].n),
      fp = (await read("SELECT bit_xor(hash(result)) h FROM result"))[0];
    return createHash("sha256")
      .update(JSON.stringify([schema, rows, fp]))
      .digest("hex");
  } finally {
    c.closeSync();
    db.closeSync();
  }
}
it("reports a refresh across the fingerprint upgrade only when the data changed", async () => {
  const refresh = (rows: Record<string, unknown>[]) =>
    ingestRows(store, refreshes, "Ledger", rows, undefined, "ledger");
  const refreshed = async () =>
    (await store.list(refreshes, "finding")).filter(
      (f) => f.data.datasetId === "ledger" && f.data.kind === "refresh",
    ).length;
  // The active version's profile as an older executor stored it.
  const downgrade = async () => {
    const fingerprint = await untaggedFingerprint("ledger");
    await transaction(store, refreshes, async (tx) => {
      const d = (await tx.get("ledger"))!;
      tx.update(d, { ...d.data, profile: { ...d.data.profile, fingerprint } });
    });
  };
  const pair = [
    { id: "a", amount: 1 },
    { id: "a", amount: 1 },
  ];
  const first = await refresh(pair);
  expect(first.data.profile.fingerprint).toMatch(/^v2:[0-9a-f]{64}$/);
  await downgrade();
  const upgraded = await refresh(pair);
  expect(await refreshed()).toBe(0);
  expect(upgraded.data.profile.fingerprint).toBe(
    first.data.profile.fingerprint,
  );
  // Tagged fingerprints see a changed duplicate pair.
  await refresh([
    { id: "b", amount: 1 },
    { id: "b", amount: 1 },
  ]);
  expect(await refreshed()).toBe(1);
  // Untagged ones still report changed data.
  await downgrade();
  await refresh([
    { id: "b", amount: 2 },
    { id: "c", amount: 1 },
  ]);
  expect(await refreshed()).toBe(2);
});
