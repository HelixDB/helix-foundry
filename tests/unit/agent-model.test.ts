import { describe, expect, it } from "vitest";
import type { FoundryAgentAction } from "@/shared/wfe";
import { ApiError } from "@/web/api";
import {
  EMPTY_STREAM,
  STOP_TEXT,
  agentRefusal,
  analystLink,
  cardState,
  isAgentOperation,
  pageContext,
  parseAgentEvent,
  payloadText,
  pretty,
  reduceTurnEvents,
  resultLink,
  inAppPath,
  onClaude,
  textParts,
  threadKey,
} from "@/web/areas/agent/model";

// The Assistant dock's pure logic (src/web/areas/agent/model.ts): what page
// is sent, which of the model's words become links, how polled events fold
// into a turn, and what an action's card offers.

const ORIGIN = "https://foundry.example";
const event = (seq: number, e: object) => ({
  seq,
  eventJson: JSON.stringify(e),
});

describe("pageContext", () => {
  it.each([
    ["/ontology/ont_1", "", { ontologyId: "ont_1" }],
    ["/ontology/ont_1/versions/v_2", "", { ontologyId: "ont_1" }],
    [
      "/ontology/ont_1/loads/load_2",
      "",
      { ontologyId: "ont_1", loadId: "load_2" },
    ],
    [
      "/ontology/ont_1/loads/load_2/rule-breaks",
      "",
      { ontologyId: "ont_1", loadId: "load_2" },
    ],
    // An ontology's own pages take no ?load=.
    ["/ontology/ont_1", "?load=load_9", { ontologyId: "ont_1" }],
    ["/explore/ont_1", "", { ontologyId: "ont_1" }],
    [
      "/explore/ont_1/records",
      "?type=Customer&load=load_3",
      { ontologyId: "ont_1", loadId: "load_3" },
    ],
    ["/ask/ont_1", "?load=load_4", { ontologyId: "ont_1", loadId: "load_4" }],
    // A malformed load is left out; the ontology stays.
    ["/ask/ont_1", "?load=a%20b", { ontologyId: "ont_1" }],
  ])("%s%s names %j", (path, search, context) => {
    expect(pageContext(path, search)).toEqual(context);
  });

  it.each([
    ["/"],
    ["/data"],
    ["/data/documents/doc_1"],
    ["/settings/ai"],
    ["/ontology"],
    ["/ontology/new"],
    ["/ontology/sample"],
    ["/explore"],
    ["/ontology/has space"],
    ["/ontology/" + "x".repeat(129)],
  ])("%s names nothing", (path) => {
    expect(pageContext(path, "?load=load_1")).toBeNull();
  });
});

describe("inAppPath", () => {
  const at = (path: string) => inAppPath(new URL(path, ORIGIN));
  it.each([
    ["/ontology", "/ontology"],
    ["/ontology/ont_1/", "/ontology/ont_1"],
    ["/ontology/ont_1?new-load=1", "/ontology/ont_1"],
    ["/ontology/ont_1/loads/l_2", "/ontology/ont_1/loads/l_2"],
    ["/ontology/ont_1/versions/v_1", "/ontology/ont_1/versions/v_1"],
    ["/ontology/ont_1/generations/g_1", "/ontology/ont_1/generations/g_1"],
    ["/ontology/ont_1/documents", "/ontology/ont_1/documents"],
    [
      "/ontology/ont_1/documents/d_1?tab=facts",
      "/ontology/ont_1/documents/d_1",
    ],
    ["/explore/ont_1?load=l_2&node=4", "/explore/ont_1?load=l_2"],
    ["/explore/ont_1/records?load=l_2", "/explore/ont_1/records?load=l_2"],
    ["/explore/ont_1/entities/e_1", "/explore/ont_1/entities/e_1"],
    ["/ask/ont_1?load=bad%20id", "/ask/ont_1"],
    ["/ask", "/ask"],
    ["/data?add=postgres&workspace=ws_9", "/data?add=postgres"],
    ["/data?add=1", "/data"],
    ["/data/documents/d_1", "/data/documents/d_1"],
    ["/data/connections/c_1?add=postgres", "/data/connections/c_1"],
    ["/settings/ai", "/settings/ai"],
  ])("opens %s as %s", (path, to) => expect(at(path)).toBe(to));

  it.each([
    "/ontology/sample",
    "/ontology/new/x",
    "/ontology/ont_1/generate",
    "/ontology/ont_1/loads",
    "/explore/ont_1/records/x",
    "/papi/v1/whoami",
    "/settings/secrets",
  ])("opens nothing for %s", (path) => expect(at(path)).toBeUndefined());
});

