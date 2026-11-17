import { createElement } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import { AgentDock, Composer } from "@/web/areas/agent/dock";
import { NEW_DASHBOARD_PROMPT } from "@/web/areas/dashboards/model";
import type { DockSeed } from "@/web/shell/context";
import { WorkspaceContext, type WorkspaceValue } from "@/web/ui";

// Founder's dock opened by a compose (useShell().assistant.compose, which
// the Dashboards page's New dashboard calls): a new thread whose composer
// already holds the text; without one, the dock opens as it always has.

const WORKSPACE: WorkspaceValue = {
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
};

function render(seed: DockSeed | null) {
  return renderToStaticMarkup(
    createElement(
      QueryClientProvider,
      { client: new QueryClient() },
      createElement(
        WorkspaceContext.Provider,
        { value: WORKSPACE },
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

// A suggestion picked on the new workflow's page: its chip says the
// ontology it was written for, which the message's page context names
// (pageContext's `about`, agent-model.test.ts).
function composer(about?: string) {
  const client = new QueryClient();
  client.setQueryData(["ws_1", "ontologies"], {
    ontologies: [{ id: "ont_1", name: "Sales graph" }],
  });
  return renderToStaticMarkup(
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        WorkspaceContext.Provider,
        { value: WORKSPACE },
        createElement(
          MemoryRouter,
          { initialEntries: ["/workflows/new"] },
          createElement(Composer, {
            threadId: "",
            runtime: "api",
            hostState: "ready",
            initialText: "Every Monday, summarise new customers",
            about,
            onSent: () => {},
          }),
        ),
      ),
    ),
  );
}

describe("Founder's composer with a suggestion's ontology", () => {
  it("names the ontology on the new workflow's chip", () => {
    expect(composer("ont_1")).toContain(
      "Making a new workflow for Sales graph",
    );
  });

  it("names none without one", () => {
    const html = composer();
    expect(html).toContain("Making a new workflow");
    expect(html).not.toContain("Making a new workflow for");
  });
});
