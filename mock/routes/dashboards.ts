import { randomBytes } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type {
  AddDashboardQuestionVisualRequest,
  CreateDashboardRequest,
  DashboardResponse,
  DashboardSnapshot,
  FoundryDashboard,
  ListDashboardsResponse,
  PinDashboardRequest,
  PreviewDashboardTileRequest,
  PreviewDashboardTileResponse,
  PublishDashboardRequest,
  ReadFoundryGraphResponse,
  UpdateDashboardRequest,
} from "../../src/shared/wfe";
import { isRecord } from "../../src/shared/json";
import { contractErrors } from "../agent-contract";
import {
  fail,
  id,
  now,
  ok,
  type MockReply,
  type MockRequest,
  type MockRoute,
} from "../http";
import { LOCAL_TOKEN, config, isReply, membership, onReset } from "../state";
import { foundryPrecondition as refused } from "./ai";
import {
  currentLoadId,
  ontologyHasProfiles,
  ontologyLoad,
  purgeLoaded,
  readLoad,
} from "./loads";
import { ontologyById } from "./ontologies";
import { storedQuestion } from "./questions";
import { manages, queryReads } from "./workspaces";

// Dashboards, as the backend serves them (backend/api/dashboards.go, its
// spec backend/dashboard): tiles of typed queries over ontologies' loads,
// which every member lists and reads, each tile through the load's :read as
// the viewer (./loads.ts readLoad). A grantee lists and reads only the
// pinned ones. A member creates dashboards and changes and deletes their own
// while unpinned; owners and admins change and delete any, pin them (20 at
// most), and publish a snapshot at a public link, read as themselves, which
// the foundry-events door serves: the mock serves it at
// GET /v1/shared-dashboards/{token} (handleSharedDashboard), with no
// session. A workspace keeps 200.
// Edits, pins and publishes name the revision read; another is refused
// FOUNDRY_DASHBOARD_CHANGED. A member deletes no published dashboard. A
// snapshot keeps each query's shape only (dashboard.PublicQuery); a publish
// is refused while a tile's ontology is being purged of People enrichment's
// profiles, and turning enrichment off ends the links of the ontologies
// whose loads hold them (endEnrichedShares).
// - `POST /__mock/config {"dashboardsPublic": false}`: no public links, as a
//   local build (the default under MOCK_LOCAL_TOKEN) or a stamp with
//   foundry.dashboards.public_enabled off: publishing is refused
//   FOUNDRY_DASHBOARDS_PUBLIC_OFF and every link shows nothing.

type Knobs = { dashboardsPublic?: boolean };
const knobs = config as typeof config & Knobs;
/** Whether public links are offered now (the Assistant prepares publishing only then). */
export const publicLinks = () => knobs.dashboardsPublic ?? !LOCAL_TOKEN;

const MAX_DASHBOARDS = 200,
  MAX_PINNED = 20,
  MAX_TILES = 20,
  MAX_ROW = 3,
  MAX_TITLE = 80,
  MAX_QUERY_BYTES = 3 << 10,
  MAX_NAME = 120,
  MAX_DESCRIPTION = 1000,
  MAX_TOKEN = 64,
  MAX_OUTPUT_BYTES = 512 << 10,
  MAX_SNAPSHOT_BYTES = 2 << 20,
  MAX_STRING = 1000;

const TILE_ID = /^[a-z0-9][a-z0-9_-]{0,31}$/,
  ID = /^[A-Za-z0-9_-]{1,128}$/,
  CONTROL = /\p{Cc}/u,
  EMPTY_SPEC = '{"v":1,"tiles":[],"layout":{"rows":[]}}';
/** The kinds a tile draws, as an Analyst visual names them (./agent.ts too). */
export const VISUALS = ["table", "bar", "line", "figure"];

type Tile = {
  id: string;
  title: string;
  ontology_id: string;
  load: { kind: "current" } | { kind: "pinned"; load_id: string };
  query: unknown;
  visual: { kind?: string };
};
type Spec = { tiles: Tile[]; rows: string[][] };

type Stored = {
  id: string;
  workspaceId: string;
  name: string;
  description: string;
  spec: Spec;
  revision: number;
  createdBy: string;
  updatedBy: string;
  clientToken: string;
  pinnedAt: string | null;
  pinnedBy: string;
  createdAt: string;
  updatedAt: string;
  share: {
    token: string;
    snapshot: DashboardSnapshot;
    specRevision: number;
    publishedBy: string;
    publishedAt: string;
    firstPublishedAt: string;
    // The ontologies its tiles read: deleting one ends the link.
    ontologyIds: string[];
  } | null;
};

