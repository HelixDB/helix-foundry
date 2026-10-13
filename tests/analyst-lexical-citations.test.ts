import { beforeEach, afterEach, expect, it, vi } from "vitest";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { datasetTable } from "../packages/shared/src/index.js";
import type { Execution } from "../services/executor/src/engine.js";

const observed = vi.hoisted(() => ({ inputs: [] as string[][] }));
// The transport boundary is local; the real DuckDB executor still ingests and
// executes the SQL. No model service or running installation is contacted.
vi.mock("../apps/api/src/executor.js", async () => {
  const { execute } = await import("../services/executor/src/engine.js");
  return {
    execute: (job: Execution) => {
      if (job.mode === "query")
        observed.inputs.push((job.inputs || []).map((x) => x.table));
      return execute(job);
    },
  };
});
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore } from "../apps/api/src/store.js";
import { buildWorkspace } from "../apps/api/src/assistant.js";
import { Provider } from "../apps/api/src/providers.js";
import { config } from "../apps/api/src/config.js";

let store: MemoryStore,
  app: Awaited<ReturnType<typeof createApp>>,
  scope: string;
beforeEach(async () => {
  observed.inputs = [];
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  scope = (await app.inject({ method: "GET", url: "/api/v1/me" })).json()
    .workspaces[0].id;
});
afterEach(async () => {
  vi.restoreAllMocks();
  await app.close();
  for (const directory of ["incoming", "datasets", "scratch", "results"])
    await rm(resolve(config.dataDir, directory, scope), {
      recursive: true,
      force: true,
    });
});

it.each([
  "canonical literal",
  "readable literal",
  "continued escape literal",
  "Unicode dollar literal",
  "comment",
  "real join",
])("saves only actual snapshot citations for %s", async (kind) => {
  const ingest = async (name: string, rows: unknown[]) => {
    const response = await app.inject({
      method: "POST",
      url: `/api/v1/workspaces/${scope}/ingest`,
      payload: { name, rows },
    });
    expect(response.statusCode).toBe(200);
    return response.json().id as string;
  };
  const orders = await ingest("orders", [
    { id: 1, amount: 3 },
    { id: 2, amount: 4 },
  ]);
  const sessions = await ingest("Sessions processed", [{ id: 1 }, { id: 2 }]);
  const note =
    kind === "canonical literal"
      ? datasetTable(sessions)
      : kind === "continued escape literal"
        ? "firstit's JOIN sessions_processed"
        : "JOIN sessions_processed";
  const sql =
    kind === "real join"
      ? "SELECT sum(o.amount) AS total FROM orders o JOIN sessions_processed s ON o.id=s.id"
      : kind === "comment"
        ? `SELECT sum(amount) AS total -- JOIN sessions_processed ${datasetTable(sessions)}\nFROM orders`
        : kind === "continued escape literal"
          ? String.raw`SELECT E'first'
'it\'s JOIN sessions_processed' AS note, sum(amount) AS total FROM orders`
          : kind === "Unicode dollar literal"
            ? `SELECT $é$${note}$é$ AS note, sum(amount) AS total FROM orders`
            : `SELECT '${note}' AS note, sum(amount) AS total FROM orders`;
  vi.spyOn(Provider.prototype, "generate").mockImplementation(
    async (system: string) => ({
      value: system.includes("Choose answer")
        ? { intent: "answer" }
        : system.includes("Summarise")
          ? { summary: "The executed total is seven.", highlights: [] }
          : {
              title: "Total orders",
              sql,
              inputs: [orders, sessions],
              assumptions: [],
            },
      tokens: 1,
    }),
  );
  const response = await app.inject({
    method: "POST",
    url: `/api/v1/workspaces/${scope}/assistant/runs`,
    payload: { goal: "What is the total?", intent: "auto" },
  });
  expect(response.statusCode).toBe(200);
  const id = response.json().id as string;
  await buildWorkspace(store, scope, id);
  const run = (await store.get(scope, id))!;
  expect(run.data.status, run.data.error).toBe("succeeded");
  const expected = kind === "real join" ? [orders, sessions] : [orders];
  expect(run.data.answer.sql).toBe(sql);
  expect(run.data.answer.inputs).toEqual(expected);
  expect(run.data.answer.rows).toEqual([
    kind.includes("literal") ? { note, total: "7" } : { total: "7" },
  ]);
  expect(run.data.answer.citations).toEqual(
    await Promise.all(
      expected.map(async (datasetId) => {
        const dataset = (await store.get(scope, datasetId))!;
        return {
          datasetId,
          name: dataset.name,
          version: dataset.data.activeVersion,
        };
      }),
    ),
  );
  expect(observed.inputs).toEqual([expected.map(datasetTable)]);
});
