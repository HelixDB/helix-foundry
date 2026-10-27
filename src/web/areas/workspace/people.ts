import type { WorkspaceRole } from "../../ui";

// How the workspace area names people and their roles.

export const roleLabel: Record<WorkspaceRole, string> = {
  owner: "Owner",
  admin: "Admin",
  member: "Member",
};

/** Up to two initials for an avatar: "Ada Lovelace" is "AL". */
export const initials = (name: string) =>
  name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((w) => w[0]!.toUpperCase())
    .join("") || "?";
