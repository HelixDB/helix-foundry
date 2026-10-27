import { LOCAL } from "../../../shared/deployment";
import type { AreaShell } from "../../shell/types";
import { workspacePaths } from "./paths";

// Workspace owns the Settings section. Settings' pages are not
// listed in the sidebar (Settings sits in its footer); the command palette
// finds them by name. A local build has no Members, Plan & billing or
// People enrichment.
export const workspaceShell: AreaShell = {
  subnav: {
    settings: [
      { to: workspacePaths.settings(), label: "General", end: true },
      { to: workspacePaths.settings("ai"), label: "AI models" },
      ...(LOCAL
        ? []
        : [
            { to: workspacePaths.settings("members"), label: "Members" },
            {
              to: workspacePaths.settings("billing"),
              label: "Plan & billing",
            },
            {
              to: workspacePaths.settings("enrichment"),
              label: "People enrichment",
            },
          ]),
    ],
  },
};
