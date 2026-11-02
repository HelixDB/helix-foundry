import { afterEach, describe, expect, it, vi } from "vitest";
import {
  FOUNDRY_EDGES as E,
  FOUNDRY_NODES as N,
} from "@/shared/foundry-labels";
import {
  parseReadResult,
  type Outcome,
  type Read,
} from "@/shared/foundry-read";
import type { OntologyVersion } from "@/shared/wfe";
import { entityId, type Fact } from "@/web/areas/explore/model";
import { namingOf, type TypeNaming } from "@/web/areas/explore/naming";
import {
  TYPED_LIMITS,
  countsPlan,
  entityPlan,
  factEvidencePlan,
  factOfKey,
  namesPlan,
  runPlan,
  typedColumns,
  typedFactKey,
  typedNarrows,
  typedOffset,
  typedRecordsPage,
  typedRecordsRead,
  type ReadPlan,
  type TypedRecords,
} from "@/web/areas/explore/typed-reads";
import type { RecordsView } from "@/web/areas/records/model";
import { recordQueries } from "@/web/areas/records/queries";
import { contractErrors } from "../../mock/agent-contract";
import {
  answer,
  compile,
  evaluate,
  readsOf,
  sweepViolations,
} from "../../mock/graph/rules";
import { MockGraph } from "../../mock/graph/store";

// The reads of a load through source grants: each page's typed reads,
// planned from the load's version, fit the engine's ReadItem schema, and
// their answers (here, the mock engine's, each held to read_result.json)
// parse into the shapes the database's reads give.

const ONTOLOGY = `version = "draft"
[types.Customer]
description = "A customer, keyed by its id."
props = { id = "string", company_name = "string", currency = "string" }
key = ["id"]
names = ["company_name"]
[types.Invoice]
description = "An invoice."
props = { id = "string", amount_due = "integer", status = "string" }
key = ["id"]
[types.Place]
description = "A place, known by its name."
[links.billed_to]
from = "Invoice"
to = "Customer"
description = "Who pays."
[links.based_in]
from = "Customer"
to = "Place"
description = "Where."
[constraints.paid_somewhere]
description = "Every customer is based somewhere."
severity = "warn"
datalog = "violation(c) :- Customer(c), not located(c). located(c) :- based_in(c, _)."
`;

const version = {
  id: "ver_1",
  format: "ONTOLOGY_FORMAT_AGENT_BUNDLE",
  definition: { ontology_toml: ONTOLOGY },
} as unknown as OntologyVersion;
const naming = namingOf(version);

function load() {
  const g = new MockGraph().view("load_1");
  const customer = (id: string, name: string, currency?: string) =>
    g.addNode("Customer", {
      entity_key: id,
      id,
      name,
      company_name: name,
      ...(currency ? { currency } : {}),
    });
  const globex = customer("cus_1", "Globex Corp", "USD"),
    initech = customer("cus_2", "Initech");
  const london = g.addNode("Place", { norm_name: "london", name: "London" });
  g.addEdge("based_in", globex, london, { fact_key: "fb" });
  for (const [i, amount] of [5400, 1200, 900].entries()) {
    const inv = g.addNode("Invoice", {
      entity_key: `in_${i}`,
      id: `in_${i}`,
      name: `in_${i}`,
      amount_due: amount,
      status: i ? "open" : "paid",
    });
    g.addEdge("billed_to", inv, i < 2 ? globex : initech, {
      fact_key: `f${i}`,
    });
  }
  // The row that states Globex and sets its props, and a quote of its currency.
  const row = g.addNode(N.Evidence, {
    source: "src_1",
    mapping: "customers",
    row_key: "cus_1",
    props: ["id", "company_name", "currency"],
  });
  g.addEdge(E.ABOUT, row, globex);
  const section = g.addNode(N.Section, { ordinal: 2 });
  const quote = g.addNode(N.Evidence, {
    doc_id: "doc_1",
    quote: "billed in GBP",
    prop: "currency",
    value: "GBP",
    start: 4,
    end: 17,
    confidence: 0.9,
  });
  g.addEdge(E.ABOUT, quote, globex);
  g.addEdge(E.FROM_SECTION, quote, section);
  // What states invoice 0's billing: a row.
  const billed = g.addNode(N.Evidence, {
    fact_key: "f0",
    source: "src_2",
    mapping: "invoices",
    row_key: "in_0",
  });
  void billed;
  const model = evaluate(g, compile(ONTOLOGY));
  sweepViolations(model, "ver_1");
  return model;
}

