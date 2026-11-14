import { describe, expect, it } from "vitest";
import { workflowDemo } from "../../mock/fixtures/workflow-demo";
import {
  insights,
  normalize,
  totalsOf,
  type InsightRun,
} from "../../mock/routes/workflow-insights";
import {
  MIN_SAMPLE,
  backedShare,
  callsPerRun,
  claimsOf,
  funnelSteps,
  funnelText,
  helpedShare,
  insightChange,
  insightSeries,
  isRunFilter,
  issueKey,
  metricText,
  principalKey,
  rollingDays,
  runBucket,
  runShown,
  starterLabel,
  starterOf,
  verdictDay,
  verdictParts,
  verdictRuns,
  windowStart,
} from "@/web/areas/workflows/model";

// What a window of runs comes to (the mock's GetFoundryWorkflowInsights, the
// backend's rules) and what the Workflows pages make of it.

const NOW = new Date("2026-11-12T15:00:00Z");

let n = 0;
function run(
  at: string,
  o: {
    workflowId?: string;
    verdict?: string;
    open?: boolean;
    calls?: number;
    claims?: string[];
    gaps?: { kind: string; what: string }[];
    suggestions?: string[];
    key?: string;
    status?: string;
  } = {},
): InsightRun {
  n++;
  const verdict = o.verdict;
  return {
    id: `r${String(n).padStart(3, "0")}`,
    workflowId: o.workflowId || "w1",
    status: o.open ? "open" : "finished",
    startedAt: at,
    ...(o.key
      ? { startedByServiceCredentialId: o.key }
      : { startedByUserId: "u1" }),
    counts: { accesses: o.calls ?? 1, reads: o.calls ?? 1 },
    ...((verdict || o.status) && {
      assessment: {
        status: o.status || "done",
        ...(verdict && { verdict }),
        usdMicros: "1000",
        ...(verdict && {
          assessmentJson: JSON.stringify({
            verdict,
            summary: "s",
            claims: (o.claims || []).map((support) => ({
              text: "c",
              support,
              calls: [1],
            })),
            gaps: (o.gaps || []).map((g) => ({ ...g, calls: [1] })),
            suggestions: o.suggestions || [],
          }),
        }),
      },
    }),
  };
}

