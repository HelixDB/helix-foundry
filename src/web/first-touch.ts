import { maskPath } from "../shared/usage";

// First-touch attribution (the hosted site's): where a visitor first came
// from, kept in this browser for 30 days and sent with the sign-up funnel's
// events (analytics.ts firstTouch). Only campaign tags, a referring host and
// a masked path: never a query value beyond the utm_ tags, or anything a
// person typed.

/**
 * What the first touch keeps: each tag sanitised (sanitiseTag; undefined
 * when absent), the path masked.
 */
export type FirstTouch = {
  source?: string;
  medium?: string;
  campaign?: string;
  referrer_host?: string;
  landing_path: string;
  /** When, in milliseconds since the epoch. */
  at: number;
};

/** The properties a funnel event carries from the first touch. */
export type FirstTouchProperties = {
  first_source?: string;
  first_medium?: string;
  first_campaign?: string;
  first_referrer?: string;
  first_landing?: string;
};

/** How long a first touch is kept before a newer one may take its place. */
export const FIRST_TOUCH_KEEP_MS = 30 * 24 * 60 * 60 * 1000;
const MAX_LENGTH = 40;

/**
 * A tag as Databuddy keeps it: lowercase `[a-z0-9._-]`, any other run of
 * characters one `_`, at most 40 characters, or undefined when nothing is
 * left or it holds an `@` (an email some mailers put in a link's tags).
 *
 * @example
 * sanitiseTag("Spring Sale!"); // "spring_sale"
 * sanitiseTag("  "); // undefined
 * sanitiseTag("ada@example.com"); // undefined
 */
export function sanitiseTag(value: string | null | undefined) {
  if (value?.includes("@")) return undefined;
  const tag = (value ?? "")
    .toLowerCase()
    .replace(/[^a-z0-9._-]+/g, "_")
    .slice(0, MAX_LENGTH)
    .replace(/^_+|_+$/g, "");
  return tag || undefined;
}

/** A path masked as page views are (maskPath), in `[a-z0-9._/*-]`, at most 40 characters. */
const landingPath = (pathname: string) =>
  maskPath(pathname)
    .toLowerCase()
    .replace(/[^a-z0-9._/*-]+/g, "_")
    .slice(0, MAX_LENGTH) || "/";

/**
 * The touch a page load makes: its utm_source, utm_medium and utm_campaign,
 * and the host of a `referrer` that isn't this site (`host`), or null when
 * it has neither.
 *
 * @example
 * firstTouchOf(
 *   { search: "?utm_source=github", pathname: "/login", referrer: "", host: "foundry.helix-db.com" },
 *   0,
 * ); // { source: "github", landing_path: "/login", at: 0 }, the rest undefined
 */
export function firstTouchOf(
  page: { search: string; pathname: string; referrer: string; host: string },
  now: number,
): FirstTouch | null {
  const query = new URLSearchParams(page.search);
  let referrer: string | undefined;
  try {
    const host = new URL(page.referrer).hostname.toLowerCase();
    if (host && host !== page.host.toLowerCase().replace(/:\d+$/, ""))
      referrer = sanitiseTag(host);
  } catch {
    // No referrer, or not a URL.
  }
  const touch = {
    source: sanitiseTag(query.get("utm_source")),
    medium: sanitiseTag(query.get("utm_medium")),
    campaign: sanitiseTag(query.get("utm_campaign")),
    referrer_host: referrer,
  };
  if (!Object.values(touch).some(Boolean)) return null;
  return {
    ...touch,
    landing_path: landingPath(page.pathname),
    at: now,
  };
}

/**
 * A stored first touch, checked rather than trusted, or null when there is
 * none, it is unreadable, or it is 30 days old (or from the future).
 */
export function readFirstTouch(
  raw: string | null,
  now: number,
): FirstTouch | null {
  let value: unknown;
  try {
    value = JSON.parse(raw ?? "");
  } catch {
    return null;
  }
  if (!value || typeof value !== "object") return null;
  const v = value as Record<string, unknown>;
  if (
    typeof v.at !== "number" ||
    v.at > now ||
    now - v.at >= FIRST_TOUCH_KEEP_MS
  )
    return null;
  const tag = (key: string) => {
    const field = v[key];
    return typeof field === "string" ? sanitiseTag(field) : undefined;
  };
  const touch = {
    source: tag("source"),
    medium: tag("medium"),
    campaign: tag("campaign"),
    referrer_host: tag("referrer_host"),
  };
  if (!Object.values(touch).some(Boolean)) return null;
  return {
    ...touch,
    landing_path:
      typeof v.landing_path === "string" ? landingPath(v.landing_path) : "/",
    at: v.at,
  };
}

/** The properties of a first touch, none for none. */
export const firstTouchProperties = (
  touch: FirstTouch | null,
): FirstTouchProperties =>
  touch
    ? {
        ...(touch.source && { first_source: touch.source }),
        ...(touch.medium && { first_medium: touch.medium }),
        ...(touch.campaign && { first_campaign: touch.campaign }),
        ...(touch.referrer_host && { first_referrer: touch.referrer_host }),
        first_landing: touch.landing_path,
      }
    : {};

const KEY = "foundry.analytics.first-touch.v1";
// Set on a tab's first page load: a later one in the same tab (back from
// WorkOS, Stripe or a provider's sign-in) is a return, whose referrer is no
// way in.
const SEEN = "foundry.analytics.first-touch.seen";

/**
 * This page load (./telemetry.tsx, the hosted site's): keeps its touch
 * unless one younger than 30 days is kept. Its referrer counts only on the
 * tab's first page load; utm_ tags on any.
 */
export function noteFirstTouch(
  page: { search: string; pathname: string; host: string; referrer: string },
  now = Date.now(),
) {
  try {
    const firstLoad = !sessionStorage.getItem(SEEN);
    sessionStorage.setItem(SEEN, "1");
    if (readFirstTouch(localStorage.getItem(KEY), now)) return;
    const touch = firstTouchOf(
      { ...page, referrer: firstLoad ? page.referrer : "" },
      now,
    );
    if (touch) localStorage.setItem(KEY, JSON.stringify(touch));
  } catch {
    // Storage blocked: no first touch is kept.
  }
}

/** The kept first touch's properties, none when there is none. */
export function storedFirstTouch(now = Date.now()): FirstTouchProperties {
  try {
    return firstTouchProperties(readFirstTouch(localStorage.getItem(KEY), now));
  } catch {
    return {};
  }
}