/**
 * Runs a plan against the mock engine as the WFE would: its reads held to
 * the request schema, each answer to the result schema, then the envelope
 * parsed as the SPA parses it.
 */
function run<T>(plan: ReadPlan<T>, model = load()): T {
  const reads = readsOf(JSON.stringify(plan.reads));
  if (typeof reads === "string") throw new Error(reads);
  const results = reads.map((r) => {
    const a = answer(model, r);
    expect(contractErrors("read_result#/$defs/ReadOutcome", a)).toEqual([]);
    return a;
  });
  const parsed = parseReadResult(
    JSON.stringify({ as_of: "2026-10-30", results, stats: {} }),
  )!;
  return plan.parse(parsed.results);
}

const globex = { type: "Customer", key: "cus_1" };

afterEach(() => vi.unstubAllGlobals());

describe("an entity through grants", () => {
  it("reads its props, facts, what states it and the rules it breaks", () => {
    const page = run(entityPlan(globex, naming));
    expect(page.entity).toMatchObject({
      id: entityId(globex),
      type: "Customer",
      name: "Globex Corp",
      identity: globex,
      props: { id: "cus_1", company_name: "Globex Corp", currency: "USD" },
    });
    // Its facts each way, the other end named by its names props or name.
    expect(
      page.facts.map((f) => [f.link, f.direction, f.other?.name]).sort(),
    ).toEqual([
      ["based_in", "out", "London"],
      ["billed_to", "in", "in_0"],
      ["billed_to", "in", "in_1"],
    ]);
    const billed = page.facts.find((f) => f.other?.name === "in_0")!;
    expect(factOfKey(billed.factKey!)).toEqual({
      link: "billed_to",
      subject: { type: "Invoice", id: "in_0" },
      object: { type: "Customer", id: "cus_1" },
    });
    // The row that states it sets the props whose evidence names it.
    expect(page.rows).toEqual([
      {
        source: "src_1",
        mapping: "customers",
        rowKey: "cus_1",
        props: ["id", "company_name", "currency"],
      },
    ]);
    expect(page.propEvidence).toEqual([
      {
        documentId: "doc_1",
        section: 2,
        start: 4,
        end: 17,
        prop: "currency",
        value: "GBP",
        quote: "billed in GBP",
        confidence: 0.9,
      },
    ]);
    expect(page.mentions).toEqual([]);
    expect(page.reviews).toEqual([]);
    expect(page.truncated).toEqual({
      facts: false,
      mentions: false,
      reviews: false,
      about: false,
    });
    // Initech is based nowhere: the swept record names it.
    const initech = run(entityPlan({ type: "Customer", key: "cus_2" }, naming));
    expect(initech.violations).toEqual([
      expect.objectContaining({
        constraint: "paid_somewhere",
        severity: "warn",
        entities: [expect.objectContaining({ name: "Initech" })],
      }),
    ]);
  });

  it("finds no entity the load doesn't hold, and fails when its read does", () => {
    expect(
      run(entityPlan({ type: "Customer", key: "cus_9" }, naming)).entity,
    ).toBeNull();
    const plan = entityPlan(globex, naming);
    expect(() =>
      plan.parse([
        { ok: false, error: { code: "budget", message: "m", limit: 64 } },
      ]),
    ).toThrow("Too deep to compute live");
  });

  it("trusts nothing in an answer: odd cells, failed parts", () => {
    const plan = entityPlan(globex, naming);
    const outcomes = run({
      reads: plan.reads,
      parse: (o) => o,
    }) as Outcome<unknown>[];
    const failing = {
      ok: false as const,
      error: { code: "invalid" as const, message: "m" },
    };
    // The entity's cell isn't one, or of a type the version doesn't know.
    for (const cell of [
      { type: "Customer" },
      { type: "Ghost", id: "x", name: "x" },
      "x",
    ])
      expect(
        plan.parse([
          {
            ok: true,
            value: {
              kind: "query",
              result: {
                kind: "rows",
                rows: [{ cells: [cell], edges: [] }],
                total: 1,
              },
            },
          },
          ...outcomes.slice(1),
        ]).entity,
      ).toBeNull();
    // What states it and the rules it breaks couldn't be read: none, said
    // as cut short.
    const evidenceAt = plan.reads.findIndex((r) => "evidence" in r),
      breaksAt = plan.reads.findIndex((r) => "violations" in r);
    const cut = plan.parse(
      outcomes.map((o, i) =>
        i === evidenceAt || i === breaksAt || i > evidenceAt ? failing : o,
      ),
    );
    expect(cut.rows).toEqual([]);
    expect(cut.propEvidence).toEqual([]);
    expect(cut.violations).toEqual([]);
    expect(cut.truncated.about).toBe(true);
    // Without the rows that state it, the rows that set its props still do.
    const fromProps = plan.parse(
      outcomes.map((o, i) => (i === evidenceAt ? failing : o)),
    );
    expect(fromProps.rows).toEqual([
      {
        source: "src_1",
        mapping: "customers",
        rowKey: "cus_1",
        props: ["id", "company_name", "currency"],
      },
    ]);
    // A fact whose edge isn't a stated fact (one rules give) isn't listed.
    const linkAt = plan.reads.findIndex((r) => "query" in r && r.query.steps);
    const derivedOnly = plan.parse(
      outcomes.map((o, i) =>
        i === linkAt
          ? {
              ok: true,
              value: {
                kind: "query",
                result: {
                  kind: "rows",
                  rows: [
                    {
                      cells: [
                        { type: "Customer", id: "cus_1", name: "G" },
                        { type: "Place", id: "paris", name: "Paris" },
                      ],
                      edges: [
                        {
                          step: 0,
                          derived: {
                            pred: "link:based_in",
                            args: [
                              { type: "Customer", id: "cus_1" },
                              { type: "Place", id: "paris" },
                            ],
                          },
                        },
                      ],
                    },
                  ],
                  total: 1,
                },
              },
            }
          : o,
      ),
    );
    expect(derivedOnly.facts.map((f) => f.other?.name)).not.toContain("Paris");
  });

  it("reads a type known by its name, and marks what it couldn't read", () => {
    const plan = entityPlan({ type: "Place", name: "london" }, naming);
    // No props of its own: one read of the entity.
    expect(plan.reads[0]).toEqual({
      query: {
        start: { type: "Place", ids: ["london"] },
        return: { rows: { columns: [{ at: 0 }], limit: 1 } },
      },
    });
    const page = run(plan);
    expect(page.entity?.name).toBe("London");
    expect(page.facts.map((f) => f.other?.name)).toEqual(["Globex Corp"]);
    // A link read that fails leaves its facts out, said as cut short.
    const outcomes = run({
      reads: plan.reads,
      parse: (o) => o,
    }) as Outcome<unknown>[];
    const failed = plan.parse(
      outcomes.map((o, i) =>
        i === 1
          ? { ok: false, error: { code: "hub", message: "m", limit: 1 } }
          : o,
      ),
    );
    expect(failed.facts).toEqual([]);
    expect(failed.truncated.facts).toBe(true);
  });

  it("reads wide types in groups of columns, and at most so many links", () => {
    const wide = namingOf({
      ...version,
      definition: {
        ontology_toml: `version = "x"
[types.T]
description = "t"
key = ["k"]
props = { ${Array.from({ length: 35 }, (_, i) => `p${i} = "string"`).join(", ")}, k = "string" }
${Array.from(
  { length: 35 },
  (_, i) => `[links.l${i}]\nfrom = "T"\nto = "T"\ndescription = "l"\n`,
).join("")}`,
      },
    } as unknown as OntologyVersion);
    const plan = entityPlan({ type: "T", key: "a" }, wide);
    const props = plan.reads.filter(
      (r) => "query" in r && !r.query.steps,
    ) as Extract<Read, { query: unknown }>[];
    expect(
      props.map(
        (r) => "rows" in r.query.return && r.query.return.rows.columns.length,
      ),
    ).toEqual([20, 18]);
    const links = plan.reads.filter((r) => "query" in r && r.query.steps);
    expect(links).toHaveLength(TYPED_LIMITS.links);
    const quoted = plan.reads.filter(
      (r) => "evidence" in r && "entity" in r.evidence && r.evidence.prop,
    );
    expect(quoted).toHaveLength(TYPED_LIMITS.propEvidence);
    // Every read fits the engine's schema.
    expect(typeof readsOf(JSON.stringify(plan.reads.slice(0, 20)))).not.toBe(
      "string",
    );
  });
});

