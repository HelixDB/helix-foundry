import { createHash } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import { Readable } from "node:stream";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { AgentEvent, ListAgentTurnEventsResponse } from "@/shared/wfe";
import {
  EMPTY_STREAM,
  parseAgentEvent,
  reduceTurnEvents,
} from "@/web/areas/agent/model";
import { OLLAMA_MODEL } from "@/web/areas/workspace/ai-api";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { handleDocumentUpload } from "../../mock/routes/documents";
import { config, db, resetState } from "../../mock/state";

// The mock WFE's Assistant (mock/routes/agent.ts) behaves as the backend's
// API does for the dock: private threads, turns that stream events in the
// backend's shapes (each one the dock's parser reads), a load prepared as a
// card and run only by its person, with the hash they were shown, once,
// while its confirmation lives and the thread is idle; a continuation turn
// after; dismiss and refresh; and its refusals, with their reasons.

/** The mock's routes as called by a signed-in person (Ada unless named). */
function harness(rs: typeof routes, users: typeof db.users) {
  const compiled = rs.map((r) => ({ ...r, c: compileTemplate(r.path) }));
  return async function call(
    method: HttpMethod,
    path: string,
    body?: unknown,
    userId = "usr_ada",
  ): Promise<MockReply & { body: any }> {
    const url = new URL(path, "http://mock");
    for (const r of compiled) {
      const params = r.method === method && matchTemplate(r.c, url.pathname);
      if (!params) continue;
      return (await r.handle({
        method,
        path: url.pathname,
        params,
        query: url.searchParams,
        body,
        principal: { user: users.get(userId)!, sessionId: "s" },
      })) as MockReply & { body: any };
    }
    throw new Error(`no mock route for ${method} ${path}`);
  };
}
const call = harness(routes, db.users);

const A = "/v1/workspaces/ws_acme/foundry/agent";
const later = (ms: number) => vi.setSystemTime(Date.now() + ms);
const reason = (r: { body: any }) => r.body.details?.[0]?.reason;
let token = 0;
const send = (text: string, extra: object = {}, userId = "usr_ada") =>
  call(
    "POST",
    `${A}/turns`,
    { text, clientToken: `t${++token}`, ...extra },
    userId,
  );

/** Every event of a turn, once it has ended. */
async function events(turnId: string, userId = "usr_ada") {
  later(60_000);
  const r = await call(
    "GET",
    `${A}/turns/${turnId}/events?afterSeq=0`,
    undefined,
    userId,
  );
  const page = r.body as ListAgentTurnEventsResponse;
  const parsed = (page.events || []).map((e) => parseAgentEvent(e.eventJson));
  expect(parsed.every(Boolean)).toBe(true);
  return { page, events: parsed as AgentEvent[] };
}

async function prepareLoad(
  text = "Load Customer contracts",
  userId = "usr_ada",
) {
  const sent = await send(text, {}, userId);
  expect(sent.status).toBe(200);
  const { events: evs } = await events(sent.body.turn.id, userId);
  const thread = await call(
    "GET",
    `${A}/threads/${sent.body.thread.id}`,
    undefined,
    userId,
  );
  return {
    sent,
    events: evs,
    thread: thread.body,
    action: thread.body.actions?.[0],
  };
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-14T12:00:00Z"));
  resetState();
  token = 0;
});
afterEach(() => vi.useRealTimers());

