import { describe, expect, it } from "vitest";
import type {
  AskResult,
  AskUsage,
  OntologyQuery,
  QueryOutput,
} from "@/shared/wfe";
import {
  parseAskResult,
  parseOutput,
  parseQuery,
  parseVisuals,
} from "@/web/areas/ontology/ask-model";
import {
  cellOf,
  cellText,
  chartHeight,
  drawable,
  placeVisuals,
  visualPlan,
  type VisualPlan,
} from "@/web/areas/ontology/ask-visuals";
import { contractErrors } from "../../mock/agent-contract";

// Ask's visuals (contract 3.11.0): a result's recorded query outputs and an
// answer's visuals parsed without trust, and how each output is drawn, by
// its shape and the visual's hint. Every well-formed fixture is the engine's
// JSON: it fits ask_result.json.

const USAGE: AskUsage = {
  input: 0,
  cache_read: 0,
  cache_write: 0,
  output: 0,
  usd: 0,
};

/** An output fixture, held to ask_result.json's RecordedOutput. */
const output = <T extends QueryOutput>(o: T): T => {
  expect(contractErrors("ask_result#/$defs/RecordedOutput", o)).toEqual([]);
  return o;
};

const BY_STATUS: OntologyQuery = {
  start: { type: "Invoice", where: [], ids: null, text: null },
  steps: [],
  filters: [],
  return: { group_count: { at: 0, prop: "status", limit: 50 } },
};
const SUM_BY_CUSTOMER: OntologyQuery = {
  start: { type: "Invoice", where: [], ids: null, text: null },
  steps: [{ via: "billed_to", dir: "out", to: "Customer", where: [] }],
  filters: [],
  return: {
    aggregate: {
      fn: "sum",
      at: 0,
      prop: "amount_due",
      group_by: { at: 1, prop: "name" },
      limit: 50,
    },
  },
};
const LARGEST: OntologyQuery = {
  start: { type: "Invoice", where: [], ids: null, text: null },
  steps: [{ via: "billed_to", dir: "out", to: "Customer", where: [] }],
  filters: [],
  return: {
    rows: {
      columns: [
        { at: 0, prop: null },
        { at: 0, prop: "amount_due" },
        { at: 1, prop: null },
        { at: 0, prop: "terms" },
      ],
      order_by: { at: 0, prop: "amount_due", desc: true },
      limit: 3,
    },
  },
};
const COUNT: OntologyQuery = {
  start: { type: "Invoice", where: [], ids: null, text: null },
  steps: [],
  filters: [],
  return: { count: { distinct_at: null } },
};
const AVERAGE: OntologyQuery = {
  ...COUNT,
  return: {
    aggregate: { fn: "avg", at: 0, prop: "amount_due", group_by: null },
  },
};

const invoice = (id: string, name = id) => ({ type: "Invoice", id, name });
const globex = { type: "Customer", id: "cus_GLOBEX01", name: "Globex" };
// The engine gives every group's value but null as {"untrusted": text}.
const STATUSES = output({
  kind: "group_count",
  groups: [
    { value: { untrusted: "paid" }, count: 40 },
    { value: { untrusted: "open" }, count: 20 },
    { value: null, count: 5 },
  ],
  total_groups: 3,
});
const ROWS = output({
  kind: "rows",
  rows: [
    {
      cells: [invoice("in_1"), 1_200_000, globex, { untrusted: "Net 30" }],
      edges: [
        {
          step: 0,
          fact: {
            link: "billed_to",
            subject: { type: "Invoice", id: "in_1" },
            object: { type: "Customer", id: "cus_GLOBEX01" },
          },
        },
      ],
    },
    { cells: [invoice("in_2"), 900_000, globex, null], edges: [] },
    { cells: [invoice("in_3"), null, globex, "Net 60"], edges: [] },
  ],
  total: 65,
  cut: "2 rows left out",
});

/** A plan's chart, or a failure naming what it was instead. */
function chartOf(plan: VisualPlan) {
  if (plan.kind !== "chart") throw new Error(`a ${plan.kind}, not a chart`);
  return plan.chart;
}

