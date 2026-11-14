import { lazy } from "react";
import { LayoutDashboard } from "lucide-react";
import type { AreaShell } from "../../shell/types";
import { dashboardPaths as paths } from "./paths";
import { dashboardQueries } from "./queries";

// The Dashboards section's list in the sidebar, under the sections (pinned
// ones first, then the rest by recency), and the command palette's group.

const DashboardList = lazy(() =>
  import("./sidebar").then((m) => ({ default: m.DashboardList })),
);

export const dashboardsShell: AreaShell = {
  sidebar: { dashboards: { label: "Dashboards", component: DashboardList } },
  palette: [
    {
      id: "dashboards",
      label: "Dashboards",
      icon: LayoutDashboard,
      load: async (ws) =>
        ((await dashboardQueries.list(ws).queryFn()).dashboards || []).map(
          (d) => ({
            id: d.id,
            title: d.name || d.id,
            subtitle: d.pinned ? "Pinned" : undefined,
            description: d.description || undefined,
            href: paths.dashboard(d.id),
          }),
        ),
    },
  ],
};