describe("messages", () => {
  it("answers a question from the read tools, as events the dock folds", async () => {
    const sent = await send("What ontologies do I have?");
    expect(sent.body.turn).toMatchObject({ status: "queued", kind: "message" });
    expect(sent.body.thread.title).toBe("What ontologies do I have?");
    const { page, events: evs } = await events(sent.body.turn.id);
    expect(page.status).toBe("done");
    expect(evs.map((e) => e.type)).toEqual([
      "started",
      "tool_call",
      "tool_result",
      "text",
      "finished",
    ]);
    const text = evs.find((e) => e.type === "text");
    expect(text).toMatchObject({
      text: expect.stringContaining("Customer contracts"),
    });
    const stream = reduceTurnEvents(EMPTY_STREAM, page);
    expect(stream.complete).toBe(true);
    expect(stream.unreadable).toBe(0);
  });

  it("reads the ontology the page names", async () => {
    const sent = await send("What is this?", {
      pageContext: { ontologyId: "ont_acme_sales" },
    });
    const { events: evs } = await events(sent.body.turn.id);
    expect(
      evs.find(
        (e) =>
          e.type === "tool_call" && e.name === "helix_foundry_get_ontology",
      ),
    ).toMatchObject({
      input: { ontology_id: "ont_acme_sales" },
    });
    expect(evs.find((e) => e.type === "text")).toMatchObject({
      text: expect.stringContaining("Sales graph is on the sales database"),
    });
  });

  it("lists every ontology when asked about them, whatever page it is on", async () => {
    const sent = await send("What ontologies do I have?", {
      pageContext: { ontologyId: "ont_acme_sales" },
    });
    const { events: evs } = await events(sent.body.turn.id);
    expect(
      evs.some(
        (e) =>
          e.type === "tool_call" && e.name === "helix_foundry_get_ontology",
      ),
    ).toBe(false);
    expect(evs.find((e) => e.type === "text")).toMatchObject({
      text: expect.stringMatching(/^This workspace has \d+ ontologies: /),
    });
  });

  it("returns the same turn for the same client token, and refuses it for another text", async () => {
    const body = { text: "Hi", clientToken: "same" };
    const first = await call("POST", `${A}/turns`, body);
    const again = await call("POST", `${A}/turns`, body);
    expect(again.body.turn.id).toBe(first.body.turn.id);
    const other = await call("POST", `${A}/turns`, { ...body, text: "Bye" });
    expect(other.status).toBe(400);
  });

  it("refuses what the backend refuses", async () => {
    const bad = [
      await call("POST", `${A}/turns`, { text: "", clientToken: "a" }),
      await call("POST", `${A}/turns`, {
        text: "x".repeat(4001),
        clientToken: "b",
      }),
      await call("POST", `${A}/turns`, { text: "x", clientToken: "" }),
      await call("POST", `${A}/turns`, {
        text: "x",
        clientToken: "c".repeat(65),
      }),
      await call("POST", `${A}/turns`, {
        text: "x",
        clientToken: "d",
        threadId: "a b",
      }),
      await call("POST", `${A}/turns`, {
        text: "x",
        clientToken: "e",
        pageContext: { loadId: "l" },
      }),
    ];
    expect(bad.map((r) => r.status)).toEqual([400, 400, 400, 400, 400, 400]);
    const missing = await send("x", { threadId: "thr_none" });
    expect(missing.status).toBe(404);
  });

  it("takes no message in a thread while a turn of it is live", async () => {
    const first = await send("Hi");
    const busy = await send("Again", { threadId: first.body.thread.id });
    expect([busy.status, reason(busy)]).toEqual([
      400,
      "FOUNDRY_AGENT_THREAD_BUSY",
    ]);
    later(60_000);
    expect(
      (await send("Again", { threadId: first.body.thread.id })).status,
    ).toBe(200);
  });

  it("refuses past 30 messages an hour", async () => {
    for (let i = 0; i < 30; i++) expect((await send(`m${i}`)).status).toBe(200);
    const over = await send("one more");
    expect(over.status).toBe(429);
    later(3_600_001);
    expect((await send("one more")).status).toBe(200);
  });

  it("keeps threads private: another person's answers NOT_FOUND", async () => {
    const sent = await send("Hi");
    const thread = sent.body.thread.id,
      turn = sent.body.turn.id;
    expect(
      (await call("GET", `${A}/threads/${thread}`, undefined, "usr_alan"))
        .status,
    ).toBe(404);
    expect(
      (await call("GET", `${A}/turns/${turn}/events`, undefined, "usr_alan"))
        .status,
    ).toBe(404);
    expect(
      (await call("POST", `${A}/turns/${turn}:cancel`, {}, "usr_alan")).status,
    ).toBe(404);
    expect(
      (await call("DELETE", `${A}/threads/${thread}`, undefined, "usr_alan"))
        .status,
    ).toBe(404);
    expect((await send("x", { threadId: thread }, "usr_alan")).status).toBe(
      404,
    );
    const alans = await call("GET", `${A}/threads`, undefined, "usr_alan");
    expect(alans.body.threads).toEqual([]);
  });

  it("lists threads newest first, 50 a page", async () => {
    for (let i = 0; i < 52; i++) {
      await send(`m${i}`);
      // Under the 30 messages an hour.
      later(150_000);
    }
    const first = await call("GET", `${A}/threads`);
    expect(first.body.threads).toHaveLength(50);
    expect(first.body.threads[0].title).toBe("m51");
    const next = await call(
      "GET",
      `${A}/threads?pageToken=${first.body.nextPageToken}`,
    );
    expect(next.body.threads.map((t: { title: string }) => t.title)).toEqual([
      "m1",
      "m0",
    ]);
    expect(next.body.nextPageToken).toBeUndefined();
    for (const bad of ["x", "7", "-50", "0"]) {
      const refused = await call("GET", `${A}/threads?pageToken=${bad}`);
      expect([refused.status, refused.body.message]).toEqual([
        400,
        "page_token is not one this list returned",
      ]);
    }
  });

  it("stops a live turn, returns a stopped one again, and refuses an ended one", async () => {
    const sent = await send("Hi");
    const stop = await call(
      "POST",
      `${A}/turns/${sent.body.turn.id}:cancel`,
      {},
    );
    expect(stop.body.turn.status).toBe("cancelled");
    const again = await call(
      "POST",
      `${A}/turns/${sent.body.turn.id}:cancel`,
      {},
    );
    expect([again.status, again.body.turn.status]).toEqual([200, "cancelled"]);
    const done = await send("Hi again");
    await events(done.body.turn.id);
    const ended = await call(
      "POST",
      `${A}/turns/${done.body.turn.id}:cancel`,
      {},
    );
    expect([ended.status, ended.body.message]).toEqual([
      400,
      "the turn already ended",
    ]);
  });

  it.each([
    [
      "OpenAI",
      {
        provider: { openai: {} },
        apiKey: "sk-proj-sentinel-0123456789abcdefwxyz",
      },
      "gpt-6.1-sol",
    ],
    [
      "Azure OpenAI",
      {
        provider: {
          azureOpenai: { endpoint: "https://acme.openai.azure.com" },
        },
        models: Object.fromEntries(
          ["draft", "ask", "extract", "verify"].map((r) => [
            r,
            { name: "prod-gpt", tableAs: "gpt-6.1-sol" },
          ]),
        ),
        apiKey: "0123456789abcdef0123456789abcdef",
      },
      "prod-gpt",
    ],
    [
      "Amazon Bedrock",
      {
        provider: { bedrock: { region: "us-east-1" } },
        apiKey: "ABSKsentinelQmVkcm9ja0FQSUtleS0wMTIz",
      },
      "global.anthropic.claude-sonnet-5-5",
    ],
    [
      "a custom server",
      {
        provider: {
          openaiCompatible: {
            baseUrl: "https://llm.example.com/v1",
            api: "FOUNDRY_AI_COMPAT_API_RESPONSES",
          },
        },
        models: Object.fromEntries(
          ["draft", "ask", "extract", "verify"].map((r) => [
            r,
            { name: "qwen-big", free: {} },
          ]),
        ),
      },
      "qwen-big",
    ],
  ])("answers on %s, on its Questions model", async (_, save, model) => {
    const saved = await call("PUT", "/v1/workspaces/ws_acme/foundry/ai", save);
    expect(saved.status).toBe(200);
    // Past the save's test, it is the setting in force.
    later(5_000);
    const sent = await send("What ontologies do I have?");
    const { page, events: evs } = await events(sent.body.turn.id);
    expect(page.status).toBe("done");
    expect(evs[0]).toEqual({ type: "started", model, attempt: 1 });
    expect(evs.map((e) => e.type)).toEqual([
      "started",
      "tool_call",
      "tool_result",
      "text",
      "finished",
    ]);
  });

  it.each([
    [
      "qwen-notoolcall",
      "The workspace's model, qwen-notoolcall, doesn't call tools, which the Assistant needs. An owner can choose another in Settings › AI models.",
    ],
    [
      "qwen-toolrefused",
      "llm.example.com refused the Assistant's calls to qwen-toolrefused. An owner can choose another model in Settings › AI models.",
    ],
  ])(
    "fails a turn at once while the setting's tool check failed on %s, in the worker's words",
    async (ask, error) => {
      const saved = await call("PUT", "/v1/workspaces/ws_acme/foundry/ai", {
        provider: {
          openaiCompatible: {
            baseUrl: "https://llm.example.com/v1",
            api: "FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS",
          },
        },
        models: {
          ...Object.fromEntries(
            ["draft", "extract", "verify"].map((r) => [
              r,
              { name: "qwen-big", free: {} },
            ]),
          ),
          ask: { name: ask, free: {} },
        },
      });
      expect(saved.status).toBe(200);
      later(5_000);
      const sent = await send("What ontologies do I have?");
      expect(sent.status).toBe(200);
      const { page, events: evs } = await events(sent.body.turn.id);
      expect(page.status).toBe("failed");
      expect(evs).toEqual([
        { type: "finished", status: "failed", reason: "failed", error },
      ]);
    },
  );

  it("streams a step's text as deltas, which its text replaces in one write", async () => {
    Object.assign(config, { agentStepMs: 1000 });
    const sent = await send("What ontologies do I have?");
    const turn = sent.body.turn.id;
    const read = async (after: number) =>
      (await call("GET", `${A}/turns/${turn}/events?afterSeq=${after}`))
        .body as ListAgentTurnEventsResponse;
    // Queued a step, then started, its call and result a step apart: its
    // text starts streaming at 5 s.
    later(5_100);
    const first = await read(0);
    const types = (p: ListAgentTurnEventsResponse) =>
      (p.events || []).map((e) => parseAgentEvent(e.eventJson)!.type);
    expect(types(first)).toEqual([
      "started",
      "tool_call",
      "tool_result",
      "text_delta",
    ]);
    // The dock's polls, every 250 ms until the text lands.
    let stream = reduceTurnEvents(EMPTY_STREAM, first);
    const deltas: string[] = [];
    const take = (p: ListAgentTurnEventsResponse) => {
      for (const e of p.events || []) {
        const ev = parseAgentEvent(e.eventJson);
        if (ev?.type === "text_delta") {
          // The call that wrote the text is the turn's second.
          expect(ev.step).toBe(2);
          deltas.push(ev.text);
        }
      }
    };
    take(first);
    expect(stream.live?.step).toBe(2);
    while (!stream.blocks.some((b) => b.kind === "text")) {
      later(250);
      const page = await read(stream.lastSeq);
      take(page);
      stream = reduceTurnEvents(stream, page);
    }
    expect(deltas.length).toBeGreaterThan(1);
    const text = stream.blocks.find((b) => b.kind === "text");
    // The deltas put together are the text, shown once, in their place.
    expect(text).toEqual({ kind: "text", text: deltas.join("") });
    expect(stream.live).toBeUndefined();
    // Read afresh, the deltas are gone: seqs skip them, and the turn's
    // last seq is its last event's.
    later(60_000);
    const all = await read(0);
    expect(types(all)).toEqual([
      "started",
      "tool_call",
      "tool_result",
      "text",
      "finished",
    ]);
    const seqs = (all.events || []).map((e) => e.seq);
    expect(seqs.slice(0, 3)).toEqual([1, 2, 3]);
    expect(seqs[3]).toBe(4 + deltas.length);
    expect(all.lastSeq).toBe(seqs.at(-1));
    expect(parseAgentEvent(all.events![3]!.eventJson)).toEqual({
      type: "text",
      step: 2,
      text: deltas.join(""),
    });
    const fresh = reduceTurnEvents(EMPTY_STREAM, all);
    expect(fresh.blocks).toEqual(
      reduceTurnEvents(stream, await read(stream.lastSeq)).blocks,
    );
    expect(fresh.complete).toBe(true);
    // The turn counts its two model calls.
    const thread = await call("GET", `${A}/threads/${sent.body.thread.id}`);
    expect(thread.body.turns[0]).toMatchObject({
      steps: 2,
      lastSeq: seqs.at(-1),
    });
  });

  it("leaves a stopped turn's deltas, which the dock shows nothing of", async () => {
    Object.assign(config, { agentStepMs: 1000 });
    const sent = await send("What ontologies do I have?");
    later(5_100);
    const stop = await call(
      "POST",
      `${A}/turns/${sent.body.turn.id}:cancel`,
      {},
    );
    expect(stop.body.turn.status).toBe("cancelled");
    later(60_000);
    const r = await call(
      "GET",
      `${A}/turns/${sent.body.turn.id}/events?afterSeq=0`,
    );
    const page = r.body as ListAgentTurnEventsResponse;
    expect(page.status).toBe("cancelled");
    expect(
      (page.events || []).map((e) => parseAgentEvent(e.eventJson)!.type).at(-1),
    ).toBe("text_delta");
    const stream = reduceTurnEvents(EMPTY_STREAM, page);
    expect(stream.complete).toBe(true);
    expect(stream.live).toBeUndefined();
  });

  it("deletes a thread, stopping its live turn", async () => {
    const sent = await send("Hi");
    expect(
      (await call("DELETE", `${A}/threads/${sent.body.thread.id}`)).status,
    ).toBe(200);
    expect(
      (await call("GET", `${A}/threads/${sent.body.thread.id}`)).status,
    ).toBe(404);
    expect((await call("GET", `${A}/threads`)).body.threads).toEqual([]);
  });

  it.each([
    ["fail", "failed", { status: "failed", reason: "failed" }],
    ["budget_exhausted", "done", { status: "done", reason: "budget" }],
  ] as const)(
    "ends a turn as agentOutcome %s says",
    async (outcome, status, finished) => {
      Object.assign(config, { agentOutcome: outcome });
      const sent = await send("Load Customer contracts");
      const { page, events: evs } = await events(sent.body.turn.id);
      expect(page.status).toBe(status);
      expect(evs.map((e) => e.type)).toEqual([
        "started",
        "tool_call",
        "tool_result",
        "finished",
      ]);
      expect(evs.at(-1)).toMatchObject(finished);
    },
  );

  it("paces its events by agentStepMs", async () => {
    Object.assign(config, { agentStepMs: 1000 });
    const sent = await send("Hi");
    later(1500);
    const r = await call(
      "GET",
      `${A}/turns/${sent.body.turn.id}/events?afterSeq=0`,
    );
    expect(r.body.status).toBe("running");
    expect(r.body.events).toHaveLength(0);
    later(1000);
    const next = await call(
      "GET",
      `${A}/turns/${sent.body.turn.id}/events?afterSeq=0`,
    );
    expect(next.body.events).toHaveLength(1);
  });
});

describe("a local install", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.resetModules();
  });

  it("answers on its Ollama", async () => {
    vi.resetModules();
    vi.stubEnv("MOCK_LOCAL_TOKEN", "local-token");
    vi.stubEnv("MOCK_AI_DEFAULT", "anthropic");
    const local = await import("../../mock/state");
    const at = harness(
      (await import("../../mock/routes")).routes,
      local.db.users,
    );
    local.resetState();
    // The install's default: its bundled Ollama's model for every task.
    Object.assign(local.config, { aiDefaultKind: "ollama" });
    const sent = await at("POST", `${A}/turns`, {
      text: "What ontologies do I have?",
      clientToken: "local-1",
    });
    expect(sent.status).toBe(200);
    later(60_000);
    const page = (
      await at("GET", `${A}/turns/${sent.body.turn.id}/events?afterSeq=0`)
    ).body as ListAgentTurnEventsResponse;
    expect(page.status).toBe("done");
    expect(parseAgentEvent(page.events![0]!.eventJson)).toEqual({
      type: "started",
      model: OLLAMA_MODEL,
      attempt: 1,
    });
  });
});

