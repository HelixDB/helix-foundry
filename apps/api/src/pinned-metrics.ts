import { z } from "zod";
import {
  PinnedMetricSpecSchema,
  type PinnedMetric,
  type Resource,
} from "../../../packages/shared/src/index.js";
import { compileMetric, metricsDatasets, runMetric } from "./metrics.js";
import { assert } from "./security.js";
import { now, resource, transaction, type Store } from "./store.js";

const numeric = (type: string) =>
  /^(TINYINT|SMALLINT|INTEGER|BIGINT|HUGEINT|UTINYINT|USMALLINT|UINTEGER|UBIGINT|FLOAT|REAL|DOUBLE|DECIMAL|NUMERIC)/i.test(
    type,
  ) && !/STRUCT|MAP\(|\[|UNION\(|LIST|JSON/i.test(type);
const temporal = (type: string) =>
  /^(DATE|TIMESTAMP)/i.test(type) && !/\[|STRUCT|LIST|MAP/i.test(type);
function options(dataset: Resource) {
  const columns = dataset.data.profile.columns.filter((c: any) => !c.sensitive);
  return {
    id: dataset.id,
    name: dataset.name,
    numericColumns: columns
      .filter((c: any) => numeric(c.type))
      .map((c: any) => c.name),
    timeColumns: columns
      .filter((c: any) => temporal(c.type))
      .map((c: any) => c.name),
  };
}
async function evaluate(
  store: Store,
  scope: string,
  pin: Resource,
  datasets: Resource[],
): Promise<PinnedMetric> {
  const spec = PinnedMetricSpecSchema.parse(pin.data.spec);
  const dataset = datasets.find((d) => d.id === spec.datasetId);
  const choices = dataset && options(dataset);
  const valid =
    choices &&
    (spec.measure === "count" ||
      choices.numericColumns.includes(spec.column!)) &&
    (!spec.timeColumn || choices.timeColumns.includes(spec.timeColumn));
  const compiled = valid
    ? compileMetric(spec, dataset)
    : { error: "The dataset or selected columns are no longer available" };
  const sql = "sql" in compiled ? compiled.sql : "";
  const result =
    "error" in compiled
      ? { error: compiled.error }
      : await runMetric(
          store,
          scope,
          { id: pin.id, spec, sql },
          dataset!.data.activeVersion,
        );
  return {
    id: pin.id,
    revision: pin.revision,
    spec,
    sql,
    dataset: dataset?.name || spec.datasetId,
    version: dataset?.data.activeVersion || null,
    computedAt: new Date().toISOString(),
    result,
  };
}
export async function pinnedMetrics(store: Store, scope: string) {
  const datasets = await metricsDatasets(store, scope);
  const pins = (await store.list(scope, "pinnedMetric")).sort(
    (a, b) =>
      a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id),
  );
  return {
    datasets: datasets.map(options),
    items: await Promise.all(
      pins.map((pin) => evaluate(store, scope, pin, datasets)),
    ),
  };
}
export async function savePinnedMetric(
  store: Store,
  scope: string,
  body: unknown,
  id?: string,
) {
  const parsed = z
    .object({
      spec: PinnedMetricSpecSchema,
      revision: z.number().int().min(1).optional(),
    })
    .strict()
    .parse(body);
  const datasets = await metricsDatasets(store, scope),
    dataset = datasets.find((d) => d.id === parsed.spec.datasetId);
  assert(dataset, "Dataset unavailable", 404);
  const current = id ? await store.get(scope, id) : undefined;
  if (id) {
    assert(current?.kind === "pinnedMetric", "Metric unavailable", 404);
    assert(
      parsed.revision === current.revision,
      "Metric changed; reload before saving",
      409,
    );
  }
  const candidate = current
    ? {
        ...current,
        name: parsed.spec.title,
        revision: current.revision + 1,
        updatedAt: now(),
        data: { spec: parsed.spec },
      }
    : resource(scope, "pinnedMetric", parsed.spec.title, { spec: parsed.spec });
  const checked = await evaluate(store, scope, candidate, datasets);
  assert(
    !checked.result.error,
    checked.result.error || "Metric could not be computed",
    422,
  );
  await transaction(store, scope, async (tx) => {
    const latestDataset = await tx.get(dataset.id);
    assert(
      latestDataset?.revision === dataset.revision &&
        latestDataset.data.activeVersion === checked.version,
      "Data changed while validating; retry",
      409,
    );
    const eligible = await metricsDatasets(store, scope);
    assert(
      eligible.some((d) => d.id === dataset.id),
      "Dataset no longer available",
      409,
    );
    if (id) {
      const latest = await tx.get(id);
      assert(
        latest?.kind === "pinnedMetric" && latest.revision === parsed.revision,
        "Metric changed; reload before saving",
        409,
      );
    } else
      assert(
        (await tx.list("pinnedMetric")).length < 8,
        "A workspace can pin up to eight metrics",
        409,
      );
    await tx.put(candidate);
  });
  const saved = (await store.get(scope, candidate.id))!;
  return { ...checked, revision: saved.revision };
}
export async function removePinnedMetric(
  store: Store,
  scope: string,
  id: string,
  revision: number,
) {
  await transaction(store, scope, async (tx) => {
    const pin = await tx.get(id);
    assert(pin?.kind === "pinnedMetric", "Metric unavailable", 404);
    assert(
      pin.revision === revision,
      "Metric changed; reload before removing",
      409,
    );
    await tx.delete(id);
  });
  return { deleted: true };
}