const dashboards = new Map<string, Stored>();
onReset(() => {
  dashboards.clear();
  delete knobs.dashboardsPublic;
});

/** JSON with every object's keys sorted, as the backend writes a query. */
function sorted(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(sorted);
  if (!isRecord(v)) return v;
  return Object.fromEntries(
    Object.keys(v)
      .sort()
      .map((k) => [k, sorted(v[k])]),
  );
}

/** A tile as the backend reads one, or why it isn't one. */
function tileOf(raw: unknown, i: number): Tile | string {
  const keys = ["id", "title", "ontology_id", "load", "query", "visual"];
  if (!isRecord(raw) || Object.keys(raw).some((k) => !keys.includes(k)))
    return `tile ${i}: the tile is not {id, title, ontology_id, load, query, visual}`;
  const id = String(raw.id ?? ""),
    title = String(raw.title ?? "").trim(),
    ontologyId = String(raw.ontology_id ?? "");
  if (!TILE_ID.test(id))
    return `tile ${i}: id "${id}" must be 1 to 32 lowercase letters, digits, dashes or underscores, starting with a letter or digit`;
  if (!title || [...title].length > MAX_TITLE || CONTROL.test(title))
    return `tile ${i}: tile ${id}'s title must be 1 to ${MAX_TITLE} characters, with no control characters`;
  if (!ID.test(ontologyId))
    return `tile ${i}: tile ${id}'s ontology_id is not an id`;
  const load = isRecord(raw.load) ? raw.load : {};
  let tileLoad: Tile["load"];
  if (load.kind === "current" && load.load_id === undefined)
    tileLoad = { kind: "current" };
  else if (load.kind === "pinned" && ID.test(String(load.load_id ?? "")))
    tileLoad = { kind: "pinned", load_id: String(load.load_id) };
  else
    return `tile ${i}: tile ${id}'s load is {kind: current} or {kind: pinned, load_id}`;
  if (!isRecord(raw.query))
    return `tile ${i}: tile ${id}'s query: a query is a JSON object: the typed Query of Foundry's graph reads`;
  const errors = contractErrors("read_request#/$defs/Query", raw.query);
  if (errors.length)
    return `tile ${i}: tile ${id}'s query: not a Query of the read contract (${errors[0]})`;
  const query = sorted(raw.query);
  if (new TextEncoder().encode(JSON.stringify(query)).length > MAX_QUERY_BYTES)
    return `tile ${i}: tile ${id}'s query: a query is at most ${MAX_QUERY_BYTES} bytes`;
  const visual = isRecord(raw.visual) ? raw.visual : {};
  if (visual.kind !== undefined && !VISUALS.includes(String(visual.kind)))
    return `tile ${i}: tile ${id}'s visual kind is "${String(visual.kind)}"; it is table, bar, line or figure, or left out`;
  return {
    id,
    title,
    ontology_id: ontologyId,
    load: tileLoad,
    query,
    visual: visual.kind === undefined ? {} : { kind: String(visual.kind) },
  };
}

/** A spec as the backend's dashboard.ParseSpec reads it, or why it isn't one. */
function specOf(raw: string): Spec | string {
  let v: unknown;
  try {
    v = JSON.parse(raw || EMPTY_SPEC);
  } catch {
    return "the spec is not {v, tiles, layout}";
  }
  if (
    !isRecord(v) ||
    Object.keys(v).some((k) => !["v", "tiles", "layout"].includes(k))
  )
    return "the spec is not {v, tiles, layout}";
  if (v.v !== 1)
    return `the spec's v is ${String(v.v)}; this Foundry reads v 1`;
  const raws = Array.isArray(v.tiles) ? v.tiles : [];
  const rows =
    isRecord(v.layout) && Array.isArray(v.layout.rows) ? v.layout.rows : [];
  const tiles: Tile[] = [];
  for (const [i, t] of raws.entries()) {
    const tile = tileOf(t, i);
    if (typeof tile === "string") return tile;
    tiles.push(tile);
  }
  return layoutOf(tiles, rows);
}

