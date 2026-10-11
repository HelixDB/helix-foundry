import { beforeAll, afterAll, afterEach, expect, it } from "vitest";
import { createServer } from "node:http";
import { once } from "node:events";
import { rm, readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { config } from "../apps/api/src/config.js";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { buildWorkspace } from "../apps/api/src/assistant.js";
import { FoundryClient } from "../packages/sdk/src/index.js";
import { datasetTable } from "../packages/shared/src/index.js";

const originalExecutor = config.executor;
let afterQuery: (() => Promise<void>) | undefined;
const contexts: any[] = [];
const executor = createServer(async (req, res) => {
  try {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(chunk);
    res.setHeader("content-type", "application/json");
    const job = JSON.parse(Buffer.concat(chunks).toString());
    const result = await nativeExecute(job);
    if (job.mode === "query" && afterQuery) {
      const hook = afterQuery;
      afterQuery = undefined;
      await hook();
    }
    res.end(JSON.stringify(result));
  } catch (e) {
    res.statusCode = 422;
    res.end(JSON.stringify({ error: (e as Error).message }));
  }
});
// The real Provider adapter talks only to this owned local model boundary.
// Replies are deterministic; the API executes their SQL using native DuckDB.
const model = createServer(async (req, res) => {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk);
  const body = JSON.parse(Buffer.concat(chunks).toString());
  const system = body.messages[0].content as string;
  const context = JSON.parse(body.messages[1].content);
  res.setHeader("content-type", "application/json");
  let value: any;
  if (system.includes("Answer the question")) {
    contexts.push(context);
    const dataset = context.datasets[0];
    const unit = dataset.columnDefinitions?.find(
      (d: any) => d.column === "amount",
    )?.unit;
    value = {
      title: "Owner-defined revenue",
      sql: `SELECT sum(amount) / ${unit === "cents" ? "100.0" : "1.0"} revenue FROM "${dataset.table.replaceAll('"', '""')}"`,
      inputs: [dataset.id],
      assumptions: [],
    };
  } else value = { summary: "Executed owner-context query", highlights: [] };
  res.end(
    JSON.stringify({
      message: { content: JSON.stringify(value) },
      done: true,
      done_reason: "stop",
      prompt_eval_count: 1,
      eval_count: 1,
    }),
  );
});
beforeAll(async () => {
  for (const server of [executor, model]) {
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
  }
  config.executor = `http://127.0.0.1:${(executor.address() as any).port}`;
});
afterAll(async () => {
  config.executor = originalExecutor;
  for (const server of [executor, model]) {
    server.closeAllConnections();
    await new Promise<void>((done) => server.close(() => done()));
  }
});
const owned: { app: Awaited<ReturnType<typeof createApp>>; scope: string }[] =
  [];
