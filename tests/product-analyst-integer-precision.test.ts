import { createElement as h } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import {
  AnswerStats,
  AnswerTable,
  chartPlan,
} from "../apps/web/src/areas/analyst/answer-view.js";

describe("Analyst integer precision", () => {
  it("preserves signed and unsigned 64-bit values in answer stats", () => {
    const html = renderToStaticMarkup(
      h(AnswerStats, {
        row: {
          total: "9007199254740993",
          adjustment: "-9007199254740993",
          signed: "9223372036854775807",
          unsigned: 18446744073709551615n,
        },
      }),
    );
    for (const value of [
      "9,007,199,254,740,993",
      "-9,007,199,254,740,993",
      "9,223,372,036,854,775,807",
      "18,446,744,073,709,551,615",
    ])
      expect(html).toContain(`>${value}</dd>`);
  });

  it("keeps IDs, years, decimals and missing values in their existing form", () => {
    const html = renderToStaticMarkup(
      h(AnswerStats, {
        row: {
          customer_id: "9007199254740993",
          year: "2026",
          amount: "1234.56",
          missing: null,
        },
      }),
    );
    expect(html).toContain(">9007199254740993</dd>");
    expect(html).toContain(">2026</dd>");
    expect(html).toContain(">1,234.56</dd>");
    expect(html).toContain('class="hf-null"');
  });

  it("shows mixed integer representations exactly in the table", () => {
    const html = renderToStaticMarkup(
      h(AnswerTable, {
        label: "Totals",
        rows: [
          { total: "9007199254740993" },
          { total: 9007199254740992 },
          { total: -9007199254740993n },
          { total: null },
        ],
      }),
    );
    expect(html).toContain("9,007,199,254,740,993");
    expect(html).toContain("-9,007,199,254,740,993");
  });

  it.each(["category", "month"])(
    "charts unsafe integer measures in %s charts with their exact text",
    (label) => {
      const labels = label === "month" ? ["2026-01", "2026-02"] : ["A", "B"];
      for (const [value, text] of [
        ["9007199254740993", "9,007,199,254,740,993"],
        ["-9007199254740993", "-9,007,199,254,740,993"],
        [9223372036854775807n, "9,223,372,036,854,775,807"],
      ] as const)
        expect(
          chartPlan([
            { [label]: labels[0], total: value },
            { [label]: labels[1], total: "10" },
          ])?.points,
        ).toEqual([
          { label: labels[0], value: Number(value), text },
          { label: labels[1], value: 10 },
        ]);
    },
  );

  it("still charts safe integers and ordinary decimal measures", () => {
    expect(
      chartPlan([
        { category: "A", total: "9007199254740991" },
        { category: "B", total: "12.5" },
      ]),
    ).toMatchObject({
      kind: "breakdown",
      points: [
        { label: "A", value: 9007199254740991 },
        { label: "B", value: 12.5 },
      ],
    });
    expect(
      chartPlan([
        { month: "2026-02", total: "20.5" },
        { month: "2026-01", total: "10" },
      ]),
    ).toMatchObject({
      kind: "trend",
      points: [
        { label: "2026-01", value: 10 },
        { label: "2026-02", value: 20.5 },
      ],
    });
  });
});
