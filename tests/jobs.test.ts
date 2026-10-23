import { it, expect, vi } from "vitest";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { startWorker } from "../apps/api/src/jobs.js";
import { seal } from "../apps/api/src/security.js";
const imports = vi.hoisted(() => ({ count: 0 }));
vi.mock("../apps/api/src/connectors.js", () => ({
  importSource: async () => {
    imports.count++;
    return { file: "test.jsonl" };
  },
}));
vi.mock("../apps/api/src/datasets.js", () => ({
  ingestFile: async (
    _s: any,
    scope: string,
    name: string,
    _f: string,
    source: string,
    id: string,
    version: string,
  ) => resource(scope, "dataset", name, { activeVersion: version }, id),
  queryDatasets: vi.fn(),
}));
// These fixtures test snapshot scheduling, without replication discovery
// attempting to contact a database that is not part of this test.
const snapshotSync = () => ({
  mode: "snapshot",
  status: "ready",
  recheckAt: Date.now() + 60_000,
});
it("coalesces scheduled imports with queued work and completes a durable job", async () => {
  const store = new MemoryStore(),
    scope = "jobs";
  await transaction(store, "global", async (tx) =>
    tx.put(resource("global", "workspaceRef", "Jobs", {}, scope)),
  );
  await transaction(store, scope, async (tx) => {
    tx.put(
      resource(
        scope,
        "source",
        "MySQL source",
        {
          kind: "mysql",
          secret: seal({ database: "fixture" }),
          schedule: "* * * * *",
          nextRun: 0,
          sync: snapshotSync(),
        },
        "source",
      ),
    );
    tx.put(
      resource(
        scope,
        "job",
        "Manual import",
        { type: "sync", sourceId: "source", status: "queued" },
        "job",
      ),
    );
  });
  // The next run is the minute after the worker scheduled it, which the import
  // itself can cross.
  const started = Date.now();
  const stop = startWorker(store);
  try {
    for (
      let i = 0;
      i < 50 && (await store.get(scope, "job"))?.data.status !== "succeeded";
      i++
    )
      await new Promise((r) => setTimeout(r, 50));
    expect((await store.get(scope, "job"))?.data.status).toBe("succeeded");
    expect(imports.count).toBe(1);
    expect((await store.list(scope, "job")).length).toBe(1);
    expect((await store.get(scope, "source"))?.data.nextRun).toBeGreaterThan(
      started,
    );
  } finally {
    await stop();
  }
});
it("recovers an interrupted import lease without creating another operation", async () => {
  const store = new MemoryStore(),
    scope = "recovery";
  await transaction(store, "global", async (tx) =>
    tx.put(resource("global", "workspaceRef", "Recovery", {}, scope)),
  );
  await transaction(store, scope, async (tx) => {
    tx.put(
      resource(
        scope,
        "source",
        "Orders",
        {
          kind: "mysql",
          secret: seal({ database: "fixture" }),
          sync: snapshotSync(),
        },
        "source",
      ),
    );
    tx.put(
      resource(
        scope,
        "job",
        "Orders",
        {
          type: "sync",
          sourceId: "source",
          status: "running",
          leaseUntil: Date.now() - 1,
          attempts: 1,
        },
        "interrupted",
      ),
    );
  });
  const stop = startWorker(store);
  try {
    for (
      let i = 0;
      i < 60 &&
      (await store.get(scope, "interrupted"))?.data.status !== "succeeded";
      i++
    )
      await new Promise((r) => setTimeout(r, 50));
    expect((await store.get(scope, "interrupted"))?.data.status).toBe(
      "succeeded",
    );
    expect((await store.get(scope, "interrupted"))?.data.attempts).toBe(2);
    expect(await store.list(scope, "job")).toHaveLength(1);
  } finally {
    await stop();
  }
});
it.each(["missing_handler", "toString"])(
  "records an unsupported %s job as failed instead of succeeded",
  async (type) => {
    const store = new MemoryStore(),
      scope = "unsupported";
    await transaction(store, "global", async (tx) =>
      tx.put(resource("global", "workspaceRef", "Unsupported", {}, scope)),
    );
    // On its last attempt, so the failure is final.
    await transaction(store, scope, async (tx) =>
      tx.put(
        resource(
          scope,
          "job",
          "Unknown",
          { type, status: "queued", attempts: 2 },
          "job",
        ),
      ),
    );
    const stop = startWorker(store);
    try {
      for (
        let i = 0;
        i < 50 && (await store.get(scope, "job"))?.data.status !== "failed";
        i++
      )
        await new Promise((r) => setTimeout(r, 50));
      const job = await store.get(scope, "job");
      expect(job?.data.status).toBe("failed");
      expect(job?.data.error).toBe("Unsupported job type: " + type);
      expect(await store.get(scope, "failure_job")).toBeDefined();
    } finally {
      await stop();
    }
  },
);
// Two scheduled sources that are due: one feeds a running setup build whose
// lease outlived a worker restart, the other does not. Builds that end with a
// proposal for review read both, and run too: a chat request, continuous
// repair, and setup from a goal. So does a chat question.
async function buildingWorkspace(store: MemoryStore, scope: string) {
  await transaction(store, "global", async (tx) =>
    tx.put(resource("global", "workspaceRef", scope, {}, scope)),
  );
  await transaction(store, scope, async (tx) => {
    for (const id of ["input", "other"]) {
      tx.put(
        resource(
          scope,
          "source",
          id,
          {
            kind: "stripe",
            secret: seal({}),
            schedule: "* * * * *",
            nextRun: 0,
          },
          id,
        ),
      );
      tx.put(
        resource(
          scope,
          "dataset",
          id,
          { sourceId: id, activeVersion: "v1" },
          id + "_data",
        ),
      );
    }
    tx.put(
      resource(
        scope,
        "run",
        "Build",
        {
          task: "onboarding_build",
          ontology: { entities: [], links: [] },
          status: "running",
          leaseUntil: Date.now() + 86400000,
          datasetIds: ["input_data"],
        },
        "build",
      ),
    );
    for (const [id, data] of [
      ["question", { intent: "auto" }],
      ["chat-build", { intent: "auto", resolvedIntent: "build" }],
      ["repair", { intent: "build", background: true }],
      [
        "goal-build",
        {
          task: "onboarding_build",
          intent: "build",
          datasetIds: ["input_data", "other_data"],
        },
      ],
    ] as const)
      tx.put(
        resource(
          scope,
          "run",
          id,
          { ...data, status: "running", leaseUntil: Date.now() + 86400000 },
          id,
        ),
      );
  });
}
const synced = async (store: MemoryStore, scope: string, source: string) =>
  (await store.list(scope, "job")).some(
    (j) => j.data.sourceId === source && j.data.status === "succeeded",
  );
