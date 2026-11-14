import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { GraphJob, OntologyGeneration } from "@/shared/wfe";
import {
  PHASE_SHARE,
  USUAL_DRAFT_MS,
  USUAL_REVISION_MS,
  expectedDuration,
  generationProgress,
  loadProgress,
  progressLine,
  timeLeft,
  timeSoFar,
} from "@/web/areas/ontology/progress";
import {
  MAX_WATCHED,
  WATCH_MS,
  followJob,
  parseWatched,
  readWatched,
  shownOn,
  subscribeWatched,
  unwatchJob,
  watchJob,
  withJob,
  withoutJob,
  type WatchedJob,
} from "@/web/areas/ontology/watched-jobs";

// A job's bar and time left (src/web/areas/ontology/progress.ts), and the
// jobs this browser follows from any page (watched-jobs.ts).

const MIN = 60_000;
const T0 = Date.parse("2026-11-14T10:00:00Z");
const iso = (ms: number) => new Date(ms).toISOString();

describe("a load job's bar", () => {
  const job = (over: Partial<GraphJob>): GraphJob => ({
    id: "job_1",
    kind: "load",
    status: "running",
    phase: "provisioning",
    startedAt: iso(T0),
    ...over,
  });

  it("waits at the start while queued, and is full once it succeeded", () => {
    expect(loadProgress(job({ status: "queued" }), undefined, T0)).toEqual({
      fraction: 0,
      remainingMs: null,
      overdue: false,
      elapsedMs: 0,
      step: "Waiting to start",
    });
    expect(
      loadProgress(job({ status: "succeeded" }), undefined, T0 + MIN),
    ).toMatchObject({ fraction: 1, remainingMs: 0, elapsedMs: MIN });
    // Ended any other way, nothing measures it.
    expect(
      loadProgress(
        job({ status: "failed", phase: "documents" }),
        undefined,
        T0,
      ),
    ).toMatchObject({ fraction: null, remainingMs: null });
  });

  it("moves by phase, the tables' stretch unmeasured", () => {
    expect(loadProgress(job({}), undefined, T0).fraction).toBe(0);
    // The tables have no total: unmeasured.
    const tables = loadProgress(
      job({ phase: "ingest", progress: { chunksDone: 40 } }),
      undefined,
      T0,
    );
    expect(tables).toMatchObject({
      fraction: null,
      remainingMs: null,
      step: "Loading tables",
    });
    // Never full before it succeeded.
    expect(
      loadProgress(job({ phase: "finishing" }), undefined, T0).fraction,
    ).toBeCloseTo(0.9);
    expect(
      loadProgress(
        job({
          phase: "documents",
          progress: { documentsDone: 9, documentsTotal: 9 },
        }),
        undefined,
        T0,
      ).fraction,
    ).toBeCloseTo(0.9);
  });

  it("reads through the documents phase, and estimates the rest at the rate seen", () => {
    const reading = job({
      phase: "documents",
      progress: { documentsDone: 20, documentsTotal: 40 },
    });
    // 10 documents read in the minute since first seen: 20 left, 2 minutes.
    expect(loadProgress(reading, { at: T0, done: 10 }, T0 + MIN)).toEqual({
      fraction: expect.closeTo(0.35 + 0.55 / 2),
      remainingMs: 2 * MIN,
      overdue: false,
      elapsedMs: MIN,
      step: "Reading 20 of 40 documents",
    });
    // Not seen yet, or nothing read since: no estimate.
    expect(loadProgress(reading, undefined, T0).remainingMs).toBeNull();
    expect(
      loadProgress(reading, { at: T0, done: 20 }, T0 + MIN).remainingMs,
    ).toBeNull();
    // A total that grew past what was read keeps the bar inside its phase.
    expect(
      loadProgress(
        job({
          phase: "documents",
          progress: { documentsDone: 50, documentsTotal: 40 },
        }),
        { at: T0, done: 10 },
        T0 + MIN,
      ),
    ).toMatchObject({ fraction: expect.closeTo(0.9), remainingMs: 0 });
  });

  it("gives an extract's documents most of its bar", () => {
    expect(PHASE_SHARE.documents.ingest).toBeUndefined();
    expect(
      loadProgress(
        job({
          kind: "extract",
          phase: "documents",
          progress: { documentsDone: 1, documentsTotal: 2 },
        }),
        undefined,
        T0,
      ).fraction,
    ).toBeCloseTo(0.05 + 0.85 / 2);
  });
});