describe("onClaude", () => {
  it("runs on Claude alone, the workspace's own or the default", () => {
    expect(onClaude({ default: { kind: "FOUNDRY_AI_KIND_ANTHROPIC" } })).toBe(
      true,
    );
    expect(
      onClaude({
        active: { id: "c", kind: "FOUNDRY_AI_KIND_ANTHROPIC" },
        default: { kind: "FOUNDRY_AI_KIND_OLLAMA" },
      }),
    ).toBe(true);
    expect(
      onClaude({
        active: { id: "c", kind: "FOUNDRY_AI_KIND_BEDROCK" },
        default: { kind: "FOUNDRY_AI_KIND_ANTHROPIC" },
      }),
    ).toBe(false);
    expect(onClaude({ default: { kind: "FOUNDRY_AI_KIND_OLLAMA" } })).toBe(
      false,
    );
    expect(onClaude({})).toBeUndefined();
  });
});

describe("textParts", () => {
  it("links paths into the app's sections, keeping the punctuation after them", () => {
    expect(
      textParts("Follow it at /ontology/ont_1/loads/load_2.", ORIGIN),
    ).toEqual([
      { kind: "text", text: "Follow it at " },
      {
        kind: "link",
        text: "/ontology/ont_1/loads/load_2",
        to: "/ontology/ont_1/loads/load_2",
      },
      { kind: "text", text: "." },
    ]);
  });

  it("links this origin's addresses as in-app paths, without what a page may not take", () => {
    expect(
      textParts(`Open (${ORIGIN}/ask/ont_1?workspace=ws_1#x) now`, ORIGIN),
    ).toEqual([
      { kind: "text", text: "Open (" },
      {
        kind: "link",
        text: `${ORIGIN}/ask/ont_1?workspace=ws_1#x`,
        to: "/ask/ont_1",
      },
      { kind: "text", text: ") now" },
    ]);
  });

  it.each([
    ["another origin", "See https://evil.example/ontology/x please"],
    ["this origin outside the app's sections", `See ${ORIGIN}/papi/v1/whoami`],
    ["a protocol-relative address", "See //evil.example/ontology/x"],
    ["a path inside a word", "a/ontology/x"],
    ["an image", "![x](https://evil.example/leak?d=secret)"],
    ["a path of another section", "See /papi/v1/whoami or /login"],
    ["a path that climbs out of the app", "See /ontology/../papi/v1/whoami"],
    ["javascript", "javascript:alert(1)"],
    ["a malformed address", "See https://[::1 now"],
    ["the sample's page, which acts on arrival", "See /ontology/sample"],
    ["the new-ontology wizard", "See /ontology/new"],
    ["a page the app doesn't have", "See /data/anything/at-all"],
    ["words packed into a path", "See /ontology/a/b/c/d/e"],
    ["a section's page that isn't a route", "See /settings/secrets"],
  ])("leaves %s as text", (_, text) => {
    const parts = textParts(text, ORIGIN);
    expect(parts.every((p) => p.kind === "text")).toBe(true);
    expect(parts.map((p) => p.text).join("")).toBe(text);
  });

  it("keeps empty text empty and several links apart", () => {
    expect(textParts("", ORIGIN)).toEqual([]);
    expect(
      textParts("/data and /settings/ai", ORIGIN).map((p) => p.kind),
    ).toEqual(["link", "text", "link"]);
  });
});

