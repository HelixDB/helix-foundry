import { describe, expect, it, vi } from "vitest";
import type {
  AutomationNode,
  AutomationSpec,
  FoundryAutomation,
} from "@/shared/wfe";
import { ApiError } from "@/web/api";
import {
  BRIEF_MAX,
  LIMITS,
  NO_NAMES,
  TEMPLATES,
  actionText,
  brief,
  edgeSides,
  longText,
  placeNodes,
  placesOf,
  addStep,
  automationRefusal,
  automationsUnavailable,
  blankConfig,
  canonicalJson,
  cannotConnect,
  conditionText,
  connect,
  disconnect,
  eventText,
  freeBranch,
  incomplete,
  impactText,
  isLiveRun,
  runImpact,
  assessing,
  listTriggerText,
  moveNode,
  newNodeId,
  nextFire,
  nodeText,
  parseEvent,
  parseOutput,
  parseSpec,
  reaches,
  removeSelection,
  removeStep,
  runUsd,
  sameSteps,
  scheduleText,
  sentence,
  setConfig,
  stepStates,
  stoppedText,
  tidy,
  triggerOfKind,
  triggerText,
  usdText,
  validZone,
  here,
  type Names,
} from "@/web/areas/automations/model";
import {
  ACTIVE_POLL_MS,
  automationQueries,
} from "@/web/areas/automations/queries";
import { automationPaths, openAutomation } from "@/web/areas/automations/paths";

// An automation's spec as the site reads it (src/web/areas/automations/
// model.ts), a port of the backend's package automation: what ParseSpec
// takes and refuses with its words, the canonical form, schedules, the
// steps in words, the templates, the builder's edits, and what a run's
// steps gave, read without trust.

const at = { x: 0, y: 0 };
const node = (
  id: string,
  kind: AutomationNode["kind"],
  config: unknown,
  position = at,
) => ({ id, kind, config, position }) as AutomationNode;
const spec = (
  nodes: AutomationNode[],
  edges: AutomationSpec["edges"] = [],
): AutomationSpec => ({ v: 1, nodes, edges });
const manual = node("start", "trigger", { kind: "manual" });
const ask = (instruction = "Summarise.", allow?: unknown[]) =>
  node("ask", "assistant", { instruction, ...(allow && { allow }) });
const tell = node("tell", "notify", { message: "Done" });
const simple = spec([manual, tell], [{ from: "start", to: "tell" }]);
const read = (s: unknown) => parseSpec(JSON.stringify(s));
const error = (s: unknown) => {
  const r = read(s);
  return r.ok ? "" : r.error;
};
const names: Names = {
  connector: (id) => (id === "c1" ? "Stripe" : undefined),
  ontology: (id) => (id === "o1" ? "Sales" : undefined),
  set: (id) => (id === "s1" ? "General" : undefined),
  dashboard: (id) => (id === "d1" ? "Revenue" : undefined),
};