describe("workflow insights", () => {
  it("normalizes a gap's text: case, spacing and end punctuation aside", () => {
    expect(normalize("  No HEALTH   score on Customer. ")).toBe(
      "no health score on customer",
    );
    expect(normalize("Add x!?")).toBe("add x");
    expect(normalize("...")).toBe("");
  });

  it("counts a window of whole UTC days ending today, and the window before", () => {
    const runs = [
      run("2026-11-06T00:00:00Z", { verdict: "essential" }), // the first instant of a 7-day window
      run("2026-11-05T23:59:59Z", { verdict: "unused", calls: 0 }), // the window before
      run("2026-11-12T14:00:00Z", { open: true }),
      run("2026-10-29T23:00:00Z", { verdict: "helpful" }), // before both
    ];
    const got = insights(runs, { days: 7, now: NOW });
    expect(got.from).toBe("2026-11-06");
    expect(got.series?.map((d) => d.date)).toEqual([
      "2026-11-06",
      "2026-11-07",
      "2026-11-08",
      "2026-11-09",
      "2026-11-10",
      "2026-11-11",
      "2026-11-12",
    ]);
    expect(got.totals).toMatchObject({
      runs: 2,
      open: 1,
      finished: 1,
      assessed: 1,
      helped: 1,
    });
    expect(got.previous).toMatchObject({
      runs: 1,
      assessed: 1,
      helped: 0,
      used: 0,
    });
    expect(got.series?.[0]?.totals?.runs).toBe(1);
    expect(got.series?.[6]?.totals?.open).toBe(1);
  });

  it("follows runs from start to the ontology helping, each step of the one before", () => {
    const at = "2026-11-12T10:00:00Z";
    const runs = [
      run(at, { open: true }),
      run(at, { status: "queued" }),
      run(at, { verdict: "unused", calls: 0 }),
      run(at, { verdict: "marginal" }),
      run(at, { verdict: "essential" }),
      run(at, { verdict: "helpful", key: "k1" }),
    ];
    const got = insights(runs, { days: 1, now: NOW });
    expect(got.funnel).toEqual({
      started: 6,
      finished: 5,
      assessed: 4,
      used: 3,
      helped: 2,
    });
    const steps = funnelSteps(got.funnel);
    expect(steps.map((s) => s.runs)).toEqual([6, 5, 4, 3, 2]);
    expect(steps[3]?.left).toEqual({
      runs: 1,
      share: 0.25,
      why: "never called it",
    });
    expect(got.agents?.map((a) => a.principal)).toEqual([
      { kind: "KIND_USER", id: "u1" },
      { kind: "KIND_SERVICE_CREDENTIAL", id: "k1" },
    ]);
  });

  it("counts claims, gaps by kind once a run, and what assessments cost", () => {
    const t = totalsOf([
      run("2026-11-12T10:00:00Z", {
        verdict: "helpful",
        claims: [
          "supported",
          "supported",
          "unsupported",
          "contradicted",
          "odd",
        ],
        gaps: [
          { kind: "missing_link", what: "a" },
          { kind: "missing_link", what: "b" },
        ],
      }),
      run("2026-11-12T10:00:00Z", {
        verdict: "marginal",
        gaps: [{ kind: "", what: "c" }],
      }),
    ]);
    expect(t).toMatchObject({
      claimsSupported: 2,
      claimsUnsupported: 1,
      claimsContradicted: 1,
      assessmentUsdMicros: "2000",
    });
    expect(t.gapKinds).toEqual([
      { kind: "missing_link", runs: 1 },
      { kind: "other", runs: 1 },
    ]);
    expect(t.verdicts).toEqual([
      { verdict: "helpful", runs: "1" },
      { verdict: "marginal", runs: "1" },
    ]);
  });

  it("groups recurring gaps and suggestions by their text, newest wording first", () => {
    const runs = [
      run("2026-11-10T10:00:00Z", {
        verdict: "marginal",
        workflowId: "w2",
        gaps: [{ kind: "missing_property", what: "No health score." }],
        suggestions: ["Add a score"],
      }),
      run("2026-11-11T10:00:00Z", {
        verdict: "marginal",
        gaps: [{ kind: "missing_data", what: "no health  score" }],
        suggestions: ["add a score."],
      }),
      run("2026-11-11T11:00:00Z", {
        verdict: "unused",
        gaps: [{ kind: "other", what: "Something else" }],
      }),
    ];
    const got = insights(runs, { days: 7, now: NOW });
    expect(got.gaps?.[0]).toMatchObject({
      kind: "missing_data",
      text: "no health  score",
      runs: 2,
      lastRunId: runs[1]!.id,
      workflowIds: ["w1", "w2"],
    });
    expect(got.suggestions?.[0]).toMatchObject({
      text: "add a score.",
      runs: 2,
    });
    expect(
      got.workflows?.map((w) => [w.workflowId, w.totals?.runs, w.topGap]),
    ).toEqual([
      ["w1", 2, "Something else"],
      ["w2", 1, "No health score."],
    ]);
    expect(
      insights(runs, { days: 7, now: NOW, workflowId: "w1" }).workflows,
    ).toEqual([]);
  });

  it("keeps the ten issues most runs named", () => {
    const runs = Array.from({ length: 12 }, (_, i) =>
      run("2026-11-12T10:00:00Z", {
        verdict: "marginal",
        gaps: Array.from({ length: i + 1 }, (_, j) => ({
          kind: "other",
          what: `gap ${j}`,
        })),
      }),
    );
    const gaps = insights(runs, { days: 1, now: NOW }).gaps || [];
    expect(gaps).toHaveLength(10);
    expect(gaps[0]).toMatchObject({ text: "gap 0", runs: 12 });
    expect(gaps[9]).toMatchObject({ text: "gap 9", runs: 3 });
  });

  it("seeds a demo the pages have trends to show from", () => {
    const demo = workflowDemo(NOW);
    expect(demo.workflows).toHaveLength(3);
    expect(demo.runs.length).toBeGreaterThan(100);
    expect(
      demo.runs.every((r) => Date.parse(r.startedAt) < NOW.getTime()),
    ).toBe(true);
    expect(workflowDemo(NOW)).toEqual(demo);
  });
});

