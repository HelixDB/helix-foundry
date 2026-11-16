import { createElement } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import { AgentDock } from "@/web/areas/agent/dock";
import { NEW_DASHBOARD_PROMPT } from "@/web/areas/dashboards/model";
import type { DockSeed } from "@/web/shell/context";
import { WorkspaceContext } from "@/web/ui";

// Founder's dock opened by a compose (useShell().assistant.compose, which
// the Dashboards page's New dashboard calls): a new thread whose composer
// already holds the text; without one, the dock opens as it always has.

function render(seed: DockSeed | null) {
  return renderToStaticMarkup(
    createElement(
      QueryClientProvider,
      { client: new QueryClient() },
      createElement(
        WorkspaceContext.Provider,
        {
          value: {
            id: "ws_1",
            slug: "acme",
            name: "Acme",
            role: "owner",
            user: { id: "usr_1", name: "Ada", email: "ada@example.com" },
            onboarding: false,
            grantee: false,
            granted: [],
            partial: [],
            workflows: true,
            notify: () => {},
          },
        },
        createElement(
          MemoryRouter,
          { initialEntries: ["/dashboards"] },
          createElement(AgentDock, { onClose: () => {}, seed }),
        ),
      ),
    ),
  );
}

describe("Founder's dock from a compose", () => {
  it("opens a new thread whose composer holds the text", () => {
    const html = render({ text: NEW_DASHBOARD_PROMPT, n: 1 });
    expect(html).toContain("New thread");
    expect(html).toMatch(
      /<textarea[^>]*>Build me a dashboard for: <\/textarea>/,
    );
  });

  it("starts empty without one", () => {
    expect(render(null)).toMatch(/<textarea[^>]*><\/textarea>/);
  });
});
