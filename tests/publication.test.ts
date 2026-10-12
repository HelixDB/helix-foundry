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
import {
  DatabaseError,
  MemoryStore,
  resource,
  transaction,
} from "../apps/api/src/store.js";
import { runCleanupJob } from "../apps/api/src/cleanup.js";
import {
  listRecords,
  snapshotNeighbours,
  type RecordQuery,
} from "../apps/api/src/records.js";
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
      // Pages of a query are only consistent in a total order.
      sql: `SELECT * FROM (${job.sql}) AS paged ORDER BY ALL`,
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
  await expect(publish()).rejects.toThrow(
    "Could not read Customer records. Read 3000 of 3001 result rows",
  );
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
it("leaves the previous generation active when a relationship or staging fails", async () => {
  const { scope, draft } = await workspace();
  await (
    await draft()
  )();
  const active = (await store.get(scope, "workspace"))!.data.activeGeneration;
  // The relationship's result is over the executor's limit.
  let publish = await draft();
  executor.mockImplementation(async (job: any) => {
    if (!job.sql.includes("from_key")) return localExecute(job);
    vi.stubEnv("EXECUTOR_RESULT_LIMIT", "100KB");
    try {
      return await localExecute(job);
    } finally {
      vi.unstubAllEnvs();
    }
  });
  try {
    await expect(publish()).rejects.toThrow(
      "Could not read the Placed by relationship. Result exceeds EXECUTOR_RESULT_LIMIT (100KB)",
    );
  } finally {
    executor.mockImplementation((job: any) => localExecute(job));
  }
  // HelixDB fails while a type's records are being staged.
  publish = await draft();
  const stage = store.stageObjects.bind(store);
  let batches = 0;
  const failing = vi
    .spyOn(store, "stageObjects")
    .mockImplementation(async (...args) => {
      if (++batches === 4) throw new Error("HelixDB is unavailable");
      return stage(...args);
    });
  try {
    await expect(publish()).rejects.toThrow("HelixDB is unavailable");
  } finally {
    failing.mockRestore();
  }
  expect(batches).toBe(4);
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
  // Not a result file, and cannot be removed as one.
  await mkdir(resolve(dir, "stray"));
  const old = new Date(Date.now() - 11 * 60000);
  for (const name of ["orphan.jsonl", "stray"])
    await utimes(resolve(dir, name), old, old);
  await sweepResults();
  expect((await readdir(dir)).sort()).toEqual(["reading.jsonl", "stray"]);
});
it("resumes a publication the database interrupted, keeping what it wrote", async () => {
  const { scope, draft } = await workspace();
  const publish = await draft();
  const stage = store.stageObjects.bind(store);
  let batches = 0;
  const spy = vi
    .spyOn(store, "stageObjects")
    .mockImplementation(async (s, objects, resume) => {
      if (++batches === 5)
        throw new DatabaseError(
          "HelixDB is not accepting connections",
          "ECONNREFUSED",
          true,
        );
      return stage(s, objects, resume);
    });
  await expect(publish()).rejects.toThrow("not accepting connections");
  const [staging] = await store.list(scope, "staging");
  expect(staging.data.status).toBe("staging");
  const generation = staging.data.generation,
    staged = async () =>
      (await store.list(scope, "object")).filter(
        (o) => o.data.generation === generation,
      );
  // Batches already in flight when it failed may have landed too.
  const partial = (await staged()).length;
  expect(partial).toBeGreaterThanOrEqual(2000);
  expect(partial).toBeLessThan(10000);
  // The same proposal again: the same generation, skipping what is there.
  const resumed: boolean[] = [];
  spy.mockImplementation(async (s, objects, resume) => {
    resumed.push(resume);
    return stage(s, objects, resume);
  });
  try {
    await publish();
  } finally {
    spy.mockRestore();
  }
  expect(resumed.length).toBeGreaterThan(0);
  expect(resumed.every(Boolean)).toBe(true);
  expect((await store.get(scope, "workspace"))!.data.activeGeneration).toBe(
    generation,
  );
  expect(await staged()).toHaveLength(10000);
  expect(
    (await store.list(scope, "relation")).filter(
      (r) => r.data.generation === generation,
    ),
  ).toHaveLength(6800);
  expect((await store.get(scope, staging.id))!.data.status).toBe("active");
});

