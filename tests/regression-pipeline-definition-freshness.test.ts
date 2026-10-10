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
it.each(["edit", "delete"])(
  "does not publish a %s-superseded pipeline result",
  async (action) => {
    const store = await fixture(),
      gate = delayed(),
      run = runPipelineJob(
        store,
        "fixture",
        (await store.get("fixture", "job"))!,
      );
    await gate.start;
    await transaction(store, "fixture", async (tx) => {
      const p = (await tx.get("pipeline"))!;
      if (action === "delete") tx.delete(p.id);
      else tx.update(p, { ...p.data, sql: "SELECT 2" });
    });
    gate.finish();
    await expect(run).rejects.toThrow("Pipeline changed during build");
    expect((await store.get("fixture", "output"))?.data.activeVersion).toBe(
      "old",
    );
    expect(await store.get("fixture", "new")).toBeUndefined();
  },
);
it("publishes an unchanged definition", async () => {
  const store = await fixture();
  query.run.mockResolvedValue({
    version: "new",
    profile: { rows: 1, columns: [] },
    rows: [],
  });
  await runPipelineJob(store, "fixture", (await store.get("fixture", "job"))!);
  expect((await store.get("fixture", "output"))?.data.activeVersion).toBe(
    "new",
  );
});
