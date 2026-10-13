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
  const scopes = [scope];
  installations.push({ app, scopes });
  const dataset = await ingestRows(store, scope, "History fixture", [
    { id: 1, value: "old" },
    { id: 2, value: "before" },
  ]);
  return { app, store, origin, scope, scopes, dataset };
}
function previewUrl(scope: string, dataset: string, version: string) {
  return `/api/v1/workspaces/${scope}/datasets/${dataset}/versions/${version}/preview`;
}
it("previews immutable older rows/profile after refresh without changing the active pointer", async () => {
  const { app, store, scope, dataset } = await fixture();
  const old = dataset.data.activeVersion;
  const refreshed = await ingestRows(
    store,
    scope,
    dataset.name,
    [{ id: 3, value: "new" }],
    undefined,
    dataset.id,
  );
  expect(refreshed.data.activeVersion).not.toBe(old);
  const before = await store.get(scope, dataset.id);
  const response = await app.inject({
    method: "POST",
    url: previewUrl(scope, dataset.id, old),
    payload: { offset: 1, limit: 1 },
  });
  expect(response.statusCode).toBe(200);
  expect(response.json()).toMatchObject({
    rows: [{ id: "2", value: "before" }],
    snapshot: { id: old, status: "historical", profile: { rows: 2 } },
    citations: [{ datasetId: dataset.id, version: old }],
    state: "dataset_snapshot",
  });
  expect(response.json().snapshot.createdAt).toBe(
    (await store.get(scope, old))!.createdAt,
  );
  expect(await store.get(scope, dataset.id)).toEqual(before);
  expect(calls.at(-1)).toMatchObject({
    mode: "query",
    inputs: [{ version: old }],
    limit: 1,
    offset: 1,
  });
  const current = await app.inject({
    method: "POST",
    url: `/api/v1/workspaces/${scope}/datasets/${dataset.id}/preview`,
    payload: {},
  });
  expect(current.json().rows).toEqual([{ id: "3", value: "new" }]);
});
it("shows staged schema-changing rows for review while current queries remain on the accepted schema", async () => {
  const { app, store, scope, dataset } = await fixture();
  const staged = await ingestRows(
    store,
    scope,
    dataset.name,
    [{ id: 4, value: "pending", extra: "new column" }],
    undefined,
    dataset.id,
  );
  const pending = staged.data.pendingVersion;
  expect(pending).toBeTruthy();
  const response = await app.inject({
    method: "POST",
    url: previewUrl(scope, dataset.id, pending),
    payload: {},
  });
  expect(response.statusCode).toBe(200);
  expect(response.json().snapshot.status).toBe("pending");
  expect(response.json().rows).toEqual([
    { id: "4", value: "pending", extra: "new column" },
  ]);
  expect(
    response.json().snapshot.profile.columns.map((c: any) => c.name),
  ).toEqual(["id", "value", "extra"]);
  expect((await store.get(scope, dataset.id))!.data.activeVersion).toBe(
    dataset.data.activeVersion,
  );
  expect((await store.get(scope, dataset.id))!.data.pendingVersion).toBe(
    pending,
  );
});
it("supports read-only SDK access with exact selected-version citations", async () => {
  const { app, origin, scope, dataset } = await fixture();
  const token = (
    await app.inject({
      method: "POST",
      url: `/api/v1/workspaces/${scope}/tokens`,
      payload: { name: "Read history", readOnly: true },
    })
  ).json().token;
  const sdk = new FoundryClient({ baseUrl: origin, workspaceId: scope, token });
  const result = await sdk.previewDatasetVersion(
    dataset.id,
    dataset.data.activeVersion,
    0,
    1,
  );
  expect(result.rows).toEqual([{ id: "1", value: "old" }]);
  expect(result.snapshot.status).toBe("current");
  expect(result.citations).toEqual([
    { datasetId: dataset.id, version: dataset.data.activeVersion },
  ]);
});
it("refuses versions from another dataset/workspace and datasets from retired generations before execution", async () => {
  const { app, store, scope, scopes, dataset } = await fixture();
  const other = await ingestRows(store, scope, "Other dataset", [
    { foreign: true },
  ]);
  const foreign = (
    await app.inject({
      method: "POST",
      url: "/api/v1/workspaces",
      payload: { name: "Foreign history" },
    })
  ).json().id;
  scopes.push(foreign);
  const external = await ingestRows(store, foreign, "Foreign dataset", [
    { secret: "fixture" },
  ]);
  calls.length = 0;
  for (const url of [
    previewUrl(scope, dataset.id, other.data.activeVersion),
    previewUrl(scope, dataset.id, external.data.activeVersion),
    previewUrl(foreign, dataset.id, dataset.data.activeVersion),
    previewUrl(scope, dataset.id, "missing-version"),
  ])
    expect(
      (await app.inject({ method: "POST", url, payload: {} })).statusCode,
    ).toBe(404);
  await transaction(store, scope, async (tx) =>
    tx.put({ ...dataset, data: { ...dataset.data, generation: "retired" } }),
  );
  expect(
    (
      await app.inject({
        method: "POST",
        url: previewUrl(scope, dataset.id, dataset.data.activeVersion),
        payload: {},
      })
    ).statusCode,
  ).toBe(404);
  expect(calls).toHaveLength(0);
});
it("returns typed schema/profile for an empty immutable snapshot and bounds pagination", async () => {
  const { app, store, scope } = await fixture();
  const dir = resolve(config.dataDir, "incoming", scope);
  await mkdir(dir, { recursive: true });
  await writeFile(resolve(dir, "empty.csv"), "");
  const empty = await ingestFile(
    store,
    scope,
    "Empty history",
    "empty.csv",
    undefined,
    undefined,
    undefined,
    [
      { name: "label", type: "string" },
      { name: "count", type: "integer" },
    ],
  );
  const url = previewUrl(scope, empty.id, empty.data.activeVersion);
  const response = await app.inject({ method: "POST", url, payload: {} });
  expect(response.statusCode).toBe(200);
  expect(response.json().rows).toEqual([]);
  expect(response.json().snapshot.profile.rows).toBe(0);
  expect(
    response.json().profile.columns.map((c: any) => [c.name, c.type]),
  ).toEqual([
    ["label", "VARCHAR"],
    ["count", "BIGINT"],
  ]);
  for (const body of [{ offset: -1 }, { limit: 0 }, { limit: 501 }])
    expect(
      (await app.inject({ method: "POST", url, payload: body })).statusCode,
    ).toBe(400);
});
it("retains the existing active-preview contract as an unchanged base control", async () => {
  const { app, scope, dataset } = await fixture();
  const response = await app.inject({
    method: "POST",
    url: `/api/v1/workspaces/${scope}/datasets/${dataset.id}/preview`,
    payload: { limit: 1 },
  });
  expect(response.statusCode).toBe(200);
  expect(response.json().rows).toEqual([{ id: "1", value: "old" }]);
  expect(response.json().citations).toEqual([
    { datasetId: dataset.id, version: dataset.data.activeVersion },
  ]);
});
