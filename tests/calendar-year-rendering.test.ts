import { DuckDBInstance } from "@duckdb/node-api";
import { expect, it } from "vitest";
import { formatDate } from "../apps/web/src/kit/util.js";

it("renders actual DuckDB calendar dates without adding 1900 to early years", async () => {
  const db = await DuckDBInstance.create(":memory:");
  const connection = await db.connect();
  try {
    const rows = (
      await connection.runAndReadAll(
        "SELECT d FROM (VALUES (DATE '0001-03-14'), (DATE '0042-03-14'), (DATE '0099-03-14'), (DATE '2026-03-14')) dates(d)",
      )
    ).getRowObjectsJson();
    for (const row of rows) {
      const value = String(row.d);
      const calendar = new Date(value + "T12:00:00");
      expect(formatDate(value, false)).toBe(
        calendar.toLocaleDateString(undefined, {
          year: "numeric",
          month: "short",
          day: "numeric",
        }),
      );
    }
  } finally {
    connection.closeSync();
    db.closeSync();
  }
});
