import { it, expect, vi } from "vitest";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
vi.mock("../apps/api/src/source-sync.js", () => ({
  startChangeStreams: () => async () => {},
  scheduledRefresh: () => false,
  needsChangeRefresh: () => false,
  nextRefresh: () => null,
}));
vi.mock("../apps/api/src/assistant.js", () => ({
  providerSettings: async () => ({ continuous: false }),
  buildWorkspace: vi.fn(),
  enqueue: vi.fn(),
}));
vi.mock("../apps/api/src/setup-jobs.js", () => ({ prepareProvider: vi.fn() }));
vi.mock("../apps/api/src/data-jobs.js", () => ({
  runSyncJob: vi.fn(),
  runUploadJob: vi.fn(),
}));
vi.mock("../apps/api/src/pipeline-jobs.js", () => ({
  runPipelineJob: vi.fn(),
}));
import { startWorker } from "../apps/api/src/jobs.js";
it.each(["missing_handler", "toString"])(
  "records unsupported operation %s as failed instead of succeeded",
  async (type) => {
    const store = new MemoryStore();
    await transaction(store, "global", async (tx) => {
      tx.put(resource("global", "workspaceRef", "Fixture", {}, "fixture"));
    });
    await transaction(store, "fixture", async (tx) => {
      tx.put(
        resource(
          "fixture",
          "job",
          "Unknown",
          { type, status: "queued", attempts: 2 },
          "job",
        ),
      );
    });
    const stop = startWorker(store);
    try {
      for (
        let i = 0;
        i < 100 &&
        (await store.get("fixture", "job"))?.data.status !== "failed";
        i++
      )
        await new Promise((r) => setTimeout(r, 5));
      const job = await store.get("fixture", "job");
      expect(job?.data.status).toBe("failed");
      expect(job?.data.error).toBe("Unsupported job type: " + type);
      expect(await store.get("fixture", "failure_job")).toBeDefined();
    } finally {
      await stop();
    }
  },
);
