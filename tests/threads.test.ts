import { beforeEach, afterEach, it, expect, vi } from "vitest";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
vi.mock("../apps/api/src/executor.js", () => ({
  execute: (job: any) => localExecute(job),
}));
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { buildWorkspace } from "../apps/api/src/assistant.js";
import { Provider } from "../apps/api/src/providers.js";
import { config } from "../apps/api/src/config.js";
import { seal } from "../apps/api/src/security.js";
import { datasetTable, type Resource } from "../packages/shared/src/index.js";

// Chats: a question and its follow-ups, listed, opened and deleted as one.
let store: MemoryStore,
  app: Awaited<ReturnType<typeof createApp>>,
  scope: string,
  actor: string,
  plan: { title: string; inputs: string[]; sql: string; assumptions: [] };
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
  const me = await app.inject({ method: "GET", url: "/api/v1/me" });
  expect(me.statusCode).toBe(200);
  scope = me.json().workspaces[0].id;
  actor = me.json().user.id;
  const ds = await request("POST", "/ingest", {
    name: "Orders",
    rows: [
      { id: 1, amount: 25, month: "2026-01" },
      { id: 2, amount: 40, month: "2026-02" },
    ],
  });
  expect(ds.statusCode, ds.body).toBe(200);
  plan = {
    title: "Total revenue",
    inputs: [ds.json().id],
    sql: `SELECT sum(amount) revenue FROM ${datasetTable(ds.json().id)}`,
    assumptions: [],
  };
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
// Timestamps are ISO strings to the millisecond; keep turns apart in time.
const later = () => new Promise((r) => setTimeout(r, 5));
// The model routes every question to an answer, answers with the plan and
// summarises the result.
const model = () =>
  vi
    .spyOn(Provider.prototype, "generate")
    .mockImplementation(async (system: string) => ({
      value: system.includes("Choose answer")
        ? { intent: "answer" }
        : system.includes("Summarise this query result")
          ? { summary: "Revenue was 65.", highlights: [] }
          : plan,
      tokens: 1,
    }));
// Asks a question, or a follow-up to an earlier run, the way the web app does.
async function ask(goal: string, resourceId?: string): Promise<Resource> {
  await later();
  const r = await request("POST", "/assistant/runs", {
    goal,
    intent: "auto",
    context: { page: "analyst", ...(resourceId ? { resourceId } : {}) },
  });
  expect(r.statusCode, r.body).toBe(200);
  return r.json();
}
async function answer(id: string) {
  await buildWorkspace(store, scope, id);
  const r = (await store.get(scope, id))!;
  expect(r.data.status, r.data.error).toBe("succeeded");
  return r;
}
const threads = async () => {
  const r = await request("GET", "/threads");
  expect(r.statusCode, r.body).toBe(200);
  return r.json().items as {
    id: string;
    title: string;
    createdAt: string;
    updatedAt: string;
    turns: number;
    status: string;
  }[];
};
const put = (...rows: Resource[]) =>
  transaction(store, scope, async (tx) => {
    for (const r of rows) tx.put(r);
  });
// A run as stored before chats had ids, at a fixed time.
const legacyRun = (
  id: string,
  goal: string,
  at: string,
  resourceId?: string,
): Resource => ({
  ...resource(
    scope,
    "run",
    goal,
    {
      goal,
      context: { page: "analyst", ...(resourceId ? { resourceId } : {}) },
      intent: "auto",
      resolvedIntent: "answer",
      status: "succeeded",
      stage: "done",
      events: [],
      calls: 2,
      tokens: 2,
      answer: { ...plan, rows: [{ revenue: "65" }], citations: [] },
    },
    id,
  ),
  createdAt: at,
  updatedAt: at,
});

it("keeps a question and its follow-ups in one chat", async () => {
  model();
  const first = await ask("What is the total revenue?");
  expect(first.data.threadId).toBe(first.id);
  await answer(first.id);
  const second = await ask("How does that split by month?", first.id);
  expect(second.data.threadId).toBe(first.id);
  await answer(second.id);
  const third = await ask("Which month was highest?", second.id);
  expect(third.data.threadId).toBe(first.id);
  // A question about a dataset starts a chat of its own.
  const aboutData = await ask("What is in this dataset?", plan.inputs[0]);
  expect(aboutData.data.threadId).toBe(aboutData.id);
  expect((await store.get(scope, third.id))?.data.threadId).toBe(first.id);
});

