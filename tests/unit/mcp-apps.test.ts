import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import {
  META_KEY,
  answerCard,
  citationLabel,
  citationLink,
  draftLink,
  sitePage,
  webUrl,
  type QuestionCard,
  type ToolResult,
} from "@/mcp-apps/answer-model";
import { AnswerView } from "@/mcp-apps/answer";
import { drawable } from "@/web/areas/ontology/ask-visuals";
import { VIEWS, buildView, committed } from "../../scripts/mcp-apps";
import { contractErrors } from "../../mock/agent-contract";

// The answer view (src/mcp-apps): what it reads of the Foundry MCP's ask and
// get_question (backend/integration/apps.go), what it shows, where its links
// go, and that the document the Go module embeds is what the sources build.

const ASK = "https://foundry.helix-db.com/ask/ont_1?workspace=ws_1&thread=q_1";

const BY_STATUS = {
  start: { type: "Invoice", where: [], ids: null, text: null },
  steps: [],
  filters: [],
  return: { group_count: { at: 0, prop: "status", limit: 50 } },
};
const STATUSES = {
  kind: "group_count",
  groups: [
    { value: { untrusted: "paid" }, count: 40 },
    { value: { untrusted: "open" }, count: 20 },
  ],
  total_groups: 2,
};
const USAGE = { input: 1, cache_read: 0, cache_write: 0, output: 1, usd: 0.01 };

// As the engine stores it, outputs and all.
const STORED = {
  status: "answered",
  answer: {
    blocks: [
      { para: "Most invoices are paid [c1]. <img src=x onerror=alert(1)>" },
      { list: ["40 paid [c2]", "20 open"] },
    ],
    visuals: [{ evidence_id: "e2", after: 0, kind: "bar", title: "By status" }],
  },
  citations: [
    {
      marker: "c1",
      evidence_id: "e2",
      target: { result: { evidence_id: "e2" } },
    },
    {
      marker: "c2",
      evidence_id: "e3",
      target: { quote: { doc_id: "doc_1", section: 0, start: 4, end: 9 } },
    },
  ],
  queries: [
    { evidence_id: "e2", query: BY_STATUS, output: STATUSES },
    { evidence_id: "e3", query: BY_STATUS, output: STATUSES },
  ],
  usage: USAGE,
};

/** What the MCP returns for STORED: its result without outputs, the visual's in _meta. */
function answered(
  meta: Record<string, unknown> = { ask_url: ASK },
): ToolResult {
  return {
    content: [{ type: "text", text: "untrusted notice" }],
    structuredContent: {
      content_trust: "untrusted",
      id: "q_1",
      thread_id: "q_1",
      load_id: "load_1",
      question: "How many invoices are paid?",
      status: "answered",
      done: true,
      result: {
        ...STORED,
        queries: STORED.queries.map((q) => ({
          evidence_id: q.evidence_id,
          query: q.query,
        })),
      },
      attempts: 1,
      usd_micros: 10000,
      created_at: "2026-11-16T10:00:00Z",
      finished_at: "2026-11-16T10:00:12Z",
    },
    _meta: {
      "io.modelcontextprotocol/serverInfo": { name: "helix", version: "1" },
      [META_KEY]: { outputs: { e2: STATUSES }, ...meta },
    },
  };
}

const question = (r: ToolResult) => {
  const card = answerCard(r);
  if (card?.kind !== "question")
    throw new Error(`not a question: ${JSON.stringify(card)}`);
  return card;
};

const render = (card?: ReturnType<typeof answerCard>, asking?: string) =>
  renderToStaticMarkup(
    createElement(AnswerView, { card, asking, open: () => {} }),
  );

