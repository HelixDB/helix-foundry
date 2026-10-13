import type { Resource } from "../../../packages/shared/src/index.js";
import { digest } from "./security.js";
import type { Store } from "./store.js";

// Policy belongs to the stable dataset identity, independently of immutable
// version profiles and of pipeline generations that replace dataset metadata.
export const datasetPrivacyId = (id: string) => "dataset_privacy_" + digest(id);

// The datasets with their privacy policies applied. Every policy is read in
// one batch, not with a request per dataset.
export async function withDatasetPrivacy(
  store: Store,
  scope: string,
  datasets: Resource[],
): Promise<Resource[]> {
  const profiled = datasets.filter(
    (d) => d.kind === "dataset" && d.data.profile?.columns,
  );
  const policies = await store.getMany(
    scope,
    profiled.map((d) => datasetPrivacyId(d.id)),
  );
  const policy = new Map<string, string[]>(
    profiled.map((d, i) => [d.id, policies[i]?.data.additionalColumns || []]),
  );
  return datasets.map((dataset) => {
    const additionalColumns = policy.get(dataset.id);
    if (!additionalColumns) return dataset;
    const hidden = new Set(additionalColumns);
    const profile = dataset.data.profile;
    return {
      ...dataset,
      data: {
        ...dataset.data,
        privacy: {
          additionalColumns,
          detectedColumns: profile.columns
            .filter((c: any) => c.sensitive)
            .map((c: any) => c.name),
        },
        profile: {
          ...profile,
          columns: profile.columns.map((c: any) => ({
            ...c,
            sensitive: c.sensitive || hidden.has(c.name),
          })),
        },
      },
    };
  });
}