describe("where the Assistant runs", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.resetModules();
  });

  it("refuses every Assistant route while the Assistant is off", async () => {
    Object.assign(config, { agentEnabled: false });
    for (const [method, path] of [
      ["GET", `${A}/threads`],
      ["POST", `${A}/turns`],
      ["GET", `${A}/runtime`],
      ["POST", `${A}/host-pairings`],
    ] as const) {
      const got = await call(method, path, method === "POST" ? {} : undefined);
      expect(got.status, path).toBe(400);
      expect(reason(got), path).toBe("FOUNDRY_AGENT_DISABLED");
    }
    resetState();
    expect((await call("GET", `${A}/threads`)).status).toBe(200);
  });

  it("is the worker's alone hosted", async () => {
    const got = await call("GET", `${A}/runtime`);
    expect(got.status).toBe(400);
    expect(reason(got)).toBe("FOUNDRY_AGENT_HOSTS_LOCAL_ONLY");
    const sent = await send("What ontologies do I have?");
    expect(sent.body.thread.runtime).toBe("api");
  });

  it("is the owner's choice on a local install, sent only while a helper runs it", async () => {
    vi.resetModules();
    vi.stubEnv("MOCK_LOCAL_TOKEN", "local-token");
    vi.stubEnv("MOCK_AI_DEFAULT", "anthropic");
    const local = await import("../../mock/state");
    const at = harness(
      (await import("../../mock/routes")).routes,
      local.db.users,
    );
    local.resetState();
    const before = await at("GET", `${A}/runtime`);
    expect(before.body).toEqual({
      runtime: {
        runtime: "api",
        hosts: [],
        online: [],
        codexConfirmedAt: null,
        codexMinVersion: "0.169.0",
      },
    });
    expect((await at("PATCH", `${A}/runtime`, { runtime: "x" })).status).toBe(
      400,
    );
    expect(
      reason(await at("PATCH", `${A}/runtime`, { runtime: "codex" })),
    ).toBe("FOUNDRY_AGENT_CODEX_UNCONFIRMED");
    const chosen = await at("PATCH", `${A}/runtime`, {
      runtime: "claude_code",
    });
    expect(chosen.body.runtime.runtime).toBe("claude_code");

    // No helper runs it: nothing is queued.
    const offline = await at("POST", `${A}/turns`, {
      text: "What ontologies do I have?",
      clientToken: "sub-1",
    });
    expect(reason(offline)).toBe("FOUNDRY_AGENT_HOST_OFFLINE");

    const pairing = await at("POST", `${A}/host-pairings`, {});
    expect(pairing.body.code).toMatch(/^hxp_/);
    expect(pairing.body.command).toBe(
      `npx -y @helixdb/foundry-agent-host@0.1.0 pair ${pairing.body.code}`,
    );
    Object.assign(local.config, {
      agentHosts: [{ name: "Ada's Mac", runtimes: ["claude_code"] }],
    });
    const listed = (await at("GET", `${A}/runtime`)).body.runtime;
    expect(listed.online).toEqual(["claude_code"]);
    expect(listed.hosts[0]).toMatchObject({ name: "Ada's Mac", online: true });
    const sent = await at("POST", `${A}/turns`, {
      text: "What ontologies do I have?",
      clientToken: "sub-2",
    });
    expect(sent.status).toBe(200);
    expect(sent.body.thread.runtime).toBe("claude_code");

    const revoked = await at(
      "POST",
      `${A}/hosts/${listed.hosts[0].id}:revoke`,
      {},
    );
    expect(revoked.body.host.revokedAt).toBeTruthy();
    expect((await at("GET", `${A}/runtime`)).body.runtime.hosts).toEqual([]);
    later(60_000);
    const again = await at("POST", `${A}/turns`, {
      text: "And now?",
      clientToken: "sub-3",
      threadId: sent.body.thread.id,
    });
    expect(reason(again)).toBe("FOUNDRY_AGENT_HOST_OFFLINE");
  });

  it("runs Codex once its owner confirmed it, and not on a Codex too old", async () => {
    vi.resetModules();
    vi.stubEnv("MOCK_LOCAL_TOKEN", "local-token");
    vi.stubEnv("MOCK_AI_DEFAULT", "anthropic");
    const local = await import("../../mock/state");
    const at = harness(
      (await import("../../mock/routes")).routes,
      local.db.users,
    );
    local.resetState();
    const confirmed = await at("PATCH", `${A}/runtime`, {
      runtime: "codex",
      confirmCodex: true,
    });
    expect(confirmed.body.runtime.runtime).toBe("codex");
    const at1 = confirmed.body.runtime.codexConfirmedAt;
    expect(at1).toBeTruthy();
    // Kept: chosen again without asking, its first confirmation kept.
    await at("PATCH", `${A}/runtime`, { runtime: "api" });
    const again = await at("PATCH", `${A}/runtime`, { runtime: "codex" });
    expect(again.body.runtime.codexConfirmedAt).toBe(at1);

    Object.assign(local.config, {
      agentHosts: [
        { name: "Ada's Mac", runtimes: ["claude_code", "codex"] },
        { name: "Old laptop", runtimes: ["codex"], codexVersion: "0.150.0" },
      ],
    });
    const listed = (await at("GET", `${A}/runtime`)).body.runtime;
    expect(listed.online).toEqual(["claude_code", "codex"]);
    expect(listed.hosts[0].accounts).toEqual([
      { runtime: "claude_code", state: "signed_in", subscription: "max" },
      {
        runtime: "codex",
        state: "signed_in",
        subscription: "plus",
        version: "0.169.0",
        outdated: false,
      },
    ]);
    expect(listed.hosts[1].runtimes).toEqual([]);
    expect(listed.hosts[1].accounts[0]).toMatchObject({
      version: "0.150.0",
      outdated: true,
    });
    const sent = await at("POST", `${A}/turns`, {
      text: "What ontologies do I have?",
      clientToken: "cx-1",
    });
    expect(sent.status).toBe(200);
    expect(sent.body.thread.runtime).toBe("codex");

    // Only the old laptop: its Codex runs nothing, and sending says why.
    Object.assign(local.config, {
      agentHosts: [
        { name: "Old laptop", runtimes: ["codex"], codexVersion: "0.150.0" },
      ],
    });
    const old = await at("POST", `${A}/turns`, {
      text: "And now?",
      clientToken: "cx-2",
    });
    expect(reason(old)).toBe("FOUNDRY_AGENT_HOST_OFFLINE");
    expect(old.body.message).toContain(
      "the Codex on Old laptop is older than 0.169.0",
    );

    // A Codex that didn't start (no version) runs nothing either, and isn't called old.
    Object.assign(local.config, {
      agentHosts: [
        {
          name: "Old laptop",
          runtimes: ["codex"],
          account: { state: "unknown" },
          codexVersion: "",
        },
      ],
    });
    const none = (await at("GET", `${A}/runtime`)).body.runtime;
    expect(none.online).toEqual([]);
    expect(none.hosts[0].accounts).toEqual([
      { runtime: "codex", state: "unknown" },
    ]);
    const unstarted = await at("POST", `${A}/turns`, {
      text: "And now?",
      clientToken: "cx-3",
    });
    expect(reason(unstarted)).toBe("FOUNDRY_AGENT_HOST_OFFLINE");
    expect(unstarted.body.message).toContain(
      "the Codex on Old laptop didn't start",
    );
  });

  /** The mock as a local install's API, with its helper's pair stand-in. */
  async function localInstall() {
    vi.resetModules();
    vi.stubEnv("MOCK_LOCAL_TOKEN", "local-token");
    vi.stubEnv("MOCK_AI_DEFAULT", "anthropic");
    const local = await import("../../mock/state");
    const hosts = await import("../../mock/routes/agent-hosts");
    const at = harness(
      (await import("../../mock/routes")).routes,
      local.db.users,
    );
    local.resetState();
    let n = 0;
    /** A message's turn, every event of it once it ended, and the thread's newest action. */
    const turn = async (text: string, threadId?: string) => {
      const sent = await at("POST", `${A}/turns`, {
        text,
        clientToken: `local-${++n}`,
        ...(threadId && { threadId }),
      });
      expect(sent.status).toBe(200);
      later(60_000);
      const page = (
        await at("GET", `${A}/turns/${sent.body.turn.id}/events?afterSeq=0`)
      ).body as ListAgentTurnEventsResponse;
      const evs = (page.events || []).map((e) => parseAgentEvent(e.eventJson)!);
      const thread = await at("GET", `${A}/threads/${sent.body.thread.id}`);
      return {
        sent,
        page,
        events: evs,
        said: evs
          .filter((e) => e.type === "text")
          .map((e) => (e as { text: string }).text)
          .join(" "),
        action: (thread.body.actions || []).at(-1),
      };
    };
    return { at, local, hosts, turn };
  }

  it("pairs a helper with a code its pair command spends once, within ten minutes", async () => {
    const { at, hosts } = await localInstall();
    const pairing = (await at("POST", `${A}/host-pairings`, {})).body;
    expect(hosts.pairHelper({ code: "hxp_unknown" }).status).toBe(404);
    const paired = hosts.pairHelper({ code: pairing.code, name: "Ada's Mac" });
    expect(paired.status).toBe(200);
    expect(hosts.pairHelper({ code: pairing.code }).body).toEqual({
      error: {
        code: "pairing_invalid",
        message:
          "the pairing code is unknown, used or expired: make another in Foundry",
      },
    });
    const hostId = (paired.body as { host_id: string }).host_id;
    // Pairing starts nothing: the helper isn't online, nor ever seen, until
    // its run or install starts it.
    let listed = (await at("GET", `${A}/runtime`)).body.runtime;
    expect(listed.online).toEqual([]);
    expect(listed.hosts).toEqual([
      expect.objectContaining({ id: hostId, name: "Ada's Mac", online: false }),
    ]);
    expect(listed.hosts[0].lastSeenAt).toBeUndefined();
    expect(hosts.startHelper({ host_id: "01JNOPE" }).status).toBe(401);
    expect(hosts.startHelper({ host_id: hostId }).body).toEqual({
      host_id: hostId,
      name: "Ada's Mac",
    });
    listed = (await at("GET", `${A}/runtime`)).body.runtime;
    expect(listed.online).toEqual(["claude_code"]);
    expect(listed.hosts).toEqual([
      expect.objectContaining({
        id: hostId,
        name: "Ada's Mac",
        runtimes: ["claude_code"],
        accounts: [
          { runtime: "claude_code", state: "signed_in", subscription: "max" },
        ],
        online: true,
      }),
    ]);
    expect(listed.hosts[0].lastSeenAt).toBeTruthy();
    // Revoked, it can't start again.
    await at("POST", `${A}/hosts/${hostId}:revoke`, {});
    expect(hosts.startHelper({ host_id: hostId }).status).toBe(401);
    // A code past its ten minutes pairs nothing.
    const late = (await at("POST", `${A}/host-pairings`, {})).body;
    later(10 * 60_000 + 1);
    expect(hosts.pairHelper({ code: late.code }).status).toBe(404);
    // A helper that hasn't said what its CLI is signed in to yet.
    const fresh = (await at("POST", `${A}/host-pairings`, {})).body;
    hosts.pairHelper({ code: fresh.code, name: "Lab", account: null });
    expect(
      (await at("GET", `${A}/runtime`)).body.runtime.hosts[0],
    ).toMatchObject({ name: "Lab", accounts: [] });
  });

  it("runs a helper's thread on Claude Code, and fails it in the backend's words while it is signed out", async () => {
    const { at, local, turn } = await localInstall();
    await at("PATCH", `${A}/runtime`, { runtime: "claude_code" });
    Object.assign(local.config, {
      agentHosts: [{ name: "Ada's Mac", runtimes: ["claude_code"] }],
    });
    const ran = await turn("What ontologies do I have?");
    expect(ran.events[0]).toEqual({
      type: "started",
      model: "Claude Code",
      attempt: 1,
    });
    expect(ran.page.status).toBe("done");

    Object.assign(local.config, {
      agentHosts: [
        {
          name: "Ada's Mac",
          runtimes: ["claude_code"],
          account: { state: "signed_out" },
        },
      ],
    });
    const listed = (await at("GET", `${A}/runtime`)).body.runtime;
    expect(listed.hosts[0].accounts).toEqual([
      { runtime: "claude_code", state: "signed_out" },
    ]);
    const out = await turn("And now?", ran.sent.body.thread.id);
    expect(out.page.status).toBe("failed");
    expect(out.events.at(-1)).toEqual({
      type: "finished",
      status: "failed",
      reason: "failed",
      error:
        "Claude Code isn't signed in on the computer running the helper. Run claude, then /login there, then send your message again.",
    });
  });

  it("prepares where new threads run, and a helper's revoke, as cards its owner runs", async () => {
    const { at, local, turn } = await localInstall();
    const chose = await turn("Run new threads on my Claude subscription");
    expect(
      chose.events.some(
        (e) =>
          e.type === "tool_call" &&
          e.name === "helix_foundry_get_assistant_runtime",
      ),
    ).toBe(true);
    expect(chose.action).toMatchObject({
      operation: "set_assistant_runtime",
      target: "workspace:ws_acme",
      status: "prepared",
    });
    expect(JSON.parse(chose.action.displayJson)).toEqual({
      runtime: "claude_code",
      current: "api",
      online: false,
    });
    expect(JSON.parse(chose.action.payloadJson)).toEqual({
      runtime: "claude_code",
    });
    expect(chose.said).toMatch(/No helper is running yet/);
    const ran = await at("POST", `${A}/actions/${chose.action.id}:execute`, {
      payloadHash: chose.action.payloadHash,
    });
    expect(ran.body.action.status).toBe("succeeded");
    expect(JSON.parse(ran.body.action.resultJson).result).toEqual({
      runtime: "claude_code",
      online: [],
    });
    expect((await at("GET", `${A}/runtime`)).body.runtime.runtime).toBe(
      "claude_code",
    );
    later(60_000);
    const went = (
      await at(
        "GET",
        `${A}/turns/${ran.body.continuation.id}/events?afterSeq=0`,
      )
    ).body as ListAgentTurnEventsResponse;
    expect(
      went.events!.map((e) => parseAgentEvent(e.eventJson)),
    ).toContainEqual(
      expect.objectContaining({
        type: "text",
        text: "New threads run on your Claude subscription now, once its helper runs on your computer. This thread keeps running where it started.",
      }),
    );
    // New threads now run on the helper: once one runs, the agent finds
    // there is nothing to switch.
    Object.assign(local.config, {
      agentHosts: [{ name: "Ada's Mac", runtimes: ["claude_code"] }],
    });
    expect(
      (await turn("Switch the Assistant to Claude Code")).action,
    ).toBeUndefined();
    const revoke = await turn("Revoke the helper");
    expect(JSON.parse(revoke.action.displayJson)).toEqual({
      host: { id: "01JHOST0000000000000000000", name: "Ada's Mac" },
      runtimes: ["claude_code"],
      online: true,
    });
    const revoked = await at(
      "POST",
      `${A}/actions/${revoke.action.id}:execute`,
      { payloadHash: revoke.action.payloadHash },
    );
    expect(revoked.body.action.status).toBe("succeeded");
    expect(revoked.body.continuation).toBeUndefined();
    expect((await at("GET", `${A}/runtime`)).body.runtime.hosts).toEqual([]);
    // No helper runs new threads now: back on the workspace's model, the
    // agent finds none to revoke.
    expect(
      reason(await at("POST", `${A}/turns`, { text: "Hi", clientToken: "x" })),
    ).toBe("FOUNDRY_AGENT_HOST_OFFLINE");
    await at("PATCH", `${A}/runtime`, { runtime: "api" });
    expect((await turn("Revoke the helper")).said).toMatch(
      /^No helper is paired with this install/,
    );
  });

  it("prepares neither hosted, where the worker runs every thread", async () => {
    const c = await card("Run new threads on my Claude subscription");
    expect(c.action).toBeUndefined();
    expect(c.said).toMatch(/chosen on a local install of Foundry only/);
    expect(
      c.events.some(
        (e) =>
          e.type === "tool_call" &&
          e.name === "helix_foundry_get_assistant_runtime",
      ),
    ).toBe(false);
  });
});

