import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import shopBundle from "../../mock/fixtures/agent-bundle.json";
import contractsBundle from "../../mock/fixtures/agent-bundle-contracts.json";
import stripeRows from "../../mock/fixtures/stripe-rows.json";
import { extractDocument, ingestChunk } from "../../mock/graph/extract";
import { scopeOf } from "../../mock/graph/scope";
import { runQuery } from "../../mock/graph/query";
import { MockGraph } from "../../mock/graph/store";
import { FOUNDRY_NODES } from "@/shared/foundry-labels";
import { MAX_COUNTS, type Scope } from "@/web/areas/explore/load-scope";
import { entityId } from "@/web/areas/explore/model";
import {
  NO_NAMING,
  namingOf,
  type TypeNaming,
} from "@/web/areas/explore/naming";
import {
  DEFAULT_COLUMNS,
  MAX_PAGE,
  RECORDS_LIMITS,
  RECORD_COLUMN,
  activeFilter,
  columnsOf,
  detailProps,
  fieldName,
  filterOf,
  pageRead,
  pageSpan,
  parsePage,
  parseTypeCounts,
  readView,
  recordTypes,
  searchOf,
  shownColumns,
  sortProperties,
  typeCountsRequests,
  viewParams,
  type RecordsView,
} from "@/web/areas/records/model";

// The Records queries, run through the mock's v3 interpreter over what a
// load of the contracts draft writes: 64 Stripe customers, 65 invoices and
// the seeded contract, beside a node of the team's own.

const naming = namingOf({
  id: "ver_3",
  format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
  definition: contractsBundle,
});
const shop = namingOf({
  id: "ver_2",
  format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
  definition: shopBundle,
});
const type = (name: string, n = naming) => n.types.get(name)!;
const CUSTOMERS: [string, string, string, string][] = [
  ["cus_GLOBEX01", "Globex Corporation", "ap@globex.example", "usd"],
  ["cus_INITECH2", "Initech LLC", "billing@initech.example", "eur"],
  ["cus_HOOLI003", "Hooli Inc", "finance@hooli.example", "usd"],
  ...(stripeRows.customers as [string, string, string, string][]),
];
const INVOICES: [string, string, number, string][] = [
  ["in_1001", "cus_GLOBEX01", 1200000, "paid"],
  ["in_1002", "cus_GLOBEX01", 1200000, "open"],
  ["in_1003", "cus_INITECH2", 104200, "paid"],
  ["in_1004", "cus_HOOLI003", 54000, "void"],
  ...(stripeRows.invoices as [string, string, number, string][]),
];

const g = new MockGraph();
// The load the tests' reads are scoped to.
const LOAD = "load_1";
const SCOPE: Scope = { kind: "load", id: LOAD };
const chunk = {
  source: "src_customers",
  mapping: "customers",
  snapshot: "2026-10-17T10:00:00.000Z",
  ontologyVersion: "foundry-v3",
};
ingestChunk(
  g.view(LOAD),
  chunk,
  CUSTOMERS.map(([key, name, email, currency]) => ({
    type: "Customer",
    key,
    name,
    props: { customer_id: key, company_name: name, email, currency },
    names: ["company_name"],
  })),
);
ingestChunk(
  g.view(LOAD),
  { ...chunk, source: "src_invoices", mapping: "invoices" },
  INVOICES.map(([key, customer, amount, status]) => ({
    type: "Invoice",
    key,
    name: key,
    props: { invoice_id: key, amount_due: amount, status },
    links: [{ link: "billed_to", toType: "Customer", toKey: customer }],
  })),
);
extractDocument(
  g.view(LOAD),
  {
    id: "doc_1",
    name: "contract.md",
    text: readFileSync(
      new URL("../../mock/fixtures/contract.md", import.meta.url),
      "utf8",
    ),
    markdown: true,
    ontologyVersion: "foundry-v3",
  },
  {
    types: new Set(["Contract", "Customer", "Invoice"]),
    names: new Map([["Customer", "company_name"]]),
  },
  // What the contracts route lets the document fill: Contracts, found
  // customers and signed_with.
  scopeOf({ Contract: { create: true }, Customer: {} }, ["signed_with"]),
);
g.addNode("Account", { account_id: "acc_1", name: "Ada Lovelace" });

