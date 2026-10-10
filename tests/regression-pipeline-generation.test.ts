import { it, expect, vi } from "vitest";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { runPipelineJob } from "../apps/api/src/pipeline-jobs.js";
import { queryDatasets } from "../apps/api/src/datasets.js";

vi.mock("../apps/api/src/datasets.js", () => ({ queryDatasets: vi.fn() }));

it("refuses an old pipeline result after another generation publishes", async () => {
  const store = new MemoryStore();
  const scope = "generation-race";
  const oldProfile = { columns: [{ name: "old", type: "INTEGER" }] };
  const newProfile = { columns: [{ name: "new", type: "INTEGER" }] };
  await transaction(store, scope, async (tx) => {
    tx.put(
      resource(
        scope,
        "workspace",
        "Workspace",
        { activeGeneration: "old" },
        "workspace",
      ),
    );
    tx.put(
      resource(
        scope,
        "pipeline",
        "Output",
        {
          generation: "old",
          inputs: [],
          sql: "SELECT 1",
          outputDatasetId: "output",
        },
        "pipeline",
      ),
    );
    tx.put(
      resource(
        scope,
        "dataset",
        "Output",
        {
          generation: "old",
          activeVersion: "initial",
          versions: ["initial"],
          profile: oldProfile,
        },
        "output",
      ),
    );
  });
  let finish!: (value: any) => void;
  let started!: () => void;
  const running = new Promise<void>((resolve) => {
    started = resolve;
  });
  vi.mocked(queryDatasets).mockImplementationOnce(async () => {
    started();
    return new Promise((resolve) => {
      finish = resolve;
    });
  });
  const job = runPipelineJob(
    store,
    scope,
    resource(scope, "job", "Build", { pipelineId: "pipeline" }),
  );
  await running;
  await transaction(store, scope, async (tx) => {
    const workspace = (await tx.get("workspace"))!;
    tx.update(workspace, { activeGeneration: "new" });
    const output = (await tx.get("output"))!;
    tx.update(output, {
      generation: "new",
      activeVersion: "published",
      versions: ["published"],
      profile: newProfile,
    });
  });
  finish({ version: "obsolete", profile: oldProfile });
  await expect(job).rejects.toThrow("Pipeline generation is no longer active");
  expect((await store.get(scope, "output"))?.data).toEqual({
    generation: "new",
    activeVersion: "published",
    versions: ["published"],
    profile: newProfile,
  });
  expect(await store.get(scope, "obsolete")).toBeUndefined();
});

it.each(["current", undefined])(
  "publishes a current or legacy pipeline (%s)",
  async (generation) => {
    const store = new MemoryStore();
    const scope = "generation-control";
    const profile = { columns: [] };
    await transaction(store, scope, async (tx) => {
      tx.put(
        resource(
          scope,
          "workspace",
          "Workspace",
          { activeGeneration: "current" },
          "workspace",
        ),
      );
      tx.put(
        resource(
          scope,
          "pipeline",
          "Output",
          {
            generation,
            inputs: [],
            sql: "SELECT 1",
            outputDatasetId: "output",
          },
          "pipeline",
        ),
      );
      tx.put(
        resource(
          scope,
          "dataset",
          "Output",
          { generation, activeVersion: "initial", versions: ["initial"] },
          "output",
        ),
      );
    });
    vi.mocked(queryDatasets).mockResolvedValueOnce({
      version: "fresh",
      profile,
    } as any);
    await runPipelineJob(
      store,
      scope,
      resource(scope, "job", "Build", { pipelineId: "pipeline" }),
    );
    expect((await store.get(scope, "output"))?.data.activeVersion).toBe(
      "fresh",
    );
    expect((await store.get(scope, "fresh"))?.data.profile).toEqual(profile);
  },
);

it("rechecks generation when publication wins the optimistic commit", async () => {
  const store = new MemoryStore();
  const scope = "generation-commit-race";
  await transaction(store, scope, async (tx) => {
    tx.put(
      resource(
        scope,
        "workspace",
        "Workspace",
        { activeGeneration: "old" },
        "workspace",
      ),
    );
    tx.put(
      resource(
        scope,
        "pipeline",
        "Output",
        {
          generation: "old",
          inputs: [],
          sql: "SELECT 1",
          outputDatasetId: "output",
        },
        "pipeline",
      ),
    );
    tx.put(
      resource(
        scope,
        "dataset",
        "Output",
        { generation: "old", activeVersion: "initial", versions: ["initial"] },
        "output",
      ),
    );
  });
  const commit = store.commit.bind(store);
  vi.spyOn(store, "commit").mockImplementationOnce(async (s, version) => {
    const workspace = (await store.get(s, "workspace"))!;
    const output = (await store.get(s, "output"))!;
    expect(
      await commit(s, version, [
        { put: { ...workspace, data: { activeGeneration: "new" } } },
        {
          put: {
            ...output,
            data: {
              generation: "new",
              activeVersion: "published",
              versions: ["published"],
            },
          },
        },
      ]),
    ).toBe(true);
    return false;
  });
  vi.mocked(queryDatasets).mockResolvedValueOnce({
    version: "obsolete",
    profile: { columns: [] },
  } as any);
  await expect(
    runPipelineJob(
      store,
      scope,
      resource(scope, "job", "Build", { pipelineId: "pipeline" }),
    ),
  ).rejects.toThrow("Pipeline generation is no longer active");
  expect((await store.get(scope, "output"))?.data.activeVersion).toBe(
    "published",
  );
  expect(await store.get(scope, "obsolete")).toBeUndefined();
});
