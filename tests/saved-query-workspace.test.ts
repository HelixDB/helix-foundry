import { beforeAll, afterAll, afterEach, expect, it } from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { config } from "../apps/api/src/config.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore } from "../apps/api/src/store.js";
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
  const dataset = await ingestRows(store, scope, "Saved query inputs", [
    { category: "A", profit: 10 },
    { category: "B", profit: -5 },
  ]);
  const base = `/api/v1/workspaces/${scope}`;
  const sql = `SELECT category, sum(profit) profit FROM ${dataset.data.table} GROUP BY category ORDER BY category`;
  const saved = await app.inject({
    method: "POST",
    url: base + "/queries",
    payload: { name: "Profit query", inputs: [dataset.id], sql },
  });
  expect(saved.statusCode, saved.body).toBe(200);
  return { store, app, origin, scope, dataset, base, query: saved.json(), sql };
}
it("reopens a saved query against current accepted snapshots after refresh without mutating its definition", async () => {
  const { store, app, scope, dataset, base, query } = await fixture();
  const before = await store.get(scope, query.id);
  const refresh = await ingestRows(
    store,
    scope,
    dataset.name,
    [
      { category: "A", profit: 40 },
      { category: "B", profit: -20 },
    ],
    undefined,
    dataset.id,
  );
  const r = await app.inject({
    method: "POST",
    url: base + `/queries/${query.id}/run`,
    payload: { offset: 1, limit: 1 },
  });
  expect(r.statusCode, r.body).toBe(200);
  expect(r.json()).toMatchObject({
    rows: [{ category: "B", profit: -20 }],
    profile: { rows: 2 },
    query: { id: query.id, name: query.name, revision: query.revision },
    citations: [{ datasetId: dataset.id, version: refresh.data.activeVersion }],
  });
  expect(await store.get(scope, query.id)).toEqual(before);
});
it("edits SQL/name with revision protection and reruns the new native query", async () => {
  const { store, app, scope, dataset, base, query } = await fixture();
  const sql = `SELECT sum(profit) total FROM ${dataset.data.table}`;
  const edit = {
    name: "Net profit",
    inputs: [dataset.id],
    sql,
    revision: query.revision,
  };
  const changed = await app.inject({
    method: "PUT",
    url: base + `/queries/${query.id}`,
    payload: edit,
  });
  expect(changed.statusCode, changed.body).toBe(200);
  expect(changed.json()).toMatchObject({
    name: "Net profit",
    revision: query.revision + 1,
    data: { sql },
  });
  const stale = await app.inject({
    method: "PUT",
    url: base + `/queries/${query.id}`,
    payload: { ...edit, name: "Stale overwrite" },
  });
  expect(stale.statusCode).toBe(409);
  expect((await store.get(scope, query.id))!.name).toBe("Net profit");
  const r = await app.inject({
    method: "POST",
    url: base + `/queries/${query.id}/run`,
    payload: {},
  });
  expect(r.statusCode, r.body).toBe(200);
  expect(r.json().rows).toEqual([{ total: 5 }]);
});
it("refuses mutating SQL during edits without changing the saved definition or rows", async () => {
  const { store, app, scope, dataset, base, query } = await fixture();
  const before = await store.get(scope, query.id);
  const edit = await app.inject({
    method: "PUT",
    url: base + `/queries/${query.id}`,
    payload: {
      name: "Unsafe",
      inputs: [dataset.id],
      sql: `DELETE FROM ${dataset.data.table}`,
      revision: query.revision,
    },
  });
  expect(edit.statusCode).toBe(422);
  expect(edit.json().error).toContain("SELECT");
  expect(await store.get(scope, query.id)).toEqual(before);
  const r = await app.inject({
    method: "POST",
    url: base + "/query",
    payload: {
      inputs: [dataset.id],
      sql: `SELECT count(*) n FROM ${dataset.data.table}`,
    },
  });
  expect(r.json().rows).toEqual([{ n: "2" }]);
});
it("supports the real SDK saved-query lifecycle and read-only token execution", async () => {
  const { app, origin, scope, dataset, base, query, sql } = await fixture();
  const rw = (
    await app.inject({
      method: "POST",
      url: base + "/tokens",
      payload: { name: "Editor", readOnly: false },
    })
  ).json().token;
  const client = new FoundryClient({
    baseUrl: origin,
    workspaceId: scope,
    token: rw,
  });
  const q = await client.saveQuery("SDK query", [dataset.id], sql);
  expect((await client.savedQueries()).items.map((x) => x.id)).toContain(q.id);
  expect((await client.runSavedQuery(q.id)).rows).toHaveLength(2);
  const update = await client.updateQuery(
    q.id,
    q.revision,
    "SDK renamed",
    [dataset.id],
    sql,
  );
  expect(update.name).toBe("SDK renamed");
  const ro = (
    await app.inject({
      method: "POST",
      url: base + "/tokens",
      payload: { name: "Viewer", readOnly: true },
    })
  ).json().token;
  const viewer = new FoundryClient({
    baseUrl: origin,
    workspaceId: scope,
    token: ro,
  });
  expect((await viewer.runSavedQuery(query.id)).rows).toHaveLength(2);
  await expect(
    viewer.updateQuery(query.id, query.revision, "Denied", [dataset.id], sql),
  ).rejects.toMatchObject({ status: 403 });
  await expect(viewer.deleteQuery(q.id)).rejects.toMatchObject({ status: 403 });
  await client.deleteQuery(q.id);
  await expect(client.runSavedQuery(q.id)).rejects.toMatchObject({
    status: 404,
  });
});
it("rejects foreign query IDs and invalid page bounds before native execution", async () => {
  const { app, base, query } = await fixture();
  const other = (
    await app.inject({
      method: "POST",
      url: "/api/v1/workspaces",
      payload: { name: "Foreign" },
    })
  ).json();
  for (const method of ["POST", "PUT", "DELETE"] as const) {
    const payload =
      method === "PUT"
        ? { name: "foreign", inputs: ["absent"], sql: "SELECT 1", revision: 1 }
        : {};
    const r = await app.inject({
      method,
      url:
        `/api/v1/workspaces/${other.id}/queries/${query.id}` +
        (method === "POST" ? "/run" : ""),
      payload,
    });
    expect(r.statusCode).toBe(404);
  }
  const r = await app.inject({
    method: "POST",
    url: base + `/queries/${query.id}/run`,
    payload: { limit: 501 },
  });
  expect(r.statusCode).toBe(400);
});
it("retains the unchanged public read-only query contract as a baseline positive control", async () => {
  const { app, dataset, base } = await fixture();
  const r = await app.inject({
    method: "POST",
    url: base + "/query",
    payload: {
      inputs: [dataset.id],
      sql: `SELECT sum(profit) total FROM ${dataset.data.table}`,
    },
  });
  expect(r.statusCode, r.body).toBe(200);
  expect(r.json().rows).toEqual([{ total: 5 }]);
  const unsafe = await app.inject({
    method: "POST",
    url: base + "/query",
    payload: { inputs: [dataset.id], sql: `DELETE FROM ${dataset.data.table}` },
  });
  expect(unsafe.statusCode).toBe(422);
});
