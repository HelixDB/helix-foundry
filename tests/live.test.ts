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
import { LIVE_MODEL, liveTransport, resultText } from "../apps/api/src/live.js";
import { datasetTable, type Resource } from "../packages/shared/src/index.js";

// Hands-free voice: GPT-Live sessions created on the server, and delegations
// answered by ordinary Analyst runs through the sideband.
let store: MemoryStore,
  app: Awaited<ReturnType<typeof createApp>>,
  scope: string,
  plan: { title: string; inputs: string[]; sql: string; assumptions: string[] };
const original = { ...liveTransport };
const request = (method: any, path: string, payload?: unknown) =>
  app.inject({
    method,
    url: `/api/v1/workspaces/${scope}` + path,
    payload: payload as any,
  });

// A sideband stand-in: records commands and lets the test send events.
class FakeSocket {
  readyState = 1;
  sent: any[] = [];
  closed = false;
  listeners = new Map<string, ((e: any) => void)[]>();
  constructor(
    public url: string,
    public headers: Record<string, string>,
  ) {}
  send(data: string) {
    this.sent.push(JSON.parse(data));
  }
  close() {
    this.closed = true;
    this.readyState = 3;
  }
  addEventListener(type: string, fn: (e: any) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) || []), fn]);
  }
  open() {
    this.readyState = 1;
    for (const fn of this.listeners.get("open") || []) fn({});
  }
  emit(event: unknown) {
    for (const fn of this.listeners.get("message") || [])
      fn({ data: JSON.stringify(event) });
  }
  of(type: string) {
    return this.sent.filter((c) => c.type === type);
  }
}
let sockets: FakeSocket[],
  openai: { url: string; body?: any; headers: any }[],
  liveStatus: number;

async function useProvider(provider: "openai" | "claude", allowRawData = true) {
  await transaction(store, scope, async (tx) =>
    tx.put(
      resource(
        scope,
        "provider",
        "AI provider",
        {
          settings: {
            provider,
            model: provider === "openai" ? "gpt-5.5" : "claude-sonnet-5-5",
            allowRawData,
          },
          secret: seal("sk-test-key"),
        },
        "provider",
      ),
    ),
  );
}

beforeEach(async () => {
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  const me = await app.inject({ method: "GET", url: "/api/v1/me" });
  scope = me.json().workspaces[0].id;
  const ds = await request("POST", "/ingest", {
    name: "Orders",
    rows: [
      { id: 1, amount: 25, source: "ads" },
      { id: 2, amount: 40, source: "email" },
    ],
  });
  expect(ds.statusCode, ds.body).toBe(200);
  plan = {
    title: "Total revenue",
    inputs: [ds.json().id],
    sql: `SELECT sum(amount) revenue FROM ${datasetTable(ds.json().id)}`,
    assumptions: ["Revenue excludes refunds"],
  };
  sockets = [];
  openai = [];
  liveStatus = 201;
  Object.assign(liveTransport, {
    pollMs: 5,
    settleMs: 5,
    settleMaxMs: 50,
    socket: (url: string, headers: Record<string, string>) => {
      const s = new FakeSocket(url, headers);
      sockets.push(s);
      return s;
    },
    fetch: async (url: string, init: RequestInit) => {
      openai.push({
        url,
        headers: init.headers,
        body: init.body ? JSON.parse(String(init.body)) : undefined,
      });
      if (url.endsWith(`/models/${LIVE_MODEL}`))
        return new Response("{}", {
          status: liveStatus === 201 ? 200 : liveStatus,
        });
      if (liveStatus !== 201) return new Response("{}", { status: liveStatus });
      return Response.json(
        {
          session: { id: "live_123" },
          transport: { type: "webrtc", sdp: "answer-sdp" },
        },
        { status: 201 },
      );
    },
  });
});
afterEach(async () => {
  Object.assign(liveTransport, original);
  vi.restoreAllMocks();
  await app.close();
  for (const d of ["incoming", "datasets"])
    await rm(resolve(config.dataDir, d, scope), {
      recursive: true,
      force: true,
    });
});

