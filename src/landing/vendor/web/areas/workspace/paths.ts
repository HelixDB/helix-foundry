// Workspace area URLs, merged into `paths` (apps/web/src/paths.ts). Owned by
// the workspace area; do not import ../../paths here (it imports this file).
const enc = encodeURIComponent;
export const workspacePaths = {
  proposals: (selected?: string) =>
    "/proposals" + (selected ? "?selected=" + enc(selected) : ""),
  settings: (tab?: string) => "/settings" + (tab ? "?tab=" + enc(tab) : ""),
};