describe("the Workflows pages' metrics", () => {
  const t = {
    runs: 10,
    assessed: 8,
    helped: 6,
    claimsSupported: 9,
    claimsUnsupported: 2,
    claimsContradicted: 1,
    reads: 20,
    questions: 5,
    verdicts: [
      { verdict: "essential", runs: "4" },
      { verdict: "helpful", runs: "2" },
      { verdict: "unused", runs: "2" },
    ],
  };
  it("reads shares and rates, none with nothing to share", () => {
    expect(helpedShare(t)).toBe(0.75);
    expect(backedShare(t)).toBe(0.75);
    expect(callsPerRun(t)).toBe(2.5);
    expect(helpedShare({})).toBeUndefined();
    expect(backedShare({})).toBeUndefined();
    expect(callsPerRun({})).toBeUndefined();
  });

  it("reads a rate of few runs as the counts it comes from", () => {
    expect(metricText("helped", { assessed: 1, helped: 1 })).toEqual({
      text: "1 of 1",
      few: true,
    });
    expect(
      metricText("backed", {
        assessed: 1,
        claimsSupported: 10,
        claimsUnsupported: 1,
      }),
    ).toEqual({ text: "10 of 11", few: true });
    expect(metricText("helped", { assessed: 20, helped: 15 })).toEqual({
      text: "75%",
      few: false,
    });
    expect(metricText("helped", {})).toEqual({ text: "—", few: false });
    expect(metricText("runs", { runs: 1200 })).toEqual({
      text: "1,200",
      few: false,
    });
    expect(metricText("misleading", { runs: 3 })).toEqual({
      text: "—",
      few: false,
    });
    expect(metricText("misleading", t)).toEqual({ text: "0", few: false });
    expect(claimsOf(t)).toBe(12);
    expect(verdictRuns(t, "essential")).toBe(4);
    expect(verdictRuns(undefined, "essential")).toBe(0);
  });

  it("says how a metric moved from the window before, and whether that is good", () => {
    const many = { runs: 20, assessed: 20, helped: 16 };
    expect(
      insightChange("helped", many, { runs: 10, assessed: 10, helped: 6 }),
    ).toEqual({ text: "▲ 20 pts", tone: "good" });
    // A rate of fewer than MIN_SAMPLE runs on either side moves nothing.
    expect(
      insightChange("helped", many, { assessed: MIN_SAMPLE - 1, helped: 1 }),
    ).toBeUndefined();
    expect(insightChange("helped", t, many)).toBeUndefined();
    expect(insightChange("runs", t, { runs: 20 })).toEqual({
      text: "▼ 10",
      tone: "bad",
    });
    expect(insightChange("runs", t, { runs: 10 })).toEqual({
      text: "No change",
      tone: "flat",
    });
    // Fewer misleading runs is good news.
    const misled = (n: number) => ({
      assessed: 5,
      verdicts: [{ verdict: "misleading", runs: String(n) }],
    });
    expect(insightChange("misleading", misled(1), misled(3))).toEqual({
      text: "▼ 2",
      tone: "good",
    });
    expect(insightChange("misleading", misled(2), misled(0))).toEqual({
      text: "▲ 2",
      tone: "bad",
    });
    expect(insightChange("misleading", misled(1), {})).toBeUndefined();
    expect(insightChange("backed", t, {})).toBeUndefined();
  });

  it("reads a rate over the days before each, so a day of few runs doesn't swing it", () => {
    const days = [
      { date: "d1", totals: { runs: 1, assessed: 1, helped: 1 } },
      { date: "d2", totals: { runs: 1, assessed: 1 } },
      { date: "d3", totals: {} },
      { date: "d4", totals: { runs: 2, assessed: 2, helped: 2 } },
    ];
    expect(insightSeries("helped", days, 2).map((d) => d.value)).toEqual([
      1, 0.5, 0, 1,
    ]);
    expect(insightSeries("helped", days).map((d) => d.value)).toEqual([
      1,
      0,
      undefined,
      1,
    ]);
    // Counts are each day's own.
    expect(insightSeries("runs", days, 7).map((d) => d.value)).toEqual([
      1, 1, 0, 2,
    ]);
    expect(insightSeries("helped", undefined)).toEqual([]);
    expect(rollingDays(7)).toBe(1);
    expect(rollingDays(30)).toBe(7);
  });

  it("says in a sentence where runs went when none left on the way", () => {
    expect(
      funnelText({ started: 1, finished: 1, assessed: 1, used: 1, helped: 1 }),
    ).toBe(
      "The run finished, was assessed and used the ontology, and the ontology helped.",
    );
    expect(
      funnelText({ started: 3, finished: 3, assessed: 3, used: 3, helped: 3 }),
    ).toMatch(/^Every run finished/);
    expect(
      funnelText({ started: 3, finished: 3, assessed: 3, used: 2, helped: 2 }),
    ).toBeUndefined();
    expect(funnelText({})).toBeUndefined();
  });

  it("charts runs by verdict and the shares by day", () => {
    expect(verdictDay({ date: "2026-11-12", totals: t })).toMatchObject({
      date: "2026-11-12",
      essential: 4,
      helpful: 2,
      marginal: 0,
      unused: 2,
      pending: 2,
    });
    expect(
      insightSeries("helped", [
        { date: "d1", totals: t },
        { date: "d2", totals: {} },
      ]),
    ).toEqual([
      { date: "d1", value: 0.75 },
      { date: "d2", value: undefined },
    ]);
    expect(verdictParts(t).map((p) => [p.verdict, p.share])).toEqual([
      ["essential", 0.5],
      ["helpful", 0.25],
      ["unused", 0.25],
    ]);
  });

  it("names who started runs", () => {
    const names = new Map([["k1", "Support agent"]]);
    expect(
      starterLabel({ kind: "KIND_SERVICE_CREDENTIAL", id: "k1" }, names, "u1"),
    ).toBe("Support agent");
    expect(
      starterLabel({ kind: "KIND_SERVICE_CREDENTIAL", id: "k2" }, names, "u1"),
    ).toBe("An agent’s key");
    expect(starterLabel({ kind: "KIND_USER", id: "u1" }, names, "u1")).toBe(
      "You",
    );
    expect(starterLabel({ kind: "KIND_USER", id: "u2" }, names, "u1")).toBe(
      "A member",
    );
    expect(starterLabel(undefined, names, "u1")).toBe("Someone");
  });
});

