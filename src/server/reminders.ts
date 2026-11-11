// The landing page's reminders (src/shared/reminders.ts) on the server: POST
// /remind takes the form and asks Foundry's public door, the foundry-events
// role (ingest.helix-db.com), to remind the address; a sign-in tells the door
// to stop reminding the address that signed in. Only this server calls the
// door's reminder routes: it holds the secret they take as their bearer,
// which the door hashes and Foundry's backend compares with the hash in its
// config (foundry.reminders.secret_sha256). The backend keeps the address
// and sends the reminders through Foundry's mail (SES).

import { createHmac } from "node:crypto";
import { isTrustedUrl } from "@/server/config";
import { sameOrigin } from "@/server/csrf";
import { LOCAL } from "@/shared/deployment";
import { CONSENT, type RemindAnswer } from "@/shared/reminders";

export type RemindersConfig = {
  // The door's origin: HELIX_INGEST_ORIGIN, else the stamp's
  // NEXT_PUBLIC_HELIX_EVENTS_URL (next.config.ts).
  ingestOrigin: string;
  // FOUNDRY_REMINDERS_SECRET: 32 random bytes in lowercase hex.
  secret: string;
};

// The form of the secret, as the door checks it: `openssl rand -hex 32`.
// The backend's config holds its SHA-256, in git, so it must be random.
const SECRET = /^[0-9a-f]{64}$/;

/**
 * The reminders' settings, or null where the site has none: a local build,
 * no FOUNDRY_REMINDERS_SECRET or one that isn't 64 lowercase hex digits
 * (`openssl rand -hex 32`), or no door at a trusted origin (https, or http
 * on loopback for the mock). Without them the landing page offers no
 * reminder and POST /remind answers 404.
 *
 * @example
 * remindersConfig({ FOUNDRY_REMINDERS_SECRET: "0a".repeat(32) }, "https://ingest.helix-db.com")
 *   ?.ingestOrigin; // "https://ingest.helix-db.com"
 */
export function remindersConfig(
  env: Record<string, string | undefined> = process.env,
  // Read here, where next build inlines it, not from `env`.
  eventsUrl: string = process.env.NEXT_PUBLIC_HELIX_EVENTS_URL ?? "",
  local: boolean = LOCAL,
): RemindersConfig | null {
  const secret = env.FOUNDRY_REMINDERS_SECRET?.trim() ?? "";
  if (local || !SECRET.test(secret)) return null;
  let url: URL;
  try {
    url = new URL(env.HELIX_INGEST_ORIGIN || eventsUrl);
  } catch {
    return null;
  }
  if (!isTrustedUrl(url) || url.origin + "/" !== url.href) return null;
  return { ingestOrigin: url.origin, secret };
}

// What the door answers in time, and the form's body at most.
const DOOR_WAIT_MS = 8_000;
const MAX_BODY_BYTES = 2048;

// As the door checks it: one @, no spaces, a dot in the domain. The backend
// parses it properly; this only spares it what plainly isn't an address.
const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

// The zone the browser named, as the backend takes it: a name of at most 64
// characters, else none. The backend keeps a zone it doesn't know as UTC,
// and so does it with none, so a zone this server's ICU doesn't know (newer
// than its data, say) never refuses an address.
function zoneOf(zone: string) {
  return zone.trim().length <= 64 ? zone.trim() : "";
}

type Form = { email: string; timeZone: string };

/** The form's fields, or null for anything else. */
export function parseRemindForm(raw: string): Form | null {
  if (Buffer.byteLength(raw, "utf8") > MAX_BODY_BYTES) return null;
  let body: unknown;
  try {
    body = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!body || typeof body !== "object" || Array.isArray(body)) return null;
  const { email, timeZone, consent, ...rest } = body as Record<string, unknown>;
  if (
    Object.keys(rest).length ||
    typeof email !== "string" ||
    typeof timeZone !== "string" ||
    consent !== CONSENT
  )
    return null;
  const address = email.trim();
  if (address.length > 254 || !EMAIL.test(address)) return null;
  return { email: address, timeZone: zoneOf(timeZone) };
}

/**
 * The request's body, or null past `max` bytes: a Content-Length past it is
 * refused unread, and a body without one is read only up to it.
 */
export async function readCapped(
  request: Request,
  max: number,
): Promise<string | null> {
  if (Number(request.headers.get("content-length")) > max) return null;
  if (!request.body) return "";
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > max) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  return Buffer.concat(chunks).toString("utf8");
}

/**
 * Who is asking, for the backend's cap on the reminders one visitor starts
 * an hour: a key of the visitor's address (Vercel's `x-real-ip`, else the
 * first `x-forwarded-for`, which Vercel sets and never passes on from the
 * client) under the site's secret, so the backend, which never holds the
 * secret, can't tell the address from it. "" when the request names none,
 * as off Vercel.
 */
