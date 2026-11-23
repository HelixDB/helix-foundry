import { trackAgents } from "@databuddy/sdk/agents";
import type { NextFetchEvent, NextRequest } from "next/server";
import { LOCAL } from "@/shared/deployment";

// The stamp's Databuddy site (next.config.ts); a local build, the mock and
// e2e have none, so this reports nothing.
const websiteId = process.env.NEXT_PUBLIC_DATABUDDY_CLIENT_ID;

/**
 * Reports AI crawlers and agents that read the public pages to Databuddy's
 * AI Agents page (`@databuddy/sdk/agents`: a known crawler's user agent, a
 * Signature-Agent header, or a client asking for Markdown first), which the
 * browser tracker never sees: they run no JavaScript. It sends the path, host,
 * user agent, Accept and referrer, after the response; browsers send nothing,
 * and the response is never changed.
 */
export function proxy(request: NextRequest, event: NextFetchEvent) {
  // A local install serves no crawler.
  if (LOCAL) return;
  if (websiteId) event.waitUntil(trackAgents(request, { websiteId }));
}

// `/` (src/app/(app)/page.tsx, or foundry.new's setup prompt) and its
// Markdown (src/app/index.md), sign-in and the files written for agents and
// crawlers: the rest of the site is the signed-in app.
export const config = {
  matcher: [
    "/",
    "/login",
    "/index.md",
    "/llms.txt",
    "/llms-full.txt",
    "/setup.md",
    "/robots.txt",
    "/.well-known/:path*",
  ],
};