describe("parseAgentEvent", () => {
  it.each([
    [
      { type: "started", model: "m", attempt: 2 },
      { type: "started", model: "m", attempt: 2 },
    ],
    [{ type: "started" }, { type: "started", model: "", attempt: 1 }],
    [
      { type: "text", text: "Hi" },
      { type: "text", text: "Hi" },
    ],
    [
      { type: "tool_call", tool_use_id: "t", name: "n", input: { a: 1 } },
      {
        type: "tool_call",
        tool_use_id: "t",
        name: "n",
        title: "",
        input: { a: 1 },
      },
    ],
    [
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "n",
        is_error: false,
        output: [1],
        truncated: true,
      },
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "n",
        is_error: false,
        output: [1],
        truncated: true,
      },
    ],
    [
      { type: "tool_result", tool_use_id: "t", is_error: true, error: "no" },
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "",
        is_error: true,
        error: "no",
      },
    ],
    [
      {
        type: "action_card",
        action_id: "a",
        operation: "create_load",
        target: "ontology:ws/o",
        payload: { version_id: "v" },
        payload_hash: "h",
        display: { ontology: { id: "o" }, version: { id: "v" }, extra: "x" },
        target_named_by_user: true,
        expires_at: "2030-01-01T00:00:00Z",
      },
      {
        type: "action_card",
        action_id: "a",
        operation: "create_load",
        target: "ontology:ws/o",
        payload: { version_id: "v" },
        payload_hash: "h",
        // Read by the card, by its operation (parseCard).
        display: { ontology: { id: "o" }, version: { id: "v" }, extra: "x" },
        target_named_by_user: true,
        expires_at: "2030-01-01T00:00:00Z",
      },
    ],
    [
      {
        type: "action_result",
        action_id: "a",
        operation: "create_load",
        status: "failed",
        error: "e",
      },
      {
        type: "action_result",
        action_id: "a",
        operation: "create_load",
        status: "failed",
        error: "e",
      },
    ],
    [
      { type: "finished", status: "done", reason: "answered" },
      { type: "finished", status: "done", reason: "answered" },
    ],
    [
      { type: "finished", status: "failed", reason: "failed", error: "x" },
      { type: "finished", status: "failed", reason: "failed", error: "x" },
    ],
  ])("reads %j", (input, parsed) => {
    expect(parseAgentEvent(JSON.stringify(input))).toEqual(parsed);
  });

  it.each([
    ["not JSON", "{"],
    ["an array", "[]"],
    ["an unknown type", '{"type":"text_delta","text":"x"}'],
    ["text that isn't a string", '{"type":"text","text":3}'],
    ["a call without its id", '{"type":"tool_call","name":"n"}'],
    ["a result without its id", '{"type":"tool_result","is_error":false}'],
    [
      "a card without its action",
      '{"type":"action_card","operation":"create_load"}',
    ],
    [
      "a result of no known status",
      '{"type":"action_result","action_id":"a","status":"maybe"}',
    ],
    [
      "an end of no known reason",
      '{"type":"finished","status":"done","reason":"bored"}',
    ],
    [
      "an end of no known status",
      '{"type":"finished","status":"ok","reason":"answered"}',
    ],
  ])("refuses %s", (_, json) => {
    expect(parseAgentEvent(json)).toBeUndefined();
  });
});

