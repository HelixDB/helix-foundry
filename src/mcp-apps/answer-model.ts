import { isRecord } from "../shared/json";
import type {
  CitationTarget,
  FoundryQuestion,
  QuestionStatus,
} from "../shared/wfe";
import { ontologyPaths } from "../web/areas/ontology/paths";
import {
  askOntologyId,
  citationHref,
  citationText,
  draftGoal,
  questionView,
  type CitationContext,
  type QuestionView,
} from "../web/areas/ontology/ask-model";

// The answer view's reading of what the Foundry MCP's ask and get_question
// return (backend/integration: FoundryQuestionOutput in structuredContent,
// and in the result's _meta under META_KEY the outputs its visuals show and
// its thread's page). It reads them as the Analyst reads a question
// (questionView), so the card says what the website would. Everything the
// result holds is data from the agent and the user's sources: the card shows
// it only as text.

/** A tool result as the host relays it (MCP's CallToolResult), as far as the card reads it. */
export type ToolResult = {
  content?: readonly { type: string; text?: string }[];
  structuredContent?: unknown;
  _meta?: unknown;
  isError?: boolean;
};

/** Where the Foundry MCP puts what only a view reads (backend/integration: appMetaKey). */
export const META_KEY = "com.helix-db/foundry";

/** A question as the Analyst would show it. */
export type QuestionCard = {
  kind: "question";
  question: string;
  view: QuestionView;
  // The load it asked about, which its citations' pages read.
  loadId?: string;
  // The question's thread in the Analyst, when the stamp names the website,
  // and the ontology it names.
  askUrl?: string;
  ontologyId?: string;
  finishedAt?: string;
};

/** What the card shows: a question, or why the tool refused it. */
export type AnswerCard = QuestionCard | { kind: "refused"; message: string };

const STATUSES = new Set<QuestionStatus>([
  "queued",
  "running",
  "answered",
  "cannot_answer",
  "failed",
  "cancelled",
]);

const str = (x: unknown) => (typeof x === "string" ? x : undefined);

/**
 * A Foundry web address, or undefined for anything else: only http(s) opens.
 *
 * @example
 * webUrl("https://foundry.helix-db.com/ask/o1?workspace=w1"); // "https://foundry.helix-db.com/ask/o1?workspace=w1"
 * webUrl("javascript:alert(1)"); // undefined
 */
export function webUrl(x: unknown): string | undefined {
  const s = str(x);
  if (!s) return undefined;
  try {
    const u = new URL(s);
    return u.protocol === "https:" || u.protocol === "http:"
      ? u.href
      : undefined;
  } catch {
    return undefined;
  }
}

/**
 * A page of the website the question's link names, in its workspace: a site
 * path (a citation's, citationHref) made absolute beside `askUrl`.
 *
 * @example
 * sitePage("https://foundry.helix-db.com/ask/o1?workspace=w1&thread=q1", "/explore/o1/entities/e1?load=l1");
 * // "https://foundry.helix-db.com/explore/o1/entities/e1?load=l1&workspace=w1"
 */
export function sitePage(askUrl: string, path: string): string {
  const ask = new URL(askUrl);
  const page = new URL(path, ask.origin);
  const workspace = ask.searchParams.get("workspace");
  if (workspace) page.searchParams.set("workspace", workspace);
  return page.href;
}

/**
 * The card for a tool result: a refusal's message, or the question with its
 * answer, its visuals' outputs put back into its queries, read as the
 * Analyst reads one. Undefined for a result that isn't a question.
 *
 * @example
 * answerCard({ isError: true, content: [{ type: "text", text: "not_found" }] }); // { kind: "refused", message: "not_found" }
 * answerCard({ structuredContent: { id: "q1", question: "How many?", status: "running" } });
 * // { kind: "question", question: "How many?", view: { kind: "running" } }
 */
