import "./dashboards.css";
import { Link, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Globe, Pin, Plus } from "lucide-react";
import type { FoundryDashboard } from "../../../shared/wfe";
import {
  ButtonLink,
  SkeletonText,
  cx,
  formatRelative,
  useNow,
} from "../../kit";
import { useWorkspace } from "../../ui";
import { dashboardPaths as paths } from "./paths";
import { dashboardQueries } from "./queries";

// The Dashboards section's list, in the shell's sidebar under the sections
// while the section is open: New dashboard, then the pinned dashboards
// (every member's, in the order they were pinned) and the rest by recency.
// The shell renders it outside the page's routes: it reads the open
// dashboard from the path and the list from the query the pages share.

export function DashboardList({ onNavigate }: { onNavigate?: () => void }) {
  const { id: ws } = useWorkspace(),
    { pathname } = useLocation(),
    now = useNow(60_000);
  const list = useQuery(dashboardQueries.list(ws));
  const open = /^\/dashboards\/([^/]+)/.exec(pathname)?.[1];
  const all = list.data?.dashboards || [];
  const groups = [
    { label: "Pinned", items: all.filter((d) => d.pinned) },
    { label: "Dashboards", items: all.filter((d) => !d.pinned) },
  ].filter((g) => g.items.length);
  const row = (d: FoundryDashboard) => {
    const on = d.id === open;
    return (
      <li key={d.id}>
        <Link
          to={paths.dashboard(d.id)}
          onClick={onNavigate}
          className={cx("db-item", on && "is-active")}
          aria-current={on ? "page" : undefined}
          title={d.name}
        >
          <span className="db-item-title">{d.name || "Untitled"}</span>
          <small>
            {d.pinned && <Pin size={11} aria-label="Pinned" />}
            {d.share && <Globe size={11} aria-label="Published" />}
            {d.updatedAt ? `Edited ${formatRelative(d.updatedAt, now)}` : ""}
          </small>
        </Link>
      </li>
    );
  };
  return (
    <div className="db-panel">
      {list.data?.canCreate && (
        <div className="db-panel-head">
          <ButtonLink
            to={`${paths.dashboards()}?new=1`}
            icon={Plus}
            className="db-new"
            onClick={onNavigate}
          >
            New dashboard
          </ButtonLink>
        </div>
      )}
      <nav className="db-list" aria-label="Dashboards">
        {list.isPending ? (
          <div className="db-note" aria-busy="true">
            <SkeletonText lines={3} />
          </div>
        ) : list.isError ? (
          <p className="db-note" role="alert">
            Couldn’t read the dashboards: {list.error.message}{" "}
            <button
              type="button"
              className="db-retry"
              onClick={() => void list.refetch()}
            >
              Retry
            </button>
          </p>
        ) : !groups.length ? (
          <p className="db-note">
            No dashboards yet. Pin an Analyst answer’s chart, or ask the
            Assistant to build one.
          </p>
        ) : (
          groups.map((g) => (
            <section key={g.label} className="db-group">
              <h2>{g.label}</h2>
              <ul aria-label={g.label}>{g.items.map(row)}</ul>
            </section>
          ))
        )}
      </nav>
    </div>
  );
}
