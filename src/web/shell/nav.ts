import {
  Cable,
  Settings,
  Shapes,
  Sparkles,
  Waypoints,
  type LucideIcon,
} from "lucide-react";
import type {
  AreaShell,
  NavChild,
  PaletteSource,
  SectionId,
  SectionPanel,
  SidebarSection,
} from "./types";
import { dataShell } from "../areas/data/shell";
import { ontologyShell } from "../areas/ontology/shell";
import { workspaceShell } from "../areas/workspace/shell";

export type NavItem = {
  id: SectionId;
  to: string;
  label: string;
  icon: LucideIcon;
  // Path prefixes that belong to this section (nested routes stay active).
  match: string[];
  // The section's sub-pages while `pathname` is open.
  children: (pathname: string) => NavChild[];
};
const areas: AreaShell[] = [dataShell, ontologyShell, workspaceShell];
const childrenOf = (id: SectionId) => (pathname: string) =>
  areas.flatMap((a) => {
    const sub = a.subnav?.[id];
    return typeof sub === "function" ? sub(pathname) : sub || [];
  });
const item = (
  id: SectionId,
  to: string,
  label: string,
  icon: LucideIcon,
  match: string[],
): NavItem => ({ id, to, label, icon, match, children: childrenOf(id) });

export const primaryNav: NavItem[] = [
  // Questions about an ontology's database, the main page (/ opens it), and
  // the database itself: each opens the ontology last opened, and picks
  // another in its header.
  item("ask", "/ask", "Ask", Sparkles, ["/ask"]),
  item("ontology", "/ontology", "Ontologies", Shapes, ["/ontology"]),
  item("explore", "/explore", "Explorer", Waypoints, ["/explore"]),
  item("data", "/data", "Connectors", Cable, ["/data"]),
];
export const secondaryNav: NavItem[] = [
  item("settings", "/settings", "Settings", Settings, ["/settings"]),
];
export const paletteSources: PaletteSource[] = areas.flatMap(
  (a) => a.palette || [],
);
// What the areas give each section, one area per section.
function bySection<T>(
  of: (a: AreaShell) => Partial<Record<SectionId, T>> | undefined,
  what: string,
): Partial<Record<SectionId, T>> {
  const entries = areas.flatMap((a) => Object.entries(of(a) || {}));
  if (new Set(entries.map(([id]) => id)).size !== entries.length)
    throw new Error(`Two areas give one section ${what}`);
  return Object.fromEntries(entries);
}
export const sectionPanels = bySection<SectionPanel>((a) => a.panel, "a panel");
export const sectionSidebars = bySection<SidebarSection>(
  (a) => a.sidebar,
  "sidebar content",
);

const under = (pathname: string, prefix: string) =>
  pathname === prefix || pathname.startsWith(prefix + "/");
export function activeSection(pathname: string): SectionId | undefined {
  // / opens Ask (main.tsx): its threads show while it does.
  if (pathname === "/") return "ask";
  return [...primaryNav, ...secondaryNav].find((n) =>
    n.match.some((m) => under(pathname, m)),
  )?.id;
}
export function childActive(pathname: string, child: NavChild) {
  const path = child.to.split("?")[0]!;
  return (
    (child.end ? pathname === path : under(pathname, path)) ||
    (child.match || []).some((m) => under(pathname, m))
  );
}