it("lists chats newest first with their first question, turns and latest status", async () => {
  model();
  const first = await ask("What is the total revenue?");
  await answer(first.id);
  const second = await ask("How does that split by month?", first.id);
  await answer(second.id);
  const third = await ask("Which month was highest?", second.id);
  // Setup and other task runs are not chats, and asking about one starts one.
  await later();
  await put({
    ...resource(
      scope,
      "run",
      "Describe the ontology",
      {
        goal: "Describe the suggested ontology",
        task: "describe_ontology",
        status: "succeeded",
        stage: "done",
        events: [],
        calls: 1,
        tokens: 1,
      },
      "setup-run",
    ),
  });
  const other = await ask("How many orders were there?");
  const afterTask = await ask("What did setup find?", "setup-run");
  expect(afterTask.data.threadId).toBe(afterTask.id);

  const listed = await threads();
  expect(listed.map((t) => t.id)).toEqual([afterTask.id, other.id, first.id]);
  expect(listed.find((t) => t.id === "setup-run")).toBeUndefined();
  expect(listed[2]).toMatchObject({
    id: first.id,
    title: "What is the total revenue?",
    turns: 3,
    status: "queued",
    createdAt: first.createdAt,
    updatedAt: third.updatedAt,
  });
  expect(listed[1]).toMatchObject({
    title: "How many orders were there?",
    turns: 1,
    status: "queued",
  });
  expect((await request("GET", "/threads/setup-run")).statusCode).toBe(404);

  // A chat with new activity moves to the top and reports its latest run.
  await later();
  await answer(third.id);
  const moved = await threads();
  expect(moved.map((t) => t.id)).toEqual([first.id, afterTask.id, other.id]);
  expect(moved[0]).toMatchObject({ turns: 3, status: "succeeded" });
});

it("groups runs from before chats had ids by the question they followed", async () => {
  const ai = model();
  await put(
    legacyRun("legacy-1", "Legacy first question", "2026-01-01T10:00:00.000Z"),
    legacyRun(
      "legacy-2",
      "Legacy second question",
      "2026-01-01T10:01:00.000Z",
      "legacy-1",
    ),
    legacyRun(
      "legacy-3",
      "Legacy third question",
      "2026-01-01T10:02:00.000Z",
      "legacy-2",
    ),
    legacyRun("legacy-alone", "Unrelated question", "2026-01-01T09:00:00.000Z"),
  );
  let listed = await threads();
  expect(listed).toHaveLength(2);
  expect(listed[0]).toMatchObject({
    id: "legacy-1",
    title: "Legacy first question",
    turns: 3,
    createdAt: "2026-01-01T10:00:00.000Z",
    updatedAt: "2026-01-01T10:02:00.000Z",
    status: "succeeded",
  });
  expect(listed[1]).toMatchObject({ id: "legacy-alone", turns: 1 });
  const chat = await request("GET", "/threads/legacy-1");
  expect(chat.statusCode, chat.body).toBe(200);
  expect(chat.json().title).toBe("Legacy first question");
  expect(chat.json().runs.map((r: Resource) => r.id)).toEqual([
    "legacy-1",
    "legacy-2",
    "legacy-3",
  ]);
  // Only the first run of a chat names it.
  expect((await request("GET", "/threads/legacy-2")).statusCode).toBe(404);

  // A new follow-up to the last legacy turn joins the same chat.
  const next = await ask("And this month?", "legacy-3");
  expect(next.data.threadId).toBe("legacy-1");
  listed = await threads();
  expect(listed.map((t) => [t.id, t.turns])).toEqual([
    ["legacy-1", 4],
    ["legacy-alone", 1],
  ]);
  expect(
    (await request("GET", "/threads/legacy-1"))
      .json()
      .runs.map((r: Resource) => r.id),
  ).toEqual(["legacy-1", "legacy-2", "legacy-3", next.id]);
  // Its model context reaches back through the legacy turns.
  await answer(next.id);
  const context = ai.mock.calls.at(-1)![1] as any;
  expect(context.resource.previousQuestion).toBe("Legacy third question");
  expect(context.resource.history.map((h: any) => h.question)).toEqual([
    "Legacy first question",
    "Legacy second question",
  ]);
});

