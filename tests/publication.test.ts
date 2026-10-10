import { afterAll, expect, it, vi } from "vitest";
import { randomUUID } from "node:crypto";
import {
  mkdir,
  readdir,
  readFile,
  rm,
  utimes,
  writeFile,
} from "node:fs/promises";
import { resolve } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
import { resultFile } from "../services/executor/src/paths.js";
const actual = vi.hoisted(() => ({ resultBatches: undefined as any }));
vi.mock("../apps/api/src/executor.js", () => ({
  execute: vi.fn((job: any) => localExecute(job)),
}));
vi.mock("../apps/api/src/results.js", async (original) => {
  const results = await original<typeof import("../apps/api/src/results.js")>();
  actual.resultBatches = results.resultBatches;
  return { ...results, resultBatches: vi.fn(results.resultBatches) };
});
import { execute } from "../apps/api/src/executor.js";
import { resultBatches, sweepResults } from "../apps/api/src/results.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { ingestFile } from "../apps/api/src/datasets.js";
import { publishProposal, saveProposal } from "../apps/api/src/assistant.js";
import { config } from "../apps/api/src/config.js";
import { BundleSchema, type Resource } from "../packages/shared/src/index.js";
const store = new MemoryStore(),
  scopes: string[] = [],
  executor = vi.mocked(execute as (job: any) => Promise<any>);