describe("recorded outputs", () => {
  it("parse every shape the engine records", () => {
    const shapes: QueryOutput[] = [
      STATUSES,
      ROWS,
      output({ kind: "count", count: 12 }),
      output({ kind: "aggregate", value: 1250.5, values: 4 }),
      output({ kind: "aggregate", value: null, values: 0 }),
      output({
        kind: "grouped_aggregate",
        groups: [
          { value: { untrusted: "Globex" }, aggregate: 1_200_000, values: 2 },
          { value: null, aggregate: null, values: 0 },
        ],
        total_groups: 9,
        cut: null,
      }),
    ];
    for (const o of shapes) {
      const { cut, ...rest } = o;
      expect(parseOutput(o), o.kind).toEqual(cut ? o : rest);
    }
  });

  it("refuses what isn't an output", () => {
    for (const bad of [
      null,
      "rows",
      [],
      { kind: "table", rows: [] },
      { kind: "rows", rows: [] },
      { kind: "rows", rows: [{ edges: [] }], total: 1 },
      { kind: "rows", rows: "<table>", total: 1 },
      { kind: "count", count: -1 },
      { kind: "count", count: 1.5 },
      { kind: "aggregate", value: "12", values: 1 },
      { kind: "aggregate", value: 1 },
      { kind: "group_count", groups: [{ count: 1 }], total_groups: 1 },
      { kind: "group_count", groups: [{ value: "a", count: 1 }] },
      { kind: "group_count", groups: [4], total_groups: 1 },
      {
        kind: "grouped_aggregate",
        groups: [{ value: "a", aggregate: 1 }],
        total_groups: 1,
      },
    ])
      expect(parseOutput(bad), JSON.stringify(bad)).toBe(undefined);
    // A row without edges has none; a cut that isn't text is no cut.
    expect(
      parseOutput({ kind: "rows", rows: [{ cells: [1] }], total: 1, cut: 4 }),
    ).toEqual({ kind: "rows", rows: [{ cells: [1], edges: [] }], total: 1 });
  });
});

