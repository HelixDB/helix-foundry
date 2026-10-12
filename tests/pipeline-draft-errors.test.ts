import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
vi.mock("../apps/api/src/executor.js", () => ({
  execute: (job: any) => localExecute(job),
}));
import { createApp } from "../apps/api/src/app.js";
import { config } from "../apps/api/src/config.js";
import { MemoryStore } from "../apps/api/src/store.js";

let app: Awaited<ReturnType<typeof createApp>>,
  store: MemoryStore,
  scope: string;
beforeEach(async () => {
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  scope = (await app.inject({ method: "GET", url: "/api/v1/me" })).json()
    .workspaces[0].id;
});
afterEach(async () => {
  await app.close();
  for (const dir of ["incoming", "datasets"])
    await rm(resolve(config.dataDir, dir, scope), {
      recursive: true,
      force: true,
    });
});
const draft = (nodes: unknown[]) =>
  app.inject({
    method: "POST",
    url: `/api/v1/workspaces/${scope}/pipelines/draft`,
    payload: { name: "Draft", inputs: ["input"], sql: "SELECT 1", nodes },
  });

for (const [nodes, message] of [
  [
    [{ id: "a", type: "filter", inputs: ["a"], config: { condition: "true" } }],
    "Pipeline has a cycle",
  ],
  [
    [
      { id: "a", type: "source", datasetId: "a", inputs: [] },
      { id: "b", type: "source", datasetId: "b", inputs: [] },
    ],
    "Connect every branch to one output node",
  ],
  [
    [
      {
        id: "a",
        type: "filter",
        inputs: ["missing"],
        config: { condition: "true" },
      },
    ],
    "Unknown node missing",
  ],
] as const)
  it(`returns actionable client validation for ${message}`, async () => {
    const response = await draft([...nodes]);
    expect(response.statusCode, response.body).toBe(400);
    expect(response.json().error).toBe(message);
    expect(await store.list(scope, "proposal")).toHaveLength(0);
  });

it("still compiles and validates a valid graph through the native executor", async () => {
  const uploaded = await app.inject({
    method: "POST",
    url: `/api/v1/workspaces/${scope}/ingest`,
    payload: { name: "Orders", rows: [{ id: 1, amount: 42 }] },
  });
  expect(uploaded.statusCode, uploaded.body).toBe(200);
  const response = await draft([
    { id: "orders", type: "source", datasetId: uploaded.json().id, inputs: [] },
  ]);
  expect(response.statusCode, response.body).toBe(200);
  expect(response.json().kind).toBe("proposal");
  expect(response.json().data.status).toBe("ready");
});
