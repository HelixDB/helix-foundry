import type { ComponentType } from "react";
import type { LucideIcon } from "lucide-react";

// Sidebar sections, in shell/nav.ts.
export type SectionId = "data" | "ontology" | "explore" | "ask" | "settings";

// A sub-page shown under its section in the sidebar while the section is active.
export type NavChild = {
  to: string;
  label: string;
  // Path prefixes that mark this child active; defaults to [to].
  match?: string[];
  // Match `to` exactly instead of as a prefix.
  end?: boolean;
};

// One result in the command palette.
export type PaletteItem = {
  id: string;
  title: string;
  subtitle?: string;
  // Also searched, never shown.
  description?: string;
  href: string;
};

// One group of command palette results. `load` runs inside the shell when
// the palette opens (cached for 30 s): keep it to one WFE list call.
export type PaletteSource = {
  id: string;
  // Group heading, e.g. "Connections".
  label: string;
  icon: LucideIcon;
  load: (workspaceId: string) => Promise<PaletteItem[]>;
};

// What a section's panel renders itself: links call `onNavigate`, set in the
// phone's drawer, as it closes the drawer even when a link changes only the
// query string. Lazy (React.lazy) to keep the area's code out of the shell's
// bundle.
type PanelComponent = ComponentType<{ onNavigate?: () => void }>;

// A section's own panel. While its section is open, the panel shows beside
// the sidebar (on a phone, the drawer shows it beside the rail of section
// icons), scrolling on its own, as an <aside> named by `label`.
// - "component": the shell renders `component` outside the page's routes,
//   so it reads what it shows from the URL and the query cache.
// - "page": the open page fills it with what only the page knows (state of
//   its own), through <PanelContent> (shell/context.ts), which portals into
//   the panel while it shows and names it; `fallback`, a component as
//   above, shows while no page does (loading, or a page with nothing there).
export type SectionPanel =
  | { kind: "component"; label: string; component: PanelComponent }
  | { kind: "page"; label: string; fallback: PanelComponent };

// What a section shows in the sidebar itself, under the sections, while it
// is open (Ask's threads): `component`, rendered outside the page's routes
// as a "component" panel is, in a region named by `label` that fills the
// rest of the sidebar and scrolls on its own.
export type SidebarSection = { label: string; component: PanelComponent };

// What each area contributes to the shell: sidebar children per section (an
// area may fill several sections), fixed or from the page's path (the pages
// of one ontology), a section's panel or what it shows in the sidebar, and
// palette groups.
export type AreaShell = {
  subnav?: Partial<
    Record<SectionId, NavChild[] | ((pathname: string) => NavChild[])>
  >;
  panel?: Partial<Record<SectionId, SectionPanel>>;
  sidebar?: Partial<Record<SectionId, SidebarSection>>;
  palette?: PaletteSource[];
};
