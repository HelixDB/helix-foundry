import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { AgentEvent, ListAgentTurnEventsResponse } from "@/shared/wfe";
import {
  EMPTY_STREAM,
  parseAgentEvent,
  reduceTurnEvents,
} from "@/web/areas/agent/model";
import { NOT_CLAUDE } from "../../mock/routes/agent";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { config, db, resetState } from "../../mock/state";

// The mock WFE's Assistant (mock/routes/agent.ts) behaves as the backend's
// API does for the dock: private threads, turns that stream events in the
// backend's shapes (each one the dock's parser reads), a load prepared as a
// card and run only by its person, with the hash they were shown, once,
// while its confirmation lives and the thread is idle; a continuation turn
// after; dismiss and refresh; and its refusals, with their reasons.

const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
async function call(
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
      principal: { user: db.users.get(userId)!, sessionId: "s" },
    })) as MockReply & { body: any };
  }
  throw new Error(`no mock route for ${method} ${path}`);
}

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

  it("fails a turn at once on a provider other than Claude, in the worker's words", async () => {
    const saved = await call("PUT", "/v1/workspaces/ws_acme/foundry/ai", {
      provider: { openai: {} },
      apiKey: "sk-proj-sentinel-0123456789abcdefwxyz",
    });
    expect(saved.status).toBe(200);
    // Past the save's test, it is the setting in force.
    later(5_000);
    const sent = await send("What ontologies do I have?");
    expect(sent.status).toBe(200);
    const { page, events: evs } = await events(sent.body.turn.id);
    expect(page.status).toBe("failed");
    expect(evs).toEqual([
      {
        type: "finished",
        status: "failed",
        reason: "failed",
        error: NOT_CLAUDE,
      },
    ]);
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

describe("actions", () => {
  it("prepares a load of the ontology named, on its newest finished draft, without a token", async () => {
    const { events: evs, action } = await prepareLoad();
    const card = evs.find((e) => e.type === "action_card");
    expect(card).toMatchObject({
      operation: "create_load",
      target: "ontology:ws_acme/ont_acme_contracts",
      payload: { version_id: "ver_seed_3" },
      display: { ontology_name: "Customer contracts", version: 3 },
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
      text: expect.stringContaining("didn't start: permission denied"),
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
    expect(
      evs.find((e) => e.type === "tool_result" && e.is_error),
    ).toMatchObject({
      error: expect.stringContaining("permission denied"),
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
