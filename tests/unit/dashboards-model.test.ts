import { describe, expect, it } from "vitest";
import type { DashboardSpec, DashboardTile } from "@/shared/wfe";
import {
  EMPTY_SPEC,
  MAX_TILES,
  SHARE_TOKEN,
  applyEdit,
  dashboardName,
  dashboardRefusal,
  loadReads,
  movesOf,
  parseSnapshot,
  parseSpec,
  tileDraw,
  tileRows,
  tileTitle,
  type TileEdit,
} from "@/web/areas/dashboards/model";
import { parseQuery } from "@/web/areas/ontology/ask-model";

// A dashboard as the page reads it (src/web/areas/dashboards/model.ts): a
// spec taken only when it keeps the backend's rules, every edit keeping
// them too, the tiles' reads one a load, each tile's drawing, the
// dashboards' refusals, and a public snapshot read without trust.

const COUNT = { start: { type: "Invoice" }, return: { count: {} } };
const tile = (
  id: string,
  extra: Partial<DashboardTile> = {},
): DashboardTile => ({
  id,
  title: `Tile ${id}`,
  ontology_id: "ont_1",
  load: { kind: "current" },
  query: COUNT,
  visual: {},
  ...extra,
});
const spec = (rows: string[][]): DashboardSpec => ({
  v: 1,
  tiles: rows.flat().map((id) => tile(id)),
  layout: { rows },
});
const rowsOf = (s: DashboardSpec) => s.layout.rows;

describe("parseSpec", () => {
  it("reads an empty spec as no tiles, and a whole one as written", () => {
    expect(parseSpec("")).toEqual(EMPTY_SPEC);
    const s = {
      v: 1,
      tiles: [
        tile("t1", { visual: { kind: "bar" } }),
        tile("t2", { load: { kind: "pinned", load_id: "load_9" } }),
      ],
      layout: { rows: [["t2", "t1"]] },
    };
    expect(parseSpec(JSON.stringify(s))).toEqual(s);
    // Its visual left out shows as its result suits.
    const { visual: _visual, ...bare } = tile("t3");
    expect(
      parseSpec(
        JSON.stringify({ v: 1, tiles: [bare], layout: { rows: [["t3"]] } }),
      )?.tiles[0]?.visual,
    ).toEqual({});
  });

  it.each([
    ["not JSON", "{"],
    ["another version", JSON.stringify({ ...spec([]), v: 2 })],
    ["no layout", JSON.stringify({ v: 1, tiles: [] })],
    [
      "tiles not a list",
      JSON.stringify({ v: 1, tiles: {}, layout: { rows: [] } }),
    ],
    [
      "a row that isn't a list of ids",
      JSON.stringify({
        v: 1,
        tiles: [tile("t1")],
        layout: { rows: [["t1", 2]] },
      }),
    ],
    [
      "a tile left out of the layout",
      JSON.stringify({
        v: 1,
        tiles: [tile("t1"), tile("t2")],
        layout: { rows: [["t1"]] },
      }),
    ],
    [
      "a tile placed twice",
      JSON.stringify({
        v: 1,
        tiles: [tile("t1")],
        layout: { rows: [["t1"], ["t1"]] },
      }),
    ],
    ["a row of four", JSON.stringify(spec([["a", "b", "c", "d"]]))],
    [
      "an empty row",
      JSON.stringify({ v: 1, tiles: [], layout: { rows: [[]] } }),
    ],
    [
      "a layout naming no tile",
      JSON.stringify({ v: 1, tiles: [], layout: { rows: [["x"]] } }),
    ],
    [
      "two tiles of one id",
      JSON.stringify({
        v: 1,
        tiles: [tile("t1"), tile("t1")],
        layout: { rows: [["t1", "t1"]] },
      }),
    ],
    ["a tile id in capitals", JSON.stringify(spec([["T1"]]))],
    [
      "an empty title",
      JSON.stringify({
        v: 1,
        tiles: [tile("t1", { title: " " })],
        layout: { rows: [["t1"]] },
      }),
    ],
    [
      "a title with a control character",
      JSON.stringify({
        v: 1,
        tiles: [tile("t1", { title: "a\u0007" })],
        layout: { rows: [["t1"]] },
      }),
    ],
    [
      "an ontology id with a slash",
      JSON.stringify({
        v: 1,
        tiles: [tile("t1", { ontology_id: "../x" })],
        layout: { rows: [["t1"]] },
      }),
    ],
    [
      "a load of another kind",
      JSON.stringify({
        v: 1,
        tiles: [tile("t1", { load: { kind: "newest" } as never })],
        layout: { rows: [["t1"]] },
      }),
    ],
    [
      "a pinned load without its id",
      JSON.stringify({
        v: 1,
        tiles: [tile("t1", { load: { kind: "pinned" } as never })],
        layout: { rows: [["t1"]] },
      }),
    ],
    [
      "a query that isn't an object",
      JSON.stringify({
        v: 1,
        tiles: [tile("t1", { query: "count" as never })],
        layout: { rows: [["t1"]] },
      }),
    ],
    [
      "a visual kind it doesn't know",
      JSON.stringify({
        v: 1,
        tiles: [tile("t1", { visual: { kind: "pie" as never } })],
        layout: { rows: [["t1"]] },
      }),
    ],
    [
      "a visual that isn't an object",
      JSON.stringify({
        v: 1,
        tiles: [tile("t1", { visual: "bar" as never })],
        layout: { rows: [["t1"]] },
      }),
    ],
    [
      "more than 20 tiles",
      JSON.stringify(
        spec(Array.from({ length: MAX_TILES + 1 }, (_, i) => [`t${i}`])),
      ),
    ],
  ])("refuses %s", (_, json) => {
    expect(parseSpec(json)).toBeUndefined();
  });

  it("lays its tiles out in their rows", () => {
    const s = spec([["a", "b"], ["c"]]);
    expect(tileRows(s).map((r) => r.map((t) => t.id))).toEqual([
      ["a", "b"],
      ["c"],
    ]);
  });
});