// The workspace model: resolves voice requests with the given answers in
// turn, routes questions to answers, plans the query and summarises it.
function model(resolutions: Record<string, unknown>[]) {
  const queue = [...resolutions];
  return vi
    .spyOn(Provider.prototype, "generate")
    .mockImplementation(async (system: string) => ({
      value: system.includes("live voice conversation")
        ? { question: "", clarification: "", ...queue.shift() }
        : system.includes("Choose answer")
          ? { intent: "answer" }
          : system.includes("Summarise this query result")
            ? { summary: "Revenue was $65 across 2 orders.", highlights: [] }
            : plan,
      tokens: 1,
    }));
}
async function startSession(chatId?: string) {
  const r = await request("POST", "/live/sessions", {
    sdp: "offer-sdp",
    chatId,
  });
  expect(r.statusCode, r.body).toBe(200);
  return { body: r.json(), socket: sockets.at(-1)! };
}
const said = (s: FakeSocket, id: string) =>
  s.of("session.commentary.append").filter((c) => c.delegation_id === id);
async function until(check: () => unknown, ms = 3000) {
  const end = Date.now() + ms;
  while (!check()) {
    if (Date.now() > end) throw new Error("Timed out");
    await new Promise((r) => setTimeout(r, 5));
  }
}
// The worker's part: run every queued Analyst question.
async function work() {
  for (const r of await store.list(scope, "run"))
    if (r.data.status === "queued") await buildWorkspace(store, scope, r.id);
}
function speak(s: FakeSocket, text: string, at: number) {
  s.emit({
    type: "session.input_transcript.delta",
    delta: text,
    start_ms: at,
    end_ms: at + 400,
  });
}

it("offers voice only for OpenAI projects with GPT-Live access", async () => {
  await useProvider("claude");
  expect((await request("GET", "/live")).json()).toMatchObject({
    available: false,
  });
  const denied = await request("POST", "/live/sessions", { sdp: "offer" });
  expect(denied.statusCode).toBe(409);

  await useProvider("openai");
  liveStatus = 404;
  const missing = (await request("GET", "/live")).json();
  expect(missing.available).toBe(false);
  expect(missing.reason).toContain("GPT-Live");
  expect(
    (await request("POST", "/live/sessions", { sdp: "o" })).json().error,
  ).toContain("doesn't have access to GPT-Live");
});

it("creates a GPT-Live session with client delegation and attaches the sideband", async () => {
  await useProvider("openai");
  liveStatus = 201;
  expect((await request("GET", "/live")).json()).toEqual({ available: true });
  const { body, socket } = await startSession();
  expect(body).toEqual({
    sessionId: "live_123",
    sdp: "answer-sdp",
    chatId: null,
  });
  const created = openai.find((c) => c.url.endsWith("/live/sessions"))!;
  expect(created.headers.authorization).toBe("Bearer sk-test-key");
  expect(created.body.session).toMatchObject({
    model: "gpt-live-1",
    delegation: { type: "client" },
    input: [],
  });
  expect(created.body.transport).toEqual({ type: "webrtc", sdp: "offer-sdp" });
  // The key stays on the server: nothing the browser receives includes it.
  expect(JSON.stringify(body)).not.toContain("sk-test");
  expect(socket.url).toBe(
    "wss://api.openai.com/v1/live/sessions/live_123/attach",
  );
  expect(socket.headers.Authorization).toBe("Bearer sk-test-key");
});

