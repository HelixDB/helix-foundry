import { describe, expect, it } from "vitest";
import type { Connector, FoundryDocument } from "@/shared/wfe";
import {
  DOCUMENTS_NAME,
  EMPTY_LAYOUT,
  MAX_FOLDERS,
  MAX_FOLDER_NAME,
  MAX_LAYOUT_BYTES,
  applyChange,
  arrange,
  destinations,
  dropBefore,
  folderNameProblem,
  layoutBytes,
  parseLayout,
  prune,
  stepBefore,
  toStored,
  type Changed,
  type Layout,
  type TreeData,
  type TreeNode,
} from "@/web/areas/data/layout";

// The Data tree's layout: reading the stored JSON (and refusing a version
// it doesn't know), arranging the tree from it, pruning what the workspace
// no longer has, applying each change, and the 64 KiB the WFE stores.

const LABELS: Record<string, string> = {
  stripe: "Stripe",
  workos: "WorkOS",
  postgres: "PostgreSQL",
};
const label = (kind: string) => LABELS[kind] ?? kind;
const connector = (id: string, kind: string, name = id): Connector => ({
  id,
  kind,
  name,
});
const doc = (id: string, setId: string, createdAt: string): FoundryDocument =>
  ({ id, name: `${id}.md`, setId, createdAt }) as FoundryDocument;

// Stripe a, b; PostgreSQL p; sets g (General: d1, d2) and s (Board: d3).
const DATA: TreeData = {
  kinds: ["stripe", "workos", "postgres"],
  connectors: [
    connector("a", "stripe", "Live"),
    connector("p", "postgres", "Shop"),
    connector("b", "stripe", "Test"),
  ],
  sets: [
    { id: "g", name: "General" },
    { id: "s", name: "Board" },
  ],
  documents: [
    doc("d2", "g", "2026-10-02"),
    doc("d1", "g", "2026-10-01"),
    doc("d3", "s", "2026-10-03"),
  ],
  label,
};
const layout = (l: Partial<Layout>): Layout => ({ ...EMPTY_LAYOUT, ...l });
// The tree in short: each root folder's key and its children's ids.
const shape = (nodes: TreeNode[]) =>
  nodes.map((n) =>
    n.type === "documents"
      ? [n.key, n.sets.map((f) => [f.set.id, f.documents.map((d) => d.id)])]
      : [n.key, n.items.map((i) => i.connector.id)],
  );
const changed = (result: Changed): Layout => {
  if ("problem" in result) throw new Error(result.problem);
  return result.layout;
};
const problem = (result: Changed) =>
  "problem" in result ? result.problem : undefined;

