import { lazy } from "react";
import { Zap } from "lucide-react";
import type { AreaShell } from "../../shell/types";
import { automationPaths as paths, openAutomation } from "./paths";
import { automationQueries } from "./queries";

// The Automations section's list in the sidebar, under the sections, while
// an automation (or one of its runs) is open: the way between them. The
// overview and the new page have their own; the command palette's group.

const AutomationList = lazy(() =>
  import("./sidebar").then((m) => ({ default: m.AutomationList })),
);

const STATUS: Record<string, string> = {
  active: "Live",
  paused: "Paused",
  needs_approval: "Needs approval",
};

export const automationsShell: AreaShell = {
  sidebar: {
    automations: {
      label: "Workflows",
      component: AutomationList,
      shows: (pathname) => !!openAutomation(pathname),
    },
  },
  palette: [
    {
      id: "automations",
      label: "Workflows",
      icon: Zap,
      load: async (ws) =>
        ((await automationQueries.list(ws).queryFn()).automations || []).map(
          (a) => ({
            id: a.id,
            title: a.name || a.id,
            subtitle: STATUS[a.status || ""],
            description: a.description || undefined,
            href: paths.automation(a.id),
          }),
        ),
    },
  ],
};