export function clientKey(request: Request, secret: string): string {
  const ip =
    request.headers.get("x-real-ip")?.trim() ||
    request.headers.get("x-forwarded-for")?.split(",")[0]?.trim() ||
    "";
  if (!ip || ip.length > 64) return "";
  return createHmac("sha256", secret)
    .update(`foundry-reminders-client\0${ip}`)
    .digest("base64url")
    .slice(0, 22);
}

function answer(status: number, body: RemindAnswer, headers?: HeadersInit) {
  return Response.json(body, {
    status,
    headers: { "Cache-Control": "no-store", ...headers },
  });
}

function door(config: RemindersConfig, path: string, body: unknown) {
  return fetch(new URL(path, config.ingestOrigin), {
    method: "POST",
    headers: {
      Authorization: `Bearer ${config.secret}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body),
    cache: "no-store",
    redirect: "manual",
    signal: AbortSignal.timeout(DOOR_WAIT_MS),
  });
}

export type RemindDeps = {
  config: RemindersConfig | null;
  // Vercel BotID's verdict on the request (botid/server's checkBotId), where
  // the build runs it: the hosted stamps on their own origin.
  isBot?: () => Promise<boolean>;
};

/**
 * POST /remind: `{email, timeZone, consent}` from this origin's landing
 * page, checked, then sent on to the door with the visitor's `clientKey`.
 * Answers `{nextSendAt, again}`, or `{error}`: `invalid` (400) for an
 * address refused, or a body past 2 KiB, `rate_limit` (429, with the door's
 * Retry-After) when the door, the backend's hourly cap or the visitor's
 * says so, `error` (403 for a bot or another origin, 502 when the door
 * failed). The address is never logged.
 */
export async function handleRemind(
  request: Request,
  { config, isBot }: RemindDeps,
): Promise<Response> {
  if (!config) return new Response(null, { status: 404 });
  if (!sameOrigin(request)) return answer(403, { error: "error" });
  if (isBot && (await isBot().catch(() => true)))
    return answer(403, { error: "error" });
  const raw = await readCapped(request, MAX_BODY_BYTES);
  const form = raw === null ? null : parseRemindForm(raw);
  if (!form) return answer(400, { error: "invalid" });
  let res: Response;
  try {
    res = await door(config, "/v1/reminders", {
      email: form.email,
      time_zone: form.timeZone,
      consent: CONSENT,
      client: clientKey(request, config.secret),
    });
  } catch {
    return answer(502, { error: "error" });
  }
  if (res.status === 400 || res.status === 429) {
    // Nothing of a refusal's body is used: let its connection go.
    await res.body?.cancel().catch(() => {});
    if (res.status === 400) return answer(400, { error: "invalid" });
    const wait = res.headers.get("Retry-After");
    return answer(
      429,
      { error: "rate_limit" },
      wait && /^\d{1,6}$/.test(wait) ? { "Retry-After": wait } : undefined,
    );
  }
  const body = (await res.json().catch(() => null)) as {
    next_send_at?: unknown;
    again?: unknown;
  } | null;
  if (!res.ok || typeof body?.next_send_at !== "string")
    return answer(502, { error: "error" });
  return answer(200, {
    nextSendAt: body.next_send_at,
    again: body.again === true,
  });
}

// How long a stop waits before asking again, by attempt; a 429's
// Retry-After is waited instead, up to the last.
const STOP_WAITS_MS = [1_000, 4_000];
const MAX_STOP_WAIT_MS = 10_000;

const pause = (ms: number) => new Promise((done) => setTimeout(done, ms));

/**
 * Tells the door that `email` signed in, so its reminders stop. A door
 * away, failing (5xx) or busy (429) is asked again, after 1 s and 4 s (or
 * the Retry-After it asked, up to 10 s): a stop lost leaves someone who
 * signed in getting reminders. Never throws: a sign-in never waits on it
 * or fails for it (the callback runs it after answering). One still not
 * taken is logged, without the address.
 */
export async function stopReminders(
  config: RemindersConfig | null,
  email: string | null | undefined,
  wait: (ms: number) => Promise<unknown> = pause,
): Promise<void> {
  if (!config || !email) return;
  let failure = "";
  for (let attempt = 0; ; attempt++) {
    let again: number | null;
    try {
      const res = await door(config, "/v1/reminders/stop", { email });
      await res.body?.cancel().catch(() => {});
      if (res.ok) return;
      failure = `the door answered ${res.status}`;
      const after = Number(res.headers.get("Retry-After"));
      again =
        res.status === 429 || res.status >= 500
          ? res.status === 429 && after > 0
            ? Math.min(after * 1000, MAX_STOP_WAIT_MS)
            : (STOP_WAITS_MS[attempt] ?? 0)
          : null;
    } catch (error) {
      failure = `the door failed: ${error instanceof Error ? error.name : "error"}`;
      again = STOP_WAITS_MS[attempt] ?? 0;
    }
    if (again === null || attempt >= STOP_WAITS_MS.length) break;
    await wait(again);
  }
  console.warn(`Stopping a signed-in address's reminders failed: ${failure}`);
}
