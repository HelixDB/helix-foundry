import { randomUUID } from "node:crypto";
import { writeFile, mkdir, rm } from "node:fs/promises";
import { resolve } from "node:path";
import { config } from "./config.js";
import { execute } from "./executor.js";
import { resource, transaction, now, type Store } from "./store.js";
import type { Resource } from "../../../packages/shared/src/index.js";
import {
  datasetTable,
  datasetAliases,
} from "../../../packages/shared/src/index.js";
import { assert, digest } from "./security.js";
import { SAMPLE_SET, sampleDatasets } from "./sample-data.js";
// A pipeline's output is registered as a derived dataset under this id.
export const pipelineId = (name: string) =>
  "pipeline_" + digest(name).slice(0, 16);
// Sample datasets loaded at once; the executor handles a few ingests in parallel.
export const SAMPLE_LOADS = 3;
export async function ingestFile(
  store: Store,
  scope: string,
  name: string,
  file: string,
  sourceId?: string,
  datasetId?: string,
  versionId?: string,
  schema?: { name: string; type: string }[],
  jobId?: string,
) {
  const version =
    versionId === jobId && jobId
      ? "snapshot_" + jobId
      : versionId || randomUUID();
  if (versionId && (await store.get(scope, version))) {
    const existing = datasetId ? await store.get(scope, datasetId) : undefined;
    if (existing) return existing;
  }
  const r = await execute({ mode: "ingest", scope, version, file, schema });
  let unchangedVersion: string | undefined;
  const committed = await transaction(store, scope, async (tx) => {
    if (jobId)
      assert(
        (await tx.get(jobId))?.data.status !== "canceled",
        "Import canceled",
        409,
      );
    const old = datasetId ? await tx.get(datasetId) : undefined;
    const ds =
      old ||
      resource(scope, "dataset", name, { sourceId, versions: [] }, datasetId);
    const oldProfile = old?.data.profile;
    const drift =
      oldProfile &&
      JSON.stringify(oldProfile.columns.map((x: any) => [x.name, x.type])) !==
        JSON.stringify(r.profile.columns.map((x) => [x.name, x.type]));
    // A refresh whose rows fingerprint the same as the active snapshot keeps
    // that snapshot: no new version, no new file, no change to the setup
    // signature. Otherwise every scheduled refresh of an unchanged table
    // would grow storage with identical copies, mark pending reviews stale
    // and abort publications that overlap the refresh.
    if (
      oldProfile &&
      !drift &&
      ds.data.activeVersion &&
      !ds.data.pendingVersion &&
      typeof oldProfile.fingerprint === "string" &&
      oldProfile.fingerprint === r.profile.fingerprint
    ) {
      unchangedVersion = version;
      return tx.put({
        ...ds,
        revision: ds.revision + 1,
        updatedAt: now(),
        data: {
          ...ds.data,
          ...(jobId ? { importJobId: jobId } : {}),
          sourceId: sourceId || ds.data.sourceId,
          table: datasetTable(ds.id),
          lastSync: now(),
        },
      });
    }
    const v = resource(
      scope,
      "version",
      name,
      { datasetId: ds.id, profile: r.profile, file: version + ".parquet" },
      version,
    );
    tx.put(v);
    const data = {
      ...ds.data,
      ...(jobId ? { importJobId: jobId } : {}),
      profile: drift ? oldProfile : r.profile,
      activeVersion: drift ? ds.data.activeVersion : version,
      pendingVersion: drift ? version : null,
      versions: [version, ...ds.data.versions],
      sourceId: sourceId || ds.data.sourceId,
      table: datasetTable(ds.id),
      lastSync: now(),
    };
    const updated = tx.put({
      ...ds,
      revision: ds.revision + 1,
      updatedAt: now(),
      data,
    });
    const qualityIssues = oldProfile
      ? r.profile.columns.flatMap((c) => {
          const before = oldProfile.columns.find((x: any) => x.name === c.name);
          return before
            ? [
                ...(before.primaryKey && !c.primaryKey
                  ? [`${c.name} lost uniqueness or contains nulls`]
                  : []),
                ...(c.nulls / Math.max(1, r.profile.rows) >
                before.nulls / Math.max(1, oldProfile.rows)
                  ? [`${c.name} null rate increased`]
                  : []),
              ]
            : [];
        })
      : [];
    // An untagged fingerprint is compared in its own format, so upgrading
    // alone does not report a refresh. It is either the legacy XOR-only
    // digest or, from the release that first counted duplicate rows, the v2
    // digest without its tag.
    if (
      oldProfile &&
      oldProfile.fingerprint !== r.profile.fingerprint &&
      "v2:" + oldProfile.fingerprint !== r.profile.fingerprint &&
      oldProfile.fingerprint !== r.legacyFingerprint
    )
      tx.put(
        resource(
          scope,
          "finding",
          drift ? "Schema changed: " + name : "Data refreshed: " + name,
          {
            datasetId: ds.id,
            version,
            kind: drift ? "schema" : "refresh",
            dismissed: false,
            evidence: {
              before: oldProfile.rows,
              after: r.profile.rows,
              qualityIssues,
              duplicateRows: r.profile.duplicateRows,
            },
            fingerprint: r.profile.fingerprint,
          },
        ),
      );
    return updated;
  });
  if (unchangedVersion)
    await rm(
      resolve(config.dataDir, "datasets", scope, unchangedVersion + ".parquet"),
      { force: true },
    );
  return committed;
}
export async function ingestRows(
  store: Store,
  scope: string,
  name: string,
  rows: unknown[],
  sourceId?: string,
  datasetId?: string,
) {
  assert(rows.length > 0, "Provide at least one row");
  const dir = resolve(config.dataDir, "incoming", scope);
  await mkdir(dir, { recursive: true });
  const file = randomUUID() + ".jsonl";
  try {
    await writeFile(
      resolve(dir, file),
      rows.map((r) => JSON.stringify(r)).join("\n"),
    );
    return await ingestFile(store, scope, name, file, sourceId, datasetId);
  } finally {
    // Rows ingestion regenerates this private input on every request; unlike
    // an uploaded file, no retry operation depends on keeping it.
    await rm(resolve(dir, file), { force: true }).catch(() => {});
  }
}
export async function datasetInputs(
  store: Store,
  scope: string,
  ids: string[],
) {
  const results = [],
    generation = (await store.get(scope, "workspace"))?.data.activeGeneration;
  for (const id of [...new Set(ids)]) {
    const ds = await store.get(scope, id);
    assert(
      ds?.kind === "dataset" &&
        ds.data.activeVersion &&
        (!ds.data.generation || ds.data.generation === generation),
      "Dataset is unavailable",
      404,
    );
    results.push({
      table: datasetTable(id),
      version: ds.data.activeVersion as string,
      aliases: datasetAliases(ds.name),
    });
  }
  return results;
}
export async function previewDatasetVersion(
  store: Store,
  scope: string,
  datasetId: string,
  versionId: string,
  opts: { offset?: number; limit?: number } = {},
) {
  const [input] = await datasetInputs(store, scope, [datasetId]);
  const ds = (await store.get(scope, datasetId))!;
  const version = await store.get(scope, versionId);
  assert(
    ds.data.versions.includes(versionId) &&
      version?.kind === "version" &&
      version.data.datasetId === datasetId,
    "Snapshot unavailable",
    404,
  );
  const result = await execute({
    mode: "query",
    scope,
    inputs: [{ ...input, version: versionId }],
    sql: `SELECT * FROM "${datasetTable(datasetId)}"`,
    version: randomUUID(),
    limit: opts.limit,
    offset: opts.offset,
  });
  return {
    ...result,
    snapshot: {
      id: versionId,
      createdAt: version.createdAt,
      profile: version.data.profile,
      status:
        versionId === ds.data.activeVersion
          ? "current"
          : versionId === ds.data.pendingVersion
            ? "pending"
            : "historical",
    },
    citations: [{ datasetId, version: versionId }],
    state: "dataset_snapshot",
  };
}
export async function queryDatasets(
  store: Store,
  scope: string,
  ids: string[],
  sql: string,
  opts: {
    limit?: number;
    offset?: number;
    persist?: boolean;
    filters?: {
      datasetId: string;
      column: string;
      value: string | number | boolean | null;
    }[];
  } = {},
) {
  const inputs = await datasetInputs(store, scope, ids);
  for (const filter of opts.filters || []) {
    assert(ids.includes(filter.datasetId), "Filter input is not declared");
    const ds = await store.get(scope, filter.datasetId);
    assert(
      ds?.data.profile.columns.some((c: any) => c.name === filter.column),
      "Unknown filter column",
    );
  }
  const filtered = inputs.map((input) => ({
    ...input,
    filters: (opts.filters || []).filter(
      (f) => datasetTable(f.datasetId) === input.table,
    ),
  }));
  const result = await execute({
    mode: "query",
    scope,
    inputs: filtered,
    sql,
    version: randomUUID(),
    limit: opts.limit,
    offset: opts.offset,
    persist: opts.persist,
  });
  return {
    ...result,
    citations: inputs.map((input, i) => ({
      datasetId: [...new Set(ids)][i],
      version: input.version,
    })),
    state: "dataset_snapshot",
  };
}

