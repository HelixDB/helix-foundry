import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import type { FoundryAgentAction, FoundryAgentHost } from "@/shared/wfe";
import { ApiError } from "@/web/api";
import {
  EMPTY_STREAM,
  HELPER_COMMANDS,
  HELPER_PACKAGE,
  STARTS_WORK,
  STOP_TEXT,
  accountText,
  agentRefusal,
  answerBlocks,
  autoState,
  cardState,
  helperAccount,
  isAgentOperation,
  madeDashboard,
  pageContext,
  parseAgentEvent,
  planName,
  payloadText,
  pretty,
  questionOf,
  stepLabel,
  reduceTurnEvents,
  resultLink,
  inAppPath,
  textParts,
  threadKey,
  threadRuntime,
  type AgentBlock,
  type AgentQuestionRef,
  type AgentStep,
  type TurnStream,
} from "@/web/areas/agent/model";

// Founder's dock's pure logic (src/web/areas/agent/model.ts): what page
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
    // A dashboard, which takes no load.
    ["/dashboards/dash_1", "?load=load_1", { dashboardId: "dash_1" }],
    // The page that starts a new workflow, its conversation or a draft.
    ["/workflows/new", "", { newAutomation: true }],
    ["/workflows/new", "?template=blank", { newAutomation: true }],
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
    ["/dashboards"],
    ["/dashboards/dash_1/edit"],
    ["/dashboards/has space"],
    ["/workflows"],
    ["/workflows/aut_1"],
    ["/workflows/new/x"],
    // Before automations became workflows.
    ["/automations/new"],
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
    [
      "/ontology/ont_1/connections?status=review",
      "/ontology/ont_1/connections",
    ],
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
    ["/dashboards", "/dashboards"],
    ["/dashboards/dash_1?load=l_1", "/dashboards/dash_1"],
    ["/workflows", "/workflows"],
    ["/workflows/aut_1", "/workflows/aut_1"],
    ["/workflows/aut_1/runs/run_2?x=1", "/workflows/aut_1/runs/run_2"],
    ["/ontology/ont_1/usage", "/ontology/ont_1/usage"],
    ["/ontology/ont_1/usage/wf_1?tab=runs", "/ontology/ont_1/usage/wf_1"],
    ["/ontology/ont_1/usage/wf_1/runs/r_1", "/ontology/ont_1/usage/wf_1/runs/r_1"],
  ])("opens %s as %s", (path, to) => expect(at(path)).toBe(to));

  it.each([
    "/ontology/sample",
    "/ontology/new/x",
    "/ontology/ont_1/generate",
    "/ontology/ont_1/loads",
    "/explore/ont_1/records/x",
    "/papi/v1/whoami",
    "/settings/secrets",
    "/dashboards/dash_1/x",
    "/workflows/new",
    "/workflows/aut_1/edit",
    "/automations/aut_1",
    "/ontology/ont_1/workflows",
    "/ontology/ont_1/usage/wf_1/x",
    "/share/hxd_token",
  ])("opens nothing for %s", (path) => expect(at(path)).toBeUndefined());
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
      { type: "text", step: 1, text: "Hi" },
      { type: "text", step: 1, text: "Hi" },
    ],
    [
      { type: "text_delta", step: 3, text: " wor" },
      { type: "text_delta", step: 3, text: " wor" },
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
      // An ask's question, kept however large its output.
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "helix_foundry_ask",
        is_error: false,
        truncated: true,
        question: {
          ontology_id: "ont_1",
          id: "q_1",
          load_id: "load_1",
          thread_id: "q_0",
          status: "answered",
        },
      },
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "helix_foundry_ask",
        is_error: false,
        truncated: true,
        question: {
          ontology_id: "ont_1",
          id: "q_1",
          load_id: "load_1",
          thread_id: "q_0",
          status: "answered",
        },
      },
    ],
    [
      // A load or thread id that isn't one is left out, and so is a status
      // that isn't text.
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "helix_foundry_get_question",
        is_error: false,
        question: {
          ontology_id: "ont_1",
          id: "q_1",
          load_id: "../l",
          thread_id: 3,
          status: 1,
        },
      },
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "helix_foundry_get_question",
        is_error: false,
        question: { ontology_id: "ont_1", id: "q_1" },
      },
    ],
    [
      // A question whose ontology or own id isn't one is dropped whole.
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "helix_foundry_ask",
        is_error: false,
        question: { ontology_id: "../o", id: "q_1" },
      },
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "helix_foundry_ask",
        is_error: false,
      },
    ],
    [
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "helix_foundry_ask",
        is_error: false,
        question: { ontology_id: "ont_1" },
      },
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "helix_foundry_ask",
        is_error: false,
      },
    ],
    [
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "helix_foundry_ask",
        is_error: false,
        question: "q_1",
      },
      {
        type: "tool_result",
        tool_use_id: "t",
        name: "helix_foundry_ask",
        is_error: false,
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
      // Ran at once: only true counts.
      {
        type: "action_card",
        action_id: "a",
        operation: "create_dashboard",
        target: "workspace:ws",
        payload_hash: "h",
        target_named_by_user: true,
        expires_at: "2030-01-01T00:00:00Z",
        auto: true,
      },
      {
        type: "action_card",
        action_id: "a",
        operation: "create_dashboard",
        target: "workspace:ws",
        payload: undefined,
        payload_hash: "h",
        display: undefined,
        target_named_by_user: true,
        expires_at: "2030-01-01T00:00:00Z",
        auto: true,
      },
    ],
    [
      {
        type: "action_card",
        action_id: "a",
        operation: "create_dashboard",
        auto: "yes",
      },
      {
        type: "action_card",
        action_id: "a",
        operation: "create_dashboard",
        target: "",
        payload: undefined,
        payload_hash: "",
        display: undefined,
        target_named_by_user: false,
        expires_at: "",
      },
    ],
    [
      {
        type: "action_result",
        action_id: "a",
        operation: "create_dashboard",
        status: "succeeded",
        result: { id: "dash_1" },
        auto: true,
      },
      {
        type: "action_result",
        action_id: "a",
        operation: "create_dashboard",
        status: "succeeded",
        result: { id: "dash_1" },
        auto: true,
      },
    ],
    [
      {
        type: "action_result",
        action_id: "a",
        operation: "create_load",
        status: "failed",
        error: "e",
        auto: 1,
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
      { type: "automation_report", summary: "Two", answer: false, count: 2 },
      { type: "automation_report", summary: "Two", answer: false, count: 2 },
    ],
    [
      { type: "automation_report", summary: "S", answer: "yes", count: 1.5 },
      { type: "automation_report", summary: "S" },
    ],
    [
      { type: "finished", status: "done", reason: "answered" },
      { type: "finished", status: "done", reason: "answered" },
    ],
    [
      { type: "finished", status: "failed", reason: "failed", error: "x" },
      { type: "finished", status: "failed", reason: "failed", error: "x" },
    ],
    [
      { type: "finished", status: "done", reason: "asked" },
      { type: "finished", status: "done", reason: "asked" },
    ],
    [
      {
        type: "questions",
        questions: [{ id: "q1", text: "Which?" }],
        generation: {
          ontology_id: "o",
          ontology_name: "Sales",
          generation_id: "g",
          generation_title: "Renewals",
          round: 2,
        },
      },
      {
        type: "questions",
        questions: [{ id: "q1", text: "Which?" }],
        generation: {
          ontology_id: "o",
          ontology_name: "Sales",
          generation_id: "g",
          generation_title: "Renewals",
          round: 2,
        },
      },
    ],
    [
      // A generation it can't read: Founder's own questions.
      { type: "questions", questions: [], generation: { ontology_id: "o" } },
      { type: "questions", questions: [] },
    ],
    [
      {
        type: "answers",
        turn_id: "t",
        answers: [
          { question_id: "q1", options: ["A", 3], text: "x" },
          { question_id: "q2" },
          "nope",
        ],
      },
      {
        type: "answers",
        turn_id: "t",
        answers: [
          { question_id: "q1", options: ["A"], text: "x" },
          { question_id: "q2", options: [] },
        ],
      },
    ],
  ])("reads %j", (input, parsed) => {
    expect(parseAgentEvent(JSON.stringify(input))).toEqual(parsed);
  });

  it.each([
    ["not JSON", "{"],
    ["an array", "[]"],
    ["an unknown type", '{"type":"text_chunk","step":1,"text":"x"}'],
    ["text that isn't a string", '{"type":"text","step":1,"text":3}'],
    ["text without its step", '{"type":"text","text":"x"}'],
    ["a delta without its step", '{"type":"text_delta","text":"x"}'],
    ["a step that isn't a count", '{"type":"text_delta","step":0,"text":"x"}'],
    ["a fractional step", '{"type":"text","step":1.5,"text":"x"}'],
    ["a step as a string", '{"type":"text_delta","step":"1","text":"x"}'],
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
    ["a report without its summary", '{"type":"automation_report"}'],
    ["questions that aren't a list", '{"type":"questions","questions":{}}'],
    ["questions that aren't objects", '{"type":"questions","questions":["q"]}'],
    ["answers without their turn", '{"type":"answers","answers":[]}'],
    ["answers that aren't a list", '{"type":"answers","turn_id":"t"}'],
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
  it("folds a turn's questions where they show, and an answers turn's answers into nothing", () => {
    const q = { id: "q1", text: "Which?" };
    const s = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        event(1, {
          type: "answers",
          turn_id: "t0",
          answers: [{ question_id: "q0", options: ["A"] }],
        }),
        event(2, { type: "text", step: 1, text: "First:" }),
        event(3, { type: "questions", questions: [q] }),
        event(4, {
          type: "questions",
          questions: [q],
          generation: {
            ontology_id: "o",
            ontology_name: "Sales",
            generation_id: "g",
            round: 1,
          },
        }),
        event(5, {
          type: "questions",
          questions: [q],
          generation: {
            ontology_id: "o",
            ontology_name: "",
            generation_id: "g",
            generation_title: "Renewals",
            round: 1,
          },
        }),
        event(6, { type: "finished", status: "done", reason: "asked" }),
      ],
      status: "done",
      lastSeq: 6,
    });
    expect(s.blocks).toEqual([
      { kind: "text", text: "First:" },
      { kind: "asked", questions: [q] },
      { kind: "asked", questions: [q], generation: "the draft of Sales" },
      { kind: "asked", questions: [q], generation: "Renewals" },
    ]);
    expect(STOP_TEXT[s.ended!.reason]).toBeNull();
    const nameless = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        event(1, {
          type: "questions",
          questions: [q],
          generation: {
            ontology_id: "o",
            ontology_name: "",
            generation_id: "g",
            round: 1,
          },
        }),
      ],
      status: "done",
      lastSeq: 1,
    });
    expect(nameless.blocks).toEqual([
      { kind: "asked", questions: [q], generation: "the draft of an ontology" },
    ]);
  });

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
        event(6, { type: "text", step: 1, text: "Done." }),
        event(7, { type: "text", step: 1, text: "  " }),
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
      {
        kind: "card",
        actionId: "a1",
        operation: "create_load",
        named: false,
        auto: false,
      },
      { kind: "text", text: "Done." },
    ]);
    expect(s.ended).toEqual({ status: "done", reason: "max_steps" });
    expect(STOP_TEXT[s.ended!.reason]).toMatch(/step limit/);
    expect(s.complete).toBe(true);
    expect(s.lastSeq).toBe(8);
  });

  it("skips events folded already, and stays incomplete while the turn is live or short of its last seq", () => {
    const first = reduceTurnEvents(EMPTY_STREAM, {
      events: [event(1, { type: "text", step: 1, text: "a" })],
      status: "running",
      lastSeq: 1,
    });
    expect(first.complete).toBe(false);
    const again = reduceTurnEvents(first, {
      events: [
        event(1, { type: "text", step: 1, text: "a" }),
        event(2, { type: "text", step: 2, text: "b" }),
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
        event(2, { type: "text", step: 1, text: "a" }),
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

  it("ends an action that ran at once on its card, with no line of its own", () => {
    const s = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        event(1, {
          type: "tool_call",
          tool_use_id: "t1",
          name: "helix_foundry_prepare_operation",
          input: { operation: "create_dashboard" },
        }),
        event(2, {
          type: "action_card",
          action_id: "a1",
          operation: "create_dashboard",
          target_named_by_user: true,
          auto: true,
        }),
        event(3, {
          type: "action_result",
          action_id: "a1",
          operation: "create_dashboard",
          status: "succeeded",
          result: { id: "dash_1" },
          auto: true,
        }),
        event(4, {
          type: "tool_result",
          tool_use_id: "t1",
          name: "helix_foundry_prepare_operation",
          is_error: false,
          output: { action_id: "a1", status: "ran" },
        }),
      ],
      status: "done",
      lastSeq: 4,
    });
    expect(s.blocks).toEqual([
      {
        kind: "step",
        step: {
          id: "t1",
          name: "helix_foundry_prepare_operation",
          input: { operation: "create_dashboard" },
          result: {
            kind: "returned",
            output: { action_id: "a1", status: "ran" },
            truncated: false,
          },
        },
      },
      {
        kind: "card",
        actionId: "a1",
        operation: "create_dashboard",
        named: true,
        auto: true,
      },
    ]);
  });

  it("keeps the question an ask returned on its step, however large", () => {
    const s = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        event(1, {
          type: "tool_call",
          tool_use_id: "t1",
          name: "helix_foundry_ask",
          input: { ontology_id: "ont_1", question: "Which invoices?" },
        }),
        event(2, {
          type: "tool_result",
          tool_use_id: "t1",
          name: "helix_foundry_ask",
          is_error: false,
          truncated: true,
          question: {
            ontology_id: "ont_1",
            id: "q_1",
            load_id: "load_1",
            status: "running",
          },
        }),
      ],
      status: "done",
      lastSeq: 2,
    });
    expect(s.blocks).toEqual([
      {
        kind: "step",
        step: {
          id: "t1",
          name: "helix_foundry_ask",
          input: { ontology_id: "ont_1", question: "Which invoices?" },
          result: {
            kind: "returned",
            output: undefined,
            truncated: true,
            question: {
              ontologyId: "ont_1",
              questionId: "q_1",
              loadId: "load_1",
            },
          },
        },
      },
    ]);
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

describe("live text", () => {
  const delta = (seq: number, step: number, text: string) =>
    event(seq, { type: "text_delta", step, text });
  const text = (seq: number, step: number, t: string) =>
    event(seq, { type: "text", step, text: t });
  const shown = (s: TurnStream) => [
    ...s.blocks.map((b) => (b.kind === "text" ? b.text : b.kind)),
    ...(s.live ? [`live:${s.live.text}`] : []),
  ];

  it("appends a step's deltas across polls, then shows its text once in their place", () => {
    const one = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        event(1, { type: "started", model: "m", attempt: 1 }),
        delta(2, 1, "Customer "),
        delta(3, 1, "contracts "),
      ],
      status: "running",
      lastSeq: 3,
    });
    expect(one.live).toEqual({ step: 1, text: "Customer contracts " });
    expect(one.blocks).toEqual([]);
    // A poll that overlaps the last folds nothing twice.
    const two = reduceTurnEvents(one, {
      events: [delta(3, 1, "contracts "), delta(4, 1, "holds")],
      status: "running",
      lastSeq: 4,
    });
    expect(two.live).toEqual({ step: 1, text: "Customer contracts holds" });
    // The server drops the deltas as it writes the text: the next poll reads
    // only the text, which replaces them.
    const three = reduceTurnEvents(two, {
      events: [
        text(5, 1, "Customer contracts holds 3 types."),
        event(6, { type: "finished", status: "done", reason: "answered" }),
      ],
      status: "done",
      lastSeq: 6,
    });
    expect(shown(three)).toEqual(["Customer contracts holds 3 types."]);
    expect(three.complete).toBe(true);
  });

  it("reads a turn reloaded mid-stream from its deltas, and one reloaded after its text without them", () => {
    // Read afresh while step 2 streams: step 1's deltas are gone already.
    const mid = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        event(1, { type: "started", model: "m", attempt: 1 }),
        text(2, 1, "Reading."),
        event(3, {
          type: "tool_call",
          tool_use_id: "t",
          name: "helix_foundry_get_load",
        }),
        event(4, { type: "tool_result", tool_use_id: "t", is_error: false }),
        delta(6, 2, "It has "),
        delta(7, 2, "two."),
      ],
      status: "running",
      lastSeq: 7,
    });
    expect(shown(mid)).toEqual(["Reading.", "step", "live:It has two."]);
    const after = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        event(1, { type: "started", model: "m", attempt: 1 }),
        text(8, 2, "It has two."),
      ],
      status: "running",
      lastSeq: 8,
    });
    expect(shown(after)).toEqual(["It has two."]);
  });

  it("keeps every text of a step, and ends deltas no text followed at the next event", () => {
    const s = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        delta(1, 1, "Half a"),
        // The stream broke: a later attempt starts, and its call streams.
        event(2, { type: "started", model: "m", attempt: 2 }),
        delta(3, 2, "Again"),
        text(4, 2, "First."),
        text(5, 2, "Second."),
        delta(6, 3, "Never "),
        // A delta of another step replaces what is live.
        delta(7, 4, "Last"),
      ],
      status: "running",
      lastSeq: 7,
    });
    expect(shown(s)).toEqual(["retry", "First.", "Second.", "live:Last"]);
    const ended = reduceTurnEvents(s, {
      events: [
        event(8, {
          type: "finished",
          status: "failed",
          reason: "failed",
          error: "x",
        }),
      ],
      status: "failed",
      lastSeq: 8,
    });
    expect(ended.live).toBeUndefined();
  });

  it("drops what a stopped turn left live once it is complete", () => {
    const live = reduceTurnEvents(EMPTY_STREAM, {
      events: [delta(1, 1, "Wri")],
      status: "running",
      lastSeq: 1,
    });
    expect(live.live?.text).toBe("Wri");
    // Stopped: no finished event and no text; the turn just ends.
    const stopped = reduceTurnEvents(live, {
      events: [],
      status: "cancelled",
      lastSeq: 1,
    });
    expect(stopped.complete).toBe(true);
    expect(stopped.live).toBeUndefined();
    // Read afresh once stopped, its deltas show nothing either.
    expect(
      reduceTurnEvents(EMPTY_STREAM, {
        events: [delta(1, 1, "Wri")],
        status: "cancelled",
        lastSeq: 1,
      }).live,
    ).toBeUndefined();
  });

  it("drops what was live once the turn is back in the queue to retry", () => {
    const live = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        event(1, { type: "started", model: "m", attempt: 1 }),
        delta(2, 1, "Half an ans"),
      ],
      status: "running",
      lastSeq: 2,
    });
    expect(live.live?.text).toBe("Half an ans");
    // The stream broke and the worker queued a retry: no event says so,
    // only the turn's status.
    const queued = reduceTurnEvents(live, {
      events: [],
      status: "queued",
      lastSeq: 2,
    });
    expect(queued.live).toBeUndefined();
    expect(queued.complete).toBe(false);
    // The retry's own deltas show again once it runs.
    const again = reduceTurnEvents(queued, {
      events: [
        event(3, { type: "started", model: "m", attempt: 2 }),
        delta(4, 2, "A whole"),
      ],
      status: "running",
      lastSeq: 4,
    });
    expect(shown(again)).toEqual(["retry", "live:A whole"]);
  });

  it("counts a delta it can't read, and keeps what is live", () => {
    const s = reduceTurnEvents(EMPTY_STREAM, {
      events: [delta(1, 1, "a"), event(2, { type: "text_delta", text: "b" })],
      status: "running",
      lastSeq: 2,
    });
    expect(s.live).toEqual({ step: 1, text: "a" });
    expect(s.unreadable).toBe(1);
  });

  it("shows an automation step's report", () => {
    const s = reduceTurnEvents(EMPTY_STREAM, {
      events: [
        event(1, { type: "automation_report", summary: "Done.", answer: true }),
        event(2, { type: "automation_report", summary: "Again.", count: 3 }),
      ],
    });
    expect(s.blocks).toEqual([
      { kind: "report", summary: "Done.", answer: true },
      { kind: "report", summary: "Again.", count: 3 },
    ]);
  });
});