it("answers a spoken question with a real Analyst run and speaks its result", async () => {
  await useProvider("openai");
  const generate = model([
    { action: "ask", question: "What is total revenue?" },
  ]);
  const { socket } = await startSession();
  speak(socket, "What's our total", 1000);
  speak(socket, " revenue?", 1400);
  socket.emit({
    type: "session.delegation.created",
    offset_ms: 1900,
    delegation: { id: "item_1", type: "delegation", target: "client" },
  });
  // The browser claims the same delegation: one task, one run.
  const claim = await request(
    "POST",
    "/live/sessions/live_123/delegations/item_1",
    {},
  );
  expect(claim.statusCode, claim.body).toBe(200);
  const c = claim.json();
  expect(c).toMatchObject({
    status: "running",
    action: "ask",
    question: "What is total revenue?",
  });
  expect(c.chatId).toBe(c.runId);
  // The resolver read the user's words from the transcript.
  const resolverCall = generate.mock.calls.find((x) =>
    String(x[0]).includes("live voice conversation"),
  )!;
  expect((resolverCall[1] as any).transcript).toEqual([
    "user: What's our total revenue?",
  ]);
  const run = (await store.get(scope, c.runId))!;
  expect(run.data).toMatchObject({
    goal: "What is total revenue?",
    intent: "answer",
    context: { page: "voice", voice: { delegationId: "item_1" } },
  });
  // A quiet acknowledgment first, never a figure.
  expect(socket.of("session.thinking.append")[0]).toMatchObject({
    delegation_id: "item_1",
  });
  expect(socket.of("session.thinking.append")[0].content).not.toMatch(/\d/);
  await work();
  await until(() => said(socket, "item_1").length);
  const spoken = said(socket, "item_1")[0].content;
  // One value: said aloud with its number.
  expect(spoken).toContain("Answer: revenue: 65");
  expect(spoken).toContain("Revenue excludes refunds");
  expect(spoken).toContain("Source: Orders");
  // The acknowledgment records delivery for latency reporting.
  socket.emit({
    type: "session.commentary.appended",
    client_event_id: said(socket, "item_1")[0].event_id,
  });
  const state = (await request("GET", "/live/sessions/live_123")).json();
  expect(state.tasks[0].timings).toMatchObject({
    resolved: expect.any(Number),
    enqueued: expect.any(Number),
    finished: expect.any(Number),
    answerSent: expect.any(Number),
    answerDelivered: expect.any(Number),
  });
  // Only one run, however many times the delegation was reported.
  socket.emit({
    type: "session.delegation.created",
    offset_ms: 1900,
    delegation: { id: "item_1", type: "delegation", target: "client" },
  });
  await request("POST", "/live/sessions/live_123/delegations/item_1", {});
  expect(
    (await store.list(scope, "run")).filter((r) => !r.data.task),
  ).toHaveLength(1);
});

it("keeps follow-ups in the same chat and reuses a question already running", async () => {
  await useProvider("openai");
  model([
    { action: "ask", question: "What is total revenue?" },
    { action: "ask", question: "What is total revenue?" },
    { action: "ask", question: "Which traffic sources brought that revenue?" },
  ]);
  const { socket } = await startSession();
  speak(socket, "What's our total revenue?", 1000);
  socket.emit({
    type: "session.delegation.created",
    offset_ms: 1500,
    delegation: { id: "item_1", target: "client" },
  });
  const first = (
    await request("POST", "/live/sessions/live_123/delegations/item_1", {})
  ).json();
  // The user interrupts the acknowledgment and repeats themselves while it
  // runs: the running lookup is reused, not replayed.
  speak(socket, "I said total revenue", 2000);
  const again = (
    await request("POST", "/live/sessions/live_123/delegations/item_2", {})
  ).json();
  expect(again.runId).toBe(first.runId);
  await work();
  await until(
    () => said(socket, "item_1").length && said(socket, "item_2").length,
  );

  speak(socket, "Which traffic sources changed?", 5000);
  const follow = (
    await request("POST", "/live/sessions/live_123/delegations/item_3", {})
  ).json();
  expect(follow.runId).not.toBe(first.runId);
  expect(follow.chatId).toBe(first.chatId);
  const run = (await store.get(scope, follow.runId))!;
  expect(run.data.threadId).toBe(first.chatId);
  expect(run.data.context.resourceId).toBe(first.runId);
  const runs = (await store.list(scope, "run")).filter((r) => !r.data.task);
  expect(runs).toHaveLength(2);
});

it("starts inside an existing chat with its history", async () => {
  await useProvider("openai");
  model([{ action: "ask", question: "And by source?" }]);
  const typed = await request("POST", "/assistant/runs", {
    goal: "What is total revenue?",
    intent: "auto",
  });
  await work();
  const chatId = typed.json().id;
  const { body } = await startSession(chatId);
  expect(body.chatId).toBe(chatId);
  const created = openai.find((c) => c.url.endsWith("/live/sessions"))!;
  expect(created.body.session.input).toEqual([
    {
      type: "message",
      role: "user",
      content: [{ type: "input_text", text: "What is total revenue?" }],
    },
    {
      type: "message",
      role: "assistant",
      content: [
        { type: "output_text", text: "Revenue was $65 across 2 orders." },
      ],
    },
  ]);
  const follow = (
    await request("POST", "/live/sessions/live_123/delegations/item_1", {})
  ).json();
  expect(follow.chatId).toBe(chatId);
  expect((await store.get(scope, follow.runId))!.data.threadId).toBe(chatId);
});

