import { describe, it, expect, vi } from "vitest";
import { createElement as h } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import {
  DataGrid,
  PageHeader,
  StatusBadge,
  Tabs,
  columnKind,
  compareValues,
  connectorKey,
  formatNumber,
  formatValue,
  inferKind,
  toStatus,
} from "../apps/web/src/kit/index.js";
import {
  AnswerChart,
  AnswerStats,
  AnswerTable,
  answerColumns,
  chartPlan,
  sortAnswer,
  type ChartPlan,
} from "../apps/web/src/areas/analyst/answer-view.js";
import { exactText } from "../apps/web/src/areas/workspace/metrics.js";
import {
  activeSection,
  childActive,
  paletteSources,
  secondaryNav,
} from "../apps/web/src/shell/nav.js";
import { paths } from "../apps/web/src/paths.js";
import { DataTable } from "../apps/web/src/ui.js";
import { chatIsActive } from "../apps/web/src/areas/analyst/launcher.js";
import {
  collapseActivity,
  describeActivity,
} from "../packages/shared/src/index.js";

// Server rendering cannot hover or focus a chart. While `charts.focused` is
// set, state that starts empty (null or false) starts set (0 or true), so a
// chart renders as if its first point had focus: with its tooltip and its
// live-region text.
const charts = vi.hoisted(() => ({ focused: false }));
vi.mock("react", async (importOriginal) => {
  const react = await importOriginal<typeof import("react")>();
  const start = (v: unknown) =>
    !charts.focused ? v : v === null ? 0 : v === false ? true : v;
  return { ...react, useState: (v: unknown) => react.useState(start(v)) };
});
const focusedChart = (plan: ChartPlan) => {
  charts.focused = true;
  try {
    return renderToStaticMarkup(h(AnswerChart, { plan }));
  } finally {
    charts.focused = false;
  }
};
const render = (node: any, url = "/") =>
  renderToStaticMarkup(h(MemoryRouter, { initialEntries: [url] }, node));

