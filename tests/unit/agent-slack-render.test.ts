import { createElement } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import type { FoundryAgentSurface, FoundryAgentTurn } from "@/shared/wfe";
import { Transcript } from "@/web/areas/agent/dock";
import { WorkspaceContext } from "@/web/ui";

// A Founder thread born in Slack (Helix Foundry's Slack app) shows in the
// person's list like any other: its turns say they came from Slack, and the
// conversation says Helix deletes it 30 days after its last message.

const NOTE =
  "This conversation is from Slack. Helix deletes Slack conversations 30 days after their last message.";

function render(surface?: FoundryAgentSurface) {
  const turn: FoundryAgentTurn = {
    id: "turn_1",
    threadId: "thr_1",
    kind: "message",
    text: "How many customers?",
    status: "done",
    lastSeq: 0,
    ...(surface && { surface }),
  };
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
            role: "member",
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
          null,
          createElement(Transcript, {
            data: { thread: { id: "thr_1" }, turns: [turn] },
            loading: false,
            failed: false,
            onRetry: () => {},
          }),
        ),
      ),
    ),
  );
}

describe("a Slack-born thread", () => {
  it("tags its person's turns Slack and says when it is deleted", () => {
    const html = render("FOUNDRY_AGENT_SURFACE_SLACK");
    expect(html).toContain(
      '<span class="hf-tag hf-tone--neutral">Slack</span>',
    );
    expect(html).toContain(NOTE);
  });
  it("is the only one: a site thread shows neither", () => {
    for (const surface of [
      undefined,
      "FOUNDRY_AGENT_SURFACE_UNSPECIFIED",
    ] as const) {
      const html = render(surface);
      expect(html).not.toContain(">Slack</span>");
      expect(html).not.toContain(NOTE);
      expect(html).toContain("How many customers?");
    }
  });
});
