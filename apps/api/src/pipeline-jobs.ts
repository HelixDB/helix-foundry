// Pipeline Builder job handler (batch build), dispatched by the worker in jobs.ts.
import type { Resource } from "../../../packages/shared/src/index.js";
import { transaction, resource, type Store } from "./store.js";
import { queryDatasets } from "./datasets.js";
import { assert } from "./security.js";

export async function runPipelineJob(
  store: Store,
  scope: string,
  job: Resource,
) {
  const p = await store.get(scope, job.data.pipelineId);
  if (!p) throw new Error("Pipeline missing");
  const result = await queryDatasets(store, scope, p.data.inputs, p.data.sql, {
    persist: true,
  });
  await transaction(store, scope, async (tx) => {
    const current = await tx.get(p.id);
    assert(
      current?.kind === "pipeline" &&
        current.revision === p.revision &&
        current.data.outputDatasetId === p.data.outputDatasetId,
      "Pipeline changed during build",
      409,
    );
    const ds = await tx.get(p.data.outputDatasetId);
    if (ds) {
      tx.put(
        resource(
          scope,
          "version",
          ds.name,
          {
            datasetId: ds.id,
            profile: result.profile,
            file: result.version + ".parquet",
          },
          result.version,
        ),
      );
      tx.update(ds, {
        ...ds.data,
        profile: result.profile,
        activeVersion: result.version,
        versions: [result.version, ...ds.data.versions],
      });
    }
  });
}