describe("tools", () => {
  const returned = (
    name: string,
    input: unknown,
    output: unknown,
    question?: AgentQuestionRef,
  ): AgentStep => ({
    id: "t",
    name,
    input,
    result: {
      kind: "returned",
      output,
      truncated: false,
      ...(question && { question }),
    },
  });

  it.each<[string, AgentStep, unknown]>([
    [
      "an ask's question, from its output and input",
      returned(
        "helix_foundry_ask",
        { ontology_id: "ont_1", question: "Which?" },
        { id: "q_1", load_id: "load_1", thread_id: "q_0", status: "answered" },
      ),
      {
        ontologyId: "ont_1",
        questionId: "q_1",
        loadId: "load_1",
        threadId: "q_0",
      },
    ],
    [
      "a get_question's, without the ids that aren't ones",
      returned(
        "helix_foundry_get_question",
        { ontology_id: "ont_1", question_id: "q_1" },
        { id: "q_1", load_id: "../l", thread_id: 7 },
      ),
      { ontologyId: "ont_1", questionId: "q_1" },
    ],
    [
      "the event's question first, when the output was cut",
      returned("helix_foundry_ask", { ontology_id: "ont_1" }, undefined, {
        ontologyId: "ont_2",
        questionId: "q_9",
      }),
      { ontologyId: "ont_2", questionId: "q_9" },
    ],
    [
      "nothing for an input whose ontology isn't an id",
      returned("helix_foundry_ask", { ontology_id: "../x" }, { id: "q_1" }),
      undefined,
    ],
    [
      "nothing for an output without an id",
      returned("helix_foundry_ask", { ontology_id: "ont_1" }, { status: "x" }),
      undefined,
    ],
    [
      "nothing for an input that isn't an object",
      returned("helix_foundry_ask", null, { id: "q_1" }),
      undefined,
    ],
    [
      "nothing for another tool",
      returned(
        "helix_foundry_get_load",
        { ontology_id: "ont_1" },
        { id: "load_1" },
        { ontologyId: "ont_1", questionId: "q_1" },
      ),
      undefined,
    ],
    [
      "nothing for an ask that failed",
      {
        id: "t",
        name: "helix_foundry_ask",
        input: { ontology_id: "ont_1" },
        result: { kind: "failed", error: "30 questions an hour" },
      },
      undefined,
    ],
    [
      "nothing for an ask that never ran",
      {
        id: "t",
        name: "helix_foundry_ask",
        input: { ontology_id: "ont_1" },
        result: { kind: "not_run", reason: "asks" },
      },
      undefined,
    ],
    [
      "nothing for an ask still running",
      { id: "t", name: "helix_foundry_ask", input: { ontology_id: "ont_1" } },
      undefined,
    ],
  ])("reads %s", (_, step, ref) => {
    expect(questionOf(step)).toEqual(ref);
  });

  it("shows a question's answer once, under the last step naming it", () => {
    const step = (id: string, name: string, q: string): AgentBlock => ({
      kind: "step",
      step: { ...returned(name, { ontology_id: "ont_1" }, { id: q }), id },
    });
    const blocks: AgentBlock[] = [
      step("a", "helix_foundry_ask", "q_1"),
      { kind: "text", text: "Still working." },
      step("b", "helix_foundry_get_question", "q_1"),
      step("c", "helix_foundry_ask", "q_2"),
      step("d", "helix_foundry_get_load", "load_1"),
    ];
    expect([...answerBlocks(blocks)].sort()).toEqual([2, 3]);
    expect(answerBlocks([]).size).toBe(0);
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

  it("names only operations it knows as starting work", () => {
    expect([...STARTS_WORK].filter((op) => !isAgentOperation(op))).toEqual([]);
    expect(STARTS_WORK.has("create_load")).toBe(true);
    expect(STARTS_WORK.has("create_dashboard")).toBe(false);
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

  it.each<[string, Partial<FoundryAgentAction>, boolean, string]>([
    ["running", { status: "executing" }, false, "making"],
    ["not yet recorded as run", { status: "prepared" }, false, "making"],
    ["made", { status: "succeeded" }, false, "made"],
    ["made, then gone", { status: "succeeded" }, true, "undone"],
    ["failed", { status: "failed" }, false, "failed"],
    ["lost", { status: "unknown" }, false, "unknown"],
    [
      "of a status this page doesn't know",
      { status: "weird" as never },
      false,
      "unknown",
    ],
  ])("words an action that ran at once: %s", (_, a, gone, state) => {
    expect(autoState(action(a), gone)).toBe(state);
  });

  it("reads the dashboard a ran create_dashboard made, and nothing else", () => {
    const made = (a: Partial<FoundryAgentAction>) =>
      madeDashboard(action({ operation: "create_dashboard", ...a }));
    expect(made({ resultJson: '{"result":{"id":"dash_1"}}' })).toBe("dash_1");
    expect(made({ resultJson: '{"result":{"id":"../d"}}' })).toBeUndefined();
    expect(made({ resultJson: '{"result":"dash_1"}' })).toBeUndefined();
    expect(made({ resultJson: "{" })).toBeUndefined();
    expect(made({})).toBeUndefined();
    expect(
      made({
        operation: "create_load",
        resultJson: '{"result":{"id":"load_1"}}',
      }),
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
  it("reads Founder's own reasons only", () => {
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

describe("where Founder runs", () => {
  const host = (accounts: FoundryAgentHost["accounts"]): FoundryAgentHost => ({
    id: "h",
    runtimes: ["claude_code"],
    accounts,
  });

  it("reads what a helper's CLI is signed in to, without trust", () => {
    expect(
      helperAccount(
        host([
          { runtime: "claude_code", state: "signed_in", subscription: "max" },
        ]),
        "claude_code",
      ),
    ).toEqual({ kind: "subscription", plan: "max" });
    expect(
      helperAccount(
        host([{ runtime: "claude_code", state: "signed_in" }]),
        "claude_code",
      ),
    ).toEqual({ kind: "other" });
    expect(
      helperAccount(
        host([{ runtime: "claude_code", state: "signed_out" }]),
        "claude_code",
      ),
    ).toEqual({ kind: "signed_out" });
    expect(
      helperAccount(
        host([{ runtime: "claude_code", state: "unknown" }]),
        "claude_code",
      ),
    ).toEqual({ kind: "unknown" });
    // Not said yet, said of another runtime, or a state it doesn't know.
    expect(helperAccount(host(undefined), "claude_code")).toEqual({
      kind: "unchecked",
    });
    expect(
      helperAccount(
        host([{ runtime: "codex", state: "signed_out" }]),
        "claude_code",
      ),
    ).toEqual({ kind: "unchecked" });
    expect(
      helperAccount(
        host([{ runtime: "claude_code", state: "maybe" as "unknown" }]),
        "claude_code",
      ),
    ).toEqual({ kind: "unchecked" });
  });

  it("says it in words, and whether its turns bill the subscription", () => {
    expect(
      accountText({ kind: "subscription", plan: "max" }, "claude_code"),
    ).toEqual({
      text: "Claude Code is signed in on a Max subscription",
      ok: true,
    });
    expect(accountText({ kind: "other" }, "claude_code")).toEqual({
      text: "Claude Code is signed in, but not on a subscription: its turns bill that account",
      ok: false,
    });
    expect(accountText({ kind: "signed_out" }, "claude_code")).toEqual({
      text: "Claude Code isn’t signed in: on that computer, run claude, then /login",
      ok: false,
    });
    expect(accountText({ kind: "signed_out" }, "codex").text).toBe(
      "Codex isn’t signed in: on that computer, run codex login",
    );
    expect(accountText({ kind: "unknown" }, "claude_code").ok).toBe(false);
    expect(accountText({ kind: "unchecked" }, "claude_code")).toEqual({
      text: "Claude Code’s sign-in isn’t known yet",
      ok: true,
    });
    expect(planName("max")).toBe("Max");
    expect(planName("claude_max_20x")).toBe("Claude max 20x");
    expect(planName("team-plan")).toBe("Team plan");
  });

  it("says Codex's plan and version, and when it is too old to run", () => {
    expect(
      accountText(
        { kind: "subscription", plan: "plus", version: "0.169.0" },
        "codex",
      ),
    ).toEqual({
      text: "Codex 0.169.0 is signed in on a ChatGPT Plus plan",
      ok: true,
    });
    expect(accountText({ kind: "other", version: "0.170.0" }, "codex")).toEqual(
      {
        text: "Codex 0.170.0 is signed in, but not with a ChatGPT plan: its turns bill that account",
        ok: false,
      },
    );
    expect(
      accountText({ kind: "outdated", version: "0.150.0" }, "codex", "0.169.0"),
    ).toEqual({
      text: "Codex 0.150.0 is older than 0.169.0, the oldest Foundry runs: on that computer, update it (npm i -g @openai/codex@latest)",
      ok: false,
    });
    expect(accountText({ kind: "outdated" }, "codex").text).toBe(
      "Codex is older than Foundry runs: on that computer, update it (npm i -g @openai/codex@latest)",
    );
    const h = (a: object) => ({
      id: "h",
      accounts: [{ runtime: "codex" as const, ...a }],
    });
    expect(
      helperAccount(
        h({ state: "signed_in", subscription: "pro", version: "0.170.0" }),
        "codex",
      ),
    ).toEqual({ kind: "subscription", plan: "pro", version: "0.170.0" });
    expect(
      helperAccount(
        h({
          state: "signed_in",
          subscription: "pro",
          version: "0.150.0",
          outdated: true,
        }),
        "codex",
      ),
    ).toEqual({ kind: "outdated", version: "0.150.0" });
    expect(
      helperAccount(h({ state: "signed_out", version: "0.1\n" }), "codex"),
    ).toEqual({ kind: "signed_out" });
  });

  it("says a thread can't be sent while its helper's Codex is too old", () => {
    const setting = {
      runtime: "codex" as const,
      online: ["claude_code" as const],
      codexMinVersion: "0.169.0",
      hosts: [
        {
          id: "h1",
          name: "Ada's Mac",
          online: true,
          runtimes: ["claude_code" as const],
          accounts: [
            {
              runtime: "codex" as const,
              state: "signed_in" as const,
              version: "0.150.0",
              outdated: true,
            },
          ],
        },
      ],
    };
    expect(threadRuntime(undefined, setting)).toEqual({
      runtime: "codex",
      state: "outdated",
      helper: { host: "Ada's Mac", version: "0.150.0", min: "0.169.0" },
    });
    // A Codex that didn't start says no version: it isn't old, it didn't run.
    const unstarted = {
      ...setting,
      hosts: [
        {
          ...setting.hosts[0]!,
          accounts: [{ runtime: "codex" as const, state: "unknown" as const }],
        },
      ],
    };
    expect(threadRuntime(undefined, unstarted)).toEqual({
      runtime: "codex",
      state: "unstarted",
      helper: { host: "Ada's Mac", version: undefined, min: "0.169.0" },
    });
    expect(
      threadRuntime("codex", {
        ...unstarted,
        hosts: [{ ...unstarted.hosts[0]!, online: false }],
      }).state,
    ).toBe("stopped");
    // Claude Code that didn't start still runs: no floor, nothing to block.
    expect(
      threadRuntime("claude_code", {
        ...unstarted,
        runtime: "claude_code" as const,
        online: [],
        hosts: [
          {
            ...unstarted.hosts[0]!,
            runtimes: [],
            accounts: [
              { runtime: "claude_code" as const, state: "unknown" as const },
            ],
          },
        ],
      }).state,
    ).toBe("unpaired");
    // Not running, it is to be started (it may run an updated Codex then).
    const stopped = {
      ...setting,
      hosts: [{ ...setting.hosts[0]!, online: false }],
    };
    expect(threadRuntime("codex", stopped)).toEqual({
      runtime: "codex",
      state: "stopped",
    });
  });

  it("knows whether a thread can be sent to now, and what stands in the way", () => {
    const setting = { runtime: "claude_code" as const, online: [] };
    // A thread keeps its runtime, whatever the workspace's is now.
    expect(threadRuntime("api", setting)).toEqual({
      runtime: "api",
      state: "ready",
    });
    // No helper paired runs it: one must be paired, not started.
    expect(threadRuntime("claude_code", setting)).toEqual({
      runtime: "claude_code",
      state: "unpaired",
    });
    // One paired runs it, and isn't running: start it.
    const paired = {
      ...setting,
      hosts: [{ id: "h1", runtimes: ["claude_code" as const] }],
    };
    expect(threadRuntime("claude_code", paired)).toEqual({
      runtime: "claude_code",
      state: "stopped",
    });
    // A helper of another runtime doesn't run it.
    expect(threadRuntime("codex", paired)).toEqual({
      runtime: "codex",
      state: "unpaired",
    });
    expect(
      threadRuntime("claude_code", { runtime: "api", online: ["claude_code"] }),
    ).toEqual({ runtime: "claude_code", state: "ready" });
    // A new thread takes the workspace's.
    expect(threadRuntime(undefined, paired)).toEqual({
      runtime: "claude_code",
      state: "stopped",
    });
    // Unread (hosted, or not the owner): never said to be offline.
    expect(threadRuntime("claude_code", undefined)).toEqual({
      runtime: "claude_code",
      state: "ready",
    });
    expect(threadRuntime(undefined, undefined)).toEqual({
      runtime: "api",
      state: "ready",
    });
  });

  it("names the helper's package as the backend and the helper pin it", () => {
    const go = readFileSync("backend/agent/host.go", "utf8");
    expect(go).toContain(`const HelperPackage = "${HELPER_PACKAGE}"`);
    const pkg = JSON.parse(readFileSync("agent-host/package.json", "utf8"));
    expect(HELPER_PACKAGE).toBe(`${pkg.name}@${pkg.version}`);
    expect(HELPER_COMMANDS).toEqual({
      run: `npx -y ${HELPER_PACKAGE} run`,
      install: `npx -y ${HELPER_PACKAGE} install`,
    });
  });
});

describe("stepLabel", () => {
  const prepare = (output: unknown) => ({
    id: "t",
    name: "helix_foundry_prepare_operation",
    input: {},
    result: { kind: "returned" as const, output, truncated: false },
  });
  it("says a prepare that ran at once ran", () => {
    expect(stepLabel(prepare({ status: "ran" }))).toBe("Ran an action");
  });
  it("keeps the tool's label for a prepare left as a card, and any other step", () => {
    expect(stepLabel(prepare({ status: "awaiting_person" }))).toBe(
      "Prepared an action",
    );
    expect(stepLabel({ id: "t", name: "helix_foundry_ask", input: {} })).toBe(
      "Asked the Analyst",
    );
    expect(
      stepLabel({ id: "t", name: "helix_foundry_unknown", input: {} }),
    ).toBe("helix_foundry_unknown");
  });
});
