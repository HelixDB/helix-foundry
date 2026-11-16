// Workflows URLs (the automations area), merged into `paths`
// (src/web/paths.ts). Owned by the automations area; do not import
// ../../paths here (it imports this file).

const enc = encodeURIComponent;

export const automationPaths = {
  // The overview: the workspace's runs of the last days and its workflows.
  automations: () => "/workflows",
  // A new one: Founder's composer, or, from a template (TEMPLATES'
  // ids, "blank" from scratch), a draft on the full canvas.
  newAutomation: (template?: string) =>
    "/workflows/new" + (template ? "?template=" + enc(template) : ""),
  // One workflow on its canvas: the page the Helix MCP's tools link to
  // (its `url`), where its owner edits and approves it. Its Runs and
  // Settings are tabs (?tab=).
  automation: (id: string, tab?: "runs" | "settings") =>
    "/workflows/" + enc(id) + (tab ? "?tab=" + tab : ""),
  // One run, its steps coloured on the canvas: the page a notify email
  // links to.
  automationRun: (id: string, runId: string) =>
    "/workflows/" + enc(id) + "/runs/" + enc(runId),
};

/** The workflow a path opens (its canvas or one of its runs); undefined on the overview and the new page. */
export const openAutomation = (pathname: string) => {
  const id = /^\/workflows\/([^/]+)/.exec(pathname)?.[1];
  return id && id !== "new" ? decodeURIComponent(id) : undefined;
};
