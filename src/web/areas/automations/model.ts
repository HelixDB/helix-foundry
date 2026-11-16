import { z } from "zod";
import type {
  AutomationAction,
  AutomationEdge,
  AutomationNode,
  AutomationNodeConfig,
  AutomationNodeOutput,
  AutomationNodeStatus,
  AutomationRefusal,
  AutomationSpec,
  AutomationTrigger,
  AutomationTriggerEvent,
  FoundryAutomation,
  FoundryAutomationOntology,
  FoundryAutomationRun,
  FoundryAutomationRunOntology,
} from "../../../shared/wfe";
import { AUTOMATION_REFUSALS } from "../../../shared/wfe";
import { isRecord } from "../../../shared/json";
import { zoneOffset, zoneTime } from "../../../shared/time-zone";
import { ApiError } from "../../api";

// Automations, as pure functions (backend/automation is the contract): an
// automation's spec, a graph of one trigger and the steps after it, read
// and checked exactly as the backend's ParseSpec reads it, so the builder
// says what the WFE would refuse before it is sent, and the mock refuses
// what the backend refuses; when a schedule fires next; each step in plain
// words, for people who never see the JSON; the templates a new automation
// starts from; the builder's edits; and what a run's steps gave, read
// without trust.

/** The backend's bounds (package automation, api/automations.go). */
export const LIMITS = {
  nodes: 50,
  edges: 100,
  instruction: 2000,
  message: 500,
  allow: 10,
  specBytes: 64 << 10,
  name: 120,
  description: 1000,
  // Off the canvas, either way.
  position: 1e6,
} as const;

export type Kind = AutomationNode["kind"];
export type ScheduleTrigger = Extract<AutomationTrigger, { kind: "schedule" }>;
export type Weekday = Extract<ScheduleTrigger, { every: "week" }>["weekday"];
type Outcome = "succeeded" | "failed" | "any";

export const WEEKDAYS: readonly Weekday[] = [
  "monday",
  "tuesday",
  "wednesday",
  "thursday",
  "friday",
  "saturday",
  "sunday",
];

const NODE_ID = /^[a-z0-9][a-z0-9_-]{0,31}$/;
const TARGET_ID = /^[A-Za-z0-9_-]{1,128}$/;
const CLOCK = /^([01][0-9]|2[0-3]):([0-5][0-9])$/;

/**
 * Whether `name` is an IANA time zone the backend reads (automation.Zone):
 * not empty, not Local, no spaces, and known; not ICU's Etc/Unknown, which
 * a browser that can't tell its zone reports and the zone database lacks.
 *
 * @example
 * validZone("Europe/London"); // true
 * validZone("Local"); // false
 */
export function validZone(name: string | undefined): boolean {
  if (
    !name ||
    name === "Local" ||
    name === "Etc/Unknown" ||
    name.length > 64 ||
    /\s/.test(name)
  )
    return false;
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: name });
    return true;
  } catch {
    return false;
  }
}

/**
 * The browser's time zone, else UTC (a browser that can't tell reports
 * none, or one the backend refuses): a schedule's default, left out of its
 * words, and the days the overview counts by.
 */
export function here(): string {
  const zone: string | undefined =
    Intl.DateTimeFormat().resolvedOptions().timeZone;
  return validZone(zone) ? zone! : "UTC";
}

// ---------------------------------------------------------------- reading

const id = z.string().regex(TARGET_ID);
const outcome = z.enum(["succeeded", "failed", "any"]);
const zone = z.string().refine(validZone);
const clock = z.string().regex(CLOCK);
const TRIGGER = z.union([
  z.strictObject({
    kind: z.literal("schedule"),
    every: z.literal("hour"),
    minute: z.number().int().min(0).max(59),
    time_zone: zone,
  }),
  z.strictObject({
    kind: z.literal("schedule"),
    every: z.literal("day"),
    at: clock,
    time_zone: zone,
  }),
  z.strictObject({
    kind: z.literal("schedule"),
    every: z.literal("week"),
    weekday: z.enum(WEEKDAYS as [Weekday, ...Weekday[]]),
    at: clock,
    time_zone: zone,
  }),
  z.strictObject({
    kind: z.literal("sync_finished"),
    connector_id: id,
    outcome,
  }),
  z.strictObject({ kind: z.literal("load_ready"), ontology_id: id }),
  z.strictObject({ kind: z.literal("document_arrived"), set_id: id }),
  z.strictObject({
    kind: z.literal("generation_finished"),
    ontology_id: id,
    outcome,
  }),
  z.strictObject({ kind: z.literal("manual") }),
]) satisfies z.ZodType<AutomationTrigger>;
const TRIGGER_SHAPES: Record<AutomationTrigger["kind"], string> = {
  schedule: `{kind: "schedule", every: "hour", minute: 0 to 59, time_zone}, {kind: "schedule", every: "day", at: "HH:MM", time_zone} or {kind: "schedule", every: "week", weekday: "monday" … "sunday", at: "HH:MM", time_zone}`,
  sync_finished: `{kind: "sync_finished", connector_id, outcome: "succeeded" | "failed" | "any"}`,
  load_ready: `{kind: "load_ready", ontology_id}`,
  document_arrived: `{kind: "document_arrived", set_id}`,
  generation_finished: `{kind: "generation_finished", ontology_id, outcome: "succeeded" | "failed" | "any"}`,
  manual: `{kind: "manual"}`,
};
const ACTION = z.union([
  z.strictObject({ operation: z.literal("sync_connector"), connector_id: id }),
  z.strictObject({ operation: z.literal("create_load"), ontology_id: id }),
  z.strictObject({
    operation: z.literal("publish_dashboard"),
    dashboard_id: id,
  }),
]) satisfies z.ZodType<AutomationAction>;
const ACTION_SHAPE = `an action is {operation: "sync_connector", connector_id}, {operation: "create_load", ontology_id} or {operation: "publish_dashboard", dashboard_id}`;
const CHECK = z.union([
  z.strictObject({
    check: z.literal("answer"),
    equals: z.enum(["yes", "no"]),
  }),
  z.strictObject({
    check: z.literal("count"),
    op: z.enum(["at_least", "at_most", "equals"]),
    value: z.number().int().min(0),
  }),
]);
const CHECK_SHAPE = `a condition is {check: "answer", equals: "yes" | "no"} or {check: "count", op: "at_least" | "at_most" | "equals", value: 0 or more}`;
const WIRE = z.strictObject({
  v: z.number(),
  nodes: z
    .array(
      z.strictObject({
        id: z.string(),
        kind: z.string(),
        config: z.unknown(),
        position: z
          .strictObject({ x: z.number(), y: z.number() })
          .default({ x: 0, y: 0 }),
      }),
    )
    .nullish(),
  edges: z
    .array(
      z.strictObject({
        from: z.string().default(""),
        to: z.string().default(""),
        branch: z.string().optional(),
      }),
    )
    .nullish(),
});

/** How a check names a node in its errors: the backend's "node <id>" by default. */
export type NodeName = (id: string) => string;
const byId: NodeName = (id) => `node ${id}`;

/** A spec that reads, canonical, with its nodes in the order a run takes them; or why not. */
export type SpecResult =
  | { ok: true; spec: AutomationSpec; order: string[] }
  | { ok: false; error: string; nodeId?: string };