const run = (req: unknown) => {
  const answer = runQuery(g, req);
  expect(answer.status, JSON.stringify(answer.body)).toBe(200);
  return answer.body;
};
const view = (v: Partial<RecordsView> = {}): RecordsView => ({
  type: "Customer",
  search: "",
  filter: null,
  sort: null,
  page: 0,
  record: "",
  ...v,
});
function page(t: TypeNaming, v: Partial<RecordsView> = {}) {
  const read = pageRead(SCOPE, t, view({ type: t.name, ...v }));
  if (read.kind !== "read") throw new Error(`no read: ${read.kind}`);
  return parsePage(run(read.request), naming, t);
}
const names = (p: { rows: { name: string }[] }) => p.rows.map((r) => r.name);

describe("types and counts", () => {
  it("lists the version's types by name, counted from their labels", () => {
    const types = recordTypes(naming).map((t) => t.name);
    expect(types).toEqual(["Contract", "Customer", "Invoice"]);
    const batches = typeCountsRequests(SCOPE, types);
    expect(batches).toHaveLength(1);
    expect(Object.keys(batches[0]!.request.parameters!)).toEqual([]);
    expect(
      parseTypeCounts(
        batches.map((b) => run(b.request)),
        SCOPE,
        batches,
      ),
    ).toEqual(
      new Map([
        ["Contract", 1],
        ["Customer", 64],
        ["Invoice", 65],
      ]),
    );
    expect(recordTypes(NO_NAMING)).toEqual([]);
  });

  it("lists only the ontology's types, never a Foundry label a version names", () => {
    const t = (name: string): TypeNaming => ({
      name,
      props: [],
      key: [],
      names: [],
    });
    const types = recordTypes({
      ...NO_NAMING,
      version: "v",
      types: new Map(
        ["Document", FOUNDRY_NODES.Chunk, "FoundryLater", "Customer"].map(
          (n) => [n, t(n)],
        ),
      ),
    }).map((x) => x.name);
    expect(types).toEqual(["Customer", "Document"]);
  });

  it("reads an answer it can't trust as unknown counts", () => {
    expect(
      parseTypeCounts([{ count_0: -1, count_1: "3" }], SCOPE, [
        { types: ["A", "B", "C"] },
      ]),
    ).toEqual(
      new Map([
        ["A", null],
        ["B", null],
        ["C", null],
      ]),
    );
    expect(parseTypeCounts([null], SCOPE, [{ types: ["A"] }])).toEqual(
      new Map([["A", null]]),
    );
    expect(parseTypeCounts([], SCOPE, [{ types: ["A"] }])).toEqual(
      new Map([["A", null]]),
    );
  });

  const entries = (b: { request: { query: { read?: unknown } } }) =>
    (b.request.query.read as { entries: unknown[] }).entries.length;

  it("counts at most maxTypes types, at most MAX_COUNTS a request", () => {
    const many = Array.from({ length: 150 }, (_, i) => `T${i}`);
    const batches = typeCountsRequests(SCOPE, many);
    expect(batches.map(entries)).toEqual([60, 40]);
    expect(batches.flatMap((b) => b.types)).toEqual(
      many.slice(0, RECORDS_LIMITS.maxTypes),
    );
    expect(
      parseTypeCounts(
        batches.map(() => ({})),
        SCOPE,
        batches,
      ).size,
    ).toBe(RECORDS_LIMITS.maxTypes);
    expect(typeCountsRequests(SCOPE, [])).toEqual([]);
  });

  it("splits Other data's counts by cost: a version's type takes one per load", () => {
    const loads = ["L1", "L2", "L3", "L4", "L5"];
    const types = Array.from({ length: 100 }, (_, i) => `T${i}`);
    const other: Scope = {
      kind: "other",
      loads,
      shared: { nodes: new Set(types.slice(0, 90)), edges: new Set() },
    };
    const batches = typeCountsRequests(other, types);
    // 90 shared types at 6 counts each, then 10 plain ones at 1.
    for (const b of batches) expect(entries(b)).toBeLessThanOrEqual(MAX_COUNTS);
    expect(batches.map(entries).reduce((n, x) => n + x, 0)).toBe(90 * 6 + 10);
    expect(batches.flatMap((b) => b.types)).toEqual(types);
    // Each batch's answers are its own: Other(T) = count(T) − Σ count(T ∧ L).
    const answers = batches.map((b) =>
      Object.fromEntries(
        b.types.flatMap((t, i) => [
          [`count_${i}`, 10],
          ...loads.map((_, k) => [`count_${i}_load_${k}`, 1]),
        ]),
      ),
    );
    const counts = parseTypeCounts(answers, other, batches);
    expect(counts.get("T0")).toBe(5);
    expect(counts.get("T89")).toBe(5);
    expect(counts.get("T99")).toBe(10);
  });
});

