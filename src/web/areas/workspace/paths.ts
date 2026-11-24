// Workspace area URLs, merged into `paths` (src/web/paths.ts). Owned by the
// workspace area; do not import ../../paths here (it imports this file).

// /settings is General; the other tabs are pages of their own, because the
// WFE's Foundry billing return URL is https://<host>/settings/billing.
export type SettingsTab =
  | "ai"
  | "members"
  | "access"
  | "apps"
  | "billing"
  | "enrichment"
  | "sharing"
  | "slack";

export const workspacePaths = {
  settings: (tab?: SettingsTab) => "/settings" + (tab ? "/" + tab : ""),
  // Settings › Access with an agent's key's sources open
  // (?principal=service_credential:<id>, read by access-model's keyParam).
  keySources: (credentialId: string) =>
    "/settings/access?principal=service_credential:" +
    encodeURIComponent(credentialId),
  // Choose or create a workspace.
  workspaces: () => "/workspaces",
};
