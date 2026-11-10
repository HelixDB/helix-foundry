// The landing page's reminders (src/shared/reminders.ts) on the server: POST
// /remind takes the form and asks Foundry's public door, the foundry-events
// role (ingest.helix-db.com), to remind the address; a sign-in tells the door
// to stop reminding the address that signed in. Only this server calls the
// door's reminder routes: it holds the secret they take as their bearer,
// which the door hashes and Foundry's backend compares with the hash in its
// config (foundry.reminders.secret_sha256). The backend keeps the address
// and sends the reminders through Foundry's mail (SES).

import { isTrustedUrl } from "@/server/config";
import { sameOrigin } from "@/server/csrf";
import { LOCAL } from "@/shared/deployment";
import { CONSENT, type RemindAnswer } from "@/shared/reminders";

export type RemindersConfig = {
  // The door's origin: HELIX_INGEST_ORIGIN, else the stamp's
  // NEXT_PUBLIC_HELIX_EVENTS_URL (next.config.ts).
  ingestOrigin: string;
  // FOUNDRY_REMINDERS_SECRET: at least 32 bytes.
  secret: string;
};

/**
 * The reminders' settings, or null where the site has none: a local build,
 * no FOUNDRY_REMINDERS_SECRET or one under 32 bytes, or no door at a trusted
 * origin (https, or http on loopback for the mock). Without them the
 * landing page offers no reminder and POST /remind answers 404.
 *
 * @example
 * remindersConfig({ FOUNDRY_REMINDERS_SECRET: "s".repeat(32) }, "https://ingest.helix-db.com")
 *   ?.ingestOrigin; // "https://ingest.helix-db.com"
 */
export function remindersConfig(
  env: Record<string, string | undefined> = process.env,
  // Read here, where next build inlines it, not from `env`.
  eventsUrl: string = process.env.NEXT_PUBLIC_HELIX_EVENTS_URL ?? "",
  local: boolean = LOCAL,
): RemindersConfig | null {
  const secret = env.FOUNDRY_REMINDERS_SECRET?.trim() ?? "";
  if (local || Buffer.byteLength(secret, "utf8") < 32) return null;
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

function validTimeZone(zone: string) {
  if (!zone || zone.length > 64) return false;
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: zone });
    return true;
  } catch {
    return false;
  }
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
  if (address.length > 254 || !EMAIL.test(address) || !validTimeZone(timeZone))
    return null;
  return { email: address, timeZone };
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
 * page, checked, then sent on to the door. Answers `{nextSendAt, again}`, or
 * `{error}`: `invalid` (400) for an address or zone refused, `rate_limit`
 * (429) when the door or the backend's hourly cap says so, `error` (403 for
 * a bot or another origin, 502 when the door failed). The address is never
 * logged.
 */
export async function handleRemind(
  request: Request,
  { config, isBot }: RemindDeps,
): Promise<Response> {
  if (!config) return new Response(null, { status: 404 });
  if (!sameOrigin(request)) return answer(403, { error: "error" });
  if (isBot && (await isBot().catch(() => true)))
    return answer(403, { error: "error" });
  const form = parseRemindForm(await request.text());
  if (!form) return answer(400, { error: "invalid" });
  let res: Response;
  try {
    res = await door(config, "/v1/reminders", {
      email: form.email,
      time_zone: form.timeZone,
      consent: CONSENT,
    });
  } catch {
    return answer(502, { error: "error" });
  }
  if (res.status === 400) return answer(400, { error: "invalid" });
  if (res.status === 429) {
    const wait = res.headers.get("Retry-After");
    return answer(
      429,
      { error: "rate_limit" },
      wait ? { "Retry-After": wait } : undefined,
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

/**
 * Tells the door that `email` signed in, so its reminders stop. Never
 * throws: a sign-in never waits on it or fails for it (the callback runs it
 * after answering), and a reminder sent anyway is only one too many.
 */
export async function stopReminders(
  config: RemindersConfig | null,
  email: string | null | undefined,
): Promise<void> {
  if (!config || !email) return;
  try {
    const res = await door(config, "/v1/reminders/stop", { email });
    await res.body?.cancel();
  } catch {
    // The door is away: nothing to do.
  }
}