describe("a workflow's runs list", () => {
  const at = "2026-11-12T10:00:00Z";
  it("files each run under one filter", () => {
    expect(runBucket(run(at, { open: true }))).toBe("pending");
    expect(runBucket(run(at, { status: "queued" }))).toBe("pending");
    expect(runBucket(run(at, { status: "failed" }))).toBe("pending");
    expect(runBucket(run(at, { verdict: "later" }))).toBe("pending");
    expect(runBucket(run(at, { verdict: "essential" }))).toBe("helped");
    expect(runBucket(run(at, { verdict: "helpful" }))).toBe("helped");
    expect(runBucket(run(at, { verdict: "marginal" }))).toBe("not_helped");
    expect(runBucket(run(at, { verdict: "unused" }))).toBe("not_helped");
    expect(runBucket(run(at, { verdict: "misleading" }))).toBe("misleading");
    expect(isRunFilter("misleading")).toBe(true);
    expect(isRunFilter("toString")).toBe(false);
    expect(isRunFilter(null)).toBe(false);
  });

  it("starts a window on its first whole UTC day, as the insights count", () => {
    expect(new Date(windowStart(7, NOW)).toISOString()).toBe(
      "2026-11-06T00:00:00.000Z",
    );
    expect(new Date(windowStart(1, NOW)).toISOString()).toBe(
      "2026-11-12T00:00:00.000Z",
    );
  });

  it("matches a gap's text as the backend does", () => {
    expect(issueKey("  No HEALTH   score on Customer. ")).toBe(
      normalize("  No HEALTH   score on Customer. "),
    );
    expect(issueKey("Add x!?")).toBe("add x");
  });

  it("shows the runs of the window, who, filter and gap asked for", () => {
    const from = windowStart(7, NOW);
    const named = run(at, {
      verdict: "misleading",
      key: "k1",
      gaps: [{ kind: "missing_property", what: "No health score." }],
      suggestions: ["Add a health_score"],
    });
    const plain = run(at, { verdict: "helpful" });
    const old = run("2026-11-01T10:00:00Z", { verdict: "helpful" });
    const q = (o: Partial<Parameters<typeof runShown>[1]>) => ({
      from,
      filter: "all" as const,
      ...o,
    });
    expect(runShown(named, q({}))).toBe(true);
    expect(runShown(old, q({}))).toBe(false);
    expect(runShown({ ...plain, startedAt: undefined }, q({}))).toBe(false);
    expect(runShown(named, q({ filter: "misleading" }))).toBe(true);
    expect(runShown(plain, q({ filter: "misleading" }))).toBe(false);
    const k1 = { kind: "KIND_SERVICE_CREDENTIAL" as const, id: "k1" };
    expect(runShown(named, q({ who: k1 }))).toBe(true);
    expect(runShown(plain, q({ who: k1 }))).toBe(false);
    expect(runShown(named, q({ issue: "no health score" }))).toBe(true);
    expect(runShown(named, q({ issue: "add a health_score" }))).toBe(true);
    expect(runShown(plain, q({ issue: "no health score" }))).toBe(false);
    expect(starterOf(named)).toEqual(k1);
    expect(starterOf(plain)).toEqual({ kind: "KIND_USER", id: "u1" });
    expect(principalKey(k1)).toBe("KIND_SERVICE_CREDENTIAL:k1");
    expect(principalKey(undefined)).toBe("");
  });
});