afterEach(async () => {
  contexts.length = 0;
  afterQuery = undefined;
  for (const { app, scope } of owned.splice(0)) {
    await app.close();
    for (const kind of ["incoming", "datasets", "results"])
      await rm(resolve(config.dataDir, kind, scope), {
        recursive: true,
        force: true,
      });
  }
});
async function fixture() {
  const store = new MemoryStore(),
    app = await createApp(store);
  const origin = await app.listen({ host: "127.0.0.1", port: 0 });
  const scope = (await app.inject("/api/v1/me")).json().workspaces[0].id;
  owned.push({ app, scope });
  const base = `/api/v1/workspaces/${scope}`;
  const request = (method: any, path: string, payload?: unknown) =>
    app.inject({ method, url: base + path, payload: payload as any });
  const dataset = (
    await request("POST", "/ingest", {
      name: "Owner accounts",
      rows: [
        { id: 1, amount: 150 },
        { id: 2, amount: 250 },
      ],
    })
  ).json();
  const path = `/datasets/${dataset.id}/dictionary`;
  await transaction(store, scope, async (tx) =>
    tx.put(
      resource(
        scope,
        "provider",
        "Local boundary",
        {
          settings: {
            provider: "local",
            model: "synthetic-dictionary-boundary",
            baseUrl: `http://127.0.0.1:${(model.address() as any).port}`,
          },
        },
        "provider",
      ),
    ),
  );
  const definitions = [
    { column: "amount", meaning: "Revenue booked before fees", unit: "cents" },
  ];
  const save = async (defs = definitions) => {
    const current = await request("GET", path);
    expect(current.statusCode, current.body).toBe(200);
    const saved = await request("PUT", path, {
      revision: current.json().revision,
      datasetRevision: current.json().datasetRevision,
      definitions: defs,
    });
    expect(saved.statusCode, saved.body).toBe(200);
    return saved.json();
  };
  const answer = async (cancelAtQuery = false) => {
    const run = (
      await request("POST", "/assistant/runs", {
        goal: "What is our revenue?",
        intent: "answer",
      })
    ).json();
    if (cancelAtQuery)
      afterQuery = async () => {
        const stopped = await request(
          "POST",
          `/assistant/runs/${run.id}/cancel`,
          {},
        );
        expect(stopped.statusCode, stopped.body).toBe(200);
      };
    await buildWorkspace(store, scope, run.id);
    return (await store.get(scope, run.id))!;
  };
  return {
    store,
    app,
    origin,
    scope,
    base,
    request,
    dataset,
    path,
    definitions,
    save,
    answer,
  };
}
it("saves owner meanings and units without changing native snapshots, profiles or values", async () => {
  const f = await fixture();
  const file = resolve(
    config.dataDir,
    "datasets",
    f.scope,
    f.dataset.data.activeVersion + ".parquet",
  );
  const before = await readFile(file),
    saved = await f.save();
  expect(saved.definitions).toEqual(f.definitions);
  const dataset = (
    await f.request("GET", `/resources/dataset/${f.dataset.id}`)
  ).json();
  expect(dataset.data.profile).toEqual(f.dataset.data.profile);
  expect(dataset.data.activeVersion).toBe(f.dataset.data.activeVersion);
  expect(await readFile(file)).toEqual(before);
  const run = await f.answer();
  expect(run.data.status, run.data.error).toBe("succeeded");
  expect(Number(run.data.answer.rows[0].revenue)).toBe(4);
  expect(contexts[0].datasets[0].columnDefinitions).toEqual(f.definitions);
  expect(run.data.answer.citations[0].version).toBe(
    f.dataset.data.activeVersion,
  );
});
it("invalidates a canceled run's saved analysis when owner units change without a data refresh", async () => {
  const f = await fixture();
  await f.save();
  const failed = await f.answer(true);
  expect(failed.data.status).toBe("canceled");
  expect(failed.data.checkpoints.analysis.sql).toContain("100.0");
  await f.save([{ ...f.definitions[0], unit: "USD" }]);
  expect(
    (await f.request("POST", `/assistant/runs/${failed.id}/resume`, {}))
      .statusCode,
  ).toBe(200);
  await buildWorkspace(f.store, f.scope, failed.id);
  const done = (await f.store.get(f.scope, failed.id))!;
  expect(done.data.status, done.data.error).toBe("succeeded");
  expect(contexts).toHaveLength(2);
  expect(contexts[1].datasets[0].columnDefinitions[0].unit).toBe("USD");
  expect(done.data.inputHash).not.toBe(failed.data.inputHash);
  expect(Number(done.data.answer.rows[0].revenue)).toBe(400);
  expect(done.data.answer.citations[0].version).toBe(
    f.dataset.data.activeVersion,
  );
});
it("retains removed definitions across accepted schema changes and restores their context when columns return", async () => {
  const f = await fixture();
  await f.save([
    { column: "id", meaning: "Stable account identity", unit: "" },
  ]);
  const update = async (rows: any[]) => {
    const response = await f.request("POST", "/ingest", {
      name: f.dataset.name,
      datasetId: f.dataset.id,
      rows,
    });
    expect(response.statusCode, response.body).toBe(200);
    expect(
      (await f.request("POST", `/datasets/${f.dataset.id}/accept-version`, {}))
        .statusCode,
    ).toBe(200);
  };
  await update([{ amount: 200 }]);
  const dormant = (await f.request("GET", f.path)).json();
  expect(dormant.definitions).toHaveLength(1);
  expect(dormant.columns.map((c: any) => c.name)).toEqual(["amount"]);
  expect((await f.answer()).data.status).toBe("succeeded");
  expect(contexts[0].datasets[0]).not.toHaveProperty("columnDefinitions");
  await update([{ id: 3, amount: 200 }]);
  expect((await f.answer()).data.status).toBe("succeeded");
  expect(contexts[1].datasets[0].columnDefinitions).toEqual(
    dormant.definitions,
  );
});
it("protects concurrent dictionary writes and stale saves after actual refreshes", async () => {
  const f = await fixture(),
    before = (await f.request("GET", f.path)).json();
  const replies = await Promise.all(
    ["First owner meaning", "Second owner meaning"].map((meaning) =>
      f.request("PUT", f.path, {
        revision: before.revision,
        datasetRevision: before.datasetRevision,
        definitions: [{ ...f.definitions[0], meaning }],
      }),
    ),
  );
  expect(replies.map((r) => r.statusCode).sort()).toEqual([200, 409]);
  const saved = (await f.request("GET", f.path)).json();
  await f.request("POST", "/ingest", {
    name: f.dataset.name,
    datasetId: f.dataset.id,
    rows: [{ id: 3, amount: 500 }],
  });
  expect(
    (
      await f.request("PUT", f.path, {
        revision: saved.revision,
        datasetRevision: saved.datasetRevision,
        definitions: [],
      })
    ).statusCode,
  ).toBe(409);
  expect((await f.request("GET", f.path)).json().definitions).toEqual(
    saved.definitions,
  );
});
it("supports real read-only and writable SDK access while rejecting unknown, duplicated and foreign columns", async () => {
  const f = await fixture();
  const writer = (
    await f.request("POST", "/tokens", {
      name: "Dictionary SDK",
      readOnly: false,
    })
  ).json().token;
  const sdk = new FoundryClient({
    baseUrl: f.origin,
    workspaceId: f.scope,
    token: writer,
  });
  const current = await sdk.columnDictionary(f.dataset.id);
  const saved = await sdk.updateColumnDictionary(
    f.dataset.id,
    current.revision,
    current.datasetRevision,
    f.definitions,
  );
  expect(saved.definitions).toEqual(f.definitions);
  const readonly = (
    await f.request("POST", "/tokens", {
      name: "Read dictionary",
      readOnly: true,
    })
  ).json().token;
  const viewer = new FoundryClient({
    baseUrl: f.origin,
    workspaceId: f.scope,
    token: readonly,
  });
  expect((await viewer.columnDictionary(f.dataset.id)).definitions).toEqual(
    f.definitions,
  );
  await expect(
    viewer.updateColumnDictionary(
      f.dataset.id,
      saved.revision,
      saved.datasetRevision,
      [],
    ),
  ).rejects.toMatchObject({ status: 403 });
  for (const definitions of [
    [{ ...f.definitions[0], column: "not-a-column" }],
    [f.definitions[0], f.definitions[0]],
    [{ ...f.definitions[0], meaning: "x".repeat(1001) }],
  ])
    expect(
      (
        await f.request("PUT", f.path, {
          revision: saved.revision,
          datasetRevision: saved.datasetRevision,
          definitions,
        })
      ).statusCode,
    ).toBe(400);
  expect(
    (await f.request("GET", "/datasets/foreign-dataset/dictionary")).statusCode,
  ).toBe(404);
  expect(
    (await f.request("GET", "/datasets/workspace/dictionary")).statusCode,
  ).toBe(404);
  expect((await f.request("GET", f.path)).json().definitions).toEqual(
    f.definitions,
  );
  const cleared = await sdk.updateColumnDictionary(
    f.dataset.id,
    saved.revision,
    saved.datasetRevision,
    [],
  );
  expect(cleared.definitions).toEqual([]);
});
it("keeps existing public native queries working without dictionary metadata", async () => {
  const f = await fixture();
  const run = await f.answer();
  expect(run.data.status, run.data.error).toBe("succeeded");
  expect(Number(run.data.answer.rows[0].revenue)).toBe(400);
  expect(contexts[0].datasets[0]).not.toHaveProperty("columnDefinitions");
});