describe("names", () => {
  it("takes a title and a name as the backend does", () => {
    expect(tileTitle("  Invoices ")).toBe("Invoices");
    expect(tileTitle("x".repeat(80))).toBe("x".repeat(80));
    expect(tileTitle("x".repeat(81))).toBeUndefined();
    expect(tileTitle("")).toBeUndefined();
    expect(dashboardName(" Revenue ")).toBe("Revenue");
    expect(dashboardName("x".repeat(121))).toBeUndefined();
    expect(dashboardName("a\nb")).toBeUndefined();
    expect(dashboardName("   ")).toBeUndefined();
  });

  it("knows a link's token by its form", () => {
    expect(SHARE_TOKEN.test("hxd_" + "a".repeat(43))).toBe(true);
    expect(SHARE_TOKEN.test("hxd_" + "a".repeat(42))).toBe(false);
    expect(SHARE_TOKEN.test("hxd_" + "a".repeat(42) + "/")).toBe(false);
  });
});

describe("applyEdit", () => {
  const two = spec([["a", "b"], ["c"]]);
  it.each<[string, DashboardSpec, TileEdit, string[][]]>([
    [
      "moves left within a row",
      two,
      { kind: "move", tile: "b", to: "left" },
      [["b", "a"], ["c"]],
    ],
    [
      "moves right within a row",
      two,
      { kind: "move", tile: "a", to: "right" },
      [["b", "a"], ["c"]],
    ],
    [
      "goes no further left",
      two,
      { kind: "move", tile: "a", to: "left" },
      [["a", "b"], ["c"]],
    ],
    [
      "goes no further right",
      two,
      { kind: "move", tile: "b", to: "right" },
      [["a", "b"], ["c"]],
    ],
    [
      "joins the row above when it has room",
      two,
      { kind: "move", tile: "c", to: "up" },
      [["a", "b", "c"]],
    ],
    [
      "joins the row below when it has room, first",
      two,
      { kind: "move", tile: "a", to: "down" },
      [["b"], ["a", "c"]],
    ],
    [
      "takes a row of its own above a full one",
      spec([
        ["a", "b", "c"],
        ["d", "e"],
      ]),
      { kind: "move", tile: "e", to: "up" },
      [["a", "b", "c"], ["e"], ["d"]],
    ],
    [
      "takes a row of its own below a full one",
      spec([
        ["a", "b"],
        ["c", "d", "e"],
      ]),
      { kind: "move", tile: "a", to: "down" },
      [["b"], ["a"], ["c", "d", "e"]],
    ],
    [
      "alone, trades places with a full row above",
      spec([["a", "b", "c"], ["d"]]),
      { kind: "move", tile: "d", to: "up" },
      [["d"], ["a", "b", "c"]],
    ],
    [
      "alone, trades places with a full row below",
      spec([["d"], ["a", "b", "c"]]),
      { kind: "move", tile: "d", to: "down" },
      [["a", "b", "c"], ["d"]],
    ],
    [
      "alone at the top, stays",
      spec([["a"], ["b", "c", "d"]]),
      { kind: "move", tile: "a", to: "up" },
      [["a"], ["b", "c", "d"]],
    ],
    [
      "alone at the bottom, stays",
      spec([["b", "c", "d"], ["a"]]),
      { kind: "move", tile: "a", to: "down" },
      [["b", "c", "d"], ["a"]],
    ],
    [
      "sharing the top row, takes a row of its own above",
      spec([["a", "b"]]),
      { kind: "move", tile: "b", to: "up" },
      [["b"], ["a"]],
    ],
    [
      "sharing the bottom row, takes a row of its own below",
      spec([["a", "b"]]),
      { kind: "move", tile: "a", to: "down" },
      [["b"], ["a"]],
    ],
    [
      "goes full width below its row",
      two,
      { kind: "full_width", tile: "a" },
      [["b"], ["a"], ["c"]],
    ],
    [
      "full width already, stays",
      two,
      { kind: "full_width", tile: "c" },
      [["a", "b"], ["c"]],
    ],
    ["removes a tile", two, { kind: "remove", tile: "a" }, [["b"], ["c"]]],
    [
      "removes its row with its last tile",
      two,
      { kind: "remove", tile: "c" },
      [["a", "b"]],
    ],
    [
      "changes nothing for a tile it doesn't have",
      two,
      { kind: "remove", tile: "z" },
      [["a", "b"], ["c"]],
    ],
  ])("%s", (_, s, edit, rows) => {
    const out = applyEdit(s, edit);
    expect(rowsOf(out)).toEqual(rows);
    // The result is still a spec the backend keeps.
    expect(parseSpec(JSON.stringify(out))).toEqual(out);
  });

  it("removes the tile itself too", () => {
    expect(
      applyEdit(two, { kind: "remove", tile: "a" }).tiles.map((t) => t.id),
    ).toEqual(["b", "c"]);
  });

  it("retitles a tile with a title the backend takes, and nothing else", () => {
    const out = applyEdit(two, { kind: "title", tile: "b", title: " Paid " });
    expect(out.tiles.find((t) => t.id === "b")?.title).toBe("Paid");
    expect(applyEdit(two, { kind: "title", tile: "b", title: "  " })).toBe(two);
  });

  it("shows a tile as asked, or as its result suits", () => {
    const bar = applyEdit(two, { kind: "visual", tile: "c", visual: "bar" });
    expect(bar.tiles.find((t) => t.id === "c")?.visual).toEqual({
      kind: "bar",
    });
    expect(
      applyEdit(bar, { kind: "visual", tile: "c", visual: null }).tiles.find(
        (t) => t.id === "c",
      )?.visual,
    ).toEqual({});
  });

  it("offers only the moves that change a tile's place", () => {
    expect(movesOf(spec([["a"]]), "a")).toEqual({
      left: false,
      right: false,
      up: false,
      down: false,
      full_width: false,
    });
    expect(movesOf(two, "a")).toEqual({
      left: false,
      right: true,
      up: true,
      down: true,
      full_width: true,
    });
  });
});