describe("reading a stored layout", () => {
  it("reads a workspace with none as the empty layout", () => {
    expect(parseLayout({})).toEqual({ version: 1, layout: EMPTY_LAYOUT });
  });

  it("leaves a version it doesn't know alone", () => {
    for (const stored of [
      { v: 2, rows: [] },
      { v: "1" },
      { names: {} },
      null,
      [],
      "x",
      undefined,
    ])
      expect(parseLayout(stored)).toEqual({ version: "unknown" });
  });

  it("keeps what has the right shape and drops the rest", () => {
    expect(
      parseLayout({
        v: 1,
        names: { "kind:stripe": " Payments ", "set:x": "No" },
        folders: [
          { id: "f1", name: "Finance" },
          { id: "f1", name: "Again" },
        ],
        order: { root: ["documents", "documents", 3], nowhere: ["a"] },
      }),
    ).toEqual({
      version: 1,
      layout: {
        names: { "kind:stripe": "Payments" },
        folders: [{ id: "f1", name: "Finance" }],
        order: { root: ["documents"] },
      },
    });
    const long = "x".repeat(MAX_FOLDER_NAME + 1);
    expect(
      parseLayout({
        v: 1,
        names: { documents: 3, "kind:": "Empty kind", "kind:posthog": "  " },
        folders: [
          "f",
          { name: "No id" },
          { id: "", name: "Blank id" },
          { id: "f2", name: " " },
          { id: "f3", name: long },
          { id: "f4", name: "Ops" },
        ],
        order: { "kind:stripe": "a", "folder:f4": ["a", null, "b"] },
      }),
    ).toEqual({
      version: 1,
      layout: {
        names: {},
        folders: [{ id: "f4", name: "Ops" }],
        order: { "folder:f4": ["a", "b"] },
      },
    });
    // Shapes that aren't lists or objects read as none.
    expect(parseLayout({ v: 1, names: [], folders: {}, order: null })).toEqual({
      version: 1,
      layout: EMPTY_LAYOUT,
    });
  });

  it("keeps up to MAX_FOLDERS folders", () => {
    const folders = Array.from({ length: MAX_FOLDERS + 3 }, (_, i) => ({
      id: `f${i}`,
      name: `Folder ${i}`,
    }));
    const read = parseLayout({ v: 1, folders });
    expect(read.version === 1 && read.layout.folders).toEqual(
      folders.slice(0, MAX_FOLDERS),
    );
  });

  it("stores version 1 without empty orders, and reads it back the same", () => {
    expect(toStored(EMPTY_LAYOUT)).toEqual({
      v: 1,
      names: {},
      folders: [],
      order: {},
    });
    const l = layout({
      names: { documents: "Files" },
      folders: [{ id: "f", name: "Finance" }],
      order: { root: ["documents"], "folder:f": [] },
    });
    const stored = toStored(l);
    expect(stored.order).toEqual({ root: ["documents"] });
    expect(parseLayout(JSON.parse(JSON.stringify(stored)))).toEqual({
      version: 1,
      layout: { ...l, order: { root: ["documents"] } },
    });
  });

  it("counts compact UTF-8 JSON, as the WFE does", () => {
    expect(layoutBytes(EMPTY_LAYOUT)).toBe(
      JSON.stringify({ v: 1, names: {}, folders: [], order: {} }).length,
    );
    expect(layoutBytes(layout({ names: { documents: "é" } }))).toBe(
      layoutBytes(layout({ names: { documents: "e" } })) + 1,
    );
  });
});