/** Text the backend takes: least to most characters, no control characters but line breaks and tabs. */
function textFits(text: string, least: number, most: number) {
  const n = [...text].length;
  return (
    n >= least &&
    n <= most &&
    // eslint-disable-next-line no-control-regex
    !/[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/.test(text)
  );
}

type NodeRead = { node: AutomationNode } | { error: string };

function readTrigger(raw: unknown): AutomationTrigger | string {
  const kind = isRecord(raw) ? raw.kind : undefined;
  if (!isRecord(raw)) return "a trigger: its config is a JSON object";
  if (typeof kind !== "string") return "a trigger: its config names its kind";
  if (!Object.hasOwn(TRIGGER_SHAPES, kind))
    return `a trigger's kind is "${kind}"; it is schedule, sync_finished, load_ready, document_arrived, generation_finished or manual`;
  const shape = TRIGGER_SHAPES[kind as AutomationTrigger["kind"]];
  if (
    kind === "schedule" &&
    typeof raw.time_zone === "string" &&
    !validZone(raw.time_zone)
  )
    return `time_zone "${raw.time_zone}" is not an IANA time zone, such as Europe/London`;
  const read = TRIGGER.safeParse(raw);
  return read.success ? read.data : `a ${kind} trigger is ${shape}`;
}

function readNode(wire: {
  id: string;
  kind: string;
  config: unknown;
  position: { x: number; y: number };
}): NodeRead {
  const { id: nodeId, position } = wire;
  if (!NODE_ID.test(nodeId))
    return {
      error: `node id "${nodeId}" must be 1 to 32 lowercase letters, digits, dashes or underscores, starting with a letter or digit`,
    };
  if (
    ![position.x, position.y].every(
      (p) => Number.isFinite(p) && Math.abs(p) <= LIMITS.position,
    )
  )
    return { error: `node ${nodeId}'s position is off the canvas` };
  const at = (error: string): NodeRead => ({
    error: `node ${nodeId}: ${error}`,
  });
  const base = { id: nodeId, position: { x: position.x, y: position.y } };
  const raw = wire.config;
  switch (wire.kind) {
    case "trigger": {
      const t = readTrigger(raw);
      return typeof t === "string"
        ? at(t)
        : { node: { ...base, kind: "trigger", config: t } };
    }
    case "assistant": {
      const read = z
        .strictObject({
          instruction: z.string(),
          allow: z.array(z.unknown()).nullish(),
        })
        .safeParse(raw);
      if (!read.success) return at("an Assistant step is {instruction, allow}");
      const instruction = read.data.instruction.trim();
      if (!textFits(instruction, 1, LIMITS.instruction))
        return at(
          `its instruction is 1 to ${LIMITS.instruction} characters, with no control characters`,
        );
      const allow = read.data.allow || [];
      if (allow.length > LIMITS.allow)
        return at(
          `an Assistant step runs at most ${LIMITS.allow} actions unasked, not ${allow.length}`,
        );
      const actions: AutomationAction[] = [];
      for (const a of allow) {
        const action = ACTION.safeParse(a);
        if (!action.success) return at(`allow: ${ACTION_SHAPE}`);
        if (actions.some((x) => sameAction(x, action.data)))
          return at(
            `allow names ${action.data.operation} on the same target twice`,
          );
        actions.push(action.data);
      }
      return {
        node: {
          ...base,
          kind: "assistant",
          config: { instruction, ...(actions.length && { allow: actions }) },
        },
      };
    }
    case "action": {
      const a = ACTION.safeParse(raw);
      return a.success
        ? { node: { ...base, kind: "action", config: a.data } }
        : at(ACTION_SHAPE);
    }
    case "condition": {
      const c = CHECK.safeParse(raw);
      return c.success
        ? { node: { ...base, kind: "condition", config: c.data } }
        : at(CHECK_SHAPE);
    }
    case "notify": {
      const n = z.strictObject({ message: z.string() }).safeParse(raw);
      if (!n.success) return at("a notify step is {message}");
      const message = n.data.message.trim();
      return textFits(message, 0, LIMITS.message)
        ? { node: { ...base, kind: "notify", config: { message } } }
        : at(
            `its message is 0 to ${LIMITS.message} characters, with no control characters`,
          );
    }
    default:
      return {
        error: `node ${nodeId}'s kind is "${wire.kind}"; it is trigger, assistant, action, condition or notify`,
      };
  }
}

/** Whether two actions are one: the same operation on the same target. */
export const sameAction = (a: AutomationAction, b: AutomationAction) =>
  actionTarget(a) === actionTarget(b) && a.operation === b.operation;
/** An action's target id. */
export const actionTarget = (a: AutomationAction) =>
  a.operation === "sync_connector"
    ? a.connector_id
    : a.operation === "create_load"
      ? a.ontology_id
      : a.dashboard_id;

/** Whether a condition can check a step's output (automation Check.checks). */
function checks(c: AutomationNodeConfig, from: AutomationNode) {
  if (c.kind !== "condition") return false;
  if (from.kind === "assistant") return true;
  if (c.config.check === "answer") return false;
  return (
    from.kind === "trigger" &&
    (from.config.kind === "document_arrived" ||
      from.config.kind === "sync_finished")
  );
}

/**
 * Reads a spec as the backend's ParseSpec does, refusing what it refuses
 * with its words: `name` says how its errors name a node (the backend's
 * "node <id>", or a step's title for the builder). The spec comes back
 * canonical (text trimmed, empty allows left out) with every node's id in
 * the order a run takes them: the trigger first, ties in the order given.
 *
 * @example
 * const r = parseSpec(JSON.stringify({
 *   v: 1,
 *   nodes: [
 *     { id: "start", kind: "trigger", config: { kind: "manual" }, position: { x: 0, y: 0 } },
 *     { id: "tell", kind: "notify", config: { message: " Done " }, position: { x: 0, y: 140 } },
 *   ],
 *   edges: [{ from: "start", to: "tell" }],
 * }));
 * r.ok && r.order; // ["start", "tell"]
 * r.ok && r.spec.nodes[1].config; // { message: "Done" }
 */
export function parseSpec(raw: string | unknown, name = byId): SpecResult {
  const text = typeof raw === "string" ? raw : JSON.stringify(raw);
  if (new TextEncoder().encode(text).length > LIMITS.specBytes)
    return {
      ok: false,
      error: `a spec is at most ${LIMITS.specBytes >> 10} KiB`,
    };
  let json: unknown;
  try {
    json = JSON.parse(text);
  } catch {
    return { ok: false, error: "the spec is not {v, nodes, edges}: not JSON" };
  }
  const wire = WIRE.safeParse(json);
  if (!wire.success)
    return {
      ok: false,
      error: `the spec is not {v, nodes, edges}: ${wire.error.issues[0]?.message ?? "it doesn't read"}`,
    };
  const wn = wire.data.nodes || [],
    we = wire.data.edges || [];
  if (wire.data.v !== 1)
    return {
      ok: false,
      error: `the spec's v is ${wire.data.v}; this Foundry reads v 1`,
    };
  if (wn.length < 2 || wn.length > LIMITS.nodes)
    return {
      ok: false,
      error: `an automation has a trigger and at least one step, at most ${LIMITS.nodes} nodes in all, not ${wn.length}`,
    };
  if (we.length > LIMITS.edges)
    return {
      ok: false,
      error: `an automation has at most ${LIMITS.edges} edges, not ${we.length}`,
    };
  const nodes: AutomationNode[] = [];
  for (const w of wn) {
    const read = readNode(w);
    if ("error" in read)
      return {
        ok: false,
        error: read.error.replace(`node ${w.id}`, name(w.id)),
        nodeId: w.id,
      };
    if (nodes.some((n) => n.id === read.node.id))
      return { ok: false, error: `two nodes have the id ${w.id}` };
    nodes.push(read.node);
  }
  const edges: AutomationEdge[] = we.map((e) => ({
    from: e.from,
    to: e.to,
    ...(e.branch ? { branch: e.branch as "true" | "false" } : {}),
  }));
  const spec: AutomationSpec = { v: 1, nodes, edges };
  const checked = checkGraph(spec, name);
  return "error" in checked
    ? { ok: false, ...checked }
    : { ok: true, spec, order: checked.order };
}

const nodeOf = (spec: AutomationSpec, id: string) =>
  spec.nodes.find((n) => n.id === id);
/** The edges into a node, in the order given. */
export const edgesInto = (spec: AutomationSpec, id: string) =>
  spec.edges.filter((e) => e.to === id);
/** The edges out of a node, in the order given. */
export const edgesOutOf = (spec: AutomationSpec, id: string) =>
  spec.edges.filter((e) => e.from === id);

/** The graph's rules (Spec.check), and its order. */
function checkGraph(
  spec: AutomationSpec,
  name: NodeName,
): { order: string[] } | { error: string; nodeId?: string } {
  const triggers = spec.nodes.filter((n) => n.kind === "trigger");
  if (triggers.length !== 1)
    return {
      error: `an automation has exactly one trigger, not ${triggers.length}`,
    };
  const seen = new Set<string>();
  for (const e of spec.edges) {
    const from = nodeOf(spec, e.from),
      to = nodeOf(spec, e.to);
    const branch = e.branch ?? "";
    if (!from)
      return { error: `an edge comes from "${e.from}", which is no node` };
    if (!to) return { error: `an edge goes to "${e.to}", which is no node` };
    if (e.from === e.to)
      return { error: `${name(e.from)} has an edge to itself`, nodeId: e.from };
    if (to.kind === "trigger")
      return {
        error: `the trigger ${e.to} starts the automation: no edge goes into it`,
      };
    if (seen.has(e.from + "\0" + e.to))
      return {
        error: `two edges go from ${name(e.from)} to ${name(e.to)}`,
        nodeId: e.to,
      };
    if (from.kind === "condition" && branch !== "true" && branch !== "false")
      return {
        error: `the edge from ${name(e.from)} to ${name(e.to)} is its true or its false branch`,
        nodeId: e.from,
      };
    if (from.kind !== "condition" && branch !== "")
      return {
        error: `the edge from ${name(e.from)} to ${name(e.to)} has a branch, which only a condition's edges have`,
        nodeId: e.from,
      };
    seen.add(e.from + "\0" + e.to);
  }
  for (const n of spec.nodes) {
    if (n.kind !== "condition") continue;
    for (const b of ["true", "false"] as const) {
      const count = spec.edges.filter(
        (e) => e.from === n.id && e.branch === b,
      ).length;
      if (count > 1)
        return {
          error: `${name(n.id)} has ${count} ${b} branches; it has at most one`,
          nodeId: n.id,
        };
    }
    const into = edgesInto(spec, n.id);
    if (into.length !== 1)
      return {
        error: `${name(n.id)} checks the one step before it, so exactly one edge goes into it, not ${into.length}`,
        nodeId: n.id,
      };
    const from = nodeOf(spec, into[0]!.from)!;
    if (!checks(n, from))
      return {
        error: `${name(n.id)} can't check ${name(from.id)}: ${
          n.config.check === "answer"
            ? "an answer is an Assistant step's"
            : "a count is an Assistant step's, or a document_arrived or sync_finished trigger's"
        }`,
        nodeId: n.id,
      };
  }
  // An action on the work its trigger waits for would start another run
  // each time it ran: the automation would never stop.
  const waitsOn = triggers[0]!.kind === "trigger" && triggers[0]!.config;
  for (const n of spec.nodes) {
    const actions =
      n.kind === "action"
        ? [n.config]
        : n.kind === "assistant"
          ? (n.config.allow ?? [])
          : [];
    for (const a of actions) {
      const what =
        waitsOn &&
        waitsOn.kind === "sync_finished" &&
        a.operation === "sync_connector" &&
        a.connector_id === waitsOn.connector_id
          ? "syncs the connector"
          : waitsOn &&
              waitsOn.kind === "load_ready" &&
              a.operation === "create_load" &&
              a.ontology_id === waitsOn.ontology_id
            ? "loads the ontology"
            : "";
      if (what)
        return {
          error: `${name(n.id)} ${what} its trigger waits on, so each run would start another: start it on a schedule instead`,
          nodeId: n.id,
        };
    }
  }
  // Kahn's, ties in the order the nodes were given.
  const trigger = triggers[0]!.id;
  const waiting = new Map<string, number>();
  for (const e of spec.edges) waiting.set(e.to, (waiting.get(e.to) ?? 0) + 1);
  const ready = [trigger],
    order: string[] = [];
  while (ready.length) {
    const at = ready.shift()!;
    order.push(at);
    for (const n of spec.nodes) {
      if (!spec.edges.some((e) => e.from === at && e.to === n.id)) continue;
      const left = (waiting.get(n.id) ?? 0) - 1;
      waiting.set(n.id, left);
      if (left === 0) ready.push(n.id);
    }
  }
  if (order.length === spec.nodes.length) return { order };
  const missing = spec.nodes.find((n) => !order.includes(n.id))!;
  return reaches(spec, trigger, missing.id)
    ? {
        error: `${name(missing.id)} is in a cycle: an automation's edges go one way`,
        nodeId: missing.id,
      }
    : {
        error: `${name(missing.id)} isn't reached from the trigger: connect it`,
        nodeId: missing.id,
      };
}

/** Whether `to` is reached from `from` along edges. */
export function reaches(spec: AutomationSpec, from: string, to: string) {
  const seen = new Set([from]),
    stack = [from];
  while (stack.length) {
    const at = stack.pop()!;
    for (const e of edgesOutOf(spec, at)) {
      if (e.to === to) return true;
      if (!seen.has(e.to)) {
        seen.add(e.to);
        stack.push(e.to);
      }
    }
  }
  return false;
}

/** The spec's trigger: a read spec has exactly one. */
export const triggerOf = (spec: AutomationSpec) =>
  spec.nodes.find(
    (n): n is Extract<AutomationNode, { kind: "trigger" }> =>
      n.kind === "trigger",
  )!;

/**
 * The spec as the backend writes it (Spec.MarshalJSON): its fields in its
 * order, empty ones left out where it leaves them out, so the same spec is
 * the same text.
 */
export function canonicalJson(spec: AutomationSpec): string {
  const config = (n: AutomationNode): Record<string, unknown> => {
    switch (n.kind) {
      case "trigger": {
        const t = n.config;
        return {
          kind: t.kind,
          ...("every" in t && { every: t.every }),
          ...("minute" in t && { minute: t.minute }),
          ...("weekday" in t && { weekday: t.weekday }),
          ...("at" in t && { at: t.at }),
          ...("time_zone" in t && { time_zone: t.time_zone }),
          ...("connector_id" in t && { connector_id: t.connector_id }),
          ...("ontology_id" in t && { ontology_id: t.ontology_id }),
          ...("set_id" in t && { set_id: t.set_id }),
          ...("outcome" in t && { outcome: t.outcome }),
        };
      }
      case "assistant":
        return {
          instruction: n.config.instruction,
          ...(n.config.allow?.length && {
            allow: n.config.allow.map(actionWire),
          }),
        };
      case "action":
        return actionWire(n.config);
      case "condition":
        return n.config.check === "answer"
          ? { check: "answer", equals: n.config.equals }
          : { check: "count", op: n.config.op, value: n.config.value };
      case "notify":
        return { message: n.config.message };
    }
  };
  return JSON.stringify({
    v: 1,
    nodes: spec.nodes.map((n) => ({
      id: n.id,
      kind: n.kind,
      config: config(n),
      position: { x: n.position.x, y: n.position.y },
    })),
    edges: spec.edges.map((e) => ({
      from: e.from,
      to: e.to,
      ...(e.branch && { branch: e.branch }),
    })),
  });
}
const actionWire = (a: AutomationAction) => ({
  operation: a.operation,
  ...("connector_id" in a && { connector_id: a.connector_id }),
  ...("ontology_id" in a && { ontology_id: a.ontology_id }),
  ...("dashboard_id" in a && { dashboard_id: a.dashboard_id }),
});

/**
 * Whether two specs run the same steps the same way, wherever the canvas
 * draws them (automation.SameSteps): moving nodes needs no new approval.
 *
 * @example
 * sameSteps(spec, { ...spec, nodes: spec.nodes.map((n) => ({ ...n, position: { x: 9, y: 9 } })) }); // true
 */
export function sameSteps(a: AutomationSpec, b: AutomationSpec) {
  const laid = (s: AutomationSpec) =>
    canonicalJson({
      ...s,
      nodes: s.nodes.map((n) => ({ ...n, position: { x: 0, y: 0 } })),
    });
  return laid(a) === laid(b);
}

// ---------------------------------------------------------------- schedules

const formatters = new Map<string, Intl.DateTimeFormat>();
function wall(t: number, zone: string) {
  let f = formatters.get(zone);
  if (!f) {
    f = new Intl.DateTimeFormat("en-US", {
      timeZone: zone,
      hourCycle: "h23",
      year: "numeric",
      month: "numeric",
      day: "numeric",
      hour: "numeric",
      minute: "numeric",
      second: "numeric",
      weekday: "short",
    });
    formatters.set(zone, f);
  }
  const p = Object.fromEntries(
    f.formatToParts(t).map((x) => [x.type, x.value]),
  );
  return {
    y: Number(p.year),
    mo: Number(p.month),
    d: Number(p.day),
    h: Number(p.hour),
    mi: Number(p.minute),
    s: Number(p.second),
    wd: ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"].indexOf(p.weekday!),
  };
}
/** The instant a wall-clock time in `zone` is (Go's time.Date: a day past the month's end rolls over). */
function instant(
  y: number,
  mo: number,
  d: number,
  h: number,
  mi: number,
  zone: string,
) {
  const naive = Date.UTC(y, mo - 1, d, h, mi);
  const offset = (t: number) => {
    const w = wall(t, zone);
    return Date.UTC(w.y, w.mo - 1, w.d, w.h, w.mi, w.s) - (t - (t % 1000));
  };
  const first = naive - offset(naive);
  return naive - offset(first);
}

/**
 * When a schedule fires first, strictly after `after` (ms), as the
 * backend's Schedule.Next: on the wall clock of its zone, every hour at its
 * minute, every day or every week at its time.
 *
 * @example
 * nextFire({ kind: "schedule", every: "day", at: "09:00", time_zone: "UTC" }, Date.parse("2026-11-16T09:00:00Z"));
 * // Date.parse("2026-11-17T09:00:00Z")
 */
export function nextFire(s: ScheduleTrigger, after: number): number {
  const w = wall(after, s.time_zone);
  if (s.every === "hour") {
    let at = instant(w.y, w.mo, w.d, w.h, s.minute, s.time_zone);
    while (at <= after) at += 3_600_000;
    return at;
  }
  const [h, mi] = s.at.split(":").map(Number) as [number, number];
  if (s.every === "day") {
    let at = instant(w.y, w.mo, w.d, h, mi, s.time_zone);
    for (let day = 1; at <= after; day++)
      at = instant(w.y, w.mo, w.d + day, h, mi, s.time_zone);
    return at;
  }
  const target = (WEEKDAYS.indexOf(s.weekday) + 1) % 7,
    ahead = (target - w.wd + 7) % 7;
  const at = instant(w.y, w.mo, w.d + ahead, h, mi, s.time_zone);
  return at > after
    ? at
    : instant(w.y, w.mo, w.d + ahead + 7, h, mi, s.time_zone);
}

// ---------------------------------------------------------------- words

/** What each target is called in the workspace, as the page reads them; undefined when unknown or gone. */
export type Names = {
  connector: (id: string) => string | undefined;
  ontology: (id: string) => string | undefined;
  set: (id: string) => string | undefined;
  dashboard: (id: string) => string | undefined;
};
export const NO_NAMES: Names = {
  connector: () => undefined,
  ontology: () => undefined,
  set: () => undefined,
  dashboard: () => undefined,
};

const quoted = (name: string | undefined, none: string) =>
  name ? `“${name}”` : none;
const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
const dayName = (d: Weekday) => cap(d);

/**
 * A schedule's times in words, in its zone's: a day's and a week's time
 * name the zone ("New York time"), an hour's minute only in a zone whose
 * offset isn't whole hours, the one place the minute past differs.
 *
 * @example
 * scheduleText({ kind: "schedule", every: "week", weekday: "monday", at: "09:00", time_zone: "Europe/London" });
 * // "every Monday at 09:00 London time"
 */
export function scheduleText(s: ScheduleTrigger) {
  if (s.every === "hour") {
    const when =
      s.minute === 0 ? "every hour, on the hour" : `every hour at ${s.minute} past`;
    return validZone(s.time_zone) && zoneOffset(s.time_zone).includes(":")
      ? `${when}, ${zoneTime(s.time_zone)}`
      : when;
  }
  const when =
    s.every === "day" ? `every day at ${s.at}` : `every ${dayName(s.weekday)} at ${s.at}`;
  return `${when} ${zoneTime(s.time_zone)}`;
}

const OUTCOME_WORDS: Record<Outcome, string> = {
  succeeded: "succeeds",
  failed: "fails",
  any: "ends",
};

/**
 * What starts the automation, as a clause after "When…" reads it.
 *
 * @example
 * triggerText({ kind: "sync_finished", connector_id: "c1", outcome: "succeeded" }, { ...NO_NAMES, connector: () => "Stripe" });
 * // "a sync of “Stripe” succeeds"
 */
export function triggerText(t: AutomationTrigger, names: Names) {
  switch (t.kind) {
    case "schedule":
      return scheduleText(t);
    case "sync_finished":
      return `a sync of ${quoted(names.connector(t.connector_id), "a connection")} ${OUTCOME_WORDS[t.outcome]}`;
    case "load_ready":
      return `a load of ${quoted(names.ontology(t.ontology_id), "an ontology")} is ready`;
    case "document_arrived":
      return `documents arrive in ${quoted(names.set(t.set_id), "a document set")}`;
    case "generation_finished":
      return `a draft of ${quoted(names.ontology(t.ontology_id), "an ontology")} ${OUTCOME_WORDS[t.outcome]}`;
    case "manual":
      return "you run it";
  }
}

/**
 * What starts an automation, from what a list holds of it (its trigger's
 * kind and target, not its spec), as a phrase.
 *
 * @example
 * listTriggerText("sync_finished", "c1", { ...NO_NAMES, connector: () => "Stripe" }); // "After syncs of “Stripe”"
 */
export function listTriggerText(
  kind: AutomationTrigger["kind"] | undefined,
  target: string | undefined,
  names: Names,
) {
  const t = target || "";
  switch (kind) {
    case "schedule":
      return "On a schedule";
    case "sync_finished":
      return `After syncs of ${quoted(names.connector(t), "a connection")}`;
    case "load_ready":
      return `When a load of ${quoted(names.ontology(t), "an ontology")} is ready`;
    case "document_arrived":
      return `When documents arrive in ${quoted(names.set(t), "a document set")}`;
    case "generation_finished":
      return `After drafts of ${quoted(names.ontology(t), "an ontology")}`;
    default:
      return "Run by hand";
  }
}

/** An action in words, as a step reads it: "sync “Stripe”". */
export function actionText(a: AutomationAction, names: Names) {
  switch (a.operation) {
    case "sync_connector":
      return `sync ${quoted(names.connector(a.connector_id), "a connection")}`;
    case "create_load":
      return `load the current version of ${quoted(names.ontology(a.ontology_id), "an ontology")}`;
    case "publish_dashboard":
      return `refresh the public snapshot of ${quoted(names.dashboard(a.dashboard_id), "a dashboard")}`;
  }
}

/** A condition in words: "the answer is yes". */
export function conditionText(
  c: Extract<AutomationNodeConfig, { kind: "condition" }>["config"],
) {
  if (c.check === "answer") return `the answer is ${c.equals}`;
  const op = {
    at_least: "at least",
    at_most: "at most",
    equals: "exactly",
  }[c.op];
  return `the count is ${op} ${c.value}`;
}

/** Each kind's name, as the builder offers a new step of it. */
export const KIND_LABELS: Record<Kind, string> = {
  trigger: "Start",
  assistant: "Ask the Assistant",
  action: "Sync, load or refresh",
  condition: "Check an answer or a count",
  notify: "Email me",
};
/** Each kind's name, as a step's head says it. */
export const KIND_HEADS: Record<Kind, string> = {
  trigger: "When",
  assistant: "Assistant",
  action: "Action",
  condition: "If",
  notify: "Email",
};

/**
 * A step in words, as a sentence's clause reads it (no capital, no stop).
 * `email` is whether a notify step emails here (false on a local build,
 * where it only notes the run).
 */
export function nodeText(
  n: AutomationNode,
  names: Names,
  email: boolean,
) {
  switch (n.kind) {
    case "trigger":
      return n.config.kind === "manual"
        ? "only when you run it"
        : `when ${triggerText(n.config, names)}`;
    case "assistant":
      return n.config.instruction.trim()
        ? `ask the Assistant: “${n.config.instruction}”`
        : "ask the Assistant";
    case "action":
      return actionText(n.config, names);
    case "condition":
      return `if ${conditionText(n.config)}`;
    case "notify":
      return email
        ? n.config.message
          ? `email you “${n.config.message}”`
          : "email you what it found"
        : "note what it found in the run (this Foundry sends no email)";
  }
}

/**
 * The automation as one sentence: its trigger, then its steps in the order
 * a run takes them (until they can be read, top down as the canvas has
 * them); a step a condition's false branch leads to says "otherwise".
 *
 * @example
 * sentence(spec, names, true);
 * // "Every Monday at 09:00 UTC, sync “Stripe”, then ask the Assistant: “…”, then email you what it found."
 */
export function sentence(
  spec: AutomationSpec,
  names: Names,
  email: boolean,
) {
  const read = parseSpec(spec);
  // Steps that can't be read yet go as the canvas lays them out, top down.
  const order = read.ok
    ? read.order
    : [...spec.nodes]
        .sort(
          (a, b) => a.position.y - b.position.y || a.position.x - b.position.x,
        )
        .map((n) => n.id);
  const t = triggerOf(spec);
  const head =
    t.config.kind === "manual"
      ? "When you run it"
      : t.config.kind === "schedule"
        ? cap(triggerText(t.config, names))
        : `When ${triggerText(t.config, names)}`;
  const steps = order
    .map((id) => nodeOf(spec, id)!)
    .filter((n) => n.kind !== "trigger");
  return (
    steps.reduce((text, n, i) => {
      const otherwise = edgesInto(spec, n.id).some((e) => e.branch === "false"),
        after = i > 0 && steps[i - 1]!.kind === "condition" ? ", " : ", then ";
      return `${text}${i === 0 ? ", " : after}${otherwise ? "otherwise " : ""}${nodeText(n, names, email)}`;
    }, head) + "."
  );
}

/**
 * What a step still needs before it can be saved, in words; undefined when
 * it has all it needs (the WFE checks the rest).
 */
export function incomplete(n: AutomationNode): string | undefined {
  switch (n.kind) {
    case "trigger": {
      const t = n.config;
      if (t.kind === "sync_finished" && !t.connector_id)
        return "Choose a connection";
      if (
        (t.kind === "load_ready" || t.kind === "generation_finished") &&
        !t.ontology_id
      )
        return "Choose an ontology";
      if (t.kind === "document_arrived" && !t.set_id)
        return "Choose a document set";
      return undefined;
    }
    case "assistant":
      return n.config.instruction.trim()
        ? n.config.allow?.some((a) => !actionTarget(a))
          ? "Choose what each allowed action acts on"
          : undefined
        : "Write what the Assistant should do";
    case "action":
      return actionTarget(n.config)
        ? undefined
        : n.config.operation === "sync_connector"
          ? "Choose a connection"
          : n.config.operation === "create_load"
            ? "Choose an ontology"
            : "Choose a dashboard";
    default:
      return undefined;
  }
}

// -------------------------------------------------------------- suggestions

/**
 * The new-automation page's suggestions: prompts for the Assistant, each an
 * outcome, under its title and the hue of its icon (`--hf-<tone>-soft` and
 * `-text`). A click puts the prompt in the composer; nothing is sent.
 */
export const AUTOMATION_SUGGESTIONS = [
  {
    title: "Monday briefing",
    tone: "info",
    prompt:
      "Every Monday at 9am, read my data and email me what changed last week: customers won and lost, deals that moved, and anything that needs my attention.",
  },
  {
    title: "Renewal radar",
    tone: "accent",
    prompt:
      "Every morning, find customers whose contract renews in the next 30 days and email me each one with its plan, recent activity and open issues.",
  },
  {
    title: "Contract check",
    tone: "warning",
    prompt:
      "When new contracts are uploaded, compare each against our standard terms (length, price, auto-renewal, liability) and email me the ones that differ, quoting the clause.",
  },
  {
    title: "Churn alert",
    tone: "danger",
    prompt:
      "Every day, look for accounts at risk of churning (falling usage, open escalations, missed payments) and, if there are any, email me who and why.",
  },
] as const;

// ---------------------------------------------------------------- templates

/** What a template fills its steps with: the workspace's first of each, when it has one. */
export type TemplateContext = {
  timeZone: string;
  connector?: { id: string; name: string };
  ontology?: { id: string; name: string };
  set?: { id: string; name: string };
};
/** The templates' ids: a fixed vocabulary, as analytics may carry it. */
export type TemplateId =
  | "weekly-summary"
  | "sync-and-summarise"
  | "load-after-sync"
  | "check-documents"
  | "load-ready-report"
  | "blank";
export type Template = {
  id: TemplateId;
  title: string;
  description: string;
  // What it needs the workspace to have, said on its card when it hasn't.
  needs: readonly ("connector" | "ontology" | "set")[];
  build: (c: TemplateContext) => {
    name: string;
    description: string;
    spec: AutomationSpec;
  };
};

const at = (x: number, y: number) => ({ x, y });
const ROW = 150;

/** The automations a new one starts from, each a working graph once its targets are chosen. */
export const TEMPLATES: readonly Template[] = [
  {
    id: "weekly-summary",
    title: "Weekly summary by email",
    description:
      "Every Monday morning, the Assistant reads an ontology and emails you what changed.",
    needs: ["ontology"],
    build: (c) => ({
      name: "Weekly summary",
      description: "What changed in the data this week, every Monday.",
      spec: {
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
              time_zone: c.timeZone,
            },
            position: at(0, 0),
          },
          {
            id: "summarise",
            kind: "assistant",
            config: {
              instruction: `Summarise what changed in ${c.ontology?.name || "the ontology"} over the last week: new and changed records, and anything that looks unusual. Keep it to a few sentences.`,
            },
            position: at(0, ROW),
          },
          {
            id: "email",
            kind: "notify",
            config: { message: "Your weekly summary" },
            position: at(0, 2 * ROW),
          },
        ],
        edges: [
          { from: "start", to: "summarise" },
          { from: "summarise", to: "email" },
        ],
      },
    }),
  },
  {
    id: "sync-and-summarise",
    title: "Sync, then summarise",
    description:
      "Every morning, sync a connection, then have the Assistant email you a summary.",
    needs: ["connector"],
    build: (c) => ({
      name: `Morning ${c.connector?.name || "sync"}`,
      description: "A fresh sync and a short summary, every morning.",
      spec: {
        v: 1,
        nodes: [
          {
            id: "start",
            kind: "trigger",
            config: {
              kind: "schedule",
              every: "day",
              at: "08:00",
              time_zone: c.timeZone,
            },
            position: at(0, 0),
          },
          {
            id: "sync",
            kind: "action",
            config: {
              operation: "sync_connector",
              connector_id: c.connector?.id || "",
            },
            position: at(0, ROW),
          },
          {
            id: "summarise",
            kind: "assistant",
            config: {
              instruction:
                "Summarise anything that needs attention today, such as overdue invoices or records that changed overnight.",
            },
            position: at(0, 2 * ROW),
          },
          {
            id: "email",
            kind: "notify",
            config: { message: "This morning’s summary" },
            position: at(0, 3 * ROW),
          },
        ],
        edges: [
          { from: "start", to: "sync" },
          { from: "sync", to: "summarise" },
          { from: "summarise", to: "email" },
        ],
      },
    }),
  },
  {
    id: "load-after-sync",
    title: "Load after every sync",
    description:
      "When a connection finishes syncing, load an ontology’s current version, so it is never behind.",
    needs: ["connector", "ontology"],
    build: (c) => ({
      name: `Keep ${c.ontology?.name || "an ontology"} fresh`,
      description: "A new load after every successful sync.",
      spec: {
        v: 1,
        nodes: [
          {
            id: "start",
            kind: "trigger",
            config: {
              kind: "sync_finished",
              connector_id: c.connector?.id || "",
              outcome: "succeeded",
            },
            position: at(0, 0),
          },
          {
            id: "load",
            kind: "action",
            config: {
              operation: "create_load",
              ontology_id: c.ontology?.id || "",
            },
            position: at(0, ROW),
          },
        ],
        edges: [{ from: "start", to: "load" }],
      },
    }),
  },
  {
    id: "check-documents",
    title: "Check new documents",
    description:
      "When documents arrive in a set, the Assistant reads them and emails you only if one needs attention.",
    needs: ["set"],
    build: (c) => ({
      name: `Check ${c.set?.name || "new documents"}`,
      description: "An email when a new document needs attention.",
      spec: {
        v: 1,
        nodes: [
          {
            id: "start",
            kind: "trigger",
            config: { kind: "document_arrived", set_id: c.set?.id || "" },
            position: at(0, 0),
          },
          {
            id: "read",
            kind: "assistant",
            config: {
              instruction:
                "Read the documents that just arrived. Does any of them need attention, such as a renewal date or a change of terms? Answer yes or no, and say which and why.",
            },
            position: at(0, ROW),
          },
          {
            id: "needs",
            kind: "condition",
            config: { check: "answer", equals: "yes" },
            position: at(0, 2 * ROW),
          },
          {
            id: "email",
            kind: "notify",
            config: { message: "A new document needs your attention" },
            position: at(-140, 3 * ROW),
          },
        ],
        edges: [
          { from: "start", to: "read" },
          { from: "read", to: "needs" },
          { from: "needs", to: "email", branch: "true" },
        ],
      },
    }),
  },
  {
    id: "load-ready-report",
    title: "Report on each new load",
    description:
      "When a load of an ontology is ready, the Assistant checks it and emails you what it found.",
    needs: ["ontology"],
    build: (c) => ({
      name: `${c.ontology?.name || "Ontology"} load report`,
      description: "A short report after every load.",
      spec: {
        v: 1,
        nodes: [
          {
            id: "start",
            kind: "trigger",
            config: { kind: "load_ready", ontology_id: c.ontology?.id || "" },
            position: at(0, 0),
          },
          {
            id: "check",
            kind: "assistant",
            config: {
              instruction:
                "Look at the new load: how many records of each type it holds, and anything that needs review. Report it in a few sentences.",
            },
            position: at(0, ROW),
          },
          {
            id: "email",
            kind: "notify",
            config: { message: "A new load is ready" },
            position: at(0, 2 * ROW),
          },
        ],
        edges: [
          { from: "start", to: "check" },
          { from: "check", to: "email" },
        ],
      },
    }),
  },
  {
    id: "blank",
    title: "Start from scratch",
    description: "Only what starts it: choose a trigger, then add steps.",
    needs: [],
    build: () => ({
      name: "New automation",
      description: "",
      spec: {
        v: 1,
        nodes: [
          {
            id: "start",
            kind: "trigger",
            config: { kind: "manual" },
            position: at(0, 0),
          },
        ],
        edges: [],
      },
    }),
  },
];

