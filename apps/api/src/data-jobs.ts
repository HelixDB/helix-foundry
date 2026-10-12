import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { config } from "./config.js";
// Data Connection job handlers (upload, sync), dispatched by the worker in jobs.ts.
import type { Resource } from "../../../packages/shared/src/index.js";
import { jobProgress } from "./setup-jobs.js";
import { prepareSourceSync } from "./source-sync.js";
import { transaction, now, type Store } from "./store.js";
import { importSource } from "./connectors.js";
import { ingestFile } from "./datasets.js";
import { unseal } from "./security.js";

export async function runUploadJob(store: Store, scope: string, job: Resource) {
  await jobProgress(store, scope, job.id, {
    message: "Importing and profiling complete file",
  });
  await ingestFile(
    store,
    scope,
    job.name,
    job.data.file,
    undefined,
    job.data.datasetId,
    job.id,
    undefined,
    job.id,
  );
}

export async function runSyncJob(store: Store, scope: string, job: Resource) {
  let source = await store.get(scope, job.data.sourceId);
  if (!source) throw new Error("Source missing");
  source = await prepareSourceSync(store, scope, source);
  const changeCounter = Number(source.data.sync?.changes || 0);
  const imported = await importSource(
    scope,
    source.data.kind,
    unseal(source.data.secret),
    (p) => jobProgress(store, scope, job.id, p),
  );
  try {
    const ds = await ingestFile(
      store,
      scope,
      source.name,
      imported.file,
      source.id,
      source.data.datasetId || "source_" + source.id,
      job.id,
      imported.schema,
      job.id,
    );
    await transaction(store, scope, async (tx) => {
      const s = await tx.get(source.id);
      if (s && s.data.secret === source.data.secret)
        tx.update(s, {
          ...s.data,
          datasetId: ds.id,
          lastSync: now(),
          lastError: null,
          sync: s.data.sync
            ? { ...s.data.sync, applied: changeCounter }
            : undefined,
        });
    });
  } finally {
    // A connector retry reads its source again. This completed input belongs
    // only to this attempt; keep original uploaded inputs in runUploadJob.
    await rm(resolve(config.dataDir, "incoming", scope, imported.file), {
      force: true,
    }).catch(() => {});
  }
}