/** tiles laid out in rows, as dashboard.NewSpec checks them. */
function layoutOf(tiles: Tile[], rows: unknown[]): Spec | string {
  if (tiles.length > MAX_TILES)
    return `a dashboard has at most ${MAX_TILES} tiles, not ${tiles.length}`;
  const placed = new Map<string, boolean>();
  for (const t of tiles) {
    if (placed.has(t.id)) return `two tiles have the id ${t.id}`;
    placed.set(t.id, false);
  }
  const out: string[][] = [];
  for (const [i, row] of rows.entries()) {
    if (!Array.isArray(row) || !row.length || row.length > MAX_ROW)
      return `layout row ${i} has ${Array.isArray(row) ? row.length : 0} tiles; a row has 1 to ${MAX_ROW}`;
    for (const tileId of row) {
      if (!placed.has(String(tileId)))
        return `layout row ${i} names "${String(tileId)}", which is no tile`;
      if (placed.get(String(tileId)))
        return `layout places tile ${String(tileId)} twice`;
      placed.set(String(tileId), true);
    }
    out.push(row.map(String));
  }
  const left = tiles.find((t) => !placed.get(t.id));
  return left
    ? `layout leaves out tile ${left.id}: each tile is in exactly one row`
    : { tiles, rows: out };
}

/** The spec as the backend writes it. */
const specJson = (s: Spec) =>
  JSON.stringify({ v: 1, tiles: s.tiles, layout: { rows: s.rows } });

/**
 * Each tile spec points somewhere new (a tile stored doesn't read the same
 * ontology and load: dashboard.Spec.NewTargets) reads a live ontology of the
 * workspace and, pinned, a load of it. A stored tile whose ontology or load
 * went since blocks no edit.
 */
function targetsOf(
  ws: string,
  spec: Spec,
  stored: Spec = { tiles: [], rows: [] },
): string | undefined {
  for (const t of spec.tiles) {
    const kept = stored.tiles.find((x) => x.id === t.id);
    if (
      kept &&
      kept.ontology_id === t.ontology_id &&
      JSON.stringify(kept.load) === JSON.stringify(t.load)
    )
      continue;
    const o = ontologyById(t.ontology_id);
    if (!o || o.workspaceId !== ws)
      return `tile ${t.id} reads an ontology this workspace doesn't have`;
    if (t.load.kind === "pinned" && !ontologyLoad(o.id, t.load.load_id))
      return `tile ${t.id} pins a load ${o.name} doesn't have`;
  }
  return undefined;
}

/**
 * Ends the public links of the workspace's dashboards whose tiles read an
 * ontology whose loads hold People enrichment's profiles, as turning
 * enrichment off does (./enrichment.ts).
 */
export function endEnrichedShares(workspaceId: string) {
  for (const d of dashboards.values())
    if (
      d.workspaceId === workspaceId &&
      d.share?.ontologyIds.some((o) => ontologyHasProfiles(o))
    )
      d.share = null;
}

function caller(req: MockRequest) {
  const m = membership(req.params.workspaceId!, req.principal.user.id);
  if (isReply(m)) return m;
  return { m, grantee: !!req.grantee, manager: manages(m) };
}
type Caller = Exclude<ReturnType<typeof caller>, MockReply>;
const isCaller = (x: Caller | MockReply): x is Caller => "m" in x;

const mayEdit = (c: Caller, d: Stored) =>
  !c.grantee &&
  (c.manager || (d.createdBy === c.m.userId && d.pinnedAt === null));

function toDashboard(
  d: Stored,
  c: Caller,
  withSpec: boolean,
): FoundryDashboard {
  return {
    id: d.id,
    name: d.name,
    description: d.description,
    specJson: withSpec ? specJson(d.spec) : "",
    revision: String(d.revision),
    createdByUserId: d.createdBy,
    updatedByUserId: d.updatedBy,
    pinned: d.pinnedAt !== null,
    pinnedAt: d.pinnedAt,
    pinnedByUserId: d.pinnedBy,
    createdAt: d.createdAt,
    updatedAt: d.updatedAt,
    share: d.share && {
      token: !c.grantee && c.manager ? d.share.token : "",
      specRevision: String(d.share.specRevision),
      publishedAt: d.share.publishedAt,
      firstPublishedAt: d.share.firstPublishedAt,
      publishedByUserId: d.share.publishedBy,
    },
    canEdit: mayEdit(c, d),
    canPin: !c.grantee && c.manager,
    canPublish: !c.grantee && c.manager && publicLinks(),
    canDelete: mayEdit(c, d) && (!d.share || c.manager),
    canUnpublish: !c.grantee && c.manager && !!d.share,
  };
}

function visible(req: MockRequest, c: Caller): Stored | MockReply {
  const d = dashboards.get(req.params.id!);
  return d && d.workspaceId === c.m.workspaceId && (!c.grantee || d.pinnedAt)
    ? d
    : fail("NOT_FOUND", "dashboard not found");
}
const isStored = (x: Stored | MockReply): x is Stored => "spec" in x;