// ---------------------------------------------------------------- edits

/** A new step's config, before its person chooses its target or words. */
export function blankConfig(
  kind: Exclude<Kind, "trigger">,
): AutomationNodeConfig {
  switch (kind) {
    case "assistant":
      return { kind, config: { instruction: "" } };
    case "action":
      return {
        kind,
        config: { operation: "sync_connector", connector_id: "" },
      };
    case "condition":
      return { kind, config: { check: "answer", equals: "yes" } };
    case "notify":
      return { kind, config: { message: "" } };
  }
}

/** A node id the spec doesn't have yet: its kind and a number. */
export function newNodeId(spec: AutomationSpec, kind: Kind) {
  for (let i = 1; ; i++) {
    const id = `${kind}-${i}`;
    if (!nodeOf(spec, id)) return id;
  }
}

/** The branch out of a condition not taken yet: true first. */
export function freeBranch(
  spec: AutomationSpec,
  conditionId: string,
): "true" | "false" | undefined {
  return (["true", "false"] as const).find(
    (b) => !spec.edges.some((e) => e.from === conditionId && e.branch === b),
  );
}

/**
 * The spec with a new step after `after`, placed under it; with no `after`,
 * under the lowest step, unconnected. Returns the new step's id.
 *
 * After a condition, it goes along the free branch (none free: unconnected).
 * After any other step, it slots in between: it takes over the step's joins
 * to the steps it led to, and those steps and everything after them move down
 * a row. A join into a condition stays, since a condition checks the one step
 * before it; a new condition takes over only a step's single join, as its
 * true branch. With nothing to take over, it starts a branch of its own.
 *
 * @example
 * // start → ask → email; a step added after ask runs between them.
 * addStep(spec, "notify", "ask").spec.edges;
 * // [{from: "start", to: "ask"}, {from: "ask", to: "notify-1"}, {from: "notify-1", to: "email"}]
 */