const big = 9007199254740993n;
// 3,000 customers with BIGINT keys past 2^53, and 7,000 orders of which 200
// reference no customer.
async function workspace() {
  const scope = "publication-" + randomUUID();
  scopes.push(scope);
  await transaction(store, scope, async (tx) =>
    tx.put(resource(scope, "workspace", "Publication", {}, "workspace")),
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
  const incoming = resolve(config.dataDir, "incoming", scope);
  await mkdir(incoming, { recursive: true });
  await writeFile(
    resolve(incoming, "customers.jsonl"),
    Array.from(
      { length: 3000 },
      (_, i) =>
        `{"id":${big + BigInt(i)},"name":"Customer ${i}","region":${i % 3 ? '"EU"' : "null"},"score":${i / 8},"joined":"2026-0${1 + (i % 9)}-15"}`,
    ).join("\n"),
  );
  await writeFile(
    resolve(incoming, "orders.jsonl"),
    Array.from(
      { length: 7000 },
      (_, i) =>
        `{"id":${i},"customer":${big + BigInt(i % 3100)},"amount":${(i % 977) / 4},"status":"${i % 5 ? "complete" : "pending"}"}`,
    ).join("\n"),
  );
  for (const name of ["customers", "orders"])
    await ingestFile(
      store,
      scope,
      name,
      name + ".jsonl",
      undefined,
      name,
      name + "_v1",
    );
  const bundle = BundleSchema.parse({
    summary: "Customers and orders",
    assumptions: [],
    catalog: [],
    pipelines: [],
    ontology: [
      {
        name: "Customer",
        datasetId: "customers",
        primaryKey: "id",
        properties: ["id", "name", "region", "score", "joined"],
      },
      {
        name: "Order",
        datasetId: "orders",
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
    dashboards: [],
    actions: [],
  });
  // A validated proposal, ready to publish.
  const draft = async () => {
    const p = await saveProposal(store, scope, bundle);
    expect(p.data.validation.valid, JSON.stringify(p.data.validation)).toBe(
      true,
    );
    return () => publishProposal(store, scope, p.id, p.data.hash, "actor");
  };
  return { scope, draft };
}
// The path this replaced: the same query, loaded and profiled again for every
// 500 rows, with rows crossing HTTP as JSON.
async function* paged(job: Parameters<typeof resultBatches>[0]) {
  for (let offset = 0, total = Infinity; offset < total; offset += 500) {
    const r = await localExecute({
      ...job,
      mode: "query",
      version: randomUUID(),
      offset,
      limit: 500,
    });
    total = r.profile.rows;
    yield JSON.parse(JSON.stringify(r.rows)) as Record<string, any>[];
  }
}
// Published records, without their generation.
async function published(scope: string) {
  const generation = (await store.get(scope, "workspace"))!.data
      .activeGeneration as string,
    strip = (id: string) => id.slice(generation.length + 1),
    current = (r: Resource) => r.data.generation === generation;
  const objects = (await store.list(scope, "object"))
    .filter(current)
    .map(({ id, name, data: { generation: _, ...data } }) => ({
      id: strip(id),
      name,
      data,
    }))
    .sort((a, b) => a.id.localeCompare(b.id));
  const relations = (await store.list(scope, "relation"))
    .filter(current)
    .map((r) => [r.name, strip(r.data.from), strip(r.data.to)].join(" "))
    .sort();
  const types = (await store.list(scope, "objectType"))
    .filter(current)
    .map((t) => [t.name, t.data.objectCount]);
  return { objects, relations, types };
}
afterAll(async () => {
  for (const scope of scopes)
    for (const dir of ["incoming", "datasets", "results"])
      await rm(resolve(config.dataDir, dir, scope), {
        recursive: true,
        force: true,
      });
});
it("publishes with one executor query per type and relationship, exactly as the paged path did", async () => {
  const before = await workspace(),
    after = await workspace();
  const publishBefore = await before.draft(),
    publishAfter = await after.draft();
  vi.mocked(resultBatches).mockImplementation(paged);
  await publishBefore();
  vi.mocked(resultBatches).mockImplementation(actual.resultBatches);
  executor.mockClear();
  await publishAfter();
  // Two types and a relationship. The paged path made 6 + 14 + 14 calls,
  // each loading and profiling the whole table again.
  expect(executor.mock.calls.map(([job]) => job.mode)).toEqual([
    "materialize",
    "materialize",
    "materialize",
  ]);
  const expected = await published(before.scope),
    result = await published(after.scope);
  expect(result.objects).toHaveLength(10000);
  expect(result.relations).toHaveLength(6800);
  expect(result.types).toEqual([
    ["Customer", 3000],
    ["Order", 7000],
  ]);
  expect(result).toEqual(expected);
  expect(
    result.objects.find((o) => o.name === "Customer 9007199254743992")?.data
      .base,
  ).toEqual({
    id: "9007199254743992",
    name: "Customer 2999",
    region: "EU",
    score: 374.875,
    joined: "2026-03-15",
  });
  expect(
    await readdir(resolve(config.dataDir, "results", after.scope)),
  ).toEqual([]);
});
it("leaves the previous generation active when a result is incomplete", async () => {
  const { scope, draft } = await workspace();
  await (
    await draft()
  )();
  const active = (await store.get(scope, "workspace"))!.data.activeGeneration;
  // The executor reports more rows than its file holds.
  let publish = await draft();
  executor.mockImplementationOnce(async (job: any) => ({
    ...(await localExecute(job)),
    count: 3001,
  }));
  await expect(publish()).rejects.toThrow("Read 3000 of 3001 result rows");
  // A complete result with fewer rows than the dataset's snapshot.
  publish = await draft();
  executor.mockImplementationOnce(async (job: any) => {
    const r = await localExecute(job),
      file = resultFile(config.dataDir, scope, r.version);
    const lines = (await readFile(file, "utf8")).split("\n").slice(0, 100);
    await writeFile(file, lines.join("\n") + "\n");
    return { ...r, count: 100 };
  });
  await expect(publish()).rejects.toThrow(
    "Read 100 of 3000 Customer records. The workspace was not published.",
  );
  expect((await store.get(scope, "workspace"))!.data.activeGeneration).toBe(
    active,
  );
  expect(await readdir(resolve(config.dataDir, "results", scope))).toEqual([]);
});
it("sweeps result files that a stopped publication left behind", async () => {
  const scope = "sweep-" + randomUUID();
  scopes.push(scope);
  const dir = resolve(config.dataDir, "results", scope);
  await mkdir(dir, { recursive: true });
  await writeFile(resolve(dir, "orphan.jsonl"), "{}\n");
  await writeFile(resolve(dir, "reading.jsonl"), "{}\n");
  const old = new Date(Date.now() - 11 * 60000);
  await utimes(resolve(dir, "orphan.jsonl"), old, old);
  await sweepResults();
  expect(await readdir(dir)).toEqual(["reading.jsonl"]);
});
