import type {
  DashboardLinkExpiry,
  DashboardPublishBlock,
  DashboardRefusal,
  DashboardSpec,
  DashboardTile,
  DashboardTileLoad,
  FoundryDashboard,
  FoundryDashboardShare,
  OntologyQuery,
  QueryOutput,
  VisualKind,
} from "../../../shared/wfe";
import { DASHBOARD_REFUSALS } from "../../../shared/wfe";
import type { Outcome } from "../../../shared/foundry-read";
import { isRecord } from "../../../shared/json";
import { parseOutput, parseQuery } from "../ontology/ask-model";

// Dashboards as pure functions: a spec read without trust, the edits the
// page offers on it (each one a closed case, each keeping the spec's
// rules), which loads its tiles read, what a tile's read gives to draw, why
// the WFE refused a change, how it is shared (its in-app link, and its
// public link's state), and a public snapshot read without trust. The
// spec is the backend's (backend/dashboard, src/shared/wfe.ts
// "Dashboards"): at most 20 tiles, each in exactly one row of 1 to 3.

export const MAX_TILES = 20,
  MAX_ROW = 3,
  MAX_TITLE = 80,
  MAX_NAME = 120,
  MAX_DESCRIPTION = 1000;

/** The visual kinds a tile may ask for; none lets its output's shape decide. */
export const VISUAL_KINDS: readonly VisualKind[] = [
  "table",
  "bar",
  "line",
  "figure",
];

/** A link's token as the door takes it: hxd_ and 32 random bytes, base64url. */
export const SHARE_TOKEN = /^hxd_[A-Za-z0-9_-]{43}$/;

const TILE_ID = /^[a-z0-9][a-z0-9_-]{0,31}$/,
  ID = /^[A-Za-z0-9_-]{1,128}$/,
  CONTROL = /\p{Cc}/u;

/** A spec with no tiles: what a new dashboard holds. */
export const EMPTY_SPEC: DashboardSpec = {
  v: 1,
  tiles: [],
  layout: { rows: [] },
};

/**
 * A tile's title as the backend takes it: 1 to 80 characters once
 * trimmed, no control character; undefined for one it would refuse.
 *
 * @example
 * tileTitle("  Invoices by status "); // "Invoices by status"
 * tileTitle(""); // undefined
 */
export function tileTitle(text: string): string | undefined {
  const t = text.trim();
  return t && [...t].length <= MAX_TITLE && !CONTROL.test(t) ? t : undefined;
}

/**
 * A dashboard's name as the backend takes it: 1 to 120 characters once
 * trimmed, no control character.
 *
 * @example
 * dashboardName(" Revenue "); // "Revenue"
 * dashboardName("a\u0007"); // undefined
 */
export function dashboardName(text: string): string | undefined {
  const t = text.trim();
  return t && [...t].length <= MAX_NAME && !CONTROL.test(t) ? t : undefined;
}

function loadOf(x: unknown): DashboardTileLoad | undefined {
  if (!isRecord(x)) return undefined;
  if (x.kind === "current") return { kind: "current" };
  return x.kind === "pinned" &&
    typeof x.load_id === "string" &&
    ID.test(x.load_id)
    ? { kind: "pinned", load_id: x.load_id }
    : undefined;
}

function tileOf(x: unknown): DashboardTile | undefined {
  if (!isRecord(x) || !isRecord(x.query) || !isRecord(x.visual ?? {}))
    return undefined;
  const id = typeof x.id === "string" ? x.id : "",
    title = typeof x.title === "string" ? tileTitle(x.title) : undefined,
    load = loadOf(x.load),
    kind = (x.visual as Record<string, unknown> | undefined)?.kind;
  if (
    !TILE_ID.test(id) ||
    !title ||
    typeof x.ontology_id !== "string" ||
    !ID.test(x.ontology_id) ||
    !load ||
    (kind !== undefined && !VISUAL_KINDS.includes(kind as VisualKind))
  )
    return undefined;
  return {
    id,
    title,
    ontology_id: x.ontology_id,
    load,
    // Kept as the spec holds it, so an edit sends it back unchanged.
    query: x.query as OntologyQuery,
    visual: kind === undefined ? {} : { kind: kind as VisualKind },
  };
}

