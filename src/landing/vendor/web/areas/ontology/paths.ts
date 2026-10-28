// Ontology area URLs, merged into `paths` (apps/web/src/paths.ts). Owned by
// the ontology area; do not import ../../paths here (it imports this file).
const enc = encodeURIComponent;
export const ontologyPaths = {
  ontology: () => "/ontology",
  objectType: (typeId: string) => "/ontology/types/" + enc(typeId),
  // The graph; with a type, that type's records in the table.
  explorer: (typeName?: string) =>
    "/ontology/explore" + (typeName ? "?view=table&type=" + enc(typeName) : ""),
  explorerTable: () => "/ontology/explore?view=table",
  object: (logicalId: string, typeName?: string) =>
    "/records/" + enc(logicalId) + (typeName ? "?type=" + enc(typeName) : ""),
};
