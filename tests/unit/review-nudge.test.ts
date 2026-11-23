import { describe, expect, it } from "vitest";
import type { FoundryLoad } from "@/shared/wfe";
import {
  NUDGE_GROWTH,
  dockOffersReview,
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
  it("reads a count the WFE left out as none", () => {
    const counting = { ontologyId: "a", loadId: "L-a", indexing: true };
    expect(
      nudgeShown({}, [{ ontologyId: "z", loadId: "L-z" }]),
    ).toBeUndefined();
    expect(nudgeShown({}, [counting, s("b", 1)])?.ontologyId).toBe("b");
    expect(
      nudgeShown({}, [s("b", 0, { indexing: true }), counting])?.ontologyId,
    ).toBe("a");
    expect(nudgeLater({}, counting)).toEqual({ a: { loadId: "L-a", open: 0 } });
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

describe("dockOffersReview", () => {
  const load = (more: Partial<FoundryLoad> = {}): FoundryLoad => ({
    id: "L-a",
    status: "ready",
    ...more,
  });
  it("is true only while the dock's card of the summary's load offers Review", () => {
    expect(dockOffersReview(load(), s("a", 7))).toBe(true);
    expect(dockOffersReview(load({ status: "partial" }), s("a", 7))).toBe(true);
    expect(
      dockOffersReview(load(), {
        ontologyId: "a",
        loadId: "L-a",
        indexing: true,
      }),
    ).toBe(true);
  });
  it("is false while a job runs on the load, or it isn't ready, or not read, or another's", () => {
    // A resend or an extract on the load: the card shows its progress.
    for (const status of ["queued", "running"] as const)
      expect(
        dockOffersReview(load({ jobs: [{ id: "j", status }] }), s("a", 7)),
      ).toBe(false);
    expect(dockOffersReview(load({ status: "writing" }), s("a", 7))).toBe(
      false,
    );
    expect(dockOffersReview(load({ status: "failed" }), s("a", 7))).toBe(false);
    expect(dockOffersReview(undefined, s("a", 7))).toBe(false);
    expect(dockOffersReview(load({ id: "L-old" }), s("a", 7))).toBe(false);
    // Nothing to decide: the card offers Explore it.
    expect(dockOffersReview(load(), s("a", 0))).toBe(false);
  });
});
