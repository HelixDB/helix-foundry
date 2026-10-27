import "./usage.css";
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  CreditCard,
  LogOut,
  Monitor,
  Moon,
  Settings,
  Sun,
  Users,
} from "lucide-react";
import { LOCAL } from "../../../shared/deployment";
import { signOut } from "../../api";
import {
  DropdownMenu,
  MenuHeader,
  MenuItem,
  MenuLabel,
  MenuSeparator,
  cx,
} from "../../kit";
import { paths } from "../../paths";
import { themeOptions, useTheme } from "../../shell/theme";
import { useWorkspace, type ShellUser, type ShellWorkspace } from "../../ui";
import { initials, roleLabel } from "./people";
import { TIER_NAME, planQuery, planSummary } from "./plan";
import { usageQuery, usageView } from "./usage";

const themeIcons = { system: Monitor, light: Sun, dark: Moon };

// The workspace's plan beside Plan & billing. The menu renders its items only
// while open, so the plan is read then, from the cache Settings shares.
function PlanHint({ workspaceId }: { workspaceId: string }) {
  const plan = useQuery(planQuery(workspaceId));
  return plan.data ? TIER_NAME[planSummary(plan.data).tier] : null;
}

// This month's AI spend by task, read when the menu opens, as the plan is.
// Until it is read, and when it can't be, the rows show dashes, so the open
// menu keeps its height and place.
function Usage({ workspaceId }: { workspaceId: string }) {
  const usage = useQuery(usageQuery(workspaceId)),
    view = usageView(usage.data);
  return (
    <>
      <MenuLabel>Usage</MenuLabel>
      <div
        className="wk-usage"
        role="presentation"
        aria-busy={usage.isPending || undefined}
      >
        <div
          className="wk-usage-total"
          title={view.resets ? `Caps start again on ${view.resets}` : undefined}
        >
          <span>This month</span>
          <strong>
            {!usage.data && usage.isError ? "Unavailable" : view.total}
          </strong>
        </div>
        {view.rows.map((r) => (
          <div
            key={r.task}
            className={cx("wk-usage-row", r.within && "is-within")}
            title={
              r.held
                ? `${r.hint}. ${r.held} held for questions running.`
                : r.hint
            }
          >
            <span className="wk-usage-name">{r.label}</span>
            <span className="wk-usage-amount">
              {r.spent}
              {r.cap?.of && <small> {r.cap.of}</small>}
            </span>
            {r.cap && (
              <span
                className={cx("wk-usage-bar", `is-${r.cap.level}`)}
                aria-hidden
              >
                <span style={{ width: `${r.cap.fill * 100}%` }} />
              </span>
            )}
          </div>
        ))}
      </div>
      <MenuSeparator />
    </>
  );
}

// The signed-in user in the sidebar's footer: the workspace's settings,
// members and plan (only the plan while it is onboarding), this month's AI
// usage, the theme, and sign out. A local build has only its settings and
// the theme: it bills nothing.
export function AccountMenu({
  user,
  current,
  collapsed,
}: {
  user: ShellUser;
  current: ShellWorkspace;
  collapsed: boolean;
}) {
  const [theme, setTheme] = useTheme(),
    [leaving, setLeaving] = useState(false),
    { onboarding } = useWorkspace();
  return (
    <DropdownMenu
      label="Account"
      side="top"
      className="hf-user"
      triggerClassName="hf-user-trigger"
      trigger={
        <>
          <span className="hf-avatar" aria-hidden>
            {initials(user.name)}
          </span>
          {!collapsed && (
            <span className="hf-user-text">
              <strong>{user.name}</strong>
              <small>{user.email}</small>
            </span>
          )}
        </>
      }
    >
      <MenuHeader
        title={user.name}
        subtitle={`${roleLabel[current.role]} · ${current.name}`}
      />
      <MenuSeparator />
      {!onboarding && (
        <MenuItem icon={Settings} to={paths.settings()}>
          Settings
        </MenuItem>
      )}
      {!onboarding && !LOCAL && (
        <MenuItem icon={Users} to={paths.settings("members")}>
          Members
        </MenuItem>
      )}
      {!LOCAL && (
        <MenuItem
          icon={CreditCard}
          to={paths.settings("billing")}
          hint={<PlanHint workspaceId={current.id} />}
        >
          Plan & billing
        </MenuItem>
      )}
      <MenuSeparator />
      {!LOCAL && <Usage workspaceId={current.id} />}
      <MenuLabel>Theme</MenuLabel>
      {themeOptions.map(({ value, label }) => (
        <MenuItem
          key={value}
          icon={themeIcons[value]}
          checked={theme === value}
          onSelect={() => setTheme(value)}
        >
          {label}
        </MenuItem>
      ))}
      {!LOCAL && (
        <>
          <MenuSeparator />
          <MenuItem
            icon={LogOut}
            disabled={leaving}
            onSelect={() => {
              setLeaving(true);
              void signOut();
            }}
          >
            Sign out
          </MenuItem>
        </>
      )}
    </DropdownMenu>
  );
}