describe("a fact's evidence through grants", () => {
  it("reads what states each fact, keyed as the page keys them", () => {
    const page = run(entityPlan(globex, naming));
    const evidence = run(factEvidencePlan(page.facts));
    const key = page.facts.find((f) => f.other?.name === "in_0")!.factKey!;
    expect(evidence.get(key)).toEqual([
      {
        kind: "row",
        source: "src_2",
        mapping: "invoices",
        rowKey: "in_0",
        confidence: 1,
      },
    ]);
    expect(evidence.size).toBe(3);
  });

  it("reads at most so many facts, and skips keys it didn't make", () => {
    const facts: Fact[] = Array.from({ length: 30 }, (_, i) => ({
      link: "billed_to",
      direction: "in",
      factKey: typedFactKey({
        link: "billed_to",
        subject: { type: "Invoice", id: `in_${i}` },
        object: { type: "Customer", id: "cus_1" },
      }),
      props: {},
    }));
    facts.push({ link: "x", direction: "out", factKey: "fk_raw", props: {} });
    const plan = factEvidencePlan(facts);
    expect(plan.reads).toHaveLength(TYPED_LIMITS.evidenceFacts);
    // A failed read gives that fact no evidence.
    const parsed = plan.parse([
      { ok: false, error: { code: "unknown_entity", message: "m" } },
    ]);
    expect(parsed.get(facts[0]!.factKey!)).toEqual([]);
    expect(factOfKey("fk_raw")).toBeUndefined();
    expect(factOfKey('["a"]')).toBeUndefined();
  });
});

