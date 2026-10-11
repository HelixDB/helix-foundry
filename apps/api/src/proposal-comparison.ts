import {
  BundleSchema,
  type Bundle,
} from "../../../packages/shared/src/index.js";
import { assert } from "./security.js";
import type { Store } from "./store.js";

const groups = [
  "catalog",
  "pipelines",
  "ontology",
  "relationships",
  "dashboards",
  "actions",
] as const;
const canonical = (value: any): any =>
  Array.isArray(value)
    ? value.map(canonical)
    : value && typeof value === "object"
      ? Object.fromEntries(
          Object.keys(value)
            .sort()
            .map((key) => [key, canonical(value[key])]),
        )
      : value;
const equal = (a: unknown, b: unknown) =>
  JSON.stringify(canonical(a)) === JSON.stringify(canonical(b));
const keyOf = (kind: string, item: any) =>
  kind === "catalog" ? item.datasetId : item.name;
export async function proposalComparison(
  store: Store,
  scope: string,
  id: string,
) {
  const proposal = await store.get(scope, id);
  assert(proposal?.kind === "proposal", "Proposal unavailable", 404);
  const workspace = await store.get(scope, "workspace"),
    activeGeneration = workspace?.data.activeGeneration || null;
  const published = activeGeneration
    ? (await store.list(scope, "proposal")).find(
        (p) => p.data.publishedGeneration === activeGeneration,
      )
    : undefined;
  assert(
    !activeGeneration || published,
    "Published definitions unavailable",
    409,
  );
  const current: Bundle = BundleSchema.parse(
    published?.data.bundle || {
      summary: "",
      assumptions: [],
      catalog: [],
      pipelines: [],
      ontology: [],
      relationships: [],
      dashboards: [],
      actions: [],
    },
  );
  const proposed = BundleSchema.parse(proposal.data.bundle),
    changes: any[] = [];
  for (const kind of groups) {
    const old = current[kind],
      next = proposed[kind];
    for (const items of [old, next])
      assert(
        new Set(items.map((item) => keyOf(kind, item))).size === items.length,
        `Comparison needs unique ${kind} names. Retest the proposal.`,
        422,
      );
    const before = new Map(old.map((item) => [keyOf(kind, item), item])),
      after = new Map(next.map((item) => [keyOf(kind, item), item]));
    for (const name of [
      ...new Set([...before.keys(), ...after.keys()]),
    ].sort()) {
      const a = before.get(name),
        b = after.get(name);
      if (equal(a, b)) continue;
      changes.push({
        kind,
        name,
        status: !a ? "added" : !b ? "removed" : "changed",
        before: a || null,
        after: b || null,
        fields: [...new Set([...Object.keys(a || {}), ...Object.keys(b || {})])]
          .filter((field) => !equal((a as any)?.[field], (b as any)?.[field]))
          .sort(),
      });
    }
  }
  const staleInputs = [];
  for (const [datasetId, validatedVersion] of Object.entries(
    proposal.data.validation.inputVersions || {},
  )) {
    const currentVersion =
      (await store.get(scope, datasetId))?.data.activeVersion || null;
    if (currentVersion !== validatedVersion)
      staleInputs.push({ datasetId, validatedVersion, currentVersion });
  }
  const validatedGeneration = proposal.data.validation.baseGeneration || null;
  return {
    proposalId: proposal.id,
    proposalRevision: proposal.revision,
    activeGeneration,
    validatedGeneration,
    stale: activeGeneration !== validatedGeneration || staleInputs.length > 0,
    staleInputs,
    changes,
    counts: {
      added: changes.filter((c) => c.status === "added").length,
      changed: changes.filter((c) => c.status === "changed").length,
      removed: changes.filter((c) => c.status === "removed").length,
    },
    metadata: ["summary", "assumptions"]
      .filter(
        (field) => !equal((current as any)[field], (proposed as any)[field]),
      )
      .map((field) => ({
        field,
        before: (current as any)[field],
        after: (proposed as any)[field],
      })),
  };
}
