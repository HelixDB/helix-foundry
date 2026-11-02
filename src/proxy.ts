import { trackAgents } from "@databuddy/sdk/agents";
import type { NextFetchEvent, NextRequest } from "next/server";

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
  if (websiteId) event.waitUntil(trackAgents(request, { websiteId }));
}

// The landing page and sign-in: the rest of the site is the signed-in app.
export const config = { matcher: ["/", "/login"] };