describe("names through grants", () => {
  it("names entities as their pages do, a read per type", () => {
    const plan = namesPlan(
      [
        { type: "Customer", id: "cus_1" },
        { type: "Customer", id: "cus_2" },
        { type: "Place", id: "london" },
        { type: "Ghost", id: "boo" },
      ],
      naming,
    )!;
    expect(plan.reads).toHaveLength(2);
    expect(run(plan)).toEqual(
      new Map([
        ["Customer\u0000cus_1", "Globex Corp"],
        ["Customer\u0000cus_2", "Initech"],
        ["Place\u0000london", "London"],
      ]),
    );
    expect(namesPlan([{ type: "Ghost", id: "boo" }], naming)).toBeNull();
  });
});

describe("Records through grants", () => {
  const view = (v: Partial<RecordsView> = {}): RecordsView => ({
    type: "Invoice",
    search: "",
    filter: null,
    sort: null,
    page: 0,
    record: "",
    ...v,
  });
  const invoice = naming.types.get("Invoice")!,
    customer = naming.types.get("Customer")!,
    place = naming.types.get("Place")!;
  const read = (t = invoice, v = view()) => {
    const r = typedRecordsRead(t, v, naming);
    if (r.kind !== "read") throw new Error(r.kind);
    return r.plan;
  };

  it("counts each type", () => {
    expect(run(countsPlan(["Customer", "Invoice", "Place"]))).toEqual(
      new Map([
        ["Customer", 2],
        ["Invoice", 3],
        ["Place", 1],
      ]),
    );
    expect(
      countsPlan(["Customer"]).parse([
        { ok: false, error: { code: "invalid", message: "m" } },
      ]),
    ).toEqual(new Map([["Customer", null]]));
  });

  it("reads a type's rows in the view's order, with its columns", () => {
    expect(typedColumns(invoice)).toEqual(["id", "amount_due", "status"]);
    const page = run(
      read(invoice, view({ sort: { column: "amount_due", dir: "desc" } })),
    );
    expect(page.rows.map((r) => [r.name, r.props.amount_due])).toEqual([
      ["in_0", 5400],
      ["in_1", 1200],
      ["in_2", 900],
    ]);
    expect(page.rows[0]).toMatchObject({
      by: "key",
      entity: { identity: { type: "Invoice", key: "in_0" } },
    });
    expect(page.matched).toBeNull();
    expect(page.more).toBe(false);
    // The record column sorts by the first names prop, or the key.
    expect(
      run(
        read(customer, view({ sort: { column: "$record", dir: "asc" } })),
      ).rows.map((r) => r.name),
    ).toEqual(["Globex Corp", "Initech"]);
  });

  it("searches names by text, else the key; filters by a prop's value", () => {
    expect(
      run(read(customer, view({ search: "initech" }))).rows.map((r) => r.name),
    ).toEqual(["Initech"]);
    expect(run(read(place, view({ search: "London" }))).matched).toEqual({
      count: 1,
      capped: false,
    });
    // A keyed type without names: its key contains the text.
    expect(read(invoice, view({ search: "in_1" })).reads[0]).toMatchObject({
      query: {
        start: { where: [{ prop: "id", op: "contains", value: "in_1" }] },
      },
    });
    expect(
      run(read(invoice, view({ search: "in_1" }))).rows.map((r) => r.name),
    ).toEqual(["in_1"]);
    expect(
      run(
        read(invoice, view({ filter: { field: "amount_due", value: "900" } })),
      ).rows.map((r) => r.name),
    ).toEqual(["in_2"]);
    // A names prop compares its value as typed.
    expect(
      read(
        customer,
        view({ filter: { field: "company_name", value: " Initech " } }),
      ).reads[0],
    ).toMatchObject({
      query: {
        start: {
          where: [{ prop: "company_name", op: "=", value: "Initech" }],
        },
      },
    });
    // What can't match reads nothing; a filter that isn't one is refused.
    expect(typedRecordsRead(customer, view({ search: "!!" }), naming)).toEqual({
      kind: "none",
    });
    expect(
      typedRecordsRead(
        invoice,
        view({ filter: { field: "amount_due", value: "lots" } }),
        naming,
      ),
    ).toEqual({ kind: "refused", message: "Enter a whole number." });
  });

  it("tests a number key by equality", () => {
    const keyed = {
      name: "Seat",
      props: [{ name: "no", type: "integer" }],
      key: ["no"],
      names: [],
    };
    const start = (search: string) => {
      const r = typedRecordsRead(keyed, view({ search }), naming);
      return r.kind === "read" && "query" in r.plan.reads[0]!
        ? r.plan.reads[0].query.start
        : r.kind;
    };
    expect(start("12")).toMatchObject({
      where: [{ prop: "no", op: "=", value: 12 }],
    });
    expect(start("twelve")).toBe("none");
    const float = { ...keyed, props: [{ name: "no", type: "float" }] };
    const r = typedRecordsRead(float, view({ search: "1.5" }), naming);
    expect(r.kind === "read" && r.plan.reads[0]).toMatchObject({
      query: { start: { where: [{ value: 1.5 }] } },
    });
    expect(typedRecordsRead(float, view({ search: "x" }), naming)).toEqual({
      kind: "none",
    });
  });

  const answered = (rows: unknown[], total: number) => [
    {
      ok: true as const,
      value: { kind: "query", result: { kind: "rows", rows, total } },
    },
  ];
  const invoiceRows = (n: number, from = 0) =>
    Array.from({ length: n }, (_, i) => ({
      cells: [
        { type: "Invoice", id: `in_${from + i}`, name: `in_${from + i}` },
        `in_${from + i}`,
        i,
        null,
      ],
      edges: [],
    }));
  const rowsOfRead = (r: Read) =>
    (r as { query: { return: { rows: { limit: number; offset?: number } } } })
      .query.return.rows;

  it("cuts a read's 200 rows into pages, and reads on past them", () => {
    const page = read().parse(answered(invoiceRows(200), 640));
    expect(page).toMatchObject({ offset: 0, total: 640, more: true });
    // An absent value is left out of the row's props.
    expect(page.rows[0]!.props).toEqual({ id: "in_0", amount_due: 0 });
    const second = typedRecordsPage(page, 1);
    expect(second.rows.map((r) => r.name)[0]).toBe("in_50");
    expect(second.more).toBe(true);
    // The read's last page: the next read's rows follow.
    expect(typedRecordsPage(page, 3).more).toBe(true);
    // Page 4 is the next read's first.
    expect(typedOffset(4, false)).toBe(200);
    expect(typedOffset(3, false)).toBe(0);
    expect(typedOffset(4, true)).toBe(0);
    const next = read(invoice, view({ page: 4 }));
    expect(rowsOfRead(next.reads[0]!)).toMatchObject({
      limit: 200,
      offset: 200,
    });
    const after = next.parse(answered(invoiceRows(14, 200), 214));
    expect(after).toMatchObject({ offset: 200, total: 214, more: false });
    const last = typedRecordsPage(after, 4);
    expect(last.rows.map((r) => r.name)[0]).toBe("in_200");
    expect(last.more).toBe(false);
    // The first read has no offset to send.
    expect(rowsOfRead(read().reads[0]!).offset).toBeUndefined();
    // A read of another page's, while this one's loads, shows none of it.
    expect(typedRecordsPage(after, 1).rows).toEqual([]);
  });

  it("tells a narrowed read from one of what is held whole", () => {
    expect(typedNarrows(invoice, ["Customer"])).toBe(false);
    expect(typedNarrows(invoice, ["Invoice"])).toBe(true);
    expect(typedNarrows(invoice, ["Invoice.status"])).toBe(true);
    expect(typedNarrows(invoice, ["Invoice.no_such_column"])).toBe(false);
    expect(typedNarrows(invoice, ["billed_to"])).toBe(false);
    // Its name reads by its type, not its stored value.
    expect(
      typedNarrows(
        {
          name: "Thing",
          key: [],
          names: ["name"],
          props: [{ name: "name", type: "string" }],
        },
        ["Thing.name"],
      ),
    ).toBe(false);
  });

  it("reads what is narrowed once, with nothing said of the rest", () => {
    const plan = (v = view()) => {
      const r = typedRecordsRead(invoice, v, naming, true);
      if (r.kind !== "read") throw new Error(r.kind);
      return r.plan;
    };
    // The rows asked for and no window, whatever the page.
    expect(rowsOfRead(plan().reads[0]!)).toEqual(
      expect.objectContaining({ limit: TYPED_LIMITS.rows }),
    );
    expect(
      rowsOfRead(plan(view({ page: 2 })).reads[0]!).offset,
    ).toBeUndefined();
    // No total, no more, and all its rows are its one page.
    const narrowed = plan().parse(answered(invoiceRows(120), 120));
    expect(narrowed).toMatchObject({ total: null, more: false, matched: null });
    expect(typedRecordsPage(narrowed, 0).rows).toHaveLength(120);
    expect(typedRecordsPage(narrowed, 3).rows).toHaveLength(120);
    expect(typedRecordsPage(narrowed, 0).more).toBe(false);
    // A search of it can't count its matches.
    expect(
      plan(view({ search: "in_1" })).parse(answered(invoiceRows(1), 1)).matched,
    ).toBeNull();
    // Read as whole, what the engine finds narrowed refuses a later window,
    // which the page then reads as narrowed.
    const window = read(invoice, view({ page: 4 })).parse([
      {
        ok: false as const,
        error: {
          code: "not_visible" as const,
          message: "m",
          pointer: "/return/rows/offset",
        },
      },
    ]);
    expect(window).toMatchObject({ refusedBy: "paging", total: null });
  });

  it("says what it can't filter, search or sort by", () => {
    const refused = (pointer: string) => [
      {
        ok: false as const,
        error: { code: "not_visible" as const, message: "m", pointer },
      },
    ];
    const sorted = read(
      invoice,
      view({ sort: { column: "amount_due", dir: "asc" } }),
    ).parse(refused("/return/rows/order_by/prop"));
    expect(sorted).toMatchObject({ refusedBy: "sort", rows: [], more: false });
    expect(sorted.refused).toContain("sort by");
    expect(sorted.refused).toContain("sources shared with you");
    const filtered = read(
      invoice,
      view({ filter: { field: "status", value: "paid" } }),
    ).parse(refused("/start/where/0/prop"));
    expect(filtered.refusedBy).toBe("filter");
    expect(filtered.refused).toContain("filter by");
    const searched = read(invoice, view({ search: "in_1" })).parse(
      refused("/start/where/0/prop"),
    );
    expect(searched.refusedBy).toBe("search");
    expect(searched.refused).toContain("search Invoice records");
    // With both, the filter is the last of its tests.
    const both = read(
      invoice,
      view({ search: "in_1", filter: { field: "status", value: "paid" } }),
    );
    expect(both.parse(refused("/start/where/1/prop")).refusedBy).toBe("filter");
    expect(both.parse(refused("/start/where/0/prop")).refusedBy).toBe("search");
  });

  it("keeps a read's other failures as failures", () => {
    expect(() =>
      read().parse([
        {
          ok: false,
          error: { code: "too_broad", message: "m", limit: 1, count: 2 },
        },
      ]),
    ).toThrow("Covers too many entities");
    expect(() =>
      read().parse([
        {
          ok: true,
          value: { kind: "query", result: { kind: "count", count: 1 } },
        },
      ]),
    ).toThrow("malformed");
  });
});

