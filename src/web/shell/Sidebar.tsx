import {
  Suspense,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Link, useLocation } from "react-router-dom";
import { Search, Settings, X } from "lucide-react";
import { SkeletonText, cx, useStoredState } from "../kit";
import { paths } from "../paths";
import { useWorkspace, type ShellUser, type ShellWorkspace } from "../ui";
import { AccountMenu } from "../areas/workspace/account-menu";
import { WorkspaceSwitcher } from "../areas/workspace/switcher";
import {
  activeSection,
  childActive,
  granteeNav,
  onboardingNav,
  primaryNav,
  sectionSidebars,
  type NavItem,
} from "./nav";
import { useShell } from "./context";
import { ChromeBoundary } from "./ErrorBoundary";
import {
  SECTION_MIN_HEIGHT,
  SECTION_OFFSET_KEY,
  clampSectionOffset,
  maxSectionOffset,
  storedSectionOffset,
} from "./section-offset";
import { ariaShortcut, shortcut } from "./shortcut";

// Settings sits beside the user in the footer rather than in the section list.
export function SettingsLink() {
  const { pathname } = useLocation(),
    active = activeSection(pathname) === "settings";
  return (
    <Link
      to={paths.settings()}
      className={cx(
        "hf-btn hf-btn--ghost hf-btn--md hf-btn--icon hf-settings-link",
        active && "is-active",
      )}
      aria-label="Settings"
      title="Settings"
      aria-current={active ? "page" : undefined}
    >
      <Settings size={17} aria-hidden />
    </Link>
  );
}
function NavLink({
  n,
  active,
  pathname,
  onNavigate,
}: {
  n: NavItem;
  active: boolean;
  pathname: string;
  onNavigate?: () => void;
}) {
  return (
    <Link
      to={n.to}
      onClick={onNavigate}
      className={cx("hf-nav-link", active && "is-active")}
      aria-current={
        active && (pathname === n.to || !n.children(pathname).length)
          ? "page"
          : undefined
      }
    >
      <n.icon size={17} aria-hidden />
      <span className="hf-nav-text">{n.label}</span>
    </Link>
  );
}

function NavLinks({
  items,
  label,
  onNavigate,
}: {
  items: NavItem[];
  label: string;
  // Closes the mobile drawer, also when a link opens the page already shown.
  onNavigate?: () => void;
}) {
  const { pathname } = useLocation(),
    section = activeSection(pathname);
  return (
    <ul className="hf-nav-list" aria-label={label}>
      {items.map((n) => {
        const active = section === n.id,
          children = n.children(pathname);
        return (
          <li key={n.id}>
            <NavLink
              n={n}
              active={active}
              pathname={pathname}
              onNavigate={onNavigate}
            />
            {active && !!children.length && (
              <ul className="hf-subnav">
                {children.map((c) => {
                  const on = childActive(pathname, c);
                  return (
                    <li key={c.to}>
                      <Link
                        to={c.to}
                        onClick={onNavigate}
                        className={cx("hf-subnav-link", on && "is-active")}
                        aria-current={on ? "page" : undefined}
                      >
                        <span>{c.label}</span>
                      </Link>
                    </li>
                  );
                })}
              </ul>
            )}
          </li>
        );
      })}
    </ul>
  );
}