it("opens a chat with its runs oldest first and 404s for an unknown chat", async () => {
  model();
  const first = await ask("What is the total revenue?");
  await answer(first.id);
  const second = await ask("How does that split by month?", first.id);
  await answer(second.id);
  const third = await ask("Which month was highest?", second.id);
  await ask("How many orders were there?");
  const r = await request("GET", `/threads/${first.id}`);
  expect(r.statusCode, r.body).toBe(200);
  const chat = r.json();
  expect(chat.id).toBe(first.id);
  expect(chat.title).toBe("What is the total revenue?");
  expect(chat.runs.map((x: Resource) => x.id)).toEqual([
    first.id,
    second.id,
    third.id,
  ]);
  expect(chat.runs.map((x: Resource) => x.data.goal)).toEqual([
    "What is the total revenue?",
    "How does that split by month?",
    "Which month was highest?",
  ]);
  expect(chat.runs[0].data.answer).toMatchObject({
    title: "Total revenue",
    rows: [{ revenue: "65" }],
  });
  expect(chat.runs.map((x: Resource) => x.data.status)).toEqual([
    "succeeded",
    "succeeded",
    "queued",
  ]);
  const missing = await request("GET", "/threads/no-such-chat");
  expect(missing.statusCode).toBe(404);
  expect(missing.json().error).toBe("Chat not found");
});

it("deletes a whole chat, but not while one of its questions is running", async () => {
  model();
  const first = await ask("What is the total revenue?");
  await answer(first.id);
  const second = await ask("How does that split by month?", first.id);
  await answer(second.id);
  const other = await ask("How many orders were there?");
  await answer(other.id);
  const busy = await ask("Which month was highest?", other.id);

  // The queued follow-up holds its whole chat.
  const queued = await request("DELETE", `/threads/${other.id}`);
  expect(queued.statusCode).toBe(409);
  expect(queued.json().error).toBe(
    "Stop the question that is still running first.",
  );
  await transaction(store, scope, async (tx) => {
    const r = (await tx.get(busy.id))!;
    tx.update(r, { ...r.data, status: "running", stage: "routing" });
  });
  expect((await request("DELETE", `/threads/${other.id}`)).statusCode).toBe(
    409,
  );
  expect((await store.get(scope, other.id))?.kind).toBe("run");
  expect((await store.get(scope, busy.id))?.kind).toBe("run");

  const deleted = await request("DELETE", `/threads/${first.id}`);
  expect(deleted.statusCode, deleted.body).toBe(200);
  expect(deleted.json()).toEqual({ ok: true });
  expect(await store.get(scope, first.id)).toBeUndefined();
  expect(await store.get(scope, second.id)).toBeUndefined();
  expect((await request("GET", `/threads/${first.id}`)).statusCode).toBe(404);
  expect((await request("DELETE", `/threads/${first.id}`)).statusCode).toBe(
    404,
  );
  // The other chat is untouched.
  expect((await threads()).map((t) => [t.id, t.turns])).toEqual([
    [other.id, 2],
  ]);
  expect(
    (await request("GET", `/threads/${other.id}`))
      .json()
      .runs.map((r: Resource) => r.id),
  ).toEqual([other.id, busy.id]);

  // Once stopped, it can go too.
  const canceled = await request("POST", `/assistant/runs/${busy.id}/cancel`);
  expect(canceled.statusCode, canceled.body).toBe(200);
  // Viewers can read chats but not delete them.
  await transaction(store, "global", async (tx) => {
    const m = (await tx.get(`${actor}:${scope}`))!;
    tx.update(m, { ...m.data, role: "viewer" });
  });
  expect((await request("GET", "/threads")).statusCode).toBe(200);
  expect((await request("DELETE", `/threads/${other.id}`)).statusCode).toBe(
    403,
  );
  await transaction(store, "global", async (tx) => {
    const m = (await tx.get(`${actor}:${scope}`))!;
    tx.update(m, { ...m.data, role: "owner" });
  });
  expect((await request("DELETE", `/threads/${other.id}`)).statusCode).toBe(
    200,
  );
  expect(await threads()).toEqual([]);
  expect(await store.list(scope, "run")).toEqual([]);
});

it("gives a follow-up the chat's earlier questions, oldest first, without the current one", async () => {
  const ai = model();
  const first = await ask("What is the total revenue?");
  await answer(first.id);
  const second = await ask("How does that split by month?", first.id);
  await answer(second.id);
  // A different chat asked in between stays out of this one's history.
  const other = await ask("How many orders were there?");
  await answer(other.id);
  const third = await ask("Which month was highest?", second.id);
  ai.mockClear();
  await answer(third.id);
  // Routing, the query and the summary.
  expect(ai).toHaveBeenCalledTimes(3);
  for (const [, context] of ai.mock.calls as [string, any, ...unknown[]][]) {
    expect(context.goal).toBe("Which month was highest?");
    expect(context.resource.previousQuestion).toBe(
      "How does that split by month?",
    );
    expect(context.resource.history).toEqual([
      { question: "What is the total revenue?", answer: "Total revenue" },
    ]);
    expect(JSON.stringify(context.resource.history)).not.toContain(
      "Which month was highest?",
    );
    expect(JSON.stringify(context.resource.history)).not.toContain(
      "How many orders were there?",
    );
  }

  // One turn further, both earlier questions come back in the order asked.
  const fourth = await ask("Why was it highest?", third.id);
  ai.mockClear();
  await answer(fourth.id);
  const context = ai.mock.calls[0][1] as any;
  expect(context.resource.previousQuestion).toBe("Which month was highest?");
  expect(context.resource.history.map((h: any) => h.question)).toEqual([
    "What is the total revenue?",
    "How does that split by month?",
  ]);
});