export function addStep(
  spec: AutomationSpec,
  kind: Exclude<Kind, "trigger">,
  after?: string,
): { spec: AutomationSpec; id: string } {
  const id = newNodeId(spec, kind),
    from = after ? nodeOf(spec, after) : undefined;
  const branch =
    from?.kind === "condition" ? freeBranch(spec, from.id) : undefined;
  const connects = !!from && (from.kind !== "condition" || !!branch);
  // The joins the new step takes over: none out of a condition.
  const outs =
    from && from.kind !== "condition"
      ? edgesOutOf(spec, from.id).filter(
          (e) => nodeOf(spec, e.to)!.kind !== "condition",
        )
      : [];
  const taken =
    kind === "condition"
      ? outs.length === 1 && edgesOutOf(spec, from!.id).length === 1
        ? outs
        : []
      : outs;
  const position = from
    ? {
        x:
          from.position.x +
          (taken.length
            ? 0
            : (branch === "false" ? 280 : 0) +
              edgesOutOf(spec, from.id).filter((e) => !e.branch).length * 280),
        y: from.position.y + ROW,
      }
    : {
        x: 0,
        y: Math.max(...spec.nodes.map((n) => n.position.y)) + ROW,
      };
  // What comes after the steps taken over, which moves down a row.
  const below = new Set<string>();
  const visit = (n: string) => {
    if (below.has(n)) return;
    below.add(n);
    edgesOutOf(spec, n).forEach((e) => visit(e.to));
  };
  taken.forEach((e) => visit(e.to));
  const node = { id, position, ...blankConfig(kind) } as AutomationNode;
  const moved = taken.map((e) => ({
    from: id,
    to: e.to,
    ...(kind === "condition" && { branch: "true" as const }),
  }));
  return {
    id,
    spec: {
      ...spec,
      nodes: [
        ...spec.nodes.map((n) =>
          below.has(n.id)
            ? { ...n, position: { ...n.position, y: n.position.y + ROW } }
            : n,
        ),
        node,
      ],
      edges: connects
        ? [
            ...spec.edges.filter((e) => !taken.includes(e)),
            { from: from.id, to: id, ...(branch && { branch }) },
            ...moved,
          ]
        : spec.edges,
    },
  };
}

