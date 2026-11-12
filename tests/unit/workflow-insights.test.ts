import { describe, expect, it } from "vitest";
import { workflowDemo } from "../../mock/fixtures/workflow-demo";
import {
  insights,
  normalize,
  totalsOf,
  type InsightRun,
} from "../../mock/routes/workflow-insights";
import {
  backedShare,
  callsPerRun,
  funnelSteps,
  helpedShare,
  insightChange,
  insightSeries,
  starterLabel,
  verdictDay,
  verdictParts,
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

  it("says how a metric moved from the window before", () => {
    expect(insightChange("helped", t, { helped: 3, assessed: 6 })).toEqual({
      text: "▲ 25 pts",
      direction: "up",
    });
    expect(insightChange("runs", t, { runs: 20 })).toEqual({
      text: "▼ 50%",
      direction: "down",
    });
    expect(insightChange("calls", t, { runs: 10, reads: 25 })).toEqual({
      text: "No change",
      direction: "flat",
    });
    expect(insightChange("runs", t, { runs: 0 })).toBeUndefined();
    expect(insightChange("backed", t, {})).toBeUndefined();
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