it("keeps a derived dataset's dictionary through real pipeline republication", async () => {
  const f = await fixture();
  const bundle = {
    summary: "Dictionary pipeline",
    assumptions: [],
    catalog: [],
    pipelines: [
      {
        name: "Dictionary output",
        inputs: [f.dataset.id],
        sql: `SELECT id, amount FROM ${datasetTable(f.dataset.id)}`,
        nodes: [],
        checks: [],
      },
    ],
    ontology: [],
    relationships: [],
    dashboards: [],
    actions: [],
  };
  const publish = async () => {
    const imported = await f.request("POST", "/definitions/import", {
      version: 1,
      proposals: [bundle],
    });
    expect(imported.statusCode, imported.body).toBe(200);
    const proposal = imported.json().items[0];
    expect(proposal.data.validation.valid).toBe(true);
    const result = await f.request(
      "POST",
      `/proposals/${proposal.id}/publish`,
      { hash: proposal.data.hash },
    );
    expect(result.statusCode, result.body).toBe(200);
    return (await f.request("GET", "/resources/pipeline")).json().items[0];
  };
  const pipeline = await publish(),
    path = `/datasets/${pipeline.data.outputDatasetId}/dictionary`;
  const initial = (await f.request("GET", path)).json();
  const saved = await f.request("PUT", path, {
    revision: initial.revision,
    datasetRevision: initial.datasetRevision,
    definitions: f.definitions,
  });
  expect(saved.statusCode, saved.body).toBe(200);
  const before = (
    await f.request(
      "GET",
      `/resources/dataset/${pipeline.data.outputDatasetId}`,
    )
  ).json();
  const published = await publish();
  expect(published.data.outputDatasetId).toBe(pipeline.data.outputDatasetId);
  const after = (
    await f.request(
      "GET",
      `/resources/dataset/${pipeline.data.outputDatasetId}`,
    )
  ).json();
  expect(after.data.generation).not.toBe(before.data.generation);
  const retained = await f.request("GET", path);
  expect(retained.statusCode, retained.body).toBe(200);
  expect(retained.json().definitions).toEqual(f.definitions);
  expect(retained.json().revision).toBe(saved.json().revision);
});

