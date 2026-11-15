// Automations area URLs, merged into `paths` (src/web/paths.ts). Owned by the
// automations area; do not import ../../paths here (it imports this file).

const enc = encodeURIComponent;

export const automationPaths = {
  // The overview: the workspace's runs of the last days and its automations.
  automations: () => "/automations",
  // A new one: the Assistant's composer, or, from a template (TEMPLATES'
  // ids, "blank" from scratch), a draft on the full canvas.
  newAutomation: (template?: string) =>
    "/automations/new" + (template ? "?template=" + enc(template) : ""),
  // One automation on its canvas: the page the Helix MCP's tools link to
  // (its `url`), where its owner edits and approves it. Its Runs and
  // Settings are tabs (?tab=).
  automation: (id: string, tab?: "runs" | "settings") =>
    "/automations/" + enc(id) + (tab ? "?tab=" + tab : ""),
  // One run, its steps coloured on the canvas: the page a notify email
  // links to.
  automationRun: (id: string, runId: string) =>
    "/automations/" + enc(id) + "/runs/" + enc(runId),
};

/** The automation a path opens (its canvas or one of its runs); undefined on the overview and the new page. */
export const openAutomation = (pathname: string) => {
  const id = /^\/automations\/([^/]+)/.exec(pathname)?.[1];
  return id && id !== "new" ? decodeURIComponent(id) : undefined;
};
