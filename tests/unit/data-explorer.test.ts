import { describe, expect, it } from "vitest";
import type { Connector, FoundryDocument } from "@/shared/wfe";
import {
  EMPTY_LAYOUT,
  arrange,
  explorerRows,
  rowKeyAction,
  selectedRow,
  selectionOf,
  tableKey,
  type ExplorerRow,
  type TreeData,
} from "@/web/areas/data/layout";

// The Data explorer's logic: what a Data page's URL opens, the rows the
// tree shows and which of them the URL selects (and the folders that hold
// it), and what a key does on a row.

const connector = (
  id: string,
  kind: string,
  tables: string[] = [],
): Connector => ({
  id,
  kind,
  name: id,
  tables: tables.map((name) => ({ name })),
});
const doc = (id: string, setId: string): FoundryDocument =>
  ({ id, name: `${id}.md`, setId, createdAt: "2026-10-01" }) as FoundryDocument;

// Stripe a (customers, charges); PostgreSQL p; sets g (d1, d2) and s (none).
const DATA: TreeData = {
  kinds: ["stripe", "postgres"],
  connectors: [
    connector("a", "stripe", ["customers", "charges"]),
    connector("p", "postgres", ["orders"]),
  ],
  sets: [
    { id: "g", name: "General" },
    { id: "s", name: "Board" },
  ],
  documents: [doc("d1", "g"), doc("d2", "g")],
};
const ALL = arrange(EMPTY_LAYOUT, DATA);
const open = () => true;

describe("what a Data page's URL opens", () => {
  it("matches the example", () => {
    expect(selectionOf("/data/connections/conn_1")).toEqual({
      type: "connection",
      id: "conn_1",
      table: null,
    });
    expect(
      selectionOf("/data/connections/conn_1/tables/order%2Fitems"),
    ).toEqual({ type: "connection", id: "conn_1", table: "order/items" });
    expect(selectionOf("/data/documents/doc_1")).toEqual({
      type: "document",
      id: "doc_1",
    });
    expect(selectionOf("/data")).toBeNull();
  });

  it("decodes each segment and ignores a trailing slash", () => {
    expect(selectionOf("/data/documents/doc%201/")).toEqual({
      type: "document",
      id: "doc 1",
    });
    expect(selectionOf("/data/connections/c/tables/t/")).toEqual({
      type: "connection",
      id: "c",
      table: "t",
    });
  });

  it("opens nothing on other pages or paths it doesn't know", () => {
    for (const path of [
      "/",
      "/data/",
      "/ontology/connections/c",
      "data/connections/c",
      "/data/connections",
      "/data/connections/c/tables",
      "/data/connections/c/columns/t",
      "/data/connections/c/tables/t/more",
      "/data/documents/d/text",
      "/data/sets/s",
      "/data/connections/%E0%A4%A",
    ])
      expect(selectionOf(path), path).toBeNull();
  });
});

describe("the explorer's rows", () => {
  it("matches the example", () => {
    expect(
      explorerRows(
        arrange(EMPTY_LAYOUT, {
          kinds: ["stripe"],
          connectors: [{ id: "a", kind: "stripe" }],
          sets: [{ id: "g", name: "General" }],
          documents: [{ id: "d", name: "a.md", setId: "g" } as FoundryDocument],
        }),
        (key) => key !== "set:g",
      ),
    ).toEqual([
      { key: "kind:stripe", level: 1, state: "open" },
      { key: "c:a", level: 2, state: "leaf" },
      { key: "documents", level: 1, state: "open" },
      { key: "set:g", level: 2, state: "closed" },
    ]);
  });

  it("lists every row of an open tree, sets empty or not", () => {
    expect(explorerRows(ALL, open)).toEqual([
      { key: "kind:stripe", level: 1, state: "open" },
      { key: "c:a", level: 2, state: "open" },
      { key: "t:a/customers", level: 3, state: "leaf" },
      { key: "t:a/charges", level: 3, state: "leaf" },
      { key: "kind:postgres", level: 1, state: "open" },
      { key: "c:p", level: 2, state: "open" },
      { key: "t:p/orders", level: 3, state: "leaf" },
      { key: "documents", level: 1, state: "open" },
      { key: "set:g", level: 2, state: "open" },
      { key: "d:d1", level: 3, state: "leaf" },
      { key: "d:d2", level: 3, state: "leaf" },
      { key: "set:s", level: 2, state: "open" },
    ]);
  });

  it("leaves out what a closed folder holds", () => {
    expect(
      explorerRows(ALL, (key) => key !== "documents" && key !== "kind:stripe"),
    ).toEqual([
      { key: "kind:stripe", level: 1, state: "closed" },
      { key: "kind:postgres", level: 1, state: "open" },
      { key: "c:p", level: 2, state: "open" },
      { key: "t:p/orders", level: 3, state: "leaf" },
      { key: "documents", level: 1, state: "closed" },
    ]);
    // A closed connection hides its tables.
    expect(explorerRows(ALL, (key) => key !== "c:a").slice(0, 3)).toEqual([
      { key: "kind:stripe", level: 1, state: "open" },
      { key: "c:a", level: 2, state: "closed" },
      { key: "kind:postgres", level: 1, state: "open" },
    ]);
  });

  it("opens a connection on the tables a search found", () => {
    expect(explorerRows(arrange(EMPTY_LAYOUT, DATA, "cust"), open)).toEqual([
      { key: "kind:stripe", level: 1, state: "open" },
      { key: "c:a", level: 2, state: "open" },
      { key: tableKey("a", "customers"), level: 3, state: "leaf" },
    ]);
    expect(tableKey("a", "customers")).toBe("t:a/customers");
  });

  it("shows nothing for an empty tree", () => {
    expect(explorerRows([], open)).toEqual([]);
  });
});