async function until(condition: () => Promise<boolean>) {
  for (let i = 0; i < 100 && !(await condition()); i++)
    await new Promise((r) => setTimeout(r, 50));
  expect(await condition()).toBe(true);
}
it("defers background syncs of a running build's inputs, and only those, until it ends", async () => {
  const store = new MemoryStore(),
    scope = "deferral";
  await buildingWorkspace(store, scope);
  // Queued before the build started.
  await transaction(store, scope, async (tx) =>
    tx.put(
      resource(
        scope,
        "job",
        "Sync input",
        { type: "sync", sourceId: "input", status: "queued", background: true },
        "queued",
      ),
    ),
  );
  const before = imports.count;
  const stop = startWorker(store);
  try {
    await until(() => synced(store, scope, "other"));
    expect(imports.count).toBe(before + 1);
    expect((await store.get(scope, "queued"))?.data.status).toBe("queued");
    // Its next refresh joins the queued one.
    expect((await store.get(scope, "input"))?.data.nextRun).toBeGreaterThan(0);
    await transaction(store, scope, async (tx) => {
      const run = (await tx.get("build"))!;
      tx.update(run, { ...run.data, status: "succeeded" });
    });
    await until(() => synced(store, scope, "input"));
    expect(imports.count).toBe(before + 2);
    expect((await store.get(scope, "queued"))?.data.status).toBe("succeeded");
    expect(
      (await store.list(scope, "job")).filter(
        (j) => j.data.sourceId === "input",
      ),
    ).toHaveLength(1);
    expect((await store.get(scope, "input"))?.data.nextRun).toBeGreaterThan(0);
  } finally {
    await stop();
  }
});
it("stops deferring a build's input 15 minutes after its sync was queued, across worker restarts", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  const store = new MemoryStore(),
    scope = "bounded-deferral";
  await buildingWorkspace(store, scope);
  let stop = startWorker(store);
  try {
    await until(() => synced(store, scope, "other"));
    const waiting = (await store.list(scope, "job")).filter(
      (j) => j.data.sourceId === "input",
    );
    expect(waiting.map((j) => j.data.status)).toEqual(["queued"]);
    // As when the build keeps crashing the worker.
    await stop();
    vi.setSystemTime(Date.now() + 15 * 60000);
    stop = startWorker(store);
    await until(() => synced(store, scope, "input"));
    expect((await store.get(scope, waiting[0].id))?.data.status).toBe(
      "succeeded",
    );
    expect((await store.get(scope, "build"))?.data.status).toBe("running");
  } finally {
    await stop();
    vi.useRealTimers();
  }
});
