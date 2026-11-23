import { describe, expect, it } from "vitest";
import {
  NUDGE_GROWTH,
  nudgeLater,
  nudgeShown,
  parseNudgeState,
} from "@/web/areas/reviews/nudge-model";

// The sidebar's review nudge (reviews/nudge-model.ts): which current load it
// points at, and what Later hides.

const s = (ontologyId: string, open: number, more = {}) => ({
  ontologyId,
  loadId: `L-${ontologyId}`,
  open,
  ...more,
});

describe("nudgeShown", () => {
  it("points at the load with the most cards to decide", () => {
    expect(nudgeShown({}, [s("a", 3), s("b", 40), s("c", 0)])?.ontologyId).toBe(
      "b",
    );
    expect(nudgeShown({}, [s("b", 4), s("a", 4)])?.ontologyId).toBe("a");
    expect(nudgeShown({}, [s("a", 0), s("b", 0)])).toBeUndefined();
    expect(nudgeShown({}, [])).toBeUndefined();
  });
  it("shows a load still being counted", () => {
    expect(nudgeShown({}, [s("a", 0, { indexing: true })])?.ontologyId).toBe(
      "a",
    );
  });
  it("keeps a load Later hid hidden until its count grows by a quarter", () => {
    const later = nudgeLater({}, s("a", 40));
    expect(later).toEqual({ a: { loadId: "L-a", open: 40 } });
    expect(nudgeShown(later, [s("a", 40)])).toBeUndefined();
    expect(nudgeShown(later, [s("a", 49)])).toBeUndefined();
    expect(nudgeShown(later, [s("a", 40 * NUDGE_GROWTH)])?.ontologyId).toBe(
      "a",
    );
    // The next one down shows meanwhile.
    expect(nudgeShown(later, [s("a", 40), s("b", 2)])?.ontologyId).toBe("b");
    // A new load shows again.
    expect(
      nudgeShown(later, [{ ...s("a", 10), loadId: "L-new" }])?.ontologyId,
    ).toBe("a");
    // Later while counting: back once there is one to decide.
    const counting = nudgeLater({}, s("a", 0, { indexing: true }));
    expect(
      nudgeShown(counting, [s("a", 0, { indexing: true })]),
    ).toBeUndefined();
    expect(nudgeShown(counting, [s("a", 1)])?.ontologyId).toBe("a");
  });
});

describe("parseNudgeState", () => {
  it("reads what Later kept, and nothing else", () => {
    expect(parseNudgeState('{"a":{"loadId":"L","open":4}}')).toEqual({
      a: { loadId: "L", open: 4 },
    });
    expect(
      parseNudgeState(
        '{"a":{"loadId":"L"},"b":null,"c":{"loadId":"M","open":1}}',
      ),
    ).toEqual({
      c: { loadId: "M", open: 1 },
    });
    for (const raw of ["", "oops", "[]", "null", "3"])
      expect(parseNudgeState(raw)).toEqual({});
  });
});
