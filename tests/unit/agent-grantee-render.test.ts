import { createElement } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import { Composer, Transcript } from "@/web/areas/agent/dock";
import { aiKey } from "@/web/areas/workspace/ai-api";
import { WorkspaceContext } from "@/web/ui";

// Founder's composer and empty conversation as a grantee (a member restricted
// to what source grants give) first sees them, beside a member's: a grantee's
// Founder only reads and asks, so its copy says so, and the workspace's AI
// setting, which the WFE denies a grantee, is never read.

function render(grantee: boolean) {
  const qc = new QueryClient();
  const html = renderToStaticMarkup(
    createElement(
      QueryClientProvider,
      { client: qc },
      createElement(
        WorkspaceContext.Provider,
        {
          value: {
            id: "ws_1",
            slug: "acme",
            name: "Acme",
            role: "member",
            user: { id: "usr_1", name: "Eve", email: "eve@example.com" },
            onboarding: false,
            grantee,
            granted: grantee ? ["ont_1"] : [],
            partial: [],
            workflows: !grantee,
            notify: () => {},
          },
        },
        createElement(
          MemoryRouter,
          { initialEntries: ["/dashboards"] },
          createElement(Transcript, {
            loading: false,
            failed: false,
            onRetry: () => {},
          }),
          createElement(Composer, {
            threadId: "",
            runtime: "api",
            hostState: "ready",
            onSent: () => {},
          }),
        ),
      ),
    ),
  );
  // The AI setting's query as the composer made it: a render on the server
  // reads nothing, so whether it would is its observer's `enabled`.
  const ai = qc.getQueryCache().find({ queryKey: aiKey("ws_1") });
  return {
    html,
    ai: ai && { enabled: (ai.options as { enabled?: unknown }).enabled },
  };
}

describe("Founder for a grantee", () => {
  it("asks about the data shared with it, and says it can't change anything", () => {
    const { html } = render(true);
    expect(html).toContain('placeholder="Ask about the data shared with you…"');
    expect(html).toContain(
      "It answers questions about the data shared with you, with cited answers. It can’t change anything in this workspace.",
    );
    expect(html).not.toContain("cards you click to run");
  });

  it("never reads the workspace's AI setting", () => {
    expect(render(true).ai?.enabled).toBe(false);
  });
});

describe("Founder for a member", () => {
  it("works in the workspace, and reads its AI setting", () => {
    const { html, ai } = render(false);
    expect(html).toContain(
      'placeholder="Ask about your workspace, or ask it to do something…"',
    );
    expect(html).toContain("Changes wait as cards you click to run");
    expect(ai).toBeDefined();
    expect(ai?.enabled).not.toBe(false);
  });
});
