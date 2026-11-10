// How Foundry reports a page's path, and what a local install's page posts
// to its own site (POST /telemetry), which forwards it to Databuddy's events
// API (src/server/telemetry.ts). Databuddy's tracker sends nothing from
// localhost, so a local build loads none: its events go through its site.

/**
 * Paths hold ids and table names: these mask them, so Databuddy counts each
 * route as one page (every load's page as one). The first pattern that
 * matches is used, and a match keeps the segments after it as they are, so a
 * route's deeper ids come first. A `*` segment matches one segment and masks
 * it; `**` matches the rest, masked as one `*` when there is any, which
 * keeps the wizard's and the sample's pages apart from an ontology's. The
 * hosted tracker masks with the same patterns (src/web/telemetry.tsx), and
 * `maskPath` masks as it does.
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

/** `path` masked by `pattern`, or null when it doesn't match. */
function maskWith(path: string, pattern: string): string | null {
  const segments = path.split("/"),
    parts = pattern.split("/"),
    rest = parts.indexOf("**"),
    fixed = rest === -1 ? parts : parts.slice(0, rest);
  if (
    fixed.some(
      (part, i) =>
        segments[i] === undefined || (part !== "*" && part !== segments[i]),
    )
  )
    return null;
  return rest === -1
    ? [...fixed, ...segments.slice(fixed.length)].join("/")
    : [
        ...fixed,
        ...(segments.slice(rest).some((segment) => segment) ? ["*"] : []),
      ].join("/");
}

/**
 * `path` with its ids masked by the first of MASK_PATTERNS that matches, else
 * as it is. Idempotent.
 *
 * @example
 * maskPath("/ask/ont_1"); // "/ask/*"
 * maskPath("/ontology/new/x"); // "/ontology/new/*"
 * maskPath("/ontology/new"); // "/ontology/new"
 * maskPath("/data"); // "/data"
 */
export const maskPath = (path: string): string =>
  MASK_PATTERNS.reduce<string | null>(
    (masked, pattern) => masked ?? maskWith(path, pattern),
    null,
  ) ?? path;

/** A custom event of src/web/analytics.ts, page views included (`page_viewed`). */
export type UsageEvent = {
  /** Its id, for Databuddy to drop a repeat. */
  id: string;
  name: string;
  /** The event's own properties and the global ones (the workspace's id). */
  properties: Record<string, string | number | boolean>;
  /** The page's path, masked (maskPath). */
  path: string;
  /** When, in milliseconds since the epoch. */
  at: number;
};

/** What a local page posts: its events, under the browser's visitor and the tab's session. */
export type UsageBatch = {
  visitor: string;
  session: string;
  events: UsageEvent[];
};

/** The most a post may hold; the server refuses more. */
export const USAGE_LIMITS = {
  bytes: 64 * 1024,
  events: 50,
  properties: 24,
  text: 200,
  path: 512,
};

const ID = /^[A-Za-z0-9-]{1,64}$/,
  NAME = /^[a-z][a-z0-9_]{0,63}$/;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/**
 * `value` as a UsageBatch, or null when anything in it isn't one: the server
 * forwards only what fits, so nothing but events' names, short values and
 * masked paths leave the machine. Paths are masked again.
 *
 * @example
 * parseUsageBatch({ visitor: "v", session: "s", events: [] }); // { visitor: "v", session: "s", events: [] }
 * parseUsageBatch({ visitor: "v", session: "s" }); // null
 */
export function parseUsageBatch(value: unknown): UsageBatch | null {
  if (
    !isRecord(value) ||
    typeof value.visitor !== "string" ||
    !ID.test(value.visitor) ||
    typeof value.session !== "string" ||
    !ID.test(value.session) ||
    !Array.isArray(value.events) ||
    value.events.length > USAGE_LIMITS.events
  )
    return null;
  const events = value.events.map((event): UsageEvent | null => {
    if (
      !isRecord(event) ||
      typeof event.id !== "string" ||
      !ID.test(event.id) ||
      typeof event.name !== "string" ||
      !NAME.test(event.name) ||
      typeof event.path !== "string" ||
      !event.path.startsWith("/") ||
      event.path.length > USAGE_LIMITS.path ||
      typeof event.at !== "number" ||
      !Number.isFinite(event.at) ||
      !isRecord(event.properties)
    )
      return null;
    const properties = Object.entries(event.properties);
    if (
      properties.length > USAGE_LIMITS.properties ||
      !properties.every(
        ([key, v]) =>
          NAME.test(key) &&
          (typeof v === "boolean" ||
            (typeof v === "number" && Number.isFinite(v)) ||
            (typeof v === "string" && v.length <= USAGE_LIMITS.text)),
      )
    )
      return null;
    return {
      id: event.id,
      name: event.name,
      properties: Object.fromEntries(properties) as UsageEvent["properties"],
      path: maskPath(event.path),
      at: event.at,
    };
  });
  return events.every((event) => event !== null)
    ? {
        visitor: value.visitor,
        session: value.session,
        events: events as UsageEvent[],
      }
    : null;
}
