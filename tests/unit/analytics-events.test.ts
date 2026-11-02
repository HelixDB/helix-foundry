import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "@/web/api";
import { jobStarted, trackEvent } from "@/web/analytics";
import { syncConnector } from "@/web/areas/data/data-api";
import { moveDocument } from "@/web/areas/documents/documents-api";
import {
  cancelQuestion,
  reviseOntologyVersion,
  setCurrentLoad,
  setCurrentVersion,
} from "@/web/areas/ontology/queries";
import { deleteReviewDecision, settleReview } from "@/web/areas/reviews/api";
import { cancelPlan, resumePlan } from "@/web/areas/workspace/plan";

// The API calls that report a custom event (src/web/analytics.ts): once the
// WFE accepts them, and never when it refuses.

vi.mock("@/web/api", async (actual) => ({
  ...(await actual<typeof import("@/web/api")>()),
  api: vi.fn(),
}));
vi.mock("@/web/analytics", () => ({
  jobSeen: vi.fn(),
  jobStarted: vi.fn(),
  trackEvent: vi.fn(),
  trackBeforeLeaving: vi.fn(),
}));
const call = vi.mocked(api);

beforeEach(() => {
  call.mockReset().mockResolvedValue({ generation: { id: "gen_1" } });
  vi.mocked(trackEvent).mockClear();
  vi.mocked(jobStarted).mockClear();
});

const cases: [string, () => Promise<unknown>, unknown[]][] = [
  [
    "a sync, with where it was queued",
    () => syncConnector("ws", "c1", "tree"),
    ["connector_sync_started", { from: "tree" }],
  ],
  ["a document move", () => moveDocument("ws", "d1", "s1"), ["document_moved"]],
  [
    "a version made current",
    () => setCurrentVersion("ws", "o", { versionId: "v2" }),
    ["version_made_current"],
  ],
  [
    "a load made current",
    () => setCurrentLoad("ws", "o", "l1"),
    ["load_made_current"],
  ],
  [
    "a cancelled question",
    () => cancelQuestion("ws", "o", "q1"),
    ["question_cancelled"],
  ],
  [
    "a settled resolve review",
    () =>
      settleReview("ws", "o", "r1", {
        decision: { resolve: { notInData: true } },
      }),
    ["review_settled", { kind: "resolve" }],
  ],
  [
    "a settled property review",
    () =>
      settleReview("ws", "o", "r1", { decision: { prop: { boolean: false } } }),
    ["review_settled", { kind: "prop" }],
  ],
  [
    "a removed decision",
    () => deleteReviewDecision("ws", "o", "d1"),
    ["review_decision_removed"],
  ],
  ["a cancelled plan", () => cancelPlan("ws"), ["plan_cancelled"]],
  ["a resumed plan", () => resumePlan("ws"), ["plan_resumed"]],
];

describe("custom events of API calls", () => {
  it.each(cases)("reports %s once it is accepted", async (_, run, event) => {
    await run();
    expect(vi.mocked(trackEvent).mock.calls).toEqual([event]);
  });

  it.each(cases)("reports nothing when %s is refused", async (_, run) => {
    call.mockRejectedValueOnce(new Error("refused"));
    await expect(run()).rejects.toThrow("refused");
    expect(trackEvent).not.toHaveBeenCalled();
  });

  it("starts a revision job as a preview or a save", async () => {
    await reviseOntologyVersion("ws", "o", "v", { edits: [], dryRun: true });
    await reviseOntologyVersion("ws", "o", "v", { edits: [] });
    expect(vi.mocked(jobStarted).mock.calls).toEqual([
      ["revision", "gen_1", "preview"],
      ["revision", "gen_1", "save"],
    ]);
    call.mockRejectedValueOnce(new Error("refused"));
    await expect(
      reviseOntologyVersion("ws", "o", "v", { edits: [] }),
    ).rejects.toThrow("refused");
    expect(jobStarted).toHaveBeenCalledTimes(2);
  });
});