describe("an answer's visuals", () => {
  const ids = new Set(["e1", "e2", "e3", "e4", "e5", "e6"]);

  it("keep what can be drawn: a query result, once, after a block the answer has", () => {
    expect(
      parseVisuals(
        [
          { evidence_id: "e1", after: 0, kind: "bar", title: "By status" },
          { evidence_id: "e2", after: null, kind: null, title: null },
          { evidence_id: "e1", after: 1 },
          { evidence_id: "e9" },
          { evidence_id: "e3", after: 2 },
          { evidence_id: "e3", after: -1 },
          { evidence_id: "e3", after: 0.5 },
          { evidence_id: "e3", after: "0" },
          { after: 0 },
          "e4",
          { evidence_id: "e4", kind: "pie" },
        ],
        ids,
        2,
      ),
    ).toEqual([
      { evidence_id: "e1", after: 0, kind: "bar", title: "By status" },
      { evidence_id: "e2" },
      // A kind the page doesn't know is no hint: the shape decides.
      { evidence_id: "e4" },
    ]);
    expect(parseVisuals(undefined, ids, 2)).toEqual([]);
    expect(parseVisuals({ evidence_id: "e1" }, ids, 2)).toEqual([]);
  });

  it("keep at most four, and only plain-text titles of 80 characters", () => {
    const many = ["e1", "e2", "e3", "e4", "e5", "e6"].map((e) => ({
      evidence_id: e,
    }));
    expect(parseVisuals(many, ids, 1).map((v) => v.evidence_id)).toEqual([
      "e1",
      "e2",
      "e3",
      "e4",
    ]);
    const [marked, long, blank, notText] = parseVisuals(
      [
        {
          evidence_id: "e1",
          title:
            '<img src=x onerror="alert(1)">**Top** [customers](https://e.com)',
        },
        { evidence_id: "e2", title: "é".repeat(90) },
        { evidence_id: "e3", title: "  <b></b> " },
        { evidence_id: "e4", title: 4 },
      ],
      ids,
      1,
    );
    expect(marked!.title).toBe("Top customers");
    expect([...long!.title!]).toHaveLength(80);
    expect(long!.title!.endsWith("é…")).toBe(true);
    expect(blank).toEqual({ evidence_id: "e3" });
    expect(notText).toEqual({ evidence_id: "e4" });
  });

  it("read with their outputs from a result, and an earlier contract's result reads without", () => {
    const answered = {
      status: "answered",
      answer: {
        blocks: [
          { para: "Most are paid [c1]." },
          { list: ["Globex owes most [c2]"] },
        ],
        visuals: [
          { evidence_id: "e2", after: 0, title: "Invoices by status" },
          { evidence_id: "e3", kind: "bar" },
        ],
      },
      citations: [
        {
          marker: "c1",
          evidence_id: "e2",
          target: { result: { evidence_id: "e2" } },
        },
        {
          marker: "c2",
          evidence_id: "e3",
          target: { entity: { type: "Customer", id: "cus_GLOBEX01" } },
        },
      ],
      queries: [
        { evidence_id: "e2", query: BY_STATUS, output: STATUSES },
        { evidence_id: "e3", query: LARGEST, output: ROWS },
      ],
      usage: USAGE,
    };
    expect(contractErrors("ask_result", answered)).toEqual([]);
    const parsed = parseAskResult(JSON.stringify(answered))!;
    expect(parsed).toEqual({
      ...answered,
      queries: answered.queries.map((q) => ({
        ...q,
        query: parseQuery(q.query),
      })),
    });
    if (parsed.status !== "answered") throw new Error(parsed.status);
    expect(drawable(parsed).map((d) => d.output.kind)).toEqual([
      "group_count",
      "rows",
    ]);

    // A list leaves outputs out: the visuals are kept, none drawable yet.
    const listed = parseAskResult(
      JSON.stringify({
        ...answered,
        queries: answered.queries.map(({ output: _, ...q }) => q),
      }),
    )!;
    if (listed.status !== "answered") throw new Error(listed.status);
    expect(listed.answer.visuals).toHaveLength(2);
    expect(drawable(listed)).toEqual([]);

    // A 3.10.0 result: no outputs, no visuals, and it still fits.
    const before = {
      ...answered,
      answer: { blocks: answered.answer.blocks },
      queries: [{ evidence_id: "e2", query: BY_STATUS }],
    };
    expect(contractErrors("ask_result", before)).toEqual([]);
    const old = parseAskResult(JSON.stringify(before))!;
    expect(old).toMatchObject({ answer: { blocks: before.answer.blocks } });
    if (old.status !== "answered") throw new Error(old.status);
    expect(old.answer.visuals).toBe(undefined);
    expect(old.queries[0]!.output).toBe(undefined);

    // A visual follows the blocks kept: one after a block that doesn't
    // read follows the block before it; an output that doesn't read is
    // none.
    const odd = parseAskResult(
      JSON.stringify({
        ...answered,
        answer: {
          blocks: [{ img: "x" }, { para: "A" }, 4, { para: "B" }],
          visuals: [
            { evidence_id: "e2", after: 0 },
            { evidence_id: "e3", after: 2 },
          ],
        },
        queries: [
          { evidence_id: "e2", query: BY_STATUS, output: { kind: "pie" } },
          { evidence_id: "e3", query: LARGEST, output: ROWS },
        ],
      }),
    )!;
    if (odd.status !== "answered") throw new Error(odd.status);
    expect(odd.answer.visuals).toEqual([
      { evidence_id: "e2", after: 0 },
      { evidence_id: "e3", after: 0 },
    ]);
    expect(odd.queries[0]!.output).toBe(undefined);
    expect(drawable(odd).map((d) => d.visual.evidence_id)).toEqual(["e3"]);
  });

  it("are placed after their blocks, the rest after the last", () => {
    const v = (evidence_id: string, after?: number) => ({
      visual: { evidence_id, ...(after !== undefined ? { after } : {}) },
    });
    const ids = (placed: { visual: { evidence_id: string } }[][]) =>
      placed.map((p) => p.map((d) => d.visual.evidence_id));
    expect(
      ids(placeVisuals([v("e1", 2), v("e2"), v("e3", 0), v("e4", 9)], 3)),
    ).toEqual([["e3"], [], ["e1", "e2", "e4"]]);
    expect(ids(placeVisuals([v("e1", 0), v("e2")], 0))).toEqual([["e1", "e2"]]);
  });
});