/**
 * Whether a spec keeps the backend's rules: at most 20 tiles of distinct
 * ids, each in exactly one row of 1 to 3.
 */
function laidOut(tiles: readonly DashboardTile[], rows: readonly string[][]) {
  const ids = tiles.map((t) => t.id),
    placed = rows.flat();
  return (
    tiles.length <= MAX_TILES &&
    new Set(ids).size === ids.length &&
    rows.every((r) => r.length >= 1 && r.length <= MAX_ROW) &&
    placed.length === ids.length &&
    new Set(placed).size === placed.length &&
    placed.every((id) => ids.includes(id))
  );
}

/**
 * A dashboard's specJson, read without trust: undefined when it isn't a
 * spec the backend keeps (an empty one is a dashboard with no tiles).
 *
 * @example
 * parseSpec("")?.tiles; // []
 * parseSpec('{"v":2,"tiles":[],"layout":{"rows":[]}}'); // undefined
 */
export function parseSpec(json: string): DashboardSpec | undefined {
  if (!json) return EMPTY_SPEC;
  let x: unknown;
  try {
    x = JSON.parse(json);
  } catch {
    return undefined;
  }
  if (
    !isRecord(x) ||
    x.v !== 1 ||
    !Array.isArray(x.tiles) ||
    !isRecord(x.layout) ||
    !Array.isArray(x.layout.rows)
  )
    return undefined;
  const tiles = x.tiles.map(tileOf);
  const rows = x.layout.rows.map((r) =>
    Array.isArray(r) && r.every((id) => typeof id === "string")
      ? (r as string[])
      : undefined,
  );
  if (tiles.some((t) => !t) || rows.some((r) => !r)) return undefined;
  const spec: DashboardSpec = {
    v: 1,
    tiles: tiles as DashboardTile[],
    layout: { rows: rows as string[][] },
  };
  return laidOut(spec.tiles, spec.layout.rows) ? spec : undefined;
}

/** The tiles of a spec in their rows, in order. */
export function tileRows(spec: DashboardSpec): DashboardTile[][] {
  const byId = new Map(spec.tiles.map((t) => [t.id, t]));
  return spec.layout.rows.map((r) => r.map((id) => byId.get(id)!));
}

// ---------------------------------------------------------------- edits

/** Where a tile moves: within its row, or to the row above or below. */
export type Direction = "left" | "right" | "up" | "down";

/** One edit of a dashboard's tiles, as the page offers it. */
export type TileEdit =
  | { kind: "move"; tile: string; to: Direction }
  // Alone in a row of its own, below the row it was in.
  | { kind: "full_width"; tile: string }
  | { kind: "remove"; tile: string }
  | { kind: "title"; tile: string; title: string }
  // null: as its output's shape suits.
  | { kind: "visual"; tile: string; visual: VisualKind | null };

/**
 * The spec with one edit made, keeping its rules (an edit that can't be
 * made, such as a title the backend would refuse or a move past the edge,
 * changes nothing). Moving up joins the row above when it has room; a tile
 * alone in its row otherwise trades places with that row, and one sharing
 * its row takes a row of its own above it. Down is the same, below.
 *
 * @example
 * const spec = { v: 1, tiles: [a, b, c], layout: { rows: [["a", "b"], ["c"]] } };
 * applyEdit(spec, { kind: "move", tile: "c", to: "up" }).layout.rows; // [["a", "b", "c"]]
 * applyEdit(spec, { kind: "full_width", tile: "a" }).layout.rows; // [["a"], ["b"], ["c"]]
 * applyEdit(spec, { kind: "remove", tile: "c" }).layout.rows; // [["a", "b"]]
 */
