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
import { ANSWER_CONTEXT_CHARS } from "../apps/api/src/answer-context.js";

// A question sends the model the datasets it needs: none to route it, and on
// a workspace too large to send whole, the ones it is about.
let store: MemoryStore,
  app: Awaited<ReturnType<typeof createApp>>,
  scope: string;
const request = (method: any, path: string, payload?: unknown) =>
  app.inject({
    method,
    url: `/api/v1/workspaces/${scope}` + path,
    payload: payload as any,
  });
const ingest = async (name: string, rows: unknown[]) =>
  (await request("POST", "/ingest", { name, rows })).json().id as string;
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

// Answers with a total over orders, recording what each call was sent.
function model(orders: string) {
  const calls: { stage: string; context: any; schema: any }[] = [];
  vi.spyOn(Provider.prototype, "generate").mockImplementation(
    async (system: string, context: any, schema: any) => {
      const stage = system.includes("Choose answer")
        ? "routing"
        : system.includes("Answer the question")
          ? "analysis"
          : "summary";
      calls.push({ stage, context, schema });
      return {
        value:
          stage === "routing"
            ? { intent: "answer" }
            : stage === "analysis"
              ? {
                  title: "Order total",
                  sql: 'SELECT sum("amount") AS total FROM orders',
                  inputs: [orders],
                  assumptions: [],
                }
              : { summary: "Orders total 30.", highlights: [] },
        tokens: 1,
      };
    },
  );
  return calls;
}
async function ask(goal: string) {
  const run = (
    await request("POST", "/assistant/runs", { goal, intent: "auto" })
  ).json();
  await buildWorkspace(store, scope, run.id);
  const done = (await store.get(scope, run.id))!;
  expect(done.data.status, done.data.error).toBe("succeeded");
  return done;
}

it("routes a question without the datasets, and answers from all of them when they fit", async () => {
  const orders = await ingest("orders", [{ id: 1, amount: 10 }]);
  await ingest("customers", [{ id: 1, name: "Ada" }]);
  const calls = model(orders);
  await ask("What is the total order amount?");
  const routing = calls.find((c) => c.stage === "routing")!,
    analysis = calls.find((c) => c.stage === "analysis")!;
  expect(routing.context.datasets).toBeUndefined();
  expect(analysis.context.datasets.map((d: any) => d.name).sort()).toEqual([
    "customers",
    "orders",
  ]);
});

it("on a workspace too large to send whole, answers from the datasets the question is about", async () => {
  const orders = await ingest("orders", [
    { id: 1, amount: 10 },
    { id: 2, amount: 20 },
  ]);
  // Wide tables whose schemas together are past the budget. DuckDB reads an
  // object of more than 200 keys as one map column, so each stays under it.
  const wide = Object.fromEntries(
    Array.from({ length: 190 }, (_, i) => [`field_${i}`, "x".repeat(60)]),
  );
  for (let i = 0; i < 10; i++) await ingest(`ledger_${i}`, [wide]);
  const calls = model(orders);
  const done = await ask("What is the total amount of our orders?");
  expect(done.data.answer.rows).toEqual([{ total: "30" }]);
  const analysis = calls.find((c) => c.stage === "analysis")!,
    names = analysis.context.datasets.map((d: any) => d.name);
  expect(names).toContain("orders");
  expect(names.length).toBeLessThan(11);
  expect(JSON.stringify(analysis.context.datasets).length).toBeLessThanOrEqual(
    ANSWER_CONTEXT_CHARS,
  );
  // The model may only cite the datasets it was shown.
  expect([...analysis.schema.properties.inputs.items.enum].sort()).toEqual(
    analysis.context.datasets.map((d: any) => d.id).sort(),
  );
});