describe("the row a URL selects", () => {
  it("matches the example", () => {
    expect(
      selectedRow(
        arrange(EMPTY_LAYOUT, {
          kinds: [],
          connectors: [],
          sets: [{ id: "g", name: "General" }],
          documents: [{ id: "d", name: "a.md", setId: "g" } as FoundryDocument],
        }),
        { type: "document", id: "d" },
      ),
    ).toEqual({ key: "d:d", folders: ["documents", "set:g"] });
  });

  it("selects a connection in its folder, for its page and its tables'", () => {
    const folder = arrange(
      {
        names: {},
        folders: [{ id: "f", name: "Finance" }],
        order: { "folder:f": ["p"] },
      },
      DATA,
    );
    expect(
      selectedRow(folder, { type: "connection", id: "p", table: null }),
    ).toEqual({ key: "c:p", folders: ["folder:f"] });
    expect(
      selectedRow(folder, { type: "connection", id: "a", table: "customers" }),
    ).toEqual({ key: "t:a/customers", folders: ["kind:stripe", "c:a"] });
    // A table it doesn't list selects the connection.
    expect(
      selectedRow(folder, { type: "connection", id: "a", table: "gone" }),
    ).toEqual({ key: "c:a", folders: ["kind:stripe"] });
  });

  it("selects a table's row while a search shows it", () => {
    const found = arrange(EMPTY_LAYOUT, DATA, "cust");
    expect(
      selectedRow(found, { type: "connection", id: "a", table: "customers" }),
    ).toEqual({ key: "t:a/customers", folders: ["kind:stripe", "c:a"] });
    // A table the search didn't find selects its connection.
    expect(
      selectedRow(found, { type: "connection", id: "a", table: "charges" }),
    ).toEqual({ key: "c:a", folders: ["kind:stripe"] });
  });

  it("selects nothing the tree doesn't show", () => {
    expect(selectedRow(ALL, null)).toBeNull();
    expect(
      selectedRow(ALL, { type: "connection", id: "gone", table: null }),
    ).toBeNull();
    expect(selectedRow(ALL, { type: "document", id: "gone" })).toBeNull();
    // A search that leaves it out.
    expect(
      selectedRow(arrange(EMPTY_LAYOUT, DATA, "orders"), {
        type: "document",
        id: "d1",
      }),
    ).toBeNull();
  });
});

describe("a connection's own sets", () => {
  // Granola gr (notes) owns Notes n; General g holds the upload d1.
  const notes = {
    id: "n",
    name: "Notes",
    documents: 418,
    connector: { connectorId: "gr", stream: "notes" },
  };
  const data: TreeData = {
    kinds: ["granola"],
    connectors: [connector("gr", "granola", ["notes"])],
    sets: [{ id: "g", name: "General" }, notes],
    documents: [doc("d1", "g")],
  };
  const tree = arrange(EMPTY_LAYOUT, data);
  // The pages a set has read so far.
  const pages = (setId: string) =>
    setId === "n" ? [doc("n1", "n"), doc("n2", "n")] : [];

  it("lists them under the connection after its tables, with the documents read so far", () => {
    expect(explorerRows(tree, open, pages)).toEqual([
      { key: "kind:granola", level: 1, state: "open" },
      { key: "c:gr", level: 2, state: "open" },
      { key: "t:gr/notes", level: 3, state: "leaf" },
      { key: "set:n", level: 3, state: "open" },
      { key: "d:n1", level: 4, state: "leaf" },
      { key: "d:n2", level: 4, state: "leaf" },
      { key: "documents", level: 1, state: "open" },
      { key: "set:g", level: 2, state: "open" },
      { key: "d:d1", level: 3, state: "leaf" },
    ]);
    // Closed, a set shows none; with nothing read, an open one holds none.
    expect(
      explorerRows(tree, (key) => key !== "set:n", pages).slice(3, 5),
    ).toEqual([
      { key: "set:n", level: 3, state: "closed" },
      { key: "documents", level: 1, state: "open" },
    ]);
    expect(explorerRows(tree, open).slice(3, 5)).toEqual([
      { key: "set:n", level: 3, state: "open" },
      { key: "documents", level: 1, state: "open" },
    ]);
  });

  it("selects a document of one by its set, opening the connection and the set", () => {
    const page = { type: "document", id: "n2" } as const;
    expect(selectedRow(tree, page, "n")).toEqual({
      key: "d:n2",
      folders: ["kind:granola", "c:gr", "set:n"],
    });
    // Until the document says its set, or for a set the tree doesn't show.
    expect(selectedRow(tree, page)).toBeNull();
    expect(selectedRow(tree, page, "gone")).toBeNull();
    // An upload is found in documents/, whatever set it names.
    expect(selectedRow(tree, { type: "document", id: "d1" }, "g")).toEqual({
      key: "d:d1",
      folders: ["documents", "set:g"],
    });
  });
});

