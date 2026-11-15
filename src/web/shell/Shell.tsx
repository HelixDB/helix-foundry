import {
  Suspense,
  lazy,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Link, useLocation } from "react-router-dom";
import { Bot, Menu, Search } from "lucide-react";
import { trackEvent } from "../analytics";
import {
  Dialog,
  PageVisitContext,
  SkeletonText,
  cx,
  useMedia,
  useStoredState,
} from "../kit";
import { useWorkspace, type ShellUser, type ShellWorkspace } from "../ui";
import { CommandPalette } from "./CommandPalette";
import { ChromeBoundary } from "./ErrorBoundary";
import {
  PanelSlotContext,
  ShellContext,
  type AssistantFrom,
  type PanelSlot,
} from "./context";
import {
  ShellBanner,
  activeSection,
  primaryNav,
  secondaryNav,
  sectionPanels,
} from "./nav";
import { Rail } from "./Rail";
import { recordRecent } from "./recents";
import { Sidebar } from "./Sidebar";
import type { SectionPanel } from "./types";

// The Assistant (areas/agent): in this build (next.config.ts), its dock
// loaded once opened.
const ASSISTANT = !!process.env.NEXT_PUBLIC_FOUNDRY_ASSISTANT;
const AgentDock = lazy(() =>
  import("../areas/agent/dock").then((m) => ({ default: m.AgentDock })),
);
// Whether the dock is open on a wide screen, per browser ("open" or "").
const DOCK_KEY = "foundry.agent.dock";

// What the Assistant's dock shows while it loads, or if it fails.
const dockLoading = (
  <div className="hf-section-panel-note" aria-busy="true">
    <SkeletonText lines={4} />
  </div>
);
const dockFailed = (
  <p className="hf-section-panel-note">
    The Assistant hit a problem. Reload to try again.
  </p>
);

// Says where a navigation landed. When the old page took focus with it, focus
// moves to the new page's heading so keyboard users continue from there.
function RouteAnnouncer() {
  const { pathname } = useLocation(),
    [text, setText] = useState(""),
    first = useRef(true);
  useEffect(() => {
    if (first.current) {
      first.current = false;
      return;
    }
    // Lazy pages render a moment later.
    const timer = setTimeout(() => {
      const heading = document.querySelector<HTMLElement>("#main h1");
      setText(
        heading?.textContent?.trim() ||
          document.title.replace(/ · Helix Foundry$/, ""),
      );
      const active = document.activeElement;
      if (heading && (!active || active === document.body)) {
        if (!heading.hasAttribute("tabindex"))
          heading.setAttribute("tabindex", "-1");
        heading.focus({ preventScroll: true });
      }
    }, 250);
    return () => clearTimeout(timer);
  }, [pathname]);
  return (
    <div className="hf-sr-only" aria-live="polite" aria-atomic="true">
      {text}
    </div>
  );
}

// If the sidebar itself fails, plain links keep the app navigable.
function SidebarFallback() {
  return (
    <nav aria-label="Main navigation" className="hf-sidebar-fallback">
      {[...primaryNav, ...secondaryNav].map((n) => (
        <Link key={n.id} to={n.to}>
          {n.label}
        </Link>
      ))}
    </nav>
  );
}

// A section's panel as a landmark of its own that scrolls on its own; if it
// fails, the sidebar keeps the app navigable. A page panel holds the slot the
// page portals into, and shows its fallback while no page has claimed it,
// named as the page's content is once one has.
function PanelAside({
  panel,
  onNavigate,
  slotRef,
  claimed,
}: {
  panel: SectionPanel;
  onNavigate?: () => void;
  slotRef: (element: HTMLElement | null) => void;
  claimed: string | null;
}) {
  const page = panel.kind === "page",
    Own = page ? panel.fallback : panel.component;
  return (
    <aside
      className="hf-section-panel"
      aria-label={(page && claimed) || panel.label}
    >
      {page && <div ref={slotRef} className="hf-panel-slot" />}
      {!(page && claimed) && (
        <ChromeBoundary
          name={panel.label}
          fallback={
            <p className="hf-section-panel-note">
              This panel hit a problem. Reload to try again.
            </p>
          }
        >
          <Suspense
            fallback={
              <div className="hf-section-panel-note" aria-busy="true">
                <SkeletonText lines={4} />
              </div>
            }
          >
            <Own onNavigate={onNavigate} />
          </Suspense>
        </ChromeBoundary>
      )}
    </aside>
  );
}