describe("cells", () => {
  it("are an entity only in an entity column, and every other value is text", () => {
    expect(cellOf(globex, true)).toEqual({
      kind: "entity",
      ref: { type: "Customer", id: "cus_GLOBEX01" },
      name: "Globex",
    });
    expect(cellOf(globex)).toEqual({
      kind: "text",
      text: JSON.stringify(globex),
      untrusted: false,
    });
    expect(cellOf({ untrusted: "<script>alert(1)</script>" })).toEqual({
      kind: "text",
      text: "<script>alert(1)</script>",
      untrusted: true,
    });
    expect(cellOf({ untrusted: 30 })).toEqual({
      kind: "text",
      text: "30",
      untrusted: true,
    });
    expect(cellOf({ untrusted: "x", more: 1 })).toMatchObject({
      untrusted: false,
    });
    expect(cellOf(true)).toEqual({
      kind: "text",
      text: "true",
      untrusted: false,
    });
    expect(cellOf([1, "a"])).toEqual({
      kind: "text",
      text: '[1,"a"]',
      untrusted: false,
    });
    expect(cellOf(Number.NaN)).toMatchObject({ kind: "text", text: "NaN" });
    expect(cellOf(undefined)).toEqual({ kind: "none" });
    expect(cellText(cellOf(1_200_000.5))).toBe("1,200,000.5");
    expect(cellText(cellOf(null))).toBe("—");
    expect(cellText(cellOf(globex, true))).toBe("Globex");
  });
});