describe("actions", () => {
  it("prepares a load of the ontology named, on its newest finished draft, without a token", async () => {
    const { events: evs, action } = await prepareLoad();
    const card = evs.find((e) => e.type === "action_card");
    expect(card).toMatchObject({
      operation: "create_load",
      target: "ontology:ws_acme/ont_acme_contracts",
      payload: { version_id: "ver_seed_3" },
      display: {
        ontology: { id: "ont_acme_contracts", name: "Customer contracts" },
        version: { id: "ver_seed_3", number: 3 },
      },
      target_named_by_user: true,
    });
    const prepared = evs.find(
      (e) =>
        e.type === "tool_result" &&
        e.name === "helix_foundry_prepare_operation",
    );
    expect(prepared).toMatchObject({
      is_error: false,
      output: { status: "awaiting_person" },
    });
    expect(JSON.stringify(evs)).not.toMatch(/token/i);
    expect(action).toMatchObject({
      status: "prepared",
      payloadJson: '{"version_id":"ver_seed_3"}',
      targetNamedByUser: true,
    });
    expect(action.payloadHash).toMatch(/^[0-9a-f]{64}$/);
  });

  it("warns when the person never named what it loads", async () => {
    const { action } = await prepareLoad("Load something for me");
    expect(action.targetNamedByUser).toBe(false);
  });

  it("names the page's ontology as the person's", async () => {
    const sent = await send("Load it", {
      pageContext: { ontologyId: "ont_acme_contracts" },
    });
    await events(sent.body.turn.id);
    const thread = await call("GET", `${A}/threads/${sent.body.thread.id}`);
    expect(thread.body.actions[0].targetNamedByUser).toBe(true);
  });

  it("loads the page's ontology even when the message says ontologies", async () => {
    const sent = await send("Reload one of my ontologies", {
      pageContext: { ontologyId: "ont_acme_renewals" },
    });
    const { events: evs } = await events(sent.body.turn.id);
    expect(evs.find((e) => e.type === "action_card")).toMatchObject({
      target: "ontology:ws_acme/ont_acme_renewals",
    });
  });

  it("runs a card once, as its person, with the hash shown: the load queues and the agent goes on", async () => {
    const { action } = await prepareLoad();
    const run = (hash = action.payloadHash, userId = "usr_ada") =>
      call(
        "POST",
        `${A}/actions/${action.id}:execute`,
        { payloadHash: hash },
        userId,
      );
    expect((await run(action.payloadHash, "usr_alan")).status).toBe(404);
    for (const malformed of ["", "abc", "g".repeat(64)]) {
      const refused = await run(malformed);
      expect([refused.status, refused.body.message]).toEqual([
        400,
        "payload_hash is the action's 64 hex digits",
      ]);
    }
    const changed = await run("0".repeat(64));
    expect([changed.status, reason(changed)]).toEqual([
      400,
      "FOUNDRY_AGENT_ACTION_CHANGED",
    ]);
    const ran = await run();
    expect(ran.status).toBe(200);
    expect(ran.body.action.status).toBe("succeeded");
    expect(JSON.parse(ran.body.action.resultJson).result).toMatchObject({
      version_id: "ver_seed_3",
      status: "writing",
    });
    expect(ran.body.continuation).toMatchObject({
      kind: "action_result",
      actionId: action.id,
    });
    const twice = await run();
    expect([twice.status, reason(twice)]).toEqual([
      400,
      "FOUNDRY_AGENT_ACTION_NOT_PREPARED",
    ]);
    const loads = await call(
      "GET",
      "/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts/loads",
    );
    expect(
      loads.body.loads.filter(
        (l: { versionId: string }) => l.versionId === "ver_seed_3",
      ),
    ).toHaveLength(1);
    const { events: evs } = await events(ran.body.continuation.id);
    expect(evs[0]).toMatchObject({
      type: "action_result",
      status: "succeeded",
    });
    expect(evs.find((e) => e.type === "text")).toMatchObject({
      text: expect.stringContaining("/ontology/ont_acme_contracts/loads/load_"),
    });
  });

  it("refuses to run while a turn of its thread is live", async () => {
    const { action, sent } = await prepareLoad();
    await send("Another question", { threadId: sent.body.thread.id });
    const busy = await call("POST", `${A}/actions/${action.id}:execute`, {
      payloadHash: action.payloadHash,
    });
    expect([busy.status, reason(busy)]).toEqual([
      400,
      "FOUNDRY_AGENT_THREAD_BUSY",
    ]);
  });

  it("refuses an expired card until it is refreshed", async () => {
    Object.assign(config, { agentActionTtlMs: 1000 });
    const { action, sent } = await prepareLoad();
    later(2000);
    const read = await call("GET", `${A}/threads/${sent.body.thread.id}`);
    expect(read.body.actions[0].status).toBe("expired");
    const expired = await call("POST", `${A}/actions/${action.id}:execute`, {
      payloadHash: action.payloadHash,
    });
    expect([expired.status, reason(expired)]).toEqual([
      400,
      "FOUNDRY_AGENT_ACTION_EXPIRED",
    ]);
    const refreshed = await call(
      "POST",
      `${A}/actions/${action.id}:refresh`,
      {},
    );
    expect(refreshed.body.action.status).toBe("prepared");
    const ran = await call("POST", `${A}/actions/${action.id}:execute`, {
      payloadHash: action.payloadHash,
    });
    expect(ran.body.action.status).toBe("succeeded");
    const after = await call("POST", `${A}/actions/${action.id}:refresh`, {});
    expect(reason(after)).toBe("FOUNDRY_AGENT_ACTION_NOT_PREPARED");
  });

  it("dismisses a card, once, and runs nothing", async () => {
    const { action } = await prepareLoad();
    const no = await call("POST", `${A}/actions/${action.id}:dismiss`, {});
    expect(no.body.action.status).toBe("dismissed");
    expect(no.body.continuation).toBeUndefined();
    expect(
      (await call("POST", `${A}/actions/${action.id}:dismiss`, {})).status,
    ).toBe(200);
    const run = await call("POST", `${A}/actions/${action.id}:execute`, {
      payloadHash: action.payloadHash,
    });
    expect(reason(run)).toBe("FOUNDRY_AGENT_ACTION_NOT_PREPARED");
    expect(
      (await call("POST", `${A}/actions/${action.id}:dismiss`, {}, "usr_alan"))
        .status,
    ).toBe(404);
  });

  it("fails a card whose person may no longer load, and reports it", async () => {
    const { action } = await prepareLoad();
    db.memberships.find((m) => m.id === "mem_ada_acme")!.role =
      "WORKSPACE_ROLE_MEMBER";
    const ran = await call("POST", `${A}/actions/${action.id}:execute`, {
      payloadHash: action.payloadHash,
    });
    expect(ran.body.action).toMatchObject({
      status: "failed",
      error: "permission denied",
    });
    const { events: evs } = await events(ran.body.continuation.id);
    expect(evs.find((e) => e.type === "text")).toMatchObject({
      text: expect.stringContaining("didn't run: permission denied"),
    });
  });

  it("refreshes as the person now: what they may no longer do comes first", async () => {
    const { action } = await prepareLoad();
    db.memberships.find((m) => m.id === "mem_ada_acme")!.role =
      "WORKSPACE_ROLE_MEMBER";
    const refused = await call("POST", `${A}/actions/${action.id}:refresh`, {});
    // gRPC's FAILED_PRECONDITION, 9.
    expect([refused.status, refused.body.code, refused.body.message]).toEqual([
      400,
      9,
      "permission denied",
    ]);
  });

  it("prepares nothing for a member", async () => {
    const { events: evs, action } = await prepareLoad(
      "Load Customer contracts",
      "usr_alan",
    );
    expect(action).toBeUndefined();
    expect(evs.some((e) => e.type === "action_card")).toBe(false);
    // A member's turn has no prepare tool: the agent says who can.
    expect(
      evs.some((e) => e.type === "tool_call" && e.name.includes("prepare")),
    ).toBe(false);
    expect(evs.find((e) => e.type === "text")).toMatchObject({
      text: "Only an owner or admin of this workspace can load Customer contracts, so I can't prepare that for you.",
    });
  });

  it("holds at most 3 waiting cards a thread", async () => {
    const { sent } = await prepareLoad();
    for (let i = 0; i < 3; i++) {
      const more = await send("Load Customer contracts", {
        threadId: sent.body.thread.id,
      });
      await events(more.body.turn.id);
    }
    const thread = await call("GET", `${A}/threads/${sent.body.thread.id}`);
    expect(thread.body.actions).toHaveLength(3);
  });
});