it("removes failed, replaced and leftover generations, and keeps the active one", async () => {
  const { scope, draft } = await workspace();
  await (
    await draft()
  )();
  const first = (await store.get(scope, "workspace"))!.data.activeGeneration;
  // An attempt that fails for good leaves 1,000 staged records.
  let publish = await draft();
  const stage = store.stageObjects.bind(store);
  let batches = 0;
  const spy = vi
    .spyOn(store, "stageObjects")
    .mockImplementation(async (s, objects, resume) => {
      if (++batches === 3) throw new Error("Unexpected record");
      return stage(s, objects, resume);
    });
  await expect(publish()).rejects.toThrow("Unexpected record");
  spy.mockRestore();
  const failed = (await store.list(scope, "staging")).find(
    (s) => s.data.status === "abandoned",
  )!.data.generation;
  // Records an earlier release staged and never removed, which no resource
  // names.
  const leftover = randomUUID();
  await transaction(store, scope, async (tx) => {
    for (const id of ["a", "b"])
      tx.put(
        resource(
          scope,
          "object",
          "Old " + id,
          { generation: leftover },
          `${leftover}_${id}`,
        ),
      );
    tx.put(
      resource(scope, "relation", "Old link", {
        from: `${leftover}_a`,
        to: `${leftover}_b`,
        generation: leftover,
      }),
    );
  });
  publish = await draft();
  await publish();
  const second = (await store.get(scope, "workspace"))!.data.activeGeneration;
  const count = async (generation: string) =>
    (await store.list(scope))
      .filter((r) => ["object", "relation"].includes(r.kind))
      .filter((r) => r.data.generation === generation).length;
  expect(await count(failed)).toBe(1000);
  const job = (await store.list(scope, "job")).find(
    (j) => j.data.type === "cleanup" && j.data.status === "queued",
  )!;
  // Until HelixDB has built the indexes it reads, it waits.
  const building = vi.spyOn(store, "indexesReady").mockResolvedValue(false);
  expect(await runCleanupJob(store, scope, job)).toEqual({
    again: true,
    retryIn: 60_000,
  });
  building.mockRestore();
  expect(await count(failed)).toBe(1000);
  // It comes back once the replaced generation's grace ends.
  const turn = await runCleanupJob(store, scope, job);
  expect(turn.again).toBe(true);
  expect(turn.retryIn).toBeGreaterThan(9 * 60_000);
  expect(await count(failed)).toBe(0);
  expect(await count(leftover)).toBe(0);
  // The replaced generation stays a few minutes for requests still reading it.
  expect(await count(first)).toBe(16800);
  expect(await count(second)).toBe(16800);
  await transaction(store, scope, async (tx) => {
    for (const p of await tx.list("publication"))
      tx.put({
        ...p,
        createdAt: new Date(Date.now() - 3600_000).toISOString(),
      });
  });
  expect(await runCleanupJob(store, scope, job)).toEqual({ again: false });
  expect(await count(first)).toBe(0);
  expect(await count(second)).toBe(16800);
  expect((await store.get(scope, "cleanup_state"))!.data.swept).toBe(true);
});

