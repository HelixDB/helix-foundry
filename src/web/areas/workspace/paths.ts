// Workspace area URLs, merged into `paths` (src/web/paths.ts). Owned by the
// workspace area; do not import ../../paths here (it imports this file).

// /settings is General; the other tabs are pages of their own, because the
// WFE's Foundry billing return URL is https://<host>/settings/billing.
export type SettingsTab = "ai" | "members" | "billing" | "enrichment";

export const workspacePaths = {
  settings: (tab?: SettingsTab) => "/settings" + (tab ? "/" + tab : ""),
  // Choose or create a workspace.
  workspaces: () => "/workspaces",
};
