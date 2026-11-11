import { trackAgents } from "@databuddy/sdk/agents";
import {
  NextResponse,
  type NextFetchEvent,
  type NextRequest,
} from "next/server";
import { LOCAL } from "@/shared/deployment";

// The stamp's Databuddy site (next.config.ts); a local build, the mock and
// e2e have none, so this reports nothing.
const websiteId = process.env.NEXT_PUBLIC_DATABUDDY_CLIENT_ID;

/**
 * Reports AI crawlers and agents that read the public pages to Databuddy's
 * AI Agents page (`@databuddy/sdk/agents`: a known crawler's user agent, a
 * Signature-Agent header, or a client asking for Markdown first), which the
 * browser tracker never sees: they run no JavaScript. It sends the path, host,
 * user agent, Accept and referrer, after the response; browsers send nothing.
 * A client that asks for Markdown over HTML gets the landing page as Markdown
 * (src/app/index.md); every other response is left as it is.
 */
export function proxy(request: NextRequest, event: NextFetchEvent) {
  // A local install serves no crawler, and has no landing page.
  if (LOCAL) return;
  if (websiteId) event.waitUntil(trackAgents(request, { websiteId }));
  if (
    request.nextUrl.pathname === "/" &&
    (request.method === "GET" || request.method === "HEAD") &&
    prefersMarkdown(request.headers.get("accept"))
  )
    return NextResponse.rewrite(new URL("/index.md", request.url), {
      headers: { Vary: "Accept" },
    });
}

type MediaRange = { type: string; subtype: string; q: number };

/** An Accept header's ranges, with their quality (1 when not given). */
const ranges = (accept: string): MediaRange[] =>
  accept.split(",").flatMap((part) => {
    const [range = "", ...params] = part
      .split(";")
      .map((value) => value.trim().toLowerCase());
    const [type, subtype] = range.split("/");
    if (!type || !subtype) return [];
    const q = params.find((param) => param.startsWith("q="));
    const quality = q === undefined ? 1 : Number(q.slice(2));
    return Number.isFinite(quality) ? [{ type, subtype, q: quality }] : [];
  });

/**
 * Whether an Accept header asks for Markdown over HTML, as helix-website's
 * proxy reads it. Only an explicit text/markdown counts, so wildcards alone
 * keep HTML; a tie goes to Markdown, since browsers never name it and
 * clients that do want it.
 *
 * @example
 * prefersMarkdown("text/markdown, text/html;q=0.9"); // true
 * prefersMarkdown("text/html,application/xhtml+xml,*\/*;q=0.8"); // false
 */
export function prefersMarkdown(accept: string | null): boolean {
  if (!accept) return false;
  const all = ranges(accept);
  const markdown = all.find(
    (r) => r.type === "text" && r.subtype === "markdown",
  );
  // HTML's quality under its most specific range.
  const html =
    all.find((r) => r.type === "text" && r.subtype === "html") ??
    all.find((r) => r.type === "text" && r.subtype === "*") ??
    all.find((r) => r.type === "*" && r.subtype === "*");
  return !!markdown && markdown.q > 0 && markdown.q >= (html?.q ?? 0);
}

// The landing page, sign-in and the files written for agents and crawlers:
// the rest of the site is the signed-in app.
export const config = {
  matcher: [
    "/",
    "/login",
    "/index.md",
    "/llms.txt",
    "/llms-full.txt",
    "/robots.txt",
    "/sitemap.xml",
    "/.well-known/:path*",
  ],
};