describe("keys on a row", () => {
  // Stripe (open): a; PostgreSQL (closed); documents/ (open): General
  // (open): d1; Board (open, empty).
  const ROWS: ExplorerRow[] = [
    { key: "kind:stripe", level: 1, state: "open" },
    { key: "c:a", level: 2, state: "leaf" },
    { key: "kind:postgres", level: 1, state: "closed" },
    { key: "documents", level: 1, state: "open" },
    { key: "set:g", level: 2, state: "open" },
    { key: "d:d1", level: 3, state: "leaf" },
    { key: "set:s", level: 2, state: "open" },
  ];
  const act = (key: string, pressed: string) =>
    rowKeyAction(ROWS, key, pressed);

  it("matches the example", () => {
    const rows: ExplorerRow[] = [
      { key: "kind:stripe", level: 1, state: "open" },
      { key: "c:a", level: 2, state: "leaf" },
    ];
    expect(rowKeyAction(rows, "c:a", "ArrowLeft")).toEqual({
      type: "focus",
      key: "kind:stripe",
    });
    expect(rowKeyAction(rows, "kind:stripe", "ArrowLeft")).toEqual({
      type: "toggle",
      key: "kind:stripe",
    });
    expect(rowKeyAction(rows, "c:a", "ArrowDown")).toBeNull();
  });

  it("moves up and down, and to either end, stopping there", () => {
    expect(act("c:a", "ArrowDown")).toEqual({
      type: "focus",
      key: "kind:postgres",
    });
    expect(act("c:a", "ArrowUp")).toEqual({
      type: "focus",
      key: "kind:stripe",
    });
    expect(act("kind:stripe", "ArrowUp")).toBeNull();
    expect(act("set:s", "ArrowDown")).toBeNull();
    expect(act("d:d1", "Home")).toEqual({ type: "focus", key: "kind:stripe" });
    expect(act("c:a", "End")).toEqual({ type: "focus", key: "set:s" });
  });

  it("opens a closed folder, then moves into it", () => {
    expect(act("kind:postgres", "ArrowRight")).toEqual({
      type: "toggle",
      key: "kind:postgres",
    });
    expect(act("documents", "ArrowRight")).toEqual({
      type: "focus",
      key: "set:g",
    });
    expect(act("set:g", "ArrowRight")).toEqual({ type: "focus", key: "d:d1" });
    // An open folder holding nothing, and a leaf, go nowhere.
    expect(act("set:s", "ArrowRight")).toBeNull();
    expect(act("d:d1", "ArrowRight")).toBeNull();
  });

  it("closes an open folder, else moves to the row's folder", () => {
    expect(act("set:g", "ArrowLeft")).toEqual({ type: "toggle", key: "set:g" });
    expect(act("d:d1", "ArrowLeft")).toEqual({ type: "focus", key: "set:g" });
    expect(act("kind:postgres", "ArrowLeft")).toBeNull();
  });

  it("opens and renames a row, and leaves other keys alone", () => {
    expect(act("c:a", "Enter")).toEqual({ type: "open", key: "c:a" });
    expect(act("documents", "Enter")).toEqual({
      type: "open",
      key: "documents",
    });
    expect(act("d:d1", "F2")).toEqual({ type: "rename", key: "d:d1" });
    expect(act("c:a", "a")).toBeNull();
    expect(act("c:a", "Tab")).toBeNull();
    expect(act("c:gone", "ArrowDown")).toBeNull();
    expect(rowKeyAction([], "c:a", "Home")).toBeNull();
  });
});