describe("a generation's bar", () => {
  const gen = (over: Partial<OntologyGeneration>): OntologyGeneration =>
    ({
      id: "gen_new",
      status: "ONTOLOGY_GENERATION_STATUS_RUNNING",
      startedAt: iso(T0),
      ...over,
    }) as OntologyGeneration;
  // Succeeded in `minutes`, finishing at `end`.
  const took = (
    id: string,
    minutes: number,
    over: Partial<OntologyGeneration> = {},
    end = T0 - 50 * MIN,
  ) =>
    gen({
      id,
      status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
      startedAt: iso(end - minutes * MIN),
      finishedAt: iso(end),
      ...over,
    });

  it("takes the usual time with no history of its kind", () => {
    expect(expectedDuration(gen({}), [], false)).toBe(USUAL_DRAFT_MS.hosted);
    expect(expectedDuration(gen({}), [], true)).toBe(USUAL_DRAFT_MS.local);
    const preview = gen({
      revision: { parentVersionId: "v", dryRun: true },
    } as Partial<OntologyGeneration>);
    expect(expectedDuration(preview, [took("a", 9)], false)).toBe(
      USUAL_REVISION_MS,
    );
  });

  it("takes the median of the latest five that succeeded, of its kind", () => {
    const history = [
      took("a", 4),
      took("b", 10),
      took("c", 6),
      gen({ id: "failed", status: "ONTOLOGY_GENERATION_STATUS_FAILED" }),
      gen({ id: "unfinished" }),
      took("save", 1, {
        revision: { parentVersionId: "v", dryRun: false },
      } as Partial<OntologyGeneration>),
      // Itself never counts.
      took("gen_new", 99),
    ];
    expect(expectedDuration(gen({}), history, false)).toBe(6 * MIN);
    expect(expectedDuration(gen({}), [...history, took("d", 8)], false)).toBe(
      7 * MIN,
    );
    // Only the latest five: the oldest, by when it finished, drops out.
    const latest = [1, 2, 3, 4, 5].map((n) =>
      took(`n${n}`, 20, {}, T0 - n * MIN),
    );
    const oldest = took("old", 1, {}, T0 - 60 * MIN);
    expect(expectedDuration(gen({}), [...latest, oldest], false)).toBe(
      20 * MIN,
    );
  });

  it("runs against its expected time, and says when it runs past it", () => {
    expect(generationProgress(gen({}), 15 * MIN, T0 + 5 * MIN)).toEqual({
      fraction: 1 / 3,
      remainingMs: 10 * MIN,
      overdue: false,
      elapsedMs: 5 * MIN,
      step: "Drafting types, links and mappings",
    });
    expect(generationProgress(gen({}), 15 * MIN, T0 + 20 * MIN)).toMatchObject({
      fraction: 0.95,
      remainingMs: null,
      overdue: true,
    });
    expect(
      generationProgress(
        gen({ revision: { parentVersionId: "v", dryRun: true } }),
        MIN,
        T0,
      ).step,
    ).toBe("Checking the edits");
    expect(
      generationProgress(
        gen({ revision: { parentVersionId: "v", dryRun: false } }),
        MIN,
        T0,
      ).step,
    ).toBe("Creating the edited version");
  });

  it("waits while queued, or to retry", () => {
    const queued = gen({
      status: "ONTOLOGY_GENERATION_STATUS_QUEUED",
      startedAt: undefined,
    });
    expect(generationProgress(queued, MIN, T0)).toMatchObject({
      fraction: 0,
      elapsedMs: 0,
      step: "Waiting to start",
    });
    expect(generationProgress({ ...queued, attempts: 1 }, MIN, T0).step).toBe(
      "Waiting to retry",
    );
  });

  it("ends full, or unmeasured when it failed", () => {
    expect(
      generationProgress(
        gen({
          status: "ONTOLOGY_GENERATION_STATUS_SUCCEEDED",
          finishedAt: iso(T0 + 3 * MIN),
        }),
        MIN,
        T0 + 60 * MIN,
      ),
    ).toMatchObject({ fraction: 1, remainingMs: 0, elapsedMs: 3 * MIN });
    expect(
      generationProgress(
        gen({ status: "ONTOLOGY_GENERATION_STATUS_FAILED" }),
        MIN,
        T0,
      ),
    ).toMatchObject({ fraction: null, step: "Stopped" });
  });
});