// Workspace chrome: an area's banner across the top when it shows one, the
// sidebar on every page, with the open section's panel beside it when it has
// one, mobile bar and drawer, the command palette, and the Assistant's dock
// (⌘J, the sidebar's button or the palette): a column right of the page on
// a wide screen, kept open per browser, else a drawer (below 1100px, or
// 1400px beside a section's panel, so the page keeps its room). Closing the
// column gives the focus back to what opened it. A grantee has no
// Assistant (the WFE denies it every route). While the workspace is
// onboarding, the sidebar holds only the workspace and the account, and there
// is no palette; a grantee has no palette either (what it lists, a grantee
// can't read).
export function Shell({
  workspaces,
  current,
  user,
  onSwitch,
  children,
}: {
  workspaces: ShellWorkspace[];
  current: ShellWorkspace;
  user: ShellUser;
  onSwitch: (id: string) => void;
  children: ReactNode;
}) {
  const { notify, onboarding, grantee } = useWorkspace(),
    searchable = !onboarding && !grantee;
  const [palette, setPalette] = useState<{ query: string } | null>(null),
    [drawer, setDrawer] = useState(false);
  const { pathname } = useLocation();
  // The open section's panel, if it has one. It shows beside the sidebar;
  // the Explorer's [ (useShell().togglePanel) hides and shows it for the
  // visit, so another section, or a reload, has it back.
  const section = activeSection(pathname),
    panel = onboarding ? undefined : section && sectionPanels[section];
  const [panelHidden, setPanelHidden] = useState(false),
    panelOpen = !panelHidden;
  useEffect(() => setPanelHidden(false), [section]);
  const assistant = ASSISTANT && !grantee,
    dockDrawer = useMedia(
      panel && panelOpen ? "(max-width: 1400px)" : "(max-width: 1100px)",
    ),
    [dockStored, setDockStored] = useStoredState(DOCK_KEY, ""),
    [dockDrawerOpen, setDockDrawerOpen] = useState(false),
    dockOpen =
      assistant && (dockDrawer ? dockDrawerOpen : dockStored === "open");
  // What had the focus when the dock opened, which has it back when the
  // column closes (the drawer, a dialog, gives it back itself).
  const dockOpener = useRef<HTMLElement | null>(null),
    dockWasOpen = useRef(dockOpen);
  useEffect(() => {
    const closed = dockWasOpen.current && !dockOpen;
    dockWasOpen.current = dockOpen;
    if (!closed || dockDrawer) return;
    const focused = document.activeElement;
    if (focused && focused !== document.body) return;
    const back = dockOpener.current?.isConnected
      ? dockOpener.current
      : document.getElementById("main");
    back?.focus();
  }, [dockOpen, dockDrawer]);
  // A phone shows the panel in the drawer only.
  const narrow = useMedia("(max-width: 800px)");
  useEffect(() => {
    setDrawer(false);
    setPalette(null);
    setDockDrawerOpen(false);
  }, [pathname, current.id]);
  const closeDock = useCallback(
    () => (dockDrawer ? setDockDrawerOpen(false) : setDockStored("")),
    [dockDrawer, setDockStored],
  );
  const toggleDock = useCallback(
    (from: AssistantFrom) => {
      if (!dockOpen) {
        trackEvent("agent_dock_opened", { from });
        dockOpener.current =
          document.activeElement instanceof HTMLElement
            ? document.activeElement
            : null;
      }
      if (dockDrawer) setDockDrawerOpen((o) => !o);
      else setDockStored((s) => (s === "open" ? "" : "open"));
    },
    [dockOpen, dockDrawer, setDockStored],
  );
  useEffect(() => {
    if (!assistant) return;
    const key = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "j") {
        e.preventDefault();
        toggleDock("shortcut");
      }
    };
    window.addEventListener("keydown", key);
    return () => window.removeEventListener("keydown", key);
  }, [assistant, toggleDock]);
  // A page panel's slot, beside the sidebar or in the drawer (the drawer's
  // while it is open), and the label of the content a page put there.
  const [pageSlot, setPageSlot] = useState<HTMLElement | null>(null),
    [drawerSlot, setDrawerSlot] = useState<HTMLElement | null>(null),
    [claimed, setClaimed] = useState<string | null>(null);
  const claim = useCallback((label: string) => {
    setClaimed(label);
    return () => setClaimed((c) => (c === label ? null : c));
  }, []);
  const closeDrawer = useCallback(() => setDrawer(false), []);
  const slot = useMemo<PanelSlot | null>(() => {
    if (panel?.kind !== "page") return null;
    if (drawerSlot)
      return { element: drawerSlot, onNavigate: closeDrawer, claim };
    return pageSlot && { element: pageSlot, claim };
  }, [panel?.kind, drawerSlot, pageSlot, closeDrawer, claim]);
  useEffect(() => {
    if (!searchable) return;
    const key = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette((p) => (p ? null : { query: "" }));
      }
    };
    window.addEventListener("keydown", key);
    return () => window.removeEventListener("keydown", key);
  }, [searchable]);
  const shell = useMemo(
    () => ({
      openPalette: (query = "") => {
        setDrawer(false);
        setPalette({ query });
      },
      togglePanel: () =>
        narrow ? setDrawer((d) => !d) : setPanelHidden((h) => !h),
      assistant: { available: assistant, open: dockOpen, toggle: toggleDock },
    }),
    [narrow, assistant, dockOpen, toggleDock],
  );
  const visit = useCallback(
    (title: string) =>
      recordRecent(
        current.id,
        window.location.pathname + window.location.search,
        title,
      ),
    [current.id],
  );
  const closePalette = useCallback(() => setPalette(null), []);
  const sidebarProps = { workspaces, current, user, onSwitch };
  return (
    <ShellContext.Provider value={shell}>
      <PanelSlotContext.Provider value={slot}>
        <PageVisitContext.Provider value={visit}>
          <a className="hf-skip" href="#main">
            Skip to content
          </a>
          {ShellBanner && (
            <ChromeBoundary name="Banner">
              <Suspense fallback={null}>
                <ShellBanner />
              </Suspense>
            </ChromeBoundary>
          )}
          <div className="hf-shell">
            <aside className="hf-sidebar">
              <ChromeBoundary name="Sidebar" fallback={<SidebarFallback />}>
                <Sidebar {...sidebarProps} />
              </ChromeBoundary>
            </aside>
            {panel && panelOpen && !narrow && (
              <PanelAside
                key={section}
                panel={panel}
                slotRef={setPageSlot}
                claimed={claimed}
              />
            )}
            <div className="hf-main-col">
              <header className="hf-mobile-bar">
                <button
                  type="button"
                  className="hf-btn hf-btn--ghost hf-btn--md hf-btn--icon"
                  aria-label="Open navigation"
                  onClick={() => setDrawer(true)}
                >
                  <Menu size={18} aria-hidden />
                </button>
                <Link
                  to="/"
                  className="hf-brand"
                  aria-label="Helix Foundry home"
                >
                  <span className="hf-brand-name">
                    helix<span>foundry</span>
                  </span>
                </Link>
                <span className="hf-mobile-ws">{current.name}</span>
                {searchable && (
                  <button
                    type="button"
                    className="hf-btn hf-btn--ghost hf-btn--md hf-btn--icon"
                    aria-label="Search"
                    onClick={() => shell.openPalette()}
                  >
                    <Search size={17} aria-hidden />
                  </button>
                )}
                {assistant && (
                  <button
                    type="button"
                    className="hf-btn hf-btn--ghost hf-btn--md hf-btn--icon"
                    aria-label="Assistant"
                    aria-pressed={dockOpen}
                    onClick={() => toggleDock("button")}
                  >
                    <Bot size={17} aria-hidden />
                  </button>
                )}
              </header>
              <main className="hf-main" id="main" tabIndex={-1}>
                {children}
              </main>
            </div>
            {dockOpen && !dockDrawer && (
              <aside className="hf-assistant" aria-label="Assistant">
                <ChromeBoundary name="Assistant" fallback={dockFailed}>
                  <Suspense fallback={dockLoading}>
                    <AgentDock onClose={closeDock} />
                  </Suspense>
                </ChromeBoundary>
              </aside>
            )}
            {dockOpen && dockDrawer && (
              <Dialog
                title="Assistant"
                hideTitle
                variant="drawer"
                className="hf-assistant-drawer"
                onClose={closeDock}
              >
                <ChromeBoundary name="Assistant" fallback={dockFailed}>
                  <Suspense fallback={dockLoading}>
                    <AgentDock onClose={closeDock} onNavigate={closeDock} />
                  </Suspense>
                </ChromeBoundary>
              </Dialog>
            )}
            <RouteAnnouncer />
            {palette && (
              <ChromeBoundary
                name="Command palette"
                onError={() => {
                  closePalette();
                  notify("Search hit a problem. Try again.", true);
                }}
              >
                <CommandPalette
                  initialQuery={palette.query}
                  workspaces={workspaces}
                  current={current}
                  onSwitch={onSwitch}
                  onClose={closePalette}
                />
              </ChromeBoundary>
            )}
            {drawer && (
              <Dialog
                title="Navigation"
                hideTitle
                variant="drawer"
                className={cx("hf-nav-drawer", panel && "has-panel")}
                onClose={closeDrawer}
              >
                <ChromeBoundary
                  name="Navigation"
                  fallback={<SidebarFallback />}
                >
                  {panel ? (
                    <div className="hf-drawer-rail">
                      <Rail {...sidebarProps} onClose={closeDrawer} />
                      <PanelAside
                        key={section}
                        panel={panel}
                        onNavigate={closeDrawer}
                        slotRef={setDrawerSlot}
                        claimed={claimed}
                      />
                    </div>
                  ) : (
                    <Sidebar {...sidebarProps} onClose={closeDrawer} />
                  )}
                </ChromeBoundary>
              </Dialog>
            )}
          </div>
        </PageVisitContext.Provider>
      </PanelSlotContext.Provider>
    </ShellContext.Provider>
  );
}
