"use client";

import { setGlobalProperties } from "@databuddy/sdk";
import dynamic from "next/dynamic";
import { DATABUDDY_CLIENT_ID } from "./analytics";

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
 * Page views and the custom events of ./analytics.ts. No web vitals (the
 * tracker sends them with the path unmasked, table names and all),
 * interactions (they name the element clicked, which may be a document or
 * an entity), errors (their messages may quote the WFE's), outgoing links (a
 * document's source page) or session replay.
 */
export function Telemetry() {
  if (!Databuddy) return null;
  return (
    <Databuddy
      clientId={DATABUDDY_CLIENT_ID}
      maskPatterns={MASK_PATTERNS}
      filter={withoutTitle}
      trackInteractions={false}
    />
  );
}