it("does not rerun the chat's latest question after a reconnect", async () => {
  await useProvider("openai");
  model([
    { action: "ask", question: "What is total revenue?" },
    { action: "ask", question: "what is total revenue" },
  ]);
  await startSession();
  const first = (
    await request("POST", "/live/sessions/live_123/delegations/item_1", {})
  ).json();
  // The connection drops; the replacement session resumes the same chat and
  // GPT-Live asks for the same lookup again.
  const { socket } = await startSession(first.chatId);
  const again = (
    await request("POST", "/live/sessions/live_123/delegations/item_9", {})
  ).json();
  expect(again.runId).toBe(first.runId);
  await work();
  await until(() => said(socket, "item_9").length);
  expect(said(socket, "item_9")[0].content).toContain("revenue: 65");
  expect(
    (await store.list(scope, "run")).filter((r) => !r.data.task),
  ).toHaveLength(1);
});

it("clarifies, chats or ends without running a query", async () => {
  await useProvider("openai");
  model([
    { action: "clarify", clarification: "Which period do you mean?" },
    { action: "none" },
    { action: "end" },
  ]);
  const { socket } = await startSession();
  for (const id of ["item_1", "item_2", "item_3"]) {
    const c = (
      await request("POST", `/live/sessions/live_123/delegations/${id}`, {})
    ).json();
    expect(c.runId).toBeNull();
  }
  expect(said(socket, "item_1")[0].content).toContain(
    "Which period do you mean?",
  );
  expect(said(socket, "item_3")[0].content).toMatch(/goodbye/);
  const end = (
    await request("POST", "/live/sessions/live_123/delegations/item_3", {})
  ).json();
  expect(end.action).toBe("end");
  expect(await store.list(scope, "run")).toHaveLength(0);
});

it("reports failures and keeps superseded results quiet", async () => {
  await useProvider("openai");
  model([
    { action: "ask", question: "What is total revenue?" },
    { action: "ask", question: "What is revenue by source?" },
  ]);
  const { socket } = await startSession();
  const first = (
    await request("POST", "/live/sessions/live_123/delegations/item_1", {})
  ).json();
  // A corrected request arrives before the first answer.
  const second = (
    await request("POST", "/live/sessions/live_123/delegations/item_2", {})
  ).json();
  await buildWorkspace(store, scope, first.runId);
  await until(() =>
    socket
      .of("session.thinking.append")
      .some(
        (c) =>
          c.delegation_id === "item_1" && c.content.includes("earlier lookup"),
      ),
  );
  expect(said(socket, "item_1")).toHaveLength(0);
  // The newer lookup fails: the user hears that, with no figures.
  await transaction(store, scope, async (tx) => {
    const r = (await tx.get(second.runId))!;
    tx.update(r, {
      ...r.data,
      status: "failed",
      error: "Binder Error: no column",
    });
  });
  await until(() => said(socket, "item_2").length);
  expect(said(socket, "item_2")[0].content).toContain("didn't complete");
});

it("lets only the session's owner use it, and closes the sideband", async () => {
  await useProvider("openai");
  const { socket } = await startSession();
  const other = await app.inject({
    method: "GET",
    url: `/api/v1/workspaces/someone-else/live/sessions/live_123`,
  });
  expect(other.statusCode).toBe(404);
  expect(
    (await request("GET", "/live/sessions/not-a-session")).statusCode,
  ).toBe(404);
  socket.emit({ type: "session.closed", reason: "close_requested" });
  expect(socket.closed).toBe(true);
  expect(
    (await request("GET", "/live/sessions/live_123")).json(),
  ).toMatchObject({ closed: true, closeReason: "close_requested" });
});

