import { afterEach, expect, it, vi } from "vitest";
import { MemoryStore, resource } from "../apps/api/src/store.js";
import { runPipelineJob } from "../apps/api/src/pipeline-jobs.js";
import { queryDatasets } from "../apps/api/src/datasets.js";

vi.mock("../apps/api/src/datasets.js", () => ({ queryDatasets: vi.fn() }));
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});
async function fixture() {
  vi.useFakeTimers();
  const store = new MemoryStore(),
    scope = "synthetic-cancel-poll";
  const pipeline = resource(
    scope,
    "pipeline",
    "test",
    { inputs: [], sql: "SELECT 1", outputDatasetId: "absent" },
    "pipeline",
  );
  const job = resource(
    scope,
    "job",
    "test",
    { pipelineId: pipeline.id, status: "running" },
    "job",
  );
  await store.commit(scope, 0, [{ put: pipeline }, { put: job }]);
  let finish!: (value: any) => void;
  let signal!: AbortSignal;
  vi.mocked(queryDatasets).mockImplementation(
    async (_store, _scope, _inputs, _sql, options) => {
      signal = options!.signal!;
      return new Promise((resolve) => {
        finish = resolve;
      });
    },
  );
  const running = runPipelineJob(store, scope, job);
  await vi.advanceTimersByTimeAsync(0);
  return {
    store,
    scope,
    job,
    running,
    get signal() {
      return signal;
    },
    finish: () => finish({ version: "synthetic", profile: {} }),
  };
}
it("does not overlap slow cancellation reads and waits one second after completion", async () => {
  const f = await fixture();
  const get = f.store.get.bind(f.store);
  let release!: () => void;
  let reads = 0;
  vi.spyOn(f.store, "get").mockImplementation(async (scope, id) => {
    if (id === f.job.id) {
      reads++;
      await new Promise<void>((resolve) => {
        release = resolve;
      });
    }
    return get(scope, id);
  });
  await vi.advanceTimersByTimeAsync(1000);
  expect(reads).toBe(1);
  await vi.advanceTimersByTimeAsync(5000);
  expect(reads).toBe(1);
  release();
  await vi.advanceTimersByTimeAsync(0);
  await vi.advanceTimersByTimeAsync(999);
  expect(reads).toBe(1);
  await vi.advanceTimersByTimeAsync(1);
  expect(reads).toBe(2);
  release();
  await vi.advanceTimersByTimeAsync(0);
  vi.restoreAllMocks();
  f.finish();
  await f.running;
  expect(vi.getTimerCount()).toBe(0);
});
it("skips a failed status read without aborting the healthy build", async () => {
  const f = await fixture();
  const get = f.store.get.bind(f.store);
  let fail = true;
  vi.spyOn(f.store, "get").mockImplementation(async (scope, id) => {
    if (id === f.job.id && fail) {
      fail = false;
      throw new Error("transient read");
    }
    return get(scope, id);
  });
  await vi.advanceTimersByTimeAsync(1000);
  expect(f.signal.aborted).toBe(false);
  await vi.advanceTimersByTimeAsync(1000);
  expect(f.signal.aborted).toBe(false);
  f.finish();
  await f.running;
  expect(vi.getTimerCount()).toBe(0);
});
it("does not rearm a pending cancellation read after the build completes", async () => {
  const f = await fixture();
  const get = f.store.get.bind(f.store);
  let release!: () => void;
  let calls = 0;
  vi.spyOn(f.store, "get").mockImplementation(async (scope, id) => {
    if (id === f.job.id && calls++ === 0)
      await new Promise<void>((resolve) => {
        release = resolve;
      });
    return get(scope, id);
  });
  await vi.advanceTimersByTimeAsync(1000);
  f.finish();
  await f.running;
  release();
  await vi.advanceTimersByTimeAsync(0);
  expect(vi.getTimerCount()).toBe(0);
});
