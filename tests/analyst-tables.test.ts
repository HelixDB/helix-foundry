import { beforeEach, afterEach, it, expect, vi } from "vitest";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
vi.mock("../apps/api/src/executor.js", () => ({
  execute: (job: any) => localExecute(job),
}));
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore } from "../apps/api/src/store.js";
import { buildWorkspace } from "../apps/api/src/assistant.js";
import { Provider } from "../apps/api/src/providers.js";
import { config } from "../apps/api/src/config.js";

// Questions see tables by readable name, so a model can't miscopy a long
// hashed name into a lookalike table or one that doesn't exist.
let store: MemoryStore,
  app: Awaited<ReturnType<typeof createApp>>,
  scope: string;
const request = (method: any, path: string, payload?: unknown) =>
  app.inject({
    method,
    url: `/api/v1/workspaces/${scope}` + path,
    payload: payload as any,
  });
beforeEach(async () => {
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  scope = (await app.inject({ method: "GET", url: "/api/v1/me" })).json()
    .workspaces[0].id;
});
afterEach(async () => {
  vi.restoreAllMocks();
  await app.close();
  for (const d of ["incoming", "datasets"])
    await rm(resolve(config.dataDir, d, scope), {
      recursive: true,
      force: true,
    });
});

it("answers with tables named readably, loading every table the SQL names", async () => {
  const ingest = async (name: string, rows: unknown[]) =>
    (await request("POST", "/ingest", { name, rows })).json().id as string;
  const categories = await ingest("categories", [{ id: 1, name: "Shoes" }]);
  await ingest("order_items", [
    { order_id: 1, category_id: 1, amount: 10 },
    { order_id: 2, category_id: 1, amount: 15 },
  ]);
  let seen: any;
  vi.spyOn(Provider.prototype, "generate").mockImplementation(
    async (system: string, context: any) => {
      if (system.includes("Choose answer"))
        return { value: { intent: "answer" }, tokens: 1 };
      if (system.includes("Answer the question")) seen = context.datasets;
      return {
        value: system.includes("Summarise")
          ? { summary: "Shoes sold 25.", highlights: [] }
          : {
              title: "Sales by category",
              // Names the item table by name without listing it as an input.
              sql: 'SELECT c."name", sum(i."amount") AS sales FROM order_items i JOIN "categories" c ON c."id" = i."category_id" GROUP BY 1',
              inputs: [categories],
              assumptions: [],
            },
        tokens: 1,
      };
    },
  );
  const run = (
    await request("POST", "/assistant/runs", {
      goal: "Sales by category?",
      intent: "auto",
    })
  ).json();
  await buildWorkspace(store, scope, run.id);
  const done = (await store.get(scope, run.id))!;
  expect(done.data.status, done.data.error).toBe("succeeded");
  expect(done.data.answer.rows).toEqual([{ name: "Shoes", sales: "25" }]);
  expect(done.data.answer.inputs).toHaveLength(2);
  expect(seen.map((d: any) => d.table).sort()).toEqual([
    "categories",
    "order_items",
  ]);
});

it("keeps the table name of a dataset whose name is too long for an alias", async () => {
  const id = (
    await request("POST", "/ingest", {
      name: "x".repeat(120),
      rows: [{ id: 1 }],
    })
  ).json().id as string;
  let seen: any;
  vi.spyOn(Provider.prototype, "generate").mockImplementation(
    async (system: string, context: any) => {
      if (system.includes("Choose answer"))
        return { value: { intent: "answer" }, tokens: 1 };
      if (system.includes("Answer the question")) seen = context.datasets;
      return {
        value: system.includes("Summarise")
          ? { summary: "One row.", highlights: [] }
          : {
              title: "Count",
              sql: `SELECT count(*) n FROM "d_${id.replace(/-/g, "")}"`,
              inputs: [id],
              assumptions: [],
            },
        tokens: 1,
      };
    },
  );
  const run = (
    await request("POST", "/assistant/runs", {
      goal: "How many rows?",
      intent: "auto",
    })
  ).json();
  await buildWorkspace(store, scope, run.id);
  const done = (await store.get(scope, run.id))!;
  expect(done.data.status, done.data.error).toBe("succeeded");
  expect(seen[0].table).toBe(`d_${id.replace(/-/g, "")}`);
});
