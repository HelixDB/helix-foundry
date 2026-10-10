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
const dataJobs = vi.hoisted(() => ({
  runSyncJob: vi.fn(),
  runUploadJob: vi.fn(),
}));
vi.mock("../apps/api/src/data-jobs.js", () => dataJobs);
vi.mock("../apps/api/src/pipeline-jobs.js", () => ({
  runPipelineJob: vi.fn(),
}));
import { startWorker } from "../apps/api/src/jobs.js";

// A worker that dies mid-job leaves the job "running" with a lease that
// expires. Reclaiming it is right the first times; reclaiming it forever is
// not, because a job that crashes its worker on every attempt would never
// fail and never produce a finding.
async function seed(attempts: number) {
  const store = new MemoryStore();
  await transaction(store, "global", async (tx) => {
    tx.put(resource("global", "workspaceRef", "Fixture", {}, "fixture"));
  });
  await transaction(store, "fixture", async (tx) => {
    tx.put(
      resource(
        "fixture",
        "job",
        "Sync orders",
        {
          type: "sync",
          status: "running",
          leaseUntil: Date.now() - 1000,
          attempts,
          sourceId: "orders",
        },
        "job",
      ),
    );
  });
  return store;
}

async function settle(store: MemoryStore, done: (status: string) => boolean) {
  for (
    let i = 0;
    i < 200 && !done((await store.get("fixture", "job"))?.data.status);
    i++
  )
    await new Promise((r) => setTimeout(r, 5));
  return store.get("fixture", "job");
}

it("fails a job whose lease expired after the last allowed attempt", async () => {
  dataJobs.runSyncJob.mockReset();
  const store = await seed(3);
  const stop = startWorker(store);
  try {
    const job = await settle(store, (s) => s === "failed");
    expect(job?.data.status).toBe("failed");
    expect(job?.data.error).toMatch(/Lease expired 3 times/);
    expect(dataJobs.runSyncJob).not.toHaveBeenCalled();
    const finding = await store.get("fixture", "failure_job");
    expect(finding?.data.kind).toBe("failure");
  } finally {
    await stop();
  }
});

it("still reclaims an expired lease while attempts remain", async () => {
  dataJobs.runSyncJob.mockReset();
  dataJobs.runSyncJob.mockResolvedValue(undefined);
  const store = await seed(1);
  const stop = startWorker(store);
  try {
    const job = await settle(store, (s) => s === "succeeded" || s === "failed");
    expect(job?.data.status).toBe("succeeded");
    expect(job?.data.attempts).toBe(2);
    expect(dataJobs.runSyncJob).toHaveBeenCalledTimes(1);
  } finally {
    await stop();
  }
});
