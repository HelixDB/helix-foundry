// Dashboards area URLs, merged into `paths` (src/web/paths.ts). Owned by the
// dashboards area; do not import ../../paths here (it imports this file).

const enc = encodeURIComponent;

export const dashboardPaths = {
  // The workspace's dashboards, pinned ones first.
  dashboards: () => "/dashboards",
  // One dashboard: the page the Helix MCP's tools link to (its `url`).
  dashboard: (id: string) => "/dashboards/" + enc(id),
  // A published dashboard's public page, on this site's origin: no app,
  // no session.
  shared: (token: string) => "/share/" + enc(token),
};
