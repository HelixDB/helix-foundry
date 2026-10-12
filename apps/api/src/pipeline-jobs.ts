// Pipeline Builder job handler (batch build), dispatched by the worker in jobs.ts.
import type { Resource } from "../../../packages/shared/src/index.js";
import { randomUUID } from "node:crypto";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { transaction, resource, type Store } from "./store.js";
import { queryDatasets } from "./datasets.js";
import { config } from "./config.js";
import { assert } from "./security.js";

export async function runPipelineJob(
  store: Store,
  scope: string,
  job: Resource,
) {
  const p = await store.get(scope, job.data.pipelineId);
  if (!p) throw new Error("Pipeline missing");
  const controller = new AbortController(),
    version = randomUUID();
  const canceled = () => new Error("Pipeline canceled");
  const check = async () => {
    if ((await store.get(scope, job.id))?.data.status === "canceled")
      controller.abort(canceled());
  };
  await check();
  controller.signal.throwIfAborted();
  const poll = setInterval(
    () => void check().catch((error) => controller.abort(error)),
    250,
  );
  let committed = false;
  try {
    const result = await queryDatasets(
      store,
      scope,
      p.data.inputs,
      p.data.sql,
      {
        persist: true,
        version,
        signal: controller.signal,
      },
    );
    await transaction(store, scope, async (tx) => {
      const current = await tx.get(job.id);
      assert(current?.data.status !== "canceled", "Pipeline canceled", 409);
      // A later publication reuses output dataset ids but creates new pipeline records.
      // Check inside the transaction so its optimistic retry also sees publication.
      if (
        p.data.generation &&
        (await tx.get("workspace"))?.data.activeGeneration !== p.data.generation
      )
        throw new Error("Pipeline generation is no longer active");
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
      // The output and terminal status commit together. A cancel that loses this
      // race reports that the job finished rather than claiming to undo a result.
      if (current)
        tx.update(current, {
          ...current.data,
          status: "succeeded",
          error: null,
          progress: { message: "Complete snapshot ready" },
        });
    });
    committed = true;
  } finally {
    clearInterval(poll);
    if (!committed)
      await rm(
        resolve(config.dataDir, "datasets", scope, version + ".parquet"),
        { force: true },
      );
  }
}
