// Dashboards area: the workspace's dashboards, each tiles of typed queries
// over ontologies' loads drawn as the Analyst draws an answer's visuals,
// lazy-loaded by main.tsx's route table under /dashboards. Every member
// sees every dashboard (a grantee only the pinned ones) and reads each tile
// as themselves; what they may change, the WFE says per dashboard.
import "./dashboards.css";
import { useMemo, useState } from "react";
import { Route, Routes } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { LayoutDashboard, Plus, Search, Sparkles } from "lucide-react";
import {
  Button,
  EmptyState,
  FilterBar,
  Page,
  PageHeader,
  Segmented,
  Skeleton,
  plural,
  useNow,
} from "../../kit";
import { useShell } from "../../shell/context";
import { RefreshFailed, useWorkspace } from "../../ui";
import { DashboardPage } from "./dashboard";
import { DashboardCard } from "./library";
import {
  NEW_DASHBOARD_PROMPT,
  libraryDashboards,
  type LibraryFilter,
} from "./model";
import { dashboardQueries } from "./queries";

export function DashboardsRoutes() {
  return (
    <Routes>
      <Route index element={<DashboardsPage />} />
      <Route path=":dashboardId" element={<DashboardPage />} />
    </Routes>
  );
}

/**
 * /dashboards: the library, every dashboard the viewer sees as a card (a
 * miniature, its name and when it was edited), pinned first, searched by
 * name or description and filtered to the pinned or the viewer's own. New
 * dashboard opens Founder on a new thread that starts "Build me a dashboard
 * for: ", where the build has Founder; a grantee can't make one.
 */
function DashboardsPage() {
  const { id: ws, user } = useWorkspace(),
    shell = useShell(),
    now = useNow(60_000);
  const list = useQuery(dashboardQueries.list(ws));
  const [query, setQuery] = useState(""),
    [filter, setFilter] = useState<LibraryFilter>("all");
  const dashboards = list.data?.dashboards || [];
  const shown = useMemo(
    () => libraryDashboards(dashboards, { query, filter, userId: user.id }),
    [dashboards, query, filter, user.id],
  );
  const counts = useMemo(
    () =>
      Object.fromEntries(
        (["all", "pinned", "yours"] as const).map((f) => [
          f,
          libraryDashboards(dashboards, {
            query: "",
            filter: f,
            userId: user.id,
          }).length,
        ]),
      ) as Record<LibraryFilter, number>,
    [dashboards, user.id],
  );
  const canCreate = !!list.data?.canCreate && shell.assistant.available;
  const build = () =>
    shell.assistant.compose(NEW_DASHBOARD_PROMPT, "new_dashboard");
  const header = (
    <PageHeader
      title="Dashboards"
      subtitle="Tables and charts of your ontologies’ data, read as each viewer may read it."
      actions={
        canCreate && (
          <Button variant="primary" icon={Plus} onClick={build}>
            New dashboard
          </Button>
        )
      }
    />
  );
  if (list.isPending)
    return (
      <Page width="wide">
        {header}
        <div className="db-cards" aria-busy="true">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} className="db-card-wait" height={236} />
          ))}
        </div>
      </Page>
    );
  if (!list.data)
    return (
      <Page width="wide">
        {header}
        <EmptyState
          icon={LayoutDashboard}
          title="Couldn’t load the dashboards"
          text={list.error?.message}
          action={<Button onClick={() => void list.refetch()}>Retry</Button>}
        />
      </Page>
    );
  return (
    <Page width="wide">
      {header}
      <RefreshFailed query={list} />
      {!dashboards.length ? (
        <EmptyState
          icon={LayoutDashboard}
          title="No dashboards yet"
          text={
            canCreate
              ? "Tell Founder what you want to see, or pin a chart or table from an answer."
              : list.data.canCreate
                ? "Pin a chart or table from an Analyst answer."
                : "An owner or admin pins dashboards for the workspace here."
          }
          action={
            canCreate && (
              <Button variant="primary" icon={Sparkles} onClick={build}>
                Build one with Founder
              </Button>
            )
          }
        />
      ) : (
        <>
          <FilterBar
            className="db-library-bar"
            search={{
              value: query,
              onChange: setQuery,
              placeholder: "Search dashboards…",
            }}
            count={
              shown.length !== dashboards.length
                ? `${shown.length} of ${plural(dashboards.length, "dashboard")}`
                : undefined
            }
          >
            <Segmented
              label="Show"
              value={filter}
              onChange={(v) => setFilter(v as LibraryFilter)}
              options={[
                { value: "all", label: "All", count: counts.all },
                { value: "pinned", label: "Pinned", count: counts.pinned },
                { value: "yours", label: "Yours", count: counts.yours },
              ]}
            />
          </FilterBar>
          {shown.length ? (
            <ul className="db-cards" aria-label="Dashboards">
              {shown.map((d) => (
                <li key={d.id}>
                  <DashboardCard dashboard={d} now={now} />
                </li>
              ))}
            </ul>
          ) : (
            <EmptyState
              icon={Search}
              title={
                query.trim()
                  ? `No dashboards match “${query.trim()}”`
                  : filter === "pinned"
                    ? "No pinned dashboards"
                    : "You haven’t made a dashboard yet"
              }
              text={
                filter === "pinned" && !query.trim()
                  ? "An owner or admin pins dashboards for everyone in the workspace."
                  : undefined
              }
              action={
                <Button
                  onClick={() => {
                    setQuery("");
                    setFilter("all");
                  }}
                >
                  Show all dashboards
                </Button>
              }
            />
          )}
        </>
      )}
    </Page>
  );
}