describe("two loads and the team's own data side by side", () => {
  // The same customers in two loads, each with its own name for the first,
  // and a Customer the team added itself.
  const db = new MockGraph();
  for (const [load, suffix] of [
    ["L1", ""],
    ["L2", " (v2)"],
  ] as const)
    ingestChunk(
      db.view(load),
      chunk,
      CUSTOMERS.slice(0, 3).map(([key, name, email, currency], i) => ({
        type: "Customer",
        key,
        name: i === 0 ? name + suffix : name,
        props: {
          customer_id: key,
          company_name: i === 0 ? name + suffix : name,
          email,
          currency,
        },
        names: ["company_name"],
      })),
    );
  db.addNode("Customer", {
    entity_key: "cus_TEAM",
    name: "Team account",
    company_name: "Team account",
  });
  const runIn = (req: unknown) => {
    const answer = runQuery(db, req);
    expect(answer.status, JSON.stringify(answer.body)).toBe(200);
    return answer.body;
  };
  const L1: Scope = { kind: "load", id: "L1" },
    L2: Scope = { kind: "load", id: "L2" },
    OTHER: Scope = {
      kind: "other",
      loads: ["L1", "L2"],
      shared: { nodes: new Set(["Customer"]), edges: new Set() },
    };
  const counted = (scope: Scope) => {
    const batches = typeCountsRequests(scope, ["Customer"]);
    return parseTypeCounts(
      batches.map((b) => runIn(b.request)),
      scope,
      batches,
    ).get("Customer");
  };
  const rowsOf = (scope: Scope, v: Partial<RecordsView> = {}) => {
    const read = pageRead(scope, type("Customer"), view(v));
    if (read.kind !== "read") throw new Error(`no read: ${read.kind}`);
    return names(parsePage(runIn(read.request), naming, type("Customer")));
  };

  it("counts and pages each load's rows only, and the team's as Other data", () => {
    expect([counted(L1), counted(L2), counted(OTHER)]).toEqual([3, 3, 1]);
    expect(rowsOf(L1)).toContain("Globex Corporation");
    expect(rowsOf(L2)).toContain("Globex Corporation (v2)");
    expect(rowsOf(L1)).toHaveLength(3);
    expect(rowsOf(L2)).toHaveLength(3);
    expect(rowsOf(OTHER)).toEqual(["Team account"]);
    // Search, filter and sort stay in the load.
    expect(rowsOf(L2, { search: "globex" })).toEqual([
      "Globex Corporation (v2)",
    ]);
    expect(rowsOf(L1, { filter: { field: "currency", value: "eur" } })).toEqual(
      ["Initech LLC"],
    );
    expect(
      rowsOf(L2, { sort: { column: RECORD_COLUMN, dir: "desc" } }),
    ).toHaveLength(3);
  });
});

