import { LOCAL } from "../shared/deployment";
import { STAR_REPOSITORY_URL } from "../shared/github-star";
import type {
  FeedbackCategory,
  SendFeedbackRequest,
  SendFeedbackResponse,
} from "../shared/wfe";
import { api } from "./api";

// Feedback from people (the sidebar's Feedback button, src/web/shell/
// feedback.tsx, and the Data area's Request a connector): hosted, the WFE's
// POST /v1/feedback, which stores it and tells Helix's team in Slack with a
// Linear issue; a local build has no Helix to send it to, so it opens a new
// issue on GitHub, filled in, for the person to send or not.

/** The Feedback button's kinds, in the order it offers them. */
export const FEEDBACK_KINDS = [
  {
    kind: "bug",
    label: "Bug",
    category: "FEEDBACK_CATEGORY_BUG",
    prompt: "What happened, and what did you expect?",
  },
  {
    kind: "idea",
    label: "Idea",
    category: "FEEDBACK_CATEGORY_MISSING_CAPABILITY",
    prompt: "What would you like Foundry to do?",
  },
  {
    kind: "confusing",
    label: "Confusing",
    category: "FEEDBACK_CATEGORY_CONFUSING_RESULT",
    prompt: "What was confusing?",
  },
  {
    kind: "other",
    label: "Other",
    category: "FEEDBACK_CATEGORY_OTHER",
    prompt: "What’s on your mind?",
  },
] as const satisfies readonly {
  kind: string;
  label: string;
  category: FeedbackCategory;
  prompt: string;
}[];
export type FeedbackKind = (typeof FEEDBACK_KINDS)[number]["kind"];

/** The most a message holds, in bytes of UTF-8, as the WFE checks it. */
export const MAX_FEEDBACK_BYTES = 4096;
/** The most a page path holds, in bytes of UTF-8. */
const MAX_PAGE_BYTES = 255;

const encoder = new TextEncoder();
export const feedbackBytes = (text: string) => encoder.encode(text).length;

/** Whether `text` is a message the WFE takes: not blank, and not too long. */
export const sendable = (text: string) =>
  !!text.trim() && feedbackBytes(text) <= MAX_FEEDBACK_BYTES;

/**
 * The page a feedback names: the path alone, never its query or hash (a
 * goal, a search, a document's span), without control characters and cut to
 * whole characters within 255 bytes.
 *
 * @example
 * feedbackPage("/data/connections/conn_1") // "/data/connections/conn_1"
 * feedbackPage("data") // "/data"
 */
export function feedbackPage(pathname: string): string {
  // eslint-disable-next-line no-control-regex
  let page = pathname.replace(/[\u0000-\u001f\u007f]/g, "");
  if (!page.startsWith("/")) page = "/" + page;
  while (feedbackBytes(page) > MAX_PAGE_BYTES)
    page = Array.from(page).slice(0, -1).join("");
  return page;
}

/**
 * Request a connector's message: the tool named on its first line, then
 * what the person wants from it.
 *
 * @example
 * connectorRequestMessage(" HubSpot ", "Deals and contacts")
 * // "Connector: HubSpot\n\nDeals and contacts"
 */
export function connectorRequestMessage(name: string, details: string) {
  const more = details.trim();
  return `Connector: ${name.trim()}` + (more ? `\n\n${more}` : "");
}

/**
 * A local build's feedback: GitHub's new-issue page for the repository,
 * with the title and body filled in. The person reads it there and sends it,
 * or not.
 */
export function githubIssueUrl(title: string, body: string): string {
  return `${STAR_REPOSITORY_URL}/issues/new?${new URLSearchParams({
    title,
    body,
  })}`;
}

/** The first line of a message, cut to `max` characters, for a title. */
export function firstLine(message: string, max = 80): string {
  const line = message.trim().split("\n", 1)[0]!.trim(),
    chars = Array.from(line);
  return chars.length > max ? chars.slice(0, max - 1).join("") + "…" : line;
}

/**
 * Sends feedback: hosted, to the WFE (`sent`); in a local build, by opening
 * GitHub's new-issue page filled in (`opened`), titled `title`.
 */
export async function sendFeedback(
  request: SendFeedbackRequest,
  title: string,
): Promise<"sent" | "opened"> {
  if (LOCAL) {
    window.open(
      githubIssueUrl(
        title,
        `${request.message.trim()}\n\n---\nPage: ${request.page ?? "/"}\nFoundry, local build`,
      ),
      "_blank",
      "noopener,noreferrer",
    );
    return "opened";
  }
  await api<SendFeedbackResponse>("/v1/feedback", request);
  return "sent";
}
