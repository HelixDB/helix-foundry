import { readStorage, writeStorage } from "../kit/util";
import { activeSection, primaryNav, secondaryNav } from "./nav";

// Recently opened resources, per viewer and workspace (this browser only).
export type Recent = { path: string; title: string; section: string };
const key = (workspaceId: string) => "foundry.recent.v1:" + workspaceId;
// Single-resource pages worth returning to. Each area adds its own, e.g.
// /^\/data\/connections\/[^/]+$/.
const resourceRoutes: RegExp[] = [];
// Section pages are not resources, whatever their title.
const generic = new Set(
  [...primaryNav, ...secondaryNav].flatMap((n) => [
    n.label,
    ...n.children(n.to).map((c) => c.label),
  ]),
);

export function readRecents(workspaceId: string): Recent[] {
  try {
    const list = JSON.parse(readStorage(key(workspaceId), "[]"));
    // Entries for pages that no longer exist (e.g. dashboards) are dropped.
    return Array.isArray(list)
      ? list.filter(
          (r) =>
            typeof r?.path === "string" &&
            typeof r?.title === "string" &&
            resourceRoutes.some((re) => re.test(r.path.split("?")[0])),
        )
      : [];
  } catch {
    return [];
  }
}
export function recordRecent(workspaceId: string, path: string, title: string) {
  const pathname = path.split("?")[0];
  if (
    !title ||
    generic.has(title) ||
    !resourceRoutes.some((r) => r.test(pathname))
  )
    return;
  const entry = { path, title, section: activeSection(pathname) || "" };
  writeStorage(
    key(workspaceId),
    JSON.stringify(
      [
        entry,
        ...readRecents(workspaceId).filter(
          (r) => r.path.split("?")[0] !== pathname,
        ),
      ].slice(0, 8),
    ),
  );
}
export const sectionIcon = (section: string) =>
  [...primaryNav, ...secondaryNav].find((n) => n.id === section)?.icon;