describe("columns and sorts", () => {
  it("puts the record's name first, then the key, then names, then the rest", () => {
    expect(columnsOf(type("Customer")).map((c) => [c.key, c.role])).toEqual([
      [RECORD_COLUMN, "record"],
      ["customer_id", "key"],
      ["company_name", "names"],
      ["currency", "prop"],
      ["email", "prop"],
    ]);
    expect(columnsOf(type("Invoice")).map((c) => [c.key, c.type])).toEqual([
      [RECORD_COLUMN, "name"],
      ["invoice_id", "string"],
      ["amount_due", "integer"],
      ["status", "string"],
    ]);
    // Documents fill a Contract's props: they are columns like any other.
    expect(columnsOf(type("Contract")).map((c) => c.key)).toEqual([
      RECORD_COLUMN,
      "signed_on",
      "term_months",
    ]);
    expect(columnsOf(type("OrderLine", shop)).map((c) => c.key)).toEqual([
      RECORD_COLUMN,
      "order_number",
      "line_no",
      "qty",
    ]);
    // The layout's normalized names never become columns.
    expect(
      columnsOf({
        name: "X",
        props: [
          { name: "a__nn", type: "string" },
          { name: "$id", type: "string" },
        ],
        key: [],
        names: [],
      }).map((c) => c.key),
    ).toEqual([RECORD_COLUMN]);
  });

  it("shows a type's other props by default, at most four, or the chosen ones", () => {
    const keys = (t: TypeNaming, chosen?: string[]) =>
      shownColumns(t, chosen).map((c) => c.key);
    // The record column stands for the key and names.
    expect(keys(type("Customer"))).toEqual([
      RECORD_COLUMN,
      "currency",
      "email",
    ]);
    expect(keys(type("Invoice"))).toEqual([
      RECORD_COLUMN,
      "amount_due",
      "status",
    ]);
    expect(keys(type("Contract"))).toEqual([
      RECORD_COLUMN,
      "signed_on",
      "term_months",
    ]);
    const wide: TypeNaming = {
      name: "W",
      props: ["id", "a", "b", "c", "d", "e"].map((name) => ({
        name,
        type: "string",
      })),
      key: ["id"],
      names: [],
    };
    expect(keys(wide)).toEqual([RECORD_COLUMN, "a", "b", "c", "d"]);
    expect(DEFAULT_COLUMNS).toBe(4);
    // Chosen columns show in column order; one the type lacks is left out.
    expect(keys(type("Customer"), ["email", "nope", "company_name"])).toEqual([
      RECORD_COLUMN,
      "company_name",
      "email",
    ]);
    expect(keys(type("Customer"), [])).toEqual([RECORD_COLUMN]);
  });

  it("labels props as helix-foundry writes field names", () => {
    expect(fieldName("company_name")).toBe("Company name");
    expect(fieldName("amountDue")).toBe("Amount Due");
    expect(fieldName("email")).toBe("Email");
    expect(fieldName("")).toBe("");
  });

  it("sorts names by their normalized form and records by their name", () => {
    const c = type("Customer");
    expect(sortProperties(c, RECORD_COLUMN)).toEqual(["company_name__nn"]);
    expect(sortProperties(c, "company_name")).toEqual(["company_name__nn"]);
    expect(sortProperties(c, "email")).toEqual(["email"]);
    expect(sortProperties(c, "nope")).toEqual([]);
    expect(sortProperties(type("Invoice"), RECORD_COLUMN)).toEqual([
      "invoice_id",
    ]);
    expect(sortProperties(type("OrderLine", shop), RECORD_COLUMN)).toEqual([
      "order_number",
      "line_no",
    ]);
    expect(sortProperties(type("Contract"), RECORD_COLUMN)).toEqual([
      "norm_name",
    ]);
    // A type known by its name keeps no normalized names beside them.
    expect(
      sortProperties(
        { name: "Tag", props: [], key: [], names: ["label"] },
        RECORD_COLUMN,
      ),
    ).toEqual(["label"]);
  });
});

