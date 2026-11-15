import { lazy } from "react";
import { Zap } from "lucide-react";
import type { AreaShell } from "../../shell/types";
import { automationPaths as paths } from "./paths";
import { automationQueries } from "./queries";

// The Automations section's list in the sidebar, under the sections (by
// recency, each with its status), and the command palette's group.

const AutomationList = lazy(() =>
  import("./sidebar").then((m) => ({ default: m.AutomationList })),
);

const STATUS: Record<string, string> = {
  active: "On",
  paused: "Paused",
  needs_approval: "Waits for approval",
};

export const automationsShell: AreaShell = {
  sidebar: {
    automations: { label: "Automations", component: AutomationList },
  },
  palette: [
    {
      id: "automations",
      label: "Automations",
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