// A second request while samples load (a client resuming a slow load) waits for
// the running one instead of ingesting the same datasets again.
const seeding = new Map<string, Promise<unknown>>();
// With advance, setup moves to the Outcome step before loading so the client can
// show loading progress (onboardingState.sampleProgress) right away.
export async function seedDemo(
  store: Store,
  scope: string,
  opts: { advance?: boolean } = {},
) {
  const running = seeding.get(scope);
  const run = (running ? running.catch(() => {}) : Promise.resolve()).then(() =>
    seedSamples(store, scope, opts),
  );
  seeding.set(scope, run);
  try {
    return await run;
  } finally {
    if (seeding.get(scope) === run) seeding.delete(scope);
  }
}
async function seedSamples(
  store: Store,
  scope: string,
  opts: { advance?: boolean },
) {
  const samples = sampleDatasets();
  const sampleIds = await transaction(store, scope, async (tx) => {
    const workspace = (await tx.get("workspace"))!;
    const onboarding = {
      ...workspace.data.onboarding,
      ...(opts.advance ? { step: "outcome" } : {}),
    };
    if (workspace.data.sampleSet === SAMPLE_SET) {
      if (opts.advance) tx.update(workspace, { ...workspace.data, onboarding });
      return workspace.data.sampleDatasetIds as Record<string, string>;
    }
    // An earlier sample set is left in place but hidden from setup.
    const retired = Object.values(
      (workspace.data.sampleDatasetIds || {}) as Record<string, string>,
    );
    const ids = Object.fromEntries(
      samples.map((sample) => [sample.name, randomUUID()]),
    );
    for (const id of retired) {
      const old = await tx.get(id);
      if (old) tx.update(old, { ...old.data, retiredSample: true });
    }
    tx.update(workspace, {
      ...workspace.data,
      sampleSet: SAMPLE_SET,
      sampleDatasetIds: ids,
      onboarding: retired.length
        ? {
            ...onboarding,
            excludedIds: [
              ...new Set([...(onboarding.excludedIds || []), ...retired]),
            ],
          }
        : onboarding,
    });
    return ids;
  });
  // A few at a time, in order, so the loading feed fills from the top.
  const out: Resource[] = [];
  let next = 0;
  const worker = async () => {
    for (let i = next++; i < samples.length; i = next++) {
      const sample = samples[i];
      const existing = await store.get(scope, sampleIds[sample.name]);
      const dataset = existing?.data.activeVersion
        ? existing
        : await ingestRows(
            store,
            scope,
            sample.name,
            sample.rows,
            undefined,
            sampleIds[sample.name],
          );
      out[i] = await transaction(store, scope, async (tx) => {
        const current = (await tx.get(dataset.id))!;
        return current.data.sampleData
          ? current
          : tx.update(
              { ...current, description: sample.description },
              {
                ...current.data,
                sampleData: true,
                sampleSource: sample.source,
              },
            );
      });
    }
  };
  await Promise.all([...Array(SAMPLE_LOADS)].map(worker));
  await transaction(store, scope, async (tx) => {
    const workspace = (await tx.get("workspace"))!;
    const onboarding = workspace.data.onboarding || {};
    const excluded: string[] = onboarding.excludedIds || [];
    const restored = new Set(Object.values(sampleIds));
    if (excluded.some((id) => restored.has(id)))
      tx.update(workspace, {
        ...workspace.data,
        onboarding: {
          ...onboarding,
          excludedIds: excluded.filter((id) => !restored.has(id)),
          recommendationRunId: null,
          buildRunId: null,
          ontologySuggestion: null,
          ontologyRunId: null,
          ontology: null,
        },
      });
  });
  return out;
}