describe("the URL", () => {
  it("round-trips a view", () => {
    const v = view({
      search: "globex",
      filter: { field: "currency", value: "eur" },
      sort: { column: "email", dir: "desc" },
      page: 3,
      record: "abc",
    });
    expect(readView(new URLSearchParams(viewParams(v)))).toEqual(v);
    expect(viewParams(view({ type: "" }))).toEqual({});
    expect(viewParams(view({ sort: { column: "email", dir: "asc" } }))).toEqual(
      { type: "Customer", sort: "email" },
    );
  });

  it("reads anything out of range as its default", () => {
    const v = readView(
      new URLSearchParams("page=-4&dir=sideways&sort=email&value=x"),
    );
    expect(v).toEqual({
      type: "",
      search: "",
      filter: null,
      sort: { column: "email", dir: "asc" },
      page: 0,
      record: "",
    });
    expect(readView(new URLSearchParams("page=nope")).page).toBe(0);
    expect(readView(new URLSearchParams("page=99999")).page).toBe(MAX_PAGE);
  });
});

describe("search and filters", () => {
  it("folds the text and looks in names, the key and the stored name", () => {
    expect(searchOf(type("Customer"), "  ")).toEqual({ kind: "all" });
    const s = searchOf(type("Customer"), " Globex, Corp ");
    expect(s.kind === "match" && s.params).toEqual({
      qn: { value: "globex corp", type: "string" },
      q: { value: "Globex, Corp", type: "string" },
      // A string key is also tried upper- and lower-cased.
      qu: { value: "GLOBEX, CORP", type: "string" },
      ql: { value: "globex, corp", type: "string" },
    });
    const lower = searchOf(type("Invoice"), "in_1");
    expect(lower.kind === "match" && Object.keys(lower.params)).toEqual([
      "q",
      "qu",
    ]);
    // A type known by its name: its normalized and stored name.
    const named = searchOf(type("Contract"), "Master");
    expect(JSON.stringify(named)).toContain("norm_name");
    // A number-only key matches a number exactly, and words never.
    const channel = type("SalesChannel", shop);
    const code = searchOf(channel, "7");
    expect(code.kind === "match" && code.params).toEqual({
      qi: { value: 7, type: "i64" },
    });
    expect(searchOf(channel, "web")).toEqual({ kind: "none" });
    expect(pageRead(SCOPE, channel, view({ search: "web" }))).toEqual({
      kind: "none",
    });
    const weight = searchOf(
      {
        name: "P",
        props: [{ name: "w", type: "float" }],
        key: ["w"],
        names: [],
      },
      "2.5",
    );
    expect(weight.kind === "match" && weight.params).toEqual({
      qf: { value: 2.5, type: "f64" },
    });
  });

  it("reads a filter's value by the prop's type", () => {
    const inv = type("Invoice");
    expect(filterOf(inv, "amount_due", "54000")).toEqual({
      ok: true,
      property: "amount_due",
      value: 54000,
      type: "i64",
    });
    expect(filterOf(inv, "amount_due", "54k")).toEqual({
      ok: false,
      error: "Enter a whole number.",
    });
    expect(filterOf(inv, "status", " paid ")).toEqual({
      ok: true,
      property: "status",
      value: "paid",
      type: "string",
    });
    expect(filterOf(inv, "nope", "x")).toMatchObject({ ok: false });
    expect(filterOf(inv, RECORD_COLUMN, "x")).toMatchObject({ ok: false });
    expect(filterOf(inv, "status", " ")).toEqual({
      ok: false,
      error: "Enter a value to match.",
    });
    // A names prop compares its normalized form.
    expect(
      filterOf(type("Customer"), "company_name", "GLOBEX corporation"),
    ).toEqual({
      ok: true,
      property: "company_name__nn",
      value: "globex corporation",
      type: "string",
    });
    expect(filterOf(type("Customer"), "company_name", "...")).toMatchObject({
      ok: false,
    });
    const t: TypeNaming = {
      name: "T",
      props: [
        { name: "f", type: "float" },
        { name: "b", type: "boolean" },
      ],
      key: [],
      names: [],
    };
    expect(filterOf(t, "f", "2.5")).toMatchObject({ value: 2.5, type: "f64" });
    expect(filterOf(t, "f", "x")).toMatchObject({ ok: false });
    expect(filterOf(t, "b", "Yes")).toMatchObject({
      value: true,
      type: "bool",
    });
    expect(filterOf(t, "b", "no")).toMatchObject({ value: false });
    expect(filterOf(t, "b", "maybe")).toMatchObject({ ok: false });
    expect(
      pageRead(
        SCOPE,
        inv,
        view({ filter: { field: "amount_due", value: "x" } }),
      ),
    ).toEqual({
      kind: "refused",
      reason: "filter",
      message: "Enter a whole number.",
    });
  });
});

