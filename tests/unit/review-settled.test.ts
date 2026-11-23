import { describe, expect, it, vi } from "vitest";
import {
  announceSettled,
  onSettled,
  settledDecision,
} from "@/web/areas/reviews/settled";

// Reviews settled from Founder's dock, which the walk takes as the person's
// own (reviews/settled.ts).

describe("settledDecision", () => {
  it("reads a settle_review result's decision as the WFE writes one", () => {
    expect(
      settledDecision(
        '{"result":{"id":"d1","kind":"release","doc_id":"doc","decided_by_user_id":"u","claim_section":2}}',
      ),
    ).toEqual({
      id: "d1",
      kind: "release",
      docId: "doc",
      decidedByUserId: "u",
      claimSection: 2,
    });
  });
  it("is nothing for anything else", () => {
    for (const raw of [
      undefined,
      "",
      "{",
      "[]",
      '{"result":1}',
      '{"result":{"kind":"release"}}',
      '{"result":{"id":"d","kind":"withdraw"}}',
    ])
      expect(settledDecision(raw)).toBeUndefined();
  });
});

describe("announceSettled", () => {
  it("tells each listener until it stops listening", () => {
    const heard = vi.fn();
    const stop = onSettled(heard);
    const s = {
      workspaceId: "w",
      ontologyId: "o",
      reviewId: "r",
      decision: { id: "d" } as never,
    };
    announceSettled(s);
    stop();
    announceSettled(s);
    expect(heard).toHaveBeenCalledTimes(1);
    expect(heard).toHaveBeenCalledWith(s);
  });
});