describe("visual plans", () => {
  it("draw a count or an aggregate as a figure, or as a table when asked", () => {
    const count = output({ kind: "count", count: 12 });
    expect(visualPlan(count, COUNT)).toEqual({
      kind: "figure",
      title: "Count of distinct Invoice",
      note: "",
      value: { kind: "number", value: 12 },
      detail: "",
    });
    expect(
      visualPlan(count, COUNT, { kind: "figure", title: "Invoices" }),
    ).toMatchObject({ kind: "figure", title: "Invoices" });
    const avg = output({ kind: "aggregate", value: 1250.5, values: 4 });
    expect(visualPlan(avg, AVERAGE)).toMatchObject({
      kind: "figure",
      title: "Average of Invoice.amount_due",
      value: { kind: "number", value: 1250.5 },
      detail: "Over 4 values",
    });
    expect(
      visualPlan(
        output({ kind: "aggregate", value: null, values: 0 }),
        AVERAGE,
      ),
    ).toMatchObject({ value: { kind: "none" }, detail: "Over 0 values" });
    // A bar or a line doesn't suit one number: a table.
    for (const kind of ["table", "bar", "line"] as const)
      expect(visualPlan(count, COUNT, { kind }).kind, kind).toBe("table");
    expect(visualPlan(avg, AVERAGE, { kind: "table" })).toMatchObject({
      table: {
        columns: [
          { label: "Average of Invoice.amount_due", numeric: true },
          { label: "Values", numeric: true },
        ],
        rows: [
          [
            { kind: "number", value: 1250.5 },
            { kind: "number", value: 4 },
          ],
        ],
      },
    });
  });

  it("draw counts by value as upright bars, most first, those without one named", () => {
    const plan = visualPlan(STATUSES, BY_STATUS);
    expect(plan).toMatchObject({
      kind: "chart",
      title: "Count by Invoice.status · top 50",
      note: "3 groups",
    });
    expect(chartOf(plan)).toEqual({
      kind: "bar",
      label: "Invoice.status",
      measure: "Count",
      horizontal: false,
      points: [
        { label: "paid", value: 40 },
        { label: "open", value: 20 },
        { label: "No status", value: 5 },
      ],
    });
    if (plan.kind !== "chart") return;
    expect(plan.table.columns).toEqual([
      { label: "Invoice.status", numeric: false },
      { label: "Count", numeric: true },
    ]);
    expect(plan.table.rows[2]).toEqual([
      { kind: "none" },
      { kind: "number", value: 5 },
    ]);
    // Asked for a table, a figure or a line (not dates): the table.
    for (const kind of ["table", "figure", "line"] as const)
      expect(visualPlan(STATUSES, BY_STATUS, { kind }).kind, kind).toBe(
        "table",
      );
    // One group is no chart.
    expect(
      visualPlan(
        output({
          kind: "group_count",
          groups: [{ value: { untrusted: "paid" }, count: 3 }],
          total_groups: 1,
        }),
        BY_STATUS,
      ).kind,
    ).toBe("table");
  });

  it("turn bars sideways for long or many labels, and say what the bound left out", () => {
    const sums = output({
      kind: "grouped_aggregate",
      groups: [
        {
          value: { untrusted: "Globex Corporation" },
          aggregate: 1_200_000,
          values: 2,
        },
        { value: { untrusted: "Initech" }, aggregate: 900_000.5, values: 1 },
        { value: { untrusted: "Hooli" }, aggregate: null, values: 0 },
      ],
      total_groups: 12,
      cut: "1 group left out",
    });
    const plan = visualPlan(sums, SUM_BY_CUSTOMER, { kind: "bar" });
    expect(plan.note).toBe("3 of 12 groups · 1 group left out");
    expect(chartOf(plan)).toEqual({
      kind: "bar",
      label: "Customer.name",
      measure: "Sum of Invoice.amount_due",
      horizontal: true,
      // An aggregate of no values has no bar; the table keeps its row.
      points: [
        {
          label: "Globex Corporation",
          value: 1_200_000,
          detail: "Over 2 values",
        },
        { label: "Initech", value: 900_000.5, detail: "Over 1 value" },
      ],
    });
    if (plan.kind !== "chart") return;
    expect(plan.table.rows).toHaveLength(3);
    expect(plan.table.columns.map((c) => c.label)).toEqual([
      "Customer.name",
      "Sum of Invoice.amount_due",
      "Values",
    ]);
    // Nine short labels turn sideways too.
    const nine = output({
      kind: "group_count",
      groups: Array.from({ length: 9 }, (_, i) => ({
        value: { untrusted: `${i}` },
        count: 9 - i,
      })),
      total_groups: 9,
    });
    const sideways = chartOf(visualPlan(nine, BY_STATUS));
    expect(sideways.horizontal).toBe(true);
    expect(sideways.points[0]).toEqual({ label: "0", value: 9 });
  });

  it("draw counts by date as a line, oldest first, those without one left to the table", () => {
    const byDay = output({
      kind: "group_count",
      groups: [
        { value: { untrusted: "2026-10-02" }, count: 4 },
        { value: { untrusted: "2026-09-30" }, count: 2 },
        { value: { untrusted: "2026-10-01T09:00:00Z" }, count: 7 },
        { value: null, count: 1 },
      ],
      total_groups: 4,
    });
    const signed: OntologyQuery = {
      ...BY_STATUS,
      return: { group_count: { at: 0, prop: "signed_on" } },
    };
    expect(chartOf(visualPlan(byDay, signed))).toEqual({
      kind: "line",
      label: "Invoice.signed_on",
      measure: "Count",
      horizontal: false,
      // Each at its date's time, for a time axis.
      points: [
        { label: "2026-09-30", value: 2, at: Date.UTC(2026, 8, 30) },
        {
          label: "2026-10-01T09:00:00Z",
          value: 7,
          at: Date.UTC(2026, 9, 1, 9),
        },
        { label: "2026-10-02", value: 4, at: Date.UTC(2026, 9, 2) },
      ],
    });
    // In time order, whatever the precision or offset.
    const mixed = output({
      kind: "group_count",
      groups: [
        { value: { untrusted: "2026-01-01T23:00:00-05:00" }, count: 1 },
        { value: { untrusted: "2026-01-02T01:00:00Z" }, count: 2 },
        { value: { untrusted: "2026-01" }, count: 3 },
        { value: { untrusted: "2026-01-02T01:00:00+00:00" }, count: 4 },
      ],
      total_groups: 4,
    });
    // The same time written two ways is in the order its text sorts.
    expect(
      chartOf(visualPlan(mixed, signed)).points.map((p) => p.value),
    ).toEqual([3, 4, 2, 1]);
    // As bars when asked, in the output's order.
    expect(
      chartOf(visualPlan(byDay, signed, { kind: "bar" })).points.map(
        (p) => p.label,
      ),
    ).toEqual([
      "2026-10-02",
      "2026-09-30",
      "2026-10-01T09:00:00Z",
      "No signed_on",
    ]);
  });

  it("draw dates as a line only when the output holds every one", () => {
    const issued: OntologyQuery = {
      ...BY_STATUS,
      return: { group_count: { at: 0, prop: "issued_on", limit: 3 } },
    };
    // The 3 busiest of 300 days, most first: no trend.
    const busiest = output({
      kind: "group_count",
      groups: [
        { value: { untrusted: "2026-10-05" }, count: 9 },
        { value: { untrusted: "2026-10-01" }, count: 7 },
        { value: { untrusted: "2026-10-03" }, count: 4 },
      ],
      total_groups: 300,
    });
    const why =
      "3 of 300 groups · A line needs every date, and this leaves some out";
    // Left to the shape: bars in the output's order, saying why.
    const plan = visualPlan(busiest, issued);
    expect(plan.note).toBe(why);
    expect(chartOf(plan)).toMatchObject({
      kind: "bar",
      points: [
        { label: "2026-10-05", value: 9 },
        { label: "2026-10-01", value: 7 },
        { label: "2026-10-03", value: 4 },
      ],
    });
    // Asked for a line: the table, saying why.
    expect(visualPlan(busiest, issued, { kind: "line" })).toMatchObject({
      kind: "table",
      note: why,
    });
    // Asked for bars or a table: those, with nothing to explain.
    expect(visualPlan(busiest, issued, { kind: "bar" })).toMatchObject({
      kind: "chart",
      note: "3 of 300 groups",
      chart: { kind: "bar" },
    });
    expect(visualPlan(busiest, issued, { kind: "table" })).toMatchObject({
      kind: "table",
      note: "3 of 300 groups",
    });
    // Groups the question's size bound left out are missing dates too.
    const bounded = output({
      ...busiest,
      total_groups: 4,
      cut: "1 group left out",
    });
    expect(chartOf(visualPlan(bounded, issued)).kind).toBe("bar");
    // Every date there is: a line, oldest first, whatever the limit.
    const every = output({ ...busiest, total_groups: 3 });
    expect(chartOf(visualPlan(every, issued, { kind: "line" }))).toMatchObject({
      kind: "line",
      points: [
        { label: "2026-10-01", at: Date.UTC(2026, 9, 1) },
        { label: "2026-10-03", at: Date.UTC(2026, 9, 3) },
        { label: "2026-10-05", at: Date.UTC(2026, 9, 5) },
      ],
    });
    // A date and time written with a space is a date; text shaped like one
    // that isn't, isn't.
    const spaced = output({
      kind: "group_count",
      groups: [
        { value: { untrusted: "2026-10-02 09:30" }, count: 2 },
        { value: { untrusted: "2026-10-01 18:00:00" }, count: 1 },
      ],
      total_groups: 2,
    });
    expect(chartOf(visualPlan(spaced, issued)).points).toEqual([
      {
        label: "2026-10-01 18:00:00",
        value: 1,
        at: new Date(2026, 9, 1, 18).getTime(),
      },
      {
        label: "2026-10-02 09:30",
        value: 2,
        at: new Date(2026, 9, 2, 9, 30).getTime(),
      },
    ]);
    const impossible = output({
      kind: "group_count",
      groups: [
        { value: { untrusted: "2026-13-45" }, count: 2 },
        { value: { untrusted: "2026-10-01" }, count: 1 },
      ],
      total_groups: 2,
    });
    expect(chartOf(visualPlan(impossible, issued)).kind).toBe("bar");
  });

  it("show a group's value as the engine wrote it, not as a document's, its numbers a number column", () => {
    const byQuantity: OntologyQuery = {
      ...BY_STATUS,
      return: { group_count: { at: 0, prop: "quantity" } },
    };
    const quantities = output({
      kind: "group_count",
      groups: [
        { value: { untrusted: "5" }, count: 4 },
        { value: { untrusted: "10" }, count: 3 },
        { value: { untrusted: "-2.5e3" }, count: 2 },
        { value: null, count: 1 },
      ],
      total_groups: 4,
    });
    const plan = visualPlan(quantities, byQuantity);
    if (plan.kind !== "chart") throw new Error(plan.kind);
    // A number column, so it sorts as numbers, each shown as written.
    expect(plan.table.columns[0]).toEqual({
      label: "Invoice.quantity",
      numeric: true,
    });
    expect(plan.table.rows.map((r) => r[0])).toEqual([
      { kind: "text", text: "5", untrusted: false },
      { kind: "text", text: "10", untrusted: false },
      { kind: "text", text: "-2.5e3", untrusted: false },
      { kind: "none" },
    ]);
    expect(plan.chart.points.map((p) => p.label)).toEqual([
      "5",
      "10",
      "-2.5e3",
      "No quantity",
    ]);
    // Anything JSON doesn't write as a number makes it a text column.
    for (const text of ["02139", "1.", "+5", ".5", "5 units", "0x1F", ""]) {
      const mixed = visualPlan(
        output({
          kind: "group_count",
          groups: [
            { value: { untrusted: "5" }, count: 2 },
            { value: { untrusted: text }, count: 1 },
          ],
          total_groups: 2,
        }),
        byQuantity,
      );
      if (mixed.kind !== "chart") throw new Error(mixed.kind);
      expect(mixed.table.columns[0]!.numeric, text).toBe(false);
    }
    // Only nulls is no number column.
    const none = visualPlan(
      output({
        kind: "group_count",
        groups: [{ value: null, count: 2 }],
        total_groups: 1,
      }),
      byQuantity,
    );
    if (none.kind !== "table") throw new Error(none.kind);
    expect(none.table.columns[0]!.numeric).toBe(false);
  });

  it("show rows as a table, named as the query names its columns", () => {
    const plan = visualPlan(ROWS, LARGEST);
    expect(plan).toMatchObject({
      kind: "table",
      title: "Invoice → billed_to → Customer",
      note: "3 of 65 rows · 2 rows left out",
    });
    if (plan.kind !== "table") return;
    expect(plan.table.columns).toEqual([
      { label: "Invoice", numeric: false },
      { label: "Invoice.amount_due", numeric: true },
      { label: "Customer", numeric: false },
      { label: "Invoice.terms", numeric: false },
    ]);
    expect(plan.table.rows[0]).toEqual([
      {
        kind: "entity",
        ref: { type: "Invoice", id: "in_1" },
        name: "in_1",
      },
      { kind: "number", value: 1_200_000 },
      {
        kind: "entity",
        ref: { type: "Customer", id: "cus_GLOBEX01" },
        name: "Globex",
      },
      { kind: "text", text: "Net 30", untrusted: true },
    ]);
  });

  it("draw rows as bars of their first entity, or a line of their dates, when asked", () => {
    const bars = chartOf(visualPlan(ROWS, LARGEST, { kind: "bar" }));
    expect(bars).toEqual({
      kind: "bar",
      label: "Invoice",
      measure: "Invoice.amount_due",
      horizontal: false,
      // A row without the number has no bar.
      points: [
        {
          label: "in_1",
          value: 1_200_000,
          entity: { type: "Invoice", id: "in_1" },
        },
        {
          label: "in_2",
          value: 900_000,
          entity: { type: "Invoice", id: "in_2" },
        },
      ],
    });
    // No entity: the first text column labels the bars.
    const names: OntologyQuery = {
      ...LARGEST,
      return: {
        rows: {
          columns: [
            { at: 1, prop: "name" },
            { at: 0, prop: "amount_due" },
          ],
        },
      },
    };
    const named = output({
      kind: "rows",
      rows: [
        { cells: ["Globex", 3], edges: [] },
        { cells: ["Initech", 2], edges: [] },
      ],
      total: 2,
    });
    expect(chartOf(visualPlan(named, names, { kind: "bar" })).points).toEqual([
      { label: "Globex", value: 3 },
      { label: "Initech", value: 2 },
    ]);
    // Dates and a number make a line when asked, oldest first.
    const dated = output({
      kind: "rows",
      rows: [
        { cells: ["2026-10-02", 3], edges: [] },
        { cells: ["2026-10-01", 5], edges: [] },
      ],
      total: 2,
    });
    expect(chartOf(visualPlan(dated, names, { kind: "line" })).points).toEqual([
      { label: "2026-10-01", value: 5, at: Date.UTC(2026, 9, 1) },
      { label: "2026-10-02", value: 3, at: Date.UTC(2026, 9, 2) },
    ]);
    // Some of the rows make a line only when ordered by its dates: the
    // first or last of them.
    const latest = output({ ...dated, total: 40 });
    expect(visualPlan(latest, names, { kind: "line" })).toMatchObject({
      kind: "table",
      note: "2 of 40 rows · A line needs every date, and this leaves some out",
    });
    const ordered = (by: { at: number; prop: string }): OntologyQuery => ({
      ...names,
      return: {
        rows: {
          columns: [
            { at: 1, prop: "name" },
            { at: 0, prop: "amount_due" },
          ],
          order_by: { ...by, desc: true },
          limit: 2,
        },
      },
    });
    expect(
      visualPlan(latest, ordered({ at: 1, prop: "name" }), { kind: "line" }),
    ).toMatchObject({ kind: "chart", note: "2 of 40 rows" });
    expect(
      visualPlan(latest, ordered({ at: 0, prop: "amount_due" }), {
        kind: "line",
      }).kind,
    ).toBe("table");
    // Bars of some rows are a ranking: drawn.
    expect(visualPlan(latest, names, { kind: "bar" }).kind).toBe("chart");
    // No number, no dates, or one row: the table.
    const words = output({
      kind: "rows",
      rows: [
        { cells: ["Globex", "a"], edges: [] },
        { cells: ["Initech", "b"], edges: [] },
      ],
      total: 2,
    });
    expect(visualPlan(words, names, { kind: "bar" }).kind).toBe("table");
    expect(visualPlan(named, names, { kind: "line" }).kind).toBe("table");
    expect(
      visualPlan({ ...named, rows: named.rows.slice(0, 1), total: 1 }, names, {
        kind: "bar",
      }).kind,
    ).toBe("table");
    expect(visualPlan(named, names, { kind: "figure" }).kind).toBe("table");
  });

  it("name the columns of rows its query doesn't describe by their place", () => {
    const plan = visualPlan(
      output({
        kind: "rows",
        rows: [
          { cells: [globex, 1], edges: [] },
          { cells: [globex], edges: [] },
        ],
        total: 2,
      }),
      COUNT,
    );
    if (plan.kind !== "table") throw new Error(plan.kind);
    expect(plan.table.columns.map((c) => c.label)).toEqual([
      "Column 1",
      "Column 2",
    ]);
    // Without a column's prop, an entity-shaped cell is the entity; a row
    // short of cells has none in the rest.
    expect(plan.table.rows[1]).toEqual([
      {
        kind: "entity",
        ref: { type: "Customer", id: "cus_GLOBEX01" },
        name: "Globex",
      },
      { kind: "none" },
    ]);
    // Groups the query doesn't describe are values and what they measure.
    const groups = visualPlan(
      output({
        kind: "grouped_aggregate",
        groups: [
          { value: { untrusted: "a" }, aggregate: 2, values: 1 },
          { value: null, aggregate: 1, values: 1 },
        ],
        total_groups: 2,
      }),
      COUNT,
    );
    expect(chartOf(groups)).toMatchObject({
      label: "Value",
      measure: "Aggregate",
      points: [
        { label: "a", value: 2 },
        { label: "None", value: 1 },
      ],
    });
  });
});