export function applyEdit(spec: DashboardSpec, edit: TileEdit): DashboardSpec {
  const rows = spec.layout.rows.map((r) => [...r]),
    r = rows.findIndex((row) => row.includes(edit.tile));
  if (r < 0) return spec;
  const row = rows[r]!,
    c = row.indexOf(edit.tile),
    alone = row.length === 1;
  const done = (tiles: DashboardTile[], next: string[][]): DashboardSpec => {
    const kept = next.filter((x) => x.length);
    // An invariant: every edit keeps the backend's rules.
    if (!laidOut(tiles, kept)) throw new Error("an edit broke the layout");
    return { v: 1, tiles, layout: { rows: kept } };
  };
  const retile = (change: (t: DashboardTile) => DashboardTile) =>
    spec.tiles.map((t) => (t.id === edit.tile ? change(t) : t));
  switch (edit.kind) {
    case "title": {
      const title = tileTitle(edit.title);
      return title
        ? done(
            retile((t) => ({ ...t, title })),
            rows,
          )
        : spec;
    }
    case "visual":
      return done(
        retile((t) => ({
          ...t,
          visual: edit.visual ? { kind: edit.visual } : {},
        })),
        rows,
      );
    case "remove":
      return done(
        spec.tiles.filter((t) => t.id !== edit.tile),
        rows.map((x) => x.filter((id) => id !== edit.tile)),
      );
    case "full_width":
      if (alone) return spec;
      row.splice(c, 1);
      rows.splice(r + 1, 0, [edit.tile]);
      return done(spec.tiles, rows);
    case "move": {
      if (edit.to === "left" || edit.to === "right") {
        const to = edit.to === "left" ? c - 1 : c + 1;
        if (to < 0 || to >= row.length) return spec;
        [row[c], row[to]] = [row[to]!, row[c]!];
        return done(spec.tiles, rows);
      }
      const up = edit.to === "up",
        n = up ? r - 1 : r + 1,
        other = rows[n];
      if (other && other.length < MAX_ROW) {
        row.splice(c, 1);
        if (up) other.push(edit.tile);
        else other.unshift(edit.tile);
        return done(spec.tiles, rows);
      }
      if (alone) {
        if (!other) return spec;
        [rows[r], rows[n]] = [other, row];
        return done(spec.tiles, rows);
      }
      row.splice(c, 1);
      rows.splice(up ? r : r + 1, 0, [edit.tile]);
      return done(spec.tiles, rows);
    }
  }
}

/**
 * Which moves change a tile's place, for the page to offer only those.
 *
 * @example
 * movesOf({ v: 1, tiles: [a], layout: { rows: [["a"]] } }, "a");
 * // { left: false, right: false, up: false, down: false, full_width: false }
 */
export function movesOf(
  spec: DashboardSpec,
  tile: string,
): Record<Direction | "full_width", boolean> {
  const rows = JSON.stringify(spec.layout.rows);
  const moved = (edit: TileEdit) =>
    JSON.stringify(applyEdit(spec, edit).layout.rows) !== rows;
  return {
    left: moved({ kind: "move", tile, to: "left" }),
    right: moved({ kind: "move", tile, to: "right" }),
    up: moved({ kind: "move", tile, to: "up" }),
    down: moved({ kind: "move", tile, to: "down" }),
    full_width: moved({ kind: "full_width", tile }),
  };
}

// ---------------------------------------------------------------- reads

/** The tiles that read one load of one ontology: one :read of their queries. */
export type LoadRead = {
  ontologyId: string;
  loadId: string;
  tiles: DashboardTile[];
};

/**
 * Which load each tile reads, grouped into one read a load: its pinned
 * load, or its ontology's current one (`current`: undefined while it isn't
 * known yet, null for an ontology with none). Tiles whose ontology has no
 * current load are `unloaded`; those whose current load isn't known yet,
 * `waiting`.
 *
 * @example
 * loadReads([tile("t1", "ont_1"), tile("t2", "ont_1")], () => "load_1").reads;
 * // [{ ontologyId: "ont_1", loadId: "load_1", tiles: [t1, t2] }]
 */
export function loadReads(
  tiles: readonly DashboardTile[],
  current: (ontologyId: string) => string | null | undefined,
): { reads: LoadRead[]; unloaded: string[]; waiting: string[] } {
  const reads = new Map<string, LoadRead>(),
    unloaded: string[] = [],
    waiting: string[] = [];
  for (const t of tiles) {
    const loadId =
      t.load.kind === "pinned" ? t.load.load_id : current(t.ontology_id);
    if (loadId === undefined) waiting.push(t.id);
    else if (loadId === null) unloaded.push(t.id);
    else {
      const key = `${t.ontology_id}\u0000${loadId}`;
      const read = reads.get(key) ?? {
        ontologyId: t.ontology_id,
        loadId,
        tiles: [],
      };
      read.tiles.push(t);
      reads.set(key, read);
    }
  }
  return { reads: [...reads.values()], unloaded, waiting };
}