it("says a single answer aloud and gives a table only as a headline", () => {
  const run = (rows: Record<string, unknown>[]) =>
    ({
      data: {
        status: "succeeded",
        answer: {
          title: "Orders by region",
          rows,
          total: rows.length,
          citations: [{ name: "Orders" }],
        },
      },
    }) as unknown as Resource;
  const one = resultText(
    "Top region?",
    run([{ region_name: "North America", order_count: "8473" }]),
  );
  expect(one).toContain(
    "Answer: region name: North America, order count: 8473.",
  );
  expect(one).toContain("Say this answer aloud");
  const table = resultText(
    "Orders by region?",
    run([
      { region: "North America", orders: 8473 },
      { region: "Europe", orders: 8100 },
      { region: "Asia", orders: 7000 },
    ]),
  );
  expect(table).toContain("a table of 3 rows, shown in the chat");
  expect(table).toContain("First row: region: North America, orders: 8473");
  expect(table).not.toContain("Europe");
  expect(table).toContain("Do not read out the table");
  // It never asks the user to read the result.
  for (const text of [one, table])
    expect(text).not.toMatch(/read .* to you|ask the user to read/i);
});

it("shares one run between overlapping requests for the same question", async () => {
  await useProvider("openai");
  model([
    { action: "ask", question: "What is total revenue?" },
    { action: "ask", question: "What is total revenue?" },
  ]);
  await startSession();
  const [a, b] = await Promise.all(
    ["item_1", "item_2"].map(async (id) =>
      (
        await request("POST", `/live/sessions/live_123/delegations/${id}`, {})
      ).json(),
    ),
  );
  expect(a.runId).toBeTruthy();
  expect(b.runId).toBe(a.runId);
  expect(
    (await store.list(scope, "run")).filter((r) => !r.data.task),
  ).toHaveLength(1);
});

it("keeps results until the sideband is open, and waits for the user's words", async () => {
  await useProvider("openai");
  const generate = model([
    { action: "ask", question: "What is total revenue?" },
  ]);
  Object.assign(liveTransport, { settleMs: 30, settleMaxMs: 400 });
  const { socket } = await startSession();
  socket.readyState = 0;
  // The delegation arrives before any transcript; the words follow.
  socket.emit({
    type: "session.delegation.created",
    delegation: { id: "item_1", target: "client" },
  });
  setTimeout(() => speak(socket, "What's our total revenue?", 1000), 10);
  const c = (
    await request("POST", "/live/sessions/live_123/delegations/item_1", {})
  ).json();
  const call = generate.mock.calls.find((x) =>
    String(x[0]).includes("live voice conversation"),
  )!;
  expect((call[1] as any).transcript).toEqual([
    "user: What's our total revenue?",
  ]);
  await work();
  await new Promise((r) => setTimeout(r, 50));
  expect(said(socket, "item_1")).toHaveLength(0);
  socket.open();
  await until(() => said(socket, "item_1").length);
  expect(socket.of("session.thinking.append")).toHaveLength(1);
  expect(c.runId).toBeTruthy();
});

it("counts reading each request against the daily budget", async () => {
  await useProvider("openai");
  const generate = model([{ action: "none" }]);
  generate.mockImplementationOnce(async () => ({
    value: { action: "none", question: "", clarification: "" },
    tokens: 150,
  }));
  const { socket } = await startSession();
  await request("POST", "/live/sessions/live_123/delegations/item_1", {});
  const usage = await store.list(scope, "usage");
  expect(usage[0].data.tokens).toBe(150);
  // With the budget used up, no further model call is made.
  await transaction(store, scope, async (tx) => {
    const r = (await tx.get("provider"))!;
    tx.update(r, {
      ...r.data,
      settings: { ...r.data.settings, dailyTokens: 1000 },
    });
    const u = (await tx.get(usage[0].id))!;
    tx.update(u, { ...u.data, tokens: 5000 });
  });
  const calls = generate.mock.calls.length;
  const blocked = (
    await request("POST", "/live/sessions/live_123/delegations/item_2", {})
  ).json();
  expect(blocked.status).toBe("failed");
  expect(blocked.error).toContain("daily AI budget");
  expect(generate.mock.calls.length).toBe(calls);
  expect(said(socket, "item_2")[0].content).toContain("daily AI budget");
});
