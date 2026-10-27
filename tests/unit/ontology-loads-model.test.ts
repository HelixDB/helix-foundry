import { describe, expect, it, vi } from "vitest";
import type {
  FoundryLoad,
  FoundryOntology,
  GraphJob,
  Project,
  Tenant,
} from "@/shared/wfe";
import { ApiError } from "@/web/api";
import {
  LOAD_LIMITS,
  budgetStopText,
  databaseChoices,
  isNoSubscription,
  chooseLoad,
  isPickable,
  jobBadge,
  jobTotals,
  loadActions,
  loadBadge,
  loadOptionLabel,
  loadVersion,
  versionLabels,
  versionLayout,
  withLoad,
  jobDuration,
  kindLabel,
  loadRefusal,
  loadRetry,
  loadSources,
  loadSteps,
  phaseLabel,
  slugify,
  type AgentMapping,
} from "@/web/areas/ontology/model";

// The Ontology area's logic for databases and loads: which databases an
// ontology can bind to, slugs for new ones, the Helix plan a new one needs,
// and what a load of the ontology's database is doing.

const job = (j: Partial<GraphJob>): GraphJob => ({ id: "j", ...j });

describe("databases", () => {
  it("slugs a name for a project or database", () => {
    expect(slugify("Customer Contracts — 2026!")).toBe(
      "customer-contracts-2026",
    );
    expect(slugify("Café Société")).toBe("cafe-societe");
    expect(slugify("  --  ")).toBe("ontology");
    expect(slugify("***", "database")).toBe("database");
    const long = slugify("a".repeat(62) + " b c");
    expect(long.length).toBeLessThanOrEqual(63);
    expect(long).not.toMatch(/-$/);
  });

  it("lists every live database by project, with the ontology on each", () => {
    const project = (id: string, displayName: string): Project => ({
      id,
      displayName,
    });
    const tenant = (
      id: string,
      name: string,
      status: Tenant["status"] = "RESOURCE_STATUS_ACTIVE",
    ): Tenant => ({
      id,
      name,
      status,
      timestamps: { createdAt: "2026-10-01" },
    });
    const ontologies: FoundryOntology[] = [
      { id: "o1", name: "Contracts", tenantId: "t2" },
    ];
    expect(
      databaseChoices(
        [
          {
            project: project("p2", "Staging"),
            databases: [
              tenant("t3", "sandbox"),
              tenant("t4", "old", "RESOURCE_STATUS_DELETED"),
            ],
          },
          {
            project: { id: "p1", slug: "production" },
            databases: [tenant("t2", "contracts"), tenant("t1", "orders")],
          },
        ],
        ontologies,
      ).map((c) => [c.project, c.name, c.usedBy]),
    ).toEqual([
      ["production", "contracts", "Contracts"],
      ["production", "orders", undefined],
      ["Staging", "sandbox", undefined],
    ]);
    expect(databaseChoices([], [])).toEqual([]);
  });

  it("knows the WFE's refusal for want of a Helix subscription", () => {
    const info = (reason: string) => ({
      "@type": "type.googleapis.com/google.rpc.ErrorInfo",
      reason,
    });
    expect(
      isNoSubscription(
        new ApiError("no subscription", 400, 9, [
          info("NO_WORKSPACE_SUBSCRIPTION"),
        ]),
      ),
    ).toBe(true);
    expect(
      isNoSubscription(
        new ApiError("pro", 400, 9, [info("FOUNDRY_PRO_REQUIRED")]),
      ),
    ).toBe(false);
    expect(isNoSubscription(new ApiError("x", 400, 9))).toBe(false);
    expect(isNoSubscription(new Error("x"))).toBe(false);
  });
});

