import { it, expect, vi, afterEach } from "vitest";
import {
  formatNumber,
  compareValues,
  columnKind,
} from "../apps/web/src/kit/util.js";
afterEach(() => vi.unstubAllGlobals());
it("classifies fixed-size arrays before their scalar element types", () => {
  for (const t of ["INTEGER[3]", "DOUBLE[4]", "TIMESTAMP[2]", "VARCHAR[2][3]"])
    expect(columnKind(t)).toBe("json");
  expect(columnKind("INTEGER")).toBe("integer");
});
