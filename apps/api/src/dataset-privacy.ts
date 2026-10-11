import type { Resource } from "../../../packages/shared/src/index.js";
import { digest } from "./security.js";
import type { Store } from "./store.js";

// Policy belongs to the stable dataset identity, independently of immutable
// version profiles and of pipeline generations that replace dataset metadata.
export const datasetPrivacyId = (id: string) => "dataset_privacy_" + digest(id);

export async function withDatasetPrivacy(
  store: Store,
  scope: string,
  dataset: Resource,
) {
  if (dataset.kind !== "dataset" || !dataset.data.profile?.columns)
    return dataset;
  const additionalColumns: string[] =
    (await store.get(scope, datasetPrivacyId(dataset.id)))?.data
      .additionalColumns || [];
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
}