// Answering: the query runs against every dataset table it names, and a
// failing query gets one repair before the question fails.
it("loads every table a query names, even ones the model didn't list", async () => {
  const extra = await request("POST", "/ingest", {
    name: "Customers",
    rows: [{ id: 1, name: "Acme" }],
  });
  const customers = extra.json().id,
    orders = plan.inputs[0];
  plan = {
    ...plan,
    // Joins Customers but lists only Orders, as the local model sometimes does.
    sql: `SELECT c.name, sum(o.amount) revenue FROM ${datasetTable(orders)} o, ${datasetTable(customers)} c GROUP BY 1`,
  };
  const ai = model();
  const run = await answer((await ask("Revenue by customer")).id);
  expect(run.data.answer.rows).toEqual([{ name: "Acme", revenue: "65" }]);
  expect(run.data.answer.citations.map((c: any) => c.datasetId).sort()).toEqual(
    [orders, customers].sort(),
  );
  // Routing, the answer and its summary: no repair call was needed.
  expect(ai).toHaveBeenCalledTimes(3);
});

it("repairs a failing query once, and Retry asks afresh when that fails too", async () => {
  const good = plan,
    bad = {
      ...plan,
      sql: `SELECT sum(nope) FROM ${datasetTable(plan.inputs[0])}`,
    };
  let replies = [bad, good];
  const contexts: any[] = [];
  vi.spyOn(Provider.prototype, "generate").mockImplementation(
    async (system: string, context: any) => {
      if (system.includes("Choose answer"))
        return { value: { intent: "answer" }, tokens: 1 };
      if (system.includes("Summarise this query result"))
        return {
          value: { summary: "Revenue was 65.", highlights: [] },
          tokens: 1,
        };
      contexts.push(context);
      return { value: replies.shift()!, tokens: 1 };
    },
  );
  const fixed = await answer((await ask("Total revenue?")).id);
  expect(fixed.data.answer.rows).toEqual([{ revenue: "65" }]);
  // The repair saw the failing SQL and the engine's error.
  expect(contexts[1].repair.sql).toBe(bad.sql);
  expect(contexts[1].repair.error).toMatch(/nope/);
  expect(contexts[0].repair).toBeUndefined();

  // Both attempts fail: the question fails and its plans are not reused.
  replies = [bad, bad];
  const failing = await ask("Total revenue again?");
  await buildWorkspace(store, scope, failing.id);
  const failed = (await store.get(scope, failing.id))!;
  expect(failed.data.status).toBe("failed");
  expect(failed.data.checkpoints?.analysis).toBeUndefined();
  expect(failed.data.checkpoints?.["analysis-repair"]).toBeUndefined();
  const resumed = await request(
    "POST",
    `/assistant/runs/${failing.id}/resume`,
    {},
  );
  expect(resumed.statusCode, resumed.body).toBe(200);
  replies = [good];
  const before = contexts.length;
  const retried = await answer(failing.id);
  expect(retried.data.answer.rows).toEqual([{ revenue: "65" }]);
  // Retry asked the model again rather than re-running the failed query.
  expect(contexts.length).toBe(before + 1);
});

it("redacts a credential the engine quotes from the data before the repair sees it", async () => {
  // A text column holding a request header, as a webhook log might.
  const hooks = (
    await request("POST", "/ingest", {
      name: "Webhooks",
      rows: [{ id: 1, header: "Bearer sk-proj-0123456789abcdefghij" }],
    })
  ).json().id;
  const failing = {
    ...plan,
    inputs: [hooks],
    sql: `SELECT CAST(header AS INTEGER) AS n FROM ${datasetTable(hooks)}`,
  };
  const replies = [failing, plan];
  const contexts: any[] = [];
  vi.spyOn(Provider.prototype, "generate").mockImplementation(
    async (system: string, context: any) => {
      if (system.includes("Choose answer"))
        return { value: { intent: "answer" }, tokens: 1 };
      if (system.includes("Summarise this query result"))
        return {
          value: { summary: "Revenue was 65.", highlights: [] },
          tokens: 1,
        };
      contexts.push(context);
      return { value: replies.shift()!, tokens: 1 };
    },
  );
  await answer((await ask("Total revenue?")).id);
  expect(contexts[1].repair.error).not.toContain("0123456789abcdefghij");
  expect(contexts[1].repair.error).toContain(
    "Could not convert string 'Bearer [redacted]'",
  );
});

