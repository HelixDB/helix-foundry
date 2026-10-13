import {
  compactSample,
  compactValue,
  type DatasetProfile,
} from "../../../packages/shared/src/index.js";
import { now, resource, transaction, type Store } from "./store.js";
import { MIGRATIONS } from "./source-discovery.js";

// Releases before samples were compacted stored every sample value whole, on
// each dataset and each of its versions, so every list of datasets read them
// back. Once per workspace, each profile is compacted, one record per
// transaction, since a single old record can be near the request limit.
export async function compactStoredSamples(store: Store, scope: string) {
  if ((await store.get(scope, MIGRATIONS))?.data.storedSamples) return;
  for (const kind of ["dataset", "version"])
    for (const listed of await store.list(scope, kind)) {
      if (!compacted(listed.data.profile)) continue;
      await transaction(store, scope, async (tx) => {
        const r = await tx.get(listed.id);
        const profile = compacted(r?.data.profile);
        if (r && profile)
          tx.put({
            ...r,
            revision: r.revision + 1,
            data: { ...r.data, profile },
          });
      });
    }
  await transaction(store, scope, async (tx) => {
    const m = await tx.get(MIGRATIONS);
    tx.put(
      m
        ? {
            ...m,
            revision: m.revision + 1,
            data: { ...m.data, storedSamples: now() },
          }
        : resource(
            scope,
            "migrationState",
            "Migrations",
            { storedSamples: now() },
            MIGRATIONS,
          ),
    );
  });
}

// The compacted profile, or undefined when compacting changes nothing.
function compacted(profile: DatasetProfile | undefined) {
  if (!profile || !Array.isArray(profile.sample)) return undefined;
  const next: DatasetProfile = {
    ...profile,
    sample: compactSample(profile.sample),
    columns: (profile.columns || []).map((c) =>
      Array.isArray(c.examples)
        ? { ...c, examples: c.examples.map(compactValue) }
        : c,
    ),
  };
  return JSON.stringify(next) === JSON.stringify(profile) ? undefined : next;
}
