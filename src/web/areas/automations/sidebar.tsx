import "./automations.css";
import { Link, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import type { FoundryAutomation } from "../../../shared/wfe";
import {
  ButtonLink,
  SkeletonText,
  StatusDot,
  cx,
  formatRelative,
  useNow,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { automationPaths as paths, openAutomation } from "./paths";
import { automationsUnavailable } from "./model";
import { automationQueries } from "./queries";
import { automationStatus } from "./parts";

// The Automations section's list, in the shell's sidebar under the
// sections while an automation or one of its runs is open (shell.ts): New
// automation, then the workspace's automations by recency, each with its
// status as a dot, to go between them. The shell renders it outside the
// page's routes: it reads the open automation from the path and the list
// from the query the pages share.

export function AutomationList({ onNavigate }: { onNavigate?: () => void }) {
  const { id: ws } = useWorkspace(),
    { pathname } = useLocation(),
    now = useNow(60_000);
  const list = useQuery(automationQueries.list(ws));
  const open = openAutomation(pathname);
  const all = list.data?.automations || [];
  const row = (a: FoundryAutomation) => {
    const on = a.id === open,
      status = automationStatus(a);
    return (
      <li key={a.id}>
        <Link
          to={paths.automation(a.id)}
          onClick={onNavigate}
          className={cx("au-item", on && "is-active")}
          aria-current={on ? "page" : undefined}
          title={a.name}
        >
          <span className="au-item-title">
            <StatusDot status={status.dot} label={status.label} />
            <span>{a.name || "Untitled"}</span>
          </span>
          <small>
            {a.lastRun?.createdAt
              ? `Ran ${formatRelative(a.lastRun.createdAt, now)}`
              : "Never run"}
          </small>
        </Link>
      </li>
    );
  };
  return (
    <div className="au-panel">
      {list.data?.canCreate && (
        <div className="au-panel-head">
          <ButtonLink
            to={paths.newAutomation()}
            icon={Plus}
            className="au-new"
            onClick={onNavigate}
          >
            New automation
          </ButtonLink>
        </div>
      )}
      <nav className="au-list" aria-label="Automations">
        {list.isPending ? (
          <div className="au-note" aria-busy="true">
            <SkeletonText lines={3} />
          </div>
        ) : list.isError && automationsUnavailable(list.error) ? (
          <p className="au-note">Automations aren’t available here.</p>
        ) : list.isError ? (
          <p className="au-note" role="alert">
            Couldn’t read the automations: {list.error.message}{" "}
            <button
              type="button"
              className="au-retry"
              onClick={() => void list.refetch()}
            >
              Retry
            </button>
          </p>
        ) : !all.length ? (
          <p className="au-note">
            No automations yet. Start one from a template, or ask Founder to set
            one up.
          </p>
        ) : (
          <ul aria-label="Automations">{all.map(row)}</ul>
        )}
      </nav>
    </div>
  );
}