describe("loads", () => {
  it("loads only an agent draft whose status is done", () => {
    expect(loadRefusal("agent_bundle", "done")).toBeUndefined();
    expect(loadRefusal("foundry_v1", "done")).toMatch(/^Only an agent draft/);
    expect(loadRefusal("agent_bundle", "needs_input")).toMatch(/questions/);
    expect(loadRefusal("agent_bundle", "budget_exhausted")).toMatch(/stopped/);
    expect(loadRefusal("agent_bundle", undefined)).toBe(
      "This draft isn’t finished.",
    );
    expect(LOAD_LIMITS).toEqual({
      jobUsd: 20,
      monthUsd: 100,
      documents: 200,
      documentBytes: 1_048_576,
    });
  });

  it("names each table a draft's mappings read once", () => {
    const m = (name: string, rows: AgentMapping["rows"]): AgentMapping => ({
      name,
      rows,
      rowKey: [],
      links: [],
      bindings: [],
    });
    expect(
      loadSources([
        m("a", { kind: "source", source: "src_1" }),
        m("b", { kind: "source", source: "src_1", object: "lines" }),
        m("c", { kind: "nested", parent: "a", records: "/x" }),
        m("d", { kind: "source", source: "src_2" }),
      ]),
    ).toEqual(["src_1", "src_2"]);
  });

  it("badges a load by its status and step", () => {
    expect(jobBadge(job({ status: "queued" }))).toEqual({
      status: "queued",
      label: "Queued",
    });
    expect(jobBadge(job({ status: "running", phase: "ingest" }))).toEqual({
      status: "running",
      label: "Loading tables",
    });
    expect(jobBadge(job({ status: "running" }))).toEqual({
      status: "running",
      label: "Preparing",
    });
    expect(jobBadge(job({ status: "succeeded" }))).toEqual({
      status: "succeeded",
      label: "Loaded",
    });
    expect(jobBadge(job({ status: "budget_exhausted" }))).toEqual({
      status: "stale",
      label: "Stopped at the cap",
    });
    expect(jobBadge(job({ status: "failed" }))).toEqual({
      status: "failed",
      label: "Failed",
    });
    expect(jobBadge(null)).toEqual({ status: "draft", label: "Unknown" });
    expect(phaseLabel(job({ phase: "documents" }))).toBe("Reading documents");
    expect(phaseLabel(job({ phase: "finishing" }))).toBe("Finishing");
  });

  it("says why a load stopped at a cap: its own words, else the caps", () => {
    expect(
      budgetStopText(
        job({
          status: "budget_exhausted",
          error: "the load spent its $20.00 cap",
        }),
      ),
    ).toBe("The load spent its $20.00 cap.");
    // An API source's allowance names the source, whatever it read.
    expect(
      budgetStopText(
        job({
          status: "budget_exhausted",
          progress: { documentsDone: 12 },
          error:
            "the API source Support notes spent its $5.00 of this month's extraction by work no one started; its newest pieces wait for next month or a load a person starts",
        }),
      ),
    ).toBe(
      "The API source Support notes spent its $5.00 of this month's extraction by work no one started; its newest pieces wait for next month or a load a person starts.",
    );
    // A job that says nothing (an older backend) gets the caps it may have
    // reached, with what it read.
    const caps = (documentsDone?: number) =>
      budgetStopText(
        job({
          status: "budget_exhausted",
          error: "",
          ...(documentsDone === undefined
            ? {}
            : { progress: { documentsDone } }),
        }),
      );
    expect(caps(1)).toBe(
      "It read 1 document before reaching a spending cap ($20 a load, $100 a workspace a month). What it read is in the database; the documents it didn’t reach are read once it is current and the month has budget.",
    );
    expect(caps(3)).toMatch(/^It read 3 documents before/);
    expect(caps()).toMatch(/^It read 0 documents before/);
  });

  it("times a load from its start", () => {
    const now = Date.parse("2026-10-18T12:05:00Z");
    expect(jobDuration(job({}), now)).toBe("");
    expect(jobDuration(job({ startedAt: "2026-10-18T12:04:18Z" }), now)).toBe(
      "42s",
    );
    expect(
      jobDuration(
        job({
          startedAt: "2026-10-18T12:00:00Z",
          finishedAt: "2026-10-18T12:03:05Z",
        }),
        now,
      ),
    ).toBe("3m 5s");
  });

  it("steps through a load and shows where one stopped", () => {
    const status = (j: Partial<GraphJob>) =>
      loadSteps(job(j), 2).map((s) => s.status);
    expect(status({ status: "queued", phase: "provisioning" })).toEqual([
      "active",
      "pending",
      "pending",
      "pending",
      "pending",
    ]);
    expect(status({ status: "running", phase: "provisioning" })).toEqual([
      "done",
      "active",
      "pending",
      "pending",
      "pending",
    ]);
    expect(status({ status: "running", phase: "ingest" })).toEqual([
      "done",
      "done",
      "active",
      "pending",
      "pending",
    ]);
    expect(status({ status: "running", phase: "documents" })).toEqual([
      "done",
      "done",
      "done",
      "active",
      "pending",
    ]);
    expect(status({ status: "running", phase: "finishing" })).toEqual([
      "done",
      "done",
      "done",
      "done",
      "active",
    ]);
    expect(status({ status: "succeeded", phase: "finishing" })).toEqual([
      "done",
      "done",
      "done",
      "done",
      "done",
    ]);
    expect(status({ status: "failed", phase: "ingest" })).toEqual([
      "done",
      "done",
      "pending",
      "pending",
      "pending",
    ]);
    expect(status({ status: "budget_exhausted", phase: "documents" })).toEqual([
      "done",
      "done",
      "done",
      "pending",
      "pending",
    ]);
    const labels = loadSteps(
      job({
        status: "running",
        phase: "documents",
        progress: { documentsDone: 1, documentsTotal: 3 },
      }),
      1,
    ).map((s) => s.label);
    expect(labels).toEqual([
      "Started",
      "Connecting to the database",
      "Loading 1 table",
      "Reading 1 of 3 documents",
      "Finishing",
    ]);
    expect(loadSteps(job({ status: "queued" }), 0)[0]!.label).toBe(
      "Waiting to start",
    );
    expect(
      loadSteps(
        job({ status: "running", progress: { documentsTotal: 1 } }),
        0,
      )[3]!.label,
    ).toBe("Reading 0 of 1 document");
  });

  it("steps an extract through the documents only", () => {
    const extract = (j: Partial<GraphJob>) =>
      loadSteps(job({ kind: "extract", ...j }), 4).map(
        (s) => `${s.label}: ${s.status}`,
      );
    expect(extract({ status: "queued", phase: "documents" })).toEqual([
      "Waiting to start: active",
      "Reading documents: pending",
      "Finishing: pending",
    ]);
    expect(
      extract({
        status: "running",
        phase: "documents",
        progress: { documentsDone: 0, documentsTotal: 2 },
      }),
    ).toEqual([
      "Started: done",
      "Reading 0 of 2 documents: active",
      "Finishing: pending",
    ]);
    expect(extract({ status: "succeeded", phase: "finishing" })).toEqual([
      "Started: done",
      "Reading documents: done",
      "Finishing: done",
    ]);
    expect(kindLabel(job({ kind: "extract" }))).toBe("New documents");
    expect(kindLabel(job({ kind: "load" }))).toBe("Tables and documents");
    expect(kindLabel(job({}))).toBe("Tables and documents");
  });

  it("says a load waiting on a new database is waiting for it to start", () => {
    const created = "2026-10-19T08:13:15.000Z",
      t = Date.parse(created);
    const refused = job({
      status: "queued",
      error: "the database refused a write; Helix will retry",
    });
    // In the database's first minutes, the database's refusals are its start.
    expect(loadRetry(refused, created, t + 9 * 60_000)).toEqual({
      kind: "starting",
    });
    expect(
      loadRetry(
        job({
          status: "queued",
          error: "building the graph's indexes: rate_limit_unavailable",
        }),
        created,
        t,
      ),
    ).toEqual({ kind: "starting" });
    // Helix failing to read its own records of the database is not its start.
    for (const error of [
      "the ontology's database can't be reached; Helix will retry",
      "the ontology’s database can’t be reached; Helix will retry",
    ])
      expect(loadRetry(job({ status: "queued", error }), created, t)).toEqual({
        kind: "failed",
        reason: error,
      });
    // After the window, or for another failure: why the attempt failed.
    expect(loadRetry(refused, created, t + 15 * 60_000)).toEqual({
      kind: "failed",
      reason: "the database refused a write; Helix will retry",
    });
    expect(loadRetry(refused, undefined, t)).toMatchObject({ kind: "failed" });
    expect(
      loadRetry(
        job({
          status: "queued",
          error: "the ontology agent is unavailable; Helix will retry",
        }),
        created,
        t,
      ),
    ).toEqual({
      kind: "failed",
      reason: "the ontology agent is unavailable; Helix will retry",
    });
    // Not waiting to be tried again: running, finished, or never failed.
    expect(loadRetry(job({ status: "queued" }), created, t)).toBeNull();
    for (const status of ["running", "failed", "succeeded"] as const)
      expect(loadRetry({ ...refused, status }, created, t), status).toBeNull();
  });

  it("never says a local build's database is starting: it has one HelixDB, always up", async () => {
    vi.stubEnv("NEXT_PUBLIC_FOUNDRY_MODE", "local");
    vi.resetModules();
    try {
      const local = await import("@/web/areas/ontology/model");
      const created = "2026-10-19T08:13:15.000Z",
        t = Date.parse(created);
      const error = "the database refused a write; Helix will retry";
      expect(
        local.loadRetry(job({ status: "queued", error }), created, t),
      ).toEqual({ kind: "failed", reason: error });
    } finally {
      vi.unstubAllEnvs();
      vi.resetModules();
    }
  });
});