/** What a tile shows: its query's output, drawn by the query as the engine read it, or why there is none. */
export type TileDraw =
  | { kind: "drawn"; output: QueryOutput; query: OntologyQuery }
  | { kind: "failed"; message: string };

/**
 * A tile's read outcome, without trust: its result drawn with the query as
 * the engine echoed it (else the tile's own), or the read's failure.
 *
 * @example
 * tileDraw({ ok: true, value: { kind: "query", query_echo: q, result: { kind: "count", count: 3 } } }, q).kind; // "drawn"
 * tileDraw({ ok: false, error: { code: "invalid", message: "`x` is no property" } }, q);
 * // { kind: "failed", message: "`x` is no property" }
 */
export function tileDraw(
  outcome: Outcome<unknown> | undefined,
  query: OntologyQuery,
): TileDraw {
  if (!outcome) return { kind: "failed", message: "The read had no result." };
  if (!outcome.ok) return { kind: "failed", message: outcome.error.message };
  const v = outcome.value;
  const output = isRecord(v) ? parseOutput(v.result) : undefined,
    echoed = isRecord(v) ? parseQuery(v.query_echo) : undefined,
    own = parseQuery(query);
  return isRecord(v) && v.kind === "query" && output && (echoed || own)
    ? { kind: "drawn", output, query: (echoed || own)! }
    : { kind: "failed", message: "The read’s answer is malformed." };
}

/**
 * The reason a dashboard route refused a change (its ErrorInfo), if it gave
 * one of the dashboards'.
 *
 * @example
 * dashboardRefusal({ details: [{ reason: "FOUNDRY_DASHBOARD_CHANGED" }] }); // "FOUNDRY_DASHBOARD_CHANGED"
 * dashboardRefusal(new Error("x")); // undefined
 */
export function dashboardRefusal(error: unknown): DashboardRefusal | undefined {
  const details = isRecord(error) ? error.details : undefined;
  for (const d of Array.isArray(details) ? details : []) {
    const reason = isRecord(d) ? d.reason : undefined;
    const known = DASHBOARD_REFUSALS.find((r) => r === reason);
    if (known) return known;
  }
  return undefined;
}

// ---------------------------------------------------------------- sharing

/**
 * A dashboard's page with its workspace, on `origin`: the link that shares
 * it inside the workspace. Whoever opens it signs in and reads it live, as
 * themselves (a grantee only through their grants); it is no snapshot.
 *
 * @example
 * workspaceLink("https://f.example", "ws 1", "d_1"); // "https://f.example/dashboards/d_1?workspace=ws%201"
 */
export function workspaceLink(origin: string, ws: string, id: string) {
  return `${origin}/dashboards/${encodeURIComponent(id)}?workspace=${encodeURIComponent(ws)}`;
}

/** When a publish ends its link, as the publish dialog offers it. */
export const LINK_EXPIRIES: readonly {
  value: DashboardLinkExpiry;
  label: string;
}[] = [
  { value: "FOUNDRY_DASHBOARD_LINK_EXPIRY_NEVER", label: "Never" },
  { value: "FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_7", label: "After 7 days" },
  { value: "FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_30", label: "After 30 days" },
  { value: "FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_90", label: "After 90 days" },
];

/**
 * Why the viewer can't make a public link of a dashboard, in words; undefined
 * when they can, or when there are no public links to make (the stamp's or
 * build's), which the page then doesn't mention at all.
 *
 * @example
 * publishBlockText("FOUNDRY_DASHBOARD_PUBLISH_BLOCK_ADMIN_ONLY"); // "Only owners and admins make public links"
 * publishBlockText("FOUNDRY_DASHBOARD_PUBLISH_BLOCK_UNAVAILABLE"); // undefined
 */
export function publishBlockText(
  block: DashboardPublishBlock | undefined,
): string | undefined {
  switch (block) {
    case "FOUNDRY_DASHBOARD_PUBLISH_BLOCK_ADMIN_ONLY":
      return "Only owners and admins make public links";
    case "FOUNDRY_DASHBOARD_PUBLISH_BLOCK_WORKSPACE_OFF":
      return "Turned off for this workspace";
    default:
      return undefined;
  }
}

