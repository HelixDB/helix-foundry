import { it, expect, vi, afterEach } from "vitest";
import {
  formatNumber,
  compareValues,
  columnKind,
} from "../apps/web/src/kit/util.js";
afterEach(() => vi.unstubAllGlobals());
it("keeps neighboring 64-bit identifiers distinct", () => {
  expect(formatNumber(9007199254740993n)).toBe("9,007,199,254,740,993");
  expect(formatNumber("9007199254740993")).toBe("9,007,199,254,740,993");
  expect(
    compareValues("9007199254740992", "9007199254740993", "integer"),
  ).toBeLessThan(0);
  expect(
    compareValues(9007199254740993n, 9007199254740992n, "integer"),
  ).toBeGreaterThan(0);
});

it("orders mixed integer representations transitively", () => {
  const a = "9007199254740992",
    b = 9007199254740992,
    c = "9007199254740993";
  expect(compareValues(a, b, "integer")).toBe(0);
  expect(compareValues(b, c, "integer")).toBeLessThan(0);
  expect(compareValues(a, c, "integer")).toBeLessThan(0);
});