/**
 * The spec without a step: what came into it goes on to what it led to
 * (along the branch the edge in took), so the steps after it stay
 * connected. The trigger is never removed.
 */
export function removeStep(spec: AutomationSpec, id: string): AutomationSpec {
  const n = nodeOf(spec, id);
  if (!n || n.kind === "trigger") return spec;
  const into = edgesInto(spec, id),
    outOf = edgesOutOf(spec, id);
  const kept = spec.edges.filter((e) => e.from !== id && e.to !== id);
  const bridged = into.flatMap((i) =>
    outOf.map((o) => ({
      from: i.from,
      to: o.to,
      ...(i.branch && { branch: i.branch }),
    })),
  );
  const edges = bridged.reduce<AutomationEdge[]>(
    (all, e) =>
      all.some(
        (x) =>
          (x.from === e.from && x.to === e.to) ||
          (!!e.branch && x.from === e.from && x.branch === e.branch),
      )
        ? all
        : [...all, e],
    kept,
  );
  return { ...spec, nodes: spec.nodes.filter((x) => x.id !== id), edges };
}

/**
 * Why `from` can't lead to `to`, in words; undefined when it can: the
 * trigger has no step before it, no step leads to itself or to one before
 * it (no cycle), two steps are joined once, and a condition checks exactly
 * one step.
 */