describe("arranging the tree", () => {
  it("puts kinds in the WFE's order, then documents/, without a layout", () => {
    expect(shape(arrange(EMPTY_LAYOUT, DATA))).toEqual([
      ["kind:stripe", ["a", "b"]],
      ["kind:postgres", ["p"]],
      [
        "documents",
        [
          ["g", ["d1", "d2"]],
          ["s", ["d3"]],
        ],
      ],
    ]);
  });

  it("names folders as the kit labels kinds, unless renamed", () => {
    const nodes = arrange(
      layout({ names: { "kind:stripe": "Payments", documents: "Files" } }),
      DATA,
    );
    expect(nodes.map((n) => n.name)).toEqual([
      "Payments",
      "PostgreSQL",
      "Files",
    ]);
    expect(arrange(EMPTY_LAYOUT, { ...DATA, label: undefined })[0]!.name).toBe(
      "stripe",
    );
    expect(arrange(EMPTY_LAYOUT, DATA).at(-1)!.name).toBe(DOCUMENTS_NAME);
  });

  it("adds kinds the WFE doesn't list after its own, in their first connector's order", () => {
    const data = {
      ...DATA,
      connectors: [
        connector("z", "zendesk"),
        ...DATA.connectors,
        connector("h", "hubspot"),
      ],
    };
    expect(arrange(EMPTY_LAYOUT, data).map((n) => n.key)).toEqual([
      "kind:stripe",
      "kind:postgres",
      "kind:zendesk",
      "kind:hubspot",
      "documents",
    ]);
  });

  it("matches the example: a folder holds what its order lists", () => {
    expect(
      arrange(
        {
          names: {},
          folders: [{ id: "f", name: "Finance" }],
          order: { root: ["folder:f"], "folder:f": ["b"] },
        },
        {
          kinds: ["stripe", "postgres"],
          sets: [],
          documents: [],
          connectors: [
            { id: "a", kind: "postgres" },
            { id: "b", kind: "stripe" },
          ],
        },
      ).map((n) => n.key),
    ).toEqual(["folder:f", "kind:postgres", "documents"]);
  });

  it("orders what an order lists first and puts the rest after it", () => {
    const nodes = arrange(
      layout({
        order: {
          root: ["documents", "kind:postgres", "kind:gone"],
          "kind:stripe": ["b", "gone"],
          documents: ["set:s"],
          "set:g": ["d2"],
        },
      }),
      DATA,
    );
    expect(shape(nodes)).toEqual([
      [
        "documents",
        [
          ["s", ["d3"]],
          ["g", ["d2", "d1"]],
        ],
      ],
      ["kind:postgres", ["p"]],
      ["kind:stripe", ["b", "a"]],
    ]);
  });

  it("keeps custom folders, empty ones too, and a kind folder only while it holds one", () => {
    const nodes = arrange(
      layout({
        folders: [
          { id: "f", name: "Finance" },
          { id: "e", name: "Empty" },
          { id: "o", name: "Ops" },
        ],
        // The first folder to list a connector holds it.
        order: { "folder:f": ["p", "a"], "folder:o": ["a", "b", "nope"] },
      }),
      DATA,
    );
    expect(shape(nodes)).toEqual([
      ["folder:f", ["p", "a"]],
      ["folder:e", []],
      ["folder:o", ["b"]],
      [
        "documents",
        [
          ["g", ["d1", "d2"]],
          ["s", ["d3"]],
        ],
      ],
    ]);
  });

  it("puts documents uploaded together, or without a date, in id order", () => {
    const data = {
      ...DATA,
      documents: [
        doc("y", "s", "2026-10-01"),
        doc("x", "s", "2026-10-01"),
        { id: "w", setId: "s" },
      ],
    };
    expect(shape(arrange(EMPTY_LAYOUT, data)).at(-1)).toEqual([
      "documents",
      [
        ["g", []],
        ["s", ["w", "x", "y"]],
      ],
    ]);
  });

  it("leaves out a document whose set isn't listed yet", () => {
    const data = { ...DATA, documents: [doc("x", "new", "2026-10-04")] };
    expect(shape(arrange(EMPTY_LAYOUT, data)).at(-1)).toEqual([
      "documents",
      [
        ["g", []],
        ["s", []],
      ],
    ]);
  });

  describe("with a search", () => {
    const data: TreeData = {
      ...DATA,
      connectors: [
        { ...connector("a", "stripe", "Live"), tables: [{ name: "charges" }] },
        {
          ...connector("p", "postgres", "Shop"),
          tables: [{ name: "orders" }, { name: "order_items" }],
        },
        connector("b", "stripe", ""),
      ],
    };
    const finance = layout({
      folders: [{ id: "f", name: "Finance" }],
      order: { "folder:f": ["p"] },
    });

    it("keeps connectors by name, kind or tables, and drops what it empties", () => {
      const found = arrange(EMPTY_LAYOUT, data, " ORDER ");
      expect(shape(found)).toEqual([["kind:postgres", ["p"]]]);
      const n = found[0]!;
      expect(
        n.type === "kind" && n.items[0]!.tables.map((t) => t.name),
      ).toEqual(["orders", "order_items"]);
      // A connector without a name goes by its kind's.
      expect(shape(arrange(EMPTY_LAYOUT, data, "live"))).toEqual([
        ["kind:stripe", ["a"]],
      ]);
      expect(shape(arrange(EMPTY_LAYOUT, data, "zzz"))).toEqual([]);
    });

    it("keeps a folder whose name matches with all it holds", () => {
      expect(shape(arrange(finance, data, "fin"))).toEqual([
        ["folder:f", ["p"]],
      ]);
      expect(
        shape(
          arrange(
            layout({ names: { "kind:stripe": "Payments" } }),
            data,
            "pay",
          ),
        ),
      ).toEqual([["kind:stripe", ["a", "b"]]]);
      // A connector's kind still finds it in a custom folder.
      expect(shape(arrange(finance, data, "postgres"))).toEqual([
        ["folder:f", ["p"]],
      ]);
    });

    it("finds sets by name and documents by theirs", () => {
      expect(shape(arrange(EMPTY_LAYOUT, data, "board"))).toEqual([
        ["documents", [["s", ["d3"]]]],
      ]);
      expect(shape(arrange(EMPTY_LAYOUT, data, "d2"))).toEqual([
        ["documents", [["g", ["d2"]]]],
      ]);
      expect(shape(arrange(EMPTY_LAYOUT, data, "documents")).at(-1)).toEqual([
        "documents",
        [
          ["g", ["d1", "d2"]],
          ["s", ["d3"]],
        ],
      ]);
    });
  });
});

