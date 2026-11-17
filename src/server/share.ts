// A published dashboard's public page (/share/<token>, src/app/(share)):
// where its browser asks for the snapshot. The foundry-events door
// (ingest.helix-db.com) serves GET /v1/shared-dashboards/{token} to any page
// with no session and no credentials, so the page's browser asks it itself:
// never /papi, which forwards a session, and never this server, whose one
// address would spend every viewer's share of the door's per-address limits.

import { isTrustedUrl } from "@/server/config";
import { LOCAL } from "@/shared/deployment";

/**
 * The door's origin for the share page, or null where there is none: a
 * local build (public links are a hosted stamp's), or no door at a trusted
 * origin (https, or http on loopback for the mock). HELIX_INGEST_ORIGIN, as
 * the reminders read it, else the stamp's NEXT_PUBLIC_HELIX_EVENTS_URL.
 *
 * @example
 * shareDoor({}, "https://ingest.helix-db.com", false); // "https://ingest.helix-db.com"
 * shareDoor({}, "https://ingest.helix-db.com", true); // null
 * shareDoor({ HELIX_INGEST_ORIGIN: "http://example.com" }, "", false); // null
 */
export function shareDoor(
  env: Record<string, string | undefined> = process.env,
  // Read here, where next build inlines it, not from `env`.
  eventsUrl: string = process.env.NEXT_PUBLIC_HELIX_EVENTS_URL ?? "",
  local: boolean = LOCAL,
): string | null {
  if (local) return null;
  let url: URL;
  try {
    url = new URL(env.HELIX_INGEST_ORIGIN || eventsUrl);
  } catch {
    return null;
  }
  return isTrustedUrl(url) && url.origin + "/" === url.href ? url.origin : null;
}
