"use client";

import { useEffect, useState } from "react";
import { setGlobalProperties } from "@databuddy/sdk";
import dynamic from "next/dynamic";
import { DATABUDDY_CLIENT_ID, trackingAllowed } from "./analytics";

// Databuddy's tracker, on the hosted site's pages: the landing page, sign-in
// and the app (both root layouts mount it). Without a client id (a local
// build, the mock, e2e) the import below is dropped from the build.
const Databuddy = DATABUDDY_CLIENT_ID
  ? dynamic(
      () => import("@databuddy/sdk/react").then((module) => module.Databuddy),
      { ssr: false },
    )
  : null;

/**
 * Paths hold ids and table names: these mask them, so Databuddy counts each
 * route as one page (every load's page as one). Databuddy never sends a
 * query string. The first pattern that matches is used, and a match keeps the
 * segments after it as they are, so a route's deeper ids come first; `**`
 * matches without masking, which keeps the wizard's and the sample's pages
 * apart from an ontology's.
 */
export const MASK_PATTERNS = [
  "/data/connections/*/tables/*",
  "/data/connections/*",
  "/data/documents/*",
  "/ontology/new/**",
  "/ontology/sample/**",
  "/ontology/*/versions/*",
  "/ontology/*/generations/*",
  "/ontology/*/loads/*",
  "/ontology/*/documents/*",
  "/ontology/*",
  "/explore/*/entities/*",
  "/explore/*",
  "/ask/*",
];

/**
 * The tracker sends document.title with every page view and page exit, and
 * Foundry's titles name what a page shows (a question, a document, an
 * ontology). Page views pass through this filter, which drops it. Page exits
 * skip the filter but take the global properties last, where an undefined
 * title leaves it out of what is sent; the tracker exists by the first page
 * view, so it is set before any exit.
 */
export function withoutTitle(event: { title?: unknown }): boolean {
  delete event.title;
  setGlobalProperties({ title: undefined });
  return true;
}

/**
 * Everything the tracker records: page views, the custom events of
 * ./analytics.ts, web vitals, uncaught errors, outgoing links, interactions
 * and session replay. Page views and replays mask paths with MASK_PATTERNS;
 * the tracker sends web vitals and errors with the path as it is (ids and
 * table names), an error's message and stack, an outgoing link's address
 * (no query) and text, and, of interactions, counts and the label of a
 * rage- or dead-clicked element (its data-track, aria-label, test id, name
 * or id, emails and digits taken out). `data-track` attributes aren't read:
 * events are trackEvent's typed ones. Hash changes aren't page views: a
 * document's hash marks a span.
 *
 * Replay (replay.js, also on in the site's Databuddy settings) masks all page
 * text, attribute text and typed input, records no canvas (the Explorer's
 * graph), iframe (a PDF) or image bytes, and records URLs without their query
 * or hash, paths masked by MASK_PATTERNS. What shows a person's account,
 * the members or a payment form is also `data-replay-block`: left out whole.
 */
export const TRACKER = {
  clientId: DATABUDDY_CLIENT_ID,
  maskPatterns: MASK_PATTERNS,
  filter: withoutTitle,
  trackWebVitals: true,
  trackErrors: true,
  trackOutgoingLinks: true,
  trackInteractions: true,
  replay: true,
};

/**
 * The tracker, once the page knows this browser may load it: not one of
 * Helix's own (analytics.ts trackingAllowed, which reads the URL and
 * storage, so only after the first render).
 */
export function Telemetry() {
  const [allowed, setAllowed] = useState(false);
  useEffect(() => setAllowed(trackingAllowed(location.search)), []);
  if (!Databuddy || !allowed) return null;
  return <Databuddy {...TRACKER} />;
}