const reply = (d: Stored, c: Caller) =>
  ok({ dashboard: toDashboard(d, c, true) } satisfies DashboardResponse);

function nameOf(name: string, description: string) {
  const n = name.trim(),
    d = description.trim();
  if (!n || [...n].length > MAX_NAME || CONTROL.test(n))
    return fail(
      "INVALID_ARGUMENT",
      `name must be 1 to ${MAX_NAME} characters, with no control characters`,
    );
  if ([...d].length > MAX_DESCRIPTION)
    return fail(
      "INVALID_ARGUMENT",
      `description must be at most ${MAX_DESCRIPTION} characters`,
    );
  return { name: n, description: d };
}

/** An edit at expected, as c: the store's refusals. */
function editable(
  c: Caller,
  d: Stored,
  expected: number,
): MockReply | undefined {
  if (!mayEdit(c, d))
    return fail(
      "PERMISSION_DENIED",
      d.createdBy !== c.m.userId
        ? "only an owner or admin changes another person's dashboard"
        : "the dashboard is pinned for the whole workspace: only an owner or admin changes it now",
    );
  if (expected !== d.revision)
    return refused(
      c.m.workspaceId,
      "FOUNDRY_DASHBOARD_CHANGED",
      `the dashboard changed since revision ${expected}: it is at ${d.revision} now; read it again`,
    );
  return undefined;
}

/**
 * Reads tiles, all of one ontology, as req's caller through ./loads.ts
 * readLoad, one read a load: each tile's outcome by id, and the day read as
 * of; or the refusal.
 */
function readTiles(
  req: MockRequest,
  c: Caller,
  tiles: Tile[],
):
  | { loadId: string; asOf: string; outcomes: Map<string, unknown> }[]
  | MockReply {
  const ontologyId = tiles[0]!.ontology_id,
    o = ontologyById(ontologyId);
  if (!o || o.workspaceId !== c.m.workspaceId)
    return fail("NOT_FOUND", "ontology not found");
  if (!queryReads(c.m))
    return fail(
      "PERMISSION_DENIED",
      `you can't read the database of ${o.name}`,
    );
  const byLoad = new Map<string, Tile[]>();
  for (const t of tiles) {
    const loadId =
      t.load.kind === "pinned" ? t.load.load_id : currentLoadId(ontologyId);
    if (!loadId)
      return refused(
        c.m.workspaceId,
        "FOUNDRY_DASHBOARD_NOT_LOADED",
        `${o.name} has no current load to read: load a version of it first`,
      );
    byLoad.set(loadId, [...(byLoad.get(loadId) || []), t]);
  }
  const out = [];
  for (const [loadId, group] of byLoad) {
    const r = readLoad(
      { ...req, params: { workspaceId: c.m.workspaceId, ontologyId, loadId } },
      JSON.stringify(group.map((t) => ({ query: t.query }))),
    );
    if (r.status !== 200) return r;
    const result = JSON.parse(
      (r.body as ReadFoundryGraphResponse).resultJson || "{}",
    ) as {
      as_of: string;
      results: unknown[];
    };
    out.push({
      loadId,
      asOf: result.as_of,
      outcomes: new Map(group.map((t, i) => [t.id, result.results[i]])),
    });
  }
  return out;
}

/**
 * A query's echo as anyone may see it (dashboard.PublicQuery): its start's
 * type, each step's link, direction and type, and what it returns; nothing
 * it tested (start.ids, start.text, where, filters).
 */
function publicQuery(
  echo: unknown,
): DashboardSnapshot["tiles"][number]["query"] {
  const q = (isRecord(echo) ? echo : {}) as {
    start?: { type?: string };
    steps?: { via: string; dir: "out" | "in"; to: string }[];
    return?: DashboardSnapshot["tiles"][number]["query"]["return"];
  };
  return {
    start: { type: String(q.start?.type ?? "") },
    steps: (q.steps || []).map(({ via, dir, to }) => ({ via, dir, to })),
    return: q.return!,
  };
}

/** A query's answer as anyone may see it (dashboard.Public). */
function publicOutput(answer: unknown): unknown {
  const cut = (v: unknown): unknown => {
    if (typeof v === "string")
      return [...v].length <= MAX_STRING
        ? v
        : [...v].slice(0, MAX_STRING - 1).join("") + "…";
    if (Array.isArray(v)) return v.map(cut);
    if (isRecord(v))
      return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, cut(x)]));
    return v;
  };
  if (!isRecord(answer) || answer.kind !== "rows") return cut(answer);
  const entity = (c: unknown) =>
    isRecord(c) &&
    Object.keys(c).length === 3 &&
    ["type", "id", "name"].every((k) => typeof c[k] === "string");
  return cut({
    ...answer,
    rows: (answer.rows as { cells: unknown[] }[]).map((row) => ({
      cells: row.cells.map((c) => {
        if (!entity(c)) return c;
        const e = c as { type: string; name: string };
        return { type: e.type, name: e.name };
      }),
      edges: [],
    })),
  });
}