describe("reduceTurnEvents", () => {
  const call = {
    type: "tool_call",
    tool_use_id: "t1",
    name: "helix_foundry_get_load",
    title: "",
    input: {},
  };
  it("folds calls, results, text, cards and the end, and is complete at the last seq", () => {
    const s = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        event(3, {
          type: "tool_result",
          tool_use_id: "t1",
          name: "x",
          is_error: false,
          output: { ok: 1 },
        }),
        event(1, { type: "started", model: "m", attempt: 1 }),
        event(2, call),
        event(4, {
          type: "action_card",
          action_id: "a1",
          operation: "create_load",
          target_named_by_user: false,
        }),
        event(5, {
          type: "action_card",
          action_id: "a1",
          operation: "create_load",
        }),
        event(6, { type: "text", text: "Done." }),
        event(7, { type: "text", text: "  " }),
        event(8, { type: "finished", status: "done", reason: "max_steps" }),
      ],
      status: "done",
      lastSeq: 8,
    });
    expect(s.blocks).toEqual([
      {
        kind: "step",
        step: {
          id: "t1",
          name: "helix_foundry_get_load",
          input: {},
          result: { kind: "returned", output: { ok: 1 }, truncated: false },
        },
      },
      { kind: "card", actionId: "a1", operation: "create_load", named: false },
      { kind: "text", text: "Done." },
    ]);
    expect(s.ended).toEqual({ status: "done", reason: "max_steps" });
    expect(STOP_TEXT[s.ended!.reason]).toMatch(/step limit/);
    expect(s.complete).toBe(true);
    expect(s.lastSeq).toBe(8);
  });

  it("skips events folded already, and stays incomplete while the turn is live or short of its last seq", () => {
    const first = reduceTurnEvents(EMPTY_STREAM, {
      events: [event(1, { type: "text", text: "a" })],
      status: "running",
      lastSeq: 1,
    });
    expect(first.complete).toBe(false);
    const again = reduceTurnEvents(first, {
      events: [
        event(1, { type: "text", text: "a" }),
        event(2, { type: "text", text: "b" }),
      ],
      status: "done",
      lastSeq: 3,
    });
    expect(again.blocks.map((b) => b.kind === "text" && b.text)).toEqual([
      "a",
      "b",
    ]);
    expect(again.complete).toBe(false);
    const done = reduceTurnEvents(again, {
      events: [],
      status: "done",
      lastSeq: 2,
    });
    expect(done.complete).toBe(true);
    // An empty page with nothing new keeps the same stream.
    expect(
      reduceTurnEvents(done, { events: [], status: "done", lastSeq: 2 }),
    ).toBe(done);
  });

  it("keeps an earlier attempt's blocks, marks the retry, and counts what it can't read", () => {
    const s = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        event(1, { type: "started", model: "m", attempt: 1 }),
        event(2, { type: "text", text: "a" }),
        event(3, { type: "started", model: "m", attempt: 2 }),
        event(4, { type: "mystery" }),
        event(5, {
          type: "tool_result",
          tool_use_id: "orphan",
          name: "n",
          is_error: true,
        }),
        event(6, {
          type: "action_result",
          action_id: "a",
          operation: "create_load",
          status: "succeeded",
        }),
        event(7, {
          type: "finished",
          status: "failed",
          reason: "failed",
          error: "boom",
        }),
      ],
      status: "failed",
      lastSeq: 7,
    });
    expect(s.blocks).toEqual([
      { kind: "text", text: "a" },
      { kind: "retry", attempt: 2 },
      {
        kind: "step",
        step: {
          id: "orphan",
          name: "n",
          input: null,
          result: { kind: "failed", error: "it failed" },
        },
      },
      {
        kind: "ran",
        actionId: "a",
        operation: "create_load",
        status: "succeeded",
      },
    ]);
    expect(s.unreadable).toBe(1);
    expect(s.ended).toEqual({
      status: "failed",
      reason: "failed",
      error: "boom",
    });
  });

  it("answers a second result of one call as a step of its own", () => {
    const s = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        event(1, call),
        event(2, {
          type: "tool_result",
          tool_use_id: "t1",
          name: "x",
          is_error: false,
        }),
        event(3, {
          type: "tool_result",
          tool_use_id: "t1",
          name: "x",
          is_error: false,
        }),
      ],
    });
    expect(s.blocks).toHaveLength(2);
    expect(s.complete).toBe(false);
  });
});

describe("tools", () => {
  it("opens an ask's ontology in the Analyst", () => {
    expect(
      analystLink({
        id: "t",
        name: "helix_foundry_ask",
        input: { ontology_id: "ont_1" },
      }),
    ).toBe("/ask/ont_1");
    expect(
      analystLink({
        id: "t",
        name: "helix_foundry_ask",
        input: { ontology_id: "../x" },
      }),
    ).toBeUndefined();
    expect(
      analystLink({ id: "t", name: "helix_foundry_ask", input: null }),
    ).toBeUndefined();
    expect(
      analystLink({
        id: "t",
        name: "helix_foundry_get_load",
        input: { ontology_id: "ont_1" },
      }),
    ).toBeUndefined();
  });

  it("shows JSON indented and text as it is", () => {
    expect(pretty({ a: 1 })).toBe('{\n  "a": 1\n}');
    expect(pretty("plain")).toBe("plain");
    expect(pretty(undefined)).toBe("");
  });
});