describe("the time in words", () => {
  it("says the time left, rounded up past a minute", () => {
    expect(timeLeft(30_000)).toBe("Less than a minute left");
    expect(timeLeft(61_000)).toBe("About 2 min left");
    expect(timeLeft(59 * MIN)).toBe("About 59 min left");
    expect(timeLeft(60 * MIN)).toBe("About 1 h left");
    expect(timeLeft(75 * MIN)).toBe("About 1 h 15 min left");
  });

  it("says the time so far", () => {
    expect(timeSoFar(42_000)).toBe("Under a minute so far");
    expect(timeSoFar(185_000)).toBe("3 min so far");
  });

  it("puts the time left first, then that it's late, then the time so far", () => {
    const base = { fraction: 0.5, overdue: false, elapsedMs: 0, step: "" };
    expect(progressLine({ ...base, remainingMs: 2 * MIN })).toBe(
      "About 2 min left",
    );
    expect(
      progressLine({
        ...base,
        remainingMs: null,
        overdue: true,
        elapsedMs: 20 * MIN,
      }),
    ).toBe("Taking longer than usual · 20 min so far");
    expect(progressLine({ ...base, remainingMs: null, elapsedMs: 5000 })).toBe(
      "Under a minute so far",
    );
    expect(progressLine({ ...base, remainingMs: null })).toBe("");
    expect(
      progressLine({ ...base, fraction: 1, remainingMs: 0, elapsedMs: 9000 }),
    ).toBe("Under a minute so far");
  });
});