// DATA with a Granola connection, gr, that owns Notes (n) and Transcripts
// (t): sets in the WFE's list that documents/ doesn't hold.
const owned = (id: string, name: string, stream: string) => ({
  id,
  name,
  connector: { connectorId: "gr", stream },
});
const GRANOLA: TreeData = {
  ...DATA,
  kinds: [...DATA.kinds, "granola"],
  connectors: [
    ...DATA.connectors,
    { ...connector("gr", "granola", "Team"), tables: [{ name: "notes" }] },
  ],
  sets: [
    DATA.sets[0]!,
    owned("n", "Notes", "notes"),
    DATA.sets[1]!,
    owned("t", "Transcripts", "transcripts"),
  ],
};

describe("a connection's own sets", () => {
  it("sit under it after its tables, in the WFE's order, and documents/ holds the rest", () => {
    const nodes = arrange(EMPTY_LAYOUT, GRANOLA);
    expect(shape(nodes)).toEqual([
      ["kind:stripe", ["a", "b"]],
      ["kind:postgres", ["p"]],
      ["kind:granola", ["gr"]],
      [
        "documents",
        [
          ["g", ["d1", "d2"]],
          ["s", ["d3"]],
        ],
      ],
    ]);
    const granola = nodes[2]!;
    expect(granola.type === "kind" && granola.items[0]).toEqual({
      connector: GRANOLA.connectors[3],
      tables: [{ name: "notes" }],
      sets: [GRANOLA.sets[1], GRANOLA.sets[3]],
    });
    // A connector with neither lists nothing.
    const stripe = nodes[0]!;
    expect(stripe.type === "kind" && stripe.items[0]).toMatchObject({
      tables: [],
      sets: [],
    });
  });

  it("are found by name; a folder or connection found by its own lists nothing", () => {
    const items = (term: string) =>
      arrange(EMPTY_LAYOUT, GRANOLA, term).flatMap((n) =>
        n.type === "documents"
          ? []
          : n.items.map((i) => [
              i.connector.id,
              i.tables.map((t) => t.name),
              i.sets.map((s) => s.id),
            ]),
      );
    expect(items("notes")).toEqual([["gr", ["notes"], ["n"]]]);
    expect(items("TRANS")).toEqual([["gr", [], ["t"]]]);
    expect(items("team")).toEqual([["gr", [], []]]);
    expect(items("granola")).toEqual([["gr", [], []]]);
    // documents/ doesn't find them.
    expect(shape(arrange(EMPTY_LAYOUT, GRANOLA, "transcripts"))).toEqual([
      ["kind:granola", ["gr"]],
    ]);
  });

  it("are in no order: pruning drops orders that name them", () => {
    expect(
      prune(
        layout({
          order: {
            documents: ["set:t", "set:s", "set:n", "set:g"],
            "set:n": ["d9"],
            "set:g": ["d2"],
          },
        }),
        GRANOLA,
      ).order,
    ).toEqual({ documents: ["set:s", "set:g"], "set:g": ["d2"] });
    expect(
      problem(
        applyChange(EMPTY_LAYOUT, GRANOLA, {
          type: "move-set",
          id: "n",
          before: null,
        }),
      ),
    ).toBe("That set isn’t in Connectors any more.");
    // Moving a set of documents/ writes an order without them.
    expect(
      changed(
        applyChange(
          layout({ order: { documents: ["set:n", "set:g"] } }),
          GRANOLA,
          { type: "move-set", id: "s", before: "g" },
        ),
      ).order,
    ).toEqual({ documents: ["set:s", "set:g"] });
  });
});