const BASE = "/v1/workspaces/{workspaceId}/foundry/dashboards" as const;

export const dashboardRoutes: MockRoute[] = [
  {
    method: "GET",
    path: BASE,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const listed = [...dashboards.values()]
        .filter(
          (d) =>
            d.workspaceId === c.m.workspaceId && (!c.grantee || d.pinnedAt),
        )
        .sort(
          (a, b) =>
            Number(!a.pinnedAt) - Number(!b.pinnedAt) ||
            (a.pinnedAt || "").localeCompare(b.pinnedAt || "") ||
            b.updatedAt.localeCompare(a.updatedAt),
        );
      return ok({
        dashboards: listed.map((d) => toDashboard(d, c, false)),
        canCreate: !c.grantee,
      } satisfies ListDashboardsResponse);
    },
  },
  {
    method: "POST",
    path: BASE,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const ws = c.m.workspaceId,
        body = (req.body || {}) as Partial<CreateDashboardRequest>,
        token = body.clientToken || "";
      if (!token || token.length > MAX_TOKEN)
        return fail(
          "INVALID_ARGUMENT",
          `client_token is required, at most ${MAX_TOKEN} characters`,
        );
      const named = nameOf(body.name || "", body.description || "");
      if ("status" in named) return named;
      const spec = specOf(body.specJson || "");
      if (typeof spec === "string") return fail("INVALID_ARGUMENT", spec);
      const target = targetsOf(ws, spec);
      if (target) return fail("INVALID_ARGUMENT", target);
      const mine = [...dashboards.values()].filter((d) => d.workspaceId === ws);
      const again = mine.find(
        (d) => d.createdBy === c.m.userId && d.clientToken === token,
      );
      if (again) return reply(again, c);
      if (mine.length >= MAX_DASHBOARDS)
        return refused(
          ws,
          "FOUNDRY_DASHBOARD_LIMIT",
          `the workspace has ${mine.length} dashboards, as many as it may: delete one first`,
        );
      const at = now();
      const d: Stored = {
        id: id("dash"),
        workspaceId: ws,
        ...named,
        spec,
        revision: 1,
        createdBy: c.m.userId,
        updatedBy: c.m.userId,
        clientToken: token,
        pinnedAt: null,
        pinnedBy: "",
        createdAt: at,
        updatedAt: at,
        share: null,
      };
      dashboards.set(d.id, d);
      return reply(d, c);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{id}`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const d = visible(req, c);
      return isStored(d) ? reply(d, c) : d;
    },
  },
  {
    method: "PATCH",
    path: `${BASE}/{id}`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const body = (req.body || {}) as Partial<UpdateDashboardRequest>,
        expected = Number(body.expectedRevision ?? 0);
      if (
        expected < 1 ||
        (body.name === undefined &&
          body.description === undefined &&
          body.specJson === undefined)
      )
        return fail(
          "INVALID_ARGUMENT",
          "expected_revision and a change of name, description or spec_json are required",
        );
      const d = visible(req, c);
      if (!isStored(d)) return d;
      const named = nameOf(
        body.name ?? d.name,
        body.description ?? d.description,
      );
      if ("status" in named) return named;
      let spec = d.spec;
      if (body.specJson !== undefined) {
        const parsed = specOf(body.specJson);
        if (typeof parsed === "string") return fail("INVALID_ARGUMENT", parsed);
        const target = targetsOf(c.m.workspaceId, parsed, d.spec);
        if (target) return fail("INVALID_ARGUMENT", target);
        spec = parsed;
      }
      const refusal = editable(c, d, expected);
      if (refusal) return refusal;
      Object.assign(d, named, {
        spec,
        revision: d.revision + 1,
        updatedBy: c.m.userId,
        updatedAt: now(),
      });
      return reply(d, c);
    },
  },
  {
    method: "DELETE",
    path: `${BASE}/{id}`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const d = visible(req, c);
      if (!isStored(d)) return d;
      if (!mayEdit(c, d))
        return fail(
          "PERMISSION_DENIED",
          "only an owner or admin deletes another person's dashboard, or a pinned one",
        );
      if (d.share && !c.manager)
        return fail(
          "PERMISSION_DENIED",
          "the dashboard is published at a public link: only an owner or admin deletes it",
        );
      dashboards.delete(d.id);
      return ok({});
    },
  },
  {
    method: "POST",
    path: `${BASE}/{id}:pin`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      if (!c.manager)
        return fail(
          "PERMISSION_DENIED",
          "only an owner or admin pins a dashboard for the workspace",
        );
      const body = (req.body || {}) as Partial<PinDashboardRequest>,
        expected = Number(body.expectedRevision ?? 0),
        pinned = !!body.pinned;
      if (expected < 1)
        return fail(
          "INVALID_ARGUMENT",
          "id and expected_revision are required",
        );
      const d = visible(req, c);
      if (!isStored(d)) return d;
      if (d.revision !== expected)
        return refused(
          d.workspaceId,
          "FOUNDRY_DASHBOARD_CHANGED",
          `the dashboard changed since revision ${expected}: it is at ${d.revision} now; read it again`,
        );
      if (pinned && !d.pinnedAt) {
        const count = [...dashboards.values()].filter(
          (x) => x.workspaceId === d.workspaceId && x.pinnedAt,
        ).length;
        if (count >= MAX_PINNED)
          return refused(
            d.workspaceId,
            "FOUNDRY_DASHBOARD_LIMIT",
            `the workspace has ${count} pinned dashboards, as many as it may: unpin one first`,
          );
        Object.assign(d, { pinnedAt: now(), pinnedBy: c.m.userId });
      } else if (!pinned) Object.assign(d, { pinnedAt: null, pinnedBy: "" });
      return reply(d, c);
    },
  },
  {
    method: "POST",
    path: `${BASE}/{id}:add-question-visual`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const body = (req.body ||
          {}) as Partial<AddDashboardQuestionVisualRequest>,
        expected = Number(body.expectedRevision ?? 0);
      if (expected < 1 || !body.evidenceId)
        return fail(
          "INVALID_ARGUMENT",
          "expected_revision and evidence_id are required",
        );
      const d = visible(req, c);
      if (!isStored(d)) return d;
      const o = ontologyById(body.ontologyId || "");
      if (!o || o.workspaceId !== c.m.workspaceId)
        return fail("NOT_FOUND", "ontology not found");
      const q = storedQuestion(c.m.workspaceId, o.id, body.questionId || "");
      if (!q) return fail("NOT_FOUND", "question not found");
      const result = (q.resultJson ? JSON.parse(q.resultJson) : {}) as {
        status?: string;
        answer?: {
          visuals?: {
            evidence_id: string;
            kind?: string | null;
            title?: string | null;
          }[];
        };
        queries?: { evidence_id: string; query: unknown }[];
      };
      if (result.status !== "answered")
        return fail(
          "FAILED_PRECONDITION",
          "the question has no answer to take a result from",
        );
      const recorded = result.queries?.find(
        (x) => x.evidence_id === body.evidenceId,
      );
      if (!recorded)
        return fail(
          "FAILED_PRECONDITION",
          `the question recorded no query result ${body.evidenceId}`,
        );
      const visual = result.answer?.visuals?.find(
        (v) => v.evidence_id === body.evidenceId,
      );
      const title = (s: string) => {
        const t = s
          .replace(/\p{Cc}/gu, " ")
          .split(/\s+/)
          .filter(Boolean)
          .join(" ");
        return [...t].length <= MAX_TITLE
          ? t
          : [...t].slice(0, MAX_TITLE - 1).join("") + "…";
      };
      let n = 1;
      while (d.spec.tiles.some((t) => t.id === `t${n}`)) n += 1;
      const tile = tileOf(
        {
          id: `t${n}`,
          title:
            title(visual?.title || "") || title(q.question) || "Answer",
          ontology_id: o.id,
          load: body.pinLoad
            ? { kind: "pinned", load_id: q.loadId }
            : { kind: "current" },
          query: recorded.query,
          visual:
            visual?.kind && VISUALS.includes(visual.kind)
              ? { kind: visual.kind }
              : {},
        },
        d.spec.tiles.length,
      );
      if (typeof tile === "string") return fail("FAILED_PRECONDITION", tile);
      if (body.pinLoad && !ontologyLoad(o.id, q.loadId))
        return fail(
          "FAILED_PRECONDITION",
          "the question's load was deleted: add its result reading the current load instead",
        );
      const spec = layoutOf(
        [...d.spec.tiles, tile],
        [...d.spec.rows, [tile.id]],
      );
      if (typeof spec === "string") return fail("FAILED_PRECONDITION", spec);
      const refusal = editable(c, d, expected);
      if (refusal) return refusal;
      Object.assign(d, {
        spec,
        revision: d.revision + 1,
        updatedBy: c.m.userId,
        updatedAt: now(),
      });
      return reply(d, c);
    },
  },
  {
    method: "POST",
    path: `${BASE}:preview-tile`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      let raw: unknown;
      try {
        raw = JSON.parse(
          (req.body as Partial<PreviewDashboardTileRequest>)?.tileJson || "",
        );
      } catch {
        return fail(
          "INVALID_ARGUMENT",
          "the tile is not {id, title, ontology_id, load, query, visual}",
        );
      }
      const tile = tileOf(raw, 0);
      if (typeof tile === "string")
        return fail("INVALID_ARGUMENT", tile.replace(/^tile 0: /, ""));
      const target = targetsOf(c.m.workspaceId, {
        tiles: [tile],
        rows: [[tile.id]],
      });
      if (target) return fail("INVALID_ARGUMENT", target);
      const reads = readTiles(req, c, [tile]);
      if (!Array.isArray(reads)) return reads;
      return ok({
        tileJson: JSON.stringify(tile),
        loadId: reads[0]!.loadId,
        asOf: reads[0]!.asOf,
        outcomeJson: JSON.stringify(reads[0]!.outcomes.get(tile.id)),
      } satisfies PreviewDashboardTileResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/{id}:publish`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const ws = c.m.workspaceId,
        expected = Number(
          (req.body as Partial<PublishDashboardRequest>)?.expectedRevision ?? 0,
        );
      if (!c.manager)
        return fail(
          "PERMISSION_DENIED",
          "only an owner or admin publishes a dashboard",
        );
      if (!publicLinks())
        return refused(
          ws,
          "FOUNDRY_DASHBOARDS_PUBLIC_OFF",
          "public dashboard links aren't available here",
        );
      if (expected < 1)
        return fail("INVALID_ARGUMENT", "expected_revision is required");
      const d = visible(req, c);
      if (!isStored(d)) return d;
      if (d.revision !== expected)
        return refused(
          ws,
          "FOUNDRY_DASHBOARD_CHANGED",
          `the dashboard changed since revision ${expected}: it is at ${d.revision} now; read it again`,
        );
      if (!d.spec.tiles.length)
        return refused(
          ws,
          "FOUNDRY_DASHBOARD_EMPTY",
          "the dashboard has no tiles to publish",
        );
      const tileFailed = (t: Tile, why: string) =>
        refused(
          ws,
          "FOUNDRY_DASHBOARD_TILE_FAILED",
          `tile "${t.title}" (${t.id}) can't be read: ${why}`,
        );
      const outputs = new Map<
        string,
        { asOf: string; output: unknown; query: unknown }
      >();
      for (const ontologyId of new Set(
        d.spec.tiles.map((t) => t.ontology_id),
      )) {
        const group = d.spec.tiles.filter((t) => t.ontology_id === ontologyId);
        const o = ontologyById(ontologyId);
        if (!o) return tileFailed(group[0]!, "its ontology was deleted");
        if (!purgeLoaded(ontologyId))
          return refused(
            ws,
            "FOUNDRY_DASHBOARD_TILE_FAILED",
            `People enrichment was turned off, and ${o.name} is being loaded again without its profiles: publish once that load is current`,
          );
        const reads = readTiles(req, c, group);
        if (!Array.isArray(reads)) {
          const reason = ((reads.body as { details?: { reason?: string }[] })
            ?.details || [])[0]?.reason;
          return reason || [403, 429, 503].includes(reads.status)
            ? reads
            : tileFailed(
                group[0]!,
                (reads.body as { message: string }).message,
              );
        }
        for (const r of reads)
          for (const t of group) {
            const outcome = r.outcomes.get(t.id) as
              | {
                  ok?: {
                    kind?: string;
                    query_echo?: unknown;
                    result?: unknown;
                  };
                  error?: { message: string };
                }
              | undefined;
            if (!outcome) continue;
            const answer = outcome.ok;
            if (answer?.kind !== "query" || !answer.result)
              return tileFailed(
                t,
                outcome.error?.message || "its read has no answer",
              );
            outputs.set(t.id, {
              asOf: r.asOf,
              output: publicOutput(answer.result),
              query: publicQuery(answer.query_echo),
            });
          }
      }
      for (const t of d.spec.tiles)
        if (JSON.stringify(outputs.get(t.id)!.output).length > MAX_OUTPUT_BYTES)
          return refused(
            ws,
            "FOUNDRY_DASHBOARD_TOO_LARGE",
            `tile ${t.id}'s output is past ${MAX_OUTPUT_BYTES >> 10} KiB: ask for fewer rows or columns`,
          );
      const at = now();
      const snapshot = {
        v: 1,
        name: d.name,
        description: d.description,
        published_at: at.replace(/\.\d+Z$/, "Z"),
        layout: { rows: d.spec.rows },
        tiles: d.spec.tiles.map((t) => ({
          id: t.id,
          title: t.title,
          visual: t.visual,
          query: outputs.get(t.id)!.query,
          as_of: outputs.get(t.id)!.asOf,
          output: outputs.get(t.id)!.output,
        })),
      } as DashboardSnapshot;
      if (JSON.stringify(snapshot).length > MAX_SNAPSHOT_BYTES)
        return refused(
          ws,
          "FOUNDRY_DASHBOARD_TOO_LARGE",
          `the snapshot is past ${MAX_SNAPSHOT_BYTES >> 10} KiB: show fewer rows`,
        );
      d.share = {
        token: d.share?.token || "hxd_" + randomBytes(32).toString("base64url"),
        snapshot,
        specRevision: d.revision,
        publishedBy: c.m.userId,
        publishedAt: at,
        firstPublishedAt: d.share?.firstPublishedAt || at,
        ontologyIds: [...new Set(d.spec.tiles.map((t) => t.ontology_id))],
      };
      return reply(d, c);
    },
  },
  {
    method: "POST",
    path: `${BASE}/{id}:unpublish`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      if (!c.manager)
        return fail(
          "PERMISSION_DENIED",
          "only an owner or admin unpublishes a dashboard",
        );
      const d = visible(req, c);
      if (!isStored(d)) return d;
      d.share = null;
      return reply(d, c);
    },
  },
];