export function cannotConnect(
  spec: AutomationSpec,
  from: string,
  to: string,
): string | undefined {
  const a = nodeOf(spec, from),
    b = nodeOf(spec, to);
  if (!a || !b) return "That step is gone";
  if (b.kind === "trigger") return "Nothing comes before the trigger";
  if (from === to) return "A step can’t lead to itself";
  if (spec.edges.some((e) => e.from === from && e.to === to))
    return "They are joined already";
  if (reaches(spec, to, from))
    return "That would make a loop: steps only go forward";
  if (b.kind === "condition" && edgesInto(spec, to).length)
    return "A condition checks exactly one step before it";
  if (a.kind === "condition" && !freeBranch(spec, from))
    return "Both of its branches lead somewhere already";
  return undefined;
}

/** The spec with `from` leading to `to`, along the condition's free branch (or `branch`). */
export function connect(
  spec: AutomationSpec,
  from: string,
  to: string,
  branch?: "true" | "false",
): AutomationSpec {
  if (cannotConnect(spec, from, to)) return spec;
  const condition = nodeOf(spec, from)!.kind === "condition";
  const b = condition
    ? branch && !spec.edges.some((e) => e.from === from && e.branch === branch)
      ? branch
      : freeBranch(spec, from)
    : undefined;
  return {
    ...spec,
    edges: [...spec.edges, { from, to, ...(b && { branch: b }) }],
  };
}

