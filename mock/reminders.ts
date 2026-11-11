import type { IncomingMessage, ServerResponse } from "node:http";
import { MOCK_REMINDERS_SECRET } from "./env";
import { after, first, knownZone } from "./reminder-schedule";
import { config, onReset } from "./state";

// The landing page's reminders at Foundry's door (backend/services/foundry-
// events: POST /v1/reminders and /v1/reminders/stop on ingest.helix-db.com),
// which the site's server calls with FOUNDRY_REMINDERS_SECRET as the bearer
// (src/server/reminders.ts). The mock is the door and the backend at once,
// with the backend's rules (backend/reminder, api/reminders.go): one schedule
// per address, the first at the next weekday 9am in its zone, and asking
// again while it runs changes nothing; one that ended starts again, never
// within a week of its last email; at most 300 new an hour
// (`{"remindersPerHour": n}`), and 5 from one visitor (the site's `client`
// key, `{"remindersPerClientPerHour": n}`). It sends nothing: GET
// /__mock/reminders lists what it holds.

const CONSENT = "remind-v1";
const MIN_GAP_DAYS = 7;
const HOUR_MS = 3_600_000;

export type MockReminder = {
  email: string;
  timeZone: string;
  consent: string;
  status: "scheduled" | "done" | "stopped";
  sent: number;
  nextSendAt: string | null;
  // When the schedule started (or started again), and the site's key for
  // who asked ("" for none), for the hourly caps.
  startedAt: string;
  client: string;
  lastSentAt: string | null;
  stopReason: "signed_in" | null;
};

type Knobs = { remindersPerHour?: number; remindersPerClientPerHour?: number };
const knobs = config as typeof config & Knobs;

const reminders = new Map<string, MockReminder>();
onReset(() => {
  reminders.clear();
  delete knobs.remindersPerHour;
  delete knobs.remindersPerClientPerHour;
});

/** Every address the mock holds, for GET /__mock/reminders. */
export const mockReminders = () => [...reminders.values()];

function reply(
  res: ServerResponse,
  status: number,
  body: unknown,
  headers: Record<string, string> = {},
) {
  res.writeHead(status, { "Content-Type": "application/json", ...headers });
  res.end(JSON.stringify(body));
  return true;
}

const refuse = (
  res: ServerResponse,
  status: number,
  code: string,
  message: string,
  headers?: Record<string, string>,
) => reply(res, status, { error: { code, message } }, headers);

// The body's fields, as the door decodes it: one object of strings, with
// no field but `keys`, any of which may be missing (""). Else null.
function fields(raw: string, keys: string[]): Record<string, string> | null {
  let body: unknown;
  try {
    body = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!body || typeof body !== "object" || Array.isArray(body)) return null;
  const record = body as Record<string, unknown>;
  if (
    !Object.keys(record).every((k) => keys.includes(k)) ||
    !Object.values(record).every((v) => typeof v === "string")
  )
    return null;
  return Object.fromEntries(keys.map((k) => [k, (record[k] as string) ?? ""]));
}

/**
 * The address as backend/reminder's Normalize keeps it, else null: trimmed
 * and lowercased, then one plain address of at most 254 bytes, its local
 * part a dot-atom and its domain with a dot, and no name, comment, quoted
 * part or control character.
 */
export function normalize(email: string): string | null {
  const address = email.trim().toLowerCase();
  if (
    !address ||
    Buffer.byteLength(address, "utf8") > 254 ||
    /["(),:;<>[\\\] ]/.test(address) ||
    /\p{Cc}/u.test(address) ||
    address.split("@").length !== 2
  )
    return null;
  const [local, domain] = address.split("@");
  const dotAtom = (part: string) =>
    part !== "" &&
    !part.startsWith(".") &&
    !part.endsWith(".") &&
    !part.includes("..");
  return dotAtom(local) && dotAtom(domain) && domain.includes(".")
    ? address
    : null;
}

/** The door's reminder routes; false for any other request. */
export function handleReminders(
  req: IncomingMessage,
  res: ServerResponse,
  url: URL,
  raw: string,
): boolean {
  const stop = url.pathname === "/v1/reminders/stop";
  if (req.method !== "POST" || (!stop && url.pathname !== "/v1/reminders"))
    return false;
  if (req.headers.authorization !== `Bearer ${MOCK_REMINDERS_SECRET}`)
    return refuse(res, 401, "unauthorized", "a reminder needs the site's key");
  const body = fields(
    raw,
    stop ? ["email"] : ["email", "time_zone", "consent", "client"],
  );
  const email = body && normalize(body.email);
  if (
    !body ||
    !email ||
    (!stop && body.consent !== CONSENT) ||
    (!stop && !/^[!-~]{0,64}$/.test(body.client))
  )
    return refuse(res, 400, "invalid_arguments", "not an email address");

  const kept = reminders.get(email);
  if (stop) {
    if (kept?.status === "scheduled")
      Object.assign(kept, {
        status: "stopped",
        nextSendAt: null,
        stopReason: "signed_in",
      });
    return reply(res, 200, {});
  }
  if (kept?.status === "scheduled")
    return reply(res, 200, { next_send_at: kept.nextSendAt, again: true });

  const now = new Date();
  const lately = [...reminders.values()].filter(
    (r) => now.getTime() - Date.parse(r.startedAt) < HOUR_MS,
  );
  if (
    lately.length >= (knobs.remindersPerHour ?? 300) ||
    (body.client &&
      lately.filter((r) => r.client === body.client).length >=
        (knobs.remindersPerClientPerHour ?? 5))
  )
    return refuse(res, 429, "rate_limited", "too many reminders this hour", {
      "Retry-After": "900",
    });
  const zone = knownZone(body.time_zone) ? body.time_zone : "UTC";
  let next = first(now, zone);
  if (kept?.lastSentAt) {
    const earliest = after(new Date(kept.lastSentAt), MIN_GAP_DAYS, zone);
    if (earliest > next) next = earliest;
  }
  reminders.set(email, {
    email,
    timeZone: zone,
    consent: CONSENT,
    status: "scheduled",
    sent: 0,
    nextSendAt: next.toISOString(),
    startedAt: now.toISOString(),
    client: body.client,
    lastSentAt: kept?.lastSentAt ?? null,
    stopReason: null,
  });
  return reply(res, 200, { next_send_at: next.toISOString(), again: false });
}