describe("a page of records", () => {
  it("reads a type in database order, a page at a time", () => {
    const first = page(type("Customer"));
    expect(first.rows).toHaveLength(RECORDS_LIMITS.page);
    expect(first.more).toBe(true);
    expect(first.matched).toBe(null);
    expect(first.rows[0]).toEqual({
      dbId: expect.any(Number),
      entity: {
        id: entityId({ type: "Customer", key: "cus_GLOBEX01" }),
        identity: { type: "Customer", key: "cus_GLOBEX01" },
      },
      name: "Globex Corporation",
      by: "names",
      props: {
        customer_id: "cus_GLOBEX01",
        company_name: "Globex Corporation",
        currency: "usd",
        email: "ap@globex.example",
      },
    });
    const second = page(type("Customer"), { page: 1 });
    expect(second.rows).toHaveLength(14);
    expect(second.more).toBe(false);
    expect(new Set([...names(first), ...names(second)]).size).toBe(64);
  });

  it("searches names however they are written", () => {
    for (const q of ["globex", "GLOBEX corp", "Globex Corporation"])
      expect(names(page(type("Customer"), { search: q })), q).toEqual([
        "Globex Corporation",
      ]);
    const amp = page(type("Customer"), { search: "alder & finch" });
    expect(names(amp)).toEqual(["Alder & Finch LLP"]);
    expect(amp.matched).toEqual({ count: 1, capped: false });
    // By key too: as typed, or a key written in one case typed in the other.
    expect(names(page(type("Customer"), { search: "cus_HOOLI" }))).toEqual([
      "Hooli Inc",
    ]);
    expect(names(page(type("Invoice"), { search: "IN_1004" }))).toEqual([
      "in_1004",
    ]);
    expect(page(type("Customer"), { search: "nobody" })).toEqual({
      rows: [],
      more: false,
      matched: { count: 0, capped: false },
    });
    expect(names(page(type("Invoice"), { search: "in_100" }))).toEqual(
      INVOICES.filter(([k]) => k.includes("in_100")).map(([k]) => k),
    );
    expect(names(page(type("Contract"), { search: "services" }))).toEqual([
      "Master services agreement",
    ]);
  });

  it("sorts by a column, names case-blind", () => {
    const sorted = [...CUSTOMERS.map(([, n]) => n)].sort((a, b) =>
      a.toLowerCase().localeCompare(b.toLowerCase()),
    );
    expect(
      names(
        page(type("Customer"), { sort: { column: RECORD_COLUMN, dir: "asc" } }),
      ).slice(0, 3),
    ).toEqual([
      "Alder & Finch LLP",
      "Aldermoor Capital Holdings",
      "Ashcombe Dental Group",
    ]);
    const desc = page(type("Customer"), {
      sort: { column: "company_name", dir: "desc" },
    });
    expect(desc.rows[0]?.name).toBe("Zephyrine Studios");
    const last = page(type("Customer"), {
      sort: { column: "company_name", dir: "desc" },
      page: 1,
    });
    expect([...names(desc), ...names(last)]).toEqual([...sorted].reverse());
    const byAmount = page(type("Invoice"), {
      sort: { column: "amount_due", dir: "desc" },
    });
    const amounts = byAmount.rows.map((r) => r.props.amount_due as number);
    expect(amounts).toEqual([...amounts].sort((a, b) => b - a));
    // A column the type doesn't have reads in database order.
    expect(
      names(
        page(type("Customer"), { sort: { column: "nope", dir: "desc" } }),
      )[0],
    ).toBe("Globex Corporation");
  });

  it("filters by a field, and counts the matches", () => {
    const eur = page(type("Customer"), {
      filter: { field: "currency", value: "eur" },
    });
    expect(eur.rows.every((r) => r.props.currency === "eur")).toBe(true);
    expect(eur.matched).toEqual({
      count: CUSTOMERS.filter((c) => c[3] === "eur").length,
      capped: false,
    });
    expect(
      names(
        page(type("Customer"), {
          filter: { field: "company_name", value: "initech llc" },
        }),
      ),
    ).toEqual(["Initech LLC"]);
    expect(
      names(
        page(type("Invoice"), {
          filter: { field: "amount_due", value: "1200000" },
        }),
      ),
    ).toEqual(["in_1001", "in_1002"]);
    // A field picked with no value yet reads the whole type, uncounted.
    for (const value of ["", "  "]) {
      const v = view({ filter: { field: "currency", value } });
      expect(activeFilter(v)).toBe(null);
      const all = page(type("Customer"), { filter: v.filter });
      expect(all.rows).toHaveLength(RECORDS_LIMITS.page);
      expect(all.matched).toBe(null);
      expect(pageRead(SCOPE, type("Customer"), v)).toEqual(
        pageRead(SCOPE, type("Customer"), view()),
      );
    }
    // A filter and a search together.
    expect(
      names(
        page(type("Customer"), {
          search: "studios",
          filter: { field: "currency", value: "usd" },
        }),
      ),
    ).toEqual(
      CUSTOMERS.filter(
        ([, n, , c]) => n.includes("Studios") && c === "usd",
      ).map(([, n]) => n),
    );
  });

  it("stays bounded: one page plus one row, and capped counts", () => {
    const read = pageRead(
      SCOPE,
      type("Customer"),
      view({ search: "a", page: 2, sort: { column: "email", dir: "asc" } }),
    );
    expect(read.kind).toBe("read");
    const json = JSON.stringify(read.kind === "read" && read.request);
    expect(json).toContain(`"range":{"input":{"order_by":{"input":{"where":`);
    expect(json).toContain(`"start":{"literal":100},"end":{"literal":151}`);
    expect(json).toContain(`"count":{"literal":1001}`);
    expect(json).not.toContain('__nn","email');
    const deep = pageRead(
      SCOPE,
      type("Customer"),
      view({ page: MAX_PAGE + 5 }),
    );
    expect(deep.kind === "read" && deep.offset).toBe(RECORDS_LIMITS.maxOffset);
    // Only what the type declares, with its identity, is projected.
    expect(json).toContain(
      `"properties":["$id","$label","entity_key","norm_name","name","customer_id","company_name","currency","email"]`,
    );
  });

  it("reads an answer it can't trust as no rows", () => {
    expect(parsePage(null, naming, type("Customer"))).toEqual({
      rows: [],
      more: false,
      matched: null,
    });
    const odd = parsePage(
      {
        rows: [{ $id: "1" }, { $id: 2.5 }, { $id: 3, $label: "Customer" }],
        matched: 2000,
      },
      naming,
      type("Customer"),
    );
    expect(odd.rows).toEqual([
      { dbId: 3, entity: undefined, name: "#3", by: "id", props: {} },
    ]);
    expect(odd.matched).toEqual({ count: 1000, capped: true });
  });
});

