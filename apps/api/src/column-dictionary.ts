import type {
  ColumnDefinition,
  ColumnDictionary,
  Resource,
} from "../../../packages/shared/src/index.js";
import { digest } from "./security.js";
import type { Store } from "./store.js";

export const columnDictionaryId = (datasetId: string) =>
  "column_dictionary_" + digest(datasetId);
export async function withColumnDictionary(
  store: Store,
  scope: string,
  dataset: Resource,
): Promise<Resource> {
  const saved = await store.get(scope, columnDictionaryId(dataset.id));
  return {
    ...dataset,
    data: { ...dataset.data, columnDefinitions: saved?.data.definitions || [] },
  };
}

export function activeColumnDefinitions(dataset: Resource): ColumnDefinition[] {
  const names = new Set(dataset.data.profile.columns.map((c: any) => c.name));
  return (dataset.data.columnDefinitions || []).filter((d: ColumnDefinition) =>
    names.has(d.column),
  );
}
export function columnDictionary(
  dataset: Resource,
  saved?: Resource,
): ColumnDictionary {
  return {
    datasetId: dataset.id,
    revision: saved?.revision || 0,
    datasetRevision: dataset.revision,
    columns: dataset.data.profile.columns.map((c: any) => ({
      name: c.name,
      type: c.type,
    })),
    definitions: saved?.data.definitions || [],
  };
}
