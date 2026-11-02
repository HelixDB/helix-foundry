import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "@/web/api";
import { jobSeen, jobStarted, trackEvent } from "@/web/analytics";
import { connectorsQuery, syncConnector } from "@/web/areas/data/data-api";
import {
  documentQueries,
  moveDocument,
} from "@/web/areas/documents/documents-api";
import {
  askQuestion,
  cancelQuestion,
  createLoad,
  generateOntology,
  reviseOntologyVersion,
  setCurrentLoad,
  setCurrentVersion,
} from "@/web/areas/ontology/queries";
import { deleteReviewDecision, settleReview } from "@/web/areas/reviews/api";
import { cancelPlan, resumePlan } from "@/web/areas/workspace/plan";

// The API calls that report a custom event (src/web/analytics.ts): once the
// WFE accepts them, and never when it refuses; those that start a job start
// it (jobStarted) or report why the WFE refused it.

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
  call.mockReset().mockResolvedValue({
    generation: { id: "gen_1" },
    load: { id: "load_1" },
    question: { id: "q_1" },
    sync: { id: "sync_1" },
  });
  vi.mocked(trackEvent).mockClear();
  vi.mocked(jobStarted).mockClear();
  vi.mocked(jobSeen).mockClear();
});

const cases: [string, () => Promise<unknown>, unknown[]][] = [
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

  const jobs: [string, () => Promise<unknown>, unknown[], string | null][] = [
    [
      "a sync, with where it was queued",
      () => syncConnector("ws", "c1", "tree"),
      [{ job: "sync", from: "tree" }, "sync_1"],
      null,
    ],
    [
      "a generation, with its source",
      () => generateOntology("ws", "o", { sample: "acme" }),
      [{ job: "generation", source: "sample" }, "gen_1"],
      "generation_refused",
    ],
    [
      "a revision's preview",
      () => reviseOntologyVersion("ws", "o", "v", { edits: [], dryRun: true }),
      [{ job: "revision", mode: "preview" }, "gen_1"],
      "revision_refused",
    ],
    [
      "a revision that saves",
      () => reviseOntologyVersion("ws", "o", "v", { edits: [] }),
      [{ job: "revision", mode: "save" }, "gen_1"],
      "revision_refused",
    ],
    [
      "a load",
      () => createLoad("ws", "o", { versionId: "v", clientToken: "t" }),
      [{ job: "load" }, "load_1"],
      "load_refused",
    ],
    [
      "a question",
      () =>
        askQuestion("ws", "o", {
          loadId: "l",
          question: "?",
          clientToken: "t",
        }),
      [{ job: "question" }, "q_1"],
      "question_refused",
    ],
  ];

  it.each(jobs)("starts %s once it is accepted", async (_, run, started) => {
    await run();
    expect(vi.mocked(jobStarted).mock.calls).toEqual([started]);
    expect(trackEvent).not.toHaveBeenCalled();
  });

  it.each(jobs)(
    "reports why %s was refused, and starts nothing",
    async (_, run, __, refused) => {
      call.mockRejectedValueOnce(new ApiError("slow down", 429));
      await expect(run()).rejects.toThrow("slow down");
      call.mockRejectedValueOnce(new ApiError("over the cap", 400));
      await expect(run()).rejects.toThrow("over the cap");
      call.mockRejectedValueOnce(new TypeError("Failed to fetch"));
      await expect(run()).rejects.toThrow("Failed to fetch");
      expect(jobStarted).not.toHaveBeenCalled();
      expect(vi.mocked(trackEvent).mock.calls).toEqual(
        refused
          ? [
              [refused, { reason: "rate_limit" }],
              [refused, { reason: "refused" }],
              [refused, { reason: "error" }],
            ]
          : [],
      );
    },
  );

  it("leaves a generation already live unreported: callers follow it", async () => {
    call.mockRejectedValueOnce(new ApiError("already live", 409));
    await expect(generateOntology("ws", "o", {})).rejects.toThrow();
    expect(trackEvent).not.toHaveBeenCalled();
  });
});

describe("reads that see jobs end", () => {
  it("hands each connector's newest sync to jobSeen, with its kind", async () => {
    call.mockResolvedValueOnce({
      connectors: [
        {
          id: "c1",
          kind: "stripe",
          lastSync: { id: "s1", status: "CONNECTOR_SYNC_STATUS_FAILED" },
        },
        { id: "c2", kind: "newkind", lastSync: { id: "s2" } },
        { id: "c3", kind: "github" },
      ],
    });
    await connectorsQuery("ws").queryFn();
    expect(vi.mocked(jobSeen).mock.calls).toEqual([
      [
        "sync",
        [
          { id: "s1", status: "CONNECTOR_SYNC_STATUS_FAILED", kind: "stripe" },
          { id: "s2", kind: "other" },
          undefined,
        ],
      ],
    ]);
  });

  it("hands each upload's read to jobSeen, with its format and code", async () => {
    const pdf = {
      id: "d1",
      mediaType: "application/pdf",
      read: {
        state: "FOUNDRY_FILE_READ_STATE_FAILED",
        error: { code: "unsupported", message: "Scanned PDF: no text to read" },
      },
    };
    call.mockResolvedValueOnce({ documents: [pdf, { id: "d2" }] });
    await documentQueries.workspace("ws").queryFn();
    call.mockResolvedValueOnce({ document: pdf });
    await documentQueries.workspaceDocument("ws", "d1").queryFn();
    const read = {
      id: "d1",
      status: "FOUNDRY_FILE_READ_STATE_FAILED",
      format: "application/pdf",
      code: "unsupported",
    };
    expect(vi.mocked(jobSeen).mock.calls).toEqual([
      ["read", [read, { id: "d2" }]],
      ["read", [read]],
    ]);
  });
});
