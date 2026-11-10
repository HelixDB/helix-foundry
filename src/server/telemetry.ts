import { parseUsageBatch, USAGE_LIMITS } from "@/shared/usage";

/**
 * Whether a local install reports how it is used (src/web/telemetry.tsx asks
 * GET /telemetry before it sends anything, and POST /telemetry asks again):
 * yes, unless its owner turned it off with FOUNDRY_TELEMETRY (off, false, no
 * or 0) or DO_NOT_TRACK (any value but 0, false or no, as the convention
 * reads it). Read when asked, so a change takes effect at the site's next
 * start.
 *
 * @example
 * telemetryEnabled({}); // true
 * telemetryEnabled({ FOUNDRY_TELEMETRY: "off" }); // false
 * telemetryEnabled({ DO_NOT_TRACK: "1" }); // false
 * telemetryEnabled({ DO_NOT_TRACK: "0" }); // true
 */
export function telemetryEnabled(
  env: Record<string, string | undefined>,
): boolean {
  const no = (value: string) => /^(off|false|no|0)$/i.test(value);
  const telemetry = env.FOUNDRY_TELEMETRY?.trim() || "",
    doNotTrack = env.DO_NOT_TRACK?.trim() || "";
  return !(telemetry && no(telemetry)) && !(doNotTrack && !no(doNotTrack));
}

// Databuddy's events API (www.databuddy.cc/docs/api/events), which takes the
// site's public client id as `website_id`: no key is shipped.
export const EVENTS_URL = "https://basket.databuddy.cc/track";
// How long a post waits for Databuddy before it answers the page anyway.
const UPSTREAM_TIMEOUT_MS = 5000;

/** Where a local install's site forwards its pages' usage events. */
export type UsageForwarding = {
  /** The loopback origin the browser uses (LocalConfig's). */
  origin: string;
  /** The "Foundry Local" site's client id. */
  clientId: string;
  /** telemetryEnabled, read for this request. */
  enabled: boolean;
};

/**
 * POST /telemetry in a local build: a page's usage events (src/shared/usage.ts),
 * forwarded to Databuddy's events API under the site's client id and the
 * browser's User-Agent. Only the site's own pages may post (its Host and
 * Origin), so another site open in the browser can't send through it; with
 * reporting off nothing is forwarded. Answers 204 once forwarded or dropped,
 * whatever Databuddy answers: a page has nothing to do about it.
 */
export async function forwardUsage(
  request: Request,
  config: UsageForwarding,
  upstream: typeof fetch,
): Promise<Response> {
  if (
    request.headers.get("host") !== new URL(config.origin).host ||
    request.headers.get("origin") !== config.origin
  )
    return new Response(null, { status: 403 });
  if (!config.enabled) return new Response(null, { status: 204 });
  // Past the Host and Origin checks only the site's own pages post, so the
  // body is read whole, then measured.
  if (Number(request.headers.get("content-length")) > USAGE_LIMITS.bytes)
    return new Response(null, { status: 413 });
  const text = await request.text();
  if (Buffer.byteLength(text) > USAGE_LIMITS.bytes)
    return new Response(null, { status: 413 });
  let batch;
  try {
    batch = parseUsageBatch(JSON.parse(text));
  } catch {
    batch = null;
  }
  if (!batch) return new Response(null, { status: 400 });
  if (batch.events.length === 0) return new Response(null, { status: 204 });
  const userAgent = request.headers.get("user-agent");
  await upstream(
    `${EVENTS_URL}?website_id=${encodeURIComponent(config.clientId)}`,
    {
      method: "POST",
      headers: {
        "content-type": "application/json",
        // Databuddy reads the browser and device from it, and drops bots.
        ...(userAgent && { "user-agent": userAgent }),
      },
      body: JSON.stringify(
        batch.events.map((event) => ({
          eventId: event.id,
          name: event.name,
          properties: event.properties,
          path: config.origin + event.path,
          anonymousId: batch.visitor,
          sessionId: batch.session,
          // As the tracker sends its own: the visitor id is hashed on arrival.
          anonymizeVisitorIds: true,
          timestamp: event.at,
          source: "browser",
        })),
      ),
      signal: AbortSignal.timeout(UPSTREAM_TIMEOUT_MS),
    },
  ).catch(() => undefined);
  return new Response(null, { status: 204 });
}
