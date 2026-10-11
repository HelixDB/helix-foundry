import { beforeAll, afterAll, afterEach, expect, it } from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { rm } from "node:fs/promises";
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
afterEach(async () => {
  await app?.close();
  for (const kind of ["incoming", "datasets", "results"])
    await rm(resolve(config.dataDir, kind, scope), {
      recursive: true,
      force: true,
    });
});
async function fixture() {
  app = await createApp(new MemoryStore());
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
    rows: Array.from({ length: 1605 }, (_, i) => ({
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
  return { request, dataset, publish, base };
}
it("finds records outside the1500-node sample and resolves their native record neighbourhood", async () => {
  const f = await fixture(),
    sample = await f.request("GET", "/graph");
  expect(sample.total).toBe(1605);
  expect(sample.nodes).toHaveLength(1500);
  const ids = new Set(sample.nodes.map((n: any) => n.id));
  let omitted: any;
  for (let offset = 0; offset < 1605 && !omitted; offset += 500) {
    const page = await f.request(
      "GET",
      `/resources/object?limit=500&offset=${offset}`,
    );
    omitted = page.items.find((r: any) => !ids.has(r.data.logicalId));
  }
  expect(omitted).toBeDefined();
  const found = await f.request(
    "GET",
    `/resources/object?${new URLSearchParams({ search: omitted.data.label, limit: "8" })}`,
  );
  expect(found.items.map((r: any) => r.id)).toEqual([omitted.id]);
  const focus = await f.request(
    "GET",
    `/graph?focus=${omitted.data.logicalId}&depth=2`,
  );
  expect(focus.nodes).toHaveLength(1);
  expect(focus.nodes[0].id).toBe(omitted.data.logicalId);
  const detail = await f.request("GET", `/records/${omitted.data.logicalId}`);
  expect(detail.record.data.effective.name).toBe(omitted.data.label);
  expect(
    (await f.request("GET", `/resources/dataset/${f.dataset.id}`)).data
      .activeVersion,
  ).toBe(f.dataset.data.activeVersion);
});
it("reports global match counts before the eight-result limit and returns no fabricated empty match", async () => {
  const f = await fixture();
  const matches = await f.request(
    "GET",
    "/resources/object?search=Search%20person&limit=8",
  );
  expect(matches.total).toBe(1605);
  expect(matches.items).toHaveLength(8);
  const empty = await f.request(
    "GET",
    "/resources/object?search=absent%20synthetic%20record&limit=8",
  );
  expect(empty).toMatchObject({ total: 0, items: [] });
});
it("searches effective human edits while preserving the original imported snapshot", async () => {
  const f = await fixture();
  const r = (await f.request("GET", "/resources/object?limit=1")).items[0];
  const values = { name: "Owner renamed graph person" };
  const preview = await f.request("POST", "/actions/preview", {
    objectId: r.id,
    values,
  });
  await f.request("POST", "/actions/execute", {
    objectId: r.id,
    values,
    revision: preview.revision,
    idempotencyKey: "graph-search-edit",
  });
  const found = await f.request(
    "GET",
    "/resources/object?search=Owner%20renamed&limit=8",
  );
  expect(found.items).toHaveLength(1);
  expect(found.items[0].data.label).toBe(values.name);
  expect(found.items[0].data.logicalId).toBe(r.data.logicalId);
  expect(preview).toBeDefined();
  expect(
    (await f.request("GET", `/resources/dataset/${f.dataset.id}`)).data
      .activeVersion,
  ).toBe(f.dataset.data.activeVersion);
});
it("limits search to the active publication and keeps record scope checks", async () => {
  const f = await fixture();
  const previous = (await f.request("GET", "/resources/object?limit=1"))
    .items[0];
  await f.publish();
  const next = await f.request(
    "GET",
    "/resources/object?search=Search%20person&limit=8",
  );
  expect(next.total).toBe(1605);
  expect(
    next.items.every(
      (r: any) => r.data.generation !== previous.data.generation,
    ),
  ).toBe(true);
  const missing = await app!.inject(f.base + "/records/not-a-record");
  expect(missing.statusCode).toBe(404);
  const foreign = await app!.inject(
    "/api/v1/workspaces/foreign-workspace/resources/object?search=Search&limit=8",
  );
  expect(foreign.statusCode).toBe(404);
});