/**
 * A dashboard of the workspace as the Assistant reads it before preparing
 * an action on it (./agent.ts): its name, revision and state; undefined
 * for one the workspace doesn't have.
 */
export function storedDashboard(workspaceId: string, id: string) {
  const d = dashboards.get(id);
  return d && d.workspaceId === workspaceId
    ? {
        id: d.id,
        name: d.name,
        revision: d.revision,
        pinned: d.pinnedAt !== null,
        published: !!d.share,
        // The revision at its public link, while published.
        publishedRevision: d.share?.specRevision,
        // The ontologies its tiles read, each once.
        ontologyIds: [...new Set(d.spec.tiles.map((t) => t.ontology_id))],
      }
    : undefined;
}

/**
 * A dashboard route's handler by its method and its path after
 * .../foundry/dashboards, for the Assistant's tools and cards, which call
 * it as their person (./agent.ts), as the backend's integration calls its
 * own API. Every dashboard route answers at once.
 */
export function dashboardRoute(method: MockRoute["method"], suffix: string) {
  const route = dashboardRoutes.find(
    (r) => r.method === method && r.path === BASE + suffix,
  );
  if (!route) throw new Error(`no dashboard route ${method} ${suffix}`);
  return route.handle as (req: MockRequest) => MockReply;
}