describe("versioned loads", () => {
  const load = (l: Partial<FoundryLoad>): FoundryLoad => ({
    id: "L",
    version: "foundry-v3",
    status: "ready",
    ...l,
  });

  it("counts a load's jobs and spend from the totals, not its newest 20", () => {
    const jobs = Array.from({ length: 20 }, (_, i) =>
      job({ id: `J${i}`, kind: "extract", usdMicros: "1000" }),
    );
    expect(jobTotals(load({ jobs, jobCount: 31, usdMicros: "45000" }))).toEqual(
      { count: 31, usdMicros: 45_000 },
    );
    // A response without the totals sums its list.
    expect(jobTotals(load({ jobs }))).toEqual({ count: 20, usdMicros: 20_000 });
    expect(jobTotals(load({}))).toEqual({ count: 0, usdMicros: 0 });
  });

  it("chooses the URL's load, else the current one, else the newest", () => {
    const a = load({ id: "A", current: true }),
      b = load({ id: "B", status: "writing" }),
      c = load({ id: "C", status: "partial" });
    const loads = [b, c, a];
    expect(chooseLoad(loads, "A", null)).toEqual({
      kind: "load",
      load: a,
      explicit: false,
    });
    expect(chooseLoad(loads, "A", "C")).toEqual({
      kind: "load",
      load: c,
      explicit: true,
    });
    expect(chooseLoad(loads, "A", "other")).toEqual({ kind: "other" });
    expect(chooseLoad(loads, "A", "gone")).toEqual({
      kind: "missing",
      id: "gone",
    });
    // No current load: the newest, even one still being written.
    expect(chooseLoad(loads, undefined, null)).toEqual({
      kind: "load",
      load: b,
      explicit: false,
    });
    // No load at all: nothing to read, whatever the URL names, but Other
    // data when asked for.
    expect(chooseLoad([], undefined, null)).toEqual({ kind: "none" });
    expect(chooseLoad([], "A", null)).toEqual({ kind: "none" });
    expect(chooseLoad([], undefined, "gone")).toEqual({ kind: "none" });
    expect(chooseLoad([], undefined, "other")).toEqual({ kind: "other" });
  });

  it("labels a load in a picker and says what may be done to it", () => {
    expect(loadOptionLabel(load({ current: true }))).toBe("Current · v3");
    expect(loadVersion({ version: "foundry-v12" })).toBe("v12");
    expect(loadVersion({ version: "draft" })).toBe("draft");
    expect(loadVersion({})).toBe("v?");
    expect(jobBadge(job({ status: "cancelled" }))).toEqual({
      status: "canceled",
      label: "Cancelled",
    });
    expect(
      loadOptionLabel(
        load({ status: "partial", createdAt: "2026-10-18T12:00:00Z" }),
      ),
    ).toMatch(/^v3 · .+ · partial$/);
    expect(
      loadOptionLabel(
        load({
          status: "writing",
          jobs: [
            {
              id: "j",
              status: "running",
              progress: { documentsDone: 2, documentsTotal: 5 },
            },
          ],
        }),
      ),
    ).toBe("v3 · writing, 2 of 5 documents");
    expect(loadOptionLabel(load({ status: "deleting" }))).toBe("v3 · deleting");
    expect(isPickable(load({}))).toBe(true);
    expect(isPickable(load({ status: "deleting" }))).toBe(false);
    expect(loadActions(load({ current: true }))).toEqual({
      setCurrent: false,
      remove: false,
      retryDelete: false,
    });
    expect(loadActions(load({}))).toEqual({
      setCurrent: true,
      remove: true,
      retryDelete: false,
    });
    expect(loadActions(load({ status: "failed" }))).toEqual({
      setCurrent: false,
      remove: true,
      retryDelete: false,
    });
    expect(loadActions(load({ status: "writing" }))).toEqual({
      setCurrent: false,
      remove: false,
      retryDelete: false,
    });
    expect(loadActions(load({ status: "delete_failed" }))).toEqual({
      setCurrent: false,
      remove: false,
      retryDelete: true,
    });
    // A load whose extract is running is neither made current nor deleted.
    expect(
      loadActions(
        load({ jobs: [{ id: "x", kind: "extract", status: "running" }] }),
      ),
    ).toEqual({ setCurrent: false, remove: false, retryDelete: false });
    expect(loadBadge(load({ status: "partial" }))).toEqual({
      status: "stale",
      label: "Partial",
    });
    expect(loadBadge(null).label).toBe("Unknown");
  });

  it("carries the page's load in links", () => {
    expect(withLoad("/explore/o/records", null)).toBe("/explore/o/records");
    expect(withLoad("/explore/o/records", "L1")).toBe(
      "/explore/o/records?load=L1",
    );
    expect(withLoad("/explore/o/entities/e?tab=facts", "other")).toBe(
      "/explore/o/entities/e?tab=facts&load=other",
    );
    expect(withLoad("/ontology/o/documents/d#s-1-2", "L1")).toBe(
      "/ontology/o/documents/d?load=L1#s-1-2",
    );
  });

  it("lays a version out for a load's explorer: hubs first, links with ends", () => {
    expect(versionLayout(null)).toEqual({ types: [], links: [] });
    expect(
      versionLabels([
        { types: new Map([["A", 1]]), links: new Set(["r"]) },
        { types: new Map([["B", 1]]), links: new Set(["s"]) },
      ]),
    ).toEqual({ nodes: new Set(["A", "B"]), edges: new Set(["r", "s"]) });
  });
});