/** The spec without the edge from `from` to `to`. */
export const disconnect = (
  spec: AutomationSpec,
  from: string,
  to: string,
): AutomationSpec => ({
  ...spec,
  edges: spec.edges.filter((e) => !(e.from === from && e.to === to)),
});

/**
 * The spec without what a canvas deleted at once: its steps, each removed as
 * `removeStep` does (so the steps after them stay joined), then the joins
 * cut between steps that remain. A canvas hands a deleted step's joins over
 * with it; cut first, the step would leave nothing to join.
 *
 * @example
 * // trigger → ask → mail, the ask step deleted with its two joins:
 * removeSelection(spec, ["ask"], [{ from: "start", to: "ask" }, { from: "ask", to: "mail" }]);
 * // trigger → mail
 */
export function removeSelection(
  spec: AutomationSpec,
  steps: string[],
  joins: { from: string; to: string }[],
): AutomationSpec {
  const gone = new Set(steps);
  return joins
    .filter((j) => !gone.has(j.from) && !gone.has(j.to))
    .reduce(
      (s, j) => disconnect(s, j.from, j.to),
      steps.reduce((s, id) => removeStep(s, id), spec),
    );
}

/** The spec with a node's config replaced (its kind kept). */
export const setConfig = (
  spec: AutomationSpec,
  id: string,
  config: AutomationNodeConfig,
): AutomationSpec => ({
  ...spec,
  nodes: spec.nodes.map((n) =>
    n.id === id && n.kind === config.kind
      ? ({ ...n, config: config.config } as AutomationNode)
      : n,
  ),
});

/** How far apart tidy() sets the steps of a row. */
const COLUMN = 280;

/**
 * The spec laid out anew, top to bottom: each step a row below the
 * furthest step before it (the longest way from the trigger), the steps of
 * a row side by side in the order they stood, centred under the trigger;
 * steps no way from the trigger reaches go in a row of their own last.
 * Only positions change, so it runs the same steps (sameSteps).
 *
 * @example
 * tidy({ v: 1, nodes: [
 *   { id: "start", kind: "trigger", config: { kind: "manual" }, position: { x: 90, y: 7 } },
 *   { id: "tell", kind: "notify", config: { message: "" }, position: { x: -40, y: 400 } },
 * ], edges: [{ from: "start", to: "tell" }] }).nodes.map((n) => n.position);
 * // [{ x: 0, y: 0 }, { x: 0, y: 150 }]
 */
export function tidy(spec: AutomationSpec): AutomationSpec {
  const trigger = spec.nodes.find((n) => n.kind === "trigger");
  const reached = new Set(
    trigger
      ? spec.nodes
          .filter((n) => reaches(spec, trigger.id, n.id))
          .map((n) => n.id)
      : [],
  );
  if (trigger) reached.add(trigger.id);
  // Longest way from the trigger, in an order where every step comes
  // after the steps before it (Kahn's): a cycle, which the canvas never
  // makes, leaves its steps unplaced, and they go last.
  const row = new Map<string, number>();
  const into = new Map(
    [...reached].map((id) => [
      id,
      spec.edges.filter((e) => e.to === id && reached.has(e.from)).length,
    ]),
  );
  const ready = [...reached].filter((id) => !into.get(id));
  for (const id of ready) row.set(id, 0);
  while (ready.length) {
    const id = ready.shift()!;
    for (const e of spec.edges.filter(
      (x) => x.from === id && reached.has(x.to),
    )) {
      row.set(e.to, Math.max(row.get(e.to) ?? 0, row.get(id)! + 1));
      into.set(e.to, into.get(e.to)! - 1);
      if (!into.get(e.to)) ready.push(e.to);
    }
  }
  const last =
    Math.max(
      -1,
      ...[...row].filter(([id]) => !into.get(id)).map(([, r]) => r),
    ) + 1;
  const rows = new Map<number, AutomationNode[]>();
  for (const n of spec.nodes) {
    const r = into.get(n.id) ? last : (row.get(n.id) ?? last);
    rows.set(r, [...(rows.get(r) ?? []), n]);
  }
  const placed = new Map<string, { x: number; y: number }>();
  for (const [r, nodes] of rows) {
    const sorted = [...nodes].sort(
      (a, b) => a.position.x - b.position.x || a.id.localeCompare(b.id),
    );
    sorted.forEach((n, i) =>
      placed.set(n.id, {
        x: Math.round((i - (sorted.length - 1) / 2) * COLUMN),
        y: r * ROW,
      }),
    );
  }
  return {
    ...spec,
    nodes: spec.nodes.map((n) => ({ ...n, position: placed.get(n.id)! })),
  };
}

/** The spec with a node moved on the canvas. */
export const moveNode = (
  spec: AutomationSpec,
  id: string,
  position: { x: number; y: number },
): AutomationSpec => ({
  ...spec,
  nodes: spec.nodes.map((n) =>
    n.id === id
      ? {
          ...n,
          position: {
            x: Math.round(
              Math.max(-LIMITS.position, Math.min(LIMITS.position, position.x)),
            ),
            y: Math.round(
              Math.max(-LIMITS.position, Math.min(LIMITS.position, position.y)),
            ),
          },
        }
      : n,
  ),
});

/** A trigger of another kind, keeping what it can of the one before (its zone). */
export function triggerOfKind(
  kind: AutomationTrigger["kind"],
  before: AutomationTrigger,
  timeZone: string,
): AutomationTrigger {
  const zone = "time_zone" in before ? before.time_zone : timeZone;
  switch (kind) {
    case "schedule":
      return { kind, every: "day", at: "09:00", time_zone: zone };
    case "sync_finished":
      return { kind, connector_id: "", outcome: "succeeded" };
    case "load_ready":
      return { kind, ontology_id: "" };
    case "document_arrived":
      return { kind, set_id: "" };
    case "generation_finished":
      return { kind, ontology_id: "", outcome: "succeeded" };
    case "manual":
      return { kind };
  }
}

// ---------------------------------------------------------------- runs

/**
 * A step's output (outputJson) as the page may show it: each field checked,
 * the rest dropped. Its text is the workspace's or a model's: untrusted.
 *
 * @example
 * parseOutput('{"summary":"Done.","answer":true,"count":"x"}'); // { summary: "Done.", answer: true }
 */
export function parseOutput(json?: string): AutomationNodeOutput {
  let x: unknown;
  try {
    x = JSON.parse(json || "{}");
  } catch {
    return {};
  }
  if (!isRecord(x)) return {};
  return {
    ...(typeof x.summary === "string" && x.summary && { summary: x.summary }),
    ...(typeof x.answer === "boolean" && { answer: x.answer }),
    ...(Number.isSafeInteger(x.count) && { count: x.count as number }),
    ...(typeof x.branch === "boolean" && { branch: x.branch }),
    ...(x.result !== undefined && { result: x.result }),
    ...(Number.isSafeInteger(x.pending) &&
      (x.pending as number) > 0 && { pending: x.pending as number }),
  };
}

/** What fired a run (triggerEventJson), read without trust; undefined when it doesn't read. */
export function parseEvent(json?: string): AutomationTriggerEvent | undefined {
  let x: unknown;
  try {
    x = JSON.parse(json || "");
  } catch {
    return undefined;
  }
  if (
    !isRecord(x) ||
    typeof x.kind !== "string" ||
    !Object.hasOwn(TRIGGER_SHAPES, x.kind)
  )
    return undefined;
  return {
    kind: x.kind as AutomationTrigger["kind"],
    at: typeof x.at === "string" ? x.at : "",
    ...(typeof x.outcome === "string" && { outcome: x.outcome }),
    ...(Number.isSafeInteger(x.count) && { count: x.count as number }),
    ...(Number.isSafeInteger(x.rows) && { rows: x.rows as number }),
    ...(typeof x.by_user_id === "string" && { by_user_id: x.by_user_id }),
  };
}