/**
 * The foundry-events door's GET /v1/shared-dashboards/{token}
 * (services/foundry-events/shared.go), not the WFE's: a dashboard's public
 * snapshot, for the site's page at its link, with no session, readable by
 * any page and never cached. A link of no snapshot, of one whose ontology
 * was since deleted, or with public links off, shows nothing (404 with the
 * door's error body). Returns whether it answered the request.
 */
export function handleSharedDashboard(
  req: IncomingMessage,
  res: ServerResponse,
  url: URL,
): boolean {
  const path = /^\/v1\/shared-dashboards\/([^/]+)$/.exec(url.pathname);
  if (req.method !== "GET" || !path) return false;
  const token = decodeURIComponent(path[1]!);
  const d = [...dashboards.values()].find((x) => x.share?.token === token);
  const live =
    d?.share &&
    publicLinks() &&
    d.share.ontologyIds.every((o) => ontologyById(o));
  res.writeHead(live ? 200 : 404, {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": "*",
    "Cache-Control": "no-store",
    "X-Robots-Tag": "noindex, nofollow",
    "Referrer-Policy": "no-referrer",
  });
  res.end(
    JSON.stringify(
      live
        ? d.share!.snapshot
        : {
            error: {
              code: "not_found",
              message:
                "no dashboard is shared at this link: it may have been unpublished",
            },
          },
    ),
  );
  return true;
}
