import { it, expect } from "vitest";
import { DuckDBInstance } from "@duckdb/node-api";
import {
  compareIntervals,
  formatInterval,
  isInterval,
  type IntervalValue,
} from "../packages/shared/src/index.js";

it("reads intervals as DuckDB writes them", () => {
  const cases: [IntervalValue, string][] = [
    [{ months: 0, days: 0, micros: "0" }, "00:00:00"],
    [{ months: 0, days: 0, micros: "5400000000" }, "01:30:00"],
    [
      { months: 14, days: 3, micros: "14706789000" },
      "1 year 2 months 3 days 04:05:06.789",
    ],
    [{ months: -14, days: 0, micros: "0" }, "-1 year -2 months"],
    [{ months: 0, days: -1, micros: "0" }, "-1 day"],
    [{ months: 0, days: 0, micros: "90000000000" }, "25:00:00"],
    [{ months: 0, days: 0, micros: "500000" }, "00:00:00.5"],
    // Micros past 2^53 keep every digit.
    [
      { months: 0, days: 0, micros: "9223372036854775807" },
      "2562047788:00:54.775807",
    ],
  ];
  for (const [value, text] of cases) expect(formatInterval(value)).toBe(text);
  expect(isInterval({ months: 0, days: 2, micros: "0" })).toBe(true);
  expect(isInterval({ months: 0, days: 2, micros: "0", note: "x" })).toBe(
    false,
  );
  expect(isInterval({ months: 0, days: 2 })).toBe(false);
});

// DuckDB is the reference for both the text and the order, ties included:
// 1 month, 30 days and 720 hours are equal there.
it("writes and orders random intervals as DuckDB does", async () => {
  // Seeded, so a failure reproduces: mixed signs and sizes in every part.
  let seed = 42;
  const random = () =>
    (seed = (seed * 1103515245 + 12345) % 2147483648) / 2147483648;
  const any = (...scales: number[]) =>
    random() < 0.3
      ? 0
      : Math.round(
          (random() * 2 - 1) * scales[Math.floor(random() * scales.length)],
        );
  const values = Array.from(
    { length: 1000 },
    (_, i) =>
      `(${i}, to_months(${any(1, 13, 1000)}) + to_days(${any(1, 31, 400, 1e5)}) + to_microseconds(${any(1, 999999, 36e8, 1728e8, 3888e9, 9e15)}::BIGINT))`,
  );
  values.push(
    "(1000, INTERVAL 1 MONTH)",
    "(1001, INTERVAL 30 DAY)",
    "(1002, INTERVAL 720 HOUR)",
    "(1003, INTERVAL 23 HOUR)",
    "(1004, INTERVAL 1 DAY - INTERVAL 1 HOUR)",
  );
  const db = await DuckDBInstance.create(":memory:"),
    c = await db.connect();
  try {
    await c.run(
      `CREATE TABLE t AS SELECT * FROM (VALUES ${values.join(",")}) v(i, iv)`,
    );
    const rows = (
      await c.runAndReadAll("SELECT iv, iv::VARCHAR AS text FROM t ORDER BY i")
    ).getRowObjectsJson();
    const intervals = rows.map((r) => r.iv as unknown as IntervalValue);
    expect(intervals.every(isInterval)).toBe(true);
    expect(intervals.map(formatInterval)).toEqual(rows.map((r) => r.text));
    const ordered = (await c.runAndReadAll("SELECT i FROM t ORDER BY iv, i"))
      .getRowObjectsJson()
      .map((r) => Number(r.i));
    expect(
      [...intervals.keys()].sort(
        (a, b) => compareIntervals(intervals[a], intervals[b]) || a - b,
      ),
    ).toEqual(ordered);
    const pairs = (
      await c.runAndReadAll(
        "SELECT a.i AS x, b.i AS y, a.iv = b.iv AS eq, a.iv < b.iv AS lt FROM t a, t b WHERE a.i % 7 = 0 AND b.i % 7 = 0",
      )
    ).getRowObjectsJson();
    const disagree = pairs.filter((p) => {
      const d = compareIntervals(
        intervals[Number(p.x)],
        intervals[Number(p.y)],
      );
      return (d === 0) !== p.eq || d < 0 !== p.lt;
    });
    expect(disagree).toEqual([]);
  } finally {
    c.closeSync();
  }
});
