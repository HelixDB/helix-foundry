import { Link, useLocation } from "react-router-dom";
import { Search, X } from "lucide-react";
import { cx } from "../kit";
import { useWorkspace, type ShellUser, type ShellWorkspace } from "../ui";
import { AccountMenu } from "../areas/workspace/account-menu";
import { WorkspaceSwitcher } from "../areas/workspace/switcher";
import { useShell } from "./context";
import { activeSection, primaryNav } from "./nav";
import { ariaShortcut, shortcut } from "./shortcut";
import { SettingsLink } from "./Sidebar";

/** Helix's mark (helix-website's favicon), in the text's color. */
function HelixMark() {
  return (
    <svg viewBox="0 0 428 428" aria-hidden>
      <path d="M0 293L67 248V428H0V293Z" />
      <path d="M428 135.75L361 181V0H428V135.75Z" />
      <path d="M67 181H361V248H67V181Z" />
      <path d="M0 135.75L67 181V157H277V146H338.5V135H67V111H216V100H277.5V89H67V65H153V54H214.5V43H67V22H110V11H171.5V0L67 0H0V135.75Z" />
      <path d="M428 293L361 248V272H152.387V283H91V294H361V318H212.84V329H151V340H361V363H276.5V374H215V385H361V406H318.794V417H257V428H361H428V293Z" />
    </svg>
  );
}

/**
 * The sidebar as a column of icons, in a phone's drawer beside the open
 * section's panel (beside the page, the sidebar has the sections' names:
 * Sidebar.tsx). The mark, search and the drawer's close button, which every
 * link presses too, a link per section, then the workspace, the account and
 * settings. While the workspace is onboarding it holds only the mark, the
 * workspace and the account.
 */
export function Rail({
  workspaces,
  current,
  user,
  onSwitch,
  onClose,
}: {
  workspaces: ShellWorkspace[];
  current: ShellWorkspace;
  user: ShellUser;
  onSwitch: (id: string) => void;
  onClose: () => void;
}) {
  const shell = useShell(),
    { onboarding } = useWorkspace(),
    section = activeSection(useLocation().pathname);
  return (
    <div className="hf-rail">
      <Link
        to="/"
        className="hf-rail-mark"
        aria-label="Helix Foundry home"
        title="Helix Foundry"
        onClick={onClose}
      >
        <HelixMark />
      </Link>
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
      {!onboarding && (
        <button
          type="button"
          className="hf-btn hf-btn--ghost hf-btn--md hf-btn--icon hf-search-button"
          aria-label="Search"
          aria-keyshortcuts={ariaShortcut}
          title={`Search (${shortcut})`}
          onClick={() => shell.openPalette()}
        >
          <Search size={17} aria-hidden />
        </button>
      )}
      {onboarding ? (
        <div className="hf-nav-spacer" />
      ) : (
        <nav aria-label="Main navigation" className="hf-rail-nav">
          <ul className="hf-rail-list" aria-label="Workspace">
            {primaryNav.map((n) => {
              const active = section === n.id;
              return (
                <li key={n.id}>
                  <Link
                    to={n.to}
                    onClick={onClose}
                    className={cx("hf-rail-link", active && "is-active")}
                    aria-label={n.label}
                    title={n.label}
                    aria-current={active ? "page" : undefined}
                  >
                    <n.icon size={18} aria-hidden />
                  </Link>
                </li>
              );
            })}
          </ul>
        </nav>
      )}
      <div className="hf-rail-foot">
        <WorkspaceSwitcher
          workspaces={workspaces}
          current={current}
          onSwitch={onSwitch}
        />
        <AccountMenu user={user} current={current} collapsed />
        {!onboarding && <SettingsLink />}
      </div>
    </div>
  );
}
