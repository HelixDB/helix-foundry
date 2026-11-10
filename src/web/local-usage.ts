import { maskPath, USAGE_LIMITS, type UsageEvent } from "../shared/usage";

// A local install's usage events (./analytics.ts in a local build): Databuddy's
// tracker sends nothing from localhost, so the page posts them to its own site
// (POST /telemetry), which forwards them (src/server/telemetry.ts). Until
// ./telemetry.tsx knows whether the owner allows it, events wait here (at
// most a post's worth); then they are posted, or dropped and never kept.

// Events are posted a moment after the first, in one post, or at once when
// that many wait or the page is hidden.
const FLUSH_MS = 2000,
  FLUSH_AT = 20;
// The browser's visitor and the tab's session, as Databuddy's tracker keeps
// its own.
const VISITOR = "foundry.usage.visitor",
  SESSION = "foundry.usage.session";

let state: "waiting" | "on" | "off" = "waiting";
let queue: UsageEvent[] = [];
let timer: ReturnType<typeof setTimeout> | undefined;
const ids: { visitor?: string; session?: string } = {};

/** The id `key` holds in `storage`, made once; one for this page if storage is blocked. */
function idIn(storage: () => Storage, key: string): string {
  try {
    const kept = storage().getItem(key);
    if (kept) return kept;
    const id = crypto.randomUUID();
    storage().setItem(key, id);
    return id;
  } catch {
    return crypto.randomUUID();
  }
}

/**
 * Reports one event on the page at `path` (masked here), with `properties`
 * that have a value. Waits while the site hasn't said; nothing once off.
 */
export function report(
  name: string,
  properties: Record<string, string | number | boolean | null | undefined>,
  path: string,
): void {
  if (
    state === "off" ||
    (state === "waiting" && queue.length >= USAGE_LIMITS.events)
  )
    return;
  queue.push({
    id: crypto.randomUUID(),
    name,
    properties: Object.fromEntries(
      Object.entries(properties).filter(
        (entry): entry is [string, string | number | boolean] =>
          entry[1] !== null && entry[1] !== undefined,
      ),
    ),
    path: maskPath(path),
    at: Date.now(),
  });
  if (state !== "on") return;
  if (queue.length >= FLUSH_AT) flush();
  else timer ??= setTimeout(flush, FLUSH_MS);
}

/** Posts what waits, in posts of the most the site takes. Survives the page leaving (keepalive). */
export function flush(): void {
  clearTimeout(timer);
  timer = undefined;
  if (state !== "on" || queue.length === 0) return;
  ids.visitor ??= idIn(() => localStorage, VISITOR);
  ids.session ??= idIn(() => sessionStorage, SESSION);
  while (queue.length > 0) {
    const events = queue.splice(0, USAGE_LIMITS.events);
    void fetch("/telemetry", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        visitor: ids.visitor,
        session: ids.session,
        events,
      }),
      keepalive: true,
    }).catch(() => undefined);
  }
}

/**
 * Whether the browser asks not to be tracked (Do Not Track or Global Privacy
 * Control), which Databuddy's tracker honours on the hosted site: a local
 * page, which loads none, honours it here.
 *
 * @example
 * optedOut({ doNotTrack: "1" }); // true
 * optedOut({ globalPrivacyControl: true }); // true
 * optedOut({ doNotTrack: "unspecified" }); // false
 */
export const optedOut = (browser: {
  doNotTrack?: string | null;
  globalPrivacyControl?: boolean;
}): boolean =>
  browser.doNotTrack === "1" || browser.globalPrivacyControl === true;

/** The owner allows it (GET /telemetry): post what waits, and what follows. */
export function start(): void {
  if (state !== "waiting") return;
  state = "on";
  addEventListener("pagehide", flush);
  flush();
}

/** Nothing is sent or kept from now on: this browser or its owner said no. */
export function stop(): void {
  state = "off";
  queue = [];
  clearTimeout(timer);
  timer = undefined;
}