// What the open section shows under the sections, with the line above it a
// handle: a pointer, a finger or the keys move it, between right under the
// sections and what the content keeps (section-offset.ts). What was chosen
// stays stored while a short window can't give it, so a taller one has it back.
function SectionSplit({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  const split = useRef<HTMLDivElement>(null),
    [stored, setStored] = useStoredState(SECTION_OFFSET_KEY, ""),
    [room, setRoom] = useState(0),
    [drag, setDrag] = useState<{ y: number; from: number } | null>(null),
    max = maxSectionOffset(room),
    offset = clampSectionOffset(storedSectionOffset(stored), room),
    move = (to: number) => setStored(String(clampSectionOffset(to, room)));
  // The offset is the split's own top padding, so its height doesn't follow it.
  useLayoutEffect(() => {
    const el = split.current!;
    setRoom(el.clientHeight);
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(() => setRoom(el.clientHeight));
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  return (
    <div
      ref={split}
      className="hf-sidebar-split"
      style={{ minHeight: SECTION_MIN_HEIGHT, paddingTop: offset }}
    >
      <div
        role="separator"
        className={cx("hf-sidebar-handle", drag && "is-dragging")}
        aria-orientation="horizontal"
        aria-label={`Resize ${label}`}
        aria-valuemin={0}
        aria-valuemax={max}
        aria-valuenow={offset}
        tabIndex={0}
        onPointerDown={(event) => {
          if (event.button !== 0) return;
          event.preventDefault();
          event.currentTarget.setPointerCapture(event.pointerId);
          setDrag({ y: event.clientY, from: offset });
        }}
        onPointerMove={(event) => {
          if (drag) move(drag.from + event.clientY - drag.y);
        }}
        onLostPointerCapture={() => setDrag(null)}
        onDoubleClick={() => setStored("")}
        onKeyDown={(event) => {
          const step = event.shiftKey ? 64 : 16,
            to =
              event.key === "ArrowUp"
                ? offset - step
                : event.key === "ArrowDown"
                  ? offset + step
                  : event.key === "Home"
                    ? 0
                    : event.key === "End"
                      ? max
                      : null;
          if (to === null) return;
          event.preventDefault();
          move(to);
        }}
      />
      <section className="hf-sidebar-section" aria-label={label}>
        {children}
      </section>
    </div>
  );
}

// The sidebar, on every page beside the page (and in a phone's drawer where
// the open section has no panel: Rail.tsx): the mark and search, the
// workspace, the sections with their names, what the open section shows
// under them (Ask's threads), then the account and settings. While the
// workspace is onboarding it holds only the mark, the workspace, the way
// back to the onboarding and the account; for a grantee, no search or
// settings, and only Analyst, Ontologies and the Explorer.
export function Sidebar({
  workspaces,
  current,
  user,
  onClose,
  onSwitch,
}: {
  workspaces: ShellWorkspace[];
  current: ShellWorkspace;
  user: ShellUser;
  // A visible close control.
  onClose?: () => void;
  onSwitch: (id: string) => void;
}) {
  const shell = useShell(),
    { onboarding, grantee } = useWorkspace(),
    section = activeSection(useLocation().pathname),
    extra = onboarding ? undefined : section && sectionSidebars[section];
  return (
    <div className="hf-sidebar-inner">
      <div className="hf-brand-row">
        <Link to="/" className="hf-brand" aria-label="Helix Foundry home">
          <span className="hf-brand-name">
            helix<span>foundry</span>
          </span>
        </Link>
        {!onboarding && !grantee && (
          <button
            type="button"
            className="hf-btn hf-btn--ghost hf-btn--sm hf-btn--icon hf-search-button"
            aria-label="Search"
            aria-keyshortcuts={ariaShortcut}
            title={`Search (${shortcut})`}
            onClick={() => shell.openPalette()}
          >
            <Search size={16} aria-hidden />
          </button>
        )}
        {onClose && (
          <button
            type="button"
            className="hf-btn hf-btn--ghost hf-btn--md hf-btn--icon"
            aria-label="Close navigation"
            title="Close navigation"
            data-autofocus
            onClick={onClose}
          >
            <X size={18} aria-hidden />
          </button>
        )}
      </div>
      <WorkspaceSwitcher
        workspaces={workspaces}
        current={current}
        onSwitch={onSwitch}
      />
      <nav
        aria-label="Main navigation"
        className={cx("hf-nav", extra && "has-section")}
      >
        <NavLinks
          items={onboarding ? onboardingNav : grantee ? granteeNav : primaryNav}
          label="Workspace"
          onNavigate={onClose}
        />
        {!extra && <div className="hf-nav-spacer" />}
      </nav>
      {extra && (
        <SectionSplit key={section} label={extra.label}>
          <ChromeBoundary
            name={extra.label}
            fallback={
              <p className="hf-section-panel-note">
                This part of the sidebar hit a problem. Reload to try again.
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
              <extra.component onNavigate={onClose} />
            </Suspense>
          </ChromeBoundary>
        </SectionSplit>
      )}
      <div className="hf-sidebar-foot">
        <AccountMenu user={user} current={current} collapsed={false} />
        {!onboarding && !grantee && <SettingsLink />}
      </div>
    </div>
  );
}