/**
 * What fired a run, in words, from its event and the trigger of the spec
 * it ran (whose target the event's work is).
 */
export function eventText(
  e: AutomationTriggerEvent | undefined,
  trigger: AutomationTrigger | undefined,
  names: Names,
): string {
  if (!e) return "Started";
  switch (e.kind) {
    case "manual":
      return "Run by hand";
    case "schedule":
      return "On its schedule";
    case "sync_finished":
      return `A sync of ${quoted(trigger?.kind === "sync_finished" ? names.connector(trigger.connector_id) : undefined, "its connection")} ${e.outcome === "failed" ? "failed" : "succeeded"}`;
    case "load_ready":
      return `A load of ${quoted(trigger?.kind === "load_ready" ? names.ontology(trigger.ontology_id) : undefined, "its ontology")} is ${e.outcome === "partial" ? "partly ready" : "ready"}`;
    case "document_arrived": {
      const n = e.count ?? 1;
      return `${n === 1 ? "A document" : `${n} documents`} arrived in ${quoted(trigger?.kind === "document_arrived" ? names.set(trigger.set_id) : undefined, "its set")}`;
    }
    case "generation_finished":
      return `A draft of ${quoted(trigger?.kind === "generation_finished" ? names.ontology(trigger.ontology_id) : undefined, "its ontology")} ${e.outcome === "failed" ? "failed" : "finished"}`;
  }
}

/** A step's place in a run: as it ended or runs, or not reached (yet). */
export type StepState = AutomationNodeStatus | "waiting";

/** Each node's state in a run, from the steps that started; the rest are waiting. */
export function stepStates(
  spec: AutomationSpec,
  run: FoundryAutomationRun,
): Map<string, StepState> {
  const started = new Map(
    (run.nodes || []).map((n) => [n.nodeId, n.status ?? "running"] as const),
  );
  return new Map(spec.nodes.map((n) => [n.id, started.get(n.id) ?? "waiting"]));
}

export const isLiveRun = (r?: FoundryAutomationRun | null) =>
  r?.status === "queued" || r?.status === "running";

/** A run's spend in dollars, from its int64 millionths. */
export const runUsd = (micros?: string) => Number(micros || 0) / 1e6;
/**
 * A spend in words: dollars and cents, or under a cent.
 *
 * @example
 * usdText("1500000"); // "$1.50"
 * usdText("1200"); // "under $0.01"
 */
export const usdText = (micros?: string) => {
  const usd = runUsd(micros);
  return usd > 0 && usd < 0.005 ? "under $0.01" : `$${usd.toFixed(2)}`;
};

// ---------------------------------------------------------------- automations

/**
 * Why an automation isn't running, in words for whoever reads it (`mine`:
 * its owner); undefined while active.
 */
export function stoppedText(
  a: FoundryAutomation,
  mine: boolean,
): string | undefined {
  switch (a.status) {
    case "needs_approval":
      return mine
        ? "It waits for your approval: nothing runs until you approve its steps."
        : "It waits for its owner’s approval: nothing runs until they approve its steps.";
    case "paused":
      return a.pausedReason === "failures"
        ? `Paused after ${a.failures || 3} failed runs in a row. ${mine ? "Fix what failed, then resume it." : "Its owner resumes it once what failed is fixed."}`
        : a.pausedReason === "owner_lost_access"
          ? "Paused: its owner can no longer act in this workspace."
          : mine
            ? "Paused: it runs only when you run it by hand."
            : "Paused: it runs only when its owner runs it by hand.";
    default:
      return undefined;
  }
}

/** The automations' reason for refusing a call (its ErrorInfo), if it gave one of its own. */
export function automationRefusal(
  error: unknown,
): AutomationRefusal | undefined {
  if (!(error instanceof ApiError)) return undefined;
  for (const d of error.details || []) {
    const reason = isRecord(d) ? d.reason : undefined;
    const known = AUTOMATION_REFUSALS.find((r) => r === reason);
    if (known) return known;
  }
  return undefined;
}

/**
 * Whether automations aren't available here: off for this Foundry
 * (FOUNDRY_AUTOMATIONS_DISABLED), or a WFE that serves no automations'
 * routes yet (404, or 501 for an RPC it doesn't implement). The section
 * then says so calmly, as no error.
 *
 * @example
 * automationsUnavailable(new ApiError("Not Found", 404)); // true
 * automationsUnavailable(new ApiError("boom", 500)); // false
 */
export const automationsUnavailable = (error: unknown) =>
  automationRefusal(error) === "FOUNDRY_AUTOMATIONS_DISABLED" ||
  (error instanceof ApiError && (error.status === 404 || error.status === 501));

// ---------------------------------------------------------------- ontologies' impact

// How much each ontology an automation uses helped its runs: Foundry
// records each run that uses one in a workflow of it (the backend's
// automation workflows), and each run's assessment gives a verdict.

/** Verdicts that say the ontology helped. */
const HELPED = new Set(["essential", "helpful"]);

/**
 * How much an ontology helped an automation's runs, in a sentence.
 *
 *   impactText({ ontologyId: "o", runs: "4", finishedRuns: "4",
 *     verdicts: [{ verdict: "essential", runs: "2" }, { verdict: "unused", runs: "1" }] })
 *   // "Helped 2 of the 3 runs assessed."
 */
export function impactText(o: FoundryAutomationOntology): string {
  if (o.untracked === "workflows_full")
    return "Its runs aren’t recorded: the ontology holds 100 workflows, the most it may. Delete one of its workflows and the next run is recorded.";
  const runs = Number(o.runs || 0),
    assessed = (o.verdicts || []).reduce((n, v) => n + Number(v.runs || 0), 0),
    helped = (o.verdicts || [])
      .filter((v) => HELPED.has(v.verdict))
      .reduce((n, v) => n + Number(v.runs || 0), 0);
  if (!runs) return "No run has used it yet: its first is recorded here.";
  if (!assessed)
    return runs === 1
      ? "One run used it; it isn’t assessed yet."
      : `${runs.toLocaleString("en-US")} runs used it; none is assessed yet.`;
  return `Helped ${helped.toLocaleString("en-US")} of the ${assessed === 1 ? "run" : `${assessed.toLocaleString("en-US")} runs`} assessed.`;
}

/** Where a run's assessment of an ontology stands, for its tag: a verdict once done. */
export type RunImpact =
  | { kind: "verdict"; verdict: string }
  | {
      kind: "pending" | "open" | "unassessed" | "failed" | "skipped";
      text: string;
    };

/**
 * How much an ontology helped one run, as its page tags it.
 *
 *   runImpact({ ontologyId: "o", workflowId: "w", workflowRunId: "r", status: "finished", assessmentStatus: "done", verdict: "helpful" })
 *   // { kind: "verdict", verdict: "helpful" }
 */
export function runImpact(o: FoundryAutomationRunOntology): RunImpact {
  if (o.status !== "finished")
    return { kind: "open", text: "Recording while the run goes" };
  switch (o.assessmentStatus) {
    case "done":
      return o.verdict
        ? { kind: "verdict", verdict: o.verdict }
        : { kind: "failed", text: "Assessed, without a verdict" };
    case "queued":
    case "running":
      return { kind: "pending", text: "Being assessed" };
    case "failed":
      return { kind: "failed", text: "Its assessment failed" };
    case "skipped":
      return {
        kind: "skipped",
        text: "Not assessed: the month’s budget is spent",
      };
    default:
      return { kind: "unassessed", text: "Not assessed" };
  }
}

/** Whether any of a run's ontologies is still to be assessed: its page reads it again until each is. */
export const assessing = (r?: FoundryAutomationRun) =>
  !!r?.ontologies?.some(
    (o) =>
      o.status !== "finished" ||
      o.assessmentStatus === "queued" ||
      o.assessmentStatus === "running",
  );

/**
 * How long after a run ends an automation's page reads it again: its
 * ontologies' assessments come in by then.
 */
export const ASSESSED_WITHIN_MS = 60_000;
