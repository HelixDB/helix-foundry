import { beforeAll, afterAll, afterEach, expect, it } from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { config } from "../apps/api/src/config.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, transaction } from "../apps/api/src/store.js";
import { startWorker } from "../apps/api/src/jobs.js";
import { datasetTable } from "../packages/shared/src/index.js";
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
  const base = `/api/v1/workspaces/${scope}`;
  const post = async (path: string, payload: any) => {
    const r = await app.inject({ method: "POST", url: base + path, payload });
    expect(r.statusCode, r.body).toBe(200);
    return r.json();
  };
  const ds = await post("/ingest", {
    name: "Pipeline inputs",
    rows: [
      { category: "A", profit: 10 },
      { category: "B", profit: -5 },
    ],
  });
  const token = await post("/tokens", {
    name: "Pipeline builder",
    readOnly: false,
  });
  const client = new FoundryClient({
    baseUrl: origin,
    workspaceId: scope,
    token: token.token,
  });
  const definition = (factor = 2) => ({
    name: "Scaled profit",
    description: "Explicit human transform",
    inputs: [ds.id],
    sql: `SELECT category,profit*${factor} profit FROM ${datasetTable(ds.id)} ORDER BY category`,
    nodes: [],
    checks: [],
  });
  const accepted = async () =>
    (await app.inject(base + "/resources/pipeline")).json().items[0];
  const publish = async (p: any) => {
    expect(p.data.validation.valid).toBe(true);
    return post(`/proposals/${p.id}/publish`, { hash: p.data.hash });
  };
  const preview = async (pipeline: any) =>
    post(`/datasets/${pipeline.data.outputDatasetId}/preview`, {});
  return {
    store,
    app,
    scope,
    base,
    post,
    ds,
    client,
    definition,
    accepted,
    publish,
    preview,
  };
}
it("first human pipeline review excludes unrelated unaccepted proposal definitions", async () => {
  const f = await fixture();
  const prior = (
    await f.post("/definitions/import", {
      version: 1,
      proposals: [
        {
          summary: "Unaccepted suggestion",
          assumptions: [],
          catalog: [],
          pipelines: [],
          relationships: [],
          dashboards: [],
          actions: [],
          ontology: [
            {
              name: "Unaccepted",
              datasetId: f.ds.id,
              primaryKey: "category",
              properties: ["category"],
            },
          ],
        },
      ],
    })
  ).items[0];
  expect(prior.data.validation.valid).toBe(true);
  const p = await f.client.draftPipeline(f.definition());
  expect(p.data.validation.valid).toBe(true);
  expect(p.data.bundle.ontology).toEqual([]);
  expect(p.data.bundle.pipelines).toEqual([f.definition()]);
  expect((await f.store.get(f.scope, prior.id))?.data.status).not.toBe(
    "published",
  );
  await f.publish(p);
  expect(
    (await f.app.inject(f.base + "/resources/objectType")).json().items,
  ).toEqual([]);
});
it("public draft review publication and durable worker refresh produce exact native immutable outputs", async () => {
  const f = await fixture();
  const p = await f.client.draftPipeline(f.definition());
  await f.publish(p);
  const pipeline = await f.accepted();
  const first = await f.preview(pipeline);
  expect(first.rows.map((r: any) => r.profit)).toEqual([20, -10]);
  const old = await f.store.get(f.scope, pipeline.data.outputDatasetId),
    generation = (await f.store.get(f.scope, "workspace"))?.data
      .activeGeneration;
  await f.post("/ingest", {
    name: f.ds.name,
    datasetId: f.ds.id,
    rows: [
      { category: "A", profit: 40 },
      { category: "B", profit: -20 },
    ],
  });
  const job = await f.client.runPipeline(pipeline.id);
  expect(job.data.status).toBe("queued");
  const stop = startWorker(f.store);
  try {
    await expect
      .poll(async () => (await f.store.get(f.scope, job.id))?.data.status, {
        timeout: 10000,
      })
      .toBe("succeeded");
  } finally {
    await stop();
  }
  const next = await f.store.get(f.scope, pipeline.data.outputDatasetId);
  expect(next?.data.activeVersion).not.toBe(old?.data.activeVersion);
  expect(next?.data.versions).toContain(old?.data.activeVersion);
  expect((await f.preview(pipeline)).rows.map((r: any) => r.profit)).toEqual([
    80, -40,
  ]);
  expect((await f.store.get(f.scope, "workspace"))?.data.activeGeneration).toBe(
    generation,
  );
  const result: any = await f.client.query(
    [pipeline.data.outputDatasetId],
    `SELECT sum(profit) total FROM ${next?.data.table}`,
  );
  expect(result.rows).toEqual([{ total: 40 }]);
  expect(result.citations).toEqual([
    {
      datasetId: pipeline.data.outputDatasetId,
      version: next?.data.activeVersion,
    },
  ]);
});
it("editing an accepted transform creates a new review and immutable pipeline identity", async () => {
  const f = await fixture();
  await f.publish(await f.client.draftPipeline(f.definition()));
  const old = await f.accepted();
  const p = await f.client.draftPipeline(f.definition(3));
  expect((await f.store.get(f.scope, old.id))?.data.sql).toBe(
    f.definition().sql,
  );
  expect((await f.accepted()).id).toBe(old.id);
  await f.publish(p);
  const next = await f.accepted();
  expect(next.id).not.toBe(old.id);
  expect(next.data.outputDatasetId).toBe(old.data.outputDatasetId);
  expect((await f.preview(next)).rows.map((r: any) => r.profit)).toEqual([
    30, -15,
  ]);
  expect((await f.store.get(f.scope, old.id))?.data.sql).toBe(
    f.definition().sql,
  );
});
it("read-only tokens cannot create reviews or enqueue accepted builds; foreign pipeline IDs stay scoped", async () => {
  const f = await fixture();
  await f.publish(await f.client.draftPipeline(f.definition()));
  const p = await f.accepted();
  const token = await f.post("/tokens", {
    name: "Pipeline readonly",
    readOnly: true,
  });
  const headers = { authorization: "Bearer " + token.token };
  for (const [path, payload] of [
    ["/pipelines/draft", f.definition()],
    [`/pipelines/${p.id}/run`, {}],
  ] as const) {
    const r = await f.app.inject({
      method: "POST",
      url: f.base + path,
      headers,
      payload,
    });
    expect(r.statusCode).toBe(403);
  }
  const foreign = (
    await f.app.inject({
      method: "POST",
      url: "/api/v1/workspaces",
      payload: { name: "Foreign" },
    })
  ).json();
  installations[installations.length - 1].scopes.push(foreign.id);
  const r = await f.app.inject({
    method: "POST",
    url: `/api/v1/workspaces/${foreign.id}/pipelines/${p.id}/run`,
    payload: {},
  });
  expect(r.statusCode).toBe(404);
  expect(await f.store.list(f.scope, "job")).toEqual([]);
});