export function answerCard(result: ToolResult): AnswerCard | undefined {
  if (result.isError) {
    const message = (result.content ?? [])
      .map((c) => (c.type === "text" ? (c.text ?? "") : ""))
      .join(" ")
      .trim();
    return { kind: "refused", message: message || "Foundry refused it." };
  }
  const q = result.structuredContent;
  if (!isRecord(q) || !str(q.id) || !STATUSES.has(q.status as QuestionStatus))
    return undefined;
  const meta = isRecord(result._meta) ? result._meta[META_KEY] : undefined;
  const view = isRecord(meta) ? meta : {};
  const outputs = isRecord(view.outputs) ? view.outputs : {};
  // The agent's result leaves its queries' outputs out; the view's _meta
  // carries those its visuals show.
  const stored = isRecord(q.result)
    ? {
        ...q.result,
        queries: (Array.isArray(q.result.queries) ? q.result.queries : []).map(
          (query) =>
            isRecord(query) &&
            str(query.evidence_id) &&
            outputs[query.evidence_id as string] !== undefined
              ? { ...query, output: outputs[query.evidence_id as string] }
              : query,
        ),
      }
    : undefined;
  const question: FoundryQuestion = {
    id: q.id as string,
    status: q.status as QuestionStatus,
    question: str(q.question),
    threadId: str(q.thread_id),
    loadId: str(q.load_id),
    error: str(q.error),
    attempts: typeof q.attempts === "number" ? q.attempts : 0,
    finishedAt: str(q.finished_at),
    ...(stored && { resultJson: JSON.stringify(stored) }),
  };
  const askUrl = webUrl(view.ask_url);
  const ontologyId = askUrl ? askOntologyId(new URL(askUrl).pathname) : "";
  return {
    kind: "question",
    question: question.question ?? "",
    view: questionView(question),
    ...(question.loadId && { loadId: question.loadId }),
    ...(askUrl && { askUrl }),
    ...(ontologyId && { ontologyId }),
    ...(question.finishedAt && { finishedAt: question.finishedAt }),
  };
}

/**
 * What a card knows to name and link a citation: no lookups, so an entity is
 * named by its id and opens no page of its own (whether its type is keyed is
 * the version's to say), while a quote, a rule break and a load do.
 */
function citations(card: QuestionCard): CitationContext {
  return {
    ontologyId: card.ontologyId ?? "",
    loadId: card.loadId ?? "",
    keyed: () => undefined,
    docName: () => "a document",
    table: () => undefined,
    name: (r) => r.id,
    result: () => "a query's result",
  };
}

/**
 * Where a citation's chip opens: its page on the website when it has one,
 * else the question's thread there; undefined where the stamp names no
 * website.
 *
 * @example
 * const card = { kind: "question", question: "", view: { kind: "running" }, loadId: "l1", askUrl: "https://foundry.helix-db.com/ask/o1?workspace=w1&thread=q1" } as const;
 * citationLink(card, { quote: { doc_id: "d1", section: 0, start: 4, end: 9 } });
 * // "https://foundry.helix-db.com/ontology/o1/documents/d1?load=l1&workspace=w1#s0-4-9" (the document's span)
 * citationLink(card, { entity: { type: "Customer", id: "acme" } }); // card.askUrl
 */
export function citationLink(
  card: QuestionCard,
  target: CitationTarget,
): string | undefined {
  if (!card.askUrl) return undefined;
  const ctx = citations(card);
  const path = ctx.ontologyId ? citationHref(target, ctx) : undefined;
  return path ? sitePage(card.askUrl, path) : card.askUrl;
}

/**
 * A citation in words, for its chip's tooltip and name.
 *
 * @example
 * citationLabel(card, { entity: { type: "Customer", id: "acme" } }); // "Customer acme"
 */
export const citationLabel = (card: QuestionCard, target: CitationTarget) =>
  citationText(target, citations(card));

/**
 * Where a question the ontology can't answer drafts the version that could:
 * Generate on the website, its goal the question and what it lacked
 * (draftGoal), as the Analyst offers. Undefined for any other question, or
 * where the stamp names no website.
 *
 * @example
 * draftLink({ ...card, view: { kind: "cannot_answer", result } });
 * // "https://foundry.helix-db.com/ontology/o1/generate?goal=…&workspace=w1"
 */
export function draftLink(card: QuestionCard): string | undefined {
  if (card.view.kind !== "cannot_answer" || !card.askUrl || !card.ontologyId)
    return undefined;
  return sitePage(
    card.askUrl,
    ontologyPaths.generate(card.ontologyId, {
      goal: draftGoal(card.question, card.view.result),
    }),
  );
}