describe("the jobs this browser follows", () => {
  const job = (id: string, at: number, kind: WatchedJob["kind"] = "load") => ({
    kind,
    ontologyId: "ont_1",
    id,
    at,
  });

  it("reads anything, keeping well-formed jobs under a day old, newest first", () => {
    expect(parseWatched(null, T0)).toEqual([]);
    expect(parseWatched("{", T0)).toEqual([]);
    expect(parseWatched('{"kind":"load"}', T0)).toEqual([]);
    const raw = JSON.stringify([
      job("old", T0 - WATCH_MS),
      job("a", T0 - 2),
      { ...job("b", T0 - 1), extra: true },
      { ...job("c", T0), kind: "question" },
      { ...job("", T0) },
      { ...job("d", T0), ontologyId: 5 },
      null,
      "x",
    ]);
    expect(parseWatched(raw, T0)).toEqual([job("b", T0 - 1), job("a", T0 - 2)]);
    const many = JSON.stringify(
      [1, 2, 3, 4, 5].map((n) => job(`j${n}`, T0 - n)),
    );
    expect(parseWatched(many, T0).map((j) => j.id)).toEqual(["j1", "j2", "j3"]);
  });

  it("adds a job first, once, and keeps the newest few", () => {
    const list = [job("a", 2), job("b", 1)];
    expect(withJob(list, job("b", 3))).toEqual([job("b", 3), job("a", 2)]);
    expect(withJob(list, job("b", 3, "generation"))).toHaveLength(3);
    expect(
      withJob([job("a", 3), job("b", 2), job("c", 1)], job("d", 4)),
    ).toHaveLength(MAX_WATCHED);
    expect(withoutJob(list, "load", "a")).toEqual([job("b", 1)]);
    expect(withoutJob(list, "generation", "a")).toEqual(list);
  });

  it("leaves a job to the pages that show it", () => {
    const gen = job("gen_1", 0, "generation"),
      load = job("load_1", 0);
    expect(shownOn(gen, "/ontology/ont_1")).toBe(true);
    expect(shownOn(load, "/ontology/ont_1")).toBe(true);
    expect(shownOn(gen, "/ontology/ont_1/generations/gen_1")).toBe(true);
    expect(shownOn(gen, "/ontology/ont_1/generations/gen_2")).toBe(false);
    expect(shownOn(load, "/ontology/ont_1/loads/load_1")).toBe(true);
    expect(shownOn(load, "/ontology/ont_1/loads/load_2")).toBe(false);
    expect(shownOn(load, "/ontology/ont_1/generations/load_1")).toBe(false);
    expect(shownOn(gen, "/ontology/ont_2")).toBe(false);
    expect(shownOn(gen, "/data")).toBe(false);
    // The sample's tour shows the sample's jobs only.
    expect(shownOn(load, "/ontology/sample", "ont_1")).toBe(true);
    expect(shownOn(load, "/ontology/sample", "ont_9")).toBe(false);
    expect(shownOn(load, "/ontology/sample")).toBe(false);
  });

  describe("in storage", () => {
    let store: Map<string, string>;
    beforeEach(() => {
      store = new Map();
      vi.stubGlobal("localStorage", {
        getItem: (k: string) => store.get(k) ?? null,
        setItem: (k: string, v: string) => void store.set(k, v),
        removeItem: (k: string) => void store.delete(k),
      });
      vi.useFakeTimers({ now: T0 });
    });
    afterEach(() => {
      vi.useRealTimers();
      vi.unstubAllGlobals();
    });
    const kept = (ws: string) =>
      JSON.parse(store.get("foundry.jobs.watched.v1:" + ws) ?? "[]");

    it("follows, hands on and stops following per workspace", () => {
      watchJob("ws_a", { kind: "generation", ontologyId: "o", id: "g" });
      expect(kept("ws_a")).toEqual([
        { kind: "generation", ontologyId: "o", id: "g", at: T0 },
      ]);
      expect(kept("ws_b")).toEqual([]);
      followJob("ws_a", kept("ws_a")[0], { kind: "load", id: "l" });
      expect(kept("ws_a")).toEqual([
        { kind: "load", ontologyId: "o", id: "l", at: T0 },
      ]);
      unwatchJob("ws_a", "load", "l");
      expect(store.has("foundry.jobs.watched.v1:ws_a")).toBe(false);
    });

    it("tells its subscribers of a change, here or in another tab", () => {
      const added: string[] = [],
        removed: string[] = [];
      vi.stubGlobal("window", {
        addEventListener: (type: string) => added.push(type),
        removeEventListener: (type: string) => removed.push(type),
      });
      const heard = vi.fn(),
        stop = subscribeWatched(heard);
      expect(added).toEqual(["storage"]);
      watchJob("ws_a", { kind: "load", ontologyId: "o", id: "l" });
      expect(heard).toHaveBeenCalledTimes(1);
      // Read again only when the stored list changes.
      expect(readWatched("ws_a")).toBe(readWatched("ws_a"));
      stop();
      expect(removed).toEqual(["storage"]);
      unwatchJob("ws_a", "load", "l");
      expect(heard).toHaveBeenCalledTimes(1);
      expect(readWatched("ws_a")).toEqual([]);
    });

    it("keeps nothing, and breaks nothing, when storage is blocked", () => {
      vi.stubGlobal("localStorage", {
        getItem: () => {
          throw new Error("blocked");
        },
        setItem: () => {
          throw new Error("blocked");
        },
        removeItem: () => {
          throw new Error("blocked");
        },
      });
      expect(() =>
        watchJob("ws_a", { kind: "load", ontologyId: "o", id: "l" }),
      ).not.toThrow();
      expect(() => unwatchJob("ws_a", "load", "l")).not.toThrow();
    });
  });
});
