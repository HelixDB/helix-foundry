// The reminders to try Foundry on a computer, which a phone visitor asks for
// on the landing page (helix-website) and Foundry's public door, the
// foundry-events role (ingest.helix-db.com), keeps: a sign-in here tells the
// door to stop reminding the address that signed in. The door's routes take
// a secret as their bearer, which the door hashes and Foundry's backend
// compares with the hash in its config (foundry.reminders.secret_sha256).

import { isTrustedUrl } from "@/server/config";
import { LOCAL } from "@/shared/deployment";

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
 * on loopback for the mock). Without them a sign-in stops none.
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

// What the door answers in time.
const DOOR_WAIT_MS = 8_000;

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
