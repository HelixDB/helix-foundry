import { it, expect } from "vitest";
import { posthogObjects } from "../apps/api/src/posthog.js";
it("imports serialized null property payloads without crashing row normalization", () => {
  for (const name of ["persons", "groups", "events"]) {
    const obj = posthogObjects.find((o) => o.name === name)!;
    const values =
      name === "persons"
        ? ["2026-01-01", true, "null", "p1"]
        : name === "groups"
          ? ["2026-01-01", "null", 0, "g1"]
          : ["signup", "2026-01-01", "d1", "p1", "null", "e1"];
    expect(obj.row(values, new Map())).toMatchObject({ properties: {} });
  }
});