describe("actions", () => {
  const NOW = Date.parse("2026-11-14T12:00:00Z");
  const action = (a: Partial<FoundryAgentAction>): FoundryAgentAction => ({
    id: "act_1",
    operation: "create_load",
    target: "ontology:ws_1/ont_1",
    payloadJson: '{"version_id":"v_3"}',
    payloadHash: "h",
    displayJson:
      '{"ontology":{"id":"ont_1","name":"Customer contracts"},"version":{"id":"v_3","number":3}}',
    status: "prepared",
    expiresAt: "2026-11-14T12:05:00Z",
    ...a,
  });

  it.each<[string, Partial<FoundryAgentAction>, unknown]>([
    [
      "a waiting action",
      {},
      {
        kind: "waiting",
        card: {
          operation: "create_load",
          display: {
            ontology: { id: "ont_1", name: "Customer contracts" },
            version: { id: "v_3", number: 3 },
          },
        },
      },
    ],
    [
      "one past its expiry by the clock",
      { expiresAt: "2026-11-14T11:59:59Z" },
      { kind: "expired" },
    ],
    ["one without an expiry", { expiresAt: "" }, { kind: "expired" }],
    ["one the WFE reads expired", { status: "expired" }, { kind: "expired" }],
    [
      "an operation this page doesn't know",
      { operation: "drop_database" },
      { kind: "unreadable" },
    ],
    [
      "a display that isn't its operation's",
      { displayJson: '{"ontology_name":"Customer contracts","version":3}' },
      { kind: "unreadable" },
    ],
    ["a running one", { status: "executing" }, { kind: "running" }],
    [
      "a ran one",
      { status: "succeeded" },
      { kind: "ended", status: "succeeded" },
    ],
    ["a failed one", { status: "failed" }, { kind: "ended", status: "failed" }],
    [
      "one whose outcome is lost",
      { status: "unknown" },
      { kind: "ended", status: "unknown" },
    ],
    [
      "a dismissed one",
      { status: "dismissed" },
      { kind: "ended", status: "dismissed" },
    ],
    [
      "one of a status this page doesn't know",
      { status: "weird" as never },
      { kind: "ended", status: "unknown" },
    ],
  ])("offers %s what it can", (_, a, state) => {
    expect(cardState(action(a), NOW)).toEqual(state);
  });

  it("knows the operations the agent may prepare, and no other key", () => {
    expect(isAgentOperation("create_load")).toBe(true);
    expect(isAgentOperation("settle_review")).toBe(true);
    expect(isAgentOperation("upload_document")).toBe(false);
    expect(isAgentOperation("toString")).toBe(false);
  });

  it("opens a created load, and nothing it can't read", () => {
    const ran = (resultJson: string, a: Partial<FoundryAgentAction> = {}) =>
      resultLink(action({ status: "succeeded", resultJson, ...a }));
    expect(
      ran('{"content_trust":"untrusted","result":{"id":"load_9"}}'),
    ).toEqual({ to: "/ontology/ont_1/loads/load_9", label: "Open the load" });
    expect(ran('{"result":{"id":"load 9"}}')).toBeUndefined();
    expect(ran('{"result":"load_9"}')).toBeUndefined();
    expect(ran("{")).toBeUndefined();
    expect(
      ran('{"result":{"id":"l"}}', { operation: "other" }),
    ).toBeUndefined();
    expect(
      ran('{"result":{"id":"l"}}', { target: "ontology:ws/../x" }),
    ).toBeUndefined();
  });

  it("shows the payload indented, or as stored when it isn't JSON", () => {
    expect(payloadText(action({}))).toBe('{\n  "version_id": "v_3"\n}');
    expect(payloadText(action({ payloadJson: "not json" }))).toBe("not json");
  });
});

describe("agentRefusal", () => {
  const refusal = (reason: unknown) =>
    new ApiError("no", 400, 9, [
      { "@type": "type.googleapis.com/google.rpc.ErrorInfo", reason },
    ]);
  it("reads the Assistant's own reasons only", () => {
    expect(agentRefusal(refusal("FOUNDRY_AGENT_ACTION_EXPIRED"))).toBe(
      "FOUNDRY_AGENT_ACTION_EXPIRED",
    );
    expect(agentRefusal(refusal("FOUNDRY_AGENT_THREAD_BUSY"))).toBe(
      "FOUNDRY_AGENT_THREAD_BUSY",
    );
    expect(agentRefusal(refusal("FOUNDRY_PRO_REQUIRED"))).toBeUndefined();
    expect(agentRefusal(new ApiError("no", 400))).toBeUndefined();
    expect(agentRefusal(new Error("no"))).toBeUndefined();
  });
});

it("keeps the open thread per workspace", () => {
  expect(threadKey("ws_1")).toBe("foundry.agent.thread:ws_1");
});