describe("pruning what the workspace no longer has", () => {
  it("matches the example", () => {
    expect(
      prune(
        {
          names: { "kind:gone": "Old" },
          folders: [],
          order: { "kind:stripe": ["deleted", "b"], "set:gone": ["d"] },
        },
        {
          kinds: ["stripe"],
          sets: [],
          documents: [],
          connectors: [{ id: "b", kind: "stripe" }],
        },
      ),
    ).toEqual({ names: {}, folders: [], order: { "kind:stripe": ["b"] } });
  });

  it("drops ids outside their container and keeps folders and known kinds", () => {
    expect(
      prune(
        layout({
          names: { documents: "Files", "kind:workos": "Auth" },
          folders: [
            { id: "f", name: "Finance" },
            { id: "e", name: "Empty" },
          ],
          order: {
            root: ["folder:gone", "kind:workos", "folder:e", "documents"],
            "kind:stripe": ["b", "a"],
            "folder:f": ["a", "x"],
            "folder:e": ["a"],
            "kind:postgres": [],
            documents: ["set:s", "set:x", "g"],
            "set:g": ["d3", "d2"],
          },
        }),
        DATA,
      ),
    ).toEqual({
      // WorkOS is a kind the WFE has, with no connector yet.
      names: { documents: "Files", "kind:workos": "Auth" },
      folders: [
        { id: "f", name: "Finance" },
        { id: "e", name: "Empty" },
      ],
      order: {
        root: ["kind:workos", "folder:e", "documents"],
        "kind:stripe": ["b"],
        "folder:f": ["a"],
        documents: ["set:s"],
        "set:g": ["d2"],
      },
    });
  });
});

