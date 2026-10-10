import { it, expect, vi } from "vitest";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
const query = vi.hoisted(() => ({ run: vi.fn() }));
vi.mock("../apps/api/src/datasets.js", () => ({ queryDatasets: query.run }));
import { runPipelineJob } from "../apps/api/src/pipeline-jobs.js";
async function fixture() {
  const store = new MemoryStore();
  await transaction(store, "fixture", async (tx) => {
    tx.put(
      resource(
        "fixture",
        "pipeline",
        "Orders",
        {
          inputs: ["input"],
          sql: "SELECT * FROM d_input",
          outputDatasetId: "output",
        },
        "pipeline",
      ),
    );
    tx.put(
      resource(
        "fixture",
        "dataset",
        "Orders",
        { versions: ["old"], activeVersion: "old" },
        "output",
      ),
    );
    tx.put(
      resource(
        "fixture",
        "job",
        "Build",
        { pipelineId: "pipeline", status: "running" },
        "job",
      ),
    );
  });
  return store;
}
function delayed() {
  let release!: (v: any) => void;
  let entered!: () => void;
  const start = new Promise<void>((r) => (entered = r));
  query.run.mockImplementation(() => {
    entered();
    return new Promise((r) => (release = r));
  });
  return {
    start,
    finish: () =>
      release({ version: "new", profile: { rows: 1, columns: [] }, rows: [] }),
  };
}
it("does not publish a result after cancellation while the query is running", async () => {
  const store = await fixture(),
    gate = delayed(),
    run = runPipelineJob(
      store,
      "fixture",
      (await store.get("fixture", "job"))!,
    );
  await gate.start;
  await transaction(store, "fixture", async (tx) => {
    const j = (await tx.get("job"))!;
    tx.update(j, { ...j.data, status: "canceled" });
  });
  gate.finish();
  await expect(run).rejects.toThrow("Pipeline build canceled");
  expect((await store.get("fixture", "output"))?.data.activeVersion).toBe(
    "old",
  );
  expect(await store.get("fixture", "new")).toBeUndefined();
});
