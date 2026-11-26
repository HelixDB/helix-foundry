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
    summary: "Globex sells widgets.",
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
        onSummary: () => {},
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

  it("opens a researched company on what it does, asking whether that looks right", () => {
    const html = panel({ kind: "idle" }, company);
    expect(html).toContain("1 of 3");
    expect(html).toContain("Globex");
    expect(html).toContain('<p class="on-guess">Globex sells widgets.</p>');
    expect(html).toContain("Does this look right to you?");
    expect(html).toContain("This looks right to me");
    expect(html).toContain("Change it");
    expect(html).not.toContain("<textarea");
    // Named by its heading; the button it focuses says what it confirms.
    expect(html).toContain('aria-labelledby="on-company-head"');
    expect(html).toContain(
      'aria-describedby="on-company-head on-guess on-guess-ask"',
    );
    // One screen at a time: no question yet.
    expect(html).not.toContain("Who asks?");
    expect(html).not.toContain("Foundry writes your goal from these next.");
  });

  it("opens on the field when there is nothing to confirm, and the last screen writes the goal", () => {
    const blank = panel({ kind: "idle" }, { ...company, summary: " " });
    expect(blank).toContain("<textarea");
    expect(blank).toContain("What does Globex do?");
    expect(blank).toContain("Next");
    expect(blank).not.toContain("Change it");
    expect(blank).not.toContain("This looks right to me");
    // Without questions what it does is the last screen.
    const only = { ...company, research: { ...company.research, questions: [] } };
    const last = panel({ kind: "idle" }, only);
    expect(last).toContain("1 of 1");
    expect(last).toContain("Foundry writes your goal from these next.");
    expect(last).toContain("This looks right to me");
    expect(panel({ kind: "idle" }, { ...only, summary: "" })).toContain(
      "Write the goal",
    );
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