describe("changing the layout", () => {
  it("matches the example: a move writes the order it changes in full", () => {
    const data = {
      kinds: ["stripe"],
      sets: [],
      documents: [],
      connectors: [
        { id: "a", kind: "stripe" },
        { id: "b", kind: "stripe" },
      ],
    };
    expect(
      applyChange(EMPTY_LAYOUT, data, {
        type: "move-connector",
        id: "b",
        folder: null,
        before: "a",
      }),
    ).toEqual({
      layout: { names: {}, folders: [], order: { "kind:stripe": ["b", "a"] } },
    });
  });

  it("moves root folders, last when what it goes before is gone", () => {
    const up = changed(
      applyChange(EMPTY_LAYOUT, DATA, {
        type: "move-root",
        key: "documents",
        before: "kind:stripe",
      }),
    );
    expect(up.order).toEqual({
      root: ["documents", "kind:stripe", "kind:postgres"],
    });
    const last = changed(
      applyChange(up, DATA, {
        type: "move-root",
        key: "documents",
        before: "folder:gone",
      }),
    );
    expect(arrange(last, DATA).map((n) => n.key)).toEqual([
      "kind:stripe",
      "kind:postgres",
      "documents",
    ]);
    expect(
      problem(
        applyChange(EMPTY_LAYOUT, DATA, {
          type: "move-root",
          key: "kind:workos",
          before: null,
        }),
      ),
    ).toBe("That folder isn’t in Connectors any more.");
  });

  describe("connectors", () => {
    const finance = layout({
      folders: [
        { id: "f", name: "Finance" },
        { id: "o", name: "Ops" },
      ],
      order: { "kind:stripe": ["b", "a"], "folder:f": ["p"] },
    });

    it("moves one into a custom folder, out of its kind's", () => {
      const moved = changed(
        applyChange(finance, DATA, {
          type: "move-connector",
          id: "a",
          folder: "f",
          before: "p",
        }),
      );
      expect(moved.order).toEqual({
        "kind:stripe": ["b"],
        "folder:f": ["a", "p"],
      });
      expect(shape(arrange(moved, DATA)).slice(0, 3)).toEqual([
        ["kind:stripe", ["b"]],
        ["folder:f", ["a", "p"]],
        ["folder:o", []],
      ]);
    });

    it("moves one between custom folders and back to its kind's", () => {
      const ops = changed(
        applyChange(finance, DATA, {
          type: "move-connector",
          id: "p",
          folder: "o",
          before: null,
        }),
      );
      // Finance's emptied order is left out.
      expect(ops.order).toEqual({
        "kind:stripe": ["b", "a"],
        "folder:o": ["p"],
      });
      // Its kind's folder was gone; it comes back with it.
      const home = changed(
        applyChange(ops, DATA, {
          type: "move-connector",
          id: "p",
          folder: null,
          before: null,
        }),
      );
      expect(shape(arrange(home, DATA)).map(([key]) => key)).toEqual([
        "kind:stripe",
        "kind:postgres",
        "folder:f",
        "folder:o",
        "documents",
      ]);
      expect(home.order).toEqual({
        "kind:stripe": ["b", "a"],
        "kind:postgres": ["p"],
      });
    });

    it("reorders one in its folder", () => {
      const moved = changed(
        applyChange(finance, DATA, {
          type: "move-connector",
          id: "b",
          folder: null,
          before: null,
        }),
      );
      expect(moved.order["kind:stripe"]).toEqual(["a", "b"]);
    });

    it("refuses a connector or folder that's gone", () => {
      expect(
        problem(
          applyChange(finance, DATA, {
            type: "move-connector",
            id: "x",
            folder: null,
            before: null,
          }),
        ),
      ).toBe("That connection isn’t in Connectors any more.");
      expect(
        problem(
          applyChange(finance, DATA, {
            type: "move-connector",
            id: "a",
            folder: "gone",
            before: null,
          }),
        ),
      ).toBe("That folder isn’t in Connectors any more.");
    });
  });

  it("orders sets in documents/ and documents in their set", () => {
    const sets = changed(
      applyChange(EMPTY_LAYOUT, DATA, {
        type: "move-set",
        id: "s",
        before: "g",
      }),
    );
    expect(sets.order).toEqual({ documents: ["set:s", "set:g"] });
    const last = changed(
      applyChange(sets, DATA, { type: "move-set", id: "s", before: null }),
    );
    expect(last.order).toEqual({ documents: ["set:g", "set:s"] });
    const docs = changed(
      applyChange(EMPTY_LAYOUT, DATA, {
        type: "move-document",
        id: "d2",
        before: "d1",
      }),
    );
    expect(docs.order).toEqual({ "set:g": ["d2", "d1"] });
    expect(
      problem(
        applyChange(EMPTY_LAYOUT, DATA, {
          type: "move-set",
          id: "x",
          before: null,
        }),
      ),
    ).toBe("That set isn’t in Connectors any more.");
    expect(
      problem(
        applyChange(EMPTY_LAYOUT, DATA, {
          type: "move-document",
          id: "x",
          before: null,
        }),
      ),
    ).toBe("That document isn’t in Connectors any more.");
  });

  describe("custom folders", () => {
    const create = (l: Layout, id: string, name: string) =>
      applyChange(l, DATA, { type: "create-folder", folder: { id, name } });

    it("creates one at the end of the root, with its name trimmed", () => {
      const made = changed(create(EMPTY_LAYOUT, "f", "  Finance "));
      expect(made.folders).toEqual([{ id: "f", name: "Finance" }]);
      expect(arrange(made, DATA).map((n) => n.key)).toEqual([
        "kind:stripe",
        "kind:postgres",
        "folder:f",
        "documents",
      ]);
      // Under an order of the root, last.
      const listed = layout({ order: { root: ["documents"] } });
      expect(
        arrange(changed(create(listed, "f", "Finance")), DATA).map(
          (n) => n.key,
        ),
      ).toEqual(["documents", "kind:stripe", "kind:postgres", "folder:f"]);
    });

    it("refuses a name another root folder has, a blank or long one, a used id, and too many", () => {
      const made = changed(create(EMPTY_LAYOUT, "f", "Finance"));
      expect(problem(create(made, "g", "FINANCE"))).toBe(
        "There’s already a folder called Finance.",
      );
      expect(problem(create(made, "g", "documénts"))).toBe(
        "There’s already a folder called Documents.",
      );
      expect(problem(create(made, "g", "  "))).toBe("Give the folder a name.");
      expect(problem(create(made, "g", "x".repeat(MAX_FOLDER_NAME + 1)))).toBe(
        `A folder’s name is at most ${MAX_FOLDER_NAME} characters.`,
      );
      expect(problem(create(made, "f", "Other"))).toBe(
        "That folder is already there.",
      );
      expect(problem(create(made, "", "Other"))).toBe(
        "That folder is already there.",
      );
      const full = layout({
        folders: Array.from({ length: MAX_FOLDERS }, (_, i) => ({
          id: `f${i}`,
          name: `Folder ${i}`,
        })),
      });
      expect(problem(create(full, "g", "One more"))).toBe(
        `Connectors holds up to ${MAX_FOLDERS} folders.`,
      );
    });

    it("renames one, its own name in another case included", () => {
      const made = changed(
        create(changed(create(EMPTY_LAYOUT, "f", "Finance")), "o", "Ops"),
      );
      const rename = (name: string, id = "f") =>
        applyChange(made, DATA, { type: "rename-folder", id, name });
      expect(changed(rename(" FINANCE ")).folders).toEqual([
        { id: "f", name: "FINANCE" },
        { id: "o", name: "Ops" },
      ]);
      expect(problem(rename("ops"))).toBe(
        "There’s already a folder called Ops.",
      );
      expect(problem(rename("Stripe"))).toBe(
        "There’s already a folder called Stripe.",
      );
      expect(problem(rename("Other", "gone"))).toBe(
        "That folder isn’t in Connectors any more.",
      );
    });

    it("deletes one, its connectors back at the end of their kinds' folders", () => {
      const l = layout({
        folders: [{ id: "f", name: "Finance" }],
        order: {
          root: ["folder:f", "documents"],
          "folder:f": ["a", "p"],
          "kind:stripe": ["b"],
        },
      });
      const gone = changed(
        applyChange(l, DATA, { type: "delete-folder", id: "f" }),
      );
      expect(gone).toEqual({
        names: {},
        folders: [],
        order: { root: ["documents"], "kind:stripe": ["b"] },
      });
      expect(shape(arrange(gone, DATA)).slice(1)).toEqual([
        ["kind:stripe", ["b", "a"]],
        ["kind:postgres", ["p"]],
      ]);
      expect(
        problem(applyChange(gone, DATA, { type: "delete-folder", id: "f" })),
      ).toBe("That folder isn’t in Connectors any more.");
    });
  });

  it("renames a kind's folder and documents/, and forgets a name back to the usual one", () => {
    const named = changed(
      applyChange(EMPTY_LAYOUT, DATA, {
        type: "rename",
        key: "kind:stripe",
        name: " Payments ",
      }),
    );
    expect(named.names).toEqual({ "kind:stripe": "Payments" });
    const files = changed(
      applyChange(named, DATA, {
        type: "rename",
        key: "documents",
        name: "Files",
      }),
    );
    expect(arrange(files, DATA).map((n) => n.name)).toEqual([
      "Payments",
      "PostgreSQL",
      "Files",
    ]);
    const usual = changed(
      applyChange(files, DATA, {
        type: "rename",
        key: "kind:stripe",
        name: "Stripe",
      }),
    );
    expect(usual.names).toEqual({ documents: "Files" });
    expect(
      changed(
        applyChange(usual, DATA, {
          type: "rename",
          key: "documents",
          name: DOCUMENTS_NAME,
        }),
      ).names,
    ).toEqual({});
    expect(
      problem(
        applyChange(files, DATA, {
          type: "rename",
          key: "kind:postgres",
          name: "payments",
        }),
      ),
    ).toBe("There’s already a folder called Payments.");
  });

  it("writes the layout pruned, so ids the workspace lost leave it", () => {
    const l = layout({
      names: { "kind:gone": "Old" },
      order: { "kind:stripe": ["deleted", "b"], "set:gone": ["x"] },
    });
    expect(
      changed(
        applyChange(l, DATA, {
          type: "move-root",
          key: "documents",
          before: null,
        }),
      ),
    ).toEqual({
      names: {},
      folders: [],
      order: {
        "kind:stripe": ["b"],
        root: ["kind:stripe", "kind:postgres", "documents"],
      },
    });
  });

  it("reapplies the same change on a layout someone else changed", () => {
    const change = {
      type: "move-connector",
      id: "a",
      folder: "f",
      before: null,
    } as const;
    const theirs = layout({
      folders: [{ id: "f", name: "Finance" }],
      order: { documents: ["set:s"], "folder:f": ["p"] },
    });
    expect(changed(applyChange(theirs, DATA, change)).order).toEqual({
      documents: ["set:s"],
      "folder:f": ["p", "a"],
    });
    // Their write deleted the folder: the change can't be made.
    expect(problem(applyChange(EMPTY_LAYOUT, DATA, change))).toBe(
      "That folder isn’t in Connectors any more.",
    );
  });

  it("refuses a change that takes the layout over 64 KiB", () => {
    const id = (i: number) => `doc_${String(i).padStart(36, "0")}`;
    const data: TreeData = {
      ...DATA,
      documents: Array.from({ length: 1600 }, (_, i) =>
        doc(id(i), "g", "2026-10-01"),
      ),
    };
    // Each id costs 44 bytes in an order: 1,600 of them is over.
    expect(
      problem(
        applyChange(EMPTY_LAYOUT, data, {
          type: "move-document",
          id: id(1),
          before: id(0),
        }),
      ),
    ).toBe(
      "The Connectors layout is full. Delete a folder or shorten a name, then try again.",
    );
    const fits: TreeData = {
      ...data,
      documents: data.documents.slice(0, 1400),
    };
    const moved = changed(
      applyChange(EMPTY_LAYOUT, fits, {
        type: "move-document",
        id: id(1),
        before: id(0),
      }),
    );
    expect(layoutBytes(moved)).toBeLessThanOrEqual(MAX_LAYOUT_BYTES);
  });
});

