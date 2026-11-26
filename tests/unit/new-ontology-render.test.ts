import { createElement, type ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import type * as wfe from "@/shared/wfe";
import { connectorsQuery } from "@/web/areas/data/data-api";
import {
  documentQueries,
  documentSetQueries,
} from "@/web/areas/documents/documents-api";
import { CompanyPanel } from "@/web/areas/ontology/company-step";
import { useGoalSources } from "@/web/areas/ontology/generate-parts";
import type { CompanyChoice, ResearchRun } from "@/web/areas/ontology/model";
import { WorkspaceContext } from "@/web/ui";

// The new-ontology wizard's parts as they render: the chosen data a goal is
// drafted from only once every read it needs is in, and the company step's
// status line, which says what the research is doing and what it found.

const WS = "ws_1";

function render(qc: QueryClient, node: ReactNode) {
  return renderToStaticMarkup(
    createElement(
      QueryClientProvider,
      { client: qc },
      createElement(
        WorkspaceContext.Provider,
        {
          value: {
            id: WS,
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
        createElement(MemoryRouter, null, node),
      ),
    ),
  );
}

describe("useGoalSources", () => {
  const Probe = () =>
    createElement(
      "pre",
      null,
      JSON.stringify(
        useGoalSources({ chosen: null, chosenEvents: null, chosenSets: null }),
      ),
    );
  const connectors: wfe.ListConnectorsResponse = {
    connectors: [
      { id: "c1", kind: "stripe", name: "Stripe", tables: [{ name: "t" }] },
    ],
  };

  it("waits for the connectors, the document sets and the documents", () => {
    const qc = new QueryClient();
    qc.setQueryData(connectorsQuery(WS).queryKey, connectors);
    expect(render(qc, createElement(Probe))).toBe("<pre>null</pre>");
    qc.setQueryData(documentSetQueries.list(WS).queryKey, []);
    expect(render(qc, createElement(Probe))).toBe("<pre>null</pre>");
    qc.setQueryData(documentQueries.workspace(WS).queryKey, []);
    const read = JSON.parse(
      render(qc, createElement(Probe))
        .replace(/^<pre>|<\/pre>$/g, "")
        .replace(/&quot;/g, '"'),
    );
    expect(read.connectors).toMatchObject([{ name: "Stripe", chosen: true }]);
    expect(read.sets).toEqual([]);
  });

  it("counts a set or document read that failed as none", () => {
    // As once its retries are spent: mounting doesn't read it again.
    const qc = new QueryClient({
      defaultOptions: { queries: { retryOnMount: false } },
    });
    qc.setQueryData(connectorsQuery(WS).queryKey, connectors);
    for (const queryKey of [
      documentSetQueries.list(WS).queryKey,
      documentQueries.workspace(WS).queryKey,
    ])
      qc.getQueryCache()
        .build(qc, { queryKey })
        .setState({ status: "error", error: new Error("down") });
    expect(render(qc, createElement(Probe))).not.toBe("<pre>null</pre>");
  });
});

describe("CompanyPanel", () => {
  const company: CompanyChoice = {
    research: {
      id: "r1",
      domain: "globex.com",
      companyName: "Globex",
      questions: [
        { id: "q1", question: "Who asks?" },
        { id: "q2", question: "What matters?" },
      ],
    },
    answers: new Map(),
  };
  const panel = (research: ResearchRun, c: CompanyChoice | null = null) =>
    render(
      new QueryClient(),
      createElement(CompanyPanel, {
        domain: "globex.com",
        company: c,
        research,
        onDomain: () => {},
        onResearch: () => {},
        onAnswer: () => {},
        onFinish: () => {},
        onSkip: () => {},
      }),
    );
  const status = (html: string) =>
    /<p class="on-muted on-company-status" role="status">(.*?)<\/p>/.exec(
      html,
    )?.[1];

  it("asks for the website first, its status line mounted, saying when it reads the site", () => {
    const idle = panel({ kind: "idle" });
    expect(idle).toContain("What’s your company’s website?");
    expect(idle).toContain("Skip");
    expect(status(idle)).toBe("");
    const pending = panel({ kind: "pending", domain: "globex.com" });
    expect(status(pending)).toBe("Reading globex.com…");
    expect(pending).toContain('aria-busy="true"');
  });

  it("opens a researched company on its first question, under what Foundry found, one question at a time", () => {
    const html = panel({ kind: "idle" }, company);
    expect(html).toContain("Globex");
    expect(html).toContain("Question 1 of 2");
    expect(html).toContain("Who asks?");
    expect(html).not.toContain("What matters?");
    expect(html).toContain("Next");
    expect(html).not.toContain("Write the goal");
    // The only question's button writes the goal.
    const one = panel(
      { kind: "idle" },
      {
        ...company,
        research: {
          ...company.research,
          questions: [company.research.questions![0]!],
        },
      },
    );
    expect(one).toContain("Question 1 of 1");
    expect(one).toContain("Write the goal");
  });

  it("says why research failed, with Retry", () => {
    const html = panel({
      kind: "failed",
      error: new Error("the site timed out"),
    });
    expect(html).toContain('role="alert"');
    expect(html).toContain("The site timed out");
    expect(html).toContain("Retry");
  });
});
