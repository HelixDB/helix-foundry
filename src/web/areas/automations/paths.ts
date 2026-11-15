// Automations area URLs, merged into `paths` (src/web/paths.ts). Owned by the
// automations area; do not import ../../paths here (it imports this file).

const enc = encodeURIComponent;

export const automationPaths = {
  // The workspace's automations, and the templates a new one starts from.
  automations: () => "/automations",
  // The builder of a new one, from a template (TEMPLATES' ids) or blank.
  newAutomation: (template?: string) =>
    "/automations/new" + (template ? "?template=" + enc(template) : ""),
  // One automation: the page the Helix MCP's tools link to (its `url`),
  // where its owner approves it.
  automation: (id: string) => "/automations/" + enc(id),
  // Its builder.
  editAutomation: (id: string) => "/automations/" + enc(id) + "/edit",
  // One run, its steps on the canvas: the page a notify email links to.
  automationRun: (id: string, runId: string) =>
    "/automations/" + enc(id) + "/runs/" + enc(runId),
};