describe("the answer view's model", () => {
  it("holds the fixture to the engine's contract", () => {
    expect(contractErrors("ask_result", STORED)).toEqual([]);
  });

  it("puts the visuals' outputs back into the answer's queries", () => {
    const card = question(answered());
    expect(card).toMatchObject({
      question: "How many invoices are paid?",
      loadId: "load_1",
      askUrl: ASK,
      ontologyId: "ont_1",
      finishedAt: "2026-11-16T10:00:12Z",
    });
    expect(card.view.kind).toBe("answered");
    if (card.view.kind !== "answered") return;
    // e2 is drawn from _meta; e3, shown by no visual, has no output to draw.
    expect(
      drawable(card.view.result).map((d) => [d.visual.evidence_id, d.output]),
    ).toEqual([["e2", STATUSES]]);
    expect(
      card.view.result.queries.find((q) => q.evidence_id === "e3")?.output,
    ).toBeUndefined();
  });

  it("reads a question still going as the Analyst does", () => {
    const going = (status: string, extra = {}) =>
      question({
        structuredContent: { id: "q_1", question: "Q", status, ...extra },
      }).view;
    expect(going("queued")).toEqual({ kind: "queued" });
    expect(going("running")).toEqual({ kind: "running" });
    expect(
      going("queued", { attempts: 1, error: "the model was busy" }),
    ).toEqual({
      kind: "retrying",
      error: "The model was busy.",
    });
    expect(going("cancelled")).toEqual({ kind: "cancelled" });
    expect(going("failed", { error: "store failed" }).kind).toBe("failed");
  });

  it("reads a question the ontology can't answer, and drafts the version that could", () => {
    const card = question({
      structuredContent: {
        id: "q_1",
        question: "Which shipments carry chemicals?",
        status: "cannot_answer",
        result: {
          status: "cannot_answer",
          reason: "Shipments have no cargo text.",
          gap: "A cargo description on Shipment.",
          queries: [],
          usage: USAGE,
        },
      },
      _meta: { [META_KEY]: { ask_url: ASK } },
    });
    expect(card.view.kind).toBe("cannot_answer");
    const draft = new URL(draftLink(card)!);
    expect(draft.origin + draft.pathname).toBe(
      "https://foundry.helix-db.com/ontology/ont_1/generate",
    );
    expect(draft.searchParams.get("workspace")).toBe("ws_1");
    expect(draft.searchParams.get("goal")).toContain(
      "Which shipments carry chemicals?",
    );
    expect(draft.searchParams.get("goal")).toContain(
      "A cargo description on Shipment.",
    );
    // Only a question it can't answer drafts; without the website, none does.
    expect(draftLink(question(answered()))).toBeUndefined();
    expect(draftLink({ ...card, askUrl: undefined })).toBeUndefined();
  });

  it("says why the tool refused", () => {
    expect(
      answerCard({
        isError: true,
        content: [{ type: "text", text: "budget spent" }],
      }),
    ).toEqual({ kind: "refused", message: "budget spent" });
    expect(answerCard({ isError: true })).toEqual({
      kind: "refused",
      message: "Foundry refused it.",
    });
  });

  it("reads nothing that isn't a question", () => {
    for (const structuredContent of [
      undefined,
      "q",
      { id: "q_1" },
      { id: "q_1", status: "done" },
      { status: "running" },
    ])
      expect(answerCard({ structuredContent })).toBeUndefined();
  });

  it("opens only the website's http(s) pages", () => {
    expect(webUrl(ASK)).toBe(ASK);
    expect(webUrl("javascript:alert(1)")).toBeUndefined();
    expect(webUrl("not a url")).toBeUndefined();
    expect(webUrl(4)).toBeUndefined();
    const card = question(answered({ ask_url: "javascript:alert(1)" }));
    expect(card.askUrl).toBeUndefined();
    expect(card.ontologyId).toBeUndefined();
  });

  it("links a citation to its page, else to the thread, as its examples say", () => {
    expect(sitePage(ASK, "/explore/o1/entities/e1?load=l1")).toBe(
      "https://foundry.helix-db.com/explore/o1/entities/e1?load=l1&workspace=ws_1",
    );
    const card = question(answered());
    expect(
      citationLink(card, {
        quote: { doc_id: "doc_1", section: 0, start: 4, end: 9 },
      }),
    ).toBe(
      "https://foundry.helix-db.com/ontology/ont_1/documents/doc_1?load=load_1&workspace=ws_1#s0-4-9",
    );
    // An entity's page needs its version's keys, which the card hasn't: the thread has them.
    expect(
      citationLink(card, { entity: { type: "Customer", id: "acme" } }),
    ).toBe(ASK);
    expect(citationLink(card, { result: { evidence_id: "e2" } })).toBe(ASK);
    const offline: QuestionCard = {
      ...card,
      askUrl: undefined,
      ontologyId: undefined,
    };
    expect(
      citationLink(offline, { entity: { type: "Customer", id: "acme" } }),
    ).toBeUndefined();
    expect(
      citationLabel(card, { entity: { type: "Customer", id: "acme" } }),
    ).toBe("Customer acme");
  });
});

describe("the answer view", () => {
  it("shows the question being asked before its result", () => {
    const html = render(undefined, "How many invoices are paid?");
    expect(html).toContain("How many invoices are paid?");
    expect(html).toContain("Asking Foundry");
  });

  it("shows an answer's text, chips, visual and the way to its thread", () => {
    const html = render(answerCard(answered()));
    expect(html).toContain("Most invoices are paid");
    expect(html).toContain('aria-label="Source 1: a query&#x27;s result"');
    expect(html).toContain('aria-label="Source 2: Quote in a document"');
    expect(html).toContain("By status");
    expect(html).toContain("Open in Analyst");
    // Untrusted text never becomes markup.
    expect(html).not.toContain("<img");
  });

  it("shows a refusal, and an answer without the website has no link", () => {
    expect(render({ kind: "refused", message: "budget spent" })).toContain(
      "budget spent",
    );
    expect(render(answerCard(answered({})))).not.toContain("Open in Analyst");
  });
});

describe("the views the Go module embeds", () => {
  it.each(VIEWS)(
    "$out is what $entry builds (pnpm mcp-apps)",
    async (view) => {
      expect(await buildView(view)).toBe(await committed(view));
    },
    60_000,
  );
});