/** A public link's state, as the page shows it. */
export type LinkState = {
  // Whether it shows its snapshot now.
  live: boolean;
  // What is said of it after "Public link": "expires 16 Dec 2026", "expired", …
  detail?: string;
};

/**
 * What a dashboard's public link is now, as the WFE says (`status`, which
 * reads the expiry by the server's clock), and when it ends; undefined for
 * one with no link.
 *
 * @example
 * linkState({ status: "FOUNDRY_DASHBOARD_SHARE_STATUS_LIVE" }, (d) => d); // { live: true }
 * linkState({ status: "FOUNDRY_DASHBOARD_SHARE_STATUS_LIVE", expiresAt: "2026-12-16" }, (d) => d); // { live: true, detail: "expires 2026-12-16" }
 * linkState({ status: "FOUNDRY_DASHBOARD_SHARE_STATUS_EXPIRED" }, (d) => d); // { live: false, detail: "expired" }
 */
export function linkState(
  share: FoundryDashboardShare | null | undefined,
  day: (iso: string) => string,
): LinkState | undefined {
  if (!share) return undefined;
  switch (share.status) {
    case "FOUNDRY_DASHBOARD_SHARE_STATUS_EXPIRED":
      return { live: false, detail: "expired" };
    case "FOUNDRY_DASHBOARD_SHARE_STATUS_WORKSPACE_OFF":
      return { live: false, detail: "turned off for this workspace" };
    case "FOUNDRY_DASHBOARD_SHARE_STATUS_UNAVAILABLE":
      return { live: false, detail: "off here" };
    default:
      return share.expiresAt
        ? { live: true, detail: `expires ${day(share.expiresAt)}` }
        : { live: true };
  }
}

/**
 * A link's state as one line: "Public link", "Public link · expired", …
 *
 * @example
 * linkText({ live: false, detail: "expired" }); // "Public link · expired"
 */
export const linkText = (state: LinkState) =>
  state.detail ? `Public link · ${state.detail}` : "Public link";

// ---------------------------------------------------------------- snapshot

/** A published tile as its public page shows it: drawn, or why it can't be. */
export type SharedTile = {
  id: string;
  title: string;
  kind?: VisualKind;
  asOf: string;
  drawn?: { output: QueryOutput; query: OntologyQuery };
};

/** A published dashboard as its public page shows it, in its rows. */
export type SharedDashboard = {
  name: string;
  description: string;
  publishedAt: string;
  rows: SharedTile[][];
};

/**
 * The door's snapshot, read without trust: its name, description, when it
 * was published and its tiles in their rows, each with its output and the
 * query that gave it when both parse. A tile the layout doesn't place, or
 * a row naming none, is left out. Undefined when it isn't a snapshot.
 *
 * @example
 * parseSnapshot({ v: 1, name: "Revenue", description: "", published_at: "2026-11-14T10:00:00Z", layout: { rows: [] }, tiles: [] });
 * // { name: "Revenue", description: "", publishedAt: "2026-11-14T10:00:00Z", rows: [] }
 */
export function parseSnapshot(x: unknown): SharedDashboard | undefined {
  if (
    !isRecord(x) ||
    x.v !== 1 ||
    typeof x.name !== "string" ||
    typeof x.published_at !== "string" ||
    !Array.isArray(x.tiles) ||
    !isRecord(x.layout) ||
    !Array.isArray(x.layout.rows)
  )
    return undefined;
  const tiles = new Map(
    x.tiles.flatMap((t): [string, SharedTile][] => {
      if (!isRecord(t) || typeof t.id !== "string") return [];
      const kind = isRecord(t.visual) ? t.visual.kind : undefined,
        output = parseOutput(t.output),
        query = parseQuery(t.query);
      return [
        [
          t.id,
          {
            id: t.id,
            title: typeof t.title === "string" ? t.title : "",
            ...(VISUAL_KINDS.includes(kind as VisualKind)
              ? { kind: kind as VisualKind }
              : {}),
            asOf: typeof t.as_of === "string" ? t.as_of : "",
            ...(output && query ? { drawn: { output, query } } : {}),
          },
        ],
      ];
    }),
  );
  return {
    name: x.name,
    description: typeof x.description === "string" ? x.description : "",
    publishedAt: x.published_at,
    rows: x.layout.rows
      .map((r) =>
        (Array.isArray(r) ? r : []).flatMap((id) => {
          const t = typeof id === "string" ? tiles.get(id) : undefined;
          return t ? [t] : [];
        }),
      )
      .filter((r) => r.length),
  };
}