describe("a type's records, read through grants", () => {
  const invoice: TypeNaming = {
    name: "Invoice",
    props: [{ name: "status", type: "string" }],
    key: [],
    names: [],
  };
  const records = { rows: [], total: 0 };
  /** Each request's first read, answered in turn. */
  function answers(...results: unknown[]) {
    const sent: { limit?: number; offset?: number; sorted: boolean }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        if (url === "/papi/csrf")
          return Response.json({ token: "t", expiresInSeconds: 600 });
        const [read] = JSON.parse(JSON.parse(String(init?.body)).readsJson);
        const { limit, offset, order_by } = read.query.return.rows;
        sent.push({ limit, offset, sorted: !!order_by });
        return Response.json({
          resultJson: JSON.stringify({
            as_of: "2026-11-02",
            results: [results.shift()],
            stats: { instances: 0, wall_ms: 1 },
          }),
        });
      }),
    );
    return sent;
  }
  const refused = (pointer: string) => ({
    error: { code: "not_visible", message: "m", pointer },
  });
  const rowsAnswer = {
    ok: { kind: "query", result: { kind: "rows", ...records } },
  };
  const page = (view: RecordsView, narrowed = false) =>
    recordQueries
      .page(
        "ws",
        "o",
        { kind: "grants" },
        { id: "l" },
        namingOf(undefined),
        invoice,
        view,
        narrowed,
      )
      .queryFn({});
  const base: RecordsView = {
    type: "Invoice",
    search: "",
    filter: null,
    sort: null,
    page: 0,
    record: "",
  };

  it("reads a later window as narrowed once the engine says it is", async () => {
    const sent = answers(refused("/return/rows/offset"), rowsAnswer);
    const read = (await page({ ...base, page: 8 })) as TypedRecords;
    expect(sent).toEqual([
      { limit: 200, offset: 400, sorted: false },
      { limit: 200, sorted: false },
    ]);
    expect(read).toMatchObject({ total: null, more: false });
    expect(read.notice).toBeUndefined();
  });

  it("reads again unsorted, and says so", async () => {
    const sent = answers(refused("/return/rows/order_by/prop"), rowsAnswer);
    const read = (await page(
      { ...base, sort: { column: "status", dir: "asc" } },
      true,
    )) as TypedRecords;
    expect(sent).toEqual([
      { limit: 200, sorted: true },
      { limit: 200, sorted: false },
    ]);
    expect(read).toMatchObject({ total: null });
    expect(read.notice).toContain("sort by");
  });

  it("keeps what else it refuses as said", async () => {
    answers(refused("/start/where/0/prop"));
    const read = (await page({ ...base, search: "in_1" })) as TypedRecords;
    expect(read.refusedBy).toBe("search");
  });
});

describe("runPlan", () => {
  it("sends a plan's reads 20 to a request, in order", async () => {
    const sent: number[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        if (url === "/papi/csrf")
          return Response.json({ token: "t", expiresInSeconds: 600 });
        const reads = JSON.parse(JSON.parse(String(init?.body)).readsJson);
        sent.push(reads.length);
        return Response.json({
          resultJson: JSON.stringify({
            as_of: "2026-10-30",
            results: reads.map(() => ({
              ok: { kind: "query", result: { kind: "count", count: 1 } },
            })),
            stats: { instances: 0, wall_ms: 1 },
          }),
        });
      }),
    );
    const types = Array.from({ length: 45 }, (_, i) => `T${i}`);
    const counts = await runPlan("ws", "o", "load_1", countsPlan(types));
    expect(sent).toEqual([20, 20, 5]);
    expect(counts.size).toBe(45);
    expect(counts.get("T44")).toBe(1);
  });
});