describe("parseSpec", () => {
  it("reads a spec, trimmed and canonical, in the order a run takes it", () => {
    const r = read(
      spec(
        [
          manual,
          node("b", "notify", { message: " b " }),
          node("a", "assistant", { instruction: " hi ", allow: [] }),
        ],
        [
          { from: "start", to: "b" },
          { from: "start", to: "a" },
        ],
      ),
    );
    expect(r.ok && r.order).toEqual(["start", "b", "a"]);
    expect(r.ok && r.spec.nodes[1]!.config).toEqual({ message: "b" });
    expect(r.ok && r.spec.nodes[2]!.config).toEqual({ instruction: "hi" });
    // An object reads as its JSON does.
    expect(parseSpec(simple).ok).toBe(true);
  });

  it("orders a node after every node with an edge into it", () => {
    const r = read(
      spec(
        [manual, node("late", "notify", { message: "" }), ask(), tell],
        [
          { from: "start", to: "ask" },
          { from: "ask", to: "tell" },
          { from: "tell", to: "late" },
          { from: "start", to: "late" },
        ],
      ),
    );
    expect(r.ok && r.order).toEqual(["start", "ask", "tell", "late"]);
  });

  it("refuses what isn't a spec", () => {
    expect(parseSpec("nope")).toEqual({
      ok: false,
      error: "the spec is not {v, nodes, edges}: not JSON",
    });
    expect(error({ ...simple, extra: 1 })).toMatch(
      /^the spec is not \{v, nodes, edges\}/,
    );
    expect(error({ ...simple, v: 2 })).toBe(
      "the spec's v is 2; this Foundry reads v 1",
    );
    expect(error({ v: 1 })).toBe(
      "a workflow has a trigger and at least one step, at most 50 nodes in all, not 0",
    );
    const many = Array.from({ length: 50 }, (_, i) =>
      node(`n${i}`, "notify", { message: "" }),
    );
    expect(error(spec([manual, ...many]))).toMatch(/not 51$/);
    const edges = Array.from({ length: 101 }, () => ({
      from: "start",
      to: "tell",
    }));
    expect(error(spec([manual, tell], edges))).toBe(
      "a workflow has at most 100 edges, not 101",
    );
    expect(parseSpec("x".repeat(LIMITS.specBytes + 1))).toEqual({
      ok: false,
      error: "a spec is at most 64 KiB",
    });
  });

  it("refuses a node that doesn't read, naming it", () => {
    expect(
      error(spec([manual, node("Bad", "notify", { message: "" })])),
    ).toMatch(/^node id "Bad" must be 1 to 32/);
    expect(error(spec([manual, { ...tell, id: "start" }]))).toBe(
      "two nodes have the id start",
    );
    expect(error(spec([manual, node("x", "wait" as never, {})]))).toBe(
      `node x's kind is "wait"; it is trigger, assistant, action, condition or notify`,
    );
    expect(
      error(
        spec([manual, node("x", "notify", { message: "" }, { x: 2e6, y: 0 })]),
      ),
    ).toBe("node x's position is off the canvas");
    expect(
      error(spec([manual, node("x", "notify", { message: "", to: 1 })])),
    ).toBe("node x: a notify step is {message}");
    expect(
      error(spec([manual, node("x", "notify", { message: "a".repeat(501) })])),
    ).toBe(
      "node x: its message is 0 to 500 characters, with no control characters",
    );
    const r = parseSpec(
      JSON.stringify(spec([manual, node("x", "notify", { message: 3 })])),
      (id) => `“${id}”`,
    );
    expect(r).toEqual({
      ok: false,
      error: "“x”: a notify step is {message}",
      nodeId: "x",
    });
  });

  it("reads every trigger, and refuses one that isn't its kind's", () => {
    const ok = (t: object) =>
      read(
        spec(
          [node("start", "trigger", t), tell],
          [{ from: "start", to: "tell" }],
        ),
      ).ok;
    const why = (t: object) =>
      error(
        spec(
          [node("start", "trigger", t), tell],
          [{ from: "start", to: "tell" }],
        ),
      );
    expect(
      ok({ kind: "schedule", every: "hour", minute: 59, time_zone: "UTC" }),
    ).toBe(true);
    expect(
      ok({
        kind: "schedule",
        every: "day",
        at: "23:59",
        time_zone: "Europe/London",
      }),
    ).toBe(true);
    expect(
      ok({
        kind: "schedule",
        every: "week",
        weekday: "sunday",
        at: "00:00",
        time_zone: "Asia/Tokyo",
      }),
    ).toBe(true);
    expect(
      ok({ kind: "sync_finished", connector_id: "c1", outcome: "any" }),
    ).toBe(true);
    expect(ok({ kind: "load_ready", ontology_id: "o1" })).toBe(true);
    expect(ok({ kind: "document_arrived", set_id: "s1" })).toBe(true);
    expect(
      ok({ kind: "generation_finished", ontology_id: "o1", outcome: "failed" }),
    ).toBe(true);
    expect(
      why({ kind: "schedule", every: "hour", minute: 60, time_zone: "UTC" }),
    ).toMatch(/^node start: a schedule trigger is/);
    expect(
      why({ kind: "schedule", every: "day", at: "24:00", time_zone: "UTC" }),
    ).toMatch(/a schedule trigger is/);
    expect(
      why({
        kind: "schedule",
        every: "day",
        at: "09:00",
        weekday: "monday",
        time_zone: "UTC",
      }),
    ).toMatch(/a schedule trigger is/);
    expect(
      why({ kind: "schedule", every: "day", at: "09:00", time_zone: "Local" }),
    ).toBe(
      `node start: time_zone "Local" is not an IANA time zone, such as Europe/London`,
    );
    expect(why({ kind: "sync_finished", connector_id: "c1" })).toMatch(
      /a sync_finished trigger is/,
    );
    expect(why({ kind: "manual", set_id: "s1" })).toMatch(
      /a manual trigger is/,
    );
    expect(why({ kind: "cron" })).toMatch(/a trigger's kind is "cron"/);
    expect(why({})).toBe("node start: a trigger: its config names its kind");
    expect(error(spec([node("start", "trigger", 3), tell]))).toBe(
      "node start: a trigger: its config is a JSON object",
    );
  });

  it("reads Founder steps, actions and conditions as the backend does", () => {
    const one = (n: AutomationNode, from: AutomationNode = manual) =>
      error(
        spec(
          from === manual ? [manual, n] : [manual, from, n],
          from === manual
            ? [{ from: "start", to: n.id }]
            : [
                { from: "start", to: from.id },
                { from: from.id, to: n.id },
              ],
        ),
      );
    expect(one(ask(" "))).toBe(
      "node ask: its instruction is 1 to 2000 characters, with no control characters",
    );
    expect(one(ask("a\u0007b"))).toMatch(/no control characters/);
    expect(one(ask("a\nb\tc"))).toBe("");
    expect(
      one(
        ask(
          "x",
          Array.from({ length: 11 }, (_, i) => ({
            operation: "sync_connector",
            connector_id: `c${i}`,
          })),
        ),
      ),
    ).toBe("node ask: a Founder step runs at most 10 actions unasked, not 11");
    const sync = { operation: "sync_connector", connector_id: "c1" };
    expect(one(ask("x", [sync, sync]))).toBe(
      "node ask: allow names sync_connector on the same target twice",
    );
    expect(
      one(ask("x", [{ operation: "delete_ontology", ontology_id: "o" }])),
    ).toMatch(/^node ask: allow: an action is/);
    expect(
      one(
        node("do", "action", { operation: "create_load", ontology_id: "o1" }),
      ),
    ).toBe("");
    expect(
      one(
        node("do", "action", { operation: "create_load", connector_id: "o1" }),
      ),
    ).toMatch(/^node do: an action is/);
    expect(
      one(
        node("do", "action", {
          operation: "publish_dashboard",
          dashboard_id: "d 1",
        }),
      ),
    ).toMatch(/an action is/);
    const yes = node("if", "condition", { check: "answer", equals: "yes" });
    const most = node("if", "condition", {
      check: "count",
      op: "at_most",
      value: 0,
    });
    expect(one(yes, ask())).toBe("");
    expect(one(most, ask())).toBe("");
    expect(
      one(
        node("if", "condition", { check: "count", op: "at_most", value: -1 }),
        ask(),
      ),
    ).toMatch(/^node if: a condition is/);
    expect(
      one(node("if", "condition", { check: "answer", equals: "maybe" }), ask()),
    ).toMatch(/a condition is/);
    expect(one(yes)).toBe(
      "node if can't check node start: an answer is a Founder step's",
    );
    expect(one(most)).toBe(
      "node if can't check node start: a count is a Founder step's, or a document_arrived or sync_finished trigger's",
    );
    // A count follows the documents that arrived.
    const arrived = node("start", "trigger", {
      kind: "document_arrived",
      set_id: "s1",
    });
    expect(error(spec([arrived, most], [{ from: "start", to: "if" }]))).toBe(
      "",
    );
  });

  it("refuses a graph that isn't one trigger with every step reached forward", () => {
    expect(error(spec([tell, ask()], [{ from: "tell", to: "ask" }]))).toBe(
      "a workflow has exactly one trigger, not 0",
    );
    expect(error(spec([manual, { ...manual, id: "again" }]))).toBe(
      "a workflow has exactly one trigger, not 2",
    );
    expect(error(spec([manual, tell], [{ from: "x", to: "tell" }]))).toBe(
      `an edge comes from "x", which is no node`,
    );
    expect(error(spec([manual, tell], [{ from: "start", to: "x" }]))).toBe(
      `an edge goes to "x", which is no node`,
    );
    expect(error(spec([manual, tell], [{ from: "tell", to: "tell" }]))).toBe(
      "node tell has an edge to itself",
    );
    expect(error(spec([manual, tell], [{ from: "tell", to: "start" }]))).toBe(
      "the trigger start starts the workflow: no edge goes into it",
    );
    expect(
      error(
        spec(
          [manual, tell],
          [
            { from: "start", to: "tell" },
            { from: "start", to: "tell" },
          ],
        ),
      ),
    ).toBe("two edges go from node start to node tell");
    expect(
      error(
        spec([manual, tell], [{ from: "start", to: "tell", branch: "true" }]),
      ),
    ).toBe(
      "the edge from node start to node tell has a branch, which only a condition's edges have",
    );
    const yes = node("if", "condition", { check: "answer", equals: "yes" });
    const two = node("two", "notify", { message: "" });
    const base = [
      { from: "start", to: "ask" },
      { from: "ask", to: "if" },
    ];
    expect(
      error(
        spec([manual, ask(), yes, tell], [...base, { from: "if", to: "tell" }]),
      ),
    ).toBe(
      "the edge from node if to node tell is its true or its false branch",
    );
    expect(
      error(
        spec(
          [manual, ask(), yes, tell, two],
          [
            ...base,
            { from: "if", to: "tell", branch: "true" },
            { from: "if", to: "two", branch: "true" },
          ],
        ),
      ),
    ).toBe("node if has 2 true branches; it has at most one");
    expect(
      error(
        spec(
          [manual, ask(), yes, tell],
          [
            ...base,
            { from: "start", to: "if" },
            { from: "if", to: "tell", branch: "false" },
          ],
        ),
      ),
    ).toBe(
      "node if checks the one step before it, so exactly one edge goes into it, not 2",
    );
    expect(
      error(
        spec(
          [manual, ask(), tell],
          [
            { from: "start", to: "ask" },
            { from: "ask", to: "tell" },
            { from: "tell", to: "ask" },
          ],
        ),
      ),
    ).toBe("node ask is in a cycle: a workflow's edges go one way");
    expect(error(spec([manual, tell]))).toBe(
      "node tell isn't reached from the trigger: connect it",
    );
    // Nothing waits on the work its own steps start: it would never stop.
    const synced = node("start", "trigger", {
      kind: "sync_finished",
      connector_id: "c1",
      outcome: "any",
    });
    const sync = node("sync", "action", {
      operation: "sync_connector",
      connector_id: "c1",
    });
    expect(error(spec([synced, sync], [{ from: "start", to: "sync" }]))).toBe(
      "node sync syncs the connector its trigger waits on, so each run would start another: start it on a schedule instead",
    );
    const other = node("sync", "action", {
      operation: "sync_connector",
      connector_id: "c2",
    });
    expect(error(spec([synced, other], [{ from: "start", to: "sync" }]))).toBe(
      "",
    );
    const loaded = node("start", "trigger", {
      kind: "load_ready",
      ontology_id: "o1",
    });
    expect(
      error(
        spec(
          [
            loaded,
            ask("Load it", [{ operation: "create_load", ontology_id: "o1" }]),
          ],
          [{ from: "start", to: "ask" }],
        ),
      ),
    ).toBe(
      "node ask loads the ontology its trigger waits on, so each run would start another: start it on a schedule instead",
    );
  });

  it("reads every template, its targets chosen, and says what is missing", () => {
    for (const t of TEMPLATES) {
      const filled = t.build({
        timeZone: "Europe/London",
        connector: { id: "c1", name: "Stripe" },
        ontology: { id: "o1", name: "Sales" },
        set: { id: "s1", name: "General" },
      });
      expect(
        filled.spec.nodes.every((n) => !incomplete(n)) || t.id === "blank",
        t.id,
      ).toBe(true);
      if (t.id !== "blank") expect(read(filled.spec).ok, t.id).toBe(true);
      const empty = t.build({ timeZone: "UTC" });
      const missing = empty.spec.nodes.map(incomplete).filter(Boolean);
      expect(missing.length, t.id).toBe(
        {
          "weekly-summary": 0,
          "sync-and-summarise": 1,
          "load-after-sync": 2,
          "check-documents": 1,
          "load-ready-report": 1,
          blank: 0,
        }[t.id],
      );
    }
  });
});

describe("canonicalJson and sameSteps", () => {
  it("writes the spec as the backend does: its fields in its order", () => {
    const s = spec(
      [
        node("start", "trigger", {
          time_zone: "UTC",
          at: "09:00",
          weekday: "monday",
          every: "week",
          kind: "schedule",
        }),
        node("ask", "assistant", {
          allow: [{ connector_id: "c1", operation: "sync_connector" }],
          instruction: "x",
        }),
        node("if", "condition", { value: 2, op: "equals", check: "count" }),
        node("tell", "notify", { message: "" }),
      ],
      [
        { to: "ask", from: "start" } as AutomationSpec["edges"][number],
        { from: "ask", to: "if" },
        { from: "if", to: "tell", branch: "true" },
      ],
    );
    expect(canonicalJson(s)).toBe(
      JSON.stringify({
        v: 1,
        nodes: [
          {
            id: "start",
            kind: "trigger",
            config: {
              kind: "schedule",
              every: "week",
              weekday: "monday",
              at: "09:00",
              time_zone: "UTC",
            },
            position: at,
          },
          {
            id: "ask",
            kind: "assistant",
            config: {
              instruction: "x",
              allow: [{ operation: "sync_connector", connector_id: "c1" }],
            },
            position: at,
          },
          {
            id: "if",
            kind: "condition",
            config: { check: "count", op: "equals", value: 2 },
            position: at,
          },
          { id: "tell", kind: "notify", config: { message: "" }, position: at },
        ],
        edges: [
          { from: "start", to: "ask" },
          { from: "ask", to: "if" },
          { from: "if", to: "tell", branch: "true" },
        ],
      }),
    );
  });

  it("writes an action step and the empty names as they are", () => {
    const act = spec(
      [
        manual,
        node("do", "action", {
          dashboard_id: "d1",
          operation: "publish_dashboard",
        }),
      ],
      [{ from: "start", to: "do" }],
    );
    expect(JSON.parse(canonicalJson(act)).nodes[1].config).toEqual({
      operation: "publish_dashboard",
      dashboard_id: "d1",
    });
    expect([
      NO_NAMES.connector("c"),
      NO_NAMES.ontology("o"),
      NO_NAMES.set("s"),
      NO_NAMES.dashboard("d"),
    ]).toEqual([undefined, undefined, undefined, undefined]);
  });

  it("sees a move as no change to the steps, and anything else as one", () => {
    expect(sameSteps(simple, moveNode(simple, "tell", { x: 300, y: 40 }))).toBe(
      true,
    );
    expect(
      sameSteps(
        simple,
        setConfig(simple, "tell", {
          kind: "notify",
          config: { message: "Other" },
        }),
      ),
    ).toBe(false);
  });
});

describe("schedules", () => {
  const day = {
    kind: "schedule",
    every: "day",
    at: "09:00",
    time_zone: "UTC",
  } as const;
  it("fires a day's time strictly after now, the next day once it passed", () => {
    expect(nextFire(day, Date.parse("2026-11-16T08:59:00Z"))).toBe(
      Date.parse("2026-11-16T09:00:00Z"),
    );
    expect(nextFire(day, Date.parse("2026-11-16T09:00:00Z"))).toBe(
      Date.parse("2026-11-17T09:00:00Z"),
    );
    // Across a month's end.
    expect(nextFire(day, Date.parse("2026-11-30T10:00:00Z"))).toBe(
      Date.parse("2026-12-01T09:00:00Z"),
    );
  });
  it("fires an hour's minute, and a week's day", () => {
    const hour = {
      kind: "schedule",
      every: "hour",
      minute: 15,
      time_zone: "UTC",
    } as const;
    expect(nextFire(hour, Date.parse("2026-11-16T08:15:00Z"))).toBe(
      Date.parse("2026-11-16T09:15:00Z"),
    );
    expect(nextFire(hour, Date.parse("2026-11-16T08:14:00Z"))).toBe(
      Date.parse("2026-11-16T08:15:00Z"),
    );
    const week = {
      kind: "schedule",
      every: "week",
      weekday: "monday",
      at: "09:00",
      time_zone: "UTC",
    } as const;
    // 2026-11-15 is a Sunday.
    expect(nextFire(week, Date.parse("2026-11-15T12:00:00Z"))).toBe(
      Date.parse("2026-11-16T09:00:00Z"),
    );
    expect(nextFire(week, Date.parse("2026-11-16T09:00:00Z"))).toBe(
      Date.parse("2026-11-23T09:00:00Z"),
    );
    expect(nextFire(week, Date.parse("2026-11-16T08:00:00Z"))).toBe(
      Date.parse("2026-11-16T09:00:00Z"),
    );
  });
  it("keeps to its zone's wall clock across daylight saving", () => {
    const london = { ...day, time_zone: "Europe/London" };
    // 09:00 in London is 09:00Z in winter and 08:00Z in summer.
    expect(nextFire(london, Date.parse("2026-03-28T10:00:00Z"))).toBe(
      Date.parse("2026-03-29T08:00:00Z"),
    );
    expect(nextFire(london, Date.parse("2026-10-24T09:00:00Z"))).toBe(
      Date.parse("2026-10-25T09:00:00Z"),
    );
    const tokyo = { ...day, time_zone: "Asia/Tokyo" };
    expect(nextFire(tokyo, Date.parse("2026-11-16T00:00:00Z"))).toBe(
      Date.parse("2026-11-17T00:00:00Z"),
    );
  });
  it("knows a time zone the backend takes", () => {
    expect(validZone("Europe/London")).toBe(true);
    expect(validZone("UTC")).toBe(true);
    for (const z of [
      "",
      "Local",
      "Mars/Base",
      "Europe/ London",
      "x".repeat(65),
      "Etc/Unknown",
      undefined,
    ])
      expect(validZone(z), z).toBe(false);
  });
  it("reads the browser's time zone, else UTC", () => {
    const real = Intl.DateTimeFormat;
    const as = (timeZone: string | undefined) =>
      vi.spyOn(Intl, "DateTimeFormat").mockImplementation(function () {
        return {
          resolvedOptions: () => ({ timeZone }),
        } as unknown as Intl.DateTimeFormat;
      });
    try {
      as("Asia/Tokyo");
      expect(here()).toBe("Asia/Tokyo");
      for (const z of ["Etc/Unknown", undefined, ""]) {
        vi.restoreAllMocks();
        as(z);
        expect(here(), String(z)).toBe("UTC");
      }
    } finally {
      vi.restoreAllMocks();
      expect(Intl.DateTimeFormat).toBe(real);
    }
  });
});

describe("words", () => {
  it("says each trigger, action and condition", () => {
    expect(
      scheduleText({
        kind: "schedule",
        every: "hour",
        minute: 0,
        time_zone: "America/New_York",
      }),
    ).toBe("every hour, on the hour");
    expect(
      scheduleText({
        kind: "schedule",
        every: "hour",
        minute: 5,
        time_zone: "UTC",
      }),
    ).toBe("every hour at 5 past");
    // An hour's minute names a zone only where it isn't whole hours off.
    expect(
      scheduleText({
        kind: "schedule",
        every: "hour",
        minute: 5,
        time_zone: "Asia/Kolkata",
      }),
    ).toBe("every hour at 5 past, Kolkata time");
    expect(
      scheduleText({
        kind: "schedule",
        every: "day",
        at: "07:30",
        time_zone: "UTC",
      }),
    ).toBe("every day at 07:30 UTC");
    expect(
      scheduleText({
        kind: "schedule",
        every: "week",
        weekday: "monday",
        at: "08:00",
        time_zone: "America/New_York",
      }),
    ).toBe("every Monday at 08:00 New York time");
    expect(
      triggerText(
        { kind: "sync_finished", connector_id: "c1", outcome: "failed" },
        names,
      ),
    ).toBe("a sync of “Stripe” fails");
    expect(
      triggerText(
        { kind: "sync_finished", connector_id: "x", outcome: "any" },
        names,
      ),
    ).toBe("a sync of a connection ends");
    expect(triggerText({ kind: "load_ready", ontology_id: "o1" }, names)).toBe(
      "a load of “Sales” is ready",
    );
    expect(triggerText({ kind: "document_arrived", set_id: "s1" }, names)).toBe(
      "documents arrive in “General”",
    );
    expect(
      triggerText(
        { kind: "generation_finished", ontology_id: "z", outcome: "succeeded" },
        names,
      ),
    ).toBe("a draft of an ontology succeeds");
    expect(triggerText({ kind: "manual" }, names)).toBe("you run it");
    expect(
      actionText({ operation: "sync_connector", connector_id: "c1" }, names),
    ).toBe("sync “Stripe”");
    expect(
      actionText({ operation: "create_load", ontology_id: "o1" }, names),
    ).toBe("load the current version of “Sales”");
    expect(
      actionText({ operation: "publish_dashboard", dashboard_id: "x" }, names),
    ).toBe("refresh the public snapshot of a dashboard");
    expect(conditionText({ check: "answer", equals: "no" })).toBe(
      "the answer is no",
    );
    expect(conditionText({ check: "count", op: "equals", value: 3 })).toBe(
      "the count is exactly 3",
    );
    expect(conditionText({ check: "count", op: "at_least", value: 1 })).toBe(
      "the count is at least 1",
    );
    expect(listTriggerText("schedule", "", names)).toBe("On a schedule");
    expect(listTriggerText("sync_finished", "c1", names)).toBe(
      "After syncs of “Stripe”",
    );
    expect(listTriggerText("load_ready", "o1", names)).toBe(
      "When a load of “Sales” is ready",
    );
    expect(listTriggerText("document_arrived", "s1", names)).toBe(
      "When documents arrive in “General”",
    );
    expect(listTriggerText("generation_finished", "o1", names)).toBe(
      "After drafts of “Sales”",
    );
    expect(listTriggerText("manual", undefined, names)).toBe("Run by hand");
  });

  it("says a notify step as this Foundry sends it", () => {
    const quiet = node("tell", "notify", { message: "" });
    expect(nodeText(tell, NO_NAMES, true)).toBe("email you “Done”");
    expect(nodeText(ask("  "), NO_NAMES, true)).toBe("ask Founder");
    expect(nodeText(ask("Sum up."), NO_NAMES, true)).toBe(
      "ask Founder: “Sum up.”",
    );
    expect(nodeText(quiet, NO_NAMES, true)).toBe("email you what it found");
    expect(nodeText(quiet, NO_NAMES, false)).toBe(
      "note what it found in the run (this Foundry sends no email)",
    );
    expect(nodeText(manual, NO_NAMES, true)).toBe("only when you run it");
  });

  it("reads the automation as one sentence", () => {
    const weekly = TEMPLATES.find((t) => t.id === "sync-and-summarise")!.build({
      timeZone: "Europe/London",
      connector: { id: "c1", name: "Stripe" },
    });
    expect(sentence(weekly.spec, names, true)).toBe(
      "Every day at 08:00 London time, sync “Stripe”, then ask Founder: “Summarise anything that needs attention today, such as overdue invoices or…”, then email you “This morning’s summary”.",
    );
    const check = TEMPLATES.find((t) => t.id === "check-documents")!.build({
      timeZone: "UTC",
      set: { id: "s1", name: "General" },
    }).spec;
    const both = {
      ...check,
      nodes: [...check.nodes, node("other", "notify", { message: "" })],
      edges: [
        ...check.edges,
        { from: "needs", to: "other", branch: "false" as const },
      ],
    };
    expect(sentence(both, names, true)).toMatch(
      /^When documents arrive in “General”, ask Founder: .*, then if the answer is yes, email you “A new document needs your attention”, then otherwise email you what it found\.$/,
    );
    expect(sentence(simple, names, false)).toBe(
      "When you run it, note what it found in the run (this Foundry sends no email).",
    );
  });

  it("cuts a long text to its first sentence, else 80 characters and an ellipsis", () => {
    // Whitespace collapses; a short text is as it is.
    expect(brief("  Short \n\t one  ")).toBe("Short one");
    expect(brief("")).toBe("");
    // The first sentence, when it fits, ends at . ! ? or … before a space
    // or the end, with any closing quote or bracket.
    expect(brief("Summarise the week. Then list anything unusual.")).toBe(
      "Summarise the week.",
    );
    expect(brief("Is it late? Say so.")).toBe("Is it late?");
    expect(brief("Wait… then go.")).toBe("Wait…");
    expect(brief("He said “stop.” Then left.")).toBe("He said “stop.”");
    expect(brief("Report (in brief.) Then stop.")).toBe("Report (in brief.)");
    // A stop inside a word or a number ends no sentence.
    expect(brief("Compare v1.2 with node.js today")).toBe(
      "Compare v1.2 with node.js today",
    );
    // Exactly the most fits; one more is cut.
    const most = "x".repeat(BRIEF_MAX);
    expect(brief(most)).toBe(most);
    expect(brief(most + "y")).toBe(most + "…");
    // A first sentence too long is cut too, back to the last space in its
    // last quarter, without trailing punctuation.
    expect(
      brief(
        "Summarise anything that needs attention today, such as overdue invoices or records that changed overnight.",
      ),
    ).toBe(
      "Summarise anything that needs attention today, such as overdue invoices or…",
    );
    expect(brief("a ".repeat(60), 10)).toBe("a a a a a…");
    expect(brief("one, two, three, four", 10)).toBe("one, two…");
    // No space in the last quarter: cut at the most.
    expect(brief("abcdefgh ijklmnopqrstuvwxyz", 12)).toBe("abcdefgh ijk…");
    // Characters are graphemes: an emoji or accented letter is whole.
    const family = "👩‍👩‍👧‍👦";
    expect(brief(family.repeat(10), 3)).toBe(family.repeat(3) + "…");
    expect(brief("e\u0301".repeat(5), 2)).toBe("e\u0301e\u0301…");
    expect(brief("Café, déjà vu! Then more.")).toBe("Café, déjà vu!");
  });

  it("says a step briefly, and knows when its text reads shorter so", () => {
    const long = `Every Monday, review the changes. ${"More detail. ".repeat(40)}`;
    expect(nodeText(ask(long), NO_NAMES, true, "brief")).toBe(
      "ask Founder: “Every Monday, review the changes.”",
    );
    expect(nodeText(ask(long), NO_NAMES, true)).toBe(`ask Founder: “${long}”`);
    const subject = node("tell", "notify", { message: "w".repeat(120) });
    expect(nodeText(subject, NO_NAMES, true, "brief")).toBe(
      `email you “${"w".repeat(BRIEF_MAX)}…”`,
    );
    expect(
      nodeText(node("tell", "notify", { message: "  " }), NO_NAMES, true),
    ).toBe("email you what it found");
    expect(longText(ask(long))).toBe(long.trim());
    expect(longText(subject)).toBe("w".repeat(120));
    expect(longText(ask("Sum up."))).toBeUndefined();
    expect(longText(ask("   "))).toBeUndefined();
    expect(longText(tell)).toBeUndefined();
    expect(longText(manual)).toBeUndefined();
    // The sentence quotes each briefly: never the whole instruction.
    const weekly = spec(
      [
        node("start", "trigger", {
          kind: "schedule",
          every: "week",
          weekday: "monday",
          at: "09:00",
          time_zone: "Europe/London",
        }),
        ask(long),
        node("tell", "notify", { message: "Weekly changes" }),
      ],
      [
        { from: "start", to: "ask" },
        { from: "ask", to: "tell" },
      ],
    );
    expect(sentence(weekly, NO_NAMES, true)).toBe(
      "Every Monday at 09:00 London time, ask Founder: “Every Monday, review the changes.”, then email you “Weekly changes”.",
    );
  });

  it("places steps anew, and joins them by the sides they face", () => {
    const placed = placeNodes(simple, {
      tell: { x: 300.4, y: 2e6 },
      gone: { x: 1, y: 1 },
    });
    expect(placed.nodes.map((n) => n.id)).toEqual(["start", "tell"]);
    expect(placed.nodes[0]).toBe(simple.nodes[0]);
    expect(placed.nodes[1]!.position).toEqual({ x: 300, y: 1e6 });
    expect(sameSteps(simple, placed)).toBe(true);
    expect(placesOf(placed)).toEqual({
      start: simple.nodes[0]!.position,
      tell: { x: 300, y: 1e6 },
    });
    const card = (x: number, y: number) => ({ x, y, width: 248, height: 80 });
    // Side by side: straight across, either way.
    expect(edgeSides(card(0, 0), card(320, 10))).toEqual({
      source: "right",
      target: "left",
    });
    expect(edgeSides(card(320, 0), card(0, 30))).toEqual({
      source: "left",
      target: "right",
    });
    // As tidy() lays them out, a branch's two steps too: down.
    expect(edgeSides(card(0, 0), card(0, 150))).toEqual({
      source: "bottom",
      target: "top",
    });
    expect(edgeSides(card(0, 0), card(140, 150))).toEqual({
      source: "bottom",
      target: "top",
    });
    expect(edgeSides(card(0, 300), card(-140, 0))).toEqual({
      source: "top",
      target: "bottom",
    });
    // Apart both ways: by the wider gap.
    expect(edgeSides(card(0, 0), card(600, 200))).toEqual({
      source: "right",
      target: "left",
    });
    expect(edgeSides(card(0, 0), card(260, 600))).toEqual({
      source: "bottom",
      target: "top",
    });
    // Overlapping: down.
    expect(edgeSides(card(0, 0), card(10, 10))).toEqual({
      source: "bottom",
      target: "top",
    });
  });

  it("says what a step still needs", () => {
    expect(incomplete(manual)).toBeUndefined();
    expect(
      incomplete(
        node("t", "trigger", {
          kind: "sync_finished",
          connector_id: "",
          outcome: "any",
        }),
      ),
    ).toBe("Choose a connection");
    expect(
      incomplete(node("t", "trigger", { kind: "load_ready", ontology_id: "" })),
    ).toBe("Choose an ontology");
    expect(
      incomplete(
        node("t", "trigger", { kind: "document_arrived", set_id: "" }),
      ),
    ).toBe("Choose a document set");
    expect(incomplete(ask(""))).toBe("Write what Founder should do");
    expect(
      incomplete(ask("x", [{ operation: "create_load", ontology_id: "" }])),
    ).toBe("Choose what each allowed action acts on");
    expect(
      incomplete(
        node("a", "action", { operation: "sync_connector", connector_id: "" }),
      ),
    ).toBe("Choose a connection");
    expect(
      incomplete(
        node("a", "action", { operation: "create_load", ontology_id: "" }),
      ),
    ).toBe("Choose an ontology");
    expect(
      incomplete(
        node("a", "action", {
          operation: "publish_dashboard",
          dashboard_id: "",
        }),
      ),
    ).toBe("Choose a dashboard");
    expect(incomplete(tell)).toBeUndefined();
  });
});

describe("edits", () => {
  const withAsk = spec([manual, ask()], [{ from: "start", to: "ask" }]);
  it("adds a step after another, a condition's along its free branch", () => {
    const one = addStep(withAsk, "condition", "ask");
    expect(one.id).toBe("condition-1");
    expect(one.spec.edges).toContainEqual({ from: "ask", to: "condition-1" });
    const yes = addStep(one.spec, "notify", "condition-1"),
      no = addStep(yes.spec, "notify", "condition-1");
    expect(no.spec.edges).toContainEqual({
      from: "condition-1",
      to: "notify-1",
      branch: "true",
    });
    expect(no.spec.edges).toContainEqual({
      from: "condition-1",
      to: "notify-2",
      branch: "false",
    });
    expect(freeBranch(no.spec, "condition-1")).toBeUndefined();
    // A third step after a full condition isn't joined.
    const third = addStep(no.spec, "notify", "condition-1");
    expect(third.spec.edges).toHaveLength(no.spec.edges.length);
    expect(read(no.spec).ok).toBe(true);
    // With none chosen, under the lowest step, alone.
    const alone = addStep(withAsk, "action");
    expect(alone.spec.edges).toEqual(withAsk.edges);
    expect(alone.spec.nodes.at(-1)!.position.y).toBe(150);
    expect(blankConfig("assistant")).toEqual({
      kind: "assistant",
      config: { instruction: "" },
    });
    expect(blankConfig("action")).toEqual({
      kind: "action",
      config: { operation: "sync_connector", connector_id: "" },
    });
  });

  it("slots a step in between a step and the steps it led to", () => {
    const row = (id: string, y: number) => ({
      ...node(id, "notify", { message: "" }),
      position: { x: 0, y },
    });
    const chain = spec(
      [
        { ...manual, position: { x: 0, y: 0 } },
        { ...ask(), position: { x: 0, y: 150 } },
        row("tell", 300),
        row("last", 450),
      ],
      [
        { from: "start", to: "ask" },
        { from: "ask", to: "tell" },
        { from: "tell", to: "last" },
      ],
    );
    const added = addStep(chain, "assistant", "ask");
    expect(added.spec.edges).toEqual([
      { from: "start", to: "ask" },
      { from: "tell", to: "last" },
      { from: "ask", to: "assistant-1" },
      { from: "assistant-1", to: "tell" },
    ]);
    const y = (id: string) =>
      added.spec.nodes.find((n) => n.id === id)!.position;
    expect(y("assistant-1")).toEqual({ x: 0, y: 300 });
    // What came after moves down a row; what came before stays.
    expect(y("tell").y).toBe(450);
    expect(y("last").y).toBe(600);
    expect(y("ask").y).toBe(150);
    // Blank, it can't be read yet: its sentence goes top down.
    expect(sentence(added.spec, NO_NAMES, false)).toBe(
      "When you run it, ask Founder: “Summarise.”, then ask Founder, then note what it found in the run (this Foundry sends no email), then note what it found in the run (this Foundry sends no email).",
    );
    // After the last step, nothing to take over: joined after it.
    const end = addStep(chain, "notify", "last");
    expect(end.spec.edges).toContainEqual({ from: "last", to: "notify-1" });
    expect(end.spec.nodes.filter((n) => n.id !== "notify-1")).toEqual(
      chain.nodes,
    );
    // A new condition takes the single join as its true branch.
    const cond = addStep(chain, "condition", "ask");
    expect(cond.spec.edges).toContainEqual({
      from: "condition-1",
      to: "tell",
      branch: "true",
    });
    expect(read(cond.spec).ok).toBe(true);
    // A join into a condition stays: the condition checks the step before it.
    const yes = node("if", "condition", { check: "answer", equals: "yes" });
    const checked = spec(
      [manual, ask(), yes, tell],
      [
        { from: "start", to: "ask" },
        { from: "ask", to: "if" },
        { from: "if", to: "tell", branch: "true" },
      ],
    );
    const beside = addStep(checked, "notify", "ask");
    expect(beside.spec.edges).toContainEqual({ from: "ask", to: "if" });
    expect(beside.spec.edges).toContainEqual({ from: "ask", to: "notify-1" });
    expect(beside.spec.nodes.find((n) => n.id === "if")).toEqual(
      checked.nodes[2],
    );
    // Fanning out: the new step takes over every join, and leads to each.
    const fan = spec(
      [manual, ask(), tell, node("two", "notify", { message: "" })],
      [
        { from: "start", to: "ask" },
        { from: "ask", to: "tell" },
        { from: "ask", to: "two" },
      ],
    );
    const mid = addStep(fan, "action", "ask");
    expect(mid.spec.edges).toEqual([
      { from: "start", to: "ask" },
      { from: "ask", to: "action-1" },
      { from: "action-1", to: "tell" },
      { from: "action-1", to: "two" },
    ]);
    // A new condition doesn't split a fan into true and false.
    const split = addStep(fan, "condition", "ask");
    expect(split.spec.edges).toHaveLength(fan.edges.length + 1);
    expect(split.spec.edges).toContainEqual({ from: "ask", to: "tell" });
  });

  it("removes a step, joining what came before it to what came after", () => {
    const chain = spec(
      [manual, ask(), tell],
      [
        { from: "start", to: "ask" },
        { from: "ask", to: "tell" },
      ],
    );
    expect(removeStep(chain, "ask")).toEqual(
      spec([manual, tell], [{ from: "start", to: "tell" }]),
    );
    expect(removeStep(chain, "start")).toBe(chain);
    expect(removeStep(chain, "gone")).toBe(chain);
    // A condition's branch goes on to the step after; one branch a step.
    const yes = node("if", "condition", { check: "answer", equals: "yes" });
    const branched = spec(
      [manual, ask(), yes, tell, node("two", "notify", { message: "" })],
      [
        { from: "start", to: "ask" },
        { from: "ask", to: "if" },
        { from: "if", to: "tell", branch: "true" },
        { from: "tell", to: "two" },
      ],
    );
    expect(removeStep(branched, "tell").edges).toContainEqual({
      from: "if",
      to: "two",
      branch: "true",
    });
  });

  it("removes what a canvas deleted at once, keeping the steps after joined", () => {
    const chain = spec(
      [manual, ask(), tell],
      [
        { from: "start", to: "ask" },
        { from: "ask", to: "tell" },
      ],
    );
    // A deleted step comes with its joins: they are bridged, not cut.
    expect(
      removeSelection(
        chain,
        ["ask"],
        [
          { from: "start", to: "ask" },
          { from: "ask", to: "tell" },
        ],
      ),
    ).toEqual(spec([manual, tell], [{ from: "start", to: "tell" }]));
    // A join deleted alone is cut.
    expect(
      removeSelection(chain, [], [{ from: "ask", to: "tell" }]).edges,
    ).toEqual([{ from: "start", to: "ask" }]);
    // Two steps in a row: the trigger joins what came after both.
    const two = spec(
      [manual, ask(), tell, node("two", "notify", { message: "" })],
      [
        { from: "start", to: "ask" },
        { from: "ask", to: "tell" },
        { from: "tell", to: "two" },
      ],
    );
    expect(removeSelection(two, ["ask", "tell"], two.edges).edges).toEqual([
      { from: "start", to: "two" },
    ]);
  });

  it("joins steps only forward, once, and a condition to one step before it", () => {
    const yes = node("if", "condition", { check: "answer", equals: "yes" });
    const s = spec(
      [manual, ask(), yes, tell],
      [
        { from: "start", to: "ask" },
        { from: "ask", to: "if" },
      ],
    );
    expect(cannotConnect(s, "ask", "start")).toBe(
      "Nothing comes before the trigger",
    );
    expect(cannotConnect(s, "ask", "ask")).toBe("A step can’t lead to itself");
    expect(cannotConnect(s, "start", "ask")).toBe("They are joined already");
    expect(cannotConnect(s, "if", "ask")).toBe(
      "That would make a loop: steps only go forward",
    );
    expect(cannotConnect(s, "start", "if")).toBe(
      "A condition checks exactly one step before it",
    );
    expect(cannotConnect(s, "gone", "if")).toBe("That step is gone");
    expect(cannotConnect(s, "if", "tell")).toBeUndefined();
    const joined = connect(s, "if", "tell");
    expect(joined.edges.at(-1)).toEqual({
      from: "if",
      to: "tell",
      branch: "true",
    });
    expect(connect(joined, "if", "tell")).toBe(joined);
    const chosen = connect(s, "if", "tell", "false");
    expect(chosen.edges.at(-1)).toEqual({
      from: "if",
      to: "tell",
      branch: "false",
    });
    expect(disconnect(joined, "if", "tell").edges).toEqual(s.edges);
    const full = connect(
      connect(s, "if", "tell"),
      "if",
      newNodeId(s, "notify"),
    );
    expect(full).toBeDefined();
    const both = connect(connect(s, "if", "tell"), "if", "ask");
    expect(cannotConnect(both, "if", "tell")).toBe("They are joined already");
    const two = node("two", "notify", { message: "" });
    const full2 = connect(
      connect({ ...s, nodes: [...s.nodes, two] }, "if", "tell"),
      "if",
      "two",
    );
    expect(
      cannotConnect(
        {
          ...full2,
          nodes: [...full2.nodes, node("three", "notify", { message: "" })],
        },
        "if",
        "three",
      ),
    ).toBe("Both of its branches lead somewhere already");
    expect(reaches(s, "start", "if")).toBe(true);
    expect(reaches(s, "if", "start")).toBe(false);
  });

  it("changes a step's config, its place and its trigger's kind", () => {
    expect(
      setConfig(simple, "tell", {
        kind: "assistant",
        config: { instruction: "x" },
      }),
    ).toEqual(simple);
    expect(
      moveNode(simple, "tell", { x: 12.6, y: -3e6 }).nodes[1]!.position,
    ).toEqual({ x: 13, y: -1e6 });
    const sched = {
      kind: "schedule",
      every: "day",
      at: "07:00",
      time_zone: "Asia/Tokyo",
    } as const;
    expect(triggerOfKind("schedule", sched, "UTC")).toEqual({
      kind: "schedule",
      every: "day",
      at: "09:00",
      time_zone: "Asia/Tokyo",
    });
    expect(triggerOfKind("schedule", { kind: "manual" }, "UTC")).toMatchObject({
      time_zone: "UTC",
    });
    for (const k of [
      "sync_finished",
      "load_ready",
      "document_arrived",
      "generation_finished",
      "manual",
    ] as const)
      expect(triggerOfKind(k, sched, "UTC").kind).toBe(k);
    expect(newNodeId(simple, "notify")).toBe("notify-1");
  });
});

describe("tidy", () => {
  const places = (s: AutomationSpec) =>
    Object.fromEntries(s.nodes.map((n) => [n.id, n.position]));

  it("lays each step a row below the furthest step before it, a row's steps side by side and centred", () => {
    const check = node(
      "check",
      "condition",
      { check: "answer", equals: "yes" },
      { x: 50, y: 900 },
    );
    const yes = node("yes", "notify", { message: "Yes" }, { x: -500, y: 3 });
    const no = node("no", "notify", { message: "No" }, { x: 500, y: 3 });
    // "late" comes after "yes" and after "ask": the longer way places it.
    const late = node("late", "notify", { message: "" }, { x: 0, y: 0 });
    const s = spec(
      [manual, ask(), check, yes, no, late],
      [
        { from: "start", to: "ask" },
        { from: "ask", to: "check" },
        { from: "check", to: "yes", branch: "true" },
        { from: "check", to: "no", branch: "false" },
        { from: "yes", to: "late" },
        { from: "ask", to: "late" },
      ],
    );
    const laid = tidy(s);
    expect(places(laid)).toEqual({
      start: { x: 0, y: 0 },
      ask: { x: 0, y: 150 },
      check: { x: 0, y: 300 },
      yes: { x: -140, y: 450 },
      no: { x: 140, y: 450 },
      late: { x: 0, y: 600 },
    });
    // Only places change: it runs the same steps, in the same order.
    expect(sameSteps(s, laid)).toBe(true);
    expect(laid.edges).toEqual(s.edges);
    expect(tidy(laid)).toEqual(laid);
  });

  it("puts steps no way from the trigger reaches in a row of their own, last", () => {
    const loose = node("loose", "notify", { message: "" }, { x: 30, y: -20 });
    const other = node(
      "other",
      "assistant",
      { instruction: "" },
      { x: 10, y: 0 },
    );
    const laid = tidy(
      spec([loose, manual, tell, other], [{ from: "start", to: "tell" }]),
    );
    expect(places(laid)).toEqual({
      start: { x: 0, y: 0 },
      tell: { x: 0, y: 150 },
      other: { x: -140, y: 300 },
      loose: { x: 140, y: 300 },
    });
  });

  it("places a trigger alone, and what a cycle holds last", () => {
    expect(places(tidy(spec([manual])))).toEqual({ start: { x: 0, y: 0 } });
    const a = node("a", "notify", { message: "" }),
      b = node("b", "notify", { message: "" });
    const laid = tidy(
      spec(
        [manual, a, b],
        [
          { from: "start", to: "a" },
          { from: "a", to: "b" },
          { from: "b", to: "a" },
        ],
      ),
    );
    expect(places(laid).start).toEqual({ x: 0, y: 0 });
    // The cycle's steps, which no run could order, go in the last row.
    expect(places(laid).a!.y).toBe(150);
    expect(places(laid).b!.y).toBe(150);
    // With no trigger at all, everything is unreached.
    expect(places(tidy(spec([a])))).toEqual({ a: { x: 0, y: 0 } });
  });
});

describe("runs", () => {
  it("reads a step's output and a run's event without trust", () => {
    expect(
      parseOutput(
        '{"summary":"Done.","answer":true,"count":"x","pending":2,"branch":false,"result":{"id":1}}',
      ),
    ).toEqual({
      summary: "Done.",
      answer: true,
      pending: 2,
      branch: false,
      result: { id: 1 },
    });
    expect(parseOutput("nope")).toEqual({});
    expect(parseOutput("[1]")).toEqual({});
    expect(parseOutput(undefined)).toEqual({});
    expect(
      parseEvent(
        '{"kind":"sync_finished","at":"t","outcome":"failed","count":2,"rows":7}',
      ),
    ).toEqual({
      kind: "sync_finished",
      at: "t",
      outcome: "failed",
      count: 2,
      rows: 7,
    });
    expect(parseEvent('{"kind":"other"}')).toBeUndefined();
    expect(parseEvent("")).toBeUndefined();
  });

  it("says what started a run", () => {
    const sync = {
      kind: "sync_finished",
      connector_id: "c1",
      outcome: "any",
    } as const;
    expect(eventText(undefined, undefined, names)).toBe("Started");
    expect(eventText({ kind: "manual", at: "" }, undefined, names)).toBe(
      "Run by hand",
    );
    expect(eventText({ kind: "schedule", at: "" }, undefined, names)).toBe(
      "On its schedule",
    );
    expect(
      eventText(
        { kind: "sync_finished", at: "", outcome: "failed" },
        sync,
        names,
      ),
    ).toBe("A sync of “Stripe” failed");
    expect(eventText({ kind: "sync_finished", at: "" }, undefined, names)).toBe(
      "A sync of its connection succeeded",
    );
    expect(
      eventText(
        { kind: "load_ready", at: "", outcome: "partial" },
        { kind: "load_ready", ontology_id: "o1" },
        names,
      ),
    ).toBe("A load of “Sales” is partly ready");
    expect(eventText({ kind: "load_ready", at: "" }, undefined, names)).toBe(
      "A load of its ontology is ready",
    );
    expect(
      eventText(
        { kind: "document_arrived", at: "", count: 3 },
        { kind: "document_arrived", set_id: "s1" },
        names,
      ),
    ).toBe("3 documents arrived in “General”");
    expect(
      eventText({ kind: "document_arrived", at: "" }, undefined, names),
    ).toBe("A document arrived in its set");
    expect(
      eventText(
        { kind: "generation_finished", at: "", outcome: "failed" },
        { kind: "generation_finished", ontology_id: "o1", outcome: "any" },
        names,
      ),
    ).toBe("A draft of “Sales” failed");
    expect(
      eventText({ kind: "generation_finished", at: "" }, undefined, names),
    ).toBe("A draft of its ontology finished");
  });

  it("places each step of a run, and reads its spend", () => {
    const states = stepStates(simple, {
      id: "r",
      nodes: [{ nodeId: "start", status: "succeeded" }, { nodeId: "x" }],
    });
    expect([...states]).toEqual([
      ["start", "succeeded"],
      ["tell", "waiting"],
    ]);
    expect(isLiveRun({ id: "r", status: "queued" })).toBe(true);
    expect(isLiveRun({ id: "r", status: "skipped" })).toBe(false);
    expect(isLiveRun(null)).toBe(false);
    expect(runUsd("1500000")).toBe(1.5);
    expect(runUsd(undefined)).toBe(0);
    expect(usdText("1500000")).toBe("$1.50");
    expect(usdText("1200")).toBe("under $0.01");
    expect(usdText("0")).toBe("$0.00");
  });
});

describe("automations", () => {
  const a = (x: Partial<FoundryAutomation>): FoundryAutomation => ({
    id: "a",
    ...x,
  });
  it("says why one isn't running, to its owner and to others", () => {
    expect(stoppedText(a({ status: "active" }), true)).toBeUndefined();
    expect(stoppedText(a({ status: "needs_approval" }), true)).toMatch(
      /your approval/,
    );
    expect(stoppedText(a({ status: "needs_approval" }), false)).toMatch(
      /its owner’s approval/,
    );
    expect(
      stoppedText(
        a({ status: "paused", pausedReason: "failures", failures: 3 }),
        true,
      ),
    ).toBe(
      "Paused after 3 failed runs in a row. Fix what failed, then resume it.",
    );
    expect(
      stoppedText(a({ status: "paused", pausedReason: "failures" }), false),
    ).toMatch(/Its owner resumes it/);
    expect(
      stoppedText(
        a({ status: "paused", pausedReason: "owner_lost_access" }),
        true,
      ),
    ).toBe("Paused: its owner can no longer act in this workspace.");
    expect(
      stoppedText(a({ status: "paused", pausedReason: "person" }), true),
    ).toMatch(/when you run it/);
    expect(stoppedText(a({ status: "paused" }), false)).toMatch(
      /its owner runs it/,
    );
  });

  it("reads the automations' refusals", () => {
    const refused = new ApiError("x", 400, 9, [
      { reason: "FOUNDRY_AUTOMATION_BUSY" },
    ]);
    expect(automationRefusal(refused)).toBe("FOUNDRY_AUTOMATION_BUSY");
    expect(
      automationRefusal(new ApiError("x", 400, 9, [{ reason: "OTHER" }, "x"])),
    ).toBeUndefined();
    expect(automationRefusal(new ApiError("x", 400))).toBeUndefined();
    expect(automationRefusal(new Error("x"))).toBeUndefined();
    // Off, or not served by this WFE yet, the section says so calmly.
    expect(
      automationsUnavailable(
        new ApiError("off", 400, 9, [
          { reason: "FOUNDRY_AUTOMATIONS_DISABLED" },
        ]),
      ),
    ).toBe(true);
    expect(automationsUnavailable(new ApiError("Not Found", 404))).toBe(true);
    expect(automationsUnavailable(new ApiError("unimplemented", 501))).toBe(
      true,
    );
    expect(automationsUnavailable(refused)).toBe(false);
    expect(automationsUnavailable(new Error("x"))).toBe(false);
  });
});

describe("the overview's reads", () => {
  const poll = (automations: object[]) =>
    automationQueries
      .overview("ws", 7, "UTC")
      .refetchInterval({ state: { data: { automations } as never } });
  it("reads again while one is live or runs, at an active automation's pace", () => {
    expect(poll([{ status: "paused" }, { status: "needs_approval" }])).toBe(
      false,
    );
    expect(poll([])).toBe(false);
    // A trigger may fire any time: its run shows without a reload.
    expect(poll([{ status: "paused" }, { status: "active" }])).toBe(
      ACTIVE_POLL_MS,
    );
    // A live run's counts are read no faster: each read counts every run.
    expect(poll([{ status: "paused", lastRun: { status: "running" } }])).toBe(
      ACTIVE_POLL_MS,
    );
    expect(
      automationQueries.overview("ws", 7, "UTC").refetchInterval({ state: {} }),
    ).toBe(false);
  });
});

// How much each ontology an automation uses helped it, in words.
describe("an automation's ontologies' impact", () => {
  const o = { ontologyId: "o1", ontologyName: "Sales" };
  it("says how many assessed runs the ontology helped", () => {
    expect(impactText({ ...o, workflowId: "w", runs: "0" })).toBe(
      "No run has used it yet: its first is recorded here.",
    );
    expect(impactText({ ...o, workflowId: "w", runs: "1" })).toBe(
      "One run used it; it isn’t assessed yet.",
    );
    expect(impactText({ ...o, workflowId: "w", runs: "1200" })).toBe(
      "1,200 runs used it; none is assessed yet.",
    );
    expect(
      impactText({
        ...o,
        workflowId: "w",
        runs: "4",
        verdicts: [
          { verdict: "essential", runs: "2" },
          { verdict: "helpful", runs: "1" },
          { verdict: "unused", runs: "1" },
        ],
      }),
    ).toBe("Helped 3 of the 4 runs assessed.");
    expect(
      impactText({
        ...o,
        workflowId: "w",
        runs: "1",
        verdicts: [{ verdict: "unused", runs: "1" }],
      }),
    ).toBe("Helped 0 of the run assessed.");
    // Runs that read or asked nothing of it aren't assessed, and are said
    // apart.
    expect(
      impactText({
        ...o,
        workflowId: "w",
        runs: "2",
        finishedRuns: "2",
        neverCalledRuns: "2",
      }),
    ).toBe(
      "2 runs used it without reading or asking it anything, so none is assessed.",
    );
    expect(
      impactText({ ...o, workflowId: "w", runs: "1", neverCalledRuns: "1" }),
    ).toBe(
      "One run used it without reading or asking it anything, so none is assessed.",
    );
    expect(
      impactText({ ...o, workflowId: "w", runs: "3", neverCalledRuns: "1" }),
    ).toBe(
      "3 runs used it; none is assessed yet. One run read or asked it nothing, so isn’t assessed.",
    );
    expect(
      impactText({
        ...o,
        workflowId: "w",
        runs: "5",
        neverCalledRuns: "2",
        verdicts: [{ verdict: "helpful", runs: "3" }],
      }),
    ).toBe(
      "Helped 3 of the 3 runs assessed. 2 runs read or asked it nothing, so aren’t assessed.",
    );
    expect(impactText({ ...o, untracked: "workflows_full" })).toMatch(
      /^Its runs aren’t recorded: the ontology holds 100 use cases/,
    );
  });

  it("tags how much an ontology helped one run", () => {
    const r = { ...o, workflowId: "w", workflowRunId: "r" };
    expect(runImpact({ ...r, status: "open" })).toEqual({
      kind: "open",
      text: "Recording while the run goes",
    });
    expect(
      runImpact({
        ...r,
        status: "finished",
        assessmentStatus: "done",
        verdict: "helpful",
      }),
    ).toEqual({
      kind: "verdict",
      verdict: "helpful",
    });
    expect(
      runImpact({ ...r, status: "finished", assessmentStatus: "done" }).kind,
    ).toBe("failed");
    expect(
      runImpact({ ...r, status: "finished", assessmentStatus: "queued" }).kind,
    ).toBe("pending");
    expect(
      runImpact({ ...r, status: "finished", assessmentStatus: "running" }).kind,
    ).toBe("pending");
    expect(
      runImpact({ ...r, status: "finished", assessmentStatus: "failed" }).kind,
    ).toBe("failed");
    expect(
      runImpact({ ...r, status: "finished", assessmentStatus: "skipped" }).kind,
    ).toBe("skipped");
    // Finished without a read or question: not assessed, whatever its row.
    expect(
      runImpact({
        ...r,
        status: "finished",
        neverCalled: true,
        assessmentStatus: "skipped",
      }),
    ).toEqual({
      kind: "unassessed",
      text: "Not assessed: it read or asked nothing of the ontology",
    });
    expect(runImpact({ ...r, status: "finished" })).toEqual({
      kind: "unassessed",
      text: "Not assessed",
    });
  });

  it("reads a run again until its ontologies are assessed", () => {
    const r = { ...o, workflowId: "w", workflowRunId: "r" };
    expect(assessing(undefined)).toBe(false);
    expect(assessing({ id: "x" })).toBe(false);
    expect(assessing({ id: "x", ontologies: [{ ...r, status: "open" }] })).toBe(
      true,
    );
    expect(
      assessing({
        id: "x",
        ontologies: [{ ...r, status: "finished", assessmentStatus: "queued" }],
      }),
    ).toBe(true);
    expect(
      assessing({
        id: "x",
        ontologies: [
          {
            ...r,
            status: "finished",
            assessmentStatus: "done",
            verdict: "helpful",
          },
        ],
      }),
    ).toBe(false);
    expect(
      assessing({ id: "x", ontologies: [{ ...r, status: "finished" }] }),
    ).toBe(false);
  });
});

describe("the workflows' paths", () => {
  it("lives under /workflows, each id one segment", () => {
    expect(automationPaths.automations()).toBe("/workflows");
    expect(automationPaths.newAutomation("blank")).toBe(
      "/workflows/new?template=blank",
    );
    expect(automationPaths.automation("a b", "runs")).toBe(
      "/workflows/a%20b?tab=runs",
    );
    expect(automationPaths.automationRun("a1", "r1")).toBe(
      "/workflows/a1/runs/r1",
    );
  });

  it.each([
    ["/workflows/a%20b", "a b"],
    ["/workflows/a1/runs/r1", "a1"],
    ["/workflows/new", undefined],
    ["/workflows", undefined],
    ["/automations/a1", undefined],
  ])("opens %s as %s", (path, id) => expect(openAutomation(path)).toBe(id));
});
