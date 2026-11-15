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
import { EMPTY_STREAM } from "@/web/areas/agent/model";
import {
  agentQueries,
  dismissAgentAction,
  executeAgentAction,
  pairAgentHelper,
  revokeAgentHelper,
  sendAgentMessage,
  setAgentRuntime,
  turnEvents,
} from "@/web/areas/agent/queries";
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
    "an approved held claim",
    () =>
      settleReview("ws", "o", "r1", {
        decision: { release: { statementHash: "a".repeat(64) } },
      }),
    ["review_settled", { kind: "release" }],
  ],
  [
    "a rejected held claim",
    () =>
      settleReview("ws", "o", "r1", {
        decision: { reject: { statementHash: "a".repeat(64) } },
      }),
    ["review_settled", { kind: "reject" }],
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

describe("the Assistant's events", () => {
  const action = {
    id: "act_1",
    operation: "create_load",
    payloadHash: "h",
    targetNamedByUser: false,
  };

  it("starts a message's turn from its section, or says why it was refused", async () => {
    call.mockResolvedValueOnce({ turn: { id: "turn_1" }, thread: { id: "t" } });
    await sendAgentMessage("ws", { text: "Hi", clientToken: "c" }, "ask");
    call.mockResolvedValueOnce({
      turn: { id: "turn_2" },
      thread: { id: "t2", runtime: "claude_code" },
    });
    await sendAgentMessage("ws", { text: "Hi", clientToken: "e" }, "data");
    expect(vi.mocked(jobStarted).mock.calls).toEqual([
      [{ job: "agent_turn", section: "ask", runtime: "api" }, "turn_1"],
      [
        { job: "agent_turn", section: "data", runtime: "claude_code" },
        "turn_2",
      ],
    ]);
    call.mockRejectedValueOnce(new ApiError("slow down", 429));
    await expect(
      sendAgentMessage("ws", { text: "Hi", clientToken: "d" }, "none"),
    ).rejects.toThrow();
    expect(vi.mocked(trackEvent).mock.calls).toEqual([
      ["agent_turn_refused", { reason: "rate_limit" }],
    ]);
  });

  it("hands a thread's turns to jobSeen", async () => {
    const turns = [{ id: "turn_1", status: "done", steps: 3 }];
    call.mockResolvedValueOnce({ thread: { id: "t" }, turns });
    await agentQueries.thread("ws", "t").queryFn();
    expect(vi.mocked(jobSeen).mock.calls).toEqual([["agent_turn", turns]]);
  });

  it.each([
    ["succeeded", "succeeded"],
    ["failed", "failed"],
    ["unknown", "unknown"],
  ])("reports a run that %s, with its operation", async (status, outcome) => {
    call.mockResolvedValueOnce({ action: { ...action, status } });
    await executeAgentAction("ws", action);
    expect(vi.mocked(trackEvent).mock.calls).toEqual([
      [
        "agent_action_executed",
        { operation: "create_load", outcome, target_named_by_user: false },
      ],
    ]);
  });

  it("reports where the Assistant runs once chosen, and its helpers paired and revoked, never when refused", async () => {
    call.mockResolvedValueOnce({ runtime: { runtime: "claude_code" } });
    expect(await setAgentRuntime("ws", "claude_code")).toEqual({
      runtime: "claude_code",
    });
    expect(call.mock.calls.at(-1)).toEqual([
      "/v1/workspaces/ws/foundry/agent/runtime",
      { runtime: "claude_code" },
      "PATCH",
    ]);
    await pairAgentHelper("ws");
    await revokeAgentHelper("ws", "01HOST");
    expect(call.mock.calls.at(-1)?.[0]).toBe(
      "/v1/workspaces/ws/foundry/agent/hosts/01HOST:revoke",
    );
    call.mockRejectedValueOnce(new ApiError("no", 400));
    await expect(setAgentRuntime("ws", "codex")).rejects.toThrow();
    expect(vi.mocked(trackEvent).mock.calls).toEqual([
      ["agent_runtime_set", { runtime: "claude_code" }],
      ["agent_helper_pairing_made"],
      ["agent_helper_revoked"],
    ]);
  });

  it("reports a refused run, and an unknown operation as other", async () => {
    call.mockRejectedValueOnce(new ApiError("expired", 400));
    await expect(executeAgentAction("ws", action)).rejects.toThrow();
    call.mockResolvedValueOnce({ action: { ...action, status: "dismissed" } });
    await dismissAgentAction("ws", { ...action, operation: "drop_all" });
    expect(vi.mocked(trackEvent).mock.calls).toEqual([
      ["agent_action_refused", { reason: "refused" }],
      [
        "agent_action_dismissed",
        { operation: "other", target_named_by_user: false },
      ],
    ]);
  });

  it("reports a card shown while its turn runs, never one read after", async () => {
    const card = {
      seq: 1,
      eventJson: JSON.stringify({
        type: "action_card",
        action_id: "a",
        operation: "create_load",
        target_named_by_user: true,
      }),
    };
    call.mockResolvedValueOnce({ events: [card], status: "done", lastSeq: 1 });
    await turnEvents("ws", "turn_1", EMPTY_STREAM);
    expect(trackEvent).not.toHaveBeenCalled();
    call.mockResolvedValueOnce({
      events: [card],
      status: "running",
      lastSeq: 1,
    });
    await turnEvents("ws", "turn_2", EMPTY_STREAM);
    expect(vi.mocked(trackEvent).mock.calls).toEqual([
      [
        "agent_action_prepared",
        { operation: "create_load", target_named_by_user: true },
      ],
    ]);
  });
});