// ---------------------------------------------------------------- every operation

const WS = "/v1/workspaces/ws_acme/foundry",
  CONTRACTS = `${WS}/ontologies/ont_acme_contracts`;

/** The bucket's side of a presigned PUT, as the browser sends it. */
async function put(
  url: string,
  bytes: Buffer,
  headers: Record<string, string>,
) {
  const req = Object.assign(Readable.from([bytes]), {
    method: "PUT",
    headers: Object.fromEntries(
      Object.entries(headers).map(([k, v]) => [k.toLowerCase(), v]),
    ),
  }) as unknown as IncomingMessage;
  let status = 0;
  const res = {
    setHeader: () => res,
    writeHead: (s: number) => ((status = s), res),
    end: () => undefined,
  } as unknown as ServerResponse;
  expect(await handleDocumentUpload(req, res, new URL(url))).toBe(true);
  return status;
}
/** A Markdown document uploaded into General. */
async function uploaded(name: string, text: string) {
  const bytes = Buffer.from(text);
  const created = await call("POST", `${WS}/documents`, {
    name,
    mediaType: "text/markdown",
    sizeBytes: String(bytes.length),
    sha256: createHash("sha256").update(bytes).digest("hex"),
    clientToken: `tok-${name}`,
    setId: "set_acme_general",
  });
  const { document, upload } = created.body;
  expect(await put(upload.url, bytes, upload.headers)).toBe(200);
  expect(
    (await call("POST", `${WS}/documents/${document.id}:complete`, {})).status,
  ).toBe(200);
  return document.id as string;
}
/** A ready load of Customer contracts' v3, now current. */
async function loaded(token: string) {
  const load = (
    await call("POST", `${CONTRACTS}/loads`, {
      versionId: "ver_seed_3",
      clientToken: token,
    })
  ).body.load;
  later(10_000);
  expect(
    (await call("GET", `${CONTRACTS}/loads/${load.id}`)).body.load.status,
  ).toBe("ready");
  return load.id as string;
}

/** The card a message prepares in a new thread (or `threadId`), and its turn's events. */
async function card(text: string, extra: object = {}, userId = "usr_ada") {
  const sent = await send(text, extra, userId);
  expect(sent.status).toBe(200);
  const { events: evs } = await events(sent.body.turn.id, userId);
  const thread = await call(
    "GET",
    `${A}/threads/${sent.body.thread.id}`,
    undefined,
    userId,
  );
  const said = evs
    .filter((e) => e.type === "text")
    .map((e) => (e as { text: string }).text)
    .join(" ");
  return {
    sent,
    events: evs,
    said,
    action: (thread.body.actions || []).at(-1),
    display: JSON.parse(
      (thread.body.actions || []).at(-1)?.displayJson || "null",
    ),
  };
}
/** Runs an action, and folds the turn the agent goes on in. */
async function runCard(action: { id: string; payloadHash: string }) {
  const ran = await call("POST", `${A}/actions/${action.id}:execute`, {
    payloadHash: action.payloadHash,
  });
  expect(ran.status).toBe(200);
  const { events: evs } = await events(ran.body.continuation.id);
  const thread = await call(
    "GET",
    `${A}/threads/${ran.body.continuation.threadId}`,
  );
  return {
    ran: ran.body.action,
    result: JSON.parse(ran.body.action.resultJson || "{}").result,
    events: evs,
    said: evs
      .filter((e) => e.type === "text")
      .map((e) => (e as { text: string }).text)
      .join(" "),
    next: (thread.body.actions || []).at(-1),
  };
}
const display = (a: { displayJson: string }) => JSON.parse(a.displayJson);

