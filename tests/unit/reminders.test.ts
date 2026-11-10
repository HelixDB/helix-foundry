import { afterEach, describe, expect, it, vi } from "vitest";
import { whenText } from "@/landing/remind-text";
import {
  handleRemind,
  parseRemindForm,
  remindersConfig,
  stopReminders,
  type RemindersConfig,
} from "@/server/reminders";

// The landing page's reminders (src/shared/reminders.ts): the words the form
// says, and POST /remind on the server, which checks the form and asks
// Foundry's door to remind the address.

const SECRET = "s".repeat(40);
const CONFIG: RemindersConfig = {
  ingestOrigin: "https://ingest.example.test",
  secret: SECRET,
};
const SITE = "https://foundry.example.test";

const form = (body: unknown, origin: string | null = SITE) =>
  new Request(`${SITE}/remind`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...(origin ? { Origin: origin } : {}),
    },
    body: typeof body === "string" ? body : JSON.stringify(body),
  });

const GOOD = {
  email: "  Ada@Example.com ",
  timeZone: "Europe/London",
  consent: "remind-v1",
};

afterEach(() => vi.unstubAllGlobals());

describe("whenText", () => {
  it.each([
    [
      "the same morning",
      "2026-11-10T07:00:00Z",
      "2026-11-10T09:00:00Z",
      "Europe/London",
      "today at 9am",
    ],
    [
      "the next morning",
      "2026-11-10T20:00:00Z",
      "2026-11-11T09:00:00Z",
      "Europe/London",
      "tomorrow at 9am",
    ],
    [
      // 21:00 on the 10th in New York, though the 11th in UTC.
      "tomorrow by the zone's own date",
      "2026-11-11T02:00:00Z",
      "2026-11-11T14:00:00Z",
      "America/New_York",
      "tomorrow at 9am",
    ],
    [
      "a Friday evening's Monday",
      "2026-11-13T20:00:00Z",
      "2026-11-16T09:00:00Z",
      "Europe/London",
      "on Monday at 9am",
    ],
    [
      "a week or more away",
      "2026-11-10T09:00:00Z",
      "2026-11-24T09:00:00Z",
      "Europe/London",
      "on 24 November at 9am",
    ],
    [
      "off the hour",
      "2026-11-10T12:00:00Z",
      "2026-11-11T04:00:00Z",
      "Asia/Kolkata",
      "tomorrow at 9:30am",
    ],
  ])("says %s", (_, now, at, zone, text) =>
    expect(whenText(new Date(at), zone, new Date(now))).toBe(text),
  );
});

describe("parseRemindForm", () => {
  it("keeps the trimmed address and the zone", () =>
    expect(parseRemindForm(JSON.stringify(GOOD))).toEqual({
      email: "Ada@Example.com",
      timeZone: "Europe/London",
    }));

  it.each([
    ["not JSON", "{"],
    ["an array", "[]"],
    ["no consent", JSON.stringify({ ...GOOD, consent: undefined })],
    ["another consent", JSON.stringify({ ...GOOD, consent: "remind-v0" })],
    ["another field", JSON.stringify({ ...GOOD, name: "Ada" })],
    ["no @", JSON.stringify({ ...GOOD, email: "ada.example.com" })],
    ["a space", JSON.stringify({ ...GOOD, email: "ada lovelace@example.com" })],
    ["too long", JSON.stringify({ ...GOOD, email: `${"a".repeat(250)}@b.co` })],
    ["an unknown zone", JSON.stringify({ ...GOOD, timeZone: "Mars/Olympus" })],
    ["no zone", JSON.stringify({ ...GOOD, timeZone: "" })],
    ["a body over 2 KiB", JSON.stringify({ ...GOOD, pad: "x".repeat(3000) })],
  ])("refuses %s", (_, raw) => expect(parseRemindForm(raw)).toBeNull());
});

describe("remindersConfig", () => {
  const events = "https://ingest.helix-db.com";
  it("takes the secret and the stamp's door", () =>
    expect(
      remindersConfig({ FOUNDRY_REMINDERS_SECRET: SECRET }, events, false),
    ).toEqual({ ingestOrigin: events, secret: SECRET }));

  it("prefers HELIX_INGEST_ORIGIN, as the mock sets it", () =>
    expect(
      remindersConfig(
        {
          FOUNDRY_REMINDERS_SECRET: SECRET,
          HELIX_INGEST_ORIGIN: "http://127.0.0.1:4110",
        },
        events,
        false,
      )?.ingestOrigin,
    ).toBe("http://127.0.0.1:4110"));

  it.each([
    ["no secret", {}, events, false],
    ["a short secret", { FOUNDRY_REMINDERS_SECRET: "short" }, events, false],
    ["no door", { FOUNDRY_REMINDERS_SECRET: SECRET }, "", false],
    [
      "plain http off loopback",
      { FOUNDRY_REMINDERS_SECRET: SECRET },
      "http://ingest.helix-db.com",
      false,
    ],
    [
      "a door with a path",
      { FOUNDRY_REMINDERS_SECRET: SECRET },
      `${events}/v1`,
      false,
    ],
    ["a local build", { FOUNDRY_REMINDERS_SECRET: SECRET }, events, true],
  ])("is off with %s", (_, env, url, local) =>
    expect(remindersConfig(env, url, local)).toBeNull(),
  );
});