describe("workspace UI kit", () => {
  it("renders only a window of a large grid, with typed, formatted cells", () => {
    const rows = Array.from({ length: 10000 }, (_, i) => ({
      id: i,
      amount: i * 1000.5,
      note: i % 2 ? null : { tag: "x" },
    }));
    const html = render(
      h(DataGrid, {
        label: "Orders",
        rows,
        columns: [
          { key: "id", type: "BIGINT" },
          { key: "amount", type: "DOUBLE" },
          { key: "note", type: "JSON" },
        ],
      }),
    );
    const rendered = html.match(/role="row"/g)!.length;
    expect(rendered).toBeGreaterThan(5);
    expect(rendered).toBeLessThan(60);
    expect(html).toContain('aria-rowcount="10001"');
    expect(html).toContain("1,000.5");
    expect(html).toContain("{&quot;tag&quot;:&quot;x&quot;}");
    expect(html).toContain('class="hf-null"');
  });
  it("maps types, statuses and connectors", () => {
    expect(columnKind("TIMESTAMP WITH TIME ZONE")).toBe("timestamp");
    expect(columnKind("DECIMAL(18,3)")).toBe("number");
    expect(columnKind("STRUCT(a INTEGER)")).toBe("json");
    expect(inferKind(["2026-01-02", null])).toBe("date");
    expect(formatValue(null, "string")).toBeNull();
    expect(toStatus("awaiting_review")).toBe("review");
    expect(toStatus("retrying")).toBe("failed");
    expect(connectorKey("PostgreSQL")).toBe("postgres");
    expect(connectorKey(undefined, "Stripe")).toBe("stripe");
  });
  it("reads a DuckDB type by its name, not its parameters", () => {
    expect(columnKind("UTINYINT")).toBe("integer");
    expect(columnKind("UHUGEINT")).toBe("integer");
    expect(columnKind("ENUM('REAL', 'INTEGER', 'MAPLE')")).toBe("string");
    expect(columnKind("INTEGER[2]")).toBe("json");
    expect(columnKind("INTERVAL")).toBe("interval");
    expect(columnKind("INTERVAL[]")).toBe("json");
  });
  it("shows and sorts INTERVAL values as DuckDB does", () => {
    const waits = [
      { wait: { months: 0, days: 1, micros: "-3600000000" } },
      { wait: { months: 14, days: 3, micros: "14706789000" } },
      { wait: { months: 0, days: 0, micros: "82800000000" } },
      { wait: { months: 0, days: -1, micros: "0" } },
    ];
    // 23 hours sorts before 1 day less an hour, as in DuckDB's ORDER BY.
    const ascending = [
      "-1 day",
      "23:00:00",
      "1 day -01:00:00",
      "1 year 2 months 3 days 04:05:06.789",
    ];
    const html = render(
      h(DataGrid, {
        label: "Waits",
        rows: waits,
        columns: [{ key: "wait", type: "INTERVAL" }],
      }),
    );
    expect(
      [...html.matchAll(/role="gridcell"[^>]*title="([^"]*)"/g)].map(
        (m) => m[1],
      ),
    ).toEqual([ascending[2], ascending[3], ascending[1], ascending[0]]);
    // What a click on the column header sorts with.
    expect(
      [...waits]
        .sort((a, b) => compareValues(a.wait, b.wait, columnKind("INTERVAL")))
        .map((r) => formatValue(r.wait, "interval")),
    ).toEqual(ascending);
    // The Analyst types the column from its values and sorts it the same way.
    expect(inferKind(waits.map((r) => r.wait))).toBe("interval");
    const columns = answerColumns(waits);
    expect(
      sortAnswer(waits, columns, { key: "wait", dir: "desc" }).map((r) =>
        formatValue(r.wait, columns[0].kind),
      ),
    ).toEqual([...ascending].reverse());
    // The dataset preview has no types, so it knows an interval by its shape.
    expect(render(h(DataTable, { rows: waits.slice(0, 1) }))).toContain(
      "1 day -01:00:00",
    );
  });
  it("keeps neighboring 64-bit integers distinct", () => {
    expect(formatNumber(9007199254740993n)).toBe("9,007,199,254,740,993");
    expect(formatNumber("9007199254740993")).toBe("9,007,199,254,740,993");
    expect(formatValue("9007199254740993", "number")).toBe(
      "9,007,199,254,740,993",
    );
    // A Number past 2^53 keeps its usual digits, not its binary expansion.
    expect(formatNumber(1e25)).toBe("10,000,000,000,000,000,000,000,000");
    for (const kind of ["integer", "number"] as const) {
      expect(
        compareValues("9007199254740992", "9007199254740993", kind),
      ).toBeLessThan(0);
      expect(
        compareValues(9007199254740993n, 9007199254740992n, kind),
      ).toBeGreaterThan(0);
      expect(compareValues("12.5", "9007199254740993", kind)).toBeLessThan(0);
    }
  });
  it("orders mixed integer representations transitively", () => {
    const a = "9007199254740992",
      b = 9007199254740992,
      c = "9007199254740993";
    expect(compareValues(a, b, "integer")).toBe(0);
    expect(compareValues(b, c, "integer")).toBeLessThan(0);
    expect(compareValues(a, c, "integer")).toBeLessThan(0);
  });
  it("orders integers and decimals beyond 2^53 transitively", () => {
    // A Number would make the decimal equal to both of its neighbours.
    expect(
      compareValues(9007199254740992, "9007199254740992.5", "number"),
    ).toBeLessThan(0);
    expect(
      compareValues("9007199254740993", "9007199254740992.5", "number"),
    ).toBeGreaterThan(0);
    expect(compareValues("12.50", 12.5, "number")).toBe(0);
    expect(compareValues("1e21", 1e21, "number")).toBe(0);
    const sorted = [
      "-1.5",
      0.25,
      2n,
      "12.5",
      9007199254740992,
      "9007199254740992.5",
      "9007199254740993",
      "1e17",
      "1e300",
    ];
    // Every starting order sorts the same way.
    for (let i = 0; i < sorted.length; i++) {
      const start = [...sorted.slice(i), ...sorted.slice(0, i)].reverse();
      expect(start.sort((x, y) => compareValues(x, y, "number"))).toEqual(
        sorted,
      );
    }
  });
  it("shows and sorts answer integers beyond 2^53 exactly", () => {
    // DuckDB sends sums and counts of BIGINT columns as text.
    const rows = [
      { region: "EU", total: "9007199254740993" },
      { region: "NA", total: null },
      { region: "US", total: "9007199254740992" },
      { region: "APAC", total: "12.5" },
    ];
    const columns = answerColumns(rows),
      totals = (dir: "asc" | "desc") =>
        sortAnswer(rows, columns, { key: "total", dir }).map((r) => r.total);
    expect(columns[1]).toMatchObject({ kind: "number", numeric: true });
    expect(totals("asc")).toEqual([
      "12.5",
      "9007199254740992",
      "9007199254740993",
      null,
    ]);
    expect(totals("desc")).toEqual([
      "9007199254740993",
      "9007199254740992",
      "12.5",
      null,
    ]);
    expect(render(h(AnswerTable, { rows, label: "Totals" }))).toContain(
      "9,007,199,254,740,993",
    );
    const stats = render(
      h(AnswerStats, { row: { total: "9007199254740993", id: "42" } }),
    );
    expect(stats).toContain("9,007,199,254,740,993");
    expect(stats).not.toContain("9,007,199,254,740,992");
  });
  it("gives answer chart values beyond 2^53 exactly in tooltips and to screen readers", () => {
    const breakdown = chartPlan([
      { region: "EU", total: "9007199254740993" },
      { region: "US", total: "12" },
      { region: "NA", total: "12.25" },
    ])!;
    expect(breakdown.kind).toBe("breakdown");
    expect(breakdown.points.map((p) => exactText(p, "number"))).toEqual([
      "9,007,199,254,740,993",
      "12",
      "12.3",
    ]);
    // The charts show and announce that text, not the nearest Number.
    const bars = focusedChart(breakdown);
    expect(bars).toContain(
      '<span class="wk-chart-tip"><strong>9,007,199,254,740,993</strong>',
    );
    expect(bars).toContain('aria-live="polite">EU: 9,007,199,254,740,993<');
    expect(bars).not.toContain("9,007,199,254,740,992");
    const trend = chartPlan([
      { month: "2026-02", total: 1.25 },
      { month: "2026-01", total: "-9007199254740995" },
    ])!;
    expect(trend).toMatchObject({ kind: "trend", grain: "month" });
    expect(trend.points.map((p) => [p.label, exactText(p, "number")])).toEqual([
      ["2026-01", "-9,007,199,254,740,995"],
      ["2026-02", "1.25"],
    ]);
    const line = focusedChart(trend);
    expect(line).toContain("<strong>-9,007,199,254,740,995</strong>");
    expect(line).toMatch(/aria-live="polite">[^<]+: -9,007,199,254,740,995</);
    expect(line).not.toMatch(/9,007,199,254,740,99[46]/);
  });
  it("renders ARIA tabs synced to the URL and a titled page header", () => {
    const html = render(
      h(Tabs, {
        label: "Dataset",
        items: [
          { id: "preview", label: "Preview" },
          { id: "schema", label: "Schema", count: 12 },
        ],
      }),
      "/data/datasets/d1?tab=schema",
    );
    expect(html).toContain('role="tablist"');
    expect(html).toMatch(/id="tab-tab-schema"[^>]*aria-selected="true"/);
    const header = render(
      h(PageHeader, {
        title: "Orders",
        breadcrumbs: [{ label: "Data", to: "/data" }, { label: "Orders" }],
        actions: h(StatusBadge, { status: "healthy" }),
      }),
    );
    expect(header).toContain("<h1>Orders</h1>");
    expect(header).toContain('aria-current="page"');
    expect(header).toContain("Healthy");
  });
  it("keeps nested routes in their sidebar section", () => {
    expect(activeSection("/")).toBe("home");
    expect(activeSection("/data/datasets/x")).toBe("data");
    expect(activeSection("/records/abc")).toBe("ontology");
    expect(activeSection("/runs/1")).toBe("analyst");
    // Removed sections: SQL, Pipelines, Activity and Dashboards have no
    // sidebar entry.
    expect(activeSection("/explore")).toBeUndefined();
    expect(activeSection("/pipelines")).toBeUndefined();
    expect(activeSection("/activity")).toBeUndefined();
    expect(activeSection("/dashboards")).toBeUndefined();
    expect(childActive("/data", { to: "/data", label: "", end: true })).toBe(
      true,
    );
    expect(
      childActive("/data/connections", { to: "/data", label: "", end: true }),
    ).toBe(false);
  });
  it("keeps Settings as the only secondary section", () => {
    expect(secondaryNav.map((n) => n.id)).toEqual(["settings"]);
    // Area path helpers merge into one `paths`.
    expect(paths.dataset("a b", "schema")).toBe(
      "/data/datasets/a%20b?tab=schema",
    );
    expect(paths.thread("r1")).toBe("/analyst/r1");
  });
  it("finds each chat once, by its first question", () => {
    const chats = paletteSources.find((s) => s.id === "threads")!;
    const run = (id: string, data: Record<string, unknown>) =>
      ({
        id,
        kind: "run",
        name: id,
        createdAt: "2026-01-01T00:00:00.000Z",
        updatedAt: "2026-01-01T00:00:00.000Z",
        data,
      }) as any;
    expect(chats.label).toBe("Chats");
    const first = run("r1", {
      goal: "Which accounts churned?",
      threadId: "r1",
    });
    expect(chats.filter!(first)).toBe(true);
    expect(chats.title!(first)).toBe("Which accounts churned?");
    expect(chats.href(first)).toBe("/analyst/r1");
    // A follow-up belongs to its chat; setup runs are not chats; a run from
    // before chats had ids is its own chat.
    const followUp = run("r2", { goal: "And last month?", threadId: "r1" });
    expect(chats.filter!(followUp)).toBe(false);
    expect(chats.href(followUp)).toBe("/analyst/r1");
    expect(chats.filter!(run("s1", { task: "onboarding_build" }))).toBe(false);
    expect(chats.filter!(run("r0", { goal: "Older question" }))).toBe(true);
    // The sidebar marks chats still being answered.
    expect(chatIsActive({ status: "running" })).toBe(true);
    expect(chatIsActive({ status: "queued" })).toBe(true);
    expect(chatIsActive({ status: "awaiting_review" })).toBe(false);
    expect(chatIsActive({ status: "succeeded" })).toBe(false);
  });
  it("describes activity the same way everywhere", () => {
    const at = (m: number) =>
      new Date(Date.UTC(2026, 0, 1, 0, m)).toISOString();
    const job = (id: string, name: string, data: object, m: number) => ({
      id,
      kind: "job",
      name,
      updatedAt: at(m),
      data,
    });
    const setup = describeActivity({
      id: "r1",
      kind: "run",
      name: "Build",
      updatedAt: at(1),
      data: { task: "onboarding_build", status: "succeeded" },
    });
    expect(setup).toMatchObject({
      kind: "setup",
      label: "Setup",
      title: "Built your workspace",
    });
    expect(
      describeActivity(
        job(
          "j0",
          "Prepare local",
          {
            type: "provider_prepare",
            status: "running",
            settings: { provider: "local" },
          },
          0,
        ),
      ).title,
    ).toBe("Preparing the local AI model");
    const feed = [
      job("j3", "Run Users", { type: "pipeline", status: "queued" }, 3),
      job("j2", "Run Accounts", { type: "pipeline", status: "succeeded" }, 2),
      job("j1", "Run Users", { type: "pipeline", status: "succeeded" }, 1),
    ].map(describeActivity);
    expect(feed[0]).toMatchObject({
      title: "Users",
      label: "Pipeline build",
    });
    const all = collapseActivity(feed);
    expect(all.map((a) => [a.id, a.repeats])).toEqual([
      ["j3", 1],
      ["j2", 0],
    ]);
    // A timeline keeps history apart from back-to-back repeats.
    expect(collapseActivity(feed, "adjacent")).toHaveLength(3);
  });
});