describe("moving step by step", () => {
  it("finds what an item goes before one place up or down", () => {
    expect(stepBefore(["a", "b", "c"], "a", 1)).toBe("c");
    expect(stepBefore(["a", "b", "c"], "b", 1)).toBeNull();
    expect(stepBefore(["a", "b", "c"], "c", 1)).toBeUndefined();
    expect(stepBefore(["a", "b", "c"], "a", -1)).toBeUndefined();
    expect(stepBefore(["a", "b", "c"], "c", -1)).toBe("b");
    expect(stepBefore(["a", "b", "c"], "x", -1)).toBeUndefined();
  });

  it("finds what a dropped item goes before, as a sortable list shows it", () => {
    expect(dropBefore(["a", "b", "c"], "a", "b")).toBe("c");
    expect(dropBefore(["a", "b", "c"], "a", "c")).toBeNull();
    expect(dropBefore(["a", "b", "c"], "c", "a")).toBe("a");
    expect(dropBefore(["a", "b", "c"], "b", "b")).toBeUndefined();
    expect(dropBefore(["a", "b", "c"], "x", "b")).toBeUndefined();
    expect(dropBefore(["a", "b", "c"], "a", "x")).toBeUndefined();
  });

  it("lists where a connector can move", () => {
    expect(
      destinations(
        {
          names: {},
          folders: [
            { id: "f", name: "Finance" },
            { id: "g", name: "Ops" },
          ],
          order: { "folder:f": ["a"] },
        },
        {
          kinds: ["stripe"],
          sets: [],
          documents: [],
          connectors: [{ id: "a", kind: "stripe" }],
          label: () => "Stripe",
        },
        "a",
      ),
    ).toEqual([
      { folder: null, name: "Stripe" },
      { folder: "g", name: "Ops" },
    ]);
    const named = layout({
      names: { "kind:stripe": "Payments" },
      folders: [{ id: "f", name: "Finance" }],
      order: { "folder:f": ["a"] },
    });
    expect(destinations(named, DATA, "a")).toEqual([
      { folder: null, name: "Payments" },
    ]);
    expect(destinations(named, DATA, "b")).toEqual([
      { folder: "f", name: "Finance" },
    ]);
    expect(destinations(named, { ...DATA, label: undefined }, "x")).toEqual([]);
  });

  it("refuses a folder name like the example", () => {
    expect(
      folderNameProblem(" stripe ", [
        {
          type: "kind",
          key: "kind:stripe",
          kind: "stripe",
          name: "Stripe",
          items: [],
        },
      ]),
    ).toBe("There’s already a folder called Stripe.");
  });
});