it("refuses an actually existing dataset from another workspace through scoped dictionary routes", async () => {
  const f = await fixture();
  const created = await f.app.inject({
    method: "POST",
    url: "/api/v1/workspaces",
    payload: { name: "Foreign dictionary oracle" },
  });
  expect(created.statusCode, created.body).toBe(200);
  const foreign = created.json();
  try {
    const ingested = await f.app.inject({
      method: "POST",
      url: `/api/v1/workspaces/${foreign.id}/ingest`,
      payload: { name: "Real foreign rows", rows: [{ id: 9, amount: 900 }] },
    });
    expect(ingested.statusCode, ingested.body).toBe(200);
    const ds = ingested.json();
    const own = await f.app.inject(
      `/api/v1/workspaces/${foreign.id}/datasets/${ds.id}/dictionary`,
    );
    expect(own.statusCode, own.body).toBe(200);
    expect(
      (await f.request("GET", `/datasets/${ds.id}/dictionary`)).statusCode,
    ).toBe(404);
    expect(
      (
        await f.request("PUT", `/datasets/${ds.id}/dictionary`, {
          revision: own.json().revision,
          datasetRevision: own.json().datasetRevision,
          definitions: [
            { column: "amount", meaning: "Must stay scoped", unit: "USD" },
          ],
        })
      ).statusCode,
    ).toBe(404);
    expect(
      (
        await f.app.inject(
          `/api/v1/workspaces/${foreign.id}/datasets/${ds.id}/dictionary`,
        )
      ).json().definitions,
    ).toEqual([]);
  } finally {
    for (const kind of ["incoming", "datasets", "results"])
      await rm(resolve(config.dataDir, kind, foreign.id), {
        recursive: true,
        force: true,
      });
  }
});