it("keeps values the engine quotes from a hosted model that may not see rows", async () => {
  await transaction(store, scope, async (tx) =>
    tx.put(
      resource(
        scope,
        "provider",
        "AI provider",
        {
          settings: { provider: "claude", model: "claude-sonnet-4-6" },
          secret: seal("sk-test-key"),
        },
        "provider",
      ),
    ),
  );
  const people = (
    await request("POST", "/ingest", {
      name: "People",
      rows: [{ id: 1, ssn: "987-65-4320", email: "alice@example.com" }],
    })
  ).json().id;
  // An ordinary mistake: comparing a text column with a number.
  const failing = {
    ...plan,
    inputs: [people],
    sql: `SELECT count(*) AS n FROM ${datasetTable(people)} WHERE ssn = 123456789`,
  };
  const replies = [failing, plan];
  const contexts: any[] = [];
  vi.spyOn(Provider.prototype, "generate").mockImplementation(
    async (system: string, context: any) => {
      if (system.includes("Choose answer"))
        return { value: { intent: "answer" }, tokens: 1 };
      contexts.push(context);
      return { value: replies.shift()!, tokens: 1 };
    },
  );
  const run = await answer((await ask("How many people?")).id);
  expect(run.data.status, run.data.error).toBe("succeeded");
  expect(contexts[1].repair.error).toBe(
    "Conversion Error: Could not convert string [value] to INT32 when casting from source column ssn",
  );
  expect(JSON.stringify(contexts)).not.toContain("987-65-4320");
  expect(JSON.stringify(contexts[1].repair)).not.toMatch(/987|4320/);
});

it("adds a plain-language summary from the result rows, and still answers if it fails", async () => {
  const contexts: Record<string, any> = {};
  let summary: any = {
    summary: "Revenue was 65 across two orders.",
    highlights: ["February was the larger month at 40."],
  };
  vi.spyOn(Provider.prototype, "generate").mockImplementation(
    async (system: string, context: any) => {
      if (system.includes("Choose answer"))
        return { value: { intent: "answer" }, tokens: 1 };
      if (system.includes("Summarise this query result")) {
        contexts.summary = context;
        if (summary instanceof Error) throw summary;
        return { value: summary, tokens: 1 };
      }
      return { value: plan, tokens: 1 };
    },
  );
  const run = await answer((await ask("Total revenue?")).id);
  expect(run.data.answer).toMatchObject({
    summary: "Revenue was 65 across two orders.",
    highlights: ["February was the larger month at 40."],
    rows: [{ revenue: "65" }],
  });
  // The summary reads the question and the rows, not the schema.
  expect(contexts.summary.result).toMatchObject({
    question: "Total revenue?",
    rows: [{ revenue: "65" }],
  });
  expect(contexts.summary.datasets).toBeUndefined();

  summary = new Error("local returned HTTP 500.");
  const plain = await answer((await ask("Total revenue again?")).id);
  expect(plain.data.answer.rows).toEqual([{ revenue: "65" }]);
  expect(plain.data.answer.summary).toBeUndefined();
});

it("does not turn an answered question into a canceled one", async () => {
  model();
  const asked = await ask("What is the total revenue?");
  const done = await answer(asked.id);
  const answered = done.data.answer;
  expect(answered).toBeTruthy();

  // Stop pressed just as the answer arrived.
  const late = await request("POST", `/assistant/runs/${asked.id}/cancel`);
  expect(late.statusCode, late.body).toBe(409);
  expect(late.json().error).toBe("This run has already finished.");
  const kept = (await store.get(scope, asked.id))!;
  expect(kept.data.status).toBe("succeeded");
  expect(kept.data.answer).toEqual(answered);
  expect((await threads())[0].status).toBe("succeeded");

  // A question still waiting can be stopped, and stopping it twice is fine.
  const waiting = await ask("How many orders were there?");
  expect(
    (await request("POST", `/assistant/runs/${waiting.id}/cancel`)).statusCode,
  ).toBe(200);
  expect((await store.get(scope, waiting.id))!.data.status).toBe("canceled");
  expect(
    (await request("POST", `/assistant/runs/${waiting.id}/cancel`)).statusCode,
  ).toBe(200);
});
