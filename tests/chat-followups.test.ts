import { it, expect } from "vitest";
import { chatFollowups } from "../apps/api/src/followups.js";
import { resource, MemoryStore, transaction } from "../apps/api/src/store.js";
import { createApp } from "../apps/api/src/app.js";
import { BundleSchema } from "../packages/shared/src/index.js";

function fixture(name = "Order") {
  const columns = [
    {
      name: "id",
      type: "BIGINT",
      distinct: 100,
      nulls: 0,
      primaryKey: true,
      sensitive: false,
      examples: [],
    },
    {
      name: "status",
      type: "VARCHAR",
      distinct: 3,
      nulls: 0,
      primaryKey: false,
      sensitive: false,
      examples: [],
    },
    {
      name: "channel",
      type: "VARCHAR",
      distinct: 5,
      nulls: 0,
      primaryKey: false,
      sensitive: false,
      examples: [],
    },
    {
      name: "date",
      type: "TIMESTAMP",
      distinct: 30,
      nulls: 0,
      primaryKey: false,
      sensitive: false,
      examples: [],
    },
    {
      name: "email",
      type: "VARCHAR",
      distinct: 10,
      nulls: 0,
      primaryKey: false,
      sensitive: true,
      examples: [],
    },
    {
      name: "empty",
      type: "VARCHAR",
      distinct: 3,
      nulls: 100,
      primaryKey: false,
      sensitive: false,
      examples: [],
    },
  ];
  const dataset = resource(
    "w",
    "dataset",
    name,
    { activeVersion: "v1", profile: { rows: 100, columns } },
    "d",
  );
  const bundle = BundleSchema.parse({
    summary: "",
    assumptions: [],
    catalog: [],
    pipelines: [],
    relationships: [],
    dashboards: [],
    ontology: [
      {
        name,
        datasetId: "d",
        primaryKey: "id",
        properties: [
          "id",
          "status",
          "channel",
          "date",
          "email",
          "invented",
          "empty",
        ],
      },
    ],
  });
  const run = resource(
    "w",
    "run",
    "Orders",
    {
      status: "succeeded",
      goal: "Orders by status",
      answer: { inputs: ["d"], citations: [{ datasetId: "d", version: "v1" }] },
    },
    "r",
  );
  return { bundle, dataset, run };
}
it("offers up to three supported property questions, prioritizing current context", () => {
  const { bundle, dataset, run } = fixture();
  const result = chatFollowups(bundle, [dataset], [run]);
  expect(result).toHaveLength(3);
  expect(result[0].ontology.property).toBe("status");
  expect(result[0].label).toBe("How do Order counts vary by status?");
  expect(result[0].question).toBe(
    "How do Order counts vary by status, with the same filters and scope?",
  );
  // Typing the start of a label completes the question it stands for.
  expect(
    result.every(
      (s) =>
        s.label.endsWith("?") &&
        s.question.startsWith(s.label.slice(0, -1)) &&
        s.question.endsWith(", with the same filters and scope?"),
    ),
  ).toBe(true);
  expect(result.map((s) => s.ontology.property).sort()).toEqual([
    "channel",
    "date",
    "status",
  ]);
  expect(result.find((s) => s.ontology.property === "date")?.label).toBe(
    "How do Order counts vary daily by date?",
  );
  expect(
    chatFollowups(fixture("Invoice").bundle, [dataset], [run])[0].question,
  ).toContain("Invoice");
});
it("gives each date column its own label naming the column and the counted type", () => {
  const { bundle, dataset, run } = fixture();
  const date = dataset.data.profile.columns.find((c: any) => c.name === "date");
  dataset.data.profile.columns.push(
    { ...date, name: "created_at" },
    { ...date, name: "updatedAt" },
  );
  bundle.ontology[0].properties = ["created_at", "updatedAt"];
  const labels = chatFollowups(bundle, [dataset], [run]).map((s) => s.label);
  expect(labels.sort()).toEqual([
    "How do Order counts vary daily by created at?",
    "How do Order counts vary daily by updated at?",
  ]);
});
it("omits unsupported, stale, failed, absent and already-asked candidates", () => {
  const { bundle, dataset, run } = fixture();
  const chosen = chatFollowups(bundle, [dataset], [run])[0];
  expect(
    chatFollowups(
      bundle,
      [dataset],
      [{ ...run, data: { ...run.data, goal: chosen.question } }],
    ).some((s) => s.id === chosen.id),
  ).toBe(false);
  expect(
    chatFollowups(
      bundle,
      [{ ...dataset, data: { ...dataset.data, activeVersion: "v2" } }],
      [run],
    ),
  ).toEqual([]);
  expect(chatFollowups(bundle, [], [run])).toEqual([]);
  expect(
    chatFollowups(
      bundle,
      [dataset],
      [{ ...run, data: { ...run.data, status: "running" } }],
    ),
  ).toEqual([]);
  expect(chatFollowups({ ...bundle, ontology: [] }, [dataset], [run])).toEqual(
    [],
  );
  expect(
    chatFollowups(
      bundle,
      [dataset],
      [{ ...run, data: { ...run.data, answer: { inputs: ["d"] } } }],
    ),
  ).toEqual([]);
});
it("reads only the authorized latest chat and current accepted publication", async () => {
  const store = new MemoryStore(),
    app = await createApp(store);
  await app.ready();
  try {
    const me = await app.inject({ method: "GET", url: "/api/v1/me" });
    const scope = me.json().workspaces[0].id;
    const { bundle, dataset, run } = fixture();
    const ws = (await store.get(scope, "workspace"))!;
    await transaction(store, scope, async (tx) => {
      tx.put({ ...ws, data: { ...ws.data, activeGeneration: "g1" } });
      tx.put({ ...dataset, workspaceId: scope });
      tx.put({ ...run, workspaceId: scope });
      tx.put(
        resource(scope, "proposal", "Accepted", {
          publishedGeneration: "g1",
          bundle,
        }),
      );
    });
    const get = () =>
      app.inject({
        method: "GET",
        url: `/api/v1/workspaces/${scope}/threads/r/followups`,
      });
    let response = await get();
    expect(response.statusCode, response.body).toBe(200);
    expect(response.json().items).toHaveLength(3);
    expect(response.json().runId).toBe("r");
    const missing = await app.inject({
      method: "GET",
      url: `/api/v1/workspaces/${scope}/threads/not-a-chat/followups`,
    });
    expect(missing.statusCode).toBe(404);
    await transaction(store, scope, async (tx) => {
      const w = (await tx.get("workspace"))!;
      tx.put({ ...w, data: { ...w.data, activeGeneration: "g2" } });
    });
    response = await get();
    expect(response.json().items).toEqual([]);
  } finally {
    await app.close();
  }
});

it("does not repeat a dimension already grouped in the answer", () => {
  const { bundle, dataset, run } = fixture();
  run.data.answer.sql = "SELECT status,count(*) FROM orders GROUP BY status";
  run.data.answer.rows = [{ status: "delivered", count: 10 }];
  expect(
    chatFollowups(bundle, [dataset], [run]).some(
      (s) => s.ontology.property === "status",
    ),
  ).toBe(false);
});

it("avoids repeating earlier breakdowns in the same chat", () => {
  const { bundle, dataset, run } = fixture();
  const before = {
    ...run,
    id: "before",
    data: {
      ...run.data,
      answer: {
        ...run.data.answer,
        sql: "SELECT channel,count(*) FROM orders GROUP BY channel",
        rows: [{ channel: "organic", count: 5 }],
      },
    },
  };
  expect(
    chatFollowups(bundle, [dataset], [before, run]).some(
      (s) => s.ontology.property === "channel",
    ),
  ).toBe(false);
});