describe("chart sizes", () => {
  it("fit sideways bars to how many show, and plot anything else at one height", () => {
    const chart = chartOf(visualPlan(STATUSES, BY_STATUS));
    expect(chartHeight(chart, false, 15)).toBe(248);
    expect(chartHeight(chart, true, 15)).toBe(3 * 30 + 28);
    expect(chartHeight(chart, true, 2)).toBe(2 * 30 + 28);
    expect(chartHeight({ ...chart, kind: "line", points: [] }, true, 15)).toBe(
      248,
    );
  });
});

describe("drawable visuals", () => {
  it("are those whose result has an output", () => {
    const result: Extract<AskResult, { status: "answered" }> = {
      status: "answered",
      answer: {
        blocks: [{ para: "x" }],
        visuals: [
          { evidence_id: "e1" },
          { evidence_id: "e2" },
          { evidence_id: "e3" },
        ],
      },
      citations: [],
      queries: [
        { evidence_id: "e1", query: BY_STATUS, output: STATUSES },
        { evidence_id: "e2", query: COUNT, output: null },
      ],
      usage: USAGE,
    };
    expect(drawable(result)).toEqual([
      { visual: { evidence_id: "e1" }, query: BY_STATUS, output: STATUSES },
    ]);
    expect(drawable({ ...result, answer: { blocks: [] } })).toEqual([]);
  });
});