describe("page spans", () => {
  it("says which rows show, of how many", () => {
    expect(pageSpan(0, 50, 64, false, true)).toMatchObject({
      text: "1–50 of 64",
      prev: false,
      next: true,
      deep: false,
    });
    expect(pageSpan(1, 14, 64, false)).toMatchObject({
      text: "51–64 of 64",
      prev: true,
      next: false,
    });
    expect(pageSpan(0, 50, 1000, true, true).text).toBe("1–50 of 1,000+");
    expect(pageSpan(0, 50, null, false, true).text).toBe("1–50");
    expect(pageSpan(0, 0, 0, false).text).toBe("");
    // A page past the last still says how many there are.
    expect(pageSpan(2, 0, 12, false)).toMatchObject({
      text: "0 of 12",
      prev: true,
      next: false,
    });
    expect(pageSpan(2, 0, null, false).text).toBe("");
    expect(pageSpan(MAX_PAGE, 50, 20000, false, true)).toMatchObject({
      next: false,
      deep: true,
    });
  });
});

describe("a record's properties", () => {
  it("lists the type's props in column order, then the rest, never the layout's", () => {
    expect(
      detailProps(naming, type("Customer"), "Customer", {
        email: "ap@globex.example",
        customer_id: "cus_GLOBEX01",
        company_name__nn: "globex corporation",
        entity_key: "cus_GLOBEX01",
        name: "Globex Corporation",
        tier: "gold",
        $id: 1,
      }),
    ).toEqual([
      ["customer_id", "cus_GLOBEX01"],
      ["company_name", undefined],
      ["currency", undefined],
      ["email", "ap@globex.example"],
      ["tier", "gold"],
    ]);
    // A label the version doesn't know keeps every property but the layout's names.
    expect(
      detailProps(naming, undefined, "Account", {
        name: "Ada",
        entity_key: "a1",
        email__nn: "x",
      }),
    ).toEqual([["entity_key", "a1"]]);
  });
});