describe("POST /remind", () => {
  const door = (status: number, body: unknown, headers?: HeadersInit) => {
    const fetch = vi.fn(async () => Response.json(body, { status, headers }));
    vi.stubGlobal("fetch", fetch);
    return fetch;
  };

  it("asks the door with the secret, and says when the first goes", async () => {
    const fetch = door(200, {
      next_send_at: "2026-11-11T09:00:00Z",
      again: false,
    });
    const res = await handleRemind(form(GOOD), { config: CONFIG });
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({
      nextSendAt: "2026-11-11T09:00:00Z",
      again: false,
    });
    const [url, init] = fetch.mock.calls[0] as unknown as [URL, RequestInit];
    expect(String(url)).toBe("https://ingest.example.test/v1/reminders");
    expect(new Headers(init.headers).get("Authorization")).toBe(
      `Bearer ${SECRET}`,
    );
    expect(JSON.parse(String(init.body))).toEqual({
      email: "Ada@Example.com",
      time_zone: "Europe/London",
      consent: "remind-v1",
    });
  });

  it("passes on that the address already had them", async () => {
    door(200, { next_send_at: "2026-11-11T09:00:00Z", again: true });
    const res = await handleRemind(form(GOOD), { config: CONFIG });
    expect(await res.json()).toMatchObject({ again: true });
  });

  it("is not there without its settings", async () => {
    const fetch = door(200, {});
    const res = await handleRemind(form(GOOD), { config: null });
    expect(res.status).toBe(404);
    expect(fetch).not.toHaveBeenCalled();
  });

  it.each([
    ["another origin", "https://evil.example.test"],
    ["no origin", null],
  ])("refuses %s", async (_, origin) => {
    const fetch = door(200, {});
    const res = await handleRemind(form(GOOD, origin), { config: CONFIG });
    expect(res.status).toBe(403);
    expect(fetch).not.toHaveBeenCalled();
  });

  it("refuses a bot, and a check that fails", async () => {
    const fetch = door(200, {});
    for (const isBot of [
      async () => true,
      async () => {
        throw new Error("BotID is away");
      },
    ]) {
      const res = await handleRemind(form(GOOD), { config: CONFIG, isBot });
      expect(res.status).toBe(403);
    }
    expect(fetch).not.toHaveBeenCalled();
  });

  it("refuses what isn't an address before asking the door", async () => {
    const fetch = door(200, {});
    const res = await handleRemind(form({ ...GOOD, email: "ada" }), {
      config: CONFIG,
    });
    expect(res.status).toBe(400);
    expect(await res.json()).toEqual({ error: "invalid" });
    expect(fetch).not.toHaveBeenCalled();
  });

  it.each([
    [400, {}, 400, { error: "invalid" }],
    [429, { "Retry-After": "60" }, 429, { error: "rate_limit" }],
    [401, {}, 502, { error: "error" }],
    [404, {}, 502, { error: "error" }],
    [503, {}, 502, { error: "error" }],
  ])("answers the door's %i as %i", async (status, headers, want, body) => {
    door(status, { error: { code: "x", message: "y" } }, headers);
    const res = await handleRemind(form(GOOD), { config: CONFIG });
    expect(res.status).toBe(want);
    expect(await res.json()).toEqual(body);
    if (status === 429) expect(res.headers.get("Retry-After")).toBe("60");
  });

  it("fails when the door can't be reached", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("fetch failed");
      }),
    );
    const res = await handleRemind(form(GOOD), { config: CONFIG });
    expect(res.status).toBe(502);
  });
});

describe("stopReminders", () => {
  it("tells the door the address signed in", async () => {
    const fetch = vi.fn(async () => Response.json({}));
    vi.stubGlobal("fetch", fetch);
    await stopReminders(CONFIG, "ada@example.com");
    const [url, init] = fetch.mock.calls[0] as unknown as [URL, RequestInit];
    expect(String(url)).toBe("https://ingest.example.test/v1/reminders/stop");
    expect(JSON.parse(String(init.body))).toEqual({
      email: "ada@example.com",
    });
  });

  it("does nothing without settings or an address, and never throws", async () => {
    const fetch = vi.fn(async () => {
      throw new TypeError("fetch failed");
    });
    vi.stubGlobal("fetch", fetch);
    await stopReminders(null, "ada@example.com");
    await stopReminders(CONFIG, "");
    expect(fetch).not.toHaveBeenCalled();
    await expect(
      stopReminders(CONFIG, "ada@example.com"),
    ).resolves.toBeUndefined();
  });
});
