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
    stop();
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
        { kind: "mysql", secret: seal({ database: "fixture" }) },
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
    stop();
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