it("lists records from the snapshot a page at a time, with workspace edits first", async () => {
  const { scope, draft } = await workspace();
  await (
    await draft()
  )();
  const list = vi.spyOn(store, "list");
  const records = (query: Partial<RecordQuery>) =>
    listRecords(store, scope, { offset: 0, limit: 20, ...query });
  try {
    const found = await records({ type: "Customer", search: "customer 2999" });
    expect(found.total).toBe(1);
    expect(found.items[0].data.effective.name).toBe("Customer 2999");
    expect(
      (await records({ type: "Customer", field: "region", value: "EU" })).total,
    ).toBe(2000);
    const scores = (
      await records({ type: "Customer", sort: "score", dir: "desc", limit: 3 })
    ).items.map((o) => o.data.effective.score);
    expect(scores).toEqual([374.875, 374.75, 374.625]);
    // Customers below 800 are placed by three orders, the rest by two.
    const linked = await records({
      type: "Customer",
      sort: "links",
      dir: "desc",
    });
    expect(linked.items.map((o) => o.data.links)).toEqual(Array(20).fill(3));
    // Sorted by links, the snapshot's counts are shown: the database counts
    // only a record whose links were edited.
    const counts = vi.spyOn(store, "linkCounts");
    const byLinks = () =>
      records({ type: "Customer", sort: "links", dir: "desc" });
    expect((await byLinks()).items.map((o) => o.data.links)).toEqual(
      Array(20).fill(3),
    );
    expect(counts.mock.calls.flatMap(([, ids]) => ids)).toEqual([]);
    await transaction(store, scope, async (tx) =>
      tx.put(
        resource(
          scope,
          "relationEdit",
          "Placed by",
          {
            fromLogicalId: "unpublished",
            toLogicalId: linked.items[0].data.logicalId,
            enabled: false,
          },
          "relationEdit_links",
        ),
      ),
    );
    counts.mockClear();
    expect((await byLinks()).items.map((o) => o.data.links)).toEqual(
      Array(20).fill(3),
    );
    expect(counts.mock.calls.flatMap(([, ids]) => ids)).toEqual([
      linked.items[0].id,
    ]);
    counts.mockRestore();
    await transaction(store, scope, async (tx) =>
      tx.delete("relationEdit_links"),
    );
    const last = await records({ type: "Customer", offset: 2990 });
    expect(last.total).toBe(3000);
    expect(last.items).toHaveLength(10);
    const both = await records({ limit: 5 });
    expect(both.total).toBe(10000);
    // An edited record is listed first, with its edited values.
    const edited = found.items[0];
    await transaction(store, scope, async (tx) =>
      tx.put(
        resource(
          scope,
          "overlay",
          edited.name,
          { values: { region: "Mars" } },
          "overlay_" + edited.data.logicalId,
        ),
      ),
    );
    const mars = await records({
      type: "Customer",
      field: "region",
      value: "Mars",
    });
    expect(mars.total).toBe(1);
    expect(mars.items[0].data.edits).toEqual({ region: "Mars" });
    const all = await records({ type: "Customer" });
    expect(all.total).toBe(3000);
    expect(all.items[0].id).toBe(edited.id);
    // No request read every record or link.
    expect(
      list.mock.calls.filter(([, kind]) =>
        ["object", "relation", undefined].includes(kind),
      ),
    ).toEqual([]);
  } finally {
    list.mockRestore();
  }
});
it("finds the best-connected records' neighbours in the snapshot, linked as publication linked them", async () => {
  const { scope, draft } = await workspace();
  await (
    await draft()
  )();
  const best = (
    await listRecords(store, scope, {
      sort: "links",
      dir: "desc",
      offset: 0,
      limit: 3,
    })
  ).items;
  const seeds = best.map((record) => ({ record, links: record.data.links }));
  const walked = await store.neighborhood(
    scope,
    best.map((o) => o.id),
    1,
    100_000,
  );
  const key = (e: { from: string; to: string; name: string }) =>
    [e.from, e.to, e.name].join(":");
  const real = new Set(walked.edges.map(key));
  // Three customers placed by three orders each share six places.
  const near = await snapshotNeighbours(store, scope, seeds, 6);
  expect(near.unmatched).toEqual([]);
  expect(near.truncated).toBe(true);
  expect(near.links).toHaveLength(6);
  expect(near.links.every((e) => real.has(key(e)))).toBe(true);
  expect(new Set(near.ids)).toEqual(new Set(near.links.map((e) => e.from)));
  // With room for all of them, they are every link HelixDB walks.
  const all = await snapshotNeighbours(store, scope, seeds, 100);
  expect(all.truncated).toBe(false);
  expect(new Set(all.links.map(key))).toEqual(real);
  // Their links are counted with them, as HelixDB counts them.
  expect(all.degrees).toEqual(
    await store.linkCounts(scope, [...all.degrees.keys()]),
  );
  expect([...all.degrees.keys()].sort()).toEqual([...all.ids].sort());
  // A pair with a relation edit is left out; an edit adding a link is in.
  const generation = best[0].id.slice(0, best[0].id.indexOf("_") + 1),
    logical = (id: string) => id.slice(generation.length),
    [dropped] = all.links,
    other = all.links.find((e) => e.to !== dropped.to)!;
  await transaction(store, scope, async (tx) => {
    tx.put(
      resource(
        scope,
        "relationEdit",
        dropped.name,
        {
          fromLogicalId: logical(dropped.from),
          toLogicalId: logical(dropped.to),
          enabled: false,
        },
        "relationEdit_dropped",
      ),
    );
    tx.put(
      resource(
        scope,
        "relationEdit",
        "Placed by",
        {
          fromLogicalId: logical(other.from),
          toLogicalId: logical(dropped.to),
          enabled: true,
        },
        "relationEdit_added",
      ),
    );
  });
  const edited = await snapshotNeighbours(store, scope, seeds, 100);
  expect(edited.links.map(key)).not.toContain(key(dropped));
  expect(edited.links).toContainEqual({
    from: other.from,
    to: dropped.to,
    name: "Placed by",
  });
  expect(edited.ids).toContain(other.from);
  // Records with an edit are left for HelixDB to count.
  expect(edited.degrees.has(other.from)).toBe(false);
  expect(edited.degrees.has(dropped.from)).toBe(false);
});
