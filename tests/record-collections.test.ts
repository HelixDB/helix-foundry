import { beforeAll, afterAll, afterEach, expect, it } from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { rm, readFile } from "node:fs/promises";
import { datasetTable } from "../packages/shared/src/index.js";
import { FoundryClient } from "../packages/sdk/src/index.js";
import { resolve } from "node:path";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { config } from "../apps/api/src/config.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore } from "../apps/api/src/store.js";

// These are existing public API controls for the graph UI integration.
// Its missing navigation path is reproduced by the installed-browser test.
const originalExecutor = config.executor;
const executor = createServer(async (req, res) => {
  try {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(chunk);
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
  await new Promise<void>((done) => executor.close(() => done()));
});
let app: Awaited<ReturnType<typeof createApp>> | undefined;
let scope = "";
let store: MemoryStore;
afterEach(async () => {
  await app?.close();
  for (const kind of ["incoming", "datasets", "results"])
    await rm(resolve(config.dataDir, kind, scope), {
      recursive: true,
      force: true,
    });
});
async function fixture(count = 3) {
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  scope = (await app.inject("/api/v1/me")).json().workspaces[0].id;
  const base = `/api/v1/workspaces/${scope}`;
  const request = async (method: any, path: string, payload?: unknown) => {
    const response = await app!.inject({
      method,
      url: base + path,
      payload: payload as any,
    });
    expect(response.statusCode, response.body).toBe(200);
    return response.json();
  };
  const dataset = await request("POST", "/ingest", {
    name: "People",
    rows: Array.from({ length: count }, (_, i) => ({
      id: i,
      name: `Search person ${String(i).padStart(4, "0")}`,
      region: "West",
    })),
  });
  const bundle = {
    summary: "People",
    assumptions: [],
    catalog: [],
    pipelines: [],
    ontology: [
      {
        name: "Person",
        datasetId: dataset.id,
        primaryKey: "id",
        properties: ["id", "name", "region"],
      },
    ],
    relationships: [],
    dashboards: [],
    actions: [],
  };
  const publish = async () => {
    const proposal = (
      await request("POST", "/definitions/import", {
        version: 1,
        proposals: [bundle],
      })
    ).items[0];
    expect(proposal.data.validation.valid).toBe(true);
    return request("POST", `/proposals/${proposal.id}/publish`, {
      hash: proposal.data.hash,
    });
  };
  await publish();
  const raw = (method: any, path: string, payload?: unknown) =>
    app!.inject({ method, url: base + path, payload: payload as any });
  const origin = await app.listen({ port: 0, host: "127.0.0.1" });
  return { request, raw, dataset, publish, base, origin, bundle };
}

async function create(
  f: Awaited<ReturnType<typeof fixture>>,
  name = "Follow up",
) {
  return f.request("POST", "/record-collections", { name });
}
async function update(
  f: Awaited<ReturnType<typeof fixture>>,
  c: any,
  logicalIds: string[],
  name = c.name,
) {
  return f.request("PUT", `/record-collections/${c.id}`, {
    revision: c.revision,
    name,
    logicalIds,
  });
}
it("curates, renames and deletes a collection through actual HTTP SDK without changing source data", async () => {
  const f = await fixture();
  const token = (
    await f.request("POST", "/tokens", {
      name: "Collection SDK",
      readOnly: false,
    })
  ).token;
  const client = new FoundryClient({
    baseUrl: f.origin,
    workspaceId: scope,
    token,
  });
  const records = (await f.request("GET", "/resources/object?limit=2")).items;
  const parquet = resolve(
    config.dataDir,
    "datasets",
    scope,
    f.dataset.data.activeVersion + ".parquet",
  );
  const bytes = await readFile(parquet);
  const before = await f.request("POST", "/query", {
    sql: `SELECT count(*) AS n FROM "${datasetTable(f.dataset.id)}"`,
    inputs: [f.dataset.id],
  });
  let c = await client.createRecordCollection("  Follow up  ");
  expect(c.name).toBe("Follow up");
  c = await client.updateRecordCollection(
    c.id,
    c.revision,
    "Priority",
    records.map((r: any) => r.data.logicalId),
  );
  const detail = await client.recordCollection(c.id);
  expect(detail.members).toHaveLength(2);
  expect(detail.members.every((m) => m.available)).toBe(true);
  expect(detail.name).toBe("Priority");
  expect((await client.recordCollections()).items.map((c) => c.id)).toContain(
    c.id,
  );
  c = await client.updateRecordCollection(c.id, c.revision, c.name, [
    records[1].data.logicalId,
  ]);
  expect(c.logicalIds).toHaveLength(1);
  expect(await client.deleteRecordCollection(c.id, c.revision)).toEqual({
    deleted: true,
  });
  await expect(client.recordCollection(c.id)).rejects.toMatchObject({
    status: 404,
  });
  const after = await f.request("POST", "/query", {
    sql: `SELECT count(*) AS n FROM "${datasetTable(f.dataset.id)}"`,
    inputs: [f.dataset.id],
  });
  expect(after.rows).toEqual(before.rows);
  expect(await readFile(parquet)).toEqual(bytes);
  expect(
    (await f.request("GET", `/resources/dataset/${f.dataset.id}`)).data,
  ).toEqual(f.dataset.data);
});
it("read-only SDK can browse collection identities but cannot create, change or delete them", async () => {
  const f = await fixture();
  const c = await create(f),
    r = (await f.request("GET", "/resources/object?limit=1")).items[0];
  const saved = await update(f, c, [r.data.logicalId]);
  const token = (
    await f.request("POST", "/tokens", {
      name: "Read collections",
      readOnly: true,
    })
  ).token;
  const viewer = new FoundryClient({
    baseUrl: f.origin,
    workspaceId: scope,
    token,
  });
  expect((await viewer.recordCollection(saved.id)).members[0].available).toBe(
    true,
  );
  expect((await viewer.recordCollections()).items).toHaveLength(1);
  await expect(viewer.createRecordCollection("No")).rejects.toMatchObject({
    status: 403,
  });
  await expect(
    viewer.updateRecordCollection(saved.id, saved.revision, "No", []),
  ).rejects.toMatchObject({ status: 403 });
  await expect(
    viewer.deleteRecordCollection(saved.id, saved.revision),
  ).rejects.toMatchObject({ status: 403 });
});
it("refuses duplicate, unknown, foreign and wrong-kind identities without replacing a collection", async () => {
  const f = await fixture(),
    c = await create(f),
    r = (await f.request("GET", "/resources/object?limit=1")).items[0];
  for (const [logicalIds, status] of [
    [[r.data.logicalId, r.data.logicalId], 400],
    [["missing-record"], 409],
  ] as const) {
    expect(
      (
        await f.raw("PUT", `/record-collections/${c.id}`, {
          revision: c.revision,
          name: c.name,
          logicalIds,
        })
      ).statusCode,
    ).toBe(status);
  }
  expect((await f.raw("GET", "/record-collections/workspace")).statusCode).toBe(
    404,
  );
  expect(
    (await app!.inject(`/api/v1/workspaces/foreign/record-collections/${c.id}`))
      .statusCode,
  ).toBe(404);
  expect(
    (
      await f.raw("POST", "/record-collections", {
        name: "x",
        credential: "forbidden",
      })
    ).statusCode,
  ).toBe(400);
  expect(
    (await f.request("GET", `/record-collections/${c.id}`)).logicalIds,
  ).toEqual([]);
});
it("serializes concurrent collection edits and rejects stale delete revisions", async () => {
  const f = await fixture(),
    c = await create(f),
    r = (await f.request("GET", "/resources/object?limit=1")).items[0];
  const replies = await Promise.all(
    ["First", "Second"].map((name) =>
      f.raw("PUT", `/record-collections/${c.id}`, {
        revision: c.revision,
        name,
        logicalIds: [r.data.logicalId],
      }),
    ),
  );
  expect(replies.map((r) => r.statusCode).sort()).toEqual([200, 409]);
  expect(
    (
      await f.raw("DELETE", `/record-collections/${c.id}`, {
        revision: c.revision,
      })
    ).statusCode,
  ).toBe(409);
  expect((await f.request("GET", `/record-collections/${c.id}`)).revision).toBe(
    2,
  );
});
it("keeps identities through real republication and resolves current human edits", async () => {
  const f = await fixture(),
    c = await create(f),
    r = (await f.request("GET", "/resources/object?limit=1")).items[0];
  const saved = await update(f, c, [r.data.logicalId]);
  const before = await f.request("GET", `/record-collections/${c.id}`);
  await f.publish();
  const next = await f.request("GET", `/record-collections/${c.id}`);
  expect(next.generation).not.toBe(before.generation);
  expect(next.members[0].available).toBe(true);
  expect(next.logicalIds).toEqual(saved.logicalIds);
  expect(next.revision).toBe(saved.revision);
  const current = (await f.request("GET", `/records/${r.data.logicalId}`))
      .record,
    values = { name: "Human renamed record" };
  const p = await f.request("POST", "/actions/preview", {
    objectId: current.id,
    values,
  });
  await f.request("POST", "/actions/execute", {
    objectId: current.id,
    values,
    revision: p.revision,
    idempotencyKey: "collection-native-rename",
  });
  expect(
    (await f.request("GET", `/record-collections/${c.id}`)).members[0].name,
  ).toBe(values.name);
});
it("retains unavailable members explicitly after actual source refresh and removes them without a lost draft", async () => {
  const f = await fixture(),
    c = await create(f),
    r = (await f.request("GET", "/resources/object?limit=1")).items[0];
  let saved = await update(f, c, [r.data.logicalId]);
  await f.request("POST", "/ingest", {
    name: "People",
    datasetId: f.dataset.id,
    rows: [{ id: 9000, name: "Replacement record", region: "East" }],
  });
  await f.publish();
  const detail = await f.request("GET", `/record-collections/${c.id}`);
  expect(detail.members).toEqual([
    { logicalId: r.data.logicalId, available: false, name: null, type: null },
  ]);
  saved = await update(f, saved, saved.logicalIds, "Still followed");
  expect(saved.logicalIds).toEqual([r.data.logicalId]);
  saved = await update(f, saved, []);
  expect(saved.logicalIds).toEqual([]);
});
it("enforces bounded collections and members using actual published records", async () => {
  const f = await fixture(201);
  let c = await create(f);
  const records = (await f.request("GET", "/resources/object?limit=500")).items;
  expect(records).toHaveLength(201);
  c = await update(
    f,
    c,
    records.slice(0, 200).map((r: any) => r.data.logicalId),
  );
  expect(
    (await f.request("GET", `/record-collections/${c.id}`)).members,
  ).toHaveLength(200);
  expect(
    (
      await f.raw("PUT", `/record-collections/${c.id}`, {
        revision: c.revision,
        name: c.name,
        logicalIds: records.map((r: any) => r.data.logicalId),
      })
    ).statusCode,
  ).toBe(400);
  for (let i = 1; i < 40; i++) await create(f, `Collection ${i}`);
  expect((await f.request("GET", "/record-collections")).items).toHaveLength(
    40,
  );
  expect(
    (await f.raw("POST", "/record-collections", { name: "Too many" }))
      .statusCode,
  ).toBe(409);
});
it("preserves the existing native record/query contract without collections", async () => {
  const f = await fixture();
  const r = (await f.request("GET", "/resources/object?limit=1")).items[0];
  expect(
    (await f.request("GET", `/records/${r.data.logicalId}`)).record.data
      .effective.region,
  ).toBe("West");
  expect(
    (
      await f.request("POST", "/query", {
        sql: `SELECT count(*) AS n FROM "${datasetTable(f.dataset.id)}"`,
        inputs: [f.dataset.id],
      })
    ).rows,
  ).toEqual([{ n: "3" }]);
});

it("refuses an added record when an actual publication wins the collection commit race", async () => {
  const f = await fixture(),
    c = await create(f),
    r = (await f.request("GET", "/resources/object?limit=1")).items[0];
  const original = store.commit.bind(store);
  let intervene = true;
  store.commit = async (scope, version, changes) => {
    if (
      intervene &&
      changes.some(
        (x) =>
          "put" in x && x.put.kind === "recordCollection" && x.put.id === c.id,
      )
    ) {
      intervene = false;
      f.bundle.ontology[0].name = "ReplacementPerson";
      await f.publish();
    }
    return original(scope, version, changes);
  };
  const result = await f.raw("PUT", `/record-collections/${c.id}`, {
    revision: c.revision,
    name: c.name,
    logicalIds: [r.data.logicalId],
  });
  expect(result.statusCode, result.body).toBe(409);
  expect(intervene).toBe(false);
  expect(
    (await f.request("GET", `/record-collections/${c.id}`)).logicalIds,
  ).toEqual([]);
  expect(
    (await f.request("GET", "/resources/object?limit=1")).items[0].data.type,
  ).toBe("ReplacementPerson");
});