describe("loadReads", () => {
  it("reads each load once, its tiles together, and says which wait or have none", () => {
    const tiles = [
      tile("t1"),
      tile("t2", { ontology_id: "ont_2" }),
      tile("t3"),
      tile("t4", { load: { kind: "pinned", load_id: "load_old" } }),
      tile("t5", { ontology_id: "ont_3" }),
      tile("t6", { ontology_id: "ont_4" }),
    ];
    const current: Record<string, string | null | undefined> = {
      ont_1: "load_1",
      ont_2: "load_2",
      ont_3: null,
      ont_4: undefined,
    };
    const { reads, unloaded, waiting } = loadReads(tiles, (o) => current[o]);
    expect(
      reads.map((r) => [r.ontologyId, r.loadId, r.tiles.map((t) => t.id)]),
    ).toEqual([
      ["ont_1", "load_1", ["t1", "t3"]],
      ["ont_2", "load_2", ["t2"]],
      ["ont_1", "load_old", ["t4"]],
    ]);
    expect(unloaded).toEqual(["t5"]);
    expect(waiting).toEqual(["t6"]);
  });
});

describe("tileDraw", () => {
  const echo = {
    start: { type: "Invoice", where: [], ids: null, text: null },
    steps: [],
    filters: [],
    return: { group_count: { at: 0, prop: "status", limit: 50 } },
  };
  const groups = {
    kind: "group_count",
    groups: [{ value: { untrusted: "paid" }, count: 3 }],
    total_groups: 1,
  };
  it("draws the result with the query as the engine read it", () => {
    expect(
      tileDraw(
        {
          ok: true,
          value: { kind: "query", query_echo: echo, result: groups },
        },
        COUNT as never,
      ),
    ).toEqual({ kind: "drawn", output: groups, query: parseQuery(echo) });
  });

  it("falls back on the tile's own query without an echo", () => {
    expect(
      tileDraw(
        {
          ok: true,
          value: { kind: "query", result: { kind: "count", count: 2 } },
        },
        COUNT as never,
      ),
    ).toEqual({
      kind: "drawn",
      output: { kind: "count", count: 2 },
      query: parseQuery(COUNT),
    });
  });

  it("says why there is nothing to draw", () => {
    expect(tileDraw(undefined, COUNT as never)).toEqual({
      kind: "failed",
      message: "The read had no result.",
    });
    expect(
      tileDraw(
        {
          ok: false,
          error: { code: "invalid", message: "`x` is no property" },
        },
        COUNT as never,
      ),
    ).toEqual({ kind: "failed", message: "`x` is no property" });
    for (const value of [
      { kind: "entity", result: groups },
      { kind: "query", result: { kind: "rows", rows: "<table>" } },
      { kind: "query", query_echo: { nope: 1 }, result: groups },
      "query",
    ])
      expect(tileDraw({ ok: true, value }, { nope: 1 } as never).kind).toBe(
        "failed",
      );
  });
});

