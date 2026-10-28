// Data area URLs, merged into `paths` (apps/web/src/paths.ts). Owned by the
// data area; do not import ../../paths here (it imports this file).
const enc = encodeURIComponent;
export const dataPaths = {
  data: () => "/data",
  // Connections are cards on the Data page; ?add=1 opens the picker.
  connections: (add = false) => "/data" + (add ? "?add=1" : ""),
  connection: (sourceId: string) => "/data/connections/" + enc(sourceId),
  dataset: (datasetId: string, tab?: string) =>
    "/data/datasets/" + enc(datasetId) + (tab ? "?tab=" + enc(tab) : ""),
};