describe("records of a type keyed by more than one prop", () => {
  it("names them by their key and sorts by every key prop", () => {
    const db = new MockGraph(),
      s = db.view(LOAD);
    for (const [order, line] of [
      ["ORD-2", 1],
      ["ORD-1", 2],
      ["ORD-1", 1],
    ] as const)
      s.addNode("OrderLine", {
        entity_key: `${order}/${line}`,
        order_number: order,
        line_no: line,
        qty: 1,
      });
    for (const code of [7, 12])
      s.addNode("SalesChannel", { entity_key: `${code}`, code });
    const read = (t: TypeNaming, v: Partial<RecordsView>) => {
      const r = pageRead(SCOPE, t, view({ type: t.name, ...v }));
      if (r.kind !== "read") throw new Error(r.kind);
      const answer = runQuery(db, r.request);
      expect(answer.status, JSON.stringify(answer.body)).toBe(200);
      return parsePage(answer.body, shop, t);
    };
    const lines = read(type("OrderLine", shop), {
      sort: { column: RECORD_COLUMN, dir: "asc" },
    });
    expect(names(lines)).toEqual(["ORD-1 · 1", "ORD-1 · 2", "ORD-2 · 1"]);
    expect(lines.rows[0]?.by).toBe("key");
    expect(names(read(type("SalesChannel", shop), { search: "12" }))).toEqual([
      "12",
    ]);
    expect(names(read(type("OrderLine", shop), { search: "ord-2" }))).toEqual([
      "ORD-2 · 1",
    ]);
    expect(
      detailProps(shop, type("OrderLine", shop), "OrderLine", {
        qty: 1,
        zone: "b",
        aisle: "a",
      }).map(([k]) => k),
    ).toEqual(["order_number", "line_no", "qty", "aisle", "zone"]);
  });
});