describe("dashboardRefusal", () => {
  it("reads the dashboards' own reasons only", () => {
    expect(
      dashboardRefusal({
        details: [{ reason: "OTHER" }, { reason: "FOUNDRY_DASHBOARD_LIMIT" }],
      }),
    ).toBe("FOUNDRY_DASHBOARD_LIMIT");
    expect(
      dashboardRefusal({ details: [{ reason: "FOUNDRY_PRO_REQUIRED" }] }),
    ).toBeUndefined();
    expect(dashboardRefusal({ details: "x" })).toBeUndefined();
    expect(dashboardRefusal(null)).toBeUndefined();
  });
});

describe("parseSnapshot", () => {
  const output = {
    kind: "rows",
    rows: [{ cells: [{ type: "Customer", name: "Globex" }, 12], edges: [] }],
    total: 1,
  };
  const query = {
    start: { type: "Customer" },
    return: { rows: { columns: [{ at: 0 }, { at: 0, prop: "n" }] } },
  };
  const snapshot = {
    v: 1,
    name: "Revenue",
    description: "Monthly",
    published_at: "2026-11-14T10:00:00Z",
    layout: { rows: [["t1", "t2"], ["gone"], ["t3"]] },
    tiles: [
      {
        id: "t1",
        title: "Customers",
        visual: { kind: "table" },
        query,
        as_of: "2026-11-14",
        output,
      },
      {
        id: "t2",
        title: "Broken",
        visual: { kind: "pie" },
        query,
        as_of: "2026-11-14",
        output: { kind: "rows", rows: "x" },
      },
      { id: "t3", title: 7, visual: null, query: null },
      "not a tile",
    ],
  };

  it("reads its tiles in their rows, each drawn when it can be", () => {
    expect(parseSnapshot(snapshot)).toEqual({
      name: "Revenue",
      description: "Monthly",
      publishedAt: "2026-11-14T10:00:00Z",
      rows: [
        [
          {
            id: "t1",
            title: "Customers",
            kind: "table",
            asOf: "2026-11-14",
            drawn: { output, query: parseQuery(query) },
          },
          { id: "t2", title: "Broken", asOf: "2026-11-14" },
        ],
        [{ id: "t3", title: "", asOf: "" }],
      ],
    });
  });

  it.each([
    ["no version", { ...snapshot, v: undefined }],
    ["no name", { ...snapshot, name: 3 }],
    ["no tiles", { ...snapshot, tiles: undefined }],
    ["no layout", { ...snapshot, layout: { rows: "x" } }],
    ["no time", { ...snapshot, published_at: undefined }],
    ["not an object", "Revenue"],
  ])("refuses one with %s", (_, x) => {
    expect(parseSnapshot(x)).toBeUndefined();
  });

  it("keeps an empty description, and leaves out a row that isn't one", () => {
    expect(
      parseSnapshot({
        ...snapshot,
        description: null,
        layout: { rows: ["t1"] },
      }),
    ).toMatchObject({ description: "", rows: [] });
  });
});