// ---------------------------------------------------------------- library

/** What New dashboard starts Founder's composer with, the caret after it. */
export const NEW_DASHBOARD_PROMPT = "Build me a dashboard for: ";

/** Which of the library's dashboards show: every one, the pinned, or those the viewer made. */
export type LibraryFilter = "all" | "pinned" | "yours";

const folded = (text: string) =>
  text.normalize("NFD").replace(/\p{M}/gu, "").toLowerCase();

/**
 * The dashboards the library shows, in the list's order (pinned first, as
 * the WFE gives them): those `filter` keeps whose name or description holds
 * every word of `query`, case and accents aside.
 *
 * @example
 * const list = [{ id: "a", name: "Révenue", pinned: true }, { id: "b", name: "Churn", createdByUserId: "u1" }];
 * libraryDashboards(list, { query: "reve", filter: "all", userId: "u1" }).map((d) => d.id); // ["a"]
 * libraryDashboards(list, { query: "", filter: "yours", userId: "u1" }).map((d) => d.id); // ["b"]
 */
export function libraryDashboards<
  D extends Pick<
    FoundryDashboard,
    "name" | "description" | "pinned" | "createdByUserId"
  >,
>(
  list: readonly D[],
  {
    query,
    filter,
    userId,
  }: { query: string; filter: LibraryFilter; userId: string },
): D[] {
  const words = folded(query).split(/\s+/).filter(Boolean);
  return list.filter((d) => {
    if (filter === "pinned" && !d.pinned) return false;
    if (filter === "yours" && (!userId || d.createdByUserId !== userId))
      return false;
    const text = folded(`${d.name || ""}\n${d.description || ""}`);
    return words.every((w) => text.includes(w));
  });
}

/** How a tile draws in a dashboard's miniature: a number, bars, a line or a table's rows. */
export type TileGlyph = "figure" | "bar" | "line" | "table";

/**
 * What a tile draws as, from its spec alone (no read): as its visual asks
 * where its query's result can be drawn so (visualPlan's rules: one number
 * is a figure or a table; groups and rows are bars, a line or a table),
 * else as its result's shape suits: a count or an ungrouped aggregate is a
 * figure, groups are bars, rows a table.
 *
 * @example
 * tileGlyph({ query: { start: { type: "Invoice" }, return: { count: {} } }, visual: {} }); // "figure"
 * tileGlyph({ query: { start: { type: "Invoice" }, return: { group_count: { at: 0, prop: "status" } } }, visual: { kind: "line" } }); // "line"
 */
export function tileGlyph(
  tile: Pick<DashboardTile, "query" | "visual">,
): TileGlyph {
  // Read without trust: a spec's query is kept as the spec holds it.
  const r: unknown = isRecord(tile.query) ? tile.query.return : undefined,
    want = tile.visual.kind;
  const agg = isRecord(r) && isRecord(r.aggregate) ? r.aggregate : undefined;
  const one = isRecord(r) && ("count" in r || (!!agg && !agg.group_by));
  const groups = isRecord(r) && ("group_count" in r || !!agg?.group_by);
  if (one) return want === "table" ? "table" : "figure";
  if (want === "bar" || want === "line" || want === "table") return want;
  return groups ? "bar" : "table";
}

/** A tile in a dashboard's miniature: its title and how it draws. */
export type PreviewTile = { id: string; title: string; glyph: TileGlyph };

/**
 * A dashboard's miniature, from its spec: its first `rows` rows of tiles,
 * each with its title and glyph (the card crops what doesn't fit).
 *
 * @example
 * previewRows({ v: 1, tiles: [t1, t2], layout: { rows: [["t1", "t2"]] } });
 * // [[{ id: "t1", title: "Invoices", glyph: "figure" }, { id: "t2", title: "By status", glyph: "bar" }]]
 */
export function previewRows(spec: DashboardSpec, rows = 3): PreviewTile[][] {
  return tileRows(spec)
    .slice(0, rows)
    .map((row) =>
      row.map((t) => ({ id: t.id, title: t.title, glyph: tileGlyph(t) })),
    );
}