describe("operations", () => {
  it("builds an ontology card by card: create, draft, make current, load", async () => {
    const created = await card(
      "Create an ontology called Orders graph on database orders, goal: Which customers order the most",
    );
    expect(created.action).toMatchObject({
      operation: "create_ontology",
      target: "tenant:ten_acme_orders",
      payloadJson: '{"name":"Orders graph"}',
      targetNamedByUser: true,
    });
    expect(created.display).toEqual({
      name: "Orders graph",
      database: { id: "ten_acme_orders", name: "orders" },
    });
    const step1 = await runCard(created.action);
    expect(step1.ran.status).toBe("succeeded");
    const ontologyId = step1.result.id;
    expect(ontologyId).toMatch(/^ont_/);
    // It goes on: the draft, on every connection, toward the goal asked.
    expect(step1.next).toMatchObject({
      operation: "generate_ontology",
      target: `ontology:ws_acme/${ontologyId}`,
      status: "prepared",
      targetNamedByUser: true,
    });
    expect(display(step1.next)).toMatchObject({
      ontology: { id: ontologyId, name: "Orders graph" },
      goal: "Which customers order the most",
      connectors_read: "every",
    });
    const step2 = await runCard(step1.next);
    expect(step2.ran.status).toBe("succeeded");
    // It read the draft until it ended, then offers it as current.
    expect(
      step2.events.find(
        (e) =>
          e.type === "tool_result" && e.name === "helix_foundry_get_generation",
      ),
    ).toMatchObject({
      output: {
        generation: { status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" },
      },
    });
    expect(step2.next.operation).toBe("set_current_version");
    expect(display(step2.next)).toMatchObject({
      ontology: { id: ontologyId },
      version: { number: 1 },
    });
    expect(display(step2.next).current).toBeUndefined();
    const step3 = await runCard(step2.next);
    expect(step3.next.operation).toBe("create_load");
    const step4 = await runCard(step3.next);
    expect(step4.ran.status).toBe("succeeded");
    expect(step4.said).toContain(`/ontology/${ontologyId}/loads/`);
  });

  it("relays a draft's questions, and sends the person's answers to it as they send them", async () => {
    Object.assign(config, { ontologyQuestions: true, ontologyStepMs: 300 });
    const created = await card(
      "Create an ontology called Orders graph on database orders, goal: Which customers order the most",
    );
    const step1 = await runCard(created.action);
    const ontologyId = step1.result.id;
    const step2 = await runCard(step1.next);
    // The draft waits on its agent's questions: asked through the tool,
    // which ends the turn; nothing prepared.
    const read = step2.events.find(
      (e) =>
        e.type === "tool_result" && e.name === "helix_foundry_get_generation",
    ) as { output: { generation: { id: string; status: string } } };
    expect(read.output.generation.status).toBe(
      "ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT",
    );
    const generationId = read.output.generation.id;
    const types = step2.events.map((e) => e.type);
    expect(types.slice(-4)).toEqual([
      "tool_call",
      "questions",
      "tool_result",
      "finished",
    ]);
    expect(step2.events.at(-1)).toEqual({
      type: "finished",
      status: "done",
      reason: "asked",
    });
    const asked = step2.events.find((e) => e.type === "questions") as Extract<
      AgentEvent,
      { type: "questions" }
    >;
    expect(asked.generation).toEqual({
      ontology_id: ontologyId,
      ontology_name: "Orders graph",
      generation_id: generationId,
      generation_title: expect.any(String),
      round: 1,
    });
    expect(
      asked.questions.map((q) => [q.id, q.multiple, q.recommended]),
    ).toEqual([
      ["Q1", false, "The company that pays"],
      ["Q2", true, "Unpaid invoices"],
      ["Q3", false, "90 days"],
      ["Q4", false, ""],
    ]);
    expect(step2.next.operation).toBe("generate_ontology");
    const threadId = step2.next.threadId as string;
    const thread = await call("GET", `${A}/threads/${threadId}`);
    const asking = thread.body.turns.at(-1);
    expect(asking.asked).toEqual({
      questions: asked.questions,
      generation: { ontologyId, generationId, round: 1 },
    });

    // Answers are checked against the questions, as the backend checks them.
    const answer = (answers: unknown, extra: object = {}) =>
      call("POST", `${A}/turns`, {
        threadId,
        answers: { turnId: asking.id, answers },
        clientToken: `a${++token}`,
        ...extra,
      });
    const given = [
      { questionId: "Q1", options: ["Each person who signs in"] },
      {
        questionId: "Q2",
        options: ["Falling usage", "Unpaid invoices"],
        text: " and churned seats ",
      },
      { questionId: "Q3", text: "60 days" },
    ];
    for (const [bad, why] of [
      [[{ questionId: "Q1", options: ["Nope"] }], "offers no option"],
      [
        [{ questionId: "Q1", options: ["The company that pays"], text: "x" }],
        "not both",
      ],
      [
        [
          {
            questionId: "Q1",
            options: ["The company that pays", "Each person who signs in"],
          },
        ],
        "takes one option, not 2",
      ],
      [[{ questionId: "Q3", options: ["60"] }], "takes an answer in words"],
      [[{ questionId: "Q3", text: "  " }], "1 to 2000 characters"],
      [[...given, given[0]], "answered twice"],
      [[{ questionId: "Q9", text: "x" }], 'no question "Q9"'],
      [given.slice(0, 2), "question Q3 needs an answer"],
    ] as const) {
      const r = await answer(bad);
      expect(r.status).toBe(400);
      expect(r.body.message).toContain(why);
    }
    const noThread = await call("POST", `${A}/turns`, {
      answers: { turnId: asking.id, answers: given },
      clientToken: "x1",
    });
    expect(noThread.status).toBe(400);
    expect((await answer(given, { threadId: "thr_nope" })).status).toBe(404);
    const askedNothing = await call("POST", `${A}/turns`, {
      threadId,
      answers: { turnId: created.sent.body.turn.id, answers: given },
      clientToken: "x2",
    });
    expect(reason(askedNothing)).toBe("FOUNDRY_AGENT_QUESTIONS_CLOSED");

    const sent = await call("POST", `${A}/turns`, {
      threadId,
      answers: { turnId: asking.id, answers: given },
      clientToken: "answers-1",
    });
    expect(sent.status).toBe(200);
    expect(sent.body.turn).toMatchObject({
      kind: "answers",
      text: "",
      answers: {
        turnId: asking.id,
        answers: [
          { questionId: "Q1", options: ["Each person who signs in"] },
          {
            questionId: "Q2",
            options: ["Unpaid invoices", "Falling usage"],
            text: "and churned seats",
          },
          { questionId: "Q3", options: [], text: "60 days" },
        ],
      },
    });
    // The same click again is the same turn; answered, they are closed.
    const again = await call("POST", `${A}/turns`, {
      threadId,
      answers: { turnId: asking.id, answers: given },
      clientToken: "answers-1",
    });
    expect(again.body.turn.id).toBe(sent.body.turn.id);
    const { events: evs } = await events(sent.body.turn.id);
    expect(evs[0]).toEqual({
      type: "answers",
      turn_id: asking.id,
      answers: [
        { question_id: "Q1", options: ["Each person who signs in"] },
        {
          question_id: "Q2",
          options: ["Unpaid invoices", "Falling usage"],
          text: "and churned seats",
        },
        { question_id: "Q3", options: [], text: "60 days" },
      ],
    });
    expect(reason(await answer(given))).toBe("FOUNDRY_AGENT_QUESTIONS_CLOSED");

    // Sent to the draft as the person, at once: Q4, left out, is left to
    // its agent; no card, and the answers turn only says so.
    const got = await call(
      "GET",
      `${WS}/ontologies/${ontologyId}/generations/${generationId}`,
    );
    expect(got.body.generation.questionRound).toBe(1);
    expect(
      got.body.generation.questions.map(
        (q: { answer?: unknown; leftToAgent?: boolean }) =>
          q.answer ?? (q.leftToAgent && "left"),
      ),
    ).toEqual([
      { questionId: "Q1", options: ["Each person who signs in"] },
      {
        questionId: "Q2",
        options: ["Unpaid invoices", "Falling usage"],
        text: "and churned seats",
      },
      { questionId: "Q3", options: [], text: "60 days" },
      "left",
    ]);
    const said = evs.filter((e) => e.type === "text").map((e) => e.text);
    expect(said.join(" ")).toContain("Your answers went to the draft");
    const after = await call("GET", `${A}/threads/${threadId}`);
    expect(
      after.body.actions.filter(
        (a: { operation: string }) =>
          a.operation === "answer_generation_questions",
      ),
    ).toEqual([]);
  });

  it("closes a relayed round the draft no longer waits on, sending nothing", async () => {
    Object.assign(config, { ontologyQuestions: true, ontologyStepMs: 300 });
    const created = await card(
      "Create an ontology called Orders graph on database orders, goal: Which customers order the most",
    );
    const step1 = await runCard(created.action);
    const ontologyId = step1.result.id;
    const step2 = await runCard(step1.next);
    const threadId = step2.next.threadId as string;
    const asking = (
      await call("GET", `${A}/threads/${threadId}`)
    ).body.turns.at(-1);
    const generationId = asking.asked.generation.generationId;
    // Answered on its page meanwhile.
    const onPage = await call(
      "POST",
      `${WS}/ontologies/${ontologyId}/generations/${generationId}:answer`,
      { answers: [], useDefaults: true, round: 1 },
    );
    expect(onPage.status).toBe(200);
    const late = await call("POST", `${A}/turns`, {
      threadId,
      answers: {
        turnId: asking.id,
        answers: [
          { questionId: "Q1", options: ["The company that pays"] },
          { questionId: "Q2", options: ["Unpaid invoices"] },
          { questionId: "Q3", text: "60 days" },
        ],
      },
      clientToken: "late-1",
    });
    expect(reason(late)).toBe("FOUNDRY_AGENT_QUESTIONS_CLOSED");
    expect(late.body.message).toContain("answered on its page");
    const thread = await call("GET", `${A}/threads/${threadId}`);
    expect(thread.body.turns.at(-1).id).toBe(asking.id);
  });

  it("asks its own questions to set up an ontology, and reads the answers back", async () => {
    const c = await card("Help me set up an ontology");
    expect(c.action).toBeUndefined();
    expect(c.events.at(-1)).toEqual({
      type: "finished",
      status: "done",
      reason: "asked",
    });
    const asked = c.events.find((e) => e.type === "questions") as Extract<
      AgentEvent,
      { type: "questions" }
    >;
    expect(asked.generation).toBeUndefined();
    expect(
      asked.questions.map((q) => [q.id, q.header, q.multiple, q.blocking]),
    ).toEqual([
      ["q1", "Goal", false, true],
      ["q2", "Data", true, true],
      ["q3", "Leave out", false, false],
    ]);
    // Its data options are the workspace's connections.
    const labels = asked.questions[1]!.options!.map((o) => o.label);
    expect(labels.length).toBeGreaterThanOrEqual(2);
    const threadId = c.sent.body.thread.id,
      turnId = c.sent.body.turn.id;
    const r = await call("POST", `${A}/turns`, {
      threadId,
      answers: {
        turnId,
        answers: [
          { questionId: "q1", options: ["Revenue and renewals"] },
          { questionId: "q2", options: labels.slice(0, 2) },
        ],
      },
      clientToken: "setup-1",
    });
    expect(r.status).toBe(200);
    const { events: evs } = await events(r.body.turn.id);
    const said = evs
      .filter((e) => e.type === "text")
      .map((e) => (e as { text: string }).text)
      .join(" ");
    expect(said).toContain(
      `an ontology for Revenue and renewals, reading ${labels[0]}; ${labels[1]}.`,
    );
  });

  it("closes the questions once the conversation goes on", async () => {
    const c = await card("Help me set up an ontology");
    const threadId = c.sent.body.thread.id;
    expect((await send("Never mind", { threadId })).status).toBe(200);
    await events(c.sent.body.turn.id);
    const r = await call("POST", `${A}/turns`, {
      threadId,
      answers: {
        turnId: c.sent.body.turn.id,
        answers: [
          { questionId: "q1", options: ["Customer health"] },
          { questionId: "q2", text: "Everything" },
        ],
      },
      clientToken: "late",
    });
    expect(reason(r)).toBe("FOUNDRY_AGENT_QUESTIONS_CLOSED");
  });

  it("answers a draft's questions only for an owner or admin", async () => {
    Object.assign(config, { ontologyQuestions: true });
    const c = await card(
      "Answer the Customer contracts questions: use the defaults",
      {},
      "usr_alan",
    );
    expect(c.action).toBeUndefined();
    expect(c.said).toContain(
      "Only an owner or admin of this workspace can answer a draft's questions",
    );
  });

  it("says when no draft waits for answers", async () => {
    const c = await card(
      "Answer the Customer contracts questions: use the defaults",
    );
    expect(c.action).toBeUndefined();
    expect(c.said).toContain(
      "No draft of Customer contracts is waiting for answers.",
    );
  });

  it("makes an ontology on a new database in the workspace's project", async () => {
    const c = await card(
      "Create an ontology called Lab on a new database Lab db",
    );
    expect(c.action.target).toBe("workspace:ws_acme");
    expect(c.display).toMatchObject({
      name: "Lab",
      new_database: { name: "Lab db", slug: "lab-db" },
    });
    expect(c.display.new_database.project_id).toMatch(/^prj_acme_/);
    const ran = await runCard(c.action);
    expect(ran.ran.status).toBe("succeeded");
    const tenants = await call(
      "GET",
      `/v1/tenants?projectId=${c.display.new_database.project_id}`,
    );
    expect(
      tenants.body.tenants.some((t: { slug: string }) => t.slug === "lab-db"),
    ).toBe(true);
  });

  it("says what is wrong with a database instead of preparing", async () => {
    expect(
      (await card("Create an ontology called X on database nowhere")).said,
    ).toMatch(/There is no database called nowhere/);
    expect(
      (await card("Create an ontology called X on database contracts")).said,
    ).toMatch(/already holds the ontology Customer contracts/);
    const vague = await card("Create an ontology please");
    expect(vague.action).toBeUndefined();
    expect(vague.said).toMatch(/Tell me what to call the ontology/);
  });

  it("drafts from the connections and sets a message names, or documents alone", async () => {
    const chosen = await card("Generate a draft of Sales graph from Stripe");
    expect(chosen.display).toMatchObject({
      connectors_read: "chosen",
      connectors: [{ id: "conn_acme_stripe", kind: "stripe" }],
    });
    expect(JSON.parse(chosen.action.payloadJson)).toEqual({
      connector_ids: ["conn_acme_stripe"],
    });
    const docs = await card("Draft Sales graph from General, documents only");
    expect(JSON.parse(docs.action.payloadJson)).toEqual({
      document_set_ids: ["set_acme_general"],
      no_connectors: true,
    });
    expect(docs.display).toMatchObject({
      connectors_read: "none",
      connectors: [],
      document_sets: [{ id: "set_acme_general", name: "General" }],
    });
  });

  it("makes a version current, against the current one", async () => {
    const c = await card("Make v3 of Customer contracts current");
    expect(JSON.parse(c.action.payloadJson)).toEqual({
      version_id: "ver_seed_3",
      expected_current_version_id: "ver_seed_1",
    });
    expect(c.display).toMatchObject({
      version: { id: "ver_seed_3", number: 3 },
      current: { id: "ver_seed_1", number: 1 },
    });
    const ran = await runCard(c.action);
    expect(ran.result).toMatchObject({ id: "ver_seed_3" });
    // A finished draft made current is offered to load next.
    expect(ran.next.operation).toBe("create_load");
    expect((await card("Make v1 of Customer contracts current")).said).toMatch(
      /I've prepared/,
    );
    expect((await card("Make v9 of Customer contracts current")).said).toBe(
      "Customer contracts has no v9.",
    );
  });

  it("makes an earlier load current", async () => {
    const first = await loaded("one");
    await loaded("two");
    const c = await card(
      "Make the previous load of Customer contracts current",
    );
    expect(c.action.operation).toBe("set_current_load");
    expect(JSON.parse(c.action.payloadJson)).toEqual({ load_id: first });
    expect(c.display).toMatchObject({
      load: { id: first, version: { number: 3 }, status: "ready" },
      current: { version: { number: 3 } },
    });
    // Refreshed after another load became current, it names that one.
    const third = await loaded("three");
    const refreshed = await call(
      "POST",
      `${A}/actions/${c.action.id}:refresh`,
      {},
    );
    expect(display(refreshed.body.action).current.id).toBe(third);
    const ran = await runCard(refreshed.body.action);
    expect(ran.ran.status).toBe("succeeded");
    expect(ran.said).toContain(`/ontology/ont_acme_contracts/loads/${first}`);
  });

  it("previews a revision of a draft's item", async () => {
    const c = await card("Preview a revision of v3 of Customer contracts");
    expect(c.action.operation).toBe("revise_version");
    const payload = JSON.parse(c.action.payloadJson);
    expect(payload).toMatchObject({ version_id: "ver_seed_3", dry_run: true });
    expect(c.display).toMatchObject({
      dry_run: true,
      edits: [{ kind: "remove", item_id: payload.edits[0].remove.item_id }],
    });
    const ran = await runCard(c.action);
    expect(ran.ran.status).toBe("succeeded");
    expect(ran.result.id).toMatch(/^gen/);
  });

  it("syncs a connection by its name or kind", async () => {
    const c = await card("Sync Stripe");
    expect(c.action).toMatchObject({
      operation: "sync_connector",
      target: "connector:ws_acme/conn_acme_stripe",
      payloadJson: "{}",
      targetNamedByUser: true,
    });
    expect(c.display).toEqual({
      connector: { id: "conn_acme_stripe", name: "Stripe", kind: "stripe" },
    });
    const ran = await runCard(c.action);
    expect(ran.said).toContain("/data/connections/conn_acme_stripe");
    expect((await card("Sync it")).said).toMatch(/^Which connection\?/);
  });

  it("adds the sample's connection and set", async () => {
    const c = await card("Add the sample data to Sales graph");
    expect(c.display).toEqual({
      ontology: { id: "ont_acme_sales", name: "Sales graph" },
    });
    const ran = await runCard(c.action);
    expect(ran.ran.status).toBe("succeeded");
    expect(ran.result).toMatchObject({ documents: [] });
    const sets = await call("GET", `${WS}/document-sets`);
    expect(
      sets.body.sets.some(
        (s: { name: string }) => s.name === "Acme Corp sample",
      ),
    ).toBe(true);
    // Its made-up data is read only when named: a draft of every
    // connection names the others, and one naming it says so.
    const draft = await card("Generate a draft of Sales graph");
    const drafted = display(draft.action);
    expect(drafted.connectors_read).toBe("chosen");
    expect(drafted.connectors.length).toBeGreaterThan(0);
    expect(
      drafted.connectors.filter(
        (c: { name: string; sample?: true }) =>
          c.sample || c.name === "Acme Corp",
      ),
    ).toEqual([]);
    expect(JSON.parse(draft.action.payloadJson).connector_ids).toEqual(
      drafted.connectors.map((c: { id: string }) => c.id),
    );
    const named = await card("Generate a draft of Sales graph from Acme Corp");
    expect(display(named.action).connectors).toEqual([
      expect.objectContaining({ name: "Acme Corp", sample: true }),
    ]);
  });

  it("creates, renames and describes a set, and moves a document into it", async () => {
    const doc = await uploaded("notes.md", "# Notes\n\nNothing yet.\n");
    const made = await card(
      "Create a document set called Leases, described as “Signed leases”",
    );
    expect(made.display).toMatchObject({
      name: "Leases",
      description: "Signed leases",
    });
    expect(made.action.targetNamedByUser).toBe(false);
    const set = (await runCard(made.action)).result;
    expect(set).toMatchObject({ name: "Leases" });
    const renamed = await card("Rename the set Leases to Contracts");
    expect(renamed.display).toMatchObject({
      set: { id: set.id, name: "Leases" },
      name: "Contracts",
    });
    expect((await runCard(renamed.action)).ran.status).toBe("succeeded");
    const described = await card(
      'Describe the set Contracts as "Signed contracts"',
    );
    expect(described.display).toMatchObject({
      description: "Signed contracts",
    });
    expect(described.display.name).toBeUndefined();
    await runCard(described.action);
    const moved = await card("Move notes.md to Contracts");
    expect(moved.display).toMatchObject({
      document: { id: doc, name: "notes.md" },
      from: { id: "set_acme_general", name: "General" },
      to: { id: set.id, name: "Contracts" },
    });
    const ran = await runCard(moved.action);
    expect(ran.result.document).toMatchObject({ id: doc, set_id: set.id });
  });

  it("settles a review the current load holds, as the review takes it", async () => {
    await uploaded(
      "variant.md",
      "# Variant contract\n\nGlobexx Corporation signed. The Agreement runs for 24 months.\n",
    );
    const load = await loaded("first");
    const c = await card(
      "Settle the review about Globexx in Customer contracts",
    );
    const listed = c.events.find(
      (e) =>
        e.type === "tool_result" && e.name === "helix_foundry_list_reviews",
    ) as { output: { load_id: string; reviews: { review_key: string }[] } };
    expect(listed.output.load_id).toBe(load);
    expect(c.display).toMatchObject({
      load_id: load,
      review: { reason: "unresolved", settle: "resolve" },
      document: { name: "variant.md" },
      decision: { kind: "link", entity_key: "cus_GLOBEX01", candidate: true },
    });
    // It names the entities the review concerns, as ListFoundryReviews does.
    expect(c.display.review.entities).toContainEqual(
      expect.objectContaining({ type: "Customer", entity_key: "cus_GLOBEX01" }),
    );
    expect(c.said).toMatch(/“globexx corporation”/);
    const ran = await runCard(c.action);
    expect(ran.result).toMatchObject({
      kind: "resolve",
      entity_key: "cus_GLOBEX01",
    });
    expect(ran.said).toMatch(/applies from the next load/);

    expect(
      (await card("Settle the review about Hooli in Customer contracts")).said,
    ).toBe(
      "No review of the current load of Customer contracts is about Hooli.",
    );
    const no = await card(
      "Settle the review about Globexx in Customer contracts: not in the data",
    );
    expect(no.display.decision).toEqual({ kind: "not_in_data" });
    // Once a load no longer holds it, Refresh refuses it in the tool's words.
    Object.assign(config, { agentActionTtlMs: 1000 });
    const stale = await card(
      "Settle the review about Globexx in Customer contracts",
    );
    await loaded("second");
    expect(
      (await card("Settle the review about Globexx in Customer contracts"))
        .said,
    ).toBe("The current load of Customer contracts has no review to settle.");
    later(2_000);
    const refreshed = await call(
      "POST",
      `${A}/actions/${stale.action.id}:refresh`,
      {},
    );
    expect([refreshed.status, refreshed.body.message]).toEqual([
      400,
      expect.stringMatching(/^the current load holds no review/),
    ]);
  });

  it("says nothing waits for review without a current load", async () => {
    expect(
      (await card("Settle the review about Globexx in Sales graph")).said,
    ).toBe("Sales graph has no current load, so nothing waits for review.");
  });

  it("links a kind's form to connect, and nothing else", async () => {
    const c = await card("Connect Slack");
    const out = c.events.find(
      (e) => e.type === "tool_result" && e.name === "helix_foundry_connect",
    ) as { output: { url: string; kind: string; available: boolean } };
    expect(new URL(out.output.url).pathname).toBe("/data");
    expect(Object.fromEntries(new URL(out.output.url).searchParams)).toEqual({
      add: "slack",
      workspace: "ws_acme",
    });
    expect(out.output).toMatchObject({ kind: "slack", available: true });
    expect(c.action).toBeUndefined();
    expect((await card("Connect something")).said).toMatch(
      /^Which kind of data\?/,
    );
  });

  it("gives a member the read tools only: no connect, no prepare", async () => {
    for (const [text, verb] of [
      ["Connect Slack", "connect data"],
      ["Sync Stripe", "sync a connection"],
      ["Settle the review about Globexx", "settle a review"],
    ]) {
      const c = await card(text, {}, "usr_alan");
      expect(c.action).toBeUndefined();
      expect(
        c.events.some(
          (e) =>
            e.type === "tool_call" &&
            [
              "helix_foundry_connect",
              "helix_foundry_prepare_operation",
            ].includes(e.name),
        ),
      ).toBe(false);
      expect(c.said).toBe(
        `Only an owner or admin of this workspace can ${verb}, so I can't prepare that for you.`,
      );
    }
  });

  it("refreshes an expired card of any operation, and refuses a person who may no longer", async () => {
    Object.assign(config, { agentActionTtlMs: 1000 });
    const c = await card("Sync Stripe");
    later(2_000);
    const refreshed = await call(
      "POST",
      `${A}/actions/${c.action.id}:refresh`,
      {},
    );
    expect(refreshed.body.action.status).toBe("prepared");
    later(2_000);
    db.memberships.find((m) => m.id === "mem_ada_acme")!.role =
      "WORKSPACE_ROLE_MEMBER";
    const refused = await call(
      "POST",
      `${A}/actions/${c.action.id}:refresh`,
      {},
    );
    expect([refused.status, refused.body.message]).toEqual([
      400,
      "permission denied",
    ]);
  });

  it("shows a call a limit kept from running, as agentOutcome not_run says", async () => {
    Object.assign(config, { agentOutcome: "not_run" });
    const c = await card("What ontologies do I have?");
    expect(
      c.events.find(
        (e) => e.type === "tool_result" && e.tool_use_id === "tu_cut",
      ),
    ).toMatchObject({ is_error: true, not_run: "calls" });
    expect(c.events.at(-1)).toMatchObject({
      type: "finished",
      reason: "answered",
    });
  });
});

describe("dashboards", () => {
  const D = `${WS}/dashboards`;
  const BUILD =
    "Build a dashboard of invoices by status for Customer contracts";

  it("says why it can't build a dashboard of an ontology with nothing loaded", async () => {
    const c = await card(BUILD);
    expect(
      c.events.find(
        (e) =>
          e.type === "tool_result" &&
          e.name === "helix_foundry_preview_dashboard_tile",
      ),
    ).toMatchObject({ is_error: true });
    expect(c.events.some((e) => e.type === "action_card")).toBe(false);
    expect(c.said).toContain(
      "I couldn't read invoices by status from Customer contracts",
    );
  });

  it("tries a tile, then prepares the dashboard, which its person creates", async () => {
    await loaded("dash");
    const c = await card(BUILD);
    expect(
      c.events.find(
        (e) =>
          e.type === "tool_result" &&
          e.name === "helix_foundry_preview_dashboard_tile",
      ),
    ).toMatchObject({
      is_error: false,
      output: { outcome: { ok: { result: { kind: "group_count" } } } },
    });
    expect(c.action).toMatchObject({
      operation: "create_dashboard",
      target: "workspace:ws_acme",
      // The person named its name: "invoices by status".
      targetNamedByUser: true,
    });
    expect(c.display).toEqual({
      name: "Invoices by status",
      description: "Customer contracts: its invoices, counted by status.",
      spec: {
        tiles: [
          {
            title: "Invoices by status",
            ontology: { id: "ont_acme_contracts", name: "Customer contracts" },
            visual: "bar",
          },
        ],
      },
    });
    const r = await runCard(c.action);
    expect(r.ran.status).toBe("succeeded");
    expect(r.result).toMatchObject({ name: "Invoices by status", revision: 1 });
    expect(r.said).toContain(`/dashboards/${r.result.id}`);
    // Made once, as its person, its spec as the card showed it.
    expect((await call("GET", D)).body.dashboards).toHaveLength(1);
    const got = await call("GET", `${D}/${r.result.id}`);
    expect(JSON.parse(got.body.dashboard.specJson)).toEqual(
      JSON.parse(c.action.payloadJson).spec,
    );
    expect(got.body.dashboard.createdByUserId).toBe("usr_ada");
  });

  it("pins, renames, publishes, unpublishes and deletes the dashboard on the page", async () => {
    const made = (await call("POST", D, { name: "Revenue", clientToken: "c" }))
      .body.dashboard;
    const page = { pageContext: { dashboardId: made.id } };
    const pin = await card("Pin this dashboard", page);
    expect(pin.action).toMatchObject({
      operation: "pin_dashboard",
      target: `dashboard:ws_acme/${made.id}`,
      targetNamedByUser: true,
    });
    expect(JSON.parse(pin.action.payloadJson)).toEqual({
      expected_revision: 1,
      pinned: true,
    });
    expect(pin.display).toEqual({
      dashboard: { id: made.id, name: "Revenue" },
      pinned: true,
    });
    expect((await runCard(pin.action)).said).toContain(
      "“Revenue” is pinned for everyone",
    );
    const renamed = await card("Rename it to Paid invoices", page);
    expect(JSON.parse(renamed.action.payloadJson)).toEqual({
      expected_revision: 1,
      name: "Paid invoices",
    });
    expect(renamed.display).toEqual({
      dashboard: { id: made.id, name: "Revenue", pinned: true },
      name: "Paid invoices",
    });
    await runCard(renamed.action);
    expect((await call("GET", `${D}/${made.id}`)).body).toMatchObject({
      dashboard: { name: "Paid invoices", pinned: true },
    });
    // Publishing needs a tile: the run fails, and the agent says why.
    const published = await runCard((await card("Publish it", page)).action);
    expect(published.ran).toMatchObject({
      status: "failed",
      error: "the dashboard has no tiles to publish",
    });
    expect(published.said).toContain("That didn't run");
    // Unpublished, there is no link to end.
    const unpublished = await card("Unpublish it", page);
    expect(unpublished.action).toBeUndefined();
    expect(unpublished.said).toContain(
      "“Paid invoices” isn't published, so there is no link to end.",
    );
    const deleted = await runCard(
      (await card("Delete this dashboard", page)).action,
    );
    expect(deleted.result).toEqual({ deleted: true, dashboard_id: made.id });
    expect(deleted.said).toBe("“Paid invoices” is deleted.");
    expect((await call("GET", D)).body.dashboards).toEqual([]);
  });

  it("draws a dashboard's card again as it is now", async () => {
    const made = (await call("POST", D, { name: "Revenue", clientToken: "c" }))
      .body.dashboard;
    const c = await card("Delete this dashboard", {
      pageContext: { dashboardId: made.id },
    });
    await call("POST", `${D}/${made.id}:pin`, {
      expectedRevision: "1",
      pinned: true,
    });
    later(6 * 60_000);
    const refreshed = await call(
      "POST",
      `${A}/actions/${c.action.id}:refresh`,
      {},
    );
    expect(JSON.parse(refreshed.body.action.displayJson)).toEqual({
      dashboard: { id: made.id, name: "Revenue", pinned: true },
    });
  });

  it("prepares no publishing without public links", async () => {
    Object.assign(config, { dashboardsPublic: false });
    const made = await call("POST", D, { name: "Revenue", clientToken: "c" });
    const c = await card("Publish this", {
      pageContext: { dashboardId: made.body.dashboard.id },
    });
    expect(c.action).toBeUndefined();
    expect(c.said).toContain("Public links aren't available here");
  });

  it("publishes, and hands the agent no link; unpublishes with links off too", async () => {
    await loaded("dash");
    const tile = {
      id: "t1",
      title: "Invoices",
      ontology_id: "ont_acme_contracts",
      load: { kind: "current" },
      query: { start: { type: "Invoice" }, return: { count: {} } },
      visual: {},
    };
    const made = (
      await call("POST", D, {
        name: "Revenue",
        clientToken: "c",
        specJson: JSON.stringify({
          v: 1,
          tiles: [tile],
          layout: { rows: [["t1"]] },
        }),
      })
    ).body.dashboard;
    const page = { pageContext: { dashboardId: made.id } };
    const published = await runCard((await card("Publish it", page)).action);
    expect(published.ran.status).toBe("succeeded");
    const token = (await call("GET", `${D}/${made.id}`)).body.dashboard.share
      .token as string;
    expect(token).toMatch(/^hxd_/);
    expect(published.result.share).toMatchObject({ published: true });
    expect(published.ran.resultJson).not.toContain(token);
    expect(published.said).not.toContain(token);

    // Links turned off: no publishing, but the link can still be ended.
    Object.assign(config, { dashboardsPublic: false });
    const again = await card("Publish this", page);
    expect(again.action).toBeUndefined();
    expect(again.said).toContain("Public links aren't available here");
    const unpublished = await runCard(
      (await card("Unpublish it", page)).action,
    );
    expect(unpublished.ran.status).toBe("succeeded");
    expect(
      (await call("GET", `${D}/${made.id}`)).body.dashboard.share,
    ).toBeFalsy();
  });

  it("builds a member's own dashboard, and leaves pinning to owners and admins", async () => {
    await loaded("dash");
    const built = await card(BUILD, {}, "usr_alan");
    expect(built.action.operation).toBe("create_dashboard");
    const ran = await call(
      "POST",
      `${A}/actions/${built.action.id}:execute`,
      { payloadHash: built.action.payloadHash },
      "usr_alan",
    );
    expect(ran.body.action.status).toBe("succeeded");
    const id = JSON.parse(ran.body.action.resultJson).result.id;
    expect(
      (await call("GET", `${D}/${id}`)).body.dashboard.createdByUserId,
    ).toBe("usr_alan");
    const pin = await card(
      "Pin this dashboard",
      { pageContext: { dashboardId: id } },
      "usr_alan",
    );
    expect(pin.action).toBeUndefined();
    expect(pin.said).toContain(
      "Only an owner or admin of this workspace can pin a dashboard",
    );
  });

  it("refuses a page naming a dashboard by anything but an id", async () => {
    const r = await send("Pin it", { pageContext: { dashboardId: "a/b" } });
    expect(r.status).toBe(400);
  });
});

describe("an automation's time zone", () => {
  const ULID = /\b[0-9A-HJKMNP-TV-Z]{26}\b/;
  const spec = (c: Awaited<ReturnType<typeof card>>) =>
    JSON.parse(c.action.payloadJson).spec;

  it("schedules in the zone the browser sent, without asking", async () => {
    const c = await card(
      "Every Monday at 8am, give me a summary of overdue invoices from Customer contracts",
      { pageContext: { newAutomation: true, timeZone: "America/New_York" } },
    );
    expect(c.events.some((e) => e.type === "questions")).toBe(false);
    expect(spec(c).nodes[0].config).toEqual({
      kind: "schedule",
      every: "week",
      weekday: "monday",
      at: "08:00",
      time_zone: "America/New_York",
    });
    expect(c.said).toContain("Every Monday at 08:00 New York time");
    // The instruction names the ontology by its name, never its id.
    const instruction = spec(c).nodes.find(
      (n: { kind: string }) => n.kind === "assistant",
    ).config.instruction;
    expect(instruction).toContain("the Customer contracts ontology");
    expect(instruction).not.toContain("ont_acme_contracts");
    expect(instruction).not.toMatch(ULID);
    expect(c.sent.body.turn.pageContext).toEqual({
      newAutomation: true,
      timeZone: "America/New_York",
    });
  });

  it("drops a zone that isn't one, as the WFE does", async () => {
    for (const timeZone of ["Mars/Olympus", "Local", "New York"]) {
      const r = await send("Hi", { pageContext: { timeZone } });
      expect(r.status).toBe(200);
      expect(r.body.turn.pageContext ?? null).toBeNull();
    }
  });

  it("asks for one in words when none was sent, and schedules in the one chosen", async () => {
    const c = await card("Every Monday at 8am, summarise the overdue invoices", {
      pageContext: { newAutomation: true },
    });
    expect(c.action).toBeUndefined();
    const asked = c.events.find((e) => e.type === "questions") as Extract<
      AgentEvent,
      { type: "questions" }
    >;
    expect(asked.questions[0]!.text).toBe(
      "Which time zone should the Monday 08:00 run keep?",
    );
    expect(asked.questions[0]!.options!.map((o) => o.label)).toEqual([
      "London (UK time)",
      "New York (US Eastern)",
      "Los Angeles (US Pacific)",
      "Berlin (Central Europe)",
    ]);
    const r = await call("POST", `${A}/turns`, {
      threadId: c.sent.body.thread.id,
      answers: {
        turnId: c.sent.body.turn.id,
        answers: [{ questionId: "q1", options: ["Berlin (Central Europe)"] }],
      },
      clientToken: "zone-1",
    });
    expect(r.status).toBe(200);
    await events(r.body.turn.id);
    const thread = await call("GET", `${A}/threads/${c.sent.body.thread.id}`);
    const prepared = thread.body.actions.at(-1);
    expect(prepared.operation).toBe("create_automation");
    expect(JSON.parse(prepared.payloadJson).spec.nodes[0].config).toMatchObject(
      { every: "week", at: "08:00", time_zone: "Europe/Berlin" },
    );
  });

  it("never asks for an hour's or an upload's", async () => {
    const c = await card("Every hour, summarise the overdue invoices", {
      pageContext: { newAutomation: true },
    });
    expect(c.events.some((e) => e.type === "questions")).toBe(false);
    expect(spec(c).nodes[0].config).toMatchObject({
      every: "hour",
      time_zone: "UTC",
    });
  });
});
